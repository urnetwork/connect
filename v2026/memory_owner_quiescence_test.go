// Fixture-only controls for constructor publication and ledger quiescence.
// The matched original keeps publication unheld; production code is unchanged.
package connect

import (
	"context"
	"sync"
	"testing"
	"time"
)

// One real constructor callback remains owned until the test explicitly releases
// it. Client.Close comes first so release cannot admit a new shutdown sender.
type ledgerFixturePublisher struct {
	entered     chan struct{}
	released    chan struct{}
	enterOnce   sync.Once
	releaseOnce sync.Once
}

func newLedgerFixturePublisher() *ledgerFixturePublisher {
	return &ledgerFixturePublisher{
		entered: make(chan struct{}), released: make(chan struct{}),
	}
}

func (self *ledgerFixturePublisher) wait() {
	self.enterOnce.Do(func() { close(self.entered) })
	<-self.released
}

func (self *ledgerFixturePublisher) release() {
	self.releaseOnce.Do(func() { close(self.released) })
}

func (self *ledgerFixturePublisher) held() bool {
	select {
	case <-self.released:
		return false
	default:
		return true
	}
}

func (self *ledgerFixturePublisher) waitForEntry(t *testing.T, ctx context.Context) {
	t.Helper()
	if self.held() {
		waitCloseWaitBarrier(t, ctx, self.entered, "constructor key publisher")
	}
}

