package extender

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	quic "github.com/quic-go/quic-go"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// This is the supported composition matrix, not twelve admission mocks: the
// outer TCP/TLS, H3, and DNS endpoints are real extender listeners, and the
// inner H1/H3/H3Dns/H3DnsPump endpoint actually decodes its wire protocol.
// Reliable payloads exercise both directions; QUIC DATAGRAM loss semantics
// and flight ownership are covered separately in the direct-hybrid tests.
func TestExtenderMobileMemoryLiveCompositionMatrix(t *testing.T) {
	old := connect.MemoryBudget()
	connect.SetMemoryBudget(32 * 1024 * 1024)
	defer connect.SetMemoryBudget(old)
	for _, inner := range []connect.TransportMode{connect.TransportModeH1, connect.TransportModeH3, connect.TransportModeH3Dns, connect.TransportModeH3DnsPump} {
		for _, outer := range []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierQuic, connect.ExtenderCarrierDns} {
			t.Run(string(inner)+"/"+outer, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				defer cancel()
				var destination string
				var packetDestination string
				fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
					settings.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
						return (&net.Dialer{}).DialContext(ctx, network, destination)
					}
					settings.DialPacketContext = func(ctx context.Context, network, address string) (net.Conn, error) {
						if address != packetDestination {
							return nil, fmt.Errorf("extender destination = %q, want unresolved %q", address, packetDestination)
						}
						return (&net.Dialer{}).DialContext(ctx, network, destination)
					}
				})
				// H3 must keep the upstream named-destination routing while
				// carrying its device memory owner through the same dial seam.
				fixture.server.allowedHosts = append(fixture.server.allowedHosts, "127.0.0.1", "alt.invalid")
				destination = newExtenderMemoryPlatformPeer(t, ctx, fixture.destination.certificate, inner)
				_, portString, err := net.SplitHostPort(destination)
				if err != nil {
					t.Fatal(err)
				}
				var port int
				fmt.Sscan(portString, &port)
				packetDestination = net.JoinHostPort("alt.invalid", portString)
				strategySettings := connect.DefaultClientStrategySettings()
				strategySettings.ConnectSettings = *fixture.connectSettings()
				strategySettings.TlsConfig = &tls.Config{InsecureSkipVerify: true} // local synthetic destination
				strategySettings.EnableNormal, strategySettings.EnableResilient = false, false
				strategySettings.ExtenderConfigs = []*connect.ExtenderConfig{fixture.extenderConfig(outer)}
				strategy := connect.NewClientStrategy(ctx, strategySettings)
				defer strategy.Close()
				settings := connect.DefaultPlatformTransportSettingsWithMemoryTarget(20 * 1024 * 1024)
				settings.EnableH3Datagrams = false
				settings.ModeInitialDelay = 0
				settings.PingTimeout = 30 * time.Second
				settings.H3Port, settings.DnsPort = port, port
				settings.AltUrl = "https://" + packetDestination
				settings.DnsPumpHost = "alt.invalid"
				settings.DnsTlds = [][]byte{[]byte(testDnsTld)}
				settings.QuicTlsConfig = &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"extender-memory"}} // local fixture
				routes := make(chan connect.Route, 1)
				settings.SendRouteObserver = func(_ connect.Transport, route connect.Route, connected bool) {
					if connected {
						select {
						case routes <- route:
						default:
						}
					}
				}
				budget, root := settings.PlatformTransportBudget, connect.DefaultPlatformTransportBudget()
				base := root.Stats()
				routeManager := connect.NewRouteManager(ctx, "extender-memory-matrix")
				reader := routeManager.OpenMultiRouteReader(connect.DestinationId(connect.NewId()))
				defer routeManager.CloseMultiRouteReader(reader)
				transport := connect.NewPlatformTransportWithTargetMode(ctx, strategy, routeManager, "wss://127.0.0.1/ws",
					&connect.ClientAuth{ByJwt: "memory", InstanceId: connect.NewId(), AppVersion: "test"}, inner, settings)
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
						t.Fatalf("carrier did not connect: %v; extender: %v", ctx.Err(), err)
					}
					t.Fatal(ctx.Err())
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
					if stats.Budget.TotalByteCount != 5*1024*1024 || stats.Root.TotalByteCount != 8*1024*1024 ||
						stats.Budget.UsedByteCount != want || stats.Root.UsedByteCount != base.UsedByteCount+want ||
						stats.Budget.UsedTransportCount != 1 || stats.Root.UsedTransportCount != base.UsedTransportCount+1 {
						t.Fatalf("composed live ownership: want=%d child/root=%+v", want, stats)
					}
				}
				assertClaim()
				for i := range 4 {
					payload := bytes.Repeat([]byte{byte(i + 1)}, 4096+i*257)
					message, err := connect.ProtoMarshal(&protocol.TransferFrame{
						TransferPath: &protocol.TransferPath{DestinationId: connect.NewId().Bytes()},
						Pack:         &protocol.Pack{MessageId: connect.NewId().Bytes(), SequenceId: connect.NewId().Bytes(), Frames: []*protocol.Frame{{MessageType: protocol.MessageType_TestSimpleMessage, MessageBytes: payload}}},
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
						t.Fatalf("bidirectional exchange %d: %v", i, err)
					}
					matched := bytes.Equal(response, expected)
					connect.MessagePoolReturn(response)
					if !matched {
						t.Fatal("inner payload changed in transit")
					}
					assertClaim()
				}
				if outer == connect.ExtenderCarrierDns {
					cancel() // cancellation joins the same graph as explicit close
				}
				cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				if err := transport.CloseAndWait(cleanup); err != nil {
					t.Fatal(err)
				}
				stats := budget.StatsWithRoot()
				if stats.Budget.UsedByteCount != 0 || stats.Budget.UsedTransportCount != 0 || stats.Budget.ReservedByteCount != stats.Budget.ReleasedByteCount ||
					stats.Root.UsedByteCount != base.UsedByteCount || stats.Root.UsedTransportCount != base.UsedTransportCount {
					t.Fatalf("composed teardown retained ownership: %+v", stats)
				}
				t.Logf("live child/root peak=%d/%d bytes; four bidirectional payloads; joined cleanup", want, base.UsedByteCount+want)
			})
		}
	}
}

