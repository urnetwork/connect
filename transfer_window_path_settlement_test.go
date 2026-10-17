package connect

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// This policy belongs to the performance fixture, not the transport. Its
// exported values are part of the trial cache key. Capacity debt is a separate
// liveness contract and never makes an otherwise unready path ready.
type windowPathSettlementPolicy struct {
	Version           string
	ReferenceBytes    int64
	ReferenceDuration time.Duration
	CapBytes          int64
	AckLifetime       time.Duration
}

type windowPathSettlementOriginal struct {
	Sequence, Message    Id
	Number, Tag          uint64
	PhysicalAtNanos      int64
	Success, H1, Retried bool
	SendCount            int
	CoveredAtNanos       int64
}

type windowPathSettlementLane struct {
	Owner        Id
	Original     windowPathSettlementOriginal
	HeadNumber   uint64
	HeadAtNanos  int64
	LostSequence Id
	LostNumber   uint64
	LostPending  bool
}

// Each copy is made under the service lock. In particular, no call to mean,
// Estimate, or roundTripEvidence can advance a production ring while observing.
type windowPathSettlementState struct {
	At                                             time.Time
	QualityAt, RecoveryAt, DrainStartedAt, EpochAt time.Time
	DrainUntil                                     time.Time
	ProbeAt                                        time.Time
	QualityPending, DrainEpoch, ProbePending       bool
	Baseline                                       windowPathSettlementTuple
	BaselineSet                                    bool
	Samples                                        [128]windowPathSettlementTuple
	Count                                          int
	Overflow                                       bool
}

type windowPathSettlementTuple struct {
	AtNanos                    int64
	Raw, Adjusted, Compression time.Duration
}

func windowPathSettlementTupleCopy(s windowReceiverRoundTripSample) windowPathSettlementTuple {
	return windowPathSettlementTuple{AtNanos: s.atNanos, Raw: s.raw, Adjusted: s.adjusted, Compression: s.compression}
}

func windowPathCopySettlementState(service *windowPacingService, at time.Time) windowPathSettlementState {
	service.stateLock.Lock()
	defer service.stateLock.Unlock()
	s := windowPathSettlementState{At: at, QualityAt: service.qualityChangedAt,
		RecoveryAt: service.lastRecoveryWriteAt, DrainStartedAt: service.drainStartedAt,
		EpochAt: service.serviceEpochAt, DrainUntil: service.drainUntil,
		QualityPending: service.qualityRoundTripPending, DrainEpoch: service.drainServiceEpoch}
	probe := service.roundTripProbe
	if !probe.sentAt.IsZero() && (probe.resetService || probe.resetInitialService) {
		s.ProbeAt, s.ProbePending = probe.sentAt, true
	}
	if ring := service.receiverRoundTrips; ring != nil {
		s.Baseline, s.BaselineSet = windowPathSettlementTupleCopy(ring.baseline), ring.baselineSet
		s.Count, s.Overflow = min(ring.count, len(s.Samples)), ring.count > len(s.Samples)
		for i := 0; i < s.Count; i++ {
			s.Samples[i] = windowPathSettlementTupleCopy(ring.samples[(ring.tail+i)%len(ring.samples)])
		}
	}
	return s
}

type windowPathSettlementReading struct {
	NominalAt, Deadline, FenceAt, StableSince, AdmittedAt time.Time
	LastState                                             windowPathSettlementState
	Lanes                                                 [8]windowPathSettlementLane
	Resets                                                uint64
	Reason, Error                                         string
}

type windowPathSettlement struct {
	mu                                sync.Mutex
	ctx                               context.Context
	destination                       Id
	service                           *windowPacingService
	physical, compression, stableTurn time.Duration
	rawResidenceCeiling               time.Duration
	reading                           windowPathSettlementReading
	lastTupleAtNanos                  int64
	lastBadTupleAtNanos               int64
}

