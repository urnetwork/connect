package connect

// The contract seals: what the platform says about a contract's source, signed
// into `StoredContract` in every provide mode and read back on `Peer`. These
// tests sign real contracts with a real provide key and put real Packs on a
// client's route, so a seal reaches a callback the way it does in production.

import (
	"bytes"
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/urnetwork/connect/protocol"
)

// Every provide mode a contract can carry today. Service joins with C9.
var contractSealProvideModes = []protocol.ProvideMode{
	protocol.ProvideMode_Network,
	protocol.ProvideMode_FriendsAndFamily,
	protocol.ProvideMode_Public,
	protocol.ProvideMode_Stream,
	protocol.ProvideMode_PublicStream,
}

// One contract's seals.
type contractSeals struct {
	sourceSubject    Id
	sourceNetwork    Id
	contractClass    protocol.ContractClass
	windowDelegation []byte
}

func newContractSeals() contractSeals {
	return contractSeals{
		sourceSubject:    NewId(),
		sourceNetwork:    NewId(),
		contractClass:    ContractClassData,
		windowDelegation: []byte("synthetic signed window delegation"),
	}
}

func (self contractSeals) fill(stored *protocol.StoredContract) {
	stored.SourceSubjectClientId = self.sourceSubject.Bytes()
	stored.SourceNetworkId = self.sourceNetwork.Bytes()
	stored.ContractClass = self.contractClass
	stored.WindowDelegation = self.windowDelegation
}

// The seals as `Peer` reports them.
func peerContractSeals(peer Peer) contractSeals {
	return contractSeals{
		sourceSubject:    peer.SourceSubject,
		sourceNetwork:    peer.SourceNetwork,
		contractClass:    peer.ContractClass,
		windowDelegation: peer.WindowDelegation,
	}
}

func (self contractSeals) equal(other contractSeals) bool {
	return self.sourceSubject == other.sourceSubject &&
		self.sourceNetwork == other.sourceNetwork &&
		self.contractClass == other.contractClass &&
		bytes.Equal(self.windowDelegation, other.windowDelegation)
}

// One receive callback: the messages it carried, in order, under one `Peer`.
type contractSealDelivery struct {
	contents []string
	peer     Peer
}

// A real client that holds a provide key for every mode, and a route into it.
type contractSealFixture struct {
	t        *testing.T
	client   *Client
	route    chan []byte
	sourceId Id

	stateLock  sync.Mutex
	deliveries []contractSealDelivery
}

// Runs inside a synctest bubble, so `synctest.Wait` is the barrier between a Pack
// and what it did.
func newContractSealFixture(t *testing.T) *contractSealFixture {
	t.Helper()
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithCancel(t.Context())
	settings := closeWaitClientSettings()
	settings.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Unix(1, 0)
	settings.ContractManagerSettings.NetworkEventTimeChangeHmac = time.Unix(1, 0)
	// nothing here answers the key publisher, so it does not run
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	self := &contractSealFixture{
		t:        t,
		client:   NewClient(ctx, NewId(), NewNoContractClientOob(), settings),
		route:    make(chan []byte, 64),
		sourceId: NewId(),
	}
	outbound := make(chan []byte, 1024)
	self.client.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{self.route})
	self.client.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{outbound})
	provideModes := map[protocol.ProvideMode]bool{}
	for _, mode := range contractSealProvideModes {
		provideModes[mode] = true
	}
	self.client.ContractManager().SetProvideModes(provideModes)
	self.client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, peer Peer) {
		contents := []string{}
		for _, frame := range frames {
			if frame.MessageType != protocol.MessageType_TestSimpleMessage {
				continue
			}
			var message protocol.SimpleMessage
			if err := proto.Unmarshal(frame.MessageBytes, &message); err != nil {
				t.Error(err)
				continue
			}
			contents = append(contents, message.Content)
		}
		if len(contents) == 0 {
			return
		}
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.deliveries = append(self.deliveries, contractSealDelivery{contents: contents, peer: peer})
	})
	t.Cleanup(func() {
		cancel()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := self.client.CloseAndWait(closeCtx); err != nil {
			t.Errorf("close seal client: %v", err)
		}
		for _, route := range []chan []byte{self.route, outbound} {
			for len(route) != 0 {
				MessagePoolReturn(<-route)
			}
		}
	})
	return self
}

