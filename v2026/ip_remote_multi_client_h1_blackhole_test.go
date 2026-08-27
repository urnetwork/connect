package connect

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"
)

// cnnDashPath models the two independent HTTP/1.1 TCP connections involved in
// the field failure: one fetches the DASH manifest and one fetches a media
// range. Both names collapse to cnn.com affinity in production.
func cnnDashPath(sourcePort int, destination string) *IpPath {
	return &IpPath{
		Version:         4,
		Protocol:        IpProtocolTcp,
		SourceIp:        net.ParseIP("10.44.0.2").To4(),
		SourcePort:      sourcePort,
		DestinationIp:   net.ParseIP(destination).To4(),
		DestinationPort: 443,
		Syn:             true,
	}
}

func bindCnnDashFlow(
	parent *RemoteUserNatMultiClient,
	client *multiClientChannel,
	path *IpPath,
	affinityKey Ip4Path,
	now time.Time,
) *multiClientChannelUpdate {
	update := newMultiClientChannelUpdate(parent.ctx, path)
	update.client.Store(client)
	update.receivedInbound.Store(true)
	update.activityTime = now
	pathKey := path.ToIp4Path()
	update.affinityIp4Paths[affinityKey] = true
	parent.ip4PathUpdates[pathKey] = update
	parent.flowUpdates[update] = true
	paths := parent.affinityIp4Paths[affinityKey]
	if paths == nil {
		paths = map[Ip4Path]time.Time{}
		parent.affinityIp4Paths[affinityKey] = paths
	}
	paths[pathKey] = now
	parent.bindClientFlow(update, client)
	return update
}

