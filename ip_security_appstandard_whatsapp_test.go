// WhatsApp Noise transport detector. No capture of the real apps was
// available: the vectors follow the byte layout of the public clients cited
// at whatsAppStream (ip_security_appstandard.go), the protobuf is encoded by
// protowire rather than by the detector, and every field that is random on
// the wire (keys, ciphertext, routing info) is deterministic test bytes.
// Positive flows first prove they exercise the encrypted heuristic; near
// misses, other ports and BitTorrent stay dropped or incidents; the Meta
// prefix exception is the backstop behind the detector.
package connect

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/urnetwork/connect/protocol"
)

// The port the flows below are sent to.
const whatsAppTestPort = whatsAppChatPort

// Appends one length-delimited protobuf field to b.
func whatsAppTestBytesField(b []byte, field protowire.Number, value []byte) []byte {
	b = protowire.AppendTag(b, field, protowire.BytesType)
	return protowire.AppendBytes(b, value)
}

// The Noise XX first message whatsmeow and Baileys send:
// HandshakeMessage{clientHello (2): {ephemeral (1)}}, 36 bytes.
func whatsAppTestXxHello(seed string) []byte {
	hello := whatsAppTestBytesField(nil, 1, appTestBytes(seed+"-ephemeral", 32))
	return whatsAppTestBytesField(nil, 2, hello)
}

// The Noise IK first message of the mobile protocol (consonance): the ephemeral
// key, the encrypted static key (32 + 16) and the encrypted payload.
func whatsAppTestIkHello(seed string, payloadLength int) []byte {
	hello := whatsAppTestBytesField(nil, 1, appTestBytes(seed+"-ephemeral", 32))
	hello = whatsAppTestBytesField(hello, 2, appTestBytes(seed+"-static", 48))
	hello = whatsAppTestBytesField(hello, 3, appTestBytes(seed+"-payload", payloadLength))
	return whatsAppTestBytesField(nil, 2, hello)
}

// A hello larger than one segment, with the newer ClientHello fields of
// whatsmeow's waWa6 protobuf: pqMode (9) and extendedEphemeral (10).
func whatsAppTestLargeHello(seed string, extendedLength int) []byte {
	hello := whatsAppTestBytesField(nil, 1, appTestBytes(seed+"-ephemeral", 32))
	hello = protowire.AppendTag(hello, 9, protowire.VarintType)
	hello = protowire.AppendVarint(hello, 1)
	hello = whatsAppTestBytesField(hello, 10, appTestBytes(seed+"-extended", extendedLength))
	return whatsAppTestBytesField(nil, 2, hello)
}

// The XX third message:
// HandshakeMessage{clientFinish (4): {static (1), payload (2)}}.
func whatsAppTestClientFinish(seed string, payloadLength int) []byte {
	finish := whatsAppTestBytesField(nil, 1, appTestBytes(seed+"-static", 48))
	finish = whatsAppTestBytesField(finish, 2, appTestBytes(seed+"-payload", payloadLength))
	return whatsAppTestBytesField(nil, 4, finish)
}

// Prefixes a frame with its 3-byte big-endian length.
func whatsAppTestFrame(data []byte) []byte {
	return appTestCat([]byte{byte(len(data) >> 16), byte(len(data) >> 8), byte(len(data))}, data)
}

// The connection header: 'W','A' and the two version bytes.
func whatsAppTestHeader(major byte, minor byte) []byte {
	return []byte{'W', 'A', major, minor}
}

// The edge routing prefix: 'E','D',0,1, then the routing info with its 3-byte
// big-endian length.
func whatsAppTestEdge(routing []byte) []byte {
	return appTestCat([]byte{'E', 'D', 0, 1}, whatsAppTestFrame(routing))
}

// 4 bytes of routing info starting with 0x08, the shape nDPI's WhatsApp
// signature matches.
func whatsAppTestRouting(seed string) []byte {
	return appTestCat([]byte{0x08}, appTestBytes(seed+"-routing", 3))
}

// A transport frame: ciphertext and its 16-byte tag.
func whatsAppTestTransport(seed string, length int) []byte {
	return whatsAppTestFrame(appTestBytes(seed, length))
}

// The layout whatsmeow and Baileys write: the header and the XX hello at once,
// the client finish after the server hello, then transport frames.
func whatsAppTestWebFlow(seed string) [][]byte {
	return [][]byte{
		appTestCat(whatsAppTestHeader(6, 3), whatsAppTestFrame(whatsAppTestXxHello(seed))),
		whatsAppTestFrame(whatsAppTestClientFinish(seed, 180)),
		whatsAppTestTransport(seed+"-t0", 240),
		whatsAppTestTransport(seed+"-t1", 320),
	}
}

