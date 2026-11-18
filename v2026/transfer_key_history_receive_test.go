// Signed-history admission protects inbound ciphers and every queued application
// dispatch while preserving plaintext bootstrap, rekey overlap, and packet custody.
package connect

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

// Owns a real receive pump with deterministic established cipher state. History
// resolution remains parked independently of that pump until fixture cleanup.
type inboundHistoryFixture struct {
	t          *testing.T
	ctx        context.Context
	client     *Client
	peerId     Id
	session    *peerEncryptionSession
	current    *sequenceCipher
	prior      *sequenceCipher
	inbound    Route
	delivered  []string
	packBudget *TransferMemoryBudget
}

// Constructs the real manager, route decoder, sequence admission, and callback.
func newInboundHistoryFixture(t *testing.T, configure func(*ClientSettings)) *inboundHistoryFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f := &inboundHistoryFixture{t: t, ctx: ctx, peerId: NewId(), inbound: make(Route, 16),
		packBudget: NewTransferMemoryBudget(64 * 1024)}
	settings := DefaultClientSettings()
	settings.Log = NewNoopLogger()
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	settings.EncryptionSettings.Mode = EncryptionModeRequired
	settings.EncryptionSettings.NewPeerClientKeyHistoryFetcher = func(Id) func(context.Context) ([][]byte, error) {
		return func(ctx context.Context) ([][]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}
	settings.ReceiveBufferSettings.IdleTimeout = time.Hour
	settings.ReceiveBufferSettings.GapTimeout = time.Hour
	settings.ReceiveBufferSettings.WriteTimeout = 0
	settings.ReceiveBufferSettings.PackQueueBudget = f.packBudget
	settings.ReceiveBufferSettings.PackQueueRetainedByteAccounting = true
	if configure != nil {
		configure(settings)
	}
	f.client = NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	f.client.ContractManager().AddNoContractPeer(f.peerId)
	f.client.RouteManager().UpdateTransport(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{f.inbound})
	f.client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			message, err := FromFrame(frame)
			if err != nil {
				t.Error(err)
				continue
			}
			if simple, ok := message.(*protocol.SimpleMessage); ok {
				f.delivered = append(f.delivered, simple.Content)
			}
		}
	})
	f.session = f.client.EncryptionSessionManager().Acquire(f.peerId, sequenceTlsRoleServer, false)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x57}, ed25519.SeedSize))
	f.session.SetPeerClientPublicKey(key.Public().(ed25519.PublicKey))
	f.prior = f.installEpoch(0x31)
	f.current = f.installEpoch(0x32)
	t.Cleanup(func() {
		f.session.Release()
		cancel()
		if err := f.client.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		for len(f.inbound) != 0 {
			MessagePoolReturn(<-f.inbound)
		}
		if f.packBudget.UsedByteCount() != 0 {
			t.Error("receive handoff retained budget after shutdown")
		}
	})
	return f
}

// Promotes an identity-verified epoch through the production rekey transition.
func (self *inboundHistoryFixture) installEpoch(seed byte) *sequenceCipher {
	self.t.Helper()
	block, err := aes.NewCipher(bytes.Repeat([]byte{seed}, 32))
	if err != nil {
		self.t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		self.t.Fatal(err)
	}
	codec := &sequenceCipher{aead: aead}
	ctx, cancel := context.WithCancel(self.ctx)
	epoch := &tlsHandshakeEpoch{ctx: ctx, cancel: cancel,
		handshakeDone: make(chan struct{}), establishmentDone: make(chan struct{}),
		derivedTlsCipher: codec, peerIdentityVerified: true}
	close(epoch.handshakeDone)
	close(epoch.establishmentDone)
	func() {
		self.session.stateLock.Lock()
		defer self.session.stateLock.Unlock()
		self.session.epoch = epoch
		self.session.markEstablishedWithLock(epoch)
	}()
	return codec
}

// Returns one owned sealed wire root for a caller to return or hand to the route.
func (self *inboundHistoryFixture) wire(codec *sequenceCipher, sequenceId Id, number uint64, content string) []byte {
	self.t.Helper()
	pack := newReceiveLoopTestPack(self.t, sequenceId, false, content)
	pack.SequenceNumber = number
	pack.Head = number == 0
	return self.wirePack(codec, pack)
}

