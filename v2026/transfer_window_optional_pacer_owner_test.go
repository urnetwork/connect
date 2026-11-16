// Observer ownership follows the measured transfer key and fixture lifetime.
package connect

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// Real sequence workers reach the existing hooks without any offered payload.
// Explicit barriers place replacement Run startup on either side of cleanup.
type windowOptionalPacerOwnerFixture struct {
	ctx          context.Context
	cancel       context.CancelFunc
	sender       *Client
	trace        *windowOptionalPacerTrace
	key          sendSequenceId
	canceledAcks atomic.Int32
}

// The fixture joins both clients before the enclosing message-pool check.
func runWindowOptionalPacerOwnerFixture(t *testing.T, check func(*testing.T, *windowOptionalPacerOwnerFixture)) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		fixture := &windowOptionalPacerOwnerFixture{ctx: ctx, cancel: cancel, trace: &windowOptionalPacerTrace{}}
		settings := func() *ClientSettings {
			s := DefaultClientSettings()
			s.Log = NewNoopLogger()
			s.EncryptionSettings.Mode = EncryptionModeOff
			s.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
			s.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
			s.SendBufferSettings.ApplyWindowSizing()
			return s
		}
		senderSettings := settings()
		senderSettings.SendBufferSettings.afterApplyAckSnapshotForTest = func(id sendSequenceId) {
			if id == fixture.key && ctx.Err() != nil {
				fixture.canceledAcks.Add(1)
			}
		}
		fixture.sender = NewClient(ctx, NewId(), NewNoContractClientOob(), senderSettings)
		receiver := NewClient(ctx, NewId(), NewNoContractClientOob(), settings())
		fixture.key = sendSequenceId{Destination: receiver.ClientId()}
		fixture.sender.ContractManager().AddNoContractPeer(receiver.ClientId())
		fixture.trace.install(windowPathCell{Arm: "window-only"}, fixture.sender, receiver)
		defer func() {
			cancel()
			for _, client := range []*Client{fixture.sender, receiver} {
				if err := client.CloseAndWait(context.Background()); err != nil {
					t.Error(err)
				}
			}
		}()
		check(t, fixture)
	})
}

// Constructs the complete key through the real buffer admission boundary.
func (self *windowOptionalPacerOwnerFixture) create(t *testing.T, key sendSequenceId) *SendSequence {
	t.Helper()
	sequence := self.sender.sendBuffer.createSendSequence(key, &SendPack{
		TransferOptions: TransferOptions{CompanionContract: key.CompanionContract, ForceStream: key.ForceStream},
		Destination:     key.Destination, Ctx: self.ctx,
		EncryptionRole: key.EncryptionRole, EncryptionCompanion: key.EncryptionCompanion,
		logicalLane: key.LogicalLane, logicalLaneExplicit: true,
	})
	if sequence == nil {
		t.Fatal("sequence admission failed")
	}
	return sequence
}

// An unrelated owner cannot claim the first record just by running first.
func TestWindowOptionalPacerTraceSelectsDataOwner(t *testing.T) {
	runWindowOptionalPacerOwnerFixture(t, func(t *testing.T, fixture *windowOptionalPacerOwnerFixture) {
		otherKey := fixture.key
		otherKey.Destination = NewId()
		fixture.create(t, otherKey)
		synctest.Wait()
		selected := fixture.create(t, fixture.key)
		synctest.Wait()
		fixture.trace.stateLock.Lock()
		defer fixture.trace.stateLock.Unlock()
		if fixture.trace.sequence != selected.sequenceId || fixture.trace.cutCount == 0 || fixture.trace.conflict {
			t.Error("unselected destination displaced the measured data owner")
		}
	})
}

// Every routing-key component separates independent owners in one buffer.
func TestWindowOptionalPacerTraceIgnoresOtherTransferKeys(t *testing.T) {
	runWindowOptionalPacerOwnerFixture(t, func(t *testing.T, fixture *windowOptionalPacerOwnerFixture) {
		fixture.create(t, fixture.key)
		synctest.Wait()
		before := fixture.trace.cutCount
		otherKeys := []sendSequenceId{fixture.key, fixture.key, fixture.key, fixture.key, fixture.key, fixture.key}
		otherKeys[0].Destination = NewId()
		otherKeys[1].CompanionContract = true
		otherKeys[2].ForceStream = true
		otherKeys[3].EncryptionRole = sequenceTlsRoleServer
		otherKeys[4].EncryptionCompanion = true
		otherKeys[5].LogicalLane = 1
		for _, key := range otherKeys {
			fixture.create(t, key)
			synctest.Wait()
		}
		fixture.trace.stateLock.Lock()
		defer fixture.trace.stateLock.Unlock()
		if before == 0 || fixture.trace.cutCount != before || fixture.trace.conflict {
			t.Error("independent transfer keys contaminated the single-owner trace")
		}
	})
}

// A live replacement for the selected key still violates the gate's premise.
func TestWindowOptionalPacerTraceRejectsLiveReplacement(t *testing.T) {
	runWindowOptionalPacerOwnerFixture(t, func(t *testing.T, fixture *windowOptionalPacerOwnerFixture) {
		original := fixture.create(t, fixture.key)
		synctest.Wait()
		original.Cancel()
		<-original.done
		fixture.create(t, fixture.key)
		synctest.Wait()
		fixture.trace.stateLock.Lock()
		defer fixture.trace.stateLock.Unlock()
		if !fixture.trace.conflict {
			t.Error("live generation replacement escaped the single-owner gate")
		}
	})
}

// A worker admitted before cancellation may first reach Run-owned hooks while
// cleanup joins it. Those hooks are outside the completed fixture interval.
func TestWindowOptionalPacerTraceExcludesCleanupStartup(t *testing.T) {
	runWindowOptionalPacerOwnerFixture(t, func(t *testing.T, fixture *windowOptionalPacerOwnerFixture) {
		original := fixture.create(t, fixture.key)
		synctest.Wait()
		before := fixture.trace.cutCount
		original.Cancel()
		<-original.done
		entered, release := make(chan struct{}), make(chan struct{})
		previousRun := fixture.sender.sendBuffer.beforeRunSendSequenceForTest
		fixture.sender.sendBuffer.beforeRunSendSequenceForTest = func(id sendSequenceId) {
			close(entered)
			<-release
			previousRun(id)
		}
		replacement := fixture.create(t, fixture.key)
		<-entered
		fixture.cancel()
		close(release)
		<-replacement.done
		fixture.trace.stateLock.Lock()
		defer fixture.trace.stateLock.Unlock()
		if fixture.canceledAcks.Load() != 1 {
			t.Error("cleanup worker did not reach the real canceled ACK-snapshot hook")
		}
		if before == 0 || fixture.trace.cutCount != before || fixture.trace.conflict {
			t.Error("cleanup startup contaminated the completed single-owner trace")
		}
	})
}
