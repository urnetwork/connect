// Canonical generations and conservative exhaustion preserve exact SYN debt.
// Every integration input traverses the original native and real peer paths.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// The paired source keeps its actual carrier/peer, while canonical lookup and
// one explicit candidate replace the old fixture's tuple-routing seam.
func newCanonicalConnectGenerationFixture(t *testing.T) *replyExpectationFixture {
	t.Helper()
	fixture := newResponseBoundaryFixture(t)
	parent := fixture.base.parent
	parent.stateLock.Lock()
	parent.sendClientPathForTest = nil
	parent.ip4PathUpdates = map[Ip4Path]*multiClientChannelUpdate{}
	parent.ip6PathUpdates = map[Ip6Path]*multiClientChannelUpdate{}
	parent.affinityIp4Paths = map[Ip4Path]map[Ip4Path]time.Time{}
	parent.affinityIp6Paths = map[Ip6Path]map[Ip6Path]time.Time{}
	parent.flowUpdates = map[*multiClientChannelUpdate]bool{}
	parent.stateLock.Unlock()
	fixture.base.selected.healthy = true
	fixture.base.selected.args.DestinationStats.IpFamily = IpFamilyDualstack
	parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel { return []*multiClientChannel{fixture.base.selected} }
	t.Cleanup(func() {
		parent.stateLock.Lock()
		updates := make([]*multiClientChannelUpdate, 0, len(parent.flowUpdates))
		for update := range parent.flowUpdates {
			updates = append(updates, update)
		}
		parent.stateLock.Unlock()
		for _, update := range updates {
			update.Close()
		}
		fixture.base.selected.closeConnectSynOwners()
	})
	return fixture
}

// Materialized generation fields belong to the live update lock, even after
// the independent peer receipt has completed the packet's transfer owner.
func canonicalConnectGenerationMatches(update *multiClientChannelUpdate, sequence uint32) bool {
	if update == nil {
		return false
	}
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	return !update.IsDone() && update.synGenerationSeen && update.synGenerationNumber == sequence
}

// Old and new native flows share the real tuple and ISN but have distinct
// canonical owners. Generated control from the retired owner cannot acquit it.
func TestNativeCanonicalReplacementKeepsConnectGeneration(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newCanonicalConnectGenerationFixture(t)
		clock := &connectGenerationAdmissionTimes{}
		fixture.base.selected.packetTransferNowForTest = clock.now
		path := replyExpectationPath(0, 45000)
		offerSourceResetConnectPacket(t, fixture, path, tcpFlagSyn)
		firstStats := fixture.await(t, 1, 1, 0)
		old := fixture.base.parent.receiveUpdate(path)
		if !canonicalConnectGenerationMatches(old, path.SequenceNumber) {
			t.Fatal("first canonical native SYN did not materialize its actual owner")
		}
		firstTimes := clock.require(t, 1)
		if !firstStats.firstUnansweredSendSynTime.Equal(firstTimes[0]) {
			t.Fatal("first canonical health age was not its actual admission")
		}
		offerSourceResetConnectPacket(t, fixture, path, tcpFlagRst)
		closed := fixture.await(t, 2, 2, 0)
		if !old.IsDone() || !closed.firstUnansweredSendSynTime.IsZero() {
			t.Fatal("actual old source reset did not retire its own canonical obligation")
		}
		time.Sleep(time.Nanosecond)
		offerSourceResetConnectPacket(t, fixture, path, tcpFlagSyn)
		fresh := fixture.await(t, 3, 3, 0)
		current := fixture.base.parent.receiveUpdate(path)
		times := clock.require(t, 2)
		if current == old || !canonicalConnectGenerationMatches(current, path.SequenceNumber) ||
			!times[0].Before(times[1]) || !fresh.firstUnansweredSendSynTime.Equal(times[1]) {
			t.Fatal("same-tuple same-ISN native replacement did not earn its own actual health age")
		}
		reset, ok := ipOosRst(path)
		if !ok {
			t.Fatal("old native owner could not construct its stale generated reset")
		}
		accepted := old.sendAffinityControl(fixture.base.selected, reset, 0)
		acknowledged := 3
		if accepted {
			acknowledged++
		}
		after := fixture.await(t, acknowledged, acknowledged, 0)
		clock.require(t, 2)
		if fixture.base.parent.receiveUpdate(path) != current || current.IsDone() || !after.firstUnansweredSendSynTime.Equal(times[1]) {
			t.Fatal("retired owner control cleared or replaced the fresh canonical connect generation")
		}
		mature, reason := sourceResetConnectMatureStats(t, fixture, times[1])
		if current.IsDone() || !mature.firstUnansweredSendSynTime.Equal(times[1]) || reason != blackholeNoReceiveSyn {
			t.Fatal("new canonical native opening lost its exact mature debt after stale source control")
		}
	})
}

