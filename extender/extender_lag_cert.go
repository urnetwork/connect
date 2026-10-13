package extender

// The measurement lifecycle of an extender (lag.dev/PLAN.md C4, M8, M9, M10).
//
// lag.dev measures latency and throughput from a visitor's browser to an
// extender through the measurement API (M1 to M7, extender_lag_api.go), which a
// browser reaches under the extender's measurement names (N) with a public
// certificate. This side keeps what outlives any one session:
//
//   - the measurement key and its certificate (C4). The key is ECDSA P-256 and
//     used for nothing else -- not the identity key, not the camouflage key --
//     and lives in `extender-measurement-key.pem`, mode 0600, beside the
//     extender key file. An activation carries a certificate request whose one
//     name is the extender's measurement name, its id under the measurement
//     zone (N1), which it learns from its activation answer (N4); the
//     lifecycle polls the operator until the certificate is issued, installs
//     the chain without a restart, and polls again for each renewal (C6). The
//     name outlives every address change, so an address change asks for
//     nothing. A certificate the operator withdraws is dropped at once (C9);
//   - the report queue (M8, extender_lag_report.go), which posts what each
//     session measured under the activation credential;
//   - the feed over websocket (M9, extender_lag_feed.go).
//
// The role makes the ExtenderMeasurementCert and hands it to the two sides that
// need it: the activation, whose MeasurementCsr hook is ActivationCsr, and the
// server, through ExtenderLagLifecycleSettings, whose lifecycle polls the
// operator and presents the certificate to the API's dispatch (M1). Without
// settings the lifecycle presents no certificate and posts nothing, which is
// every extender with measurement off.
//
// Everything here is safe for concurrent use. The lifecycle's loops run as
// workers of the extender server, which ends them at Close and joins them at
// CloseAndWait.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// The measurement key file (C4), beside the extender key file.
const ExtenderMeasurementKeyFileName = "extender-measurement-key.pem"

// The certificate route (A8), appended to the api url.
const ExtenderMeasurementCertPath = "/network/extender-measurement-cert"

// The states the certificate route answers (A8).
const (
	ExtenderMeasurementCertStateNone    = "none"
	ExtenderMeasurementCertStatePending = "pending"
	ExtenderMeasurementCertStateIssued  = "issued"
	ExtenderMeasurementCertStateRefused = "refused"
	// the operator withdrew the certificate (C9): the extender was deactivated,
	// or overridden to the gated tier or a canary
	ExtenderMeasurementCertStateWithdrawn = "withdrawn"
)

// The measurement key file beside an extender key file (C4).
func ExtenderMeasurementKeyPath(extenderKeyFile string) string {
	return filepath.Join(filepath.Dir(extenderKeyFile), ExtenderMeasurementKeyFileName)
}

// The measurement key and the certificate issued for it (C4): what the
// activation asks for and what the API presents. The role makes one and hands
// it to the activation (ActivationCsr) and to the server
// (ExtenderLagLifecycleSettings.Certificate), whose lifecycle fills it from the
// operator. Safe for concurrent use.
type ExtenderMeasurementCert struct {
	key     *ecdsa.PrivateKey
	keyFile string
	zone    string
	// hex sha-256 of the key's SubjectPublicKeyInfo DER, which is how the
	// operator says which key it holds a request or a certificate for (A8)
	keySha256 string

	// whether the owner opted out of measurement (SetEnabled); while it stands
	// no certificate is served and no request is asked for
	disabled atomic.Bool

	// wakes the lifecycle's poll when an activation carries a request, or
	// measurement is turned back on
	offerMonitor *connect.Monitor
	// the installed certificate, swapped whole so a handshake never sees half
	// of one (M1)
	installed atomic.Pointer[extenderMeasurementInstalled]
	// counts every install and removal (ChangeMonitor)
	installMonitor *connect.MonitorValue[uint64]

	stateLock sync.Mutex
	// the names the operator holds a request or a certificate for under this
	// key -- its names and its key both -- from the last poll. Nil while it
	// holds none, or one for another key, or before a poll has answered.
	heldNames []string
	// the names of the last request an activation carried, until the operator
	// answers for them
	offeredNames []string
	// the request for csrNames, kept so each pass does not sign anew
	csrNames []string
	csr      string
	// asks the activation for a pass at once (SetReactivate); nil asks nothing
	reactivate func()
}

// One installed certificate.
type extenderMeasurementInstalled struct {
	names       []string
	certificate *tls.Certificate
	notBefore   time.Time
	notAfter    time.Time
}

