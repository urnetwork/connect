package connect

// Deterministic regressions for DNS inside the tunnel when an exit stalls
// (the owner's iOS logs of 2026-11-13: in-tunnel DoH timed out on up to a
// third of lookups, 60s each, nearly all in minutes with an exit failure).
// Each test is named for the failure it pins. The DoH servers are synthetic:
// a fake transport answers by name, and each server's connection rides a
// synthetic exit that can stall or carry no ipv6. The mux is wired as
// NewUpgradeMux wires it, minus the network stack, and runs in a synctest
// bubble, so every time below is exact.

import (
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/http2"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Synthetic tunnel DoH servers at documentation addresses. Outside the
// built-in endpoints, so no policy guard applies, and outside the known
// operator pairs, so the n-th ipv4 server pairs with the n-th ipv6 server.
const (
	dnsFailoverServerA     = "https://192.0.2.11/dns-query"
	dnsFailoverServerB     = "https://192.0.2.12/dns-query"
	dnsFailoverServerC     = "https://192.0.2.13/dns-query"
	dnsFailoverServerD     = "https://192.0.2.14/dns-query"
	dnsFailoverServerA6    = "https://[2001:db8::11]/dns-query"
	dnsFailoverServerB6    = "https://[2001:db8::12]/dns-query"
	dnsFailoverClientIp    = "198.51.100.7"
	dnsFailoverStandInIp   = "198.51.100.53"
	dnsFailoverStalledExit = "exit-stalled"
	dnsFailoverHealthyExit = "exit-healthy"
)

// The answer the synthetic servers give a name: its addresses (for A and
// AAAA) and the ipv4hint of its HTTPS record, at ttl.
type dnsFailoverAnswer struct {
	addrs     []netip.Addr
	httpsHint netip.Addr
	ttl       uint32
}

// One request a synthetic server received.
type dnsFailoverRequest struct {
	host      string
	name      string
	qType     dnsmessage.Type
	startTime time.Time
}

// Synthetic DoH servers behind a fake transport. Each server's connection
// rides one synthetic exit (exitByHost). A stalled exit holds every request on
// it until the exit is restored, when they answer, or until the request ends;
// an exit that carries no ipv6 holds an ipv6 server's requests until they end,
// as a dial no exit can carry waits out its timeout. Every request is recorded.
type dnsFailoverServers struct {
	t *testing.T

	stateLock sync.Mutex
	// the exit each server's connection rides, by url host
	exitByHost map[string]string
	// a stalled exit's release, closed when it is restored
	stalledExitReleases map[string]chan struct{}
	ipv4OnlyExits       map[string]bool
	answerByName        map[string]dnsFailoverAnswer
	requests            []dnsFailoverRequest
}

func newDnsFailoverServers(t *testing.T) *dnsFailoverServers {
	return &dnsFailoverServers{
		t:                   t,
		exitByHost:          map[string]string{},
		stalledExitReleases: map[string]chan struct{}{},
		ipv4OnlyExits:       map[string]bool{},
		answerByName:        map[string]dnsFailoverAnswer{},
	}
}

// Puts the connection of the server at dohUrl on exit.
func (self *dnsFailoverServers) ride(dohUrl string, exit string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.exitByHost[dnsFailoverHost(self.t, dohUrl)] = exit
}

func (self *dnsFailoverServers) answer(name string, answer dnsFailoverAnswer) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.answerByName[name] = answer
}

func (self *dnsFailoverServers) stall(exit string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if _, ok := self.stalledExitReleases[exit]; !ok {
		self.stalledExitReleases[exit] = make(chan struct{})
	}
}

// Ends a stall: the requests it held answer.
func (self *dnsFailoverServers) restore(exit string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if release, ok := self.stalledExitReleases[exit]; ok {
		close(release)
		delete(self.stalledExitReleases, exit)
	}
}

func (self *dnsFailoverServers) carryNoIpv6(exit string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.ipv4OnlyExits[exit] = true
}

// The exit the connection of the server at host rides.
func (self *dnsFailoverServers) exitOf(host string) string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.exitByHost[host]
}

func (self *dnsFailoverServers) recorded() []dnsFailoverRequest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.requests)
}

// The requests recorded for a name.
func (self *dnsFailoverServers) requestsFor(name string) []dnsFailoverRequest {
	var requests []dnsFailoverRequest
	for _, request := range self.recorded() {
		if request.name == name {
			requests = append(requests, request)
		}
	}
	return requests
}

