// The DNS translation's expiry timer never moves a pending timer, so its
// per-packet re-arming doesn't make the runtime walk a P's timer heap
// (server LBALTOPT2 §3.4), and it never expires late.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

func TestDnsExpiryTimerKeepsAPendingDeadlineAndNeverFiresLate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		timer := newDnsExpiryTimer()
		defer timer.stop()

		start := time.Now()
		expiry := timer.armBy(start.Add(5 * time.Second))
		// a later deadline, as each packet's younger oldest entry gives,
		// leaves the pending timer where it is
		if again := timer.armBy(start.Add(7 * time.Second)); again != expiry {
			t.Fatal("re-arming changed the channel")
		}
		if timer.deadline != start.Add(5*time.Second) {
			t.Fatalf("a later deadline moved the pending timer to %s", timer.deadline.Sub(start))
		}
		<-expiry
		if fired := time.Since(start); fired != 5*time.Second {
			t.Fatalf("the expiry fired after %s, want 5s", fired)
		}
		timer.fired()

		// after firing, the next deadline arms it again
		rearmed := time.Now()
		expiry = timer.armBy(rearmed.Add(2 * time.Second))
		<-expiry
		if fired := time.Since(rearmed); fired != 2*time.Second {
			t.Fatalf("the re-armed expiry fired after %s, want 2s", fired)
		}
		timer.fired()

		// an earlier deadline than the pending one moves it, so nothing
		// expires late
		moved := time.Now()
		expiry = timer.armBy(moved.Add(10 * time.Second))
		expiry = timer.armBy(moved.Add(3 * time.Second))
		<-expiry
		if fired := time.Since(moved); fired != 3*time.Second {
			t.Fatalf("the moved expiry fired after %s, want 3s", fired)
		}
		timer.fired()

		// no stale fire follows
		time.Sleep(time.Minute)
		select {
		case <-expiry:
			t.Fatal("a stale expiry fired")
		default:
		}
	})
}
