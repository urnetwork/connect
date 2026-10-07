// Signed deadlines cross the real sender, receiver, retry, and cleanup paths.
package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// Takes request buffers, lends responses for their callback, and records exact
// terminal reports. The first grant cohort shares one absolute deadline.
type expirationLifecycleAuthority struct {
	ctx                   context.Context
	sourceId              Id
	peer                  func() *Client
	initialDeadline       time.Time
	stateLock             sync.Mutex
	grantDeadlines        map[Id]time.Time
	reports               []*protocol.CloseContract
	firstRenewalRequested time.Time
	blockRenewals         atomic.Bool
	renewalEntered        chan struct{}
	renewalOnce           sync.Once
}

// Normal requests belong to the OOB lifecycle. Shutdown cleanup explicitly
// supplies its independent context through SendControlWithCtx.
func (self *expirationLifecycleAuthority) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	self.SendControlWithCtx(self.ctx, frames, callback)
}

// An explicit barrier models cancellation while the replacement is in flight.
func (self *expirationLifecycleAuthority) SendControlWithCtx(ctx context.Context, frames []*protocol.Frame, callback OobResultFunction) {
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
		if report, ok := message.(*protocol.CloseContract); ok {
			self.stateLock.Lock()
			self.reports = append(self.reports, proto.Clone(report).(*protocol.CloseContract))
			self.stateLock.Unlock()
			continue
		}
		request, ok := message.(*protocol.CreateContract)
		if !ok {
			continue
		}
		deadline := self.initialDeadline
		requestedAt := time.Now()
		if !requestedAt.Before(deadline) {
			self.stateLock.Lock()
			if self.firstRenewalRequested.IsZero() {
				self.firstRenewalRequested = requestedAt
			}
			self.stateLock.Unlock()
			if self.blockRenewals.Load() {
				self.renewalOnce.Do(func() { close(self.renewalEntered) })
				<-ctx.Done()
				callback(nil, ctx.Err())
				return
			}
			deadline = time.Now().Add(time.Hour)
		}
		peer := self.peer()
		if RequireIdFromBytes(request.DestinationId) != peer.ClientId() {
			panic("synthetic expiration grant requested for another peer")
		}
		secret, ok := peer.ContractManager().GetProvideSecretKey(protocol.ProvideMode_Network)
		if !ok {
			panic("synthetic expiration receiver has no provide key")
		}
		contractId := NewId()
		expirationTimeUnixMilli := deadline.UnixMilli()
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
		self.stateLock.Lock()
		self.grantDeadlines[contractId] = deadline
		self.stateLock.Unlock()
		responses = append(responses, &protocol.Frame{
			MessageType: protocol.MessageType_TransferCreateContractResult, MessageBytes: responseBytes,
		})
	}
	if callback != nil {
		callback(responses, nil)
	}
}

// Snapshotting never borrows a mutable protocol report from its external owner.
func (self *expirationLifecycleAuthority) terminalReports(contractId Id) []*protocol.CloseContract {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	var reports []*protocol.CloseContract
	for _, report := range self.reports {
		if !report.Checkpoint && RequireIdFromBytes(report.ContractId) == contractId {
			reports = append(reports, proto.Clone(report).(*protocol.CloseContract))
		}
	}
	return reports
}

