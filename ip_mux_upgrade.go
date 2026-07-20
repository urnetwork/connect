package connect

// UpgradeMux is the concrete IpMux that intercepts local DNS (UDP/53) — resolving it over DoH
// that egresses the tunnel and recording the IP→hostname reverse index for ServerName path
// affinity — and applies the HTTP (TCP/80) policy: pass through to the egress, or drop. It
// wraps the remote UserNat (the exit path) and is held by the SDK device.

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/connect/protocol"
)

// HttpUpgradeMode selects how intercepted plaintext HTTP (TCP/80) is handled: passed through to
// the egress unchanged, or dropped. String-valued so it is readable in configs and logs; an
// empty/unrecognized value is treated as Unencrypted (pass-through).
type HttpUpgradeMode string

const (
	// HttpUpgradeUnencrypted passes the HTTP request through to the egress unchanged.
	HttpUpgradeUnencrypted HttpUpgradeMode = "unencrypted"
	// HttpUpgradeBlock drops the request.
	HttpUpgradeBlock HttpUpgradeMode = "block"
)

// HttpUpgradeSettings configures HTTP handling.
type HttpUpgradeSettings struct {
	// Mode is HttpUpgradeUnencrypted (pass plaintext HTTP/80 through to the egress) or
	// HttpUpgradeBlock (drop it). Empty is treated as Unencrypted.
	Mode HttpUpgradeMode
}

func DefaultHttpUpgradeSettings() *HttpUpgradeSettings {
	return &HttpUpgradeSettings{Mode: HttpUpgradeUnencrypted}
}

// DnsUpgradeSettings configures how the mux intercepts and resolves DNS (UDP/53),
// symmetric with HttpUpgradeSettings.
type DnsUpgradeSettings struct {
	// Resolver is how intercepted queries are resolved (DoH etc.). nil disables DNS
	// interception entirely (queries pass through to the egress).
	Resolver *DnsResolverSettings
	// ResolveTimeout is the single timeout for resolving an intercepted query: it bounds the
	// tunnel-DoH retry loop (the per-query context) and the underlying DoH request alike — the
	// tun's DoH request timeout is derived from it, so they don't diverge. Sized to cover a slow
	// first connect + TLS handshake while a tunnel is still establishing. 0 means no bound (rely
	// on the resolver's own timeout).
	ResolveTimeout time.Duration
	// ResponseTtl is the TTL (seconds) set on synthesized DNS replies.
	ResponseTtl uint32
	// ReverseTtl bounds how long an IP→hostname affinity record (used for ServerName path
	// affinity) is retained after its last use. Refreshed whenever the IP is resolved or
	// looked up; an active maintenance goroutine evicts records idle longer than this so the
	// reverse map does not grow unbounded with every resolved IP. 0 disables eviction.
	ReverseTtl time.Duration
	// ReverseMaxEntries hard-caps the IP→hostname affinity map between TTL sweeps: inserting
	// a new IP at the cap evicts the least-recently-active of a sample first, so a resolve
	// burst cannot grow the map without bound on a memory-constrained host. 0 uses a default.
	ReverseMaxEntries int
	// MaxInflightQueries caps the distinct DNS questions the mux resolves concurrently.
	// Identical concurrent queries (client retransmits, duplicate lookups) coalesce onto one
	// resolution pipeline, so this bounds the pipelines — and with them the mux's burst
	// memory — regardless of client behavior. A claimed query beyond the cap is dropped
	// unanswered; the client retries and lands in a freed slot. 0 uses a default.
	MaxInflightQueries int
	// LocalFallbackTimeout handicaps the local fallback: if the tunnel-DoH (Resolver, egressing
	// the tunnel) hasn't answered a query within this delay, the query is also raced against
	// Fallback (a DoH resolver over the LOCAL host egress). The delay is the handicap — the tunnel
	// result is preferred whenever it arrives first, so the fallback only wins while the tunnel is
	// still establishing. 0 (or a nil Fallback) disables the fallback.
	LocalFallbackTimeout time.Duration
	// Fallback resolves over the local host egress (not the tunnel), used as the handicapped
	// fallback above so DNS stays responsive while the tunnel-DoH is still coming up — preventing
	// the OS from tearing down an apparently-unresponsive tunnel, at the cost of a brief DNS leak
	// during startup. nil disables the fallback.
	Fallback *DnsResolverSettings
}

// UpgradeMuxSettings holds the DNS and HTTP upgrade policies. Each consumer (apps,
// server/proxy) sets its own; defaults are conservative.
type UpgradeMuxSettings struct {
	Dns  *DnsUpgradeSettings
	Http *HttpUpgradeSettings
}

// DefaultUpgradeMuxSettings is the app/device default: DNS (UDP/53) is intercepted and resolved
// over DoH that egresses the tunnel, and plaintext HTTP (TCP/80) is passed through to the egress
// unchanged. While the tunnel-DoH is still establishing on a fresh connect (its connect and TLS
// budgets are tens of seconds), a query the tunnel can't answer within LocalFallbackTimeout is
// raced against a handicapped DoH resolver over the LOCAL host egress (Fallback), so DNS stays
// responsive and the OS doesn't tear the tunnel down — at the cost of a brief DNS leak during
// startup. The tunnel result is always preferred when it arrives first.
//
// For pure pass-through to the egress (the server/proxy use case — no DNS interception,
// no HTTP upgrade), do not install a mux at all: pass nil settings, which avoids a
// per-device tun/stack. A mux with nil Dns still passes DNS through but exists for HTTP.
func DefaultUpgradeMuxSettings() *UpgradeMuxSettings {
	resolver := DefaultDnsResolverSettings()
	return &UpgradeMuxSettings{
		Dns: &DnsUpgradeSettings{
			// DoH only, using the DoH client's configured servers, resolved through the
			// tun (egresses via the exit). Deliberately no EnableLocalDns/EnableLocalDoh
			// (host dialer) or EnableRemoteDns (plaintext :53), which would resolve
			// off-tunnel or in the clear. The remote dns servers are carried for the one
			// permitted plaintext use while EnableRemoteDns is off: resolving a
			// hostname-form doh server name through the tunnel (see DohCache.resolve).
			Resolver: &DnsResolverSettings{
				EnableRemoteDoh:   true,
				RemoteDohUrlsIpv4: resolver.RemoteDohUrlsIpv4,
				RemoteDohUrlsIpv6: resolver.RemoteDohUrlsIpv6,
				RemoteDnsIpv4:     resolver.RemoteDnsIpv4,
				RemoteDnsIpv6:     resolver.RemoteDnsIpv6,
			},
			// the tunnel/upstream can take tens of seconds to establish on first connect; the
			// budget must cover a full slow connect (tun dial 30s) plus TLS handshake (30s) so a
			// query racing startup waits for the tunnel rather than failing early.
			ResolveTimeout: 60 * time.Second,
			ResponseTtl:    60,
			// server names are stable and the map is capped + memory-shed, so retain
			// affinity records well past the OS resolver's own cache lifetime: a client
			// that dials from its DNS cache (long-TTL records like pbs.com's 24h) emits no
			// query the mux can record, so a short reverse TTL would idle-evict the name
			// while the client keeps using the ip, blanking the block-action host feed.
			ReverseTtl:         1 * time.Hour,
			ReverseMaxEntries:  defaultReverseMaxEntries,
			MaxInflightQueries: defaultMaxInflightDnsQueries,
			// handicapped local fallback: if the tunnel-DoH hasn't answered within 5s, also resolve
			// over the local host egress so DNS stays responsive while the tunnel comes up. Same DoH
			// servers, but via the host (EnableLocalDoh) rather than the tunnel.
			LocalFallbackTimeout: 5 * time.Second,
			Fallback: &DnsResolverSettings{
				EnableLocalDoh:   true,
				LocalDohUrlsIpv4: resolver.RemoteDohUrlsIpv4,
				LocalDohUrlsIpv6: resolver.RemoteDohUrlsIpv6,
			},
		},
		Http: &HttpUpgradeSettings{Mode: HttpUpgradeUnencrypted},
	}
}

