// Pure controls and payload-bearing SYNs use different byte-admission policies,
// while both retain actual public ownership, source writes and peer ACKs.
package connect

import (
	"testing"
	"testing/synctest"
)

// A payload-bearing SYN still requires DATA permission and preserves exact
// retry eligibility after an actual wholly-unwritten retention refusal.
func TestColdSynPayloadRetentionRefusalRetriesExactBytes(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(1)
		failed := newGroupDispositionQueueFixture(t, budget, nil)
		owned := newGroupDispositionQueueFixture(t, nil, nil)
		failed.update.client.Store(nil)
		failed.parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			return []*multiClientChannel{failed.selected, owned.selected}
		}
		syn := groupRecoveryPacket(100, 0, 64, tcpFlagSyn, []byte("synthetic opening payload"))
		if count, accepted := failed.offerWithResults(syn); count != 1 || !accepted[0] {
			t.Fatal("initial exact SYN was not admitted by both source queues")
		}
		failed.requireParked()
		owned.requireParked()
		failed.unpark()
		synctest.Wait()
		if len(failed.route) != 0 || failed.started.Load() != 1 || failed.terminal.Load() != 1 || budget.UsedByteCount() != 0 || failed.update.race == nil {
			t.Fatal("first candidate did not complete the actual wholly-unwritten source failure")
		}
		budget.SetTotalByteCount(kib(128))
		synctest.Wait()
		if count, accepted := failed.offerWithResults(syn); count != 1 || !accepted[0] {
			t.Fatal("failed candidate could not immediately acquire the exact retry after real capacity publication")
		}
		failed.acknowledgeAll(syn)
		owned.unpark()
		owned.acknowledgeAll(syn)
		if failed.started.Load() != 2 || owned.started.Load() != 1 || failed.offer(syn) != 0 {
			t.Fatal("retry duplicated the still-owned candidate or failed to install the recovered candidate's own gate")
		}
		failed.update.stateLock.Lock()
		failed.update.commitRaceClientWithLock(failed.selected)
		failed.update.stateLock.Unlock()
		failed.update.releaseCollapseClaims()
		if budget.UsedByteCount() != 0 || failed.update.sequenceClaims != nil {
			t.Fatal("recovered winner retained a discarded candidate or range owner")
		}
	})
}

// The real retained DATA original fills its leaf. A later pure SYN writes
// without borrowing or refunding that original, and both need their own ACK.
func TestColdSynPureControlProgressesAtFullDataBudget(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(kib(128))
		var groups []*parsedPacketGroup
		f := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
				groups = append(groups, target.(*parsedPacketGroup))
			}
		})
		data := groupRecoveryPacket(101, 0, 64, tcpFlagAck, []byte("retained DATA owner"))
		syn := groupDispositionControlPacket(900, 0, 64, tcpFlagSyn)
		if f.offer(data) != 1 {
			t.Fatal("DATA predecessor did not reach public source ownership")
		}
		f.requireParked()
		f.unpark()
		synctest.Wait()
		used := budget.UsedByteCount()
		if used <= 0 || len(f.route) != 1 || f.started.Load() != 1 || f.terminal.Load() != 0 {
			t.Fatal("DATA predecessor did not retain its actual unacknowledged wire owner")
		}
		budget.SetTotalByteCount(used)
		f.client.RouteManager().UpdateTransport(f.transport, []Route{f.route})
		synctest.Wait()
		if budget.Available() != 0 {
			t.Fatal("actual retained DATA did not fill the configured leaf")
		}
		if count, accepted := f.offerWithResults(syn); count != 1 || !accepted[0] {
			t.Fatal("pure SYN inherited its predecessor's full DATA admission")
		}
		synctest.Wait()
		if len(f.route) != 2 || f.started.Load() != 2 || f.terminal.Load() != 0 ||
			budget.UsedByteCount() != used || len(groups) != 2 || groups[1].collapseOwner == nil ||
			groups[1].collapseOwner.budget != nil || groups[1].collapseOwner.budgetBytes <= 0 {
			t.Fatal("pure SYN did not retain its own uncharged graph and reliable wire without releasing DATA")
		}
		f.acknowledgeAll(data, syn)
		if f.terminal.Load() != 2 || f.update.sequenceClaims != nil || budget.UsedByteCount() != 0 || f.offer(syn) != 0 {
			t.Fatal("actual DATA/control ACKs did not settle exact ownership and the SYN duplicate gate")
		}
	})
}
