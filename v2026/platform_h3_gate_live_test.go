package connect

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

// A socket that holds what it reads until released, so a dial over it waits
// in its handshake for as long as the test says.
type gateLiveTestHeldConn struct {
	net.PacketConn
	release   <-chan struct{}
	writeOnce sync.Once
	written   chan struct{}
	closeOnce sync.Once
	closed    chan struct{}
}

func (self *gateLiveTestHeldConn) WriteTo(b []byte, address net.Addr) (int, error) {
	self.writeOnce.Do(func() { close(self.written) })
	return self.PacketConn.WriteTo(b, address)
}

func (self *gateLiveTestHeldConn) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case <-self.release:
	case <-self.closed:
		return 0, nil, net.ErrClosed
	}
	return self.PacketConn.ReadFrom(b)
}

func (self *gateLiveTestHeldConn) Close() error {
	self.closeOnce.Do(func() { close(self.closed) })
	return self.PacketConn.Close()
}

// One h3 waiter's view of the gate: the active owner's mode, and whether it
// was still dialing.
type gateLiveTestView struct {
	activeMode TransportMode
	dialing    bool
}

// A running transport's h3dns dial keeps the gate from a better mode for its
// whole handshake and auth: direct H3 waiting for the gate, as a retrying h3
// runner does, leaves the dial alone while it is held in its handshake, ends
// the h3dns connection only once runH3 reports it connected, and acquires the
// gate after the connection closes. The old gate ended the dial at once.
func TestPlatformH3GateKeepsARunningDnsDial(t *testing.T) {
	old := MemoryBudget()
	defer SetMemoryBudget(old)
	SetMemoryBudget(mib(32))
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	port, serverErrors := newMobileMemoryQuicEcho(t, ctx, TransportModeH3Dns)
	settings := testingDefaultPlatformTransportSettingsWithMemoryTarget(mib(20))
	settings.AltUrl = fmt.Sprintf("https://127.0.0.1:%d", port)
	settings.DnsPort, settings.H3Port = port, port
	settings.DnsTlds = [][]byte{[]byte("memory.example.")}
	settings.QuicTlsConfig = &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"memory-matrix"}} // loopback-only fixture
	settings.ModeInitialDelay = 0
	settings.PingTimeout = 30 * time.Second
	// keep the default preferences, where h3 is better than h3dns, rather
	// than an h3 target's h1 fallback ranking
	settings.H3BudgetFallbackTimeout = -1
	releaseReads := make(chan struct{})
	releaseReadsOnce := sync.OnceFunc(func() { close(releaseReads) })
	defer releaseReadsOnce()
	type opened struct {
		conn *gateLiveTestHeldConn
		ctx  context.Context
	}
	openedConns := make(chan opened, 8)
	settings.H3PacketConnFactory = func(dialCtx context.Context) (net.PacketConn, error) {
		raw, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		conn := &gateLiveTestHeldConn{
			PacketConn: raw,
			release:    releaseReads,
			written:    make(chan struct{}),
			closed:     make(chan struct{}),
		}
		select {
		case openedConns <- opened{conn: conn, ctx: dialCtx}:
		default:
		}
		return conn, nil
	}
	strategy := NewClientStrategyWithDefaults(ctx)
	defer strategy.Close()
	routeManager := NewRouteManager(ctx, "h3-gate-live")
	transport := NewPlatformTransportWithTargetMode(ctx, strategy, routeManager, "https://127.0.0.1",
		&ClientAuth{ByJwt: "gate", InstanceId: NewId(), AppVersion: "test"}, TransportModeH3Dns, settings)
	if !transport.isBetterMode(TransportModeH3, TransportModeH3Dns) {
		t.Fatal("the transport does not rank h3 above h3dns")
	}
	defer func() {
		releaseReadsOnce()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if err := transport.CloseAndWait(cleanupCtx); err != nil {
			t.Error(err)
		}
	}()

	var dial opened
	select {
	case dial = <-openedConns:
	case err := <-serverErrors:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("the h3dns dial opened no socket")
	}
	select {
	case <-dial.conn.written:
	case <-ctx.Done():
		t.Fatal("the h3dns dial sent no ClientHello")
	}

	gate := transport.h3Gate
	views := make(chan gateLiveTestView, 64)
	func() {
		gate.mutex.Lock()
		defer gate.mutex.Unlock()
		gate.waitingForTest = func(mode TransportMode) {
			if mode != TransportModeH3 {
				return
			}
			gate.mutex.Lock()
			defer gate.mutex.Unlock()
			select {
			case views <- gateLiveTestView{activeMode: gate.activeMode, dialing: gate.activeDialing}:
			default:
			}
		}
	}()
	directAcquired := acquirePlatformH3GateTest(ctx, gate, TransportModeH3)
	var view gateLiveTestView
	select {
	case view = <-views:
	case <-ctx.Done():
		t.Fatal("direct H3 never waited for the gate")
	}
	if view.activeMode != TransportModeH3Dns || !view.dialing {
		t.Fatalf("direct H3 waited on %+v, want the dialing h3dns owner", view)
	}
	if dial.ctx.Err() != nil {
		t.Fatal("a waiting better mode ended the h3dns dial in its handshake")
	}

	// the handshake and the auth complete; runH3 reports the connection,
	// and only then does direct H3 end it
	releaseReadsOnce()
	connectedSeen := false
	for !connectedSeen {
		select {
		case view = <-views:
			connectedSeen = view.activeMode == TransportModeH3Dns && !view.dialing
		case releaseDirect := <-directAcquired:
			releaseDirect()
			t.Fatal("direct H3 acquired the gate without the h3dns dial connecting")
		case err := <-serverErrors:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal("the h3dns dial never reported itself connected")
		}
	}
	select {
	case <-dial.ctx.Done():
	case <-ctx.Done():
		t.Fatal("direct H3 did not end the connected h3dns carrier")
	}
	select {
	case releaseDirect := <-directAcquired:
		releaseDirect()
	case <-ctx.Done():
		t.Fatal("direct H3 did not acquire the gate after the h3dns connection closed")
	}
}
