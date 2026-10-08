// Separately counted current-profile graphs preserve the historical20/5
// matrix. Every top-level case opens real inner and outer loopback carriers,
// exchanges payload, and joins both close and parent-cancel lifecycles.
package extender

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// H1 over the TCP+TLS extender.
func TestExtenderIosMemoryLiveH1Tcp(t *testing.T) {
	testExtenderIosMemoryLive(t, connect.TransportModeH1, connect.ExtenderCarrierTcp)
}

// H1 over the QUIC extender.
func TestExtenderIosMemoryLiveH1Quic(t *testing.T) {
	testExtenderIosMemoryLive(t, connect.TransportModeH1, connect.ExtenderCarrierQuic)
}

// H1 over the DNS extender.
func TestExtenderIosMemoryLiveH1Dns(t *testing.T) {
	testExtenderIosMemoryLive(t, connect.TransportModeH1, connect.ExtenderCarrierDns)
}

// Inner H3 over the TCP+TLS extender.
func TestExtenderIosMemoryLiveH3Tcp(t *testing.T) {
	testExtenderIosMemoryLive(t, connect.TransportModeH3, connect.ExtenderCarrierTcp)
}

// Inner H3 over the QUIC extender.
func TestExtenderIosMemoryLiveH3Quic(t *testing.T) {
	testExtenderIosMemoryLive(t, connect.TransportModeH3, connect.ExtenderCarrierQuic)
}

// Inner H3 over the DNS extender.
func TestExtenderIosMemoryLiveH3Dns(t *testing.T) {
	testExtenderIosMemoryLive(t, connect.TransportModeH3, connect.ExtenderCarrierDns)
}

// Inner encoded DNS over the TCP+TLS extender.
func TestExtenderIosMemoryLiveH3DnsTcp(t *testing.T) {
	testExtenderIosMemoryLive(t, connect.TransportModeH3Dns, connect.ExtenderCarrierTcp)
}

// Inner encoded DNS over the QUIC extender.
func TestExtenderIosMemoryLiveH3DnsQuic(t *testing.T) {
	testExtenderIosMemoryLive(t, connect.TransportModeH3Dns, connect.ExtenderCarrierQuic)
}

// Inner and outer DNS retain distinct translation claims.
func TestExtenderIosMemoryLiveH3DnsDns(t *testing.T) {
	testExtenderIosMemoryLive(t, connect.TransportModeH3Dns, connect.ExtenderCarrierDns)
}

// Inner pump-only replies over the TCP+TLS extender.
func TestExtenderIosMemoryLiveH3DnsPumpTcp(t *testing.T) {
	testExtenderIosMemoryLive(t, connect.TransportModeH3DnsPump, connect.ExtenderCarrierTcp)
}

// Inner pump-only replies over the QUIC extender.
func TestExtenderIosMemoryLiveH3DnsPumpQuic(t *testing.T) {
	testExtenderIosMemoryLive(t, connect.TransportModeH3DnsPump, connect.ExtenderCarrierQuic)
}

// The largest graph retains separate inner-pump and outer-DNS ownership.
func TestExtenderIosMemoryLiveH3DnsPumpDns(t *testing.T) {
	testExtenderIosMemoryLive(t, connect.TransportModeH3DnsPump, connect.ExtenderCarrierDns)
}

// A socket observer must preserve the socket's native buffer-control surface.
type extenderIosMemoryPacketConn struct {
	net.PacketConn
	closeOnce sync.Once
	closeErr  error
	onClose   func()
}

// Observe physical close once, before the containing extender releases bytes.
func (self *extenderIosMemoryPacketConn) Close() error {
	self.closeOnce.Do(func() {
		self.closeErr = self.PacketConn.Close()
		self.onClose()
	})
	return self.closeErr
}

// Forward the requested raw receive-buffer cap to the loopback socket.
func (self *extenderIosMemoryPacketConn) SetReadBuffer(n int) error {
	return self.PacketConn.(interface{ SetReadBuffer(int) error }).SetReadBuffer(n)
}

// Forward the requested raw send-buffer cap to the loopback socket.
func (self *extenderIosMemoryPacketConn) SetWriteBuffer(n int) error {
	return self.PacketConn.(interface{ SetWriteBuffer(int) error }).SetWriteBuffer(n)
}

// TCP socket ownership is observed below the outer TLS layer.
type extenderIosMemoryConn struct {
	net.Conn
	closeOnce sync.Once
	closeErr  error
	onClose   func()
}

// Native close precedes the claim-release witness.
func (self *extenderIosMemoryConn) Close() error {
	self.closeOnce.Do(func() {
		self.closeErr = self.Conn.Close()
		self.onClose()
	})
	return self.closeErr
}

