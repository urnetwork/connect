package connect

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A retained admission and an ordinary timeout must have a single winner.
// In particular, an admitted input may still be only a queued Pack when the
// timeout chooses its disposition; scanning the resend queue cannot protect it.
func TestReturnRetentionAdmissionArbitratesSequenceTimeout(t *testing.T) {
	for range 1000 {
		sequence := &SendSequence{}
		start := make(chan struct{})
		var admitted bool
		var disposition error
		var done sync.WaitGroup
		done.Add(2)
		go func() {
			defer done.Done()
			<-start
			admitted = sequence.protectRetainedAdmission()
		}()
		go func() {
			defer done.Done()
			<-start
			disposition = sequence.ackLifetimeDisposition(nil, time.Time{})
		}()
		close(start)
		done.Wait()
		if admitted != (disposition == errSendAckLifetime) {
			t.Fatalf("retained admission=%t raced with terminal disposition=%v", admitted, disposition)
		}
		if !admitted && !errors.Is(disposition, context.DeadlineExceeded) {
			t.Fatalf("ordinary timeout did not retain its close disposition: %v", disposition)
		}
	}
}

// The expired opener's callback can no longer acknowledge its contract. The
// surviving data's complete proof must do so, without crediting abandoned
// bytes or keeping an obsolete contract open forever.
func TestReturnRetentionExpiredOpeningContractRecoversAccounting(t *testing.T) {
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	harness := newNoAckBudgetHarness(t, ctx, func(contract *sequenceContract) {
		contract.minUpdateByteCount = 64
		contract.contract = &protocol.Contract{StoredContractBytes: []byte("opening proof")}
	})
	sequence, contract := harness.sequence, harness.contract
	sequence.sendContractAcked = false
	if !contract.update(0) {
		t.Fatal("opening control debit refused")
	}
	sequence.sendWithSetContract(nil, sequence.contractOpenAckCallback(contract), true, true, false)
	MessagePoolReturn(<-harness.route)
	opener := sequence.resendQueue.GetBySequenceNumber(0)
	frame := budgetTestFrame(128)
	if !contract.update(MessageByteCount([]*protocol.Frame{frame})) {
		t.Fatal("retained data debit refused")
	}
	completed := make(chan error, 2)
	sequence.sendRecord([]*protocol.Frame{frame}, sendAckRecord{retainAfterAckTimeout: true, callback: func(err error) {
		completed <- err
	}}, noAckSendRecord{}, true, false)
	MessagePoolReturn(<-harness.route)
	data := sequence.resendQueue.GetBySequenceNumber(1)
	dataByteCount := data.messageByteCount
	sequence.expireSendItem(opener, time.Now())
	if sequence.sendContractAcked || contract.ackedByteCount != 0 || contract.abandonedByteCount != 64 ||
		contract.unackedByteCount != 64+dataByteCount || len(completed) != 0 {
		t.Fatal("opening expiry manufactured contract delivery or discarded the retained data")
	}
	wire, fullProof, err := sequence.setHead(data, false)
	if err != nil || !fullProof {
		t.Fatalf("retained head did not restore the opening proof: proof=%t error=%v", fullProof, err)
	}
	MessagePoolReturn(data.transferFrameBytes)
	data.transferFrameBytes, data.head, data.hasContractFrame = wire, true, fullProof
	sequence.receiveAck(data.messageId, false, sequenceTag{}, true)
	requireReturnRetentionResult(t, completed, nil)
	if !sequence.sendContractAcked || contract.ackedByteCount != dataByteCount || contract.unackedByteCount != 64 {
		t.Fatal("surviving proof did not confirm only the actually delivered contract bytes")
	}
	// Future NoAck traffic must not be permanently promoted onto the Ack lane.
	frame = budgetTestFrame(16)
	if !contract.update(MessageByteCount([]*protocol.Frame{frame})) {
		t.Fatal("following NoAck debit refused")
	}
	sequence.sendRecord([]*protocol.Frame{frame}, sendAckRecord{}, noAckSendRecord{}, false, false)
	wire = <-harness.route
	pack := decodeSendPackLifecycleWirePack(t, wire)
	MessagePoolReturn(wire)
	if !pack.Nack || sequence.resendQueue.Len() != 0 {
		t.Fatal("recovered contract kept new NoAck data on the reliable lane")
	}
	sequence.sendContract = nil
	sequence.retireSendContract(contract)
	if len(sequence.openSendContracts) != 0 || contract.unackedByteCount != 64 ||
		contract.ackedByteCount != dataByteCount+64 ||
		harness.client.ContractManager().LocalStats().ReceiveContractCloseByteCount != dataByteCount+64 {
		t.Fatal("abandoned contract debt was acknowledged or prevented obsolete-contract retirement")
	}
}
