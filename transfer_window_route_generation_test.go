// Route changes retain one paced owner, its physical clock and its original deadlines.
package connect

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A real client owns the sender and both routes; the first successful write
// holds its worker so assertions cannot race later recovery or cleanup.
type windowRouteGenerationFixture struct {
	start            time.Time
	client           *Client
	sequence         *SendSequence
	h1               *sendGatewayTransport
	h3               *sendGatewayTransport
	route, alternate Route
	written          chan windowInitialLifetimeBoundary
	release          chan struct{}
	cancel           context.CancelFunc
	observations     int
	providerState    providerEvaluationState
}

// Settings, the owner barrier and pooled buffers have the same lifecycle in
// the preimage and successor; only tests explicitly changing a route do so.
// Callbacks receive the bubble's test so a fatal assertion unwinds that owner.
func runWindowRouteGenerationFixture(t *testing.T, configure func(*testing.T, *windowRouteGenerationFixture), check func(*testing.T, *windowRouteGenerationFixture)) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		destination := NewId()
		startWorker := make(chan struct{})
		fixture := &windowRouteGenerationFixture{
			route: make(Route, 8), alternate: make(Route, 8),
			written: make(chan windowInitialLifetimeBoundary, 1),
			release: make(chan struct{}), cancel: cancel,
			h1: NewSendGatewayTransportWithType(TransportTypeH1),
			h3: NewSendGatewayTransportWithType(TransportTypeH3),
		}
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
		settings.SendBufferSettings.ApplyWindowSizing()
		settings.SendBufferSettings.providerEvaluation = &providerEvaluationAttempt{
			owner: &fixture.providerState, destinationId: destination, observeLocalWrite: true,
		}
		settings.SendBufferSettings.TransferWireMessageObserver = func(TransferWireMessageObservation) { fixture.observations++ }
		settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
			if id.Destination == destination {
				select {
				case <-ctx.Done():
				case <-startWorker:
				}
			}
		}
		settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
			if id.Destination != destination || number != 0 {
				return
			}
			item := fixture.sequence.resendQueue.PeekFirst()
			if item == nil || !item.transportWriteObserved {
				return
			}
			fixture.written <- windowInitialLifetimeBoundary{
				at: time.Now(), offered: item.sendTime,
				physical: fixture.sequence.firstPhysicalRecoveryTime(item),
				recovery: item.resendTime, lifetime: item.sendTime.Add(item.ackTimeout),
			}
			select {
			case <-ctx.Done():
			case <-fixture.release:
			}
		}
		fixture.client = NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		defer func() {
			cancel()
			if err := fixture.client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			for _, route := range []Route{fixture.route, fixture.alternate} {
				for len(route) > 0 {
					MessagePoolReturn(<-route)
				}
			}
		}()
		fixture.client.ContractManager().AddNoContractPeer(destination)
		fixture.client.RouteManager().UpdateTransport(fixture.h1, []Route{fixture.route})
		fixture.sequence = fixture.client.sendBuffer.createSendSequence(sendSequenceId{Destination: destination}, &SendPack{Destination: destination, Ctx: ctx})
		synctest.Wait()
		fixture.start = time.Now()
		service := fixture.sequence.windowPacer.service
		service.next = fixture.start.Add(time.Second)
		fixture.sequence.windowPacer.rate = 1000000
		fixture.sequence.windowPacer.estimateRate = 1000000
		fixture.sequence.windowPacer.rateUpdated = fixture.start
		fixture.sequence.windowPacer.probeRate, fixture.sequence.windowPacer.probeLimit = 0, 0
		if configure != nil {
			configure(t, fixture)
		}
		frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1280)}
		clear(frame.MessageBytes)
		// Keep the synthetic caller budget beyond the one-second pacing
		// boundary. Dedicated 200ms and zero WriteTimeout cases still win.
		if !fixture.client.SendWithTimeout(frame, destination, func(error) {}, 3*time.Second) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("fixture offer was refused")
		}
		close(startWorker)
		synctest.Wait()
		check(t, fixture)
	})
}

