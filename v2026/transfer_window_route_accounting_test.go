// Concrete selector accounting preserves one dispatch across bounded wakes.
package connect

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"
)

// Reuse the production lifetime/pool fixture, replacing only its custom writer
// with the real selector and a ready cached H1 pacing envelope.
func newWindowRouteAccountingFixture(t *testing.T, capacity int) (*SendSequence, *sendItem, *MultiRouteSelector, Route, *int, *providerEvaluationState) {
	t.Helper()
	sequence, item, _, observations := newRecoveryAccountingFixture(t)
	sequence.sendBufferSettings.DeliverySizedWindowScale = 2
	selector := NewMultiRouteSelector(sequence.ctx, "route-accounting", nil, DestinationId(sequence.destination), true)
	route := make(Route, capacity)
	selector.updateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{route})
	sequence.contractMultiRouteWriter = selector
	sequence.windowPacer = windowBurstPacer{
		service: &windowPacingService{next: time.Now()}, serviceSequenceId: sequence.sequenceId,
		rate: 1000000000, estimateRate: 1000000000, rateUpdated: time.Now(),
	}
	state := &providerEvaluationState{}
	sequence.sendBufferSettings.providerEvaluation = &providerEvaluationAttempt{
		owner: state, destinationId: sequence.destination, observeLocalWrite: true,
	}
	t.Cleanup(func() {
		sequence.windowPacer.close()
		selector.Close()
		for len(route) > 0 {
			MessagePoolReturn(<-route)
		}
	})
	return sequence, item, selector, route, observations, state
}

// One standalone retry owns completion independently of its normal result;
// a test callback's goroutine exit must still release the joining test owner.
func runWindowRouteAccountingRetry(sequence *SendSequence, item *sendItem, done chan<- error, finished chan<- struct{}) {
	defer close(finished)
	_, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, true, false)
	done <- err
}

// Once a real selector timed out under an older lifetime, a later ACK at its
// continuation cannot turn that observed rejection into an unissued retry.
func TestWindowPacingRouteGenerationConcreteLifetimeAckKeepsObservedFailure(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence, item, _, route, observations, state := newWindowRouteAccountingFixture(t, 0)
		start := time.Now()
		older := &sendItem{transferItem: transferItem{messageId: NewId()},
			sendTime: start, ackTimeout: 10 * time.Millisecond, expectsAck: true}
		sequence.addResendItem(older)
		ackPublished := false
		sequence.beforeContractWriterAccessForTest = func(publish bool) {
			if publish || ackPublished || older.ackLifetimeIndex != 0 || *observations != 1 {
				return
			}
			if time.Now() != start.Add(older.ackTimeout) {
				t.Error("continuation did not follow the exact rejected lifetime-bounded dispatch")
				sequence.cancel()
				return
			}
			ackPublished = true
			sequence.ackWindow.Update(sequenceAck{sequenceNumber: item.sequenceNumber, messageId: item.messageId})
		}
		done := make(chan error, 1)
		finished := make(chan struct{})
		go runWindowRouteAccountingRetry(sequence, item, done, finished)
		defer func() {
			sequence.cancel()
			<-finished
		}()
		synctest.Wait()
		if *observations != 1 || state.pendingLocalWrites.Load() != 1 || len(done) != 0 {
			t.Fatal("fixture did not reach its first committed concrete selector wait")
		}
		time.Sleep(9 * time.Millisecond)
		sequence.ackWindow.Update(sequenceAck{messageId: older.messageId})
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if !ackPublished || len(done) != 1 {
			t.Fatal("fixture did not reach the renewed-lifetime continuation")
		}
		err := <-done
		if !errors.Is(err, errTransferRouteWriteTimeout) || *observations != 1 || len(route) != 0 ||
			state.pendingLocalWrites.Load() != 0 || !state.localWriteFailed.Load() || state.applicationWriteAdmitted.Load() ||
			!sequence.ackWindow.pendingDeliveryFor(item.sequenceNumber, item.messageId) {
			t.Fatalf("late ACK erased actual rejected dispatch: err=%v observations=%d", err, *observations)
		}
	})
}

