package connect

// A window client's two contract duties: carrying the subject's delegation to the
// platform, and holding every contract to the key it expects of its target. The
// control service here is the test: it sees each request and answers it, with the
// contract as the platform could write it.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// A key that is nobody's, for a check that wants another.
func contractDelegationTestKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	publicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return publicKey
}

// A contract from `client` to `destinationId` whose stored bytes name
// `storedKey`, as a result frame. Nothing signs what `outerKey` says.
func contractDelegationResult(
	t *testing.T,
	client *Client,
	destinationId Id,
	storedKey []byte,
	outerKey []byte,
) (*protocol.Frame, Id) {
	t.Helper()
	contractId := NewId()
	storedBytes, err := proto.Marshal(&protocol.StoredContract{
		ContractId:                 contractId.Bytes(),
		TransferByteCount:          uint64(mib(1)),
		SourceId:                   client.ClientId().Bytes(),
		DestinationId:              destinationId.Bytes(),
		DestinationClientPublicKey: storedKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	resultBytes, err := proto.Marshal(&protocol.CreateContractResult{Contract: &protocol.Contract{
		StoredContractBytes:        storedBytes,
		ProvideMode:                protocol.ProvideMode_Network,
		DestinationClientPublicKey: outerKey,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return &protocol.Frame{MessageType: protocol.MessageType_TransferCreateContractResult, MessageBytes: resultBytes}, contractId
}

// Every close a client's manager reports, and its statuses. `statuses` is the
// public callback, which keeps the latest per contract key; `dispatched` has all
// of them, in order.
type contractDelegationWatch struct {
	stateLock  sync.Mutex
	closes     []*protocol.CloseContract
	dispatched []ContractStatus
	statuses   chan *ContractStatus
}

func watchContractDelegation(t *testing.T, client *Client) *contractDelegationWatch {
	t.Helper()
	self := &contractDelegationWatch{statuses: make(chan *ContractStatus, 64)}
	manager := client.ContractManager()
	manager.beforeOriginalCloseFrameForTest = func(report *protocol.CloseContract) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.closes = append(self.closes, proto.Clone(report).(*protocol.CloseContract))
	}
	t.Cleanup(manager.addContractStatusDispatchCallback(func(status *ContractStatus) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.dispatched = append(self.dispatched, *status)
	}))
	t.Cleanup(manager.AddContractStatusCallback(func(status *ContractStatus) {
		self.statuses <- status
	}))
	return self
}

// Every status so far, in order.
func (self *contractDelegationWatch) dispatchedStatuses() []ContractStatus {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.dispatched)
}

// The one close of `contractId`, which must have spent nothing.
func (self *contractDelegationWatch) requireUnusedClose(t *testing.T, contractId Id) {
	t.Helper()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	closes := slices.DeleteFunc(slices.Clone(self.closes), func(report *protocol.CloseContract) bool {
		return !bytes.Equal(report.ContractId, contractId.Bytes())
	})
	if len(closes) != 1 || closes[0].AckedByteCount != 0 || closes[0].UnackedByteCount != 0 || closes[0].Checkpoint {
		t.Fatalf("closes of %s = %v, want one spending nothing", contractId, closes)
	}
}

func (self *contractDelegationWatch) closeCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.closes)
}