// Replacing an H1-only route must release its pending original immediately,
// without turning the reservation into another probe or discarding its debt.
func TestWindowPacingRouteGenerationWakesPendingOriginal(t *testing.T) {
	// The semantic preimage failure must unwind the bubble, not its parent.
	outerT := t
	var bubbleT *testing.T
	runWindowRouteGenerationFixture(t, func(t *testing.T, _ *windowRouteGenerationFixture) {
		bubbleT = t
		if t == outerT {
			t.Error("fixture configure callback received the outer test")
		}
	}, func(t *testing.T, fixture *windowRouteGenerationFixture) {
		if t == outerT || t != bubbleT {
			t.Error("fixture check callback did not receive the same bubble test")
			return
		}
		service := fixture.sequence.windowPacer.service
		beforeNext, beforeProbe, beforeSent := service.next, service.probeSent, service.sent
		if service.pacingReservations != 1 || len(fixture.written) != 0 {
			t.Fatal("original did not reach its serialization wait")
		}
		fixture.client.RouteManager().UpdateTransport(fixture.h1, nil)
		fixture.client.RouteManager().UpdateTransport(fixture.h3, []Route{fixture.alternate})
		synctest.Wait()
		if len(fixture.written) != 1 || len(fixture.alternate) != 1 || len(fixture.route) != 0 {
			t.Fatal("published reliable replacement remained hidden by the H1 serialization wait")
		}
		if service.next != beforeNext || service.probeSent != beforeProbe || service.sent != beforeSent ||
			service.pacingReservations != 0 || service.reservedByteCount != 0 {
			t.Fatal("route bypass recharged or refunded a reservation")
		}
		if fixture.observations != 1 {
			t.Fatal("route revalidation duplicated the logical write observation")
		}
	})
}

// A publication between reservation and the first park is a level change,
// not a notification that can be missed by subscribing afterward.
func TestWindowPacingRouteGenerationPublicationBeforePark(t *testing.T) {
	changed := false
	runWindowRouteGenerationFixture(t, func(t *testing.T, fixture *windowRouteGenerationFixture) {
		fixture.sequence.beforeContractWriterAccessForTest = func(publish bool) {
			if !publish && !changed && fixture.sequence.windowPacer.service.pacingReservations == 1 {
				changed = true
				fixture.client.RouteManager().UpdateTransport(fixture.h1, nil)
				fixture.client.RouteManager().UpdateTransport(fixture.h3, []Route{fixture.alternate})
			}
		}
	}, func(t *testing.T, fixture *windowRouteGenerationFixture) {
		if !changed || len(fixture.written) != 1 || len(fixture.alternate) != 1 {
			t.Fatal("pre-park route publication did not release the same original")
		}
		if fixture.sequence.windowPacer.service.pacingReservations != 0 {
			t.Fatal("pre-park replacement retained a waiter")
		}
	})
}

// A new H1 generation re-arms the notification without bypassing the original
// deadline or reserving another physical envelope.
func TestWindowPacingRouteGenerationH1ReplacementKeepsDebt(t *testing.T) {
	runWindowRouteGenerationFixture(t, nil, func(t *testing.T, fixture *windowRouteGenerationFixture) {
		service := fixture.sequence.windowPacer.service
		service.stateLock.Lock()
		next, probe, sent := service.next, service.probeSent, service.sent
		service.stateLock.Unlock()
		original := fixture.sequence.transferFlightPolicy()
		fixture.client.RouteManager().UpdateTransport(fixture.h1, []Route{fixture.alternate})
		synctest.Wait()
		current := fixture.sequence.transferFlightPolicy()
		if !current.h1Only || current.generation == original.generation {
			t.Fatal("fixture did not publish a distinct H1 generation")
		}
		service.stateLock.Lock()
		debtPreserved := service.next == next && service.probeSent == probe && service.sent == sent && service.pacingReservations == 1
		service.stateLock.Unlock()
		if len(fixture.written) != 0 || !debtPreserved {
			t.Fatal("H1 replacement bypassed or repeated the original charge")
		}
		time.Sleep(time.Until(fixture.start.Add(time.Second)))
		synctest.Wait()
		service.stateLock.Lock()
		reservationReleased := service.pacingReservations == 0
		service.stateLock.Unlock()
		if len(fixture.alternate) != 1 || len(fixture.written) != 1 || !reservationReleased {
			t.Fatal("new H1 generation did not preserve the original physical deadline")
		}
	})
}

