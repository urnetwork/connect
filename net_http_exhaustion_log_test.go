package connect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"
)

// The log form of a failed request (connect#229). An exhausted request names
// each cause its attempts retained, once with its count, after its own fixed
// message; the request url of an attempt, which may carry an env secret in
// its path, never reaches the line, even from a url error the exhaustion keeps
// whole for want of a cause. Any other error is its own message.
func TestHttpRequestErrorWithCauses(t *testing.T) {
	const secretPath = "test-env-secret"
	requestUrl := fmt.Sprintf("https://api-v4.space.example/%s/network/extender-activate", secretPath)
	tlsCause := errors.New("test certificate is not trusted")

	causes := newHttpRequestCauses(context.Background())
	for range 3 {
		causes.record(&url.Error{
			Op:  "Post",
			URL: requestUrl,
			Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
		})
	}
	causes.record(&url.Error{Op: "Post", URL: requestUrl, Err: tlsCause})
	causes.record(&url.Error{Op: "Post", URL: requestUrl, Err: tlsCause})
	// a url error with no cause is kept whole
	causes.record(&url.Error{Op: "Get", URL: requestUrl})
	exhausted := causes.exhausted(context.Background(), nil)

	message := httpRequestErrorWithCauses(exhausted)
	t.Logf("exhausted request: %s", message)
	if !strings.HasPrefix(message, exhausted.Error()+": ") {
		t.Errorf("%q does not start with the exhausted message", message)
	}
	for _, expected := range []string{
		syscall.ECONNREFUSED.Error(),
		tlsCause.Error() + " (x2)",
		"Get: ",
		context.DeadlineExceeded.Error(),
	} {
		if !strings.Contains(message, expected) {
			t.Errorf("%q does not name %q", message, expected)
		}
	}
	if strings.Count(message, tlsCause.Error()) != 1 {
		t.Errorf("%q names a repeated cause more than once", message)
	}
	if strings.Contains(message, secretPath) || strings.Contains(message, "api-v4.space.example") {
		t.Errorf("%q carries the request url", message)
	}

	// wrapped, the exhaustion is still found and its causes still named
	wrapped := fmt.Errorf("activate: %w", exhausted)
	if wrappedMessage := httpRequestErrorWithCauses(wrapped); wrappedMessage != "activate: "+message {
		t.Errorf("wrapped = %q, expected %q", wrappedMessage, "activate: "+message)
	}

	// an exhaustion with no cause, any other error, and no error
	for _, c := range []struct {
		err     error
		message string
	}{
		{err: &HttpRequestExhaustedError{}, message: "http request attempts exhausted"},
		{err: tlsCause, message: tlsCause.Error()},
		{err: nil, message: ""},
	} {
		if message := httpRequestErrorWithCauses(c.err); message != c.message {
			t.Errorf("httpRequestErrorWithCauses(%v) = %q, expected %q", c.err, message, c.message)
		}
	}
}
