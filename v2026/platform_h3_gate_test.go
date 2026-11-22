package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// The bound on one step of the gate tests; never waited out on success.
const platformH3GateTestTimeout = 5 * time.Second

// A gate of the default mode preferences, and each mode whose waiter has
// seen the gate's state and waits for its next change.
func newPlatformH3GateTest() (*platformH3Gate, chan TransportMode) {
	transport := testingPlatformTransportModes()
	transport.settings = testingDefaultPlatformTransportSettings()
	transport.modePreferences = normalizeTransportModePreferences(nil)
	gate := newPlatformH3Gate(transport)
	waiting := make(chan TransportMode, 16)
	func() {
		gate.mutex.Lock()
		defer gate.mutex.Unlock()
		gate.waitingForTest = func(mode TransportMode) { waiting <- mode }
	}()
	return gate, waiting
}

// Acquires the gate for the mode on another goroutine; the channel carries
// the lease's release once it is acquired.
func acquirePlatformH3GateTest(ctx context.Context, gate *platformH3Gate, mode TransportMode) chan func() {
	acquired := make(chan func(), 1)
	go func() {
		_, _, release, ok := gate.Acquire(ctx, mode)
		if ok {
			acquired <- release
		}
	}()
	return acquired
}

func waitPlatformH3GateTest(t *testing.T, waiting chan TransportMode, mode TransportMode) {
	t.Helper()
	for {
		select {
		case waitingMode := <-waiting:
			if waitingMode == mode {
				return
			}
		case <-time.After(platformH3GateTestTimeout):
			t.Fatalf("no %s waiter came to wait", mode)
		}
	}
}

// A waiting better family ends a lower family's connection, once the lower
// owner reports itself connected, and acquires the gate only after the
// lower owner releases it.
func TestPlatformH3GatePreemptsLowerPriorityFamily(t *testing.T) {
	gate, waiting := newPlatformH3GateTest()
	dnsCtx, dnsConnected, releaseDNS, ok := gate.Acquire(t.Context(), TransportModeH3Dns)
	if !ok {
		t.Fatal("DNS family did not acquire an empty gate")
	}
	dnsConnected()
	directAcquired := acquirePlatformH3GateTest(t.Context(), gate, TransportModeH3)
	select {
	case <-dnsCtx.Done():
	case <-time.After(platformH3GateTestTimeout):
		t.Fatal("higher-priority direct H3 did not cancel the connected DNS carrier")
	}
	waitPlatformH3GateTest(t, waiting, TransportModeH3)
	select {
	case <-directAcquired:
		t.Fatal("direct H3 acquired before the canceled DNS owner released the socket")
	default:
	}
	releaseDNS()
	select {
	case releaseDirect := <-directAcquired:
		releaseDirect()
	case <-time.After(platformH3GateTestTimeout):
		t.Fatal("direct H3 did not acquire after DNS released the gate")
	}
}

// A waiting better family never ends a lower family's dial: the DNS owner
// keeps its context through the handshake while direct H3 waits, and is
// ended only once it reports itself connected.
func TestPlatformH3GateKeepsALowerFamilyDial(t *testing.T) {
	gate, waiting := newPlatformH3GateTest()
	dnsCtx, dnsConnected, releaseDNS, ok := gate.Acquire(t.Context(), TransportModeH3Dns)
	if !ok {
		t.Fatal("DNS family did not acquire an empty gate")
	}
	directAcquired := acquirePlatformH3GateTest(t.Context(), gate, TransportModeH3)
	// direct H3 has seen the DNS owner dialing and waits
	waitPlatformH3GateTest(t, waiting, TransportModeH3)
	if dnsCtx.Err() != nil {
		t.Fatal("a waiting better family ended the DNS carrier's dial")
	}
	select {
	case <-directAcquired:
		t.Fatal("direct H3 acquired the gate from a dialing DNS owner")
	default:
	}

	dnsConnected()
	select {
	case <-dnsCtx.Done():
	case <-time.After(platformH3GateTestTimeout):
		t.Fatal("direct H3 did not end the DNS carrier once it connected")
	}
	releaseDNS()
	select {
	case releaseDirect := <-directAcquired:
		releaseDirect()
	case <-time.After(platformH3GateTestTimeout):
		t.Fatal("direct H3 did not acquire after DNS released the gate")
	}
}

