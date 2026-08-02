package connect

import (
	"os"
	"strings"
	"testing"
	"time"
)

// The two blackhole signals are not equally strong and must not share a bound.
//
// A provider that acknowledges nothing is gone. A provider that acknowledges
// our sends but returns no destination data is demonstrably alive, and may
// simply be carrying a flow that is waiting on a slow origin. Removing an exit
// destroys every flow pinned to it, not just the quiet one, so the ambiguous
// case has to clear a higher bar.
//
// On mainnet the shared 5s bound removed 44 providers out of 44 -- roughly one
// every 18s under load -- and every one of those providers was still
// acknowledging sends, up to 602 of them / 222KB.
func TestBlackholeReceiveTimeoutIsSeparateFromSendTimeout(t *testing.T) {
	settings := DefaultMultiClientSettings()

	if settings.BlackholeReceiveTimeout <= settings.BlackholeTimeout {
		t.Errorf(
			"receive bound %v must be longer than the send bound %v: it is the weaker signal",
			settings.BlackholeReceiveTimeout, settings.BlackholeTimeout,
		)
	}
}

// The receive bound is compared against an age derived from surviving stat
// buckets, and coalesceEventBuckets drops every bucket older than
// StatsWindowDuration. So the age can never exceed roughly
// StatsWindowDuration + StatsWindowBucketDuration, and a receive bound at or
// above that ceiling never fires -- silently, with no error and no log.
//
// This guards the margin. Without it, reducing StatsWindowDuration to make
// stats more responsive would permanently disable the check while every other
// test still passed.
func TestBlackholeReceiveTimeoutIsReachable(t *testing.T) {
	settings := DefaultMultiClientSettings()

	ceiling := settings.StatsWindowDuration + settings.StatsWindowBucketDuration
	if ceiling <= settings.BlackholeReceiveTimeout {
		t.Fatalf(
			"receive bound %v is at or above the reachable ceiling %v (StatsWindowDuration %v + bucket %v): it can never fire",
			settings.BlackholeReceiveTimeout, ceiling,
			settings.StatsWindowDuration, settings.StatsWindowBucketDuration,
		)
	}

	// require real headroom, not a single bucket of luck
	if margin := ceiling - settings.BlackholeReceiveTimeout; margin < 2*settings.StatsWindowBucketDuration {
		t.Errorf(
			"receive bound %v leaves only %v under the ceiling %v; want at least %v",
			settings.BlackholeReceiveTimeout, margin, ceiling, 2*settings.StatsWindowBucketDuration,
		)
	}
}

// A provider acknowledging sends with nothing back must survive the shorter
// send bound -- this is the case that was killing healthy exits.
func TestAckingProviderSurvivesShortWindow(t *testing.T) {
	stats := &clientWindowStats{
		log:               DefaultLogger(),
		firstSendNackTime: time.Now().Add(-10 * time.Second),
		sendAckCount:      7,
		sendAckByteCount:  360,
		receiveAckCount:   0,
	}

	reason, _ := blackholeReasonFromStats(time.Now(), stats, 5*time.Second, 20*time.Second, 30*time.Second, blackholeGates{})
	if reason != blackholeNone {
		t.Errorf("a provider still acknowledging sends was removed on the send bound: %s", reason)
	}

	// once the longer receive bound elapses it is removed, and the reason says
	// which signal did it
	stats.firstSendNackTime = time.Now().Add(-21 * time.Second)
	reason, _ = blackholeReasonFromStats(time.Now(), stats, 5*time.Second, 20*time.Second, 30*time.Second, blackholeGates{})
	if reason != blackholeNoReceiveAck {
		t.Errorf("past the receive bound: reason = %q, want %q", reason, blackholeNoReceiveAck)
	}
}

// A provider acknowledging nothing is unambiguously gone and must still be
// removed quickly -- the fix must not slow down the case that works.
func TestSilentProviderStillRemovedOnSendBound(t *testing.T) {
	stats := &clientWindowStats{
		log:               DefaultLogger(),
		firstSendNackTime: time.Now().Add(-10 * time.Second),
		sendAckCount:      0,
		receiveAckCount:   0,
	}

	reason, _ := blackholeReasonFromStats(time.Now(), stats, 5*time.Second, 20*time.Second, 30*time.Second, blackholeGates{})
	if reason != blackholeNoSendAck {
		t.Errorf("silent provider: reason = %q, want %q", reason, blackholeNoSendAck)
	}
}

