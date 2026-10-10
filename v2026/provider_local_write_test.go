package connect

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// The real selector has an active route whose queue cannot admit a frame.
// No peer, ACK, timeout sleep or simulated provider verdict is involved.
func TestProviderEvaluationRejectedLocalRouteIsUnavailable(t *testing.T) {
	assertMessagePoolOwnership(t)
	sequence, item, _, _ := newRecoveryAccountingFixture(t)
	window := &multiClientWindow{}
	attempt := &providerEvaluationAttempt{observeLocalWrite: true, owner: &window.providerEvaluation, destinationId: sequence.destination}
	sequence.sendBufferSettings.providerEvaluation = attempt
	sequence.sendBufferSettings.WriteTimeout = 0
	routes := NewRouteManager(sequence.ctx, "local-admission-test")
	transport := NewSendClientTransport(DestinationId(sequence.destination))
	blocked := make(Route)
	routes.UpdateTransport(transport, []Route{blocked})
	sequence.contractMultiRouteWriter = routes.OpenMultiRouteWriter(DestinationId(sequence.destination))
	defer routes.CloseMultiRouteWriter(sequence.contractMultiRouteWriter)
	_, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, true, false)
	if !errors.Is(err, errTransferRouteWriteTimeout) {
		t.Fatalf("actual local route did not refuse admission: %v", err)
	}
	client := &RemoteUserNatMultiClient{windows: map[WindowType]*multiClientWindow{WindowTypeQuality: window}}
	if !client.ProviderLocalWriteUnavailable() {
		t.Fatal("rejected local route has no unavailable-writer proof")
	}
	if !window.providerEvaluation.providerContact.Load() {
		t.Fatal("local writer observation changed the existing contract-attempt fence")
	}
	accepted := make(Route, 1)
	routes.UpdateTransport(transport, []Route{accepted})
	if _, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, true, false); err != nil {
		t.Fatal(err)
	}
	MessagePoolReturn(<-accepted)
	if client.ProviderLocalWriteUnavailable() {
		t.Fatal("later application admission retained stale absence")
	}
	routes.UpdateTransport(transport, []Route{blocked})
	_, _ = sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, true, false)
	if client.ProviderLocalWriteUnavailable() {
		t.Fatal("later rejection erased earlier application admission")
	}
}

// The actual writer remains blocked at its route handoff when the measurement
// snapshots it. Completion publishes admission before releasing pending state.
func TestProviderEvaluationPendingLocalRouteIsUnavailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		sequence, item, _, _ := newRecoveryAccountingFixture(t)
		window := &multiClientWindow{}
		client := &RemoteUserNatMultiClient{windows: map[WindowType]*multiClientWindow{WindowTypeQuality: window}}
		sequence.sendBufferSettings.providerEvaluation = &providerEvaluationAttempt{observeLocalWrite: true, owner: &window.providerEvaluation, destinationId: sequence.destination}
		sequence.sendBufferSettings.WriteTimeout = time.Hour
		routes := NewRouteManager(sequence.ctx, "pending-admission-test")
		transport := NewSendClientTransport(DestinationId(sequence.destination))
		blocked := make(Route)
		routes.UpdateTransport(transport, []Route{blocked})
		sequence.contractMultiRouteWriter = routes.OpenMultiRouteWriter(DestinationId(sequence.destination))
		defer routes.CloseMultiRouteWriter(sequence.contractMultiRouteWriter)
		done := make(chan error, 1)
		go func() {
			_, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, true, false)
			done <- err
		}()
		synctest.Wait()
		if !client.ProviderLocalWriteUnavailable() || window.providerEvaluation.pendingLocalWrites.Load() != 1 {
			t.Fatal("pending actual writer is invisible")
		}
		sequence.cancel()
		if err := <-done; err == nil || !errors.Is(sequence.ctx.Err(), context.Canceled) {
			t.Fatal(err)
		}
		if window.providerEvaluation.pendingLocalWrites.Load() != 0 || !client.ProviderLocalWriteUnavailable() {
			t.Fatal("canceled writer lost its refused handoff or leaked pending state")
		}
	})
}

