// The H3 owner re-arms its stream deadlines only when less than their timeout
// is left, and paces its pings from its last write (server CPUOPT2.md §9). A
// stream-only QUIC peer on the loopback echoes the authentication, then trades
// payloads with the owner one at a time.
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

// How far past its WriteTimeout a re-armed stream write deadline reaches.
func h3WriteDeadlineSlack(settings *PlatformTransportSettings) time.Duration {
	return time.Duration(float64(settings.WriteTimeout) * settings.WriteDeadlineSlackFraction)
}

// How far past its ReadTimeout a re-armed stream read deadline reaches.
func h3ReadDeadlineSlack(settings *PlatformTransportSettings) time.Duration {
	return time.Duration(float64(settings.ReadTimeout) * settings.ReadDeadlineSlackFraction)
}

// The stream deadlines and ping timer arms an H3 owner made, and the least and
// most time its write deadline left as each stream write began.
type h3LazyDeadlineCounts struct {
	writeDeadlineCount     int
	readDeadlineCount      int
	writeDeadline          time.Time
	streamWriteCount       int
	leastWriteDeadlineLeft time.Duration
	mostWriteDeadlineLeft  time.Duration
	pingTimerArmCount      int
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
			self.counts.writeDeadline = deadline
		} else {
			self.counts.readDeadlineCount += 1
		}
	}
	settings.beforeH3StreamWriteForTest = func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		counts := &self.counts
		left := time.Until(counts.writeDeadline)
		counts.streamWriteCount += 1
		if counts.streamWriteCount == 1 {
			counts.leastWriteDeadlineLeft = left
			counts.mostWriteDeadlineLeft = left
		} else {
			counts.leastWriteDeadlineLeft = min(counts.leastWriteDeadlineLeft, left)
			counts.mostWriteDeadlineLeft = max(counts.mostWriteDeadlineLeft, left)
		}
	}
	settings.pingTimerArmForTest = func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.counts.pingTimerArmCount += 1
	}
}

// Forgets the counts so far, but not the write deadline in force.
func (self *h3LazyDeadlineStats) reset() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.counts = h3LazyDeadlineCounts{writeDeadline: self.counts.writeDeadline}
}

// The counts since the last reset.
func (self *h3LazyDeadlineStats) snapshot() h3LazyDeadlineCounts {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.counts
}

// Every payload is its own stream batch and every inbound payload its own
// stream read. Within one slack the owner installs each stream deadline at
// most once and arms no ping timer, where it used to install one per batch and
// per read and arm the ping timer per batch. Every stream write still begins
// with at least WriteTimeout left.
func TestH3LazyStreamDeadlinesInstallOncePerSlack(t *testing.T) {
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
		if h3WriteDeadlineSlack(settings) <= 0 || h3ReadDeadlineSlack(settings) <= 0 {
			t.Fatal("the default settings re-arm stream deadlines before every operation")
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
		// one installation per slack that elapsed, so a slow run cannot fail it
		writeDeadlineAllowance := 1 + int(elapsed/h3WriteDeadlineSlack(settings))
		readDeadlineAllowance := 1 + int(elapsed/h3ReadDeadlineSlack(settings))
		if writeDeadlineAllowance < got.writeDeadlineCount || readDeadlineAllowance < got.readDeadlineCount {
			t.Fatalf("in %s, %d stream writes and %d reads installed %d write and %d read deadlines, want at most %d and %d",
				elapsed, sendCount, receiveCount, got.writeDeadlineCount, got.readDeadlineCount,
				writeDeadlineAllowance, readDeadlineAllowance)
		}
		// real time passes between the lazy check and the write it admits
		const checkToWriteAllowance = time.Second
		if got.leastWriteDeadlineLeft < settings.WriteTimeout-checkToWriteAllowance ||
			settings.WriteTimeout+h3WriteDeadlineSlack(settings) < got.mostWriteDeadlineLeft {
			t.Fatalf("stream writes began with %s to %s left, want %s to %s",
				got.leastWriteDeadlineLeft, got.mostWriteDeadlineLeft,
				settings.WriteTimeout, settings.WriteTimeout+h3WriteDeadlineSlack(settings))
		}
		if pingTimerArmAllowance := int(elapsed / settings.PingTimeout); pingTimerArmAllowance < got.pingTimerArmCount {
			t.Fatalf("in %s, %d stream writes armed the ping timer %d times, want at most %d", elapsed, sendCount, got.pingTimerArmCount, pingTimerArmAllowance)
		}
	})
}
