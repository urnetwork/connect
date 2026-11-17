package connect

// net_tls_hello_test.go — the hello of the normal and resilient dialers
// against local Go tls servers with a private root (net_tls_hello.go): the
// hello as the server reads it, handshakes and requests over every dialer, h2
// on the api path, http/1.1 on the websocket path, resumption, the hello
// retry, certificate verification, the error types and the kill switch.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	utls "github.com/refraction-networking/utls"
	"golang.org/x/crypto/cryptobyte"
)

// The synthetic name the hello test servers are certified for.
const testTlsHelloServerName = "api.tls-hello.example"

// The Chrome 133 hello as a server reads it, grease aside.
var (
	testChromeCipherSuites = []uint16{
		0x1301, 0x1302, 0x1303,
		0xc02b, 0xc02f, 0xc02c, 0xc030, 0xcca9, 0xcca8,
		0xc013, 0xc014, 0x009c, 0x009d, 0x002f, 0x0035,
	}
	testChromeSupportedGroups   = []uint16{0x11ec, 0x001d, 0x0017, 0x0018}
	testChromeKeyShareGroups    = []uint16{0x11ec, 0x001d}
	testChromeSupportedVersions = []uint16{0x0304, 0x0303}
	// sorted: alpn (16) goes only with an offered protocol, alps (17613)
	// only with an offered h2
	testChromeExtensionTypes = []uint16{0, 5, 10, 11, 13, 16, 18, 23, 27, 35, 43, 45, 51, 17613, 65037, 65281}
)

const (
	testTlsExtensionServerName        = 0
	testTlsExtensionSupportedGroups   = 10
	testTlsExtensionAlpn              = 16
	testTlsExtensionPreSharedKey      = 41
	testTlsExtensionSupportedVersions = 43
	testTlsExtensionKeyShare          = 51
	testTlsExtensionAlps              = 17613
)

// A local https server certified for testTlsHelloServerName by a private
// root, offering h2 and http/1.1. It records each connection's client hello
// as it arrived, before its tls server reads it.
type testTlsHelloServer struct {
	server       *httptest.Server
	rootCertPool *x509.CertPool

	stateLock sync.Mutex
	hellos    []*testCapturedHello
}

// One connection's client hello as it arrived.
type testCapturedHello struct {
	// the tls records it arrived in: more than one only when fragmented
	recordCount int
	message     []byte
}

// Starts a hello test server. configure, when set, adjusts the server's tls
// configuration before it starts.
func newTestTlsHelloServer(t *testing.T, handler http.Handler, configure func(*tls.Config)) *testTlsHelloServer {
	t.Helper()
	rootCertPool, certificate := newTestTlsHelloCertificates(t, testTlsHelloServerName)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	helloServer := &testTlsHelloServer{
		rootCertPool: rootCertPool,
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener.Close()
	server.Listener = &testHelloCaptureListener{Listener: listener, server: helloServer}
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{certificate},
		NextProtos:   []string{"h2", "http/1.1"},
	}
	if configure != nil {
		configure(server.TLS)
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	helloServer.server = server
	return helloServer
}

// A private root and a leaf it certifies for serverName.
func newTestTlsHelloCertificates(t *testing.T, serverName string) (*x509.CertPool, tls.Certificate) {
	t.Helper()
	now := time.Now()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rootTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "tls hello test root"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	rootDer, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(rootDer)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: serverName},
		DNSNames:     []string{serverName},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDer, err := x509.CreateCertificate(rand.Reader, leafTemplate, root, &leafKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	rootCertPool := x509.NewCertPool()
	rootCertPool.AddCert(root)
	return rootCertPool, tls.Certificate{Certificate: [][]byte{leafDer}, PrivateKey: leafKey}
}

// Records one connection's hello.
func (self *testTlsHelloServer) addHello(hello *testCapturedHello) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.hellos = append(self.hellos, hello)
}

// The captured hellos in arrival order. A hello is captured before the server
// answers it, so a completed dial's hello is always here.
func (self *testTlsHelloServer) capturedHellos() []*testCapturedHello {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.hellos)
}

// The server's authority under its certified name.
func (self *testTlsHelloServer) authority() string {
	return self.authorityFor(testTlsHelloServerName)
}

// The server's authority under serverName, which every dial reaches.
func (self *testTlsHelloServer) authorityFor(serverName string) string {
	port := self.server.Listener.Addr().(*net.TCPAddr).Port
	return net.JoinHostPort(serverName, fmt.Sprintf("%d", port))
}

// Strategy settings whose dials reach the server whatever name they dial,
// trusting its root as production trusts the pinned roots.
func (self *testTlsHelloServer) clientStrategySettings(t *testing.T) *ClientStrategySettings {
	t.Helper()
	tlsConfig, err := DefaultTlsConfig()
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig.RootCAs = self.rootCertPool
	settings := DefaultClientStrategySettings()
	settings.TlsConfig = tlsConfig
	listenerAddress := self.server.Listener.Addr().String()
	settings.ConnectSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp4", listenerAddress)
		},
	}
	return settings
}

// Hands each accepted connection to a capture of its client hello.
type testHelloCaptureListener struct {
	net.Listener
	server *testTlsHelloServer
}

// The next connection, behind a capture of its hello.
func (self *testHelloCaptureListener) Accept() (net.Conn, error) {
	conn, err := self.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &testHelloCaptureConn{Conn: conn, server: self.server}, nil
}

// Keeps the bytes its tls server reads until they hold a complete client
// hello. Read is called only by the goroutine serving the connection.
type testHelloCaptureConn struct {
	net.Conn
	server   *testTlsHelloServer
	raw      []byte
	captured bool
}

// Reads through, keeping what was read until the hello is complete.
func (self *testHelloCaptureConn) Read(b []byte) (int, error) {
	n, err := self.Conn.Read(b)
	if !self.captured && 0 < n {
		self.raw = append(self.raw, b[:n]...)
		if hello := parseTestHelloRecords(self.raw); hello != nil {
			self.captured = true
			self.raw = nil
			self.server.addHello(hello)
		}
	}
	return n, err
}

