// The generation contract is optional and checked at acquired-snapshot ownership.
package connect

import (
	"context"
	"maps"
	"reflect"
	"testing"
	"testing/synctest"
	"time"
)

// The preimage must compile and perform its actual legacy dispatch, not fail
// because the new method or sentinel is absent from its production sources.
type windowRouteGenerationWriter interface {
	writeDetailedWithPolicyGeneration(context.Context, []byte, time.Duration, bool, uint64, func()) (bool, transferWriteDisposition, error)
}

// Legacy fallback is a test-only causal preimage, not a protected writer.
// Takes wire on success; failure leaves the same share with its caller.
func windowRouteGenerationWrite(selector *MultiRouteSelector, ctx context.Context, wire []byte, timeout time.Duration, generation uint64, commit func()) (bool, transferWriteDisposition, error) {
	if writer, ok := any(selector).(windowRouteGenerationWriter); ok {
		return writer.writeDetailedWithPolicyGeneration(ctx, wire, timeout, false, generation, commit)
	}
	commit()
	return selector.writeDetailedWithCarrier(ctx, wire, timeout)
}

// Even a ready route and a zero timeout cannot consume a share acquired under
// another policy. Matching generations retain the one immediate attempt.
func TestWindowPacingRouteGenerationSelectorZeroBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	selector := NewMultiRouteSelector(ctx, "route-zero", nil, DestinationId(NewId()), true)
	defer selector.Close()
	h1 := NewSendGatewayTransportWithType(TransportTypeH1)
	route := make(Route, 1)
	selector.updateTransport(h1, []Route{route})
	old := selector.transferFlightPolicy()
	selector.updateTransport(h1, []Route{make(Route)})
	selector.updateTransport(h1, []Route{route})
	current := selector.transferFlightPolicy()
	if old.generation == current.generation || !current.h1Only {
		t.Fatal("fixture did not publish a distinct H1 generation")
	}
	wire := make([]byte, 128)
	observations := 0
	commit := func() { observations++ }
	success, _, err := windowRouteGenerationWrite(selector, ctx, wire, 0, old.generation, commit)
	if success || err == nil || observations != 0 || len(route) != 0 {
		t.Fatal("stale zero-budget generation consumed or observed its unadmitted share")
	}
	success, disposition, err := windowRouteGenerationWrite(selector, ctx, wire, 0, current.generation, commit)
	if !success || err != nil || disposition.transportType != TransportTypeH1 || observations != 1 || len(route) != 1 {
		t.Fatal("matching zero-budget generation lost its ready attempt")
	}
	if accepted := <-route; &accepted[0] != &wire[0] {
		t.Fatal("ready attempt replaced its share")
	}
}

// Both optimized small selects and the reflect fallback must return a changed
// generation without consuming the share or committing a second observation.
func TestWindowPacingRouteGenerationSelectorBlockedReplacement(t *testing.T) {
	for _, routeCount := range []int{1, 3} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			selector := NewMultiRouteSelector(ctx, "route-blocked", nil, DestinationId(NewId()), true)
			defer selector.Close()
			h3 := NewSendGatewayTransportWithType(TransportTypeH3)
			routes := make([]Route, routeCount)
			for i := range routes {
				routes[i] = make(Route)
			}
			selector.updateTransport(h3, routes)
			generation := selector.transferFlightPolicy().generation
			wire := make([]byte, 128)
			observed := false
			observations := 0
			type result struct {
				success bool
				err     error
			}
			done := make(chan result, 1)
			go func() {
				success, _, err := windowRouteGenerationWrite(selector, ctx, wire, time.Second, generation, func() {
					if !observed {
						observed = true
						observations++
					}
				})
				done <- result{success, err}
			}()
			synctest.Wait()
			if observations != 1 || len(done) != 0 {
				t.Fatal("fixture did not reach its committed blocked writer")
			}
			ready := make(Route, 1)
			selector.updateTransport(h3, []Route{ready})
			synctest.Wait()
			if len(done) != 1 {
				t.Fatal("writer did not return the unconsumed generation change")
			}
			got := <-done
			if got.success || got.err == nil || observations != 1 || len(ready) != 0 {
				t.Fatalf("routes=%d replacement consumed the old policy share", routeCount)
			}
		})
	}
}

