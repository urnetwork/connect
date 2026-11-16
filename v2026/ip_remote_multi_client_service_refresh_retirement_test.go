// The full-exhaustion path keeps the original paid source through a held
// listing and actual handover; ordinary membership invalidation remains active.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

type serviceRefreshRetirementFixture struct {
	h              *reraceHarness
	generator      *discoveryDemandTestGenerator
	window         *multiClientWindow
	path           *IpPath
	siblingPaths   []*IpPath
	siblingOwners  []*multiClientChannel
	victim         *multiClientChannel
	update         *multiClientChannelUpdate
	owner          *tcpSynRetry
	credit         *transferRetainedControlCredit
	stamp          uint64
	serial         uint64
	started        time.Time
	attemptStarted time.Time
	reads          int
	mints          int
	publication    <-chan struct{}
}

func newServiceRefreshRetirementFixture(t *testing.T, port int) *serviceRefreshRetirementFixture {
	t.Helper()
	h, generator, window, path, paths, owners := newFullServiceRefreshFixture(t, port)
	f := &serviceRefreshRetirementFixture{h: h, generator: generator, window: window, path: path, siblingPaths: paths, siblingOwners: owners}
	for _, client := range window.unorderedClients() {
		if h.multiClient.clientFlowCount(client) == 0 {
			if f.victim != nil {
				t.Fatal("full-exhaustion retirement control has more than one actual idle victim")
			}
			f.victim = client
		}
	}
	if f.victim == nil || len(window.unorderedClients()) != 5 {
		t.Fatal("full-exhaustion retirement control lacks five owners and one idle victim")
	}
	f.reads, _, f.mints, _ = generator.counts()
	f.publication = discoveryDemandTestObservePublication(window)
	f.started = time.Now()
	if !privateCrossWindowNativeSyn(h, path) {
		t.Fatal("full-exhaustion retirement original native source was not admitted")
	}
	f.update = h.update(path)
	if f.update == nil {
		t.Fatal("full-exhaustion retirement source has no canonical update")
	}
	f.owner = discoveryDemandServiceRefreshOwner(t, window, f.update)
	f.update.stateLock.Lock()
	valid := len(f.update.dialFailedClients) == 5 && f.update.synRetryRemoteTailRefusedWithLock(f.victim) &&
		f.update.client.Load() == nil && !f.update.receivedInbound.Load() && !f.update.sourceReset.Load()
	f.stamp, f.serial, f.attemptStarted, f.credit = f.update.discoveryAttemptStamp.Load(), f.owner.serial, f.update.discoveryAttemptStarted, f.owner.credit
	f.update.stateLock.Unlock()
	if !valid || h.metrics().DialFailuresIntercepted != 5 {
		t.Fatal("full-exhaustion retirement lacks five real same-service refusals on the original source")
	}
	return f
}

// The late provider answers its real control ping but holds the target reply.
// Availability sends no notification; the unchanged native offer wakes listing.
func (self *serviceRefreshRetirementFixture) triggerLate(t *testing.T) {
	t.Helper()
	self.generator.mutex.Lock()
	self.generator.holdNextAvailable = true
	self.generator.mutex.Unlock()
	self.h.sleep(self.started.Add(733 * time.Millisecond).Sub(time.Now()))
	self.h.addExit(reraceExitSpec{name: "retirement-late", answer: func(path *IpPath) reraceAnswer {
		if reraceSameFlow(path, self.path) {
			return reraceSilent
		}
		return reraceAccept
	}})
	self.h.sleep(self.started.Add(1109 * time.Millisecond).Sub(time.Now()))
	// All original contenders actually refused; acceptance may differ from
	// the partial-refusal control's still-live persistent collapse race.
	accepted := privateCrossWindowNativeSyn(self.h, self.path)
	t.Logf("full-exhaustion retirement unchanged native offer: accepted=%t age=%s", accepted, time.Since(self.started))
	if !discoveryDemandTestAwaitPublication(t, self.generator.availableEntered, self.started.Add(2500*time.Millisecond)) {
		t.Fatal("full-exhaustion source never reached its held available listing")
	}
	// The synchronous native offer finishes before publication can expose a
	// fresh singleton to affinity reuse. The target still has only paid replay.
	if self.h.update(self.path) != self.update || self.update.IsDone() {
		t.Fatal("full-exhaustion native offer replaced its source before actual publication")
	}
	close(self.generator.availableRelease)
}

