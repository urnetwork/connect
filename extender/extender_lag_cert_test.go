package extender

// The measurement key and certificate of the lifecycle (lag.dev/PLAN.md C4,
// C6, A8, M10).
//
// The operator is an httptest server that answers the certificate route from
// a state the test sets, issues chains from a test certificate authority, and
// knows the extender by one id, which names its certificate (N1).
// The lifecycle reads a test clock and waits on it, so nothing sleeps: each
// wait the lifecycle starts is reported to the test, which moves the clock
// past it to fire it.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
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

// Waits for the lifecycle to start a wait of `expected`, passing over the
// waits it started before that one.
func (self *testLagClock) waitFor(t *testing.T, expected time.Duration) {
	t.Helper()
	for {
		if wait := self.nextWait(t); wait == expected {
			return
		}
	}
}

// Passes over every wait the lifecycle has started so far.
func (self *testLagClock) skipWaits() {
	for {
		select {
		case <-self.waits:
		default:
			return
		}
	}
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

	stateLock sync.Mutex
	// what the certificate route answers; nil answers 404
	certResult *extenderMeasurementCertResult
	// the identity key the last read of the certificate route named
	certPublicKeyHexValue string
	reportStatus          int
	reportResult          *extenderLagReportResult
	// called as each post of the report route comes, before it is answered
	beforeReportAnswer func()
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
		// every activation is of the one v4 address and answers the extender's
		// id (N4), and a request it carries is pending until the test issues
		// it (A10)
		args := &connect.ExtenderActivateArgs{}
		if err := json.NewDecoder(r.Body).Decode(args); err != nil {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		if args.MeasurementCsr != "" {
			request := parseTestLagCsr(self.t, args.MeasurementCsr)
			keySha256, err := extenderMeasurementKeySha256(request.PublicKey)
			if err != nil {
				self.t.Error(err)
			}
			self.setCertResult(&extenderMeasurementCertResult{
				State:     ExtenderMeasurementCertStatePending,
				Names:     request.DNSNames,
				KeySha256: keySha256,
			})
		}
		extenderId := testLagCertExtenderId
		json.NewEncoder(w).Encode(&connect.ExtenderActivateResult{
			Activated:  true,
			ExtenderId: &extenderId,
			Ip:         testLagActivateIp,
			IpVersion:  4,
			Carriers:   args.Carriers,
		})
		self.activationCsrs <- args.MeasurementCsr
	case r.Method == http.MethodGet && r.URL.Path == ExtenderMeasurementCertPath:
		self.stateLock.Lock()
		certResult := self.certResult
		self.certPublicKeyHexValue = r.URL.Query().Get("public_key_hex")
		self.stateLock.Unlock()
		if certResult == nil {
			w.WriteHeader(http.StatusNotFound)
		} else {
			json.NewEncoder(w).Encode(certResult)
		}
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
		beforeReportAnswer := self.beforeReportAnswer
		self.stateLock.Unlock()
		if beforeReportAnswer != nil {
			beforeReportAnswer()
		}
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

// The identity key the last read of the certificate route named.
func (self *testLagApi) certPublicKeyHex() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.certPublicKeyHexValue
}

func (self *testLagApi) setReportAnswer(reportStatus int, reportResult *extenderLagReportResult) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.reportStatus = reportStatus
	self.reportResult = reportResult
}

// Runs `beforeReportAnswer` as each post of the report route comes, before
// the post is answered and so before the lifecycle goes on; nil runs nothing.
func (self *testLagApi) setBeforeReportAnswer(beforeReportAnswer func()) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.beforeReportAnswer = beforeReportAnswer
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
	// an extender with an identity, which is what its reads name it by
	identityKeySeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	serverSettings := DefaultExtenderSettings()
	serverSettings.IdentityKeySeed = identityKeySeed
	server := NewExtenderServer(ctx, nil, nil, nil, &net.Dialer{}, serverSettings)

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
		current, change := certificate.ChangeMonitor().Get()
		if version < current {
			return current
		}
		select {
		case <-change:
		case <-time.After(30 * time.Second):
			t.Fatal("the lifecycle did not install or drop a certificate")
			return 0
		}
	}
}