// Loads the measurement key from `keyFile`, or makes one and keeps it there
// (C4), for the measurement names under `zone` (N1; empty is
// connect.ExtenderMeasurementZone). An empty path keeps the key in memory for
// the life of the process, which is all a role with an ephemeral identity
// can do.
func NewExtenderMeasurementCert(keyFile string, zone string) (*ExtenderMeasurementCert, error) {
	key, err := loadExtenderMeasurementKey(keyFile)
	if err != nil {
		return nil, err
	}
	zone = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(zone), "."))
	if zone == "" {
		zone = connect.ExtenderMeasurementZone
	}
	keySha256, err := extenderMeasurementKeySha256(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	return &ExtenderMeasurementCert{
		key:            key,
		keyFile:        keyFile,
		zone:           zone,
		keySha256:      keySha256,
		offerMonitor:   connect.NewMonitor(),
		installMonitor: connect.NewMonitorValue[uint64](0),
	}, nil
}

// How the operator names a key (A8): the hex sha-256 of its
// SubjectPublicKeyInfo DER.
func extenderMeasurementKeySha256(publicKey any) (string, error) {
	publicKeyDer, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return "", err
	}
	keySha256 := sha256.Sum256(publicKeyDer)
	return hex.EncodeToString(keySha256[:]), nil
}

// Sets whether this extender serves the measurement API (lag.dev/PLAN.md C4,
// decision 18), which is on until the owner opts out. Off takes the
// certificate down at once -- Certificate answers nil, so the dispatch admits
// no new session, and the API closes the open ones on the change -- and asks
// for no request; the activation then says measurement_disabled, and the
// operator withdraws the name (C9). On again asks for the request at the next
// activation: an operator that withdrew the name issues it again, as a renewal
// of the same name, and one that never heard of the opt-out holds the request
// already. Neither waits for a restart.
func (self *ExtenderMeasurementCert) SetEnabled(enabled bool) {
	if self.disabled.Swap(!enabled) == !enabled {
		return
	}
	// what the operator held may be withdrawn by now: ask again once on
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.heldNames = nil
		self.offeredNames = nil
	}()
	if enabled {
		// read the operator's state soon, in case it never withdrew
		self.offerMonitor.NotifyAll()
	} else {
		self.drop()
	}
	self.installMonitor.Update(func(version uint64) uint64 {
		return version + 1
	})
}

// Whether this extender serves the measurement API: true until the owner opts
// out (SetEnabled).
func (self *ExtenderMeasurementCert) Enabled() bool {
	return !self.disabled.Load()
}

// Sets what asks the activation for a pass at once (C4): the role's
// connect.ExtenderActivator.Reactivate, once its activator exists. The
// lifecycle asks when the operator holds a request or a certificate for
// another key, so this key's request goes out now rather than at the next
// tick. Nil asks for nothing.
func (self *ExtenderMeasurementCert) SetReactivate(reactivate func()) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.reactivate = reactivate
}

// Asks for an activation pass at once, when something can.
func (self *ExtenderMeasurementCert) requestReactivate() {
	reactivate := func() func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.reactivate
	}()
	if reactivate != nil {
		reactivate()
	}
}

// The zone the measurement names are under.
func (self *ExtenderMeasurementCert) Zone() string {
	return self.zone
}

// The file the measurement key is kept in, empty for a key in memory.
func (self *ExtenderMeasurementCert) KeyFile() string {
	return self.keyFile
}

// The measurement key's public half.
func (self *ExtenderMeasurementCert) PublicKey() *ecdsa.PublicKey {
	return &self.key.PublicKey
}

// The activation's MeasurementCsr hook (connect.ExtenderActivatorSettings, C4):
// the standard base64 of the DER certificate request whose one name is the
// measurement name of `extenderId` (N1), this extender's id as the operator's
// activation answer reported it (N4), signed by the measurement key, which
// never leaves the extender. "" for the zero id, which has no name; while the
// owner has opted out; and when the operator already holds a request or a
// certificate for that name under this key, so a routine activation asks for
// nothing new. The request is made
// once and kept, since the id never changes. Being asked for a request wakes
// the lifecycle's poll, which reads what the operator made of it once the post
// has landed.
func (self *ExtenderMeasurementCert) ActivationCsr(extenderId connect.Id) string {
	if self.disabled.Load() {
		// opted out: the activation says so instead (SetEnabled)
		return ""
	}
	name := connect.ExtenderMeasurementName(extenderId, self.zone)
	if name == "" {
		return ""
	}
	names := []string{name}
	held, csr := func() (bool, string) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if slices.Equal(self.heldNames, names) {
			return true, ""
		}
		if slices.Equal(self.csrNames, names) {
			return false, self.csr
		}
		return false, ""
	}()
	if held {
		return ""
	}
	if csr == "" {
		// signed outside the lock, which nothing waits on
		var err error
		if csr, err = newExtenderMeasurementCsr(self.key, names); err != nil {
			log.Printf("[extender] lag: measurement certificate request err = %s", err)
			return ""
		}
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.csrNames = names
		self.csr = csr
		if !slices.Equal(self.offeredNames, names) {
			// a new offer; the poll is already reading for one outstanding
			self.offeredNames = names
			self.offerMonitor.NotifyAll()
		}
	}()
	return csr
}