// A contract from this fixture's source, as `fill` leaves it, signed under `mode`.
func (self *contractSealFixture) contract(
	mode protocol.ProvideMode,
	fill func(*protocol.StoredContract),
) *protocol.Contract {
	self.t.Helper()
	stored := &protocol.StoredContract{
		ContractId:        NewId().Bytes(),
		TransferByteCount: uint64(mib(1)),
		SourceId:          self.sourceId.Bytes(),
		DestinationId:     self.client.ClientId().Bytes(),
		Roles:             []string{"synthetic-role"},
		Principal:         "synthetic-principal",
	}
	if fill != nil {
		fill(stored)
	}
	storedBytes, err := proto.Marshal(stored)
	if err != nil {
		self.t.Fatal(err)
	}
	manager := self.client.ContractManager()
	secret, ok := manager.GetProvideSecretKey(mode)
	if !ok {
		self.t.Fatalf("no provide key for %s", mode)
	}
	return &protocol.Contract{
		StoredContractBytes: storedBytes,
		StoredContractHmac:  SignStoredContract(manager.settings, secret, storedBytes),
		ProvideMode:         mode,
	}
}

func contractSealContractFrame(t *testing.T, contract *protocol.Contract) *protocol.Frame {
	t.Helper()
	contractBytes, err := proto.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	return &protocol.Frame{MessageType: protocol.MessageType_TransferContract, MessageBytes: contractBytes}
}

// A Pack of one message at `sequenceNumber` of `sequenceId`.
func contractSealPack(t *testing.T, sequenceId Id, sequenceNumber uint64, content string) *protocol.Pack {
	t.Helper()
	messageBytes, err := proto.Marshal(&protocol.SimpleMessage{Content: content})
	if err != nil {
		t.Fatal(err)
	}
	return &protocol.Pack{
		MessageId:      NewId().Bytes(),
		SequenceId:     sequenceId.Bytes(),
		SequenceNumber: sequenceNumber,
		Frames:         []*protocol.Frame{{MessageType: protocol.MessageType_TestSimpleMessage, MessageBytes: messageBytes}},
	}
}

// Puts a Pack on the route and waits for all it causes.
func (self *contractSealFixture) inject(pack *protocol.Pack) {
	self.t.Helper()
	wire, err := ProtoMarshal(&protocol.TransferFrame{
		TransferPath: TransferPath{SourceId: self.sourceId, DestinationId: self.client.ClientId()}.ToProtobuf(),
		Pack:         pack,
	})
	if err != nil {
		self.t.Fatal(err)
	}
	self.route <- wire
	synctest.Wait()
}

func (self *contractSealFixture) takeDeliveries() []contractSealDelivery {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	deliveries := self.deliveries
	self.deliveries = nil
	return deliveries
}

// The one delivery of `content`, as a callback of its own.
func (self *contractSealFixture) requireDelivery(content string) Peer {
	self.t.Helper()
	deliveries := self.takeDeliveries()
	if len(deliveries) != 1 || !slices.Equal(deliveries[0].contents, []string{content}) {
		self.t.Fatalf("delivered %+v, want %q alone", deliveries, content)
	}
	return deliveries[0].peer
}

// A verified contract in each mode reaches `Peer` with all four seals, and, unlike
// roles and principal, in every mode rather than Network alone.
func TestContractSealsReachPeerInEveryProvideMode(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture := newContractSealFixture(t)
		for _, mode := range contractSealProvideModes {
			fixture.sourceId = NewId()
			seals := newContractSeals()
			pack := contractSealPack(t, NewId(), 0, "sealed")
			pack.Head = true
			pack.ContractFrame = contractSealContractFrame(t, fixture.contract(mode, seals.fill))
			fixture.inject(pack)

			peer := fixture.requireDelivery("sealed")
			if peer.ProvideMode != mode {
				t.Errorf("%s: peer provide mode %s", mode, peer.ProvideMode)
			}
			if !peerContractSeals(peer).equal(seals) {
				t.Errorf("%s: peer seals %+v, want %+v", mode, peerContractSeals(peer), seals)
			}
			if network := mode == protocol.ProvideMode_Network; network != (peer.Principal != "") ||
				network != (0 < len(peer.Roles)) {
				t.Errorf("%s: roles %v and principal %q honored in the wrong mode", mode, peer.Roles, peer.Principal)
			}
		}
	})
}

