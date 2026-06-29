package connect

// UpgradeMux is the concrete IpMux that intercepts and (optionally) upgrades local
// DNS (UDP/53) and HTTP (TCP/80) egress. It wraps the remote UserNat (the exit
// path) and is held by the SDK device.
//
// In this step it is a PASS-THROUGH: onSend claims nothing, so all traffic flows to
// the upstream unchanged and behavior is identical to having no mux. Step 3 adds DNS
// interception/upgrade and the IP→hostname reverse lookup; step 4 adds HTTP.

import (
	"context"
	"crypto/tls"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/connect/protocol"
)

// HttpUpgradeMode selects how intercepted plaintext HTTP (TCP/80) is handled. HTTP
// always egresses remotely (there is no local-egress option). String-valued so it is
// readable in configs and logs; an empty/unrecognized value is treated as Unencrypted
// (pass-through).
type HttpUpgradeMode string

const (
	// HttpUpgradeUnencrypted egresses the HTTP request unencrypted, remotely.
	HttpUpgradeUnencrypted HttpUpgradeMode = "unencrypted"
	// HttpUpgradeHttps converts the request to HTTPS (modern tls.Config) and egresses remotely.
	HttpUpgradeHttps HttpUpgradeMode = "https"
	// HttpUpgradeHttpsFallback tries HTTPS and, on TLS-handshake failure, falls back to unencrypted HTTP.
	HttpUpgradeHttpsFallback HttpUpgradeMode = "https_fallback"
	// HttpUpgradeBlock drops the request.
	HttpUpgradeBlock HttpUpgradeMode = "block"
)

// HttpUpgradeSettings configures HTTP handling. The detailed shape is finalized in
// step 4; the Mode is the agreed behavior selector.
type HttpUpgradeSettings struct {
	Mode HttpUpgradeMode
	// HttpsPort is the port the HTTPS upgrade connects to (default 443; configurable for
	// non-standard HTTPS ports and for tests). 0 falls back to 443.
	HttpsPort int
	// TlsConfig, if set, is the base TLS config for the HTTPS upgrade (cert pinning in
	// production; trusting a local server's cert in tests). ServerName is set per
	// request. Not serialized.
	TlsConfig *tls.Config
	// UpgradeTimeout bounds the HTTPS dial + TLS handshake. On expiry the request falls
	// back to plaintext (HttpsFallback) or errors (Https), so a closed/filtered/stalled
	// :443 cannot hang the request (the mux ctx itself has no deadline). 0 means no bound.
	UpgradeTimeout time.Duration
	// UpgradeFailureHoldOff: after an HTTPS upgrade fails for a server name in
	// HttpsFallback mode, skip the upgrade for that name for this long and go straight to
	// plaintext — so a server without HTTPS on the original IP is not re-probed (and
	// re-stalled) on every request. It also serves as the TTL of the failure record: an
	// active maintenance goroutine clears records older than this. 0 disables the hold-off
	// (every request retries HTTPS).
	UpgradeFailureHoldOff time.Duration
	// NatIdleTimeout bounds how long an HTTP-upgrade NAT record (client flow → original
	// destination) is retained without traffic. The record is normally deleted when the
	// terminated connection closes; this TTL is the backstop that frees records for flows
	// that are DNAT'd but never establish (abandoned/half-open handshakes). 0 disables it.
	NatIdleTimeout time.Duration
}

func DefaultHttpUpgradeSettings() *HttpUpgradeSettings {
	return &HttpUpgradeSettings{
		Mode:                  HttpUpgradeUnencrypted,
		HttpsPort:             443,
		UpgradeTimeout:        5 * time.Second,
		UpgradeFailureHoldOff: 5 * time.Minute,
		NatIdleTimeout:        5 * time.Minute,
	}
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
// (TCP/80) is passed through to the egress unchanged. HTTP upgrade terminates the client
// connection and re-originates it from the mux, which changes the egress identity the origin
// sees and breaks IP-bound services (e.g. Pandora audio), so passthrough is the default and
// intended behavior; the HttpUpgrade* modes are slated for removal.
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
		Http: &HttpUpgradeSettings{
			// pass plaintext HTTP through to the egress unchanged. the HttpUpgrade* modes
			// terminate and re-originate the connection from the mux, changing the egress
			// identity and breaking IP-bound services (Pandora) — slated for removal. the fields
			// below apply only if Mode is set to an upgrade mode.
			Mode:                  HttpUpgradeUnencrypted,
			HttpsPort:             443,
			UpgradeTimeout:        5 * time.Second,
			UpgradeFailureHoldOff: 5 * time.Minute,
			NatIdleTimeout:        5 * time.Minute,
		},
	}
}

