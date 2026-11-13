package connect

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// The delay header on both h1 carriers over real sockets (DELAY.md §5, §6). A
// new client and a new server negotiate it on the 101, stamp every message in
// both directions, including WebSocket ready batches, h1+ write chunks and
// zero-length pings, and strip it before a payload reaches its reader. Either
// side alone keeps the legacy wire byte for byte.

const (
	testTransportDelayH1CarrierWebSocket = "websocket"
	testTransportDelayH1CarrierH1Plus    = "h1plus"

	testTransportDelayH1MaxMessageByteCount = 65535
)

var testTransportDelayH1Carriers = []string{
	testTransportDelayH1CarrierWebSocket,
	testTransportDelayH1CarrierH1Plus,
}

// One side writes these payload sizes as single physical writes, then the
// batch sizes as one physical write. 0 is a ping, and 5 and 16 are the
// control lengths both h1 readers classify after stripping the header.
var testTransportDelayH1SingleSizes = []int{0, 5, 16, 17, 1400, 9000}
var testTransportDelayH1BatchSizes = []int{0, 40, 1400, 3000}

// testTransportDelayH1Settings measures from the first write and reports a
// standing queue from a single sample, so a short exchange yields full stats.
func testTransportDelayH1Settings(interval time.Duration) *TransportDelaySettings {
	settings := DefaultTransportDelaySettings()
	settings.Interval = interval
	settings.Warmup = 0
	settings.MinDelaySampleCount = 1
	return settings
}

type testTransportDelayH1ServerMode int

const (
	// knows nothing of the delay header
	testTransportDelayH1ServerOld testTransportDelayH1ServerMode = iota
	// accepts an offer on the 101 and reads and writes through the peer conn
	testTransportDelayH1ServerNew
	// accepts an offer on the 101 but keeps the raw conn, to read the wire
	testTransportDelayH1ServerRaw
)

// testTransportDelayH1ServerConn is one upgraded connection of the test server.
type testTransportDelayH1ServerConn struct {
	conn    H1MessageConn
	offered bool
	delayed bool
}

// testTransportDelayH1BatchResponseWriter installs the WebSocket write batch
// boundary at hijack, as the connect server does.
type testTransportDelayH1BatchResponseWriter struct {
	http.ResponseWriter
}

func (self *testTransportDelayH1BatchResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, readWriter, err := self.ResponseWriter.(http.Hijacker).Hijack()
	if err != nil {
		return nil, nil, err
	}
	return NewWebSocketWriteBatchConn(conn), readWriter, nil
}

// newTestTransportDelayH1Server upgrades each request on the carrier the
// client asks for. serve owns the connection until it returns.
func newTestTransportDelayH1Server(
	t *testing.T,
	mode testTransportDelayH1ServerMode,
	serve func(serverConn *testTransportDelayH1ServerConn),
) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offered := TransportDelayH1Offered(r)
		var acceptHeader http.Header
		if mode != testTransportDelayH1ServerOld && offered {
			acceptHeader = TransportDelayH1AcceptHeader()
		}
		var conn H1MessageConn
		if r.Header.Get("Upgrade") == H1FramerProtocol {
			var raw net.Conn
			var err error
			if mode == testTransportDelayH1ServerOld {
				raw, err = AcceptFramedUpgrade(w, r, H1FramerProtocol, time.Second)
			} else {
				raw, err = AcceptFramedUpgradeWithHeader(w, r, H1FramerProtocol, time.Second, acceptHeader)
			}
			if err != nil {
				return
			}
			framed, err := NewFramedMessageConn(raw, H1FramerProtocol, testTransportDelayH1MaxMessageByteCount, nil)
			if err != nil {
				raw.Close()
				return
			}
			conn = framed
		} else {
			upgrader := websocket.Upgrader{}
			webSocket, err := upgrader.Upgrade(&testTransportDelayH1BatchResponseWriter{ResponseWriter: w}, r, acceptHeader)
			if err != nil {
				return
			}
			conn = webSocket
		}
		defer conn.Close()
		serverConn := &testTransportDelayH1ServerConn{conn: conn, offered: offered}
		if mode == testTransportDelayH1ServerNew {
			if delayed, ok := NewTransportDelayH1PeerConn(conn, NewId()); ok {
				serverConn.conn = delayed
				serverConn.delayed = true
			}
		}
		serve(serverConn)
	}))
	t.Cleanup(server.Close)
	return server
}