// A subprotocol listener is where the class is read, so a seal must get past
// the dispatcher to both kinds of listener.
func TestContractSealsReachSubprotocolListeners(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture := newContractSealFixture(t)
		var stateLock sync.Mutex
		rawPeers := []Peer{}
		if _, err := fixture.client.AddSubprotocolRawCallback(SubprotocolId(4401), func(_ TransferPath, _ SubprotocolId, _ []byte, peer Peer) {
			stateLock.Lock()
			defer stateLock.Unlock()
			rawPeers = append(rawPeers, peer)
		}); err != nil {
			t.Fatal(err)
		}
		typedPeers := []Peer{}
		if _, err := RegisterSubprotocol[*fixedMessage](fixture.client, SubprotocolId(4401), &fixedCodec{}, func(_ TransferPath, _ *fixedMessage, peer Peer) {
			stateLock.Lock()
			defer stateLock.Unlock()
			typedPeers = append(typedPeers, peer)
		}); err != nil {
			t.Fatal(err)
		}

		seals := newContractSeals()
		seals.contractClass = ContractClassControl
		message := &fixedMessage{}
		message.set([]byte("sealed"))
		frame, err := encodeSubprotocolFrame[*fixedMessage](fixture.client, &fixedCodec{}, SubprotocolId(4401), message)
		if err != nil {
			t.Fatal(err)
		}
		pack := contractSealPack(t, NewId(), 0, "sealed")
		pack.Head = true
		pack.Frames = []*protocol.Frame{frame}
		pack.ContractFrame = contractSealContractFrame(t, fixture.contract(protocol.ProvideMode_Network, seals.fill))
		fixture.inject(pack)
		MessagePoolReturn(frame.MessageBytes)

		stateLock.Lock()
		defer stateLock.Unlock()
		if len(rawPeers) != 1 || !peerContractSeals(rawPeers[0]).equal(seals) {
			t.Errorf("raw listener saw %+v, want %+v", rawPeers, seals)
		}
		if len(typedPeers) != 1 || !peerContractSeals(typedPeers[0]).equal(seals) {
			t.Errorf("typed listener saw %+v, want %+v", typedPeers, seals)
		}
	})
}

// Items wait in the queue and drain as one run. One callback carries one `Peer`,
// so a change of any one seal must end the batch, or the frames of a control
// contract are delivered under the data contract that follows them.
func TestContractSealsBoundDrainedDeliveryBatches(t *testing.T) {
	GetMessagePoolAggregateStats()
	first := newContractSeals()
	first.contractClass = ContractClassControl
	changes := map[string]func(*contractSeals){
		"subject":    func(seals *contractSeals) { seals.sourceSubject = NewId() },
		"network":    func(seals *contractSeals) { seals.sourceNetwork = NewId() },
		"class":      func(seals *contractSeals) { seals.contractClass = ContractClassData },
		"delegation": func(seals *contractSeals) { seals.windowDelegation = []byte("a later delegation") },
	}
	for name, change := range changes {
		synctest.Test(t, func(t *testing.T) {
			fixture := newContractSealFixture(t)
			second := first
			change(&second)

			sequenceId := NewId()
			head := contractSealPack(t, sequenceId, 0, "first 0")
			head.Head = true
			head.ContractFrame = contractSealContractFrame(t, fixture.contract(protocol.ProvideMode_Network, first.fill))
			next := contractSealPack(t, sequenceId, 2, "second 0")
			next.ContractFrame = contractSealContractFrame(t, fixture.contract(protocol.ProvideMode_Network, second.fill))

			// everything behind the head waits for it
			fixture.inject(contractSealPack(t, sequenceId, 1, "first 1"))
			fixture.inject(next)
			fixture.inject(contractSealPack(t, sequenceId, 3, "second 1"))
			if deliveries := fixture.takeDeliveries(); len(deliveries) != 0 {
				t.Fatalf("%s: delivered ahead of the head: %+v", name, deliveries)
			}
			fixture.inject(head)

			deliveries := fixture.takeDeliveries()
			if len(deliveries) != 2 {
				t.Fatalf("%s: %d callbacks, want one per contract: %+v", name, len(deliveries), deliveries)
			}
			if !slices.Equal(deliveries[0].contents, []string{"first 0", "first 1"}) ||
				!peerContractSeals(deliveries[0].peer).equal(first) {
				t.Errorf("%s: first batch %+v, want seals %+v", name, deliveries[0], first)
			}
			if !slices.Equal(deliveries[1].contents, []string{"second 0", "second 1"}) ||
				!peerContractSeals(deliveries[1].peer).equal(second) {
				t.Errorf("%s: second batch %+v, want seals %+v", name, deliveries[1], second)
			}
		})
	}
}

