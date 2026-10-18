// Cold, explicitly requested snapshots of existing pacing and admission state.
// No packet hook, credit mutation, payload reference, timer or worker is added.
package connect

import (
	"fmt"
	"time"
)

const transferPacingDiagnosticClientCapacity = 16
const transferPacingDiagnosticSequenceCapacity = 16

// Fixed inventory bounds apply across both windows, not independently per client.
// Rows are observations, not one atomic controller decision. Each component owns
// its clock bracket and lock; all output consists of copied scalars and strings.
type TransferPacingDiagnosticSnapshot struct {
	ClientCapacity          int                                `json:"client_capacity"`
	SequenceCapacity        int                                `json:"sequence_capacity"`
	ClientCount             int                                `json:"client_count"`
	SequenceCount           int                                `json:"sequence_count"`
	Truncated               bool                               `json:"truncated"`
	StartElapsedNanoseconds int64                              `json:"start_elapsed_ns,string"`
	EndElapsedNanoseconds   int64                              `json:"end_elapsed_ns,string"`
	Sequences               []transferPacingDiagnosticSequence `json:"sequences"`
}

// Identity is the immutable sequence generation, not an address or packet key.
type transferPacingDiagnosticSequence struct {
	Window string `json:"window"`
	// Process-local stack receiver binding, not a persistent identity.
	SequenceOwner string                            `json:"sequence_owner"`
	SequenceId    string                            `json:"sequence_id"`
	DestinationId string                            `json:"destination_id"`
	LogicalLane   uint32                            `json:"logical_lane"`
	Canceled      bool                              `json:"canceled"`
	Estimate      transferPacingDiagnosticEstimate  `json:"estimate"`
	Admission     transferPacingDiagnosticAdmission `json:"admission"`
	Service       transferPacingDiagnosticService   `json:"service"`
}

// This is the existing non-retaining estimate, not a new pacing calculation.
// It retains the aggregate route eligibility used by ordinary statistics.
type transferPacingDiagnosticEstimate struct {
	StartElapsedNanoseconds   int64         `json:"start_elapsed_ns,string"`
	EndElapsedNanoseconds     int64         `json:"end_elapsed_ns,string"`
	Window                    ByteCount     `json:"window_bytes,string"`
	CandidateWindow           ByteCount     `json:"candidate_window_bytes,string"`
	Floor                     ByteCount     `json:"floor_bytes,string"`
	Ceiling                   ByteCount     `json:"ceiling_bytes,string"`
	Obtainable                ByteCount     `json:"obtainable_bytes,string"`
	DeliveredByteCount        ByteCount     `json:"delivered_bytes,string"`
	Interval                  time.Duration `json:"interval_ns,string"`
	WindowRoundTrip           time.Duration `json:"window_round_trip_ns,string"`
	SampleCount               int           `json:"sample_count"`
	Reason                    string        `json:"reason"`
	ServiceByteRate           ByteCount     `json:"service_bytes_per_second,string"`
	AggregateDeliveryByteRate ByteCount     `json:"aggregate_delivery_bytes_per_second,string"`
	PacingByteRate            ByteCount     `json:"pacing_bytes_per_second,string"`
	PacingProbeByteRate       ByteCount     `json:"probe_bytes_per_second,string"`
	PacingHeldByteRate        ByteCount     `json:"held_bytes_per_second,string"`
	ServiceEstablished        bool          `json:"service_established"`
	ServiceBacklogged         bool          `json:"service_backlogged"`
	PacingDiscovery           bool          `json:"discovery"`
}

// Only totals and the first blocked producer's scalar constraint are copied.
// Neither the scheduling key nor any waiter/Pack reference leaves the call.
type transferPacingDiagnosticAdmission struct {
	StartElapsedNanoseconds int64 `json:"start_elapsed_ns,string"`
	EndElapsedNanoseconds   int64 `json:"end_elapsed_ns,string"`
	Present                 bool  `json:"present"`
	Closed                  bool  `json:"closed"`
	Capacity                int   `json:"capacity"`
	Count                   int   `json:"count"`
	FlowCount               int   `json:"flow_count"`
	WaiterCount             int   `json:"waiter_count"`
	OldestFlowCount         int   `json:"oldest_flow_count"`
	OldestFlowLimit         int   `json:"oldest_flow_limit"`
	ChannelCount            int   `json:"channel_count"`
	ChannelCapacity         int   `json:"channel_capacity"`
}

