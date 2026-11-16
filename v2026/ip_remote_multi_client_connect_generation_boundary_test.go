// Genuine native source controls retire only their own connect obligation.
// The existing paired carrier supplies all packet delivery and peer receipts.
package connect

import (
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Observe the original clock without altering it. Grouped sends sample this
// clock on actual health admission, not at native offer or peer ACK receipt.
// A fixed census rejects an unexpected caller instead of guessing its identity.
type connectGenerationAdmissionTimes struct {
	stateLock sync.Mutex
	times     [4]time.Time
	count     int
}

func (self *connectGenerationAdmissionTimes) now() time.Time {
	at := time.Now()
	self.stateLock.Lock()
	if self.count < len(self.times) {
		self.times[self.count] = at
	}
	self.count++
	self.stateLock.Unlock()
	return at
}

func (self *connectGenerationAdmissionTimes) require(t *testing.T, count int) [4]time.Time {
	t.Helper()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.count != count || count > len(self.times) {
		t.Fatalf("actual grouped admission clock census=%d, want %d", self.count, count)
	}
	return self.times
}

func requireConnectGenerationCarrier(t *testing.T, fixture *replyExpectationFixture, stats *clientWindowStats) {
	t.Helper()
	if fixture.base.client.IsDone() || fixture.peer.IsDone() || !fixture.base.selected.hasActiveTransport() ||
		stats.receiveAckCount != 0 || stats.receiveSynCount != 0 {
		t.Fatal("native connect control lost live carrier or zero native response premise")
	}
}

// This reads only the own source-dequeued generation and the actual peer's
// native delivery. Fully materialized descriptors need not stay linked.
func requireConnectGenerationOpening(t *testing.T, fixture *replyExpectationFixture, slot, packetIndex int, path *IpPath) *multiClientChannelUpdate {
	t.Helper()
	fixture.stateLock.Lock()
	update, packet := fixture.updates[slot], fixture.packets[packetIndex]
	fixture.stateLock.Unlock()
	if update == nil || !packet.syn || packet.ack || packet.rst || packet.fin || packet.payload {
		t.Fatal("real receiver did not attest the exact native SYN")
	}
	update.stateLock.Lock()
	valid := update.synGenerationSeen && update.synGenerationNumber == path.SequenceNumber &&
		update.sequenceMetricSynSeen && update.sequenceMetricSynNumber == path.SequenceNumber &&
		update.sequencePacketCount > 0 && !update.receivedInbound.Load() && !update.IsDone()
	update.stateLock.Unlock()
	if !valid {
		t.Fatal("actual source did not materialize its own live SYN generation")
	}
	return update
}

func requireConnectGenerationReset(t *testing.T, fixture *replyExpectationFixture, update *multiClientChannelUpdate, packetIndex int) {
	t.Helper()
	fixture.stateLock.Lock()
	packet := fixture.packets[packetIndex]
	fixture.stateLock.Unlock()
	if !update.IsDone() || !packet.rst || packet.syn || packet.ack || packet.fin || packet.payload {
		t.Fatal("actual source reset did not close its exact opening")
	}
}

// The genuine source must recover through its own ordinary retransmission.
// The retained first ACK is never replayed as progress by this test.
func awaitConnectGenerationPeerAcks(t *testing.T, fixture *replyExpectationFixture, packets int) *clientWindowStats {
	t.Helper()
	deadline := time.Now().Add(fixture.base.client.settings.SendBufferSettings.MaxResendInterval + 2*defaultAckCompressTimeout)
	for {
		synctest.Wait()
		fixture.stateLock.Lock()
		received, invalid := fixture.packetCount, fixture.invalid
		fixture.stateLock.Unlock()
		stats, err := fixture.base.selected.WindowStats()
		if err != nil || invalid {
			t.Fatal("late genuine ACK lost its carrier premise")
		}
		requireConnectGenerationCarrier(t, fixture, stats)
		if received == packets && stats.sendAckCount == packets && stats.sendNackCount == 0 && stats.sendNackByteCount == 0 {
			return stats
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("genuine peer ACK did not recover after return gate: received=%d acked=%d pending=%d/%d", received, stats.sendAckCount, stats.sendNackCount, stats.sendNackByteCount)
		}
		time.Sleep(defaultAckCompressTimeout)
	}
}

// The later opening keeps its own actual admission time after the earlier
// completed opening is reset, including beyond rolling telemetry expiry.
func TestNativeSourceResetPreservesSiblingConnectAge(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newResponseBoundaryFixture(t)
		clock := &connectGenerationAdmissionTimes{}
		fixture.base.selected.packetTransferNowForTest = clock.now
		firstPath, siblingPath := replyExpectationPath(0, 45000), replyExpectationPath(1, 45001)
		offerSourceResetConnectPacket(t, fixture, firstPath, tcpFlagSyn)
		firstStats := fixture.await(t, 1, 1, 0)
		requireConnectGenerationCarrier(t, fixture, firstStats)
		first := requireConnectGenerationOpening(t, fixture, 0, 0, firstPath)
		firstTimes := clock.require(t, 1)
		if !firstStats.firstUnansweredSendSynTime.Equal(firstTimes[0]) {
			t.Fatal("first health marker did not use its actual admission clock")
		}
		time.Sleep(time.Nanosecond)
		offerSourceResetConnectPacket(t, fixture, siblingPath, tcpFlagSyn)
		both := fixture.await(t, 2, 2, 0)
		requireConnectGenerationCarrier(t, fixture, both)
		sibling := requireConnectGenerationOpening(t, fixture, 1, 1, siblingPath)
		times := clock.require(t, 2)
		if !times[0].Before(times[1]) || !both.firstUnansweredSendSynTime.Equal(times[0]) || both.sendSynCount != 2 {
			t.Fatal("two independent genuine SYN admissions did not retain the earlier health minimum")
		}
		offerSourceResetConnectPacket(t, fixture, firstPath, tcpFlagRst)
		immediate := fixture.await(t, 3, 3, 0)
		requireConnectGenerationReset(t, fixture, first, 2)
		requireConnectGenerationCarrier(t, fixture, immediate)
		clock.require(t, 2)
		if sibling.IsDone() || !immediate.firstUnansweredSendSynTime.Equal(times[1]) {
			t.Fatal("reset of earlier source changed or erased the live sibling's own admission age")
		}
		mature, reason := sourceResetConnectMatureStats(t, fixture, times[1])
		if sibling.IsDone() || !mature.firstUnansweredSendSynTime.Equal(times[1]) || !unansweredConnectSince(mature).Equal(times[1]) || reason != blackholeNoReceiveSyn {
			t.Fatal("live sibling lost its exact mature connect evidence after original telemetry expired")
		}
	})
}

