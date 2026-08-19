package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func closeTransferGroupTestClient(t *testing.T, client *Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.CloseAndWait(ctx); err != nil {
		t.Errorf("close logical-group test client: %v", err)
	}
}

// transferGroupTestFrames returns independently owned raw frames so each
// chunk disposition can be verified with message-pool witnesses.
func transferGroupTestFrames(t *testing.T, count int, byteCount int) ([]*protocol.Frame, [][]byte) {
	t.Helper()
	frames := make([]*protocol.Frame, 0, count)
	witnesses := make([][]byte, 0, count)
	for index := range count {
		messageBytes := MessagePoolGet(byteCount)
		for byteIndex := range messageBytes {
			messageBytes[byteIndex] = byte(index + 1)
		}
		pooled, _ := MessagePoolCheck(messageBytes)
		if !pooled {
			for _, frame := range frames {
				MessagePoolReturn(frame.MessageBytes)
			}
			for _, witness := range witnesses {
				MessagePoolReturn(witness)
			}
			MessagePoolReturn(messageBytes)
			t.Fatalf("group frame byte count %d did not use the message pool", byteCount)
		}
		frames = append(frames, &protocol.Frame{
			MessageType:  protocol.MessageType_TransferExchangeSignals,
			MessageBytes: messageBytes,
			Raw:          true,
		})
		witnesses = append(witnesses, MessagePoolShareReadOnly(messageBytes))
	}
	return frames, witnesses
}

// releaseTransferGroupTestWitnesses proves every successfully admitted source
// frame has reached a terminal owner before the test releases its witness.
func releaseTransferGroupTestWitnesses(t *testing.T, frames []*protocol.Frame, witnesses [][]byte) {
	t.Helper()
	for index, witness := range witnesses {
		if MessagePoolReturn(witness) {
			continue
		}
		// Release a leaked owner so a failing test does not contaminate later
		// message-pool accounting in the same process.
		MessagePoolReturn(frames[index].MessageBytes)
		t.Fatalf("logical group retained frame %d after terminal completion", index)
	}
}

