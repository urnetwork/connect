package connect

import (
	"context"
	"net"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// The goroutines whose stack is inside a race completion.
func raceCompletionGoroutines() int {
	buf := make([]byte, 1024*1024)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	goroutines := 0
	for _, goroutine := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(goroutine, "(*RemoteUserNatMultiClient).scheduleCompleteRace") {
			goroutines += 1
		}
	}
	return goroutines
}

// A parent that resolves flows by path, as the race completion does.
func newRaceCompletionTestParent(ctx context.Context) *RemoteUserNatMultiClient {
	return &RemoteUserNatMultiClient{
		ctx:                ctx,
		settings:           DefaultMultiClientSettings(),
		log:                NewNoopLogger(),
		ip4PathUpdates:     map[Ip4Path]*multiClientChannelUpdate{},
		ip6PathUpdates:     map[Ip6Path]*multiClientChannelUpdate{},
		clientUpdates:      map[*multiClientChannel]map[*multiClientChannelUpdate]bool{},
		reliabilityMetrics: newReliabilityMetrics(),
	}
}

// A flow racing two candidates, neither of which has answered.
func startRaceCompletionTestRace(ctx context.Context, parent *RemoteUserNatMultiClient, sourcePort int) (*multiClientChannelUpdate, *multiClientChannelUpdateRace) {
	ipPath := &IpPath{
		Version:         4,
		Protocol:        IpProtocolTcp,
		SourceIp:        net.ParseIP("192.0.2.10"),
		SourcePort:      sourcePort,
		DestinationIp:   net.ParseIP("198.51.100.1"),
		DestinationPort: 443,
	}
	update := newMultiClientChannelUpdate(ctx, ipPath)
	func() {
		parent.stateLock.Lock()
		defer parent.stateLock.Unlock()
		parent.ip4PathUpdates[update.ipPath.ToIp4Path()] = update
	}()
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	update.initRaceWithLock()
	for range 2 {
		update.race.clientStates[stallTestChannel()] = &multiClientChannelRaceClientState{sendTime: time.Now()}
	}
	return update, update.race
}

// Whether the race's completion has judged it: with no answer to choose, it
// marks the response window elapsed.
func raceCompletionTestCompleted(update *multiClientChannelUpdate, race *multiClientChannelUpdateRace) bool {
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	return race.responseWindowElapsed
}

// A flow racing exits waits out its response window on a timer, not on a
// goroutine parked for the window. The completion still runs when the window
// ends, at once when enough candidates answered early, and never for a race
// that ended first.
func TestRaceCompletionParksNoGoroutine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		parent := newRaceCompletionTestParent(ctx)
		window := parent.settings.MultiRaceSetOnResponseTimeout

		before := raceCompletionGoroutines()
		raceCount := 8
		updates := []*multiClientChannelUpdate{}
		races := []*multiClientChannelUpdateRace{}
		for i := range raceCount {
			update, race := startRaceCompletionTestRace(ctx, parent, 40000+i)
			parent.scheduleCompleteRace(update.ipPath, race)
			updates = append(updates, update)
			races = append(races, race)
		}
		synctest.Wait()
		if n := raceCompletionGoroutines() - before; 0 < n {
			t.Fatalf("%d racing flows parked %d goroutines for their completion, want none", raceCount, n)
		}

		// enough candidates answered: the first race completes at once
		races[0].completeEarly()
		synctest.Wait()
		if !raceCompletionTestCompleted(updates[0], races[0]) {
			t.Fatal("an early completion did not complete its race at once")
		}
		// the second race ends before its window does
		func() {
			updates[1].stateLock.Lock()
			defer updates[1].stateLock.Unlock()
			updates[1].clearRaceWithLock()
		}()

		time.Sleep(window - time.Nanosecond)
		synctest.Wait()
		for i := 2; i < raceCount; i += 1 {
			if raceCompletionTestCompleted(updates[i], races[i]) {
				t.Fatalf("race %d completed before its %s response window", i, window)
			}
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		for i := 2; i < raceCount; i += 1 {
			if !raceCompletionTestCompleted(updates[i], races[i]) {
				t.Fatalf("race %d did not complete when its %s response window ended", i, window)
			}
		}
		if raceCompletionTestCompleted(updates[1], races[1]) {
			t.Fatal("a race that ended first was completed")
		}
		if n := raceCompletionGoroutines() - before; 0 < n {
			t.Fatalf("%d goroutines remain in race completion after every window ended", n)
		}
	})
}

// An early completion asked before the race's first send returned, which is
// when the window starts, completes the race as soon as it is scheduled.
func TestRaceCompletionHonorsAnEarlyCompletionBeforeScheduling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		parent := newRaceCompletionTestParent(ctx)
		update, race := startRaceCompletionTestRace(ctx, parent, 40000)
		race.completeEarly()
		parent.scheduleCompleteRace(update.ipPath, race)
		synctest.Wait()
		if !raceCompletionTestCompleted(update, race) {
			t.Fatal("an early completion asked before scheduling was lost")
		}
	})
}
