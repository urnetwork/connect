package vlessinterop

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/app/proxyman"
	_ "github.com/xtls/xray-core/app/proxyman/inbound"
	_ "github.com/xtls/xray-core/app/proxyman/outbound"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/protocol/tls/cert"
	"github.com/xtls/xray-core/common/serial"
	core "github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/proxy/freedom"
	"github.com/xtls/xray-core/proxy/vless"
	"github.com/xtls/xray-core/proxy/vless/inbound"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/httpupgrade"
	"github.com/xtls/xray-core/transport/internet/reality"
	_ "github.com/xtls/xray-core/transport/internet/tcp"
	xtls "github.com/xtls/xray-core/transport/internet/tls"
	"github.com/xtls/xray-core/transport/internet/websocket"
)

// A synthetic user id.
const testUserId = "5783a3e7-e373-51cd-8642-c83782b807c5"

func testPickPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// Starts an Xray VLESS inbound on loopback with the stream settings and user
// flow given, relaying to the destinations requested, and returns its port.
func testStartXray(t *testing.T, streamConfig *internet.StreamConfig, flow string) int {
	t.Helper()
	port := testPickPort(t)
	config := &core.Config{
		App: []*serial.TypedMessage{
			serial.ToTypedMessage(&dispatcher.Config{}),
			serial.ToTypedMessage(&proxyman.InboundConfig{}),
			serial.ToTypedMessage(&proxyman.OutboundConfig{}),
		},
		Inbound: []*core.InboundHandlerConfig{{
			ReceiverSettings: serial.ToTypedMessage(&proxyman.ReceiverConfig{
				PortList:       &xnet.PortList{Range: []*xnet.PortRange{xnet.SinglePortRange(xnet.Port(port))}},
				Listen:         xnet.NewIPOrDomain(xnet.LocalHostIP),
				StreamSettings: streamConfig,
			}),
			ProxySettings: serial.ToTypedMessage(&inbound.Config{
				Clients: []*protocol.User{{
					Account: serial.ToTypedMessage(&vless.Account{Id: testUserId, Flow: flow}),
				}},
				Decryption: "none",
			}),
		}},
		Outbound: []*core.OutboundHandlerConfig{{
			ProxySettings: serial.ToTypedMessage(&freedom.Config{}),
		}},
	}
	instance, err := core.New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { instance.Close() })
	return port
}

// The https destination behind the VLESS server, with a small and a large
// response. Its tls runs end to end through the stream.
type testDestination struct {
	server  *httptest.Server
	bigBody []byte
	roots   *x509.CertPool
}

func newTestDestination(t *testing.T) *testDestination {
	t.Helper()
	bigBody := make([]byte, 512*1024)
	if _, err := rand.Read(bigBody); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello through xray"))
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		w.Write(bigBody)
	})
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	return &testDestination{server: server, bigBody: bigBody, roots: roots}
}

// Fetches through a client strategy whose only dialer is the VLESS server --
// a small body once and the large body three times -- then opens a stream
// with DialVless directly and runs the destination's tls on it.
func testFetchThrough(t *testing.T, config *connect.VlessConfig, destination *testDestination) {
	t.Helper()
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	settings := connect.DefaultClientStrategySettings()
	settings.EnableNormal = false
	settings.EnableResilient = false
	settings.ExposeServerIps = false
	settings.ExposeServerHostNames = false
	settings.ExtenderDirectory = nil
	settings.ConnectSettings.TlsConfig = &tls.Config{RootCAs: destination.roots}
	settings.VlessConfigs = []*connect.VlessConfig{config}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	strategy := connect.NewClientStrategy(ctx, settings)
	defer strategy.Close()

	body, err := connect.HttpGetWithStrategyRaw(ctx, strategy, destination.server.URL+"/hello", "")
	if err != nil {
		t.Fatalf("hello: %s", err)
	}
	if string(body) != "hello through xray" {
		t.Fatalf("hello body = %q", body)
	}
	for i := 0; i < 3; i += 1 {
		body, err = connect.HttpGetWithStrategyRaw(ctx, strategy, destination.server.URL+"/big", "")
		if err != nil {
			t.Fatalf("big: %s", err)
		}
		if sha256.Sum256(body) != sha256.Sum256(destination.bigBody) {
			t.Fatalf("big body differs: %d bytes", len(body))
		}
	}

	conn, err := connect.DialVless(ctx, &settings.ConnectSettings, config, "tcp", strings.TrimPrefix(destination.server.URL, "https://"))
	if err != nil {
		t.Fatalf("dial: %s", err)
	}
	tlsConn := tls.Client(conn, &tls.Config{RootCAs: destination.roots, ServerName: "127.0.0.1"})
	defer tlsConn.Close()
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		t.Fatalf("inner handshake: %s", err)
	}
}