// testTransportDelayH1Dial dials the carrier through a write batch boundary,
// as the strategy's dialers do, offering the delay header when offer is set.
func testTransportDelayH1Dial(t *testing.T, server *httptest.Server, carrier string, offer bool) H1MessageConn {
	t.Helper()
	header := http.Header{}
	if offer {
		applyTransportDelayH1Offer(header)
	}
	dialer := &websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
		NetDialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return NewWebSocketWriteBatchConn(conn), nil
		},
	}
	address := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var conn H1MessageConn
	switch carrier {
	case testTransportDelayH1CarrierH1Plus:
		raw, err := DialFramedUpgrade(ctx, address, header, dialer, H1FramerProtocol)
		if err != nil {
			t.Fatal(err)
		}
		framed, err := NewFramedMessageConn(raw, H1FramerProtocol, testTransportDelayH1MaxMessageByteCount, nil)
		if err != nil {
			raw.Close()
			t.Fatal(err)
		}
		conn = framed
	default:
		webSocket, err := DialH1Messages(ctx, address, header, dialer, H1FramerProtocol, testTransportDelayH1MaxMessageByteCount, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := webSocket.(*websocket.Conn); !ok {
			webSocket.Close()
			t.Fatalf("dial selected %T, expected a WebSocket", webSocket)
		}
		conn = webSocket
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func testTransportDelayH1Payload(size int, seed byte) []byte {
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = seed ^ byte(i*31) ^ byte(i>>8)
	}
	return payload
}

// testTransportDelayH1Payloads is what one side writes: singles, then a batch.
func testTransportDelayH1Payloads(seed byte) (singles [][]byte, batch [][]byte) {
	for i, size := range testTransportDelayH1SingleSizes {
		singles = append(singles, testTransportDelayH1Payload(size, seed+byte(i)))
	}
	for i, size := range testTransportDelayH1BatchSizes {
		batch = append(batch, testTransportDelayH1Payload(size, seed+byte(64+i)))
	}
	return
}

// testTransportDelayH1Write writes the singles one physical write each, then
// the batch as one physical write: one ready batch through the WebSocket write
// batch boundary, or one h1+ WriteMessages chunk.
func testTransportDelayH1Write(conn H1MessageConn, seed byte) error {
	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	singles, batch := testTransportDelayH1Payloads(seed)
	for _, message := range singles {
		if err := conn.WriteMessage(websocket.BinaryMessage, message); err != nil {
			return err
		}
	}
	if framed, ok := unobservedH1MessageConn(conn).(*FramedMessageConn); ok {
		return framed.WriteMessages(batch)
	}
	batchConn, ok := conn.UnderlyingConn().(*WebSocketWriteBatchConn)
	if !ok {
		return errors.New("the WebSocket has no write batch boundary")
	}
	batchConn.BeginWriteBatch()
	for _, message := range batch {
		if err := conn.WriteMessage(websocket.BinaryMessage, message); err != nil {
			batchConn.AbortWriteBatch()
			return err
		}
	}
	return batchConn.FlushWriteBatch()
}

// testTransportDelayH1Read reads what testTransportDelayH1Write wrote with
// seed and compares every payload. A reader that does not strip the header
// passes its length as prefixByteCount and gets each message's prefix back.
func testTransportDelayH1Read(conn H1MessageConn, seed byte, prefixByteCount int) ([][]byte, error) {
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return nil, err
	}
	singles, batch := testTransportDelayH1Payloads(seed)
	expectedMessages := slices.Concat(singles, batch)
	prefixes := make([][]byte, 0, len(expectedMessages))
	for index, expected := range expectedMessages {
		kind, message, err := ReadH1PooledMessage(conn, testTransportDelayH1MaxMessageByteCount)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", index, err)
		}
		var mismatch error
		if kind != websocket.BinaryMessage || len(message) != prefixByteCount+len(expected) {
			mismatch = fmt.Errorf("message %d: kind %d with %d bytes, expected %d", index, kind, len(message), prefixByteCount+len(expected))
		} else if !bytes.Equal(message[prefixByteCount:], expected) {
			mismatch = fmt.Errorf("message %d: payload differs", index)
		} else {
			prefixes = append(prefixes, slices.Clone(message[:prefixByteCount]))
		}
		MessagePoolReturn(message)
		if mismatch != nil {
			return nil, mismatch
		}
	}
	return prefixes, nil
}