func (self *dnsFailoverServers) roundTrip(request *http.Request) (*http.Response, error) {
	query := testingDohRequestQuery(self.t, request)
	var p dnsmessage.Parser
	header, err := p.Start(query)
	if err != nil {
		return nil, err
	}
	question, err := p.Question()
	if err != nil {
		return nil, err
	}
	host := request.URL.Hostname()
	var hold chan struct{}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.requests = append(self.requests, dnsFailoverRequest{
			host:      host,
			name:      strings.TrimSuffix(question.Name.String(), "."),
			qType:     question.Type,
			startTime: time.Now(),
		})
		exit := self.exitByHost[host]
		if release, ok := self.stalledExitReleases[exit]; ok {
			hold = release
		}
		if self.ipv4OnlyExits[exit] && dnsFailoverHostIs6(host) {
			// never released: no exit carries it
			hold = make(chan struct{})
		}
	}()
	if hold != nil {
		select {
		case <-hold:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
	}
	var answer dnsFailoverAnswer
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		answer = self.answerByName[strings.TrimSuffix(question.Name.String(), ".")]
	}()
	recorder := httptest.NewRecorder()
	recorder.Header().Set("Content-Type", "application/dns-message")
	recorder.Write(dnsFailoverResponse(self.t, header.ID, question, answer))
	response := recorder.Result()
	response.Request = request
	return response, nil
}

// The DNS response of a synthetic server to question.
func dnsFailoverResponse(t *testing.T, id uint16, question dnsmessage.Question, answer dnsFailoverAnswer) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, Response: true, RecursionAvailable: true})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := b.Question(question); err != nil {
		t.Fatal(err)
	}
	if err := b.StartAnswers(); err != nil {
		t.Fatal(err)
	}
	rh := dnsmessage.ResourceHeader{Name: question.Name, Class: dnsmessage.ClassINET, TTL: answer.ttl}
	switch question.Type {
	case dnsmessage.TypeA:
		for _, addr := range answer.addrs {
			if addr.Is4() {
				if err := b.AResource(rh, dnsmessage.AResource{A: addr.As4()}); err != nil {
					t.Fatal(err)
				}
			}
		}
	case dnsmessage.TypeAAAA:
		for _, addr := range answer.addrs {
			if addr.Is6() {
				if err := b.AAAAResource(rh, dnsmessage.AAAAResource{AAAA: addr.As16()}); err != nil {
					t.Fatal(err)
				}
			}
		}
	case dnsTypeHttps:
		if answer.httpsHint.IsValid() {
			var r dnsmessage.HTTPSResource
			r.Priority = 1
			r.Target = dnsmessage.MustNewName(".")
			hint := answer.httpsHint.As4()
			r.SetParam(dnsmessage.SVCParamIPv4Hint, hint[:])
			if err := b.HTTPSResource(rh, r); err != nil {
				t.Fatal(err)
			}
		}
	}
	response, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return response
}

// The servers asked, each with when, for a failure message.
func dnsFailoverAsked(requests []dnsFailoverRequest, startTime time.Time) string {
	asked := []string{}
	for _, request := range requests {
		asked = append(asked, request.host+" at "+request.startTime.Sub(startTime).String())
	}
	return "[" + strings.Join(asked, ", ") + "]"
}

func dnsFailoverHost(t *testing.T, dohUrl string) string {
	t.Helper()
	request, err := http.NewRequest("GET", dohUrl, nil)
	if err != nil {
		t.Fatal(err)
	}
	return request.URL.Hostname()
}

func dnsFailoverHostIs6(host string) bool {
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Is6()
}

// One answer the mux delivered to the client, with when it left.
type dnsFailoverReply struct {
	payload []byte
	time    time.Time
}

// A mux wired as NewUpgradeMux wires it (newUpgradeMuxState,
// upgradeMuxTunSettings, tunnelDohSettings, and the tun's own buildDohCache),
// without the network stack: its tunnel DoH cache asks the synthetic servers,
// and the answers it delivers are recorded. Build it inside a synctest bubble
// and close it before the bubble ends.
type dnsFailoverMux struct {
	t       *testing.T
	mux     *UpgradeMux
	cache   *DohCache
	servers *dnsFailoverServers

	stateLock sync.Mutex
	replies   []dnsFailoverReply
}

