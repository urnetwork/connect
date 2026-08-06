package connect

// Stateful BitTorrent / unsanctioned-encrypted egress detector for non-privileged (>=1024)
// destination ports.
//
// This is the deep-packet-inspection backstop the port rules in ip_security.go
// were always waiting for (see the "better deep packet inspection" FIXMEs). It
// advances a small state machine over the first payload-bearing packets of each
// egress flow until it reaches a terminal verdict:
//
//   - a privileged destination port (<1024) -> Allow without inspection: peers, DHT, uTP, and
//     plaintext trackers run on ephemeral/high ports, so a privileged port can't host a p2p hole;
//     skipping it lets legitimate non-web-standard encrypted services (e.g. Telegram MTProto on
//     443) through. Peer traffic on high ports is still inspected.
//   - a positive, plaintext BitTorrent signature  -> Incident (report) + Drop
//   - an initial payload that looks fully encrypted/random AND is NOT a
//     whitelisted web standard (TLS, DTLS, QUIC, STUN/TURN) -> Drop
//   - anything else (plaintext unknown protocol, a web standard, or budget
//     exhausted without a hit) -> Allow
//
// Packets are allowed through while the flow is still INSPECTING; enforcement
// only begins once a terminal verdict is reached. Detection is keyed off the
// outbound (client->destination) direction.
//
// Everything here is a clean-room implementation from the public protocol
// definitions: BitTorrent BEP 3 (peer wire / HTTP tracker), BEP 5 (DHT KRPC),
// BEP 15 (UDP tracker), BEP 29 (uTP); TLS RFC 8446/5246; DTLS RFC 6347/9147;
// QUIC RFC 9000/9369; STUN RFC 5389. The byte signatures are protocol facts,
// not derived from any third-party implementation.

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"math/bits"
	"sync"
	"sync/atomic"
	"time"
)

// number of flow-table shards. Sharding keeps the per-packet table lookup off a
// single global lock on the hot path.
const dmcaFlowShards = 16

// DmcaSecurityPolicySettings holds every threshold and decision input for the
// egress BitTorrent detector, so behavior can be tuned (and later driven by a
// control message) without code changes. Use DefaultDmcaSecurityPolicySettings
// for reasonable defaults.
type DmcaSecurityPolicySettings struct {
	// Enabled turns the payload detector on. When false the egress policy keeps
	// only its port/ip rules and never inspects payloads.
	Enabled bool

	// LogOnly evaluates the state machine and records stats but never converts a
	// detection into Drop/Incident. Use to measure false positives before
	// enforcing.
	LogOnly bool

	// DropBittorrentSignature enforces on flows matching a positive, plaintext
	// BitTorrent signature.
	DropBittorrentSignature bool
	// ReportBittorrentIncident returns SecurityPolicyResultIncident (ReportAbuse)
	// rather than a silent Drop for signature matches.
	ReportBittorrentIncident bool

	// DropUnsanctionedEncrypted enforces on flows whose initial payload looks
	// fully encrypted/random and are not positively identified as a whitelisted
	// web standard. This is the heuristic backstop for obfuscated BitTorrent
	// (MSE/PE over TCP, or encrypted uTP over UDP).
	DropUnsanctionedEncrypted bool

	// InspectionPacketBudget is the max number of payload-bearing packets to
	// inspect for a flow before giving up and treating it as not-BitTorrent.
	InspectionPacketBudget int
	// EncryptedDecisionPackets is how many consecutive encrypted-looking,
	// otherwise-unidentified payload packets are required before the encrypted
	// heuristic fires (capped by InspectionPacketBudget).
	EncryptedDecisionPackets int
	// MaxInspectionPayload caps how many leading payload bytes are examined.
	MaxInspectionPayload int

	// MinEncryptedPayload is the minimum payload length before the entropy
	// heuristic will classify a payload as encrypted (short payloads are
	// statistically unreliable and treated as inconclusive).
	MinEncryptedPayload int
	// EncryptedPopcountBand is the max distance from 0.5 of the fraction of set
	// bits for a payload to be considered random (0.10 => [0.40, 0.60]).
	EncryptedPopcountBand float64
	// EncryptedMaxPrintableFraction is the max fraction of printable-ASCII bytes
	// allowed for a payload to be considered encrypted (text protocols are mostly
	// printable; ciphertext is mostly not).
	EncryptedMaxPrintableFraction float64
	// EncryptedMinNormalizedEntropy is the min Shannon entropy (normalized to the
	// sample size, 0..1) for a payload to be considered encrypted.
	EncryptedMinNormalizedEntropy float64

	// MaxFlows bounds the total tracked flows for memory; the oldest are evicted
	// first. 0 disables the bound.
	MaxFlows int

	// FlowTtl evicts a tracked flow after this much wall-clock time with no packet
	// activity in either direction — every sent (RefreshEgress) and received (RefreshIngress)
	// packet refreshes it, so only genuinely idle flows are reclaimed. An active scan goroutine
	// performs the eviction. 0 disables the scan (flows then persist until MaxFlows
	// capacity-LRU eviction).
	FlowTtl time.Duration
}

