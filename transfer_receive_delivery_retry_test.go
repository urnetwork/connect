package connect

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestReceiveDeliveryRetryPreservesFlowOrderAndControlProgress(t *testing.T) {
	assertMessagePoolOwnership(t)
	q, _ := testReceiveDeliveryLedger(t, 8)
	q.maxBytes = 16 * 1024
	flowA, _ := ipPacketFlowKeyFromPath(icmpTcpTestPath(4))
	flowB := flowA
	flowB.sourcePort++
	capacity := make(chan struct{})
	ready := false
	var attempts [4]int
	var releases [4]int
	for index, flow := range []ipPacketFlowKey{flowA, flowA, flowB, flowA} {
		r, ok := q.append(testReceiveDeliveryItem(uint64(index)), true)
		if !ok {
			t.Fatal("bounded fixture admission")
		}
		r.operation(flow, index == 3, func() <-chan struct{} { return capacity }, func() receiveDeliveryAttempt {
			attempts[index]++
			if index == 0 && !ready {
				return receiveDeliveryWaiting
			}
			return receiveDeliverySecured
		}, func() { releases[index]++ })
		r.seal()
	}
	wakes := q.pump()
	if attempts != [4]int{1, 0, 1, 1} || len(wakes) != 1 || wakes[0] != capacity {
		t.Fatalf("same-flow order/control/other-flow progress=%v wakes=%d", attempts, len(wakes))
	}
	if present, _ := reliableIngressCumulativeHead(q.sequence); present {
		t.Fatal("independent progress cumulatively ACKed an earlier blocked packet")
	}
	// No worker/timer is hidden in the scheduler. With no capacity signal it
	// performs no repeated admission attempts while the queue is still full.
	if attempts[0] != 1 {
		t.Fatal("full queue spun without a wake")
	}
	ready = true
	close(capacity)
	q.pump()
	if attempts != [4]int{2, 1, 1, 1} || releases != [4]int{1, 1, 1, 1} || len(q.operations) != 0 || len(q.items) != 0 || q.bytes != 0 {
		t.Fatalf("capacity release changed order or ownership: attempts=%v releases=%v", attempts, releases)
	}
}

func TestReceiveDeliveryRetrySubscribesBeforeCapacityCheck(t *testing.T) {
	assertMessagePoolOwnership(t)
	q, _ := testReceiveDeliveryLedger(t, 1)
	r, _ := q.append(testReceiveDeliveryItem(0), true)
	capacity := make(chan struct{})
	subscribed := false
	attempts := 0
	r.operation(ipPacketFlowKey{}, false, func() <-chan struct{} {
		subscribed = true
		return capacity
	}, func() receiveDeliveryAttempt {
		attempts++
		if !subscribed {
			t.Error("capacity check happened before notification subscription")
		}
		if attempts == 1 {
			// Capacity changed just as an earlier nonblocking offer failed.
			// Its already-subscribed wake must survive that race.
			close(capacity)
			return receiveDeliveryWaiting
		}
		return receiveDeliverySecured
	}, nil)
	r.seal()
	wakes := q.pump()
	if len(wakes) != 1 {
		t.Fatal("failed offer lost its subscribed notification")
	}
	select {
	case <-wakes[0]:
	default:
		t.Fatal("capacity release between subscribe and offer was lost")
	}
	q.pump()
	if attempts != 2 || len(q.items) != 0 {
		t.Fatal("racing capacity edge failed to release the original owner")
	}
}

func TestReceiveDeliveryRetryKeepsOneOwnerAcrossNatAndTcp(t *testing.T) {
	assertMessagePoolOwnership(t)
	q, _ := testReceiveDeliveryLedger(t, 1)
	r, _ := q.append(testReceiveDeliveryItem(0), true)
	natAttempts, tcpAttempts, releases := 0, 0, 0
	op := r.operation(ipPacketFlowKey{}, false, nil, func() receiveDeliveryAttempt {
		natAttempts++
		return receiveDeliveryInFlight
	}, func() { releases++ })
	r.seal()
	q.pump()
	for range 20 {
		q.notify()
		q.pump()
	}
	if natAttempts != 1 || r.pending != 1 || releases != 0 {
		t.Fatal("capacity wakes duplicated an in-flight NAT owner")
	}
	op.retry(nil, func() receiveDeliveryAttempt {
		tcpAttempts++
		return receiveDeliverySecured
	})
	q.pump()
	if natAttempts != 1 || tcpAttempts != 1 || releases != 1 || len(q.items) != 0 || len(q.operations) != 0 {
		t.Fatal("final TCP retry lost or duplicated its original owner")
	}
}

