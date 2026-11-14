// Real signed sparse H1 originals exercise explicitly configured finite
// count growth with exact physical ownership and shared quota admission.
// These component regressions make no throughput or SDK policy claim.
package connect

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/proto"
)

// One declared memory profile fixes the signed wire and finite count geometry.
type smallGroupCountProfile struct {
	label                                       string
	encoded, physical, root, tx, handoffEncoded ByteCount
	count                                       int
}

// The recorded 32 MiB configuration supplies every independent budget.
func smallGroupCount32() smallGroupCountProfile {
	return smallGroupCountProfile{label: "32", encoded: 2181038, physical: 7633633, root: 21810380, tx: 5752188, handoffEncoded: 174762, count: 256}
}

// The recorded 64 MiB configuration keeps the same bounded growth shape.
func smallGroupCount64() smallGroupCountProfile {
	return smallGroupCountProfile{label: "64", encoded: 4362076, physical: 15267266, root: 43620760, tx: 11504376, handoffEncoded: 262144, count: 512}
}

// Scalar receipts survive the joined fixture without retaining packet owners.
type smallGroupCountResult struct {
	refused                           bool
	queued                            int64
	encodedFlight, queuedEncoded      ByteCount
	incomingEncoded, incomingPhysical ByteCount
	rxAvailable, rootAvailable        ByteCount
	quotaAvailable                    ByteCount
	recovery                          time.Duration
	ownerProofs                       int64
	growthCount, saturationCount      uint64
	finalLimit                        int64
	finalByteLimit                    ByteCount
	channelCapacity                   int
	ordinaryRefusal                   bool
}