// The default mux settings (no host fallback) with the given tunnel DoH
// servers, adjusted by configure.
func newDnsFailoverMux(t *testing.T, servers *dnsFailoverServers, dohUrlsIpv4 []string, dohUrlsIpv6 []string, configure func(settings *UpgradeMuxSettings)) *dnsFailoverMux {
	t.Helper()
	settings := DefaultUpgradeMuxSettings()
	settings.Dns.Resolver = &DnsResolverSettings{
		EnableRemoteDoh:       true,
		DnsUpgradeMaskAddress: DefaultDnsUpgradeMaskAddress,
		RemoteDohUrlsIpv4:     dohUrlsIpv4,
		RemoteDohUrlsIpv6:     dohUrlsIpv6,
	}
	settings.Dns.Fallback = nil
	if configure != nil {
		configure(settings)
	}
	log := NewNoopLogger()
	ctx, cancel := context.WithCancel(context.Background())
	mux := newUpgradeMuxState(ctx, cancel, TransferPath{}, protocol.ProvideMode_Network, settings)
	tunSettings := upgradeMuxTunSettings(settings, log)
	tunSettings.DohSettings = mux.tunnelDohSettings(settings)
	tunSettings.DohRequestTimeout = dohRequestTimeout(settings)
	tun := &Tun{
		ctx:         ctx,
		cancel:      cancel,
		log:         log,
		settings:    tunSettings,
		ipv6Enabled: true,
	}
	cache := tun.buildDohCache(dnsResolverSettings(settings), tunSettings.DohRequestTimeout)
	cache.remoteClient.httpClient.Transport = dohRoundTripperFunc(servers.roundTrip)
	tun.dohResolver.Store(cache)
	harness := &dnsFailoverMux{
		t:       t,
		mux:     mux,
		cache:   cache,
		servers: servers,
	}
	mux.mux = &IpMux{
		ctx:            ctx,
		cancel:         cancel,
		log:            log,
		tun:            tun,
		onSend:         mux.onSend,
		receivers:      NewCallbackList[ReceivePacketFunction](),
		batchReceivers: NewCallbackList[ReceivePacketsFunction](),
	}
	mux.mux.receivers.Add(harness.receive)
	// an inactive timeline: no process-wide log worker in the bubble
	mux.firstLoad = &firstLoadTimeline{}
	mux.unregisterShed = func() {}
	return harness
}

// Forces the order the tunnel cache tries its servers in: the earlier a url
// in dohUrls, the earlier it is tried among servers of equal score.
func (self *dnsFailoverMux) order(dohUrls ...string) {
	draws := map[string]float64{}
	for i, dohUrl := range dohUrls {
		draws[dohUrl] = 0.9 - 0.1*float64(i)
	}
	stats := self.cache.remoteClient.stats
	stats.lock.Lock()
	defer stats.lock.Unlock()
	stats.draw = func(dohUrl string) float64 {
		return draws[dohUrl]
	}
}

func (self *dnsFailoverMux) receive(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
	_, payload, err := ParseIpPathWithPayload(packet)
	if err != nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.replies = append(self.replies, dnsFailoverReply{payload: slices.Clone(payload), time: time.Now()})
}

// Sends one client query over UDP/53, from its own port.
func (self *dnsFailoverMux) send(name string, qType dnsmessage.Type, id uint16) {
	self.t.Helper()
	query := testingDnsClientQuery(self.t, name+".", qType, id, false)
	packet := ipOosPacket(&IpPath{
		Version:         4,
		Protocol:        IpProtocolUdp,
		SourceIp:        net.ParseIP(dnsFailoverClientIp),
		SourcePort:      30000 + int(id),
		DestinationIp:   net.ParseIP(dnsFailoverStandInIp),
		DestinationPort: 53,
	}, query)
	if !self.mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, packet, 0) {
		self.t.Fatalf("the query for %s was not claimed", name)
	}
}

// The answer to the query with id, if one left.
func (self *dnsFailoverMux) reply(id uint16) (dnsFailoverReply, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, reply := range self.replies {
		if 2 <= len(reply.payload) && binary.BigEndian.Uint16(reply.payload[:2]) == id {
			return reply, true
		}
	}
	return dnsFailoverReply{}, false
}

// Ends the mux and its cache and joins every worker in the bubble, past the
// http client's own request timer, which a canceled request's unread body
// leaves armed.
func (self *dnsFailoverMux) close() {
	self.mux.cancel()
	self.cache.Close()
	time.Sleep(2 * dohRequestTimeout(self.mux.settings.Load()))
	synctest.Wait()
}

// Runs f in a synctest bubble, the process-wide message pools (and their
// stats worker) built outside it first.
func dnsFailoverSynctest(t *testing.T, f func(t *testing.T)) {
	t.Helper()
	orderedMessagePools()
	synctest.Test(t, f)
}

// What one answer says: its response code, its A/AAAA addresses and HTTPS
// ipv4hints, and the least ttl of its records.
type dnsFailoverParsed struct {
	rcode  dnsmessage.RCode
	addrs  []netip.Addr
	hints  []netip.Addr
	minTtl uint32
	count  int
}

func dnsFailoverParse(t *testing.T, payload []byte) dnsFailoverParsed {
	t.Helper()
	var message dnsmessage.Message
	if err := message.Unpack(payload); err != nil {
		t.Fatalf("answer does not parse: %v", err)
	}
	parsed := dnsFailoverParsed{rcode: message.Header.RCode}
	for i, answer := range message.Answers {
		if i == 0 || answer.Header.TTL < parsed.minTtl {
			parsed.minTtl = answer.Header.TTL
		}
		parsed.count += 1
		switch body := answer.Body.(type) {
		case *dnsmessage.AResource:
			parsed.addrs = append(parsed.addrs, netip.AddrFrom4(body.A))
		case *dnsmessage.AAAAResource:
			parsed.addrs = append(parsed.addrs, netip.AddrFrom16(body.AAAA))
		case *dnsmessage.HTTPSResource:
			if hint, ok := body.GetParam(dnsmessage.SVCParamIPv4Hint); ok && len(hint) == 4 {
				parsed.hints = append(parsed.hints, netip.AddrFrom4([4]byte(hint)))
			}
		}
	}
	return parsed
}

