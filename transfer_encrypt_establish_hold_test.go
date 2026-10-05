package connect

// Pins for the Opportunistic establish hold (EncryptionSettings.
// OpportunisticEstablishHold): under EncryptionModeOpportunistic, an
// application send waits at SendSequence.Pack's entry for the session's first
// cipher, for at most the hold, measured from the session's first
// establishment attempt. Each test reads the wire itself: every message the
// sender writes toward the peer passes a tap that records whether it was
// sealed and, if not, which frames it carried in the clear.
//
//   - a peer that answers: no application frame crosses the wire readable;
//   - a peer that never answers: plaintext starts at the deadline, not before,
//     and later sends do not wait again;
//   - an establishment failure ends the hold at once;
//   - a zero budget does not wait, and a budget that ends inside the hold is
//     not-sent with no error, never plaintext;
//   - zero hold is today's Opportunistic: plaintext before the session seals;
//   - Required with a hold set still refuses and never sends plaintext;
//   - the no-acknowledgement fast path, which writes on the caller's goroutine,
//     is held the same way, and refuses on its own when a hold began after
//     Pack's entry check.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// one message the sender wrote toward the peer, as the tap read it
type establishHoldWireMessage struct {
	at     time.Time
	sealed bool
	// frames readable on the wire, by kind
	application int
	control     int
}

type establishHoldWire struct {
	mutex    sync.Mutex
	messages []establishHoldWireMessage
}

func (self *establishHoldWire) record(at time.Time, wireBytes []byte) {
	message := establishHoldWireMessage{at: at}
	var transferFrame protocol.TransferFrame
	if err := ProtoUnmarshal(wireBytes, &transferFrame); err != nil {
		return
	}
	if 0 < len(transferFrame.GetEncryptedTransferFrame()) {
		message.sealed = true
	} else {
		pack := transferFrame.GetPack()
		if pack == nil && transferFrame.GetFrame() != nil &&
			transferFrame.GetFrame().GetMessageType() == protocol.MessageType_TransferPack {
			var framePack protocol.Pack
			if err := ProtoUnmarshal(transferFrame.GetFrame().GetMessageBytes(), &framePack); err == nil {
				pack = &framePack
			}
		}
		for _, frame := range pack.GetFrames() {
			switch frame.GetMessageType() {
			case protocol.MessageType_TestSimpleMessage:
				message.application += 1
			case protocol.MessageType_TransferEncryptedControl:
				message.control += 1
			}
		}
	}
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.messages = append(self.messages, message)
}

func (self *establishHoldWire) snapshot() []establishHoldWireMessage {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return append([]establishHoldWireMessage(nil), self.messages...)
}

// counts: sealed messages, readable application frames, readable handshake
// frames, and when the first readable application frame was written (zero if
// none was)
func (self *establishHoldWire) counts() (sealed int, application int, control int, firstApplication time.Time) {
	for _, message := range self.snapshot() {
		if message.sealed {
			sealed += 1
		}
		application += message.application
		control += message.control
		if 0 < message.application && firstApplication.IsZero() {
			firstApplication = message.at
		}
	}
	return
}