// Actual public wire ownership distinguishes optional count growth from byte permission.
func testSharedPumpSmallGroupAdaptiveCount(t *testing.T, profile smallGroupCountProfile, paused bool, receive func(*Client, func(Peer, [][]byte, bool))) smallGroupCountResult {
	t.Helper()
	channelCapacity := profile.count + 1
	GetMessagePoolAggregateStats()
	var result smallGroupCountResult
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		root := NewTransferMemoryBudget(profile.root)
		rx := NewTransferMemoryBudgetWithParent(profile.physical, root)
		head := NewTransferMemoryBudgetWithParent(profile.encoded, root)
		pack := NewTransferMemoryBudgetWithParent(profile.encoded, root)
		tx := NewTransferMemoryBudget(profile.tx)
		// Registered before the fixture so its Clients and all captured wires
		// join/return first. The typed behavior assertion runs outside this bubble.
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{tx, pack, head, rx, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("small-group source retained quota after Clients and wire owners joined")
				}
			}
			t.Log("SMALL_H1_COUNT_CLEANUP_JOINED")

			t.Log("SMALL_H1_ADAPTIVE_COUNT_CLEANUP_JOINED")

		})

		var target atomic.Pointer[Id]
		var targetEnds, targetCumulative, targetSelective atomic.Int64
		var targetAccepted, targetOrdinaryRefusal atomic.Bool
		var paidOwnerProofs atomic.Int64
		progress := func(event TransferProgressEvent) {
			if id := target.Load(); id != nil && event.MessageId == *id {
				switch event.Stage {
				case "receive_pack_end":
					targetAccepted.Store(event.Success)
					targetOrdinaryRefusal.Store(!event.Success && event.ErrorKind == "refused")
					targetEnds.Add(1)
				case "receive_ack_begin":
					if event.Selective {
						targetSelective.Add(1)
					} else {
						targetCumulative.Add(1)
					}
				}
			}
		}
		layout := consumerPaidPackByteLayout{label: profile.label, byteLimit: profile.handoffEncoded,
			leafBytes: profile.encoded, rootBytes: profile.root, countLimit: profile.count}
		fixture, log, _, _ := newConsumerPaidPackByteFixture(t, layout, rx, head, pack, progress,
			func(receiver bool, settings *ClientSettings) {
				settings.ContractManagerSettings.InitialContractTransferByteCount = 256 * 1024 * 1024
				settings.ContractManagerSettings.InitialNetworkPeerContractTransferByteCount = 256 * 1024 * 1024
				settings.ContractManagerSettings.StandardContractTransferByteCount = 256 * 1024 * 1024
				if receiver {
					receive := settings.ReceiveBufferSettings

					// All policy is installed before NewClient; one finite count step,
					// with no local-byte growth and no change to paid authority.
					receive.H1SequenceBufferAdaptiveMaxSize = channelCapacity
					receive.H1SequenceBufferAdaptiveStepSize = 1
					receive.H1SequenceBufferAdaptiveSaturationThreshold = 1
					receive.H1SequenceBufferAdaptiveSaturationWindow = time.Hour
					receive.H1SequenceBufferAdaptiveMaxByteCount = 0
					receive.H1SequenceBufferAdaptiveStepByteCount = 0
					receive.receiveOwnerStageForTest = func(_ *ReceiveSequence, owner *decodedPackOwner, stage string) {
						if owner == nil || (stage != "handoff_refused" && stage != "pack_released") {
							return
						}
						id := target.Load()
						if id == nil || owner.receiveItem.messageId != *id {
							return
						}
						candidate := &owner.receivePack
						charge := max(ByteCount(len(candidate.TransferFrameBytes)), candidate.sequenceQueueByteCount)
						if !owner.inUse || !candidate.receiveMemoryOwned || candidate.Pack != &owner.pack ||
							candidate.TransportType != TransportTypeH1 || owner.receiveItem.memoryBudget != rx ||
							receiveMemoryQuotaStop(pack, owner.receiveItem.memoryBudget) != root || charge <= 0 || charge > owner.receiveItem.QueueByteCount() {
							t.Error("adaptive public target lost its exact canonical paid-H1 authority")
							return
						}
						paidOwnerProofs.Add(1)
					}

					receive.AdvertiseReceiveWindow = true
					receive.AdvertisedReceiveWindowBudgetScaleNumerator = 2
					receive.AdvertisedReceiveWindowBudgetScaleDenominator = 7
				} else {
					send := settings.SendBufferSettings
					send.WindowSizing = WindowSizingFromDelivery
					send.ApplyWindowSizing()
					send.ResendQueueBudget = tx
					send.ResendQueueMaxByteCount = profile.encoded
				}
			})
		entered, release := make(chan struct{}), make(chan struct{})
		var enteredOnce, releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(unblock)
		var armed atomic.Bool
		var stateLock sync.Mutex
		packetCounts := make(map[uint32]int, profile.count+2)
		receive(fixture.receiver, func(peer Peer, packets [][]byte, valid bool) {
			if !armed.Load() {
				return
			}
			if peer.TransferKey.LogicalLane != 0 {
				t.Error("small-group callback escaped its original logical lane")
				return
			}
			if paused {
				enteredOnce.Do(func() {
					close(entered)
					select {
					case <-release:
					case <-fixture.ctx.Done():
					}
				})
			}
			for _, packet := range packets {
				var path IpPath
				if !valid || len(packet) != 1280 {
					t.Error("small-group callback changed its actual raw consumer packet")
					continue
				}
				payload, err := parseIpPathWithPayloadBorrowed(packet, &path)
				if err != nil || path.Protocol != IpProtocolTcp || len(payload) != 1240 {
					t.Error("small-group callback lost its actual TCP payload")
					continue
				}
				stateLock.Lock()
				packetCounts[path.SequenceNumber]++
				stateLock.Unlock()
			}
		})
		acknowledged := make([]atomic.Bool, profile.count+3)
		completed := make([]atomic.Int64, len(acknowledged))
		started := time.Now()
		nextSlot := 0
		send := func() consumerPaidPackByteWire {
			t.Helper()
			if nextSlot >= len(acknowledged) {
				t.Fatal("small-group public source exceeded its fixed owner bound")
			}
			slot := nextSlot
			nextSlot++
			// Taking each actual wire before the next public call leaves no
			// already-ready future Pack for ordinary H1 coalescing to consume.
			return consumerPaidPackByteSend(t, fixture, 0, 1, slot, &acknowledged[slot], &completed[slot], started)
		}
		warm := send()
		fixture.forward(warm.frame, fixture.receiverIn)
		fixture.acknowledge()
		sequence := consumerForeignHeadSequence(t, fixture, warm.frame)
		sender := consumerEstablishedWireHeadSender(t, fixture, warm.frame)
		permission, known := sender.receivedWindowAdvertisement()
		if !warm.acked.Load() || !known || permission != profile.encoded || len(sender.sendItems) != 0 || root.UsedByteCount() != 0 ||
			sequence.receiveQueue.Budget() != rx || sequence.packQueueH1Limit != int64(profile.count) || cap(sequence.packs) != channelCapacity ||
			sequence.packQueueH1ByteLimit != profile.handoffEncoded || sequence.packQueueH1AdaptiveMaxLimit != int64(channelCapacity) ||
			fixture.receiver.settings.ReceiveBufferSettings.ReceiveHeadRecoveryBudget != head || fixture.receiver.settings.ReceiveBufferSettings.PackQueueBudget != pack ||
			rx.TotalByteCount() != profile.physical || head.TotalByteCount() != profile.encoded || pack.TotalByteCount() != profile.encoded {
			t.Fatal("small-group warm source lost its real feedback, qualified profile or paid owner identities")
		}
		initialStats := fixture.receiver.ReceiveStats()
		armed.Store(true)
		seed := send()
		if !bytes.Equal(seed.frame.pack.SequenceId, warm.frame.pack.SequenceId) {
			t.Fatal("small-group callback seed escaped its real warm sender generation")
		}
		encodedFlight := ByteCount(len(seed.frame.bytes))
		fixture.forward(seed.frame, fixture.receiverIn)
		if paused {
			select {
			case <-entered:
			default:
				t.Fatal("small-group callback did not reach its real ownership barrier")
			}
			if seed.acked.Load() || sequence.packQueueCount.Load() != 0 || pack.Stats().QuotaByteCount != 0 || rx.UsedByteCount() != seed.scan.retainedByteCount {
				t.Fatal("small-group callback did not retain its paid seed outside the empty handoff queue")
			}
		} else {
			fixture.acknowledge()
			if !seed.acked.Load() {
				t.Fatal("unpaused small-group seed did not complete through its actual ACK")
			}
			encodedFlight = 0
		}
		var overflow consumerPaidPackByteWire
		for index := 0; index <= profile.count; index++ {
			wire := send()
			if !bytes.Equal(wire.frame.pack.SequenceId, warm.frame.pack.SequenceId) || wire.frame.pack.Head && paused ||
				paused && (wire.frame.pack.ContractFrame != nil || wire.frame.pack.ContractId != nil || wire.frame.pack.ContractAhead) {
				t.Fatal("small-group source changed its signed generation or queued original wire class")
			}
			incoming := ByteCount(len(wire.frame.bytes))
			beforeCount, beforeBytes := sequence.packQueueCount.Load(), sequence.packQueueByteCount.Load()
			beforeRx, beforeRoot, beforeQuota := rx.UsedByteCount(), root.UsedByteCount(), pack.Stats().QuotaByteCount
			if encodedFlight+incoming >= permission || rx.Available() < wire.scan.retainedByteCount || root.Available() < wire.scan.retainedByteCount ||
				pack.receiveQuotaAvailable(root) < incoming || beforeQuota != beforeBytes || head.UsedByteCount() != 0 {
				t.Fatal("small-group count witness reached encoded/physical/root/shared-quota admission first")
			}
			if paused && beforeCount != int64(index) || !paused && (beforeCount != 0 || beforeBytes != 0) {
				t.Fatal("small-group public source did not order each actual handoff transition")
			}
			if index == profile.count {
				overflow = wire
				id := wire.scan.messageId
				target.Store(&id)
				result.queued, result.encodedFlight, result.queuedEncoded = beforeCount, encodedFlight, beforeBytes
				result.incomingEncoded, result.incomingPhysical = incoming, wire.scan.retainedByteCount
				result.rxAvailable, result.rootAvailable, result.quotaAvailable = rx.Available(), root.Available(), pack.receiveQuotaAvailable(root)
			}
			beforeDrops := fixture.receiver.ReceiveStats().PackHandoffDropCount
			fixture.forward(wire.frame, fixture.receiverIn)
			if index == profile.count {
				if targetEnds.Load() != 1 {
					t.Fatal("small-group target did not finish its actual post-decode handoff offer")
				}
				result.refused = !targetAccepted.Load()
				result.ordinaryRefusal = targetOrdinaryRefusal.Load()
			}
			refused := index == profile.count && result.refused
			if paused {
				wantCount, wantBytes, wantRx, wantDrops := beforeCount+1, beforeBytes+incoming, beforeRx+wire.scan.retainedByteCount, uint64(0)
				if refused {
					wantCount, wantBytes, wantRx, wantDrops = beforeCount, beforeBytes, beforeRx, 1
				}
				if wire.acked.Load() || sequence.packQueueCount.Load() != wantCount || sequence.packQueueByteCount.Load() != wantBytes ||
					len(sequence.packs) != int(wantCount) || rx.UsedByteCount() != wantRx || root.UsedByteCount() != beforeRoot+wantRx-beforeRx ||
					pack.Stats().QuotaByteCount != beforeQuota+wantBytes-beforeBytes || fixture.receiver.ReceiveStats().PackHandoffDropCount-beforeDrops != wantDrops {
					t.Fatal("small-group handoff lost its exact count/byte/physical/quota or unacknowledged custody")
				}
				encodedFlight += incoming
			} else {
				fixture.acknowledge()
				if !wire.acked.Load() || refused || sequence.packQueueCount.Load() != 0 || root.UsedByteCount() != 0 || pack.Stats().QuotaByteCount != 0 {
					t.Fatal("unpaused single-packet source refused, lost ACK or retained actual ownership")
				}
			}
		}
		if paused && (targetCumulative.Load() != 0 || targetSelective.Load() != 0 || overflow.acked.Load()) {
			t.Fatal("blocked or refused small-group target received an invented ACK")
		}
		releasedAt := time.Now()
		unblock()
		synctest.Wait()
		fixture.acknowledge()
		if result.refused {
			number, present := sender.resendQueue.ContainsMessageId(overflow.scan.messageId)
			if overflow.acked.Load() || !present || number != overflow.scan.sequenceNumber {
				t.Fatal("small-group refused original lost its actual sender retry owner")
			}
			retry := pressureContractNaturalRetry(t, fixture, overflow.frame.pack)
			if !bytes.Equal(retry.pack.MessageId, overflow.frame.pack.MessageId) || !bytes.Equal(retry.pack.SequenceId, overflow.frame.pack.SequenceId) ||
				retry.pack.SequenceNumber != overflow.frame.pack.SequenceNumber || len(retry.pack.Frames) != 1 || !proto.Equal(retry.pack.Frames[0], overflow.frame.pack.Frames[0]) {
				t.Fatal("small-group natural retry changed its retained original identity or payload")
			}
			fixture.forward(retry, fixture.receiverIn)
			fixture.acknowledge()
		}
		result.recovery = time.Since(releasedAt)
		for index := 0; index < nextSlot; index++ {
			if !acknowledged[index].Load() {
				t.Errorf("small-group source original %d did not receive its actual terminal ACK", index)
			}
		}
		stateLock.Lock()
		if len(packetCounts) != profile.count+2 {
			t.Error("small-group callback omitted an original packet")
		}
		for slot := 1; slot < nextSlot; slot++ {
			if packetCounts[uint32(1000+slot*8)] != 1 {
				t.Errorf("small-group callback packet %d was omitted or duplicated", slot)
			}
		}
		stateLock.Unlock()
		stats := fixture.receiver.ReceiveStats()
		result.ownerProofs = paidOwnerProofs.Load()
		result.growthCount = stats.PackHandoffDepthGrowCount - initialStats.PackHandoffDepthGrowCount
		result.saturationCount = stats.PackHandoffSaturationCount - initialStats.PackHandoffSaturationCount
		result.finalLimit, result.finalByteLimit = sequence.packQueueH1Limit, sequence.packQueueH1ByteLimit
		result.channelCapacity = cap(sequence.packs)
		if result.ownerProofs == 0 {
			t.Fatal("adaptive target never exposed its real owned decoder custody")
		}
		wantDrops := uint64(0)
		if result.refused {
			wantDrops = 1
		}
		if targetCumulative.Load() == 0 || targetSelective.Load() != 0 || len(sender.sendItems) != 0 || sequence.packQueueCount.Load() != 0 ||
			sequence.packQueueByteCount.Load() != 0 || len(sequence.packs) != 0 || sequence.receiveQueue.Len() != 0 || sequence.holdPromise.ingressHead != nil ||
			root.UsedByteCount() != 0 || pack.Stats().QuotaByteCount != 0 || stats.PackHandoffDropCount-initialStats.PackHandoffDropCount != wantDrops ||
			stats.ReceiveDecodePressureDropCount != initialStats.ReceiveDecodePressureDropCount || stats.ReceiveQueueDropCount != initialStats.ReceiveQueueDropCount ||
			stats.ReceiveQueueEvictionCount != initialStats.ReceiveQueueEvictionCount || stats.ReceiveQueueTentativeEvictionCount != initialStats.ReceiveQueueTentativeEvictionCount || len(log.messages()) != 0 {
			t.Fatal("small-group drain lost ACK, clean delivery or exact paid custody, or incurred another loss mechanism")
		}
		t.Logf("SMALL_H1_COUNT_BEHAVIOR_DRAINED profile=%s paused=%t refused=%t count=%d queued=%d encoded_flight=%d permission=%d incoming=%d physical=%d rx_available=%d root_available=%d pack_quota_available=%d all_acked=%d recovery=%s",
			profile.label, paused, result.refused, profile.count, result.queued, result.encodedFlight, permission,
			result.incomingEncoded, result.incomingPhysical, result.rxAvailable, result.rootAvailable, result.quotaAvailable, nextSlot, result.recovery)
	})
	return result
}

