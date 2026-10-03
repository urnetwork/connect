// Accepted feedback must survive ACK-worker scheduling and every owner-side
// expiry check. Barriers belong to workers, not the coalescer they exercise.
package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A worker-only pause remains meaningful if ACK publication later moves to
// the receiving caller. Cleanup always releases it before joining workers.
type ackHandoffLifetimeBarrier struct {
	once    sync.Once
	reached chan struct{}
	release chan struct{}
	resume  sync.Once
}

// Constructs a closed-by-owner barrier without timers or scheduler polling.
func newAckHandoffLifetimeBarrier() *ackHandoffLifetimeBarrier {
	return &ackHandoffLifetimeBarrier{reached: make(chan struct{}), release: make(chan struct{})}
}

// Stops just the first selected worker visit.
func (self *ackHandoffLifetimeBarrier) wait() {
	self.once.Do(func() {
		close(self.reached)
		<-self.release
	})
}

// Releases the worker exactly once, including assertion-failure unwinding.
func (self *ackHandoffLifetimeBarrier) close() {
	self.resume.Do(func() { close(self.release) })
}

// Quiescence proves the selected worker reached its barrier; a timer is not
// used to manufacture an otherwise uncertain interleaving.
func (self *ackHandoffLifetimeBarrier) requireReached(t *testing.T) {
	t.Helper()
	synctest.Wait()
	select {
	case <-self.reached:
	default:
		t.Fatal("selected ACK worker did not reach the deterministic barrier")
	}
}

// Constructor-owned ACK publication has no queue worker. Keep the timing
// barrier for an explicit legacy channel, and otherwise require the actual
// inline owner before advancing the unchanged lifetime controls.
func (self *ackHandoffLifetimeBarrier) requireReachedOrInline(t *testing.T, sequence *SendSequence) {
	t.Helper()
	if sequence.acks == nil && sequence.ackWindow != nil && sequence.resendQueue != nil {
		synctest.Wait()
		return
	}
	self.requireReached(t)
}

// The receiving callback returned accepted while the bounded channel still
// owns the exact cumulative ACK. The original lifetime must not expire it.
func TestTransferAckAcceptedBeforeWorkerReceiveKeepsLifetime(t *testing.T) {
	runAckHandoffLifetime(t, false)
}

// Removing an ACK from its channel does not make it processed: this case
// protects the worker-owned interval that a channel-only drain cannot see.
func TestTransferAckAcceptedBeforeWorkerCoalesceKeepsLifetime(t *testing.T) {
	runAckHandoffLifetime(t, true)
}

// Only scheduling changes: the receiver delivered the real Pack, its ACK was
// accepted at 29 seconds, and the real sender keeps its 30-second lifetime.
func runAckHandoffLifetime(t *testing.T, dequeued bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		barrier := newAckHandoffLifetimeBarrier()
		defer barrier.close()
		var destination Id
		fixture, _, _ := newAckRetirementFixture(t, TransportTypeH1, 2, NewNoopLogger(), func(settings *ClientSettings) {
			pause := func(id sendSequenceId) {
				if id.Destination == destination {
					barrier.wait()
				}
			}
			if dequeued {
				settings.SendBufferSettings.beforeAckWorkerCoalesceForTest = pause
			} else {
				settings.SendBufferSettings.beforeAckWorkerReceiveForTest = pause
			}
		})
		destination = fixture.receiver.ClientId()
		original := fixture.write(64)
		sequence := fixture.sequence()
		messageId := RequireIdFromBytes(original.pack.MessageId)
		if sequence.resendQueue.GetByMessageId(messageId) == nil || sequence.sendBufferSettings.AckTimeout != 30*time.Second {
			t.Fatal("fixture must retain the real message with the unchanged 30-second lifetime")
		}
		deadline := time.Now().Add(sequence.sendBufferSettings.AckTimeout)
		number := original.pack.SequenceNumber
		reply := fixture.receive(original)
		if reply.ack.Selective || RequireIdFromBytes(reply.ack.MessageId) != messageId || fixture.deliveredCount != 1 {
			t.Fatal("receiver did not publish the exact cumulative delivery acknowledgement")
		}
		time.Sleep(time.Until(deadline.Add(-time.Second)))
		accepted, err := sequence.ackMessageDetailed(receiveAckMessage{
			sequenceId: sequence.sequenceId, messageId: messageId,
		}, 0)
		if accepted != receiveAckHandoffAccepted || err != nil {
			t.Fatalf("predeadline ACK was not accepted: result=%d err=%v", accepted, err)
		}
		synctest.Wait()
		select {
		case <-barrier.reached:
		default:
			// A receiving caller may publish synchronously. Do not require it
			// to visit an obsolete worker handoff merely to satisfy this test.
			if fixture.ackedCount != 1 && !sequence.ackWindow.pendingDeliveryFor(number, messageId) {
				t.Fatal("accepted ACK is neither worker-owned nor already published")
			}
		}
		time.Sleep(time.Until(deadline.Add(time.Nanosecond)))
		synctest.Wait()
		if sequence.ctx.Err() != nil {
			t.Fatal("timely accepted cumulative ACK expired while its ACK worker was paused")
		}
		barrier.close()
		synctest.Wait()
		if fixture.ackedCount != 1 || sequence.resendQueue.Len() != 0 || len(sequence.ackLifetimes.items) != 0 {
			t.Fatal("resumed acknowledgement did not complete and release exactly one retained owner")
		}
	})
}

