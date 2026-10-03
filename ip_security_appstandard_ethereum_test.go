// Ethereum devp2p detectors (discovery v4, RLPx auth). Positive flows must
// first prove they exercise the encrypted heuristic; every near miss, every
// encrypted BitTorrent variant and random payloads must stay dropped; the
// BitTorrent signatures keep precedence. All bytes are deterministic.
package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"golang.org/x/crypto/sha3"

	"github.com/urnetwork/connect/protocol"
)

const ethTestPort = 30303

func ethTestKeccak(b []byte) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(b)
	return h.Sum(nil)
}

func ethTestRlpHeader(offset byte, length int) []byte {
	if length <= 55 {
		return []byte{offset + byte(length)}
	}
	if length <= 0xff {
		return []byte{offset + 56, byte(length)}
	}
	return []byte{offset + 57, byte(length >> 8), byte(length)}
}

func ethTestRlpString(b []byte) []byte {
	return appTestCat(ethTestRlpHeader(0x80, len(b)), b)
}

func ethTestRlpList(items ...[]byte) []byte {
	payload := appTestCat(items...)
	return appTestCat(ethTestRlpHeader(0xc0, len(payload)), payload)
}

// ethTestDiscv4 is hash || signature || packet-type || packet-data with
// hash = keccak256(signature || packet-type || packet-data). The detector does
// not verify the signature, so it is random here.
func ethTestDiscv4(seed string, packetType byte, data []byte) []byte {
	body := appTestCat(appTestBytes(seed+"-sig", 65), []byte{packetType}, data)
	return appTestCat(ethTestKeccak(body), body)
}

// ethTestDiscv4Data is a findnode-like list: [target (64), expiration].
func ethTestDiscv4Data(seed string) []byte {
	return ethTestRlpList(ethTestRlpString(appTestBytes(seed, 64)), ethTestRlpString([]byte{0x6a, 0xb0, 0x00, 0x00}))
}

func ethTestPoint(seed string) []byte {
	return secp256k1.PrivKeyFromBytes(appTestBytes(seed, 32)).PubKey().SerializeUncompressed()
}

// ethTestRlpxEip8 is auth-size || R || iv || c || d with an auth body of
// bodyLength bytes.
func ethTestRlpxEip8(seed string, bodyLength int) []byte {
	size := make([]byte, 2)
	binary.BigEndian.PutUint16(size, uint16(65+16+bodyLength+32))
	return appTestCat(size, ethTestPoint(seed), appTestBytes(seed+"-c", 16+bodyLength+32))
}

// ethTestRlpxPreEip8 is the 307-byte R || iv || c (194) || d.
func ethTestRlpxPreEip8(seed string) []byte {
	return appTestCat(ethTestPoint(seed), appTestBytes(seed+"-c", 16+194+32))
}

func ethTestFrames(seed string, sizes ...int) [][]byte {
	frames := [][]byte{}
	for i, size := range sizes {
		frames = append(frames, appTestBytes(fmt.Sprintf("%s-%d", seed, i), 16+16+(size+15)/16*16+16))
	}
	return frames
}

func ethTestDiscv4Flow(seed string) [][]byte {
	return [][]byte{
		ethTestDiscv4(seed+"-ping", 0x01, ethTestDiscv4Data(seed+"-ping")),
		ethTestDiscv4(seed+"-pong", 0x02, ethTestDiscv4Data(seed+"-pong")),
		ethTestDiscv4(seed+"-enr", 0x05, ethTestDiscv4Data(seed+"-enr")),
		ethTestDiscv4(seed+"-find", 0x03, ethTestDiscv4Data(seed+"-find")),
	}
}

func ethTestRlpxFlow(seed string, eip8 bool) [][]byte {
	auth := ethTestRlpxPreEip8(seed)
	if eip8 {
		auth = ethTestRlpxEip8(seed, 169+150)
	}
	return append([][]byte{auth}, ethTestFrames(seed+"-frame", 140, 100, 3)...)
}