func newWindowPathSettlement(ctx context.Context, cell windowPathCell, start time.Time, ackLifetime time.Duration) (*windowPathSettlement, error) {
	if cell.Settlement == nil || cell.Settlement.Version != "paired-circulation-v1" || !cell.MeasurementGrid || cell.Settlement.CapBytes != int64(cell.Budget) || cell.Settlement.AckLifetime != ackLifetime || cell.Arm != "delivery" || cell.Flows != 8 || cell.Lanes != 8 ||
		cell.Tcp || cell.Upload || cell.Bidirectional || !cell.QualityChanged || cell.RoundTripAfter <= 0 || ackLifetime <= 0 {
		return nil, fmt.Errorf("unsupported settled-measurement fixture: %+v", cell)
	}
	physical := cell.RoundTripAfter
	return &windowPathSettlement{ctx: ctx, physical: physical, compression: cell.Compression,
		stableTurn:          physical + physical/4 + cell.Compression,
		rawResidenceCeiling: physical + physical/4 + cell.Compression,
		reading: windowPathSettlementReading{NominalAt: start.Add(cell.Warmup),
			Deadline: start.Add(cell.RoundTripChangeAfter + ackLifetime),
			FenceAt:  start.Add(cell.RoundTripChangeAfter), Reason: "awaiting post-quality owners and paired circulation"}}, nil
}

// These cells run in synctest. Record the real virtual sample edge and reject
// lateness instead of silently relabeling a longer interval as one second.
func windowPathAwaitGrid(t *testing.T, edge time.Time) {
	t.Helper()
	if remaining := time.Until(edge); remaining > 0 {
		time.Sleep(remaining)
	}
	if actual := time.Now(); actual != edge {
		t.Fatalf("measurement grid missed: actual=%s expected=%s lateness=%s", actual, edge, actual.Sub(edge))
	}
}

func (o *windowPathSettlement) fail(err error) {
	if err == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.reading.Error == "" {
		o.reading.Error = err.Error()
	}
}

func (o *windowPathSettlement) lookup(buffer *SendBuffer, id sendSequenceId) (*SendSequence, error) {
	if id.Destination != o.destination || id.LogicalLane < 1 || id.LogicalLane > 8 || o.ctx.Err() != nil {
		return nil, nil
	}
	sequence := buffer.lookupSendSequence(id, nil)
	if sequence == nil && o.ctx.Err() == nil {
		return nil, fmt.Errorf("live settlement owner missing: %+v", id)
	}
	return sequence, nil
}

