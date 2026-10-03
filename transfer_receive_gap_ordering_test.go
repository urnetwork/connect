// Receive gap expiry belongs to an unresolved sequence hole, not time spent
// waiting for local work after the missing predecessor has already arrived.
package connect

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Capture error classification without changing the worker or its scheduling.
type receiveGapTestLogger struct {
	Logger
	stateLock sync.Mutex
	entries   []string
}

// Each entry is copied while owned so assertions remain race-safe even if a
// separate client shutdown log arrives after the receive worker has exited.
func (self *receiveGapTestLogger) Errorf(format string, values ...any) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.entries = append(self.entries, fmt.Sprintf(format, values...))
}

// The diagnostic snapshot does not expose a slice that a live writer owns.
func (self *receiveGapTestLogger) messages() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string(nil), self.entries...)
}

// Use the real worker, dispatch and pool paths with generated peer identities;
// no socket, contract service or application retry policy is involved.
func receiveGapSequenceTest(t *testing.T) (*ReceiveSequence, *receiveGapTestLogger) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	log := &receiveGapTestLogger{Logger: NewNoopLogger()}
	settings := DefaultClientSettings()
	settings.Log = log
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	sourceId := NewId()
	client.ContractManager().AddNoContractPeer(sourceId)
	receiveSettings := DefaultReceiveBufferSettings()
	receiveSettings.IdleTimeout = time.Hour
	receiveSettings.GapTimeout = time.Minute
	sequence := newReceiveSequence(ctx, client, SourceId(sourceId), NewId(), TransferKey{}, receiveSettings)
	t.Cleanup(func() {
		sequence.Cancel()
		cancel()
		owner, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if err := client.CloseAndWait(owner); err != nil {
			t.Error(err)
		}
	})
	return sequence, log
}

// Queue admission transfers one pool buffer to the exact worker under test.
func addReceiveGapTestItem(t *testing.T, sequence *ReceiveSequence, number uint64, received time.Time, frames bool) {
	t.Helper()
	item := &receiveItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: number, messageByteCount: 16},
		receiveTime: received, committed: true, transferFrameBytes: MessagePoolGet(16), receiveCallback: sequence.client.receiveCallback}
	if frames {
		item.frames = []*protocol.Frame{{MessageType: protocol.MessageType_TransferExchangeSignals}}
	}
	sequence.receiveQueue.Add(item)
}

// Exit is the ownership barrier. The timeout is a deadlock guard, never the
// mechanism that creates the aged item or decides whether delivery succeeded.
func waitReceiveGapTestExit(t *testing.T, sequence *ReceiveSequence) {
	t.Helper()
	select {
	case <-sequence.exit:
	case <-time.After(5 * time.Second):
		sequence.Cancel()
		t.Fatal("receive gap worker did not publish its final ownership barrier")
	}
}

// Simulate a completed predecessor followed by a long local stall. Ready
// frames still deliver, and an obsolete duplicate cannot expire the new head.
func TestReceiveSequenceGapClosedBeforeExpiredQueueDrain(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, duplicate := range []bool{false, true} {
		sequence, log := receiveGapSequenceTest(t)
		var callbacks atomic.Int32
		sequence.client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			callbacks.Add(int32(len(frames)))
			sequence.Cancel()
		})
		old := time.Now().Add(-time.Hour)
		if duplicate {
			sequence.nextSequenceNumber = 1
			addReceiveGapTestItem(t, sequence, 0, old, false)
		}
		expected := sequence.nextSequenceNumber
		addReceiveGapTestItem(t, sequence, expected, old, true)
		go sequence.Run()
		waitReceiveGapTestExit(t, sequence)
		if callbacks.Load() != 1 || sequence.nextSequenceNumber != expected+1 || sequence.receiveQueue.Len() != 0 {
			t.Fatalf("duplicate=%t closed gap lost delivery: callbacks=%d next=%d errors=%v", duplicate, callbacks.Load(), sequence.nextSequenceNumber, log.messages())
		}
		if errors := log.messages(); len(errors) != 0 {
			t.Fatalf("duplicate=%t ready data was classified as failure: %v", duplicate, errors)
		}
	}
}

// An already-canceled owner does not expire its old gap or dispatch a queued
// ready frame. Cleanup still returns every admitted buffer exactly once.
func TestReceiveSequenceGapOwnerCancellationPrecedesQueueInspection(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, number := range []uint64{0, 1} {
		sequence, log := receiveGapSequenceTest(t)
		var callbacks atomic.Int32
		sequence.client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) { callbacks.Add(int32(len(frames))) })
		addReceiveGapTestItem(t, sequence, number, time.Now().Add(-time.Hour), true)
		sequence.Cancel()
		go sequence.Run()
		waitReceiveGapTestExit(t, sequence)
		if callbacks.Load() != 0 || sequence.nextSequenceNumber != 0 || sequence.receiveQueue.Len() != 0 || len(log.messages()) != 0 {
			t.Fatalf("canceled owner number=%d dispatched or invented gap: callbacks=%d next=%d errors=%v", number, callbacks.Load(), sequence.nextSequenceNumber, log.messages())
		}
	}
}

// A genuine missing predecessor still terminates at its existing deadline.
// Diagnostics expose the distinguishing sequence facts for future incidents.
func TestReceiveSequenceGapRetainsUnresolvedDeadlineAndEvidence(t *testing.T) {
	assertMessagePoolOwnership(t)
	sequence, log := receiveGapSequenceTest(t)
	addReceiveGapTestItem(t, sequence, 1, time.Now().Add(-time.Hour), true)
	go sequence.Run()
	waitReceiveGapTestExit(t, sequence)
	entries := log.messages()
	if sequence.nextSequenceNumber != 0 || sequence.receiveQueue.Len() != 0 || len(entries) != 1 || !strings.Contains(entries[0], "exit gap timeout expected=0 queued=1") || !strings.Contains(entries[0], "budget=1m0s") {
		t.Fatalf("unresolved gap lost its deadline or diagnostic evidence: next=%d logs=%v", sequence.nextSequenceNumber, entries)
	}
}
