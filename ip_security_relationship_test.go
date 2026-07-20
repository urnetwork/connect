package connect

import (
	"context"
	"net"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// egressRelationship combines a packet source's provide mode with the local
// client's own provide mode into the relationship the security policy enforces on
// egress. ProvideMode is a set of flags, not an ordered scale, so the result must
// be decided per-case: only a genuine same-Network relationship on BOTH sides may
// reach non-public destinations; anything else (including an unspecified None)
// must fall under the public rules.
func TestEgressRelationship(t *testing.T) {
	cases := []struct {
		source protocol.ProvideMode
		client protocol.ProvideMode
		want   protocol.ProvideMode
	}{
		// both Network -> Network (the only combination that may reach a LAN)
		{protocol.ProvideMode_Network, protocol.ProvideMode_Network, protocol.ProvideMode_Network},

		// the hosted proxy: its multiclient is ProvideMode_Public, so even the
		// hard-coded ProvideMode_Network egress source must resolve to Public
		{protocol.ProvideMode_Network, protocol.ProvideMode_Public, protocol.ProvideMode_Public},

		// anything other than "both Network" -> Public
		{protocol.ProvideMode_Public, protocol.ProvideMode_Network, protocol.ProvideMode_Public},
		{protocol.ProvideMode_None, protocol.ProvideMode_Network, protocol.ProvideMode_Public},
		{protocol.ProvideMode_Network, protocol.ProvideMode_None, protocol.ProvideMode_Public},
		{protocol.ProvideMode_None, protocol.ProvideMode_None, protocol.ProvideMode_Public},
		{protocol.ProvideMode_Public, protocol.ProvideMode_Public, protocol.ProvideMode_Public},
		{protocol.ProvideMode_FriendsAndFamily, protocol.ProvideMode_Network, protocol.ProvideMode_Public},
		{protocol.ProvideMode_Stream, protocol.ProvideMode_Network, protocol.ProvideMode_Public},
		{protocol.ProvideMode_Network, protocol.ProvideMode_Stream, protocol.ProvideMode_Public},
	}
	for _, c := range cases {
		if got := egressRelationship(c.source, c.client); got != c.want {
			t.Errorf("egressRelationship(%v, %v) = %v, want %v", c.source, c.client, got, c.want)
		}
	}
}

func testEgressPath(dstIp string) *IpPath {
	return &IpPath{
		Version:         4,
		Protocol:        IpProtocolTcp,
		SourceIp:        net.ParseIP("10.0.0.2"),
		SourcePort:      12345,
		DestinationIp:   net.ParseIP(dstIp),
		DestinationPort: 443,
		Syn:             true,
	}
}

// The security guarantee the proxy relies on: a same-Network relationship
// bypasses the public rules (may reach a LAN), while any other relationship
// enforces isPublicUnicast — so a private / loopback / link-local (incl. the
// cloud metadata endpoint) destination is an Incident, which blockActionApply
// treats as a non-overridable block.
func TestInspectEgressNetworkBypassAndPublicEnforcement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	policy := DefaultSecurityPolicy(ctx)

	nonPublic := []string{
		"10.0.0.5",        // RFC1918
		"192.168.1.10",    // RFC1918
		"127.0.0.1",       // loopback
		"169.254.169.254", // link-local: the cloud metadata endpoint
	}

	// ProvideMode_Network: trusted same-network relationship reaches the LAN
	for _, ip := range nonPublic {
		r, err := policy.InspectEgress(protocol.ProvideMode_Network, testEgressPath(ip), nil)
		if err != nil {
			t.Fatalf("InspectEgress(Network, %s): unexpected error %v", ip, err)
		}
		if r != SecurityPolicyResultAllow {
			t.Errorf("InspectEgress(Network, %s) = %v, want Allow (same-network bypass)", ip, r)
		}
	}

	// ProvideMode_Public: the same LAN destinations are blocked as incidents
	for _, ip := range nonPublic {
		r, err := policy.InspectEgress(protocol.ProvideMode_Public, testEgressPath(ip), nil)
		if err != nil {
			t.Fatalf("InspectEgress(Public, %s): unexpected error %v", ip, err)
		}
		if r != SecurityPolicyResultIncident {
			t.Errorf("InspectEgress(Public, %s) = %v, want Incident (isPublicUnicast enforced)", ip, r)
		}
	}

	// a public destination under Public passes isPublicUnicast (not an incident)
	if r, err := policy.InspectEgress(protocol.ProvideMode_Public, testEgressPath("8.8.8.8"), nil); err != nil {
		t.Fatalf("InspectEgress(Public, 8.8.8.8): unexpected error %v", err)
	} else if r == SecurityPolicyResultIncident {
		t.Errorf("InspectEgress(Public, 8.8.8.8) = Incident, want a public unicast destination to pass")
	}
}

