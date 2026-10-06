package connect

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Two native clients transfer real serialized frames and receiver-generated
// ACKs. Holding only the return hop lets a transport retire after delivery but
// before its ACK, without manufacturing sendItems or invoking the clock helper.
type routeAckRetentionHarness struct {
	t          *testing.T
	ctx        context.Context
	cancel     context.CancelFunc
	sender     *Client
	receiver   *Client
	transport  Transport
	input      Transport
	feedback   Route
	ackReturn  Route
	current    Route
	selector   *MultiRouteSelector
	mutex      sync.Mutex
	deliveries [][]byte
	closes     []*protocol.CloseContract
	witnesses  []*closeWaitPoolWitness
}

func newRouteAckRetentionHarness(t *testing.T) *routeAckRetentionHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	h := &routeAckRetentionHarness{t: t, ctx: ctx, cancel: cancel,
		feedback: make(Route, 4096), ackReturn: make(Route, 4096)}
	settings := func() *ClientSettings {
		s := closeWaitClientSettings()
		s.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		s.SendBufferSettings.IdleTimeout = time.Hour
		s.SendBufferSettings.WriteTimeout = time.Millisecond
		s.ReceiveBufferSettings.IdleTimeout = time.Hour
		s.ReceiveBufferSettings.AckCompressTimeout = 0
		return s
	}
	h.sender = NewClient(ctx, NewId(), NewNoContractClientOob(), settings())
	h.receiver = NewClient(ctx, ControlId, NewNoContractClientOob(), settings())
	h.sender.ContractManager().AddNoContractPeer(h.receiver.ClientId())
	h.receiver.ContractManager().AddNoContractPeer(h.sender.ClientId())
	h.sender.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{h.ackReturn})
	h.receiver.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{h.feedback})
	h.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		h.mutex.Lock()
		defer h.mutex.Unlock()
		for _, frame := range frames {
			if frame.MessageType == protocol.MessageType_TransferCloseContract {
				var closeContract protocol.CloseContract
				if err := ProtoUnmarshal(frame.MessageBytes, &closeContract); err != nil {
					t.Error(err)
				}
				h.closes = append(h.closes, &closeContract)
			} else {
				h.deliveries = append(h.deliveries, bytes.Clone(frame.MessageBytes))
			}
		}
	})
	h.transport, h.input = NewSendGatewayTransport(), NewReceiveGatewayTransport()
	h.install(make(Route, 4096))
	t.Cleanup(func() {
		cancel()
		for _, client := range []*Client{h.sender, h.receiver} {
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}
		for _, route := range []Route{h.current, h.feedback, h.ackReturn} {
			for len(route) > 0 {
				MessagePoolReturn(<-route)
			}
		}
		for _, witness := range h.witnesses {
			witness.cleanup()
		}
	})
	return h
}

func (h *routeAckRetentionHarness) install(route Route) {
	h.current = route
	h.receiver.RouteManager().UpdateTransport(h.input, []Route{route})
	h.sender.RouteManager().UpdateTransport(h.transport, []Route{route})
}

func (h *routeAckRetentionHarness) send(index int) (<-chan error, *closeWaitPoolWitness) {
	h.t.Helper()
	witness := newCloseWaitPoolWitness(h.t)
	for i := range witness.owner {
		witness.owner[i] = byte(index + i)
	}
	want := bytes.Clone(witness.owner)
	ack := make(chan error, 2)
	frame := &protocol.Frame{MessageType: protocol.MessageType_TestSimpleMessage, MessageBytes: witness.owner}
	if !h.sender.SendWithTimeout(frame, h.receiver.ClientId(), func(err error) { ack <- err }, time.Second) {
		witness.cleanup()
		h.t.Fatal("native data admission failed")
	}
	h.witnesses = append(h.witnesses, witness)
	synctest.Wait()
	h.mutex.Lock()
	if len(h.deliveries) != index+1 || !bytes.Equal(h.deliveries[index], want) {
		h.mutex.Unlock()
		h.t.Fatalf("native payload %d was not delivered once with exact bytes", index)
	}
	h.mutex.Unlock()
	select {
	case err := <-ack:
		h.t.Fatalf("data completed before the held return ACK: %v", err)
	default:
	}
	manager := h.sender.RouteManager()
	manager.mutex.Lock()
	selectors := manager.writerMatchState.destinationMultiRouteSelectors[DestinationId(h.receiver.ClientId())]
	if len(selectors) != 1 {
		manager.mutex.Unlock()
		h.t.Fatalf("native destination has %d writers, want one", len(selectors))
	}
	for selector := range selectors {
		if h.selector != nil && h.selector != selector {
			manager.mutex.Unlock()
			h.t.Fatal("native send sequence changed across route replacement")
		}
		h.selector = selector
	}
	manager.mutex.Unlock()
	return ack, witness
}