// One logical group is admitted once, split only at Transfer's wire bounds,
// and produces one Ack, NoAck, and lifecycle completion after every chunk has
// reached its initial writer disposition.
func TestSendMultiHopGroupChunksOneLogicalAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	destinationId := NewId()
	noAckObserver, noAckEvents := noAckObserverTestEvents()
	lifecycleObserver, lifecycleEvents := sendPackLifecycleTestObserver(destinationId)
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.SendBufferSettings.NoAckSendObserver = noAckObserver
	settings.SendBufferSettings.SendPackLifecycleObserver = lifecycleObserver
	var sequenceCreateCount atomic.Int32
	var groupCompletionCreateCount atomic.Int32
	var groupCompletionChunkCount atomic.Int32
	settings.SendBufferSettings.beforeCreateSendSequenceForTest = func(id sendSequenceId) {
		if id.Destination == destinationId {
			sequenceCreateCount.Add(1)
		}
	}
	settings.SendBufferSettings.afterCreateSendGroupCompletionForTest = func(
		id sendSequenceId,
		chunkCount int,
	) {
		if id.Destination == destinationId {
			groupCompletionChunkCount.Store(int32(chunkCount))
			groupCompletionCreateCount.Add(1)
		}
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer closeTransferGroupTestClient(t, client)
	client.ContractManager().AddNoContractPeer(destinationId)
	route := make(chan []byte, 8)
	client.RouteManager().UpdateTransport(
		NewSendClientTransport(DestinationId(destinationId)),
		[]Route{route},
	)

	frames, witnesses := transferGroupTestFrames(t, 5, 64)
	result := make(chan error, 2)
	var resultCount atomic.Int32
	destination := RequireMultiHopId(NewId(), destinationId)
	success, err := client.sendMultiHopGroupWithTimeoutDetailed(
		frames,
		destination,
		func(err error) {
			resultCount.Add(1)
			result <- err
		},
		time.Second,
		NoAck(),
	)
	if !success || err != nil {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
		for _, witness := range witnesses {
			MessagePoolReturn(witness)
		}
		t.Fatalf("logical group admission success=%t err=%v", success, err)
	}

	started := waitNoAckObservation(t, ctx, noAckEvents)
	if started.Phase != NoAckSendPhaseStarted {
		t.Fatalf("NoAck start=%+v", started)
	}
	lifecycleStarted := waitSendPackLifecycleObservation(t, ctx, lifecycleEvents)
	requireSendPackLifecycleObservation(
		t,
		lifecycleStarted,
		client.ClientId(),
		destinationId,
		lifecycleStarted.Token,
		SendPackLifecyclePhaseStarted,
		false,
		false,
	)

	// The terminal callback is a positive causality barrier: when it fires,
	// every chunk has completed its first route-write disposition. Queue length
	// is therefore an exact assertion, not a scheduler-dependent negative wait.
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("logical group completion: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("wait for logical group completion: %v", ctx.Err())
	}
	if count := resultCount.Load(); count != 1 {
		t.Fatalf("logical group completion count=%d, want 1", count)
	}
	if routeCount := len(route); routeCount != 3 {
		t.Fatalf("logical group wire chunk count=%d, want 3", routeCount)
	}
	if createCount := sequenceCreateCount.Load(); createCount != 1 {
		t.Fatalf("logical group sequence creation count=%d, want 1", createCount)
	}
	if createCount := groupCompletionCreateCount.Load(); createCount != 1 {
		t.Fatalf("logical group completion creation count=%d, want 1", createCount)
	}
	if chunkCount := groupCompletionChunkCount.Load(); chunkCount != 3 {
		t.Fatalf("logical group completion chunk count=%d, want 3", chunkCount)
	}

	wantFrameCounts := []int{2, 2, 1}
	nextFrameIndex := 0
	for chunkIndex, wantFrameCount := range wantFrameCounts {
		transferFrameBytes := <-route
		pack := decodeSendPackLifecycleWirePack(t, transferFrameBytes)
		if frameCount := len(pack.Frames); frameCount != wantFrameCount {
			MessagePoolReturn(transferFrameBytes)
			t.Fatalf(
				"logical group chunk %d frame count=%d, want %d",
				chunkIndex,
				frameCount,
				wantFrameCount,
			)
		}
		for _, frame := range pack.Frames {
			if len(frame.MessageBytes) != 64 || frame.MessageBytes[0] != byte(nextFrameIndex+1) {
				MessagePoolReturn(transferFrameBytes)
				t.Fatalf(
					"logical group chunk %d frame %d did not preserve order",
					chunkIndex,
					nextFrameIndex,
				)
			}
			nextFrameIndex += 1
		}
		MessagePoolReturn(transferFrameBytes)
	}

	completed := waitNoAckObservation(t, ctx, noAckEvents)
	if completed.Phase != NoAckSendPhaseCompleted ||
		completed.Token != started.Token || completed.Err != nil {
		t.Fatalf("NoAck completion=%+v, start=%+v", completed, started)
	}
	firstRoute := waitSendPackLifecycleObservation(t, ctx, lifecycleEvents)
	terminal := waitSendPackLifecycleObservation(t, ctx, lifecycleEvents)
	requireSendPackLifecycleObservation(
		t,
		firstRoute,
		client.ClientId(),
		destinationId,
		lifecycleStarted.Token,
		SendPackLifecyclePhaseFirstRouteWrite,
		false,
		false,
	)
	requireSendPackLifecycleObservation(
		t,
		terminal,
		client.ClientId(),
		destinationId,
		lifecycleStarted.Token,
		SendPackLifecyclePhaseTerminal,
		false,
		false,
	)
	if count := len(noAckEvents); count != 0 {
		t.Fatalf("logical group left %d extra NoAck observations", count)
	}
	requireNoSendPackLifecycleObservations(t, lifecycleEvents, "logical group")
	releaseTransferGroupTestWitnesses(t, frames, witnesses)
}