// Takes the next status, which must be a trust refusal for `contractKey`.
func (self *contractDelegationWatch) requireRefusal(t *testing.T, contractKey ContractKey) {
	t.Helper()
	select {
	case status := <-self.statuses:
		if status.Error == nil || *status.Error != protocol.ContractError_Trust || status.Key != contractKey {
			t.Fatalf("status %+v, want a trust refusal for %+v", status, contractKey)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no contract status was reported")
	}
}

// Asks for a contract and answers it with `result`.
func requestContractDelegationResult(
	t *testing.T,
	client *Client,
	oob *pendingContractCreates,
	contractKey ContractKey,
	seqIndex uint64,
	result *protocol.Frame,
) {
	t.Helper()
	index := oob.count()
	client.ContractManager().CreateContract(contractKey, seqIndex, 0)
	if oob.count() != index+1 {
		t.Fatalf("%d requests, want %d", oob.count(), index+1)
	}
	oob.completeFrames(index, []*protocol.Frame{result}, nil)
}

// The bytes reach the request as they were set, and the caller's slice is not
// what the manager holds.
func TestWindowDelegationReachesCreateContractUnchanged(t *testing.T) {
	client, oob := newPendingContractClient(t, nil)
	manager := client.ContractManager()
	delegation := []byte("synthetic signed window delegation")
	want := bytes.Clone(delegation)
	manager.SetWindowDelegation(delegation)
	delegation[0] ^= 0xff

	contractKey := ContractKey{Destination: DestinationId(NewId())}
	manager.CreateContract(contractKey, 0, 0)
	if oob.count() != 1 {
		t.Fatalf("%d requests, want 1", oob.count())
	}
	if !bytes.Equal(oob.requests[0].WindowDelegation, want) {
		t.Fatalf("request carried %q, want %q", oob.requests[0].WindowDelegation, want)
	}
}

// A re-sign reaches the next contract requested, and neither an earlier request
// nor a contract already queued moves with it.
func TestWindowDelegationSwapReachesOnlyLaterContracts(t *testing.T) {
	client, oob := newPendingContractClient(t, nil)
	manager := client.ContractManager()
	first, second := []byte("first delegation"), []byte("second delegation")
	contractKey := ContractKey{Destination: DestinationId(NewId())}

	manager.SetWindowDelegation(first)
	manager.CreateContract(contractKey, 0, 0)
	result, contractId := contractDelegationResult(t, client, contractKey.Destination.DestinationId, nil, nil)
	oob.completeFrames(0, []*protocol.Frame{result}, nil)

	manager.SetWindowDelegation(second)
	manager.CreateContract(contractKey, 1, 0)
	if oob.count() != 2 {
		t.Fatalf("%d requests, want 2", oob.count())
	}
	if !bytes.Equal(oob.requests[0].WindowDelegation, first) {
		t.Errorf("first request carries %q", oob.requests[0].WindowDelegation)
	}
	if !bytes.Equal(oob.requests[1].WindowDelegation, second) {
		t.Errorf("second request carries %q", oob.requests[1].WindowDelegation)
	}
	contract := manager.TakeContract(context.Background(), contractKey, 0)
	if contract == nil {
		t.Fatal("the re-sign disturbed the queued contract")
	}
	var stored protocol.StoredContract
	if err := proto.Unmarshal(contract.StoredContractBytes, &stored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored.ContractId, contractId.Bytes()) {
		t.Errorf("took %x, want %s", stored.ContractId, contractId)
	}

	manager.SetWindowDelegation(nil)
	manager.CreateContract(contractKey, 2, 0)
	if oob.count() != 3 || oob.requests[2].WindowDelegation != nil {
		t.Errorf("a cleared delegation still rode request %d: %q", oob.count()-1, oob.requests[oob.count()-1].WindowDelegation)
	}
}

// A companion's contract inherits its forward contract's seals at the signer, so
// the request itself carries nothing to seal.
func TestCompanionContractRequestCarriesNoWindowDelegation(t *testing.T) {
	client, oob := newPendingContractClient(t, nil)
	manager := client.ContractManager()
	manager.SetWindowDelegation([]byte("synthetic signed window delegation"))

	destinationId := NewId()
	manager.CreateContract(ContractKey{Destination: DestinationId(destinationId), CompanionContract: true}, 0, 0)
	manager.CreateContract(ContractKey{Destination: DestinationId(destinationId)}, 0, 0)
	if oob.count() != 2 {
		t.Fatalf("%d requests, want 2", oob.count())
	}
	if !oob.requests[0].Companion || oob.requests[0].WindowDelegation != nil {
		t.Errorf("companion request: companion=%t delegation=%q", oob.requests[0].Companion, oob.requests[0].WindowDelegation)
	}
	if oob.requests[1].Companion || oob.requests[1].WindowDelegation == nil {
		t.Errorf("forward request: companion=%t delegation=%q", oob.requests[1].Companion, oob.requests[1].WindowDelegation)
	}
}

// With neither call made, a request has the bytes of one built before they
// existed, and every contract is taken as it comes: no key, a forged outer copy,
// another destination's id. The check stays out of a client that did not ask.
func TestContractDelegationOffChangesNothing(t *testing.T) {
	client, oob := newPendingContractClient(t, nil)
	manager := client.ContractManager()
	watch := watchContractDelegation(t, client)
	destinationId := NewId()
	contractKey := ContractKey{Destination: DestinationId(destinationId)}

	result, _ := contractDelegationResult(t, client, destinationId, nil, contractDelegationTestKey(t))
	requestContractDelegationResult(t, client, oob, contractKey, 0, result)
	streamVersion := uint32(DefaultStreamVersion)
	senderRole := contractKey.EncryptionRole.toProtobuf()
	forceStream := false
	want, err := proto.MarshalOptions{Deterministic: true}.Marshal(&protocol.CreateContract{
		DestinationId:     destinationId.Bytes(),
		IntermediaryIds:   contractKey.IntermediaryIds.Bytes(),
		TransferByteCount: oob.requests[0].TransferByteCount,
		ForceStream:       &forceStream,
		StreamVersion:     &streamVersion,
		SenderRole:        &senderRole,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := proto.MarshalOptions{Deterministic: true}.Marshal(oob.requests[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("request bytes moved:\n%x\n%x", got, want)
	}

	if manager.TakeContract(context.Background(), contractKey, 0) == nil {
		t.Error("a contract with no key was held back")
	}
	result, _ = contractDelegationResult(t, client, NewId(), contractDelegationTestKey(t), nil)
	requestContractDelegationResult(t, client, oob, contractKey, 1, result)
	if manager.TakeContract(context.Background(), contractKey, 0) == nil {
		t.Error("a contract naming another destination was held back")
	}
	if watch.closeCount() != 0 {
		t.Errorf("%d closes, want none", watch.closeCount())
	}
	statuses := watch.dispatchedStatuses()
	if len(statuses) != 2 {
		t.Fatalf("%d statuses, want one per contract", len(statuses))
	}
	for _, status := range statuses {
		if status.Error != nil {
			t.Errorf("status error %s", *status.Error)
		}
	}
}

// What a check is given, with the contract the check is run on.
type contractDelegationCheckCall struct {
	destinationId Id
	key           []byte
}

// A check that admits only `goodKey`, and remembers what it was asked.
type contractDelegationCheck struct {
	goodKey []byte

	stateLock sync.Mutex
	calls     []contractDelegationCheckCall
}

func (self *contractDelegationCheck) check(destinationId Id, destinationClientPublicKey []byte) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.calls = append(self.calls, contractDelegationCheckCall{destinationId: destinationId, key: bytes.Clone(destinationClientPublicKey)})
	return bytes.Equal(destinationClientPublicKey, self.goodKey)
}

func (self *contractDelegationCheck) callCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.calls)
}

// The stored key decides, whichever way the outer copy says otherwise, because
// nothing signs the outer copy. Each case arrives through both queue paths: a
// request's own result, and a control frame with no request behind it.
func TestContractDestinationCheckReadsOnlyTheStoredKey(t *testing.T) {
	goodKey := contractDelegationTestKey(t)
	badKey := contractDelegationTestKey(t)
	cases := []struct {
		name     string
		stored   []byte
		outer    []byte
		admitted bool
	}{
		{name: "good stored", stored: goodKey, admitted: true},
		{name: "forged outer, good stored", stored: goodKey, outer: badKey, admitted: true},
		{name: "bad stored", stored: badKey},
		{name: "bad stored, good outer", stored: badKey, outer: goodKey},
		{name: "empty stored, good outer", outer: goodKey},
		{name: "empty stored", stored: nil},
		{name: "short stored", stored: goodKey[:ed25519.PublicKeySize-1], outer: goodKey},
		{name: "long stored", stored: append(bytes.Clone(goodKey), 0), outer: goodKey},
	}
	for _, c := range cases {
		for _, ownedQueue := range []bool{true, false} {
			client, oob := newPendingContractClient(t, nil)
			manager := client.ContractManager()
			watch := watchContractDelegation(t, client)
			check := &contractDelegationCheck{goodKey: goodKey}
			manager.SetContractDestinationCheck(check.check)

			destinationId := NewId()
			contractKey := ContractKey{Destination: DestinationId(destinationId)}
			result, contractId := contractDelegationResult(t, client, destinationId, c.stored, c.outer)
			if ownedQueue {
				requestContractDelegationResult(t, client, oob, contractKey, 0, result)
			} else if err := manager.HandleControlFrame(contractKey, result); err != nil {
				t.Fatal(err)
			}

			contract := manager.TakeContract(context.Background(), contractKey, 0)
			if c.admitted {
				if contract == nil {
					t.Errorf("%s (owned=%t): refused", c.name, ownedQueue)
				}
				if watch.closeCount() != 0 {
					t.Errorf("%s (owned=%t): %d closes", c.name, ownedQueue, watch.closeCount())
				}
				continue
			}
			if contract != nil {
				t.Errorf("%s (owned=%t): a sequence took the contract", c.name, ownedQueue)
			}
			watch.requireUnusedClose(t, contractId)
			watch.requireRefusal(t, contractKey)
			if opened := manager.LocalStats().ContractOpenCount; opened != 0 {
				t.Errorf("%s (owned=%t): %d open contracts", c.name, ownedQueue, opened)
			}
		}
	}
}

// A contract for another destination is no use on this key however good its own
// key is, and the check is never asked about it.
func TestContractDestinationCheckRefusesAnotherDestination(t *testing.T) {
	goodKey := contractDelegationTestKey(t)
	client, oob := newPendingContractClient(t, nil)
	manager := client.ContractManager()
	watch := watchContractDelegation(t, client)
	check := &contractDelegationCheck{goodKey: goodKey}
	manager.SetContractDestinationCheck(check.check)

	contractKey := ContractKey{Destination: DestinationId(NewId())}
	result, contractId := contractDelegationResult(t, client, NewId(), goodKey, nil)
	requestContractDelegationResult(t, client, oob, contractKey, 0, result)

	if manager.TakeContract(context.Background(), contractKey, 0) != nil {
		t.Error("a sequence took another destination's contract")
	}
	watch.requireUnusedClose(t, contractId)
	watch.requireRefusal(t, contractKey)
	if check.callCount() != 0 {
		t.Errorf("the check ran %d times on a mismatched contract", check.callCount())
	}
}

// The check is told the destination and the stored key, the same on both queue
// paths, once as the contract is queued and once as a sequence takes it.
func TestContractDestinationCheckRunsAtQueueAndAtTake(t *testing.T) {
	goodKey := contractDelegationTestKey(t)
	client, oob := newPendingContractClient(t, nil)
	manager := client.ContractManager()
	check := &contractDelegationCheck{goodKey: goodKey}
	manager.SetContractDestinationCheck(check.check)

	destinationId := NewId()
	contractKey := ContractKey{Destination: DestinationId(destinationId)}
	result, _ := contractDelegationResult(t, client, destinationId, goodKey, nil)
	requestContractDelegationResult(t, client, oob, contractKey, 0, result)
	if check.callCount() != 1 {
		t.Fatalf("%d calls after the result, want 1", check.callCount())
	}
	if manager.TakeContract(context.Background(), contractKey, 0) == nil {
		t.Fatal("the contract was refused")
	}
	if check.callCount() != 2 {
		t.Fatalf("%d calls after the take, want 2", check.callCount())
	}
	for _, call := range check.calls {
		if call.destinationId != destinationId || !bytes.Equal(call.key, goodKey) {
			t.Errorf("check asked about %s %x", call.destinationId, call.key)
		}
	}
}

// A contract queued before the check was installed still answers to it as a
// sequence takes it. The refusal spends that contract alone: the same take moves
// on to the good one behind it.
func TestContractDestinationCheckCatchesAContractAlreadyQueued(t *testing.T) {
	goodKey := contractDelegationTestKey(t)
	client, _ := newPendingContractClient(t, nil)
	manager := client.ContractManager()
	watch := watchContractDelegation(t, client)

	destinationId := NewId()
	contractKey := ContractKey{Destination: DestinationId(destinationId)}
	result, refusedId := contractDelegationResult(t, client, destinationId, contractDelegationTestKey(t), nil)
	if err := manager.HandleControlFrame(contractKey, result); err != nil {
		t.Fatal(err)
	}
	check := &contractDelegationCheck{goodKey: goodKey}
	manager.SetContractDestinationCheck(check.check)

	if manager.TakeContract(context.Background(), contractKey, 0) != nil {
		t.Fatal("a sequence took a contract queued with the wrong key")
	}
	watch.requireUnusedClose(t, refusedId)
	statuses := watch.dispatchedStatuses()
	if len(statuses) != 2 || statuses[0].Error != nil || statuses[1].Error == nil ||
		*statuses[1].Error != protocol.ContractError_Trust || statuses[1].Key != contractKey {
		t.Fatalf("statuses %+v, want the queued contract's then its refusal", statuses)
	}
	stats := manager.LocalStats()
	if stats.ContractOpenCount != 1 || stats.ContractCloseCount != 1 || 0 < len(stats.ContractOpenKeys) || stats.ContractCloseByteCount != 0 {
		t.Errorf("stats after a refused take: %+v", stats)
	}

	// a good contract behind a bad one, in either order the queue gives them
	manager.SetContractDestinationCheck(nil)
	result, badId := contractDelegationResult(t, client, destinationId, contractDelegationTestKey(t), nil)
	if err := manager.HandleControlFrame(contractKey, result); err != nil {
		t.Fatal(err)
	}
	result, goodId := contractDelegationResult(t, client, destinationId, goodKey, nil)
	if err := manager.HandleControlFrame(contractKey, result); err != nil {
		t.Fatal(err)
	}
	manager.SetContractDestinationCheck(check.check)
	contract := manager.TakeContract(context.Background(), contractKey, 0)
	if contract == nil {
		t.Fatal("the refusal took the good contract with it")
	}
	var stored protocol.StoredContract
	if err := proto.Unmarshal(contract.StoredContractBytes, &stored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored.ContractId, goodId.Bytes()) {
		t.Errorf("took %x, want the good contract %s", stored.ContractId, goodId)
	}
	if manager.TakeContract(context.Background(), contractKey, 0) != nil {
		t.Error("the bad contract was still there to take")
	}
	watch.requireUnusedClose(t, badId)
}

// A panic in the check is a refusal, not a crash on a shared control callback
// and not an admitted contract.
func TestContractDestinationCheckPanicRefuses(t *testing.T) {
	client, oob := newPendingContractClient(t, nil)
	manager := client.ContractManager()
	watch := watchContractDelegation(t, client)
	manager.SetContractDestinationCheck(func(Id, []byte) bool {
		panic(errors.New("synthetic check failure"))
	})

	destinationId := NewId()
	contractKey := ContractKey{Destination: DestinationId(destinationId)}
	result, contractId := contractDelegationResult(t, client, destinationId, contractDelegationTestKey(t), nil)
	requestContractDelegationResult(t, client, oob, contractKey, 0, result)
	if manager.TakeContract(context.Background(), contractKey, 0) != nil {
		t.Error("a sequence took a contract the check never answered for")
	}
	watch.requireUnusedClose(t, contractId)
	watch.requireRefusal(t, contractKey)
}

// A contract granted by a control service whose key the test chooses, for a real
// sequence to run into.
type contractDelegationOob struct {
	clientId   Id
	storedKey  func(requestIndex int) []byte
	grantCount atomic.Int64
}

func (self *contractDelegationOob) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	resultFrames := []*protocol.Frame{}
	for _, frame := range frames {
		message, err := FromFrame(frame)
		MessagePoolReturn(frame.MessageBytes)
		if err != nil {
			continue
		}
		createContract, ok := message.(*protocol.CreateContract)
		if !ok {
			continue
		}
		index := int(self.grantCount.Add(1)) - 1
		storedBytes, err := proto.Marshal(&protocol.StoredContract{
			ContractId:                 NewId().Bytes(),
			TransferByteCount:          createContract.TransferByteCount,
			SourceId:                   self.clientId.Bytes(),
			DestinationId:              createContract.DestinationId,
			DestinationClientPublicKey: self.storedKey(index),
		})
		if err != nil {
			continue
		}
		resultBytes, err := proto.Marshal(&protocol.CreateContractResult{Contract: &protocol.Contract{
			StoredContractBytes: storedBytes,
			ProvideMode:         protocol.ProvideMode_Network,
		}})
		if err != nil {
			continue
		}
		resultFrames = append(resultFrames, &protocol.Frame{MessageType: protocol.MessageType_TransferCreateContractResult, MessageBytes: resultBytes})
	}
	callback(resultFrames, nil)
}