// testTransportDelayH1AwaitClose holds a connection open until the peer
// closes it, so no unread close resets data the peer has not read yet.
func testTransportDelayH1AwaitClose(conn H1MessageConn) {
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

// testTransportDelayH1WireByteCount is the wire size the delay accounting
// gives one stamped message of size payload bytes.
func testTransportDelayH1WireByteCount(carrier string, size int) ByteCount {
	if carrier == testTransportDelayH1CarrierH1Plus {
		// the frame's length prefix and split, then the header
		return ByteCount(4 + transportDelayHeaderByteCount + size)
	}
	return ByteCount(webSocketMessageWireByteCount(transportDelayHeaderByteCount + size))
}

func testTransportDelayH1ExchangeByteCount(carrier string) ByteCount {
	byteCount := ByteCount(0)
	for _, size := range slices.Concat(testTransportDelayH1SingleSizes, testTransportDelayH1BatchSizes) {
		byteCount += testTransportDelayH1WireByteCount(carrier, size)
	}
	return byteCount
}

// testTransportDelayH1Published collects published stats from any goroutine.
type testTransportDelayH1Published struct {
	stateLock sync.Mutex
	stats     []TransportDelayStats
}

func (self *testTransportDelayH1Published) add(stats *TransportDelayStats) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.stats = append(self.stats, *stats)
}

func (self *testTransportDelayH1Published) snapshot() []TransportDelayStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.stats)
}

func testTransportDelayH1ServerResult(t *testing.T, results <-chan error) {
	t.Helper()
	select {
	case err := <-results:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not finish")
	}
}

