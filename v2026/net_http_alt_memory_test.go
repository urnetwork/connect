package connect

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/qpack"
	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	h3qlog "github.com/quic-go/quic-go/http3/qlog"
	"github.com/quic-go/quic-go/qlog"
	"github.com/quic-go/quic-go/qlogwriter"
)

func newAltMemoryFixture(t *testing.T, handler http.Handler) (*http.Client, *atomic.Int32) {
	return newAltMemoryFixtureMode(t, handler, true)
}

func newAltMemoryFixtureMode(t *testing.T, handler http.Handler, bounded bool) (*http.Client, *atomic.Int32) {
	return newAltMemoryFixtureOptions(t, handler, bounded, &quic.Config{}, nil)
}

func newAltMemoryFixtureOptions(t *testing.T, handler http.Handler, bounded bool, config *quic.Config, wrap func(net.PacketConn) net.PacketConn, configure ...func(*http3.Server)) (*http.Client, *atomic.Int32) {
	t.Helper()
	certPEM, keyPEM, err := selfSign([]string{testAltApiHost}, "alt-memory", time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	hellos := &atomic.Int32{}
	socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if wrap != nil {
		socket = wrap(socket)
	}
	quicTransport := &quic.Transport{Conn: socket}
	listener, err := quicTransport.Listen(&tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{http3.NextProtoH3}, GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) { hellos.Add(1); return nil, nil }}, config)
	if err != nil {
		t.Fatal(err)
	}
	server := &http3.Server{Handler: handler}
	for _, configure := range configure {
		configure(server)
	}
	done := make(chan error, 1)
	go func() { done <- server.ServeListener(listener) }()
	t.Cleanup(func() { server.Close(); listener.Close(); quicTransport.Close(); socket.Close(); <-done })
	if !bounded {
		transport := &http3.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}, MaxResponseHeaderBytes: altMaxHeaderBytes}
		transport.Dial = func(ctx context.Context, _ string, tlsConfig *tls.Config, config *quic.Config) (*quic.Conn, error) {
			return quic.DialAddr(ctx, listener.Addr().String(), tlsConfig, config)
		}
		t.Cleanup(func() { transport.Close() })
		return &http.Client{Transport: transport}, hellos
	}
	fixture := &testAltServer{altUrl: "https://" + listener.Addr().String(), rootCAs: roots}
	strategy := newTestAltStrategy(t, fixture)
	return testAltDialer(t, strategy, "alt h3").HttpClient(), hellos
}

func waitAltMemorySlot(t *testing.T, client *http.Client) {
	t.Helper()
	transport := client.Transport.(*altQuicBoundedTransport)
	select {
	case transport.slot <- struct{}{}:
		<-transport.slot
	case <-time.After(2 * time.Second):
		t.Fatal("alt request slot did not drain")
	}
}

func TestAltMemorySerializesRequestsAndPreservesPooling(t *testing.T) {
	var requests atomic.Int32
	client, hellos := newAltMemoryFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Write([]byte("hello"))
	}))
	url := "https://" + testAltApiHost + "/hello"
	first, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Body.Close()
	result := make(chan error, 1)
	go func() {
		second, err := client.Get(url)
		if err == nil {
			_, err = io.ReadAll(second.Body)
			second.Body.Close()
		}
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("second request escaped held response: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if requests.Load() != 1 {
		t.Fatalf("requests while body held = %d", requests.Load())
	}
	io.Copy(io.Discard, first.Body)
	first.Body.Close()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second request never acquired released slot")
	}
	waitAltMemorySlot(t, client)
	if requests.Load() != 2 || hellos.Load() != 1 {
		t.Fatalf("pooling changed: requests=%d connections=%d", requests.Load(), hellos.Load())
	}
}

