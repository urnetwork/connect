package extender

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/pion/transport/v4/vnet"
	"github.com/pion/webrtc/v4"

	"github.com/urnetwork/connect"
)

// Tests of the real extender server behind the peer-to-peer webrtc carrier
// (EXTENDER.md S, C2a): the server is the carrier's stream handler, so a
// stream is refused, limited, answered and forwarded by exactly the handler
// the tcp carrier runs. The extender sits behind a simulated NAT on pion's
// virtual network and the dialer on its public side, with the signaling
// bridged in memory.

const (
	testWebRtcWanCidr       = "203.0.113.0/24"
	testWebRtcDialerIp      = "203.0.113.10"
	testWebRtcNatIp         = "203.0.113.1"
	testWebRtcLanCidr       = "192.0.2.0/24"
	testWebRtcExtenderLanIp = "192.0.2.5"
)

// testWebRtcNat is the simulated NAT: the dialer on the WAN, the extender on
// a LAN behind an endpoint-independent NAT.
type testWebRtcNat struct {
	dialerNet   *vnet.Net
	extenderNet *vnet.Net
}

func newTestWebRtcNat(t *testing.T) *testWebRtcNat {
	t.Helper()
	loggerFactory := logging.NewDefaultLoggerFactory()
	loggerFactory.DefaultLogLevel = logging.LogLevelError
	wan, err := vnet.NewRouter(&vnet.RouterConfig{
		CIDR:          testWebRtcWanCidr,
		MinDelay:      time.Millisecond,
		LoggerFactory: loggerFactory,
	})
	if err != nil {
		t.Fatal(err)
	}
	lan, err := vnet.NewRouter(&vnet.RouterConfig{
		CIDR:      testWebRtcLanCidr,
		StaticIPs: []string{testWebRtcNatIp},
		NATType: &vnet.NATType{
			MappingBehavior:   vnet.EndpointIndependent,
			FilteringBehavior: vnet.EndpointIndependent,
		},
		MinDelay:      time.Millisecond,
		LoggerFactory: loggerFactory,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := wan.AddRouter(lan); err != nil {
		t.Fatal(err)
	}
	dialerNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{testWebRtcDialerIp}})
	if err != nil {
		t.Fatal(err)
	}
	if err := wan.AddNet(dialerNet); err != nil {
		t.Fatal(err)
	}
	extenderNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{testWebRtcExtenderLanIp}})
	if err != nil {
		t.Fatal(err)
	}
	if err := lan.AddNet(extenderNet); err != nil {
		t.Fatal(err)
	}
	if err := wan.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := wan.Stop(); err != nil {
			t.Errorf("stop the virtual network: %v", err)
		}
	})
	return &testWebRtcNat{dialerNet: dialerNet, extenderNet: extenderNet}
}

func testWebRtcCarrierSettings(network *vnet.Net) *connect.WebRtcSettings {
	settings := connect.DefaultWebRtcExtenderSettings()
	settings.Log = connect.NewNoopLogger()
	settings.Network = network
	settings.IceServerUrls = nil
	settings.IceServerPoolUrls = nil
	settings.ExtenderCarrierOpenTimeout = 10 * time.Second
	return settings
}

// testWebRtcBridge bridges a dial to the extender server's answerer.
type testWebRtcBridge struct {
	ctx      context.Context
	answerer connect.WebRtcExtenderOfferAnswerer
}

func (self *testWebRtcBridge) ExchangeOffer(
	ctx context.Context,
	offer webrtc.SessionDescription,
) (webrtc.SessionDescription, error) {
	return self.answerer.AnswerWebRtcExtenderOffer(self.ctx, offer)
}

// One open extender fixture serving the webrtc carrier, with the dial side's
// connect settings carrying a carrier that reaches it.
type testWebRtcFixture struct {
	fixture         *extenderFixture
	publicKey       []byte
	connectSettings *connect.ConnectSettings
	extenderCarrier *connect.WebRtcExtenderCarrier
}

func newTestWebRtcFixture(t *testing.T, ctx context.Context, configure func(settings *ExtenderSettings)) *testWebRtcFixture {
	t.Helper()
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := connect.ExtenderPublicKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newExtenderFixtureWithSecrets(t, "127.0.0.1", nil, func(settings *ExtenderSettings) {
		settings.IdentityKeySeed = seed
		settings.WebRtcCarrier = true
		if configure != nil {
			configure(settings)
		}
	})
	select {
	case <-fixture.server.Listening():
	case <-ctx.Done():
		t.Fatal("the extender did not bind")
	}

	nat := newTestWebRtcNat(t)
	extenderCarrier := connect.NewWebRtcExtenderCarrier(ctx, testWebRtcCarrierSettings(nat.extenderNet), nil)
	t.Cleanup(extenderCarrier.Close)
	bridge := &testWebRtcBridge{ctx: ctx, answerer: extenderCarrier.Answerer(fixture.server)}
	dialerCarrier := connect.NewWebRtcExtenderCarrier(
		ctx,
		testWebRtcCarrierSettings(nat.dialerNet),
		func(extenderPublicKey []byte) (connect.WebRtcExtenderOfferExchanger, bool) {
			return bridge, true
		},
	)
	t.Cleanup(dialerCarrier.Close)
	connectSettings := fixture.connectSettings()
	connectSettings.WebRtcExtenderCarrier = dialerCarrier
	return &testWebRtcFixture{
		fixture:         fixture,
		publicKey:       publicKey,
		connectSettings: connectSettings,
		extenderCarrier: extenderCarrier,
	}
}

