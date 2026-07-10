package connect

// UpgradeMux is the concrete IpMux that intercepts local DNS (UDP/53) — resolving it over DoH
// that egresses the tunnel and recording the IP→hostname reverse index for ServerName path
// affinity — and applies the HTTP (TCP/80) policy: pass through to the egress, or drop. It
// wraps the remote UserNat (the exit path) and is held by the SDK device.

import (
	"context"
	"net"
	"net/netip"
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
			ResolveTimeout:     60 * time.Second,
			ResponseTtl:        60,
			ReverseTtl:         5 * time.Minute,
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
	// multi-client's ServerName path affinity (point 4).
	reverseLock sync.Mutex
	reverse     map[netip.Addr]reverseEntry
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
		reverse:     map[netip.Addr]reverseEntry{},
	}
	self.settings.Store(settings)
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
		self.recordServerNames(addrs, domain)
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
// passes through to the upstream. The claim decision is a pure function of (protocol, dst
// port), so it is read from a cheap, allocation-free header peek — only a claimed flow (or a
// header the peek can't classify, e.g. IPv6 extension headers) needs the allocating full
// parse. This keeps the pass-through bulk off the parse/allocation path entirely.
func (self *UpgradeMux) onSend(source TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool {
	switch peekClaim(packet) {
	case peekOther:
		return false // not a claimable flow — pass through without parsing
	case peekHttp:
		// block drops claimed plaintext HTTP; otherwise it passes through unchanged. neither
		// needs the full parse, so the pass-through :80 bulk stays off the allocating parse path.
		return self.httpBlocked()
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

// peekResult classifies a send packet by the flow the mux may claim, so the common pass-through
// bulk (peekOther) and pass-through TCP/80 (peekHttp, not blocking) are decided without the
// allocating full parse. Only DNS and unclassifiable packets parse.
type peekResult int

const (
	peekOther     peekResult = iota // not a claimable flow → pass through
	peekDns                         // UDP/53 (DNS)
	peekHttp                        // TCP/80 (HTTP)
	peekUndecided                   // header can't be classified cheaply → full parse
)

// peekClaim classifies a packet from the fixed IP/L4 header offsets without allocating
// (ParseIpPathWithPayload allocates the IpPath and an address backing per call). peekUndecided
// means the header can't be classified cheaply — IPv6 with extension headers, or a
// short/unsupported header — and the caller must fall back to the full parse.
func peekClaim(packet []byte) peekResult {
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
			if 80 == int(packet[ihl+2])<<8|int(packet[ihl+3]) {
				return peekHttp
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
			if 80 == int(packet[42])<<8|int(packet[43]) {
				return peekHttp
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
}

// handleDns claims a single A/AAAA DNS query and attaches it to the resolution
// pipeline for its question — joining the in-flight pipeline when one exists, else
// starting one (resolution can block on the network, so it runs asynchronously via
// the Tun's DohCache). The pipeline writes each attached client its own response and
// records the IP→hostname mapping. Other query types are not claimed and pass
// through to the upstream.
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
	var recordType string
	switch question.Type {
	case dnsmessage.TypeA:
		recordType = "A"
	case dnsmessage.TypeAAAA:
		recordType = "AAAA"
	default:
		return false
	}
	domain := strings.TrimSuffix(question.Name.String(), ".")

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
		self.startDnsPipeline(key, fl, recordType, domain)
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

	queryContext := func() (context.Context, context.CancelFunc) {
		if 0 < resolveTimeout {
			return context.WithTimeout(self.ctx, resolveTimeout)
		}
		return self.ctx, func() {}
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
		if responders == nil {
			return
		}
		if 0 < len(addrs) {
			self.recordServerNames(addrs, domain)
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
		self.inflightLock.Lock()
		defer self.inflightLock.Unlock()
		fl.workers -= 1
		if fl.workers == 0 && !fl.replied && self.inflight[key] == fl {
			delete(self.inflight, key)
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
	}()

	// primary: resolve over the tunnel-DoH (preferred — egresses the tunnel, no DNS leak), retrying
	// on the dnsRetryBackoff schedule. The first connect/TLS over a freshly-connecting tunnel can
	// take tens of seconds (the tun dial and TLS handshake budgets are 30s each, within the 60s
	// ResolveTimeout), so a slow attempt waits for the tunnel rather than failing fast.
	tunnelOk := make(chan struct{})
	go HandleError(func() {
		defer workerDone()
		queryCtx, queryCancel := queryContext()
		defer queryCancel()
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
			select {
			case <-time.After(localFallbackTimeout):
			case <-tunnelOk:
				return
			case <-self.ctx.Done():
				return
			}
			queryCtx, queryCancel := queryContext()
			defer queryCancel()
			addrs, authoritative := fallback.QueryResult(queryCtx, recordType, domain)
			reply(addrs, authoritative)
		})
	}
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

func (self *UpgradeMux) recordServerNames(addrs []netip.Addr, domain string) {
	maxEntries := defaultReverseMaxEntries
	if dns := self.settings.Load().Dns; dns != nil && 0 < dns.ReverseMaxEntries {
		maxEntries = dns.ReverseMaxEntries
	}
	now := time.Now().UnixNano()
	self.reverseLock.Lock()
	defer self.reverseLock.Unlock()
	for _, addr := range addrs {
		e, ok := self.reverse[addr]
		if !ok && maxEntries <= len(self.reverse) {
			// at the cap: make room by evicting the least-recently-active of a sample,
			// so a resolve burst cannot grow the map without bound between TTL sweeps
			self.evictOldestReverseSampleLocked()
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
		}
		e.lastActivityNanos = now
		self.reverse[addr] = e
	}
}

// evictOldestReverseSampleLocked deletes the least-recently-active record of a
// reverseEvictSampleSize sample (map iteration starts at a random bucket, so the
// sample is effectively random): approximate LRU at O(sample) per over-cap insert.
func (self *UpgradeMux) evictOldestReverseSampleLocked() {
	var oldestAddr netip.Addr
	var oldestNanos int64
	found := false
	i := 0
	for addr, e := range self.reverse {
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
		delete(self.reverse, oldestAddr)
	}
}

// ServerNames returns the hostname(s) the mux has resolved to the given IP, for
// ServerName-based path affinity. Empty if none seen. Implements ServerNameLookup.
func (self *UpgradeMux) ServerNames(ip string) []string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return nil
	}
	self.reverseLock.Lock()
	defer self.reverseLock.Unlock()
	e, ok := self.reverse[addr]
	if !ok {
		return nil
	}
	// refresh activity so an IP that is actively routed keeps its affinity record
	e.lastActivityNanos = time.Now().UnixNano()
	self.reverse[addr] = e
	return append([]string{}, e.serverNames...)
}

// touchServerNames refreshes the affinity record for ip — a return packet's source — so an
// IP with live (return) traffic keeps its IP→hostname affinity and is not idle-evicted. It
// is a no-op for an IP with no record (e.g. a direct-IP flow that was never resolved here).
func (self *UpgradeMux) touchServerNames(ip net.IP) {
	addr, ok := netIPAddr(ip)
	if !ok {
		return
	}
	self.reverseLock.Lock()
	defer self.reverseLock.Unlock()
	if e, ok := self.reverse[addr]; ok {
		e.lastActivityNanos = time.Now().UnixNano()
		self.reverse[addr] = e
	}
}

// reverseTtl is the configured IP→hostname affinity TTL (0 if unset/disabled).
func (self *UpgradeMux) reverseTtl() time.Duration {
	if dns := self.settings.Load().Dns; dns != nil {
		return dns.ReverseTtl
	}
	return 0
}

// evictReverse drops affinity records idle at least ttl (none when ttl <= 0).
func (self *UpgradeMux) evictReverse(ttl time.Duration) {
	if 0 < ttl {
		cutoff := time.Now().UnixNano() - int64(ttl)
		self.reverseLock.Lock()
		defer self.reverseLock.Unlock()
		for addr, e := range self.reverse {
			if e.lastActivityNanos <= cutoff {
				delete(self.reverse, addr)
			}
		}
	}
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
		self.evictReverse(ttl)
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
		self.touchServerNames(ipPath.SourceIp)
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

// ShedMemory drops the mux's recoverable caches — the resolver caches (with their pooled
// connections) and the IP→hostname affinity map — under host memory pressure. Subsequent
// queries re-resolve and re-dial; flows that re-look-up affinity fall back to by-IP routing
// until re-resolved.
func (self *UpgradeMux) ShedMemory() {
	self.mux.Tun().DohCache().ShedMemory()
	if fallback := self.fallbackDohCache.Load(); fallback != nil {
		fallback.ShedMemory()
	}
	self.reverseLock.Lock()
	defer self.reverseLock.Unlock()
	clear(self.reverse)
}

func (self *UpgradeMux) Close() {
	self.cancel()
	self.unregisterShed()
	if fallback := self.fallbackDohCache.Load(); fallback != nil {
		fallback.Close()
	}
	self.mux.Close()
}
