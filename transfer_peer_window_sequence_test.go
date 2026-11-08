// Peer windows across real sequences: two clients joined by routes, with ordinary
// wire codecs, acks and contract requests, on synctest's fake clock. Nothing moves
// the clock but a test's sleep, so a contract's creation time says exactly which
// step created it.
package connect

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// What a pair's platform did, in order.
type peerWindowEvent struct {
	time time.Time
	// the client that asked or reported
	clientId   Id
	contractId Id
	// a contract created, else a report
	grant      bool
	acked      uint64
	unacked    uint64
	checkpoint bool
}

// Grants contracts with the platform's 60 minute life and keeps the pair's log.
type peerWindowAuthority struct {
	ctx      context.Context
	sourceId Id
	peer     func() *Client
	fixture  *peerWindowPairFixture
	// a control plane that answers no contract request
	refuse atomic.Bool
}

func (self *peerWindowAuthority) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	self.SendControlWithCtx(self.ctx, frames, callback)
}

func (self *peerWindowAuthority) SendControlWithCtx(ctx context.Context, frames []*protocol.Frame, callback OobResultFunction) {
	defer func() {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
	}()
	var responses []*protocol.Frame
	for _, frame := range frames {
		message, err := FromFrame(frame)
		if err != nil {
			panic(err)
		}
		request, ok := message.(*protocol.CreateContract)
		if !ok {
			continue
		}
		if self.refuse.Load() {
			// a contract error, which the backend-degraded gate does not count
			contractError := protocol.ContractError_NoPermission
			responseBytes, err := proto.Marshal(&protocol.CreateContractResult{Error: &contractError})
			if err != nil {
				panic(err)
			}
			responses = append(responses, &protocol.Frame{
				MessageType:  protocol.MessageType_TransferCreateContractResult,
				MessageBytes: responseBytes,
			})
			continue
		}
		peer := self.peer()
		secret, ok := peer.ContractManager().GetProvideSecretKey(protocol.ProvideMode_Network)
		if !ok {
			panic("the peer has no provide key")
		}
		contractId := newPeerWindowTestId()
		expirationTimeUnixMilli := time.Now().Add(60 * time.Minute).UnixMilli()
		storedBytes, err := proto.Marshal(&protocol.StoredContract{
			ContractId:                 contractId.Bytes(),
			TransferByteCount:          request.TransferByteCount,
			SourceId:                   self.sourceId.Bytes(),
			DestinationId:              peer.ClientId().Bytes(),
			DestinationClientPublicKey: peer.ClientKeyManager().PublicKey(),
			ExpirationTimeUnixMilli:    &expirationTimeUnixMilli,
		})
		if err != nil {
			panic(err)
		}
		responseBytes, err := proto.Marshal(&protocol.CreateContractResult{Contract: &protocol.Contract{
			StoredContractBytes: storedBytes,
			StoredContractHmac:  SignStoredContract(peer.ContractManager().settings, secret, storedBytes),
			ProvideMode:         protocol.ProvideMode_Network,
		}})
		if err != nil {
			panic(err)
		}
		self.fixture.record(peerWindowEvent{clientId: self.sourceId, contractId: contractId, grant: true})
		responses = append(responses, &protocol.Frame{
			MessageType:  protocol.MessageType_TransferCreateContractResult,
			MessageBytes: responseBytes,
		})
	}
	if callback != nil {
		callback(responses, nil)
	}
}

// Holds one message on the wire until released.
type peerWindowWireBarrier struct {
	content string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

// Two clients, the one that marks first and the one that marks on receipt, and
// what the caller above connect does on each side.
type peerWindowPairFixture struct {
	t         *testing.T
	cancel    context.CancelFunc
	initiator *Client
	responder *Client
	barrier   atomic.Pointer[peerWindowWireBarrier]
	routes    []Route
	pumps     sync.WaitGroup
	closed    bool

	stateLock  sync.Mutex
	events     []peerWindowEvent
	deliveries []string
	// the lane each client last heard from its peer on
	transferKeys map[Id]TransferKey

	authorities map[Id]*peerWindowAuthority
}

func newPeerWindowPairFixture(t *testing.T, configure ...func(*ClientSettings)) *peerWindowPairFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	f := &peerWindowPairFixture{
		t:            t,
		cancel:       cancel,
		transferKeys: map[Id]TransferKey{},
		authorities:  map[Id]*peerWindowAuthority{},
	}
	settings := func() *ClientSettings {
		value := DefaultClientSettingsWithBufferSize(64)
		value.Log = NewNoopLogger()
		value.ControlPingTimeout = 0
		value.EncryptionSettings.Mode = EncryptionModeOff
		value.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Unix(1, 0)
		value.ContractManagerSettings.NetworkEventTimeChangeHmac = time.Unix(1, 0)
		value.SendBufferSettings.AckTailProbeLimit = 0
		value.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		for _, change := range configure {
			change(value)
		}
		return value
	}
	initiatorId, responderId := NewId(), NewId()
	initiatorAuthority := &peerWindowAuthority{ctx: ctx, sourceId: initiatorId, peer: func() *Client { return f.responder }, fixture: f}
	responderAuthority := &peerWindowAuthority{ctx: ctx, sourceId: responderId, peer: func() *Client { return f.initiator }, fixture: f}
	f.authorities[initiatorId] = initiatorAuthority
	f.authorities[responderId] = responderAuthority
	f.initiator = NewClient(ctx, initiatorId, initiatorAuthority, settings())
	f.responder = NewClient(ctx, responderId, responderAuthority, settings())
	for _, client := range []*Client{f.initiator, f.responder} {
		client.ContractManager().SetProvideModes(map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
		clientId := client.ClientId()
		client.ContractManager().beforeOriginalCloseFrameForTest = func(report *protocol.CloseContract) {
			f.record(peerWindowEvent{
				clientId:   clientId,
				contractId: RequireIdFromBytes(report.ContractId),
				acked:      report.AckedByteCount,
				unacked:    report.UnackedByteCount,
				checkpoint: report.Checkpoint,
			})
		}
	}
	forward, forwardDelivered := make(Route, 64), make(Route, 64)
	reverse, reverseDelivered := make(Route, 64), make(Route, 64)
	f.routes = []Route{forward, forwardDelivered, reverse, reverseDelivered}
	pump := func(input Route, output Route) {
		defer f.pumps.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case wire := <-input:
				var transfer protocol.TransferFrame
				if err := proto.Unmarshal(wire, &transfer); err != nil {
					panic(err)
				}
				if barrier := f.barrier.Load(); barrier != nil && transfer.Pack != nil {
					for _, frame := range transfer.Pack.Frames {
						var message protocol.SimpleMessage
						if frame.MessageType != protocol.MessageType_TestSimpleMessage ||
							proto.Unmarshal(frame.MessageBytes, &message) != nil ||
							message.Content != barrier.content {
							continue
						}
						barrier.once.Do(func() { close(barrier.entered) })
						select {
						case <-barrier.release:
						case <-ctx.Done():
							MessagePoolReturn(wire)
							return
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
	go pump(forward, forwardDelivered)
	go pump(reverse, reverseDelivered)
	f.initiator.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(responderId)), []Route{forward})
	f.responder.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{forwardDelivered})
	f.responder.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(initiatorId)), []Route{reverse})
	f.initiator.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{reverseDelivered})
	for _, client := range []*Client{f.initiator, f.responder} {
		clientId := client.ClientId()
		client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, peer Peer) {
			for _, frame := range frames {
				if frame.MessageType != protocol.MessageType_TestSimpleMessage {
					continue
				}
				var message protocol.SimpleMessage
				if err := proto.Unmarshal(frame.MessageBytes, &message); err != nil {
					panic(err)
				}
				f.stateLock.Lock()
				f.deliveries = append(f.deliveries, message.Content)
				f.transferKeys[clientId] = peer.TransferKey
				f.stateLock.Unlock()
			}
		})
	}
	return f
}

