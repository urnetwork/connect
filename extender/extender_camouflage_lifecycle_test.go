// Deterministic shutdown, cancellation and byte-preservation regressions for
// the camouflage accept path. All connections and borrowed names are synthetic.
package extender

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect"
)

// A connection whose first write fails without implicitly closing it.
type camouflageLifecycleWriteErrorConn struct {
	net.Conn
	closed atomic.Bool
}

// Fails the initial replay without closing the dialed connection implicitly.
func (self *camouflageLifecycleWriteErrorConn) Write(p []byte) (int, error) {
	return 0, io.ErrClosedPipe
}

// Records ownership release before closing the pipe.
func (self *camouflageLifecycleWriteErrorConn) Close() error {
	self.closed.Store(true)
	return self.Conn.Close()
}

// Dialed connections must be closed on the relay's first-write error path.
func TestExtenderCamouflageSpliceClosesSiteOnInitialWriteError(t *testing.T) {
	clientConn, clientPeer := net.Pipe()
	defer clientConn.Close()
	defer clientPeer.Close()
	sitePipe, sitePeer := net.Pipe()
	defer sitePeer.Close()
	siteConn := &camouflageLifecycleWriteErrorConn{Conn: sitePipe}
	defer siteConn.Close()
	settings := DefaultExtenderSettings()
	settings.CamouflageBorrowNames = []string{"borrow.example"}
	settings.CamouflageSpliceDialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
		return siteConn, nil
	}
	server := &ExtenderServer{settings: settings}
	camouflage, err := newExtenderCamouflage(server, settings)
	if err != nil {
		t.Fatal(err)
	}
	if !camouflage.splice(context.Background(), clientConn, []byte("synthetic initial bytes"), "borrow.example") {
		t.Fatal("the injected connection was not selected")
	}
	if !siteConn.closed.Load() {
		t.Fatal("the dialed site connection remained open after its first write failed")
	}
}

// Context cancellation must interrupt the initial write as well as later relay
// reads and writes. net.Pipe supplies a deterministic blocked write.
func TestExtenderCamouflageSpliceCancellationInterruptsInitialWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clientConn, clientPeer := net.Pipe()
		defer clientConn.Close()
		defer clientPeer.Close()
		siteConn, sitePeer := net.Pipe()
		defer siteConn.Close()
		defer sitePeer.Close()
		settings := DefaultExtenderSettings()
		settings.ProxyIdleTimeout = 0
		settings.CamouflageBorrowNames = []string{"borrow.example"}
		settings.CamouflageSpliceDialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
			return siteConn, nil
		}
		server := &ExtenderServer{settings: settings}
		camouflage, err := newExtenderCamouflage(server, settings)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() {
			camouflage.splice(ctx, clientConn, []byte("synthetic initial bytes"), "borrow.example")
			close(done)
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		finished := false
		select {
		case <-done:
			finished = true
		default:
		}
		// Always clean up before failing, so the test itself owns no live work.
		sitePeer.Close()
		<-done
		if !finished {
			t.Fatal("cancellation left the splice blocked in its initial site write")
		}
	})
}

// The newly introduced peek must observe handler cancellation before waiting
// for the entire header deadline. The virtual clock makes the wait exact.
func TestExtenderCamouflageCanceledHandlerDoesNotWaitForHello(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultExtenderSettings()
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = bytes.Repeat([]byte{0x36}, 32)
		server := NewExtenderServer(context.Background(), nil, nil, nil, &net.Dialer{}, settings)
		defer server.CloseAndWait()
		clientConn, peer := net.Pipe()
		defer peer.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()
		server.HandleExtenderConnection(ctx, clientConn)
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("an already canceled handler waited %v for its initial read", elapsed)
		}
	})
}