// Holds a payload or returning ack before delivery. Releasing it orders test
// snapshots before the receiver or sender resumes mutating sequence state.
type expirationLifecycleWireBarrier struct {
	content string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

// Both directions use ordinary Client wire codecs, ownership, and ACK workers.
// Fake time advances only after explicit quiescence or a request barrier.
type expirationLifecycleFixture struct {
	t                *testing.T
	cancel           context.CancelFunc
	sender           *Client
	receiver         *Client
	authority        *expirationLifecycleAuthority
	reverseAuthority *expirationLifecycleAuthority
	deadline         time.Time
	dropAcksBefore   atomic.Int64
	droppedAcks      atomic.Int32
	lostAckWrites    atomic.Int32
	forwardBarrier   atomic.Pointer[expirationLifecycleWireBarrier]
	ackBarrier       atomic.Pointer[expirationLifecycleWireBarrier]
	stateLock        sync.Mutex
	deliveries       map[string]int
	routes           []Route
	pumps            sync.WaitGroup
	closed           bool
}

// Small grants force the existing ahead-announcement threshold without filling
// a contract; expiration, rather than byte exhaustion, must trigger renewal.
func newExpirationLifecycleFixture(t *testing.T, announceAhead bool, configure ...func(*ClientSettings)) *expirationLifecycleFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	f := &expirationLifecycleFixture{
		t: t, cancel: cancel, deadline: time.Now().Add(time.Second),
		deliveries: map[string]int{},
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
		value.SendBufferSettings.ContractAheadFloorByteCount = mib(1)
		if !announceAhead {
			value.SendBufferSettings.ContractAheadScale = 0
		}
		value.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		for _, change := range configure {
			change(value)
		}
		return value
	}
	senderId, receiverId := NewId(), NewId()
	f.authority = &expirationLifecycleAuthority{
		ctx: ctx, sourceId: senderId, peer: func() *Client { return f.receiver },
		initialDeadline: f.deadline, grantDeadlines: map[Id]time.Time{}, renewalEntered: make(chan struct{}),
	}
	f.reverseAuthority = &expirationLifecycleAuthority{
		ctx: ctx, sourceId: receiverId, peer: func() *Client { return f.sender },
		initialDeadline: f.deadline, grantDeadlines: map[Id]time.Time{}, renewalEntered: make(chan struct{}),
	}
	f.sender = NewClient(ctx, senderId, f.authority, settings())
	f.receiver = NewClient(ctx, receiverId, f.reverseAuthority, settings())
	for _, client := range []*Client{f.sender, f.receiver} {
		client.ContractManager().SetProvideModes(map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
	}
	forward, forwardDelivered := make(Route, 64), make(Route, 64)
	reverse, reverseDelivered := make(Route, 64), make(Route, 64)
	f.routes = []Route{forward, forwardDelivered, reverse, reverseDelivered}
	pump := func(input, output Route, ackRoute bool) {
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
				if ackRoute && transfer.Ack != nil && time.Now().UnixNano() < f.dropAcksBefore.Load() {
					f.droppedAcks.Add(1)
					MessagePoolReturn(wire)
					continue
				}
				if ackRoute && transfer.Ack != nil {
					if barrier := f.ackBarrier.Load(); barrier != nil {
						barrier.once.Do(func() { close(barrier.entered) })
						select {
						case <-barrier.release:
						case <-ctx.Done():
							MessagePoolReturn(wire)
							return
						}
					}
				}
				if !ackRoute && transfer.Pack != nil {
					for _, frame := range transfer.Pack.Frames {
						if frame.MessageType == protocol.MessageType_TestSimpleMessage {
							var message protocol.SimpleMessage
							if err := proto.Unmarshal(frame.MessageBytes, &message); err != nil {
								panic(err)
							}
							if barrier := f.forwardBarrier.Load(); barrier != nil && message.Content == barrier.content {
								barrier.once.Do(func() { close(barrier.entered) })
								select {
								case <-barrier.release:
								case <-ctx.Done():
									MessagePoolReturn(wire)
									return
								}
							}
							if message.Content == "lost-ack" {
								f.lostAckWrites.Add(1)
							}
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
	go pump(forward, forwardDelivered, false)
	go pump(reverse, reverseDelivered, true)
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
				f.stateLock.Unlock()
			}
		}
	}
	f.receiver.AddReceiveCallback(receive)
	f.sender.AddReceiveCallback(receive)
	return f
}

// The barrier and Client join finish all native and cleanup owners before the
// test reads reports or reconciles any pooled bytes left in physical routes.
func (self *expirationLifecycleFixture) close() {
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

// Successful admission transfers the frame; every failure retains it here.
func (self *expirationLifecycleFixture) startSend(content string, options ...any) <-chan error {
	self.t.Helper()
	return self.startClientSend(self.sender, self.receiver.ClientId(), content, options...)
}

// Either direction retains the same explicit takes-on-success ownership rule.
func (self *expirationLifecycleFixture) startClientSend(client *Client, destinationId Id, content string, options ...any) <-chan error {
	self.t.Helper()
	ack := make(chan error, 1)
	frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: content})
	if !client.SendWithTimeout(frame, destinationId, func(err error) { ack <- err }, time.Second, options...) {
		MessagePoolReturn(frame.MessageBytes)
		self.t.Fatal("signed expiration transfer was not admitted")
	}
	return ack
}

// Timeout is only a deadlock guard; the actual ordering is ACK completion.
func (self *expirationLifecycleFixture) finishSend(ack <-chan error) {
	self.t.Helper()
	select {
	case err := <-ack:
		if err != nil {
			self.t.Fatal("signed expiration transfer failed", err)
		}
	case <-time.After(20 * time.Second):
		self.t.Fatal("signed expiration transfer never acknowledged")
	}
	synctest.Wait()
}

// Lookup follows the production lane identity, then the sender's sequence id.
func (self *expirationLifecycleFixture) sequences() (*SendSequence, *ReceiveSequence) {
	self.t.Helper()
	sender := self.sender.sendBuffer.lookupSendSequence(sendSequenceId{Destination: self.receiver.ClientId()}, nil)
	if sender == nil {
		self.t.Fatal("signed transfer has no sender sequence")
	}
	self.receiver.receiveBuffer.mutex.Lock()
	defer self.receiver.receiveBuffer.mutex.Unlock()
	for id, receiver := range self.receiver.receiveBuffer.receiveSequences {
		if id.SequenceId == sender.sequenceId {
			return sender, receiver
		}
	}
	self.t.Fatal("signed transfer has no matching receiver sequence")
	return nil, nil
}

// The same sequence renews an unspent active contract and retires an already
// acknowledged, unused successor. Neither can survive by being prefetched.
func TestSignedContractExpirationRenewsActiveAndAheadWithoutLosingSequence(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newExpirationLifecycleFixture(t, true)
		defer f.close()
		f.finishSend(f.startSend("opening"))
		barrier := &expirationLifecycleWireBarrier{
			content: "pending-head", entered: make(chan struct{}), release: make(chan struct{}),
		}
		f.forwardBarrier.Store(barrier)
		pending := f.startSend(barrier.content)
		select {
		case <-barrier.entered:
		case <-time.After(20 * time.Second):
			t.Fatal("fixture never held an outstanding contract head")
		}
		announcement := f.startSend("announce-successor")
		synctest.Wait()
		close(barrier.release)
		f.finishSend(pending)
		f.finishSend(announcement)
		sender, receiver := f.sequences()
		active, ahead := sender.sendContract, sender.aheadSendContract
		if active == nil || ahead == nil || !ahead.acknowledgedAhead || len(sender.sendItems) != 0 {
			t.Fatalf("fixture did not establish an active contract and acknowledged successor: active=%t ahead=%t supported=%t attempted=%t pending=%d",
				active != nil, ahead != nil, sender.contractAheadSupported.Load(), sender.aheadSendContractAttempted, len(sender.sendItems))
		}
		activeUsed := active.ackedByteCount + active.unackedByteCount
		if activeUsed >= active.effectiveTransferByteCount {
			t.Fatal("fixture exhausted its byte allowance before expiry")
		}
		time.Sleep(time.Until(f.deadline))
		synctest.Wait()
		if !time.Now().Equal(f.deadline) {
			t.Fatal("fixture missed the exact signed deadline")
		}
		f.finishSend(f.startSend("renewed"))
		f.authority.stateLock.Lock()
		renewalRequested := f.authority.firstRenewalRequested
		f.authority.stateLock.Unlock()
		if !renewalRequested.Equal(f.deadline) {
			t.Fatalf("known expired grants delayed renewal by %s", renewalRequested.Sub(f.deadline))
		}
		currentSender, currentReceiver := f.sequences()
		if currentSender != sender || currentReceiver != receiver || sender.sendContract == active || sender.sendContract == ahead {
			t.Fatal("expiration failed to replace the contract on the live sequence")
		}
		if sender.openSendContracts[active.contractId] != nil || sender.openSendContracts[ahead.contractId] != nil {
			t.Fatal("expired active or unused announced contract remained open")
		}
		if active.ackedByteCount+active.unackedByteCount != activeUsed || ahead.ackedByteCount != 0 || ahead.unackedByteCount != 0 {
			t.Fatal("expiration charged renewed traffic or an unused opening reservation to an expired contract")
		}
		f.finishSend(f.startSend("renewed-datagram", NoAck()))
		f.stateLock.Lock()
		if f.deliveries["renewed"] != 1 || f.deliveries["renewed-datagram"] != 1 {
			t.Error("renewed reliable and NoAck traffic did not each deliver exactly once")
		}
		f.stateLock.Unlock()
		f.close()
		activeReports, aheadReports := f.authority.terminalReports(active.contractId), f.authority.terminalReports(ahead.contractId)
		if len(activeReports) != 1 || activeReports[0].AckedByteCount != uint64(activeUsed) || activeReports[0].UnackedByteCount != 0 {
			t.Fatal("expired active contract lost or duplicated its final accounting")
		}
		if len(aheadReports) != 1 || aheadReports[0].AckedByteCount != 0 || aheadReports[0].UnackedByteCount != 0 {
			t.Fatal("unused expired successor was not closed exactly once with zero usage")
		}
	})
}

