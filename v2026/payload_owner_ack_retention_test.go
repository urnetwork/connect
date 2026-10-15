package connect

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"testing/synctest"
	"time"
)

// Canonical main expires a disposable ACK item without canceling the retained
// successor. Its terminal buffer return must also retire its payload charge.
func TestTransferPayloadOwnerItemLocalAckExpiry(t *testing.T) {
	assertMessagePoolOwnership(t)
	oldCopy := DebugTransferCopyOnWrite
	DebugTransferCopyOnWrite = false
	defer func() { DebugTransferCopyOnWrite = oldCopy }()
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var ledger TransferPayloadOwnerLedger
				clientIndex := 0
				fixture, _, _ := newAckRetirementFixture(t, TransportTypeH1, version, NewNoopLogger(), func(settings *ClientSettings) {
					settings.SendBufferSettings.AckTimeout = 500 * time.Millisecond
					settings.SendBufferSettings.MinResendInterval = time.Second
					settings.SendBufferSettings.RttMinResendInterval = time.Second
					settings.SendBufferSettings.MaxResendInterval = time.Second
					if clientIndex == 0 {
						settings.PayloadOwnerLedger = &ledger
					}
					clientIndex++
				})
				fixture.forward(fixture.receive(fixture.write(32)), fixture.senderIn)
				before := requirePayloadLedger(t, &ledger, 0, 0, 0)
				control, failed := returnRetentionWrite(t, fixture, false)
				retained, completed := returnRetentionWrite(t, fixture, true)
				// These captured wire shares have the encoded owners' backing
				// capacities. Reading them does not inspect worker-owned state
				// before the timer resumes that worker.
				requirePayloadLedger(t, &ledger, 2, 0, int64(cap(control.bytes)+cap(retained.bytes)))

				time.Sleep(500 * time.Millisecond)
				synctest.Wait()
				requireReturnRetentionResult(t, failed, context.DeadlineExceeded)
				recovery := fixture.takePack(retained.pack.SequenceNumber)
				if !recovery.pack.Head || !bytes.Equal(recovery.pack.MessageId, retained.pack.MessageId) {
					t.Fatal("retained survivor did not preserve its native recovery identity")
				}
				sequence := fixture.sequence()
				if sequence.ctx.Err() != nil || len(completed) != 0 || sequence.resendQueue.Len() != 1 ||
					sequence.resendQueue.GetByMessageId(RequireIdFromBytes(control.pack.MessageId)) != nil {
					t.Fatal("item-local expiry changed retained ownership or retained the expired ACK identity")
				}
				owners, backingBytes := payloadLedgerSendCharges(t, sequence)
				if owners != 1 {
					t.Fatalf("native survivor owners=%d, want 1", owners)
				}
				afterExpiry := requirePayloadLedger(t, &ledger, owners, 0, backingBytes)
				if afterExpiry.SendAck.ReleasedTotal != before.SendAck.ReleasedTotal+1 {
					t.Fatalf("item-local expiry did not release exactly one charge: before=%+v after=%+v", before, afterExpiry)
				}
				fixture.forward(fixture.receive(recovery), fixture.senderIn)
				requireReturnRetentionResult(t, completed, nil)
				final := requirePayloadLedger(t, &ledger, 0, 0, 0)
				if final.SendAck.AdmittedTotal != before.SendAck.AdmittedTotal+2 ||
					final.SendAck.ReleasedTotal != before.SendAck.ReleasedTotal+2 || len(failed) != 0 || len(completed) != 0 {
					t.Fatalf("native expiry and survivor delivery did not conserve two owners exactly once: before=%+v final=%+v", before, final)
				}
			})
		})
	}
}