// Cancellation after the real handler has blocked in its peek must also release
// it without advancing the virtual clock to the header timeout.
func TestExtenderCamouflageHandlerCancellationInterruptsHello(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultExtenderSettings()
		settings.CamouflageEnabled = true
		settings.IdentityKeySeed = bytes.Repeat([]byte{0x37}, 32)
		server := NewExtenderServer(context.Background(), nil, nil, nil, &net.Dialer{}, settings)
		defer server.CloseAndWait()
		clientConn, peer := net.Pipe()
		defer peer.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() {
			server.HandleExtenderConnection(ctx, clientConn)
			close(done)
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		finished := false
		select {
		case <-done:
			finished = true
		default:
		}
		peer.Close()
		<-done
		if !finished {
			t.Fatal("handler cancellation left the hello read blocked")
		}
	})
}

// A read may consume a prefix before returning a deadline error; clearing the
// deadline allows the remaining stream to be read, so replay must keep it all.
type camouflageLifecyclePartialReadConn struct {
	net.Conn
	reader        *bytes.Reader
	readCount     int
	errorRead     int
	partialLength int
}

// Consumes a prefix and fails once, then permits the remaining bytes to replay.
func (self *camouflageLifecyclePartialReadConn) Read(p []byte) (int, error) {
	self.readCount++
	if self.readCount == self.errorRead {
		n, _ := self.reader.Read(p[:self.partialLength])
		return n, os.ErrDeadlineExceeded
	}
	return self.reader.Read(p)
}

// An incomplete record header still belongs to the next protocol reader.
func TestExtenderCamouflagePeekPreservesPartialHeader(t *testing.T) {
	input := []byte{22, 3, 1, 0, 8, 1, 0, 0, 4, 1, 2, 3, 4}
	conn := &camouflageLifecyclePartialReadConn{reader: bytes.NewReader(input), errorRead: 1, partialLength: 3}
	readBytes, _, _ := peekClientHello(conn, 16*1024)
	got, err := io.ReadAll(newConnWithInitialBytes(conn, readBytes, ""))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, input) {
		t.Fatalf("peek/replay lost consumed bytes: got %x, want %x", got, input)
	}
}

// An incomplete record body must follow its header in the replay buffer.
func TestExtenderCamouflagePeekPreservesPartialFragment(t *testing.T) {
	input := []byte{22, 3, 1, 0, 8, 1, 0, 0, 4, 1, 2, 3, 4}
	conn := &camouflageLifecyclePartialReadConn{reader: bytes.NewReader(input), errorRead: 2, partialLength: 2}
	readBytes, _, _ := peekClientHello(conn, 16*1024)
	got, err := io.ReadAll(newConnWithInitialBytes(conn, readBytes, ""))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, input) {
		t.Fatalf("peek/replay lost consumed bytes: got %x, want %x", got, input)
	}
}

// Holds an already parsed hello immediately before its first splice dial.
type camouflageLifecycleRemoteAddrBarrierConn struct {
	net.Conn
	entered chan struct{}
	release chan struct{}
}

// Parks the real accept path after parsing but before lazy resolver admission.
func (self *camouflageLifecycleRemoteAddrBarrierConn) RemoteAddr() net.Addr {
	close(self.entered)
	<-self.release
	return self.Conn.RemoteAddr()
}

// Closing a server while its first splice is entering the lazy resolver must
// leave that in-flight handler able to unwind without dereferencing nil.
func TestExtenderCamouflageShutdownBeforeFirstSpliceDoesNotPanic(t *testing.T) {
	settings := DefaultExtenderSettings()
	settings.CamouflageEnabled = true
	settings.ExtenderCamouflageSplice = true
	settings.CamouflageBorrowNames = []string{"borrow.example"}
	settings.IdentityKeySeed = bytes.Repeat([]byte{0x54}, 32)
	settings.DialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
		return nil, net.ErrClosed
	}
	server := NewExtenderServer(context.Background(), nil, nil, nil, &net.Dialer{}, settings)
	defer server.CloseAndWait()
	clientConn, peer := net.Pipe()
	defer clientConn.Close()
	defer peer.Close()
	barrierConn := &camouflageLifecycleRemoteAddrBarrierConn{
		Conn:    clientConn,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	done := make(chan struct{})
	var panicValue any
	go func() {
		defer close(done)
		defer func() { panicValue = recover() }()
		server.HandleExtenderConnection(server.ctx, barrierConn)
	}()
	if _, err := peer.Write(rawUnauthenticatedHelloRecord(t, "borrow.example")); err != nil {
		t.Fatal(err)
	}
	<-barrierConn.entered
	server.Close()
	close(barrierConn.release)
	<-done
	if panicValue != nil {
		t.Fatalf("shutdown during the first splice panicked: %v", panicValue)
	}
}