// A client with a Required session to each peer it sends to, a contract service,
// and a route that shows everything it wrote.
func newContractDelegationSendClient(t *testing.T, oob *contractDelegationOob) (*Client, chan []byte) {
	t.Helper()
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithCancel(context.Background())
	settings := contractRetryTestSettings()
	settings.EncryptionSettings.Mode = EncryptionModeRequired
	settings.SendBufferSettings.PrewarmOpeningContract = false
	settings.SendBufferSettings.CreateContractRetryJitter = 0
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	client := NewClient(ctx, oob.clientId, oob, settings)
	route := make(chan []byte, 1024)
	client.RouteManager().UpdateTransport(newDataGatewayTransport(), []Route{route})
	t.Cleanup(func() {
		cancel()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := client.CloseAndWait(closeCtx); err != nil {
			t.Errorf("close delegation client: %v", err)
		}
		for len(route) != 0 {
			MessagePoolReturn(<-route)
		}
	})
	return client, route
}

// The key a live session to `peerId` holds, nil for none.
func contractDelegationSessionKey(client *Client, peerId Id) ed25519.PublicKey {
	for _, session := range client.encryptionSessionManager.sessionsForPeer(peerId) {
		if key := session.PeerClientPublicKey(); key != nil {
			return key
		}
	}
	return nil
}