func DefaultDmcaSecurityPolicySettings() *DmcaSecurityPolicySettings {
	return &DmcaSecurityPolicySettings{
		Enabled:                       true,
		LogOnly:                       false,
		DropBittorrentSignature:       true,
		ReportBittorrentIncident:      true,
		DropUnsanctionedEncrypted:     true,
		InspectionPacketBudget:        8,
		EncryptedDecisionPackets:      3,
		MaxInspectionPayload:          512,
		MinEncryptedPayload:           32,
		EncryptedPopcountBand:         0.10,
		EncryptedMaxPrintableFraction: 0.50,
		EncryptedMinNormalizedEntropy: 0.85,
		// scaled by the memory budget: each tracked flow holds ~100-200
		// bytes (state + map overhead), so the cap bounds the policy's
		// worst case footprint per instance
		MaxFlows: MemoryScaledCount(65536, 4096),
		FlowTtl:  300 * time.Second,
	}
}

type dmcaVerdict int32

const (
	dmcaInspecting    dmcaVerdict = 0
	dmcaAllow         dmcaVerdict = 1
	dmcaDropEncrypted dmcaVerdict = 2
	dmcaBittorrent    dmcaVerdict = 3
)

// dmcaFlowState is the per-flow state machine. It implements UserLimited so the
// shared applyLruUserLimit eviction can bound memory.
type dmcaFlowState struct {
	// atomic; first field for 64-bit alignment on 32-bit architectures
	lastActivityUnixNanos int64
	// atomic dmcaVerdict; the terminal-verdict fast path reads this without
	// taking mu, so steady-state packets on a decided flow are lock-free
	terminal int32

	key Ip6Path

	// mu guards the inspection bookkeeping below, touched only while INSPECTING
	mu               sync.Mutex
	inspectedPackets int
	encryptedPackets int
	sawObservation   bool
	sawFlowStart     bool
	sawPlaintext     bool
}

func (self *dmcaFlowState) LastActivityTime() time.Time {
	return time.Unix(0, atomic.LoadInt64(&self.lastActivityUnixNanos))
}

func (self *dmcaFlowState) Cancel() {
	// no async resources; eviction just drops the map entry
}

func (self *dmcaFlowState) setTerminal(v dmcaVerdict) dmcaVerdict {
	atomic.StoreInt32(&self.terminal, int32(v))
	return v
}