func (self *testWebRtcFixture) extenderConfig() *connect.ExtenderConfig {
	return &connect.ExtenderConfig{
		Profile: connect.ExtenderProfile{
			ConnectMode: connect.ExtenderConnectModeWebRtc,
			Port:        self.fixture.tcpPort,
		},
		Ip:        self.fixture.ip,
		PublicKey: self.publicKey,
	}
}

// Root cause: a NATed extender cannot be reached on any socket carrier, so
// the real server must serve the carrier's stream exactly as a terminated
// tcp connection: the A3 request, the response naming the carrier, and the
// forward. Observable: a dial through the carrier is answered by the server
// with its identity and carrier list, and a verified https GET to the
// destination forwards through the data channel on the family of the ICE
// pair (A7).
func TestExtenderServesTheWebRtcCarrierStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	webRtc := newTestWebRtcFixture(t, ctx, nil)

	conn, response, err := connect.DialExtender(ctx, webRtc.connectSettings, webRtc.extenderConfig(), &connect.ExtenderDial{
		DestinationHost: "dest.example",
		DestinationPort: 443,
	})
	if err != nil {
		t.Fatalf("dial the server over the carrier: %v", err)
	}
	conn.Close()
	if string(response.PublicKey) != string(webRtc.publicKey) {
		t.Fatalf("the server published another key")
	}
	if !slices.Contains(response.Carriers, connect.ExtenderCarrierWebRtc) || !slices.Contains(response.Carriers, connect.ExtenderCarrierTcp) {
		t.Fatalf("response carriers = %v, want tcp and webrtc among them", response.Carriers)
	}

	client := connect.NewExtenderHttpClient(webRtc.connectSettings, webRtc.extenderConfig())
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://dest.example/hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	httpResponse, err := client.Do(request)
	if err != nil {
		t.Fatalf("https through the carrier: %v", err)
	}
	defer httpResponse.Body.Close()
	bodyBytes, err := io.ReadAll(io.LimitReader(httpResponse.Body, 64*1024))
	if err != nil {
		t.Fatal(err)
	}
	if httpResponse.StatusCode != http.StatusOK || !strings.Contains(string(bodyBytes), testHelloClientAddress) {
		t.Fatalf("hello = %d %q", httpResponse.StatusCode, bodyBytes)
	}
	// the forward follows the family of the stream's ICE pair (A7)
	forwardNetwork, err := webRtc.fixture.nextForwardNetwork()
	if err != nil {
		t.Fatal(err)
	}
	if forwardNetwork != "tcp4" {
		t.Fatalf("forward network = %q, want tcp4", forwardNetwork)
	}
}

// The operator's probes over the carrier (C2a): the challenge probe proves
// the identity, the forward probe proves a verified hello through the data
// channel and reports the address the api saw.
func TestExtenderAnswersTheWebRtcProbes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	webRtc := newTestWebRtcFixture(t, ctx, nil)

	response, err := connect.ProbeExtenderWebRtcCarrier(ctx, webRtc.connectSettings, webRtc.publicKey, "dest.example", 443)
	if err != nil {
		t.Fatalf("carrier probe: %v", err)
	}
	if !slices.Contains(response.Carriers, connect.ExtenderCarrierWebRtc) {
		t.Fatalf("probe carriers = %v", response.Carriers)
	}
	clientAddress, err := connect.ProbeExtenderWebRtcForward(
		ctx,
		webRtc.connectSettings,
		webRtc.publicKey,
		"https://dest.example",
		&tls.Config{RootCAs: webRtc.fixture.destination.rootCas},
	)
	if err != nil {
		t.Fatalf("forward probe: %v", err)
	}
	if clientAddress != testHelloClientAddress {
		t.Fatalf("client address = %q, want %q", clientAddress, testHelloClientAddress)
	}
	// both probes need the key: without it there is no identity to prove
	if _, err := connect.ProbeExtenderWebRtcCarrier(ctx, webRtc.connectSettings, nil, "dest.example", 443); err == nil {
		t.Fatalf("a carrier probe without a key was accepted")
	}
	if _, err := connect.ProbeExtenderWebRtcForward(ctx, webRtc.connectSettings, nil, "https://dest.example", nil); err == nil {
		t.Fatalf("a forward probe without a key was accepted")
	}
	// and a carrier: the plain settings refuse at once
	if _, err := connect.ProbeExtenderWebRtcCarrier(ctx, webRtc.fixture.connectSettings(), webRtc.publicKey, "dest.example", 443); !errors.Is(err, connect.ErrWebRtcExtenderCarrierUnavailable) {
		t.Fatalf("probe without a carrier err = %v", err)
	}
}