// Answers one query while the servers are healthy, which proves the tunnel
// DoH path: the mux is warm after it (DnsUpgradeSettings.WarmAttemptTimeout).
func (self *dnsFailoverMux) warm(id uint16) {
	self.t.Helper()
	self.servers.answer("warm.example", dnsFailoverAnswer{addrs: []netip.Addr{netip.MustParseAddr("203.0.113.1")}, ttl: 300})
	self.send("warm.example", dnsmessage.TypeA, id)
	synctest.Wait()
	if _, ok := self.reply(id); !ok {
		self.t.Fatal("the warm-up query was not answered by healthy servers")
	}
}

// Root cause: the per-request timeout of a tunnel DoH lookup was the whole
// 60s flight budget, and the flight retried only after an attempt returned,
// so the exit that carried the first servers' connections held the lookup
// for the whole budget once it stalled. Pinned: on a warm tunnel the attempt
// ends at the cap and the retry answers through a server on another exit
// within 5s.
func TestDnsWarmTunnelStalledExitAnswersThroughAnotherPathWithinFiveSeconds(t *testing.T) {
	dnsFailoverSynctest(t, func(t *testing.T) {
		servers := newDnsFailoverServers(t)
		servers.ride(dnsFailoverServerA, dnsFailoverStalledExit)
		servers.ride(dnsFailoverServerB, dnsFailoverStalledExit)
		servers.ride(dnsFailoverServerC, dnsFailoverHealthyExit)
		servers.ride(dnsFailoverServerD, dnsFailoverHealthyExit)
		harness := newDnsFailoverMux(t, servers, []string{dnsFailoverServerA, dnsFailoverServerB, dnsFailoverServerC, dnsFailoverServerD}, nil, nil)
		defer harness.close()
		harness.order(dnsFailoverServerA, dnsFailoverServerB, dnsFailoverServerC, dnsFailoverServerD)
		harness.warm(0x0101)

		servers.stall(dnsFailoverStalledExit)
		answer := netip.MustParseAddr("203.0.113.21")
		servers.answer("stalled.example", dnsFailoverAnswer{addrs: []netip.Addr{answer}, ttl: 300})
		startTime := time.Now()
		harness.send("stalled.example", dnsmessage.TypeA, 0x0102)
		time.Sleep(5 * time.Second)
		synctest.Wait()

		reply, ok := harness.reply(0x0102)
		if !ok {
			t.Fatalf("DNS_STALLED_EXIT_HELD_THE_WHOLE_BUDGET: no answer 5s after the exit stalled; asked %s", dnsFailoverAsked(servers.requestsFor("stalled.example"), startTime))
		}
		parsed := dnsFailoverParse(t, reply.payload)
		if elapsed := reply.time.Sub(startTime); 5*time.Second < elapsed || len(parsed.addrs) != 1 || parsed.addrs[0] != answer {
			t.Fatalf("answer after %v with %v, want %s within 5s", elapsed, parsed.addrs, answer)
		}
		answeredOnHealthyExit := false
		for _, request := range servers.requestsFor("stalled.example") {
			if servers.exitOf(request.host) == dnsFailoverHealthyExit {
				answeredOnHealthyExit = true
			}
		}
		if !answeredOnHealthyExit {
			t.Fatal("the retry did not go through a server on another exit")
		}
	})
}

