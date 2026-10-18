// Failed selector writes retain causal diagnostics without changing dispatch ownership.
package connect

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// Every static and reflected select branch reports the branch actually selected.
func TestMultiRouteWriteFailureDisposition(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, routeCount := range []int{0, 1, 2, 3} {
		for _, kind := range []transferWriteFailureKind{
			transferWriteFailureRouteTimeout, transferWriteFailureCallerCanceled,
			transferWriteFailureCallerDeadline, transferWriteFailureSelectorClosed,
		} {
			synctest.Test(t, func(t *testing.T) {
				selector := NewMultiRouteSelector(context.Background(), "synthetic-failure", NewNoopLogger(), DestinationId(NewId()), false)
				defer selector.Close()
				routes := make([]Route, routeCount)
				for i := range routes {
					routes[i] = make(Route)
				}
				selector.updateTransport(NewSendGatewayTransportWithType(TransportTypeH1), routes)
				const wait = 7 * time.Second
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if kind == transferWriteFailureCallerDeadline {
					var cancelDeadline context.CancelFunc
					ctx, cancelDeadline = context.WithTimeout(ctx, wait)
					defer cancelDeadline()
				}
				timeout := time.Hour
				if kind == transferWriteFailureRouteTimeout {
					timeout = wait
				}
				message := MessagePoolGet(31)
				defer MessagePoolReturn(message)
				var success bool
				var disposition transferWriteDisposition
				var err error
				done := make(chan struct{})
				go func() {
					defer close(done)
					success, disposition, err = selector.writeDetailedWithCarrier(ctx, message, timeout)
				}()
				defer func() {
					cancel()
					selector.cancel()
					<-done
				}()
				synctest.Wait()
				time.Sleep(wait)
				if kind == transferWriteFailureCallerCanceled {
					cancel()
				} else if kind == transferWriteFailureSelectorClosed {
					selector.cancel()
				}
				<-done
				if success || disposition.failureKind != kind || !disposition.failureInitiallyBlocked || disposition.failureWaitDuration != wait {
					t.Fatalf("routes=%d kind=%d: success=%t disposition=%+v", routeCount, kind, success, disposition)
				}
				if disposition.initiallyBlocked || disposition.initialWaitDuration != 0 || disposition.transportType != "" {
					t.Fatalf("failed write changed legacy success disposition: %+v", disposition)
				}
				wantError := "Context done"
				if kind == transferWriteFailureSelectorClosed {
					wantError = "Done"
				}
				if kind == transferWriteFailureRouteTimeout {
					if err != nil {
						t.Fatalf("route expiry changed false/nil result: %v", err)
					}
				} else if err == nil || err.Error() != wantError {
					t.Fatalf("kind=%d returned error=%v, want %q", kind, err, wantError)
				}
			})
		}
	}
}

// Immediate refusal is not a timed wait, and a stale policy never attempted a route.
func TestMultiRouteWriteImmediateFailureDisposition(t *testing.T) {
	assertMessagePoolOwnership(t)
	selector := NewMultiRouteSelector(context.Background(), "synthetic-immediate", NewNoopLogger(), DestinationId(NewId()), false)
	defer selector.Close()
	message := MessagePoolGet(31)
	defer MessagePoolReturn(message)
	success, disposition, err := selector.writeDetailedWithCarrier(context.Background(), message, 0)
	if success || err != nil || disposition.failureKind != transferWriteFailureRouteTimeout || !disposition.failureInitiallyBlocked || disposition.failureWaitDuration != 0 {
		t.Fatalf("zero-wait refusal: success=%t disposition=%+v error=%v", success, disposition, err)
	}
	wrongGeneration := selector.activeRoutesSnapshot.Load().generation + 1
	success, disposition, err = selector.writeDetailedWithRoutePolicyGeneration(context.Background(), message, time.Hour, TransportTypeUnknown, false, wrongGeneration, true, nil)
	if success || !errors.Is(err, errTransferRoutePolicyChanged) || disposition.failureKind != transferWriteFailureOther || disposition.failureInitiallyBlocked || disposition.failureWaitDuration != 0 {
		t.Fatalf("policy refusal: success=%t disposition=%+v error=%v", success, disposition, err)
	}
}

// A successful write carries no failure evidence, including a blocked success.
func TestMultiRouteWriteSuccessfulDispositionHasNoFailure(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		selector := NewMultiRouteSelector(context.Background(), "synthetic-success", NewNoopLogger(), DestinationId(NewId()), false)
		defer selector.Close()
		route := make(Route, 1)
		selector.updateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{route})
		for _, blocked := range []bool{false, true} {
			if blocked {
				route <- nil
			}
			message := MessagePoolGet(31)
			var success bool
			var disposition transferWriteDisposition
			var err error
			done := make(chan struct{})
			go func() {
				defer close(done)
				success, disposition, err = selector.writeDetailedWithCarrier(context.Background(), message, time.Hour)
			}()
			synctest.Wait()
			if blocked {
				time.Sleep(time.Second)
				<-route
			}
			<-done
			if !success {
				MessagePoolReturn(message)
				t.Fatalf("success control failed: %v", err)
			}
			MessagePoolReturn(<-route)
			if err != nil || disposition.failureKind != transferWriteFailureNone || disposition.failureInitiallyBlocked || disposition.failureWaitDuration != 0 || disposition.initiallyBlocked != blocked {
				t.Fatalf("blocked=%t: disposition=%+v error=%v", blocked, disposition, err)
			}
		}
	})
}
