package connect

import (
	"context"
	"net"
	"sync"
	"testing"
	"testing/synctest"
)

func serviceFailureRetentionTestPath(version int) *IpPath {
	source, destination := "192.0.2.21", "203.0.113.41"
	if version == 6 {
		source, destination = "2001:db8:1::21", "2001:db8:2::41"
	}
	return &IpPath{
		Version:         version,
		Protocol:        IpProtocolTcp,
		SourceIp:        net.ParseIP(source),
		SourcePort:      49152,
		DestinationIp:   net.ParseIP(destination),
		DestinationPort: 443,
		Syn:             true,
		SequenceNumber:  8000,
	}
}

// A refused exit is already absent from clientUpdates. Removal must still
// release its service-memory keys, including on that early-return path.
func TestDestinationServiceFailureRemovalDropsRetiredChannelGeneration(t *testing.T) {
	for _, c := range []struct {
		version        int
		keepEmptyEntry bool
	}{
		{version: 4, keepEmptyEntry: false},
		{version: 6, keepEmptyEntry: false},
		{version: 4, keepEmptyEntry: true},
		{version: 6, keepEmptyEntry: true},
	} {
		path := serviceFailureRetentionTestPath(c.version)
		parent, exit, update, forwarded := dialFailureTestParent(t, true, path)
		parent.log = DefaultLogger()
		parent.settings.QuicRebindOnExitLoss = false
		exitCtx, cancelExit := context.WithCancel(parent.ctx)
		t.Cleanup(cancelExit)
		exit.ctx = exitCtx
		exit.cancel = cancelExit
		exit.log = DefaultLogger()
		update.noteConnectAttempt(path)
		if !parent.clientDialFailureSignal(exit, path) {
			t.Fatalf("v%d: setup failure did not unbind its exit", c.version)
		}
		key, ok := destinationServiceFailureKeyFor(path, exit)
		if !ok {
			t.Fatalf("v%d: setup service key is invalid", c.version)
		}
		if _, found := parent.destinationServiceFailures[key]; !found {
			t.Fatalf("v%d: actual live failure did not create service memory", c.version)
		}
		otherPath := *path
		otherPath.SourcePort++
		otherPath.DestinationPort = 8443
		otherUpdate := rebindTestFlow(parent, exit, &otherPath, false)
		otherUpdate.noteConnectAttempt(&otherPath)
		if !parent.clientDialFailureSignal(exit, &otherPath) {
			t.Fatalf("v%d: setup second live failure did not unbind its exit", c.version)
		}
		otherKey, otherKeyValid := destinationServiceFailureKeyFor(&otherPath, exit)
		if _, found := parent.destinationServiceFailures[otherKey]; !otherKeyValid || !found || key == otherKey {
			t.Fatalf("v%d: actual failures did not create two independent service entries", c.version)
		}
		if _, found := parent.clientUpdates[exit]; found || update.client.Load() != nil {
			t.Fatalf("v%d: setup failure retained a bound-flow entry", c.version)
		}
		if c.keepEmptyEntry {
			parent.clientUpdates[exit] = map[*multiClientChannelUpdate]bool{}
		}

		exit.Cancel()
		parent.removeClient(exit)
		for _, removedKey := range []destinationServiceFailureKey{key, otherKey} {
			if _, retained := parent.destinationServiceFailures[removedKey]; retained {
				t.Fatalf("SERVICE_FAILURE_REMOVED_GENERATION_RETAINED: v%d empty_entry=%t", c.version, c.keepEmptyEntry)
			}
		}
		if parent.ctx.Err() != nil || len(*forwarded) != 0 {
			t.Fatalf("v%d: removing an unbound exit affected its parent or delivered a teardown", c.version)
		}
		if got := parent.reliabilityMetrics.dialFailures.Load(); got != 2 {
			t.Errorf("v%d: intercepted failures = %d, want 2", c.version, got)
		}
		if got := parent.reliabilityMetrics.flowsReraced.Load(); got != 2 {
			t.Errorf("v%d: counted reraces = %d, want 2", c.version, got)
		}
	}
}