// advance moves the state machine forward by one packet and returns the current
// verdict. The payload is read synchronously and never retained, so the shared
// packet buffer is not aliased.
func (self *dmcaFlowState) advance(ipPath *IpPath, payload []byte, settings *DmcaSecurityPolicySettings, web *webStandardDetector) dmcaVerdict {
	self.mu.Lock()
	defer self.mu.Unlock()

	// recheck: another goroutine may have decided between the fast-path load and here
	if v := dmcaVerdict(atomic.LoadInt32(&self.terminal)); v != dmcaInspecting {
		return v
	}

	if !self.sawObservation {
		self.sawObservation = true
		// we can only trust the encrypted heuristic if we observed the flow from
		// its start. For TCP that means we saw the SYN; for UDP the first datagram
		// of a 5-tuple is effectively the start.
		if IpProtocolTcp == ipPath.Protocol {
			self.sawFlowStart = ipPath.Syn
		} else {
			self.sawFlowStart = true
		}
	}

	// empty payloads (TCP SYN / pure ACK) carry no signal but keep the flow open
	if 0 == len(payload) {
		return dmcaInspecting
	}

	self.inspectedPackets += 1

	b := payload
	if settings.MaxInspectionPayload < len(b) {
		b = b[:settings.MaxInspectionPayload]
	}

	if detectBittorrentSignature(ipPath, b) {
		return self.setTerminal(dmcaBittorrent)
	}
	if web.match(ipPath, b) {
		// a sanctioned web standard (TLS/QUIC/DTLS/STUN): the fallback that keeps
		// the encrypted-traffic heuristic from dropping a legitimate encrypted flow
		return self.setTerminal(dmcaAllow)
	}
	if isHttpRequest(b) {
		// raw plaintext HTTP (a request line) — media/radio streaming relies on it,
		// including on non-standard ports. Allow definitively (and early, before any
		// budget/entropy bookkeeping) so a later high-entropy body never trips the
		// encrypted-traffic heuristic. Checked after the BitTorrent signatures so an
		// HTTP-tracker GET is still classified as BitTorrent.
		return self.setTerminal(dmcaAllow)
	}
	if payloadLooksEncrypted(b, settings) {
		self.encryptedPackets += 1
	} else if settings.MinEncryptedPayload <= len(b) {
		// long enough to judge and not random: an unidentified plaintext protocol
		self.sawPlaintext = true
	}
	// payloads too short to judge are inconclusive and only consume budget

	decisionPackets := settings.EncryptedDecisionPackets
	if settings.InspectionPacketBudget < decisionPackets {
		decisionPackets = settings.InspectionPacketBudget
	}

	switch {
	case self.sawPlaintext:
		// per policy only sketchy (encrypted) non-web-standard traffic is dropped
		return self.setTerminal(dmcaAllow)
	case settings.DropUnsanctionedEncrypted && self.sawFlowStart && decisionPackets <= self.encryptedPackets:
		return self.setTerminal(dmcaDropEncrypted)
	case settings.InspectionPacketBudget <= self.inspectedPackets:
		return self.setTerminal(dmcaAllow)
	default:
		return dmcaInspecting
	}
}

type dmcaFlowShard struct {
	mu    sync.RWMutex
	flows map[Ip6Path]*dmcaFlowState
}

type dmcaDetector struct {
	settings    *DmcaSecurityPolicySettings
	web         *webStandardDetector
	perShardCap int
	shards      [dmcaFlowShards]*dmcaFlowShard
}

func newDmcaDetector(ctx context.Context, settings *DmcaSecurityPolicySettings, web *webStandardDetector) *dmcaDetector {
	perShardCap := 0
	if 0 < settings.MaxFlows {
		perShardCap = settings.MaxFlows / dmcaFlowShards
		if perShardCap < 1 {
			perShardCap = 1
		}
	}
	self := &dmcaDetector{
		settings:    settings,
		web:         web,
		perShardCap: perShardCap,
	}
	for i := range self.shards {
		self.shards[i] = &dmcaFlowShard{
			flows: map[Ip6Path]*dmcaFlowState{},
		}
	}
	// reclaim flows idle past FlowTtl. The capacity-LRU eviction (evictWithLock) still
	// bounds memory under load; this adds prompt time-based reclamation when egress is quiet.
	if ctx != nil && 0 < settings.FlowTtl {
		go self.run(ctx)
	}
	return self
}

// run periodically evicts flows whose last packet activity (sent or received) is older
// than FlowTtl.
func (self *dmcaDetector) run(ctx context.Context) {
	scanInterval := self.settings.FlowTtl / 4
	if scanInterval < 5*time.Second {
		scanInterval = 5 * time.Second
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(scanInterval):
			self.evictIdle(time.Now())
		}
	}
}

// evictIdle drops every flow whose last activity is older than FlowTtl.
func (self *dmcaDetector) evictIdle(now time.Time) {
	ttl := self.settings.FlowTtl
	if ttl <= 0 {
		return
	}
	cutoff := now.Add(-ttl)
	for _, shard := range self.shards {
		shard.mu.Lock()
		for key, st := range shard.flows {
			if st.LastActivityTime().Before(cutoff) {
				delete(shard.flows, key)
			}
		}
		shard.mu.Unlock()
	}
}

