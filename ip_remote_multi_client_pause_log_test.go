// Pause logging must not delay recovery, retain canceled owners, or create an
// unbounded worker backlog. Barriers pin each external-sink failure boundary.
package connect

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// A held external sink must never become part of pause detection or hold
// publication. The barrier makes the bad ordering deterministic.
type schedulerPauseBlockingTestLogger struct {
	Logger
	once         sync.Once
	entered      chan struct{}
	release      chan struct{}
	match        string
	onEntry      func()
	stateLock    sync.Mutex
	enteredCount int
	lines        []string
}

// Creates a sink whose matching records wait at an explicit release barrier.
func newSchedulerPauseBlockingTestLogger() *schedulerPauseBlockingTestLogger {
	return &schedulerPauseBlockingTestLogger{
		Logger:  NewNoopLogger(),
		entered: make(chan struct{}),
		release: make(chan struct{}),
		match:   "event=scheduler_pause",
	}
}

// Blocks only the selected event and records each completed logger call.
func (self *schedulerPauseBlockingTestLogger) Infof(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if !strings.Contains(line, self.match) {
		return
	}
	self.once.Do(func() {
		if self.onEntry != nil {
			self.onEntry()
		}
		close(self.entered)
	})
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.enteredCount += 1
	}()
	<-self.release
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines = append(self.lines, line)
}

// Copies completed records without sharing the logger's mutable slice.
func (self *schedulerPauseBlockingTestLogger) loggedLines() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.lines)
}

// Both clients must receive their own recovery windows before an external
// logger is entered, and another pause must still extend both windows while
// that logger remains held.
func TestSharedSchedulerPauseLoggingDoesNotDelayHolds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		detector := NewSchedulerPauseDetector(ctx)
		defer detector.Close()
		logger := newSchedulerPauseBlockingTestLogger()
		clients := make([]*RemoteUserNatMultiClient, 2)
		for i := range clients {
			settings := DefaultMultiClientSettings()
			settings.SchedulerPauseRecoveryTimeout += time.Duration(i) * time.Second
			clients[i] = &RemoteUserNatMultiClient{ctx: ctx, settings: settings, log: logger}
			detector.subscribe(clients[i])
		}
		entryHolds := make([]time.Time, len(clients))
		logger.onEntry = func() {
			for i, client := range clients {
				entryHolds[i] = schedulerPauseHoldUntil(client)
			}
		}

		now := time.Now()
		done := make(chan struct{})
		go func() {
			defer close(done)
			detector.observeSchedulerPause(nil, now.Add(-30*time.Second), now)
		}()
		<-logger.entered
		synctest.Wait()
		for i, client := range clients {
			want := now.Add(client.settings.SchedulerPauseRecoveryTimeout)
			if !entryHolds[i].Equal(want) {
				t.Errorf("client %d hold at logger entry = %v, want %v", i, entryHolds[i], want)
			}
			if hold := schedulerPauseHoldUntil(client); !hold.Equal(want) {
				t.Errorf("client %d hold = %v, want %v while logger is held", i, hold, want)
			}
		}
		select {
		case <-done:
		default:
			close(logger.release)
			<-done
			t.Fatal("the external logger blocked the detector pass")
		}

		time.Sleep(10 * time.Second)
		now = time.Now()
		detector.observeSchedulerPause(nil, now.Add(-30*time.Second), now)
		for i, client := range clients {
			want := now.Add(client.settings.SchedulerPauseRecoveryTimeout)
			if hold := schedulerPauseHoldUntil(client); !hold.Equal(want) {
				t.Errorf("client %d second hold = %v, want %v while logger is held", i, hold, want)
			}
		}
		close(logger.release)
		synctest.Wait()
	})
}

