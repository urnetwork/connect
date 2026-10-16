// A relay generation that dies at a contract rollover drops every frame it
// had accepted, in both directions, and tells neither endpoint. Transfer must
// recover that loss by itself, promptly, whatever the shape of the loss.
package connect

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// A data-only send gateway with an explicit carrier type, so the sender's
// route policy is the one a platform H1 carrier gets.
type rolloverRelayTestTransport struct {
	transportId Id
}

func (self *rolloverRelayTestTransport) TransportId() Id                  { return self.transportId }
func (self *rolloverRelayTestTransport) TransportType() TransportType     { return TransportTypeH1 }
func (self *rolloverRelayTestTransport) Priority() int                    { return 100 }
func (self *rolloverRelayTestTransport) Weight() float32                  { return 0 }
func (self *rolloverRelayTestTransport) MatchesReceive(TransferPath) bool { return false }
func (self *rolloverRelayTestTransport) Downgrade(TransferPath)           {}
func (self *rolloverRelayTestTransport) CanEvalRouteWeight(*RouteStats, map[Transport]*RouteStats) bool {
	return true
}
func (self *rolloverRelayTestTransport) RouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) float32 {
	return 1.0 / float32(1+len(remainingStats))
}
func (self *rolloverRelayTestTransport) MatchesSend(destination TransferPath) bool {
	return destination.DestinationId != ControlId
}

// Grants signed contracts as idleContractTestOob does, capped the way the
// platform caps a payer, so the first contract rolls over early.
type rolloverRelayTestOob struct {
	sourceId             Id
	peer                 func() *Client
	maxTransferByteCount ByteCount
}