func (self *peerWindowPairFixture) record(event peerWindowEvent) {
	event.time = time.Now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.events = append(self.events, event)
}

func (self *peerWindowPairFixture) close() {
	if self.closed {
		return
	}
	self.closed = true
	self.cancel()
	self.pumps.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, client := range []*Client{self.initiator, self.responder} {
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

// Takes the frame on success and reports when the send is acknowledged.
func (self *peerWindowPairFixture) startSend(from *Client, to *Client, content string, options ...any) <-chan error {
	self.t.Helper()
	ack := make(chan error, 1)
	frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: content})
	if !from.SendWithTimeout(frame, to.ClientId(), func(err error) { ack <- err }, time.Second, options...) {
		MessagePoolReturn(frame.MessageBytes)
		self.t.Fatal("the send was not admitted")
	}
	return ack
}

func (self *peerWindowPairFixture) finish(ack <-chan error) {
	self.t.Helper()
	select {
	case err := <-ack:
		if err != nil {
			self.t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		self.t.Fatal("the send was never acknowledged")
	}
	synctest.Wait()
}

func (self *peerWindowPairFixture) send(from *Client, to *Client, content string, options ...any) {
	self.t.Helper()
	self.finish(self.startSend(from, to, content, options...))
}

// The lane a client last heard its peer on.
func (self *peerWindowPairFixture) transferKey(client *Client) TransferKey {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.transferKeys[client.ClientId()]
}

// Answers a client's last frame on the lane it came in on, as any reply does.
func (self *peerWindowPairFixture) reply(from *Client, to *Client, content string) {
	self.t.Helper()
	self.send(from, to, content, self.transferKey(from))
}

// What the caller above connect does around a marking exchange. The initiator
// marks and retires before it sends. The other side marks on receipt, adopts what
// the frame rode, and retires before it answers. The initiator then adopts what
// the answer rode.
func (self *peerWindowPairFixture) markingExchange() {
	self.t.Helper()
	initiatorManager, responderManager := self.initiator.ContractManager(), self.responder.ContractManager()
	initiatorMarked := initiatorManager.SetPeerWindowAccounting(self.responder.ClientId(), true)
	if initiatorMarked {
		initiatorManager.RetirePeerContracts(self.responder.ClientId())
	}
	self.send(self.initiator, self.responder, "marking-request")
	if responderManager.SetPeerWindowAccounting(self.initiator.ClientId(), true) {
		if !responderManager.AdoptPeerWindowContract(self.initiator.ClientId(), self.transferKey(self.responder)) {
			self.t.Fatal("the marking frame's contract could not be adopted")
		}
		responderManager.RetirePeerContracts(self.initiator.ClientId())
	}
	self.reply(self.responder, self.initiator, "marking-reply")
	if initiatorMarked && !initiatorManager.AdoptPeerWindowContract(self.responder.ClientId(), self.transferKey(self.initiator)) {
		self.t.Fatal("the answer's contract could not be adopted")
	}
}

// The keepalive's exchange, as its caller notes it on both sides.
func (self *peerWindowPairFixture) keepalive() {
	self.t.Helper()
	self.send(self.initiator, self.responder, "keepalive")
	self.reply(self.responder, self.initiator, "keepalive-reply")
	self.initiator.ContractManager().NotePeerWindowActivity(self.responder.ClientId())
	self.responder.ContractManager().NotePeerWindowActivity(self.initiator.ClientId())
}

// A keepalive a minute, the last one a minute before `when`.
func (self *peerWindowPairFixture) keepaliveUntil(when time.Time) {
	self.t.Helper()
	for time.Minute < time.Until(when) {
		time.Sleep(time.Minute)
		self.keepalive()
	}
}

// The ids of contracts `clientId` was granted, in order of creation.
func (self *peerWindowPairFixture) grants(clientId Id) []peerWindowEvent {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	grants := []peerWindowEvent{}
	for _, event := range self.events {
		if event.grant && event.clientId == clientId {
			grants = append(grants, event)
		}
	}
	return grants
}

func (self *peerWindowPairFixture) grantCount() int {
	return len(self.grants(self.initiator.ClientId())) + len(self.grants(self.responder.ClientId()))
}

// The reports `clientId` made of one contract.
func (self *peerWindowPairFixture) reports(clientId Id, contractId Id) []peerWindowEvent {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	reports := []peerWindowEvent{}
	for _, event := range self.events {
		if !event.grant && event.clientId == clientId && event.contractId == contractId {
			reports = append(reports, event)
		}
	}
	return reports
}

// Every report the pair made, whoever made it.
func (self *peerWindowPairFixture) reportCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	reportCount := 0
	for _, event := range self.events {
		if !event.grant {
			reportCount += 1
		}
	}
	return reportCount
}

