// Write eligibility bounds H1 pacing without broadening mixed-route windows.
package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// A mixed publication does not make the preferred H1 writer another carrier.
// Both reliable and datagram H3 alternatives leave its charged deadline intact.
func TestWindowPacingWriteEligibilityMixedH1RetainsDeadline(t *testing.T) {
	for _, unreliable := range []bool{false, true} {
		var stateLock sync.Mutex
		runWindowRouteGenerationFixture(t, func(t *testing.T, fixture *windowRouteGenerationFixture) {
			// Configure runs while beforeRunSendSequenceForTest holds the owner;
			// releasing startWorker publishes this callback before its first use.
			fixture.sequence.sendBufferSettings.TransferWireMessageObserver = func(TransferWireMessageObservation) {
				stateLock.Lock()
				fixture.observations++
				stateLock.Unlock()
			}
		}, func(t *testing.T, fixture *windowRouteGenerationFixture) {
			observationCount := func() int {
				stateLock.Lock()
				defer stateLock.Unlock()
				return fixture.observations
			}
			service := fixture.sequence.windowPacer.service
			service.stateLock.Lock()
			next, probe, sent := service.next, service.probeSent, service.sent
			service.stateLock.Unlock()
			before := fixture.sequence.transferFlightPolicy()
			fixture.client.RouteManager().UpdateTransportWithProperties(fixture.h3, []Route{fixture.alternate}, TransferCarrierProperties{Unreliable: unreliable})
			synctest.Wait()
			policy := fixture.sequence.transferFlightPolicy()
			snapshot := fixture.sequence.contractMultiRouteWriter.(*MultiRouteSelector).activeRoutesSnapshot.Load()
			if policy.generation == before.generation || policy.h1Only || policy.limited != unreliable || !policy.reliableRouteAvailable ||
				snapshot.preferDirectRoute != fixture.route {
				t.Fatal("fixture did not publish mixed policy with the same preferred H1")
			}
			service.stateLock.Lock()
			debtPreserved := service.pacingReservations == 1 && service.next == next && service.probeSent == probe && service.sent == sent
			service.stateLock.Unlock()
			if len(fixture.written) != 0 || len(fixture.route) != 0 || len(fixture.alternate) != 0 || observationCount() != 0 ||
				fixture.providerState.pendingLocalWrites.Load() != 0 || !debtPreserved {
				t.Fatalf("unreliable=%t mixed preferred H1 bypassed or repeated its owed service", unreliable)
			}
			time.Sleep(time.Until(fixture.start.Add(time.Second)))
			synctest.Wait()
			item := fixture.sequence.resendQueue.PeekFirst()
			service.stateLock.Lock()
			reservationReleased := service.pacingReservations == 0 && service.reservedByteCount == 0
			service.stateLock.Unlock()
			if len(fixture.written) != 1 || len(fixture.route) != 1 || len(fixture.alternate) != 0 || item == nil ||
				!item.reliableCarrierObserved || item.unreliableCarrierObserved || item.carrierRoute != fixture.route ||
				!reservationReleased || observationCount() != 1 {
				t.Fatalf("unreliable=%t preferred H1 lost its exact dispatch ownership", unreliable)
			}
			boundary := <-fixture.written
			if boundary.physical != fixture.start.Add(time.Second) {
				t.Fatalf("mixed H1 physical clock=%s, want 1s", boundary.physical.Sub(fixture.start))
			}
		})
	}
}

// Withdrawing the selected H1, not merely publishing H3, makes the alternate
// eligible immediately. Neither reliable nor datagram H3 inherits H1 debt.
func TestWindowPacingWriteEligibilityH3RetirementKeepsCarrierOwnership(t *testing.T) {
	for _, unreliable := range []bool{false, true} {
		runWindowRouteGenerationFixture(t, func(t *testing.T, fixture *windowRouteGenerationFixture) {
			fixture.route = make(Route)
			fixture.client.RouteManager().UpdateTransport(fixture.h1, []Route{fixture.route})
		}, func(t *testing.T, fixture *windowRouteGenerationFixture) {
			service := fixture.sequence.windowPacer.service
			next, probe := service.next, service.probeSent
			fixture.client.RouteManager().UpdateTransportWithProperties(fixture.h3, []Route{fixture.alternate}, TransferCarrierProperties{Unreliable: unreliable})
			synctest.Wait()
			if len(fixture.written) != 0 || len(fixture.alternate) != 0 || service.pacingReservations != 1 {
				t.Fatal("mixed publication discarded the still-selected H1 owner")
			}
			fixture.client.RouteManager().UpdateTransport(fixture.h1, nil)
			synctest.Wait()
			item := fixture.sequence.resendQueue.PeekFirst()
			if len(fixture.written) != 1 || len(fixture.alternate) != 1 || len(fixture.route) != 0 || item == nil ||
				item.carrierRoute != fixture.alternate || item.unreliableCarrierObserved != unreliable ||
				item.reliableCarrierObserved == unreliable || fixture.observations != 1 ||
				fixture.providerState.pendingLocalWrites.Load() != 0 || service.pacingReservations != 0 ||
				service.reservedByteCount != 0 || service.next != next || service.probeSent != probe {
				t.Fatalf("unreliable=%t eligible H3 repeated debt or inherited another carrier", unreliable)
			}
			if boundary := <-fixture.written; boundary.physical != fixture.start {
				t.Fatalf("eligible H3 retained H1 pacing: physical=%s", boundary.physical.Sub(fixture.start))
			}
		})
	}
}

