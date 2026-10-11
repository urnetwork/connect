package connect

// The rule that decides when a contract's successor is requested
// (`ContractPrefetchLeadTime`), row by row: the projection, the wake a
// no-acknowledgement caller sends the owner, the sequences that still request
// at take, and a successor that is already queued. The fixtures are those of
// transfer_contract_successor_test.go.

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The projection itself, on the row's clock: the rate is charged bytes over a
// known interval, so the row computes what the rule must decide at every
// charge. Charged over a hundred seconds, the contract is past its minimum
// long before it would run out within the lead time, and the request waits for
// the first charge at which it would; the same charges over one second are due
// as soon as they reach the minimum.
func TestTheSuccessorIsRequestedWhenTheContractWouldRunOutWithinTheLead(t *testing.T) {
	type projectionTestCase struct {
		name    string
		elapsed time.Duration
	}
	for _, c := range []projectionTestCase{
		{name: "slow", elapsed: 100 * time.Second},
		{name: "fast", elapsed: time.Second},
	} {
		client, oob, sequence := newContractSuccessorTestSequence(t, false, false)
		setTime := time.Unix(1_000_000, 0)
		now := setTime
		sequence.successorContractNowForTest = func() time.Time { return now }
		takeContractSuccessorTestOpening(t, client, oob, sequence)
		now = setTime.Add(c.elapsed)
		settings := sequence.sendBufferSettings
		contract := sequence.sendContract

		// what the rule decides at a charge, from the same terms
		dueAt := func(usedByteCount ByteCount) bool {
			headroomByteCount := contract.effectiveTransferByteCount - usedByteCount - settings.ContractAheadFloorByteCount
			return settings.ContractPrefetchMinUsedByteCount <= usedByteCount &&
				(headroomByteCount <= 0 ||
					float64(headroomByteCount)*c.elapsed.Seconds() <= settings.ContractPrefetchLeadTime.Seconds()*float64(usedByteCount))
		}
		const messageByteCount = ByteCount(4 * 1024)
		minimumChargeByteCount := ByteCount(0)
		for contract.canUpdate(messageByteCount) {
			if !sequence.updateContract(messageByteCount) {
				t.Fatalf("%s: the contract refused a message it has room for", c.name)
			}
			usedByteCount := contract.ackedByteCount + contract.unackedByteCount
			if minimumChargeByteCount == 0 && settings.ContractPrefetchMinUsedByteCount <= usedByteCount {
				minimumChargeByteCount = usedByteCount
			}
			requested := oob.count() == 2
			if requested != dueAt(usedByteCount) {
				t.Fatalf("%s: at %d of %d charged bytes over %s the successor is requested=%t, the projection says due=%t", c.name, usedByteCount, contract.effectiveTransferByteCount, c.elapsed, requested, dueAt(usedByteCount))
			}
			if requested {
				if c.name == "fast" && usedByteCount != minimumChargeByteCount {
					t.Errorf("fast: requested at %d charged bytes, after the %d byte minimum", usedByteCount, minimumChargeByteCount)
				}
				if c.name == "slow" && usedByteCount == minimumChargeByteCount {
					t.Errorf("slow: requested at the minimum, for a contract that would not run out within the lead")
				}
				break
			}
		}
		if oob.count() != 2 {
			t.Errorf("%s: the successor was never requested before the contract ran out", c.name)
		}
	}
}