func requireEthereumAllowed(t *testing.T, name string, transport IpProtocol, payloads [][]byte, reason SecurityPolicyReason) {
	t.Helper()
	for _, payload := range payloads {
		requireLooksEncrypted(t, name, payload)
	}
	// the policy before this change drops the flow
	legacy := classifyAll(newAppTestDetector(func(settings *DmcaSecurityPolicySettings) {
		settings.App.EthereumDiscv4 = false
		settings.App.EthereumRlpx = false
	}), transport, 46000, ethTestPort, payloads...)
	if _, verdict := firstDecision(legacy); verdict != dmcaDropEncrypted {
		t.Fatalf("%s: without the ethereum detectors verdicts = %v, want drop", name, legacy)
	}

	detector := newAppTestDetector(nil)
	verdicts := classifyAll(detector, transport, 46001, ethTestPort, payloads...)
	for i, verdict := range verdicts {
		if verdict != dmcaAllow {
			t.Fatalf("%s: packet %d verdict = %v (all %v), want allow", name, i, verdict, verdicts)
		}
	}
	state := appTestFlowState(t, detector, appTestPath(transport, 46001, ethTestPort, false))
	state.mu.Lock()
	appReason := state.appReason
	state.mu.Unlock()
	if appReason != reason {
		t.Fatalf("%s: reason = %s, want %s", name, appReason, reason)
	}
}

func TestDmcaEthereumDiscv4Allowed(t *testing.T) {
	requireEthereumAllowed(t, "discv4", IpProtocolUdp, ethTestDiscv4Flow("discv4"), SecurityPolicyReasonAllowEthereumDiscv4)

	// every packet type, on its own flow
	for packetType := byte(1); packetType <= 6; packetType += 1 {
		packet := ethTestDiscv4(fmt.Sprintf("discv4-type-%d", packetType), packetType, ethTestDiscv4Data("discv4-type"))
		if verdict := newAppTestDetector(nil).classify(appTestPath(IpProtocolUdp, 46010, ethTestPort, false), packet); verdict != dmcaAllow {
			t.Fatalf("type %d verdict = %d, want allow", packetType, verdict)
		}
	}

	// a neighbors-sized packet: the hash covers bytes past MaxInspectionPayload
	neighbors := ethTestDiscv4("discv4-neighbors", 0x04, ethTestRlpList(ethTestRlpString(appTestBytes("discv4-neighbors-data", 1100))))
	if len(neighbors) <= DefaultDmcaSecurityPolicySettings().MaxInspectionPayload || ethereumDiscv4MaxPacketSize < len(neighbors) {
		t.Fatalf("neighbors length %d", len(neighbors))
	}
	if verdict := newAppTestDetector(nil).classify(appTestPath(IpProtocolUdp, 46011, ethTestPort, false), neighbors); verdict != dmcaAllow {
		t.Fatalf("neighbors verdict = %d, want allow", verdict)
	}
}

// Discovery v5 shares the socket with v4 and cannot be recognized; a v4 packet
// after unidentified datagrams still admits the flow while it is inspecting.
func TestDmcaEthereumDiscv4AfterUnidentifiedDatagrams(t *testing.T) {
	for unidentified := 0; unidentified < DefaultDmcaSecurityPolicySettings().EncryptedDecisionPackets; unidentified += 1 {
		payloads := appTestRandomPackets(fmt.Sprintf("discv5-%d", unidentified), unidentified, 120)
		payloads = append(payloads, ethTestDiscv4("discv4-late", 0x01, ethTestDiscv4Data("discv4-late")))
		verdicts := classifyAll(newAppTestDetector(nil), IpProtocolUdp, 46020+unidentified, ethTestPort, payloads...)
		if index, verdict := firstDecision(verdicts); index != unidentified || verdict != dmcaAllow {
			t.Fatalf("%d unidentified datagrams: verdicts = %v, want allow at %d", unidentified, verdicts, unidentified)
		}
	}
	// after the heuristic decided, the drop is terminal
	payloads := appTestRandomPackets("discv5-decided", 3, 120)
	payloads = append(payloads, ethTestDiscv4("discv4-too-late", 0x01, ethTestDiscv4Data("discv4-too-late")))
	verdicts := classifyAll(newAppTestDetector(nil), IpProtocolUdp, 46030, ethTestPort, payloads...)
	if verdicts[3] != dmcaDropEncrypted {
		t.Fatalf("v4 after the drop: verdicts = %v, want drop", verdicts)
	}
}

