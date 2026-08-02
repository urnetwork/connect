package connect

import (
	"context"
	"testing"
	"time"
)

func TestMemoryTargetAcquireReleaseAndResize(t *testing.T) {
	target := NewMemoryTarget(kib(8))
	if !target.TryReserve(kib(8)) {
		t.Fatal("initial reservation should fit")
	}
	AssertEqual(t, target.Used(), kib(8))
	AssertEqual(t, target.Available(), ByteCount(0))

	acquired := make(chan bool, 1)
	go func() {
		acquired <- target.Acquire(context.Background(), kib(4))
	}()
	select {
	case <-acquired:
		t.Fatal("acquisition should wait while the target is full")
	case <-time.After(10 * time.Millisecond):
	}

	// A live increase wakes waiters without rebuilding the target.
	target.SetCapacity(kib(12))
	select {
	case ok := <-acquired:
		if !ok {
			t.Fatal("acquisition should succeed after capacity grows")
		}
	case <-time.After(time.Second):
		t.Fatal("capacity increase did not wake acquisition")
	}
	AssertEqual(t, target.Used(), kib(12))

	// Shrinking is non-evicting and blocks new admission until usage drains.
	target.SetCapacity(kib(4))
	AssertEqual(t, target.Available(), ByteCount(0))
	if target.TryReserve(1) {
		t.Fatal("over-target usage should reject a new reservation")
	}
	target.Release(kib(8))
	AssertEqual(t, target.Used(), kib(4))
	target.Release(kib(4))
	AssertEqual(t, target.Used(), ByteCount(0))
	AssertEqual(t, target.Available(), kib(4))
}

func TestMemoryTargetAcquireCancellation(t *testing.T) {
	target := NewMemoryTarget(kib(1))
	if !target.TryReserve(kib(1)) {
		t.Fatal("initial reservation should fit")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if target.Acquire(ctx, kib(1)) {
		t.Fatal("canceled acquisition should fail")
	}
	AssertEqual(t, target.Used(), kib(1))
	target.Release(kib(1))
}

func TestMemoryTargetUnlimitedAndSingletonOverdraft(t *testing.T) {
	unlimited := NewMemoryTarget(0)
	if !unlimited.TryReserve(mib(4)) {
		t.Fatal("zero-capacity target should be unlimited")
	}
	AssertEqual(t, unlimited.Available(), ByteCount(^uint64(0)>>1))
	unlimited.Release(mib(4))

	// One item larger than capacity is admitted from empty to avoid an
	// impossible-to-satisfy waiter. Further admission is blocked until it
	// releases; callers requiring a strict ceiling must cap item size.
	target := NewMemoryTarget(kib(1))
	if !target.TryReserve(kib(2)) {
		t.Fatal("singleton reservation should make progress")
	}
	if target.TryReserve(1) {
		t.Fatal("singleton overdraft should block additional reservations")
	}
	target.Release(kib(2))
	AssertEqual(t, target.Used(), ByteCount(0))
}