func (self *peerWindowPairFixture) requireReport(clientId Id, contractId Id, acked uint64) {
	self.t.Helper()
	reports := self.reports(clientId, contractId)
	if len(reports) != 1 || reports[0].acked != acked || reports[0].unacked != 0 || reports[0].checkpoint {
		self.t.Fatalf("contract %s reports = %+v, want one final of %d", contractId, reports, acked)
	}
}

// The window each client opened, read before it ends.
func (self *peerWindowPairFixture) sendWindow(from *Client, to *Client) PeerWindow {
	self.t.Helper()
	status := from.ContractManager().PeerWindow(to.ClientId())
	if status.Send == nil {
		self.t.Fatalf("no send window: %+v", status)
	}
	return *status.Send
}

// What the pair did, for a failure to show.
func (self *peerWindowPairFixture) log() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	start := time.Time{}
	for _, event := range self.events {
		if start.IsZero() {
			start = event.time
		}
		name := "responder"
		if event.clientId == self.initiator.ClientId() {
			name = "initiator"
		}
		kind := "grant"
		if !event.grant {
			kind = fmt.Sprintf("report acked=%d unacked=%d checkpoint=%t", event.acked, event.unacked, event.checkpoint)
		}
		self.t.Logf("%s %s %s %s", event.time.Sub(start), name, event.contractId, kind)
	}
}

// Sleeps until `when` and lets what it woke settle.
func sleepUntilPeerWindowEvent(when time.Time) {
	time.Sleep(time.Until(when))
	synctest.Wait()
}

// The root cause: a pair left idle inside a window. Both clients' sequences exit,
// which sends a checkpoint and a close today, and neither goes out until the
// window's end, where each client reports the floor on the first contract of each
// direction and zero on the others.
func TestPeerWindowSequenceIdleExitsSendNothingInsideAWindow(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPeerWindowPairFixture(t)
		defer f.close()
		initiatorId, responderId := f.initiator.ClientId(), f.responder.ClientId()
		f.markingExchange()
		window := f.sendWindow(f.initiator, f.responder)
		reportCount := f.reportCount()

		time.Sleep(f.initiator.settings.SendBufferSettings.IdleTimeout + time.Second)
		synctest.Wait()
		if f.initiator.sendBuffer.lookupSendSequence(sendSequenceId{Destination: responderId}, nil) != nil {
			t.Fatal("the send sequence did not idle out")
		}
		f.responder.receiveBuffer.mutex.Lock()
		liveReceiveSequenceCount := len(f.responder.receiveBuffer.receiveSequences)
		f.responder.receiveBuffer.mutex.Unlock()
		if liveReceiveSequenceCount != 0 {
			t.Fatalf("%d receive sequences did not idle out", liveReceiveSequenceCount)
		}
		if f.reportCount() != reportCount {
			f.log()
			t.Fatal("idle exits reported inside the window")
		}

		sleepUntilPeerWindowEvent(window.EndTime.Add(-time.Second))
		if f.reportCount() != reportCount {
			t.Fatal("a report went out before the window's end")
		}
		sleepUntilPeerWindowEvent(window.EndTime.Add(time.Second))
		up, down := f.grants(initiatorId)[0], f.grants(responderId)[0]
		for _, clientId := range []Id{initiatorId, responderId} {
			f.requireReport(clientId, up.contractId, uint64(peerWindowFloorByteCount))
			f.requireReport(clientId, down.contractId, uint64(peerWindowFloorByteCount))
		}
		// the successor each sender requested with its contract reports zero
		f.requireReport(initiatorId, f.grants(initiatorId)[1].contractId, 0)
		f.requireReport(responderId, f.grants(responderId)[1].contractId, 0)
	})
}

// A keepalive holds a pair's sequences for a whole window, so nothing opens a
// contract at an exchange in either direction: not 30 minutes in, and not 10 seconds
// after the window's end, where the next window's contracts were taken at the end
// itself. Each contract was created at the window's start or at its end.
func TestPeerWindowKeepaliveOpensNoContractAtAnExchange(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPeerWindowPairFixture(t)
		defer f.close()
		f.markingExchange()
		start := time.Now()
		window := f.sendWindow(f.initiator, f.responder)
		grantCount := f.grantCount()
		reportCount := f.reportCount()

		exchange := func(name string) {
			t.Helper()
			before := f.grantCount()
			f.send(f.initiator, f.responder, name+"-request")
			f.reply(f.responder, f.initiator, name+"-reply")
			if after := f.grantCount(); after != before {
				t.Fatalf("the exchange %s opened %d contracts", name, after-before)
			}
		}
		for minute := 1; minute <= 54; minute += 1 {
			time.Sleep(time.Minute)
			f.keepalive()
			if minute == 30 {
				exchange("half-way")
			}
		}
		if f.grantCount() != grantCount {
			t.Fatalf("%d contracts opened inside the window", f.grantCount()-grantCount)
		}
		if f.reportCount() != reportCount {
			f.log()
			t.Fatal("a report went out inside the window")
		}

		sleepUntilPeerWindowEvent(window.EndTime)
		if f.grantCount() != grantCount+4 {
			t.Fatalf("the window's end opened %d contracts, want each sender's next and its successor", f.grantCount()-grantCount)
		}
		time.Sleep(10 * time.Second)
		exchange("after-the-end")

		for _, clientId := range []Id{f.initiator.ClientId(), f.responder.ClientId()} {
			for _, grant := range f.grants(clientId) {
				if !grant.time.Equal(start) && !grant.time.Equal(window.EndTime) {
					t.Fatalf("a contract was created at %s, %s into the window", grant.time, grant.time.Sub(start))
				}
			}
		}
	})
}

