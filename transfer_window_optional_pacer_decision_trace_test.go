// Bounded passive copies at the existing owners' boundaries. These roots keep
// the failing performance gates; they do not install a pacing correction.
package connect

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type windowOptionalPacerCut struct {
	Stage                      string
	At                         time.Duration
	SequenceId                 Id
	PermissionKnown            bool
	Permission                 uint64
	PermissionAt               int64
	PhysicalWrites             uint64
	PhysicalRecoveryWrites     uint64
	ApplicationPacing          bool
	TestPacingDisabled         bool
	DeliveryWindowSizing       bool
	OwnerKnown                 bool
	PacerRate, PacerEstimate   ByteCount
	PacerUpdated               int64
	Estimate                   SendWindowEstimate
	OneSecondEvidence          windowOptionalPacerEvidence
	WindowEvidence             windowOptionalPacerEvidence
	HeldPace, HeldService      ByteCount
	ServiceEpoch, HoldEpoch    int64
	HoldBytes, EpochBase       ByteCount
	QueueAt, FeedbackAt        int64
	FeedbackPending            bool
	FeedbackInterval           time.Duration
	DrainUntil, DrainObserved  int64
	Drained                    bool
	Total, Sent, Reserved      ByteCount
	DrainedSent, ProbeSent     ByteCount
	Outstanding                float64
	Reservations               int
	MaximumMessage             ByteCount
	BurstLimit, FeedbackBurst  ByteCount
	BucketInterval             time.Duration
	Compression                time.Duration
	LastRoundTrip, RecoveryAt  int64
	ReceiverHeldPrefix         int64
	Minimum, Latest, Residence time.Duration
	FeedbackPaired             bool
}

// Both reads use retain=false. Their endpoints describe the copied state at
// this hook, not an invented record of the preceding admission calculation.
type windowOptionalPacerEvidence struct {
	Horizon                       time.Duration
	Rate, Total, Latest, Support  ByteCount
	ProofAt                       int64
	FlightBoundAtRate, AtHeldPace float64
}

type windowOptionalPacerWire struct {
	Stage             string
	At                time.Duration
	SequenceId        Id
	Number            uint64
	MessageId         Id
	OriginalKnown     bool
	EncodedBytes      int
	PacingBytes       ByteCount
	OfferedAt         int64
	PhysicalWrites    uint64
	ServiceHeadKnown  bool
	ServiceHeadNumber uint64
}

// These are value-only production summaries, not borrowed queue items or
// reconstructed individual ACK credit. A merged credit may lack exact offers.
type windowOptionalPacerProof struct {
	Set      bool
	Label    string
	Cut      windowOptionalPacerCut
	Samples  [deliveredBytesRingSize]windowServiceSample
	Cycle    windowServiceSample
	Complete windowServiceSample
	Fresh    windowServiceSample
	Probe    windowPacingRoundTripProbe
}

type windowOptionalPacerTrace struct {
	stateLock            sync.Mutex
	start                time.Time
	cell                 windowPathCell
	sequence             Id
	last                 windowOptionalPacerCut
	lastOwner            windowOptionalPacerCut
	cuts                 [4096]windowOptionalPacerCut
	cutCount             int
	wires                [16384]windowOptionalPacerWire
	wireCount            int
	unpacedWritesOmitted int
	proofs               [8]windowOptionalPacerProof
	overflow             bool
	conflict             bool
}

func windowOptionalPacerNanos(at time.Time) int64 {
	if at.IsZero() {
		return 0
	}
	return at.UnixNano()
}

