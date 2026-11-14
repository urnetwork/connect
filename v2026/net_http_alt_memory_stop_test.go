package connect

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/quic-go/qlog"
	"github.com/quic-go/quic-go/qlogwriter"
)

// The first byte has been written, but the uploader cannot finish or write its
// second byte until the test has observed a real peer STOP_SENDING.
type altPeerStoppedUploadBody struct {
	first, second            bool
	writeAfterStop           bool
	finalErr                 error
	waiting, release, closed chan struct{}
	waitOnce, closeOnce      sync.Once
}

func (b *altPeerStoppedUploadBody) Read(p []byte) (int, error) {
	if !b.first {
		b.first = true
		p[0] = 'x'
		return 1, nil
	}
	b.waitOnce.Do(func() { close(b.waiting) })
	select {
	case <-b.release:
	case <-b.closed:
		return 0, context.Canceled
	}
	if b.writeAfterStop && !b.second {
		b.second = true
		p[0] = 'y'
		return 1, nil
	}
	if b.finalErr != nil {
		return 0, b.finalErr
	}
	return 0, io.EOF
}

func (b *altPeerStoppedUploadBody) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func waitAltStopEvent(t *testing.T, event <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-event:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func altActiveRequestWriter(t *testing.T, client *http.Client) *quicSendFlightWriter {
	t.Helper()
	transport := client.Transport.(*altQuicBoundedTransport)
	conn := transport.connection(testAltApiHost + ":443")
	if conn == nil {
		t.Fatal("request has no pooled connection")
	}
	flight := quicSendFlightForConn(conn)
	flight.mutex.Lock()
	defer flight.mutex.Unlock()
	for _, writer := range flight.writers {
		if writer != nil {
			return writer
		}
	}
	t.Fatal("request has no registered writer")
	return nil
}

func newAltStoppedResponseFixture(t *testing.T, body *altPeerStoppedUploadBody, stop bool) (*http.Client, *atomic.Int32) {
	t.Helper()
	return newAltMemoryFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/next" {
			io.WriteString(w, "next")
			return
		}
		select {
		case <-body.waiting:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Length", "8")
		w.WriteHeader(http.StatusOK)
		stream := w.(http3.HTTPStreamer).HTTPStream()
		defer stream.Close()
		if stop {
			stream.CancelRead(quic.StreamErrorCode(http3.ErrCodeNoError))
		}
		select {
		case <-body.closed:
			stream.Write([]byte("survived"))
		case <-r.Context().Done():
		}
	}))
}

// A valid final response may stop the request upload. Hold its DATA until the
// upload worker has reacted, so cancelling the response on that send-side stop
// cannot hide behind a lucky response/upload scheduling order.
func TestAltMemoryPeerStoppedUploadPreservesResponseAndPool(t *testing.T) {
	for _, writeAfterStop := range []bool{false, true} {
		name := "close"
		if writeAfterStop {
			name = "write"
		}
		t.Run(name, func(t *testing.T) {
			body := &altPeerStoppedUploadBody{
				writeAfterStop: writeAfterStop,
				waiting:        make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{}),
			}
			release := sync.OnceFunc(func() { close(body.release) })
			defer release()
			client, hellos := newAltStoppedResponseFixture(t, body, true)
			request, _ := http.NewRequestWithContext(t.Context(), http.MethodPatch, "https://"+testAltApiHost+"/early", body)
			request.ContentLength = 1
			if writeAfterStop {
				request.ContentLength = 2
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			writer := altActiveRequestWriter(t, client)
			waitAltStopEvent(t, writer.stream.Context().Done(), "peer upload stop")
			var streamErr *quic.StreamError
			if !errors.As(context.Cause(writer.stream.Context()), &streamErr) || !streamErr.Remote || streamErr.ErrorCode != quic.StreamErrorCode(http3.ErrCodeNoError) {
				t.Fatalf("upload was not stopped by peer H3_NO_ERROR: %v", context.Cause(writer.stream.Context()))
			}
			release()
			waitAltStopEvent(t, body.closed, "upload worker closing its body")
			payload, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || string(payload) != "survived" {
				t.Fatalf("peer upload stop destroyed the final response: body=%q err=%v", payload, err)
			}
			waitAltMemorySlot(t, client)
			if got := writer.flight.snapshot(); got.Frames != 0 || got.PendingFrames != 0 || got.Failed || got.Closed {
				t.Fatalf("peer-stopped request did not retire its retained ownership: %+v", got)
			}
			next, err := client.Get("https://" + testAltApiHost + "/next")
			if err != nil {
				t.Fatal(err)
			}
			payload, err = io.ReadAll(next.Body)
			next.Body.Close()
			if err != nil || strings.TrimSpace(string(payload)) != "next" {
				t.Fatalf("next request: body=%q err=%v", payload, err)
			}
			waitAltMemorySlot(t, client)
			if hellos.Load() != 1 {
				t.Fatalf("peer upload stop unnecessarily retired the pooled connection: %d", hellos.Load())
			}
		})
	}
}