// 0 disables the receive check, leaving only the unambiguous signal. This is
// the setting to compare against when measuring how much churn the receive
// branch is responsible for.
func TestBlackholeReceiveTimeoutZeroDisables(t *testing.T) {
	stats := &clientWindowStats{
		log:               DefaultLogger(),
		firstSendNackTime: time.Now().Add(-10 * time.Minute),
		sendAckCount:      7,
		receiveAckCount:   0,
	}

	reason, _ := blackholeReasonFromStats(time.Now(), stats, 5*time.Second, 0, 30*time.Second, blackholeGates{})
	if reason != blackholeNone {
		t.Errorf("receive check ran with the bound disabled: %s", reason)
	}
}

// Unanswered syns alone must not remove an exit whose established traffic is
// flowing. The syn-ack only exists after the provider's upstream dial
// succeeds, so "syns out, none back" cannot distinguish a broken provider from
// a destination that silently drops connections -- and on device this branch
// removed an exit moving 48 packets / 8.7KB of return traffic because ~18 syns
// to a handful of unresponsive destinations went unanswered, destroying 276
// working connections. Removal is reserved for an exit that has established
// nothing at all; a live exit's connect trouble is handled per-flow by the
// dial-failure re-race and the dial-strike warning instead.
func TestBlackholeSynBranchSparesEstablishedTraffic(t *testing.T) {
	// the field case: syns unanswered past the bound, established flows moving
	stats := &clientWindowStats{
		log:                 DefaultLogger(),
		firstSendSynTime:    time.Now().Add(-31 * time.Second),
		sendSynCount:        18,
		receiveSynCount:     0,
		sendAckCount:        78,
		sendAckByteCount:    11071,
		receiveAckCount:     48,
		receiveAckByteCount: 8712,
	}

	reason, _ := blackholeReasonFromStats(time.Now(), stats, 5*time.Second, 20*time.Second, 30*time.Second, blackholeGates{})
	if reason != blackholeNone {
		t.Errorf("an exit with flowing established traffic was removed for unanswered syns: %s", reason)
	}

	// with nothing established the same syn silence still removes -- the fix
	// must not blind the branch to an exit that never worked at all
	stats.receiveAckCount = 0
	stats.receiveAckByteCount = 0
	reason, _ = blackholeReasonFromStats(time.Now(), stats, 5*time.Second, 20*time.Second, 30*time.Second, blackholeGates{})
	if reason != blackholeNoReceiveSyn {
		t.Errorf("an exit that established nothing was kept: reason = %q, want %q", reason, blackholeNoReceiveSyn)
	}
}

// The connect-wait clock behind the client-side dial-failure inference: it
// arms on the first syn an exit carries for the flow, trips only after the
// timeout on that same exit, and restarts when the flow moves -- otherwise the
// first syn through a fresh exit would inherit the old exit's wait and strike
// it immediately.
func TestSynWaitExceededIsPerExit(t *testing.T) {
	update := &multiClientChannelUpdate{}
	exitA := &multiClientChannel{}
	exitB := &multiClientChannel{}
	// wide enough that a scheduler or GC stall between two adjacent calls
	// cannot age the clock past the bound and fail the "does not trip"
	// assertions spuriously on a loaded runner
	timeout := 250 * time.Millisecond

	// first syn arms the clock, nothing trips
	if update.synWaitExceeded(exitA, timeout) {
		t.Fatal("first syn tripped the clock")
	}
	// a retransmit inside the window does not trip
	if update.synWaitExceeded(exitA, timeout) {
		t.Fatal("retransmit inside the window tripped the clock")
	}

	time.Sleep(timeout + 50*time.Millisecond)

	// moving to a new exit must restart the wait, not inherit the old one
	if update.synWaitExceeded(exitB, timeout) {
		t.Fatal("a fresh exit inherited the previous exit's wait")
	}

	time.Sleep(timeout + 50*time.Millisecond)

	// the same exit past the timeout trips
	if !update.synWaitExceeded(exitB, timeout) {
		t.Fatal("an aged wait on the same exit did not trip")
	}
	// and tripping restarts the clock rather than firing on every retransmit
	if update.synWaitExceeded(exitB, timeout) {
		t.Fatal("the clock did not restart after tripping")
	}
}

