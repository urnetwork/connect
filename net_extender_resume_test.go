package connect

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Measuring again after the host resumes from a sleep on the same path
// (DESIGNNOTES4.md §6).
//
// The probe loop's timers run on the monotonic clock, which stops while the
// host sleeps, and nothing else woke it on a resume with no path change: a
// member that woke after a night measured again only once the rest of the
// refresh period had passed in awake time, up to six hours. A resume keeps
// the samples from before the sleep in use, due a refresh, so the candidate
// order ranks by them until the probe pass has measured them again. These
// tests run on the model of the host clock of net_extender_sample_clock_test.go,
// whose sleep moves the wall reading alone, and drive the probe loop's checks
// through its wait seam, so no test sleeps.

// The bug: a resume dropped the samples taken before the sleep, so until the
// probe pass measured again the candidate order ranked those extenders as
// never measured. A resume keeps every sample: the ones the host slept
// through are due a refresh, still ranking but out of the probe pass's
// window, and the ones taken since the host woke are not; the hold stays.
func TestExtenderDirectoryRefreshSleptLatenciesKeepsEverySample(t *testing.T) {
	hostClock := newTestHostClock(t)
	directory := newTestHostClockDirectory(t, hostClock, "192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4")
	beforeIp := netip.MustParseAddr("192.0.2.1")
	afterIp := netip.MustParseAddr("192.0.2.2")
	heldIp := netip.MustParseAddr("192.0.2.3")
	minSleep := 15 * time.Minute
	// the samples the order ranks by and the ones the probe pass's window
	// counts, each in ascending order
	assertLatencies := func(what string, expected []time.Duration, expectedWindow []time.Duration) {
		t.Helper()
		latencies := directory.MeasuredLatencies(4, false)
		window := directory.probeWindowLatencies(4, false)
		slices.Sort(latencies)
		slices.Sort(window)
		if !slices.Equal(latencies, expected) || !slices.Equal(window, expectedWindow) {
			t.Fatalf("%s: latencies = %v, window = %v, expected %v and %v", what, latencies, window, expected, expectedWindow)
		}
	}
	bothLatencies := []time.Duration{20 * time.Millisecond, 30 * time.Millisecond}

	directory.RecordLatency(beforeIp, 20*time.Millisecond, true)
	directory.RecordLatency(heldIp, 40*time.Millisecond, false)
	hostClock.awake(time.Hour)
	hostClock.sleep(time.Hour)
	hostClock.awake(time.Minute)
	directory.RecordLatency(afterIp, 30*time.Millisecond, true)
	// six failures hold it for 320 min, past every sleep below
	for range 6 {
		directory.RecordFailure(heldIp, ExtenderConnectModeTcpTls)
	}
	version, _ := directory.ChangeMonitor().Get()

	directory.RefreshSleptLatencies(minSleep)
	assertLatencies("after the resume", bothLatencies, []time.Duration{30 * time.Millisecond})
	if latencies := directory.probeWindowLatencies(4, true); len(latencies) != 1 {
		t.Fatalf("attested window = %v, expected the one taken since the host woke", latencies)
	}
	if latencies := directory.MeasuredLatencies(4, true); len(latencies) != 2 {
		t.Fatalf("attested latencies = %v, expected both kept", latencies)
	}
	// the order still ranks by both
	candidates := directory.Candidates(4, 8)
	assertIpOrder(t, candidates, "192.0.2.1", "192.0.2.2", "192.0.2.4")
	assertTestCandidateSamples(
		t,
		"after the resume",
		candidates,
		map[string]time.Duration{"192.0.2.1": 20 * time.Millisecond, "192.0.2.2": 30 * time.Millisecond},
		map[string]bool{"192.0.2.1": true},
	)
	// the probe pass takes the sample due a refresh first, then the
	// unmeasured
	assertIpOrder(t, directory.ProbeCandidates(4, 8, false), "192.0.2.1", "192.0.2.4", "192.0.2.2")
	if entry := testDirectoryEntry(t, directory, heldIp); entry.Latency != 40*time.Millisecond {
		t.Fatalf("the held address shows %s, expected its sample from before the sleep kept", entry.Latency)
	}
	if state := testDirectoryState(t, directory, heldIp); state != ExtenderStateHold {
		t.Fatalf("state = %s, expected the hold to stay", state)
	}
	if after, _ := directory.ChangeMonitor().Get(); after != version {
		t.Fatalf("version = %d after the resume, expected no change from %d", after, version)
	}

	// nothing more is due
	directory.RefreshSleptLatencies(minSleep)
	assertLatencies("after a second look", bothLatencies, []time.Duration{30 * time.Millisecond})
	// a nap is not a resume
	hostClock.sleep(minSleep - time.Second)
	directory.RefreshSleptLatencies(minSleep)
	assertLatencies("after a nap", bothLatencies, []time.Duration{30 * time.Millisecond})
	// by choice: a wall clock set back hides as much sleep
	hostClock.setBack(time.Hour)
	hostClock.sleep(time.Hour)
	directory.RefreshSleptLatencies(minSleep)
	assertLatencies("after a sleep a set back hid", bothLatencies, []time.Duration{30 * time.Millisecond})
	// a minimum <= 0 makes nothing due
	hostClock.sleep(time.Hour)
	directory.RefreshSleptLatencies(0)
	assertLatencies("with no minimum", bothLatencies, []time.Duration{30 * time.Millisecond})
	directory.RefreshSleptLatencies(minSleep)
	assertLatencies("after the next sleep", bothLatencies, []time.Duration{})
}