// Borrows a pack and returns its owned sealed wire root.
func (self *inboundHistoryFixture) wirePack(codec *sequenceCipher, pack *protocol.Pack) []byte {
	self.t.Helper()
	path := TransferPath{SourceId: self.peerId, DestinationId: self.client.ClientId()}
	inner, err := proto.Marshal(&protocol.TransferFrame{TransferPath: path.ToProtobuf(), Pack: pack})
	if err != nil {
		self.t.Fatal(err)
	}
	wire, err := codec.SealOuterFrame(path, inner, protocol.SequenceRole_SequenceRoleClient, false)
	if err != nil {
		self.t.Fatal(err)
	}
	return wire
}

// All hint routes must withhold both established and prior ciphers for each
// blocked history outcome, not just the outbound Cipher accessor.
func TestRequiredSignedHistoryBlocksInboundCipherCandidates(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, state := range []clientKeyHistoryState{clientKeyHistoryPending, clientKeyHistoryRejected, clientKeyHistoryStoreUnavailable} {
		synctest.Test(t, func(t *testing.T) {
			f := newInboundHistoryFixture(t, nil)
			switch state {
			case clientKeyHistoryRejected:
				f.session.rejectKeyHistory("synthetic signed-history conflict")
			case clientKeyHistoryStoreUnavailable:
				f.session.blockKeyHistoryStore(errors.New("synthetic pin store failure"))
			}
			companion := false
			for _, codec := range []*sequenceCipher{f.current, f.prior} {
				wire := f.wire(codec, NewId(), 0, "blocked")
				var outer protocol.TransferFrame
				if err := proto.Unmarshal(wire, &outer); err != nil {
					t.Fatal(err)
				}
				MessagePoolReturn(wire)
				for _, hint := range []struct {
					role      protocol.SequenceRole
					companion *bool
				}{
					{role: protocol.SequenceRole_SequenceRoleClient, companion: &companion},
					{role: protocol.SequenceRole_SequenceRoleClient},
					{role: protocol.SequenceRole_SequenceRoleUnknown},
				} {
					opened, _, _, err := f.client.unwrapFrame(f.peerId, hint.role, hint.companion, outer.EncryptedTransferFrame)
					MessagePoolReturn(opened)
					if !errors.Is(err, errInboundKeyHistoryBlocked) {
						t.Errorf("state=%s role=%s: encrypted application admission returned %v", state, hint.role, err)
					}
				}
			}
		})
	}
}

// The real receive loop refuses pending ciphertext without a false lost-cipher
// signal, then accepts a sender retry and the retained prior epoch after approval.
func TestRequiredSignedHistoryReceiveRetriesAfterApproval(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newInboundHistoryFixture(t, nil)
		nacks := 0
		f.session.unknownWrapNackForTest = func(*protocol.EncryptedControl) { nacks++ }
		sequenceId := NewId()
		f.inbound <- f.wire(f.current, sequenceId, 0, "retry")
		synctest.Wait()
		if len(f.delivered) != 0 || nacks != 0 || len(f.client.receiveBuffer.receiveSequences) != 0 {
			t.Fatal("pending history delivered data, created a receive sequence, or requested an unnecessary re-handshake")
		}
		f.session.openKeyHistoryGate(clientKeyHistoryVerified, "")
		f.inbound <- f.wire(f.current, sequenceId, 0, "retry")
		f.inbound <- f.wire(f.prior, sequenceId, 1, "prior epoch")
		synctest.Wait()
		if len(f.delivered) != 2 || f.delivered[0] != "retry" || f.delivered[1] != "prior epoch" || f.client.IsDone() {
			t.Fatalf("verified retry/rekey delivery = %v, client done=%t", f.delivered, f.client.IsDone())
		}
	})
}