// P2P is outside the tied direct H1/H3 affinity. A ready P2P route remains
// eligible in the ordinary mixed set, with reliability from its actual lane.
func TestWindowPacingWriteEligibilityP2pAndMixedRemainUnpaced(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		for _, unreliable := range []bool{false, true} {
			runWindowRouteGenerationFixture(t, func(t *testing.T, fixture *windowRouteGenerationFixture) {
				fixture.route = make(Route)
				fixture.h3 = NewSendGatewayTransportWithType(TransportTypeP2p)
				fixture.client.RouteManager().UpdateTransport(fixture.h1, []Route{fixture.route})
			}, func(t *testing.T, fixture *windowRouteGenerationFixture) {
				service := fixture.sequence.windowPacer.service
				next, probe := service.next, service.probeSent
				if !mixed {
					fixture.client.RouteManager().UpdateTransport(fixture.h1, nil)
				}
				fixture.client.RouteManager().UpdateTransportWithProperties(fixture.h3, []Route{fixture.alternate}, TransferCarrierProperties{Unreliable: unreliable})
				synctest.Wait()
				policy := fixture.sequence.transferFlightPolicy()
				snapshot := fixture.sequence.contractMultiRouteWriter.(*MultiRouteSelector).activeRoutesSnapshot.Load()
				item := fixture.sequence.resendQueue.PeekFirst()
				if policy.h1Only || policy.limited != unreliable || policy.reliableRouteAvailable != (mixed || !unreliable) ||
					snapshot.preferDirectRoute != nil || len(fixture.written) != 1 || len(fixture.alternate) != 1 ||
					item == nil || item.carrierRoute != fixture.alternate || item.unreliableCarrierObserved != unreliable ||
					item.reliableCarrierObserved == unreliable || service.pacingReservations != 0 ||
					service.reservedByteCount != 0 || service.next != next || service.probeSent != probe || fixture.observations != 1 {
					t.Fatalf("mixed=%t unreliable=%t P2P acquired direct H1 affinity, pacing or wrong ownership", mixed, unreliable)
				}
			})
		}
	}
}

