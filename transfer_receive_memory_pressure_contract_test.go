// Signed sender-created heads exercise pressure authority without borrowing
// decoded roots, changing a receive policy, or manufacturing a contract owner.
package connect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// Retain the maintained signed wire-round fixture's codecs, grant authority,
// routes and cleanup. Every setting is installed before either client starts.
func newPressureContractFixture(t *testing.T, rx *TransferMemoryBudget, required bool, deadline time.Time) (*windowRoundFixture, *expirationLifecycleAuthority) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	settings := func(receiver bool) *ClientSettings {
		value := DefaultClientSettingsWithBufferSize(64)
		value.Log = NewNoopLogger()
		value.ControlPingTimeout = 0
		value.EncryptionSettings.Mode = EncryptionModeOff
		if receiver && required {
			value.EncryptionSettings.Mode = EncryptionModeRequired
		}
		value.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		value.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Unix(1, 0)
		value.ContractManagerSettings.NetworkEventTimeChangeHmac = time.Unix(1, 0)
		value.ContractManagerSettings.LegacyCreateContract = false
		value.SendBufferSettings.WindowSizing = WindowSizingConstant
		value.SendBufferSettings.ApplyWindowSizing()
		value.SendBufferSettings.SequenceBufferSize = 0
		value.SendBufferSettings.ProtocolVersion = 2
		value.SendBufferSettings.ResendQueueMaxByteCount = 64 * 1024
		value.SendBufferSettings.CompactContractHead = false
		value.ReceiveBufferSettings.WindowSizing = WindowSizingConstant
		value.ReceiveBufferSettings.ApplyWindowSizing()
		value.ReceiveBufferSettings.AckCompressTimeout = 0
		value.ReceiveBufferSettings.ReceiveQueueBudget = nil
		if receiver {
			value.ReceiveBufferSettings.ReceiveQueueBudget = rx
			value.ReceiveBufferSettings.ReceiveQueueRetainedByteAccounting = true
			value.ReceiveBufferSettings.ReceiveQueueMinByteCount = 0
			value.ReceiveBufferSettings.ReceiveQueueMaxByteCount = 2*1283 + 1
			value.ReceiveBufferSettings.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
			value.ReceiveBufferSettings.AdvertiseReceiveWindow = false
			value.ReceiveBufferSettings.PackQueueBudget = nil
			value.ReceiveBufferSettings.PackQueueRetainedByteAccounting = false
		}
		return value
	}
	fixture := &windowRoundFixture{t: t, ctx: ctx, cancel: cancel,
		senderOut: make(Route, 128), senderIn: make(Route, 128),
		receiverOut: make(Route, 128), receiverIn: make(Route, 128)}
	senderId, receiverId := NewId(), NewId()
	forward := &expirationLifecycleAuthority{ctx: ctx, sourceId: senderId,
		peer: func() *Client { return fixture.receiver }, initialDeadline: deadline,
		grantDeadlines: map[Id]time.Time{}, renewalEntered: make(chan struct{})}
	reverse := &expirationLifecycleAuthority{ctx: ctx, sourceId: receiverId,
		peer: func() *Client { return fixture.sender }, initialDeadline: deadline,
		grantDeadlines: map[Id]time.Time{}, renewalEntered: make(chan struct{})}
	fixture.sender = NewClient(ctx, senderId, forward, settings(false))
	fixture.receiver = NewClient(ctx, receiverId, reverse, settings(true))
	fixture.sender.RouteManager().UpdateTransport(newDataGatewayTransport(), []Route{fixture.senderOut})
	fixture.sender.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{fixture.senderIn})
	fixture.receiver.RouteManager().UpdateTransport(newDataGatewayTransport(), []Route{fixture.receiverOut})
	fixture.receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{fixture.receiverIn})
	for _, client := range []*Client{fixture.sender, fixture.receiver} {
		client.ContractManager().SetProvideModes(map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
	}
	fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			if frame.MessageType == protocol.MessageType_TestSimpleMessage {
				fixture.deliveredCount++
			}
		}
	})
	t.Cleanup(func() {
		cancel()
		owner, release := context.WithTimeout(context.Background(), 5*time.Second)
		defer release()
		for _, client := range []*Client{fixture.sender, fixture.receiver} {
			if err := client.CloseAndWait(owner); err != nil {
				t.Errorf("close pressure contract client: %v", err)
			}
		}
		for _, frame := range fixture.heldFrames {
			if frame.bytes != nil {
				MessagePoolReturn(frame.bytes)
				frame.bytes = nil
			}
		}
		for _, route := range []Route{fixture.senderOut, fixture.senderIn, fixture.receiverOut, fixture.receiverIn} {
			for len(route) != 0 {
				MessagePoolReturn(<-route)
			}
		}
	})
	return fixture, forward
}

