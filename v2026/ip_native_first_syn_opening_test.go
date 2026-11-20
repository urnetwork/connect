// First native SYN pressure is separate from retry of a previously written SYN.
// The regressions use real constructor-owned parent workers and queues.
package connect

import (
	"bytes"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// One real ACK source supplies pressure to a distinct, never-materialized flow
// owned by a fully constructed parent. The routing seam chooses only its exit.
type firstSynPressureFixture struct {
	base       *groupDispositionQueueFixture
	harness    *reraceHarness
	path       *IpPath
	source     *SendSequence
	acks       [][]byte
	syn        []byte
	refusals   atomic.Int64
	otherError atomic.Int64
	native     atomic.Int64
	retries    atomic.Int64
	receipts   []receiveAckMessage
}

// A locked channel cut distinguishes actual outstanding ACK traffic from an
// optional refused opening; neither a refusal nor retention is peer progress.
type firstSynHealthCut struct {
	outstanding, acknowledged           int
	outstandingBytes, acknowledgedBytes ByteCount
	pending, lastAck                    time.Time
	generation                          uint64
}

// All compared fields belong to the same existing provider accounting lock.
func (self *firstSynPressureFixture) healthCut() firstSynHealthCut {
	client := self.base.selected
	client.stateLock.Lock()
	defer client.stateLock.Unlock()
	return firstSynHealthCut{
		outstanding: client.packetStats.sendNackCount, outstandingBytes: client.packetStats.sendNackByteCount,
		acknowledged: client.packetStats.sendAckCount, acknowledgedBytes: client.packetStats.sendAckByteCount,
		pending: client.pendingSendTime, lastAck: client.lastSendAckTime, generation: client.pendingSendGeneration,
	}
}

// The parked pressure source owns real outstanding work. A refused first SYN
// preserves that ledger exactly instead of forging either debt or ACK progress.
func (self *firstSynPressureFixture) requireRefusalHealth(t *testing.T, before firstSynHealthCut) {
	t.Helper()
	after := self.healthCut()
	if before.outstanding != len(self.acks) || before.pending.IsZero() || after != before {
		t.Fatalf("raw first refusal changed existing channel progress: before=%+v after=%+v", before, after)
	}
	if _, err := self.base.selected.WindowStats(); err != nil {
		t.Fatalf("raw first refusal penalized a healthy provider: %v", err)
	}
}

// Successful peer receipts leave their real progress timestamp even at zero
// outstanding. The packet ledger and the actual stall classifier must agree;
// an unanswered application SYN is independent of these Transfer receipts.
func (self *firstSynPressureFixture) requirePeerAckIdle(t *testing.T, syns int, before time.Time) {
	t.Helper()
	client := self.base.selected
	stats, err := client.WindowStats()
	if err != nil {
		t.Fatalf("actual peer receipts left a provider error: %v", err)
	}
	wantBytes := ByteCount(syns * len(self.syn))
	for _, packet := range self.acks {
		wantBytes += ByteCount(len(packet))
	}
	cut := self.healthCut()
	if stats.sendAckCount != len(self.acks)+syns || stats.sendAckByteCount != wantBytes ||
		cut.outstanding != 0 || cut.outstandingBytes != 0 || !stats.sendProgressKnown || !hasPeerSendAckInWindow(stats) ||
		cut.lastAck.IsZero() || !cut.pending.Equal(cut.lastAck) || !stats.pendingSendTime.Equal(cut.lastAck) ||
		cut.lastAck.Before(before) || time.Now().Before(cut.lastAck) || client.hasOutstandingSends() ||
		client.sendStalled(time.Nanosecond) || !unacknowledgedSendSince(stats).IsZero() {
		t.Fatalf("actual peer receipts lost healthy idle accounting: ACKs=%d/%d want=%d/%d cut=%+v",
			stats.sendAckCount, stats.sendAckByteCount, len(self.acks)+syns, wantBytes, cut)
	}
	bar := time.Second
	future := cut.lastAck.Add(2 * bar)
	reason, held := blackholeReasonFromStats(future, stats, bar, 0, 3*bar, blackholeGates{})
	if reason != blackholeNone || held != blackholeNone {
		t.Fatalf("historical peer ACK convicted idle Transfer custody: reason=%s held=%s", reason, held)
	}
	// This pure classifier copy changes no source state or timer. Its known
	// outstanding packet must still reach the ordinary no-send-ACK verdict.
	outstanding := *stats
	outstanding.sendNackCount, outstanding.sendNackByteCount = 1, ByteCount(len(self.syn))
	reason, held = blackholeReasonFromStats(future, &outstanding, bar, 0, 3*bar, blackholeGates{})
	if reason != blackholeNoSendAck || held != blackholeNone {
		t.Fatalf("idle-accounting oracle missed real outstanding control: reason=%s held=%s", reason, held)
	}
}

// Constructor policy sets the count; no test mutates a live gate or owner count.
func newFirstSynPressureFixture(t *testing.T, capacity int, port int, configure ...func(*ClientSettings)) *firstSynPressureFixture {
	t.Helper()
	self := &firstSynPressureFixture{}
	self.base = newGroupDispositionQueueFixture(t, nil, func(settings *ClientSettings) {
		settings.SendBufferSettings.SequenceBufferSize = capacity
		previous := settings.SendBufferSettings.SendPackLifecycleObserver
		settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
			previous(event)
			if event.Phase != SendPackLifecyclePhaseTerminal || event.Err == nil {
				return
			}
			var admission *SendPackAdmissionError
			if errors.As(event.Err, &admission) && admission.Boundary == "pack-admission" && admission.Timeout == 0 {
				self.refusals.Add(1)
			} else {
				self.otherError.Add(1)
			}
		}
		for _, apply := range configure {
			apply(settings)
		}
	})
	self.base.selected.settings = self.base.parent.settings
	for index := range capacity + 1 {
		packet := groupRecoveryPacket(100, 500, uint16(64+index), tcpFlagAck, nil)
		if !protocolClassPureTcpControl(packet) {
			t.Fatal("pressure packet is not pure protocol feedback")
		}
		if self.base.offer(packet) == 0 {
			break
		}
		self.acks = append(self.acks, packet)
	}
	if len(self.acks) == 0 || len(self.acks) > capacity {
		t.Fatal("finite pressure must preserve at least one healthy ACK admission")
	}
	self.refusals.Store(0)
	self.otherError.Store(0)
	self.base.requireParked()
	self.source = self.base.update.sequenceClaims.collapseSource
	if self.source == nil || self.source.logicalLane != 0 {
		t.Fatal("ACK pressure has no actual compatibility-lane source")
	}
	self.source.protocolAdmission.mutex.Lock()
	count, limit := self.source.protocolAdmission.count, self.source.protocolAdmission.capacity
	self.source.protocolAdmission.mutex.Unlock()
	self.source.packAdmission.mutex.Lock()
	paidCount := self.source.packAdmission.count
	self.source.packAdmission.mutex.Unlock()
	if count != len(self.acks) || limit != capacity || paidCount != 0 || self.source.resendQueue.Len() != 0 {
		t.Fatal("premise requires raw protocol pressure with no paid or serialized debt")
	}
	self.harness, self.path = synRetryControlHarness(t, port, nil, DefaultProtocolVersion)
	if self.harness.multiClient.flowReaperDone == nil || self.harness.multiClient.sendClientPathForTest != nil ||
		self.harness.update(self.path) != nil {
		t.Fatal("first SYN must start without an update under the real parent lifecycle")
	}
	self.syn = reraceTcpPacket(self.path, tcpFlagSyn, self.path.SequenceNumber, 0, nil)
	if !protocolClassPureTcpControl(self.syn) || reraceSameFlow(groupRecoveryParsed(t, self.acks[0]).ipPath, self.path) {
		t.Fatal("SYN must be a distinct pure-control flow")
	}
	self.harness.multiClient.groupRaceCandidatesForTest = func(group *parsedPacketGroup) []*multiClientChannel {
		if reraceSameFlow(group.ipPath, self.path) {
			return []*multiClientChannel{self.base.selected}
		}
		return []*multiClientChannel{self.harness.channel("a"), self.harness.channel("b")}
	}
	self.harness.multiClient.beforePacketGroupGateForTest = func(_ *multiClientChannelUpdate, _ *multiClientChannel, group *parsedPacketGroup) {
		if reraceSameFlow(group.ipPath, self.path) && group.ipPath.Syn && !group.ipPath.Ack {
			if group.isSynRetry() {
				self.retries.Add(1)
			} else {
				self.native.Add(1)
			}
		}
	}
	return self
}

