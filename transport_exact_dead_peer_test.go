// The platform transport cuts a dead peer off at the bounds it had before its
// deadlines went lazy (connect caad1de6). A read ends ReadTimeout after it
// began, so a silent peer is cut off ReadTimeout after its last frame, and a
// stalled write fails WriteTimeout after it began, on H1+, WebSocket and H3.
// H1 runs against the synthetic in-memory peer in virtual time, so its
// boundaries are exact; H3 runs against a loopback QUIC peer in real time,
// within a tolerance well under the lazy slack it rules out.
package connect

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
	quic "github.com/quic-go/quic-go"
)

// How long after since the fixture's carrier was withdrawn: exactly bound when
// it is still connected 1ns before since plus bound and withdrawn at it. One
// connected past the bound is followed to its withdrawal, in 10ms steps up to
// three bounds. One withdrawn sooner reads as 1ns less than the bound.
func h1WithdrawnAfter(fixture *h1LivenessFixture, since time.Time, bound time.Duration) time.Duration {
	time.Sleep(time.Until(since.Add(bound)) - time.Nanosecond)
	synctest.Wait()
	if !fixture.transport.IsConnected() {
		return time.Since(since)
	}
	time.Sleep(time.Nanosecond)
	synctest.Wait()
	for fixture.transport.IsConnected() && time.Since(since) < 3*bound {
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
	}
	return time.Since(since)
}

// A silent peer is cut off exactly ReadTimeout after its last frame, never
// before, wherever that frame lands. With the fixture's 3s ReadTimeout it
// lands at the connection's start, and before, at and after the points where
// a lazy deadline armed at the start would have kept or re-armed itself.
func TestH1SilentPeerCutOffExactlyReadTimeoutAfterLastFrame(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	for _, h1Plus := range []bool{true, false} {
		for _, lastFrameAfter := range []time.Duration{0, time.Second / 2, time.Second, 3 * time.Second / 2, 5 * time.Second / 2} {
			what := map[bool]string{true: "h1+", false: "websocket"}[h1Plus] + " silent peer, last frame at " + lastFrameAfter.String()
			// measured in the bubble and judged outside it, so that every case
			// reports
			var readTimeout, cutOff time.Duration
			synctest.Test(t, func(t *testing.T) {
				fixture := newH1LivenessFixture(t, withH1Plus(h1Plus))
				if 0 < lastFrameAfter {
					time.Sleep(lastFrameAfter)
					if err := fixture.peer.WriteMessage(websocket.BinaryMessage, nil); err != nil {
						t.Fatal(err)
					}
					synctest.Wait()
				}
				readTimeout = fixture.transport.settings.ReadTimeout
				cutOff = h1WithdrawnAfter(fixture, time.Now(), readTimeout)
				if cutOff != readTimeout {
					return
				}
				fixture.assertWithdrawn(t)
				if fixture.client.writeTimedOut.Load() {
					t.Errorf("%s: a writer timeout ended the silent connection", what)
				}
			})
			if cutOff != readTimeout {
				t.Errorf("%s: cut off %s after the last frame, want exactly %s", what, cutOff, readTimeout)
			}
		}
	}
}

// One kind of carrier write, stalled on the paused peer.
type h1StalledWriteKind struct {
	name string
	// settings beyond the fixture's
	configure func(*PlatformTransportSettings)
	// Stalls the write half a second after the connection's last completed
	// write. Returns when it began and a witness of its payload, nil when the
	// owner has none to return.
	stall func(t *testing.T, fixture *h1LivenessFixture) (time.Time, []byte)
}

// Starts speed mode, whose echo is the one message the paused peer reads, and
// returns when that write completed.
func startH1SpeedTestOnPausedPeer(t *testing.T, fixture *h1LivenessFixture) time.Time {
	t.Helper()
	if err := fixture.peer.WriteMessage(websocket.BinaryMessage, []byte{TransportControlSpeedStart, 0, 0, 0, 1}); err != nil {
		t.Fatal(err)
	}
	synctest.Wait()
	select {
	case <-fixture.peerReadBlocked:
	default:
		t.Fatal("the paused peer did not read the speed echo")
	}
	return time.Now()
}

// Waits for the ping that a write completed at lastWrite makes due, which
// stalls on the paused peer, and returns when it began.
func stallH1Ping(t *testing.T, fixture *h1LivenessFixture, lastWrite time.Time) time.Time {
	t.Helper()
	pingTimeout := fixture.transport.settings.PingTimeout
	ping := WakeupTime(lastWrite.Add(pingTimeout), pingTimeout)
	time.Sleep(time.Until(ping))
	synctest.Wait()
	if fixture.client.writesInProgress.Load() != 1 {
		t.Fatal("the ping did not stall on the paused peer")
	}
	return ping
}

