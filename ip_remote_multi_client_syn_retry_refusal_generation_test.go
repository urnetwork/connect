// A real pending native sibling may delay retry dispatch but must not erase
// an authoritative provider refusal for the exact paid source generation.
package connect

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"testing/synctest"
)

// Actual queue admission and physical first-Pack materialization force the
// failing ordering, while the second native sibling remains durably raw.
func TestSynRefusalRecordsWhileNativeSiblingStillBlocksReplay(t *testing.T) {
	testSynRefusalRawSibling(t, "")
}

// An accepted/dequeued reset may defer context retirement behind its creator,
// but the actual control observer already relinquishes SYN-cache authority.
func TestSynRefusalDequeuedResetWithCreatorCountCannotRecord(t *testing.T) {
	testSynRefusalRawSibling(t, "RST")
}

// A native FIN retires the SYN cache even while the half-closed flow stays live.
func TestSynRefusalDequeuedFinWithCreatorCountCannotRecord(t *testing.T) {
	testSynRefusalRawSibling(t, "FIN")
}

// Only existing fixtures own queue barriers, clients and physical wire ACKs.
// The decoded failure enters the actual channel interceptor after its SYN Pack
// was read; the test neither fabricates delivery nor acknowledges unread data.
func testSynRefusalRawSibling(t *testing.T, terminal string) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		h := newReraceHarness(t, []reraceExitSpec{
			{name: "history-a", answer: reraceAlways(reraceSilent)},
			{name: "history-b", answer: reraceAlways(reraceSilent)},
		}, nil)
		root := NewTransferMemoryBudget(256 * 1024)
		firstBudget := NewTransferMemoryBudgetWithParent(128*1024, root)
		secondBudget := NewTransferMemoryBudgetWithParent(128*1024, root)
		firstAdmitted, secondAdmitted := make(chan *parsedPacketGroup, 1), make(chan *parsedPacketGroup, 1)
		first := newGroupDispositionQueueFixture(t, firstBudget, func(settings *ClientSettings) {
			settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
				select {
				case firstAdmitted <- target.(*parsedPacketGroup):
				default:
				}
			}
		})
		second := newGroupDispositionQueueFixture(t, secondBudget, func(settings *ClientSettings) {
			settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
				select {
				case secondAdmitted <- target.(*parsedPacketGroup):
				default:
				}
			}
		})
		first.selected.settings, second.selected.settings = h.multiClient.settings, h.multiClient.settings
		first.selected.dialFailureCallback = h.multiClient.clientDialFailureSignal
		first.selected.clientReceivePacketCallback = h.multiClient.clientReceivePacket
		h.multiClient.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			return []*multiClientChannel{first.selected, second.selected}
		}
		path := reraceFlow(4, 108, 53410)
		original := reraceTcpPacket(path, tcpFlagSyn, path.SequenceNumber, 0, nil)
		var update *multiClientChannelUpdate
		defer func() {
			if update != nil {
				update.stateLock.Lock()
				update.sequenceSynOffers = 0
				update.stateLock.Unlock()
			}
			second.unpark()
			if err := h.multiClient.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			if err := first.closeClient(context.Background()); err != nil {
				t.Error(err)
			}
			if err := second.closeClient(context.Background()); err != nil {
				t.Error(err)
			}
			for _, route := range []Route{first.route, second.route} {
				for len(route) != 0 {
					MessagePoolReturn(<-route)
				}
			}
			synctest.Wait()
			for _, ledger := range []*TransferMemoryBudget{firstBudget, secondBudget, root} {
				reserved, released := ledger.Counts()
				if ledger.UsedByteCount() != 0 || reserved != released {
					t.Errorf("joined refusal control retained permission: used=%d reserved=%d released=%d", ledger.UsedByteCount(), reserved, released)
				}
			}
		}()
		if !privateCrossWindowNativeSyn(h, path) {
			t.Fatal("initial native source did not enter both real candidate queues")
		}
		update = h.update(path)
		first.requireParked()
		second.requireParked()
		var firstGroup, secondGroup *parsedPacketGroup
		select {
		case firstGroup = <-firstAdmitted:
		default:
			t.Fatal("first real native admission missing")
		}
		select {
		case secondGroup = <-secondAdmitted:
		default:
			t.Fatal("second real native admission missing")
		}
		update.stateLock.Lock()
		initial := update.synRetry == nil && firstGroup.synRetry != nil && firstGroup.synRetry == secondGroup.synRetry &&
			firstGroup.synRetry.serial == 0 && firstGroup.synRetry.nativeOwnerSerial == 0 &&
			update.race != nil && len(update.race.clientStates) == 2 && update.client.Load() == nil
		update.stateLock.Unlock()
		if !initial {
			t.Fatal("native sibling ordering did not precede first source-owned cache materialization")
		}
		first.unpark()
		first.acknowledgeAll(original)
		owner := synRetryControlOwner(t, update)
		credit := owner.credit
		update.stateLock.Lock()
		flags := secondGroup.completionFlags.Load()
		raw := flags&(groupCompletionAdmitted|groupCompletionLinked|groupCompletionCandidate) == groupCompletionAdmitted|groupCompletionLinked|groupCompletionCandidate &&
			flags&(groupCompletionDequeued|groupCompletionDone|groupCompletionAbandoned) == 0 &&
			secondGroup.synRetry == firstGroup.synRetry && owner.validGenerationWithLock(update) &&
			!owner.validWithLock(update) && owner.pendingConflictingControlWithLock(update)
		update.stateLock.Unlock()
		if !raw || len(second.route) != 0 {
			t.Fatal("forced pending ordinary native sibling did not independently fence replay")
		}
		if terminal != "" {
			second.unpark()
			second.acknowledgeAll(original)
			update.stateLock.Lock()
			// Reproduce the observed deferred-close scalar boundary with an
			// actual cached source and the production control observer.
			update.sequenceSynOffers = 1
			closed := update.observeTcpControlWithLock(tcpControlObservation{valid: true, rst: terminal == "RST", fin: terminal == "FIN"}, false)
			update.sequenceCloseReady.Store(closed)
			terminalState := update.synRetry == nil && !update.IsDone() && !update.sourceReset.Load() &&
				update.sequenceSynOffers == 1 && !update.pendingCollapseCloseWithLock() &&
				(terminal != "RST" || update.sequenceCloseReady.Load())
			update.stateLock.Unlock()
			update.releaseSynRetryMemory()
			if !terminalState || !credit.released.Load() {
				t.Fatal("terminal control did not reach the cache-free deferred-creator boundary")
			}
		}
		signal := reraceProviderSignal(t, path)
		frame, err := ipPacketFromProviderFrame(signal, DefaultProtocolVersion)
		if err != nil {
			MessagePoolReturn(signal)
			t.Fatal(err)
		}
		if !frame.Raw {
			MessagePoolReturn(signal)
		}
		synRefusalDeliverOneFrame(first.selected.clientReceive, SourceId(first.peer), frame, Peer{})
		MessagePoolReturn(frame.MessageBytes)
		h.settle()
		update.stateLock.Lock()
		standing := slices.Contains(update.dialFailedClients, first.selected) && slices.Contains(update.dialFailureSignaledClients, first.selected)
		history := len(owner.refusedRemoteIds) == 1 && owner.refusedRemoteIds[0] == first.selected.Destination().Tail()
		fullValid, reusable := owner.validWithLock(update), update.synRetryHasRemoteRefusalWithLock()
		queued, references := owner.queued, owner.references
		update.stateLock.Unlock()
		if terminal != "" {
			if history || reusable || queued || len(owner.refusedRemoteIds) != 0 {
				t.Fatalf("SYN_REFUSAL_TERMINAL_OWNER_RECORDED: terminal=%s", terminal)
			}
			synRetryControlReleased(t, owner)
			return
		}
		if !standing || !history {
			t.Fatalf("SYN_REFUSAL_HISTORY_LOST_BEHIND_RAW_SIBLING: standing=%t history=%t full_valid=%t refs=%d", standing, history, fullValid, references)
		}
		if fullValid || reusable || queued || references != 1 || len(first.route) != 0 || len(second.route) != 0 {
			t.Fatal("SYN_REFUSAL_RECORDING_PREMATURELY_ENABLED_REPLAY: history bypassed the independent raw-sibling fence")
		}
		second.unpark()
		second.acknowledgeAll(original)
		update.stateLock.Lock()
		settled := update.synRetry == owner && owner.validWithLock(update) && update.synRetryHasRemoteRefusalWithLock() &&
			len(owner.refusedRemoteIds) == 1 && !owner.queued && owner.references == 1 && bytes.Equal(owner.packet, original)
		update.stateLock.Unlock()
		if !settled {
			t.Fatal("dequeued sibling did not preserve exact cached bytes and received refusal authority")
		}
		update.Close()
		h.settle()
		synRetryControlReleased(t, owner)
		if !credit.released.Load() {
			t.Fatal("joined native refusal control retained its actual cache credit")
		}
	})
}