// Root cause: during a stall an A or AAAA lookup got no reply at all for the
// whole 60s budget, and a forwarded type (HTTPS) got SERVFAIL when its single
// attempt ran out. Pinned: on a warm tunnel both get their NOERROR answer,
// through another exit, within 5s.
func TestDnsStalledExitGivesAddressAndHttpsAnswersNotSilenceOrServfail(t *testing.T) {
	dnsFailoverSynctest(t, func(t *testing.T) {
		servers := newDnsFailoverServers(t)
		servers.ride(dnsFailoverServerA, dnsFailoverStalledExit)
		servers.ride(dnsFailoverServerB, dnsFailoverStalledExit)
		servers.ride(dnsFailoverServerC, dnsFailoverHealthyExit)
		servers.ride(dnsFailoverServerD, dnsFailoverHealthyExit)
		harness := newDnsFailoverMux(t, servers, []string{dnsFailoverServerA, dnsFailoverServerB, dnsFailoverServerC, dnsFailoverServerD}, nil, nil)
		defer harness.close()
		harness.order(dnsFailoverServerA, dnsFailoverServerB, dnsFailoverServerC, dnsFailoverServerD)
		harness.warm(0x0201)

		servers.stall(dnsFailoverStalledExit)
		answer := netip.MustParseAddr("203.0.113.22")
		hint := netip.MustParseAddr("203.0.113.23")
		servers.answer("page.example", dnsFailoverAnswer{addrs: []netip.Addr{answer}, httpsHint: hint, ttl: 300})
		startTime := time.Now()
		harness.send("page.example", dnsmessage.TypeA, 0x0202)
		harness.send("page.example", dnsTypeHttps, 0x0203)
		time.Sleep(5 * time.Second)
		synctest.Wait()

		addressReply, addressOk := harness.reply(0x0202)
		httpsReply, httpsOk := harness.reply(0x0203)
		describe := func(reply dnsFailoverReply, ok bool) string {
			if !ok {
				return "no reply"
			}
			parsed := dnsFailoverParse(t, reply.payload)
			return parsed.rcode.String() + " after " + reply.time.Sub(startTime).String()
		}
		if !addressOk || !httpsOk {
			t.Fatalf("DNS_STALL_SILENCE_OR_SERVFAIL: 5s into the stall A got %s, HTTPS got %s", describe(addressReply, addressOk), describe(httpsReply, httpsOk))
		}
		addressParsed := dnsFailoverParse(t, addressReply.payload)
		if addressParsed.rcode != dnsmessage.RCodeSuccess || len(addressParsed.addrs) != 1 || addressParsed.addrs[0] != answer {
			t.Fatalf("A answer %s %v, want NOERROR %s", addressParsed.rcode, addressParsed.addrs, answer)
		}
		httpsParsed := dnsFailoverParse(t, httpsReply.payload)
		if httpsParsed.rcode != dnsmessage.RCodeSuccess || len(httpsParsed.hints) != 1 || httpsParsed.hints[0] != hint {
			t.Fatalf("HTTPS answer %s %v, want NOERROR with hint %s", httpsParsed.rcode, httpsParsed.hints, hint)
		}
		for _, reply := range []dnsFailoverReply{addressReply, httpsReply} {
			if elapsed := reply.time.Sub(startTime); 5*time.Second < elapsed {
				t.Fatalf("answer after %v, want within 5s", elapsed)
			}
		}
	})
}

// Root cause: the in-flight question cap was checked before the resolver
// cache, so at the cap a name the cache held was dropped like a new one, and
// the client waited out its own retry. Pinned: a cached name is answered at
// once while the cap is full.
func TestDnsCachedNameIsAnsweredWhenTheInflightCapIsFull(t *testing.T) {
	dnsFailoverSynctest(t, func(t *testing.T) {
		servers := newDnsFailoverServers(t)
		servers.ride(dnsFailoverServerA, dnsFailoverStalledExit)
		servers.ride(dnsFailoverServerB, dnsFailoverStalledExit)
		harness := newDnsFailoverMux(t, servers, []string{dnsFailoverServerA, dnsFailoverServerB}, nil, func(settings *UpgradeMuxSettings) {
			settings.Dns.MaxInflightQueries = 2
		})
		defer harness.close()
		cached := netip.MustParseAddr("203.0.113.31")
		servers.answer("cached.example", dnsFailoverAnswer{addrs: []netip.Addr{cached}, ttl: 300})
		harness.send("cached.example", dnsmessage.TypeA, 0x0301)
		synctest.Wait()
		if _, ok := harness.reply(0x0301); !ok {
			t.Fatal("the first lookup of the cached name was not answered")
		}

		// two new names fill the cap behind a stalled exit
		servers.stall(dnsFailoverStalledExit)
		harness.send("fill-one.example", dnsmessage.TypeA, 0x0302)
		harness.send("fill-two.example", dnsmessage.TypeA, 0x0303)
		synctest.Wait()
		harness.send("cached.example", dnsmessage.TypeA, 0x0304)
		synctest.Wait()

		reply, ok := harness.reply(0x0304)
		if !ok {
			t.Fatal("DNS_CACHED_NAME_DROPPED_AT_INFLIGHT_CAP: a name the cache holds got no answer while the cap was full")
		}
		if parsed := dnsFailoverParse(t, reply.payload); len(parsed.addrs) != 1 || parsed.addrs[0] != cached {
			t.Fatalf("cached answer %v, want %s", parsed.addrs, cached)
		}
	})
}