// A no-acknowledgement Pack names the contract it rides. Once a successor is
// current it can still name the predecessor, and it carries the seals of the
// contract it named, not those of the current one.
func TestContractSealsFollowTheContractANoAckPackNames(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture := newContractSealFixture(t)
		predecessorSeals := newContractSeals()
		predecessorSeals.contractClass = ContractClassControl
		predecessor := fixture.contract(protocol.ProvideMode_Network, predecessorSeals.fill)
		var stored protocol.StoredContract
		if err := proto.Unmarshal(predecessor.StoredContractBytes, &stored); err != nil {
			t.Fatal(err)
		}
		currentSeals := newContractSeals()

		sequenceId := NewId()
		head := contractSealPack(t, sequenceId, 0, "predecessor")
		head.Head = true
		head.ContractFrame = contractSealContractFrame(t, predecessor)
		fixture.inject(head)
		fixture.requireDelivery("predecessor")

		successor := contractSealPack(t, sequenceId, 1, "current")
		successor.ContractFrame = contractSealContractFrame(t, fixture.contract(protocol.ProvideMode_Network, currentSeals.fill))
		fixture.inject(successor)
		if peer := fixture.requireDelivery("current"); !peerContractSeals(peer).equal(currentSeals) {
			t.Errorf("current peer seals %+v, want %+v", peerContractSeals(peer), currentSeals)
		}

		noAck := contractSealPack(t, sequenceId, 2, "rides the predecessor")
		noAck.Nack = true
		noAck.ContractId = stored.ContractId
		fixture.inject(noAck)
		if peer := fixture.requireDelivery("rides the predecessor"); !peerContractSeals(peer).equal(predecessorSeals) {
			t.Errorf("no-ack peer seals %+v, want the predecessor's %+v", peerContractSeals(peer), predecessorSeals)
		}
	})
}

// A successor announced ahead of its use is stored without becoming current, and
// the Packs still in flight keep the seals of the contract they ride. The seals
// change at the switch.
func TestContractSealsOfAnAnnouncedSuccessorApplyFromTheSwitch(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture := newContractSealFixture(t)
		currentSeals := newContractSeals()
		aheadSeals := newContractSeals()
		aheadSeals.contractClass = ContractClassControl
		ahead := contractSealContractFrame(t, fixture.contract(protocol.ProvideMode_Network, aheadSeals.fill))

		sequenceId := NewId()
		head := contractSealPack(t, sequenceId, 0, "current")
		head.Head = true
		head.ContractFrame = contractSealContractFrame(t, fixture.contract(protocol.ProvideMode_Network, currentSeals.fill))
		fixture.inject(head)
		fixture.requireDelivery("current")

		announcement := contractSealPack(t, sequenceId, 1, "")
		announcement.Frames = nil
		announcement.ContractFrame = ahead
		announcement.ContractAhead = true
		fixture.inject(announcement)

		fixture.inject(contractSealPack(t, sequenceId, 2, "in flight"))
		if peer := fixture.requireDelivery("in flight"); !peerContractSeals(peer).equal(currentSeals) {
			t.Errorf("in-flight seals %+v, want the current contract's %+v", peerContractSeals(peer), currentSeals)
		}

		switched := contractSealPack(t, sequenceId, 3, "switched")
		switched.ContractFrame = ahead
		fixture.inject(switched)
		if peer := fixture.requireDelivery("switched"); !peerContractSeals(peer).equal(aheadSeals) {
			t.Errorf("switched seals %+v, want the successor's %+v", peerContractSeals(peer), aheadSeals)
		}
	})
}