// Sends one message on a goroutine, since a Required send waits at its gate for
// a session that nothing here can finish. The returned function joins it.
func startContractDelegationSend(t *testing.T, client *Client, destinationId Id) func() {
	t.Helper()
	frame, err := ToFrame(&protocol.SimpleMessage{Content: "bridged"}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if success, _ := client.SendWithTimeoutDetailed(frame, destinationId, func(error) {}, 5*time.Second); !success {
			MessagePoolReturn(frame.MessageBytes)
		}
	}()
	return func() { <-done }
}

// A substituted key at a real send: the sequence never sees the contract, so its
// session never learns the key, no byte goes to the destination, the contract
// closes unused and the status fires.
func TestContractDestinationCheckRefusalSendsNothing(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		goodKey, substituted := contractDelegationTestKey(t), contractDelegationTestKey(t)
		oob := &contractDelegationOob{
			clientId:  NewId(),
			storedKey: func(int) []byte { return substituted },
		}
		client, route := newContractDelegationSendClient(t, oob)
		manager := client.ContractManager()
		watch := watchContractDelegation(t, client)
		check := &contractDelegationCheck{goodKey: goodKey}
		manager.SetContractDestinationCheck(check.check)

		destinationId := NewId()
		join := startContractDelegationSend(t, client, destinationId)
		synctest.Wait()
		if oob.grantCount.Load() == 0 || check.callCount() == 0 {
			t.Fatalf("no contract reached the check: grants=%d calls=%d", oob.grantCount.Load(), check.callCount())
		}
		if key := contractDelegationSessionKey(client, destinationId); key != nil {
			t.Errorf("the session learned a key: %x", key)
		}
		join()
		synctest.Wait()

		for len(route) != 0 {
			wire := <-route
			var transferFrame protocol.TransferFrame
			if err := ProtoUnmarshal(wire, &transferFrame); err != nil {
				t.Error(err)
			}
			MessagePoolReturn(wire)
			if path, err := TransferPathFromProtobuf(transferFrame.TransferPath); err == nil && path.DestinationId == destinationId {
				t.Errorf("a frame reached the destination: %v", path)
			}
		}
		if int64(watch.closeCount()) != oob.grantCount.Load() {
			t.Errorf("%d closes for %d grants", watch.closeCount(), oob.grantCount.Load())
		}
		watch.stateLock.Lock()
		for _, report := range watch.closes {
			if report.AckedByteCount != 0 || report.UnackedByteCount != 0 {
				t.Errorf("a refused contract spent bytes: %v", report)
			}
		}
		watch.stateLock.Unlock()
		statuses := watch.dispatchedStatuses()
		if int64(len(statuses)) != oob.grantCount.Load() {
			t.Errorf("%d statuses for %d grants", len(statuses), oob.grantCount.Load())
		}
		for _, status := range statuses {
			if status.Error == nil || *status.Error != protocol.ContractError_Trust {
				t.Errorf("status %+v", status)
			}
		}
	})
}

