package connect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"
)

// The udp of a space whose egress is a proxy (net_proxy_packet_path.go,
// connect#227). A socks proxy carries no udp, so every host socket such a
// space opened sent its traffic from this host, outside the proxy. Each test
// here puts a canary where that traffic would land, or counts the dials a walk
// tries, and keeps the same dial without a proxy as the control.
//
// Addresses are loopback and RFC 5737 documentation addresses.

// A proxy no test dials: these tests decide before any stream dial.
func testProxySettings() *ProxySettings {
	return &ProxySettings{
		Network: "tcp",
		Address: "192.0.2.1:1080",
	}
}

// A udp socket on loopback that counts nothing and answers nothing; whatever
// reached it is read back with a non-blocking read.
func newTestUdpCanary(t *testing.T) net.PacketConn {
	t.Helper()
	canary, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		canary.Close()
	})
	return canary
}

// Whether a datagram is waiting at the canary. A datagram sent on loopback is
// in the receiving socket's queue by the time its send returns, so a dial that
// has returned has delivered whatever it sent.
func testUdpCanaryReceived(t *testing.T, canary net.PacketConn) bool {
	t.Helper()
	if err := canary.SetReadDeadline(time.Now()); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 2048)
	_, _, err := canary.ReadFrom(buffer)
	return err == nil
}

// A quic or dns carrier of a space whose egress is a proxy opens no socket of
// this host's: the dial ends at once with the refusal, and nothing reaches the
// extender's udp port. With a packet endpoint the owner supplied, that
// endpoint is opened instead; without a proxy, the host socket is opened as
// before.
func TestProxiedSpaceOpensNoHostUdpSocket(t *testing.T) {
	for _, connectMode := range []ExtenderConnectMode{ExtenderConnectModeQuic, ExtenderConnectModeDns} {
		canary := newTestUdpCanary(t)
		canaryPort := canary.LocalAddr().(*net.UDPAddr).Port
		extenderConfig := &ExtenderConfig{
			Profile: ExtenderProfile{
				ConnectMode: connectMode,
				ServerName:  "extender.space.example",
				Port:        canaryPort,
			},
			Ip:       netip.MustParseAddr("127.0.0.1"),
			DnsPorts: []int{canaryPort},
		}
		connectSettings := DefaultConnectSettings()
		connectSettings.ProxySettings = testProxySettings()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		conn, _, err := DialExtender(ctx, connectSettings, extenderConfig, &ExtenderDial{})
		cancel()
		if err == nil {
			conn.Close()
			t.Fatalf("%s: a proxied space dialed a udp carrier", connectMode)
		}
		if !errors.Is(err, ErrExtenderPacketCarrierUnavailable) {
			t.Fatalf("%s: the dial failed with %v, expected the proxy's refusal", connectMode, err)
		}
		if testUdpCanaryReceived(t, canary) {
			t.Fatalf("%s: a datagram left this host outside the proxy", connectMode)
		}
	}

	udpAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}
	// an endpoint the owner supplied is the space's own udp path
	factoryCount := 0
	suppliedSettings := DefaultConnectSettings()
	suppliedSettings.ProxySettings = testProxySettings()
	suppliedSettings.DialContextSettings = &DialContextSettings{
		PacketConnFactory: func(ctx context.Context) (net.PacketConn, error) {
			factoryCount += 1
			return net.ListenPacket("udp4", "127.0.0.1:0")
		},
	}
	suppliedConn, err := openExtenderPacketConn(context.Background(), suppliedSettings, udpAddr)
	if err != nil {
		t.Fatalf("the supplied endpoint was refused: %v", err)
	}
	suppliedConn.Close()
	if factoryCount != 1 {
		t.Fatalf("the supplied endpoint was opened %d times, expected once", factoryCount)
	}
	// with no proxy, the host socket, as before
	hostConn, err := openExtenderPacketConn(context.Background(), DefaultConnectSettings(), udpAddr)
	if err != nil {
		t.Fatalf("a space with no proxy was refused a udp socket: %v", err)
	}
	hostConn.Close()

	// the webrtc carrier gathers its own host candidates, so a proxied space
	// never dials it, endpoint or not
	webRtcConfig := &ExtenderConfig{
		Profile: ExtenderProfile{
			ConnectMode: ExtenderConnectModeWebRtc,
			Port:        ExtenderTcpPort,
		},
		Ip: netip.MustParseAddr("192.0.2.2"),
	}
	if _, _, err := DialExtender(context.Background(), suppliedSettings, webRtcConfig, &ExtenderDial{}); !errors.Is(err, ErrExtenderPacketCarrierUnavailable) {
		t.Fatalf("a proxied space's webrtc dial failed with %v, expected the proxy's refusal", err)
	}
}