// The writes of every kind the carrier writer makes. Pings and reads stay out
// of the way unless the kind is a ping; a kind that is a ping pings half a
// second after the last write.
func h1StalledWriteKinds() []h1StalledWriteKind {
	quiet := func(settings *PlatformTransportSettings) {
		settings.ReadTimeout = time.Minute
		settings.PingTimeout = time.Minute
	}
	pinging := func(settings *PlatformTransportSettings) {
		settings.ReadTimeout = time.Minute
		settings.PingTimeout = time.Second / 2
	}
	return []h1StalledWriteKind{
		{
			name:      "ready batch payload",
			configure: quiet,
			stall: func(t *testing.T, fixture *h1LivenessFixture) (time.Time, []byte) {
				stopH1BackpressurePeerReads(t, fixture)
				time.Sleep(time.Second / 2)
				witness := blockH1BackpressurePayload(t, fixture)
				return time.Now(), witness
			},
		},
		{
			name:      "ping",
			configure: pinging,
			stall: func(t *testing.T, fixture *h1LivenessFixture) (time.Time, []byte) {
				stopH1BackpressurePeerReads(t, fixture)
				return stallH1Ping(t, fixture, time.Now()), nil
			},
		},
		{
			name:      "control echo",
			configure: quiet,
			stall: func(t *testing.T, fixture *h1LivenessFixture) (time.Time, []byte) {
				stopH1BackpressurePeerReads(t, fixture)
				time.Sleep(time.Second / 2)
				// a latency probe, which the reader hands to the writer to echo
				if err := fixture.peer.WriteMessage(websocket.BinaryMessage, make([]byte, 16)); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				if fixture.client.writesInProgress.Load() != 1 {
					t.Fatal("the control echo did not stall on the paused peer")
				}
				return time.Now(), nil
			},
		},
		{
			name:      "speed test payload",
			configure: quiet,
			stall: func(t *testing.T, fixture *h1LivenessFixture) (time.Time, []byte) {
				startH1SpeedTestOnPausedPeer(t, fixture)
				time.Sleep(time.Second / 2)
				witness := blockH1BackpressurePayload(t, fixture)
				return time.Now(), witness
			},
		},
		{
			name:      "speed test ping",
			configure: pinging,
			stall: func(t *testing.T, fixture *h1LivenessFixture) (time.Time, []byte) {
				lastWrite := startH1SpeedTestOnPausedPeer(t, fixture)
				return stallH1Ping(t, fixture, lastWrite), nil
			},
		},
	}
}

// A stalled write fails exactly WriteTimeout after it began, never before,
// whatever kind of write it is. With the fixture's 2s WriteTimeout each begins
// half a second after the write before it, while a lazy deadline armed for
// that write would still have had more than WriteTimeout left.
func TestH1StalledWriteFailsExactlyWriteTimeoutAfterItBegan(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	for _, h1Plus := range []bool{true, false} {
		for _, kind := range h1StalledWriteKinds() {
			what := map[bool]string{true: "h1+", false: "websocket"}[h1Plus] + " stalled " + kind.name
			// measured in the bubble and judged outside it, so that every case
			// reports
			var writeTimeout, cutOff time.Duration
			synctest.Test(t, func(t *testing.T) {
				fixture := newH1LivenessFixtureWithBackpressure(t, true, 0, withH1Plus(h1Plus), kind.configure)
				stallStart, witness := kind.stall(t, fixture)
				writeTimeout = fixture.transport.settings.WriteTimeout
				cutOff = h1WithdrawnAfter(fixture, stallStart, writeTimeout)
				if witness != nil && !MessagePoolReturn(witness) {
					t.Errorf("%s: the ended write retained its payload", what)
				}
				if cutOff != writeTimeout {
					return
				}
				fixture.assertWithdrawn(t)
				if !fixture.client.writeTimedOut.Load() || fixture.client.writesInProgress.Load() != 0 {
					t.Errorf("%s: did not end as a write timeout", what)
				}
			})
			if cutOff != writeTimeout {
				t.Errorf("%s: failed %s after it began, want exactly %s", what, cutOff, writeTimeout)
			}
		}
	}
}

// A stream-only QUIC peer on the loopback. It echoes the authentication, then
// reads the owner's stream unless told not to, and writes what it is given.
type h3ExactPeer struct {
	listener *quic.EarlyListener
	addr     *net.UDPAddr
	errs     chan error
	received chan struct{}
	send     chan []byte
	framer   *Framer
}

