package connect

// The control-source rule of SUBPROTOCOL.md §11, at the client's real receive
// entry: hand-built transfer frames enter a route of the client's route
// manager, so each one takes the read loop, the receive sequence and
// Client.receive before it reaches a listener.

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// What the generic receive callback saw of one SimpleMessage.
type sourceRuleDelivery struct {
	source  TransferPath
	content string
	peer    Peer
}

// One send sequence of a source, as a sender numbers it.
type sourceRuleSequence struct {
	sourceId       Id
	sequenceId     Id
	sequenceNumber uint64
}

// A new sequence of `sourceId`, numbered from the start.
func newSourceRuleSequence(sourceId Id) *sourceRuleSequence {
	return &sourceRuleSequence{sourceId: sourceId, sequenceId: NewId()}
}

// One client and the receive route of one carrier.
type sourceRuleFixture struct {
	t          *testing.T
	client     *Client
	route      chan []byte
	deliveries chan sourceRuleDelivery
}

// Close-wait settings with 30-second ack and gap timeouts, as long as the
// fixture's waits, so neither end of a sequence gives up while a test waits.
func sourceRuleClientSettings() *ClientSettings {
	settings := closeWaitClientSettings()
	settings.SendBufferSettings.AckTimeout = 30 * time.Second
	settings.ReceiveBufferSettings.GapTimeout = 30 * time.Second
	return settings
}

// A client reached by a route of the given carrier. A client with no send route
// cannot deliver its transfer acks, and these tests assert none.
func newSourceRuleFixture(t *testing.T, transportType TransportType, configure ...func(*ClientSettings)) *sourceRuleFixture {
	t.Helper()
	settings := sourceRuleClientSettings()
	for _, change := range configure {
		change(settings)
	}
	client := NewClient(t.Context(), NewId(), NewNoContractClientOob(), settings)
	t.Cleanup(client.Cancel)
	self := &sourceRuleFixture{
		t:          t,
		client:     client,
		route:      make(chan []byte),
		deliveries: make(chan sourceRuleDelivery, 64),
	}
	client.RouteManager().UpdateTransport(NewReceiveGatewayTransportWithType(transportType), []Route{self.route})
	client.AddReceiveCallback(self.receive)
	return self
}

// Inline and non-blocking; the channel outlives every test's traffic.
func (self *sourceRuleFixture) receive(source TransferPath, frames []*protocol.Frame, peer Peer) {
	for _, frame := range frames {
		if frame.MessageType != protocol.MessageType_TestSimpleMessage {
			continue
		}
		var message protocol.SimpleMessage
		if err := proto.Unmarshal(frame.MessageBytes, &message); err != nil {
			self.t.Error(err)
			continue
		}
		select {
		case self.deliveries <- sourceRuleDelivery{source: source, content: message.Content, peer: peer}:
		default:
			self.t.Error("delivery collector overflow")
		}
	}
}

// A simple message frame carrying `content`.
func sourceRuleSimpleFrame(content string) *protocol.Frame {
	return RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: content})
}