// Preserve the existing typed receive boundary without a new child import.
func synRefusalDeliverOneFrame[Frame any](receive func(TransferPath, []Frame, Peer), source TransferPath, frame Frame, peer Peer) {
	receive(source, []Frame{frame}, peer)
}

// Accepted terminal or different-SYN claims cannot acquire recording authority.
func TestSynRefusalPendingTerminalAndNewSynRejectRecording(t *testing.T) {
	for _, control := range []struct {
		name          string
		fin, rst, ack bool
		sequenceDelta uint32
	}{
		{name: "FIN", fin: true}, {name: "RST", rst: true}, {name: "new-SYN", sequenceDelta: 1}, {name: "SYN-ACK", ack: true},
	} {
		f, failed := newSynRefusalGenerationTestFixture(t)
		f.update.stateLock.Lock()
		f.group.ipPath.Syn = !control.fin && !control.rst
		f.group.ipPath.Fin, f.group.ipPath.Rst, f.group.ipPath.Ack = control.fin, control.rst, control.ack || control.fin
		f.group.ipPath.SequenceNumber += control.sequenceDelta
		f.owner.noteRemoteRefusalWithLock(f.update, failed)
		recorded := len(f.owner.refusedRemoteIds)
		f.update.stateLock.Unlock()
		if recorded != 0 {
			t.Fatalf("SYN_REFUSAL_PENDING_GENERATION_CHANGE_RECORDED: control=%s", control.name)
		}
	}
}