// A new client and a new server: every payload arrives intact in both
// directions, both sides echo after receiving, and the client publishes the
// connection's interval with exact byte counts and both queue samples.
func TestTransportDelayH1EndToEnd(t *testing.T) {
	for _, carrier := range testTransportDelayH1Carriers {
		t.Run(carrier, func(t *testing.T) {
			resetH1UpgradeTestState(t)
			serverResults := make(chan error, 4)
			server := newTestTransportDelayH1Server(t, testTransportDelayH1ServerNew, func(serverConn *testTransportDelayH1ServerConn) {
				serverResults <- func() error {
					if !serverConn.offered || !serverConn.delayed {
						return fmt.Errorf("server offered=%t delayed=%t, expected both", serverConn.offered, serverConn.delayed)
					}
					readLane := h1TransportDelayReadLane(serverConn.conn)
					if readLane == nil {
						return errors.New("the server conn has no delay read lane")
					}
					if _, err := testTransportDelayH1Read(serverConn.conn, 1, 0); err != nil {
						return fmt.Errorf("server read: %w", err)
					}
					// the server echoes the one-way delay of the client's writes
					if echo := readLane.delay.echo.Load(); echo == 0 {
						return errors.New("the server echo is 0 after receiving")
					}
					if err := testTransportDelayH1Write(serverConn.conn, 2); err != nil {
						return fmt.Errorf("server write: %w", err)
					}
					testTransportDelayH1AwaitClose(serverConn.conn)
					return nil
				}()
			})

			conn := testTransportDelayH1Dial(t, server, carrier, true)
			if subprotocol := h1MessageConnSubprotocol(conn); subprotocol != TransportDelayH1Subprotocol {
				t.Fatalf("the 101 selected %q, expected %q", subprotocol, TransportDelayH1Subprotocol)
			}
			published := &testTransportDelayH1Published{}
			delay := newTransportDelay(testTransportDelayH1Settings(time.Hour), NewId(), TransportTypeH1, published.add)
			delayH1 := enableTransportDelayH1(conn, delay)
			if delayH1 == nil {
				t.Fatal("the client did not switch to the delay header")
			}
			conn = delayH1.conn

			if echo := delay.echo.Load(); echo != 0 {
				t.Fatalf("the client echo is %d before receiving, expected 0", echo)
			}
			if err := testTransportDelayH1Write(conn, 1); err != nil {
				t.Fatalf("client write: %s", err)
			}
			if _, err := testTransportDelayH1Read(conn, 2, 0); err != nil {
				t.Fatalf("client read: %s", err)
			}
			if echo := delay.echo.Load(); echo == 0 {
				t.Fatal("the client echo is 0 after receiving")
			}
			// the batch was one physical write with one stamp, so the server's
			// writes are its singles plus one group
			func() {
				delay.stateLock.Lock()
				defer delay.stateLock.Unlock()
				if count := delay.interval.downQueue.count; count != len(testTransportDelayH1SingleSizes)+1 {
					t.Fatalf("received %d write groups, expected %d", count, len(testTransportDelayH1SingleSizes)+1)
				}
			}()
			conn.Close()
			testTransportDelayH1ServerResult(t, serverResults)

			delay.flush()
			stats := published.snapshot()
			if len(stats) != 1 {
				t.Fatalf("published %d intervals, expected 1", len(stats))
			}
			interval := stats[0]
			if interval.TransportType != TransportTypeH1 || interval.ConnectionId != delay.connectionId || interval.ClientId != delay.clientId {
				t.Fatalf("interval identity = %v %s %s", interval.TransportType, interval.ConnectionId, interval.ClientId)
			}
			expectedByteCount := testTransportDelayH1ExchangeByteCount(carrier)
			if interval.ReceiveByteCount != expectedByteCount || interval.SendByteCount != expectedByteCount {
				t.Fatalf("receive %d send %d bytes, expected %d each", interval.ReceiveByteCount, interval.SendByteCount, expectedByteCount)
			}
			// down from the server's stamps, up from its echo of the client's
			if !interval.ReceiveQueueDelayValid || !interval.SendQueueDelayValid {
				t.Fatalf("queue delay valid receive=%t send=%t, expected both", interval.ReceiveQueueDelayValid, interval.SendQueueDelayValid)
			}
		})
	}
}

// The wire of a negotiated connection (DELAY.md §5): every message, pings
// included, starts with the 12-byte header. The frames of one physical write
// share the header, a new write never repeats a start stamp, the first write
// reports no previous write, and nothing is echoed before anything arrived.
func TestTransportDelayH1WireStampsEachPhysicalWrite(t *testing.T) {
	for _, carrier := range testTransportDelayH1Carriers {
		t.Run(carrier, func(t *testing.T) {
			resetH1UpgradeTestState(t)
			serverResults := make(chan error, 4)
			server := newTestTransportDelayH1Server(t, testTransportDelayH1ServerRaw, func(serverConn *testTransportDelayH1ServerConn) {
				serverResults <- func() error {
					headers, err := testTransportDelayH1Read(serverConn.conn, 1, transportDelayHeaderByteCount)
					if err != nil {
						return fmt.Errorf("server read: %w", err)
					}
					singleCount := len(testTransportDelayH1SingleSizes)
					// each single is its own write group; the batch is one more
					groups := slices.Clone(headers[:singleCount])
					for _, header := range headers[singleCount:] {
						if !bytes.Equal(header, headers[singleCount]) {
							return fmt.Errorf("the batch frames carry different headers: %x and %x", headers[singleCount], header)
						}
					}
					groups = append(groups, headers[singleCount])
					previousSendStart := uint32(0)
					for index, header := range groups {
						sendStart, previousWrite, echo := parseTransportDelayHeader(header)
						if 0 < index && int32(sendStart-previousSendStart) <= 0 {
							return fmt.Errorf("write %d starts at %d, not after %d", index, sendStart, previousSendStart)
						}
						previousSendStart = sendStart
						if index == 0 && previousWrite != 0 {
							return fmt.Errorf("the first write reports a previous write of %d us", previousWrite)
						}
						if 0 < index && previousWrite == 0 {
							return fmt.Errorf("write %d reports no previous write", index)
						}
						if echo != 0 {
							return fmt.Errorf("write %d echoes %d before the client received anything", index, echo)
						}
					}
					testTransportDelayH1AwaitClose(serverConn.conn)
					return nil
				}()
			})

			conn := testTransportDelayH1Dial(t, server, carrier, true)
			delayH1 := enableTransportDelayH1(conn, newTransportDelay(testTransportDelayH1Settings(time.Hour), NewId(), TransportTypeH1, func(*TransportDelayStats) {}))
			if delayH1 == nil {
				t.Fatal("the client did not switch to the delay header")
			}
			if err := testTransportDelayH1Write(delayH1.conn, 1); err != nil {
				t.Fatalf("client write: %s", err)
			}
			delayH1.conn.Close()
			testTransportDelayH1ServerResult(t, serverResults)
		})
	}
}

