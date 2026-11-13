package connect

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Seed bootstrap tests (EXTENDER-UPDATE2.md §2.3, §2.4): local tls mirrors,
// recorded dials, a dialer that refuses every name (dns blocked), and the fake
// clock for freshness. No public site is contacted.

// The names the test certificate covers. `wrong.example` is not one of them.
var testSeedMirrorNames = []string{"seed-a.example", "seed-b.example", "seed-dns.example"}

// A self-signed certificate for the mirror names, and the pool that trusts it.
func newTestSeedMirrorCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: testSeedMirrorNames[0]},
		DNSNames:              testSeedMirrorNames,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// A local tls mirror serving `handler` under the test certificate.
func newTestSeedMirrorServer(t *testing.T, certificate tls.Certificate, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

// Serves one artifact.
func testSeedArtifactHandler(artifact []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(artifact)
	}
}

// Answers nothing until the request ends.
func testSeedStallHandler(started chan<- struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if started != nil {
			select {
			case started <- struct{}{}:
			default:
			}
		}
		<-r.Context().Done()
	}
}

// A listener that accepts and never speaks, which stalls the tls handshake.
func newTestSeedSilentListener(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var connsLock sync.Mutex
	conns := []net.Conn{}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connsLock.Lock()
			conns = append(conns, conn)
			connsLock.Unlock()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		connsLock.Lock()
		defer connsLock.Unlock()
		for _, conn := range conns {
			conn.Close()
		}
	})
	return listener.Addr().String()
}

// The recorded dials of a pass. A name is refused -- the dns bootstrap is
// blocked -- and an address is dialed on loopback. It counts the connections
// open at once.
type testSeedDialer struct {
	stateLock     sync.Mutex
	targets       []string
	openCount     int
	maxOpenCount  int
	refusedTarget []string
}

func (self *testSeedDialer) DialContext(ctx context.Context, network string, address string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	self.stateLock.Lock()
	self.targets = append(self.targets, address)
	self.stateLock.Unlock()
	if _, err := netip.ParseAddr(host); err != nil {
		self.stateLock.Lock()
		self.refusedTarget = append(self.refusedTarget, address)
		self.stateLock.Unlock()
		return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("name resolution is blocked")}
	}
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	self.stateLock.Lock()
	self.openCount += 1
	self.maxOpenCount = max(self.maxOpenCount, self.openCount)
	self.stateLock.Unlock()
	return &testSeedCountedConn{Conn: conn, dialer: self}, nil
}

func (self *testSeedDialer) dialed() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.targets)
}

func (self *testSeedDialer) maxOpen() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.maxOpenCount
}

type testSeedCountedConn struct {
	net.Conn
	dialer    *testSeedDialer
	closeOnce sync.Once
}

func (self *testSeedCountedConn) Close() error {
	self.closeOnce.Do(func() {
		self.dialer.stateLock.Lock()
		self.dialer.openCount -= 1
		self.dialer.stateLock.Unlock()
	})
	return self.Conn.Close()
}

// Everything one bootstrap test needs: the fake clock, a directory with the
// fixed root and no records, an acceptor, the certificate and the dialer.
type testSeedBootstrap struct {
	clock       *testClock
	directory   *ExtenderDirectory
	acceptor    *ExtenderSeedAcceptor
	certificate tls.Certificate
	pool        *x509.CertPool
	dialer      *testSeedDialer
}

func newTestSeedBootstrap(t *testing.T) *testSeedBootstrap {
	t.Helper()
	clock := newTestClock()
	certificate, pool := newTestSeedMirrorCertificate(t)
	return &testSeedBootstrap{
		clock:       clock,
		directory:   newTestSeedDirectory(t, clock, testSeedRootKey),
		acceptor:    NewExtenderSeedAcceptor(NewExtenderSeedMemoryStore(), nil),
		certificate: certificate,
		pool:        pool,
		dialer:      &testSeedDialer{},
	}
}

// Settings over the given mirrors with the test seams and short backoffs.
func (self *testSeedBootstrap) settings(mirrors ...*ExtenderSeedMirror) *ExtenderSeedBootstrapSettings {
	settings := DefaultExtenderSeedBootstrapSettings()
	settings.Mirrors = mirrors
	settings.DialContext = self.dialer.DialContext
	settings.RootCAs = self.pool
	settings.Now = self.clock.Now
	settings.MinBackoff = 5 * time.Millisecond
	settings.MaxBackoff = 20 * time.Millisecond
	return settings
}