// Old owner tokens cannot write their retired history or the replacement's.
func TestSynRefusalOldOwnerTokenCannotRecordIntoReplacement(t *testing.T) {
	f, failed := newSynRefusalGenerationTestFixture(t)
	next := &tcpSynRetry{update: f.update, packet: bytes.Clone(f.owner.packet), source: f.owner.source,
		provideMode: f.owner.provideMode, pin: f.owner.pin, sequence: f.owner.sequence,
		serial: f.owner.serial + 1, references: 1, refusedRemoteIds: make([]Id, 0, 2)}
	f.update.stateLock.Lock()
	f.update.synRetry = next
	f.owner.noteRemoteRefusalWithLock(f.update, failed)
	staleRecorded := len(f.owner.refusedRemoteIds) != 0 || len(next.refusedRemoteIds) != 0
	next.noteRemoteRefusalWithLock(f.update, failed)
	currentRecorded := len(next.refusedRemoteIds)
	f.update.stateLock.Unlock()
	if staleRecorded {
		t.Fatal("SYN_REFUSAL_STALE_TOKEN_RECORDED: old cache token changed either owner's history")
	}
	if currentRecorded != 1 {
		t.Fatal("current replacement token could not record its own supplied standing fact")
	}
}

// Every lifetime/generation guard first has the same positive recording proof.
func TestSynRefusalRecordingPreservesEveryOwnerGenerationFence(t *testing.T) {
	for _, control := range []struct {
		name   string
		change func(*nativeSynAttestationFixture)
	}{
		{name: "closed", change: func(f *nativeSynAttestationFixture) { f.cancel() }},
		{name: "source-reset", change: func(f *nativeSynAttestationFixture) { f.update.sourceReset.Store(true) }},
		{name: "answered", change: func(f *nativeSynAttestationFixture) { f.update.receivedInbound.Store(true) }},
		{name: "closed-affinity", change: func(f *nativeSynAttestationFixture) { f.update.affinitySources.closed = true }},
		{name: "new-generation", change: func(f *nativeSynAttestationFixture) { f.update.synGenerationNumber++ }},
		{name: "new-attempt", change: func(f *nativeSynAttestationFixture) { f.update.dialFailedSyn++ }},
		{name: "no-attempt", change: func(f *nativeSynAttestationFixture) { f.update.dialFailedSynSeen = false }},
		{name: "retired-reference", change: func(f *nativeSynAttestationFixture) { f.owner.references = 0 }},
		{name: "zero-token", change: func(f *nativeSynAttestationFixture) { f.owner.serial = 0 }},
		{name: "detached-token", change: func(f *nativeSynAttestationFixture) { f.update.synRetry = nil }},
	} {
		f, failed := newSynRefusalGenerationTestFixture(t)
		f.update.stateLock.Lock()
		f.owner.noteRemoteRefusalWithLock(f.update, failed)
		positive := len(f.owner.refusedRemoteIds) == 1
		f.owner.refusedRemoteIds = f.owner.refusedRemoteIds[:0]
		control.change(f)
		f.owner.noteRemoteRefusalWithLock(f.update, failed)
		recorded := len(f.owner.refusedRemoteIds)
		f.update.stateLock.Unlock()
		if !positive {
			t.Fatalf("%s lacked the positive generation recording proof", control.name)
		}
		if recorded != 0 {
			t.Fatalf("recording bypassed owner generation fence: %s", control.name)
		}
	}
}

// Metadata-only controls borrow the canonical caller-owned packet fixture;
// actual queue credit and physical materialization are proved above separately.
func newSynRefusalGenerationTestFixture(t *testing.T) (*nativeSynAttestationFixture, *multiClientChannel) {
	t.Helper()
	f := newNativeSynAttestationFixture(t, 4)
	f.update.noteConnectAttempt(f.group.ipPath)
	f.owner.refusedRemoteIds = make([]Id, 0, 2)
	f.prepare(f.group)
	f.makePending(f.group)
	f.group.synRetry.nativeOwnerSerial = 0
	failed := &multiClientChannel{ctx: t.Context(), args: &multiClientChannelArgs{Destination: RequireMultiHopId(NewId())}}
	return f, failed
}
