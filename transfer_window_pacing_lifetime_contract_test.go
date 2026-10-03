// Pending contract recovery renews one exact compact record without proving
// delivery, losing newer feedback, or changing its original rtt timestamp.
package connect

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A real compact original is followed by a retained younger write paced until
// two seconds. A selective reply at 200 ms and contract request at 400 ms stay
// pending together, forcing lifetime reconciliation before either is consumed.
func runWindowPacingLifetimeContractFeedback(t *testing.T, matching, fullProof, selective bool, expiry time.Duration) {
	t.Helper()
	var contractId Id
	runWindowInitialLifetimeFixture(t, 0, 500*time.Millisecond, false, func(t *testing.T, fixture *windowInitialLifetimeFixture) {
		<-fixture.written
		wire := <-fixture.route
		pack := decodeSendPackLifecycleWirePack(t, wire)
		MessagePoolReturn(wire)
		if (pack.ContractFrame != nil) != fullProof || !fullProof && string(pack.ContractId) != string(contractId.Bytes()) {
			t.Fatal("fixture did not physically send the configured contract form")
		}
		older := fixture.sequence.resendQueue.PeekFirst()
		olderId := older.messageId
		time.Sleep(100 * time.Millisecond)
		service := fixture.sequence.windowPacer.service
		service.stateLock.Lock()
		service.next = fixture.start.Add(2 * time.Second)
		service.stateLock.Unlock()
		frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1280)}
		clear(frame.MessageBytes)
		if !fixture.client.SendWithTimeout(frame, fixture.sequence.destination, func(error) {}, time.Second, sendPackRecoveryOption{retainAfterAckTimeout: true}) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("retained younger original was not admitted")
		}
		close(fixture.release)
		synctest.Wait()
		time.Sleep(100 * time.Millisecond)
		if selective {
			if ok, err := fixture.sequence.ackMessage(receiveAckMessage{sequenceId: fixture.sequence.sequenceId, messageId: olderId, selective: true}, 0); !ok || err != nil {
				t.Fatalf("selective reply was refused: %v", err)
			}
			synctest.Wait()
		}
		time.Sleep(200 * time.Millisecond)
		missingContractId := contractId
		if !matching {
			missingContractId = NewId()
		}
		if ok, err := fixture.sequence.ackMessage(receiveAckMessage{
			sequenceId: fixture.sequence.sequenceId, messageId: olderId,
			contractMissing: true, missingContractId: missingContractId,
		}, 0); !ok || err != nil {
			t.Fatalf("contract recovery request was refused: %v", err)
		}
		synctest.Wait()
		if !fixture.sequence.ackWindow.PendingDispositionFor(older.sequenceNumber, olderId) {
			t.Fatal("contract feedback did not remain pending during the younger wait")
		}
		time.Sleep(150 * time.Millisecond)
		synctest.Wait()
		if fixture.sequence.ctx.Err() != nil || len(fixture.failed) != 0 || len(fixture.route) != 0 {
			t.Fatal("valid pending feedback failed before its first renewal")
		}
		fixture.sequence.resendQueue.stateLock.Lock()
		originalSendTime := older.sendTime
		fixture.sequence.resendQueue.stateLock.Unlock()
		if originalSendTime != fixture.start {
			t.Fatal("pending contract reconciliation rewrote the original RTT timestamp")
		}
		time.Sleep(expiry - 550*time.Millisecond - time.Nanosecond)
		synctest.Wait()
		select {
		case at := <-fixture.failed:
			t.Fatalf("pending feedback retired the record at %s before its %s deadline", at.Sub(fixture.start), expiry)
		default:
		}
		if len(fixture.route) != 0 {
			t.Fatal("pending contract feedback claimed physical pacing permission")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		select {
		case at := <-fixture.failed:
			if at != fixture.start.Add(expiry) {
				t.Fatalf("contract lifetime ended at %s, want %s", at.Sub(fixture.start), expiry)
			}
		default:
			t.Fatal("one pending contract request renewed its lifetime more than once")
		}
		if len(fixture.route) != 0 || fixture.sequence.resendQueue.Len() != 0 {
			t.Fatal("contract lifetime expiry dispatched or retained pending bytes")
		}
	}, func(fixture *windowInitialLifetimeFixture) {
		contractId = NewId()
		contract := &sequenceContract{
			log: fixture.client.log, localId: NewId(), tag: "s", contractId: contractId,
			transferByteCount: 1024 * 1024, effectiveTransferByteCount: 1024 * 1024,
			contract:                         &protocol.Contract{StoredContractBytes: []byte("synthetic contract recovery proof")},
			path:                             TransferPath{SourceId: fixture.client.ClientId(), DestinationId: fixture.sequence.destination},
			compactContractRecoverySupported: !fullProof,
		}
		manager := fixture.client.ContractManager()
		manager.mutex.Lock()
		manager.sendNoContractClientIds[fixture.sequence.destination] = false
		manager.mutex.Unlock()
		fixture.sequence.sendContract = contract
		fixture.sequence.sendContractAcked = true
		fixture.sequence.sendContractMetadataGeneration = fixture.sequence.contractMetadata().generation
		fixture.sequence.openSendContracts[contractId] = contract
	})
}

// A valid newer contract request must not be shadowed by an older selective reply.
func TestWindowPacingLifetimeNewerContractRequestPreservesRenewal(t *testing.T) {
	runWindowPacingLifetimeContractFeedback(t, true, false, true, 900*time.Millisecond)
}

// Contract identity is exact; an unrelated proof request adds no lifetime.
func TestWindowPacingLifetimeWrongContractCannotExtendRenewal(t *testing.T) {
	runWindowPacingLifetimeContractFeedback(t, false, false, true, 700*time.Millisecond)
}

// A record already carrying its full proof has no compact state to restore.
func TestWindowPacingLifetimeFullProofCannotExtendRenewal(t *testing.T) {
	runWindowPacingLifetimeContractFeedback(t, true, true, true, 700*time.Millisecond)
}

// A request alone grants one recovery lifetime, without becoming delivery.
func TestWindowPacingLifetimeContractRequestRenewsOnlyOnce(t *testing.T) {
	runWindowPacingLifetimeContractFeedback(t, true, false, false, 900*time.Millisecond)
}