func newExtenderMemoryPlatformPeer(t *testing.T, ctx context.Context, certificate *tls.Certificate, mode connect.TransportMode) string {
	t.Helper()
	if mode == connect.TransportModeH1 {
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ws, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer ws.Close()
			stop := context.AfterFunc(ctx, func() { ws.Close() })
			defer stop()
			for {
				kind, payload, err := ws.ReadMessage()
				if err != nil {
					return
				}
				if err := ws.WriteMessage(kind, payload); err != nil {
					return
				}
			}
		}))
		server.TLS = &tls.Config{Certificates: []tls.Certificate{*certificate}}
		server.StartTLS()
		t.Cleanup(server.Close)
		return server.Listener.Addr().String()
	}
	socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := socket.LocalAddr().String()
	if mode != connect.TransportModeH3 {
		settings := connect.DefaultPacketTranslationSettings()
		settings.DnsTlds = [][]byte{[]byte(testDnsTld)}
		translationMode := connect.PacketTranslationModeDecode53
		if mode == connect.TransportModeH3DnsPump {
			translationMode = connect.PacketTranslationModeDecode53RequireDnsPump
		}
		socket, err = connect.NewPacketTranslation(ctx, translationMode, socket, settings)
		if err != nil {
			t.Fatal(err)
		}
	}
	qt := &quic.Transport{Conn: socket}
	listener, err := qt.Listen(&tls.Config{Certificates: []tls.Certificate{*certificate}, NextProtos: []string{"extender-memory"}}, &quic.Config{MaxIdleTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close(); qt.Close(); socket.Close() })
	go func() {
		conn, err := listener.Accept(ctx)
		if err != nil {
			return
		}
		defer conn.CloseWithError(0, "fixture ended")
		stop := context.AfterFunc(ctx, func() { conn.CloseWithError(0, "fixture canceled") })
		defer stop()
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		framer := connect.NewFramer(connect.DefaultFramerSettings(8192))
		auth, err := framer.Read(stream)
		if err != nil {
			return
		}
		authMessage, err := connect.DecodeFrame(auth)
		connect.MessagePoolReturn(auth)
		if err != nil {
			return
		}
		authRequest, ok := authMessage.(*protocol.Auth)
		if !ok {
			return
		}
		authResponse, _ := connect.AcceptH3DatagramAuthOffer(authRequest, false, false, false)
		response, err := connect.EncodeFrame(authResponse, connect.DefaultProtocolVersion)
		if err != nil {
			return
		}
		err = framer.Write(stream, response)
		connect.MessagePoolReturn(response)
		if err != nil {
			return
		}
		io.Copy(stream, stream)
	}()
	return address
}