// Unknown message identities cannot keep an unrelated retained owner alive.
func TestTransferAckHandoffUnknownCannotExtendLifetime(t *testing.T) {
	runAckHandoffInvalidLifetime(t, "unknown")
}

// A stale sequence identity is rejected before any feedback is published.
func TestTransferAckHandoffWrongSequenceCannotExtendLifetime(t *testing.T) {
	runAckHandoffInvalidLifetime(t, "wrong_sequence")
}

// A later SACK does not cover the missing earlier cumulative prefix.
func TestTransferAckHandoffUnrelatedSackCannotExtendLifetime(t *testing.T) {
	runAckHandoffInvalidLifetime(t, "unrelated_sack")
}

// No queue admission or direct publication can resurrect a closed sequence.
func TestTransferAckHandoffAfterCloseCannotExtendLifetime(t *testing.T) {
	runAckHandoffInvalidLifetime(t, "after_close")
}

// Worker scheduling remains identical to the positive control. Only feedback
// identity or arrival after actual cancellation changes the outcome.
func runAckHandoffInvalidLifetime(t *testing.T, kind string) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		barrier := newAckHandoffLifetimeBarrier()
		defer barrier.close()
		var destination Id
		fixture, _, _ := newAckRetirementFixture(t, TransportTypeH1, 2, NewNoopLogger(), func(settings *ClientSettings) {
			settings.SendBufferSettings.beforeAckWorkerReceiveForTest = func(id sendSequenceId) {
				if id.Destination == destination {
					barrier.wait()
				}
			}
		})
		destination = fixture.receiver.ClientId()
		original := fixture.write(64)
		sequence := fixture.sequence()
		messageId := RequireIdFromBytes(original.pack.MessageId)
		deadline := time.Now().Add(sequence.sendBufferSettings.AckTimeout)
		ack := receiveAckMessage{sequenceId: sequence.sequenceId, messageId: messageId}
		switch kind {
		case "unknown":
			ack.messageId = NewId()
		case "wrong_sequence":
			ack.sequenceId = NewId()
		case "unrelated_sack":
			time.Sleep(time.Second)
			younger := fixture.write(64)
			ack.messageId = RequireIdFromBytes(younger.pack.MessageId)
			ack.selective = true
		}
		barrier.requireReachedOrInline(t, sequence)
		arrival := deadline.Add(-time.Second)
		if kind == "after_close" {
			arrival = deadline.Add(time.Nanosecond)
		}
		time.Sleep(time.Until(arrival))
		synctest.Wait()
		result, err := sequence.ackMessageDetailed(ack, 0)
		switch kind {
		case "wrong_sequence":
			if result != receiveAckHandoffSequenceMissing || err != nil {
				t.Fatalf("wrong-sequence handoff=%d err=%v", result, err)
			}
		case "after_close":
			if result != receiveAckHandoffSequenceClosed || err == nil {
				t.Fatalf("closed-sequence handoff=%d err=%v", result, err)
			}
		default:
			if result != receiveAckHandoffAccepted || err != nil {
				t.Fatalf("negative-control handoff=%d err=%v", result, err)
			}
		}
		time.Sleep(max(0, time.Until(deadline.Add(time.Nanosecond))))
		synctest.Wait()
		if sequence.ctx.Err() == nil {
			t.Fatal("noncovering feedback postponed the original lifetime")
		}
		barrier.close()
		synctest.Wait()
		if fixture.ackedCount != 0 || sequence.resendQueue.Len() != 0 || len(sequence.ackLifetimes.items) != 0 {
			t.Fatal("negative feedback claimed delivery or retained canceled ownership")
		}
	})
}

