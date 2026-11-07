package connect

// C16, `EncryptionSettings.StickyRequired`: an Opportunistic client stops falling
// back to plaintext for a peer once a session with it has sealed, for as long as
// the contract it sealed under runs. Each gate is driven at the entry point real
// traffic reaches it by, and read from the wire: every message a client writes
// passes a tap that records what crossed readable.
//
//   - a peer that never forms a session keeps being served in plaintext;
//   - after a seal, a forced reset holds application frames at the entry gate,
//     the writer's backstop and the no-acknowledgement fast path, and a plaintext
//     pack is dropped, acknowledged and audited at the receive gate;
//   - the latch is the peer's, so a reply on a session that never sealed is held,
//     and losing every session inside the contract does not clear it;
//   - it ends at the signed deadline of the last contract sealed under;
//   - off, and in the other modes, nothing changes. The twins that run with it
//     off show the plaintext the rest of this file refuses.
//
// A rig's session is held down with `stickyWire.dropControl`, which drops the
// handshake on the wire, so the gate is all that stands between a frame and the
// wire for as long as the test needs: no test depends on a handshake being slow.

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// One direction of a rig's wire: what crossed it, and a switch that drops the
// handshake frames on it.
type stickyWire struct {
	establishHoldWire
	dropControl atomic.Bool
}

// Whether a message carries handshake frames readable on the wire.
func stickyWireCarriesControl(wireBytes []byte) bool {
	wire := &establishHoldWire{}
	wire.record(time.Time{}, wireBytes)
	messages := wire.snapshot()
	return len(messages) == 1 && 0 < messages[0].control
}

// Takes each received root, records it, then forwards or disposes it. The
// returned channel joins the relay after custody ends.
func (self *stickyWire) relay(ctx context.Context, in <-chan []byte, out chan<- []byte) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case message, ok := <-in:
				if !ok {
					return
				}
				self.record(time.Now(), message)
				if self.dropControl.Load() && stickyWireCarriesControl(message) {
					MessagePoolReturn(message)
					continue
				}
				if !forwardOwnedTestWire(ctx, out, message) {
					return
				}
			}
		}
	}()
	return done
}

// Application frames written readable so far.
func (self *stickyWire) readable() int {
	_, application, _, _ := self.counts()
	return application
}

// Handshake frames written so far.
func (self *stickyWire) control() int {
	_, _, control, _ := self.counts()
	return control
}

// Records the audits a client sends to the control. Takes the frames it is given,
// as every out-of-band control does.
type stickyAuditOob struct {
	*grantingClientOob
	audits chan *protocol.PeerAudit
}

func (self *stickyAuditOob) SendControl(frames []*protocol.Frame, callback func([]*protocol.Frame, error)) {
	for _, frame := range frames {
		if frame.MessageType != protocol.MessageType_TransferPeerAudit {
			continue
		}
		if message, err := FromFrame(frame); err == nil {
			if audit, ok := message.(*protocol.PeerAudit); ok {
				select {
				case self.audits <- audit:
				default:
				}
			}
		}
	}
	self.grantingClientOob.SendControl(frames, callback)
}

type stickyRigSettings struct {
	// the zero value of each mode is Off, so a rig states both
	aMode   EncryptionMode
	bMode   EncryptionMode
	aSticky bool
	bSticky bool
	// how long every contract runs; zero leaves contracts with no deadline
	contractLife time.Duration
	mutateA      func(*ClientSettings)
	mutateB      func(*ClientSettings)
}

// Two clients on a tapped loopback, granted Network contracts by each other.
type stickyRig struct {
	ctx       context.Context
	a         *Client
	b         *Client
	aClientId Id
	bClientId Id
	// what each client wrote toward the other
	aWire     *stickyWire
	bWire     *stickyWire
	receivesA chan string
	receivesB chan string
	eventsA   chan EncryptionEventType
	eventsB   chan EncryptionEventType
	// the audits b sent
	auditsB chan *protocol.PeerAudit
	// what every contract carries, zero for no deadline
	contractDeadlineUnixMilli int64
}

func stickyReceives(client *Client) chan string {
	receives := make(chan string, 1024)
	client.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			if message, err := FromFrame(frame); err == nil {
				if simple, ok := message.(*protocol.SimpleMessage); ok {
					select {
					case receives <- simple.Content:
					default:
					}
				}
			}
		}
	})
	return receives
}

func stickyEvents(client *Client) chan EncryptionEventType {
	events := make(chan EncryptionEventType, 1024)
	client.EncryptionSessionManager().AddEncryptionEventCallback(func(event *EncryptionEvent) {
		select {
		case events <- event.Type:
		default:
		}
	})
	return events
}