// A missing route can park the ordinary writer, but its later H1 replacement
// must resume the pending reservation rather than inherit the non-H1 bypass.
func TestWindowPacingRouteGenerationNoneToH1ResumesOneReservation(t *testing.T) {
	runWindowRouteGenerationFixture(t, nil, func(t *testing.T, fixture *windowRouteGenerationFixture) {
		service := fixture.sequence.windowPacer.service
		service.stateLock.Lock()
		next, sent, probe := service.next, service.sent, service.probeSent
		service.stateLock.Unlock()
		fixture.client.RouteManager().UpdateTransport(fixture.h1, nil)
		synctest.Wait()
		fixture.client.RouteManager().UpdateTransport(fixture.h1, []Route{fixture.route})
		synctest.Wait()
		service.stateLock.Lock()
		debtPreserved := service.next == next && service.sent == sent && service.probeSent == probe && service.pacingReservations == 1
		service.stateLock.Unlock()
		if len(fixture.written) != 0 || !debtPreserved {
			t.Fatal("H1 reentry bypassed or double-reserved pending pacing")
		}
		time.Sleep(time.Until(fixture.start.Add(time.Second)))
		synctest.Wait()
		service.stateLock.Lock()
		reservationReleased := service.pacingReservations == 0 && service.reservedByteCount == 0
		service.stateLock.Unlock()
		if len(fixture.written) != 1 || !reservationReleased {
			t.Fatal("H1 reentry failed to finish its one reservation")
		}
	})
}

// The original non-H1 attempt was committed but accepted no bytes. If a later
// H1 generation needs pacing, first-physical recovery starts after that wait.
func TestWindowPacingRouteGenerationBlockedReentryRestampsPhysicalTime(t *testing.T) {
	runWindowRouteGenerationFixture(t, func(t *testing.T, fixture *windowRouteGenerationFixture) {
		fixture.alternate = make(Route)
		fixture.client.RouteManager().UpdateTransport(fixture.h1, nil)
		fixture.client.RouteManager().UpdateTransport(fixture.h3, []Route{fixture.alternate})
	}, func(t *testing.T, fixture *windowRouteGenerationFixture) {
		if fixture.observations != 1 || len(fixture.written) != 0 || fixture.providerState.pendingLocalWrites.Load() != 1 {
			t.Fatal("non-H1 dispatch did not reach the blocked writer")
		}
		fixture.client.RouteManager().UpdateTransport(fixture.h3, nil)
		fixture.client.RouteManager().UpdateTransport(fixture.h1, []Route{fixture.route})
		synctest.Wait()
		if len(fixture.written) != 0 || len(fixture.route) != 0 ||
			fixture.providerState.pendingLocalWrites.Load() != 1 || fixture.providerState.localWriteFailed.Load() {
			t.Fatal("new H1 generation dispatched before its existing pacing debt")
		}
		time.Sleep(time.Until(fixture.start.Add(time.Second)))
		synctest.Wait()
		if len(fixture.written) != 1 {
			t.Fatal("paced H1 replacement did not dispatch")
		}
		boundary := <-fixture.written
		item := fixture.sequence.resendQueue.PeekFirst()
		if boundary.physical != fixture.start.Add(time.Second) ||
			boundary.recovery != boundary.physical.Add(fixture.sequence.resendIntervalForItem(item, 1)) {
			t.Fatalf("unconsumed blocked attempt polluted physical recovery: physical=%s recovery=%s", boundary.physical.Sub(fixture.start), boundary.recovery.Sub(fixture.start))
		}
		if fixture.observations != 1 || item.sendCount != 1 || fixture.providerState.pendingLocalWrites.Load() != 0 ||
			fixture.providerState.localWriteFailed.Load() || !fixture.providerState.applicationWriteAdmitted.Load() {
			t.Fatal("policy retry duplicated observation or send count")
		}
	})
}

