package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/extender"
)

// The NLayer hops of the connectctl extender (EXTENDER.md A11): the
// --nlayer-hop spec, the file a private hop's secret is read from, and the
// command relaying every forward to the hops it names.

// A synthetic identity key in hex, for a spec that pins its hop.
const testHopPublicKeyHex = "5f3c9a4be1d07a6c2e8b9f10d4a3c7e65b2a1f0e9d8c7b6a5f4e3d2c1b0a9f8e"

// A synthetic secret of a private hop, which only ever lives in a file.
const testHopSecret = "synthetic-hop-secret"

// Writes content to a file only the test user can read, and returns its path.
func writeTestSecretFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hop.secret")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The grammar takes --nlayer-hop more than once, and each arrives as one hop,
// a private one with the secret its file holds.
func TestExtenderUsageParsesNLayerHops(t *testing.T) {
	secretPath := writeTestSecretFile(t, testHopSecret+"\n")
	argv := []string{
		"extender",
		"--jwt=test-jwt",
		"--nlayer-hop=tcp://203.0.113.5:8443",
		"--nlayer-hop=quic://[2001:db8::5]?key=" + testHopPublicKeyHex + "&secret_file=" + secretPath,
	}
	opts, err := docopt.ParseArgs(connectCtlUsage(), argv, ConnectCtlVersion)
	if err != nil {
		t.Fatal(err)
	}
	options, err := extenderOptionsFromOpts(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(options.nlayerHops) != 2 {
		t.Fatalf("hops = %d, expected 2", len(options.nlayerHops))
	}
	first := options.nlayerHops[0]
	if first.Profile.ConnectMode != connect.ExtenderConnectModeTcpTls || first.Profile.Port != 8443 ||
		first.Ip != netip.MustParseAddr("203.0.113.5") || first.Secret != "" || 0 < len(first.PublicKey) {
		t.Fatalf("first hop = %+v", first)
	}
	second := options.nlayerHops[1]
	if second.Profile.ConnectMode != connect.ExtenderConnectModeQuic || second.Profile.Port != connect.ExtenderQuicPort ||
		second.Ip != netip.MustParseAddr("2001:db8::5") || second.Secret != testHopSecret ||
		hex.EncodeToString(second.PublicKey) != testHopPublicKeyHex {
		t.Fatalf("second hop = %+v", second)
	}

	// without the flag the extender forwards to its destinations
	opts, err = docopt.ParseArgs(connectCtlUsage(), []string{"extender", "--jwt=test-jwt"}, ConnectCtlVersion)
	if err != nil {
		t.Fatal(err)
	}
	options, err = extenderOptionsFromOpts(opts)
	if err != nil {
		t.Fatal(err)
	}
	if 0 < len(options.nlayerHops) {
		t.Fatalf("hops = %v, expected none", options.nlayerHops)
	}
}

// Every part of the spec lands in the config it names, with the carrier's
// fixed port and the default tld where the spec names none, and the secret is
// the file's content trimmed, whatever characters it holds.
func TestParseExtenderHopSpec(t *testing.T) {
	secretPath := writeTestSecretFile(t, "  s3cr:t@with#marks \r\n")
	cases := []struct {
		spec   string
		expect func(hop *connect.ExtenderConfig) bool
	}{
		{
			spec: "203.0.113.5",
			expect: func(hop *connect.ExtenderConfig) bool {
				return hop.Profile.ConnectMode == connect.ExtenderConnectModeTcpTls &&
					hop.Profile.Port == connect.ExtenderTcpPort &&
					hop.Ip == netip.MustParseAddr("203.0.113.5")
			},
		},
		{
			spec: "  [2001:db8::7]:1443  ",
			expect: func(hop *connect.ExtenderConfig) bool {
				return hop.Profile.ConnectMode == connect.ExtenderConnectModeTcpTls &&
					hop.Profile.Port == 1443 &&
					hop.Ip == netip.MustParseAddr("2001:db8::7")
			},
		},
		{
			spec: "quic://198.51.100.9",
			expect: func(hop *connect.ExtenderConfig) bool {
				return hop.Profile.ConnectMode == connect.ExtenderConnectModeQuic &&
					hop.Profile.Port == connect.ExtenderQuicPort
			},
		},
		{
			spec: "dns://198.51.100.9",
			expect: func(hop *connect.ExtenderConfig) bool {
				return hop.Profile.ConnectMode == connect.ExtenderConnectModeDns &&
					hop.Profile.Port == connect.ExtenderDnsPort &&
					hop.Profile.DnsTld == connect.DefaultExtenderDnsTld
			},
		},
		{
			spec: "dns://198.51.100.9:53?tld=t.example",
			expect: func(hop *connect.ExtenderConfig) bool {
				return hop.Profile.Port == 53 && hop.Profile.DnsTld == "t.example."
			},
		},
		{
			spec: "tcp://203.0.113.5/?secret_file=" + secretPath,
			expect: func(hop *connect.ExtenderConfig) bool {
				return hop.Secret == "s3cr:t@with#marks"
			},
		},
		{
			spec: "tcp://203.0.113.5?key=" + testHopPublicKeyHex + "&sni=front.example",
			expect: func(hop *connect.ExtenderConfig) bool {
				return hex.EncodeToString(hop.PublicKey) == testHopPublicKeyHex &&
					hop.Profile.ServerName == "front.example"
			},
		},
		{
			spec: "tcp://203.0.113.5?fragment&reorder=false",
			expect: func(hop *connect.ExtenderConfig) bool {
				return hop.Profile.Fragment && !hop.Profile.Reorder
			},
		},
		{
			// the name a client dialer presents (A10): one of the spoof list,
			// and none at all while the list is empty
			spec: "tcp://203.0.113.5",
			expect: func(hop *connect.ExtenderConfig) bool {
				if hop.Secret != "" || 0 < len(hop.PublicKey) {
					return false
				}
				if spoofDomains := connect.SpoofDomains(); 0 < len(spoofDomains) {
					return slices.Contains(spoofDomains, hop.Profile.ServerName)
				}
				return hop.Profile.ServerName == ""
			},
		},
	}
	for _, c := range cases {
		hop, err := parseExtenderHopSpec(c.spec)
		if err != nil {
			t.Errorf("%q: %v", c.spec, err)
			continue
		}
		if !c.expect(hop) {
			t.Errorf("%q parsed as %+v", c.spec, hop)
		}
	}
}

// A secret in the address is refused, whatever the rest of the spec says, with
// an error that points to secret_file and repeats nothing of the input.
func TestParseExtenderHopSpecRefusesASecretInTheAddress(t *testing.T) {
	const secret = "do-not-print-this-secret"
	specs := []string{
		secret + "@203.0.113.5",
		"tcp://" + secret + "@203.0.113.5:443",
		"quic://user:" + secret + "@[2001:db8::5]",
		"tcp://" + secret + "@203.0.113.5:port",
		"tcp://" + secret + "@hop.example?key=00",
		"tcp://" + secret + "@203.0.113.5?secret_file=/elsewhere",
	}
	for _, spec := range specs {
		hop, err := parseExtenderHopSpec(spec)
		if err == nil {
			t.Errorf("a secret in the address was accepted as %+v", hop)
			continue
		}
		if !strings.Contains(err.Error(), "secret_file") {
			t.Errorf("the refusal %q does not say to use secret_file", err)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "203.0.113.5") {
			t.Errorf("the refusal %q repeats the input", err)
		}
	}
}

// A spec that does not name one dialable extender is refused. A secret file
// that cannot be read, or holds nothing, fails at start with an error that
// names its path and never what it holds, including when the file is fine and
// the rest of the spec is not.
func TestParseExtenderHopSpecRefusesWhatItCannotDial(t *testing.T) {
	secretPath := writeTestSecretFile(t, testHopSecret)
	emptyPath := writeTestSecretFile(t, " \n\t\n")
	missingPath := filepath.Join(t.TempDir(), "missing.secret")
	directoryPath := t.TempDir()
	cases := []struct {
		spec     string
		mentions string
	}{
		{spec: ""},
		{spec: "   "},
		{spec: "udp://203.0.113.5"},
		{spec: "tcp://hop.example"},
		{spec: "tcp://203.0.113.5:0"},
		{spec: "tcp://203.0.113.5:65536"},
		{spec: "tcp://203.0.113.5:port"},
		{spec: "tcp://203.0.113.5/path"},
		{spec: "tcp://203.0.113.5#fragment"},
		{spec: "tcp://203.0.113.5?key=00"},
		{spec: "tcp://203.0.113.5?unknown=1"},
		{spec: "tcp://203.0.113.5?sni=a&sni=b"},
		{spec: "tcp://203.0.113.5?tld=t.example"},
		{spec: "dns://203.0.113.5?tld="},
		{spec: "quic://203.0.113.5?fragment"},
		{spec: "tcp://203.0.113.5?reorder=maybe"},
		{spec: "tcp://203.0.113.5?%zz"},
		{spec: "tcp://203.0.113.5?secret_file="},
		{spec: "tcp://203.0.113.5?secret_file=" + missingPath, mentions: missingPath},
		{spec: "tcp://203.0.113.5?secret_file=" + directoryPath, mentions: directoryPath},
		{spec: "tcp://203.0.113.5?secret_file=" + emptyPath, mentions: emptyPath},
		// the file is read only once the rest is known good
		{spec: "tcp://203.0.113.5?secret_file=" + secretPath + "&unknown=1"},
		{spec: "tcp://203.0.113.5:port?secret_file=" + secretPath},
	}
	for _, c := range cases {
		hop, err := parseExtenderHopSpec(c.spec)
		if err == nil {
			t.Errorf("%q was accepted as %+v", c.spec, hop)
			continue
		}
		if c.mentions != "" && !strings.Contains(err.Error(), c.mentions) {
			t.Errorf("%q failed with %q, which does not name %s", c.spec, err, c.mentions)
		}
		if strings.Contains(err.Error(), testHopSecret) {
			t.Errorf("%q failed with %q, which carries the secret", c.spec, err)
		}
	}
}

// A missing secret file fails the command at start: the flag names which hop
// was wrong and the path it named.
func TestExtenderOptionsFailOnAMissingSecretFile(t *testing.T) {
	missingPath := filepath.Join(t.TempDir(), "missing.secret")
	opts, err := docopt.ParseArgs(connectCtlUsage(), []string{
		"extender",
		"--jwt=test-jwt",
		"--nlayer-hop=tcp://203.0.113.5",
		"--nlayer-hop=tcp://203.0.113.6?secret_file=" + missingPath,
	}, ConnectCtlVersion)
	if err != nil {
		t.Fatal(err)
	}
	options, err := extenderOptionsFromOpts(opts)
	if err == nil {
		t.Fatalf("a missing secret file was accepted as %+v", options)
	}
	if !strings.Contains(err.Error(), "--nlayer-hop 1") || !strings.Contains(err.Error(), missingPath) {
		t.Fatalf("the flag failed with %q", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the flag failed with %q, which does not say the file is missing", err)
	}
}

// The log line of a hop names its carrier and address and whether it is
// private or pinned, never its secret.
func TestExtenderHopDescription(t *testing.T) {
	secretPath := writeTestSecretFile(t, testHopSecret)
	hop, err := parseExtenderHopSpec(
		"quic://[2001:db8::5]:4443?key=" + testHopPublicKeyHex + "&secret_file=" + secretPath,
	)
	if err != nil {
		t.Fatal(err)
	}
	description := extenderHopDescription(hop)
	if description != "quic [2001:db8::5]:4443 private pinned" {
		t.Fatalf("description = %q", description)
	}
}

// A certificate for one name, so a test site can be verified by name.
func testSiteCertificate(t *testing.T, serverName string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"NLayer Test"}},
		DNSNames:              []string{serverName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	certificateBytes, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(certificateBytes)
	if err != nil {
		t.Fatal(err)
	}
	rootCas := x509.NewCertPool()
	rootCas.AddCert(leaf)
	return tls.Certificate{
		Certificate: [][]byte{certificateBytes},
		PrivateKey:  privateKey,
		Leaf:        leaf,
	}, rootCas
}

// A tls site for the api host, which answers every request with body, and the
// roots it verifies under. It stops with the test.
func startTestSite(t *testing.T, body string) (string, *x509.CertPool) {
	t.Helper()
	siteCertificate, siteRootCas := testSiteCertificate(t, testExtenderApiHost)
	siteListener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{siteCertificate},
	})
	if err != nil {
		t.Fatal(err)
	}
	siteServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Write([]byte(body))
		}),
	}
	go siteServer.Serve(siteListener)
	t.Cleanup(func() { siteServer.Close() })
	return siteListener.Addr().String(), siteRootCas
}

