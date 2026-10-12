package extender

// The measurement key and certificate of the lifecycle (lag.dev/PLAN.md C4,
// C6, A8, M10).
//
// The operator is an httptest server that answers the certificate route from
// a state the test sets and issues chains from a test certificate authority.
// The lifecycle reads a test clock and waits on it, so nothing sleeps: each
// wait the lifecycle starts is reported to the test, which moves the clock
// past it to fire it.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// The bearer every request of the lifecycle carries in these tests.
const testLagJwt = "test-lag-jwt"

// A clock the lifecycle reads and waits on, moved only by the test.
type testLagClock struct {
	stateLock sync.Mutex
	now       time.Time
	timers    []*testLagTimer
	// one entry per wait the lifecycle starts
	waits chan time.Duration
}

type testLagTimer struct {
	deadline time.Time
	fire     chan time.Time
}

func newTestLagClock() *testLagClock {
	return &testLagClock{
		now:   time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC),
		waits: make(chan time.Duration, 1024),
	}
}

func (self *testLagClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *testLagClock) After(wait time.Duration) <-chan time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	fire := make(chan time.Time, 1)
	self.timers = append(self.timers, &testLagTimer{
		deadline: self.now.Add(wait),
		fire:     fire,
	})
	self.waits <- wait
	return fire
}

// Moves the clock and fires every wait that came due.
func (self *testLagClock) advance(d time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(d)
	timers := []*testLagTimer{}
	for _, timer := range self.timers {
		if self.now.Before(timer.deadline) {
			timers = append(timers, timer)
		} else {
			timer.fire <- self.now
		}
	}
	self.timers = timers
}

// Waits for the lifecycle to start a wait, and returns how long it is.
func (self *testLagClock) nextWait(t *testing.T) time.Duration {
	t.Helper()
	select {
	case wait := <-self.waits:
		return wait
	case <-time.After(30 * time.Second):
		t.Fatal("the lifecycle did not wait")
		return 0
	}
}

// A certificate authority for the chains the test operator issues.
type testLagCa struct {
	key         *ecdsa.PrivateKey
	certificate *x509.Certificate

	stateLock sync.Mutex
	serial    int64
}

func newTestLagCa(t *testing.T) *testLagCa {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "lag test ca"},
		NotBefore:             time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:              time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	certificateBytes, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(certificateBytes)
	if err != nil {
		t.Fatal(err)
	}
	return &testLagCa{
		key:         key,
		certificate: certificate,
		serial:      1,
	}
}

// The chain of a leaf for the request, as the operator returns it: the leaf,
// then the authority.
func (self *testLagCa) issue(t *testing.T, csr string, notBefore time.Time, notAfter time.Time) string {
	t.Helper()
	request := parseTestLagCsr(t, csr)
	return self.issueFor(t, request.PublicKey, request.DNSNames, notBefore, notAfter)
}

func (self *testLagCa) issueFor(
	t *testing.T,
	publicKey any,
	names []string,
	notBefore time.Time,
	notAfter time.Time,
) string {
	t.Helper()
	self.stateLock.Lock()
	self.serial += 1
	serial := self.serial
	self.stateLock.Unlock()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		DNSNames:     names,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafBytes, err := x509.CreateCertificate(rand.Reader, template, self.certificate, publicKey, self.key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafBytes})) +
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: self.certificate.Raw}))
}