// Route revalidation after a timer reaches its deadline precedes spending
// meter credit; the original serialization/probe accounting stays charged.
func TestWindowPacingRouteGenerationDeadlineEdgeAvoidsNewMeterSpend(t *testing.T) {
	var beforeSpent float64
	runWindowRouteGenerationFixture(t, func(t *testing.T, fixture *windowRouteGenerationFixture) {
		fixture.sequence.windowPacer.afterWaitForTest = func() {
			beforeSpent = fixture.sequence.windowPacer.service.burstMeter.spent
			fixture.client.RouteManager().UpdateTransport(fixture.h1, nil)
			fixture.client.RouteManager().UpdateTransport(fixture.h3, []Route{fixture.alternate})
		}
	}, func(t *testing.T, fixture *windowRouteGenerationFixture) {
		time.Sleep(time.Until(fixture.start.Add(time.Second)))
		synctest.Wait()
		if len(fixture.alternate) != 1 || fixture.sequence.windowPacer.service.burstMeter.spent != beforeSpent {
			t.Fatal("already-due route replacement spent obsolete H1 dispatch credit")
		}
	})
}

// An admission-edge route swap must not repeat the paid burst or its hook.
func TestWindowPacingRouteGenerationPaidAdmissionIsNotRepeated(t *testing.T) {
	admissions := 0
	var next time.Time
	var spent float64
	runWindowRouteGenerationFixture(t, func(t *testing.T, fixture *windowRouteGenerationFixture) {
		fixture.sequence.windowPacer.afterAdmissionForTest = func() {
			admissions++
			next = fixture.sequence.windowPacer.service.next
			spent = fixture.sequence.windowPacer.service.burstMeter.spent
			fixture.client.RouteManager().UpdateTransport(fixture.h1, nil)
			fixture.client.RouteManager().UpdateTransport(fixture.h3, []Route{fixture.alternate})
			fixture.client.RouteManager().UpdateTransport(fixture.h3, nil)
			fixture.client.RouteManager().UpdateTransport(fixture.h1, []Route{fixture.route})
		}
	}, func(t *testing.T, fixture *windowRouteGenerationFixture) {
		time.Sleep(time.Until(fixture.start.Add(time.Second)))
		synctest.Wait()
		service := fixture.sequence.windowPacer.service
		if admissions != 1 || len(fixture.route) != 1 || len(fixture.alternate) != 0 ||
			service.next != next || service.burstMeter.spent != spent || service.pacingReservations != 0 {
			t.Fatal("dispatch-edge generation cycle repeated paid admission")
		}
	})
}