// A lower family's dial that fails hands the gate to the waiting better
// family without ever being ended by it, and a released owner's late
// `connected` does not expose the next owner's dial.
func TestPlatformH3GateHandsAFailedLowerDialOn(t *testing.T) {
	gate, waiting := newPlatformH3GateTest()
	dnsCtx, dnsConnected, releaseDNS, ok := gate.Acquire(t.Context(), TransportModeH3Dns)
	if !ok {
		t.Fatal("DNS family did not acquire an empty gate")
	}
	directAcquired := acquirePlatformH3GateTest(t.Context(), gate, TransportModeH3)
	waitPlatformH3GateTest(t, waiting, TransportModeH3)
	if dnsCtx.Err() != nil {
		t.Fatal("the DNS dial was ended before it failed")
	}
	// the dial fails: the owner releases the gate without connecting
	releaseDNS()
	select {
	case releaseDirect := <-directAcquired:
		releaseDirect()
	case <-time.After(platformH3GateTestTimeout):
		t.Fatal("direct H3 did not acquire after the failed DNS dial")
	}

	pumpCtx, _, releasePump, ok := gate.Acquire(t.Context(), TransportModeH3DnsPump)
	if !ok {
		t.Fatal("the pump family did not acquire the free gate")
	}
	defer releasePump()
	// the released DNS owner's late report changes nothing: a better family
	// still waits on the pump's dial
	dnsConnected()
	dnsWaitCtx, dnsWaitCancel := context.WithCancel(t.Context())
	defer dnsWaitCancel()
	dnsAcquired := acquirePlatformH3GateTest(dnsWaitCtx, gate, TransportModeH3Dns)
	waitPlatformH3GateTest(t, waiting, TransportModeH3Dns)
	if pumpCtx.Err() != nil {
		t.Fatal("a stale connected report exposed the pump's dial")
	}
	select {
	case <-dnsAcquired:
		t.Fatal("the DNS family acquired the gate from a dialing pump owner")
	default:
	}
}

// A dns carrier's dial that keeps hearing from alt keeps the gate from a
// waiting better family however long it takes; once it has heard nothing for
// the stall timeout, the better family ends it, at that instant. A direct H3
// lease carries no clock and never stalls.
func TestPlatformH3GateEndsAStalledLowerFamilyDial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate, waiting := newPlatformH3GateTest()
		stallTimeout := gate.owner.settings.H3DnsRaceStallTimeout
		dnsCtx, _, releaseDNS, ok := gate.Acquire(t.Context(), TransportModeH3Dns)
		if !ok {
			t.Fatal("DNS family did not acquire an empty gate")
		}
		clock := quicDialReceiveClockFromContext(dnsCtx)
		if clock == nil {
			t.Fatal("the dns carrier's lease carries no receive clock")
		}
		directAcquired := acquirePlatformH3GateTest(t.Context(), gate, TransportModeH3)
		waitPlatformH3GateTest(t, waiting, TransportModeH3)

		// alt answers every second for ten seconds, five stall timeouts
		for range 10 {
			time.Sleep(stallTimeout / 2)
			clock.received()
		}
		synctest.Wait()
		if dnsCtx.Err() != nil {
			t.Fatal("a better family ended a dial that was hearing from alt")
		}
		lastHeard := time.Now()

		// then nothing more: the dial is ended once it has stalled
		<-dnsCtx.Done()
		if stalledFor := time.Since(lastHeard); stalledFor != stallTimeout {
			t.Fatalf("the stalled dial was ended %s after alt last answered, want %s", stalledFor, stallTimeout)
		}
		releaseDNS()
		var releaseDirect func()
		select {
		case releaseDirect = <-directAcquired:
		case <-time.After(platformH3GateTestTimeout):
			t.Fatal("direct H3 did not acquire after the stalled dial released the gate")
		}
		directCtx, _, _, _ := func() (context.Context, func(), func(), bool) {
			releaseDirect()
			return gate.Acquire(t.Context(), TransportModeH3)
		}()
		if quicDialReceiveClockFromContext(directCtx) != nil {
			t.Fatal("a direct H3 lease carries a receive clock")
		}
	})
}

func TestPlatformH3GateAllowsOnlyOneFamily(t *testing.T) {
	transport := testingPlatformTransportModes()
	transport.settings = testingDefaultPlatformTransportSettings()
	transport.modePreferences = normalizeTransportModePreferences(map[TransportMode]int{
		TransportModeH3Dns:     2,
		TransportModeH3DnsPump: 2,
	})
	gate := newPlatformH3Gate(transport)

	_, dnsConnected, releaseDNS, ok := gate.Acquire(t.Context(), TransportModeH3Dns)
	if !ok {
		t.Fatal("first H3 family did not acquire")
	}
	dnsConnected()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pumpAcquired := make(chan bool, 1)
	go func() {
		_, _, release, acquired := gate.Acquire(ctx, TransportModeH3DnsPump)
		if acquired {
			release()
		}
		pumpAcquired <- acquired
	}()
	select {
	case <-pumpAcquired:
		t.Fatal("equal-priority H3 family overlapped the active carrier")
	case <-time.After(25 * time.Millisecond):
	}
	releaseDNS()
	select {
	case acquired := <-pumpAcquired:
		if !acquired {
			t.Fatal("waiting H3 family did not acquire after release")
		}
	case <-time.After(time.Second):
		t.Fatal("waiting H3 family was not woken")
	}
}
