// Real native queue handoffs exercise selection receipts and connect generations.
// Producer return, source dequeue and peer acknowledgement remain separate edges.
package connect

import (
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Populate the production failure lookup while retaining the canonical fixture's
// explicit routing seam. It changes no queue, source or admission result.
func nativeSynClockBoundaryRegister(f *groupDispositionQueueFixture, path *IpPath) {
	f.parent.settings.DialFailureRerace = true
	f.update.ipPath = path
	f.parent.stateLock.Lock()
	if f.parent.ip4PathUpdates == nil {
		f.parent.ip4PathUpdates = map[Ip4Path]*multiClientChannelUpdate{}
	}
	f.parent.ip4PathUpdates[path.ToIp4Path()] = f.update
	if f.parent.flowUpdates == nil {
		f.parent.flowUpdates = map[*multiClientChannelUpdate]bool{}
	}
	f.parent.flowUpdates[f.update] = true
	if f.parent.clientUpdates[f.selected] == nil {
		f.parent.clientUpdates[f.selected] = map[*multiClientChannelUpdate]bool{}
	}
	f.parent.clientUpdates[f.selected][f.update] = true
	f.parent.stateLock.Unlock()
}

// Prepare witnesses on the test owner before a producer goroutine starts.
// The real native entry takes every pooled original, including refusals.
func nativeSynClockBoundaryPackets(f *groupDispositionQueueFixture, template []byte) [][]byte {
	packet := MessagePoolCopy(template)
	f.witnesses = append(f.witnesses, MessagePoolShareReadOnly(packet))
	return [][]byte{packet}
}

// Keep the actual native per-input receipt; a queue callback is not substituted
// for the public result or for a physical route write.
func nativeSynClockBoundaryOffer(t *testing.T, f *groupDispositionQueueFixture, template []byte) bool {
	t.Helper()
	accepted := [1]bool{}
	count := f.parent.SendNativePacketBatchWithResults(f.source, protocol.ProvideMode_Network,
		nativeSynClockBoundaryPackets(f, template), 5*time.Second, accepted[:])
	synctest.Wait()
	if count < 0 || count > 1 || (count == 1) != accepted[0] {
		t.Fatalf("native clock boundary result count=%d accepted=%t", count, accepted[0])
	}
	return accepted[0]
}

// The old A receipt has already crossed irreversible enqueue when selection
// leaves A and returns to the same channel with a new receipt. Neither its late
// admission callback nor later real dequeue may arm the replacement's clock.
func TestNativeSynAdmissionClockDelayedAdmissionRejectsSelectionAba(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		resume := func() { releaseOnce.Do(func() { close(release) }) }
		var admitted atomic.Int64
		f := newGroupDispositionQueueFixture(t, nil, func(settings *ClientSettings) {
			settings.SendBufferSettings.beforeGroupAdmissionForTest = func() {
				close(entered)
				<-release
			}
			settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
				if group, ok := target.(*parsedPacketGroup); ok && group.ipPath.Syn && !group.ipPath.Ack {
					admitted.Add(1)
				}
			}
		})
		t.Cleanup(resume)
		template := groupDispositionControlPacket(81000, 0, 64, tcpFlagSyn)
		path := groupRecoveryParsed(t, template).ipPath
		nativeSynClockBoundaryRegister(f, path)
		original := f.update.client.loadSelection()
		packets := nativeSynClockBoundaryPackets(f, template)
		result := make(chan bool, 1)
		producerDone := make(chan struct{})
		go func() {
			defer close(producerDone)
			accepted := [1]bool{}
			count := f.parent.SendNativePacketBatchWithResults(f.source, protocol.ProvideMode_Network,
				packets, 5*time.Second, accepted[:])
			result <- count == 1 && accepted[0]
		}()
		t.Cleanup(func() {
			resume()
			f.cancel()
			<-producerDone
		})
		f.requireParked()
		select {
		case <-entered:
		default:
			t.Fatal("native SYN did not reach the actual post-enqueue admission gap")
		}
		if admitted.Load() != 0 || len(f.route) != 0 {
			t.Fatal("source dequeue or admission observation bypassed the two explicit barriers")
		}
		b := newPacketTransferTestChannel()
		b.ctx, b.settings = f.parent.ctx, f.parent.settings
		f.update.stateLock.Lock()
		f.update.selectAffinityClient(b)
		middle := f.update.client.loadSelection()
		f.update.selectAffinityClient(f.selected)
		replacement := f.update.client.loadSelection()
		f.update.stateLock.Unlock()
		if original == nil || middle == original || middle == replacement || replacement == original || replacement.client != f.selected {
			t.Fatal("selection ABA did not create a distinct current receipt for the same A channel")
		}
		resume()
		if !<-result {
			t.Fatal("irreversibly queued native SYN lost its successful public admission result")
		}
		synctest.Wait()
		if admitted.Load() != 1 || len(f.route) != 0 {
			t.Fatal("late producer did not complete one real admission while its source remained parked")
		}
		f.update.stateLock.Lock()
		admissionClockEmpty := f.update.synWaitStart.IsZero() && f.update.synWaitClient == nil
		f.update.stateLock.Unlock()
		if !admissionClockEmpty || f.update.client.loadSelection() != replacement {
			t.Error("NATIVE_SYN_STALE_SELECTION_ADMISSION_CLOCK: stale A admission armed or replaced current A receipt")
		}
		f.unpark()
		f.acknowledgeAll(template)
		f.update.stateLock.Lock()
		dequeueClockEmpty := f.update.synWaitStart.IsZero() && f.update.synWaitClient == nil
		generation := f.update.synGenerationNumber
		f.update.stateLock.Unlock()
		if !dequeueClockEmpty || f.update.client.loadSelection() != replacement || generation != path.SequenceNumber || f.terminal.Load() != 1 {
			t.Errorf("NATIVE_SYN_STALE_SELECTION_DEQUEUE_CLOCK: empty=%t receipt_preserved=%t generation=%d terminals=%d",
				dequeueClockEmpty, f.update.client.loadSelection() == replacement, generation, f.terminal.Load())
		}
	})
}