// Empty opening controls complete through the actual receive/ACK path. The
// application sequence number and full proof come from the sender's own wire.
func pressureContractData(t *testing.T, fixture *windowRoundFixture) *windowRoundFrame {
	t.Helper()
	if accepted, err := fixture.send(1280); !accepted || err != nil {
		t.Fatalf("signed pressure send accepted=%t error=%v", accepted, err)
	}
	for range cap(fixture.senderOut) {
		frame := fixture.take(fixture.senderOut)
		if frame.pack == nil {
			t.Fatal("signed pressure sender published no Pack")
		}
		for _, payload := range frame.pack.Frames {
			if payload.MessageType == protocol.MessageType_TestSimpleMessage {
				return frame
			}
		}
		fixture.forward(frame, fixture.receiverIn)
		fixture.acknowledge()
	}
	t.Fatal("signed pressure data hidden behind a full control route")
	return nil
}

// This is a bounded blocking wire receive, not a forced resend or timer
// override. Unreleased earlier/later writes cannot accidentally close the gap.
func pressureContractNaturalRetry(t *testing.T, fixture *windowRoundFixture, original *protocol.Pack) *windowRoundFrame {
	t.Helper()
	limit, cancel := context.WithTimeout(fixture.ctx, fixture.sender.settings.SendBufferSettings.AckTimeout)
	defer cancel()
	for range cap(fixture.senderOut) {
		select {
		case wire := <-fixture.senderOut:
			frame := &windowRoundFrame{bytes: wire, pack: decodeCompactContractTestPack(t, wire)}
			fixture.heldFrames = append(fixture.heldFrames, frame)
			if bytes.Equal(frame.pack.MessageId, original.MessageId) &&
				bytes.Equal(frame.pack.SequenceId, original.SequenceId) &&
				frame.pack.SequenceNumber == original.SequenceNumber {
				return frame
			}
		case <-limit.Done():
			t.Fatal("RX_PRESSURE_FULL_PROOF_RETRY_NOT_PUBLISHED")
		}
	}
	t.Fatal("RX_PRESSURE_FULL_PROOF_RETRY_HIDDEN")
	return nil
}

