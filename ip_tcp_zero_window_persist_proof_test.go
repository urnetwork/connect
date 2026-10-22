package connect

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Copied at the actual reader's Cond.Wait boundary while its mutex is held.
type zeroWindowWaitSnapshot struct {
	at       time.Time
	window   uint32
	ack      uint32
	next     uint32
	retained ByteCount
	chunks   int
}

// Cond.Wait's Unlock runs after waiter registration, before releasing the
// connection mutex. No production hook or polling invents the waiting state.
type zeroWindowWaitLocker struct {
	sequence *TcpSequence
	waited   chan zeroWindowWaitSnapshot
	lost     atomic.Int64
}

func (self *zeroWindowWaitLocker) Lock() { self.sequence.mutex.Lock() }

func (self *zeroWindowWaitLocker) Unlock() {
	sequence := self.sequence
	snapshot := zeroWindowWaitSnapshot{at: time.Now(), window: sequence.receiveWindowSize,
		ack: sequence.receiveSeqAck, next: sequence.receiveSeq,
		retained: sequence.returnByteCount, chunks: len(sequence.returnChunks) - sequence.returnHead}
	select {
	case self.waited <- snapshot:
	default:
		self.lost.Add(1)
	}
	sequence.mutex.Unlock()
}

// Source packets are synthetic but fully serialized and parsed. Their ACKs
// use the ordinary established receive transition, not field mutation.
func zeroWindowSourcePacket(sequence, ack uint32, flags byte, window uint16, payload []byte) *TcpSendItem {
	state := ConnectionState{ipVersion: 4,
		sourceIp: net.IPv4(198, 51, 100, 17).To4(), sourcePort: 80,
		destinationIp: net.IPv4(192, 0, 2, 17).To4(), destinationPort: 41005,
		sendSeq: ack, windowSize: uint32(window)}
	packet := state.tcpPacket(flags, sequence, payload)
	_, source, destination, transport, _ := parseIpv4(packet)
	var tcp parsedTcp
	parseTcpPacket(source, destination, transport, &tcp)
	return &TcpSendItem{ipPacket: packet, tcp: tcp}
}

// Lost reopen feedback must not strand a reader whose zero-window ACK already
// retired every return byte. A persist probe elicits the current open window.
func TestTcpZeroWindowMissingPersistAfterLostReopen(t *testing.T) {
	checkTcpZeroWindowReopen(t, true)
}

// Injecting only the equivalent genuine window update proves the very same
// waiting reader and HTTP body are healthy independently of a missing probe.
func TestTcpZeroWindowReopenAckResumesHttpControl(t *testing.T) {
	checkTcpZeroWindowReopen(t, false)
}

