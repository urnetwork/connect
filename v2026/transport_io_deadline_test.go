// Native deadlines distinguish failed I/O from an owner delayed after the
// socket already succeeded. A delayed callback must not poison its successor.
package connect

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/testutils/simnet"
)

// Holds a successful physical operation past its budget, then proves another
// payload still crosses the same live carrier. The gate is below the actual
// H1 owner, after the pipe transferred the bytes and before its call returned.
func testH1SuccessfulIoOwnerDelay(t *testing.T, write bool) {
	t.Helper()
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	for _, h1Plus := range []bool{true, false} {
		synctest.Test(t, func(t *testing.T) {
			const timeout = 2 * time.Second
			fixture := newH1LivenessFixture(t, withH1Plus(h1Plus), func(settings *PlatformTransportSettings) {
				settings.ReadTimeout = time.Minute
				settings.WriteTimeout = time.Minute
				settings.PingTimeout = time.Minute
				if write {
					settings.WriteTimeout = timeout
				} else {
					settings.ReadTimeout = timeout
				}
			})
			completed := make(chan struct{})
			resume := make(chan struct{})
			release := sync.OnceFunc(func() { close(resume) })
			defer release()
			var completions atomic.Int64
			hook := func(wrote bool, n int) {
				if wrote != write || n < 64 {
					return
				}
				if completions.Add(1) == 1 {
					close(completed)
					<-resume
				}
			}
			fixture.client.afterIoForTest.Store(&hook)
			send := func() {
				t.Helper()
				if write {
					fixture.sendMessage(t, h1PlusOwnedTestMessage(0x31, 64))
				} else if err := fixture.peer.WriteMessage(websocket.BinaryMessage, bytes.Repeat([]byte{0x31}, 64)); err != nil {
					t.Fatal(err)
				}
			}
			send()
			<-completed
			// Virtual time crosses the deadline while the completed operation's
			// owner is parked at the explicit post-I/O barrier.
			time.Sleep(2 * timeout)
			synctest.Wait()
			release()
			synctest.Wait()
			fixture.assertConnected(t)
			send()
			synctest.Wait()
			fixture.assertConnected(t)
			if got := completions.Load(); got != 2 {
				t.Fatalf("h1+=%t write=%t: %d successful physical operations, want both payloads", h1Plus, write, got)
			}
			if write && fixture.payloads.Load() != 2 {
				t.Fatalf("h1+=%t: successor payload did not reach the peer", h1Plus)
			}
		})
	}
}

// A completed write remains successful when its caller resumes late.
func TestH1SuccessfulWriteOwnerDelayPreservesSuccessor(t *testing.T) {
	testH1SuccessfulIoOwnerDelay(t, true)
}

// A completed read must not leave an expired deadline on the next read.
func TestH1SuccessfulReadOwnerDelayPreservesSuccessor(t *testing.T) {
	testH1SuccessfulIoOwnerDelay(t, false)
}

