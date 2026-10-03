// Signed contracts must recover when only the receive sequence retires during
// an idle interval. The sender's unchanged sequence resumes above zero.
package connect

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// Takes request frame ownership and lends ordinary response bytes only for the
// callback. Signed grants bind generated identities to the exact test peer.
type idleContractTestOob struct {
	sourceId Id
	peer     func() *Client
}

// This in-memory authority follows the production control ownership contract.
func (self *idleContractTestOob) SendControl(frames []*protocol.Frame, callback func([]*protocol.Frame, error)) {
	defer func() {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
	}()
	var resultFrames []*protocol.Frame
	for _, frame := range frames {
		message, err := FromFrame(frame)
		if err != nil {
			continue
		}
		create, ok := message.(*protocol.CreateContract)
		if !ok {
			continue
		}
		peer := self.peer()
		if RequireIdFromBytes(create.DestinationId) != peer.ClientId() {
			panic("synthetic grant requested for another peer")
		}
		secret, ok := peer.ContractManager().GetProvideSecretKey(protocol.ProvideMode_Network)
		if !ok {
			panic("synthetic receiver has no provide key")
		}
		storedBytes, err := proto.Marshal(&protocol.StoredContract{
			ContractId: NewId().Bytes(), TransferByteCount: create.TransferByteCount,
			SourceId: self.sourceId.Bytes(), DestinationId: peer.ClientId().Bytes(),
			DestinationClientPublicKey: peer.ClientKeyManager().PublicKey(),
		})
		if err != nil {
			panic(err)
		}
		resultBytes, err := proto.Marshal(&protocol.CreateContractResult{Contract: &protocol.Contract{
			StoredContractBytes: storedBytes,
			StoredContractHmac:  SignStoredContract(peer.ContractManager().settings, secret, storedBytes),
			ProvideMode:         protocol.ProvideMode_Network,
		}})
		if err != nil {
			panic(err)
		}
		resultFrames = append(resultFrames, &protocol.Frame{MessageType: protocol.MessageType_TransferCreateContractResult, MessageBytes: resultBytes})
	}
	if callback != nil {
		callback(resultFrames, nil)
	}
}

// Use production buffers, signed contract verification, wire codecs and
// acknowledgement workers. Fake time forces the asymmetric idle boundary.
type idleContractRecoveryTestSettings struct {
	encryptionMode       EncryptionMode
	loseEncryptionState  bool
	lostContractRequests int
	crossAheadThreshold  bool
}