// Records each activation pass the lifecycle asks for.
func testLagReactivations(certificate *ExtenderMeasurementCert) chan struct{} {
	reactivations := make(chan struct{}, 64)
	certificate.SetReactivate(func() {
		reactivations <- struct{}{}
	})
	return reactivations
}

// Checks the passes asked for since the last check. The asks happen in the
// poll, which is over by the time the lifecycle starts its next wait.
func testLagExpectReactivations(t *testing.T, reactivations chan struct{}, expectedCount int) {
	t.Helper()
	count := 0
	for {
		select {
		case <-reactivations:
			count += 1
			continue
		default:
		}
		break
	}
	if count != expectedCount {
		t.Fatalf("asked for %d passes, expected %d", count, expectedCount)
	}
}

// The address the test operator activates.
const testLagActivateIp = "198.51.100.7"

// The id the test operator knows this extender by, and its measurement name
// under the default zone (N1).
var testLagCertExtenderId = connect.RequireIdFromBytes([]byte{
	0x01, 0x92, 0xe4, 0xa1, 0x7b, 0x3c, 0x7d, 0x2e,
	0x9f, 0x10, 0x5a, 0x6b, 0x7c, 0x8d, 0x9e, 0x0f,
})

const testLagCertName = "0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.extender.bringyour.com"

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

// The request names exactly the extender's measurement name, its id under the
// zone (N1, C4): one name, nothing else in the request, and signed by the
// measurement key.
func TestLagCertCsrName(t *testing.T) {
	if name := connect.ExtenderMeasurementName(testLagCertExtenderId, ""); name != testLagCertName {
		t.Fatalf("measurement name = %s, expected %s", name, testLagCertName)
	}

	certificate := newTestLagCertificate(t)
	csr := certificate.ActivationCsr(testLagCertExtenderId)
	request := parseTestLagCsr(t, csr)
	if !slices.Equal(request.DNSNames, []string{testLagCertName}) {
		t.Fatalf("request names = %v, expected %s alone", request.DNSNames, testLagCertName)
	}
	if 0 < len(request.IPAddresses)+len(request.EmailAddresses)+len(request.URIs) || request.Subject.CommonName != "" {
		t.Fatalf("request = %+v, expected the one dns name alone", request)
	}
	if publicKey, ok := request.PublicKey.(*ecdsa.PublicKey); !ok || !publicKey.Equal(certificate.PublicKey()) {
		t.Fatal("the request is not for the measurement key")
	}
	if request.SignatureAlgorithm != x509.ECDSAWithSHA256 {
		t.Fatalf("request signature = %s", request.SignatureAlgorithm)
	}

	// the id never changes, so the request is made once and kept; no id asks
	// for nothing
	if again := certificate.ActivationCsr(testLagCertExtenderId); again != csr {
		t.Fatal("the same id was asked for with another request")
	}
	if csr := certificate.ActivationCsr(connect.Id{}); csr != "" {
		t.Fatalf("no id asked with %q", csr)
	}

	// a configured zone names under it
	zoned, err := NewExtenderMeasurementCert("", "Measure.Example.")
	if err != nil {
		t.Fatal(err)
	}
	request = parseTestLagCsr(t, zoned.ActivationCsr(testLagCertExtenderId))
	zonedName := "0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.measure.example"
	if !slices.Equal(request.DNSNames, []string{zonedName}) {
		t.Fatalf("zoned names = %v, expected %s", request.DNSNames, zonedName)
	}
}