// The mobile layout as yowsup writes it: the edge header, the routing info and
// the header (yowsup's version 4.0) in separate segments, then the IK hello and
// transport frames.
func whatsAppTestNativeFlow(seed string) [][]byte {
	return [][]byte{
		{'E', 'D', 0, 1},
		whatsAppTestFrame(whatsAppTestRouting(seed)),
		whatsAppTestHeader(4, 0),
		whatsAppTestFrame(whatsAppTestIkHello(seed, 220)),
		whatsAppTestTransport(seed+"-t0", 260),
		whatsAppTestTransport(seed+"-t1", 300),
		whatsAppTestTransport(seed+"-t2", 200),
	}
}

// Requires every payload long enough to judge to exercise the encrypted
// heuristic; shorter prefix segments are inconclusive.
func requireWhatsAppEncrypted(t *testing.T, name string, payloads [][]byte) {
	t.Helper()
	for _, payload := range payloads {
		if DefaultDmcaSecurityPolicySettings().MinEncryptedPayload <= len(payload) {
			requireLooksEncrypted(t, name, payload)
		}
	}
}

// Requires a flow whose payloads look encrypted to be dropped without the
// detector, and admitted with it.
func requireWhatsAppAllowed(t *testing.T, name string, payloads [][]byte, allowAt int) {
	t.Helper()
	requireWhatsAppEncrypted(t, name, payloads)
	legacy := classifyAll(newAppTestDetector(func(settings *DmcaSecurityPolicySettings) {
		settings.App.WhatsApp = false
	}), IpProtocolTcp, 48000, whatsAppTestPort, payloads...)
	if _, verdict := firstDecision(legacy); verdict != dmcaDropEncrypted {
		t.Fatalf("%s: without the whatsapp detector verdicts = %v, want drop", name, legacy)
	}
	requireWhatsAppAdmitted(t, name, payloads, allowAt)
}

// Requires the flow to be admitted from packet allowAt on, never dropped, for
// the whatsapp reason.
func requireWhatsAppAdmitted(t *testing.T, name string, payloads [][]byte, allowAt int) {
	t.Helper()
	detector := newAppTestDetector(nil)
	verdicts := classifyAll(detector, IpProtocolTcp, 48001, whatsAppTestPort, payloads...)
	if index, verdict := firstDecision(verdicts); index != allowAt || verdict != dmcaAllow {
		t.Fatalf("%s: verdicts = %v, want inspecting then allow from packet %d", name, verdicts, allowAt)
	}
	for i, verdict := range verdicts[allowAt:] {
		if verdict != dmcaAllow {
			t.Fatalf("%s: packet %d = %d (all %v), want allow", name, allowAt+i, verdict, verdicts)
		}
	}
	_, reason, _ := detector.classifyForSenderDetailed(Id{}, appTestPath(IpProtocolTcp, 48001, whatsAppTestPort, false), whatsAppTestTransport(name+"-after", 200))
	if reason != SecurityPolicyReasonAllowWhatsApp {
		t.Fatalf("%s: reason = %s, want allow-app-standard:whatsapp", name, reason)
	}
}

// Both public layouts, the edge prefix with the hello, a header or frame
// length written ahead of the hello, a hello across segments and every
// version byte below 16 are admitted, where without the detector the
// encrypted heuristic drops them.
func TestDmcaWhatsAppNoiseFlowsAllowed(t *testing.T) {
	web := whatsAppTestWebFlow("wa-web")

	// whatsmeow and Baileys: the header and the XX hello in one write
	requireWhatsAppAllowed(t, "web", web, 0)

	// the mobile layout in separate writes: the prefix completes with the hello
	requireWhatsAppAllowed(t, "native", whatsAppTestNativeFlow("wa-native"), 3)

	hello := whatsAppTestFrame(whatsAppTestIkHello("wa-split", 200))
	transports := [][]byte{whatsAppTestTransport("wa-split-t0", 240), whatsAppTestTransport("wa-split-t1", 280)}

	// the edge prefix, the header and the hello in one write
	edge := appTestCat(whatsAppTestEdge(whatsAppTestRouting("wa-edge")), whatsAppTestHeader(4, 0), hello)
	requireWhatsAppAllowed(t, "edge prefix with the hello", appTestCat2(edge, transports...), 0)

	// Baileys holding routing info writes the edge prefix before the XX hello.
	// That short opening is too structured to look encrypted, so without the
	// detector the heuristic let the flow through as plaintext; the detector
	// admits it for its own reason.
	baileys := appTestCat2(appTestCat(whatsAppTestEdge(whatsAppTestRouting("wa-baileys")), web[0]), web[1:]...)
	legacy := newAppTestDetector(func(settings *DmcaSecurityPolicySettings) {
		settings.App.WhatsApp = false
	})
	legacy.classify(appTestPath(IpProtocolTcp, 48002, whatsAppTestPort, true), nil)
	if _, reason, _ := legacy.classifyForSenderDetailed(Id{}, appTestPath(IpProtocolTcp, 48002, whatsAppTestPort, false), baileys[0]); reason != SecurityPolicyReasonAllowPlaintext {
		t.Fatalf("web with edge routing without the detector: reason = %s, want allow-plaintext", reason)
	}
	requireWhatsAppAdmitted(t, "web with edge routing", baileys, 0)

	// a client that writes the header, or the header and the frame length,
	// before the rest of the hello frame
	requireWhatsAppAllowed(t, "header alone", appTestCat2(whatsAppTestHeader(6, 3), appTestCat2(hello, transports...)...), 1)
	requireWhatsAppAllowed(t, "frame length with the header", appTestCat2(appTestCat(whatsAppTestHeader(6, 3), hello[:3]), appTestCat2(hello[3:], transports...)...), 1)

	// a hello larger than one segment: the key is in the first segment and the
	// rest of the frame follows
	large := appTestCat(whatsAppTestHeader(6, 3), whatsAppTestFrame(whatsAppTestLargeHello("wa-large", 1600)))
	requireWhatsAppAllowed(t, "hello across segments", [][]byte{large[:1200], large[1200:], transports[0], transports[1]}, 0)

	// any version byte below 16
	for _, version := range [][2]byte{{6, 3}, {4, 0}, {0, 0}, {15, 15}} {
		name := fmt.Sprintf("version %d.%d", version[0], version[1])
		requireWhatsAppAllowed(t, name, appTestCat2(appTestCat(whatsAppTestHeader(version[0], version[1]), hello), transports...), 0)
	}
}