// A resend ahead of the delivery point replaces what the queue holds. The
// replayed copy must still reach the callback with its contract's seals.
func TestContractSealsSurviveAResendOfAQueuedPack(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture := newContractSealFixture(t)
		firstSeals := newContractSeals()
		secondSeals := newContractSeals()
		secondSeals.contractClass = ContractClassControl

		sequenceId := NewId()
		head := contractSealPack(t, sequenceId, 0, "first")
		head.Head = true
		head.ContractFrame = contractSealContractFrame(t, fixture.contract(protocol.ProvideMode_Network, firstSeals.fill))
		fixture.inject(head)
		fixture.requireDelivery("first")

		queued := contractSealPack(t, sequenceId, 2, "resent")
		queued.ContractFrame = contractSealContractFrame(t, fixture.contract(protocol.ProvideMode_Network, secondSeals.fill))
		fixture.inject(queued)
		resent := proto.Clone(queued).(*protocol.Pack)
		fixture.inject(resent)
		if deliveries := fixture.takeDeliveries(); len(deliveries) != 0 {
			t.Fatalf("delivered ahead of the gap: %+v", deliveries)
		}

		fixture.inject(contractSealPack(t, sequenceId, 1, "gap"))
		deliveries := fixture.takeDeliveries()
		if len(deliveries) != 2 || !slices.Equal(deliveries[0].contents, []string{"gap"}) ||
			!slices.Equal(deliveries[1].contents, []string{"resent"}) {
			t.Fatalf("delivered %+v, want the gap then one copy of the resent Pack", deliveries)
		}
		if !peerContractSeals(deliveries[0].peer).equal(firstSeals) {
			t.Errorf("gap seals %+v, want %+v", peerContractSeals(deliveries[0].peer), firstSeals)
		}
		if !peerContractSeals(deliveries[1].peer).equal(secondSeals) {
			t.Errorf("resent seals %+v, want %+v", peerContractSeals(deliveries[1].peer), secondSeals)
		}
	})
}

// A replacement receiver holds a Pack with no contract of its own until the
// sender's promoted head brings the proof. The held Pack replays under that
// proof, and it is the proof's seals that must reach the callback.
func TestContractSealsSurviveAHeadRepairOfAHeldPack(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		fixture, log := newHeldHeadContractFixture(t, nil)
		sequence, _, promoted := stageHeldHeadContract(t, fixture)

		// the sender's retry is the contract it already holds, so seal that one
		seals := newContractSeals()
		var contract protocol.Contract
		if err := ProtoUnmarshal(promoted.pack.ContractFrame.MessageBytes, &contract); err != nil {
			t.Fatal(err)
		}
		var stored protocol.StoredContract
		if err := ProtoUnmarshal(contract.StoredContractBytes, &stored); err != nil {
			t.Fatal(err)
		}
		seals.fill(&stored)
		var err error
		contract.StoredContractBytes, err = proto.Marshal(&stored)
		if err != nil {
			t.Fatal(err)
		}
		manager := fixture.receiver.ContractManager()
		secret, ok := manager.GetProvideSecretKey(protocol.ProvideMode_Network)
		if !ok {
			t.Fatal("no provide key")
		}
		contract.StoredContractHmac = SignStoredContract(manager.settings, secret, contract.StoredContractBytes)
		pack := proto.Clone(promoted.pack).(*protocol.Pack)
		pack.ContractFrame.MessageBytes, err = proto.Marshal(&contract)
		if err != nil {
			t.Fatal(err)
		}
		rewriteHeldHeadContractWire(t, promoted, pack)

		var stateLock sync.Mutex
		peers := []Peer{}
		fixture.receiver.AddReceiveCallback(func(_ TransferPath, _ []*protocol.Frame, peer Peer) {
			stateLock.Lock()
			defer stateLock.Unlock()
			peers = append(peers, peer)
		})
		fixture.forward(promoted, fixture.receiverIn)
		fixture.acknowledge()

		stateLock.Lock()
		defer stateLock.Unlock()
		if len(peers) == 0 || sequence.nextSequenceNumber != promoted.pack.SequenceNumber+1 {
			t.Fatalf("held Pack not delivered: peers=%d next=%d errors=%v", len(peers), sequence.nextSequenceNumber, log.messages())
		}
		for _, peer := range peers {
			if !peerContractSeals(peer).equal(seals) {
				t.Errorf("repaired peer seals %+v, want %+v", peerContractSeals(peer), seals)
			}
		}
	})
}

