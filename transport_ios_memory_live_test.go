// Real loopback carrier graphs for the current iOS target32/soft32 profile.
// The peer shares this host process; these are ownership/progress tests, not
// observations of a physical device's absolute runtime-memory ceiling.
package connect

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/urnetwork/connect/protocol"
)

// Process sizing and the runtime soft limit are values. Each call still
// constructs a fresh admission owner; cleanup restores both process values.
func newIosMemoryLiveSettings(t *testing.T) *PlatformTransportSettings {
	t.Helper()
	previousTarget := MemoryBudget()
	previousSoftLimit := debug.SetMemoryLimit(32 * 1024 * 1024)
	SetMemoryBudget(mib(32))
	t.Cleanup(func() {
		SetMemoryBudget(previousTarget)
		debug.SetMemoryLimit(previousSoftLimit)
	})
	settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(32))
	ApplyMobilePlatformTransportMemoryPolicy(settings, mib(32))
	assertIosMemoryLiveSettings(t, settings)
	return settings
}

// Assert the effective profile before any fixture or client socket opens.
func assertIosMemoryLiveSettings(t *testing.T, settings *PlatformTransportSettings) {
	t.Helper()
	if MemoryBudget() != mib(32) || debug.SetMemoryLimit(-1) != 32*1024*1024 {
		t.Fatal("current iOS graph requires effective target32 and soft32")
	}
	budget := settings.PlatformTransportBudget
	if budget == nil || budget.root != budget {
		t.Fatal("current iOS graph lacks its independent carrier owner")
	}
	stats := budget.Stats()
	if stats.TotalByteCount != mib(8) || stats.MaxTransportCount != 16 {
		t.Fatalf("current iOS carrier profile=%+v", stats)
	}
	config := newPlatformQuicConfig(settings, 1)
	if settings.H1BudgetByteCount != kib(256) || settings.H3BudgetByteCount != kib(5696) ||
		settings.H3SocketReadBufferByteCount != kib(64) || settings.H3SocketWriteBufferByteCount != kib(64) ||
		config.InitialStreamReceiveWindow != uint64(kib(128)) || config.InitialConnectionReceiveWindow != uint64(kib(256)) ||
		config.MaxStreamReceiveWindow != uint64(kib(3072)) || config.MaxConnectionReceiveWindow != uint64(mib(4)) ||
		config.Allow0RTT || !settings.h3RetainedByteAccounting ||
		(&PlatformTransport{settings: settings}).h3TransportBufferSize() != 4 {
		t.Fatalf("current iOS carrier windows/claims drifted: settings=%+v config=%+v", settings, config)
	}
	datagrams := settings.H3DatagramSettings
	if datagrams == nil || datagrams.MaxMessageByteCount != 8192 || datagrams.MaxFragmentCount != 1 ||
		datagrams.MaxReassemblyMessageCount != 32 || datagrams.MaxReassemblyByteCount != 64*1024 ||
		datagrams.ProcessReassemblyByteCount != 64*1024 {
		t.Fatalf("current iOS datagram ownership escaped its profile: %+v", datagrams)
	}
}

// Observe real raw-socket closure once, before the carrier releases its claim.
type iosMemoryLivePacketConn struct {
	net.PacketConn
	closeOnce sync.Once
	closeErr  error
	onClose   func()
}

// The native endpoint closes before publishing the test's lifecycle witness.
func (self *iosMemoryLivePacketConn) Close() error {
	self.closeOnce.Do(func() {
		self.closeErr = self.PacketConn.Close()
		self.onClose()
	})
	return self.closeErr
}

// Preserve the raw socket's receive-buffer cap through the close observer.
func (self *iosMemoryLivePacketConn) SetReadBuffer(n int) error {
	return self.PacketConn.(interface{ SetReadBuffer(int) error }).SetReadBuffer(n)
}

// Preserve the raw socket's send-buffer cap through the close observer.
func (self *iosMemoryLivePacketConn) SetWriteBuffer(n int) error {
	return self.PacketConn.(interface{ SetWriteBuffer(int) error }).SetWriteBuffer(n)
}

// Direct QUIC with both physical stream and datagram payload lanes.
func TestIosMemoryLiveH3(t *testing.T) {
	testIosMemoryLiveH3(t, TransportModeH3)
}

// Encoded DNS packets retain the same inner QUIC ownership and payloads.
func TestIosMemoryLiveH3Dns(t *testing.T) {
	testIosMemoryLiveH3(t, TransportModeH3Dns)
}