func TestReceiveDeliveryRetryCancelRacesWithWakeAndFinalCompletion(t *testing.T) {
	for _, inFlight := range []bool{false, true} {
		t.Run(map[bool]string{false: "waiting", true: "in-flight"}[inFlight], func(t *testing.T) {
			assertMessagePoolOwnership(t)
			q, _ := testReceiveDeliveryLedger(t, 1)
			r, _ := q.append(testReceiveDeliveryItem(0), true)
			var releases atomic.Int64
			op := r.operation(ipPacketFlowKey{}, false, nil, func() receiveDeliveryAttempt {
				if inFlight {
					return receiveDeliveryInFlight
				}
				return receiveDeliveryWaiting
			}, func() { releases.Add(1) })
			r.seal()
			q.pump()
			q.cancel()
			if inFlight && releases.Load() != 0 {
				t.Fatal("cancel released a packet still owned by the NAT queue")
			}
			var workers sync.WaitGroup
			for range 20 {
				workers.Go(func() { q.notify(); q.cancel() })
				workers.Go(func() { op.complete(true) })
			}
			workers.Wait()
			if releases.Load() != 1 || q.bytes != 0 || len(q.items) != 0 || len(q.operations) != 0 || q.sequence.ackWindow.Pending() {
				t.Fatal("cancel/wake/completion race leaked or falsely ACKed the original")
			}
		})
	}
}

func TestReceiveDeliveryRetryAllowsSynchronousCompletion(t *testing.T) {
	assertMessagePoolOwnership(t)
	q, _ := testReceiveDeliveryLedger(t, 1)
	r, _ := q.append(testReceiveDeliveryItem(0), true)
	releases := 0
	var op *receiveDeliveryOperation
	op = r.operation(ipPacketFlowKey{}, false, nil, func() receiveDeliveryAttempt {
		// Fail cleanly on the old implementation before demonstrating the
		// actual reentrant callback; do not hang the test in that mutex.
		if !op.mutex.TryLock() {
			t.Error("attempt runs under operation mutex: synchronous completion would deadlock")
			return receiveDeliveryWaiting
		}
		op.mutex.Unlock()
		op.complete(true)
		if releases != 0 || q.sequence.ackWindow.Pending() {
			t.Error("synchronous completion returned bytes still borrowed by attempt")
		}
		return receiveDeliveryInFlight
	}, func() { releases++ })
	r.seal()
	q.pump()
	if releases != 1 || len(q.operations) != 0 || len(q.items) != 0 || !q.sequence.ackWindow.Pending() {
		t.Error("synchronous completion lost its final secured owner")
	}
}

func TestReceiveDeliveryRetryAllowsSynchronousRetry(t *testing.T) {
	assertMessagePoolOwnership(t)
	q, _ := testReceiveDeliveryLedger(t, 1)
	r, _ := q.append(testReceiveDeliveryItem(0), true)
	natAttempts, tcpAttempts := 0, 0
	var op *receiveDeliveryOperation
	op = r.operation(ipPacketFlowKey{}, false, nil, func() receiveDeliveryAttempt {
		natAttempts++
		if !op.mutex.TryLock() {
			t.Error("attempt runs under operation mutex: synchronous retry would deadlock")
			return receiveDeliveryWaiting
		}
		op.mutex.Unlock()
		op.retry(nil, func() receiveDeliveryAttempt {
			tcpAttempts++
			return receiveDeliverySecured
		})
		// The old admission result must not overwrite the newly installed
		// final-queue retry edge when this stack returns.
		return receiveDeliveryInFlight
	}, nil)
	r.seal()
	q.pump()
	q.pump()
	if natAttempts != 1 || tcpAttempts != 1 || len(q.operations) != 0 || len(q.items) != 0 {
		t.Error("synchronous retry was overwritten by the earlier attempt result")
	}
}