// The good key reaches the session, and only once the check has answered for its
// contract: a session that held it sooner would have been told by another path.
func TestContractDestinationCheckRunsBeforeTheSessionLearnsTheKey(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		goodKey := contractDelegationTestKey(t)
		oob := &contractDelegationOob{
			clientId:  NewId(),
			storedKey: func(int) []byte { return goodKey },
		}
		client, _ := newContractDelegationSendClient(t, oob)
		manager := client.ContractManager()

		destinationId := NewId()
		sessionKeysSeen := [][]byte{}
		var stateLock sync.Mutex
		manager.SetContractDestinationCheck(func(id Id, key []byte) bool {
			stateLock.Lock()
			defer stateLock.Unlock()
			sessionKeysSeen = append(sessionKeysSeen, contractDelegationSessionKey(client, id))
			return bytes.Equal(key, goodKey)
		})

		join := startContractDelegationSend(t, client, destinationId)
		synctest.Wait()
		if key := contractDelegationSessionKey(client, destinationId); !bytes.Equal(key, goodKey) {
			t.Errorf("session key %x, want %x", key, goodKey)
		}
		func() {
			stateLock.Lock()
			defer stateLock.Unlock()
			if len(sessionKeysSeen) < 2 {
				t.Fatalf("the check ran %d times, want at the queue and at the take", len(sessionKeysSeen))
			}
			for index, key := range sessionKeysSeen[:2] {
				if key != nil {
					t.Errorf("call %d found the session already holding %x", index, key)
				}
			}
		}()
		join()
	})
}