// A request as the operator reads it: standard base64 DER, signature checked.
func parseTestLagCsr(t *testing.T, csr string) *x509.CertificateRequest {
	t.Helper()
	csrBytes, err := base64.StdEncoding.DecodeString(csr)
	if err != nil {
		t.Fatal(err)
	}
	request, err := x509.ParseCertificateRequest(csrBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := request.CheckSignature(); err != nil {
		t.Fatal(err)
	}
	return request
}

// The operator's api: the certificate route (A8), the report route (A7) and
// the hello the client strategy reads.
type testLagApi struct {
	t      *testing.T
	server *httptest.Server

	polls chan struct{}
	posts chan []string
	// the measurement request of each activation that carried one
	activationCsrs chan string

	stateLock    sync.Mutex
	certResult   *extenderMeasurementCertResult
	reportStatus int
	reportResult *extenderLagReportResult
}

func newTestLagApi(t *testing.T) *testLagApi {
	t.Helper()
	api := &testLagApi{
		t:              t,
		polls:          make(chan struct{}, 1024),
		posts:          make(chan []string, 1024),
		activationCsrs: make(chan string, 1024),
		certResult:     &extenderMeasurementCertResult{State: ExtenderMeasurementCertStateNone},
	}
	api.server = httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(api.server.Close)
	return api
}

func (self *testLagApi) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/hello" {
		w.Write([]byte(`{"client_address":"198.51.100.1:443"}`))
		return
	}
	// every route of the lifecycle is under the activation credential
	if r.Header.Get("Authorization") != "Bearer "+testLagJwt {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == connect.ExtenderActivatePath:
		// every activation is of the one v4 address, and a request it carries
		// is pending until the test issues it (A10)
		args := &connect.ExtenderActivateArgs{}
		if err := json.NewDecoder(r.Body).Decode(args); err != nil {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		if args.MeasurementCsr != "" {
			request := parseTestLagCsr(self.t, args.MeasurementCsr)
			self.setCertResult(&extenderMeasurementCertResult{
				State: ExtenderMeasurementCertStatePending,
				Names: request.DNSNames,
			})
		}
		json.NewEncoder(w).Encode(&connect.ExtenderActivateResult{
			Activated: true,
			Ip:        testLagActivateIp,
			IpVersion: 4,
			Carriers:  args.Carriers,
		})
		self.activationCsrs <- args.MeasurementCsr
	case r.Method == http.MethodGet && r.URL.Path == ExtenderMeasurementCertPath:
		self.stateLock.Lock()
		certResult := *self.certResult
		self.stateLock.Unlock()
		json.NewEncoder(w).Encode(&certResult)
		self.polls <- struct{}{}
	case r.Method == http.MethodPost && r.URL.Path == ExtenderLagReportPath:
		args := &extenderLagReportArgs{}
		if err := json.NewDecoder(r.Body).Decode(args); err != nil {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		self.stateLock.Lock()
		reportStatus := self.reportStatus
		reportResult := self.reportResult
		self.stateLock.Unlock()
		if reportStatus != 0 && reportStatus != http.StatusOK {
			w.WriteHeader(reportStatus)
			w.Write([]byte("the operator is unavailable"))
		} else if reportResult != nil {
			json.NewEncoder(w).Encode(reportResult)
		} else {
			json.NewEncoder(w).Encode(&extenderLagReportResult{Accepted: len(args.Reports)})
		}
		self.posts <- args.Reports
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (self *testLagApi) setCertResult(certResult *extenderMeasurementCertResult) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.certResult = certResult
}

func (self *testLagApi) setReportAnswer(reportStatus int, reportResult *extenderLagReportResult) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.reportStatus = reportStatus
	self.reportResult = reportResult
}

// Waits for one read of the certificate route.
func (self *testLagApi) waitPoll() {
	self.t.Helper()
	select {
	case <-self.polls:
	case <-time.After(30 * time.Second):
		self.t.Fatal("the lifecycle did not poll the certificate")
	}
}

// Waits for one post of the report route, and returns its reports.
func (self *testLagApi) waitPost() []string {
	self.t.Helper()
	select {
	case reports := <-self.posts:
		return reports
	case <-time.After(30 * time.Second):
		self.t.Fatal("the lifecycle did not post reports")
		return nil
	}
}

// Waits for one activation post, and returns the request it carried.
func (self *testLagApi) waitActivation() string {
	self.t.Helper()
	select {
	case csr := <-self.activationCsrs:
		return csr
	case <-time.After(30 * time.Second):
		self.t.Fatal("the activator did not post an activation")
		return ""
	}
}

// Checks that nothing was posted or polled beyond what the test waited for.
func (self *testLagApi) expectQuiet() {
	self.t.Helper()
	select {
	case <-self.polls:
		self.t.Fatal("the lifecycle polled unexpectedly")
	case reports := <-self.posts:
		self.t.Fatalf("the lifecycle posted %d reports unexpectedly", len(reports))
	default:
	}
}

// A lifecycle against the test operator on the test clock. `configure` adjusts
// the role's settings before the lifecycle is built.
func newTestLagLifecycle(
	t *testing.T,
	api *testLagApi,
	clock *testLagClock,
	certificate *ExtenderMeasurementCert,
	configure func(settings *ExtenderLagLifecycleSettings),
) *extenderLagLifecycle {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	// one dialer, so one request reaches the operator per call
	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.EnableResilient = false
	clientStrategy := connect.NewDirectClientStrategy(ctx, strategySettings, 0)
	server := NewExtenderServer(ctx, nil, nil, nil, &net.Dialer{}, DefaultExtenderSettings())

	settings := DefaultExtenderLagLifecycleSettings()
	settings.Certificate = certificate
	settings.ApiUrl = api.server.URL
	settings.ByJwt = func() string {
		return testLagJwt
	}
	settings.ClientStrategy = clientStrategy
	settings.Now = clock.Now
	settings.After = clock.After
	if configure != nil {
		configure(settings)
	}
	lifecycle := newExtenderLagLifecycle(server, settings)
	t.Cleanup(func() {
		server.CloseAndWait()
		clientStrategy.Close()
		cancel()
	})
	return lifecycle
}

// A measurement certificate with its key in a test directory.
func newTestLagCertificate(t *testing.T) *ExtenderMeasurementCert {
	t.Helper()
	certificate, err := NewExtenderMeasurementCert(filepath.Join(t.TempDir(), ExtenderMeasurementKeyFileName), "")
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

// Waits for the next install or removal of a certificate, after `version`.
func waitTestLagInstall(t *testing.T, certificate *ExtenderMeasurementCert, version uint64) uint64 {
	t.Helper()
	for {
		current, change := certificate.installMonitor.Get()
		if version < current {
			return current
		}
		select {
		case <-change:
		case <-time.After(30 * time.Second):
			t.Fatal("the lifecycle did not install a certificate")
			return 0
		}
	}
}

// The address the test operator activates.
const testLagActivateIp = "198.51.100.7"

// The addresses of the name vectors, and their names under the default zone
// (N1).
var testLagNameVectors = []struct {
	addr string
	name string
}{
	{addr: "203.0.113.7", name: "203-0-113-7.extender.bringyour.com"},
	{addr: "::ffff:203.0.113.7", name: "203-0-113-7.extender.bringyour.com"},
	{addr: "2001:db8::1", name: "2001-db8--1.extender.bringyour.com"},
	{addr: "2001:db8:0:1:1:1:1:1", name: "2001-db8-0-1-1-1-1-1.extender.bringyour.com"},
	{addr: "2001:db8::", name: "2001-db8--0.extender.bringyour.com"},
}

// The key is made once, kept beside the extender key file with mode 0600 as
// PKCS #8 PEM, and the same key is loaded from it again (C4).
func TestLagCertKeyPersistence(t *testing.T) {
	stateDir := t.TempDir()
	keyFile := ExtenderMeasurementKeyPath(filepath.Join(stateDir, ".extender_key"))
	if keyFile != filepath.Join(stateDir, "extender-measurement-key.pem") {
		t.Fatalf("key file = %s, expected beside the extender key file", keyFile)
	}

	certificate, err := NewExtenderMeasurementCert(keyFile, "")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Fatalf("key file mode = %o, expected 600", mode)
	}
	keyPem, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(keyPem)
	if block == nil || block.Type != "PRIVATE KEY" {
		t.Fatalf("key file = %q, expected a PKCS #8 pem block", keyPem)
	}
	if certificate.PublicKey().Curve != elliptic.P256() {
		t.Fatal("the key is not P-256")
	}
	// nothing but the key file is left in the directory
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("state dir holds %d files, expected the key file alone", len(entries))
	}

	again, err := NewExtenderMeasurementCert(keyFile, "")
	if err != nil {
		t.Fatal(err)
	}
	if !again.PublicKey().Equal(certificate.PublicKey()) {
		t.Fatal("the key was not loaded again from its file")
	}

	// a key file readable beyond its owner is narrowed
	if err := os.Chmod(keyFile, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewExtenderMeasurementCert(keyFile, ""); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(keyFile); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("key file mode = %v %v, expected narrowed to 600", info.Mode().Perm(), err)
	}

	// an empty path keeps a key in memory only
	ephemeral, err := NewExtenderMeasurementCert("", "")
	if err != nil {
		t.Fatal(err)
	}
	if ephemeral.PublicKey().Equal(certificate.PublicKey()) {
		t.Fatal("an ephemeral key equals the kept one")
	}
}

