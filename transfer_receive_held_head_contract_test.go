// A replacement receiver may hold an original non-head before the sender's
// promoted head arrives. Contract recovery must preserve that retained owner.
package connect

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// Reuses controlled wire rounds, but requires real signed contracts instead
// of the no-contract shortcut. Every captured or queued wire has one owner.
func newHeldHeadContractFixture(t *testing.T, budget *TransferMemoryBudget) (*windowRoundFixture, *receiveGapTestLogger) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	log := &receiveGapTestLogger{Logger: NewNoopLogger()}
	settings := func() *ClientSettings {
		value := DefaultClientSettingsWithBufferSize(64)
		value.Log = log
		value.EncryptionSettings.Mode = EncryptionModeOff
		value.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		value.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Unix(1, 0)
		value.ContractManagerSettings.NetworkEventTimeChangeHmac = time.Unix(1, 0)
		value.ContractManagerSettings.LegacyCreateContract = false
		value.SendBufferSettings.WindowSizing = WindowSizingConstant
		value.SendBufferSettings.ApplyWindowSizing()
		value.SendBufferSettings.SequenceBufferSize = 0
		value.ReceiveBufferSettings.WindowSizing = WindowSizingConstant
		value.ReceiveBufferSettings.ApplyWindowSizing()
		value.ReceiveBufferSettings.AckCompressTimeout = 0
		value.ReceiveBufferSettings.ReceiveHoldPolicy = ReceiveHoldRefuse
		value.ReceiveBufferSettings.ReceiveQueueBudget = budget
		value.ReceiveBufferSettings.ReceiveQueueRetainedByteAccounting = budget != nil
		return value
	}
	senderId, receiverId := NewId(), NewId()
	fixture := &windowRoundFixture{t: t, ctx: ctx,
		senderOut: make(Route, 128), senderIn: make(Route, 128),
		receiverOut: make(Route, 128), receiverIn: make(Route, 128)}
	fixture.sender = NewClient(ctx, senderId, &idleContractTestOob{
		sourceId: senderId, peer: func() *Client { return fixture.receiver },
	}, settings())
	fixture.receiver = NewClient(ctx, receiverId, &idleContractTestOob{
		sourceId: receiverId, peer: func() *Client { return fixture.sender },
	}, settings())
	fixture.sender.RouteManager().UpdateTransport(newDataGatewayTransport(), []Route{fixture.senderOut})
	fixture.sender.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{fixture.senderIn})
	fixture.receiver.RouteManager().UpdateTransport(newDataGatewayTransport(), []Route{fixture.receiverOut})
	fixture.receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{fixture.receiverIn})
	for _, client := range []*Client{fixture.sender, fixture.receiver} {
		client.ContractManager().SetProvideModes(map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
	}
	fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			if frame.MessageType == protocol.MessageType_TestSimpleMessage {
				fixture.deliveredCount++
			}
		}
	})
	t.Cleanup(func() {
		cancel()
		owner, release := context.WithTimeout(context.Background(), 5*time.Second)
		defer release()
		for _, client := range []*Client{fixture.sender, fixture.receiver} {
			if err := client.CloseAndWait(owner); err != nil {
				t.Errorf("close signed controlled client: %v", err)
			}
		}
		for _, frame := range fixture.heldFrames {
			if frame.bytes != nil {
				MessagePoolReturn(frame.bytes)
				frame.bytes = nil
			}
		}
		for _, route := range []Route{fixture.senderOut, fixture.senderIn, fixture.receiverOut, fixture.receiverIn} {
			for len(route) != 0 {
				MessagePoolReturn(<-route)
			}
		}
	})
	return fixture, log
}