func newStickyRig(t *testing.T, ctx context.Context, rigSettings stickyRigSettings) *stickyRig {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	rig := &stickyRig{
		ctx:       ctx,
		aClientId: NewId(),
		bClientId: NewId(),
		aWire:     &stickyWire{},
		bWire:     &stickyWire{},
		auditsB:   make(chan *protocol.PeerAudit, 1024),
	}
	var contractExpirationUnixMilli func() *int64
	if 0 < rigSettings.contractLife {
		rig.contractDeadlineUnixMilli = time.Now().Add(rigSettings.contractLife).UnixMilli()
		contractExpirationUnixMilli = func() *int64 {
			deadline := rig.contractDeadlineUnixMilli
			return &deadline
		}
	}

	aSend := make(chan []byte)
	aTapped := make(chan []byte)
	bSend := make(chan []byte)
	bTapped := make(chan []byte)
	aTapDone := rig.aWire.relay(ctx, aSend, aTapped)
	bTapDone := rig.bWire.relay(ctx, bSend, bTapped)
	aConditioner, bReceive := newConditioner(ctx, aTapped)
	bConditioner, aReceive := newConditioner(ctx, bTapped)

	provideModes := map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true}
	makeSettings := func(mode EncryptionMode, sticky bool, mutate func(*ClientSettings)) *ClientSettings {
		s := DefaultClientSettingsWithBufferSize(64)
		s.SendBufferSettings.AckTimeout = 120 * time.Second
		s.SendBufferSettings.IdleTimeout = 120 * time.Second
		s.SendBufferSettings.MinResendInterval = 10 * time.Millisecond
		s.ReceiveBufferSettings.GapTimeout = 120 * time.Second
		s.ReceiveBufferSettings.IdleTimeout = 120 * time.Second
		s.ForwardBufferSettings.IdleTimeout = 1 * time.Second
		s.ContractManagerSettings.LegacyCreateContract = false
		s.EncryptionSettings.Mode = mode
		s.EncryptionSettings.StickyRequired = sticky
		// a held-down handshake outlives the test rather than its timeout
		s.EncryptionSettings.TlsTimeout = 10 * time.Minute
		s.EncryptionSettings.EncryptionControlUseCompanion = true
		if mutate != nil {
			mutate(s)
		}
		return s
	}

	aOob := &grantingClientOob{
		sourceId: rig.aClientId,
		settings: DefaultContractManagerSettings(),
		destSecretKey: func(destinationId Id) ([]byte, bool) {
			return rig.b.ContractManager().GetProvideSecretKey(protocol.ProvideMode_Network)
		},
		destClientPublicKey: func(destinationId Id) []byte {
			return rig.b.ClientKeyManager().PublicKey()
		},
		contractExpirationUnixMilli: contractExpirationUnixMilli,
	}
	bOob := &stickyAuditOob{
		grantingClientOob: &grantingClientOob{
			sourceId: rig.bClientId,
			settings: DefaultContractManagerSettings(),
			destSecretKey: func(destinationId Id) ([]byte, bool) {
				return rig.a.ContractManager().GetProvideSecretKey(protocol.ProvideMode_Network)
			},
			destClientPublicKey: func(destinationId Id) []byte {
				return rig.a.ClientKeyManager().PublicKey()
			},
			contractExpirationUnixMilli: contractExpirationUnixMilli,
		},
		audits: rig.auditsB,
	}

	rig.a = NewClient(ctx, rig.aClientId, aOob, makeSettings(rigSettings.aMode, rigSettings.aSticky, rigSettings.mutateA))
	rig.a.RouteManager().UpdateTransport(newDataGatewayTransport(), []Route{aSend})
	rig.a.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{aReceive})
	aSinkDone := blackholeControlId(ctx, rig.a.RouteManager())
	rig.a.ContractManager().SetProvideModes(provideModes)

	rig.b = NewClient(ctx, rig.bClientId, bOob, makeSettings(rigSettings.bMode, rigSettings.bSticky, rigSettings.mutateB))
	rig.b.RouteManager().UpdateTransport(newDataGatewayTransport(), []Route{bSend})
	rig.b.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{bReceive})
	bSinkDone := blackholeControlId(ctx, rig.b.RouteManager())
	rig.b.ContractManager().SetProvideModes(provideModes)

	rig.receivesA = stickyReceives(rig.a)
	rig.receivesB = stickyReceives(rig.b)
	rig.eventsA = stickyEvents(rig.a)
	rig.eventsB = stickyEvents(rig.b)

	t.Cleanup(func() {
		cancel()
		<-aTapDone
		<-bTapDone
		<-aConditioner.done
		<-bConditioner.done
		<-aSinkDone
		<-bSinkDone
		joinCtx, joinCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer joinCancel()
		for _, client := range []*Client{rig.a, rig.b} {
			if err := client.CloseAndWait(joinCtx); err != nil {
				t.Errorf("sticky rig client did not join: %v", err)
			}
		}
	})
	return rig
}

// Sends one labeled frame. A refused frame stays with the caller, so it is returned
// here.
func stickySend(t *testing.T, client *Client, destinationId Id, label string, timeout time.Duration, opts ...any) (bool, error) {
	t.Helper()
	frame := requiredGateFrame(t, label)
	admitted, err := client.SendWithTimeoutDetailed(frame, destinationId, func(error) {}, timeout, opts...)
	if !admitted {
		MessagePoolReturn(frame.MessageBytes)
	}
	return admitted, err
}