// Listens on the loopback. With reading off, the peer never reads past the
// authentication, and its small receive windows stall the owner's writes.
func newH3ExactPeer(t *testing.T, ipVersion int, nextProto string, reading bool) *h3ExactPeer {
	t.Helper()
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
	quicConfig := &quic.Config{MaxIdleTimeout: 30 * time.Second}
	if !reading {
		quicConfig.InitialStreamReceiveWindow = 16 * 1024
		quicConfig.MaxStreamReceiveWindow = 16 * 1024
		quicConfig.InitialConnectionReceiveWindow = 32 * 1024
		quicConfig.MaxConnectionReceiveWindow = 32 * 1024
	}
	listener, err := quic.ListenAddrEarly(
		testLoopbackHostPort(ipVersion, 0),
		&tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{nextProto},
		},
		quicConfig,
	)
	if err != nil {
		t.Fatal(err)
	}
	peer := &h3ExactPeer{
		listener: listener,
		addr:     listener.Addr().(*net.UDPAddr),
		errs:     make(chan error, 2),
		received: make(chan struct{}, 1024),
		send:     make(chan []byte),
		framer:   NewFramer(DefaultFramerSettings(int(DefaultClientSettings().MinimumMessageLenLimit()))),
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		listener.Close()
	})
	go func() {
		connection, err := listener.Accept(ctx)
		if err != nil {
			peer.errs <- err
			return
		}
		stream, err := connection.AcceptStream(ctx)
		if err != nil {
			peer.errs <- err
			return
		}
		authBytes, err := peer.framer.Read(stream)
		if err != nil {
			peer.errs <- err
			return
		}
		// an exact echo: a stream-only legacy peer
		err = peer.framer.Write(stream, authBytes)
		MessagePoolReturn(authBytes)
		if err != nil {
			peer.errs <- err
			return
		}
		if reading {
			go func() {
				readFramer := NewFramer(DefaultFramerSettings(int(DefaultClientSettings().MinimumMessageLenLimit())))
				for {
					message, err := readFramer.Read(stream)
					if err != nil {
						return
					}
					MessagePoolReturn(message)
					select {
					case peer.received <- struct{}{}:
					default:
					}
				}
			}()
		}
		for {
			select {
			case <-ctx.Done():
				return
			case message := <-peer.send:
				if err := peer.framer.Write(stream, message); err != nil {
					peer.errs <- err
					return
				}
			}
		}
	}()
	return peer
}

// When an owner's H3 carrier was published and withdrawn, from its send route.
type h3CarrierLifetime struct {
	stateLock sync.Mutex
	published time.Time
	withdrawn time.Time
	changed   chan struct{}
}

// Wraps settings to note the carrier's lifetime.
func observeH3CarrierLifetime(settings *PlatformTransportSettings) *h3CarrierLifetime {
	lifetime := &h3CarrierLifetime{changed: make(chan struct{}, 4)}
	settings.SendRouteObserver = func(_ Transport, _ Route, active bool) {
		func() {
			lifetime.stateLock.Lock()
			defer lifetime.stateLock.Unlock()
			if active && lifetime.published.IsZero() {
				lifetime.published = time.Now()
			} else if !active && lifetime.withdrawn.IsZero() {
				lifetime.withdrawn = time.Now()
			}
		}()
		select {
		case lifetime.changed <- struct{}{}:
		default:
		}
	}
	return lifetime
}

// Waits up to timeout for the first carrier's withdrawal and returns when it
// came, zero when it did not.
func (self *h3CarrierLifetime) waitWithdrawn(timeout time.Duration) time.Time {
	deadline := time.After(timeout)
	for {
		withdrawn := func() time.Time {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return self.withdrawn
		}()
		if !withdrawn.IsZero() {
			return withdrawn
		}
		select {
		case <-self.changed:
		case <-deadline:
			return time.Time{}
		}
	}
}

// Runs a stream-only H3 owner against peer, with the listed settings.
func newH3ExactOwner(t *testing.T, ipVersion int, port int, nextProto string, configure func(*PlatformTransportSettings)) (*PlatformTransport, *RouteManager, *h3CarrierLifetime) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := testingPlatformTransportSettings()
	settings.H3Port = port
	settings.resolveH3AddrsForTest = testingH3LoopbackResolver(ipVersion, port)
	settings.QuicTlsConfig = &tls.Config{
		InsecureSkipVerify: true, // test-only self-signed endpoint
		NextProtos:         []string{nextProto},
	}
	settings.FramerSettings = DefaultFramerSettings(int(DefaultClientSettings().MinimumMessageLenLimit()))
	settings.ReconnectTimeout = time.Hour
	settings.ReadTimeout = time.Minute
	settings.WriteTimeout = time.Minute
	configure(settings)
	lifetime := observeH3CarrierLifetime(settings)
	routeManager := NewRouteManager(ctx, "h3-exact-dead-peer")
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
	t.Cleanup(func() {
		cancel()
		transport.Close()
	})
	if !testingWaitForActiveMode(transport, TransportModeH3, 5*time.Second) {
		t.Fatal("the H3 transport was never elected")
	}
	return transport, routeManager, lifetime
}