// A valid binary artifact of two records at `sequence`.
func (self *testSeedBootstrap) artifact(t *testing.T, sequence uint64) []byte {
	t.Helper()
	now := self.clock.Now()
	envelope := testSeedEnvelope(t, testSeedRootKey, sequence, now, []*protocol.ExtenderRecord{
		testSeedRecord(t, testSeedRootKey, 1, now, "203.0.113.1"),
		testSeedRecord(t, testSeedRootKey, 2, now, "2001:db8::2"),
	}, nil)
	envelopeBytes, err := MarshalExtenderSeedEnvelope(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return envelopeBytes
}

func (self *testSeedBootstrap) run(t *testing.T, ctx context.Context, settings *ExtenderSeedBootstrapSettings) (*ExtenderSeedBootstrapResult, error, time.Duration) {
	t.Helper()
	startTime := time.Now()
	result, err := ExtenderSeedBootstrap(ctx, self.directory, self.acceptor, settings)
	if result == nil {
		t.Fatal("the pass returned no result")
	}
	if len(result.Mirrors) != len(settings.Mirrors) {
		t.Fatalf("mirror results = %d, expected one per mirror (%d)", len(result.Mirrors), len(settings.Mirrors))
	}
	return result, err, time.Since(startTime)
}

// The bound a pass that should end promptly must finish within, generous for
// a loaded host and far below the stall timeouts the tests use.
const testSeedPromptTimeout = 10 * time.Second

// Recovery with empty local state, the dns bootstrap blocked and no control
// reach (§2.4): a mirror that needs a name fails, the mirror with an address
// is reached there with its certificate verified for its name, and the
// bundle's verified records become the only candidates. No name is ever
// resolved and no unsigned hint is dialed.
func TestExtenderSeedBootstrapColdStartWithoutDns(t *testing.T) {
	bootstrap := newTestSeedBootstrap(t)
	requireNothingDialable(t, bootstrap.directory)
	server := newTestSeedMirrorServer(t, bootstrap.certificate, testSeedArtifactHandler(bootstrap.artifact(t, 4)))

	settings := bootstrap.settings(
		&ExtenderSeedMirror{Url: "https://seed-dns.example/seed"},
		&ExtenderSeedMirror{Url: "https://seed-a.example/seed", Address: server.Listener.Addr().String()},
	)
	result, err, _ := bootstrap.run(t, t.Context(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ExtenderSeedOutcomeAccepted || result.Bundle.Sequence != 4 || result.Apply.UsableRecordCount != 2 {
		t.Fatalf("pass = %q sequence %v apply %+v", result.Outcome, result.Bundle, result.Apply)
	}
	if outcome := result.Mirrors[1].Outcome; outcome != ExtenderSeedMirrorOutcomeAccepted {
		t.Fatalf("the addressed mirror's outcome = %s", outcome)
	}
	if outcome := result.Mirrors[0].Outcome; outcome != ExtenderSeedMirrorOutcomeDial && outcome != ExtenderSeedMirrorOutcomeCanceled {
		t.Fatalf("the name-only mirror's outcome = %s, expected dial or canceled", outcome)
	}
	for _, target := range bootstrap.dialer.dialed() {
		if target != server.Listener.Addr().String() && target != "seed-dns.example:443" {
			t.Fatalf("the pass dialed %s", target)
		}
	}

	candidates := bootstrap.directory.Candidates(0, 64)
	requireCandidateIps(t, candidates, "203.0.113.1", "2001:db8::2")
	for _, candidate := range candidates {
		if !candidate.Verified || candidate.Source != ExtenderSourceSeed {
			t.Fatalf("a candidate after the pass is not a verified seed record: %+v", candidate)
		}
	}
	if sequence, err := bootstrap.acceptor.Sequence(testExtenderNetworkHost, bootstrap.directory.RootKeys()); err != nil || sequence != 4 {
		t.Fatalf("floor = %d, %v", sequence, err)
	}
}

// The text form is an artifact too.
func TestExtenderSeedBootstrapServesTheTextForm(t *testing.T) {
	bootstrap := newTestSeedBootstrap(t)
	envelope, err := ParseExtenderSeedEnvelope(bootstrap.artifact(t, 2))
	if err != nil {
		t.Fatal(err)
	}
	text, err := EncodeExtenderSeedText(envelope)
	if err != nil {
		t.Fatal(err)
	}
	server := newTestSeedMirrorServer(t, bootstrap.certificate, testSeedArtifactHandler([]byte(text+"\n")))
	result, err, _ := bootstrap.run(t, t.Context(), bootstrap.settings(
		&ExtenderSeedMirror{Url: "https://seed-a.example/seed.txt", Address: server.Listener.Addr().String()},
	))
	if err != nil || result.Bundle == nil {
		t.Fatalf("pass = %v, %v", result, err)
	}
}

// One stalled mirror -- at the http layer or in the tls handshake -- holds up
// nothing but itself: the pass ends with the working mirror's bundle long
// before any stalled attempt would time out.
func TestExtenderSeedBootstrapStalledMirrorDoesNotBlock(t *testing.T) {
	bootstrap := newTestSeedBootstrap(t)
	stalled := newTestSeedMirrorServer(t, bootstrap.certificate, testSeedStallHandler(nil))
	silent := newTestSeedSilentListener(t)
	working := newTestSeedMirrorServer(t, bootstrap.certificate, testSeedArtifactHandler(bootstrap.artifact(t, 1)))

	settings := bootstrap.settings(
		&ExtenderSeedMirror{Url: "https://seed-a.example/seed", Address: stalled.Listener.Addr().String()},
		&ExtenderSeedMirror{Url: "https://seed-b.example/seed", Address: silent},
		&ExtenderSeedMirror{Url: "https://seed-dns.example/seed", Address: working.Listener.Addr().String()},
	)
	settings.AttemptTimeout = time.Minute
	settings.Timeout = 2 * time.Minute
	result, err, elapsed := bootstrap.run(t, t.Context(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if testSeedPromptTimeout <= elapsed {
		t.Fatalf("the pass took %s with a working mirror", elapsed)
	}
	if result.Mirrors[2].Outcome != ExtenderSeedMirrorOutcomeAccepted {
		t.Fatalf("the working mirror's outcome = %s", result.Mirrors[2].Outcome)
	}
	for _, i := range []int{0, 1} {
		if result.Mirrors[i].Outcome != ExtenderSeedMirrorOutcomeCanceled {
			t.Fatalf("stalled mirror %d's outcome = %s, expected canceled", i, result.Mirrors[i].Outcome)
		}
	}
}

// When every mirror stalls, the pass's own deadline ends it.
func TestExtenderSeedBootstrapDeadline(t *testing.T) {
	bootstrap := newTestSeedBootstrap(t)
	stalled := newTestSeedMirrorServer(t, bootstrap.certificate, testSeedStallHandler(nil))
	settings := bootstrap.settings(
		&ExtenderSeedMirror{Url: "https://seed-a.example/seed", Address: stalled.Listener.Addr().String()},
		&ExtenderSeedMirror{Url: "https://seed-b.example/seed", Address: newTestSeedSilentListener(t)},
	)
	settings.AttemptTimeout = time.Minute
	settings.Timeout = 300 * time.Millisecond
	result, err, elapsed := bootstrap.run(t, t.Context(), settings)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeFetch)
	if testSeedPromptTimeout <= elapsed {
		t.Fatalf("the pass took %s past its deadline", elapsed)
	}
	if result.Bundle != nil {
		t.Fatal("a bundle came from stalled mirrors")
	}
	requireNothingDialable(t, bootstrap.directory)
}

// Canceling the pass ends every attempt at once.
func TestExtenderSeedBootstrapCancellation(t *testing.T) {
	bootstrap := newTestSeedBootstrap(t)
	started := make(chan struct{}, 1)
	stalled := newTestSeedMirrorServer(t, bootstrap.certificate, testSeedStallHandler(started))
	settings := bootstrap.settings(
		&ExtenderSeedMirror{Url: "https://seed-a.example/seed", Address: stalled.Listener.Addr().String()},
	)
	settings.AttemptTimeout = time.Minute
	settings.Timeout = 2 * time.Minute
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-started
		cancel()
	}()
	result, err, elapsed := bootstrap.run(t, ctx, settings)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeCanceled)
	if testSeedPromptTimeout <= elapsed {
		t.Fatalf("the canceled pass took %s", elapsed)
	}
	if result.Mirrors[0].Outcome != ExtenderSeedMirrorOutcomeCanceled {
		t.Fatalf("outcome = %s", result.Mirrors[0].Outcome)
	}
}

// Redirects are followed only over https, only to configured destinations and
// only within the bound; a redirect to another configured mirror's host takes
// that mirror's dns-independent route, and one to anywhere else is never
// dialed.
func TestExtenderSeedBootstrapRedirects(t *testing.T) {
	bootstrap := newTestSeedBootstrap(t)
	artifact := bootstrap.artifact(t, 1)
	var target atomic.Pointer[string]
	mux := http.NewServeMux()
	mux.HandleFunc("/seed", testSeedArtifactHandler(artifact))
	mux.HandleFunc("/same", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/seed", http.StatusFound)
	})
	mux.HandleFunc("/other", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, *target.Load(), http.StatusFound)
	})
	mux.HandleFunc("/unlisted", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://unlisted.example/seed", http.StatusFound)
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) {
		hop, _ := strconv.Atoi(r.URL.Query().Get("hop"))
		http.Redirect(w, r, "/loop?hop="+strconv.Itoa(hop+1), http.StatusFound)
	})
	mux.HandleFunc("/plaintext", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://seed-a.example/seed", http.StatusFound)
	})
	server := newTestSeedMirrorServer(t, bootstrap.certificate, mux)
	// the other mirror serves the artifact only where the redirect points
	otherMux := http.NewServeMux()
	otherMux.HandleFunc("/seed", testSeedArtifactHandler(artifact))
	other := newTestSeedMirrorServer(t, bootstrap.certificate, otherMux)
	otherUrl := "https://seed-b.example/seed"
	target.Store(&otherUrl)

	address := server.Listener.Addr().String()
	for _, path := range []string{"/same", "/other"} {
		t.Run(path, func(t *testing.T) {
			bootstrap.acceptor = NewExtenderSeedAcceptor(nil, nil)
			bootstrap.dialer = &testSeedDialer{}
			settings := bootstrap.settings(
				&ExtenderSeedMirror{Url: "https://seed-a.example" + path, Address: address},
				// configured, with its own route; its own url answers 404,
				// so only the redirect reaches its artifact
				&ExtenderSeedMirror{Url: "https://seed-b.example/unused", Address: other.Listener.Addr().String()},
			)
			settings.ParallelCount = 1
			result, err, _ := bootstrap.run(t, t.Context(), settings)
			if err != nil {
				t.Fatal(err)
			}
			if result.Mirrors[0].Outcome != ExtenderSeedMirrorOutcomeAccepted {
				t.Fatalf("outcome = %s", result.Mirrors[0].Outcome)
			}
			for _, dialed := range bootstrap.dialer.dialed() {
				if dialed != address && dialed != other.Listener.Addr().String() {
					t.Fatalf("the redirect dialed %s", dialed)
				}
			}
		})
	}
	for _, path := range []string{"/unlisted", "/loop", "/plaintext"} {
		t.Run(path, func(t *testing.T) {
			bootstrap.acceptor = NewExtenderSeedAcceptor(nil, nil)
			bootstrap.dialer = &testSeedDialer{}
			settings := bootstrap.settings(
				&ExtenderSeedMirror{Url: "https://seed-a.example" + path, Address: address},
			)
			result, err, _ := bootstrap.run(t, t.Context(), settings)
			requireExtenderSeedCode(t, err, ExtenderSeedCodeFetch)
			if result.Mirrors[0].Outcome != ExtenderSeedMirrorOutcomeRedirect || result.Mirrors[0].AttemptCount != 1 {
				t.Fatalf("outcome = %s after %d attempts, expected one redirect refusal", result.Mirrors[0].Outcome, result.Mirrors[0].AttemptCount)
			}
			for _, dialed := range bootstrap.dialer.dialed() {
				if dialed != address {
					t.Fatalf("a refused redirect dialed %s", dialed)
				}
			}
			if dialCount := len(bootstrap.dialer.dialed()); settings.MaxRedirectCount+1 < dialCount {
				t.Fatalf("dials = %d, past the redirect bound", dialCount)
			}
		})
	}
}

