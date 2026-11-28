package connect

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

// Tests for what the extender startup gate (E4) costs the calls that do not
// wait. The gate starts every dial and GET of a strategy with an extender
// directory, and almost every call finds no first sample pending.

// A call that does not wait costs nothing. The gate used to create its wait
// timer, and copy the custom extenders, before it knew whether it would wait.
func TestClientStrategyStartupGateAllocatesNothingWhenItDoesNotWait(t *testing.T) {
	if allocationsInFreshProcess(t) {
		return
	}
	clock := newTestClock()
	clientStrategy, directory, rootPrivateKey := newTestExtenderStrategy(t, clock, func(settings *ClientStrategySettings) {
		// long enough that a gate that waited would not return
		settings.ExtenderInitialSampleTimeout = time.Hour
	})
	ctx := context.Background()
	gate := func() {
		clientStrategy.waitForExtenderInitialSample(ctx)
	}

	// no network client has started a first sample
	if allocs := testing.AllocsPerRun(1000, gate); allocs != 0 {
		t.Errorf("gate with no sample pending allocated %g objects/run, want 0", allocs)
	}

	// a first sample is pending, but a stored record can already be dialed
	directory.SetInitialSamplePending()
	record := signTestRecord(
		t,
		rootPrivateKey,
		newTestExtenderKey(t),
		clock.Now(),
		clock.Now().Add(14*24*time.Hour),
		testExtenderAddress("192.0.2.110"),
	)
	if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	if allocs := testing.AllocsPerRun(1000, gate); allocs != 0 {
		t.Errorf("gate with a usable directory allocated %g objects/run, want 0", allocs)
	}

	// the first sample is done, as for every later call
	directory.SetInitialSampleDone()
	if allocs := testing.AllocsPerRun(1000, gate); allocs != 0 {
		t.Errorf("gate after the first sample allocated %g objects/run, want 0", allocs)
	}
}

// Custom extenders override discovery, so the gate never waits for a first
// sample while any are set, even with nothing in the directory to dial (E4).
func TestClientStrategyStartupGateSkipsWithCustomExtenders(t *testing.T) {
	clock := newTestClock()
	clientStrategy, directory, _ := newTestExtenderStrategy(t, clock, func(settings *ClientStrategySettings) {
		settings.ExtenderInitialSampleTimeout = time.Hour
	})
	directory.SetInitialSamplePending()
	clientStrategy.SetCustomExtenders(map[netip.Addr]string{
		netip.MustParseAddr("192.0.2.111"): "synthetic-secret",
	})
	requireStartupGateReturns(t, clientStrategy, "with custom extenders")
}
