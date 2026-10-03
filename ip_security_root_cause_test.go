// Root-cause regressions for IPSECURITY-UPDATE4, written against the public
// policy API only so they run unchanged on the code before the update:
//
//   - legitimate WireGuard, OpenVPN, RTMP, Monero Levin and RakNet flows were
//     dropped by the fully encrypted heuristic;
//   - a BitTorrent handshake or DHT query to a privileged port was allowed;
//   - Ethereum discovery v4 and RLPx flows were dropped by the fully encrypted
//     heuristic, while encrypted BitTorrent shaped like them must stay dropped.
//
// Inputs are the fixed fixtures in testdata/ipsecurity; nothing depends on
// timing, the network, or randomness.
package connect

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

func TestRootCauseAppStandardFlowsNotDropped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	names := []string{
		"wireguard-handshake",
		"wireguard-midstream",
		"openvpn-udp",
		"openvpn-tcp",
		"rtmp-publish",
		"levin-dense",
		"raknet-open-connection-random-padded",
	}
	for i, name := range names {
		fixture := loadSecurityFixture(t, filepath.Join("testdata", "ipsecurity", name+".json"))
		results := replayFixture(t, DefaultSecurityPolicy(ctx), fixture, 45000+i)
		for j, result := range results {
			if result != SecurityPolicyResultAllow {
				t.Errorf("%s: packet %d = %v (all %v), want every packet allowed", name, j, result, results)
				break
			}
		}
	}
}

func TestRootCausePrivilegedPortBittorrentNotAllowed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i, name := range []string{"bittorrent-tcp-443", "bittorrent-tcp-80", "dht-udp-443"} {
		fixture := loadSecurityFixture(t, filepath.Join("testdata", "ipsecurity", name+".json"))
		results := replayFixture(t, DefaultSecurityPolicy(ctx), fixture, 45100+i)
		if last := results[len(results)-1]; last != SecurityPolicyResultIncident {
			t.Errorf("%s: last result = %v, want incident", name, last)
		}

		// the provider's reversed policy reaches the same verdict
		provider := DefaultProviderSecurityPolicy(ctx)
		packets := fixturePackets(t, fixture, 45200+i)
		ipPath, payload, err := ParseIpPathWithPayload(packets[len(packets)-1])
		if err != nil {
			t.Fatal(err)
		}
		if result, _ := provider.InspectIngress(protocol.ProvideMode_Public, ipPath, payload); result != SecurityPolicyResultIncident {
			t.Errorf("%s: provider result = %v, want incident", name, result)
		}
	}
}

func TestRootCauseEthereumFlowsNotDropped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i, name := range []string{"ethereum-discv4", "ethereum-rlpx-eip8", "ethereum-rlpx-pre-eip8"} {
		fixture := loadSecurityFixture(t, filepath.Join("testdata", "ipsecurity", name+".json"))
		results := replayFixture(t, DefaultSecurityPolicy(ctx), fixture, 45300+i)
		for j, result := range results {
			if result != SecurityPolicyResultAllow {
				t.Errorf("%s: packet %d = %v (all %v), want every packet allowed", name, j, result, results)
				break
			}
		}

		// the provider's reversed policy admits the same flow
		provider := DefaultProviderSecurityPolicy(ctx)
		for j, packet := range fixturePackets(t, fixture, 45400+i) {
			ipPath, payload, err := ParseIpPathWithPayload(packet)
			if err != nil {
				t.Fatal(err)
			}
			if result, _ := provider.InspectIngress(protocol.ProvideMode_Public, ipPath, payload); result != SecurityPolicyResultAllow {
				t.Errorf("%s: provider packet %d = %v, want allow", name, j, result)
				break
			}
		}
	}

	// look-alikes without the cryptographic invariant stay dropped
	for i, name := range []string{"ethereum-discv4-bad-hash", "ethereum-rlpx-off-curve", "mse-tcp-pad211"} {
		fixture := loadSecurityFixture(t, filepath.Join("testdata", "ipsecurity", name+".json"))
		results := replayFixture(t, DefaultSecurityPolicy(ctx), fixture, 45500+i)
		if last := results[len(results)-1]; last != SecurityPolicyResultDrop {
			t.Errorf("%s: last result = %v (all %v), want drop", name, last, results)
		}
	}
}