// The operator's state decides whether an activation asks: nothing while the
// operator holds a request for the name, whatever the address the extender is
// at, and a request again once the operator holds none (C4).
func TestLagCertCsrAsksOnlyWhileTheOperatorHoldsNone(t *testing.T) {
	clock := newTestLagClock()
	api := newTestLagApi(t)
	certificate := newTestLagCertificate(t)
	reactivations := testLagReactivations(certificate)

	api.setCertResult(&extenderMeasurementCertResult{
		State:     ExtenderMeasurementCertStatePending,
		Names:     []string{testLagCertName},
		KeySha256: certificate.keySha256,
	})
	newTestLagLifecycle(t, api, clock, certificate, nil)
	api.waitPoll()
	clock.nextWait(t)
	if csr := certificate.ActivationCsr(testLagCertExtenderId); csr != "" {
		t.Fatalf("asked with %q for the name the operator holds a request for", csr)
	}
	if count := len(reactivations); count != 0 {
		t.Fatalf("asked for %d passes with the request held", count)
	}

	// refused: the operator holds nothing, and the next activation asks again
	api.setCertResult(&extenderMeasurementCertResult{
		State:     ExtenderMeasurementCertStateRefused,
		Names:     []string{testLagCertName},
		Reason:    "the extender has no public inbound address",
		KeySha256: certificate.keySha256,
	})
	clock.advance(30 * time.Second)
	api.waitPoll()
	clock.nextWait(t)
	request := parseTestLagCsr(t, certificate.ActivationCsr(testLagCertExtenderId))
	if !slices.Equal(request.DNSNames, []string{testLagCertName}) {
		t.Fatalf("request names = %v, expected %s", request.DNSNames, testLagCertName)
	}
}

// Held means the name and the key (A8): a request for the name under another
// key holds nothing for this one, which asks for a pass at once, once however
// often it is read; the request for this key, once the operator holds it, asks
// nothing.
func TestLagCertHeldMeansNameAndKey(t *testing.T) {
	clock := newTestLagClock()
	api := newTestLagApi(t)
	certificate := newTestLagCertificate(t)
	reactivations := testLagReactivations(certificate)
	otherKeySha256 := newTestLagCertificate(t).keySha256

	api.setCertResult(&extenderMeasurementCertResult{
		State:     ExtenderMeasurementCertStatePending,
		Names:     []string{testLagCertName},
		KeySha256: otherKeySha256,
	})
	newTestLagLifecycle(t, api, clock, certificate, nil)
	api.waitPoll()
	clock.nextWait(t)
	testLagExpectReactivations(t, reactivations, 1)
	if csr := certificate.ActivationCsr(testLagCertExtenderId); csr == "" {
		t.Fatal("no request for this key while the operator holds another key's")
	}
	// read again: the same mismatch asks no second pass
	clock.advance(30 * time.Second)
	api.waitPoll()
	clock.nextWait(t)
	testLagExpectReactivations(t, reactivations, 0)

	api.setCertResult(&extenderMeasurementCertResult{
		State:     ExtenderMeasurementCertStatePending,
		Names:     []string{testLagCertName},
		KeySha256: certificate.keySha256,
	})
	clock.advance(time.Hour)
	api.waitPoll()
	clock.nextWait(t)
	if csr := certificate.ActivationCsr(testLagCertExtenderId); csr != "" {
		t.Fatal("asked again for the request the operator holds for this key")
	}
	testLagExpectReactivations(t, reactivations, 0)
}

// A key replaced while its request is pending -- the key file lost, a new key
// made -- finds the operator holding the old key's request. The new key's
// request goes out with a pass asked for at once, and a chain issued for the
// old key is refused and followed by another pass, rather than either waiting
// for the 24 hour tick.
func TestLagCertKeyReplacedWhilePending(t *testing.T) {
	clock := newTestLagClock()
	api := newTestLagApi(t)
	ca := newTestLagCa(t)
	oldCertificate := newTestLagCertificate(t)
	oldCsr := oldCertificate.ActivationCsr(testLagCertExtenderId)
	certificate := newTestLagCertificate(t)
	reactivations := testLagReactivations(certificate)

	api.setCertResult(&extenderMeasurementCertResult{
		State:     ExtenderMeasurementCertStatePending,
		Names:     []string{testLagCertName},
		KeySha256: oldCertificate.keySha256,
	})
	lifecycle := newTestLagLifecycle(t, api, clock, certificate, nil)
	api.waitPoll()
	clock.nextWait(t)
	testLagExpectReactivations(t, reactivations, 1)
	request := parseTestLagCsr(t, certificate.ActivationCsr(testLagCertExtenderId))
	if publicKey, ok := request.PublicKey.(*ecdsa.PublicKey); !ok || !publicKey.Equal(certificate.PublicKey()) {
		t.Fatal("the request that went out is not for the new key")
	}
	// that request woke the poll
	if wait := clock.nextWait(t); wait != 15*time.Second {
		t.Fatalf("offer wait = %s, expected the offer poll timeout", wait)
	}

	// the operator issues for the old key's request before it has the new one
	now := clock.Now()
	api.setCertResult(&extenderMeasurementCertResult{
		State:     ExtenderMeasurementCertStateIssued,
		Names:     []string{testLagCertName},
		ChainPem:  ca.issue(t, oldCsr, now, now.Add(90*time.Hour)),
		KeySha256: oldCertificate.keySha256,
	})
	clock.advance(15 * time.Second)
	api.waitPoll()
	clock.nextWait(t)
	if _, tlsCertificate := lifecycle.Certificate(); tlsCertificate != nil {
		t.Fatal("a chain for the old key was installed")
	}
	testLagExpectReactivations(t, reactivations, 1)
	if csr := certificate.ActivationCsr(testLagCertExtenderId); csr == "" {
		t.Fatal("no request for the new key after a chain for the old one")
	}
}

