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
// new flows on purpose); quarantine refuses only without group-follow or once
// the episode has outlived the follow window. The gate is episode age, NOT
// receive recency -- the benching verdicts require a silent stats window and
// any receive lifts the bench, so a quarantined channel structurally never
// has recent receive evidence and a recency gate could never open.
func TestAffinityDonorEligibleVerdicts(t *testing.T) {
	parent := bindFlowTestParent()
	client := bindFlowTestChannel(parent)
	window := 45 * time.Second

	if got := client.affinityDonorEligible(true, window); got != donorEligible {
		t.Errorf("clean channel: verdict %v, want donorEligible", got)
	}

	client.setWarning(true, warnUnhealthy)
	if got := client.affinityDonorEligible(true, window); got != donorRefused {
		t.Errorf("warned channel: verdict %v, want donorRefused even with follow on", got)
	}
	client.setWarning(false, warnNone)

	// a FRESH quarantine episode follows: the exact state production creates
	// (setQuarantined stamps quarantineStart = now), no hand-set receive
	// evidence required -- this is what makes the mechanism reachable
	client.setQuarantined(blackholeNoReceiveAck)
	if got := client.affinityDonorEligible(true, window); got != donorQuarantineFollowed {
		t.Errorf("fresh quarantine: verdict %v, want donorQuarantineFollowed", got)
	}
	if got := client.affinityDonorEligible(false, window); got != donorQuarantineScattered {
		t.Errorf("quarantined, follow off: verdict %v, want donorQuarantineScattered", got)
	}
	if got := client.affinityDonorEligible(true, 0); got != donorQuarantineScattered {
		t.Errorf("quarantined, zero window: verdict %v, want donorQuarantineScattered", got)
	}

	// an episode older than the window has stopped being a false-positive
	// bet and is trending toward the drain-to-conviction execution: it must
	// stop collecting its sites' flows
	client.stateLock.Lock()
	client.quarantineStart = time.Now().Add(-2 * window)
	client.stateLock.Unlock()
	if got := client.affinityDonorEligible(true, window); got != donorQuarantineScattered {
		t.Errorf("aged quarantine: verdict %v, want donorQuarantineScattered", got)
	}

	// warning wins over quarantine: an unhealthy benched exit never donates
	client.clearQuarantine()
	client.setQuarantined(blackholeNoReceiveAck)
	client.setWarning(true, warnUnhealthy)
	if got := client.affinityDonorEligible(true, window); got != donorRefused {
		t.Errorf("warned AND quarantined: verdict %v, want donorRefused", got)
	}
}

// The inherit path end to end: a freshly benched donor keeps its own site
// under the shipped defaults -- the exact state production creates, with no
// hand-set evidence -- and the flow-level ledger counts one follow; with
// follow off the same flow is scattered and counted as such. The ledger is
// booked by the CALLER's aggregate (bookGroupLedger), never per inherit call:
// a flow whose first group scattered but whose second group donated was
// placed with its group, not scattered.
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

	// production state, nothing hand-set: a fresh episode is inside the
	// follow window by construction
	donor.setQuarantined(blackholeNoReceiveAck)

	newFlow := &multiClientChannelUpdate{}
	parent.stateLock.Lock()
	verdict, scattered := parent.inheritAffinityClient4WithLock(newFlow, donorPaths)
	parent.stateLock.Unlock()
	if newFlow.client.Load() != donor {
		t.Error("a freshly benched donor did not keep its site")
	}
	if verdict != donorQuarantineFollowed {
		t.Errorf("verdict %v, want donorQuarantineFollowed", verdict)
	}
	parent.bookGroupLedger(newFlow.client.Load() != nil, verdict == donorQuarantineFollowed, scattered)
	if got := parent.reliabilityMetrics.groupsFollowed.Load(); got != 1 {
		t.Errorf("follows counted %d, want 1", got)
	}

	// the A/B point: follow off scatters, and the ledger says so
	parent.settings.QuarantineGroupFollow = false
	scatteredFlow := &multiClientChannelUpdate{}
	parent.stateLock.Lock()
	verdict, scattered = parent.inheritAffinityClient4WithLock(scatteredFlow, donorPaths)
	parent.stateLock.Unlock()
	if scatteredFlow.client.Load() != nil {
		t.Error("with follow off, a benched donor still donated")
	}
	parent.bookGroupLedger(scatteredFlow.client.Load() != nil, verdict == donorQuarantineFollowed, scattered)
	if got := parent.reliabilityMetrics.groupsScattered.Load(); got != 1 {
		t.Errorf("scatters counted %d, want 1", got)
	}

	// and the overcount case the per-call counting shipped with: a scattered
	// group followed by a successful donation books NOTHING -- the flow was
	// placed with its group
	parent.reliabilityMetrics.reset()
	parent.bookGroupLedger(true, false, true)
	if got := parent.reliabilityMetrics.groupsScattered.Load(); got != 0 {
		t.Errorf("a placed flow booked %d scatters, want 0", got)
	}
}