// A counter of every install and removal of the certificate, so a role can
// say what the extender serves when it changes: read Certificate and wait on
// the channel the same Get returned.
func (self *ExtenderMeasurementCert) ChangeMonitor() *connect.MonitorValue[uint64] {
	return self.installMonitor
}

// The installed certificate and the one name it serves (M10's
// lagCertificates), or nil when none is installed, it has expired, the
// operator withdrew it, or the owner opted out.
func (self *ExtenderMeasurementCert) Certificate() (names []string, certificate *tls.Certificate) {
	return self.certificateAt(time.Now())
}

func (self *ExtenderMeasurementCert) certificateAt(now time.Time) ([]string, *tls.Certificate) {
	if self.disabled.Load() {
		return nil, nil
	}
	installed := self.installed.Load()
	if installed == nil || !now.Before(installed.notAfter) {
		return nil, nil
	}
	return slices.Clone(installed.names), installed.certificate
}

// What the operator answered for this key: `heldNames`, the names it holds a
// request or a certificate for, nil for none, so an activation asks again only
// for other names; and `answeredNames`, the names its answer was about, which
// settle the offered request when they are its names -- an issued
// certificate, a pending request or a refusal of exactly that request.
func (self *ExtenderMeasurementCert) setHeld(heldNames []string, answeredNames []string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.heldNames = slices.Clone(heldNames)
	if 0 < len(answeredNames) && slices.Equal(self.offeredNames, answeredNames) {
		self.offeredNames = nil
	}
}

// Whether a request was offered that the operator has not answered for.
func (self *ExtenderMeasurementCert) offered() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return 0 < len(self.offeredNames)
}

// Forgets the offer after the poll has waited for it as long as it will: the
// post that carried it may never have landed, and the next activation offers
// again.
func (self *ExtenderMeasurementCert) dropOffer() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.offeredNames = nil
}

// Installs a checked certificate, replacing the one installed (C4, C6).
// Reports whether it was new.
func (self *ExtenderMeasurementCert) install(installed *extenderMeasurementInstalled) bool {
	current := self.installed.Load()
	if current != nil && slices.Equal(current.certificate.Certificate[0], installed.certificate.Certificate[0]) {
		return false
	}
	self.installed.Store(installed)
	self.installMonitor.Update(func(version uint64) uint64 {
		return version + 1
	})
	return true
}

// The installed certificate, nil when none is.
func (self *ExtenderMeasurementCert) installedCertificate() *extenderMeasurementInstalled {
	return self.installed.Load()
}

// Removes the installed certificate once it has expired, so its schedule is
// read no more.
func (self *ExtenderMeasurementCert) dropExpired(now time.Time) {
	installed := self.installed.Load()
	if installed == nil || now.Before(installed.notAfter) {
		return
	}
	if self.installed.CompareAndSwap(installed, nil) {
		self.installMonitor.Update(func(version uint64) uint64 {
			return version + 1
		})
	}
}

// Removes the installed certificate at once, so the next handshake for its
// name is no longer the API's (C9), and returns it; nil when none was.
func (self *ExtenderMeasurementCert) drop() *extenderMeasurementInstalled {
	installed := self.installed.Swap(nil)
	if installed != nil {
		self.installMonitor.Update(func(version uint64) uint64 {
			return version + 1
		})
	}
	return installed
}

