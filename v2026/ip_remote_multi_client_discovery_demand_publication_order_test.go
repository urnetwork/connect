// Discovery publication can finish before the source offer that requested it
// selects a provider. That real earlier success must retain collapse ownership.
package connect

import (
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestDiscoveryDemandPublicationCanServeSameIpv4SourceSyn(t *testing.T) {
	testDiscoveryDemandPublicationBeforeSourceSelection(t, 4, false)
}

func TestDiscoveryDemandPublicationCanServeSameIpv6SourceSyn(t *testing.T) {
	testDiscoveryDemandPublicationBeforeSourceSelection(t, 6, false)
}

// A retained original can answer before the source reassertion leaves its
// gate. That established ownership must suppress the duplicate source offer.
func TestDiscoveryDemandRetainedPublicationWinsBeforeSameIpv4SourceSyn(t *testing.T) {
	testDiscoveryDemandPublicationBeforeSourceSelection(t, 4, true)
}

func TestDiscoveryDemandRetainedPublicationWinsBeforeSameIpv6SourceSyn(t *testing.T) {
	testDiscoveryDemandPublicationBeforeSourceSelection(t, 6, true)
}

// Explicit dispatch and real SYN-ACK events choose each winner. Publication,
// selection, wire traffic and response ownership remain the production paths.
func testDiscoveryDemandPublicationBeforeSourceSelection(t *testing.T, version int, automaticFirst bool) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		path := reraceFlow(version, 81, 52880+version)
		specs := make([]reraceExitSpec, 4)
		for index := range specs {
			specs[index] = reraceExitSpec{name: string(rune('a' + index)), answer: func(candidate *IpPath) reraceAnswer {
				if candidate.DestinationIp.Equal(path.DestinationIp) {
					return reraceSignal
				}
				return reraceAccept
			}}
		}
		h, generator := newDiscoveryDemandTestHarness(t, specs)
		window, paths, owners := discoveryDemandTestCarryFour(t, h)
		publication := discoveryDemandTestObservePublication(window)
		synctest.Wait()
		window.stateLock.Lock()
		deadline := window.discoveryDemand.deadline
		liveSlot := window.discoveryDemand.owner != nil && window.discoveryDemand.slotOpen && time.Until(deadline) > 4*time.Second
		window.stateLock.Unlock()
		if !liveSlot || len(h.liveChannels()) != 4 || h.multiClient.backendDegraded() {
			t.Fatal("publication order control lacks the original vacant acquisition")
		}
		beforeReads, beforeEmpty, beforeMints, beforeRetired := generator.counts()
		start := time.Now()
		if !h.syn(path) {
			t.Fatal("original source SYN was not admitted")
		}
		update := h.update(path)
		update.stateLock.Lock()
		failed, inbound, bound := len(update.dialFailedClients), update.receivedInbound.Load(), update.client.Load()
		update.stateLock.Unlock()
		firstReads, firstEmpty, _, _ := generator.counts()
		if failed != 4 || inbound || bound != nil || h.metrics().DialFailuresIntercepted != 4 ||
			firstReads != beforeReads+1 || firstEmpty != beforeEmpty+1 {
			t.Fatal("publication order control lacks four refusals and the completed empty read")
		}
		originalOwner := synRetryControlOwner(t, update)
		originalSerial, originalCredit := originalOwner.serial, originalOwner.credit
		var lateExit *reraceExit
		publicationBound := discoveryDemandTestEarlierTime(deadline, start.Add(reraceSynSchedule[2]))
		dispatchEntered, dispatchRelease := make(chan struct{}), make(chan struct{})
		answered := make(chan struct{})
		var enteredOnce, releaseOnce, answeredOnce sync.Once
		var dispatchVisits, retainedGateVisits, selectionVisits atomic.Int32
		var dispatchOwnerValid, retainedOwnerValid atomic.Bool
		releaseDispatch := func() { releaseOnce.Do(func() { close(dispatchRelease) }) }
		t.Cleanup(releaseDispatch)
		h.multiClient.beforeSynRetryDispatchForTest = func(current *multiClientChannelUpdate) {
			if current != update {
				return
			}
			current.stateLock.Lock()
			valid := current.synRetry == originalOwner && originalOwner.serial == originalSerial &&
				originalOwner.update == current && originalOwner.sequence == path.SequenceNumber &&
				originalOwner.references == 2 && !originalOwner.queued && originalOwner.validWithLock(current)
			current.stateLock.Unlock()
			dispatchOwnerValid.Store(valid)
			dispatchVisits.Add(1)
			enteredOnce.Do(func() { close(dispatchEntered) })
			select {
			case <-dispatchRelease:
			case <-h.multiClient.ctx.Done():
			}
		}
		h.multiClient.SetReceivePacketCallback(discoveryDemandPublicationReceiveObserver(h.receive, func(reply *IpPath, packet []byte) {
			if reply == nil || !(reraceSameFlow(reply, path) || reraceSameFlow(reply, path.Reverse())) {
				return
			}
			if parsed, err := ParseIpPath(packet); err == nil && parsed.Syn && parsed.Ack && parsed.AckSequenceNumber == path.SequenceNumber+1 {
				answeredOnce.Do(func() { close(answered) })
			}
		}))
		beforeCollapse := h.multiClient.TcpCollapseDropCount()
		h.multiClient.beforePacketGroupGateForTest = func(current *multiClientChannelUpdate, snapshot *multiClientChannel, group *parsedPacketGroup) {
			if !reraceSameFlow(group.ipPath, path) {
				return
			}
			if group.isSynRetry() {
				retainedOwnerValid.Store(current == update && group.synRetry.update == update &&
					group.synRetry.serial == originalSerial && group.synRetry.source == h.source &&
					group.ipPath.SequenceNumber == path.SequenceNumber && group.ipPath.Syn && !group.ipPath.Ack)
				retainedGateVisits.Add(1)
				return
			}
			if selectionVisits.Add(1) != 1 {
				return
			}
			if current != update || snapshot != nil || !group.ipPath.Syn || group.ipPath.Ack || group.synRetry == nil ||
				group.synRetry.update != update || group.synRetry.nativeOwnerSerial != originalSerial {
				t.Fatal("publication barrier did not observe the unchanged unbound source SYN and exact retained owner")
			}
			if !discoveryDemandTestAwaitPublication(t, publication, publicationBound) || !time.Now().Before(publicationBound) || len(h.liveChannels()) != 5 {
				t.Fatal("source gate did not observe real publication before its original next SYN")
			}
			if !discoveryDemandTestAwaitPublication(t, dispatchEntered, publicationBound) || !dispatchOwnerValid.Load() || dispatchVisits.Load() != 1 {
				t.Fatal("PUBLICATION_WINNER_DISPATCH_OWNER_MISSING: publication did not dispatch the original exact paid owner")
			}
			if automaticFirst {
				releaseDispatch()
				if !discoveryDemandTestAwaitPublication(t, answered, publicationBound) || !time.Now().Before(publicationBound) ||
					h.boundTo(path) != h.channel("late") || h.update(path) != update || !update.receivedInbound.Load() ||
					len(lateExit.syns(path)) != 1 || retainedGateVisits.Load() != 1 || !retainedOwnerValid.Load() {
					t.Fatal("PUBLICATION_RETAINED_WIN_NOT_OBSERVED: the exact retained source did not physically establish before reassertion")
				}
			} else if len(lateExit.syns(path)) != 0 || update.receivedInbound.Load() || retainedGateVisits.Load() != 0 {
				t.Fatal("PUBLICATION_SOURCE_FIRST_BARRIER_MISSING: retained dispatch escaped its actual pre-send barrier")
			}
		}
		// Both barriers are armed before late discovery or clock advancement.
		// Only the reasserting source gate can release the automatic winner.
		time.Sleep(100 * time.Millisecond)
		lateExit = h.addExit(reraceExitSpec{name: "late"})
		h.sleep(start.Add(reraceSynSchedule[1]).Sub(time.Now()))
		accepted := h.syn(path)
		// Keep hooks installed until the fixture's joined close. The callback
		// fields may still be read by a real producer after this source returns.
		expectedAccepted := !automaticFirst
		if accepted != expectedAccepted {
			t.Fatalf("PUBLICATION_WINNER_ACCEPTANCE_MISMATCH: automatic_first=%t accepted=%t expected=%t", automaticFirst, accepted, expectedAccepted)
		}
		wantedCollapse := uint64(0)
		if automaticFirst {
			wantedCollapse = 1
		} else {
			update.stateLock.Lock()
			stillOwned := update.synRetry == nil && originalOwner.references == 1 && originalOwner.update == update
			update.stateLock.Unlock()
			if !stillOwned || originalCredit.released.Load() || retainedGateVisits.Load() != 0 {
				t.Fatal("source-first answer lost the held dispatch reference or sent duplicate retained bytes")
			}
		}
		if h.multiClient.TcpCollapseDropCount() != beforeCollapse+wantedCollapse {
			t.Fatal("winner ordering did not retain its exact source collapse result")
		}
		releaseDispatch()
		h.settle()
		synRetryControlReleased(t, originalOwner)
		if !originalCredit.released.Load() || !originalOwner.publicationRetired.Load() || originalOwner.publicationIndexed ||
			dispatchVisits.Load() != 1 || !dispatchOwnerValid.Load() {
			t.Fatal("winner ordering retained paid cache, publication, dispatch or budget custody")
		}
		reads, empty, mints, retired := generator.counts()
		late := h.channel("late")
		if selectionVisits.Load() != 1 || len(h.sentSyns(path)) != 2 || !time.Now().Before(start.Add(reraceSynSchedule[2])) ||
			h.boundTo(path) != late || len(h.exit("late").syns(path)) != 1 || h.metrics().DialFailuresIntercepted != 4 {
			t.Fatal("reasserting source SYN did not use its already published real provider")
		}
		if packet, answered := h.synAck(path); !answered || !update.receivedInbound.Load() || h.update(path) != update {
			t.Fatal("same source offer did not establish the original connect attempt")
		} else if reply, err := ParseIpPath(packet.packet); err != nil || reply.AckSequenceNumber != path.SequenceNumber+1 ||
			len(lateExit.syns(path)) != 1 || lateExit.syns(path)[0].path.SequenceNumber != path.SequenceNumber {
			t.Fatal("winner reply or physical SYN escaped the original source generation")
		}
		if reads != beforeReads+2 || empty != beforeEmpty+1 || mints != beforeMints+1 || retired != beforeRetired {
			t.Fatal("earlier source success exceeded its original discovery budget")
		}
		collapseDrops := h.multiClient.TcpCollapseDropCount()
		h.sleep(start.Add(reraceSynSchedule[2]).Sub(time.Now()))
		if h.syn(path) || h.multiClient.TcpCollapseDropCount() != collapseDrops+1 ||
			len(h.exit("late").syns(path)) != 1 || h.boundTo(path) != late {
			t.Fatal("established same-SYN duplicate lost the earlier source's recovery ownership")
		}
		reraceAssertNoErrorDelivered(t, h, path)
		payload := []byte("held original CSS sibling")
		for index, original := range paths {
			if h.boundTo(original) != owners[index] || owners[index].IsDone() || owners[index].isWarning() || owners[index].dialStarved() ||
				!h.send(reraceSegment(original, uint32(4*len(payload)), payload)) || len(h.delivered(original)) != 6 {
				t.Fatal("earlier discovery publication disturbed an original healthy sibling")
			}
		}
	})
}

// Observers borrow the live receive packet after the actual application
// callback records it. No additional packet owner or child package is needed.
func discoveryDemandPublicationReceiveObserver[Mode any](receive func(TransferPath, Mode, *IpPath, []byte), observe func(*IpPath, []byte)) func(TransferPath, Mode, *IpPath, []byte) {
	return func(source TransferPath, mode Mode, path *IpPath, packet []byte) {
		receive(source, mode, path, packet)
		observe(path, packet)
	}
}
