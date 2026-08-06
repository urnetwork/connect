package connect

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// TestSendSequenceCloseCallbackBackpressureIsDestinationLocal verifies that an
// intentionally blocking acknowledgement callback retains backpressure only
// for the sequence it belongs to. Sequence cleanup must publish map removal
// before invoking the callback and must not hold SendBuffer.mutex while the
// callback is blocked.
func TestSendSequenceCloseCallbackBackpressureIsDestinationLocal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sendBuffer := &SendBuffer{
		ctx:                        ctx,
		sendSequences:              map[sendSequenceId]*SendSequence{},
		wireSendSequences:          map[sendSequenceWireId]*SendSequence{},
		sendSequencesByDestination: map[TransferPath]map[*SendSequence]bool{},
		sendSequenceDestinations:   map[*SendSequence]map[TransferPath]bool{},
	}
	sequenceCtx, sequenceCancel := context.WithCancel(ctx)
	destination := DestinationId(NewId())
	sequence := &SendSequence{
		ctx:         sequenceCtx,
		cancel:      sequenceCancel,
		destination: destination,
		packs:       make(chan *SendPack, 1),
		acks:        make(chan *protocol.Ack, 1),
	}
	id := sequence.id()
	wireId := id.wireId()
	sendBuffer.sendSequences[id] = sequence
	sendBuffer.wireSendSequences[wireId] = sequence

	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseCallback)
		})
	}
	defer release()

	messageBytes := MessagePoolGet(1)
	messageBytes[0] = 1
	sequence.packs <- &SendPack{
		Frame: &protocol.Frame{
			MessageType:  protocol.MessageType_TransferExchangeSignals,
			MessageBytes: messageBytes,
		},
		AckCallback: func(error) {
			close(callbackStarted)
			<-releaseCallback
		},
		Ctx: ctx,
	}

	closeDone := make(chan struct{})
	go func() {
		defer close(closeDone)
		sendBuffer.closeSendSequence(id, wireId, sequence)
	}()

	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		t.Fatal("sequence close did not reach its queued acknowledgement callback")
	}
	select {
	case <-closeDone:
		t.Fatal("sequence close escaped an intentionally blocked callback")
	default:
	}

	lookupDone := make(chan struct{})
	go func() {
		defer close(lookupDone)
		if found := sendBuffer.lookupSendSequence(
			sendSequenceId{Destination: DestinationId(NewId())},
			nil,
		); found != nil {
			t.Error("unrelated destination unexpectedly found a send sequence")
		}
	}()
	select {
	case <-lookupDone:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("blocked acknowledgement callback retained the buffer-wide send lock")
	}

	release()
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("sequence close did not finish after callback backpressure released")
	}
}

// TestReceiveSequenceReplacementBackpressureIsSourceLocal models a receive
// worker parked in an intentional receive callback. A newer generation for the
// same source must wait for that callback to finish to preserve ordering, but
// the wait must occur outside ReceiveBuffer.mutex so unrelated sources remain
// observable and admissible.
func TestReceiveSequenceReplacementBackpressureIsSourceLocal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultClientSettings()
	settings.Log = NewNoopLogger()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer client.Cancel()

	receiveBuffer := client.receiveBuffer
	sourceId := NewId()
	source := SourceId(sourceId)
	client.ContractManager().AddNoContractPeer(sourceId)
	oldSequenceId := NewId()
	newSequenceId := NewId()
	if !oldSequenceId.LessThan(newSequenceId) {
		t.Fatal("NewId did not preserve sequence ordering")
	}

	oldCtx, oldCancel := context.WithCancel(ctx)
	oldSequence := &ReceiveSequence{
		ctx:    oldCtx,
		cancel: oldCancel,
		exit:   make(chan struct{}),
	}
	oldId := receiveSequenceId{
		Source:         source,
		SequenceId:     oldSequenceId,
		EncryptionRole: sequenceTlsRoleServer,
	}
	headKey := receiveSequenceHeadKey{
		Source:         source,
		EncryptionRole: sequenceTlsRoleServer,
	}
	receiveBuffer.mutex.Lock()
	receiveBuffer.receiveSequences[oldId] = oldSequence
	receiveBuffer.headReceiveSequenceIds[headKey] = oldId
	receiveBuffer.mutex.Unlock()

	transferFrameBytes := MessagePoolGet(1)
	transferFrameBytes[0] = 1
	replacementResult := make(chan struct {
		success bool
		err     error
	}, 1)
	go func() {
		success, packErr := receiveBuffer.Pack(
			&ReceivePack{
				Source:     source,
				SequenceId: newSequenceId,
				Pack: &protocol.Pack{
					MessageId:      NewId().Bytes(),
					SequenceId:     newSequenceId.Bytes(),
					SequenceNumber: 0,
					Head:           true,
					Nack:           true,
				},
				ReceiveCallback:    func(TransferPath, []*protocol.Frame, Peer) {},
				TransferFrameBytes: transferFrameBytes,
				Ctx:                ctx,
				Unwrapped:          true,
				EncryptionRole:     sequenceTlsRoleServer,
			},
			time.Second,
		)
		replacementResult <- struct {
			success bool
			err     error
		}{success: success, err: packErr}
	}()

	select {
	case <-oldCtx.Done():
		// The replacement reached the old worker and canceled it.
	case <-time.After(time.Second):
		t.Fatal("new generation did not retire the old receive sequence")
	}
	select {
	case result := <-replacementResult:
		t.Fatalf(
			"same-source replacement bypassed callback ordering: (%t, %v)",
			result.success,
			result.err,
		)
	default:
	}

	unrelatedDone := make(chan struct{})
	go func() {
		defer close(unrelatedDone)
		receiveBuffer.ReceiveQueueSizeAndMessageTypes(
			SourceId(NewId()),
			NewId(),
		)
	}()
	select {
	case <-unrelatedDone:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("same-source callback backpressure retained the buffer-wide receive lock")
	}

	close(oldSequence.exit)
	select {
	case result := <-replacementResult:
		if result.err != nil || !result.success {
			t.Fatalf(
				"replacement result after callback release = (%t, %v)",
				result.success,
				result.err,
			)
		}
	case <-time.After(time.Second):
		t.Fatal("same-source replacement did not resume after callback release")
	}
}
