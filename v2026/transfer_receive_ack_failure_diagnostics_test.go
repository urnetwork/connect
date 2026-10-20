// Real acknowledgement workers retain failure causes and leave legacy totals intact.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// The actual worker distinguishes a closed selector from a route-budget expiry.
func TestReceiveSequenceAckFailureDiagnostics(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, transport := range []TransportType{TransportTypeUnknown, TransportTypeH1, TransportTypeP2p} {
		for _, timeout := range []bool{false, true} {
			synctest.Test(t, func(t *testing.T) {
				opened := make(chan *MultiRouteSelector, 1)
				owner := &ackRouteWriteFailureCounters{}
				fixture := newAckGapTestSequenceWithClientSettings(t, func(settings *ReceiveBufferSettings) {
					settings.AckCompressTimeout = 0
					settings.WriteTimeout = 15 * time.Second
					settings.afterAckWriterOpenForTest = func(_ receiveSequenceId, writer MultiRouteWriter) {
						opened <- writer.(*MultiRouteSelector)
					}
				}, func(settings *ClientSettings) {
					settings.EnableAckRouteWriteDiagnostics = true
					settings.ackRouteWriteFailureOwner = owner
				})
				writer := <-opened
				for range cap(fixture.route) {
					fixture.route <- nil
				}
				fixture.receiveSequence.sendAck(1, NewId(), false, sequenceTag{}, false, transport)
				synctest.Wait()
				wait := 7 * time.Second
				if timeout {
					wait = 15 * time.Second
				}
				time.Sleep(wait)
				if !timeout {
					writer.cancel()
				}
				synctest.Wait()
				stats := fixture.receiveSequence.client.ReceiveStats()
				failure := stats.AckRouteWriteFailures
				if failure == nil || failure.ErrorCount != 1 || failure.BlockedCount != 1 || failure.WaitDuration != wait || failure.MaxWait != wait {
					t.Fatalf("transport=%s timeout=%t: failure=%+v", transport, timeout, failure)
				}
				wantTimeout, wantClosed := uint64(0), uint64(1)
				if timeout {
					wantTimeout, wantClosed = 1, 0
				}
				if failure.RouteTimeoutCount != wantTimeout || failure.SelectorClosedCount != wantClosed || failure.CallerCanceledCount != 0 || failure.CallerDeadlineCount != 0 || failure.OtherCount != 0 {
					t.Fatalf("transport=%s timeout=%t: wrong selected cause %+v", transport, timeout, failure)
				}
				if stats.AckRouteWriteCount != 1 || stats.AckRouteWriteErrorCount != 1 || stats.AckRouteWriteTimeoutByTransport[transport] != wantTimeout || stats.AckRouteWriteBlockedCount != 0 || stats.AckRouteWriteWaitDuration != 0 {
					t.Fatalf("transport=%s timeout=%t: legacy accounting %+v", transport, timeout, stats)
				}
				if got := owner.snapshot(); got == nil || *got != *failure {
					t.Fatalf("owner did not capture the actual worker failure: %+v", got)
				}
			})
		}
	}
}

// Unknown writes keep their original carrier/wait reporting even when the real route blocks.
func TestReceiveSequenceAckUnknownSuccessKeepsLegacyAccounting(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan *MultiRouteSelector, 1)
		fixture := newAckGapTestSequenceWithClientSettings(t, func(settings *ReceiveBufferSettings) {
			settings.AckCompressTimeout = 0
			settings.WriteTimeout = time.Hour
			settings.afterAckWriterOpenForTest = func(_ receiveSequenceId, writer MultiRouteWriter) {
				opened <- writer.(*MultiRouteSelector)
			}
		}, func(settings *ClientSettings) { settings.EnableAckRouteWriteDiagnostics = true })
		writer := <-opened
		// Replace only this standalone selector's policy; the receiver's original
		// transport remains installed for the fixture's normal joined cleanup.
		writer.mutex.Lock()
		for transport := range writer.transportRoutes {
			delete(writer.transportRoutes, transport)
		}
		writer.mutex.Unlock()
		writer.updateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{fixture.route})
		for range cap(fixture.route) {
			fixture.route <- nil
		}
		messageId := NewId()
		fixture.receiveSequence.sendAck(1, messageId, false, sequenceTag{}, false, TransportTypeUnknown)
		synctest.Wait()
		time.Sleep(time.Second)
		for range cap(fixture.route) {
			<-fixture.route
		}
		if got, ok := fixture.readAck(t, time.Second); !ok || got != messageId {
			t.Fatal("unknown-carrier ACK did not complete through the same route")
		}
		synctest.Wait()
		stats := fixture.receiveSequence.client.ReceiveStats()
		if stats.AckRouteWriteCount != 1 || stats.AckRouteWriteCountByTransport[TransportTypeUnknown] != 1 || stats.AckRouteWriteCountByTransport[TransportTypeH1] != 0 || stats.AckRouteWriteBlockedCount != 0 || stats.AckRouteWriteWaitDuration != 0 || stats.AckRouteWriteErrorCount != 0 || stats.AckRouteWriteFailures != nil {
			t.Fatalf("unknown success changed legacy accounting: %+v", stats)
		}
	})
}