// Only an actual Run-owned hook may copy the mutable pacer. ACK-publication
// hooks copy atomics and lock-owned service/queue state, never that pacer.
func (self *windowOptionalPacerTrace) capture(sequence *SendSequence, stage string, owner bool, number uint64, wire bool) {
	service := sequence.windowPacer.service
	if service == nil {
		return
	}
	now := time.Now()
	cut := windowOptionalPacerCut{Stage: stage, At: now.Sub(self.start), SequenceId: sequence.sequenceId,
		PermissionKnown: sequence.receiveWindowSet.Load(), Permission: sequence.receiveWindowByteCount.Load(),
		PermissionAt: sequence.receiveWindowSetAtNanos.Load(), PhysicalWrites: sequence.writeCount.Load(),
		PhysicalRecoveryWrites: sequence.resendWriteCount.Load(), OwnerKnown: owner}
	cut.ApplicationPacing, cut.TestPacingDisabled = sequence.sendBufferSettings.ApplicationPacing, sequence.sendBufferSettings.disableWindowPacingForTest
	cut.DeliveryWindowSizing = sequence.sendBufferSettings.WindowSizing == WindowSizingFromDelivery
	if owner {
		cut.PacerRate, cut.PacerEstimate = sequence.windowPacer.rate, sequence.windowPacer.estimateRate
		cut.PacerUpdated = windowOptionalPacerNanos(sequence.windowPacer.rateUpdated)
		// This is the existing non-retaining calculation. It is a separate
		// read, not the selected interval of the previous admitting decision.
		cut.Estimate = sequence.sendWindowSnapshot(now)
	}
	windowHorizon := time.Duration(0)
	if owner {
		windowHorizon = max(2*cut.Estimate.WindowRoundTrip, 4*sequence.deliveredBytesSampleIntervalAt(now))
	}
	service.stateLock.Lock()
	cut.HeldPace, cut.HeldService = service.heldPacingRate, service.serviceHoldRate
	cut.ServiceEpoch, cut.HoldEpoch = windowOptionalPacerNanos(service.serviceEpochAt), windowOptionalPacerNanos(service.serviceHoldEpochAt)
	cut.HoldBytes, cut.EpochBase = service.serviceHoldByteCount, service.serviceEpochByteBase
	cut.QueueAt, cut.FeedbackAt = windowOptionalPacerNanos(service.queueObservedAt), windowOptionalPacerNanos(service.feedbackAt)
	cut.FeedbackPending, cut.FeedbackInterval = service.feedbackPending, service.feedbackInterval
	cut.DrainUntil, cut.DrainObserved = windowOptionalPacerNanos(service.drainUntil), windowOptionalPacerNanos(service.drainObservedAt)
	cut.Drained, cut.Total, cut.Sent = service.drained, service.total, service.sent
	cut.Reserved, cut.Reservations = service.reservedByteCount, service.pacingReservations
	cut.DrainedSent, cut.ProbeSent = service.drainedSent, service.probeSent
	cut.Outstanding, cut.MaximumMessage = service.outstandingWithLock(), service.maxMessageByteCount
	cut.BurstLimit, cut.FeedbackBurst = service.burstMeter.limit, service.feedbackBurstByteCount
	cut.BucketInterval, cut.Compression = service.bucketInterval, service.compression
	cut.LastRoundTrip, cut.RecoveryAt = windowOptionalPacerNanos(service.lastRoundTrip), windowOptionalPacerNanos(service.lastRecoveryWriteAt)
	cut.ReceiverHeldPrefix = service.receiverHeldPrefixAtNanos
	timing := service.roundTripEvidenceWithLock(now)
	cut.Minimum, cut.Latest, cut.Residence, cut.FeedbackPaired = timing.minimum, timing.latest, timing.residence, timing.feedbackPaired
	readEvidence := func(horizon time.Duration) windowOptionalPacerEvidence {
		rate, total, latest, support, proofAt := service.measureEvidenceWithLock(horizon, now, false)
		selected := rate
		if selected == 0 {
			selected = latest
		}
		return windowOptionalPacerEvidence{Horizon: horizon, Rate: rate, Total: total, Latest: latest,
			Support: support, ProofAt: proofAt,
			FlightBoundAtRate: service.flightBoundAtWithLock(selected, timing.residence),
			AtHeldPace:        service.flightBoundAtWithLock(max(selected, service.heldPacingRate), timing.residence)}
	}
	cut.OneSecondEvidence = readEvidence(time.Second)
	if windowHorizon > 0 {
		cut.WindowEvidence = readEvidence(windowHorizon)
	}
	proof := windowOptionalPacerProof{Set: true, Cut: cut, Samples: service.samples,
		Cycle: service.feedbackCycle, Complete: service.feedbackComplete, Fresh: service.feedbackFresh, Probe: service.roundTripProbe}
	service.stateLock.Unlock()
	var write windowOptionalPacerWire
	if wire {
		write = windowOptionalPacerWire{Stage: stage, At: cut.At, SequenceId: cut.SequenceId, Number: number, PhysicalWrites: cut.PhysicalWrites}
		sequence.resendQueue.stateLock.Lock()
		write.ServiceHeadKnown, write.ServiceHeadNumber = sequence.serviceAckHeadSet, sequence.serviceAckHeadNumber
		if item := sequence.resendQueue.sequenceNumberItems[number]; item != nil {
			write.MessageId, write.OriginalKnown = item.messageId, true
			write.EncodedBytes, write.PacingBytes, write.OfferedAt = len(item.transferFrameBytes), item.pacingByteCount, item.pacingSentAtNanos
		}
		sequence.resendQueue.stateLock.Unlock()
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.sequence == (Id{}) {
		self.sequence = cut.SequenceId
	} else if self.sequence != cut.SequenceId {
		// These selected cells have one real transfer sequence (flows are
		// scheduler keys). Never merge another owner's rates into its trace.
		self.conflict = true
		return
	}
	before := self.last
	fall := before.HeldPace > 0 && cut.HeldPace < before.HeldPace
	serviceFall := before.HeldService > 0 && cut.HeldService < before.HeldService
	keep := self.cutCount == 0 || stage == "ack_published" ||
		cut.HeldPace != before.HeldPace || cut.HeldService != before.HeldService ||
		cut.Permission != before.Permission || cut.PermissionAt != before.PermissionAt ||
		cut.QueueAt != before.QueueAt || cut.ServiceEpoch != before.ServiceEpoch ||
		cut.FeedbackPending != before.FeedbackPending || cut.DrainUntil != before.DrainUntil ||
		owner && (cut.PacerRate != self.lastOwner.PacerRate || cut.Estimate.ServiceBacklogged != self.lastOwner.Estimate.ServiceBacklogged ||
			cut.Permission != self.lastOwner.Permission || cut.PermissionAt != self.lastOwner.PermissionAt ||
			cut.Estimate.Window != self.lastOwner.Estimate.Window || cut.Estimate.LearnedWindow != self.lastOwner.Estimate.LearnedWindow)
	if keep {
		if self.cutCount == len(self.cuts) {
			self.overflow = true
		} else {
			self.cuts[self.cutCount] = cut
			self.cutCount++
		}
	}
	if wire {
		// The off control has no paced offer envelope to reconstruct. Keep
		// its first original samples and all ACK heads, plus all decision cuts.
		if owner && !cut.ApplicationPacing && cut.PhysicalWrites > 128 {
			self.unpacedWritesOmitted++
		} else if self.wireCount == len(self.wires) {
			self.overflow = true
		} else {
			self.wires[self.wireCount] = write
			self.wireCount++
		}
	}
	for i, selected := range []bool{
		!self.proofs[0].Set,
		fall && cut.HeldPace == 0,
		fall && cut.HeldPace > 0,
		serviceFall,
		before.HeldPace >= self.cell.Rate && cut.HeldPace < self.cell.Rate,
		(fall || serviceFall) && self.cell.ReceiveWindow > 0 && cut.Permission > uint64(self.cell.ReceiveWindow),
		(fall || serviceFall) && cut.At >= self.cell.Warmup,
		true,
	} {
		if selected && (!self.proofs[i].Set || i == len(self.proofs)-1) {
			proof.Label = []string{"first", "first_held_clear", "first_positive_held_fall", "first_service_fall", "first_below_physical_rate", "first_post_permission_fall", "first_measured_fall", "last"}[i]
			self.proofs[i] = proof
		}
	}
	self.last = cut
	if owner {
		self.lastOwner = cut
	}
}

// Installation precedes all offered data. Existing cold-witness hooks retain
// their original order and assertions; this observer follows them and never waits.
func (self *windowOptionalPacerTrace) install(cell windowPathCell, sender, _ *Client) {
	if cell.Arm != "delivery" && cell.Arm != "window-only" {
		return
	}
	self.start, self.cell = time.Now(), cell
	buffer := sender.sendBuffer
	lookup := func(id sendSequenceId) *SendSequence {
		buffer.mutex.Lock()
		defer buffer.mutex.Unlock()
		return buffer.sendSequences[id]
	}
	previousRun := buffer.beforeRunSendSequenceForTest
	buffer.beforeRunSendSequenceForTest = func(id sendSequenceId) {
		if previousRun != nil {
			previousRun(id)
		}
		if sequence := lookup(id); sequence != nil {
			previous := sequence.windowPacer.afterAdmissionForTest
			sequence.windowPacer.afterAdmissionForTest = func() {
				if previous != nil {
					previous()
				}
				self.capture(sequence, "pacing_admitted", true, 0, false)
			}
		}
	}
	previousWrite := buffer.afterInitialWriteQueuedForTest
	buffer.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
		if previousWrite != nil {
			previousWrite(id, number)
		}
		if sequence := lookup(id); sequence != nil {
			self.capture(sequence, "write_queued", true, number, true)
		}
	}
	previousAck := buffer.afterApplyAckSnapshotForTest
	buffer.afterApplyAckSnapshotForTest = func(id sendSequenceId) {
		if previousAck != nil {
			previousAck(id)
		}
		if sequence := lookup(id); sequence != nil {
			self.capture(sequence, "ack_applied", true, 0, false)
		}
	}
	previousPublish := buffer.afterAckCoalescedForTest
	buffer.afterAckCoalescedForTest = func(id sendSequenceId, number uint64) {
		if previousPublish != nil {
			previousPublish(id, number)
		}
		if sequence := lookup(id); sequence != nil {
			self.capture(sequence, "ack_published", false, number, true)
		}
	}
	previousCapacity := buffer.beforeResendCapacityWaitForTest
	buffer.beforeResendCapacityWaitForTest = func(id sendSequenceId) {
		if previousCapacity != nil {
			previousCapacity(id)
		}
		if sequence := lookup(id); sequence != nil {
			self.capture(sequence, "capacity_wait", true, 0, false)
		}
	}
}