// A pinned flow (host pin or app pin) follows its benched donor past the
// follow window: stability over everything short of removal. An unpinned
// flow of the same group scatters at the same moment -- the window still
// governs everyone else.
func TestPinnedFlowFollowsBenchWithoutWindow(t *testing.T) {
	parent := bindFlowTestParent()
	parent.ip4PathUpdates = map[Ip4Path]*multiClientChannelUpdate{}
	donor := bindFlowTestChannel(parent)

	flowPath := &IpPath{
		Version:         4,
		Protocol:        IpProtocolTcp,
		SourceIp:        net.IPv4(10, 0, 0, 5),
		SourcePort:      44000,
		DestinationIp:   net.IPv4(203, 0, 113, 40),
		DestinationPort: 443,
	}
	flowUpdate := &multiClientChannelUpdate{}
	flowUpdate.client.Store(donor)
	ip4 := flowPath.ToIp4Path()
	parent.ip4PathUpdates[ip4] = flowUpdate
	donorPaths := map[Ip4Path]time.Time{ip4: time.Now()}

	// a bench well past the 45s follow window
	donor.setQuarantined(blackholeNoReceiveAck)
	donor.stateLock.Lock()
	donor.quarantineStart = time.Now().Add(-2 * parent.settings.GroupFollowWindow)
	donor.stateLock.Unlock()

	unpinned := &multiClientChannelUpdate{}
	parent.stateLock.Lock()
	verdict, _ := parent.inheritAffinityClient4WithLock(unpinned, donorPaths)
	parent.stateLock.Unlock()
	if unpinned.client.Load() != nil || verdict == donorQuarantineFollowed {
		t.Error("an unpinned flow followed an aged bench: the window no longer governs")
	}

	pinnedFlow := &multiClientChannelUpdate{pinned: true}
	parent.stateLock.Lock()
	verdict, _ = parent.inheritAffinityClient4WithLock(pinnedFlow, donorPaths)
	parent.stateLock.Unlock()
	if pinnedFlow.client.Load() != donor || verdict != donorQuarantineFollowed {
		t.Error("a pinned flow scattered off an aged bench: the pin does not hold")
	}

	// but the pin's window is BOUNDED: past it, even a pinned flow scatters,
	// so a failing exit can finally drain, execute, and be replaced
	donor.stateLock.Lock()
	donor.quarantineStart = time.Now().Add(-2 * pinnedFollowWindow(parent.settings.GroupFollowWindow))
	donor.stateLock.Unlock()
	longBenched := &multiClientChannelUpdate{pinned: true}
	parent.stateLock.Lock()
	verdict, _ = parent.inheritAffinityClient4WithLock(longBenched, donorPaths)
	parent.stateLock.Unlock()
	if longBenched.client.Load() != nil {
		t.Error("a pinned flow followed a bench past even the pinned window: the exit can never drain")
	}

	// restore the fresh-enough bench for the warned-donor check below
	donor.stateLock.Lock()
	donor.quarantineStart = time.Now()
	donor.stateLock.Unlock()

	// but a WARNED donor refuses a pinned flow too: a pin is not a license
	// to board a retiring or unhealthy exit
	donor.setWarning(true, warnUnhealthy)
	refused := &multiClientChannelUpdate{pinned: true}
	parent.stateLock.Lock()
	verdict, _ = parent.inheritAffinityClient4WithLock(refused, donorPaths)
	parent.stateLock.Unlock()
	if refused.client.Load() != nil {
		t.Error("a pinned flow boarded a warned donor")
	}
}

