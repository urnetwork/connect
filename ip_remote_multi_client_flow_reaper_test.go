package connect

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"
)

// A new WireGuard stack can reuse its peer address while its predecessor's
// UDP flows are still retained by the hosted multi-client. Expiring an old
// flow must quote its old port, not a current socket's port. Such a quotation
// is expected stale-flow evidence, not evidence that NAT changed a port.
func TestFlowReaperUDPPreviousPeerGenerationKeepsOriginalPort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	settings.DestinationAffinity = false
	if settings.SequenceIdleTimeout != 120*time.Second || !settings.UdpTeardownSignal {
		t.Fatal("this control must exercise the actual two-minute UDP teardown defaults")
	}
	parent := flowReaperTestParent(ctx, settings)
	parent.reliabilityMetrics = newReliabilityMetrics()
	oldPath := flowReaperTestPath(4, IpProtocolUdp, 54321)
	oldPath.SourceIp = net.IPv4(169, 254, 7, 9).To4()
	oldPath.DestinationIp = net.IPv4(1, 1, 1, 1).To4()
	oldPath.DestinationPort = 53
	newPath := *oldPath
	newPath.SourcePort = 54322
	old, _, _ := parent.sendUpdate(oldPath, flowPin{})
	fresh, _, _ := parent.sendUpdate(&newPath, flowPin{})
	if old == nil || fresh == nil || old == fresh {
		t.Fatal("distinct source ports did not acquire distinct owned flow generations")
	}
	defer old.Close()
	defer fresh.Close()
	// Advance only the explicit reaper clock; never wait for a wall timer.
	started := time.Unix(1234567890, 0)
	old.activityTime = started
	fresh.activityTime = started.Add(119 * time.Second)
	deadline := started.Add(settings.SequenceIdleTimeout)
	retired, delay, live := parent.detachIdleFlows(deadline.Add(-time.Nanosecond))
	if len(retired) != 0 || !live || delay != time.Nanosecond {
		t.Fatal("old UDP flow expired before its exact default deadline")
	}
	retired, delay, live = parent.detachIdleFlows(deadline)
	if len(retired) != 1 || retired[0].update != old || !retired[0].shouldSignal || !live || delay != 119*time.Second {
		t.Fatal("deadline did not retire only the previous peer generation")
	}
	parent.finishRetiredFlows(retired)
	select {
	case returned := <-parent.removalReceiveQueue:
		packet := returned.Packet
		if len(packet) != 56 || packet[9] != 1 || packet[20] != 3 || packet[21] != 3 || returned.IpPath.SourcePort != 54321 {
			t.Fatal("old flow did not generate its own 56-byte ICMP3/3 teardown")
		}
		peer := netip.MustParseAddr("10.55.12.34")
		if !RewriteIpv4Destination(packet, peer) {
			t.Fatal("restore the same peer address after reconnect")
		}
		assertNatChecksums(t, packet, "previous generation envelope")
		assertNatChecksums(t, packet[28:], "previous generation quotation")
		quote, ok := ipParseIcmpEmbeddedPath(packet[28:])
		if !ok || !quote.SourceIp.Equal(net.IP(peer.AsSlice())) || quote.SourcePort != 54321 ||
			!quote.DestinationIp.Equal(newPath.DestinationIp) || quote.DestinationPort != newPath.DestinationPort {
			t.Fatal("NAT changed a quoted port or another field of the old flow")
		}
	default:
		t.Fatal("old flow expiration did not enqueue its teardown")
	}
	if fresh.IsDone() || parent.ip4PathUpdates[newPath.ToIp4Path()] != fresh || parent.ip4PathUpdates[oldPath.ToIp4Path()] != nil {
		t.Fatal("old teardown disturbed the fresh source-port generation")
	}
	retired, _, _ = parent.detachIdleFlows(deadline)
	parent.finishRetiredFlows(retired)
	if len(retired) != 0 || len(parent.removalReceiveQueue) != 0 {
		t.Fatal("one old generation generated more than one idle teardown")
	}
}

func flowReaperTestParent(ctx context.Context, settings *MultiClientSettings) *RemoteUserNatMultiClient {
	return &RemoteUserNatMultiClient{
		ctx:                 ctx,
		settings:            settings,
		log:                 loggerOrDefault(nil),
		ip4PathUpdates:      map[Ip4Path]*multiClientChannelUpdate{},
		ip6PathUpdates:      map[Ip6Path]*multiClientChannelUpdate{},
		flowUpdates:         map[*multiClientChannelUpdate]bool{},
		affinityIp4Paths:    map[Ip4Path]map[Ip4Path]time.Time{},
		affinityIp6Paths:    map[Ip6Path]map[Ip6Path]time.Time{},
		clientUpdates:       map[*multiClientChannel]map[*multiClientChannelUpdate]bool{},
		flowReaperWake:      make(chan struct{}, 1),
		removalReceiveQueue: make(chan receivePacket, 1),
	}
}

func flowReaperTestPath(version int, protocol IpProtocol, sourcePort int) *IpPath {
	sourceIp := net.ParseIP("10.44.0.2")
	destinationIp := net.ParseIP("198.51.100.44")
	if version == 6 {
		sourceIp = net.ParseIP("fd00::2")
		destinationIp = net.ParseIP("2001:db8::44")
	}
	return &IpPath{
		Version:         version,
		Protocol:        protocol,
		SourceIp:        sourceIp,
		SourcePort:      sourcePort,
		DestinationIp:   destinationIp,
		DestinationPort: 443,
	}
}