// Called only after the real fixture has joined every worker. No formatting,
// logging, new goroutine, timer, or polling runs on the sampled packet path.
func (self *windowOptionalPacerTrace) report(t *testing.T) {
	t.Helper()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	t.Logf("optional-pacer-trace counters: cuts=%d/%d originals_and_ack_heads=%d/%d intentionally_omitted_unpaced_writes=%d overflow=%t owner_conflict=%t", self.cutCount, len(self.cuts), self.wireCount, len(self.wires), self.unpacedWritesOmitted, self.overflow, self.conflict)
	for _, cut := range self.cuts[:self.cutCount] {
		encoded, err := json.Marshal(cut)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("optional-pacer-cut %s", encoded)
	}
	for _, wire := range self.wires[:self.wireCount] {
		encoded, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("optional-pacer-original-or-ack %s", encoded)
	}
	for _, proof := range self.proofs {
		if !proof.Set {
			continue
		}
		t.Logf("optional-pacer-proof %s cut=%+v cycle=%+v complete=%+v fresh=%+v probe=%+v", proof.Label, proof.Cut, proof.Cycle, proof.Complete, proof.Fresh, proof.Probe)
		for i, sample := range proof.Samples {
			if sample.bytes > 0 {
				t.Logf("optional-pacer-sample label=%s slot=%d %s", proof.Label, i, fmt.Sprintf("%+v", sample))
			}
		}
	}
	if self.overflow || self.conflict || self.cutCount == 0 || self.wireCount == 0 {
		t.Error("bounded optional-pacer diagnostic lacks complete single-owner records")
	}
}

