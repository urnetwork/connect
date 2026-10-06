// Fake-time regressions preserve bounded live window accounting and bucket ownership.
package connect

import (
	"net"
	"testing"
	"testing/synctest"
	"time"
)

// Bare channels exercise the shipping accounting without transport workers.
func newChannelWindowStatsFixture() (*multiClientChannel, []*IpPath) {
	settings := DefaultMultiClientSettings()
	settings.StatsWindowDuration = time.Second
	settings.StatsWindowBucketDuration = 100 * time.Millisecond
	channel := &multiClientChannel{
		settings:                  settings,
		packetStats:               &clientWindowStats{},
		ip4DestinationSourceCount: map[Ip4Path]map[Ip4Path]int{},
		ip6DestinationSourceCount: map[Ip6Path]map[Ip6Path]int{},
	}
	paths := []*IpPath{
		{Version: 4, Protocol: IpProtocolUdp, SourceIp: net.ParseIP("192.0.2.1"),
			SourcePort: 12345, DestinationIp: net.ParseIP("198.51.100.1"), DestinationPort: 443},
		{Version: 6, Protocol: IpProtocolUdp, SourceIp: net.ParseIP("2001:db8::1"),
			SourcePort: 12345, DestinationIp: net.ParseIP("2001:db8::2"), DestinationPort: 443},
	}
	return channel, paths
}

// Every bucket owns one packet and one reference to each synthetic path.
func addChannelWindowStatsPacket(channel *multiClientChannel, paths []*IpPath) {
	channel.addSendNack(1)
	channel.addSendAck(1)
	for _, path := range paths {
		channel.addSource(path)
	}
}

// Fake time leaves each completed bucket's last event near its end.
func seedChannelWindowStats(channel *multiClientChannel, paths []*IpPath, count int) {
	for i := 0; i < count; i++ {
		addChannelWindowStatsPacket(channel, paths)
		if i < count-1 {
			time.Sleep(99 * time.Millisecond)
			func() {
				channel.stateLock.Lock()
				defer channel.stateLock.Unlock()
				channel.eventBucket()
			}()
			time.Sleep(2 * time.Millisecond)
		}
	}
}

// Counts and source references must describe the same retained owners.
func requireChannelWindowStats(t *testing.T, channel *multiClientChannel, paths []*IpPath, wantCount int) {
	t.Helper()
	if len(channel.eventBuckets) != wantCount || channel.packetStats.sendAckCount != wantCount ||
		channel.packetStats.sendAckByteCount != ByteCount(wantCount) {
		t.Fatalf("retained accounting: buckets=%d acks=%d bytes=%d want=%d",
			len(channel.eventBuckets), channel.packetStats.sendAckCount,
			channel.packetStats.sendAckByteCount, wantCount)
	}
	ip4Path := paths[0].ToIp4Path()
	ip6Path := paths[1].ToIp6Path()
	if count := channel.ip4DestinationSourceCount[ip4Path.Destination()][ip4Path.Source()]; count != wantCount {
		t.Errorf("ipv4 bucket references=%d want=%d", count, wantCount)
	}
	if count := channel.ip6DestinationSourceCount[ip6Path.Destination()][ip6Path.Source()]; count != wantCount {
		t.Errorf("ipv6 bucket references=%d want=%d", count, wantCount)
	}
	if wantCount == 0 && (len(channel.ip4DestinationSourceCount) != 0 || len(channel.ip6DestinationSourceCount) != 0) {
		t.Error("empty window retained source-map owners")
	}
}

// Being exactly at the bound must not trim a live bucket.
func TestMultiClientChannelWindowExactBoundPreservesAccounting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		channel, paths := newChannelWindowStatsFixture()
		seedChannelWindowStats(channel, paths, 11)
		requireChannelWindowStats(t, channel, paths, 11)
		stats, err := channel.WindowStats()
		if err != nil || stats.bucketCount != 9 {
			t.Fatalf("exact-bound window: stats=%+v err=%v", stats, err)
		}
		requireChannelWindowStats(t, channel, paths, 11)
	})
}

