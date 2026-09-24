package connect

// net_dial_race.go — the shared dual-stack ("happy eyeballs") dial race used
// by every hostname dial in this package: ConnectSettings.DialContext, the
// internal DoH resolver's protected-name path, and the gVisor tun's internal
// dialer (IPV6.md C6).
//
// DoH stream dials feed each completed A or AAAA answer into the TCP race,
// so one pending family cannot strand the other. A narrowed network still
// resolves only its permitted family, and explicit net.Resolver callers keep
// their configured lookup behavior. The first usable family starts at once;
// later ready addresses alternate families with one delay between attempts.
// A definitive failure launches
// the next address at once; the first success cancels the rest and every
// losing connection is closed. IP literals need no resolution.
//
// Only stream dials race. A UDP "dial" is a local socket operation that
// succeeds for any family the host has an address in, so a race would always
// crown the first address rather than a working path; UDP callers pick one
// address and rely on their own reply timeouts.
//
// Every function here is pure or safe for concurrent use.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultDialFallbackDelay is the delay between successive address launches
// in a dial race. RFC 8305 recommends 250ms; it is short enough that a dead
// family costs one quarter second, and long enough that a healthy first
// address usually completes before the second is launched.
const DefaultDialFallbackDelay = 250 * time.Millisecond

// dialAddrFunction dials one already-resolved address. The port and the base
// network are the caller's, captured in the closure.
type dialAddrFunction func(ctx context.Context, addr netip.Addr) (net.Conn, error)

// isRaceableDialNetwork reports whether the network is a stream family that
// benefits from racing resolved addresses.
func isRaceableDialNetwork(network string) bool {
	switch network {
	case "tcp", "tcp4", "tcp6":
		return true
	default:
		return false
	}
}

// lookupNetworkForDial maps a dial network, possibly narrowed to one family by
// controlDialNetwork, to the net.Resolver lookup network: a "4" suffix
// resolves A only, a "6" suffix AAAA only, anything else both.
func lookupNetworkForDial(network string) string {
	switch {
	case strings.HasSuffix(network, "4"):
		return "ip4"
	case strings.HasSuffix(network, "6"):
		return "ip6"
	default:
		return "ip"
	}
}

// familyDialNetwork is the family-specific network for dialing one address:
// the caller's base network ("tcp", "udp") with the address's family suffix.
// Dialing a literal with the matching suffix keeps the family explicit for
// every layer below (the egress interface bind, the dial log), and never
// contradicts the literal.
func familyDialNetwork(network string, addr netip.Addr) string {
	base := strings.TrimRight(network, "46")
	if addr.Unmap().Is4() {
		return base + "4"
	}
	return base + "6"
}

// dialAddrsMatchNetwork keeps only the addresses the (possibly narrowed)
// network can dial, so a resolver that answered both families for a "tcp4"
// dial cannot hand the race a v6 address.
func dialAddrsMatchNetwork(network string, addrs []netip.Addr) []netip.Addr {
	lookup := lookupNetworkForDial(network)
	if lookup == "ip" {
		return addrs
	}
	matched := make([]netip.Addr, 0, len(addrs))
	for _, addr := range addrs {
		if (lookup == "ip4") == addr.Unmap().Is4() {
			matched = append(matched, addr)
		}
	}
	return matched
}