// Openings that leave the layout anywhere -- the header, the edge prefix, the
// frame, the hello's fields or lengths, or the position in the flow -- are
// dropped as encrypted, and so is a WhatsApp opening on another port or over
// udp.
func TestDmcaWhatsAppNoiseNearMissesDrop(t *testing.T) {
	header := whatsAppTestHeader(6, 3)
	ephemeral := whatsAppTestBytesField(nil, 1, appTestBytes("wa-near-ephemeral", 32))
	static := whatsAppTestBytesField(nil, 2, appTestBytes("wa-near-static", 48))
	payload := whatsAppTestBytesField(nil, 3, appTestBytes("wa-near-payload", 220))
	inner := appTestCat(ephemeral, static, payload)
	hello := whatsAppTestBytesField(nil, 2, inner)
	frame := whatsAppTestFrame(hello)
	withLength := func(length int, data []byte) []byte {
		return appTestCat([]byte{byte(length >> 16), byte(length >> 8), byte(length)}, data)
	}
	// 34, the XX clientHello length, as a padded two-byte varint
	xx := whatsAppTestBytesField(nil, 1, appTestBytes("wa-near-xx", 32))
	paddedVarint := appTestCat([]byte{whatsAppClientHelloTag, 0x80 | byte(len(xx)), 0x00}, xx)
	shortKey := whatsAppTestBytesField(nil, 2, whatsAppTestBytesField(nil, 1, appTestBytes("wa-near-short", 31)))
	transports := [][]byte{whatsAppTestTransport("wa-near-t0", 240), whatsAppTestTransport("wa-near-t1", 280)}
	first := func(opening []byte) [][]byte {
		return appTestCat2(opening, transports...)
	}

	cases := []struct {
		name     string
		payloads [][]byte
	}{
		// a random 3-byte length prefix without the connection header
		{name: "frames without the header", payloads: appTestCat2(frame, transports...)},
		{name: "header with a random 3-byte length", payloads: first(appTestCat(header, appTestBytes("wa-near-random-length", 3), hello))},
		// near-miss headers
		{name: "lowercase header", payloads: first(appTestCat([]byte{'w', 'a', 6, 3}, frame))},
		{name: "swapped header", payloads: first(appTestCat([]byte{'A', 'W', 6, 3}, frame))},
		{name: "protocol version 16", payloads: first(appTestCat(whatsAppTestHeader(16, 3), frame))},
		{name: "dictionary version 16", payloads: first(appTestCat(whatsAppTestHeader(6, 16), frame))},
		{name: "edge header version 2", payloads: first(appTestCat([]byte{'E', 'D', 0, 2}, whatsAppTestFrame(whatsAppTestRouting("wa-near")), header, frame))},
		{name: "routing info past the bound", payloads: first(appTestCat([]byte{'E', 'D', 0, 1}, withLength(whatsAppMaxRoutingInfoLength+1, appTestBytes("wa-near-routing", 300))))},
		{name: "routing length overruns the header", payloads: first(appTestCat([]byte{'E', 'D', 0, 1}, withLength(6, whatsAppTestRouting("wa-near")), header, frame))},
		// near-miss frames
		{name: "server hello first", payloads: first(appTestCat(header, whatsAppTestFrame(whatsAppTestBytesField(nil, 3, inner))))},
		{name: "client finish first", payloads: first(appTestCat(header, whatsAppTestFrame(whatsAppTestBytesField(nil, 4, inner))))},
		{name: "33-byte ephemeral", payloads: first(appTestCat(header, whatsAppTestFrame(whatsAppTestBytesField(nil, 2, appTestCat(whatsAppTestBytesField(nil, 1, appTestBytes("wa-near-33", 33)), static, payload)))))},
		{name: "static before ephemeral", payloads: first(appTestCat(header, whatsAppTestFrame(whatsAppTestBytesField(nil, 2, appTestCat(static, ephemeral, payload)))))},
		{name: "frame length one more", payloads: first(appTestCat(header, withLength(len(hello)+1, appTestCat(hello, []byte{0}))))},
		{name: "frame length one less", payloads: first(appTestCat(header, withLength(len(hello)-1, hello)))},
		{name: "padded clientHello length", payloads: first(appTestCat(header, whatsAppTestFrame(paddedVarint), appTestBytes("wa-near-padded", 220)))},
		{name: "frame shorter than a hello", payloads: first(appTestCat(header, whatsAppTestFrame(shortKey), appTestBytes("wa-near-shortkey", 220)))},
		{name: "frame longer than the bound", payloads: first(appTestCat(header, withLength(whatsAppMaxHelloFrameLength+1, hello)))},
		// the prefix only starts a flow and must keep its structure
		{name: "header not first", payloads: [][]byte{appTestBytes("wa-near-lead", 300), appTestCat(header, frame), transports[0]}},
		{name: "segmented prefix that diverges", payloads: [][]byte{{'E', 'D', 0, 1}, whatsAppTestFrame(whatsAppTestRouting("wa-near-div")), appTestBytes("wa-near-div", 300)}},
		{name: "header alone then random", payloads: [][]byte{header, appTestBytes("wa-near-alone", 300)}},
	}
	for _, c := range cases {
		if 32 <= len(c.payloads[0]) {
			requireLooksEncrypted(t, c.name, c.payloads[0])
			if _, ok := whatsAppNoise(c.payloads[0]); ok {
				t.Fatalf("%s: first payload matched", c.name)
			}
		}
		requireDropWithoutAllow(t, c.name, IpProtocolTcp, whatsAppTestPort, c.payloads)
	}

	// other ports and udp never consult the detector
	for _, port := range []int{5223, 9000} {
		requireDropWithoutAllow(t, fmt.Sprintf("port %d", port), IpProtocolTcp, port, first(appTestCat(header, frame)))
	}
	requireDropWithoutAllow(t, "udp", IpProtocolUdp, whatsAppTestPort, first(appTestCat(header, frame)))
}

