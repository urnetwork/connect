package connect

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/quic-go/qlog"
	"github.com/quic-go/quic-go/qlogwriter"
)

func flightSent(flight *quicSendFlight, packet int64, frames ...*qlog.StreamFrame) {
	event := qlog.PacketSent{Header: qlog.PacketHeader{PacketType: qlog.PacketType1RTT, PacketNumber: qlog.PacketNumber(packet)}}
	for _, frame := range frames {
		event.Frames = append(event.Frames, qlog.Frame{Frame: frame})
	}
	flight.sent(event)
}

func flightAck(flight *quicSendFlight, first, last int64) {
	flight.received(qlog.PacketReceived{
		Header: qlog.PacketHeader{PacketType: qlog.PacketType1RTT},
		Frames: []qlog.Frame{{Frame: &qlog.AckFrame{AckRanges: []qlog.AckRange{{
			Smallest: qlog.PacketNumber(first), Largest: qlog.PacketNumber(last),
		}}}}},
	})
}

func TestQuicSendFlightOwnsRetransmissionRootsUntilTheirAck(t *testing.T) {
	flight := newQuicSendFlight()
	flightSent(flight, 10, &qlog.StreamFrame{StreamID: 0, Offset: 0, Length: 1200})
	flight.lost(qlog.PacketLost{Header: qlog.PacketHeader{PacketType: qlog.PacketType1RTT, PacketNumber: 10}})
	flightAck(flight, 10, 10)
	if got := flight.snapshot(); got.Frames != 1 || got.Bytes != 1200 {
		t.Fatalf("late ACK released retransmission owner: %+v", got)
	}
	flightSent(flight, 11, &qlog.StreamFrame{StreamID: 0, Offset: 0, Length: 400})
	if got := flight.snapshot(); got.Frames != 2 || got.Bytes != 1200 {
		t.Fatalf("split must retain two roots without duplicating payload: %+v", got)
	}
	flightAck(flight, 11, 11)
	if got := flight.snapshot(); got.Frames != 1 || got.Bytes != 800 {
		t.Fatalf("prefix ACK released wrong owner: %+v", got)
	}
	flightSent(flight, 12, &qlog.StreamFrame{StreamID: 0, Offset: 400, Length: 800})
	flightAck(flight, 10, 11)
	if got := flight.snapshot(); got.Frames != 1 || got.Bytes != 800 {
		t.Fatalf("duplicate ACK released retransmission: %+v", got)
	}
	flightAck(flight, 12, 12)
	if got := flight.snapshot(); got.Frames != 0 || got.Bytes != 0 {
		t.Fatalf("final ACK retained roots: %+v", got)
	}
}

func TestQuicSendFlightSilentPtoTransferAndLateAck(t *testing.T) {
	flight := newQuicSendFlight()
	flightSent(flight, 1,
		&qlog.StreamFrame{StreamID: 0, Offset: 0, Length: 600},
		&qlog.StreamFrame{StreamID: 4, Offset: 0, Length: 500},
	)
	// QueueProbePacket transfers the whole old packet without PacketLost.
	// Seeing one retransmitted frame must transfer its sibling as well.
	flightSent(flight, 2, &qlog.StreamFrame{StreamID: 0, Offset: 0, Length: 600})
	flightAck(flight, 1, 1)
	if got := flight.snapshot(); got.Frames != 2 || got.Bytes != 1100 {
		t.Fatalf("PTO late ACK released a sibling still queued for retransmission: %+v", got)
	}
	flightAck(flight, 2, 2)
	flightSent(flight, 3, &qlog.StreamFrame{StreamID: 4, Offset: 0, Length: 500})
	flightAck(flight, 3, 3)
	if got := flight.snapshot(); got.Frames != 0 || got.Bytes != 0 {
		t.Fatalf("PTO transfer leaked roots: %+v", got)
	}
}

type flightTestStream struct {
	ctx       context.Context
	cancelled atomic.Int32
}

