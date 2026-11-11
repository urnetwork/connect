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

// The maintained signed fixture, with a receipt-sized byte limit installed
// before construction. Both ordinary and ToProvider application frames count.
func newProviderGrowthContractFixture(t *testing.T, rx *TransferMemoryBudget, required bool, deadline time.Time) (*windowRoundFixture, *expirationLifecycleAuthority) {
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
			value.ReceiveBufferSettings.ReceiveQueueMaxByteCount = kib(64)
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
			if frame.MessageType == protocol.MessageType_TestSimpleMessage || frame.MessageType == protocol.MessageType_IpIpPacketToProvider {
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

// Keep this exact first failed-growth transition reusable by the real provider
// controls. The complete decoded owner returns before the worker funds D.
func providerGrowthEscrow(t *testing.T, fixture *receiveProviderPressureFixture) {
	t.Helper()
	fixture.forward(fixture.originals[1], fixture.receiverIn)
	first := fixture.sequence.memoryPressure
	if !first.activeSet || !first.ready || first.reservedByteCount != fixture.decodeBytes ||
		fixture.root.Available() != fixture.deliveryBytes-fixture.decodeBytes {
		t.Fatal("provider growth lost the original decoded-only pressure premise")
	}
	fixture.reserveForeign(fixture.root.Available())
	before := fixture.receiver.ReceiveStats()
	fixture.retryHead()
	state := fixture.sequence.memoryPressure
	if !state.activeSet || !state.ready || state.pendingSet || state.owner != nil || state.reclaimOwner != nil ||
		state.borrowedByteCount != 0 || state.reservedByteCount != fixture.deliveryBytes || state.active.queueByteCount != fixture.deliveryBytes ||
		state.active.messageId != RequireIdFromBytes(fixture.originals[1].pack.MessageId) ||
		fixture.sequence.holdPromise.ingressHead != nil || fixture.sequence.receiveQueue.Len() != 0 ||
		fixture.rx.UsedByteCount() != fixture.deliveryBytes || fixture.ackedCount != 1 || len(fixture.receiverOut) != 0 ||
		fixture.receiver.ReceiveStats().ReceiveQueueDropCount != before.ReceiveQueueDropCount+1 ||
		fixture.receiver.ReceiveStats().ReceiveQueueTentativeEvictionCount != before.ReceiveQueueTentativeEvictionCount+1 ||
		fixture.receiver.ReceiveStats().ReceiveQueueEvictionCount != 0 || fixture.sequence.ctx.Err() != nil {
		t.Fatal("failed provider growth did not return its actual owner before funding exact downstream retry credit")
	}
	if fixture.root.UsedByteCount() != fixture.rx.UsedByteCount()+fixture.natBudget.UsedByteCount()+fixture.foreignBytes {
		t.Fatal("provider growth changed common-root conservation")
	}
}

// Even after a sibling consumes every newly free byte, the complete funded
// retry must reach the actual receipt/NAT/TCP path and retire once.
func TestReceiveMemoryProviderGrowthCreditExcludesCompetingFlow(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newReceiveProviderPressureFixture(t)
		providerGrowthEscrow(t, fixture)
		fixture.reserveForeign(fixture.root.Available())
		if fixture.foreignBudget.TryReserve(1) {
			fixture.foreignBudget.Release(1)
			t.Fatal("competing flow consumed exact provider retry credit")
		}
		fixture.retryHead()
		fixture.proveNatCustody()
		fixture.drain()
		fixture.close()
		t.Log("[rx-provider-growth-competing] full_root_retry=true exact_downstream_credit=true delivered=12 cumulative=12 all_joined=true")
	})
}

