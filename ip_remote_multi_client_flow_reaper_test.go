package connect

import (
	"context"
	"net"
	"testing"
	"time"
)

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
