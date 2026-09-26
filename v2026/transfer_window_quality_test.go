// Generation roots use exact first-write, signal and ACK clocks; no scheduler
// sleep substitutes for evidence that belonged to one side of the change.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// Only fresh measured admission may lower capacity during the fixed interval.
func TestWindowQualityShrinkNeedsFreshEvidenceAndExpires(t *testing.T) {
	at := time.Unix(1700000000, 0)
	state := sendWindowSizeState{}
	if state.estimate(1000, 8000, true, true) != 8000 {
		t.Fatal("fixture failed to grow")
	}
	state.remeasure(at)
	for _, read := range []struct{ qualified, retain, fresh bool }{
		{false, true, true}, {true, false, true}, {true, true, false},
	} {
		if got := state.estimateAt(1000, 2000, read.qualified, read.retain, at.Add(time.Second), read.fresh); got != 8000 {
			t.Fatalf("unqualified/statistics/provisional read shrank capacity: %+v got=%d", read, got)
		}
	}
	if got := state.estimateAt(1000, 2000, true, true, at.Add(time.Second), true); got != 2000 {
		t.Fatalf("fresh remeasurement did not shrink: %d", got)
	}
	if got := state.estimateAt(1000, 500, true, true, at.Add(windowQualityRemeasureInterval), true); got != 2000 {
		t.Fatalf("expired remeasurement still shrank: %d", got)
	}
	if got := state.estimateAt(1000, 9000, true, true, at.Add(2*windowQualityRemeasureInterval), true); got != 9000 {
		t.Fatalf("later growth was disabled: %d", got)
	}
}

// Continuous listener noise cannot extend one generation or renew its window.
func TestWindowQualityNoisyEventsNeedQuietBeforeNewGeneration(t *testing.T) {
	sequence, at := newRetainedWindowFixture(t)
	sequence.networkQualityChanged(at)
	for i := range 20 {
		sequence.networkQualityChanged(at.Add(time.Duration(i+1) * time.Second))
	}
	if sequence.windowQualityAfterNanos != at.UnixNano() || sequence.windowSize.remeasureUntil != at.Add(windowQualityRemeasureInterval) {
		t.Fatal("noisy events renewed the measurement generation")
	}
	next := at.Add(26 * time.Second)
	sequence.networkQualityChanged(next)
	if sequence.windowQualityAfterNanos != next.UnixNano() || sequence.windowSize.remeasureUntil != next.Add(windowQualityRemeasureInterval) {
		t.Fatal("a new event after a quiet interval was lost")
	}
}

// Recovery retains a provisional RTT, but both legacy and timed old replies
// are barred from replacing the fresh generation's RTT in either direction.
func TestWindowQualityRttOldRepliesCannotSeedNewGeneration(t *testing.T) {
	at := time.Unix(1700000000, 0)
	window := NewRttWindow(nil, 8, time.Minute, 2, time.Second, time.Millisecond, 8*time.Second)
	window.observeReceiverRoundTrip(20*time.Millisecond, 10*time.Millisecond, 10000, at)
	window.networkQualityChanged(at)
	window.closeSendTime(uint64(at.Add(-time.Millisecond).UnixMilli()), at.Add(time.Second))
	window.observeReceiverRoundTrip(2*time.Second, 0, 0, at.Add(time.Second))
	if got := window.estimate(at.Add(time.Second)); got.SampleCount != 1 || got.Min != 20*time.Millisecond || window.freshQualityEstimate() {
		t.Fatalf("old ACK changed provisional RTT: %+v", got)
	}
	window.observeReceiverRoundTrip(200*time.Millisecond, 0, 0, at.Add(time.Second))
	window.observeReceiverRoundTrip(3*time.Second, 0, 0, at.Add(2*time.Second))
	window.closeSendTime(uint64(at.Add(-time.Millisecond).UnixMilli()), at.Add(2*time.Second))
	if got := window.estimate(at.Add(2 * time.Second)); got.SampleCount != 1 || got.Min != 200*time.Millisecond || !window.freshQualityEstimate() {
		t.Fatalf("old ACK contaminated new RTT: %+v", got)
	}
}

