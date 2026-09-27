package connect

import "sync"

// Observation-only storage is bounded even when an unbounded send repeatedly
// encounters backpressure. Overflow remains an unrecovered terminal failure;
// the diagnostic cannot turn unknown history into a passing verdict.
const sendPackAdmissionObservationCapacity = 64

type pendingSendPackAdmissionObservation struct {
	observer    func(SendPackLifecycleObservation)
	observation SendPackLifecycleObservation
}

// One scope belongs to one exact input group and one native send invocation,
// not a destination, flow tuple, Client, or token reused by another call.
type sendPackAdmissionObservations struct {
	mutex     sync.Mutex
	pending   []pendingSendPackAdmissionObservation
	completed bool
}

// Called only by the serialized selection owner, before candidate goroutines
// start. The ordinary nil-observer path allocates neither scope nor callback.
func (self *parsedPacketGroup) prepareAdmissionObservations(client *multiClientChannel) {
	if self.admissionObservations == nil && client != nil && client.client != nil &&
		client.client.settings.SendBufferSettings.SendPackLifecycleObserver != nil {
		self.admissionObservations = &sendPackAdmissionObservations{}
	}
}

// Only enqueue's typed synchronous refusal is deferred. Accepted-Pack write
// failures, timeouts, and terminal acknowledgements pass through unchanged.
func (self *sendPackAdmissionObservations) wrap(observer func(SendPackLifecycleObservation)) func(SendPackLifecycleObservation) {
	return func(observation SendPackLifecycleObservation) {
		if observation.Phase == SendPackLifecyclePhaseTerminal {
			if admissionErr, ok := observation.Err.(*SendPackAdmissionError); ok {
				self.mutex.Lock()
				if !self.completed && len(self.pending) < sendPackAdmissionObservationCapacity {
					self.pending = append(self.pending, pendingSendPackAdmissionObservation{observer, observation})
					self.mutex.Unlock()
					return
				}
				if !self.completed {
					err := *admissionErr
					err.OwnerTrackingOverflow = true
					observation.Err = &err
				}
				self.mutex.Unlock()
			}
		}
		observer(observation)
	}
}

// All candidate sends have returned before the owner calls this. Publication
// happens before the native send returns, preserving source-boundary joins.
func (self *sendPackAdmissionObservations) complete(accepted bool) {
	self.mutex.Lock()
	self.completed = true
	pending := self.pending
	self.pending = nil
	self.mutex.Unlock()
	for _, entry := range pending {
		err := *entry.observation.Err.(*SendPackAdmissionError)
		err.RecoveredByOwner = accepted
		entry.observation.Err = &err
		safeSendPackLifecycleObserve(entry.observer, entry.observation)
	}
}

// Internal observer override; only the group owner supplies it, and public
// callbacks and send results never use this measurement-only option.
type sendPackLifecycleObserverOption struct {
	observer func(SendPackLifecycleObservation)
}
