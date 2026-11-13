// Pending inherited openings retain only their exact selection and real source
// lifetime. These controls reuse the actual public refusal and provider fixture.
package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The control keeps identity receipts, never extra packet or budget ownership.
type inheritedSynOpeningCredits struct {
	owner                     *tcpSynRetry
	packetCredit, readyCredit *transferRetainedControlCredit
}

// Both allocations must be paid before the raw source can become available.
func (self *inheritedSynOpeningFixture) pending(t *testing.T) inheritedSynOpeningCredits {
	t.Helper()
	self.update.stateLock.Lock()
	defer self.update.stateLock.Unlock()
	owner := self.update.synRetry
	if owner == nil || owner.openingSerial == 0 || owner.openingSelection != self.selection ||
		owner.credit == nil || owner.credit.noBudget || owner.credit.byteCount <= 0 ||
		owner.readiness == nil || owner.readiness.credit == nil || owner.readiness.credit.noBudget ||
		owner.readiness.credit.byteCount <= 0 || !owner.readiness.indexed.Load() || owner.readiness.count != 1 ||
		owner.references != 1 || owner.queued || self.update.synGenerationSeen || !self.update.synWaitStart.IsZero() ||
		self.budget.UsedByteCount() != owner.credit.byteCount+owner.readiness.credit.byteCount {
		t.Fatal("inherited first refusal did not retain exactly paid pending custody")
	}
	return inheritedSynOpeningCredits{owner: owner, packetCredit: owner.credit, readyCredit: owner.readiness.credit}
}

// No stale setup packet, false Transfer ACK, provider strike or optional byte
// reservation may survive an invalidated pending opening.
func (self *inheritedSynOpeningFixture) requireRetired(t *testing.T, credits inheritedSynOpeningCredits) {
	t.Helper()
	self.harness.settle()
	self.update.stateLock.Lock()
	cachePresent := self.update.synRetry != nil
	self.update.stateLock.Unlock()
	if cachePresent || !credits.packetCredit.released.Load() || !credits.readyCredit.released.Load() || self.budget.UsedByteCount() != 0 {
		t.Fatalf("INHERITED_OPENING_CUSTODY_NOT_RETIRED: cache=%t packet_released=%t readiness_released=%t budget=%d",
			cachePresent, credits.packetCredit.released.Load(), credits.readyCredit.released.Load(), self.budget.UsedByteCount())
	}
	synRetryControlReleased(t, credits.owner)
	if len(self.harness.exit("a").syns(self.path)) != 0 || len(self.harness.delivered(self.path)) != 0 || time.Since(self.before) != 0 {
		t.Fatal("invalidated inherited opening reached the provider or required a timer")
	}
	stats, err := self.selected.WindowStats()
	self.selected.stateLock.Lock()
	endErr, nackCount, nackBytes := self.selected.endErr, self.selected.packetStats.sendNackCount, self.selected.packetStats.sendNackByteCount
	self.selected.stateLock.Unlock()
	if err != nil || stats.sendNackCount != 0 || stats.sendNackByteCount != 0 || self.selected.dialFailureCount() != 0 || self.selected.IsDone() {
		t.Fatalf("local opening cancellation penalized or left debt on the healthy provider: stats_error=%v end_error=%v nack_count=%d nack_bytes=%d dial_failures=%d done=%t",
			err, endErr, nackCount, nackBytes, self.selected.dialFailureCount(), self.selected.IsDone())
	}
}

// Actual echo traffic on the already-established donor remains runnable.
func (self *inheritedSynOpeningFixture) requireDonorProgress(t *testing.T) {
	t.Helper()
	before := len(self.harness.delivered(self.donorPath))
	accepted := self.harness.send(reraceSegment(self.donorPath, 0, []byte("still live")))
	after := len(self.harness.delivered(self.donorPath))
	selection := self.donor.client.loadSelection()
	if !accepted || after != before+1 || selection != self.selection || !self.donor.receivedInbound.Load() {
		t.Fatalf("retiring the optional opening damaged its established sibling: accepted=%t delivered_before=%d delivered_after=%d same_selection=%t same_provider=%t received_inbound=%t donor_done=%t source_error=%v",
			accepted, before, after, selection == self.selection, self.donor.client.Load() == self.selected,
			self.donor.receivedInbound.Load(), self.donor.IsDone(), self.source.ctx.Err())
	}
}

