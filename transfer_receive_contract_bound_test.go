package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

type receiveContractBoundClose struct {
	count      int
	acked      uint64
	unacked    uint64
	checkpoint bool
}

// The contracts have synthetic identities, but use the real provider HMAC,
// protobuf decoder, path verification and receive registration paths. Financial
// retirement uses the real CloseContract/ControlSync and native client loopback;
// the callback observes its decoded close messages, without an external service.
type receiveContractBoundHarness struct {
	t        *testing.T
	client   *Client
	sequence *ReceiveSequence
	source   Id
	closed   bool
	mutex    sync.Mutex
	posts    map[Id]receiveContractBoundClose
	expected map[Id]receiveContractBoundClose
	ids      []Id
	onClose  func()
}

func newReceiveContractBoundHarness(t *testing.T) *receiveContractBoundHarness {
	t.Helper()
	h := &receiveContractBoundHarness{
		t: t, source: NewId(), posts: map[Id]receiveContractBoundClose{},
		expected: map[Id]receiveContractBoundClose{},
	}
	h.client = NewClient(t.Context(), ControlId, NewNoContractClientOob(), closeWaitClientSettings())
	h.client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			if frame.MessageType != protocol.MessageType_TransferCloseContract {
				continue
			}
			message, err := FromFrame(frame)
			if err != nil {
				t.Error(err)
				continue
			}
			closeContract := message.(*protocol.CloseContract)
			id, err := IdFromBytes(closeContract.ContractId)
			if err != nil {
				t.Error(err)
				continue
			}
			h.mutex.Lock()
			post := h.posts[id]
			post.count++
			post.acked = closeContract.AckedByteCount
			post.unacked = closeContract.UnackedByteCount
			post.checkpoint = closeContract.Checkpoint
			h.posts[id] = post
			onClose := h.onClose
			h.mutex.Unlock()
			if onClose != nil {
				onClose()
			}
		}
	})
	h.client.ContractManager().SetProvideModesWithReturnTraffic(map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Network: true,
	})
	// Starting stats makes closed flags observable independently of map removal.
	h.client.ContractManager().AddContractStatsCallback(func([]*ContractStatsEvent) {})
	h.sequence = NewReceiveSequence(t.Context(), h.client, SourceId(h.source), NewId(),
		sequenceTlsRoleClient, false, DefaultReceiveBufferSettings())
	h.sequence.peerAudit = NewSequencePeerAudit(h.client, SourceId(h.source), 0)
	t.Cleanup(func() {
		h.finishSequence()
		if err := h.client.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return h
}

func (h *receiveContractBoundHarness) newFrame(expirationTimes ...int64) *protocol.Frame {
	h.t.Helper()
	id := NewId()
	storedContract := &protocol.StoredContract{
		ContractId: id.Bytes(), TransferByteCount: uint64(mib(1)),
		SourceId: h.source.Bytes(), DestinationId: h.client.ClientId().Bytes(),
	}
	if len(expirationTimes) != 0 {
		storedContract.ExpirationTimeUnixMilli = &expirationTimes[0]
	}
	stored, err := ProtoMarshal(storedContract)
	if err != nil {
		h.t.Fatal(err)
	}
	defer MessagePoolReturn(stored)
	manager := h.client.ContractManager()
	secret, ok := manager.GetProvideSecretKey(protocol.ProvideMode_Network)
	if !ok {
		h.t.Fatal("receiver did not publish its local provider key")
	}
	frame, err := ToFrame(&protocol.Contract{
		StoredContractBytes: stored,
		StoredContractHmac:  SignStoredContract(manager.settings, secret, stored),
		ProvideMode:         protocol.ProvideMode_Network,
	}, DefaultProtocolVersion)
	if err != nil {
		h.t.Fatal(err)
	}
	h.ids = append(h.ids, id)
	h.expected[id] = receiveContractBoundClose{count: 1}
	return frame
}

func (h *receiveContractBoundHarness) register(frame *protocol.Frame, ahead bool) {
	h.t.Helper()
	if err := h.sequence.registerContracts(&receiveItem{contractFrame: frame, contractAhead: ahead}); err != nil {
		h.t.Fatalf("verified contract registration (ahead=%t): %v", ahead, err)
	}
}

func (h *receiveContractBoundHarness) debit(contract *sequenceContract, implicit bool, acked, unacked ByteCount) {
	h.t.Helper()
	item := &receiveItem{transferItem: transferItem{messageByteCount: acked + unacked}}
	if !implicit {
		item.contractId = &contract.contractId
	}
	if !h.sequence.updateContract(item) || item.contractId == nil || *item.contractId != contract.contractId {
		h.t.Fatal("held/reordered data was refused or charged to the wrong contract")
	}
	contract.ack(acked)
	expected := h.expected[contract.contractId]
	expected.acked += uint64(acked)
	expected.unacked += uint64(unacked)
	h.expected[contract.contractId] = expected
}

func (h *receiveContractBoundHarness) rotate(count int, announce bool) {
	h.t.Helper()
	for index := range count {
		frame := h.newFrame()
		previous := h.sequence.receiveContract
		if previous != nil {
			if announce {
				h.register(frame, true)
				// An acknowledged retransmission must neither reserve a second slot
				// nor reset the successor's accounting/stat entry.
				ahead := h.sequence.openReceiveContracts[h.ids[len(h.ids)-1]]
				h.register(frame, true)
				if h.sequence.receiveContract != previous || h.sequence.openReceiveContracts[ahead.contractId] != ahead {
					h.t.Fatal("announcement changed current ownership or replaced its accepted successor")
				}
			}
			h.debit(previous, true, ByteCount(index+1), 7)
		}
		h.register(frame, false)
		MessagePoolReturn(frame.MessageBytes)
		if previous != nil && !previous.statsEntry.closed.Load() {
			h.t.Fatal("activating an announced successor left predecessor stats open")
		}
		if h.sequence.receiveContract.statsEntry.closed.Load() {
			h.t.Fatal("activation closed the current contract's stats")
		}
	}
}

func (h *receiveContractBoundHarness) finishSequence() {
	if h.closed {
		return
	}
	h.closed = true
	if current := h.sequence.receiveContract; current != nil {
		expected := h.expected[current.contractId]
		expected.checkpoint = true
		h.expected[current.contractId] = expected
	}
	// Direct driving ends before Run starts. Its canceled entry executes the
	// production final close/checkpoint, audit and ACK-worker cleanup path.
	h.sequence.Cancel()
	h.sequence.Run()
	h.sequence.Close()
}

func (h *receiveContractBoundHarness) requirePosts(final bool) {
	h.t.Helper()
	synctest.Wait()
	h.mutex.Lock()
	defer h.mutex.Unlock()
	expectedPosts := 0
	var expectedAcked uint64
	for id, expected := range h.expected {
		_, retained := h.sequence.openReceiveContracts[id]
		if !final && retained {
			if actual := h.posts[id]; actual.count != 0 {
				h.t.Errorf("retained contract %s was closed early: %+v", id, actual)
			}
			continue
		}
		expectedPosts++
		expectedAcked += expected.acked
		if actual := h.posts[id]; actual != expected {
			h.t.Errorf("close %s = %+v, want %+v", id, actual, expected)
		}
	}
	if len(h.posts) != expectedPosts {
		h.t.Errorf("posted contract identities = %d, want %d", len(h.posts), expectedPosts)
	}
	manager := h.client.ContractManager()
	if actual := manager.LocalStats().ReceiveContractCloseByteCount; uint64(actual) != expectedAcked {
		h.t.Errorf("financial receive-close stats = %d, want %d", actual, expectedAcked)
	}
	if final {
		manager.contractStatsLock.Lock()
		defer manager.contractStatsLock.Unlock()
		for key, entry := range manager.contractStatsEntries {
			if _, ours := h.expected[key.contractId]; ours && key.receive && !entry.closed.Load() {
				h.t.Error("sequence retirement left a contract stats entry open")
			}
		}
	}
}

func TestReceiveContractAheadRotationsRetireOverflow(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		h := newReceiveContractBoundHarness(t)
		h.rotate(64, true)
		limit := h.sequence.receiveBufferSettings.MaxOpenReceiveContract
		retainedBytes := 0
		for _, contract := range h.sequence.openReceiveContracts {
			retainedBytes += len(contract.contract.StoredContractBytes)
		}
		t.Logf("64 authenticated rotations: retained=%d limit=%d signed_contract_bytes=%d", len(h.sequence.openReceiveContracts), limit, retainedBytes)
		if got := len(h.sequence.openReceiveContracts); got != limit {
			t.Fatalf("announced-successor rotations retained %d contracts; want existing allowance %d", got, limit)
		}
		for _, id := range h.ids[len(h.ids)-limit:] {
			if h.sequence.openReceiveContracts[id] == nil {
				t.Fatal("overflow trim removed a contract from the existing reorder tail")
			}
		}
		h.requirePosts(false)
		h.debit(h.sequence.receiveContract, true, 11, 13)
		h.finishSequence()
		h.requirePosts(true)
	})
}