// Root cause: the tunnel DoH cache dialed its ipv6 servers whatever the exits
// carried, so with exits that carry no ipv6 a lookup waited out a dial no exit
// could carry (30s each on the owner's run 1). Pinned: while the multi client
// reports no exit can carry ipv6, no ipv6 server is asked, and an ipv4 server
// answers at once.
func TestDnsIpv6EndpointIsNeverDialedThroughV4OnlyExits(t *testing.T) {
	dnsFailoverSynctest(t, func(t *testing.T) {
		servers := newDnsFailoverServers(t)
		for _, dohUrl := range []string{dnsFailoverServerA, dnsFailoverServerB, dnsFailoverServerA6, dnsFailoverServerB6} {
			servers.ride(dohUrl, dnsFailoverHealthyExit)
		}
		servers.carryNoIpv6(dnsFailoverHealthyExit)
		harness := newDnsFailoverMux(t, servers, []string{dnsFailoverServerA, dnsFailoverServerB}, []string{dnsFailoverServerA6, dnsFailoverServerB6}, nil)
		defer harness.close()
		harness.mux.SetIpv6Unroutable(func() bool { return true })
		harness.order(dnsFailoverServerA6, dnsFailoverServerB6, dnsFailoverServerA, dnsFailoverServerB)

		answer := netip.MustParseAddr("203.0.113.41")
		servers.answer("v4only.example", dnsFailoverAnswer{addrs: []netip.Addr{answer}, ttl: 300})
		startTime := time.Now()
		harness.send("v4only.example", dnsmessage.TypeA, 0x0401)
		time.Sleep(time.Second)
		synctest.Wait()

		for _, request := range servers.requestsFor("v4only.example") {
			if dnsFailoverHostIs6(request.host) {
				t.Fatalf("DNS_IPV6_ENDPOINT_DIALED_THROUGH_V4_ONLY_EXIT: ipv6 server %s asked at %v while no exit carries ipv6", request.host, request.startTime.Sub(startTime))
			}
		}
		reply, ok := harness.reply(0x0401)
		if !ok {
			t.Fatal("no answer within 1s from the ipv4 servers")
		}
		if elapsed := reply.time.Sub(startTime); elapsed != 0 {
			t.Fatalf("answer after %v, want at once", elapsed)
		}
	})
}

// Root cause: a fan-out took the first two servers of its order whatever
// their family, so when ipv6 servers ranked first and the exits carrying ipv6
// black-holed it, the whole lookup waited on two ipv6 servers while every
// ipv4 server would have answered. Pinned: the first fan-out keeps an ipv4
// server and answers at once.
func TestDnsFanOutKeepsAnIpv4ServerWhenIpv6ServersRankFirst(t *testing.T) {
	dnsFailoverSynctest(t, func(t *testing.T) {
		servers := newDnsFailoverServers(t)
		for _, dohUrl := range []string{dnsFailoverServerA, dnsFailoverServerB, dnsFailoverServerA6, dnsFailoverServerB6} {
			servers.ride(dohUrl, dnsFailoverHealthyExit)
		}
		// labelled to carry ipv6, but the ipv6 it is given goes nowhere
		servers.carryNoIpv6(dnsFailoverHealthyExit)
		harness := newDnsFailoverMux(t, servers, []string{dnsFailoverServerA, dnsFailoverServerB}, []string{dnsFailoverServerA6, dnsFailoverServerB6}, nil)
		defer harness.close()
		harness.mux.SetIpv6Unroutable(func() bool { return false })
		harness.order(dnsFailoverServerA6, dnsFailoverServerB6, dnsFailoverServerA, dnsFailoverServerB)

		answer := netip.MustParseAddr("203.0.113.42")
		servers.answer("dualstack.example", dnsFailoverAnswer{addrs: []netip.Addr{answer}, ttl: 300})
		startTime := time.Now()
		harness.send("dualstack.example", dnsmessage.TypeA, 0x0451)
		time.Sleep(time.Second)
		synctest.Wait()

		reply, ok := harness.reply(0x0451)
		if !ok {
			t.Fatalf("DNS_FAN_OUT_OF_IPV6_ONLY_WAITED: no answer 1s into a lookup whose servers carried ipv6 nowhere; asked %s", dnsFailoverAsked(servers.requestsFor("dualstack.example"), startTime))
		}
		if elapsed := reply.time.Sub(startTime); elapsed != 0 {
			t.Fatalf("answer after %v, want at once", elapsed)
		}
	})
}

