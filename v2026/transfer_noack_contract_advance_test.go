package connect

// FILLFRACTION.md. A no-acknowledgement Pack advances its sequence's contract
// itself, the way an acknowledged Pack does, without being promoted onto the
// acknowledged lane to do it. Acknowledged traffic stops at the fill
// (ContractFillFraction, 0.9), and the headroom above it carries
// no-acknowledgement traffic while the next contract is introduced
// (transfer_noack_contract_headroom_test.go).
//
// The root causes these rows pin, as the tree before the change had them:
//
//   - a no-acknowledgement Pack at a contract boundary could not switch to a
//     successor the receiver had already acknowledged: the switch was gated on
//     permission to promote (updateContractWithAckPromotion), so the Pack was
//     deferred to recovery admission and then promoted while a successor
//     opened unacknowledged;
//   - a sequence carrying only no-acknowledgement traffic never announced a
//     successor ahead: the announcement returned whenever no reliable item was
//     outstanding (maybeAnnounceContractAhead), which on such a sequence is
//     always, so every boundary opened its successor unacknowledged;
//   - nothing ever charged the headroom above the fill, the one share of a
//     contract that a no-acknowledgement Pack can ride during a switch.
//
// Each row reads the cause rather than a symptom of it, and the composed rows
// read it at the public send entry and on the wire between two real clients.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

// The switch. A successor announced ahead and acknowledged is already open at
// the receiver, so a no-acknowledgement Pack that the current contract cannot
// carry may take it with no round trip and nothing promoted, exactly as an
// acknowledged Pack does. Before the change the same call deferred the Pack
// to recovery admission, where it was promoted onto a newly opened contract.
func TestANoAckPackAdvancesToTheAcknowledgedSuccessorItself(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, sequence, destinationId, contract := newSendNoContractHarness(t, ctx)
	// nothing left in the current contract: the next message is the boundary
	contract.ackedByteCount = contract.effectiveTransferByteCount
	ahead := newContractAheadTestContract(t, client, destinationId)
	ahead.acknowledgedAhead = true
	sequence.openSendContracts[ahead.contractId] = ahead
	sequence.aheadSendContract = ahead
	sequence.aheadSendContractMetadataGeneration = sequence.contractMetadata().generation

	const messageByteCount = ByteCount(1024)
	updated, deferForRecoveryAdmission, err := sequence.updateContractWithoutAckPromotionOutcome(messageByteCount)
	if err != nil {
		t.Fatalf("the boundary returned an error: %v", err)
	}
	if !updated || deferForRecoveryAdmission {
		t.Fatalf(
			"a no-ack pack at a contract boundary did not advance to the acknowledged successor: updated=%t deferred=%t; deferred, it waits for recovery admission and is promoted onto a successor opened unacknowledged",
			updated, deferForRecoveryAdmission,
		)
	}
	if sequence.sendContract != ahead {
		t.Fatal("the acknowledged successor did not become the current contract")
	}
	if !sequence.sendContractAcked {
		t.Fatal("the successor became current unacknowledged, so the pack would be promoted")
	}
	if ahead.ackedByteCount+ahead.unackedByteCount < messageByteCount {
		t.Errorf(
			"the message was not debited to the successor it advanced to: %d + %d against %d bytes",
			ahead.ackedByteCount, ahead.unackedByteCount, messageByteCount,
		)
	}
}