func TestReceiveContractAheadPreservesHeldSuccessorAndFinancialTail(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		h := newReceiveContractBoundHarness(t)
		held := make(chan struct{})
		release := make(chan struct{})
		var heldOnce, releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		defer unblock()
		h.mutex.Lock()
		h.onClose = func() {
			heldOnce.Do(func() { close(held) })
			<-release
		}
		h.mutex.Unlock()
		h.rotate(12, true)
		synctest.Wait()
		select {
		case <-held:
		default:
			t.Fatal("overflow retirement did not reach the real close callback")
		}
		current := h.sequence.receiveContract
		frame := h.newFrame()
		h.register(frame, true)
		h.register(frame, true)
		MessagePoolReturn(frame.MessageBytes)
		future := h.sequence.openReceiveContracts[h.ids[len(h.ids)-1]]
		if h.sequence.receiveContract != current || future == nil || future.statsEntry.closed.Load() {
			t.Fatal("held announcement changed the current contract or retired the accepted future")
		}
		limit := h.sequence.receiveBufferSettings.MaxOpenReceiveContract
		if got := len(h.sequence.openReceiveContracts); got != limit+1 {
			t.Fatalf("held successor retained %d contracts, want %d existing plus one accepted future", got, limit)
		}
		// The oldest retained predecessor remains usable during reordering;
		// unqualified traffic still belongs to the active contract, not future.
		oldest := h.sequence.openReceiveContracts[h.ids[len(h.ids)-1-limit]]
		h.debit(oldest, false, 3, 5)
		h.debit(current, true, 17, 19)
		h.finishSequence()
		synctest.Wait()
		h.client.ContractManager().mutex.Lock()
		pendingCloses := len(h.client.ContractManager().closeControlSyncs)
		h.client.ContractManager().mutex.Unlock()
		if pendingCloses == 0 {
			t.Fatal("sequence retirement abandoned financial close work before callback completion")
		}
		unblock()
		h.requirePosts(true)
		t.Logf("accepted future closed unused; current checkpointed; %d held close owners completed with exact byte accounting", pendingCloses)
	})
}