// Each fault is applied only after the original receive worker has joined and
// the same authenticated sender remains live with no outstanding payload.
func exerciseIdleContractRecovery(t *testing.T, testSettings idleContractRecoveryTestSettings) {
	t.Helper()
	mode := testSettings.encryptionMode
	lostRequests := testSettings.lostContractRequests
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		log := &receiveGapTestLogger{Logger: NewNoopLogger()}
		settings := func() *ClientSettings {
			value := DefaultClientSettingsWithBufferSize(64)
			value.Log = log
			value.EncryptionSettings.Mode = mode
			value.EncryptionSettings.EncryptionControlUseCompanion = false
			// Synctest starts before the production activation date. Require
			// real contracts at that synthetic time instead of exercising legacy.
			value.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Unix(1, 0)
			value.ContractManagerSettings.NetworkEventTimeChangeHmac = time.Unix(1, 0)
			value.ContractManagerSettings.LegacyCreateContract = false
			value.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
			return value
		}
		senderId, receiverId := NewId(), NewId()
		var sender, receiver *Client
		sender = NewClient(ctx, senderId, &idleContractTestOob{sourceId: senderId, peer: func() *Client { return receiver }}, settings())
		receiver = NewClient(ctx, receiverId, &idleContractTestOob{sourceId: receiverId, peer: func() *Client { return sender }}, settings())
		forward, reverse := make(chan []byte, 64), make(chan []byte, 64)
		reverseDelivered := make(chan []byte, 64)
		var droppedRequests atomic.Int32
		reverseDone := make(chan struct{})
		go func() {
			defer close(reverseDone)
			for {
				select {
				case <-ctx.Done():
					return
				case wire := <-reverse:
					var transfer protocol.TransferFrame
					if lostRequests != 0 && ProtoUnmarshal(wire, &transfer) == nil &&
						transfer.Ack != nil && len(transfer.Ack.MissingContractId) != 0 &&
						(lostRequests < 0 || droppedRequests.Load() < int32(lostRequests)) {
						droppedRequests.Add(1)
						MessagePoolReturn(wire)
						continue
					}
					select {
					case reverseDelivered <- wire:
					case <-ctx.Done():
						MessagePoolReturn(wire)
						return
					}
				}
			}
		}()
		defer func() {
			cancel()
			<-reverseDone
			owner, release := context.WithTimeout(context.Background(), 5*time.Second)
			defer release()
			for _, client := range []*Client{sender, receiver} {
				if err := client.CloseAndWait(owner); err != nil {
					t.Error(err)
				}
			}
			for _, route := range []chan []byte{forward, reverse, reverseDelivered} {
				for len(route) != 0 {
					MessagePoolReturn(<-route)
				}
			}
		}()
		sender.RouteManager().UpdateTransport(newDataGatewayTransport(), []Route{forward})
		receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{forward})
		receiver.RouteManager().UpdateTransport(newDataGatewayTransport(), []Route{reverse})
		sender.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{reverseDelivered})
		for _, client := range []*Client{sender, receiver} {
			client.ContractManager().SetProvideModes(map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
		}
		var delivered atomic.Int32
		encryptionEvents := make(chan *EncryptionEvent, 64)
		receiverEncryptionEvents := make(chan *EncryptionEvent, 64)
		if mode != EncryptionModeOff {
			defer sender.EncryptionSessionManager().AddEncryptionEventCallback(func(event *EncryptionEvent) {
				select {
				case encryptionEvents <- event:
				default:
				}
			})()
			defer receiver.EncryptionSessionManager().AddEncryptionEventCallback(func(event *EncryptionEvent) {
				select {
				case receiverEncryptionEvents <- event:
				default:
				}
			})()
		}
		receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				if frame.MessageType == protocol.MessageType_TestSimpleMessage {
					delivered.Add(1)
				}
			}
		})
		startSend := func(number int, contentOverride ...string) chan error {
			ack := make(chan error, 1)
			content := fmt.Sprintf("synthetic-%d", number)
			if len(contentOverride) != 0 {
				content = contentOverride[0]
			}
			frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: content})
			if !sender.SendWithTimeout(frame, receiverId, func(err error) { ack <- err }, 5*time.Second) {
				MessagePoolReturn(frame.MessageBytes)
				t.Fatal("signed transfer admission failed")
			}
			return ack
		}
		finishSend := func(number int, ack chan error) {
			select {
			case err := <-ack:
				if err != nil {
					t.Fatalf("signed transfer %d failed: %v; receiver requests=%d sender recoveries=%d errors=%v", number, err, receiver.missingContractRequestCount.Load(), sender.missingContractWriteCount.Load(), log.messages())
				}
			case <-time.After(70 * time.Second):
				t.Fatalf("signed transfer %d has no acknowledgement; receiver requests=%d sender recoveries=%d errors=%v", number, receiver.missingContractRequestCount.Load(), sender.missingContractWriteCount.Load(), log.messages())
			}
			synctest.Wait()
		}
		send := func(number int, contentOverride ...string) {
			finishSend(number, startSend(number, contentOverride...))
		}
		for number := range 13 {
			send(number)
		}
		prefix := 13
		if mode != EncryptionModeOff {
			for _, events := range []chan *EncryptionEvent{encryptionEvents, receiverEncryptionEvents} {
				select {
				case event := <-events:
					if event.Type != EncryptionEventSealed {
						t.Fatalf("synthetic peer failed to establish encryption: %+v", event)
					}
				case <-time.After(35 * time.Second):
					t.Fatal("synthetic peer encryption never established")
				}
			}
			// A delivered encrypted application frame follows the handshake
			// controls in sequence and proves their prefix acknowledgement.
			send(prefix)
			prefix++
		}
		var original *SendSequence
		func() {
			sender.sendBuffer.mutex.Lock()
			defer sender.sendBuffer.mutex.Unlock()
			for _, sequence := range sender.sendBuffer.sendSequences {
				if sequence.destination == receiverId && sequence.encryptionRole == sequenceTlsRoleClient && !sequence.encryptionCompanion && !sequence.companionContract && sequence.logicalLane == 0 {
					original = sequence
				}
			}
		}()
		if original == nil {
			t.Fatal("no retained sender sequence")
		}
		findReceiver := func() *ReceiveSequence {
			receiver.receiveBuffer.mutex.Lock()
			defer receiver.receiveBuffer.mutex.Unlock()
			for id, sequence := range receiver.receiveBuffer.receiveSequences {
				if id.SequenceId == original.sequenceId {
					return sequence
				}
			}
			return nil
		}
		originalReceiver := findReceiver()
		if originalReceiver == nil {
			t.Fatal("synthetic prefix has no receiver")
		}
		// Identity proofs repeat through the 30-second establishment window.
		// Advance past its final write plus the 120-second receive idle bound,
		// while remaining inside the unchanged 300-second send idle bound.
		time.Sleep(155 * time.Second)
		synctest.Wait()
		select {
		case <-originalReceiver.done:
		default:
			t.Fatal("original receive worker did not retire at its idle boundary")
		}
		retainedSender := func() bool {
			sender.sendBuffer.mutex.Lock()
			defer sender.sendBuffer.mutex.Unlock()
			return sender.sendBuffer.sendSequences[original.id()] == original
		}()
		if !retainedSender || findReceiver() != nil {
			t.Fatal("asymmetric idle boundary did not preserve only the sender")
		}
		t.Logf("idle boundary mode=%d original receiver retired; same sender retained", mode)
		if testSettings.loseEncryptionState {
			receiver.EncryptionSessionManager().Testing_DropSessions(senderId)
			synctest.Wait()
		}
		if lostRequests < 0 {
			ack := startSend(prefix)
			synctest.Wait()
			if droppedRequests.Load() != 1 || receiver.missingContractRequestCount.Load() != 1 {
				t.Fatal("missing-contract feedback was not observed and dropped at the wire")
			}
			// A later tail makes the receive gap observable independently of
			// the sender's first pending message and its own lifetime deadline.
			time.Sleep(55 * time.Second)
			tailAck := startSend(prefix + 1)
			synctest.Wait()
			reformed := findReceiver()
			if reformed == nil {
				t.Fatal("resumed traffic did not reform the receive generation")
			}
			for _, pending := range []chan error{ack, tailAck} {
				select {
				case err := <-pending:
					if err == nil {
						t.Fatal("missing proof was acknowledged as delivered")
					}
				case <-time.After(10 * time.Second):
					t.Fatal("missing proof suppressed the sender lifetime bound")
				}
			}
			synctest.Wait()
			if delivered.Load() != int32(prefix) || sender.missingContractWriteCount.Load() != 0 {
				t.Fatal("dropped feedback forged contract recovery or application delivery")
			}
			time.Sleep(55 * time.Second)
			synctest.Wait()
			select {
			case <-reformed.done:
			default:
				t.Fatal("unresolved receive gap lost its deadline")
			}
			entries := log.messages()
			if len(entries) != 1 || !strings.Contains(entries[0], "exit gap timeout expected=0") {
				t.Fatalf("unresolved receive gap lost strict evidence: %v", entries)
			}
			return
		}
		expectedDelivered := int32(prefix + 1)
		if testSettings.crossAheadThreshold {
			if sender.contractAheadAnnounceCount.Load() != 0 {
				t.Fatal("synthetic prefix did not leave a fresh contract-ahead threshold")
			}
			// Ordinary prefetch expires sooner than the retained send
			// sequence. Complete a fresh signed grant before resumption so
			// this case actually exercises an available successor.
			metadata := original.contractMetadata()
			sender.ContractManager().CreateContract(metadata.key, 1, 1024)
			synctest.Wait()
			// This payload leaves less than the existing lead floor after the
			// short prefix, while fitting inside the initial signed grant.
			contractSettings := sender.ContractManager().settings
			sendSettings := sender.sendBuffer.sendBufferSettings
			byteCount := ByteCount(float32(contractSettings.InitialContractTransferByteCount)*sendSettings.ContractFillFraction) - sendSettings.ContractAheadFloorByteCount/2
			if byteCount <= 0 || 2*1024*1024 < byteCount {
				t.Fatalf("synthetic threshold payload is not bounded: %d", byteCount)
			}
			firstAck := startSend(prefix, strings.Repeat("x", int(byteCount)))
			synctest.Wait()
			// The next application Pack is admitted while its recovering head
			// is outstanding. Normal successor prefetch must still proceed.
			secondAck := startSend(prefix + 1)
			finishSend(prefix, firstAck)
			finishSend(prefix+1, secondAck)
			if sender.contractAheadAnnounceCount.Load() != 1 || sender.contractAheadAcknowledgedCount.Load() != 1 {
				t.Fatal("head recovery disabled or duplicated ordinary successor prefetch")
			}
			expectedDelivered++
		} else {
			send(prefix)
		}
		if delivered.Load() != expectedDelivered || receiver.missingContractRequestCount.Load() != uint64(lostRequests+1) || sender.missingContractWriteCount.Load() != 1 || droppedRequests.Load() != int32(lostRequests) {
			t.Fatalf("recovery did not deliver exactly once: delivered=%d requests=%d recoveries=%d errors=%v", delivered.Load(), receiver.missingContractRequestCount.Load(), sender.missingContractWriteCount.Load(), log.messages())
		}
	})
}