// The connect signal is independent of the send/receive bounds and keeps its
// own reason, so a capture can tell the branches apart. They previously all
// reported an identical error string, which is why 44 field removals could not
// be attributed to a branch.
func TestBlackholeReasonsAreDistinct(t *testing.T) {
	noSyn := &clientWindowStats{
		log:               DefaultLogger(),
		firstSendSynTime:  time.Now().Add(-31 * time.Second),
		firstSendNackTime: time.Time{},
		receiveSynCount:   0,
	}

	reason, _ := blackholeReasonFromStats(time.Now(), noSyn, 5*time.Second, 20*time.Second, 30*time.Second, blackholeGates{})
	if reason != blackholeNoReceiveSyn {
		t.Errorf("no syn back: reason = %q, want %q", reason, blackholeNoReceiveSyn)
	}

	seen := map[blackholeReason]bool{}
	for _, r := range []blackholeReason{
		blackholeNone, blackholeNoSendAck, blackholeNoReceiveAck, blackholeNoReceiveSyn,
	} {
		if seen[r] {
			t.Errorf("duplicate blackhole reason %q -- a capture could not tell them apart", r)
		}
		seen[r] = true
	}
}

// A clean window is not a blackhole by any signal.
func TestHealthyProviderIsNotBlackholed(t *testing.T) {
	stats := &clientWindowStats{
		log:               DefaultLogger(),
		firstSendNackTime: time.Now().Add(-60 * time.Second),
		firstSendSynTime:  time.Now().Add(-60 * time.Second),
		sendAckCount:      100,
		receiveAckCount:   100,
		receiveSynCount:   3,
	}

	reason, _ := blackholeReasonFromStats(time.Now(), stats, 5*time.Second, 20*time.Second, 30*time.Second, blackholeGates{})
	if reason != blackholeNone {
		t.Errorf("healthy provider removed: %s", reason)
	}
}

// detectBlackhole must actually consult the decision, and must pass the
// runtime-override receive bound rather than the static setting. Without this
// the decision function could be correct and simply not used -- which is the
// shape of bug that has already shipped here more than once.
func TestDetectBlackholeUsesTheReasonAndOverride(t *testing.T) {
	source, err := readSource("ip_remote_multi_client.go")
	if err != nil {
		t.Fatal(err)
	}

	body, ok := functionBody(source, "func (self *multiClientChannel) detectBlackhole()")
	if !ok {
		t.Fatal("could not find detectBlackhole")
	}

	if !strings.Contains(body, "blackholeReasonFromStats(") {
		t.Error("detectBlackhole does not call blackholeReasonFromStats: the decision is not reached")
	}
	if !strings.Contains(body, "self.reliabilitySettings().BlackholeReceiveTimeout") {
		t.Error("detectBlackhole does not read the receive bound from the runtime override, so the developer control has no effect")
	}
	if !strings.Contains(body, "reason") {
		t.Error("detectBlackhole does not report the reason, so a capture cannot attribute a removal to a branch")
	}
}