// The client hello at the start of raw once raw holds all of it, its records'
// handshake payloads joined. Nil while it is incomplete, or when raw does not
// start with handshake records.
func parseTestHelloRecords(raw []byte) *testCapturedHello {
	var message []byte
	recordCount := 0
	for 5 <= len(raw) {
		if raw[0] != TlsContentTypeHandshake {
			return nil
		}
		length := int(binary.BigEndian.Uint16(raw[3:5]))
		if len(raw) < 5+length {
			return nil
		}
		message = append(message, raw[5:5+length]...)
		raw = raw[5+length:]
		recordCount += 1
		if 4 <= len(message) {
			messageLength := 4 + (int(message[1])<<16 | int(message[2])<<8 | int(message[3]))
			if messageLength <= len(message) {
				return &testCapturedHello{recordCount: recordCount, message: message[:messageLength]}
			}
		}
	}
	return nil
}

// Answers "ok" at /, and at /ws upgrades to a websocket that sends "ok".
func testTlsHelloHandler() http.Handler {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		if err := connection.WriteMessage(websocket.TextMessage, []byte("ok")); err != nil {
			return
		}
		// held until the client closes
		_, _, _ = connection.ReadMessage()
	})
	return mux
}

// A dialer kind under test: the normal dialer or one of the resilient ones.
type testTlsHelloDialer struct {
	description string
	resilient   bool
	fragment    bool
	reorder     bool
	segment     bool
}

var testTlsHelloDialers = []testTlsHelloDialer{
	{description: "normal"},
	{description: "fragment", resilient: true, fragment: true},
	{description: "reorder", resilient: true, reorder: true},
	{description: "fragment+reorder", resilient: true, fragment: true, reorder: true},
	{description: "fragment+segment", resilient: true, fragment: true, segment: true},
}

// The strategy dialer of this kind over settings, built as NewClientStrategy
// builds it.
func (self testTlsHelloDialer) clientDialer(settings *ClientStrategySettings) *clientDialer {
	dialer := &clientDialer{
		description: self.description,
		settings:    settings,
	}
	if self.resilient {
		dialer.dialTlsContext = newResilientDialTlsContext(&settings.ConnectSettings, self.fragment, self.reorder, self.segment, clientWebSocketNextProtos)
		dialer.httpDialTlsContext = newResilientDialTlsContext(&settings.ConnectSettings, self.fragment, self.reorder, self.segment, clientHttpNextProtos)
	} else {
		dialer.dialTlsContext = newNormalDialTlsContext(settings, clientWebSocketNextProtos)
		dialer.httpDialTlsContext = newNormalDialTlsContext(settings, clientHttpNextProtos)
	}
	return dialer
}

// Requests / of the server at authority through client and checks the
// answer. The body is read and closed; the response keeps its tls state.
func testTlsHelloApiRequest(t *testing.T, client *http.Client, authority string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+authority+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("api request: %s", err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("read api response: %s", err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("api response = %d %q, want 200 ok", response.StatusCode, body)
	}
	return response
}

// Dials the server's websocket at authority through dialer and reads its
// greeting. Returns the tls connection under the websocket, open until the
// test ends.
func testTlsHelloWebSocket(t *testing.T, dialer *clientDialer, authority string) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	connection, response, err := dialer.WsDialer(dialer.settings).DialContext(ctx, "wss://"+authority+"/ws", nil)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		t.Fatalf("websocket dial: %s", err)
	}
	t.Cleanup(func() { connection.Close() })
	if err := connection.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, message, err := connection.ReadMessage()
	if err != nil || string(message) != "ok" {
		t.Fatalf("websocket greeting = %q, %v", message, err)
	}
	batchConnection, ok := connection.UnderlyingConn().(*WebSocketWriteBatchConn)
	if !ok {
		t.Fatalf("websocket transport is %T", connection.UnderlyingConn())
	}
	return batchConnection.conn
}

// The parts of a client hello the tests check.
type testClientHello struct {
	cipherSuites      []uint16
	extensionTypes    []uint16
	serverName        string
	alpnProtocols     []string
	alpsProtocols     []string
	supportedGroups   []uint16
	keyShareGroups    []uint16
	supportedVersions []uint16
}

// The parts of a client hello handshake message the tests check. A malformed
// message fails the test.
func parseTestClientHello(t *testing.T, message []byte) *testClientHello {
	t.Helper()
	malformed := func(part string) {
		t.Helper()
		t.Fatalf("malformed client hello: %s", part)
	}
	readUint16s := func(list cryptobyte.String) []uint16 {
		var values []uint16
		for !list.Empty() {
			var value uint16
			if !list.ReadUint16(&value) {
				malformed("uint16 list")
			}
			values = append(values, value)
		}
		return values
	}
	// the alpn and alps lists share one shape
	readProtocols := func(data cryptobyte.String) []string {
		var list cryptobyte.String
		if !data.ReadUint16LengthPrefixed(&list) || !data.Empty() {
			malformed("protocol list")
		}
		var protocols []string
		for !list.Empty() {
			var protocol cryptobyte.String
			if !list.ReadUint8LengthPrefixed(&protocol) {
				malformed("protocol")
			}
			protocols = append(protocols, string(protocol))
		}
		return protocols
	}

	input := cryptobyte.String(message)
	var messageType uint8
	var body cryptobyte.String
	if !input.ReadUint8(&messageType) || messageType != 1 || !input.ReadUint24LengthPrefixed(&body) || !input.Empty() {
		malformed("handshake header")
	}
	var sessionId, cipherSuites, compressionMethods, extensions cryptobyte.String
	if !body.Skip(2+32) ||
		!body.ReadUint8LengthPrefixed(&sessionId) ||
		!body.ReadUint16LengthPrefixed(&cipherSuites) ||
		!body.ReadUint8LengthPrefixed(&compressionMethods) ||
		!body.ReadUint16LengthPrefixed(&extensions) ||
		!body.Empty() {
		malformed("body")
	}
	hello := &testClientHello{
		cipherSuites: readUint16s(cipherSuites),
	}
	for !extensions.Empty() {
		var extensionType uint16
		var data cryptobyte.String
		if !extensions.ReadUint16(&extensionType) || !extensions.ReadUint16LengthPrefixed(&data) {
			malformed("extension")
		}
		hello.extensionTypes = append(hello.extensionTypes, extensionType)
		switch extensionType {
		case testTlsExtensionServerName:
			var names, name cryptobyte.String
			var nameType uint8
			if !data.ReadUint16LengthPrefixed(&names) || !names.ReadUint8(&nameType) || nameType != 0 || !names.ReadUint16LengthPrefixed(&name) {
				malformed("server name")
			}
			hello.serverName = string(name)
		case testTlsExtensionAlpn:
			hello.alpnProtocols = readProtocols(data)
		case testTlsExtensionAlps:
			hello.alpsProtocols = readProtocols(data)
		case testTlsExtensionSupportedGroups:
			var groups cryptobyte.String
			if !data.ReadUint16LengthPrefixed(&groups) {
				malformed("supported groups")
			}
			hello.supportedGroups = readUint16s(groups)
		case testTlsExtensionKeyShare:
			var shares cryptobyte.String
			if !data.ReadUint16LengthPrefixed(&shares) {
				malformed("key shares")
			}
			for !shares.Empty() {
				var group uint16
				var key cryptobyte.String
				if !shares.ReadUint16(&group) || !shares.ReadUint16LengthPrefixed(&key) {
					malformed("key share")
				}
				hello.keyShareGroups = append(hello.keyShareGroups, group)
			}
		case testTlsExtensionSupportedVersions:
			var versions cryptobyte.String
			if !data.ReadUint8LengthPrefixed(&versions) {
				malformed("supported versions")
			}
			hello.supportedVersions = readUint16s(versions)
		}
	}
	return hello
}

