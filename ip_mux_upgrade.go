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
	// ResolveTimeout bounds a single intercepted query's resolution; 0 means no bound
	// (rely on the resolver's own timeout). A slow query should reply (empty) quickly,
	// since clients retry fast and — under matchDomains — all DNS funnels through here.
	ResolveTimeout time.Duration
	// ResponseTtl is the TTL (seconds) set on synthesized DNS replies.
	ResponseTtl uint32
	// ReverseTtl bounds how long an IP→hostname affinity record (used for ServerName path
	// affinity) is retained after its last use. Refreshed whenever the IP is resolved or
	// looked up; an active maintenance goroutine evicts records idle longer than this so the
	// reverse map does not grow unbounded with every resolved IP. 0 disables eviction.
	ReverseTtl time.Duration
}

// UpgradeMuxSettings holds the DNS and HTTP upgrade policies. Each consumer (apps,
// server/proxy) sets its own; defaults are conservative.
type UpgradeMuxSettings struct {
	Dns  *DnsUpgradeSettings
	Http *HttpUpgradeSettings
}

// DefaultUpgradeMuxSettings is the app/device default: DNS (UDP/53) is intercepted and
// resolved over DoH that egresses the tunnel — DoH only, with no plaintext or off-tunnel
// fallback, so a DoH failure fails the query rather than leaking DNS — and plaintext HTTP
// (TCP/80) is passed through to the egress unchanged.
//
// For pure pass-through to the egress (the server/proxy use case — no DNS interception,
// no HTTP upgrade), do not install a mux at all: pass nil settings, which avoids a
// per-device tun/stack. A mux with nil Dns still passes DNS through but exists for HTTP.
func DefaultUpgradeMuxSettings() *UpgradeMuxSettings {
	return &UpgradeMuxSettings{
		Dns: &DnsUpgradeSettings{
			// DoH only, using the DoH client's configured servers, resolved through the
			// tun (egresses via the exit). Deliberately no EnableLocalDns/EnableLocalDoh
			// (host dialer) or EnableRemoteDns (plaintext :53), which would resolve
			// off-tunnel or in the clear.
			Resolver: &DnsResolverSettings{
				EnableRemoteDoh:      true,
				RemoteDohServersIpv4: DefaultDnsResolverSettings().RemoteDohServersIpv4,
				RemoteDohServersIpv6: DefaultDnsResolverSettings().RemoteDohServersIpv6,
			},
			ResolveTimeout: 5 * time.Second,
			ResponseTtl:    60,
			ReverseTtl:     5 * time.Minute,
		},
		Http: &HttpUpgradeSettings{Mode: HttpUpgradeUnencrypted},
	}
}