// Physical repayment and logical checkpoint eligibility are independent.
// A cumulative prefix crossing the event is conservatively absent as a rate.
func TestWindowQualityOldAndMixedServiceCreditRepaysWithoutSampling(t *testing.T) {
	at := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, at, 0)
	old := fixture.write(0, 1000, at)
	change := at.Add(time.Millisecond)
	fixture.sequence.networkQualityChanged(change)
	fresh := fixture.write(1, 1000, change.Add(time.Millisecond))
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(fresh, change.Add(10*time.Millisecond), 0))
	if fixture.service.total != 2000 || fixture.sequence.windowPacer.serviceAcked != 2000 || fixture.service.hasSamples || fixture.service.aggregate.hasSamples {
		t.Fatalf("mixed prefix seeded a rate or lost ownership: total=%d samples=%t", fixture.service.total, fixture.service.hasSamples)
	}
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(old, change.Add(20*time.Millisecond), 0))
	if fixture.service.total != 2000 || fixture.service.latestRoundTrip != 9*time.Millisecond {
		t.Fatal("old duplicate retimed the new head or duplicated delivery")
	}
	var train [2]*sendItem
	for i := range train {
		train[i] = fixture.write(uint64(i+2), 1000, change.Add(time.Duration(30+i)*time.Millisecond))
	}
	for i, item := range train {
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(item, change.Add(time.Duration(39+i*10)*time.Millisecond), 0))
	}
	if got := fixture.measured(change.Add(50 * time.Millisecond)); got != 100000 || !fixture.service.freshQualityEstimate() {
		t.Fatalf("fresh exact pair failed to replace old path: service=%d", got)
	}
}

// Capturing credit before the event must not relabel it when its worker
// publishes after newer ACKs. Service ownership still closes exactly once.
func TestWindowQualityDelayedCreditKeepsOriginalGeneration(t *testing.T) {
	sequence, items := testWindowServiceCreditSequence([]uint64{0, 1})
	at := time.Unix(1700000001, 0)
	credit := sequence.takePacingServiceCredit(items[0])
	sequence.networkQualityChanged(at)
	sequence.observePacingServiceCredit(credit, at.Add(time.Second))
	if sequence.windowPacer.service.hasSamples || sequence.windowPacer.service.total != 1000 {
		t.Fatal("late worker converted old delivery to new evidence")
	}
	sequence.windowPacer.close()
	sequence.observePacingServiceCredit(sequence.takePacingServiceCredit(items[1]), at.Add(2*time.Second))
	if sequence.windowPacer.service.sent != 1000 || sequence.windowPacer.service.total != 1000 {
		t.Fatal("closed generation recreated physical ownership")
	}
}

// An ACK captured while the writer is pending cannot cross the signal just
// because the physical confirmation arrives afterwards.
func TestWindowQualityPendingReceiverAckCannotCrossConfirmation(t *testing.T) {
	at := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, at, 10*time.Millisecond)
	item := fixture.offer(0, 1000, at)
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(item, at.Add(20*time.Millisecond), 10*time.Millisecond))
	fixture.sequence.networkQualityChanged(at.Add(30 * time.Millisecond))
	fixture.confirm(item)
	if fixture.sequence.rttWindow.Estimate().Sampled() || fixture.service.latestRoundTrip != 0 || fixture.service.hasSamples || fixture.service.total != 1000 {
		t.Fatal("post-event writer confirmation seeded old receiver timing")
	}
}

