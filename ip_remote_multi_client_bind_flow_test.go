package connect

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func bindFlowTestParent() *RemoteUserNatMultiClient {
	settings := DefaultMultiClientSettings()
	return &RemoteUserNatMultiClient{
		settings:      settings,
		clientUpdates: map[*multiClientChannel]map[*multiClientChannelUpdate]bool{},
	}
}

// bindFlowTestChannel carries a live ctx: bindClientFlow asks IsDone(), which
// reads it, and a bare struct would panic where production never can.
func bindFlowTestChannel(parent *RemoteUserNatMultiClient) *multiClientChannel {
	return &multiClientChannel{ctx: context.Background(), settings: parent.settings}
}

// A flow that wins its exit from an async race is committed without the send
// path ever seeing a transition, so it was never entered in clientUpdates.
// That map is read by the flow cap, by removeClient's teardown, and by the
// blast-radius metric -- an uncounted flow is uncapped, gets no teardown, and
// is invisible in the numbers. On device this showed as 14 exits with flows
// reported on one.
func TestBindClientFlowRecordsRaceAssignedFlow(t *testing.T) {
	parent := bindFlowTestParent()
	client := bindFlowTestChannel(parent)
	update := &multiClientChannelUpdate{}

	// the race stores the winner directly, as clientReceivePacket and
	// scheduleCompleteRace both do
	update.client.Store(client)

	if 0 != len(parent.clientUpdates[client]) {
		t.Fatal("baseline: the flow should not be recorded before binding")
	}

	parent.bindClientFlow(update, client)

	if 1 != len(parent.clientUpdates[client]) {
		t.Errorf("race-assigned flow not recorded: %d entries, want 1", len(parent.clientUpdates[client]))
	}
	if !parent.clientUpdates[client][update] {
		t.Error("the recorded entry is not this flow")
	}
}

// A re-raced flow moves between exits. A stale entry on the old exit would
// inflate its flow count, count against its cap, and hand it a teardown for a
// flow it no longer carries.
func TestBindClientFlowMovesFlowOffThePreviousExit(t *testing.T) {
	parent := bindFlowTestParent()
	oldClient := bindFlowTestChannel(parent)
	newClient := bindFlowTestChannel(parent)
	update := &multiClientChannelUpdate{}

	update.client.Store(oldClient)
	parent.bindClientFlow(update, oldClient)

	// re-raced onto another exit
	update.client.Store(newClient)
	parent.bindClientFlow(update, newClient)

	if _, ok := parent.clientUpdates[oldClient]; ok {
		t.Errorf("stale entry left on the previous exit: %d entries", len(parent.clientUpdates[oldClient]))
	}
	if 1 != len(parent.clientUpdates[newClient]) {
		t.Errorf("flow not recorded on the new exit: %d entries, want 1", len(parent.clientUpdates[newClient]))
	}
}

// The race stores its winner under the per-flow leaf lock and binds after
// releasing it, so the flow can move or die in between. Binding must not
// resurrect a flow that has since moved on.
func TestBindClientFlowIgnoresStaleWinner(t *testing.T) {
	parent := bindFlowTestParent()
	raceWinner := bindFlowTestChannel(parent)
	actualClient := bindFlowTestChannel(parent)
	update := &multiClientChannelUpdate{}

	// the flow has already moved on by the time the bind runs
	update.client.Store(actualClient)
	parent.bindClientFlow(update, raceWinner)

	if 0 != len(parent.clientUpdates[raceWinner]) {
		t.Errorf("stale race winner was recorded: %d entries, want 0", len(parent.clientUpdates[raceWinner]))
	}
}

// nil arguments are the normal case: both call sites pass whatever the closure
// left behind, and most packets commit nothing.
func TestBindClientFlowNilIsNoop(t *testing.T) {
	parent := bindFlowTestParent()
	client := bindFlowTestChannel(parent)

	parent.bindClientFlow(nil, client)
	parent.bindClientFlow(&multiClientChannelUpdate{}, nil)
	parent.bindClientFlow(nil, nil)

	if 0 != len(parent.clientUpdates) {
		t.Errorf("nil bind recorded something: %d clients", len(parent.clientUpdates))
	}
}

// Binding must decline an exit that was torn down while the race ran. The
// winner is chosen under the per-flow leaf lock and bound after releasing it,
// so removeClient can land in between -- recording onto a dead exit would hand
// it a teardown list it will never process.
func TestBindClientFlowDeclinesDoneClient(t *testing.T) {
	parent := bindFlowTestParent()
	ctx, cancel := context.WithCancel(context.Background())
	client := &multiClientChannel{ctx: ctx, settings: parent.settings}
	update := &multiClientChannelUpdate{}
	update.client.Store(client)

	cancel() // the exit is removed while the race is completing
	parent.bindClientFlow(update, client)

	if 0 != len(parent.clientUpdates[client]) {
		t.Errorf("bound a flow onto a removed exit: %d entries, want 0", len(parent.clientUpdates[client]))
	}
}