type UpgradeMux struct {
	ctx    context.Context
	cancel context.CancelFunc

	mux      *IpMux
	settings atomic.Pointer[UpgradeMuxSettings]

	// source/provideMode stamp packets the mux injects downstream (DNS replies).
	source      TransferPath
	provideMode protocol.ProvideMode

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
	// the mux's DoH connections run on an isolated stack so their packets are emitted via the
	// tun (pump → exit) rather than delivered intra-stack to a peer sharing the process-wide stack
	tunSettings.PrivateStack = true
	// the DoH connections run inside a memory-constrained host (notably the iOS network
	// extension). Keep the gVisor TCP/UDP buffers small (16KB) — Max applies per connection,
	// and DoH responses fit comfortably in 16KB.
	tunSettings.TcpReceiveBuffer = TcpBufferRange{Min: 4 * 1024, Default: 16 * 1024, Max: 16 * 1024}
	tunSettings.TcpSendBuffer = TcpBufferRange{Min: 4 * 1024, Default: 16 * 1024, Max: 16 * 1024}
	tunSettings.UdpReceiveBufferByteCount = 16 * 1024
	tunSettings.UdpSendBufferByteCount = 16 * 1024
	tun, err := CreateTunWithResolver(cancelCtx, tunSettings, dnsResolverSettings(settings))
	if err != nil {
		cancel()
		return nil, err
	}
	self := &UpgradeMux{
		ctx:         cancelCtx,
		cancel:      cancel,
		source:      source,
		provideMode: provideMode,
		reverse:     map[netip.Addr]reverseEntry{},
	}
	self.settings.Store(settings)
	self.mux = NewIpMux(cancelCtx, tun, source, provideMode, sendTimeout, self.onSend, nil, initialReceiver, log)
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

// handleDns claims a single A/AAAA DNS query and resolves it via the Tun's DohCache
// asynchronously (resolution can block on the network), writes the response back to
// the client, and records the IP→hostname mapping. Other query types are not claimed
// and pass through to the upstream.
//
// The parsed question is a value (dnsmessage copies the name into a fixed array), so
// nothing aliases the recycled packet buffer across the goroutine.
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
	id := header.ID
	// the response flows from the queried resolver back to the client
	reverse := ipPath.Reverse()

	var resolveTimeout time.Duration
	var responseTtl uint32
	if dns := self.settings.Load().Dns; dns != nil {
		resolveTimeout = dns.ResolveTimeout
		responseTtl = dns.ResponseTtl
	}

	go HandleError(func() {
		// hold the query until the resolver returns a real answer (records, or an authoritative
		// no-record), retrying within the resolve budget. on a freshly-connected tunnel the DoH
		// connection isn't up yet and the dial fails fast; without the retry we'd reply SERVFAIL
		// immediately and the client's lookups would fail until the tunnel settled. bounded by
		// ResolveTimeout so a genuinely-dead resolver still turns over to SERVFAIL (below).
		queryCtx := self.ctx
		if 0 < resolveTimeout {
			var queryCancel context.CancelFunc
			queryCtx, queryCancel = context.WithTimeout(self.ctx, resolveTimeout)
			defer queryCancel()
		}
		retryInterval := 250 * time.Millisecond
		const maxRetryInterval = 1 * time.Second
		var addrs []netip.Addr
		var authoritative bool
		for {
			addrs, authoritative = self.mux.Tun().DohCache().QueryResult(queryCtx, recordType, domain)
			if 0 < len(addrs) || authoritative {
				break
			}
			// resolution failed (not an authoritative no-record) — wait briefly and retry until
			// the budget is exhausted, so a query racing tunnel startup waits for DoH to come up
			select {
			case <-time.After(retryInterval):
			case <-queryCtx.Done():
			}
			if queryCtx.Err() != nil {
				break
			}
			// back off so a first-connect burst doesn't hammer the not-yet-ready tunnel
			retryInterval = min(2*retryInterval, maxRetryInterval)
		}
		if 0 < len(addrs) {
			self.recordServerNames(addrs, domain)
		}
		// the retries above were exhausted without a real answer — SERVFAIL so the client retries;
		// replying NOERROR with no answers would be read as an authoritative "no address" and the
		// client would give up
		rcode := dnsmessage.RCodeSuccess
		if len(addrs) == 0 && !authoritative {
			rcode = dnsmessage.RCodeServerFailure
		}
		respPayload, err := buildDnsResponse(id, question, addrs, responseTtl, rcode)
		if err != nil {
			return
		}
		self.mux.deliverDownstream(source, provideMode, reverse, ipOosPacket(reverse, respPayload))
	})
	return true
}

func buildDnsResponse(id uint16, question dnsmessage.Question, addrs []netip.Addr, ttl uint32, rcode dnsmessage.RCode) ([]byte, error) {
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:                 id,
		Response:           true,
		RecursionAvailable: true,
		RCode:              rcode,
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
	now := time.Now().UnixNano()
	self.reverseLock.Lock()
	defer self.reverseLock.Unlock()
	for _, addr := range addrs {
		e := self.reverse[addr]
		found := false
		for _, name := range e.serverNames {
			if name == domain {
				found = true
				break
			}
		}
		if !found {
			e.serverNames = append(e.serverNames, domain)
		}
		e.lastActivityNanos = now
		self.reverse[addr] = e
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
	self.mux.Tun().SetDnsResolverSettings(dnsResolverSettings(settings))
}

func (self *UpgradeMux) Close() {
	self.cancel()
	self.mux.Close()
}