// Pump-only replies must remain usable under the same current profile.
func TestIosMemoryLiveH3DnsPump(t *testing.T) {
	testIosMemoryLiveH3(t, TransportModeH3DnsPump)
}

// Each top-level mode runs two fresh graphs so explicit close and parent
// cancellation both prove joined release without sharing mutable budgets.
func testIosMemoryLiveH3(t *testing.T, mode TransportMode) {
	t.Helper()
	run := func(cancelParent bool) {
		settings := newIosMemoryLiveSettings(t)
		ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
		defer cancel()
		assertIosMemoryLiveSettings(t, settings)
		port, serverErrors, beginPeerShutdown, closePeer := newIosMemoryQuicEcho(t, mode)
		defer closePeer()
		settings.AltUrl = fmt.Sprintf("https://127.0.0.1:%d", port)
		settings.DnsPort, settings.H3Port = port, port
		settings.DnsPumpHost = "127.0.0.1"
		settings.DnsTlds = [][]byte{[]byte("memory.example.")}
		settings.QuicTlsConfig = &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"memory-matrix"}}
		settings.ModeInitialDelay = 0
		settings.PingTimeout = 30 * time.Second
		budget := settings.PlatformTransportBudget
		unrelated := NewPlatformTransportBudgetForMemoryTarget(mib(32))
		unrelatedBase := unrelated.StatsWithRoot()
		want := settings.H3BudgetByteCount
		if mode != TransportModeH3 {
			want += kib(144)
		}
		assertClaim := func() {
			t.Helper()
			stats := budget.StatsWithRoot()
			if stats.Budget.TotalByteCount != mib(8) || stats.Root != stats.Budget ||
				stats.Budget.UsedByteCount != want || stats.Budget.UsedTransportCount != 1 ||
				stats.Budget.ReservedByteCount-stats.Budget.ReleasedByteCount != want ||
				unrelated.StatsWithRoot() != unrelatedBase {
				t.Errorf("%s cancel=%t: complete current-profile claim: want=%d got=%+v", mode, cancelParent, want, stats)
			}
		}
		var opened, live, datagrams, streams atomic.Int32
		settings.H3PacketConnFactory = func(context.Context) (net.PacketConn, error) {
			assertClaim()
			socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			opened.Add(1)
			live.Add(1)
			return &iosMemoryLivePacketConn{PacketConn: socket, onClose: func() {
				assertClaim()
				live.Add(-1)
			}}, nil
		}
		settings.H3SendLaneObserver = func(_ []byte, datagram bool) {
			if datagram {
				datagrams.Add(1)
			} else {
				streams.Add(1)
			}
		}
		routes := make(chan Route, 1)
		settings.SendRouteObserver = func(_ Transport, route Route, connected bool) {
			if connected {
				select {
				case routes <- route:
				default:
				}
			}
		}
		strategy := NewClientStrategyWithDefaults(ctx)
		defer strategy.Close()
		routeManager := NewRouteManager(ctx, "ios-memory-live")
		reader := routeManager.OpenMultiRouteReader(DestinationId(NewId()))
		defer routeManager.CloseMultiRouteReader(reader)
		carrierReader, ok := reader.(transferCarrierMultiRouteReader)
		if !ok {
			t.Fatal("current H3 reader does not expose its exact receive lane")
		}
		assertIosMemoryLiveSettings(t, settings)
		transport := NewPlatformTransportWithTargetMode(ctx, strategy, routeManager, "https://127.0.0.1",
			&ClientAuth{ByJwt: "synthetic-ios-memory", InstanceId: NewId(), AppVersion: "test"}, mode, settings)
		defer func() {
			cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
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
		if cap(route) != 4 || opened.Load() != 1 || live.Load() != 1 {
			t.Fatalf("%s: route/socket graph depth=%d opened=%d live=%d", mode, cap(route), opened.Load(), live.Load())
		}
		assertClaim()
		for i := range 16 {
			payloadBytes := 500
			if i%2 != 0 {
				payloadBytes = 4096
			}
			message, err := ProtoMarshal(&protocol.TransferFrame{
				TransferPath: &protocol.TransferPath{DestinationId: NewId().Bytes()},
				Pack: &protocol.Pack{MessageId: NewId().Bytes(), SequenceId: NewId().Bytes(), Frames: []*protocol.Frame{{
					MessageType: protocol.MessageType_TestSimpleMessage, MessageBytes: bytes.Repeat([]byte{byte(i)}, payloadBytes),
				}}},
			})
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
			response, disposition, err := carrierReader.readWithCarrier(ctx, 5*time.Second)
			if err != nil {
				t.Fatalf("%s cancel=%t exchange=%d bytes=%d: %v", mode, cancelParent, i, payloadBytes, err)
			}
			matched := bytes.Equal(response, expected)
			MessagePoolReturn(response)
			if !matched {
				t.Fatalf("%s exchange=%d changed payload", mode, i)
			}
			wantReliability := CarrierReliabilityUnreliable
			if payloadBytes == 4096 {
				wantReliability = CarrierReliabilityReliable
			}
			if disposition.transportType != transportTypeFromMode(mode) || disposition.reliability != wantReliability {
				t.Fatalf("%s exchange=%d payload=%d used the wrong receive lane: %+v", mode, i, payloadBytes, disposition)
			}
			assertClaim()
		}
		if selected, _ := transport.activeMode(); selected != mode {
			t.Fatalf("selected=%s want=%s", selected, mode)
		}
		cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		beginPeerShutdown()
		if cancelParent {
			cancel()
			select {
			case <-transport.Done():
			case <-cleanupCtx.Done():
				t.Fatal("parent cancellation did not independently join the H3 graph")
			}
		}
		if err := transport.CloseAndWait(cleanupCtx); err != nil {
			t.Fatal(err)
		}
		stats := budget.StatsWithRoot()
		if datagrams.Load() == 0 || streams.Load() == 0 || live.Load() != 0 ||
			stats.Budget.UsedByteCount != 0 || stats.Budget.UsedTransportCount != 0 ||
			stats.Budget.ReservedByteCount != stats.Budget.ReleasedByteCount || stats.Root != stats.Budget ||
			unrelated.StatsWithRoot() != unrelatedBase {
			t.Fatalf("%s cancel=%t: joined graph streams=%d datagrams=%d sockets=%d claims=%+v",
				mode, cancelParent, streams.Load(), datagrams.Load(), live.Load(), stats)
		}
	}
	for _, cancelParent := range []bool{false, true} {
		run(cancelParent)
	}
}

