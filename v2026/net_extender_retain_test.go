package connect

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

// What a path change and a resume keep (DESIGNNOTES4.md §6).
//
// A path change, a link or quality change the host reports as one, and a
// resume from a long sleep keep every piece of extender evidence -- holds,
// limits, failure counts and latency samples -- since an extender that
// answered, failed or was fast before most likely still is, and learning it
// all again costs dials and time. The samples are due a refresh: they keep
// ranking the candidates until the one probe pass that follows the first
// sample after the change measures them again. Before, a path change and a
// resume dropped the samples, so until that pass the candidate order ranked
// the extenders as never measured. These tests run the real probe loop on the
// host clock model and fire its checks through its wait seam, so nothing
// sleeps; an armed check is the barrier that the loop has finished.

// A held address (five failures, a hold of 160 min) and a limited one (a
// Retry-After of six hours, three to six hours) added to the directory of
// newTestResumeProbeClient, with what the directory says of each.
func addTestHeldAndLimitedExtenders(
	t *testing.T,
	directory *ExtenderDirectory,
) (heldIp netip.Addr, limitedIp netip.Addr, heldEntry *ExtenderDirectoryEntry, limitedUntil time.Time) {
	t.Helper()
	heldIp = netip.MustParseAddr("192.0.2.12")
	limitedIp = netip.MustParseAddr("192.0.2.13")
	directory.AddManual(heldIp)
	directory.AddManual(limitedIp)
	for range 5 {
		directory.RecordFailure(heldIp, ExtenderConnectModeTcpTls)
	}
	limitedUntil = directory.RecordLimited(limitedIp, 6*time.Hour)
	if limitedUntil.IsZero() {
		t.Fatal("the 429 limited nothing")
	}
	return heldIp, limitedIp, testDirectoryEntry(t, directory, heldIp), limitedUntil
}

// Fails unless the held address is still held with its failures, and the
// limited one still limited until the same time.
func assertTestHeldAndLimitedExtenders(
	t *testing.T,
	what string,
	directory *ExtenderDirectory,
	heldIp netip.Addr,
	limitedIp netip.Addr,
	heldEntry *ExtenderDirectoryEntry,
	limitedUntil time.Time,
) {
	t.Helper()
	if directory.AddressUsable(heldIp) {
		t.Fatalf("%s: the hold was released", what)
	}
	if state := testDirectoryState(t, directory, heldIp); state != ExtenderStateHold {
		t.Fatalf("%s: state = %s, expected the hold to stay", what, state)
	}
	if entry := testDirectoryEntry(t, directory, heldIp); entry.FailureCount != heldEntry.FailureCount ||
		!entry.LastFailureTime.Equal(heldEntry.LastFailureTime) {
		t.Fatalf("%s: failures = %d at %s, expected %d at %s", what, entry.FailureCount, entry.LastFailureTime, heldEntry.FailureCount, heldEntry.LastFailureTime)
	}
	if until := directory.AddressLimitedUntil(limitedIp); !until.Equal(limitedUntil) {
		t.Fatalf("%s: limited until %s, expected the limit to stay at %s", what, until, limitedUntil)
	}
}

// A path change keeps every hold, limit, failure count and sample, and the
// candidate order ranks by them at once. Only the probe pass that follows the
// first sample on the new path measures again, once: the change itself wakes
// no probe, so nothing probes a path that has not worked yet, and a later
// sample, the feed reconnecting, finds nothing due.
func TestExtenderNetworkClientMeasuresOnceAfterAPathChange(t *testing.T) {
	hostClock := newTestHostClock(t)
	probes := newTestSampleClockProbeLog()
	networkClient, armedChecks := newTestResumeProbeClient(t, hostClock, probes)
	directory := networkClient.directory

	check := nextTestResumeCheck(t, armedChecks, "the first pass")
	if count := probes.count(); count != 2 {
		t.Fatalf("probes = %d, expected the first pass to measure both extenders", count)
	}
	heldIp, limitedIp, heldEntry, limitedUntil := addTestHeldAndLimitedExtenders(t, directory)
	refreshWake := networkClient.wakeMonitor.NotifyChannel()
	probeWake := networkClient.probeWake.NotifyChannel()

	// the new path reverses the two
	probes.setRtts(map[string]time.Duration{
		"192.0.2.10": 40 * time.Millisecond,
		"192.0.2.11": 15 * time.Millisecond,
	})
	networkClient.networkChanged()
	candidates := directory.Candidates(4, 8)
	assertIpOrder(t, candidates, "192.0.2.10", "192.0.2.11", "192.0.2.13")
	assertTestCandidateSamples(
		t,
		"after the path change",
		candidates,
		map[string]time.Duration{"192.0.2.10": 20 * time.Millisecond, "192.0.2.11": 25 * time.Millisecond},
		map[string]bool{"192.0.2.10": true, "192.0.2.11": true},
	)
	assertTestHeldAndLimitedExtenders(t, "after the path change", directory, heldIp, limitedIp, heldEntry, limitedUntil)
	if !testMonitorFired(refreshWake) {
		t.Fatal("the path change did not wake the refresh loop for a sample")
	}
	if testMonitorFired(probeWake) {
		t.Fatal("the path change woke the probe pass before a sample on the new path")
	}
	hostClock.awake(time.Minute)
	check = fireTestResumeCheck(t, check, armedChecks)
	if count := probes.count(); count != 2 {
		t.Fatalf("probes = %d, expected none before a sample on the new path", count)
	}

	completeTestResumeSample(networkClient)
	nextTestResumeCheck(t, armedChecks, "the pass after the sample")
	if count := probes.count(); count != 4 {
		t.Fatalf("probes = %d, expected the pass after the sample to measure both again", count)
	}
	candidates = directory.Candidates(4, 8)
	assertIpOrder(t, candidates, "192.0.2.11", "192.0.2.10", "192.0.2.13")
	assertTestCandidateSamples(
		t,
		"after the pass",
		candidates,
		map[string]time.Duration{"192.0.2.10": 40 * time.Millisecond, "192.0.2.11": 15 * time.Millisecond},
		map[string]bool{},
	)
	assertTestHeldAndLimitedExtenders(t, "after the pass", directory, heldIp, limitedIp, heldEntry, limitedUntil)

	completeTestResumeSample(networkClient)
	nextTestResumeCheck(t, armedChecks, "the pass after another sample")
	if count := probes.count(); count != 4 {
		t.Fatalf("probes = %d, expected one pass to measure again after the path change", count)
	}
}