// The scheduler's half of the same cause. A no-acknowledgement Pack is taken
// regardless of resend capacity only while the bypass predicate holds, and it
// did not hold for a Pack the current contract cannot carry even when an
// acknowledged successor could: such a Pack waited behind resend capacity it
// never uses, at every boundary, which is when capacity is most often short.
func TestANoAckPackAtAContractBoundaryNeedsNoRecoveryAdmission(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, sequence, destinationId, contract := newSendNoContractHarness(t, ctx)
	contract.ackedByteCount = contract.effectiveTransferByteCount
	ahead := newContractAheadTestContract(t, client, destinationId)
	ahead.acknowledgedAhead = true
	sequence.openSendContracts[ahead.contractId] = ahead
	sequence.aheadSendContract = ahead
	sequence.aheadSendContractMetadataGeneration = sequence.contractMetadata().generation

	pack := &SendPack{
		TransferOptions: TransferOptions{Ack: false},
		Frame:           noAckFastPathTestFrame(t),
		Destination:     destinationId,
		Ctx:             ctx,
	}
	defer MessagePoolReturn(pack.Frame.MessageBytes)
	if !sequence.noAckPackCanBypassRecoveryAdmission(pack) {
		t.Fatal("a no-ack pack the acknowledged successor can carry still waits for recovery admission at the boundary")
	}

	// and without an acknowledged successor it may not bypass: it has to
	// advance first, which is not the bypass's to decide
	ahead.acknowledgedAhead = false
	if sequence.noAckPackCanBypassRecoveryAdmission(pack) {
		t.Fatal("a no-ack pack bypassed recovery admission onto a successor the receiver has not acknowledged")
	}
}

// The announcement on a sequence with nothing reliable outstanding. Such a
// sequence is what an ordinary UDP flow is, and before the change it never
// announced: the announcement may not be a head, and with nothing outstanding
// it would have been one. The change sends the head first, under the current
// contract, and announces behind it.
func TestANoAckOnlySequenceAnnouncesItsSuccessorAhead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, sequence, destinationId, contract := newSendNoContractHarness(t, ctx)
	sequence.sendBuffer = client.sendBuffer
	contract.contract = noAckAdvanceTestContract(t, client.ClientId(), destinationId, kib(100))
	route := make(chan []byte, 4)
	client.RouteManager().UpdateTransport(
		NewSendClientTransport(DestinationId(destinationId)),
		[]Route{route},
	)
	t.Cleanup(func() {
		for len(route) != 0 {
			MessagePoolReturn(<-route)
		}
	})
	// a peer that advertised the capability, and a successor in the queue
	sequence.contractAheadSupported.Store(true)
	successor := noAckAdvanceTestContract(t, client.ClientId(), destinationId, kib(100))
	if err := client.ContractManager().addContract(sequence.contractMetadata().key, successor); err != nil {
		t.Fatalf("queue the successor: %v", err)
	}
	if len(sequence.sendItems) != 0 {
		t.Fatal("the fixture has a reliable item outstanding, so this row reads nothing")
	}

	sequence.maybeAnnounceContractAhead()

	if sequence.aheadSendContract == nil {
		t.Fatal("a sequence with nothing reliable outstanding never announced its successor, so every boundary it reaches opens the successor unacknowledged")
	}
	var packs []*protocol.Pack
	for len(route) != 0 {
		transferFrameBytes := <-route
		packs = append(packs, decodeCompactContractTestPack(t, transferFrameBytes))
		MessagePoolReturn(transferFrameBytes)
	}
	if len(packs) != 2 {
		t.Fatalf("%d packs on the route, want the head under the current contract and the announcement", len(packs))
	}
	if !packs[0].GetHead() || packs[0].GetContractAhead() {
		t.Error("the first pack is not a head under the current contract, so the announcement would be the head")
	}
	if packs[1].GetHead() || !packs[1].GetContractAhead() {
		t.Error("the second pack is not the announcement")
	}
	for _, pack := range packs {
		if 0 < len(pack.GetFrames()) {
			t.Errorf("a control pack carried %d data frames", len(pack.GetFrames()))
		}
	}
}