// A contract queued before the check was installed meets it at a sequence's real
// take: the sequence asks again, and only the replacement's key reaches the session.
func TestContractDestinationCheckRefusesAQueuedContractAtARealTake(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		goodKey, substituted := contractDelegationTestKey(t), contractDelegationTestKey(t)
		oob := &contractDelegationOob{
			clientId:  NewId(),
			storedKey: func(int) []byte { return goodKey },
		}
		client, _ := newContractDelegationSendClient(t, oob)
		manager := client.ContractManager()
		watch := watchContractDelegation(t, client)

		destinationId := NewId()
		contractKey := ContractKey{Destination: DestinationId(destinationId)}
		result, refusedId := contractDelegationResult(t, client, destinationId, substituted, nil)
		if err := manager.HandleControlFrame(contractKey, result); err != nil {
			t.Fatal(err)
		}
		manager.SetContractDestinationCheck((&contractDelegationCheck{goodKey: goodKey}).check)

		join := startContractDelegationSend(t, client, destinationId)
		synctest.Wait()
		watch.requireUnusedClose(t, refusedId)
		if key := contractDelegationSessionKey(client, destinationId); !bytes.Equal(key, goodKey) {
			t.Errorf("session key %x, want the replacement's %x", key, goodKey)
		}
		join()
	})
}