// A blocked logger permits one in-flight record and one pending record per
// live registration, regardless of how many pauses occur. Coalescing preserves
// the exact number of pauses when the sink recovers.
func TestSharedSchedulerPauseLogsCoalesceWithoutLosingPauses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		detector := NewSchedulerPauseDetector(ctx)
		defer detector.Close()
		logger := newSchedulerPauseBlockingTestLogger()
		for range 2 {
			detector.subscribe(&RemoteUserNatMultiClient{ctx: ctx, settings: DefaultMultiClientSettings(), log: logger})
		}
		now := time.Now()
		detector.observeSchedulerPause(nil, now.Add(-30*time.Second), now)
		<-logger.entered
		for range 20 {
			detector.observeSchedulerPause(nil, now.Add(-30*time.Second), now)
		}
		synctest.Wait()
		func() {
			detector.logs.stateLock.Lock()
			defer detector.logs.stateLock.Unlock()
			pendingCount := 0
			for slot := detector.logs.head; slot != nil; slot = slot.next {
				pendingCount += 1
			}
			if pendingCount != 2 {
				t.Errorf("%d pending records for two clients, want exactly two", pendingCount)
			}
		}()
		func() {
			logger.stateLock.Lock()
			defer logger.stateLock.Unlock()
			if logger.enteredCount != 1 {
				t.Errorf("%d concurrent logger calls, want one worker", logger.enteredCount)
			}
		}()
		close(logger.release)
		synctest.Wait()
		lines := logger.loggedLines()
		if len(lines) != 3 {
			t.Fatalf("%d log lines, want the in-flight record plus two pending records", len(lines))
		}
		pauseCount := uint64(len(lines))
		for _, line := range lines {
			for _, field := range strings.Fields(line) {
				if count, ok := strings.CutPrefix(field, "coalesced="); ok {
					coalesced, err := strconv.ParseUint(count, 10, 64)
					if err != nil {
						t.Fatal(err)
					}
					pauseCount += coalesced
				}
			}
		}
		if pauseCount != 42 {
			t.Fatalf("logs account for %d client pauses, want 42", pauseCount)
		}
	})
}

// The real constructor's cancellation subscription removes pending logs
// without joining a blocked sink or stopping healthy siblings' recovery.
func TestSharedSchedulerPauseCanceledClientReleasesPendingLog(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		detector := NewSchedulerPauseDetector(ctx)
		defer detector.Close()
		logger := newSchedulerPauseBlockingTestLogger()
		retiring := newPauseDetectorTestMultiClient(detector, func(settings *MultiClientSettings) {
			settings.Log = logger
		})
		sibling := newPauseDetectorTestMultiClient(detector, func(settings *MultiClientSettings) {
			settings.Log = NewNoopLogger()
		})
		defer closePauseDetectorTestMultiClient(t, sibling)
		slot := detector.clients[retiring]
		now := time.Now()
		detector.observeSchedulerPause(nil, now.Add(-30*time.Second), now)
		<-logger.entered
		detector.observeSchedulerPause(nil, now.Add(-30*time.Second), now)
		closePauseDetectorTestMultiClient(t, retiring)
		synctest.Wait()
		if detector.ClientCount() != 1 || detector.ctx.Err() != nil || sibling.ctx.Err() != nil {
			t.Error("closing one client stopped shared detection or the healthy sibling")
		}
		func() {
			detector.logs.stateLock.Lock()
			defer detector.logs.stateLock.Unlock()
			if slot.active || slot.pending || slot.log != nil || slot.event != (schedulerPauseLogEvent{}) {
				t.Error("the canceled client retained a pending log or logger")
			}
		}()
		time.Sleep(10 * time.Second)
		now = time.Now()
		detector.observeSchedulerPause(nil, now.Add(-30*time.Second), now)
		if hold := schedulerPauseHoldUntil(sibling); !hold.Equal(now.Add(sibling.settings.SchedulerPauseRecoveryTimeout)) {
			t.Errorf("healthy sibling did not receive its next recovery hold: %v", hold)
		}
		close(logger.release)
		synctest.Wait()
		if lines := logger.loggedLines(); len(lines) != 1 {
			t.Errorf("the retired client logged %d records, want only the already-entered one", len(lines))
		}
	})
}

