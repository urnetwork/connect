package connect

import (
	"net"
	"strconv"
	"testing"
)

// Where an h3 carrier asks an extender to relay to.
//
// The requirement is that h3 and dns go to the ALT service when one is
// configured, exactly as a direct dial does, and that the destination travels
// as a name so the extender resolves it. These pin the naming half; the
// extender half is in connect/extender/extender_datagram_host_test.go.

func newH3DestinationTransport(altUrl string, dnsPort int, h3Port int) *PlatformTransport {
	return &PlatformTransport{
		settings: &PlatformTransportSettings{
			AltUrl:  altUrl,
			DnsPort: dnsPort,
			H3Port:  h3Port,
		},
	}
}

// With an alt deployment, every h3 carrier targets alt rather than the
// platform host. The platform host remains the sni and the quic identity; this
// is only where the packets go (L4).
func TestH3CarrierDestinationPrefersTheAltService(t *testing.T) {
	// A port on the alt url pins every carrier to it (L4).
	pinned := newH3DestinationTransport("https://alt.example:4443", 53, 443)
	// Without one, the dns carriers offer alt's whodis listener on both ports,
	// 53 first because that is the one reachable through a router (L2).
	unpinned := newH3DestinationTransport("https://alt.example", 53, 443)

	for _, testCase := range []struct {
		name      string
		transport *PlatformTransport
		mode      TransportMode
		wantPorts []int
	}{
		{"pinned h3", pinned, TransportModeH3, []int{4443}},
		{"pinned dns", pinned, TransportModeH3Dns, []int{4443}},
		{"pinned pump", pinned, TransportModeH3DnsPump, []int{4443}},
		{"unpinned h3", unpinned, TransportModeH3, []int{443}},
		{"unpinned dns", unpinned, TransportModeH3Dns, []int{53, DefaultWhodisPort}},
		{"unpinned pump", unpinned, TransportModeH3DnsPump, []int{53, DefaultWhodisPort}},
	} {
		host, ports, err := testCase.transport.h3CarrierDestination(testCase.mode, "connect.example")
		if err != nil {
			t.Errorf("%s: %v", testCase.name, err)
			continue
		}
		// The alt host, in every mode. This is the requirement: h3 and dns go
		// to alt, and the platform host stays the sni and the quic identity.
		if host != "alt.example" {
			t.Errorf("%s: host = %q, want the alt host", testCase.name, host)
		}
		if len(ports) != len(testCase.wantPorts) {
			t.Errorf("%s: ports = %v, want %v", testCase.name, ports, testCase.wantPorts)
			continue
		}
		for i, wantPort := range testCase.wantPorts {
			if ports[i] != wantPort {
				t.Errorf("%s: ports = %v, want %v", testCase.name, ports, testCase.wantPorts)
				break
			}
		}
	}
}

// Without an alt deployment the plain carrier stays on the platform host,
// which is the behavior of every space with no alt.
func TestH3CarrierDestinationFallsBackToThePlatformHost(t *testing.T) {
	transport := newH3DestinationTransport("", 53, 443)

	host, ports, err := transport.h3CarrierDestination(TransportModeH3, "connect.example")
	if err != nil {
		t.Fatalf("h3: %v", err)
	}
	if host != "connect.example" {
		t.Errorf("host = %q, want the platform host", host)
	}
	if len(ports) != 1 || ports[0] != 443 {
		t.Errorf("ports = %v, want [443]", ports)
	}

	// The pump has no host of its own to fall back to, so it says so rather
	// than deriving an infrastructure name from the packet codec (L3).
	if _, _, err := transport.h3CarrierDestination(TransportModeH3DnsPump, "connect.example"); err == nil {
		t.Error("a pump with no alt and no pump host should be an error")
	}
}

// The direct path and the extender path choose the same destination, which is
// the point of sharing the selection: a client that reaches the operator
// through an extender must not end up at a different service.
func TestH3ExtenderDestinationMatchesTheDirectSelection(t *testing.T) {
	transport := newH3DestinationTransport("https://alt.example:4443", 53, 443)
	transport.clientStrategy = extenderOnlyStrategy()

	for _, mode := range []TransportMode{
		TransportModeH3, TransportModeH3Dns, TransportModeH3DnsPump,
	} {
		host, ports, err := transport.h3CarrierDestination(mode, "connect.example")
		if err != nil {
			t.Errorf("%s: %v", mode, err)
			continue
		}
		extenderConfig, destination, err := transport.h3ExtenderDestination(mode, "connect.example")
		if err != nil {
			t.Errorf("%s: %v", mode, err)
			continue
		}
		if extenderConfig == nil {
			t.Errorf("%s: an extender-only strategy should name an extender", mode)
			continue
		}
		want := joinHostPortInt(host, ports[0])
		if destination != want {
			t.Errorf("%s: destination = %q, want %q", mode, destination, want)
		}
	}
}

// A strategy with any direct path keeps dialing h3 exactly as before, so this
// cannot regress an ordinary client.
func TestH3ExtenderDestinationIsEmptyWithADirectPath(t *testing.T) {
	transport := newH3DestinationTransport("https://alt.example:4443", 53, 443)

	for name, settings := range map[string]*ClientStrategySettings{
		"normal enabled":    extenderStrategySettings(true, false),
		"resilient enabled": extenderStrategySettings(false, true),
		"no extenders":      extenderStrategySettings(false, false),
	} {
		if name == "no extenders" {
			settings.ExtenderConfigs = nil
		}
		transport.clientStrategy = &ClientStrategy{settings: settings}
		extenderConfig, destination, err := transport.h3ExtenderDestination(
			TransportModeH3, "connect.example",
		)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if extenderConfig != nil || destination != "" {
			t.Errorf("%s: should not route h3 through an extender", name)
		}
	}
}

// extenderStrategySettings is a strategy configuration with one extender and
// the direct strategies as given.
func extenderStrategySettings(normal bool, resilient bool) *ClientStrategySettings {
	settings := DefaultClientStrategySettings()
	settings.EnableNormal = normal
	settings.EnableResilient = resilient
	settings.ExtenderConfigs = []*ExtenderConfig{
		{
			Profile: ExtenderProfile{
				ConnectMode: ExtenderConnectModeQuic,
				ServerName:  "spoof.invalid",
				Port:        443,
			},
			Secret: "destination-test-secret",
		},
	}
	return settings
}

// extenderOnlyStrategy has no direct path, which is what puts h3 on an
// extender.
func extenderOnlyStrategy() *ClientStrategy {
	return &ClientStrategy{settings: extenderStrategySettings(false, false)}
}

func joinHostPortInt(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}
