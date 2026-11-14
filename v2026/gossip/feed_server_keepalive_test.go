// Feed keepalives measure silence on the subscriber's stream. Records in
// another partition must neither postpone that clock nor leak to the client.
package gossip

import (
	"context"
	"fmt"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// One decoded frame and the synthetic-clock instant when the client received it.
type feedHeartbeatFrame struct {
	frame *protocol.ExtenderFeedFrame
	at    time.Time
}

// A real feed/directory pair with records inside and outside the subscriber's
// partition, and a dedicated client reader that records completed frames.
type feedHeartbeatFixture struct {
	directory *connect.ExtenderDirectory
	rootKey   *testKey
	keys      []*testKey
	included  int
	excluded  int
	frames    chan feedHeartbeatFrame
	client    *testFeedClient
	feed      *FeedServer
	start     time.Time
}

// The generated record set guarantees distinct partitions; the fixture picks
// one member and one nonmember after the partition has been constructed.
func newFeedHeartbeatFixture(t *testing.T, settings *FeedServerSettings) *feedHeartbeatFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rootKey := newTestKey(t)
	directory := newTestDirectory(t, rootKey)
	keys := make([]*testKey, 64)
	for i := range keys {
		keys[i] = newTestKey(t)
		applyTestFeedRecord(t, directory, rootKey, keys[i], fmt.Sprintf("192.0.2.%d", i+1))
	}
	remote := &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 40000}
	vantage := connect.ExtenderVantageKeyOfAddr(remote)
	included, excluded := -1, -1
	for i, key := range keys {
		if directory.OpenPartitionContains(vantage, key.publicKey) {
			included = i
		} else {
			excluded = i
		}
	}
	if included < 0 || excluded < 0 {
		t.Fatal("fixture did not construct distinct open partitions")
	}
	feed := NewFeedServer(ctx, directory, nil, settings)
	t.Cleanup(feed.Close)
	client := newTestFeedClientAt(t, feed, remote.String(), &protocol.ExtenderFeedRequest{Subscribe: true})
	client.readEndOfSample(t)
	fixture := &feedHeartbeatFixture{
		directory: directory,
		rootKey:   rootKey,
		keys:      keys,
		included:  included,
		excluded:  excluded,
		frames:    make(chan feedHeartbeatFrame, 64),
		client:    client,
		feed:      feed,
		start:     time.Now(),
	}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			frame, err := connect.ReadExtenderFeedFrame(client.conn)
			if err != nil {
				return
			}
			select {
			case fixture.frames <- feedHeartbeatFrame{frame: frame, at: time.Now()}:
			case <-ctx.Done():
				return
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		client.conn.Close()
		<-readDone
	})
	synctest.Wait()
	return fixture
}

// Refreshes an existing signed record without changing the partition's size.
func (self *feedHeartbeatFixture) publish(t *testing.T, included bool) {
	t.Helper()
	index := self.excluded
	if included {
		index = self.included
	}
	record := signTestRecord(t, self.rootKey, self.keys[index], fmt.Sprintf("192.0.2.%d", index+1), 8443, time.Now())
	if changed, err := self.directory.ApplyRecord(record, connect.ExtenderSourceGossip); err != nil || !changed {
		t.Fatalf("record refresh: changed=%t, err=%v", changed, err)
	}
	synctest.Wait()
}

// Checks one completed frame without moving the synthetic clock.
func (self *feedHeartbeatFixture) expect(t *testing.T, elapsed time.Duration, keepalive bool) {
	t.Helper()
	synctest.Wait()
	select {
	case received := <-self.frames:
		if received.at != self.start.Add(elapsed) || received.frame.GetKeepalive() != keepalive ||
			(!keepalive && received.frame.GetRecord() == nil) {
			t.Fatalf("frame at %s: %v, expected keepalive=%t at %s", received.at.Sub(self.start), received.frame, keepalive, elapsed)
		}
	default:
		t.Fatalf("no frame at %s, expected keepalive=%t", elapsed, keepalive)
	}
}

// Sustained unrelated records formerly restarted time.After on every select,
// suppressing all five keepalives while the subscriber held its slot for 160 s.
func TestGossipFeedFilteredRecordsCannotPostponeKeepalive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newFeedHeartbeatFixture(t, DefaultFeedServerSettings())
		for i := 1; i <= 16; i++ {
			time.Sleep(10 * time.Second)
			fixture.publish(t, false)
			if i%3 == 0 {
				fixture.expect(t, time.Duration(i)*10*time.Second, true)
			}
		}
		select {
		case frame := <-fixture.frames:
			t.Fatalf("unexpected frame after five keepalives: %+v", frame)
		default:
		}
	})
}

// Actual record delivery renews the silence clock; unrelated records before
// and after that delivery do not. This also exercises a configured cadence.
func TestGossipFeedKeepaliveTracksLastWrittenRecord(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultFeedServerSettings()
		settings.KeepaliveTimeout = 12 * time.Second
		fixture := newFeedHeartbeatFixture(t, settings)
		time.Sleep(4 * time.Second)
		fixture.publish(t, false)
		time.Sleep(4 * time.Second)
		fixture.publish(t, true)
		fixture.expect(t, 8*time.Second, false)
		for range 3 {
			time.Sleep(4 * time.Second)
			fixture.publish(t, false)
		}
		fixture.expect(t, 20*time.Second, true)
		time.Sleep(4 * time.Second)
		fixture.publish(t, true)
		fixture.expect(t, 24*time.Second, false)
		for range 3 {
			time.Sleep(4 * time.Second)
			fixture.publish(t, false)
		}
		fixture.expect(t, 36*time.Second, true)
	})
}

// Disabling heartbeats parks the stream until data or cancellation. It must
// not create the immediate timer loop of a non-positive time.After duration.
func TestGossipFeedDisabledKeepaliveStillStreamsAndCloses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultFeedServerSettings()
		settings.KeepaliveTimeout = 0
		fixture := newFeedHeartbeatFixture(t, settings)
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		select {
		case frame := <-fixture.frames:
			t.Fatalf("disabled heartbeat produced a frame: %+v", frame)
		default:
		}
		fixture.publish(t, true)
		fixture.expect(t, 2*time.Minute, false)
		fixture.feed.Close()
		<-fixture.client.done
		if got := fixture.feed.SubscriberCount(); got != 0 {
			t.Fatalf("canceled feed kept %d subscribers", got)
		}
	})
}
