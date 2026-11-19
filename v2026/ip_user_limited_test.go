// A flow's activity stamp, which LRU eviction and the shared UDP idle reap
// read, is taken per packet without a lock.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// The stamp reads back as the time it was taken, so elapsed activity and the
// order of two flows' activity are exact.
func TestUserLimitedActivityStampTracksActivity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var earlier, later userLimited
		earlier.UpdateLastActivityTime()
		earlierAt := time.Now()
		time.Sleep(time.Second)
		later.UpdateLastActivityTime()
		laterAt := time.Now()
		time.Sleep(time.Second)

		if got := earlier.LastActivityTime(); !got.Equal(earlierAt) || time.Since(got) != 2*time.Second {
			t.Fatalf("earlier activity reads %s ago, want 2s", time.Since(got))
		}
		if got := later.LastActivityTime(); !got.Equal(laterAt) || time.Since(got) != time.Second {
			t.Fatalf("later activity reads %s ago, want 1s", time.Since(got))
		}
		if !earlier.LastActivityTime().Before(later.LastActivityTime()) {
			t.Fatal("earlier activity does not order before later activity")
		}
		earlier.UpdateLastActivityTime()
		if !later.LastActivityTime().Before(earlier.LastActivityTime()) || time.Since(earlier.LastActivityTime()) != 0 {
			t.Fatal("new activity did not move the stamp to now")
		}
	})
}

// One flow's reader and writer each stamp every packet.
func BenchmarkUserLimitedUpdateLastActivityTime(b *testing.B) {
	var activity userLimited
	for b.Loop() {
		activity.UpdateLastActivityTime()
	}
}

func BenchmarkUserLimitedUpdateLastActivityTimeParallel(b *testing.B) {
	var activity userLimited
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			activity.UpdateLastActivityTime()
		}
	})
}