// The receive verdicts convict on silence, and during a network migration
// silence is tunnel-wide: nothing from any provider can arrive, so every exit
// looks identically guilty. On device one wifi migration executed 7 exits in
// 79s, every verdict `no-receive-ack recv 0/0B`. While the uplink is stale
// those verdicts must be held -- and reported as held, so the gate is
// countable rather than silently eating verdicts.
func TestBlackholeUplinkStaleHoldsReceiveVerdicts(t *testing.T) {
	// the field case: acking provider, nothing received, past the receive bound
	stats := &clientWindowStats{
		log:               DefaultLogger(),
		firstSendNackTime: time.Now().Add(-21 * time.Second),
		sendAckCount:      7,
		receiveAckCount:   0,
	}

	reason, held := blackholeReasonFromStats(time.Now(), stats, 5*time.Second, 20*time.Second, 30*time.Second, blackholeGates{uplinkStale: true})
	if reason != blackholeNone {
		t.Errorf("a receive verdict fired through a stale uplink: %s", reason)
	}
	if held != blackholeNoReceiveAck {
		t.Errorf("held = %q, want %q -- a gate that hides what it held cannot be measured", held, blackholeNoReceiveAck)
	}

	// the syn branch is a receive verdict too: nothing established, silence
	// past the connect bound
	synStats := &clientWindowStats{
		log:              DefaultLogger(),
		firstSendSynTime: time.Now().Add(-31 * time.Second),
		sendSynCount:     18,
	}
	reason, held = blackholeReasonFromStats(time.Now(), synStats, 5*time.Second, 20*time.Second, 30*time.Second, blackholeGates{uplinkStale: true})
	if reason != blackholeNone {
		t.Errorf("the syn verdict fired through a stale uplink: %s", reason)
	}
	if held != blackholeNoReceiveSyn {
		t.Errorf("held = %q, want %q", held, blackholeNoReceiveSyn)
	}
}

// The no-send-ack verdict is the unambiguous signal and must never be gated
// or rebased by uplink staleness: send acks ride the tunnel transport whose
// liveness the transport gate tracks separately, so a provider that stops
// acknowledging while that transport is up is convicted on its own signal.
func TestBlackholeUplinkGateNeverHoldsNoSendAck(t *testing.T) {
	stats := &clientWindowStats{
		log:               DefaultLogger(),
		firstSendNackTime: time.Now().Add(-10 * time.Second),
		sendAckCount:      0,
		receiveAckCount:   0,
	}

	// a stale uplink AND a rebase point younger than the nack clock: neither
	// may touch the send verdict
	gates := blackholeGates{
		uplinkStale:       true,
		receiveFreshSince: time.Now().Add(-1 * time.Second),
	}
	reason, held := blackholeReasonFromStats(time.Now(), stats, 5*time.Second, 20*time.Second, 30*time.Second, gates)
	if reason != blackholeNoSendAck {
		t.Errorf("the uplink gate touched the send verdict: reason = %q, want %q", reason, blackholeNoSendAck)
	}
	if held != blackholeNone {
		t.Errorf("the send verdict was reported held: %q", held)
	}
}

// When a gated epoch ends, the receive-branch clocks count from the epoch end
// rather than the original first-send time -- otherwise every verdict held
// across the silence matures at once on unfreeze and the executions merely
// arrive in a burst instead of a drip.
func TestBlackholeRebaseRestartsReceiveClocks(t *testing.T) {
	now := time.Now()
	stats := &clientWindowStats{
		log:               DefaultLogger(),
		firstSendNackTime: now.Add(-25 * time.Second),
		sendAckCount:      7,
		receiveAckCount:   0,
	}

	// ungated, the verdict is mature and fires
	reason, _ := blackholeReasonFromStats(now, stats, 5*time.Second, 20*time.Second, 30*time.Second, blackholeGates{})
	if reason != blackholeNoReceiveAck {
		t.Fatalf("baseline: reason = %q, want %q", reason, blackholeNoReceiveAck)
	}

	// a stale epoch ended 3s ago: the clock restarts there, so no verdict --
	// and nothing held either, because nothing would have fired
	reason, held := blackholeReasonFromStats(now, stats, 5*time.Second, 20*time.Second, 30*time.Second, blackholeGates{receiveFreshSince: now.Add(-3 * time.Second)})
	if reason != blackholeNone {
		t.Errorf("a rebased clock still fired: %s", reason)
	}
	if held != blackholeNone {
		t.Errorf("a verdict that was not firing was reported held: %q", held)
	}

	// once a fresh full receive window elapses after the epoch end, silence
	// convicts again -- the rebase is a restart, not immunity
	reason, _ = blackholeReasonFromStats(now, stats, 5*time.Second, 20*time.Second, 30*time.Second, blackholeGates{receiveFreshSince: now.Add(-21 * time.Second)})
	if reason != blackholeNoReceiveAck {
		t.Errorf("a fully re-aged clock did not fire: reason = %q, want %q", reason, blackholeNoReceiveAck)
	}

	// the syn clock rebases the same way
	synStats := &clientWindowStats{
		log:              DefaultLogger(),
		firstSendSynTime: now.Add(-40 * time.Second),
		sendSynCount:     5,
	}
	reason, _ = blackholeReasonFromStats(now, synStats, 5*time.Second, 20*time.Second, 30*time.Second, blackholeGates{receiveFreshSince: now.Add(-5 * time.Second)})
	if reason != blackholeNone {
		t.Errorf("a rebased syn clock still fired: %s", reason)
	}
}