// A companion contract carries its forward contract's seals: the lane differs and
// the seals name the forward source, not the companion's source.
func TestCompanionContractCarriesForwardSeals(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture := newContractSealFixture(t)
		forward := newContractSeals()
		for _, mode := range []protocol.ProvideMode{protocol.ProvideMode_Network, protocol.ProvideMode_Stream} {
			fixture.sourceId = NewId()
			pack := contractSealPack(t, NewId(), 0, "reply")
			pack.Head = true
			pack.CompanionContract = true
			pack.ContractFrame = contractSealContractFrame(t, fixture.contract(mode, forward.fill))
			fixture.inject(pack)

			peer := fixture.requireDelivery("reply")
			if !peer.TransferKey.CompanionContract {
				t.Errorf("%s: reply did not arrive on the companion lane", mode)
			}
			if !peerContractSeals(peer).equal(forward) {
				t.Errorf("%s: companion seals %+v, want the forward contract's %+v", mode, peerContractSeals(peer), forward)
			}
		}
	})
}

// The seals are inside the signed bytes, so changing one breaks the signature, in
// whichever way a holder of the contract tries it.
func TestContractSealTamperingIsRefused(t *testing.T) {
	GetMessagePoolAggregateStats()
	changes := map[string]func(*protocol.StoredContract){
		"subject":    func(stored *protocol.StoredContract) { stored.SourceSubjectClientId = NewId().Bytes() },
		"network":    func(stored *protocol.StoredContract) { stored.SourceNetworkId = NewId().Bytes() },
		"class":      func(stored *protocol.StoredContract) { stored.ContractClass = ContractClassData },
		"delegation": func(stored *protocol.StoredContract) { stored.WindowDelegation = []byte("a forged delegation") },
		"stripped": func(stored *protocol.StoredContract) {
			stored.SourceSubjectClientId, stored.SourceNetworkId, stored.WindowDelegation = nil, nil, nil
			stored.ContractClass = ContractClassUnset
		},
	}
	for name, change := range changes {
		synctest.Test(t, func(t *testing.T) {
			fixture := newContractSealFixture(t)
			seals := newContractSeals()
			seals.contractClass = ContractClassControl
			contract := fixture.contract(protocol.ProvideMode_Network, seals.fill)
			manager := fixture.client.ContractManager()
			if !manager.Verify(contract.StoredContractHmac, contract.StoredContractBytes, contract.ProvideMode) {
				t.Fatalf("%s: the untampered contract did not verify", name)
			}
			var stored protocol.StoredContract
			if err := proto.Unmarshal(contract.StoredContractBytes, &stored); err != nil {
				t.Fatal(err)
			}
			change(&stored)
			var err error
			contract.StoredContractBytes, err = proto.Marshal(&stored)
			if err != nil {
				t.Fatal(err)
			}
			if manager.Verify(contract.StoredContractHmac, contract.StoredContractBytes, contract.ProvideMode) {
				t.Fatalf("%s: the signature still covers the tampered bytes", name)
			}

			pack := contractSealPack(t, NewId(), 0, "tampered")
			pack.Head = true
			pack.ContractFrame = contractSealContractFrame(t, contract)
			fixture.inject(pack)
			if deliveries := fixture.takeDeliveries(); len(deliveries) != 0 {
				t.Errorf("%s: a tampered contract delivered %+v", name, deliveries)
			}
		})
	}
}

// A malformed id under a good signature is a signer's fault, and a receiver must
// not read it as an unsealed contract.
func TestContractMalformedSealIdIsRefused(t *testing.T) {
	GetMessagePoolAggregateStats()
	changes := map[string]func(*protocol.StoredContract){
		"subject": func(stored *protocol.StoredContract) { stored.SourceSubjectClientId = []byte{1, 2, 3} },
		"network": func(stored *protocol.StoredContract) { stored.SourceNetworkId = []byte{1, 2, 3} },
	}
	for name, change := range changes {
		synctest.Test(t, func(t *testing.T) {
			fixture := newContractSealFixture(t)
			pack := contractSealPack(t, NewId(), 0, "malformed")
			pack.Head = true
			pack.ContractFrame = contractSealContractFrame(t, fixture.contract(protocol.ProvideMode_Network, change))
			fixture.inject(pack)
			if deliveries := fixture.takeDeliveries(); len(deliveries) != 0 {
				t.Errorf("%s: a malformed seal delivered %+v", name, deliveries)
			}
		})
	}
}