// Whether value is a grease value (rfc 8701): 0x?a?a with equal bytes.
func isTestGreaseValue(value uint16) bool {
	return value&0x0f0f == 0x0a0a && value>>8 == value&0xff
}

// values without its grease
func testWithoutGrease(values []uint16) []uint16 {
	return slices.DeleteFunc(slices.Clone(values), isTestGreaseValue)
}

// Checks that hello is the Chrome profile offering alpnProtocols to
// testTlsHelloServerName.
func assertTestChromeClientHello(t *testing.T, hello *testClientHello, alpnProtocols []string) {
	t.Helper()
	// grease leads each list it is in, and opens and closes the extensions
	// (before a pre_shared_key, which is always last)
	leadingGrease := func(name string, values []uint16, want []uint16) {
		t.Helper()
		if len(values) == 0 || !isTestGreaseValue(values[0]) || !slices.Equal(values[1:], want) {
			t.Fatalf("%s = %04x, want grease then %04x", name, values, want)
		}
	}
	leadingGrease("cipher suites", hello.cipherSuites, testChromeCipherSuites)
	leadingGrease("supported groups", hello.supportedGroups, testChromeSupportedGroups)
	leadingGrease("key share groups", hello.keyShareGroups, testChromeKeyShareGroups)
	leadingGrease("supported versions", hello.supportedVersions, testChromeSupportedVersions)

	extensionTypes := hello.extensionTypes
	if 0 < len(extensionTypes) && extensionTypes[len(extensionTypes)-1] == testTlsExtensionPreSharedKey {
		extensionTypes = extensionTypes[:len(extensionTypes)-1]
	}
	if len(extensionTypes) < 2 || !isTestGreaseValue(extensionTypes[0]) || !isTestGreaseValue(extensionTypes[len(extensionTypes)-1]) {
		t.Fatalf("extensions %d do not open and close with grease", hello.extensionTypes)
	}
	middleTypes := slices.Clone(extensionTypes[1 : len(extensionTypes)-1])
	slices.Sort(middleTypes)
	h2 := slices.Contains(alpnProtocols, "h2")
	wantTypes := slices.DeleteFunc(slices.Clone(testChromeExtensionTypes), func(extensionType uint16) bool {
		switch extensionType {
		case testTlsExtensionAlpn:
			return len(alpnProtocols) == 0
		case testTlsExtensionAlps:
			return !h2
		}
		return false
	})
	if !slices.Equal(middleTypes, wantTypes) {
		t.Fatalf("extensions (sorted, grease aside) = %d, want %d", middleTypes, wantTypes)
	}

	if !slices.Equal(hello.alpnProtocols, alpnProtocols) {
		t.Fatalf("alpn = %q, want %q", hello.alpnProtocols, alpnProtocols)
	}
	var wantAlpsProtocols []string
	if h2 {
		wantAlpsProtocols = []string{"h2"}
	}
	if !slices.Equal(hello.alpsProtocols, wantAlpsProtocols) {
		t.Fatalf("alps = %q, want %q", hello.alpsProtocols, wantAlpsProtocols)
	}
	if hello.serverName != testTlsHelloServerName {
		t.Fatalf("sni = %q, want %q", hello.serverName, testTlsHelloServerName)
	}
}

// Checks that hello is Go's own, offering alpnProtocols to
// testTlsHelloServerName: no grease anywhere and no alps.
func assertTestGoClientHello(t *testing.T, hello *testClientHello, alpnProtocols []string) {
	t.Helper()
	for _, values := range [][]uint16{hello.cipherSuites, hello.extensionTypes, hello.supportedGroups, hello.keyShareGroups, hello.supportedVersions} {
		if slices.ContainsFunc(values, isTestGreaseValue) {
			t.Fatalf("Go's hello carries grease: %04x", values)
		}
	}
	if slices.Contains(hello.extensionTypes, testTlsExtensionAlps) {
		t.Fatal("Go's hello offers alps")
	}
	if !slices.Equal(hello.alpnProtocols, alpnProtocols) {
		t.Fatalf("alpn = %q, want %q", hello.alpnProtocols, alpnProtocols)
	}
	if hello.serverName != testTlsHelloServerName {
		t.Fatalf("sni = %q, want %q", hello.serverName, testTlsHelloServerName)
	}
}

// Checks hello against what a path offering alpnProtocols presents by
// default: the Chrome hello, except on an h2 path where net/http cannot read
// the uTLS connection state (net_tls_hello_go126.go).
func assertTestDefaultClientHello(t *testing.T, hello *testClientHello, alpnProtocols []string) {
	t.Helper()
	if slices.Contains(alpnProtocols, "h2") && !httpTransportReadsTlsConnectionState {
		assertTestGoClientHello(t, hello, alpnProtocols)
		return
	}
	assertTestChromeClientHello(t, hello, alpnProtocols)
}

// The connection state of a dialed tls connection, whichever hello made it.
func testTlsConnectionState(t *testing.T, conn net.Conn) tls.ConnectionState {
	t.Helper()
	stater, ok := conn.(interface{ ConnectionState() tls.ConnectionState })
	if !ok {
		t.Fatalf("dialed connection %T has no tls connection state", conn)
	}
	return stater.ConnectionState()
}