// Cancellation occurs after the real partial RX→NAT move and before final
// TCP admission. Both owners must return without securing the pending head.
func TestReceiveMemoryProviderGrowthCancellationReturnsBothOwners(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newReceiveProviderPressureFixture(t)
		providerGrowthEscrow(t, fixture)
		fixture.reserveForeign(fixture.root.Available())
		fixture.retryHead()
		fixture.proveNatCustody()
		target := RequireIdFromBytes(fixture.originals[1].pack.MessageId)
		fixture.close()
		if fixture.ackedCount != 1 || fixture.ackCounts[1] != 0 || fixture.dials.Load() != 0 ||
			fixture.sequence.memoryPressure != (receiveMemoryPressure{}) || fixture.sequence.holdPromise.ingressHead != nil {
			t.Fatalf("provider cancellation ACKed or retained pending original: acked=%d head_acks=%d dials=%d pressure=%+v ingress_present=%t", fixture.ackedCount, fixture.ackCounts[1], fixture.dials.Load(), fixture.sequence.memoryPressure, fixture.sequence.holdPromise.ingressHead != nil)
		}
		if present, head := reliableIngressCumulativeHead(fixture.sequence); present && head.messageId == target {
			t.Fatal("canceled final TCP owner entered the cumulative ACK window")
		}
		t.Log("[rx-provider-growth-cancel] partial_move=true pending_original_unacked=true rx=0 nat=0 root=0 all_joined=true")
	})
}

// The failed-growth gate receives the decoded Pack's actual wrap bit and
// encrypted-control membership. The signed-route test below supplies real MAC
// verification; this synthetic already-verified proof isolates classification.
func TestReceiveMemoryGrowthUsesActualPacketAuthority(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, cell := range []struct{ unwrapped, encryptedControl, wantProposal bool }{
		{unwrapped: true},
		{wantProposal: true},
		{encryptedControl: true},
	} {
		func() {
			const cost = ByteCount(8192)
			root, rx, buffer, sequence := receiveMemoryTestState(t, 2*cost)
			sequence.closeNotify = make(chan struct{}, 1)
			sequence.receiveBufferSettings.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
			sequence.session = &peerEncryptionSession{settings: &EncryptionSettings{Mode: EncryptionModeRequired}}
			proof := []byte("synthetic previously verified proof for wrap classification only")
			sequence.receiveContract = &sequenceContract{verifiedReceiveProofSet: true, verifiedReceiveProof: sha256.Sum256(proof)}
			id := NewId()
			if !rx.TryReserve(cost) {
				t.Fatal("wrap classification escrow setup")
			}
			sequence.memoryPressure = receiveMemoryPressure{active: receiveMemoryPressureRequest{messageId: id, sequenceNumber: 1, queueByteCount: cost}, activeSet: true, ready: true, reservedByteCount: cost}
			owner := buffer.takeReceiveMemoryOwner(sequence, id, 1, cost)
			if owner == nil {
				t.Fatal("wrap classification paid decoder could not consume exact credit")
			}
			defer func() {
				if owner != nil {
					owner.release()
				}
			}()
			if !root.TryReserve(cost) {
				t.Fatal("wrap classification competing claim setup")
			}
			foreign := true
			defer func() {
				if foreign {
					root.Release(cost)
				}
			}()
			owner.receivePack.Pack = &owner.pack
			owner.receivePack.Unwrapped = cell.unwrapped
			owner.pack.Frames = []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true,
				MessageBytes: MessagePoolCopy(ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 101, bytes.Repeat([]byte{1}, 1240)))}}
			owner.pack.ContractFrame = &protocol.Frame{MessageType: protocol.MessageType_TransferContract, MessageBytes: MessagePoolCopy(proof)}
			if cell.encryptedControl {
				controlBytes, err := proto.Marshal(&protocol.EncryptedControl{
					ControlType: protocol.EncryptedControlType_EncryptedControlHandshake, Payload: []byte{1},
				})
				if err != nil {
					t.Fatal(err)
				}
				owner.pack.Frames = append(owner.pack.Frames, &protocol.Frame{
					MessageType:  protocol.MessageType_TransferEncryptedControl,
					MessageBytes: MessagePoolCopy(controlBytes),
				})
			}
			item := &owner.receiveItem
			item.decodedOwner, item.ack, item.head, item.unwrapped = owner, true, true, cell.unwrapped
			item.frames, item.contractFrame = owner.pack.Frames, owner.pack.ContractFrame
			before := root.Stats()
			if sequence.prepareReceiveDeliveryCredit(item) {
				t.Fatal("full-root provider prepay unexpectedly succeeded")
			}
			proposed := sequence.memoryPressure.pendingSet
			if root.Stats() != before || item.QueueByteCount() != cost || item.deliveryPrepared || item.deliveryPrepaid != 0 ||
				sequence.memoryPressure.owner != owner || sequence.memoryPressure.borrowedByteCount != cost || sequence.memoryPressure.reservedByteCount != 0 {
				t.Fatal("failed provider growth changed or released its still-live physical claim")
			}
			if proposed && (sequence.memoryPressure.pending.queueByteCount <= cost || !sequence.memoryPressure.pending.decrypted) {
				t.Fatal("wrapped growth proposal lost its larger exact requirement or decrypted authority")
			}
			// The worker cannot reclaim while this actual paid owner is live.
			sequence.applyReceiveMemoryPressure()
			if root.Stats() != before || sequence.memoryPressure.owner != owner || sequence.memoryPressure.pendingSet != proposed {
				t.Fatal("growth planning preempted before actual decoded owner return")
			}
			owner.release()
			owner = nil
			sequence.releaseHoldPromise()
			root.Release(cost)
			foreign = false
			if root.UsedByteCount() != 0 || rx.UsedByteCount() != 0 || root.Stats().QuotaByteCount != 0 {
				t.Fatal("wrap classification owner cleanup failed")
			}
			t.Logf("[rx-provider-growth-wrap] plaintext=%t encrypted_control=%t proposed=%t owner_returned=true root=0", cell.unwrapped, cell.encryptedControl, proposed)
			if proposed != cell.wantProposal {
				t.Error("RX_PROVIDER_GROWTH_CURRENT_AUTHORITY: failed growth ignored actual wrap or encrypted-control authority")
			}
		}()
	}
}