func stickyRefused(t *testing.T, admitted bool, err error) {
	t.Helper()
	if admitted {
		t.Fatal("the send was admitted while nothing could seal it")
	}
	if !errors.Is(err, ErrEncryptionRequiredNotEstablished) {
		t.Fatalf("the send failed with %v, want %v", err, ErrEncryptionRequiredNotEstablished)
	}
}

func (self *stickyRig) waitEvent(t *testing.T, events chan EncryptionEventType, want EncryptionEventType) {
	t.Helper()
	for {
		select {
		case got := <-events:
			if got == want {
				return
			}
		case <-self.ctx.Done():
			t.Fatalf("event %d never fired", want)
		}
	}
}

func (self *stickyRig) receive(t *testing.T, receives chan string, want string) {
	t.Helper()
	select {
	case got := <-receives:
		AssertEqual(t, want, got)
	case <-self.ctx.Done():
		t.Fatalf("%q never arrived", want)
	}
}

// Seals the pair and carries one frame across it sealed. The opening frame goes
// first and alone: anything else a's Opportunistic side wrote while the session
// formed would land in the window the receive gate discards.
func (self *stickyRig) seal(t *testing.T) {
	t.Helper()
	if admitted, err := stickySend(t, self.a, self.bClientId, "opening", 60*time.Second); !admitted {
		t.Fatalf("the opening send was refused: %v", err)
	}
	self.receive(t, self.receivesB, "opening")
	self.waitEvent(t, self.eventsA, EncryptionEventSealed)
	self.waitEvent(t, self.eventsB, EncryptionEventSealed)
	if admitted, err := stickySend(t, self.a, self.bClientId, "sealed", 60*time.Second); !admitted {
		t.Fatalf("the sealed send was refused: %v", err)
	}
	self.receive(t, self.receivesB, "sealed")
}

// The session a's sends to b ride.
func (self *stickyRig) aSession(t *testing.T) *peerEncryptionSession {
	t.Helper()
	session := self.a.encryptionSessionManager.Lookup(self.bClientId, sequenceTlsRoleClient, false)
	if session == nil {
		t.Fatal("a has no session to b")
	}
	return session
}

// Forces a's session down as a forged unknown-wrap nack does, the strongest claim
// one can make, and keeps it down: b's answers to the new handshake are dropped.
func (self *stickyRig) forceReset(t *testing.T) {
	t.Helper()
	self.bWire.dropControl.Store(true)
	session := self.aSession(t)
	session.handleUnknownWrapNack(Id{})
	if session.Cipher() != nil {
		t.Fatal("the forced reset left a cipher, so nothing here reads the gates")
	}
}

// A peer that never forms a session keeps being served, with the setting on: the
// latch sets only after a seal.
func TestStickyRequiredServesAPeerThatNeverSeals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	rig := newStickyRig(t, ctx, stickyRigSettings{
		aMode:   EncryptionModeOpportunistic,
		bMode:   EncryptionModeOff,
		aSticky: true,
	})

	if admitted, err := stickySend(t, rig.a, rig.bClientId, "first", 60*time.Second); !admitted {
		t.Fatalf("the first send was refused: %v", err)
	}
	rig.receive(t, rig.receivesB, "first")
	// a non-blocking send is not refused either: nothing latched
	if admitted, err := stickySend(t, rig.a, rig.bClientId, "second", 0); !admitted {
		t.Fatalf("a send to a peer with no session was refused: %v", err)
	}
	rig.receive(t, rig.receivesB, "second")

	if rig.aWire.readable() == 0 {
		t.Fatal("the tap read no application frame, though the peer cannot read a sealed one")
	}
	if establishHoldSealed(rig.a, rig.bClientId) {
		t.Fatal("a session formed with a peer that has encryption off")
	}
	if rig.a.encryptionSessionManager.requireEncryptionForPeer(rig.bClientId) {
		t.Fatal("a peer that never sealed is latched")
	}
}

// Off, a forced reset gets plaintext: this is what the rest of this file refuses.
func TestStickyRequiredOffFallsBackToPlaintextOnAForcedReset(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	rig := newStickyRig(t, ctx, stickyRigSettings{
		aMode: EncryptionModeOpportunistic,
		bMode: EncryptionModeOpportunistic,
	})
	rig.seal(t)
	readable := rig.aWire.readable()
	rig.forceReset(t)

	if admitted, err := stickySend(t, rig.a, rig.bClientId, "fallback", 0); !admitted {
		t.Fatalf("Opportunistic refused a send with no cipher: %v", err)
	}
	rig.receive(t, rig.receivesB, "fallback")
	if rig.aWire.readable() <= readable {
		t.Fatal("the fallback did not cross the wire readable, so the tap reads nothing")
	}

	session := rig.aSession(t)
	if session.stickyRequired != nil {
		t.Fatal("a session holds a latch with the setting off")
	}
	if session.RequireEncryption() {
		t.Fatal("a session with the setting off requires encryption")
	}
	if count := len(rig.a.encryptionSessionManager.stickyRequiredPeers); count != 0 {
		t.Fatalf("%d latches with the setting off", count)
	}
}