func testTlsStream(network string, transportSettings []*internet.TransportConfig) *internet.StreamConfig {
	certificate, _ := cert.MustGenerate(nil, cert.DNSNames("vless.example"))
	return &internet.StreamConfig{
		ProtocolName:      network,
		TransportSettings: transportSettings,
		SecurityType:      serial.GetMessageType(&xtls.Config{}),
		SecuritySettings: []*serial.TypedMessage{
			serial.ToTypedMessage(&xtls.Config{Certificate: []*xtls.Certificate{xtls.ParseCertificate(certificate)}}),
		},
	}
}

func testConfig(port int, network string, security string, flow string) *connect.VlessConfig {
	return &connect.VlessConfig{
		Address:  "127.0.0.1",
		Port:     port,
		Id:       testUserId,
		Flow:     flow,
		Network:  network,
		Security: security,
	}
}

func TestXrayRawTcp(t *testing.T) {
	destination := newTestDestination(t)
	port := testStartXray(t, &internet.StreamConfig{ProtocolName: "tcp"}, "")
	testFetchThrough(t, testConfig(port, connect.VlessNetworkTcp, connect.VlessSecurityNone, ""), destination)
}

func TestXrayTls(t *testing.T) {
	destination := newTestDestination(t)
	port := testStartXray(t, testTlsStream("tcp", nil), "")
	config := testConfig(port, connect.VlessNetworkTcp, connect.VlessSecurityTls, "")
	config.ServerName = "vless.example"
	config.AllowInsecure = true
	testFetchThrough(t, config, destination)
}

// Vision over tls with the Go hello and with an imitated hello. The server
// switches the downlink to direct after the inner tls 1.3 handshake.
func TestXrayTlsVision(t *testing.T) {
	destination := newTestDestination(t)
	port := testStartXray(t, testTlsStream("tcp", nil), vless.XRV)
	for _, fingerprint := range []string{"", "chrome"} {
		config := testConfig(port, connect.VlessNetworkTcp, connect.VlessSecurityTls, connect.VlessFlowVision)
		config.ServerName = "vless.example"
		config.AllowInsecure = true
		config.Fingerprint = fingerprint
		testFetchThrough(t, config, destination)
	}
}

func TestXrayWebSocketTls(t *testing.T) {
	destination := newTestDestination(t)
	port := testStartXray(t, testTlsStream("websocket", []*internet.TransportConfig{{
		ProtocolName: "websocket",
		Settings:     serial.ToTypedMessage(&websocket.Config{Path: "/ws", Host: "cdn.example"}),
	}}), "")
	config := testConfig(port, connect.VlessNetworkWs, connect.VlessSecurityTls, "")
	config.ServerName = "vless.example"
	config.AllowInsecure = true
	config.Path = "/ws?ed=2048"
	config.Host = "cdn.example"
	config.Fingerprint = "chrome"
	testFetchThrough(t, config, destination)
}