// The control at measured rates, on the row's clock. Each charge advances the
// clock by the time the transfer takes to send it, and the platform answers a
// request only once its latency has passed on that clock. At every boundary
// the debit the current contract cannot carry runs with a zero acquisition
// timeout, so it succeeds only if the successor is already in hand. These are
// rates and latencies at which requesting at take never stalled either: a
// megabyte a second and two and a half against the 260 ms a device measures
// for a contract round trip, and ten megabytes a second against a 50 ms one.
func TestASustainedTransferAtItsRateHasItsNextContractInTime(t *testing.T) {
	type rateTestCase struct {
		byteCountPerSecond ByteCount
		latency            time.Duration
	}
	const messageByteCount = ByteCount(16 * 1024)
	const boundaryCount = 3
	for _, c := range []rateTestCase{
		{byteCountPerSecond: mib(1), latency: 260 * time.Millisecond},
		{byteCountPerSecond: mib(5) / 2, latency: 260 * time.Millisecond},
		{byteCountPerSecond: mib(10), latency: 50 * time.Millisecond},
	} {
		client, oob, sequence := newContractSuccessorTestSequence(t, false, false)
		now := time.Unix(1_000_000, 0)
		sequence.successorContractNowForTest = func() time.Time { return now }
		takeContractSuccessorTestOpening(t, client, oob, sequence)
		createContractTimeout := sequence.sendBufferSettings.CreateContractTimeout
		messageTime := time.Duration(float64(time.Second) * float64(messageByteCount) / float64(c.byteCountPerSecond))

		requestedAt := map[int]time.Time{}
		grantedContractIds := map[Id]bool{}
		answerDueRequests := func() {
			for index := range oob.count() {
				if _, ok := requestedAt[index]; !ok {
					requestedAt[index] = now
				}
				if contractSuccessorTestRequestHeld(oob, index) && !now.Before(requestedAt[index].Add(c.latency)) {
					grantedContractIds[grantContractSuccessorTestRequest(t, client, oob, index)] = true
				}
			}
		}
		for boundary := range boundaryCount {
			current := sequence.sendContract
			for sequence.sendContract == current {
				now = now.Add(messageTime)
				answerDueRequests()
				if !current.canUpdate(messageByteCount) {
					sequence.sendBufferSettings.CreateContractTimeout = 0
				}
				updated := sequence.updateContract(messageByteCount)
				sequence.sendBufferSettings.CreateContractTimeout = createContractTimeout
				if !updated {
					t.Fatalf(
						"%d bytes a second, %s latency, boundary %d: the transfer stalled at the end of a %d byte contract",
						c.byteCountPerSecond, c.latency, boundary, current.transferByteCount,
					)
				}
				answerDueRequests()
			}
			if !grantedContractIds[sequence.sendContract.contractId] {
				t.Fatalf("%d bytes a second, boundary %d: the transfer continued on a contract the platform never granted", c.byteCountPerSecond, boundary)
			}
		}
	}
}

// A no-acknowledgement write is charged on the owner's next pass, not when the
// caller writes it, so a sequence carrying nothing else would see its contract
// fill without deciding. The caller whose reservation reaches the armed check
// wakes the owner exactly once, and the owner's pass counts reservations not
// yet charged and requests the successor.
func TestANoAcknowledgementWriteThatReachesTheCheckWakesTheOwnerToRequest(t *testing.T) {
	client, oob, sequence := newContractSuccessorTestSequence(t, false, false)
	takeContractSuccessorTestOpening(t, client, oob, sequence)
	// the receiver acknowledged the opening contract, which is what lets a
	// caller write against it
	sequence.setContractAcked(sequence.sendContract, true)
	snapshot := sequence.noAckFastPath.Load()
	if snapshot == nil || snapshot.contract != sequence.sendContract {
		t.Fatal("the acknowledged contract was not published for callers to write against")
	}
	contract := sequence.sendContract
	minUsedByteCount := sequence.sendBufferSettings.ContractPrefetchMinUsedByteCount
	wantCheck := int64(contract.effectiveTransferByteCount - minUsedByteCount)
	if got := contract.successorCheckRemainingByteCount.Load(); got != wantCheck {
		t.Fatalf("the check is armed at %d bytes left, want %d, where the minimum is reached", got, wantCheck)
	}

	woken := func() bool {
		select {
		case <-sequence.noAckFastPathSettled:
			return true
		default:
			return false
		}
	}
	const writeByteCount = ByteCount(8 * 1024)
	written := ByteCount(0)
	for written+writeByteCount < minUsedByteCount {
		if !snapshot.reserve(writeByteCount) {
			t.Fatal("a caller could not reserve room the contract has")
		}
		written += writeByteCount
		if woken() {
			t.Fatalf("a caller woke the owner at %d bytes written, before the %d byte check", written, minUsedByteCount)
		}
	}
	for range 2 {
		if !snapshot.reserve(writeByteCount) {
			t.Fatal("a caller could not reserve room the contract has")
		}
	}
	if !woken() {
		t.Fatal("the caller whose reservation reached the check did not wake the owner, so the owner would decide only when the contract refused a write")
	}
	if !snapshot.reserve(writeByteCount) || woken() {
		t.Fatal("a later caller woke the owner again for the same check")
	}
	if oob.count() != 1 {
		t.Fatal("a caller's reservation requested a contract itself; the owner decides")
	}

	// the owner's pass
	sequence.maybeRequestSuccessorContract(0)
	if got := oob.count(); got != 2 {
		t.Fatalf("the owner's pass made %d requests in all, want the opening one and the successor; reservations not yet charged were not counted", got)
	}
	if got := contract.successorCheckRemainingByteCount.Load(); got != 0 {
		t.Errorf("the check is still armed at %d bytes after the successor was requested", got)
	}
}