// The hello is the newest Chrome profile of the vendored uTLS. An upgrade that
// moves Auto past it fails here, so the profile, and these tests' picture of
// it, moves only deliberately.
func TestChromeClientHelloIdIsTheNewestChromeProfile(t *testing.T) {
	if chromeClientHelloId != utls.HelloChrome_Auto {
		t.Fatalf(
			"the hello is %s %s, but the newest Chrome profile of uTLS is %s %s",
			chromeClientHelloId.Client, chromeClientHelloId.Version,
			utls.HelloChrome_Auto.Client, utls.HelloChrome_Auto.Version,
		)
	}
}

// The api dial of the normal dialer presents the Chrome hello: grease, the
// profile's suites, extensions, groups, key shares and versions, the path's
// alpn, and the host as sni. Two first contacts shuffle the extensions
// differently, as Chrome does per connection; two shuffles of the profile's
// 16 extensions agree with a chance of 1 in 16!, about 5e-14.
func TestNormalDialPresentsChromeClientHello(t *testing.T) {
	server := newTestTlsHelloServer(t, testTlsHelloHandler(), nil)
	for range 2 {
		// a new dialer, and so a new session cache: the second dial is a first
		// contact too
		dialer := testTlsHelloDialers[0].clientDialer(server.clientStrategySettings(t))
		client := dialer.HttpClient()
		response := testTlsHelloApiRequest(t, client, server.authority())
		client.CloseIdleConnections()
		if response.ProtoMajor != 2 {
			t.Fatalf("api request negotiated HTTP/%d, want HTTP/2", response.ProtoMajor)
		}
	}
	hellos := server.capturedHellos()
	if len(hellos) != 2 {
		t.Fatalf("captured %d hellos, want 2", len(hellos))
	}
	first := parseTestClientHello(t, hellos[0].message)
	second := parseTestClientHello(t, hellos[1].message)
	for _, hello := range []*testClientHello{first, second} {
		assertTestDefaultClientHello(t, hello, clientHttpNextProtos)
		if slices.Contains(hello.extensionTypes, testTlsExtensionPreSharedKey) {
			t.Fatal("a first contact offered a pre_shared_key")
		}
	}
	if httpTransportReadsTlsConnectionState {
		if slices.Equal(testWithoutGrease(first.extensionTypes), testWithoutGrease(second.extensionTypes)) {
			t.Fatalf("two dials sent the extensions in the same order %d", first.extensionTypes)
		}
	}
}

// The websocket dial presents the Chrome hello with http/1.1 alone and no
// alps, as Chrome sends for a websocket, and negotiates http/1.1 with a server
// that prefers h2.
func TestWebSocketDialPresentsChromeClientHelloWithHttp11Only(t *testing.T) {
	server := newTestTlsHelloServer(t, testTlsHelloHandler(), nil)
	dialer := testTlsHelloDialers[0].clientDialer(server.clientStrategySettings(t))
	conn := testTlsHelloWebSocket(t, dialer, server.authority())
	if _, ok := conn.(*chromeTlsConn); !ok {
		t.Fatalf("websocket tls connection is %T, want the Chrome hello's", conn)
	}
	if negotiated := testTlsConnectionState(t, conn).NegotiatedProtocol; negotiated != "http/1.1" {
		t.Fatalf("websocket negotiated %q, want http/1.1", negotiated)
	}
	hellos := server.capturedHellos()
	if len(hellos) != 1 {
		t.Fatalf("captured %d hellos, want 1", len(hellos))
	}
	assertTestChromeClientHello(t, parseTestClientHello(t, hellos[0].message), clientWebSocketNextProtos)
}

// Every dialer, normal and resilient, carries its hello to a working request
// against a server with a private root: the api path negotiates h2, the
// websocket path http/1.1. The fragmenting dialers deliver the hello in
// several records and the others in one, so fragmentation takes the larger
// hello as it took Go's.
func TestDialersCarryChromeClientHelloToRequests(t *testing.T) {
	for _, testDialer := range testTlsHelloDialers {
		server := newTestTlsHelloServer(t, testTlsHelloHandler(), nil)
		dialer := testDialer.clientDialer(server.clientStrategySettings(t))

		response := testTlsHelloApiRequest(t, dialer.HttpClient(), server.authority())
		if response.ProtoMajor != 2 || response.TLS == nil || response.TLS.NegotiatedProtocol != "h2" {
			t.Fatalf("%s: api request negotiated HTTP/%d (tls %v), want h2", testDialer.description, response.ProtoMajor, response.TLS)
		}
		conn := testTlsHelloWebSocket(t, dialer, server.authority())
		if negotiated := testTlsConnectionState(t, conn).NegotiatedProtocol; negotiated != "http/1.1" {
			t.Fatalf("%s: websocket negotiated %q, want http/1.1", testDialer.description, negotiated)
		}

		hellos := server.capturedHellos()
		if len(hellos) != 2 {
			t.Fatalf("%s: captured %d hellos, want 2", testDialer.description, len(hellos))
		}
		for i, alpnProtocols := range [][]string{clientHttpNextProtos, clientWebSocketNextProtos} {
			assertTestDefaultClientHello(t, parseTestClientHello(t, hellos[i].message), alpnProtocols)
			if testDialer.fragment {
				if hellos[i].recordCount < 2 {
					t.Fatalf("%s: the hello arrived in %d record, want fragments", testDialer.description, hellos[i].recordCount)
				}
			} else if hellos[i].recordCount != 1 {
				t.Fatalf("%s: the hello arrived in %d records, want 1", testDialer.description, hellos[i].recordCount)
			}
		}
	}
}

