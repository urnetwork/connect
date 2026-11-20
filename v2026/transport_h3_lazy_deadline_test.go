// H3 keeps native stream deadlines while pacing pings without a timer reset
// after every batch. A real QUIC peer exchanges one payload at a time.
package connect

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
)

// Counts the stream deadlines, stream writes and ping timer arms.
type h3LazyDeadlineCounts struct {
	writeDeadlineCount int
	readDeadlineCount  int
	streamWriteCount   int
	pingTimerArmCount  int
}

// stateLock guards counts, which the owner's hooks update.
type h3LazyDeadlineStats struct {
	stateLock sync.Mutex
	counts    h3LazyDeadlineCounts
}

// Installs the counting hooks.
func (self *h3LazyDeadlineStats) observe(settings *PlatformTransportSettings) {
	settings.h3StreamDeadlineForTest = func(write bool, deadline time.Time) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if write {
			self.counts.writeDeadlineCount += 1
		} else {
			self.counts.readDeadlineCount += 1
		}
	}
	settings.beforeH3StreamWriteForTest = func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.counts.streamWriteCount += 1
	}
	settings.pingTimerArmForTest = func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.counts.pingTimerArmCount += 1
	}
}

// Forgets the counts so far.
func (self *h3LazyDeadlineStats) reset() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.counts = h3LazyDeadlineCounts{}
}

// The counts since the last reset.
func (self *h3LazyDeadlineStats) snapshot() h3LazyDeadlineCounts {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.counts
}

// Every stream operation retains its own deadline; only ping timer resets
// are coalesced while payloads are busy.
func TestH3NativeDeadlinesKeepPingTimerPacing(t *testing.T) {
	forEachIpVersion(t, func(t *testing.T, ipVersion int) {
		certPem, keyPem, err := selfSign(
			[]string{testLoopbackIp(ipVersion)},
			testLoopbackIp(ipVersion),
			24*time.Hour,
			24*time.Hour,
		)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := tls.X509KeyPair(certPem, keyPem)
		if err != nil {
			t.Fatal(err)
		}
		const nextProto = "urnetwork-platform-lazy-deadline-test"
		listener, err := quic.ListenAddrEarly(
			testLoopbackHostPort(ipVersion, 0),
			&tls.Config{
				Certificates: []tls.Certificate{cert},
				NextProtos:   []string{nextProto},
			},
			&quic.Config{MaxIdleTimeout: 30 * time.Second},
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = listener.Close()
		})

		const sendCount = 24
		const receiveCount = 12
		payload := bytes.Repeat([]byte{0x5a}, 256)
		serverCtx, serverCancel := context.WithCancel(t.Context())
		defer serverCancel()
		serverErrors := make(chan error, 2)
		serverReceived := make(chan struct{}, sendCount)
		serverSend := make(chan struct{})
		framerSettings := DefaultFramerSettings(int(DefaultClientSettings().MinimumMessageLenLimit()))
		go func() {
			connection, acceptErr := listener.Accept(serverCtx)
			if acceptErr != nil {
				serverErrors <- acceptErr
				return
			}
			stream, acceptErr := connection.AcceptStream(serverCtx)
			if acceptErr != nil {
				serverErrors <- acceptErr
				return
			}
			framer := NewFramer(framerSettings)
			authBytes, readErr := framer.Read(stream)
			if readErr != nil {
				serverErrors <- readErr
				return
			}
			// an exact echo: a stream-only legacy peer
			writeErr := framer.Write(stream, authBytes)
			MessagePoolReturn(authBytes)
			if writeErr != nil {
				serverErrors <- writeErr
				return
			}
			go func() {
				readFramer := NewFramer(framerSettings)
				for {
					message, readErr := readFramer.Read(stream)
					if readErr != nil {
						return
					}
					MessagePoolReturn(message)
					serverReceived <- struct{}{}
				}
			}()
			for {
				select {
				case <-serverCtx.Done():
					return
				case <-serverSend:
				}
				if writeErr := framer.Write(stream, payload); writeErr != nil {
					serverErrors <- writeErr
					return
				}
			}
		}()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		stats := &h3LazyDeadlineStats{}
		clientReceived := make(chan struct{}, receiveCount)
		settings := testingPlatformTransportSettings()
		settings.H3Port = listener.Addr().(*net.UDPAddr).Port
		settings.resolveH3AddrsForTest = testingH3LoopbackResolver(ipVersion, settings.H3Port)
		settings.QuicTlsConfig = &tls.Config{
			InsecureSkipVerify: true, // test-only self-signed endpoint
			NextProtos:         []string{nextProto},
		}
		settings.FramerSettings = framerSettings
		settings.afterH3ReceiveEnqueueForTest = func([]byte) {
			clientReceived <- struct{}{}
		}
		stats.observe(settings)
		routeManager := NewRouteManager(ctx, "h3-lazy-deadline")
		transport := NewPlatformTransportWithTargetMode(
			ctx,
			NewClientStrategyWithDefaults(ctx),
			routeManager,
			"https://"+testLoopbackHost(ipVersion),
			&ClientAuth{
				ByJwt:      "testing",
				InstanceId: NewId(),
				AppVersion: "testing",
			},
			TransportModeH3,
			settings,
		)
		t.Cleanup(transport.Close)
		if !testingWaitForActiveMode(transport, TransportModeH3, 5*time.Second) {
			t.Fatal("the H3 transport was never elected")
		}
		stats.reset()

		writer := routeManager.OpenMultiRouteWriter(DestinationId(NewId()))
		defer routeManager.CloseMultiRouteWriter(writer)
		startTime := time.Now()
		var writeCount atomic.Int64
		for range sendCount {
			message := MessagePoolGet(len(payload))
			copy(message, payload)
			if writeErr := writer.Write(ctx, message, 5*time.Second); writeErr != nil {
				MessagePoolReturn(message)
				t.Fatalf("route write: %v", writeErr)
			}
			writeCount.Add(1)
			select {
			case <-serverReceived:
			case serverErr := <-serverErrors:
				t.Fatal(serverErr)
			case <-time.After(5 * time.Second):
				t.Fatalf("the peer did not receive payload %d", writeCount.Load())
			}
		}
		for i := range receiveCount {
			select {
			case serverSend <- struct{}{}:
			case serverErr := <-serverErrors:
				t.Fatal(serverErr)
			}
			select {
			case <-clientReceived:
			case <-time.After(5 * time.Second):
				t.Fatalf("the owner did not receive payload %d", i)
			}
		}

		elapsed := time.Since(startTime)

		got := stats.snapshot()
		if got.streamWriteCount != sendCount {
			t.Fatalf("%d payloads took %d stream writes, want one each", sendCount, got.streamWriteCount)
		}
		if got.writeDeadlineCount < sendCount || got.readDeadlineCount < receiveCount {
			t.Fatalf("native deadline bounds missing: writes=%d/%d reads=%d/%d", got.writeDeadlineCount, sendCount, got.readDeadlineCount, receiveCount)
		}
		if pingTimerArmAllowance := int(elapsed / settings.PingTimeout); pingTimerArmAllowance < got.pingTimerArmCount {
			t.Fatalf("in %s, %d stream writes armed the ping timer %d times, want at most %d", elapsed, sendCount, got.pingTimerArmCount, pingTimerArmAllowance)
		}
	})
}