func TestXrayHttpUpgrade(t *testing.T) {
	destination := newTestDestination(t)
	port := testStartXray(t, &internet.StreamConfig{
		ProtocolName: "httpupgrade",
		TransportSettings: []*internet.TransportConfig{{
			ProtocolName: "httpupgrade",
			Settings:     serial.ToTypedMessage(&httpupgrade.Config{Path: "/up", Host: "up.example"}),
		}},
	}, "")
	config := testConfig(port, connect.VlessNetworkHttpUpgrade, connect.VlessSecurityNone, "")
	config.Path = "/up"
	config.Host = "up.example"
	testFetchThrough(t, config, destination)
}

// A reality server with a fresh key, borrowing a local tls 1.3 server as its
// cover site, and the client configuration that matches it.
func testStartReality(t *testing.T, flow string, shortId []byte) (int, *ecdh.PrivateKey) {
	t.Helper()
	cover := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("cover"))
	}))
	t.Cleanup(cover.Close)
	privateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// the server keeps each short id zero-padded to 8 bytes
	paddedShortId := append(append([]byte{}, shortId...), make([]byte, 8-len(shortId))...)
	port := testStartXray(t, &internet.StreamConfig{
		ProtocolName: "tcp",
		SecurityType: serial.GetMessageType(&reality.Config{}),
		SecuritySettings: []*serial.TypedMessage{serial.ToTypedMessage(&reality.Config{
			Dest:        strings.TrimPrefix(cover.URL, "https://"),
			Type:        "tcp",
			ServerNames: []string{"www.cover.example"},
			PrivateKey:  privateKey.Bytes(),
			ShortIds:    [][]byte{paddedShortId},
		})},
	}, flow)
	return port, privateKey
}

func TestXrayReality(t *testing.T) {
	destination := newTestDestination(t)
	shortId := []byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}
	port, privateKey := testStartReality(t, "", shortId)
	config := testConfig(port, connect.VlessNetworkTcp, connect.VlessSecurityReality, "")
	config.ServerName = "www.cover.example"
	config.PublicKey = privateKey.PublicKey().Bytes()
	config.ShortId = shortId
	config.Fingerprint = "chrome"
	testFetchThrough(t, config, destination)
}

// Reality with vision under every browser hello a user can pick, against one
// server, read from a share link as a user would paste it.
func TestXrayRealityVisionFingerprints(t *testing.T) {
	destination := newTestDestination(t)
	shortId := []byte{0x6b, 0xa8}
	port, privateKey := testStartReality(t, vless.XRV, shortId)
	for _, fingerprint := range []string{"chrome", "firefox", "safari", "ios", "edge", "random"} {
		link := "vless://" + testUserId + "@127.0.0.1:" + strconv.Itoa(port) +
			"?encryption=none&flow=xtls-rprx-vision&security=reality&sni=www.cover.example&fp=" + fingerprint +
			"&pbk=" + connect.EncodeVlessPublicKey(privateKey.PublicKey().Bytes()) + "&sid=6ba8&type=tcp#interop"
		config, err := connect.ParseVlessLink(link)
		if err != nil {
			t.Fatal(err)
		}
		testFetchThrough(t, config, destination)
	}
}

// A client with the wrong key never completes a reality dial: the server
// treats it as an ordinary visitor of the cover site, whose certificate does
// not verify for the borrowed name, and no VLESS request is ever sent.
func TestXrayRealityWrongKeyFails(t *testing.T) {
	destination := newTestDestination(t)
	shortId := []byte{0x01, 0x02}
	port, _ := testStartReality(t, "", shortId)
	otherKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	config := testConfig(port, connect.VlessNetworkTcp, connect.VlessSecurityReality, "")
	config.ServerName = "www.cover.example"
	config.PublicKey = otherKey.PublicKey().Bytes()
	config.ShortId = shortId
	config.Fingerprint = "chrome"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := connect.DialVless(ctx, connect.DefaultConnectSettings(), config, "tcp", strings.TrimPrefix(destination.server.URL, "https://"))
	if err == nil {
		conn.Close()
		t.Fatal("a reality dial with the wrong key must fail")
	}
}
