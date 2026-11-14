// A finite first body uses the real clients and the existing reliable link
// model. No bulk warmup can conceal a feedback or startup pacing penalty.
package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// A fresh 64 KiB body fits the unchanged 2 MiB opening before body feedback.
func TestWindowOpeningCleanFirstFlight(t *testing.T) {
	runWindowOpeningCleanFirstFlight(t, false)
}

// One acknowledged tiny control may consume allowance but cannot turn its
// request/reply cadence into a rate limit on the first useful body.
func TestWindowOpeningCleanFirstFlightAfterTinyControl(t *testing.T) {
	runWindowOpeningCleanFirstFlight(t, true)
}

// Run only one bounded body, with an optional one-byte setup exchange.
// Completion and the first body acknowledgement are explicit event barriers.
func runWindowOpeningCleanFirstFlight(t *testing.T, tinyControl bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const bodyBytes = 64 * 1024
		const payloadBytes = 1048
		const roundTrip = 4 * time.Millisecond
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		settings := func() *ClientSettings {
			s := DefaultClientSettings()
			s.Log = NewNoopLogger()
			s.EncryptionSettings.Mode = EncryptionModeOff
			s.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
			s.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
			s.SendBufferSettings.ApplyWindowSizing()
			s.SendBufferSettings.ResendQueueBudget = nil
			if s.SendBufferSettings.ResendQueueMaxByteCount != 2*1024*1024 ||
				s.SendBufferSettings.ResendQueueMinByteCount != 256*1024 ||
				s.ReceiveBufferSettings.AckCompressTimeout != 10*time.Millisecond {
				t.Fatal("the clean guard requires the unchanged original opening and compression")
			}
			return s
		}
		senderSettings, receiverSettings := settings(), settings()
		sender := NewClient(ctx, NewId(), NewNoContractClientOob(), senderSettings)
		receiver := NewClient(ctx, NewId(), NewNoContractClientOob(), receiverSettings)
		sender.ContractManager().AddNoContractPeer(receiver.ClientId())
		receiver.ContractManager().AddNoContractPeer(sender.ClientId())
		sendOut, sendIn := make(Route, 128), make(Route, 128)
		receiveOut, receiveIn := make(Route, 128), make(Route, 128)
		sender.RouteManager().UpdateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{sendOut})
		sender.RouteManager().UpdateTransportWithProperties(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{sendIn}, TransferCarrierProperties{ReceiveReliability: CarrierReliabilityReliable})
		receiver.RouteManager().UpdateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{receiveOut})
		receiver.RouteManager().UpdateTransportWithProperties(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{receiveIn}, TransferCarrierProperties{ReceiveReliability: CarrierReliabilityReliable})
		dataLink := windowPathLink{rate: 125000000, delay: roundTrip / 2, queueCount: 4096, queueBytes: 8 * 1024 * 1024}
		ackLink := windowPathLink{rate: 125000000, delay: roundTrip / 2, queueCount: 4096, queueBytes: 8 * 1024 * 1024}
		var workers sync.WaitGroup
		workers.Go(func() { dataLink.run(ctx, sendOut, receiveIn) })
		workers.Go(func() { ackLink.run(ctx, receiveOut, sendIn) })
		defer func() {
			cancel()
			workers.Wait()
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer closeCancel()
			for _, client := range []*Client{sender, receiver} {
				if err := client.CloseAndWait(closeCtx); err != nil {
					t.Errorf("finite body client cleanup: %v", err)
				}
			}
			for _, route := range []Route{sendOut, sendIn, receiveOut, receiveIn} {
				for len(route) != 0 {
					MessagePoolReturn(<-route)
				}
			}
			for _, s := range []*ClientSettings{senderSettings, receiverSettings} {
				for _, budget := range []*TransferMemoryBudget{s.SendBufferSettings.ResendQueueBudget, s.ReceiveBufferSettings.ReceiveQueueBudget, s.ReceiveBufferSettings.PackQueueBudget} {
					if budget != nil && budget.UsedByteCount() != 0 {
						t.Errorf("finite body retained %d owner budget bytes", budget.UsedByteCount())
					}
				}
			}
		}()
		var receivedBytes atomic.Int64
		bodyComplete := make(chan time.Time, 1)
		controlReceived := make(chan struct{}, 1)
		receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				if len(frame.MessageBytes) == 1 && frame.MessageBytes[0] == 0x53 {
					select {
					case controlReceived <- struct{}{}:
					default:
						t.Error("duplicate tiny control delivery")
					}
					continue
				}
				for _, value := range frame.MessageBytes {
					if value != 0 {
						t.Error("finite body payload changed")
						break
					}
				}
				if received := receivedBytes.Add(int64(len(frame.MessageBytes))); received == bodyBytes {
					select {
					case bodyComplete <- time.Now():
					default:
						t.Error("duplicate finite body completion")
					}
				} else if received > bodyBytes {
					t.Errorf("finite body delivered extra bytes: %d", received)
				}
			}
		})
		if tinyControl {
			acknowledged := make(chan error, 1)
			payload := MessagePoolGet(1)
			payload[0] = 0x53
			frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, MessageBytes: payload, Raw: true}
			if ok, err := sender.SendWithTimeoutDetailed(frame, receiver.ClientId(), func(err error) { acknowledged <- err }, -1); !ok || err != nil {
				MessagePoolReturn(payload)
				t.Fatalf("tiny control admission: ok=%t error=%v", ok, err)
			}
			select {
			case err := <-acknowledged:
				if err != nil {
					t.Fatalf("tiny control acknowledgement: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("tiny control did not settle")
			}
			select {
			case <-controlReceived:
			case <-ctx.Done():
				t.Fatal("tiny control did not reach the application")
			}
		}
		type acknowledgement struct {
			at  time.Time
			err error
		}
		firstAcknowledged := make(chan acknowledgement, 1)
		firstOffered := time.Now()
		for offered := 0; offered < bodyBytes; {
			count := min(payloadBytes, bodyBytes-offered)
			payload := MessagePoolGet(count)
			clear(payload)
			frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, MessageBytes: payload, Raw: true}
			var acknowledged AckFunction
			if offered == 0 {
				acknowledged = func(err error) { firstAcknowledged <- acknowledgement{at: time.Now(), err: err} }
			}
			if ok, err := sender.SendWithTimeoutDetailed(frame, receiver.ClientId(), acknowledged, -1); !ok || err != nil {
				MessagePoolReturn(payload)
				t.Fatalf("finite body admission at %d: ok=%t error=%v", offered, ok, err)
			}
			offered += count
		}
		var completed time.Time
		select {
		case completed = <-bodyComplete:
		case <-ctx.Done():
			t.Fatalf("finite body did not complete: delivered=%d", receivedBytes.Load())
		}
		var firstReply acknowledgement
		select {
		case firstReply = <-firstAcknowledged:
		case <-ctx.Done():
			t.Fatal("finite body's first acknowledgement did not settle")
		}
		if firstReply.err != nil {
			t.Fatalf("finite body's first acknowledgement: %v", firstReply.err)
		}
		// The 64 KiB body needs less than one millisecond of serialization
		// plus two milliseconds of propagation, before any four-ms feedback.
		if elapsed := completed.Sub(firstOffered); elapsed >= roundTrip || !completed.Before(firstReply.at) {
			t.Errorf("finite first flight waited for body feedback: tiny-control=%t body=%s first-ack=%s", tinyControl, elapsed, firstReply.at.Sub(firstOffered))
		}
		stats := sender.DestinationSendStats(receiver.ClientId())
		if stats.ResendWriteCount != 0 || stats.SendWindow.Window != 2*1024*1024 || stats.SendWindow.Initial != 2*1024*1024 ||
			dataLink.dropped.Load() != 0 || ackLink.dropped.Load() != 0 {
			t.Errorf("finite clean guard changed original permission or needed recovery: %+v", stats)
		}
		t.Logf("finite-clean bytes=%d body=%s first-ack=%s tiny-control=%t", receivedBytes.Load(), completed.Sub(firstOffered), firstReply.at.Sub(firstOffered), tinyControl)
	})
}