func ethTestDiscv4NearMisses() map[string][]byte {
	flip := func(b []byte, i int) []byte {
		b = append([]byte{}, b...)
		b[i] ^= 0x01
		return b
	}
	valid := ethTestDiscv4("near-valid", 0x01, ethTestDiscv4Data("near-valid"))
	oversize := ethTestDiscv4("near-oversize", 0x04, ethTestRlpList(ethTestRlpString(appTestBytes("near-oversize", 1200))))
	nonCanonical := ethTestDiscv4("near-noncanonical", 0x01, appTestCat([]byte{0xf8, 0x30}, appTestBytes("near-noncanonical", 0x30)))
	overrun := ethTestDiscv4("near-overrun", 0x01, appTestCat([]byte{0xf8, 0x60}, appTestBytes("near-overrun", 0x50)))
	return map[string][]byte{
		"hash flipped":       flip(valid, 5),
		"signature flipped":  flip(valid, 40),
		"data flipped":       flip(valid, len(valid)-1),
		"type 0":             ethTestDiscv4("near-type0", 0x00, ethTestDiscv4Data("near-type0")),
		"type 7":             ethTestDiscv4("near-type7", 0x07, ethTestDiscv4Data("near-type7")),
		"data not a list":    ethTestDiscv4("near-string", 0x01, ethTestRlpString(appTestBytes("near-string", 70))),
		"list overruns":      overrun,
		"non-canonical list": nonCanonical,
		"oversize":           oversize,
		"truncated":          valid[:len(valid)-1],
	}
}

func TestDmcaEthereumDiscv4NearMissesDrop(t *testing.T) {
	for name, packet := range ethTestDiscv4NearMisses() {
		requireLooksEncrypted(t, name, packet)
		if _, _, ok := newAppStandardDetector(DefaultAppStandardSettings()).match(appTestPath(IpProtocolUdp, 1, ethTestPort, false), packet, true); ok {
			t.Fatalf("%s: matched", name)
		}
		requireDropWithoutAllow(t, "discv4 "+name, IpProtocolUdp, ethTestPort, [][]byte{packet, packet, packet})
	}
	// a valid discv4 packet over tcp is not discovery
	requireDropWithoutAllow(t, "discv4 over tcp", IpProtocolTcp, ethTestPort, ethTestDiscv4Flow("discv4-tcp"))
}

func TestDmcaEthereumRlpxAuthAllowed(t *testing.T) {
	requireEthereumAllowed(t, "rlpx eip-8", IpProtocolTcp, ethTestRlpxFlow("rlpx-eip8", true), SecurityPolicyReasonAllowEthereumRlpx)
	requireEthereumAllowed(t, "rlpx pre-eip-8", IpProtocolTcp, ethTestRlpxFlow("rlpx-pre", false), SecurityPolicyReasonAllowEthereumRlpx)
	// the smallest and a large EIP-8 auth
	for _, bodyLength := range []int{169, 169 + 100, 169 + 300, 1200} {
		auth := ethTestRlpxEip8(fmt.Sprintf("rlpx-size-%d", bodyLength), bodyLength)
		if verdict := classifyAll(newAppTestDetector(nil), IpProtocolTcp, 46040, ethTestPort, auth)[0]; verdict != dmcaAllow {
			t.Fatalf("body %d: verdict = %d, want allow", bodyLength, verdict)
		}
	}
}

