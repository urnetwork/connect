// Absolute deadlines are tested with native signed contracts and logical time;
// byte settlement remains valid after new admission has stopped.
package connect

import (
	"context"
	"errors"
	"math"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// Uses ordinary protobuf ownership so focused parser/queue tests need no pool.
func expirationTestContract(t *testing.T, source, destination Id, expiration *int64) (*protocol.Contract, *protocol.StoredContract) {
	t.Helper()
	stored := &protocol.StoredContract{
		ContractId: NewId().Bytes(), SourceId: source.Bytes(), DestinationId: destination.Bytes(),
		TransferByteCount: uint64(mib(1)), ExpirationTimeUnixMilli: expiration,
	}
	wire, err := proto.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	return &protocol.Contract{StoredContractBytes: wire, ProvideMode: protocol.ProvideMode_Network}, stored
}

// Pin the peer policy under its normal lock; synthetic time predates rollout.
func expirationRequireSendContract(client *Client, destination Id) {
	manager := client.ContractManager()
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	manager.sendNoContractClientIds[destination] = false
}

// Explicit zero, negative, and the Unix representation of Go's zero time must
// never acquire the legacy absent-field meaning. Integer extremes never wrap.
func TestContractExpirationPresenceAndSignedBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UnixMilli()
		cases := []struct {
			name     string
			deadline int64
			absent   bool
			allowed  bool
		}{
			{name: "legacy", absent: true, allowed: true},
			{name: "zero", deadline: 0},
			{name: "negative", deadline: -1},
			{name: "go-zero-time", deadline: time.Time{}.UnixMilli()},
			{name: "minimum", deadline: math.MinInt64},
			{name: "maximum", deadline: math.MaxInt64, allowed: true},
			{name: "before", deadline: now - 1},
			{name: "equal", deadline: now},
			{name: "after", deadline: now + 1, allowed: true},
		}
		for _, testCase := range cases {
			var deadline *int64
			if !testCase.absent {
				deadline = &testCase.deadline
			}
			wire, stored := expirationTestContract(t, NewId(), NewId(), deadline)
			contract, err := newSequenceContract(NewNoopLogger(), "s", wire, 0, 1)
			if err != nil {
				t.Fatal(err)
			}
			if got := contract.canUpdate(10); got != testCase.allowed {
				t.Errorf("%s canUpdate=%t want=%t", testCase.name, got, testCase.allowed)
			}
			if got := contract.update(10); got != testCase.allowed {
				t.Errorf("%s update=%t want=%t", testCase.name, got, testCase.allowed)
			}
			if acquired := contract.acquireNoAckWriter(); acquired {
				contract.releaseNoAckWriter()
				if !testCase.allowed {
					t.Errorf("%s admitted a no-ack writer", testCase.name)
				}
			} else if testCase.allowed {
				t.Errorf("%s refused an unexpired no-ack writer", testCase.name)
			}
			queue := newContractQueue(NewNoopLogger(), false)
			if err := queue.Add(wire, stored); err != nil {
				t.Fatal(err)
			}
			taken, expired := queue.Poll(time.Time{})
			wantExpired := 1
			if testCase.allowed {
				wantExpired = 0
			}
			if (taken != nil) != testCase.allowed || len(expired) != wantExpired {
				t.Errorf("%s queue admitted=%t expired=%d", testCase.name, taken != nil, len(expired))
			}
			if !testCase.allowed && (contract.ackedByteCount != 0 || contract.unackedByteCount != 0) {
				t.Errorf("%s rejection changed accounting", testCase.name)
			}
		}
	})
}

// Queue re-publication may refresh enqueue time but never the signed deadline.
func TestContractExpirationQueueReaddCannotExtendDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		deadline := time.Now().Add(time.Second).UnixMilli()
		wire, stored := expirationTestContract(t, NewId(), NewId(), &deadline)
		queue := newContractQueue(NewNoopLogger(), true)
		if err := queue.Add(wire, stored); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		if err := queue.Add(wire, stored); err != nil {
			t.Fatal(err)
		}
		if taken, expired := queue.Poll(time.Time{}); taken != nil || len(expired) != 1 || expired[0] != wire {
			t.Fatal("re-enqueue extended the signed deadline with orphan expiry disabled")
		}
	})
}