// Marking retires exactly what the sender holds: its current contract and queued
// successor report at once with their exact counts, before any frame of the mode.
// The first frame after takes a new contract, which opens the window. A repeat
// mark retires nothing.
func TestPeerWindowMarkRetiresAndReportsExactly(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPeerWindowPairFixture(t, func(settings *ClientSettings) {
			// a successor is queued at take
			settings.SendBufferSettings.ContractPrefetchLeadTime = 0
		})
		defer f.close()
		initiatorId, responderId := f.initiator.ClientId(), f.responder.ClientId()
		f.send(f.initiator, f.responder, "before-the-mark")
		grants := f.grants(initiatorId)
		if len(grants) != 2 {
			t.Fatalf("%d contracts before the mark, want the opening one and its successor", len(grants))
		}
		held, queued := grants[0], grants[1]
		sequence := f.initiator.sendBuffer.lookupSendSequence(sendSequenceId{Destination: responderId}, nil)
		if sequence.sendContract == nil || sequence.sendContract.contractId != held.contractId {
			t.Fatal("the fixture's sequence does not hold the opening contract")
		}
		countedByteCount := uint64(sequence.sendContract.ackedByteCount + sequence.sendContract.unackedByteCount)

		manager := f.initiator.ContractManager()
		if !manager.SetPeerWindowAccounting(responderId, true) {
			t.Fatal("the first mark changed nothing")
		}
		manager.RetirePeerContracts(responderId)
		synctest.Wait()
		f.requireReport(initiatorId, held.contractId, countedByteCount)
		f.requireReport(initiatorId, queued.contractId, 0)

		f.send(f.initiator, f.responder, "first-of-the-mode")
		if sequence.sendContract == nil || sequence.sendContract.contractId == held.contractId {
			t.Fatal("the first frame after the mark rode a retired contract")
		}
		window := f.sendWindow(f.initiator, f.responder)
		opening := sequence.sendContract.contractId
		reportCount := f.reportCount()

		if manager.SetPeerWindowAccounting(responderId, true) {
			t.Fatal("a repeat mark changed the mode")
		}
		manager.RetirePeerContracts(responderId)
		synctest.Wait()
		if sequence.sendContract == nil || sequence.sendContract.contractId != opening {
			t.Fatal("a repeat mark moved the sequence off the window's contract")
		}
		if !manager.PeerWindow(responderId).Send.EndTime.Equal(window.EndTime) {
			t.Fatal("a repeat mark moved the window")
		}
		if f.reportCount() != reportCount {
			t.Fatal("a repeat mark reported")
		}
	})
}

// A Pack can reach a debit without passing the top of the sequence's loop, batched
// behind one taken earlier. The move requested before it still comes first.
func TestPeerWindowDebitAfterARequestRidesWhatTheMoveLeaves(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPeerWindowPairFixture(t)
		defer f.close()
		responderId := f.responder.ClientId()
		f.send(f.initiator, f.responder, "before-the-mark")
		manager := f.initiator.ContractManager()
		manager.SetPeerWindowAccounting(responderId, true)
		sequence := f.initiator.sendBuffer.lookupSendSequence(sendSequenceId{Destination: responderId}, nil)
		held := sequence.sendContract
		countedByteCount := held.ackedByteCount + held.unackedByteCount

		// the request lands with the sequence past its pass, so only the debit
		// can see it: its goroutine is parked, and nothing wakes it
		sequence.peerContractMove.Or(peerContractMoveRetire)
		if !sequence.updateContract(16) {
			t.Fatal("the debit found no contract")
		}
		if sequence.sendContract == held {
			t.Fatal("a debit after the request rode the retired contract")
		}
		if held.ackedByteCount+held.unackedByteCount != countedByteCount {
			t.Fatal("the retired contract was charged")
		}
		sequence.rollbackUnwrittenContractDebit(16)
	})
}

// A caller's no-acknowledgement write never passes through the sequence goroutine,
// so a mark has to fence it there: after the mark returns, no such write rides the
// retired contract.
func TestPeerWindowMarkFencesANoAckWrite(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPeerWindowPairFixture(t)
		defer f.close()
		responderId := f.responder.ClientId()
		f.send(f.initiator, f.responder, "before-the-mark")
		sequence := f.initiator.sendBuffer.lookupSendSequence(sendSequenceId{Destination: responderId}, nil)
		snapshot := sequence.noAckFastPath.Load()
		if snapshot == nil || snapshot.contract == nil {
			t.Fatal("the fixture published no fast path")
		}
		manager := f.initiator.ContractManager()
		manager.SetPeerWindowAccounting(responderId, true)
		// carried out on the sequence's goroutine later, and already refused here
		sequence.requestPeerContractMove(false)
		if snapshot.reserve(1) {
			t.Fatal("a caller reserved on a contract a mark retired")
		}
	})
}