func TestWindowPacingColdPermissionDecisionTrace(t *testing.T) {
	trace := &windowOptionalPacerTrace{}
	defer trace.report(t)
	checkWindowPermissionCapacity(t, mib(2), true, trace.install)
}

func TestWindowPacingUnchangedPermissionDecisionTrace(t *testing.T) {
	trace := &windowOptionalPacerTrace{}
	defer trace.report(t)
	checkWindowPermissionCapacity(t, kib(64), false, trace.install)
}

// The same permission increase with the shipping switch false still exercises
// delivery-sized window admission. The independent test bypass remains false.
func TestWindowPacingDisabledPermissionDecisionTrace(t *testing.T) {
	assertMessagePoolOwnership(t)
	trace := &windowOptionalPacerTrace{}
	defer trace.report(t)
	cell := windowPathCell{SendWindow: mib(48), ReceiveWindow: kib(64), ReceiveWindowAfter: mib(2),
		WindowChangeAfter: 2 * time.Second, Warmup: 4 * time.Second,
		RoundTrip: 100 * time.Millisecond, Compression: 50 * time.Millisecond,
		Flows: 8, Rate: 12500000, Budget: mib(48), Payload: 1280}
	var readings [2]windowPathReading
	for i, arm := range []string{"ceiling", "window-only"} {
		trial := cell
		trial.Arm, trial.Drop = arm, arm == "window-only"
		if arm == "ceiling" {
			trial.CalibrationWindow, trial.ReceiveWindow, trial.ReceiveWindowAfter = mib(2), mib(2), 0
		}
		synctest.Test(t, func(t *testing.T) {
			readings[i] = measureWindowPathCell(t, trial, time.Second, trace.install)
			logWindowServiceReading(t, readings[i])
		})
	}
	ceiling, fixed := readings[0], readings[1]
	windowRate := float64(min(cell.SendWindow, cell.ReceiveWindowAfter)) / (cell.RoundTrip + cell.Compression).Seconds()
	if ceiling.Mbps < .85*min(float64(cell.Rate), windowRate)*8/1e6 || fixed.Mbps < .90*ceiling.Mbps ||
		fixed.MinFlowMbps == 0 || fixed.Window.Window > mib(2) || fixed.MeasurementRelayDrops != 0 ||
		fixed.Receiver.ReceiveQueueDropCount != 0 || fixed.Receiver.ReceiveQueueEvictionCount != 0 {
		t.Error("mismatched windows lost attainable capacity, violated a limit, dropped data or stalled a flow")
	}
	trace.stateLock.Lock()
	defer trace.stateLock.Unlock()
	before, after := false, false
	for _, cut := range trace.cuts[:trace.cutCount] {
		if cut.ApplicationPacing || cut.TestPacingDisabled || !cut.DeliveryWindowSizing ||
			cut.Sent != 0 || cut.Total != 0 || cut.HeldService != 0 || cut.OwnerKnown && cut.PacerRate != 0 {
			t.Error("pacing-off control acquired paced service credit or changed the delivery-window policy")
			break
		}
		if cut.OwnerKnown && cut.PermissionKnown {
			before = before || cut.Permission == uint64(kib(64)) && cut.Estimate.Window == kib(64)
			after = after || cut.Permission == uint64(mib(2)) && cut.Estimate.Window == mib(2)
		}
	}
	if !before || !after {
		t.Error("pacing-off control did not witness the actual small and grown admission windows")
	}
}