// The settings hook observes the actual writer deadline before publishing H1.
// A new inner wait cannot extend that already established absolute bound.
func TestWindowPacingRouteGenerationFirstInnerWaitKeepsWriteDeadline(t *testing.T) {
	changed := false
	var deadline time.Time
	runWindowRouteGenerationFixture(t, func(t *testing.T, fixture *windowRouteGenerationFixture) {
		fixture.alternate = make(Route)
		fixture.client.RouteManager().UpdateTransport(fixture.h1, nil)
		fixture.client.RouteManager().UpdateTransport(fixture.h3, []Route{fixture.alternate})
		fixture.sequence.sendBufferSettings.WriteTimeout = 200 * time.Millisecond
		fixture.sequence.sendBufferSettings.afterTransferWriteDeadlineForTest = func(until time.Time) {
			if changed || time.Now() != fixture.start || until != fixture.start.Add(200*time.Millisecond) ||
				fixture.sequence.windowPacer.service.pacingReservations != 0 || fixture.observations != 0 {
				t.Fatal("fixture did not reach its first unobserved absolute writer budget")
			}
			deadline, changed = until, true
			fixture.client.RouteManager().UpdateTransport(fixture.h3, nil)
			fixture.client.RouteManager().UpdateTransport(fixture.h1, []Route{fixture.route})
		}
	}, func(t *testing.T, fixture *windowRouteGenerationFixture) {
		if !changed {
			t.Fatal("fixture did not publish H1 at the first inner revalidation")
		}
		time.Sleep(time.Until(deadline))
		synctest.Wait()
		if len(fixture.written) != 0 || len(fixture.route) != 0 || fixture.observations != 0 ||
			fixture.sequence.windowPacer.service.pacingReservations != 0 {
			t.Fatal("new inner pacing exceeded the original writer budget or fabricated a dispatch")
		}
	})
}

// Zero means one ready attempt, not an already-expired general deadline and
// not permission to park behind a newly required H1 serialization interval.
func TestWindowPacingRouteGenerationZeroBudgetIsReadyOnly(t *testing.T) {
	for _, ready := range []bool{true, false} {
		runWindowRouteGenerationFixture(t, func(t *testing.T, fixture *windowRouteGenerationFixture) {
			fixture.alternate = make(Route)
			fixture.client.RouteManager().UpdateTransport(fixture.h1, nil)
			fixture.client.RouteManager().UpdateTransport(fixture.h3, []Route{fixture.alternate})
			fixture.sequence.sendBufferSettings.WriteTimeout = 0
			if ready {
				fixture.sequence.windowPacer.service.next = fixture.start
			}
			fixture.sequence.sendBufferSettings.afterTransferWriteDeadlineForTest = func(until time.Time) {
				if until != fixture.start || time.Now() != fixture.start || fixture.observations != 0 {
					t.Fatal("zero-budget witness was not its first unobserved attempt")
				}
				fixture.client.RouteManager().UpdateTransport(fixture.h3, nil)
				fixture.client.RouteManager().UpdateTransport(fixture.h1, []Route{fixture.route})
			}
		}, func(t *testing.T, fixture *windowRouteGenerationFixture) {
			want := 0
			if ready {
				want = 1
			}
			service := fixture.sequence.windowPacer.service
			if time.Now() != fixture.start || len(fixture.written) != want || len(fixture.route) != want ||
				fixture.observations != want || service.pacingReservations != 0 || service.reservedByteCount != 0 {
				t.Fatalf("ready=%t zero budget waited, fabricated a write or rejected ready admission", ready)
			}
			if !ready && !service.next.After(fixture.start.Add(time.Second)) {
				t.Fatal("refused zero-budget attempt refunded its serialization debt")
			}
		})
	}
}

// These sibling fixtures own one completion result. Cancel and join before
// closing the pacer; an already-consumed normal result is never received twice.
func closeWindowRouteSiblingAfterJoin(sibling *windowBurstPacer, cancel context.CancelFunc, done <-chan error, joined bool) {
	cancel()
	if !joined {
		<-done
	}
	sibling.close()
}