func ethTestRlpxNearMisses() map[string][][]byte {
	frames := ethTestFrames("rlpx-near-frame", 140, 100)
	with := func(first ...[]byte) [][]byte {
		return append(append([][]byte{}, first...), frames...)
	}
	offCurve := ethTestRlpxEip8("rlpx-near-off", 169+150)
	copy(offCurve[2+33:2+65], appTestBytes("rlpx-near-off-y", 32))
	xTooBig := ethTestRlpxEip8("rlpx-near-x", 169+150)
	copy(xTooBig[3:35], bytes.Repeat([]byte{0xff}, 32))
	compressed := ethTestRlpxEip8("rlpx-near-format", 169+150)
	compressed[2] = 0x02
	hybrid := ethTestRlpxEip8("rlpx-near-hybrid", 169+150)
	hybrid[2] = 0x06
	sizeLow := ethTestRlpxEip8("rlpx-near-size-low", 169+150)
	binary.BigEndian.PutUint16(sizeLow[0:2], uint16(len(sizeLow)-3))
	sizeHigh := ethTestRlpxEip8("rlpx-near-size-high", 169+150)
	binary.BigEndian.PutUint16(sizeHigh[0:2], uint16(len(sizeHigh)-1))
	preOffCurve := ethTestRlpxPreEip8("rlpx-near-pre-off")
	copy(preOffCurve[33:65], appTestBytes("rlpx-near-pre-off-y", 32))
	pre := ethTestRlpxPreEip8("rlpx-near-pre")
	split := ethTestRlpxEip8("rlpx-near-split", 169+150)
	return map[string][][]byte{
		"off curve":           with(offCurve),
		"x >= p":              with(xTooBig),
		"compressed format":   with(compressed),
		"hybrid format":       with(hybrid),
		"size prefix short":   with(sizeLow),
		"size prefix long":    with(sizeHigh),
		"auth body too small": with(ethTestRlpxEip8("rlpx-near-small", 168)),
		"pre-eip-8 off curve": with(preOffCurve),
		"pre-eip-8 306 bytes": with(pre[:306]),
		"pre-eip-8 308 bytes": with(appTestCat(pre, []byte{0x5a})),
		"auth not first":      with(appTestBytes("rlpx-near-first", 200), ethTestRlpxEip8("rlpx-near-second", 169+150)),
		"auth split segments": {split[:200], split[200:], frames[0], frames[1]},
		"two auths coalesced": with(appTestCat(ethTestRlpxEip8("rlpx-near-c1", 169+150), ethTestRlpxEip8("rlpx-near-c2", 169+150))),
	}
}

func TestDmcaEthereumRlpxNearMissesDrop(t *testing.T) {
	for name, payloads := range ethTestRlpxNearMisses() {
		requireLooksEncrypted(t, name, payloads[0])
		requireDropWithoutAllow(t, "rlpx "+name, IpProtocolTcp, ethTestPort, payloads)
	}
	// an RLPx auth over udp is not RLPx
	requireDropWithoutAllow(t, "rlpx over udp", IpProtocolUdp, ethTestPort, ethTestRlpxFlow("rlpx-udp", true))
}