// Opting out (SetEnabled) takes the certificate down at once and asks for no
// request, and nothing is read while it stands; opting back in reads the
// operator's state soon and serves the certificate it still holds.
func TestLagCertOptOut(t *testing.T) {
	installed := newTestLagInstalled(t)
	_, installedCertificate := installed.lifecycle.Certificate()
	version, _ := installed.certificate.ChangeMonitor().Get()

	installed.certificate.SetEnabled(false)
	if installed.certificate.Enabled() {
		t.Fatal("measurement still on after opting out")
	}
	if names, tlsCertificate := installed.lifecycle.Certificate(); names != nil || tlsCertificate != nil {
		t.Fatal("a certificate was served after opting out")
	}
	if current, _ := installed.certificate.ChangeMonitor().Get(); current == version {
		t.Fatal("opting out did not change the certificate the API watches")
	}
	if csr := installed.certificate.ActivationCsr(testLagCertExtenderId); csr != "" {
		t.Fatal("a request was asked for after opting out")
	}
	// the hourly read comes due and reads nothing
	installed.clock.advance(time.Hour)
	if wait := installed.clock.nextWait(t); wait != time.Hour {
		t.Fatalf("opted out wait = %s, expected the check timeout", wait)
	}
	installed.api.expectQuiet()

	// back on: the operator's state is read soon, and the certificate it still
	// holds is served again without a restart
	installed.certificate.SetEnabled(true)
	if wait := installed.clock.nextWait(t); wait != 15*time.Second {
		t.Fatalf("opted in wait = %s, expected the offer poll timeout", wait)
	}
	version, _ = installed.certificate.ChangeMonitor().Get()
	installed.clock.advance(15 * time.Second)
	installed.api.waitPoll()
	waitTestLagInstall(t, installed.certificate, version)
	if names, tlsCertificate := installed.lifecycle.Certificate(); !slices.Equal(names, []string{testLagCertName}) || tlsCertificate == nil ||
		!bytes.Equal(tlsCertificate.Certificate[0], installedCertificate.Certificate[0]) {
		t.Fatalf("served names = %v after opting back in, expected the held certificate", names)
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
	names := []string{testLagCertName}
	csr := certificate.ActivationCsr(testLagCertExtenderId)

	api.setCertResult(&extenderMeasurementCertResult{
		State: ExtenderMeasurementCertStatePending,
		Names: names,
	})
	lifecycle := newTestLagLifecycle(t, api, clock, certificate, func(settings *ExtenderLagLifecycleSettings) {
		// a day between idle reads, so the read after an install is the
		// renewal's rather than the hourly check
		settings.CertCheckTimeout = 24 * time.Hour
	})
	// the first read is at start, and a pending request is read again on the
	// backoff, which doubles
	api.waitPoll()
	if wait := clock.nextWait(t); wait != 30*time.Second {
		t.Fatalf("pending wait = %s, expected the min backoff", wait)
	}
	if names, tlsCertificate := lifecycle.Certificate(); names != nil || tlsCertificate != nil {
		t.Fatal("a certificate was presented before one was issued")
	}
	// the read names this extender, which a client that activated several
	// needs
	if publicKeyHex := api.certPublicKeyHex(); publicKeyHex != hex.EncodeToString(lifecycle.server.PublicKey()) {
		t.Fatalf("poll public key = %q, expected the extender's identity key", publicKeyHex)
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
	version, _ := certificate.ChangeMonitor().Get()
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

// A lifecycle with a certificate installed, its wait for the next read
// already taken.
type testLagInstalled struct {
	clock       *testLagClock
	api         *testLagApi
	certificate *ExtenderMeasurementCert
	lifecycle   *extenderLagLifecycle
	// what the operator answered to install it
	issued *extenderMeasurementCertResult
}

func newTestLagInstalled(t *testing.T) *testLagInstalled {
	t.Helper()
	clock := newTestLagClock()
	api := newTestLagApi(t)
	certificate := newTestLagCertificate(t)
	now := clock.Now()
	issued := &extenderMeasurementCertResult{
		State: ExtenderMeasurementCertStateIssued,
		Names: []string{testLagCertName},
		ChainPem: newTestLagCa(t).issue(
			t, certificate.ActivationCsr(testLagCertExtenderId), now, now.Add(90*24*time.Hour)),
		KeySha256: certificate.keySha256,
	}
	api.setCertResult(issued)
	version, _ := certificate.ChangeMonitor().Get()
	lifecycle := newTestLagLifecycle(t, api, clock, certificate, nil)
	api.waitPoll()
	waitTestLagInstall(t, certificate, version)
	// with a certificate installed and far from its renewal, the state is read
	// again every hour, which is how soon a withdrawal is seen
	if wait := clock.nextWait(t); wait != time.Hour {
		t.Fatalf("installed wait = %s, expected the check timeout", wait)
	}
	if names, tlsCertificate := lifecycle.Certificate(); !slices.Equal(names, []string{testLagCertName}) || tlsCertificate == nil {
		t.Fatalf("served names = %v, expected %s", names, testLagCertName)
	}
	return &testLagInstalled{
		clock:       clock,
		api:         api,
		certificate: certificate,
		lifecycle:   lifecycle,
		issued:      issued,
	}
}

// A withdrawn answer drops the installed certificate at once, so the extender
// stops answering to its measurement name, whichever name the answer carries;
// and the operator then holds nothing, so the next activation asks again (C9).
func TestLagCertWithdrawnDropsAtOnce(t *testing.T) {
	otherName := connect.ExtenderMeasurementName(connect.NewId(), "")
	for _, withdrawnNames := range [][]string{{testLagCertName}, nil, {otherName}} {
		installed := newTestLagInstalled(t)
		installed.api.setCertResult(&extenderMeasurementCertResult{
			State:  ExtenderMeasurementCertStateWithdrawn,
			Names:  withdrawnNames,
			Reason: "the extender is gated",
		})
		version, _ := installed.certificate.ChangeMonitor().Get()
		installed.clock.advance(time.Hour)
		installed.api.waitPoll()
		waitTestLagInstall(t, installed.certificate, version)
		if names, tlsCertificate := installed.lifecycle.Certificate(); names != nil || tlsCertificate != nil {
			t.Errorf("withdrawn %v: the certificate is still presented for %v", withdrawnNames, names)
		}
		if names, tlsCertificate := installed.certificate.Certificate(); names != nil || tlsCertificate != nil {
			t.Errorf("withdrawn %v: the certificate is still held for %v", withdrawnNames, names)
		}
		if wait := installed.clock.nextWait(t); wait != time.Hour {
			t.Errorf("withdrawn %v: wait = %s, expected the check timeout", withdrawnNames, wait)
		}
		if csr := installed.certificate.ActivationCsr(testLagCertExtenderId); csr == "" {
			t.Errorf("withdrawn %v: the next activation did not ask again", withdrawnNames)
		}
	}
}

// An installed, unexpired certificate is kept through a none, pending or
// refused answer, an answer in no known state and a read that failed, so a
// transient answer cannot take a valid one down; and the same chain answered
// again is not installed twice (C4).
func TestLagCertKeepsThroughATransientAnswer(t *testing.T) {
	for _, transient := range []*extenderMeasurementCertResult{
		{State: ExtenderMeasurementCertStateNone},
		{State: ExtenderMeasurementCertStatePending, Names: []string{testLagCertName}},
		{State: ExtenderMeasurementCertStateRefused, Names: []string{testLagCertName}, Reason: "the operator does not issue now"},
		{State: "unsettled"},
		// answered 404, which is a read that failed
		nil,
	} {
		installed := newTestLagInstalled(t)
		_, installedCertificate := installed.lifecycle.Certificate()
		version, _ := installed.certificate.ChangeMonitor().Get()

		installed.api.setCertResult(transient)
		installed.clock.advance(time.Hour)
		installed.api.waitPoll()
		// the wait the lifecycle starts next is after it took the answer in
		wait := installed.clock.nextWait(t)
		names, tlsCertificate := installed.lifecycle.Certificate()
		if !slices.Equal(names, []string{testLagCertName}) || tlsCertificate != installedCertificate {
			t.Errorf("transient %+v: served %v, expected the installed certificate kept", transient, names)
		}

		// the operator answers issued again: nothing changes
		installed.api.setCertResult(installed.issued)
		installed.clock.advance(wait)
		installed.api.waitPoll()
		installed.clock.nextWait(t)
		if _, tlsCertificate := installed.lifecycle.Certificate(); tlsCertificate != installedCertificate {
			t.Errorf("transient %+v: the same chain was installed again", transient)
		}
		if current, _ := installed.certificate.ChangeMonitor().Get(); current != version {
			t.Errorf("transient %+v: the certificate changed %d times", transient, current-version)
		}
	}
}

// An activation that carries a request wakes the poll, which reads the
// operator once the post has landed (C4).
func TestLagCertOfferPollsSoon(t *testing.T) {
	clock := newTestLagClock()
	api := newTestLagApi(t)
	certificate := newTestLagCertificate(t)
	names := []string{testLagCertName}

	newTestLagLifecycle(t, api, clock, certificate, nil)
	api.waitPoll()
	// nothing is pending or installed: the next read is the hourly check
	if wait := clock.nextWait(t); wait != time.Hour {
		t.Fatalf("idle wait = %s, expected the check timeout", wait)
	}

	certificate.ActivationCsr(testLagCertExtenderId)
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

	// a refusal of exactly that request settles it: the next read is the
	// hourly check, and the next activation asks again
	api.setCertResult(&extenderMeasurementCertResult{
		State:  ExtenderMeasurementCertStateRefused,
		Names:  names,
		Reason: "the extender is gated",
	})
	clock.advance(60 * time.Second)
	api.waitPoll()
	if wait := clock.nextWait(t); wait != time.Hour {
		t.Fatalf("refused wait = %s, expected the check timeout", wait)
	}
	if csr := certificate.ActivationCsr(testLagCertExtenderId); csr == "" {
		t.Fatal("a refused request was not asked for again")
	}
	api.expectQuiet()
}

// The whole of C4 with the real activation loop. A new identity learns its id
// from its first activation answer, so that post carries no request; with one
// post a pass, one more pass follows at once and carries the request for the
// one name of that id. The lifecycle polls once the post has landed and
// installs the chain the operator issued for it, and a later activation asks
// for nothing new while the operator holds the certificate.
func TestLagCertFromActivationToInstall(t *testing.T) {
	clock := newTestLagClock()
	api := newTestLagApi(t)
	ca := newTestLagCa(t)
	certificate := newTestLagCertificate(t)
	lifecycle := newTestLagLifecycle(t, api, clock, certificate, nil)
	api.waitPoll()
	if wait := clock.nextWait(t); wait != time.Hour {
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
		t.Fatalf("the first activation carried %q before its id was known", csr)
	}
	csr := api.waitActivation()
	request := parseTestLagCsr(t, csr)
	names := []string{testLagCertName}
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
	version, _ := certificate.ChangeMonitor().Get()
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
		t.Fatalf("an activation asked again with %q for the installed name", csr)
	}
}

// The key mismatch asks the real activation loop for a pass, which runs at once
// -- the activation's own clock never moves here -- and carries this key's
// request: when the operator's pending request turns out to be for an old key,
// and when it issues for that old key.
func TestLagCertKeyMismatchAsksForAPassNow(t *testing.T) {
	clock := newTestLagClock()
	api := newTestLagApi(t)
	ca := newTestLagCa(t)
	oldCertificate := newTestLagCertificate(t)
	oldCsr := oldCertificate.ActivationCsr(testLagCertExtenderId)
	certificate := newTestLagCertificate(t)

	api.setCertResult(&extenderMeasurementCertResult{
		State:     ExtenderMeasurementCertStatePending,
		Names:     []string{testLagCertName},
		KeySha256: certificate.keySha256,
	})
	newTestLagLifecycle(t, api, clock, certificate, nil)
	api.waitPoll()
	if wait := clock.nextWait(t); wait != 30*time.Second {
		t.Fatalf("pending wait = %s, expected the min backoff", wait)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.EnableResilient = false
	clientStrategy := connect.NewDirectClientStrategy(ctx, strategySettings, 0)
	defer clientStrategy.Close()
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
	activatorSettings.AddNetworkChangeListener = func(listener func()) func() {
		return func() {}
	}
	activator := connect.NewExtenderActivator(ctx, activatorSettings)
	defer activator.Close()
	certificate.SetReactivate(activator.Reactivate)

	// the operator holds this key's request: one post, and nothing follows it
	if csr := api.waitActivation(); csr != "" {
		t.Fatalf("the first activation carried %q", csr)
	}

	// the request it holds is an old key's after all
	api.setCertResult(&extenderMeasurementCertResult{
		State:     ExtenderMeasurementCertStatePending,
		Names:     []string{testLagCertName},
		KeySha256: oldCertificate.keySha256,
	})
	clock.advance(30 * time.Second)
	api.waitPoll()
	if csr := api.waitActivation(); csr == "" {
		t.Fatal("the pass asked for at once carried no request")
	} else if request := parseTestLagCsr(t, csr); !request.PublicKey.(*ecdsa.PublicKey).Equal(certificate.PublicKey()) {
		t.Fatal("the request that went out is not for this key")
	}

	// and the operator issues for that old key
	now := clock.Now()
	api.setCertResult(&extenderMeasurementCertResult{
		State:     ExtenderMeasurementCertStateIssued,
		Names:     []string{testLagCertName},
		ChainPem:  ca.issue(t, oldCsr, now, now.Add(90*time.Hour)),
		KeySha256: oldCertificate.keySha256,
	})
	// the request just carried woke the poll
	clock.waitFor(t, 15*time.Second)
	clock.advance(15 * time.Second)
	api.waitPoll()
	if csr := api.waitActivation(); csr == "" {
		t.Fatal("the pass after a chain for the old key carried no request")
	}
}

// A chain issued for another key, for a name outside the measurement zone, for
// a name under it that is no extender's, or for more than the one name, is
// never installed, and the next activation asks for a request for this key.
func TestLagCertRefusesAChainItCannotServe(t *testing.T) {
	clock := newTestLagClock()
	api := newTestLagApi(t)
	ca := newTestLagCa(t)
	certificate := newTestLagCertificate(t)
	names := []string{testLagCertName}

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
	if csr := certificate.ActivationCsr(testLagCertExtenderId); csr == "" {
		t.Fatal("no request was asked for after a chain for another key")
	}
	if wait := clock.nextWait(t); wait != 15*time.Second {
		t.Fatalf("offer wait = %s, expected the offer poll timeout", wait)
	}

	for _, chainNames := range [][]string{
		{"0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.other.example"},
		{"www.extender.bringyour.com"},
		{testLagCertName, connect.ExtenderMeasurementName(connect.NewId(), "")},
	} {
		api.setCertResult(&extenderMeasurementCertResult{
			State:    ExtenderMeasurementCertStateIssued,
			Names:    chainNames,
			ChainPem: ca.issueFor(t, certificate.PublicKey(), chainNames, now, now.Add(90*time.Hour)),
		})
		clock.advance(time.Hour)
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
