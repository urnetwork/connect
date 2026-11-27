// Required-mode tests share real client routes and control the delivery of complete wire messages.
package connect

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// Two production clients on in-memory platform routes. The tap borrows each
// actual route write and can withhold it before the other client's receive loop.
type tlsWirePair struct {
	t         *testing.T
	ctx       context.Context
	cancel    context.CancelFunc
	ids       [2]Id
	clients   [2]*Client
	out, in   [2]Route
	stateLock sync.Mutex
	delivered [2][]string
	events    chan struct{}
	wireLock  sync.Mutex
	accept    func(int, []byte) bool
	configure func(int, *ClientSettings)
	oob       [2]OutOfBandControl
}

func newTlsWirePair(t *testing.T, ids [2]Id, oob [2]OutOfBandControl, configure func(int, *ClientSettings)) *tlsWirePair {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	pair := &tlsWirePair{t: t, ctx: ctx, cancel: cancel, ids: ids, oob: oob, configure: configure, events: make(chan struct{}, 1)}
	for side := range 2 {
		pair.in[side], pair.out[side] = make(Route), make(Route, 32)
	}
	for side := range 2 {
		pair.start(side)
		go pair.relay(side)
	}
	return pair
}

func (self *tlsWirePair) start(side int) {
	settings := DefaultClientSettingsWithBufferSize(64)
	settings.Log = NewNoopLogger()
	settings.ClientKeySeed = bytes.Repeat([]byte{byte(31 + side)}, ed25519.SeedSize)
	settings.EncryptionSettings.Mode = EncryptionModeRequired
	settings.EncryptionSettings.EncryptionControlUseCompanion = false
	settings.EncryptionSettings.IdentityProofResendInterval = 0
	settings.EncryptionSettings.TlsTimeout = 30 * time.Second
	settings.SendBufferSettings.MinResendInterval = 100 * time.Millisecond
	settings.SendBufferSettings.IdleTimeout = time.Minute
	settings.ReceiveBufferSettings.IdleTimeout = time.Minute
	settings.ContractManagerSettings.LegacyCreateContract = false
	oob := self.oob[side]
	if oob == nil {
		oob = NewNoContractClientOob()
		settings.EncryptionSettings.NewPeerClientPublicKeyFetcher = func(peer Id) func(context.Context) ([]byte, error) {
			return func(context.Context) ([]byte, error) {
				if peer != self.ids[1-side] {
					return nil, context.Canceled
				}
				return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{byte(31 + 1 - side)}, ed25519.SeedSize)).Public().(ed25519.PublicKey), nil
			}
		}
	}
	if self.configure != nil {
		self.configure(side, settings)
	}
	client := NewClient(self.ctx, self.ids[side], oob, settings)
	self.clients[side] = client
	if self.oob[side] == nil {
		client.ContractManager().AddNoContractPeer(self.ids[1-side])
	}
	client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(self.ids[1-side])), []Route{self.out[side]})
	client.RouteManager().UpdateTransport(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{self.in[side]})
	blackholeControlId(client.Ctx(), client.RouteManager())
	client.EncryptionSessionManager().AddEncryptionEventCallback(func(*EncryptionEvent) { self.signal() })
	client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		if labels := tlsWireApplicationLabels(self.t, frames); len(labels) > 0 {
			self.stateLock.Lock()
			self.delivered[side] = append(self.delivered[side], labels...)
			self.stateLock.Unlock()
			self.signal()
		}
	})
}

func (self *tlsWirePair) deliveries(side int) []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.delivered[side])
}

func (self *tlsWirePair) setWirePolicy(accept func(int, []byte) bool) {
	self.wireLock.Lock()
	defer self.wireLock.Unlock()
	self.accept = accept
}

func (self *tlsWirePair) relay(side int) {
	for {
		select {
		case <-self.ctx.Done():
			return
		case wire := <-self.out[side]:
			self.wireLock.Lock()
			accept := self.accept == nil || self.accept(side, wire)
			self.wireLock.Unlock()
			next := self.in[1-side]
			if !accept {
				next = nil
			}
			if !forwardOwnedTestWire(self.ctx, next, wire) {
				return
			}
			self.signal()
		}
	}
}

func (self *tlsWirePair) close() {
	self.cancel()
	for _, client := range self.clients {
		if err := client.CloseAndWait(context.Background()); err != nil {
			self.t.Error(err)
		}
	}
	synctest.Wait()
	for _, out := range self.out {
		for len(out) > 0 {
			MessagePoolReturn(<-out)
		}
	}
}

// Decode for observation only. Protobuf owns these copies independently of the
// route buffer. A clear control, an encrypted record and a clear data Pack are distinct.
func tlsWireContents(wire []byte) (*protocol.Pack, []byte) {
	var envelope protocol.TransferFrame
	if err := proto.Unmarshal(wire, &envelope); err != nil {
		panic(err)
	}
	if f := envelope.GetFrame(); f != nil && f.MessageType == protocol.MessageType_TransferPack {
		pack := &protocol.Pack{}
		if err := proto.Unmarshal(f.MessageBytes, pack); err != nil {
			panic(err)
		}
		return pack, nil
	}
	return envelope.GetPack(), envelope.GetEncryptedTransferFrame()
}

func tlsWireControl(wire []byte, kind protocol.EncryptedControlType) *protocol.EncryptedControl {
	pack, _ := tlsWireContents(wire)
	for _, f := range pack.GetFrames() {
		if f.MessageType == protocol.MessageType_TransferEncryptedControl {
			var ec protocol.EncryptedControl
			if err := proto.Unmarshal(f.MessageBytes, &ec); err != nil {
				panic(err)
			}
			if ec.ControlType == kind {
				return &ec
			}
		}
	}
	return nil
}

func (self *tlsWirePair) signal() {
	select {
	case self.events <- struct{}{}:
	default:
	}
}

// Wait for a route or lifecycle notification and check at quiescence. The
// virtual deadline only bounds a missing event. Tests order steps by barriers.
func (self *tlsWirePair) await(what string, ready func() bool) {
	self.t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		synctest.Wait()
		self.wireLock.Lock()
		done := ready()
		self.wireLock.Unlock()
		if done {
			return
		}
		select {
		case <-self.events:
		case <-deadline.C:
			self.t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func tlsWireApplicationLabels(t *testing.T, frames []*protocol.Frame) []string {
	t.Helper()
	var labels []string
	for _, frame := range frames {
		if frame.MessageType != protocol.MessageType_TestSimpleMessage {
			continue
		}
		message, err := FromFrame(frame)
		if err != nil {
			t.Error(err)
			continue
		}
		labels = append(labels, message.(*protocol.SimpleMessage).Content)
	}
	return labels
}

func (self *tlsWirePair) waitResult(what string, done <-chan error) error {
	self.t.Helper()
	select {
	case err := <-done:
		synctest.Wait()
		return err
	case <-time.After(5 * time.Second):
		self.t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

// Reports a reliable receipt or a no-ACK write disposition.
func (self *tlsWirePair) send(label string, opts ...any) <-chan error {
	frame := requiredGateFrame(self.t, label)
	receipt := make(chan error, 2)
	go func() {
		ok, err := self.clients[0].SendWithTimeoutDetailed(frame, self.ids[1], func(err error) { receipt <- err; self.signal() }, -1, opts...)
		if !ok || err != nil {
			MessagePoolReturn(frame.MessageBytes)
			receipt <- fmt.Errorf("send %q was refused: %v", label, err)
			self.signal()
		}
	}()
	return receipt
}