// TestDmcaEthereumBittorrentStillDropped runs every BitTorrent variant with the
// Ethereum detectors on, on the Ethereum port and on a random peer port.
func TestDmcaEthereumBittorrentStillDropped(t *testing.T) {
	for _, port := range []int{ethTestPort, 50321} {
		// MSE/PE: Ya (96) + PadA (0-512); 211 makes the pre-EIP-8 auth length
		for _, padding := range []int{0, 52, 148, 211, 300, 512} {
			first := mseFirstMessage(fmt.Sprintf("eth-mse-%d", padding), padding)
			payloads := [][]byte{first, appTestBytes("eth-mse-2", 120), appTestBytes("eth-mse-3", 300)}
			for _, transport := range []IpProtocol{IpProtocolTcp, IpProtocolUdp} {
				verdicts := classifyAll(newAppTestDetector(nil), transport, 46100+padding, port, payloads...)
				if index, verdict := firstDecision(verdicts); index != 2 || verdict != dmcaDropEncrypted {
					t.Fatalf("port %d mse pad %d %v: verdicts %v, want drop at the third packet", port, padding, transport, verdicts)
				}
			}
		}

		// MSE blobs that pass every structural Ethereum check except the
		// cryptographic one: an EIP-8 size prefix with the 0x04 format byte, a
		// 307-byte blob starting 0x04, and a discv4-shaped datagram
		eip8Shaped := mseFirstMessage("eth-mse-eip8-shaped", 300)
		binary.BigEndian.PutUint16(eip8Shaped[0:2], uint16(len(eip8Shaped)-2))
		eip8Shaped[2] = secp256k1UncompressedFormat
		preShaped := mseFirstMessage("eth-mse-pre-shaped", 211)
		preShaped[0] = secp256k1UncompressedFormat
		discv4Shaped := mseFirstMessage("eth-mse-discv4-shaped", 148)
		discv4Shaped[97] = 0x01
		discv4Shaped[98] = 0xc0
		for name, first := range map[string][]byte{"eip-8 shaped": eip8Shaped, "pre-eip-8 shaped": preShaped, "discv4 shaped": discv4Shaped} {
			payloads := [][]byte{first, appTestBytes("eth-shaped-2", 120), appTestBytes("eth-shaped-3", 300)}
			for _, transport := range []IpProtocol{IpProtocolTcp, IpProtocolUdp} {
				verdicts := classifyAll(newAppTestDetector(nil), transport, 46700, port, payloads...)
				if index, verdict := firstDecision(verdicts); index != 2 || verdict != dmcaDropEncrypted {
					t.Fatalf("port %d %s %v: verdicts %v, want drop at the third packet", port, name, transport, verdicts)
				}
			}
		}

		// the 148-byte datagram that looks like a wireguard initiation leaks one
		// packet, as before
		prefixed := appTestCat([]byte{1, 0, 0, 0}, appTestBytes("eth-mse-prefixed", 144))
		payloads := [][]byte{prefixed, appTestBytes("eth-mse-p2", 120), appTestBytes("eth-mse-p3", 300), appTestBytes("eth-mse-p4", 200)}
		verdicts := classifyAll(newAppTestDetector(nil), IpProtocolUdp, 46800, port, payloads...)
		if index, verdict := firstDecision(verdicts); index != 3 || verdict != dmcaDropEncrypted {
			t.Fatalf("port %d prefixed mse: verdicts %v, want drop at the fourth packet", port, verdicts)
		}

		// encrypted uTP
		utpHeader := func(packetType byte, sequence uint16) []byte {
			header := make([]byte, 20)
			header[0] = packetType<<4 | 1
			copy(header[2:12], appTestBytes(fmt.Sprintf("eth-utp-%d", sequence), 10))
			binary.BigEndian.PutUint16(header[16:18], sequence)
			return header
		}
		utp := [][]byte{
			utpHeader(4, 1),
			appTestCat(utpHeader(0, 2), appTestBytes("eth-utp-d2", 400)),
			appTestCat(utpHeader(0, 3), appTestBytes("eth-utp-d3", 400)),
			appTestCat(utpHeader(0, 4), appTestBytes("eth-utp-d4", 400)),
		}
		verdicts = classifyAll(newAppTestDetector(nil), IpProtocolUdp, 46900, port, utp...)
		if index, verdict := firstDecision(verdicts); index != 3 || verdict != dmcaDropEncrypted {
			t.Fatalf("port %d encrypted utp: verdicts %v, want drop", port, verdicts)
		}

		// plaintext signatures
		utpHandshake := appTestCat(utpHeader(0, 5), btHandshake())
		udpTracker := appTestCat(udpTrackerConnectMagic, make([]byte, 4), appTestBytes("eth-tracker", 4))
		for name, c := range map[string]struct {
			transport IpProtocol
			payload   []byte
		}{
			"peer wire handshake": {IpProtocolTcp, btHandshake()},
			"http tracker":        {IpProtocolTcp, []byte("GET /announce?info_hash=%01%02&peer_id=x HTTP/1.1\r\n\r\n")},
			"dht":                 {IpProtocolUdp, appTestDhtPing()},
			"udp tracker":         {IpProtocolUdp, udpTracker},
			"utp handshake":       {IpProtocolUdp, utpHandshake},
		} {
			if verdict := classifyAll(newAppTestDetector(nil), c.transport, 47000, port, c.payload)[0]; verdict != dmcaBittorrent {
				t.Fatalf("port %d %s: verdict %d, want bittorrent", port, name, verdict)
			}
		}
	}

	// bittorrent on privileged ports, end to end
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, c := range []struct {
		transport IpProtocol
		port      int
		payload   []byte
	}{
		{IpProtocolTcp, 443, btHandshake()},
		{IpProtocolTcp, 80, btHandshake()},
		{IpProtocolUdp, 443, appTestDhtPing()},
	} {
		policy := DefaultSecurityPolicy(ctx)
		if r, _ := policy.InspectEgress(protocol.ProvideMode_Public, appTestPath(c.transport, 47100, c.port, false), c.payload); r != SecurityPolicyResultIncident {
			t.Fatalf("%v/%d: result %v, want incident", c.transport, c.port, r)
		}
	}
}