// Real time passes between an H3 boundary and the owner's withdrawal: QUIC
// and the teardown run on real goroutines. Well under every lazy slack.
const h3ExactBoundTolerance = 200 * time.Millisecond

// A silent H3 peer is cut off ReadTimeout after its last frame on a
// stream-only connection. The frame arrives as the connection starts, where a
// lazy deadline armed for the first read would have kept its slack.
func TestH3SilentPeerCutOffReadTimeoutAfterLastFrame(t *testing.T) {
	forEachIpVersion(t, func(t *testing.T, ipVersion int) {
		const nextProto = "urnetwork-platform-exact-read-test"
		peer := newH3ExactPeer(t, ipVersion, nextProto, true)
		readTimeout := 900 * time.Millisecond
		var lastFrameNanos atomic.Int64
		_, _, lifetime := newH3ExactOwner(t, ipVersion, peer.addr.Port, nextProto, func(settings *PlatformTransportSettings) {
			settings.ReadTimeout = readTimeout
			settings.afterH3ReceiveEnqueueForTest = func([]byte) {
				lastFrameNanos.Store(time.Now().UnixNano())
			}
		})
		select {
		case peer.send <- bytes.Repeat([]byte{0x5a}, 64):
		case err := <-peer.errs:
			t.Fatal(err)
		}
		withdrawn := lifetime.waitWithdrawn(readTimeout + 5*time.Second)
		if withdrawn.IsZero() {
			t.Fatal("the silent peer was never cut off")
		}
		if lastFrameNanos.Load() == 0 {
			t.Fatal("the owner never received the last frame")
		}
		cutOff := withdrawn.Sub(time.Unix(0, lastFrameNanos.Load()))
		t.Logf("cut off %s after the last frame", cutOff)
		if cutOff < readTimeout || readTimeout+h3ExactBoundTolerance < cutOff {
			t.Fatalf("cut off %s after the last frame, want %s", cutOff, readTimeout)
		}
	})
}

// A stalled H3 stream write fails WriteTimeout after it began. The peer stops
// reading after the authentication, so the owner's first writes fill its
// receive window and the next one stalls just after the first, where a lazy
// deadline armed for the first write would have kept its slack.
func TestH3StalledWriteFailsWriteTimeoutAfterItBegan(t *testing.T) {
	forEachIpVersion(t, func(t *testing.T, ipVersion int) {
		const nextProto = "urnetwork-platform-exact-write-test"
		peer := newH3ExactPeer(t, ipVersion, nextProto, false)
		writeTimeout := 800 * time.Millisecond
		var writeStartNanos atomic.Int64
		_, routeManager, lifetime := newH3ExactOwner(t, ipVersion, peer.addr.Port, nextProto, func(settings *PlatformTransportSettings) {
			settings.WriteTimeout = writeTimeout
			settings.beforeH3StreamWriteForTest = func() {
				writeStartNanos.Store(time.Now().UnixNano())
			}
		})
		writer := routeManager.OpenMultiRouteWriter(DestinationId(NewId()))
		defer routeManager.CloseMultiRouteWriter(writer)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sent := make(chan struct{})
		go func() {
			defer close(sent)
			// far more than the peer's window, until the owner is withdrawn
			for range 1024 {
				if ctx.Err() != nil {
					return
				}
				message := MessagePoolGet(1024)
				if err := writer.Write(ctx, message, 50*time.Millisecond); err != nil {
					MessagePoolReturn(message)
				}
			}
		}()
		withdrawn := lifetime.waitWithdrawn(writeTimeout + 5*time.Second)
		cancel()
		<-sent
		if withdrawn.IsZero() {
			t.Fatal("the stalled write never failed")
		}
		cutOff := withdrawn.Sub(time.Unix(0, writeStartNanos.Load()))
		t.Logf("the stalled write failed %s after it began", cutOff)
		if cutOff < writeTimeout-h3ExactBoundTolerance/4 || writeTimeout+h3ExactBoundTolerance < cutOff {
			t.Fatalf("the stalled write failed %s after it began, want %s", cutOff, writeTimeout)
		}
	})
}
