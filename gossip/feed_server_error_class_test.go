// What the feed server logs of a request it could not read (log_address_test.go has the
// rule on a real carrier).

package gossip

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
)

// What the feed server says of each kind of failed read is one fixed text, which holds
// nothing of the error's own.
func TestGossipFeedRequestErrorClass(t *testing.T) {
	client := &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 44444}
	server := &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 443}
	opErr := func(err error) error {
		return &net.OpError{Op: "read", Net: "tcp", Source: server, Addr: client, Err: err}
	}
	for _, c := range []struct {
		err    error
		expect string
	}{
		{err: opErr(os.ErrDeadlineExceeded), expect: "the request did not arrive in time"},
		{err: fmt.Errorf("tls: %w", opErr(os.ErrDeadlineExceeded)), expect: "the request did not arrive in time"},
		{err: opErr(net.ErrClosed), expect: "the stream was closed"},
		{err: io.EOF, expect: "the stream ended before the request"},
		{err: io.ErrUnexpectedEOF, expect: "the stream ended before the request"},
		{err: opErr(errors.New("connection reset by peer")), expect: "the request could not be read"},
		{err: fmt.Errorf("from %s: the request is malformed", client), expect: "the request could not be read"},
	} {
		if class := feedRequestErrorClass(c.err); class != c.expect {
			t.Errorf("%v: class %q, expected %q", c.err, class, c.expect)
		} else if strings.Contains(class, "203.0.113.7") || strings.Contains(class, "44444") {
			t.Errorf("%v: class %q names the client", c.err, class)
		}
	}
}