// Contract authentication alone must not depend on TLS or a carrier family.
func TestSignedContractRecoversAfterReceiverIdle(t *testing.T) {
	exerciseIdleContractRecovery(t, idleContractRecoveryTestSettings{})
}

// The same state loss must recover through the authenticated encrypted path.
func TestEncryptedContractRecoversAfterReceiverIdle(t *testing.T) {
	exerciseIdleContractRecovery(t, idleContractRecoveryTestSettings{encryptionMode: EncryptionModeOpportunistic})
}

// Losing the peer cipher alongside the receive generation must not strand the
// signed contract recovery behind ciphertext the receiver can no longer open.
func TestEncryptedContractRecoversAfterReceiverStateLoss(t *testing.T) {
	exerciseIdleContractRecovery(t, idleContractRecoveryTestSettings{encryptionMode: EncryptionModeOpportunistic, loseEncryptionState: true})
}

// One lost receiver request must be regenerated by the retained compact head.
func TestSignedContractRecoversAfterLostMissingContractRequest(t *testing.T) {
	exerciseIdleContractRecovery(t, idleContractRecoveryTestSettings{lostContractRequests: 1})
}

// Sustained feedback loss is an explicit transport fault, not a permission to
// acknowledge unauthenticated data or to discard strict deadline evidence.
func TestSignedContractMissingFeedbackRetainsStrictDeadlines(t *testing.T) {
	exerciseIdleContractRecovery(t, idleContractRecoveryTestSettings{lostContractRequests: -1})
}

// Crossing the unchanged contract lead threshold on a resumed application
// message must not put a non-head announcement before its recovering head.
func TestSignedContractRecoversWhenIdleResumeCrossesAheadThreshold(t *testing.T) {
	exerciseIdleContractRecovery(t, idleContractRecoveryTestSettings{crossAheadThreshold: true})
}

// Cipher wrapping must preserve the same head-before-announcement boundary.
func TestEncryptedContractRecoversWhenIdleResumeCrossesAheadThreshold(t *testing.T) {
	exerciseIdleContractRecovery(t, idleContractRecoveryTestSettings{encryptionMode: EncryptionModeOpportunistic, crossAheadThreshold: true})
}