// Root cause: a stale answer (RFC 8767) was served only after the live
// lookup failed, which a stalled exit put off for the whole 60s budget, so
// the client waited although the cache held the name. Pinned: the retained
// answer goes out 1.5s into a hanging lookup at a 30s TTL, and the live
// lookup goes on: when the exit recovers the fresh answer replaces it.
func TestDnsStaleAnswerIsServedWhileTheLiveLookupHangs(t *testing.T) {
	dnsFailoverSynctest(t, func(t *testing.T) {
		servers := newDnsFailoverServers(t)
		servers.ride(dnsFailoverServerA, dnsFailoverStalledExit)
		servers.ride(dnsFailoverServerB, dnsFailoverStalledExit)
		harness := newDnsFailoverMux(t, servers, []string{dnsFailoverServerA, dnsFailoverServerB}, nil, nil)
		defer harness.close()
		old := netip.MustParseAddr("203.0.113.51")
		servers.answer("stale.example", dnsFailoverAnswer{addrs: []netip.Addr{old}, ttl: 30})
		harness.send("stale.example", dnsmessage.TypeA, 0x0501)
		synctest.Wait()
		if _, ok := harness.reply(0x0501); !ok {
			t.Fatal("the first lookup was not answered")
		}

		// past the 30s ttl, inside the stale bound; the exit stalls and the
		// name moves
		time.Sleep(31 * time.Second)
		servers.stall(dnsFailoverStalledExit)
		fresh := netip.MustParseAddr("203.0.113.52")
		servers.answer("stale.example", dnsFailoverAnswer{addrs: []netip.Addr{fresh}, ttl: 30})
		startTime := time.Now()
		harness.send("stale.example", dnsmessage.TypeA, 0x0502)
		time.Sleep(2 * time.Second)
		synctest.Wait()

		reply, ok := harness.reply(0x0502)
		if !ok {
			t.Fatal("DNS_NO_STALE_ANSWER_WHILE_LOOKUP_HANGS: no answer 2s into a hanging lookup although the cache retains the name")
		}
		parsed := dnsFailoverParse(t, reply.payload)
		if elapsed := reply.time.Sub(startTime); elapsed != 1500*time.Millisecond || len(parsed.addrs) != 1 || parsed.addrs[0] != old || parsed.minTtl != 30 {
			t.Fatalf("answer %v ttl %d after %v, want the stale %s at ttl 30 after 1.5s", parsed.addrs, parsed.minTtl, elapsed, old)
		}

		// the live lookup refreshes the cache once the exit recovers
		servers.restore(dnsFailoverStalledExit)
		synctest.Wait()
		harness.send("stale.example", dnsmessage.TypeA, 0x0503)
		synctest.Wait()
		reply, ok = harness.reply(0x0503)
		if !ok {
			t.Fatal("no answer after the exit recovered")
		}
		if parsed := dnsFailoverParse(t, reply.payload); len(parsed.addrs) != 1 || parsed.addrs[0] != fresh {
			t.Fatalf("answer %v after the refresh, want the fresh %s", parsed.addrs, fresh)
		}
	})
}

// Root cause: every synthesized A and AAAA answer told the OS a fixed 60s TTL,
// whatever the upstream TTL was, so the OS asked again every minute for names
// that live for hours, through whatever exit then carried DNS. Pinned: the OS
// gets the upstream TTL, at most 300s, counting down while cached.
func TestDnsAnswerTtlIsTheUpstreamTtlCappedAtFiveMinutes(t *testing.T) {
	dnsFailoverSynctest(t, func(t *testing.T) {
		servers := newDnsFailoverServers(t)
		servers.ride(dnsFailoverServerA, dnsFailoverHealthyExit)
		servers.ride(dnsFailoverServerB, dnsFailoverHealthyExit)
		harness := newDnsFailoverMux(t, servers, []string{dnsFailoverServerA, dnsFailoverServerB}, nil, nil)
		defer harness.close()
		servers.answer("long.example", dnsFailoverAnswer{addrs: []netip.Addr{netip.MustParseAddr("203.0.113.61")}, ttl: 3600})
		servers.answer("short.example", dnsFailoverAnswer{addrs: []netip.Addr{netip.MustParseAddr("203.0.113.62")}, ttl: 120})
		harness.send("long.example", dnsmessage.TypeA, 0x0601)
		harness.send("short.example", dnsmessage.TypeA, 0x0602)
		synctest.Wait()
		time.Sleep(10 * time.Second)
		harness.send("short.example", dnsmessage.TypeA, 0x0603)
		synctest.Wait()

		ttls := []uint32{}
		for _, id := range []uint16{0x0601, 0x0602, 0x0603} {
			reply, ok := harness.reply(id)
			if !ok {
				t.Fatalf("query %04x was not answered", id)
			}
			ttls = append(ttls, dnsFailoverParse(t, reply.payload).minTtl)
		}
		if !slices.Equal(ttls, []uint32{300, 120, 110}) {
			t.Fatalf("DNS_FIXED_60S_TTL: answer ttls %v for upstream 3600, 120 and 120 cached 10s, want [300 120 110]", ttls)
		}
	})
}

