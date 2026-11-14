package connect

// The actual delay wrapper must pass a representable wire cap to Gorilla for
// every positive payload cap, while preserving disabled-limit semantics.
import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Observes the exact value the production wrapper gives an actual WebSocket;
// all connection and limit behavior still delegates to that real connection.
type transportDelayObservedReadLimitConn struct {
	H1MessageConn
	readLimit int64
}

// Delegates the actual upgrade selection so normal negotiation stays in use.
func (self *transportDelayObservedReadLimitConn) Subprotocol() string {
	return h1MessageConnSubprotocol(self.H1MessageConn)
}

// Records the production argument before forwarding it to Gorilla unchanged.
func (self *transportDelayObservedReadLimitConn) SetReadLimit(limit int64) {
	self.readLimit = limit
	self.H1MessageConn.SetReadLimit(limit)
}

// No huge message is allocated: the boundary oracle observes the exact cap
// passed to the real reader, then that same reader consumes an actual payload.
func TestTransportDelayWebSocketReadLimitSaturatesWireCap(t *testing.T) {
	resetH1UpgradeTestState(t)
	expected := testTransportDelayH1Payload(37, 0x19)
	serverDone := make(chan struct{})
	serverResult := make(chan error, 1)
	server := newTestTransportDelayH1Server(t, testTransportDelayH1ServerNew, func(serverConn *testTransportDelayH1ServerConn) {
		defer close(serverDone)
		serverResult <- func() error {
			if !serverConn.offered || !serverConn.delayed {
				return errors.New("the real peer did not negotiate the delay header")
			}
			if err := serverConn.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return err
			}
			return serverConn.conn.WriteMessage(websocket.BinaryMessage, expected)
		}()
	})
	raw := testTransportDelayH1Dial(t, server, testTransportDelayH1CarrierWebSocket, true)
	t.Cleanup(func() {
		raw.Close()
		<-serverDone
	})
	observed := &transportDelayObservedReadLimitConn{H1MessageConn: raw}
	delayed := enableTransportDelayH1(observed, newTransportDelay(nil, NewId(), TransportTypeH1, nil))
	if delayed == nil {
		t.Fatal("the actual observed WebSocket did not negotiate the delay header")
	}
	for _, c := range []struct {
		payloadLimit int64
		wireLimit    int64
	}{
		{payloadLimit: 0, wireLimit: 0},
		{payloadLimit: -7, wireLimit: -7},
		{payloadLimit: 1, wireLimit: 1 + transportDelayHeaderByteCount},
		{payloadLimit: math64MaxInt - transportDelayHeaderByteCount, wireLimit: math64MaxInt},
		{payloadLimit: math64MaxInt - transportDelayHeaderByteCount + 1, wireLimit: math64MaxInt},
		{payloadLimit: math64MaxInt - 1, wireLimit: math64MaxInt},
		{payloadLimit: math64MaxInt, wireLimit: math64MaxInt},
	} {
		delayed.conn.SetReadLimit(c.payloadLimit)
		if observed.readLimit != c.wireLimit {
			t.Fatalf("TRANSPORT_DELAY_READ_LIMIT_WRAPPED: payload_limit=%d actual_wire_limit=%d want=%d", c.payloadLimit, observed.readLimit, c.wireLimit)
		}
	}
	if err := delayed.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// The finite pooled entry isolates this cap test from ReadMessage's
	// independently qualified largest-limit lookahead repair.
	kind, message, err := ReadH1PooledMessage(delayed.conn, int64(len(expected)))
	if message != nil {
		defer MessagePoolReturn(message)
	}
	if err != nil || kind != websocket.BinaryMessage || !bytes.Equal(message, expected) {
		t.Fatalf("the saturated actual WebSocket lost payload: kind=%d got=%d err=%v", kind, len(message), err)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
}