func TestDmcaBittorrentPrecedenceOverEthereum(t *testing.T) {
	discv4 := ethTestDiscv4("bt-discv4", 0x01, ethTestDiscv4Data("bt-discv4"))
	// a discv4 packet whose bytes after the RLP list carry a dht query
	trailing := ethTestDiscv4("bt-discv4-trailing", 0x01, appTestCat(ethTestRlpList(ethTestRlpString([]byte{4})), appTestDhtPing()))
	if _, _, ok := newAppStandardDetector(DefaultAppStandardSettings()).match(appTestPath(IpProtocolUdp, 1, ethTestPort, false), trailing, true); !ok {
		t.Fatal("trailing fixture must be a valid discv4 packet")
	}
	// the same past MaxInspectionPayload
	trailingLong := ethTestDiscv4("bt-discv4-trailing-long", 0x04, appTestCat(ethTestRlpList(ethTestRlpString(appTestBytes("bt-discv4-long", 600))), appTestDhtPing()))
	auth := ethTestRlpxEip8("bt-rlpx", 169+150)
	// an auth whose bytes after the ephemeral key are a peer wire handshake
	carrying := ethTestRlpxEip8("bt-rlpx-carrying", 169+150)
	copy(carrying[67:], btHandshake())
	cases := []struct {
		name      string
		transport IpProtocol
		payloads  [][]byte
	}{
		{"discv4 then dht", IpProtocolUdp, [][]byte{discv4, appTestDhtPing()}},
		{"discv4 then utp handshake", IpProtocolUdp, [][]byte{discv4, appTestCat([]byte{0x01, 0}, make([]byte, 18), btHandshake())}},
		{"discv4 carrying dht after the list", IpProtocolUdp, [][]byte{trailing}},
		{"long discv4 carrying dht after the list", IpProtocolUdp, [][]byte{trailingLong}},
		{"rlpx then peer wire handshake", IpProtocolTcp, [][]byte{auth, btHandshake()}},
		{"rlpx then tracker", IpProtocolTcp, [][]byte{auth, appTestBytes("bt-rlpx-frame", 64), []byte("GET /scrape?info_hash=%01 HTTP/1.1\r\n\r\n")}},
		{"rlpx carrying a handshake after the key", IpProtocolTcp, [][]byte{carrying}},
	}
	for _, c := range cases {
		verdicts := classifyAll(newAppTestDetector(nil), c.transport, 47200, ethTestPort, c.payloads...)
		if verdicts[len(verdicts)-1] != dmcaBittorrent {
			t.Errorf("%s: verdicts = %v, want bittorrent last", c.name, verdicts)
		}
	}
}