// A channel whose transport set is empty cannot deliver or receive anything,
// so its silence proves nothing about the provider: every verdict is held,
// including the otherwise-ungated no-send-ack.
func TestBlackholeTransportDownHoldsAllVerdicts(t *testing.T) {
	now := time.Now()
	cases := []struct {
		stats *clientWindowStats
		want  blackholeReason
	}{
		// silent provider that would be convicted on the send bound
		{&clientWindowStats{
			log:               DefaultLogger(),
			firstSendNackTime: now.Add(-10 * time.Second),
		}, blackholeNoSendAck},
		// acking-then-quiet provider that would be convicted on the receive bound
		{&clientWindowStats{
			log:               DefaultLogger(),
			firstSendNackTime: now.Add(-21 * time.Second),
			sendAckCount:      7,
		}, blackholeNoReceiveAck},
		// nothing-established provider that would be convicted on the syn bound
		{&clientWindowStats{
			log:              DefaultLogger(),
			firstSendSynTime: now.Add(-31 * time.Second),
			sendSynCount:     3,
		}, blackholeNoReceiveSyn},
	}
	for _, c := range cases {
		reason, held := blackholeReasonFromStats(now, c.stats, 5*time.Second, 20*time.Second, 30*time.Second, blackholeGates{transportDown: true})
		if reason != blackholeNone {
			t.Errorf("a verdict fired with the transport down: %s", reason)
		}
		if held != c.want {
			t.Errorf("held = %q, want %q", held, c.want)
		}
	}

	// a window with nothing firing reports nothing held: held is a suppressed
	// verdict, not a gate-engaged flag
	healthy := &clientWindowStats{
		log:               DefaultLogger(),
		firstSendNackTime: now.Add(-60 * time.Second),
		sendAckCount:      100,
		receiveAckCount:   100,
	}
	reason, held := blackholeReasonFromStats(now, healthy, 5*time.Second, 20*time.Second, 30*time.Second, blackholeGates{transportDown: true})
	if reason != blackholeNone || held != blackholeNone {
		t.Errorf("a healthy window reported reason %q held %q with the transport down", reason, held)
	}
}

// uplinkGateTestParent builds a bare parent whose window carries
// sendingChannels channels that each hold one outstanding send -- the state
// the gate's degenerate-case guard counts.
func uplinkGateTestParent(sendingChannels int) *RemoteUserNatMultiClient {
	mc := &RemoteUserNatMultiClient{
		settings:      DefaultMultiClientSettings(),
		clientUpdates: map[*multiClientChannel]map[*multiClientChannelUpdate]bool{},
	}
	for range sendingChannels {
		client := stallTestChannel()
		client.addSend(1440, udpTestPath(4))
		mc.clientUpdates[client] = map[*multiClientChannelUpdate]bool{
			new(multiClientChannelUpdate): true,
		}
	}
	return mc
}