// A network client that runs its probe loop alone, on the host clock, over
// the two extenders of newTestSampleClockProbeClient. The loop never comes due
// on the refresh period, and hands every resume check it arms to the returned
// channel, so a test fires each check, and an armed check is a barrier: the
// loop has finished with everything before it.
func newTestResumeProbeClient(
	t *testing.T,
	hostClock *testHostClock,
	probes *testProbeLog,
) (*ExtenderNetworkClient, <-chan chan time.Time) {
	t.Helper()
	networkClient := newTestSampleClockProbeClient(t, hostClock.Now, hostClock.awake, probes)
	armedChecks := make(chan chan time.Time)
	networkClient.settings.ProbeAfter = func(wait time.Duration) <-chan time.Time {
		if wait != networkClient.settings.ResumeCheckTimeout {
			return nil
		}
		check := make(chan time.Time, 1)
		select {
		case armedChecks <- check:
		case <-networkClient.ctx.Done():
		}
		return check
	}
	ready := make(chan struct{})
	close(ready)
	networkClient.initialProbeReady = ready
	networkClient.initialHintDone = ready
	probeDone := make(chan struct{})
	go func() {
		defer close(probeDone)
		networkClient.runProbes()
	}()
	// the context is canceled before the cleanups run
	t.Cleanup(func() {
		<-probeDone
	})
	return networkClient, armedChecks
}

// The next check the loop arms, which it does once it has finished with
// `what`.
func nextTestResumeCheck(t *testing.T, armedChecks <-chan chan time.Time, what string) chan time.Time {
	t.Helper()
	select {
	case check := <-armedChecks:
		return check
	case <-time.After(10 * time.Second):
		t.Fatalf("the loop armed no check after %s", what)
		return nil
	}
}

// Fires `check` and returns the check the loop arms after it.
func fireTestResumeCheck(t *testing.T, check chan time.Time, armedChecks <-chan chan time.Time) chan time.Time {
	t.Helper()
	check <- time.Time{}
	return nextTestResumeCheck(t, armedChecks, "the check")
}

// What runFeed does at the end of a sample: the status takes its time, and the
// probe pass is woken to follow it.
func completeTestResumeSample(networkClient *ExtenderNetworkClient) {
	sampleTime := networkClient.settings.Now()
	networkClient.updateStatus(func(status *ExtenderNetworkClientStatus) {
		status.LastSampleTime = sampleTime
	})
	networkClient.probeWake.NotifyAll()
}

// Whether a monitor channel subscribed before has fired.
func testMonitorFired(notify <-chan struct{}) bool {
	select {
	case <-notify:
		return true
	default:
		return false
	}
}

