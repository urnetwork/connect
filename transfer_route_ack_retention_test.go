// Native route churn controls keep one writer alive across retired channel generations.
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

// Two native clients transfer serialized frames and receiver-generated acks.
// Holding the return hop allows retirement after delivery but before acknowledgment.
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
	stateLock  sync.Mutex
	deliveries [][]byte
	closes     []*protocol.CloseContract
	witnesses  []*closeWaitPoolWitness
}

// Keeps return acknowledgments pending and joins both clients during cleanup.
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
		h.stateLock.Lock()
		defer h.stateLock.Unlock()
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

// Publishes the same data route to the native sender and receiver.
func (self *routeAckRetentionHarness) install(route Route) {
	self.current = route
	self.receiver.RouteManager().UpdateTransport(self.input, []Route{route})
	self.sender.RouteManager().UpdateTransport(self.transport, []Route{route})
}

// Admits a pooled frame and verifies delivery while its acknowledgment remains held.
func (self *routeAckRetentionHarness) send(index int) (<-chan error, *closeWaitPoolWitness) {
	self.t.Helper()
	witness := newCloseWaitPoolWitness(self.t)
	for i := range witness.owner {
		witness.owner[i] = byte(index + i)
	}
	want := bytes.Clone(witness.owner)
	ack := make(chan error, 2)
	frame := &protocol.Frame{MessageType: protocol.MessageType_TestSimpleMessage, MessageBytes: witness.owner}
	if !self.sender.SendWithTimeout(frame, self.receiver.ClientId(), func(err error) { ack <- err }, time.Second) {
		witness.cleanup()
		self.t.Fatal("native data admission failed")
	}
	self.witnesses = append(self.witnesses, witness)
	synctest.Wait()
	self.stateLock.Lock()
	if len(self.deliveries) != index+1 || !bytes.Equal(self.deliveries[index], want) {
		self.stateLock.Unlock()
		self.t.Fatalf("native payload %d was not delivered once with exact bytes", index)
	}
	self.stateLock.Unlock()
	select {
	case err := <-ack:
		self.t.Fatalf("data completed before the held return ACK: %v", err)
	default:
	}
	manager := self.sender.RouteManager()
	manager.mutex.Lock()
	selectors := manager.writerMatchState.destinationMultiRouteSelectors[DestinationId(self.receiver.ClientId())]
	if len(selectors) != 1 {
		manager.mutex.Unlock()
		self.t.Fatalf("native destination has %d writers, want one", len(selectors))
	}
	for selector := range selectors {
		if self.selector != nil && self.selector != selector {
			manager.mutex.Unlock()
			self.t.Fatal("native send sequence changed across route replacement")
		}
		self.selector = selector
	}
	manager.mutex.Unlock()
	return ack, witness
}

// Releases real acknowledgments and verifies callback and pooled-buffer ownership.
func (self *routeAckRetentionHarness) deliverAcks(ack <-chan error, witness *closeWaitPoolWitness) {
	self.t.Helper()
	if len(self.feedback) == 0 {
		self.t.Fatal("receiver did not produce its real return ACK")
	}
	for len(self.feedback) > 0 {
		self.ackReturn <- <-self.feedback
	}
	synctest.Wait()
	select {
	case err := <-ack:
		if err != nil {
			self.t.Fatalf("late native ACK failed: %v", err)
		}
	case <-time.After(time.Second):
		self.t.Fatal("native ACK did not complete the accepted data within the existing no-route write wait")
	}
	synctest.Wait()
	select {
	case err := <-ack:
		self.t.Fatalf("native ACK completed one item twice: %v", err)
	default:
	}
	witness.requireOwnerReleased(self.t, "native ACK completion")
}

// Counts exact channel roots without relying on collection or heap sampling.
func (self *routeAckRetentionHarness) clocks() int {
	count := 0
	self.selector.routeAckProgress.Range(func(_, _ any) bool { count++; return true })
	return count
}

// Late acknowledgments preserve accepted work without republishing removed route keys.
func TestRouteAckProgressNativeRemovalReleasesChannelKeys(t *testing.T) {
	testRouteAckProgressNativeRetirementReleasesChannelKeys(t, false)
}

// Replacing a transport route drops its old clock while the native writer stays live.
func TestRouteAckProgressNativeReplacementReleasesChannelKeys(t *testing.T) {
	testRouteAckProgressNativeRetirementReleasesChannelKeys(t, true)
}

// Retires full-size channels through one native writer, then settles a financial close.
func testRouteAckProgressNativeRetirementReleasesChannelKeys(t *testing.T, replacement bool) {
	t.Helper()
	MessagePoolReturn(MessagePoolGet(1))
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
				// Deliver through the independent return hop before publishing a data
				// route, so a resend cannot change the original carrier attribution.
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
		contractId := NewId()
		h.sender.ContractManager().CloseContract(contractId, 121, 7)
		synctest.Wait()
		for len(h.feedback) > 0 {
			h.ackReturn <- <-h.feedback
		}
		synctest.Wait()
		h.stateLock.Lock()
		if len(h.closes) != 1 || !bytes.Equal(h.closes[0].ContractId, contractId.Bytes()) || h.closes[0].AckedByteCount != 121 || h.closes[0].UnackedByteCount != 7 || h.closes[0].Checkpoint {
			t.Error("financial close identity or byte accounting changed")
		}
		h.stateLock.Unlock()
		manager := h.sender.ContractManager()
		manager.mutex.Lock()
		pending := len(manager.closeControlSyncs)
		manager.mutex.Unlock()
		if pending != 0 {
			t.Errorf("financial close left %d sync owners after real ACK", pending)
		}
	})
}
