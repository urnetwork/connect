// Contract acquisition is local control work, not evidence of a silent provider.
// These owner-scoped witnesses survive send-sequence and candidate retirement;
// any attempted provider write permanently disables the no-contact proof.
package connect

import (
	"sync/atomic"
	"time"
)

// Window-owned pending waits and monotonic outcomes, safe for concurrent readers.
// It is deliberately conservative across retries: an old contact is not erased.
type providerEvaluationState struct {
	localContractFailure     atomic.Bool
	providerContact          atomic.Bool
	pendingContractWaiters   atomic.Int32
	pendingLocalWrites       atomic.Int32
	localWriteFailed         atomic.Bool
	applicationWriteAdmitted atomic.Bool
}

// One candidate owns this witness before its Client is constructed. Only actual
// contract waits and bounded writes to the selected destination may update it.
// All fields are safe for concurrent send sequences and evaluation callbacks.
type providerEvaluationAttempt struct {
	owner             *providerEvaluationState
	destinationId     Id
	observeLocalWrite bool
	contractWaiters   atomic.Int32
	contractFailed    atomic.Bool
	providerContact   atomic.Bool
}

// Nil is the ordinary non-multi-client path and adds no ownership or observer.
func (self *providerEvaluationAttempt) beginContractWait(destinationId Id) {
	if self != nil && self.destinationId == destinationId {
		self.owner.pendingContractWaiters.Add(1)
		self.contractWaiters.Add(1)
	}
}

// Publish a natural acquisition failure before releasing its waiting witness.
// Owner cancellation alone must never become a local-control failure.
func (self *providerEvaluationAttempt) endContractWait(destinationId Id, failed bool) {
	if self != nil && self.destinationId == destinationId {
		if failed {
			self.contractFailed.Store(true)
			self.owner.localContractFailure.Store(true)
		}
		self.contractWaiters.Add(-1)
		self.owner.pendingContractWaiters.Add(-1)
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

// True during a local acquisition wait or after its failure, before any attempted
// provider write. A probe can stop before the later ping timer classifies a wait.
// This declines measurement; it never pronounces a provider good. The immutable
// windows map and atomic witnesses permit concurrent reads.
func (self *RemoteUserNatMultiClient) ProviderContractAcquisitionUnavailable() bool {
	if self == nil {
		return false
	}
	unavailable := false
	for _, window := range self.windows {
		unavailable = unavailable || window.providerEvaluation.pendingContractWaiters.Load() > 0 ||
			window.providerEvaluation.localContractFailure.Load()
	}
	// Check every monotonic contact fence after reading absence evidence, so a
	// different window's earlier write cannot be hidden by a newly pending wait.
	for _, window := range self.windows {
		if window.providerEvaluation.providerContact.Load() {
			return false
		}
	}
	return unavailable
}

// Only data-only probes opt into the actual route-writer observation. Ordinary
// clients retain no additional per-packet atomic work or measuring authority.
func (self *providerEvaluationAttempt) beginLocalWrite(destinationId Id) {
	if self != nil && self.observeLocalWrite && self.destinationId == destinationId {
		self.owner.pendingLocalWrites.Add(1)
	}
}

// Successful application handoff is monotonic across candidate replacement.
// A contract-only head cannot prove that the queued application was admitted.
// Queue acceptance is not a claim of physical delivery or a provider reply.
func (self *providerEvaluationAttempt) endLocalWrite(destinationId Id, admitted, application bool) {
	if self != nil && self.observeLocalWrite && self.destinationId == destinationId {
		if admitted && application {
			self.owner.applicationWriteAdmitted.Store(true)
		} else if !admitted {
			self.owner.localWriteFailed.Store(true)
		}
		self.owner.pendingLocalWrites.Add(-1)
	}
}

// True only with an observed pending/refused writer and no application frame
// ever admitted to a local route. Preserve the separate attempted-write fence
// used by contract acquisition. False does not prove contact or delivery.
func (self *RemoteUserNatMultiClient) ProviderLocalWriteUnavailable() bool {
	if self == nil {
		return false
	}
	unavailable := false
	for _, window := range self.windows {
		unavailable = unavailable || window.providerEvaluation.pendingLocalWrites.Load() > 0 || window.providerEvaluation.localWriteFailed.Load()
	}
	for _, window := range self.windows {
		if window.providerEvaluation.applicationWriteAdmitted.Load() {
			return false
		}
	}
	return unavailable
}
