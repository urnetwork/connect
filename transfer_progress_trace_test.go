package connect

import (
	"context"
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func progressTraceTestObserver() (func(TransferProgressEvent), chan TransferProgressEvent) {
	events := make(chan TransferProgressEvent, 128)
	return func(event TransferProgressEvent) { events <- event }, events
}

func takeProgressTraceEvents(events chan TransferProgressEvent) []TransferProgressEvent {
	var result []TransferProgressEvent
	for len(events) > 0 {
		result = append(result, <-events)
	}
	return result
}

func TestTransferProgressTraceNilAndPanicCannotChangeOwnership(t *testing.T) {
	wire := make([]byte, 1400)
	event := TransferProgressEvent{Stage: "test"}
	if allocations := testing.AllocsPerRun(100, func() {
		got := beginTransferProgress(nil, event, wire)
		if got.AtUnixNano != 0 || got.WireHash != 0 {
			t.Fatal("nil observer performed trace work")
		}
		endTransferProgress(nil, got, "end", false, context.Canceled)
	}); allocations != 0 {
		t.Fatalf("nil trace allocated %g objects", allocations)
	}
	observer := func(TransferProgressEvent) { panic("diagnostic failure") }
	beginTransferProgress(observer, event, wire)
	endTransferProgress(observer, event, "end", true, nil)
	for err, expected := range map[error]string{
		errTransferRouteWriteTimeout: "route_timeout", context.Canceled: "canceled",
		context.DeadlineExceeded: "deadline", ErrSendPackNotAdmitted: "admission",
		net.ErrClosed: "closed", errors.New("private endpoint text"): "error",
	} {
		if got := transferProgressErrorKind(err); got != expected {
			t.Fatalf("error class=%q, want %q", got, expected)
		}
	}
}

// Actual receive ACK worker: preserve the distinction between a pending
// write, a typed local timeout, a retry and eventual carrier admission.
func TestTransferProgressTraceAckBackpressure(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		observer, events := progressTraceTestObserver()
		fixture := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			settings.ProgressObserver = observer
			settings.AckCompressTimeout = 10 * time.Millisecond
			settings.WriteTimeout = 15 * time.Second
		})
		for range cap(fixture.route) {
			fixture.route <- nil
		}
		messageId := NewId()
		fixture.receiveSequence.sendAck(565, messageId, false, sequenceTag{}, false, TransportTypeP2p)
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		initial := takeProgressTraceEvents(events)
		if len(initial) != 1 || initial[0].Stage != "ack_write_begin" || initial[0].MessageId != messageId || initial[0].SequenceNumber != 565 {
			t.Fatalf("blocked ACK begin=%+v", initial)
		}
		time.Sleep(15 * time.Second)
		synctest.Wait()
		expired := takeProgressTraceEvents(events)
		if len(expired) != 2 || expired[0].Stage != "ack_write_end" || expired[0].Success ||
			expired[0].ErrorKind != "route_timeout" || expired[0].ElapsedNanos != int64(15*time.Second) ||
			expired[1].Stage != "ack_write_begin" || expired[1].MessageId != messageId {
			t.Fatalf("ACK timeout/retry trace=%+v", expired)
		}
		for range cap(fixture.route) {
			if wire := <-fixture.route; wire != nil {
				MessagePoolReturn(wire)
				t.Fatal("blocked ACK was accepted before capacity returned")
			}
		}
		got, ok := fixture.readAck(t, time.Second)
		synctest.Wait()
		finished := takeProgressTraceEvents(events)
		if !ok || got != messageId || len(finished) != 1 || finished[0].Stage != "ack_write_end" ||
			!finished[0].Success || finished[0].ErrorKind != "" {
			t.Fatalf("recovered ACK trace=%+v accepted=%t", finished, ok)
		}
	})
}

// Real Client decode, ReceiveSequence delivery and acknowledgement write keep
// message/sequence identities and a carrier checksum without retaining bytes.
func TestTransferProgressTraceReceiveAndDelivery(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		observer, events := progressTraceTestObserver()
		fixture := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
			settings.ProgressObserver = observer
		}, func(settings *ReceiveBufferSettings) {
			settings.ProgressObserver = observer
		})
		wire := fixture.write(100)
		messageId := RequireIdFromBytes(wire.pack.MessageId)
		fixture.forward(wire, fixture.receiverIn)
		fixture.acknowledge()
		synctest.Wait()
		stages := map[string]TransferProgressEvent{}
		for _, event := range takeProgressTraceEvents(events) {
			stages[event.Stage] = event
		}
		for _, stage := range []string{"send_attempt", "receive_wire", "receive_pack_begin", "receive_pack_end", "deliver_begin", "deliver_end", "ack_write_begin", "ack_write_end"} {
			event, ok := stages[stage]
			if !ok || event.AtUnixNano == 0 {
				t.Fatalf("missing %s in trace", stage)
			}
			if stage != "receive_wire" && event.MessageId != messageId {
				t.Fatalf("%s lost message identity", stage)
			}
		}
		if stages["send_attempt"].WireHash != stages["receive_wire"].WireHash ||
			stages["send_attempt"].WireHash == 0 || !stages["receive_pack_end"].Success ||
			!stages["deliver_end"].Success || !stages["ack_write_end"].Success || fixture.ackedCount != 1 {
			t.Fatal("trace changed or failed to correlate the real delivery lifecycle")
		}
	})
}