// A key file that is not an ECDSA P-256 key is refused rather than replaced.
func TestLagCertKeyRefusesAnotherKey(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), ExtenderMeasurementKeyFileName)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaKeyBytes, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	rsaKeyPem := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rsaKeyBytes})
	if err := os.WriteFile(keyFile, rsaKeyPem, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewExtenderMeasurementCert(keyFile, ""); err == nil {
		t.Fatal("an rsa key file was accepted")
	}
	if keyPem, err := os.ReadFile(keyFile); err != nil || string(keyPem) != string(rsaKeyPem) {
		t.Fatal("the key file was replaced")
	}

	p384Key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p384KeyBytes, err := x509.MarshalECPrivateKey(p384Key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: p384KeyBytes}), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewExtenderMeasurementCert(keyFile, ""); err == nil {
		t.Fatal("a P-384 key file was accepted")
	}

	// SEC 1 is read as well as PKCS #8
	p256Key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p256KeyBytes, err := x509.MarshalECPrivateKey(p256Key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: p256KeyBytes}), 0600); err != nil {
		t.Fatal(err)
	}
	certificate, err := NewExtenderMeasurementCert(keyFile, "")
	if err != nil {
		t.Fatal(err)
	}
	if !certificate.PublicKey().Equal(&p256Key.PublicKey) {
		t.Fatal("the SEC 1 key was not loaded")
	}
}

