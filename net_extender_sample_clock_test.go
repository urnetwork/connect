package connect

import (
	"context"
	"net/netip"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// The clock the extender directory judges its local evidence by, and what a
// path change does to the latency samples (DESIGNNOTES4.md §6).
//
// A latency sample aged on the monotonic clock alone, which stops while the
// host sleeps, so after a night's sleep a sample still ranked as fresh and
// filled the probe pass's window. A hold and a limit lapsed on the same clock.
// A path change keeps every sample in use but due a refresh, so the candidate
// order still ranks by it and the probe pass measures it again. These tests
// run the directory on a model of the host's clock that carries a monotonic
// reading, as time.Now's readings do, since a fake clock of wall readings
// alone already compares by the wall clock and cannot show any of it.

// A host's clock as time.Now reads it in a running process: each reading
// carries a monotonic reading beside its wall one, and Go compares and
// subtracts two such readings by their monotonic parts alone. `awake` moves
// both; `sleep` moves the wall reading alone, which is what a host that
// sleeps does to a Go process (mach_absolute_time on darwin and
// CLOCK_MONOTONIC on linux and android stop while it sleeps); `setBack` moves
// the wall reading alone the other way, as a clock correction does.
//
// The readings are made from one real reading, the only source of a monotonic
// part. time.Time offers no way to set one, so a reading's unexported
// monotonic field is set through reflect, and the constructor checks that
// what it makes compares the way it must before any test relies on it.
type testHostClock struct {
	stateLock sync.Mutex
	// a real reading
	base time.Time
	// how far each part of a reading has moved from base
	monotonicOffset time.Duration
	wallOffset      time.Duration
}

// A host clock from the real reading taken now, checked to compare by its
// monotonic part the way time.Now's readings do.
func newTestHostClock(t *testing.T) *testHostClock {
	t.Helper()
	base := time.Now()
	if base == base.Round(0) {
		t.Fatal("time.Now carries no monotonic reading here")
	}
	self := &testHostClock{
		base: base,
	}
	// a reading after an hour of sleep must compare by its monotonic part,
	// which has not moved, and keep its wall part, which has
	slept, ok := self.reading(0, time.Hour)
	if !ok || slept.Sub(base) != 0 || slept.Round(0).Sub(base.Round(0)) != time.Hour {
		t.Fatal("this Go version's time.Time cannot carry a monotonic reading apart from its wall one")
	}
	return self
}

// The reading whose monotonic and wall parts are the given offsets past base,
// and whether it could be made.
func (self *testHostClock) reading(monotonicOffset time.Duration, wallOffset time.Duration) (time.Time, bool) {
	reading := self.base.Add(wallOffset)
	if reading == reading.Round(0) {
		return time.Time{}, false
	}
	// the monotonic part (time.Time's ext while a reading carries one), which
	// Add moved with the wall part
	monotonic := reflect.ValueOf(&reading).Elem().FieldByName("ext")
	if !monotonic.IsValid() || monotonic.Kind() != reflect.Int64 {
		return time.Time{}, false
	}
	*(*int64)(unsafe.Pointer(monotonic.UnsafeAddr())) += int64(monotonicOffset - wallOffset)
	return reading, true
}

// The reading the host's time.Now would return at this point.
func (self *testHostClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	reading, _ := self.reading(self.monotonicOffset, self.wallOffset)
	return reading
}

// The host runs for `d`: both parts move.
func (self *testHostClock) awake(d time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.monotonicOffset += d
	self.wallOffset += d
}

// The host sleeps for `d`: the wall part moves and the monotonic part stays.
func (self *testHostClock) sleep(d time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.wallOffset += d
}

// The wall clock is set back by `d`, and the monotonic part stays.
func (self *testHostClock) setBack(d time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.wallOffset -= d
}

// A directory on the host clock with one verified record per ip, each issued
// now by the host clock and living two weeks of its wall time.
func newTestHostClockDirectory(t *testing.T, hostClock *testHostClock, ips ...string) *ExtenderDirectory {
	t.Helper()
	directory, rootPrivateKey := newTestExtenderDirectory(t, newTestClock(), func(settings *ExtenderDirectorySettings) {
		settings.Now = hostClock.Now
	})
	for _, ip := range ips {
		record := signTestRecord(
			t,
			rootPrivateKey,
			newTestExtenderKey(t),
			hostClock.Now(),
			hostClock.Now().Add(14*24*time.Hour),
			testExtenderAddress(ip),
		)
		if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

// The bug: a sample aged on the monotonic clock alone, so a sample taken before
// a night's sleep still ranked first after it, and still counted in the probe
// pass's window, until LatencyMaxAge of awake time had passed. The time the
// host slept counts now.
func TestExtenderDirectoryLatencySampleAgesWhileTheHostSleeps(t *testing.T) {
	hostClock := newTestHostClock(t)
	directory := newTestHostClockDirectory(t, hostClock, "192.0.2.1", "192.0.2.2", "192.0.2.3")
	latencyMaxAge := directory.settings.LatencyMaxAge

	directory.RecordLatency(netip.MustParseAddr("192.0.2.3"), 20*time.Millisecond, true)
	directory.RecordLatency(netip.MustParseAddr("192.0.2.1"), 80*time.Millisecond, false)
	assertIpOrder(t, directory.Candidates(4, 8), "192.0.2.3", "192.0.2.1", "192.0.2.2")

	// an hour awake and all but two hours of the max age asleep: current
	hostClock.awake(time.Hour)
	hostClock.sleep(latencyMaxAge - 2*time.Hour)
	assertIpOrder(t, directory.Candidates(4, 8), "192.0.2.3", "192.0.2.1", "192.0.2.2")
	if latencies := directory.MeasuredLatencies(4, false); len(latencies) != 2 {
		t.Fatalf("latencies = %v within the max age, expected both", latencies)
	}

	// the hour awake that completes the max age ages both out, though the
	// monotonic clock has run two hours
	hostClock.awake(time.Hour)
	assertIpOrder(t, directory.Candidates(4, 8), "192.0.2.1", "192.0.2.2", "192.0.2.3")
	for _, candidate := range directory.Candidates(4, 8) {
		if candidate.Latency != 0 || candidate.LatencyAttested {
			t.Fatalf("%s carries a sample from before the sleep: %s attested=%t", candidate.Ip, candidate.Latency, candidate.LatencyAttested)
		}
	}
	if latencies := directory.MeasuredLatencies(4, false); len(latencies) != 0 {
		t.Fatalf("latencies = %v after the sleep, expected none", latencies)
	}
	// so the probe pass measures every address, in the plain order
	assertIpOrder(t, directory.ProbeCandidates(4, 8, false), "192.0.2.1", "192.0.2.2", "192.0.2.3")
	if entry := testDirectoryEntry(t, directory, netip.MustParseAddr("192.0.2.3")); entry.Latency != 0 {
		t.Fatalf("the status shows a sample from before the sleep: %s", entry.Latency)
	}

	// a sample taken after the sleep is current
	directory.RecordLatency(netip.MustParseAddr("192.0.2.2"), 5*time.Millisecond, false)
	assertIpOrder(t, directory.Candidates(4, 8), "192.0.2.2", "192.0.2.1", "192.0.2.3")
}

// A hold lapses with the time the host slept too. Before, a run of failures
// just before a night's sleep kept the address out of every candidate list,
// the usable count the startup gate reads and the active tier for the rest of
// its hold in awake time, up to MaxHoldTimeout after waking.
func TestExtenderDirectoryHoldLapsesWhileTheHostSleeps(t *testing.T) {
	hostClock := newTestHostClock(t)
	directory := newTestHostClockDirectory(t, hostClock, "192.0.2.1")
	ip := netip.MustParseAddr("192.0.2.1")

	// seven failures hold it for the max, six hours: ten minutes doubled six
	// times is past it
	for range 7 {
		directory.RecordFailure(ip, ExtenderConnectModeTcpTls)
	}
	if holdTimeout := directory.holdTimeout(7); holdTimeout != directory.settings.MaxHoldTimeout {
		t.Fatalf("hold = %s after seven failures, expected the max", holdTimeout)
	}
	assertHeld := func(what string, held bool) {
		t.Helper()
		if usable := directory.AddressUsable(ip); usable == held {
			t.Fatalf("%s: usable = %t", what, usable)
		}
		if count := directory.UsableCount(4); (count == 0) != held {
			t.Fatalf("%s: usable count = %d", what, count)
		}
		if count := len(directory.Candidates(4, 8)); (count == 0) != held {
			t.Fatalf("%s: candidates = %d", what, count)
		}
		if count := directory.ActiveRecordCount(); (count == 0) != held {
			t.Fatalf("%s: active records = %d", what, count)
		}
		if state := testDirectoryState(t, directory, ip); (state == ExtenderStateHold) != held {
			t.Fatalf("%s: state = %s", what, state)
		}
	}
	assertHeld("at the failure", true)

	hostClock.awake(time.Hour)
	assertHeld("an hour into the hold", true)

	// a night's sleep, past the hold by the wall clock and five hours short of
	// it by the monotonic one
	hostClock.sleep(6 * time.Hour)
	assertHeld("after the sleep", false)
	if state := testDirectoryState(t, directory, ip); state != ExtenderStateWarning {
		t.Fatalf("state = %s after the hold, expected the failures to leave a warning", state)
	}
}

// A limit lapses with the time the host slept too, and a 429 after the sleep
// limits the address afresh. Before, a limit set before the sleep kept the
// address last and out of the probe pass after it; and with only the lapse
// fixed, the old limit, which the monotonic clock alone still places after the
// new one, would have kept the new one from being recorded.
func TestExtenderDirectoryLimitLapsesWhileTheHostSleeps(t *testing.T) {
	hostClock := newTestHostClock(t)
	directory := newTestHostClockDirectory(t, hostClock, "192.0.2.1", "192.0.2.2")
	limitedIp := netip.MustParseAddr("192.0.2.1")

	// a Retry-After of six hours limits it for three to six
	limitedUntil := directory.RecordLimited(limitedIp, 6*time.Hour)
	if limitedUntil.IsZero() || !limitedUntil.Equal(directory.AddressLimitedUntil(limitedIp)) {
		t.Fatalf("limited until %s, the directory says %s", limitedUntil, directory.AddressLimitedUntil(limitedIp))
	}
	assertIpOrder(t, directory.Candidates(4, 8), "192.0.2.2", "192.0.2.1")
	assertIpOrder(t, directory.ProbeCandidates(4, 8, false), "192.0.2.2")

	hostClock.awake(time.Minute)
	hostClock.sleep(7 * time.Hour)
	if until := directory.AddressLimitedUntil(limitedIp); !until.IsZero() {
		t.Fatalf("still limited until %s after the sleep", until)
	}
	candidates := directory.Candidates(4, 8)
	assertIpOrder(t, candidates, "192.0.2.1", "192.0.2.2")
	if !candidates[0].LimitedUntil.IsZero() {
		t.Fatalf("the candidate says limited until %s after the sleep", candidates[0].LimitedUntil)
	}
	assertIpOrder(t, directory.ProbeCandidates(4, 8, false), "192.0.2.1", "192.0.2.2")

	// a 429 after the sleep: limited for five to fifteen seconds from now
	renewedUntil := directory.RecordLimited(limitedIp, 10*time.Second)
	until := directory.AddressLimitedUntil(limitedIp)
	if until.IsZero() || !until.Equal(renewedUntil) {
		t.Fatalf("a 429 after the sleep left the address limited until %s, recorded %s", until, renewedUntil)
	}
	if now := hostClock.Now(); until.Before(now.Add(5*time.Second)) || now.Add(15*time.Second).Before(until) {
		t.Fatalf("limited until %s, expected 5 s to 15 s from %s", until, now)
	}
	assertIpOrder(t, directory.Candidates(4, 8), "192.0.2.2", "192.0.2.1")
}

// A wall clock set back extends nothing: the sample still ages out, and the
// hold and the limit still lapse, on the monotonic clock's time. Judging by the
// wall clock alone would keep each of them a day longer here.
func TestExtenderDirectoryClockSetBackExtendsNothing(t *testing.T) {
	hostClock := newTestHostClock(t)
	directory := newTestHostClockDirectory(t, hostClock, "192.0.2.1", "192.0.2.2", "192.0.2.3")
	sampleIp := netip.MustParseAddr("192.0.2.1")
	heldIp := netip.MustParseAddr("192.0.2.2")
	limitedIp := netip.MustParseAddr("192.0.2.3")

	directory.RecordLatency(sampleIp, 20*time.Millisecond, false)
	// a ten minute hold, and a limit of five to fifteen minutes
	directory.RecordFailure(heldIp, ExtenderConnectModeTcpTls)
	directory.RecordLimited(limitedIp, 10*time.Minute)

	hostClock.setBack(24 * time.Hour)
	if latencies := directory.MeasuredLatencies(4, false); len(latencies) != 1 {
		t.Fatalf("latencies = %v once the clock was set back, expected the sample", latencies)
	}
	if directory.AddressUsable(heldIp) {
		t.Fatal("the clock set back lifted the hold")
	}
	if directory.AddressLimitedUntil(limitedIp).IsZero() {
		t.Fatal("the clock set back lifted the limit")
	}

	hostClock.awake(15 * time.Minute)
	if !directory.AddressUsable(heldIp) {
		t.Fatal("the clock set back extended the hold")
	}
	if until := directory.AddressLimitedUntil(limitedIp); !until.IsZero() {
		t.Fatalf("the clock set back extended the limit to %s", until)
	}
	hostClock.awake(directory.settings.LatencyMaxAge)
	if latencies := directory.MeasuredLatencies(4, false); len(latencies) != 0 {
		t.Fatalf("latencies = %v, the clock set back kept the sample past the max age", latencies)
	}
}

// The operator's last country ages by the wall clock (CountryHintMaxAge), as
// fix/persist-hint-country made it: past the max age after a sleep it is not
// used, though the monotonic clock has run an hour.
func TestExtenderDirectoryCountryAgesWhileTheHostSleeps(t *testing.T) {
	setTestNetworkCountryCode(t, "")
	hostClock := newTestHostClock(t)
	directory := newTestHostClockDirectory(t, hostClock)

	directory.SetCountryHint("de")
	directory.ExpireCountryHint()
	if countryCode := directory.SpoofCountryCode(); countryCode != "de" {
		t.Fatalf("spoof country = %q, expected the last country while it is young", countryCode)
	}
	hostClock.awake(time.Hour)
	hostClock.sleep(directory.settings.CountryHintMaxAge)
	if countryCode := directory.SpoofCountryCode(); countryCode != "" {
		t.Fatalf("spoof country = %q past the max age, expected none", countryCode)
	}
}

// The samples each candidate carries, by ip, and which of them are due a
// refresh.
func testCandidateSamples(candidates []*ExtenderCandidate) (map[string]time.Duration, map[string]bool) {
	ipLatencies := map[string]time.Duration{}
	ipRefreshDues := map[string]bool{}
	for _, candidate := range candidates {
		ipLatencies[candidate.Ip.String()] = candidate.Latency
		ipRefreshDues[candidate.Ip.String()] = candidate.LatencyRefreshDue
	}
	return ipLatencies, ipRefreshDues
}

// Fails unless the candidates carry exactly the expected samples, each due a
// refresh as expected, and none for any other candidate.
func assertTestCandidateSamples(
	t *testing.T,
	what string,
	candidates []*ExtenderCandidate,
	expectedIpLatencies map[string]time.Duration,
	expectedIpRefreshDues map[string]bool,
) {
	t.Helper()
	ipLatencies, ipRefreshDues := testCandidateSamples(candidates)
	for _, candidate := range candidates {
		ip := candidate.Ip.String()
		if ipLatencies[ip] != expectedIpLatencies[ip] || ipRefreshDues[ip] != expectedIpRefreshDues[ip] {
			t.Fatalf(
				"%s: samples = %v, due a refresh = %v, expected %v due %v",
				what,
				ipLatencies,
				ipRefreshDues,
				expectedIpLatencies,
				expectedIpRefreshDues,
			)
		}
	}
}

// The bug: a path change dropped every sample, so the candidate order fell
// back to the last success and the probe pass had to measure every extender
// again before any ranked by rtt. A path change keeps every sample in use, due
// a refresh, and the rest of the local evidence with it: the order still ranks
// by the samples, the status shows them and nothing is published. The probe
// pass counts none of them toward its window and measures them first, lowest
// first, and a sample taken on the new path replaces one.
func TestExtenderDirectoryRefreshLatenciesKeepsEverySample(t *testing.T) {
	directory, _ := newTestProximityDirectory(t)
	heldIp := netip.MustParseAddr("192.0.2.4")
	limitedIp := netip.MustParseAddr("192.0.2.5")
	directory.AddManual(heldIp)
	directory.AddManual(limitedIp)
	directory.RecordFailure(heldIp, ExtenderConnectModeTcpTls)
	directory.RecordFailure(heldIp, ExtenderConnectModeTcpTls)
	directory.RecordLatency(netip.MustParseAddr("192.0.2.3"), 20*time.Millisecond, true)
	directory.RecordLatency(netip.MustParseAddr("192.0.2.1"), 80*time.Millisecond, false)
	directory.RecordSuccess(netip.MustParseAddr("192.0.2.1"), ExtenderConnectModeTcpTls)
	limitedUntil := directory.RecordLimited(limitedIp, 30*time.Second)
	assertIpOrder(t, directory.Candidates(4, 8), "192.0.2.3", "192.0.2.1", "192.0.2.2", "192.0.2.5")
	heldEntry := testDirectoryEntry(t, directory, heldIp)
	version, _ := directory.ChangeMonitor().Get()

	directory.RefreshLatencies()
	// the order still ranks by the samples, and the limited address stays
	// last
	candidates := directory.Candidates(4, 8)
	assertIpOrder(t, candidates, "192.0.2.3", "192.0.2.1", "192.0.2.2", "192.0.2.5")
	assertTestCandidateSamples(
		t,
		"after the path change",
		candidates,
		map[string]time.Duration{"192.0.2.3": 20 * time.Millisecond, "192.0.2.1": 80 * time.Millisecond},
		map[string]bool{"192.0.2.3": true, "192.0.2.1": true},
	)
	if !candidates[0].LatencyAttested {
		t.Fatal("the attested sample lost its attestation")
	}
	if after, _ := directory.ChangeMonitor().Get(); after != version {
		t.Fatalf("version = %d after the path change, expected no change from %d", after, version)
	}
	if entry := testDirectoryEntry(t, directory, netip.MustParseAddr("192.0.2.3")); entry.Latency != 20*time.Millisecond {
		t.Fatalf("the status shows %s, expected the sample kept", entry.Latency)
	}
	for _, c := range []struct {
		attesting bool
		count     int
	}{
		{attesting: false, count: 2},
		{attesting: true, count: 1},
	} {
		if latencies := directory.MeasuredLatencies(4, c.attesting); len(latencies) != c.count {
			t.Fatalf("latencies = %v attesting=%t, expected %d kept", latencies, c.attesting, c.count)
		}
		if latencies := directory.probeWindowLatencies(4, c.attesting); len(latencies) != 0 {
			t.Fatalf("window = %v attesting=%t, expected no sample due a refresh to count", latencies, c.attesting)
		}
	}
	// the probe pass measures the samples due a refresh first, lowest first,
	// then the unmeasured; a provider's pass counts the unattested one as none
	assertIpOrder(t, directory.ProbeCandidates(4, 8, false), "192.0.2.3", "192.0.2.1", "192.0.2.2")
	assertIpOrder(t, directory.ProbeCandidates(4, 8, true), "192.0.2.3", "192.0.2.1", "192.0.2.2")
	// the rest of the local evidence stays
	if entry := testDirectoryEntry(t, directory, netip.MustParseAddr("192.0.2.1")); entry.SuccessCount != 1 {
		t.Fatalf("success count = %d, expected the success to stay", entry.SuccessCount)
	}
	if until := directory.AddressLimitedUntil(limitedIp); !until.Equal(limitedUntil) {
		t.Fatalf("limited until %s, expected the limit to stay at %s", until, limitedUntil)
	}
	if state := testDirectoryState(t, directory, heldIp); state != ExtenderStateHold {
		t.Fatalf("state = %s, expected the hold to stay", state)
	}
	if entry := testDirectoryEntry(t, directory, heldIp); entry.FailureCount != heldEntry.FailureCount || !entry.LastFailureTime.Equal(heldEntry.LastFailureTime) {
		t.Fatalf("failures = %d at %s, expected %d at %s", entry.FailureCount, entry.LastFailureTime, heldEntry.FailureCount, heldEntry.LastFailureTime)
	}

	// a sample on the new path replaces the old one and counts toward the
	// window; the other stays due
	directory.RecordLatency(netip.MustParseAddr("192.0.2.1"), 10*time.Millisecond, false)
	candidates = directory.Candidates(4, 8)
	assertIpOrder(t, candidates, "192.0.2.1", "192.0.2.3", "192.0.2.2", "192.0.2.5")
	assertTestCandidateSamples(
		t,
		"after a sample on the new path",
		candidates,
		map[string]time.Duration{"192.0.2.1": 10 * time.Millisecond, "192.0.2.3": 20 * time.Millisecond},
		map[string]bool{"192.0.2.3": true},
	)
	if latencies := directory.probeWindowLatencies(4, false); len(latencies) != 1 || latencies[0] != 10*time.Millisecond {
		t.Fatalf("window = %v, expected the new sample alone", latencies)
	}
	assertIpOrder(t, directory.ProbeCandidates(4, 8, false), "192.0.2.3", "192.0.2.2", "192.0.2.1")

	// another path change makes the new sample due too, and changes nothing
	// else either
	directory.RefreshLatencies()
	if after, _ := directory.ChangeMonitor().Get(); after != version+1 {
		t.Fatalf("version = %d, expected the one change of the sample from %d", after, version)
	}
	assertTestCandidateSamples(
		t,
		"after a second path change",
		directory.Candidates(4, 8),
		map[string]time.Duration{"192.0.2.1": 10 * time.Millisecond, "192.0.2.3": 20 * time.Millisecond},
		map[string]bool{"192.0.2.1": true, "192.0.2.3": true},
	)
}

// A network client that owns its probe pass, with no goroutines: the pass and
// the path change are called directly. Two manual v4 extenders answer in 20
// and 25 ms, both close, so one pass fills the window of two.
func newTestSampleClockProbeClient(
	t *testing.T,
	now func() time.Time,
	advance func(d time.Duration),
	probes *testProbeLog,
) *ExtenderNetworkClient {
	t.Helper()
	directory, _ := newTestExtenderDirectory(t, newTestClock(), func(settings *ExtenderDirectorySettings) {
		settings.Now = now
	})
	for _, ip := range []string{"192.0.2.10", "192.0.2.11"} {
		directory.AddManual(netip.MustParseAddr(ip))
	}
	settings := DefaultExtenderNetworkClientSettings()
	settings.Now = now
	settings.ProbeWindowCount = 2
	settings.ProbeCountPerExtender = 1
	settings.ProbeCloseFactor = 2
	settings.ProbeCloseFloor = 10 * time.Millisecond
	settings.IpVersionSupported = func(ipVersion int) bool { return ipVersion == 4 }
	settings.Probe = func(ctx context.Context, candidate *ExtenderCandidate, attestor *ExtenderProbeAttestor) (time.Duration, ExtenderPingOutcome, error) {
		advance(time.Second)
		return probes.probe(ctx, candidate, attestor)
	}
	return &ExtenderNetworkClient{
		ctx:           t.Context(),
		log:           NewNoopLogger(),
		directory:     directory,
		settings:      settings,
		statusMonitor: NewMonitorValue[ExtenderNetworkClientStatus](ExtenderNetworkClientStatus{}),
		wakeMonitor:   NewMonitor(),
		probeWake:     NewMonitor(),
		hintWake:      NewMonitor(),
	}
}

// The probes of the two extenders of newTestSampleClockProbeClient.
func newTestSampleClockProbeLog() *testProbeLog {
	return newTestProbeLog(map[string]time.Duration{
		"192.0.2.10": 20 * time.Millisecond,
		"192.0.2.11": 25 * time.Millisecond,
	})
}

// Sets the rtt each ip answers the probes with from here on.
func (self *testProbeLog) setRtts(rtts map[string]time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.rtts = rtts
}

// A path change keeps the samples of the old path in use: the candidate order
// ranks by them at once. Before, it dropped them, and the order fell back to
// the unmeasured one until the probe pass had measured again. The pass that
// follows the first sample on the new path measures both again, though both
// were current, and each new sample replaces an old one as it lands, so no
// candidate goes without one in between. Here the new path reverses the two.
func TestExtenderNetworkClientMeasuresAgainAfterAPathChange(t *testing.T) {
	clock := newTestClock()
	probes := newTestSampleClockProbeLog()
	networkClient := newTestSampleClockProbeClient(t, clock.Now, clock.advance, probes)
	directory := networkClient.directory

	networkClient.probePass()
	if count := probes.count(); count != 2 {
		t.Fatalf("probes = %d, expected both extenders", count)
	}
	// the window is full: another pass on the same path measures nothing
	networkClient.probePass()
	if count := probes.count(); count != 2 {
		t.Fatalf("probes = %d, expected none with the window full", count)
	}

	probes.setRtts(map[string]time.Duration{
		"192.0.2.10": 40 * time.Millisecond,
		"192.0.2.11": 15 * time.Millisecond,
	})
	networkClient.networkChanged()
	candidates := directory.Candidates(4, 8)
	assertIpOrder(t, candidates, "192.0.2.10", "192.0.2.11")
	assertTestCandidateSamples(
		t,
		"after the path change",
		candidates,
		map[string]time.Duration{"192.0.2.10": 20 * time.Millisecond, "192.0.2.11": 25 * time.Millisecond},
		map[string]bool{"192.0.2.10": true, "192.0.2.11": true},
	)

	// the samples the candidates carry as each probe of the next pass starts
	probeIpLatencies := []map[string]time.Duration{}
	probe := networkClient.settings.Probe
	networkClient.settings.Probe = func(ctx context.Context, candidate *ExtenderCandidate, attestor *ExtenderProbeAttestor) (time.Duration, ExtenderPingOutcome, error) {
		ipLatencies, _ := testCandidateSamples(directory.Candidates(4, 8))
		probeIpLatencies = append(probeIpLatencies, ipLatencies)
		return probe(ctx, candidate, attestor)
	}
	networkClient.probePass()
	if count := probes.count(); count != 4 {
		t.Fatalf("probes = %d, expected the pass after the path change to measure both again", count)
	}
	// the lowest sample due a refresh is measured first, and the other keeps
	// its old sample until its own probe
	if len(probeIpLatencies) != 2 ||
		probeIpLatencies[0]["192.0.2.10"] != 20*time.Millisecond ||
		probeIpLatencies[0]["192.0.2.11"] != 25*time.Millisecond ||
		probeIpLatencies[1]["192.0.2.10"] != 40*time.Millisecond ||
		probeIpLatencies[1]["192.0.2.11"] != 25*time.Millisecond {
		t.Fatalf("samples at each probe = %v, expected both old ones at the first, then .10's new one beside .11's old one", probeIpLatencies)
	}
	candidates = directory.Candidates(4, 8)
	assertIpOrder(t, candidates, "192.0.2.11", "192.0.2.10")
	assertTestCandidateSamples(
		t,
		"after the pass",
		candidates,
		map[string]time.Duration{"192.0.2.10": 40 * time.Millisecond, "192.0.2.11": 15 * time.Millisecond},
		map[string]bool{},
	)
	// nothing is due any more: the next pass measures nothing
	networkClient.probePass()
	if count := probes.count(); count != 4 {
		t.Fatalf("probes = %d, expected one pass to measure again", count)
	}
}

// After the host slept past LatencyMaxAge the probe pass measures again,
// where the samples from before the sleep kept its window full until as much
// awake time had passed.
func TestExtenderNetworkClientMeasuresAgainAfterTheHostSlept(t *testing.T) {
	hostClock := newTestHostClock(t)
	probes := newTestSampleClockProbeLog()
	networkClient := newTestSampleClockProbeClient(t, hostClock.Now, hostClock.awake, probes)

	networkClient.probePass()
	if count := probes.count(); count != 2 {
		t.Fatalf("probes = %d, expected both extenders", count)
	}
	hostClock.awake(time.Hour)
	networkClient.probePass()
	if count := probes.count(); count != 2 {
		t.Fatalf("probes = %d, expected none an hour on", count)
	}

	hostClock.sleep(networkClient.directory.settings.LatencyMaxAge)
	networkClient.probePass()
	if count := probes.count(); count != 4 {
		t.Fatalf("probes = %d, expected the pass after the sleep to measure both again", count)
	}
}

// The strategy draws its extender dialers in the directory's candidate order.
// Before, after a night's sleep it still spent its expand budget on the
// extender that was fastest on the path before the sleep; that sample has aged
// out now, and the budget goes where an unmeasured directory puts it.
func TestClientStrategyExpandsPastASampleTheHostSleptThrough(t *testing.T) {
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

	directory.RecordLatency(netip.MustParseAddr("192.0.2.3"), 10*time.Millisecond, false)
	if candidates := directory.Candidates(4, 1); candidates[0].Ip != netip.MustParseAddr("192.0.2.3") {
		t.Fatalf("first candidate = %s before the sleep, expected the measured one", candidates[0].Ip)
	}

	hostClock.awake(time.Minute)
	hostClock.sleep(directory.settings.LatencyMaxAge)
	expandedDialers := clientStrategy.expandExtenderDialers()
	if len(expandedDialers) != 1 {
		t.Fatalf("dialers = %d, expected the budget of one", len(expandedDialers))
	}
	if ip := expandedDialers[0].extenderConfig.Ip; ip != netip.MustParseAddr("192.0.2.1") {
		t.Fatalf("the strategy expanded %s, expected 192.0.2.1 with the sample from before the sleep aged out", ip)
	}
}

// A peer pinger renews its pings, and draws its sample again, by the time the
// host slept too. Before, both came due on the monotonic clock alone, so an
// extender host that slept through a night renewed its pings a whole refresh
// of awake time after the last, past the day the operator keeps them for
// (GEOMAP §2.1).
func TestExtenderPeerPingerRefreshesAfterTheHostSlept(t *testing.T) {
	hostClock := newTestHostClock(t)
	// the pinger's clock, counting its reads: with no ping in flight the
	// pinger reads it once per pass, so a count of reads is a pass barrier
	var readCount atomic.Int64
	read := make(chan struct{}, 1)
	now := func() time.Time {
		readCount.Add(1)
		select {
		case read <- struct{}{}:
		default:
		}
		return hostClock.Now()
	}
	fixture := newTestPeerPingerFixture(t, "192.0.2.1")
	fixture.addPeer(t, "192.0.2.11")
	pinger := fixture.start(t, func(settings *ExtenderPeerPingerSettings) {
		settings.Now = now
		settings.SpreadTimeout = 0
		settings.RefreshTimeout = 12 * time.Hour
		settings.ProbeCount = 1
	})
	// no ping is in flight and none was made past `callCount`, established
	// over three passes
	assertNoNewPing := func(what string, callCount int) {
		t.Helper()
		target := readCount.Load() + 3
		deadline := time.After(10 * time.Second)
		for readCount.Load() < target {
			select {
			case <-read:
			case <-deadline:
				t.Fatalf("%s: the pinger read its clock %d times, expected %d", what, readCount.Load(), target)
			}
		}
		pinging := func() bool {
			pinger.stateLock.Lock()
			defer pinger.stateLock.Unlock()
			for _, peer := range pinger.peers {
				if peer.pinging {
					return true
				}
			}
			return false
		}()
		if pinging {
			t.Fatalf("%s: a ping is in flight", what)
		}
		if calls := fixture.pings.callsValue(); len(calls) != callCount {
			t.Fatalf("%s: calls = %v, expected %d", what, calls, callCount)
		}
	}
	sampleTimeValue := func() time.Time {
		pinger.stateLock.Lock()
		defer pinger.stateLock.Unlock()
		return pinger.sampleTime
	}

	waitForPingCount(t, pinger, 1)
	assertNoNewPing("after the first ping", 1)
	sampleTime := sampleTimeValue()

	hostClock.awake(time.Hour)
	assertNoNewPing("an hour on", 1)
	if !sampleTimeValue().Equal(sampleTime) {
		t.Fatal("the sample was drawn again an hour on")
	}

	// a night's sleep, past the refresh by the wall clock and eleven hours
	// short of it by the monotonic one
	hostClock.sleep(12 * time.Hour)
	waitForPingCount(t, pinger, 2)
	if sampleTimeValue().Equal(sampleTime) {
		t.Fatal("the sample was not drawn again after the sleep")
	}
}
