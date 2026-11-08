package connect

// C9: `ProvideMode_Service`, the relationship of a platform edge's streams. A
// Service source reaches the subprotocols its receiver lists and nothing else: no
// IP, no generic frame, no query (sidecar DESIGN.md §6.10.1). These run two
// clients over the in-process gateways under hand-fed contracts, unless a test
// says otherwise, so a frame takes the real path through contract verification
// and the receive sequence.
//
// The audit behind this file read every `ProvideMode_` site in connect for a default
// that would widen Service:
//
//   - Fell to the public rules, so changed: `egressRelationship` and the policy's
//     egress, group and ingress paths.
//   - Would have moved a default client, so changed: `InitProvideSecretKeys`, which
//     ranges the enum and whose keys the sdk saves, and `observePeer`, which would
//     open a Stream reply toward a Service peer.
//   - A range over the enum: the original contract evidence, which now names its
//     modes and still refuses Service.
//   - Already narrower, so left: provide pings, inbound provider streams, `Verify`
//     and `provideFrame` under a pause, roles and principal, P2P priority.
//   - Past the gate, so left: the provider's return-mode cache and reply keys, and
//     the query reply's Stream fallback.
//   - Stamped locally, so left: the multi client's synthesized packets, its window
//     clients' provide modes, the loopback peer and connectctl.

import (
	"context"
	"encoding/hex"
	"maps"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

const (
	serviceModeListedId   SubprotocolId = 4600
	serviceModeUnlistedId SubprotocolId = 4601
	// long enough for a loaded machine: a healthy path takes milliseconds
	serviceModeTestTimeout = 30 * time.Second
)

// What the generic callbacks of the receiver got.
type serviceModeGenericFrames struct {
	stateLock    sync.Mutex
	messageTypes []protocol.MessageType
}

func (self *serviceModeGenericFrames) receive(source TransferPath, frames []*protocol.Frame, peer Peer) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, frame := range frames {
		self.messageTypes = append(self.messageTypes, frame.MessageType)
	}
}

func (self *serviceModeGenericFrames) received() []protocol.MessageType {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.messageTypes)
}

// A delivery to a raw subprotocol listener.
type serviceModeDelivery struct {
	id      SubprotocolId
	payload string
	peer    Peer
}

// Two clients where a sends to b under a contract of one mode.
type serviceModeFixture struct {
	t       *testing.T
	ctx     context.Context
	a       *Client
	b       *Client
	generic *serviceModeGenericFrames
	// a delivery on either listed or unlisted id at b
	deliveries chan serviceModeDelivery
}

func newServiceModeSettings(serviceSubprotocolIds []SubprotocolId) *ClientSettings {
	settings := DefaultClientSettingsWithBufferSize(64)
	settings.SendBufferSettings.AckTimeout = 300 * time.Second
	settings.SendBufferSettings.IdleTimeout = 300 * time.Second
	settings.ReceiveBufferSettings.GapTimeout = 300 * time.Second
	settings.ReceiveBufferSettings.IdleTimeout = 300 * time.Second
	settings.ForwardBufferSettings.IdleTimeout = 300 * time.Second
	settings.ContractManagerSettings.LegacyCreateContract = true
	settings.ServiceSubprotocolIds = serviceSubprotocolIds
	applyTestEncryptionSettings(settings, encryptionModeOff)
	return settings
}

// Joins two clients over gateways that run until ctx ends.
func joinServiceModeClients(ctx context.Context, aSettings *ClientSettings, bSettings *ClientSettings) (*Client, *Client) {
	aSend := make(chan []byte)
	bSend := make(chan []byte)
	_, bReceive := newConditioner(ctx, aSend)
	_, aReceive := newConditioner(ctx, bSend)

	a := NewClient(ctx, NewId(), NewNoContractClientOob(), aSettings)
	a.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{aSend})
	a.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{aReceive})

	b := NewClient(ctx, NewId(), NewNoContractClientOob(), bSettings)
	b.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{bSend})
	b.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{bReceive})
	return a, b
}