// The keep-newest exception belongs only to enqueue age. An expired newest
// result cannot displace an older, still valid rollover reserve.
func TestContractExpirationActivePrefetchKeepsOnlyValidSuccessor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, key, valid := newActiveContractPrefetchTestManager(t, true)
		queue := manager.destinationContracts[key]
		deadline := time.Now().UnixMilli()
		expiredWire, stored := expirationTestContract(t, NewId(), key.Destination.DestinationId, &deadline)
		if err := queue.Add(expiredWire, stored); err != nil {
			t.Fatal(err)
		}
		expired := manager.expireQueuedContractsBefore(time.Now().Add(-time.Minute))
		if len(expired) != 1 || expired[0] != expiredWire {
			t.Fatal("active ownership retained the expired newest prefetch")
		}
		if taken, discarded := queue.Poll(time.Time{}); taken != valid || len(discarded) != 0 {
			t.Fatal("expiry discarded the still-valid older rollover reserve")
		}
		if err := queue.Add(expiredWire, stored); err != nil {
			t.Fatal(err)
		}
		expired = manager.expireQueuedContractsBefore(time.Time{})
		if len(expired) != 1 || len(queue.contracts) != 0 {
			t.Fatal("disabled enqueue expiry retained an expired active successor")
		}
	})
}

// Idle grants still retire when the caller has disabled orphan-age cleanup.
func TestContractExpirationIdleQueueExpiresWithAgeLimitDisabled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := closeWaitClientSettings()
		settings.ContractManagerSettings.ContractQueueExpireTimeout = 0
		client := NewClient(t.Context(), ControlId, NewNoContractClientOob(), settings)
		defer func() {
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		manager := client.ContractManager()
		key := ContractKey{Destination: DestinationId(NewId())}
		deadline := time.Now().Add(time.Minute).UnixMilli()
		wire, _ := expirationTestContract(t, client.ClientId(), key.Destination.DestinationId, &deadline)
		if err := manager.addContract(key, wire); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		time.Sleep(time.Minute)
		synctest.Wait()
		if count := expiryQueueCount(manager); count != 0 {
			t.Fatalf("idle expired grant retained %d queues", count)
		}
	})
}

// Even the smallest positive orphan window must yield between cleanup passes.
func TestContractExpirationQueueMinimumCleanupInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := closeWaitClientSettings()
		settings.ContractManagerSettings.ContractQueueExpireTimeout = time.Nanosecond
		client := NewClient(t.Context(), ControlId, NewNoContractClientOob(), settings)
		defer func() {
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		manager := client.ContractManager()
		key := ContractKey{Destination: DestinationId(NewId())}
		wire, _ := expirationTestContract(t, client.ClientId(), key.Destination.DestinationId, nil)
		if err := manager.addContract(key, wire); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		time.Sleep(3 * time.Nanosecond)
		synctest.Wait()
		if count := expiryQueueCount(manager); count != 0 {
			t.Fatalf("minimum interval did not remove orphan queue: %d", count)
		}
	})
}

// New bytes move to the successor while late acknowledgements retain the old
// debit, preventing either over-credit or a premature zero-byte close.
func TestContractExpirationRolloverRetainsPendingAcknowledgements(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, sequence, destination, old := newSendNoContractHarness(t, t.Context())
		expirationRequireSendContract(client, destination)
		deadline := time.Now().Add(time.Second).UnixMilli()
		old.expirationTimeUnixMilli = &deadline
		if !old.update(100) || !old.update(200) {
			t.Fatal("initial debit refused")
		}
		sequence.ackItem(newContractSendItem(&old.contractId, 0, 100))
		ahead := newContractAheadTestContract(t, client, destination)
		ahead.acknowledgedAhead = true
		sequence.openSendContracts[ahead.contractId] = ahead
		sequence.aheadSendContract = ahead
		sequence.aheadSendContractMetadataGeneration = sequence.contractMetadata().generation
		time.Sleep(time.Second)
		if !sequence.updateContract(50) || sequence.sendContract != ahead || !sequence.sendContractAcked {
			t.Fatal("expired active contract did not hand off to its acknowledged successor")
		}
		if old.ackedByteCount != 100 || old.unackedByteCount != 200 || sequence.openSendContracts[old.contractId] != old {
			t.Fatal("rollover changed or closed pending old accounting")
		}
		sequence.ackItem(newContractSendItem(&old.contractId, 1, 200))
		if old.ackedByteCount != 300 || old.unackedByteCount != 0 || sequence.openSendContracts[old.contractId] != nil {
			t.Fatal("late acknowledgement failed to settle the retired contract")
		}
		if ahead.unackedByteCount != 50 || client.ContractManager().LocalStats().ReceiveContractCloseByteCount != 300 {
			t.Fatal("old close or new debit was attributed to the wrong contract")
		}
	})
}

