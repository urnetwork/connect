package connect

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// The idle reservation is entered after Pack has acquired its producer lock.
// Observing it while the actual queue is full proves the first caller owns
// admission. No sleep or worker scheduling assumption supplies that ordering.
func waitForForwardProducerAdmission(t *testing.T, sequence *ForwardSequence) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		sequence.idleCondition.mutex.Lock()
		active := sequence.idleCondition.updateOpenCount
		sequence.idleCondition.mutex.Unlock()
		if active == 1 {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("first forwarding producer did not enter its queue wait")
}

// One infinite producer must not hide another caller's zero/finite timeout or
// cancellation. The two accepted sibling buffers and shared destination stay
// owned while the independent caller refuses admission.
func TestForwardSequenceBlockedProducerPreservesIndependentCaller(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	for _, test := range []struct {
		name    string
		timeout time.Duration
		cancel  bool
	}{
		{name: "zero-wait", timeout: 0},
		{name: "finite-wait", timeout: 25 * time.Millisecond},
		{name: "canceled", timeout: -1, cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newForwardPackCallerOwnerFixture(t)
			fixture.fill(t)
			firstCtx, cancelFirst := context.WithCancel(fixture.client.ctx)
			defer cancelFirst()
			firstDone := make(chan struct{})
			go func() {
				fixture.send(firstCtx, "held unadmitted producer", -1)
				close(firstDone)
			}()
			waitForForwardProducerAdmission(t, fixture.sequence)

			callerCtx, cancelCaller := context.WithCancel(fixture.client.ctx)
			defer cancelCaller()
			if test.cancel {
				cancelCaller()
			}
			type admissionResult struct {
				accepted bool
				err      error
			}
			secondDone := make(chan admissionResult, 1)
			go func() {
				accepted, err := fixture.send(callerCtx, "independent unadmitted caller", test.timeout)
				secondDone <- admissionResult{accepted, err}
			}()
			var got admissionResult
			coupled := false
			select {
			case got = <-secondDone:
			case <-time.After(time.Second):
				coupled = true
			}
			// Release every fixture owner before asserting, including RED.
			cancelFirst()
			<-firstDone
			if coupled {
				got = <-secondDone
			}
			fixture.requireSiblings(t)
			if coupled {
				t.Fatal("unrelated forwarding producer hid the caller timeout or cancellation")
			}
			if got.accepted || (got.err != nil) != test.cancel {
				t.Fatalf("independent forwarding admission: accepted=%t error=%v", got.accepted, got.err)
			}
		})
	}
}