// The bug: a member that woke on the same path had nothing to wake its probe
// loop, and measured again only once the rest of the refresh period had
// passed in awake time. The loop checks the host clock while it waits now: a
// resume keeps the samples from before the sleep in use, due a refresh, and
// asks the refresh loop for a sample, and the pass that follows the sample
// measures both extenders again, the new samples replacing the old. Nothing
// is probed before that sample, and one resume asks once.
func TestExtenderNetworkClientMeasuresAgainAfterTheHostResumes(t *testing.T) {
	hostClock := newTestHostClock(t)
	probes := newTestSampleClockProbeLog()
	networkClient, armedChecks := newTestResumeProbeClient(t, hostClock, probes)

	check := nextTestResumeCheck(t, armedChecks, "the first pass")
	if count := probes.count(); count != 2 {
		t.Fatalf("probes = %d, expected the first pass to measure both extenders", count)
	}
	refreshWake := networkClient.wakeMonitor.NotifyChannel()

	// a night's sleep short of LatencyMaxAge, so the samples are current by
	// their age; the check that sees it waits for the host to stay awake
	hostClock.sleep(8 * time.Hour)
	check = fireTestResumeCheck(t, check, armedChecks)
	if latencies := networkClient.directory.MeasuredLatencies(4, false); len(latencies) != 2 {
		t.Fatalf("latencies = %v at the check that saw the sleep, expected both", latencies)
	}
	if testMonitorFired(refreshWake) {
		t.Fatal("the refresh loop was woken before the host had stayed awake")
	}

	hostClock.awake(time.Minute)
	check = fireTestResumeCheck(t, check, armedChecks)
	// the samples from before the sleep still rank, due a refresh
	assertTestCandidateSamples(
		t,
		"after the resume",
		networkClient.directory.Candidates(4, 8),
		map[string]time.Duration{"192.0.2.10": 20 * time.Millisecond, "192.0.2.11": 25 * time.Millisecond},
		map[string]bool{"192.0.2.10": true, "192.0.2.11": true},
	)
	if window := networkClient.directory.probeWindowLatencies(4, false); len(window) != 0 {
		t.Fatalf("window = %v after the resume, expected no sample from before the sleep to count", window)
	}
	if !testMonitorFired(refreshWake) {
		t.Fatal("the resume did not wake the refresh loop for a sample")
	}
	if count := probes.count(); count != 2 {
		t.Fatalf("probes = %d, expected none before a sample completed after the sleep", count)
	}

	// the checks after it ask for nothing more
	refreshWake = networkClient.wakeMonitor.NotifyChannel()
	for range 3 {
		hostClock.awake(time.Minute)
		check = fireTestResumeCheck(t, check, armedChecks)
	}
	if testMonitorFired(refreshWake) {
		t.Fatal("one resume woke the refresh loop again")
	}
	if count := probes.count(); count != 2 {
		t.Fatalf("probes = %d, expected none before a sample", count)
	}

	// the host woke somewhere the two answer otherwise
	probes.setRtts(map[string]time.Duration{
		"192.0.2.10": 30 * time.Millisecond,
		"192.0.2.11": 12 * time.Millisecond,
	})
	completeTestResumeSample(networkClient)
	nextTestResumeCheck(t, armedChecks, "the pass after the sample")
	if count := probes.count(); count != 4 {
		t.Fatalf("probes = %d, expected the pass after the sample to measure both again", count)
	}
	candidates := networkClient.directory.Candidates(4, 8)
	assertIpOrder(t, candidates, "192.0.2.11", "192.0.2.10")
	assertTestCandidateSamples(
		t,
		"after the pass",
		candidates,
		map[string]time.Duration{"192.0.2.10": 30 * time.Millisecond, "192.0.2.11": 12 * time.Millisecond},
		map[string]bool{},
	)
}