const (
	// dnsTypeSvcb / dnsTypeHttps are the SVCB (64) and HTTPS (65) question types
	// (RFC 9460), not defined by x/net/dns/dnsmessage.
	dnsTypeSvcb  = dnsmessage.Type(64)
	dnsTypeHttps = dnsmessage.Type(65)
	// defaultMaxInflightDnsQueries is the DnsUpgradeSettings.MaxInflightQueries default.
	defaultMaxInflightDnsQueries = 96
	// maxDnsRespondersPerQuestion caps the responders attached to one in-flight question, so
	// a flood of same-question queries with distinct transaction ids stays bounded. A dropped
	// responder's client retries and is answered from the resolver cache.
	maxDnsRespondersPerQuestion = 32
	// defaultReverseMaxEntries is the DnsUpgradeSettings.ReverseMaxEntries default.
	defaultReverseMaxEntries = 4096
	// maxServerNamesPerIp caps the hostnames retained per affinity record (a CDN IP fronting
	// many hosts); the oldest name is dropped for a new one.
	maxServerNamesPerIp = 4
	// reverseEvictSampleSize is how many affinity records an over-cap insert samples to evict
	// the least-recently-active from (approximate LRU, O(sample) instead of a full scan).
	reverseEvictSampleSize = 32
)

type UpgradeMux struct {
	ctx    context.Context
	cancel context.CancelFunc

	mux      *IpMux
	settings atomic.Pointer[UpgradeMuxSettings]

	// blocker, when set and enabled, blocks resolution of ad/tracking
	// hostnames in handleDns. installed by the device via SetBlocker —
	// deliberately not part of the swappable settings, so SetSettings
	// cannot clear it.
	blocker atomic.Pointer[Blocker]

	// fallbackDohCache resolves over the local host egress (not the tun); the handicapped local
	// fallback used when the tunnel-DoH is slow to come up. nil when no Fallback is configured.
	fallbackDohCache atomic.Pointer[DohCache]

	// source/provideMode stamp packets the mux injects downstream (DNS replies).
	source      TransferPath
	provideMode protocol.ProvideMode

	// unregisterShed removes this mux from the memory-shedder registry on Close.
	unregisterShed func()

	// inflight coalesces claimed DNS queries by question: one resolution pipeline per
	// distinct in-flight question, fanning the answer out to the attached responders. Burst
	// memory then scales with distinct questions (capped by MaxInflightQueries), not with
	// claimed packets (client stub resolvers retransmit unanswered queries every ~1s).
	inflightLock sync.Mutex
	inflight     map[DohKey]*dnsFlight

	// reverse maps a resolved IP to the hostname(s) the mux served for it, for the
	// multi-client's ServerName path affinity (point 4) and block-action server-name
	// reporting. Self-contained + independently testable (see reverseIndex); the mux
	// records DoH resolutions into it, drives its idle eviction from run(), and its
	// ServerNameLookup / ServerNamesLearnedNotifier methods delegate to it.
	reverse *reverseIndex

	// sni passively captures the TLS SNI of egress TCP/443 ClientHellos into the reverse
	// index, naming flows the DNS path can't (a client dialing an ip from its own DNS
	// cache emits no query the mux sees). Observation only — the flow always passes
	// through. Self-contained + independently testable (see sniSniffer).
	sni *sniSniffer
}

// dnsResolverSettings extracts the resolver config from the mux settings (nil = no DNS
// interception).
func dnsResolverSettings(settings *UpgradeMuxSettings) *DnsResolverSettings {
	if settings != nil && settings.Dns != nil {
		return settings.Dns.Resolver
	}
	return nil
}

// fallbackResolverSettings extracts the local-egress fallback resolver config from the mux
// settings (nil = no fallback).
func fallbackResolverSettings(settings *UpgradeMuxSettings) *DnsResolverSettings {
	if settings != nil && settings.Dns != nil {
		return settings.Dns.Fallback
	}
	return nil
}

// dohRequestTimeout is the per-DoH-request timeout derived from the mux's resolve budget: a single
// ResolveTimeout bounds both the resolve retry loop and each underlying DoH request. 0 (no Dns, or
// no bound) lets the tun fall back to its default.
func dohRequestTimeout(settings *UpgradeMuxSettings) time.Duration {
	if settings != nil && settings.Dns != nil {
		return settings.Dns.ResolveTimeout
	}
	return 0
}

// buildFallbackDohCache builds a DoH resolver that egresses the LOCAL host network (not the tun),
// used as the handicapped local fallback when the tunnel-DoH is slow to come up. A nil rs (no
// Fallback configured) disables it. The resolver dials the host net dialer (DefaultDohSettings
// leaves DialContextSettings nil) and queries rs's local DoH servers (EnableLocalDoh).
func buildFallbackDohCache(rs *DnsResolverSettings) *DohCache {
	if rs == nil {
		return nil
	}
	dohSettings := DefaultDohSettings()
	dohSettings.DnsResolverSettings = rs
	// the fallback only bridges tunnel startup; keep its in-flight footprint and cache small
	// so it adds little to the (memory-constrained) extension on top of the primary
	// tunnel-DoH cache.
	dohSettings.MaxConcurrentHttpRequests = 4
	dohSettings.MaxConcurrentResolutions = 8
	dohSettings.MaxServersPerQuery = 2
	dohSettings.CacheMaxEntries = 256
	return NewDohCache(dohSettings)
}