// This isolates the provider's TCP/window lifecycle. It deliberately does not
// claim an actual Tun/native refusal; that composed discriminator follows.
func checkTcpZeroWindowReopen(t *testing.T, requirePersist bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		local, origin := net.Pipe()
		request := []byte("GET /asset HTTP/1.1\r\nHost: persist.example\r\n\r\n")
		response := []byte("HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\ndone")
		settings := DefaultTcpBufferSettingsWithBufferSize(4)
		settings.Log = NewNoopLogger()
		settings.Mtu = 44
		settings.ReadBufferByteCount = 128
		settings.ReturnResendTimeout = time.Second
		settings.ReturnQueueBudget = NewTransferMemoryBudget(2048)
		settings.DialContextSettings = &DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) { return local, nil }}
		source := SourceId(NewId())
		var sequence *TcpSequence
		var peerLock sync.Mutex
		nextAck := uint32(101)
		var received []byte
		var open atomic.Bool
		var reopenKnown atomic.Bool
		handshake, receivedAll := make(chan struct{}), make(chan struct{})
		var handshakeOnce, receivedOnce sync.Once
		probes := make(chan time.Time, 8)
		applyAck := func(number uint32, window uint16) {
			item := zeroWindowSourcePacket(101+uint32(len(request)), number, tcpFlagAck, window, nil)
			if !sequence.applyEstablishedPureAck(source, TransferKey{}, &item.tcp, item.ipPacket) {
				MessagePoolReturn(item.ipPacket)
				t.Error("ordinary established source ACK was not admitted")
			}
		}
		sequence = NewTcpSequence(ctx, func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
			_, sourceIp, destinationIp, transport, ok := parseIpv4(packet)
			var tcp parsedTcp
			if !ok || !parseTcpPacket(sourceIp, destinationIp, transport, &tcp) {
				t.Error("invalid provider return packet")
				return
			}
			if tcp.syn {
				applyAck(101, 4)
				handshakeOnce.Do(func() { close(handshake) })
				return
			}
			if len(tcp.payload) == 0 {
				return // A pure ACK is not a persist request or an ACK loop.
			}
			peerLock.Lock()
			if tcp.seq == nextAck {
				received = append(received, tcp.payload...)
				nextAck += uint32(len(tcp.payload))
			} else if tcp.seq == nextAck-1 && len(tcp.payload) == 1 && tcp.ack && !tcp.syn && !tcp.fin && !tcp.rst {
				select {
				case probes <- time.Now():
				default:
					t.Error("persist observations exceeded bounded slots")
				}
				if reopenKnown.Load() {
					open.Store(true)
				}
			} else {
				t.Errorf("unexpected return range seq=%d size=%d next=%d", tcp.seq, len(tcp.payload), nextAck)
			}
			number := nextAck
			complete := len(received) == len(response)
			peerLock.Unlock()
			window := uint16(0)
			if open.Load() {
				window = 4096
			}
			applyAck(number, window)
			if complete {
				receivedOnce.Do(func() { close(receivedAll) })
			}
		}, source, protocol.ProvideMode_Network, 4,
			net.IPv4(192, 0, 2, 17).To4(), 41005,
			net.IPv4(198, 51, 100, 17).To4(), 80, 100, settings)
		waits := &zeroWindowWaitLocker{sequence: sequence, waited: make(chan zeroWindowWaitSnapshot, 8)}
		sequence.receiveAckCondOnce.Do(func() { sequence.receiveAckCond = sync.NewCond(waits) })
		serverDone := make(chan error, 1)
		go func() {
			parsed, err := http.ReadRequest(bufio.NewReader(origin))
			if err != nil {
				serverDone <- err
				return
			}
			_ = parsed.Body.Close()
			if parsed.Method != http.MethodGet || parsed.URL.Path != "/asset" || parsed.Host != "persist.example" {
				serverDone <- fmt.Errorf("origin parsed unexpected HTTP request: %s %s %s", parsed.Method, parsed.URL.Path, parsed.Host)
				return
			}
			n, err := origin.Write(response)
			if err == nil && n != len(response) {
				err = io.ErrShortWrite
			}
			serverDone <- err
		}()
		done := make(chan struct{})
		go func() { defer close(done); sequence.Run() }()
		serverJoined := false
		defer func() {
			cancel()
			_ = origin.Close()
			_ = local.Close()
			if !serverJoined {
				<-serverDone
			}
			<-done
			if used := settings.ReturnQueueBudget.UsedByteCount(); used != 0 {
				t.Errorf("joined zero-window flow retained %d return bytes", used)
			}
		}()
		send := func(item *TcpSendItem) {
			if accepted, err := sequence.send(item, -1); !accepted {
				MessagePoolReturn(item.ipPacket)
				t.Fatalf("source TCP admission: %v", err)
			}
		}
		send(zeroWindowSourcePacket(100, 0, tcpFlagSyn, 4, nil))
		<-handshake
		send(zeroWindowSourcePacket(101, 101, tcpFlagAck, 4, request))
		var parked zeroWindowWaitSnapshot
		for parked.window != 0 || parked.ack != 105 || parked.next != 105 || parked.retained != 0 || parked.chunks != 0 {
			parked = <-waits.waited
		}
		synctest.Wait()
		peerLock.Lock()
		prefix := bytes.Clone(received)
		peerLock.Unlock()
		if string(prefix) != "HTTP" || waits.lost.Load() != 0 {
			t.Fatal("zero-window barrier lacks exact first-four-byte/owner evidence")
		}
		// The source application has drained its buffer and emitted an update,
		// but that single ACK is withheld before the provider sees it.
		reopenKnown.Store(true)
		deadline := time.NewTimer(2 * settings.ReturnResendTimeout)
		<-deadline.C
		synctest.Wait()
		probed := false
		select {
		case <-probes:
			probed = true
		default:
		}
		if !probed {
			// A positive release control uses only the equivalent source ACK.
			// It cannot create a new response owner or replace the waiting reader.
			open.Store(true)
			applyAck(105, 4096)
		}
		<-receivedAll
		if err := <-serverDone; err != nil {
			serverJoined = true
			t.Fatal("HTTP origin", err)
		}
		serverJoined = true
		synctest.Wait()
		peerLock.Lock()
		fullResponse := bytes.Clone(received)
		peerLock.Unlock()
		parsedResponse, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(fullResponse)), nil)
		if err != nil {
			t.Fatal("recovered HTTP response", err)
		}
		body, readErr := io.ReadAll(parsedResponse.Body)
		closeErr := parsedResponse.Body.Close()
		sequence.mutex.Lock()
		retained := sequence.returnByteCount
		sequence.mutex.Unlock()
		if parsedResponse.StatusCode != http.StatusOK || string(body) != "done" || readErr != nil || closeErr != nil || !bytes.Equal(fullResponse, response) || retained != 0 {
			t.Fatal("equivalent reopen ACK did not resume the exact HTTP stream and retire its owners")
		}
		t.Logf("zero-window reader actual_wait=%t ack=%d next=%d retained=%d chunks=%d persist_probe=%t exact_http_resumed=true", parked.window == 0, parked.ack, parked.next, parked.retained, parked.chunks, probed)
		if requirePersist && !probed {
			t.Fatalf("TCP_ZERO_WINDOW_NO_PERSIST_OWNER: actual reader parked window0 ack=next=%d with no retained TCP owner; lost reopen produced no probe across %s; equivalent ACK alone resumed exactHTTP", parked.ack, 2*settings.ReturnResendTimeout)
		}
	})
}
