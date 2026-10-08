// Current-profile inner peers outlive client cancellation and join their
// serving workers explicitly. Historical memory fixtures remain unchanged.
package extender

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	quic "github.com/quic-go/quic-go"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// Preserve any bytes read past the HTTP upgrade while the framed peer takes
// ownership of the hijacked native connection.
type extenderIosMemoryBufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

// Reads buffered upgrade tail bytes before returning to the native socket.
func (self *extenderIosMemoryBufferedConn) Read(message []byte) (int, error) {
	return self.reader.Read(message)
}

// Only the dial seam resolves the synthetic named destination. Both peers
// retain independent background lifetimes until the client graph has joined.
func newExtenderIosMemoryFixture(t *testing.T, inner connect.TransportMode) (*extenderFixture, int, string, func(), func()) {
	t.Helper()
	var port int
	var packetDestination string
	var beginPeerShutdown, closeInner func()
	outerErrors := newExtenderIosMemoryPeerErrors()
	fixture := newExtenderFixtureWithSetup(t, "127.0.0.1", []string{testSecret}, []string{"127.0.0.1", "alt.invalid"}, func(fixture *extenderFixture, settings *ExtenderSettings) {
		var destination string
		destination, beginPeerShutdown, closeInner = newExtenderIosMemoryPeer(t, fixture.destination.certificate, inner)
		outerErrors.errors = fixture.errors
		recordOuterError := settings.ErrorHandler
		settings.ErrorHandler = func(stage string, err error) {
			if outerErrors.shouldRecord(err) {
				recordOuterError(stage, err)
			}
		}
		_, portString, err := net.SplitHostPort(destination)
		if err != nil {
			t.Fatal(err)
		}
		port, err = strconv.Atoi(portString)
		if err != nil {
			t.Fatal(err)
		}
		packetDestination = net.JoinHostPort("alt.invalid", portString)
		settings.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, destination)
		}
		settings.DialPacketContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != packetDestination {
				return nil, fmt.Errorf("current-profile extender destination = %q, want unresolved %q", address, packetDestination)
			}
			return (&net.Dialer{}).DialContext(ctx, network, destination)
		}
	})
	beginShutdown := func() {
		outerErrors.beginShutdown()
		beginPeerShutdown()
	}
	var closeOnce sync.Once
	closePeers := func() {
		closeOnce.Do(func() {
			beginShutdown()
			fixture.server.CloseAndWait()
			closeInner()
			for {
				err, ok := outerErrors.takeAfterJoin()
				if !ok {
					break
				}
				t.Errorf("current-profile outer peer failed during graph or teardown: %v", err)
			}
		})
	}
	t.Cleanup(closePeers)
	return fixture, port, packetDestination, beginShutdown, closePeers
}