// The exceptions keep the request at take. A same-network peer's contracts
// escrow nothing and the queue retains its newest successor past the expiry,
// so the early request is what makes its rollover immediate at any rate; and a
// zero lead time is the setting that restores the previous behavior.
func TestAnEagerSequenceStillRequestsItsSuccessorAtTake(t *testing.T) {
	type eagerTestCase struct {
		name        string
		networkPeer bool
		leadTime    time.Duration
	}
	for _, c := range []eagerTestCase{
		{name: "same-network peer", networkPeer: true, leadTime: DefaultSendBufferSettings().ContractPrefetchLeadTime},
		{name: "zero lead time", networkPeer: false, leadTime: 0},
	} {
		client, oob, sequence := newContractSuccessorTestSequence(t, false, c.networkPeer)
		sequence.sendBufferSettings.ContractPrefetchLeadTime = c.leadTime
		takeContractSuccessorTestOpening(t, client, oob, sequence)
		if got := oob.count(); got != 2 {
			t.Errorf("%s: %d contracts requested by the take, want the opening one and its successor", c.name, got)
			continue
		}
		if !sequence.successorContractEager {
			t.Errorf("%s: the sequence does not report its successor as requested eagerly", c.name)
		}
	}
}

// A successor already in the queue for this sequence is the successor: the
// rule coming due must not request another one beside it.
func TestASuccessorAlreadyQueuedIsNotRequestedAgain(t *testing.T) {
	client, oob, sequence := newContractSuccessorTestSequence(t, false, false)
	takeContractSuccessorTestOpening(t, client, oob, sequence)
	// an answer that arrived for this queue without this sequence asking
	client.ContractManager().CreateContract(sequence.contractMetadata().key, 1, 1024)
	grantContractSuccessorTestRequest(t, client, oob, 1)
	requests := oob.count()

	for range 8 {
		if !sequence.updateContract(16 * 1024) {
			t.Fatal("the contract refused a message it has room for")
		}
	}
	if !sequence.successorContractRequested {
		t.Fatal("the rule never came due for a contract charged past its minimum at once")
	}
	if got := oob.count(); got != requests {
		t.Errorf("%d more contracts requested with a successor already queued", got-requests)
	}
}

// An announced successor that is discarded before the switch, because its
// signed deadline passed, leaves the current contract with nothing after it.
// The contract must be free to request again, or the switch would wait on the
// platform.
func TestADiscardedAnnouncementLeavesTheContractFreeToRequestAgain(t *testing.T) {
	client, oob, sequence := newContractSuccessorTestSequence(t, false, false)
	takeContractSuccessorTestOpening(t, client, oob, sequence)
	sequence.contractAheadSupported.Store(true)
	const messageByteCount = ByteCount(16 * 1024)

	// charge until the successor is requested, answer it, and charge on
	// until it is announced
	for oob.count() == 1 {
		if !sequence.updateContract(messageByteCount) {
			t.Fatal("the contract refused a message it has room for")
		}
	}
	grantContractSuccessorTestRequest(t, client, oob, 1)
	for sequence.aheadSendContract == nil {
		if !sequence.sendContract.canUpdate(messageByteCount) {
			t.Fatal("the contract ran out without announcing the successor it had queued")
		}
		if !sequence.updateContract(messageByteCount) {
			t.Fatal("the contract refused a message it has room for")
		}
	}

	// the announced successor's deadline passes before the switch
	expired := time.Now().Add(-time.Second).UnixMilli()
	sequence.aheadSendContract.expirationTimeUnixMilli = &expired
	for range 2 {
		if !sequence.updateContract(messageByteCount) {
			t.Fatal("the contract refused a message it has room for")
		}
	}
	if sequence.aheadSendContract != nil {
		t.Fatal("the expired announced successor was not discarded")
	}
	if got := oob.count(); got != 3 {
		t.Errorf("%d contracts requested in all, want the opening one, the discarded successor and its replacement", got)
	}
}