// A BitTorrent signature anywhere in an admitted WhatsApp flow, even right
// behind the key, is an incident.
func TestDmcaWhatsAppNoiseBittorrentPrecedence(t *testing.T) {
	web := whatsAppTestWebFlow("wa-bt")
	native := whatsAppTestNativeFlow("wa-bt-native")
	// a hello frame whose bytes right after the key are a peer wire handshake
	inner := appTestCat([]byte{whatsAppEphemeralTag, whatsAppEphemeralLength}, appTestBytes("wa-bt-key", 32), btHandshake())
	carrying := appTestCat(whatsAppTestHeader(6, 3), whatsAppTestFrame(whatsAppTestBytesField(nil, 2, inner)))
	if end, ok := whatsAppNoise(carrying); !ok || !hasBittorrentHandshake(carrying[end:]) {
		t.Fatal("the carrying hello must match with the handshake right after the key")
	}
	tracker := []byte("GET /announce?info_hash=%01%02&peer_id=x HTTP/1.1\r\nHost: tracker.example\r\n\r\n")
	cases := []struct {
		name     string
		payloads [][]byte
	}{
		{name: "web hello then handshake", payloads: [][]byte{web[0], btHandshake()}},
		{name: "hello carrying a handshake after the key", payloads: [][]byte{carrying}},
		{name: "native prefix then handshake", payloads: [][]byte{native[0], native[1], btHandshake()}},
		{name: "native flow then tracker", payloads: appTestCat2(native[0], append(append([][]byte{}, native[1:5]...), tracker)...)},
		{name: "header alone then handshake", payloads: [][]byte{whatsAppTestHeader(6, 3), btHandshake()}},
	}
	for _, c := range cases {
		verdicts := classifyAll(newAppTestDetector(nil), IpProtocolTcp, 48100, whatsAppTestPort, c.payloads...)
		if verdicts[len(verdicts)-1] != dmcaBittorrent {
			t.Errorf("%s: verdicts = %v, want bittorrent last", c.name, verdicts)
		}
	}
}