// A session with no contract key asks the fetcher, and the platform answers
// there as it does in a contract. A first-set-wins key from that path would stand
// against every contract the check admits, so it answers to the check too.
func TestContractDestinationCheckHoldsTheFetchedKey(t *testing.T) {
	goodKey, substituted := contractDelegationTestKey(t), contractDelegationTestKey(t)
	cases := []struct {
		name    string
		fetched []byte
		check   bool
		wantKey []byte
	}{
		{name: "no check commits the fetch", fetched: substituted, wantKey: substituted},
		{name: "a substituted fetch is refused", fetched: substituted, check: true},
		{name: "the expected fetch commits", fetched: goodKey, check: true, wantKey: goodKey},
	}
	for _, c := range cases {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			settings := closeWaitClientSettings()
			settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
			settings.EncryptionSettings.Mode = EncryptionModeOpportunistic
			settings.EncryptionSettings.NewPeerClientPublicKeyFetcher = func(Id) func(context.Context) ([]byte, error) {
				return func(context.Context) ([]byte, error) {
					return c.fetched, nil
				}
			}
			client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
			t.Cleanup(func() {
				cancel()
				closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer closeCancel()
				if err := client.CloseAndWait(closeCtx); err != nil {
					t.Errorf("close fetch client: %v", err)
				}
			})
			check := &contractDelegationCheck{goodKey: goodKey}
			if c.check {
				client.ContractManager().SetContractDestinationCheck(check.check)
			}

			peerId := NewId()
			session := client.encryptionSessionManager.Acquire(peerId, sequenceTlsRoleClient, false)
			defer session.Release()
			session.maybeFetchPeerClientPublicKeyForIdentity()
			synctest.Wait()

			if key := session.PeerClientPublicKey(); !bytes.Equal(key, c.wantKey) {
				t.Errorf("%s: session key %x, want %x", c.name, key, c.wantKey)
			}
			if c.check && check.callCount() != 1 {
				t.Errorf("%s: %d check calls", c.name, check.callCount())
			}
		})
	}
}