// Replacing history never replenishes the shared physical burst or startup
// probe; a queued sibling retains its FIFO ownership and serialization debt.
func TestWindowQualityPreservesPacingAndLifetimeOwnership(t *testing.T) {
	at := time.Unix(1700000000, 0)
	service := newWindowPacingService(DefaultSendBufferSettings())
	first, second := windowPacingWaiter{}, windowPacingWaiter{}
	service.reserve(at, 1000, 100000, 100000, 100000, 1000, false, &first)
	service.reserve(at, 1000, 100000, 100000, 100000, 1000, false, &second)
	service.burstMeter.wait(at, 1000)
	beforeMeter, beforeBurst, beforeNext := service.burstMeter, service.burst, service.next
	beforeProbe, beforeReserved, beforeSent := service.probeSent, service.reservedByteCount, service.sent
	oldGeneration := service.qualityGeneration()
	service.networkQualityChanged(at.Add(time.Millisecond))
	service.holdPacingForGeneration(1000000000, oldGeneration)
	service.holdPacingForGeneration(1000000000, service.qualityGeneration())
	if service.heldPacingRate != 0 || service.burstMeter != beforeMeter || service.burst != beforeBurst || service.next != beforeNext ||
		service.probeSent != beforeProbe || service.reservedByteCount != beforeReserved || service.sent != beforeSent ||
		service.waiterHead != &first || service.waiterTail != &second || service.pacingReservations != 2 {
		t.Fatal("quality event spent or erased physical ownership, or restored a provisional held pace")
	}
}

// Late old cumulative bytes cannot enter the next checkpoint even when the
// coalescer already credited them and passes no new service bytes to a worker.
func TestWindowQualityLogicalCreditRejectsOldWorkerAndMixedPrefix(t *testing.T) {
	sequence, at := newRetainedWindowFixture(t)
	sequence.networkQualityChanged(at)
	for i, sent := range []int64{at.Add(time.Millisecond).UnixNano(), at.Add(-time.Millisecond).UnixNano(), 0, at.Add(2 * time.Millisecond).UnixNano()} {
		sequence.observeAckedBytesForWrite(1000, 0, windowServiceAckCredit{}, at.Add(time.Duration(i+1)*10*time.Millisecond), sent)
	}
	if sequence.deliveredByteTotal != 2000 || sequence.deliveredBytesCount != 2 {
		t.Fatalf("old worker polluted cumulative checkpoints: bytes=%d count=%d", sequence.deliveredByteTotal, sequence.deliveredBytesCount)
	}
}

// Destination scope is independent of logical lanes and an unrelated peer.
func TestWindowQualityPeerScopeResetsSiblingServiceOnce(t *testing.T) {
	at := time.Unix(1700000000, 0)
	peer, other := NewId(), NewId()
	shared := newWindowPacingService(DefaultSendBufferSettings())
	one := &SendSequence{windowPacer: windowBurstPacer{service: shared}}
	two := &SendSequence{windowPacer: windowBurstPacer{service: shared}}
	independent := &SendSequence{windowPacer: windowBurstPacer{service: newWindowPacingService(DefaultSendBufferSettings())}}
	buffer := &SendBuffer{sendSequences: map[sendSequenceId]*SendSequence{
		{Destination: peer}: one, {Destination: peer, LogicalLane: 1}: two, {Destination: other}: independent,
	}}
	buffer.networkQualityChanged(peer, at)
	if one.windowQualityAfterNanos != at.UnixNano() || two.windowQualityAfterNanos != at.UnixNano() || independent.windowQualityAfterNanos != 0 {
		t.Fatal("peer signal missed a sibling or reset an unrelated peer")
	}
	shared.observeRoundTrip(time.Millisecond, 0, at.Add(2*time.Millisecond))
	buffer.networkQualityChanged(peer, at)
	if shared.qualityRoundTripPending || shared.latestRoundTrip != time.Millisecond {
		t.Fatal("duplicate peer signal erased already fresh sibling timing")
	}
	buffer.closed = true
	buffer.networkQualityChanged(Id{}, at.Add(time.Minute))
	if independent.windowQualityAfterNanos != 0 {
		t.Fatal("closed owner reset an estimator")
	}
}