// The master switch, nil settings and the detector's own toggle each restore
// the drop; the other detectors and the messaging exception do not gate it.
func TestDmcaWhatsAppNoiseDisabledRestoreDrop(t *testing.T) {
	flows := map[string][][]byte{
		"web":    whatsAppTestWebFlow("wa-web"),
		"native": whatsAppTestNativeFlow("wa-toggle-native"),
	}
	for name, payloads := range flows {
		requireWhatsAppEncrypted(t, name, payloads)
		for configuration, configure := range map[string]func(*DmcaSecurityPolicySettings){
			"master switch": func(settings *DmcaSecurityPolicySettings) { settings.App.Enabled = false },
			"nil settings":  func(settings *DmcaSecurityPolicySettings) { settings.App = nil },
			"own toggle":    func(settings *DmcaSecurityPolicySettings) { settings.App.WhatsApp = false },
		} {
			verdicts := classifyAll(newAppTestDetector(configure), IpProtocolTcp, 48200, whatsAppTestPort, payloads...)
			if _, verdict := firstDecision(verdicts); verdict != dmcaDropEncrypted {
				t.Errorf("%s with %s: verdicts = %v, want drop", name, configuration, verdicts)
			}
		}
		// the other application standards and the messaging exception do not gate it
		verdicts := classifyAll(newAppTestDetector(func(settings *DmcaSecurityPolicySettings) {
			settings.App.WireGuard = false
			settings.App.OpenVpn = false
			settings.App.Rtmp = false
			settings.App.Levin = false
			settings.App.RakNet = false
			settings.App.EthereumDiscv4 = false
			settings.App.EthereumRlpx = false
			settings.Messaging = nil
		}), IpProtocolTcp, 48201, whatsAppTestPort, payloads...)
		if _, verdict := firstDecision(verdicts); verdict != dmcaAllow {
			t.Errorf("%s with the other detectors off: verdicts = %v, want allow", name, verdicts)
		}
	}
}

// The detector decides before the Meta prefix exception, so a recognized
// WhatsApp flow to Meta reports the whatsapp reason. The exception is the
// backstop: it admits what the detector does not recognize there, while the
// same near miss to any other address is dropped.
func TestDmcaWhatsAppNoiseBeforeMetaException(t *testing.T) {
	flow := func(address string, payloads ...[]byte) ([]dmcaVerdict, []SecurityPolicyReason) {
		detector := newAppTestDetector(nil)
		syn := whatsAppTestPath(address, IpProtocolTcp, whatsAppTestPort, true)
		detector.classify(syn, nil)
		data := *syn
		data.Syn = false
		verdicts := []dmcaVerdict{}
		reasons := []SecurityPolicyReason{}
		for _, payload := range payloads {
			verdict, reason, _ := detector.classifyForSenderDetailed(Id{}, &data, payload)
			verdicts = append(verdicts, verdict)
			reasons = append(reasons, reason)
		}
		return verdicts, reasons
	}
	meta := metaTestAddress(t, 4, 0)

	_, webReasons := flow(meta, whatsAppTestWebFlow("wa-meta-web")...)
	for i, reason := range webReasons {
		if reason != SecurityPolicyReasonAllowWhatsApp {
			t.Fatalf("web flow to Meta packet %d reason = %s, want allow-app-standard:whatsapp", i, reason)
		}
	}
	verdicts, reasons := flow(meta, whatsAppTestNativeFlow("wa-meta-native")...)
	for i := range verdicts {
		wantVerdict, wantReason := dmcaAllow, SecurityPolicyReasonAllowWhatsApp
		if i < 3 {
			wantVerdict, wantReason = dmcaInspecting, SecurityPolicyReasonInspecting
		}
		if verdicts[i] != wantVerdict || reasons[i] != wantReason {
			t.Fatalf("native flow to Meta packet %d = %d/%s, want %d/%s", i, verdicts[i], reasons[i], wantVerdict, wantReason)
		}
	}

	random := appTestRandomPackets("wa-meta-random", 4, 300)
	nearMiss := [][]byte{
		appTestCat(whatsAppTestHeader(16, 3), whatsAppTestFrame(whatsAppTestIkHello("wa-meta-near", 220))),
		whatsAppTestTransport("wa-meta-near-t0", 240),
		whatsAppTestTransport("wa-meta-near-t1", 280),
	}
	for name, payloads := range map[string][][]byte{"random": random, "near miss": nearMiss} {
		verdicts, reasons := flow(meta, payloads...)
		for i := range verdicts {
			if verdicts[i] != dmcaAllow || reasons[i] != SecurityPolicyReasonAllowMessaging {
				t.Fatalf("%s to Meta packet %d = %d/%s, want allow/allow-messaging", name, i, verdicts[i], reasons[i])
			}
		}
		verdicts, _ = flow(metaTestOutsideAddress, payloads...)
		if _, verdict := firstDecision(verdicts); verdict != dmcaDropEncrypted {
			t.Fatalf("%s outside Meta = %v, want drop", name, verdicts)
		}
	}
}