// Composition as the proxy experiences it: the DeviceLocal egress hard-codes a
// ProvideMode_Network source, but the multiclient is ProvideMode_Public, so
// egressRelationship resolves to Public and a LAN destination is blocked. A
// genuine same-Network client (client mode Network) may still reach the LAN.
func TestProxyEgressBlocksLocalDestinations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	policy := DefaultSecurityPolicy(ctx)

	const hostedClientMode = protocol.ProvideMode_Public // the proxy multiclient
	const egressSource = protocol.ProvideMode_Network    // device_local hard-codes this

	proxyRel := egressRelationship(egressSource, hostedClientMode)
	if r, _ := policy.InspectEgress(proxyRel, testEgressPath("169.254.169.254"), nil); r != SecurityPolicyResultIncident {
		t.Errorf("hosted proxy egress to metadata endpoint = %v, want Incident (blocked)", r)
	}

	sameNetworkRel := egressRelationship(egressSource, protocol.ProvideMode_Network)
	if r, _ := policy.InspectEgress(sameNetworkRel, testEgressPath("10.0.0.5"), nil); r != SecurityPolicyResultAllow {
		t.Errorf("same-network client egress to LAN = %v, want Allow", r)
	}
}

// recordSourceProvideMode chooses the return provide mode per-case (never a
// numeric min): it prefers the same-Network relationship once a source has used
// it, otherwise it remembers the source's latest non-Network mode. The recorded
// value drives companion-contract selection on the provider return path.
func TestRecordSourceProvideMode(t *testing.T) {
	newProvider := func() *RemoteUserNatProvider {
		return &RemoteUserNatProvider{
			// no source cap: these cases exercise the record/return logic,
			// not eviction
			settings:          &RemoteUserNatProviderSettings{MaxSourceCount: 0},
			sourceProvideMode: map[Id]protocol.ProvideMode{},
		}
	}
	fallback := protocol.ProvideMode_Stream

	t.Run("untracked source returns the fallback", func(t *testing.T) {
		p := newProvider()
		if got := p.sourceReturnProvideMode(NewId(), fallback); got != fallback {
			t.Errorf("untracked = %v, want fallback %v", got, fallback)
		}
	})

	t.Run("records the first mode seen", func(t *testing.T) {
		p := newProvider()
		id := NewId()
		p.recordSourceProvideMode(id, protocol.ProvideMode_Public)
		if got := p.sourceReturnProvideMode(id, fallback); got != protocol.ProvideMode_Public {
			t.Errorf("first-seen = %v, want Public", got)
		}
	})

	t.Run("prefers Network once the source has used it", func(t *testing.T) {
		p := newProvider()
		id := NewId()
		p.recordSourceProvideMode(id, protocol.ProvideMode_Public)
		p.recordSourceProvideMode(id, protocol.ProvideMode_Network)
		if got := p.sourceReturnProvideMode(id, fallback); got != protocol.ProvideMode_Network {
			t.Errorf("public then network = %v, want Network", got)
		}
	})

	t.Run("keeps Network even after a later non-Network mode", func(t *testing.T) {
		p := newProvider()
		id := NewId()
		p.recordSourceProvideMode(id, protocol.ProvideMode_Network)
		p.recordSourceProvideMode(id, protocol.ProvideMode_Public)
		if got := p.sourceReturnProvideMode(id, fallback); got != protocol.ProvideMode_Network {
			t.Errorf("network then public = %v, want Network", got)
		}
	})

	t.Run("updates to the latest non-Network mode", func(t *testing.T) {
		p := newProvider()
		id := NewId()
		p.recordSourceProvideMode(id, protocol.ProvideMode_Public)
		p.recordSourceProvideMode(id, protocol.ProvideMode_Stream)
		if got := p.sourceReturnProvideMode(id, fallback); got != protocol.ProvideMode_Stream {
			t.Errorf("public then stream = %v, want Stream", got)
		}
	})

	t.Run("caps the tracked source count", func(t *testing.T) {
		p := &RemoteUserNatProvider{
			settings:          &RemoteUserNatProviderSettings{MaxSourceCount: 2},
			sourceProvideMode: map[Id]protocol.ProvideMode{},
		}
		id1 := NewId()
		id2 := NewId()
		id3 := NewId()
		p.recordSourceProvideMode(id1, protocol.ProvideMode_Public)
		p.recordSourceProvideMode(id2, protocol.ProvideMode_Public)
		// adding a third new source evicts one existing entry to stay at the cap
		p.recordSourceProvideMode(id3, protocol.ProvideMode_Public)
		if got := len(p.sourceProvideMode); got != 2 {
			t.Errorf("tracked source count = %d, want 2 (capped)", got)
		}
		// the just-recorded source is retained; an evicted source falls back
		if got := p.sourceReturnProvideMode(id3, fallback); got != protocol.ProvideMode_Public {
			t.Errorf("newest source = %v, want Public", got)
		}
	})
}