// A frame already decrypted and admitted to the real worker channel must be
// rechecked after rejection, with no delivery ACK, retained budget, or parent exit.
func runRequiredSignedHistoryQueuedRejection(t *testing.T, noAck bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequenceId := NewId()
		held := make(chan receiveSequenceId, 1)
		release := make(chan struct{})
		f := newInboundHistoryFixture(t, func(settings *ClientSettings) {
			settings.ReceiveBufferSettings.beforeRunReceiveSequenceForTest = func(id receiveSequenceId) {
				if id.SequenceId == sequenceId {
					held <- id
					<-release
				}
			}
		})
		defer func() {
			if !isClosed(release) {
				close(release)
			}
		}()
		f.session.openKeyHistoryGate(clientKeyHistoryVerified, "")
		pack := newReceiveLoopTestPack(t, sequenceId, false, "queued before rejection")
		pack.Nack = noAck
		wire := f.wirePack(f.current, pack)
		witness := MessagePoolShareReadOnly(wire)
		defer func() { MessagePoolReturn(witness) }()
		f.inbound <- wire
		synctest.Wait()
		var id receiveSequenceId
		select {
		case id = <-held:
		default:
			t.Fatal("frame never reached the decrypted receive queue")
		}
		sequence := f.client.receiveBuffer.receiveSequences[id]
		if sequence == nil || sequence.packQueueCount.Load() != 1 {
			t.Fatal("fixture did not retain the decoded pack")
		}
		f.session.rejectKeyHistory("synthetic rejection after decryption")
		close(release)
		synctest.Wait()
		if len(f.delivered) != 0 || sequence.ctx.Err() == nil || f.client.IsDone() ||
			sequence.ackWindow.Pending() || f.packBudget.UsedByteCount() != 0 {
			t.Fatal("queued rejection delivered/acknowledged data, retained ownership, or closed the shared client")
		}
		select {
		case <-sequence.done:
		default:
			t.Fatal("refused receive worker did not join")
		}
		probe := witness
		witness = nil
		if !MessagePoolReturn(probe) {
			t.Fatal("refused decoded pack retained its wire owner")
		}
		// The trusted platform path is an unrelated sibling of this peer.
		pack = newReceiveLoopTestPack(t, NewId(), false, "healthy sibling")
		plain, err := proto.Marshal(&protocol.TransferFrame{
			TransferPath: TransferPath{DestinationId: f.client.ClientId()}.ToProtobuf(), Pack: pack,
		})
		if err != nil {
			t.Fatal(err)
		}
		f.inbound <- plain
		synctest.Wait()
		if len(f.delivered) != 1 || f.delivered[0] != "healthy sibling" {
			t.Fatal("one refused peer stopped unrelated receive delivery")
		}
	})
}

// A queued acknowledged Pack must release ownership without delivery or an ACK.
func TestRequiredSignedHistoryRejectsAlreadyDecryptedPack(t *testing.T) {
	runRequiredSignedHistoryQueuedRejection(t, false)
}

// No-ACK data uses receiveNack before joining the same final admission boundary.
func TestRequiredSignedHistoryRejectsAlreadyDecryptedNoAck(t *testing.T) {
	runRequiredSignedHistoryQueuedRejection(t, true)
}

// Rejection after head assembly must also withdraw a previously held selective
// lease. Neither the ready predecessor nor the drained held item was delivered.
func TestRequiredSignedHistoryRejectsBufferedHeldDelivery(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequenceId := NewId()
		var f *inboundHistoryFixture
		f = newInboundHistoryFixture(t, func(settings *ClientSettings) {
			settings.ReceiveBufferSettings.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
			settings.ReceiveBufferSettings.EvictionNotice = true
			settings.ReceiveBufferSettings.ProgressObserver = func(progress TransferProgressEvent) {
				if progress.Stage == "deliver_begin" && progress.SequenceId == sequenceId {
					f.session.rejectKeyHistory("synthetic rejection after delivery batching")
				}
			}
		})
		f.session.openKeyHistoryGate(clientKeyHistoryVerified, "")
		f.inbound <- f.wire(f.current, sequenceId, 1, "held successor")
		synctest.Wait()
		id := receiveSequenceId{Source: SourceId(f.peerId), SequenceId: sequenceId, EncryptionRole: sequenceTlsRoleServer}
		sequence := f.client.receiveBuffer.receiveSequences[id]
		if sequence == nil {
			t.Fatal("fixture did not create the held receive sequence")
		}
		held := sequence.receiveQueue.PeekFirst()
		if held == nil || held.sequenceNumber != 1 || !held.committed {
			t.Fatal("fixture did not establish selective-acknowledgement custody")
		}
		f.inbound <- f.wire(f.current, sequenceId, 0, "ready predecessor")
		synctest.Wait()
		if len(f.delivered) != 0 || sequence.ctx.Err() == nil || f.client.IsDone() ||
			sequence.receiveQueue.Len() != 0 || len(sequence.deliverItems) != 0 ||
			f.client.ReceiveStats().ReceiveQueueExitWithdrawCount != 1 {
			t.Fatal("buffered history rejection delivered data or lost held retry custody")
		}
		if present, _ := reliableIngressCumulativeHead(sequence); present {
			t.Fatal("refused buffered delivery acquired cumulative acknowledgement")
		}
	})
}

