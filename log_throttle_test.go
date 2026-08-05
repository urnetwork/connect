package connect

import (
	"testing"
	"time"
)

func TestLogThrottle_FirstCallAllowedZeroSuppressed(t *testing.T) {
	th := newLogThrottle(time.Minute)
	ok, suppressed := th.Allow(time.Unix(100, 0))
	if !ok {
		t.Fatal("first call should be allowed")
	}
	if suppressed != 0 {
		t.Fatalf("first call should report 0 suppressed, got %d", suppressed)
	}
}

func TestLogThrottle_SuppressesWithinIntervalThenReportsCount(t *testing.T) {
	th := newLogThrottle(time.Minute)
	base := time.Unix(1000, 0)

	if ok, _ := th.Allow(base); !ok {
		t.Fatal("first call should be allowed")
	}
	for i := 1; i <= 3; i++ {
		if ok, _ := th.Allow(base.Add(time.Duration(i) * time.Second)); ok {
			t.Fatalf("call %ds in (within interval) should be suppressed", i)
		}
	}

	ok, suppressed := th.Allow(base.Add(2 * time.Minute))
	if !ok {
		t.Fatal("call after the interval elapsed should be allowed")
	}
	if suppressed != 3 {
		t.Fatalf("expected 3 suppressed since the last allowed line, got %d", suppressed)
	}
}

func TestLogThrottle_SuppressedCountResetsAfterEmit(t *testing.T) {
	th := newLogThrottle(time.Minute)
	base := time.Unix(1000, 0)

	th.Allow(base)                      // allowed
	th.Allow(base.Add(1 * time.Second)) // suppressed -> count 1
	th.Allow(base.Add(2 * time.Minute)) // allowed, reports & resets count

	// A subsequent allowed line with no suppression in between reports 0.
	ok, suppressed := th.Allow(base.Add(4 * time.Minute))
	if !ok || suppressed != 0 {
		t.Fatalf("expected allowed with 0 suppressed, got ok=%v suppressed=%d", ok, suppressed)
	}
}

// A real outage drives every sequence at the fault simultaneously. Exactly one
// caller may emit per interval; the rest must be counted, not lost.
func TestLogThrottle_ConcurrentCallersEmitOncePerInterval(t *testing.T) {
	th := newLogThrottle(time.Minute)
	now := time.Unix(2000, 0)

	const callers = 64
	allowed := make(chan int64, callers)
	start := make(chan struct{})
	done := make(chan struct{})

	for i := 0; i < callers; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			<-start
			if ok, suppressed := th.Allow(now); ok {
				allowed <- suppressed
			}
		}()
	}
	close(start)
	for i := 0; i < callers; i++ {
		<-done
	}
	close(allowed)

	emitted := 0
	for range allowed {
		emitted++
	}
	if emitted != 1 {
		t.Fatalf("expected exactly 1 emission across %d concurrent callers, got %d", callers, emitted)
	}

	// The other 63 were suppressed, and the next allowed line must report them.
	ok, suppressed := th.Allow(now.Add(2 * time.Minute))
	if !ok {
		t.Fatal("call after the interval elapsed should be allowed")
	}
	if suppressed != callers-1 {
		t.Fatalf("expected %d suppressed, got %d", callers-1, suppressed)
	}
}