// Feeds from a contract of the mode to, signed with to's key for it, as the
// platform would.
func feedServiceModeContracts(t *testing.T, from *Client, to *Client, mode protocol.ProvideMode) {
	t.Helper()
	for range 2 {
		err := from.ContractManager().HandleControlFrame(
			ContractKey{Destination: DestinationId(to.ClientId())},
			requireContractResult(
				mode,
				to.ContractManager().RequireProvideSecretKey(mode),
				from.ClientId(),
				to.ClientId(),
			),
		)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func newServiceModeFixture(t *testing.T, mode protocol.ProvideMode, serviceSubprotocolIds []SubprotocolId) *serviceModeFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	a, b := joinServiceModeClients(ctx, newServiceModeSettings(nil), newServiceModeSettings(serviceSubprotocolIds))
	t.Cleanup(a.Cancel)
	t.Cleanup(b.Cancel)
	b.ContractManager().SetProvideModes(map[protocol.ProvideMode]bool{mode: true})
	feedServiceModeContracts(t, a, b, mode)

	self := &serviceModeFixture{
		t:          t,
		ctx:        ctx,
		a:          a,
		b:          b,
		generic:    &serviceModeGenericFrames{},
		deliveries: make(chan serviceModeDelivery, 64),
	}
	b.AddReceiveCallback(self.generic.receive)
	for _, id := range []SubprotocolId{serviceModeListedId, serviceModeUnlistedId} {
		if _, err := b.AddSubprotocolRawCallback(id, func(source TransferPath, id SubprotocolId, messageBytes []byte, peer Peer) {
			self.deliveries <- serviceModeDelivery{id: id, payload: string(messageBytes), peer: peer}
		}); err != nil {
			t.Fatal(err)
		}
	}
	return self
}

// Sends one frame from a and returns once b has acknowledged it, which b does
// only after every callback of that frame's batch has returned. A later check that
// a frame was not delivered is therefore final, with nothing to wait out.
func (self *serviceModeFixture) sendAcked(frame *protocol.Frame) {
	self.t.Helper()
	acked := make(chan error, 1)
	if !self.a.SendWithTimeout(frame, self.b.ClientId(), func(err error) { acked <- err }, serviceModeTestTimeout) {
		MessagePoolReturn(frame.MessageBytes)
		self.t.Fatal("the send was refused")
	}
	select {
	case err := <-acked:
		if err != nil {
			self.t.Fatalf("ack: %v", err)
		}
	case <-time.After(serviceModeTestTimeout):
		self.t.Fatal("the frame was not acknowledged: the sequence did not survive it")
	}
}

func (self *serviceModeFixture) requireDelivery(id SubprotocolId, payload string) Peer {
	self.t.Helper()
	select {
	case delivery := <-self.deliveries:
		if delivery.id != id || delivery.payload != payload {
			self.t.Fatalf("delivered %d %q, want %d %q", delivery.id, delivery.payload, id, payload)
		}
		return delivery.peer
	case <-time.After(serviceModeTestTimeout):
		self.t.Fatalf("%q was not delivered", payload)
		return Peer{}
	}
}

func (self *serviceModeFixture) requireNoDelivery() {
	self.t.Helper()
	select {
	case delivery := <-self.deliveries:
		self.t.Fatalf("delivered %d %q", delivery.id, delivery.payload)
	default:
	}
}

func (self *serviceModeFixture) requireNothingGeneric() {
	self.t.Helper()
	if received := self.generic.received(); 0 < len(received) {
		self.t.Fatalf("the generic callbacks got %v", received)
	}
}

func serviceModeSubprotocolFrame(id SubprotocolId, payload string) *protocol.Frame {
	return &protocol.Frame{
		MessageType:  protocol.MessageType_Subprotocol,
		MessageBytes: wrapSubprotocolBytes(id, []byte(payload)),
	}
}

// A packet a provider would dial out for: a TCP SYN to a public destination.
func serviceModeTcpPacket() []byte {
	return MessagePoolCopy(dotTestSyn(
		netip.MustParseAddr("192.0.2.1"),
		netip.MustParseAddr("198.51.100.1"),
		7000,
	))
}

// Every IP frame type, in the raw form a v2 client sends and the wrapped form.
func serviceModeIpFrames(t *testing.T) []*protocol.Frame {
	t.Helper()
	toProviderRaw, err := ipPacketToProviderFrame(serviceModeTcpPacket(), DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	toProviderWrapped, err := ipPacketToProviderFrame(serviceModeTcpPacket(), 1)
	if err != nil {
		t.Fatal(err)
	}
	fromProviderRaw, err := ipPacketFromProviderFrame(serviceModeTcpPacket(), DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	ping, err := ToFrame(&protocol.IpPing{}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err := ToFrame(&protocol.IpProviderDiagnostics{}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	return []*protocol.Frame{toProviderRaw, toProviderWrapped, fromProviderRaw, ping, diagnostics}
}

// A Service source's IP frames are dropped and counted by reason, and the
// sequence carries on: an IP frame is not a reason to tear down the listed traffic
// behind it. A provider on the receiver stays untouched, TCP included, whose
// delivery receipt would cancel the sequence if the frame reached delivery
// unclaimed.
func TestServiceSourceIpFramesAreDroppedAndCounted(t *testing.T) {
	fixture := newServiceModeFixture(t, protocol.ProvideMode_Service, []SubprotocolId{serviceModeListedId})
	localUserNat := NewLocalUserNatWithDefaults(fixture.ctx, "service mode")
	t.Cleanup(localUserNat.Close)
	provider := NewRemoteUserNatProviderWithDefaults(fixture.b, localUserNat)
	t.Cleanup(provider.Close)

	frames := serviceModeIpFrames(t)
	for _, frame := range frames {
		fixture.sendAcked(frame)
	}
	fixture.sendAcked(serviceModeSubprotocolFrame(serviceModeListedId, "after"))
	fixture.requireDelivery(serviceModeListedId, "after")

	fixture.requireNothingGeneric()
	stats := fixture.b.SubprotocolStats()
	AssertEqual(t, stats.DroppedServiceIp, uint64(len(frames)))
	AssertEqual(t, stats.DroppedServiceSubprotocol, uint64(0))
	AssertEqual(t, stats.DroppedServiceOther, uint64(0))

	packetStats := provider.PacketStats()
	AssertEqual(t, packetStats.RemoteIngressPacketCount, int64(0))
	AssertEqual(t, packetStats.BlockIngressPacketCount, int64(0))
	provider.stateLock.Lock()
	sources := len(provider.sourceProvideMode)
	provider.stateLock.Unlock()
	AssertEqual(t, sources, 0)
}

// An id the receiver does not list is dropped, though a listener is registered
// for it: the list, not the registry, decides what a Service source reaches.
func TestServiceSourceUnlistedSubprotocolIsDropped(t *testing.T) {
	fixture := newServiceModeFixture(t, protocol.ProvideMode_Service, []SubprotocolId{serviceModeListedId})

	fixture.sendAcked(serviceModeSubprotocolFrame(serviceModeUnlistedId, "unlisted"))
	fixture.requireNoDelivery()
	fixture.requireNothingGeneric()

	stats := fixture.b.SubprotocolStats()
	AssertEqual(t, stats.DroppedServiceSubprotocol, uint64(1))
	AssertEqual(t, stats.Received, uint64(0))
	AssertEqual(t, stats.ReceivedById[serviceModeUnlistedId], uint64(0))
}

// A listed id reaches every listener, raw and typed, with the bytes as sent and
// the peer's mode. The receiver can act on the relationship; Service carries no
// roles or principal.
func TestServiceSourceListedSubprotocolIsDelivered(t *testing.T) {
	fixture := newServiceModeFixture(t, protocol.ProvideMode_Service, []SubprotocolId{serviceModeListedId})
	typed := make(chan string, 1)
	if _, err := RegisterSubprotocol[*fixedMessage](fixture.b, serviceModeListedId, &fixedCodec{}, func(source TransferPath, message *fixedMessage, peer Peer) {
		if source.SourceId == fixture.a.ClientId() && peer.ProvideMode == protocol.ProvideMode_Service {
			typed <- string(message.bytes())
		}
	}); err != nil {
		t.Fatal(err)
	}

	fixture.sendAcked(serviceModeSubprotocolFrame(serviceModeListedId, "listed"))

	peer := fixture.requireDelivery(serviceModeListedId, "listed")
	AssertEqual(t, peer.ProvideMode, protocol.ProvideMode_Service)
	AssertEqual(t, len(peer.Roles), 0)
	AssertEqual(t, peer.Principal, "")
	select {
	case got := <-typed:
		AssertEqual(t, got, "listed")
	default:
		t.Fatal("the typed handler was not called")
	}
	fixture.requireNothingGeneric()

	stats := fixture.b.SubprotocolStats()
	AssertEqual(t, stats.ReceivedById[serviceModeListedId], uint64(1))
	AssertEqual(t, stats.DroppedServiceSubprotocol, uint64(0))
}

// With the settings a client starts from, a Service source reaches nothing, even
// at a client that provides Service. This is C9's default-off pin, with
// TestServiceModeIsRefusedByAClientThatDoesNotProvideIt.
func TestServiceSourceDefaultSettingsPassNothing(t *testing.T) {
	if ids := DefaultClientSettings().ServiceSubprotocolIds; len(ids) != 0 {
		t.Fatalf("the default lists %v", ids)
	}
	fixture := newServiceModeFixture(t, protocol.ProvideMode_Service, DefaultClientSettings().ServiceSubprotocolIds)

	fixture.sendAcked(serviceModeSubprotocolFrame(serviceModeListedId, "listed nowhere"))
	fixture.sendAcked(requiredGateFrame(t, "generic"))
	for _, frame := range serviceModeIpFrames(t) {
		fixture.sendAcked(frame)
	}
	fixture.requireNoDelivery()
	fixture.requireNothingGeneric()

	stats := fixture.b.SubprotocolStats()
	AssertEqual(t, stats.Received, uint64(0))
	AssertEqual(t, stats.DroppedServiceSubprotocol, uint64(1))
	AssertEqual(t, stats.DroppedServiceOther, uint64(1))
	AssertEqual(t, stats.DroppedServiceIp, uint64(5))
}

// A client that does not provide Service refuses the contract itself, so its
// sequence never opens: the mode needs its key, and no default client holds one.
func TestServiceModeIsRefusedByAClientThatDoesNotProvideIt(t *testing.T) {
	fixture := newServiceModeFixture(t, protocol.ProvideMode_Network, []SubprotocolId{serviceModeListedId})
	contractManager := fixture.b.ContractManager()

	if _, ok := contractManager.GetProvideSecretKey(protocol.ProvideMode_Service); ok {
		t.Fatal("a client that does not provide Service has a key for it")
	}
	storedContractBytes, err := proto.Marshal(&protocol.StoredContract{ContractId: NewId().Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	signingKey := contractManager.RequireProvideSecretKey(protocol.ProvideMode_Network)
	hmac := SignStoredContract(DefaultContractManagerSettings(), signingKey, storedContractBytes)
	if contractManager.Verify(hmac, storedContractBytes, protocol.ProvideMode_Service) {
		t.Fatal("a Service contract verified at a client that does not provide it")
	}
	if !contractManager.Verify(hmac, storedContractBytes, protocol.ProvideMode_Network) {
		t.Fatal("the control contract did not verify")
	}
}

// Everything else a Service source might send stops at the same gate: a generic
// frame, and the queries, which would otherwise be answered and show what this
// client speaks.
func TestServiceSourceOtherFramesAreDropped(t *testing.T) {
	fixture := newServiceModeFixture(t, protocol.ProvideMode_Service, []SubprotocolId{serviceModeListedId})

	query, err := ToFrame(&protocol.SubprotocolsQuery{QueryId: NewId().Bytes()}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	queryResult, err := ToFrame(&protocol.SubprotocolsQueryResult{QueryId: NewId().Bytes()}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	signals, err := ToFrame(&protocol.ExchangeSignals{StreamId: NewId().Bytes()}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	frames := []*protocol.Frame{requiredGateFrame(t, "generic"), query, queryResult, signals}
	for _, frame := range frames {
		fixture.sendAcked(frame)
	}
	fixture.requireNothingGeneric()

	stats := fixture.b.SubprotocolStats()
	AssertEqual(t, stats.DroppedServiceOther, uint64(len(frames)))
	AssertEqual(t, stats.QueriesAnswered, uint64(0))
}

// A wrapper that does not decode names no id, so it cannot be listed.
func TestServiceSourceMalformedSubprotocolFrameIsDropped(t *testing.T) {
	fixture := newServiceModeFixture(t, protocol.ProvideMode_Service, []SubprotocolId{serviceModeListedId})

	malformed := MessagePoolGet(2)
	malformed[0], malformed[1] = 0xff, 0xff
	fixture.sendAcked(&protocol.Frame{MessageType: protocol.MessageType_Subprotocol, MessageBytes: malformed})
	fixture.requireNoDelivery()

	stats := fixture.b.SubprotocolStats()
	AssertEqual(t, stats.DroppedServiceSubprotocol, uint64(1))
	AssertEqual(t, stats.DroppedDecode, uint64(0))
}

// A no-acknowledgement Pack skips the sequence's ordering and takes another
// receive path to the same gate.
func TestServiceSourceNoAckFramesAreFiltered(t *testing.T) {
	fixture := newServiceModeFixture(t, protocol.ProvideMode_Service, []SubprotocolId{serviceModeListedId})
	// a sender holds no-acknowledgement traffic off a contract its receiver has
	// not acknowledged, so this introduces it
	fixture.sendAcked(serviceModeSubprotocolFrame(serviceModeListedId, "introduce"))
	fixture.requireDelivery(serviceModeListedId, "introduce")

	frames := []*protocol.Frame{
		serviceModeSubprotocolFrame(serviceModeUnlistedId, "unlisted"),
		serviceModeSubprotocolFrame(serviceModeListedId, "listed"),
	}
	for _, frame := range frames {
		if !fixture.a.SendWithTimeout(frame, fixture.b.ClientId(), func(error) {}, serviceModeTestTimeout, NoAck()) {
			t.Fatal("the send was refused")
		}
	}
	fixture.requireDelivery(serviceModeListedId, "listed")
	if !waitForCondition(serviceModeTestTimeout, func() bool {
		return fixture.b.SubprotocolStats().DroppedServiceSubprotocol == 1
	}) {
		t.Fatalf("stats %+v", fixture.b.SubprotocolStats())
	}
	fixture.requireNoDelivery()
}

// Service is the relationship in both directions, and Required sessions rise over
// it: their handshake frames are the transfer layer's, so the gate never sees them.
func TestServiceModeCarriesARequiredSessionBothWays(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*serviceModeTestTimeout)
	defer cancel()

	var a, b *Client
	newSettings := func(peer func() *Client) *ClientSettings {
		settings := newServiceModeSettings([]SubprotocolId{serviceModeListedId})
		settings.EncryptionSettings.Mode = EncryptionModeRequired
		settings.EncryptionSettings.TlsTimeout = serviceModeTestTimeout
		// a hand-fed contract carries no identity, and no companion follows it
		settings.EncryptionSettings.EncryptionControlUseCompanion = false
		settings.EncryptionSettings.NewPeerClientPublicKeyFetcher = func(peerId Id) func(ctx context.Context) ([]byte, error) {
			return func(ctx context.Context) ([]byte, error) {
				return peer().ClientKeyManager().PublicKey(), nil
			}
		}
		return settings
	}
	a, b = joinServiceModeClients(
		ctx,
		newSettings(func() *Client { return b }),
		newSettings(func() *Client { return a }),
	)
	defer a.Cancel()
	defer b.Cancel()
	provideModes := map[protocol.ProvideMode]bool{protocol.ProvideMode_Service: true}
	a.ContractManager().SetProvideModes(provideModes)
	b.ContractManager().SetProvideModes(provideModes)
	feedServiceModeContracts(t, a, b, protocol.ProvideMode_Service)
	feedServiceModeContracts(t, b, a, protocol.ProvideMode_Service)

	receives := map[*Client]chan Peer{a: make(chan Peer, 4), b: make(chan Peer, 4)}
	for client, receive := range receives {
		if _, err := client.AddSubprotocolRawCallback(serviceModeListedId, func(_ TransferPath, _ SubprotocolId, _ []byte, peer Peer) {
			receive <- peer
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, direction := range []struct{ from, to *Client }{{from: a, to: b}, {from: b, to: a}} {
		if !direction.from.SendWithTimeout(
			serviceModeSubprotocolFrame(serviceModeListedId, "sealed"),
			direction.to.ClientId(),
			func(error) {},
			serviceModeTestTimeout,
		) {
			t.Fatal("the send was refused")
		}
		select {
		case peer := <-receives[direction.to]:
			AssertEqual(t, peer.ProvideMode, protocol.ProvideMode_Service)
		case <-ctx.Done():
			t.Fatal("no delivery: the session did not rise over Service")
		}
	}
	waitForSealedSession(t, ctx, a)
	waitForSealedSession(t, ctx, b)
	AssertEqual(t, a.SubprotocolStats().DroppedServiceOther, uint64(0))
	AssertEqual(t, b.SubprotocolStats().DroppedServiceOther, uint64(0))
}

// The gate keeps the frames in order, never writes the batch the receive item
// still owns, and costs a healthy batch nothing.
func TestAdmitServiceFramesKeepsTheListedInOrder(t *testing.T) {
	client := testSubprotocolClient(t)
	client.settings.ServiceSubprotocolIds = []SubprotocolId{serviceModeListedId}

	first := subprotocolTestFrame(serviceModeListedId, []byte("first"))
	second := subprotocolTestFrame(serviceModeListedId, []byte("second"))
	unlisted := subprotocolTestFrame(serviceModeUnlistedId, []byte("unlisted"))
	simple := requiredGateFrame(t, "generic")

	cases := []struct {
		frames []*protocol.Frame
		want   []*protocol.Frame
	}{
		{frames: nil, want: nil},
		{frames: []*protocol.Frame{first, second}, want: []*protocol.Frame{first, second}},
		{frames: []*protocol.Frame{unlisted, first, simple, second}, want: []*protocol.Frame{first, second}},
		{frames: []*protocol.Frame{first, nil, second}, want: []*protocol.Frame{first, second}},
		{frames: []*protocol.Frame{first, unlisted}, want: []*protocol.Frame{first}},
		{frames: []*protocol.Frame{unlisted, simple}, want: []*protocol.Frame{}},
	}
	for i, c := range cases {
		before := slices.Clone(c.frames)
		got := client.admitServiceFrames(c.frames)
		if !slices.Equal(got, c.want) {
			t.Errorf("case %d kept %d frames, want %d", i, len(got), len(c.want))
		}
		if !slices.Equal(c.frames, before) {
			t.Errorf("case %d wrote the batch", i)
		}
	}

	healthy := []*protocol.Frame{first, second}
	if allocs := testing.AllocsPerRun(100, func() {
		client.admitServiceFrames(healthy)
	}); allocs != 0 {
		t.Fatalf("a healthy batch allocated %v times", allocs)
	}
}

// The other modes never meet the gate: with a list set, each still delivers
// generic frames and every subprotocol, unlisted included, and counts nothing.
func TestOtherProvideModesAreNotFiltered(t *testing.T) {
	for _, mode := range []protocol.ProvideMode{
		protocol.ProvideMode_Network,
		protocol.ProvideMode_FriendsAndFamily,
		protocol.ProvideMode_Public,
		protocol.ProvideMode_Stream,
		protocol.ProvideMode_PublicStream,
	} {
		fixture := newServiceModeFixture(t, mode, []SubprotocolId{serviceModeListedId})

		fixture.sendAcked(serviceModeSubprotocolFrame(serviceModeUnlistedId, "unlisted"))
		peer := fixture.requireDelivery(serviceModeUnlistedId, "unlisted")
		AssertEqual(t, peer.ProvideMode, mode)
		fixture.sendAcked(requiredGateFrame(t, "generic"))
		frames := serviceModeIpFrames(t)
		for _, frame := range frames {
			fixture.sendAcked(frame)
		}

		want := []protocol.MessageType{protocol.MessageType_TestSimpleMessage}
		for _, frame := range frames {
			want = append(want, frame.MessageType)
		}
		if got := fixture.generic.received(); !slices.Equal(got, want) {
			t.Fatalf("%v: generic frames %v, want %v", mode, got, want)
		}
		stats := fixture.b.SubprotocolStats()
		AssertEqual(t, stats.DroppedServiceIp+stats.DroppedServiceSubprotocol+stats.DroppedServiceOther, uint64(0))
	}
}

// Other modes stay byte-identical on the wire: their numbers, and the bytes of a
// contract and a provide key naming each, as they were before Service.
func TestProvideModeWireBytesAreUnchanged(t *testing.T) {
	cases := []struct {
		mode   protocol.ProvideMode
		number int32
		name   string
		// a `Contract` naming only the mode
		contract string
		// a `ProvideKey` naming only the mode
		provideKey string
	}{
		{mode: protocol.ProvideMode_None, number: 0, name: "None", contract: "", provideKey: ""},
		{mode: protocol.ProvideMode_Network, number: 1, name: "Network", contract: "1801", provideKey: "0801"},
		{mode: protocol.ProvideMode_FriendsAndFamily, number: 2, name: "FriendsAndFamily", contract: "1802", provideKey: "0802"},
		{mode: protocol.ProvideMode_Public, number: 3, name: "Public", contract: "1803", provideKey: "0803"},
		{mode: protocol.ProvideMode_Stream, number: 4, name: "Stream", contract: "1804", provideKey: "0804"},
		{mode: protocol.ProvideMode_PublicStream, number: 5, name: "PublicStream", contract: "1805", provideKey: "0805"},
		{mode: protocol.ProvideMode_Service, number: 6, name: "Service", contract: "1806", provideKey: "0806"},
	}
	AssertEqual(t, len(protocol.ProvideMode_name), len(cases))
	for _, c := range cases {
		AssertEqual(t, int32(c.mode), c.number)
		AssertEqual(t, c.mode.String(), c.name)
		contract, err := proto.Marshal(&protocol.Contract{ProvideMode: c.mode})
		if err != nil {
			t.Fatal(err)
		}
		AssertEqual(t, hex.EncodeToString(contract), c.contract)
		provideKey, err := proto.Marshal(&protocol.ProvideKey{Mode: c.mode})
		if err != nil {
			t.Fatal(err)
		}
		AssertEqual(t, hex.EncodeToString(provideKey), c.provideKey)
	}
}

// Provide modes are chosen by name, and a client holds no Service key until it
// chooses Service: the keys an owner saves, and the frame it registers them with,
// stay as they were. Provide secret keys are persisted by the sdk.
func TestDefaultProvideSecretKeysOmitService(t *testing.T) {
	client := testSubprotocolClient(t)
	contractManager := client.ContractManager()
	contractManager.InitProvideSecretKeys()

	got := slices.Sorted(maps.Keys(contractManager.GetProvideSecretKeys()))
	want := []protocol.ProvideMode{
		protocol.ProvideMode_None,
		protocol.ProvideMode_Network,
		protocol.ProvideMode_FriendsAndFamily,
		protocol.ProvideMode_Public,
		protocol.ProvideMode_Stream,
		protocol.ProvideMode_PublicStream,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("keys %v, want %v", got, want)
	}

	contractManager.SetProvideModes(map[protocol.ProvideMode]bool{protocol.ProvideMode_Service: true})
	if key, ok := contractManager.GetProvideSecretKey(protocol.ProvideMode_Service); !ok || len(key) == 0 {
		t.Fatal("providing Service made no key for it")
	}
}

// Providing Service admits no inbound provider stream: a Service pair stays on the
// relay, where the platform sees the contract it issued.
func TestServiceModeGrantsNoProviderStream(t *testing.T) {
	provideModes := map[protocol.ProvideMode]bool{protocol.ProvideMode_Service: true}
	allowAny, allowNetwork := inboundProviderStreamPolicy(provideModes, false)
	if allowAny || allowNetwork {
		t.Fatalf("allowAny=%t allowNetwork=%t, want neither", allowAny, allowNetwork)
	}
}

// A pause leaves only return traffic and network peers, as before Service.
func TestServiceModeIsRefusedWhileProvidePaused(t *testing.T) {
	client := testSubprotocolClient(t)
	contractManager := client.ContractManager()
	contractManager.SetProvideModes(map[protocol.ProvideMode]bool{protocol.ProvideMode_Service: true})
	storedContractBytes, err := proto.Marshal(&protocol.StoredContract{ContractId: NewId().Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	key := contractManager.RequireProvideSecretKey(protocol.ProvideMode_Service)
	hmac := SignStoredContract(DefaultContractManagerSettings(), key, storedContractBytes)
	if !contractManager.Verify(hmac, storedContractBytes, protocol.ProvideMode_Service) {
		t.Fatal("the contract did not verify")
	}
	contractManager.SetProvidePaused(true)
	if contractManager.Verify(hmac, storedContractBytes, protocol.ProvideMode_Service) {
		t.Fatal("a Service contract verified while paused")
	}
}

// A provider refuses a Service source itself, whatever policy it runs. The client
// stops these frames first, so this calls the provider directly: its policy here
// is the disabled one, which admits anything. A Public source through the same
// call does reach the provider's source state, so the empty state means a refusal.
func TestProviderRefusesAServiceSourceUnderAnyPolicy(t *testing.T) {
	for _, generator := range []func(context.Context, *SecurityPolicyStatsCollector) SecurityPolicy{
		DefaultSecurityPolicyWithStats,
		DisableSecurityPolicyWithStats,
	} {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), newServiceModeSettings(nil))
		defer client.Cancel()
		localUserNat := NewLocalUserNatWithDefaults(ctx, "service mode")
		defer localUserNat.Close()
		settings := DefaultRemoteUserNatProviderSettings()
		settings.SecurityPolicyGenerator = generator
		provider := NewRemoteUserNatProvider(client, localUserNat, settings)
		defer provider.Close()

		source := TransferPath{SourceId: NewId(), DestinationId: client.ClientId()}
		frames := serviceModeIpFrames(t)
		var byteCount int64
		for _, frame := range frames {
			byteCount += int64(len(frame.MessageBytes))
		}
		provider.ClientReceive(source, frames, Peer{ProvideMode: protocol.ProvideMode_Service})
		// a receive callback borrows its frames, so they are still this test's
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}

		packetStats := provider.PacketStats()
		AssertEqual(t, packetStats.RemoteIngressPacketCount, int64(0))
		AssertEqual(t, packetStats.BlockIngressPacketCount, int64(len(frames)))
		AssertEqual(t, packetStats.BlockIngressByteCount, byteCount)
		AssertEqual(t, client.SubprotocolStats().DroppedServiceIp, uint64(len(frames)))
		sourceProvideModeCount := func() int {
			provider.stateLock.Lock()
			defer provider.stateLock.Unlock()
			return len(provider.sourceProvideMode)
		}
		AssertEqual(t, sourceProvideModeCount(), 0)

		provider.ClientReceive(source, nil, Peer{ProvideMode: protocol.ProvideMode_Public})
		AssertEqual(t, sourceProvideModeCount(), 1)
	}
}

// Service on either side stays Service, whatever the other side is. Falling to
// Public would hand it the public rules, which admit public destinations.
func TestEgressRelationshipKeepsService(t *testing.T) {
	for _, other := range []protocol.ProvideMode{
		protocol.ProvideMode_None,
		protocol.ProvideMode_Network,
		protocol.ProvideMode_FriendsAndFamily,
		protocol.ProvideMode_Public,
		protocol.ProvideMode_Stream,
		protocol.ProvideMode_PublicStream,
		protocol.ProvideMode_Service,
	} {
		AssertEqual(t, egressRelationship(protocol.ProvideMode_Service, other), protocol.ProvideMode_Service)
		AssertEqual(t, egressRelationship(other, protocol.ProvideMode_Service), protocol.ProvideMode_Service)
	}
}

// Every way into the built-in policy refuses Service, in each direction, where the
// same destination passes for a Public source. The verdict is an incident, since
// the local bypass and an override can still route a drop.
func TestSecurityPolicyRefusesAServiceSource(t *testing.T) {
	forEachIpVersion(t, func(t *testing.T, ipVersion int) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		policy := DefaultSecurityPolicy(ctx)
		provider := Reverse(DefaultSecurityPolicy(ctx))
		path := testEgressPath(ipVersion, testPublicResolver(ipVersion))
		const service = protocol.ProvideMode_Service

		requireRefused := func(entry string, result SecurityPolicyResult, err error) {
			t.Helper()
			if err != nil {
				t.Fatalf("%s: %v", entry, err)
			}
			if result != SecurityPolicyResultIncident {
				t.Errorf("%s = %v, want incident", entry, result)
			}
		}

		if r, err := policy.InspectEgress(protocol.ProvideMode_Public, path, nil); err != nil || r != SecurityPolicyResultAllow {
			t.Fatalf("the control passed %v, %v: this destination must pass for Public", r, err)
		}
		if r, err := policy.InspectIngress(protocol.ProvideMode_Public, path, nil); err != nil || r != SecurityPolicyResultAllow {
			t.Fatalf("the ingress control passed %v, %v", r, err)
		}

		r, err := policy.InspectEgress(service, path, nil)
		requireRefused("InspectEgress", r, err)
		r, err = policy.InspectIngress(service, path, nil)
		requireRefused("InspectIngress", r, err)
		r, err = provider.InspectEgress(service, path, nil)
		requireRefused("reversed InspectEgress", r, err)
		r, err = provider.InspectIngress(service, path, nil)
		requireRefused("reversed InspectIngress", r, err)
		r, err = inspectAndRefreshEgressBorrowed(policy, service, *path, nil)
		requireRefused("borrowed egress", r, err)
		r, err = inspectAndRefreshEgressForSenderBorrowed(policy, NewId(), service, *path, nil)
		requireRefused("borrowed sender egress", r, err)
		r, err = inspectAndRefreshIngressBorrowed(policy, service, *path, nil)
		requireRefused("borrowed ingress", r, err)
		r, err = inspectAndRefreshIngressForSenderBorrowed(provider, NewId(), service, *path, nil)
		requireRefused("borrowed reversed sender ingress", r, err)

		r, decision, err := inspectEgressDetailed(policy, service, path, nil)
		requireRefused("detailed", r, err)
		AssertEqual(t, decision.reason, SecurityPolicyReasonServiceSource)
		r, decision, err = inspectAndRefreshEgressGroupDetailedBorrowed(
			policy,
			service,
			[]IpPath{*path, *path},
			[][]byte{nil, nil},
		)
		requireRefused("detailed group", r, err)
		AssertEqual(t, decision.reason, SecurityPolicyReasonServiceSource)
		r, err = inspectAndRefreshEgressGroupBorrowed(policy, service, []IpPath{*path}, [][]byte{nil})
		requireRefused("group", r, err)

		AssertEqual(t, SecurityPolicyReasonServiceSource.String(), "service-source")
		if counts := policy.Stats().Reasons(false)[SecurityPolicyReasonServiceSource]; len(counts) == 0 {
			t.Fatal("the reason was not counted")
		}
	})
}

// An incident has no route: not local under the bypass, and not by an override,
// either of which would carry a mere drop out the device.
func TestServiceRefusalCannotBeRoutedLocally(t *testing.T) {
	block, local := blockActionApply(SecurityPolicyResultIncident, true, false, nil)
	if !block || local {
		t.Fatalf("block=%t local=%t, want block and no local route", block, local)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), newServiceModeSettings(nil))
	defer client.Cancel()
	userNatClient := NewRemoteUserNatClient(
		client,
		func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {},
		[]MultiHopId{RequireMultiHopId(NewId())},
		protocol.ProvideMode_Network,
	)
	defer userNatClient.Close()
	userNatClient.SetLocalSecurityBypass(true)

	packet := serviceModeTcpPacket()
	defer MessagePoolReturn(packet)
	if userNatClient.SendPacket(TransferPath{}, protocol.ProvideMode_Service, packet, 0) {
		t.Fatal("a Service packet was accepted under the local bypass")
	}
}

// A Service peer is not remembered for network quality notices, which would open a
// return contract to carry a frame its receiver refuses. Other peers still are.
func TestServicePeerIsNotObservedForNetworkQuality(t *testing.T) {
	client := newNetworkQualityTestClient(t)
	observed := func(peerId Id) bool {
		client.networkQuality.stateLock.Lock()
		defer client.networkQuality.stateLock.Unlock()
		_, found := client.networkQuality.observedPeers[peerId]
		return found
	}

	servicePeerId := NewId()
	client.observeNetworkQualityPeer(TransferPath{SourceId: servicePeerId}, Peer{ProvideMode: protocol.ProvideMode_Service})
	if observed(servicePeerId) {
		t.Fatal("a Service peer was remembered")
	}

	streamPeerId := NewId()
	client.observeNetworkQualityPeer(TransferPath{SourceId: streamPeerId}, Peer{ProvideMode: protocol.ProvideMode_Stream})
	if !observed(streamPeerId) {
		t.Fatal("a Stream peer was not remembered")
	}
}
