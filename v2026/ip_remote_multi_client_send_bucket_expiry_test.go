package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// A singleton's real Transfer expiry must retract the older bucket that
// recorded it, preserving the next real send's full receive-silence interval.
func TestQueuedReliableIpExpiryPreservesReceiveAckClock(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newGroupDispositionQueueFixture(t, nil, nil)
		channel := fixture.selected
		first := reraceSegment(reraceFlow(4, 1, 40401), 0, []byte("locally expired"))
		second := reraceSegment(reraceFlow(4, 2, 40402), 0, []byte("first wire packet"))
		third := reraceSegment(reraceFlow(4, 3, 40403), 0, []byte("second wire packet"))
		secondAgain := reraceSegment(reraceFlow(4, 2, 40402), 17, []byte("later wire packet"))
		send := func(template []byte, timeout time.Duration) {
			t.Helper()
			packet := MessagePoolCopy(template)
			parsed, err := newParsedPacket(packet)
			if err != nil {
				MessagePoolReturn(packet)
				t.Fatal(err)
			}
			if admitted, err := channel.SendDetailedWithAck(parsed, timeout, true); !admitted || err != nil {
				MessagePoolReturn(packet)
				t.Fatalf("TCP admission=%t err=%v", admitted, err)
			}
		}

		const receiveTimeout = 20 * time.Second
		const bucketRotation = 2 * time.Second
		send(first, time.Second)
		fixture.requireParked()
		time.Sleep(bucketRotation)
		secondSendTime := time.Now()
		send(second, 0)
		fixture.requireParked()
		fixture.unpark()
		fixture.acknowledgeAll(second)
		if drops := fixture.client.ReceiveStats().SendPackDeadlineDropCount; drops != 1 {
			t.Fatalf("unwritten deadline drops=%d, want 1", drops)
		}

		time.Sleep(receiveTimeout - bucketRotation)
		send(third, 0)
		fixture.acknowledgeAll(third)
		stats, err := channel.WindowStats()
		if err != nil {
			t.Fatal(err)
		}
		if stats.bucketCount != 1 || stats.sendDestinationCount != 3 || stats.receiveAckDestinationCount != 2 ||
			stats.sendAckCount != 2 || stats.sendNackCount != 0 || stats.receiveAckCount != 0 || !hasPeerSendAckInWindow(stats) {
			t.Fatalf("unexpected receive-ACK premise: buckets=%d offered=%d admitted=%d sent=%d pending=%d received=%d peerAck=%t",
				stats.bucketCount, stats.sendDestinationCount, stats.receiveAckDestinationCount, stats.sendAckCount,
				stats.sendNackCount, stats.receiveAckCount, hasPeerSendAckInWindow(stats))
		}
		gates := blackholeGates{minReceiveAckDestinations: 2}
		if reason, held := blackholeReasonFromStats(time.Now(), stats, 30*time.Second, receiveTimeout, 30*time.Second, gates); reason != blackholeNone || held != blackholeNone {
			t.Errorf("expired unwritten packet triggered receive-ACK policy: reason=%s held=%s", reason, held)
		}

		time.Sleep(bucketRotation)
		send(secondAgain, 0)
		fixture.acknowledgeAll(secondAgain)
		stats, err = channel.WindowStats()
		if err != nil {
			t.Fatal(err)
		}
		if !stats.firstSendNackTime.Equal(secondSendTime) || time.Since(secondSendTime) != receiveTimeout {
			t.Errorf("receive-ACK clock did not start at the surviving send: first=%s surviving=%s age=%s",
				stats.firstSendNackTime, secondSendTime, time.Since(secondSendTime))
		}
		if reason, held := blackholeReasonFromStats(time.Now(), stats, 30*time.Second, receiveTimeout, 30*time.Second, gates); reason != blackholeNoReceiveAck || held != blackholeNone {
			t.Errorf("surviving send did not trigger receive-ACK policy at its deadline: reason=%s held=%s", reason, held)
		}
	})
}