// No sleep, no resume: awake time, a nap under the minimum and a wall clock
// set back a day leave the samples, the refresh loop and the probe pass alone.
func TestExtenderNetworkClientTellsNoResumeWithoutASleep(t *testing.T) {
	hostClock := newTestHostClock(t)
	probes := newTestSampleClockProbeLog()
	networkClient, armedChecks := newTestResumeProbeClient(t, hostClock, probes)

	check := nextTestResumeCheck(t, armedChecks, "the first pass")
	refreshWake := networkClient.wakeMonitor.NotifyChannel()
	for range 10 {
		hostClock.awake(time.Minute)
		check = fireTestResumeCheck(t, check, armedChecks)
	}
	hostClock.sleep(networkClient.settings.ResumeMinSleep - time.Second)
	check = fireTestResumeCheck(t, check, armedChecks)
	for range 2 {
		hostClock.awake(time.Minute)
		check = fireTestResumeCheck(t, check, armedChecks)
	}
	hostClock.setBack(24 * time.Hour)
	check = fireTestResumeCheck(t, check, armedChecks)
	for range 2 {
		hostClock.awake(time.Minute)
		check = fireTestResumeCheck(t, check, armedChecks)
	}

	if latencies := networkClient.directory.probeWindowLatencies(4, false); len(latencies) != 2 {
		t.Fatalf("window = %v, expected both samples to stay, none due a refresh", latencies)
	}
	if testMonitorFired(refreshWake) {
		t.Fatal("the refresh loop was woken with no sleep")
	}
	if count := probes.count(); count != 2 {
		t.Fatalf("probes = %d, expected only the first pass", count)
	}
}

// A sample that completed after the host woke has shown the path works: the
// resume wakes the probe pass at once and asks for no sample. The pass that
// followed that sample found its window full of the samples from before the
// sleep, current by their age, and measured nothing.
func TestExtenderNetworkClientResumeAfterASampleMeasuresAtOnce(t *testing.T) {
	hostClock := newTestHostClock(t)
	probes := newTestSampleClockProbeLog()
	networkClient, armedChecks := newTestResumeProbeClient(t, hostClock, probes)

	check := nextTestResumeCheck(t, armedChecks, "the first pass")
	hostClock.sleep(8 * time.Hour)
	check = fireTestResumeCheck(t, check, armedChecks)
	completeTestResumeSample(networkClient)
	check = nextTestResumeCheck(t, armedChecks, "the pass after the sample")
	if count := probes.count(); count != 2 {
		t.Fatalf("probes = %d, expected the pass after the sample to find its window full", count)
	}

	refreshWake := networkClient.wakeMonitor.NotifyChannel()
	hostClock.awake(time.Minute)
	// the check that tells the resume, then the pass it wakes
	fireTestResumeCheck(t, check, armedChecks)
	nextTestResumeCheck(t, armedChecks, "the pass the resume woke")
	if count := probes.count(); count != 4 {
		t.Fatalf("probes = %d, expected the resume to measure both again at once", count)
	}
	if testMonitorFired(refreshWake) {
		t.Fatal("the resume asked for a sample after one had completed")
	}
}

// A sleep past LatencyMaxAge aged the samples out, so the pass that followed
// the first sample after the wake measured both already. The resume keeps what
// that pass measured, and its pass finds the window full: one resume, one pass
// that probes.
func TestExtenderNetworkClientResumeKeepsWhatWasMeasuredSinceTheWake(t *testing.T) {
	hostClock := newTestHostClock(t)
	probes := newTestSampleClockProbeLog()
	networkClient, armedChecks := newTestResumeProbeClient(t, hostClock, probes)

	check := nextTestResumeCheck(t, armedChecks, "the first pass")
	hostClock.sleep(networkClient.directory.settings.LatencyMaxAge + time.Hour)
	check = fireTestResumeCheck(t, check, armedChecks)
	completeTestResumeSample(networkClient)
	check = nextTestResumeCheck(t, armedChecks, "the pass after the sample")
	if count := probes.count(); count != 4 {
		t.Fatalf("probes = %d, expected the pass after the sample to measure the aged samples", count)
	}

	hostClock.awake(time.Minute)
	fireTestResumeCheck(t, check, armedChecks)
	nextTestResumeCheck(t, armedChecks, "the pass the resume woke")
	if latencies := networkClient.directory.probeWindowLatencies(4, false); len(latencies) != 2 {
		t.Fatalf("window = %v after the resume, expected the two taken since the wake, neither due a refresh", latencies)
	}
	if count := probes.count(); count != 4 {
		t.Fatalf("probes = %d, expected the resume to measure nothing again", count)
	}
}