func TestAltMemoryRejectsOversizedKnownAndUnknownUploads(t *testing.T) {
	for _, size := range []string{"known", "unknown", "lying", "headers"} {
		t.Run(size, func(t *testing.T) {
			var received atomic.Int64
			client, hellos := newAltMemoryFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n, err := io.Copy(io.Discard, r.Body)
				received.Add(n)
				if err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Write([]byte("ok"))
			}))
			request, _ := http.NewRequest(http.MethodPost, "https://"+testAltApiHost+"/upload", strings.NewReader(strings.Repeat("x", altMaxUploadBytes+1)))
			switch size {
			case "unknown":
				request.ContentLength = -1
			case "lying":
				request.ContentLength = 10
			case "headers":
				request.Body = nil
				request.ContentLength = 0
				request.Header.Set("X-Large", strings.Repeat("x", altMaxHeaderBytes))
			}
			response, err := client.Do(request)
			if err == nil {
				_, readErr := io.ReadAll(response.Body)
				response.Body.Close()
				if readErr == nil && response.StatusCode < 400 {
					t.Fatal("oversized upload succeeded")
				}
			}
			waitAltMemorySlot(t, client)
			if received.Load() > altMaxUploadBytes {
				t.Fatalf("peer received %d upload bytes", received.Load())
			}
			if (size == "known" || size == "headers") && hellos.Load() != 0 {
				t.Fatal("oversized request opened a connection")
			}
		})
	}
}

func TestAltMemoryCancellationAndTrailers(t *testing.T) {
	client, _ := newAltMemoryFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Trailer", "X-Reply-End")
		w.Write([]byte("ok"))
		w.Header().Set("X-Reply-End", "response-trailer")
	}))
	request, _ := http.NewRequest(http.MethodPost, "https://"+testAltApiHost+"/upload", strings.NewReader("hello"))
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	if response.Trailer.Get("X-Reply-End") != "response-trailer" {
		t.Fatalf("lost response trailer: %+v", response.Trailer)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	waiting, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+testAltApiHost+"/waiting", nil)
	if _, err := client.Do(waiting); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting cancellation = %v", err)
	}
	response.Body.Close()
	waitAltMemorySlot(t, client)
}

type altLateTrailerBody struct {
	io.Reader
	trailer http.Header
	value   string
}

func (self *altLateTrailerBody) Read(b []byte) (int, error) {
	n, err := self.Reader.Read(b)
	if err == io.EOF {
		self.trailer.Set("X-End", self.value)
	}
	return n, err
}

func (*altLateTrailerBody) Close() error { return nil }

func TestAltMemoryRejectsUndeclaredLateTrailer(t *testing.T) {
	client, _ := newAltMemoryFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	trailer := http.Header{}
	request, _ := http.NewRequest(http.MethodPost, "https://"+testAltApiHost+"/upload", &altLateTrailerBody{
		Reader: strings.NewReader(strings.Repeat("x", altMaxUploadBytes)), trailer: trailer, value: "late",
	})
	request.Trailer = trailer
	response, err := client.Do(request)
	if err == nil {
		response.Body.Close()
		t.Fatal("undeclared late trailer was silently discarded")
	}
	var unsupported *AltQuicUnsupportedRequestError
	if !errors.As(err, &unsupported) {
		t.Fatalf("late trailer refusal = %v", err)
	}
	waitAltMemorySlot(t, client)
}

