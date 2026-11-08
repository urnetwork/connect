// Explicit waiting producers own their frames/compact ACK values. A zero-time
// caller must not inherit that producer's mutex wait or disturb its custody.
// These are fixed-candidate-only guards. A reverted blocking mutex is not a
// qualified synctest old-negative; the independent H1 wire witness owns that.
package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

type sharedPumpPackResult struct {
	accepted bool
	err      error
}

// The real owned decoder prepays this caller's copied wire and payload roots.
// This helper takes its carrier copy and returns the ordinary Pack API input.
func sharedPumpOwnedPack(t *testing.T, f *windowRoundFixture, wire *windowRoundFrame) *ReceivePack {
	t.Helper()
	carrier := MessagePoolCopy(wire.bytes)
	path, err := FilteredTransferPath(carrier)
	if err != nil {
		MessagePoolReturn(carrier)
		t.Fatal(err)
	}
	decoded := inboundDecodedTransferFrames.take()
	ok, pressure := f.receiver.unmarshalReceiveMemoryFrame(carrier, decoded, true, path, false, sequenceTlsRoleServer, false)
	if !ok || pressure || decoded.packOwner == nil || decoded.frame.Pack == nil {
		inboundDecodedTransferFrames.put(decoded)
		MessagePoolReturn(carrier)
		t.Fatal("dedicated API input did not receive actual decoded RX ownership")
	}
	owner := decoded.detachPackOwner()
	inboundDecodedTransferFrames.put(decoded)
	pack := &owner.receivePack
	root := owner.receiveItem.memoryBudget.admissionRoot()
	root.admissionLock.Lock()
	pack.Source, pack.SequenceId, pack.Pack = path.SourceMask(), RequireIdFromBytes(owner.pack.SequenceId), &owner.pack
	pack.ReceiveCallback, pack.MessageByteCount = f.receiver.receiveCallback, MessageByteCount(owner.pack.Frames)
	pack.TransferFrameBytes, pack.TransportType, pack.Unwrapped = carrier, TransportTypeH1, true
	pack.EncryptionRole, pack.EncryptionCompanion = sequenceTlsRoleServer, false
	pack.receivePublishedReliable = !owner.pack.Nack
	pack.receiveConsumerData = receiveHeadRecoveryPack(&owner.pack)
	root.admissionLock.Unlock()
	return pack
}

func TestSharedPumpZeroPackRefusesBehindDedicatedWaitOwner(t *testing.T) {
	sharedPumpPackContention(t, true)
}
func TestDedicatedReceivePackWaitPreservesRealAdmission(t *testing.T) {
	sharedPumpPackContention(t, false)
}