func (self *flightTestStream) Write(b []byte) (int, error)      { return len(b), nil }
func (self *flightTestStream) StreamID() quic.StreamID          { return 0 }
func (self *flightTestStream) Context() context.Context         { return self.ctx }
func (self *flightTestStream) SetWriteDeadline(time.Time) error { return nil }
func (self *flightTestStream) CancelWrite(quic.StreamErrorCode) { self.cancelled.Add(1) }

func TestQuicSendFlightSmallFramesHardStopAndDeadline(t *testing.T) {
	flight := newQuicSendFlight()
	stream := &flightTestStream{ctx: t.Context()}
	writer := flight.newWriter(stream)
	for i := range quicSendFlightWriteLimit {
		flightSent(flight, int64(i), &qlog.StreamFrame{StreamID: 0, Offset: int64(i), Length: 1})
	}
	writer.SetWriteDeadline(time.Now().Add(5 * time.Millisecond))
	if n, err := writer.Write([]byte{1}); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("full flight did not honor deadline: n=%d err=%v", n, err)
	}
	for i := quicSendFlightWriteLimit; i < quicSendFlightFrameLimit; i++ {
		flightSent(flight, int64(i), &qlog.StreamFrame{StreamID: 0, Offset: int64(i), Length: 1})
	}
	if got := flight.snapshot(); !got.Failed || got.Frames != quicSendFlightFrameLimit || got.Bytes != quicSendFlightFrameLimit || stream.cancelled.Load() == 0 {
		t.Fatalf("tiny payloads escaped the root limit: %+v canceled=%d", got, stream.cancelled.Load())
	}
	flight.close()
	if got := flight.snapshot(); got.Frames != 0 || got.Bytes != 0 || got.PendingFrames != 0 || !got.Closed {
		t.Fatalf("close retained owners: %+v", got)
	}
}

func TestQuicSendFlightDatagramAdmissionAckLossAndQueueFailure(t *testing.T) {
	flight := newQuicSendFlight()
	marker := errors.New("queue closed")
	if err := flight.trySendDatagram(func([]byte) error { return marker }, make([]byte, 1000)); !errors.Is(err, marker) {
		t.Fatal(err)
	}
	if got := flight.snapshot(); got.DatagramFrames != 0 || got.DatagramBytes != 0 {
		t.Fatalf("queue error retained roots: %+v", got)
	}
	for i := range quicSendFlightDatagrams {
		if err := flight.trySendDatagram(func([]byte) error { return nil }, make([]byte, 1000)); err != nil {
			t.Fatal(err)
		}
		flight.sent(qlog.PacketSent{Header: qlog.PacketHeader{PacketType: qlog.PacketType1RTT, PacketNumber: qlog.PacketNumber(i)}, Frames: []qlog.Frame{{Frame: &qlog.DatagramFrame{Length: 1000}}}})
	}
	called := false
	if err := flight.trySendDatagram(func([]byte) error { called = true; return nil }, []byte{1}); !errors.Is(err, errQuicDatagramFlightFull) || called {
		t.Fatalf("full flight copied another datagram: called=%v err=%v", called, err)
	}
	flightAck(flight, 0, 15)
	for i := 16; i < quicSendFlightDatagrams; i++ {
		flight.lost(qlog.PacketLost{Header: qlog.PacketHeader{PacketType: qlog.PacketType1RTT, PacketNumber: qlog.PacketNumber(i)}})
	}
	flightAck(flight, 0, 31)
	if got := flight.snapshot(); got.DatagramFrames != 0 || got.DatagramBytes != 0 || got.Failed {
		t.Fatalf("ACK/loss cleanup: %+v", got)
	}
}