// Both an explicit reliable-only request and a full real datagram flight
// select H1 from a mixed set. The projected decision cannot consume the outer
// generation or bypass the physical service already owed by that H1 writer.
func TestWindowPacingWriteEligibilityReliableOnlyRetainsDeadline(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, transportType := range []TransportType{TransportTypeH3, TransportTypeP2p} {
		for _, requested := range []bool{false, true} {
			synctest.Test(t, func(t *testing.T) {
				sequence, first, selector, h1Route, observations, state := newWindowRouteAccountingFixture(t, 1)
				var stateLock sync.Mutex
				sequence.sendBufferSettings.TransferWireMessageObserver = func(TransferWireMessageObservation) {
					stateLock.Lock()
					*observations++
					stateLock.Unlock()
				}
				observationCount := func() int {
					stateLock.Lock()
					defer stateLock.Unlock()
					return *observations
				}
				start := time.Now()
				selector.mutex.Lock()
				var h1 Transport
				for transport := range selector.transportRoutes {
					h1 = transport
				}
				selector.mutex.Unlock()
				selector.updateTransport(h1, nil)
				alternate := NewSendGatewayTransportWithType(transportType)
				alternateRoute := make(Route, 1)
				defer func() {
					for len(alternateRoute) > 0 {
						MessagePoolReturn(<-alternateRoute)
					}
				}()
				selector.updateTransportWithProperties(alternate, []Route{alternateRoute}, TransferCarrierProperties{Unreliable: true})
				if !requested {
					sequence.sendBufferSettings.UnreliableInitialFlightByteCount = 1
					sequence.sendBufferSettings.UnreliableMinimumFlightByteCount = 1
					sequence.sendBufferSettings.UnreliableMaximumFlightByteCount = 1
				}
				sequence.flightController = newSendFlightController(sequence.sendBufferSettings)
				sequence.flightController.applyPolicy(selector.transferFlightPolicy())
				firstDisposition, err := sequence.writeMaybeWrappedBytes(first.transferFrameBytes, TransferPath{}, true, first, false, false)
				if err != nil || !firstDisposition.unreliable || len(alternateRoute) != 1 {
					t.Fatalf("fixture did not issue its real datagram: disposition=%+v err=%v", firstDisposition, err)
				}
				sequence.observeCarrierWrite(first, firstDisposition)
				// Carrier consumption frees its local slot, not Transfer flight.
				MessagePoolReturn(<-alternateRoute)
				if sequence.flightController.canSend() != requested || observationCount() != 1 {
					t.Fatalf("requested=%t fixture did not establish the intended flight occupancy", requested)
				}
				generation := sequence.flightController.generation
				selector.updateTransport(h1, []Route{h1Route})
				policy := selector.transferFlightPolicy()
				if policy.h1Only || !policy.limited || !policy.reliableRouteAvailable || policy.generation == generation {
					t.Fatal("fixture did not publish a fresh mixed policy")
				}
				item := &sendItem{
					transferItem: transferItem{messageId: NewId(), sequenceNumber: 2},
					sendTime:     start, ackTimeout: 10 * time.Second, sendCount: 1, expectsAck: true,
				}
				item.transferFrameBytes = marshalSendPackTransferFrame(&sendPackFrame{
					path:      sendTransferPath(sequence.client.ClientId(), DestinationId(sequence.destination)),
					messageId: item.messageId, sequenceId: sequence.sequenceId, sequenceNumber: item.sequenceNumber,
				})
				sequence.sendItems = append(sequence.sendItems, item)
				sequence.addResendItem(item)
				service := sequence.windowPacer.service
				service.next = start.Add(time.Second)
				sequence.windowPacer.rate, sequence.windowPacer.estimateRate = 1000000, 1000000
				sequence.windowPacer.rateUpdated = start
				type result struct {
					disposition transferWriteDisposition
					err         error
				}
				done := make(chan result, 1)
				go func() {
					disposition, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, false, requested)
					done <- result{disposition: disposition, err: err}
				}()
				joined := false
				defer func() {
					sequence.cancel()
					if !joined {
						<-done
					}
					synctest.Wait()
				}()
				synctest.Wait()
				service.stateLock.Lock()
				reservationRetained := service.pacingReservations == 1
				service.stateLock.Unlock()
				if len(done) != 0 || len(h1Route) != 0 || observationCount() != 1 || state.pendingLocalWrites.Load() != 0 ||
					!reservationRetained || sequence.flightController.generation != generation {
					t.Fatalf("transport=%s requested=%t reliable-only H1 bypassed pacing or consumed the outer generation", transportType, requested)
				}
				time.Sleep(time.Second)
				synctest.Wait()
				if len(done) != 1 {
					t.Fatal("reliable-only H1 did not reach its original service deadline")
				}
				got := <-done
				joined = true
				service.stateLock.Lock()
				reservationReleased := service.pacingReservations == 0 && service.reservedByteCount == 0
				service.stateLock.Unlock()
				if got.err != nil || got.disposition.transportType != TransportTypeH1 || !got.disposition.reliable || got.disposition.unreliable ||
					len(h1Route) != 1 || len(alternateRoute) != 0 || observationCount() != 2 || state.pendingLocalWrites.Load() != 0 ||
					!reservationReleased ||
					item.pacingSentAtNanos != start.Add(time.Second).UnixNano() ||
					sequence.flightController.generation != generation || sequence.flightController.messageCount != 1 {
					t.Fatalf("transport=%s requested=%t reliable-only dispatch lost its physical clock or ownership: %+v", transportType, requested, got)
				}
				sequence.observeCarrierWrite(item, got.disposition)
				if item.unreliableCarrierObserved || !item.reliableCarrierObserved || sequence.flightController.messageCount != 1 ||
					!sequence.flightController.applyPolicy(policy) {
					t.Fatal("nested reliable-only dispatch consumed outer transition or unreliable flight")
				}
			})
		}
	}
}