// One callback owns decoded data; a second actual original fills its one-slot
// queue. A separately owned API producer then waits on a third real original.
func sharedPumpPackContention(t *testing.T, probeZero bool) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		t.Cleanup(func() {
			if !t.Failed() {
				t.Log("SHARED_PUMP_PACK_API_CLEANUP_JOINED")
			}
		})
		assertMessagePoolOwnership(t)
		root := NewTransferMemoryBudget(mib(1))
		rx := NewTransferMemoryBudgetWithParent(kib(128), root)
		packBudget := NewTransferMemoryBudgetWithParent(kib(64), root)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{packBudget, rx, root} {
				assertRetainedBudgetBalance(t, budget)
				if quota := budget.Stats().QuotaByteCount; quota != 0 {
					t.Errorf("Pack API cleanup retained quota: %d", quota)
				}
			}
		})
		f := newWindowRoundFixture(t, func(send *SendBufferSettings) { send.ProtocolVersion = 2 }, func(receive *ReceiveBufferSettings) {
			receive.ProtocolVersion = 2
			receive.SequenceBufferSize, receive.H1SequenceBufferSize = 1, 1
			receive.H1SequenceBufferAdaptiveMaxSize = 0
			receive.ReceiveQueueBudget, receive.PackQueueBudget = rx, packBudget
			receive.ReceiveQueueRetainedByteAccounting, receive.PackQueueRetainedByteAccounting = true, true
		})
		entered, release := make(chan struct{}), make(chan struct{})
		var enterOnce, releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		var calls sync.WaitGroup
		t.Cleanup(func() { unblock(); f.cancel(); calls.Wait() })
		f.receiver.AddReceiveCallback(func(source TransferPath, _ []*protocol.Frame, _ Peer) {
			if source.SourceId == f.sender.ClientId() {
				enterOnce.Do(func() { close(entered); <-release })
			}
		})
		first, second, third := f.write(800), f.write(800), f.write(800)
		f.forward(first, f.receiverIn)
		<-entered
		secondPath, pathErr := FilteredTransferPath(second.bytes)
		if pathErr != nil {
			t.Fatal(pathErr)
		}
		secondScan, status := scanReceiveMemoryPack(second.bytes, secondPath)
		if status != receiveMemoryPackScanReady || secondScan.retainedByteCount <= 0 {
			t.Fatal("actual queued original lacks its exact scanner-paid physical cost")
		}
		queuedQuota, queuedEncoded := secondScan.retainedByteCount, ByteCount(len(second.bytes))
		f.forward(second, f.receiverIn)
		sequence := f.receiveSequence()
		// RX owns both decoded originals. Pack is a sibling permission leaf,
		// so only its target-exclusive quota is charged for the queued one.
		// These samples take the same root lock; root/RX are never double-paid.
		census := func(stage string) {
			stats := root.StatsWithDescendants(rx, packBudget)
			t.Logf("SHARED_PUMP_PACK_API_CENSUS stage=%s count=%d channel=%d encoded=%d wanted_encoded=%d base_limit=%d h1_limit=%d wait=%d acked=%d root_used=%d root_quota=%d rx_used=%d rx_quota=%d pack_used=%d pack_quota=%d wanted_quota=%d pack_reserved=%d pack_released=%d exact_pack_budget=%t exact_rx_budget=%t", stage,
				sequence.packQueueCount.Load(), len(sequence.packs), sequence.packQueueByteCount.Load(), queuedEncoded,
				sequence.packQueueBaseLimit, sequence.packQueueH1Limit, f.receiver.ReceiveStats().PackHandoffWaitCount, f.ackedCount,
				stats[0].UsedByteCount, stats[0].QuotaByteCount, stats[1].UsedByteCount, stats[1].QuotaByteCount,
				stats[2].UsedByteCount, stats[2].QuotaByteCount, queuedQuota, stats[2].ReservedByteCount, stats[2].ReleasedByteCount,
				sequence.receiveBufferSettings.PackQueueBudget == packBudget, sequence.receiveQueue.budget == rx)
		}
		initial := root.StatsWithDescendants(rx, packBudget)
		if sequence.packQueueCount.Load() != 1 || len(sequence.packs) != 1 || sequence.packQueueByteCount.Load() != queuedEncoded ||
			sequence.receiveBufferSettings.PackQueueBudget != packBudget || sequence.receiveQueue.budget != rx ||
			initial[0].UsedByteCount != initial[1].UsedByteCount || initial[0].QuotaByteCount != 0 || initial[1].QuotaByteCount != 0 ||
			initial[2].UsedByteCount != 0 || initial[2].QuotaByteCount != queuedQuota || initial[1].UsedByteCount <= queuedQuota || f.ackedCount != 0 {
			census("initial")
			t.Fatal("actual callback and queued original did not establish bounded dedicated pressure")
		}
		start := func(input *ReceivePack, timeout time.Duration) <-chan sharedPumpPackResult {
			done := make(chan sharedPumpPackResult, 1)
			calls.Add(1)
			go func() {
				defer calls.Done()
				accepted, err := sequence.Pack(input, timeout)
				if !accepted {
					input.messagePoolReturn()
				}
				done <- sharedPumpPackResult{accepted: accepted, err: err}
			}()
			return done
		}
		dedicated := start(sharedPumpOwnedPack(t, f, third), -1)
		synctest.Wait()
		waiting := root.StatsWithDescendants(rx, packBudget)
		if f.receiver.ReceiveStats().PackHandoffWaitCount != 1 || sequence.packQueueCount.Load() != 1 || f.ackedCount != 0 ||
			waiting[0].UsedByteCount != waiting[1].UsedByteCount || waiting[2] != initial[2] {
			census("dedicated_wait")
			t.Fatal("explicit waiting Pack API did not reach its actual full queue")
		}
		select {
		case <-dedicated:
			t.Fatal("dedicated wait returned before capacity")
		default:
		}
		var fourth *windowRoundFrame
		var zero <-chan sharedPumpPackResult
		var zeroResult sharedPumpPackResult
		zeroReturned := false
		if probeZero {
			fourth = f.write(800)
			before := root.StatsWithDescendants(rx, packBudget)
			zero = start(sharedPumpOwnedPack(t, f, fourth), 0)
			synctest.Wait()
			select {
			case zeroResult = <-zero:
				zeroReturned = true
				after := root.StatsWithDescendants(rx, packBudget)
				if zeroResult.accepted || zeroResult.err != nil || after[0].UsedByteCount != before[0].UsedByteCount ||
					after[1].UsedByteCount != before[1].UsedByteCount || after[0].UsedByteCount != after[1].UsedByteCount ||
					after[2] != before[2] || f.ackedCount != 0 || sequence.packQueueCount.Load() != 1 || len(sequence.packs) != 1 || sequence.packQueueByteCount.Load() != queuedEncoded {
					census("zero_refused")
					t.Fatal("zero Pack caller spent dedicated ownership, queued data or changed accounting")
				}
				closed := make(chan struct{})
				close(closed)
				canceledInput := sharedPumpOwnedPack(t, f, fourth)
				canceledInput.carrierDone = closed
				canceled := start(canceledInput, 0)
				synctest.Wait()
				select {
				case result := <-canceled:
					afterClosed := root.StatsWithDescendants(rx, packBudget)
					if result.accepted || result.err == nil || f.ctx.Err() != nil || sequence.ctx.Err() != nil ||
						afterClosed[0].UsedByteCount != before[0].UsedByteCount || afterClosed[1].UsedByteCount != before[1].UsedByteCount ||
						afterClosed[0].UsedByteCount != afterClosed[1].UsedByteCount || afterClosed[2] != before[2] ||
						f.ackedCount != 0 || sequence.packQueueCount.Load() != 1 || len(sequence.packs) != 1 || sequence.packQueueByteCount.Load() != queuedEncoded {
						census("closed_refused")
						t.Fatal("closed carrier took ownership, canceled the dedicated producer or spent credit")
					}
				default:
					t.Fatal("closed carrier waited behind the dedicated Pack mutex")
				}
			default:
				t.Fatal("zero Pack did not return before the dedicated owner released")
			}
		}
		unblock()
		result := <-dedicated
		if !result.accepted || result.err != nil {
			t.Fatal("dedicated Pack lost its normal wait/grant semantics", result.err)
		}
		if probeZero && !zeroReturned {
			zeroResult = <-zero
		}
		synctest.Wait()
		if probeZero && zeroResult.err != nil {
			t.Fatal("zero Pack acquired an unrelated lifecycle error", zeroResult.err)
		}
		want := 3
		if probeZero && zeroResult.accepted {
			want++
		}
		drained := root.StatsWithDescendants(rx, packBudget)
		if f.deliveredCount != want || sequence.packQueueCount.Load() != 0 || drained[0].UsedByteCount != 0 ||
			drained[1].UsedByteCount != 0 || drained[2].UsedByteCount != 0 || drained[2].QuotaByteCount != 0 {
			census("accepted_drained")
			t.Fatal("accepted API originals did not drain exact decoded/Pack ownership")
		}
		f.acknowledge()
		if f.ackedCount != want {
			t.Fatal("accepted prefix did not retire through actual peer ACKs")
		}
		if probeZero && !zeroResult.accepted {
			if f.sequence().resendQueue.Len() != 1 {
				t.Fatal("refused fourth original lost sender retry custody")
			}
			retry := sharedPumpOwnedPack(t, f, fourth)
			accepted, err := sequence.Pack(retry, 0)
			if !accepted {
				retry.messagePoolReturn()
			}
			if !accepted || err != nil {
				t.Fatal("exact refused Pack could not retry after capacity", err)
			}
			synctest.Wait()
			f.acknowledge()
			want++
		}
		finished := root.StatsWithDescendants(rx, packBudget)
		if f.deliveredCount != want || f.ackedCount != want || f.sequence().resendQueue.Len() != 0 ||
			finished[0].UsedByteCount != 0 || finished[1].UsedByteCount != 0 || finished[2].UsedByteCount != 0 || finished[2].QuotaByteCount != 0 {
			census("retry_finished")
			t.Fatal("actual Pack retry/ACKs did not finish exact ownership")
		}
		t.Logf("SHARED_PUMP_PACK_API_DRAINED zero_probe=%t zero_returned_before_release=%t delivered=%d acked=%d", probeZero, zeroReturned, f.deliveredCount, f.ackedCount)
	})
}