// A queued NoAck packet must return to ordinary recovery admission when the
// deadline requires a fresh opening control, without spending the old grant.
func TestContractExpirationNoAckDebitDefersForRenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, sequence, destination, contract := newSendNoContractHarness(t, t.Context())
		expirationRequireSendContract(client, destination)
		deadline := time.Now().UnixMilli()
		contract.expirationTimeUnixMilli = &deadline
		updated, deferForRecovery := sequence.updateContractWithoutAckPromotion(10)
		if updated || !deferForRecovery || contract.unackedByteCount != 0 || contract.ackedByteCount != 0 {
			t.Fatal("expired NoAck debit did not defer to ordinary contract recovery")
		}
	})
}

// Both the rollover and announcement paths must release an unusable successor.
func TestContractExpirationAnnouncedSuccessorReleasesOpeningReservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, announce := range []bool{false, true} {
			client, sequence, destination, old := newSendNoContractHarness(t, t.Context())
			ahead := newContractAheadTestContract(t, client, destination)
			ahead.minUpdateByteCount = 64
			deadline := time.Now().Add(time.Second).UnixMilli()
			ahead.expirationTimeUnixMilli = &deadline
			if !ahead.update(0) || !old.update(100) {
				t.Fatal("opening reservation refused")
			}
			sequence.openSendContracts[ahead.contractId] = ahead
			sequence.aheadSendContract = ahead
			sequence.aheadSendContractAttempted = true
			sequence.aheadSendContractMetadataGeneration = sequence.contractMetadata().generation
			time.Sleep(time.Second)
			if announce {
				sequence.maybeAnnounceContractAhead()
			} else if sequence.setAheadContract(10) {
				t.Fatal("expired successor became current")
			}
			if sequence.aheadSendContract != nil || sequence.aheadSendContractAttempted || sequence.openSendContracts[ahead.contractId] != nil {
				t.Fatalf("announce=%t retained an expired successor", announce)
			}
			if ahead.ackedByteCount != 0 || ahead.unackedByteCount != 0 || old.unackedByteCount != 100 {
				t.Fatalf("announce=%t changed wire accounting or retained the unused opening debit", announce)
			}
		}
	})
}

// Failure after charging the current contract must undo its unsent control
// debit. This also covers a signed successor expiring between take and debit.
func TestContractExpirationFailedAheadOpeningReturnsAnnouncementDebit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, sequence, destination, current := newSendNoContractHarness(t, t.Context())
		current.minUpdateByteCount = 16
		wire, stored := expirationTestContract(t, client.ClientId(), destination, nil)
		stored.TransferByteCount = 0
		var err error
		wire.StoredContractBytes, err = proto.Marshal(stored)
		if err != nil {
			t.Fatal(err)
		}
		if err := client.ContractManager().addContract(sequence.contractMetadata().key, wire); err != nil {
			t.Fatal(err)
		}
		sequence.announceContractAhead()
		if current.ackedByteCount != 0 || current.unackedByteCount != 0 || sequence.aheadSendContract != nil {
			t.Fatal("failed successor opening consumed the unsent announcement debit")
		}
	})
}