// requiredGatePair's two clients, with a tap on everything a writes toward b
func establishHoldPair(
	t *testing.T,
	ctx context.Context,
	aMode EncryptionMode,
	bMode EncryptionMode,
	mutateA func(*ClientSettings),
) (a *Client, b *Client, bClientId Id, wire *establishHoldWire, receivesB chan string) {
	t.Helper()
	aClientId := NewId()
	bClientId = NewId()

	aSend := make(chan []byte)
	tapped := make(chan []byte)
	bSend := make(chan []byte)
	wire = &establishHoldWire{}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case wireBytes, ok := <-aSend:
				if !ok {
					return
				}
				wire.record(time.Now(), wireBytes)
				select {
				case tapped <- wireBytes:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	_, bReceive := newConditioner(ctx, tapped)
	_, aReceive := newConditioner(ctx, bSend)

	provideModes := map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true}

	makeSettings := func(mode EncryptionMode, mutate func(*ClientSettings)) *ClientSettings {
		s := DefaultClientSettingsWithBufferSize(64)
		s.SendBufferSettings.AckTimeout = 60 * time.Second
		s.SendBufferSettings.IdleTimeout = 60 * time.Second
		s.SendBufferSettings.MinResendInterval = 10 * time.Millisecond
		s.ReceiveBufferSettings.GapTimeout = 60 * time.Second
		s.ReceiveBufferSettings.IdleTimeout = 60 * time.Second
		s.ForwardBufferSettings.IdleTimeout = 1 * time.Second
		s.ContractManagerSettings.LegacyCreateContract = false
		s.EncryptionSettings.Mode = mode
		s.EncryptionSettings.TlsTimeout = 30 * time.Second
		s.EncryptionSettings.EncryptionControlUseCompanion = true
		if mutate != nil {
			mutate(s)
		}
		return s
	}

	aOob := &grantingClientOob{
		sourceId: aClientId,
		settings: DefaultContractManagerSettings(),
		destSecretKey: func(destinationId Id) ([]byte, bool) {
			return b.ContractManager().GetProvideSecretKey(protocol.ProvideMode_Network)
		},
		destClientPublicKey: func(destinationId Id) []byte {
			return b.ClientKeyManager().PublicKey()
		},
	}
	bOob := &grantingClientOob{
		sourceId: bClientId,
		settings: DefaultContractManagerSettings(),
		destSecretKey: func(destinationId Id) ([]byte, bool) {
			return a.ContractManager().GetProvideSecretKey(protocol.ProvideMode_Network)
		},
		destClientPublicKey: func(destinationId Id) []byte {
			return a.ClientKeyManager().PublicKey()
		},
	}

	a = NewClient(ctx, aClientId, aOob, makeSettings(aMode, mutateA))
	a.RouteManager().UpdateTransport(newDataGatewayTransport(), []Route{aSend})
	a.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{aReceive})
	blackholeControlId(ctx, a.RouteManager())
	a.ContractManager().SetProvideModes(provideModes)

	b = NewClient(ctx, bClientId, bOob, makeSettings(bMode, nil))
	b.RouteManager().UpdateTransport(newDataGatewayTransport(), []Route{bSend})
	b.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{bReceive})
	blackholeControlId(ctx, b.RouteManager())
	b.ContractManager().SetProvideModes(provideModes)

	receivesB = make(chan string, 1024)
	b.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			if m, err := FromFrame(frame); err == nil {
				if sm, ok := m.(*protocol.SimpleMessage); ok {
					select {
					case receivesB <- sm.Content:
					default:
					}
				}
			}
		}
	})
	t.Cleanup(func() {
		a.Cancel()
		b.Cancel()
	})
	return
}

func establishHoldSettings(hold time.Duration) func(*ClientSettings) {
	return func(s *ClientSettings) {
		s.EncryptionSettings.OpportunisticEstablishHold = hold
	}
}

// the session a's sends to the peer ride, once the first send has made it
func establishHoldSessionStart(t *testing.T, a *Client, peerId Id) time.Time {
	t.Helper()
	session := a.encryptionSessionManager.Lookup(
		peerId, sequenceTlsRoleClient, a.settings.DefaultTransferOpts.CompanionContract,
	)
	if session == nil {
		t.Fatal("the send made no per-peer session")
	}
	session.stateLock.Lock()
	defer session.stateLock.Unlock()
	if session.establishHoldStart.IsZero() {
		t.Fatal("the session's first establishment never started")
	}
	return session.establishHoldStart
}

func establishHoldSealed(a *Client, peerId Id) bool {
	for _, state := range a.encryptionSessionManager.PeerEncryptionStates() {
		if state.PeerId == peerId && state.Sealed {
			return true
		}
	}
	return false
}

func establishHoldReceive(t *testing.T, receives chan string, want string, within time.Duration) {
	t.Helper()
	select {
	case got := <-receives:
		AssertEqual(t, want, got)
	case <-time.After(within):
		t.Fatalf("the peer never received %q", want)
	}
}