// An unset class stays visible as unset, for a receiver to read as the narrowest
// class. A class from a later build is no wider than that.
func TestContractClassUnsetAndUnknownReadAsUnset(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture := newContractSealFixture(t)
		for _, contractClass := range []protocol.ContractClass{ContractClassUnset, protocol.ContractClass(3), protocol.ContractClass(-1)} {
			fixture.sourceId = NewId()
			pack := contractSealPack(t, NewId(), 0, "unclassed")
			pack.Head = true
			pack.ContractFrame = contractSealContractFrame(t, fixture.contract(protocol.ProvideMode_Network, func(stored *protocol.StoredContract) {
				stored.ContractClass = contractClass
			}))
			fixture.inject(pack)
			if peer := fixture.requireDelivery("unclassed"); peer.ContractClass != ContractClassUnset {
				t.Errorf("class %d read as %s", contractClass, peer.ContractClass)
			}
		}
		for _, contractClass := range []protocol.ContractClass{ContractClassData, ContractClassControl} {
			fixture.sourceId = NewId()
			pack := contractSealPack(t, NewId(), 0, "classed")
			pack.Head = true
			pack.ContractFrame = contractSealContractFrame(t, fixture.contract(protocol.ProvideMode_Network, func(stored *protocol.StoredContract) {
				stored.ContractClass = contractClass
			}))
			fixture.inject(pack)
			if peer := fixture.requireDelivery("classed"); peer.ContractClass != contractClass {
				t.Errorf("class %s read as %s", contractClass, peer.ContractClass)
			}
		}
	})
}

// Contracts signed before the seals existed, and the frames of a no-contract peer,
// parse as they always did and read with every seal empty.
func TestContractsWithoutSealsReadEmpty(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture := newContractSealFixture(t)
		for _, mode := range contractSealProvideModes {
			fixture.sourceId = NewId()
			contract := fixture.contract(mode, nil)
			// nothing a signer writes without seals is news to an older parser
			older := dynamicpb.NewMessage(olderContractMessage(t, "StoredContract", 12))
			if err := proto.Unmarshal(contract.StoredContractBytes, older); err != nil {
				t.Fatal(err)
			}
			if 0 < len(older.GetUnknown()) {
				t.Fatalf("%s: an unsealed contract writes %x past the older fields", mode, older.GetUnknown())
			}
			pack := contractSealPack(t, NewId(), 0, "older")
			pack.Head = true
			pack.ContractFrame = contractSealContractFrame(t, contract)
			fixture.inject(pack)
			peer := fixture.requireDelivery("older")
			if !peerContractSeals(peer).equal(contractSeals{}) || peer.WindowDelegation != nil {
				t.Errorf("%s: peer seals %+v, want none", mode, peerContractSeals(peer))
			}
		}

		fixture.sourceId = NewId()
		fixture.client.ContractManager().AddNoContractPeer(fixture.sourceId)
		pack := contractSealPack(t, NewId(), 0, "no contract")
		pack.Head = true
		fixture.inject(pack)
		if peer := fixture.requireDelivery("no contract"); !peerContractSeals(peer).equal(contractSeals{}) {
			t.Errorf("no-contract peer seals %+v, want none", peerContractSeals(peer))
		}
	})
}

