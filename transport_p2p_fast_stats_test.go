//go:build !js

// Final carrier counters belong to route workers, not to the receiving test.
// These roots force the asynchronous publication that a delivered message
// alone cannot join.
package connect

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// A final route snapshot must join every producer that publishes its counters.
// These fixtures own both routes and connections. Callers must first stop
// their external route forwarders and consumers.
func snapshotP2pFastPathTestRoutes(t testing.TB, stats *P2pDataPlaneStats, routes ...Transport) P2pDataPlaneStatsSnapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, route := range routes {
		route.(P2pRouteLifecycle).Close()
	}
	// Finish accepted writes before closing their peers. A peerConn deadline
	// applies to the next Read, so its already-blocked SCTP reader is released
	// by closing the fixture-owned connection before joining that receive route.
	for _, route := range routes {
		if _, send := route.(*P2pSendTransport); send {
			if err := route.(P2pRouteLifecycle).CloseAndWait(ctx); err != nil {
				t.Fatalf("join final P2P send counters: %v", err)
			}
		}
	}
	for _, route := range routes {
		if receive, ok := route.(*P2pReceiveTransport); ok {
			_ = receive.conn.Close()
		}
	}
	for _, route := range routes {
		if err := route.(P2pRouteLifecycle).CloseAndWait(ctx); err != nil {
			t.Fatalf("join final P2P route counters: %v", err)
		}
	}
	return stats.Snapshot()
}

// Queue delivery can finish while the fast receive worker has not published
// any accounting yet. Freeze that exact ordering and require the final test
// snapshot to join the worker before inspecting either messages or bytes.
func TestP2pFastPathStatsJoinDelayedReceiveAccounting(t *testing.T) {
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelWait()
	ctx, cancel := context.WithCancel(context.Background())
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	conn := &lifecycleFastReceiveConn{
		lifecycleReadBarrierConn: &lifecycleReadBarrierConn{
			Conn: local, readEntered: make(chan struct{}),
		},
		messages: make(chan p2pFastPathReceivedMessage, 1),
	}
	settings := DefaultP2pTransportSettings()
	settings.DataPlaneStats = &P2pDataPlaneStats{}
	value, route := NewP2pReceiveTransport(ctx, cancel, conn, NewId(), settings)
	receiver := value.(*P2pReceiveTransport)
	queued := make(chan struct{})
	receiver.afterFastReceiveEnqueueForTest = func() {
		close(queued)
		<-ctx.Done()
	}
	message := bytes.Repeat([]byte{0x5a}, 4*1024)
	capture := newLifecyclePoolCapture(MessagePoolCopy(message))
	defer capture.cleanup()
	defer func() {
		if err := receiver.CloseAndWait(waitCtx); err != nil {
			t.Errorf("join delayed stats receiver: %v", err)
		}
	}()
	capture.requireOwnerLive(t, "delayed stats receive")
	conn.messages <- p2pFastPathReceivedMessage{message: capture.owner, fragmentCount: 4}
	waitCloseWaitBarrier(t, waitCtx, queued, "fast receive publication before counters")
	select {
	case received := <-route:
		if !bytes.Equal(received, message) {
			MessagePoolReturn(received)
			t.Fatal("delayed accounting changed the delivered message")
		}
		MessagePoolReturn(received)
	case <-waitCtx.Done():
		t.Fatal("receive queue did not forward the accounted message")
	}
	if got := settings.DataPlaneStats.Snapshot(); got.FastReceiveMessageCount != 0 {
		t.Fatalf("barrier did not force delivery before producer accounting: %+v", got)
	}
	got := snapshotP2pFastPathTestRoutes(t, settings.DataPlaneStats, value)
	if got.FastReceiveMessageCount != 1 || got.FastReceiveByteCount != uint64(len(message)) ||
		got.FastReceiveFragmentCount != 4 || got.FastDropCount != 0 {
		t.Fatalf("final route snapshot overtook receive accounting: %+v", got)
	}
	if receiver.pendingReceiveMessageCount.Load() != 0 || receiver.pendingReceiveByteCount.Load() != 0 {
		t.Fatal("final route snapshot retained a physical receive reservation")
	}
	capture.requireOwnerReturned(t, "delayed stats receive")
}

// A completed peer delivery need not mean the sender's Write has returned.
// Hold that return until lifecycle cancellation, before the send counters can
// be published, to cover the adjacent side of the same snapshot boundary.
type p2pStatsDelayedSendConn struct {
	net.Conn
	ctx       context.Context
	delivered chan []byte
}