// The move requests each next contract's successor at once, so neither a later
// frame nor the first one's rollover opens a contract, and a queued contract of
// the old window is not what the sequence moves onto.
func TestPeerWindowEndMovesOntoANewContractWithItsSuccessor(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPeerWindowPairFixture(t, func(settings *ClientSettings) {
			// the old window's successor stays queued to the end
			settings.ContractManagerSettings.ContractQueueExpireTimeout = 2 * time.Hour
		})
		defer f.close()
		initiatorId, responderId := f.initiator.ClientId(), f.responder.ClientId()
		f.markingExchange()
		window := f.sendWindow(f.initiator, f.responder)
		oldGrants := f.grants(initiatorId)
		if len(oldGrants) != 2 {
			t.Fatalf("%d contracts at the window's start", len(oldGrants))
		}
		f.keepaliveUntil(window.EndTime)
		sequence := f.initiator.sendBuffer.lookupSendSequence(sendSequenceId{Destination: responderId}, nil)
		sleepUntilPeerWindowEvent(window.EndTime)

		grants := f.grants(initiatorId)
		if len(grants) != 4 {
			t.Fatalf("%d contracts by the window's end, want 4", len(grants))
		}
		if !grants[2].time.Equal(window.EndTime) || !grants[3].time.Equal(window.EndTime) {
			t.Fatalf("the move's contracts were created at %s and %s", grants[2].time, grants[3].time)
		}
		if sequence.sendContract == nil || sequence.sendContract.contractId != grants[2].contractId {
			t.Fatal("the sequence did not take the first new contract")
		}
		manager := f.initiator.ContractManager()
		key := sequence.contractMetadata().key
		if !manager.hasQueuedContract(key) {
			t.Fatal("the move's successor is not queued")
		}
		if next := manager.PeerWindow(responderId).Send; next == nil ||
			!next.EndTime.Equal(window.EndTime.Add(55*time.Minute)) {
			t.Fatalf("the move's contract did not open the next window: %+v", next)
		}
		f.requireReport(initiatorId, oldGrants[0].contractId, uint64(peerWindowFloorByteCount))
		f.requireReport(initiatorId, oldGrants[1].contractId, 0)
		// the receiver heard of the new contract at the move, and it opens its window
		if next := f.responder.ContractManager().PeerWindow(initiatorId).Receive; next == nil ||
			!next.EndTime.Equal(window.EndTime.Add(55*time.Minute)) {
			t.Fatalf("the receiver's next window = %+v", next)
		}
	})
}

// Windows go by with no traffic to speak of, so the contract a move takes is sized
// as an opening one, not by how many windows the sequence has seen.
func TestPeerWindowMoveTakesAnOpeningSizedContract(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPeerWindowPairFixture(t)
		defer f.close()
		responderId := f.responder.ClientId()
		f.markingExchange()
		sequence := f.initiator.sendBuffer.lookupSendSequence(sendSequenceId{Destination: responderId}, nil)
		opening := sequence.sendContract.transferByteCount
		for range 6 {
			window := f.sendWindow(f.initiator, f.responder)
			f.keepaliveUntil(window.EndTime)
			sleepUntilPeerWindowEvent(window.EndTime)
			if sequence.sendContract.transferByteCount != opening {
				t.Fatalf("a move took %d bytes, the opening took %d", sequence.sendContract.transferByteCount, opening)
			}
		}
	})
}

// Reports follow the take: the platform sees the next contract first, so the pair
// is never without one, and what a sequence charged on its way off is counted.
func TestPeerWindowReportsFollowTheNextContract(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPeerWindowPairFixture(t)
		defer f.close()
		initiatorId := f.initiator.ClientId()
		f.markingExchange()
		window := f.sendWindow(f.initiator, f.responder)
		opening := f.grants(initiatorId)[0]
		f.keepaliveUntil(window.EndTime)
		sleepUntilPeerWindowEvent(window.EndTime)

		f.stateLock.Lock()
		defer f.stateLock.Unlock()
		grantIndex, reportIndex := -1, -1
		for index, event := range f.events {
			if event.clientId != initiatorId {
				continue
			}
			if event.grant && event.time.Equal(window.EndTime) && grantIndex < 0 {
				grantIndex = index
			}
			if !event.grant && event.contractId == opening.contractId && reportIndex < 0 {
				reportIndex = index
			}
		}
		if grantIndex < 0 || reportIndex < 0 || reportIndex < grantIndex {
			t.Fatalf("the next contract is at %d and the report at %d", grantIndex, reportIndex)
		}
	})
}

// A control plane that answers nothing cannot hold a window's reports past the
// move's timeout, and the sequence survives with no contract until one comes.
func TestPeerWindowReportsDoNotWaitForAStalledMove(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPeerWindowPairFixture(t)
		defer f.close()
		initiatorId, responderId := f.initiator.ClientId(), f.responder.ClientId()
		f.markingExchange()
		window := f.sendWindow(f.initiator, f.responder)
		opening := f.grants(initiatorId)[0]
		f.keepaliveUntil(window.EndTime)
		f.authorities[initiatorId].refuse.Store(true)

		moveTimeout := f.initiator.settings.ContractManagerSettings.PeerWindow.MoveTimeout
		sleepUntilPeerWindowEvent(window.EndTime.Add(moveTimeout - time.Second))
		if reports := f.reports(initiatorId, opening.contractId); len(reports) != 0 {
			t.Fatalf("the report did not wait for the move: %+v", reports)
		}
		sleepUntilPeerWindowEvent(window.EndTime.Add(moveTimeout))
		f.requireReport(initiatorId, opening.contractId, uint64(peerWindowFloorByteCount))

		// the contract request outlives its budget, and the next frame asks again
		f.authorities[initiatorId].refuse.Store(false)
		time.Sleep(f.initiator.settings.SendBufferSettings.CreateContractTimeout)
		synctest.Wait()
		f.send(f.initiator, f.responder, "after-the-outage")
		if status := f.initiator.ContractManager().PeerWindow(responderId); status.Send == nil {
			t.Fatalf("the frame after the outage opened no window: %+v", status)
		}
	})
}

// A window that ends with no activity in its last stretch clears the mode: no new
// contract at its end, and the next contract reports exactly, until the next mark.
func TestPeerWindowQuietEndClearsTheMode(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPeerWindowPairFixture(t)
		defer f.close()
		initiatorId, responderId := f.initiator.ClientId(), f.responder.ClientId()
		f.markingExchange()
		window := f.sendWindow(f.initiator, f.responder)
		opening := f.grants(initiatorId)[0]
		activityTimeout := f.initiator.settings.ContractManagerSettings.PeerWindow.ActivityTimeout
		f.keepaliveUntil(window.EndTime.Add(-activityTimeout))
		grantCount := f.grantCount()

		sleepUntilPeerWindowEvent(window.EndTime)
		if f.initiator.ContractManager().PeerWindow(responderId).Accounting ||
			f.responder.ContractManager().PeerWindow(initiatorId).Accounting {
			t.Fatal("a quiet last stretch left the mode on")
		}
		if f.grantCount() != grantCount {
			t.Fatalf("a window that cleared the mode took %d contracts", f.grantCount()-grantCount)
		}
		f.requireReport(initiatorId, opening.contractId, uint64(peerWindowFloorByteCount))
		f.requireReport(responderId, opening.contractId, uint64(peerWindowFloorByteCount))

		// out of the mode the next contract reports its exact count
		f.send(f.initiator, f.responder, "after-the-clear")
		sequence := f.initiator.sendBuffer.lookupSendSequence(sendSequenceId{Destination: responderId}, nil)
		exact := sequence.sendContract
		if exact == nil || exact.contractId == opening.contractId {
			t.Fatal("the sequence stayed on a reported contract")
		}
		countedByteCount := uint64(exact.ackedByteCount + exact.unackedByteCount)
		time.Sleep(f.initiator.settings.SendBufferSettings.IdleTimeout + time.Second)
		synctest.Wait()
		f.requireReport(initiatorId, exact.contractId, countedByteCount)
		if f.initiator.ContractManager().PeerWindow(responderId).Send != nil {
			t.Fatal("a contract opened a window with the mode off")
		}

		// and the next mark opens a new window
		f.markingExchange()
		next := f.sendWindow(f.initiator, f.responder)
		if !next.EndTime.After(window.EndTime) {
			t.Fatalf("the next mark's window = %+v", next)
		}
	})
}