// A stale published pointer cannot admit a new write after expiry. A lease
// accepted earlier is retained through its real wire write and final accounting.
func TestContractExpirationNoAckSnapshotRejectsNewLeaseAndSettlesAcceptedWrite(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		deadline := time.Now().Add(time.Second).UnixMilli()
		h := newNoAckBudgetHarness(t, t.Context(), func(contract *sequenceContract) {
			contract.expirationTimeUnixMilli = &deadline
		})
		expirationRequireSendContract(h.client, h.destinationId)
		sequence, old := h.sequence, h.contract
		writer := sequence.contractMultiRouteWriter
		defer func() { sequence.contractMultiRouteWriter = writer }()
		barrier := &h1RetirementBarrierWriter{MultiRouteWriter: writer, entered: make(chan struct{}), release: make(chan error)}
		defer close(barrier.release)
		sequence.contractMultiRouteWriter = barrier
		sequence.publishNoAckFastPath()
		snapshot := sequence.noAckFastPath.Load()
		if snapshot == nil || snapshot.contract != old {
			t.Fatal("fixture did not publish a contract-backed snapshot")
		}
		frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(40)}
		clear(frame.MessageBytes)
		pack := &SendPack{TransferOptions: TransferOptions{Ack: false}, Frame: frame, Ctx: t.Context(), Destination: h.destinationId}
		done := make(chan bool, 1)
		go func() { done <- sequence.writeNoAckFastPath(snapshot, pack) }()
		<-barrier.entered
		time.Sleep(time.Second)
		lateFrame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(10)}
		clear(lateFrame.MessageBytes)
		latePack := &SendPack{TransferOptions: TransferOptions{Ack: false}, Frame: lateFrame, Ctx: t.Context(), Destination: h.destinationId}
		if sequence.writeNoAckFastPath(snapshot, latePack) {
			t.Fatal("stale snapshot admitted a new write at the signed deadline")
		}
		latePack.disposeUnsentGroup(context.Canceled)
		next := newContractAheadTestContract(t, h.client, h.destinationId)
		sequence.setContract(next, sequence.contractMetadata().generation)
		if sequence.openSendContracts[old.contractId] != old {
			t.Fatal("retirement closed an accepted caller write before settlement")
		}
		barrier.release <- nil
		if !<-done {
			pack.disposeUnsentGroup(context.Canceled)
			t.Fatal("write admitted before expiry was discarded")
		}
		MessagePoolReturn(<-h.route)
		sequence.applyNoAckFastPathAccounting()
		if sequence.openSendContracts[old.contractId] != nil || old.ackedByteCount != 40 || old.unackedByteCount != 0 || next.ackedByteCount != 0 {
			t.Fatal("accepted pre-expiry write settled against the wrong contract")
		}
	})
}

// Verification must reject stale signed proofs before registration, including
// ahead announcements, without confusing clock expiry with signature tampering.
func TestContractExpirationReceiverRejectsExpiredSignedRegistration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newReceiveContractBoundHarness(t)
		for _, ahead := range []bool{false, true} {
			frame := h.newFrame(time.Now().UnixMilli())
			err := h.sequence.registerContracts(&receiveItem{contractFrame: frame, contractAhead: ahead})
			MessagePoolReturn(frame.MessageBytes)
			delete(h.expected, h.ids[len(h.ids)-1])
			if (!ahead && !errors.Is(err, errContractExpired)) || (ahead && err != nil) ||
				h.sequence.receiveContract != nil || len(h.sequence.openReceiveContracts) != 0 {
				t.Fatalf("ahead=%t admitted expired signed proof: %v", ahead, err)
			}
			if h.sequence.rejectRetransmits {
				t.Fatal("ordinary expiry tombstoned the sender as an invalid signature")
			}
		}
	})
}