// Rollout in either order keeps the legacy wire: a new client offering to an
// old server, and an old client dialing a new server, exchange every payload
// byte for byte with neither side stamping.
func TestTransportDelayH1NegotiationMatrix(t *testing.T) {
	type negotiationCase struct {
		name        string
		clientOffer bool
		serverMode  testTransportDelayH1ServerMode
	}
	negotiationCases := []negotiationCase{
		{name: "new-client-old-server", clientOffer: true, serverMode: testTransportDelayH1ServerOld},
		{name: "old-client-new-server", clientOffer: false, serverMode: testTransportDelayH1ServerNew},
	}
	for _, carrier := range testTransportDelayH1Carriers {
		for _, negotiation := range negotiationCases {
			t.Run(carrier+"/"+negotiation.name, func(t *testing.T) {
				resetH1UpgradeTestState(t)
				serverResults := make(chan error, 4)
				server := newTestTransportDelayH1Server(t, negotiation.serverMode, func(serverConn *testTransportDelayH1ServerConn) {
					serverResults <- func() error {
						if serverConn.offered != negotiation.clientOffer {
							return fmt.Errorf("server saw offered=%t, expected %t", serverConn.offered, negotiation.clientOffer)
						}
						if serverConn.delayed {
							return errors.New("the server switched to the delay header without a negotiation")
						}
						if _, err := testTransportDelayH1Read(serverConn.conn, 1, 0); err != nil {
							return fmt.Errorf("server read: %w", err)
						}
						if err := testTransportDelayH1Write(serverConn.conn, 2); err != nil {
							return fmt.Errorf("server write: %w", err)
						}
						testTransportDelayH1AwaitClose(serverConn.conn)
						return nil
					}()
				})

				conn := testTransportDelayH1Dial(t, server, carrier, negotiation.clientOffer)
				if subprotocol := h1MessageConnSubprotocol(conn); subprotocol != "" {
					t.Fatalf("the 101 selected %q, expected none", subprotocol)
				}
				delay := newTransportDelay(testTransportDelayH1Settings(time.Hour), NewId(), TransportTypeH1, func(*TransportDelayStats) {
					t.Error("a legacy connection published delay stats")
				})
				if delayH1 := enableTransportDelayH1(conn, delay); delayH1 != nil {
					t.Fatal("the client switched to the delay header without an accept")
				}
				if err := testTransportDelayH1Write(conn, 1); err != nil {
					t.Fatalf("client write: %s", err)
				}
				if _, err := testTransportDelayH1Read(conn, 2, 0); err != nil {
					t.Fatalf("client read: %s", err)
				}
				conn.Close()
				testTransportDelayH1ServerResult(t, serverResults)
				delay.flush()
			})
		}
	}
}