// A callback may exit its worker before a normal result, but only the original
// retained frame exists at this pre-share boundary; cleanup still joins it.
func TestWindowPacingRouteGenerationFatalPreflightPublishesWorkerCompletion(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence, item, _, route, observations, state := newWindowRouteAccountingFixture(t, 0)
		start := time.Now()
		if pooled, shared := MessagePoolCheck(item.transferFrameBytes); !pooled || shared {
			t.Fatal("fixture did not retain one unshared frame before worker start")
		}
		type preflight struct {
			at     time.Time
			pooled bool
			shared bool
		}
		entered := make(chan preflight, 1)
		sequence.beforeContractWriterAccessForTest = func(publish bool) {
			if publish {
				return
			}
			pooled, shared := MessagePoolCheck(item.transferFrameBytes)
			entered <- preflight{at: time.Now(), pooled: pooled, shared: shared}
			// This is Fatal's worker-exit mechanism without failing the control.
			// The first policy read precedes the write's consumer share.
			runtime.Goexit()
		}
		done := make(chan error, 1)
		finished := make(chan struct{})
		workerFinished := make(chan struct{})
		go func() {
			// Independent ownership lets the missing-defer mutant fail cleanly.
			defer close(workerFinished)
			runWindowRouteAccountingRetry(sequence, item, done, finished)
		}()
		defer func() {
			sequence.cancel()
			<-workerFinished
		}()
		synctest.Wait()
		if len(entered) != 1 {
			t.Fatal("actual retry did not reach its pre-share policy boundary")
		}
		boundary := <-entered
		select {
		case <-workerFinished:
		default:
			t.Fatal("preflight callback did not terminate its actual worker")
		}
		select {
		case <-finished:
		default:
			t.Fatal("abnormal worker exit failed to publish completion")
		}
		service := sequence.windowPacer.service
		service.stateLock.Lock()
		unreserved := service.pacingReservations == 0 && service.reservedByteCount == 0 && service.waiterHead == nil
		service.stateLock.Unlock()
		pooled, shared := MessagePoolCheck(item.transferFrameBytes)
		if boundary.at != start || !boundary.pooled || boundary.shared || !pooled || shared || time.Now() != start ||
			len(done) != 0 || len(route) != 0 || *observations != 0 || state.pendingLocalWrites.Load() != 0 || !unreserved {
			t.Fatal("abnormal pre-share exit fabricated a result, dispatch, reservation or consumer share")
		}
	})
}

// The generation check precedes observation, but a matched dispatch still
// starts timing before a synchronous ACK and confirms the exact carrier once.
func TestWindowPacingRouteGenerationSynchronousAckKeepsDispatch(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence, item, _, route, observations, state := newWindowRouteAccountingFixture(t, 1)
		sequence.sendBufferSettings.TransferWireMessageObserver = func(TransferWireMessageObservation) {
			*observations++
			if item.rttState != sendItemRttWritePending {
				t.Fatal("dispatch observer preceded timing enrollment")
			}
			sequence.observeReceiverAckRtt(receiveAckMessage{
				messageId:           item.messageId,
				tag:                 sequenceTag{set: true, sendTime: uint64(item.sendTime.UnixMilli())},
				receiverAckDelaySet: true, ackCompressTimeoutSet: true,
				receivedAtNanos: item.pacingSentAtNanos + int64(time.Millisecond),
			})
			sequence.ackWindow.Update(sequenceAck{sequenceNumber: item.sequenceNumber, messageId: item.messageId})
		}
		disposition, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, false, false)
		estimate := sequence.rttWindow.Estimate()
		if err != nil || disposition.transportType != TransportTypeH1 || len(route) != 1 || *observations != 1 ||
			item.rttState != sendItemRttObserved || estimate.SampleCount != 1 || estimate.Mean != time.Millisecond ||
			state.pendingLocalWrites.Load() != 0 || state.localWriteFailed.Load() || !state.applicationWriteAdmitted.Load() {
			t.Fatalf("synchronous ACK lost committed dispatch or exact timing: err=%v estimate=%+v", err, estimate)
		}
	})
}

