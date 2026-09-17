package connect

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/urnetwork/connect/protocol"
)

// A real QUIC peer, not an admission mock. The DNS arms decode the actual
// client wire protocol, including pump-only replies. Only the client claims
// are under test; an in-process server's heap is not a device runtime sample.
func newMobileMemoryQuicEcho(t *testing.T, ctx context.Context, mode TransportMode) (int, <-chan error) {
	t.Helper()
	certPem, keyPem, err := selfSign([]string{"127.0.0.1"}, "memory-matrix", time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		t.Fatal(err)
	}
	socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := socket.LocalAddr().(*net.UDPAddr).Port
	if mode != TransportModeH3 {
		settings := DefaultPacketTranslationSettings()
		settings.DnsTlds = [][]byte{[]byte("memory.example.")}
		ptMode := PacketTranslationModeDecode53
		if mode == TransportModeH3DnsPump {
			ptMode = PacketTranslationModeDecode53RequireDnsPump
		}
		socket, err = NewPacketTranslation(ctx, ptMode, socket, settings)
		if err != nil {
			t.Fatal(err)
		}
	}
	qt := &quic.Transport{Conn: socket}
	listener, err := qt.Listen(&tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"memory-matrix"}},
		&quic.Config{EnableDatagrams: true, MaxIdleTimeout: 20 * time.Second})
	if err != nil {
		socket.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close(); qt.Close(); socket.Close() })
	errors := make(chan error, 4)
	go func() {
		conn, err := listener.Accept(ctx)
		if err != nil {
			errors <- err
			return
		}
		defer conn.CloseWithError(0, "test done")
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			errors <- err
			return
		}
		framer := NewFramer(DefaultFramerSettings(8192))
		authBytes, err := framer.Read(stream)
		if err != nil {
			errors <- err
			return
		}
		authMessage, err := DecodeFrame(authBytes)
		MessagePoolReturn(authBytes)
		if err != nil {
			errors <- err
			return
		}
		auth, ok := authMessage.(*protocol.Auth)
		if !ok {
			errors <- fmt.Errorf("auth type %T", authMessage)
			return
		}
		state := conn.ConnectionState()
		response, accepted := AcceptH3DatagramAuthOffer(auth, true, state.SupportsDatagrams.Local, state.SupportsDatagrams.Remote)
		if !accepted {
			errors <- fmt.Errorf("hybrid not negotiated")
			return
		}
		responseBytes, err := EncodeFrame(response, DefaultProtocolVersion)
		if err != nil {
			errors <- err
			return
		}
		err = framer.Write(stream, responseBytes)
		MessagePoolReturn(responseBytes)
		if err != nil {
			errors <- err
			return
		}
		go func() {
			for {
				packet, err := conn.ReceiveDatagram(ctx)
				if err != nil {
					return
				}
				if err := conn.SendDatagram(packet); err != nil {
					errors <- err
					return
				}
			}
		}()
		for {
			message, err := framer.Read(stream)
			if err != nil {
				return
			}
			err = framer.Write(stream, message)
			MessagePoolReturn(message)
			if err != nil {
				return
			}
		}
	}()
	return port, errors
}

