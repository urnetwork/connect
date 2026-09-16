// A confirmed physical drain must not be undone by a stale lane recovery clock.
package connect

import (
	"context"
	"math"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Creates one actual controlled H1 probe after a real old-tail ACK. The
// sender is durably blocked before every inspected worker-owned state.
func testWindowPacingProbeRecoveryFixture(t *testing.T, configure func(*SendBufferSettings), prepare func(*SendSequence), check func(*testing.T, *SendSequence, *Client, Route, *protocol.Pack, time.Time, <-chan error)) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		destination := NewId()
		startWorker := make(chan struct{})
		initialWrite := make(chan struct{})
		releaseInitial := make(chan struct{})
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
		settings.SendBufferSettings.ApplyWindowSizing()
		settings.SendBufferSettings.AckTimeout = time.Minute
		if configure != nil {
			configure(settings.SendBufferSettings)
		}
		settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
			if id.Destination == destination && number == 0 {
				close(initialWrite)
				select {
				case <-releaseInitial:
				case <-ctx.Done():
				}
			}
		}
		settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
			if id.Destination == destination {
				select {
				case <-startWorker:
				case <-ctx.Done():
				}
			}
		}
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		route := make(Route, 32)
		defer func() {
			cancel()
			client.CloseAndWait(context.Background())
			for {
				select {
				case bytes := <-route:
					MessagePoolReturn(bytes)
				default:
					return
				}
			}
		}()
		client.ContractManager().AddNoContractPeer(destination)
		client.RouteManager().UpdateTransport(
			&h1SendClientTransportForGroupTest{sendClientTransport: NewSendClientTransport(DestinationId(destination))},
			[]Route{route},
		)
		sequence := client.sendBuffer.createSendSequence(sendSequenceId{Destination: destination}, &SendPack{Destination: destination})
		synctest.Wait()
		service := sequence.windowPacer.service
		start := time.Now()
		service.observeRoundTrip(time.Millisecond, 0, start.Add(-90*time.Millisecond))
		for ago := 8; ago > 0; ago-- {
			service.observeRoundTrip(1200*time.Millisecond, 0, start.Add(-time.Duration(ago)*10*time.Millisecond))
		}
		sequence.rttWindow.closeSendTime(uint64(start.Add(-time.Millisecond).UnixMilli()), start)
		if got := sequence.rttWindow.ScaledRtt(); got != 300*time.Millisecond {
			t.Fatalf("stale lane resend interval=%s, want 300ms", got)
		}
		tailSequence, tailMessage := NewId(), NewId()
		service.sent = 1000
		service.beginWrite(tailSequence, tailMessage, 1, start.Add(-1100*time.Millisecond), false)
		service.finishWrite(tailSequence, tailMessage, true)
		callbacks := make(chan error, 1)
		frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1000)}
		if !client.SendWithTimeout(frame, destination, func(err error) { callbacks <- err }, time.Second) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("probe Pack was not admitted")
		}
		close(startWorker)
		synctest.Wait()
		service.stateLock.Lock()
		drainUntil, waiterHead := service.drainUntil, service.waiterHead
		service.stateLock.Unlock()
		if drainUntil.IsZero() || waiterHead != &sequence.windowPacer.waiter {
			t.Fatal("actual send worker did not wait for its old physical tail")
		}
		time.Sleep(100 * time.Millisecond)
		service.acknowledgeWrite(tailSequence, tailMessage, 1, false, 0, time.Now())
		service.observe(1000, time.Now())
		synctest.Wait()
		<-initialWrite
		var bytes []byte
		select {
		case bytes = <-route:
		default:
			t.Fatal("successful drain did not release the actual probe write")
		}
		pack := decodeSendPackLifecycleWirePack(t, bytes)
		probeId, err := IdFromBytes(pack.MessageId)
		if err != nil {
			t.Fatal(err)
		}
		MessagePoolReturn(bytes)
		probeAt := time.Now()
		probeState := func() windowPacingRoundTripProbe {
			service.stateLock.Lock()
			defer service.stateLock.Unlock()
			return service.roundTripProbe
		}()
		if probeState.messageId != probeId || !probeState.written || !probeState.resetService {
			t.Fatal("released initial write is not the physically confirmed controlled probe")
		}
		if prepare != nil {
			prepare(sequence)
		}
		close(releaseInitial)
		synctest.Wait()
		check(t, sequence, client, route, pack, probeAt, callbacks)
	})
}