func TestQuicSendFlightPtoMetricsMoveMixedPacketOwnership(t *testing.T) {
	flight := newQuicSendFlight()
	recorder := &quicSendFlightRecorder{flight: flight}
	packet := func(pn int, frames ...qlog.Frame) qlog.PacketSent {
		return qlog.PacketSent{Header: qlog.PacketHeader{PacketType: qlog.PacketType1RTT, PacketNumber: qlog.PacketNumber(pn)}, Frames: frames}
	}
	// Two DATAGRAM-only packets precede one STREAM+DATAGRAM packet and PING.
	for pn := range 3 {
		if err := flight.trySendDatagram(func([]byte) error { return nil }, make([]byte, 500)); err != nil {
			t.Fatal(err)
		}
		frames := []qlog.Frame{{Frame: &qlog.DatagramFrame{Length: 500}}}
		if pn == 2 {
			frames = append(frames, qlog.Frame{Frame: &qlog.StreamFrame{StreamID: 0, Length: 1000}})
		}
		recorder.RecordEvent(packet(pn, frames...))
	}
	recorder.RecordEvent(packet(3, qlog.Frame{Frame: &qlog.PingFrame{}}))
	// Encryption1RTT is the public qlog alias's value4 in pinned v0.61.
	recorder.RecordEvent(qlog.LossTimerUpdated{Type: qlog.LossTimerUpdateTypeExpired, TimerType: qlog.TimerTypePTO, EncLevel: 4})
	// QueueProbePacket silently discarded packets0,1,2 to find the STREAM.
	recorder.RecordEvent(packet(5, qlog.Frame{Frame: &qlog.StreamFrame{StreamID: 0, Length: 400}}))
	recorder.RecordEvent(qlog.MetricsUpdated{PacketsInFlight: 2})
	if got := flight.snapshot(); got.DatagramFrames != 0 || got.Failed {
		t.Fatalf("PTO metrics retained silently discarded datagrams: %+v", got)
	}
	flightAck(flight, 0, 2)
	if got := flight.snapshot(); got.DatagramFrames != 0 || got.Frames != 2 || got.Bytes != 1000 || got.Failed {
		t.Fatalf("first mixed PTO transfer: %+v", got)
	}
	// Probe2 drops the old PING and sends the remaining STREAM root. Count
	// remains2, so MetricsUpdated omits PacketsInFlight (zero in the event).
	recorder.RecordEvent(packet(6, qlog.Frame{Frame: &qlog.StreamFrame{StreamID: 0, Offset: 400, Length: 600}}))
	recorder.RecordEvent(qlog.MetricsUpdated{BytesInFlight: 1000})
	flightAck(flight, 3, 3)
	if got := flight.snapshot(); got.Frames != 2 || got.Bytes != 1000 || got.Failed {
		t.Fatalf("second PTO transfer: %+v", got)
	}
	flightAck(flight, 5, 6)
	if got := flight.snapshot(); got.Frames != 0 || got.Bytes != 0 || got.Failed {
		t.Fatalf("probe ACK cleanup: %+v", got)
	}
}

func TestQuicSendFlightPtoUnobservablePacketFailsClosed(t *testing.T) {
	flight := newQuicSendFlight()
	stream := &flightTestStream{ctx: t.Context()}
	flight.newWriter(stream)
	recorder := &quicSendFlightRecorder{flight: flight}
	flightSent(flight, 1, &qlog.StreamFrame{StreamID: 0, Length: 20})
	recorder.RecordEvent(qlog.LossTimerUpdated{Type: qlog.LossTimerUpdateTypeExpired, TimerType: qlog.TimerTypePTO, EncLevel: 4})
	recorder.RecordEvent(qlog.PacketSent{Header: qlog.PacketHeader{PacketType: qlog.PacketType1RTT, PacketNumber: 3}, Frames: []qlog.Frame{{Frame: &qlog.AckFrame{}}}})
	if got := flight.snapshot(); !got.Failed || got.Bytes != 20 || stream.cancelled.Load() == 0 {
		t.Fatalf("ambiguous PTO released credit: %+v", got)
	}
}