func NewUpgradeMux(
	ctx context.Context,
	source TransferPath,
	provideMode protocol.ProvideMode,
	sendTimeout time.Duration,
	initialReceiver ReceivePacketFunction,
	settings *UpgradeMuxSettings,
	log Logger,
) (*UpgradeMux, error) {
	cancelCtx, cancel := context.WithCancel(ctx)
	tunSettings := DefaultTunSettings()
	tunSettings.Log = log
	// the DoH connections run inside a memory-constrained host (notably the iOS network
	// extension). Keep the gVisor TCP/UDP buffers small (16KB) — Max applies per connection,
	// and DoH responses fit comfortably in 16KB.
	tunSettings.TcpReceiveBuffer = TcpBufferRange{Min: 4 * 1024, Default: 16 * 1024, Max: 16 * 1024}
	tunSettings.TcpSendBuffer = TcpBufferRange{Min: 4 * 1024, Default: 16 * 1024, Max: 16 * 1024}
	tunSettings.UdpReceiveBufferByteCount = 16 * 1024
	tunSettings.UdpSendBufferByteCount = 16 * 1024
	// this stack only carries DoH resolution traffic, so the endpoint queue can be much
	// smaller than the data-plane default (1024), which bounds a stack-emit burst
	tunSettings.ChannelSize = 128
	// a single dial per attempt: the DoH servers are anycast and reliable, and each raced
	// dial holds an extra gVisor endpoint (and its goroutines) for up to the dial timeout
	// while the tunnel is still establishing
	tunSettings.DialRace = 1
	self := &UpgradeMux{
		ctx:         cancelCtx,
		cancel:      cancel,
		source:      source,
		provideMode: provideMode,
		inflight:    map[DohKey]*dnsFlight{},
	}
	self.settings.Store(settings)
	// the reverse index reads its cap from the live settings (reverseMaxEntries), so a
	// SetSettings change to ReverseMaxEntries applies without rebuilding the index
	self.reverse = newReverseIndex(self.reverseMaxEntries)
	// captured TLS SNIs feed the same reverse index as DNS resolutions (firing the
	// learned callbacks that invalidate stale block-action decisions)
	self.sni = newSniSniffer(func(dstAddr netip.Addr, serverName string) {
		self.reverse.record([]netip.Addr{dstAddr}, serverName)
	})
	// bound the resolver cache and fan-out below the server/proxy defaults (see
	// DefaultDohSettings); the mux resolves a device's queries, not a data plane's
	dohSettings := DefaultDohSettings()
	dohSettings.CacheMaxEntries = 1024
	dohSettings.MaxConcurrentResolutions = 24
	dohSettings.MaxConcurrentHttpRequests = 8
	dohSettings.MaxServersPerQuery = 2
	// record doh server name resolutions into the ip→hostname reverse index,
	// so the block action ignore matcher (which lists the server names)
	// also matches the resolved server addresses
	dohSettings.DohServerResolvedCallback = func(domain string, addrs []netip.Addr) {
		self.reverse.record(addrs, domain)
	}
	tunSettings.DohSettings = dohSettings
	// ResolveTimeout is the single DNS-resolution timeout: it bounds each handleDns attempt (the
	// query context) and the underlying DoH request through the tun alike. SetSettings re-derives
	// it too, so a runtime settings change updates both.
	tunSettings.DohRequestTimeout = dohRequestTimeout(settings)
	tun, err := CreateTunWithResolver(cancelCtx, tunSettings, dnsResolverSettings(settings))
	if err != nil {
		cancel()
		return nil, err
	}
	self.fallbackDohCache.Store(buildFallbackDohCache(fallbackResolverSettings(settings)))
	self.mux = NewIpMux(cancelCtx, tun, source, provideMode, sendTimeout, self.onSend, nil, initialReceiver, log)
	// drop recoverable caches when the host signals memory pressure
	self.unregisterShed = AddMemoryShedder(self.ShedMemory)
	// active maintenance: TTL-evict the IP→hostname affinity map so it doesn't grow unbounded
	go HandleError(self.run)
	return self, nil
}