// The bind is only worth anything if the assignment sites call it. Both are
// async race paths whose winner the send path never sees as a transition, so
// nothing else records them -- and a helper that is correct but uncalled is
// the failure mode this codebase has shipped more than once. This reads the
// call sites directly rather than trusting that they exist.
func TestRaceAssignmentSitesBindTheFlow(t *testing.T) {
	source, err := readSource("ip_remote_multi_client.go")
	if err != nil {
		t.Fatal(err)
	}

	for _, site := range []struct{ fn, desc string }{
		{"func (self *RemoteUserNatMultiClient) clientReceivePacket(", "receive-path race lock-in"},
		{"func (self *RemoteUserNatMultiClient) scheduleCompleteRace(", "race completion"},
	} {
		body, ok := functionBody(source, site.fn)
		if !ok {
			t.Fatalf("could not find %s", site.fn)
		}
		if !strings.Contains(body, "bindClientFlow(") {
			t.Errorf("%s does not call bindClientFlow: flows it commits are never recorded, so they are uncapped and get no teardown", site.desc)
		}
	}

	// and the send path must route through the same helper rather than adding
	// locally, or a flow can end up recorded under two clients at once
	body, ok := functionBody(source, "func (self *RemoteUserNatMultiClient) sendClientPath(")
	if !ok {
		t.Fatal("could not find sendClientPath")
	}
	if !strings.Contains(body, "bindClientFlow(") {
		t.Error("sendClientPath does not use bindClientFlow, so bookkeeping is not single-sourced")
	}
}

// The flow cap reads clientUpdates, so a flow the bookkeeping never saw is a
// flow the cap cannot bound. This is why exits exceeded a cap of 16 in the
// field.
func TestBindClientFlowMakesRacedFlowsVisibleToTheCap(t *testing.T) {
	parent := bindFlowTestParent()
	parent.settings.MaxFlowsPerExit = 2
	client := bindFlowTestChannel(parent)

	for range 3 {
		update := &multiClientChannelUpdate{}
		update.client.Store(client)
		parent.bindClientFlow(update, client)
	}

	parent.stateLock.Lock()
	atCap := parent.clientAtFlowCapWithLock(client)
	parent.stateLock.Unlock()

	if !atCap {
		t.Errorf("exit carrying 3 raced flows is not at a cap of 2: counted %d", len(parent.clientUpdates[client]))
	}
}

// A donor at the flow cap must still donate to its own affinity group: the cap
// bounds which exits collect NEW sites, never a site's growth on the exit it is
// already pinned to. The veto this replaces split a busy site's egress ip
// exactly when the site was busiest -- flow n+1 was refused its donor and raced
// onto a different exit, and services that bind sessions or signed media urls
// to the client ip then rejected the strays.
func TestAffinityInheritanceIsStickyPastTheFlowCap(t *testing.T) {
	parent := bindFlowTestParent()
	parent.settings.MaxFlowsPerExit = 2
	parent.ip4PathUpdates = map[Ip4Path]*multiClientChannelUpdate{}
	donor := bindFlowTestChannel(parent)

	// the donor exit is at its cap, all flows in one affinity group
	donorPaths := map[Ip4Path]time.Time{}
	for i := range 2 {
		flowPath := &IpPath{
			Version:         4,
			Protocol:        IpProtocolTcp,
			SourceIp:        net.IPv4(10, 0, 0, 1),
			SourcePort:      40000 + i,
			DestinationIp:   net.IPv4(203, 0, 113, 7),
			DestinationPort: 443,
		}
		flowUpdate := &multiClientChannelUpdate{}
		flowUpdate.client.Store(donor)
		parent.bindClientFlow(flowUpdate, donor)
		ip4 := flowPath.ToIp4Path()
		parent.ip4PathUpdates[ip4] = flowUpdate
		donorPaths[ip4] = time.Now()
	}

	parent.stateLock.Lock()
	if !parent.clientAtFlowCapWithLock(donor) {
		parent.stateLock.Unlock()
		t.Fatal("fixture: the donor is not at cap")
	}
	newFlow := &multiClientChannelUpdate{}
	parent.inheritAffinityClient4WithLock(newFlow, donorPaths)
	parent.stateLock.Unlock()
	if newFlow.client.Load() != donor {
		t.Error("a donor at cap did not donate: the site's next flow changes egress ip")
	}

	// the pre-change veto is the A/B point
	parent.settings.AffinityStickyPastCap = false
	vetoed := &multiClientChannelUpdate{}
	parent.stateLock.Lock()
	parent.inheritAffinityClient4WithLock(vetoed, donorPaths)
	parent.stateLock.Unlock()
	if vetoed.client.Load() != nil {
		t.Error("with the veto restored, a donor at cap still donated")
	}
}