// Completion is observed after the caller has released any held retirement.
func (self *serviceRefreshRetirementFixture) awaitLate(t *testing.T) {
	t.Helper()
	if !discoveryDemandTestAwaitPublication(t, self.publication, self.started.Add(2500*time.Millisecond)) {
		t.Fatal("full-exhaustion retirement did not publish its actual evaluated alternative")
	}
	self.h.settle()
}

func (self *serviceRefreshRetirementFixture) publishLate(t *testing.T) {
	t.Helper()
	self.triggerLate(t)
	self.awaitLate(t)
}

func (self *serviceRefreshRetirementFixture) requireOriginalAttempt(t *testing.T) {
	t.Helper()
	self.update.stateLock.Lock()
	stamp := self.update.discoveryAttemptStamp.Load()
	owner := self.update.synRetry
	valid := owner == self.owner && owner != nil && owner.serial == self.serial && owner.packet != nil && owner.credit == self.credit &&
		!self.credit.released.Load() && owner.validGenerationWithLock(self.update) &&
		self.update.discoveryAttemptStarted.Equal(self.attemptStarted) && self.update.dialFailedSynSeen && self.update.dialFailedSyn == self.path.SequenceNumber
	census := map[string]any{"same_owner": owner == self.owner, "owner_present": owner != nil, "credit_released": self.credit.released.Load(),
		"update_done": self.update.IsDone(), "source_reset": self.update.sourceReset.Load(), "received": self.update.receivedInbound.Load(),
		"same_attempt_time": self.update.discoveryAttemptStarted.Equal(self.attemptStarted), "syn_seen": self.update.dialFailedSynSeen,
		"same_syn": self.update.dialFailedSyn == self.path.SequenceNumber, "stamp_before": self.stamp, "stamp_after": stamp}
	if owner != nil {
		census["same_serial"], census["packet_present"], census["same_credit"], census["generation_valid"], census["references"] = owner.serial == self.serial, owner.packet != nil, owner.credit == self.credit, owner.validGenerationWithLock(self.update), owner.references
	}
	self.update.stateLock.Unlock()
	census["victim_done"], census["victim_installed"], census["installed_count"] = self.victim.IsDone(), strictHandoverTestInstalled(self.window, self.victim), len(self.window.unorderedClients())
	census["same_current_update"] = self.h.update(self.path) == self.update
	t.Logf("AGGREGATE_OWN_RETIREMENT_PREMISES %v", census)
	if !valid || self.h.update(self.path) != self.update || !self.victim.IsDone() || strictHandoverTestInstalled(self.window, self.victim) || len(self.window.unorderedClients()) != 5 {
		t.Fatal("own-victim retirement lost its paid source, genuine retirement or exact hard ceiling")
	}
	reads, _, mints, _ := self.generator.counts()
	if reads > self.reads+3 || mints != self.mints+1 {
		t.Fatal("own-victim retirement exceeded the existing listing/mint authority")
	}
	// Provider removal may advance the membership stamp. The behavioral proof
	// is the still-current paid source and its actual replay/answer below.
}

// This adds no early-list policy. The canonical all-five-refused eligibility
// reaches the same actual swap, then only a real provider SYN/ACK completes it.
func TestFullServiceRefreshOwnRetirementPreservesPaidSourceAttempt(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newServiceRefreshRetirementFixture(t, 53320)
		f.publishLate(t)
		f.requireOriginalAttempt(t)
		late := f.h.channel("retirement-late")
		wire := f.h.exit("retirement-late").syns(f.path)
		if len(wire) != 1 || wire[0].path.SequenceNumber != f.path.SequenceNumber {
			t.Fatalf("SERVICE_REFRESH_OWN_RETIREMENT_MISSING_ORIGINAL_WIRE_SYN: actual=%d", len(wire))
		}
		f.h.exit("retirement-late").sendTo(wire[0].source, reraceSynAck(wire[0].path))
		f.h.settle()
		reply, answered := f.h.synAck(f.path)
		if !answered || f.h.boundTo(f.path) != late || f.h.update(f.path) != f.update {
			t.Fatal("own-retirement alternative did not deliver the actual unchanged navigation SYN/ACK")
		}
		parsed, err := ParseIpPath(reply.packet)
		if err != nil || parsed.AckSequenceNumber != f.path.SequenceNumber+1 || !f.credit.released.Load() {
			t.Fatal("own-retirement recovery did not acknowledge/release the exact original paid source")
		}
		discoveryDemandFullWindowAssertSiblings(t, f.h, f.siblingPaths, f.siblingOwners)
		reraceAssertNoErrorDelivered(t, f.h, f.path)
	})
}