// After a seal, a forced reset refuses a non-blocking send at the entry gate,
// refuses a bounded one at its budget, and holds a patient one until the session
// is back, sealed. Handshake frames keep crossing while the gate holds.
func TestStickyRequiredHoldsSendsAcrossAForcedReset(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	gateEntries := make(chan struct{}, 1024)
	rig := newStickyRig(t, ctx, stickyRigSettings{
		aMode:   EncryptionModeOpportunistic,
		bMode:   EncryptionModeOpportunistic,
		aSticky: true,
		mutateA: func(s *ClientSettings) {
			s.SendBufferSettings.beforeRequiredEncryptionWaitForTest = func(sendSequenceId) {
				gateEntries <- struct{}{}
			}
		},
	})
	rig.seal(t)
	// the sealed session's packs reach the gate too, with the cipher there
	for len(gateEntries) != 0 {
		<-gateEntries
	}
	readable := rig.aWire.readable()
	control := rig.aWire.control()
	rig.forceReset(t)

	gateEntered := func() {
		t.Helper()
		select {
		case <-gateEntries:
		case <-ctx.Done():
			t.Fatal("the send never reached the entry gate")
		}
	}
	admitted, err := stickySend(t, rig.a, rig.bClientId, "non-blocking", 0)
	stickyRefused(t, admitted, err)
	gateEntered()
	rig.waitEvent(t, rig.eventsA, EncryptionEventRequiredSendBlocked)

	budget := 200 * time.Millisecond
	sendStart := time.Now()
	admitted, err = stickySend(t, rig.a, rig.bClientId, "bounded", budget)
	stickyRefused(t, admitted, err)
	if waited := time.Since(sendStart); waited < budget {
		t.Fatalf("the bounded send was refused after %s of its %s", waited, budget)
	}
	gateEntered()

	heldFrame := requiredGateFrame(t, "held")
	held := make(chan error, 1)
	go func() {
		admitted, err := rig.a.SendWithTimeoutDetailed(heldFrame, rig.bClientId, func(error) {}, 120*time.Second)
		if !admitted {
			MessagePoolReturn(heldFrame.MessageBytes)
			if err == nil {
				err = errors.New("not admitted")
			}
		}
		held <- err
	}()
	// the patient send is at the gate before the session is let back up
	gateEntered()
	select {
	case err := <-held:
		t.Fatalf("the held send returned with the session down: %v", err)
	default:
	}

	rig.bWire.dropControl.Store(false)
	rig.waitEvent(t, rig.eventsA, EncryptionEventSealed)
	select {
	case err := <-held:
		if err != nil {
			t.Fatalf("the held send was not delivered: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("the held send never returned")
	}
	rig.receive(t, rig.receivesB, "held")

	if now := rig.aWire.readable(); now != readable {
		t.Fatalf("%d application frame(s) crossed readable after the seal", now-readable)
	}
	if rig.aWire.control() <= control {
		t.Fatal("no handshake frame crossed while the gate held, so they are not exempt")
	}
}

// Both sides on, a forged nack with the peer alive costs a re-handshake: the send
// that follows is delivered, sealed, with no hand from the test.
func TestStickyRequiredForgedNackCausesNoOutageAndNoPlaintext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	rig := newStickyRig(t, ctx, stickyRigSettings{
		aMode:   EncryptionModeOpportunistic,
		bMode:   EncryptionModeOpportunistic,
		aSticky: true,
		bSticky: true,
	})
	rig.seal(t)
	readable := rig.aWire.readable()

	rig.aSession(t).handleUnknownWrapNack(Id{})
	if admitted, err := stickySend(t, rig.a, rig.bClientId, "after", 120*time.Second); !admitted {
		t.Fatalf("the forged nack caused an outage: %v", err)
	}
	rig.receive(t, rig.receivesB, "after")
	if now := rig.aWire.readable(); now != readable {
		t.Fatalf("%d application frame(s) crossed readable after a forged nack", now-readable)
	}
}

// Drives the writer the way a resend does, on a frame sequenced under a cipher the
// session has since lost. With the peer latched the write is refused and nothing
// reaches the route. The twin has no latch: the same write is plaintext, which is
// what the backstop stops.
func TestStickyRequiredBackstopRefusesAFrameSequencedBeforeTheReset(t *testing.T) {
	for _, latched := range []bool{true, false} {
		sequence, session, route := newContractRekeyWriteFixture(t)
		session.settings = &EncryptionSettings{Mode: EncryptionModeOpportunistic, StickyRequired: true}
		session.stickyRequired = &stickyRequiredPeer{}
		if latched {
			session.stickyRequired.seal()
		}
		sequence.sendWithSetContract([]*protocol.Frame{requiredGateFrame(t, "sequenced")}, nil, true, true, false)
		requireContractRekeyWireWrapped(t, route, true)
		item := sequence.resendQueue.PeekFirst()
		if item == nil || item.forceUnwrapped {
			t.Fatal("the application frame has no wrapped retained item")
		}

		session.stateLock.Lock()
		session.establishedEpoch = nil
		session.stateLock.Unlock()

		path := sendTransferPath(sequence.client.ClientId(), DestinationId(sequence.destination))
		_, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, path, item.forceUnwrapped, item, true, false)
		if !latched {
			if err != nil {
				t.Fatalf("twin: the write failed: %v", err)
			}
			requireContractRekeyWireWrapped(t, route, false)
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "fail-closed") {
			t.Fatalf("the write with no cipher returned %v, want the fail-closed refusal", err)
		}
		select {
		case message := <-route:
			MessagePoolReturn(message)
			t.Fatal("a refused write reached the route")
		default:
		}
	}
}