func (self *p2pStatsDelayedSendConn) FastPathReady() bool { return true }
func (self *p2pStatsDelayedSendConn) WaitFastPathReady(context.Context, time.Duration) bool {
	return true
}
func (self *p2pStatsDelayedSendConn) FastPathMessages() <-chan p2pFastPathReceivedMessage {
	return nil
}
func (self *p2pStatsDelayedSendConn) WriteFastPathMessage(message []byte) (int, error) {
	select {
	case self.delivered <- bytes.Clone(message):
	case <-self.ctx.Done():
		return 0, self.ctx.Err()
	}
	<-self.ctx.Done()
	return 1, nil
}

func TestP2pFastPathStatsJoinDelayedSendAccounting(t *testing.T) {
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelWait()
	ctx, cancel := context.WithCancel(context.Background())
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	conn := &p2pStatsDelayedSendConn{Conn: local, ctx: ctx, delivered: make(chan []byte)}
	settings := DefaultP2pTransportSettings()
	settings.DataPlaneStats = &P2pDataPlaneStats{}
	value, route := NewP2pSendTransport(ctx, cancel, conn, NewId(), settings)
	sender := value.(*P2pSendTransport)
	message := bytes.Repeat([]byte{0x36}, 512)
	capture := newLifecyclePoolCapture(MessagePoolCopy(message))
	defer capture.cleanup()
	defer func() {
		if err := sender.CloseAndWait(waitCtx); err != nil {
			t.Errorf("join delayed stats sender: %v", err)
		}
	}()
	capture.requireOwnerLive(t, "delayed stats send")
	route <- capture.owner
	select {
	case received := <-conn.delivered:
		if !bytes.Equal(received, message) {
			t.Fatal("delayed accounting changed the sent message")
		}
	case <-waitCtx.Done():
		t.Fatal("send worker did not deliver the accounted message")
	}
	if got := settings.DataPlaneStats.Snapshot(); got.FastSendMessageCount != 0 {
		t.Fatalf("barrier did not force delivery before send accounting: %+v", got)
	}
	got := snapshotP2pFastPathTestRoutes(t, settings.DataPlaneStats, value)
	if got.FastSendMessageCount != 1 || got.FastSendByteCount != uint64(len(message)) ||
		got.FastSendFragmentCount != 1 || got.FastDropCount != 0 {
		t.Fatalf("final route snapshot overtook send accounting: %+v", got)
	}
	capture.requireOwnerReturned(t, "delayed stats send")
}

// Like the native peer wrapper, this already-started read needs its owned
// connection closed; changing the next read's deadline cannot release it.
type p2pStatsOwnedReadConn struct {
	net.Conn
	entered   chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
}

func (self *p2pStatsOwnedReadConn) Read([]byte) (int, error) {
	close(self.entered)
	<-self.closed
	return 0, io.EOF
}
func (self *p2pStatsOwnedReadConn) SetReadDeadline(time.Time) error { return nil }
func (self *p2pStatsOwnedReadConn) Close() error {
	self.closeOnce.Do(func() { close(self.closed) })
	return nil
}

// The stats fixture must retire its own peer connection before joining a
// blocked receive worker, rather than depending on later manager cleanup.
func TestP2pFastPathStatsJoinClosesOwnedReadConnection(t *testing.T) {
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelWait()
	ctx, cancel := context.WithCancel(context.Background())
	conn := &p2pStatsOwnedReadConn{entered: make(chan struct{}), closed: make(chan struct{})}
	settings := DefaultP2pTransportSettings()
	settings.DataPlaneStats = &P2pDataPlaneStats{}
	value, _ := NewP2pReceiveTransport(ctx, cancel, conn, NewId(), settings)
	receiver := value.(*P2pReceiveTransport)
	defer func() {
		_ = conn.Close()
		if err := receiver.CloseAndWait(waitCtx); err != nil {
			t.Errorf("join owned read connection: %v", err)
		}
	}()
	waitCloseWaitBarrier(t, waitCtx, conn.entered, "native-like blocked receive")
	got := snapshotP2pFastPathTestRoutes(t, settings.DataPlaneStats, value)
	if got.FastReceiveMessageCount != 0 || got.LegacyReceiveMessageCount != 0 {
		t.Fatalf("teardown manufactured received messages: %+v", got)
	}
	select {
	case <-conn.closed:
	default:
		t.Fatal("final stats did not retire the fixture-owned connection")
	}
}