// A non-H1 writer can block while a charged H1 reservation remains pending.
// Cancellation releases only that owner, then its already-charged successor.
func TestWindowPacingRouteGenerationPendingBypassKeepsSharedFifo(t *testing.T) {
	runWindowRouteGenerationFixture(t, func(t *testing.T, fixture *windowRouteGenerationFixture) {
		fixture.alternate = make(Route)
	}, func(t *testing.T, fixture *windowRouteGenerationFixture) {
		service := fixture.sequence.windowPacer.service
		siblingCtx, siblingCancel := context.WithCancel(context.Background())
		sibling := windowBurstPacer{service: service, rate: 1000000, estimateRate: 1000000, serviceSequenceId: NewId()}
		done := make(chan error, 1)
		go func() { done <- sibling.waitForService(siblingCtx, 1280) }()
		joined := false
		defer func() { closeWindowRouteSiblingAfterJoin(&sibling, siblingCancel, done, joined) }()
		synctest.Wait()
		next, probe := service.next, service.probeSent
		fixture.client.RouteManager().UpdateTransport(fixture.h1, nil)
		fixture.client.RouteManager().UpdateTransport(fixture.h3, []Route{fixture.alternate})
		time.Sleep(time.Until(fixture.start.Add(time.Second)))
		synctest.Wait()
		if service.pacingReservations != 2 || service.waiterHead != &fixture.sequence.windowPacer.waiter ||
			len(done) != 0 || fixture.providerState.pendingLocalWrites.Load() != 1 {
			t.Fatal("blocked bypass released its pending FIFO ownership")
		}
		fixture.cancel()
		synctest.Wait()
		if len(done) != 1 {
			t.Fatal("cancellation did not release the existing FIFO successor")
		}
		err := <-done
		joined = true
		if err != nil {
			t.Fatal(err)
		}
		if service.pacingReservations != 0 || service.reservedByteCount != 0 || service.next != next || service.probeSent != probe ||
			fixture.providerState.pendingLocalWrites.Load() != 0 {
			t.Fatal("cancellation duplicated ownership or refunded shared debt")
		}
	})
}

// Early-return cleanup joins a cancelled FIFO sibling before closing its
// retained service, without disturbing the still-blocked bypass owner.
func TestWindowPacingRouteGenerationSiblingCleanupJoinsBeforeClose(t *testing.T) {
	runWindowRouteGenerationFixture(t, func(t *testing.T, fixture *windowRouteGenerationFixture) {
		fixture.alternate = make(Route)
	}, func(t *testing.T, fixture *windowRouteGenerationFixture) {
		service := fixture.sequence.windowPacer.service
		service.stateLock.Lock()
		primaryReserved := service.reservedByteCount
		service.stateLock.Unlock()
		siblingCtx, siblingCancel := context.WithCancel(context.Background())
		sibling := windowBurstPacer{service: service, rate: 1000000, estimateRate: 1000000, serviceSequenceId: NewId()}
		type completion struct {
			err                error
			serviceClosed      bool
			pacingReservations int
			reservedByteCount  ByteCount
			waiterHead         *windowPacingWaiter
			next               time.Time
			probeSent          ByteCount
			sent               ByteCount
		}
		completed := make(chan completion, 1)
		done := make(chan error, 1)
		go func() {
			err := sibling.waitForService(siblingCtx, 1280)
			service.stateLock.Lock()
			got := completion{
				err: err, serviceClosed: sibling.serviceClosed,
				pacingReservations: service.pacingReservations, reservedByteCount: service.reservedByteCount,
				waiterHead: service.waiterHead, next: service.next, probeSent: service.probeSent, sent: service.sent,
			}
			service.stateLock.Unlock()
			completed <- got
			done <- err
		}()
		var next time.Time
		var probe, sent ByteCount
		func() {
			// Return without consuming done, exactly like an assertion failure
			// before the ordinary positive path reaches its completion receive.
			defer func() { closeWindowRouteSiblingAfterJoin(&sibling, siblingCancel, done, false) }()
			synctest.Wait()
			fixture.client.RouteManager().UpdateTransport(fixture.h1, nil)
			fixture.client.RouteManager().UpdateTransport(fixture.h3, []Route{fixture.alternate})
			time.Sleep(time.Until(fixture.start.Add(time.Second)))
			synctest.Wait()
			now := time.Now()
			service.stateLock.Lock()
			next, probe, sent = service.next, service.probeSent, service.sent
			retained := service.pacingReservations == 2 && service.reservedByteCount == primaryReserved+1280 &&
				service.waiterHead == &fixture.sequence.windowPacer.waiter && service.waiterTail == &sibling.waiter &&
				!sibling.waiter.deadline.After(now)
			service.stateLock.Unlock()
			if !retained || len(done) != 0 || len(completed) != 0 || fixture.providerState.pendingLocalWrites.Load() != 1 {
				t.Fatal("fixture did not hold the expired-deadline sibling behind the blocked bypass owner")
			}
		}()
		got := <-completed
		if !errors.Is(got.err, context.Canceled) || got.serviceClosed || got.pacingReservations != 1 ||
			got.reservedByteCount != primaryReserved || got.waiterHead != &fixture.sequence.windowPacer.waiter ||
			got.next != next || got.probeSent != probe || got.sent != sent {
			t.Fatalf("sibling closed before its completion or changed the surviving owner: %+v", got)
		}
		service.stateLock.Lock()
		survivorPreserved := sibling.serviceClosed && service.pacingReservations == 1 && service.reservedByteCount == primaryReserved &&
			service.waiterHead == &fixture.sequence.windowPacer.waiter && service.waiterTail == &fixture.sequence.windowPacer.waiter &&
			service.next == next && service.probeSent == probe && service.sent == sent-1280
		service.stateLock.Unlock()
		if !survivorPreserved || len(done) != 0 || fixture.providerState.pendingLocalWrites.Load() != 1 ||
			len(fixture.written) != 0 || len(fixture.route) != 0 || len(fixture.alternate) != 0 || time.Now() != fixture.start.Add(time.Second) {
			t.Fatal("joined sibling cleanup released the bypass owner, rewrote its debt or fabricated a dispatch")
		}
	})
}