// The shared service lock protects these fields, including waiter membership.
// Worker-owned pacer rate, timer, current Pack and waiter.sentAt are not read.
// Meter credit is raw at meter_at_elapsed_ns; capture does not refill it.
type transferPacingDiagnosticService struct {
	StartElapsedNanoseconds          int64         `json:"start_elapsed_ns,string"`
	EndElapsedNanoseconds            int64         `json:"end_elapsed_ns,string"`
	Present                          bool          `json:"present"`
	NextElapsedNanoseconds           int64         `json:"next_elapsed_ns,string"`
	MeterInitialized                 bool          `json:"meter_initialized"`
	MeterAtElapsedNanoseconds        int64         `json:"meter_at_elapsed_ns,string"`
	MeterPaidUntilElapsedNanoseconds int64         `json:"meter_paid_until_elapsed_ns,string"`
	MeterRate                        ByteCount     `json:"meter_bytes_per_second,string"`
	MeterLimit                       ByteCount     `json:"meter_limit_bytes,string"`
	MeterReducedLimit                ByteCount     `json:"meter_reduced_limit_bytes,string"`
	MeterAvailable                   float64       `json:"meter_available_bytes"`
	MeterSpent                       float64       `json:"meter_spent_bytes"`
	EstimateInterval                 time.Duration `json:"estimate_interval_ns,string"`
	ProbeSent                        ByteCount     `json:"probe_sent_bytes,string"`
	HeldPacingRate                   ByteCount     `json:"held_bytes_per_second,string"`
	QueueObservedElapsedNanoseconds  int64         `json:"queue_observed_elapsed_ns,string"`
	Reservations                     int           `json:"reservations"`
	ReservedByteCount                ByteCount     `json:"reserved_bytes,string"`
	Sent                             ByteCount     `json:"sent_bytes,string"`
	Delivered                        ByteCount     `json:"delivered_bytes,string"`
	PendingWrites                    int           `json:"pending_writes"`
	DrainUntilElapsedNanoseconds     int64         `json:"drain_until_elapsed_ns,string"`
	WaiterQueued                     bool          `json:"waiter_queued"`
	WaiterHead                       bool          `json:"waiter_head"`
	WaiterReadyCount                 int           `json:"waiter_ready_count"`
	WaiterDeadlineElapsedNanoseconds int64         `json:"waiter_deadline_elapsed_ns,string"`
	WaiterSerialization              time.Duration `json:"waiter_serialization_ns,string"`
}

// Explicit diagnostic only. Windows and clients keep their normal lifetimes;
// bounded ordinary object pointers are held only for this synchronous call.
// No owner lock is held across an external snapshot. The read-only estimate
// retains its existing internal lock discipline and never learns a new rate.
func (self *RemoteUserNatMultiClient) PacingDiagnosticSnapshot(origin time.Time) TransferPacingDiagnosticSnapshot {
	return self.pacingDiagnosticSnapshot(origin, time.Now)
}

// The injected clock is private to deterministic observation tests.
func (self *RemoteUserNatMultiClient) pacingDiagnosticSnapshot(origin time.Time, now func() time.Time) TransferPacingDiagnosticSnapshot {
	result := TransferPacingDiagnosticSnapshot{
		ClientCapacity:          transferPacingDiagnosticClientCapacity,
		SequenceCapacity:        transferPacingDiagnosticSequenceCapacity,
		StartElapsedNanoseconds: now().Sub(origin).Nanoseconds(),
		Sequences:               make([]transferPacingDiagnosticSequence, 0, transferPacingDiagnosticSequenceCapacity),
	}
	if self != nil {
		var clients [transferPacingDiagnosticClientCapacity]struct {
			window string
			client *Client
		}
		count, visited := 0, 0
		for _, windowType := range []WindowType{WindowTypeQuality, WindowTypeSpeed} {
			window := self.windows[windowType]
			if window == nil {
				continue
			}
			func() {
				window.stateLock.Lock()
				defer window.stateLock.Unlock()
				result.ClientCount += len(window.clients)
				for _, channel := range window.clients {
					if visited == len(clients) {
						result.Truncated = true
						break
					}
					visited++
					if channel == nil || channel.client == nil {
						continue
					}
					clients[count].window = "speed"
					if windowType == WindowTypeQuality {
						clients[count].window = "quality"
					}
					clients[count].client = channel.client
					count++
				}
			}()
		}
		for _, entry := range clients[:count] {
			buffer := entry.client.sendBuffer
			if buffer == nil {
				continue
			}
			var sequences [transferPacingDiagnosticSequenceCapacity]*SendSequence
			count := 0
			func() {
				buffer.mutex.Lock()
				defer buffer.mutex.Unlock()
				result.SequenceCount += len(buffer.activeSendSequences)
				for sequence := range buffer.activeSendSequences {
					if len(result.Sequences)+count == len(sequences) {
						result.Truncated = true
						break
					}
					sequences[count] = sequence
					count++
				}
			}()
			for _, sequence := range sequences[:count] {
				row := sequence.pacingDiagnosticSnapshot(origin, now)
				row.Window = entry.window
				result.Sequences = append(result.Sequences, row)
			}
		}
	}
	result.EndElapsedNanoseconds = now().Sub(origin).Nanoseconds()
	return result
}