func TestAltMemoryUnsupportedRequestsRefuseBeforeDial(t *testing.T) {
	for _, feature := range []string{"trailers", "trailer-header", "connect", "0rtt"} {
		t.Run(feature, func(t *testing.T) {
			client, hellos := newAltMemoryFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unsupported request reached the server") }))
			request, _ := http.NewRequest(http.MethodPost, "https://"+testAltApiHost+"/upload", strings.NewReader("replayable"))
			switch feature {
			case "trailers":
				request.Trailer = http.Header{"X-End": []string{"complete"}}
			case "trailer-header":
				request.Header.Set("Trailer", "X-End")
			case "connect":
				request.Method = http.MethodConnect
			case "0rtt":
				request.Method = http3.MethodGet0RTT
			}
			_, err := client.Do(request)
			var unsupported *AltQuicUnsupportedRequestError
			if !errors.As(err, &unsupported) || hellos.Load() != 0 {
				t.Fatalf("pre-dial refusal: err=%v connections=%d", err, hellos.Load())
			}
			// ClientStrategy rebuilds an independent body for the next carrier.
			// A typed alt refusal must leave that replay path intact.
			retry, err := cloneHttpRequestForAttempt(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := io.ReadAll(retry.Body)
			retry.Body.Close()
			if err != nil || string(payload) != "replayable" {
				t.Fatalf("carrier retry lost body: %q %v", payload, err)
			}
		})
	}
}

func TestAltMemoryUnsupportedTrailerFallsBackThroughStrategy(t *testing.T) {
	settings := DefaultClientStrategySettings()
	settings.RequestTimeout = time.Second
	var dialed, fallback atomic.Int32
	alt := newAltQuicBoundedTransport(&http3.Transport{Dial: func(context.Context, string, *tls.Config, *quic.Config) (*quic.Conn, error) {
		dialed.Add(1)
		return nil, errors.New("unsupported request must not dial")
	}})
	failed := &clientDialer{description: "bounded-alt", settings: settings, minimumWeight: 1, successCount: 1, lastSuccessTime: time.Now(),
		httpClient: &http.Client{Transport: alt}}
	healthy := &clientDialer{description: "other-carrier", settings: settings, minimumWeight: 1, priority: 1, successCount: 1, lastSuccessTime: time.Now(),
		httpClient: &http.Client{Transport: serialTestRoundTripper(func(request *http.Request) (*http.Response, error) {
			fallback.Add(1)
			payload, err := io.ReadAll(request.Body)
			request.Body.Close()
			if err != nil || string(payload) != "payload" || request.Trailer.Get("X-End") != "complete" {
				return nil, fmt.Errorf("fallback changed request: %q %v %+v", payload, err, request.Trailer)
			}
			return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok")), Request: request}, nil
		})}}
	strategy := &ClientStrategy{ctx: t.Context(), log: loggerOrDefault(nil), settings: settings,
		dialers: map[*clientDialer]bool{failed: true, healthy: true}, extenderIpSecrets: map[netip.Addr]string{}}
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.example.invalid/test", strings.NewReader("payload"))
	request.Trailer = http.Header{"X-End": []string{"complete"}}
	hello, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.example.invalid/hello", nil)
	result, err := strategy.HttpSerial(request, hello)
	if err != nil {
		t.Fatal(err)
	}
	// HttpSerial has already consumed and closed the response body.
	if result.response.StatusCode != http.StatusOK || dialed.Load() != 0 || fallback.Load() != 1 || failed.errorCount != 1 {
		t.Fatalf("safe carrier fallback: dialed=%d fallback=%d alt failures=%d result=%+v", dialed.Load(), fallback.Load(), failed.errorCount, result)
	}
}

func TestAltMemoryPreservesHTTPMethodsRedirectsGzipAndHead(t *testing.T) {
	client, hellos := newAltMemoryFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			w.Header().Set("Location", "/final?q=1")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "123")
			return
		}
		payload, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPatch || r.URL.RawQuery != "q=1" || string(payload) != "payload" || r.Header.Get("X-Test") != "value" {
			t.Errorf("changed HTTP request: %s %s %q %+v", r.Method, r.URL, payload, r.Header)
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusCreated)
		zip := gzip.NewWriter(w)
		io.WriteString(zip, "decoded")
		zip.Close()
	}))
	request, _ := http.NewRequest(http.MethodPatch, "https://"+testAltApiHost+"/redirect", strings.NewReader("payload"))
	request.Header.Set("X-Test", "value")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(payload) != "decoded" || response.StatusCode != http.StatusCreated || !response.Uncompressed || response.Request.URL.Path != "/final" || response.TLS == nil {
		t.Fatalf("response semantics: body=%q err=%v response=%+v", payload, err, response)
	}
	response, err = client.Head("https://" + testAltApiHost + "/head")
	if err != nil {
		t.Fatal(err)
	}
	payload, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || len(payload) != 0 || response.ContentLength != 123 {
		t.Fatalf("HEAD semantics: body=%q length=%d err=%v", payload, response.ContentLength, err)
	}
	waitAltMemorySlot(t, client)
	if hellos.Load() != 1 {
		t.Fatalf("redirect/HEAD did not reuse the pooled connection: %d", hellos.Load())
	}
}