// A peer that answers the handshake is sealed from the first application
// frame: every application frame the sender writes is sealed. Without the
// hold, Opportunistic writes the first ones readable while the handshake runs
// (TestEstablishHoldZeroIsTodaysOpportunistic). The readable handshake frames
// are the tap's control: they show the tap reads plaintext when there is any.
func TestEstablishHoldSealsFromTheFirstApplicationFrame(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a, _, bClientId, wire, receivesB := establishHoldPair(
		t, ctx, EncryptionModeOpportunistic, EncryptionModeOpportunistic, establishHoldSettings(5*time.Second),
	)

	labels := []string{"first", "second", "third", "fourth", "fifth"}
	for _, label := range labels {
		if !a.SendWithTimeout(requiredGateFrame(t, label), bClientId, func(error) {}, -1) {
			t.Fatalf("the %s send was not admitted", label)
		}
	}
	for _, label := range labels {
		establishHoldReceive(t, receivesB, label, 15*time.Second)
	}
	// count once the session has sealed, when every handshake frame has been
	// written, so the control below cannot fire for want of time
	for sealDeadline := time.Now().Add(15 * time.Second); !establishHoldSealed(a, bClientId); {
		if sealDeadline.Before(time.Now()) {
			t.Fatal("the session never sealed with a peer that answers")
		}
		time.Sleep(10 * time.Millisecond)
	}

	sealed, application, control, firstApplication := wire.counts()
	if application != 0 {
		t.Fatalf("%d application frame(s) crossed the wire readable, the first at %s; the hold let them out before the session sealed",
			application, firstApplication.Format(time.RFC3339Nano))
	}
	if control == 0 {
		t.Fatal("the tap read no handshake frame in the clear, so it cannot tell a readable application frame either")
	}
	// every send arrived and none crossed readable, so each rode a sealed
	// message (a pack may carry several)
	if sealed == 0 {
		t.Fatal("no sealed message crossed the wire")
	}
	t.Logf("%d sends delivered in %d sealed message(s); %d handshake frame(s) readable", len(labels), sealed, control)
}

// A peer that never answers (mode Off drops every ClientHello) gets plaintext
// at the hold's deadline, counted from the session's first establishment
// attempt, and not before. The hold belongs to the session: a send after the
// deadline does not wait again.
func TestEstablishHoldFallsThroughAtTheDeadlineForASilentPeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hold := 1 * time.Second
	a, _, bClientId, wire, receivesB := establishHoldPair(
		t, ctx, EncryptionModeOpportunistic, EncryptionModeOff, establishHoldSettings(hold),
	)

	if !a.SendWithTimeout(requiredGateFrame(t, "held"), bClientId, func(error) {}, -1) {
		t.Fatal("the held send was not admitted")
	}
	returned := time.Now()
	start := establishHoldSessionStart(t, a, bClientId)
	if returned.Before(start.Add(hold)) {
		t.Fatalf("the held send returned %s after the session's first establishment attempt, inside the %s hold",
			returned.Sub(start), hold)
	}
	establishHoldReceive(t, receivesB, "held", 15*time.Second)
	_, application, _, firstApplication := wire.counts()
	if application == 0 {
		t.Fatal("no application frame crossed the wire, so the hold did not fall through")
	}
	if firstApplication.Before(start.Add(hold)) {
		t.Fatalf("the first application frame crossed the wire readable %s after the first establishment attempt, before the %s deadline",
			firstApplication.Sub(start), hold)
	}
	if start.Add(hold + 2*time.Second).Before(firstApplication) {
		t.Fatalf("the first application frame crossed the wire %s after the first establishment attempt, long past the %s deadline",
			firstApplication.Sub(start), hold)
	}

	// later sends to the same session do not wait again
	sendStart := time.Now()
	if !a.SendWithTimeout(requiredGateFrame(t, "after"), bClientId, func(error) {}, -1) {
		t.Fatal("the send after the deadline was not admitted")
	}
	if waited := time.Since(sendStart); hold/2 <= waited {
		t.Fatalf("a send after the deadline waited %s: the hold ran again for the same session", waited)
	}
	establishHoldReceive(t, receivesB, "after", 15*time.Second)
}

// The deadline runs from the session's first establishment attempt. A later
// attempt that starts while the hold is on (a new ClientHello for a server
// role, or a peer on a newer generation) does not move it.
func TestEstablishHoldRunsFromTheFirstEstablishmentAttempt(t *testing.T) {
	session, cleanup := newTestEncryptionSession(t, sequenceTlsRoleClient)
	defer cleanup()
	settings := *session.settings
	settings.OpportunisticEstablishHold = 10 * time.Second
	session.settings = &settings

	session.restartHandshake()
	holding, _, first := session.establishHoldState(time.Now())
	if !holding {
		t.Fatal("no hold after the first establishment attempt started")
	}
	time.Sleep(50 * time.Millisecond)
	session.reset()
	holding, _, second := session.establishHoldState(time.Now())
	if !holding {
		t.Fatal("no hold after the second establishment attempt started")
	}
	if !second.Equal(first) {
		t.Fatalf("a second establishment attempt moved the hold's deadline by %s", second.Sub(first))
	}
}