// Tls is verified for the url's host whatever address the route dials: a
// mirror whose certificate does not name its url's host is refused and not
// tried again.
func TestExtenderSeedBootstrapVerifiesTheIntendedServerName(t *testing.T) {
	bootstrap := newTestSeedBootstrap(t)
	server := newTestSeedMirrorServer(t, bootstrap.certificate, testSeedArtifactHandler(bootstrap.artifact(t, 1)))
	settings := bootstrap.settings(
		&ExtenderSeedMirror{Url: "https://wrong.example/seed", Address: server.Listener.Addr().String()},
	)
	result, err, _ := bootstrap.run(t, t.Context(), settings)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeFetch)
	if result.Mirrors[0].Outcome != ExtenderSeedMirrorOutcomeTls || result.Mirrors[0].AttemptCount != 1 {
		t.Fatalf("outcome = %s after %d attempts, expected one tls refusal", result.Mirrors[0].Outcome, result.Mirrors[0].AttemptCount)
	}

	// and an untrusted certificate for the right name is refused too
	settings = bootstrap.settings(
		&ExtenderSeedMirror{Url: "https://seed-a.example/seed", Address: server.Listener.Addr().String()},
	)
	settings.RootCAs = x509.NewCertPool()
	result, err, _ = bootstrap.run(t, t.Context(), settings)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeFetch)
	if result.Mirrors[0].Outcome != ExtenderSeedMirrorOutcomeTls {
		t.Fatalf("outcome = %s, expected tls", result.Mirrors[0].Outcome)
	}
	requireNothingDialable(t, bootstrap.directory)
}