// One activity in the last stretch holds the mode through the window's end.
func TestPeerWindowActivityInTheLastStretchHoldsTheMode(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPeerWindowPairFixture(t)
		defer f.close()
		f.markingExchange()
		window := f.sendWindow(f.initiator, f.responder)
		activityTimeout := f.initiator.settings.ContractManagerSettings.PeerWindow.ActivityTimeout
		f.keepaliveUntil(window.EndTime.Add(-activityTimeout + time.Minute))
		sleepUntilPeerWindowEvent(window.EndTime)
		if !f.initiator.ContractManager().PeerWindow(f.responder.ClientId()).Accounting {
			t.Fatal("activity at the start of the last stretch did not hold the mode")
		}
	})
}

// The move keeps a sequence's order and acks. A frame held on the wire across the
// window's end is delivered once, before what the sequence sent after the move,
// and the sequence, its contract's one report and the ack all survive it.
func TestPeerWindowMoveKeepsOrderAndAcks(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPeerWindowPairFixture(t)
		defer f.close()
		initiatorId, responderId := f.initiator.ClientId(), f.responder.ClientId()
		f.markingExchange()
		window := f.sendWindow(f.initiator, f.responder)
		opening := f.grants(initiatorId)[0]
		f.keepaliveUntil(window.EndTime)
		sequence := f.initiator.sendBuffer.lookupSendSequence(sendSequenceId{Destination: responderId}, nil)

		barrier := &peerWindowWireBarrier{content: "across-the-end", entered: make(chan struct{}), release: make(chan struct{})}
		f.barrier.Store(barrier)
		time.Sleep(59 * time.Second)
		across := f.startSend(f.initiator, f.responder, barrier.content)
		<-barrier.entered
		sleepUntilPeerWindowEvent(window.EndTime.Add(time.Second))
		if sequence.sendContract == nil || sequence.sendContract.contractId == opening.contractId {
			t.Fatal("the sequence did not move with a frame outstanding")
		}
		after := f.startSend(f.initiator, f.responder, "after-the-move")
		synctest.Wait()
		close(barrier.release)
		f.finish(across)
		f.finish(after)

		if current := f.initiator.sendBuffer.lookupSendSequence(sendSequenceId{Destination: responderId}, nil); current != sequence {
			t.Fatal("the move replaced the sequence")
		}
		if open := sequence.openSendContracts[opening.contractId]; open != nil {
			t.Fatal("the old contract stayed open after its last ack")
		}
		f.stateLock.Lock()
		deliveries := []string{}
		for _, content := range f.deliveries {
			if content == "across-the-end" || content == "after-the-move" {
				deliveries = append(deliveries, content)
			}
		}
		f.stateLock.Unlock()
		if fmt.Sprint(deliveries) != "[across-the-end after-the-move]" {
			t.Fatalf("delivered %v", deliveries)
		}
		f.requireReport(initiatorId, opening.contractId, uint64(peerWindowFloorByteCount))
		f.requireReport(responderId, opening.contractId, uint64(peerWindowFloorByteCount))
	})
}

// Past the first contract's grant a successor rolls as it always does, inside the
// window, and both clients report the window's steps across the two contracts.
func TestPeerWindowOverflowRollsAndSpillsInsideTheWindow(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPeerWindowPairFixture(t)
		defer f.close()
		initiatorId, responderId := f.initiator.ClientId(), f.responder.ClientId()
		f.markingExchange()
		window := f.sendWindow(f.initiator, f.responder)
		opening := f.grants(initiatorId)[0]
		content := make([]byte, 100*1024)
		for index := range content {
			content[index] = 'x'
		}
		for range 14 {
			f.send(f.initiator, f.responder, string(content))
		}
		sequence := f.initiator.sendBuffer.lookupSendSequence(sendSequenceId{Destination: responderId}, nil)
		rolled := sequence.sendContract
		if rolled == nil || rolled.contractId == opening.contractId {
			t.Fatal("1.4 MiB did not roll past a 1 MiB contract")
		}
		if len(f.reports(initiatorId, opening.contractId)) != 0 {
			t.Fatal("the roll reported the exhausted contract inside the window")
		}
		f.keepaliveUntil(window.EndTime)
		sleepUntilPeerWindowEvent(window.EndTime)

		// 14 x 100 KiB and the little else the window carried, up to the next step
		expected := uint64(1408 * 1024)
		for _, clientId := range []Id{initiatorId, responderId} {
			f.requireReport(clientId, opening.contractId, uint64(mib(1)))
			f.requireReport(clientId, rolled.contractId, expected-uint64(mib(1)))
		}
	})
}

