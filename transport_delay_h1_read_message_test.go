package connect

// Real negotiated WebSocket readers must strip timing bytes while preserving
// the message, its next-frame tail, and the caller's configured payload cap.
import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Borrows payloads until the server's joined writer exits. Existing carrier
// fixtures provide the actual upgrade, subprotocol, and peer-side wrapper.
func readLimitBoundaryWebSocket(t *testing.T, payloads [][]byte, ping bool) (H1MessageConn, <-chan error) {
	t.Helper()
	resetH1UpgradeTestState(t)
	serverDone := make(chan struct{})
	serverResult := make(chan error, 1)
	negotiated := make(chan bool, 1)
	server := newTestTransportDelayH1Server(t, testTransportDelayH1ServerNew, func(serverConn *testTransportDelayH1ServerConn) {
		defer close(serverDone)
		negotiated <- serverConn.offered && serverConn.delayed
		serverResult <- func() error {
			deadline := time.Now().Add(5 * time.Second)
			if err := serverConn.conn.SetWriteDeadline(deadline); err != nil {
				return err
			}
			if ping {
				webSocket, ok := H1WebSocketConn(serverConn.conn)
				if !ok {
					return errors.New("negotiated peer has no WebSocket control writer")
				}
				if err := webSocket.WriteControl(websocket.PingMessage, []byte("synthetic-ping"), deadline); err != nil {
					return err
				}
			}
			for _, payload := range payloads {
				if err := serverConn.conn.WriteMessage(websocket.BinaryMessage, payload); err != nil {
					return err
				}
			}
			return nil
		}()
	})
	raw := testTransportDelayH1Dial(t, server, testTransportDelayH1CarrierWebSocket, true)
	t.Cleanup(func() {
		raw.Close()
		<-serverDone
	})
	if !<-negotiated {
		t.Fatal("the real WebSocket peer did not accept the delay subprotocol")
	}
	delayed := enableTransportDelayH1(raw, newTransportDelay(nil, NewId(), TransportTypeH1, nil))
	if delayed == nil {
		t.Fatal("the real WebSocket client did not accept the delay subprotocol")
	}
	if _, ok := delayed.conn.(*transportDelayWebSocketConn); !ok {
		t.Fatalf("negotiated client is %T, expected the WebSocket delay reader", delayed.conn)
	}
	if err := delayed.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return delayed.conn, serverResult
}

