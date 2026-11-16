// Sustained signed H1 traffic extends the paused-worker wire witness. Every
// original and retry traverses the real shared pump; only one callback pauses.
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

func TestSharedPumpSustainedCappedFlightMeetsLoadedRequestBound(t *testing.T) {
	testSharedPumpSustainedWorkerRecovery(t, 655360, true)
}

func TestSharedPumpSustainedUnpausedFlightControl(t *testing.T) {
	testSharedPumpSustainedWorkerRecovery(t, 0, false)
}

func TestSharedPumpSustainedBackedReceiveMatchedDepthMeetsLoadedRequestBound(t *testing.T) {
	// This separate count counterfactual changes no shipping queue policy.
	testSharedPumpSustainedWorkerRecovery(t, 2181038, true, sharedPumpSustainedReceiveGeometry{physicalBytes: 7633633, count: 256})
}

type sharedPumpSustainedReceiveGeometry struct {
	physicalBytes ByteCount
	count         int
}

// Fixed descriptors hold scalar wire/receipt facts, never extra pool shares.
// The original fixture remains the sole owner of each captured carrier.
type sharedPumpSustainedOriginal struct {
	id                   Id
	number               uint64
	encoded, retained    ByteCount
	originalAt, retryAt  time.Time
	refusedAt, resendDue time.Time
	finalRefusal         bool
	retryDueAt           time.Time
	retryDeadline        time.Time
	retryKind            sendRecoveryKind
	retryDueObserved     bool
	acknowledged         atomic.Bool
	acknowledgedAtNanos  atomic.Int64
	encodedForAck        atomic.Int64
}

// Run publishes only copied values. The reader never borrows a mutable item
// through a later ACK or RTO; requested cuts have an explicit release edge.
type sharedPumpSustainedSenderCut struct {
	at, due          time.Time
	id               Id
	number           uint64
	retained, queued int
	encoded, window  ByteCount
	acknowledged     int64
	wait             int64
	kind             sendRecoveryKind
	detail           sharedPumpSustainedSenderDetail
}

// Diagnostic copies are taken only on Run; no pooled pointer escapes.
type sharedPumpSustainedItemDetail struct {
	number                                                                                      uint64
	sent, due, deferAck                                                                         time.Time
	sends, defers                                                                               int
	kind                                                                                        sendRecoveryKind
	selective, head, deferred, repairMarked, repairPending, repairAccepted, lanePassed, pending bool
}

type sharedPumpSustainedSenderDetail struct {
	first                              [8]sharedPumpSustainedItemDetail
	heap                               sharedPumpSustainedItemDetail
	selective, dueCount                int
	next                               uint64
	lastAck                            time.Time
	storedRttMean, storedRttMin        time.Duration
	storedRttCount                     int
	pacer, selected, cumulativePending bool
	pacerRate, pacerSent, pacerAcked   ByteCount
	pacerDue                           time.Time
}

type sharedPumpSustainedCutRequest struct {
	reply   chan sharedPumpSustainedSenderCut
	release chan struct{}
	raw     bool
}