func TestSharedPumpZeroAckRefusesBehindDedicatedWaitOwner(t *testing.T) {
	sharedPumpAckContention(t, true)
}
func TestDedicatedAckWaitPreservesCompactValueAdmission(t *testing.T) {
	sharedPumpAckContention(t, false)
}

// Legacy channel-only Ack is an explicit supported API boundary. Its values
// are copied from a real peer ACK, but queue admission alone cannot retire the
// sender original; only forwarding the saved wire through Client may do that.
func sharedPumpAckContention(t *testing.T, probeZero bool) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		t.Cleanup(func() {
			if !t.Failed() {
				t.Log("SHARED_PUMP_ACK_API_CLEANUP_JOINED")
			}
		})
		assertMessagePoolOwnership(t)
		f := newWindowRoundFixture(t, nil, nil)
		wire := f.write(800)
		f.forward(wire, f.receiverIn)
		ack := f.take(f.receiverOut)
		if ack.ack == nil || ack.ack.Selective || f.ackedCount != 0 || f.sequence().resendQueue.Len() != 1 {
			t.Fatal("actual peer ACK did not retain its sole sender-owned original")
		}
		ctx, cancel := context.WithCancel(f.ctx)
		sequence := &SendSequence{ctx: ctx, cancel: cancel, client: f.sender, sequenceId: RequireIdFromBytes(ack.ack.SequenceId), destination: f.receiver.ClientId(), acks: make(chan receiveAckMessage, 1)}
		var calls sync.WaitGroup
		t.Cleanup(func() { cancel(); calls.Wait() })
		accepted, err := sequence.Ack(ack.ack, 0)
		if !accepted || err != nil {
			t.Fatal("ordinary compact ACK did not fill its real API queue", err)
		}
		start := func(timeout time.Duration) <-chan sharedPumpPackResult {
			done := make(chan sharedPumpPackResult, 1)
			calls.Add(1)
			go func() {
				defer calls.Done()
				accepted, err := sequence.Ack(ack.ack, timeout)
				done <- sharedPumpPackResult{accepted: accepted, err: err}
			}()
			return done
		}
		dedicated := start(-1)
		synctest.Wait()
		select {
		case <-dedicated:
			t.Fatal("explicit ACK waiter returned before its real queue drained")
		default:
		}
		if len(sequence.acks) != 1 || f.ackedCount != 0 || f.sequence().resendQueue.Len() != 1 {
			t.Fatal("compact ACK admission falsely retired its original")
		}
		var zero <-chan sharedPumpPackResult
		var zeroResult sharedPumpPackResult
		zeroReturned := false
		if probeZero {
			zero = start(0)
			synctest.Wait()
			select {
			case zeroResult = <-zero:
				zeroReturned = true
				if zeroResult.accepted || zeroResult.err != nil || len(sequence.acks) != 1 || f.ackedCount != 0 {
					t.Fatal("zero ACK caller spent compact custody or advanced real sender")
				}
			default:
				t.Fatal("zero ACK did not return before the dedicated owner released")
			}
		}
		first := <-sequence.acks
		if first.messageId != RequireIdFromBytes(ack.ack.MessageId) {
			t.Fatal("first accepted compact ACK changed identity")
		}
		result := <-dedicated
		if !result.accepted || result.err != nil {
			t.Fatal("dedicated ACK wait lost its capacity grant", result.err)
		}
		if probeZero && !zeroReturned {
			zeroResult = <-zero
		}
		if probeZero && (zeroResult.accepted || zeroResult.err != nil) {
			t.Fatal("zero ACK waiter changed the full-queue refusal", zeroResult.err)
		}
		second := <-sequence.acks
		if second.messageId != first.messageId || f.ackedCount != 0 || f.sequence().resendQueue.Len() != 1 {
			t.Fatal("duplicate compact ACK value acquired retirement authority")
		}
		if probeZero {
			accepted, err := sequence.Ack(ack.ack, 0)
			if !accepted || err != nil {
				t.Fatal("exact compact ACK could not retry after queue release", err)
			}
			if third := <-sequence.acks; third.messageId != first.messageId {
				t.Fatal("compact retry changed identity")
			}
		}
		cancel()
		if accepted, err := sequence.Ack(ack.ack, 0); accepted || err == nil || f.ctx.Err() != nil || f.sender.ctx.Err() != nil || len(sequence.acks) != 0 || f.ackedCount != 0 {
			t.Fatal("closed dedicated ACK boundary affected the live parent or accepted feedback")
		}
		f.forward(ack, f.senderIn)
		if f.ackedCount != 1 || f.sequence().resendQueue.Len() != 0 || len(sequence.acks) != 0 {
			t.Fatal("real sender did not retire exactly once through the saved peer wire")
		}
		t.Logf("SHARED_PUMP_ACK_API_DRAINED zero_probe=%t zero_returned_before_release=%t real_sender_acked=1", probeZero, zeroReturned)
	})
}
