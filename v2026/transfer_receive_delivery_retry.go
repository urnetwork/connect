package connect

import "sync"

type receiveDeliveryAttempt uint8

const (
	receiveDeliveryWaiting receiveDeliveryAttempt = iota
	receiveDeliveryInFlight
	receiveDeliverySecured
	receiveDeliveryRejected
)

// One original packet has one operation, even after NAT queue admission and
// a refused final TCP queue. Neither a wake nor a duplicate Pack may enqueue
// a second copy of an in-flight owner. Only the ReceiveSequence worker calls
// pump; downstream workers may complete or return an operation to waiting.
type receiveDeliveryOperation struct {
	mutex             sync.Mutex
	claim             *receiveDeliveryClaim
	flow              ipPacketFlowKey
	control           bool
	state             receiveDeliveryAttempt
	terminal          bool
	canceled          bool
	attempting        bool
	generation        uint64
	completionPending bool
	completionSecured bool
	subscribe         func() <-chan struct{}
	attempt           func() receiveDeliveryAttempt
	release           func()
	contexts          []<-chan struct{}
	budget            *TransferMemoryBudget
}

func (r *receiveDeliveryReceipt) operation(flow ipPacketFlowKey, control bool,
	subscribe func() <-chan struct{}, attempt func() receiveDeliveryAttempt, release func(),
) *receiveDeliveryOperation {
	claim := r.hold()
	if claim == nil {
		return nil
	}
	op := &receiveDeliveryOperation{claim: claim, flow: flow, control: control,
		subscribe: subscribe, attempt: attempt, release: release}
	q := r.queue
	q.mutex.Lock()
	q.operations = append(q.operations, op)
	closed := q.closed
	q.mutex.Unlock()
	if closed {
		op.cancel()
	} else {
		q.notify()
	}
	return op
}

func (q *receiveDeliveryQueue) notify() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Subscribe before testing capacity. A release racing the failed attempt
// then either admits it or closes the returned channel; no wake is lost.
// The existing sequence event loop waits on these capacity sources plus wake
// and cancellation. This helper creates no timer or goroutine.
func (q *receiveDeliveryQueue) pump() []<-chan struct{} {
	q.mutex.Lock()
	if q.closed {
		q.mutex.Unlock()
		return nil
	}
	operations := append([]*receiveDeliveryOperation(nil), q.operations...)
	q.mutex.Unlock()
	var wakes []<-chan struct{}
	for index, op := range operations {
		q.mutex.Lock()
		closed := q.closed
		q.mutex.Unlock()
		if closed {
			break
		}
		for _, done := range op.contexts {
			select {
			case <-done:
				op.cancel()
			default:
				wakes = append(wakes, done)
			}
		}
		if op.budget != nil {
			wakes = append(wakes, op.budget.CapacityNotify())
		}
		blocked := false
		if !op.control {
			for _, earlier := range operations[:index] {
				if earlier.flow != op.flow || earlier.control {
					continue
				}
				earlier.mutex.Lock()
				blocked = !earlier.terminal
				earlier.mutex.Unlock()
				if blocked {
					break
				}
			}
		}
		if blocked {
			continue
		}
		wake, result := op.tryOnce()
		if result == receiveDeliveryWaiting && wake != nil {
			wakes = append(wakes, wake)
		}
	}
	q.pruneOperations()
	return wakes
}

func (op *receiveDeliveryOperation) tryOnce() (<-chan struct{}, receiveDeliveryAttempt) {
	op.mutex.Lock()
	if op.terminal || op.canceled || op.attempting || op.state == receiveDeliveryInFlight {
		op.mutex.Unlock()
		return nil, receiveDeliveryInFlight
	}
	// Reserve one execution/owner before calling external admission code.
	// Cancellation can mark it but cannot release its borrowed bytes until
	// that stack returns. Both callbacks are allowed to complete/retry inline.
	op.attempting, op.state = true, receiveDeliveryInFlight
	op.generation++
	generation, subscribe, attempt := op.generation, op.subscribe, op.attempt
	op.mutex.Unlock()
	var wake <-chan struct{}
	if subscribe != nil {
		wake = subscribe()
	}
	op.mutex.Lock()
	run := !op.canceled && !op.completionPending && generation == op.generation
	op.mutex.Unlock()
	result := receiveDeliveryRejected
	if run {
		result = attempt()
	}
	op.mutex.Lock()
	op.attempting = false
	if op.completionPending {
		secured := op.completionSecured
		op.completionPending = false
		op.mutex.Unlock()
		op.complete(secured)
		return nil, receiveDeliveryInFlight
	}
	if op.generation != generation {
		// An inline retry installed the next edge. Ignore this older return.
		canceled := op.canceled
		op.mutex.Unlock()
		if canceled {
			op.complete(false)
		}
		return nil, receiveDeliveryInFlight
	}
	op.state = result
	canceled := op.canceled
	op.mutex.Unlock()
	if result == receiveDeliverySecured || result == receiveDeliveryRejected || canceled && result != receiveDeliveryInFlight {
		op.complete(result == receiveDeliverySecured && !canceled)
	}
	return wake, result
}

func (op *receiveDeliveryOperation) complete(secured bool) {
	op.mutex.Lock()
	if op.terminal || op.completionPending {
		op.mutex.Unlock()
		return
	}
	if op.attempting {
		op.completionPending, op.completionSecured = true, secured
		op.mutex.Unlock()
		return
	}
	op.terminal = true
	secured = secured && !op.canceled
	release := op.release
	op.release, op.attempt, op.subscribe = nil, nil, nil
	op.mutex.Unlock()
	if release != nil {
		release()
	}
	op.claim.complete(secured)
	op.claim.receipt.queue.notify()
	op.claim.receipt.queue.pruneOperations()
}

// A downstream queue still owns in-flight bytes during cancellation. It
// must dispose of that exact owner and call complete; cancel cannot race it
// by releasing the same packet/credit itself.
func (op *receiveDeliveryOperation) cancel() {
	op.mutex.Lock()
	op.canceled = true
	inFlight := op.state == receiveDeliveryInFlight || op.attempting
	op.mutex.Unlock()
	if !inFlight {
		op.complete(false)
	}
}

// Final admission refused after NAT admission. Keep the original operation,
// replace only its next admission edge, and wake its sequence worker.
func (op *receiveDeliveryOperation) retry(subscribe func() <-chan struct{}, attempt func() receiveDeliveryAttempt) {
	op.mutex.Lock()
	if op.terminal || op.completionPending {
		op.mutex.Unlock()
		return
	}
	op.generation++
	op.state, op.subscribe, op.attempt = receiveDeliveryWaiting, subscribe, attempt
	canceled := op.canceled && !op.attempting
	op.mutex.Unlock()
	if canceled {
		op.complete(false)
	} else {
		op.claim.receipt.queue.notify()
	}
}

func (q *receiveDeliveryQueue) pruneOperations() {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	remaining := q.operations[:0]
	for _, op := range q.operations {
		op.mutex.Lock()
		terminal := op.terminal
		op.mutex.Unlock()
		if !terminal {
			remaining = append(remaining, op)
		}
	}
	clear(q.operations[len(remaining):])
	q.operations = remaining
	if len(q.operations) == 0 {
		q.operations = nil
	}
}
