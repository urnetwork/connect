// Deadline-install errors terminate the exact user-NAT owner before dependent
// socket I/O; ordinary positive-progress timeout policy remains unchanged.
package connect

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Reads wait for explicit teardown and writes succeed despite rejected bounds.
// This separates an ignored setter from an unrelated downstream socket error.
type ipDeadlineConn struct {
	net.Conn
	readErr   error
	writeErr  error
	reads     atomic.Int64
	writes    atomic.Int64
	closed    chan struct{}
	closeOnce sync.Once
}

// No origin payload is needed to prove whether a read was attempted.
func (self *ipDeadlineConn) Read([]byte) (int, error) {
	self.reads.Add(1)
	<-self.closed
	return 0, net.ErrClosed
}

// The old source writes successfully after the injected installation error.
func (self *ipDeadlineConn) Write(message []byte) (int, error) {
	self.writes.Add(1)
	return len(message), nil
}

// Closes without a writer lock and joins no child worker.
func (self *ipDeadlineConn) Close() error {
	self.closeOnce.Do(func() { close(self.closed) })
	return nil
}

// These flow owners use directional deadline operations.
func (self *ipDeadlineConn) SetReadDeadline(time.Time) error { return self.readErr }

// A rejected installation does not manufacture a downstream I/O error.
func (self *ipDeadlineConn) SetWriteDeadline(time.Time) error { return self.writeErr }

// The test exercises the actual dedicated-socket UDP Run branch, not the
// independently checked shared-socket writer/poller branch.
func newIpDeadlineUdpSequence(t *testing.T, conn *ipDeadlineConn) *UdpSequence {
	t.Helper()
	settings := DefaultUdpBufferSettings()
	settings.Log = NewNoopLogger()
	settings.SharedSocketLifecycle = false
	settings.SequenceBufferSize = 4
	settings.WriteBatchSize = 4
	settings.IdleTimeout = time.Hour
	settings.DialContextSettings = &DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) { return conn, nil }}
	sequence := NewUdpSequence(context.Background(), func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {}, SourceId(NewId()), protocol.ProvideMode_Network, 4,
		net.IPv4(192, 0, 2, 10).To4(), 40001, net.IPv4(203, 0, 113, 20).To4(), 443, settings)
	if sequence == nil {
		t.Fatal("synthetic UDP budget refused")
	}
	return sequence
}

// Enqueues real pool-owned datagrams before Run so one ready batch is certain.
func queueIpDeadlineUdpItems(t *testing.T, sequence *UdpSequence) [][]byte {
	t.Helper()
	witnesses := make([][]byte, 0, 2)
	for _, size := range []int{37, 53} {
		packet := MessagePoolGet(size)
		witness := MessagePoolShareReadOnly(packet)
		item := &UdpSendItem{source: sequence.source, provideMode: protocol.ProvideMode_Network, udp: parsedUdp{payload: packet}, ipPacket: packet}
		if ok, err := sequence.send(item, 0); !ok || err != nil {
			item.release()
			MessagePoolReturn(witness)
			t.Fatalf("synthetic UDP enqueue: %v", err)
		}
		witnesses = append(witnesses, witness)
	}
	return witnesses
}

// A rejected read bound cancels the owner and joins the idle sender immediately.
func TestIpDeadlineUdpReadRejectionRetiresOwner(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		conn := &ipDeadlineConn{readErr: errors.New("synthetic UDP read deadline rejected"), closed: make(chan struct{})}
		sequence := newIpDeadlineUdpSequence(t, conn)
		done := make(chan struct{})
		go func() { defer close(done); sequence.Run() }()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Error("UDP owner retained after rejected read deadline")
		}
		if conn.reads.Load() != 0 {
			t.Error("UDP read started after deadline rejection")
		}
		sequence.Cancel()
		conn.Close()
		<-done
	})
}

// Every gathered datagram remains owned until released even on the early exit.
func TestIpDeadlineUdpWriteRejectionReturnsGatheredOwners(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		conn := &ipDeadlineConn{writeErr: errors.New("synthetic UDP write deadline rejected"), closed: make(chan struct{})}
		sequence := newIpDeadlineUdpSequence(t, conn)
		witnesses := queueIpDeadlineUdpItems(t, sequence)
		done := make(chan struct{})
		go func() { defer close(done); sequence.Run() }()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Error("UDP owner retained after rejected batch deadline")
		}
		if conn.writes.Load() != 0 {
			t.Error("UDP payload escaped rejected batch deadline")
		}
		sequence.Cancel()
		conn.Close()
		<-done
		for _, witness := range witnesses {
			if !MessagePoolReturn(witness) {
				t.Error("UDP batch retained packet ownership")
			}
		}
	})
}

// Nil deadline errors preserve both datagrams and ordinary idle lifecycle.
func TestIpDeadlineUdpHealthyBatchKeepsOwner(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		conn := &ipDeadlineConn{closed: make(chan struct{})}
		sequence := newIpDeadlineUdpSequence(t, conn)
		witnesses := queueIpDeadlineUdpItems(t, sequence)
		done := make(chan struct{})
		go func() { defer close(done); sequence.Run() }()
		synctest.Wait()
		select {
		case <-done:
			t.Error("healthy UDP owner retired before cancellation")
		default:
		}
		if conn.writes.Load() != 2 || conn.reads.Load() != 1 {
			t.Errorf("healthy UDP writes=%d reads=%d", conn.writes.Load(), conn.reads.Load())
		}
		sequence.Cancel()
		conn.Close()
		<-done
		for _, witness := range witnesses {
			if !MessagePoolReturn(witness) {
				t.Error("healthy UDP batch retained ownership")
			}
		}
	})
}