func TestAltMemoryPeerStopDoesNotHideBodyFailure(t *testing.T) {
	for _, failure := range []string{"reader", "short", "overrun", "request-cancel"} {
		t.Run(failure, func(t *testing.T) {
			body := &altPeerStoppedUploadBody{waiting: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
			release := sync.OnceFunc(func() { close(body.release) })
			defer release()
			if failure == "reader" {
				body.finalErr = errors.New("genuine upload reader failure")
			}
			body.writeAfterStop = failure == "overrun"
			client, _ := newAltStoppedResponseFixture(t, body, true)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			request, _ := http.NewRequestWithContext(ctx, http.MethodPatch, "https://"+testAltApiHost+"/early", body)
			request.ContentLength = 1
			if failure == "short" {
				request.ContentLength = 2
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			writer := altActiveRequestWriter(t, client)
			waitAltStopEvent(t, writer.stream.Context().Done(), "peer upload stop")
			if failure == "request-cancel" {
				cancel()
			}
			release()
			waitAltStopEvent(t, body.closed, "failed upload closing its body")
			payload, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err == nil {
				t.Fatalf("peer stop hid %s failure: body=%q", failure, payload)
			}
			waitAltMemorySlot(t, client)
		})
	}
}

// Closing an early response intentionally stops its still-blocked upload, but
// is not a request-context cancellation or an independent body-reader error.
func TestAltMemoryResponseCloseStopsUploadAndReusesPool(t *testing.T) {
	body := &altPeerStoppedUploadBody{waiting: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	client, hellos := newAltStoppedResponseFixture(t, body, false)
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodPatch, "https://"+testAltApiHost+"/early", body)
	request.ContentLength = 1
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	writer := altActiveRequestWriter(t, client)
	conn := client.Transport.(*altQuicBoundedTransport).connection(testAltApiHost + ":443")
	response.Body.Close()
	waitAltStopEvent(t, body.closed, "response close stopping the upload reader")
	waitAltMemorySlot(t, client)
	if got := writer.flight.snapshot(); got.Closed || got.Failed || got.Frames != 0 || got.PendingFrames != 0 {
		t.Fatalf("response close retired a reusable connection: %+v request=%v send=%v connection=%v", got, request.Context().Err(), context.Cause(writer.stream.Context()), context.Cause(conn.Context()))
	}
	next, err := client.Get("https://" + testAltApiHost + "/next")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, next.Body)
	next.Body.Close()
	waitAltMemorySlot(t, client)
	if hellos.Load() != 1 {
		t.Fatalf("response close lost pooling: %d", hellos.Load())
	}
}

func TestAltMemoryPeerStoppedUploadHoldsAdmissionUntilResetAck(t *testing.T) {
	body := &altPeerStoppedUploadBody{waiting: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	release := sync.OnceFunc(func() { close(body.release) })
	defer release()
	var reverse *flightBlackholePacketConn
	resetReceived := make(chan struct{})
	var holdOnce sync.Once
	config := &quic.Config{Tracer: func(context.Context, bool, quic.ConnectionID) qlogwriter.Trace {
		return &altTestTrace{record: func(event qlogwriter.Event) {
			packet, ok := event.(qlog.PacketReceived)
			if !ok {
				return
			}
			for _, frame := range packet.Frames {
				if reset, ok := frame.Frame.(*qlog.ResetStreamFrame); ok && reset.StreamID == 0 {
					holdOnce.Do(func() {
						reverse.drop.Store(true) // before this received packet can be ACKed
						close(resetReceived)
					})
				}
			}
		}}
	}}
	client, hellos := newAltMemoryFixtureOptions(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/next" {
			io.WriteString(w, "next")
			return
		}
		select {
		case <-body.waiting:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Length", "2")
		stream := w.(http3.HTTPStreamer).HTTPStream()
		stream.Write([]byte("ok"))
		stream.Close()
		stream.CancelRead(quic.StreamErrorCode(http3.ErrCodeNoError))
	}), true, config, func(conn net.PacketConn) net.PacketConn {
		reverse = &flightBlackholePacketConn{PacketConn: conn}
		return reverse
	})
	defer reverse.drop.Store(false)
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodPatch, "https://"+testAltApiHost+"/early", body)
	request.ContentLength = 1
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	writer := altActiveRequestWriter(t, client)
	waitAltStopEvent(t, resetReceived, "peer receipt of the upload reset")
	release()
	waitAltStopEvent(t, body.closed, "upload worker exit")
	payload, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(payload) != "ok" {
		t.Fatalf("early response: body=%q err=%v", payload, err)
	}
	assertFlightFinished(t, writer.flight, writer.stream.StreamID(), false)
	transport := client.Transport.(*altQuicBoundedTransport)
	select {
	case transport.slot <- struct{}{}:
		<-transport.slot
		t.Fatal("request admission released before the reset ACK")
	default:
	}
	reverse.drop.Store(false)
	waitAltMemorySlot(t, client)
	next, err := client.Get("https://" + testAltApiHost + "/next")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, next.Body)
	next.Body.Close()
	waitAltMemorySlot(t, client)
	if hellos.Load() != 1 {
		t.Fatalf("reset ACK recovery retired the connection: %d", hellos.Load())
	}
}