// The fill fraction (FILLFRACTION.md). Acknowledged and ordinary traffic
// stops at `transferByteCount * ContractFillFraction`, 0.9 by default. The
// rest of the signed size is headroom that only no-acknowledgement Packs
// charge, during a switch, so the contract's total can reach its signed size
// with no-acknowledgement traffic and never pass it. A rollback returns
// headroom, never the fast path's budget.
func TestAcknowledgedTrafficStopsAtTheFillAndTheHeadroomCarriesNoAckToTheSignedSize(t *testing.T) {
	settings := DefaultSendBufferSettings()
	if settings.ContractFillFraction != 0.9 {
		t.Fatalf("the default fill fraction is %.2f, want 0.9", settings.ContractFillFraction)
	}
	sourceId, destinationId := NewId(), NewId()
	contract := noAckAdvanceTestContract(t, sourceId, destinationId, mib(1))
	sendContract, err := newSequenceContract(
		NewNoopLogger(),
		"s",
		contract,
		settings.MinMessageByteCount,
		settings.ContractFillFraction,
	)
	if err != nil {
		t.Fatal(err)
	}
	fill := sendContract.effectiveTransferByteCount
	if want := ByteCount(float32(mib(1)) * 0.9); fill != want {
		t.Fatalf("the fill is %d of %d signed bytes, want %d", fill, sendContract.transferByteCount, want)
	}
	if !sendContract.update(fill) {
		t.Fatal("ordinary traffic was refused before the fill")
	}
	if sendContract.update(1) {
		t.Fatal("ordinary traffic was charged past the fill")
	}
	headroom := sendContract.transferByteCount - fill
	if !sendContract.updateNoAckHeadroom(headroom) {
		t.Fatalf("the headroom refused %d bytes of no-ack traffic", headroom)
	}
	if used := sendContract.ackedByteCount + sendContract.unackedByteCount; used != sendContract.transferByteCount {
		t.Fatalf("the contract carries %d bytes, want its signed %d", used, sendContract.transferByteCount)
	}
	if sendContract.canUpdateNoAckHeadroom(1) || sendContract.updateNoAckHeadroom(1) {
		t.Fatal("the headroom charged a byte past the signed size")
	}
	sendContract.rollbackUnwrittenNoAckHeadroom(headroom)
	if remaining := sendContract.noAckHeadroomRemainingByteCount(); remaining != headroom {
		t.Fatalf("a rolled-back headroom debit left %d of %d headroom bytes", remaining, headroom)
	}
}

// The receiver's half, pinned rather than changed: a contract filled to its
// signed size is admitted, and one byte past it is not. This is what makes a
// sender that fills to the whole contract safe against every receiver.
func TestAReceiverAdmitsAContractFilledToItsSignedSize(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	receiver, sequence, sourceId := newContractAheadTestReceiveSequence(t, ctx)
	contractFrame := contractAheadTestContractFrame(t, receiver, sourceId, receiver.ClientId())
	contractId := contractAheadTestContractId(t, contractFrame)
	if err := sequence.registerContracts(&receiveItem{contractFrame: contractFrame}); err != nil {
		t.Fatalf("register the contract: %v", err)
	}
	full := &receiveItem{
		transferItem: transferItem{messageByteCount: mib(1)},
		contractId:   &contractId,
	}
	if !sequence.updateContract(full) {
		t.Fatal("the receiver refused a no-ack pack that fills its contract to the signed size")
	}
	past := &receiveItem{
		transferItem: transferItem{messageByteCount: 1},
		contractId:   &contractId,
	}
	if sequence.updateContract(past) {
		t.Fatal("the receiver admitted a byte past the contract's signed size")
	}
}

// Contracts advance on a sequence that carries nothing but no-acknowledgement
// traffic, through the public send entry, between two real clients: every
// message is delivered once, contracts advance across at least three
// boundaries, and no no-acknowledgement message crosses the wire on the
// acknowledged lane. Before the change the message that met each boundary
// opened the successor and was promoted onto the acknowledged lane with it.
//
// The opening contract is opened by a reliable message first, so every
// contract after it is a boundary that no-acknowledgement traffic meets.
func TestANoAckOnlySequenceCrossesContractBoundariesWithoutPromotion(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNoAckAdvanceFixture(t)
		defer f.close()
		f.finishSend(f.startSend("opening"))

		const count = 160
		payload := strings.Repeat("x", 4000)
		for i := range count {
			f.finishSend(f.startSend(fmt.Sprintf("noack-%03d-%s", i, payload), NoAck()))
		}

		f.stateLock.Lock()
		defer f.stateLock.Unlock()
		for i := range count {
			content := fmt.Sprintf("noack-%03d-%s", i, payload)
			if n := f.deliveries[content]; n != 1 {
				t.Errorf("no-ack message %d was delivered %d times, want once", i, n)
			}
		}
		promoted := 0
		contractIds := map[Id]bool{}
		for _, observed := range f.wire {
			if !strings.HasPrefix(observed.content, "noack-") {
				continue
			}
			if !observed.nack {
				promoted += 1
				continue
			}
			if observed.hasContractId {
				contractIds[observed.contractId] = true
			}
		}
		if promoted != 0 {
			t.Errorf("%d no-ack messages crossed the wire on the acknowledged lane; a boundary promoted them", promoted)
		}
		if len(contractIds) < 4 {
			t.Errorf("no-ack traffic used %d contracts, want at least four (three boundaries)", len(contractIds))
		}
	})
}

