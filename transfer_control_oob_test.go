package connect

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/connect/protocol"
)

// testOobControl is a mock OutOfBandControl: it fails the first `failUntil`
// SendControl attempts and then succeeds, recording the number of attempts. It
// mirrors ApiOutOfBandControl by consuming the frames it is given and invoking
// the callback asynchronously.
type testOobControl struct {
	stateLock sync.Mutex
	attempts  int
	failUntil int
}

func (self *testOobControl) setFailUntil(n int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.failUntil = n
}

func (self *testOobControl) attemptCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.attempts
}

func (self *testOobControl) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	// mimic ApiOutOfBandControl: the input frames are consumed
	for _, frame := range frames {
		MessagePoolReturn(frame.MessageBytes)
	}

	self.stateLock.Lock()
	self.attempts += 1
	attempt := self.attempts
	failUntil := self.failUntil
	self.stateLock.Unlock()

	go func() {
		if attempt <= failUntil {
			callback(nil, fmt.Errorf("simulated oob failure (attempt %d)", attempt))
		} else {
			// success: the oob returns (the platform processed the message)
			callback(nil, nil)
		}
	}()
}

func newControlSyncOobTestClient(ctx context.Context, oob OutOfBandControl) *Client {
	settings := DefaultClientSettings()
	// the oob path does not use the transport; disable the control ping so the
	// client does not spin trying to ping without a transport
	settings.ControlPingTimeout = 0
	return NewClient(ctx, NewId(), oob, settings)
}

// the oob ack fires once after the platform succeeds, retrying through
// transient failures
func TestControlSyncOobRetryUntilSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testing in short mode")
	}

	makeFrame := func(index uint32) *protocol.Frame {
		frame, err := ToFrame(&protocol.SimpleMessage{MessageIndex: index}, DefaultProtocolVersion)
		assert.Equal(t, err, nil)
		return frame
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	oob := &testOobControl{failUntil: 3}
	client := newControlSyncOobTestClient(ctx, oob)

	cs := NewControlSyncOob(ctx, client, "m1")
	cs.retryTimeout = 10 * time.Millisecond
	defer cs.Close()

	var ackLock sync.Mutex
	ackCount := 0
	ackCh := make(chan error, 1)
	cs.Send(makeFrame(1), func(err error) {
		ackLock.Lock()
		ackCount += 1
		ackLock.Unlock()
		select {
		case ackCh <- err:
		default:
		}
	})

	select {
	case err := <-ackCh:
		assert.Equal(t, err, nil)
	case <-time.After(10 * time.Second):
		t.Fatal("oob ack did not fire")
	}

	// 3 failures + 1 success
	assert.Equal(t, oob.attemptCount(), 4)

	// the ack fires exactly once
	select {
	case <-time.After(500 * time.Millisecond):
	}
	ackLock.Lock()
	assert.Equal(t, ackCount, 1)
	ackLock.Unlock()
}

// immediate success acks without retrying
func TestControlSyncOobImmediateSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testing in short mode")
	}

	makeFrame := func(index uint32) *protocol.Frame {
		frame, err := ToFrame(&protocol.SimpleMessage{MessageIndex: index}, DefaultProtocolVersion)
		assert.Equal(t, err, nil)
		return frame
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	oob := &testOobControl{failUntil: 0}
	client := newControlSyncOobTestClient(ctx, oob)

	cs := NewControlSyncOob(ctx, client, "m1")
	cs.retryTimeout = 10 * time.Millisecond
	defer cs.Close()

	ackCh := make(chan error, 1)
	cs.Send(makeFrame(1), func(err error) {
		select {
		case ackCh <- err:
		default:
		}
	})

	select {
	case err := <-ackCh:
		assert.Equal(t, err, nil)
	case <-time.After(5 * time.Second):
		t.Fatal("oob ack did not fire")
	}
	assert.Equal(t, oob.attemptCount(), 1)
}

// a newer Send for the same scope supersedes an older one still retrying
func TestControlSyncOobLatestSupersedes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testing in short mode")
	}

	makeFrame := func(index uint32) *protocol.Frame {
		frame, err := ToFrame(&protocol.SimpleMessage{MessageIndex: index}, DefaultProtocolVersion)
		assert.Equal(t, err, nil)
		return frame
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// keep failing so neither send completes until allowed
	oob := &testOobControl{failUntil: 1 << 30}
	client := newControlSyncOobTestClient(ctx, oob)

	cs := NewControlSyncOob(ctx, client, "m1")
	cs.retryTimeout = 10 * time.Millisecond
	defer cs.Close()

	firstAcked := make(chan struct{}, 1)
	cs.Send(makeFrame(1), func(err error) {
		select {
		case firstAcked <- struct{}{}:
		default:
		}
	})

	// let the first send fail a few times
	select {
	case <-time.After(100 * time.Millisecond):
	}

	// supersede with a newer send (still failing), then allow success — so
	// the first send is superseded while it can never succeed
	secondAcked := make(chan error, 1)
	cs.Send(makeFrame(2), func(err error) {
		select {
		case secondAcked <- err:
		default:
		}
	})
	oob.setFailUntil(0)

	select {
	case err := <-secondAcked:
		assert.Equal(t, err, nil)
	case <-time.After(5 * time.Second):
		t.Fatal("second oob ack did not fire")
	}

	// the superseded first send must not ack
	select {
	case <-firstAcked:
		t.Fatal("superseded send unexpectedly acked")
	case <-time.After(200 * time.Millisecond):
	}
}

// Close stops retrying
func TestControlSyncOobCloseStopsRetries(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testing in short mode")
	}

	makeFrame := func(index uint32) *protocol.Frame {
		frame, err := ToFrame(&protocol.SimpleMessage{MessageIndex: index}, DefaultProtocolVersion)
		assert.Equal(t, err, nil)
		return frame
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	oob := &testOobControl{failUntil: 1 << 30} // always fail
	client := newControlSyncOobTestClient(ctx, oob)

	cs := NewControlSyncOob(ctx, client, "m1")
	cs.retryTimeout = 10 * time.Millisecond

	acked := make(chan struct{}, 1)
	cs.Send(makeFrame(1), func(err error) {
		select {
		case acked <- struct{}{}:
		default:
		}
	})

	// let it retry a few times
	select {
	case <-time.After(100 * time.Millisecond):
	}
	attemptsBefore := oob.attemptCount()
	assert.Equal(t, 0 < attemptsBefore, true)

	cs.Close()

	select {
	case <-time.After(300 * time.Millisecond):
	}
	// retries stop promptly (allow a couple of in-flight attempts at close,
	// but not the ~30 that would accrue over the wait if it kept retrying)
	attemptsAfter := oob.attemptCount()
	assert.Equal(t, attemptsAfter <= attemptsBefore+2, true)

	// it never acked (always failing)
	select {
	case <-acked:
		t.Fatal("send acked despite always failing")
	default:
	}
}