// The request names exactly the measurement names of the addresses (N1, N3,
// C4): one per address in ip version order, a v4-mapped address as its v4,
// nothing else in the request, and signed by the measurement key.
func TestLagCertCsrNames(t *testing.T) {
	for _, vector := range testLagNameVectors {
		names := extenderMeasurementNames([]netip.Addr{netip.MustParseAddr(vector.addr)}, connect.ExtenderMeasurementZone)
		if !slices.Equal(names, []string{vector.name}) {
			t.Errorf("names of %s = %v, expected %s", vector.addr, names, vector.name)
		}
	}

	certificate := newTestLagCertificate(t)
	addrs := []netip.Addr{
		netip.MustParseAddr("2001:db8::1"),
		netip.MustParseAddr("::ffff:203.0.113.7"),
		netip.MustParseAddr("203.0.113.7"),
		{},
	}
	request := parseTestLagCsr(t, certificate.ActivationCsr(addrs))
	expectedNames := []string{
		"203-0-113-7.extender.bringyour.com",
		"2001-db8--1.extender.bringyour.com",
	}
	if !slices.Equal(request.DNSNames, expectedNames) {
		t.Fatalf("request names = %v, expected %v", request.DNSNames, expectedNames)
	}
	if 0 < len(request.IPAddresses)+len(request.EmailAddresses)+len(request.URIs) || request.Subject.CommonName != "" {
		t.Fatalf("request = %+v, expected the dns names alone", request)
	}
	if publicKey, ok := request.PublicKey.(*ecdsa.PublicKey); !ok || !publicKey.Equal(certificate.PublicKey()) {
		t.Fatal("the request is not for the measurement key")
	}
	if request.SignatureAlgorithm != x509.ECDSAWithSHA256 {
		t.Fatalf("request signature = %s", request.SignatureAlgorithm)
	}

	// the same names ask with the same request; no address asks for nothing
	if again := certificate.ActivationCsr(addrs[0:3]); again != certificate.ActivationCsr(addrs) {
		t.Fatal("the same names were asked for with another request")
	}
	if csr := certificate.ActivationCsr(nil); csr != "" {
		t.Fatalf("no address asked with %q", csr)
	}

	// a configured zone names under it
	zoned, err := NewExtenderMeasurementCert("", "Measure.Example.")
	if err != nil {
		t.Fatal(err)
	}
	request = parseTestLagCsr(t, zoned.ActivationCsr([]netip.Addr{netip.MustParseAddr("198.51.100.7")}))
	if !slices.Equal(request.DNSNames, []string{"198-51-100-7.measure.example"}) {
		t.Fatalf("zoned names = %v", request.DNSNames)
	}
}