// A pooled transfer frame holding one Pack of the sequence, taking the frames.
func sourceRulePackBytes(t *testing.T, destinationId Id, sequence *sourceRuleSequence, frames ...*protocol.Frame) []byte {
	t.Helper()
	sequenceNumber := sequence.sequenceNumber
	sequence.sequenceNumber += 1
	wire, err := ProtoMarshal(&protocol.TransferFrame{
		TransferPath: TransferPath{SourceId: sequence.sourceId, DestinationId: destinationId}.ToProtobuf(),
		Pack: &protocol.Pack{
			MessageId:      NewId().Bytes(),
			SequenceId:     sequence.sequenceId.Bytes(),
			SequenceNumber: sequenceNumber,
			Head:           sequenceNumber == 0,
			Frames:         frames,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, frame := range frames {
		MessagePoolReturn(frame.MessageBytes)
	}
	return wire
}

// Hands one Pack to the route. The client's read loop has taken it when this
// returns.
func (self *sourceRuleFixture) inject(sequence *sourceRuleSequence, frames ...*protocol.Frame) {
	self.t.Helper()
	wire := sourceRulePackBytes(self.t, self.client.ClientId(), sequence, frames...)
	select {
	case self.route <- wire:
	case <-time.After(30 * time.Second):
		self.t.Fatal("the client did not read its route")
	}
}

// Returns the next delivery, failing the test unless it carries `content` and
// arrives within 30 seconds.
func (self *sourceRuleFixture) requireDelivery(content string) sourceRuleDelivery {
	self.t.Helper()
	select {
	case delivery := <-self.deliveries:
		if delivery.content != content {
			self.t.Fatalf("delivered %q, want %q", delivery.content, content)
		}
		return delivery
	case <-time.After(30 * time.Second):
		self.t.Fatalf("%q was not delivered", content)
		return sourceRuleDelivery{}
	}
}

// Sends a fence behind what was injected on the sequence. Delivery is in order
// within a sequence, so whatever came before it has been dispatched once the
// fence arrives.
func (self *sourceRuleFixture) fence(sequence *sourceRuleSequence) sourceRuleDelivery {
	self.t.Helper()
	self.inject(sequence, sourceRuleSimpleFrame("fence"))
	return self.requireDelivery("fence")
}

// What a listener of one id received, by the listener's name.
type subprotocolSourceReceipts struct {
	stateLock        sync.Mutex
	listenerReceipts map[string][]subprotocolSourceReceipt
}

// One message a listener received, with the source and Peer it came with.
type subprotocolSourceReceipt struct {
	source  TransferPath
	payload string
	peer    Peer
}

// Receipts with nothing received yet.
func newSubprotocolSourceReceipts() *subprotocolSourceReceipts {
	return &subprotocolSourceReceipts{listenerReceipts: map[string][]subprotocolSourceReceipt{}}
}

// Adds one message to the named listener's receipts. Safe to call from the
// receive goroutine while the test reads.
func (self *subprotocolSourceReceipts) record(listener string, source TransferPath, payload []byte, peer Peer) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.listenerReceipts[listener] = append(self.listenerReceipts[listener], subprotocolSourceReceipt{
		source:  source,
		payload: string(payload),
		peer:    peer,
	})
}

// A copy of what the named listener has received so far.
func (self *subprotocolSourceReceipts) get(listener string) []subprotocolSourceReceipt {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]subprotocolSourceReceipt(nil), self.listenerReceipts[listener]...)
}

// An id with a raw listener and a typed handler, each with the given options.
func (self *subprotocolSourceReceipts) register(t *testing.T, client *Client, id SubprotocolId, opts ...SubprotocolListenerOption) {
	t.Helper()
	if _, err := client.AddSubprotocolRawCallback(id, func(source TransferPath, _ SubprotocolId, messageBytes []byte, peer Peer) {
		self.record("raw", source, messageBytes, peer)
	}, opts...); err != nil {
		t.Fatal(err)
	}
	if _, err := RegisterSubprotocol[*fixedMessage](client, id, &fixedCodec{}, func(source TransferPath, m *fixedMessage, peer Peer) {
		self.record("typed", source, m.bytes(), peer)
	}, opts...); err != nil {
		t.Fatal(err)
	}
}

const subprotocolSourceTestId SubprotocolId = 4100

// Every opt-in of SUBPROTOCOL.md §11 starts off, on every default a client is
// built from. connect_default_unchanged_test.go shows what that means in traffic.
func TestSourceRuleOptInsAreOffByDefault(t *testing.T) {
	for _, settings := range []*ClientSettings{
		DefaultClientSettings(),
		DefaultClientSettingsWithBufferSize(0),
	} {
		if settings.P2pPeerSourceOnly {
			t.Error("P2pPeerSourceOnly is on by default")
		}
		if settings.EncryptionSettings.ExposeVerifiedPeerClientKey {
			t.Error("ExposeVerifiedPeerClientKey is on by default")
		}
	}
	if DefaultEncryptionSettings().ExposeVerifiedPeerClientKey {
		t.Error("ExposeVerifiedPeerClientKey is on by default")
	}
	if subprotocolListenerAcceptsControlSource(nil) {
		t.Error("a listener with no options accepts the control id")
	}
}

