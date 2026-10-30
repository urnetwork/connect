package connect

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The prober over a window of idle exits whose qualification is fresh, with
// the time of each scan recorded and its wake made as the constructor makes
// it.
func newSleepingProberTestMultiClient(ctx context.Context, cancel context.CancelFunc) (*RemoteUserNatMultiClient, *atomic.Pointer[[]time.Time]) {
	multiClient := newIdleProberTestMultiClient(ctx, cancel, 8)
	multiClient.proberWake = make(chan struct{}, 1)
	scanTimes := &atomic.Pointer[[]time.Time]{}
	scanTimes.Store(&[]time.Time{})
	multiClient.proberScanForTest = func() {
		times := append(*scanTimes.Load(), time.Now())
		scanTimes.Store(&times)
	}
	return multiClient, scanTimes
}

// The prober sleeps until an exit falls due, at most ProberScanMaxInterval,
// instead of rescanning a window that needs nothing every few seconds.
func TestProberSleepsUntilAnExitFallsDue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		multiClient, scanTimes := newSleepingProberTestMultiClient(ctx, cancel)
		maxInterval := multiClient.settings.ProberScanMaxInterval
		done := make(chan struct{})
		go func() {
			defer close(done)
			multiClient.runProber()
		}()
		defer func() {
			cancel()
			<-done
		}()

		// every exit is qualified, so nothing falls due inside the idle
		idle := QualificationMaxAge / 2
		time.Sleep(idle)
		synctest.Wait()
		if n, most := len(*scanTimes.Load()), int(idle/maxInterval)+1; most < n {
			t.Fatalf("the prober scanned %d times over %s with nothing due, want at most %d", n, idle, most)
		}
	})
}

// The scan falls exactly on the due time: an exit whose proof goes stale is
// re-probed then, not at the next bound.
func TestProberScansWhenAnExitFallsDue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		multiClient, scanTimes := newSleepingProberTestMultiClient(ctx, cancel)
		// a bound that does not divide the qualification lifetime
		multiClient.settings.ProberScanMaxInterval = 7 * time.Minute
		qualifiedAt := multiClient.qualification[MultiHopId{}].qualifiedAt
		done := make(chan struct{})
		go func() {
			defer close(done)
			multiClient.runProber()
		}()
		defer func() {
			cancel()
			<-done
		}()

		dueAt := qualifiedAt.Add(QualificationMaxAge)
		time.Sleep(time.Until(dueAt))
		synctest.Wait()
		times := *scanTimes.Load()
		if len(times) == 0 || !times[len(times)-1].Equal(dueAt) {
			t.Fatalf("the prober scanned at %v, want a scan when the exits fell due at %v", times, dueAt)
		}
	})
}

// A window admitting an exit wakes the prober to scan a join delay later,
// and the exits admitted inside that delay share the one scan: a joiner's
// first probe does not wait out the sleep, and a window formed in a burst is
// swept together.
func TestProberWakesForAJoiner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		multiClient, scanTimes := newSleepingProberTestMultiClient(ctx, cancel)
		done := make(chan struct{})
		go func() {
			defer close(done)
			multiClient.runProber()
		}()
		defer func() {
			cancel()
			<-done
		}()
		time.Sleep(multiClient.settings.ProberScanMaxInterval / 2)
		synctest.Wait()

		joinDelay := multiClient.settings.ProberJoinDelay
		window := multiClient.windows[WindowTypeQuality]
		admit := func() {
			joiner := stallTestChannel()
			func() {
				window.stateLock.Lock()
				defer window.stateLock.Unlock()
				window.clients[NewId()] = joiner
			}()
			multiClient.clientAdded(joiner)
		}
		before := len(*scanTimes.Load())
		admitTime := time.Now()
		admit()
		time.Sleep(joinDelay / 2)
		admit()
		time.Sleep(time.Until(admitTime.Add(joinDelay)) - time.Nanosecond)
		synctest.Wait()
		if n := len(*scanTimes.Load()) - before; n != 0 {
			t.Fatalf("the prober scanned %d times inside the join delay", n)
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		times := *scanTimes.Load()
		if n := len(times) - before; n != 1 || !times[len(times)-1].Equal(admitTime.Add(joinDelay)) {
			t.Fatalf("the prober scanned %d times by the end of the join delay (last at %v), want once at %v", n, times[len(times)-1], admitTime.Add(joinDelay))
		}
	})
}