// The production window rule shrinks from a new service/RTT pair, then
// returns to grow-only while its pacing rate keeps following slower feedback.
func TestWindowQualityLearnedWindowShrinksOnlyDuringFreshRemeasurement(t *testing.T) {
	sequence, at := newRetainedWindowFixture(t)
	at = sampleRetainedWindow(sequence, at, 128*1024)
	before := sequence.sendWindowEstimate(at)
	change := at.Add(time.Millisecond)
	sequence.networkQualityChanged(change)
	if got := sequence.sendWindowEstimate(change); got.Window != before.Window {
		t.Fatal("notification without samples shrank the learned window")
	}
	sequence.rttWindow.observeReceiverRoundTrip(20*time.Millisecond, 0, 0, change.Add(30*time.Millisecond))
	sample := func(start time.Time, bytes ByteCount) time.Time {
		for range deliveredBytesRingSize + 4 {
			start = start.Add(10 * time.Millisecond)
			sequence.observeAckedBytesForWrite(bytes, bytes, windowServiceAckCredit{}, start, start.Add(-20*time.Millisecond).UnixNano())
		}
		return start
	}
	at = sample(change.Add(30*time.Millisecond), 4096)
	if snapshot := sequence.sendWindowSnapshot(at); snapshot.Window != before.Window {
		t.Fatal("statistics committed the shrink")
	}
	after := sequence.sendWindowEstimate(at)
	if !after.Sized || after.Window != 16384 || after.Window >= before.Window || after.PacingByteRate >= before.PacingByteRate {
		t.Fatalf("fresh lower path failed to resize and repace: before=%+v after=%+v", before, after)
	}
	at = sample(change.Add(windowQualityRemeasureInterval), 2048)
	steady := sequence.sendWindowEstimate(at)
	if steady.Window != after.Window || steady.PacingByteRate >= after.PacingByteRate {
		t.Fatalf("expired shrink interval froze pacing or shrank bytes: before=%+v after=%+v", after, steady)
	}
}

// One H1 sibling's new physical timing is valid service-wide evidence. A
// lane need not invent local replies before it can resize its retained window.
func TestWindowQualityFreshSharedServiceResizesIdleSibling(t *testing.T) {
	sequence, at := newRetainedWindowFixture(t)
	at = sampleRetainedWindow(sequence, at, 128*1024)
	before := sequence.sendWindowEstimate(at)
	service := newWindowPacingService(sequence.sendBufferSettings)
	sequence.windowPacer.service = service
	sequence.contractMultiRouteWriter = &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{h1Only: true}}
	change := at.Add(time.Millisecond)
	sequence.networkQualityChanged(change)
	fixture := newWindowReceiverCreditFixture(t, service, change, 0)
	first := fixture.write(0, 10000, change.Add(time.Millisecond))
	second := fixture.write(1, 10000, change.Add(11*time.Millisecond))
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(first, change.Add(21*time.Millisecond), 0))
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(second, change.Add(31*time.Millisecond), 0))
	after := sequence.sendWindowEstimate(change.Add(32 * time.Millisecond))
	if !after.ServiceSized || after.Sized || after.Window != 40000 || after.Window >= before.Window || after.RoundTrip != 20*time.Millisecond {
		t.Fatalf("fresh service failed to resize an idle logical sibling: before=%+v after=%+v", before, after)
	}
}

