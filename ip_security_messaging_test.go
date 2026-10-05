package connect

import (
	"context"
	"net/netip"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// whatsAppTestPath is a client -> destination tuple; the Steam fixture
// builder is generic over address, transport and port.
func whatsAppTestPath(address string, transport IpProtocol, port int, syn bool) *IpPath {
	return steamTestPath(netip.MustParseAddr(address), transport, port, syn)
}

func TestMetaNetworkPrefixSnapshot(t *testing.T) {
	expected := []string{
		"31.13.24.0/21",
		"31.13.64.0/18",
		"45.64.40.0/22",
		"57.141.0.0/20",
		"57.141.16.0/21",
		"57.141.24.0/23",
		"57.144.0.0/14",
		"66.220.144.0/20",
		"69.63.176.0/20",
		"69.171.224.0/19",
		"74.119.76.0/22",
		"102.132.96.0/20",
		"103.4.96.0/22",
		"129.134.0.0/16",
		"147.75.208.0/20",
		"157.240.0.0/16",
		"163.70.128.0/17",
		"163.77.128.0/17",
		"173.252.64.0/18",
		"179.60.192.0/22",
		"185.60.216.0/22",
		"185.89.216.0/22",
		"204.15.20.0/22",
		"2401:db00::/32",
		"2620:0:1c00::/40",
		"2a03:2880::/31",
		"2a03:2887:ff2c::/47",
		"2a03:83e0::/32",
	}
	if len(metaNetworkPrefixes) != len(expected) {
		t.Fatalf("Meta prefix count = %d, want snapshot count %d", len(metaNetworkPrefixes), len(expected))
	}

	v4Count, v6Count := 0, 0
	for i, prefix := range metaNetworkPrefixes {
		if prefix != prefix.Masked() {
			t.Fatalf("prefix %d is not masked: %s", i, prefix)
		}
		if got := prefix.String(); got != expected[i] {
			t.Fatalf("prefix %d = %s, want %s", i, got, expected[i])
		}
		if prefix.Addr().Is4() {
			v4Count++
		} else {
			v6Count++
		}
		for j, other := range metaNetworkPrefixes {
			if i != j && prefix.Contains(other.Addr()) {
				t.Fatalf("prefix %s contains prefix %s", prefix, other)
			}
		}
	}
	if v4Count != 23 || v6Count != 5 {
		t.Fatalf("Meta prefixes = %d IPv4 / %d IPv6, want 23 / 5", v4Count, v6Count)
	}
}

func TestMetaNetworkPrefixBoundaries(t *testing.T) {
	inSnapshot := func(address netip.Addr) bool {
		for _, prefix := range metaNetworkPrefixes {
			if prefix.Contains(address) {
				return true
			}
		}
		return false
	}
	for _, prefix := range metaNetworkPrefixes {
		first := prefix.Masked().Addr()
		last := lastAddressInPrefix(prefix)
		for _, address := range []netip.Addr{first, last} {
			path := steamTestPath(address, IpProtocolTcp, whatsAppChatPort, false)
			if !isWhatsAppMetaEndpoint(path) {
				t.Errorf("Meta prefix boundary %s did not match", address)
			}
		}
		for _, address := range []netip.Addr{first.Prev(), last.Next()} {
			// collapsed prefixes can still be adjacent: the neighbor may be
			// the edge of the next snapshot prefix
			if !address.IsValid() || inSnapshot(address) {
				continue
			}
			path := steamTestPath(address, IpProtocolTcp, whatsAppChatPort, false)
			if isWhatsAppMetaEndpoint(path) {
				t.Errorf("address adjacent to %s matched: %s", prefix, address)
			}
		}
	}
}

// The exception is the intersection of Meta address space, TCP and the chat
// port: any one of the three alone admits nothing.
func TestWhatsAppMetaEndpointScopeAndSettings(t *testing.T) {
	settings := DefaultMessagingSecurityPolicySettings()
	for _, address := range []string{"157.240.0.53", "31.13.64.51", "2a03:2880:f20c:e3:face:b00c:0:167"} {
		if !isSanctionedMessagingEndpoint(settings, whatsAppTestPath(address, IpProtocolTcp, 5222, false)) {
			t.Fatalf("default WhatsApp exception did not match %s:5222", address)
		}
	}

	path := whatsAppTestPath("157.240.0.53", IpProtocolTcp, 5222, false)
	if isSanctionedMessagingEndpoint(settings, path.Reverse()) {
		t.Fatal("reverse-direction tuple matched the destination-scoped WhatsApp exception")
	}
	for _, near := range []struct {
		name string
		path *IpPath
	}{
		{"non-Meta destination", whatsAppTestPath("8.8.8.8", IpProtocolTcp, 5222, false)},
		{"non-Meta IPv6 destination", whatsAppTestPath("2001:4860:4860::8888", IpProtocolTcp, 5222, false)},
		{"port 5223 before a capture confirms it", whatsAppTestPath("157.240.0.53", IpProtocolTcp, 5223, false)},
		{"another Meta port", whatsAppTestPath("157.240.0.53", IpProtocolTcp, 4244, false)},
		{"udp", whatsAppTestPath("157.240.0.53", IpProtocolUdp, 5222, false)},
	} {
		if isSanctionedMessagingEndpoint(settings, near.path) {
			t.Fatalf("%s matched the WhatsApp exception", near.name)
		}
	}
	wrongVersion := *path
	wrongVersion.Version = 6
	if isSanctionedMessagingEndpoint(settings, &wrongVersion) {
		t.Fatal("IPv4 address labeled IPv6 matched the WhatsApp exception")
	}

	settings.AllowWhatsApp = false
	if isSanctionedMessagingEndpoint(settings, path) {
		t.Fatal("disabled WhatsApp exception matched")
	}
	settings.AllowWhatsApp = true
	settings.Enabled = false
	if isSanctionedMessagingEndpoint(settings, path) {
		t.Fatal("disabled messaging master switch matched the WhatsApp exception")
	}
	if isSanctionedMessagingEndpoint(nil, path) {
		t.Fatal("nil messaging settings matched the WhatsApp exception")
	}
}

func TestWhatsAppMetaEndpointZeroAlloc(t *testing.T) {
	path := whatsAppTestPath("157.240.0.53", IpProtocolTcp, 5222, false)
	if allocations := testing.AllocsPerRun(1000, func() {
		if !isWhatsAppMetaEndpoint(path) {
			t.Fatal("WhatsApp endpoint did not match")
		}
	}); allocations != 0 {
		t.Fatalf("WhatsApp endpoint lookup allocates %.2f objects per call, want 0", allocations)
	}
}

// dmcaTestTcpFlow drives one TCP flow from its SYN, as a provider sees it,
// and returns the verdict after each payload.
func dmcaTestTcpFlow(detector *dmcaDetector, syn *IpPath, payloads ...[]byte) []dmcaVerdict {
	detector.classify(syn, nil)
	data := *syn
	data.Syn = false
	verdicts := []dmcaVerdict{}
	for _, payload := range payloads {
		verdicts = append(verdicts, detector.classify(&data, payload))
	}
	return verdicts
}

// WhatsApp's Noise session to Meta's edge on 5222 is random after its short
// header, so the encrypted heuristic dropped it after three payloads. It is
// now allowed through the whole inspection budget and beyond, while the
// BitTorrent signatures keep precedence on every inspected packet.
func TestDmcaWhatsAppMetaExceptionAndPrecedence(t *testing.T) {
	t.Run("encrypted TCP to Meta 5222 allowed", func(t *testing.T) {
		for _, address := range []string{"157.240.0.53", "2a03:2880:f20c:e3:face:b00c:0:167"} {
			settings := DefaultDmcaSecurityPolicySettings()
			detector := newDmcaDetector(nil, settings, newWebStandardDetector(DefaultWebStandardSettings()))
			payloads := [][]byte{}
			for i := 0; i < settings.InspectionPacketBudget+2; i++ {
				payloads = append(payloads, encryptedPayload(512))
			}
			if !payloadLooksEncrypted(payloads[0], settings) {
				t.Fatal("fixture must exercise the encrypted heuristic")
			}
			for i, verdict := range dmcaTestTcpFlow(detector, whatsAppTestPath(address, IpProtocolTcp, 5222, true), payloads...) {
				if verdict != dmcaAllow {
					t.Fatalf("WhatsApp payload %d to %s = %d, want allow", i, address, verdict)
				}
			}
		}
	})

	t.Run("BitTorrent signature wins", func(t *testing.T) {
		detector := newDmcaDetector(nil, DefaultDmcaSecurityPolicySettings(), newWebStandardDetector(DefaultWebStandardSettings()))
		verdicts := dmcaTestTcpFlow(detector, whatsAppTestPath("157.240.0.53", IpProtocolTcp, 5222, true), btHandshake())
		if verdicts[0] != dmcaBittorrent {
			t.Fatalf("BitTorrent on the Meta endpoint = %d, want bittorrent", verdicts[0])
		}
	})

	t.Run("BitTorrent signature after the allow still wins", func(t *testing.T) {
		detector := newDmcaDetector(nil, DefaultDmcaSecurityPolicySettings(), newWebStandardDetector(DefaultWebStandardSettings()))
		verdicts := dmcaTestTcpFlow(detector, whatsAppTestPath("157.240.0.53", IpProtocolTcp, 5222, true), encryptedPayload(512), btHandshake())
		if verdicts[0] != dmcaAllow || verdicts[1] != dmcaBittorrent {
			t.Fatalf("allowed Meta flow carrying BitTorrent = %v, want allow then bittorrent", verdicts)
		}
	})

	tests := []struct {
		name      string
		address   string
		transport IpProtocol
		port      int
		configure func(*DmcaSecurityPolicySettings)
	}{
		{name: "non-Meta destination", address: "8.8.8.8", transport: IpProtocolTcp, port: 5222},
		{name: "Meta port 5223", address: "157.240.0.53", transport: IpProtocolTcp, port: 5223},
		{name: "Meta udp 5222", address: "157.240.0.53", transport: IpProtocolUdp, port: 5222},
		{
			name:      "WhatsApp disabled",
			address:   "157.240.0.53",
			transport: IpProtocolTcp,
			port:      5222,
			configure: func(settings *DmcaSecurityPolicySettings) {
				settings.Messaging.AllowWhatsApp = false
			},
		},
		{
			name:      "messaging disabled",
			address:   "157.240.0.53",
			transport: IpProtocolTcp,
			port:      5222,
			configure: func(settings *DmcaSecurityPolicySettings) {
				settings.Messaging.Enabled = false
			},
		},
		{
			name:      "nil messaging settings",
			address:   "157.240.0.53",
			transport: IpProtocolTcp,
			port:      5222,
			configure: func(settings *DmcaSecurityPolicySettings) {
				settings.Messaging = nil
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := DefaultDmcaSecurityPolicySettings()
			if tt.configure != nil {
				tt.configure(settings)
			}
			detector := newDmcaDetector(nil, settings, newWebStandardDetector(DefaultWebStandardSettings()))
			path := whatsAppTestPath(tt.address, tt.transport, tt.port, tt.transport == IpProtocolTcp)
			if tt.transport == IpProtocolTcp {
				detector.classify(path, nil)
				data := *path
				data.Syn = false
				path = &data
			}
			var verdict dmcaVerdict
			for i := 0; i < settings.EncryptedDecisionPackets; i++ {
				verdict = detector.classify(path, encryptedPayload(512))
			}
			if verdict != dmcaDropEncrypted {
				t.Fatalf("near-miss WhatsApp flow verdict = %d, want encrypted drop", verdict)
			}
		})
	}
}

// End to end through the policy a provider runs: the WhatsApp flow passes
// every packet, a BitTorrent handshake on the same endpoint is an incident,
// and the same encrypted flow to an address outside Meta is still dropped.
func TestSecurityPolicyWhatsAppMetaException(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inspect := DefaultProviderSecurityPolicy(ctx).InspectIngress

	flow := func(address string, payloads ...[]byte) []SecurityPolicyResult {
		syn := whatsAppTestPath(address, IpProtocolTcp, 5222, true)
		results := []SecurityPolicyResult{}
		result, err := inspect(protocol.ProvideMode_Public, syn, nil)
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, result)
		data := *syn
		data.Syn = false
		for _, payload := range payloads {
			result, err := inspect(protocol.ProvideMode_Public, &data, payload)
			if err != nil {
				t.Fatal(err)
			}
			results = append(results, result)
		}
		return results
	}

	payloads := [][]byte{}
	for i := 0; i < DefaultDmcaSecurityPolicySettings().InspectionPacketBudget+2; i++ {
		payloads = append(payloads, encryptedPayload(512))
	}
	for i, result := range flow("157.240.0.53", payloads...) {
		if result != SecurityPolicyResultAllow {
			t.Fatalf("WhatsApp packet %d = %v, want allow", i, result)
		}
	}

	results := flow("157.240.0.54", btHandshake())
	if results[1] != SecurityPolicyResultIncident {
		t.Fatalf("BitTorrent on the Meta endpoint = %v, want incident", results[1])
	}

	results = flow("8.8.8.8", payloads[:DefaultDmcaSecurityPolicySettings().EncryptedDecisionPackets]...)
	if last := results[len(results)-1]; last != SecurityPolicyResultDrop {
		t.Fatalf("encrypted 5222 flow outside Meta = %v, want drop", last)
	}
}