// A lost delivery ACK may be recovered after expiry, but duplicate delivery
// spends no new allowance and the next payload must obtain a fresh contract.
func TestSignedContractExpirationLostAckRetryPreservesAccounting(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newExpirationLifecycleFixture(t, false)
		defer f.close()
		f.finishSend(f.startSend("opening"))
		sender, receiver := f.sequences()
		active, received := sender.sendContract, receiver.receiveContract
		barrier := &expirationLifecycleWireBarrier{
			entered: make(chan struct{}), release: make(chan struct{}),
		}
		f.ackBarrier.Store(barrier)
		f.dropAcksBefore.Store(f.deadline.UnixNano())
		ack := f.startSend("lost-ack")
		synctest.Wait()
		if f.lostAckWrites.Load() != 1 || len(sender.sendItems) != 1 {
			t.Fatal("fixture did not retain the first delivery for its lost acknowledgement")
		}
		senderUsed := active.ackedByteCount + active.unackedByteCount
		receiverUsed := received.ackedByteCount + received.unackedByteCount
		time.Sleep(time.Until(f.deadline))
		synctest.Wait()
		select {
		case <-ack:
			t.Fatal("lost acknowledgement completed before the post-expiry retry")
		default:
		}
		select {
		case <-barrier.entered:
		case <-time.After(20 * time.Second):
			t.Fatal("post-expiry retry never reached the acknowledgement barrier")
		}
		// Quiescence precedes the snapshots; this release also orders them before
		// the timer-driven retry ack updates the pending queue and byte counts.
		close(barrier.release)
		f.finishSend(ack)
		if f.droppedAcks.Load() == 0 || f.lostAckWrites.Load() < 2 || len(sender.sendItems) != 0 {
			t.Fatal("the duplicate did not recover the lost acknowledgement after expiry")
		}
		if active.ackedByteCount != senderUsed || active.unackedByteCount != 0 ||
			received.ackedByteCount+received.unackedByteCount != receiverUsed {
			t.Fatal("post-expiry duplicate changed contract byte accounting")
		}
		f.finishSend(f.startSend("fresh-after-retry"))
		currentSender, currentReceiver := f.sequences()
		if currentSender != sender || currentReceiver != receiver || sender.sendContract == active {
			t.Fatal("the lost-ACK retry extended permission for new payload")
		}
		f.stateLock.Lock()
		if f.deliveries["lost-ack"] != 1 || f.deliveries["fresh-after-retry"] != 1 {
			t.Error("retry duplicated delivery or renewal lost the following payload")
		}
		f.stateLock.Unlock()
		f.close()
		reports := f.authority.terminalReports(active.contractId)
		if len(reports) != 1 || reports[0].AckedByteCount != uint64(senderUsed) || reports[0].UnackedByteCount != 0 {
			t.Fatal("post-expiry retry lost or duplicated final close accounting")
		}
	})
}

