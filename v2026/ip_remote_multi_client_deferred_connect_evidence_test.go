package connect

import (
	"bytes"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

func TestOrderedDataAfterImmediateSynAckAnswersConnect(t *testing.T) {
	checkDeferredConnectEvidence(t, "none")
}

func TestOrderedDataAfterImmediateSynAckKeepsEarlierUnansweredConnect(t *testing.T) {
	checkDeferredConnectEvidence(t, "earlier")
}

func TestOrderedDataAfterImmediateSynAckKeepsLaterUnansweredConnect(t *testing.T) {
	checkDeferredConnectEvidence(t, "later")
}

func TestOrderedDataAfterImmediateSynAckKeepsConnectAdmittedBeforeReply(t *testing.T) {
	checkDeferredConnectEvidence(t, "later-before-reply")
}

func TestOrderedDataWithoutSynAckCoverageKeepsUnansweredConnect(t *testing.T) {
	checkDeferredConnectEvidence(t, "uncovered-ack")
}

// The original SYN and any independently unanswered SYN use the same exit.
// Both ends of the real return sequence carry the exact outbound endpoint IDs.
func checkDeferredConnectEvidence(t *testing.T, scenario string) {
	t.Helper()
	ownedTestWireSynctest(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		fixture := newAffinitySynQueueFixture(t, 4, nil)
		exit := fixture.first.selected
		fixture.raceOnly(exit)
		exit.clientReceivePacketCallback = fixture.parent.clientReceivePacket
		exit.reliabilitySettingsFunc = fixture.parent.ReliabilitySettings
		var delivered [][]byte
		fixture.parent.SetReceivePacketCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
			delivered = append(delivered, bytes.Clone(packet))
		})

		first := true
		admit := func(path *IpPath) time.Time {
			t.Helper()
			before := time.Now()
			packet := reraceTcpPacket(path, tcpFlagSyn, path.SequenceNumber, 0, nil)
			if fixture.first.offer(packet) != 1 {
				t.Fatal("public TCP SYN was refused")
			}
			if first {
				fixture.first.requireParked()
				fixture.first.unpark()
				first = false
			}
			fixture.first.acknowledgeAll(packet)
			update := fixture.parent.receiveUpdate(path)
			if update == nil || update.client.Load() != exit || !update.synGenerationSeen ||
				update.synGenerationNumber != path.SequenceNumber || !time.Now().Equal(before) {
				t.Fatal("reliable SYN admission lost its selected generation or virtual admission time")
			}
			return before
		}
		snapshot := func() *clientWindowStats {
			t.Helper()
			stats, err := exit.WindowStats()
			if err != nil || !stats.connectProgressKnown {
				t.Fatalf("exit connect evidence unavailable: %v", err)
			}
			return stats
		}
		other := *fixture.path
		other.SourcePort++
		other.SequenceNumber += 1000
		var otherStarted time.Time
		if scenario == "earlier" {
			otherStarted = admit(&other)
			time.Sleep(time.Millisecond)
		}
		primaryStarted := admit(fixture.path)
		if scenario == "later-before-reply" {
			time.Sleep(time.Millisecond)
			otherStarted = admit(&other)
		}
		earliest := primaryStarted
		if scenario == "earlier" {
			earliest = otherStarted
		}
		update := fixture.parent.receiveUpdate(fixture.path)
		if debt := unansweredConnectSince(snapshot()); !debt.Equal(earliest) {
			t.Fatalf("admitted connect clock = %v, want %v", debt, earliest)
		}

		returnWire := newWindowRoundFixtureWithIds(t, exit.args.Destination.Tail(), exit.ClientId(), nil, nil)
		wire := openImmediateParentHealthGap(t, exit, fixture.path, returnWire)
		ack := fixture.path.SequenceNumber + 1
		synAck := reraceTcpPacket(fixture.path.Reverse(), tcpFlagSyn|tcpFlagAck, 7000, ack, nil)
		immediate := wire.sendControl(synAck, true)
		synctest.Wait()
		if len(delivered) != 1 || !bytes.Equal(delivered[0], synAck) || update.client.Load() != exit ||
			!update.receivedInbound.Load() || !update.deferredInboundHealth || !unansweredConnectSince(snapshot()).Equal(earliest) {
			t.Fatal("ahead-of-gap SYN-ACK lost application ownership or cleared unanswered evidence")
		}
		if scenario == "later" {
			time.Sleep(time.Millisecond)
			otherStarted = admit(&other)
		}

		wire.repair()
		synctest.Wait()
		repairBytes := ipOosTcpPacketSequence(wire.returnPath, tcpFlagAck|tcpFlagPsh, 1000, make([]byte, 1240))
		if len(delivered) != 2 || !bytes.Equal(delivered[1], repairBytes) || !unansweredConnectSince(snapshot()).Equal(earliest) {
			t.Fatal("repair of a different flow lost delivery or cleared the unresolved clock")
		}
		orderedAck := ack
		if scenario == "uncovered-ack" {
			// The segment is on the same return sequence, but its TCP ACK
			// does not cover the current opening SYN.
			orderedAck = fixture.path.SequenceNumber
		}
		data := reraceTcpPacket(fixture.path.Reverse(), tcpFlagAck|tcpFlagPsh, 7001, orderedAck, []byte("ordered"))
		ordered := wire.sendControl(data, false)
		synctest.Wait()
		exit.stateLock.Lock()
		successes := len(exit.connectSuccessTimes)
		exit.stateLock.Unlock()
		if !bytes.Equal(immediate.pack.SequenceId, ordered.pack.SequenceId) ||
			ordered.pack.SequenceNumber <= immediate.pack.SequenceNumber {
			t.Fatal("ordered DATA did not follow the early SYN-ACK on its return sequence")
		}
		if scenario != "uncovered-ack" && (len(delivered) != 3 || !bytes.Equal(delivered[2], data) ||
			successes != 1 || update.deferredInboundHealth) {
			t.Fatal("same-generation ordered DATA did not confirm the earlier handshake")
		}

		// Judge the real coalesced snapshot after both the original bar and
		// the recent receive counters have expired, with open carrier gates.
		connectTimeout := fixture.parent.settings.BlackholeConnectTimeout
		time.Sleep(max(connectTimeout, exit.settings.StatsWindowDuration) + 2*exit.settings.StatsWindowBucketDuration)
		synctest.Wait()
		stats := snapshot()
		if stats.receiveSynCount != 0 || stats.receiveAckCount != 0 || stats.sendSynCount != 0 {
			t.Fatal("the original SYN or its ordered return still occupies the recent window")
		}
		reason, held := blackholeReasonFromStats(time.Now(), stats, 0, 0, connectTimeout, blackholeGates{})
		wantDebt := otherStarted
		if scenario == "uncovered-ack" {
			wantDebt = primaryStarted
		}
		wantReason := blackholeNone
		if !wantDebt.IsZero() {
			wantReason = blackholeNoReceiveSyn
		}
		if debt := unansweredConnectSince(stats); !debt.Equal(wantDebt) || reason != wantReason || held != blackholeNone {
			t.Fatalf("ordered DATA left the wrong unanswered connect: clock=%v want=%v reason=%q want=%q held=%q",
				debt, wantDebt, reason, wantReason, held)
		}
	})
}