// A second dial of a path resumes the session of its first and offers the
// ticket as the final extension, as Chrome does; the other path of the same
// dialer has its own cache and offers nothing, so a ticket never links paths.
// Also the regression for a session cache with a spec that cannot carry the
// tls 1.3 ticket, which uTLS answers with a panic on the second dial.
func TestChromeClientHelloResumesSessionsPerPath(t *testing.T) {
	for _, testDialer := range []testTlsHelloDialer{testTlsHelloDialers[0], testTlsHelloDialers[3]} {
		server := newTestTlsHelloServer(t, testTlsHelloHandler(), nil)
		dialer := testDialer.clientDialer(server.clientStrategySettings(t))
		client := dialer.HttpClient()

		first := testTlsHelloApiRequest(t, client, server.authority())
		client.CloseIdleConnections()
		second := testTlsHelloApiRequest(t, client, server.authority())
		if first.TLS.DidResume || !second.TLS.DidResume {
			t.Fatalf("%s: resumed first=%t second=%t, want only the second", testDialer.description, first.TLS.DidResume, second.TLS.DidResume)
		}
		conn := testTlsHelloWebSocket(t, dialer, server.authority())
		if testTlsConnectionState(t, conn).DidResume {
			t.Fatalf("%s: the websocket path resumed the api path's session", testDialer.description)
		}

		hellos := server.capturedHellos()
		if len(hellos) != 3 {
			t.Fatalf("%s: captured %d hellos, want 3", testDialer.description, len(hellos))
		}
		for i, alpnProtocols := range [][]string{clientHttpNextProtos, clientHttpNextProtos, clientWebSocketNextProtos} {
			hello := parseTestClientHello(t, hellos[i].message)
			assertTestDefaultClientHello(t, hello, alpnProtocols)
			offered := slices.Contains(hello.extensionTypes, testTlsExtensionPreSharedKey)
			if offered != (i == 1) {
				t.Fatalf("%s: hello %d offered a pre_shared_key: %t", testDialer.description, i, offered)
			}
			if offered && hello.extensionTypes[len(hello.extensionTypes)-1] != testTlsExtensionPreSharedKey {
				t.Fatalf("%s: the pre_shared_key is not last in %d", testDialer.description, hello.extensionTypes)
			}
		}
	}
}

// A server that wants a key share the hello did not send (P-256) answers with
// a hello retry. The handshake completes on the retried hello, through the
// fragment/reorder layer too, and the path keeps none of that server's
// tickets: uTLS cannot retry a hello that offered one, so each later dial is a
// first contact that retries again.
func TestChromeClientHelloCompletesHelloRetry(t *testing.T) {
	for _, testDialer := range []testTlsHelloDialer{testTlsHelloDialers[0], testTlsHelloDialers[3]} {
		server := newTestTlsHelloServer(t, testTlsHelloHandler(), func(config *tls.Config) {
			config.CurvePreferences = []tls.CurveID{tls.CurveP256}
		})
		dialer := testDialer.clientDialer(server.clientStrategySettings(t))

		response := testTlsHelloApiRequest(t, dialer.HttpClient(), server.authority())
		if response.ProtoMajor != 2 {
			t.Fatalf("%s: api request negotiated HTTP/%d, want HTTP/2", testDialer.description, response.ProtoMajor)
		}
		for range 2 {
			conn := testTlsHelloWebSocket(t, dialer, server.authority())
			if testTlsConnectionState(t, conn).DidResume {
				t.Fatalf("%s: a websocket dial resumed a session of a retrying server", testDialer.description)
			}
		}

		hellos := server.capturedHellos()
		if len(hellos) != 3 {
			t.Fatalf("%s: captured %d first hellos, want 3", testDialer.description, len(hellos))
		}
		for i, captured := range hellos {
			hello := parseTestClientHello(t, captured.message)
			if slices.Contains(hello.keyShareGroups, uint16(tls.CurveP256)) {
				t.Fatalf("%s: hello %d already shares P-256, so nothing was retried", testDialer.description, i)
			}
			if slices.Contains(hello.extensionTypes, testTlsExtensionPreSharedKey) {
				t.Fatalf("%s: hello %d offered a ticket to a retrying server", testDialer.description, i)
			}
		}
	}
}

// A server that starts asking for a hello retry after it issued a ticket
// costs one dial: uTLS fails the hello that offered the ticket, and drops the
// ticket. The next dial is a first contact that retries, and from then on the
// path keeps none of that server's tickets, so no later dial fails.
func TestChromeClientHelloRecoversWhenServerStartsHelloRetry(t *testing.T) {
	var retrying atomic.Bool
	server := newTestTlsHelloServer(t, testTlsHelloHandler(), func(config *tls.Config) {
		config.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
			if !retrying.Load() {
				return nil, nil
			}
			retryConfig := config.Clone()
			retryConfig.GetConfigForClient = nil
			retryConfig.CurvePreferences = []tls.CurveID{tls.CurveP256}
			return retryConfig, nil
		}
	})
	settings := server.clientStrategySettings(t)
	dialer := testTlsHelloDialers[0].clientDialer(settings)
	// one websocket dial; the upgrade response is read, and with it the
	// server's ticket
	dial := func() error {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		connection, response, err := dialer.WsDialer(settings).DialContext(ctx, "wss://"+server.authority()+"/ws", nil)
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if err != nil {
			return err
		}
		return connection.Close()
	}

	if err := dial(); err != nil {
		t.Fatalf("first contact: %s", err)
	}
	retrying.Store(true)
	if err := dial(); err == nil {
		t.Fatal("the dial that offered a ticket to a retrying server succeeded, so the retry was not exercised")
	}
	for i := range 2 {
		if err := dial(); err != nil {
			t.Fatalf("dial %d after the failed resumption: %s", i, err)
		}
	}

	hellos := server.capturedHellos()
	if len(hellos) != 4 {
		t.Fatalf("captured %d hellos, want 4", len(hellos))
	}
	for i, captured := range hellos {
		offered := slices.Contains(parseTestClientHello(t, captured.message).extensionTypes, testTlsExtensionPreSharedKey)
		if offered != (i == 1) {
			t.Fatalf("hello %d offered a ticket: %t, want only hello 1 to", i, offered)
		}
	}
}

// A server that speaks tls 1.2 at most completes the Chrome hello over one of
// the profile's tls 1.2 suites, negotiates h2 for the api and http/1.1 for the
// websocket, and resumes a later dial from its session ticket.
func TestChromeClientHelloNegotiatesTls12(t *testing.T) {
	server := newTestTlsHelloServer(t, testTlsHelloHandler(), func(config *tls.Config) {
		config.MaxVersion = tls.VersionTLS12
	})
	dialer := testTlsHelloDialers[0].clientDialer(server.clientStrategySettings(t))
	client := dialer.HttpClient()

	first := testTlsHelloApiRequest(t, client, server.authority())
	client.CloseIdleConnections()
	second := testTlsHelloApiRequest(t, client, server.authority())
	for _, response := range []*http.Response{first, second} {
		if response.TLS.Version != tls.VersionTLS12 || response.ProtoMajor != 2 {
			t.Fatalf("api request negotiated tls %04x and HTTP/%d, want tls 1.2 and HTTP/2", response.TLS.Version, response.ProtoMajor)
		}
		if !slices.Contains(testChromeCipherSuites, response.TLS.CipherSuite) {
			t.Fatalf("negotiated suite %04x, which the Chrome hello does not offer", response.TLS.CipherSuite)
		}
	}
	if first.TLS.DidResume || !second.TLS.DidResume {
		t.Fatalf("resumed first=%t second=%t, want only the second", first.TLS.DidResume, second.TLS.DidResume)
	}
	conn := testTlsHelloWebSocket(t, dialer, server.authority())
	if state := testTlsConnectionState(t, conn); state.Version != tls.VersionTLS12 || state.NegotiatedProtocol != "http/1.1" {
		t.Fatalf("websocket negotiated tls %04x and %q, want tls 1.2 and http/1.1", state.Version, state.NegotiatedProtocol)
	}

	hellos := server.capturedHellos()
	if len(hellos) != 3 {
		t.Fatalf("captured %d hellos, want 3", len(hellos))
	}
	for i, alpnProtocols := range [][]string{clientHttpNextProtos, clientHttpNextProtos, clientWebSocketNextProtos} {
		assertTestDefaultClientHello(t, parseTestClientHello(t, hellos[i].message), alpnProtocols)
	}
}

