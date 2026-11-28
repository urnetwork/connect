// A partial reliable group supplies only its materialized member range as
// receive-clock evidence. Destination corroboration retains its usual rules.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// A retained reset prefix cannot start the disposed payload suffix's clock.
func TestMultiClientReceiveAckMaterializedResetPrefixKeepsFreshPayloadDeadline(t *testing.T) {
	testReceiveAckMaterializedPrefix(t, true)
}

// A retained payload prefix keeps its own original deadline after a suffix fails.
func TestMultiClientReceiveAckMaterializedPayloadPrefixKeepsOriginalDeadline(t *testing.T) {
	testReceiveAckMaterializedPrefix(t, false)
}

// Use the normal channel and source range callback to force the two exact
// dispositions. The independent sibling and the reset earn real peer ACKs.
func testReceiveAckMaterializedPrefix(t *testing.T, resetFirst bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newGroupDispositionQueueFixture(t, nil, nil)
		channel := fixture.selected
		path := reraceFlow(4, 1, 40801)
		reset, ok := ipOosRst(path)
		if !ok {
			t.Fatal("could not create the group reset")
		}
		data := reraceSegment(path, 0, []byte("group payload"))
		sibling := reraceSegment(reraceFlow(4, 2, 40802), 0, []byte("sibling payload"))
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

		templates := [][]byte{data, reset}
		if resetFirst {
			templates = [][]byte{reset, data}
		}
		group := &parsedPacketGroup{}
		for _, template := range templates {
			packet := MessagePoolCopy(template)
			defer MessagePoolReturn(packet)
			group.packets = append(group.packets, *groupRecoveryParsed(t, packet))
			group.byteCount += ByteCount(len(packet))
		}
		group.ipPath = group.packets[0].ipPath
		var complete AckFunction
		channel.sendTransferForTest = func(callback AckFunction) (bool, error) {
			complete = callback
			group.sendGroupDisposition(0, 1, true)
			group.sendGroupDisposition(1, 2, false)
			return true, nil
		}
		if admitted, err := channel.SendGroupDetailedWithAck(group, 0, true); !admitted || err != nil || complete == nil {
			t.Fatalf("mixed group admitted=%t err=%v", admitted, err)
		}
		complete(errSendPackExpiredUnwritten)
		channel.sendTransferForTest = nil

		// The two later real sends leave the original bucket mature.
		for range 2 {
			time.Sleep(2 * time.Second)
			send(sibling)
			fixture.acknowledgeAll(sibling)
		}
		const receiveTimeout = 20 * time.Second
		time.Sleep(receiveTimeout - 4*time.Second)
		check := func(stage string, want blackholeReason) {
			t.Helper()
			stats, err := channel.WindowStats()
			if err != nil || stats.receiveAckDestinationCount != 2 || !hasPeerSendAckInWindow(stats) ||
				stats.sendNackCount != 0 || stats.receiveAckCount != 0 {
				t.Fatalf("%s lost exact range or sibling custody: destinations=%d outstanding=%d received=%d err=%v",
					stage, stats.receiveAckDestinationCount, stats.sendNackCount, stats.receiveAckCount, err)
			}
			reason, held := blackholeReasonFromStats(time.Now(), stats, 30*time.Second,
				receiveTimeout, 30*time.Second, blackholeGates{minReceiveAckDestinations: 2})
			if reason != want || held != blackholeNone {
				t.Errorf("%s: verdict=%q held=%q, want %q/%q", stage, reason, held, want, blackholeNone)
			}
		}
		want := blackholeNoReceiveAck
		if resetFirst {
			want = blackholeNone
		}
		check("original mixed group deadline", want)
		time.Sleep(2 * time.Second)
		send(sibling)
		fixture.acknowledgeAll(sibling)
		check("sibling payload deadline", blackholeNoReceiveAck)
	})
}