// Holds each contract request until the row grants it, then grants a contract
// the destination can verify, as `grantingClientOob` does. Announces each
// request as it arrives.
type heldContractGrantOob struct {
	grantingClientOob
	stateLock sync.Mutex
	frames    []*protocol.Frame
	callbacks []OobResultFunction
	arrived   chan int
}

func (self *heldContractGrantOob) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	var request *protocol.Frame
	for _, frame := range frames {
		if message, err := FromFrame(frame); err == nil {
			if _, ok := message.(*protocol.CreateContract); ok {
				request = &protocol.Frame{
					MessageType:  frame.MessageType,
					MessageBytes: append([]byte(nil), frame.MessageBytes...),
				}
			}
		}
	}
	if request == nil {
		callback(nil, nil)
		return
	}
	index := func() int {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.frames = append(self.frames, request)
		self.callbacks = append(self.callbacks, callback)
		return len(self.frames) - 1
	}()
	select {
	case self.arrived <- index:
	default:
		panic("held contract request arrivals overflowed the fixture")
	}
}

// Grants held request `index` on the caller's goroutine.
func (self *heldContractGrantOob) grant(index int) {
	frame, callback := func() (*protocol.Frame, OobResultFunction) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		callback := self.callbacks[index]
		self.callbacks[index] = nil
		return self.frames[index], callback
	}()
	if callback != nil {
		self.grantingClientOob.SendControl([]*protocol.Frame{frame}, callback)
	}
}

// Releases every held request with a cancellation after the clients close.
func (self *heldContractGrantOob) cancelHeld() {
	self.stateLock.Lock()
	callbacks := self.callbacks
	self.callbacks = nil
	self.stateLock.Unlock()
	for _, callback := range callbacks {
		if callback != nil {
			callback(nil, context.Canceled)
		}
	}
}