// The fast path writes on the caller's goroutine, after Pack's entry. Its own check
// refuses a latched peer with no cipher, and Pack's refuses the same send through
// the client. The twin, nothing latched, is written readable. Sealed again, it is
// written sealed.
func TestStickyRequiredNoAckFastPathRefusesAfterAReset(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	harness, session := establishHoldFastPathHarness(t, ctx, 0)
	session.stickyRequired = &stickyRequiredPeer{}
	newSendPack := func(frame *protocol.Frame) *SendPack {
		return &SendPack{
			TransferOptions:  TransferOptions{Ack: false},
			Frame:            frame,
			Destination:      harness.destinationId,
			AckCallback:      func(error) {},
			MessageByteCount: ByteCount(len(frame.MessageBytes)),
			Ctx:              ctx,
		}
	}
	frame := noAckFastPathTestFrame(t)
	sendPack := newSendPack(frame)

	session.stickyRequired.seal()
	snapshot := harness.sequence.readNoAckFastPath(sendPack)
	if snapshot == nil {
		t.Fatal("no fast path snapshot")
	}
	if harness.sequence.writeNoAckFastPath(snapshot, sendPack) {
		t.Fatal("the fast path wrote for a latched peer with no cipher")
	}
	select {
	case wireBytes := <-harness.route:
		MessagePoolReturn(wireBytes)
		t.Fatal("the fast path refused, yet a message reached the route")
	default:
	}
	entry := noAckFastPathTestFrame(t)
	admitted, err := harness.client.SendWithTimeoutDetailed(entry, harness.destinationId, nil, 0, NoAck())
	if !admitted {
		MessagePoolReturn(entry.MessageBytes)
	}
	stickyRefused(t, admitted, err)

	session.stickyRequired.clear()
	snapshot = harness.sequence.readNoAckFastPath(sendPack)
	if !harness.sequence.writeNoAckFastPath(snapshot, sendPack) {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatal("twin: the fast path did not write with nothing latched")
	}
	if establishHoldTakeOne(t, harness.route) {
		t.Fatal("twin: the pack was sealed by a session with no cipher")
	}

	establishHoldSealer(t, session)()
	sealedFrame := noAckFastPathTestFrame(t)
	sendPack = newSendPack(sealedFrame)
	snapshot = harness.sequence.readNoAckFastPath(sendPack)
	if !harness.sequence.writeNoAckFastPath(snapshot, sendPack) {
		MessagePoolReturn(sealedFrame.MessageBytes)
		t.Fatal("the fast path did not write with the cipher back")
	}
	if !establishHoldTakeOne(t, harness.route) {
		t.Fatal("the pack was written readable by a sealed session")
	}
}

// Sends one frame from an a that has lost its cipher and falls back, the plaintext
// a downgrade delivers, and returns the error its acknowledgement carried. The
// frame has crossed the wire readable by then.
func stickyPlaintextAfterASeal(t *testing.T, rig *stickyRig) error {
	t.Helper()
	rig.seal(t)
	readable := rig.aWire.readable()
	rig.forceReset(t)

	acknowledged := make(chan error, 1)
	frame := requiredGateFrame(t, "plaintext")
	if admitted, err := rig.a.SendWithTimeoutDetailed(frame, rig.bClientId, func(err error) {
		acknowledged <- err
	}, 60*time.Second); !admitted {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatalf("Opportunistic refused a send with no cipher: %v", err)
	}
	var err error
	select {
	case err = <-acknowledged:
	case <-rig.ctx.Done():
		t.Fatal("the plaintext frame was never acknowledged")
	}
	if rig.aWire.readable() <= readable {
		t.Fatal("the frame did not cross the wire readable, so the gate saw nothing")
	}
	return err
}