// What a resume asks for, by what the feed did around the sleep: a stream
// from before the sleep is closed so the refresh loop takes a sample over a
// new one, a stream opened since is left to finish its sample, which wakes
// the pass, and a sample completed since wakes the pass at once. The samples
// from before the sleep stay in use, due a refresh, in every case.
func TestExtenderNetworkClientResumeAsksForASampleAfterTheSleep(t *testing.T) {
	cases := []struct {
		name string
		// what the feed did before the sleep and since the host woke
		streamBefore bool
		sampleBefore bool
		streamSince  bool
		sampleSince  bool
		// what the resume does
		closesStream bool
		wakesRefresh bool
		wakesProbe   bool
	}{
		{
			name:         "a subscription from before the sleep",
			streamBefore: true,
			sampleBefore: true,
			closesStream: true,
			wakesRefresh: true,
		},
		{
			name:         "a member between one-shot samples",
			sampleBefore: true,
			wakesRefresh: true,
		},
		{
			name:         "no sample yet",
			wakesRefresh: true,
		},
		{
			name:         "a stream opened since, still taking its sample",
			sampleBefore: true,
			streamSince:  true,
		},
		{
			name:        "a stream opened since that completed its sample",
			streamSince: true,
			sampleSince: true,
			wakesProbe:  true,
		},
	}
	for _, c := range cases {
		hostClock := newTestHostClock(t)
		probes := newTestSampleClockProbeLog()
		networkClient := newTestSampleClockProbeClient(t, hostClock.Now, hostClock.awake, probes)
		networkClient.probePass()
		var stream *ExtenderFeedStream
		var serverConn net.Conn
		openStream := func() {
			stream, serverConn = newTestFeedStream(t)
			networkClient.setFeedStream(stream)
		}
		if c.streamBefore {
			openStream()
		}
		if c.sampleBefore {
			completeTestResumeSample(networkClient)
		}
		hostClock.awake(time.Minute)
		hostClock.sleep(8 * time.Hour)
		hostClock.awake(time.Minute)
		if c.streamSince {
			openStream()
		}
		if c.sampleSince {
			completeTestResumeSample(networkClient)
		}
		refreshWake := networkClient.wakeMonitor.NotifyChannel()
		probeWake := networkClient.probeWake.NotifyChannel()

		networkClient.hostResumed(hostClock.Now(), 8*time.Hour)
		if latencies, window := networkClient.directory.MeasuredLatencies(4, false), networkClient.directory.probeWindowLatencies(4, false); len(latencies) != 2 || len(window) != 0 {
			t.Errorf("%s: latencies = %v, window = %v, expected both samples from before the sleep kept, due a refresh", c.name, latencies, window)
		}
		if woken := testMonitorFired(refreshWake); woken != c.wakesRefresh {
			t.Errorf("%s: refresh loop woken = %t, expected %t", c.name, woken, c.wakesRefresh)
		}
		if woken := testMonitorFired(probeWake); woken != c.wakesProbe {
			t.Errorf("%s: probe pass woken = %t, expected %t", c.name, woken, c.wakesProbe)
		}
		if stream == nil {
			continue
		}
		if c.closesStream {
			// the server end reads the end of the stream
			serverConn.SetReadDeadline(time.Now().Add(10 * time.Second))
			if _, err := serverConn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
				t.Errorf("%s: the stream from before the sleep is open: %v", c.name, err)
			}
			continue
		}
		// the stream still delivers what the server writes
		writeErrs := make(chan error, 1)
		go func() {
			writeErrs <- WriteExtenderFeedFrame(serverConn, &protocol.ExtenderFeedFrame{
				Frame: &protocol.ExtenderFeedFrame_Keepalive{Keepalive: true},
			})
		}()
		readCtx, readCancel := context.WithTimeout(context.Background(), 10*time.Second)
		frame, err := stream.Next(readCtx)
		readCancel()
		if err != nil || !frame.GetKeepalive() {
			t.Errorf("%s: the stream opened since the sleep was ended: %v", c.name, err)
		}
		if err := <-writeErrs; err != nil {
			t.Errorf("%s: write err = %v", c.name, err)
		}
	}
}