type altTestTrace struct {
	record     func(qlogwriter.Event)
	allSchemas bool
}

func (self *altTestTrace) SupportsSchemas(schema string) bool {
	return self.allSchemas || schema == qlog.EventSchema
}
func (self *altTestTrace) AddProducer() qlogwriter.Recorder   { return self }
func (self *altTestTrace) RecordEvent(event qlogwriter.Event) { self.record(event) }
func (*altTestTrace) Close() error                            { return nil }

func TestAltMemoryRealOneByteFlowControlAndPoolWriterReuse(t *testing.T) {
	var oneByteFrames atomic.Int64
	config := &quic.Config{InitialStreamReceiveWindow: 1, MaxStreamReceiveWindow: 1}
	config.Tracer = func(context.Context, bool, quic.ConnectionID) qlogwriter.Trace {
		return &altTestTrace{record: func(event qlogwriter.Event) {
			packet, ok := event.(qlog.PacketReceived)
			if !ok {
				return
			}
			for _, frame := range packet.Frames {
				if stream, ok := frame.Frame.(*qlog.StreamFrame); ok && stream.StreamID%4 == 0 && stream.Length == 1 {
					oneByteFrames.Add(1)
				}
			}
		}}
	}
	client, hellos := newAltMemoryFixtureOptions(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil || n != 100 {
			t.Errorf("fragmented upload=%d %v", n, err)
		}
		fmt.Fprint(w, "ok")
	}), true, config, nil)
	for range quicSendFlightStreams + 3 {
		response, err := client.Post("https://"+testAltApiHost+"/tiny", "text/plain", strings.NewReader(strings.Repeat("x", 100)))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		waitAltMemorySlot(t, client)
		conn := client.Transport.(*altQuicBoundedTransport).connection(testAltApiHost + ":443")
		flight := quicSendFlightForConn(conn)
		if got := flight.snapshot(); got.Frames != 0 || got.Failed || got.PeakFrames > quicSendFlightFrameLimit {
			t.Fatalf("tiny-frame owner after FIN ACK: %+v", got)
		}
		flight.mutex.Lock()
		for _, writer := range flight.writers {
			if writer != nil {
				t.Error("acknowledged pooled writer retained")
			}
		}
		flight.mutex.Unlock()
	}
	if hellos.Load() != 1 || oneByteFrames.Load() < 1100 {
		t.Fatalf("one-byte fragmentation/reuse not exercised: hellos=%d frames=%d", hellos.Load(), oneByteFrames.Load())
	}
}

// The public RequestStream does not expose WriteWithLimit. Model the hostile
// endpoint's smallest credit grant and suppress every ACK, using actual
// packet-sized retained allocations, to prove CancelWrite executes in the
// packet callback before another one-byte frame can be allocated. The real
// one-byte QUIC flow-control integration above separately verifies the pinned
// dependency emits precisely these frame boundaries.
type altFragmentedRequestStream struct {
	flight   *quicSendFlight
	roots    [][]byte
	canceled bool
}

func (*altFragmentedRequestStream) StreamID() quic.StreamID               { return 0 }
func (*altFragmentedRequestStream) Context() context.Context              { return context.Background() }
func (*altFragmentedRequestStream) SetWriteDeadline(time.Time) error      { return nil }
func (self *altFragmentedRequestStream) CancelWrite(quic.StreamErrorCode) { self.canceled = true }
func (self *altFragmentedRequestStream) Write(b []byte) (int, error) {
	for i := range b {
		if self.canceled {
			return i, errQuicSendFlight
		}
		root := make([]byte, 1452)
		root[0] = b[i]
		self.roots = append(self.roots, root)
		flightSent(self.flight, int64(len(self.roots)), &qlog.StreamFrame{StreamID: 0, Offset: int64(len(self.roots) - 1), Length: 1})
	}
	return len(b), nil
}