func (o *windowPathSettlement) install(sender, receiver *Client) {
	o.destination = receiver.ClientId()
	buffer := sender.sendBuffer
	previousRun := buffer.beforeRunSendSequenceForTest
	buffer.beforeRunSendSequenceForTest = func(id sendSequenceId) {
		if previousRun != nil {
			previousRun(id)
		}
		sequence, err := o.lookup(buffer, id)
		if err != nil {
			o.fail(err)
			return
		}
		if sequence == nil {
			return
		}
		o.mu.Lock()
		lane, service := &o.reading.Lanes[id.LogicalLane-1], sequence.windowPacer.service
		invalid := service == nil || o.service != nil && o.service != service || lane.Owner != (Id{}) && lane.Owner != sequence.sequenceId
		if !invalid {
			o.service, lane.Owner = service, sequence.sequenceId
		}
		o.mu.Unlock()
		if invalid {
			o.fail(fmt.Errorf("settlement shared owner changed: lane=%d", id.LogicalLane))
			return
		}
		previousAdmission := sequence.windowPacer.afterAdmissionForTest
		sequence.windowPacer.afterAdmissionForTest = func() {
			if previousAdmission != nil {
				previousAdmission()
			}
			o.capture(time.Now())
		}
	}
	previousInitial := buffer.afterInitialWriteQueuedForTest
	buffer.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
		if previousInitial != nil {
			previousInitial(id, number)
		}
		sequence, err := o.lookup(buffer, id)
		if err != nil {
			o.fail(err)
			return
		}
		if sequence == nil {
			return
		}
		sequence.resendQueue.stateLock.Lock()
		item := sequence.resendQueue.sequenceNumberItems[number]
		var row windowPathSettlementOriginal
		if item != nil {
			row = windowPathSettlementOriginal{Sequence: sequence.sequenceId, Message: item.messageId,
				Number: number, Tag: uint64(item.sendTime.UnixMilli()), PhysicalAtNanos: item.pacingSentAtNanos,
				Success: item.transportWriteObserved, H1: item.rttH1, SendCount: item.sendCount}
		}
		sequence.resendQueue.stateLock.Unlock()
		if item == nil {
			o.fail(fmt.Errorf("settlement original missing: lane=%d number=%d", id.LogicalLane, number))
			return
		}
		o.capture(time.Now())
		o.original(id.LogicalLane, row)
	}
	previousWire := buffer.sendBufferSettings.TransferWireMessageObserver
	buffer.sendBufferSettings.TransferWireMessageObserver = func(event TransferWireMessageObservation) {
		if previousWire != nil {
			previousWire(event)
		}
		if !event.Resend {
			return
		}
		o.mu.Lock()
		defer o.mu.Unlock()
		for i := range o.reading.Lanes {
			lane := &o.reading.Lanes[i]
			if lane.Original.Message == event.MessageId {
				lane.Original = windowPathSettlementOriginal{}
			}
		}
		// An attempt is a conservative boundary, never successful-copy proof.
		o.resetWithLock(time.Now(), "retry attempt")
	}
	previousAck := buffer.afterAckCoalescedForTest
	buffer.afterAckCoalescedForTest = func(id sendSequenceId, number uint64) {
		if previousAck != nil {
			previousAck(id, number)
		}
		sequence, err := o.lookup(buffer, id)
		if err != nil {
			o.fail(err)
			return
		}
		if sequence == nil {
			return
		}
		sequence.ackWindow.ackLock.Lock()
		head, present := sequence.ackWindow.headAck, sequence.ackWindow.hasHeadAck
		sequence.ackWindow.ackLock.Unlock()
		if !present {
			return
		}
		o.capture(time.Now())
		o.cumulative(id.LogicalLane, sequence.sequenceId, head)
	}
	previousApply := buffer.afterApplyAckSnapshotForTest
	buffer.afterApplyAckSnapshotForTest = func(id sendSequenceId) {
		if previousApply != nil {
			previousApply(id)
		}
		if id.Destination == o.destination && id.LogicalLane >= 1 && id.LogicalLane <= 8 && o.ctx.Err() == nil {
			o.capture(time.Now())
		}
	}
}

func (o *windowPathSettlement) original(laneNumber uint32, row windowPathSettlementOriginal) {
	o.mu.Lock()
	defer o.mu.Unlock()
	lane := &o.reading.Lanes[laneNumber-1]
	// Latch one candidate through its return path. Replacing it on every
	// original would outrun a 600 ms ACK and never establish circulation.
	if lane.Owner == row.Sequence && lane.Original.Message == (Id{}) && row.Success && row.H1 && !row.Retried && row.SendCount == 1 && row.PhysicalAtNanos > o.reading.FenceAt.UnixNano() {
		lane.Original = row
	}
}

func (o *windowPathSettlement) cumulative(laneNumber uint32, sequence Id, head sequenceAck) {
	o.mu.Lock()
	defer o.mu.Unlock()
	lane := &o.reading.Lanes[laneNumber-1]
	if sequence != lane.Owner || head.selective {
		return
	}
	if head.receivedAtNanos > lane.HeadAtNanos {
		lane.HeadNumber, lane.HeadAtNanos = head.sequenceNumber, head.receivedAtNanos
	}
	if lane.LostPending && lane.LostSequence == sequence && head.sequenceNumber >= lane.LostNumber {
		lane.LostPending = false
	}
	row := &lane.Original
	if row.Message != (Id{}) && row.Sequence == sequence && !row.Retried && row.CoveredAtNanos == 0 &&
		head.sequenceNumber >= row.Number && head.receivedAtNanos > row.PhysicalAtNanos {
		row.CoveredAtNanos = head.receivedAtNanos
	}
}

func windowPathMeasurementReady(settlement *windowPathSettlement, at time.Time) (bool, error) {
	if settlement == nil {
		return true, nil
	}
	return settlement.admit(at)
}

func (o *windowPathSettlement) resetWithLock(fence time.Time, reason string) {
	if !fence.After(o.reading.FenceAt) {
		return
	}
	o.invalidateWithLock(fence, reason)
}

