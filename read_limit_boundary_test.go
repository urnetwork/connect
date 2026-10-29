package connect

// Signed-limit boundaries must preserve payloads, finite-cap rejection, and
// caller-owned reader tails without retaining partial pooled messages.
import (
	"bytes"
	"errors"
	"io"
	"math"
	"net/http"
	"testing"
	"testing/iotest"
)

// Returns a terminal read error after any preceding reader has been consumed.
type readLimitBoundaryErrorReader struct {
	err error
}

// Produces the injected error without consuming or retaining the caller's bytes.
func (self *readLimitBoundaryErrorReader) Read([]byte) (int, error) {
	return 0, self.err
}

// Exercises the actual bounded pooled reader with short reads at both largest
// signed limits; neither limit needs an unbounded reader or a large allocation.
func TestMessagePoolReadAllLimitLargestSignedLimit(t *testing.T) {
	expected := bytes.Repeat([]byte{0x23, 0x7a, 0x10}, 47)
	for _, limit := range []int64{math.MaxInt64 - 1, math.MaxInt64} {
		func() {
			message, err := MessagePoolReadAllLimit(iotest.OneByteReader(bytes.NewReader(expected)), limit)
			if message != nil {
				defer MessagePoolReturn(message)
			}
			if err != nil || !bytes.Equal(message, expected) {
				t.Fatalf("MESSAGE_POOL_MAX_LIMIT_DROPPED_PAYLOAD: limit=%d got=%d want=%d err=%v", limit, len(message), len(expected), err)
			}
		}()
	}
}

// A finite cap reads at most its one-byte lookahead and leaves the remainder
// with its original reader. Exact and zero limits retain their old meaning.
func TestMessagePoolReadAllLimitFiniteCapsPreserveTail(t *testing.T) {
	for _, c := range []struct {
		limit    int64
		size     int
		tooLarge bool
	}{
		{limit: 0, size: 0, tooLarge: false},
		{limit: 0, size: 19, tooLarge: true},
		{limit: 31, size: 31, tooLarge: false},
		{limit: 31, size: 97, tooLarge: true},
	} {
		func() {
			expected := bytes.Repeat([]byte{0x71}, c.size)
			reader := bytes.NewReader(expected)
			message, err := MessagePoolReadAllLimit(reader, c.limit)
			if message != nil {
				defer MessagePoolReturn(message)
			}
			if c.tooLarge {
				if !errors.Is(err, ErrMessageTooLarge) || message != nil || reader.Len() != c.size-int(c.limit)-1 {
					t.Fatalf("limit=%d size=%d message=%d remaining=%d err=%v", c.limit, c.size, len(message), reader.Len(), err)
				}
			} else if err != nil || !bytes.Equal(message, expected) || reader.Len() != 0 {
				t.Fatalf("exact limit=%d size=%d message=%d remaining=%d err=%v", c.limit, c.size, len(message), reader.Len(), err)
			}
		}()
	}
}

// Errors return no owned buffer, and a negative cap must not consume input.
func TestMessagePoolReadAllLimitErrorAndNegativeRelease(t *testing.T) {
	beforeCount, beforeBytes := messagePoolOutstandingSnapshot()
	readErr := errors.New("synthetic bounded read failure")
	prefix := bytes.NewReader(bytes.Repeat([]byte{0x49}, 37))
	message, err := MessagePoolReadAllLimit(io.MultiReader(prefix, &readLimitBoundaryErrorReader{err: readErr}), math.MaxInt64)
	if message != nil {
		MessagePoolReturn(message)
		t.Fatal("a failed bounded read returned an owned message")
	}
	if !errors.Is(err, readErr) || prefix.Len() != 0 {
		t.Fatalf("read failure was lost: remaining=%d err=%v", prefix.Len(), err)
	}
	untouched := bytes.NewReader([]byte{1, 2, 3})
	message, err = MessagePoolReadAllLimit(untouched, -1)
	if message != nil {
		MessagePoolReturn(message)
		t.Fatal("a negative bounded read returned an owned message")
	}
	if err == nil || untouched.Len() != 3 {
		t.Fatalf("negative cap consumed input or succeeded: remaining=%d err=%v", untouched.Len(), err)
	}
	if count, byteCount := messagePoolOutstandingSnapshot(); count != beforeCount || byteCount != beforeBytes {
		t.Fatalf("partial pooled read retained ownership: before=%d/%d after=%d/%d", beforeCount, beforeBytes, count, byteCount)
	}
}

// The same lookahead arithmetic also applies to explicit large HTTP body caps,
// with either a known content length or an unknown/chunked length.
func TestHttpResponseBodyLargestSignedLimit(t *testing.T) {
	expected := bytes.Repeat([]byte{0x41, 0x62, 0x03}, 17)
	for _, limit := range []int64{math.MaxInt64 - 1, math.MaxInt64} {
		for _, length := range []int64{-1, int64(len(expected))} {
			body := io.NopCloser(iotest.OneByteReader(bytes.NewReader(expected)))
			response := &http.Response{Body: body, ContentLength: length}
			message, err := readHttpResponseBody(response, limit)
			body.Close()
			if err != nil || !bytes.Equal(message, expected) {
				t.Fatalf("HTTP_MAX_LIMIT_DROPPED_PAYLOAD: limit=%d content_length=%d got=%d want=%d err=%v", limit, length, len(message), len(expected), err)
			}
		}
	}
}
