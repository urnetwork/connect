// Optional send-owner samples distinguish current retained originals from
// lifetime maxima. Readers request work; only the owner inspects its items.
package connect

import (
	"sync/atomic"
	"time"
)

// One immutable owner sample. All ownership, ages and admission facts refer to
// SampledAt, never the read time. SampleAge exposes staleness without assuming a
// blocked owner still holds the same items. An unsampled row is unknown, not zero.
// PeerWindow is the latest received encoded-byte ceiling, not free receive
// memory or a retained/shared credit promise. Ack progress counts feedback
// already applied by this owner. Oldest age covers valid first-write samples;
// missing/invalid counts make incomplete timestamp coverage explicit.
type SendSequenceLiveSnapshot struct {
	SequenceId        Id
	SourceId          Id
	DestinationId     Id
	LogicalLane       uint32
	CompanionContract bool
	ForceStream       bool
	SessionRole       string
	SessionCompanion  bool
	Sampled           bool
	SampledAt         time.Time
	SampleAge         time.Duration
	MonotonicTimeBase bool
	UnavailableReason string
	Closed            bool

	// Originals remain owned after a selective ack until cumulative retirement.
	// Encoded bytes exclude retransmitted copies. Retained charge counts actual
	// item lifetime reservations; it is not a root-budget or queue-window sum.
	OriginalItemCount               int
	OriginalEncodedByteCount        ByteCount
	OriginalRetainedChargeByteCount ByteCount
	RetainedAccountingEnabled       bool
	UnacknowledgedItemCount         int
	UnacknowledgedEncodedByteCount  ByteCount
	OldestFirstWriteAge             time.Duration
	OldestFirstWriteAgeSampled      bool
	FirstWriteMissingCount          int
	FirstWriteInvalidCount          int
	PeerWindowByteCount             ByteCount
	PeerWindowAdvertised            bool

	CumulativeAckCount          uint64
	CumulativeAckItemCount      uint64
	CumulativeAckSequenceNumber uint64
	CumulativeAckAge            time.Duration
	CumulativeAckAgeSampled     bool
	SelectiveAckCount           uint64
	SelectiveAckSequenceNumber  uint64
	SelectiveAckAge             time.Duration
	SelectiveAckAgeSampled      bool
	// The actual owner's gate at sampling. "resend_capacity" includes the
	// encoded window and memory eligibility; it does not infer which caused it.
	AdmissionReason string
}

// The send worker exclusively owns progress and publication. Readers touch only
// the atomic request/latest and the bounded wake. No packet allocates a sample.
type sendLiveDiagnostics struct {
	requested atomic.Bool
	latest    atomic.Pointer[SendSequenceLiveSnapshot]
	wake      chan struct{}

	cumulativeAckCount          uint64
	cumulativeAckItemCount      uint64
	cumulativeAckSequenceNumber uint64
	cumulativeAckAtNanos        int64
	selectiveAckCount           uint64
	selectiveAckSequenceNumber  uint64
	selectiveAckAtNanos         int64
}

// Disabled sequences allocate no diagnostic state or wake channel.
func newSendLiveDiagnostics(enabled bool) *sendLiveDiagnostics {
	if !enabled {
		return nil
	}
	return &sendLiveDiagnostics{wake: make(chan struct{}, 1)}
}

// Immutable identity survives missing samples and prevents a recycled observer
// from attaching an old generation's values to a new sequence.
func (self *SendSequence) sendLiveIdentity() SendSequenceLiveSnapshot {
	var sourceId Id
	if self.client != nil {
		sourceId = self.client.ClientId()
	}
	role := "client"
	if self.encryptionRole == sequenceTlsRoleServer {
		role = "server"
	}
	return SendSequenceLiveSnapshot{
		SequenceId: self.sequenceId, SourceId: sourceId, DestinationId: self.destination,
		LogicalLane: self.logicalLane, CompanionContract: self.companionContract,
		ForceStream: self.forceStream, SessionRole: role,
		SessionCompanion: self.encryptionCompanion,
	}
}

// Nonblocking readers never inspect mutable items or acquire a receive-path
// lock. The existing owner loop fulfills one coalesced request at its next gate.
func (self *SendSequence) readSendLiveSnapshot() SendSequenceLiveSnapshot {
	result := self.sendLiveIdentity()
	diagnostics := self.liveDiagnostics
	if diagnostics == nil {
		result.UnavailableReason = "disabled"
		return result
	}
	diagnostics.requested.Store(true)
	select {
	case diagnostics.wake <- struct{}{}:
	default:
	}
	sample := diagnostics.latest.Load()
	now := time.Now()
	if sample == nil {
		result.UnavailableReason = "awaiting_owner"
		return result
	}
	if sample.SequenceId != result.SequenceId || sample.SourceId != result.SourceId || sample.DestinationId != result.DestinationId ||
		sample.LogicalLane != result.LogicalLane || sample.CompanionContract != result.CompanionContract ||
		sample.ForceStream != result.ForceStream || sample.SessionRole != result.SessionRole ||
		sample.SessionCompanion != result.SessionCompanion {
		result.UnavailableReason = "generation_mismatch"
		return result
	}
	if sample.SampledAt.IsZero() || now.Before(sample.SampledAt) {
		result.UnavailableReason = "invalid_sample_time"
		return result
	}
	result = *sample
	result.SampleAge = now.Sub(sample.SampledAt)
	return result
}