// Opening controls, if present, finish through the real receiver before the
// requested application Pack is returned. Sequence numbers are never guessed.
func writeHeldHeadContractData(t *testing.T, fixture *windowRoundFixture) *windowRoundFrame {
	t.Helper()
	if admitted, err := fixture.send(100); !admitted {
		t.Fatalf("signed controlled send: admitted=%t error=%v", admitted, err)
	}
	for range cap(fixture.senderOut) {
		frame := fixture.take(fixture.senderOut)
		if frame.pack == nil {
			t.Fatal("signed sender published a non-Pack")
		}
		for _, payload := range frame.pack.Frames {
			if payload.MessageType == protocol.MessageType_TestSimpleMessage {
				return frame
			}
		}
		fixture.forward(frame, fixture.receiverIn)
		fixture.acknowledge()
	}
	t.Fatal("signed application Pack was hidden behind a full control route")
	return nil
}

// Explicit wire and joined-lifecycle barriers force the original non-head to
// reach a replacement receiver before its same-identity, sender-generated head.
func stageHeldHeadContract(t *testing.T, fixture *windowRoundFixture, holdFullProof ...bool) (*ReceiveSequence, *receiveItem, *windowRoundFrame) {
	t.Helper()
	fixture.forward(writeHeldHeadContractData(t, fixture), fixture.receiverIn)
	fixture.acknowledge()
	predecessor := writeHeldHeadContractData(t, fixture)
	tail := writeHeldHeadContractData(t, fixture)
	if tail.pack.Head || tail.pack.ContractFrame != nil || tail.pack.ContractId != nil {
		t.Fatal("controlled original was not a metadata-free non-head")
	}
	fixture.forward(predecessor, fixture.receiverIn)
	fixture.acknowledge()
	old := fixture.receiveSequence()
	old.Cancel()
	<-old.done
	synctest.Wait()
	sender := fixture.sequence()
	if len(sender.sendItems) != 1 || sender.sendItems[0].sequenceNumber != tail.pack.SequenceNumber {
		t.Fatal("predecessor acknowledgement did not leave the exact tail as retry owner")
	}
	wire, full, err := sender.setHead(sender.sendItems[0], true)
	if err != nil || !full {
		t.Fatalf("real sender did not restore its head proof: full=%t error=%v", full, err)
	}
	promoted := &windowRoundFrame{bytes: wire, pack: decodeCompactContractTestPack(t, wire)}
	fixture.heldFrames = append(fixture.heldFrames, promoted)
	fullOriginal := len(holdFullProof) != 0 && holdFullProof[0]
	if fullOriginal {
		pack := proto.Clone(tail.pack).(*protocol.Pack)
		pack.ContractFrame = proto.Clone(promoted.pack.ContractFrame).(*protocol.Frame)
		legacy := len(holdFullProof) > 1 && holdFullProof[1]
		rewriteHeldHeadContractWire(t, tail, pack, legacy)
	}
	fixture.forward(tail, fixture.receiverIn)
	sequence := fixture.receiveSequence()
	held := sequence.receiveQueue.GetBySequenceNumber(tail.pack.SequenceNumber)
	if sequence == old || sequence.nextSequenceNumber != 0 || sequence.receiveContract != nil ||
		held == nil || !held.committed || (held.contractFrame != nil) != fullOriginal || held.contractId != nil || held.head {
		t.Fatal("replacement receiver did not retain the original without contract authority")
	}
	// Keep the selective ACK at the wire boundary. Only final cumulative
	// delivery may retire the sender's last retry owner in these tests.
	for len(fixture.receiverOut) != 0 {
		fixture.take(fixture.receiverOut)
	}
	return sequence, held, promoted
}

// Retains the real transfer envelope while changing only a synthetic Pack's
// metadata. The replacement takes ownership before the old wire is returned.
func rewriteHeldHeadContractWire(t *testing.T, frame *windowRoundFrame, pack *protocol.Pack, legacy ...bool) {
	t.Helper()
	var transfer protocol.TransferFrame
	if err := ProtoUnmarshal(frame.bytes, &transfer); err != nil {
		t.Fatal(err)
	}
	transfer.Pack = pack
	if len(legacy) != 0 && legacy[0] {
		packBytes, err := proto.Marshal(pack)
		if err != nil {
			t.Fatal(err)
		}
		transfer.Pack = nil
		transfer.Frame = &protocol.Frame{MessageType: protocol.MessageType_TransferPack, MessageBytes: packBytes}
	}
	wire, err := ProtoMarshal(&transfer)
	if err != nil {
		t.Fatal(err)
	}
	MessagePoolReturn(frame.bytes)
	frame.bytes, frame.pack = wire, pack
}