// onSend claims and terminates intercepted DNS (UDP/53) and HTTP (TCP/80); everything else
// passes through to the upstream. TCP/443 passes through too, but is first observed for its
// TLS SNI (never claimed). The claim decision is a pure function of (protocol, dst port), so
// it is read from a cheap, allocation-free header peek — only a claimed flow (or a header the
// peek can't classify, e.g. IPv6 extension headers) needs the allocating full parse. This
// keeps the pass-through bulk off the parse/allocation path entirely.
func (self *UpgradeMux) onSend(source TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool {
	var tls tlsSegment
	switch peekClaim(packet, &tls) {
	case peekOther:
		return false // not a claimable flow — pass through without parsing
	case peekHttp:
		// block drops claimed plaintext HTTP; otherwise it passes through unchanged. neither
		// needs the full parse, so the pass-through :80 bulk stays off the allocating parse path.
		return self.httpBlocked()
	case peekTls:
		// observe the ClientHello for its SNI, then always pass through — TLS is never
		// claimed, only named. peekClaim already extracted the flow/payload while classifying,
		// so the sniffer reassembles from that segment without re-walking the L4 headers.
		self.sni.observeSegment(tls)
		return false
	}
	// peekDns or peekUndecided — classify with the authoritative full parse
	ipPath, payload, err := ParseIpPathWithPayload(packet)
	if err != nil {
		return false
	}
	switch {
	case IpProtocolUdp == ipPath.Protocol && 53 == ipPath.DestinationPort:
		if dns := self.settings.Load().Dns; dns != nil && dns.Resolver != nil {
			return self.handleDns(source, provideMode, ipPath, payload)
		}
		return false // DNS interception disabled — pass through to the egress
	case IpProtocolTcp == ipPath.Protocol && 80 == ipPath.DestinationPort:
		return self.httpBlocked() // reached via peekUndecided (e.g. IPv6 extension headers)
	}
	return false
}

// httpBlocked reports whether claimed plaintext HTTP (TCP/80) should be dropped (block mode);
// otherwise it passes through to the egress unchanged.
func (self *UpgradeMux) httpBlocked() bool {
	s := self.settings.Load()
	return s.Http != nil && HttpUpgradeBlock == s.Http.Mode
}

// SetBlocker installs (or, with nil, removes) the ad/tracker Blocker
// consulted by handleDns for every claimed dns query. the blocker is shared
// with the multi client and owned by the device; enabling/disabling happens
// on the blocker itself.
func (self *UpgradeMux) SetBlocker(blocker Blocker) {
	if blocker == nil {
		self.blocker.Store(nil)
	} else {
		self.blocker.Store(&blocker)
	}
}

func (self *UpgradeMux) getBlocker() Blocker {
	if b := self.blocker.Load(); b != nil {
		return *b
	}
	return nil
}

// peekResult classifies a send packet by the flow the mux may claim, so the common pass-through
// bulk (peekOther) and pass-through TCP/80 (peekHttp, not blocking) are decided without the
// allocating full parse. Only DNS and unclassifiable packets parse.
type peekResult int

const (
	peekOther     peekResult = iota // not a claimable flow → pass through
	peekDns                         // UDP/53 (DNS)
	peekHttp                        // TCP/80 (HTTP)
	peekTls                         // TCP/443 (observed for SNI, never claimed)
	peekUndecided                   // header can't be classified cheaply → full parse
)

// peekClaim classifies a packet from the fixed IP/L4 header offsets without allocating
// (ParseIpPathWithPayload allocates the IpPath and an address backing per call). peekUndecided
// means the header can't be classified cheaply — IPv6 with extension headers, or a
// short/unsupported header — and the caller must fall back to the full parse.
//
// For a TCP/443 packet (peekTls) it also fills *seg with the flow 4-tuple and TCP payload, so
// the SNI sniffer can reassemble without re-walking the L4 headers; seg is left untouched for
// every other result. Pass a throwaway *tlsSegment when only the classification is wanted.
func peekClaim(packet []byte, seg *tlsSegment) peekResult {
	if len(packet) < 20 {
		return peekUndecided
	}
	switch packet[0] >> 4 {
	case 4:
		ihl := int(packet[0]&0x0f) * 4
		if ihl < 20 || len(packet) < ihl+4 {
			return peekUndecided
		}
		switch packet[9] { // protocol
		case 6: // tcp
			switch int(packet[ihl+2])<<8 | int(packet[ihl+3]) {
			case 80:
				return peekHttp
			case 443:
				totalLen := int(packet[2])<<8 | int(packet[3])
				if totalLen < ihl || len(packet) < totalLen {
					totalLen = len(packet)
				}
				src, _ := netip.AddrFromSlice(packet[12:16])
				dst, _ := netip.AddrFromSlice(packet[16:20])
				if s, ok := tcpSegment443(packet, ihl, totalLen, src, dst); ok {
					*seg = s
					return peekTls
				}
				return peekOther
			}
			return peekOther
		case 17: // udp
			if 53 == int(packet[ihl+2])<<8|int(packet[ihl+3]) {
				return peekDns
			}
			return peekOther
		default:
			return peekOther // not tcp/udp: never claimed
		}
	case 6:
		if len(packet) < 44 {
			return peekUndecided
		}
		switch packet[6] { // next header
		case 6: // tcp
			switch int(packet[42])<<8 | int(packet[43]) {
			case 80:
				return peekHttp
			case 443:
				payloadLen := int(packet[4])<<8 | int(packet[5])
				end := 40 + payloadLen
				if len(packet) < end {
					end = len(packet)
				}
				src, _ := netip.AddrFromSlice(packet[8:24])
				dst, _ := netip.AddrFromSlice(packet[24:40])
				if s, ok := tcpSegment443(packet, 40, end, src, dst); ok {
					*seg = s
					return peekTls
				}
				return peekOther
			}
			return peekOther
		case 17: // udp
			if 53 == int(packet[42])<<8|int(packet[43]) {
				return peekDns
			}
			return peekOther
		default:
			return peekUndecided // extension header / other: needs the full parse
		}
	default:
		return peekUndecided
	}
}

// dnsResponder is one claimed client query awaiting the shared resolution of its
// question: enough to synthesize that client's exact reply — its transaction id and
// question (casing preserved, for clients that randomize it) — on its reversed path.
type dnsResponder struct {
	id          uint16
	question    dnsmessage.Question
	source      TransferPath
	provideMode protocol.ProvideMode
	reverse     *IpPath
}

// dnsFlight is the shared resolution pipeline for one in-flight question (see
// UpgradeMux.inflight). All fields are guarded by inflightLock.
type dnsFlight struct {
	responders []dnsResponder
	// replied is set when the answer was delivered (the flight is removed at the same
	// time); a slower worker's answer is dropped
	replied bool
	// workers is the outstanding pipeline goroutine count; the flight is removed when it
	// reaches 0 without a reply (resolution failed: send nothing, the clients retry)
	workers int
	// cancel is shared by every worker in an A/AAAA race. The first
	// authoritative answer cancels its losing sibling, and the flight remains
	// in inflight until all workers have observed cancellation and exited.
	cancel context.CancelFunc
}

// handleDns claims a single A/AAAA DNS query and attaches it to the resolution
// pipeline for its question — joining the in-flight pipeline when one exists, else
// starting one (resolution can block on the network, so it runs asynchronously via
// the Tun's DohCache). The pipeline writes each attached client its own response and
// records the IP→hostname mapping. SVCB/HTTPS (64/65) queries are claimed and
// forwarded opaquely over the tunnel DoH — the record (alpn/hints/ech) is preserved
// for the client, and its ipv4hint/ipv6hint addresses are recorded into the reverse
// index so those flows are named; when the forward cannot answer (remote DoH off or
// failed) the client gets a prompt SERVFAIL, and an over-UDP-size record gets a
// truncated (TC) reply, so a resolver waiting on the HTTPS RR never hangs on the
// claimed type. Other query types are not claimed and pass through to the upstream.
//
// The parsed question is a value (dnsmessage copies the name into a fixed array), and
// the reversed path owns its address bytes, so nothing aliases the recycled packet
// buffer across the pipeline goroutines.
func (self *UpgradeMux) handleDns(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) bool {
	var parser dnsmessage.Parser
	header, err := parser.Start(payload)
	if err != nil {
		return false
	}
	question, err := parser.Question()
	if err != nil {
		return false
	}
	// lower cased: clients that randomize query casing (dns 0x20) must coalesce,
	// cache, and record as one name — the reverse index and its learned callbacks
	// key on this. The client's reply echoes the original-cased question, which is
	// carried separately on the responder.
	domain := strings.ToLower(strings.TrimSuffix(question.Name.String(), "."))

	// the blocker consults every query type, ahead of any resolution or
	// caching: a blocked name answers A/AAAA with the unspecified address
	// (the OS fails the connect instantly and locally) and every other type
	// — notably HTTPS/SVCB (65), whose ipv4hint/ipv6hint would bypass the
	// null A/AAAA — with an empty NOERROR. blocked replies never populate
	// the reverse index or the resolver cache, so toggling the blocker
	// takes effect immediately.
	if blocker := self.getBlocker(); blocker != nil && blocker.BlockHost(domain) {
		var responseTtl uint32
		if dns := self.settings.Load().Dns; dns != nil {
			responseTtl = dns.ResponseTtl
		}
		var addrs []netip.Addr
		switch question.Type {
		case dnsmessage.TypeA:
			addrs = []netip.Addr{netip.IPv4Unspecified()}
		case dnsmessage.TypeAAAA:
			addrs = []netip.Addr{netip.IPv6Unspecified()}
		}
		respPayload, err := buildDnsResponse(header.ID, question, addrs, responseTtl)
		if err != nil {
			return false
		}
		reverse := ipPath.Reverse()
		self.mux.deliverDownstream(source, provideMode, reverse, ipOosPacket(reverse, respPayload))
		return true
	}

	// A/AAAA resolve-and-synthesize; SVCB/HTTPS forward the record opaquely (the resolver
	// parses A/AAAA into addresses but not SVCB, and forwarding preserves the record's
	// alpn/hints/ech for the client). Both coalesce on the same in-flight machinery.
	var recordType string
	forward := false
	switch question.Type {
	case dnsmessage.TypeA:
		recordType = "A"
	case dnsmessage.TypeAAAA:
		recordType = "AAAA"
	case dnsTypeHttps:
		recordType = "HTTPS"
		forward = true
	case dnsTypeSvcb:
		recordType = "SVCB"
		forward = true
	default:
		return false
	}

	responder := dnsResponder{
		id:          header.ID,
		question:    question,
		source:      source,
		provideMode: provideMode,
		// the response flows from the queried resolver back to the client
		reverse: ipPath.Reverse(),
	}
	key := NewDohKey(recordType, domain)
	if fl := self.attachDnsResponder(key, responder); fl != nil {
		if forward {
			self.startHttpsForwardPipeline(key, fl, question.Type, domain)
		} else {
			self.startDnsPipeline(key, fl, recordType, domain)
		}
	}
	return true
}

// attachDnsResponder attaches a claimed query to the resolution pipeline for its
// question: it joins the in-flight pipeline when one exists (client retransmits and
// a burst's duplicate lookups attach as responders, ~100 bytes, instead of spawning
// their own pipelines), starts a new flight otherwise, or drops the query at the
// caps — the client retries into a freed slot, which bounds burst memory regardless
// of client behavior. It returns the new flight when the caller must start the
// pipeline, else nil.
func (self *UpgradeMux) attachDnsResponder(key DohKey, responder dnsResponder) *dnsFlight {
	self.inflightLock.Lock()
	defer self.inflightLock.Unlock()
	if fl, ok := self.inflight[key]; ok {
		// An answer has already snapshotted this flight's responders. Keep the
		// flight counted until its losing workers exit, but do not attach a
		// responder that could no longer receive that answer.
		if fl.replied {
			return nil
		}
		for _, r := range fl.responders {
			if r.id == responder.id && r.reverse.DestinationPort == responder.reverse.DestinationPort && r.reverse.DestinationIp.Equal(responder.reverse.DestinationIp) {
				// a retransmit of an attached query; it is answered when the pipeline replies
				return nil
			}
		}
		if len(fl.responders) < maxDnsRespondersPerQuestion {
			fl.responders = append(fl.responders, responder)
		}
		// else over the responder cap: drop (the client retries into the answer cache)
		return nil
	}
	maxInflight := defaultMaxInflightDnsQueries
	if dns := self.settings.Load().Dns; dns != nil && 0 < dns.MaxInflightQueries {
		maxInflight = dns.MaxInflightQueries
	}
	if maxInflight <= len(self.inflight) {
		// at the question cap: drop (the client retries; slots free as pipelines finish)
		return nil
	}
	fl := &dnsFlight{
		responders: []dnsResponder{responder},
	}
	self.inflight[key] = fl
	return fl
}

// startDnsPipeline resolves one question and fans the first successful answer out to
// the flight's responders, exactly once, retiring the flight. A later identical query
// starts a fresh pipeline, answered immediately from the resolver cache.
func (self *UpgradeMux) startDnsPipeline(key DohKey, fl *dnsFlight, recordType string, domain string) {
	var resolveTimeout time.Duration
	var responseTtl uint32
	var localFallbackTimeout time.Duration
	if dns := self.settings.Load().Dns; dns != nil {
		resolveTimeout = dns.ResolveTimeout
		responseTtl = dns.ResponseTtl
		localFallbackTimeout = dns.LocalFallbackTimeout
	}
	fallback := self.fallbackDohCache.Load()

	var queryCtx context.Context
	var queryCancel context.CancelFunc
	if 0 < resolveTimeout {
		queryCtx, queryCancel = context.WithTimeout(self.ctx, resolveTimeout)
	} else {
		queryCtx, queryCancel = context.WithCancel(self.ctx)
	}

	// reply delivers the first successful resolution to every attached responder, exactly
	// once. A failure — no records and no authoritative no-record answer, from both the
	// tunnel and the fallback — sends nothing: the clients time out and retry, which the OS
	// tolerates far better than a SERVFAIL or empty NOERROR reply it would surface as
	// "can't resolve address".
	reply := func(addrs []netip.Addr, authoritative bool) {
		if len(addrs) == 0 && !authoritative {
			return
		}
		var responders []dnsResponder
		var cancel context.CancelFunc
		func() {
			self.inflightLock.Lock()
			defer self.inflightLock.Unlock()
			if fl.replied {
				return
			}
			fl.replied = true
			responders = fl.responders
			cancel = fl.cancel
		}()
		if responders == nil {
			return
		}
		// Cancel the losing resolver before doing response construction and
		// delivery. The flight remains in the map until workerDone observes
		// every worker exit, so MaxInflightQueries also bounds queued losers.
		if cancel != nil {
			cancel()
		}
		if 0 < len(addrs) {
			self.reverse.record(addrs, domain)
		}
		for i := range responders {
			r := &responders[i]
			respPayload, err := buildDnsResponse(r.id, r.question, addrs, responseTtl)
			if err != nil {
				continue
			}
			self.mux.deliverDownstream(r.source, r.provideMode, r.reverse, ipOosPacket(r.reverse, respPayload))
		}
	}

	// workerDone retires the flight once every worker has exited without an answer, so a
	// failed question frees its slot for the clients' retries to start a fresh pipeline.
	workerDone := func() {
		var cancel context.CancelFunc
		self.inflightLock.Lock()
		fl.workers -= 1
		if fl.workers == 0 {
			if self.inflight[key] == fl {
				delete(self.inflight, key)
			}
			cancel = fl.cancel
			fl.cancel = nil
		}
		self.inflightLock.Unlock()
		if cancel != nil {
			cancel()
		}
	}

	workers := 1
	if fallback != nil && 0 < localFallbackTimeout {
		workers = 2
	}
	func() {
		self.inflightLock.Lock()
		defer self.inflightLock.Unlock()
		fl.workers = workers
		fl.cancel = queryCancel
	}()

	// primary: resolve over the tunnel-DoH (preferred — egresses the tunnel, no DNS leak), retrying
	// on the dnsRetryBackoff schedule. The first connect/TLS over a freshly-connecting tunnel can
	// take tens of seconds (the tun dial and TLS handshake budgets are 30s each, within the 60s
	// ResolveTimeout), so a slow attempt waits for the tunnel rather than failing fast.
	tunnelOk := make(chan struct{})
	go HandleError(func() {
		defer workerDone()
		addrs, authoritative := self.resolveTunnelDoh(queryCtx, recordType, domain)
		if 0 < len(addrs) || authoritative {
			close(tunnelOk) // the tunnel won — signal the fallback to skip its local query (no leak)
		}
		reply(addrs, authoritative)
	})

	// handicapped local fallback: if the tunnel-DoH hasn't produced an answer within
	// LocalFallbackTimeout, also resolve over the LOCAL host egress (bypassing the tunnel). The
	// delay handicaps the local resolver so the tunnel wins whenever it can; the fallback only
	// answers while the tunnel is still establishing, keeping DNS responsive so the OS doesn't tear
	// down the tunnel (at the cost of a brief DNS leak during startup).
	if workers == 2 {
		go HandleError(func() {
			defer workerDone()
			timer := time.NewTimer(localFallbackTimeout)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-tunnelOk:
				return
			case <-queryCtx.Done():
				return
			}
			addrs, authoritative := fallback.QueryResult(queryCtx, recordType, domain)
			reply(addrs, authoritative)
		})
	}
}

