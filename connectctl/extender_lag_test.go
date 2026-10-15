package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/gorilla/websocket"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/extender"
	"github.com/urnetwork/connect/protocol"
)

// The lag.dev measurement API of the connectctl extender (lag.dev/PLAN.md C4,
// M3, M10): the owner's switch, the kept identity it needs, and a ticket the
// operator signed admitted end to end through the settings the command wires.
//
// The operator is the fixture operator of extender_test.go fronted by a server
// that also answers what the measurement needs: the extender's id in each
// activation answer (N4), a certificate issued at once for the request an
// activation carries, and the certificate route (A8).

// The origin a browser on lag.dev sends (M2).
const testMeasurementOrigin = "https://lag.dev"

// The fixture operator, fronted.
type testMeasurementOperator struct {
	t      *testing.T
	inner  *testExtenderOperator
	server *httptest.Server

	extenderId    connect.Id
	caKey         *ecdsa.PrivateKey
	caCertificate *x509.Certificate
	activations   chan *connect.ExtenderActivateArgs

	stateLock sync.Mutex
	// what the certificate route answers
	certResult map[string]any
	certPolls  int
	// whether a request an activation carries is issued at once, else held
	// pending
	issueOnRequest bool
}

func newTestMeasurementOperator(t *testing.T) *testMeasurementOperator {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "measurement test ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caBytes, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCertificate, err := x509.ParseCertificate(caBytes)
	if err != nil {
		t.Fatal(err)
	}
	operator := &testMeasurementOperator{
		t:              t,
		inner:          newTestExtenderOperator(t),
		extenderId:     connect.NewId(),
		caKey:          caKey,
		caCertificate:  caCertificate,
		activations:    make(chan *connect.ExtenderActivateArgs, 64),
		certResult:     map[string]any{"state": "none", "names": []string{}},
		issueOnRequest: true,
	}
	operator.server = httptest.NewServer(http.HandlerFunc(operator.handle))
	t.Cleanup(operator.server.Close)
	return operator
}

func (self *testMeasurementOperator) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case connect.ExtenderActivatePath:
		requestBytes, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		args := &connect.ExtenderActivateArgs{}
		if err := json.Unmarshal(requestBytes, args); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(requestBytes))
		recorder := httptest.NewRecorder()
		self.inner.handle(recorder, r)
		result := &connect.ExtenderActivateResult{}
		if err := json.Unmarshal(recorder.Body.Bytes(), result); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if result.Activated {
			extenderId := self.extenderId
			result.ExtenderId = &extenderId
		}
		if args.MeasurementCsr != "" {
			// issued at once, so the next read of the certificate route
			// installs it, or held pending (A10, C5)
			self.request(args.MeasurementCsr)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
		self.activations <- args
	case extender.ExtenderMeasurementCertPath:
		self.stateLock.Lock()
		self.certPolls += 1
		certResult := self.certResult
		self.stateLock.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(certResult)
	default:
		self.inner.handle(w, r)
	}
}

// Takes a request: issues a chain for its key and its one name, the
// extender's, or holds it pending (C5).
func (self *testMeasurementOperator) request(csr string) {
	csrBytes, err := base64.StdEncoding.DecodeString(csr)
	if err != nil {
		self.t.Error(err)
		return
	}
	request, err := x509.ParseCertificateRequest(csrBytes)
	if err != nil || request.CheckSignature() != nil {
		self.t.Errorf("the measurement request does not verify: %v", err)
		return
	}
	name := connect.ExtenderMeasurementName(self.extenderId, "")
	if !slices.Equal(request.DNSNames, []string{name}) {
		self.t.Errorf("request names = %v, expected %s", request.DNSNames, name)
		return
	}
	self.stateLock.Lock()
	issueOnRequest := self.issueOnRequest
	self.stateLock.Unlock()
	if !issueOnRequest {
		self.setCertResult(map[string]any{
			"state":      "pending",
			"names":      request.DNSNames,
			"key_sha256": testMeasurementKeySha256(self.t, request.PublicKey),
		})
		return
	}
	self.issueFor(request.PublicKey, request.DNSNames)
}

