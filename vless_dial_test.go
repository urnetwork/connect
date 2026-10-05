package connect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// The destination behind the VLESS server: an https server with a small and a
// large response, reached with its own tls end to end through the stream.
type testVlessDestination struct {
	server    *httptest.Server
	bigBody   []byte
	tlsConfig *tls.Config
}

func newTestVlessDestination(t *testing.T) *testVlessDestination {
	t.Helper()
	bigBody := make([]byte, 384*1024)
	random := rand.New(rand.NewPCG(1, 2))
	for i := range bigBody {
		bigBody[i] = byte(random.UintN(256))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello through vless"))
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		w.Write(bigBody)
	})
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	rootCas := x509.NewCertPool()
	rootCas.AddCert(server.Certificate())
	return &testVlessDestination{
		server:    server,
		bigBody:   bigBody,
		tlsConfig: &tls.Config{RootCAs: rootCas},
	}
}

func testVlessConnectSettings(destination *testVlessDestination) *ConnectSettings {
	connectSettings := DefaultConnectSettings()
	connectSettings.TlsConfig = destination.tlsConfig
	return connectSettings
}

// GETs path from the destination through config's server with the strategy
// dialer's tls dial, and returns the body.
func testVlessGet(t *testing.T, config *VlessConfig, destination *testVlessDestination, path string) []byte {
	t.Helper()
	connectSettings := testVlessConnectSettings(destination)
	client := &http.Client{
		Transport: &http.Transport{
			DialTLSContext: newVlessDialTlsContext(connectSettings, config, []string{"http/1.1"}),
			DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
				t.Fatalf("a plain dial is not expected for an https destination")
				return nil, nil
			},
			DisableKeepAlives: true,
		},
		Timeout: 30 * time.Second,
	}
	response, err := client.Get(destination.server.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d", response.StatusCode)
	}
	return body
}

func testVlessAssertBodies(t *testing.T, config *VlessConfig, destination *testVlessDestination) {
	t.Helper()
	if body := testVlessGet(t, config, destination, "/hello"); string(body) != "hello through vless" {
		t.Fatalf("body = %q", body)
	}
	// large enough for many records after the padding ends
	body := testVlessGet(t, config, destination, "/big")
	if sha256.Sum256(body) != sha256.Sum256(destination.bigBody) {
		t.Fatalf("large body differs (%d bytes, expected %d)", len(body), len(destination.bigBody))
	}
}

func testVlessAssertDestination(t *testing.T, server *testVlessServer, destination *testVlessDestination, flow string) {
	t.Helper()
	destinations, flows, _ := server.seen()
	expected := strings.TrimPrefix(destination.server.URL, "https://")
	if len(destinations) == 0 {
		t.Fatalf("the server saw no stream")
	}
	for i, seenDestination := range destinations {
		if seenDestination != expected {
			t.Errorf("destination = %s, expected %s", seenDestination, expected)
		}
		if flows[i] != flow {
			t.Errorf("flow = %q, expected %q", flows[i], flow)
		}
	}
}

func TestVlessDialRawTcp(t *testing.T) {
	destination := newTestVlessDestination(t)
	server := newTestVlessServer(t, VlessSecurityNone, VlessNetworkTcp)
	config := server.config(VlessFlowNone)
	testVlessAssertBodies(t, config, destination)
	testVlessAssertDestination(t, server, destination, VlessFlowNone)
}

func TestVlessDialTls(t *testing.T) {
	destination := newTestVlessDestination(t)
	server := newTestVlessServer(t, VlessSecurityTls, VlessNetworkTcp)
	config := server.config(VlessFlowNone)
	testVlessAssertBodies(t, config, destination)
	testVlessAssertDestination(t, server, destination, VlessFlowNone)
}

// The outer certificate is verified unless the configuration allows an
// insecure server: the test server's self-signed certificate fails the dial.
func TestVlessDialTlsVerifiesTheServer(t *testing.T) {
	destination := newTestVlessDestination(t)
	server := newTestVlessServer(t, VlessSecurityTls, VlessNetworkTcp)
	config := server.config(VlessFlowNone)
	config.AllowInsecure = false
	_, err := DialVless(context.Background(), testVlessConnectSettings(destination), config, "tcp", "api.example:443")
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("dial err = %v, expected a certificate error", err)
	}
	if destinations, _, _ := server.seen(); len(destinations) != 0 {
		t.Fatalf("no request may reach a server that failed verification")
	}
}

// The imitated browser hello works over the tls security too.
func TestVlessDialTlsFingerprint(t *testing.T) {
	destination := newTestVlessDestination(t)
	server := newTestVlessServer(t, VlessSecurityTls, VlessNetworkTcp)
	config := server.config(VlessFlowNone)
	config.Fingerprint = "chrome"
	testVlessAssertBodies(t, config, destination)
}