// The destination-exit join the Local statistics screen renders: each live
// flow's destination ip is attributed to the exit CURRENTLY carrying it, so a
// re-raced flow reads as its new exit and a site split across exits shows as
// one ip with two rows.
func TestDestinationExitsJoinsFlowsToCurrentExits(t *testing.T) {
	parent := bindFlowTestParent()
	parent.ip4PathUpdates = map[Ip4Path]*multiClientChannelUpdate{}
	exitA := bindFlowTestChannel(parent)
	exitA.args = &multiClientChannelArgs{
		MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: NewId()},
	}
	exitB := bindFlowTestChannel(parent)
	exitB.args = &multiClientChannelArgs{
		MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: NewId()},
	}

	flow := func(sourcePort int, destIp net.IP, client *multiClientChannel) {
		flowPath := &IpPath{
			Version:         4,
			Protocol:        IpProtocolTcp,
			SourceIp:        net.IPv4(10, 0, 0, 3),
			SourcePort:      sourcePort,
			DestinationIp:   destIp,
			DestinationPort: 443,
		}
		// a real update (live ctx), as production registers them
		update := newMultiClientChannelUpdate(context.Background(), flowPath)
		update.client.Store(client)
		parent.ip4PathUpdates[flowPath.ToIp4Path()] = update
	}

	site := net.IPv4(203, 0, 113, 20)
	other := net.IPv4(203, 0, 113, 21)
	flow(42000, site, exitA)
	flow(42001, site, exitA)
	// the split case: one of the site's flows re-raced onto exitB
	flow(42002, site, exitB)
	flow(42003, other, exitB)

	rows := parent.DestinationExits()
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	find := func(ip net.IP, clientId Id) int {
		addr, _ := netip.AddrFromSlice(ip.To4())
		for _, row := range rows {
			if row.DestinationIp == addr && row.ClientId == clientId {
				return row.FlowCount
			}
		}
		return -1
	}
	if got := find(site, exitA.ClientId()); got != 2 {
		t.Errorf("site on exitA counted %d flows, want 2", got)
	}
	if got := find(site, exitB.ClientId()); got != 1 {
		t.Errorf("site on exitB counted %d flows, want 1", got)
	}
	if got := find(other, exitB.ClientId()); got != 1 {
		t.Errorf("other on exitB counted %d flows, want 1", got)
	}
}

// The G-1 verdict table: a resize warning always refuses (those exits shed
// new flows on purpose); quarantine refuses only without group-follow or
// without fresh receive evidence. Suspicion alone must not split a site's
// egress ip -- but a receive-silent bench gets no fresh flows.
func TestAffinityDonorEligibleVerdicts(t *testing.T) {
	parent := bindFlowTestParent()
	client := bindFlowTestChannel(parent)
	freshness := 10 * time.Second

	if got := client.affinityDonorEligible(true, freshness); got != donorEligible {
		t.Errorf("clean channel: verdict %v, want donorEligible", got)
	}

	client.setWarning(true, warnUnhealthy)
	if got := client.affinityDonorEligible(true, freshness); got != donorRefused {
		t.Errorf("warned channel: verdict %v, want donorRefused even with follow on", got)
	}
	client.setWarning(false, warnNone)

	client.setQuarantined(blackholeNoReceiveAck)
	if got := client.affinityDonorEligible(false, freshness); got != donorQuarantineScattered {
		t.Errorf("quarantined, follow off: verdict %v, want donorQuarantineScattered", got)
	}
	if got := client.affinityDonorEligible(true, 0); got != donorQuarantineScattered {
		t.Errorf("quarantined, zero freshness: verdict %v, want donorQuarantineScattered", got)
	}
	if got := client.affinityDonorEligible(true, freshness); got != donorQuarantineScattered {
		t.Errorf("quarantined, never received: verdict %v, want donorQuarantineScattered", got)
	}

	client.stateLock.Lock()
	client.lastReceiveAckTime = time.Now()
	client.stateLock.Unlock()
	if got := client.affinityDonorEligible(true, freshness); got != donorQuarantineFollowed {
		t.Errorf("quarantined, receive fresh: verdict %v, want donorQuarantineFollowed", got)
	}

	client.stateLock.Lock()
	client.lastReceiveAckTime = time.Now().Add(-2 * freshness)
	client.stateLock.Unlock()
	if got := client.affinityDonorEligible(true, freshness); got != donorQuarantineScattered {
		t.Errorf("quarantined, receive stale: verdict %v, want donorQuarantineScattered", got)
	}

	// warning wins over quarantine: an unhealthy benched exit never donates
	client.setWarning(true, warnUnhealthy)
	client.stateLock.Lock()
	client.lastReceiveAckTime = time.Now()
	client.stateLock.Unlock()
	if got := client.affinityDonorEligible(true, freshness); got != donorRefused {
		t.Errorf("warned AND quarantined: verdict %v, want donorRefused", got)
	}
}