// One accepted selective receipt grants only its existing arrival-based
// renewal. A paused worker must not turn that finite renewal into a lease.
func TestTransferAckHandoffSackRenewsOnlyOnce(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		barrier := newAckHandoffLifetimeBarrier()
		defer barrier.close()
		var destination Id
		fixture, _, _ := newAckRetirementFixture(t, TransportTypeH1, 2, NewNoopLogger(), func(settings *ClientSettings) {
			settings.SendBufferSettings.beforeAckWorkerReceiveForTest = func(id sendSequenceId) {
				if id.Destination == destination {
					barrier.wait()
				}
			}
		})
		destination = fixture.receiver.ClientId()
		original := fixture.write(64)
		sequence := fixture.sequence()
		messageId := RequireIdFromBytes(original.pack.MessageId)
		lifetime := sequence.sendBufferSettings.AckTimeout
		deadline := time.Now().Add(lifetime)
		barrier.requireReachedOrInline(t, sequence)
		time.Sleep(time.Until(deadline.Add(-time.Second)))
		renewedDeadline := time.Now().Add(lifetime)
		result, err := sequence.ackMessageDetailed(receiveAckMessage{
			sequenceId: sequence.sequenceId, messageId: messageId, selective: true,
		}, 0)
		if result != receiveAckHandoffAccepted || err != nil {
			t.Fatalf("selective handoff=%d err=%v", result, err)
		}
		time.Sleep(time.Until(deadline.Add(time.Nanosecond)))
		synctest.Wait()
		if sequence.ctx.Err() != nil || fixture.ackedCount != 0 {
			t.Fatal("selective feedback either expired early or falsely claimed cumulative delivery")
		}
		time.Sleep(time.Until(renewedDeadline.Add(-time.Nanosecond)))
		synctest.Wait()
		if sequence.ctx.Err() != nil {
			t.Fatal("selective lifetime expired before its one arrival-based renewal")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if sequence.ctx.Err() == nil {
			t.Fatal("a single selective receipt kept extending the lifetime")
		}
		barrier.close()
		synctest.Wait()
		if fixture.ackedCount != 0 || sequence.resendQueue.Len() != 0 || len(sequence.ackLifetimes.items) != 0 {
			t.Fatal("selective expiry claimed delivery or retained canceled ownership")
		}
	})
}

// Teardown joins every ACK publisher before releasing retained packet pools,
// whether publication runs in the receiver or in the compatibility worker.
func TestTransferAckHandoffShutdownJoinsPublisher(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		barrier := newAckHandoffLifetimeBarrier()
		defer barrier.close()
		var destination Id
		fixture, _, _ := newAckRetirementFixture(t, TransportTypeH1, 2, NewNoopLogger(), func(settings *ClientSettings) {
			settings.SendBufferSettings.afterAckCoalescedForTest = func(id sendSequenceId, _ uint64) {
				if id.Destination == destination {
					barrier.wait()
				}
			}
		})
		destination = fixture.receiver.ClientId()
		original := fixture.write(64)
		sequence := fixture.sequence()
		closeWaitEntered := make(chan struct{})
		fixture.sender.sendBuffer.beforeCloseWaitForTest = func(id sendSequenceId) {
			if id == sequence.id() {
				close(closeWaitEntered)
			}
		}
		handoffDone := make(chan struct{})
		go func() {
			defer close(handoffDone)
			sequence.ackMessageDetailed(receiveAckMessage{
				sequenceId: sequence.sequenceId, messageId: RequireIdFromBytes(original.pack.MessageId), selective: true,
			}, 0)
		}()
		barrier.requireReached(t)
		closed := make(chan error, 1)
		go func() { closed <- fixture.sender.sendBuffer.closeAndWait(context.Background()) }()
		// The real close boundary canceled the sequence and entered its join.
		// Do not call synctest.Wait while teardown is waiting on ackMutex:
		// mutex waits are deliberately not durable synctest blocking points.
		<-closeWaitEntered
		if sequence.ctx.Err() == nil {
			t.Fatal("close boundary did not cancel the active sequence")
		}
		select {
		case err := <-closed:
			t.Fatalf("teardown returned before ACK publication joined: %v", err)
		default:
		}
		barrier.close()
		<-handoffDone
		if err := <-closed; err != nil {
			t.Fatalf("sender teardown failed: %v", err)
		}
		if sequence.resendQueue.Len() != 0 || len(sequence.ackLifetimes.items) != 0 {
			t.Fatal("joined teardown retained packet ownership")
		}
	})
}