// Paused and healthy controls use the same declared finite adaptive policy.
func assertSmallGroupAdaptiveCountBehavior(t *testing.T, profile smallGroupCountProfile, paused bool, receive func(*Client, func(Peer, [][]byte, bool))) {
	t.Helper()
	result := testSharedPumpSmallGroupAdaptiveCount(t, profile, paused, receive)
	if t.Failed() {
		return
	}
	if result.channelCapacity != profile.count+1 || result.finalByteLimit != profile.handoffEncoded {
		t.Fatal("adaptive witness changed its finite channel ceiling or local byte policy")
	}
	if paused && result.refused {
		if !result.ordinaryRefusal || result.queued != int64(profile.count) || result.queuedEncoded <= profile.handoffEncoded ||
			result.finalLimit != int64(profile.count) || result.growthCount != 0 || result.saturationCount != 1 {
			t.Fatal("adaptive target failed outside the configured paid local-byte growth branch")
		}
		t.Logf("SMALL_H1_ADAPTIVE_COUNT_BEHAVIOR_DRAINED profile=%s paused=%t refused=true growth=%d paid_owner_proofs=%d cap=%d local_bytes=%d queued_bytes=%d", profile.label, paused,
			result.growthCount, result.ownerProofs, result.channelCapacity, result.finalByteLimit, result.queuedEncoded)
		t.Fatalf("SMALL_H1_ADAPTIVE_PAID_LOCAL_CAP_BLOCKED_GROWTH: profile=%s count=%d cap=%d queued=%d queued_bytes=%d local_cap=%d encoded_flight=%d incoming=%d physical=%d rx_free=%d root_free=%d quota_free=%d owner_proofs=%d; all originals ACKed and custody joined",
			profile.label, profile.count, result.channelCapacity, result.queued, result.queuedEncoded, profile.handoffEncoded,
			result.encodedFlight, result.incomingEncoded, result.incomingPhysical, result.rxAvailable, result.rootAvailable, result.quotaAvailable, result.ownerProofs)
	}
	wantLimit, wantGrowth, wantSaturation := int64(profile.count), uint64(0), uint64(0)
	if paused {
		wantLimit++
		wantGrowth, wantSaturation = 1, 1
	}
	if result.refused || result.finalLimit != wantLimit || result.growthCount != wantGrowth || result.saturationCount != wantSaturation || result.recovery != 0 {
		t.Fatalf("adaptive public source did not preserve its exact one-step policy: paused=%t refused=%t limit=%d growth=%d saturation=%d", paused, result.refused, result.finalLimit, result.growthCount, result.saturationCount)
	}
	t.Logf("SMALL_H1_ADAPTIVE_COUNT_BEHAVIOR_DRAINED profile=%s paused=%t refused=false growth=%d paid_owner_proofs=%d cap=%d local_bytes=%d queued_bytes=%d", profile.label, paused,
		result.growthCount, result.ownerProofs, result.channelCapacity, result.finalByteLimit, result.queuedEncoded)
}