// No-reply recovery is delayed once to an absolute deadline, still bounded by
// each configured maximum. An eventual ambiguous ACK cannot refresh the floor.
func TestWindowPacingProbeRecoveryHasFiniteConfiguredDeadline(t *testing.T) {
	for _, maximum := range []time.Duration{8 * time.Second, 600 * time.Millisecond} {
		testWindowPacingProbeRecoveryFixture(t, func(settings *SendBufferSettings) { settings.MaxResendInterval = maximum }, nil, func(t *testing.T, sequence *SendSequence, client *Client, route Route, pack *protocol.Pack, at time.Time, _ <-chan error) {
			service := sequence.windowPacer.service
			delay := min(2400*time.Millisecond, maximum)
			time.Sleep(time.Until(at.Add(delay - time.Nanosecond)))
			synctest.Wait()
			select {
			case bytes := <-route:
				MessagePoolReturn(bytes)
				t.Fatalf("maximum=%s: probe retried before its bounded deadline", maximum)
			default:
			}
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			select {
			case bytes := <-route:
				retry := decodeSendPackLifecycleWirePack(t, bytes)
				MessagePoolReturn(bytes)
				retryId, _ := IdFromBytes(retry.MessageId)
				originalId, _ := IdFromBytes(pack.MessageId)
				if retryId != originalId {
					t.Fatalf("maximum=%s: retry did not preserve the original message", maximum)
				}
			default:
				t.Fatalf("maximum=%s: missing reply postponed recovery past %s", maximum, delay)
			}
			time.Sleep(time.Millisecond)
			acknowledgeSendPackLifecycleWirePack(t, client, sequence.destination, pack)
			synctest.Wait()
			if got := service.roundTrip(); got != time.Millisecond {
				t.Fatalf("maximum=%s: late ambiguous ACK raised floor to %s", maximum, got)
			}
		})
	}
}

// A shorter configured delivery lifetime closes normally; the probe never
// adds lifetime or emits a retry after the item has reached that bound.
func TestWindowPacingProbeRecoveryCannotExtendAckLifetime(t *testing.T) {
	testWindowPacingProbeRecoveryFixture(t, func(settings *SendBufferSettings) { settings.AckTimeout = 500 * time.Millisecond }, nil, func(t *testing.T, sequence *SendSequence, _ *Client, route Route, _ *protocol.Pack, at time.Time, callbacks <-chan error) {
		time.Sleep(time.Until(at.Add(400 * time.Millisecond)))
		synctest.Wait()
		select {
		case err := <-callbacks:
			if err == nil {
				t.Fatal("expired unacknowledged Pack completed successfully")
			}
		default:
			t.Fatal("probe postponed configured ACK timeout")
		}
		select {
		case bytes := <-route:
			MessagePoolReturn(bytes)
			t.Fatal("delivery expiry emitted a probe retry")
		default:
		}
		if !sequence.windowPacer.service.roundTripProbe.sentAt.IsZero() {
			t.Fatal("closed sequence retained a live RTT probe")
		}
	})
}

// Receiver-evidenced recovery and carrier changes are never borrowed silence.
// Their real worker writes still invalidate ambiguous RTT observations.
func TestWindowPacingProbeRecoveryPreservesExplicitRecovery(t *testing.T) {
	for _, reason := range []sendRecoveryKind{sendRecoveryCarrierChange, sendRecoverySelectiveGap, sendRecoveryAckTailProbe, sendRecoveryCumulativeProbe, sendRecoveryContractMissing, sendRecoveryEviction} {
		testWindowPacingProbeRecoveryFixture(t, nil, func(sequence *SendSequence) {
			item := sequence.resendQueue.PeekFirst()
			item.recoveryKind = reason
			if reason == sendRecoveryCarrierChange {
				item.carrierChanged = true
			}
		}, func(t *testing.T, sequence *SendSequence, client *Client, route Route, pack *protocol.Pack, at time.Time, _ <-chan error) {
			// The controlled pause ends 100 ms after local offer; it does not
			// consume any part of the configured 300 ms physical RTO.
			time.Sleep(time.Until(at.Add(300*time.Millisecond - time.Nanosecond)))
			synctest.Wait()
			select {
			case bytes := <-route:
				MessagePoolReturn(bytes)
				t.Fatal("recovery fired before its unchanged 300ms physical interval")
			default:
			}
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			select {
			case bytes := <-route:
				MessagePoolReturn(bytes)
			default:
				t.Fatalf("reason=%d: explicit recovery borrowed the probe deadline", reason)
			}
			time.Sleep(time.Until(at.Add(1200 * time.Millisecond)))
			acknowledgeSendPackLifecycleWirePack(t, client, sequence.destination, pack)
			synctest.Wait()
			if got := sequence.windowPacer.service.roundTrip(); got != time.Millisecond {
				t.Fatalf("reason=%d: ambiguous recovery raised floor to %s", reason, got)
			}
		})
	}
}

