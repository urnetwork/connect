// Optional initial-ping diagnostics. These snapshots do not classify a provider
// or alter admission, retries, cancellation, timeouts, or ownership.
package connect

import (
	"sync/atomic"
	"time"
)

type initialPingOutcome int

const (
	initialPingAcknowledged initialPingOutcome = iota
	initialPingExpired
	initialPingError
	initialPingCanceled
)

var initialPingOutcomeLabels = [...]string{"acknowledged", "expired", "error", "canceled_or_ended"}

type initialPingDependency int

const (
	initialPingCarrierUnknown initialPingDependency = iota
	initialPingCarrierAbsent
	initialPingContractWaiting
	initialPingContractFailed
	initialPingWriteAttempted
	initialPingNoContractOrWrite
)

var initialPingDependencyLabels = [...]string{
	"carrier_unknown",
	"carrier_absent",
	"carrier_present_contract_waiting_without_write",
	"carrier_present_contract_failed_without_write",
	"carrier_present_write_attempted",
	"carrier_present_no_contract_or_write",
}

type initialPingObservationCell struct {
	count  atomic.Uint64
	micros atomic.Uint64
}

// InitialPingObservations records only terminal initial-ping evaluations after
// successful construction. Acknowledged is not admission: an evaluated surplus
// or a replacement can still be declined. Pending evaluations are not durations
// in this aggregate. Started and completed cells are independent atomic reads.
//
// The aggregate has 24 fixed cells (count and elapsed seconds) plus one started
// count: 49 scalar series in Snapshot. PathSnapshot adds 48 fixed scalar
// series for the exact initial Pack's route and ACK witnesses. It retains no identifiers,
// destinations, errors, URLs, clients, callbacks or goroutines. Do not copy it
// after use. A nil observer performs no snapshot or recording work.
type InitialPingObservations struct {
	started   atomic.Uint64
	cells     [len(initialPingOutcomeLabels)][len(initialPingDependencyLabels)]initialPingObservationCell
	pathCells [len(initialPingOutcomeLabels)][len(initialPingRouteWriteLabels)][len(initialPingAckCallbackLabels)]initialPingObservationCell
}

type InitialPingObservation struct {
	Outcome    string
	Dependency string
	Count      uint64
	Seconds    float64
}

func (self *InitialPingObservations) Started() uint64 {
	if self == nil {
		return 0
	}
	return self.started.Load()
}

func (self *InitialPingObservations) Snapshot() [len(initialPingOutcomeLabels) * len(initialPingDependencyLabels)]InitialPingObservation {
	var values [len(initialPingOutcomeLabels) * len(initialPingDependencyLabels)]InitialPingObservation
	i := 0
	for outcome, outcomeLabel := range initialPingOutcomeLabels {
		for dependency, dependencyLabel := range initialPingDependencyLabels {
			values[i] = InitialPingObservation{Outcome: outcomeLabel, Dependency: dependencyLabel}
			if self != nil {
				cell := &self.cells[outcome][dependency]
				values[i].Count = cell.count.Load()
				values[i].Seconds = float64(cell.micros.Load()) / 1e6
			}
			i++
		}
	}
	return values
}

func (self *InitialPingObservations) begin() {
	if self != nil {
		self.started.Add(1)
	}
}

func (self *InitialPingObservations) record(outcome initialPingOutcome, dependency initialPingDependency, elapsed time.Duration) {
	if self == nil || outcome < 0 || int(outcome) >= len(initialPingOutcomeLabels) ||
		dependency < 0 || int(dependency) >= len(initialPingDependencyLabels) || elapsed < 0 {
		return
	}
	cell := &self.cells[outcome][dependency]
	cell.micros.Add(uint64(elapsed.Microseconds()))
	cell.count.Add(1)
}