// Expiring the prefix releases only that prefix's accounting and references.
func TestMultiClientChannelWindowExpiredPrefixPreservesReferences(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		channel, paths := newChannelWindowStatsFixture()
		seedChannelWindowStats(channel, paths, 11)
		time.Sleep(101 * time.Millisecond)
		addChannelWindowStatsPacket(channel, paths)
		requireChannelWindowStats(t, channel, paths, 11)
		stats, err := channel.WindowStats()
		if err != nil || stats.bucketCount != 9 {
			t.Fatalf("expired-prefix window: stats=%+v err=%v", stats, err)
		}
		requireChannelWindowStats(t, channel, paths, 11)
	})
}

// Quiet windows legitimately become empty; fresh activity owns a new bucket.
func TestMultiClientChannelWindowInactiveExpiryAndRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		channel, paths := newChannelWindowStatsFixture()
		seedChannelWindowStats(channel, paths, 3)
		stats, err := channel.WindowStats()
		if err != nil || stats.bucketCount != 1 {
			t.Fatalf("active window: stats=%+v err=%v", stats, err)
		}
		time.Sleep(time.Second + time.Nanosecond)
		stats, err = channel.WindowStats()
		if err != nil || stats.bucketCount != 0 {
			t.Fatalf("inactive window: stats=%+v err=%v", stats, err)
		}
		requireChannelWindowStats(t, channel, paths, 0)

		addChannelWindowStatsPacket(channel, paths)
		requireChannelWindowStats(t, channel, paths, 1)
		stats, err = channel.WindowStats()
		if err != nil || stats.bucketCount != 0 {
			t.Fatalf("partial restarted window: stats=%+v err=%v", stats, err)
		}
	})
}

// An event at the cutoff is live; the next nanosecond releases its owner.
func TestMultiClientChannelWindowExpiryBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		channel, paths := newChannelWindowStatsFixture()
		addChannelWindowStatsPacket(channel, paths)
		time.Sleep(time.Second)
		if _, err := channel.WindowStats(); err != nil {
			t.Fatal(err)
		}
		requireChannelWindowStats(t, channel, paths, 1)
		time.Sleep(time.Nanosecond)
		if _, err := channel.WindowStats(); err != nil {
			t.Fatal(err)
		}
		requireChannelWindowStats(t, channel, paths, 0)
	})
}

// A quiet boundary expires only the oldest bucket before the next append;
// coalescing must retain all live owners and the newest returned pointer.
func TestMultiClientChannelWindowRetainsLiveBucketsAfterExpiredPrefix(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultMultiClientSettings()
		settings.StatsWindowDuration = time.Second
		settings.StatsWindowBucketDuration = 100 * time.Millisecond
		channel := &multiClientChannel{
			settings:    settings,
			packetStats: &clientWindowStats{},
		}
		touch := func() {
			channel.stateLock.Lock()
			channel.eventBucket()
			channel.stateLock.Unlock()
		}
		for i := 0; i < 11; i++ {
			channel.addSendNack(1)
			channel.addSendAck(1)
			if i < 10 {
				time.Sleep(99 * time.Millisecond)
				touch()
				time.Sleep(2 * time.Millisecond)
			}
		}
		if len(channel.eventBuckets) != 11 || channel.packetStats.sendAckCount != 11 {
			t.Fatalf("invalid setup: buckets=%d ack_count=%d", len(channel.eventBuckets), channel.packetStats.sendAckCount)
		}

		// No event happens between 1.010s and 1.111s. At the next event,
		// bucket zero's last event at .099s is expired; bucket one's last
		// event at .200s is still live. Appending makes 12 before coalescing.
		time.Sleep(101 * time.Millisecond)
		channel.stateLock.Lock()
		newest := channel.eventBucket()
		bucketCount := len(channel.eventBuckets)
		ackCount := channel.packetStats.sendAckCount
		newestRetained := bucketCount > 0 && channel.eventBuckets[bucketCount-1] == newest
		channel.stateLock.Unlock()

		if bucketCount != 11 {
			t.Errorf("coalescing discarded live buckets: got %d want 11", bucketCount)
		}
		if ackCount != 10 {
			t.Errorf("coalescing discarded live accounting: got %d want 10", ackCount)
		}
		if !newestRetained {
			t.Error("eventBucket returned an orphaned newest bucket")
		}
	})
}
