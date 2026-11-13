package connect

import (
	"context"
	"net/netip"
	"testing"
)

// Cluster membership is only timing, so a bypass of a busy host must not carry
// the unrelated hosts active beside it out of the tunnel.
func TestBlockActionLocalRouteStaysWithDestination(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	multiClient, _, _ := newBlockActionTestMultiClient(ctx)
	defer multiClient.Close()

	bypassed := netip.MustParseAddr("198.18.0.1")
	neighbor := netip.MustParseAddr("198.18.0.2")
	AssertEqual(t, multiClient.ipAssoc != nil, true)
	members := []netip.Addr{bypassed, neighbor}
	multiClient.ipAssoc.clusters.Store(&ipAssocClusters{
		version: multiClient.ipAssoc.clusters.Load().version + 1,
		members: map[netip.Addr][]netip.Addr{bypassed: members, neighbor: members},
	})

	decide := func(addr netip.Addr) *blockActionDecision {
		return multiClient.blockActionDecision(
			multiClient.blockActionState.Load(),
			nil,
			false,
			&IpPath{Version: 4, Protocol: IpProtocolUdp, DestinationIp: addr.AsSlice(), DestinationPort: 443},
		)
	}

	local := &RouteOverride{Local: true}
	block := &BlockOverride{Block: true}

	for _, host := range []string{"198.18.0.1", "198.18.0.0/31"} {
		multiClient.SetBlockActionOverrides([]*BlockActionOverride{{OverrideId: NewId(), Hosts: []string{host}, RouteOverride: local}})
		AssertEqual(t, decide(bypassed).match.routeOverride, local)
		AssertEqual(t, decide(neighbor).match == nil, true)
	}

	// a block still extends, also when it shares an override with a local route
	multiClient.SetBlockActionOverrides([]*BlockActionOverride{{OverrideId: NewId(), Hosts: []string{"198.18.0.1"}, BlockOverride: block}})
	AssertEqual(t, decide(neighbor).match.blockOverride, block)
	multiClient.SetBlockActionOverrides([]*BlockActionOverride{{OverrideId: NewId(), Hosts: []string{"198.18.0.1"}, BlockOverride: block, RouteOverride: local}})
	neighborMatch := decide(neighbor).match
	AssertEqual(t, neighborMatch.blockOverride, block)
	AssertEqual(t, neighborMatch.routeOverride == nil, true)

	// a provider route still extends
	pin := &RouteOverride{Pin: true}
	multiClient.SetBlockActionOverrides([]*BlockActionOverride{{OverrideId: NewId(), Hosts: []string{"198.18.0.1"}, RouteOverride: pin}})
	AssertEqual(t, decide(neighbor).match.routeOverride, pin)
}
