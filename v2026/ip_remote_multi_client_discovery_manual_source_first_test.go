// Discovery controls which require a manual source winner hold the actual
// already-paid automatic dispatcher, outside every packet/parent/flow lock.
package connect

import (
	"sync"
	"sync/atomic"
	"testing"
)

// One exact source generation owns these test barriers; no production state
// is replaced. Both dispatch and receive still traverse the ordinary pipeline.
type discoveryDemandManualSourceBarrier struct {
	h                  *reraceHarness
	path               *IpPath
	update             *multiClientChannelUpdate
	owner              *tcpSynRetry
	credit             *transferRetainedControlCredit
	serial             uint64
	collapseBefore     uint64
	entered            chan struct{}
	release            chan struct{}
	enteredOnce        sync.Once
	releaseOnce        sync.Once
	dispatchVisits     atomic.Uint32
	retainedGateVisits atomic.Uint32
	sourceGateVisits   atomic.Uint32
	sourceOwnerValid   atomic.Bool
	capturedOwnerValid atomic.Bool
}

// Install before late availability changes or clock advancement. The completed
// original source is settled here, so the immutable hooks have a real HB edge.
func discoveryDemandTestHoldAutomaticRetry(t *testing.T, h *reraceHarness, path *IpPath, update *multiClientChannelUpdate) *discoveryDemandManualSourceBarrier {
	t.Helper()
	h.settle()
	owner := synRetryControlOwner(t, update)
	barrier := &discoveryDemandManualSourceBarrier{h: h, path: path, update: update, owner: owner, credit: owner.credit, serial: owner.serial, collapseBefore: h.multiClient.TcpCollapseDropCount(), entered: make(chan struct{}), release: make(chan struct{})}
	barrier.sourceOwnerValid.Store(true)
	t.Cleanup(barrier.finish)
	h.multiClient.beforeSynRetryDispatchForTest = func(current *multiClientChannelUpdate) {
		if current != update {
			return
		}
		current.stateLock.Lock()
		valid := current.synRetry == owner && owner.update == current && owner.serial == barrier.serial &&
			owner.sequence == path.SequenceNumber && owner.references == 2 && !owner.queued && owner.validWithLock(current)
		current.stateLock.Unlock()
		barrier.capturedOwnerValid.Store(valid)
		barrier.dispatchVisits.Add(1)
		barrier.enteredOnce.Do(func() { close(barrier.entered) })
		select {
		case <-barrier.release:
		case <-h.multiClient.ctx.Done():
		}
	}
	h.multiClient.beforePacketGroupGateForTest = func(current *multiClientChannelUpdate, _ *multiClientChannel, group *parsedPacketGroup) {
		if current != update || !reraceSameFlow(group.ipPath, path) {
			return
		}
		if group.isSynRetry() {
			barrier.retainedGateVisits.Add(1)
		} else {
			barrier.sourceGateVisits.Add(1)
			if group.synRetry == nil || group.synRetry.update != update || group.synRetry.source != h.source ||
				group.synRetry.nativeOwnerSerial != barrier.serial || !group.ipPath.Syn || group.ipPath.Ack || group.ipPath.SequenceNumber != path.SequenceNumber {
				barrier.sourceOwnerValid.Store(false)
			}
		}
	}
	return barrier
}

// Fatal cleanup releases the same real dispatcher before parent teardown.
func (self *discoveryDemandManualSourceBarrier) finish() {
	self.releaseOnce.Do(func() { close(self.release) })
}

// The next manual SYN remains unanswered only because the exact current
// cache+dispatcher references are physically held, not because success is ignored.
func (self *discoveryDemandManualSourceBarrier) requireHeld(t *testing.T) {
	t.Helper()
	entered := false
	select {
	case <-self.entered:
		entered = true
	default:
	}
	self.update.stateLock.Lock()
	held := self.update.synRetry == self.owner && self.owner.serial == self.serial && self.owner.references == 2 &&
		self.owner.update == self.update && !self.owner.queued && self.owner.validWithLock(self.update) &&
		self.update.client.Load() == nil && !self.update.receivedInbound.Load()
	self.update.stateLock.Unlock()
	wire := len(self.h.exit("late").syns(self.path))
	_, answered := self.h.synAck(self.path)
	if !entered || !self.capturedOwnerValid.Load() || self.dispatchVisits.Load() != 1 || !held ||
		self.credit.released.Load() || self.retainedGateVisits.Load() != 0 || wire != 0 || answered || self.h.update(self.path) != self.update {
		self.finish()
		t.Fatalf("DISCOVERY_MANUAL_SOURCE_FIRST_BARRIER_MISSING: entered=%t valid_capture=%t visits=%d held_owner=%t retained_visits=%d wire_syns=%d real_syn_ack=%t", entered, self.capturedOwnerValid.Load(), self.dispatchVisits.Load(), held, self.retainedGateVisits.Load(), wire, answered)
	}
}

// Manual success consumes the cache reference, leaves the held dispatcher
// live, and returns its exact credit only after release/revalidation/join.
func (self *discoveryDemandManualSourceBarrier) requireManualWinnerAndRelease(t *testing.T) {
	t.Helper()
	self.update.stateLock.Lock()
	held := self.update.synRetry == nil && self.owner.update == self.update && self.owner.references == 1
	self.update.stateLock.Unlock()
	wire := self.h.exit("late").syns(self.path)
	packet, answered := self.h.synAck(self.path)
	reply, err := ParseIpPath(packet.packet)
	if !held || self.credit.released.Load() || self.retainedGateVisits.Load() != 0 || self.sourceGateVisits.Load() != 2 || !self.sourceOwnerValid.Load() || self.h.update(self.path) != self.update ||
		self.h.boundTo(self.path) != self.h.channel("late") || !self.update.receivedInbound.Load() ||
		len(wire) != 1 || wire[0].path.SequenceNumber != self.path.SequenceNumber || !answered || err != nil ||
		reply.AckSequenceNumber != self.path.SequenceNumber+1 || self.h.multiClient.TcpCollapseDropCount() != self.collapseBefore {
		self.finish()
		t.Fatal("DISCOVERY_MANUAL_SOURCE_WINNER_OWNERSHIP_CHANGED: source-first ordering lost original cache/dispatch/physical SYN/SYNACK/collapse custody")
	}
	self.finish()
	self.h.settle()
	synRetryControlReleased(t, self.owner)
	if !self.credit.released.Load() || !self.owner.publicationRetired.Load() || self.owner.publicationIndexed || self.dispatchVisits.Load() != 1 || self.retainedGateVisits.Load() != 0 {
		t.Fatal("DISCOVERY_MANUAL_SOURCE_DISPATCH_NOT_RETIRED: released automatic loser retained owner/credit/index or sent duplicate bytes")
	}
}