// A reliability settings change wakes the prober, so turning the probe on is
// honored within the join delay rather than the long sleep.
func TestProberWakesForASettingsChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		multiClient, scanTimes := newSleepingProberTestMultiClient(ctx, cancel)
		off := *multiClient.defaultReliabilitySettings
		off.ProviderProbe = false
		multiClient.SetReliabilitySettings(&off)
		done := make(chan struct{})
		go func() {
			defer close(done)
			multiClient.runProber()
		}()
		defer func() {
			cancel()
			<-done
		}()
		time.Sleep(multiClient.settings.ProberScanMaxInterval / 2)
		synctest.Wait()

		before := len(*scanTimes.Load())
		on := off
		on.ProviderProbe = true
		multiClient.SetReliabilitySettings(&on)
		time.Sleep(multiClient.settings.ProberJoinDelay)
		synctest.Wait()
		if len(*scanTimes.Load()) == before {
			t.Fatal("a settings change did not wake the prober within the join delay")
		}
	})
}

// The plan's next due time agrees with its picks: a candidate it passes over
// is not picked an instant before its due time and is picked at it.
func TestProberPlanNextDue(t *testing.T) {
	now := time.Now()
	settings := DefaultMultiClientSettings()
	cases := []struct {
		name      string
		candidate proberCandidate
		// zero: nothing to wait for
		due time.Time
	}{
		{"attempt floor",
			proberCandidate{lastAttemptAt: now.Add(-time.Second)},
			now.Add(-time.Second + settings.ProberAttemptMinInterval)},
		{"fresh qualification",
			proberCandidate{lastProbeAt: now.Add(-time.Minute), qualifiedAt: now.Add(-time.Minute)},
			now.Add(-time.Minute + QualificationMaxAge)},
		{"stale but recently probed",
			proberCandidate{lastProbeAt: now.Add(-time.Minute), qualifiedAt: now.Add(-QualificationMaxAge - time.Second)},
			now.Add(-time.Minute + settings.ProberReprobeInterval)},
		{"the latest wait wins",
			proberCandidate{
				lastAttemptAt: now.Add(-time.Second),
				lastProbeAt:   now.Add(-settings.ProberReprobeInterval + 2*settings.ProberAttemptMinInterval),
			},
			now.Add(2 * settings.ProberAttemptMinInterval)},
		{"loaded and stale waits on its flows, not a time",
			proberCandidate{flowCount: 3, lastProbeAt: now.Add(-time.Minute), qualifiedAt: now.Add(-QualificationMaxAge - time.Second)},
			time.Time{}},
		{"in flight waits on its attempt floor",
			proberCandidate{inFlight: true, lastAttemptAt: now.Add(-time.Second)},
			now.Add(-time.Second + settings.ProberAttemptMinInterval)},
		{"in flight past its floor waits on nothing",
			proberCandidate{inFlight: true, lastAttemptAt: now.Add(-2 * settings.ProberAttemptMinInterval)},
			time.Time{}},
		{"picked now falls due again at its floor",
			proberCandidate{},
			now.Add(settings.ProberAttemptMinInterval)},
	}
	for _, c := range cases {
		_, due := proberPlan(now, []proberCandidate{c.candidate}, settings)
		if !due.Equal(c.due) {
			t.Errorf("%s: due %v, want %v", c.name, due, c.due)
			continue
		}
		// a candidate passed over is picked exactly at its due time
		if due.IsZero() || c.candidate.inFlight || c.candidate == (proberCandidate{}) {
			continue
		}
		if picks, _ := proberPlan(due.Add(-time.Nanosecond), []proberCandidate{c.candidate}, settings); len(picks) != 0 {
			t.Errorf("%s: picked an instant before its due time", c.name)
		}
		if picks, _ := proberPlan(due, []proberCandidate{c.candidate}, settings); len(picks) != 1 {
			t.Errorf("%s: not picked at its due time", c.name)
		}
	}

	// the earliest wait across candidates
	_, due := proberPlan(now, []proberCandidate{
		{lastProbeAt: now, qualifiedAt: now},
		{lastAttemptAt: now.Add(-time.Second)},
		{flowCount: 1, lastProbeAt: now.Add(-time.Hour)},
	}, settings)
	if want := now.Add(-time.Second + settings.ProberAttemptMinInterval); !due.Equal(want) {
		t.Errorf("due %v across candidates, want the earliest %v", due, want)
	}
}

// Both windows the parent builds wake its prober when they admit an exit.
func TestWindowsWakeTheProberOnAdmission(t *testing.T) {
	source, err := readSource("ip_remote_multi_client.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := functionBody(source, "func (self *multiClientWindow) expand(")
	if !ok {
		t.Fatal("could not find expand")
	}
	if !strings.Contains(body, "self.clientAddCallback(client)") {
		t.Error("admitting a client does not call the window's admission callback")
	}
	constructor, ok := functionBody(source, "func NewRemoteUserNatMultiClient(")
	if !ok {
		t.Fatal("could not find NewRemoteUserNatMultiClient")
	}
	if n := strings.Count(constructor, "multiClient.clientAdded,"); n != 2 {
		t.Errorf("the parent hands its admission callback to %d windows, want both", n)
	}
}