// Under Required encryption a pair's direction has a sequence for each session
// role, and replies ride the server role. A marking frame that starts a session
// follows a handshake, whose contract the other side took before it marked and
// retires exactly at its mark. That contract opened the sender's receive window,
// already marked, but the sender adopts what the answer rode, so the answer's
// contract is that window's first and the handshake's leaves it. Both clients name
// the answer's contract first, and both report the handshake's exactly, which
// carried nothing of the mode. The next window, which a move opened, has one first
// on both clients in each direction.
func TestPeerWindowMoveUnderRequiredEncryption(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPeerWindowPairFixture(t, func(settings *ClientSettings) {
			settings.EncryptionSettings.Mode = EncryptionModeRequired
		})
		defer f.close()
		initiatorId, responderId := f.initiator.ClientId(), f.responder.ClientId()
		f.markingExchange()
		window := f.sendWindow(f.initiator, f.responder)
		handshake, opening := f.grants(responderId)[0], f.grants(responderId)[1]
		handshakeReports := f.reports(responderId, handshake.contractId)
		if len(handshakeReports) != 1 || handshakeReports[0].checkpoint || handshakeReports[0].acked == 0 ||
			uint64(peerWindowFloorByteCount) <= handshakeReports[0].acked {
			t.Fatalf("the retired handshake contract reported %+v, want its exact count at the mark", handshakeReports)
		}
		if f.initiator.ContractManager().peerWindows.Load().owns(handshake.contractId) {
			t.Fatal("the handshake's contract stayed in the window the answer's contract opens")
		}
		f.keepaliveUntil(window.EndTime)
		sleepUntilPeerWindowEvent(window.EndTime.Add(time.Second))
		f.requireReport(initiatorId, opening.contractId, uint64(peerWindowFloorByteCount))
		f.requireReport(responderId, opening.contractId, uint64(peerWindowFloorByteCount))

		next := f.sendWindow(f.initiator, f.responder)
		grantCount := f.grantCount()
		f.send(f.initiator, f.responder, "after-the-move")
		f.reply(f.responder, f.initiator, "after-the-move-reply")
		if f.grantCount() != grantCount {
			t.Fatalf("an exchange after the move opened %d contracts", f.grantCount()-grantCount)
		}
		f.keepaliveUntil(next.EndTime)
		f.stateLock.Lock()
		eventCount := len(f.events)
		f.stateLock.Unlock()
		sleepUntilPeerWindowEvent(next.EndTime.Add(time.Second))
		firstContracts := func(clientId Id) map[Id]bool {
			f.stateLock.Lock()
			defer f.stateLock.Unlock()
			contracts := map[Id]bool{}
			for _, event := range f.events[eventCount:] {
				if !event.grant && event.clientId == clientId && event.acked == uint64(peerWindowFloorByteCount) {
					contracts[event.contractId] = true
				}
			}
			return contracts
		}
		initiatorFirsts, responderFirsts := firstContracts(initiatorId), firstContracts(responderId)
		if len(initiatorFirsts) != 2 || len(responderFirsts) != 2 {
			f.log()
			t.Fatalf("firsts of the two directions: initiator %d, responder %d", len(initiatorFirsts), len(responderFirsts))
		}
		for contractId := range initiatorFirsts {
			if !responderFirsts[contractId] {
				f.log()
				t.Fatalf("the clients name different firsts: %v and %v", initiatorFirsts, responderFirsts)
			}
		}

		// the initiator's sequence lets the handshake's contract go by a trim or, once
		// the pair is quiet, its exit, and reports what it counted, as the responder did
		time.Sleep(f.initiator.settings.ReceiveBufferSettings.IdleTimeout + time.Second)
		synctest.Wait()
		initiatorHandshakeReports := f.reports(initiatorId, handshake.contractId)
		if len(initiatorHandshakeReports) != 1 || initiatorHandshakeReports[0].checkpoint ||
			initiatorHandshakeReports[0].acked+initiatorHandshakeReports[0].unacked !=
				handshakeReports[0].acked+handshakeReports[0].unacked {
			f.log()
			t.Fatalf("the initiator reported the handshake's contract %+v, want its exact count %+v", initiatorHandshakeReports, handshakeReports[0])
		}
	})
}

// Adoption finds the one sequence the frame rode, so a lane the peer never used
// adopts nothing and opens no window.
func TestPeerWindowAdoptionFollowsTheFramesLane(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPeerWindowPairFixture(t)
		defer f.close()
		initiatorId := f.initiator.ClientId()
		f.send(f.initiator, f.responder, "marking-request")
		manager := f.responder.ContractManager()
		manager.SetPeerWindowAccounting(initiatorId, true)
		if manager.AdoptPeerWindowContract(initiatorId, TransferKey{ForceStream: true}) {
			t.Fatal("a lane the peer never used adopted a contract")
		}
		if status := manager.PeerWindow(initiatorId); status.Receive != nil {
			t.Fatalf("a failed adoption opened a window: %+v", status.Receive)
		}
		if !manager.AdoptPeerWindowContract(initiatorId, f.transferKey(f.responder)) {
			t.Fatal("the frame's own lane adopted nothing")
		}
		status := manager.PeerWindow(initiatorId)
		if status.Receive == nil {
			t.Fatal("the adoption opened no window")
		}
		opening := f.grants(initiatorId)[0]
		if !status.Receive.EndTime.Equal(opening.time.Add(55 * time.Minute)) {
			t.Fatalf("the adopted window ends at %s", status.Receive.EndTime)
		}
	})
}

