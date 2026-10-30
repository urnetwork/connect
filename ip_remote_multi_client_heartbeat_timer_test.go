package connect

import (
	"context"
	"testing"
	"time"
)

// A bare client with the reliability projection a constructed client installs.
func newHeartbeatTestMultiClient(ctx context.Context, cancel context.CancelFunc, log Logger) *RemoteUserNatMultiClient {
	settings := &MultiClientSettings{HeartbeatInterval: time.Millisecond}
	return &RemoteUserNatMultiClient{
		ctx:                        ctx,
		cancel:                     cancel,
		log:                        log,
		settings:                   settings,
		defaultReliabilitySettings: ReliabilitySettingsFrom(settings),
		windows:                    map[WindowType]*multiClientWindow{},
		clientUpdates:              map[*multiClientChannel]map[*multiClientChannelUpdate]bool{},
	}
}

// The heartbeat arms one timer for its life, not one per beat, and still
// narrates.
func TestHeartbeatLoopReusesOneTimer(t *testing.T) {
	log := newRecordingLogger()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mc := newHeartbeatTestMultiClient(ctx, cancel, log)

	timers := timerAllocationsUnder(t, ").runHeartbeat", func() {
		go mc.runHeartbeat()
		time.Sleep(100 * time.Millisecond)
	})
	if 4 < timers {
		t.Fatalf("the heartbeat allocated %d timer objects over ~100 beats, want one timer for its life", timers)
	}
	if got := len(log.linesWith("event=heartbeat")); got != 1 {
		t.Fatalf("the heartbeat logged %d lines, want the single baseline beat", got)
	}
}

// A client whose logger discards everything (a hosted device) does not build
// the heartbeat readouts the line would carry.
func TestHeartbeatSilentLoggerSkipsReadouts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mc := newHeartbeatTestMultiClient(ctx, cancel, NewNoopLogger())
	go mc.runHeartbeat()
	time.Sleep(10 * time.Millisecond)

	allocations := allocationsUnder(t, ").runHeartbeat", func() {
		time.Sleep(100 * time.Millisecond)
	})
	if 4 < allocations {
		t.Fatalf("a silent heartbeat allocated %d objects over ~100 beats, want none: it builds readouts nobody prints", allocations)
	}
}