type UpgradeMux struct {
	ctx    context.Context
	cancel context.CancelFunc

	mux      *IpMux
	settings atomic.Pointer[UpgradeMuxSettings]

	// source/provideMode stamp packets the mux injects downstream (un-NAT'd HTTP replies).
	source      TransferPath
	provideMode protocol.ProvideMode

	// reverse maps a resolved IP to the hostname(s) the mux served for it, for the
	// multi-client's ServerName path affinity (point 4).
	reverseLock sync.Mutex
	reverse     map[netip.Addr]reverseEntry

	// HTTP upgrade (TCP/80) state. muxAddr is the internal stack's address: claimed
	// TCP/80 is DNAT'd to it for local termination, and it discriminates the stack's
	// client-bound replies (src muxAddr:80) from its own upstream connections in the
	// pump. httpNat maps a client (ip,port) to the original destination for un-NAT.
	muxAddr     netip.Addr
	httpOnce    sync.Once
	httpNatLock sync.Mutex
	httpNat     map[httpNatKey]httpNatEntry

	// httpsUpgradeFail records, per server name, the last time an HTTPS upgrade failed. In
	// HttpsFallback mode a recently-failed name skips the upgrade and goes straight to
	// plaintext (see HttpUpgradeSettings.UpgradeFailureHoldOff).
	httpsUpgradeFailLock sync.Mutex
	httpsUpgradeFail     map[string]time.Time

	// dialUpstream originates the mux's own connections (the HTTPS upgrade) through the
	// internal tun (→ pump → exit). Overridable in tests to reach a local server.
	dialUpstream func(ctx context.Context, network string, address string) (net.Conn, error)
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
	// the mux terminates client connections on its stack; an isolated stack ensures its
	// replies are emitted via the tun (pump → un-NAT → downstream) rather than delivered
	// intra-stack to a peer that happens to share the process-wide stack
	tunSettings.PrivateStack = true
	// the mux's connections (DoH, HTTP upgrade) run on a private stack inside a
	// memory-constrained host (notably the iOS network extension). Keep the gVisor
	// TCP/UDP buffers small (16KB) — Max applies per connection, so a large data-plane
	// buffer would blow the extension's memory limit under upgrade load. DoH responses and
	// the HTTP-upgrade request/response fit comfortably in 16KB.
	tunSettings.TcpReceiveBuffer = TcpBufferRange{Min: 4 * 1024, Default: 16 * 1024, Max: 16 * 1024}
	tunSettings.TcpSendBuffer = TcpBufferRange{Min: 4 * 1024, Default: 16 * 1024, Max: 16 * 1024}
	tunSettings.UdpReceiveBufferByteCount = 16 * 1024
	tunSettings.UdpSendBufferByteCount = 16 * 1024
	tun, err := CreateTunWithResolver(cancelCtx, tunSettings, dnsResolverSettings(settings))
	if err != nil {
		cancel()
		return nil, err
	}
	muxAddr := netip.Addr{}
	if addrs := tun.LocalAddresses(); 0 < len(addrs) {
		muxAddr = addrs[0]
	}
	self := &UpgradeMux{
		ctx:              cancelCtx,
		cancel:           cancel,
		source:           source,
		provideMode:      provideMode,
		reverse:          map[netip.Addr]reverseEntry{},
		muxAddr:          muxAddr,
		httpNat:          map[httpNatKey]httpNatEntry{},
		httpsUpgradeFail: map[string]time.Time{},
	}
	self.settings.Store(settings)
	self.mux = NewIpMux(cancelCtx, tun, source, provideMode, sendTimeout, self.onSend, self.onPump, initialReceiver, log)
	self.dialUpstream = self.mux.Tun().DialContext
	// active maintenance: TTL-evict the mux's accumulating state (upgrade-failure records,
	// the HTTP-upgrade NAT table, and the IP→hostname affinity map)
	go HandleError(self.run)
	return self, nil
}