// TestEthereumDetectorsRejectRandomPayloads: random payloads never match, and
// neither do random payloads forced through every structural pre-check, so the
// cryptographic invariant (keccak, curve) carries each decision.
func TestEthereumDetectorsRejectRandomPayloads(t *testing.T) {
	app := newAppStandardDetector(DefaultAppStandardSettings())
	tcp := appTestPath(IpProtocolTcp, 1, ethTestPort, false)
	udp := appTestPath(IpProtocolUdp, 1, ethTestPort, false)
	random := rand.New(rand.NewChaCha8([32]byte{'e', 't', 'h'}))
	fill := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(random.Uint32())
		}
		return b
	}
	matched := func(path *IpPath, b []byte) bool {
		reason, _, ok := app.match(path, b, true)
		return ok && (reason == SecurityPolicyReasonAllowEthereumDiscv4 || reason == SecurityPolicyReasonAllowEthereumRlpx)
	}
	for i := 0; i < 50000; i += 1 {
		b := fill(1 + i%1500)
		if matched(tcp, b) || matched(udp, b) {
			t.Fatalf("random payload %d (%d bytes) matched", i, len(b))
		}
	}
	for i := 0; i < 5000; i += 1 {
		eip8 := fill(284 + i%1000)
		binary.BigEndian.PutUint16(eip8[0:2], uint16(len(eip8)-2))
		eip8[2] = secp256k1UncompressedFormat
		pre := fill(ethereumRlpxPreEip8AuthLength)
		pre[0] = secp256k1UncompressedFormat
		discv4 := fill(99 + i%1182)
		discv4[97] = byte(1 + i%6)
		discv4[98] = 0xc0
		if matched(tcp, eip8) || matched(tcp, pre) || matched(udp, discv4) {
			t.Fatalf("structured random payload %d matched", i)
		}
	}
}

func TestDmcaEthereumDisabledRestoreDrop(t *testing.T) {
	flows := map[string]struct {
		transport IpProtocol
		payloads  [][]byte
	}{
		"discv4": {IpProtocolUdp, ethTestDiscv4Flow("toggle-discv4")},
		"rlpx":   {IpProtocolTcp, ethTestRlpxFlow("toggle-rlpx", true)},
	}
	toggles := map[string]func(*AppStandardSettings){
		"discv4": func(settings *AppStandardSettings) { settings.EthereumDiscv4 = false },
		"rlpx":   func(settings *AppStandardSettings) { settings.EthereumRlpx = false },
	}
	for name, flow := range flows {
		for configuration, configure := range map[string]func(*DmcaSecurityPolicySettings){
			"master switch": func(settings *DmcaSecurityPolicySettings) { settings.App.Enabled = false },
			"nil settings":  func(settings *DmcaSecurityPolicySettings) { settings.App = nil },
			"own toggle":    func(settings *DmcaSecurityPolicySettings) { toggles[name](settings.App) },
		} {
			verdicts := classifyAll(newAppTestDetector(configure), flow.transport, 47300, ethTestPort, flow.payloads...)
			if _, verdict := firstDecision(verdicts); verdict != dmcaDropEncrypted {
				t.Errorf("%s with %s: verdicts = %v, want drop", name, configuration, verdicts)
			}
		}
		for other, toggle := range toggles {
			if other == name {
				continue
			}
			verdicts := classifyAll(newAppTestDetector(func(settings *DmcaSecurityPolicySettings) { toggle(settings.App) }), flow.transport, 47301, ethTestPort, flow.payloads...)
			if _, verdict := firstDecision(verdicts); verdict != dmcaAllow {
				t.Errorf("%s with %s off: verdicts = %v, want allow", name, other, verdicts)
			}
		}
	}
}

