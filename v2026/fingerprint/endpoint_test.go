package fingerprint

// endpoint_test.go -- the shared endpoint captures a real on-wire Chrome hello
// the same way a golden is captured, the diff passes it against the correct
// golden and fails it against a deliberately-stale one (the engine
// discriminates end to end without connect's dialer), and the endpoint offers
// the post-quantum exchange a client needs to emit its pq key share.

import (
	"context"
	"crypto/tls"
	"net"
	"slices"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// captureChromeHelloAtEndpoint starts the shared endpoint, completes one real
// uTLS Chrome 133 handshake against it, and returns the captured first flight
// parsed, with its record count. this is the impl-independent stand-in for the
// Docker-Chrome capture of Layer B: uTLS HelloChrome_133 is the profile connect
// parrots and the synthetic golden is made from.
func captureChromeHelloAtEndpoint(t *testing.T) *ClientHelloFingerprint {
	t.Helper()
	endpoint, err := NewEndpoint(EndpointOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(endpoint.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tcpConn, err := (&net.Dialer{}).DialContext(ctx, "tcp4", endpoint.Addr())
	if err != nil {
		t.Fatal(err)
	}
	uconn := utls.UClient(tcpConn, &utls.Config{ServerName: ServerName, RootCAs: endpoint.CaCertPool()}, utls.HelloChrome_133)
	if err := uconn.HandshakeContext(ctx); err != nil {
		uconn.Close()
		t.Fatalf("chrome handshake against the endpoint: %s", err)
	}
	// the server read the client hello before it answered, so the capture is
	// recorded (under the endpoint's lock) before this handshake returned.
	uconn.Close()

	captures := endpoint.CapturedClientHellos()
	if len(captures) != 1 {
		t.Fatalf("captured %d client hellos, want 1", len(captures))
	}
	if captures[0].RecordCount != 1 {
		t.Fatalf("the chrome hello arrived in %d records, want 1", captures[0].RecordCount)
	}
	fingerprint, err := ParseClientHello(captures[0].Message)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint.RecordCount = captures[0].RecordCount
	return fingerprint
}

// A real Chrome hello captured at the endpoint diffs clean against the golden,
// records and all: the pass side of the drift gate, proven without connect.
func TestEndpointCapturesChromeHelloMatchingGolden(t *testing.T) {
	golden := loadSyntheticGolden(t)
	got := captureChromeHelloAtEndpoint(t)
	opts := chromeNavigationOptions()
	opts.ExpectRecordCount = 1
	if drifts := Diff(golden, got, opts); len(drifts) != 0 {
		t.Fatalf("a captured chrome hello drifts from the golden: %s", FormatDrift(GoldenChrome133Synthetic, drifts))
	}
}

// The same captured hello drifts from a deliberately-stale golden (its
// post-quantum key share removed): the fail side of the drift gate. together
// with the test above this is the task's "the diff must FAIL on the stale one
// and PASS on the correct one", end to end at the endpoint.
func TestEndpointCaptureDriftsFromStaleGolden(t *testing.T) {
	correct := loadSyntheticGolden(t)
	stale := cloneFingerprint(correct)
	stale.SupportedGroups = slices.DeleteFunc(stale.SupportedGroups, func(group uint16) bool { return group == groupX25519Mlkem768 })
	stale.KeyShareGroups = slices.DeleteFunc(stale.KeyShareGroups, func(group uint16) bool { return group == groupX25519Mlkem768 })

	got := captureChromeHelloAtEndpoint(t)
	opts := chromeNavigationOptions()
	opts.ExpectRecordCount = 1
	if drifts := Diff(stale, got, opts); len(drifts) == 0 {
		t.Fatal("a captured chrome hello did not drift from a stale golden missing the pq key share")
	}
	if drifts := Diff(correct, got, opts); len(drifts) != 0 {
		t.Fatalf("the same hello drifted from the correct golden: %s", FormatDrift(GoldenChrome133Synthetic, drifts))
	}
}

// The endpoint offers the post-quantum exchange: a client that shares a key for
// X25519MLKEM768 alone completes in one round trip, which it could not if the
// endpoint did not negotiate that group (it would ask for a hello retry the
// client cannot answer).
func TestEndpointOffersX25519Mlkem768(t *testing.T) {
	endpoint, err := NewEndpoint(EndpointOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(endpoint.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tcpConn, err := (&net.Dialer{}).DialContext(ctx, "tcp4", endpoint.Addr())
	if err != nil {
		t.Fatal(err)
	}
	conn := tls.Client(tcpConn, &tls.Config{
		ServerName:       ServerName,
		RootCAs:          endpoint.CaCertPool(),
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: []tls.CurveID{tls.X25519MLKEM768},
	})
	if err := conn.HandshakeContext(ctx); err != nil {
		conn.Close()
		t.Fatalf("handshake restricted to X25519MLKEM768: %s", err)
	}
	defer conn.Close()
	if version := conn.ConnectionState().Version; version != tls.VersionTLS13 {
		t.Fatalf("negotiated tls %04x, want tls 1.3", version)
	}
}