// A contract switched to from its announcement, once retired, closes with its
// count. The announcement charged the successor an opening reservation that no
// Pack under it carries and the receiver never counts, and nothing released
// it, so the retired contract kept an unacknowledged byte for its whole life
// and never closed: the payer's escrow for it stayed held until the sequence
// ended or the platform expired the contract an hour later. Reliable traffic
// pipelined in pairs announces its successors on the tree before the change,
// which is what this row needs to read the cause there.
func TestARetiredAnnouncedContractClosesWithItsCount(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNoAckAdvanceFixture(t)
		defer f.close()
		payload := strings.Repeat("x", 4000)
		for i := 0; i < 160; i += 2 {
			first := f.startSend(fmt.Sprintf("rel-%03d-%s", i, payload))
			second := f.startSend(fmt.Sprintf("rel-%03d-%s", i+1, payload))
			f.finishSend(first)
			f.finishSend(second)
		}
		f.settle()
		if switches := f.sender.SendRecoveryStats().ContractAheadSwitchCount; switches < 3 {
			t.Fatalf("the sequence switched to an announced successor %d times, want at least three", switches)
		}
		sender, _ := f.sequences()
		f.stateLock.Lock()
		contractIds := []Id{}
		for contractId := range f.signedContracts {
			contractIds = append(contractIds, contractId)
		}
		f.stateLock.Unlock()
		retiredIds := []Id{}
		for _, contractId := range contractIds {
			if sender.sendContract != nil && contractId == sender.sendContract.contractId {
				continue
			}
			if sender.aheadSendContract != nil && contractId == sender.aheadSendContract.contractId {
				continue
			}
			retiredIds = append(retiredIds, contractId)
			if contract, open := sender.openSendContracts[contractId]; open {
				t.Errorf(
					"retired contract %s is still open with %d acknowledged and %d unacknowledged bytes",
					contractId, contract.ackedByteCount, contract.unackedByteCount,
				)
			}
		}
		if len(retiredIds) < 3 {
			t.Errorf("%d contracts retired, want at least three", len(retiredIds))
		}
		// the close reports leave through the control channel, flushed by the
		// client's close
		f.close()
		for _, contractId := range retiredIds {
			reports := f.authority.terminalReports(contractId)
			if len(reports) != 1 {
				t.Errorf("retired contract %s has %d final reports, want one", contractId, len(reports))
				continue
			}
			if reports[0].UnackedByteCount != 0 {
				t.Errorf("retired contract %s closed with %d unacknowledged bytes", contractId, reports[0].UnackedByteCount)
			}
		}
	})
}

// One routed Pack, as the forward pump saw it.
type noAckAdvanceWireRecord struct {
	content       string
	nack          bool
	contractAhead bool
	head          bool
	hasContractId bool
	contractId    Id
}