// maxForwardedHttpsResponse bounds the SVCB/HTTPS response delivered to the client over UDP/53.
// A larger record's hints are still recorded, but the record itself is not delivered — the
// client instead gets a truncated (TC) reply, the accurate signal that the answer exceeds
// UDP/53, so it can fail over to its own TCP/EDNS0 path (which passes through the mux
// unclaimed) or fall back to A/AAAA without waiting out a timeout.
const maxForwardedHttpsResponse = 1232

// startHttpsForwardPipeline forwards one SVCB/HTTPS question over the tunnel DoH and fans the raw
// record out to the flight's responders (each gets the record with its own DNS transaction id),
// recording the record's ipv4hint/ipv6hint addresses into the reverse index so those flows are
// named. The type is claimed unconditionally, so a forward that cannot answer (remote DoH
// disabled, or the forward failed) must not black-hole the query: the client gets a prompt
// SERVFAIL and falls back to A/AAAA immediately — resolvers serialize on the HTTPS RR answer,
// and silence here would stall them until their own timeout.
func (self *UpgradeMux) startHttpsForwardPipeline(key DohKey, fl *dnsFlight, qType dnsmessage.Type, domain string) {
	var resolveTimeout time.Duration
	if dns := self.settings.Load().Dns; dns != nil {
		resolveTimeout = dns.ResolveTimeout
	}
	go HandleError(func() {
		// always retire the flight so the key can't leak
		defer func() {
			self.inflightLock.Lock()
			defer self.inflightLock.Unlock()
			if self.inflight[key] == fl {
				delete(self.inflight, key)
			}
		}()

		queryCtx := self.ctx
		if 0 < resolveTimeout {
			var cancel context.CancelFunc
			queryCtx, cancel = context.WithTimeout(self.ctx, resolveTimeout)
			defer cancel()
		}
		if response, ok := self.mux.Tun().DohCache().Forward(queryCtx, qType, domain); ok {
			self.fanOutHttpsForward(key, fl, domain, response)
		} else {
			// fail fast instead of dropping: remote DoH is off or the forward
			// failed, and no answer will ever come from this pipeline
			self.deliverDnsStatus(self.takeHttpsForwardResponders(key, fl), dnsmessage.RCodeServerFailure, false)
		}
	})
}

// takeHttpsForwardResponders snapshots and retires an SVCB/HTTPS flight exactly once,
// returning nil when the flight has already replied. Removing the flight in the same
// critical section as the responder snapshot means a query racing completion either
// joins before this snapshot and is answered, or creates a new flight afterward.
func (self *UpgradeMux) takeHttpsForwardResponders(key DohKey, fl *dnsFlight) []dnsResponder {
	var responders []dnsResponder
	func() {
		self.inflightLock.Lock()
		defer self.inflightLock.Unlock()
		if fl.replied {
			return
		}
		fl.replied = true
		responders = fl.responders
		if self.inflight[key] == fl {
			delete(self.inflight, key)
		}
	}()
	return responders
}

// deliverDnsStatus answers each responder with a header+question-only response carrying
// `rcode` and, when `truncated`, the TC bit — the fail-fast replies (SERVFAIL for a dead
// forward, TC for an answer that exceeds UDP/53).
func (self *UpgradeMux) deliverDnsStatus(responders []dnsResponder, rcode dnsmessage.RCode, truncated bool) {
	for i := range responders {
		r := &responders[i]
		respPayload, err := buildDnsStatusResponse(r.id, r.question, rcode, truncated)
		if err != nil {
			continue
		}
		self.mux.deliverDownstream(r.source, r.provideMode, r.reverse, ipOosPacket(r.reverse, respPayload))
	}
}