// A resume from an hour's sleep keeps every hold, limit, failure count and
// sample, and the candidate order ranks by them at once; the hold and the
// limit outlast the hour by either clock. The pass that follows the sample
// the resume asks for replaces the samples.
func TestExtenderNetworkClientKeepsItsEvidenceAcrossALongResume(t *testing.T) {
	hostClock := newTestHostClock(t)
	probes := newTestSampleClockProbeLog()
	networkClient, armedChecks := newTestResumeProbeClient(t, hostClock, probes)
	directory := networkClient.directory

	check := nextTestResumeCheck(t, armedChecks, "the first pass")
	heldIp, limitedIp, heldEntry, limitedUntil := addTestHeldAndLimitedExtenders(t, directory)

	hostClock.sleep(time.Hour)
	check = fireTestResumeCheck(t, check, armedChecks)
	hostClock.awake(time.Minute)
	refreshWake := networkClient.wakeMonitor.NotifyChannel()
	// the check that tells the resume
	fireTestResumeCheck(t, check, armedChecks)
	if !testMonitorFired(refreshWake) {
		t.Fatal("the resume did not wake the refresh loop for a sample")
	}
	candidates := directory.Candidates(4, 8)
	assertIpOrder(t, candidates, "192.0.2.10", "192.0.2.11", "192.0.2.13")
	assertTestCandidateSamples(
		t,
		"after the resume",
		candidates,
		map[string]time.Duration{"192.0.2.10": 20 * time.Millisecond, "192.0.2.11": 25 * time.Millisecond},
		map[string]bool{"192.0.2.10": true, "192.0.2.11": true},
	)
	assertTestHeldAndLimitedExtenders(t, "after the resume", directory, heldIp, limitedIp, heldEntry, limitedUntil)

	probes.setRtts(map[string]time.Duration{
		"192.0.2.10": 40 * time.Millisecond,
		"192.0.2.11": 15 * time.Millisecond,
	})
	completeTestResumeSample(networkClient)
	nextTestResumeCheck(t, armedChecks, "the pass after the sample")
	if count := probes.count(); count != 4 {
		t.Fatalf("probes = %d, expected the pass after the sample to measure both again", count)
	}
	candidates = directory.Candidates(4, 8)
	assertIpOrder(t, candidates, "192.0.2.11", "192.0.2.10", "192.0.2.13")
	assertTestCandidateSamples(
		t,
		"after the pass",
		candidates,
		map[string]time.Duration{"192.0.2.10": 40 * time.Millisecond, "192.0.2.11": 15 * time.Millisecond},
		map[string]bool{},
	)
	assertTestHeldAndLimitedExtenders(t, "after the pass", directory, heldIp, limitedIp, heldEntry, limitedUntil)
}