// An equal-priority mixed publication preserves direct H1 affinity. Only
// withdrawal makes H3 eligible; its actual carrier then owns the item.
// Separate eligibility tests require that still-selected H1 retain pacing.
func TestWindowPacingRouteGenerationUnreliableReplacementIsRevalidated(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		runWindowRouteGenerationFixture(t, func(t *testing.T, fixture *windowRouteGenerationFixture) {
			fixture.route = make(Route)
			fixture.client.RouteManager().UpdateTransport(fixture.h1, []Route{fixture.route})
		}, func(t *testing.T, fixture *windowRouteGenerationFixture) {
			service := fixture.sequence.windowPacer.service
			next, probe := service.next, service.probeSent
			if !mixed {
				fixture.client.RouteManager().UpdateTransport(fixture.h1, nil)
			}
			fixture.client.RouteManager().UpdateTransportWithProperties(fixture.h3, []Route{fixture.alternate}, TransferCarrierProperties{Unreliable: true})
			synctest.Wait()
			policy := fixture.sequence.transferFlightPolicy()
			item := fixture.sequence.resendQueue.PeekFirst()
			if mixed {
				snapshot := fixture.sequence.contractMultiRouteWriter.(*MultiRouteSelector).activeRoutesSnapshot.Load()
				if policy.h1Only || !policy.limited || !policy.reliableRouteAvailable ||
					snapshot.preferDirectRoute != fixture.route || len(fixture.written) != 0 || len(fixture.alternate) != 0 ||
					service.next != next || service.probeSent != probe || service.pacingReservations != 1 {
					t.Fatal("mixed publication did not preserve its existing direct-carrier affinity")
				}
				fixture.client.RouteManager().UpdateTransport(fixture.h1, nil)
				synctest.Wait()
				policy = fixture.sequence.transferFlightPolicy()
				item = fixture.sequence.resendQueue.PeekFirst()
			}
			if policy.h1Only || !policy.limited || policy.reliableRouteAvailable || len(fixture.written) != 1 ||
				len(fixture.alternate) != 1 || item == nil || !item.unreliableCarrierObserved ||
				service.next != next || service.probeSent != probe || service.pacingReservations != 0 {
				t.Fatalf("mixed=%t replacement retained obsolete H1 pacing or carrier ownership", mixed)
			}
		})
	}
}