// Cancellation crosses the exact replacement-request barrier, after the old
// contract expires and before any replacement grants authority for more data.
func TestSignedContractExpirationRenewalCancellationClosesOldAccounting(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newExpirationLifecycleFixture(t, false)
		defer f.close()
		f.finishSend(f.startSend("opening"))
		sender, _ := f.sequences()
		active := sender.sendContract
		used := active.ackedByteCount + active.unackedByteCount
		f.authority.blockRenewals.Store(true)
		time.Sleep(time.Until(f.deadline))
		ack := f.startSend("canceled-renewal")
		select {
		case <-f.authority.renewalEntered:
		case <-time.After(20 * time.Second):
			t.Fatal("expired contract did not reach replacement request")
		}
		f.close()
		select {
		case err := <-ack:
			if err == nil {
				t.Fatal("canceled replacement reported successful delivery")
			}
		default:
			t.Fatal("joined cancellation left a payload callback pending")
		}
		f.stateLock.Lock()
		if f.deliveries["canceled-renewal"] != 0 {
			t.Error("expired contract authorized data while replacement was blocked")
		}
		f.stateLock.Unlock()
		reports := f.authority.terminalReports(active.contractId)
		if len(reports) != 1 || reports[0].AckedByteCount != uint64(used) || reports[0].UnackedByteCount != 0 {
			t.Fatal("cancellation lost or duplicated the expired contract's final accounting")
		}
	})
}