// With no selection change, the identical delayed native handoff must arm at
// accepted admission while the source is parked, then retain that timestamp
// through later source order and its actual reliable wire acknowledgement.
func TestNativeSynAdmissionClockDelayedCurrentReceiptArmsBeforeDequeue(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		resume := func() { releaseOnce.Do(func() { close(release) }) }
		f := newGroupDispositionQueueFixture(t, nil, func(settings *ClientSettings) {
			settings.SendBufferSettings.beforeGroupAdmissionForTest = func() {
				close(entered)
				<-release
			}
		})
		t.Cleanup(resume)
		template := groupDispositionControlPacket(82000, 0, 64, tcpFlagSyn)
		path := groupRecoveryParsed(t, template).ipPath
		nativeSynClockBoundaryRegister(f, path)
		selection := f.update.client.loadSelection()
		packets := nativeSynClockBoundaryPackets(f, template)
		result := make(chan bool, 1)
		producerDone := make(chan struct{})
		go func() {
			defer close(producerDone)
			accepted := [1]bool{}
			count := f.parent.SendNativePacketBatchWithResults(f.source, protocol.ProvideMode_Network,
				packets, 5*time.Second, accepted[:])
			result <- count == 1 && accepted[0]
		}()
		t.Cleanup(func() {
			resume()
			f.cancel()
			<-producerDone
		})
		f.requireParked()
		select {
		case <-entered:
		default:
			t.Fatal("current-receipt native SYN missed the real post-enqueue gap")
		}
		acceptedAt := time.Now()
		resume()
		if !<-result {
			t.Fatal("current-receipt native handoff did not return its actual successful admission")
		}
		f.update.stateLock.Lock()
		clock, client := f.update.synWaitStart, f.update.synWaitClient
		f.update.stateLock.Unlock()
		if clock.IsZero() || clock.Before(acceptedAt) || client != f.selected || len(f.route) != 0 {
			t.Error("NATIVE_SYN_CURRENT_SELECTION_MISSING_ADMISSION_CLOCK: current accepted receipt did not arm before source dequeue")
		}
		time.Sleep(time.Second)
		f.unpark()
		f.acknowledgeAll(template)
		f.update.stateLock.Lock()
		preserved := f.update.synWaitStart.Equal(clock) && f.update.synWaitClient == client
		f.update.stateLock.Unlock()
		if !preserved || f.update.client.loadSelection() != selection || f.terminal.Load() != 1 {
			t.Fatal("source dequeue or ACK renewed a valid admission clock or changed its current selection")
		}
	})
}

