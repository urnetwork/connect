package connect

// A contract's successor is requested when the contract is projected to run
// out, not the moment the contract is taken (`ContractPrefetchLeadTime`).
//
// Measured on the main network, most contracts end with nothing acknowledged
// by either party, and those hold most of the escrowed bytes. The largest share
// was a sequence that never exhausted its opening contract: a quality window's
// continuous pings, the provider's echo of them, a ping to a provider that
// never answers, a short exchange. Each took its one-megabyte opening contract
// and in the same step requested a successor of a quarter of the standard size,
// which nothing ever used. It was closed at zero at sequence end or after the
// queue expiry, and until the platform settled it the escrow stayed held.
//
// Most rows drive a sequence directly, so every contract decision happens on
// the test goroutine, and the platform is a fixture that holds each request
// until the row answers it. Platform latency is modelled in bytes: a request is
// answered only after the sequence has charged a given number of bytes more.
// The last row runs two clients end to end. These rows use only what the code
// had before the change, so they run against it unmodified; the rule's own
// rows are in transfer_contract_successor_rule_test.go.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Grants every contract request as it arrives, as `grantingClientOob` does,
// and counts the requests.
type countingContractOob struct {
	grantingClientOob
	createCount atomic.Int64
}

func (self *countingContractOob) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	for _, frame := range frames {
		if message, err := FromFrame(frame); err == nil {
			if _, ok := message.(*protocol.CreateContract); ok {
				self.createCount.Add(1)
			}
		}
	}
	self.grantingClientOob.SendControl(frames, callback)
}

// A sequence that nothing runs, writing to a route that absorbs its contract
// Packs, on a client whose contract requests the returned fixture holds.
func newContractSuccessorTestSequence(
	t *testing.T,
	companionContract bool,
	networkPeer bool,
) (*Client, *pendingContractCreates, *SendSequence) {
	t.Helper()
	client, oob := newPendingContractClient(t, nil)
	destinationId := NewId()
	route := make(chan []byte, 1024)
	client.RouteManager().UpdateTransport(
		NewSendClientTransport(DestinationId(destinationId)),
		[]Route{route},
	)
	t.Cleanup(func() {
		for {
			select {
			case transferFrameBytes := <-route:
				MessagePoolReturn(transferFrameBytes)
			default:
				return
			}
		}
	})
	sequence := NewSendSequence(
		t.Context(),
		client,
		client.sendBuffer,
		destinationId,
		MultiHopId{},
		companionContract,
		false,
		networkPeer,
		sequenceTlsRoleClient,
		false,
		client.settings.SendBufferSettings,
	)
	t.Cleanup(sequence.Close)
	return client, oob, sequence
}