// Feedback published after the owner's snapshot must also protect the later
// resend scan, not only remove the same item's indexed lifetime.
func TestTransferAckAfterOwnerSnapshotKeepsResendLifetime(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		barrier := newAckHandoffLifetimeBarrier()
		defer barrier.close()
		var armed atomic.Bool
		var destination Id
		fixture, _, _ := newAckRetirementFixture(t, TransportTypeH1, 2, NewNoopLogger(), func(settings *ClientSettings) {
			settings.SendBufferSettings.afterApplyAckSnapshotForTest = func(id sendSequenceId) {
				if id.Destination == destination && armed.Load() {
					barrier.wait()
				}
			}
		})
		destination = fixture.receiver.ClientId()
		original := fixture.write(64)
		sequence := fixture.sequence()
		messageId := RequireIdFromBytes(original.pack.MessageId)
		if sequence.resendQueue.GetByMessageId(messageId) == nil || sequence.sendBufferSettings.AckTimeout != 30*time.Second {
			t.Fatal("fixture must retain the unchanged lifetime")
		}
		deadline := time.Now().Add(sequence.sendBufferSettings.AckTimeout)
		reply := fixture.receive(original)
		time.Sleep(time.Until(deadline.Add(-time.Second)))
		armed.Store(true)
		// Wake only the existing owner; no fabricated ACK state is published.
		select {
		case sequence.ackWindow.ackNotify <- struct{}{}:
		default:
		}
		barrier.requireReached(t)
		fixture.forward(reply, fixture.senderIn)
		if !sequence.ackWindow.pendingDeliveryFor(original.pack.SequenceNumber, messageId) {
			t.Fatal("real receiver ACK did not publish behind the owner's consumed snapshot")
		}
		time.Sleep(time.Until(deadline.Add(time.Nanosecond)))
		barrier.close()
		synctest.Wait()
		if sequence.ctx.Err() != nil || fixture.ackedCount != 1 || sequence.resendQueue.Len() != 0 || len(sequence.ackLifetimes.items) != 0 {
			t.Fatal("coalesced predeadline ACK survived indexed expiry but failed the resend scan")
		}
	})
}

// Restarting the owner after its consumed snapshot must consume one SACK,
// not spin on its marker or turn one selective receipt into unbounded life.
func TestTransferAckAfterOwnerSnapshotConsumesFiniteSack(t *testing.T) {
	runAckAfterSnapshotRenewal(t, false)
}

// The same boundary must consume an exact compact-contract repair request.
// Restoring the proof is recovery, never cumulative delivery or infinite life.
func TestTransferAckAfterOwnerSnapshotConsumesFiniteContract(t *testing.T) {
	runAckAfterSnapshotRenewal(t, true)
}

