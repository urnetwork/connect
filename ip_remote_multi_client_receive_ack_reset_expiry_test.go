// Rotation and reset eligibility meet at the original singleton's stats
// bucket. These controls keep the exact wire bytes and peer receipt visible.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// The sole response-bearing singleton in a reset bucket expires unwritten.
func TestMultiClientReceiveAckResetBucketDropsUnwrittenSingleton(t *testing.T) {
	testReceiveAckResetBucketExpiry(t, false)
}

// A later payload to the same endpoint keeps its own full silence interval.
func TestMultiClientReceiveAckResetBucketPreservesNeighborDeadline(t *testing.T) {
	testReceiveAckResetBucketExpiry(t, true)
}

// The source is held before its first run. A real expiry removes the first
// payload, then an independently decoded Pack earns each later peer receipt.
func testReceiveAckResetBucketExpiry(t *testing.T, sameTuple bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newGroupDispositionQueueFixture(t, nil, nil)
		channel := fixture.selected
		firstPath := reraceFlow(4, 1, 40601)
		secondPath := reraceFlow(4, 2, 40602)
		if sameTuple {
			secondPath = firstPath
		}
		thirdPath := reraceFlow(4, 3, 40603)
		reset, ok := ipOosRst(reraceFlow(4, 4, 40604))
		if !ok {
			t.Fatal("could not build the terminal reset")
		}
		expired := reraceSegment(firstPath, 0, []byte("expires before serialization"))
		second := reraceSegment(secondPath, 43, []byte("surviving request"))
		third := reraceSegment(thirdPath, 0, []byte("another request"))
		again := reraceSegment(secondPath, 95, []byte("later request"))
		send := func(template []byte, timeout time.Duration) {
			t.Helper()
			packet := MessagePoolCopy(template)
			parsed := groupRecoveryParsed(t, packet)
			if admitted, err := channel.SendDetailedWithAck(parsed, timeout, true); !admitted || err != nil {
				MessagePoolReturn(packet)
				t.Fatalf("native offer admitted=%t err=%v", admitted, err)
			}
		}
		start := time.Now()
		send(expired, time.Second)
		fixture.requireParked()
		send(reset, 0)
		// No peer receipt can exist at this parked cut. Only the pending
		// expiring payload can qualify its original bucket.
		stats, err := channel.WindowStats()
		if err != nil || stats.sendDestinationCount != 2 || stats.sendNackCount != 2 ||
			stats.sendAckCount != 0 || hasPeerSendAckInWindow(stats) {
			t.Fatalf("lost parked source evidence: offered=%d outstanding=%d ACKs=%d err=%v",
				stats.sendDestinationCount, stats.sendNackCount, stats.sendAckCount, err)
		}
		if stats.receiveAckDestinationCount != 1 {
			t.Errorf("only the pending payload can qualify the parked bucket: got %d destinations", stats.receiveAckDestinationCount)
		}
		// Independent timing controls choose either a survivor in the next
		// bucket, or a survivor in the same tuple and original bucket.
		const bucketRotation = 2 * time.Second
		delay := bucketRotation
		if sameTuple {
			delay = 500 * time.Millisecond
		}
		time.Sleep(delay)
		survivingTime := time.Now()
		send(second, 0)
		if sameTuple {
			time.Sleep(bucketRotation - delay)
			send(third, 0)
		}
		fixture.requireParked()
		fixture.unpark()
		if sameTuple {
			fixture.acknowledgeAll(reset, second, third)
		} else {
			fixture.acknowledgeAll(reset, second)
		}
		if drops := fixture.client.ReceiveStats().SendPackDeadlineDropCount; drops != 1 {
			t.Fatalf("source dropped %d original deadlines, want exactly the unwritten payload", drops)
		}
		const receiveTimeout = 20 * time.Second
		time.Sleep(time.Until(start.Add(receiveTimeout)))
		if sameTuple {
			send(again, 0)
			fixture.acknowledgeAll(again)
		} else {
			send(third, 0)
			fixture.acknowledgeAll(third)
		}
		gates := blackholeGates{minReceiveAckDestinations: 2}
		check := func(stage string, want blackholeReason) {
			t.Helper()
			stats, err := channel.WindowStats()
			wantOffered := 4
			if sameTuple {
				wantOffered = 3
			}
			if err != nil || stats.sendDestinationCount != wantOffered || stats.sendNackCount != 0 ||
				stats.receiveAckCount != 0 || !hasPeerSendAckInWindow(stats) {
				t.Fatalf("%s lost actual offered paths, peer receipts or quiet IP path: offered=%d outstanding=%d received=%d err=%v",
					stage, stats.sendDestinationCount, stats.sendNackCount, stats.receiveAckCount, err)
			}
			reason, held := blackholeReasonFromStats(time.Now(), stats, 30*time.Second,
				receiveTimeout, 30*time.Second, gates)
			if stats.receiveAckDestinationCount != 2 || reason != want || held != blackholeNone {
				t.Errorf("%s: verdict=%q held=%q destination count=%d, want %q/%q/2",
					stage, reason, held, stats.receiveAckDestinationCount, want, blackholeNone)
			}
		}
		check("retired singleton's deadline", blackholeNone)
		time.Sleep(time.Until(survivingTime.Add(receiveTimeout)))
		if !sameTuple {
			send(again, 0)
			fixture.acknowledgeAll(again)
		}
		check("surviving singleton's deadline", blackholeNoReceiveAck)
	})
}