// A second signed contract is an adversarial same-message proof, not a claim
// that SendSequence.setHead changes its immutable sendItem.contractId.
func newHeldHeadContractProof(t *testing.T, receiver *Client, original *protocol.Frame) *protocol.Frame {
	t.Helper()
	var contract protocol.Contract
	if err := ProtoUnmarshal(original.MessageBytes, &contract); err != nil {
		t.Fatal(err)
	}
	var stored protocol.StoredContract
	if err := ProtoUnmarshal(contract.StoredContractBytes, &stored); err != nil {
		t.Fatal(err)
	}
	stored.ContractId = NewId().Bytes()
	var err error
	contract.StoredContractBytes, err = proto.Marshal(&stored)
	if err != nil {
		t.Fatal(err)
	}
	manager := receiver.ContractManager()
	secret, ok := manager.GetProvideSecretKey(protocol.ProvideMode_Network)
	if !ok {
		t.Fatal("synthetic provider key disappeared")
	}
	contract.StoredContractHmac = SignStoredContract(manager.settings, secret, contract.StoredContractBytes)
	wire, err := proto.Marshal(&contract)
	if err != nil {
		t.Fatal(err)
	}
	return &protocol.Frame{MessageType: protocol.MessageType_TransferContract, MessageBytes: wire}
}

// A full shared budget still admits proof repair: it consumes the incoming
// proof synchronously, rather than retaining a second wire/decoded owner.
func TestReceiveHeldHeadContractProofPreservesOwner(t *testing.T) {
	// The pool statistics worker belongs to the process, outside virtual time.
	GetMessagePoolAggregateStats()
	for _, exact := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			assertMessagePoolOwnership(t)
			var budget *TransferMemoryBudget
			if exact {
				budget = NewTransferMemoryBudget(kib(64))
			}
			fixture, log := newHeldHeadContractFixture(t, budget)
			sequence, held, promoted := stageHeldHeadContract(t, fixture)
			originalTag, originalCharge := held.tag, held.queueByteCount
			originalPayload := append([]byte(nil), held.frames[0].MessageBytes...)
			var reserved ByteCount
			if budget != nil {
				budget.SetTotalByteCount(budget.UsedByteCount())
				reserved, _ = budget.Counts()
			}
			observedOwner := false
			fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
				observedOwner = true
				if held.tag != originalTag || held.queueByteCount != originalCharge ||
					!held.committed || len(frames) != 1 || !bytes.Equal(frames[0].MessageBytes, originalPayload) {
					t.Error("head proof replaced the committed payload, tag, or retained charge")
				}
				if budget != nil {
					after, _ := budget.Counts()
					if held.memoryBudget != budget || after != reserved {
						t.Error("proof repair surrendered or grew the exact held reservation")
					}
				}
			})
			fixture.forward(promoted, fixture.receiverIn)
			fixture.acknowledge()
			if !observedOwner || fixture.deliveredCount != 3 || fixture.ackedCount != 3 ||
				sequence.nextSequenceNumber != promoted.pack.SequenceNumber+1 || sequence.ctx.Err() != nil {
				t.Fatalf("exact=%t held proof lost delivery: observed=%t delivered=%d ACKs=%d next=%d errors=%v",
					exact, observedOwner, fixture.deliveredCount, fixture.ackedCount, sequence.nextSequenceNumber, log.messages())
			}
			if entries := log.messages(); len(entries) != 0 {
				t.Fatalf("valid held-head recovery logged errors: %v", entries)
			}
			if len(fixture.sequence().sendItems) != 0 {
				t.Fatal("cumulative delivery did not release the last sender retry owner")
			}
			if budget != nil {
				assertRetainedBudgetBalance(t, budget)
			}
		})
	}
}