func (o *windowPathSettlement) invalidateWithLock(fence time.Time, reason string) {
	if !o.reading.AdmittedAt.IsZero() && o.reading.Error == "" {
		o.reading.Error = "new settlement boundary after measurement admission: " + reason
	}
	if fence.After(o.reading.FenceAt) {
		o.reading.FenceAt = fence
	}
	o.reading.StableSince, o.reading.Reason = time.Time{}, reason
	o.reading.Resets++
	o.lastTupleAtNanos = 0
	o.lastBadTupleAtNanos = 0
	for i := range o.reading.Lanes {
		o.reading.Lanes[i].Original = windowPathSettlementOriginal{}
	}
}

func (o *windowPathSettlement) capture(at time.Time) {
	o.mu.Lock()
	service := o.service
	o.mu.Unlock()
	if service == nil {
		return
	}
	state := windowPathCopySettlementState(service, at)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.observeWithLock(state)
}

func (o *windowPathSettlement) observeWithLock(s windowPathSettlementState) {
	// Concurrent callbacks may publish out of order after releasing the service
	// lock. Older observations cannot replace newer evidence or open readiness.
	if s.At.Before(o.reading.LastState.At) {
		return
	}
	previous := o.reading.LastState
	o.reading.LastState = s
	// Completion can publish an earlier ACK timestamp than a later retry or
	// observation. Compare boundary identity as well as its clock; in particular
	// clearing a probe is not itself proof that resumed traffic circulated.
	if !previous.At.IsZero() && (s.QualityAt != previous.QualityAt || s.RecoveryAt != previous.RecoveryAt ||
		s.DrainStartedAt != previous.DrainStartedAt || s.EpochAt != previous.EpochAt ||
		s.ProbeAt != previous.ProbeAt || s.QualityPending != previous.QualityPending ||
		s.DrainEpoch != previous.DrainEpoch || s.ProbePending != previous.ProbePending) {
		o.invalidateWithLock(s.At, "observed epoch/drain/probe transition")
	}
	for _, fence := range []time.Time{s.QualityAt, s.RecoveryAt, s.DrainStartedAt, s.EpochAt, s.ProbeAt} {
		o.resetWithLock(fence, "quality/recovery/drain/service epoch")
	}
	blocked := s.Overflow || s.QualityPending || s.DrainEpoch || s.ProbePending || s.At.Before(s.DrainUntil)
	if blocked || s.Count == 0 || !s.BaselineSet || s.Baseline.Adjusted < o.physical || s.Baseline.AtNanos > s.At.UnixNano() {
		o.reading.StableSince = time.Time{}
		o.reading.Reason = "live drain/probe, pending quality, or missing physical baseline"
		return
	}
	// Equal-clock samples are legitimate. A bad tuple at that clock must not
	// disappear merely because a good sibling was observed first. Inspect the
	// copied bounded ring, then allow a new quiet turn only strictly after the
	// latest bad tuple, independently of callback ordering.
	for i := 0; i < s.Count; i++ {
		sample := s.Samples[i]
		if sample.AtNanos <= o.reading.FenceAt.UnixNano() {
			continue
		}
		if sample.AtNanos > s.At.UnixNano() || sample.Adjusted < o.physical || sample.Adjusted > o.physical+o.physical/4 || sample.Raw < sample.Adjusted || sample.Raw > o.rawResidenceCeiling || sample.AtNanos-int64(sample.Raw) <= o.reading.FenceAt.UnixNano() {
			o.lastBadTupleAtNanos = max(o.lastBadTupleAtNanos, sample.AtNanos)
		}
	}
	if !o.reading.StableSince.IsZero() && o.reading.StableSince.UnixNano() <= o.lastBadTupleAtNanos {
		o.reading.StableSince = time.Time{}
	}
	for i := 0; i < s.Count; i++ {
		sample := s.Samples[i]
		if sample.AtNanos <= o.reading.FenceAt.UnixNano() || sample.AtNanos <= o.lastTupleAtNanos {
			continue
		}
		if o.lastTupleAtNanos > 0 && time.Duration(sample.AtNanos-o.lastTupleAtNanos) > o.stableTurn {
			o.reading.StableSince = time.Time{}
		}
		o.lastTupleAtNanos = sample.AtNanos
		if sample.AtNanos <= o.lastBadTupleAtNanos || sample.AtNanos-int64(sample.Raw) <= o.reading.FenceAt.UnixNano() {
			o.reading.StableSince = time.Time{}
			o.reading.Reason = "paired tuple is bad or its physical origin precedes the fence"
			continue
		}
		if o.reading.StableSince.IsZero() {
			o.reading.StableSince = time.Unix(0, sample.AtNanos)
		}
	}
	if o.lastTupleAtNanos == 0 || s.At.UnixNano()-o.lastTupleAtNanos > int64(o.stableTurn) {
		o.reading.StableSince = time.Time{}
	}
}

