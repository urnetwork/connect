// The synthetic zero obligation may wait for counted delivery, but must never
// acquire frame ownership beyond the original report's shutdown handoff.
package connect

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// Separates an external result callback from the SendControl return that
// transfers frame custody, covering either callback order without timers.
type contractCloseReportsTestOob struct {
	stateLock            sync.Mutex
	reportFrames         []*protocol.Frame
	callback             OobResultFunction
	callbackBeforeReturn bool
	sendErr              error
	entered              chan struct{}
	release              <-chan struct{}
}

// Records immutable evidence and returns the original frame owner at handoff.
func (self *contractCloseReportsTestOob) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	self.stateLock.Lock()
	for _, frame := range frames {
		self.reportFrames = append(self.reportFrames, proto.Clone(frame).(*protocol.Frame))
	}
	self.callback = callback
	self.stateLock.Unlock()
	if self.callbackBeforeReturn {
		callback(nil, self.sendErr)
	}
	close(self.entered)
	if self.release != nil {
		<-self.release
	}
	for _, frame := range frames {
		MessagePoolReturn(frame.MessageBytes)
	}
}

// Reads the sole counted cleanup independently of its pooled frame lifetime.
func (self *contractCloseReportsTestOob) requireCheckpoint(t *testing.T, contractId Id) {
	t.Helper()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.reportFrames) != 1 {
		t.Fatalf("cleanup frames = %d, want only the counted checkpoint", len(self.reportFrames))
	}
	message, err := FromFrame(self.reportFrames[0])
	if err != nil {
		t.Fatal(err)
	}
	report, ok := message.(*protocol.CloseContract)
	if !ok || RequireIdFromBytes(report.ContractId) != contractId || !report.Checkpoint || report.AckedByteCount != 121 || report.UnackedByteCount != 7 {
		t.Fatalf("cleanup report = %v, want the original counted checkpoint", message)
	}
}

// A synchronous success callback cannot retire the locally owned original
// frame or let CloseAndWait complete before the external call returns custody.
func TestContractCloseReportsCallbackBeforeHandoffKeepsOwnership(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var releaseOnce sync.Once
		resume := func() { releaseOnce.Do(func() { close(release) }) }
		oob := &contractCloseReportsTestOob{entered: make(chan struct{}), release: release, callbackBeforeReturn: true}
		client, contractId := newCloseDeliveryTestClient(t, t.Context(), oob)
		t.Cleanup(resume)
		manager := client.ContractManager()
		var witness []byte
		manager.beforeCloseControlSendForTest = func(frame *protocol.Frame) {
			witness = MessagePoolShareReadOnly(frame.MessageBytes)
		}
		manager.CheckpointContract(contractId, 121, 7)
		manager.requestContractClose(contractId)
		synctest.Wait()
		client.Close()
		<-oob.entered
		manager.mutex.Lock()
		pending := manager.pendingCloseReportKVs[contractId]
		retained := pending != nil && pending.pendingCount == 1 && pending.requested && !pending.final
		manager.mutex.Unlock()
		if !retained {
			t.Fatal("success callback retired counted ownership before SendControl returned")
		}
		joined := make(chan error, 1)
		go func() { joined <- client.CloseAndWait(context.Background()) }()
		synctest.Wait()
		select {
		case err := <-joined:
			t.Fatalf("join returned before original frame custody transferred: %v", err)
		default:
		}
		resume()
		synctest.Wait()
		if err := <-joined; err != nil {
			t.Fatal(err)
		}
		oob.requireCheckpoint(t, contractId)
		manager.mutex.Lock()
		pendingCount := len(manager.pendingCloseReportKVs)
		manager.mutex.Unlock()
		if pendingCount != 0 {
			t.Fatalf("joined manager retained %d pending report entries", pendingCount)
		}
		if witness == nil || !MessagePoolReturn(witness) {
			t.Fatal("joined handoff retained a locally owned frame share")
		}
	})
}

// A late external result owns only the handed-off original. Shutdown discards
// the coalesced zero request and clears the manager's ownership index.
func runContractCloseReportsLateCleanup(t *testing.T, sendErr error) {
	t.Helper()
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		oob := &contractCloseReportsTestOob{entered: make(chan struct{}), sendErr: sendErr}
		client, contractId := newCloseDeliveryTestClient(t, t.Context(), oob)
		manager := client.ContractManager()
		manager.CheckpointContract(contractId, 121, 7)
		for range 3 {
			manager.requestContractClose(contractId)
		}
		synctest.Wait()
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		oob.requireCheckpoint(t, contractId)
		manager.mutex.Lock()
		pendingCount := len(manager.pendingCloseReportKVs)
		manager.mutex.Unlock()
		if pendingCount != 0 {
			t.Fatalf("joined manager retained %d external cleanup entries", pendingCount)
		}
		oob.stateLock.Lock()
		callback := oob.callback
		oob.stateLock.Unlock()
		if callback == nil {
			t.Fatal("original counted cleanup was not handed off")
		}
		callback(nil, sendErr)
		synctest.Wait()
		oob.requireCheckpoint(t, contractId)
	})
}

// Failed cleanup must not settle a zero final after dropping counted work.
func TestContractCloseReportsFailedCleanupDropsSyntheticFinal(t *testing.T) {
	runContractCloseReportsLateCleanup(t, errors.New("cleanup unavailable"))
}

// External callback success after a completed join cannot start a new sender.
func TestContractCloseReportsLateCleanupDropsSyntheticFinalAfterJoin(t *testing.T) {
	runContractCloseReportsLateCleanup(t, nil)
}

// Repeated platform requests retain one bit behind all existing reports, with
// no extra serialized finals, and release only after counted delivery ACKs.
func TestContractCloseReportsCoalescesZeroBehindCountedReports(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		h := newCloseSequencesHarness(t)
		releaseAcknowledgements := h.holdControlAcknowledgements()
		contractId := NewId()
		manager := h.client.ContractManager()
		manager.CheckpointContract(contractId, 11, 3)
		manager.CheckpointContract(contractId, 17, 5)
		for range 64 {
			manager.requestContractClose(contractId)
		}
		synctest.Wait()
		manager.mutex.Lock()
		pending := manager.pendingCloseReportKVs[contractId]
		coalesced := len(manager.pendingCloseReportKVs) == 1 && pending != nil && pending.pendingCount == 2 && pending.requested && !pending.final
		manager.mutex.Unlock()
		if !coalesced {
			t.Fatal("repeated requests did not coalesce behind the existing counted report owners")
		}
		first := closeSequencesReport{acked: 11, unacked: 3, checkpoint: true}
		second := closeSequencesReport{acked: 17, unacked: 5, checkpoint: true}
		h.requireReports(contractId, first, second)
		releaseAcknowledgements()
		synctest.Wait()
		h.requireReports(contractId, first, second, closeSequencesReport{})
		manager.mutex.Lock()
		pendingCount := len(manager.pendingCloseReportKVs)
		manager.mutex.Unlock()
		if pendingCount != 0 {
			t.Fatalf("acknowledged final retained %d pending ownership entries", pendingCount)
		}
	})
}
