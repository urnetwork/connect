package connect

// Root-cause tests for the bundled borrow list and the role-start verification
// (EXTENDER.md P5). Deterministic and in-process: the verification dials an
// in-process TLS server through an injected dialer, so no name service or real
// network is touched, and synthetic .example names stand in for the borrow list.

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"testing/fstest"
	"time"
)

// A country borrow list round-trips through the resource codec and the per-
// country override, exactly as the spoof list (P5).
func TestBorrowDomainsForCountryRoundTrip(t *testing.T) {
	countryBorrowDomains := []string{"shop.example", "news.example"}
	resource, err := EncodeSpoofDomainsResource(countryBorrowDomains)
	if err != nil {
		t.Fatal(err)
	}
	restore := setBorrowCountryResourcesForTest(fstest.MapFS{
		BorrowResourcePath("zz"): &fstest.MapFile{Data: resource},
	})
	defer restore()

	got := BorrowDomainsForCountry("zz")
	if len(got) != len(countryBorrowDomains) {
		t.Fatalf("country borrow list = %v, expected %v", got, countryBorrowDomains)
	}
	for i, borrowDomain := range countryBorrowDomains {
		if got[i] != borrowDomain {
			t.Fatalf("country borrow list = %v, expected %v", got, countryBorrowDomains)
		}
	}
	// a country with no list falls back to the global list, which the bundled
	// resource leaves empty (no list bundled yet, P5)
	if fallback := BorrowDomainsForCountry("yy"); len(fallback) != len(BorrowDomains()) {
		t.Fatalf("missing country did not fall back to the global list: %v", fallback)
	}
}

// Runs an in-process TLS server with the given max version and returns a dialer
// that reaches it, so the verification is exercised without a real network.
func newBorrowVerifyServer(t *testing.T, maxVersion uint16) DialContextFunction {
	t.Helper()
	_, certificate := newTestTlsHelloCertificates(t, "candidate.example")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
		MaxVersion:   maxVersion,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				tlsConn := tls.Server(conn, tlsConfig)
				tlsConn.Handshake()
				tlsConn.Close()
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-done
	})
	address := listener.Addr().String()
	return func(ctx context.Context, network string, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}
}

// A reachable TLS 1.3 site that offers X25519 passes verification (P5).
func TestVerifyExtenderBorrowDomainAcceptsModernTls(t *testing.T) {
	dialContext := newBorrowVerifyServer(t, tls.VersionTLS13)
	if !VerifyExtenderBorrowDomain(context.Background(), dialContext, "tcp4", "candidate.example", 5*time.Second, nil) {
		t.Fatal("a modern tls 1.3 X25519 site failed verification")
	}
}

// A site that cannot reach TLS 1.3 is rejected (P5): the spliced handshake would
// not be as modern as the Chrome hello that fronted it.
func TestVerifyExtenderBorrowDomainRejectsOldTls(t *testing.T) {
	dialContext := newBorrowVerifyServer(t, tls.VersionTLS12)
	if VerifyExtenderBorrowDomain(context.Background(), dialContext, "tcp4", "candidate.example", 5*time.Second, nil) {
		t.Fatal("a tls 1.2-only site passed verification")
	}
}

// A borrowed name the extender cannot reach is rejected (P5), so an unreachable
// candidate is dropped rather than left to fail a splice later.
func TestVerifyExtenderBorrowDomainRejectsUnreachable(t *testing.T) {
	dialContext := func(ctx context.Context, network string, address string) (net.Conn, error) {
		return nil, net.ErrClosed
	}
	if VerifyExtenderBorrowDomain(context.Background(), dialContext, "tcp4", "candidate.example", 5*time.Second, nil) {
		t.Fatal("an unreachable candidate passed verification")
	}
}

// A peer the cdn check flags is rejected (P5): a big-site name pinned to an
// arbitrary address is the sni-to-ip tell the borrow list exists to avoid.
func TestVerifyExtenderBorrowDomainRejectsCdn(t *testing.T) {
	dialContext := newBorrowVerifyServer(t, tls.VersionTLS13)
	isCdn := func(leaf *tls.Certificate, serverName string) bool {
		return true
	}
	if VerifyExtenderBorrowDomain(context.Background(), dialContext, "tcp4", "candidate.example", 5*time.Second, isCdn) {
		t.Fatal("a cdn-flagged peer passed verification")
	}
}
