package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"
)

func legacyQueueTestPacket(identity uint32, size int) []byte {
	packet := MessagePoolGet(size)
	for index := range packet {
		packet[index] = byte(identity)
	}
	binary.BigEndian.PutUint32(packet, identity)
	return packet
}

func TestP2pLegacySendQueueSharedBudgetRetainsBlockedOwnerUntilJoin(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		budget := NewTransferMemoryBudget(kib(512))
		if !budget.TryReserve(kib(128)) {
			t.Fatal("shared owner reservation failed")
		}
		gate := make(chan struct{})
		entered := make(chan struct{})
		calls := 0
		q := newP2pLegacySendQueue(ctx, cancel, func([]byte, time.Time) error {
			calls++
			if calls == 1 {
				close(entered)
				<-gate
			}
			return nil
		}, kib(256), budget)
		if q == nil {
			t.Fatal("queue owner was not admitted")
		}
		for index := range 30 {
			if err := q.enqueue(legacyQueueTestPacket(uint32(index), 1134), time.Time{}, false); err != nil {
				t.Fatal(err)
			}
		}
		<-entered
		synctest.Wait()
		q.mutex.Lock()
		retained, peak := q.retained, q.peakRetained
		q.mutex.Unlock()
		if retained <= 4*2060 || peak > kib(256) || budget.UsedByteCount() != kib(128)+q.ownerCharge+retained {
			t.Fatalf("live shared accounting: retained=%d peak=%d budget=%d", retained, peak, budget.UsedByteCount())
		}
		cancel()
		synctest.Wait()
		select {
		case <-q.done:
			t.Fatal("worker joined while the physical write still borrowed its root")
		default:
		}
		if budget.UsedByteCount() != kib(128)+q.ownerCharge+retained {
			t.Fatal("cancellation released a physically borrowed root")
		}
		close(gate)
		q.stopAndWait()
		q.stopAndWait()
		if calls != 1 || budget.UsedByteCount() != kib(128) {
			t.Fatalf("final drain calls=%d shared=%d", calls, budget.UsedByteCount())
		}
		budget.Release(kib(128))
		if stats := budget.Stats(); stats.ReservedByteCount != stats.ReleasedByteCount {
			t.Fatalf("unbalanced lifecycle: %+v", stats)
		}
	})
}

func TestP2pLegacySendQueueControlLargeAndDeadlineOrdering(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, large := range []bool{false, true} {
		t.Run(fmt.Sprintf("large=%t", large), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				budget := NewTransferMemoryBudget(kib(512))
				gate := make(chan struct{})
				var identities []uint32
				deadline := time.Now().Add(2 * time.Second)
				q := newP2pLegacySendQueue(ctx, cancel, func(packet []byte, observedDeadline time.Time) error {
					identity := binary.BigEndian.Uint32(packet)
					if identity == 0 {
						<-gate
					}
					if !observedDeadline.Equal(deadline) {
						t.Errorf("deadline changed: got=%s want=%s", observedDeadline, deadline)
					}
					if !bytes.Equal(packet[4:], bytes.Repeat([]byte{byte(identity)}, len(packet)-4)) {
						t.Error("compact storage overwrote a borrowed physical write")
					}
					identities = append(identities, identity)
					return nil
				}, kib(256), budget)
				defer q.stopAndWait()
				for index := range 30 {
					if err := q.enqueue(legacyQueueTestPacket(uint32(index), 1134), deadline, false); err != nil {
						t.Fatal(err)
					}
				}
				completed := make(chan error, 1)
				size := 64
				if large {
					size = 16 * 1024
				}
				go func() { completed <- q.enqueue(legacyQueueTestPacket(30, size), deadline, !large) }()
				synctest.Wait()
				select {
				case <-completed:
					t.Fatal("ordered synchronous write overtook a blocked older message")
				default:
				}
				close(gate)
				if err := <-completed; err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				if len(identities) != 31 {
					t.Fatalf("write count=%d", len(identities))
				}
				for index, identity := range identities {
					if identity != uint32(index) {
						t.Fatalf("ordering at %d: %v", index, identities)
					}
				}
				if budget.UsedByteCount() != q.ownerCharge {
					t.Fatal("quiet queue retained packet roots")
				}
			})
		})
	}
}

func TestP2pLegacySendQueueNoBudgetFallbackStaysSynchronous(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		budget := NewTransferMemoryBudget(kib(64))
		gate, entered := make(chan struct{}), make(chan struct{})
		q := newP2pLegacySendQueue(ctx, cancel, func([]byte, time.Time) error { close(entered); <-gate; return nil }, kib(256), budget)
		if q == nil {
			t.Fatal("queue metadata admission failed")
		}
		external := budget.Available()
		if !budget.TryReserve(external) {
			t.Fatal("competing reservation failed")
		}
		completed := make(chan error, 1)
		go func() { completed <- q.enqueue(legacyQueueTestPacket(1, 1134), time.Time{}, false) }()
		<-entered
		synctest.Wait()
		select {
		case <-completed:
			t.Fatal("no-budget write became asynchronous")
		default:
		}
		q.mutex.Lock()
		retained := q.retained
		q.mutex.Unlock()
		if retained != 0 || budget.UsedByteCount() != kib(64) {
			t.Fatal("no-budget fallback added retained ownership")
		}
		cancel()
		<-q.done
		if budget.UsedByteCount() != kib(64) {
			t.Fatal("worker exit released metadata still used by the synchronous producer")
		}
		close(gate)
		if err := <-completed; err != nil {
			t.Fatal(err)
		}
		q.stopAndWait()
		if budget.UsedByteCount() != external {
			t.Fatal("producer join did not release exact owner")
		}
		budget.Release(external)
	})
}

func TestP2pLegacySendQueueErrorCancelsAndReturnsAllRoots(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		budget := NewTransferMemoryBudget(kib(512))
		gate := make(chan struct{})
		writeError := errors.New("deterministic carrier failure")
		q := newP2pLegacySendQueue(ctx, cancel, func([]byte, time.Time) error { <-gate; return writeError }, kib(256), budget)
		for index := range 30 {
			if err := q.enqueue(legacyQueueTestPacket(uint32(index), 1134), time.Time{}, false); err != nil {
				t.Fatal(err)
			}
		}
		close(gate)
		<-q.done
		if !errors.Is(q.flush(), writeError) || ctx.Err() == nil {
			t.Fatal("carrier failure did not cancel its generation")
		}
		q.stopAndWait()
		if budget.UsedByteCount() != 0 {
			t.Fatal("error drain retained budget")
		}
	})
}