// An establishment attempt that fails before the deadline ends the hold at
// once. Here the attempt is bounded by TlsTimeout, far inside a long hold.
func TestEstablishHoldEndsAtOnceOnAnEstablishmentFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hold := 20 * time.Second
	tlsTimeout := 400 * time.Millisecond
	a, _, bClientId, wire, receivesB := establishHoldPair(
		t, ctx, EncryptionModeOpportunistic, EncryptionModeOff,
		func(s *ClientSettings) {
			s.EncryptionSettings.OpportunisticEstablishHold = hold
			s.EncryptionSettings.TlsTimeout = tlsTimeout
		},
	)

	sent := make(chan bool, 1)
	go func() {
		sent <- a.SendWithTimeout(requiredGateFrame(t, "after-failure"), bClientId, func(error) {}, -1)
	}()
	select {
	case ok := <-sent:
		if !ok {
			t.Fatal("the send was not admitted after the establishment failure")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the send was still held 5s in, with the establishment failed at %s: a failure did not end the %s hold",
			tlsTimeout, hold)
	}
	start := establishHoldSessionStart(t, a, bClientId)
	establishHoldReceive(t, receivesB, "after-failure", 15*time.Second)
	_, _, _, firstApplication := wire.counts()
	if firstApplication.Before(start.Add(tlsTimeout - 50*time.Millisecond)) {
		t.Fatalf("the application frame crossed the wire %s after the first establishment attempt, before the attempt failed at %s",
			firstApplication.Sub(start), tlsTimeout)
	}
}

// The caller's budget, inside a hold: zero does not wait, and a budget that
// ends first is not-sent with no error (backpressure, so a caller retries
// rather than resets). Neither writes anything readable.
func TestEstablishHoldShortBudgetsReturnNotSentWithoutError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hold := 3 * time.Second
	a, _, bClientId, wire, receivesB := establishHoldPair(
		t, ctx, EncryptionModeOpportunistic, EncryptionModeOff, establishHoldSettings(hold),
	)

	sendStart := time.Now()
	admitted, err := a.SendWithTimeoutDetailed(requiredGateFrame(t, "zero"), bClientId, func(error) {}, 0)
	if waited := time.Since(sendStart); 500*time.Millisecond <= waited {
		t.Fatalf("a zero-budget send waited %s inside the hold", waited)
	}
	if admitted || err != nil {
		t.Fatalf("a zero-budget send inside the hold returned admitted=%t err=%v, want not-sent with no error", admitted, err)
	}

	budget := 400 * time.Millisecond
	sendStart = time.Now()
	admitted, err = a.SendWithTimeoutDetailed(requiredGateFrame(t, "budget"), bClientId, func(error) {}, budget)
	waited := time.Since(sendStart)
	if admitted || err != nil {
		t.Fatalf("a %s-budget send inside the hold returned admitted=%t err=%v, want not-sent with no error", budget, admitted, err)
	}
	if waited < budget-50*time.Millisecond || 2*time.Second <= waited {
		t.Fatalf("a %s-budget send returned after %s", budget, waited)
	}

	start := establishHoldSessionStart(t, a, bClientId)
	if wait := time.Until(start.Add(hold)) - 200*time.Millisecond; 0 < wait {
		time.Sleep(wait)
	}
	if _, application, _, _ := wire.counts(); application != 0 {
		t.Fatalf("%d application frame(s) crossed the wire readable inside the hold", application)
	}
	select {
	case got := <-receivesB:
		t.Fatalf("the peer received %q, sent by a send that returned not-sent", got)
	default:
	}
}

// Zero, the default, is today's Opportunistic: application frames cross the
// wire readable while the session establishes, and no send enters the hold.
func TestEstablishHoldZeroIsTodaysOpportunistic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var holdWaits atomic.Int32
	a, _, bClientId, wire, receivesB := establishHoldPair(
		t, ctx, EncryptionModeOpportunistic, EncryptionModeOpportunistic,
		func(s *ClientSettings) {
			s.EncryptionSettings.OpportunisticEstablishHold = 0
			s.SendBufferSettings.beforeEstablishHoldWaitForTest = func(sendSequenceId) {
				holdWaits.Add(1)
			}
		},
	)

	labels := []string{"first", "second", "third"}
	sendStart := time.Now()
	for _, label := range labels {
		if !a.SendWithTimeout(requiredGateFrame(t, label), bClientId, func(error) {}, -1) {
			t.Fatalf("the %s send was not admitted", label)
		}
	}
	if waited := time.Since(sendStart); time.Second <= waited {
		t.Fatalf("three sends took %s with no hold", waited)
	}
	for _, label := range labels {
		establishHoldReceive(t, receivesB, label, 15*time.Second)
	}
	if _, application, _, _ := wire.counts(); application == 0 {
		t.Fatal("no application frame crossed the wire readable: zero hold held the sends, which is not today's Opportunistic")
	}
	if n := holdWaits.Load(); n != 0 {
		t.Fatalf("%d send(s) entered the hold with the hold at zero", n)
	}
}