// fanOutHttpsForward delivers a forwarded SVCB/HTTPS record to the flight's responders (each with
// its own DNS transaction id stamped in) and records the record's ipv4hint/ipv6hint addresses into
// the reverse index. An oversized record is captured (hints recorded) but answered with a
// truncated (TC) reply instead of the record; a malformed record is answered SERVFAIL. Split from
// the forward round-trip so it is testable with a canned response (the tunnel-DoH round-trip
// itself can't be driven in a connect unit test — see the skipped
// TestUpgradeMuxDefaultDnsThroughTunnel).
func (self *UpgradeMux) fanOutHttpsForward(key DohKey, fl *dnsFlight, domain string, response []byte) {
	responders := self.takeHttpsForwardResponders(key, fl)
	if responders == nil {
		return
	}

	// record the hint addresses -> domain (fires the learned callbacks that invalidate stale
	// block-action decisions), so a flow to a hint ip reports the server name
	if hints := parseHttpsHints(response); 0 < len(hints) {
		self.reverse.record(hints, domain)
	}

	if maxForwardedHttpsResponse < len(response) {
		// oversized for UDP/53: truncation is the accurate signal — the client
		// retries over its own TCP path (unclaimed pass-through) or falls back
		// to A/AAAA, instead of timing out on silence
		self.deliverDnsStatus(responders, dnsmessage.RCodeSuccess, true)
		return
	}
	if len(response) < 2 {
		// malformed beyond repair (no transaction id to patch): a failure
		self.deliverDnsStatus(responders, dnsmessage.RCodeServerFailure, false)
		return
	}
	for i := range responders {
		r := &responders[i]
		// The DoH lookup uses the normalized lowercase domain, but DNS 0x20
		// clients validate that the response echoes their exact query casing.
		// Patch both the transaction id and the (same-length) question name
		// without changing answer offsets or compression pointers.
		resp, ok := dnsResponseForResponder(response, r)
		if !ok {
			continue
		}
		self.mux.deliverDownstream(r.source, r.provideMode, r.reverse, ipOosPacket(r.reverse, resp))
	}
}

// dnsResponseForResponder copies a one-question DNS response and restores the
// responder's original question casing. The normalized DoH query and original
// query differ only by ASCII case, so the wire name has identical length and
// answer compression offsets remain valid. A compressed/mismatched question is
// rejected rather than returning a response that can fail DNS 0x20 validation.
func dnsResponseForResponder(response []byte, responder *dnsResponder) ([]byte, bool) {
	if len(response) < 12 || binary.BigEndian.Uint16(response[4:6]) != 1 {
		return nil, false
	}
	name := responder.question.Name.String()
	if name == "" || name[len(name)-1] != '.' {
		return nil, false
	}

	wireName := make([]byte, 0, len(name)+1)
	for labelStart := 0; labelStart < len(name)-1; {
		labelEnd := strings.IndexByte(name[labelStart:], '.')
		if labelEnd < 0 || 63 < labelEnd {
			return nil, false
		}
		labelEnd += labelStart
		wireName = append(wireName, byte(labelEnd-labelStart))
		wireName = append(wireName, name[labelStart:labelEnd]...)
		labelStart = labelEnd + 1
	}
	wireName = append(wireName, 0)

	questionEnd := 12 + len(wireName)
	if len(response) < questionEnd+4 {
		return nil, false
	}
	responseName := response[12:questionEnd]
	for i := range wireName {
		a := responseName[i]
		b := wireName[i]
		if 'A' <= a && a <= 'Z' {
			a += 'a' - 'A'
		}
		if 'A' <= b && b <= 'Z' {
			b += 'a' - 'A'
		}
		if a != b {
			return nil, false
		}
	}
	if dnsmessage.Type(binary.BigEndian.Uint16(response[questionEnd:questionEnd+2])) != responder.question.Type ||
		dnsmessage.Class(binary.BigEndian.Uint16(response[questionEnd+2:questionEnd+4])) != responder.question.Class {
		return nil, false
	}

	result := make([]byte, len(response))
	copy(result, response)
	binary.BigEndian.PutUint16(result[0:2], responder.id)
	copy(result[12:questionEnd], wireName)
	return result, true
}

// parseHttpsHints extracts the ipv4hint (SvcParamKey 4) and ipv6hint (key 6) addresses from the
// SVCB/HTTPS answers of a DNS response wire, for recording into the reverse index. Malformed input
// yields the hints parsed so far.
func parseHttpsHints(response []byte) []netip.Addr {
	var parser dnsmessage.Parser
	if _, err := parser.Start(response); err != nil {
		return nil
	}
	if err := parser.SkipAllQuestions(); err != nil {
		return nil
	}
	var hints []netip.Addr
	for {
		h, err := parser.AnswerHeader()
		if err != nil { // ErrSectionDone or malformed
			break
		}
		switch h.Type {
		case dnsTypeHttps:
			r, err := parser.HTTPSResource()
			if err != nil {
				return hints
			}
			hints = appendSvcbHints(hints, &r.SVCBResource)
		case dnsTypeSvcb:
			r, err := parser.SVCBResource()
			if err != nil {
				return hints
			}
			hints = appendSvcbHints(hints, &r)
		default:
			if err := parser.SkipAnswer(); err != nil {
				return hints
			}
		}
	}
	return hints
}

func appendSvcbHints(hints []netip.Addr, r *dnsmessage.SVCBResource) []netip.Addr {
	if v, ok := r.GetParam(dnsmessage.SVCParamIPv4Hint); ok {
		for i := 0; i+4 <= len(v); i += 4 {
			hints = append(hints, netip.AddrFrom4([4]byte(v[i:i+4])))
		}
	}
	if v, ok := r.GetParam(dnsmessage.SVCParamIPv6Hint); ok {
		for i := 0; i+16 <= len(v); i += 16 {
			hints = append(hints, netip.AddrFrom16([16]byte(v[i:i+16])))
		}
	}
	return hints
}

// dnsRetryBackoff is the wait between tunnel-DoH resolution attempts, for attempts that fail
// fast (a hanging attempt already waits inside its request until ResolveTimeout). The schedule
// is short: each retry rebuilds a full server fan-out, and once the pipeline retires, the
// clients' own retransmits start a fresh one anyway — so long mux-side backoff only holds
// pipeline memory without improving resolution.
var dnsRetryBackoff = []time.Duration{
	1 * time.Second,
	2 * time.Second,
}

// resolveTunnelDoh resolves over the tun's DoH cache, retrying on dnsRetryBackoff until it gets a
// real answer (records, or an authoritative no-record), the backoff is exhausted, or ctx is done.
func (self *UpgradeMux) resolveTunnelDoh(ctx context.Context, recordType string, domain string) ([]netip.Addr, bool) {
	doh := self.mux.Tun().DohCache()
	var addrs []netip.Addr
	var authoritative bool
	for i := 0; ; i++ {
		addrs, authoritative = doh.QueryResult(ctx, recordType, domain)
		if 0 < len(addrs) || authoritative {
			return addrs, authoritative
		}
		if len(dnsRetryBackoff) <= i {
			return addrs, authoritative
		}
		select {
		case <-time.After(dnsRetryBackoff[i]):
		case <-ctx.Done():
			return addrs, authoritative
		}
	}
}