// One forced composition, with a fresh owner for each termination path. The
// fixed exact-profile checks precede even the fixture's listener creation.
func testExtenderIosMemoryLive(t *testing.T, inner connect.TransportMode, outer string) {
	t.Helper()
	previousTarget := connect.MemoryBudget()
	previousSoftLimit := debug.SetMemoryLimit(32 * 1024 * 1024)
	connect.SetMemoryBudget(32 * 1024 * 1024)
	t.Cleanup(func() {
		connect.SetMemoryBudget(previousTarget)
		debug.SetMemoryLimit(previousSoftLimit)
	})
	run := func(cancelParent bool) {
		settings := connect.DefaultPlatformTransportSettingsWithMemoryTarget(32 * 1024 * 1024)
		connect.ApplyMobilePlatformTransportMemoryPolicy(settings, 32*1024*1024)
		budget := settings.PlatformTransportBudget
		assertProfile := func() {
			t.Helper()
			stats := budget.StatsWithRoot()
			if connect.MemoryBudget() != 32*1024*1024 || debug.SetMemoryLimit(-1) != 32*1024*1024 ||
				stats.Budget.TotalByteCount != 8*1024*1024 || stats.Budget.MaxTransportCount != 16 || stats.Root != stats.Budget ||
				settings.H1BudgetByteCount != 256*1024 || settings.H3BudgetByteCount != 5696*1024 ||
				settings.H3MaxConnectionReceiveWindowByteCount != 4*1024*1024 || settings.H3MaxStreamReceiveWindowByteCount != 3072*1024 ||
				settings.H3InitialConnectionReceiveWindowByteCount != 256*1024 || settings.H3InitialStreamReceiveWindowByteCount != 128*1024 ||
				settings.H3SocketReadBufferByteCount != 64*1024 || settings.H3SocketWriteBufferByteCount != 64*1024 {
				t.Fatalf("%s/%s: graph is not the exact target32/soft32/carrier8 profile: %+v", inner, outer, stats)
			}
		}
		assertProfile()
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture, port, packetDestination, beginPeerShutdown, closePeer := newExtenderIosMemoryFixture(t, inner)
		defer closePeer()
		strategySettings := connect.DefaultClientStrategySettings()
		strategySettings.ConnectSettings = *fixture.connectSettings()
		strategySettings.TlsConfig = &tls.Config{InsecureSkipVerify: true}
		strategySettings.EnableNormal, strategySettings.EnableResilient = false, false
		strategySettings.ExpandExtenderProfileCount = 0
		strategySettings.ExtenderConfigs = []*connect.ExtenderConfig{fixture.extenderConfig(outer)}
		settings.EnableH3Datagrams = false
		settings.ModeInitialDelay = 0
		settings.PingTimeout = 30 * time.Second
		settings.H3Port, settings.DnsPort = port, port
		settings.AltUrl = "https://" + packetDestination
		settings.DnsPumpHost = "alt.invalid"
		settings.DnsTlds = [][]byte{[]byte(testDnsTld)}
		settings.QuicTlsConfig = &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"extender-memory"}}
		unrelated := connect.NewPlatformTransportBudgetForMemoryTarget(32 * 1024 * 1024)
		unrelatedBase := unrelated.StatsWithRoot()
		if unrelated == budget || unrelatedBase.Root.TotalByteCount != 8*1024*1024 {
			t.Fatal("current profile did not construct independent admission owners")
		}
		want := settings.H1BudgetByteCount
		if inner != connect.TransportModeH1 {
			want = settings.H3BudgetByteCount
			if inner != connect.TransportModeH3 {
				want += 144 * 1024
			}
		}
		if outer == connect.ExtenderCarrierTcp {
			want += settings.H1BudgetByteCount
		} else {
			want += (512 + 512 + 256 + 128) * 1024
			if outer == connect.ExtenderCarrierDns {
				want += 144 * 1024
			}
		}
		assertClaim := func() {
			t.Helper()
			stats := budget.StatsWithRoot()
			if stats.Budget.TotalByteCount != 8*1024*1024 || stats.Root != stats.Budget ||
				stats.Budget.UsedByteCount != want || stats.Budget.UsedTransportCount != 1 ||
				stats.Budget.ReservedByteCount-stats.Budget.ReleasedByteCount != want ||
				unrelated.StatsWithRoot() != unrelatedBase {
				t.Errorf("%s/%s cancel=%t: complete graph claim want=%d got=%+v", inner, outer, cancelParent, want, stats)
			}
		}
		var tcpOpened, udpOpened, live atomic.Int32
		onClose := func() {
			assertClaim()
			live.Add(-1)
		}
		strategySettings.ConnectSettings.DialContextSettings = &connect.DialContextSettings{
			DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
				assertClaim()
				wantAddress := net.JoinHostPort(fixture.ip.String(), fmt.Sprint(fixture.tcpPort))
				if outer != connect.ExtenderCarrierTcp || address != wantAddress {
					return nil, fmt.Errorf("unexpected outer socket: %s/%s at %s, want %s", inner, outer, address, wantAddress)
				}
				conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
				if err != nil {
					return nil, err
				}
				tcpOpened.Add(1)
				live.Add(1)
				return &extenderIosMemoryConn{Conn: conn, onClose: onClose}, nil
			},
			PacketConnFactory: func(context.Context) (net.PacketConn, error) {
				assertClaim()
				if outer == connect.ExtenderCarrierTcp {
					return nil, fmt.Errorf("TCP extender attempted a raw UDP socket")
				}
				socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
				if err != nil {
					return nil, err
				}
				udpOpened.Add(1)
				live.Add(1)
				return &extenderIosMemoryPacketConn{PacketConn: socket, onClose: onClose}, nil
			},
		}
		routes := make(chan connect.Route, 1)
		settings.SendRouteObserver = func(_ connect.Transport, route connect.Route, connected bool) {
			if connected {
				select {
				case routes <- route:
				default:
				}
			}
		}
		assertProfile()
		strategy := connect.NewClientStrategy(ctx, strategySettings)
		defer strategy.Close()
		routeManager := connect.NewRouteManager(ctx, "extender-ios-memory")
		reader := routeManager.OpenMultiRouteReader(connect.DestinationId(connect.NewId()))
		defer routeManager.CloseMultiRouteReader(reader)
		transport := connect.NewPlatformTransportWithTargetMode(ctx, strategy, routeManager, "wss://127.0.0.1/ws",
			&connect.ClientAuth{ByJwt: "synthetic-ios-memory", InstanceId: connect.NewId(), AppVersion: "test"}, inner, settings)
		defer func() {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if err := transport.CloseAndWait(cleanup); err != nil {
				t.Error(err)
			}
		}()
		var route connect.Route
		select {
		case route = <-routes:
		case <-ctx.Done():
			if err, ok := fixture.nextError(); ok {
				t.Fatalf("%s/%s: carrier did not connect: %v; extender: %v", inner, outer, ctx.Err(), err)
			}
			t.Fatal(ctx.Err())
		}
		wantDepth := 4
		if inner == connect.TransportModeH1 {
			wantDepth = settings.TransportBufferSize
		}
		if cap(route) != wantDepth || live.Load() != 1 ||
			(outer == connect.ExtenderCarrierTcp && (tcpOpened.Load() == 0 || udpOpened.Load() != 0)) ||
			(outer != connect.ExtenderCarrierTcp && (udpOpened.Load() == 0 || tcpOpened.Load() != 0)) {
			t.Fatalf("%s/%s: wrong live route/socket graph: depth=%d TCP=%d UDP=%d live=%d", inner, outer, cap(route), tcpOpened.Load(), udpOpened.Load(), live.Load())
		}
		assertClaim()
		for i := range 4 {
			payload := bytes.Repeat([]byte{byte(i + 1)}, 4096+i*257)
			message, err := connect.ProtoMarshal(&protocol.TransferFrame{
				TransferPath: &protocol.TransferPath{DestinationId: connect.NewId().Bytes()},
				Pack: &protocol.Pack{MessageId: connect.NewId().Bytes(), SequenceId: connect.NewId().Bytes(), Frames: []*protocol.Frame{{
					MessageType: protocol.MessageType_TestSimpleMessage, MessageBytes: payload,
				}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			expected := bytes.Clone(message)
			select {
			case route <- message:
			case <-ctx.Done():
				connect.MessagePoolReturn(message)
				t.Fatal(ctx.Err())
			}
			response, err := reader.Read(ctx, 5*time.Second)
			if err != nil {
				t.Fatalf("%s/%s cancel=%t exchange=%d: %v", inner, outer, cancelParent, i, err)
			}
			matched := bytes.Equal(response, expected)
			connect.MessagePoolReturn(response)
			if !matched {
				t.Fatalf("%s/%s: inner payload changed in transit", inner, outer)
			}
			assertClaim()
		}
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		beginPeerShutdown()
		if cancelParent {
			cancel()
			select {
			case <-transport.Done():
			case <-cleanup.Done():
				t.Fatalf("%s/%s: parent cancellation did not independently join the graph", inner, outer)
			}
		}
		if err := transport.CloseAndWait(cleanup); err != nil {
			t.Fatal(err)
		}
		stats := budget.StatsWithRoot()
		if live.Load() != 0 || stats.Budget.UsedByteCount != 0 || stats.Budget.UsedTransportCount != 0 ||
			stats.Budget.ReservedByteCount != stats.Budget.ReleasedByteCount || stats.Root != stats.Budget ||
			unrelated.StatsWithRoot() != unrelatedBase {
			t.Fatalf("%s/%s cancel=%t: composed joined teardown retained sockets=%d or claims=%+v", inner, outer, cancelParent, live.Load(), stats)
		}
		t.Logf("current target32/soft32/carrier8; %s/%s; claim=%d; four payload echoes; cancel=%t; joined cleanup", inner, outer, want, cancelParent)
	}
	for _, cancelParent := range []bool{false, true} {
		run(cancelParent)
	}
}
