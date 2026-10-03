// Real Client workers read from a carrier whose reader paused while a fixed
// upstream FIFO filled. Arrival timestamps are assertions, not estimator input.
package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A bounded numerical record separates physical arrivals from Client reads.
type receiverCarrierBufferEvidence struct {
	physicalRate     ByteCount
	priorRate        ByteCount
	serviceRate      ByteCount
	pacingRate       ByteCount
	initialBurst     ByteCount
	resultBurst      ByteCount
	physicalBytes    int
	physicalSpan     time.Duration
	offerSpan        time.Duration
	receiveSpan      time.Duration
	maxQueued        int
	maxCarrierQueued int
	ackCount         int
	pairs            int
	logicalCredit    ByteCount
	physicalCopies   int
	applicationByte  int64
}

// A stalled carrier reader can compress its own observation clock even while
// the downstream Client route stays empty. This queue is before Client ingress.
func TestWindowReceiverCarrierBufferedReadsDoNotInflateServiceBurst(t *testing.T) {
	evidence := runReceiverCarrierBufferWorker(t, true, 12500000, 12500000, 0)
	if evidence.serviceRate > evidence.physicalRate*101/100 || evidence.resultBurst > evidence.initialBurst*101/100 {
		t.Fatalf("Buffered carrier reads inflated measured service/burst: %+v", evidence)
	}
}

// The same two Clients with no consumption stall retain the physical capacity.
func TestWindowReceiverCarrierUnqueuedTrainPreservesCapacity(t *testing.T) {
	evidence := runReceiverCarrierBufferWorker(t, false, 12500000, 12500000, 0)
	if evidence.serviceRate < evidence.physicalRate*99/100 || evidence.serviceRate > evidence.physicalRate*101/100 {
		t.Fatalf("unqueued physical train changed its capacity: %+v", evidence)
	}
}

// A faster serializer gives real evidence while queue bounds and offer cadence
// stay identical. A fix cannot discard this measured upward transition.
func TestWindowReceiverCarrierFasterTrainRaisesMeasuredCapacity(t *testing.T) {
	evidence := runReceiverCarrierBufferWorker(t, false, 12500000, 125000000, 0)
	if evidence.serviceRate < evidence.physicalRate*90/100 || evidence.serviceRate > evidence.physicalRate*101/100 {
		t.Fatalf("genuinely faster physical train was not measured: %+v", evidence)
	}
}