// Every cell starts with actual verified authority and two unacknowledged
// tails. Mutants change one incoming wire fact; the source's retained original
// and the receiver's verified proof remain untouched.
func testPressureContractAuthority(t *testing.T, mode string) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const ownerBytes = ByteCount(5120)
		required := mode == "required-plaintext"
		rootBytes := 2 * ownerBytes
		if required {
			// The required-mode constructor owns its separate standing decode
			// scratch; this does not enlarge the two-owner RX leaf.
			minimum := DefaultClientSettingsWithBufferSize(64).MinimumMessageLenLimit()
			rootBytes += 2*receiveMemoryScanCopyRoot(int(minimum)) + 2048
		}
		root := NewTransferMemoryBudget(rootBytes)
		rx := NewTransferMemoryBudgetWithParent(2*ownerBytes, root)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{rx, root} {
				state := budget.Stats()
				if state.UsedByteCount != 0 || state.QuotaByteCount != 0 || state.ReservedByteCount != state.ReleasedByteCount {
					t.Errorf("pressure contract cleanup imbalance: %+v", state)
				}
			}
		})
		deadline := time.Now().Add(time.Hour)
		if mode == "expired" {
			deadline = time.Now().Add(time.Second)
		}
		fixture, authority := newPressureContractFixture(t, rx, required, deadline)
		warm := pressureContractData(t, fixture)
		fixture.forward(warm, fixture.receiverIn)
		fixture.acknowledge()
		sequence := fixture.receiveSequence()
		verified := sequence.receiveContract
		if verified == nil || !verified.verifiedReceiveProofSet || verified.expired() ||
			fixture.ackedCount != 1 || rx.UsedByteCount() != 0 ||
			fixture.sender.ContractManager().ReceiveNoContract(fixture.receiver.ClientId()) ||
			fixture.receiver.ContractManager().ReceiveNoContract(fixture.sender.ClientId()) {
			t.Fatal("RX_PRESSURE_SIGNED_AUTHORITY_PREMISE_FAILED")
		}
		if required && (sequence.session == nil || !sequence.session.RequireEncryption()) {
			t.Fatal("RX_PRESSURE_REQUIRED_SESSION_PREMISE_FAILED")
		}
		wantDelivered := 1
		if required {
			wantDelivered = 0
		}
		if fixture.deliveredCount != wantDelivered {
			t.Fatal("RX_PRESSURE_PLAINTEXT_DELIVERY_POLICY_CHANGED")
		}
		standing := root.UsedByteCount()
		if standing != fixture.receiver.receiveDecodeReservedByteCount.Load() {
			t.Fatal("RX_PRESSURE_UNCLASSIFIED_STANDING_ROOT")
		}

		originals := make([]*windowRoundFrame, 11)
		for index := range originals {
			originals[index] = pressureContractData(t, fixture)
		}
		head, firstTail, lastTail := originals[0], originals[9], originals[10]
		headId := RequireIdFromBytes(head.pack.MessageId)
		if !head.pack.Head || head.pack.Nack || head.pack.ContractFrame == nil ||
			head.pack.ContractFrame.MessageType != protocol.MessageType_TransferContract ||
			sha256.Sum256(head.pack.ContractFrame.MessageBytes) != verified.verifiedReceiveProof ||
			head.pack.SequenceNumber != sequence.nextSequenceNumber ||
			firstTail.pack.Head || lastTail.pack.Head ||
			firstTail.pack.ContractFrame != nil || lastTail.pack.ContractFrame != nil ||
			!bytes.Equal(head.pack.SequenceId, warm.pack.SequenceId) ||
			!bytes.Equal(firstTail.pack.SequenceId, head.pack.SequenceId) ||
			!bytes.Equal(lastTail.pack.SequenceId, head.pack.SequenceId) {
			t.Fatal("RX_PRESSURE_SENDER_FULL_HEAD_PREMISE_FAILED")
		}
		fixture.forward(firstTail, fixture.receiverIn)
		fixture.forward(lastTail, fixture.receiverIn)
		for _, frame := range []*windowRoundFrame{firstTail, lastTail} {
			item := sequence.receiveQueue.GetByMessageId(RequireIdFromBytes(frame.pack.MessageId))
			if item == nil || item.committed || !item.ack || item.memoryBudget != rx || item.QueueByteCount() != ownerBytes {
				t.Fatal("RX_PRESSURE_SIGNED_TAIL_PREMISE_FAILED")
			}
		}
		if rx.UsedByteCount() != 2*ownerBytes || root.UsedByteCount() != standing+2*ownerBytes ||
			sequence.holdPromise.set || sequence.holdPromise.reservedByteCount != 0 ||
			len(fixture.receiverOut) != 0 || fixture.ackedCount != 1 || fixture.deliveredCount != wantDelivered {
			t.Fatal("RX_PRESSURE_SIGNED_FULL_HOLD_PREMISE_FAILED")
		}

		if mode == "expired" {
			authority.blockRenewals.Store(true)
			time.Sleep(time.Until(deadline) + time.Millisecond)
			synctest.Wait()
			if !verified.expired() || rx.UsedByteCount() != 2*ownerBytes {
				t.Fatal("RX_PRESSURE_REAL_EXPIRY_PREMISE_FAILED")
			}
		}
		if mode == "changed-proof" {
			pack := proto.Clone(head.pack).(*protocol.Pack)
			var proof protocol.Contract
			if err := ProtoUnmarshal(pack.ContractFrame.MessageBytes, &proof); err != nil {
				t.Fatal(err)
			}
			proof.StoredContractHmac = append([]byte(nil), proof.StoredContractHmac...)
			if len(proof.StoredContractHmac) == 0 {
				t.Fatal("signed pressure proof lacks MAC")
			}
			proof.StoredContractHmac[0] ^= 1
			changed, err := proto.Marshal(&proof)
			if err != nil {
				t.Fatal(err)
			}
			pack.ContractFrame.MessageBytes = changed
			rewriteHeldHeadContractWire(t, head, pack)
		} else if mode == "wrong-generation" || mode == "wrong-role" || mode == "wrong-peer" {
			var transfer protocol.TransferFrame
			if err := ProtoUnmarshal(head.bytes, &transfer); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "wrong-generation":
				transfer.Pack.SequenceId = NewId().Bytes()
			case "wrong-role":
				role := sequence.encryptionRole.toProtobuf()
				transfer.SessionRole = &role
			case "wrong-peer":
				transfer.TransferPath.SourceId = NewId().Bytes()
			}
			wire, err := ProtoMarshal(&transfer)
			if err != nil {
				t.Fatal(err)
			}
			MessagePoolReturn(head.bytes)
			head.bytes, head.pack = wire, transfer.Pack
		}
		before := fixture.receiver.ReceiveStats()
		fixture.forward(head, fixture.receiverIn)
		synctest.Wait()
		root.admissionLock.Lock()
		pressure := sequence.memoryPressure
		root.admissionLock.Unlock()
		after := fixture.receiver.ReceiveStats()
		if mode != "repeat" {
			if pressure != (receiveMemoryPressure{}) || sequence.ctx.Err() != nil || fixture.ctx.Err() != nil || fixture.receiver.ctx.Err() != nil ||
				rx.UsedByteCount() != 2*ownerBytes || root.UsedByteCount() != standing+2*ownerBytes ||
				after.ReceiveQueueTentativeEvictionCount != before.ReceiveQueueTentativeEvictionCount ||
				after.ReceiveQueueEvictionCount != before.ReceiveQueueEvictionCount ||
				sequence.receiveQueue.GetByMessageId(RequireIdFromBytes(firstTail.pack.MessageId)) == nil ||
				sequence.receiveQueue.GetByMessageId(RequireIdFromBytes(lastTail.pack.MessageId)) == nil ||
				fixture.ackedCount != 1 || fixture.deliveredCount != wantDelivered || len(fixture.receiverOut) != 0 {
				t.Fatalf("RX_PRESSURE_UNVERIFIED_AUTHORITY_RECLAIMED: mode=%s", mode)
			}
			t.Logf("RX_PRESSURE_AUTHORITY_CELL mode=%s reclaimed=0 escrow=0 full_rx=%d", mode, rx.UsedByteCount())
			return
		}
		if !pressure.activeSet || !pressure.ready || pressure.pendingSet || pressure.owner != nil || pressure.reclaimOwner != nil ||
			pressure.active.messageId != headId || !pressure.active.contractFrameSet || !pressure.active.head ||
			pressure.reservedByteCount != pressure.active.queueByteCount ||
			rx.UsedByteCount() != pressure.reservedByteCount || root.UsedByteCount() != standing+pressure.reservedByteCount ||
			sequence.receiveQueue.Len() != 0 ||
			after.ReceiveQueueTentativeEvictionCount-before.ReceiveQueueTentativeEvictionCount != 2 ||
			after.ReceiveQueueEvictionCount != before.ReceiveQueueEvictionCount ||
			after.PackHandoffDropCount-before.PackHandoffDropCount != 1 ||
			fixture.ackedCount != 1 || fixture.deliveredCount != wantDelivered || len(fixture.receiverOut) != 0 {
			t.Fatal("RX_PRESSURE_VERIFIED_FULL_HEAD_ESCROW_MISSING")
		}
		retry := pressureContractNaturalRetry(t, fixture, head.pack)
		if !retry.pack.Head || retry.pack.ContractFrame == nil ||
			!bytes.Equal(retry.pack.ContractFrame.MessageBytes, head.pack.ContractFrame.MessageBytes) ||
			MessageByteCount(retry.pack.Frames) != MessageByteCount(head.pack.Frames) {
			t.Fatal("RX_PRESSURE_FULL_PROOF_RETRY_CHANGED_IDENTITY")
		}
		fixture.forward(retry, fixture.receiverIn)
		fixture.acknowledge()
		root.admissionLock.Lock()
		pressure = sequence.memoryPressure
		root.admissionLock.Unlock()
		if pressure != (receiveMemoryPressure{}) || sequence.ctx.Err() != nil || fixture.receiver.ctx.Err() != nil ||
			fixture.receiver.ReceiveStats().ReceiveQueueEvictionCount != 0 ||
			rx.UsedByteCount() != 0 || root.UsedByteCount() != standing ||
			fixture.deliveredCount != 2 || fixture.ackedCount != 2 || sequence.receiveQueue.Len() != 0 {
			t.Fatal("RX_PRESSURE_FULL_PROOF_NATURAL_RETRY_DID_NOT_DRAIN")
		}
		t.Logf("RX_PRESSURE_AUTHORITY_CELL mode=repeat full_proof=true first_offer_refused=true natural_same_identity_retry=true escrow=0 rx=0")
	})
}