// A mirror that is overloaded or mid-update is tried again after a backoff;
// one that answers that it has nothing is not.
func TestExtenderSeedBootstrapRetries(t *testing.T) {
	bootstrap := newTestSeedBootstrap(t)
	artifact := bootstrap.artifact(t, 1)
	var hitCount atomic.Int64
	flaky := newTestSeedMirrorServer(t, bootstrap.certificate, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hitCount.Add(1) == 1 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		w.Write(artifact)
	}))
	result, err, _ := bootstrap.run(t, t.Context(), bootstrap.settings(
		&ExtenderSeedMirror{Url: "https://seed-a.example/seed", Address: flaky.Listener.Addr().String()},
	))
	if err != nil {
		t.Fatal(err)
	}
	if result.Mirrors[0].AttemptCount != 2 || result.Mirrors[0].Outcome != ExtenderSeedMirrorOutcomeAccepted {
		t.Fatalf("outcome = %s after %d attempts, expected acceptance on the second", result.Mirrors[0].Outcome, result.Mirrors[0].AttemptCount)
	}

	missing := newTestSeedMirrorServer(t, bootstrap.certificate, http.NotFoundHandler())
	bootstrap.acceptor = NewExtenderSeedAcceptor(nil, nil)
	result, err, _ = bootstrap.run(t, t.Context(), bootstrap.settings(
		&ExtenderSeedMirror{Url: "https://seed-a.example/seed", Address: missing.Listener.Addr().String()},
	))
	requireExtenderSeedCode(t, err, ExtenderSeedCodeFetch)
	if result.Mirrors[0].AttemptCount != 1 || result.Mirrors[0].Outcome != ExtenderSeedMirrorOutcomeHttpStatus {
		t.Fatalf("outcome = %s after %d attempts, expected one http status", result.Mirrors[0].Outcome, result.Mirrors[0].AttemptCount)
	}
}