func TestQuicSendFlightPacketTableIsBoundedAndTracersCompose(t *testing.T) {
	stats := &H3QuicPacketStats{}
	config := &quic.Config{Tracer: stats.Tracer}
	installQuicSendFlight(config)
	trace := config.Tracer(t.Context(), true, quic.ConnectionID{})
	var recorder qlogwriter.Recorder = trace.AddProducer()
	flight := trace.(*quicSendFlightTrace).flight
	stream := &flightTestStream{ctx: t.Context()}
	flight.newWriter(stream)
	for i := range quicSendFlightPackets + 1 {
		recorder.RecordEvent(qlog.PacketSent{Header: qlog.PacketHeader{PacketType: qlog.PacketType1RTT, PacketNumber: qlog.PacketNumber(i)}, Frames: []qlog.Frame{{Frame: &qlog.PingFrame{}}}})
	}
	if got := flight.snapshot(); !got.Failed || stream.cancelled.Load() == 0 {
		t.Fatalf("packet table overflow did not stop writer: %+v", got)
	}
	if got := stats.Snapshot().SentPacketCount; got != quicSendFlightPackets+1 {
		t.Fatalf("previous tracer lost packet stats: %d", got)
	}
	recorder.RecordEvent(qlog.ConnectionClosed{})
	if got := flight.snapshot(); !got.Closed || got.DatagramFrames != 0 || got.Frames != 0 {
		t.Fatalf("close: %+v", got)
	}
}

type flightBlackholePacketConn struct {
	net.PacketConn
	drop atomic.Bool
}

func (self *flightBlackholePacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if self.drop.Load() {
		return len(b), nil
	}
	return self.PacketConn.WriteTo(b, addr)
}