// The unchanged signed full head must earn escrow, then consume it only on
// its sender-owned natural retry while keeping both tentative evictions safe.
func TestReceiveMemoryPressureRepeatedFullHeadUsesVerifiedProof(t *testing.T) {
	testPressureContractAuthority(t, "repeat")
}

// A same-id proof with a changed MAC must not borrow prior verification.
func TestReceiveMemoryPressureChangedFullProofCannotReclaim(t *testing.T) {
	testPressureContractAuthority(t, "changed-proof")
}

// Real signed expiry removes pressure authority without removing held owners.
func TestReceiveMemoryPressureExpiredFullProofCannotReclaim(t *testing.T) {
	testPressureContractAuthority(t, "expired")
}

// Generation, role and peer mismatches cannot reach the resident's escrow.
func TestReceiveMemoryPressureForeignGenerationCannotReclaim(t *testing.T) {
	for _, mode := range []string{"wrong-generation", "wrong-role", "wrong-peer"} {
		testPressureContractAuthority(t, mode)
	}
}

// An authenticated contract does not authorize plaintext pressure under the
// receiver's required-encryption policy, even before its cipher is available.
func TestReceiveMemoryPressureRequiredPlaintextCannotReclaim(t *testing.T) {
	testPressureContractAuthority(t, "required-plaintext")
}
