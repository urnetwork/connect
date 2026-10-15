// Original DNS children cross the actual HTTP and H1 owners without requiring
// duplicate transport flags on their enclosing standard-library wrapper.
package connect

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/gorilla/websocket"
)

// Both request owners retain the actual leaf, rather than manufacturing a
// hard DNS wrapper merely because its optional transport flags are unset.
func TestHttpRequestExhaustionPublicFollowsUnflaggedDnsChild(t *testing.T) {
	for _, serial := range []bool{false, true} {
		for _, end := range []error{context.DeadlineExceeded, context.Canceled} {
			for _, child := range []error{context.DeadlineExceeded, syscall.ECONNRESET, io.EOF, context.Canceled} {
				dns := &net.DNSError{Err: "synthetic wrapped resolver failure", Name: "child.example", UnwrapErr: child}
				err := runHttpExhaustionTest(t, serial, end, dns)
				exhausted, ok := err.(*HttpRequestExhaustedError)
				if !ok {
					t.Fatalf("public DNS exhaustion lost its owner: %T", err)
				}
				foundChild, foundEnd := false, false
				for _, cause := range exhausted.causes {
					foundChild = foundChild || cause == child
					foundEnd = foundEnd || cause == end
					if cause == dns || cause == errHttpExhaustionCauseIncomplete || cause == errHttpExhaustionCauseTraversal {
						t.Fatalf("serial=%t absent DNS flags replaced a complete child", serial)
					}
				}
				retained := flattenHttpRequestCauses(dns)
				if !foundChild || !foundEnd || len(retained) != 1 || retained[0].err != child || retained[0].kind == 0 {
					t.Fatalf("serial=%t original DNS child or caller stop was lost", serial)
				}
			}
		}
	}
}

// Following a DNS child still applies custody, absent-receiver and traversal
// bounds before an actual exhausted request can publish its retained causes.
func TestHttpRequestExhaustionPublicRetainsUnflaggedDnsHardChild(t *testing.T) {
	for _, serial := range []bool{false, true} {
		hard := &os.PathError{Op: "read", Path: "synthetic-dns-child-custody", Err: io.EOF}
		cycle := &httpIncompleteCycleRootTestError{}
		for _, fixture := range []struct {
			child    error
			expected error
		}{
			{child: errors.Join(syscall.ECONNRESET, hard), expected: hard},
			{child: (*net.DNSError)(nil), expected: errHttpExhaustionCauseIncomplete},
			{child: cycle, expected: errHttpExhaustionCauseTraversal},
		} {
			dns := &net.DNSError{Err: "synthetic resolver original child", Name: "hard-child.example", UnwrapErr: fixture.child}
			err := runHttpExhaustionTest(t, serial, context.Canceled, dns)
			exhausted, ok := err.(*HttpRequestExhaustedError)
			if !ok {
				t.Fatalf("public DNS exhaustion lost its owner: %T", err)
			}
			foundChild, foundCancel := false, false
			for _, cause := range exhausted.causes {
				foundChild = foundChild || cause == fixture.expected
				foundCancel = foundCancel || cause == context.Canceled
			}
			if !foundChild || !foundCancel || cycle.maximum.Load() > httpRequestCauseDepth {
				t.Fatalf("serial=%t DNS child lost its original hard or finite-bound refusal", serial)
			}
		}
	}
}

// A real fresh WebSocket negotiation succeeds after a custom response read
// observes an unflagged DNS transport child. Neither attempt reuses the socket.
func TestDialH1MessagesUnflaggedDnsChildRecovers(t *testing.T) {
	resetH1UpgradeTestState(t)
	for _, child := range []error{context.DeadlineExceeded, syscall.ECONNRESET} {
		func() {
			finished := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				upgrader := websocket.Upgrader{}
				connection, err := upgrader.Upgrade(writer, request, nil)
				if err != nil {
					finished <- err
					return
				}
				defer connection.Close()
				finished <- connection.WriteMessage(websocket.BinaryMessage, []byte("synthetic recovered DNS child"))
			}))
			defer server.Close()
			dns := &net.DNSError{Err: "synthetic response transport child", Name: "recover.example", UnwrapErr: child}
			first := &httpUpgradeReadFailureTestConn{h1UpgradeScriptConn: newH1UpgradeScriptConn(nil), failure: dns}
			dials := 0
			dialer := &websocket.Dialer{NetDialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				dials++
				if dials == 1 {
					return first, nil
				}
				return (&net.Dialer{}).DialContext(ctx, network, address)
			}}
			address := "ws" + strings.TrimPrefix(server.URL, "http")
			result, err := DialH1Messages(t.Context(), address, nil, dialer, H1FramerProtocol, 1024, true, nil)
			if result == nil || err != nil {
				if result != nil {
					result.Close()
				}
				t.Fatal("original transport child did not allow a fresh successful negotiation", err)
			}
			defer result.Close()
			kind, raw, err := result.ReadMessage()
			if err != nil || kind != websocket.BinaryMessage || string(raw) != "synthetic recovered DNS child" || dials != 2 || !first.closed.Load() {
				t.Fatal("fresh negotiation did not retain socket ownership or message framing", err, dials)
			}
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-t.Context().Done():
				t.Fatal(t.Context().Err())
			}
			if !FramedUpgradePermitted(address, H1FramerProtocol) {
				t.Fatal("transport child became a cached protocol refusal")
			}
		}()
	}
}

// An actual custom response cannot gain a second negotiation from not-found,
// a mixed custody child, cancellation or an absent child receiver.
func TestDialH1MessagesDnsChildRefusalKeepsHardPriority(t *testing.T) {
	resetH1UpgradeTestState(t)
	hard := &os.PathError{Op: "read", Path: "synthetic-dns-upgrade-custody", Err: io.EOF}
	for _, dns := range []*net.DNSError{
		{Err: "synthetic absent name", Name: "absent-child.example", IsNotFound: true, UnwrapErr: syscall.ECONNRESET},
		{Err: "synthetic mixed resolver cause", Name: "mixed-child.example", UnwrapErr: errors.Join(syscall.ECONNRESET, hard)},
		{Err: "synthetic canceled resolver", Name: "cancel-child.example", UnwrapErr: context.Canceled},
		{Err: "synthetic missing resolver child", Name: "nil-child.example", UnwrapErr: (*net.DNSError)(nil)},
	} {
		connection := &httpUpgradeReadFailureTestConn{h1UpgradeScriptConn: newH1UpgradeScriptConn(nil), failure: dns}
		dials := 0
		dialer := &websocket.Dialer{NetDialContext: func(context.Context, string, string) (net.Conn, error) {
			dials++
			return connection, nil
		}}
		result, err := DialH1Messages(t.Context(), "ws://hard-dns-child.example/", nil, dialer, H1FramerProtocol, 1024, true, nil)
		if result != nil {
			result.Close()
			t.Fatal("refused original DNS graph returned a connection")
		}
		if err == nil || dials != 1 || !connection.closed.Load() || HTTPUpgradeAllowsFallback(err) {
			t.Fatal("hard original DNS graph authorized another negotiation", dials)
		}
	}
}