// A hop for this command to relay to: an extender on a loopback tcp carrier
// that forwards the api host to siteAddress and requires one of
// allowedSecrets when it has any (A4). It returns the carrier port and stops
// with the test.
func startTestHopExtender(t *testing.T, allowedSecrets []string, siteAddress string) int {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	hopListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hopPort := hopListener.Addr().(*net.TCPAddr).Port
	hopSettings := extender.DefaultExtenderSettings()
	hopSettings.Listen = func(network string, address string) (net.Listener, error) {
		return hopListener, nil
	}
	hopSettings.DialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", siteAddress)
	}
	hop := extender.NewExtenderServer(
		ctx,
		allowedSecrets,
		[]string{testExtenderApiHost},
		map[int][]connect.ExtenderConnectMode{hopPort: {connect.ExtenderConnectModeTcpTls}},
		&net.Dialer{},
		hopSettings,
	)
	hopDone := make(chan error, 1)
	go func() { hopDone <- hop.ListenAndServe() }()
	t.Cleanup(func() {
		hop.CloseAndWait()
		cancel()
		<-hopDone
	})
	select {
	case <-hop.Listening():
	case <-time.After(10 * time.Second):
		t.Fatal("the hop did not bind")
	}
	return hopPort
}

// The secret a hop spec reads from its file is what signs the header the hop
// is sent (A4): a private hop accepts it, and refuses the header another
// file's secret signs.
func TestExtenderHopSecretFileSignsTheHeader(t *testing.T) {
	siteAddress, _ := startTestSite(t, "signed")
	hopPort := startTestHopExtender(t, []string{testHopSecret}, siteAddress)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dial := func(secretContent string) error {
		hop, err := parseExtenderHopSpec(fmt.Sprintf(
			"tcp://127.0.0.1:%d?secret_file=%s", hopPort, writeTestSecretFile(t, secretContent),
		))
		if err != nil {
			return err
		}
		conn, _, err := connect.DialExtender(ctx, connect.DefaultConnectSettings(), hop, &connect.ExtenderDial{
			DestinationHost: testExtenderApiHost,
			DestinationPort: 443,
		})
		if conn != nil {
			conn.Close()
		}
		return err
	}
	if err := dial(testHopSecret + "\n"); err != nil {
		t.Fatalf("the header signed with the file's secret was refused: %v", err)
	}
	var refusedErr *connect.ExtenderRefusedError
	if err := dial("another-synthetic-secret"); !errors.As(err, &refusedErr) || refusedErr.StatusCode != http.StatusForbidden {
		t.Fatalf("the header signed with another secret got %v, expected a 403", err)
	}
}

