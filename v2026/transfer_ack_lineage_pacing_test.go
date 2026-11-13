//go:build acklineagetrace

package connect

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func ackLineagePacingFixture() (*SendSequence, *sendItem) {
	return &SendSequence{
		client: &Client{clientId: NewId()}, destination: NewId(), sequenceId: NewId(),
		sendBufferSettings: &SendBufferSettings{ResendQueueMaxByteCount: 5000},
		windowPacer:        windowBurstPacer{rate: 1234, estimateRate: 456, probeRate: 789, probeLimit: 1000},
	}, &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 7}, expectsAck: true, pacingByteCount: 55}
}

func TestAckLineagePacingObserverSnapshotAndExclusiveOwner(t *testing.T) {
	sequence, item := ackLineagePacingFixture()
	var got AckLineagePacingSnapshot
	closeOwner, ok := InstallAckLineagePacingObserver(func(AckLineagePacingIdentity) bool { return true }, func(value AckLineagePacingSnapshot) { got = value })
	if !ok {
		t.Fatal("diagnostic owner unavailable")
	}
	defer closeOwner()
	if _, ok := InstallAckLineagePacingObserver(func(AckLineagePacingIdentity) bool { return true }, func(AckLineagePacingSnapshot) {}); ok {
		t.Fatal("conflicting observer admitted")
	}
	observeAckLineagePacing(sequence, item, true)
	if got.Message != item.messageId || got.Sequence != sequence.sequenceId || got.Number != 7 || !got.Resend || got.NoAck ||
		got.CachedRate != 1234 || got.CachedServiceRate != 456 || got.CachedProbeRate != 789 || got.CachedProbeLimit != 1000 ||
		got.ItemPacingBytes != 55 || got.Window.Window != 5000 || got.AtUnixNano == 0 {
		t.Fatalf("snapshot lost cached/fresh distinction: %+v", got)
	}
	closeOwner()
	got = AckLineagePacingSnapshot{}
	observeAckLineagePacing(sequence, item, false)
	if got.AtUnixNano != 0 {
		t.Fatal("retired observer called")
	}
	nextClose, ok := InstallAckLineagePacingObserver(func(AckLineagePacingIdentity) bool { return false }, func(AckLineagePacingSnapshot) { t.Error("rejected claim observed") })
	if !ok {
		t.Fatal("retired owner did not release slot")
	}
	defer nextClose()
	closeOwner() // A stale cleanup cannot evict the new owner.
	if ackLineagePacingOwnerSlot.Load() == nil {
		t.Fatal("stale cleanup removed new owner")
	}
	observeAckLineagePacing(sequence, item, false)
}

func TestAckLineagePacingObserverCleanupJoinsAndPanicIsContained(t *testing.T) {
	func() {
		sequence, item := ackLineagePacingFixture()
		entered, release, finished, closed := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
		cleanup, ok := InstallAckLineagePacingObserver(func(AckLineagePacingIdentity) bool { return true }, func(AckLineagePacingSnapshot) {
			close(entered)
			<-release
			panic("diagnostic-only panic")
		})
		if !ok {
			t.Fatal("observer unavailable")
		}
		owner := ackLineagePacingOwnerSlot.Load()
		go func() { observeAckLineagePacing(sequence, item, false); close(finished) }()
		<-entered
		go func() { cleanup(); close(closed) }()
		var releaseOnce sync.Once
		defer func() { releaseOnce.Do(func() { close(release) }); <-finished; <-closed }()
		// Observe the actual pending writer, not a scheduler delay. Mutex
		// waits are not durable waits in testing/synctest.
		until := time.Now().Add(2 * time.Second)
		for {
			select {
			case <-closed:
				t.Fatal("cleanup passed active observer")
			default:
			}
			if !owner.join.TryRLock() {
				break
			}
			owner.join.RUnlock()
			if time.Now().After(until) {
				t.Fatal("cleanup never reached join")
			}
			runtime.Gosched()
		}
		releaseOnce.Do(func() { close(release) })
		<-finished
		<-closed
		if ackLineagePacingOwnerSlot.Load() != nil {
			t.Fatal("panic leaked owner")
		}
	}()
}

func TestAckLineagePacingObserverNilAndRejectedClaimAreAllocationFree(t *testing.T) {
	sequence, item := ackLineagePacingFixture()
	if got := testing.AllocsPerRun(100, func() { observeAckLineagePacing(sequence, item, false) }); got != 0 {
		t.Fatalf("nil observer allocations=%g", got)
	}
	var claims atomic.Int64
	cleanup, ok := InstallAckLineagePacingObserver(func(AckLineagePacingIdentity) bool { claims.Add(1); return false }, func(AckLineagePacingSnapshot) { t.Fatal("rejected snapshot") })
	if !ok {
		t.Fatal("observer unavailable")
	}
	defer cleanup()
	if got := testing.AllocsPerRun(100, func() { observeAckLineagePacing(sequence, item, false) }); got != 0 || claims.Load() == 0 {
		t.Fatalf("rejected observer allocations=%g claims=%d", got, claims.Load())
	}
}