// Invalid, absent and future timestamps never become a plausible zero-age
// measurement. Both arguments use the client's compact monotonic time domain.
func sendLiveAge(nowNanos int64, atNanos int64) (time.Duration, bool) {
	if atNanos <= 0 || nowNanos < atNanos {
		return 0, false
	}
	return time.Duration(nowNanos - atNanos), true
}

// The authoritative identity list includes originals temporarily detached for a
// rewrite. It is send-owner-only; no queue scan lock or new receive lock is used.
// Work and allocation occur only for an explicit sample request or final close.
func (self *SendSequence) publishSendLiveSnapshot(reason string, now time.Time, closed bool) {
	diagnostics := self.liveDiagnostics
	if diagnostics == nil {
		return
	}
	requested := diagnostics.requested.Swap(false)
	if !requested && !closed {
		return
	}
	sample := self.sendLiveIdentity()
	sample.Sampled = true
	sample.SampledAt = now
	sample.MonotonicTimeBase = self.client != nil && !self.client.feedbackTimeBase.IsZero()
	sample.Closed = closed
	sample.AdmissionReason = reason
	if self.resendQueue != nil {
		sample.RetainedAccountingEnabled = self.resendQueue.lifetimeBudget
	}
	nowNanos := self.client.feedbackArrivalNanos(now)
	for _, item := range self.sendItems {
		if item == nil {
			continue
		}
		sample.OriginalItemCount++
		sample.OriginalEncodedByteCount += item.MessageByteCount()
		if item.memoryBudget != nil {
			sample.OriginalRetainedChargeByteCount += item.queueByteCount
		}
		if !item.selectiveAcked {
			sample.UnacknowledgedItemCount++
			sample.UnacknowledgedEncodedByteCount += item.MessageByteCount()
		}
		// sendTime is deliberately absent: a selective ack refreshes it.
		if item.pacingSentAtNanos == 0 {
			sample.FirstWriteMissingCount++
		} else if age, valid := sendLiveAge(nowNanos, item.pacingSentAtNanos); valid {
			sample.OldestFirstWriteAge = max(sample.OldestFirstWriteAge, age)
			sample.OldestFirstWriteAgeSampled = true
		} else {
			sample.FirstWriteInvalidCount++
		}
	}
	sample.PeerWindowByteCount, sample.PeerWindowAdvertised = self.receivedWindowAdvertisement()
	sample.CumulativeAckCount = diagnostics.cumulativeAckCount
	sample.CumulativeAckItemCount = diagnostics.cumulativeAckItemCount
	sample.CumulativeAckSequenceNumber = diagnostics.cumulativeAckSequenceNumber
	sample.CumulativeAckAge, sample.CumulativeAckAgeSampled = sendLiveAge(nowNanos, diagnostics.cumulativeAckAtNanos)
	sample.SelectiveAckCount = diagnostics.selectiveAckCount
	sample.SelectiveAckSequenceNumber = diagnostics.selectiveAckSequenceNumber
	sample.SelectiveAckAge, sample.SelectiveAckAgeSampled = sendLiveAge(nowNanos, diagnostics.selectiveAckAtNanos)
	diagnostics.latest.Store(&sample)
}

// Only first delivery evidence counts; duplicate selective feedback may renew
// recovery's lease but must not look like new delivery progress.
func (self *SendSequence) observeLiveSelectiveAck(item *sendItem, receivedAtNanos int64) {
	if diagnostics := self.liveDiagnostics; diagnostics != nil {
		diagnostics.selectiveAckCount++
		diagnostics.selectiveAckSequenceNumber = max(diagnostics.selectiveAckSequenceNumber, item.sequenceNumber)
		diagnostics.selectiveAckAtNanos = max(diagnostics.selectiveAckAtNanos, receivedAtNanos)
	}
}

// Counts the actual retired prefix after queue removal and ownership release.
func (self *SendSequence) observeLiveCumulativeAck(number uint64, count int, receivedAtNanos int64) {
	if diagnostics := self.liveDiagnostics; diagnostics != nil && count > 0 {
		diagnostics.cumulativeAckCount++
		diagnostics.cumulativeAckItemCount += uint64(count)
		diagnostics.cumulativeAckSequenceNumber = max(diagnostics.cumulativeAckSequenceNumber, number)
		diagnostics.cumulativeAckAtNanos = max(diagnostics.cumulativeAckAtNanos, receivedAtNanos)
	}
}