func TestAltMemoryOneByteUnackedHeaderAndBodyHardStop(t *testing.T) {
	for _, header := range []bool{true, false} {
		t.Run(fmt.Sprintf("header=%v", header), func(t *testing.T) {
			flight := newQuicSendFlight()
			stream := &altFragmentedRequestStream{flight: flight}
			writer := flight.newWriter(stream) // registration precedes headers
			var output io.Writer = writer
			if header {
				output = stream
			} // SendRequestHeader bypasses DATA writer
			n, err := output.Write(make([]byte, quicSendFlightChunk))
			if !errors.Is(err, errQuicSendFlight) || !stream.canceled || n != quicSendFlightFrameLimit || len(stream.roots) != quicSendFlightFrameLimit {
				t.Fatalf("one-byte flight overshot before synchronous cancellation: n=%d roots=%d canceled=%v err=%v", n, len(stream.roots), stream.canceled, err)
			}
			if owned := len(stream.roots) * cap(stream.roots[0]); owned > int(extenderQuicSendMemoryByteCount) {
				t.Fatalf("retained pooled root bytes=%d", owned)
			}
			if got := flight.snapshot(); !got.Failed || got.Frames != len(stream.roots) {
				t.Fatalf("exact owner mismatch: %+v", got)
			}
			flight.close()
			stream.roots = nil
		})
	}
}

type altGatedEOFBody struct {
	first  bool
	eof    chan struct{}
	closed chan struct{}
	once   sync.Once
}

func (self *altGatedEOFBody) Read(b []byte) (int, error) {
	if !self.first {
		self.first = true
		b[0] = 'x'
		return 1, nil
	}
	select {
	case <-self.eof:
		return 0, io.EOF
	case <-self.closed:
		return 0, context.Canceled
	}
}
func (self *altGatedEOFBody) Close() error { self.once.Do(func() { close(self.closed) }); return nil }

func TestAltMemoryHoldsAdmissionUntilFinAckAndThenReuses(t *testing.T) {
	var reverse *flightBlackholePacketConn
	client, hellos := newAltMemoryFixtureOptions(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var first [1]byte
			if _, err := io.ReadFull(r.Body, first[:]); err != nil {
				return
			}
			fmt.Fprint(w, "ok")
			w.(http.Flusher).Flush()
			io.Copy(io.Discard, r.Body)
		} else {
			fmt.Fprint(w, "ok")
		}
	}), true, &quic.Config{}, func(conn net.PacketConn) net.PacketConn {
		reverse = &flightBlackholePacketConn{PacketConn: conn}
		return reverse
	})
	requestBody := &altGatedEOFBody{eof: make(chan struct{}), closed: make(chan struct{})}
	response, err := client.Post("https://"+testAltApiHost+"/fin", "text/plain", requestBody)
	if err != nil {
		t.Fatal(err)
	}
	var reply [2]byte
	if _, err := io.ReadFull(response.Body, reply[:]); err != nil {
		t.Fatal(err)
	}
	reverse.drop.Store(true)
	close(requestBody.eof)
	transport := client.Transport.(*altQuicBoundedTransport)
	conn := transport.connection(testAltApiHost + ":443")
	flight := quicSendFlightForConn(conn)
	deadline := time.Now().Add(time.Second)
	for {
		flight.mutex.Lock()
		fin := false
		for _, frame := range flight.frames {
			fin = fin || frame.used && frame.fin
		}
		flight.mutex.Unlock()
		if fin {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("request FIN not tracked: %+v", flight.snapshot())
		}
		time.Sleep(time.Millisecond)
	}
	response.Body.Close()
	second := make(chan error, 1)
	go func() {
		r, err := client.Get("https://" + testAltApiHost + "/next")
		if err == nil {
			_, err = io.Copy(io.Discard, r.Body)
			r.Body.Close()
		}
		second <- err
	}()
	select {
	case err := <-second:
		t.Fatalf("request slot released before FIN ACK: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	reverse.drop.Store(false)
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("FIN ACK did not release the pooled request slot")
	}
	waitAltMemorySlot(t, client)
	if hellos.Load() != 1 {
		t.Fatalf("FIN ACK recovery unnecessarily retired the connection: %d", hellos.Load())
	}
}