// Checks a chain the operator issued (A8) for this key and zone: PEM
// certificates, leaf first, each signed by the next; the leaf for this key,
// unexpired, and named with one measurement name of the zone and nothing
// else, so the dispatch never presents it for anything else (M1).
func (self *ExtenderMeasurementCert) parseChain(chainPem string, now time.Time) (*extenderMeasurementInstalled, error) {
	certificateDers := [][]byte{}
	certificates := []*x509.Certificate{}
	rest := []byte(chainPem)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("the chain holds a %q block", block.Type)
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		certificateDers = append(certificateDers, block.Bytes)
		certificates = append(certificates, certificate)
	}
	if len(certificates) == 0 {
		return nil, fmt.Errorf("the chain holds no certificate")
	}
	leaf := certificates[0]
	if leafKey, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok || !leafKey.Equal(&self.key.PublicKey) {
		return nil, errExtenderMeasurementKeyMismatch
	}
	if !now.Before(leaf.NotAfter) {
		return nil, fmt.Errorf("the certificate expired at %s", leaf.NotAfter.Format(time.RFC3339))
	}
	if len(leaf.DNSNames) != 1 || 0 < len(leaf.IPAddresses)+len(leaf.EmailAddresses)+len(leaf.URIs) {
		return nil, fmt.Errorf("the certificate is not for one measurement name alone")
	}
	if _, ok := connect.ParseExtenderMeasurementName(leaf.DNSNames[0], self.zone); !ok {
		return nil, fmt.Errorf("%q is not a measurement name under %s", leaf.DNSNames[0], self.zone)
	}
	for i := 0; i+1 < len(certificates); i += 1 {
		if err := certificates[i].CheckSignatureFrom(certificates[i+1]); err != nil {
			return nil, fmt.Errorf("the chain is broken at %d: %w", i, err)
		}
	}
	return &extenderMeasurementInstalled{
		names: slices.Clone(leaf.DNSNames),
		certificate: &tls.Certificate{
			Certificate: certificateDers,
			PrivateKey:  self.key,
			Leaf:        leaf,
		},
		notBefore: leaf.NotBefore,
		notAfter:  leaf.NotAfter,
	}, nil
}

var errExtenderMeasurementKeyMismatch = errors.New("the certificate is for another key")

// A certificate request for exactly `names`, in the subject alternative names
// and nowhere else, as standard base64 of its DER (C4).
func newExtenderMeasurementCsr(key *ecdsa.PrivateKey, names []string) (string, error) {
	csrBytes, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		SignatureAlgorithm: x509.ECDSAWithSHA256,
		DNSNames:           names,
	}, key)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(csrBytes), nil
}

// Loads the measurement key, or makes one and keeps it at `keyFile` (C4). The
// file is PKCS #8 PEM with mode 0600, written whole to a temporary file beside
// it and linked into place, so a crash leaves either no key file or a complete
// one, and two makers never replace each other's key. An empty path keeps the
// key in memory only.
func loadExtenderMeasurementKey(keyFile string) (*ecdsa.PrivateKey, error) {
	if keyFile == "" {
		return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	key, err := readExtenderMeasurementKey(keyFile)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return key, err
	}

	key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPem := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	if err := os.MkdirAll(filepath.Dir(keyFile), 0700); err != nil {
		return nil, err
	}
	// CreateTemp makes the file 0600
	tempFile, err := os.CreateTemp(filepath.Dir(keyFile), "."+ExtenderMeasurementKeyFileName+".*")
	if err != nil {
		return nil, err
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)
	if _, err := tempFile.Write(keyPem); err != nil {
		tempFile.Close()
		return nil, err
	}
	if err := tempFile.Sync(); err != nil {
		tempFile.Close()
		return nil, err
	}
	if err := tempFile.Close(); err != nil {
		return nil, err
	}
	if err := os.Link(tempPath, keyFile); err != nil {
		if errors.Is(err, fs.ErrExist) {
			// another maker kept its key first; that one is the key
			return readExtenderMeasurementKey(keyFile)
		}
		// a file system without hard links
		if err := os.Rename(tempPath, keyFile); err != nil {
			return nil, err
		}
	}
	// the new name is durable once its directory is; a platform that cannot
	// sync a directory keeps what it keeps
	if directory, err := os.Open(filepath.Dir(keyFile)); err == nil {
		directory.Sync()
		directory.Close()
	}
	return key, nil
}

// Reads a kept measurement key: an ECDSA P-256 key in PKCS #8 or SEC 1 PEM. A
// file readable beyond its owner is narrowed to 0600 (C4).
func readExtenderMeasurementKey(keyFile string) (*ecdsa.PrivateKey, error) {
	keyPem, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(keyPem)
	if block == nil {
		return nil, fmt.Errorf("%s holds no pem block", keyFile)
	}
	var parsedKey any
	switch block.Type {
	case "PRIVATE KEY":
		parsedKey, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		parsedKey, err = x509.ParseECPrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("%s holds a %q block, not a private key", keyFile, block.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", keyFile, err)
	}
	key, ok := parsedKey.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%s is not an ECDSA P-256 key", keyFile)
	}
	if info, err := os.Stat(keyFile); err == nil && info.Mode().Perm()&0077 != 0 {
		if err := os.Chmod(keyFile, 0600); err != nil {
			log.Printf("[extender] lag: measurement key %s is readable beyond its owner: %s", keyFile, err)
		}
	}
	return key, nil
}