// Late optional metadata cannot tear down a live grant or become permission
// for the enclosing Pack when its current grant is absent or expired.
func TestContractExpirationLateAnnouncementKeepsOnlyCurrentAuthority(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, currentState := range []string{"live", "expired", "absent"} {
			h := newReceiveContractBoundHarness(t)
			manager := h.client.ContractManager()
			manager.mutex.Lock()
			manager.receiveNoContractClientIds[h.source] = false
			manager.mutex.Unlock()
			if currentState != "absent" {
				deadline := time.Now().Add(time.Hour).UnixMilli()
				if currentState == "expired" {
					deadline = time.Now().Add(time.Second).UnixMilli()
				}
				frame := h.newFrame(deadline)
				h.register(frame, false)
				MessagePoolReturn(frame.MessageBytes)
			}
			current := h.sequence.receiveContract
			if currentState == "expired" {
				time.Sleep(time.Second)
			}
			frame := h.newFrame(time.Now().UnixMilli())
			expiredId := h.ids[len(h.ids)-1]
			delete(h.expected, expiredId)
			pack := &protocol.Pack{
				MessageId: NewId().Bytes(), SequenceId: h.sequence.sequenceId.Bytes(),
				SequenceNumber: 0, ContractAhead: true, ContractFrame: frame,
			}
			if current != nil {
				pack.ContractId = current.contractId.Bytes()
			}
			received, err := h.sequence.receive(&ReceivePack{Pack: pack})
			if received {
				h.sequence.flushDeliver()
			} else {
				MessagePoolReturn(frame.MessageBytes)
			}
			if received != (currentState == "live") || (err == nil) != received {
				t.Fatalf("current=%s expired announcement received=%t err=%v", currentState, received, err)
			}
			if h.sequence.openReceiveContracts[expiredId] != nil || h.sequence.receiveContract != current || h.sequence.rejectRetransmits {
				t.Fatalf("current=%s expired metadata changed registered authority", currentState)
			}
			if received {
				expected := h.expected[current.contractId]
				expected.acked = uint64(current.minUpdateByteCount)
				h.expected[current.contractId] = expected
				if current.ackedByteCount != current.minUpdateByteCount || current.unackedByteCount != 0 || current.statsEntry.closed.Load() {
					t.Fatal("late announcement changed current accounting or stats")
				}
				delivered := false
				payload := []byte("synthetic-after-announcement")
				received, err = h.sequence.receive(&ReceivePack{
					Pack: &protocol.Pack{
						MessageId: NewId().Bytes(), SequenceId: h.sequence.sequenceId.Bytes(),
						SequenceNumber: 1, ContractId: current.contractId.Bytes(),
						Frames: []*protocol.Frame{{MessageType: protocol.MessageType_TestSimpleMessage, MessageBytes: payload}},
					},
					MessageByteCount: ByteCount(len(payload)),
					ReceiveCallback:  func(_ TransferPath, _ []*protocol.Frame, _ Peer) { delivered = true },
				})
				if err != nil || !received {
					t.Fatalf("live lane failed after expired announcement: %v", err)
				}
				h.sequence.flushDeliver()
				if !delivered {
					t.Fatal("expired optional metadata prevented later current-contract delivery")
				}
				expected.acked += uint64(len(payload))
				h.expected[current.contractId] = expected
			}
		}
	})
}

// Already-registered explicit and implicit contracts stop new receive debits
// at the deadline even if no-contract traffic is otherwise permitted.
func TestContractExpirationReceiverStopsDebitButAllowsSettlement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newReceiveContractBoundHarness(t)
		deadline := time.Now().Add(time.Second).UnixMilli()
		frame := h.newFrame(deadline)
		h.register(frame, false)
		MessagePoolReturn(frame.MessageBytes)
		contract := h.sequence.receiveContract
		h.debit(contract, false, 100, 50)
		time.Sleep(time.Second)
		for _, implicit := range []bool{false, true} {
			item := &receiveItem{transferItem: transferItem{messageByteCount: 10}}
			if !implicit {
				item.contractId = &contract.contractId
			}
			if h.sequence.updateContract(item) {
				t.Fatalf("implicit=%t debited an expired receive contract", implicit)
			}
		}
		if contract.ackedByteCount != 100 || contract.unackedByteCount != 50 {
			t.Fatal("rejected receive debit changed accounting")
		}
		contract.ack(50)
		expected := h.expected[contract.contractId]
		expected.acked, expected.unacked = 150, 0
		h.expected[contract.contractId] = expected
		if contract.ackedByteCount != 150 || contract.unackedByteCount != 0 {
			t.Fatal("expiry prevented settlement of previously admitted receive bytes")
		}
	})
}