// A previously admitted old snapshot may complete under existing retirement
// semantics. Generation binding is not a lock held against publication.
func TestWindowPacingRouteGenerationAcquiredSnapshotKeepsLifetime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		manager := NewRouteManager(ctx, "route-acquired")
		opened := manager.OpenMultiRouteWriter(DestinationId(NewId()))
		defer manager.CloseMultiRouteWriter(opened)
		selector, ok := opened.(*MultiRouteSelector)
		if !ok {
			t.Fatal("route manager did not provide the concrete selector")
		}
		h3 := NewSendGatewayTransportWithType(TransportTypeH3)
		oldRoute, newRoute := make(Route, 1), make(Route, 1)
		manager.UpdateTransport(h3, []Route{oldRoute})
		policy := selector.transferFlightPolicy()
		writer, ok := any(selector).(windowRouteGenerationWriter)
		if !ok {
			t.Fatal("selector cannot bind dispatch to its acquired generation")
		}
		release, committed, updated, done := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan bool, 1)
		defer func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}()
		wire := make([]byte, 128)
		go func() {
			success, _, err := writer.writeDetailedWithPolicyGeneration(ctx, wire, time.Second, false, policy.generation, func() {
				close(committed)
				<-release
			})
			done <- success && err == nil
		}()
		<-committed
		go func() { manager.UpdateTransport(h3, []Route{newRoute}); close(updated) }()
		<-policy.notify
		synctest.Wait()
		select {
		case <-updated:
			t.Fatal("route retirement returned before the acquired writer released")
		default:
		}
		if selector.transferFlightPolicy().generation == policy.generation {
			t.Fatal("publication did not retain the old acquired writer")
		}
		close(release)
		if !<-done {
			t.Fatal("retained old-generation dispatch was rejected")
		}
		<-updated
		if len(oldRoute) != 1 || len(newRoute) != 0 {
			t.Fatal("acquired snapshot silently switched routes")
		}
	})
}

// Applying a fresh route only to a scalar projection must leave the real
// generation transition for outer retired-carrier recovery.
func TestWindowPacingRouteGenerationProjectionKeepsOuterTransition(t *testing.T) {
	settings := DefaultSendBufferSettings()
	settings.UnreliableInitialFlightByteCount = 64
	settings.UnreliableMinimumFlightByteCount = 64
	settings.UnreliableMaximumFlightByteCount = 256
	controller := newSendFlightController(settings)
	controller.applyPolicy(transferFlightPolicySnapshot{generation: 7})
	controller.byteCount, controller.messageCount = 128, 1
	key := sendSchedulingKey{valid: true}
	controller.messageCountByKey[key] = 1
	sequence := &SendSequence{sendBufferSettings: settings, flightController: controller}
	projector, ok := any(sequence).(interface {
		projectedReliableOnlyWrite(transferFlightPolicySnapshot) bool
	})
	if !ok {
		t.Fatal("nested routing cannot project a fresh generation without consuming it")
	}
	before := *controller
	before.messageCountByKey = maps.Clone(controller.messageCountByKey)
	fresh := transferFlightPolicySnapshot{generation: 8, limited: true, byteLimit: 64, reliableRouteAvailable: true}
	if !projector.projectedReliableOnlyWrite(fresh) || !reflect.DeepEqual(before, *controller) {
		t.Fatal("fresh overflow routing either used stale limits or mutated authoritative flight ownership")
	}
	if !controller.applyPolicy(fresh) {
		t.Fatal("nested projection consumed outer retired-carrier recovery")
	}
	if projector.projectedReliableOnlyWrite(transferFlightPolicySnapshot{generation: 9, h1Only: true, reliableRouteAvailable: true}) ||
		controller.generation != 8 || controller.byteCount != 128 || controller.messageCountByKey[key] != 1 {
		t.Fatal("H1 projection retained obsolete unreliable gating or mutated outstanding ownership")
	}
}