// The serializer and paused carrier reader have independent fixed queues.
// Every protocol, ACK and pacing transition still runs through actual workers.
func runReceiverCarrierBufferWorker(t *testing.T, stall bool, priorRate, physicalRate ByteCount, releaseDelay time.Duration) receiverCarrierBufferEvidence {
	t.Helper()
	assertMessagePoolOwnership(t)
	defer startIngressCounterDiagnostic(t)()
	var evidence receiverCarrierBufferEvidence
	synctest.Test(t, func(t *testing.T) {
		const trainCount = 24
		const payloadBytes = 2048
		ctx, cancel := context.WithCancel(context.Background())
		startSend, releaseReceive, receivePaused := make(chan struct{}), make(chan struct{}), make(chan struct{})
		type physicalEvent struct {
			at    time.Time
			bytes int
		}
		writes := make(chan time.Time, trainCount+4)
		arrivals := make(chan physicalEvent, trainCount+4)
		callbacks := make(chan error, trainCount+4)
		var ackCount atomic.Int64
		newSettings := func() *ClientSettings {
			s := DefaultClientSettings()
			s.Log = NewNoopLogger()
			s.EncryptionSettings.Mode = EncryptionModeOff
			s.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
			s.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
			s.SendBufferSettings.ApplyWindowSizing()
			s.SendBufferSettings.ResendQueueBudget = NewTransferMemoryBudget(mib(8))
			s.ReceiveBufferSettings.ReceiveQueueBudget = NewTransferMemoryBudget(mib(8))
			s.ReceiveBufferSettings.AdvertiseReceiveWindow = true
			s.ReceiveBufferSettings.AckCompressTimeout = 0
			return s
		}
		senderSettings, receiverSettings := newSettings(), newSettings()
		senderSettings.SendBufferSettings.beforeRunSendSequenceForTest = func(sendSequenceId) {
			select {
			case <-startSend:
			case <-ctx.Done():
			}
		}
		senderSettings.SendBufferSettings.afterInitialWriteQueuedForTest = func(sendSequenceId, uint64) {
			writes <- time.Now()
			// A real worker dispatches originals into its bounded route more
			// quickly than the slower serializer consumes them.
			time.Sleep(10 * time.Microsecond)
		}
		senderSettings.SendBufferSettings.afterAckCoalescedForTest = func(sendSequenceId, uint64) { ackCount.Add(1) }
		firstReceive := true
		receiverSettings.ReceiveBufferSettings.beforeCreateReceiveSequenceForTest = func(receiveSequenceId) {
			if firstReceive {
				firstReceive = false
				close(receivePaused)

			}
			// This ordinary Client-to-sequence handoff is synchronous. A tiny
			// fixed virtual processing cost lets separate queued reads retain
			// distinct observation times, without supplying any rate oracle.
			time.Sleep(time.Microsecond)
		}
		sender := NewClient(ctx, NewId(), NewNoContractClientOob(), senderSettings)
		receiver := NewClient(ctx, NewId(), NewNoContractClientOob(), receiverSettings)
		sender.ContractManager().AddNoContractPeer(receiver.ClientId())
		receiver.ContractManager().AddNoContractPeer(sender.ClientId())
		queueSize := DefaultPlatformTransportSettings().TransportBufferSize
		if queueSize != 32 {
			t.Fatal("carrier queue precondition changed")
		}
		carrierBuffered := make(Route, queueSize)
		sendOut, sendIn := make(Route, queueSize), make(Route, queueSize)
		receiveOut, receiveIn := make(Route, queueSize), make(Route, queueSize)
		sender.RouteManager().UpdateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{sendOut})
		sender.RouteManager().UpdateTransportWithProperties(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{sendIn}, TransferCarrierProperties{ReceiveReliability: CarrierReliabilityReliable})
		receiver.RouteManager().UpdateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{receiveOut})
		receiver.RouteManager().UpdateTransportWithProperties(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{receiveIn}, TransferCarrierProperties{ReceiveReliability: CarrierReliabilityReliable})
		dataCarrier := &PlatformTransport{receiveStats: &PlatformTransportReceiveStats{}}
		ackCarrier := &PlatformTransport{receiveStats: &PlatformTransportReceiveStats{}}
		var workers sync.WaitGroup
		var maxQueued, maxCarrierQueued atomic.Int64
		var delivered atomic.Int64
		receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				delivered.Add(int64(len(frame.MessageBytes)))
			}
		})
		workers.Go(func() {
			for {
				var wire []byte
				select {
				case <-ctx.Done():
					return
				case wire = <-sendOut:
				}
				span := time.Duration(int64(len(wire)) * int64(time.Second) / int64(physicalRate))
				select {
				case <-ctx.Done():
					MessagePoolReturn(wire)
					return
				case <-time.After(span):
				}
				arrival := physicalEvent{at: time.Now(), bytes: len(wire)}
				select {
				case <-ctx.Done():
					MessagePoolReturn(wire)
					return
				case carrierBuffered <- wire:
				}
				maxCarrierQueued.Store(max(maxCarrierQueued.Load(), int64(len(carrierBuffered))))
				arrivals <- arrival
			}
		})
		workers.Go(func() {
			if stall {
				select {
				case <-releaseReceive:
				case <-ctx.Done():
					return
				}
			}
			for {
				var wire []byte
				select {
				case <-ctx.Done():
					return
				case wire = <-carrierBuffered:
				}
				open, admitted := dataCarrier.offerReceive(ctx.Done(), TransportModeH1, CarrierReliabilityReliable, receiveIn, wire)
				if !open {
					return
				}
				if !admitted {
					t.Error("reliable reader delivery refused")
					return
				}
				maxQueued.Store(max(maxQueued.Load(), int64(len(receiveIn))))
				// A fixed reader processing cost remains faster than either
				// physical serializer, while the Client can await every read.
				time.Sleep(10 * time.Microsecond)
			}
		})
		workers.Go(func() {
			for {
				var wire []byte
				select {
				case <-ctx.Done():
					return
				case wire = <-receiveOut:
				}
				open, admitted := ackCarrier.offerReceive(ctx.Done(), TransportModeH1, CarrierReliabilityReliable, sendIn, wire)
				if !open {
					return
				}
				if !admitted {
					t.Error("reliable ACK refused")
					return
				}
			}
		})
		defer func() {
			cancel()
			workers.Wait()
			for _, client := range []*Client{sender, receiver} {
				if err := client.CloseAndWait(context.Background()); err != nil {
					t.Errorf("client cleanup: %v", err)
				}
			}
			for _, route := range []Route{sendOut, sendIn, receiveOut, receiveIn, carrierBuffered} {
				for len(route) != 0 {
					MessagePoolReturn(<-route)
				}
			}
			for _, settings := range []*ClientSettings{senderSettings, receiverSettings} {
				for _, budget := range []*TransferMemoryBudget{settings.SendBufferSettings.ResendQueueBudget, settings.ReceiveBufferSettings.ReceiveQueueBudget} {
					if got := budget.UsedByteCount(); got != 0 {
						t.Errorf("retained budget after close: %d", got)
					}
				}
			}
		}()
		sequence := sender.sendBuffer.createSendSequence(sendSequenceId{Destination: receiver.ClientId()}, &SendPack{Destination: receiver.ClientId()})
		synctest.Wait()
		service := sequence.windowPacer.service
		start := time.Now()
		for i := 0; i <= 8; i++ {
			at := start.Add(time.Duration(i-8) * 10 * time.Millisecond)
			bytes := priorRate / 100
			service.sent += bytes
			service.observeReceiverRoundTrip(0, 300*time.Microsecond, 300*time.Microsecond, 0, at)
			service.observe(bytes, at)
		}
		if rate, _, _ := service.measured(time.Second, start); rate != priorRate {
			t.Fatalf("prior service precondition: %d", rate)
		}
		close(startSend)
		var firstOffer, lastOffer time.Time
		for i := 0; i < trainCount; i++ {
			frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(payloadBytes)}
			if !sender.SendWithTimeout(frame, receiver.ClientId(), func(err error) { callbacks <- err }, -1) {
				MessagePoolReturn(frame.MessageBytes)
				t.Fatal("original producer refused")
			}
			at := <-writes
			if i == 0 {
				firstOffer = at
			}
			lastOffer = at
		}
		var firstArrival, lastArrival time.Time
		var serializedBytes int
		for i := 0; i < trainCount; i++ {
			arrival := <-arrivals
			if i == 0 {
				firstArrival = arrival.at
			} else {
				serializedBytes += arrival.bytes
			}
			lastArrival = arrival.at
		}
		synctest.Wait()
		if stall {
			if len(carrierBuffered) != trainCount || len(receiveIn) != 0 || delivered.Load() != 0 {
				t.Fatalf("carrier stall did not retain the physical train upstream: carrier=%d client=%d bytes=%d", len(carrierBuffered), len(receiveIn), delivered.Load())
			}
			time.Sleep(releaseDelay)
			close(releaseReceive)
		}
		<-receivePaused
		for i := 0; i < trainCount; i++ {
			if err := <-callbacks; err != nil {
				t.Fatalf("logical delivery: %v", err)
			}
		}
		synctest.Wait()
		service.stateLock.Lock()
		initialBurst := service.burstMeter.limit
		logicalCredit := sequence.windowPacer.serviceAcked
		service.stateLock.Unlock()
		// Wait only for the existing 10ms cached-controller refresh; this is
		// virtual time and does not alter estimator evidence.
		time.Sleep(11 * time.Millisecond)
		estimate := sequence.sendWindowEstimate(time.Now())
		d := &ingressCounterDiagnostic
		d.stateLock.Lock()
		pairs := d.pairs
		var receiveSpan time.Duration
		if observed := d.senders[service]; observed != nil {
			for _, sample := range observed.rates {
				if sample.rate == estimate.ServiceByteRate {
					receiveSpan = time.Duration(sample.receiverEnd - sample.receiverStart)
				}
			}
		}
		d.stateLock.Unlock()
		frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(payloadBytes)}
		if !sender.SendWithTimeout(frame, receiver.ClientId(), func(err error) { callbacks <- err }, -1) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("post-observation producer refused")
		}
		<-writes
		synctest.Wait()
		service.stateLock.Lock()
		resultBurst := service.burstMeter.limit
		service.stateLock.Unlock()
		if err := <-callbacks; err != nil {
			t.Fatalf("post-observation delivery: %v", err)
		}
		synctest.Wait()
		for _, client := range []*Client{sender, receiver} {
			stats := client.ReceiveStats()
			if stats.PackHandoffDropCount != 0 || stats.ReceiveQueueDropCount != 0 || stats.AckHandoffDropCount != 0 {
				t.Fatalf("Client dropped the train: %+v", stats)
			}
		}
		if dataCarrier.ReceiveStats().H1.QueueDropMessageCount != 0 || ackCarrier.ReceiveStats().H1.QueueDropMessageCount != 0 {
			t.Fatal("carrier dropped the train")
		}
		if sender.SendRecoveryStats().TimeoutResendWriteCount != 0 {
			t.Fatal("train retried")
		}
		evidence = receiverCarrierBufferEvidence{
			physicalRate: physicalRate, priorRate: priorRate, serviceRate: estimate.ServiceByteRate, pacingRate: estimate.PacingByteRate,
			initialBurst: initialBurst, resultBurst: resultBurst, physicalBytes: serializedBytes, physicalSpan: lastArrival.Sub(firstArrival), offerSpan: lastOffer.Sub(firstOffer),
			receiveSpan: receiveSpan, maxQueued: int(maxQueued.Load()), maxCarrierQueued: int(maxCarrierQueued.Load()), ackCount: int(ackCount.Load()), pairs: pairs, logicalCredit: logicalCredit, physicalCopies: trainCount,
			applicationByte: delivered.Load(),
		}
		if pairs < 2 || delivered.Load() != int64((trainCount+1)*payloadBytes) || maxQueued.Load() > int64(queueSize) {
			t.Fatalf("missing bounded worker evidence: %+v", evidence)
		}
		t.Logf("receiver-carrier-buffer stall=%t evidence=%+v", stall, evidence)
	})
	return evidence
}

// Bucket placement must not decide whether identical queued bytes prove a
// capacity rise. Every delay follows the explicit completed-serializer barrier.
func TestWindowReceiverCarrierBufferedReadsAcrossSamplerBuckets(t *testing.T) {
	for _, delay := range []time.Duration{5 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond, 50 * time.Millisecond} {
		evidence := runReceiverCarrierBufferWorker(t, true, 12500000, 12500000, delay)
		if evidence.serviceRate > evidence.physicalRate*101/100 || evidence.resultBurst > evidence.initialBurst*101/100 {
			t.Errorf("release delay %s changed queued service/burst: %+v", delay, evidence)
		}
	}
}