// The original roots retain their 32 MiB receive geometry and independent TX
// owner. Separate controls change physical RX backing and then H1 count. This
// signed component topology does not qualify a new SDK memory configuration.
func testSharedPumpSustainedWorkerRecovery(t *testing.T, advertisedCap ByteCount, paused bool, receiveGeometry ...sharedPumpSustainedReceiveGeometry) {
	t.Helper()
	if len(receiveGeometry) > 1 {
		t.Fatal("sustained source accepts at most one physical RX control")
	}
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		layout := consumerPaidPackByteLayout{label: "32", byteLimit: 174762,
			leafBytes: 2181038, rootBytes: 21810380, countLimit: 85}
		rxLimit := layout.leafBytes
		if len(receiveGeometry) == 1 {
			if receiveGeometry[0].physicalBytes != 7633633 || advertisedCap != layout.leafBytes || !paused ||
				receiveGeometry[0].count != 0 && receiveGeometry[0].count != 256 {
				t.Fatal("backed RX control changed its physical capacity, encoded permission or pause")
			}
			rxLimit = receiveGeometry[0].physicalBytes
			if receiveGeometry[0].count != 0 {
				layout.countLimit = receiveGeometry[0].count
			}
		}
		root := NewTransferMemoryBudget(layout.rootBytes)
		rx := NewTransferMemoryBudgetWithParent(rxLimit, root)
		head := NewTransferMemoryBudgetWithParent(layout.leafBytes, root)
		pack := NewTransferMemoryBudgetWithParent(layout.leafBytes, root)
		tx := NewTransferMemoryBudget(5752188)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{tx, pack, head, rx, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("sustained source retained quota after all wire owners and Clients joined")
				}
			}
			t.Log("SHARED_PUMP_SUSTAINED_CLEANUP_JOINED")
		})

		activity := make(chan struct{}, 1)
		notify := func() {
			select {
			case activity <- struct{}{}:
			default:
			}
		}
		var capacityWaits, timerArms, dueResends, recoveryClaims atomic.Int64
		var firstRefusedNumber atomic.Uint64
		var firstRefusedKnown atomic.Bool
		var firstDueAtNanos atomic.Int64
		var firstDueDeadlineNanos atomic.Int64
		var firstDueKind atomic.Int64
		var duePredicateFailures atomic.Int64
		var senderRef atomic.Pointer[SendSequence]
		const maxOriginals = 8192
		var initialCuts [maxOriginals + 32]atomic.Pointer[sharedPumpSustainedSenderCut]
		var capacityCut atomic.Pointer[sharedPumpSustainedSenderCut]
		var lastRefusedDueCut atomic.Pointer[sharedPumpSustainedSenderCut]
		cutRequests := make(chan sharedPumpSustainedCutRequest, 1)
		var acknowledged, acknowledgedEncoded, offered, accepted, ackCallbacks, ackErrors atomic.Int64
		copySender := func(owner *SendSequence, raw bool) sharedPumpSustainedSenderCut {
			count, encoded := owner.resendQueue.QueueSize()
			window, _ := owner.receivedWindowAdvertisement()
			if !raw {
				// Preserve the original fixture's existing capacity/warm checks.
				window = owner.sendWindowSnapshot(time.Now()).Window
			}
			cut := sharedPumpSustainedSenderCut{at: time.Now(), retained: len(owner.sendItems), queued: count,
				encoded: encoded, window: window, acknowledged: acknowledgedEncoded.Load()}
			if !raw {
				return cut
			}
			copyItem := func(item *sendItem) sharedPumpSustainedItemDetail {
				if item == nil {
					return sharedPumpSustainedItemDetail{}
				}
				return sharedPumpSustainedItemDetail{number: item.sequenceNumber,
					sent: item.sendTime, due: item.resendTime, deferAck: item.timeoutDeferAckTime,
					sends: item.sendCount, defers: item.timeoutDeferCount, kind: item.recoveryKind,
					selective: item.selectiveAcked, head: item.head, deferred: item.deferralOutstanding,
					repairMarked:  owner.fifoRepair.passed.set && owner.fifoRepair.passed.messageId == item.messageId && owner.fifoRepair.passed.number == item.sequenceNumber,
					repairPending: item.fifoRepairPending, repairAccepted: item.fifoRepairAccepted,
					lanePassed: owner.laneAcknowledgedPast(item),
					pending:    owner.ackWindow.PendingDispositionFor(item.sequenceNumber, item.messageId)}
			}
			cut.detail = sharedPumpSustainedSenderDetail{heap: copyItem(owner.resendQueue.PeekFirst()),
				next: owner.nextSequenceNumber, lastAck: owner.lastCumulativeAckTime,
				pacer: owner.windowPacer.service != nil, selected: owner.pendingRecovery != nil,
				cumulativePending: owner.ackWindow.PendingCumulativeProgress()}
			if service := owner.windowPacer.service; service != nil {
				service.stateLock.Lock()
				cut.detail.pacerRate, cut.detail.pacerSent, cut.detail.pacerAcked = owner.windowPacer.rate, owner.windowPacer.serviceSent, owner.windowPacer.serviceAcked
				cut.detail.pacerDue = owner.windowPacer.waiter.deadline
				service.stateLock.Unlock()
			}
			owner.rttWindow.stateLock.Lock()
			cut.detail.storedRttCount = owner.rttWindow.windowCount
			if owner.rttWindow.windowCount > 0 {
				cut.detail.storedRttMean = owner.rttWindow.netRtt / time.Duration(owner.rttWindow.windowCount)
			}
			if owner.rttWindow.minimumCount > 0 {
				cut.detail.storedRttMin = owner.rttWindow.minimums[owner.rttWindow.minimumHeadIndex].rtt
			}
			owner.rttWindow.stateLock.Unlock()
			for index, item := range owner.sendItems {
				if index < len(cut.detail.first) {
					cut.detail.first[index] = copyItem(item)
				}
				if item != nil && item.selectiveAcked {
					cut.detail.selective++
				}
				if item != nil && !time.Now().Before(item.resendTime) {
					cut.detail.dueCount++
				}
			}
			return cut
		}
		var observed atomic.Pointer[Id]
		var observedBegin, observedEnd atomic.Int64
		progress := func(event TransferProgressEvent) {
			if id := observed.Load(); id != nil && event.MessageId == *id {
				switch event.Stage {
				case "receive_pack_begin":
					observedBegin.Add(1)
				case "receive_pack_end":
					observedEnd.Add(1)
				}
			}
		}
		fixture, _, _, _ := newConsumerPaidPackByteFixture(t, layout, rx, head, pack, progress,
			func(receiver bool, settings *ClientSettings) {
				settings.ContractManagerSettings.InitialContractTransferByteCount = 256 * 1024 * 1024
				settings.ContractManagerSettings.InitialNetworkPeerContractTransferByteCount = 256 * 1024 * 1024
				settings.ContractManagerSettings.StandardContractTransferByteCount = 256 * 1024 * 1024
				if !receiver {
					send := settings.SendBufferSettings
					// Constant mode ignores the advertised field. Enable the real
					// rule before construction, with the same TX owner in both arms.
					send.ResendQueueBudget = tx
					send.WindowSizing = WindowSizingFromDelivery
					send.ApplyWindowSizing()
					send.ResendQueueMaxByteCount = 5752188
					send.TransferWireMessageObserver = func(TransferWireMessageObservation) { notify() }
					send.beforeResendCapacityWaitForTest = func(id sendSequenceId) {
						if id.LogicalLane == 0 {
							wait := capacityWaits.Add(1)
							if owner := senderRef.Load(); owner != nil && owner.id() == id {
								cut := copySender(owner, false)
								cut.wait = wait
								capacityCut.Store(&cut)
							}
							notify()
						}
					}
					send.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
						if owner := senderRef.Load(); owner != nil && owner.id() == id {
							if number >= uint64(len(initialCuts)) {
								t.Error("sustained initial owner cut exceeded its fixed bound")
								return
							}
							if item := owner.resendQueue.GetBySequenceNumber(number); item != nil {
								cut := sharedPumpSustainedSenderCut{at: time.Now(), due: item.resendTime,
									id: item.messageId, number: item.sequenceNumber}
								initialCuts[number].Store(&cut)
							}
						}
					}
					send.afterApplyAckSnapshotForTest = func(id sendSequenceId) {
						if owner := senderRef.Load(); owner != nil && owner.id() == id {
							select {
							case request := <-cutRequests:
								request.reply <- copySender(owner, request.raw)
								select {
								case <-request.release:
								case <-owner.ctx.Done():
								}
							default:
							}
						}
					}
					send.afterDeadlineTimerArmForTest = func(id sendSequenceId) {
						if id.LogicalLane == 0 {
							timerArms.Add(1)
						}
					}
					send.beforeDueResendForTest = func(id sendSequenceId, number uint64) {
						if id.LogicalLane == 0 {
							dueResends.Add(1)
							if firstRefusedKnown.Load() && number == firstRefusedNumber.Load() {
								// This hook runs on the actual retained-item owner.
								// Inspect its current due predicate, not a test sleep.
								owner := senderRef.Load()
								if owner != nil {
									item := owner.resendQueue.GetBySequenceNumber(number)
									if item == nil || item.resendTime.IsZero() || time.Now().Before(item.resendTime) {
										duePredicateFailures.Add(1)
									} else {
										cut := sharedPumpSustainedSenderCut{at: time.Now(), due: item.resendTime,
											id: item.messageId, number: item.sequenceNumber, kind: item.recoveryKind}
										lastRefusedDueCut.Store(&cut)
										if firstDueAtNanos.CompareAndSwap(0, cut.at.UnixNano()) {
											firstDueDeadlineNanos.Store(cut.due.UnixNano())
											firstDueKind.Store(int64(cut.kind))
										}
									}
								}
							}
						}
					}
					return
				}
				settings.ReceiveBufferSettings.AdvertiseReceiveWindow = true
				settings.ReceiveBufferSettings.AdvertisedReceiveWindowMaxByteCount = advertisedCap
				settings.ReceiveBufferSettings.receiveOwnerStageForTest = func(_ *ReceiveSequence, owner *decodedPackOwner, stage string) {
					if stage == "pack_released" && owner != nil && owner.receiveItem.memoryBudget == head {
						recoveryClaims.Add(1)
					}
				}
			})

		entered, release := make(chan struct{}), make(chan struct{})
		var enterOnce, releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		var pauseArmed atomic.Bool
		var pausedPacketNumber atomic.Uint64
		var delivered, duplicatePackets atomic.Int64
		var packetStateLock sync.Mutex
		packetCounts := make(map[uint32]int)
		fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, peer Peer) {
			if peer.TransferKey.LogicalLane != 0 {
				return
			}
			if paused && pauseArmed.Load() {
				enterOnce.Do(func() {
					if len(frames) != 0 {
						var first IpPath
						if _, err := parseIpPathWithPayloadBorrowed(frames[0].MessageBytes, &first); err == nil {
							pausedPacketNumber.Store(uint64(first.SequenceNumber))
						}
					}
					close(entered)
					select {
					case <-release:
					case <-fixture.ctx.Done():
					}
				})
			}
			for _, frame := range frames {
				if frame == nil || !frame.Raw || frame.MessageType != protocol.MessageType_IpIpPacketFromProvider {
					t.Error("sustained callback lost its actual consumer packet")
					continue
				}
				var path IpPath
				payload, err := parseIpPathWithPayloadBorrowed(frame.MessageBytes, &path)
				if err != nil || path.Protocol != IpProtocolTcp || len(payload) != 1240 {
					t.Error("sustained callback changed its TCP payload")
					continue
				}
				if path.SequenceNumber < 100000 {
					continue // Warm, actual signed feedback is outside the workload.
				}
				packetStateLock.Lock()
				packetCounts[path.SequenceNumber]++
				if packetCounts[path.SequenceNumber] != 1 {
					duplicatePackets.Add(1)
				}
				packetStateLock.Unlock()
				delivered.Add(1)
			}
		})

		// A real warm Pack discovers each cost and receives the actual ceiling.
		var warmAck atomic.Bool
		var warmCompleted atomic.Int64
		warm := consumerPaidPackByteSend(t, fixture, 0, 8, 0, &warmAck, &warmCompleted, time.Now())
		warmEncoded := ByteCount(len(warm.frame.bytes))
		fixture.forward(warm.frame, fixture.receiverIn)
		sequence := consumerForeignHeadSequence(t, fixture, warm.frame)
		sender := consumerEstablishedWireHeadSender(t, fixture, warm.frame)
		senderRef.Store(sender)
		ownerCut := func(raw ...bool) sharedPumpSustainedSenderCut {
			t.Helper()
			request := sharedPumpSustainedCutRequest{reply: make(chan sharedPumpSustainedSenderCut, 1), release: make(chan struct{})}
			request.raw = len(raw) == 1 && raw[0]
			cutRequests <- request
			sender.ackWindow.Wake() // Diagnostic wake supplies no receipt authority.
			timer := time.NewTimer(fixture.sender.settings.SendBufferSettings.AckTimeout)
			defer timer.Stop()
			select {
			case cut := <-request.reply:
				close(request.release)
				synctest.Wait()
				return cut
			case <-fixture.ctx.Done():
				t.Fatal("sustained send owner stopped before its copied cut")
				return sharedPumpSustainedSenderCut{}
			case <-sender.ctx.Done():
				t.Fatal("SUSTAINED_UNQUALIFIED_OWNER_CUT: actual send owner terminated before its copied cut")
				return sharedPumpSustainedSenderCut{}
			case <-timer.C:
				t.Fatal("SUSTAINED_UNQUALIFIED_OWNER_CUT: actual send owner failed to answer within its existing lifetime")
				return sharedPumpSustainedSenderCut{}
			}
		}
		logStall := func(label string) {
			cut := ownerCut(true)
			held := sequence.receiveQueue.AscendingItems(nil)
			numbers := make([]uint64, 0, len(held))
			committed := 0
			for _, item := range held {
				numbers = append(numbers, item.sequenceNumber)
				if item.committed {
					committed++
				}
			}
			root.admissionLock.Lock()
			floor, boundary := sequence.holdPromise.ingressFloor, sequence.holdPromise.boundary
			promiseSet, promiseReserved, promiseLoan := sequence.holdPromise.set, sequence.holdPromise.reservedByteCount, sequence.holdPromise.ingressLoanByteCount
			pressure := sequence.memoryPressure
			root.admissionLock.Unlock()
			t.Logf("SUSTAINED_DEBUG_SEND cut=%s at=%s retained=%d encoded=%d advertised=%d detail=%+v", label, cut.at, cut.retained, cut.encoded, cut.window, cut.detail)
			t.Logf("SUSTAINED_DEBUG_RECEIVE cut=%s floor=%d boundary=%d promise=%t reserved=%d loan=%d held=%v committed=%d rx=%d head=%d root=%d pending=%t pending_number=%d active=%t active_number=%d ready=%t pressure_reserved=%d pressure_borrowed=%d pressure_owner=%t pack_count=%d pack_bytes=%d delivered=%d acked=%d accepted=%d",
				label, floor, boundary, promiseSet, promiseReserved, promiseLoan, numbers, committed,
				rx.UsedByteCount(), head.UsedByteCount(), root.UsedByteCount(), pressure.pendingSet, pressure.pending.sequenceNumber,
				pressure.activeSet, pressure.active.sequenceNumber, pressure.ready, pressure.reservedByteCount, pressure.borrowedByteCount,
				pressure.owner != nil, sequence.packQueueCount.Load(), sequence.packQueueByteCount.Load(), delivered.Load(), acknowledged.Load(), accepted.Load())
			t.Logf("SUSTAINED_DEBUG_CUSTODY cut=%s send_live=%t receive_live=%t ack_callbacks=%d ack_errors=%d receive=%+v", label, sender.ctx.Err() == nil, sequence.ctx.Err() == nil, ackCallbacks.Load(), ackErrors.Load(), fixture.receiver.ReceiveStats())
		}
		time.Sleep(40 * time.Millisecond)
		fixture.acknowledge()
		permission, known := sender.receivedWindowAdvertisement()
		wantPermission := layout.leafBytes
		if advertisedCap > 0 {
			wantPermission = min(wantPermission, advertisedCap)
		}
		if !warmAck.Load() || !known || permission != wantPermission || root.UsedByteCount() != 0 ||
			ownerCut().window != permission || warm.scan.retainedByteCount <= warm.scan.messageByteCount {
			t.Fatal("sustained source did not consume real advertised feedback and pay its signed decode geometry")
		}
		// The actual scanned payload is a lower bound on encoded wire cost,
		// hence this source exceeds twenty-four full DEFAULT encoded flights.
		// No guessed number of physical owners chooses the overflow position.
		originalCount := 24 * (int(layout.leafBytes/ByteCount(warm.scan.messageByteCount)) + 1)
		if originalCount > maxOriginals || originalCount < 140 {
			t.Fatal("signed wire geometry exceeds the fixed sustained descriptor bound")
		}
		if len(receiveGeometry) == 1 {
			receive := fixture.receiver.settings.ReceiveBufferSettings
			if originalCount != 5112 || permission != 2181038 || rx.TotalByteCount() != 7633633 ||
				root.TotalByteCount() != 21810380 || head.TotalByteCount() != 2181038 || pack.TotalByteCount() != 2181038 ||
				tx.TotalByteCount() != 5752188 || rx.Parent() != root || head.Parent() != root || pack.Parent() != root || tx.Parent() != nil ||
				sequence.receiveQueue.Budget() != rx || receive.ReceiveQueueBudget != rx || receive.ReceiveHeadRecoveryBudget != head || receive.PackQueueBudget != pack ||
				receive.ReceiveQueueMaxByteCount != 2181038 || receive.AdvertisedReceiveWindowMaxByteCount != 2181038 ||
				receive.H1SequenceBufferSize != layout.countLimit || sequence.packQueueH1Limit != int64(layout.countLimit) ||
				receive.H1SequenceBufferByteCount != 174762 || sequence.packQueueH1ByteLimit != 174762 ||
				fixture.sender.settings.SendBufferSettings.ResendQueueBudget != tx {
				t.Fatal("backed RX control lost the actual shared root, unchanged encoded flight or independent TX owner")
			}
			// Payload is a lower bound on each encoded group. Round its full
			// permission count plus a head/protocol margin to a power of two.
			flightWithMargin := int((permission+warm.scan.messageByteCount-1)/warm.scan.messageByteCount) + 2
			matchedCount := 1
			for matchedCount < flightWithMargin {
				matchedCount *= 2
			}
			if receiveGeometry[0].count != 0 && (matchedCount != 256 || layout.countLimit != matchedCount) {
				t.Fatal("matched-depth control did not cover the actual encoded flight with its fixed bounded count")
			}
			t.Logf("SHARED_PUMP_BACKED_RECEIVE_GEOMETRY physical_rx=%d encoded_permission=%d root=%d head=%d pack=%d tx=%d originals=%d actual_h1_count=%d flight_with_margin=%d matched_count=%d h1_bytes=%d",
				rx.TotalByteCount(), permission, root.TotalByteCount(), head.TotalByteCount(), pack.TotalByteCount(), tx.TotalByteCount(), originalCount,
				sequence.packQueueH1Limit, flightWithMargin, matchedCount, sequence.packQueueH1ByteLimit)
		}
		var originals [maxOriginals]sharedPumpSustainedOriginal
		var sourceBusy, sourceRefused atomic.Bool
		var admissionRefusals atomic.Int64
		sourceDone := make(chan struct{})
		pauseDone := make(chan struct{})
		var releasedAtNanos atomic.Int64
		ackPumpDone := startLaneAckTestPump(fixture.ctx, fixture.receiverOut, fixture.senderIn, 40*time.Millisecond)
		// This cleanup runs before the inherited Client/route cleanup. No
		// concurrent wire owner may race the inherited route drain.
		t.Cleanup(func() {
			unblock()
			fixture.cancel()
			<-sourceDone
			<-pauseDone
			<-ackPumpDone
		})
		// An independent finite timer owns the one callback release. Even a
		// mutant that blocks the shared receive pump cannot strand this gate.
		go func() {
			defer close(pauseDone)
			select {
			case <-entered:
			case <-fixture.ctx.Done():
				return
			}
			timer := time.NewTimer(80 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
				releasedAtNanos.Store(time.Now().UnixNano())
				unblock()
				notify()
			case <-fixture.ctx.Done():
			}
		}()
		go func() {
			defer close(sourceDone)
			defer notify()
			for slot := range originalCount {
				frames := make([]*protocol.Frame, 8)
				for index := range frames {
					packet := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck,
						uint32(100000+slot*8+index), bytes.Repeat([]byte{0x58}, 1240))
					frames[index] = &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider,
						Raw: true, MessageBytes: MessagePoolCopy(packet)}
				}
				ack := func(err error) {
					ackCallbacks.Add(1)
					if err != nil {
						ackErrors.Add(1)
					}
					if err == nil {
						originals[slot].acknowledgedAtNanos.Store(time.Now().UnixNano())
						originals[slot].acknowledged.Store(true)
						acknowledged.Add(1)
						acknowledgedEncoded.Add(originals[slot].encodedForAck.Load())
						notify()
					}
				}
				admitted := false
				// A refused one-second offer keeps caller ownership. The same
				// source group retries that real admission, still bounded by the
				// unchanged reliable ACK lifetime and the outer owner watchdog.
				sourceBusy.Store(true)
				for attempt := 0; attempt <= int(fixture.sender.settings.SendBufferSettings.AckTimeout/time.Second); attempt++ {
					offered.Add(1)
					if fixture.sender.SendMultiWithTimeout(frames, fixture.receiver.ClientId(), ack,
						time.Second, TransferKey{LogicalLane: 0}) {
						admitted = true
						break
					}
					if fixture.ctx.Err() != nil {
						break
					}
					admissionRefusals.Add(1)
				}
				sourceBusy.Store(false)
				if !admitted {
					for _, frame := range frames {
						MessagePoolReturn(frame.MessageBytes)
					}
					if fixture.ctx.Err() == nil {
						sourceRefused.Store(true)
					}
					return
				}
				accepted.Add(1)
			}
		}()

		started := time.Now()
		watchdogDuration := min(fixture.sender.settings.SendBufferSettings.AckTimeout,
			fixture.receiver.settings.ReceiveBufferSettings.GapTimeout)
		watchdog := time.NewTimer(watchdogDuration)
		defer watchdog.Stop()
		var boundWake <-chan time.Time
		var recoveryTimer *time.Timer
		if !paused {
			recoveryTimer = time.NewTimer(5 * time.Second)
			defer recoveryTimer.Stop()
			boundWake = recoveryTimer.C
		}
		var releasedAt time.Time
		var firstPressure receiveMemoryPressureRequest
		var firstPressurePending bool
		var firstRefusedRx, firstRefusedHead, firstRefusedRoot ByteCount
		var firstRefusedDistance uint64
		firstRefusedSlot := -1
		firstTimerArms := int64(0)
		refusedOriginals, physicalRetries, refillWindows := 0, 0, 0
		lastCapacity, lastRefillEncoded := capacityWaits.Load(), int64(0)
		maxEncodedFlight, maxProjectedRetained, maxRx := ByteCount(0), ByteCount(0), ByteCount(0)
		maxOriginalEncoded := warmEncoded
		originalWrites, seenPause := 0, false
		refillsAtRelease := 0
		enteredRemaining, releasedRemaining := ByteCount(0), ByteCount(0)
		completed, boundMiss, watchdogExpired := false, false, false
		var boundAckCount int64
		var firstStoredDeadlineReached bool
		for transitions := 0; transitions < maxOriginals*16; transitions++ {
			synctest.Wait()
			if !seenPause && pauseArmed.Load() {
				select {
				case <-entered:
					seenPause = true
					enteredRemaining = ByteCount(int64(originalCount)-accepted.Load()) * warm.scan.messageByteCount
					if !sourceBusy.Load() || enteredRemaining < 10*permission {
						t.Error("SUSTAINED_SOURCE_ENDED_AT_PAUSE: ten full permissions of continuously offered remaining demand were not present")
					}
					packetNumber := pausedPacketNumber.Load()
					if packetNumber < 100000 || packetNumber >= uint64(100000+8*originalCount) {
						t.Fatal("pause barrier lost its actual callback packet identity")
					}
					owner := &originals[(packetNumber-100000)/8]
					if owner.originalAt.IsZero() || owner.acknowledged.Load() || rx.UsedByteCount() < owner.retained {
						t.Fatal("pause barrier did not retain its actual physical callback owner without ACK")
					}
				default:
				}
			}
			if nanos := releasedAtNanos.Load(); releasedAt.IsZero() && nanos != 0 {
				releasedAt = time.Unix(0, nanos)
				refillsAtRelease = refillWindows
				releasedRemaining = ByteCount(int64(originalCount)-accepted.Load()) * warm.scan.messageByteCount
				if !sourceBusy.Load() || releasedRemaining < 10*permission {
					t.Error("SUSTAINED_SOURCE_ENDED_AT_RELEASE: ten full permissions of actual remaining demand were not present")
				}
				recoveryTimer = time.NewTimer(max(0, time.Until(releasedAt.Add(5*time.Second))))
				defer recoveryTimer.Stop()
				boundWake = recoveryTimer.C
			}
			if len(fixture.senderOut) != 0 {
				wire := fixture.take(fixture.senderOut)
				if wire.pack == nil {
					t.Fatal("sustained application route published a non-Pack")
				}
				if !bytes.Equal(wire.pack.SequenceId, warm.frame.pack.SequenceId) {
					t.Fatal("SUSTAINED_DEBUG_GENERATION_CHANGED: emitted Pack escaped the warm sender generation")
				}
				if len(wire.pack.Frames) == 0 {
					fixture.forward(wire, fixture.receiverIn)
					wire.pack = nil
					continue
				}
				path, err := FilteredTransferPath(wire.bytes)
				if err != nil {
					t.Fatal(err)
				}
				scan, status := scanReceiveMemoryPack(wire.bytes, path)
				if status != receiveMemoryPackScanReady || scan.consumerFrameCount != 8 ||
					scan.logicalLane != 0 || !scan.loanSafe || wire.pack.Nack {
					t.Fatal("sustained producer changed its exact reliable eight-packet wire group")
				}
				var packetPath IpPath
				if _, err := parseIpPathWithPayloadBorrowed(wire.pack.Frames[0].MessageBytes, &packetPath); err != nil ||
					packetPath.SequenceNumber < 100000 || (packetPath.SequenceNumber-100000)%8 != 0 {
					t.Fatal("sustained wire cannot be joined to its actual caller-owned packet identity")
				}
				slot := int((packetPath.SequenceNumber - 100000) / 8)
				if slot >= originalCount {
					t.Fatal("sustained wire exceeded its finite source")
				}
				original := &originals[slot]
				first := original.originalAt.IsZero()
				if first {
					original.id, original.number = scan.messageId, scan.sequenceNumber
					original.encoded, original.retained = ByteCount(len(wire.bytes)), scan.retainedByteCount
					original.encodedForAck.Store(int64(original.encoded))
					maxOriginalEncoded = max(maxOriginalEncoded, original.encoded)
					original.originalAt = time.Now()
					originalWrites++
				} else {
					if original.id != scan.messageId || original.number != scan.sequenceNumber ||
						!bytes.Equal(wire.pack.SequenceId, warm.frame.pack.SequenceId) {
						t.Fatal("actual resend changed the retained original or generation")
					}
					physicalRetries++
					if original.retryAt.IsZero() {
						original.retryAt = time.Now()
						if cut := lastRefusedDueCut.Load(); cut != nil && cut.id == original.id && cut.number == original.number {
							original.retryDueAt, original.retryDeadline = cut.at, cut.due
							original.retryKind, original.retryDueObserved = cut.kind, true
						}
					}
				}
				id := scan.messageId
				observed.Store(&id)
				observedBegin.Store(0)
				observedEnd.Store(0)
				before := fixture.receiver.ReceiveStats().PackHandoffDropCount
				forwardStarted := time.Now()
				fixture.forward(wire, fixture.receiverIn)
				wire.pack = nil // Keep scalar evidence, not decoded payload history.
				if seenPause && !forwardStarted.Equal(time.Now()) {
					t.Error("SHARED_PUMP_WAITED_FOR_PAUSED_WORKER: a forwarded share waited through virtual time instead of completing shared admission")
				}
				maxRx = max(maxRx, rx.UsedByteCount())
				if first && fixture.receiver.ReceiveStats().PackHandoffDropCount > before {
					if len(receiveGeometry) == 1 && firstRefusedSlot == -1 {
						// This counter also covers a decoded Pack refused by the
						// bounded zero-wait worker handoff. Preserve that boundary.
						t.Logf("SUSTAINED_BACKED_FIRST_REFUSAL number=%d drop_delta=%d receive_begin=%d receive_end=%d acked=%t rx_used=%d rx_available=%d head_available=%d root_available=%d pack_count=%d count_limit=%d pack_bytes=%d byte_limit=%d pack_quota=%d",
							scan.sequenceNumber, fixture.receiver.ReceiveStats().PackHandoffDropCount-before,
							observedBegin.Load(), observedEnd.Load(), original.acknowledged.Load(), rx.UsedByteCount(), rx.Available(),
							head.Available(), root.Available(), sequence.packQueueCount.Load(), layout.countLimit,
							sequence.packQueueByteCount.Load(), layout.byteLimit, pack.Stats().QuotaByteCount)
					}
					if len(receiveGeometry) == 0 && (fixture.receiver.ReceiveStats().PackHandoffDropCount != before+1 ||
						observedBegin.Load() != 0 || observedEnd.Load() != 0 || original.acknowledged.Load()) {
						t.Fatal("sustained first refusal was not one pre-Pack decode refusal without terminal ACK")
					}
					original.finalRefusal, original.refusedAt = true, time.Now()
					refusedOriginals++
					if scan.sequenceNumber >= uint64(len(initialCuts)) {
						t.Fatal("silent refused original exceeded the fixed copied owner census")
					}
					cut := initialCuts[scan.sequenceNumber].Load()
					if cut == nil || cut.id != id || cut.number != scan.sequenceNumber {
						t.Fatal("silent final decode refusal lost its copied actual initial owner clock")
					}
					original.resendDue = cut.due
					if firstRefusedSlot == -1 {
						firstRefusedSlot = slot
						firstRefusedNumber.Store(scan.sequenceNumber)
						firstRefusedKnown.Store(true)
						root.admissionLock.Lock()
						firstPressure, firstPressurePending = sequence.memoryPressure.pending, sequence.memoryPressure.pendingSet
						firstRefusedDistance = scan.sequenceNumber - sequence.holdPromise.ingressFloor
						root.admissionLock.Unlock()
						firstRefusedRx, firstRefusedHead, firstRefusedRoot = rx.UsedByteCount(), head.UsedByteCount(), root.UsedByteCount()
						firstTimerArms = timerArms.Load()
						if len(receiveGeometry) == 0 && (!paused || !seenPause || !releasedAt.IsZero() || rx.Available() >= scan.retainedByteCount ||
							root.Available() < scan.retainedByteCount || head.Available() < scan.retainedByteCount ||
							sequence.packQueueCount.Load() >= int64(layout.countLimit) || firstRefusedDistance <= 64) {
							t.Error("SUSTAINED_REFUSAL_GEOMETRY_CHANGED: first final refusal was not paused ordinary RX exhaustion beyond the bounded successor proof")
						}
					}
				}
				observed.Store(nil)
				continue
			}

			// No new original can serialize at this actual capacity boundary.
			// At least one whole advertised ceiling of real terminal encoded
			// receipts separates counted refills. A partial ACK or repeated
			// capacity wake cannot stand in for a whole refill window.
			if cut := capacityCut.Load(); cut != nil && cut.wait != lastCapacity && sourceBusy.Load() &&
				cut.acknowledged-lastRefillEncoded >= int64(permission) {
				encoded := cut.encoded
				if encoded >= cut.window && cut.window > 0 {
					lastCapacity, lastRefillEncoded = cut.wait, cut.acknowledged
					refillWindows++
					maxEncodedFlight = max(maxEncodedFlight, encoded)
					projected := ByteCount(0)
					for index := range originalCount {
						original := &originals[index]
						if !original.originalAt.IsZero() && !original.acknowledged.Load() {
							projected += original.retained
						}
					}
					maxProjectedRetained = max(maxProjectedRetained, projected)
					if cut.window != permission || encoded >= permission+maxOriginalEncoded {
						t.Fatal("post-feedback encoded flight exceeded the real ceiling plus one bounded wire pack")
					}
					if paused && refillWindows >= 10 {
						pauseArmed.Store(true)
					}
				}
			}
			if acknowledged.Load() == int64(originalCount) {
				completed = true
				break
			}
			select {
			case <-activity:
			case <-boundWake:
				boundWake = nil
				synctest.Wait()
				boundAckCount = acknowledged.Load()
				boundMiss = boundAckCount != int64(originalCount)
				if firstRefusedSlot >= 0 {
					first := &originals[firstRefusedSlot]
					firstStoredDeadlineReached = !first.resendDue.IsZero() && !time.Now().Before(first.resendDue)
				}
				t.Logf("SHARED_PUMP_SUSTAINED_FIVE_SECOND_CUT completed=%d/%d refills=%d first_stored_due_reached=%t first_actual_due_ns=%d first_actual_due_deadline_ns=%d observed_miss=%t continuing_real_source=true",
					boundAckCount, originalCount, refillWindows, firstStoredDeadlineReached, firstDueAtNanos.Load(), firstDueDeadlineNanos.Load(), boundMiss)
				logStall("five_seconds")
			case <-watchdog.C:
				watchdogExpired = true
				logStall("watchdog")
				transitions = maxOriginals * 16
			}
		}
		synctest.Wait()
		// Completion after the exact bar still fails it. Continue to actual
		// terminal ACK/custody when possible; the product lifetime is solely
		// a finite observation watchdog, never a relaxed five-second pass bar.
		if paused && !releasedAt.IsZero() && time.Since(releasedAt) > 5*time.Second {
			boundMiss = true
		}
		if !paused && time.Since(started) > 5*time.Second {
			boundMiss = true
		}
		recovery := fixture.sender.SendRecoveryStats()
		stats := fixture.receiver.ReceiveStats()
		t.Logf("SHARED_PUMP_SUSTAINED cap=%d paused=%t permission=%d initial_payload=%d initial_encoded=%d initial_retained=%d originals=%d/%d accepted=%d offered=%d refills=%d max_encoded_flight=%d max_projected_retained=%d rx_limit=%d rx_peak=%d final_refused_originals=%d handoff_drops=%d retries=%d timeout_writes=%d lane_probe_writes=%d selective_gap_writes=%d due_events=%d timer_arms=%d first_timer_arms=%d pressure_pending=%t first_distance=%d first_rx=%d first_head=%d first_root=%d completed=%d delivered=%d elapsed=%s released_at=%s source_refused=%t",
			advertisedCap, paused, permission, warm.scan.messageByteCount, warmEncoded, warm.scan.retainedByteCount,
			originalWrites, originalCount, accepted.Load(), offered.Load(), refillWindows, maxEncodedFlight,
			maxProjectedRetained, rxLimit, maxRx, refusedOriginals, stats.PackHandoffDropCount,
			physicalRetries, recovery.TimeoutResendWriteCount, recovery.LaneProbeWriteCount, recovery.SelectiveGapWriteCount,
			dueResends.Load(), timerArms.Load(), firstTimerArms, firstPressurePending, firstRefusedDistance,
			firstRefusedRx, firstRefusedHead, firstRefusedRoot, acknowledged.Load(), delivered.Load(), time.Since(started), releasedAt, sourceRefused.Load())
		t.Logf("SHARED_PUMP_SUSTAINED_WATCHDOG duration=%s expired=%t admission_refusals=%d due_predicate_failures=%d bound_miss=%t bound_completed=%d/%d",
			watchdogDuration, watchdogExpired, admissionRefusals.Load(), duePredicateFailures.Load(), boundMiss, boundAckCount, originalCount)
		t.Logf("SHARED_PUMP_SUSTAINED_CONTINUITY entered_remaining_payload=%d released_remaining_payload=%d permission=%d refills_before_release=%d refills_after_release=%d",
			enteredRemaining, releasedRemaining, permission, refillsAtRelease, refillWindows-refillsAtRelease)
		if firstRefusedSlot >= 0 {
			first := &originals[firstRefusedSlot]
			t.Logf("SHARED_PUMP_SILENT_REFUSAL number=%d encoded=%d retained=%d refused_at=%s stored_due=%s physical_retry_at=%s actual_due_hook_ns=%d terminal_ack_ns=%d pressure_same_owner=%t pressure_cost=%d pressure_number=%d",
				first.number, first.encoded, first.retained, first.refusedAt, first.resendDue, first.retryAt,
				firstDueAtNanos.Load(), first.acknowledgedAtNanos.Load(), firstPressure.messageId == first.id,
				firstPressure.queueByteCount, firstPressure.sequenceNumber)
			if len(receiveGeometry) == 0 && (!firstPressurePending || firstPressure.messageId != first.id || firstPressure.sequenceNumber != first.number ||
				firstPressure.queueByteCount != first.retained || first.resendDue.IsZero() || timerArms.Load() <= firstTimerArms) {
				t.Error("SILENT_REFUSAL_PRESSURE_TIMER_PREMISE_MISSING: actual owner pressure or subsequent timer readiness was not observed")
			}
			kind := "unobserved"
			if first.retryDueObserved {
				switch first.retryKind {
				case sendRecoveryNone:
					kind = "ordinary-timer-entry"
				case sendRecoverySelectiveGap:
					kind = "selective-gap"
				default:
					kind = "other-explicit-recovery"
				}
			}
			t.Logf("SHARED_PUMP_EXACT_REFUSED_RECOVERY physical_retry_entry_kind=%s kind_value=%d initial_due_kind=%d actual_due_ns=%d actual_due_deadline_ns=%d physical_due_ns=%d physical_due_deadline_ns=%d physical_retry_ns=%d terminal_ack_ns=%d",
				kind, first.retryKind, firstDueKind.Load(), firstDueAtNanos.Load(), firstDueDeadlineNanos.Load(), first.retryDueAt.UnixNano(), first.retryDeadline.UnixNano(), first.retryAt.UnixNano(), first.acknowledgedAtNanos.Load())
			if len(receiveGeometry) == 0 && completed && (firstDueAtNanos.Load() == 0 || firstDueDeadlineNanos.Load() == 0 ||
				firstDueAtNanos.Load() < firstDueDeadlineNanos.Load() || firstDueAtNanos.Load() < first.refusedAt.UnixNano() ||
				!first.retryDueObserved || first.retryDueAt.Before(first.retryDeadline) || first.retryDueAt.Before(first.refusedAt) ||
				first.retryAt.IsZero() || first.retryAt.Before(first.retryDueAt) ||
				first.acknowledgedAtNanos.Load() < first.retryAt.UnixNano()) {
				t.Error("SUSTAINED_EXACT_REFUSED_RECOVERY_UNQUALIFIED: actual pressure, due predicate, physical retry and ACK order was not observed")
			}
		}
		if refillWindows < 10 || paused && (!seenPause || releasedAt.IsZero()) {
			t.Error("SUSTAINED_REFILL_PREMISE_MISSING: ten actual full/refilled windows and the selected finite pause are required")
		}
		if paused && completed && refillWindows-refillsAtRelease < 10 {
			t.Error("SUSTAINED_RECOVERY_REFILLS_MISSING: fewer than ten actual full capacity refills followed callback release")
		}
		if paused && advertisedCap == 0 && (firstRefusedSlot < 0 || maxProjectedRetained <= layout.leafBytes) {
			t.Error("SUSTAINED_OVERCOMMIT_PREMISE_MISSING: the default negative geometry did not exceed physical RX and produce a final refusal")
		}
		if advertisedCap > 0 && maxProjectedRetained > rxLimit {
			t.Error("SUSTAINED_CAP_PHYSICAL_FLIGHT_EXCEEDED: actual outstanding original decode costs exceeded the configured RX leaf")
		}
		if advertisedCap > 0 || !paused {
			if refusedOriginals != 0 || stats.PackHandoffDropCount != 0 || stats.ReceiveQueueDropCount != 0 || admissionRefusals.Load() != 0 {
				t.Error("SUSTAINED_CONTROL_REFUSED: capped, physically backed or unpaused control lost actual receive admission")
			}
		}
		if !completed || sourceRefused.Load() || accepted.Load() != int64(originalCount) {
			t.Errorf("SUSTAINED_UNQUALIFIED_WATCHDOG_OR_SOURCE: ACK=%d/%d accepted=%d source_stopped=%t watchdog_expired=%t; not a qualified five-second regression result", acknowledged.Load(), originalCount, accepted.Load(), sourceRefused.Load(), watchdogExpired)
			return
		}
		if duePredicateFailures.Load() != 0 {
			t.Error("SUSTAINED_DUE_PREDICATE_PREMISE_FAILED: actual selected recovery was not yet due")
		}
		<-sourceDone
		finalOwner := ownerCut()
		if duplicatePackets.Load() != 0 || delivered.Load() != int64(8*originalCount) ||
			finalOwner.retained != 0 || finalOwner.queued != 0 || sequence.receiveQueue.Len() != 0 || sequence.packQueueCount.Load() != 0 ||
			sequence.packQueueByteCount.Load() != 0 || root.UsedByteCount() != 0 || rx.UsedByteCount() != 0 ||
			head.UsedByteCount() != 0 || pack.Stats().QuotaByteCount != 0 {
			t.Error("sustained source lost exact payload/ACK ownership or retained a final receive owner")
			return
		}
		if t.Failed() {
			t.Log("SUSTAINED_UNQUALIFIED_SETUP_OR_CUSTODY: prior premise errors cannot qualify as the desired latency RED")
			return // These setup failures cannot qualify as the desired latency RED.
		}
		if boundMiss {
			t.Error("SUSTAINED_SOURCE_EXCEEDS_LOADED_REQUEST_BOUND: genuine completed/ACKed workload exceeded 5 s from callback release, or source start for the unpaused control")
		}
		if firstRefusedSlot >= 0 {
			first := &originals[firstRefusedSlot]
			if time.Unix(0, first.acknowledgedAtNanos.Load()).Sub(releasedAt) > 5*time.Second {
				t.Error("SILENT_REFUSAL_EXCEEDS_LOADED_REQUEST_BOUND: exact refused original completed after the 5 s release bound")
			}
		}
	})
}