// Sample before the evaluation's own cancellation removes its carrier. TryLock
// deliberately returns unknown under contention: a diagnostic must not wait on
// route publication while holding the expansion's existing terminal mutex.
// Present means at least one registered send or receive route at this instant;
// it is neither continuous availability nor successful provider delivery.
func initialPingDependencySnapshot(client *Client, attempt *providerEvaluationAttempt) initialPingDependency {
	if client == nil || client.routeManager == nil || attempt == nil {
		return initialPingCarrierUnknown
	}
	manager := client.routeManager
	if !manager.mutex.TryLock() {
		return initialPingCarrierUnknown
	}
	active := len(manager.writerMatchState.transportRoutes) > 0 || len(manager.readerMatchState.transportRoutes) > 0
	manager.mutex.Unlock()
	if !active {
		return initialPingCarrierAbsent
	}
	// Contact is checked after absence witnesses so an earlier bounded write
	// cannot be hidden by a new wait. This is still an endpoint snapshot, not
	// an atomic timeline. An attempted write never proves remote receipt.
	waiting, failed := attempt.contractWaiters.Load() > 0, attempt.contractFailed.Load()
	if attempt.providerContact.Load() {
		return initialPingWriteAttempted
	}
	if waiting {
		return initialPingContractWaiting
	}
	if failed {
		return initialPingContractFailed
	}
	return initialPingNoContractOrWrite
}

// These witnesses belong only to the original initial-ping Pack. The existing
// provider-contact witness also includes control heads and unsuccessful writes;
// it retains its original failure-authority semantics.
var initialPingRouteWriteLabels = [...]string{"not_observed", "accepted"}
var initialPingAckCallbackLabels = [...]string{"pending", "success", "error"}

type initialPingPathWitness struct {
	routeAccepted atomic.Bool
	ackCallback   atomic.Uint32 // 0 pending, 1 success, 2 error; retain first entry
}

func (self *initialPingPathWitness) observeRouteWrite(_ TransportType) {
	if self != nil {
		self.routeAccepted.Store(true)
	}
}

func (self *initialPingPathWitness) observeAckCallback(err error) {
	if self == nil {
		return
	}
	state := uint32(1)
	if err != nil {
		state = 2
	}
	self.ackCallback.CompareAndSwap(0, state)
}

func (self *initialPingPathWitness) snapshot() (route, ack int) {
	if self == nil {
		return
	}
	if self.routeAccepted.Load() {
		route = 1
	}
	ack = int(self.ackCallback.Load())
	return
}

// InitialPingPathObservation is one fixed terminal-evaluation cell. Accepted
// means the original ping Pack had a successful local route write, including a
// later retry; it does not prove remote receipt. Not observed is an absence of
// this witness, not proof that no bytes were delivered. ACK callback states are
// sampled before this evaluation's own cancellation and may race the deadline.
// Callback success is not necessarily admission. Seconds is whole evaluation
// residence, not time spent in the named route or callback state.
//
// Route and callback witnesses are independent atomic reads. In particular,
// success with route acceptance not yet observed is conservatively retained;
// a fast peer ACK can precede publication of the writer's return observation.
type InitialPingPathObservation struct {
	Outcome     string
	RouteWrite  string
	AckCallback string
	Count       uint64
	Seconds     float64
}

// PathSnapshot exports 24 fixed cells (48 count/seconds scalars), including
// zeros. Like Snapshot, its fields are independent reads, not an atomic census.
func (self *InitialPingObservations) PathSnapshot() [len(initialPingOutcomeLabels) * len(initialPingRouteWriteLabels) * len(initialPingAckCallbackLabels)]InitialPingPathObservation {
	var values [len(initialPingOutcomeLabels) * len(initialPingRouteWriteLabels) * len(initialPingAckCallbackLabels)]InitialPingPathObservation
	i := 0
	for outcome, outcomeLabel := range initialPingOutcomeLabels {
		for route, routeLabel := range initialPingRouteWriteLabels {
			for ack, ackLabel := range initialPingAckCallbackLabels {
				values[i] = InitialPingPathObservation{Outcome: outcomeLabel, RouteWrite: routeLabel, AckCallback: ackLabel}
				if self != nil {
					cell := &self.pathCells[outcome][route][ack]
					values[i].Count = cell.count.Load()
					values[i].Seconds = float64(cell.micros.Load()) / 1e6
				}
				i++
			}
		}
	}
	return values
}

func (self *InitialPingObservations) recordPath(outcome initialPingOutcome, route, ack int, elapsed time.Duration) {
	if self == nil || outcome < 0 || int(outcome) >= len(initialPingOutcomeLabels) ||
		route < 0 || route >= len(initialPingRouteWriteLabels) || ack < 0 || ack >= len(initialPingAckCallbackLabels) || elapsed < 0 {
		return
	}
	cell := &self.pathCells[outcome][route][ack]
	cell.micros.Add(uint64(elapsed.Microseconds()))
	cell.count.Add(1)
}
