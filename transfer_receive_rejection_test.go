// Receive rejection tests ensure deterministic contract failures cannot create
// resend-paced sequence recreation and logging loops.
package connect

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// TestReceiveSequenceMalformedContractRejectsRetransmits verifies that every
// deterministic contract decode failure asks the owning buffer for a tombstone.
func TestReceiveSequenceMalformedContractRejectsRetransmits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultClientSettings()
	settings.Log = NewNoopLogger()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer client.Cancel()

	source := SourceId(NewId())
	sequence := NewReceiveSequence(
		ctx,
		client,
		source,
		NewId(),
		sequenceTlsRoleServer,
		false,
		settings.ReceiveBufferSettings,
	)
	defer sequence.Close()
	sequence.peerAudit = NewSequencePeerAudit(
		client,
		source,
		settings.ReceiveBufferSettings.MaxPeerAuditDuration,
	)
	err := sequence.registerContracts(&receiveItem{
		contractFrame: &protocol.Frame{
			MessageType:  protocol.MessageType_TransferContract,
			MessageBytes: []byte{0xff},
		},
	})
	if err == nil {
		t.Fatal("malformed contract was accepted")
	}
	if !sequence.rejectRetransmits {
		t.Fatal("malformed contract did not mark its sequence for rejection")
	}
}

// Repeats of the exact rejected sequence are discarded before constructing
// another receive worker. A rejection cannot judge a different generation.
func TestReceiveBufferRejectedSequenceDropsRetransmits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &Client{
		ctx: ctx,
		log: NewNoopLogger(),
	}
	buffer := NewReceiveBuffer(ctx, client, DefaultReceiveBufferSettings())
	source := SourceId(NewId())
	rejectedSequenceId := NewId()
	headKey := receiveSequenceHeadKey{
		Source:         source,
		EncryptionRole: sequenceTlsRoleServer,
	}
	buffer.mutex.Lock()
	buffer.rejectReceiveSequenceWithLock(headKey, rejectedSequenceId)
	buffer.mutex.Unlock()

	for i := 0; i < 2; i++ {
		success, err := buffer.Pack(
			&ReceivePack{
				Source:         source,
				SequenceId:     rejectedSequenceId,
				EncryptionRole: sequenceTlsRoleServer,
			},
			0,
		)
		if err != nil || !success {
			t.Fatalf(
				"rejected retransmit result = (%t, %v), want silent successful drop",
				success,
				err,
			)
		}
	}
	buffer.mutex.Lock()
	receiveSequenceCount := len(buffer.receiveSequences)
	buffer.mutex.Unlock()
	if receiveSequenceCount != 0 {
		t.Fatalf("rejected retransmits created %d receive sequences", receiveSequenceCount)
	}
}