// The operator's state decides whether an activation asks: nothing while the
// operator holds a request or a certificate for the names, and a new request
// once the names change (C4).
func TestLagCertCsrAsksOnlyForNewNames(t *testing.T) {
	clock := newTestLagClock()
	api := newTestLagApi(t)
	certificate := newTestLagCertificate(t)
	v4 := netip.MustParseAddr("198.51.100.7")
	v6 := netip.MustParseAddr("2001:db8::7")
	names := []string{"198-51-100-7.extender.bringyour.com", "2001-db8--7.extender.bringyour.com"}

	api.setCertResult(&extenderMeasurementCertResult{
		State: ExtenderMeasurementCertStatePending,
		Names: names,
	})
	newTestLagLifecycle(t, api, clock, certificate, nil)
	api.waitPoll()
	clock.nextWait(t)

	if csr := certificate.ActivationCsr([]netip.Addr{v4, v6}); csr != "" {
		t.Fatalf("asked with %q for names the operator holds a request for", csr)
	}
	// a new address is a new request, for exactly the new names
	moved := netip.MustParseAddr("198.51.100.8")
	request := parseTestLagCsr(t, certificate.ActivationCsr([]netip.Addr{moved, v6}))
	movedNames := []string{"198-51-100-8.extender.bringyour.com", "2001-db8--7.extender.bringyour.com"}
	if !slices.Equal(request.DNSNames, movedNames) {
		t.Fatalf("request names = %v, expected %v", request.DNSNames, movedNames)
	}
	// so is a family the operator holds none for
	request = parseTestLagCsr(t, certificate.ActivationCsr([]netip.Addr{v4}))
	if !slices.Equal(request.DNSNames, names[0:1]) {
		t.Fatalf("request names = %v, expected %v", request.DNSNames, names[0:1])
	}
}