// A completed cache remains retired: shutdown joins its active resolver dial,
// and the interrupted query must not fall through to an unowned name dial.
func TestExtenderCamouflageShutdownJoinsResolverAndRefusesFallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dialEntered := make(chan struct{}, 1)
		dialCanceled := make(chan struct{}, 1)
		releaseDial := make(chan struct{})
		settings := DefaultExtenderSettings()
		settings.DohSettings = connect.DefaultDohSettings()
		settings.DohSettings.DnsResolverSettings = &connect.DnsResolverSettings{
			EnableRemoteDns: true,
			RemoteDnsIpv4:   []string{"192.0.2.53"},
		}
		settings.DohSettings.DialContextSettings = &connect.DialContextSettings{
			DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
				select {
				case dialEntered <- struct{}{}:
				default:
				}
				<-ctx.Done()
				select {
				case dialCanceled <- struct{}{}:
				default:
				}
				<-releaseDial
				return nil, ctx.Err()
			},
		}
		var fallbackDialed atomic.Bool
		settings.DialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
			fallbackDialed.Store(true)
			return nil, net.ErrClosed
		}
		server := &ExtenderServer{settings: settings}
		camouflage, err := newExtenderCamouflage(server, settings)
		if err != nil {
			t.Fatal(err)
		}
		defer camouflage.close()
		dialDone := make(chan error, 1)
		go func() {
			_, err := camouflage.spliceDial(context.Background(), "tcp4", "borrow.example")
			dialDone <- err
		}()
		<-dialEntered
		closeDone := make(chan struct{})
		go func() {
			camouflage.close()
			close(closeDone)
		}()
		<-dialCanceled
		synctest.Wait()
		select {
		case <-closeDone:
			t.Error("shutdown returned before its resolver dial was joined")
		default:
		}
		close(releaseDial)
		<-closeDone
		if err := <-dialDone; !errors.Is(err, net.ErrClosed) {
			t.Errorf("interrupted splice dial error = %v, want a retired owner", err)
		}
		if fallbackDialed.Load() {
			t.Fatal("the retired resolver fell through to a new borrowed-site dial")
		}
	})
}

// A writer that accepts only a prefix per call and immediately ends reads.
// The initial hello must be replayed completely before relay workers start.
type camouflageLifecycleShortWriteConn struct {
	net.Conn
	writtenBytes bytes.Buffer
}

// Forces the replay writer to retain and retry the unwritten suffix.
func (self *camouflageLifecycleShortWriteConn) Write(p []byte) (int, error) {
	return self.writtenBytes.Write(p[:min(3, len(p))])
}

// Ends the relay after its initial replay has completed.
func (self *camouflageLifecycleShortWriteConn) Read(p []byte) (int, error) {
	return 0, io.EOF
}

// Short successful writes must not truncate the initial hello sent to the site.
func TestExtenderCamouflageSplicePreservesShortInitialWrites(t *testing.T) {
	clientConn, clientPeer := net.Pipe()
	defer clientConn.Close()
	defer clientPeer.Close()
	sitePipe, sitePeer := net.Pipe()
	defer sitePipe.Close()
	defer sitePeer.Close()
	siteConn := &camouflageLifecycleShortWriteConn{Conn: sitePipe}
	camouflage := &extenderCamouflage{settings: DefaultExtenderSettings()}
	initialBytes := []byte("synthetic initial hello")
	camouflage.relaySplice(context.Background(), clientConn, siteConn, initialBytes)
	if !bytes.Equal(siteConn.writtenBytes.Bytes(), initialBytes) {
		t.Fatalf("initial replay = %q, want %q", siteConn.writtenBytes.Bytes(), initialBytes)
	}
}