// The inherit path end to end: a quarantined donor with fresh receive
// evidence keeps its own site under the shipped defaults, and the ledger
// counts the follow; with follow off the same flow is scattered and counted
// as such.
func TestQuarantinedDonorKeepsItsSite(t *testing.T) {
	parent := bindFlowTestParent()
	parent.ip4PathUpdates = map[Ip4Path]*multiClientChannelUpdate{}
	parent.reliabilityMetrics = newReliabilityMetrics()
	donor := bindFlowTestChannel(parent)

	flowPath := &IpPath{
		Version:         4,
		Protocol:        IpProtocolTcp,
		SourceIp:        net.IPv4(10, 0, 0, 2),
		SourcePort:      41000,
		DestinationIp:   net.IPv4(203, 0, 113, 9),
		DestinationPort: 443,
	}
	flowUpdate := &multiClientChannelUpdate{}
	flowUpdate.client.Store(donor)
	ip4 := flowPath.ToIp4Path()
	parent.ip4PathUpdates[ip4] = flowUpdate
	donorPaths := map[Ip4Path]time.Time{ip4: time.Now()}

	donor.setQuarantined(blackholeNoReceiveAck)
	donor.stateLock.Lock()
	donor.lastReceiveAckTime = time.Now()
	donor.stateLock.Unlock()

	newFlow := &multiClientChannelUpdate{}
	parent.stateLock.Lock()
	parent.inheritAffinityClient4WithLock(newFlow, donorPaths)
	parent.stateLock.Unlock()
	if newFlow.client.Load() != donor {
		t.Error("a benched receive-fresh donor did not keep its site")
	}
	if got := parent.reliabilityMetrics.groupsFollowed.Load(); got != 1 {
		t.Errorf("follows counted %d, want 1", got)
	}

	// the A/B point: follow off scatters, and the ledger says so
	parent.settings.QuarantineGroupFollow = false
	scatteredFlow := &multiClientChannelUpdate{}
	parent.stateLock.Lock()
	parent.inheritAffinityClient4WithLock(scatteredFlow, donorPaths)
	parent.stateLock.Unlock()
	if scatteredFlow.client.Load() != nil {
		t.Error("with follow off, a benched donor still donated")
	}
	if got := parent.reliabilityMetrics.groupsScattered.Load(); got != 1 {
		t.Errorf("scatters counted %d, want 1", got)
	}
}

// A cdn constellation must collapse to ONE affinity group: the service binds
// state across its domains (signed media urls carry the client ip the manifest
// was fetched from), so the site domain and its cdn domains have to share an
// exit or the media requests present the wrong egress ip.
func TestAffinityNameCollapsesCdnConstellations(t *testing.T) {
	// the full chain: subdomain -> base domain -> constellation alias
	if got := affinityNameForServerName("r3---sn-ab5l6ne7.googlevideo.com"); got != "youtube.com" {
		t.Errorf("googlevideo collapsed to %q, want youtube.com", got)
	}
	if got := affinityNameForServerName("i.ytimg.com"); got != "youtube.com" {
		t.Errorf("ytimg collapsed to %q, want youtube.com", got)
	}
	// a site outside the table gets base-domain collapse and nothing else
	if got := affinityNameForServerName("b.c.example.com"); got != "example.com" {
		t.Errorf("example collapsed to %q, want example.com", got)
	}
	// values are canonical: an alias chain would put flows one hop apart in
	// different groups, which is the exact split the table exists to prevent
	for from, to := range domainAffinityAliases {
		if from == to {
			t.Errorf("alias %q maps to itself", from)
		}
		if _, ok := domainAffinityAliases[to]; ok {
			t.Errorf("alias %q -> %q chains: %q is itself aliased", from, to, to)
		}
	}
}