// flowCount returns the number of tracked flows across all shards.
func (self *dmcaDetector) flowCount() int {
	n := 0
	for _, shard := range self.shards {
		shard.mu.RLock()
		n += len(shard.flows)
		shard.mu.RUnlock()
	}
	return n
}

// refresh updates a tracked flow's last-activity time — the eviction key for both the idle scan
// and the capacity-LRU. An untracked key (a privileged-port flow, or one already evicted) is a
// no-op; reverse-direction packets never create state.
func (self *dmcaDetector) refresh(key Ip6Path) {
	shard := self.shards[dmcaShardIndex(key)]
	shard.mu.RLock()
	st := shard.flows[key]
	shard.mu.RUnlock()
	if st != nil {
		atomic.StoreInt64(&st.lastActivityUnixNanos, time.Now().UnixNano())
	}
}

// touchEgress refreshes a flow from a sent (client->destination) packet — the packet's 5-tuple is
// the flow key directly.
func (self *dmcaDetector) touchEgress(ipPath *IpPath) {
	if !self.settings.Enabled {
		return
	}
	switch ipPath.Protocol {
	case IpProtocolTcp, IpProtocolUdp:
	default:
		return
	}
	// mirror classify: a privileged destination port is never tracked
	if ipPath.DestinationPort < 1024 {
		return
	}
	key := ipPath.ToIp6Path()
	// the flow 5-tuple is the identity here; the affinity ServerName is not set
	key.ServerName = ""
	self.refresh(key)
}

// touchIngress refreshes a flow from a received (destination->client) packet, reversing the
// 5-tuple to the egress key the flow is stored under.
func (self *dmcaDetector) touchIngress(ipPath *IpPath) {
	if !self.settings.Enabled {
		return
	}
	switch ipPath.Protocol {
	case IpProtocolTcp, IpProtocolUdp:
	default:
		return
	}
	// the egress destination port is the ingress source port; mirror the privileged-port skip
	if ipPath.SourcePort < 1024 {
		return
	}
	key := ipPath.Reverse().ToIp6Path()
	key.ServerName = ""
	self.refresh(key)
}

func dmcaShardIndex(key Ip6Path) int {
	// FNV-1a over the destination ip + ports; distribution, not security
	h := uint32(2166136261)
	for _, b := range key.DestinationIp {
		h = (h ^ uint32(b)) * 16777619
	}
	h = (h ^ uint32(key.SourcePort)) * 16777619
	h = (h ^ uint32(key.DestinationPort)) * 16777619
	return int(h % dmcaFlowShards)
}

// classify advances the per-flow state machine and returns the raw verdict
// (consulting the injected web-standards detector during inspection).
func (self *dmcaDetector) classify(ipPath *IpPath, payload []byte) dmcaVerdict {
	if !self.settings.Enabled {
		return dmcaAllow
	}
	switch ipPath.Protocol {
	case IpProtocolTcp, IpProtocolUdp:
	default:
		return dmcaAllow
	}

	// A privileged destination port (<1024) can't host a peer-to-peer / BitTorrent hole — peers,
	// DHT, uTP, and plaintext trackers all run on ephemeral/high ports — so allow it without
	// payload inspection or flow tracking. This lets legitimate non-web-standard encrypted services
	// on privileged ports (e.g. Telegram MTProto on 443) through, which the unsanctioned-encrypted
	// heuristic would otherwise drop. Peer traffic on high ports is still inspected.
	if ipPath.DestinationPort < 1024 {
		return dmcaAllow
	}

	key := ipPath.ToIp6Path()
	// the flow 5-tuple is the identity here; the affinity ServerName is not set
	key.ServerName = ""
	shard := self.shards[dmcaShardIndex(key)]

	shard.mu.RLock()
	st := shard.flows[key]
	shard.mu.RUnlock()
	if st == nil {
		shard.mu.Lock()
		st = shard.flows[key]
		if st == nil {
			// seed the activity time on creation; ongoing refreshes come from the per-direction
			// RefreshEgress/RefreshIngress calls at the forwarding points
			st = &dmcaFlowState{key: key, lastActivityUnixNanos: time.Now().UnixNano()}
			self.evictWithLock(shard)
			shard.flows[key] = st
		}
		shard.mu.Unlock()
	}

	v := dmcaVerdict(atomic.LoadInt32(&st.terminal))
	if dmcaInspecting == v {
		v = st.advance(ipPath, payload, self.settings, self.web)
	}
	return v
}