// Opaque writers intentionally retain legacy pacing. They cannot promise the
// acquired-generation contract merely by exposing a current policy snapshot.
func TestWindowPacingRouteGenerationCustomWriterKeepsLegacyWait(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence, item, writer, observations := newRecoveryAccountingFixture(t)
		sequence.sendBufferSettings.DeliverySizedWindowScale = 2
		writer.policy = transferFlightPolicySnapshot{generation: 1, h1Only: true, reliableRouteAvailable: true}
		start := time.Now()
		sequence.windowPacer = windowBurstPacer{service: &windowPacingService{next: start.Add(time.Second)},
			serviceSequenceId: sequence.sequenceId, rate: 1000000, estimateRate: 1000000, rateUpdated: start}
		defer sequence.windowPacer.close()
		inspectionDone := make(chan struct{})
		sequence.windowPacer.afterWaitForTest = func() {
			if time.Now() != start.Add(time.Second) {
				t.Error("legacy writer woke before or after its original pacing deadline")
			}
			// Publish the negative inspection to the later opaque writer.
			// The channel is already closed at the unchanged one-second wake.
			select {
			case <-sequence.ctx.Done():
			case <-inspectionDone:
			}
		}
		done := make(chan error, 1)
		go func() {
			_, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, false, false)
			done <- err
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
		writer.policy = transferFlightPolicySnapshot{generation: 2, reliableRouteAvailable: true}
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		if writer.writes != 0 || *observations != 0 || len(done) != 0 {
			t.Fatal("opaque writer was treated as generation protected")
		}
		close(inspectionDone)
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		if len(done) != 1 {
			t.Fatal("legacy writer lost its original pacing deadline")
		}
		err := <-done
		joined = true
		if err != nil || writer.writes != 1 || *observations != 1 {
			t.Fatalf("legacy writer behavior changed: err=%v writes=%d observations=%d", err, writer.writes, *observations)
		}
	})
}

// Cancellation joins the actual writer while its inspection channel remains
// unclosed; pooled buffers and the retained service outlive that join.
func TestWindowPacingRouteGenerationCustomWriterCancellationJoinsHeldWait(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence, item, writer, observations := newRecoveryAccountingFixture(t)
		sequence.sendBufferSettings.DeliverySizedWindowScale = 2
		writer.policy = transferFlightPolicySnapshot{generation: 1, h1Only: true, reliableRouteAvailable: true}
		start := time.Now()
		service := &windowPacingService{next: start.Add(time.Second)}
		sequence.windowPacer = windowBurstPacer{service: service, serviceSequenceId: sequence.sequenceId,
			rate: 1000000, estimateRate: 1000000, rateUpdated: start}
		defer sequence.windowPacer.close()
		inspectionDone := make(chan struct{})
		timerWoke := make(chan time.Time, 1)
		sequence.windowPacer.afterWaitForTest = func() {
			timerWoke <- time.Now()
			select {
			case <-sequence.ctx.Done():
			case <-inspectionDone:
			}
		}
		done := make(chan error, 1)
		go func() {
			_, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, false, false)
			done <- err
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
		time.Sleep(time.Second)
		synctest.Wait()
		if len(timerWoke) != 1 || len(done) != 0 || writer.writes != 0 || *observations != 0 {
			t.Fatal("fixture did not hold its actual writer at the original service wake")
		}
		if at := <-timerWoke; at != start.Add(time.Second) {
			t.Fatalf("held timer wake=%s, want 1s", at.Sub(start))
		}
		service.stateLock.Lock()
		next, probe, sent := service.next, service.probeSent, service.sent
		retained := service.pacingReservations == 1 && service.reservedByteCount > 0 && service.waiterHead == &sequence.windowPacer.waiter
		service.stateLock.Unlock()
		if !retained {
			t.Fatal("held writer did not retain its exact reservation")
		}
		// inspectionDone is deliberately never closed. Only cancellation
		// releases the hook, and completion precedes all resource cleanup.
		sequence.cancel()
		err := <-done
		joined = true
		service.stateLock.Lock()
		released := service.pacingReservations == 0 && service.reservedByteCount == 0 && service.waiterHead == nil
		debtPreserved := service.next == next && service.probeSent == probe && service.sent == sent
		service.stateLock.Unlock()
		if !errors.Is(err, context.Canceled) || writer.writes != 0 || *observations != 0 || !released || !debtPreserved {
			t.Fatalf("cancelled held writer lost completion, reservation or dispatch ownership: err=%v writes=%d observations=%d", err, writer.writes, *observations)
		}
	})
}

