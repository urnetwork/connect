// The real sender's admitted window and physical offer span can be identical
// for two receiver serialization schedules hidden by reverse queue variation.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Capture only deterministic sender observations. Generated message identities
// are unrelated to the two causal schedules and do not enter the comparison.
type receiverIngressReverseCompressedEvidence struct {
	initialWrites    int
	initialWireBytes int
	firstWireBytes   int
	initialSpan      time.Duration
	nextOfferAt      time.Duration
	window           ByteCount
	windowBytes      ByteCount
	serviceRate      ByteCount
}

// A saturated production send worker dispatches one peer window, parks with
// accepted demand, then reopens precisely when the first exact reply arrives.
// Both hidden receiver paths reuse this same actual sender observation.
func checkWindowPacingReverseCompression(t *testing.T, receiverDelay uint32) {
	t.Helper()
	assertMessagePoolOwnership(t)
	defer startIngressCounterDiagnostic(t)()
	for _, hiddenRate := range []ByteCount{12500000, 1250000} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			destination := NewId()
			startWorker := make(chan struct{})
			// The physical writer publishes a bounded timestamp record before it parks.
			type writeEvent struct {
				number uint64
				at     time.Time
			}
			writes := make(chan writeEvent, 128)
			settings := DefaultClientSettings()
			settings.Log = NewNoopLogger()
			settings.EncryptionSettings.Mode = EncryptionModeOff
			settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
			settings.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
			settings.SendBufferSettings.ApplyWindowSizing()
			settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
				if id.Destination == destination {
					select {
					case <-startWorker:
					case <-ctx.Done():
					}
				}
			}
			settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
				if id.Destination == destination {
					writes <- writeEvent{number: number, at: time.Now()}
				}
			}
			client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
			route := make(Route, 128)
			var producerDone chan struct{}
			defer func() {
				cancel()
				client.CloseAndWait(context.Background())
				if producerDone != nil {
					<-producerDone
				}
				for {
					select {
					case wire := <-route:
						MessagePoolReturn(wire)
					default:
						return
					}
				}
			}()
			client.ContractManager().AddNoContractPeer(destination)
			client.RouteManager().UpdateTransport(&h1SendClientTransportForGroupTest{sendClientTransport: NewSendClientTransport(DestinationId(destination))}, []Route{route})
			sequence := client.sendBuffer.createSendSequence(sendSequenceId{Destination: destination}, &SendPack{Destination: destination})
			synctest.Wait()
			sequence.observeReceiveWindowAdvertisement(receiveAckMessage{receiveWindowSet: true, receiveWindowByteCount: 64 * 1024, ackCompressTimeoutSet: true, ackCompressTimeoutMicros: 50000})
			service := sequence.windowPacer.service
			start := time.Now()
			for i := 0; i <= 8; i++ {
				at := start.Add(time.Duration(i-8) * 50 * time.Millisecond)
				service.sent += 625000
				service.observeReceiverRoundTrip(0, 150*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond, at)
				service.observe(625000, at)
			}
			if rate, _, _ := service.measured(time.Second, start); rate != 12500000 {
				t.Fatalf("missing prior serialization: %d", rate)
			}
			firstOffer := make(chan struct{})
			producerDone = make(chan struct{})
			go func() {
				defer close(producerDone)
				for i := range 64 {
					frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(2048)}
					if !client.SendWithTimeout(frame, destination, func(error) {}, -1) {
						MessagePoolReturn(frame.MessageBytes)
						if ctx.Err() == nil {
							t.Error("sustained offer refused")
						}
						return
					}
					if i == 0 {
						close(firstOffer)
					}
				}
			}()
			<-firstOffer
			close(startWorker)
			synctest.Wait()
			evidence := receiverIngressReverseCompressedEvidence{window: sequence.sendWindowSnapshot(time.Now()).Window}
			_, evidence.windowBytes = sequence.resendQueue.QueueSize()
			if !sequence.resendCapacityUnavailable.Load() || sequence.resendQueue.CanAdd(0, evidence.window) {
				t.Fatal("actual worker did not park at the full peer window")
			}
			var initial []*protocol.Pack
			var initialWireByteCounts []ByteCount
			var firstAt, lastAt time.Time
		drainInitial:
			for {
				select {
				case wire := <-route:
					pack := decodeSendPackLifecycleWirePack(t, wire)
					if len(initial) == 0 {
						evidence.firstWireBytes = len(wire)
					}
					evidence.initialWireBytes += len(wire)
					initial = append(initial, pack)
					initialWireByteCounts = append(initialWireByteCounts, ByteCount(len(wire)))
					MessagePoolReturn(wire)
					event := <-writes
					if event.number != pack.SequenceNumber {
						t.Fatal("physical offer log changed order")
					}
					if firstAt.IsZero() {
						firstAt = event.at
					}
					lastAt = event.at
				default:
					break drainInitial
				}
			}
			evidence.initialWrites = len(initial)
			evidence.initialSpan = lastAt.Sub(firstAt)
			if len(initial) < 2 || len(initial) >= 64 || evidence.initialSpan >= 40*time.Millisecond || evidence.window != 64*1024 {
				t.Fatalf("unexpected finite physical flight: %+v", evidence)
			}
			ack := func(pack *protocol.Pack, delay uint32) {
				compression, capacity := uint32(50000), uint64(64*1024)
				if ok, err := sequence.Ack(&protocol.Ack{MessageId: pack.MessageId, SequenceId: pack.SequenceId, Tag: pack.Tag, ReceiverAckDelayMicros: &delay, AckCompressTimeoutMicros: &compression, ReceiveWindowByteCount: &capacity}, 0); !ok || err != nil {
					t.Fatalf("exact reply refused: %t %v", ok, err)
				}
				synctest.Wait()
			}
			var tailPrefix ByteCount
			for i, pack := range initial {
				if i > 0 {
					tailPrefix += initialWireByteCounts[i]
				}
				elapsed := time.Duration(float64(tailPrefix) * float64(time.Second) / float64(hiddenRate))
				time.Sleep(time.Until(start.Add(50*time.Millisecond + elapsed)))
				recordIngressCounterDiagnostic(destination, client.ClientId(), pack, sequenceTlsRoleServer, false, TransportTypeH1, initialWireByteCounts[i], time.Now())
			}
			time.Sleep(time.Until(start.Add(160213840 * time.Nanosecond)))
			ack(initial[0], 0)
			time.Sleep(time.Until(start.Add(160214840*time.Nanosecond + time.Duration(receiverDelay)*time.Microsecond)))
			ack(initial[len(initial)-1], receiverDelay)
			rate, _, latest := service.measure(time.Second, time.Now(), false)
			evidence.serviceRate = max(rate, latest)
			if evidence.serviceRate > 13750000 {
				t.Errorf("reverse ACK compression fabricated service capacity: %d B/s", evidence.serviceRate)
			}
			firstCorrected := 160213840 * time.Nanosecond
			tailCorrected := 160214840*time.Nanosecond - 0*time.Millisecond
			if tailCorrected-firstCorrected != time.Microsecond {
				t.Fatal("reverse compression no longer spans exactly one microsecond")
			}
			// Every original Pack was offered before 40 ms on either path.
			// The tail's return queue changes so sender arrival and delay match.
			span := time.Duration(float64(evidence.initialWireBytes-evidence.firstWireBytes) * float64(time.Second) / float64(hiddenRate))
			firstReverse := 160213840*time.Nanosecond - 50*time.Millisecond
			tailReverse := 160214840*time.Nanosecond - 50*time.Millisecond - span
			if firstReverse < 50*time.Millisecond || tailReverse < 50*time.Millisecond {
				t.Fatalf("hidden forward schedule cannot meet identical ACKs: %s", tailReverse)
			}
			t.Logf("hidden-rate=%d receiver-span-nanos=%d first-reverse-nanos=%d tail-reverse-nanos=%d writes=%d wire-bytes=%d first-wire-bytes=%d offer-span-nanos=%d next-offer-nanos=%d window=%d held-window-bytes=%d sender-service-rate=%d", hiddenRate, span, firstReverse, tailReverse, evidence.initialWrites, evidence.initialWireBytes, evidence.firstWireBytes, evidence.initialSpan, evidence.nextOfferAt, evidence.window, evidence.windowBytes, evidence.serviceRate)

		})
	}
}

// A queued reverse lane can compress two valid, unambiguous ACKs without
// changing the receiver's actual first-delivery serialization interval.
func TestWindowPacingReverseCompressionDoesNotCreateService(t *testing.T) {
	checkWindowPacingReverseCompression(t, 0)
}

// Subtracting a positive measured wait leaves the same reverse-path alias.
func TestWindowPacingReverseCompressionWithMeasuredDelayDoesNotCreateService(t *testing.T) {
	checkWindowPacingReverseCompression(t, 50000)
}