// This narrow echo peer retains unexpected failures through client termination.
// Marking termination never cancels, closes, or otherwise helps the client.
func newExtenderIosMemoryPeer(t *testing.T, certificate *tls.Certificate, mode connect.TransportMode) (string, func(), func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	peerErrors := newExtenderIosMemoryPeerErrors()
	recordError := peerErrors.record
	beginShutdown := peerErrors.beginShutdown
	reportError := func() {
		select {
		case err := <-peerErrors.errors:
			t.Errorf("current-profile inner %s peer failed during graph or teardown: %v", mode, err)
		default:
		}
	}
	if mode == connect.TransportModeH1 {
		var stateLock sync.Mutex
		peerClosed := false
		connections := map[connect.H1MessageConn]bool{}
		var workers sync.WaitGroup
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			admitted := func() bool {
				stateLock.Lock()
				defer stateLock.Unlock()
				if peerClosed {
					return false
				}
				workers.Add(1)
				return true
			}()
			if !admitted {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			defer workers.Done()
			var ws connect.H1MessageConn
			pooled := false
			register := func() bool {
				stateLock.Lock()
				defer stateLock.Unlock()
				if peerClosed {
					return false
				}
				connections[ws] = true
				return true
			}
			defer func() {
				if ws != nil {
					ws.Close()
					stateLock.Lock()
					defer stateLock.Unlock()
					delete(connections, ws)
				}
			}()
			if request.Header.Get("Upgrade") == connect.H1FramerProtocol {
				// The current default may choose H1+. Support it directly; the
				// untouched historical fixture still covers websocket fallback.
				raw, buffered, err := w.(http.Hijacker).Hijack()
				if err != nil {
					recordError(err)
					return
				}
				framed, err := connect.NewFramedMessageConn(&extenderIosMemoryBufferedConn{Conn: raw, reader: buffered.Reader}, connect.H1FramerProtocol, 65535, nil)
				if err != nil {
					raw.Close()
					recordError(err)
					return
				}
				ws, pooled = framed, true
				if !register() {
					return
				}
				if _, err := fmt.Fprintf(buffered, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", connect.H1FramerProtocol); err != nil {
					recordError(err)
					return
				}
				if err := buffered.Flush(); err != nil {
					recordError(err)
					return
				}
			} else {
				webSocket, err := (&websocket.Upgrader{HandshakeTimeout: 5 * time.Second, CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, request, nil)
				if err != nil {
					recordError(err)
					return
				}
				ws = webSocket
				if !register() {
					return
				}
			}
			for {
				kind, payload, err := ws.ReadMessage()
				if err != nil {
					recordError(err)
					return
				}
				err = ws.WriteMessage(kind, payload)
				if pooled {
					connect.MessagePoolReturn(payload)
				}
				if err != nil {
					recordError(err)
					return
				}
			}
		}))
		server.TLS = &tls.Config{Certificates: []tls.Certificate{*certificate}}
		server.StartTLS()
		var closeOnce sync.Once
		closePeer := func() {
			closeOnce.Do(func() {
				beginShutdown()
				cancel()
				active := func() []connect.H1MessageConn {
					stateLock.Lock()
					defer stateLock.Unlock()
					peerClosed = true
					active := make([]connect.H1MessageConn, 0, len(connections))
					for ws := range connections {
						active = append(active, ws)
					}
					return active
				}()
				for _, ws := range active {
					ws.Close()
				}
				server.Close()
				workers.Wait()
				reportError()
			})
		}
		t.Cleanup(closePeer)
		return server.Listener.Addr().String(), beginShutdown, closePeer
	}
	raw, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	address := raw.LocalAddr().String()
	socket := raw
	if mode != connect.TransportModeH3 {
		settings := connect.DefaultPacketTranslationSettings()
		settings.DnsTlds = [][]byte{[]byte(testDnsTld)}
		translationMode := connect.PacketTranslationModeDecode53
		if mode == connect.TransportModeH3DnsPump {
			translationMode = connect.PacketTranslationModeDecode53RequireDnsPump
		}
		translation, err := connect.NewPacketTranslation(ctx, translationMode, raw, settings)
		if err != nil {
			t.Fatal(err)
		}
		socket = translation
		t.Cleanup(func() { translation.Close() })
	}
	quicTransport := &quic.Transport{Conn: socket}
	t.Cleanup(func() { quicTransport.Close() })
	listener, err := quicTransport.Listen(&tls.Config{
		Certificates: []tls.Certificate{*certificate}, NextProtos: []string{"extender-memory"},
	}, &quic.Config{MaxIdleTimeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept(ctx)
		if err != nil {
			recordError(err)
			return
		}
		defer conn.CloseWithError(0, "fixture complete")
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			recordError(err)
			return
		}
		framer := connect.NewFramer(connect.DefaultFramerSettings(8192))
		authBytes, err := framer.Read(stream)
		if err != nil {
			recordError(err)
			return
		}
		message, err := connect.DecodeFrame(authBytes)
		connect.MessagePoolReturn(authBytes)
		if err != nil {
			recordError(err)
			return
		}
		auth, ok := message.(*protocol.Auth)
		if !ok {
			recordError(fmt.Errorf("unexpected current-profile inner auth %T", message))
			return
		}
		response, _ := connect.AcceptH3DatagramAuthOffer(auth, false, false, false)
		responseBytes, err := connect.EncodeFrame(response, connect.DefaultProtocolVersion)
		if err != nil {
			recordError(err)
			return
		}
		err = framer.Write(stream, responseBytes)
		connect.MessagePoolReturn(responseBytes)
		if err != nil {
			recordError(err)
			return
		}
		for {
			message, err := framer.Read(stream)
			if err != nil {
				recordError(err)
				return
			}
			err = framer.Write(stream, message)
			connect.MessagePoolReturn(message)
			if err != nil {
				recordError(err)
				return
			}
		}
	}()
	var closeOnce sync.Once
	closePeer := func() {
		closeOnce.Do(func() {
			beginShutdown()
			cancel()
			listener.Close()
			quicTransport.Close()
			socket.Close()
			<-done
			reportError()
		})
	}
	t.Cleanup(closePeer)
	return address, beginShutdown, closePeer
}