func TestAltMemoryUploadStopClassification(t *testing.T) {
	peer := &quic.StreamError{StreamID: 0, Remote: true, ErrorCode: quic.StreamErrorCode(http3.ErrCodeNoError)}
	local := &quic.StreamError{StreamID: 0, ErrorCode: quic.StreamErrorCode(http3.ErrCodeRequestCanceled)}
	peerFailure := &quic.StreamError{StreamID: 0, Remote: true, ErrorCode: quic.StreamErrorCode(http3.ErrCodeRequestCanceled)}
	for _, test := range []struct {
		name                                  string
		cause, err                            error
		requestCanceled, responseClosed, want bool
	}{
		{name: "peer-write", cause: peer, err: peer, want: true},
		{name: "credit-wait", cause: peer, err: context.Canceled, want: true},
		{name: "budget", cause: peer, err: errQuicSendFlight},
		{name: "deadline", cause: peer, err: context.DeadlineExceeded},
		{name: "peer-failure", cause: peerFailure, err: peerFailure},
		{name: "local-cancel", cause: local, err: local},
		{name: "response-close", cause: local, err: local, responseClosed: true, want: true},
		{name: "client-timeout-context-after-close", cause: local, err: context.Canceled, requestCanceled: true, responseClosed: true, want: true},
		{name: "request-cancel", cause: peer, err: peer, requestCanceled: true},
		{name: "other-stream", cause: peer, err: &quic.StreamError{StreamID: 4, Remote: true, ErrorCode: quic.StreamErrorCode(http3.ErrCodeNoError)}},
		{name: "connection-close", cause: &quic.ApplicationError{ErrorCode: 0x100, Remote: true}, err: context.Canceled},
		{name: "no-peer-proof", cause: context.Canceled, err: peer},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.requestCanceled {
				cancel()
			}
			streamCtx, stop := context.WithCancelCause(t.Context())
			stop(test.cause)
			if got := altUploadSendStopped(ctx, streamCtx, test.err, test.responseClosed); got != test.want {
				t.Fatalf("normal send stop=%v, want %v", got, test.want)
			}
		})
	}
}