// Direct-drive only the final receive transition, while its real worker is
// quiescent, so rejected proof state can be inspected before terminal cleanup.
// The Pack still uses the production owned decoder and exact pool return path.
func receiveHeldHeadContractControl(t *testing.T, sequence *ReceiveSequence, pack *protocol.Pack) (bool, error) {
	t.Helper()
	wire, err := ProtoMarshal(pack)
	if err != nil {
		t.Fatal(err)
	}
	owner, decoded := decodePackOwned(wire)
	MessagePoolReturn(wire)
	if !decoded {
		t.Fatal("controlled head did not decode through the owned Pack codec")
	}
	receivePack := &owner.receivePack
	*receivePack = ReceivePack{Pack: &owner.pack, decodedOwner: owner,
		MessageByteCount: MessageByteCount(owner.pack.Frames), Unwrapped: true,
		ReceiveCallback: sequence.client.receiveCallback}
	accepted, receiveErr := sequence.receive(receivePack)
	if !accepted {
		receivePack.messagePoolReturn()
	}
	return accepted, receiveErr
}

// Signed-but-expired/wrong-path proofs have no more authority than a bad
// signature. Identity collisions must not upgrade a different retained Pack.
func TestReceiveHeldHeadContractRejectsUntrustedPromotion(t *testing.T) {
	// The pool statistics worker belongs to the process, outside virtual time.
	GetMessagePoolAggregateStats()
	for _, kind := range []string{"signature", "expired", "source", "destination", "message", "sequence", "ahead"} {
		synctest.Test(t, func(t *testing.T) {
			assertMessagePoolOwnership(t)
			budget := NewTransferMemoryBudget(kib(64))
			fixture, _ := newHeldHeadContractFixture(t, budget)
			sequence, held, promoted := stageHeldHeadContract(t, fixture)
			pack := proto.Clone(promoted.pack).(*protocol.Pack)
			var contract protocol.Contract
			if err := ProtoUnmarshal(pack.ContractFrame.MessageBytes, &contract); err != nil {
				t.Fatal(err)
			}
			var stored protocol.StoredContract
			if err := ProtoUnmarshal(contract.StoredContractBytes, &stored); err != nil {
				t.Fatal(err)
			}
			wantError := false
			switch kind {
			case "signature":
				contract.StoredContractHmac[0] ^= 1
				wantError = true
			case "expired":
				deadline := time.Now().Add(-time.Millisecond).UnixMilli()
				stored.ExpirationTimeUnixMilli = &deadline
				wantError = true
			case "source":
				stored.SourceId = NewId().Bytes()
				wantError = true
			case "destination":
				stored.DestinationId = NewId().Bytes()
				wantError = true
			case "message":
				pack.MessageId = NewId().Bytes()
			case "sequence":
				pack.SequenceNumber++
			case "ahead":
				pack.ContractAhead = true
			}
			if kind == "expired" || kind == "source" || kind == "destination" {
				var err error
				contract.StoredContractBytes, err = proto.Marshal(&stored)
				if err != nil {
					t.Fatal(err)
				}
				manager := fixture.receiver.ContractManager()
				secret, ok := manager.GetProvideSecretKey(protocol.ProvideMode_Network)
				if !ok {
					t.Fatal("synthetic provider key disappeared")
				}
				contract.StoredContractHmac = SignStoredContract(manager.settings, secret, contract.StoredContractBytes)
			}
			var err error
			pack.ContractFrame.MessageBytes, err = proto.Marshal(&contract)
			if err != nil {
				t.Fatal(err)
			}
			budget.SetTotalByteCount(budget.UsedByteCount())
			reserved, released := budget.Counts()
			originalTag, originalCharge := held.tag, held.queueByteCount
			var ackUpdates atomic.Int32
			sequence.ackWindow.afterUpdateForTest = func() { ackUpdates.Add(1) }
			accepted, receiveErr := receiveHeldHeadContractControl(t, sequence, pack)
			if accepted || (receiveErr != nil) != wantError {
				t.Errorf("%s promotion result accepted=%t error=%v, want error=%t", kind, accepted, receiveErr, wantError)
			}
			if kind == "expired" && !errors.Is(receiveErr, errContractExpired) {
				t.Errorf("expired proof lost ordinary expiry classification: %v", receiveErr)
			}
			afterReserved, afterReleased := budget.Counts()
			if sequence.nextSequenceNumber != 0 || sequence.receiveContract != nil || len(sequence.openReceiveContracts) != 0 ||
				sequence.receiveQueue.GetByMessageId(held.messageId) != held || !held.committed || held.head ||
				held.contractFrame != nil || held.contractId != nil || held.tag != originalTag || held.queueByteCount != originalCharge ||
				held.memoryBudget != budget || reserved != afterReserved || released != afterReleased || ackUpdates.Load() != 0 || fixture.deliveredCount != 2 {
				t.Errorf("%s proof changed prefix, ACK, contract authority, or committed ownership before rejection", kind)
			}
		})
	}
}