// A controls-only cleanup batch following a refused application batch must not
// cumulatively acknowledge across that gap, even if history opens during cleanup.
func TestRequiredSignedHistoryRefusalKeepsCleanupAckGap(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newInboundHistoryFixture(t, nil)
		queue, ctx := testReceiveDeliveryLedger(t, 4)
		sequence := queue.sequence
		sequence.client, sequence.session = f.client, f.session
		sequence.receiveBufferSettings = DefaultReceiveBufferSettings()
		first, control := testReceiveDeliveryItem(0), testReceiveDeliveryItem(1)
		calls := 0
		first.receiveCallback = func(TransferPath, []*protocol.Frame, Peer) { calls++ }
		sequence.deliverItems = []*receiveItem{first}
		sequence.deliverFrames = append([]*protocol.Frame(nil), first.frames...)
		sequence.flushDeliver()
		f.session.openKeyHistoryGate(clientKeyHistoryVerified, "")
		// A peer/contract boundary can flush the previous batch inside
		// receiveHead before that method appends its current controls-only item.
		sequence.deliverItems = []*receiveItem{control}
		sequence.flushDeliver()
		if calls != 0 || ctx.Err() == nil || first.frames != nil || control.frames != nil {
			t.Fatal("refused delivery escaped the admission gate or retained cleanup ownership")
		}
		if present, _ := reliableIngressCumulativeHead(sequence); present {
			t.Fatal("cleanup controls acknowledged the refused application gap")
		}
	})
}

// A callback already admitted may reject its session without deadlocking. Later
// receipt callbacks in that batch are refused, and only the completed item is acked.
func TestRequiredSignedHistoryRejectsLaterReceiptDispatch(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newInboundHistoryFixture(t, nil)
		f.session.openKeyHistoryGate(clientKeyHistoryVerified, "")
		queue, ctx := testReceiveDeliveryLedger(t, 4)
		sequence := queue.sequence
		sequence.client, sequence.session, sequence.deliveryQueue = f.client, f.session, queue
		sequence.receiveBufferSettings = DefaultReceiveBufferSettings()
		first, later := testReceiveDeliveryItem(0), testReceiveDeliveryItem(1)
		firstId := first.messageId
		calls := 0
		first.receiveCallback = func(TransferPath, []*protocol.Frame, Peer) {
			calls++
			f.session.rejectKeyHistory("synthetic rejection inside admitted callback")
		}
		later.receiveCallback = func(TransferPath, []*protocol.Frame, Peer) { calls++ }
		if !queue.admit(first) || !queue.admit(later) {
			t.Fatal("fixture did not reserve both receipt owners")
		}
		sequence.deliverItems = []*receiveItem{first, later}
		sequence.deliverFrames = append(append([]*protocol.Frame(nil), first.frames...), later.frames...)
		sequence.flushDeliver()
		if calls != 1 || ctx.Err() == nil || f.client.IsDone() || first.frames != nil || later.frames != nil ||
			len(queue.admitted) != 0 || queue.pending() || queue.bytes != 0 {
			t.Fatal("rejection failed to isolate later receipt delivery and release its admission")
		}
		if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != firstId {
			t.Fatal("receipt rejection acknowledged more or less than the already-admitted callback")
		}
	})
}

// A mixed bootstrap/application item may finish control processing while the
// signed-history gate is pending, but plaintext application content stays filtered.
func TestRequiredSignedHistoryKeepsPlaintextBootstrap(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newInboundHistoryFixture(t, nil)
		ecBytes, err := proto.Marshal(&protocol.EncryptedControl{
			ControlType: protocol.EncryptedControlType_EncryptedControlIdentityProof,
			SessionRole: protocol.SequenceRole_SequenceRoleClient,
			Payload:     []byte("synthetic already-verified proof"),
		})
		if err != nil {
			t.Fatal(err)
		}
		pack := newReceiveLoopTestPack(t, NewId(), false, "plaintext must stay hidden")
		pack.Frames = append(pack.Frames, &protocol.Frame{
			MessageType: protocol.MessageType_TransferEncryptedControl, MessageBytes: ecBytes,
		})
		plain, err := proto.Marshal(&protocol.TransferFrame{
			TransferPath: TransferPath{SourceId: f.peerId, DestinationId: f.client.ClientId()}.ToProtobuf(), Pack: pack,
		})
		if err != nil {
			t.Fatal(err)
		}
		f.inbound <- plain
		synctest.Wait()
		if len(f.delivered) != 0 || f.client.IsDone() || len(f.client.receiveBuffer.receiveSequences) != 1 {
			t.Fatal("history admission blocked plaintext bootstrap or exposed plaintext application content")
		}
		for _, sequence := range f.client.receiveBuffer.receiveSequences {
			if present, _ := reliableIngressCumulativeHead(sequence); !present || sequence.ctx.Err() != nil {
				t.Fatal("bootstrap did not complete its existing control/ack path")
			}
		}
	})
}

