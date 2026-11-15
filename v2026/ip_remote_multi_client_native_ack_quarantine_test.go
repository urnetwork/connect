//go:build !js && !windows

// Quarantine and actual source reset share one flow's retained retry lifetime.
package connect

import (
	"sync"
	"testing"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The first signal precedes both conflicting accesses. Retirement releases
// the source only after its parent-owned phase, without ordering those accesses.
func TestNativeAckRetryQuarantineSerializesWithSourceReset(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	for _, resetSource := range []bool{true, false} {
		f := newNativeAckRetryOwnerFixture(t, 4)
		parent, update := f.parent, f.update
		client := update.client.Load()
		parent.settings = DefaultMultiClientSettings()
		parent.log = NewNoopLogger()
		parent.reliabilityMetrics = newReliabilityMetrics()
		parent.flowUpdates = map[*multiClientChannelUpdate]bool{update: true}
		parent.clientUpdates = map[*multiClientChannel]map[*multiClientChannelUpdate]bool{
			client: {update: true},
		}
		parent.removalReceiveQueue = make(chan receivePacket, 1)
		client.settings = parent.settings

		group := f.prepare(t, f.packet)
		owner := f.retain(t, group)
		attempt := owner.attempt
		credit := owner.credit
		witness := MessagePoolShareReadOnly(owner.packet)
		t.Cleanup(func() {
			if witness != nil {
				MessagePoolReturn(witness)
			}
		})
		if !attempt.current() || !f.source.protocolAdmission.protocolReadinessGate().Load() {
			t.Fatal("the exact live feedback owner did not wait on actual full protocol capacity")
		}
		select {
		case <-parent.flowReaperWake:
		default:
			t.Fatal("actual retry registration did not wake its parent")
		}

		resetBytes := reraceTcpPacket(update.ipPath, tcpFlagSyn, 101, 500, nil)
		resetPath, payload, err := ParseIpPathWithPayload(resetBytes)
		if err != nil || !resetPath.Syn {
			t.Fatal("source reset lacks its actual parsed SYN")
		}
		resetPacket := parsedPacket{packet: resetBytes, ipPath: resetPath, payload: payload}
		locked := make(chan bool)
		release := make(chan struct{})
		resetDone := make(chan struct{})
		parentRetired := make(chan struct{})
		migrationDone := make(chan struct{})
		var releaseOnce, cancelOnce sync.Once
		var fencedBeforeClose bool
		actualCancel := update.cancel
		update.cancel = func() {
			actualCancel()
			cancelOnce.Do(func() { close(parentRetired) })
		}

		go func() {
			defer close(resetDone)
			update.stateLock.Lock()
			defer update.stateLock.Unlock()
			live := !update.IsDone() && !update.sourceReset.Load() &&
				update.client.Load() == client && f.source.ctx.Err() == nil
			locked <- live
			if live && resetSource {
				// The real source holds this lock after its live-owner guard.
				// Do not send a reset-complete signal to migration: that would
				// hide the missing synchronization in the original implementation.
				update.resetSequenceWithLock(&resetPacket)
			}
			<-release
			// Cancellation is permanent even if the admitted source SYN reset
			// cleared sourceReset. Close still cannot take this held flow lock.
			fencedBeforeClose = !attempt.currentWithLock()
		}()
		wasLive := <-locked
		var rebound, replacements, remaining int
		go func() {
			defer close(migrationDone)
			rebound, replacements, remaining = parent.migrateClientFlows(client, "bench")
		}()
		t.Cleanup(func() {
			releaseOnce.Do(func() { close(release) })
			<-resetDone
			<-migrationDone
		})
		select {
		case <-parentRetired:
		case <-migrationDone:
			t.Fatal("quarantine returned without canceling the retired generation")
		}
		releaseOnce.Do(func() { close(release) })
		<-resetDone
		<-migrationDone

		if !wasLive || !fencedBeforeClose || rebound != 0 || replacements != 0 || remaining != 0 {
			t.Fatal("the live source and actual quarantine did not complete their intended transitions")
		}
		if !update.IsDone() || update.client.Load() != nil || attempt.current() ||
			parent.ip4PathUpdates[update.ipPath.ToIp4Path()] != nil || parent.flowUpdates[update] ||
			len(parent.clientUpdates[client]) != 0 || parent.ctx.Err() != nil ||
			client.IsDone() || f.source.ctx.Err() != nil {
			t.Fatal("quarantine did not fence exactly the old flow while its shared owners remained live")
		}
		select {
		case reset := <-parent.removalReceiveQueue:
			path, err := ParseIpPath(reset.Packet)
			valid := err == nil && path.Rst && reset.generation == update && len(parent.removalReceiveQueue) == 0
			if reset.Pooled {
				MessagePoolReturn(reset.Packet)
			}
			if !valid {
				t.Fatal("quarantine did not retain exactly its old-generation source reset")
			}
		default:
			t.Fatal("quarantine failed to publish its actual source reset")
		}
		select {
		case <-parent.flowReaperWake:
		default:
			t.Fatal("safe flow close did not wake the retained feedback owner")
		}
		parent.drainNativeAckRetries()
		if !credit.released.Load() || owner.packet != nil || owner.credit != nil || owner.memoryByteCount != 0 ||
			owner.indexed || owner.installation != 0 || update.nativeAckRetry != nil ||
			parent.nativeAckRetryCount != 0 || parent.nativeAckRetryHead != nil || parent.nativeAckRetryTail != nil {
			t.Fatal("retirement failed to return the exact retained packet, token and readiness registration")
		}
		returned := MessagePoolReturn(witness)
		witness = nil
		if !returned {
			t.Fatal("retirement left an extra share of the copied native feedback packet")
		}

		// Reopen actual protocol capacity after retirement. Neither the finite
		// reaper nor a late original refusal may recreate the canceled generation.
		f.source.protocolAdmission.release(sendSchedulingKey{})
		parent.drainNativeAckRetries()
		parent.retainNativeAckRetry(group, &attempt, owner.inputSource, protocol.ProvideMode_Network)
		if attempt.current() || update.nativeAckRetry != nil || parent.nativeAckRetryCount != 0 ||
			parent.nativeAckRetryHead != nil || parent.nativeAckRetryTail != nil || len(parent.removalReceiveQueue) != 0 {
			t.Fatal("later capacity or a stale original refusal revived the retired feedback owner")
		}
		if acquired, closed, _ := f.source.protocolAdmission.tryAcquire(sendSchedulingKey{}, false); !acquired || closed {
			t.Fatal("retiring one flow damaged the independent live source's protocol capacity")
		}
	}
}