// Missing compact authority is a proof request, never an acknowledgement of
// the held payload. The same sender-generated full head completes recovery.
func TestReceiveHeldHeadContractUnknownCompactKeepsCustody(t *testing.T) {
	// The pool statistics worker belongs to the process, outside virtual time.
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		budget := NewTransferMemoryBudget(kib(64))
		fixture, log := newHeldHeadContractFixture(t, budget)
		sequence, held, promoted := stageHeldHeadContract(t, fixture)
		sender := fixture.sequence()
		wire, full, err := sender.setHead(sender.sendItems[0], false)
		if err != nil {
			t.Fatal(err)
		}
		compact := &windowRoundFrame{bytes: wire, pack: decodeCompactContractTestPack(t, wire)}
		fixture.heldFrames = append(fixture.heldFrames, compact)
		if full || compact.pack.ContractId == nil || compact.pack.ContractFrame != nil {
			t.Fatal("sender did not use the capability negotiated by its earlier delivered proof")
		}
		budget.SetTotalByteCount(budget.UsedByteCount())
		reserved, released := budget.Counts()
		fixture.forward(compact, fixture.receiverIn)
		afterReserved, afterReleased := budget.Counts()
		if sequence.nextSequenceNumber != 0 || sequence.receiveContract != nil ||
			sequence.receiveQueue.GetByMessageId(held.messageId) != held || held.contractId != nil ||
			reserved != afterReserved || released != afterReleased || fixture.deliveredCount != 2 {
			t.Fatal("unknown compact head changed prefix or retained owner")
		}
		request := fixture.take(fixture.receiverOut)
		if request.ack == nil || !bytes.Equal(request.ack.MissingContractId, compact.pack.ContractId) ||
			!bytes.Equal(request.ack.MessageId, compact.pack.MessageId) || len(fixture.receiverOut) != 0 {
			t.Fatal("unknown compact head emitted acceptance rather than its exact proof request")
		}
		fixture.forward(promoted, fixture.receiverIn)
		fixture.acknowledge()
		if fixture.deliveredCount != 3 || fixture.ackedCount != 3 || len(log.messages()) != 0 {
			t.Fatalf("full proof failed after compact refusal: delivered=%d ACKs=%d errors=%v",
				fixture.deliveredCount, fixture.ackedCount, log.messages())
		}
		assertRetainedBudgetBalance(t, budget)
	})
}