// The shared budget bounds the whole pass: attempts over every mirror, dials
// over every attempt, bytes over every response, and attempts in flight.
func TestExtenderSeedBootstrapBudgets(t *testing.T) {
	t.Run("attempts", func(t *testing.T) {
		bootstrap := newTestSeedBootstrap(t)
		failing := newTestSeedMirrorServer(t, bootstrap.certificate, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "busy", http.StatusServiceUnavailable)
		}))
		mirrors := []*ExtenderSeedMirror{}
		for range 3 {
			mirrors = append(mirrors, &ExtenderSeedMirror{Url: "https://seed-a.example/seed", Address: failing.Listener.Addr().String()})
		}
		settings := bootstrap.settings(mirrors...)
		settings.MaxAttemptCount = 4
		result, err, _ := bootstrap.run(t, t.Context(), settings)
		requireExtenderSeedCode(t, err, ExtenderSeedCodeFetch)
		attemptCount := 0
		budgetCount := 0
		for _, mirrorResult := range result.Mirrors {
			attemptCount += mirrorResult.AttemptCount
			if mirrorResult.Outcome == ExtenderSeedMirrorOutcomeBudget {
				budgetCount += 1
			}
		}
		if attemptCount != 4 || result.AttemptCount != 4 || budgetCount != 3 {
			t.Fatalf("attempts = %d (%d), budget outcomes = %d; expected 4 attempts and every mirror out of budget", attemptCount, result.AttemptCount, budgetCount)
		}
	})
	t.Run("dials", func(t *testing.T) {
		bootstrap := newTestSeedBootstrap(t)
		settings := bootstrap.settings(
			&ExtenderSeedMirror{Url: "https://seed-a.example/seed"},
			&ExtenderSeedMirror{Url: "https://seed-b.example/seed"},
		)
		settings.MaxDialCount = 3
		result, err, _ := bootstrap.run(t, t.Context(), settings)
		requireExtenderSeedCode(t, err, ExtenderSeedCodeFetch)
		if dialCount := len(bootstrap.dialer.dialed()); dialCount != 3 || result.DialCount != 3 {
			t.Fatalf("dials = %d (%d), expected the budget of 3", dialCount, result.DialCount)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		bootstrap := newTestSeedBootstrap(t)
		server := newTestSeedMirrorServer(t, bootstrap.certificate, testSeedArtifactHandler(bootstrap.artifact(t, 1)))
		settings := bootstrap.settings(
			&ExtenderSeedMirror{Url: "https://seed-a.example/seed", Address: server.Listener.Addr().String()},
		)
		settings.MaxByteCount = 100
		result, err, _ := bootstrap.run(t, t.Context(), settings)
		requireExtenderSeedCode(t, err, ExtenderSeedCodeFetch)
		if result.Mirrors[0].Outcome != ExtenderSeedMirrorOutcomeBudget || 100 < result.ByteCount {
			t.Fatalf("outcome = %s with %d bytes, expected the byte budget", result.Mirrors[0].Outcome, result.ByteCount)
		}
	})
	t.Run("parallel", func(t *testing.T) {
		bootstrap := newTestSeedBootstrap(t)
		// requests in flight at the mirror, which the semaphore bounds; the
		// client's connections close asynchronously, so they are not the
		// measure
		var inFlightCount atomic.Int64
		var maxInFlightCount atomic.Int64
		slow := newTestSeedMirrorServer(t, bootstrap.certificate, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inFlight := inFlightCount.Add(1)
			defer inFlightCount.Add(-1)
			for {
				maxInFlight := maxInFlightCount.Load()
				if inFlight <= maxInFlight || maxInFlightCount.CompareAndSwap(maxInFlight, inFlight) {
					break
				}
			}
			time.Sleep(50 * time.Millisecond)
			http.Error(w, "missing", http.StatusNotFound)
		}))
		mirrors := []*ExtenderSeedMirror{}
		for range 4 {
			mirrors = append(mirrors, &ExtenderSeedMirror{Url: "https://seed-a.example/seed", Address: slow.Listener.Addr().String()})
		}
		settings := bootstrap.settings(mirrors...)
		settings.ParallelCount = 1
		if _, err, _ := bootstrap.run(t, t.Context(), settings); err == nil {
			t.Fatal("missing artifacts were accepted")
		}
		if maxInFlight := maxInFlightCount.Load(); maxInFlight != 1 {
			t.Fatalf("requests in flight at once = %d, expected 1", maxInFlight)
		}
	})
}