// On a 300us -> 1.2s path change the old 300ms floor used to retry the
// first new-path write, making its eventual receiver RTT ambiguous forever.
func TestWindowQualityFirstNewPathReplyUsesColdRecoveryFloor(t *testing.T) {
	at := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, at, 10*time.Millisecond)
	sequence := fixture.sequence
	sequence.sendBufferSettings = DefaultSendBufferSettings()
	sequence.contractMultiRouteWriter = &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{h1Only: true}}
	sequence.rttWindow = NewRttWindow(nil, 8, time.Minute, 2, 2*time.Second, 300*time.Millisecond, 8*time.Second)
	sequence.rttWindow.observeReceiverRoundTrip(10300*time.Microsecond, 10*time.Millisecond, 10000, at)
	fixture.service.observeReceiverRoundTrip(0, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, at)
	if sequence.rttWindow.probeRtt(at) != 300*time.Millisecond || sequence.rttWindow.scaledRtt(at) != 300*time.Millisecond {
		t.Fatal("fixture did not establish the old short recovery floor")
	}
	change := at.Add(time.Millisecond)
	sequence.networkQualityChanged(change)
	item := fixture.write(0, 1000, change.Add(time.Millisecond))
	item.reliableCarrierObserved = true
	due := change.Add(301 * time.Millisecond)
	interval := sequence.sharedRawRecoveryInterval(item, due)
	if interval != 2*time.Second || sequence.rttWindow.probeRtt(due) != 2*time.Second ||
		sequence.rttWindow.scaledRtt(due) != 2*time.Second || sequence.rttWindow.deviationRtt(due) != 2*time.Second {
		t.Fatalf("old RTO invalidated fresh evidence before arrival: physical=%s probe=%s scaled=%s deviation=%s", interval,
			sequence.rttWindow.probeRtt(due), sequence.rttWindow.scaledRtt(due), sequence.rttWindow.deviationRtt(due))
	}
	// A repeated notification cannot move this item's absolute recovery edge.
	sequence.networkQualityChanged(change.Add(time.Second))
	if got := sequence.firstPhysicalRecoveryTime(item).Add(sequence.sharedRawRecoveryInterval(item, change.Add(time.Second))); got != change.Add(2001*time.Millisecond) {
		t.Fatalf("noise extended recovery from its physical anchor: %s", got)
	}
	arrive := change.Add(1201 * time.Millisecond)
	sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(item, arrive, 0))
	if fixture.service.qualityRoundTripPending || !sequence.rttWindow.freshQualityEstimate() ||
		sequence.rttWindow.scaledRtt(arrive) != 2400*time.Millisecond {
		t.Fatal("first new-path reply could not establish ordinary recovery")
	}
	// A later faster path sample restores the ordinary sampled floor.
	sequence.rttWindow.observeReceiverRoundTrip(time.Millisecond, 0, 0, arrive.Add(2*time.Millisecond))
	if sequence.rttWindow.probeRtt(arrive.Add(2*time.Millisecond)) != 300*time.Millisecond {
		t.Fatal("cold grace survived fresh RTT evidence")
	}
}

// Unknown, copied, changed and old-generation physical writes retain their
// established recovery contracts; the hint cannot suppress every timeout.
func TestWindowQualityColdRecoveryIsOnlyForNewReliableFirstWrites(t *testing.T) {
	at := time.Unix(1700000000, 0)
	sequence := &SendSequence{sendBufferSettings: DefaultSendBufferSettings(),
		windowPacer:              windowBurstPacer{service: newWindowPacingService(DefaultSendBufferSettings())},
		contractMultiRouteWriter: &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{h1Only: true}}}
	sequence.networkQualityChanged(at)
	for _, test := range []struct {
		name string
		item sendItem
	}{
		{"old", sendItem{reliableCarrierObserved: true, rttH1: true, pacingSentAtNanos: at.Add(-time.Millisecond).UnixNano()}},
		{"copied", sendItem{sendCount: 2, reliableCarrierObserved: true, rttH1: true, pacingSentAtNanos: at.Add(time.Millisecond).UnixNano()}},
		{"unknown", sendItem{rttH1: true, pacingSentAtNanos: at.Add(time.Millisecond).UnixNano()}},
		{"unreliable", sendItem{reliableCarrierObserved: true, unreliableCarrierObserved: true, rttH1: true, pacingSentAtNanos: at.Add(time.Millisecond).UnixNano()}},
		{"changed", sendItem{reliableCarrierObserved: true, carrierChanged: true, rttH1: true, pacingSentAtNanos: at.Add(time.Millisecond).UnixNano()}},
	} {
		if got := sequence.sharedRawRecoveryInterval(&test.item, at.Add(time.Second)); got != 0 {
			t.Errorf("%s physical write borrowed cold recovery: %s", test.name, got)
		}
	}
	if got := sequence.windowPacer.service.qualityRecoveryInterval(at.Add(time.Millisecond).UnixNano(), 2*time.Second, time.Second); got != time.Second {
		t.Fatalf("cold floor exceeded the configured recovery maximum: %s", got)
	}
}