// The old SYN owns the actual one-slot source queue. A different-ISN offer
// after the old clock matures is refused locally and cannot spend or replace
// that clock. Only the later successful source-ordered new SYN may replace it.
func TestNativeSynAdmissionClockRefusedNewGenerationPreservesAcceptedClock(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var admitted atomic.Int64
		f := newGroupDispositionQueueFixture(t, nil, func(settings *ClientSettings) {
			settings.SendBufferSettings.SequenceBufferSize = 1
			settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
				if group, ok := target.(*parsedPacketGroup); ok && group.ipPath.Syn && !group.ipPath.Ack {
					admitted.Add(1)
				}
			}
		})
		old := groupDispositionControlPacket(83000, 0, 64, tcpFlagSyn)
		next := groupDispositionControlPacket(84000, 0, 64, tcpFlagSyn)
		path := groupRecoveryParsed(t, old).ipPath
		nativeSynClockBoundaryRegister(f, path)
		selection := f.update.client.loadSelection()
		if !nativeSynClockBoundaryOffer(t, f, old) || admitted.Load() != 1 {
			t.Fatal("old SYN did not fill the real one-slot source queue through native admission")
		}
		f.requireParked()
		f.update.stateLock.Lock()
		clock, client := f.update.synWaitStart, f.update.synWaitClient
		f.update.stateLock.Unlock()
		if clock.IsZero() || client != f.selected || len(f.route) != 0 {
			t.Fatal("accepted old SYN lacks its pre-dequeue admission clock")
		}
		time.Sleep(inferredDialFailureTimeout + time.Nanosecond)
		if nativeSynClockBoundaryOffer(t, f, next) || admitted.Load() != 1 || len(f.route) != 0 {
			t.Fatal("different-ISN SYN bypassed the occupied actual source queue")
		}
		f.update.stateLock.Lock()
		preserved := f.update.synWaitStart.Equal(clock) && f.update.synWaitClient == client
		f.update.stateLock.Unlock()
		key, ok := destinationServiceFailureKeyFor(path, f.selected)
		if !ok {
			t.Fatal("generation control lacks an exact provider/service key")
		}
		f.parent.stateLock.Lock()
		failure := f.parent.destinationServiceFailures[key]
		f.parent.stateLock.Unlock()
		if !preserved || f.update.client.loadSelection() != selection || failure.count != 0 ||
			f.selected.dialFailureCount() != 0 || f.parent.ReliabilityMetrics().FlowsReraced != 0 {
			t.Fatal("NATIVE_SYN_REFUSED_GENERATION_SPENT_ACCEPTED_CLOCK: unadmitted new ISN changed the accepted old cohort or blamed its provider")
		}
		f.unpark()
		f.acknowledgeAll(old)
		f.update.stateLock.Lock()
		oldOrdered := f.update.synGenerationSeen && f.update.synGenerationNumber == 83000 && f.update.synWaitStart.Equal(clock)
		f.update.stateLock.Unlock()
		if !oldOrdered {
			t.Fatal("real old-SYN dequeue failed to preserve its earlier accepted clock")
		}
		acceptedAt := time.Now()
		if !nativeSynClockBoundaryOffer(t, f, next) || admitted.Load() != 2 {
			t.Fatal("same refused new SYN lost retry authority after the source queue drained")
		}
		f.acknowledgeAll(next)
		f.update.stateLock.Lock()
		newOrdered := f.update.synGenerationSeen && f.update.synGenerationNumber == 84000 &&
			!f.update.synWaitStart.Before(acceptedAt) && f.update.synWaitClient == f.selected
		f.update.stateLock.Unlock()
		if !newOrdered || f.update.client.loadSelection() != selection {
			t.Fatal("actual admitted new-SYN source order did not replace only its own generation and clock")
		}
	})
}