// The lifecycle polls until the operator issues, installs the chain without a
// restart, polls again at two thirds of its life and swaps in the renewal, and
// stops presenting a certificate once it expires (C4, C6, A8).
func TestLagCertPollInstallRenew(t *testing.T) {
	clock := newTestLagClock()
	api := newTestLagApi(t)
	ca := newTestLagCa(t)
	certificate := newTestLagCertificate(t)
	addrs := []netip.Addr{netip.MustParseAddr("198.51.100.7"), netip.MustParseAddr("2001:db8::7")}
	names := []string{"198-51-100-7.extender.bringyour.com", "2001-db8--7.extender.bringyour.com"}
	csr := certificate.ActivationCsr(addrs)

	api.setCertResult(&extenderMeasurementCertResult{
		State: ExtenderMeasurementCertStatePending,
		Names: names,
	})
	lifecycle := newTestLagLifecycle(t, api, clock, certificate, nil)
	// the first read is at start, and a pending request is read again on the
	// backoff, which doubles
	api.waitPoll()
	if wait := clock.nextWait(t); wait != 30*time.Second {
		t.Fatalf("pending wait = %s, expected the min backoff", wait)
	}
	if names, tlsCertificate := lifecycle.Certificate(); names != nil || tlsCertificate != nil {
		t.Fatal("a certificate was presented before one was issued")
	}
	clock.advance(30 * time.Second)
	api.waitPoll()
	if wait := clock.nextWait(t); wait != 60*time.Second {
		t.Fatalf("pending wait = %s, expected the doubled backoff", wait)
	}

	// issued: installed on the next read
	issueTime := clock.Now()
	// two thirds of it falls within the check timeout, so the read after the
	// install is the renewal's
	lifetime := 30 * time.Hour
	api.setCertResult(&extenderMeasurementCertResult{
		State:    ExtenderMeasurementCertStateIssued,
		Names:    names,
		ChainPem: ca.issue(t, csr, issueTime, issueTime.Add(lifetime)),
		NotAfter: issueTime.Add(lifetime).Format(time.RFC3339),
	})
	version, _ := certificate.installMonitor.Get()
	clock.advance(60 * time.Second)
	api.waitPoll()
	version = waitTestLagInstall(t, certificate, version)
	servedNames, tlsCertificate := lifecycle.Certificate()
	if !slices.Equal(servedNames, names) || tlsCertificate == nil {
		t.Fatalf("served names = %v, expected %v", servedNames, names)
	}
	if len(tlsCertificate.Certificate) != 2 || tlsCertificate.Leaf == nil {
		t.Fatal("the certificate does not carry the chain and its leaf")
	}
	if privateKey, ok := tlsCertificate.PrivateKey.(*ecdsa.PrivateKey); !ok || !privateKey.PublicKey.Equal(certificate.PublicKey()) {
		t.Fatal("the certificate does not present the measurement key")
	}
	firstSerial := tlsCertificate.Leaf.SerialNumber

	// the next read is at two thirds of the life, when the operator renews;
	// the read before it was 60 s into the life
	renewWait := issueTime.Add(lifetime * 2 / 3).Sub(clock.Now())
	if wait := clock.nextWait(t); wait != renewWait {
		t.Fatalf("installed wait = %s, expected %s to the renewal", wait, renewWait)
	}
	renewTime := issueTime.Add(lifetime * 2 / 3)
	api.setCertResult(&extenderMeasurementCertResult{
		State:    ExtenderMeasurementCertStateIssued,
		Names:    names,
		ChainPem: ca.issue(t, csr, renewTime, renewTime.Add(lifetime)),
	})
	clock.advance(renewWait)
	api.waitPoll()
	version = waitTestLagInstall(t, certificate, version)
	_, renewed := lifecycle.Certificate()
	if renewed == nil || renewed.Leaf.SerialNumber.Cmp(firstSerial) == 0 {
		t.Fatal("the renewal was not swapped in")
	}

	// a renewal the operator has not made yet is read again at a quarter of
	// the time left, and the certificate stops at its expiry
	clock.nextWait(t)
	expiryTime := renewTime.Add(lifetime)
	clock.advance(expiryTime.Add(-time.Hour).Sub(clock.Now()))
	api.waitPoll()
	if wait := clock.nextWait(t); wait != 30*time.Minute {
		t.Fatalf("near expiry wait = %s, expected the max backoff", wait)
	}
	if _, tlsCertificate := lifecycle.Certificate(); tlsCertificate == nil {
		t.Fatal("the certificate stopped before its expiry")
	}
	clock.advance(time.Hour)
	api.waitPoll()
	if names, tlsCertificate := lifecycle.Certificate(); names != nil || tlsCertificate != nil {
		t.Fatal("an expired certificate was presented")
	}
	// an expired certificate is dropped, and its schedule with it
	if wait := clock.nextWait(t); wait != 24*time.Hour {
		t.Fatalf("expired wait = %s, expected the check timeout", wait)
	}
}

// An activation that carries a request wakes the poll, which reads the
// operator once the post has landed (C4).
func TestLagCertOfferPollsSoon(t *testing.T) {
	clock := newTestLagClock()
	api := newTestLagApi(t)
	certificate := newTestLagCertificate(t)

	newTestLagLifecycle(t, api, clock, certificate, nil)
	api.waitPoll()
	// nothing is pending or installed: the next read is a day away
	if wait := clock.nextWait(t); wait != 24*time.Hour {
		t.Fatalf("idle wait = %s, expected the check timeout", wait)
	}

	names := []string{"198-51-100-7.extender.bringyour.com"}
	certificate.ActivationCsr([]netip.Addr{netip.MustParseAddr("198.51.100.7")})
	if wait := clock.nextWait(t); wait != 15*time.Second {
		t.Fatalf("offer wait = %s, expected the offer poll timeout", wait)
	}
	clock.advance(15 * time.Second)
	api.waitPoll()
	// the operator has not stored the request yet: read again on the backoff
	if wait := clock.nextWait(t); wait != 30*time.Second {
		t.Fatalf("unanswered offer wait = %s, expected the min backoff", wait)
	}
	api.setCertResult(&extenderMeasurementCertResult{
		State: ExtenderMeasurementCertStatePending,
		Names: names,
	})
	clock.advance(30 * time.Second)
	api.waitPoll()
	if wait := clock.nextWait(t); wait != 60*time.Second {
		t.Fatalf("pending wait = %s, expected the doubled backoff", wait)
	}

	// a refusal of exactly that request settles it: the next read is a day
	// away, and the next activation asks again
	api.setCertResult(&extenderMeasurementCertResult{
		State:  ExtenderMeasurementCertStateRefused,
		Names:  names,
		Reason: "the extender is gated",
	})
	clock.advance(60 * time.Second)
	api.waitPoll()
	if wait := clock.nextWait(t); wait != 24*time.Hour {
		t.Fatalf("refused wait = %s, expected the check timeout", wait)
	}
	if csr := certificate.ActivationCsr([]netip.Addr{netip.MustParseAddr("198.51.100.7")}); csr == "" {
		t.Fatal("a refused request was not asked for again")
	}
	api.expectQuiet()
}

