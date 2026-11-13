// A paused signed H1 consumer fills the real SDK32 RX child, then refuses a
// contiguous run at the shared Client boundary. This extends the maintained
// paid-byte witness beyond its first overflow; no product policy is changed.
package connect

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The five-second bound is the existing overlapping-request workload gate.
// Virtual time and callback barriers select the ordering; host speed does not.
func TestSharedPumpPausedWorkerRefusedRunMeetsLoadedRequestBound(t *testing.T) {
	testSharedPumpPausedWorkerRecovery(t, true)
}

// The identical public packet stream, signed contract, carrier and budgets
// complete without recovery when no receive callback is parked.
func TestSharedPumpPausedWorkerUnpausedStreamControl(t *testing.T) {
	testSharedPumpPausedWorkerRecovery(t, false)
}

// All settings precede construction. The existing fixture installs the actual
// 32 MiB RX/root and H1 limits and charges every decoded physical owner.
func testSharedPumpPausedWorkerRecovery(t *testing.T, paused bool) {
	t.Helper()
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		layout := consumerPaidPackByteLayout{
			label: "32", byteLimit: 174762, leafBytes: 2181038,
			rootBytes: 21810380, countLimit: 85,
		}
		root := NewTransferMemoryBudget(layout.rootBytes)
		rx := NewTransferMemoryBudgetWithParent(layout.leafBytes, root)
		head := NewTransferMemoryBudgetWithParent(layout.leafBytes, root)
		pack := NewTransferMemoryBudgetWithParent(layout.leafBytes, root)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{pack, head, rx, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("paused-worker cleanup retained shared handoff quota")
				}
			}
			if !t.Failed() {
				t.Log("SHARED_PUMP_PAUSED_WORKER_CLEANUP_JOINED")
			}
		})

		var observed atomic.Pointer[Id]
		var observedBegin, observedEnd, observedRefused atomic.Int64
		var recoveryClaims atomic.Int64
		progress := func(event TransferProgressEvent) {
			id := observed.Load()
			if id == nil || event.MessageId != *id {
				return
			}
			switch event.Stage {
			case "receive_pack_begin":
				observedBegin.Add(1)
			case "receive_pack_end":
				observedEnd.Add(1)
				if !event.Success {
					observedRefused.Add(1)
				}
			}
		}
		fixture, _, _, _ := newConsumerPaidPackByteFixture(t, layout, rx, head, pack, progress,
			func(receiver bool, settings *ClientSettings) {
				// A bounded offered run fits one ordinary provider flight. The
				// receiver's actual byte/count limits and timers stay unchanged.
				settings.ContractManagerSettings.InitialContractTransferByteCount = 256 * 1024 * 1024
				settings.ContractManagerSettings.InitialNetworkPeerContractTransferByteCount = 256 * 1024 * 1024
				settings.ContractManagerSettings.StandardContractTransferByteCount = 256 * 1024 * 1024
				if !receiver {
					settings.SendBufferSettings.ResendQueueMaxByteCount = 5752188
					return
				}
				settings.ReceiveBufferSettings.receiveOwnerStageForTest = func(_ *ReceiveSequence, owner *decodedPackOwner, stage string) {
					if stage == "pack_released" && owner != nil && owner.receiveItem.memoryBudget == head {
						recoveryClaims.Add(1)
					}
				}
			})

		entered, release := make(chan struct{}), make(chan struct{})
		var enteredOnce, releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(unblock)
		var armed atomic.Bool
		var deliveredHot, deliveredSibling atomic.Int64
		var duplicatePackets atomic.Int64
		var packetStateLock sync.Mutex
		packetCounts := map[uint32]int{}
		fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, peer Peer) {
			if !armed.Load() {
				return
			}
			if paused && peer.TransferKey.LogicalLane == 0 {
				enteredOnce.Do(func() {
					close(entered)
					select {
					case <-release:
					case <-fixture.ctx.Done():
					}
				})
			}
			for _, frame := range frames {
				if frame == nil || !frame.Raw || frame.MessageType != protocol.MessageType_IpIpPacketFromProvider {
					t.Error("paused-worker callback received an unexpected application frame")
					continue
				}
				var path IpPath
				payload, err := parseIpPathWithPayloadBorrowed(frame.MessageBytes, &path)
				if err != nil || path.Protocol != IpProtocolTcp || len(payload) != 1240 {
					t.Error("paused-worker callback lost its actual TCP payload")
					continue
				}
				packetStateLock.Lock()
				packetCounts[path.SequenceNumber]++
				if packetCounts[path.SequenceNumber] != 1 {
					duplicatePackets.Add(1)
				}
				packetStateLock.Unlock()
			}
			if peer.TransferKey.LogicalLane == 0 {
				deliveredHot.Add(int64(len(frames)))
			} else if peer.TransferKey.LogicalLane == 1 {
				deliveredSibling.Add(int64(len(frames)))
			}
		})

		var receipts [384]atomic.Bool
		var completions [384]atomic.Int64
		started := time.Now()
		nextSlot := 0
		last := map[uint32]uint64{}
		send := func(lane uint32, count int) consumerPaidPackByteWire {
			if nextSlot == len(receipts) {
				t.Fatal("paused-worker bounded source exhausted its descriptor array")
			}
			slot := nextSlot
			nextSlot++
			wire := consumerPaidPackByteSend(t, fixture, lane, count, slot, &receipts[slot], &completions[slot], started)
			if previous, exists := last[lane]; exists && wire.scan.sequenceNumber != previous+1 {
				t.Fatalf("paused-worker source captured a retry or skipped an original: lane=%d previous=%d next=%d", lane, previous, wire.scan.sequenceNumber)
			}
			last[lane] = wire.scan.sequenceNumber
			return wire
		}
		for _, lane := range []uint32{0, 1} {
			warm := send(lane, 1)
			fixture.forward(warm.frame, fixture.receiverIn)
			time.Sleep(40 * time.Millisecond)
			synctest.Wait()
			fixture.acknowledge()
			if !warm.acked.Load() || root.UsedByteCount() != 0 {
				t.Fatal("paused-worker warm signed generation did not drain its actual ACK and physical owner")
			}
		}
		armed.Store(true)
		seed := send(0, 8)
		sequence := consumerForeignHeadSequence(t, fixture, seed.frame)
		sender := consumerEstablishedWireHeadSender(t, fixture, seed.frame)
		fixture.forward(seed.frame, fixture.receiverIn)
		if paused {
			<-entered
			if seed.acked.Load() || sequence.packQueueCount.Load() != 0 || rx.UsedByteCount() != seed.scan.retainedByteCount {
				t.Fatal("pause barrier did not hold exactly the real callback owner")
			}
		} else {
			fixture.acknowledge()
		}
		hot := []consumerPaidPackByteWire{seed}
		refused := []consumerPaidPackByteWire{}
		firstRefusalClass := "none"
		var firstRefusalDistance uint64
		var firstRefusalRx, firstRefusalHead, firstRefusalRoot ByteCount
		var firstRefusalCount int64
		// Ninety-six refused originals fit inside this bounded provider flight.
		// Stopping by observable refusal makes the seed independent of
		// descriptor size, while the finite offer cap catches fixture drift.
		unpausedOriginalCount := int(layout.leafBytes/seed.scan.retainedByteCount) + 96
		for index := 0; index < 240; index++ {
			wire := send(0, 8)
			id := wire.scan.messageId
			observed.Store(&id)
			observedBegin.Store(0)
			observedEnd.Store(0)
			observedRefused.Store(0)
			before := fixture.receiver.ReceiveStats().PackHandoffDropCount
			fixture.forward(wire.frame, fixture.receiverIn)
			hot = append(hot, wire)
			if !paused {
				fixture.acknowledge()
				if !wire.acked.Load() || fixture.receiver.ReceiveStats().PackHandoffDropCount != before {
					t.Fatal("unpaused identical public stream refused or failed to ACK an original")
				}
				if len(hot) == unpausedOriginalCount {
					break
				}
				continue
			}
			if wire.acked.Load() {
				t.Fatal("parked callback or its undelivered successors received a false terminal ACK")
			}
			if fixture.receiver.ReceiveStats().PackHandoffDropCount == before {
				continue
			}
			if fixture.receiver.ReceiveStats().PackHandoffDropCount != before+1 {
				t.Fatal("one bounded shared-pump offer counted more than one refusal")
			}
			refused = append(refused, wire)
			if len(refused) == 1 {
				firstRefusalClass = "decode"
				if observedBegin.Load() != 0 || observedEnd.Load() != 0 {
					firstRefusalClass = "handoff"
					if observedBegin.Load() != 1 || observedEnd.Load() != 1 || observedRefused.Load() != 1 {
						t.Fatal("first handoff refusal did not finish its real zero-time public admission")
					}
				}
				firstRefusalDistance = wire.scan.sequenceNumber - sequence.holdPromise.ingressFloor
				firstRefusalRx, firstRefusalHead, firstRefusalRoot = rx.UsedByteCount(), head.UsedByteCount(), root.UsedByteCount()
				firstRefusalCount = sequence.packQueueCount.Load()
				if firstRefusalClass != "decode" || rx.Available() >= wire.scan.retainedByteCount || root.Available() < wire.scan.retainedByteCount || head.Available() < wire.scan.retainedByteCount || firstRefusalCount >= sequence.packQueueH1Limit || len(sequence.packs) >= cap(sequence.packs) || firstRefusalDistance <= 64 {
					t.Fatalf("paused-worker first loss was not RX-full beyond the bounded successor proof: class=%s distance=%d count=%d/%d rx=%d/%d recovery=%d/%d root=%d/%d", firstRefusalClass, firstRefusalDistance, firstRefusalCount, sequence.packQueueH1Limit, firstRefusalRx, rx.TotalByteCount(), firstRefusalHead, head.TotalByteCount(), firstRefusalRoot, root.TotalByteCount())
				}
			}
			if len(refused) == 96 {
				break
			}
		}
		observed.Store(nil)
		if paused && len(refused) != 96 {
			t.Fatal("paused-worker bounded original flight did not reproduce a contiguous refused run")
		}
		if paused {
			for index, wire := range refused {
				if wire.scan.sequenceNumber != refused[0].scan.sequenceNumber+uint64(index) {
					t.Fatal("refused public originals did not form one contiguous run")
				}
			}
		}

		// A current sibling head is not subject to the 64-position successor
		// proof. It must complete while the parked worker still owns full RX.
		beforeRecovery := recoveryClaims.Load()
		sibling := send(1, 8)
		fixture.forward(sibling.frame, fixture.receiverIn)
		fixture.acknowledge()
		if !sibling.acked.Load() || deliveredSibling.Load() != 8 || paused && (deliveredHot.Load() != 0 || recoveryClaims.Load() <= beforeRecovery) || fixture.ctx.Err() != nil || sequence.ctx.Err() != nil {
			t.Fatal("a paused RX owner stranded the independent signed current head, its real ACK or shared lifetime")
		}

		unblock()
		synctest.Wait()
		fixture.acknowledge()
		if paused {
			if sequence.nextSequenceNumber != refused[0].scan.sequenceNumber || rx.UsedByteCount() != 0 || head.UsedByteCount() != 0 {
				t.Fatal("released callback failed to drain the exact accepted prefix to the first refused original")
			}
			for _, wire := range refused {
				number, retained := sender.resendQueue.ContainsMessageId(wire.scan.messageId)
				if wire.acked.Load() || !retained || number != wire.scan.sequenceNumber {
					t.Fatal("refused original lost exact sender retry custody before any recovery")
				}
			}
		}
		// Ordinary traffic after the pause now arrives beyond the refused run.
		// These tails exercise tentative/committed custody under the real hold
		// policy; they are never synthesized or assigned a fake receiver floor.
		for range 48 {
			wire := send(0, 8)
			fixture.forward(wire.frame, fixture.receiverIn)
			hot = append(hot, wire)
			fixture.acknowledge()
		}
		heldCount := sequence.receiveQueue.Len()
		heldRx := rx.UsedByteCount()
		if paused && (heldCount == 0 || heldRx == 0 || sequence.nextSequenceNumber != refused[0].scan.sequenceNumber) {
			t.Fatal("later real originals did not retain DATA behind the actual refused run")
		}
		recoveryStarted := time.Now()
		// The maintained bounded pump models concurrent propagation, not
		// forty milliseconds of serial service for every ACK. Its separate
		// multi-frame regression proves that distinction.
		fixture.startWire(40*time.Millisecond, 0)
		// Fake time is the declared request deadline, not a wall-time sleep or
		// a short negative assertion used to manufacture a race.
		time.Sleep(5 * time.Second)
		synctest.Wait()
		recoveryStats := fixture.sender.SendRecoveryStats()
		remaining := 0
		for _, wire := range hot {
			if !wire.acked.Load() {
				remaining++
			}
		}
		t.Logf("SHARED_PUMP_PAUSED_WORKER_RECOVERY paused=%t first_refusal=%s distance=%d initial_queue=%d initial_rx=%d initial_head=%d initial_root=%d refused=%d later_held=%d later_rx=%d elapsed_ns=%d hot_packs=%d delivered_packets=%d pending_originals=%d recovery_claims=%d floor=%d", paused, firstRefusalClass, firstRefusalDistance, firstRefusalCount, firstRefusalRx, firstRefusalHead, firstRefusalRoot, len(refused), heldCount, heldRx, time.Since(recoveryStarted).Nanoseconds(), len(hot), deliveredHot.Load(), remaining, recoveryClaims.Load(), sequence.nextSequenceNumber)
		t.Logf("SHARED_PUMP_PAUSED_WORKER_SEND_POLICY lane_rule=%t defer_progress=%t timeout_writes=%d lane_probes=%d lane_rides=%d lane_promotions=%d selective_gap_writes=%d", sender.sendBufferSettings.ReliableLaneProvenRecovery, sender.sendBufferSettings.DeferTimeoutResendWhileCumulativeProgress, recoveryStats.TimeoutResendWriteCount, recoveryStats.LaneProbeWriteCount, recoveryStats.LaneProbeRideCount, recoveryStats.LaneHeadPromotionCount, recoveryStats.SelectiveGapWriteCount)
		if remaining != 0 {
			t.Fatalf("PAUSED_WORKER_REFUSED_RUN_EXCEEDS_LOADED_REQUEST_BOUND: pending=%d/%d after5s, refused=%d first_loss=%s distance=%d rx=%d recovery=%d root=%d; shared resident still live=%t", remaining, len(hot), len(refused), firstRefusalClass, firstRefusalDistance, rx.UsedByteCount(), head.UsedByteCount(), root.UsedByteCount(), fixture.ctx.Err() == nil && sequence.ctx.Err() == nil)
		}
		if duplicatePackets.Load() != 0 || deliveredHot.Load() != int64(8*len(hot)) || deliveredSibling.Load() != 8 || len(sender.sendItems) != 0 || sequence.receiveQueue.Len() != 0 || sequence.packQueueCount.Load() != 0 || sequence.packQueueByteCount.Load() != 0 || root.UsedByteCount() != 0 || rx.UsedByteCount() != 0 || head.UsedByteCount() != 0 || pack.Stats().QuotaByteCount != 0 {
			t.Fatal("paused-worker recovery duplicated delivery, lost a real terminal ACK or retained physical custody")
		}
		for _, wire := range refused {
			if !bytes.Equal(wire.frame.pack.SequenceId, seed.frame.pack.SequenceId) {
				t.Fatal("the refused run silently changed signed receive generation")
			}
		}
	})
}