// Drives the actual SYN/dial/read-owner boundary with synthetic packet state.
func newIpDeadlineTcpSequence(t *testing.T, conn *ipDeadlineConn) *TcpSequence {
	t.Helper()
	settings := DefaultTcpBufferSettingsWithBufferSize(4)
	settings.Log = NewNoopLogger()
	settings.IdleTimeout = time.Hour
	settings.AckCompressTimeout = 0
	settings.WriteBatchSize = 1
	settings.DialContextSettings = &DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) { return conn, nil }}
	sequence := NewTcpSequence(context.Background(), func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {}, SourceId(NewId()), protocol.ProvideMode_Network, 4,
		net.IPv4(192, 0, 2, 11).To4(), 40002, net.IPv4(203, 0, 113, 21).To4(), 443, 1000, settings)
	if sequence == nil {
		t.Fatal("synthetic TCP budget refused")
	}
	item := &TcpSendItem{source: sequence.source, provideMode: protocol.ProvideMode_Network, tcp: parsedTcp{seq: 1000, syn: true, windowSize: 65535}, ipPacket: MessagePoolGet(40)}
	if ok, err := sequence.send(item, 0); !ok || err != nil {
		item.release()
		t.Fatalf("synthetic SYN enqueue: %v", err)
	}
	return sequence
}

// Rejection takes the existing reset/drain/cancel path, never an unbounded Read.
func TestIpDeadlineTcpReadRejectionRetiresOwner(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		conn := &ipDeadlineConn{readErr: errors.New("synthetic TCP read deadline rejected"), closed: make(chan struct{})}
		sequence := newIpDeadlineTcpSequence(t, conn)
		done := make(chan struct{})
		go func() { defer close(done); sequence.Run() }()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Error("TCP owner retained after rejected read deadline")
		}
		if conn.reads.Load() != 0 {
			t.Error("TCP read started after deadline rejection")
		}
		sequence.Cancel()
		conn.Close()
		<-done
	})
}

// A healthy blocked reader remains connected until explicit owner cancellation.
func TestIpDeadlineTcpHealthyReaderKeepsOwner(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		conn := &ipDeadlineConn{closed: make(chan struct{})}
		sequence := newIpDeadlineTcpSequence(t, conn)
		done := make(chan struct{})
		go func() { defer close(done); sequence.Run() }()
		synctest.Wait()
		select {
		case <-done:
			t.Error("healthy TCP owner retired before cancellation")
		default:
		}
		if conn.reads.Load() != 1 {
			t.Errorf("healthy TCP reads=%d", conn.reads.Load())
		}
		sequence.Cancel()
		conn.Close()
		<-done
	})
}

// Counts exact positive progress and can reject the first or a later rearm.
type ipDeadlineProgressConn struct {
	net.Conn
	want     error
	rejectAt int
	sets     int
	writes   int
	stalled  bool
}

// Every successful partial write returns a timeout, requiring one fresh bound.
func (self *ipDeadlineProgressConn) Write(message []byte) (int, error) {
	self.writes++
	if self.stalled {
		return 0, writeTimeoutError{}
	}
	n := min(3, len(message))
	if n < len(message) {
		return n, writeTimeoutError{}
	}
	return n, nil
}

// Rejection must preserve all bytes accepted by earlier bounded writes only.
func (self *ipDeadlineProgressConn) SetWriteDeadline(time.Time) error {
	self.sets++
	if self.sets == self.rejectAt {
		return self.want
	}
	return nil
}

// The very first failed installation must prevent all upstream payload writes.
func TestIpDeadlineProgressInitialRejectionWritesNothing(t *testing.T) {
	conn := &ipDeadlineProgressConn{want: errors.New("synthetic initial bound rejected"), rejectAt: 1}
	n, err := writeWithProgressDeadline(conn, net.Buffers{make([]byte, 10)}, time.Second)
	if !errors.Is(err, conn.want) || n != 0 || conn.writes != 0 {
		t.Fatalf("initial bound result=%v bytes=%d writes=%d", err, n, conn.writes)
	}
}

// A failed rearm cannot erase the already accepted prefix or emit more bytes.
func TestIpDeadlineProgressRearmPreservesExactPrefix(t *testing.T) {
	conn := &ipDeadlineProgressConn{want: errors.New("synthetic rearm rejected"), rejectAt: 2}
	n, err := writeWithProgressDeadline(conn, net.Buffers{make([]byte, 10)}, time.Second)
	if !errors.Is(err, conn.want) || n != 3 || conn.writes != 1 {
		t.Fatalf("rearm result=%v bytes=%d writes=%d", err, n, conn.writes)
	}
}

// Nil installations retain the zero-progress, not whole-transfer, policy.
func TestIpDeadlineProgressHealthyPartialWritesContinue(t *testing.T) {
	conn := &ipDeadlineProgressConn{}
	n, err := writeWithProgressDeadline(conn, net.Buffers{make([]byte, 10)}, time.Second)
	if err != nil || n != 10 || conn.writes != 4 || conn.sets != 4 {
		t.Fatalf("healthy progress result=%v bytes=%d writes=%d sets=%d", err, n, conn.writes, conn.sets)
	}
}

// A true zero-progress timeout still terminates at the first write.
func TestIpDeadlineProgressHealthyStallStillFails(t *testing.T) {
	conn := &ipDeadlineProgressConn{stalled: true}
	n, err := writeWithProgressDeadline(conn, net.Buffers{make([]byte, 10)}, time.Second)
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() || n != 0 || conn.writes != 1 {
		t.Fatalf("stalled progress result=%v bytes=%d writes=%d", err, n, conn.writes)
	}
}