// The external consumer owns protocol callback decoding. This test-only bridge
// keeps signed private fixture access out of the production package API.
func RunSmallGroupAdaptiveCountFixture(t *testing.T, memoryMiB int, paused bool, receive func(*Client, func(Peer, [][]byte, bool))) {
	t.Helper()
	profile := smallGroupCount32()
	switch memoryMiB {
	case 32:
	case 64:
		profile = smallGroupCount64()
	default:
		t.Fatal("unsupported small-group memory profile")
	}
	assertSmallGroupAdaptiveCountBehavior(t, profile, paused, receive)
}

// The maintained decoder guard has no running worker. Construct a fresh
// sequence with every adaptive setting assigned before its first offer.
func newSmallGroupAdaptivePaidGuard(t *testing.T, quotaSlots int) *paidPackByteGuardFixture {
	t.Helper()
	f := newPaidPackByteGuardFixture(t, paidPackByteGuardSettings{count: 2, physicalSlots: 5, quotaSlots: quotaSlots})
	settings := *f.sequence.receiveBufferSettings
	f.sequence.Close()
	settings.H1SequenceBufferAdaptiveMaxSize = 3
	settings.H1SequenceBufferAdaptiveStepSize = 1
	settings.H1SequenceBufferAdaptiveSaturationThreshold = 1
	settings.H1SequenceBufferAdaptiveSaturationWindow = time.Hour
	settings.H1SequenceBufferAdaptiveMaxByteCount = 0
	settings.H1SequenceBufferAdaptiveStepByteCount = 0
	f.buffer.receiveBufferSettings = &settings
	f.sequence = newReceiveSequence(t.Context(), &Client{log: NewNoopLogger()},
		f.path.SourceMask(), f.sequenceId, TransferKey{}, &settings)
	return f
}