func TestAltMemoryCancellationDuringUploadAndResponseRead(t *testing.T) {
	for _, upload := range []bool{true, false} {
		t.Run(fmt.Sprintf("upload=%v", upload), func(t *testing.T) {
			started, stopped := make(chan struct{}), make(chan struct{})
			client, _ := newAltMemoryFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(stopped)
				if upload {
					var first [1]byte
					io.ReadFull(r.Body, first[:])
					close(started)
					io.Copy(io.Discard, r.Body)
				} else {
					fmt.Fprint(w, "x")
					w.(http.Flusher).Flush()
					close(started)
					<-r.Context().Done()
				}
			}))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var body *altGatedEOFBody
			request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+testAltApiHost+"/cancel", nil)
			if upload {
				body = &altGatedEOFBody{eof: make(chan struct{}), closed: make(chan struct{})}
				request.Body = body
				request.ContentLength = -1
			}
			result := make(chan error, 1)
			go func() {
				response, err := client.Do(request)
				if err == nil {
					_, err = io.Copy(io.Discard, response.Body)
					response.Body.Close()
				}
				result <- err
			}()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("request never started")
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("active cancellation returned %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("active cancellation left a blocked reader")
			}
			waitAltMemorySlot(t, client)
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("peer handler did not stop")
			}
			if body != nil {
				select {
				case <-body.closed:
				default:
					t.Fatal("canceled upload body was not closed")
				}
			}
		})
	}
}

func TestAltMemoryPoolExpirationCloseAndCloseIdleConnections(t *testing.T) {
	client, hellos := newAltMemoryFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
	transport := client.Transport.(*altQuicBoundedTransport)
	url := "https://" + testAltApiHost + "/pool"
	request := func() *http.Response {
		response, err := client.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			t.Fatal(err)
		}
		return response
	}
	response := request()
	first := transport.connection(testAltApiHost + ":443")
	client.CloseIdleConnections()
	if first.Context().Err() != nil {
		t.Fatal("CloseIdleConnections closed the active body owner")
	}
	response.Body.Close()
	waitAltMemorySlot(t, client) // read-to-EOF alone did not release; Close did
	client.CloseIdleConnections()
	if first.Context().Err() == nil {
		t.Fatal("idle connection was not closed")
	}
	response = request()
	response.Body.Close()
	waitAltMemorySlot(t, client)
	expired := transport.connection(testAltApiHost + ":443")
	expired.CloseWithError(0, "expired pooled entry")
	response = request()
	if hellos.Load() != 3 {
		t.Fatalf("closed entries were reused: hellos=%d", hellos.Load())
	}
	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	waitAltMemorySlot(t, client)
	if _, err := client.Get(url); !errors.Is(err, http3.ErrTransportClosed) {
		t.Fatalf("closed transport reused: %v", err)
	}
	transport.mutex.Lock()
	defer transport.mutex.Unlock()
	for _, entry := range transport.connections {
		if entry != nil {
			t.Fatal("closed pool retained a connection")
		}
	}
}