// b seals, then a, which has lost its cipher, sends plaintext. b acknowledges and
// drops it, so the sequence keeps flowing, fires the event and audits the pack.
// Once a is sealed again its frames arrive: the audit leaves on the next frame.
func TestStickyRequiredDropsAndAuditsPlaintextAfterASeal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	rig := newStickyRig(t, ctx, stickyRigSettings{
		aMode:   EncryptionModeOpportunistic,
		bMode:   EncryptionModeOpportunistic,
		bSticky: true,
		mutateB: func(s *ClientSettings) {
			s.SendPeerAudits = true
			// each audit leaves on the sequence's next observation
			s.ReceiveBufferSettings.MaxPeerAuditDuration = 0
		},
	})
	if err := stickyPlaintextAfterASeal(t, rig); err != nil {
		t.Fatalf("the plaintext frame was not acknowledged: %v", err)
	}
	rig.waitEvent(t, rig.eventsB, EncryptionEventRequiredReceiveDiscarded)
	select {
	case got := <-rig.receivesB:
		t.Fatalf("the receive gate delivered %q", got)
	default:
	}

	rig.bWire.dropControl.Store(false)
	rig.waitEvent(t, rig.eventsA, EncryptionEventSealed)
	if admitted, err := stickySend(t, rig.a, rig.bClientId, "resealed", 60*time.Second); !admitted {
		t.Fatalf("the send after the reseal was refused: %v", err)
	}
	rig.receive(t, rig.receivesB, "resealed")
	for {
		select {
		case audit := <-rig.auditsB:
			if 0 < audit.BadMessageCount {
				return
			}
		case <-rig.ctx.Done():
			t.Fatal("the dropped pack was never audited")
		}
	}
}

// The twin with the setting off: the same pack is delivered, so what the test above
// dropped really was an application pack at the receive gate.
func TestStickyRequiredOffDeliversThatPlaintext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	rig := newStickyRig(t, ctx, stickyRigSettings{
		aMode: EncryptionModeOpportunistic,
		bMode: EncryptionModeOpportunistic,
	})
	if err := stickyPlaintextAfterASeal(t, rig); err != nil {
		t.Fatalf("the plaintext frame was not acknowledged: %v", err)
	}
	rig.receive(t, rig.receivesB, "plaintext")
}

// Sessions are per (peer, companion, role). b sealed with a as the responder, and a
// send of b's own rides a client-role session that has never sealed: a return on a
// new session, or a companion contract. Each is held to the latch.
func TestStickyRequiredHoldsTheOtherSessionsOfASealedPeer(t *testing.T) {
	cases := []struct {
		name string
		opts []any
	}{
		{name: "return", opts: nil},
		{name: "companion", opts: []any{CompanionContract()}},
	}
	for _, c := range cases {
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
			defer cancel()
			rig := newStickyRig(t, ctx, stickyRigSettings{
				aMode:   EncryptionModeOpportunistic,
				bMode:   EncryptionModeOpportunistic,
				bSticky: true,
			})
			rig.seal(t)
			readable := rig.bWire.readable()

			// a's answers to the new session's handshake are dropped
			rig.aWire.dropControl.Store(true)
			admitted, err := stickySend(t, rig.b, rig.aClientId, "refused", 0, c.opts...)
			if admitted {
				t.Fatalf("%s: a reply on an unsealed session of a sealed peer was admitted", c.name)
			}
			stickyRefused(t, admitted, err)

			rig.aWire.dropControl.Store(false)
			if admitted, err := stickySend(t, rig.b, rig.aClientId, "reply", 120*time.Second, c.opts...); !admitted {
				t.Fatalf("%s: the reply was refused with the handshake free: %v", c.name, err)
			}
			rig.receive(t, rig.receivesA, "reply")
			if now := rig.bWire.readable(); now != readable {
				t.Fatalf("%s: %d reply frame(s) crossed readable", c.name, now-readable)
			}
		}()
	}
}

// The twin with the setting off: the same reply crosses readable, which is the
// leak of §17.12.
func TestStickyRequiredOffLeavesThoseRepliesReadable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	rig := newStickyRig(t, ctx, stickyRigSettings{
		aMode: EncryptionModeOpportunistic,
		bMode: EncryptionModeOpportunistic,
	})
	rig.seal(t)
	readable := rig.bWire.readable()
	rig.aWire.dropControl.Store(true)
	if admitted, err := stickySend(t, rig.b, rig.aClientId, "reply", 60*time.Second); !admitted {
		t.Fatalf("the reply was refused: %v", err)
	}
	rig.receive(t, rig.receivesA, "reply")
	if rig.bWire.readable() <= readable {
		t.Fatal("the reply crossed sealed, so this twin shows nothing")
	}
}

// A session is reaped when idle while its contract stays open, so losing every
// session inside the contract must not reopen plaintext: a new session of the peer
// is latched from its first frame.
func TestStickyRequiredOutlivesTheSessionInsideTheContract(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	rig := newStickyRig(t, ctx, stickyRigSettings{
		aMode:        EncryptionModeOpportunistic,
		bMode:        EncryptionModeOpportunistic,
		aSticky:      true,
		contractLife: time.Hour,
	})
	rig.seal(t)
	readable := rig.aWire.readable()

	rig.bWire.dropControl.Store(true)
	rig.a.encryptionSessionManager.Testing_DropSessions(rig.bClientId)
	admitted, err := stickySend(t, rig.a, rig.bClientId, "refused", 0)
	stickyRefused(t, admitted, err)
	if now := rig.aWire.readable(); now != readable {
		t.Fatalf("%d application frame(s) crossed readable after the session was lost", now-readable)
	}
}