// Parent cancellation releases the registry, queue and reusable pass buffer
// while exactly one external log call remains in flight. It is joined only
// after the test releases the sink, never by detector Close.
func TestSharedSchedulerPauseCancellationReleasesQueuedOwners(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		detector := NewSchedulerPauseDetector(ctx)
		defer detector.Close()
		logger := newSchedulerPauseBlockingTestLogger()
		var slots []*schedulerPauseLogSlot
		for range 3 {
			client := &RemoteUserNatMultiClient{ctx: ctx, settings: DefaultMultiClientSettings(), log: logger}
			detector.subscribe(client)
			slots = append(slots, detector.clients[client])
		}
		now := time.Now()
		observations := detector.observeSchedulerPause(nil, now.Add(-30*time.Second), now)
		<-logger.entered
		for _, observation := range observations {
			if observation.client != nil || observation.slot != nil || observation.event != (schedulerPauseLogEvent{}) {
				t.Error("the reusable pass buffer retained an owner")
			}
		}
		cancel()
		synctest.Wait()
		select {
		case <-detector.done:
		default:
			t.Error("a blocked sink delayed timer cancellation")
		}
		if detector.ClientCount() != 0 {
			t.Error("cancellation retained client registrations")
		}
		func() {
			detector.logs.stateLock.Lock()
			defer detector.logs.stateLock.Unlock()
			if !detector.logs.closed || detector.logs.head != nil || detector.logs.tail != nil {
				t.Error("cancellation retained the pending log queue")
			}
			for _, slot := range slots {
				if slot.active || slot.pending || slot.log != nil || slot.event != (schedulerPauseLogEvent{}) {
					t.Error("cancellation retained a pending log or logger")
				}
			}
		}()
		detector.subscribe(&RemoteUserNatMultiClient{ctx: context.Background(), settings: DefaultMultiClientSettings(), log: logger})
		if detector.ClientCount() != 0 {
			t.Error("a canceled detector admitted a new client")
		}
		close(logger.release)
		<-detector.logs.done
		if lines := logger.loggedLines(); len(lines) != 1 {
			t.Errorf("cancellation emitted %d records, want only the in-flight record", len(lines))
		}
	})
}

// A detector's copied cadence governs its expected wait, while tolerances
// and recovery windows remain per client and can change at runtime.
func TestSharedSchedulerPauseSettingsAndClientOverrides(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		detectorSettings := DefaultSchedulerPauseDetectorSettings()
		detectorSettings.ProbeInterval = 5 * time.Second
		detector := NewSchedulerPauseDetectorWithSettings(ctx, detectorSettings)
		defer detector.Close()
		detectorSettings.ProbeInterval = time.Hour
		first := &RemoteUserNatMultiClient{ctx: ctx, settings: DefaultMultiClientSettings(), log: NewNoopLogger()}
		second := &RemoteUserNatMultiClient{ctx: ctx, settings: DefaultMultiClientSettings(), log: NewNoopLogger()}
		second.settings.SchedulerPauseTolerance = 40 * time.Second
		detector.subscribe(first)
		detector.subscribe(second)
		now := time.Now()
		detector.observeSchedulerPause(nil, now.Add(-6*time.Second), now)
		if !schedulerPauseHoldUntil(first).IsZero() || !schedulerPauseHoldUntil(second).IsZero() {
			t.Fatal("the shared detector ignored its configured expected interval")
		}
		detector.observeSchedulerPause(nil, now.Add(-8*time.Second), now)
		if !schedulerPauseHoldUntil(first).Equal(now.Add(5*time.Second)) || !schedulerPauseHoldUntil(second).IsZero() {
			t.Fatal("the shared detector did not apply each client's tolerance")
		}
		override := ReliabilitySettingsFrom(second.settings)
		override.SchedulerPauseTolerance = time.Second
		override.SchedulerPauseRecoveryTimeout = 9 * time.Second
		second.reliability.Store(override)
		detector.observeSchedulerPause(nil, now.Add(-8*time.Second), now)
		if hold := schedulerPauseHoldUntil(second); !hold.Equal(now.Add(9 * time.Second)) {
			t.Fatalf("runtime recovery hold = %v, want %v", hold, now.Add(9*time.Second))
		}
	})
}