func TestAltMemoryGoawayRetiresPoolAndJoinsShutdown(t *testing.T) {
	goaway := make(chan struct{})
	var signal sync.Once
	config := &quic.Config{Tracer: func(context.Context, bool, quic.ConnectionID) qlogwriter.Trace {
		return &altTestTrace{allSchemas: true, record: func(event qlogwriter.Event) {
			if frame, ok := event.(h3qlog.FrameCreated); ok {
				if _, ok := frame.Frame.Frame.(h3qlog.GoAwayFrame); ok {
					signal.Do(func() { close(goaway) })
				}
			}
		}}
	}}
	var server *http3.Server
	client, _ := newAltMemoryFixtureOptions(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "x")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}), true, config, nil, func(value *http3.Server) { server = value })
	response, err := client.Get("https://" + testAltApiHost + "/goaway")
	if err != nil {
		t.Fatal(err)
	}
	transport := client.Transport.(*altQuicBoundedTransport)
	conn := transport.connection(testAltApiHost + ":443")
	shutdown := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		shutdown <- server.Shutdown(ctx)
	}()
	select {
	case <-goaway:
	case <-time.After(2 * time.Second):
		t.Fatal("server never sent GOAWAY")
	}
	response.Body.Close()
	waitAltMemorySlot(t, client)
	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("GOAWAY left shutdown goroutines blocked")
	}
	if conn.Context().Err() == nil {
		t.Fatal("GOAWAY connection remained live")
	}
	// Sweep an expired entry without dialling a server which has shut down.
	client.CloseIdleConnections()
	if transport.connection(testAltApiHost+":443") != nil {
		t.Fatal("GOAWAY retained a pooled connection")
	}
}

func TestAltMemoryCloseCancelsPendingDial(t *testing.T) {
	started := make(chan struct{})
	transport := newAltQuicBoundedTransport(&http3.Transport{Dial: func(ctx context.Context, _ string, _ *tls.Config, _ *quic.Config) (*quic.Conn, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	client := &http.Client{Transport: transport}
	result := make(chan error, 1)
	go func() { _, err := client.Get("https://api.example.invalid/pending"); result <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("dial did not begin")
	}
	transport.Close()
	select {
	case err := <-result:
		if !errors.Is(err, http3.ErrTransportClosed) {
			t.Fatalf("pending dial close=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close left the pending dial blocked")
	}
	waitAltMemorySlot(t, client)
}

func TestAltMemorySmallResponseAllocationSample(t *testing.T) {
	// Alt previously had no packet tracer. Unlike the already-traced platform
	// path, its new tracing cost must be measured against a native transport.
	for _, bounded := range []bool{false, true} {
		t.Run(fmt.Sprintf("bounded=%v", bounded), func(t *testing.T) {
			client, hellos := newAltMemoryFixtureMode(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }), bounded)
			url := "https://" + testAltApiHost + "/small"
			request := func() {
				response, err := client.Get(url)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := io.Copy(io.Discard, response.Body); err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if bounded {
					waitAltMemorySlot(t, client)
				}
			}
			request()
			started := time.Now()
			allocations := testing.AllocsPerRun(20, request)
			if allocations > 2000 {
				t.Fatalf("small alt response allocated %.0f objects", allocations)
			}
			if hellos.Load() != 1 {
				t.Fatalf("small requests lost pooling: %d connections", hellos.Load())
			}
			t.Logf("small alt GET bounded=%v: %.0f allocations/request, elapsed for 21 requests=%s", bounded, allocations, time.Since(started))
		})
	}
}

func TestAltMemoryRejectsOversizedResponseHeaders(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprintf("compressed=%v", compressed), func(t *testing.T) {
			value := strings.Repeat("a", altMaxHeaderBytes+512)
			if !compressed {
				value = strings.Repeat("X~Z|Q^", altMaxHeaderBytes)
			}
			var encoded bytes.Buffer
			if err := qpack.NewEncoder(&encoded).WriteField(qpack.HeaderField{Name: "x-large", Value: value}); err != nil {
				t.Fatal(err)
			}
			if (encoded.Len() < altMaxHeaderBytes) != compressed {
				t.Fatalf("fixture encoded=%d decoded=%d", encoded.Len(), len(value))
			}
			client, _ := newAltMemoryFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Large", value)
				fmt.Fprint(w, "ok")
			}))
			response, err := client.Get("https://" + testAltApiHost + "/headers")
			if err == nil {
				response.Body.Close()
				t.Fatal("oversized response headers escaped the bound")
			}
			waitAltMemorySlot(t, client)
			if !strings.Contains(err.Error(), "header") {
				t.Fatalf("not a response-header refusal: %v", err)
			}
		})
	}
}