// Proof repair cannot bypass an already-closed downstream admission ledger.
// Refusal leaves both the retained payload and uninstalled proof unchanged.
func TestReceiveHeldHeadContractRefusedAdmissionKeepsState(t *testing.T) {
	// The pool statistics worker belongs to the process, outside virtual time.
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		budget := NewTransferMemoryBudget(kib(64))
		fixture, _ := newHeldHeadContractFixture(t, budget)
		sequence, held, promoted := stageHeldHeadContract(t, fixture)
		sequence.deliveryQueue = newReceiveDeliveryQueue(sequence, 1, kib(64))
		sequence.deliveryQueue.closed = true
		budget.SetTotalByteCount(budget.UsedByteCount())
		reserved, released := budget.Counts()
		var ackUpdates atomic.Int32
		sequence.ackWindow.afterUpdateForTest = func() { ackUpdates.Add(1) }
		accepted, err := receiveHeldHeadContractControl(t, sequence, promoted.pack)
		afterReserved, afterReleased := budget.Counts()
		if accepted || err != nil || sequence.nextSequenceNumber != 0 || sequence.receiveContract != nil ||
			len(sequence.openReceiveContracts) != 0 || sequence.receiveQueue.GetByMessageId(held.messageId) != held ||
			held.contractId != nil || !held.committed || reserved != afterReserved || released != afterReleased || ackUpdates.Load() != 0 {
			t.Fatal("downstream refusal changed proof authority, prefix, ACK, or held reservation")
		}
	})
}

// The held wire may carry a complete old proof. A verified incoming proof
// must not be undone by replaying that old metadata during the ordered drain.
func TestReceiveHeldHeadContractOldProofDoesNotReplay(t *testing.T) {
	// The pool statistics worker belongs to the process, outside virtual time.
	GetMessagePoolAggregateStats()
	for _, legacy := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			assertMessagePoolOwnership(t)
			budget := NewTransferMemoryBudget(kib(64))
			fixture, log := newHeldHeadContractFixture(t, budget)
			sequence, held, promoted := stageHeldHeadContract(t, fixture, true, legacy)
			if (held.decodedOwner == nil) != legacy {
				t.Fatal("held full proof did not exercise its selected wire decoder ownership")
			}
			originalProof := held.contractFrame
			originalOwner := held.decodedOwner
			pack := proto.Clone(promoted.pack).(*protocol.Pack)
			pack.ContractFrame = newHeldHeadContractProof(t, fixture.receiver, promoted.pack.ContractFrame)
			rewriteHeldHeadContractWire(t, promoted, pack)
			budget.SetTotalByteCount(budget.UsedByteCount())
			reserved, _ := budget.Counts()
			observed := false
			fixture.receiver.AddReceiveCallback(func(_ TransferPath, _ []*protocol.Frame, _ Peer) {
				observed = true
				after, _ := budget.Counts()
				if held.contractFrame != nil || held.contractId == nil || sequence.receiveContract == nil ||
					*held.contractId != sequence.receiveContract.contractId || held.decodedOwner != originalOwner || after != reserved {
					t.Error("old proof replayed or lost its charged owner before payload return")
				}
				if legacy {
					if len(originalProof.MessageBytes) != 0 {
						t.Error("legacy direct proof owner was not returned before unlinking")
					}
				} else if originalOwner.pack.ContractFrame != originalProof || len(originalProof.MessageBytes) == 0 {
					t.Error("decoded owner lost its original proof before normal return")
				}
			})
			fixture.forward(promoted, fixture.receiverIn)
			fixture.acknowledge()
			var contract protocol.Contract
			if err := ProtoUnmarshal(pack.ContractFrame.MessageBytes, &contract); err != nil {
				t.Fatal(err)
			}
			var stored protocol.StoredContract
			if err := ProtoUnmarshal(contract.StoredContractBytes, &stored); err != nil {
				t.Fatal(err)
			}
			if !observed || sequence.receiveContract == nil ||
				sequence.receiveContract.contractId != RequireIdFromBytes(stored.ContractId) ||
				fixture.deliveredCount != 3 || fixture.ackedCount != 3 || len(log.messages()) != 0 {
				t.Fatal("old retained proof restored obsolete current authority during drain")
			}
			assertRetainedBudgetBalance(t, budget)
		})
	}
}

