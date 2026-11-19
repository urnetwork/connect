// A written response-bearing packet keeps its original silence deadline when
// the peer's Transfer acknowledgement is withheld.
package connect

import (
	"bytes"
	"testing"
	"testing/synctest"
	"time"
)

// A terminal reset supplies the sole peer receipt. Withhold every cumulative
// ACK after it so later TCP data cannot accidentally acknowledge earlier data.
func TestMultiClientReceiveAckResetBucketAgesWrittenNeighborWithoutPeerAck(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newGroupDispositionQueueFixture(t, nil, nil)
		channel := fixture.selected
		reset, ok := ipOosRst(reraceFlow(4, 1, 40701))
		if !ok {
			t.Fatal("could not build the terminal reset")
		}
		first := reraceSegment(reraceFlow(4, 2, 40702), 0, []byte("first written request"))
		otherPath := reraceFlow(4, 3, 40703)
		second := reraceSegment(otherPath, 0, []byte("second written request"))
		again := reraceSegment(otherPath, 37, []byte("later written request"))
		send := func(template []byte) {
			t.Helper()
			packet := MessagePoolCopy(template)
			if admitted, err := channel.SendDetailedWithAck(groupRecoveryParsed(t, packet), 0, true); !admitted || err != nil {
				MessagePoolReturn(packet)
				t.Fatalf("native TCP offer admitted=%t err=%v", admitted, err)
			}
		}
		send(reset)
		fixture.requireParked()
		fixture.unpark()
		fixture.acknowledgeAll(reset)

		// Drain the real selected route, including any normal retries whose
		// bytes were already seen. Only this fixture can send a peer ACK.
		requiredWrite := func(want []byte) {
			t.Helper()
			synctest.Wait()
			found := false
			for len(fixture.route) != 0 {
				wire := <-fixture.route
				func() {
					defer MessagePoolReturn(wire)
					pack := decodeSendPackLifecycleWirePack(t, wire)
					if pack.Nack || len(pack.Frames) == 0 {
						t.Fatal("lost reliable TCP wire ownership")
					}
					for _, frame := range pack.Frames {
						packet := frame.MessageBytes
						if !bytes.Equal(packet, first) && !bytes.Equal(packet, second) && !bytes.Equal(packet, again) {
							t.Fatal("unexpected frame on the unacknowledged data route")
						}
						if bytes.Equal(packet, want) {
							found = true
						}
					}
				}()
			}
			if !found {
				t.Fatal("response-bearing original did not reach its carrier route")
			}
		}

		const offset = 500 * time.Millisecond
		const timeout = 20 * time.Second
		start := time.Now()
		time.Sleep(offset)
		send(first)
		requiredWrite(first)
		time.Sleep(2*time.Second - offset)
		send(second)
		requiredWrite(second)
		time.Sleep(2 * time.Second)
		send(again)
		requiredWrite(again)
		gates := blackholeGates{minReceiveAckDestinations: 2}
		check := func(stage string, want blackholeReason) {
			t.Helper()
			stats, err := channel.WindowStats()
			if err != nil || stats.sendDestinationCount != 3 || stats.sendAckCount != 1 ||
				stats.sendAckByteCount != ByteCount(len(reset)) || stats.sendNackCount != 3 ||
				stats.receiveAckCount != 0 || !hasPeerSendAckInWindow(stats) {
				t.Fatalf("%s lost carrier/peer premise: offered=%d ACKs=%d/%dB outstanding=%d received=%d err=%v",
					stage, stats.sendDestinationCount, stats.sendAckCount, stats.sendAckByteCount, stats.sendNackCount, stats.receiveAckCount, err)
			}
			reason, held := blackholeReasonFromStats(time.Now(), stats, 30*time.Second, timeout, 30*time.Second, gates)
			if reason != want || held != blackholeNone || stats.receiveAckDestinationCount != 2 {
				t.Errorf("%s: verdict=%q held=%q destinations=%d, want %q/%q/2", stage,
					reason, held, stats.receiveAckDestinationCount, want, blackholeNone)
			}
		}
		time.Sleep(time.Until(start.Add(timeout)))
		check("reset deadline", blackholeNone)
		time.Sleep(offset)
		check("written payload deadline with its ACK lost", blackholeNoReceiveAck)
	})
}
