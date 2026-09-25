// Progressive protected-name UDP candidates preserve late-family handshake
// fallback without delaying the first usable address. State belongs to one dial.
package connect

import (
	"context"
	"net"
	"net/netip"
	"sync"
)

// Owns at most two family queries. The planner consumes the first usable batch;
// its one caller then consumes pending results and closes this owner on return.
// Closing cancels and joins only these query workers, not a shared cache.
type udpDialCandidateSource struct {
	results      <-chan dohDialQueryResult
	pending      int
	ports        []int
	cancel       context.CancelFunc
	queryWorkers sync.WaitGroup
}

// Resolves the first usable family while retaining later family answers for
// QUIC/Alt. Pins, literals, non-protected names and custom resolvers retain the
// existing resolver policy; only an actual generic protected DoH lookup streams.
func (self *ClientStrategy) startControlUdpCandidates(
	ctx context.Context,
	address string,
	ipFamily int,
) ([]*net.UDPAddr, *udpDialCandidateSource, error) {
	host, portString, splitErr := net.SplitHostPort(address)
	_, literalErr := netip.ParseAddr(host)
	if splitErr != nil || host == "" || literalErr == nil ||
		normalizeIpFamily(ipFamily) != 0 || self == nil ||
		self.internalDohResolver == nil || !self.internalDohResolver.matches(host) {
		addrs, err := self.resolveControlUDPAddrs(ctx, address, ipFamily)
		return addrs, nil, err
	}
	port, err := parseControlUDPPort(address, portString)
	if err != nil {
		return nil, nil, err
	}
	network, err := controlDialNetwork("udp", address)
	if err != nil {
		return nil, nil, err
	}
	if network != "udp" {
		addrs, err := self.resolveControlUDPAddrs(ctx, address, ipFamily)
		return addrs, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	queryCtx, cancel := context.WithCancel(ctx)
	recordTypes := dohDialRecordTypes(network)
	results := make(chan dohDialQueryResult, len(recordTypes))
	source := &udpDialCandidateSource{
		results: results,
		pending: len(recordTypes),
		ports:   []int{port},
		cancel:  cancel,
	}
	for _, recordType := range recordTypes {
		source.queryWorkers.Add(1)
		go func() {
			defer source.queryWorkers.Done()
			addrs, authoritative := self.internalDohResolver.cache.QueryResult(queryCtx, recordType, host)
			results <- dohDialQueryResult{
				addrs:         dialAddrsMatchNetwork(network, addrs),
				authoritative: authoritative,
			}
		}()
	}
	authoritativeCount := 0
	for 0 < source.pending {
		select {
		case <-ctx.Done():
			source.close()
			return nil, nil, ctx.Err()
		case result := <-results:
			source.pending--
			if err := ctx.Err(); err != nil {
				source.close()
				return nil, nil, err
			}
			if result.authoritative {
				authoritativeCount++
			}
			if addrs := source.candidates(result); 0 < len(addrs) {
				if source.pending == 0 {
					source.close()
					return addrs, nil, nil
				}
				return addrs, source, nil
			}
		}
	}
	source.close()
	if authoritativeCount == len(recordTypes) {
		return nil, nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return nil, nil, &net.DNSError{Err: "DoH resolution failed", Name: host, IsTemporary: true}
}

// Converts one ready family to candidates in the owning carrier's port order.
// The planner sets the port list before returning this source to its caller.
func (self *udpDialCandidateSource) candidates(result dohDialQueryResult) []*net.UDPAddr {
	addrs := orderDialAddrs(result.addrs)
	candidates := make([]*net.UDPAddr, 0, len(addrs)*len(self.ports))
	for _, port := range self.ports {
		for _, addr := range addrs {
			candidates = append(candidates, &net.UDPAddr{
				IP:   net.IP(addr.AsSlice()),
				Port: port,
				Zone: addr.Zone(),
			})
		}
	}
	return candidates
}

// Joins the bounded request workers before their dial owner can finish.
func (self *udpDialCandidateSource) close() {
	if self != nil {
		self.cancel()
		self.queryWorkers.Wait()
	}
}

// Nil disables the select arm after every family result has been consumed.
func (self *udpDialCandidateSource) resultChannel() <-chan dohDialQueryResult {
	if self == nil || self.pending == 0 {
		return nil
	}
	return self.results
}

// Only the planner/racer owner consumes results. A late family preserves the
// existing candidates and port order without scheduling a duplicate endpoint.
func (self *udpDialCandidateSource) appendReady(candidates []*net.UDPAddr, result dohDialQueryResult) []*net.UDPAddr {
	self.pending--
	seen := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		seen[candidate.String()] = true
	}
	for _, candidate := range self.candidates(result) {
		if key := candidate.String(); !seen[key] {
			seen[key] = true
			candidates = append(candidates, candidate)
		}
	}
	return candidates
}