// Cancellation consumes the protected probe and its item independently of a
// delayed peer; neither the timer nor a later ACK may recreate its evidence.
func TestWindowPacingProbeRecoveryCancellationConsumesEvidence(t *testing.T) {
	testWindowPacingProbeRecoveryFixture(t, nil, nil, func(t *testing.T, sequence *SendSequence, _ *Client, route Route, pack *protocol.Pack, at time.Time, callbacks <-chan error) {
		time.Sleep(time.Until(at.Add(250 * time.Millisecond)))
		synctest.Wait()
		sequence.Cancel()
		synctest.Wait()
		select {
		case err := <-callbacks:
			if err == nil {
				t.Fatal("canceled Pack completed successfully")
			}
		default:
			t.Fatal("probe cancellation failed to complete the Pack")
		}
		service := sequence.windowPacer.service
		if !service.roundTripProbe.sentAt.IsZero() {
			t.Fatal("canceled sequence retained a probe")
		}
		id, _ := IdFromBytes(pack.MessageId)
		time.Sleep(time.Until(at.Add(1200 * time.Millisecond)))
		service.acknowledgeWrite(sequence.sequenceId, id, pack.SequenceNumber, false, 0, time.Now())
		if got := service.roundTrip(); got != time.Millisecond {
			t.Fatalf("late canceled ACK raised floor to %s", got)
		}
		select {
		case bytes := <-route:
			MessagePoolReturn(bytes)
			t.Fatal("canceled probe wrote recovery")
		default:
		}
	})
}

// Identity and dispatch-time evidence bound protection. Later sibling samples
// cannot extend it; read-only or unrelated queries cannot spend or create it.
func TestWindowPacingProbeRecoveryMatchesOnlyItsLiveEvidence(t *testing.T) {
	start := time.Unix(1000, 0)
	sequence, message := NewId(), NewId()
	for _, state := range []string{"live", "other-message", "other-sequence", "unconfirmed", "natural", "acknowledged", "invalidated"} {
		service := &windowPacingService{roundTripProbe: windowPacingRoundTripProbe{sequenceId: sequence, messageId: message, sentAt: start, written: true, resetService: true, observedRoundTrip: 1200 * time.Millisecond}}
		querySequence, queryMessage := sequence, message
		switch state {
		case "other-message":
			queryMessage = NewId()
		case "other-sequence":
			querySequence = NewId()
		case "unconfirmed":
			service.roundTripProbe.written = false
		case "natural":
			service.roundTripProbe.resetService = false
		case "acknowledged":
			service.roundTripProbe.ackedAt = start.Add(time.Second)
		case "invalidated":
			service.invalidateMessageProbe(sequence, message)
		}
		got := service.probeRecoveryDeadline(querySequence, queryMessage, 2, 8*time.Second)
		if state != "live" {
			if !got.IsZero() {
				t.Fatalf("%s borrowed another live probe: %s", state, got)
			}
			continue
		}
		want := start.Add(2400 * time.Millisecond)
		if got != want {
			t.Fatalf("live probe deadline=%s, want=%s", got, want)
		}
		service.observeRoundTrip(time.Minute, 0, start.Add(time.Second))
		if again := service.probeRecoveryDeadline(sequence, message, 2, 8*time.Second); again != want {
			t.Fatalf("later shared residence extended the absolute deadline: %s", again)
		}
		service.roundTripProbe.observedRoundTrip = time.Duration(math.MaxInt64)
		if got := service.probeRecoveryDeadline(sequence, message, math.MaxFloat32, 8*time.Second); got != start.Add(8*time.Second) {
			t.Fatalf("large residence overflowed configured recovery maximum: %s", got)
		}
		if got := service.probeRecoveryDeadline(sequence, message, 2, 0); !got.IsZero() {
			t.Fatal("nonpositive maximum granted a probe wait")
		}
	}
}

// The configured drain bound may be smaller than one sample interval or near
// the duration limit. Neither doubling residence nor cooldown may wrap time.
func TestWindowPacingDrainBoundsAndCooldownCannotOverflow(t *testing.T) {
	for _, maximum := range []time.Duration{time.Nanosecond, 19 * time.Millisecond, 2 * time.Second, time.Duration(math.MaxInt64)} {
		service := &windowPacingService{drainMaximumTime: maximum, minRoundTrip: time.Millisecond}
		start := time.Unix(1000, 0)
		sequence, message := NewId(), NewId()
		service.beginWrite(sequence, message, 1, start, false)
		service.finishWrite(sequence, message, true)
		service.roundTripStats.ring = newWindowBucketStats(deliverySizedWindowSampleInterval, 4)
		service.roundTripStats.add(1, float64(math.MaxInt64), start.Add(-deliverySizedWindowSampleInterval))
		waiter := &windowPacingWaiter{}
		got, _ := service.admitBurst(start, 1000, false, waiter)
		if got != maximum {
			t.Fatalf("maximum=%s: drain bound=%s", maximum, got)
		}
		if !service.drainCheckAt.After(service.drainUntil) && maximum < time.Duration(math.MaxInt64) {
			t.Fatalf("maximum=%s: cooldown wrapped before expiry", maximum)
		}
		if service.drainCheckAt.Before(start) {
			t.Fatalf("maximum=%s: cooldown wrapped into the past", maximum)
		}
	}
}