// Continues `StoredContract`'s numbers after 12 and `CreateContract`'s after 9.
// A field number here is the wire format, and it never moves.
func TestContractSealFieldNumbers(t *testing.T) {
	stored := (&protocol.StoredContract{}).ProtoReflect().Descriptor().Fields()
	for name, number := range map[protoreflect.Name]protoreflect.FieldNumber{
		"source_subject_client_id": 13,
		"source_network_id":        14,
		"contract_class":           15,
		"window_delegation":        16,
	} {
		if field := stored.ByName(name); field == nil || field.Number() != number {
			t.Errorf("StoredContract.%s is not field %d: %v", name, number, field)
		}
	}
	create := (&protocol.CreateContract{}).ProtoReflect().Descriptor().Fields()
	if field := create.ByName("window_delegation"); field == nil || field.Number() != 10 {
		t.Errorf("CreateContract.window_delegation is not field 10: %v", field)
	}
	for number, name := range map[protoreflect.EnumNumber]string{
		0: "ContractClassUnset",
		1: "ContractClassData",
		2: "ContractClassControl",
	} {
		if protocol.ContractClass_name[int32(number)] != name {
			t.Errorf("class %d is %q, want %q", number, protocol.ContractClass_name[int32(number)], name)
		}
	}
}

// A descriptor of `transfer.proto` as an older peer has it: `message` without any
// field numbered past `lastFieldNumber`.
func olderContractMessage(t *testing.T, message protoreflect.Name, lastFieldNumber int32) protoreflect.MessageDescriptor {
	t.Helper()
	fileProto := protodesc.ToFileDescriptorProto(protocol.File_transfer_proto)
	for _, messageProto := range fileProto.MessageType {
		if protoreflect.Name(messageProto.GetName()) != message {
			continue
		}
		messageProto.Field = slices.DeleteFunc(messageProto.Field, func(field *descriptorpb.FieldDescriptorProto) bool {
			return lastFieldNumber < field.GetNumber()
		})
		// a field's `optional` is a oneof of its own, which goes with the field
		oneofs := []*descriptorpb.OneofDescriptorProto{}
		for _, field := range messageProto.Field {
			if field.OneofIndex != nil {
				oneofs = append(oneofs, messageProto.OneofDecl[field.GetOneofIndex()])
				field.OneofIndex = proto.Int32(int32(len(oneofs) - 1))
			}
		}
		messageProto.OneofDecl = oneofs
	}
	file, err := protodesc.NewFile(fileProto, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatal(err)
	}
	return file.Messages().ByName(message)
}

// An older peer has no seal fields. It reads everything else as before, keeps the
// seals as unknown bytes, and verifies the stored bytes as they arrived.
func TestOlderPeerIgnoresContractSeals(t *testing.T) {
	seals := newContractSeals()
	sourceId, destinationId := NewId(), NewId()
	storedContract := &protocol.StoredContract{
		ContractId:        NewId().Bytes(),
		TransferByteCount: 1024,
		SourceId:          sourceId.Bytes(),
		DestinationId:     destinationId.Bytes(),
	}
	seals.fill(storedContract)
	storedBytes, err := proto.Marshal(storedContract)
	if err != nil {
		t.Fatal(err)
	}

	older := dynamicpb.NewMessage(olderContractMessage(t, "StoredContract", 12))
	if err := proto.Unmarshal(storedBytes, older); err != nil {
		t.Fatal(err)
	}
	fields := older.Descriptor().Fields()
	if !bytes.Equal(older.Get(fields.ByName("source_id")).Bytes(), sourceId.Bytes()) ||
		!bytes.Equal(older.Get(fields.ByName("destination_id")).Bytes(), destinationId.Bytes()) ||
		older.Get(fields.ByName("transfer_byte_count")).Uint() != 1024 {
		t.Errorf("older peer misread the contract: %v", older)
	}
	if len(older.GetUnknown()) == 0 {
		t.Error("older peer lost the seals")
	}
	again, err := proto.MarshalOptions{Deterministic: true}.Marshal(older)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, storedBytes) {
		t.Errorf("older peer's copy of the signed bytes moved:\n%x\n%x", again, storedBytes)
	}

	createContract := &protocol.CreateContract{
		DestinationId:    destinationId.Bytes(),
		WindowDelegation: seals.windowDelegation,
	}
	createBytes, err := proto.Marshal(createContract)
	if err != nil {
		t.Fatal(err)
	}
	olderCreate := dynamicpb.NewMessage(olderContractMessage(t, "CreateContract", 9))
	if err := proto.Unmarshal(createBytes, olderCreate); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(olderCreate.Get(olderCreate.Descriptor().Fields().ByName("destination_id")).Bytes(), destinationId.Bytes()) ||
		len(olderCreate.GetUnknown()) == 0 {
		t.Errorf("older platform misread the request: %v", olderCreate)
	}
}