// Required with a hold set is still Required: a zero budget is refused with
// ErrEncryptionRequiredNotEstablished, a budget longer than the hold is
// refused at the budget and not let out at the hold's deadline, and nothing
// crosses the wire readable.
func TestEstablishHoldLeavesRequiredUnchanged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hold := 500 * time.Millisecond
	a, _, bClientId, wire, receivesB := establishHoldPair(
		t, ctx, EncryptionModeRequired, EncryptionModeOff, establishHoldSettings(hold),
	)

	admitted, err := a.SendWithTimeoutDetailed(requiredGateFrame(t, "zero"), bClientId, func(error) {}, 0)
	if admitted || !errors.Is(err, ErrEncryptionRequiredNotEstablished) {
		t.Fatalf("Required with a hold: a zero-budget send returned admitted=%t err=%v, want ErrEncryptionRequiredNotEstablished", admitted, err)
	}

	budget := 1500 * time.Millisecond
	sendStart := time.Now()
	admitted, err = a.SendWithTimeoutDetailed(requiredGateFrame(t, "budget"), bClientId, func(error) {}, budget)
	waited := time.Since(sendStart)
	if admitted || !errors.Is(err, ErrEncryptionRequiredNotEstablished) {
		t.Fatalf("Required with a hold: a %s-budget send returned admitted=%t err=%v after %s, want ErrEncryptionRequiredNotEstablished",
			budget, admitted, err, waited)
	}
	if waited < budget-50*time.Millisecond {
		t.Fatalf("Required with a hold refused after %s, before its %s budget", waited, budget)
	}

	select {
	case got := <-receivesB:
		t.Fatalf("Required with a hold delivered %q to a peer with no session", got)
	case <-time.After(500 * time.Millisecond):
	}
	if _, application, _, _ := wire.counts(); application != 0 {
		t.Fatalf("Required with a hold wrote %d application frame(s) readable", application)
	}
}

// the no-acknowledgement fast path's harness, with an Opportunistic session in
// its establish hold: started now, unsealed, nothing failed
func establishHoldFastPathHarness(t *testing.T, ctx context.Context, hold time.Duration) (*noAckFastPathHarness, *peerEncryptionSession) {
	t.Helper()
	harness := newNoAckFastPathHarness(t, ctx, 4)
	settings := DefaultEncryptionSettings()
	settings.Mode = EncryptionModeOpportunistic
	settings.OpportunisticEstablishHold = hold
	keyManager, err := NewClientKeyManager(ctx, harness.client)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewEncryptionSessionManager(ctx, harness.client, keyManager, settings)
	session := newPeerEncryptionSession(
		ctx, manager, harness.client, harness.destinationId, sequenceTlsRoleClient,
		settings, manager.ClientTlsConfig(), false,
	)
	session.stateLock.Lock()
	session.establishHoldStart = time.Now()
	session.stateLock.Unlock()
	harness.sequence.session = session
	t.Cleanup(func() { harness.sequence.session = nil })
	return harness, session
}

// returns what seals the session as a verified identity proof does; it is
// safe to call from any goroutine
func establishHoldSealer(t *testing.T, session *peerEncryptionSession) func() {
	t.Helper()
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	epochCtx, epochCancel := context.WithCancel(session.ctx)
	epoch := &tlsHandshakeEpoch{
		ctx: epochCtx, cancel: epochCancel, epochId: NewId(),
		handshakeDone: make(chan struct{}), establishmentDone: make(chan struct{}),
		derivedTlsCipher: &sequenceCipher{aead: aead}, peerIdentityVerified: true,
	}
	return func() {
		session.stateLock.Lock()
		defer session.stateLock.Unlock()
		session.epoch = epoch
		session.markEstablishedWithLock(epoch)
	}
}