// The observer is the production tracker, fed by the synchronous QUIC packet
// events which describe real pooled STREAM roots. It is not a budget-claim
// counter. The independent heap delta catches a writer that retains an entire
// blackholed upload while leaving those ownership tables unchanged.
func TestQuicSendFlightWarmedUploadReverseAckBlackhole(t *testing.T) {
	for _, useHTTP3 := range []bool{false, true} {
		name := "raw"
		if useHTTP3 {
			name = "http3"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			certPEM, keyPEM, err := selfSign([]string{"127.0.0.1"}, "flight-test", time.Hour, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			cert, err := tls.X509KeyPair(certPEM, keyPEM)
			if err != nil {
				t.Fatal(err)
			}
			socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			serverSocket := &flightBlackholePacketConn{PacketConn: socket}
			serverTransport := &quic.Transport{Conn: serverSocket}
			listener, err := serverTransport.Listen(&tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{http3.NextProtoH3}}, &quic.Config{
				MaxIdleTimeout: 10 * time.Second, InitialStreamReceiveWindow: uint64(mib(8)), MaxStreamReceiveWindow: uint64(mib(8)),
				InitialConnectionReceiveWindow: uint64(mib(16)), MaxConnectionReceiveWindow: uint64(mib(16)),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer serverTransport.Close()
			defer serverSocket.Close()
			defer listener.Close()
			var received atomic.Int64
			serverDone := make(chan struct{})
			go func() {
				defer close(serverDone)
				conn, err := listener.Accept(ctx)
				if err != nil {
					return
				}
				defer conn.CloseWithError(0, "")
				consume := func(r io.Reader) {
					buf := make([]byte, 32*1024)
					for {
						n, err := r.Read(buf)
						received.Add(int64(n))
						if err != nil {
							return
						}
					}
				}
				if useHTTP3 {
					server := &http3.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.WriteHeader(http.StatusOK)
						w.(http.Flusher).Flush()
						consume(r.Body)
					})}
					server.ServeQUICConn(conn)
				} else {
					stream, err := conn.AcceptStream(ctx)
					if err == nil {
						consume(stream)
					}
				}
			}()
			config := &quic.Config{MaxIdleTimeout: 10 * time.Second, DisablePathMTUDiscovery: true}
			installQuicSendFlight(config)
			conn, err := quic.DialAddr(ctx, socket.LocalAddr().String(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{http3.NextProtoH3}}, config)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseWithError(0, "")
			flight := quicSendFlightForConn(conn)
			flight.bind(conn)
			var stream quicSendFlightStream
			if useHTTP3 {
				client := (&http3.Transport{}).NewClientConn(conn)
				requestStream, err := client.OpenRequestStream(ctx)
				if err != nil {
					t.Fatal(err)
				}
				stream = requestStream
				request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://127.0.0.1/upload", http.NoBody)
				request.ContentLength = -1
				if err := requestStream.SendRequestHeader(request); err != nil {
					t.Fatal(err)
				}
				if _, err := requestStream.ReadResponse(); err != nil {
					t.Fatal(err)
				}
			} else {
				stream, err = conn.OpenStreamSync(ctx)
				if err != nil {
					t.Fatal(err)
				}
			}
			writer := flight.newWriter(stream)
			warm := make([]byte, 2*1024*1024)
			if n, err := writer.Write(warm); err != nil || n != len(warm) {
				t.Fatalf("warm upload = %d, %v", n, err)
			}
			if err := flight.waitIdle(ctx); err != nil {
				t.Fatal(err)
			}
			// Allocate the caller's payload before the baseline. The question is
			// whether QUIC makes additional retained copies of it during loss.
			burst := make([]byte, 8*1024*1024)
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)
			serverSocket.drop.Store(true)
			// Include PTO progress even under -race. Congestion may impose a
			// smaller flight than our ceiling; the exact full-admission boundary
			// is tested above without depending on a loopback scheduler's cwnd.
			writer.SetWriteDeadline(time.Now().Add(time.Second))
			n, err := writer.Write(burst)
			if !errors.Is(err, os.ErrDeadlineExceeded) || n == 0 || n > 256*1024 {
				t.Fatalf("blackholed write must backpressure at retained roots: n=%d err=%v stats=%+v", n, err, flight.snapshot())
			}
			stats := flight.snapshot()
			if stats.Failed || stats.Frames == 0 || stats.PeakFrames > quicSendFlightFrameLimit || stats.Bytes > int64(extenderQuicSendMemoryByteCount) {
				t.Fatalf("blackhole ownership = %+v", stats)
			}
			runtime.GC()
			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			runtime.KeepAlive(burst)
			if growth := int64(after.HeapAlloc) - int64(before.HeapAlloc); growth > 2*1024*1024 {
				t.Fatalf("blackhole retained heap grew %d bytes; owned roots=%+v", growth, stats)
			}
			serverSocket.drop.Store(false)
			writer.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := writer.Write(warm[:64*1024]); err != nil {
				t.Fatalf("recovery write: %v stats=%+v", err, flight.snapshot())
			}
			if err := flight.waitIdle(ctx); err != nil {
				t.Fatalf("recovery ACKs: %v stats=%+v", err, flight.snapshot())
			}
			conn.CloseWithError(0, "")
			if got := flight.snapshot(); got.Frames != 0 || got.Bytes != 0 {
				t.Fatalf("close retained roots: %+v", got)
			}
			select {
			case <-serverDone:
			case <-ctx.Done():
				t.Fatal("server did not stop")
			}
			t.Logf("warm=%d blackhole-admitted=%d actual-STREAM-roots=%d payload=%d heap-growth=%d received=%d", len(warm), n, stats.Frames, stats.Bytes, int64(after.HeapAlloc)-int64(before.HeapAlloc), received.Load())
		})
	}
}

