// Terminal native resets secure Transfer receipts without asking the provider
// for return IP traffic. Other controls and payload retain their usual evidence.
package connect

import (
	"net"
	"testing"
	"testing/synctest"
	"time"
)

// Exercises both channel entries, the generated race-retirement reset, and the
// normal serialized Transfer and peer receipt for each original.
func TestMultiClientReceiveAckTerminalResetsDoNotCorroborateSilence(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newGroupDispositionQueueFixture(t, nil, nil)
		channel := fixture.selected
		path := func(last byte) *IpPath {
			return &IpPath{Version: 4, Protocol: IpProtocolTcp, SourceIp: net.IPv4(192, 0, 2, 60),
				SourcePort: 42000, DestinationIp: net.IPv4(198, 51, 100, last), DestinationPort: 443}
		}
		first, second := path(61), path(62)
		firstReset, ok := ipOosRst(first)
		if !ok {
			t.Fatal("could not create the normal race-retirement reset")
		}
		secondReset := ipOosTcpPacketSequence(second, tcpFlagRst|tcpFlagAck, 102, nil)
		update := newMultiClientChannelUpdate(t.Context(), first)
		update.client.Store(channel)
		t.Cleanup(update.Close)

		// The helper borrows the generated packet. Retain the caller's root
		// through the send, as the race-retirement path does.
		borrowed := MessagePoolCopy(firstReset)
		defer MessagePoolReturn(borrowed)
		groupReset := func() {
			t.Helper()
			if !update.sendAffinityControl(channel, borrowed, 0) {
				t.Fatal("generated terminal reset was refused")
			}
		}
		send := func(grouped bool, templates ...[]byte) {
			t.Helper()
			group := &parsedPacketGroup{}
			for _, template := range templates {
				packet := groupRecoveryParsed(t, MessagePoolCopy(template))
				group.packets = append(group.packets, *packet)
				group.byteCount += ByteCount(len(template))
			}
			group.ipPath = group.packets[0].ipPath
			var admitted bool
			var err error
			if grouped {
				admitted, err = channel.SendGroupDetailedWithAck(group, 0, false)
			} else {
				admitted, err = channel.SendDetailedWithAck(&group.packets[0], 0, false)
			}
			if !admitted || err != nil {
				for _, packet := range group.packets {
					MessagePoolReturn(packet.packet)
				}
				t.Fatalf("native TCP offer admitted=%t err=%v", admitted, err)
			}
		}
		groupReset()
		fixture.requireParked()
		fixture.unpark()
		fixture.acknowledgeAll(firstReset)
		send(false, secondReset)
		fixture.acknowledgeAll(secondReset)

		const receiveTimeout = 20 * time.Second
		// Three real send buckets put the first one past the two partial
		// buckets excluded from the rolling window.
		for range 2 {
			time.Sleep(2 * time.Second)
			groupReset()
			fixture.acknowledgeAll(firstReset)
		}
		time.Sleep(receiveTimeout - 4*time.Second)
		check := func(stage string, destinations, completions int, bytes ByteCount, want blackholeReason) {
			t.Helper()
			stats, err := channel.WindowStats()
			if err != nil || stats.sendDestinationCount != 2 || stats.sendAckCount != completions ||
				stats.sendAckByteCount != bytes || stats.sendNackCount != 0 || stats.firstSendNackTime.IsZero() ||
				!hasPeerSendAckInWindow(stats) || stats.receiveAckCount != 0 || stats.receiveSynCount != 0 {
				t.Fatalf("%s lost wire premise: offered destinations=%d ACKs=%d/%dB outstanding=%d received=%d/%d err=%v",
					stage, stats.sendDestinationCount, stats.sendAckCount, stats.sendAckByteCount, stats.sendNackCount,
					stats.receiveAckCount, stats.receiveSynCount, err)
			}
			for _, minDestinations := range []int{2, 1, 0} {
				reason, held := blackholeReasonFromStats(time.Now(), stats, 30*time.Second,
					receiveTimeout, 30*time.Second, blackholeGates{minReceiveAckDestinations: minDestinations})
				if reason != want || held != blackholeNone || stats.receiveAckDestinationCount != destinations {
					t.Errorf("%s min=%d: verdict=%q held=%q destinations=%d, want %q/%q/%d",
						stage, minDestinations, reason, held, stats.receiveAckDestinationCount, want, blackholeNone, destinations)
				}
			}
		}
		resetBytes := ByteCount(3*len(firstReset) + len(secondReset))
		check("only terminal resets", 0, 4, resetBytes, blackholeNone)

		// Keep the reset cohort in the same stats window as new questions
		// and retain real payload in either position of a mixed group.
		groupReset()
		fixture.acknowledgeAll(firstReset)
		const offset = 500 * time.Millisecond
		time.Sleep(offset)
		firstData := reraceSegment(first, 0, []byte("first request"))
		secondData := reraceSegment(second, 0, []byte("second request"))
		send(true, firstReset, firstData)
		fixture.acknowledgeAll(firstReset, firstData)
		send(true, secondData, secondReset)
		fixture.acknowledgeAll(secondData, secondReset)
		mixedBytes := ByteCount(len(firstReset) + len(secondReset) + len(firstData) + len(secondData))
		check("fresh requests after resets", 2, 9, resetBytes+ByteCount(len(firstReset))+mixedBytes, blackholeNone)
		for range 2 {
			time.Sleep(2 * time.Second)
			send(false, firstData)
			fixture.acknowledgeAll(firstData)
		}
		// The old reset-only buckets have expired. The retained window has
		// the reset just before the mixed requests, both groups and both
		// subsequent sends. The earlier reset cannot advance their deadline.
		time.Sleep(receiveTimeout - 4*time.Second - offset)
		retainedBytes := ByteCount(len(firstReset)+2*len(firstData)) + mixedBytes
		check("same-bucket reset before the request deadline", 2, 7, retainedBytes, blackholeNone)
		time.Sleep(offset)
		check("aged unanswered requests", 2, 7, retainedBytes, blackholeNoReceiveAck)
	})
}
