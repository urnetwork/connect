// Measure tail-accounting CPU cost without physical serialization or RTT probes.
package connect

import (
	"testing"
	"time"
)

// One confirmed sibling remains unacknowledged throughout. Every measured
// cycle replaces and completes another sequence's tail, so the service never
// becomes drained and cannot update an RTT probe or timing ring. IDs and both
// map slots are prepared before the timer; callbacks retain their real locks.
func benchmarkWindowPacingTailLifecycle(b *testing.B, retry bool) {
	at := time.Unix(1700000000, 0)
	service := &windowPacingService{}
	sibling, siblingMessage := NewId(), NewId()
	sequence, original, fresh := NewId(), NewId(), NewId()
	service.beginWrite(sibling, siblingMessage, 1, at, false)
	service.finishWrite(sibling, siblingMessage, true)
	service.beginWrite(sequence, original, 1, at, false)
	service.finishWrite(sequence, original, true)
	service.acknowledgeWrite(sequence, original, 1, false, 0, at)
	var number uint64 = 2
	b.ReportAllocs()
	for b.Loop() {
		service.beginWrite(sequence, original, number, at, false)
		service.finishWrite(sequence, original, true)
		if retry {
			// Recovery invalidates before its paced physical retry; the
			// ambiguous reply cannot complete that tail. A fresh original
			// then supplies the eventual cumulative delivery proof.
			service.invalidateMessageProbe(sequence, original)
			service.beginWrite(sequence, original, number, at, true)
			service.finishWrite(sequence, original, true)
			service.acknowledgeWrite(sequence, original, number, false, 0, at)
			number++
			service.beginWrite(sequence, fresh, number, at, false)
			service.finishWrite(sequence, fresh, true)
			service.acknowledgeWrite(sequence, fresh, number, false, 0, at)
		} else {
			service.acknowledgeWrite(sequence, original, number, false, 0, at)
		}
		number++
	}
	if service.pendingWrites != 1 || !service.writes[sibling].pending || service.writes[sequence].pending ||
		service.drained || !service.roundTripProbe.sentAt.IsZero() || service.roundTripStats.ring != nil || !service.canDrainWithLock() {
		b.Fatal("tail accounting lost ownership or created timing evidence")
	}
}

// An ordinary physical write and cumulative reply pay the accounting hot path.
func BenchmarkWindowPacingTailLifecycleOriginal(b *testing.B) {
	benchmarkWindowPacingTailLifecycle(b, false)
}

// A copied tail and its fresh replacement exercise ambiguous-tail maintenance.
func BenchmarkWindowPacingTailLifecycleRetryFresh(b *testing.B) {
	benchmarkWindowPacingTailLifecycle(b, true)
}
