// A real cold H1 opening must not price receiver-window refill silence as
// serializer congestion before a later permission increase.
package connect

import (
	"sync"
	"testing"
	"time"
)

// Value copies from existing worker ACK, write and capacity-wait boundaries;
// coalesced arrivals do not identify the estimator's selected ring pair.
type windowPermissionCapacitySnapshot struct {
	sequenceId      Id
	physicalWrites  uint64
	physicalNumber  uint64
	pacerRate       ByteCount
	pacerEstimate   ByteCount
	pacerUpdated    time.Time
	heldPace        ByteCount
	burstLimit      ByteCount
	feedbackBytes   ByteCount
	lastRoundTrip   time.Time
	lastRecovery    time.Time
	heldPrefixNanos int64
	at              time.Time
	permission      ByteCount
	permissionAt    time.Time
	queueAt         time.Time
	epochAt         time.Time
	feedbackAt      time.Time
	feedbackGap     time.Duration
	feedbackPending bool
	drainUntil      time.Time
	heldRate        ByteCount
	probeSpent      ByteCount
	applied         ByteCount
	drained         ByteCount
	outstanding     ByteCount
	messageBytes    ByteCount
	timing          windowReceiverRoundTripEstimate
	estimate        SendWindowEstimate
}

// Fixed scalar records, not retained wire buffers or a new sampling worker.
type windowPermissionCapacityObserver struct {
	stateLock      sync.Mutex
	hooksOccupied  bool
	firstPhysical  time.Time
	physicalWrites uint64
	lastFeedback   time.Time
	firstQueue     windowPermissionCapacitySnapshot
	firstDrain     windowPermissionCapacitySnapshot
	beforeStep     windowPermissionCapacitySnapshot
	firstStep      windowPermissionCapacitySnapshot
	coldEligible   windowPermissionCapacitySnapshot
	coldCount      uint64
	coldCandidate  windowPermissionCapacitySnapshot
	postWrite      windowPermissionCapacitySnapshot
	retainedFall   windowPermissionCapacitySnapshot
	laterWrite     windowPermissionCapacitySnapshot
	last           windowPermissionCapacitySnapshot
	probeChanged   bool
}

