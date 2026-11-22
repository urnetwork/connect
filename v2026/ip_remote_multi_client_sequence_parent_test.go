// Parent pointer synchronization belongs to the canonical flow, independently
// of packet ownership. Real admissions/reuse and weak-index retirement compose
// around the same critical section; none of these probes changes send policy.
package connect

import (
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// This metadata-only weak-index fixture owns no packet or transfer permission;
// real native source/custody coverage is supplied by the composed root below.
func newSequenceParentMetadataFixture(t *testing.T) (*RemoteUserNatMultiClient, *multiClientChannelUpdate, *tcpSynRetry) {
	t.Helper()
	parent := &RemoteUserNatMultiClient{ctx: t.Context(), settings: DefaultMultiClientSettings(), flowReaperDone: make(chan struct{})}
	path := unboundFlowTcpPath(53931)
	update := newMultiClientChannelUpdate(t.Context(), path)
	group := &parsedPacketGroup{ipPath: path, packets: []parsedPacket{{ipPath: path}}}
	if !group.prepareCollapseAdmission(update, parent) {
		t.Fatal("parent synchronization fixture could not prepare its actual SYN admission")
	}
	group.finishCollapseSynOffer()
	owner := &tcpSynRetry{update: update, references: 1, serial: 1, sequence: path.SequenceNumber}
	update.stateLock.Lock()
	update.synRetry = owner
	update.stateLock.Unlock()
	t.Cleanup(update.Close)
	return parent, update, owner
}

// The normal negative tests have no competing flow holder: TryLock therefore
// identifies a missing read critical section without scheduler or race timing.
func sequenceParentReadProbe(parent *RemoteUserNatMultiClient, unlocked, nested *atomic.Bool, count *atomic.Int32) func(*multiClientChannelUpdate) {
	return func(update *multiClientChannelUpdate) {
		count.Add(1)
		if update.stateLock.TryLock() {
			unlocked.Store(true)
			update.stateLock.Unlock()
		}
		if parent.stateLock.TryLock() {
			parent.stateLock.Unlock()
		} else {
			nested.Store(true)
		}
	}
}

func TestSynPublicationParentSnapshotOwnsFlowLock(t *testing.T) {
	parent, update, owner := newSequenceParentMetadataFixture(t)
	var unlocked, nested atomic.Bool
	var reads atomic.Int32
	parent.afterSequenceParentReadForTest = sequenceParentReadProbe(parent, &unlocked, &nested, &reads)
	update.indexSynRetryPublication()
	parent.stateLock.Lock()
	indexed := owner.publicationIndexed && parent.synPublicationOwnerHead == owner && parent.synPublicationOwnerCount == 1
	parent.stateLock.Unlock()
	if !indexed || reads.Load() != 2 || unlocked.Load() || nested.Load() {
		t.Fatalf("SYN_PARENT_INDEX_SNAPSHOT_UNPROTECTED: indexed=%t reads=%d unlocked=%t parent_nested=%t", indexed, reads.Load(), unlocked.Load(), nested.Load())
	}
	update.Close()
	parent.stateLock.Lock()
	retained := parent.synPublicationOwnerCount != 0 || owner.publicationIndexed
	parent.stateLock.Unlock()
	if retained || owner.update != nil {
		t.Fatal("final source retirement retained its weak publication index")
	}
}

func TestSynRetryReleaseSnapshotsParentWithDetachedOwners(t *testing.T) {
	parent, update, owner := newSequenceParentMetadataFixture(t)
	update.indexSynRetryPublication()
	var unlocked, nested atomic.Bool
	var reads atomic.Int32
	parent.afterSequenceParentReadForTest = sequenceParentReadProbe(parent, &unlocked, &nested, &reads)
	update.Close()
	parent.stateLock.Lock()
	retired := parent.synPublicationOwnerCount == 0 && !owner.publicationIndexed
	parent.stateLock.Unlock()
	if !retired || owner.update != nil || reads.Load() != 1 || unlocked.Load() || nested.Load() {
		t.Fatalf("SYN_PARENT_RELEASE_SNAPSHOT_UNPROTECTED: retired=%t reads=%d unlocked=%t parent_nested=%t", retired, reads.Load(), unlocked.Load(), nested.Load())
	}
}

// A concurrent admission is allowed at the existing hook. The newly current
// parent+owner must not be attached through a stale earlier parent snapshot.
func TestSynPublicationParentAndOwnerCaptureCannotMixGenerations(t *testing.T) {
	parent, update, original := newSequenceParentMetadataFixture(t)
	next := &RemoteUserNatMultiClient{ctx: t.Context(), settings: DefaultMultiClientSettings(), flowReaperDone: make(chan struct{})}
	entered, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(resume) }) }
	parent.beforeSynPublicationIndexCaptureForTest = func(current *multiClientChannelUpdate) { close(entered); <-resume }
	go func() { defer close(done); update.indexSynRetryPublication() }()
	t.Cleanup(func() { release(); <-done; parent.removeSynPublicationOwner(original) })
	<-entered
	// This synthetic parent-mismatch boundary replaces exact metadata;
	// no pointer list is pruned to manufacture native retry authority.
	path := unboundFlowTcpPath(53932)
	group := &parsedPacketGroup{ipPath: path, packets: []parsedPacket{{ipPath: path}}}
	if !group.prepareCollapseAdmission(update, next) {
		t.Fatal("new parent admission was not prepared")
	}
	group.finishCollapseSynOffer()
	replacement := &tcpSynRetry{update: update, references: 1, serial: 2, sequence: path.SequenceNumber}
	update.stateLock.Lock()
	update.clearSynRetryWithLock()
	update.synRetry = replacement
	update.stateLock.Unlock()
	// The old unindexed synthetic owner is retired before resuming capture.
	update.releaseSynRetryMemory()
	release()
	<-done
	parent.stateLock.Lock()
	wrong := parent.synPublicationOwnerCount != 0 || replacement.publicationIndexed
	parent.stateLock.Unlock()
	if wrong {
		parent.removeSynPublicationOwner(replacement)
		t.Fatal("SYN_PARENT_OWNER_GENERATION_MIXED: stale parent indexed a later exact owner")
	}
	update.indexSynRetryPublication()
	next.stateLock.Lock()
	correct := replacement.publicationIndexed && next.synPublicationOwnerHead == replacement && next.synPublicationOwnerCount == 1
	next.stateLock.Unlock()
	if !correct {
		t.Fatal("current parent could not index its own still-live exact owner")
	}
}