// A group that fits one wire Pack uses the original SendPack completion
// records directly. The terminal callback is an exact barrier proving the
// multi-chunk aggregator could no longer be constructed afterward.
func TestSendMultiHopOneChunkGroupBypassesCompletionAggregator(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	destinationId := NewId()
	var groupCompletionCreateCount atomic.Int32
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.SendBufferSettings.afterCreateSendGroupCompletionForTest = func(
		id sendSequenceId,
		chunkCount int,
	) {
		if id.Destination == destinationId {
			groupCompletionCreateCount.Add(1)
		}
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer closeTransferGroupTestClient(t, client)
	client.ContractManager().AddNoContractPeer(destinationId)
	route := make(chan []byte, 1)
	client.RouteManager().UpdateTransport(
		NewSendClientTransport(DestinationId(destinationId)),
		[]Route{route},
	)

	frames, witnesses := transferGroupTestFrames(t, 2, 64)
	result := make(chan error, 1)
	success, err := client.sendMultiHopGroupWithTimeoutDetailed(
		frames,
		RequireMultiHopId(NewId(), destinationId),
		func(err error) { result <- err },
		time.Second,
		NoAck(),
	)
	if !success || err != nil {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
		for _, witness := range witnesses {
			MessagePoolReturn(witness)
		}
		t.Fatalf("one-chunk group admission success=%t err=%v", success, err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("one-chunk group completion: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("wait for one-chunk group completion: %v", ctx.Err())
	}
	if routeCount := len(route); routeCount != 1 {
		t.Fatalf("one-chunk group wire Pack count=%d, want 1", routeCount)
	}
	if createCount := groupCompletionCreateCount.Load(); createCount != 0 {
		t.Fatalf("one-chunk group constructed %d completion aggregators", createCount)
	}
	MessagePoolReturn(<-route)
	releaseTransferGroupTestWitnesses(t, frames, witnesses)
}

// Public SendMultiWithTimeout remains an explicit one-Pack API. Logical-group
// chunking is opt-in through the internal multi-hop group entry point.
func TestSendMultiWithTimeoutPreservesOneWirePack(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	destinationId := NewId()
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer closeTransferGroupTestClient(t, client)
	client.ContractManager().AddNoContractPeer(destinationId)
	route := make(chan []byte, 2)
	client.RouteManager().UpdateTransport(
		NewSendClientTransport(DestinationId(destinationId)),
		[]Route{route},
	)

	frames, witnesses := transferGroupTestFrames(t, 5, 64)
	result := make(chan error, 1)
	if !client.SendMultiWithTimeout(
		frames,
		destinationId,
		func(err error) { result <- err },
		time.Second,
		NoAck(),
	) {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
		for _, witness := range witnesses {
			MessagePoolReturn(witness)
		}
		t.Fatal("explicit one-Pack send was not admitted")
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("explicit one-Pack completion: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("wait for explicit one-Pack completion: %v", ctx.Err())
	}
	if routeCount := len(route); routeCount != 1 {
		t.Fatalf("explicit SendMulti wire Pack count=%d, want 1", routeCount)
	}
	transferFrameBytes := <-route
	pack := decodeSendPackLifecycleWirePack(t, transferFrameBytes)
	if frameCount := len(pack.Frames); frameCount != 5 {
		MessagePoolReturn(transferFrameBytes)
		t.Fatalf("explicit SendMulti frame count=%d, want 5", frameCount)
	}
	MessagePoolReturn(transferFrameBytes)
	releaseTransferGroupTestWitnesses(t, frames, witnesses)
}

// Cumulative acknowledgement of an early chunk must not complete the logical
// group. The test hook runs synchronously after that exact send item is removed,
// proving any premature callback would already be observable at the barrier.
func TestSendMultiHopGroupAckWaitsForEveryChunk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	destinationId := NewId()
	firstChunkAcked := make(chan struct{})
	releaseFirstChunkAck := make(chan struct{})
	var firstChunkAckedOnce sync.Once
	var releaseFirstChunkAckOnce sync.Once
	releaseAck := func() {
		releaseFirstChunkAckOnce.Do(func() { close(releaseFirstChunkAck) })
	}
	defer releaseAck()
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.SendBufferSettings.afterAckSendItemForTest = func(id sendSequenceId, sequenceNumber uint64) {
		if id.Destination != destinationId || sequenceNumber != 0 {
			return
		}
		firstChunkAckedOnce.Do(func() { close(firstChunkAcked) })
		<-releaseFirstChunkAck
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer closeTransferGroupTestClient(t, client)
	client.ContractManager().AddNoContractPeer(destinationId)
	route := make(chan []byte, 8)
	client.RouteManager().UpdateTransport(
		NewSendClientTransport(DestinationId(destinationId)),
		[]Route{route},
	)

	frames, witnesses := transferGroupTestFrames(t, 5, 64)
	result := make(chan error, 2)
	var resultCount atomic.Int32
	destination := RequireMultiHopId(NewId(), destinationId)
	success, err := client.sendMultiHopGroupWithTimeoutDetailed(
		frames,
		destination,
		func(err error) {
			resultCount.Add(1)
			result <- err
		},
		time.Second,
	)
	if !success || err != nil {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
		for _, witness := range witnesses {
			MessagePoolReturn(witness)
		}
		t.Fatalf("reliable logical group admission success=%t err=%v", success, err)
	}

	packs := make([]*protocol.Pack, 0, 3)
	for range 3 {
		select {
		case transferFrameBytes := <-route:
			packs = append(packs, decodeSendPackLifecycleWirePack(t, transferFrameBytes))
			MessagePoolReturn(transferFrameBytes)
		case <-ctx.Done():
			t.Fatalf("wait for reliable logical-group chunk: %v", ctx.Err())
		}
	}
	acknowledgeSendPackLifecycleWirePack(t, client, destinationId, packs[0])
	select {
	case <-firstChunkAcked:
	case <-ctx.Done():
		t.Fatalf("wait for first chunk Ack barrier: %v", ctx.Err())
	}
	if completionCount := len(result); completionCount != 0 {
		t.Fatalf("first of three chunk Acks completed logical group %d times", completionCount)
	}
	releaseAck()
	acknowledgeSendPackLifecycleWirePack(t, client, destinationId, packs[2])
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("reliable logical group completion: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("wait for reliable logical group completion: %v", ctx.Err())
	}
	if count := resultCount.Load(); count != 1 {
		t.Fatalf("reliable logical group completion count=%d, want 1", count)
	}
	releaseTransferGroupTestWitnesses(t, frames, witnesses)
}

// A failure after the first chunk was materialized must dispose every later
// source frame while the already-written chunk converges on the same one-shot
// completion. The forced second contract update is an exact production seam.
func TestSendMultiHopGroupPartialMaterializationDisposesRemainder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	destinationId := NewId()
	var contractUpdateCount atomic.Int32
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.SendBufferSettings.forceContractFailureForTest = func(id sendSequenceId) bool {
		return id.Destination == destinationId && contractUpdateCount.Add(1) == 2
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer closeTransferGroupTestClient(t, client)
	client.ContractManager().AddNoContractPeer(destinationId)
	route := make(chan []byte, 8)
	client.RouteManager().UpdateTransport(
		NewSendClientTransport(DestinationId(destinationId)),
		[]Route{route},
	)

	frames, witnesses := transferGroupTestFrames(t, 5, 64)
	result := make(chan error, 2)
	var resultCount atomic.Int32
	destination := RequireMultiHopId(NewId(), destinationId)
	success, err := client.sendMultiHopGroupWithTimeoutDetailed(
		frames,
		destination,
		func(err error) {
			resultCount.Add(1)
			result <- err
		},
		time.Second,
		NoAck(),
	)
	if !success || err != nil {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
		for _, witness := range witnesses {
			MessagePoolReturn(witness)
		}
		t.Fatalf("partial logical group admission success=%t err=%v", success, err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("partial logical group completed without the forced error")
		}
	case <-ctx.Done():
		t.Fatalf("wait for partial logical group completion: %v", ctx.Err())
	}
	if count := resultCount.Load(); count != 1 {
		t.Fatalf("partial logical group completion count=%d, want 1", count)
	}
	if routeCount := len(route); routeCount != 1 {
		t.Fatalf("partial logical group wire chunk count=%d, want 1", routeCount)
	}
	MessagePoolReturn(<-route)
	releaseTransferGroupTestWitnesses(t, frames, witnesses)
}

// A group refused before SendSequence ownership leaves every source frame with
// the caller. The witness transitions prove Transfer neither returned nor kept
// an owner on the canceled admission path.
func TestSendMultiHopGroupRefusalRetainsWholeGroupOwnership(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer closeTransferGroupTestClient(t, client)
	destinationId := NewId()
	client.ContractManager().AddNoContractPeer(destinationId)

	frames, witnesses := transferGroupTestFrames(t, 5, 64)
	sendCtx, cancelSend := context.WithCancel(ctx)
	cancelSend()
	success, err := client.sendMultiHopGroupWithTimeoutDetailed(
		frames,
		RequireMultiHopId(NewId(), destinationId),
		nil,
		time.Second,
		Ctx(sendCtx),
	)
	if success || err == nil {
		t.Fatalf("canceled logical group admission success=%t err=%v", success, err)
	}
	for index, frame := range frames {
		if MessagePoolReturn(frame.MessageBytes) {
			t.Fatalf("Transfer returned caller-owned frame %d after refusal", index)
		}
		if !MessagePoolReturn(witnesses[index]) {
			t.Fatalf("caller-owned frame %d retained another owner after refusal", index)
		}
	}
}

// Closing after the first chunk materializes joins its reliable owner with the
// unsent cursor. The original callback must observe one terminal error only
// after all five frame owners are released.
func TestSendMultiHopGroupCloseJoinsMaterializedAndPendingChunks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	destinationId := NewId()
	sequenceEntered := make(chan struct{})
	releaseSequence := make(chan struct{})
	resendCapacityWait := make(chan struct{})
	releaseResendCapacityWait := make(chan struct{})
	var sequenceEnteredOnce sync.Once
	var releaseSequenceOnce sync.Once
	var resendCapacityWaitOnce sync.Once
	var releaseResendCapacityWaitOnce sync.Once
	release := func() {
		releaseSequenceOnce.Do(func() { close(releaseSequence) })
		releaseResendCapacityWaitOnce.Do(func() { close(releaseResendCapacityWait) })
	}
	defer release()
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.SendBufferSettings.SequenceBufferSize = 1
	settings.SendBufferSettings.ResendQueueMaxByteCount = 1
	settings.SendBufferSettings.beforeResendCapacityWaitForTest = func(id sendSequenceId) {
		if id.Destination == destinationId {
			resendCapacityWaitOnce.Do(func() { close(resendCapacityWait) })
			<-releaseResendCapacityWait
		}
	}
	settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
		if id.Destination != destinationId {
			return
		}
		sequenceEnteredOnce.Do(func() { close(sequenceEntered) })
		<-releaseSequence
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	client.ContractManager().AddNoContractPeer(destinationId)
	route := make(chan []byte, 1)
	client.RouteManager().UpdateTransport(
		NewSendClientTransport(DestinationId(destinationId)),
		[]Route{route},
	)

	frames, witnesses := transferGroupTestFrames(t, 5, 64)
	result := make(chan error, 2)
	var resultCount atomic.Int32
	success, err := client.sendMultiHopGroupWithTimeoutDetailed(
		frames,
		RequireMultiHopId(NewId(), destinationId),
		func(err error) {
			resultCount.Add(1)
			result <- err
		},
		time.Second,
	)
	if !success || err != nil {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
		for _, witness := range witnesses {
			MessagePoolReturn(witness)
		}
		t.Fatalf("close logical group admission success=%t err=%v", success, err)
	}
	select {
	case <-sequenceEntered:
	case <-ctx.Done():
		t.Fatalf("wait for held logical-group sequence: %v", ctx.Err())
	}
	releaseSequenceOnce.Do(func() { close(releaseSequence) })
	select {
	case transferFrameBytes := <-route:
		MessagePoolReturn(transferFrameBytes)
	case <-ctx.Done():
		t.Fatalf("wait for first logical-group chunk: %v", ctx.Err())
	}
	select {
	case <-resendCapacityWait:
	case <-ctx.Done():
		t.Fatalf("wait for logical-group resend-capacity barrier: %v", ctx.Err())
	}
	client.Cancel()
	release()
	if err := client.CloseAndWait(ctx); err != nil {
		t.Fatalf("close logical-group client: %v", err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("closed logical group completed without an error")
		}
	default:
		t.Fatal("closed logical group did not publish terminal completion")
	}
	if count := resultCount.Load(); count != 1 {
		t.Fatalf("closed logical group completion count=%d, want 1", count)
	}
	releaseTransferGroupTestWitnesses(t, frames, witnesses)
}

func TestNextSendGroupChunkEndUsesCompatibilityBounds(t *testing.T) {
	frames := []*protocol.Frame{
		{MessageBytes: make([]byte, int(sendPackBatchMaxMessageByteCount)+1)},
		{MessageBytes: make([]byte, int(sendPackBatchMaxMessageByteCount)-64)},
		{MessageBytes: make([]byte, 64)},
	}
	if end := nextSendGroupChunkEnd(frames, 0); end != 1 {
		t.Fatalf("payload bound first chunk end=%d, want 1", end)
	}
	if end := nextSendGroupChunkEnd(frames, 1); end != 3 {
		t.Fatalf("2-frame compatibility bound second chunk end=%d, want 3", end)
	}
}