// One native batch can contain several source controls; an unowned composite
// fallback must not interpret the first retired SYN as acquittal of its tail.
func TestNativeMixedResetAndSynKeepsLiveConnectDebt(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newResponseBoundaryFixture(t)
		path := replyExpectationPath(0, 45000)
		first := reraceTcpPacket(path, tcpFlagSyn, path.SequenceNumber, 0, nil)
		reset := reraceTcpPacket(path, tcpFlagRst, path.SequenceNumber+1, 0, nil)
		next := reraceTcpPacket(path, tcpFlagSyn, path.SequenceNumber+100, 0, nil)
		if fixture.base.offer(first, reset, next) != 3 {
			t.Fatal("actual mixed control batch was not admitted")
		}
		stats := fixture.await(t, 3, 3, 0)
		fixture.stateLock.Lock()
		update := fixture.updates[0]
		packets := fixture.packets
		fixture.stateLock.Unlock()
		if !canonicalConnectGenerationMatches(update, path.SequenceNumber+100) ||
			!packets[0].syn || !packets[1].rst || !packets[2].syn || stats.sendSynCount != 2 || stats.firstUnansweredSendSynTime.IsZero() ||
			stats.receiveAckCount != 0 || stats.receiveSynCount != 0 {
			t.Fatal("real SYN/reset/SYN source did not leave its final native opening live")
		}
		mature, reason := sourceResetConnectMatureStats(t, fixture, stats.firstUnansweredSendSynTime)
		if update.IsDone() || mature.firstUnansweredSendSynTime.IsZero() || reason != blackholeNoReceiveSyn {
			t.Fatal("composite source controls falsely acquitted the later actual SYN")
		}
	})
}

// Reserve the real bounded optional metadata quota without manufacturing a
// packet receipt. Exhaustion must not alter native admission or erase debt.
func TestNativeConnectOwnerExhaustionKeepsConservativeEvidence(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newResponseBoundaryFixture(t)
		quota := &connectSynOwnerQuota{}
		quota.state.Store(1) // actual channel lease of this private quota
		held := 0
		for quota.reserveOwner() {
			held++
		}
		if held != connectSynOwnerMaxCount {
			t.Fatal("optional owner quota did not enforce its exact fixed bound")
		}
		fixture.base.selected.connectSynQuota.Store(quota)
		t.Cleanup(func() {
			for index := 0; index < held; index++ {
				quota.releaseOwner()
			}
			fixture.base.selected.closeConnectSynOwners()
		})
		path, update, started := sourceResetConnectOpening(t, fixture)
		stats := sourceResetConnectClose(t, fixture, path, update, 2)
		if stats.firstUnansweredSendSynTime.IsZero() || stats.sendAckCount != 2 || stats.sendNackCount != 0 {
			t.Fatal("exhausted optional metadata changed real native admission or silently acquitted unknown evidence")
		}
		mature, reason := sourceResetConnectMatureStats(t, fixture, started)
		if mature.firstUnansweredSendSynTime.IsZero() || reason != blackholeNoReceiveSyn {
			t.Fatal("conservative metadata-exhaustion fallback disabled the existing connect detector")
		}
	})
}