// Answers the certificate route with a chain for a key and names.
func (self *testMeasurementOperator) issueFor(publicKey any, names []string) {
	leafBytes, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		DNSNames:     names,
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, self.caCertificate, publicKey, self.caKey)
	if err != nil {
		self.t.Error(err)
		return
	}
	chainPem := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafBytes})) +
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: self.caCertificate.Raw}))
	self.setCertResult(map[string]any{
		"state":      "issued",
		"names":      names,
		"chain_pem":  chainPem,
		"key_sha256": testMeasurementKeySha256(self.t, publicKey),
	})
}

func (self *testMeasurementOperator) setCertResult(certResult map[string]any) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.certResult = certResult
}

func (self *testMeasurementOperator) setIssueOnRequest(issueOnRequest bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.issueOnRequest = issueOnRequest
}

// How the operator names a key (A8): the hex sha-256 of its
// SubjectPublicKeyInfo DER.
func testMeasurementKeySha256(t *testing.T, publicKey any) string {
	publicKeyDer, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Error(err)
		return ""
	}
	keySha256 := sha256.Sum256(publicKeyDer)
	return hex.EncodeToString(keySha256[:])
}

func (self *testMeasurementOperator) certPollCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.certPolls
}

// Waits for one activation post, and returns what it carried.
func (self *testMeasurementOperator) waitActivation() *connect.ExtenderActivateArgs {
	self.t.Helper()
	select {
	case args := <-self.activations:
		return args
	case <-time.After(60 * time.Second):
		self.t.Fatal("the extender did not activate")
		return nil
	}
}

// A running extender command against the operator, and what it was built with.
type testMeasurementExtender struct {
	run     *extenderRun
	tcpPort int
	// the identity, nil when it is ephemeral
	publicKey ed25519.PublicKey
	stateDir  string
}

// Runs the extender command on ephemeral loopback carriers against the
// operator, until the test ends. `keep` keeps the identity in a key file;
// `configure` adjusts the options last.
func startTestMeasurementExtender(
	t *testing.T,
	operator *testMeasurementOperator,
	keep bool,
	configure func(options *extenderOptions),
) *testMeasurementExtender {
	t.Helper()
	operatorAddress := operator.server.Listener.Addr().String()
	_, operatorPort, err := net.SplitHostPort(operatorAddress)
	if err != nil {
		t.Fatal(err)
	}

	started := &testMeasurementExtender{}
	options := &extenderOptions{
		jwt:    "test-jwt",
		apiUrl: fmt.Sprintf("http://%s:%s", testExtenderApiHost, operatorPort),
	}
	if keep {
		started.stateDir = t.TempDir()
		keySeed, err := connect.NewExtenderKeySeed()
		if err != nil {
			t.Fatal(err)
		}
		options.keyFile = filepath.Join(started.stateDir, "extender.key")
		if err := os.WriteFile(options.keyFile, []byte(connect.ExtenderKeySeedHex(keySeed)), 0600); err != nil {
			t.Fatal(err)
		}
		if started.publicKey, err = connect.ExtenderPublicKeyFromSeed(keySeed); err != nil {
			t.Fatal(err)
		}
	}

	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	quicPacketConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dnsPacketConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started.tcpPort = tcpListener.Addr().(*net.TCPAddr).Port
	quicPort := quicPacketConn.LocalAddr().(*net.UDPAddr).Port
	dnsPort := dnsPacketConn.LocalAddr().(*net.UDPAddr).Port
	options.tcpPort = started.tcpPort
	options.udpPort = quicPort
	options.dnsPort = dnsPort
	options.listen = func(network string, address string) (net.Listener, error) {
		if address != fmt.Sprintf(":%d", started.tcpPort) {
			return nil, fmt.Errorf("unexpected extender listen %s %s", network, address)
		}
		return tcpListener, nil
	}
	options.listenPacket = func(network string, address string) (net.PacketConn, error) {
		switch address {
		case fmt.Sprintf(":%d", quicPort):
			return quicPacketConn, nil
		case fmt.Sprintf(":%d", dnsPort):
			return dnsPacketConn, nil
		default:
			return nil, fmt.Errorf("unexpected extender listen packet %s %s", network, address)
		}
	}
	// every operator name is the one in-process server
	options.dialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if !strings.HasSuffix(host, testExtenderNetworkHost) {
			return nil, fmt.Errorf("unexpected dial %s %s", network, address)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", operatorAddress)
	}
	// the udp carrier dials send to loopback only, and the dns bootstrap stays
	// in the process, as in the extender command's own fixture
	options.packetConnFactory = testLoopbackPacketConn
	options.configureNetworkClient = testHermeticNetworkClient
	// a read once the request has landed, and soon after on a backoff
	options.configureLagLifecycle = func(settings *extender.ExtenderLagLifecycleSettings) {
		settings.CertOfferPollTimeout = 20 * time.Millisecond
		settings.CertPollMinBackoff = 20 * time.Millisecond
		settings.CertPollMaxBackoff = 200 * time.Millisecond
	}
	runs := make(chan *extenderRun, 1)
	options.onStart = func(run *extenderRun) {
		runs <- run
	}
	if configure != nil {
		configure(options)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() {
		runDone <- runExtender(ctx, options)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(60 * time.Second):
			t.Error("the extender did not stop")
		}
	})
	select {
	case started.run = <-runs:
	case err := <-runDone:
		t.Fatalf("the extender exited before it started: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("the extender did not start")
	}
	return started
}