func TestQuicSendFlightWarmedDatagramReverseAckBlackhole(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	certPEM, keyPEM, err := selfSign([]string{"127.0.0.1"}, "datagram-flight", time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverSocket := &flightBlackholePacketConn{PacketConn: socket}
	qt := &quic.Transport{Conn: serverSocket}
	listener, err := qt.Listen(&tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"datagram-flight"}}, &quic.Config{
		EnableDatagrams: true, InitialStreamReceiveWindow: uint64(mib(4)), MaxStreamReceiveWindow: uint64(mib(4)),
		InitialConnectionReceiveWindow: uint64(mib(4)), MaxConnectionReceiveWindow: uint64(mib(4)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { listener.Close(); qt.Close(); serverSocket.Close() }()
	var received atomic.Int64
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		peer, err := listener.Accept(ctx)
		if err != nil {
			return
		}
		defer peer.CloseWithError(0, "")
		stream, err := peer.AcceptStream(ctx)
		if err != nil {
			return
		}
		if _, err := io.CopyN(io.Discard, stream, 2*1024*1024); err != nil {
			return
		}
		if _, err := stream.Write([]byte{1}); err != nil {
			return
		}
		for {
			message, err := peer.ReceiveDatagram(ctx)
			if err != nil {
				return
			}
			received.Add(int64(len(message)))
		}
	}()
	config := &quic.Config{EnableDatagrams: true, DisablePathMTUDiscovery: true}
	installQuicSendFlight(config)
	conn, err := quic.DialAddr(ctx, socket.LocalAddr().String(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"datagram-flight"}}, config)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "")
	flight := quicSendFlightForConn(conn)
	flight.bind(conn)
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	writer := flight.newWriter(stream)
	if _, err := writer.Write(make([]byte, 2*1024*1024)); err != nil {
		t.Fatal(err)
	}
	var ready [1]byte
	if _, err := io.ReadFull(stream, ready[:]); err != nil {
		t.Fatal(err)
	}
	if err := flight.waitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 1000)
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	serverSocket.drop.Store(true)
	admitted, full := 0, 0
	deadline := time.Now().Add(350 * time.Millisecond)
	for time.Now().Before(deadline) {
		err := flight.trySendDatagram(conn.SendDatagram, payload)
		if err == nil {
			admitted++
		} else if errors.Is(err, errQuicDatagramFlightFull) {
			full++
			time.Sleep(time.Millisecond)
		} else {
			t.Fatalf("bounded datagram send: %v; %+v", err, flight.snapshot())
		}
		if got := flight.snapshot(); got.DatagramFrames > quicSendFlightDatagrams || got.DatagramBytes > quicSendFlightDatagrams*int64(len(payload)) || got.Failed {
			t.Fatalf("actual queued + sent DATAGRAM ownership: %+v", got)
		}
	}
	if admitted < quicSendFlightDatagrams || full == 0 {
		t.Fatalf("did not exercise nonblocking flight-full fallback: admitted=%d full=%d", admitted, full)
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if growth := int64(after.HeapAlloc) - int64(before.HeapAlloc); growth > 2*1024*1024 {
		t.Fatalf("DATAGRAM blackhole retained heap grew %d bytes", growth)
	}
	serverSocket.drop.Store(false)
	// DATAGRAMs discarded by PTO are not retransmitted. An ordinary stream
	// byte supplies fresh ACK-eliciting traffic and makes recovery observable.
	if _, err := writer.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := flight.waitIdle(ctx); err != nil {
		t.Fatalf("DATAGRAM recovery: %v %+v", err, flight.snapshot())
	}
	conn.CloseWithError(0, "")
	if got := flight.snapshot(); got.DatagramFrames != 0 || got.Frames != 0 {
		t.Fatalf("close retained roots: %+v", got)
	}
	select {
	case <-serverDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	t.Logf("real DATAGRAM blackhole admitted=%d full=%d received=%d heap-growth=%d", admitted, full, received.Load(), int64(after.HeapAlloc)-int64(before.HeapAlloc))
}

func TestQuicSendFlightReusesFixedTablesAndSeparatesPacketNumberSpaces(t *testing.T) {
	flight := newQuicSendFlight()
	for generation := range 1024 {
		packet := int64(generation * 4)
		if err := flight.trySendDatagram(func([]byte) error { return nil }, []byte{1, 2, 3}); err != nil {
			t.Fatal(err)
		}
		flight.sent(qlog.PacketSent{Header: qlog.PacketHeader{PacketType: qlog.PacketType1RTT, PacketNumber: qlog.PacketNumber(packet)}, Frames: []qlog.Frame{
			{Frame: &qlog.StreamFrame{StreamID: 0, Offset: int64(generation * 100), Length: 100}},
			{Frame: &qlog.DatagramFrame{Length: 3}}, {Frame: &qlog.PingFrame{}},
		}})
		for _, space := range []qlog.PacketType{qlog.PacketTypeInitial, qlog.PacketTypeHandshake} {
			flight.received(qlog.PacketReceived{Header: qlog.PacketHeader{PacketType: space}, Frames: []qlog.Frame{{Frame: &qlog.AckFrame{AckRanges: []qlog.AckRange{{Smallest: 0, Largest: qlog.PacketNumber(packet + 2)}}}}}})
		}
		if got := flight.snapshot(); got.Frames != 1 || got.DatagramFrames != 1 {
			t.Fatalf("another packet-number space released roots: %+v", got)
		}
		flight.lost(qlog.PacketLost{Header: qlog.PacketHeader{PacketType: qlog.PacketType1RTT, PacketNumber: qlog.PacketNumber(packet)}})
		flightAck(flight, packet, packet)
		flightSent(flight, packet+1, &qlog.StreamFrame{StreamID: 0, Offset: int64(generation * 100), Length: 40})
		flightSent(flight, packet+2, &qlog.StreamFrame{StreamID: 0, Offset: int64(generation*100 + 40), Length: 60})
		flightAck(flight, packet, packet+2)
		if got := flight.snapshot(); got.Frames != 0 || got.DatagramFrames != 0 || got.Failed {
			t.Fatalf("generation %d did not release/reuse fixed storage: %+v", generation, got)
		}
	}
}

// Each operation includes a MiB upload and an application round trip. The
// peer allocates no per-operation upload buffer, so allocs/op includes the
// actual QUIC send/ACK/receive work in both halves of the loopback pair.
func BenchmarkQuicSendFlightUpload(b *testing.B) {
	for _, mode := range []string{"untraced", "existing_stats", "bounded"} {
		b.Run(mode, func(b *testing.B) {
			certPEM, keyPEM, err := selfSign([]string{"127.0.0.1"}, "flight-bench", time.Hour, time.Hour)
			if err != nil {
				b.Fatal(err)
			}
			cert, err := tls.X509KeyPair(certPEM, keyPEM)
			if err != nil {
				b.Fatal(err)
			}
			listener, err := quic.ListenAddr("127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"flight-bench"}}, &quic.Config{
				InitialStreamReceiveWindow: uint64(mib(4)), MaxStreamReceiveWindow: uint64(mib(4)),
				InitialConnectionReceiveWindow: uint64(mib(4)), MaxConnectionReceiveWindow: uint64(mib(4)),
			})
			if err != nil {
				b.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithCancel(b.Context())
			defer cancel()
			serverDone := make(chan struct{})
			go func() {
				defer close(serverDone)
				conn, err := listener.Accept(ctx)
				if err != nil {
					return
				}
				defer conn.CloseWithError(0, "")
				stream, err := conn.AcceptStream(ctx)
				if err != nil {
					return
				}
				var scratch [32 * 1024]byte
				for {
					for range 32 {
						if _, err := io.ReadFull(stream, scratch[:]); err != nil {
							return
						}
					}
					if _, err := stream.Write([]byte{1}); err != nil {
						return
					}
				}
			}()
			config := &quic.Config{}
			if mode != "untraced" {
				config.Tracer = (&H3QuicPacketStats{}).Tracer
			}
			if mode == "bounded" {
				installQuicSendFlight(config)
			}
			conn, err := quic.DialAddr(ctx, listener.Addr().String(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"flight-bench"}}, config)
			if err != nil {
				b.Fatal(err)
			}
			defer conn.CloseWithError(0, "")
			stream, err := conn.OpenStreamSync(ctx)
			if err != nil {
				b.Fatal(err)
			}
			var writer io.Writer = stream
			if mode == "bounded" {
				flight := quicSendFlightForConn(conn)
				flight.bind(conn)
				writer = flight.newWriter(stream)
			}
			payload := make([]byte, 1024*1024)
			var ack [1]byte
			for range 2 {
				if _, err := writer.Write(payload); err != nil {
					b.Fatal(err)
				}
				if _, err := io.ReadFull(stream, ack[:]); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			b.ResetTimer()
			for range b.N {
				if _, err := writer.Write(payload); err != nil {
					b.Fatal(err)
				}
				if _, err := io.ReadFull(stream, ack[:]); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			conn.CloseWithError(0, "")
			<-serverDone
		})
	}
}