// The gate's lifecycle against one continuous silence: engage past the gate
// bound, disengage past the hard cap so a genuinely dead window can still be
// recycled, and rebase the verdict clocks only when receiving actually
// resumes.
func TestBlackholeUplinkGateStaleEpochAndCap(t *testing.T) {
	mc := uplinkGateTestParent(2)
	now := time.Now()

	// ingress within the gate: fresh, nothing to rebase
	mc.uplinkLastIngressNanos.Store(now.Add(-1 * time.Second).UnixNano())
	stale, freshSince := mc.uplinkGate(now)
	if stale {
		t.Fatal("a fresh uplink read as stale")
	}
	if !freshSince.IsZero() {
		t.Errorf("never-stale must rebase nothing: freshSince = %v", freshSince)
	}

	// silence past the gate opens a stale epoch
	mc.uplinkLastIngressNanos.Store(now.Add(-6 * time.Second).UnixNano())
	stale, freshSince = mc.uplinkGate(now)
	if !stale {
		t.Fatal("tunnel-wide silence past the gate did not read as stale")
	}
	if !freshSince.IsZero() {
		t.Errorf("no earlier epoch to rebase from: freshSince = %v", freshSince)
	}

	// past the hard cap the gate stops applying -- a window whose every
	// provider is dead is also tunnel-wide silence, and it must still recycle
	capped := now.Add(uplinkStalenessMaxHold + time.Second)
	stale, freshSince = mc.uplinkGate(capped)
	if stale {
		t.Error("the gate still held past the hard cap")
	}
	// the epoch stays open at the cap: advancing the rebase point here would
	// rebase away the very recycle the cap exists to allow
	if !freshSince.IsZero() {
		t.Errorf("the cap advanced the rebase point: freshSince = %v", freshSince)
	}

	// receiving resumes: the epoch closes and the rebase point records when
	resumed := capped.Add(2 * time.Second)
	mc.uplinkLastIngressNanos.Store(resumed.Add(-100 * time.Millisecond).UnixNano())
	stale, freshSince = mc.uplinkGate(resumed)
	if stale {
		t.Error("a resumed uplink read as stale")
	}
	if !freshSince.Equal(resumed) {
		t.Errorf("freshSince = %v, want the epoch end %v", freshSince, resumed)
	}
}

// With fewer than two channels talking, tunnel-wide silence is
// indistinguishable from that one provider being dead, so the gate carries
// zero exculpatory information and must not engage.
func TestBlackholeUplinkGateRequiresTwoSendingChannels(t *testing.T) {
	mc := uplinkGateTestParent(1)
	now := time.Now()
	mc.uplinkLastIngressNanos.Store(now.Add(-10 * time.Second).UnixNano())

	if stale, _ := mc.uplinkGate(now); stale {
		t.Error("the gate engaged with a single talking channel")
	}

	// a channel with nothing outstanding is not talking and must not count
	idle := stallTestChannel()
	mc.clientUpdates[idle] = map[*multiClientChannelUpdate]bool{
		new(multiClientChannelUpdate): true,
	}
	if stale, _ := mc.uplinkGate(now); stale {
		t.Error("an idle channel counted toward the degenerate guard")
	}

	// a second channel with outstanding sends makes the silence meaningful
	talking := stallTestChannel()
	talking.addSend(1440, udpTestPath(4))
	mc.clientUpdates[talking] = map[*multiClientChannelUpdate]bool{
		new(multiClientChannelUpdate): true,
	}
	if stale, _ := mc.uplinkGate(now); !stale {
		t.Error("the gate did not engage with two talking channels silent")
	}
}

// 0 disables the gate entirely, and a client that has never received anything
// has no baseline to have gone stale from -- a dead-on-arrival window is left
// to the ordinary verdicts rather than held for the cap first.
func TestBlackholeUplinkGateOffAndNoBaseline(t *testing.T) {
	mc := uplinkGateTestParent(2)
	mc.settings.UplinkStalenessGate = 0
	now := time.Now()
	mc.uplinkLastIngressNanos.Store(now.Add(-10 * time.Minute).UnixNano())

	stale, freshSince := mc.uplinkGate(now)
	if stale {
		t.Error("the gate engaged while disabled")
	}
	if !freshSince.IsZero() {
		t.Errorf("a disabled gate must rebase nothing: freshSince = %v", freshSince)
	}

	// no stamp ever
	fresh := uplinkGateTestParent(2)
	if stale, _ := fresh.uplinkGate(now); stale {
		t.Error("the gate engaged with no ingress baseline")
	}
}