// A response past the artifact cap is refused, by its length when it states
// one and by what is read when it does not, and never read past the cap. The
// cap is the mirror contract's (api/bringyour.yml, `/v1/seed.bin`): the raw
// envelope at most 64 KiB, so a response one byte past it is already too
// large, refused before anything is decoded.
func TestExtenderSeedBootstrapOversizedResponse(t *testing.T) {
	for _, size := range []int{ExtenderSeedMaxEnvelopeByteCount + 1, 4 * extenderSeedMaxArtifactByteCount} {
		for _, stated := range []bool{true, false} {
			t.Run(strconv.Itoa(size)+"/"+strconv.FormatBool(stated), func(t *testing.T) {
				bootstrap := newTestSeedBootstrap(t)
				oversize := strings.Repeat("A", size)
				server := newTestSeedMirrorServer(t, bootstrap.certificate, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if stated {
						w.Header().Set("Content-Length", strconv.Itoa(len(oversize)))
					}
					io.WriteString(w, oversize)
				}))
				result, err, _ := bootstrap.run(t, t.Context(), bootstrap.settings(
					&ExtenderSeedMirror{Url: "https://seed-a.example/seed", Address: server.Listener.Addr().String()},
				))
				requireExtenderSeedCode(t, err, ExtenderSeedCodeFetch)
				if result.Mirrors[0].Outcome != ExtenderSeedMirrorOutcomeTooLarge || result.Mirrors[0].AttemptCount != 1 {
					t.Fatalf("outcome = %s/%s after %d attempts", result.Mirrors[0].Outcome, result.Mirrors[0].Code, result.Mirrors[0].AttemptCount)
				}
				if int64(extenderSeedMaxArtifactByteCount) < result.ByteCount {
					t.Fatalf("read %d bytes, past the cap", result.ByteCount)
				}
				if stated && result.ByteCount != 0 {
					t.Fatalf("read %d bytes of a response whose length is past the cap", result.ByteCount)
				}
			})
		}
	}
}