func (self *rolloverRelayTestOob) SendControl(frames []*protocol.Frame, callback func([]*protocol.Frame, error)) {
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
		secret, ok := peer.ContractManager().GetProvideSecretKey(protocol.ProvideMode_Network)
		if !ok {
			panic("synthetic receiver has no provide key")
		}
		transferByteCount := min(create.TransferByteCount, uint64(self.maxTransferByteCount))
		storedBytes, err := proto.Marshal(&protocol.StoredContract{
			ContractId: NewId().Bytes(), TransferByteCount: transferByteCount,
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

// The contract a forward Pack carries a full frame for, if any. An ahead
// announcement is reported as not carrying the current contract.
func rolloverRelayPackContract(wire []byte) (Id, bool) {
	var transfer protocol.TransferFrame
	if ProtoUnmarshal(wire, &transfer) != nil {
		return Id{}, false
	}
	pack := transfer.Pack
	if pack == nil && transfer.Frame != nil && transfer.Frame.MessageType == protocol.MessageType_TransferPack {
		pack = &protocol.Pack{}
		if ProtoUnmarshal(transfer.Frame.MessageBytes, pack) != nil {
			return Id{}, false
		}
	}
	if pack == nil || pack.ContractFrame == nil || pack.ContractAhead {
		return Id{}, false
	}
	var contract protocol.Contract
	if ProtoUnmarshal(pack.ContractFrame.MessageBytes, &contract) != nil {
		return Id{}, false
	}
	var stored protocol.StoredContract
	if ProtoUnmarshal(contract.StoredContractBytes, &stored) != nil {
		return Id{}, false
	}
	contractId, err := IdFromBytes(stored.ContractId)
	return contractId, err == nil
}

type rolloverRelayLoss struct {
	name string
	// from the first Pack carrying a contract other than the opening one,
	// drop every frame for these durations: both directions, then one each
	bothDuration    time.Duration
	forwardDuration time.Duration
	reverseDuration time.Duration
	// cancel the receiver's receive sequences this long after the switch,
	// losing whatever they hold, as a receive gap timeout does
	cancelReceiverAfter time.Duration
	// a burst sender has its whole window in flight when the loss starts
	burst bool
	// sends are socket-owned TCP returns, retained past the ack timeout
	retained bool
}

type rolloverRelayResult struct {
	switchSeen   bool
	switchAt     time.Duration
	complete     bool
	completeAt   time.Duration
	delivered    int
	ackErrors    int
	droppedCount int
	errors       []string
}

func exerciseRolloverRelayLoss(t *testing.T, loss rolloverRelayLoss, messageCount int) rolloverRelayResult {
	var result rolloverRelayResult
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		log := &receiveGapTestLogger{Logger: NewNoopLogger()}
		settings := func() *ClientSettings {
			value := DefaultClientSettingsWithBufferSize(64)
			value.Log = log
			value.EncryptionSettings.Mode = EncryptionModeOff
			value.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Unix(1, 0)
			value.ContractManagerSettings.NetworkEventTimeChangeHmac = time.Unix(1, 0)
			value.ContractManagerSettings.LegacyCreateContract = false
			// a small opening contract and a capped successor, so the
			// rollover lands inside the flow
			value.ContractManagerSettings.InitialContractTransferByteCount = kib(256)
			value.ContractManagerSettings.InitialNetworkPeerContractTransferByteCount = kib(256)
			value.ContractManagerSettings.StandardContractTransferByteCount = mib(1)
			value.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
			return value
		}
		start := time.Now()
		senderId, receiverId := NewId(), NewId()
		var sender, receiver *Client
		sender = NewClient(ctx, senderId, &rolloverRelayTestOob{sourceId: senderId, peer: func() *Client { return receiver }, maxTransferByteCount: mib(1)}, settings())
		receiver = NewClient(ctx, receiverId, &rolloverRelayTestOob{sourceId: receiverId, peer: func() *Client { return sender }, maxTransferByteCount: mib(1)}, settings())

		forwardIn, forwardOut := make(chan []byte, 1024), make(chan []byte, 1024)
		reverseIn, reverseOut := make(chan []byte, 1024), make(chan []byte, 1024)

		var lossLock sync.Mutex
		var openingContractId Id
		openingSeen := false
		switchSeen := false
		var switchAt time.Duration
		droppedCount := 0
		var bothUntil, forwardUntil, reverseUntil time.Time

		// One relay per direction with a fixed one-way latency, in order.
		const latency = 5 * time.Millisecond
		type delayed struct {
			wire []byte
			at   time.Time
		}
		var workers sync.WaitGroup
		relay := func(in <-chan []byte, out chan<- []byte, forward bool) {
			line := make(chan delayed, 8192)
			workers.Add(2)
			go func() {
				defer workers.Done()
				for {
					select {
					case <-ctx.Done():
						return
					case entry := <-line:
						if wait := time.Until(entry.at); 0 < wait {
							select {
							case <-time.After(wait):
							case <-ctx.Done():
								MessagePoolReturn(entry.wire)
								return
							}
						}
						select {
						case out <- entry.wire:
						case <-ctx.Done():
							MessagePoolReturn(entry.wire)
							return
						}
					}
				}
			}()
			go func() {
				defer workers.Done()
				for {
					select {
					case <-ctx.Done():
						return
					case wire := <-in:
						now := time.Now()
						lossLock.Lock()
						if forward && !switchSeen {
							if contractId, ok := rolloverRelayPackContract(wire); ok {
								if !openingSeen {
									openingSeen = true
									openingContractId = contractId
								} else if contractId != openingContractId {
									// the first Pack under the successor:
									// the relay generation dies here
									switchSeen = true
									switchAt = now.Sub(start)
									bothUntil = now.Add(loss.bothDuration)
									forwardUntil = now.Add(loss.forwardDuration)
									reverseUntil = now.Add(loss.reverseDuration)
									if 0 < loss.cancelReceiverAfter {
										time.AfterFunc(loss.cancelReceiverAfter, func() {
											receiver.receiveBuffer.mutex.Lock()
											defer receiver.receiveBuffer.mutex.Unlock()
											for id, sequence := range receiver.receiveBuffer.receiveSequences {
												if id.Source.SourceId == senderId {
													sequence.Cancel()
												}
											}
										})
									}
								}
							}
						}
						drop := now.Before(bothUntil) ||
							forward && now.Before(forwardUntil) ||
							!forward && now.Before(reverseUntil)
						if drop {
							droppedCount += 1
						}
						lossLock.Unlock()
						if drop {
							MessagePoolReturn(wire)
							continue
						}
						select {
						case line <- delayed{wire: wire, at: now.Add(latency)}:
						case <-ctx.Done():
							MessagePoolReturn(wire)
							return
						}
					}
				}
			}()
		}
		relay(forwardIn, forwardOut, true)
		relay(reverseIn, reverseOut, false)
		defer func() {
			cancel()
			workers.Wait()
			owner, release := context.WithTimeout(context.Background(), 5*time.Second)
			defer release()
			for _, client := range []*Client{sender, receiver} {
				if err := client.CloseAndWait(owner); err != nil {
					t.Error(err)
				}
			}
			for _, route := range []chan []byte{forwardIn, forwardOut, reverseIn, reverseOut} {
				for len(route) != 0 {
					MessagePoolReturn(<-route)
				}
			}
		}()

		sender.RouteManager().UpdateTransport(&rolloverRelayTestTransport{transportId: NewId()}, []Route{forwardIn})
		receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{forwardOut})
		receiver.RouteManager().UpdateTransport(&rolloverRelayTestTransport{transportId: NewId()}, []Route{reverseIn})
		sender.RouteManager().UpdateTransport(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{reverseOut})
		for _, client := range []*Client{sender, receiver} {
			client.ContractManager().SetProvideModes(map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
		}

		var delivered atomic.Int32
		receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				if frame.MessageType == protocol.MessageType_TestSimpleMessage {
					delivered.Add(1)
				}
			}
		})

		var acked, ackErrors atomic.Int32
		allAcked := make(chan struct{})
		content := strings.Repeat("x", 2000)
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range messageCount {
				if !loss.burst {
					select {
					case <-time.After(200 * time.Microsecond):
					case <-ctx.Done():
						return
					}
				}
				var opts []any
				if loss.retained {
					opts = append(opts, sendPackRecoveryOption{retainAfterAckTimeout: true})
				}
				frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: content})
				if !sender.SendWithTimeout(frame, receiverId, func(err error) {
					if err != nil {
						ackErrors.Add(1)
					}
					if int(acked.Add(1)) == messageCount {
						close(allAcked)
					}
				}, -1, opts...) {
					MessagePoolReturn(frame.MessageBytes)
					return
				}
			}
		}()

		select {
		case <-allAcked:
			result.complete = true
			result.completeAt = time.Since(start)
		case <-time.After(120 * time.Second):
		}
		synctest.Wait()
		lossLock.Lock()
		result.switchSeen = switchSeen
		result.switchAt = switchAt
		result.droppedCount = droppedCount
		lossLock.Unlock()
		result.delivered = int(delivered.Load())
		result.ackErrors = int(ackErrors.Load())
		result.errors = log.messages()
	})
	return result
}