// The role's settings of the measurement lifecycle (M10).
type ExtenderLagLifecycleSettings struct {
	// The measurement key and certificate (C4), which the role also hands to
	// the activation as ExtenderActivatorSettings.MeasurementCsr =
	// Certificate.ActivationCsr. Nil polls for no certificate, so the API never
	// serves. The role makes one only when `measurement.enabled` is set and
	// the extender keeps its identity in a file, since a certificate publishes
	// the extender's measurement name (§2) and an identity that is new at every
	// start would ask for a new one each time.
	Certificate *ExtenderMeasurementCert

	// The plain api url the certificate is read from (A8) and reports are
	// posted to (A7). Empty does neither.
	ApiUrl string
	// The activation credential: the client jwt the activation posts with,
	// read at each request so a refresh is picked up.
	ByJwt func() string
	// The strategy every request goes through, the activation's direct-only
	// one. Nil does no request.
	ClientStrategy *connect.ClientStrategy
	// Budget of one request, a poll or a post.
	RequestTimeout time.Duration

	// The certificate poll (C4, C6, A8). A pending request, and a read that
	// failed, are polled again on a backoff from the min doubling to the max.
	// A request an activation carries is first polled CertOfferPollTimeout
	// after it was asked for, once the post has landed. With nothing pending
	// the state is read again at least every CertCheckTimeout, which is how a
	// withdrawal is seen and so bounds how long the extender goes on answering
	// to a withdrawn name (C9); an installed certificate is read again at two
	// thirds of its life, when the operator renews it, and from then on at a
	// quarter of the time it has left, but never further apart than
	// CertCheckTimeout or closer than CertPollMaxBackoff.
	CertPollMinBackoff   time.Duration
	CertPollMaxBackoff   time.Duration
	CertOfferPollTimeout time.Duration
	CertCheckTimeout     time.Duration

	// The report queue (M8). A batch of up to ReportMaxBatchCount reports is
	// posted when it is full or ReportFlushTimeout after its first report;
	// beyond ReportMaxPendingCount the oldest report is dropped. A post that
	// failed is retried on a backoff from the min doubling to the max.
	ReportMaxBatchCount   int
	ReportMaxPendingCount int
	ReportFlushTimeout    time.Duration
	ReportMinBackoff      time.Duration
	ReportMaxBackoff      time.Duration

	// The only clock the lifecycle reads, and its waits. Tests install a fake
	// pair. Nil is time.Now and time.After.
	Now   func() time.Time
	After func(wait time.Duration) <-chan time.Time
}

func DefaultExtenderLagLifecycleSettings() *ExtenderLagLifecycleSettings {
	return &ExtenderLagLifecycleSettings{
		RequestTimeout: 30 * time.Second,

		CertPollMinBackoff:   30 * time.Second,
		CertPollMaxBackoff:   30 * time.Minute,
		CertOfferPollTimeout: 15 * time.Second,
		CertCheckTimeout:     1 * time.Hour,

		ReportMaxBatchCount:   32,
		ReportMaxPendingCount: 256,
		ReportFlushTimeout:    10 * time.Second,
		ReportMinBackoff:      15 * time.Second,
		ReportMaxBackoff:      15 * time.Minute,
	}
}

// The lifecycle side of M10: the certificate the API presents
// (lagCertificates), the queue its reports go to (lagReports), and the feed it
// mounts. A lifecycle without settings presents no certificate and drops every
// report. Safe for concurrent use.
type extenderLagLifecycle struct {
	server   *ExtenderServer
	settings *ExtenderLagLifecycleSettings
	// nil when the role gave none, or there is no api to poll
	certificate *ExtenderMeasurementCert
	// nil when there is no api to post to
	reporter *extenderLagReporter
	// nil when the server serves no feed
	feedHandler http.Handler
	// the last key mismatch an activation pass was asked for, so a mismatch
	// that stands asks once. Read and written by the poll loop alone.
	keyMismatch string
}