// buildDnsStatusResponse builds a header+question-only response: `rcode` (SERVFAIL for a
// forward that can never answer) and/or the TC bit (an answer that exceeds UDP/53). Used by
// the SVCB/HTTPS forward path to fail fast — the claimed type must never be a black hole.
func buildDnsStatusResponse(id uint16, question dnsmessage.Question, rcode dnsmessage.RCode, truncated bool) ([]byte, error) {
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:                 id,
		Response:           true,
		RecursionAvailable: true,
		Truncated:          truncated,
		RCode:              rcode,
	})
	builder.EnableCompression()
	if err := builder.StartQuestions(); err != nil {
		return nil, err
	}
	if err := builder.Question(question); err != nil {
		return nil, err
	}
	return builder.Finish()
}

func buildDnsResponse(id uint16, question dnsmessage.Question, addrs []netip.Addr, ttl uint32) ([]byte, error) {
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:                 id,
		Response:           true,
		RecursionAvailable: true,
	})
	builder.EnableCompression()
	if err := builder.StartQuestions(); err != nil {
		return nil, err
	}
	if err := builder.Question(question); err != nil {
		return nil, err
	}
	if err := builder.StartAnswers(); err != nil {
		return nil, err
	}
	rh := dnsmessage.ResourceHeader{
		Name:  question.Name,
		Class: dnsmessage.ClassINET,
		TTL:   ttl,
	}
	for _, addr := range addrs {
		switch question.Type {
		case dnsmessage.TypeA:
			if addr.Is4() {
				if err := builder.AResource(rh, dnsmessage.AResource{A: addr.As4()}); err != nil {
					return nil, err
				}
			}
		case dnsmessage.TypeAAAA:
			if addr.Is6() && !addr.Is4In6() {
				if err := builder.AAAAResource(rh, dnsmessage.AAAAResource{AAAA: addr.As16()}); err != nil {
					return nil, err
				}
			}
		}
	}
	return builder.Finish()
}

// reverseEntry is the IP→hostname affinity record: the server names resolved to an IP and
// the last time the IP saw activity — resolved, looked up for affinity, or seen as the
// source of a return packet — for idle TTL eviction (see ReverseTtl).
type reverseEntry struct {
	serverNames       []string
	lastActivityNanos int64
}

// reverseIndex maps a resolved ip to the server name(s) observed for it (from DNS
// resolution), for the multi-client's ServerName path affinity and block-action
// server-name reporting. It is self-contained — it holds no tun/mux and runs no
// background loop — so it can be constructed and driven directly in a test; the
// owning UpgradeMux records DoH resolutions into it (record), drives its idle
// eviction from its maintenance loop (evictIdle), and delegates its ServerNameLookup
// / ServerNamesLearnedNotifier methods to it.
type reverseIndex struct {
	// maxEntries returns the live hard cap on the map between TTL sweeps. Read per
	// insert (not snapshotted) so a settings change to ReverseMaxEntries applies
	// without rebuilding the index.
	maxEntries func() int

	lock             sync.Mutex
	entries          map[netip.Addr]reverseEntry
	learnedCallbacks *CallbackList[ServerNamesLearnedFunction]
}

func newReverseIndex(maxEntries func() int) *reverseIndex {
	return &reverseIndex{
		maxEntries:       maxEntries,
		entries:          map[netip.Addr]reverseEntry{},
		learnedCallbacks: NewCallbackList[ServerNamesLearnedFunction](),
	}
}

// record associates domain with each of addrs (a DoH resolution), refreshing their
// activity, and fires the learned callbacks for the ips that newly gained the name.
func (self *reverseIndex) record(addrs []netip.Addr, domain string) {
	maxEntries := self.maxEntries()
	now := time.Now().UnixNano()
	// the ips for which this domain was newly recorded (took the !found branch)
	var learned []netip.Addr
	func() {
		self.lock.Lock()
		defer self.lock.Unlock()
		for _, addr := range addrs {
			e, ok := self.entries[addr]
			if !ok && maxEntries <= len(self.entries) {
				// at the cap: make room by evicting the least-recently-active of a sample,
				// so a resolve burst cannot grow the map without bound between TTL sweeps
				self.evictOldestSampleLocked()
			}
			found := false
			for _, name := range e.serverNames {
				if name == domain {
					found = true
					break
				}
			}
			if !found {
				if maxServerNamesPerIp <= len(e.serverNames) {
					// keep the most recent names for a shared IP (CDN fronting many hosts):
					// drop the oldest
					copy(e.serverNames, e.serverNames[1:])
					e.serverNames[len(e.serverNames)-1] = domain
				} else {
					e.serverNames = append(e.serverNames, domain)
				}
				learned = append(learned, addr)
			}
			e.lastActivityNanos = now
			self.entries[addr] = e
		}
	}()
	// notify downstream (e.g. the multi-client's block-action decision caches) of
	// the newly-learned names, OUTSIDE the lock, so subsequent block actions for
	// these ips report the server name instead of the ip going forward
	if 0 < len(learned) {
		for _, callback := range self.learnedCallbacks.Get() {
			HandleError(func() {
				callback(learned)
			})
		}
	}
}

// addLearnedCallback registers a callback fired with the ips for which a new server
// name was just learned.
func (self *reverseIndex) addLearnedCallback(callback ServerNamesLearnedFunction) func() {
	callbackId := self.learnedCallbacks.Add(callback)
	return func() {
		self.learnedCallbacks.Remove(callbackId)
	}
}

// evictOldestSampleLocked deletes the least-recently-active record of a
// reverseEvictSampleSize sample (map iteration starts at a random bucket, so the
// sample is effectively random): approximate LRU at O(sample) per over-cap insert.
func (self *reverseIndex) evictOldestSampleLocked() {
	var oldestAddr netip.Addr
	var oldestNanos int64
	found := false
	i := 0
	for addr, e := range self.entries {
		if !found || e.lastActivityNanos < oldestNanos {
			oldestAddr = addr
			oldestNanos = e.lastActivityNanos
			found = true
		}
		i += 1
		if reverseEvictSampleSize <= i {
			break
		}
	}
	if found {
		delete(self.entries, oldestAddr)
	}
}

// serverNames returns the hostname(s) recorded for the given ip, for ServerName-based
// path affinity. Empty if none seen. Refreshes the record's activity.
func (self *reverseIndex) serverNames(ip string) []string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return nil
	}
	self.lock.Lock()
	defer self.lock.Unlock()
	e, ok := self.entries[addr]
	if !ok {
		return nil
	}
	// refresh activity so an IP that is actively routed keeps its affinity record
	e.lastActivityNanos = time.Now().UnixNano()
	self.entries[addr] = e
	return append([]string{}, e.serverNames...)
}

// touch refreshes the affinity record for ip — a return packet's source — so an IP with
// live (return) traffic keeps its IP→hostname affinity and is not idle-evicted. It is a
// no-op for an IP with no record (e.g. a direct-IP flow that was never resolved here).
func (self *reverseIndex) touch(ip net.IP) {
	addr, ok := netIPAddr(ip)
	if !ok {
		return
	}
	self.lock.Lock()
	defer self.lock.Unlock()
	if e, ok := self.entries[addr]; ok {
		e.lastActivityNanos = time.Now().UnixNano()
		self.entries[addr] = e
	}
}

// evictIdle drops affinity records idle at least ttl (none when ttl <= 0).
func (self *reverseIndex) evictIdle(ttl time.Duration) {
	if 0 < ttl {
		cutoff := time.Now().UnixNano() - int64(ttl)
		self.lock.Lock()
		defer self.lock.Unlock()
		for addr, e := range self.entries {
			if e.lastActivityNanos <= cutoff {
				delete(self.entries, addr)
			}
		}
	}
}