func TestProviderReversePolicyAdmitsEthereum(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sender := NewId()
	flows := map[string]struct {
		transport IpProtocol
		payloads  [][]byte
	}{
		"discv4":     {IpProtocolUdp, ethTestDiscv4Flow("provider-discv4")},
		"rlpx eip-8": {IpProtocolTcp, ethTestRlpxFlow("provider-rlpx", true)},
		"rlpx pre":   {IpProtocolTcp, ethTestRlpxFlow("provider-rlpx-pre", false)},
		"mse":        {IpProtocolTcp, [][]byte{mseFirstMessage("provider-mse", 211), appTestBytes("provider-mse-2", 120), appTestBytes("provider-mse-3", 300)}},
	}
	for name, flow := range flows {
		policy := DefaultProviderSecurityPolicy(ctx)
		if flow.transport == IpProtocolTcp {
			inspectAndRefreshIngressForSenderBorrowed(policy, sender, protocol.ProvideMode_Public, *appTestPath(flow.transport, 47400, ethTestPort, true), nil)
		}
		results := []SecurityPolicyResult{}
		for _, payload := range flow.payloads {
			r, err := inspectAndRefreshIngressForSenderBorrowed(policy, sender, protocol.ProvideMode_Public, *appTestPath(flow.transport, 47400, ethTestPort, false), payload)
			if err != nil {
				t.Fatal(err)
			}
			results = append(results, r)
		}
		want := SecurityPolicyResultAllow
		if name == "mse" {
			want = SecurityPolicyResultDrop
		}
		if results[len(results)-1] != want {
			t.Fatalf("%s: provider results = %v, want %v last", name, results, want)
		}
	}
}

func TestEthereumDetectorsZeroAlloc(t *testing.T) {
	app := newAppStandardDetector(DefaultAppStandardSettings())
	udp := appTestPath(IpProtocolUdp, 1, ethTestPort, false)
	tcp := appTestPath(IpProtocolTcp, 1, ethTestPort, false)
	discv4 := ethTestDiscv4("alloc-discv4", 0x01, ethTestDiscv4Data("alloc-discv4"))
	eip8 := ethTestRlpxEip8("alloc-rlpx", 169+150)
	pre := ethTestRlpxPreEip8("alloc-rlpx-pre")
	// warm the hash pool
	app.match(udp, discv4, true)
	if allocations := testing.AllocsPerRun(1000, func() {
		if _, _, ok := app.match(udp, discv4, true); !ok {
			t.Fatal("discv4 did not match")
		}
		if _, _, ok := app.match(tcp, eip8, true); !ok {
			t.Fatal("eip-8 auth did not match")
		}
		if _, _, ok := app.match(tcp, pre, true); !ok {
			t.Fatal("pre-eip-8 auth did not match")
		}
	}); allocations != 0 {
		t.Fatalf("ethereum detectors allocate %.2f objects per call, want 0", allocations)
	}
}

// FuzzEthereumDetectors cross-checks every match against an independent
// implementation of its invariant: the hash recomputed with a fresh Keccak and
// the key parsed by secp256k1.ParsePubKey.
func FuzzEthereumDetectors(f *testing.F) {
	for _, payload := range ethTestDiscv4Flow("fuzz") {
		f.Add(payload, false)
	}
	for _, payload := range ethTestRlpxFlow("fuzz", true) {
		f.Add(payload, true)
	}
	f.Add(ethTestRlpxPreEip8("fuzz-pre"), true)
	for _, packet := range ethTestDiscv4NearMisses() {
		f.Add(packet, false)
	}
	for _, payloads := range ethTestRlpxNearMisses() {
		f.Add(payloads[0], true)
	}
	f.Fuzz(func(t *testing.T, payload []byte, tcp bool) {
		if tcp {
			if end, ok := ethereumRlpxAuth(payload); ok {
				if _, err := secp256k1.ParsePubKey(payload[end-65 : end]); err != nil || payload[end-65] != 0x04 {
					t.Fatalf("rlpx matched an invalid key: %v", err)
				}
			}
		} else if _, ok := ethereumDiscv4Packet(payload); ok {
			if !bytes.Equal(ethTestKeccak(payload[32:]), payload[:32]) {
				t.Fatal("discv4 matched a wrong hash")
			}
		}
		detector := newAppTestDetector(nil)
		transport := IpProtocolUdp
		if tcp {
			transport = IpProtocolTcp
		}
		detector.classify(appTestPath(transport, 1, ethTestPort, false), payload)
	})
}