// A forward wall-clock step can make an old echoed tag numerically newer
// than the monotonic quality boundary. Only its physical first-write stamp
// decides whether the sample belongs to the new generation.
func TestWindowQualityLegacyWallClockCannotRelabelOldFirstWrite(t *testing.T) {
	at := time.Unix(1700000000, 0)
	window := NewRttWindow(nil, 8, time.Minute, 2, time.Second, time.Millisecond, 8*time.Second)
	window.networkQualityChanged(at)
	wallSend := at.Add(time.Hour)
	window.closeSendTimeForWrite(uint64(wallSend.UnixMilli()), wallSend.Add(time.Millisecond), at.Add(-time.Millisecond).UnixNano())
	if window.freshQualityEstimate() || window.Estimate().Sampled() {
		t.Fatal("wall-clock tag relabeled old physical send as new evidence")
	}
	window.closeSendTimeForWrite(uint64(wallSend.UnixMilli()), wallSend.Add(time.Millisecond), at.Add(time.Millisecond).UnixNano())
	if got := window.estimate(wallSend.Add(time.Millisecond)); !window.freshQualityEstimate() || got.SampleCount != 1 || got.Min != time.Millisecond {
		t.Fatalf("fresh physical write did not establish legacy RTT: %+v", got)
	}
}

// A newly created lane must join the service's existing generation. Otherwise
// a duplicate hint could reopen shrinking using the service's older samples.
func TestWindowQualityNewSiblingCannotRenewSharedRemeasurement(t *testing.T) {
	at := time.Unix(1700000000, 0)
	service := newWindowPacingService(DefaultSendBufferSettings())
	first := &SendSequence{windowPacer: windowBurstPacer{service: service}}
	first.networkQualityChanged(at)
	service.observeReceiverRoundTrip(0, time.Millisecond, time.Millisecond, 0, at.Add(2*time.Millisecond))
	second := &SendSequence{windowPacer: windowBurstPacer{service: service}}
	second.networkQualityChanged(at.Add(4 * time.Second))
	if second.windowQualityAfterNanos != at.UnixNano() || second.windowSize.remeasureUntil != at.Add(windowQualityRemeasureInterval) || service.qualityRoundTripPending {
		t.Fatal("new sibling relabeled or reopened the existing generation")
	}
	for i := 5; i <= 20; i++ {
		first.networkQualityChanged(at.Add(time.Duration(i) * time.Second))
		second.networkQualityChanged(at.Add(time.Duration(i) * time.Second))
	}
	if service.qualityChangedAt != at || first.windowQualityAfterNanos != at.UnixNano() || second.windowQualityAfterNanos != at.UnixNano() {
		t.Fatal("shared-service listener noise reopened measurement")
	}
	change := at.Add(26 * time.Second)
	first.networkQualityChanged(change)
	second.networkQualityChanged(change)
	if first.windowQualityAfterNanos != change.UnixNano() || second.windowQualityAfterNanos != change.UnixNano() || service.qualityChangedAt != change {
		t.Fatal("quiet-interval event failed to move both siblings together")
	}
}

// The caller owns its lane's estimate, while another live lane owns the same
// physical service. Pause after computing the old candidate to force reset
// before either learned bytes or the pacing hold can be published.
func TestWindowQualitySiblingResetRejectsInProgressOldEstimate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence, at := newRetainedServiceGrowthFixture(t, 512)
		entered, release := make(chan struct{}), make(chan struct{})
		sequence.beforeWindowRetentionForTest = func() { close(entered); <-release }
		result := make(chan SendWindowEstimate, 1)
		go func() { result <- sequence.sendWindowEstimate(at) }()
		<-entered
		sequence.windowPacer.service.networkQualityChanged(at.Add(time.Nanosecond))
		close(release)
		estimate := <-result
		if estimate.CandidateWindow < 40000000 || estimate.LearnedWindow != 2*1024*1024 ||
			estimate.Window != 2*1024*1024 || sequence.windowPacer.service.heldPacingRate != 0 {
			t.Fatalf("old sibling estimate committed after shared reset: %+v", estimate)
		}
	})
}