// Builds the lifecycle of a server (lag.dev/PLAN.md M10), the lifecycle side's
// one wiring function, which the API side calls from NewExtenderServer:
//
//	lagLifecycle := newExtenderLagLifecycle(self, settings.LagLifecycle)
//	newExtenderLagApi(self, ..., lagLifecycle, lagLifecycle, lagLifecycle.FeedHandler())
//
// `settings` is the role's, nil for none. Never nil, so it is always safe to
// pass as both interfaces. The poll and the report loops start here as
// workers of the server, so Close ends them and CloseAndWait joins them.
func newExtenderLagLifecycle(server *ExtenderServer, settings *ExtenderLagLifecycleSettings) *extenderLagLifecycle {
	self := &extenderLagLifecycle{
		server: server,
	}
	if feedConnHandler := server.settings.FeedConnHandler; feedConnHandler != nil {
		self.feedHandler = newExtenderLagFeedHandler(server, feedConnHandler)
	}
	if settings == nil {
		return self
	}

	copied := *settings
	defaults := DefaultExtenderLagLifecycleSettings()
	if copied.RequestTimeout <= 0 {
		copied.RequestTimeout = defaults.RequestTimeout
	}
	if copied.CertPollMinBackoff <= 0 {
		copied.CertPollMinBackoff = defaults.CertPollMinBackoff
	}
	if copied.CertPollMaxBackoff < copied.CertPollMinBackoff {
		copied.CertPollMaxBackoff = max(copied.CertPollMinBackoff, defaults.CertPollMaxBackoff)
	}
	if copied.CertOfferPollTimeout <= 0 {
		copied.CertOfferPollTimeout = defaults.CertOfferPollTimeout
	}
	if copied.CertCheckTimeout < copied.CertPollMaxBackoff {
		copied.CertCheckTimeout = max(copied.CertPollMaxBackoff, defaults.CertCheckTimeout)
	}
	if copied.ReportMaxBatchCount <= 0 {
		copied.ReportMaxBatchCount = defaults.ReportMaxBatchCount
	}
	if copied.ReportMaxPendingCount < copied.ReportMaxBatchCount {
		copied.ReportMaxPendingCount = max(copied.ReportMaxBatchCount, defaults.ReportMaxPendingCount)
	}
	if copied.ReportFlushTimeout <= 0 {
		copied.ReportFlushTimeout = defaults.ReportFlushTimeout
	}
	if copied.ReportMinBackoff <= 0 {
		copied.ReportMinBackoff = defaults.ReportMinBackoff
	}
	if copied.ReportMaxBackoff < copied.ReportMinBackoff {
		copied.ReportMaxBackoff = max(copied.ReportMinBackoff, defaults.ReportMaxBackoff)
	}
	if copied.Now == nil {
		copied.Now = time.Now
	}
	if copied.After == nil {
		copied.After = time.After
	}
	self.settings = &copied

	if strings.TrimSpace(copied.ApiUrl) == "" || copied.ClientStrategy == nil {
		log.Printf("[extender] lag: the lifecycle has no api to poll or post to")
		return self
	}
	self.reporter = newExtenderLagReporter(server, self.settings)
	if copied.Certificate != nil && server.beginWorker() {
		self.certificate = copied.Certificate
		go func() {
			defer server.endWorker()
			connect.HandleError(self.run)
		}()
	}
	return self
}

// The installed certificate and the one name it serves now (lagCertificates),
// or nil when none is installed, it has expired, or the operator withdrew it.
func (self *extenderLagLifecycle) Certificate() (names []string, certificate *tls.Certificate) {
	if self == nil || self.certificate == nil {
		return nil, nil
	}
	return self.certificate.certificateAt(self.settings.Now())
}

// Queues one signed report for the operator (lagReports, M8). It never blocks;
// without an api to post to the report is dropped.
func (self *extenderLagLifecycle) Submit(report *protocol.LagReport) {
	if self == nil || self.reporter == nil {
		return
	}
	self.reporter.submit(report)
}

// The feed over websocket (M9), mounted by the API at its feed path, which
// checks the origin before it is reached. Nil when the server serves no feed.
func (self *extenderLagLifecycle) FeedHandler() http.Handler {
	if self == nil || self.feedHandler == nil {
		return nil
	}
	return self.feedHandler
}

// The certificate poll (C4, C6, C9, A8). It reads the operator's state at
// start; when an activation carries a request, once the post has landed; on a
// backoff while a request is pending, a read fails, or an offered request has
// not been answered for; ahead of each renewal of the installed certificate;
// and at least every CertCheckTimeout otherwise. A newly issued chain is
// installed at once and a withdrawn one dropped at once, each a swap the next
// handshake sees.
func (self *extenderLagLifecycle) run() {
	settings := self.settings
	backoff := settings.CertPollMinBackoff
	nextPollTime := settings.Now()
	for {
		offer := self.certificate.offerMonitor.NotifyChannel()
		if now := settings.Now(); !now.Before(nextPollTime) {
			nextPollTime, backoff = self.poll(now, backoff)
		}
		wait := nextPollTime.Sub(settings.Now())
		if wait <= 0 {
			// a poll is never closer than the min backoff, whatever came due
			wait = settings.CertPollMinBackoff
		}
		select {
		case <-self.server.ctx.Done():
			return
		case <-offer:
			// an activation is carrying a request: read what the operator made
			// of it once the post has landed, and then as for a pending one
			if offerPollTime := settings.Now().Add(settings.CertOfferPollTimeout); offerPollTime.Before(nextPollTime) {
				nextPollTime = offerPollTime
			}
			backoff = settings.CertPollMinBackoff
		case <-settings.After(wait):
		}
	}
}

