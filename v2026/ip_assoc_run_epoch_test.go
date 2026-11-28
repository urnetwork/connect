package connect

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func ipAssocRunEpochTestSettings() *IpAssocSettings {
	settings := DefaultIpAssocSettings()
	settings.ActivityGranule = time.Millisecond
	settings.PacketAssociationTime = 100 * time.Millisecond
	settings.ClusterEpoch = 2 * time.Millisecond
	return settings
}

func feedIpAssocPair(ipAssoc *IpAssoc, a string, b string) {
	for range 8 {
		ipAssoc.AddEgressPacket(testingIpAssocPath(a))
		ipAssoc.AddEgressPacket(testingIpAssocPath(b))
		time.Sleep(2 * time.Millisecond)
	}
}

func waitForIpAssocCluster(t *testing.T, ipAssoc *IpAssoc, addr string, message string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(ipAssoc.GetCluster(testingIpAssocPath(addr))) != 2 {
		if deadline.Before(time.Now()) {
			t.Fatal(message)
		}
		time.Sleep(time.Millisecond)
	}
}

// The clustering loop arms one timer for its life, not one per epoch.
func TestIpAssocRunReusesOneTimer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ipAssoc := NewIpAssoc(ctx, ipAssocRunEpochTestSettings())
	defer ipAssoc.Close()

	timers := timerAllocationsUnder(t, "(*IpAssoc).run", func() {
		// new activity every epoch
		for range 12 {
			feedIpAssocPair(ipAssoc, "1.0.0.1", "1.0.0.2")
		}
	})
	if 4 < timers {
		t.Fatalf("the clustering loop allocated %d timer objects over ~100 epochs, want one timer for its life", timers)
	}
}

// With no new activity the loop parks instead of waking every epoch; new
// activity wakes it and is clustered.
func TestIpAssocRunParksWithoutActivity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ipAssoc := NewIpAssoc(ctx, ipAssocRunEpochTestSettings())
	defer ipAssoc.Close()
	wakes := atomic.Int64{}
	countWake := func() {
		wakes.Add(1)
	}
	ipAssoc.epochWakeForTest.Store(&countWake)

	feedIpAssocPair(ipAssoc, "1.0.0.1", "1.0.0.2")
	waitForIpAssocCluster(t, ipAssoc, "1.0.0.1", "the active pair did not cluster")
	time.Sleep(20 * time.Millisecond)

	before := wakes.Load()
	time.Sleep(100 * time.Millisecond)
	if idleWakes := wakes.Load() - before; 2 < idleWakes {
		t.Fatalf("the clustering loop woke %d times over 100ms with no activity at a 2ms epoch, want it parked", idleWakes)
	}

	// activity after the park still clusters
	feedIpAssocPair(ipAssoc, "1.0.0.3", "1.0.0.4")
	waitForIpAssocCluster(t, ipAssoc, "1.0.0.3", "activity after the park did not cluster")
}