// Two real clients joined by pumps that decode what crosses the wire. The
// platform is the expiration fixture's authority, granting long-lived
// contracts; contracts are small so the ladder reaches several boundaries in
// a few hundred kilobytes. Time is fake (synctest) and advances only when
// every goroutine is blocked.
type noAckAdvanceFixture struct {
	t                    *testing.T
	cancel               context.CancelFunc
	sender               *Client
	receiver             *Client
	authority            *expirationLifecycleAuthority
	reverseAuthority     *expirationLifecycleAuthority
	stateLock            sync.Mutex
	deliveries           map[string]int
	deliveryOrder        []string
	wire                 []noAckAdvanceWireRecord
	signedContracts      map[Id]ByteCount
	dropAnnouncements    int
	droppedAnnouncements int
	// FILLFRACTION.md. When set, every acknowledged Pack that introduces a
	// contract (an announcement, or any Pack carrying a contract frame) is
	// held in the network this long. No-acknowledgement Packs are not held,
	// so they overtake it: they arrive out of order with the new contract.
	introductionDelay    time.Duration
	delayedIntroductions int
	// the first contract the pump saw, the opening's, whose own
	// introduction is never delayed
	firstContractId   Id
	haveFirstContract bool
	routes            []Route
	pumps             sync.WaitGroup
	closed            bool
}

func newNoAckAdvanceFixture(t *testing.T, configure ...func(*ClientSettings)) *noAckAdvanceFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	f := &noAckAdvanceFixture{
		t:               t,
		cancel:          cancel,
		deliveries:      map[string]int{},
		signedContracts: map[Id]ByteCount{},
	}
	settings := func() *ClientSettings {
		value := DefaultClientSettingsWithBufferSize(64)
		value.Log = NewNoopLogger()
		value.ControlPingTimeout = 0
		value.EncryptionSettings.Mode = EncryptionModeOff
		value.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Unix(1, 0)
		value.ContractManagerSettings.NetworkEventTimeChangeHmac = time.Unix(1, 0)
		value.ContractManagerSettings.LegacyCreateContract = false
		value.ContractManagerSettings.InitialContractTransferByteCount = kib(64)
		value.ContractManagerSettings.StandardContractTransferByteCount = kib(256)
		value.ContractManagerSettings.ContractQueueExpireTimeout = time.Hour
		value.SendBufferSettings.MinResendInterval = 2 * time.Second
		value.SendBufferSettings.RttMinResendInterval = 2 * time.Second
		value.SendBufferSettings.MaxResendInterval = 2 * time.Second
		value.SendBufferSettings.AckTailProbeLimit = 0
		// announce while a few messages of room remain; the fake round trip
		// is zero, so the threshold is the floor
		value.SendBufferSettings.ContractAheadFloorByteCount = kib(32)
		value.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		for _, change := range configure {
			change(value)
		}
		return value
	}
	senderId, receiverId := NewId(), NewId()
	deadline := time.Now().Add(24 * time.Hour)
	f.authority = &expirationLifecycleAuthority{
		ctx: ctx, sourceId: senderId, peer: func() *Client { return f.receiver },
		initialDeadline: deadline, grantDeadlines: map[Id]time.Time{}, renewalEntered: make(chan struct{}),
	}
	f.reverseAuthority = &expirationLifecycleAuthority{
		ctx: ctx, sourceId: receiverId, peer: func() *Client { return f.sender },
		initialDeadline: deadline, grantDeadlines: map[Id]time.Time{}, renewalEntered: make(chan struct{}),
	}
	f.sender = NewClient(ctx, senderId, f.authority, settings())
	f.receiver = NewClient(ctx, receiverId, f.reverseAuthority, settings())
	for _, client := range []*Client{f.sender, f.receiver} {
		client.ContractManager().SetProvideModes(map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
	}
	forward, forwardDelivered := make(Route, 64), make(Route, 64)
	reverse, reverseDelivered := make(Route, 64), make(Route, 64)
	f.routes = []Route{forward, forwardDelivered, reverse, reverseDelivered}
	pump := func(input, output Route, observe bool) {
		defer f.pumps.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case wire := <-input:
				if observe {
					var transfer protocol.TransferFrame
					if err := proto.Unmarshal(wire, &transfer); err != nil {
						panic(err)
					}
					if pack := transfer.Pack; pack != nil {
						record := noAckAdvanceWireRecord{
							nack:          pack.Nack,
							contractAhead: pack.ContractAhead,
							head:          pack.Head,
						}
						if 0 < len(pack.ContractId) {
							if contractId, err := IdFromBytes(pack.ContractId); err == nil {
								record.hasContractId = true
								record.contractId = contractId
							}
						}
						drop := false
						delay := time.Duration(0)
						f.stateLock.Lock()
						introducesSuccessor := pack.ContractAhead
						if contractId, transferByteCount, ok := noAckAdvanceSignedContract(pack.ContractFrame); ok {
							f.signedContracts[contractId] = transferByteCount
							if !f.haveFirstContract {
								f.firstContractId = contractId
								f.haveFirstContract = true
							}
							if contractId != f.firstContractId {
								introducesSuccessor = true
							}
						}
						if pack.ContractAhead && f.droppedAnnouncements < f.dropAnnouncements {
							f.droppedAnnouncements += 1
							drop = true
						}
						if !pack.Nack && 0 < f.introductionDelay && introducesSuccessor {
							delay = f.introductionDelay
							f.delayedIntroductions += 1
						}
						for _, frame := range pack.Frames {
							if frame.MessageType != protocol.MessageType_TestSimpleMessage {
								continue
							}
							var message protocol.SimpleMessage
							if err := proto.Unmarshal(frame.MessageBytes, &message); err != nil {
								panic(err)
							}
							observed := record
							observed.content = message.Content
							f.wire = append(f.wire, observed)
						}
						if len(pack.Frames) == 0 {
							f.wire = append(f.wire, record)
						}
						f.stateLock.Unlock()
						if drop {
							MessagePoolReturn(wire)
							continue
						}
						if 0 < delay {
							// the pump itself holds a count, so this Add never
							// races the close's Wait
							f.pumps.Add(1)
							go func(wire []byte) {
								defer f.pumps.Done()
								select {
								case <-time.After(delay):
								case <-ctx.Done():
									MessagePoolReturn(wire)
									return
								}
								select {
								case output <- wire:
								case <-ctx.Done():
									MessagePoolReturn(wire)
								}
							}(wire)
							continue
						}
					}
				}
				select {
				case output <- wire:
				case <-ctx.Done():
					MessagePoolReturn(wire)
					return
				}
			}
		}
	}
	f.pumps.Add(2)
	go pump(forward, forwardDelivered, true)
	go pump(reverse, reverseDelivered, false)
	f.sender.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(receiverId)), []Route{forward})
	f.receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{forwardDelivered})
	f.receiver.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(senderId)), []Route{reverse})
	f.sender.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{reverseDelivered})
	receive := func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			if frame.MessageType == protocol.MessageType_TestSimpleMessage {
				var message protocol.SimpleMessage
				if err := proto.Unmarshal(frame.MessageBytes, &message); err != nil {
					panic(err)
				}
				f.stateLock.Lock()
				f.deliveries[message.Content]++
				f.deliveryOrder = append(f.deliveryOrder, message.Content)
				f.stateLock.Unlock()
			}
		}
	}
	f.receiver.AddReceiveCallback(receive)
	return f
}