// Allow0RTT in quic.Config controls server acceptance only. Pin the client
// behavior with a warmed TLS ticket and a second server handshake deliberately
// held after ClientHello: DialEarly would return before that handshake ends.
func TestPlatformMobileResumptionWaitsForHandshakeBeforeTrackedWrites(t *testing.T) {
	old := MemoryBudget()
	defer SetMemoryBudget(old)
	SetMemoryBudget(mib(32))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	certPem, keyPem, err := selfSign([]string{"127.0.0.1"}, "memory-resume", time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		t.Fatal(err)
	}
	var hellos atomic.Int32
	secondHello := make(chan struct{})
	releaseHello := make(chan struct{})
	listener, err := quic.ListenAddrEarly("127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"memory-resume"},
		SessionTicketKey: [32]byte{1, 2, 3}, // stable test-only key across cloned server configs
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			if hellos.Add(1) == 2 {
				close(secondHello)
				select {
				case <-releaseHello:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return nil, nil
		}}, &quic.Config{Allow0RTT: true, MaxIdleTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept(ctx)
			if err != nil {
				return
			}
			go func() { <-ctx.Done(); conn.CloseWithError(0, "") }()
		}
	}()
	settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(20))
	claim := settings.PlatformTransportBudget.register(platformTransportBudgetH3Explicit, settings.H3BudgetByteCount, true)
	if !claim.TryAcquire() {
		t.Fatal("mobile carrier claim")
	}
	defer claim.Release()
	transport := newTestAltTransport(t, settings)
	cache := tls.NewLRUClientSessionCache(1)
	tlsConfig := &tls.Config{InsecureSkipVerify: true, ServerName: "127.0.0.1", NextProtos: []string{"memory-resume"}, ClientSessionCache: cache}
	address := listener.Addr().(*net.UDPAddr)
	wrap := func(_ context.Context, conn net.PacketConn) (net.PacketConn, error) { return conn, nil }
	dial := func() (*h3DialAttempt, error) {
		return transport.dialH3(ctx, TransportModeH3, tlsConfig.ServerName, address, wrap, tlsConfig, newPlatformQuicConfig(settings, 1), 1, false)
	}
	first, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	defer first.close()
	for {
		if _, ok := cache.Get("127.0.0.1"); ok {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("server did not issue a resumption ticket")
		case <-time.After(time.Millisecond):
		}
	}
	first.close()
	type result struct {
		attempt *h3DialAttempt
		err     error
	}
	resultCh := make(chan result, 1)
	go func() { attempt, err := dial(); resultCh <- result{attempt, err} }()
	select {
	case <-secondHello:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case early := <-resultCh:
		early.attempt.close()
		close(releaseHello)
		t.Fatalf("mobile dial returned before resumed handshake: %v", early.err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseHello)
	var resumed result
	select {
	case resumed = <-resultCh:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if resumed.err != nil {
		t.Fatal(resumed.err)
	}
	defer resumed.attempt.close()
	if !resumed.attempt.conn.ConnectionState().TLS.DidResume {
		t.Fatal("fixture did not resume the cached session")
	}
	select {
	case <-resumed.attempt.conn.HandshakeComplete():
	default:
		t.Fatal("resumed connection permits early writes")
	}
	if quicSendFlightForConn(resumed.attempt.conn) == nil {
		t.Fatal("resumed connection lost its retained-send tracker")
	}
}

func TestPlatformMobileLiveH3DnsPumpBidirectionalAndTeardown(t *testing.T) {
	old := MemoryBudget()
	defer SetMemoryBudget(old)
	SetMemoryBudget(mib(32))
	for _, mode := range []TransportMode{TransportModeH3, TransportModeH3Dns, TransportModeH3DnsPump} {
		t.Run(string(mode), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			port, serverErrors := newMobileMemoryQuicEcho(t, ctx, mode)
			settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(20))
			settings.AltUrl = fmt.Sprintf("https://127.0.0.1:%d", port)
			settings.DnsPort, settings.H3Port = port, port
			settings.DnsPumpHost = "127.0.0.1"
			settings.DnsTlds = [][]byte{[]byte("memory.example.")}
			settings.QuicTlsConfig = &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"memory-matrix"}} // loopback-only fixture
			settings.ModeInitialDelay = 0
			settings.PingTimeout = 30 * time.Second
			routes := make(chan Route, 1)
			settings.SendRouteObserver = func(_ Transport, route Route, connected bool) {
				if connected {
					select {
					case routes <- route:
					default:
					}
				}
			}
			var datagramCount, streamCount int
			settings.H3SendLaneObserver = func(_ []byte, datagram bool) {
				if datagram {
					datagramCount++
				} else {
					streamCount++
				}
			}
			budget, root := settings.PlatformTransportBudget, DefaultPlatformTransportBudget()
			base := root.Stats().UsedByteCount
			strategy := NewClientStrategyWithDefaults(ctx)
			defer strategy.Close()
			routeManager := NewRouteManager(ctx, "memory-live-matrix")
			reader := routeManager.OpenMultiRouteReader(DestinationId(NewId()))
			defer routeManager.CloseMultiRouteReader(reader)
			transport := NewPlatformTransportWithTargetMode(ctx, strategy, routeManager, "https://127.0.0.1",
				&ClientAuth{ByJwt: "memory", InstanceId: NewId(), AppVersion: "test"}, mode, settings)
			defer func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cleanupCancel()
				if err := transport.CloseAndWait(cleanupCtx); err != nil {
					t.Error(err)
				}
			}()
			var route Route
			select {
			case route = <-routes:
			case err := <-serverErrors:
				t.Fatal(err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if cap(route) != 4 {
				t.Fatalf("live H3 route depth=%d", cap(route))
			}
			want := settings.H3BudgetByteCount
			if mode != TransportModeH3 {
				want += kib(144)
			}
			if budget.Stats().UsedByteCount != want || root.Stats().UsedByteCount != base+want {
				t.Fatalf("live claim child=%+v root=%+v", budget.Stats(), root.Stats())
			}
			// Alternate true DATAGRAM and reliable stream frames in both
			// directions. DNS intentionally keeps its production packet pacing;
			// this is identity/ownership coverage, not a throughput benchmark.
			for i := 0; i < 16; i++ {
				payloadBytes := 500
				if i%2 != 0 {
					payloadBytes = 4096
				}
				message, err := ProtoMarshal(&protocol.TransferFrame{TransferPath: &protocol.TransferPath{DestinationId: NewId().Bytes()},
					Pack: &protocol.Pack{MessageId: NewId().Bytes(), SequenceId: NewId().Bytes(), Frames: []*protocol.Frame{{MessageType: protocol.MessageType_TestSimpleMessage, MessageBytes: bytes.Repeat([]byte{byte(i)}, payloadBytes)}}}})
				if err != nil {
					t.Fatal(err)
				}
				expected := bytes.Clone(message)
				select {
				case route <- message:
				case <-ctx.Done():
					MessagePoolReturn(message)
					t.Fatal(ctx.Err())
				}
				response, err := reader.Read(ctx, 5*time.Second)
				if err != nil {
					t.Fatalf("round trip %d (%d payload bytes): %v", i, payloadBytes, err)
				}
				matched := bytes.Equal(response, expected)
				MessagePoolReturn(response)
				if !matched {
					t.Fatalf("%s round trip %d changed payload", mode, i)
				}
				if budget.Stats().UsedByteCount != want || root.Stats().UsedByteCount != base+want {
					t.Fatal("traffic changed admitted graph")
				}
			}
			if selected, _ := transport.activeMode(); selected != mode {
				t.Fatalf("selected=%s want=%s", selected, mode)
			}
			if err := transport.CloseAndWait(ctx); err != nil {
				t.Fatal(err)
			}
			if datagramCount == 0 || streamCount == 0 {
				t.Fatalf("stream/DATAGRAM path coverage=%d/%d", streamCount, datagramCount)
			}
			stats := budget.Stats()
			if stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount || root.Stats().UsedByteCount != base {
				t.Fatalf("live teardown child=%+v root=%+v", stats, root.Stats())
			}
		})
	}
}