// The server's reader censors its echo through the peer conn: after a handoff
// longer than the receiver-limited threshold, the next write group it receives
// echoes no sample, and the one after it echoes again.
func TestTransportDelayH1NoteHandoffCensorsTheEcho(t *testing.T) {
	for _, carrier := range testTransportDelayH1Carriers {
		t.Run(carrier, func(t *testing.T) {
			resetH1UpgradeTestState(t)
			serverResults := make(chan error, 4)
			server := newTestTransportDelayH1Server(t, testTransportDelayH1ServerNew, func(serverConn *testTransportDelayH1ServerConn) {
				serverResults <- func() error {
					readLane := h1TransportDelayReadLane(serverConn.conn)
					if !serverConn.delayed || readLane == nil {
						return errors.New("the server did not switch to the delay header")
					}
					if err := serverConn.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
						return err
					}
					// the handoff of message 0 is long, of message 1 short
					for index, handoff := range []time.Duration{10 * time.Millisecond, 0, 0} {
						_, message, err := ReadH1PooledMessage(serverConn.conn, testTransportDelayH1MaxMessageByteCount)
						if err != nil {
							return err
						}
						MessagePoolReturn(message)
						echo := readLane.delay.echo.Load()
						if index == 1 && echo != 0 {
							return fmt.Errorf("the write after a long handoff echoes %d, expected 0", echo)
						}
						if index != 1 && echo == 0 {
							return fmt.Errorf("write %d echoes no sample", index)
						}
						NoteTransportDelayHandoff(serverConn.conn, handoff)
					}
					testTransportDelayH1AwaitClose(serverConn.conn)
					return nil
				}()
			})

			conn := testTransportDelayH1Dial(t, server, carrier, true)
			delayH1 := enableTransportDelayH1(conn, newTransportDelay(testTransportDelayH1Settings(time.Hour), NewId(), TransportTypeH1, func(*TransportDelayStats) {}))
			if delayH1 == nil {
				t.Fatal("the client did not switch to the delay header")
			}
			// three single writes are three write groups
			if err := delayH1.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				if err := delayH1.conn.WriteMessage(websocket.BinaryMessage, testTransportDelayH1Payload(100, 7)); err != nil {
					t.Fatal(err)
				}
			}
			delayH1.conn.Close()
			testTransportDelayH1ServerResult(t, serverResults)
		})
	}
}

// A delayed WebSocket bounds the payload alone: the read limit set through the
// wrapper admits a payload at the limit although the header rides inside the
// WebSocket message, and refuses one byte more. Close frames reach the peer
// through the unwrapped conn, as on the legacy wire.
func TestTransportDelayH1WebSocketReadLimitAndClose(t *testing.T) {
	const readLimit = 4096
	const closeCode = 4123

	if _, ok := H1WebSocketConn(&FramedMessageConn{}); ok {
		t.Fatal("h1+ has no WebSocket to unwrap")
	}

	for _, overLimit := range []bool{false, true} {
		t.Run(fmt.Sprintf("overLimit=%t", overLimit), func(t *testing.T) {
			resetH1UpgradeTestState(t)
			serverResults := make(chan error, 4)
			server := newTestTransportDelayH1Server(t, testTransportDelayH1ServerNew, func(serverConn *testTransportDelayH1ServerConn) {
				serverResults <- func() error {
					if !serverConn.delayed {
						return errors.New("the server did not switch to the delay header")
					}
					serverConn.conn.SetReadLimit(readLimit)
					if err := serverConn.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
						return err
					}
					_, message, err := ReadH1PooledMessage(serverConn.conn, 2*readLimit)
					if overLimit {
						if err == nil {
							MessagePoolReturn(message)
							return errors.New("a payload over the limit was read")
						}
						return nil
					}
					if err != nil {
						return fmt.Errorf("a payload at the limit failed: %w", err)
					}
					byteCount := len(message)
					MessagePoolReturn(message)
					if byteCount != readLimit {
						return fmt.Errorf("read %d payload bytes, expected %d", byteCount, readLimit)
					}
					ws, ok := H1WebSocketConn(serverConn.conn)
					if !ok {
						return errors.New("the delayed conn did not unwrap to its WebSocket")
					}
					return ws.WriteControl(
						websocket.CloseMessage,
						websocket.FormatCloseMessage(closeCode, "test"),
						time.Now().Add(time.Second),
					)
				}()
				testTransportDelayH1AwaitClose(serverConn.conn)
			})

			conn := testTransportDelayH1Dial(t, server, testTransportDelayH1CarrierWebSocket, true)
			delayH1 := enableTransportDelayH1(conn, newTransportDelay(testTransportDelayH1Settings(time.Hour), NewId(), TransportTypeH1, func(*TransportDelayStats) {}))
			if delayH1 == nil {
				t.Fatal("the client did not switch to the delay header")
			}
			payloadByteCount := readLimit
			if overLimit {
				payloadByteCount += 1
			}
			if err := delayH1.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := delayH1.conn.WriteMessage(websocket.BinaryMessage, testTransportDelayH1Payload(payloadByteCount, 3)); err != nil {
				t.Fatal(err)
			}
			if !overLimit {
				delayH1.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				_, _, err := delayH1.conn.ReadMessage()
				var closeErr *websocket.CloseError
				if !errors.As(err, &closeErr) || closeErr.Code != closeCode {
					t.Fatalf("read %v, expected the close frame %d", err, closeCode)
				}
			}
			delayH1.conn.Close()
			testTransportDelayH1ServerResult(t, serverResults)
		})
	}
}