// A real auxiliary sender is forced in the old fixture, not inferred from a
// schedule. The isolated model exercises beginCleanup at the existing snapshot
// boundary while real owner locks are held. It never mutates the real ledger.
func TestTransferMemoryOwnerLedgerFixturePublisherQuiescence(t *testing.T) {
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var ledger TransferMemoryOwnerLedger
	settings, publisher := ledgerFixtureClientSettings()
	settings.MemoryOwnerLedger = &ledger
	settings.SendBufferSettings.SequenceBufferSize = 5
	settings.SendBufferSettings.AckBufferSize = 11
	settings.ForwardBufferSettings.SequenceBufferSize = 13
	sendEntered, releaseSend := make(chan struct{}), make(chan struct{})
	forwardEntered, releaseForward := make(chan struct{}), make(chan struct{})
	cleaning, releaseCleanup := make(chan struct{}), make(chan struct{})
	var sendOnce, forwardOnce, cleanupOnce sync.Once
	var releaseSendOnce, releaseForwardOnce, releaseCleanupOnce sync.Once
	path := DestinationId(NewId())
	settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
		if id.Destination == ControlId {
			sendOnce.Do(func() { close(sendEntered) })
			<-releaseSend
		}
	}
	settings.ForwardBufferSettings.beforeRunForwardSequenceForTest = func(destination TransferPath) {
		if destination == path {
			forwardOnce.Do(func() { close(forwardEntered) })
			<-releaseForward
		}
	}
	settings.ForwardBufferSettings.afterRunForwardSequenceForTest = func(destination TransferPath) {
		if destination == path {
			cleanupOnce.Do(func() { close(cleaning) })
			<-releaseCleanup
		}
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	var message *closeWaitPoolWitness
	admitted := false
	defer func() {
		client.Close()
		publisher.release()
		releaseSendOnce.Do(func() { close(releaseSend) })
		releaseForwardOnce.Do(func() { close(releaseForward) })
		releaseCleanupOnce.Do(func() { close(releaseCleanup) })
		cleanupCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if err := client.CloseAndWait(cleanupCtx); err != nil {
			t.Error(err)
		} else if admitted && message != nil && !message.released {
			message.requireOwnerReleased(t, "joined cause-control forward")
		}
		if message != nil {
			message.cleanup()
		}
	}()
	message = newCloseWaitPoolWitness(t)
	publicationHeld := publisher.held()
	if publicationHeld {
		publisher.waitForEntry(t, ctx)
	} else {
		waitCloseWaitBarrier(t, ctx, sendEntered, "actual constructor control sender")
	}
	accepted, err := client.forwardBuffer.Pack(&ForwardPack{
		Destination: path, Ctx: ctx, TransferFrameBytes: message.owner,
	}, 0)
	admitted = accepted
	if !accepted || err != nil {
		t.Fatalf("forward admission=%t err=%v", accepted, err)
	}
	waitCloseWaitBarrier(t, ctx, forwardEntered, "cause-control forward worker")
	client.Close()
	releaseForwardOnce.Do(func() { close(releaseForward) })
	waitCloseWaitBarrier(t, ctx, cleaning, "cause-control forward cleanup")
	stable := ledger.Snapshot()
	wantSend := int64(1)
	if publicationHeld {
		wantSend = 0
	}
	if !stable.Enabled || !stable.Complete || stable.Send.Workers != wantSend ||
		stable.Send.CleanupWorkers != 0 || stable.Send.AdmittedTotal != uint64(wantSend) ||
		stable.Send.FinishedTotal != 0 || stable.Receive.Workers != 0 ||
		stable.Forward.Workers != 1 || stable.Forward.CleanupWorkers != 1 ||
		stable.Forward.AdmittedTotal != 1 || stable.Forward.FinishedTotal != 0 {
		t.Fatalf("constructor/worker barriers did not establish the control: %+v held=%t", stable, publicationHeld)
	}
	t.Logf("cause control: publisher_held=%t real_send_workers=%d real_forward_cleanup=%d", publicationHeld, stable.Send.Workers, stable.Forward.CleanupWorkers)
	modelIssue := ""
	// ledgerSnapshotAgainstOwners runs this callback in its one joined result
	// goroutine. Report issues to the test owner; never call Fatal from here.
	sample := func() TransferMemoryOwnerSnapshot {
		actual := ledger.Snapshot()
		if actual != stable {
			modelIssue = "real held owners changed before model sampling"
			return actual
		}
		if publicationHeld {
			return actual
		}
		var modeled TransferMemoryOwnerLedger
		modeled.admit(transferMemoryOwnerSend, stable.Send.KnownChannelSlotBytes)
		modeled.admit(transferMemoryOwnerForward, stable.Forward.KnownChannelSlotBytes)
		modeled.beginCleanup(transferMemoryOwnerForward, stable.Forward.KnownChannelSlotBytes)
		if modeled.Snapshot() != stable {
			modelIssue = "isolated model did not reproduce the stable real ownership"
			return actual
		}
		view := modeled.snapshot(func(final bool) {
			if !final {
				modeled.beginCleanup(transferMemoryOwnerSend, stable.Send.KnownChannelSlotBytes)
			}
		})
		modeled.finish(transferMemoryOwnerSend, stable.Send.KnownChannelSlotBytes, true)
		modeled.finish(transferMemoryOwnerForward, stable.Forward.KnownChannelSlotBytes, true)
		joined := modeled.Snapshot()
		if view.Complete || !joined.Complete || joined.Send.Workers != 0 ||
			joined.Forward.Workers != 0 || joined.Send.CleanupWorkers != 0 ||
			joined.Forward.CleanupWorkers != 0 || joined.Send.KnownChannelSlotBytes != 0 ||
			joined.Forward.KnownChannelSlotBytes != 0 ||
			joined.Send.CleanupKnownChannelSlotBytes != 0 ||
			joined.Forward.CleanupKnownChannelSlotBytes != 0 ||
			joined.Send.AdmittedTotal != joined.Send.FinishedTotal ||
			joined.Forward.AdmittedTotal != joined.Forward.FinishedTotal {
			modelIssue = "modeled cleanup overlap or final conservation failed"
			return actual
		}
		return view
	}
	// The matched original fails HERE, in its unchanged Complete assertion.
	// The corrected fixture has no auxiliary admission and uses the real sample.
	ledgerSnapshotAgainstOwners(t, client, &ledger, sample)
	if modelIssue != "" {
		t.Fatal(modelIssue)
	}
	if !publicationHeld {
		t.Fatal("unheld publisher escaped the deterministic quiescence control")
	}
	publisher.release()
	releaseSendOnce.Do(func() { close(releaseSend) })
	releaseCleanupOnce.Do(func() { close(releaseCleanup) })
	if err := client.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	joined := ledger.Snapshot()
	if !joined.Complete || joined.Send.Workers != 0 || joined.Forward.Workers != 0 ||
		joined.Send.AdmittedTotal != joined.Send.FinishedTotal ||
		joined.Forward.AdmittedTotal != joined.Forward.FinishedTotal {
		t.Fatalf("real ownership remained after joined control: %+v", joined)
	}
	message.requireOwnerReleased(t, "cause-control forward")
}