// The actual native public entry consumes its original; a witness verifies
// final release after both independently constructed owners have joined.
func (self *firstSynPressureFixture) offer(t *testing.T) bool {
	t.Helper()
	packet := MessagePoolCopy(self.syn)
	self.base.witnesses = append(self.base.witnesses, MessagePoolShareReadOnly(packet))
	return synRetryControlOffer(t, self.harness, packet)
}

// Decode actual route writes before applying ordinary compact peer receipts.
// A bounded drain permits newly runnable work but never advances a timer.
func (self *firstSynPressureFixture) drain(t *testing.T, acknowledge bool) (int, int) {
	t.Helper()
	acks, syns := 0, 0
	for round := 0; round < 16; round++ {
		synctest.Wait()
		if len(self.base.route) == 0 {
			return acks, syns
		}
		for len(self.base.route) != 0 {
			wire := <-self.base.route
			pack := decodeSendPackLifecycleWirePack(t, wire)
			if pack.Nack || pack.LogicalLane != self.source.logicalLane {
				MessagePoolReturn(wire)
				t.Fatal("wire departed the reliable shared source lane")
			}
			sequenceId, sequenceErr := IdFromBytes(pack.SequenceId)
			messageId, messageErr := IdFromBytes(pack.MessageId)
			if sequenceErr != nil || messageErr != nil || sequenceId != self.source.sequenceId {
				MessagePoolReturn(wire)
				t.Fatal("wire belongs to a different source generation")
			}
			for _, frame := range pack.Frames {
				if bytes.Equal(frame.MessageBytes, self.syn) {
					if pack.DeliverImmediately {
						MessagePoolReturn(wire)
						t.Fatal("pure first SYN unexpectedly acquired immediate-delivery policy")
					}
					syns++
					continue
				}
				found := false
				for _, ack := range self.acks {
					found = found || bytes.Equal(frame.MessageBytes, ack)
				}
				if !found {
					MessagePoolReturn(wire)
					t.Fatal("wire contains an unowned pressure packet")
				}
				acks++
			}
			MessagePoolReturn(wire)
			receipt := receiveAckMessage{
				sequenceId: sequenceId, messageId: messageId,
			}
			if !acknowledge {
				self.receipts = append(self.receipts, receipt)
			} else if !self.base.client.sendBuffer.ackMessage(self.base.peer, receipt, 0) {
				t.Fatal("actual serialized original rejected its peer receipt")
			}
		}
		if !acknowledge {
			return acks, syns
		}
	}
	t.Fatal("bounded wire drain did not settle")
	return 0, 0
}