func TestVlessDialWebSocket(t *testing.T) {
	destination := newTestVlessDestination(t)
	server := newTestVlessServer(t, VlessSecurityTls, VlessNetworkWs)
	config := server.config(VlessFlowNone)
	config.Path = server.path + "?ed=2048"
	testVlessAssertBodies(t, config, destination)
	testVlessAssertDestination(t, server, destination, VlessFlowNone)
	func() {
		server.stateLock.Lock()
		defer server.stateLock.Unlock()
		for i, path := range server.requestPaths {
			if path != server.path {
				t.Errorf("request path = %s, expected %s without the early data parameter", path, server.path)
			}
			if server.requestHosts[i] != "cdn.example" {
				t.Errorf("request host = %s", server.requestHosts[i])
			}
		}
	}()
}

func TestVlessDialWebSocketFingerprint(t *testing.T) {
	destination := newTestVlessDestination(t)
	server := newTestVlessServer(t, VlessSecurityTls, VlessNetworkWs)
	config := server.config(VlessFlowNone)
	config.Fingerprint = "firefox"
	testVlessAssertBodies(t, config, destination)
}

func TestVlessDialHttpUpgrade(t *testing.T) {
	destination := newTestVlessDestination(t)
	server := newTestVlessServer(t, VlessSecurityNone, VlessNetworkHttpUpgrade)
	config := server.config(VlessFlowNone)
	testVlessAssertBodies(t, config, destination)
	testVlessAssertDestination(t, server, destination, VlessFlowNone)
}

// Vision over tls where the server ends its padding with the end command: the
// downlink stays inside the outer tls.
func TestVlessDialVisionEnd(t *testing.T) {
	destination := newTestVlessDestination(t)
	server := newTestVlessServer(t, VlessSecurityTls, VlessNetworkTcp)
	config := server.config(VlessFlowVision)
	testVlessAssertBodies(t, config, destination)
	testVlessAssertDestination(t, server, destination, VlessFlowVision)
	testVlessAssertUplinkCommands(t, server)
}

// Vision over tls where the server switches the downlink to direct at the
// inner tls's first application data record: every byte after the direct
// command arrives outside the outer tls, often in the same tcp segment as
// the record that carried the command.
func TestVlessDialVisionDirect(t *testing.T) {
	destination := newTestVlessDestination(t)
	server := newTestVlessServer(t, VlessSecurityTls, VlessNetworkTcp)
	server.visionDirect = true
	config := server.config(VlessFlowVision)
	testVlessAssertBodies(t, config, destination)
	testVlessAssertDestination(t, server, destination, VlessFlowVision)
	testVlessAssertUplinkCommands(t, server)
}

// A vision server that does not pad its downlink is read as plain.
func TestVlessDialVisionUnpaddedDownlink(t *testing.T) {
	destination := newTestVlessDestination(t)
	server := newTestVlessServer(t, VlessSecurityTls, VlessNetworkTcp)
	server.visionNoPadding = true
	config := server.config(VlessFlowVision)
	testVlessAssertBodies(t, config, destination)
}

// The client pads only until its first application data record and ends with
// the end command, never direct.
func testVlessAssertUplinkCommands(t *testing.T, server *testVlessServer) {
	t.Helper()
	_, _, uplinkCommands := server.seen()
	if len(uplinkCommands) == 0 {
		t.Fatalf("no uplink block seen")
	}
	if bytes.Contains(uplinkCommands, []byte{vlessVisionCommandDirect}) {
		t.Fatalf("uplink commands %v include direct", uplinkCommands)
	}
	if !bytes.Contains(uplinkCommands, []byte{vlessVisionCommandEnd}) {
		t.Fatalf("uplink commands %v never end the padding", uplinkCommands)
	}
}

// Only tcp can be carried; an invalid configuration is refused before any
// connection is made.
func TestVlessDialRefusesWhatItCannotCarry(t *testing.T) {
	destination := newTestVlessDestination(t)
	server := newTestVlessServer(t, VlessSecurityNone, VlessNetworkTcp)
	config := server.config(VlessFlowNone)
	if _, err := DialVless(context.Background(), testVlessConnectSettings(destination), config, "udp", "api.example:443"); err == nil {
		t.Fatalf("udp must be refused")
	}
	invalid := config.Copy()
	invalid.Port = 0
	if _, err := DialVless(context.Background(), testVlessConnectSettings(destination), invalid, "tcp", "api.example:443"); VlessConfigErrorCode(err) != VlessErrorPortInvalid {
		t.Fatalf("err = %v, expected the port error", err)
	}
	if destinations, _, _ := server.seen(); len(destinations) != 0 {
		t.Fatalf("nothing may reach the server")
	}
}