// Each sequence to the peer moves onto a new contract of its own, and one window
// spans them: one floor a direction, on the earliest contract.
func TestPeerWindowEverySequenceToThePeerMoves(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPeerWindowPairFixture(t)
		defer f.close()
		initiatorId, responderId := f.initiator.ClientId(), f.responder.ClientId()
		f.markingExchange()
		f.send(f.initiator, f.responder, "second-lane", CompanionContract())
		window := f.sendWindow(f.initiator, f.responder)
		lanes := []*SendSequence{
			f.initiator.sendBuffer.lookupSendSequence(sendSequenceId{Destination: responderId}, nil),
			f.initiator.sendBuffer.lookupSendSequence(sendSequenceId{Destination: responderId, CompanionContract: true, EncryptionCompanion: true}, nil),
		}
		for _, lane := range lanes {
			if lane == nil {
				t.Fatal("the fixture has one sequence, not two")
			}
		}
		contracts := []Id{lanes[0].sendContract.contractId, lanes[1].sendContract.contractId}
		if contracts[0] == contracts[1] {
			t.Fatal("the two lanes share a contract")
		}
		for time.Minute < time.Until(window.EndTime) {
			time.Sleep(time.Minute)
			f.keepalive()
			f.send(f.initiator, f.responder, "keep-the-second-lane", CompanionContract())
		}

		sleepUntilPeerWindowEvent(window.EndTime)
		for index, lane := range lanes {
			if lane.sendContract == nil || lane.sendContract.contractId == contracts[index] {
				t.Fatalf("lane %d did not move", index)
			}
		}
		if lanes[0].sendContract.contractId == lanes[1].sendContract.contractId {
			t.Fatal("the two lanes moved onto one contract")
		}
		f.requireReport(initiatorId, contracts[0], uint64(peerWindowFloorByteCount))
		f.requireReport(initiatorId, contracts[1], 0)
		f.requireReport(responderId, contracts[0], uint64(peerWindowFloorByteCount))
		f.requireReport(responderId, contracts[1], 0)
	})
}

// A pair nobody marked moves nothing at any window's time: each contract reports
// its exact count as it always has, checkpoint included.
func TestPeerWindowUnmarkedPairIsUnchanged(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPeerWindowPairFixture(t)
		defer f.close()
		initiatorId, responderId := f.initiator.ClientId(), f.responder.ClientId()
		f.send(f.initiator, f.responder, "unmarked")
		sequence := f.initiator.sendBuffer.lookupSendSequence(sendSequenceId{Destination: responderId}, nil)
		contract := sequence.sendContract
		countedByteCount := uint64(contract.ackedByteCount + contract.unackedByteCount)

		// the receiver idles out first and checkpoints its current contract
		time.Sleep(f.responder.settings.ReceiveBufferSettings.IdleTimeout + time.Second)
		synctest.Wait()
		reports := f.reports(responderId, contract.contractId)
		if len(reports) != 1 || !reports[0].checkpoint || reports[0].acked != countedByteCount {
			t.Fatalf("the receiver's idle exit reported %+v", reports)
		}
		time.Sleep(f.initiator.settings.SendBufferSettings.IdleTimeout)
		synctest.Wait()
		f.requireReport(initiatorId, contract.contractId, countedByteCount)
		if f.initiator.ContractManager().peerWindows.Load() != nil ||
			f.responder.ContractManager().peerWindows.Load() != nil {
			t.Fatal("an unmarked pair holds a ledger")
		}
	})
}

// A marked pair and a pair beside it on the same client: the one that was never
// marked keeps its exact reports and its contract through the marked pair's end.
func TestPeerWindowUnmarkedPeerBesideAMarkedOneIsUnchanged(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPeerWindowPairFixture(t)
		defer f.close()
		manager := f.initiator.ContractManager()
		otherPeerId := NewId()
		manager.SetPeerWindowAccounting(otherPeerId, true)
		manager.RetirePeerContracts(otherPeerId)
		f.send(f.initiator, f.responder, "beside")
		sequence := f.initiator.sendBuffer.lookupSendSequence(sendSequenceId{Destination: f.responder.ClientId()}, nil)
		contract := sequence.sendContract
		synctest.Wait()
		time.Sleep(55 * time.Minute)
		synctest.Wait()
		reports := f.reports(f.initiator.ClientId(), contract.contractId)
		if len(reports) != 1 || reports[0].acked != uint64(contract.ackedByteCount) {
			t.Fatalf("the unmarked contract reported %+v", reports)
		}
		if f.grantCount() != len(f.grants(f.initiator.ClientId())) || f.grantCount() != 1 {
			t.Fatalf("%d contracts on an unmarked pair", f.grantCount())
		}
	})
}

// The window's opening is named to a listener with the end both clients share,
// which is what a keepalive's grid runs from, and each later window is too.
func TestPeerWindowCallbackNamesEachWindowStart(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPeerWindowPairFixture(t)
		defer f.close()
		var stateLock sync.Mutex
		windows := []PeerWindow{}
		remove := f.initiator.ContractManager().AddPeerWindowCallback(func(window PeerWindow) {
			stateLock.Lock()
			defer stateLock.Unlock()
			windows = append(windows, window)
		})
		defer remove()
		opened := func(direction PeerWindowDirection) []PeerWindow {
			stateLock.Lock()
			defer stateLock.Unlock()
			matching := []PeerWindow{}
			for _, window := range windows {
				if window.Direction == direction {
					matching = append(matching, window)
				}
			}
			return matching
		}
		f.markingExchange()
		start := time.Now()
		synctest.Wait()
		first := opened(PeerWindowSend)
		if len(first) != 1 || !first[0].StartTime.Equal(start) || first[0].PeerId != f.responder.ClientId() {
			t.Fatalf("the listener heard %+v", first)
		}
		if !first[0].EndTime.Equal(start.Add(55 * time.Minute)) {
			t.Fatalf("the window's end was named as %s", first[0].EndTime)
		}
		if len(opened(PeerWindowReceive)) != 1 {
			t.Fatal("the listener did not hear the receive window")
		}
		f.keepaliveUntil(first[0].EndTime)
		sleepUntilPeerWindowEvent(first[0].EndTime)
		next := opened(PeerWindowSend)
		if len(next) != 2 || !next[1].StartTime.Equal(first[0].EndTime) {
			t.Fatalf("the listener heard %+v after the move", next)
		}
	})
}

// A listener that never returns holds only itself: the window's reports go out.
func TestPeerWindowBlockedListenerHoldsNoReport(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newPeerWindowPairFixture(t)
		defer f.close()
		release := make(chan struct{})
		f.initiator.ContractManager().AddPeerWindowCallback(func(PeerWindow) {
			<-release
		})
		f.markingExchange()
		window := f.sendWindow(f.initiator, f.responder)
		opening := f.grants(f.initiator.ClientId())[0]
		sleepUntilPeerWindowEvent(window.EndTime.Add(time.Second))
		f.requireReport(f.initiator.ClientId(), opening.contractId, uint64(peerWindowFloorByteCount))
		close(release)
	})
}