// A maximum sequence id with an invalid contract must reject its own retries,
// then allow the real peer's lower generation through the ordinary Pack path.
func TestReceiveBufferRejectedMaximumSequenceDoesNotPoisonPeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var maxSequenceId Id
	for i := range maxSequenceId {
		maxSequenceId[i] = 0xff
	}
	legitSequenceId := Id{15: 1}
	rejected := make(chan struct{})
	release := make(chan struct{})
	settings := DefaultClientSettings()
	settings.Log = NewNoopLogger()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.ReceiveBufferSettings.afterRunReceiveSequenceForTest = func(id receiveSequenceId) {
		if id.SequenceId == maxSequenceId {
			close(rejected)
		}
	}
	settings.ReceiveBufferSettings.beforeRunReceiveSequenceForTest = func(id receiveSequenceId) {
		if id.SequenceId == legitSequenceId {
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer func() {
		cancel()
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Errorf("client cleanup: %v", err)
		}
	}()
	source := SourceId(NewId())
	contractFrame, err := ToFrame(&protocol.Contract{
		StoredContractBytes: []byte{1}, StoredContractHmac: []byte{2},
		ProvideMode: protocol.ProvideMode_Stream,
	}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	attack := &ReceivePack{
		Source: source, SequenceId: maxSequenceId,
		EncryptionRole: sequenceTlsRoleServer, Unwrapped: true,
		Pack: &protocol.Pack{
			MessageId: NewId().Bytes(), SequenceId: maxSequenceId.Bytes(),
			Head: true, ContractFrame: contractFrame,
		},
	}
	if ok, err := client.receiveBuffer.Pack(attack, time.Second); err != nil || !ok {
		attack.messagePoolReturn()
		t.Fatalf("invalid opening was not admitted: %t, %v", ok, err)
	}
	select {
	case <-rejected:
	case <-ctx.Done():
		t.Fatal("invalid opening did not finish rejection")
	}
	headKey := receiveSequenceHeadKey{Source: source, EncryptionRole: sequenceTlsRoleServer}
	client.receiveBuffer.mutex.Lock()
	tombstone, exists := client.receiveBuffer.rejectedReceiveSequenceIds[headKey]
	client.receiveBuffer.mutex.Unlock()
	if !exists || tombstone != maxSequenceId {
		t.Fatal("invalid opening did not install the maximum rejection")
	}
	client.ContractManager().AddNoContractPeer(source.SourceId)
	delivered := make(chan struct{}, 1)
	frame := requiredGateFrame(t, "legitimate lower sequence")
	legit := &ReceivePack{
		Source: source, SequenceId: legitSequenceId,
		EncryptionRole: sequenceTlsRoleServer, Unwrapped: true,
		MessageByteCount: MessageByteCount([]*protocol.Frame{frame}),
		Pack: &protocol.Pack{
			MessageId: NewId().Bytes(), SequenceId: legitSequenceId.Bytes(),
			Head: true, Nack: true, Frames: []*protocol.Frame{frame},
		},
		ReceiveCallback: func(TransferPath, []*protocol.Frame, Peer) { delivered <- struct{}{} },
	}
	if ok, err := client.receiveBuffer.Pack(legit, time.Second); err != nil || !ok {
		legit.messagePoolReturn()
		t.Fatalf("legitimate opening was not admitted: %t, %v", ok, err)
	}
	// The barrier keeps an admitted worker indexed until this assertion; a
	// silent rejection therefore fails immediately without a negative timeout.
	client.receiveBuffer.mutex.Lock()
	head, admitted := client.receiveBuffer.headReceiveSequenceIds[headKey]
	client.receiveBuffer.mutex.Unlock()
	if !admitted || head.SequenceId != legitSequenceId {
		t.Fatal("maximum rejected sequence permanently blocked the legitimate lower generation")
	}
	close(release)
	select {
	case <-delivered:
	case <-ctx.Done():
		t.Fatal("admitted lower generation did not deliver application data")
	}
}

// The independently funded opening path must apply the same exact rejection
// boundary before an ordinary worker can verify the new opening's contract.
func TestReceiveFreshOpeningRejectsOnlyExactFailedSequence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := TransferPath{SourceId: NewId(), DestinationId: NewId()}
	var maxSequenceId Id
	for i := range maxSequenceId {
		maxSequenceId[i] = 0xff
	}
	root := NewTransferMemoryBudget(32 * 1024)
	rx := NewTransferMemoryBudgetWithParent(16*1024, root)
	recovery := NewTransferMemoryBudgetWithParent(16*1024, root)
	settings := DefaultReceiveBufferSettings()
	settings.ReceiveQueueBudget = rx
	settings.ReceiveHeadRecoveryBudget = recovery
	buffer := NewReceiveBuffer(ctx, &Client{ctx: ctx, clientId: path.DestinationId, log: NewNoopLogger()}, settings)
	headKey := receiveSequenceHeadKey{Source: path.SourceMask(), EncryptionRole: sequenceTlsRoleServer}
	buffer.mutex.Lock()
	buffer.rejectReceiveSequenceWithLock(headKey, maxSequenceId)
	buffer.mutex.Unlock()
	for _, sequenceId := range []Id{maxSequenceId, {15: 1}} {
		pack := sendPackFrame{
			messageId: NewId(), sequenceId: sequenceId, head: true,
			contractFrame: &protocol.Frame{
				MessageType: protocol.MessageType_TransferContract, MessageBytes: []byte{0},
			},
		}
		wire := receiveMemoryScanTestWire(path, pack.appendPack(nil), false)
		scan, status := scanReceiveMemoryPack(wire, path)
		if status != receiveMemoryPackScanReady || !receiveConsumerOpeningHeadScan(scan) {
			t.Fatal("opening fixture did not reach the real scanner admission boundary")
		}
		owner := buffer.takeReceiveFreshOpeningOwnerInSession(scan, sequenceTlsRoleServer, false, false)
		if owner != nil {
			owner.receivePack.messagePoolReturn()
		}
		if (owner != nil) != (sequenceId != maxSequenceId) {
			t.Fatalf("opening admission for %s did not respect exact failed-generation rejection", sequenceId)
		}
	}
	for _, budget := range []*TransferMemoryBudget{rx, recovery, root} {
		assertRetainedBudgetBalance(t, budget)
	}
}

// TestReceiveBufferNewerSequenceClearsRejection verifies that a peer can
// recover by reforming with a genuinely newer sequence and fresh contract.
func TestReceiveBufferNewerSequenceClearsRejection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buffer := NewReceiveBuffer(
		ctx,
		&Client{
			ctx: ctx,
			log: NewNoopLogger(),
		},
		DefaultReceiveBufferSettings(),
	)
	headKey := receiveSequenceHeadKey{
		Source:         SourceId(NewId()),
		EncryptionRole: sequenceTlsRoleClient,
	}
	rejectedSequenceId := NewId()
	newSequenceId := NewId()

	buffer.mutex.Lock()
	buffer.rejectReceiveSequenceWithLock(headKey, rejectedSequenceId)
	rejected := buffer.rejectReceiveSequenceRetransmitWithLock(
		headKey,
		newSequenceId,
	)
	_, remains := buffer.rejectedReceiveSequenceIds[headKey]
	buffer.mutex.Unlock()
	if rejected {
		t.Fatal("newer sequence was rejected")
	}
	if remains {
		t.Fatal("newer sequence did not clear the old rejection")
	}
}