// The `exp` claim reads as a time, and a token that lacks it or carries nonsense
// still parses as it always did: expiry here bounds, it does not gate.
func TestParseByJwtUnverifiedExpireTime(t *testing.T) {
	signed := func(claims gojwt.MapClaims) string {
		token, err := gojwt.NewWithClaims(gojwt.SigningMethodHS256, claims).SignedString([]byte("synthetic signing key"))
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	clientId := NewId()
	expireTime := time.Unix(1893456000, 0)

	byJwt, err := ParseByJwtUnverified(signed(gojwt.MapClaims{"client_id": clientId.String(), "exp": expireTime.Unix()}))
	if err != nil {
		t.Fatal(err)
	}
	if !byJwt.ExpireTime.Equal(expireTime) || byJwt.ClientId != clientId {
		t.Errorf("parsed %+v, want expiry %s", byJwt, expireTime)
	}

	for name, claims := range map[string]gojwt.MapClaims{
		"absent":    {"client_id": clientId.String()},
		"zero":      {"client_id": clientId.String(), "exp": 0},
		"malformed": {"client_id": clientId.String(), "exp": "tomorrow"},
	} {
		byJwt, err := ParseByJwtUnverified(signed(claims))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !byJwt.ExpireTime.IsZero() || byJwt.ClientId != clientId {
			t.Errorf("%s: parsed %+v, want no expiry", name, byJwt)
		}
	}
}