// A listener that did not opt in never sees the control id's frames, and the
// drop is counted, not delivered: nothing counts as received. Top-level control
// frames in the same sequence still reach the generic callbacks.
func TestSubprotocolControlSourceDroppedWithoutOptIn(t *testing.T) {
	fixture := newSourceRuleFixture(t, TransportTypeH1)
	receipts := newSubprotocolSourceReceipts()
	receipts.register(t, fixture.client, subprotocolSourceTestId)

	control := newSourceRuleSequence(ControlId)
	fixture.inject(control, subprotocolTestFrame(subprotocolSourceTestId, []byte("hint")))
	delivery := fixture.fence(control)
	if !delivery.source.IsControlSource() {
		t.Fatalf("the fence's source is %s", delivery.source)
	}

	if raw, typed := receipts.get("raw"), receipts.get("typed"); len(raw) != 0 || len(typed) != 0 {
		t.Fatalf("a listener that did not opt in received raw=%v typed=%v", raw, typed)
	}
	stats := fixture.client.SubprotocolStats()
	if stats.DroppedControlSource != 1 || stats.Received != 0 || stats.ReceivedById[subprotocolSourceTestId] != 0 {
		t.Fatalf("stats %+v", stats)
	}
}

// §22.5's "kept over h1": an opted-in listener receives the control id's frames
// on every platform carrier, and the carrier is visible to it for D-PROTO-8's
// own guard. A listener of the same id that did not opt in stays silent.
func TestSubprotocolControlSourceReachesOptedInListenerOnPlatformCarriers(t *testing.T) {
	for _, transportType := range []TransportType{
		TransportTypeH1,
		TransportTypeH3,
		TransportTypeH3Dns,
		TransportTypeH3DnsPump,
	} {
		fixture := newSourceRuleFixture(t, transportType)
		receipts := newSubprotocolSourceReceipts()
		receipts.register(t, fixture.client, subprotocolSourceTestId, WithSubprotocolControlSource())
		var unopted []string
		if _, err := fixture.client.AddSubprotocolRawCallback(subprotocolSourceTestId, func(source TransferPath, _ SubprotocolId, messageBytes []byte, peer Peer) {
			unopted = append(unopted, string(messageBytes))
		}); err != nil {
			t.Fatal(err)
		}

		control := newSourceRuleSequence(ControlId)
		fixture.inject(control, subprotocolTestFrame(subprotocolSourceTestId, []byte("hint")))
		fixture.fence(control)

		for _, listener := range []string{"raw", "typed"} {
			received := receipts.get(listener)
			if len(received) != 1 {
				t.Fatalf("%s: %s received %v", transportType, listener, received)
			}
			if !received[0].source.IsControlSource() || received[0].payload != "hint" ||
				received[0].peer.TransportType != transportType {
				t.Fatalf("%s: %s received %+v", transportType, listener, received[0])
			}
		}
		if len(unopted) != 0 {
			t.Fatalf("%s: a listener that did not opt in received %v", transportType, unopted)
		}
		stats := fixture.client.SubprotocolStats()
		if stats.Received != 1 || stats.ReceivedById[subprotocolSourceTestId] != 1 || stats.DroppedControlSource != 0 {
			t.Fatalf("%s: stats %+v", transportType, stats)
		}
	}
}

// The opt-in is no way past the carrier: only a platform carrier delivers the
// control id, so a forged frame on a P2P route, or on a route nothing vouches
// for, is dropped from an opted-in listener too, whatever the client's other
// settings. The transport type is the carrier's, not the frame's.
func TestSubprotocolControlSourceDroppedFromOptedInListenerOffPlatformCarriers(t *testing.T) {
	for _, transportType := range []TransportType{TransportTypeP2p, TransportTypeUnknown} {
		fixture := newSourceRuleFixture(t, transportType)
		receipts := newSubprotocolSourceReceipts()
		receipts.register(t, fixture.client, subprotocolSourceTestId, WithSubprotocolControlSource())

		control := newSourceRuleSequence(ControlId)
		fixture.inject(control, subprotocolTestFrame(subprotocolSourceTestId, []byte("forged")))
		fixture.fence(control)

		if raw, typed := receipts.get("raw"), receipts.get("typed"); len(raw) != 0 || len(typed) != 0 {
			t.Fatalf("%s: forged control frame reached raw=%v typed=%v", transportType, raw, typed)
		}
		stats := fixture.client.SubprotocolStats()
		if stats.DroppedControlSource != 1 || stats.Received != 0 {
			t.Fatalf("%s: stats %+v", transportType, stats)
		}
	}
}