func TestDetachIdleFlowsCleansRoutingAndPreservesNextDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultMultiClientSettings()
	settings.SequenceIdleTimeout = time.Minute
	settings.TcpSequenceIdleTimeout = 10 * time.Minute
	parent := flowReaperTestParent(ctx, settings)
	now := time.Now()

	expiredPath := flowReaperTestPath(4, IpProtocolUdp, 41001)
	expired := newMultiClientChannelUpdate(ctx, expiredPath)
	expired.activityTime = now.Add(-2 * time.Minute)
	expiredKey := expiredPath.ToIp4Path()
	affinityKey := (&IpPath{Version: 4, DestinationIp: expiredPath.DestinationIp}).ToIp4Path()
	expired.affinityIp4Paths[affinityKey] = true
	client := &multiClientChannel{ctx: ctx}
	expired.client.Store(client)
	parent.ip4PathUpdates[expiredKey] = expired
	parent.flowUpdates[expired] = true
	parent.affinityIp4Paths[affinityKey] = map[Ip4Path]time.Time{expiredKey: now}
	parent.clientUpdates[client] = map[*multiClientChannelUpdate]bool{expired: true}

	livePath := flowReaperTestPath(6, IpProtocolTcp, 41002)
	live := newMultiClientChannelUpdate(ctx, livePath)
	live.activityTime = now
	liveKey := livePath.ToIp6Path()
	parent.ip6PathUpdates[liveKey] = live
	parent.flowUpdates[live] = true

	retired, nextDelay, hasNext := parent.detachIdleFlows(now)
	if len(retired) != 1 || retired[0].update != expired {
		t.Fatalf("retired flows = %#v, want only expired IPv4 flow", retired)
	}
	if !retired[0].shouldSignal {
		t.Fatal("an ordinary idle expiration must retain the historical teardown signal")
	}
	if _, ok := parent.ip4PathUpdates[expiredKey]; ok {
		t.Fatal("expired flow remained in the IPv4 routing table")
	}
	if _, ok := parent.affinityIp4Paths[affinityKey]; ok {
		t.Fatal("expired flow remained in its affinity group")
	}
	if _, ok := parent.clientUpdates[client]; ok {
		t.Fatal("expired flow remained in client flow bookkeeping")
	}
	if parent.ip6PathUpdates[liveKey] != live {
		t.Fatal("live IPv6 flow was detached with the expired flow")
	}
	if !hasNext || nextDelay < 9*time.Minute || 10*time.Minute < nextDelay {
		t.Fatalf("next delay = %v, want live TCP deadline near 10m", nextDelay)
	}

	for _, flow := range retired {
		flow.update.Close()
	}
	live.Close()
}

func TestFlowReaperExpiresFlowsWithoutPerFlowWaiters(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultMultiClientSettings()
	settings.SequenceIdleTimeout = 15 * time.Millisecond
	parent := flowReaperTestParent(ctx, settings)
	path := flowReaperTestPath(4, IpProtocolUdp, 42001)
	update := newMultiClientChannelUpdate(ctx, path)
	update.activityTime = time.Now()
	key := path.ToIp4Path()
	parent.ip4PathUpdates[key] = update
	parent.flowUpdates[update] = true

	done := make(chan struct{})
	go func() {
		parent.runFlowReaper()
		close(done)
	}()
	parent.notifyFlowReaper()

	deadline := time.Now().Add(2 * time.Second)
	for {
		parent.stateLock.Lock()
		_, present := parent.ip4PathUpdates[key]
		parent.stateLock.Unlock()
		if !present {
			break
		}
		if deadline.Before(time.Now()) {
			t.Fatal("shared flow reaper did not expire the flow")
		}
		time.Sleep(time.Millisecond)
	}
	if !update.IsDone() {
		t.Fatal("retired flow context remains live")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shared flow reaper did not stop with its parent")
	}
}

// Sticky affinity preserves one provider/IP for a busy site, but it must not
// retain hundreds of idle H1 connections on that exit. Once the steady bound
// is exceeded, the shared reaper retires only the oldest idle excess; active
// flows stay on the same provider and no mid-session egress-IP split occurs.
func TestFlowReaperBoundsStickyExitWithOldestIdleFlows(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultMultiClientSettings()
	settings.MaxStickyFlowsPerExit = 2
	settings.StickyFlowIdleTimeout = 30 * time.Second
	parent := flowReaperTestParent(ctx, settings)
	parent.reliabilityMetrics = newReliabilityMetrics()
	client := &multiClientChannel{ctx: ctx, settings: settings}
	now := time.Now()

	updates := make([]*multiClientChannelUpdate, 0, 3)
	for i := 0; i < 3; i++ {
		path := flowReaperTestPath(4, IpProtocolTcp, 45000+i)
		update := newMultiClientChannelUpdate(ctx, path)
		update.activityTime = now.Add(-time.Duration(40-i*15) * time.Second)
		update.client.Store(client)
		parent.ip4PathUpdates[path.ToIp4Path()] = update
		parent.flowUpdates[update] = true
		parent.bindClientFlow(update, client)
		updates = append(updates, update)
	}

	retired, _, _ := parent.detachIdleFlows(now)
	if len(retired) != 1 || retired[0].update != updates[0] {
		t.Fatalf("retired %#v, want only the oldest idle excess flow", retired)
	}
	if updates[1].client.Load() != client || updates[2].client.Load() != client {
		t.Fatal("the sticky site's active flows changed provider")
	}
	if got := len(parent.clientUpdates[client]); got != 2 {
		t.Fatalf("sticky exit retains %d flows, want steady bound 2", got)
	}

	parent.finishRetiredFlows(retired)
	for _, update := range updates[1:] {
		update.Close()
	}
	if got := parent.ReliabilityMetrics().StickyFlowsRetired; got != 1 {
		t.Fatalf("sticky flows retired = %d, want 1", got)
	}
}