// A logger reached from the uplink gate must not retain the leaf lock needed
// by the shared pause detector. Cover all three transition log sites.
func TestSharedSchedulerPauseUplinkLogsDoNotHoldRecoveryLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, state := range []string{"stale", "cap", "fresh"} {
			ctx, cancel := context.WithCancel(context.Background())
			detector := NewSchedulerPauseDetector(ctx)
			logger := newSchedulerPauseBlockingTestLogger()
			logger.match = "event=uplink state=" + state
			client := uplinkGateTestParent(2)
			client.ctx = ctx
			client.log = logger
			sibling := &RemoteUserNatMultiClient{ctx: ctx, settings: DefaultMultiClientSettings(), log: NewNoopLogger()}
			detector.subscribe(client)
			detector.subscribe(sibling)
			now := time.Now()
			client.uplinkLastIngressNanos.Store(now.Add(-6 * time.Second).UnixNano())
			if state == "cap" {
				client.uplinkStaleSince = now.Add(-2 * time.Minute)
			} else if state == "fresh" {
				client.uplinkStaleSince = now.Add(-20 * time.Second)
				client.uplinkLastIngressNanos.Store(now.UnixNano())
			}
			gateDone := make(chan bool, 1)
			go func() {
				stale, _ := client.uplinkGate(now)
				gateDone <- stale
			}()
			<-logger.entered
			// The barrier proves the sink is already entered; TryLock proves
			// ownership directly without a scheduler-dependent negative wait.
			if !client.uplinkStateLock.TryLock() {
				t.Errorf("%s log held the recovery lock and stopped shared detection", state)
				close(logger.release)
				<-gateDone
				cancel()
				detector.Close()
				continue
			}
			client.uplinkStateLock.Unlock()
			detector.observeSchedulerPause(nil, now.Add(-30*time.Second), now)
			for _, observed := range []*RemoteUserNatMultiClient{client, sibling} {
				if hold := schedulerPauseHoldUntil(observed); !hold.Equal(now.Add(observed.settings.SchedulerPauseRecoveryTimeout)) {
					t.Errorf("%s log held: recovery window = %v", state, hold)
				}
			}
			close(logger.release)
			if stale := <-gateDone; stale != (state == "stale") {
				t.Errorf("%s transition changed its verdict result to %t", state, stale)
			}
			cancel()
			detector.Close()
		}
	})
}

// The migrated hold cap keeps its shipped default and remains independently
// tunable without disabling the scheduler-pause recovery window.
func TestSchedulerPauseUplinkHoldCapSettings(t *testing.T) {
	client := uplinkGateTestParent(2)
	client.log = NewNoopLogger()
	if client.settings.UplinkStalenessMaxHold != time.Minute {
		t.Fatalf("default uplink hold cap = %s, want 1m", client.settings.UplinkStalenessMaxHold)
	}
	override := ReliabilitySettingsFrom(client.settings)
	if override.UplinkStalenessMaxHold != time.Minute {
		t.Fatal("the runtime projection lost the default hold cap")
	}
	override.UplinkStalenessMaxHold = 7 * time.Second
	client.reliability.Store(override)
	now := time.Now()
	client.uplinkLastIngressNanos.Store(now.Add(-6 * time.Second).UnixNano())
	if stale, _ := client.uplinkGate(now); !stale {
		t.Fatal("the configured uplink hold did not begin")
	}
	if stale, freshSince := client.uplinkGate(now.Add(8 * time.Second)); stale || !freshSince.IsZero() {
		t.Fatalf("the configured cap did not release verdicts without rebasing: stale=%t fresh=%v", stale, freshSince)
	}
	for _, cap := range []time.Duration{0, -time.Second} {
		overrideCopy := *override
		overrideCopy.UplinkStalenessMaxHold = cap
		client.reliability.Store(&overrideCopy)
		client.notifySchedulerPause(30 * time.Second)
		if stale, _ := client.uplinkGate(time.Now()); !stale {
			t.Fatalf("uplink cap %s canceled the independent pause hold", cap)
		}
	}
}

// A non-positive probe interval disables sampling instead of creating a hot
// timer loop. The default constructor remains the one-second compatibility
// path, independent of caller mutation of a settings value.
func TestSchedulerPauseDetectorSettingsDisableProbes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, interval := range []time.Duration{0, -time.Second} {
			ctx, cancel := context.WithCancel(context.Background())
			settings := DefaultSchedulerPauseDetectorSettings()
			settings.ProbeInterval = interval
			detector := NewSchedulerPauseDetectorWithSettings(ctx, settings)
			client := &RemoteUserNatMultiClient{ctx: ctx, settings: DefaultMultiClientSettings(), log: NewNoopLogger()}
			detector.subscribe(client)
			now := time.Now()
			detector.observeSchedulerPause(nil, now.Add(-time.Hour), now)
			time.Sleep(time.Minute)
			if !schedulerPauseHoldUntil(client).IsZero() {
				t.Errorf("probe interval %s did not disable detection", interval)
			}
			cancel()
			<-detector.done
			<-detector.logs.done
		}
	})
}