// The old MaxInt64+1 path returns empty,nil for the first nonempty real frame.
// Header-only frames and control lengths surround ordinary and large payloads;
// earlier unpooled results must remain unchanged through all later reads.
func TestTransportDelayWebSocketReadMessagePreservesPayload(t *testing.T) {
	payloads := [][]byte{}
	for i, size := range []int{0, 5, 16, 17, 1400, 9000, 0, 37} {
		payloads = append(payloads, testTransportDelayH1Payload(size, byte(11+i)))
	}
	conn, serverResult := readLimitBoundaryWebSocket(t, payloads, true)
	conn.SetReadLimit(16 * 1024)
	webSocket, ok := H1WebSocketConn(conn)
	if !ok {
		t.Fatal("the real client WebSocket is unavailable")
	}
	pingCount := 0
	webSocket.SetPingHandler(func(string) error {
		pingCount += 1
		return nil
	})
	results := make([][]byte, 0, len(payloads))
	for i, expected := range payloads {
		kind, message, err := conn.ReadMessage()
		if err != nil || kind != websocket.BinaryMessage || !bytes.Equal(message, expected) {
			t.Fatalf("TRANSPORT_DELAY_READ_MESSAGE_DROPPED_PAYLOAD: index=%d kind=%d got=%d want=%d err=%v", i, kind, len(message), len(expected), err)
		}
		results = append(results, message)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
	if pingCount != 1 {
		t.Fatalf("control callback count=%d, want 1", pingCount)
	}
	for i, message := range results {
		if !bytes.Equal(message, payloads[i]) {
			t.Fatalf("an earlier caller-owned result changed after its tail was read: index=%d", i)
		}
	}
}

// The public server peer wrapper uses the same convenience reader in the
// opposite direction and must return each exact payload without its header.
func TestTransportDelayWebSocketPeerReadMessagePreservesPayload(t *testing.T) {
	resetH1UpgradeTestState(t)
	payloads := [][]byte{nil, testTransportDelayH1Payload(17, 0x43), testTransportDelayH1Payload(1400, 0x59), nil, testTransportDelayH1Payload(31, 0x07)}
	serverDone := make(chan struct{})
	serverResult := make(chan error, 1)
	server := newTestTransportDelayH1Server(t, testTransportDelayH1ServerNew, func(serverConn *testTransportDelayH1ServerConn) {
		defer close(serverDone)
		serverResult <- func() error {
			if !serverConn.offered || !serverConn.delayed {
				return errors.New("the public peer wrapper did not negotiate the delay header")
			}
			if err := serverConn.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return err
			}
			var firstErr error
			for i, expected := range payloads {
				kind, message, err := serverConn.conn.ReadMessage()
				if err != nil {
					return err
				}
				if firstErr == nil && (kind != websocket.BinaryMessage || !bytes.Equal(message, expected)) {
					firstErr = fmt.Errorf("TRANSPORT_DELAY_PEER_READ_MESSAGE_DROPPED_PAYLOAD: index=%d kind=%d got=%d want=%d", i, kind, len(message), len(expected))
				}
			}
			return firstErr
		}()
	})
	raw := testTransportDelayH1Dial(t, server, testTransportDelayH1CarrierWebSocket, true)
	t.Cleanup(func() {
		raw.Close()
		<-serverDone
	})
	delayed := enableTransportDelayH1(raw, newTransportDelay(nil, NewId(), TransportTypeH1, nil))
	if delayed == nil {
		t.Fatal("the real WebSocket client did not negotiate the delay header")
	}
	if err := delayed.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, payload := range payloads {
		if err := delayed.conn.WriteMessage(websocket.BinaryMessage, payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
}

// A finite pooled cap still refuses after one lookahead byte, and the next
// frame remains readable after the WebSocket discards that refused frame's tail.
func TestTransportDelayWebSocketPooledLimitPreservesNextFrame(t *testing.T) {
	const limit = 64
	payloads := [][]byte{testTransportDelayH1Payload(limit, 0x09), testTransportDelayH1Payload(3*limit, 0x35), testTransportDelayH1Payload(17, 0x11)}
	conn, serverResult := readLimitBoundaryWebSocket(t, payloads, false)
	conn.SetReadLimit(4 * limit)
	kind, first, err := ReadH1PooledMessage(conn, limit)
	if first != nil {
		defer MessagePoolReturn(first)
	}
	if err != nil || kind != websocket.BinaryMessage || !bytes.Equal(first, payloads[0]) {
		t.Fatalf("exact finite-cap frame changed: kind=%d got=%d err=%v", kind, len(first), err)
	}
	_, refused, err := ReadH1PooledMessage(conn, limit)
	if refused != nil {
		MessagePoolReturn(refused)
		t.Fatal("a refused frame returned an owned pooled buffer")
	}
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("oversized frame error=%v, want %v", err, ErrMessageTooLarge)
	}
	kind, tail, err := ReadH1PooledMessage(conn, limit)
	if tail != nil {
		defer MessagePoolReturn(tail)
	}
	if err != nil || kind != websocket.BinaryMessage || !bytes.Equal(tail, payloads[2]) || !bytes.Equal(first, payloads[0]) {
		t.Fatalf("the refused frame changed its healthy neighbors: kind=%d tail=%d err=%v", kind, len(tail), err)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
}

// ReadMessage's arithmetic repair must retain the underlying Gorilla payload
// limit, including the delay header's exact contribution to the wire size.
func TestTransportDelayWebSocketReadMessageHonorsPayloadLimit(t *testing.T) {
	const limit = 64
	payloads := [][]byte{testTransportDelayH1Payload(limit, 0x21), testTransportDelayH1Payload(limit+1, 0x47)}
	conn, serverResult := readLimitBoundaryWebSocket(t, payloads, false)
	conn.SetReadLimit(limit)
	kind, message, err := conn.ReadMessage()
	if err != nil || kind != websocket.BinaryMessage || !bytes.Equal(message, payloads[0]) {
		t.Fatalf("exact underlying-limit frame changed: kind=%d got=%d err=%v", kind, len(message), err)
	}
	_, message, err = conn.ReadMessage()
	if !errors.Is(err, websocket.ErrReadLimit) || message != nil {
		t.Fatalf("underlying payload cap was lost: message=%d err=%v", len(message), err)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
}
