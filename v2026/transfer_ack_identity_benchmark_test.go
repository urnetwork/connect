package connect

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Measure the static-window hot coalescer with the same retained identities
// and feedback before and after the recovery-lookup change. The live-item and
// sequence sizes make fixed owner cost distinct from per-message growth.
func BenchmarkRetainedAckHeadCoalescing(b *testing.B) {
	sequence := &SendSequence{client: &Client{}, resendQueue: newResendQueue(nil, 0)}
	item := &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 7}}
	sequence.resendQueue.Add(item)
	window := newSequenceAckWindow()
	ack := receiveAckMessage{messageId: item.messageId}
	sequence.coalesceReceivedAck(window, ack)
	window.Snapshot(true)
	b.ReportAllocs()
	for b.Loop() {
		sequence.coalesceReceivedAck(window, ack)
		window.Snapshot(true)
	}
	b.ReportMetric(float64(unsafe.Sizeof(SendSequence{})), "sequence-B")
	b.ReportMetric(float64(unsafe.Sizeof(sendItem{})), "item-B")
}

// Real sender and ACK workers, wire encoding/decoding, route publication and
// final callbacks. The retry arm drops the initial route-owned Pack and retires
// that route, so real carrier recovery runs immediately without sleeping out
// an RTO or introducing a host network into this CPU/ownership comparison.
func BenchmarkRetainedAckSendLifecycle(b *testing.B) {
	for _, retry := range []bool{false, true} {
		b.Run(fmt.Sprintf("retry=%t", retry), func(b *testing.B) {
			ctx, cancel := context.WithCancel(context.Background())
			settings := DefaultClientSettings()
			settings.Log = NewNoopLogger()
			settings.EncryptionSettings.Mode = EncryptionModeOff
			settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
			settings.SendBufferSettings.WindowSizing = WindowSizingConstant
			settings.SendBufferSettings.ApplyWindowSizing()
			client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
			peer := NewId()
			client.ContractManager().AddNoContractPeer(peer)
			routes := [2]Route{make(Route, 8), make(Route, 8)}
			fromPeer := make(Route, 8)
			transport := NewSendGatewayTransportWithType(TransportTypeH1)
			client.RouteManager().UpdateTransport(transport, []Route{routes[0]})
			client.RouteManager().UpdateTransport(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{fromPeer})
			b.Cleanup(func() {
				cancel()
				if err := client.CloseAndWait(context.Background()); err != nil {
					b.Error(err)
				}
				for _, route := range []Route{routes[0], routes[1], fromPeer} {
					drainFlightGateRoute(route)
				}
			})
			terminal := make(chan error, 1)
			callback := func(err error) { terminal <- err }
			active := 0
			b.ReportAllocs()
			b.SetBytes(1280)
			for b.Loop() {
				frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1280)}
				if !client.SendWithTimeout(frame, peer, callback, time.Second) {
					MessagePoolReturn(frame.MessageBytes)
					b.Fatal("lifecycle Pack not admitted")
				}
				pack := decodeFlightGatePack(b, <-routes[active])
				if pack == nil {
					b.Fatal("lifecycle wire did not contain a Pack")
				}
				if retry {
					active = 1 - active
					client.RouteManager().UpdateTransport(transport, []Route{routes[active]})
					repeated := decodeFlightGatePack(b, <-routes[active])
					if repeated == nil || !bytes.Equal(repeated.MessageId, pack.MessageId) || repeated.SequenceNumber != pack.SequenceNumber {
						b.Fatal("retired route did not recover the exact retained Pack")
					}
					pack = repeated
				}
				ack, err := ProtoMarshal(&protocol.TransferFrame{
					TransferPath: sendTransferPath(peer, DestinationId(client.ClientId())).ToProtobuf(),
					Ack:          &protocol.Ack{SequenceId: pack.SequenceId, MessageId: pack.MessageId},
				})
				if err != nil {
					b.Fatal(err)
				}
				fromPeer <- ack
				if err := <-terminal; err != nil {
					b.Fatalf("lifecycle terminal callback: %v", err)
				}
			}
			if retry && client.SendRecoveryStats().CarrierChangeWriteCount != uint64(b.N) {
				b.Fatal("lifecycle retry count did not match actual retired-route recovery")
			}
		})
	}
}

// Allocate only the sequence envelope, with all owners kept live together.
// This separates allocator size-class cost from unchanged routes, queues and
// retained messages; it is not a complete-client memory qualification.
func BenchmarkRetainedAckSequenceFanout(b *testing.B) {
	for _, count := range []int{1, 64, 1024} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			owners := make([]*SendSequence, count)
			b.ReportAllocs()
			for b.Loop() {
				for i := range owners {
					owners[i] = &SendSequence{}
				}
			}
			runtime.KeepAlive(owners)
			b.ReportMetric(float64(unsafe.Sizeof(SendSequence{}))*float64(count), "live-owner-B")
			b.ReportMetric(float64(count), "owners/op")
		})
	}
}
