// Contract acquisition is local control work, not evidence of a silent provider.
// These owner-scoped witnesses survive send-sequence and candidate retirement;
// any attempted provider write permanently disables the no-contact proof.
package connect

import (
	"sync/atomic"
	"time"
)

// Monotonic window evidence, safe for concurrent candidate and probe readers.
// It is deliberately conservative across retries: an old contact is not erased.
type providerEvaluationState struct {
	localContractFailure atomic.Bool
	providerContact      atomic.Bool
}

// One candidate owns this witness before its Client is constructed. Only actual
// contract waits and bounded writes to the selected destination may update it.
// All fields are safe for concurrent send sequences and evaluation callbacks.
type providerEvaluationAttempt struct {
	owner           *providerEvaluationState
	destinationId   Id
	contractWaiters atomic.Int32
	contractFailed  atomic.Bool
	providerContact atomic.Bool
}

// Nil is the ordinary non-multi-client path and adds no ownership or observer.
func (self *providerEvaluationAttempt) beginContractWait(destinationId Id) {
	if self != nil && self.destinationId == destinationId {
		self.contractWaiters.Add(1)
	}
}

// Publish a natural acquisition failure before releasing its waiting witness.
// Owner cancellation alone must never become a local-control failure.
func (self *providerEvaluationAttempt) endContractWait(destinationId Id, failed bool) {
	if self != nil && self.destinationId == destinationId {
		if failed {
			self.contractFailed.Store(true)
		}
		self.contractWaiters.Add(-1)
	}
}

// Call only at the actual bounded writer, including contract and crypto heads.
// Even an unsuccessful attempt prevents claiming that the provider was untested.
func (self *providerEvaluationAttempt) noteProviderWrite(destinationId Id) {
	if self != nil && self.destinationId == destinationId && !self.providerContact.Load() {
		self.owner.providerContact.Store(true)
		self.providerContact.Store(true)
	}
}

// Snapshot before cancellation can release an in-progress acquisition wait.
func (self *providerEvaluationAttempt) localContractUnavailable() bool {
	return self != nil && !self.providerContact.Load() &&
		(self.contractWaiters.Load() > 0 || self.contractFailed.Load())
}

// A caller has already established live-owner, no-contact contract failure.
// The existing retry/admission policy consumes platform evidence unchanged.
func (self *multiClientWindow) recordLocalContractFailure(args *multiClientChannelArgs) {
	args.providerEvaluation.owner.localContractFailure.Store(true)
	if ok, suppressed := self.pingFailThrottle.Allow(time.Now()); ok {
		self.log.Infof("[multi]evaluation local contract acquisition unavailable [%s]%s\n",
			args.ClientId, suppressedSuffix(suppressed))
	}
	self.recordEvaluationFailure(windowFailurePlatform, nil)
}

// True only after a local acquisition failure and before any provider-directed
// write attempt in this multi-client's lifetime. This is not a health verdict:
// a probe may use it to decline measurement, never to pronounce a provider good.
// The immutable windows map and atomic witnesses permit concurrent reads.
func (self *RemoteUserNatMultiClient) ProviderContractAcquisitionUnavailable() bool {
	if self == nil {
		return false
	}
	unavailable := false
	for _, window := range self.windows {
		if window.providerEvaluation.providerContact.Load() {
			return false
		}
		unavailable = unavailable || window.providerEvaluation.localContractFailure.Load()
	}
	return unavailable
}