// the one message on the route: whether it was sealed; fails if there is none
// or more than one
func establishHoldTakeOne(t *testing.T, route chan []byte) bool {
	t.Helper()
	var wireBytes []byte
	select {
	case wireBytes = <-route:
	default:
		t.Fatal("nothing reached the route")
	}
	defer MessagePoolReturn(wireBytes)
	select {
	case extra := <-route:
		MessagePoolReturn(extra)
		t.Fatal("more than one message reached the route")
	default:
	}
	var transferFrame protocol.TransferFrame
	if err := ProtoUnmarshal(wireBytes, &transferFrame); err != nil {
		t.Fatalf("the route holds no transfer frame: %v", err)
	}
	return 0 < len(transferFrame.GetEncryptedTransferFrame())
}

// The fast path writes a no-acknowledgement pack on the caller's goroutine,
// after Pack's entry. A session that seals inside the hold gets that pack
// sealed; one that never seals gets it in plaintext at the deadline. No send
// loop runs here, so the fast path is the only way to the wire.
func TestEstablishHoldCoversTheNoAckFastPath(t *testing.T) {
	t.Run("sealed in time", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		harness, session := establishHoldFastPathHarness(t, ctx, 5*time.Second)
		sealAfter := 300 * time.Millisecond
		seal := establishHoldSealer(t, session)
		go func() {
			time.Sleep(sealAfter)
			seal()
		}()
		sendStart := time.Now()
		frame := noAckFastPathTestFrame(t)
		admitted, err := harness.client.SendWithTimeoutDetailed(frame, harness.destinationId, nil, 5*time.Second, NoAck())
		waited := time.Since(sendStart)
		if err != nil || !admitted {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatalf("admitted=%t err=%v", admitted, err)
		}
		if waited < sealAfter-50*time.Millisecond {
			t.Fatalf("the no-ack send returned after %s, before the session sealed at %s", waited, sealAfter)
		}
		if !establishHoldTakeOne(t, harness.route) {
			t.Fatal("the no-ack pack was written readable, though the session sealed inside the hold")
		}
		if n := harness.client.ReceiveStats().SendNoAckFastPathWriteCount; n != 1 {
			t.Fatalf("%d fast-path writes, want 1", n)
		}
	})
	t.Run("never sealed", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		hold := 600 * time.Millisecond
		harness, session := establishHoldFastPathHarness(t, ctx, hold)
		frame := noAckFastPathTestFrame(t)
		admitted, err := harness.client.SendWithTimeoutDetailed(frame, harness.destinationId, nil, 5*time.Second, NoAck())
		returned := time.Now()
		if err != nil || !admitted {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatalf("admitted=%t err=%v", admitted, err)
		}
		session.stateLock.Lock()
		start := session.establishHoldStart
		session.stateLock.Unlock()
		if returned.Before(start.Add(hold)) {
			t.Fatalf("the no-ack send returned %s into a %s hold", returned.Sub(start), hold)
		}
		if establishHoldTakeOne(t, harness.route) {
			t.Fatal("the no-ack pack was sealed by a session that never sealed")
		}
	})
	// The fast path's own check, called the way Pack calls it: a hold that is
	// on when the fast path runs gets no write. The control, the same pack
	// once the hold is over, is written.
	t.Run("its own check", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		harness, session := establishHoldFastPathHarness(t, ctx, 30*time.Second)
		frame := noAckFastPathTestFrame(t)
		sendPack := &SendPack{
			TransferOptions:  TransferOptions{Ack: false},
			Frame:            frame,
			Destination:      harness.destinationId,
			AckCallback:      func(error) {},
			MessageByteCount: ByteCount(len(frame.MessageBytes)),
			Ctx:              ctx,
		}
		snapshot := harness.sequence.readNoAckFastPath(sendPack)
		if snapshot == nil {
			t.Fatal("no fast path snapshot")
		}
		if harness.sequence.writeNoAckFastPath(snapshot, sendPack) {
			t.Fatal("the fast path wrote a no-ack pack inside the hold")
		}
		select {
		case wireBytes := <-harness.route:
			MessagePoolReturn(wireBytes)
			t.Fatal("the fast path refused, yet a message reached the route")
		default:
		}

		session.stateLock.Lock()
		session.establishHoldStart = time.Now().Add(-time.Minute)
		session.stateLock.Unlock()
		snapshot = harness.sequence.readNoAckFastPath(sendPack)
		if !harness.sequence.writeNoAckFastPath(snapshot, sendPack) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("control: the fast path did not write the same pack once the hold was over")
		}
		if establishHoldTakeOne(t, harness.route) {
			t.Fatal("control: the pack was sealed by a session with no cipher")
		}
	})
}