// inspect classifies the flow and maps the verdict to a SecurityPolicyResult via
// the policy settings. The egress policy switches on classify directly (to keep
// the bittorrent / web-standard / encrypted decision explicit); this is the
// convenience form for callers that only want the enforced result.
func (self *dmcaDetector) inspect(ipPath *IpPath, payload []byte) SecurityPolicyResult {
	return self.result(self.classify(ipPath, payload))
}

// evictWithLock drops the oldest flows so that inserting one more stays within
// the per-shard cap. Caller holds shard.mu for writing.
func (self *dmcaDetector) evictWithLock(shard *dmcaFlowShard) {
	if self.perShardCap <= 0 {
		return
	}
	if len(shard.flows) < self.perShardCap {
		return
	}
	applyLruMapLimit(shard.flows, self.perShardCap-1, func(key Ip6Path, st *dmcaFlowState) bool {
		delete(shard.flows, key)
		return true
	})
}

func (self *dmcaDetector) result(v dmcaVerdict) SecurityPolicyResult {
	switch v {
	case dmcaBittorrent:
		if self.settings.LogOnly || !self.settings.DropBittorrentSignature {
			return SecurityPolicyResultAllow
		}
		if self.settings.ReportBittorrentIncident {
			return SecurityPolicyResultIncident
		}
		return SecurityPolicyResultDrop
	case dmcaDropEncrypted:
		if self.settings.LogOnly || !self.settings.DropUnsanctionedEncrypted {
			return SecurityPolicyResultAllow
		}
		return SecurityPolicyResultDrop
	default:
		return SecurityPolicyResultAllow
	}
}

// --- positive BitTorrent signatures (clean-room from the BEPs) ---

// BEP 3 peer wire handshake: <0x13><"BitTorrent protocol"> (20 leading bytes of
// a 68-byte handshake). The pstr length byte 0x13 == 19 == len("BitTorrent protocol").
var bittorrentHandshakePrefix = []byte("\x13BitTorrent protocol")

func hasBittorrentHandshake(b []byte) bool {
	return bytes.HasPrefix(b, bittorrentHandshakePrefix)
}

// BEP 3 HTTP tracker: a GET to an /announce or /scrape endpoint carrying an
// info_hash query parameter.
func hasHttpTrackerRequest(b []byte) bool {
	if !bytes.HasPrefix(b, []byte("GET ")) {
		return false
	}
	line := b
	if i := bytes.IndexByte(b, '\n'); 0 <= i {
		line = b[:i]
	}
	if !bytes.Contains(line, []byte("info_hash=")) {
		return false
	}
	return bytes.Contains(line, []byte("/announce")) || bytes.Contains(line, []byte("/scrape"))
}

// HTTP/1.x request methods (RFC 9110) — the leading token of a request line. CONNECT
// is deliberately excluded: it opens an opaque tunnel that could carry anything.
var httpRequestMethods = [][]byte{
	[]byte("GET "), []byte("HEAD "), []byte("POST "), []byte("PUT "),
	[]byte("DELETE "), []byte("OPTIONS "), []byte("PATCH "), []byte("TRACE "),
}

// isHttpRequest reports whether b begins with a plaintext HTTP/1.x request line — a
// method token followed by an "HTTP/1." version on the first line. Used to positively
// allow raw HTTP (e.g. media/radio streaming) on any port, including non-standard ones.
func isHttpRequest(b []byte) bool {
	method := false
	for _, m := range httpRequestMethods {
		if bytes.HasPrefix(b, m) {
			method = true
			break
		}
	}
	if !method {
		return false
	}
	line := b
	if i := bytes.IndexByte(b, '\n'); 0 <= i {
		line = b[:i]
	}
	return bytes.Contains(line, []byte(" HTTP/1."))
}