// TestReceiveBufferRejectedSequenceTombstonesAreBounded verifies predictable
// memory even if many distinct peers present invalid contracts.
func TestReceiveBufferRejectedSequenceTombstonesAreBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buffer := NewReceiveBuffer(
		ctx,
		&Client{
			ctx: ctx,
			log: NewNoopLogger(),
		},
		DefaultReceiveBufferSettings(),
	)
	if buffer.rejectedReceiveSequenceIds != nil ||
		buffer.rejectedReceiveSequenceOrder != nil {
		t.Fatal("unused rejection tombstones were allocated eagerly")
	}
	firstHeadKey := receiveSequenceHeadKey{
		Source:         SourceId(NewId()),
		EncryptionRole: sequenceTlsRoleServer,
	}
	buffer.mutex.Lock()
	buffer.rejectReceiveSequenceWithLock(firstHeadKey, NewId())
	if cap(buffer.rejectedReceiveSequenceOrder) !=
		rejectedReceiveSequenceCapacity {
		t.Fatalf(
			"rejected sequence order capacity = %d, want %d",
			cap(buffer.rejectedReceiveSequenceOrder),
			rejectedReceiveSequenceCapacity,
		)
	}
	for i := 1; i <= rejectedReceiveSequenceCapacity; i++ {
		buffer.rejectReceiveSequenceWithLock(
			receiveSequenceHeadKey{
				Source:         SourceId(NewId()),
				EncryptionRole: sequenceTlsRoleServer,
			},
			NewId(),
		)
	}
	tombstoneCount := len(buffer.rejectedReceiveSequenceIds)
	orderCount := len(buffer.rejectedReceiveSequenceOrder)
	_, firstRemains := buffer.rejectedReceiveSequenceIds[firstHeadKey]
	buffer.mutex.Unlock()

	if tombstoneCount != rejectedReceiveSequenceCapacity {
		t.Fatalf(
			"rejected sequence tombstones = %d, want %d",
			tombstoneCount,
			rejectedReceiveSequenceCapacity,
		)
	}
	if orderCount != rejectedReceiveSequenceCapacity {
		t.Fatalf(
			"rejected sequence order = %d, want %d",
			orderCount,
			rejectedReceiveSequenceCapacity,
		)
	}
	if firstRemains {
		t.Fatal("oldest rejected sequence was not evicted at capacity")
	}
}

// TestReceiveBufferInvalidContractTombstonesItsSequence exercises the complete
// receive-worker cleanup path that records the rejection.
func TestReceiveBufferInvalidContractTombstonesItsSequence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultClientSettings()
	settings.Log = NewNoopLogger()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer client.Cancel()

	source := SourceId(NewId())
	sequenceId := NewId()
	contractFrame, err := ToFrame(
		&protocol.Contract{
			StoredContractBytes: []byte{1},
			StoredContractHmac:  []byte{2},
			ProvideMode:         protocol.ProvideMode_Stream,
		},
		DefaultProtocolVersion,
	)
	if err != nil {
		t.Fatalf("encode invalid contract: %v", err)
	}
	receivePack := &ReceivePack{
		Source:     source,
		SequenceId: sequenceId,
		Pack: &protocol.Pack{
			MessageId:      NewId().Bytes(),
			SequenceId:     sequenceId.Bytes(),
			SequenceNumber: 0,
			Head:           true,
			ContractFrame:  contractFrame,
		},
		ReceiveCallback: func(
			source TransferPath,
			frames []*protocol.Frame,
			peer Peer,
		) {
			t.Error("invalid contract reached receive callback")
		},
		Unwrapped:      true,
		EncryptionRole: sequenceTlsRoleServer,
	}
	success, err := client.receiveBuffer.Pack(receivePack, time.Second)
	if err != nil || !success {
		t.Fatalf("queue invalid contract = (%t, %v), want worker admission", success, err)
	}

	headKey := receiveSequenceHeadKey{
		Source:         source,
		EncryptionRole: sequenceTlsRoleServer,
	}
	deadline := time.Now().Add(time.Second)
	for {
		client.receiveBuffer.mutex.Lock()
		rejectedSequenceId, rejected :=
			client.receiveBuffer.rejectedReceiveSequenceIds[headKey]
		client.receiveBuffer.mutex.Unlock()
		if rejected && rejectedSequenceId == sequenceId {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("invalid contract sequence was not tombstoned")
		}
		time.Sleep(time.Millisecond)
	}

	success, err = client.receiveBuffer.Pack(
		&ReceivePack{
			Source:         source,
			SequenceId:     sequenceId,
			EncryptionRole: sequenceTlsRoleServer,
		},
		0,
	)
	if err != nil || !success {
		t.Fatalf(
			"invalid-contract retransmit result = (%t, %v), want silent drop",
			success,
			err,
		)
	}
}