// The kill switch: `TlsClientHelloFingerprintGo` restores Go's hello on every
// dialer and path, which still negotiates h2 for the api and http/1.1 for the
// websocket.
func TestTlsClientHelloFingerprintGoRestoresGoHello(t *testing.T) {
	for _, testDialer := range testTlsHelloDialers {
		server := newTestTlsHelloServer(t, testTlsHelloHandler(), nil)
		settings := server.clientStrategySettings(t)
		settings.TlsClientHelloFingerprint = TlsClientHelloFingerprintGo
		dialer := testDialer.clientDialer(settings)

		response := testTlsHelloApiRequest(t, dialer.HttpClient(), server.authority())
		if response.ProtoMajor != 2 {
			t.Fatalf("%s: api request negotiated HTTP/%d, want HTTP/2", testDialer.description, response.ProtoMajor)
		}
		conn := testTlsHelloWebSocket(t, dialer, server.authority())
		if _, ok := conn.(*tls.Conn); !ok {
			t.Fatalf("%s: websocket tls connection is %T, want Go's", testDialer.description, conn)
		}

		hellos := server.capturedHellos()
		if len(hellos) != 2 {
			t.Fatalf("%s: captured %d hellos, want 2", testDialer.description, len(hellos))
		}
		for i, alpnProtocols := range [][]string{clientHttpNextProtos, clientWebSocketNextProtos} {
			assertTestGoClientHello(t, parseTestClientHello(t, hellos[i].message), alpnProtocols)
		}
	}
}

// An established Chrome-hello connection keeps neither the hello's extensions
// nor its key share private keys, which only the handshake reads. The request,
// websocket and resumption tests run on such connections.
func TestChromeTlsConnReleasesHandshakeKeys(t *testing.T) {
	server := newTestTlsHelloServer(t, testTlsHelloHandler(), nil)
	dialTlsContext := newNormalDialTlsContext(server.clientStrategySettings(t), clientWebSocketNextProtos)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := dialTlsContext(ctx, "tcp", server.authority())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	chromeConn, ok := conn.(*chromeTlsConn)
	if !ok {
		t.Fatalf("dialed %T, want the Chrome hello's connection", conn)
	}
	if chromeConn.Extensions != nil || chromeConn.HandshakeState.State13.KeyShareKeys != nil {
		t.Fatal("the established connection keeps the hello's extensions or key share private keys")
	}
}

// The Chrome hello goes only where it carries the configuration exactly and
// the consumer can read what it negotiates; everywhere else Go's hello stays.
func TestChromeClientHelloAppliesOnlyWhereItCarriesTheConfig(t *testing.T) {
	defaultTlsConfig, err := DefaultTlsConfig()
	if err != nil {
		t.Fatal(err)
	}
	httpConfig := newClientTlsConfig(defaultTlsConfig, clientHttpNextProtos)
	webSocketConfig := newClientTlsConfig(defaultTlsConfig, clientWebSocketNextProtos)
	withConfig := func(config *tls.Config, change func(*tls.Config)) *tls.Config {
		changed := config.Clone()
		change(changed)
		return changed
	}
	cases := []struct {
		description             string
		fingerprint             string
		config                  *tls.Config
		readsTlsConnectionState bool
		want                    bool
	}{
		{description: "default", fingerprint: "", config: httpConfig, readsTlsConnectionState: true, want: true},
		{description: "chrome", fingerprint: TlsClientHelloFingerprintChrome, config: httpConfig, readsTlsConnectionState: true, want: true},
		{description: "go", fingerprint: TlsClientHelloFingerprintGo, config: httpConfig, readsTlsConnectionState: true, want: false},
		{description: "unknown fingerprint", fingerprint: "netscape", config: webSocketConfig, readsTlsConnectionState: true, want: false},
		{description: "h2 without the state", fingerprint: "", config: httpConfig, readsTlsConnectionState: false, want: false},
		{description: "http/1.1 without the state", fingerprint: "", config: webSocketConfig, readsTlsConnectionState: false, want: true},
		{description: "no alpn without the state", fingerprint: "", config: newClientTlsConfig(defaultTlsConfig, nil), readsTlsConnectionState: false, want: true},
		{description: "client certificate", fingerprint: "", config: withConfig(webSocketConfig, func(config *tls.Config) {
			config.Certificates = []tls.Certificate{{}}
		}), readsTlsConnectionState: true, want: false},
		{description: "client certificate callback", fingerprint: "", config: withConfig(webSocketConfig, func(config *tls.Config) {
			config.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return nil, nil }
		}), readsTlsConnectionState: true, want: false},
		{description: "ech", fingerprint: "", config: withConfig(webSocketConfig, func(config *tls.Config) {
			config.EncryptedClientHelloConfigList = []byte{0}
		}), readsTlsConnectionState: true, want: false},
		{description: "cipher suites", fingerprint: "", config: withConfig(webSocketConfig, func(config *tls.Config) {
			config.CipherSuites = []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256}
		}), readsTlsConnectionState: true, want: false},
		{description: "groups", fingerprint: "", config: withConfig(webSocketConfig, func(config *tls.Config) {
			config.CurvePreferences = []tls.CurveID{tls.X25519}
		}), readsTlsConnectionState: true, want: false},
		{description: "tls 1.3 minimum", fingerprint: "", config: withConfig(webSocketConfig, func(config *tls.Config) {
			config.MinVersion = tls.VersionTLS13
		}), readsTlsConnectionState: true, want: false},
		{description: "tls 1.2 maximum", fingerprint: "", config: withConfig(webSocketConfig, func(config *tls.Config) {
			config.MaxVersion = tls.VersionTLS12
		}), readsTlsConnectionState: true, want: false},
		{description: "tls 1.3 maximum", fingerprint: "", config: withConfig(webSocketConfig, func(config *tls.Config) {
			config.MaxVersion = tls.VersionTLS13
		}), readsTlsConnectionState: true, want: true},
		{description: "skip verify", fingerprint: "", config: withConfig(webSocketConfig, func(config *tls.Config) {
			config.InsecureSkipVerify = true
		}), readsTlsConnectionState: true, want: true},
	}
	for _, c := range cases {
		if got := chromeClientHelloApplies(c.fingerprint, c.config, c.readsTlsConnectionState); got != c.want {
			t.Errorf("%s: chromeClientHelloApplies = %t, want %t", c.description, got, c.want)
		}
	}
}