// The server's whitelist applies on the carrier as on every other: a forward
// to a host off it is refused with 403, which the dial reports as the typed
// refusal (A5).
func TestExtenderRefusesAWebRtcForwardOffTheWhitelist(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	webRtc := newTestWebRtcFixture(t, ctx, nil)
	conn, _, err := connect.DialExtender(ctx, webRtc.connectSettings, webRtc.extenderConfig(), &connect.ExtenderDial{
		DestinationHost: "elsewhere.example",
		DestinationPort: 443,
	})
	var refusedErr *connect.ExtenderRefusedError
	if !errors.As(err, &refusedErr) || refusedErr.StatusCode != http.StatusForbidden {
		t.Fatalf("err = %v, want a 403 refusal", err)
	}
	if conn != nil {
		t.Fatalf("a refused forward handed out a stream")
	}
}

// Root cause: without a source address the carrier's streams would share
// one admission key. Observable: with one connection per source allowed, a
// second concurrent stream from the same dialer is refused while the first
// is held, since both arrive from the dialer's public address.
func TestExtenderAdmitsWebRtcStreamsByTheirIceRemoteAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	webRtc := newTestWebRtcFixture(t, ctx, func(settings *ExtenderSettings) {
		settings.MaxConnectionCountPerSource = 1
	})
	first, _, err := connect.DialExtender(ctx, webRtc.connectSettings, webRtc.extenderConfig(), &connect.ExtenderDial{
		DestinationHost: "dest.example",
		DestinationPort: 443,
	})
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	defer first.Close()
	// the held stream is counted against its source before the second arrives
	if count := webRtc.fixture.server.ConnectionCount(); count != 1 {
		t.Fatalf("connections after the first dial = %d, want 1", count)
	}

	second, _, err := connect.DialExtender(ctx, webRtc.connectSettings, webRtc.extenderConfig(), &connect.ExtenderDial{
		DestinationHost: "dest.example",
		DestinationPort: 443,
	})
	if err == nil {
		second.Close()
		t.Fatalf("a second stream from the same source was admitted past the per-source limit")
	}
	if webRtc.fixture.server.ConnectionCount() != 1 {
		t.Fatalf("connections = %d, want the held one", webRtc.fixture.server.ConnectionCount())
	}
}

// The carrier is listed only when the server is configured to serve it, and
// a server configured for it stays up with no socket carrier at all (G2).
func TestExtenderListsTheWebRtcCarrierOnlyWhenEnabled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	plain := newExtenderFixtureWithSecrets(t, "127.0.0.1", nil, nil)
	select {
	case <-plain.server.Listening():
	case <-ctx.Done():
		t.Fatal("the extender did not bind")
	}
	if slices.Contains(plain.server.Carriers(), connect.ExtenderCarrierWebRtc) {
		t.Fatalf("carriers = %v list webrtc without the setting", plain.server.Carriers())
	}

	withCarrier := newExtenderFixtureWithSecrets(t, "127.0.0.1", nil, func(settings *ExtenderSettings) {
		settings.WebRtcCarrier = true
	})
	select {
	case <-withCarrier.server.Listening():
	case <-ctx.Done():
		t.Fatal("the extender did not bind")
	}
	carriers := withCarrier.server.Carriers()
	if carriers[len(carriers)-1] != connect.ExtenderCarrierWebRtc {
		t.Fatalf("carriers = %v, want webrtc last", carriers)
	}

	// no socket carrier at all: the server is up on the webrtc carrier alone
	for _, webRtcCarrier := range []bool{true, false} {
		settings := DefaultExtenderSettings()
		settings.WebRtcCarrier = webRtcCarrier
		server := NewExtenderServer(ctx, nil, []string{"dest.example"}, map[int][]connect.ExtenderConnectMode{}, &net.Dialer{}, settings)
		serveDone := make(chan error, 1)
		go func() { serveDone <- server.ListenAndServe() }()
		select {
		case <-server.Listening():
		case <-ctx.Done():
			t.Fatal("the server did not report its carriers")
		}
		if webRtcCarrier {
			if !slices.Equal(server.Carriers(), []string{connect.ExtenderCarrierWebRtc}) {
				t.Fatalf("carriers = %v, want webrtc alone", server.Carriers())
			}
			select {
			case err := <-serveDone:
				t.Fatalf("the server exited with %v instead of serving the carrier", err)
			default:
			}
			server.CloseAndWait()
			if err := <-serveDone; err != nil {
				t.Fatalf("serve returned %v", err)
			}
		} else {
			if err := <-serveDone; err == nil {
				t.Fatalf("a server with nothing to serve kept running")
			}
		}
	}
}
