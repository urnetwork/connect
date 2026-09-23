//go:build unix

package connect

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Compare production message carriers over real loopback TLS. Both arms have
// the same ready 11-message/1200-byte batch (the existing 12 KiB drain threshold
// permits the last ready message to cross it), 16 KiB bounded scratch, TLS
// configuration, payload verification, and one reader/writer per direction.
// Handshake and warming are excluded. This is a carrier efficiency benchmark;
// it does not measure Android battery, VPN throughput, RTT or loss behavior.
// Upload/download have one packet per op. Duplex has two packets per op: its
// standard ns/op, B/op, and allocs/op must be divided by two for per-packet
// comparisons. The explicit cpu-ns/packet and tls-writes/packet are normalized.
//
// Reproduce with paired runs, keeping host activity/cores fixed:
// go test -run '^$' -bench '^BenchmarkH1PlusTLS1200$' -benchtime=300000x -count=10
func BenchmarkH1PlusTLS1200(b *testing.B) {
	for _, direction := range []string{"upload", "download", "duplex"} {
		b.Run(direction, func(b *testing.B) {
			for _, framed := range []bool{false, true} {
				name := "websocket"
				if framed {
					name = "h1plus"
				}
				b.Run(name, func(b *testing.B) { benchmarkH1PlusTLS1200(b, direction, framed) })
			}
		})
	}
}

type h1PlusBenchmarkCountConn struct {
	net.Conn
	writes atomic.Uint64
}

func (c *h1PlusBenchmarkCountConn) Write(p []byte) (int, error) {
	c.writes.Add(1)
	return c.Conn.Write(p)
}

type h1PlusBenchmarkEndpoint struct {
	conn    H1MessageConn
	counted *h1PlusBenchmarkCountConn
	batch   *WebSocketWriteBatchConn
}

func (e *h1PlusBenchmarkEndpoint) write(messages [][]byte) error {
	if err := e.conn.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return err
	}
	if framed, ok := e.conn.(*FramedMessageConn); ok {
		return framed.WriteMessages(messages)
	}
	e.batch.BeginWriteBatch()
	for _, message := range messages {
		if err := e.conn.WriteMessage(websocket.BinaryMessage, message); err != nil {
			e.batch.AbortWriteBatch()
			return err
		}
	}
	return e.batch.FlushWriteBatch()
}

// Place the same above-TLS write counter (and WS-only production coalescer)
// on the accepted server socket, retaining the HTTP reader's prefetched bytes.
type h1PlusBenchmarkResponseWriter struct {
	http.ResponseWriter
	endpoint *h1PlusBenchmarkEndpoint
	framed   bool
}

func (w *h1PlusBenchmarkResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffered, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err != nil {
		return nil, nil, err
	}
	w.endpoint.counted = &h1PlusBenchmarkCountConn{Conn: conn}
	conn = w.endpoint.counted
	if !w.framed {
		w.endpoint.batch = NewWebSocketWriteBatchConn(conn)
		conn = w.endpoint.batch
	}
	return conn, bufio.NewReadWriter(buffered.Reader, bufio.NewWriter(conn)), nil
}

func h1PlusBenchmarkCPU(b *testing.B) int64 {
	b.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		b.Fatal(err)
	}
	return usage.Utime.Nano() + usage.Stime.Nano()
}