// The detector is consulted on the chat port and on 443, only for a flow's
// first payload, and never on other ports or over udp; on 443 the policy
// admits the flow as privileged before any detector runs.
func TestWhatsAppNoisePorts(t *testing.T) {
	app := newAppStandardDetector(DefaultAppStandardSettings())
	opening := whatsAppTestWebFlow("wa-ports")[0]
	edgeHeader := whatsAppTestNativeFlow("wa-ports-native")[0]
	for _, port := range []int{whatsAppChatPort, whatsAppHttpsPort} {
		path := appTestPath(IpProtocolTcp, 1, port, false)
		if reason, _, ok := app.match(path, opening, true); !ok || reason != SecurityPolicyReasonAllowWhatsApp {
			t.Fatalf("port %d: opening did not match", port)
		}
		if _, ok := app.open(path, edgeHeader, true); !ok {
			t.Fatalf("port %d: the edge header did not open a candidate", port)
		}
		// only a flow's first payload starts the prefix
		if _, _, ok := app.match(path, opening, false); ok {
			t.Fatalf("port %d: a later payload matched", port)
		}
		if _, ok := app.open(path, edgeHeader, false); ok {
			t.Fatalf("port %d: a later payload opened a candidate", port)
		}
	}
	for _, port := range []int{80, 4244, 5223, 9000} {
		path := appTestPath(IpProtocolTcp, 1, port, false)
		if _, _, ok := app.match(path, opening, true); ok {
			t.Fatalf("port %d: opening matched", port)
		}
		if _, ok := app.open(path, edgeHeader, true); ok {
			t.Fatalf("port %d: the edge header opened a candidate", port)
		}
	}
	udp := appTestPath(IpProtocolUdp, 1, whatsAppChatPort, false)
	if _, _, ok := app.match(udp, opening, true); ok {
		t.Fatal("udp opening matched")
	}

	// 443 is a privileged port: the policy admits it before any flow state
	// exists, so a WhatsApp flow there is allowed with or without the detector
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stats := DefaultSecurityPolicyStatsCollector()
	policy := DefaultSecurityPolicyWithStats(ctx, stats)
	policy.InspectEgress(protocol.ProvideMode_Public, appTestPath(IpProtocolTcp, 48400, whatsAppHttpsPort, true), nil)
	for i, payload := range whatsAppTestNativeFlow("wa-ports-443") {
		if r, err := policy.InspectEgress(protocol.ProvideMode_Public, appTestPath(IpProtocolTcp, 48400, whatsAppHttpsPort, false), payload); err != nil || r != SecurityPolicyResultAllow {
			t.Fatalf("443 packet %d = %v %v, want allow", i, r, err)
		}
	}
	destination := SecurityDestination{Version: 4, Protocol: IpProtocolTcp, Port: whatsAppHttpsPort}
	if count := stats.Reasons(false)[SecurityPolicyReasonAllowPrivileged][destination]; count == 0 {
		t.Fatalf("443 reasons = %v, want allow-privileged", stats.Reasons(false))
	}
}

// End to end through the policy a provider runs, to an address outside Meta's
// prefixes (which the exception never covered): both layouts pass every
// packet, a provider without the detector drops them, and a BitTorrent
// handshake on an admitted flow is an incident.
func TestSecurityPolicyWhatsAppNoiseProvider(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sender := NewId()
	run := func(policy SecurityPolicy, sourcePort int, payloads ...[]byte) []SecurityPolicyResult {
		inspectAndRefreshIngressForSenderBorrowed(policy, sender, protocol.ProvideMode_Public, *appTestPath(IpProtocolTcp, sourcePort, whatsAppTestPort, true), nil)
		results := []SecurityPolicyResult{}
		for _, payload := range payloads {
			r, err := inspectAndRefreshIngressForSenderBorrowed(policy, sender, protocol.ProvideMode_Public, *appTestPath(IpProtocolTcp, sourcePort, whatsAppTestPort, false), payload)
			if err != nil {
				t.Fatal(err)
			}
			results = append(results, r)
		}
		return results
	}
	legacyPolicy := func() SecurityPolicy {
		dmca := DefaultDmcaSecurityPolicySettings()
		dmca.App.WhatsApp = false
		return Reverse(NewSecurityPolicy(ctx, DefaultCfaaSecurityPolicySettings(), dmca, DefaultWebStandardSettings(), DefaultSecurityPolicyStatsCollector()))
	}
	flows := map[string][][]byte{
		"web":    whatsAppTestWebFlow("wa-web"),
		"native": whatsAppTestNativeFlow("wa-provider-native"),
	}
	for name, payloads := range flows {
		requireWhatsAppEncrypted(t, name, payloads)
		for i, result := range run(DefaultProviderSecurityPolicy(ctx), 48500, payloads...) {
			if result != SecurityPolicyResultAllow {
				t.Fatalf("%s: provider packet %d = %v, want allow", name, i, result)
			}
		}
		if results := run(legacyPolicy(), 48501, payloads...); results[len(results)-1] != SecurityPolicyResultDrop {
			t.Fatalf("%s: provider without the detector = %v, want drop last", name, results)
		}
		results := run(DefaultProviderSecurityPolicy(ctx), 48502, append(append([][]byte{}, payloads...), btHandshake())...)
		if last := results[len(results)-1]; last != SecurityPolicyResultIncident {
			t.Fatalf("%s: BitTorrent after the flow = %v, want incident", name, last)
		}
	}
}