// The spec of a dial carries the path's protocols: its alpn list in Chrome's
// alpn extension, alps only beside an offered h2, no alpn extension for a path
// without protocols (NewResilientDialTlsContext), and the pre_shared_key
// last.
func TestChromeClientHelloSpecCarriesThePathProtocols(t *testing.T) {
	cases := []struct {
		nextProtos []string
		alps       bool
	}{
		{nextProtos: clientHttpNextProtos, alps: true},
		{nextProtos: clientWebSocketNextProtos, alps: false},
		{nextProtos: nil, alps: false},
	}
	for _, c := range cases {
		spec, err := chromeClientHelloSpec(c.nextProtos)
		if err != nil {
			t.Fatal(err)
		}
		var alpnProtocols []string
		alps := false
		for _, extension := range spec.Extensions {
			switch extension := extension.(type) {
			case *utls.ALPNExtension:
				alpnProtocols = extension.AlpnProtocols
			case *utls.ApplicationSettingsExtensionNew:
				alps = true
			}
		}
		if !slices.Equal(alpnProtocols, c.nextProtos) {
			t.Errorf("%q: the spec offers alpn %q", c.nextProtos, alpnProtocols)
		}
		if alps != c.alps {
			t.Errorf("%q: the spec offers alps: %t, want %t", c.nextProtos, alps, c.alps)
		}
		if _, ok := spec.Extensions[len(spec.Extensions)-1].(*utls.UtlsPreSharedKeyExtension); !ok {
			t.Errorf("%q: the last extension is %T, want the pre_shared_key", c.nextProtos, spec.Extensions[len(spec.Extensions)-1])
		}
	}
}

// The uTLS configuration carries the dial's server name and verification, its
// other settings that apply to a client, and the path's session cache, with an
// empty pre_shared_key omitted.
func TestChromeTlsConfigCarriesTheDialConfig(t *testing.T) {
	refused := errors.New("refused by the verifier")
	now := time.Now()
	keyLog := &bytes.Buffer{}
	var verifiedState tls.ConnectionState
	config := &tls.Config{
		Rand:               rand.Reader,
		Time:               func() time.Time { return now },
		RootCAs:            x509.NewCertPool(),
		ServerName:         testTlsHelloServerName,
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func([][]byte, [][]*x509.Certificate) error {
			return refused
		},
		VerifyConnection: func(state tls.ConnectionState) error {
			verifiedState = state
			return refused
		},
		SessionTicketsDisabled:      true,
		DynamicRecordSizingDisabled: true,
		KeyLogWriter:                keyLog,
	}
	sessionCache := newChromeSessionCache()
	chromeConfig := newChromeTlsConfig(config, sessionCache)

	if chromeConfig.Rand != config.Rand || chromeConfig.Time == nil || !chromeConfig.Time().Equal(now) {
		t.Error("the uTLS configuration lost the randomness or the clock")
	}
	if chromeConfig.RootCAs != config.RootCAs || chromeConfig.ServerName != config.ServerName || !chromeConfig.InsecureSkipVerify {
		t.Error("the uTLS configuration lost the roots, the server name or the skip")
	}
	if chromeConfig.VerifyPeerCertificate == nil || !errors.Is(chromeConfig.VerifyPeerCertificate(nil, nil), refused) {
		t.Error("the uTLS configuration lost the peer verifier")
	}
	if chromeConfig.VerifyConnection == nil || !errors.Is(chromeConfig.VerifyConnection(utls.ConnectionState{
		ServerName:         testTlsHelloServerName,
		NegotiatedProtocol: "h2",
		DidResume:          true,
	}), refused) {
		t.Error("the uTLS configuration lost the connection verifier")
	}
	if verifiedState.ServerName != testTlsHelloServerName || verifiedState.NegotiatedProtocol != "h2" || !verifiedState.DidResume {
		t.Errorf("the connection verifier saw %+v", verifiedState)
	}
	if !chromeConfig.SessionTicketsDisabled || !chromeConfig.DynamicRecordSizingDisabled || chromeConfig.KeyLogWriter != keyLog {
		t.Error("the uTLS configuration lost the ticket, record sizing or key log setting")
	}
	if chromeConfig.ClientSessionCache != utls.ClientSessionCache(sessionCache) || !chromeConfig.OmitEmptyPsk {
		t.Error("the uTLS configuration does not use the path's session cache with an empty pre_shared_key omitted")
	}
}