// An idle receive worker can disappear while its sender retains an old grant.
// After the deadline the replacement receiver must see a newly signed head.
func TestSignedContractExpirationRenewsAfterReceiverIdleRetirement(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newExpirationLifecycleFixture(t, false, func(settings *ClientSettings) {
			settings.ReceiveBufferSettings.IdleTimeout = 200 * time.Millisecond
		})
		defer f.close()
		f.finishSend(f.startSend("opening"))
		sender, receiver := f.sequences()
		active := sender.sendContract
		used := active.ackedByteCount + active.unackedByteCount
		time.Sleep(time.Until(f.deadline))
		synctest.Wait()
		select {
		case <-receiver.done:
		default:
			t.Fatal("shorter receive idle lifetime did not retire the original receiver")
		}
		f.finishSend(f.startSend("after-receiver-idle"))
		currentSender, currentReceiver := f.sequences()
		if currentSender != sender || currentReceiver == receiver || sender.sendContract == active {
			t.Fatal("retained sender did not recover a fresh contract and receive worker")
		}
		if currentReceiver.receiveContract.contractId != sender.sendContract.contractId ||
			active.ackedByteCount+active.unackedByteCount != used {
			t.Fatal("reformed receive worker lost fresh authority or charged the expired grant")
		}
		f.stateLock.Lock()
		if f.deliveries["after-receiver-idle"] != 1 {
			t.Error("post-idle renewal did not deliver exactly once")
		}
		f.stateLock.Unlock()
	})
}

// Forward and companion senders own distinct reservations and may both outlive
// their deadline while idle. Renewal must preserve the return lane identity.
func TestSignedContractExpirationRenewsReverseCompanionIndependently(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newExpirationLifecycleFixture(t, false)
		defer f.close()
		f.finishSend(f.startSend("forward-opening"))
		f.finishSend(f.startClientSend(f.receiver, f.sender.ClientId(), "companion-opening", CompanionContract()))
		forward, _ := f.sequences()
		// The public companion option selects both contract and session
		// identity, including when payload encryption is disabled.
		returnKey := sendSequenceId{
			Destination: f.sender.ClientId(), CompanionContract: true, EncryptionCompanion: true,
		}
		reverse := f.receiver.sendBuffer.lookupSendSequence(returnKey, nil)
		if reverse == nil || reverse.sendContract == nil || !reverse.sendContractAcked {
			t.Fatal("reverse companion never established its independent signed contract")
		}
		forwardContract, reverseContract := forward.sendContract, reverse.sendContract
		forwardUsed := forwardContract.ackedByteCount + forwardContract.unackedByteCount
		reverseUsed := reverseContract.ackedByteCount + reverseContract.unackedByteCount
		time.Sleep(time.Until(f.deadline))
		synctest.Wait()
		f.finishSend(f.startClientSend(f.receiver, f.sender.ClientId(), "companion-renewed", CompanionContract()))
		if f.receiver.sendBuffer.lookupSendSequence(returnKey, nil) != reverse || reverse.sendContract == reverseContract {
			t.Fatal("expired reverse contract did not renew on its companion lane")
		}
		if forward.sendContract != forwardContract || forwardContract.ackedByteCount+forwardContract.unackedByteCount != forwardUsed {
			t.Fatal("reverse renewal mutated the original forward contract")
		}
		f.finishSend(f.startSend("forward-renewed"))
		currentForward, _ := f.sequences()
		if currentForward != forward || forward.sendContract == forwardContract ||
			reverseContract.ackedByteCount+reverseContract.unackedByteCount != reverseUsed {
			t.Fatal("forward renewal changed reverse accounting or lost its own sequence")
		}
		f.finishSend(f.startClientSend(f.receiver, f.sender.ClientId(), "companion-datagram", CompanionContract(), NoAck()))
		f.stateLock.Lock()
		if f.deliveries["companion-renewed"] != 1 || f.deliveries["forward-renewed"] != 1 || f.deliveries["companion-datagram"] != 1 {
			t.Error("renewed forward and companion traffic failed to deliver independently")
		}
		f.stateLock.Unlock()
		f.close()
		for _, observation := range []struct {
			reports []*protocol.CloseContract
			used    ByteCount
		}{
			{reports: f.authority.terminalReports(forwardContract.contractId), used: forwardUsed},
			{reports: f.reverseAuthority.terminalReports(reverseContract.contractId), used: reverseUsed},
		} {
			if len(observation.reports) != 1 || observation.reports[0].AckedByteCount != uint64(observation.used) || observation.reports[0].UnackedByteCount != 0 {
				t.Fatal("renewing one direction changed the other's final accounting")
			}
		}
	})
}