// One read of the operator's state and what follows from it: when to read
// again, and the backoff after that.
func (self *extenderLagLifecycle) poll(now time.Time, backoff time.Duration) (time.Time, time.Duration) {
	settings := self.settings
	nextBackoff := min(2*backoff, settings.CertPollMaxBackoff)
	self.certificate.dropExpired(now)
	if !self.certificate.Enabled() {
		// opted out: nothing is served or asked for, so there is nothing to
		// read until the owner turns measurement back on, which wakes this
		return now.Add(settings.CertCheckTimeout), settings.CertPollMinBackoff
	}

	result, err := self.getCertificate()
	if err != nil {
		// a read the close interrupted is no failure worth a line
		if self.server.ctx.Err() == nil {
			log.Printf("[extender] lag: measurement certificate poll err = %s", err)
		}
		return now.Add(backoff), nextBackoff
	}

	switch result.State {
	case ExtenderMeasurementCertStatePending:
		if result.KeySha256 != self.certificate.keySha256 {
			// a request for another key -- one this extender made before its key
			// was replaced -- holds nothing for this key, whose request goes
			// out with a pass now
			self.certificate.setHeld(nil, nil)
			self.askForKey("request pending", result.KeySha256)
			return now.Add(backoff), nextBackoff
		}
		self.keyMismatch = ""
		self.certificate.setHeld(result.Names, result.Names)
		return now.Add(backoff), nextBackoff

	case ExtenderMeasurementCertStateIssued:
		installed, err := self.certificate.parseChain(result.ChainPem, now)
		if err != nil {
			// a chain this key cannot serve is of no use: the operator needs a
			// request for this key, which a chain for another key asks for
			// with a pass now
			log.Printf("[extender] lag: measurement certificate not installed = %s", err)
			self.certificate.setHeld(nil, nil)
			if errors.Is(err, errExtenderMeasurementKeyMismatch) {
				self.askForKey("certificate issued", result.KeySha256)
			}
			return self.idlePollTime(now, backoff)
		}
		self.keyMismatch = ""
		self.certificate.setHeld(installed.names, installed.names)
		if self.certificate.install(installed) {
			log.Printf(
				"[extender] lag: measurement certificate installed for %s until %s",
				strings.Join(installed.names, ","),
				installed.notAfter.Format(time.RFC3339),
			)
		}
		nextPollTime := self.installedPollTime(self.certificate.installedCertificate(), now)
		if self.certificate.offered() {
			// a request for other names is out: keep reading for it, serving
			// this certificate meanwhile
			if offerPollTime := now.Add(backoff); offerPollTime.Before(nextPollTime) {
				return offerPollTime, nextBackoff
			}
		}
		return nextPollTime, settings.CertPollMinBackoff

	case ExtenderMeasurementCertStateWithdrawn:
		// the operator withdrew the certificate (C9): the extender was
		// deactivated, or overridden to the gated tier or a canary, and must
		// stop answering to the name the certificate logs list. Whatever is
		// installed goes at once, whichever name the answer carries: while the
		// operator answers withdrawn it publishes nothing for this extender.
		// Nothing is held, so the next activation asks again, and an extender
		// that qualifies again is issued a new one.
		if dropped := self.certificate.drop(); dropped != nil {
			log.Printf(
				"[extender] lag: measurement certificate for %s withdrawn by the operator = %s",
				strings.Join(dropped.names, ","),
				result.Reason,
			)
		}
		self.certificate.setHeld(nil, result.Names)
		return self.idlePollTime(now, backoff)

	case ExtenderMeasurementCertStateNone, ExtenderMeasurementCertStateRefused:
		answeredNames := []string(nil)
		if result.State == ExtenderMeasurementCertStateRefused {
			log.Printf("[extender] lag: measurement certificate refused by the operator = %s", result.Reason)
			if result.KeySha256 == "" || result.KeySha256 == self.certificate.keySha256 {
				// the refusal of this key's request settles it
				answeredNames = result.Names
			} else {
				// a refusal of another key's request says nothing of this key's,
				// which goes out with a pass now
				self.askForKey("request refused", result.KeySha256)
			}
		}
		// the installed certificate, if any, is served until it expires or is
		// withdrawn: a transient answer must not take a valid one down (C4)
		self.certificate.setHeld(nil, answeredNames)
		return self.idlePollTime(now, backoff)

	default:
		log.Printf("[extender] lag: measurement certificate state %q is not known", result.State)
		return now.Add(backoff), nextBackoff
	}
}