// The Chrome hello verifies the server as Go's does: the configured roots and
// the server name, the skip, and the peer and connection verifiers, with a
// refused certificate reported in crypto/tls's error type.
func TestChromeClientHelloKeepsCertificateVerification(t *testing.T) {
	server := newTestTlsHelloServer(t, testTlsHelloHandler(), nil)
	otherRootCertPool, _ := newTestTlsHelloCertificates(t, testTlsHelloServerName)
	refused := errors.New("refused by the verifier")
	cases := []struct {
		description string
		serverName  string
		configure   func(*tls.Config)
		check       func(conn net.Conn, err error) error
	}{
		{
			description: "the private root",
			serverName:  testTlsHelloServerName,
			configure:   func(*tls.Config) {},
			check: func(conn net.Conn, err error) error {
				if err != nil {
					return err
				}
				if state := testTlsConnectionState(t, conn); len(state.VerifiedChains) == 0 || state.ServerName != testTlsHelloServerName {
					return fmt.Errorf("state verified %d chains for %q", len(state.VerifiedChains), state.ServerName)
				}
				return nil
			},
		},
		{
			description: "another root",
			serverName:  testTlsHelloServerName,
			configure: func(config *tls.Config) {
				config.RootCAs = otherRootCertPool
			},
			check: func(conn net.Conn, err error) error {
				var verificationErr *tls.CertificateVerificationError
				var unknownAuthorityErr x509.UnknownAuthorityError
				if !errors.As(err, &verificationErr) || !errors.As(err, &unknownAuthorityErr) {
					return fmt.Errorf("err = %v (%T), want a certificate verification error from an unknown authority", err, err)
				}
				return nil
			},
		},
		{
			description: "another name",
			serverName:  "other.tls-hello.example",
			configure:   func(*tls.Config) {},
			check: func(conn net.Conn, err error) error {
				var verificationErr *tls.CertificateVerificationError
				var hostnameErr x509.HostnameError
				if !errors.As(err, &verificationErr) || !errors.As(err, &hostnameErr) {
					return fmt.Errorf("err = %v (%T), want a certificate verification error for the name", err, err)
				}
				return nil
			},
		},
		{
			description: "skip verify",
			serverName:  testTlsHelloServerName,
			configure: func(config *tls.Config) {
				config.RootCAs = otherRootCertPool
				config.InsecureSkipVerify = true
			},
			check: func(conn net.Conn, err error) error {
				return err
			},
		},
		{
			description: "peer verifier",
			serverName:  testTlsHelloServerName,
			configure: func(config *tls.Config) {
				config.VerifyPeerCertificate = func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
					if len(rawCerts) != 1 || len(verifiedChains) == 0 {
						return fmt.Errorf("verifier saw %d certificates and %d chains", len(rawCerts), len(verifiedChains))
					}
					return refused
				}
			},
			check: func(conn net.Conn, err error) error {
				if !errors.Is(err, refused) {
					return fmt.Errorf("err = %v, want the verifier's refusal", err)
				}
				return nil
			},
		},
		{
			description: "connection verifier",
			serverName:  testTlsHelloServerName,
			configure: func(config *tls.Config) {
				config.VerifyConnection = func(state tls.ConnectionState) error {
					if state.ServerName != testTlsHelloServerName || len(state.PeerCertificates) != 1 {
						return fmt.Errorf("verifier saw %q with %d certificates", state.ServerName, len(state.PeerCertificates))
					}
					return refused
				}
			},
			check: func(conn net.Conn, err error) error {
				if !errors.Is(err, refused) {
					return fmt.Errorf("err = %v, want the verifier's refusal", err)
				}
				return nil
			},
		},
	}
	for _, c := range cases {
		settings := server.clientStrategySettings(t)
		c.configure(settings.TlsConfig)
		// the websocket path presents the Chrome hello on any toolchain
		dialTlsContext := newNormalDialTlsContext(settings, clientWebSocketNextProtos)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		conn, err := dialTlsContext(ctx, "tcp", server.authorityFor(c.serverName))
		cancel()
		if conn != nil {
			if _, ok := conn.(*chromeTlsConn); !ok {
				t.Errorf("%s: dialed %T, want the Chrome hello's connection", c.description, conn)
			}
		}
		if checkErr := c.check(conn, err); checkErr != nil {
			t.Errorf("%s: %s", c.description, checkErr)
		}
		if conn != nil {
			conn.Close()
		}
	}
	for _, captured := range server.capturedHellos() {
		hello := parseTestClientHello(t, captured.message)
		if !isTestGreaseValue(hello.cipherSuites[0]) {
			t.Fatal("a verification case did not present the Chrome hello")
		}
	}
}

// A peer that answers the hello with something other than tls is reported in
// crypto/tls's error type, as Go's hello reports it.
func TestChromeClientHelloReportsNonTlsPeerAsRecordHeaderError(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.WriteString(conn, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
				_, _ = io.Copy(io.Discard, conn)
			}()
		}
	}()
	settings := DefaultClientStrategySettings()
	settings.ConnectSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp4", listener.Addr().String())
		},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := newNormalDialTlsContext(settings, clientWebSocketNextProtos)(ctx, "tcp", testTlsHelloServerName+":443")
	if err == nil {
		conn.Close()
		t.Fatal("a plain-text peer completed a tls handshake")
	}
	var recordHeaderErr tls.RecordHeaderError
	if !errors.As(err, &recordHeaderErr) {
		t.Fatalf("err = %v (%T), want crypto/tls's record header error", err, err)
	}
}

// The framed (H1+) upgrade dials through the strategy's websocket tls dial,
// so it presents the Chrome hello with http/1.1 alone and selects the framed
// carrier over it.
func TestFramedUpgradeOverChromeWebSocketDial(t *testing.T) {
	resetH1UpgradeTestState(t)
	observation := &h1UpgradeObservedRequests{}
	server := newTestTlsHelloServer(t, h1UpgradeTestEchoServer(H1FramerProtocol, observation, http.StatusSwitchingProtocols), nil)
	settings := server.clientStrategySettings(t)
	dialer := testTlsHelloDialers[0].clientDialer(settings)
	header := http.Header{"Authorization": []string{"Bearer synthetic-test"}}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := DialH1Messages(ctx, "wss://"+server.authority()+"/", header, dialer.WsDialer(settings), H1FramerProtocol, 65535, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	upgraded, ok := conn.UnderlyingConn().(*httpUpgradeConn)
	if !ok {
		conn.Close()
		t.Fatalf("selected carrier is %T, want the framed upgrade", conn.UnderlyingConn())
	}
	batchConnection, ok := upgraded.Conn.(*WebSocketWriteBatchConn)
	if !ok {
		conn.Close()
		t.Fatalf("framed upgrade transport is %T", upgraded.Conn)
	}
	if negotiated := testTlsConnectionState(t, batchConnection.conn).NegotiatedProtocol; negotiated != "http/1.1" {
		conn.Close()
		t.Fatalf("framed upgrade negotiated %q, want http/1.1", negotiated)
	}
	h1UpgradeTestEcho(t, conn)

	hellos := server.capturedHellos()
	if len(hellos) != 1 {
		t.Fatalf("captured %d hellos, want 1", len(hellos))
	}
	assertTestChromeClientHello(t, parseTestClientHello(t, hellos[0].message), clientWebSocketNextProtos)
}
