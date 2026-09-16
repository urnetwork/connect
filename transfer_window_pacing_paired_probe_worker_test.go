// Exact receiver timing still needs one unretried physical probe through the
// full raw residence. These worker tests force stale lane recovery explicitly.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Creates one actual controlled H1 probe after a real old-tail ACK. The
// sender is durably blocked before every inspected worker-owned state.
func testWindowPacingPairedProbeRecoveryFixture(t *testing.T, configure func(*SendBufferSettings), prepare func(*SendSequence), check func(*testing.T, *SendSequence, *Client, Route, *protocol.Pack, time.Time, <-chan error)) {
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
		service.observeReceiverRoundTrip(0, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, start.Add(-90*time.Millisecond))
		for ago := 8; ago > 0; ago-- {
			service.observeReceiverRoundTrip(uint64(9-ago), 1200*time.Millisecond, 1190*time.Millisecond, 10*time.Millisecond, start.Add(-time.Duration(ago)*10*time.Millisecond))
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

// A real controlled drain is followed by a confirmed initial H1 write. Its
// delayed exact reply can raise the unloaded path baseline only if the stale
// per-lane timer does not turn that same message into two ambiguous copies.
func TestWindowPacingPairedProbeOutlivesStaleLaneTimer(t *testing.T) {
	testWindowPacingPairedProbeDelayedReply(t, false)
}

// A selective exact reply has the same physical timing provenance as a head.
func TestWindowPacingPairedProbeSelectiveReplyOutlivesStaleLaneTimer(t *testing.T) {
	testWindowPacingPairedProbeDelayedReply(t, true)
}

// Separate top-level head and SACK roots preserve both failure-before results.
func testWindowPacingPairedProbeDelayedReply(t *testing.T, selective bool) {
	t.Helper()
	testWindowPacingPairedProbeRecoveryFixture(t, nil, nil, func(t *testing.T, sequence *SendSequence, _ *Client, route Route, pack *protocol.Pack, at time.Time, _ <-chan error) {
		service := sequence.windowPacer.service
		if got := service.roundTripEvidence(at).minimum; got != 300*time.Microsecond {
			t.Fatalf("selective=%t: initial unloaded baseline=%s", selective, got)
		}
		time.Sleep(time.Until(at.Add(1210 * time.Millisecond)))
		synctest.Wait()
		copies := 0
	drainCopies:
		for {
			select {
			case wire := <-route:
				retry := decodeSendPackLifecycleWirePack(t, wire)
				MessagePoolReturn(wire)
				if string(retry.MessageId) != string(pack.MessageId) {
					t.Fatal("unexpected message in single-probe fixture")
				}
				copies++
			default:
				break drainCopies
			}
		}
		delay, compression := uint32(10000), uint32(10000)
		ok, err := sequence.Ack(&protocol.Ack{MessageId: pack.MessageId, SequenceId: pack.SequenceId, Tag: pack.Tag, Selective: selective, ReceiverAckDelayMicros: &delay, AckCompressTimeoutMicros: &compression}, 0)
		if !ok || err != nil {
			t.Fatalf("selective=%t: exact receiver reply refused: %t %v", selective, ok, err)
		}
		synctest.Wait()
		floor := service.roundTripEvidence(time.Now()).minimum
		raw := sequence.rttWindow.Estimate()
		t.Logf("selective=%t same-message-retries=%d adjusted-baseline=%s raw-samples=%d raw-mean=%s", selective, copies, floor, raw.SampleCount, raw.Mean)
		if copies != 0 || floor != 1200*time.Millisecond {
			t.Errorf("selective=%t: stale lane timer destroyed exact probe: copies=%d floor=%s, want unretried 1.2s", selective, copies, floor)
		}
		if raw.SampleCount != 2 || raw.Mean != 605500*time.Microsecond {
			t.Errorf("selective=%t: raw recovery lost full 1.210s residence: %+v", selective, raw)
		}
	})
}

// Optional metadata cannot create an indefinite protected probe. The same
// absolute raw-residence bound obeys both default and shorter configured RTOs.
func TestWindowPacingPairedProbeNoReplyHasFiniteDeadline(t *testing.T) {
	for _, maximum := range []time.Duration{8 * time.Second, 600 * time.Millisecond} {
		testWindowPacingPairedProbeRecoveryFixture(t, func(settings *SendBufferSettings) { settings.MaxResendInterval = maximum }, nil, func(t *testing.T, sequence *SendSequence, _ *Client, route Route, pack *protocol.Pack, at time.Time, _ <-chan error) {
			deadline := min(2400*time.Millisecond, maximum)
			time.Sleep(time.Until(at.Add(deadline - time.Nanosecond)))
			synctest.Wait()
			select {
			case wire := <-route:
				MessagePoolReturn(wire)
				t.Fatalf("maximum=%s: copied probe before absolute raw-residence bound", maximum)
			default:
			}
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			select {
			case wire := <-route:
				retry := decodeSendPackLifecycleWirePack(t, wire)
				MessagePoolReturn(wire)
				if string(retry.MessageId) != string(pack.MessageId) {
					t.Fatal("finite retry changed message identity")
				}
			default:
				t.Fatalf("maximum=%s: absent reply suppressed bounded retry", maximum)
			}
			time.Sleep(time.Millisecond)
			delay := uint32(10000)
			if ok, err := sequence.Ack(&protocol.Ack{MessageId: pack.MessageId, SequenceId: pack.SequenceId, Tag: pack.Tag, ReceiverAckDelayMicros: &delay}, 0); !ok || err != nil {
				t.Fatalf("late ambiguous reply refused: %t %v", ok, err)
			}
			synctest.Wait()
			if got := sequence.windowPacer.service.roundTripEvidence(time.Now()).minimum; got != 300*time.Microsecond {
				t.Fatalf("maximum=%s: ambiguous retry raised unloaded baseline to %s", maximum, got)
			}
		})
	}
}