// The exact same source runs on the hook-only preimage and successor. Root
// compares actual compiler counts; logging alone does not qualify a regression.
func TestWindowPacingRouteGenerationSendReadyAllocationCensus(t *testing.T) {
	sequence, item, _, route, _, _ := newWindowRouteAccountingFixture(t, 1)
	sequence.sendBufferSettings.TransferWireMessageObserver = nil
	sequence.sendBufferSettings.providerEvaluation = nil
	service := sequence.windowPacer.service
	allocations := testing.AllocsPerRun(1000, func() {
		// The direct fixture has no worker. Reuse only its allocated map and
		// waiter wakeup; each sample starts with no outstanding pacing/RTT
		// owner. This measures ready dispatch allocation, not throughput.
		writes, ready := service.writes, sequence.windowPacer.waiter.ready
		if service.pendingWrites != 0 || len(writes) != 0 || service.pacingReservations != 0 || service.reservedByteCount != 0 {
			panic("prior sample retained pacing ownership")
		}
		*service = windowPacingService{next: time.Now(), writes: writes}
		sequence.windowPacer = windowBurstPacer{
			service: service, serviceSequenceId: sequence.sequenceId,
			rate: 1000000000000, estimateRate: 1000000000000, rateUpdated: time.Now(),
			waiter: windowPacingWaiter{ready: ready},
		}
		sequence.pendingReceiverRtt = pendingReceiverRtt{}
		item.pacingByteCount, item.pacingBurst, item.pacingSentAtNanos, item.rttState, item.rttH1 = 0, 0, 0, sendItemRttUnavailable, false
		_, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, false, false)
		if err != nil {
			panic("ready SendSequence dispatch failed")
		}
		MessagePoolReturn(<-route)
		if sequence.windowPacer.timer != nil || item.rttState != sendItemRttWriteConfirmed || service.pendingWrites != 1 {
			panic("sample was not one ready original write")
		}
		sequence.windowPacer.close()
	})
	t.Logf("ready_send_allocations=%g shared_settings_bytes=%d sequence_bytes=%d pacer_bytes=%d",
		allocations, unsafe.Sizeof(SendBufferSettings{}), unsafe.Sizeof(SendSequence{}), unsafe.Sizeof(windowBurstPacer{}))
}

// Cancellation and an already accepted delivery remain higher priority than
// unlimited writer budgets. Neither grants a hidden pacing or route attempt.
func TestWindowPacingRouteGenerationUnlimitedBudgetKeepsPreflightPriority(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, acknowledged := range []bool{false, true} {
		func() {
			sequence, item, _, route, observations, _ := newWindowRouteAccountingFixture(t, 1)
			sequence.sendBufferSettings.WriteTimeout = -1
			want := error(context.Canceled)
			if acknowledged {
				sequence.ackWindow.Update(sequenceAck{sequenceNumber: item.sequenceNumber, messageId: item.messageId})
				want = errWindowPacingAcknowledged
			} else {
				sequence.cancel()
			}
			_, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, true, false)
			if !errors.Is(err, want) || len(route) != 0 || *observations != 0 {
				t.Fatalf("acknowledged=%t preflight priority lost: err=%v observations=%d", acknowledged, err, *observations)
			}
		}()
	}
}