// A real QUIC carrier runs on the virtual network, so a post-success owner
// delay can cross the exact native timeout without scheduler-dependent sleeps.
func testH3SuccessfulIoOwnerDelay(t *testing.T, write bool) {
	t.Helper()
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		const timeout = 2 * time.Second
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		network := &simnet.Simnet{Router: &simnet.PerfectRouter{}}
		links := simnet.NodeBiDiLinkSettings{Latency: time.Millisecond}
		client := network.NewEndpoint(&net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 50000}, links)
		peer := network.NewEndpoint(&net.UDPAddr{IP: net.ParseIP("192.0.2.2"), Port: 443}, links)
		if err := network.Start(); err != nil {
			t.Fatal(err)
		}
		defer network.Close()
		defer client.Close()
		defer peer.Close()
		serverTls := extenderQuicKeepAliveTestTlsConfig(t)
		listener, err := quic.Listen(peer, serverTls, &quic.Config{MaxIdleTimeout: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		sendToClient := make(chan []byte)
		serverErrors := make(chan error, 2)
		framerSettings := DefaultFramerSettings(int(DefaultClientSettings().MinimumMessageLenLimit()))
		go func() {
			conn, err := listener.Accept(ctx)
			if err != nil {
				serverErrors <- err
				return
			}
			defer conn.CloseWithError(0, "")
			stream, err := conn.AcceptStream(ctx)
			if err != nil {
				serverErrors <- err
				return
			}
			framer := NewFramer(framerSettings)
			auth, err := framer.Read(stream)
			if err != nil {
				serverErrors <- err
				return
			}
			err = framer.Write(stream, auth)
			MessagePoolReturn(auth)
			if err != nil {
				serverErrors <- err
				return
			}
			go func() {
				reader := NewFramer(framerSettings)
				for {
					message, err := reader.Read(stream)
					if err != nil {
						return
					}
					MessagePoolReturn(message)
				}
			}()
			for {
				select {
				case <-ctx.Done():
					return
				case message := <-sendToClient:
					if err := framer.Write(stream, message); err != nil {
						serverErrors <- err
						return
					}
				}
			}
		}()
		completed := make(chan struct{})
		successor := make(chan struct{})
		resume := make(chan struct{})
		release := sync.OnceFunc(func() { close(resume) })
		defer release()
		var completions atomic.Int64
		settings := testingPlatformTransportSettings()
		settings.Log = NewNoopLogger()
		settings.ReadTimeout = time.Minute
		settings.WriteTimeout = time.Minute
		settings.PingTimeout = time.Minute
		settings.ReconnectTimeout = time.Hour
		settings.FramerSettings = framerSettings
		if write {
			settings.WriteTimeout = timeout
		} else {
			settings.ReadTimeout = timeout
		}
		settings.QuicTlsConfig = &tls.Config{
			InsecureSkipVerify: true, // synthetic certificate above
			NextProtos:         serverTls.NextProtos,
		}
		settings.H3PacketConnFactory = func(context.Context) (net.PacketConn, error) { return client, nil }
		settings.resolveH3AddrsForTest = func(context.Context, string, int) ([]*net.UDPAddr, error) {
			return []*net.UDPAddr{peer.LocalAddr().(*net.UDPAddr)}, nil
		}
		settings.afterH3StreamIoForTest = func(wrote bool) {
			if wrote != write {
				return
			}
			switch completions.Add(1) {
			case 1:
				close(completed)
				<-resume
			case 2:
				close(successor)
			}
		}
		published := make(chan struct{})
		var publishedOnce sync.Once
		settings.SendRouteObserver = func(_ Transport, _ Route, active bool) {
			if active {
				publishedOnce.Do(func() { close(published) })
			}
		}
		routes := NewRouteManagerWithLogger(ctx, "synthetic-h3-deadline", NewNoopLogger())
		strategy := NewClientStrategyWithDefaults(ctx)
		defer strategy.Close()
		transport := NewPlatformTransportWithTargetMode(ctx, strategy, routes,
			"https://deadline.example", &ClientAuth{ByJwt: "synthetic-token", InstanceId: NewId()},
			TransportModeH3, settings)
		defer func() {
			release()
			cancel()
			if err := transport.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		select {
		case <-published:
		case err := <-serverErrors:
			t.Fatal(err)
		case <-time.After(10 * time.Second):
			t.Fatal("H3 carrier did not publish")
		}
		writer := routes.OpenMultiRouteWriter(DestinationId(NewId()))
		defer routes.CloseMultiRouteWriter(writer)
		send := func() {
			t.Helper()
			if write {
				message := h1PlusOwnedTestMessage(0x42, 64)
				if err := writer.Write(ctx, message, timeout); err != nil {
					MessagePoolReturn(message)
					t.Fatal(err)
				}
			} else {
				select {
				case sendToClient <- bytes.Repeat([]byte{0x42}, 64):
				case err := <-serverErrors:
					t.Fatal(err)
				}
			}
		}
		send()
		<-completed
		time.Sleep(2 * timeout)
		synctest.Wait()
		release()
		synctest.Wait()
		if !transport.IsConnected() || !routes.HasActiveTransport() {
			t.Fatal("successful I/O was converted into a carrier failure during owner delay")
		}
		send()
		select {
		case <-successor:
		case <-time.After(timeout):
			t.Fatal("healthy successor inherited the completed operation's timeout")
		}
		if !transport.IsConnected() || !routes.HasActiveTransport() {
			t.Fatal("the successor withdrew its healthy carrier")
		}
	})
}

// Stream writes that succeeded do not poison the following write.
func TestH3SuccessfulWriteOwnerDelayPreservesSuccessor(t *testing.T) {
	testH3SuccessfulIoOwnerDelay(t, true)
}

// Stream reads retain their success across the same scheduling gap.
func TestH3SuccessfulReadOwnerDelayPreservesSuccessor(t *testing.T) {
	testH3SuccessfulIoOwnerDelay(t, false)
}

// A deadline is an I/O bound, not a permanent connection failure. Both setter
// forms must forward a past bound and allow the next operation to replace it.
func TestWebSocketBatchNativeDeadlineCanBeReplacedBeforeSuccessor(t *testing.T) {
	for _, combined := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			client, peer := net.Pipe()
			defer client.Close()
			defer peer.Close()
			conn := NewWebSocketWriteBatchConn(client)
			set := conn.SetWriteDeadline
			if combined {
				set = conn.SetDeadline
			}
			if err := set(time.Now().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Write([]byte{1}); err == nil {
				t.Fatal("the native expired deadline did not bound the write")
			}
			if err := set(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := conn.Write([]byte{2})
				done <- err
			}()
			synctest.Wait()
			select {
			case err := <-done:
				t.Fatalf("combined=%t: successor failed before its deadline: %v", combined, err)
			default:
			}
			var message [1]byte
			if _, err := io.ReadFull(peer, message[:]); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil || message[0] != 2 {
				t.Fatalf("combined=%t: successor=%v error=%v", combined, message, err)
			}
		})
	}
}