func benchmarkH1PlusTLS1200(b *testing.B, direction string, framed bool) {
	b.StopTimer()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	serverReady := make(chan h1PlusBenchmarkEndpoint, 1)
	serverError := make(chan error, 1)
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e := h1PlusBenchmarkEndpoint{}
		wrapped := &h1PlusBenchmarkResponseWriter{ResponseWriter: w, endpoint: &e, framed: framed}
		var err error
		if framed {
			var conn net.Conn
			conn, err = AcceptFramedUpgrade(wrapped, r, H1FramerProtocol, time.Second)
			if err == nil {
				e.conn, err = NewFramedMessageConn(conn, H1FramerProtocol, 65535, nil)
			}
		} else {
			upgrader := websocket.Upgrader{ReadBufferSize: 4096, WriteBufferSize: 4096}
			e.conn, err = upgrader.Upgrade(wrapped, r, nil)
		}
		if err != nil {
			serverError <- err
			return
		}
		defer e.conn.Close()
		e.conn.SetReadLimit(65535)
		serverReady <- e
		select {
		case <-release:
		case <-ctx.Done():
		}
	}))
	defer server.Close()
	defer close(release)
	client := h1PlusBenchmarkEndpoint{}
	tlsConfig := server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tlsConfig.NextProtos = []string{"http/1.1"}
	dialer := &websocket.Dialer{HandshakeTimeout: 3 * time.Second, ReadBufferSize: 2048, WriteBufferSize: 2048}
	dialer.NetDialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&tls.Dialer{Config: tlsConfig}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		client.counted = &h1PlusBenchmarkCountConn{Conn: conn}
		// ClientStrategy uses its shared WsDialer for both Upgrade attempts,
		// so the custom arm also retains this inactive pass-through wrapper.
		client.batch = NewWebSocketWriteBatchConn(client.counted)
		return client.batch, nil
	}
	var err error
	client.conn, err = DialH1Messages(ctx, "wss"+strings.TrimPrefix(server.URL, "https"), nil, dialer, H1FramerProtocol, 65535, framed, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer client.conn.Close()
	var peer h1PlusBenchmarkEndpoint
	select {
	case peer = <-serverReady:
	case err := <-serverError:
		b.Fatal(err)
	case <-ctx.Done():
		b.Fatal(ctx.Err())
	}
	_ = client.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	_ = peer.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	var payloads [32][]byte
	for i := range payloads {
		payloads[i] = bytes.Repeat([]byte{byte(i + 1)}, 1200)
	}
	read := func(e *h1PlusBenchmarkEndpoint, count int) error {
		for i := range count {
			kind, message, err := ReadH1PooledMessage(e.conn, 1200)
			if err != nil {
				return err
			}
			correct := kind == websocket.BinaryMessage && bytes.Equal(message, payloads[i%len(payloads)])
			MessagePoolReturn(message)
			if !correct {
				return fmt.Errorf("packet %d changed, reordered, or changed message boundary", i)
			}
		}
		return nil
	}
	// Exclude lazy per-connection storage and initial small TLS records from
	// the sample, without changing the steady-state encoder/decoder code.
	for _, pair := range [][2]*h1PlusBenchmarkEndpoint{{&client, &peer}, {&peer, &client}} {
		for range 16 {
			if err := pair[0].write(payloads[:1]); err != nil {
				b.Fatal(err)
			}
			if err := read(pair[1], 1); err != nil {
				b.Fatal(err)
			}
		}
	}
	write := func(e *h1PlusBenchmarkEndpoint) error {
		var batch [11][]byte
		for i := 0; i < b.N; {
			count := min(len(batch), b.N-i)
			for j := range count {
				batch[j] = payloads[(i+j)%len(payloads)]
			}
			if err := e.write(batch[:count]); err != nil {
				return err
			}
			i += count
		}
		return nil
	}
	clientWrites := client.counted.writes.Load()
	serverWrites := peer.counted.writes.Load()
	processCPU := h1PlusBenchmarkCPU(b)
	packets := b.N
	if direction == "duplex" {
		packets *= 2
	}
	b.SetBytes(int64(packets / b.N * 1200))
	b.ReportAllocs()
	b.ResetTimer()
	b.StartTimer()
	done := make(chan error, 4)
	workers := 0
	if direction == "upload" || direction == "duplex" {
		workers += 2
		go func() { done <- write(&client) }()
		go func() { done <- read(&peer, b.N) }()
	}
	if direction == "download" || direction == "duplex" {
		workers += 2
		go func() { done <- write(&peer) }()
		go func() { done <- read(&client, b.N) }()
	}
	for range workers {
		if err := <-done; err != nil {
			b.StopTimer()
			b.Fatal(err)
		}
	}
	b.StopTimer()
	processCPU = h1PlusBenchmarkCPU(b) - processCPU
	writes := client.counted.writes.Load() - clientWrites + peer.counted.writes.Load() - serverWrites
	wantWrites := uint64((b.N + 10) / 11)
	if direction == "duplex" {
		wantWrites *= 2
	}
	if writes != wantWrites {
		b.Fatalf("batching mismatch: TLS writes=%d want=%d", writes, wantWrites)
	}
	b.ReportMetric(float64(processCPU)/float64(packets), "cpu-ns/packet")
	b.ReportMetric(float64(writes)/float64(packets), "tls-writes/packet")
}
