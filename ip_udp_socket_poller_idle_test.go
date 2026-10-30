// The readiness shards wait in the kernel with no timeout: an idle shard does
// not wake, and cancellation still ends it through the backend's wake.
package connect

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// A shard with no flows, or only idle ones, must sleep in the kernel. A
// timed wait woke every shard ten times a second for the provider's life:
// 20-80 wakes a second on a phone that is providing nothing.
func TestUdpSocketPollShardsDoNotWakeWhileIdle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultUdpBufferSettingsWithBufferSize(1)
	settings.SocketReadShardCount = 2
	wakes := atomic.Int64{}
	settings.afterSocketPollWaitForTest = func() {
		wakes.Add(1)
	}
	poller := newUdpSocketReadPoller(ctx, settings)
	if poller == nil {
		t.Skip("socket readiness poller unavailable on this platform")
	}
	idle := 350 * time.Millisecond
	time.Sleep(idle)
	if idleWakes := wakes.Load(); idleWakes != 0 {
		t.Errorf("%d idle readiness shards woke %d times in %s with no flow and no wake", len(poller.shards), idleWakes, idle)
	}

	cancel()
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		poller.waitForLifecycle()
	}()
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled readiness shards did not stop")
	}
}

// Waits only for readiness or a wake, as the kernel backends now do. Nothing
// times out, so a shard that relied on a periodic return to notice its
// cancellation would wait forever.
type udpPollWakeOnlyBackend struct {
	wakes  chan struct{}
	waits  atomic.Int64
	closed atomic.Bool
}

func newUdpPollWakeOnlyBackend() *udpPollWakeOnlyBackend {
	return &udpPollWakeOnlyBackend{wakes: make(chan struct{}, 1)}
}

func (self *udpPollWakeOnlyBackend) add(int) error { return nil }

func (self *udpPollWakeOnlyBackend) remove(int) {}

func (self *udpPollWakeOnlyBackend) wake() {
	select {
	case self.wakes <- struct{}{}:
	default:
	}
}

func (self *udpPollWakeOnlyBackend) wait(events []udpSocketPollEvent) (int, error) {
	self.waits.Add(1)
	<-self.wakes
	events[0] = udpSocketPollEvent{fd: -1}
	return 1, nil
}

func (self *udpPollWakeOnlyBackend) close() error {
	self.closed.Store(true)
	return nil
}

// Cancellation wakes the shard's untimed wait, and the shard closes its
// backend without treating that wake as a capacity resume.
func TestUdpSocketPollShardCancelWakesUntimedWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backend := newUdpPollWakeOnlyBackend()
	shard := &udpSocketReadPollShard{
		ctx:        ctx,
		backend:    backend,
		readBuffer: make([]byte, 64),
		byFd:       map[int]*udpSocketReadRegistration{},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		shard.run()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for backend.waits.Load() == 0 {
		if deadline.Before(time.Now()) {
			t.Fatal("the shard never waited for readiness")
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		// release the stranded shard so it does not outlive the test
		backend.wake()
		<-done
		t.Fatal("cancellation did not wake the shard's untimed readiness wait")
	}
	if !backend.closed.Load() {
		t.Fatal("the stopped shard did not close its backend")
	}
	if waits := backend.waits.Load(); waits != 1 {
		t.Fatalf("the shard waited %d times, want the one wait that cancellation ended", waits)
	}
}