// A successfully admitted contract-only head is not application admission.
func TestProviderEvaluationContractHeadCannotHideRejectedApplication(t *testing.T) {
	assertMessagePoolOwnership(t)
	sequence, item, _, _ := newRecoveryAccountingFixture(t)
	window := &multiClientWindow{}
	client := &RemoteUserNatMultiClient{windows: map[WindowType]*multiClientWindow{WindowTypeQuality: window}}
	sequence.sendBufferSettings.providerEvaluation = &providerEvaluationAttempt{observeLocalWrite: true, owner: &window.providerEvaluation, destinationId: sequence.destination}
	sequence.sendBufferSettings.WriteTimeout = 0
	routes := NewRouteManager(sequence.ctx, "head-admission-test")
	transport := NewSendClientTransport(DestinationId(sequence.destination))
	accepted := make(Route, 1)
	routes.UpdateTransport(transport, []Route{accepted})
	sequence.contractMultiRouteWriter = routes.OpenMultiRouteWriter(DestinationId(sequence.destination))
	defer routes.CloseMultiRouteWriter(sequence.contractMultiRouteWriter)
	item.contractControl = true
	if _, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, true, false); err != nil {
		t.Fatal(err)
	}
	MessagePoolReturn(<-accepted)
	if client.ProviderLocalWriteUnavailable() {
		t.Fatal("successful head alone invented local failure")
	}
	item.contractControl = false
	routes.UpdateTransport(transport, []Route{make(Route)})
	if _, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, true, false); !errors.Is(err, errTransferRouteWriteTimeout) {
		t.Fatal(err)
	}
	if !client.ProviderLocalWriteUnavailable() {
		t.Fatal("contract-only head hid rejected application")
	}
}

// Other destinations, owners and unobserved writers cannot create absence
// evidence. One admitted application in any window conservatively wins.
func TestProviderEvaluationLocalWriterScopeAndReplacement(t *testing.T) {
	first, second := &multiClientWindow{}, &multiClientWindow{}
	client := &RemoteUserNatMultiClient{windows: map[WindowType]*multiClientWindow{WindowTypeQuality: first, WindowTypeSpeed: second}}
	other := &RemoteUserNatMultiClient{windows: map[WindowType]*multiClientWindow{WindowTypeQuality: {}}}
	destination := NewId()
	attempt := &providerEvaluationAttempt{observeLocalWrite: true, owner: &first.providerEvaluation, destinationId: destination}
	attempt.beginLocalWrite(ControlId)
	attempt.endLocalWrite(ControlId, false, true)
	if client.ProviderLocalWriteUnavailable() {
		t.Fatal("control traffic crossed provider scope")
	}
	attempt.beginLocalWrite(destination)
	attempt.endLocalWrite(destination, false, true)
	if !client.ProviderLocalWriteUnavailable() || other.ProviderLocalWriteUnavailable() {
		t.Fatal("rejection lost owner scope")
	}
	replacement := &providerEvaluationAttempt{observeLocalWrite: true, owner: &second.providerEvaluation, destinationId: destination}
	replacement.beginLocalWrite(destination)
	replacement.endLocalWrite(destination, true, true)
	attempt.beginLocalWrite(destination)
	if client.ProviderLocalWriteUnavailable() {
		t.Fatal("another window's admitted application was erased")
	}
	attempt.endLocalWrite(destination, false, true)
	if first.providerEvaluation.pendingLocalWrites.Load() != 0 {
		t.Fatal("pending writer leaked")
	}
}

// Ordinary clients retain the old contract-attempt fence without probe-only
// write accounting, including when their real route refuses a handoff.
func TestProviderEvaluationOrdinaryWriterDoesNotObserveAdmission(t *testing.T) {
	assertMessagePoolOwnership(t)
	sequence, item, _, _ := newRecoveryAccountingFixture(t)
	window := &multiClientWindow{}
	client := &RemoteUserNatMultiClient{windows: map[WindowType]*multiClientWindow{WindowTypeQuality: window}}
	sequence.sendBufferSettings.providerEvaluation = &providerEvaluationAttempt{owner: &window.providerEvaluation, destinationId: sequence.destination}
	sequence.sendBufferSettings.WriteTimeout = 0
	routes := NewRouteManager(sequence.ctx, "ordinary-admission-test")
	routes.UpdateTransport(NewSendClientTransport(DestinationId(sequence.destination)), []Route{make(Route)})
	sequence.contractMultiRouteWriter = routes.OpenMultiRouteWriter(DestinationId(sequence.destination))
	defer routes.CloseMultiRouteWriter(sequence.contractMultiRouteWriter)
	if _, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, true, false); !errors.Is(err, errTransferRouteWriteTimeout) {
		t.Fatal(err)
	}
	if client.ProviderLocalWriteUnavailable() || window.providerEvaluation.pendingLocalWrites.Load() != 0 || window.providerEvaluation.localWriteFailed.Load() {
		t.Fatal("ordinary writer acquired probe authority")
	}
}