// A provider pointer is insufficient: replacing its immutable binding receipt
// invalidates the old pending opening even when the provider itself is unchanged.
func TestNativeFirstSynInheritedAffinitySelectionChangeRetiresOpening(t *testing.T) {
	testNativeFirstSynInheritedAffinityOpening(t, nil, func(t *testing.T, fixture *inheritedSynOpeningFixture) {
		credits := fixture.pending(t)
		fixture.update.stateLock.Lock()
		fixture.update.selectAffinityClient(fixture.selected)
		replacement := fixture.update.client.loadSelection()
		fixture.update.stateLock.Unlock()
		if replacement == fixture.selection || replacement.client != fixture.selected {
			t.Fatal("control did not replace the selection receipt for the same provider")
		}
		fixture.release()
		fixture.requireRetired(t, credits)
		fixture.requireDonorProgress(t)
	})
}

// Hold the replay after successful admission, before source FIFO dequeue.
// This proves source validation rather than only a prequeue selection check.
func testInheritedSynQueuedRetirement(t *testing.T, cancelSource bool) {
	t.Helper()
	var entered chan *SendSequence
	var release chan struct{}
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	var armed atomic.Bool
	testNativeFirstSynInheritedAffinityOpening(t, func(settings *ClientSettings) {
		previous := settings.SendBufferSettings.beforeGroupDequeueForTest
		settings.SendBufferSettings.beforeGroupDequeueForTest = func(source *SendSequence) {
			if armed.CompareAndSwap(true, false) {
				entered <- source
				select {
				case <-release:
				case <-source.ctx.Done():
				}
			}
			previous(source)
		}
	}, func(t *testing.T, fixture *inheritedSynOpeningFixture) {
		entered, release = make(chan *SendSequence, 1), make(chan struct{})
		defer finish()
		credits := fixture.pending(t)
		armed.Store(true)
		fixture.release()
		synctest.Wait()
		select {
		case source := <-entered:
			if source != fixture.source {
				t.Fatal("opening left its inherited provider's real source")
			}
		default:
			t.Fatal("automatic retry never reached admitted-before-dequeue boundary")
		}
		fixture.update.stateLock.Lock()
		claim := fixture.update.sequenceClaims
		if claim == nil || claim.synRetry == nil || claim.synRetry.serial != credits.owner.serial ||
			claim.completionFlags.Load()&groupCompletionAdmitted == 0 || claim.completionFlags.Load()&groupCompletionDequeued != 0 {
			fixture.update.stateLock.Unlock()
			t.Fatal("control did not hold the actual admitted opening before dequeue")
		}
		if !cancelSource {
			fixture.update.selectAffinityClient(fixture.selected)
		}
		fixture.update.stateLock.Unlock()
		if cancelSource {
			fixture.source.Cancel()
		}
		finish()
		fixture.requireRetired(t, credits)
		if cancelSource {
			// Cancel retired this shared physical source; admission to its
			// replacement is not an immediate delivery receipt.
			if fixture.donor.client.loadSelection() != fixture.selection || !fixture.donor.receivedInbound.Load() || fixture.donor.IsDone() {
				t.Fatal("source retirement changed the established sibling's logical owner")
			}
		} else {
			fixture.requireDonorProgress(t)
		}
	})
}

// Rebinding after raw admission cannot give the old SYN a wire ordinal.
func TestNativeFirstSynInheritedAffinityQueuedSelectionChangeRetiresOpening(t *testing.T) {
	testInheritedSynQueuedRetirement(t, false)
}

// Source cancellation after admission still retires the optional packet and
// readiness owner even though capacity watches were already removed.
func TestNativeFirstSynInheritedAffinityQueuedSourceCloseReturnsBacking(t *testing.T) {
	testInheritedSynQueuedRetirement(t, true)
}