// The opt-in belongs to one listener, a typed handler as much as a raw one. Here
// the raw listener opted in and the handler of the same id did not.
func TestSubprotocolControlSourceOptInIsPerListener(t *testing.T) {
	fixture := newSourceRuleFixture(t, TransportTypeH1)
	receipts := newSubprotocolSourceReceipts()
	if _, err := fixture.client.AddSubprotocolRawCallback(subprotocolSourceTestId, func(source TransferPath, _ SubprotocolId, messageBytes []byte, peer Peer) {
		receipts.record("raw", source, messageBytes, peer)
	}, WithSubprotocolControlSource()); err != nil {
		t.Fatal(err)
	}
	if _, err := RegisterSubprotocol[*fixedMessage](fixture.client, subprotocolSourceTestId, &fixedCodec{}, func(source TransferPath, m *fixedMessage, peer Peer) {
		receipts.record("typed", source, m.bytes(), peer)
	}); err != nil {
		t.Fatal(err)
	}

	control := newSourceRuleSequence(ControlId)
	fixture.inject(control, subprotocolTestFrame(subprotocolSourceTestId, []byte("hint")))
	fixture.fence(control)

	if raw := receipts.get("raw"); len(raw) != 1 {
		t.Fatalf("raw received %v", raw)
	}
	if typed := receipts.get("typed"); len(typed) != 0 {
		t.Fatalf("a handler that did not opt in received %v", typed)
	}
}

// Opting in adds the control id and takes nothing away: peers' frames reach
// every listener as before.
func TestSubprotocolPeerFramesReachEveryListener(t *testing.T) {
	fixture := newSourceRuleFixture(t, TransportTypeH1)
	opted := newSubprotocolSourceReceipts()
	opted.register(t, fixture.client, subprotocolSourceTestId, WithSubprotocolControlSource())
	unopted := newSubprotocolSourceReceipts()
	unopted.register(t, fixture.client, subprotocolSourceTestId+1)

	peerId := NewId()
	fixture.client.ContractManager().AddNoContractPeer(peerId)
	sequence := newSourceRuleSequence(peerId)
	fixture.inject(sequence,
		subprotocolTestFrame(subprotocolSourceTestId, []byte("a")),
		subprotocolTestFrame(subprotocolSourceTestId+1, []byte("b")),
	)
	fixture.fence(sequence)

	for _, c := range []struct {
		receipts *subprotocolSourceReceipts
		payload  string
	}{
		{receipts: opted, payload: "a"},
		{receipts: unopted, payload: "b"},
	} {
		for _, listener := range []string{"raw", "typed"} {
			received := c.receipts.get(listener)
			if len(received) != 1 || received[0].payload != c.payload || received[0].source.SourceId != peerId {
				t.Fatalf("%s received %+v, want %q from the peer", listener, received, c.payload)
			}
		}
	}
	if stats := fixture.client.SubprotocolStats(); stats.DroppedControlSource != 0 || stats.Received != 2 {
		t.Fatalf("stats %+v", stats)
	}
}

// The query frames are peer-to-peer: the control id is not queried, so a query
// from it gets no answer, even one the client could send, and a result from it
// completes nothing. A peer's result for the same query id still does.
func TestSubprotocolQueryFramesFromControlIdAreDropped(t *testing.T) {
	fixture := newSourceRuleFixture(t, TransportTypeH1)
	receipts := newSubprotocolSourceReceipts()
	receipts.register(t, fixture.client, subprotocolSourceTestId, WithSubprotocolControlSource())

	queryId := NewId()
	pending := make(chan []SubprotocolId, 1)
	registry := fixture.client.subprotocols
	func() {
		registry.stateLock.Lock()
		defer registry.stateLock.Unlock()
		registry.pendingQueries[queryId] = pending
	}()
	result := func() *protocol.Frame {
		return RequireToFrameWithDefaultProtocolVersion(&protocol.SubprotocolsQueryResult{
			QueryId:        queryId.Bytes(),
			SubprotocolIds: []uint32{uint32(subprotocolSourceTestId)},
		})
	}

	control := newSourceRuleSequence(ControlId)
	fixture.inject(control,
		RequireToFrameWithDefaultProtocolVersion(&protocol.SubprotocolsQuery{QueryId: NewId().Bytes()}),
		result(),
	)
	fixture.fence(control)

	stats := fixture.client.SubprotocolStats()
	if stats.QueriesAnswered != 0 || stats.DroppedControlSource != 2 {
		t.Fatalf("stats %+v", stats)
	}
	select {
	case ids := <-pending:
		t.Fatalf("a control-sourced result completed a query with %v", ids)
	default:
	}

	peerId := NewId()
	fixture.client.ContractManager().AddNoContractPeer(peerId)
	sequence := newSourceRuleSequence(peerId)
	fixture.inject(sequence, result())
	fixture.fence(sequence)
	select {
	case ids := <-pending:
		if len(ids) != 1 || ids[0] != subprotocolSourceTestId {
			t.Fatalf("result %v", ids)
		}
	default:
		t.Fatal("a peer's result did not complete the query")
	}
}

