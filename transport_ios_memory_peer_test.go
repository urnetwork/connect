// Current-profile peers have independent lifetimes and explicitly joined
// workers. Client cancellation must succeed while these peers remain alive.
package connect

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/urnetwork/connect/protocol"
)

// Marking expected client shutdown only classifies terminal peer reads; it
// never cancels the peer, closes a socket, or helps the client finish.
func newIosMemoryQuicEcho(t *testing.T, mode TransportMode) (int, <-chan error, func(), func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	certPem, keyPem, err := selfSign([]string{"127.0.0.1"}, "ios-memory-peer", time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	port := raw.LocalAddr().(*net.UDPAddr).Port
	socket := raw
	if mode != TransportModeH3 {
		settings := DefaultPacketTranslationSettings()
		settings.DnsTlds = [][]byte{[]byte("memory.example.")}
		translationMode := PacketTranslationModeDecode53
		if mode == TransportModeH3DnsPump {
			translationMode = PacketTranslationModeDecode53RequireDnsPump
		}
		translation, err := NewPacketTranslation(ctx, translationMode, raw, settings)
		if err != nil {
			t.Fatal(err)
		}
		socket = translation
		t.Cleanup(func() { translation.Close() })
	}
	quicTransport := &quic.Transport{Conn: socket}
	t.Cleanup(func() { quicTransport.Close() })
	listener, err := quicTransport.Listen(&tls.Config{
		Certificates: []tls.Certificate{cert}, NextProtos: []string{"memory-matrix"},
	}, &quic.Config{EnableDatagrams: true, MaxIdleTimeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	peerErrors := newIosMemoryPeerErrors()
	recordError := peerErrors.record
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
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
		framer := NewFramer(DefaultFramerSettings(8192))
		authBytes, err := framer.Read(stream)
		if err != nil {
			recordError(err)
			return
		}
		message, err := DecodeFrame(authBytes)
		MessagePoolReturn(authBytes)
		if err != nil {
			recordError(err)
			return
		}
		auth, ok := message.(*protocol.Auth)
		if !ok {
			recordError(fmt.Errorf("unexpected current-profile auth %T", message))
			return
		}
		state := conn.ConnectionState()
		response, accepted := AcceptH3DatagramAuthOffer(auth, true, state.SupportsDatagrams.Local, state.SupportsDatagrams.Remote)
		if !accepted {
			recordError(fmt.Errorf("current-profile peer did not negotiate hybrid lanes"))
			return
		}
		responseBytes, err := EncodeFrame(response, DefaultProtocolVersion)
		if err != nil {
			recordError(err)
			return
		}
		err = framer.Write(stream, responseBytes)
		MessagePoolReturn(responseBytes)
		if err != nil {
			recordError(err)
			return
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				packet, err := conn.ReceiveDatagram(ctx)
				if err != nil {
					recordError(err)
					return
				}
				if err := conn.SendDatagram(packet); err != nil {
					recordError(err)
					return
				}
			}
		}()
		for {
			message, err := framer.Read(stream)
			if err != nil {
				recordError(err)
				return
			}
			err = framer.Write(stream, message)
			MessagePoolReturn(message)
			if err != nil {
				recordError(err)
				return
			}
		}
	}()
	beginShutdown := peerErrors.beginShutdown
	var closeOnce sync.Once
	closePeer := func() {
		closeOnce.Do(func() {
			beginShutdown()
			cancel()
			listener.Close()
			quicTransport.Close()
			socket.Close()
			workers.Wait()
			select {
			case err := <-peerErrors.errors:
				t.Errorf("current-profile peer failed during graph or teardown: %v", err)
			default:
			}
		})
	}
	t.Cleanup(closePeer)
	return port, peerErrors.errors, beginShutdown, closePeer
}