// onSend claims and terminates intercepted DNS (UDP/53) and HTTP (TCP/80); everything else
// passes through to the upstream. The claim decision is a pure function of (protocol, dst
// port), so it is read from a cheap, allocation-free header peek — only a claimed flow (or a
// header the peek can't classify, e.g. IPv6 extension headers) needs the allocating full
// parse. This keeps the pass-through bulk off the parse/allocation path entirely.
func (self *UpgradeMux) onSend(source TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool {
	if claim, decided := peekClaim(packet); decided && !claim {
		return false
	}
	ipPath, payload, err := ParseIpPathWithPayload(packet)
	if err != nil {
		return false
	}
	switch {
	case IpProtocolUdp == ipPath.Protocol && 53 == ipPath.DestinationPort:
		if dns := self.settings.Load().Dns; dns == nil || dns.Resolver == nil {
			// DNS interception disabled; pass through to the egress
			return false
		}
		return self.handleDns(source, provideMode, ipPath, payload)
	case IpProtocolTcp == ipPath.Protocol && 80 == ipPath.DestinationPort:
		return self.handleHttp(source, provideMode, ipPath, packet)
	}
	return false
}

// peekClaim reports whether a packet targets a flow the mux claims (UDP/53 or TCP/80), read
// from the fixed IP/L4 header offsets without allocating (ParseIpPathWithPayload allocates
// the IpPath and an address backing per call). decided is false when the packet can't be
// classified cheaply — IPv6 with extension headers, or a short/unsupported header — and the
// caller must fall back to the full parse.
func peekClaim(packet []byte) (claim bool, decided bool) {
	if len(packet) < 20 {
		return false, false
	}
	switch packet[0] >> 4 {
	case 4:
		ihl := int(packet[0]&0x0f) * 4
		if ihl < 20 || len(packet) < ihl+4 {
			return false, false
		}
		switch packet[9] { // protocol
		case 6: // tcp
			dstPort := int(packet[ihl+2])<<8 | int(packet[ihl+3])
			return 80 == dstPort, true
		case 17: // udp
			dstPort := int(packet[ihl+2])<<8 | int(packet[ihl+3])
			return 53 == dstPort, true
		default:
			return false, true // not tcp/udp: never claimed
		}
	case 6:
		if len(packet) < 44 {
			return false, false
		}
		switch packet[6] { // next header
		case 6: // tcp
			dstPort := int(packet[42])<<8 | int(packet[43])
			return 80 == dstPort, true
		case 17: // udp
			dstPort := int(packet[42])<<8 | int(packet[43])
			return 53 == dstPort, true
		default:
			return false, false // extension header / other: needs the full parse
		}
	default:
		return false, false
	}
}

// handleHttp applies the HTTP upgrade policy to a TCP/80 packet:
//   - Unencrypted: pass through to the upstream (plaintext egress, unchanged)
//   - Block:       claim and drop
//   - Https / HttpsFallback: DNAT onto the internal stack and terminate, re-issuing
//     the request as HTTPS (see handleHttpUpgrade)
func (self *UpgradeMux) handleHttp(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) bool {
	mode := HttpUpgradeUnencrypted
	if settings := self.settings.Load(); settings.Http != nil {
		mode = settings.Http.Mode
	}
	switch mode {
	case HttpUpgradeBlock:
		return true
	case HttpUpgradeHttps, HttpUpgradeHttpsFallback:
		return self.handleHttpUpgrade(source, provideMode, ipPath, packet)
	default:
		return false
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