// The gate does not remove publication: releasing it admits the actual
// ControlId send sequence, whose buffered key frame remains client-owned.
func TestTransferMemoryOwnerLedgerPublisherReleaseJoinsActualSender(t *testing.T) {
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var ledger TransferMemoryOwnerLedger
	settings := closeWaitClientSettings()
	settings.MemoryOwnerLedger = &ledger
	publisher := newLedgerFixturePublisher()
	settings.beforeClientKeyPublishForTest = publisher.wait
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
		if id.Destination == ControlId {
			enterOnce.Do(func() { close(entered) })
			<-release
		}
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer func() {
		client.Close()
		publisher.release()
		releaseOnce.Do(func() { close(release) })
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if err := client.CloseAndWait(cleanup); err != nil {
			t.Error(err)
		}
	}()
	publisher.waitForEntry(t, ctx)
	if held := ledger.Snapshot(); !held.Enabled || !held.Complete || held.Revision != 0 {
		t.Fatalf("held constructor publisher admitted transfer work: %+v", held)
	}
	publisher.release()
	waitCloseWaitBarrier(t, ctx, entered, "released constructor control sender")
	admitted := ledgerSnapshotAgainstOwners(t, client, &ledger)
	if admitted.Send.Workers != 1 || admitted.Send.CleanupWorkers != 0 ||
		admitted.Send.AdmittedTotal != 1 || admitted.Send.FinishedTotal != 0 ||
		admitted.Send.KnownChannelSlotBytes == 0 {
		t.Fatalf("released constructor publisher did not own its sender: %+v", admitted)
	}
	client.Close()
	releaseOnce.Do(func() { close(release) })
	if err := client.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	joined := ledger.Snapshot()
	if !joined.Complete || joined.Send.Workers != 0 || joined.Send.CleanupWorkers != 0 ||
		joined.Send.KnownChannelSlotBytes != 0 || joined.Send.CleanupKnownChannelSlotBytes != 0 ||
		joined.Send.AdmittedTotal != 1 || joined.Send.FinishedTotal != 1 {
		t.Fatalf("constructor sender retained ownership after join: %+v", joined)
	}
}

// All three beginCleanup sites precede the relevant owner-map mutex. These
// isolated counters exercise that permitted boundary without touching a real
// worker's counters or pretending that its eventual finish already happened.
func TestTransferMemoryOwnerLedgerCleanupOverlapIsNotQuiescent(t *testing.T) {
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	settings := closeWaitClientSettings()
	publisher := newLedgerFixturePublisher()
	settings.beforeClientKeyPublishForTest = publisher.wait
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer func() {
		client.Close()
		publisher.release()
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if err := client.CloseAndWait(cleanup); err != nil {
			t.Error(err)
		}
	}()
	publisher.waitForEntry(t, ctx)
	for _, kind := range []transferMemoryOwnerKind{
		transferMemoryOwnerSend, transferMemoryOwnerReceive, transferMemoryOwnerForward,
	} {
		var modeled TransferMemoryOwnerLedger
		const slots = 32
		modeled.admit(kind, slots)
		view := func() TransferMemoryOwnerSnapshot {
			client.sendBuffer.mutex.Lock()
			defer client.sendBuffer.mutex.Unlock()
			client.receiveBuffer.mutex.Lock()
			defer client.receiveBuffer.mutex.Unlock()
			client.forwardBuffer.mutex.Lock()
			defer client.forwardBuffer.mutex.Unlock()
			return modeled.snapshot(func(final bool) {
				if !final {
					modeled.beginCleanup(kind, slots)
				}
			})
		}()
		group := modeled.counters(kind).snapshot(0)
		if !view.Enabled || view.Complete || view.Revision != 2 || group.Workers != 1 ||
			group.CleanupWorkers != 1 || group.KnownChannelSlotBytes != slots ||
			group.CleanupKnownChannelSlotBytes != slots || group.AdmittedTotal != 1 ||
			group.FinishedTotal != 0 {
			t.Fatalf("owner locks incorrectly implied quiescence, kind=%d: %+v", kind, view)
		}
		modeled.finish(kind, slots, true)
		joined := modeled.Snapshot()
		group = modeled.counters(kind).snapshot(0)
		if !joined.Complete || group.Workers != 0 || group.CleanupWorkers != 0 ||
			group.KnownChannelSlotBytes != 0 || group.CleanupKnownChannelSlotBytes != 0 ||
			group.AdmittedTotal != 1 || group.FinishedTotal != 1 {
			t.Fatalf("modeled lifecycle was not reconciled, kind=%d: %+v", kind, joined)
		}
	}
}
