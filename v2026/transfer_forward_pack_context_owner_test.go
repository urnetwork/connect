// A forwarding caller owns only its unadmitted frame. Its cancellation must
// preserve the shared destination worker and already-admitted sibling bytes.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// Pause real workers before Run so the bounded two-entry channel is the exact
// admission boundary; client cancellation releases every paused owner.
type forwardPackCallerOwnerFixture struct {
	client      *Client
	destination TransferPath
	sequence    *ForwardSequence
}

// All context waits are inside the explicit synctest clock. No socket or route
// must become available for admission, cancellation, or final ownership release.
func newForwardPackCallerOwnerFixture(t *testing.T) *forwardPackCallerOwnerFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := closeWaitClientSettings()
	settings.ForwardBufferSettings.SequenceBufferSize = 2
	settings.ForwardBufferSettings.beforeRunForwardSequenceForTest = func(TransferPath) { <-ctx.Done() }
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	fixture := &forwardPackCallerOwnerFixture{client: client, destination: DestinationId(NewId())}
	t.Cleanup(func() {
		cancel()
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Errorf("forward caller fixture close: %v", err)
		}
	})
	return fixture
}

// Read the destination owner under the same lock used for replacement.
func (self *forwardPackCallerOwnerFixture) currentSequence() *ForwardSequence {
	self.client.forwardBuffer.mutex.Lock()
	defer self.client.forwardBuffer.mutex.Unlock()
	return self.client.forwardBuffer.forwardSequences[self.destination]
}

// A true return transfers the pooled bytes to the real ForwardSequence.
func (self *forwardPackCallerOwnerFixture) send(ctx context.Context, value string, timeout time.Duration) (bool, error) {
	bytes := MessagePoolCopy([]byte(value))
	accepted, err := self.client.forwardBuffer.Pack(&ForwardPack{
		Destination: self.destination, TransferFrameBytes: bytes, Ctx: ctx,
	}, timeout)
	if !accepted {
		MessagePoolReturn(bytes)
	}
	return accepted, err
}

// Retain two accepted sibling frames before the attempted canceled caller.
func (self *forwardPackCallerOwnerFixture) fill(t *testing.T) {
	t.Helper()
	for _, value := range []string{"accepted-first", "accepted-second"} {
		if accepted, err := self.send(self.client.ctx, value, 0); !accepted || err != nil {
			t.Fatalf("forward sibling admission: %t %v", accepted, err)
		}
	}
	self.sequence = self.currentSequence()
	if self.sequence == nil || len(self.sequence.packs) != 2 {
		t.Fatal("forward fixture did not occupy its two admitted slots")
	}
}

// The original shared worker and both accepted buffers survive caller failure.
func (self *forwardPackCallerOwnerFixture) requireSiblings(t *testing.T) {
	t.Helper()
	if self.currentSequence() != self.sequence || self.sequence.ctx.Err() != nil {
		t.Error("canceled forwarding caller retired the healthy shared destination")
	}
	if len(self.sequence.packs) != 2 {
		t.Errorf("forwarded sibling ownership changed: %d", len(self.sequence.packs))
	}
	for _, expected := range []string{"accepted-first", "accepted-second"} {
		select {
		case pack := <-self.sequence.packs:
			if string(pack.TransferFrameBytes) != expected {
				t.Errorf("forward sibling bytes changed: %q", pack.TransferFrameBytes)
			}
			MessagePoolReturn(pack.TransferFrameBytes)
		default:
			t.Error("forward sibling disappeared before its consumer")
		}
	}
}

// Pre-canceled admission is not evidence that a live destination closed.
func TestForwardBufferCallerCancelPreservesSharedAdmission(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newForwardPackCallerOwnerFixture(t)
		fixture.fill(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		accepted, err := fixture.send(ctx, "canceled-unadmitted", 0)
		if accepted || err == nil {
			t.Errorf("canceled forwarding caller admission: %t %v", accepted, err)
		}
		fixture.requireSiblings(t)
	})
}

// A cancellation arriving while the real queue wait is blocked has the same
// caller-only ownership as cancellation before the first attempt.
func TestForwardBufferCallerCancelDuringAdmissionKeepsSiblings(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newForwardPackCallerOwnerFixture(t)
		fixture.fill(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		type result struct {
			accepted bool
			err      error
		}
		done := make(chan result, 1)
		go func() {
			accepted, err := fixture.send(ctx, "blocked-unadmitted", -1)
			done <- result{accepted: accepted, err: err}
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("full forward queue did not block this caller")
		default:
		}
		cancel()
		got := <-done
		if got.accepted || got.err == nil {
			t.Errorf("canceled blocked forwarding admission: %t %v", got.accepted, got.err)
		}
		fixture.requireSiblings(t)
	})
}

// A later caller cancellation cannot retract a frame already owned by the
// forwarding sequence, even when that caller tries another send.
func TestForwardBufferCallerCancelKeepsAcceptedPackOwned(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newForwardPackCallerOwnerFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		if accepted, err := fixture.send(ctx, "accepted-caller", 0); !accepted || err != nil {
			cancel()
			t.Fatalf("first forwarding caller was not admitted: %t %v", accepted, err)
		}
		original := fixture.currentSequence()
		cancel()
		accepted, err := fixture.send(ctx, "canceled-next", 0)
		if accepted || err == nil || fixture.currentSequence() != original || original.ctx.Err() != nil {
			t.Error("canceled next caller retired the admitted predecessor's owner")
		}
		select {
		case pack := <-original.packs:
			if string(pack.TransferFrameBytes) != "accepted-caller" {
				t.Error("admitted forwarding bytes changed")
			}
			MessagePoolReturn(pack.TransferFrameBytes)
		default:
			t.Error("admitted frame was reclaimed before its consumer")
		}
	})
}

// A genuinely closed destination still replaces its generation for a caller
// whose own context remains live; cancellation classification cannot pin it.
func TestForwardBufferClosedSequenceStillRecreatesForLiveCaller(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newForwardPackCallerOwnerFixture(t)
		fixture.fill(t)
		fixture.sequence.Cancel()
		accepted, err := fixture.send(fixture.client.ctx, "replacement-caller", 0)
		replacement := fixture.currentSequence()
		if !accepted || err != nil || replacement == nil || replacement == fixture.sequence {
			t.Fatalf("live forwarding caller failed closed-sequence replacement: %t %v", accepted, err)
		}
		select {
		case pack := <-replacement.packs:
			if string(pack.TransferFrameBytes) != "replacement-caller" {
				t.Error("replacement owns different forwarding bytes")
			}
			MessagePoolReturn(pack.TransferFrameBytes)
		default:
			t.Error("replacement admission lost caller ownership")
		}
	})
}