// A writer the command's loggers can share with the test that reads them.
type lockedLogBuffer struct {
	stateLock sync.Mutex
	buffer    bytes.Buffer
}

// Implements io.Writer.
func (self *lockedLogBuffer) Write(b []byte) (int, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.buffer.Write(b)
}

// What was written so far.
func (self *lockedLogBuffer) String() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.buffer.String()
}

// The command with --nlayer-hop relays every forward to its hops: a client of
// this extender reaches the site behind the private hop, whose secret the
// command read from its file, a hop that cannot be dialed is held and logged,
// both hops are logged at start, and the secret appears in no log line.
func TestExtenderCommandRelaysToNLayerHops(t *testing.T) {
	// the command logs through the package loggers, which keep writing where
	// they did and also into the buffer this test reads
	logBuffer := &lockedLogBuffer{}
	outWriter := Out.Writer()
	errWriter := Err.Writer()
	Out.SetOutput(io.MultiWriter(logBuffer, outWriter))
	Err.SetOutput(io.MultiWriter(logBuffer, errWriter))
	t.Cleanup(func() {
		Out.SetOutput(outWriter)
		Err.SetOutput(errWriter)
	})

	operator := newTestExtenderOperator(t)
	operatorAddress := operator.server.Listener.Addr().String()
	_, operatorPort, err := net.SplitHostPort(operatorAddress)
	if err != nil {
		t.Fatal(err)
	}
	siteAddress, siteRootCas := startTestSite(t, "through the hop")
	hopPort := startTestHopExtender(t, []string{testHopSecret}, siteAddress)
	brokenListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	brokenPort := brokenListener.Addr().(*net.TCPAddr).Port
	brokenListener.Close()

	secretPath := writeTestSecretFile(t, testHopSecret+"\n")
	nlayerHops := []*connect.ExtenderConfig{}
	for _, spec := range []string{
		fmt.Sprintf("tcp://127.0.0.1:%d?secret_file=%s", hopPort, secretPath),
		fmt.Sprintf("127.0.0.1:%d", brokenPort),
	} {
		hopConfig, err := parseExtenderHopSpec(spec)
		if err != nil {
			t.Fatal(err)
		}
		nlayerHops = append(nlayerHops, hopConfig)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcpPort := tcpListener.Addr().(*net.TCPAddr).Port
	runs := make(chan *extenderRun, 1)
	options := &extenderOptions{
		jwt:      "test-jwt",
		apiUrl:   fmt.Sprintf("http://%s:%s", testExtenderApiHost, operatorPort),
		stateDir: t.TempDir(),
		// the udp carriers are configured apart, as a carrier must be, and
		// never bind here
		tcpPort:    tcpPort,
		udpPort:    brokenPort,
		dnsPort:    hopPort,
		nlayerHops: nlayerHops,
		listen: func(network string, address string) (net.Listener, error) {
			return tcpListener, nil
		},
		listenPacket: func(network string, address string) (net.PacketConn, error) {
			return nil, fmt.Errorf("this test serves tcp only")
		},
		dialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", operatorAddress)
		},
		configureNetworkClient: func(settings *connect.ExtenderNetworkClientSettings) {
			settings.ResolveDns = func(ctx context.Context, name string) ([]netip.Addr, error) {
				return nil, nil
			}
		},
		onStart: func(run *extenderRun) {
			runs <- run
		},
	}
	runDone := make(chan error, 1)
	go func() {
		runDone <- runExtender(ctx, options)
	}()
	var run *extenderRun
	select {
	case run = <-runs:
	case err := <-runDone:
		t.Fatalf("the extender exited before it started: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("the extender did not start")
	}

	connectSettings := connect.DefaultConnectSettings()
	connectSettings.TlsConfig = &tls.Config{RootCAs: siteRootCas}
	client := connect.NewExtenderHttpClient(connectSettings, &connect.ExtenderConfig{
		Profile: connect.ExtenderProfile{
			ConnectMode: connect.ExtenderConnectModeTcpTls,
			Port:        tcpPort,
		},
		Ip: netip.MustParseAddr("127.0.0.1"),
	})
	defer client.CloseIdleConnections()
	// the broken hop is picked at random; every connection still reaches the
	// site through the private hop within its two attempts
	for i := 0; run.server.NLayerStats()[1].FailedCount == 0; i += 1 {
		if 32 <= i {
			t.Fatal("the broken hop was never picked")
		}
		response, err := client.Get(fmt.Sprintf("https://%s/", testExtenderApiHost))
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		client.CloseIdleConnections()
		if err != nil || string(body) != "through the hop" {
			t.Fatalf("dial %d body = %q, %v", i, body, err)
		}
	}
	if hopStats := run.server.NLayerStats(); hopStats[0].RelayCount == 0 || hopStats[0].RefusedCount != 0 || hopStats[1].HeldUntil.IsZero() {
		t.Fatalf("stats = %+v, expected relays through the private hop and the broken one held", hopStats)
	}

	logs := logBuffer.String()
	for _, line := range []string{
		"extender nlayer: relaying every forward to one of 2 hops",
		fmt.Sprintf("extender nlayer hop 0: tcp 127.0.0.1:%d private", hopPort),
		fmt.Sprintf("extender nlayer hop 1: tcp 127.0.0.1:%d", brokenPort),
		fmt.Sprintf("extender nlayer hop 1 (tcp 127.0.0.1:%d) held for 30s: ", brokenPort),
	} {
		if !strings.Contains(logs, line) {
			t.Fatalf("the log has no %q:\n%s", line, logs)
		}
	}
	if strings.Contains(logs, testHopSecret) {
		t.Fatal("the log carries the hop's secret")
	}

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the extender did not stop")
	}
}
