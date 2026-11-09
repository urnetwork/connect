// C18's source half (sidecar design §17.9): a share link's multi client admits
// a packet to its private destinations as a same-network one, refuses one to
// any other private address as before, admits replies from its private
// destinations alone, and changes nothing without them. Destinations are a
// share's aliases in 100.64.0.0/10 and RFC 1918, and documentation addresses.
package connect

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// The parts of a share link's multi client its policy decisions read.
func newShareTestMultiClient(t *testing.T, sharePrivateDestinations []netip.Prefix) *RemoteUserNatMultiClient {
	t.Helper()
	settings := DefaultMultiClientSettings()
	settings.SharePrivateDestinations = sharePrivateDestinations
	return &RemoteUserNatMultiClient{
		settings:       settings,
		provideMode:    protocol.ProvideMode_FriendsAndFamily,
		securityPolicy: DefaultSecurityPolicy(context.Background()),
	}
}

func shareTestPath(destination net.IP, destinationPort int) *IpPath {
	return &IpPath{
		Version: 4, Protocol: IpProtocolTcp,
		SourceIp: net.IPv4(192, 0, 2, 2).To4(), SourcePort: 42200,
		DestinationIp: destination.To4(), DestinationPort: destinationPort,
	}
}

var shareTestAliases = []netip.Prefix{
	netip.MustParsePrefix("100.80.1.0/24"),
	netip.MustParsePrefix("10.44.0.0/16"),
}

// A packet to an alias is admitted; one to another private address keeps the
// FriendsAndFamily link's public rules, which refuse it; a public destination
// keeps them too.
func TestShareSourceAdmitsOnlyItsPrivateDestinations(t *testing.T) {
	multiClient := newShareTestMultiClient(t, shareTestAliases)
	for _, c := range []struct {
		destination net.IP
		admitted    bool
	}{
		{destination: net.IPv4(100, 80, 1, 30), admitted: true},
		{destination: net.IPv4(10, 44, 9, 9), admitted: true},
		{destination: net.IPv4(10, 45, 0, 1), admitted: false},
		{destination: net.IPv4(192, 168, 1, 1), admitted: false},
		{destination: net.IPv4(127, 0, 0, 1), admitted: false},
	} {
		path := shareTestPath(c.destination, 22)
		relationship := multiClient.egressRelationshipFor(protocol.ProvideMode_Network, path)
		result, _, err := inspectEgressDetailed(multiClient.securityPolicy, relationship, path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if (result == SecurityPolicyResultAllow) != c.admitted {
			t.Fatalf("%s: relationship %s gave %s, want admitted %t", c.destination, relationship, result, c.admitted)
		}
	}
	public := shareTestPath(net.IPv4(203, 0, 113, 9), 6881)
	if relationship := multiClient.egressRelationshipFor(protocol.ProvideMode_Network, public); relationship != protocol.ProvideMode_Public {
		t.Fatalf("a public destination's relationship is %s, want Public", relationship)
	}
	if relationship := multiClient.egressRelationshipFor(protocol.ProvideMode_Service, shareTestPath(net.IPv4(100, 80, 1, 30), 22)); relationship != protocol.ProvideMode_Service {
		t.Fatalf("a Service source reached an alias as %s", relationship)
	}
}

// Replies from an alias are admitted, a privileged port included; a reply from
// any other private address is dropped; a public reply keeps the policy's
// check, and a same-network reply is the policy's as before.
func TestShareSourceAdmitsRepliesFromItsPrivateDestinationsAlone(t *testing.T) {
	multiClient := newShareTestMultiClient(t, shareTestAliases)
	for _, c := range []struct {
		source      net.IP
		sourcePort  int
		provideMode protocol.ProvideMode
		want        SecurityPolicyResult
	}{
		{source: net.IPv4(100, 80, 1, 30), sourcePort: 22, provideMode: protocol.ProvideMode_FriendsAndFamily, want: SecurityPolicyResultAllow},
		{source: net.IPv4(10, 45, 0, 1), sourcePort: 8443, provideMode: protocol.ProvideMode_FriendsAndFamily, want: SecurityPolicyResultDrop},
		{source: net.IPv4(100, 81, 0, 1), sourcePort: 8443, provideMode: protocol.ProvideMode_FriendsAndFamily, want: SecurityPolicyResultDrop},
		{source: net.IPv4(203, 0, 113, 9), sourcePort: 443, provideMode: protocol.ProvideMode_FriendsAndFamily, want: SecurityPolicyResultAllow},
		{source: net.IPv4(203, 0, 113, 9), sourcePort: 22, provideMode: protocol.ProvideMode_FriendsAndFamily, want: SecurityPolicyResultDrop},
		{source: net.IPv4(10, 45, 0, 1), sourcePort: 8443, provideMode: protocol.ProvideMode_Network, want: SecurityPolicyResultAllow},
	} {
		reply := shareTestPath(c.source, c.sourcePort).Reverse()
		if result, err := multiClient.inspectIngressFor(c.provideMode, reply); err != nil || result != c.want {
			t.Fatalf("a %s reply from %s:%d gave %s err=%v, want %s", c.provideMode, c.source, c.sourcePort, result, err, c.want)
		}
	}
}

// Without private destinations the link is exactly as before: an alias is a
// private destination its public rules refuse, and a private reply meets the
// policy alone.
func TestShareSourceOffChangesNothing(t *testing.T) {
	multiClient := newShareTestMultiClient(t, nil)
	path := shareTestPath(net.IPv4(10, 44, 9, 9), 22)
	if relationship := multiClient.egressRelationshipFor(protocol.ProvideMode_Network, path); relationship != egressRelationship(protocol.ProvideMode_Network, protocol.ProvideMode_FriendsAndFamily) {
		t.Fatalf("relationship %s without private destinations", relationship)
	}
	reply := shareTestPath(net.IPv4(10, 44, 9, 9), 8443).Reverse()
	want, _ := multiClient.securityPolicy.InspectIngress(protocol.ProvideMode_FriendsAndFamily, reply, nil)
	if result, _ := multiClient.inspectIngressFor(protocol.ProvideMode_FriendsAndFamily, reply); result != want {
		t.Fatalf("a reply gave %s without private destinations, want the policy's %s", result, want)
	}
}

// The share's prefixes judge a v4-mapped v6 address as its v4 address, and the
// private test covers the shared address space.
func TestShareDestinationHelpers(t *testing.T) {
	if !sharePrivateDestinationContains(shareTestAliases, net.ParseIP("::ffff:100.80.1.30")) {
		t.Fatal("a v4-mapped alias was not contained")
	}
	if sharePrivateDestinationContains(nil, net.IPv4(100, 80, 1, 30)) {
		t.Fatal("no prefixes contained an address")
	}
	for ip, private := range map[string]bool{
		"100.64.0.1":  true,
		"10.0.0.1":    true,
		"127.0.0.1":   true,
		"203.0.113.9": false,
		"100.63.0.1":  false,
	} {
		if egressPrivateAddress(net.ParseIP(ip)) != private {
			t.Fatalf("%s private=%t", ip, !private)
		}
	}
}