// A proxied strategy builds dialers for the tcp carrier of an extender alone,
// so a udp carrier is never tried and never counts against the extender; a
// strategy with no proxy keeps every carrier.
func TestProxiedStrategyDialsOnlyTcpExtenderCarriers(t *testing.T) {
	for _, proxied := range []bool{true, false} {
		clock := newTestClock()
		clientStrategy, directory, rootPrivateKey := newTestExtenderStrategy(t, clock, func(settings *ClientStrategySettings) {
			if proxied {
				settings.ProxySettings = testProxySettings()
			}
		})
		ip := netip.MustParseAddr("192.0.2.100")
		applyTestReadOnlyRecord(t, clock, directory, rootPrivateKey, ip)

		connectModes := []ExtenderConnectMode{}
		for _, dialer := range clientStrategy.expandExtenderDialers() {
			connectModes = append(connectModes, dialer.extenderConfig.Profile.ConnectMode)
		}
		slices.Sort(connectModes)
		expectModes := []ExtenderConnectMode{ExtenderConnectModeDns, ExtenderConnectModeQuic, ExtenderConnectModeTcpTls}
		if proxied {
			expectModes = []ExtenderConnectMode{ExtenderConnectModeTcpTls}
		}
		slices.Sort(expectModes)
		if !slices.Equal(connectModes, expectModes) {
			t.Fatalf("proxied %t: the strategy dials carriers %v, expected %v", proxied, connectModes, expectModes)
		}
	}
}

// A proxied strategy builds no alt dialers: they are udp, and resolve the alt
// host on this host before they dial, so not even a packet endpoint of the
// owner's keeps them. A strategy with no proxy keeps both.
func TestProxiedStrategyBuildsNoAltDialers(t *testing.T) {
	altDescriptions := func(configure func(settings *ClientStrategySettings)) []string {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := DefaultClientStrategySettings()
		settings.AltUrl = "https://alt.space.example"
		configure(settings)
		clientStrategy := NewClientStrategy(ctx, settings)
		defer clientStrategy.Close()
		descriptions := []string{}
		clientStrategy.mutex.Lock()
		defer clientStrategy.mutex.Unlock()
		for dialer := range clientStrategy.dialers {
			if dialer.description == "alt h3" || dialer.description == "alt whodis" {
				descriptions = append(descriptions, dialer.description)
			}
		}
		slices.Sort(descriptions)
		return descriptions
	}
	if descriptions := altDescriptions(func(settings *ClientStrategySettings) {
		settings.ProxySettings = testProxySettings()
	}); len(descriptions) != 0 {
		t.Fatalf("a proxied strategy built the alt dialers %v", descriptions)
	}
	if descriptions := altDescriptions(func(settings *ClientStrategySettings) {
		settings.ProxySettings = testProxySettings()
		settings.DialContextSettings = &DialContextSettings{
			PacketConnFactory: func(ctx context.Context) (net.PacketConn, error) {
				return net.ListenPacket("udp4", "127.0.0.1:0")
			},
		}
	}); len(descriptions) != 0 {
		t.Fatalf("a proxied strategy with a packet endpoint built the alt dialers %v", descriptions)
	}
	expectDescriptions := []string{"alt h3", "alt whodis"}
	if descriptions := altDescriptions(func(settings *ClientStrategySettings) {}); !slices.Equal(descriptions, expectDescriptions) {
		t.Fatalf("a strategy with no proxy built %v, expected %v", descriptions, expectDescriptions)
	}
}