// The minimum protocol capacity must continue to carry ordinary feedback
// after the setup obligation retires; a reservation cannot consume its quota.
func (self *firstSynPressureFixture) requireLaterAck(t *testing.T) {
	t.Helper()
	packet := groupRecoveryPacket(100, 701, 96, tcpFlagAck, nil)
	self.acks = append(self.acks, packet)
	if self.base.offer(packet) != 1 {
		t.Fatal("healthy later ACK lost admission after first SYN recovery")
	}
	acks, syns := self.drain(t, true)
	if acks != 1 || syns != 0 {
		t.Fatalf("later feedback changed setup custody: ACKs=%d SYNs=%d", acks, syns)
	}
}

// A bounded public ACK fill permits either reservation or optional retention.
// One first native offer must progress at the actual source opening; the
// caller's original receipt is never retrospectively changed to success.
func testFirstSynRawProtocolOpening(t *testing.T, capacity int, port int) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newFirstSynPressureFixture(t, capacity, port)
		before := time.Now()
		health := fixture.healthCut()
		accepted := fixture.offer(t)
		if !accepted {
			fixture.requireRefusalHealth(t, health)
		}
		update := fixture.harness.update(fixture.path)
		if update == nil || update == fixture.base.update || fixture.native.Load() != 1 || fixture.otherError.Load() != 0 {
			t.Fatal("first native offer did not use its distinct real-parent flow")
		}
		fixture.base.client.sendBuffer.mutex.Lock()
		sources := 0
		for id, source := range fixture.base.client.sendBuffer.sendSequences {
			if id.Destination == fixture.base.peer {
				sources++
				if source != fixture.source {
					fixture.base.client.sendBuffer.mutex.Unlock()
					t.Fatal("first SYN selected a different source than the ACK pressure")
				}
			}
		}
		fixture.base.client.sendBuffer.mutex.Unlock()
		if sources != 1 || len(fixture.base.route) != 0 || time.Since(before) != 0 {
			t.Fatal("pressure premise changed source, wire state or native clock")
		}
		fixture.base.unpark()
		acks, syns := fixture.drain(t, true)
		if acks != len(fixture.acks) || syns > 1 || time.Since(before) != 0 {
			t.Fatal("actual opening lost ACK work or required timer progress")
		}
		if syns == 1 {
			if fixture.native.Load() != 1 || fixture.harness.boundTo(fixture.path) != fixture.base.selected {
				t.Fatal("automatic first-SYN recovery changed native input or selection")
			}
			fixture.requireLaterAck(t)
			fixture.requirePeerAckIdle(t, 1, before)
			t.Logf("FIRST_SYN_PROTOCOL_READY capacity=%d native_offers=1 accepted=%t timer_advance=0", capacity, accepted)
			return
		}
		update.stateLock.Lock()
		cacheAbsent, clockEmpty := update.synRetry == nil, update.synWaitStart.IsZero()
		update.stateLock.Unlock()
		fixture.source.protocolAdmission.mutex.Lock()
		remaining := fixture.source.protocolAdmission.count
		fixture.source.protocolAdmission.mutex.Unlock()
		if accepted || fixture.refusals.Load() != 1 || !cacheAbsent || !clockEmpty || remaining != 0 || fixture.retries.Load() != 0 {
			t.Fatal("missing SYN was not the exact first-offer raw protocol refusal")
		}
		// This explicit second native offer is a source-retry control, not a
		// claim that the fixture models an application's actual TCP RTO.
		time.Sleep(time.Second)
		if !fixture.offer(t) {
			t.Fatal("open source refused the explicit native retry control")
		}
		acks, syns = fixture.drain(t, true)
		if acks != 0 || syns != 1 || fixture.native.Load() != 2 || time.Since(before) != time.Second ||
			fixture.harness.boundTo(fixture.path) != fixture.base.selected {
			t.Fatal("explicit source retry failed to restore the same real source path")
		}
		t.Fatalf("FIRST_NATIVE_SYN_PROTOCOL_OPENING_LOST: capacity=%d first_offer_refused=true cache_absent=true actual_slots_reopened=true native_retry_control=1s; no autonomous progress at release", capacity)
	})
}