func TestReceiveContractAheadActivationPreservesNewerAcceptedFutures(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		h := newReceiveContractBoundHarness(t)
		h.rotate(4, true)
		const futureCount = 8
		frames := make([]*protocol.Frame, 0, futureCount)
		defer func() {
			for _, frame := range frames {
				if frame != nil {
					MessagePoolReturn(frame.MessageBytes)
				}
			}
		}()
		for range futureCount {
			frame := h.newFrame()
			frames = append(frames, frame)
			h.register(frame, true)
		}
		futureIDs := h.ids[len(h.ids)-futureCount:]
		for index, frame := range frames {
			h.register(frame, false)
			MessagePoolReturn(frame.MessageBytes)
			frames[index] = nil
			for _, id := range futureIDs[index:] {
				contract := h.sequence.openReceiveContracts[id]
				if contract == nil || contract.statsEntry.closed.Load() {
					t.Fatal("activation retired an already accepted current/future contract")
				}
			}
			h.requirePosts(false)
		}
		if got, want := len(h.sequence.openReceiveContracts), h.sequence.receiveBufferSettings.MaxOpenReceiveContract; got != want {
			t.Fatalf("activated future set retained %d contracts, want %d", got, want)
		}
		h.finishSequence()
		h.requirePosts(true)
	})
}

func TestReceiveContractOrdinaryRotationsPreserveRetirement(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		h := newReceiveContractBoundHarness(t)
		h.rotate(12, false)
		if got, want := len(h.sequence.openReceiveContracts), h.sequence.receiveBufferSettings.MaxOpenReceiveContract; got != want {
			t.Fatalf("unannounced rotations retained %d contracts, want %d", got, want)
		}
		h.requirePosts(false)
		h.finishSequence()
		h.requirePosts(true)
	})
}