// A reader that counts the bytes it hands out.
type testSeedCountingReader struct {
	reader    io.Reader
	readCount int
}

func (self *testSeedCountingReader) Read(p []byte) (int, error) {
	n, err := self.reader.Read(p)
	self.readCount += n
	return n, err
}

// The read takes at most one byte past the cap from a response body, however
// much more the body has, and a body of exactly the cap is read whole.
func TestExtenderSeedBootstrapReadsAtMostOneBytePastTheCap(t *testing.T) {
	newPass := func() *extenderSeedBootstrapPass {
		return &extenderSeedBootstrapPass{
			budget: &extenderSeedPassBudget{
				maxAttemptCount: 1,
				maxDialCount:    1,
				maxByteCount:    int64(8 * extenderSeedMaxArtifactByteCount),
			},
		}
	}
	for _, size := range []int{ExtenderSeedMaxEnvelopeByteCount + 1, ExtenderSeedMaxEnvelopeByteCount + 4096, 4 * ExtenderSeedMaxEnvelopeByteCount} {
		body := &testSeedCountingReader{reader: strings.NewReader(strings.Repeat("A", size))}
		pass := newPass()
		if _, err := pass.readArtifact(body); !errors.Is(err, errExtenderSeedTooLarge) {
			t.Fatalf("a %d byte body read as %v, expected too large", size, err)
		}
		if ExtenderSeedMaxEnvelopeByteCount+1 < body.readCount {
			t.Fatalf("read %d bytes of a %d byte body, past one byte over the cap", body.readCount, size)
		}
		if used := pass.budget.byteCount.Load(); int64(ExtenderSeedMaxEnvelopeByteCount) < used {
			t.Fatalf("the budget took %d bytes, past the cap", used)
		}
	}

	body := &testSeedCountingReader{reader: strings.NewReader(strings.Repeat("A", ExtenderSeedMaxEnvelopeByteCount))}
	artifact, err := newPass().readArtifact(body)
	if err != nil || len(artifact) != ExtenderSeedMaxEnvelopeByteCount {
		t.Fatalf("a body of exactly the cap read as %d bytes, %v", len(artifact), err)
	}
}

// What a mirror serves is judged like any seed: an expired bundle is reported
// as a freshness failure -- a wrong local clock is told as one -- a forged one
// as a signature failure, and a rollback below the floor as a rollback. None
// changes the directory.
func TestExtenderSeedBootstrapRefusedArtifacts(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		bootstrap := newTestSeedBootstrap(t)
		server := newTestSeedMirrorServer(t, bootstrap.certificate, testSeedArtifactHandler(bootstrap.artifact(t, 1)))
		bootstrap.clock.advance(25 * time.Hour)
		result, err, _ := bootstrap.run(t, t.Context(), bootstrap.settings(
			&ExtenderSeedMirror{Url: "https://seed-a.example/seed", Address: server.Listener.Addr().String()},
		))
		requireExtenderSeedCode(t, err, ExtenderSeedCodeExpired)
		if result.Mirrors[0].Outcome != ExtenderSeedMirrorOutcomeRefused || result.Mirrors[0].Code != ExtenderSeedCodeExpired {
			t.Fatalf("outcome = %s/%s", result.Mirrors[0].Outcome, result.Mirrors[0].Code)
		}
		requireNothingDialable(t, bootstrap.directory)
	})
	t.Run("forged", func(t *testing.T) {
		bootstrap := newTestSeedBootstrap(t)
		now := bootstrap.clock.Now()
		forged, err := MarshalExtenderSeedEnvelope(testSeedEnvelope(t, testSeedForeignRootKey, 1, now, []*protocol.ExtenderRecord{
			testSeedRecord(t, testSeedForeignRootKey, 1, now, "203.0.113.1"),
		}, nil))
		if err != nil {
			t.Fatal(err)
		}
		server := newTestSeedMirrorServer(t, bootstrap.certificate, testSeedArtifactHandler(forged))
		_, err, _ = bootstrap.run(t, t.Context(), bootstrap.settings(
			&ExtenderSeedMirror{Url: "https://seed-a.example/seed", Address: server.Listener.Addr().String()},
		))
		requireExtenderSeedCode(t, err, ExtenderSeedCodeSignature)
		requireNothingDialable(t, bootstrap.directory, "203.0.113.1")
	})
	t.Run("rollback", func(t *testing.T) {
		bootstrap := newTestSeedBootstrap(t)
		newer := newTestSeedMirrorServer(t, bootstrap.certificate, testSeedArtifactHandler(bootstrap.artifact(t, 5)))
		if _, err, _ := bootstrap.run(t, t.Context(), bootstrap.settings(
			&ExtenderSeedMirror{Url: "https://seed-a.example/seed", Address: newer.Listener.Addr().String()},
		)); err != nil {
			t.Fatal(err)
		}
		older := newTestSeedMirrorServer(t, bootstrap.certificate, testSeedArtifactHandler(bootstrap.artifact(t, 4)))
		_, err, _ := bootstrap.run(t, t.Context(), bootstrap.settings(
			&ExtenderSeedMirror{Url: "https://seed-a.example/seed", Address: older.Listener.Addr().String()},
		))
		requireExtenderSeedCode(t, err, ExtenderSeedCodeRollback)
	})
}