// An unregistered id and a malformed wrapper count where they always have, so a
// non-opted client's existing drop stats keep their meaning for the control id.
func TestSubprotocolControlSourceDropsKeepTheirCounters(t *testing.T) {
	client := testSubprotocolClient(t)
	control := TransferPath{SourceId: ControlId}
	peer := Peer{TransportType: TransportTypeH1}

	unregistered := subprotocolTestFrame(4200, []byte("x"))
	defer MessagePoolReturn(unregistered.MessageBytes)
	malformed := &protocol.Frame{MessageType: protocol.MessageType_Subprotocol, MessageBytes: []byte{0x08}}
	if kept := client.dispatchSubprotocolFrames(control, []*protocol.Frame{unregistered, malformed}, peer); len(kept) != 0 {
		t.Fatalf("kept %v", kept)
	}
	stats := client.SubprotocolStats()
	if stats.DroppedUnregistered != 1 || stats.DroppedDecode != 1 || stats.DroppedControlSource != 0 {
		t.Fatalf("stats %+v", stats)
	}
}

// Removing one listener leaves the other's choice alone, so a table republish
// cannot widen or narrow who opted in.
func TestSubprotocolControlSourceOptInSurvivesRepublish(t *testing.T) {
	client := testSubprotocolClient(t)
	var seen [][]byte
	if _, err := client.AddSubprotocolRawCallback(4300, func(_ TransferPath, _ SubprotocolId, messageBytes []byte, _ Peer) {
		seen = append(seen, bytes.Clone(messageBytes))
	}, WithSubprotocolControlSource()); err != nil {
		t.Fatal(err)
	}
	remove, err := client.AddSubprotocolRawCallback(4300, func(TransferPath, SubprotocolId, []byte, Peer) {
		t.Error("the listener that did not opt in was called")
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatch := func(payload string) {
		frame := subprotocolTestFrame(4300, []byte(payload))
		defer MessagePoolReturn(frame.MessageBytes)
		client.dispatchSubprotocolFrames(TransferPath{SourceId: ControlId}, []*protocol.Frame{frame}, Peer{TransportType: TransportTypeH1})
	}
	dispatch("before")
	remove()
	dispatch("after")
	if len(seen) != 2 || string(seen[0]) != "before" || string(seen[1]) != "after" {
		t.Fatalf("seen %q", seen)
	}

	// a typed handler's choice is its own, and goes with it
	unregistered := testSubprotocolClient(t)
	unregister, err := RegisterSubprotocol[*fixedMessage](unregistered, 4301, &fixedCodec{}, func(TransferPath, *fixedMessage, Peer) {}, WithSubprotocolControlSource())
	if err != nil {
		t.Fatal(err)
	}
	if !unregistered.subprotocols.table.Load().entries[4301].acceptsControlSource() {
		t.Fatal("the registration did not opt in")
	}
	unregister()
	if _, ok := unregistered.subprotocols.table.Load().entries[4301]; ok {
		t.Fatal("an unregistered id kept its entry")
	}
}

// A client still refuses to send to the control id, in every send form:
// decision 6 as amended lets it receive them and never send them.
func TestSubprotocolSendToControlIdStaysRefused(t *testing.T) {
	client := testSubprotocolClient(t)
	codec := &fixedCodec{}
	m := &fixedMessage{}
	m.set([]byte("x"))
	if _, err := SendSubprotocolWithTimeout[*fixedMessage](client, 2000, m, ControlId, nil, 0, WithSubprotocolCodec[*fixedMessage](codec)); !errors.Is(err, errSubprotocolControl) {
		t.Fatalf("send = %v", err)
	}
	if _, err := client.QuerySubprotocols(context.Background(), ControlId); !errors.Is(err, errSubprotocolControl) {
		t.Fatalf("query = %v", err)
	}
	if client.SendSubprotocolBytes(2000, MessagePoolCopy([]byte("x")), ControlId, nil) {
		t.Fatal("bytes send reached the control id")
	}
}