// Several ordinary ACKs cannot force a new flow to await another native offer.
func TestNativeFirstSynProtocolOpeningReoffersWithoutSourceTimer(t *testing.T) {
	testFirstSynRawProtocolOpening(t, 3, 51401)
}

// The minimum configured capacity is an explicit policy boundary, not spare room.
func TestNativeFirstSynProtocolOpeningCapacityOneReoffersWithoutSourceTimer(t *testing.T) {
	testFirstSynRawProtocolOpening(t, 1, 51402)
}

// A pure SYN remains ordered and does not consume the ACK/SYNACK-specific
// immediate count. Actual reliable wire custody still ends only on a receipt.
func TestNativeFirstSynSerializedAckPressurePreservesOrderedProgress(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newFirstSynPressureFixture(t, 3, 51403)
		fixture.base.unpark()
		acks, syns := fixture.drain(t, false)
		if acks != len(fixture.acks) || syns != 0 {
			t.Fatal("initial pressure ACKs did not serialize")
		}
		// The first ready drain may coalesce the three queued ACKs. Fill each
		// remaining serialized slot with one offer/write cycle, holding every
		// actual receipt; the count is never inferred from native packet count.
		for round := 0; fixture.source.immediateSendCount < 3 && round < 3; round++ {
			ack := groupRecoveryPacket(100, uint32(600+round), uint16(80+round), tcpFlagAck, nil)
			fixture.acks = append(fixture.acks, ack)
			if fixture.base.offer(ack) != 1 {
				t.Fatal("sequential ACK failed to occupy a serialized immediate slot")
			}
			writtenAcks, writtenSyns := fixture.drain(t, false)
			if writtenAcks != 1 || writtenSyns != 0 {
				t.Fatal("sequential ACK occupancy did not follow an actual wire write")
			}
		}
		fixture.source.protocolAdmission.mutex.Lock()
		rawCount := fixture.source.protocolAdmission.count
		fixture.source.protocolAdmission.mutex.Unlock()
		if rawCount != 0 || fixture.source.immediateSendCount != 3 || fixture.source.immediateSendLimit != 3 {
			t.Fatal("premise requires serialized ACK custody with vacant raw protocol slots")
		}
		before := time.Now()
		if !fixture.offer(t) || fixture.refusals.Load() != 0 || fixture.otherError.Load() != 0 {
			t.Fatal("spare raw slot did not admit the first native SYN")
		}
		if elapsed := time.Since(before); elapsed != 0 {
			t.Fatalf("first SYN required a timer while raw slots were vacant: %s", elapsed)
		}
		acks, syns = fixture.drain(t, false)
		if acks != 0 || syns != 1 || fixture.source.immediateSendCount != 3 {
			t.Fatalf("ordered first SYN changed ACK/SYNACK-specific custody: ACKs=%d SYNs=%d immediate=%d want=3",
				acks, syns, fixture.source.immediateSendCount)
		}
		for _, receipt := range fixture.receipts {
			if !fixture.base.client.sendBuffer.ackMessage(fixture.base.peer, receipt, 0) {
				t.Fatal("held real ACK original rejected its peer receipt")
			}
		}
		fixture.receipts = nil
		acks, syns = fixture.drain(t, true)
		if acks != 0 || syns != 0 || fixture.native.Load() != 1 || fixture.retries.Load() != 0 ||
			time.Since(before) != 0 || fixture.source.immediateSendCount != 0 {
			t.Fatalf("actual ordered receipts failed to retire pressure custody: ACKs=%d SYNs=%d native=%d retries=%d immediate=%d elapsed=%s",
				acks, syns, fixture.native.Load(), fixture.retries.Load(), fixture.source.immediateSendCount, time.Since(before))
		}
		fixture.requirePeerAckIdle(t, 1, before)
		t.Log("FIRST_NATIVE_SYN_SERIALIZED_CONTROL raw_admission=accepted ordered_SYN_before_receipt=1 ACK_immediate_count_before=3 after_receipts=0 native_offers=1 timer_advance=0")
	})
}