// Install existing owner hooks before the fixture starts offering data.
func (self *windowPermissionCapacityObserver) install(cell windowPathCell, sender, _ *Client) {
	if cell.Arm != "delivery" {
		return
	}
	buffer := sender.sendBuffer
	if buffer.afterInitialWriteQueuedForTest != nil || buffer.afterApplyAckSnapshotForTest != nil ||
		buffer.beforeResendCapacityWaitForTest != nil {
		self.stateLock.Lock()
		self.hooksOccupied = true
		self.stateLock.Unlock()
		return
	}
	lookup := func(id sendSequenceId) *SendSequence {
		buffer.mutex.Lock()
		defer buffer.mutex.Unlock()
		return buffer.sendSequences[id]
	}
	// All callers are existing Run-owned hooks. Only shared service fields
	// need its lock; no callback reads a borrowed item or writes pacing state.
	takeSnapshot := func(sequence *SendSequence) windowPermissionCapacitySnapshot {
		now := time.Now()
		snapshot := windowPermissionCapacitySnapshot{
			at: now, sequenceId: sequence.sequenceId, physicalWrites: sequence.writeCount.Load(),
			pacerRate: sequence.windowPacer.rate, pacerEstimate: sequence.windowPacer.estimateRate,
			pacerUpdated: sequence.windowPacer.rateUpdated,
			permission: ByteCount(sequence.receiveWindowByteCount.Load()),
		}
		if at := sequence.receiveWindowSetAtNanos.Load(); at != 0 {
			snapshot.permissionAt = time.Unix(0, at)
		}
		service := sequence.windowPacer.service
		service.stateLock.Lock()
		snapshot.queueAt, snapshot.epochAt = service.queueObservedAt, service.serviceEpochAt
		snapshot.feedbackAt = service.feedbackAt
		snapshot.feedbackPending, snapshot.drainUntil = service.feedbackPending, service.drainUntil
		snapshot.heldRate, snapshot.probeSpent = service.serviceHoldRate, service.probeSent
		snapshot.heldPace = service.heldPacingRate
		snapshot.burstLimit, snapshot.feedbackBytes = service.burstMeter.limit, service.feedbackBurstByteCount
		snapshot.lastRoundTrip, snapshot.lastRecovery = service.lastRoundTrip, service.lastRecoveryWriteAt
		snapshot.heldPrefixNanos = service.receiverHeldPrefixAtNanos
		snapshot.applied, snapshot.drained = service.total, service.drainedSent
		snapshot.outstanding = ByteCount(service.outstandingWithLock())
		snapshot.messageBytes = service.maxMessageByteCount
		snapshot.timing = service.roundTripEvidenceWithLock(now)
		service.stateLock.Unlock()
		// This computes the existing policy without retaining either service
		// or pacing. It is not atomic with the preceding service/permission copy.
		snapshot.estimate = sequence.sendWindowSnapshot(now)
		return snapshot
	}
	buffer.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
		sequence := lookup(id)
		if sequence == nil {
			return
		}
		// The existing hook also runs after failure. Only a newly advanced
		// success counter can establish the next physical transition.
		writes := sequence.writeCount.Load()
		self.stateLock.Lock()
		fresh := writes > self.physicalWrites
		if fresh && self.firstPhysical.IsZero() {
			self.firstPhysical = time.Now()
		}
		self.physicalWrites = max(self.physicalWrites, writes)
		self.stateLock.Unlock()
		if !fresh || sequence.windowPacer.service == nil {
			return
		}
		snapshot := takeSnapshot(sequence)
		snapshot.physicalNumber = number
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		cold := self.coldCandidate
		if !cold.at.IsZero() && self.postWrite.at.IsZero() &&
			sameWindowPermissionFeedback(cold, snapshot) &&
			snapshot.physicalWrites > cold.physicalWrites &&
			snapshot.heldRate == cold.estimate.ServiceByteRate &&
			snapshot.pacerEstimate == cold.estimate.ServiceByteRate &&
			max(snapshot.burstLimit, snapshot.feedbackBytes) < max(cold.burstLimit, cold.feedbackBytes) &&
			snapshot.estimate.ServiceBacklogged &&
			snapshot.estimate.PacingByteRate < cold.heldPace {
			self.postWrite = snapshot
		}
		// A full small window can postpone the next actual write until a new
		// ACK. Keep that later feedback identity; never force a same-ACK send.
		if !self.retainedFall.at.IsZero() && self.laterWrite.at.IsZero() &&
			snapshot.sequenceId == cold.sequenceId && snapshot.epochAt.Equal(cold.epochAt) &&
			snapshot.permission == cold.permission && snapshot.permissionAt.Equal(cold.permissionAt) &&
			snapshot.queueAt.Equal(cold.queueAt) && snapshot.lastRecovery.IsZero() &&
			snapshot.physicalWrites > self.postWrite.physicalWrites &&
			snapshot.pacerUpdated.After(self.postWrite.pacerUpdated) &&
			snapshot.pacerRate > 0 && snapshot.pacerRate < cold.heldPace {
			self.laterWrite = snapshot
		}
	}
	buffer.beforeResendCapacityWaitForTest = func(id sendSequenceId) {
		sequence := lookup(id)
		if sequence == nil || sequence.windowPacer.service == nil {
			return
		}
		snapshot := takeSnapshot(sequence)
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.postWrite.at.IsZero() && self.retainedFall.at.IsZero() &&
			sameWindowPermissionFeedback(self.postWrite, snapshot) &&
			snapshot.physicalWrites == self.postWrite.physicalWrites &&
			snapshot.heldPace > 0 && snapshot.heldPace < self.coldCandidate.heldPace {
			self.retainedFall = snapshot
		}
	}
	buffer.afterApplyAckSnapshotForTest = func(id sendSequenceId) {
		sequence := lookup(id)
		if sequence == nil || sequence.windowPacer.service == nil {
			return
		}
		snapshot := takeSnapshot(sequence)
		self.stateLock.Lock()
		if snapshot.feedbackAt.IsZero() || !snapshot.feedbackAt.After(self.lastFeedback) {
			self.stateLock.Unlock()
			return
		}
		if !self.lastFeedback.IsZero() {
			snapshot.feedbackGap = snapshot.feedbackAt.Sub(self.lastFeedback)
		}
		self.lastFeedback = snapshot.feedbackAt
		defer self.stateLock.Unlock()
		if !snapshot.queueAt.IsZero() && self.firstQueue.at.IsZero() {
			self.firstQueue = snapshot
		}
		if snapshot.drained > 0 && snapshot.applied >= snapshot.drained && self.firstDrain.at.IsZero() {
			self.firstDrain = snapshot
		}
		if !self.firstQueue.at.IsZero() && snapshot.probeSpent != self.firstQueue.probeSpent {
			self.probeChanged = true
		}
		if snapshot.permission == cell.ReceiveWindow {
			self.beforeStep = snapshot
		} else if snapshot.permission > cell.ReceiveWindow && self.firstStep.at.IsZero() {
			self.firstStep = snapshot
		}
		// An older physical burst really queued, but this fresh first reply
		// is unloaded and the outstanding bytes still fit the small window
		// plus one indivisible Pack. Cold service must not turn refill silence
		// into its own backlogged upper bound and discard the admitted pace.
		coldEligible := snapshot.permission == cell.ReceiveWindow &&
			snapshot.heldRate == 0 && !snapshot.queueAt.IsZero() && !snapshot.epochAt.IsZero() &&
			!snapshot.feedbackPending && !snapshot.at.Before(snapshot.drainUntil) &&
			snapshot.feedbackGap > cell.Compression &&
			snapshot.timing.feedbackPaired && snapshot.timing.minimum > 0 &&
			snapshot.timing.latest <= snapshot.timing.minimum+2*time.Millisecond &&
			snapshot.outstanding <= snapshot.permission+snapshot.messageBytes
		if coldEligible {
			self.coldCount++
			if self.coldEligible.at.IsZero() {
				self.coldEligible = snapshot
			}
		}
		if coldEligible && self.coldCandidate.at.IsZero() &&
			snapshot.estimate.ServiceEstablished && snapshot.estimate.ServiceByteRate > 0 &&
			!snapshot.estimate.ServiceBacklogged && snapshot.heldPace > snapshot.estimate.ServiceByteRate &&
			snapshot.estimate.PacingByteRate >= snapshot.heldPace && snapshot.lastRecovery.IsZero() {
			self.coldCandidate = snapshot
		}
		self.last = snapshot
	}
}