// The strategy draws its extender dialers in the directory's candidate order,
// so after a path change it spends its expand budget on the extender the kept
// samples rank first; before, the change dropped them, and the budget went
// where an unmeasured directory put it. The strategy's own path change closes
// only the idle connections: its dialers stay, with their outcomes.
func TestClientStrategyExpandsByTheSamplesKeptAcrossAPathChange(t *testing.T) {
	restoreSpoof := setSpoofDomainsForTest([]string{"spoof.example"})
	t.Cleanup(restoreSpoof)
	restoreProbe := swapControlFamilyProbe(func(family int) bool { return true })
	t.Cleanup(restoreProbe)
	hostClock := newTestHostClock(t)
	directory := newTestHostClockDirectory(t, hostClock, "192.0.2.1", "192.0.2.2", "192.0.2.3")
	settings := DefaultClientStrategySettings()
	settings.ExtenderDirectory = directory
	settings.ExpandExtenderProfileCount = 1
	ctx, cancel := context.WithCancel(context.Background())
	clientStrategy := NewClientStrategy(ctx, settings)
	t.Cleanup(func() {
		clientStrategy.Close()
		cancel()
	})
	// the network client of the same directory, with no loops running: the
	// path change is called on it and on the strategy, as the host's
	// broadcast does
	networkClientSettings := DefaultExtenderNetworkClientSettings()
	networkClientSettings.Now = hostClock.Now
	networkClient := &ExtenderNetworkClient{
		ctx:           t.Context(),
		log:           NewNoopLogger(),
		directory:     directory,
		settings:      networkClientSettings,
		statusMonitor: NewMonitorValue[ExtenderNetworkClientStatus](ExtenderNetworkClientStatus{}),
		wakeMonitor:   NewMonitor(),
		probeWake:     NewMonitor(),
		hintWake:      NewMonitor(),
	}
	pathChange := func() {
		clientStrategy.networkChanged()
		networkClient.networkChanged()
	}

	directory.RecordLatency(netip.MustParseAddr("192.0.2.3"), 10*time.Millisecond, false)
	pathChange()
	expandedDialers := clientStrategy.expandExtenderDialers()
	if len(expandedDialers) != 1 {
		t.Fatalf("dialers = %d, expected the budget of one", len(expandedDialers))
	}
	dialer := expandedDialers[0]
	if ip := dialer.extenderConfig.Ip; ip != netip.MustParseAddr("192.0.2.3") {
		t.Fatalf("the strategy expanded %s, expected 192.0.2.3 by the sample kept across the path change", ip)
	}

	// a success, then another path change: the dialer and its outcome stay
	dialer.Update(context.Background(), nil)
	weight := dialer.Weight()
	pathChange()
	kept := func() bool {
		clientStrategy.mutex.Lock()
		defer clientStrategy.mutex.Unlock()
		return clientStrategy.dialers[dialer]
	}()
	if !kept {
		t.Fatal("the path change dropped the extender dialer")
	}
	if dialer.Weight() != weight || !dialer.IsLastSuccess() {
		t.Fatalf("weight = %f, last success = %t after the path change, expected %f and true", dialer.Weight(), dialer.IsLastSuccess(), weight)
	}
	if entry := testDirectoryEntry(t, directory, netip.MustParseAddr("192.0.2.3")); entry.SuccessCount != 1 || entry.Latency != 10*time.Millisecond {
		t.Fatalf("success count = %d, latency = %s, expected the success and the sample kept", entry.SuccessCount, entry.Latency)
	}
}

// The pass after a path change measures the samples due a refresh before an
// address never measured, so it spends its window on the samples the
// candidate order ranks by: otherwise an unmeasured address, here first by
// its address, can fill the window first and leave a sample of the old path
// ranking until it ages out. The window of two fills with the two samples
// measured again, and the address learned since waits, as it would have on
// the old path.
func TestExtenderNetworkClientMeasuresTheSamplesDueARefreshFirst(t *testing.T) {
	clock := newTestClock()
	probes := newTestSampleClockProbeLog()
	networkClient := newTestSampleClockProbeClient(t, clock.Now, clock.advance, probes)
	directory := networkClient.directory

	networkClient.probePass()
	if count := probes.count(); count != 2 {
		t.Fatalf("probes = %d, expected the window of two to fill", count)
	}
	// an address learned once the window was full
	directory.AddManual(netip.MustParseAddr("192.0.2.1"))

	probes.setRtts(map[string]time.Duration{
		"192.0.2.1":  30 * time.Millisecond,
		"192.0.2.10": 21 * time.Millisecond,
		"192.0.2.11": 26 * time.Millisecond,
	})
	networkClient.networkChanged()
	networkClient.probePass()
	if count := probes.count(); count != 4 {
		t.Fatalf("probes = %d, expected the pass to measure the two samples again and stop", count)
	}
	candidates := directory.Candidates(4, 8)
	assertIpOrder(t, candidates, "192.0.2.10", "192.0.2.11", "192.0.2.1")
	assertTestCandidateSamples(
		t,
		"after the pass",
		candidates,
		map[string]time.Duration{"192.0.2.10": 21 * time.Millisecond, "192.0.2.11": 26 * time.Millisecond},
		map[string]bool{},
	)
}
