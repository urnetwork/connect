//go:build darwin || ios || linux || android

// The readiness poller's shards are separate goroutines that pause and resume
// their flows' return reads on the same provider credit, so the order in
// which a freed credit reaches them decides each flow's share.
package connect

import (
	"bytes"
	"context"
	"net"
	"os"
	"runtime"
	"slices"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
)

// Level-triggered readiness over test-owned datagram sockets, as with epoll:
// an armed descriptor is reported while it holds a datagram, again after each
// report, and a wake is reported once. Used only inside a synctest bubble.
type udpReturnReadOrderBackend struct {
	ctx    context.Context
	mutex  sync.Mutex
	armed  []int
	woken  bool
	signal chan struct{}
}

func newUdpReturnReadOrderBackend(ctx context.Context) *udpReturnReadOrderBackend {
	return &udpReturnReadOrderBackend{ctx: ctx, signal: make(chan struct{}, 1)}
}

func (self *udpReturnReadOrderBackend) notify() {
	select {
	case self.signal <- struct{}{}:
	default:
	}
}

func (self *udpReturnReadOrderBackend) add(fd int) error {
	func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		if !slices.Contains(self.armed, fd) {
			self.armed = append(self.armed, fd)
		}
	}()
	self.notify()
	return nil
}

func (self *udpReturnReadOrderBackend) remove(fd int) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.armed = slices.DeleteFunc(self.armed, func(armed int) bool { return armed == fd })
}

func (self *udpReturnReadOrderBackend) wake() {
	func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		self.woken = true
	}()
	self.notify()
}

func (self *udpReturnReadOrderBackend) wait(events []udpSocketPollEvent) (int, error) {
	for {
		count := 0
		func() {
			self.mutex.Lock()
			defer self.mutex.Unlock()
			if self.woken {
				self.woken = false
				events[count] = udpSocketPollEvent{fd: -1}
				count += 1
			}
			for _, fd := range self.armed {
				var peek [1]byte
				if count < len(events) {
					if _, _, err := syscall.Recvfrom(fd, peek[:], syscall.MSG_PEEK|syscall.MSG_DONTWAIT); err == nil {
						events[count] = udpSocketPollEvent{fd: fd}
						count += 1
					}
				}
			}
		}()
		if count != 0 {
			return count, nil
		}
		select {
		case <-self.ctx.Done():
			return 0, self.ctx.Err()
		case <-self.signal:
		}
	}
}

func (*udpReturnReadOrderBackend) close() error { return nil }

// Three flows read by two poller shards, flows 0 and 2 on the first and flow 1
// on the second, each socket always holding another datagram, wait for credit
// that the test frees one acknowledgement at a time. Each flow must get an
// equal share. A shard pauses a flow refused credit and resumes its paused
// flows in arrival order, but the shards themselves raced for a freed credit
// after every capacity edge woke them all.
func TestUdpReturnReadPollerShardsShareScarceProviderCreditOnOneProcessor(t *testing.T) {
	assertMessagePoolOwnership(t)
	// Keep the process-global statistics worker outside the bubble.
	MessagePoolReturn(MessagePoolGet(1))
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	synctest.Test(t, func(t *testing.T) {
		const flowCount = 3
		const shardCount = 2
		const ackCount = 10 * flowCount
		order := newUdpReturnReadOrderFixture(t, flowCount, nil)
		shardCtx, cancelShards := context.WithCancel(order.ctx)
		poller := &udpSocketReadPoller{}
		var backends []*udpReturnReadOrderBackend
		for range shardCount {
			backend := newUdpReturnReadOrderBackend(shardCtx)
			backends = append(backends, backend)
			poller.shards = append(poller.shards, udpSocketReadPollShard{
				ctx: shardCtx, backend: backend, readBuffer: make([]byte, udpReturnReadOrderPayloadByteCount),
				byFd: map[int]*udpSocketReadRegistration{},
			})
		}
		// Datagram socket pairs queue every write before it returns, so the
		// test alone decides what each flow's socket holds.
		payload := bytes.Repeat([]byte{0x5a}, udpReturnReadOrderPayloadByteCount)
		var writeFds []int
		refill := func() {
			for _, fd := range writeFds {
				for {
					if _, err := syscall.Write(fd, payload); err != nil {
						break
					}
				}
			}
			for _, backend := range backends {
				backend.notify()
			}
		}
		for index, sequence := range order.sequences {
			fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := syscall.SetNonblock(fds[1], true); err != nil {
				t.Fatal(err)
			}
			writeFds = append(writeFds, fds[1])
			readFile := os.NewFile(uintptr(fds[0]), "udp-return-flow")
			socket, err := net.FileConn(readFile)
			readFile.Close()
			if err != nil {
				t.Fatal(err)
			}
			sequence.socketReadPoller = poller
			sequence.receiveShard = index % shardCount
			sequence.sharedSocket = socket
			if !poller.register(sequence, socket) {
				t.Fatal("register")
			}
		}
		t.Cleanup(func() {
			for _, fd := range writeFds {
				syscall.Close(fd)
			}
		})
		refill()
		var shards sync.WaitGroup
		t.Cleanup(func() {
			cancelShards()
			shards.Wait()
		})
		// Shards start, and so their flows first read and wait, in index order.
		for index := range poller.shards {
			shards.Go(poller.shards[index].run)
			synctest.Wait()
		}

		if flows := order.takeWritten(); len(flows) != 2 {
			t.Fatalf("readers filled the two-datagram budget with %v", flows)
		}
		var flowOrder []int
		writtenCounts := make([]int, flowCount)
		for range ackCount {
			refill()
			order.acknowledgeOldest()
			for _, index := range order.takeWritten() {
				flowOrder = append(flowOrder, index)
				writtenCounts[index] += 1
			}
		}
		t.Logf("flows written as credit freed: %v", flowOrder)
		for index, count := range writtenCounts {
			if count != ackCount/flowCount {
				t.Errorf("flow %d got %d of %d freed credits, want %d", index, count, ackCount, ackCount/flowCount)
			}
		}
	})
}