// The barrier and Client join finish every owner before the test reads
// reports or reconciles pooled bytes left in the routes.
func (self *noAckAdvanceFixture) close() {
	if self.closed {
		return
	}
	self.closed = true
	self.cancel()
	self.pumps.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, client := range []*Client{self.sender, self.receiver} {
		if err := client.CloseAndWait(ctx); err != nil {
			self.t.Error(err)
		}
	}
	for _, route := range self.routes {
		for len(route) != 0 {
			MessagePoolReturn(<-route)
		}
	}
}

// Takes on success: a refused frame is returned here.
func (self *noAckAdvanceFixture) startSend(content string, options ...any) <-chan error {
	self.t.Helper()
	ack := make(chan error, 1)
	frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: content})
	if !self.sender.SendWithTimeout(frame, self.receiver.ClientId(), func(err error) { ack <- err }, time.Second, options...) {
		MessagePoolReturn(frame.MessageBytes)
		self.t.Fatal("the transfer was not admitted")
	}
	return ack
}

// The timeout is only a deadlock guard; ordering is the completion itself,
// then quiescence, so the receiver has handled whatever the sender wrote.
func (self *noAckAdvanceFixture) finishSend(ack <-chan error) {
	self.t.Helper()
	select {
	case err := <-ack:
		if err != nil {
			self.t.Fatal("the transfer failed", err)
		}
	case <-time.After(20 * time.Second):
		self.t.Fatal("the transfer never completed")
	}
	synctest.Wait()
}