// The prefix may arrive in any number of segments: every split matches where
// the whole opening does, and so does one byte at a time. The verdict never
// depends on the segment boundaries, including for bytes that follow the
// hello frame.
func TestWhatsAppNoiseStreamSplits(t *testing.T) {
	openings := map[string][]byte{
		"web":                   appTestCat(whatsAppTestHeader(6, 3), whatsAppTestFrame(whatsAppTestXxHello("wa-splits-web"))),
		"edge ik":               appTestCat(whatsAppTestEdge(whatsAppTestRouting("wa-splits")), whatsAppTestHeader(4, 0), whatsAppTestFrame(whatsAppTestIkHello("wa-splits", 60))),
		"empty routing info":    appTestCat(whatsAppTestEdge(nil), whatsAppTestHeader(6, 3), whatsAppTestFrame(whatsAppTestXxHello("wa-splits-empty"))),
		"two-byte hello length": appTestCat(whatsAppTestHeader(6, 3), whatsAppTestFrame(whatsAppTestIkHello("wa-splits-long", 300))),
		"bytes past the frame":  appTestCat(whatsAppTestHeader(4, 0), whatsAppTestFrame(whatsAppTestXxHello("wa-splits-past")), appTestBytes("wa-splits-past", 120)),
	}
	for name, opening := range openings {
		keyEnd, ok := whatsAppNoise(opening)
		if !ok {
			t.Fatalf("%s: opening did not match", name)
		}
		consume := func(segments ...[]byte) (whatsAppProgress, int) {
			var stream whatsAppStream
			offset := 0
			for i, segment := range segments {
				progress, end := stream.consume(segment)
				if progress != whatsAppNeedMore || i == len(segments)-1 {
					return progress, offset + end
				}
				offset += len(segment)
			}
			return whatsAppNeedMore, offset
		}
		for split := 1; split < len(opening); split += 1 {
			if progress, end := consume(opening[:split], opening[split:]); progress != whatsAppMatched || end != keyEnd {
				t.Fatalf("%s split at %d: %d at %d, want matched at %d", name, split, progress, end, keyEnd)
			}
		}
		for first := 1; first < keyEnd; first += 1 {
			for second := first + 1; second < keyEnd; second += 1 {
				if progress, end := consume(opening[:first], opening[first:second], opening[second:]); progress != whatsAppMatched || end != keyEnd {
					t.Fatalf("%s split at %d and %d: %d at %d, want matched at %d", name, first, second, progress, end, keyEnd)
				}
			}
		}
		var stream whatsAppStream
		for i := 0; i < keyEnd; i += 1 {
			progress, end := stream.consume(opening[i : i+1])
			if i < keyEnd-1 && progress != whatsAppNeedMore || i == keyEnd-1 && (progress != whatsAppMatched || end != 1) {
				t.Fatalf("%s byte %d: %d at %d", name, i, progress, end)
			}
		}
	}
}

// A pending prefix is not counted as encrypted, like a two-packet opener, but
// only while its bytes keep the structure, and the routing info is bounded: a
// flow that leaves the structure is judged as without the detector.
func TestDmcaWhatsAppPendingPrefixIsBounded(t *testing.T) {
	routing := appTestBytes("wa-pending-routing", 1000)
	payloads := [][]byte{
		appTestCat([]byte{'E', 'D', 0, 1, 0, 0x03, 0xe8}, routing[:393]),
		routing[393:793],
		routing[793:],
		appTestBytes("wa-pending-0", 300),
		appTestBytes("wa-pending-1", 300),
		appTestBytes("wa-pending-2", 300),
	}
	verdicts := classifyAll(newAppTestDetector(nil), IpProtocolTcp, 48600, whatsAppTestPort, payloads...)
	if index, verdict := firstDecision(verdicts); index != 5 || verdict != dmcaDropEncrypted {
		t.Fatalf("verdicts = %v, want inspecting through the routing info, then drop at the third encrypted payload", verdicts)
	}
	tooLong := appTestCat([]byte{'E', 'D', 0, 1, 0, 0x04, 0x01}, appTestBytes("wa-pending-long", 393))
	requireDropWithoutAllow(t, "routing info past the bound", IpProtocolTcp, whatsAppTestPort, [][]byte{tooLong})
}

// Seeded random payloads, alone and behind a valid header, frame length and
// tag, never match.
func TestWhatsAppNoiseRejectsRandomPayloads(t *testing.T) {
	app := newAppStandardDetector(DefaultAppStandardSettings())
	path := appTestPath(IpProtocolTcp, 1, whatsAppTestPort, false)
	random := rand.New(rand.NewChaCha8([32]byte{'w', 'a'}))
	fill := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(random.Uint32())
		}
		return b
	}
	matched := func(b []byte) bool {
		reason, _, ok := app.match(path, b, true)
		return ok && reason == SecurityPolicyReasonAllowWhatsApp
	}
	for i := 0; i < 50000; i += 1 {
		if b := fill(1 + i%1500); matched(b) {
			t.Fatalf("random payload %d (%d bytes) matched", i, len(b))
		}
	}
	// random bytes behind a valid header, then also behind a frame length
	// that fits the payload and the clientHello tag
	for i := 0; i < 20000; i += 1 {
		b := fill(64 + i%1400)
		copy(b, whatsAppTestHeader(6, 3))
		if matched(b) {
			t.Fatalf("random payload %d behind a header matched", i)
		}
		frameLength := len(b) - 7
		copy(b[4:], []byte{byte(frameLength >> 16), byte(frameLength >> 8), byte(frameLength), whatsAppClientHelloTag})
		if matched(b) {
			t.Fatalf("random payload %d behind a header, frame length and tag matched", i)
		}
	}
}

