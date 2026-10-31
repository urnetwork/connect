package extender

// Explicit extender request limits exercise the constructed proxy's real body
// admission helper, including caller-owned tails, errors, and disabled caps.
import (
	"bytes"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/iotest"
)

// Constructs the actual settings owner before any request is admitted.
func newReadLimitBoundaryExtender(t *testing.T, limit int64) *extenderProxy {
	t.Helper()
	settings := DefaultExtenderSettings()
	settings.ProxyMaxRequestByteCount = limit
	settings.SpoofDomains = []string{"proxy.example"}
	server := NewExtenderServer(t.Context(), nil, []string{"proxy.example"}, nil, nil, settings)
	t.Cleanup(func() { server.CloseAndWait() })
	if server.settings.ProxyMaxRequestByteCount != limit {
		t.Fatal("the constructor did not retain the actual request limit")
	}
	return server.proxy
}

// The old positive MaxInt64+1 branch returns an empty reader with nil error.
// Both known-length and chunked requests must preserve their actual body.
func TestExtenderProxyLargestRequestLimitPreservesBody(t *testing.T) {
	expected := bytes.Repeat([]byte{0x29, 0x57, 0x11}, 19)
	for _, limit := range []int64{math.MaxInt64 - 1, math.MaxInt64} {
		proxy := newReadLimitBoundaryExtender(t, limit)
		for _, length := range []int64{-1, int64(len(expected))} {
			request := httptest.NewRequest(http.MethodPost, "https://proxy.example/upload", iotest.OneByteReader(bytes.NewReader(expected)))
			request.ContentLength = length
			reader, err := proxy.readRequestBody(request)
			request.Body.Close()
			if err != nil || reader == nil {
				t.Fatalf("largest request limit refused its body: limit=%d length=%d err=%v", limit, length, err)
			}
			body, err := io.ReadAll(reader)
			if err != nil || !bytes.Equal(body, expected) {
				t.Fatalf("EXTENDER_PROXY_MAX_LIMIT_DROPPED_BODY: limit=%d content_length=%d got=%d want=%d err=%v", limit, length, len(body), len(expected), err)
			}
		}
	}
}

// A real positive finite bound retains its pre-read known-length refusal and
// one-byte chunked lookahead; no larger tail or partial body is admitted.
func TestExtenderProxyFiniteRequestLimitPreservesTail(t *testing.T) {
	const limit = 31
	proxy := newReadLimitBoundaryExtender(t, limit)
	for _, c := range []struct {
		size          int
		contentLength int64
		remaining     int
		refused       bool
	}{
		{size: limit, contentLength: limit, remaining: 0, refused: false},
		{size: 97, contentLength: 97, remaining: 97, refused: true},
		{size: 97, contentLength: -1, remaining: 97 - limit - 1, refused: true},
	} {
		expected := bytes.Repeat([]byte{0x63}, c.size)
		source := bytes.NewReader(expected)
		request := httptest.NewRequest(http.MethodPost, "https://proxy.example/upload", source)
		request.ContentLength = c.contentLength
		reader, err := proxy.readRequestBody(request)
		request.Body.Close()
		if source.Len() != c.remaining {
			t.Fatalf("size=%d length=%d consumed the wrong reader tail: remaining=%d want=%d", c.size, c.contentLength, source.Len(), c.remaining)
		}
		if c.refused {
			if err == nil || reader != nil {
				t.Fatalf("size=%d length=%d admitted an oversized request: reader=%T err=%v", c.size, c.contentLength, reader, err)
			}
		} else {
			if err != nil || reader == nil {
				t.Fatalf("exact request was refused: err=%v", err)
			}
			body, err := io.ReadAll(reader)
			if err != nil || !bytes.Equal(body, expected) {
				t.Fatalf("exact request changed: size=%d got=%d err=%v", c.size, len(body), err)
			}
		}
	}
}

// Zero and negative values keep their documented disabled-cap behavior;
// absent bodies remain absent rather than becoming synthetic empty readers.
func TestExtenderProxyDisabledRequestLimitPreservesBody(t *testing.T) {
	expected := bytes.Repeat([]byte{0x39}, 97)
	for _, limit := range []int64{0, -1} {
		proxy := newReadLimitBoundaryExtender(t, limit)
		request := httptest.NewRequest(http.MethodPost, "https://proxy.example/upload", bytes.NewReader(expected))
		reader, err := proxy.readRequestBody(request)
		request.Body.Close()
		if err != nil || reader == nil {
			t.Fatalf("disabled cap=%d refused a body: %v", limit, err)
		}
		body, err := io.ReadAll(reader)
		if err != nil || !bytes.Equal(body, expected) {
			t.Fatalf("disabled cap=%d changed the body: got=%d err=%v", limit, len(body), err)
		}
		for _, absent := range []io.ReadCloser{nil, http.NoBody} {
			request.Body = absent
			reader, err := proxy.readRequestBody(request)
			if err != nil || reader != nil {
				t.Fatalf("disabled cap=%d changed an absent body: reader=%T err=%v", limit, reader, err)
			}
		}
	}
}

// The caller retains the request body, including on a read failure.
type extenderReadLimitBoundaryBody struct {
	io.Reader
	closeCount int
}

// Counts the caller's explicit retirement without changing read behavior.
func (self *extenderReadLimitBoundaryBody) Close() error {
	self.closeCount += 1
	return nil
}

// Produces a terminal error after the actual prefix is consumed.
type extenderReadLimitBoundaryError struct {
	err error
}

// Returns the configured failure without retaining the borrowed read storage.
func (self *extenderReadLimitBoundaryError) Read([]byte) (int, error) {
	return 0, self.err
}

// A partial failing request produces no admitted reader and preserves the
// exact failure; neither success nor refusal takes the caller's body lifetime.
func TestExtenderProxyRequestReadErrorRetainsCallerOwnership(t *testing.T) {
	proxy := newReadLimitBoundaryExtender(t, math.MaxInt64)
	readErr := errors.New("synthetic extender request read failure")
	prefix := bytes.NewReader(bytes.Repeat([]byte{0x57}, 37))
	body := &extenderReadLimitBoundaryBody{Reader: io.MultiReader(prefix, &extenderReadLimitBoundaryError{err: readErr})}
	defer func() {
		if body.closeCount == 0 {
			body.Close()
		}
	}()
	request := httptest.NewRequest(http.MethodPost, "https://proxy.example/upload", nil)
	request.Body = body
	request.ContentLength = -1
	reader, err := proxy.readRequestBody(request)
	if reader != nil || !errors.Is(err, readErr) || prefix.Len() != 0 || body.closeCount != 0 {
		t.Fatalf("failed request lost its source ownership or failure: reader=%T remaining=%d closed=%d err=%v", reader, prefix.Len(), body.closeCount, err)
	}
	request.Body.Close()
	if body.closeCount != 1 {
		t.Fatalf("request source retired %d times, want 1", body.closeCount)
	}
}
