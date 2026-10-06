package connect

// Tests of the provider-scoped messaging exception (ip_security_messaging.go):
// the Meta prefix table, the exception's scope and settings, and its place in
// the dmca detector and the provider policy.

import (
	"context"
	"net/netip"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// A client -> destination tuple; the Steam fixture builder is generic over
// address, transport and port.
func whatsAppTestPath(address string, transport IpProtocol, port int, syn bool) *IpPath {
	return steamTestPath(netip.MustParseAddr(address), transport, port, syn)
}

// The table is generated (ip_security_messaging_meta.go) and every release
// build refreshes it, so this checks its shape rather than its contents, as
// for the CFAA tables: masked, sorted IPv4 then IPv6, pairwise disjoint and
// collapsed (no two siblings one prefix would cover). That it still covers
// where WhatsApp's chat edge resolves is the generator's anchor check, which
// security's TestCheckedInMetaSnapshot runs on this table, so no production
// prefix is written into this test.
func TestMetaNetworkPrefixInvariant(t *testing.T) {
	v4Count, v6Count := 0, 0
	for i, prefix := range metaNetworkPrefixes {
		if !prefix.IsValid() || prefix != prefix.Masked() {
			t.Fatalf("prefix %d is not masked: %s", i, prefix)
		}
		if prefix.Addr().Is4() {
			if 0 < v6Count {
				t.Fatalf("IPv4 prefix %s follows the IPv6 prefixes", prefix)
			}
			v4Count++
		} else {
			v6Count++
		}
		if i == 0 {
			continue
		}
		previous := metaNetworkPrefixes[i-1]
		if previous.Addr().BitLen() == prefix.Addr().BitLen() &&
			lastAddressInPrefix(previous).Compare(prefix.Addr()) >= 0 {
			t.Fatalf("prefixes %s and %s are not sorted and disjoint", previous, prefix)
		}
		if previous.Bits() == prefix.Bits() &&
			netip.PrefixFrom(previous.Addr(), previous.Bits()-1).Masked() == netip.PrefixFrom(prefix.Addr(), prefix.Bits()-1).Masked() {
			t.Fatalf("prefixes %s and %s are siblings left uncollapsed", previous, prefix)
		}
	}
	if v4Count == 0 || v6Count == 0 {
		t.Fatalf("Meta prefixes = %d IPv4 / %d IPv6, want both families", v4Count, v6Count)
	}
}

// Every prefix's first and last address match, and the addresses just outside
// it do not, unless they are the edge of a neighboring prefix.
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
	meta := metaTestAddress(t, 4, 0)
	for _, address := range []string{meta, metaTestAddress(t, 4, -1), metaTestAddress(t, 6, 0)} {
		if !isSanctionedMessagingEndpoint(settings, whatsAppTestPath(address, IpProtocolTcp, 5222, false)) {
			t.Fatalf("default WhatsApp exception did not match %s:5222", address)
		}
	}

	path := whatsAppTestPath(meta, IpProtocolTcp, 5222, false)
	if isSanctionedMessagingEndpoint(settings, path.Reverse()) {
		t.Fatal("reverse-direction tuple matched the destination-scoped WhatsApp exception")
	}
	for _, near := range []struct {
		name string
		path *IpPath
	}{
		{name: "non-Meta destination", path: whatsAppTestPath(metaTestOutsideAddress, IpProtocolTcp, 5222, false)},
		{name: "non-Meta IPv6 destination", path: whatsAppTestPath(metaTestOutsideAddressIpv6, IpProtocolTcp, 5222, false)},
		{name: "port 5223 before a capture confirms it", path: whatsAppTestPath(meta, IpProtocolTcp, 5223, false)},
		{name: "another Meta port", path: whatsAppTestPath(meta, IpProtocolTcp, 4244, false)},
		{name: "udp", path: whatsAppTestPath(meta, IpProtocolUdp, 5222, false)},
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

// The lookup on the flow path does not allocate.
func TestWhatsAppMetaEndpointZeroAlloc(t *testing.T) {
	path := whatsAppTestPath(metaTestAddress(t, 4, 0), IpProtocolTcp, 5222, false)
	if allocations := testing.AllocsPerRun(1000, func() {
		if !isWhatsAppMetaEndpoint(path) {
			t.Fatal("WhatsApp endpoint did not match")
		}
	}); allocations != 0 {
		t.Fatalf("WhatsApp endpoint lookup allocates %.2f objects per call, want 0", allocations)
	}
}

// Addresses outside the Meta prefixes, from the documentation ranges.
const (
	metaTestOutsideAddress     = "192.0.2.53"
	metaTestOutsideAddressIpv6 = "2001:db8::53"
)

// An address inside the Meta prefixes, taken from the table so that no
// production address is written into the tests: the first address after the
// network address of the family's prefix at the index, counted from the end
// when negative.
func metaTestAddress(t *testing.T, ipVersion int, index int) string {
	t.Helper()
	familyPrefixes := []netip.Prefix{}
	for _, prefix := range metaNetworkPrefixes {
		if prefix.Addr().Is4() == (ipVersion == 4) {
			familyPrefixes = append(familyPrefixes, prefix)
		}
	}
	if index < 0 {
		index += len(familyPrefixes)
	}
	if index < 0 || len(familyPrefixes) <= index {
		t.Fatalf("the Meta prefixes hold %d ipv%d prefixes, no index %d", len(familyPrefixes), ipVersion, index)
	}
	return familyPrefixes[index].Masked().Addr().Next().String()
}

// Drives one TCP flow from its SYN, as a provider sees it, and returns the
// verdict after each payload.
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
	meta := metaTestAddress(t, 4, 0)

	// encrypted TCP to Meta 5222 is allowed
	for _, address := range []string{meta, metaTestAddress(t, 6, 0)} {
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

	// a BitTorrent signature wins
	detector := newDmcaDetector(nil, DefaultDmcaSecurityPolicySettings(), newWebStandardDetector(DefaultWebStandardSettings()))
	verdicts := dmcaTestTcpFlow(detector, whatsAppTestPath(meta, IpProtocolTcp, 5222, true), btHandshake())
	if verdicts[0] != dmcaBittorrent {
		t.Fatalf("BitTorrent on the Meta endpoint = %d, want bittorrent", verdicts[0])
	}

	// a BitTorrent signature after the allow still wins
	detector = newDmcaDetector(nil, DefaultDmcaSecurityPolicySettings(), newWebStandardDetector(DefaultWebStandardSettings()))
	verdicts = dmcaTestTcpFlow(detector, whatsAppTestPath(meta, IpProtocolTcp, 5222, true), encryptedPayload(512), btHandshake())
	if verdicts[0] != dmcaAllow || verdicts[1] != dmcaBittorrent {
		t.Fatalf("allowed Meta flow carrying BitTorrent = %v, want allow then bittorrent", verdicts)
	}

	// near misses are judged by the encrypted heuristic
	cases := []struct {
		name      string
		address   string
		transport IpProtocol
		port      int
		configure func(*DmcaSecurityPolicySettings)
	}{
		{name: "non-Meta destination", address: metaTestOutsideAddress, transport: IpProtocolTcp, port: 5222},
		{name: "Meta port 5223", address: meta, transport: IpProtocolTcp, port: 5223},
		{name: "Meta udp 5222", address: meta, transport: IpProtocolUdp, port: 5222},
		{
			name:      "WhatsApp disabled",
			address:   meta,
			transport: IpProtocolTcp,
			port:      5222,
			configure: func(settings *DmcaSecurityPolicySettings) {
				settings.Messaging.AllowWhatsApp = false
			},
		},
		{
			name:      "messaging disabled",
			address:   meta,
			transport: IpProtocolTcp,
			port:      5222,
			configure: func(settings *DmcaSecurityPolicySettings) {
				settings.Messaging.Enabled = false
			},
		},
		{
			name:      "nil messaging settings",
			address:   meta,
			transport: IpProtocolTcp,
			port:      5222,
			configure: func(settings *DmcaSecurityPolicySettings) {
				settings.Messaging = nil
			},
		},
	}
	for _, c := range cases {
		settings := DefaultDmcaSecurityPolicySettings()
		if c.configure != nil {
			c.configure(settings)
		}
		detector := newDmcaDetector(nil, settings, newWebStandardDetector(DefaultWebStandardSettings()))
		path := whatsAppTestPath(c.address, c.transport, c.port, c.transport == IpProtocolTcp)
		if c.transport == IpProtocolTcp {
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
			t.Fatalf("%s: near-miss WhatsApp flow verdict = %d, want encrypted drop", c.name, verdict)
		}
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
	for i, result := range flow(metaTestAddress(t, 4, 0), payloads...) {
		if result != SecurityPolicyResultAllow {
			t.Fatalf("WhatsApp packet %d = %v, want allow", i, result)
		}
	}

	results := flow(metaTestAddress(t, 4, 1), btHandshake())
	if results[1] != SecurityPolicyResultIncident {
		t.Fatalf("BitTorrent on the Meta endpoint = %v, want incident", results[1])
	}

	results = flow(metaTestOutsideAddress, payloads[:DefaultDmcaSecurityPolicySettings().EncryptedDecisionPackets]...)
	if last := results[len(results)-1]; last != SecurityPolicyResultDrop {
		t.Fatalf("encrypted 5222 flow outside Meta = %v, want drop", last)
	}
}