// Opens a lag session on the extender's tcp carrier under its measurement
// name, as a browser on lag.dev does (M1, M4), and returns the first message.
func (self *testMeasurementExtender) openSession(t *testing.T, name string, rootCas *x509.CertPool, ticketText string) map[string]any {
	t.Helper()
	dialer := &websocket.Dialer{
		NetDialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", self.tcpPort))
		},
		TLSClientConfig:  &tls.Config{RootCAs: rootCas},
		HandshakeTimeout: 30 * time.Second,
	}
	header := http.Header{}
	header.Set("Origin", testMeasurementOrigin)
	conn, _, err := dialer.Dial(
		"wss://"+name+"/lag/v1/session?ticket="+url.QueryEscape(ticketText),
		header,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	messageType, messageBytes, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.TextMessage {
		t.Fatalf("message type = %d, expected text", messageType)
	}
	message := map[string]any{}
	if err := json.Unmarshal(messageBytes, &message); err != nil {
		t.Fatal(err)
	}
	return message
}

// A ticket for one session on the extender (A6), signed by `rootPrivateKey`.
func testMeasurementTicket(
	t *testing.T,
	rootPrivateKey ed25519.PrivateKey,
	extenderPublicKey ed25519.PublicKey,
	networkHost string,
) string {
	t.Helper()
	randomBytes := func() []byte {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	now := time.Now()
	ticket, err := connect.SignLagTicket(&protocol.LagTicketBody{
		TicketId:          randomBytes(),
		ClientId:          randomBytes(),
		ExtenderPublicKey: extenderPublicKey,
		NetworkHost:       networkHost,
		IssueTimeMs:       uint64(now.UnixMilli()),
		ExpireTimeMs:      uint64(now.Add(2 * time.Minute).UnixMilli()),
		TrackId:           randomBytes(),
		PingIntervalMs:    250,
		DownloadMs:        5000,
		UploadMs:          5000,
		MaxStreams:        4,
	}, rootPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	ticketText, err := connect.EncodeLagTicket(ticket)
	if err != nil {
		t.Fatal(err)
	}
	return ticketText
}

// Measurement is on unless --no_measurement, the zone is the flag's, and the
// grammar takes both flags.
func TestExtenderMeasurementFlags(t *testing.T) {
	opts, err := docopt.ParseArgs(connectCtlUsage(), []string{"extender", "--jwt=test-jwt"}, ConnectCtlVersion)
	if err != nil {
		t.Fatal(err)
	}
	options, err := extenderOptionsFromOpts(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !options.measurementEnabled || options.measurementZone != "" {
		t.Fatalf("measurement = %v %q, expected on in the default zone", options.measurementEnabled, options.measurementZone)
	}

	opts, err = docopt.ParseArgs(connectCtlUsage(), []string{
		"extender",
		"--jwt=test-jwt",
		"--no_measurement",
		"--measurement_zone=measure.example",
	}, ConnectCtlVersion)
	if err != nil {
		t.Fatal(err)
	}
	options, err = extenderOptionsFromOpts(opts)
	if err != nil {
		t.Fatal(err)
	}
	if options.measurementEnabled || options.measurementZone != "measure.example" {
		t.Fatalf("measurement = %v %q, expected off in measure.example", options.measurementEnabled, options.measurementZone)
	}
}

// The certificate exists only when measurement is on and the identity is kept
// in a file; its key is kept beside that file, and the server settings follow:
// the API's trust and hosts and the lifecycle with it, or no API at all.
func TestExtenderMeasurementNeedsAKeptIdentity(t *testing.T) {
	stateDir := t.TempDir()
	cases := []struct {
		options      *extenderOptions
		expectedFile string
	}{
		{
			options:      &extenderOptions{measurementEnabled: true, keyFile: filepath.Join(stateDir, "one", "extender.key")},
			expectedFile: filepath.Join(stateDir, "one", extender.ExtenderMeasurementKeyFileName),
		},
		{
			options:      &extenderOptions{measurementEnabled: true, stateDir: filepath.Join(stateDir, "two")},
			expectedFile: filepath.Join(stateDir, "two", extender.ExtenderMeasurementKeyFileName),
		},
		// an ephemeral identity, and the owner's switch off
		{options: &extenderOptions{measurementEnabled: true}},
		{options: &extenderOptions{keyFile: filepath.Join(stateDir, "three", "extender.key")}},
	}
	for i, c := range cases {
		c.options.apiUrl = testOptionsApiUrl
		c.options.jwt = "test-jwt"
		run := newTestExtenderRun(t, c.options)
		ctx, cancel := context.WithCancel(context.Background())
		run.directory = connect.NewExtenderDirectory(ctx, connect.DefaultExtenderDirectorySettings())
		rootPublicKey, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		run.directory.SetRootKeys(connect.NewExtenderRootKeySet(rootPublicKey))

		run.measurementCert = run.newMeasurementCert()
		serverSettings := extender.DefaultExtenderSettings()
		run.measurementSettings(serverSettings)
		if c.expectedFile == "" {
			if run.measurementCert != nil {
				t.Errorf("case %d: a certificate without measurement on and a kept identity", i)
			}
			if serverSettings.Measurement.Enabled || serverSettings.LagLifecycle != nil {
				t.Errorf("case %d: measurement settings = %+v, expected none", i, serverSettings.Measurement)
			}
		} else {
			if run.measurementCert == nil || run.measurementCert.KeyFile() != c.expectedFile {
				t.Errorf("case %d: certificate key file = %v, expected %s", i, run.measurementCert, c.expectedFile)
			} else if info, err := os.Stat(c.expectedFile); err != nil || info.Mode().Perm() != 0600 {
				t.Errorf("case %d: measurement key file = %v %v, expected mode 600", i, info, err)
			}
			if !serverSettings.Measurement.Enabled || serverSettings.LagLifecycle == nil ||
				serverSettings.LagLifecycle.Certificate != run.measurementCert ||
				serverSettings.LagLifecycle.ApiUrl != testOptionsApiUrl ||
				serverSettings.LagLifecycle.ByJwt() != "test-jwt" {
				t.Errorf("case %d: measurement settings are not wired", i)
			}
			if rootPublicKeys := serverSettings.Measurement.RootPublicKeys(); len(rootPublicKeys) != 1 || !rootPublicKeys[0].Equal(rootPublicKey) {
				t.Errorf("case %d: root keys = %v, expected the directory's", i, rootPublicKeys)
			}
			if !slices.Equal(serverSettings.Measurement.NetworkHosts, run.networkHosts()) {
				t.Errorf("case %d: network hosts = %v, expected %v", i, serverSettings.Measurement.NetworkHosts, run.networkHosts())
			}
		}
		run.directory.Close()
		cancel()
	}
	// nothing was kept for the cases that are off
	if _, err := os.Stat(filepath.Join(stateDir, "three", extender.ExtenderMeasurementKeyFileName)); err == nil {
		t.Error("a measurement key was kept with measurement off")
	}
}

// End to end: the activation carries the request once the operator has
// reported the id, the certificate it issues is installed, and a session opened
// under the measurement name with a ticket the operator's root key signed is
// admitted. A ticket another key signed, and one for another network, are
// refused as tickets: the API trusts the directory's root keys and hosts.
func TestExtenderCommandMeasurementAdmitsATicket(t *testing.T) {
	operator := newTestMeasurementOperator(t)
	started := startTestMeasurementExtender(t, operator, true, func(options *extenderOptions) {
		options.measurementEnabled = true
	})
	if started.run.measurementCert == nil {
		t.Fatal("measurement is off with a kept identity")
	}

	// the first post learns the id; the request rides the next one
	if args := operator.waitActivation(); args.MeasurementCsr != "" || args.MeasurementDisabled {
		t.Fatalf("the first activation carried %q, disabled %t", args.MeasurementCsr, args.MeasurementDisabled)
	}
	if args := operator.waitActivation(); args.MeasurementCsr == "" || args.MeasurementDisabled {
		t.Fatal("the activation after the first carried no request")
	}
	name := connect.ExtenderMeasurementName(operator.extenderId, "")
	for {
		_, change := started.run.measurementCert.ChangeMonitor().Get()
		if names, certificate := started.run.measurementCert.Certificate(); certificate != nil {
			if !slices.Equal(names, []string{name}) {
				t.Fatalf("served names = %v, expected %s", names, name)
			}
			break
		}
		select {
		case <-change:
		case <-time.After(60 * time.Second):
			t.Fatal("the measurement certificate was not installed")
		}
	}

	rootCas := x509.NewCertPool()
	rootCas.AddCert(operator.caCertificate)
	message := started.openSession(t, name, rootCas, testMeasurementTicket(
		t, operator.inner.rootPrivateKey, started.publicKey, testExtenderNetworkHost))
	if message["type"] != "hello" {
		t.Fatalf("session answer = %v, expected hello", message)
	}

	_, otherRootPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, ticketText := range []string{
		testMeasurementTicket(t, otherRootPrivateKey, started.publicKey, testExtenderNetworkHost),
		testMeasurementTicket(t, operator.inner.rootPrivateKey, started.publicKey, "other.example"),
	} {
		message := started.openSession(t, name, rootCas, ticketText)
		if message["type"] != "error" || message["code"] != "ticket" {
			t.Fatalf("session answer = %v, expected a ticket error", message)
		}
	}
}

// With measurement off, or with an ephemeral identity, the command keeps no
// certificate, asks for none and never reads the certificate route. Off with
// a kept identity, every activation says measurement_disabled, so a name the
// extender had is withdrawn (C4, C9); an ephemeral identity has none to
// withdraw and says nothing.
func TestExtenderCommandMeasurementOff(t *testing.T) {
	for _, c := range []struct {
		keep             bool
		measurement      bool
		expectedDisabled bool
	}{
		{keep: true, measurement: false, expectedDisabled: true},
		{keep: false, measurement: false, expectedDisabled: false},
		{keep: false, measurement: true, expectedDisabled: false},
	} {
		operator := newTestMeasurementOperator(t)
		started := startTestMeasurementExtender(t, operator, c.keep, func(options *extenderOptions) {
			options.measurementEnabled = c.measurement
		})
		if started.run.measurementCert != nil {
			t.Fatalf("keep %v: a measurement certificate with measurement off", c.keep)
		}
		args := operator.waitActivation()
		if args.MeasurementCsr != "" {
			t.Fatalf("keep %v: the activation carried a request", c.keep)
		}
		if args.MeasurementDisabled != c.expectedDisabled {
			t.Fatalf("keep %v, measurement %v: measurement_disabled = %t, expected %t",
				c.keep, c.measurement, args.MeasurementDisabled, c.expectedDisabled)
		}
		if count := operator.certPollCount(); count != 0 {
			t.Fatalf("keep %v: the certificate route was read %d times", c.keep, count)
		}
		if c.keep {
			if _, err := os.Stat(filepath.Join(started.stateDir, extender.ExtenderMeasurementKeyFileName)); err == nil {
				t.Fatalf("keep %v: a measurement key was kept with measurement off", c.keep)
			}
		}
	}
}

// With measurement on and a kept identity whose measurement key cannot be
// used, the command keeps no certificate and serves no measurement name, so
// every activation says measurement_disabled: a name the extender had is
// withdrawn (C4, C9) rather than renewed and offered to lag.dev with nothing
// answering it. The key file is left as it was.
func TestExtenderCommandMeasurementKeyNotUsable(t *testing.T) {
	operator := newTestMeasurementOperator(t)
	keyPem := []byte("not a measurement key\n")
	measurementKeyFile := ""
	started := startTestMeasurementExtender(t, operator, true, func(options *extenderOptions) {
		options.measurementEnabled = true
		measurementKeyFile = extender.ExtenderMeasurementKeyPath(options.keyFile)
		if err := os.WriteFile(measurementKeyFile, keyPem, 0600); err != nil {
			t.Fatal(err)
		}
	})
	if started.run.measurementCert != nil {
		t.Fatal("a measurement certificate with a key that is not usable")
	}
	args := operator.waitActivation()
	if args.MeasurementCsr != "" || !args.MeasurementDisabled {
		t.Fatalf("the activation carried %q, measurement_disabled %t", args.MeasurementCsr, args.MeasurementDisabled)
	}
	if count := operator.certPollCount(); count != 0 {
		t.Fatalf("the certificate route was read %d times", count)
	}
	if b, err := os.ReadFile(measurementKeyFile); err != nil || !bytes.Equal(b, keyPem) {
		t.Fatalf("measurement key file = %q %v, expected it untouched", b, err)
	}
}

// A chain the operator issued for another key asks the activation for a pass
// at once, which carries this key's request again (C4): the lifecycle's ask
// reaches the command's activator.
func TestExtenderCommandMeasurementAsksForAPassOnAnotherKey(t *testing.T) {
	operator := newTestMeasurementOperator(t)
	operator.setIssueOnRequest(false)
	started := startTestMeasurementExtender(t, operator, true, func(options *extenderOptions) {
		options.measurementEnabled = true
	})
	// the passes of the start: the request rides the post after the first
	operator.waitActivation()
	if args := operator.waitActivation(); args.MeasurementCsr == "" {
		t.Fatal("the activation after the first carried no request")
	}

	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	operator.issueFor(&otherKey.PublicKey, []string{connect.ExtenderMeasurementName(operator.extenderId, "")})
	// nothing else would activate before the 24 hour tick
	for {
		args := operator.waitActivation()
		if args.MeasurementCsr != "" {
			break
		}
	}
	if _, certificate := started.run.measurementCert.Certificate(); certificate != nil {
		t.Fatal("a chain for another key was installed")
	}
}

// The status line names the certificate the extender serves, or none.
func TestExtenderMeasurementLine(t *testing.T) {
	measurementCert, err := extender.NewExtenderMeasurementCert("", "")
	if err != nil {
		t.Fatal(err)
	}
	if line := extenderMeasurementLine(measurementCert); line != "extender measurement: no certificate" {
		t.Fatalf("line = %q", line)
	}
}