// A successful raw admission can dequeue before the dispatching worker returns.
// Holding materialization exposes that admission's still-pending cache owner.
func testInheritedSynDequeuedOpening(t *testing.T, retire string) {
	t.Helper()
	var entered, release chan struct{}
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	var armed atomic.Bool
	var retains, rearms atomic.Int64
	testNativeFirstSynInheritedAffinityOpening(t, func(settings *ClientSettings) {
		entered, release = make(chan struct{}), make(chan struct{})
		settings.SendBufferSettings.beforeGroupRetainForTest = func() {
			// The pressure ACK precedes the automatically admitted SYN.
			if armed.Load() && retains.Add(1) == 2 {
				close(entered)
				<-release
			}
		}
	}, func(t *testing.T, fixture *inheritedSynOpeningFixture) {
		defer finish()
		credits := fixture.pending(t)
		fixture.harness.multiClient.beforeSynRetryReadinessArmForTest = func(update *multiClientChannelUpdate) {
			if update == fixture.update {
				<-entered
				rearms.Add(1)
			}
		}
		armed.Store(true)
		fixture.release()
		fixture.harness.settle()
		select {
		case <-entered:
		default:
			t.Fatal("automatic inherited opening never dequeued at the source")
		}
		fixture.update.stateLock.Lock()
		pending := fixture.update.synRetry == credits.owner && credits.owner.openingSerial != 0 &&
			fixture.update.synGenerationSeen && fixture.update.client.loadSelection() == fixture.selection
		fixture.update.stateLock.Unlock()
		if !pending || credits.packetCredit.released.Load() || credits.readyCredit.released.Load() ||
			fixture.nativeOffers.Load() != 1 || fixture.retries.Load() != 1 || retains.Load() != 2 {
			t.Fatalf("admitted inherited opening lost pending custody before materialization: pending=%t packet_released=%t readiness_released=%t rearms=%d",
				pending, credits.packetCredit.released.Load(), credits.readyCredit.released.Load(), rearms.Load())
		}
		if retire == "source" {
			fixture.source.Cancel()
		} else if retire == "selection" {
			fixture.update.stateLock.Lock()
			fixture.update.selectAffinityClient(fixture.selected)
			fixture.update.stateLock.Unlock()
		}
		finish()
		fixture.harness.settle()
		if retire != "" {
			fixture.requireRetired(t, credits)
			if retire == "source" {
				if fixture.donor.client.loadSelection() != fixture.selection || !fixture.donor.receivedInbound.Load() || fixture.donor.IsDone() {
					t.Fatal("dequeued source cancellation changed its established sibling's logical owner")
				}
			} else {
				fixture.requireDonorProgress(t)
			}
			return
		}
		if _, ok := fixture.harness.synAck(fixture.path); !ok || len(fixture.harness.exit("a").syns(fixture.path)) != 1 ||
			fixture.budget.UsedByteCount() != 0 || time.Since(fixture.before) != 0 {
			t.Fatal("admitted inherited opening did not establish exactly once without source timer progress")
		}
		fixture.requireDonorProgress(t)
	})
}

// Its own dequeue must preserve pending custody until the source materializes.
func TestNativeFirstSynInheritedAffinityDequeuedOpeningKeepsCustody(t *testing.T) {
	testInheritedSynDequeuedOpening(t, "")
}

// Cancellation after dequeue still returns both reservations without wire work.
func TestNativeFirstSynInheritedAffinityDequeuedSourceCloseReturnsBacking(t *testing.T) {
	testInheritedSynDequeuedOpening(t, "source")
}

// A source-owned claim cannot serialize after its exact selection is replaced.
func TestNativeFirstSynInheritedAffinityDequeuedSelectionChangeRetiresOpening(t *testing.T) {
	testInheritedSynDequeuedOpening(t, "selection")
}