// A zero budget cannot hide a FIFO, meter or drain park after its route
// changes. Each row identifies the exact existing ownership it must retain.
func TestWindowPacingRouteGenerationZeroBudgetCannotPark(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, boundary := range []string{"fifo", "meter", "drain"} {
		synctest.Test(t, func(t *testing.T) {
			sequence, item, selector, route, observations, _ := newWindowRouteAccountingFixture(t, 1)
			start := time.Now()
			h3 := NewSendGatewayTransportWithType(TransportTypeH3)
			selector.mutex.Lock()
			var initialTransport Transport
			for transport := range selector.transportRoutes {
				initialTransport = transport
			}
			initialCount := len(selector.transportRoutes)
			selector.mutex.Unlock()
			if initialCount != 1 {
				t.Fatal("fixture did not start with one known H1 transport")
			}
			selector.updateTransport(initialTransport, nil)
			selector.updateTransport(h3, []Route{route})
			sequence.sendBufferSettings.WriteTimeout = 0
			sequence.windowPacer.rate, sequence.windowPacer.estimateRate = 1000000, 1000000
			service := sequence.windowPacer.service
			var blocker windowPacingWaiter
			sibling := NewId()
			switch boundary {
			case "fifo":
				service.reserve(start, 1, 1000000, 1000000, 0, 0, false, &blocker)
				t.Cleanup(func() {
					service.stateLock.Lock()
					service.removeWaiterWithLock(&blocker)
					service.pacingReservations--
					service.reservedByteCount--
					service.sent--
					service.stateLock.Unlock()
				})
			case "meter":
				service.burstMeter = windowPacingBurstMeter{at: start, spent: 10000, limit: 10000, rate: 1000000}
			case "drain":
				service.stateLock.Lock()
				service.beginWriteWithLock(sibling, NewId(), 0, start, false)
				service.drainStartedAt, service.drainUntil = start, start.Add(40*time.Millisecond)
				service.drainWake = make(chan struct{}, 1)
				service.stateLock.Unlock()
				t.Cleanup(func() {
					service.stateLock.Lock()
					delete(service.writes, sibling)
					service.pendingWrites--
					service.stateLock.Unlock()
				})
			}
			beforeReservations, beforeReserved, beforePending := service.pacingReservations, service.reservedByteCount, service.pendingWrites
			sequence.sendBufferSettings.afterTransferWriteDeadlineForTest = func(until time.Time) {
				if until != start || time.Now() != start || *observations != 0 {
					t.Fatal("fixture did not reach its unobserved zero writer budget")
				}
				// This small frame fits the current ten-millisecond allowance;
				// its burst starts now even after the one-byte FIFO reservation.
				// Thus serialization itself cannot explain any rejected row.
				if len(item.transferFrameBytes) >= 9999 || service.burst.bytes > 1 ||
					!service.burst.start.IsZero() && service.burst.start != start ||
					service.next.Before(start) || service.next.After(start.Add(time.Microsecond)) {
					t.Fatal("zero-budget boundary fixture was already serialization-blocked")
				}
				switch boundary {
				case "fifo":
					if service.waiterHead != &blocker || service.pacingReservations != 1 || service.reservedByteCount != 1 ||
						service.pendingWrites != 0 || !service.drainUntil.IsZero() || service.burstMeter.available < float64(len(item.transferFrameBytes)) {
						t.Fatal("FIFO row did not isolate the older reservation from meter/drain waits")
					}
				case "meter":
					if service.waiterHead != nil || service.pendingWrites != 0 || !service.drainUntil.IsZero() ||
						service.burstMeter.at != start || service.burstMeter.available != 0 || service.burstMeter.spent != 10000 {
						t.Fatal("meter row did not isolate exhausted release credit")
					}
				case "drain":
					if service.waiterHead != nil || service.pendingWrites != 1 || !service.writes[sibling].unambiguous ||
						!service.drainUntil.After(start) || !service.burstMeter.at.IsZero() || service.unprovableWriteCount != 0 {
						t.Fatal("drain row did not isolate a live older write and ready meter")
					}
				}
				selector.updateTransport(h3, nil)
				selector.updateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{route})
			}
			done := make(chan error, 1)
			go func() {
				_, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, false, false)
				done <- err
			}()
			synctest.Wait()
			if len(done) != 1 {
				sequence.cancel()
				synctest.Wait()
				if len(done) == 1 {
					<-done
				}
				t.Fatalf("%s zero budget actually parked", boundary)
			}
			err := <-done
			if !errors.Is(err, errTransferRouteWriteTimeout) || time.Now() != start || len(route) != 0 || *observations != 0 ||
				sequence.windowPacer.timer != nil || service.pacingReservations != beforeReservations ||
				service.reservedByteCount != beforeReserved || service.pendingWrites != beforePending {
				t.Fatalf("%s zero budget parked, observed or changed another owner: err=%v", boundary, err)
			}
		})
	}
}