// shed drops the least-recently-active records under host memory pressure, keeping the
// most-recently-active half. A full clear would strip the names for every live flow
// (which re-look-up affinity only on a new flow, not per packet), flipping active
// downloads from by-name to by-IP routing and blanking the block-action host feed; the
// active records are the small, useful part of the map, so retaining them costs little
// while still releasing the bulk (idle, never-to-be-seen-again resolutions).
func (self *reverseIndex) shed() {
	self.lock.Lock()
	defer self.lock.Unlock()
	if len(self.entries) <= 1 {
		return
	}
	// keep the most-recently-active half: collect activity times, find the median, and
	// drop records at or below it (approximate — ties around the median may skew the
	// kept count slightly, which is fine for a memory-pressure shed).
	times := make([]int64, 0, len(self.entries))
	for _, e := range self.entries {
		times = append(times, e.lastActivityNanos)
	}
	slices.Sort(times)
	cutoff := times[len(times)/2]
	for addr, e := range self.entries {
		if e.lastActivityNanos < cutoff {
			delete(self.entries, addr)
		}
	}
}

// adoptFrom copies src's records into this index (keeping the more-recently-active on a
// key collision), so a freshly built index inherits the names a prior instance learned —
// e.g. across a mux rebuild on reconnect. Server names outlive the physical connection,
// so carrying them avoids blanking the host feed for every already-open flow after a
// reconnect. Callbacks are NOT copied (the new index has its own subscribers). No-op on a
// nil src.
func (self *reverseIndex) adoptFrom(src *reverseIndex) {
	if src == nil {
		return
	}
	// snapshot src under its lock, then merge under ours, so the two locks never nest.
	// serverNames is deep-copied in the snapshot: record mutates a capped entry's slice
	// in place, and src may still be recording (a draining prior mux) after the merge —
	// the adopted entries must not share backing arrays with it
	var srcEntries map[netip.Addr]reverseEntry
	func() {
		src.lock.Lock()
		defer src.lock.Unlock()
		srcEntries = make(map[netip.Addr]reverseEntry, len(src.entries))
		for addr, e := range src.entries {
			e.serverNames = append([]string{}, e.serverNames...)
			srcEntries[addr] = e
		}
	}()
	maxEntries := self.maxEntries()
	self.lock.Lock()
	defer self.lock.Unlock()
	for addr, e := range srcEntries {
		existing, ok := self.entries[addr]
		if ok && existing.lastActivityNanos >= e.lastActivityNanos {
			continue
		}
		if !ok && maxEntries <= len(self.entries) {
			self.evictOldestSampleLocked()
		}
		self.entries[addr] = e
	}
}

// count returns the number of records held (observability / tests).
func (self *reverseIndex) count() int {
	self.lock.Lock()
	defer self.lock.Unlock()
	return len(self.entries)
}

// reverseMaxEntries is the live hard cap on the reverse index between TTL sweeps, from
// the mux's DNS settings (ReverseMaxEntries), falling back to the default.
func (self *UpgradeMux) reverseMaxEntries() int {
	if dns := self.settings.Load().Dns; dns != nil && 0 < dns.ReverseMaxEntries {
		return dns.ReverseMaxEntries
	}
	return defaultReverseMaxEntries
}

// ServerNames returns the hostname(s) the mux has resolved to the given IP, for
// ServerName-based path affinity. Empty if none seen. Implements ServerNameLookup.
func (self *UpgradeMux) ServerNames(ip string) []string {
	return self.reverse.serverNames(ip)
}

// AddServerNamesLearnedCallback registers a callback fired with the ips for which
// a new server name was just learned (implements ServerNamesLearnedNotifier).
func (self *UpgradeMux) AddServerNamesLearnedCallback(callback ServerNamesLearnedFunction) func() {
	return self.reverse.addLearnedCallback(callback)
}

// reverseTtl is the configured IP→hostname affinity TTL (0 if unset/disabled).
func (self *UpgradeMux) reverseTtl() time.Duration {
	if dns := self.settings.Load().Dns; dns != nil {
		return dns.ReverseTtl
	}
	return 0
}

// run is the mux's lifecycle loop: it TTL-evicts the IP→hostname affinity map so it doesn't grow
// unbounded with every resolved IP. It ticks on the reverse TTL (a default cadence when disabled)
// and runs until the mux ctx is done.
func (self *UpgradeMux) run() {
	for {
		ttl := self.reverseTtl()
		interval := ttl
		if interval <= 0 {
			interval = 5 * time.Minute
		}
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(interval):
		}
		self.reverse.evictIdle(ttl)
	}
}

// SendPacket conforms to the UserNat send signature (the device's send route calls it).
func (self *UpgradeMux) SendPacket(source TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool {
	return self.mux.SendPacket(source, provideMode, packet, timeout)
}

// Receive is installed as the wrapped upstream's receive callback. A return packet's source
// is the server IP, so it refreshes that IP's affinity record — keeping a flow with live
// return traffic from being idle-evicted. The multi-client does not re-look-up affinity per
// packet (only on a new flow), so without this an active download's record would expire at
// the idle TTL and its routing would flip from base-domain to by-IP, breaking the flow.
func (self *UpgradeMux) Receive(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
	if ipPath != nil {
		self.reverse.touch(ipPath.SourceIp)
	}
	self.mux.Receive(source, provideMode, ipPath, packet)
}

// SetUpstream wires the wrapped upstream send (the remote UserNat).
func (self *UpgradeMux) SetUpstream(upstream IpMuxSend) {
	self.mux.SetUpstream(upstream)
}

// SetSettings updates the mux's DNS and HTTP policy at runtime. The tun's DohCache is
// rebuilt so a changed DNS resolution path takes effect immediately; HTTP mode changes
// apply to subsequent connections (the terminator binds lazily on the first HTTPS
// claim). settings must be non-nil — disabling the mux entirely is a teardown, not a
// settings change.
func (self *UpgradeMux) SetSettings(settings *UpgradeMuxSettings) {
	self.settings.Store(settings)
	self.mux.Tun().SetDnsResolverSettings(dnsResolverSettings(settings), dohRequestTimeout(settings))
	if replaced := self.fallbackDohCache.Swap(buildFallbackDohCache(fallbackResolverSettings(settings))); replaced != nil {
		// release the replaced cache's pooled connections now instead of holding them
		// (and their keepalive pings) until the idle timeout
		replaced.Close()
	}
}

// ShedMemory drops the mux's recoverable caches under host memory pressure: the resolver
// caches (with their pooled connections) fully, and the IP→hostname affinity map down to
// its most-recently-active half. The active affinity records are the small, useful part
// (live flows re-look-up affinity only on a new flow), so they are retained — a full drop
// would flip active flows to by-IP routing and blank the block-action host feed — while
// the idle bulk is released.
func (self *UpgradeMux) ShedMemory() {
	self.mux.Tun().DohCache().ShedMemory()
	if fallback := self.fallbackDohCache.Load(); fallback != nil {
		fallback.ShedMemory()
	}
	self.reverse.shed()
	self.sni.shed()
}

// AdoptServerNames seeds this mux's reverse index with the names a prior mux learned,
// so a rebuild (e.g. on reconnect / location change) doesn't blank the IP→hostname
// affinity — and the block-action host feed — for flows the OS keeps open across the
// reconnect (server names outlive the physical connection). No-op on a nil prior.
func (self *UpgradeMux) AdoptServerNames(prior *UpgradeMux) {
	if prior == nil {
		return
	}
	self.reverse.adoptFrom(prior.reverse)
}

func (self *UpgradeMux) Close() {
	self.cancel()
	self.unregisterShed()
	if fallback := self.fallbackDohCache.Load(); fallback != nil {
		fallback.Close()
	}
	self.mux.Close()
}