// Called only at the existing one-second fixture edges. No throughput value
// enters this decision; once it opens, later errors never restart measurement.
func (o *windowPathSettlement) admit(at time.Time) (bool, error) {
	o.capture(at)
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.reading.Error != "" {
		return false, fmt.Errorf("%s", o.reading.Error)
	}
	if !o.reading.AdmittedAt.IsZero() {
		return true, nil
	}
	if !at.Before(o.reading.Deadline) {
		return false, fmt.Errorf("settlement deadline: %s", o.reading.Reason)
	}
	if at.Before(o.reading.NominalAt) || o.reading.StableSince.IsZero() || at.Sub(o.reading.StableSince) < o.stableTurn {
		return false, nil
	}
	turnStart := o.reading.StableSince
	for _, lane := range o.reading.Lanes {
		if lane.Owner == (Id{}) || lane.LostPending || lane.Original.Sequence != lane.Owner || lane.Original.CoveredAtNanos <= o.reading.FenceAt.UnixNano() {
			return false, nil
		}
		if covered := time.Unix(0, lane.Original.CoveredAtNanos); turnStart.Before(covered) {
			turnStart = covered
		}
	}
	if at.Sub(turnStart) < o.stableTurn {
		return false, nil
	}
	if o.lastTupleAtNanos < turnStart.Add(o.stableTurn).UnixNano() {
		o.reading.Reason = "waiting for an actual paired tuple to close the full post-prefix turn"
		return false, nil
	}
	o.reading.AdmittedAt, o.reading.Reason = at, "all owners circulated after the fence; full paired residence turn"
	return true, nil
}

// The original finite FIFO calls this only for a naturally rejected wire. No
// extra queue, decoder in production, or retained payload is introduced.
func (o *windowPathSettlement) dropped(at time.Time, wire []byte) {
	var frame protocol.TransferFrame
	if err := ProtoUnmarshal(wire, &frame); err != nil {
		o.fail(fmt.Errorf("settlement dropped wire: %w", err))
		return
	}
	pack := frame.Pack
	if pack == nil && frame.Frame != nil && frame.Frame.MessageType == protocol.MessageType_TransferPack {
		pack = &protocol.Pack{}
		if err := ProtoUnmarshal(frame.Frame.MessageBytes, pack); err != nil {
			o.fail(err)
			return
		}
	}
	if pack == nil || pack.LogicalLane < 1 || pack.LogicalLane > 8 {
		return
	}
	sequence, err := IdFromBytes(pack.SequenceId)
	if err != nil {
		o.fail(err)
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	lane := &o.reading.Lanes[pack.LogicalLane-1]
	if lane.Owner != (Id{}) && lane.Owner != sequence {
		if o.reading.Error == "" {
			o.reading.Error = "relay dropped a different settlement owner"
		}
		return
	}
	if (lane.HeadAtNanos == 0 || pack.SequenceNumber > lane.HeadNumber) && (!lane.LostPending || pack.SequenceNumber > lane.LostNumber) {
		lane.LostSequence, lane.LostNumber, lane.LostPending = sequence, pack.SequenceNumber, true
	}
	o.resetWithLock(at, "natural relay loss")
}

func (o *windowPathSettlement) snapshot() windowPathSettlementReading {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.reading
}