// A closing sequence remains a valid ordinary Go object after lookup removal.
// Its admission/service locks still protect their scalars; no lifecycle is held.
func (self *SendSequence) pacingDiagnosticSnapshot(origin time.Time, now func() time.Time) transferPacingDiagnosticSequence {
	row := transferPacingDiagnosticSequence{
		SequenceOwner: fmt.Sprintf("%p", self),
		SequenceId:    self.sequenceId.String(), DestinationId: self.destination.String(),
		LogicalLane: self.logicalLane, Canceled: self.ctx != nil && self.ctx.Err() != nil,
	}
	start := now()
	estimate := self.sendWindowSnapshot(start)
	row.Estimate = transferPacingDiagnosticEstimate{
		StartElapsedNanoseconds: start.Sub(origin).Nanoseconds(), EndElapsedNanoseconds: now().Sub(origin).Nanoseconds(),
		Window: estimate.Window, CandidateWindow: estimate.CandidateWindow, Floor: estimate.Floor,
		Ceiling: estimate.Ceiling, Obtainable: estimate.Obtainable, DeliveredByteCount: estimate.DeliveredByteCount,
		Interval: estimate.Interval, WindowRoundTrip: estimate.WindowRoundTrip, SampleCount: estimate.SampleCount,
		Reason: estimate.Reason, ServiceByteRate: estimate.ServiceByteRate, AggregateDeliveryByteRate: estimate.AggregateDeliveryByteRate,
		PacingByteRate: estimate.PacingByteRate, PacingProbeByteRate: estimate.PacingProbeByteRate,
		PacingHeldByteRate: estimate.PacingHeldByteRate, ServiceEstablished: estimate.ServiceEstablished,
		ServiceBacklogged: estimate.ServiceBacklogged, PacingDiscovery: estimate.PacingDiscovery,
	}
	row.Admission = self.packAdmission.pacingDiagnosticSnapshot(self.packs, origin, now)
	row.Service = self.windowPacer.service.pacingDiagnosticSnapshot(&self.windowPacer.waiter, origin, now)
	return row
}

// Sampling neither grants a slot nor drains a ready channel.
func (self *sendPackAdmission) pacingDiagnosticSnapshot(packs chan *SendPack, origin time.Time, now func() time.Time) transferPacingDiagnosticAdmission {
	row := transferPacingDiagnosticAdmission{StartElapsedNanoseconds: now().Sub(origin).Nanoseconds()}
	row.ChannelCount, row.ChannelCapacity = len(packs), cap(packs)
	if self != nil {
		func() {
			self.mutex.Lock()
			defer self.mutex.Unlock()
			row.Present, row.Closed = true, self.closed
			row.Capacity, row.Count, row.FlowCount, row.WaiterCount = self.capacity, self.count, len(self.byKey), len(self.waiters)
			if len(self.waiters) != 0 {
				key := self.waiters[0].key
				row.OldestFlowCount, row.OldestFlowLimit = self.byKey[key], self.capacity
				if key.valid && row.OldestFlowLimit > 1 {
					row.OldestFlowLimit--
				}
			}
		}()
	}
	row.EndElapsedNanoseconds = now().Sub(origin).Nanoseconds()
	return row
}

// No mutation, including meter refill or ready-channel consumption, is allowed.
func (self *windowPacingService) pacingDiagnosticSnapshot(waiter *windowPacingWaiter, origin time.Time, now func() time.Time) transferPacingDiagnosticService {
	row := transferPacingDiagnosticService{StartElapsedNanoseconds: now().Sub(origin).Nanoseconds()}
	if self != nil {
		elapsed := func(at time.Time) int64 {
			if at.IsZero() {
				return 0
			}
			return at.Sub(origin).Nanoseconds()
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			row.Present = true
			row.NextElapsedNanoseconds = elapsed(self.next)
			row.MeterInitialized = !self.burstMeter.at.IsZero()
			row.MeterAtElapsedNanoseconds = elapsed(self.burstMeter.at)
			row.MeterPaidUntilElapsedNanoseconds = elapsed(self.burstMeter.paidUntil)
			row.MeterRate, row.MeterLimit, row.MeterReducedLimit = self.burstMeter.rate, self.burstMeter.limit, self.burstMeter.reducedLimit
			row.MeterAvailable, row.MeterSpent = self.burstMeter.available, self.burstMeter.spent
			row.EstimateInterval, row.ProbeSent, row.HeldPacingRate = self.burstEstimateTime, self.probeSent, self.heldPacingRate
			row.QueueObservedElapsedNanoseconds = elapsed(self.queueObservedAt)
			row.Reservations, row.ReservedByteCount = self.pacingReservations, self.reservedByteCount
			row.Sent, row.Delivered, row.PendingWrites = self.sent, self.total, self.pendingWrites
			row.DrainUntilElapsedNanoseconds = elapsed(self.drainUntil)
			if waiter != nil {
				row.WaiterHead = self.waiterHead == waiter
				row.WaiterQueued = row.WaiterHead || waiter.previous != nil || waiter.next != nil
				if row.WaiterQueued {
					row.WaiterDeadlineElapsedNanoseconds = elapsed(waiter.deadline)
					row.WaiterSerialization = waiter.serialization
					row.WaiterReadyCount = len(waiter.ready)
				}
			}
		}()
	}
	row.EndElapsedNanoseconds = now().Sub(origin).Nanoseconds()
	return row
}
