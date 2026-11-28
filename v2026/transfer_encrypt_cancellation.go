// First-cancellation attribution for peer TLS establishment.
package connect

import (
	"context"
	"sync/atomic"
)

// A private context cause carries one fixed owner. Counters span the process
// and retain only the wire role and companion flag, never peers or epochs.
type tlsCancellationOwner struct {
	name   string
	counts [2][2]atomic.Uint64
}

var (
	tlsParentLifetime         = &tlsCancellationOwner{name: "parent_lifetime"}
	tlsManagerRetired         = &tlsCancellationOwner{name: "manager_retired"}
	tlsSessionRetired         = &tlsCancellationOwner{name: "session_retired"}
	tlsControlEnqueueRejected = &tlsCancellationOwner{name: "control_enqueue_rejected"}
	tlsEpochRetired           = &tlsCancellationOwner{name: "epoch_retired"}
	tlsWorkerRecovery         = &tlsCancellationOwner{name: "worker_recovery"}
	tlsCancellationUnknown    = &tlsCancellationOwner{name: "unknown"}

	tlsCancellationOwners = [...]*tlsCancellationOwner{
		tlsParentLifetime, tlsManagerRetired, tlsSessionRetired, tlsControlEnqueueRejected,
		tlsEpochRetired, tlsWorkerRecovery, tlsCancellationUnknown,
	}
)

// Keep cancellation text stable for callers inspecting the cause.
func (self *tlsCancellationOwner) Error() string { return context.Canceled.Error() }

// Preserve cancellation matching for callers that inspect the cause chain.
func (self *tlsCancellationOwner) Unwrap() error { return context.Canceled }

// Local cancellations use the fixed owners above. An inherited cause may
// contain private application data. A live context has no known owner.
func tlsHandshakeCancellationOwner(ctx context.Context) *tlsCancellationOwner {
	if ctx == nil || ctx.Err() != context.Canceled {
		return tlsCancellationUnknown
	}
	if owner, ok := context.Cause(ctx).(*tlsCancellationOwner); ok {
		return owner
	}
	return tlsParentLifetime
}

// Count once on the same edge as the existing error and establishment event.
func (self *tlsCancellationOwner) record(role sequenceTlsRole, companion bool) {
	if role != sequenceTlsRoleClient && role != sequenceTlsRoleServer {
		return
	}
	companionIndex := 0
	if companion {
		companionIndex = 1
	}
	self.counts[role][companionIndex].Add(1)
}

// A cumulative process count of emitted, coalesced current-epoch
// context-canceled handshake completions. Each field has a fixed domain.
type TlsHandshakeCancellationCount struct {
	Source    string
	Role      string
	Companion bool
	Count     uint64
}

// Snapshot all 28 cells, including observed zero counters. Each cell is
// atomic; a cancellation during a scrape may appear on the following one.
func GetTlsHandshakeCancellationCounts() []TlsHandshakeCancellationCount {
	counts := make([]TlsHandshakeCancellationCount, 0, len(tlsCancellationOwners)*4)
	for _, owner := range tlsCancellationOwners {
		for role := range owner.counts {
			for companion := range owner.counts[role] {
				counts = append(counts, TlsHandshakeCancellationCount{
					Source: owner.name, Role: sequenceTlsRole(role).String(), Companion: companion != 0,
					Count: owner.counts[role][companion].Load(),
				})
			}
		}
	}
	return counts
}