// Matching an opening and carrying a prefix across segments do not allocate.
func TestWhatsAppNoiseDetectorZeroAlloc(t *testing.T) {
	app := newAppStandardDetector(DefaultAppStandardSettings())
	path := appTestPath(IpProtocolTcp, 1, whatsAppTestPort, false)
	opening := whatsAppTestWebFlow("wa-alloc")[0]
	native := whatsAppTestNativeFlow("wa-alloc-native")
	if allocations := testing.AllocsPerRun(1000, func() {
		if _, _, ok := app.match(path, opening, true); !ok {
			t.Fatal("opening did not match")
		}
		candidate, ok := app.open(path, native[0], true)
		if !ok {
			t.Fatal("the edge header did not open a candidate")
		}
		for _, segment := range native[1:3] {
			if reason, ok := app.confirm(&candidate, path, segment); ok || reason != SecurityPolicyReasonInspecting {
				t.Fatal("the prefix did not stay pending")
			}
		}
		if reason, ok := app.confirm(&candidate, path, native[3]); !ok || reason != SecurityPolicyReasonAllowWhatsApp {
			t.Fatal("the hello did not confirm")
		}
	}); allocations != 0 {
		t.Fatalf("whatsapp detector allocates %.2f objects per call, want 0", allocations)
	}
}

// Cross-checks every match against protowire's parser, and checks that cutting
// the bytes into two segments never changes the outcome.
func FuzzWhatsAppNoiseDetector(f *testing.F) {
	for _, payloads := range [][][]byte{whatsAppTestWebFlow("fuzz-wa"), whatsAppTestNativeFlow("fuzz-wa")} {
		for _, payload := range payloads {
			f.Add(payload, uint16(7))
		}
	}
	f.Add(appTestCat(whatsAppTestEdge(whatsAppTestRouting("fuzz-wa")), whatsAppTestHeader(4, 0), whatsAppTestFrame(whatsAppTestIkHello("fuzz-wa", 60))), uint16(20))
	f.Fuzz(func(t *testing.T, payload []byte, cut uint16) {
		keyEnd, ok := whatsAppNoise(payload)
		if ok {
			offset := 0
			if bytes.HasPrefix(payload, whatsAppEdgeHeader) {
				offset = 7 + (int(payload[4])<<16 | int(payload[5])<<8 | int(payload[6]))
			}
			header := payload[offset : offset+4]
			if header[0] != 'W' || header[1] != 'A' || 16 <= header[2] || 16 <= header[3] {
				t.Fatalf("matched header %x", header)
			}
			frameLength := int(payload[offset+4])<<16 | int(payload[offset+5])<<8 | int(payload[offset+6])
			frame := payload[offset+7:]
			number, wireType, n := protowire.ConsumeTag(frame)
			if n < 0 || number != 2 || wireType != protowire.BytesType {
				t.Fatal("matched without a clientHello")
			}
			helloLength, m := protowire.ConsumeVarint(frame[n:])
			if m < 0 || n+m+int(helloLength) != frameLength {
				t.Fatal("matched with a frame length that disagrees with the clientHello")
			}
			hello := frame[n+m:]
			number, wireType, k := protowire.ConsumeTag(hello)
			if k < 0 || number != 1 || wireType != protowire.BytesType {
				t.Fatal("matched without the ephemeral key first")
			}
			key, l := protowire.ConsumeBytes(hello[k:])
			if l < 0 || len(key) != whatsAppEphemeralLength || keyEnd != offset+7+n+m+k+l {
				t.Fatal("matched without a complete 32-byte ephemeral key")
			}
		}
		if 0 < len(payload) {
			split := int(cut) % len(payload)
			var stream whatsAppStream
			progress, end := stream.consume(payload[:split])
			if progress == whatsAppNeedMore {
				progress, end = stream.consume(payload[split:])
				end += split
			}
			if (progress == whatsAppMatched) != ok || ok && end != keyEnd {
				t.Fatalf("two segments cut at %d: %d at %d, the whole: %t at %d", split, progress, end, ok, keyEnd)
			}
		}
		detector := newAppTestDetector(nil)
		path := appTestPath(IpProtocolTcp, 1, whatsAppTestPort, false)
		detector.classify(path, payload)
		detector.classify(path, payload)
	})
}