// The exact same test reaches the old estimator through a compatibility
// fallback. Missing dispatch scope is a rate/discovery failure, not a build
// failure; mixed-route window qualification and read-only stats stay unchanged.
func TestWindowPacingWriteEligibilityRefreshKeepsPhysicalHold(t *testing.T) {
	for _, congested := range []bool{false, true} {
		func() {
			sequence, at := newWindowFixedPacingFixture(t)
			service := sequence.windowPacer.service
			if congested {
				at = at.Add(time.Millisecond)
				service.observeReceiverRoundTrip(0, 31*time.Millisecond, 21*time.Millisecond, 10*time.Millisecond, at)
				at = at.Add(time.Millisecond)
				service.observeReceiverRoundTrip(0, 11*time.Millisecond, time.Millisecond, 10*time.Millisecond, at)
			}
			before := sequence.sendWindowEstimate(at)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			selector := NewMultiRouteSelector(ctx, "write-pacing-scope", nil, TransferPath{}, true)
			defer selector.Close()
			selector.updateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{make(Route, 1)})
			selector.updateTransportWithProperties(NewSendGatewayTransportWithType(TransportTypeH3), []Route{make(Route, 1)}, TransferCarrierProperties{Unreliable: true})
			sequence.contractMultiRouteWriter = selector
			if sequence.transferFlightPolicy().h1Only || before.PacingDiscovery == congested {
				t.Fatal("fixture lost mixed policy or physical discovery state")
			}
			_, held := service.pacingHold()
			stats := sequence.sendWindowSnapshot(at)
			if stats.PacingDiscovery || stats.PacingHeldByteRate != 0 || stats.ServiceSized || stats.Window != before.Window {
				t.Fatal("mixed-route statistics inherited H1-only window or pacing scope")
			}
			if _, afterStats := service.pacingHold(); afterStats != held {
				t.Fatal("read-only statistics changed physical pacing retention")
			}
			var after SendWindowEstimate
			if estimator, ok := any(sequence).(interface {
				sendWindowPacingEstimate(time.Time) SendWindowEstimate
			}); ok {
				after = estimator.sendWindowPacingEstimate(at)
			} else {
				after = sequence.sendWindowEstimate(at)
			}
			if after.PacingDiscovery != before.PacingDiscovery || after.PacingHeldByteRate != held || held != before.PacingByteRate ||
				after.PacingByteRate != before.PacingByteRate || after.ServiceSized || after.Window != before.Window ||
				after.WindowRoundTrip != before.WindowRoundTrip || after.LearnedWindow != before.LearnedWindow {
				t.Fatalf("congested=%t dispatch refresh lost physical pacing or expanded mixed window: before=%+v after=%+v", congested, before, after)
			}
		}()
	}
}

// A mixed publication keeps the same queued owner. Cancellation releases
// only that owner; its sibling still pays the original serialization debt.
func TestWindowPacingWriteEligibilityMixedCancellationKeepsFifoDebt(t *testing.T) {
	runWindowRouteGenerationFixture(t, nil, func(t *testing.T, fixture *windowRouteGenerationFixture) {
		service := fixture.sequence.windowPacer.service
		siblingCtx, siblingCancel := context.WithCancel(context.Background())
		defer siblingCancel()
		sibling := windowBurstPacer{service: service, rate: 1000000, estimateRate: 1000000, serviceSequenceId: NewId()}
		defer sibling.close()
		done := make(chan error, 1)
		go func() { done <- sibling.waitForService(siblingCtx, 1280) }()
		joined := false
		defer func() {
			siblingCancel()
			if !joined {
				<-done
			}
			synctest.Wait()
		}()
		synctest.Wait()
		service.stateLock.Lock()
		next, probe := service.next, service.probeSent
		service.stateLock.Unlock()
		fixture.client.RouteManager().UpdateTransportWithProperties(fixture.h3, []Route{fixture.alternate}, TransferCarrierProperties{Unreliable: true})
		synctest.Wait()
		service.stateLock.Lock()
		ownersPreserved := service.pacingReservations == 2 && service.waiterHead == &fixture.sequence.windowPacer.waiter &&
			service.next == next && service.probeSent == probe
		service.stateLock.Unlock()
		if len(fixture.written) != 0 || len(done) != 0 || !ownersPreserved {
			t.Fatal("mixed publication released or repeated a FIFO owner")
		}
		fixture.cancel()
		synctest.Wait()
		service.stateLock.Lock()
		siblingPreserved := service.pacingReservations == 1 && service.waiterHead == &sibling.waiter
		service.stateLock.Unlock()
		if len(done) != 0 || !siblingPreserved {
			t.Fatal("cancellation erased its sibling's physical debt")
		}
		time.Sleep(time.Until(fixture.start.Add(time.Second)))
		synctest.Wait()
		if len(done) != 1 {
			t.Fatal("sibling did not complete its original pacing deadline")
		}
		err := <-done
		joined = true
		service.stateLock.Lock()
		debtPreserved := service.pacingReservations == 0 && service.reservedByteCount == 0 && service.next == next && service.probeSent == probe
		service.stateLock.Unlock()
		if err != nil || !debtPreserved || len(fixture.route) != 0 || len(fixture.alternate) != 0 {
			t.Fatalf("cancellation changed sibling accounting: %v", err)
		}
	})
}