// A real TLS/exporter-proof exchange can complete while both evidence requests
// are parked. Approving history then releases the original application send.
func TestRequiredSignedHistoryBootstrapFinishesBeforeAdmission(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		releaseHistory := make(chan struct{})
		a, b, _, bId, _, delivered := requiredGatePair(ctx, EncryptionModeRequired, EncryptionModeRequired,
			func(settings *ClientSettings) {
				settings.Log = NewNoopLogger()
				settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
				settings.EncryptionSettings.NewPeerClientKeyHistoryFetcher = func(Id) func(context.Context) ([][]byte, error) {
					return func(ctx context.Context) ([][]byte, error) {
						select {
						case <-ctx.Done():
							return nil, ctx.Err()
						case <-releaseHistory:
							return nil, nil
						}
					}
				}
			}, true)
		t.Cleanup(func() {
			cancel()
			for _, client := range []*Client{a, b} {
				if err := client.CloseAndWait(context.Background()); err != nil {
					t.Error(err)
				}
			}
			synctest.Wait()
		})
		// The contract fixture supplies destination keys only. Commit both
		// legitimate peers before the exchange so this test controls history
		// admission independently of companion-contract key discovery.
		sessions := []*peerEncryptionSession{
			a.EncryptionSessionManager().Acquire(bId, sequenceTlsRoleClient, false),
			b.EncryptionSessionManager().Acquire(a.ClientId(), sequenceTlsRoleServer, false),
		}
		for _, session := range sessions {
			t.Cleanup(session.Release)
		}
		sessions[0].SetPeerClientPublicKey(b.ClientKeyManager().PublicKey())
		sessions[1].SetPeerClientPublicKey(a.ClientKeyManager().PublicKey())
		var established []chan struct{}
		for _, client := range []*Client{a, b} {
			ready := make(chan struct{}, 1)
			established = append(established, ready)
			remove := client.EncryptionSessionManager().AddEncryptionEventCallback(func(event *EncryptionEvent) {
				if event.Type == EncryptionEventSealed {
					select {
					case ready <- struct{}{}:
					default:
					}
				}
			})
			t.Cleanup(remove)
		}
		frame := requiredGateFrame(t, "approved after bootstrap")
		sent := make(chan bool, 1)
		go func() {
			accepted := a.SendWithTimeout(frame, bId, func(error) {}, -1)
			if !accepted {
				MessagePoolReturn(frame.MessageBytes)
			}
			sent <- accepted
		}()
		for _, ready := range established {
			select {
			case <-ready:
			case <-ctx.Done():
				for _, client := range []*Client{a, b} {
					for _, state := range client.EncryptionSessionManager().PeerEncryptionStates() {
						t.Logf("handshake state: %+v", *state)
					}
				}
				t.Fatal("pending history blocked the real TLS/identity bootstrap")
			}
		}
		synctest.Wait()
		for _, session := range sessions {
			if session == nil {
				t.Fatal("pending history prevented the real handshake session from opening")
			}
			func() {
				session.stateLock.Lock()
				defer session.stateLock.Unlock()
				if session.establishedEpoch == nil || !session.establishedEpoch.peerIdentityVerified ||
					session.keyHistoryState != clientKeyHistoryPending {
					t.Fatal("TLS/identity bootstrap did not finish independently of pending history")
				}
			}()
		}
		select {
		case <-delivered:
			t.Fatal("pending history delivered application data")
		default:
		}
		select {
		case <-sent:
			t.Fatal("the Required application sender escaped its pending admission")
		default:
		}
		close(releaseHistory)
		select {
		case accepted := <-sent:
			if !accepted {
				t.Fatal("approved history refused the parked application send")
			}
		case <-ctx.Done():
			t.Fatal("approved history left the application sender parked")
		}
		select {
		case content := <-delivered:
			if content != "approved after bootstrap" {
				t.Fatal("wrong application content after history admission")
			}
		case <-ctx.Done():
			t.Fatal("approved history did not deliver the original application message")
		}
	})
}