// The actual owner consumes an empty snapshot at 400 ms, feedback arrives
// behind it, and it resumes at 550 ms after the original 500 ms deadline.
func runAckAfterSnapshotRenewal(t *testing.T, contract bool) {
	t.Helper()
	type applicationEvidence struct {
		retained, selective, fullProof bool
		deadline                       time.Time
	}
	var barrier *ackHandoffLifetimeBarrier
	var armed atomic.Bool
	var visits atomic.Int64
	var contractId Id
	var messageId Id
	var applied chan applicationEvidence
	runWindowInitialLifetimeFixture(t, 0, 500*time.Millisecond, false, func(t *testing.T, fixture *windowInitialLifetimeFixture) {
		defer barrier.close()
		<-fixture.written
		wire := <-fixture.route
		pack := decodeSendPackLifecycleWirePack(t, wire)
		MessagePoolReturn(wire)
		if contract && (pack.ContractFrame != nil || string(pack.ContractId) != string(contractId.Bytes())) {
			t.Fatal("initial physical Pack did not carry the exact compact contract")
		}
		messageId = RequireIdFromBytes(pack.MessageId)
		close(fixture.release)
		synctest.Wait()
		time.Sleep(400 * time.Millisecond)
		armed.Store(true)
		select {
		case fixture.sequence.ackWindow.ackNotify <- struct{}{}:
		default:
		}
		barrier.requireReached(t)
		ack := receiveAckMessage{sequenceId: fixture.sequence.sequenceId, messageId: messageId, selective: !contract}
		if contract {
			ack.contractMissing, ack.missingContractId = true, contractId
		}
		if result, err := fixture.sequence.ackMessageDetailed(ack, 0); result != receiveAckHandoffAccepted || err != nil {
			t.Fatalf("post-snapshot feedback handoff=%d err=%v", result, err)
		}
		synctest.Wait()
		if !fixture.sequence.ackWindow.PendingDispositionFor(pack.SequenceNumber, messageId) {
			t.Fatal("exact feedback did not remain behind the consumed snapshot")
		}
		time.Sleep(150 * time.Millisecond)
		barrier.close()
		synctest.Wait()
		if visits.Load() > 8 || fixture.sequence.ctx.Err() != nil || len(fixture.failed) != 0 {
			t.Fatal("post-snapshot feedback spun or failed instead of renewing once")
		}
		pending := fixture.sequence.ackWindow.Snapshot(false)
		if pending.ackUpdateCount != 0 || len(pending.selectiveAcks) != 0 || len(pending.contractMissingAcks) != 0 {
			t.Fatal("resend reconciliation did not consume its pending marker")
		}
		var evidence applicationEvidence
		select {
		case evidence = <-applied:
		default:
			t.Fatal("owner did not publish its post-application evidence")
		}
		if !evidence.retained || contract && !evidence.fullProof || !contract && !evidence.selective {
			t.Fatal("ordinary owner application did not update the exact retained item")
		}
		deadline := evidence.deadline
		if deadline != fixture.start.Add(1050*time.Millisecond) {
			t.Fatal("feedback changed the existing owner-applied finite lifetime")
		}
		time.Sleep(time.Until(deadline.Add(-time.Nanosecond)))
		synctest.Wait()
		if fixture.sequence.ctx.Err() != nil || len(fixture.failed) != 0 {
			t.Fatal("single renewal expired before its actual deadline")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if visits.Load() > 8 || len(fixture.failed) != 1 || <-fixture.failed != deadline || fixture.sequence.resendQueue.Len() != 0 {
			t.Fatal("single post-snapshot renewal did not expire and release exactly once")
		}
	}, func(fixture *windowInitialLifetimeFixture) {
		barrier = newAckHandoffLifetimeBarrier()
		applied = make(chan applicationEvidence, 1)
		fixture.cleanup = barrier.close
		fixture.client.sendBuffer.afterApplyAckSnapshotForTest = func(id sendSequenceId) {
			if id == fixture.sequence.id() && armed.Load() {
				visit := visits.Add(1)
				if visit > 8 {
					// Bound an erroneous restart loop without scheduler timing.
					fixture.sequence.cancel()
					return
				}
				barrier.wait()
				if visit == 2 {
					// Copy mutable fields on their owner; tests never borrow a
					// pooled item across future resend/expiry mutations.
					evidence := applicationEvidence{}
					if item := fixture.sequence.resendQueue.GetByMessageId(messageId); item != nil {
						evidence.retained, evidence.selective, evidence.fullProof = true, item.selectiveAcked, item.hasContractFrame
						evidence.deadline = item.sendTime.Add(item.ackTimeout)
					}
					applied <- evidence
				}
			}
		}
		if contract {
			contractId = NewId()
			proof := &sequenceContract{
				log: fixture.client.log, localId: NewId(), tag: "s", contractId: contractId,
				transferByteCount: 1024 * 1024, effectiveTransferByteCount: 1024 * 1024,
				contract:                         &protocol.Contract{StoredContractBytes: []byte("synthetic snapshot repair proof")},
				path:                             TransferPath{SourceId: fixture.client.ClientId(), DestinationId: fixture.sequence.destination},
				compactContractRecoverySupported: true,
			}
			manager := fixture.client.ContractManager()
			manager.mutex.Lock()
			manager.sendNoContractClientIds[fixture.sequence.destination] = false
			manager.mutex.Unlock()
			fixture.sequence.sendContract = proof
			fixture.sequence.sendContractAcked = true
			fixture.sequence.sendContractMetadataGeneration = fixture.sequence.contractMetadata().generation
			fixture.sequence.openSendContracts[contractId] = proof
		}
	})
}