// The connect half of the M2 churn stall (server
// TestProxyContractChurnLoadFreePayerCap): at the first contract rollover the
// platform retired the sender's resident and silently dropped what it held.
// Whatever that loss contains, the sequence must deliver everything exactly
// once, without an acknowledgement failure, and within a few retransmit
// backoffs rather than an ack lifetime.
func TestContractRolloverRecoversAfterRelayGenerationLoss(t *testing.T) {
	// far below the 60 s ack lifetime, and above the cold 2 s retransmit
	// floor plus its backoff, which a burst sender with no samples pays
	const recoveryBound = 10 * time.Second
	for _, loss := range []rolloverRelayLoss{
		{name: "both directions lost at the switch", bothDuration: 500 * time.Millisecond},
		{name: "whole window lost at the switch", bothDuration: 200 * time.Millisecond, burst: true},
		{name: "acknowledgements lost at the switch", reverseDuration: 2 * time.Second},
		{name: "receiver loses its held items", forwardDuration: 20 * time.Millisecond, cancelReceiverAfter: 100 * time.Millisecond},
		{name: "retained returns lost at the switch", bothDuration: 500 * time.Millisecond, retained: true},
	} {
		messageCount := 1200
		if loss.burst {
			messageCount = 3000
		}
		result := exerciseRolloverRelayLoss(t, loss, messageCount)
		if !result.switchSeen {
			t.Errorf("%s: no Pack carried the successor contract, so the loss was never applied", loss.name)
			continue
		}
		if result.droppedCount == 0 {
			t.Errorf("%s: the relay dropped nothing, so this row proves nothing", loss.name)
			continue
		}
		if !result.complete {
			t.Errorf("%s: the sequence did not recover from the relay loss: %d of %d acknowledged, errors %v", loss.name, result.delivered, messageCount, result.errors)
			continue
		}
		if recovery := result.completeAt - result.switchAt; recoveryBound < recovery {
			t.Errorf("%s: recovery took %s after the switch, more than %s", loss.name, recovery, recoveryBound)
		}
		if result.ackErrors != 0 {
			t.Errorf("%s: %d sends failed their acknowledgement", loss.name, result.ackErrors)
		}
		if result.delivered != messageCount {
			t.Errorf("%s: delivered %d of %d messages", loss.name, result.delivered, messageCount)
		}
		if len(result.errors) != 0 {
			t.Errorf("%s: transfer logged errors: %v", loss.name, result.errors)
		}
		t.Logf("%s: switch at %s, %d frames dropped, recovered at %s", loss.name, result.switchAt, result.droppedCount, result.completeAt)
	}
}