// Match the established direct compact-head rule: resolve its explicit id
// through the verified map, without replacing a different current contract.
func TestReceiveHeldHeadContractKnownCompactUsesExactAuthority(t *testing.T) {
	// The pool statistics worker belongs to the process, outside virtual time.
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		budget := NewTransferMemoryBudget(kib(64))
		fixture, log := newHeldHeadContractFixture(t, budget)
		sequence, _, promoted := stageHeldHeadContract(t, fixture, true)
		if err := sequence.registerContracts(&receiveItem{contractFrame: promoted.pack.ContractFrame}); err != nil {
			t.Fatal(err)
		}
		current := sequence.receiveContract
		aheadProof := newHeldHeadContractProof(t, fixture.receiver, promoted.pack.ContractFrame)
		if err := sequence.registerContracts(&receiveItem{contractFrame: aheadProof, contractAhead: true}); err != nil {
			t.Fatal(err)
		}
		var referenced *sequenceContract
		for _, contract := range sequence.openReceiveContracts {
			if contract != current {
				referenced = contract
			}
		}
		if referenced == nil || sequence.receiveContract != current {
			t.Fatal("verified announcement did not remain separate from current authority")
		}
		pack := proto.Clone(promoted.pack).(*protocol.Pack)
		pack.ContractFrame = nil
		pack.ContractId = referenced.contractId.Bytes()
		rewriteHeldHeadContractWire(t, promoted, pack)
		budget.SetTotalByteCount(budget.UsedByteCount())
		fixture.forward(promoted, fixture.receiverIn)
		fixture.acknowledge()
		if sequence.receiveContract != current || current.ackedByteCount != 0 || current.unackedByteCount != 0 ||
			referenced.ackedByteCount == 0 || referenced.unackedByteCount != 0 || fixture.deliveredCount != 3 ||
			fixture.ackedCount != 3 || len(log.messages()) != 0 {
			t.Fatal("compact held head used current/old-proof authority instead of its verified reference")
		}
		assertRetainedBudgetBalance(t, budget)
	})
}

// No-contract peers still promote a metadata-free held owner. A successful
// nil-proof registration is not a promise that receiveContract is non-nil.
func TestReceiveHeldHeadContractNoContractPromotion(t *testing.T) {
	// The pool statistics worker belongs to the process, outside virtual time.
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		fixture := newWindowRoundFixture(t, nil, nil)
		fixture.forward(fixture.write(100), fixture.receiverIn)
		fixture.acknowledge()
		predecessor, tail := fixture.write(100), fixture.write(100)
		fixture.forward(predecessor, fixture.receiverIn)
		fixture.acknowledge()
		old := fixture.receiveSequence()
		old.Cancel()
		<-old.done
		synctest.Wait()
		sender := fixture.sequence()
		wire, full, err := sender.setHead(sender.sendItems[0], true)
		if err != nil {
			t.Fatal(err)
		}
		promoted := &windowRoundFrame{bytes: wire, pack: decodeCompactContractTestPack(t, wire)}
		fixture.heldFrames = append(fixture.heldFrames, promoted)
		if full || promoted.pack.ContractFrame != nil || promoted.pack.ContractId != nil {
			t.Fatal("no-contract sender unexpectedly supplied contract authority")
		}
		fixture.forward(tail, fixture.receiverIn)
		sequence := fixture.receiveSequence()
		if sequence.receiveQueue.Len() != 1 || sequence.receiveContract != nil {
			t.Fatal("no-contract original did not enter the replacement hold")
		}
		fixture.forward(promoted, fixture.receiverIn)
		fixture.acknowledge()
		if fixture.deliveredCount != 3 || fixture.ackedCount != 3 || sequence.receiveContract != nil {
			t.Fatal("metadata-free head could not advance the no-contract held owner")
		}
	})
}
