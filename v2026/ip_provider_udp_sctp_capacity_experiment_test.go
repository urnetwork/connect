//go:build !js

package connect

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// This test-only sweep measures the cold flight's outstanding-data requirement.
// More channel slots alone are not a production candidate: admission must also
// charge retained bytes and preserve the process budget before a larger queue
// can be considered. The low-level fixture includes every admitted identity and
// exact terminal refusals, so no loss metric changes when the depth changes.
func TestProviderUdpSctpColdFlightCapacityRequirement(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, carrierSlots := range []int{4, 32, 64, 128, 256} {
		t.Run(fmt.Sprintf("carrier-slots-%d", carrierSlots), func(t *testing.T) {
			result := runProviderUdpSctpCapacityExperiment(t, 120*time.Millisecond, false, false, carrierSlots)
			t.Logf("carrier_slots=%d %+v", carrierSlots, result)
			if result.retransmits != 0 {
				t.Fatalf("lossless capacity fixture retransmitted DATA: %+v", result)
			}
		})
	}
}

// Test-only compact backlog holds a hard number of pooled 8-KiB roots instead
// of rounding each ~1,134-byte datagram separately to a 2-KiB packet root.
// It adds a separate owner and worker, so its result cannot be called a
// memory-neutral production repair without composing admission upstream.
type udpSctpCompactQueueExperiment struct {
	net.Conn
	mutex       sync.Mutex
	cond        *sync.Cond
	slabs       []udpSctpCompactExperimentSlab
	head, count int
	closed      bool
	done        chan struct{}
}

type udpSctpCompactExperimentSlab struct {
	bytes         []byte
	read, written int
}

func newUdpSctpCompactQueueExperiment(conn net.Conn, byteLimit int) *udpSctpCompactQueueExperiment {
	q := &udpSctpCompactQueueExperiment{Conn: conn, slabs: make([]udpSctpCompactExperimentSlab, byteLimit/8192), done: make(chan struct{})}
	q.cond = sync.NewCond(&q.mutex)
	go q.run()
	return q
}

func (q *udpSctpCompactQueueExperiment) Write(wire []byte) (int, error) {
	if len(wire) > 8190 {
		return 0, io.ErrShortBuffer
	}
	q.mutex.Lock()
	defer q.mutex.Unlock()
	for !q.closed {
		var tail *udpSctpCompactExperimentSlab
		if q.count > 0 {
			tail = &q.slabs[(q.head+q.count-1)%len(q.slabs)]
		}
		if tail == nil || len(tail.bytes)-tail.written < len(wire)+2 {
			if q.count == len(q.slabs) {
				q.cond.Wait()
				continue
			}
			tail = &q.slabs[(q.head+q.count)%len(q.slabs)]
			*tail = udpSctpCompactExperimentSlab{bytes: MessagePoolGet(8192)}
			q.count++
		}
		binary.BigEndian.PutUint16(tail.bytes[tail.written:], uint16(len(wire)))
		copy(tail.bytes[tail.written+2:], wire)
		tail.written += len(wire) + 2
		q.cond.Broadcast()
		return len(wire), nil
	}
	return 0, net.ErrClosed
}

func (q *udpSctpCompactQueueExperiment) run() {
	defer close(q.done)
	defer func() {
		q.mutex.Lock()
		defer q.mutex.Unlock()
		q.closed = true
		for index := range q.slabs {
			MessagePoolReturn(q.slabs[index].bytes)
			q.slabs[index] = udpSctpCompactExperimentSlab{}
		}
		q.count = 0
		q.cond.Broadcast()
	}()
	for {
		q.mutex.Lock()
		for q.count == 0 && !q.closed {
			q.cond.Wait()
		}
		if q.closed {
			q.mutex.Unlock()
			return
		}
		head := &q.slabs[q.head]
		n := int(binary.BigEndian.Uint16(head.bytes[head.read:]))
		wire := head.bytes[head.read+2 : head.read+2+n]
		q.mutex.Unlock()
		written, err := q.Conn.Write(wire)
		q.mutex.Lock()
		head.read += n + 2
		if head.read == head.written {
			MessagePoolReturn(head.bytes)
			*head = udpSctpCompactExperimentSlab{}
			q.head = (q.head + 1) % len(q.slabs)
			q.count--
		}
		q.cond.Broadcast()
		q.mutex.Unlock()
		if err != nil || written != n {
			return
		}
	}
}

func (q *udpSctpCompactQueueExperiment) closeAndWait() {
	q.mutex.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mutex.Unlock()
	<-q.done
}

func TestProviderUdpSctpCompactColdFlightRequirement(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, byteLimit := range []int{128 * 1024, 192 * 1024, 256 * 1024} {
		t.Run(fmt.Sprintf("compact-kib-%d", byteLimit/1024), func(t *testing.T) {
			result := runProviderUdpSctpQueueExperiment(t, 120*time.Millisecond, false, false, 4, byteLimit)
			t.Logf("compact_byte_limit=%d %+v", byteLimit, result)
			if result.retransmits != 0 {
				t.Fatalf("lossless compact fixture retransmitted DATA: %+v", result)
			}
		})
	}
}