// The app pin's group formation is the mechanism that puts an app's api
// session and all of its cdn destinations on one exit: both sendUpdate
// versions must place the flow in the app-scoped group, and both must
// consult the cross-version donor -- the affinity maps are per-ip-version,
// so without that a dual-stack app takes one exit per version, which is the
// two egress ips pinning exists to prevent.
func TestAppPinJoinsAppGroup(t *testing.T) {
	source, err := readSource("ip_remote_multi_client.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := functionBody(source, "func (self *RemoteUserNatMultiClient) sendUpdate(")
	if !ok {
		t.Fatal("could not find sendUpdate")
	}
	if got := strings.Count(body, `ServerName: appAffinityName(pin.appId)`); got < 2 {
		t.Errorf("the app group is joined in %d ip version(s), want 2", got)
	}
	if got := strings.Count(body, "self.appPinDonorWithLock("); got < 2 {
		t.Errorf("the cross-version app donor is consulted in %d ip version(s), want 2: a dual-stack pinned app splits across two exits", got)
	}
	if got := strings.Count(body, "self.recordAppPinWithLock("); got < 2 {
		t.Errorf("the app placement is recorded in %d ip version(s), want 2", got)
	}
	if !strings.Contains(body, "update.pinned = pin.pinned()") {
		t.Error("sendUpdate does not mark pinned flows; the inherit path cannot honor the pin")
	}
	// the app group REPLACES the domain groups: a pinned flow that also
	// joined youtube.com would donate its app-chosen exit to unrelated
	// youtube traffic
	if strings.Contains(body, "append([]*IpPath{{ServerName: appAffinityName") {
		t.Error("the app group is prepended to the domain groups instead of replacing them: a pinned flow contaminates every domain group it joins")
	}

	// ORDER: the app donor must be consulted BEFORE the destination bridge,
	// and the bridge must be skipped for app-pinned flows entirely. The
	// bridge places by destination ip, which on a shared cdn address is a
	// stranger's exit -- letting it win both scattered the pinned app and
	// rewrote the app's canonical exit to the stranger's.
	donorAt := strings.Index(body, "self.appPinDonorWithLock(")
	bridgeAt := strings.Index(body, "affinityFallbackIpPathsWithLock(")
	if donorAt < 0 || bridgeAt < 0 {
		t.Fatal("could not find both the app donor and the destination bridge in sendUpdate")
	}
	if bridgeAt < donorAt {
		t.Error("the destination bridge runs before the app pin donor: a pinned app is placed on whatever exit already served the destination ip")
	}
	if got := strings.Count(body, `pin.appId == ""`); got < 2 {
		t.Errorf("the destination bridge is gated on an empty app pin in %d ip version(s), want 2", got)
	}

	// and the race path records the placement, which is the ONLY path that
	// places an app's first flow of each ip version -- without it the
	// cross-version convergence never fires for the case it was built for
	bindBody, ok := functionBody(source, "func (self *RemoteUserNatMultiClient) bindClientFlow(")
	if !ok {
		t.Fatal("could not find bindClientFlow")
	}
	if !strings.Contains(bindBody, "recordAppPinWithLock(update.pinAppId") {
		t.Error("bindClientFlow does not record app-pinned placements: a dual-stack pinned app takes one exit per ip version")
	}
}

// The pinned follow window is bounded. Unbounded following is
// self-defeating: a soft verdict only executes against a FLOWLESS exit and a
// quarantine only lifts on one, so an endless stream of pinned flows keeps a
// failing exit both un-executed and un-released.
func TestPinnedFollowWindowIsBounded(t *testing.T) {
	base := 45 * time.Second
	got := pinnedFollowWindow(base)
	if got <= base {
		t.Errorf("pinned window %v does not exceed the ordinary window %v", got, base)
	}
	if 10*time.Minute < got {
		t.Errorf("pinned window %v is long enough to keep a failing exit alive indefinitely", got)
	}
	// follow disabled stays disabled
	if got := pinnedFollowWindow(0); got != 0 {
		t.Errorf("a disabled follow scaled to %v, want 0", got)
	}
}

// The bench-time hand-off latch: once per quarantine EPISODE, and a new
// episode after an acquittal gets its own. Field motivation (2026-08-03): a
// pinned app's 46 flows sat on a benched exit for 55s -- the whole
// sustained-evidence window -- before it was executed, and that hold IS the
// freeze the user experiences.
func TestQuarantineMigrateLatchIsPerEpisode(t *testing.T) {
	parent := bindFlowTestParent()
	client := bindFlowTestChannel(parent)

	// not benched: nothing to hand off
	if client.markQuarantineMigrateOnce() {
		t.Error("an unbenched exit reported a migration to run")
	}

	client.setQuarantined(blackholeNoReceiveAck)
	if !client.markQuarantineMigrateOnce() {
		t.Error("the first bench pass did not run the hand-off")
	}
	if client.markQuarantineMigrateOnce() {
		t.Error("the hand-off ran twice in one episode")
	}

	// acquitted, then benched again: a new episode, a new hand-off
	client.clearQuarantine()
	client.setQuarantined(blackholeNoReceiveSyn)
	if !client.markQuarantineMigrateOnce() {
		t.Error("a second quarantine episode did not get its own hand-off")
	}
}

// And the resize pass has to actually run it -- a correct-but-uncalled helper
// is the failure mode this suite pins against.
func TestResizeQuarantineBranchMigrates(t *testing.T) {
	source, err := readSource("ip_remote_multi_client.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := functionBody(source, "func (self *multiClientWindow) resize()")
	if !ok {
		t.Fatal("could not find resize")
	}
	at := strings.Index(body, "} else if client.isQuarantined() {")
	if at < 0 {
		t.Fatal("could not find the quarantine branch in resize")
	}
	branch := body[at:]
	if end := strings.Index(branch, "} else {"); 0 <= end {
		branch = branch[:end]
	}
	if !strings.Contains(branch, "markQuarantineMigrateOnce()") {
		t.Error("the quarantine branch does not latch a hand-off: a benched exit holds its movable flows for the whole sustained-evidence window")
	}
	if !strings.Contains(branch, "self.clientMigrateFunc(client)") {
		t.Error("the quarantine branch does not call the migration seam")
	}
}

// G-3's drain-time migration: established quic moves to a live replacement
// with its bookkeeping, tcp STAYS ALIVE on the draining exit (split-tcp: the
// exit holds the remote end, moving it would break it), and nothing is ever
// torn down or unpinned -- the whole point of migrating at drain time instead
// of at the deadline.
func TestMigrateClientFlowsMovesQuicKeepsTcp(t *testing.T) {
	parent := bindFlowTestParent()
	parent.ip4PathUpdates = map[Ip4Path]*multiClientChannelUpdate{}
	parent.reliabilityMetrics = newReliabilityMetrics()
	draining := bindFlowTestChannel(parent)
	draining.args = &multiClientChannelArgs{
		MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: NewId()},
	}
	target := bindFlowTestChannel(parent)
	target.args = &multiClientChannelArgs{
		MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: NewId()},
	}
	parent.rebindCandidatesFunc = func(dying *multiClientChannel) []*multiClientChannel {
		return []*multiClientChannel{target}
	}

	flow := func(sourcePort int, protocol IpProtocol, destPort int, established bool) *multiClientChannelUpdate {
		flowPath := &IpPath{
			Version:         4,
			Protocol:        protocol,
			SourceIp:        net.IPv4(10, 0, 0, 4),
			SourcePort:      sourcePort,
			DestinationIp:   net.IPv4(203, 0, 113, 30),
			DestinationPort: destPort,
		}
		update := newMultiClientChannelUpdate(context.Background(), flowPath)
		update.client.Store(draining)
		if established {
			update.receivedInbound.Store(true)
		}
		parent.ip4PathUpdates[flowPath.ToIp4Path()] = update
		parent.bindClientFlow(update, draining)
		return update
	}

	quicEstablished := flow(43000, IpProtocolUdp, 443, true)
	quicUnestablished := flow(43001, IpProtocolUdp, 443, false)
	tcp := flow(43002, IpProtocolTcp, 443, true)

	rebound, replacements, remaining := parent.migrateClientFlows(draining)

	if rebound != 1 || replacements != 1 {
		t.Errorf("rebound=%d replacements=%d, want 1/1", rebound, replacements)
	}
	if remaining != 2 {
		t.Errorf("remaining=%d, want 2 (the tcp flow and the unestablished quic)", remaining)
	}
	if quicEstablished.client.Load() != target {
		t.Error("the established quic flow did not move to the replacement")
	}
	// the stayers are untouched: still pinned, still booked, never nil'd
	if tcp.client.Load() != draining {
		t.Error("the tcp flow left the draining exit; split-tcp cannot move")
	}
	if quicUnestablished.client.Load() != draining {
		t.Error("the unestablished quic flow moved without a proven connection")
	}
	if !parent.clientUpdates[draining][tcp] || !parent.clientUpdates[draining][quicUnestablished] {
		t.Error("a staying flow lost its bookkeeping on the draining exit")
	}
	// and the mover's book followed it
	if parent.clientUpdates[draining][quicEstablished] {
		t.Error("the moved flow is still booked on the draining exit")
	}
	if !parent.clientUpdates[target][quicEstablished] {
		t.Error("the moved flow is not booked on its replacement")
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