// A standalone current-profile API graph uses its own lifecycle owner.
func TestIosMemoryLiveAltH3(t *testing.T) {
	testIosMemoryLiveAlt(t, false)
}

// The DNS API graph separately admits translation and returns it on close.
func TestIosMemoryLiveAltWhodis(t *testing.T) {
	testIosMemoryLiveAlt(t, true)
}

// A real HTTP/3 peer echoes uploaded bytes through the selected packet codec.
// The fixture server is joined in cleanup and is not a device-memory sample.
func newIosMemoryAltEcho(t *testing.T, whodis bool) (*testAltServer, func(), func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	certPem, keyPem, err := selfSign([]string{testAltApiHost}, "ios-memory-echo", time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPem) {
		t.Fatal("invalid synthetic certificate")
	}
	peer := &testAltServer{rootCAs: roots}
	socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	raw := socket
	t.Cleanup(func() { raw.Close() })
	peer.altUrl = "https://" + socket.LocalAddr().String()
	if whodis {
		settings := DefaultPacketTranslationSettings()
		settings.DnsTlds = [][]byte{[]byte(testAltDnsTld)}
		translation, err := NewPacketTranslation(ctx, PacketTranslationModeDecode53, raw, settings)
		if err != nil {
			t.Fatal(err)
		}
		socket = translation
		t.Cleanup(func() { translation.Close() })
	}
	quicTransport := &quic.Transport{Conn: socket}
	t.Cleanup(func() { quicTransport.Close() })
	listener, err := quicTransport.Listen(&tls.Config{
		Certificates: []tls.Certificate{cert}, NextProtos: []string{http3.NextProtoH3},
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			peer.noteClientHello(hello)
			return nil, nil
		},
	}, &quic.Config{MaxIdleTimeout: 30 * time.Second})
	if err != nil {
		socket.Close()
		t.Fatal(err)
	}
	peerErrors := newIosMemoryPeerErrors()
	recordError := peerErrors.record
	var stateLock sync.Mutex
	var handlerWorkers sync.WaitGroup
	peerClosed := false
	server := &http3.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		admitted := func() bool {
			stateLock.Lock()
			defer stateLock.Unlock()
			if peerClosed {
				return false
			}
			handlerWorkers.Add(1)
			return true
		}()
		if !admitted {
			return
		}
		defer handlerWorkers.Done()
		if request.Host != testAltApiHost || request.Method != http.MethodPost {
			recordError(fmt.Errorf("unexpected current API request %s %s", request.Method, request.Host))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, altMaxUploadBytes+1))
		if err != nil || len(body) > altMaxUploadBytes {
			if err == nil {
				err = fmt.Errorf("current API upload exceeded its bound: %d", len(body))
			}
			recordError(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		written, err := w.Write(body)
		if err == nil && written != len(body) {
			err = io.ErrShortWrite
		}
		recordError(err)
	})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		recordError(server.ServeListener(listener))
	}()
	beginShutdown := peerErrors.beginShutdown
	var closeOnce sync.Once
	closePeer := func() {
		closeOnce.Do(func() {
			beginShutdown()
			func() {
				stateLock.Lock()
				defer stateLock.Unlock()
				peerClosed = true
			}()
			cancel()
			server.Close()
			listener.Close()
			quicTransport.Close()
			socket.Close()
			<-done
			handlerWorkers.Wait()
			select {
			case err := <-peerErrors.errors:
				t.Errorf("current API peer failed during graph or teardown: %v", err)
			default:
			}
		})
	}
	t.Cleanup(closePeer)
	return peer, beginShutdown, closePeer
}