// A client strategy with only a VLESS dialer -- no direct, resilient or
// extender dialers, nothing exposed -- reaches the destination through the
// VLESS server, and replacing the configurations removes the dialer.
func TestClientStrategyVlessDialer(t *testing.T) {
	destination := newTestVlessDestination(t)
	server := newTestVlessServer(t, VlessSecurityTls, VlessNetworkTcp)
	config := server.config(VlessFlowVision)
	server.visionDirect = true

	settings := DefaultClientStrategySettings()
	settings.EnableNormal = false
	settings.EnableResilient = false
	settings.ExposeServerIps = false
	settings.ExposeServerHostNames = false
	settings.ExtenderDirectory = nil
	settings.ConnectSettings.TlsConfig = destination.tlsConfig
	settings.VlessConfigs = []*VlessConfig{config}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := NewClientStrategy(ctx, settings)
	defer strategy.Close()

	// the strategy keeps its own copy of the configuration
	config.Port = 1
	vlessConfigs := strategy.VlessConfigs()
	if len(vlessConfigs) != 1 || vlessConfigs[0].Port == 1 {
		t.Fatalf("strategy configurations = %+v, expected its own copy", vlessConfigs)
	}

	requestCtx, requestCancel := context.WithTimeout(ctx, 30*time.Second)
	defer requestCancel()
	body, err := HttpGetWithStrategyRaw(requestCtx, strategy, destination.server.URL+"/hello", "")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello through vless" {
		t.Fatalf("body = %q", body)
	}
	testVlessAssertDestination(t, server, destination, VlessFlowVision)

	strategy.SetVlessConfigs(nil)
	if vlessConfigs := strategy.VlessConfigs(); len(vlessConfigs) != 0 {
		t.Fatalf("configurations after removal = %d", len(vlessConfigs))
	}
	failCtx, failCancel := context.WithTimeout(ctx, 3*time.Second)
	defer failCancel()
	if _, err := HttpGetWithStrategyRaw(failCtx, strategy, destination.server.URL+"/hello", ""); err == nil {
		t.Fatalf("a strategy with no dialers left cannot reach the destination")
	}

	strategy.SetVlessConfigs([]*VlessConfig{server.config(VlessFlowNone)})
	againCtx, againCancel := context.WithTimeout(ctx, 30*time.Second)
	defer againCancel()
	if body, err := HttpGetWithStrategyRaw(againCtx, strategy, destination.server.URL+"/hello", ""); err != nil || string(body) != "hello through vless" {
		t.Fatalf("after setting a configuration again: body = %q err = %v", body, err)
	}
}

// A strategy carries the websocket through its VLESS dialer too.
func TestClientStrategyVlessWebSocket(t *testing.T) {
	destination := newTestVlessDestination(t)
	server := newTestVlessServer(t, VlessSecurityNone, VlessNetworkTcp)

	settings := DefaultClientStrategySettings()
	settings.EnableNormal = false
	settings.EnableResilient = false
	settings.ExposeServerIps = false
	settings.ExposeServerHostNames = false
	settings.ExtenderDirectory = nil
	settings.ConnectSettings.TlsConfig = destination.tlsConfig
	settings.VlessConfigs = []*VlessConfig{server.config(VlessFlowNone)}

	upgradeDestination := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		wsConn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer wsConn.Close()
		messageType, message, err := wsConn.ReadMessage()
		if err != nil {
			return
		}
		wsConn.WriteMessage(messageType, append([]byte("echo "), message...))
	}))
	upgradeDestination.StartTLS()
	defer upgradeDestination.Close()
	settings.ConnectSettings.TlsConfig.RootCAs.AddCert(upgradeDestination.Certificate())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := NewClientStrategy(ctx, settings)
	defer strategy.Close()

	dialCtx, dialCancel := context.WithTimeout(ctx, 30*time.Second)
	defer dialCancel()
	wsUrl := "wss://" + strings.TrimPrefix(upgradeDestination.URL, "https://") + "/ws"
	wsConn, _, err := strategy.WsDialContext(dialCtx, wsUrl, http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	defer wsConn.Close()
	if err := wsConn.WriteMessage(websocket.BinaryMessage, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	_, message, err := wsConn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if string(message) != "echo ping" {
		t.Fatalf("message = %q", message)
	}
	destinations, _, _ := server.seen()
	if len(destinations) == 0 || destinations[0] != strings.TrimPrefix(upgradeDestination.URL, "https://") {
		t.Fatalf("destinations = %v", destinations)
	}
}

// A family-pinned direct strategy never takes the VLESS servers.
func TestDirectClientStrategyDropsVless(t *testing.T) {
	settings := DefaultClientStrategySettings()
	settings.VlessConfigs = []*VlessConfig{{
		Address:  "192.0.2.1",
		Port:     443,
		Id:       testVlessUserId,
		Network:  VlessNetworkTcp,
		Security: VlessSecurityNone,
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := NewDirectClientStrategy(ctx, settings, 4)
	defer strategy.Close()
	if vlessConfigs := strategy.VlessConfigs(); len(vlessConfigs) != 0 {
		t.Fatalf("direct strategy has %d VLESS dialers", len(vlessConfigs))
	}
	if len(settings.VlessConfigs) != 1 {
		t.Fatalf("the caller's settings must not change")
	}
}