// The store is gated inside the actual admission's flow lock. A real native
// source has already materialized; reader capture is concurrent with a retry
// write, not a test assignment to an otherwise unused parent pointer.
func TestNativeSynRetryPublicationParentCaptureSurvivesConcurrentAdmission(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		h, path := synRetryControlHarness(t, 53933, nil, DefaultProtocolVersion)
		if !privateStaleBoundNativeSyn(t, h, path) {
			t.Fatal("concurrent parent control lacks its actual paid native SYN")
		}
		update := h.update(path)
		owner := synRetryControlOwner(t, update)
		serial := owner.serial
		entered, resume, readerStarted, readerDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
		var armed atomic.Bool
		var unlocked atomic.Bool
		var reads atomic.Int32
		var resumeOnce sync.Once
		release := func() { resumeOnce.Do(func() { close(resume) }) }
		// Existing source admissions are quiescent before these immutable
		// test hooks are published; only the explicitly armed retry can wait.
		h.multiClient.beforeSequenceParentWriteForTest = func(current *multiClientChannelUpdate) {
			if current == update && armed.CompareAndSwap(true, false) {
				close(entered)
				<-resume
			}
		}
		h.multiClient.afterSequenceParentReadForTest = func(current *multiClientChannelUpdate) {
			if current != update {
				return
			}
			reads.Add(1)
			if current.stateLock.TryLock() {
				unlocked.Store(true)
				current.stateLock.Unlock()
			}
		}
		readerLaunched := false
		t.Cleanup(func() {
			release()
			if readerLaunched {
				<-readerDone
			}
			h.concurrent = false
		})
		h.concurrent = true
		armed.Store(true)
		h.inject("a", reraceProviderSignal(t, path))
		<-entered
		readerLaunched = true
		go func() { defer close(readerDone); close(readerStarted); update.indexSynRetryPublication() }()
		<-readerStarted
		// Reader and writer share only the real flow lock. The explicit gate
		// is released before any bubble-wide Wait can encounter that mutex.
		release()
		<-readerDone
		h.concurrent = false
		h.settle()
		update.stateLock.Lock()
		same := update.sequenceParent == h.multiClient && update.synRetry == owner && owner.serial == serial && owner.references == 1 && !owner.queued
		update.stateLock.Unlock()
		if !same || reads.Load() == 0 || unlocked.Load() || owner.credit == nil || owner.credit.released.Load() || len(h.exit("b").syns(path)) != 1 {
			t.Fatalf("SYN_PARENT_CONCURRENT_ADMISSION_LOST_OWNER: same=%t reads=%d unlocked=%t replacement_syns=%d", same, reads.Load(), unlocked.Load(), len(h.exit("b").syns(path)))
		}
	})
}

// Runtime frames distinguish the direct grouped-send snapshot from later
// async materialization/release snapshots; no extra production seam is needed.
func sequenceParentSnapshotCaller() string {
	var pcs [12]uintptr
	count := runtime.Callers(3, pcs[:])
	frames := runtime.CallersFrames(pcs[:count])
	for {
		frame, more := frames.Next()
		if !strings.HasSuffix(frame.Function, ".sequenceParentWithLock") && !strings.HasSuffix(frame.Function, ".sequenceParentSnapshot") {
			return frame.Function
		}
		if !more {
			return ""
		}
	}
}

func TestNativeAffinitySynReuseSnapshotsParentBeforeReentry(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newServiceRefreshAffinityTestFixture(t)
		var direct atomic.Int32
		var unlocked atomic.Bool
		f.h.multiClient.afterSequenceParentReadForTest = func(update *multiClientChannelUpdate) {
			if update != f.update || !strings.HasSuffix(sequenceParentSnapshotCaller(), ").SendGroupDetailedWithAck") {
				return
			}
			direct.Add(1)
			if update.stateLock.TryLock() {
				unlocked.Store(true)
				update.stateLock.Unlock()
			}
		}
		f.resume()
		<-f.sendDone
		f.h.concurrent = false
		f.h.settle()
		if !f.accepted || direct.Load() == 0 || unlocked.Load() || f.h.update(f.path) != f.update || f.h.boundTo(f.path) != f.victim || !f.update.receivedInbound.Load() {
			t.Fatalf("SYN_PARENT_AFFINITY_REUSE_UNPROTECTED: accepted=%t direct_snapshots=%d unlocked=%t", f.accepted, direct.Load(), unlocked.Load())
		}
		if f.h.boundTo(f.siblingPath) != f.siblingOwner || f.siblingOwner.IsDone() {
			t.Fatal("parent snapshot changed the independent established sibling")
		}
	})
}