// A contract for `sourceId`, unsigned: the send side never verifies, and the
// manager's queue only reads the stored bytes.
func noAckAdvanceTestContract(t *testing.T, sourceId Id, destinationId Id, transferByteCount ByteCount) *protocol.Contract {
	t.Helper()
	storedContractBytes, err := ProtoMarshal(&protocol.StoredContract{
		ContractId:        NewId().Bytes(),
		TransferByteCount: uint64(transferByteCount),
		SourceId:          sourceId.Bytes(),
		DestinationId:     destinationId.Bytes(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &protocol.Contract{
		StoredContractBytes: storedContractBytes,
		ProvideMode:         protocol.ProvideMode_Network,
	}
}

// The signed size of a contract frame the pump saw, keyed by contract id.
func noAckAdvanceSignedContract(frame *protocol.Frame) (Id, ByteCount, bool) {
	if frame == nil {
		return Id{}, 0, false
	}
	var contract protocol.Contract
	if err := proto.Unmarshal(frame.MessageBytes, &contract); err != nil {
		return Id{}, 0, false
	}
	var storedContract protocol.StoredContract
	if err := proto.Unmarshal(contract.StoredContractBytes, &storedContract); err != nil {
		return Id{}, 0, false
	}
	contractId, err := IdFromBytes(storedContract.ContractId)
	if err != nil {
		return Id{}, 0, false
	}
	return contractId, ByteCount(storedContract.TransferByteCount), true
}

// Every non-checkpoint report this authority received, as the bytes each
// contract was closed with.
func (self *expirationLifecycleAuthority) terminalReportsByContract() map[Id]ByteCount {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	byteCounts := map[Id]ByteCount{}
	for _, report := range self.reports {
		if report.Checkpoint {
			continue
		}
		contractId := RequireIdFromBytes(report.ContractId)
		byteCounts[contractId] = ByteCount(report.AckedByteCount + report.UnackedByteCount)
	}
	return byteCounts
}

// Takes on success, with the caller's budget given.
func (self *noAckAdvanceFixture) startSendWithTimeout(content string, timeout time.Duration, options ...any) <-chan error {
	self.t.Helper()
	ack := make(chan error, 1)
	frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: content})
	if !self.sender.SendWithTimeout(frame, self.receiver.ClientId(), func(err error) { ack <- err }, timeout, options...) {
		MessagePoolReturn(frame.MessageBytes)
		self.t.Fatal("the transfer was not admitted")
	}
	return ack
}

// Lets fake time pass the receiver's acknowledgement timers, so every control
// Pack the sender wrote last is acknowledged before a row reads accounting.
func (self *noAckAdvanceFixture) settle() {
	self.t.Helper()
	time.Sleep(time.Second)
	synctest.Wait()
}

// The completion's error, then quiescence.
func (self *noAckAdvanceFixture) waitSend(ack <-chan error) error {
	self.t.Helper()
	var err error
	select {
	case err = <-ack:
	case <-time.After(20 * time.Second):
		self.t.Fatal("the transfer never completed")
	}
	synctest.Wait()
	return err
}

// Lookup follows the production lane identity, then the sender's sequence id.
func (self *noAckAdvanceFixture) sequences() (*SendSequence, *ReceiveSequence) {
	self.t.Helper()
	sender := self.sender.sendBuffer.lookupSendSequence(sendSequenceId{Destination: self.receiver.ClientId()}, nil)
	if sender == nil {
		self.t.Fatal("the transfer has no sender sequence")
	}
	self.receiver.receiveBuffer.mutex.Lock()
	defer self.receiver.receiveBuffer.mutex.Unlock()
	for id, receiver := range self.receiver.receiveBuffer.receiveSequences {
		if id.SequenceId == sender.sequenceId {
			return sender, receiver
		}
	}
	return sender, nil
}