// Both old and candidate must preserve the finite ceiling. The public roots
// above separately require the configured growth step to be usable.
func TestReceivePaidPackBytesAdaptiveKeepsFiniteCountCeiling(t *testing.T) {
	assertMessagePoolOwnership(t)
	f := newSmallGroupAdaptivePaidGuard(t, 5)
	for range 2 {
		if accepted, err := f.offer(f.original()); !accepted || err != nil {
			t.Fatal("finite adaptive control lost its paid prefix")
		}
	}
	third := f.original()
	acceptedThird, err := f.offer(third)
	if err != nil {
		t.Fatal(err)
	}
	wantCount := int64(2)
	if acceptedThird {
		wantCount++
	}
	if cap(f.sequence.packs) != 3 || f.sequence.packQueueH1Limit != wantCount || f.sequence.packQueueCount.Load() != wantCount {
		t.Fatal("paid adaptive control lost its configured finite count/channel ceiling")
	}
	before := f.pack.Stats()
	fourth := f.original()
	if accepted, err := f.offer(fourth); accepted || err != nil || f.pack.Stats() != before ||
		f.sequence.packQueueH1Limit != wantCount || f.sequence.packQueueCount.Load() != wantCount ||
		len(f.sequence.packs) != int(wantCount) || f.pack.receiveQuotaAvailable(f.root) < f.encoded {
		t.Fatal("paid adaptive control escaped its finite count ceiling despite free byte rights")
	}
}

// Shared quota exhaustion still refuses growth despite spare physical backing.
func TestReceivePaidPackBytesAdaptiveKeepsSharedQuotaBoundary(t *testing.T) {
	assertMessagePoolOwnership(t)
	f := newSmallGroupAdaptivePaidGuard(t, 2)
	for range 2 {
		if accepted, err := f.offer(f.original()); !accepted || err != nil {
			t.Fatal("quota adaptive control lost its paid prefix")
		}
	}
	before := f.pack.Stats()
	third := f.original()
	if accepted, err := f.offer(third); accepted || err != nil || f.pack.Stats() != before ||
		f.sequence.packQueueH1Limit != 2 || f.sequence.packQueueCount.Load() != 2 || len(f.sequence.packs) != 2 ||
		f.sequence.client.ReceiveStats().PackHandoffDepthGrowCount != 0 || f.pack.receiveQuotaAvailable(f.root) != 0 {
		t.Fatal("paid adaptive control used growth to bypass shared Pack quota")
	}
}
