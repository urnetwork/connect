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

type pendingNoAckAdmissionObservation struct {
	observer    func(NoAckSendObservation)
	observation NoAckSendObservation
}

// One scope belongs to one exact input group and one native send invocation,
// not a destination, flow tuple, Client, or token reused by another call.
type sendPackAdmissionObservations struct {
	mutex        sync.Mutex
	pending      []pendingSendPackAdmissionObservation
	noAckPending []pendingNoAckAdmissionObservation
	completed    bool
	// Only a new TCP generation needs this inline observation. Reuse the
	// optional scope, not every packet/group descriptor or a packet owner.
	synAdmission tcpSynAdmission
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
	self.synAdmission.clear()
	self.mutex.Lock()
	self.completed = true
	pending := self.pending
	self.pending = nil
	noAckPending := self.noAckPending
	self.noAckPending = nil
	self.mutex.Unlock()
	for _, entry := range pending {
		err := *entry.observation.Err.(*SendPackAdmissionError)
		err.RecoveredByOwner = accepted
		entry.observation.Err = &err
		safeSendPackLifecycleObserve(entry.observer, entry.observation)
	}
	for _, entry := range noAckPending {
		entry.observation.RecoveredByOwner = accepted
		entry.observer(entry.observation)
	}
}

// Only the synchronous not-admitted result is deferred. A queued datagram's
// route-write error is an independent failure even if some later send works.
func (self *sendPackAdmissionObservations) wrapNoAck(observer func(NoAckSendObservation)) func(NoAckSendObservation) {
	return func(observation NoAckSendObservation) {
		if observation.Phase == NoAckSendPhaseCompleted && observation.Err == ErrNoAckSendNotAdmitted {
			self.mutex.Lock()
			if !self.completed && len(self.noAckPending) < sendPackAdmissionObservationCapacity {
				self.noAckPending = append(self.noAckPending, pendingNoAckAdmissionObservation{observer, observation})
				self.mutex.Unlock()
				return
			}
			if !self.completed {
				observation.OwnerTrackingOverflow = true
			}
			self.mutex.Unlock()
		}
		observer(observation)
	}
}

// Internal observer override; only the group owner supplies it, and public
// callbacks and send results never use this measurement-only option.
type sendPackLifecycleObserverOption struct {
	observer func(SendPackLifecycleObservation)
}

type sendNoAckObserverOption struct {
	observer func(NoAckSendObservation)
}