// End to end for a sequence that carries nothing but no-acknowledgement Packs
// once its contract is acknowledged, the shape of a QUIC flow through a window
// exit. Callers write on the fast path and the owner's loop does not run for
// their writes, so only the wake from the caller that reaches the armed check
// can bring the successor due before the contract refuses a write. The row
// proves that wake runs the owner's pass: every write before the request is a
// fast-path write. The lead time is set far beyond any interval this row can
// take, so the request is due at the first wake on any machine; the rate rule
// has its own row.
func TestANoAcknowledgementTransferRequestsItsSuccessorWhileTheContractHasRoom(t *testing.T) {
	// a deadlock guard only; every wait below ends on an event
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	aClientId := NewId()
	bClientId := NewId()
	aSend := make(chan []byte, 4096)
	bSend := make(chan []byte, 4096)
	_, bReceive := newConditioner(ctx, aSend)
	_, aReceive := newConditioner(ctx, bSend)
	provideModes := map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true}

	var a, b *Client
	aOob := &heldContractGrantOob{
		grantingClientOob: grantingClientOob{
			sourceId: aClientId,
			settings: DefaultContractManagerSettings(),
			destSecretKey: func(destinationId Id) ([]byte, bool) {
				return b.ContractManager().GetProvideSecretKey(protocol.ProvideMode_Network)
			},
		},
		arrived: make(chan int, 64),
	}
	bOob := &grantingClientOob{
		sourceId: bClientId,
		settings: DefaultContractManagerSettings(),
		destSecretKey: func(destinationId Id) ([]byte, bool) {
			return a.ContractManager().GetProvideSecretKey(protocol.ProvideMode_Network)
		},
	}
	var fastPathWriteCount, queuedWriteCount atomic.Int64
	aSettings := DefaultClientSettingsWithBufferSize(64)
	aSettings.EncryptionSettings.Mode = EncryptionModeOff
	aSettings.ControlPingTimeout = 0
	aSettings.SendBufferSettings.ContractPrefetchLeadTime = 1000 * time.Hour
	aSettings.SendBufferSettings.afterNoAckFastPathForTest = func(id sendSequenceId, attempted bool, written bool, timeout time.Duration) {
		if id.Destination != bClientId {
			return
		}
		if written {
			fastPathWriteCount.Add(1)
		} else {
			queuedWriteCount.Add(1)
		}
	}
	bSettings := DefaultClientSettingsWithBufferSize(64)
	bSettings.EncryptionSettings.Mode = EncryptionModeOff
	bSettings.ControlPingTimeout = 0
	a = NewClient(ctx, aClientId, aOob, aSettings)
	b = NewClient(ctx, bClientId, bOob, bSettings)
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
		}
		aOob.cancelHeld()
		for _, client := range []*Client{a, b} {
			joinCtx, joinCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := client.CloseAndWait(joinCtx); err != nil {
				t.Error(err)
			}
			joinCancel()
		}
	}()
	awaitRequest := func(what string) int {
		t.Helper()
		select {
		case index := <-aOob.arrived:
			return index
		case <-ctx.Done():
			t.Fatalf("no contract request for %s", what)
			return -1
		}
	}
	sendAcknowledged := func(content string) {
		t.Helper()
		acked := make(chan error, 1)
		frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: content})
		if !a.SendWithTimeout(frame, bClientId, func(err error) { acked <- err }, -1) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatalf("%s was not admitted", content)
		}
		select {
		case err := <-acked:
			if err != nil {
				t.Fatalf("%s was not acknowledged: %v", content, err)
			}
		case <-ctx.Done():
			t.Fatalf("%s was never acknowledged", content)
		}
	}

	// the opening contract, acknowledged, which is what publishes the fast path
	opened := make(chan struct{})
	go func() {
		defer close(opened)
		sendAcknowledged("open")
	}()
	aOob.grant(awaitRequest("the opening contract"))
	<-opened
	sequence := a.sendBuffer.lookupSendSequence(sendSequenceId{Destination: bClientId}, nil)
	if sequence == nil {
		t.Fatal("the opening message left no send sequence")
	}
	snapshot := sequence.noAckFastPath.Load()
	if snapshot == nil || snapshot.contract == nil {
		t.Fatal("the acknowledged opening contract was not published for callers to write against")
	}
	effectiveByteCount := snapshot.contract.effectiveTransferByteCount

	payload := strings.Repeat("x", 1000)
	sendNoAck := func() ByteCount {
		frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: payload})
		byteCount := ByteCount(len(frame.MessageBytes))
		if !a.SendWithTimeout(frame, bClientId, func(error) {}, time.Second, NoAck()) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("a no-acknowledgement message was not admitted")
		}
		return byteCount
	}
	writtenByteCount := ByteCount(0)
	minUsedByteCount := aSettings.SendBufferSettings.ContractPrefetchMinUsedByteCount
	for writtenByteCount < minUsedByteCount {
		writtenByteCount += sendNoAck()
	}
	successorIndex := awaitRequest("the successor while the contract had room")
	if queued := queuedWriteCount.Load(); queued != 0 {
		t.Fatalf("%d writes went through the owner's loop, so this row did not exercise the caller's wake", queued)
	}
	if room := effectiveByteCount - writtenByteCount; room <= aSettings.SendBufferSettings.ContractAheadFloorByteCount {
		t.Errorf("the successor was requested with %d bytes of room, at or below the announcement floor", room)
	}
	aOob.grant(successorIndex)

	// past the end of the opening contract, onto the successor
	for writtenByteCount < effectiveByteCount+effectiveByteCount/2 {
		writtenByteCount += sendNoAck()
	}
	sendAcknowledged("after the switch")
	if got := a.ContractManager().LocalStats().ContractOpenCount; got != 2 {
		t.Errorf("the transfer took %d contracts, want the opening one and its successor", got)
	}
	if 0 == fastPathWriteCount.Load() {
		t.Error("no write took the fast path")
	}
}