// The probe walk the network client and the peer pinger share tries the tcp
// carrier alone for a proxied space, and every carrier otherwise.
func TestProxiedSpaceProbeWalkTriesOnlyTcp(t *testing.T) {
	clock := newTestClock()
	_, directory, rootPrivateKey := newTestExtenderStrategy(t, clock, nil)
	ip := netip.MustParseAddr("192.0.2.101")
	applyTestReadOnlyRecord(t, clock, directory, rootPrivateKey, ip)
	candidates := directory.Candidates(4, 1)
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, expected the one record", len(candidates))
	}

	proxiedSettings := DefaultConnectSettings()
	proxiedSettings.ProxySettings = testProxySettings()
	for _, c := range []struct {
		connectSettings *ConnectSettings
		expectModes     []ExtenderConnectMode
	}{
		{
			connectSettings: proxiedSettings,
			expectModes:     []ExtenderConnectMode{ExtenderConnectModeTcpTls},
		},
		{
			connectSettings: DefaultConnectSettings(),
			expectModes:     []ExtenderConnectMode{ExtenderConnectModeTcpTls, ExtenderConnectModeQuic, ExtenderConnectModeDns},
		},
	} {
		probedModes := []ExtenderConnectMode{}
		_, err := probeExtenderCandidate(
			context.Background(),
			c.connectSettings,
			directory,
			candidates[0],
			1,
			time.Second,
			func(ctx context.Context, extenderConfig *ExtenderConfig) (*ExtenderLatencyProbe, error) {
				probedModes = append(probedModes, extenderConfig.Profile.ConnectMode)
				return nil, errors.New("synthetic probe failure")
			},
		)
		if err == nil {
			t.Fatal("a walk of failing probes answered")
		}
		if !slices.Equal(probedModes, c.expectModes) {
			t.Fatalf("proxy %t: the walk probed %v, expected %v", c.connectSettings.ProxySettings != nil, probedModes, c.expectModes)
		}
	}
}

// A platform transport of a proxied strategy offers no h3 mode, which would
// resolve the platform host on this host before it dials, even through a
// packet endpoint of the owner's: an explicit h3 target falls back to h1, which
// carries the transport through the proxy. A strategy with no proxy keeps h3.
func TestProxiedPlatformTransportOffersNoH3(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, c := range []struct {
		proxied        bool
		packetEndpoint bool
	}{
		{proxied: true},
		{proxied: true, packetEndpoint: true},
		{proxied: false},
	} {
		proxied := c.proxied
		strategySettings := DefaultClientStrategySettings()
		if proxied {
			strategySettings.ProxySettings = testProxySettings()
		}
		strategy := NewClientStrategy(ctx, strategySettings)
		settings := testingFamilyTransportSettings()
		if c.packetEndpoint {
			settings.H3PacketConnFactory = func(ctx context.Context) (net.PacketConn, error) {
				return net.ListenPacket("udp4", "127.0.0.1:0")
			}
		}
		// held: the mode shape is decided at construction, no dial is needed
		settings.StartDisabled = true
		transport := NewPlatformTransportWithTargetMode(
			ctx,
			strategy,
			NewRouteManager(ctx, "proxy"),
			"https://localhost",
			testingFamilyAuth(),
			TransportModeH3,
			settings,
		)
		expectTarget := TransportModeH3
		if proxied {
			expectTarget = TransportModeH1
		}
		if transport.targetMode != expectTarget {
			t.Fatalf("proxied %t: target mode = %s, expected %s", proxied, transport.targetMode, expectTarget)
		}
		for _, h3Mode := range []TransportMode{TransportModeH3, TransportModeH3Dns, TransportModeH3DnsPump} {
			if offered := transport.modePreference(h3Mode) != modePreferenceNone; offered == proxied {
				t.Fatalf("proxied %t: offers %s = %t", proxied, h3Mode, offered)
			}
		}
		if transport.modePreference(TransportModeH1) == modePreferenceNone {
			t.Fatalf("proxied %t: h1 is not offered", proxied)
		}
		transport.Close()
		strategy.Close()
	}
}

// A block attempt whose space cannot carry the carriers it tries -- a udp
// carrier through a proxy -- dialed nothing, so it is no evidence against the
// extender: the attempt says nothing, as a refusal of this host's memory budget
// does, and the suspect is kept for an attempt that can dial.
func TestExtenderBlockAttemptProxyRefusalIsNoEvidence(t *testing.T) {
	harness := newTestBlockEvidence(t)
	harness.released("192.0.2.1")
	harness.dialFailed("192.0.2.1")
	harness.clock.advance(harness.settings.AttemptSettleTimeout)
	suspects := harness.look().AttemptSuspects
	if len(suspects) != 1 {
		t.Fatalf("suspects = %d, expected the one released address", len(suspects))
	}
	probedModes := []ExtenderConnectMode{}
	outcome := harness.directory.AttemptBlockSuspect(context.Background(), suspects[0], func(ctx context.Context, extenderConfig *ExtenderConfig) error {
		probedModes = append(probedModes, extenderConfig.Profile.ConnectMode)
		return fmt.Errorf("dial: %w", ErrExtenderPacketCarrierUnavailable)
	})
	if outcome != ExtenderBlockAttemptAborted {
		t.Fatalf("an attempt the space could not dial came to %s after probing %v", outcome, probedModes)
	}
	harness.requireSuspectCount("after an attempt the space could not dial", 1)
}