// Both ends learn the deadline from the contract: a from the one it took, b from
// the one it verified.
func TestStickyRequiredRunsToTheSignedDeadlineOnBothEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	rig := newStickyRig(t, ctx, stickyRigSettings{
		aMode:        EncryptionModeOpportunistic,
		bMode:        EncryptionModeOpportunistic,
		aSticky:      true,
		bSticky:      true,
		contractLife: time.Hour,
	})
	rig.seal(t)
	deadline := uint64(rig.contractDeadlineUnixMilli)
	for _, side := range []struct {
		client *Client
		peerId Id
	}{
		{client: rig.a, peerId: rig.bClientId},
		{client: rig.b, peerId: rig.aClientId},
	} {
		manager := side.client.encryptionSessionManager
		sticky := func() *stickyRequiredPeer {
			manager.stateLock.Lock()
			defer manager.stateLock.Unlock()
			return manager.stickyRequiredPeers[side.peerId]
		}()
		if sticky == nil {
			t.Fatal("no latch for the sealed peer")
		}
		AssertEqual(t, sticky.latchedUntil.Load(), deadline)
	}
}

// A bare session and latch, for the rules alone. Sealing and losing the cipher act
// as `markEstablishedWithLock` and a demotion do.
func newStickyBareSession() *peerEncryptionSession {
	return &peerEncryptionSession{
		settings:       &EncryptionSettings{Mode: EncryptionModeOpportunistic, StickyRequired: true},
		stickyRequired: &stickyRequiredPeer{},
	}
}

func stickyBareSeal(session *peerEncryptionSession) {
	session.stateLock.Lock()
	defer session.stateLock.Unlock()
	epoch := &tlsHandshakeEpoch{cancel: func() {}, peerIdentityVerified: true}
	session.epoch = epoch
	session.markEstablishedWithLock(epoch)
}

func stickyBareReset(session *peerEncryptionSession) {
	session.stateLock.Lock()
	defer session.stateLock.Unlock()
	session.establishedEpoch = nil
}

func stickyContractFor(life time.Duration) *sequenceContract {
	deadline := time.Now().Add(life).UnixMilli()
	return &sequenceContract{expirationTimeUnixMilli: &deadline}
}

// The latch runs to the last contract a session sealed under, on an injected clock.
// A contract taken with nothing sealed does not stretch it, so a peer that has
// stopped sealing is served again at that deadline.
func TestStickyRequiredEndsAtTheLastContractSealedUnder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		session := newStickyBareSession()
		session.observeContract(stickyContractFor(time.Hour))
		if session.RequireEncryption() {
			t.Fatal("a contract latched a peer that has not sealed")
		}

		stickyBareSeal(session)
		if !session.RequireEncryption() {
			t.Fatal("the seal set no latch")
		}
		time.Sleep(30 * time.Minute)

		// a reset inside the contract changes nothing
		stickyBareReset(session)
		if !session.RequireEncryption() {
			t.Fatal("a reset cleared the latch inside the contract")
		}
		session.observeContract(stickyContractFor(2 * time.Hour))
		time.Sleep(29 * time.Minute)
		if !session.RequireEncryption() {
			t.Fatal("the latch ended before its contract's deadline")
		}
		time.Sleep(2 * time.Minute)
		if session.RequireEncryption() {
			t.Fatal("a contract taken unsealed stretched the latch past the last one sealed under")
		}

		// a new seal reaches the contract that had been waiting
		stickyBareSeal(session)
		if !session.RequireEncryption() {
			t.Fatal("the seal did not reach the newest contract")
		}
		time.Sleep(time.Hour)
		if !session.RequireEncryption() {
			t.Fatal("the latch ended before the newest contract's deadline")
		}

		// a contract taken while sealed extends it with no new seal
		session.observeContract(stickyContractFor(3 * time.Hour))
		time.Sleep(2 * time.Hour)
		if !session.RequireEncryption() {
			t.Fatal("a contract taken while sealed did not extend the latch")
		}
		time.Sleep(2 * time.Hour)
		if session.RequireEncryption() {
			t.Fatal("the latch outlived every contract")
		}
	})
}

func newStickyClient(t *testing.T, ctx context.Context, mode EncryptionMode, sticky bool) *Client {
	t.Helper()
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = mode
	settings.EncryptionSettings.StickyRequired = sticky
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	t.Cleanup(client.Cancel)
	return client
}

// Under a contract with no deadline, or under none, nothing outlasts the sessions:
// the latch holds while one remains and is gone when the last one is.
func TestStickyRequiredWithNoDeadlineLastsAsLongAsASession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := newStickyClient(t, ctx, EncryptionModeOpportunistic, true).encryptionSessionManager

	for _, contract := range []*sequenceContract{nil, {}} {
		peerId := NewId()
		session := manager.getOrCreate(peerId, sequenceTlsRoleClient, false)
		if contract != nil {
			session.observeContract(contract)
		}
		establishHoldSealer(t, session)()
		if !manager.requireEncryptionForPeer(peerId) {
			t.Fatal("the seal set no latch")
		}
		sibling := manager.getOrCreate(peerId, sequenceTlsRoleServer, false)
		if !sibling.RequireEncryption() {
			t.Fatal("a session of a sealed peer is not latched")
		}

		manager.Testing_DropSessions(peerId)
		fresh := manager.getOrCreate(peerId, sequenceTlsRoleClient, false)
		if fresh.RequireEncryption() {
			t.Fatal("the latch outlived the last session with nothing to hold it")
		}
		manager.Testing_DropSessions(peerId)
	}
}