// Answers held request `index` with a contract from this client to the
// requested destination for the requested size, the way the platform does.
// Escrowed public mode, like a quality window's contract to a provider. The
// sender never verifies the platform signature; the receiver does.
func grantContractSuccessorTestRequest(t *testing.T, client *Client, oob *pendingContractCreates, index int) Id {
	t.Helper()
	request := func() *protocol.CreateContract {
		oob.stateLock.Lock()
		defer oob.stateLock.Unlock()
		return oob.requests[index]
	}()
	contractId := NewId()
	storedContractBytes, err := ProtoMarshal(&protocol.StoredContract{
		ContractId:        contractId.Bytes(),
		TransferByteCount: request.TransferByteCount,
		SourceId:          client.ClientId().Bytes(),
		DestinationId:     request.DestinationId,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(storedContractBytes)
	frame, err := ToFrame(&protocol.CreateContractResult{Contract: &protocol.Contract{
		StoredContractBytes: storedContractBytes,
		ProvideMode:         protocol.ProvideMode_Public,
	}}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(frame.MessageBytes)
	oob.completeFrames(index, []*protocol.Frame{frame}, nil)
	return contractId
}

// Whether held request `index` is still waiting for an answer.
func contractSuccessorTestRequestHeld(oob *pendingContractCreates, index int) bool {
	oob.stateLock.Lock()
	defer oob.stateLock.Unlock()
	return oob.callbacks[index] != nil
}

// Takes the opening contract the way a live sequence does: the request made as
// the sequence starts is still in flight when the first message needs a
// contract, and the platform answers while the sequence waits on its queue.
// Returns the opening contract's id.
func takeContractSuccessorTestOpening(t *testing.T, client *Client, oob *pendingContractCreates, sequence *SendSequence) Id {
	t.Helper()
	var openingContractId Id
	client.sendBuffer.beforeTakeContractForTest = func(sendSequenceId) {
		if oob.count() == 1 && contractSuccessorTestRequestHeld(oob, 0) {
			openingContractId = grantContractSuccessorTestRequest(t, client, oob, 0)
		}
	}
	defer func() { client.sendBuffer.beforeTakeContractForTest = nil }()
	sequence.prewarmOpeningContract()
	if oob.count() != 1 {
		t.Fatalf("the sequence start made %d contract requests, want the one opening request", oob.count())
	}
	// the first message, an empty ping
	if !sequence.updateContract(0) {
		t.Fatal("the first message could not take the opening contract")
	}
	if sequence.sendContract == nil || sequence.sendContract.contractId != openingContractId {
		t.Fatal("the opening contract the platform granted did not become current")
	}
	return openingContractId
}

// The root cause, at the sequence where it happens. A sequence whose traffic
// never comes near the end of its opening contract must request nothing past
// it. Every one of these used to request a successor of a quarter of the
// standard size in the step that took the opening megabyte, and in each of
// them that successor was never used.
//
// Acknowledgements do not enter into it, so the same row stands for a ping a
// provider answers and one it never does; the origin and the companion
// direction take the same path, so the same row stands for the window's pings
// and the provider's echo of them.
func TestASequenceThatNeverExhaustsItsContractRequestsNoSuccessor(t *testing.T) {
	type contractSuccessorTestCase struct {
		name              string
		companionContract bool
		messageByteCount  ByteCount
		messageCount      int
	}
	cases := []contractSuccessorTestCase{
		// an hour of continuous pings at one per ten seconds, a byte each
		{name: "continuous pings", companionContract: false, messageByteCount: 0, messageCount: 360},
		{name: "the echo of continuous pings", companionContract: true, messageByteCount: 0, messageCount: 360},
		// forty full-size packets, under the minimum a successor waits for
		{name: "a short exchange", companionContract: false, messageByteCount: 1400, messageCount: 40},
		{name: "a short reply", companionContract: true, messageByteCount: 1400, messageCount: 40},
	}
	for _, c := range cases {
		client, oob, sequence := newContractSuccessorTestSequence(t, c.companionContract, false)
		takeContractSuccessorTestOpening(t, client, oob, sequence)
		for range c.messageCount {
			if !sequence.updateContract(c.messageByteCount) {
				t.Fatalf("%s: the opening contract refused a message it has room for", c.name)
			}
		}
		if got := oob.count(); got != 1 {
			oob.stateLock.Lock()
			successorByteCount := oob.requests[got-1].TransferByteCount
			oob.stateLock.Unlock()
			t.Errorf(
				"%s: %d contracts requested, want only the opening one; the last is a %d byte successor for a sequence that charged %d bytes of its %d byte contract",
				c.name,
				got,
				successorByteCount,
				sequence.sendContract.ackedByteCount+sequence.sendContract.unackedByteCount,
				sequence.sendContract.transferByteCount,
			)
		}
	}
}

// The adjacent path to the same unused contract. The opening request made as
// the sequence starts can complete before the first message looks for a
// contract, when that message waits on something else first. The first
// acquisition then requested a second opening contract before looking at the
// queue, because a completed request no longer absorbs an identical one, and
// that second contract sat in the queue unused. The queued contract has to be
// taken before anything is requested.
func TestAnOpeningContractThatLandedFirstIsTakenWithoutAnotherRequest(t *testing.T) {
	client, oob, sequence := newContractSuccessorTestSequence(t, false, false)
	sequence.prewarmOpeningContract()
	if oob.count() != 1 {
		t.Fatalf("the sequence start made %d contract requests, want one", oob.count())
	}
	// the platform answers before the first message is ready
	openingContractId := grantContractSuccessorTestRequest(t, client, oob, 0)

	if !sequence.updateContract(0) {
		t.Fatal("the first message could not take the opening contract that was already queued")
	}
	if sequence.sendContract == nil || sequence.sendContract.contractId != openingContractId {
		t.Fatal("the first message did not take the opening contract that was already queued")
	}
	if got := oob.count(); got != 1 {
		t.Errorf("%d contracts requested for one message's sequence, want the one opening request that was already answered", got)
	}
}

// The control: what the change must not cost. A sustained transfer has to have
// its next contract in hand before each contract runs out, through the ramp's
// larger contracts too, with and without the receiver acknowledging an
// announced successor ahead of the switch.
//
// The proof that nothing waits is structural, not timed. The acquisition
// timeout is set to zero for the debit that the current contract cannot carry,
// so that debit succeeds only if the successor is already queued or announced;
// with nothing available it fails at once rather than waiting on the platform.
// The platform answers each request only after the sequence has charged
// another half megabyte, which at three megabytes a second is a 175 ms answer.
func TestASustainedTransferHasItsNextContractBeforeEachContractRunsOut(t *testing.T) {
	const messageByteCount = ByteCount(16 * 1024)
	const latencyByteCount = ByteCount(512 * 1024)
	const boundaryCount = 3
	for _, contractAhead := range []bool{false, true} {
		client, oob, sequence := newContractSuccessorTestSequence(t, false, false)
		takeContractSuccessorTestOpening(t, client, oob, sequence)
		if contractAhead {
			// the receiver advertised that it registers announced successors
			sequence.contractAheadSupported.Store(true)
		}
		createContractTimeout := sequence.sendBufferSettings.CreateContractTimeout

		chargedByteCount := ByteCount(0)
		// the charged total at which each request was made, by request index
		requestedAt := map[int]ByteCount{}
		grantedContractIds := map[Id]bool{}
		answerDueRequests := func() {
			for index := range oob.count() {
				if _, ok := requestedAt[index]; !ok {
					requestedAt[index] = chargedByteCount
				}
				if contractSuccessorTestRequestHeld(oob, index) &&
					requestedAt[index]+latencyByteCount <= chargedByteCount {
					grantedContractIds[grantContractSuccessorTestRequest(t, client, oob, index)] = true
				}
			}
		}

		for boundary := range boundaryCount {
			current := sequence.sendContract
			for sequence.sendContract == current {
				if !current.canUpdate(messageByteCount) {
					// the debit the current contract cannot carry, with no time
					// to wait for anything that is not already in hand
					sequence.sendBufferSettings.CreateContractTimeout = 0
				}
				updated := sequence.updateContract(messageByteCount)
				sequence.sendBufferSettings.CreateContractTimeout = createContractTimeout
				if !updated {
					t.Fatalf(
						"contract ahead %t, boundary %d: the transfer stalled at the end of a %d byte contract; its successor was not in hand (requests %d, %d bytes charged)",
						contractAhead,
						boundary,
						current.transferByteCount,
						oob.count(),
						chargedByteCount,
					)
				}
				chargedByteCount += messageByteCount
				answerDueRequests()
			}
			if !grantedContractIds[sequence.sendContract.contractId] {
				t.Fatalf("contract ahead %t, boundary %d: the transfer continued on a contract the platform never granted", contractAhead, boundary)
			}
		}
	}
}

// End to end, two clients and the real loops: one sends pings to the other,
// which echoes each back on a companion contract, the shape of a quality
// window's continuous ping and the provider's answer. The platform grants
// every request as it arrives. Each direction must take exactly one contract
// for the whole exchange.
func TestPingsAndTheirEchoEachTakeOneContract(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	aClientId := NewId()
	bClientId := NewId()
	aSend := make(chan []byte)
	bSend := make(chan []byte)
	_, bReceive := newConditioner(ctx, aSend)
	_, aReceive := newConditioner(ctx, bSend)
	provideModes := map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true}
	makeSettings := func() *ClientSettings {
		settings := DefaultClientSettingsWithBufferSize(64)
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.ControlPingTimeout = 0
		return settings
	}

	var a, b *Client
	aOob := &countingContractOob{grantingClientOob: grantingClientOob{
		sourceId: aClientId,
		settings: DefaultContractManagerSettings(),
		destSecretKey: func(destinationId Id) ([]byte, bool) {
			return b.ContractManager().GetProvideSecretKey(protocol.ProvideMode_Network)
		},
	}}
	bOob := &countingContractOob{grantingClientOob: grantingClientOob{
		sourceId: bClientId,
		settings: DefaultContractManagerSettings(),
		destSecretKey: func(destinationId Id) ([]byte, bool) {
			return a.ContractManager().GetProvideSecretKey(protocol.ProvideMode_Network)
		},
	}}
	a = NewClient(ctx, aClientId, aOob, makeSettings())
	b = NewClient(ctx, bClientId, bOob, makeSettings())
	for _, endpoint := range []struct {
		client  *Client
		send    chan []byte
		receive chan []byte
	}{
		{client: a, send: aSend, receive: aReceive},
		{client: b, send: bSend, receive: bReceive},
	} {
		endpoint.client.RouteManager().UpdateTransport(newDataGatewayTransport(), []Route{endpoint.send})
		endpoint.client.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{endpoint.receive})
		blackholeControlId(ctx, endpoint.client.RouteManager())
		endpoint.client.ContractManager().SetProvideModes(provideModes)
	}
	defer func() {
		cancel()
		for _, client := range []*Client{a, b} {
			client.Cancel()
			joinCtx, joinCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := client.CloseAndWait(joinCtx); err != nil {
				t.Error(err)
			}
			joinCancel()
		}
	}()

	const pingCount = 20
	// b answers on a goroutine it owns: a receive callback must not send
	echoes := make(chan struct{}, pingCount)
	b.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			if frame.MessageType == protocol.MessageType_TestSimpleMessage {
				select {
				case echoes <- struct{}{}:
				default:
				}
			}
		}
	})
	echoed := make(chan struct{}, pingCount)
	a.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			if frame.MessageType == protocol.MessageType_TestSimpleMessage {
				select {
				case echoed <- struct{}{}:
				default:
				}
			}
		}
	})
	go func() {
		for range pingCount {
			select {
			case <-ctx.Done():
				return
			case <-echoes:
			}
			frame, err := ToFrame(&protocol.SimpleMessage{Content: "echo"}, DefaultProtocolVersion)
			if err != nil {
				return
			}
			if !b.SendWithTimeout(frame, aClientId, func(error) {}, -1, CompanionContract()) {
				MessagePoolReturn(frame.MessageBytes)
			}
		}
	}()

	for index := range pingCount {
		frame, err := ToFrame(&protocol.SimpleMessage{Content: "ping"}, DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		if !a.SendWithTimeout(frame, bClientId, func(error) {}, -1) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatalf("ping %d was not admitted", index)
		}
		select {
		case <-echoed:
		case <-ctx.Done():
			t.Fatalf("ping %d was never echoed", index)
		}
	}

	if got := aOob.createCount.Load(); got != 1 {
		t.Errorf("the pinging client requested %d contracts for %d pings, want one", got, pingCount)
	}
	if got := bOob.createCount.Load(); got != 1 {
		t.Errorf("the echoing client requested %d companion contracts for %d echoes, want one", got, pingCount)
	}
}