// The final watched source's close releases pending custody immediately.
func TestNativeFirstSynInheritedAffinitySourceCloseReturnsBacking(t *testing.T) {
	testNativeFirstSynInheritedAffinityOpening(t, nil, func(t *testing.T, fixture *inheritedSynOpeningFixture) {
		credits := fixture.pending(t)
		entered, release := make(chan struct{}, 1), make(chan struct{})
		var releaseOnce sync.Once
		finish := func() { releaseOnce.Do(func() { close(release) }) }
		defer finish()
		fixture.harness.multiClient.beforeSynRetryReadinessForTest = func(update *multiClientChannelUpdate) {
			if update != fixture.update {
				return
			}
			entered <- struct{}{}
			select {
			case <-release:
			case <-fixture.harness.multiClient.ctx.Done():
			}
		}
		fixture.release()
		fixture.harness.settle()
		select {
		case <-entered:
		default:
			t.Fatal("released source did not grant the pending opening readiness")
		}
		stats, err := fixture.selected.WindowStats()
		if err != nil || stats.sendNackCount != 0 || stats.sendNackByteCount != 0 || fixture.retries.Load() != 0 {
			t.Fatal("source-close premise did not finish the pressure ACK before holding the unadmitted opening")
		}
		fixture.source.Cancel()
		finish()
		fixture.requireRetired(t, credits)
		if fixture.donor.client.loadSelection() != fixture.selection || !fixture.donor.receivedInbound.Load() || fixture.donor.IsDone() {
			t.Fatal("source retirement changed the established sibling's logical owner")
		}
	})
}

// Parent teardown joins readiness and returns both exact paid reservations.
func TestNativeFirstSynInheritedAffinityParentCloseReturnsBacking(t *testing.T) {
	testNativeFirstSynInheritedAffinityOpening(t, nil, func(t *testing.T, fixture *inheritedSynOpeningFixture) {
		credits := fixture.pending(t)
		fixture.harness.multiClient.Close()
		fixture.release()
		if err := fixture.harness.multiClient.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		synRetryControlReleased(t, credits.owner)
		if !credits.packetCredit.released.Load() || !credits.readyCredit.released.Load() || fixture.budget.UsedByteCount() != 0 {
			t.Fatal("parent join retained inherited opening permission")
		}
	})
}

// The inherited retry still obeys the existing final health check; refusing
// that local continuation must not leave its now-unwatched optional owner.
func TestNativeFirstSynInheritedAffinityWarningRetiresOpening(t *testing.T) {
	testNativeFirstSynInheritedAffinityOpening(t, nil, func(t *testing.T, fixture *inheritedSynOpeningFixture) {
		credits := fixture.pending(t)
		fixture.selected.stateLock.Lock()
		fixture.selected.warning = true
		fixture.selected.stateLock.Unlock()
		fixture.release()
		fixture.requireRetired(t, credits)
		fixture.selected.stateLock.Lock()
		fixture.selected.warning = false
		fixture.selected.stateLock.Unlock()
		fixture.requireDonorProgress(t)
	})
}

// The eligible worker must return optional custody when its exact inherited
// selection is superseded before it can offer a packet to any source.
func TestNativeFirstSynInheritedAffinityDispatchSelectionChangeRetiresOpening(t *testing.T) {
	testNativeFirstSynInheritedAffinityOpening(t, nil, func(t *testing.T, fixture *inheritedSynOpeningFixture) {
		credits := fixture.pending(t)
		var dispatches atomic.Int64
		fixture.harness.multiClient.beforeSynRetryDispatchForTest = func(update *multiClientChannelUpdate) {
			if update != fixture.update {
				return
			}
			update.stateLock.Lock()
			update.selectAffinityClient(fixture.selected)
			update.stateLock.Unlock()
			dispatches.Add(1)
		}
		fixture.release()
		fixture.harness.settle()
		if dispatches.Load() != 1 || fixture.update.client.loadSelection() == fixture.selection || fixture.retries.Load() != 0 {
			t.Fatal("control did not replace the inherited token between eligible dequeue and dispatch")
		}
		fixture.requireRetired(t, credits)
		fixture.requireDonorProgress(t)
	})
}