// A pass with nothing to verify under fetches nothing, and a pass with no
// usable mirror dials nothing.
func TestExtenderSeedBootstrapPreconditions(t *testing.T) {
	bootstrap := newTestSeedBootstrap(t)
	unconfigured := newTestSeedDirectory(t, bootstrap.clock)
	result, err := ExtenderSeedBootstrap(t.Context(), unconfigured, bootstrap.acceptor, bootstrap.settings(
		&ExtenderSeedMirror{Url: "https://seed-a.example/seed", Address: "127.0.0.1:1"},
	))
	requireExtenderSeedCode(t, err, ExtenderSeedCodeNoRootKeys)
	if result == nil || len(bootstrap.dialer.dialed()) != 0 {
		t.Fatal("a pass with no root keys dialed")
	}

	settings := bootstrap.settings(
		&ExtenderSeedMirror{Url: "http://seed-a.example/seed"},
		&ExtenderSeedMirror{Url: "https://seed-a.example/seed", Address: "not an address"},
		&ExtenderSeedMirror{Url: "https://user@seed-a.example/seed"},
		&ExtenderSeedMirror{Url: ""},
		nil,
	)
	result, err, _ = bootstrap.run(t, t.Context(), settings)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeNoMirror)
	for i, mirrorResult := range result.Mirrors {
		if mirrorResult.Outcome != ExtenderSeedMirrorOutcomeInvalid {
			t.Fatalf("mirror %d outcome = %s, expected invalid", i, mirrorResult.Outcome)
		}
	}
	if len(bootstrap.dialer.dialed()) != 0 {
		t.Fatal("a pass with no usable mirror dialed")
	}
}

// A bundle whose records the directory already holds revoked restores nothing:
// the pass reports it rather than success.
func TestExtenderSeedBootstrapNoUsableRecord(t *testing.T) {
	bootstrap := newTestSeedBootstrap(t)
	now := bootstrap.clock.Now()
	for _, n := range []byte{1, 2} {
		if _, err := bootstrap.directory.ApplyRevocation(testSeedRevocation(t, testSeedRootKey, n, now.Add(-time.Minute))); err != nil {
			t.Fatal(err)
		}
	}
	server := newTestSeedMirrorServer(t, bootstrap.certificate, testSeedArtifactHandler(bootstrap.artifact(t, 1)))
	result, err, _ := bootstrap.run(t, t.Context(), bootstrap.settings(
		&ExtenderSeedMirror{Url: "https://seed-a.example/seed", Address: server.Listener.Addr().String()},
	))
	requireExtenderSeedCode(t, err, ExtenderSeedCodeNoUsableRecord)
	if result.Bundle == nil || result.Apply == nil || result.Apply.UsableRecordCount != 0 {
		t.Fatalf("result = %+v", result)
	}
	requireNothingDialable(t, bootstrap.directory, "203.0.113.1", "2001:db8::2")
}