// An ACK can arrive at the Client and still target a retired/wrong sequence.
// Exercise the actual decoder and SendBuffer lookup rather than an injected
// observer result, and distinguish it from accepted peer feedback.
func TestTransferProgressTraceAckAcceptedVersusMissingSequence(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		observer, events := progressTraceTestObserver()
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.ReceiveBufferSettings.ProgressObserver = observer
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		peer := NewId()
		client.ContractManager().AddNoContractPeer(peer)
		outgoing, incoming := make(Route, 8), make(Route, 8)
		client.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{outgoing})
		client.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{incoming})
		defer func() {
			cancel()
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			for _, route := range []Route{outgoing, incoming} {
				for len(route) > 0 {
					MessagePoolReturn(<-route)
				}
			}
		}()
		completed := make(chan error, 1)
		if sent, err := client.SendWithTimeoutDetailed(budgetTestFrame(100), peer, func(err error) { completed <- err }, time.Second); !sent || err != nil {
			t.Fatalf("admission=%t,%v", sent, err)
		}
		synctest.Wait()
		wire := <-outgoing
		pack := decodeSendPackLifecycleWirePack(t, wire)
		MessagePoolReturn(wire)
		for _, missing := range []bool{true, false} {
			sequenceId := pack.SequenceId
			if missing {
				sequenceId = NewId().Bytes()
			}
			frame := &protocol.TransferFrame{
				TransferPath: (TransferPath{SourceId: peer, DestinationId: client.ClientId()}).ToProtobuf(),
				Ack:          &protocol.Ack{MessageId: pack.MessageId, SequenceId: sequenceId},
			}
			ackWire, err := ProtoMarshal(frame)
			if err != nil {
				t.Fatal(err)
			}
			incoming <- ackWire
			synctest.Wait()
			trace := takeProgressTraceEvents(events)
			if len(trace) != 3 || trace[0].Stage != "receive_wire" || trace[1].Stage != "receive_ack_begin" || trace[2].Stage != "receive_ack_end" {
				t.Fatalf("ACK handoff trace=%+v", trace)
			}
			want := "accepted"
			if missing {
				want = "sequence_missing"
			}
			if trace[2].Outcome != want || trace[2].Success == missing {
				t.Fatalf("ACK outcome=%+v", trace[2])
			}
		}
		if err := <-completed; err != nil {
			t.Fatalf("real ACK failed: %v", err)
		}
	})
}

// The carrier begins Write but has not returned. This is different from a
// route-queue stall upstream or a completed write whose peer never received it.
func TestTransferProgressTraceP2pBlockedWriteAndReceive(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		left, right := net.Pipe()
		defer left.Close()
		defer right.Close()
		observer, events := progressTraceTestObserver()
		settings := DefaultP2pTransportSettings()
		settings.DataPlaneMode = P2pDataPlaneModeLegacyOnly
		settings.ProgressObserver = observer
		transport, route := NewP2pSendTransport(ctx, cancel, left, NewId(), settings)
		defer transport.(P2pRouteLifecycle).CloseAndWait(context.Background())
		wire := []byte("metadata-only carrier correlation")
		route <- MessagePoolCopy(wire)
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		begun := takeProgressTraceEvents(events)
		if len(begun) != 1 || begun[0].Stage != "p2p_write_begin" {
			t.Fatalf("blocked SCTP boundary=%+v", begun)
		}
		buffer := make([]byte, len(wire))
		if n, err := right.Read(buffer); err != nil || n != len(wire) {
			t.Fatalf("carrier read=%d,%v", n, err)
		}
		synctest.Wait()
		written := takeProgressTraceEvents(events)
		if len(written) != 1 || written[0].Stage != "p2p_write_end" || !written[0].Success || written[0].ElapsedNanos != int64(time.Second) {
			t.Fatalf("carrier write completion=%+v", written)
		}
		receiver := &P2pReceiveTransport{ctx: ctx, settings: settings, receive: make(Route)}
		done := make(chan bool, 1)
		go func() { done <- receiver.offerReceive(MessagePoolCopy(buffer), false, 1, false, true) }()
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		receiving := takeProgressTraceEvents(events)
		if len(receiving) != 1 || receiving[0].Stage != "p2p_receive_begin" || receiving[0].WireHash != begun[0].WireHash {
			t.Fatalf("blocked reliable receive=%+v", receiving)
		}
		MessagePoolReturn(<-receiver.receive)
		if !<-done {
			t.Fatal("reliable receive was refused")
		}
		synctest.Wait()
		accepted := takeProgressTraceEvents(events)
		if len(accepted) != 1 || accepted[0].Stage != "p2p_receive_end" || !accepted[0].Success || accepted[0].ElapsedNanos != int64(time.Second) {
			t.Fatalf("receive completion=%+v", accepted)
		}
	})
}