func (h *routeAckRetentionHarness) deliverAcks(ack <-chan error, witness *closeWaitPoolWitness) {
	h.t.Helper()
	if len(h.feedback) == 0 {
		h.t.Fatal("receiver did not produce its real return ACK")
	}
	for len(h.feedback) > 0 {
		h.ackReturn <- <-h.feedback
	}
	synctest.Wait()
	select {
	case err := <-ack:
		if err != nil {
			h.t.Fatalf("late native ACK failed: %v", err)
		}
	case <-time.After(time.Second):
		h.t.Fatal("native ACK did not complete the accepted data within the existing no-route write wait")
	}
	synctest.Wait()
	select {
	case err := <-ack:
		h.t.Fatalf("native ACK completed one item twice: %v", err)
	default:
	}
	witness.requireOwnerReleased(h.t, "native ACK completion")
}

func (h *routeAckRetentionHarness) clocks() int {
	count := 0
	h.selector.routeAckProgress.Range(func(_, _ any) bool { count++; return true })
	return count
}

// A live selector must not accumulate the old 4096-slot route channel keys
// after terminal ACK completion. No GC or sampled heap profile supplies this
// assertion: the exact map is the strong-reference witness.
func TestRouteAckProgressNativeRetirementReleasesChannelKeys(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	for _, replacement := range []bool{false, true} {
		name := "remove_then_late_ack"
		if replacement {
			name = "nonempty_replacement_after_ack"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newRouteAckRetentionHarness(t)
				const generations = 32
				stale := 0
				for index := range generations {
					old := h.current
					ack, witness := h.send(index)
					if replacement {
						h.deliverAcks(ack, witness)
						if _, ok := h.selector.RouteAckProgressAge(old); !ok {
							t.Fatal("live-route native ACK lost its progress clock")
						}
						h.install(make(Route, 4096))
					} else {
						h.sender.RouteManager().RemoveTransport(h.transport)
						h.receiver.RouteManager().RemoveTransport(h.input)
						// The healthy return hop is independent from the removed
						// data route. Deliver its ACK before publishing the next
						// data route, so a resend cannot change carrier attribution.
						h.deliverAcks(ack, witness)
						h.install(make(Route, 4096))
					}
					if _, ok := h.selector.RouteAckProgressAge(old); ok {
						stale++
					}
					if len(old) != 0 {
						t.Fatal("retired data route retained a queued payload")
					}
				}
				// The same writer remains healthy on its replacement generation.
				ack, witness := h.send(generations)
				h.deliverAcks(ack, witness)
				if _, ok := h.selector.RouteAckProgressAge(h.current); !ok {
					t.Fatal("replacement route did not record its own real ACK")
				}
				t.Logf("retired_generations=%d stale_channel_keys=%d total_clock_keys=%d retired_slot_bytes=%d", generations, stale, h.clocks(), stale*4096*24)
				if stale != 0 || h.clocks() != 1 {
					t.Errorf("retired route channels remain strongly referenced: stale=%d clocks=%d, want 0/1", stale, h.clocks())
				}
				// Real financial work on the surviving route must still settle.
				contractID := NewId()
				h.sender.ContractManager().CloseContract(contractID, 121, 7)
				synctest.Wait()
				for len(h.feedback) > 0 {
					h.ackReturn <- <-h.feedback
				}
				synctest.Wait()
				h.mutex.Lock()
				if len(h.closes) != 1 || !bytes.Equal(h.closes[0].ContractId, contractID.Bytes()) || h.closes[0].AckedByteCount != 121 || h.closes[0].UnackedByteCount != 7 || h.closes[0].Checkpoint {
					t.Error("financial close identity or byte accounting changed")
				}
				h.mutex.Unlock()
				manager := h.sender.ContractManager()
				manager.mutex.Lock()
				pending := len(manager.closeControlSyncs)
				manager.mutex.Unlock()
				if pending != 0 {
					t.Errorf("financial close left %d sync owners after real ACK", pending)
				}
			})
		})
	}
}