// A reset after real SYN delivery closes the source while its genuine return
// ACK is still withheld. Later success must not resurrect that closed debt.
func TestNativeSourceResetFencesLatePeerAckConnectEvidence(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newResponseBoundaryFixture(t)
		clock := &connectGenerationAdmissionTimes{}
		fixture.base.selected.packetTransferNowForTest = clock.now
		fixture.holdAcks.Store(true)
		path := replyExpectationPath(0, 45000)
		offerSourceResetConnectPacket(t, fixture, path, tcpFlagSyn)
		pending := fixture.await(t, 1, 0, 1)
		requireConnectGenerationCarrier(t, fixture, pending)
		update := requireConnectGenerationOpening(t, fixture, 0, 0, path)
		times := clock.require(t, 1)
		ackDeadline := time.Now().Add(2 * defaultAckCompressTimeout)
		for !fixture.heldAck.Load() {
			if !time.Now().Before(ackDeadline) {
				t.Fatal("real peer did not produce the held ACK")
			}
			time.Sleep(defaultAckCompressTimeout)
			synctest.Wait()
		}
		if !fixture.heldAck.Load() || !pending.firstUnansweredSendSynTime.Equal(times[0]) || pending.sendNackByteCount <= 0 {
			t.Fatal("actual held peer ACK did not preserve its admitted SYN debt")
		}
		offerSourceResetConnectPacket(t, fixture, path, tcpFlagRst)
		closed := fixture.await(t, 2, 0, 2)
		requireConnectGenerationReset(t, fixture, update, 1)
		requireConnectGenerationCarrier(t, fixture, closed)
		fixture.stateLock.Lock()
		pendingBytes := fixture.packets[0].bytes + fixture.packets[1].bytes
		fixture.stateLock.Unlock()
		if closed.sendNackByteCount != ByteCount(pendingBytes) || closed.sendAckCount != 0 {
			t.Fatal("source reset erased real transport debt before peer receipt")
		}
		if !closed.firstUnansweredSendSynTime.IsZero() || !unansweredConnectSince(closed).IsZero() {
			t.Fatal("delivered source reset retained a connect expectation for its closed opening")
		}
		fixture.holdAcks.Store(false)
		completed := awaitConnectGenerationPeerAcks(t, fixture, 2)
		clock.require(t, 1)
		if !completed.firstUnansweredSendSynTime.IsZero() || !unansweredConnectSince(completed).IsZero() || !update.IsDone() {
			t.Fatal("late genuine peer ACK resurrected the reset opening's connect debt")
		}
		mature, reason := sourceResetConnectMatureStats(t, fixture, times[0])
		if !mature.firstUnansweredSendSynTime.IsZero() || !unansweredConnectSince(mature).IsZero() || reason != blackholeNone {
			t.Fatal("reset opening regained a mature verdict after its real late completion")
		}
	})
}