// The production h1 lifecycle offers the header from its settings, switches to
// it when the 101 accepts, and publishes the connection's intervals through the
// route manager. Its reader classifies stripped payloads: a latency test is
// echoed, and a payload at the client's message limit still fits although the
// header rides inside the WebSocket message. A transport without delay
// settings, and a connection that declares provide intent, never offer. The
// edge's answer to an offer, an old server's included, replaces the one the
// route manager recorded for the client's P2P (DELAY.md §6.1); a connection
// that does not offer leaves it.
func TestTransportDelayH1PlatformTransport(t *testing.T) {
	type platformCase struct {
		name           string
		enableH1Plus   bool
		transportDelay *TransportDelaySettings
		provideIntent  bool
		// the server predates the header and never selects it
		oldServer  bool
		negotiated bool
		// the recorded edge answer before and after the connection
		edgeAcceptedBefore bool
		edgeAcceptedAfter  bool
	}
	platformCases := []platformCase{
		{name: "websocket", transportDelay: testTransportDelayH1Settings(50 * time.Millisecond), negotiated: true, edgeAcceptedAfter: true},
		{name: "h1plus", enableH1Plus: true, transportDelay: testTransportDelayH1Settings(50 * time.Millisecond), negotiated: true, edgeAcceptedAfter: true},
		{name: "old-server", transportDelay: testTransportDelayH1Settings(50 * time.Millisecond), oldServer: true, edgeAcceptedBefore: true},
		{name: "provide-intent", transportDelay: testTransportDelayH1Settings(50 * time.Millisecond), provideIntent: true, edgeAcceptedBefore: true, edgeAcceptedAfter: true},
		{name: "no-settings", edgeAcceptedBefore: true, edgeAcceptedAfter: true},
	}
	for _, platform := range platformCases {
		t.Run(platform.name, func(t *testing.T) {
			resetH1UpgradeTestState(t)
			settings := testingPlatformTransportSettings()
			settings.EnableH1Plus = platform.enableH1Plus
			settings.TransportDelay = platform.transportDelay
			// the client's pings keep its writer busy across intervals
			settings.PingTimeout = 20 * time.Millisecond
			limitPayload := testTransportDelayH1Payload(int(settings.H1MaxMessageByteCount), 9)
			latencyTest := testTransportDelayH1Payload(16, 3)

			type platformConnection struct {
				offered bool
				delayed bool
			}
			connections := make(chan platformConnection, 16)
			echoes := make(chan bool, 16)
			serverMode := testTransportDelayH1ServerNew
			if platform.oldServer {
				serverMode = testTransportDelayH1ServerOld
			}
			server := newTestTransportDelayH1Server(t, serverMode, func(serverConn *testTransportDelayH1ServerConn) {
				select {
				case connections <- platformConnection{offered: serverConn.offered, delayed: serverConn.delayed}:
				default:
				}
				stop := make(chan struct{})
				writerDone := make(chan struct{})
				go func() {
					defer close(writerDone)
					write := func(message []byte) bool {
						if err := serverConn.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
							return false
						}
						return serverConn.conn.WriteMessage(websocket.BinaryMessage, message) == nil
					}
					// the latency test follows the limit payload, so its echo
					// shows the connection outlived the payload
					if !write(limitPayload) || !write(latencyTest) {
						return
					}
					for {
						select {
						case <-stop:
							return
						case <-time.After(10 * time.Millisecond):
						}
						if !write([]byte{}) {
							return
						}
					}
				}()
				for {
					if err := serverConn.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
						break
					}
					_, message, err := ReadH1PooledMessage(serverConn.conn, testTransportDelayH1MaxMessageByteCount)
					if err != nil {
						break
					}
					if bytes.Equal(message, latencyTest) {
						select {
						case echoes <- serverConn.delayed:
						default:
						}
					}
					MessagePoolReturn(message)
				}
				close(stop)
				serverConn.conn.Close()
				<-writerDone
			})

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			strategySettings := DefaultClientStrategySettings()
			strategySettings.EnableNormal = true
			strategySettings.EnableResilient = false
			strategy := NewClientStrategy(ctx, strategySettings)
			defer strategy.Close()
			routeManager := NewRouteManager(ctx, "transport-delay-h1")
			routeManager.noteTransportDelayEdgeAnswer(platform.edgeAcceptedBefore)
			published := &testTransportDelayH1Published{}
			defer routeManager.addTransportDelayStatsCallback(published.add)()
			transport := NewPlatformTransportWithTargetMode(
				ctx,
				strategy,
				routeManager,
				"ws"+strings.TrimPrefix(server.URL, "http"),
				&ClientAuth{ByJwt: "testing", InstanceId: NewId(), AppVersion: "testing", ProvideIntent: platform.provideIntent},
				TransportModeH1,
				settings,
			)
			defer transport.Close()

			offered := platform.transportDelay != nil && !platform.provideIntent
			connection := receivePlatformAuthWitness(t, connections, "the transport did not connect")
			if connection.offered != offered || connection.delayed != platform.negotiated {
				t.Fatalf(
					"server saw offered=%t delayed=%t, expected offered=%t delayed=%t",
					connection.offered,
					connection.delayed,
					offered,
					platform.negotiated,
				)
			}
			if delayed := receivePlatformAuthWitness(t, echoes, "the latency test was not echoed"); delayed != platform.negotiated {
				t.Fatalf("the echo arrived on a connection with delayed=%t, expected %t", delayed, platform.negotiated)
			}
			// the client's reader echoed, so the connection is past its 101
			if accepted := routeManager.transportDelayEdgeAccepted.Load(); accepted != platform.edgeAcceptedAfter {
				t.Fatalf("recorded edge acceptance=%t after the connection, expected %t", accepted, platform.edgeAcceptedAfter)
			}
			if platform.negotiated && !waitForCondition(5*time.Second, func() bool {
				for _, interval := range published.snapshot() {
					if interval.TransportType == TransportTypeH1 && 0 < interval.ReceiveByteCount &&
						interval.ReceiveQueueDelayValid && interval.SendQueueDelayValid {
						return true
					}
				}
				return false
			}) {
				t.Fatalf("no interval with both queue samples was published: %+v", published.snapshot())
			}

			closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer closeCancel()
			if err := transport.CloseAndWait(closeCtx); err != nil {
				t.Fatal(err)
			}
			intervals := published.snapshot()
			if !platform.negotiated {
				if len(intervals) != 0 {
					t.Fatalf("a legacy connection published %d intervals", len(intervals))
				}
				return
			}
			// the connection's last interval was flushed at its end, so the sum
			// covers everything the connection received
			receiveByteCount := ByteCount(0)
			connectionIds := map[Id]bool{}
			for _, interval := range intervals {
				receiveByteCount += interval.ReceiveByteCount
				connectionIds[interval.ConnectionId] = true
			}
			if len(connectionIds) != 1 {
				t.Fatalf("intervals of %d connections, expected 1", len(connectionIds))
			}
			if receiveByteCount < ByteCount(len(limitPayload)) {
				t.Fatalf("received %d bytes, fewer than the %d byte limit payload", receiveByteCount, len(limitPayload))
			}
		})
	}
}