// This is the root-cause regression for the Android CNN capture. A provider
// passed admission, established the browser's H1 connections, then stopped
// returning transfer traffic. Quarantine used to preserve both split-TCP
// flows and their affinity records, so the player kept its poisoned manifest
// connection and fresh media handshakes followed the same benched exit.
//
// Recovery must be one atomic policy transition from the application's point
// of view: detach every established H1 flow, reset it toward the browser, and
// forget every DNS/site/app affinity reference to the failed exit. The next
// browser connection may then bind to a healthy provider immediately.
func TestH1DashBlackholeQuarantineResetsAndRebinds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultMultiClientSettings()
	settings.QuicRebindOnExitLoss = true
	parent := flowReaperTestParent(ctx, settings)
	parent.reliabilityMetrics = newReliabilityMetrics()
	parent.removalReceiveQueue = make(chan receivePacket, 8)
	parent.dnsExitHints = map[string]dnsExitHint{}
	parent.dnsAddressExitHints = map[netip.Addr]dnsExitHint{}

	failed := &multiClientChannel{ctx: ctx, settings: settings}
	healthy := &multiClientChannel{ctx: ctx, settings: settings}
	parent.rebindCandidatesFunc = func(client *multiClientChannel) []*multiClientChannel {
		if client != failed {
			t.Fatalf("candidate gather for %p, want failed exit %p", client, failed)
		}
		return []*multiClientChannel{healthy}
	}

	affinityName := affinityNameForServerName("gcp.amer-free.prd.media.cnn.com")
	if affinityName != affinityNameForServerName("www.cnn.com") {
		t.Fatalf("CNN page and media names do not share affinity: %q", affinityName)
	}
	affinityKey := (&IpPath{ServerName: affinityName}).ToIp4Path()
	now := time.Now()
	manifest := bindCnnDashFlow(parent, failed, cnnDashPath(44001, "198.51.100.20"), affinityKey, now)
	segment := bindCnnDashFlow(parent, failed, cnnDashPath(44002, "198.51.100.21"), affinityKey, now)

	manifestAddress := netip.MustParseAddr("198.51.100.20")
	parent.dnsExitHints[affinityName] = dnsExitHint{client: failed, affinityName: affinityName, createTime: now}
	parent.dnsAddressExitHints[manifestAddress] = dnsExitHint{client: failed, affinityName: affinityName, createTime: now}
	parent.appPinClients = map[string]*multiClientChannel{"cnn-app": failed}

	stampDonorReceived(failed)
	failed.setQuarantined(blackholeNoReceiveAck)
	rebound, _, remaining := parent.migrateClientFlows(failed, "bench")
	if rebound != 0 {
		t.Fatalf("rebound %d H1 flows, want 0: split TCP cannot migrate", rebound)
	}
	if remaining != 0 {
		t.Fatalf("failed exit retained %d flows after confirmed blackhole", remaining)
	}

	for name, update := range map[string]*multiClientChannelUpdate{"manifest": manifest, "segment": segment} {
		if !update.IsDone() {
			t.Errorf("%s flow was not retired", name)
		}
		if update.client.Load() != nil {
			t.Errorf("%s flow still points at failed provider", name)
		}
		if parent.ip4PathUpdates[update.ipPath.ToIp4Path()] == update {
			t.Errorf("%s flow remains routable after reset", name)
		}
	}
	if _, ok := parent.affinityIp4Paths[affinityKey]; ok {
		t.Fatal("CNN site affinity still contains the quarantined exit")
	}
	if _, ok := parent.dnsExitHints[affinityName]; ok {
		t.Fatal("CNN DNS name hint still points at the quarantined exit")
	}
	if _, ok := parent.dnsAddressExitHints[manifestAddress]; ok {
		t.Fatal("CNN DNS address hint still points at the quarantined exit")
	}
	if _, ok := parent.appPinClients["cnn-app"]; ok {
		t.Fatal("CNN app pin still points at the quarantined exit")
	}

	for i := 0; i < 2; i++ {
		select {
		case reset := <-parent.removalReceiveQueue:
			resetPath, err := ParseIpPath(reset.Packet)
			if err != nil || !resetPath.Rst {
				t.Fatalf("reset %d is not a TCP RST: path=%#v err=%v", i, resetPath, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("browser received only %d of 2 H1 reset signals", i)
		}
	}

	metrics := parent.ReliabilityMetrics()
	if metrics.QuarantineTcpResets != 2 {
		t.Fatalf("quarantine TCP resets = %d, want 2", metrics.QuarantineTcpResets)
	}
	if metrics.QuarantineAffinityInvalidations < 4 {
		t.Fatalf("affinity invalidations = %d, want site+name+address+app", metrics.QuarantineAffinityInvalidations)
	}

	// A fresh resolver result arrives over the healthy provider. The browser's
	// retry must inherit that provider, never the quarantined donor retained by
	// the old flow generation.
	retryAddress := netip.MustParseAddr("198.51.100.22")
	parent.dnsExitHints[affinityName] = dnsExitHint{client: healthy, affinityName: affinityName, createTime: time.Now()}
	parent.dnsAddressExitHints[retryAddress] = dnsExitHint{client: healthy, affinityName: affinityName, createTime: time.Now()}
	retryPath := cnnDashPath(44003, retryAddress.String())
	retry := newMultiClientChannelUpdate(ctx, retryPath)
	defer retry.Close()
	parent.stateLock.Lock()
	bound := parent.inheritDnsExitHintWithLock(retry, retryPath, []*IpPath{{ServerName: affinityName}})
	parent.stateLock.Unlock()
	if !bound || retry.client.Load() != healthy {
		t.Fatal("CNN retry did not bind to the healthy resolver exit")
	}
}

// Even before the parent has completed its affinity cleanup, a newly-created
// handshake must never follow a quarantined donor. This closes the small
// setQuarantined -> parent callback race and deliberately supersedes the old
// false-positive group-follow behavior for fresh flows.
func TestFreshHandshakeNeverFollowsQuarantinedAffinityDonor(t *testing.T) {
	parent := bindFlowTestParent()
	parent.ip4PathUpdates = map[Ip4Path]*multiClientChannelUpdate{}
	donor := bindFlowTestChannel(parent)
	stampDonorReceived(donor)
	donor.setQuarantined(blackholeNoReceiveAck)

	path := cnnDashPath(44100, "198.51.100.30")
	pathKey := path.ToIp4Path()
	donorUpdate := newMultiClientChannelUpdate(context.Background(), path)
	defer donorUpdate.Close()
	donorUpdate.client.Store(donor)
	parent.ip4PathUpdates[pathKey] = donorUpdate

	fresh := newMultiClientChannelUpdate(context.Background(), cnnDashPath(44101, "198.51.100.31"))
	defer fresh.Close()
	parent.stateLock.Lock()
	verdict, _ := parent.inheritAffinityClient4WithLock(fresh, map[Ip4Path]time.Time{pathKey: time.Now()})
	parent.stateLock.Unlock()
	if fresh.client.Load() != nil || verdict == donorQuarantineFollowed {
		t.Fatal("a fresh H1 handshake followed the quarantined affinity donor")
	}
}