// Require one owner and one unchanged accepted-feedback tuple across the
// real reservation. A one-shot historical queue latch alone is not enough.
func sameWindowPermissionFeedback(before, after windowPermissionCapacitySnapshot) bool {
	return before.sequenceId == after.sequenceId &&
		before.epochAt.Equal(after.epochAt) &&
		before.permission == after.permission && before.permissionAt.Equal(after.permissionAt) &&
		before.feedbackAt.Equal(after.feedbackAt) && before.applied == after.applied &&
		before.lastRoundTrip.Equal(after.lastRoundTrip) && before.timing == after.timing &&
		before.queueAt.Equal(after.queueAt) && before.heldPrefixNanos == after.heldPrefixNanos &&
		before.probeSpent == after.probeSpent &&
		before.lastRecovery.IsZero() && after.lastRecovery.IsZero() &&
		after.timing.feedbackPaired && after.timing.minimum > 0 &&
		after.timing.latest <= after.timing.minimum+2*time.Millisecond &&
		!after.feedbackPending && !after.at.Before(after.drainUntil)
}

// This is the unchanged 64 KiB -> 2 MiB, 100 ms RTT, 50 ms compression cell.
// Its real workers own begin/finish/ACK, its independent FIFO owns 12.5 MB/s,
// and the original matched-ceiling helper still enforces 90% capacity.
func TestWindowPacingColdLimitedPermissionGrowthKeepsCapacity(t *testing.T) {
	checkWindowPermissionCapacity(t, mib(2), true)
}

// An equal permission publication cannot renew the opening or invent a new
// capacity step. The same small-window physical ceiling remains attainable.
func TestWindowPacingUnchangedLimitedPermissionKeepsOwnership(t *testing.T) {
	checkWindowPermissionCapacity(t, kib(64), false)
}