// BEP 5 DHT (Kademlia KRPC over UDP): bencoded dictionaries. Bencode keys are
// lexically sorted, which fixes the query/response prefixes; the generic case
// keys off the single-char 'y' (message type) and 't' (transaction) keys.
func isDhtKrpc(b []byte) bool {
	if len(b) < 5 || 'd' != b[0] {
		return false
	}
	if bytes.HasPrefix(b, []byte("d1:ad2:id20:")) || bytes.HasPrefix(b, []byte("d1:rd2:id20:")) {
		return true
	}
	hasType := bytes.Contains(b, []byte("1:y1:q")) ||
		bytes.Contains(b, []byte("1:y1:r")) ||
		bytes.Contains(b, []byte("1:y1:e"))
	return hasType && bytes.Contains(b, []byte("1:t"))
}

// BEP 15 UDP tracker: the connect request opens with the 64-bit magic
// 0x41727101980 followed by a 32-bit action == 0.
var udpTrackerConnectMagic = []byte{0x00, 0x00, 0x04, 0x17, 0x27, 0x10, 0x19, 0x80}

func isUdpTrackerConnect(b []byte) bool {
	if len(b) < 16 {
		return false
	}
	if !bytes.HasPrefix(b, udpTrackerConnectMagic) {
		return false
	}
	return 0 == binary.BigEndian.Uint32(b[8:12])
}

// BEP 29 uTP: a version-1 header (20 bytes) whose payload begins with a plaintext
// peer-wire handshake. A bare uTP header is only a weak structural hint (~2% of
// random datagrams), so it is not by itself a drop trigger; encrypted uTP is left
// to the entropy heuristic.
func utpV1CarriesHandshake(b []byte) bool {
	if len(b) < 20 {
		return false
	}
	packetType := b[0] >> 4
	version := b[0] & 0x0f
	if 1 != version || 4 < packetType {
		return false
	}
	if 2 < b[1] {
		return false
	}
	return hasBittorrentHandshake(b[20:])
}

func detectBittorrentSignature(ipPath *IpPath, b []byte) bool {
	switch ipPath.Protocol {
	case IpProtocolTcp:
		return hasBittorrentHandshake(b) || hasHttpTrackerRequest(b)
	case IpProtocolUdp:
		return isDhtKrpc(b) || isUdpTrackerConnect(b) || utpV1CarriesHandshake(b)
	}
	return false
}

// --- "looks fully encrypted" heuristic ---

// payloadLooksEncrypted reports whether a payload is statistically
// indistinguishable from random bytes: a near-even fraction of set bits, few
// printable-ASCII bytes, and near-maximal entropy for the sample size. All three
// gates must pass, which keeps false positives off structured/text protocols.
func payloadLooksEncrypted(b []byte, settings *DmcaSecurityPolicySettings) bool {
	if len(b) < settings.MinEncryptedPayload {
		return false
	}
	printableFraction, popcountRatio, normalizedEntropy := dmcaByteStats(b)
	if settings.EncryptedMaxPrintableFraction < printableFraction {
		return false
	}
	if settings.EncryptedPopcountBand < math.Abs(popcountRatio-0.5) {
		return false
	}
	return settings.EncryptedMinNormalizedEntropy <= normalizedEntropy
}

func dmcaByteStats(b []byte) (printableFraction float64, popcountRatio float64, normalizedEntropy float64) {
	var counts [256]int
	printable := 0
	setBits := 0
	for _, c := range b {
		counts[c] += 1
		if 0x20 <= c && c <= 0x7e {
			printable += 1
		}
		setBits += bits.OnesCount8(c)
	}
	n := float64(len(b))
	printableFraction = float64(printable) / n
	popcountRatio = float64(setBits) / (n * 8)

	entropy := 0.0
	for _, count := range counts {
		if 0 < count {
			p := float64(count) / n
			entropy -= p * math.Log2(p)
		}
	}
	maxDistinct := n
	if 256 < maxDistinct {
		maxDistinct = 256
	}
	if maxBits := math.Log2(maxDistinct); 0 < maxBits {
		normalizedEntropy = entropy / maxBits
	}
	return printableFraction, popcountRatio, normalizedEntropy
}