// orderDialAddrs makes the launch order stable: unmapped, deduplicated,
// sorted within each family, and interleaved v6 first so a dead IPv6 or IPv4
// path can consume at most one fallback delay before the other family is
// tried.
func orderDialAddrs(addrs []netip.Addr) []netip.Addr {
	ipv4 := make([]netip.Addr, 0, len(addrs))
	ipv6 := make([]netip.Addr, 0, len(addrs))
	seen := map[netip.Addr]bool{}
	for _, addr := range addrs {
		addr = addr.Unmap()
		if !addr.IsValid() || seen[addr] {
			continue
		}
		seen[addr] = true
		if addr.Is4() {
			ipv4 = append(ipv4, addr)
		} else {
			ipv6 = append(ipv6, addr)
		}
	}
	sort.Slice(ipv4, func(i int, j int) bool { return ipv4[i].Less(ipv4[j]) })
	sort.Slice(ipv6, func(i int, j int) bool { return ipv6[i].Less(ipv6[j]) })
	ordered := make([]netip.Addr, 0, len(ipv4)+len(ipv6))
	for i := 0; i < max(len(ipv4), len(ipv6)); i++ {
		if i < len(ipv6) {
			ordered = append(ordered, ipv6[i])
		}
		if i < len(ipv4) {
			ordered = append(ordered, ipv4[i])
		}
	}
	return ordered
}

// resolveDialAddrs resolves a hostname through a net.Resolver for the families
// the dial network permits, ordered for racing. The resolver must not be nil;
// callers pass dialResolver(custom) so the platform's egress-bound resolver is
// used while this process provides a tunnel.
func resolveDialAddrs(ctx context.Context, resolver *net.Resolver, network string, host string) ([]netip.Addr, error) {
	addrs, err := resolver.LookupNetIP(ctx, lookupNetworkForDial(network), host)
	if err != nil {
		return nil, err
	}
	ordered := orderDialAddrs(dialAddrsMatchNetwork(network, addrs))
	if len(ordered) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return ordered, nil
}

// One record type's answer, shared by the bulk and progressive consumers.
type dohDialQueryResult struct {
	addrs         []netip.Addr
	authoritative bool
}

// resolveDohDialAddrs resolves a hostname through a DohCache for the families
// the dial network permits: A and AAAA concurrently for a family-agnostic
// network, one record type for a narrowed one. The answer is ordered for
// racing. "No such host" is reported only when every queried record type was
// answered authoritatively empty; a failed query is temporary.
func resolveDohDialAddrs(ctx context.Context, cache *DohCache, network string, host string) ([]netip.Addr, error) {
	recordTypes := dohDialRecordTypes(network)
	if len(recordTypes) == 0 {
		return nil, fmt.Errorf("resolve %s: ipv4 and ipv6 are both disabled", host)
	}

	results := make(chan dohDialQueryResult, len(recordTypes))
	for _, recordType := range recordTypes {
		go func() {
			addrs, authoritative := cache.QueryResult(ctx, recordType, host)
			results <- dohDialQueryResult{addrs: addrs, authoritative: authoritative}
		}()
	}

	var addrs []netip.Addr
	authoritativeCount := 0
	for range recordTypes {
		result := <-results
		addrs = append(addrs, result.addrs...)
		if result.authoritative {
			authoritativeCount++
		}
	}
	if 0 < len(addrs) {
		return orderDialAddrs(dialAddrsMatchNetwork(network, addrs)), nil
	}
	if authoritativeCount == len(recordTypes) {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, &net.DNSError{Err: "DoH resolution failed", Name: host, IsTemporary: true}
}

// A datagram socket needs only the first usable family. Empty or failed
// answers keep the other query live; a winner cancels and joins our waiters.
// Multi-candidate transports retain the bulk resolver until their own race
// can consume late families without throwing away handshake fallback.
func resolveFirstDohDialAddrs(ctx context.Context, cache *DohCache, network string, host string) ([]netip.Addr, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	recordTypes := dohDialRecordTypes(network)
	queryCtx, queryCancel := context.WithCancel(ctx)
	results := make(chan dohDialQueryResult, len(recordTypes))
	var queryWorkers sync.WaitGroup
	defer func() {
		queryCancel()
		queryWorkers.Wait()
	}()
	for _, recordType := range recordTypes {
		queryWorkers.Add(1)
		go func() {
			defer queryWorkers.Done()
			addrs, authoritative := cache.QueryResult(queryCtx, recordType, host)
			results <- dohDialQueryResult{addrs: addrs, authoritative: authoritative}
		}()
	}
	authoritativeCount := 0
	for range recordTypes {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case result := <-results:
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			addrs := orderDialAddrs(dialAddrsMatchNetwork(network, result.addrs))
			if 0 < len(addrs) {
				return addrs, nil
			}
			if result.authoritative {
				authoritativeCount++
			}
		}
	}
	if authoritativeCount == len(recordTypes) {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return nil, &net.DNSError{Err: "DoH resolution failed", Name: host, IsTemporary: true}
}

// Selects only the record families the caller's network permits.
func dohDialRecordTypes(network string) []string {
	recordTypes := make([]string, 0, 2)
	if !strings.HasSuffix(network, "4") {
		recordTypes = append(recordTypes, "AAAA")
	}
	if !strings.HasSuffix(network, "6") {
		recordTypes = append(recordTypes, "A")
	}
	return recordTypes
}

// Resolves each permitted family concurrently and feeds completed answers to
// the existing address race. The first usable answer starts immediately;
// only subsequent TCP attempts are staggered. A failed first path retains the pending
// family. The caller's configured cache remains the only resolver authority.
func dialDohAddrsRace(
	ctx context.Context,
	cache *DohCache,
	network string,
	host string,
	fallbackDelay time.Duration,
	dial dialAddrFunction,
) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raceCtx, raceCancel := context.WithCancel(ctx)
	recordTypes := dohDialRecordTypes(network)
	results := make(chan dohDialQueryResult, len(recordTypes))
	var queryWorkers sync.WaitGroup
	defer func() {
		raceCancel()
		queryWorkers.Wait()
	}()
	for _, recordType := range recordTypes {
		queryWorkers.Add(1)
		go func() {
			defer queryWorkers.Done()
			addrs, authoritative := cache.QueryResult(raceCtx, recordType, host)
			results <- dohDialQueryResult{
				addrs:         dialAddrsMatchNetwork(network, addrs),
				authoritative: authoritative,
			}
		}()
	}
	return dialAddrsRaceWithResolution(raceCtx, nil, fallbackDelay, dial, &dialAddrResolution{
		results: results,
		pending: len(recordTypes),
		host:    host,
	})
}