// A writer that makes no progress once, then errors to keep the pre-fix loop
// bounded. The second call itself is the regression, without scheduler timing.
type camouflageLifecycleNoProgressConn struct {
	net.Conn
	writeCount int
}

// Detects retrying a zero-progress write that must terminate the relay.
func (self *camouflageLifecycleNoProgressConn) Write(p []byte) (int, error) {
	self.writeCount++
	if self.writeCount == 1 {
		return 0, nil
	}
	return 0, io.ErrNoProgress
}

// A zero-progress write must terminate instead of spinning on the same suffix.
func TestExtenderCamouflageSpliceStopsOnZeroProgressWrite(t *testing.T) {
	clientConn, clientPeer := net.Pipe()
	defer clientConn.Close()
	defer clientPeer.Close()
	sitePipe, sitePeer := net.Pipe()
	defer sitePipe.Close()
	defer sitePeer.Close()
	siteConn := &camouflageLifecycleNoProgressConn{Conn: sitePipe}
	camouflage := &extenderCamouflage{settings: DefaultExtenderSettings()}
	done := make(chan struct{})
	go func() {
		camouflage.relaySplice(context.Background(), clientConn, siteConn, nil)
		close(done)
	}()
	if _, err := clientPeer.Write([]byte("synthetic relay bytes")); err != nil {
		t.Fatal(err)
	}
	<-done
	if siteConn.writeCount != 1 {
		t.Fatalf("zero-progress writes = %d, want exactly one attempt", siteConn.writeCount)
	}
}

// The hello deadline belongs to parsing. With relay idle timeouts disabled,
// the real handler must still relay traffic after that earlier budget expires.
func TestExtenderCamouflageSpliceClearsHelloDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clientConn, clientPeer := net.Pipe()
		siteConn, sitePeer := net.Pipe()
		settings := DefaultExtenderSettings()
		settings.CamouflageEnabled = true
		settings.ExtenderCamouflageSplice = true
		settings.CamouflageBorrowNames = []string{"borrow.example"}
		settings.IdentityKeySeed = bytes.Repeat([]byte{0x55}, 32)
		settings.HeaderTimeout = time.Second
		settings.ProxyIdleTimeout = 0
		settings.CamouflageSpliceDialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
			return siteConn, nil
		}
		server := NewExtenderServer(context.Background(), nil, nil, nil, &net.Dialer{}, settings)
		defer server.CloseAndWait()
		ctx, cancel := context.WithCancel(server.ctx)
		handlerDone := make(chan struct{})
		go func() {
			server.HandleExtenderConnection(ctx, clientConn)
			close(handlerDone)
		}()
		defer func() {
			cancel()
			clientPeer.Close()
			sitePeer.Close()
			<-handlerDone
		}()
		helloBytes := rawUnauthenticatedHelloRecord(t, "borrow.example")
		if _, err := clientPeer.Write(helloBytes); err != nil {
			t.Fatal(err)
		}
		replayedHelloBytes := make([]byte, len(helloBytes))
		if _, err := io.ReadFull(sitePeer, replayedHelloBytes); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(replayedHelloBytes, helloBytes) {
			t.Fatal("splice changed the initial hello bytes")
		}
		synctest.Wait()
		time.Sleep(2 * settings.HeaderTimeout)
		payload := []byte("synthetic bytes after the hello budget")
		receivedBytes := make([]byte, len(payload))
		readDone := make(chan error, 1)
		go func() {
			_, err := io.ReadFull(sitePeer, receivedBytes)
			readDone <- err
		}()
		_, writeErr := clientPeer.Write(payload)
		readErr := <-readDone
		if writeErr != nil || readErr != nil {
			t.Fatalf("hello deadline terminated the relay: write=%v read=%v", writeErr, readErr)
		}
		if !bytes.Equal(receivedBytes, payload) {
			t.Fatalf("relayed bytes = %q, want %q", receivedBytes, payload)
		}
	})
}