// Latches are per peer and a provider meets many peers, so the map holds what is
// live and no more: peers that never sealed leave nothing, and a latch goes once its
// deadline has passed and its sessions are gone, not before.
func TestStickyRequiredLatchesAreSwept(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := newStickyClient(t, ctx, EncryptionModeOpportunistic, true).encryptionSessionManager
	latchCount := func() int {
		manager.stateLock.Lock()
		defer manager.stateLock.Unlock()
		return len(manager.stickyRequiredPeers)
	}
	visit := func(count int) {
		for range count {
			peerId := NewId()
			manager.getOrCreate(peerId, sequenceTlsRoleClient, false)
			manager.Testing_DropSessions(peerId)
		}
	}

	visit(64)
	if count := latchCount(); 1 < count {
		t.Fatalf("%d latches left by peers that never sealed", count)
	}

	heldId := NewId()
	session := manager.getOrCreate(heldId, sequenceTlsRoleClient, false)
	session.observeContract(stickyContractFor(time.Hour))
	establishHoldSealer(t, session)()
	sticky := session.stickyRequired
	manager.Testing_DropSessions(heldId)

	visit(64)
	if !manager.requireEncryptionForPeer(heldId) {
		t.Fatal("the sweep took a latch inside its contract")
	}
	if count := latchCount(); 3 < count {
		t.Fatalf("%d latches with one live", count)
	}

	sticky.latchedUntil.Store(1)
	visit(4)
	manager.stateLock.Lock()
	_, ok := manager.stickyRequiredPeers[heldId]
	manager.stateLock.Unlock()
	if ok {
		t.Fatal("the sweep kept a latch past its deadline with no sessions")
	}
}

// The setting is off by default, and it means nothing outside Opportunistic: no
// latch in the other modes, and Required refuses from the first frame as it did.
func TestStickyRequiredDefaultsOffAndLeavesTheOtherModes(t *testing.T) {
	if DefaultEncryptionSettings().StickyRequired {
		t.Fatal("the setting is on by default")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	required := newStickyClient(t, ctx, EncryptionModeRequired, true).encryptionSessionManager
	session := required.getOrCreate(NewId(), sequenceTlsRoleClient, false)
	if session.stickyRequired != nil {
		t.Fatal("a Required session holds a latch")
	}
	if !session.RequireEncryption() {
		t.Fatal("a Required session does not require encryption")
	}

	off := newStickyClient(t, ctx, EncryptionModeOff, true).encryptionSessionManager
	if session := off.Acquire(NewId(), sequenceTlsRoleClient, false); session != nil {
		t.Fatal("an Off client made a session")
	}
	if off.requireEncryptionForPeer(NewId()) {
		t.Fatal("an Off client requires encryption")
	}
}

// The control plane has no session, so no latch reaches it.
func TestStickyRequiredLeavesNoSessionPeersExempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := newStickyClient(t, ctx, EncryptionModeOpportunistic, true).encryptionSessionManager

	if !manager.SendNoSession(ControlId) || !manager.ReceiveNoSession(ControlId) {
		t.Fatal("the control plane expects a session")
	}
	if manager.requireEncryptionForPeer(ControlId) {
		t.Fatal("the control plane requires encryption")
	}
}

// A multi client's ping budget waits for the cipher where a send does, so it reads
// the latch for its own destination, and only that one.
func TestStickyRequiredReachesTheMultiClientPingBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := newStickyClient(t, ctx, EncryptionModeOpportunistic, true)
	sealedId := NewId()
	channel := func(destinationId Id) *multiClientChannel {
		return &multiClientChannel{
			client: client,
			args:   &multiClientChannelArgs{Destination: RequireMultiHopId(destinationId)},
		}
	}

	if channel(sealedId).requiresEncryption() {
		t.Fatal("a channel to an unsealed peer requires encryption")
	}
	session := client.encryptionSessionManager.getOrCreate(sealedId, sequenceTlsRoleClient, false)
	establishHoldSealer(t, session)()
	if !channel(sealedId).requiresEncryption() {
		t.Fatal("a channel to a latched peer does not require encryption")
	}
	if channel(NewId()).requiresEncryption() {
		t.Fatal("the latch reached another destination")
	}
	if (&multiClientChannel{client: client}).requiresEncryption() {
		t.Fatal("a channel with no destination requires encryption")
	}
	if (&multiClientChannel{}).requiresEncryption() {
		t.Fatal("a bare channel requires encryption")
	}
}