// The whole of C4 with the real activation loop: the first activation cannot
// name the address the operator has not reported yet, so the pass that
// follows carries the request; the lifecycle polls once that post has landed,
// installs the chain the operator issued for it, and a later activation asks
// for nothing new while the operator holds the certificate.
func TestLagCertFromActivationToInstall(t *testing.T) {
	clock := newTestLagClock()
	api := newTestLagApi(t)
	ca := newTestLagCa(t)
	certificate := newTestLagCertificate(t)
	lifecycle := newTestLagLifecycle(t, api, clock, certificate, nil)
	api.waitPoll()
	if wait := clock.nextWait(t); wait != 24*time.Hour {
		t.Fatalf("idle wait = %s, expected the check timeout", wait)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.EnableResilient = false
	clientStrategy := connect.NewDirectClientStrategy(ctx, strategySettings, 0)
	defer clientStrategy.Close()
	// the activation loop's own clock never moves: only its triggers run it
	activatorNow := clock.Now()
	activatorSettings := connect.DefaultExtenderActivatorSettings()
	activatorSettings.ApiUrlV4 = api.server.URL
	activatorSettings.ByJwt = func() string {
		return testLagJwt
	}
	activatorSettings.ClientStrategy = clientStrategy
	activatorSettings.PublicKey = make([]byte, 32)
	activatorSettings.Carriers = func() []string {
		return []string{connect.ExtenderCarrierTcp}
	}
	activatorSettings.MeasurementCsr = certificate.ActivationCsr
	activatorSettings.Now = func() time.Time {
		return activatorNow
	}
	activatorSettings.PassAfter = func(wait time.Duration) <-chan time.Time {
		return nil
	}
	activatorSettings.ResumeCheckTimeout = 0
	activatorSettings.IpVersionSupported = func(ipVersion int) bool {
		return ipVersion == 4
	}
	var networkChanged func()
	activatorSettings.AddNetworkChangeListener = func(listener func()) func() {
		networkChanged = listener
		return func() {}
	}
	activator := connect.NewExtenderActivator(ctx, activatorSettings)
	defer activator.Close()

	if csr := api.waitActivation(); csr != "" {
		t.Fatalf("the first activation carried %q before its address was known", csr)
	}
	csr := api.waitActivation()
	request := parseTestLagCsr(t, csr)
	names := []string{"198-51-100-7.extender.bringyour.com"}
	if !slices.Equal(request.DNSNames, names) {
		t.Fatalf("request names = %v, expected %v", request.DNSNames, names)
	}

	// the request woke the poll, which reads once the post has landed
	if wait := clock.nextWait(t); wait != 15*time.Second {
		t.Fatalf("offer wait = %s, expected the offer poll timeout", wait)
	}
	clock.advance(15 * time.Second)
	api.waitPoll()
	if wait := clock.nextWait(t); wait != 30*time.Second {
		t.Fatalf("pending wait = %s, expected the min backoff", wait)
	}

	now := clock.Now()
	api.setCertResult(&extenderMeasurementCertResult{
		State:    ExtenderMeasurementCertStateIssued,
		Names:    names,
		ChainPem: ca.issue(t, csr, now, now.Add(90*24*time.Hour)),
	})
	version, _ := certificate.installMonitor.Get()
	clock.advance(30 * time.Second)
	api.waitPoll()
	waitTestLagInstall(t, certificate, version)
	servedNames, tlsCertificate := lifecycle.Certificate()
	if !slices.Equal(servedNames, names) || tlsCertificate == nil {
		t.Fatalf("served names = %v, expected %v", servedNames, names)
	}

	// the operator holds the certificate: a later activation asks for nothing
	networkChanged()
	if csr := api.waitActivation(); csr != "" {
		t.Fatalf("an activation asked again with %q for the installed names", csr)
	}
}

// A chain issued for another key, or for a name outside the measurement zone,
// is never installed, and the next activation asks for a request for this key.
func TestLagCertRefusesAChainItCannotServe(t *testing.T) {
	clock := newTestLagClock()
	api := newTestLagApi(t)
	ca := newTestLagCa(t)
	certificate := newTestLagCertificate(t)
	addrs := []netip.Addr{netip.MustParseAddr("198.51.100.7")}
	names := []string{"198-51-100-7.extender.bringyour.com"}

	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := clock.Now()
	api.setCertResult(&extenderMeasurementCertResult{
		State:    ExtenderMeasurementCertStateIssued,
		Names:    names,
		ChainPem: ca.issueFor(t, &otherKey.PublicKey, names, now, now.Add(90*time.Hour)),
	})
	lifecycle := newTestLagLifecycle(t, api, clock, certificate, nil)
	api.waitPoll()
	clock.nextWait(t)
	if _, tlsCertificate := lifecycle.Certificate(); tlsCertificate != nil {
		t.Fatal("a chain for another key was installed")
	}
	if csr := certificate.ActivationCsr(addrs); csr == "" {
		t.Fatal("no request was asked for after a chain for another key")
	}
	if wait := clock.nextWait(t); wait != 15*time.Second {
		t.Fatalf("offer wait = %s, expected the offer poll timeout", wait)
	}

	for _, chainNames := range [][]string{
		{"198-51-100-7.other.example"},
		{"198-51-100-7.extender.bringyour.com", "www.example.com"},
	} {
		api.setCertResult(&extenderMeasurementCertResult{
			State:    ExtenderMeasurementCertStateIssued,
			Names:    chainNames,
			ChainPem: ca.issueFor(t, certificate.PublicKey(), chainNames, now, now.Add(90*time.Hour)),
		})
		clock.advance(24 * time.Hour)
		api.waitPoll()
		clock.nextWait(t)
		if _, tlsCertificate := lifecycle.Certificate(); tlsCertificate != nil {
			t.Fatalf("a chain for %v was installed", chainNames)
		}
	}
}

// A lifecycle without settings presents no certificate and drops every report,
// and one whose server serves no feed mounts none (M10).
func TestLagCertWithoutSettings(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewExtenderServer(ctx, nil, nil, nil, &net.Dialer{}, DefaultExtenderSettings())
	defer server.CloseAndWait()

	lifecycle := newExtenderLagLifecycle(server, nil)
	if names, tlsCertificate := lifecycle.Certificate(); names != nil || tlsCertificate != nil {
		t.Fatal("a lifecycle without settings presented a certificate")
	}
	lifecycle.Submit(&protocol.LagReport{Body: []byte{1}})
	if handler := lifecycle.FeedHandler(); handler != nil {
		t.Fatal("a server without a feed mounted one")
	}

	feedSettings := DefaultExtenderSettings()
	feedSettings.FeedConnHandler = func(conn net.Conn) {}
	feedServer := NewExtenderServer(ctx, nil, nil, nil, &net.Dialer{}, feedSettings)
	defer feedServer.CloseAndWait()
	if handler := newExtenderLagLifecycle(feedServer, nil).FeedHandler(); handler == nil {
		t.Fatal("a server with a feed mounted none")
	}
}

// M10 as this side implements it, declared again here so these tests do not
// depend on the API side's declaration of lagCertificates and lagReports.
var _ interface {
	Certificate() (names []string, certificate *tls.Certificate)
	Submit(report *protocol.LagReport)
	FeedHandler() http.Handler
} = (*extenderLagLifecycle)(nil)