// The signal passes the real receive callback's live-context gate, then waits
// while removal or parent close completes. Removal preserves its normal race
// withdrawal; parent close leaves no flow on which the late signal can act.
func TestDestinationServiceFailureRemovedRaceCannotRecreateMemory(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	for _, closeParent := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			harness := newReraceHarness(t, []reraceExitSpec{
				{name: "a", answer: reraceAlways(reraceSilent)},
				{name: "b", answer: reraceAlways(reraceSilent)},
			}, nil)
			a, b := harness.channel("a"), harness.channel("b")
			path := serviceFailureRetentionTestPath(4)
			if !harness.syn(path) {
				t.Fatal("setup public SYN was refused")
			}
			update := harness.update(path)
			if update == nil {
				t.Fatal("setup public SYN has no current flow")
			}
			update.stateLock.Lock()
			wide := update.client.Load() == nil && update.race != nil && !update.race.single &&
				len(update.race.clientStates) == 2 && update.race.clientStates[a] != nil && update.race.clientStates[b] != nil
			update.stateLock.Unlock()
			if !wide || len(harness.exit("a").syns(path)) != 1 || len(harness.exit("b").syns(path)) != 1 {
				t.Fatal("setup did not admit the public SYN to both real provider candidates")
			}

			entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			previous := a.dialFailureCallback
			if previous == nil {
				t.Fatal("setup channel has no parent failure callback")
			}
			var releaseOnce sync.Once
			a.dialFailureCallback = func(source *multiClientChannel, egress *IpPath) bool {
				if source != a || !reraceSameFlow(egress, path) {
					return previous(source, egress)
				}
				close(entered)
				defer close(finished)
				<-release
				return previous(source, egress)
			}
			defer func() {
				releaseOnce.Do(func() { close(release) })
				synctest.Wait()
				a.dialFailureCallback = previous
			}()
			harness.inject("a", reraceProviderSignal(t, path))
			select {
			case <-entered:
			default:
				t.Fatal("real provider signal did not reach the held parent callback")
			}

			parent := harness.multiClient
			parent.stateLock.Lock()
			_, boundEntry := parent.clientUpdates[a]
			parent.stateLock.Unlock()
			if boundEntry {
				t.Fatal("setup wide-race candidate unexpectedly owns a bound-flow entry")
			}
			if closeParent {
				parent.Close()
			} else {
				a.Cancel()
				parent.removeClient(a)
			}
			if !a.IsDone() {
				t.Fatal("source channel did not finish cancellation before removal")
			}
			releaseOnce.Do(func() { close(release) })
			<-finished
			synctest.Wait()

			key, ok := destinationServiceFailureKeyFor(path, a)
			if !ok {
				t.Fatal("setup late signal has no service key")
			}
			parent.stateLock.Lock()
			_, retained := parent.destinationServiceFailures[key]
			parent.stateLock.Unlock()
			if retained {
				t.Fatal("SERVICE_FAILURE_LATE_CALLBACK_RETAINED_GENERATION: signal recreated memory after removal")
			}
			if closeParent {
				parent.stateLock.Lock()
				cacheEmpty := len(parent.destinationServiceFailures) == 0
				flowsEmpty := len(parent.ip4PathUpdates) == 0 && len(parent.ip6PathUpdates) == 0
				parent.stateLock.Unlock()
				if !cacheEmpty || !flowsEmpty || parent.ctx.Err() == nil || !b.IsDone() || harness.ctx.Err() != nil {
					t.Fatal("closed parent retained a late failure or changed its outer owner's lifetime")
				}
				if metrics := harness.metrics(); metrics.DialFailuresIntercepted != 1 || metrics.FlowsReraced != 0 {
					t.Fatalf("late closed-parent signal metrics = intercepted %d reraced %d, want 1 and 0", metrics.DialFailuresIntercepted, metrics.FlowsReraced)
				}
				if a.dialFailureCount() != 0 || len(harness.delivered(path)) != 0 {
					t.Fatal("late closed-parent signal acted on a retired flow or fabricated app delivery")
				}
				return
			}
			update.stateLock.Lock()
			withdrawn := update.client.Load() == nil && update.race != nil &&
				update.race.clientStates[a] == nil && update.race.clientStates[b] != nil
			update.stateLock.Unlock()
			if !withdrawn || b.IsDone() || harness.ctx.Err() != nil {
				t.Fatal("late signal changed normal withdrawal or the healthy sibling's lifetime")
			}
			if metrics := harness.metrics(); metrics.DialFailuresIntercepted != 1 || metrics.FlowsReraced != 0 {
				t.Fatalf("late wide-race signal metrics = intercepted %d reraced %d, want 1 and 0", metrics.DialFailuresIntercepted, metrics.FlowsReraced)
			}
			if got := a.dialFailureCount(); got != 1 {
				t.Fatalf("late wide-race signal strikes = %d, want unchanged policy's one strike", got)
			}
			if len(harness.delivered(path)) != 0 {
				t.Fatal("late signal fabricated an app response or teardown")
			}
			harness.inject("b", reraceSynAck(path))
			if _, answered := harness.synAck(path); !answered || harness.boundTo(path) != b {
				t.Fatal("remaining live provider did not complete the original flow")
			}
			reraceAssertNoErrorDelivered(t, harness, path)
		})
	}
}