// Keep the physical matched-ceiling oracle, then inspect joined value copies.
func checkWindowPermissionCapacity(t *testing.T, after ByteCount, growth bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	observer := &windowPermissionCapacityObserver{}
	reading := checkWindowMismatchCell(t, windowPathCell{
		SendWindow: mib(48), ReceiveWindow: kib(64), ReceiveWindowAfter: after,
		WindowChangeAfter: 2 * time.Second, Warmup: 4 * time.Second,
		RoundTrip: 100 * time.Millisecond, Compression: 50 * time.Millisecond,
		Flows: 8, Rate: 12500000,
	}, observer.install)
	// The unchanged helper has canceled and joined every worker/client and
	// checked each owned budget before any final witness is inspected.
	observer.stateLock.Lock()
	defer observer.stateLock.Unlock()
	recovery := reading.Recovery
	if recovery.TimeoutResendWriteCount+recovery.CarrierChangeWriteCount+
		recovery.SendEvictionResendCount+recovery.SelectiveGapWriteCount+
		recovery.AckTailProbeWriteCount+recovery.CumulativeProbeWriteCount+
		recovery.MissingContractWriteCount != 0 {
		t.Fatal("recovery writes contaminated the real cold opening witness")
	}
	if observer.hooksOccupied || observer.firstPhysical.IsZero() || observer.physicalWrites == 0 ||
		observer.firstQueue.at.IsZero() || observer.firstDrain.at.IsZero() ||
		observer.firstQueue.queueAt.Before(observer.firstPhysical) ||
		observer.firstDrain.drained <= 0 || observer.beforeStep.epochAt.IsZero() {
		t.Fatalf("real physical opening/queue/drain/limited epoch was not observed: hooks=%t physical=%s writes=%d queue=%s drained=%d epoch=%s",
			observer.hooksOccupied, observer.firstPhysical, observer.physicalWrites,
			observer.firstQueue.queueAt, observer.firstDrain.drained, observer.beforeStep.epochAt)
	}
	if observer.coldCount == 0 || observer.coldEligible.at.IsZero() {
		t.Fatal("real cold refill with zero held service, no pending feedback cycle and no active drain was not observed")
	}
	if observer.probeChanged || observer.last.probeSpent != observer.firstQueue.probeSpent {
		t.Error("queued permission change renewed opening credit")
	}
	if observer.beforeStep.estimate.Window > kib(64) || observer.last.estimate.Window > after {
		t.Error("observer found a sender window above real receiver permission")
	}
	if growth {
		step := observer.firstStep
		if step.at.IsZero() || step.permission != after ||
			!observer.firstQueue.queueAt.Before(step.permissionAt) ||
			!observer.beforeStep.epochAt.Before(step.permissionAt) ||
			!observer.firstDrain.at.Before(step.permissionAt) {
			t.Fatalf("growth did not follow the real queued opening and limited epoch: queue=%s drain=%s epoch=%s permission=%s",
				observer.firstQueue.queueAt, observer.firstDrain.at, observer.beforeStep.epochAt, step.permissionAt)
		}
	} else if !observer.firstStep.at.IsZero() ||
		observer.last.permissionAt != observer.firstQueue.permissionAt {
		t.Error("unchanged permission invented a growth boundary")
	}
	// The bad chain is conditional, not a required setup state for a future
	// correction. Both cases still require the independent cold precursor.
	if !observer.retainedFall.at.IsZero() {
		cold, physical, retained, later := observer.coldCandidate, observer.postWrite, observer.retainedFall, observer.laterWrite
		t.Errorf("cold refill candidate changed its own congestion allowance: service=%d feedback=%s writes=%d->%d burst=%d->%d feedback-burst=%d->%d outstanding=%d held-pace=%d->%d",
			cold.estimate.ServiceByteRate, cold.feedbackAt.Sub(observer.firstPhysical),
			cold.physicalWrites, physical.physicalWrites, cold.burstLimit, physical.burstLimit,
			cold.feedbackBytes, physical.feedbackBytes, physical.outstanding,
			cold.heldPace, retained.heldPace)
		if later.at.IsZero() {
			t.Error("retained cold pace fall lacked the later real write-rate witness")
		}
		t.Logf("synthetic real pacing consequence: reserve-number=%d reserve-at=%s reserve-rate=%d reserve-estimate=%d retained-at=%s retained-feedback=%s later-at=%s later-feedback=%s later-rate-updated=%s later-rate=%d later-estimate=%d later-network=%s/%s",
			physical.physicalNumber, physical.at.Sub(observer.firstPhysical), physical.pacerRate, physical.pacerEstimate,
			retained.at.Sub(observer.firstPhysical), retained.feedbackAt.Sub(observer.firstPhysical),
			later.at.Sub(observer.firstPhysical), later.feedbackAt.Sub(observer.firstPhysical),
			later.pacerUpdated.Sub(observer.firstPhysical), later.pacerRate, later.pacerEstimate,
			later.timing.latest, later.timing.minimum)
	}
	t.Logf("synthetic permission witness: growth=%t physical-writes=%d cold-count=%d cold-gap=%s queue-at=%s drain-at=%s limited-epoch=%s permission-step=%s opening-spent=%d",
		growth, observer.physicalWrites, observer.coldCount, observer.coldEligible.feedbackGap, observer.firstQueue.queueAt.Sub(observer.firstPhysical),
		observer.firstDrain.at.Sub(observer.firstPhysical), observer.beforeStep.epochAt.Sub(observer.firstPhysical),
		observer.firstStep.permissionAt, observer.last.probeSpent)
}
