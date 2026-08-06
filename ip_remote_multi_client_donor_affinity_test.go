package connect

import (
	"context"
	"net"
	"testing"
	"time"
)

// The donor-affinity contract this file pins was the deciding argument in a
// merge conflict: a pre-merge fix had update.Close() clear the committed
// client ("a retired flow must not retain its selected provider"), and the
// reliability checkpoint removed that clear. The removal is deliberate and
// load-bearing — the affinity donor read deliberately looks at RETIRED flows'
// updates, IsDone-guarded, so the next flow to the same site inherits the
// exit the site was just using (providers terminate tcp, so keeping a site's
// flows on one exit is what keeps its connections working). Re-adding the
// clear would silently break site affinity for every reconnect-after-close,
// which is most of them: a browser reopening a connection to the site it just
// closed one to.
//
// The retention is BOUNDED, not indefinite: every flow spawned through
// sendUpdate has a reaper goroutine that, once the update is done, removes it
// from ip4PathUpdates and its affinity groups and nils the committed client —
// atomically under the parent stateLock, so there is no observable
// registered-but-cleared state. The donor window is therefore "from Close
// until the reaper runs". These tests construct the retired flow's
// registration directly (the suite's established fixture pattern) rather
// than through sendUpdate, so the reaper does not race the assertion — an
// earlier version used sendUpdate and lost that race under -race scheduling.

func donorAffinityPath(sourcePort int) *IpPath {
	return &IpPath{
		Version:         4,
		Protocol:        IpProtocolTcp,
		SourceIp:        net.ParseIP("10.20.30.40"),
		SourcePort:      sourcePort,
		DestinationIp:   net.ParseIP("198.51.100.7"),
		DestinationPort: 443,
	}
}

// registerDonorFlow installs a committed flow record and its affinity-group
// membership exactly as sendUpdate would, minus the reaper goroutine, so the
// tests control when (whether) the record is reaped.
func registerDonorFlow(mc *RemoteUserNatMultiClient, ipPath *IpPath, exit *multiClientChannel) *multiClientChannelUpdate {
	update := newMultiClientChannelUpdate(mc.ctx, ipPath)
	update.client.Store(exit)
	ip4Path := ipPath.ToIp4Path()
	mc.stateLock.Lock()
	defer mc.stateLock.Unlock()
	mc.ip4PathUpdates[ip4Path] = update
	for _, affinityIpPath := range mc.affinityIpPathsWithLock(ipPath) {
		affinityIp4Path := affinityIpPath.ToIp4Path()
		update.affinityIp4Paths[affinityIp4Path] = true
		paths, ok := mc.affinityIp4Paths[affinityIp4Path]
		if !ok {
			paths = map[Ip4Path]time.Time{}
			mc.affinityIp4Paths[affinityIp4Path] = paths
		}
		paths[ip4Path] = time.Now()
	}
	if updates, ok := mc.clientUpdates[exit]; ok {
		updates[update] = true
	} else {
		mc.clientUpdates[exit] = map[*multiClientChannelUpdate]bool{update: true}
	}
	return update
}

func donorAffinityClient(ctx context.Context) *RemoteUserNatMultiClient {
	mc := &RemoteUserNatMultiClient{
		ctx: ctx,
		settings: &MultiClientSettings{
			DestinationAffinity:    true,
			SequenceIdleTimeout:    10 * time.Minute,
			TcpSequenceIdleTimeout: 10 * time.Minute,
		},
		ip4PathUpdates:   map[Ip4Path]*multiClientChannelUpdate{},
		ip6PathUpdates:   map[Ip6Path]*multiClientChannelUpdate{},
		affinityIp4Paths: map[Ip4Path]map[Ip4Path]time.Time{},
		affinityIp6Paths: map[Ip6Path]map[Ip6Path]time.Time{},
		clientUpdates:    map[*multiClientChannel]map[*multiClientChannelUpdate]bool{},
	}
	mc.config.Store(&multiClientConfig{})
	return mc
}

// TestDonorAffinityRetiredFlowDonatesItsExit: a flow that commits to an exit
// and then closes must still donate that exit to the next flow to the same
// site. This is the exact property update.Close clearing the committed client
// would destroy.
func TestDonorAffinityRetiredFlowDonatesItsExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mc := donorAffinityClient(ctx)
	exit := &multiClientChannel{ctx: ctx}

	// flow 1 commits to the exit, then retires (the connection closed). The
	// registration is built directly so the flow reaper does not race the
	// donor read below (see the file doc).
	flow1 := donorAffinityPath(40001)
	update1 := registerDonorFlow(mc, flow1, exit)
	update1.Close()

	if got := update1.client.Load(); got != exit {
		t.Fatal("a retired flow's update must retain its last exit: the donor read depends on it, and clearing it silently breaks site affinity for every reconnect-after-close")
	}

	// flow 2 to the same site inherits the retired flow's exit
	update2, _, current2 := mc.sendUpdate(donorAffinityPath(40002), flowPin{})
	if current2 != exit || update2.client.Load() != exit {
		t.Fatalf(
			"a new flow to the same site must inherit the retired flow's exit (got current=%p committed=%p want %p)",
			current2, update2.client.Load(), exit,
		)
	}
}

// TestDonorAffinityDeadExitIsNotInherited is the guard on the guard: the
// retained pointer is only usable through the IsDone check, so a dead exit is
// never donated. Retention without this check would hand new flows a corpse.
func TestDonorAffinityDeadExitIsNotInherited(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mc := donorAffinityClient(ctx)
	exitCtx, cancelExit := context.WithCancel(context.Background())
	exit := &multiClientChannel{ctx: exitCtx}

	flow1 := donorAffinityPath(40001)
	update1 := registerDonorFlow(mc, flow1, exit)
	update1.Close()

	// the exit dies after the flow retired
	cancelExit()

	update2, _, current2 := mc.sendUpdate(donorAffinityPath(40002), flowPin{})
	if current2 == exit || update2.client.Load() == exit {
		t.Fatal("a dead exit must never be donated: the retention is only safe because the donor read is IsDone-guarded")
	}
}

// TestDonorAffinityWarnedExitIsNotInherited pins the second eligibility gate:
// a warned exit (dial-starved, quarantine-scarred) is refused as a donor even
// while alive, so a site does not keep re-boarding an exit the window is
// steering away from.
func TestDonorAffinityWarnedExitIsNotInherited(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mc := donorAffinityClient(ctx)
	exit := &multiClientChannel{ctx: ctx}
	func() {
		exit.stateLock.Lock()
		defer exit.stateLock.Unlock()
		exit.warning = true
	}()

	flow1 := donorAffinityPath(40001)
	update1 := registerDonorFlow(mc, flow1, exit)
	update1.Close()

	update2, _, current2 := mc.sendUpdate(donorAffinityPath(40002), flowPin{})
	if current2 == exit || update2.client.Load() == exit {
		t.Fatal("a warned exit must be refused as a donor")
	}
}