// Select the actual existing cell rather than re-deriving its warmup or bounds.
// Each arm retains its own virtual clock, factories, payload and calibration.
func checkWindowOptionalPacerCompressedTrace(t *testing.T, flows int) {
	t.Helper()
	assertMessagePoolOwnership(t)
	var cell windowPathCell
	found := false
	for _, candidate := range windowServicePerformanceCells() {
		if candidate.Rate == 125000 && candidate.RoundTrip == 400*time.Millisecond && candidate.Compression == 50*time.Millisecond && candidate.Flows == flows {
			cell, found = candidate, true
			break
		}
	}
	if !found {
		t.Fatal("original compressed service cell is absent")
	}
	trace := &windowOptionalPacerTrace{}
	defer trace.report(t)
	var readings [2]windowPathReading
	for i, arm := range []string{"ceiling", "delivery"} {
		trial := cell
		trial.Arm, trial.Drop = arm, arm == "delivery"
		synctest.Test(t, func(t *testing.T) {
			readings[i] = measureWindowPathCell(t, trial, max(time.Second, 2*cell.RoundTrip+4*cell.Compression), trace.install)
			logWindowServiceReading(t, readings[i])
		})
	}
	ceiling, fixed := readings[0], readings[1]
	if ceiling.Mbps < .90*float64(cell.Rate)*8/1e6 || fixed.Mbps < .90*ceiling.Mbps || fixed.MinFlowMbps == 0 || fixed.MeasurementRelayDrops != 0 || fixed.MaxRelayQueued > 4096 || fixed.MaxRelayQueuedBytes > int64(mib(8)) {
		t.Errorf("service pacing underfilled, stalled or exceeded a finite queue: %+v", fixed)
	}
}

func TestWindowPacingCompressedSingleFlowDecisionTrace(t *testing.T) {
	checkWindowOptionalPacerCompressedTrace(t, 1)
}

func TestWindowPacingCompressedEightFlowDecisionTrace(t *testing.T) {
	checkWindowOptionalPacerCompressedTrace(t, 8)
}