// Refusal and a canceled request must not create sockets or consume another
// owner's claim. The same real route must remain usable immediately afterward.
func testIosMemoryLiveAlt(t *testing.T, whodis bool) {
	t.Helper()
	run := func(cancelParent bool) {
		deviceSettings := newIosMemoryLiveSettings(t)
		deviceClaim := deviceSettings.PlatformTransportBudget.register(platformTransportBudgetH3Explicit, deviceSettings.H3BudgetByteCount, true)
		if !deviceClaim.TryAcquire() {
			t.Fatal("current-profile device claim did not fit")
		}
		defer deviceClaim.Release()
		deviceBase := deviceSettings.PlatformTransportBudget.StatsWithRoot()
		apiBudget := NewPlatformTransportBudgetForMemoryTarget(mib(32))
		if apiBudget == deviceSettings.PlatformTransportBudget || apiBudget.root != apiBudget ||
			apiBudget.Stats().TotalByteCount != mib(8) || apiBudget.Stats().MaxTransportCount != 16 {
			t.Fatal("API did not receive a separate current-profile owner")
		}
		ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
		defer cancel()
		requestCtx := context.WithValue(ctx, platformTransportNestedBudgetContextKey{}, apiBudget)
		assertIosMemoryLiveSettings(t, deviceSettings)
		strategySettings := DefaultClientStrategySettings()
		strategySettings.EnableNormal, strategySettings.EnableResilient = false, false
		strategySettings.ExpandExtenderProfileCount = 0
		strategySettings.DnsTlds = [][]byte{[]byte(testAltDnsTld)}
		policy := newExtenderQuicMemoryPolicy(requestCtx, &strategySettings.ConnectSettings)
		want := kib(1664)
		if whodis {
			policy.packetTranslationSettings()
			want += kib(144)
		}
		if policy.budget != apiBudget || policy.byteCount != want || !policy.usesSlot || policy.unbudgeted ||
			policy.quicConfig.MaxConnectionReceiveWindow != uint64(kib(512)) ||
			policy.quicConfig.MaxStreamReceiveWindow != uint64(kib(256)) || policy.quicConfig.Allow0RTT {
			t.Fatalf("standalone current-profile API policy=%+v", policy)
		}
		peer, beginPeerShutdown, closePeer := newIosMemoryAltEcho(t, whodis)
		defer closePeer()
		strategySettings.AltUrl = peer.altUrl
		strategySettings.ConnectSettings.TlsConfig = &tls.Config{RootCAs: peer.rootCAs, MinVersion: tls.VersionTLS13}
		assertDevice := func() {
			t.Helper()
			if got := deviceSettings.PlatformTransportBudget.StatsWithRoot(); got != deviceBase {
				t.Errorf("API changed another owner: before=%+v after=%+v", deviceBase, got)
			}
		}
		var opened, live atomic.Int32
		var failNext atomic.Bool
		marker := errors.New("synthetic admitted socket failure")
		strategySettings.ConnectSettings.DialContextSettings = &DialContextSettings{
			PacketConnFactory: func(context.Context) (net.PacketConn, error) {
				stats := apiBudget.Stats()
				if stats.UsedByteCount != want || stats.UsedTransportCount != 1 {
					t.Errorf("API socket opened before its complete claim: %+v", stats)
				}
				assertDevice()
				if failNext.Swap(false) {
					return nil, marker
				}
				socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
				if err != nil {
					return nil, err
				}
				opened.Add(1)
				live.Add(1)
				return &iosMemoryLivePacketConn{PacketConn: socket, onClose: func() {
					if apiBudget.Stats().UsedByteCount != want {
						t.Error("API released ownership before raw socket close")
					}
					live.Add(-1)
				}}, nil
			},
		}
		assertIosMemoryLiveSettings(t, deviceSettings)
		strategy := NewClientStrategy(ctx, strategySettings)
		defer strategy.Close()
		name := "alt h3"
		if whodis {
			name = "alt whodis"
		}
		client := testAltDialer(t, strategy, name).HttpClient()
		bounded := client.Transport.(*altQuicBoundedTransport)
		defer bounded.Close()
		request := func(ctx context.Context, payload []byte) (*http.Response, error) {
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+testAltApiHost+"/echo", bytes.NewReader(payload))
			if err != nil {
				return nil, err
			}
			return client.Do(request)
		}
		filler := apiBudget.register(platformTransportBudgetExtender, mib(8), true)
		defer filler.Release()
		if !filler.TryAcquire() {
			t.Fatal("could not fill the API owner")
		}
		response, err := request(requestCtx, []byte("refused"))
		if response != nil {
			response.Body.Close()
		}
		if !errors.Is(err, errExtenderMemoryBudget) || opened.Load() != 0 || apiBudget.Stats().UsedByteCount != mib(8) {
			t.Fatalf("full current API owner opened a socket: response=%v err=%v opened=%d", response, err, opened.Load())
		}
		filler.Release()
		canceledCtx, stopRequest := context.WithCancel(requestCtx)
		stopRequest()
		response, err = request(canceledCtx, []byte("canceled"))
		if response != nil {
			response.Body.Close()
		}
		if !errors.Is(err, context.Canceled) || opened.Load() != 0 || apiBudget.Stats().UsedByteCount != 0 {
			t.Fatalf("canceled current API request opened a socket: %v", err)
		}
		failNext.Store(true)
		response, err = request(requestCtx, []byte("failed"))
		if response != nil {
			response.Body.Close()
		}
		if !errors.Is(err, marker) || opened.Load() != 0 || apiBudget.Stats().UsedByteCount != 0 {
			t.Fatalf("failed current API dial retained ownership: %v %+v", err, apiBudget.Stats())
		}
		for i := range 4 {
			payload := bytes.Repeat([]byte{byte(i + 1)}, 4096+i*257)
			response, err := request(requestCtx, payload)
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(io.LimitReader(response.Body, int64(len(payload)+1)))
			closeErr := response.Body.Close()
			if readErr != nil || closeErr != nil || response.StatusCode != http.StatusOK || !bytes.Equal(body, payload) {
				t.Fatalf("%s cancel=%t exchange=%d: read=%v close=%v status=%d body=%d", name, cancelParent, i, readErr, closeErr, response.StatusCode, len(body))
			}
			if stats := apiBudget.Stats(); stats.UsedByteCount != want || stats.UsedTransportCount != 1 {
				t.Fatalf("API traffic changed its admitted graph: %+v", stats)
			}
			assertDevice()
		}
		testAltAssertClientHello(t, peer)
		if opened.Load() != 1 || live.Load() != 1 {
			t.Fatalf("API route opened=%d live=%d", opened.Load(), live.Load())
		}
		connection := bounded.connection(net.JoinHostPort(testAltApiHost, "443"))
		if connection == nil {
			t.Fatal("the selected API route has no live QUIC connection")
		}
		beginPeerShutdown()
		if cancelParent {
			cancel()
		} else {
			strategy.Close()
		}
		cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		select {
		case <-connection.Context().Done():
		case <-cleanupCtx.Done():
			t.Fatal("API lifecycle did not close its native connection")
		}
		for {
			notify := apiBudget.CapacityNotify()
			stats := apiBudget.Stats()
			if stats.UsedByteCount == 0 {
				if stats.UsedTransportCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount || live.Load() != 0 {
					t.Fatalf("API released claim without joined raw graph: %+v sockets=%d", stats, live.Load())
				}
				break
			}
			select {
			case <-notify:
			case <-cleanupCtx.Done():
				t.Fatalf("API claim did not release after native close: %+v", apiBudget.Stats())
			}
		}
		assertDevice()
		deviceClaim.Release()
		if stats := deviceSettings.PlatformTransportBudget.Stats(); stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
			t.Fatalf("device companion claim retained ownership: %+v", stats)
		}
	}
	for _, cancelParent := range []bool{false, true} {
		run(cancelParent)
	}
}
