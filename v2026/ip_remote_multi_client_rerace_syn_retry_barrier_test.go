// Exact-flow retry barriers separate source retransmission controls from
// enabled retained-SYN continuation without changing production policy.
package connect

import (
	"sync"
	"sync/atomic"
	"testing"
)

// The exact source update is latched before its failure. Entry stays observable
// after any number of assertions, and release also runs before fixture teardown.
type reraceSynRetryBarrier struct {
	harness     *reraceHarness
	update      atomic.Pointer[multiClientChannelUpdate]
	entered     chan struct{}
	unblock     chan struct{}
	enterOnce   sync.Once
	releaseOnce sync.Once
}

// Only the supplied generation enters the barrier.
func reraceHoldSynRetry(t *testing.T, harness *reraceHarness, update *multiClientChannelUpdate) *reraceSynRetryBarrier {
	t.Helper()
	if update == nil {
		t.Fatal("retry barrier requires the exact admitted source update")
	}
	return newReraceSynRetryBarrier(t, harness, update)
}

// Captures the exact original source update before admission can produce an
// early provider failure. The dispatch hook itself never matches by tuple.
func reraceHoldSynRetryOnSource(t *testing.T, harness *reraceHarness, path *IpPath) *reraceSynRetryBarrier {
	t.Helper()
	barrier := newReraceSynRetryBarrier(t, harness, nil)
	previous := harness.multiClient.beforePacketGroupGateForTest
	harness.multiClient.beforePacketGroupGateForTest = func(update *multiClientChannelUpdate, client *multiClientChannel, group *parsedPacketGroup) {
		if !group.isSynRetry() && group.ipPath.Protocol == IpProtocolTcp && group.ipPath.Syn && !group.ipPath.Ack &&
			group.ipPath.SequenceNumber == path.SequenceNumber && reraceSameFlow(group.ipPath, path) {
			barrier.update.CompareAndSwap(nil, update)
		}
		if previous != nil {
			previous(update, client, group)
		}
	}
	return barrier
}

// Hook publication happens at fixture quiescence and stays stable through
// teardown; the dispatcher waits outside all parent and flow locks.
func newReraceSynRetryBarrier(t *testing.T, harness *reraceHarness, update *multiClientChannelUpdate) *reraceSynRetryBarrier {
	t.Helper()
	harness.settle()
	barrier := &reraceSynRetryBarrier{
		harness: harness,
		entered: make(chan struct{}),
		unblock: make(chan struct{}),
	}
	barrier.update.Store(update)
	previous := harness.multiClient.beforeSynRetryDispatchForTest
	harness.multiClient.beforeSynRetryDispatchForTest = func(update *multiClientChannelUpdate) {
		if update == barrier.update.Load() {
			barrier.enterOnce.Do(func() { close(barrier.entered) })
			select {
			case <-barrier.unblock:
			case <-harness.multiClient.ctx.Done():
			}
		}
		if previous != nil {
			previous(update)
		}
	}
	t.Cleanup(barrier.release)
	return barrier
}

// Settle before calling: lack of entry is a state assertion, not a timeout.
func (self *reraceSynRetryBarrier) assertNotEntered(t *testing.T) {
	t.Helper()
	update := self.update.Load()
	if update == nil || self.harness.update(update.ipPath) != update {
		t.Fatal("retry barrier did not latch the exact current source update")
	}
	select {
	case <-self.entered:
		t.Fatal("unrelated signal reached the exact flow's retry dispatcher")
	default:
	}
}

// A persistent entry latch verifies every observation against the same flow.
func (self *reraceSynRetryBarrier) assertEntered(t *testing.T) {
	t.Helper()
	update := self.update.Load()
	if update == nil || self.harness.update(update.ipPath) != update {
		t.Fatal("retry barrier did not latch the exact current source update")
	}
	select {
	case <-self.entered:
	default:
		t.Fatal("explicit failure did not reach the exact flow's retry dispatcher")
	}
}

// Explicit release and cleanup share one idempotent ownership handoff.
func (self *reraceSynRetryBarrier) release() {
	self.releaseOnce.Do(func() { close(self.unblock) })
}