// Carries only the bounded family queries owned by one stream dial.
type dialAddrResolution struct {
	results <-chan dohDialQueryResult
	pending int
	host    string
}

// dialRaceResult is one attempt's outcome inside dialAddrsRace.
type dialRaceResult struct {
	conn net.Conn
	err  error
}

// dialAddrsRace races dials to addrs in order. Each launch is staggered by
// fallbackDelay; a definitive failure launches the next address immediately;
// the first success wins and cancels the rest; every loser is closed. When
// every address fails the joined errors are returned. A non-positive
// fallbackDelay launches every address at once.
func dialAddrsRace(
	ctx context.Context,
	addrs []netip.Addr,
	fallbackDelay time.Duration,
	dial dialAddrFunction,
) (net.Conn, error) {
	return dialAddrsRaceWithResolution(ctx, addrs, fallbackDelay, dial, nil)
}

// The same TCP race accepts late DNS-family answers without waiting for an
// unrelated query. Static callers retain their supplied address order. A
// progressive caller cancels and joins every launched TCP worker before return.
func dialAddrsRaceWithResolution(
	ctx context.Context,
	addrs []netip.Addr,
	fallbackDelay time.Duration,
	dial dialAddrFunction,
	resolution *dialAddrResolution,
) (net.Conn, error) {
	if len(addrs) == 0 && resolution == nil {
		return nil, errors.New("dial race: no addresses")
	}
	raceCtx, raceCancel := context.WithCancel(ctx)
	var dialWorkers sync.WaitGroup
	defer func() {
		raceCancel()
		if resolution != nil {
			dialWorkers.Wait()
		}
	}()

	// Only the selected receiver takes a successful connection. Cancellation
	// closes every other result before its worker can finish.
	results := make(chan dialRaceResult)
	pendingAddrs := append([]netip.Addr(nil), addrs...)
	var seenAddrs map[netip.Addr]bool
	var queryResults <-chan dohDialQueryResult
	queryPending := 0
	if resolution != nil {
		seenAddrs = map[netip.Addr]bool{}
		for _, addr := range addrs {
			seenAddrs[addr.Unmap()] = true
		}
		queryResults = resolution.results
		queryPending = resolution.pending
	}
	authoritativeCount := 0
	launched := 0
	completed := 0
	errs := make([]error, 0, len(addrs))
	preferIpv6 := true
	launchReady := true

	var fallbackC <-chan time.Time
	var fallbackTimer *time.Timer
	stopFallback := func() {
		if fallbackTimer != nil {
			fallbackTimer.Stop()
			fallbackTimer = nil
		}
		fallbackC = nil
	}
	defer stopFallback()
	armFallback := func() {
		stopFallback()
		if 0 < fallbackDelay && (0 < len(pendingAddrs) || 0 < queryPending) {
			fallbackTimer = time.NewTimer(fallbackDelay)
			fallbackC = fallbackTimer.C
		}
	}
	launch := func() {
		index := 0
		if resolution != nil {
			for candidate, addr := range pendingAddrs {
				if addr.Is6() == preferIpv6 {
					index = candidate
					break
				}
			}
		}
		addr := pendingAddrs[index]
		pendingAddrs = append(pendingAddrs[:index], pendingAddrs[index+1:]...)
		preferIpv6 = !addr.Is6()
		launched++
		dialWorkers.Add(1)
		go HandleError(func() {
			defer dialWorkers.Done()
			conn, err := dial(raceCtx, addr)
			select {
			case results <- dialRaceResult{conn: conn, err: err}:
			case <-raceCtx.Done():
				if conn != nil {
					conn.Close()
				}
			}
		})
		launchReady = fallbackDelay <= 0
		armFallback()
	}

	for {
		for launchReady && 0 < len(pendingAddrs) {
			launch()
		}
		if queryPending == 0 && len(pendingAddrs) == 0 && completed == launched {
			if 0 < len(errs) {
				return nil, errors.Join(errs...)
			}
			if resolution != nil {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if authoritativeCount == resolution.pending {
					return nil, &net.DNSError{Err: "no such host", Name: resolution.host, IsNotFound: true}
				}
				return nil, &net.DNSError{Err: "DoH resolution failed", Name: resolution.host, IsTemporary: true}
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-fallbackC:
			stopFallback()
			launchReady = true
		case result := <-queryResults:
			queryPending--
			if result.authoritative {
				authoritativeCount++
			}
			for _, addr := range orderDialAddrs(result.addrs) {
				if !seenAddrs[addr] {
					seenAddrs[addr] = true
					pendingAddrs = append(pendingAddrs, addr)
				}
			}
			if queryPending == 0 {
				queryResults = nil
			}
		case result := <-results:
			completed++
			if result.err == nil && result.conn != nil {
				return result.conn, nil
			}
			if result.conn != nil {
				result.conn.Close()
			}
			if result.err == nil {
				result.err = errors.New("dial returned no connection")
			}
			errs = append(errs, result.err)
			// A definitive failure advances immediately, including when
			// the next family is still resolving and arrives later.
			launchReady = true
		}
	}
}

// dialHostPortRace resolves host and races the addresses through dial, which
// receives a family-specific network and the address joined with port. This
// is the form the ConnectSettings and internal DoH seams share.
func dialHostPortRace(
	ctx context.Context,
	network string,
	port string,
	addrs []netip.Addr,
	fallbackDelay time.Duration,
	dial DialContextFunction,
) (net.Conn, error) {
	return dialAddrsRace(ctx, addrs, fallbackDelay, func(ctx context.Context, addr netip.Addr) (net.Conn, error) {
		return dial(ctx, familyDialNetwork(network, addr), net.JoinHostPort(addr.String(), port))
	})
}