// Root cause: an HTTPS answer was never cached, unlike A and AAAA, so every
// origin of every page asked the tunnel for it again, and a stalled exit held
// each of those lookups. Pinned: a repeat HTTPS question is answered from the
// cache, its TTL counting down, without asking a server.
func TestDnsHttpsAnswerIsCachedLikeAddressAnswers(t *testing.T) {
	dnsFailoverSynctest(t, func(t *testing.T) {
		servers := newDnsFailoverServers(t)
		servers.ride(dnsFailoverServerA, dnsFailoverHealthyExit)
		servers.ride(dnsFailoverServerB, dnsFailoverHealthyExit)
		harness := newDnsFailoverMux(t, servers, []string{dnsFailoverServerA, dnsFailoverServerB}, nil, nil)
		defer harness.close()
		hint := netip.MustParseAddr("203.0.113.71")
		servers.answer("origin.example", dnsFailoverAnswer{httpsHint: hint, ttl: 120})
		harness.send("origin.example", dnsTypeHttps, 0x0701)
		synctest.Wait()
		asked := len(servers.requestsFor("origin.example"))
		time.Sleep(10 * time.Second)
		harness.send("origin.example", dnsTypeHttps, 0x0702)
		synctest.Wait()

		reply, ok := harness.reply(0x0702)
		if !ok {
			t.Fatal("the repeat HTTPS question was not answered")
		}
		if again := len(servers.requestsFor("origin.example")) - asked; again != 0 {
			t.Fatalf("DNS_HTTPS_NOT_CACHED: the repeat HTTPS question asked the servers %d more times", again)
		}
		parsed := dnsFailoverParse(t, reply.payload)
		if len(parsed.hints) != 1 || parsed.hints[0] != hint || parsed.minTtl != 110 {
			t.Fatalf("cached HTTPS answer %v ttl %d, want hint %s at ttl 110", parsed.hints, parsed.minTtl, hint)
		}
	})
}

// Guard (passes before and after the fix): while the tunnel DoH is cold —
// nothing answered yet on this connect — one attempt keeps the whole budget,
// as a tunnel still forming needs: a server that answers after 20s is waited
// for, and no retry interrupts it.
func TestDnsColdTunnelKeepsTheWholeFlightBudget(t *testing.T) {
	dnsFailoverSynctest(t, func(t *testing.T) {
		servers := newDnsFailoverServers(t)
		servers.ride(dnsFailoverServerA, dnsFailoverStalledExit)
		servers.ride(dnsFailoverServerB, dnsFailoverStalledExit)
		harness := newDnsFailoverMux(t, servers, []string{dnsFailoverServerA, dnsFailoverServerB}, nil, nil)
		defer harness.close()
		answer := netip.MustParseAddr("203.0.113.81")
		servers.answer("forming.example", dnsFailoverAnswer{addrs: []netip.Addr{answer}, ttl: 300})
		servers.stall(dnsFailoverStalledExit)
		startTime := time.Now()
		harness.send("forming.example", dnsmessage.TypeA, 0x0801)
		synctest.Wait()
		launched := len(servers.requestsFor("forming.example"))
		time.Sleep(20 * time.Second)
		servers.restore(dnsFailoverStalledExit)
		synctest.Wait()

		reply, ok := harness.reply(0x0801)
		if !ok {
			t.Fatal("a cold lookup gave up before its server answered at 20s")
		}
		if elapsed := reply.time.Sub(startTime); elapsed != 20*time.Second {
			t.Fatalf("cold answer after %v, want at 20s", elapsed)
		}
		if requests := len(servers.requestsFor("forming.example")); requests != launched {
			t.Fatalf("a cold attempt was cut and retried: %d requests, %d launched first", requests, launched)
		}
	})
}

// Root cause: the in-tunnel DoH connections kept host-network timers through
// the tunnel: a 30s tls handshake and a 30s+15s HTTP/2 health check, so a
// connection through a dead exit held its lookups that long. Pinned: 8s and
// 10s+5s on the tunnel resolver the mux builds. (The 5s warm dial cap is
// pinned in TestDnsWarmDialCapEndsAStalledDialAndSparesAColdOne.)
func TestDnsTunnelDohConnectionTimersAreShort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultUpgradeMuxSettings()
	settings.Dns.Fallback = nil
	mux, err := NewUpgradeMux(ctx, TransferPath{}, protocol.ProvideMode_Network, 0, func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {}, settings, NewNoopLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer mux.Close()
	cache := mux.mux.Tun().DohCache()
	h2tr := &http2.Transport{}
	configureDohHttp2Transport(h2tr, cache.settings)
	tlsTimeout := cache.remoteClient.httpClient.Transport.(*http.Transport).TLSHandshakeTimeout
	if tlsTimeout != 8*time.Second || h2tr.ReadIdleTimeout != 10*time.Second || h2tr.PingTimeout != 5*time.Second {
		t.Fatalf("DNS_TUNNEL_DOH_HOST_TIMERS: tls %v, http/2 health check %v+%v, want 8s, 10s+5s",
			tlsTimeout, h2tr.ReadIdleTimeout, h2tr.PingTimeout)
	}
}