// A same-ID retry with a changed signed MAC can consume its existing decoded
// credit, but cannot use that old identity as authority to evict another tail.
func TestReceiveMemoryProviderGrowthRevalidatesActualSignedProof(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		root := NewTransferMemoryBudget(mib(1))
		rx := NewTransferMemoryBudgetWithParent(kib(64), root)
		foreign := NewTransferMemoryBudgetWithParent(mib(1), root)
		foreignBytes := ByteCount(0)
		t.Cleanup(func() {
			if foreignBytes != 0 {
				foreign.Release(foreignBytes)
				foreignBytes = 0
			}
			clean := true
			for _, budget := range []*TransferMemoryBudget{root, rx, foreign} {
				stats := budget.Stats()
				if stats.UsedByteCount != 0 || stats.QuotaByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
					clean = false
					t.Errorf("signed growth cleanup imbalance: %+v", stats)
				}
			}
			if clean && !t.Failed() {
				t.Log("[rx-provider-growth-proof-cleanup] root=0 rx=0 foreign=0 quota=0 clients_joined=true")
			}
		})
		wire, _ := newProviderGrowthContractFixture(t, rx, false, time.Now().Add(time.Hour))
		warm := pressureContractData(t, wire)
		wire.forward(warm, wire.receiverIn)
		wire.acknowledge()
		sequence := wire.receiveSequence()
		verified := sequence.receiveContract
		if verified == nil || !verified.verifiedReceiveProofSet || verified.expired() || wire.ackedCount != 1 ||
			wire.receiver.ContractManager().ReceiveNoContract(wire.sender.ClientId()) || root.UsedByteCount() != 0 {
			t.Fatal("signed provider growth lacks actual verified authority")
		}
		fixture := &receivePressureTestFixture{windowRoundFixture: wire, root: root, rx: rx, sequence: sequence, originals: map[uint64]*windowRoundFrame{}}
		first := sequence.nextSequenceNumber
		path := reliableDeliveryTestPath(4)
		for index := uint64(0); index < 11; index++ {
			packet := MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagAck, uint32(101+index*1240), bytes.Repeat([]byte{byte(index + 1)}, 1240)))
			frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
			if err != nil || !frame.Raw {
				MessagePoolReturn(packet)
				t.Fatal("signed provider frame setup")
			}
			accepted, err := wire.sender.SendWithTimeoutDetailed(frame, wire.receiver.ClientId(), func(err error) {
				if err == nil {
					wire.ackedCount++
				}
			}, time.Second)
			if !accepted || err != nil {
				MessagePoolReturn(frame.MessageBytes)
				t.Fatal("signed provider source refused original")
			}
			fixture.originals[first+index] = wire.takePack(first + index)
		}
		head, tailA, tailB := fixture.originals[first], fixture.originals[first+9], fixture.originals[first+10]
		if !head.pack.Head || head.pack.ContractFrame == nil || sha256.Sum256(head.pack.ContractFrame.MessageBytes) != verified.verifiedReceiveProof ||
			tailA.pack.Head || tailB.pack.Head || tailA.pack.ContractFrame != nil || tailB.pack.ContractFrame != nil {
			t.Fatal("sender did not preserve the real full-head/tail proof shape")
		}
		transferPath := TransferPath{SourceId: wire.sender.ClientId(), DestinationId: wire.receiver.ClientId()}
		headScan, headStatus := scanReceiveMemoryPack(head.bytes, transferPath)
		tailScan, tailStatus := scanReceiveMemoryPack(tailA.bytes, transferPath)
		if headStatus != receiveMemoryPackScanReady || tailStatus != receiveMemoryPackScanReady {
			t.Fatal("signed provider scanner premise")
		}
		charge := func(frame *windowRoundFrame, cost ByteCount) ByteCount {
			probe := &receiveItem{transferItem: transferItem{queueByteCount: cost}, ack: true, frames: frame.pack.Frames}
			if !sequence.prepareReceiveDeliveryCredit(probe) {
				t.Fatal("signed provider prepay geometry")
			}
			return probe.QueueByteCount()
		}
		headCharge, tailCharge := charge(head, headScan.retainedByteCount), charge(tailA, tailScan.retainedByteCount)
		if headScan.retainedByteCount >= tailCharge || headCharge <= headScan.retainedByteCount || headCharge >= 2*tailCharge {
			t.Fatal("signed provider cannot isolate one reclaimed tail and later downstream deficit")
		}
		foreignBytes = root.Available() - 2*tailCharge
		if foreignBytes <= 0 || !foreign.TryReserve(foreignBytes) {
			t.Fatal("signed provider competing root setup")
		}
		wire.forward(tailA, wire.receiverIn)
		wire.forward(tailB, wire.receiverIn)
		for _, number := range []uint64{first + 9, first + 10} {
			item := sequence.receiveQueue.GetBySequenceNumber(number)
			if item == nil || item.committed || !item.ack || item.QueueByteCount() != tailCharge || item.deliveryPrepaid == 0 {
				t.Fatal("signed provider tails did not own actual uncommitted downstream prepayment")
			}
		}
		if root.Available() != 0 || sequence.holdPromise.set || len(wire.receiverOut) != 0 {
			t.Fatal("signed provider root is not full without a SACK promise")
		}
		wire.forward(head, wire.receiverIn)
		surplus := root.Available()
		if sequence.memoryPressure.reservedByteCount != headScan.retainedByteCount || surplus != tailCharge-headScan.retainedByteCount || sequence.receiveQueue.Len() != 1 {
			t.Fatal("signed provider first refusal did not retain exact decoded-head credit")
		}
		if !foreign.TryReserve(surplus) {
			t.Fatal("signed provider competitor did not take released surplus")
		}
		foreignBytes += surplus
		before, pressureBefore, rootBefore := wire.receiver.ReceiveStats(), sequence.memoryPressure, root.Stats()
		mutant := fixture.naturalRetry(head)
		pack := proto.Clone(mutant.pack).(*protocol.Pack)
		var contract protocol.Contract
		if err := ProtoUnmarshal(pack.ContractFrame.MessageBytes, &contract); err != nil {
			t.Fatal(err)
		}
		if len(contract.StoredContractHmac) == 0 {
			t.Fatal("real signed provider proof has no MAC")
		}
		contract.StoredContractHmac[0] ^= 1
		changed, err := proto.Marshal(&contract)
		if err != nil {
			t.Fatal(err)
		}
		pack.ContractFrame.MessageBytes = changed
		rewriteHeldHeadContractWire(t, mutant, pack)
		mutantScan, status := scanReceiveMemoryPack(mutant.bytes, transferPath)
		if status != receiveMemoryPackScanReady || mutantScan.retainedByteCount != headScan.retainedByteCount ||
			!bytes.Equal(mutant.pack.MessageId, head.pack.MessageId) || !bytes.Equal(mutant.pack.Frames[0].MessageBytes, head.pack.Frames[0].MessageBytes) {
			t.Fatal("changed proof did not isolate one same-ID same-cost retry fact")
		}
		wire.forward(mutant, wire.receiverIn)
		if sequence.memoryPressure != pressureBefore || root.Stats() != rootBefore || sequence.receiveQueue.GetBySequenceNumber(first+9) == nil ||
			wire.receiver.ReceiveStats().ReceiveQueueTentativeEvictionCount != before.ReceiveQueueTentativeEvictionCount ||
			wire.receiver.ReceiveStats().ReceiveQueueDropCount != before.ReceiveQueueDropCount+1 ||
			wire.receiver.ReceiveStats().PackHandoffDropCount != before.PackHandoffDropCount || len(wire.receiverOut) != 0 ||
			wire.ackedCount != 1 || sequence.nextSequenceNumber != first || sequence.ctx.Err() != nil || verified.verifiedReceiveProof != pressureBefore.active.contractProof {
			t.Fatal("changed current proof spent old retry authority to reclaim downstream credit")
		}
		wire.forward(fixture.naturalRetry(head), wire.receiverIn)
		larger := sequence.memoryPressure
		if !larger.activeSet || !larger.ready || larger.owner != nil || larger.pendingSet || larger.borrowedByteCount != 0 ||
			larger.reservedByteCount != headCharge || larger.active.queueByteCount != headCharge || larger.active.contractProof != verified.verifiedReceiveProof ||
			sequence.receiveQueue.Len() != 0 || sequence.holdPromise.ingressHead != nil || rx.UsedByteCount() != headCharge || wire.ackedCount != 1 {
			t.Fatal("unchanged verified retry did not earn larger credit after returning its actual decoded owner")
		}
		free := root.Available()
		if !foreign.TryReserve(free) {
			t.Fatal("signed provider second competing reservation")
		}
		foreignBytes += free
		wire.forward(fixture.naturalRetry(head), wire.receiverIn)
		wire.acknowledge()
		if wire.ackedCount != 2 || wire.deliveredCount != 2 || rx.UsedByteCount() != 0 || sequence.memoryPressure != (receiveMemoryPressure{}) ||
			root.UsedByteCount() != foreignBytes || sequence.ctx.Err() != nil || wire.receiver.ReceiveStats().ReceiveQueueEvictionCount != 0 {
			t.Fatal("verified same-size full-root retry did not consume its prepaid downstream credit once")
		}
		t.Log("[rx-provider-growth-proof] changed_same_id_rejected=true actual_verified_retry=true second_sibling_full=true rx=0 committed_evictions=0")
	})
}