// Asks for an activation pass at once because the operator holds or answered
// for another key than this one (lag.dev/PLAN.md C4): the pass carries this
// key's request, instead of the 24 hour tick. A mismatch that stands asks
// once, however often it is read.
func (self *extenderLagLifecycle) askForKey(what string, keySha256 string) {
	keyMismatch := what + " " + keySha256
	if keyMismatch == self.keyMismatch {
		return
	}
	self.keyMismatch = keyMismatch
	log.Printf("[extender] lag: the operator has a measurement %s for another key; asking for a pass now", what)
	self.certificate.requestReactivate()
}

// When to read again with nothing pending: on the backoff while an offered
// request has not been answered for, until the backoff reaches its max, and
// then at the check timeout, or ahead of the installed certificate's renewal.
func (self *extenderLagLifecycle) idlePollTime(now time.Time, backoff time.Duration) (time.Time, time.Duration) {
	settings := self.settings
	if self.certificate.offered() {
		if backoff < settings.CertPollMaxBackoff {
			return now.Add(backoff), min(2*backoff, settings.CertPollMaxBackoff)
		}
		// the post that carried it may never have landed; the next
		// activation offers again
		self.certificate.dropOffer()
	}
	nextPollTime := now.Add(settings.CertCheckTimeout)
	if installed := self.certificate.installedCertificate(); installed != nil {
		if installedPollTime := self.installedPollTime(installed, now); installedPollTime.Before(nextPollTime) {
			nextPollTime = installedPollTime
		}
	}
	return nextPollTime, settings.CertPollMinBackoff
}

// When to read again with a certificate installed (C6): at two thirds of its
// life, when the operator renews it; from then on, while the operator still
// answers with it, at a quarter of the time it has left, so the renewed chain
// is installed before this one runs out; and never further apart than the
// check timeout or closer than the max backoff.
func (self *extenderLagLifecycle) installedPollTime(installed *extenderMeasurementInstalled, now time.Time) time.Time {
	settings := self.settings
	renewTime := installed.notBefore.Add(installed.notAfter.Sub(installed.notBefore) * 2 / 3)
	var wait time.Duration
	if now.Before(renewTime) {
		wait = renewTime.Sub(now)
	} else {
		wait = max(installed.notAfter.Sub(now)/4, settings.CertPollMaxBackoff)
	}
	return now.Add(min(wait, settings.CertCheckTimeout))
}

// Reads the certificate state of this extender under the activation
// credential (A8).
func (self *extenderLagLifecycle) getCertificate() (*extenderMeasurementCertResult, error) {
	ctx, cancel := context.WithTimeout(self.server.ctx, self.settings.RequestTimeout)
	defer cancel()
	byJwt := ""
	if self.settings.ByJwt != nil {
		byJwt = self.settings.ByJwt()
	}
	certUrl := strings.TrimRight(strings.TrimSpace(self.settings.ApiUrl), "/") + ExtenderMeasurementCertPath
	if publicKey := self.server.PublicKey(); 0 < len(publicKey) {
		// a client may have activated more than one extender; the identity key
		// picks this one
		certUrl += "?public_key_hex=" + hex.EncodeToString(publicKey)
	}
	result, err := connect.HttpGetWithStrategy(
		ctx,
		self.settings.ClientStrategy,
		certUrl,
		byJwt,
		&extenderMeasurementCertResult{},
		connect.NewNoopApiCallback[*extenderMeasurementCertResult](),
	)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("the operator sent no certificate state")
	}
	return result, nil
}

// The certificate route's answer (A8). The json is the server's contract.
type extenderMeasurementCertResult struct {
	State string `json:"state"`
	// the extender's one measurement name: the request's when pending or
	// refused, the certificate's when issued or withdrawn
	Names []string `json:"names,omitempty"`
	// the issued chain, leaf first
	ChainPem string `json:"chain_pem,omitempty"`
	// RFC 3339. The leaf's own NotAfter is what is read.
	NotAfter string `json:"not_after,omitempty"`
	// why a request was refused
	Reason string `json:"reason,omitempty"`
	// hex sha-256 of the SubjectPublicKeyInfo DER of the key the request or
	// the certificate is for: what is held is held for that key alone
	KeySha256 string `json:"key_sha256,omitempty"`
}