// The stamp is on the download hot path, so it is coarsened: a fresh stamp is
// left alone and only a stale one is rewritten.
func TestBlackholeUplinkStampCoarsens(t *testing.T) {
	mc := &RemoteUserNatMultiClient{}

	mc.stampUplinkIngress()
	first := mc.uplinkLastIngressNanos.Load()
	if first == 0 {
		t.Fatal("the first stamp did not store")
	}

	// immediately again: within the coarseness window, skipped
	mc.stampUplinkIngress()
	if got := mc.uplinkLastIngressNanos.Load(); got != first {
		t.Errorf("a fresh stamp was rewritten: %d -> %d", first, got)
	}

	// an aged stamp is refreshed
	aged := time.Now().Add(-1 * time.Second).UnixNano()
	mc.uplinkLastIngressNanos.Store(aged)
	mc.stampUplinkIngress()
	if got := mc.uplinkLastIngressNanos.Load(); got <= aged {
		t.Errorf("a stale stamp was not refreshed: %d", got)
	}
}

// The default gate must exist and sit well under the receive bound it
// protects, and it must survive the round trip through the override type -- a
// missed field zeroes (gate off) on every settings write.
func TestBlackholeUplinkGateDefault(t *testing.T) {
	settings := DefaultMultiClientSettings()

	if settings.UplinkStalenessGate != 5*time.Second {
		t.Errorf("UplinkStalenessGate = %v, want 5s", settings.UplinkStalenessGate)
	}
	if settings.BlackholeReceiveTimeout <= settings.UplinkStalenessGate {
		t.Errorf(
			"gate %v must engage before the receive bound %v can mature on silent evidence",
			settings.UplinkStalenessGate, settings.BlackholeReceiveTimeout,
		)
	}
	if ReliabilitySettingsFrom(settings).UplinkStalenessGate != settings.UplinkStalenessGate {
		t.Error("UplinkStalenessGate is dropped by ReliabilitySettingsFrom, so every override write turns the gate off")
	}
}

// The stamp is only worth anything if the ingress sites actually call it --
// the correct-but-uncalled helper is the failure mode this codebase has
// shipped more than once. Both sites are pinned: the provider-originated
// receive path, and the intercepted dial failure (deliberately not a
// receive-ack, but proof the uplink delivers).
func TestBlackholeUplinkStampSites(t *testing.T) {
	source, err := readSource("ip_remote_multi_client.go")
	if err != nil {
		t.Fatal(err)
	}

	for _, site := range []struct{ fn, desc string }{
		{"func (self *RemoteUserNatMultiClient) clientReceivePacket(", "provider-originated ingress"},
		{"func (self *RemoteUserNatMultiClient) clientDialFailure(", "intercepted dial failure"},
	} {
		body, ok := functionBody(source, site.fn)
		if !ok {
			t.Fatalf("could not find %s", site.fn)
		}
		if !strings.Contains(body, "stampUplinkIngress(") {
			t.Errorf("%s does not stamp the uplink: the gate would go stale under working ingress", site.desc)
		}
	}
}

// detectBlackhole must actually consult both gates and count what they hold.
// The decision function carrying gate parameters is worthless if the caller
// passes zero values forever.
func TestBlackholeDetectConsultsGates(t *testing.T) {
	source, err := readSource("ip_remote_multi_client.go")
	if err != nil {
		t.Fatal(err)
	}

	body, ok := functionBody(source, "func (self *multiClientChannel) detectBlackhole()")
	if !ok {
		t.Fatal("could not find detectBlackhole")
	}

	if !strings.Contains(body, "self.uplinkGate(") {
		t.Error("detectBlackhole does not read the tunnel-wide uplink gate")
	}
	if !strings.Contains(body, "hasActiveTransport(") {
		t.Error("detectBlackhole does not cross-check the channel's transport liveness")
	}
	if !strings.Contains(body, "blackholeGates{") {
		t.Error("detectBlackhole does not pass the gates into the decision")
	}
	if !strings.Contains(body, "verdictHeldUplinkStale(") || !strings.Contains(body, "verdictHeldTransportDown(") {
		t.Error("detectBlackhole does not count held verdicts, so the gates cannot be measured")
	}
}

func readSource(name string) (string, error) {
	b, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// functionBody returns the source between a function's signature and the
// closing brace at column 0. Good enough for asserting a call site exists,
// which is all it is used for.
func functionBody(source string, signature string) (string, bool) {
	start := strings.Index(source, signature)
	if start < 0 {
		return "", false
	}
	rest := source[start:]
	if end := strings.Index(rest, "\n}\n"); 0 <= end {
		return rest[:end], true
	}
	return rest, true
}
