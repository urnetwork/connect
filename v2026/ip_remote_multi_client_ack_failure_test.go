// Window-owned diagnostic totals survive peer churn without rewriting live totals.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// Removal, same-slot replacement and a late callback count once at the original owner.
func TestMultiClientAckRouteFailuresSurviveRetirement(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		window := &multiClientWindow{clients: map[Id]*multiClientChannel{}}
		multi := &RemoteUserNatMultiClient{windows: map[WindowType]*multiClientWindow{WindowTypeQuality: window}}
		owner := window.ackRouteWriteFailures.load(true)
		newClient := func() *Client {
			return newAckGapTestSequenceWithClientSettings(t, nil, func(settings *ClientSettings) {
				settings.EnableAckRouteWriteDiagnostics = true
				settings.ackRouteWriteFailureOwner = owner
			}).receiveSequence.client
		}
		oldClient, replacement := newClient(), newClient()
		slot := NewId()
		window.clients[slot] = &multiClientChannel{client: oldClient}
		oldClient.recordReceiveAckRouteWrite(TransportTypeH1, 0, false, false, context.Canceled)
		if got := multi.MemorySnapshot().AckRouteWriteErrorCount; got != 1 {
			t.Fatalf("initial live errors=%d, want 1", got)
		}
		release, joined := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(joined)
			<-release
			oldClient.recordReceiveAckRouteWriteDisposition(TransportTypeH1, 0, false, false, context.Canceled, transferWriteDisposition{
				failureKind: transferWriteFailureSelectorClosed,
			})
		}()
		window.clients[slot] = &multiClientChannel{client: replacement}
		oldClient.Cancel()
		close(release)
		<-joined
		replacement.recordReceiveAckRouteWrite(TransportTypeH1, 0, false, false, errTransferRouteWriteTimeout)
		quality, speed := multi.AckRouteWriteLifetimeFailures()
		if quality == nil || quality.ErrorCount != 3 || quality.CallerCanceledCount != 1 || quality.SelectorClosedCount != 1 || quality.RouteTimeoutCount != 1 || speed != nil {
			t.Fatalf("replacement lost or double-counted owner evidence: quality=%+v speed=%+v", quality, speed)
		}
		if got := multi.MemorySnapshot().AckRouteWriteErrorCount; got != 1 {
			t.Fatalf("retired errors leaked into legacy live total: %d", got)
		}
		delete(window.clients, slot)
		quality, _ = multi.AckRouteWriteLifetimeFailures()
		if quality == nil || quality.ErrorCount != 3 || multi.MemorySnapshot().AckRouteWriteErrorCount != 0 {
			t.Fatal("empty window lost lifetime evidence or retained live totals")
		}
		otherWindow := &multiClientWindow{clients: map[Id]*multiClientChannel{}}
		other := &RemoteUserNatMultiClient{windows: map[WindowType]*multiClientWindow{WindowTypeSpeed: otherWindow}}
		otherOwner := otherWindow.ackRouteWriteFailures.load(true)
		otherOwner.record(transferWriteFailureOther, transferWriteDisposition{})
		otherQuality, otherSpeed := other.AckRouteWriteLifetimeFailures()
		quality, _ = multi.AckRouteWriteLifetimeFailures()
		if otherOwner == owner || otherQuality != nil || otherSpeed == nil || otherSpeed.ErrorCount != 1 || otherSpeed.OtherCount != 1 || quality.ErrorCount != 3 {
			t.Fatal("independent owner/window counters were shared")
		}
	})
}

// Disabled construction allocates no owner counter set and never records into one.
func TestMultiClientAckRouteFailureDiagnosticsOptIn(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		for _, enabled := range []bool{false, true} {
			window := &multiClientWindow{clients: map[Id]*multiClientChannel{}}
			owner := window.ackRouteWriteFailures.load(enabled)
			fixture := newAckGapTestSequenceWithClientSettings(t, nil, func(settings *ClientSettings) {
				settings.EnableAckRouteWriteDiagnostics = enabled
				settings.ackRouteWriteFailureOwner = owner
			})
			client := fixture.receiveSequence.client
			if enabled != (client.receiveAckRouteWriteFailures != nil) || enabled != (client.ackRouteWriteFailureOwner != nil) || enabled != (owner != nil) {
				t.Fatalf("enabled=%t: constructor did not preserve diagnostic opt-in", enabled)
			}
			if allocations := testing.AllocsPerRun(100, func() {
				if window.ackRouteWriteFailures.load(false).snapshot() != nil {
					t.Fatal("unused diagnostic owner fabricated failures")
				}
			}); allocations != 0 {
				t.Errorf("enabled=%t: unused owner snapshot allocations=%g", enabled, allocations)
			}
			client.recordReceiveAckRouteWrite(TransportTypeH1, 0, false, false, context.Canceled)
			if !enabled && (window.ackRouteWriteFailures.load(false) != nil || client.ReceiveStats().AckRouteWriteFailures != nil) {
				t.Fatal("disabled diagnostic error allocated an owner or per-client snapshot")
			}
		}
	})
}

// The real expansion and channel/client constructors pass the owner before workers start.
func TestMultiClientAckRouteFailureExpansionOwnership(t *testing.T) {
	fixture := newMultiClientExpandLifecycleFixture(t)
	generator := fixture.window.generator.(*multiClientExpandLifecycleGenerator).TestMultiClientGenerator
	originalSettings := generator.newClientSettings
	generator.newClientSettings = func() *ClientSettings {
		settings := originalSettings()
		settings.EnableAckRouteWriteDiagnostics = true
		return settings
	}
	created := make(chan *Client, 1)
	originalNewClient := generator.newClient
	generator.newClient = func(ctx context.Context, args *MultiClientGeneratorClientArgs, settings *ClientSettings) (*Client, error) {
		client, err := originalNewClient(ctx, args, settings)
		if err == nil {
			created <- client
		}
		return client, err
	}
	done := fixture.start()
	defer func() {
		fixture.cancelEvaluation()
		fixture.releasePing()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("expansion owner did not join")
		}
	}()
	fixture.wait(t, "owned initial ping", fixture.pingResultEntered)
	var client *Client
	select {
	case client = <-created:
	case <-fixture.waitCtx.Done():
		t.Fatal("expansion did not construct the test client")
	}
	owner := fixture.window.ackRouteWriteFailures.load(false)
	if owner == nil || client.ackRouteWriteFailureOwner != owner || client.receiveAckRouteWriteFailures == nil {
		t.Fatal("expansion/channel/client construction omitted the original window owner")
	}
	client.recordReceiveAckRouteWrite(TransportTypeH1, 0, false, false, context.Canceled)
	if got := owner.snapshot(); got == nil || got.CallerCanceledCount != 1 || got.ErrorCount != 1 {
		t.Fatalf("constructed client did not record into the original owner: %+v", got)
	}
}