// An inner mixed-H1 selection is still bounded by the already-started writer
// deadline, even though its aggregate h1Only flag must remain false.
func TestWindowPacingWriteEligibilityMixedFirstInnerWriteDeadline(t *testing.T) {
	var deadline time.Time
	runWindowRouteGenerationFixture(t, func(t *testing.T, fixture *windowRouteGenerationFixture) {
		fixture.alternate = make(Route)
		fixture.client.RouteManager().UpdateTransport(fixture.h1, nil)
		fixture.client.RouteManager().UpdateTransport(fixture.h3, []Route{fixture.alternate})
		fixture.sequence.sendBufferSettings.WriteTimeout = 200 * time.Millisecond
		fixture.sequence.sendBufferSettings.afterTransferWriteDeadlineForTest = func(until time.Time) {
			deadline = until
			fixture.client.RouteManager().UpdateTransport(fixture.h3, nil)
			fixture.client.RouteManager().UpdateTransport(fixture.h1, []Route{fixture.route})
			fixture.client.RouteManager().UpdateTransport(fixture.h3, []Route{fixture.alternate})
		}
	}, func(t *testing.T, fixture *windowRouteGenerationFixture) {
		if deadline != fixture.start.Add(200*time.Millisecond) || fixture.sequence.transferFlightPolicy().h1Only {
			t.Fatal("fixture did not publish mixed preferred H1 inside the original writer budget")
		}
		time.Sleep(time.Until(deadline))
		synctest.Wait()
		service := fixture.sequence.windowPacer.service
		if len(fixture.written) != 0 || len(fixture.route) != 0 || fixture.observations != 0 ||
			fixture.providerState.pendingLocalWrites.Load() != 0 || service.pacingReservations != 0 || service.reservedByteCount != 0 {
			t.Fatal("mixed inner H1 pacing ignored the original absolute writer deadline")
		}
	})
}

// Zero permits an immediately ready H1 attempt but no new mixed-policy wait.
// Both rows retain exact ready/refusal accounting at the same virtual instant.
func TestWindowPacingWriteEligibilityMixedZeroBudgetReadyOnly(t *testing.T) {
	for _, ready := range []bool{true, false} {
		var deadline time.Time
		runWindowRouteGenerationFixture(t, func(t *testing.T, fixture *windowRouteGenerationFixture) {
			fixture.alternate = make(Route)
			fixture.client.RouteManager().UpdateTransport(fixture.h1, nil)
			fixture.client.RouteManager().UpdateTransport(fixture.h3, []Route{fixture.alternate})
			fixture.sequence.sendBufferSettings.WriteTimeout = 0
			if ready {
				fixture.sequence.windowPacer.service.next = fixture.start
			}
			fixture.sequence.sendBufferSettings.afterTransferWriteDeadlineForTest = func(until time.Time) {
				deadline = until
				fixture.client.RouteManager().UpdateTransport(fixture.h3, nil)
				fixture.client.RouteManager().UpdateTransport(fixture.h1, []Route{fixture.route})
				fixture.client.RouteManager().UpdateTransport(fixture.h3, []Route{fixture.alternate})
			}
		}, func(t *testing.T, fixture *windowRouteGenerationFixture) {
			want := 0
			if ready {
				want = 1
			}
			service := fixture.sequence.windowPacer.service
			if deadline != fixture.start || time.Now() != fixture.start || fixture.sequence.transferFlightPolicy().h1Only ||
				len(fixture.written) != want || len(fixture.route) != want || len(fixture.alternate) != 0 ||
				fixture.observations != want || service.pacingReservations != 0 || service.reservedByteCount != 0 {
				t.Fatalf("ready=%t mixed H1 zero budget waited, bypassed debt or refused ready dispatch", ready)
			}
			if !ready && !service.next.After(fixture.start.Add(time.Second)) {
				t.Fatal("mixed zero-budget refusal refunded its charged serialization")
			}
		})
	}
}
