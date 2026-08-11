// This file verifies the optional all-Pack lifecycle observer used to build
// exact integration boundaries around reliable and unreliable send work.
package connect

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// sendPackLifecycleTestObserver records only the requested destinations so
// unrelated client control traffic cannot satisfy an application assertion.
func sendPackLifecycleTestObserver(
	destinationIds ...Id,
) (func(SendPackLifecycleObservation), <-chan SendPackLifecycleObservation) {
	accepted := map[Id]bool{}
	for _, destinationId := range destinationIds {
		accepted[destinationId] = true
	}
	events := make(chan SendPackLifecycleObservation, 64)
	return func(observation SendPackLifecycleObservation) {
		if !accepted[observation.DestinationId] {
			return
		}
		select {
		case events <- observation:
		default:
			panic("send Pack lifecycle test event overflow")
		}
	}, events
}

// waitSendPackLifecycleObservation uses its context only as a liveness bound.
func waitSendPackLifecycleObservation(
	t *testing.T,
	ctx context.Context,
	events <-chan SendPackLifecycleObservation,
) SendPackLifecycleObservation {
	t.Helper()
	select {
	case observation := <-events:
		return observation
	case <-ctx.Done():
		t.Fatalf("wait for send Pack lifecycle observation: %v", ctx.Err())
		return SendPackLifecycleObservation{}
	}
}

// requireNoSendPackLifecycleObservations is used only after causality or a
// lifecycle join proves that the tested Pack has no remaining producer.
func requireNoSendPackLifecycleObservations(
	t *testing.T,
	events <-chan SendPackLifecycleObservation,
	name string,
) {
	t.Helper()
	if eventCount := len(events); eventCount != 0 {
		t.Fatalf("%s left %d unexpected lifecycle observations", name, eventCount)
	}
}

// requireSendPackLifecycleObservation verifies one immutable phase identity.
func requireSendPackLifecycleObservation(
	t *testing.T,
	observation SendPackLifecycleObservation,
	clientId Id,
	destinationId Id,
	token uint64,
	phase SendPackLifecyclePhase,
	ackRequired bool,
	wantError bool,
) {
	t.Helper()
	if observation.ClientId != clientId ||
		observation.DestinationId != destinationId ||
		observation.Token != token || observation.Phase != phase ||
		observation.AckRequired != ackRequired ||
		(observation.Err != nil) != wantError {
		t.Fatalf(
			"send Pack lifecycle=%+v, want client=%s destination=%s token=%d phase=%d ack=%t error=%t",
			observation,
			clientId,
			destinationId,
			token,
			phase,
			ackRequired,
			wantError,
		)
	}
}

// decodeSendPackLifecycleWirePack accepts both current direct-Pack encoding
// and the legacy TransferPack frame encoding used by older protocol settings.
func decodeSendPackLifecycleWirePack(
	t *testing.T,
	transferFrameBytes []byte,
) *protocol.Pack {
	t.Helper()
	var transferFrame protocol.TransferFrame
	if err := ProtoUnmarshal(transferFrameBytes, &transferFrame); err != nil {
		t.Fatalf("decode lifecycle transfer frame: %v", err)
	}
	if transferFrame.Pack != nil {
		return transferFrame.Pack
	}
	frame := transferFrame.GetFrame()
	if frame == nil || frame.MessageType != protocol.MessageType_TransferPack {
		t.Fatalf("lifecycle transfer had no Pack: %+v", transferFrame)
	}
	pack := &protocol.Pack{}
	if err := ProtoUnmarshal(frame.MessageBytes, pack); err != nil {
		t.Fatalf("decode lifecycle legacy Pack: %v", err)
	}
	return pack
}

// acknowledgeSendPackLifecycleWirePack injects the peer's cumulative Ack for
// a Pack already accepted by the route writer.
func acknowledgeSendPackLifecycleWirePack(
	t *testing.T,
	client *Client,
	destinationId Id,
	pack *protocol.Pack,
) {
	t.Helper()
	ack := &protocol.Ack{
		MessageId:  append([]byte{}, pack.MessageId...),
		SequenceId: append([]byte{}, pack.SequenceId...),
	}
	if !client.sendBuffer.Ack(destinationId, ack, time.Second) {
		t.Fatal("peer Ack was not admitted")
	}
}

// cleanupSendPackLifecycleSequence cancels and joins one destination sequence
// before returning any route-owned wire buffers.
func cleanupSendPackLifecycleSequence(
	t *testing.T,
	ctx context.Context,
	client *Client,
	release func(),
	sequenceDone <-chan struct{},
	route chan []byte,
) {
	t.Helper()
	client.Cancel()
	if release != nil {
		release()
	}
	if sequenceDone != nil {
		select {
		case <-sequenceDone:
		case <-ctx.Done():
			t.Errorf("cleanup send Pack lifecycle sequence: %v", ctx.Err())
		}
	}
	if route == nil {
		return
	}
	for {
		select {
		case transferFrameBytes := <-route:
			MessagePoolReturn(transferFrameBytes)
		default:
			return
		}
	}
}

// testSendPackLifecycleCoalescing verifies one phase per original Pack even
// when two compatible sends share one sequence number and physical write.
func testSendPackLifecycleCoalescing(t *testing.T, ackRequired bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	destinationId := NewId()
	observer, events := sendPackLifecycleTestObserver(destinationId)
	sequenceEntered := make(chan struct{})
	releaseSequence := make(chan struct{})
	sequenceDone := make(chan struct{})
	var sequenceEnteredOnce sync.Once
	var releaseSequenceOnce sync.Once
	var sequenceDoneOnce sync.Once
	release := func() { releaseSequenceOnce.Do(func() { close(releaseSequence) }) }

	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.SendBufferSettings.SendPackLifecycleObserver = observer
	settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
		if id.Destination != destinationId {
			return
		}
		sequenceEnteredOnce.Do(func() { close(sequenceEntered) })
		<-releaseSequence
	}
	settings.SendBufferSettings.afterRunSendSequenceForTest = func(id sendSequenceId) {
		if id.Destination == destinationId {
			sequenceDoneOnce.Do(func() { close(sequenceDone) })
		}
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	route := make(chan []byte, 1)
	defer cleanupSendPackLifecycleSequence(t, ctx, client, release, sequenceDone, route)
	client.ContractManager().AddNoContractPeer(destinationId)
	client.RouteManager().UpdateTransport(
		NewSendClientTransport(DestinationId(destinationId)),
		[]Route{route},
	)

	send := func(content string) {
		frame := RequireToFrameWithDefaultProtocolVersion(
			&protocol.SimpleMessage{Content: content},
		)
		var success bool
		if ackRequired {
			success = client.SendWithTimeout(frame, destinationId, nil, time.Second)
		} else {
			success = client.SendWithTimeout(frame, destinationId, nil, time.Second, NoAck())
		}
		if !success {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatalf("coalesced lifecycle send %q was not admitted", content)
		}
	}

	send("first")
	waitPendingSendPackBarrier(t, ctx, sequenceEntered, "lifecycle coalescer startup")
	firstStarted := waitSendPackLifecycleObservation(t, ctx, events)
	send("second")
	secondStarted := waitSendPackLifecycleObservation(t, ctx, events)
	if firstStarted.Token == secondStarted.Token {
		t.Fatalf("coalesced Packs shared token %d", firstStarted.Token)
	}
	for _, started := range []SendPackLifecycleObservation{firstStarted, secondStarted} {
		requireSendPackLifecycleObservation(
			t,
			started,
			client.ClientId(),
			destinationId,
			started.Token,
			SendPackLifecyclePhaseStarted,
			ackRequired,
			false,
		)
	}

	release()
	var transferFrameBytes []byte
	select {
	case transferFrameBytes = <-route:
	case <-ctx.Done():
		t.Fatalf("wait for coalesced lifecycle wire Pack: %v", ctx.Err())
	}
	defer MessagePoolReturn(transferFrameBytes)
	pack := decodeSendPackLifecycleWirePack(t, transferFrameBytes)
	if len(pack.Frames) != 2 || pack.Nack == ackRequired {
		t.Fatalf("coalesced lifecycle wire Pack=%+v ackRequired=%t", pack, ackRequired)
	}
	for _, started := range []SendPackLifecycleObservation{firstStarted, secondStarted} {
		firstWrite := waitSendPackLifecycleObservation(t, ctx, events)
		requireSendPackLifecycleObservation(
			t,
			firstWrite,
			client.ClientId(),
			destinationId,
			started.Token,
			SendPackLifecyclePhaseFirstRouteWrite,
			ackRequired,
			false,
		)
	}
	if ackRequired {
		// With no receive path, a terminal phase is impossible before this exact
		// injected peer Ack. This is a causality check, not a timed negative wait.
		if eventCount := len(events); eventCount != 0 {
			t.Fatalf("reliable coalesced Pack had %d terminal events before peer Ack", eventCount)
		}
		acknowledgeSendPackLifecycleWirePack(t, client, destinationId, pack)
	}
	for _, started := range []SendPackLifecycleObservation{firstStarted, secondStarted} {
		terminal := waitSendPackLifecycleObservation(t, ctx, events)
		requireSendPackLifecycleObservation(
			t,
			terminal,
			client.ClientId(),
			destinationId,
			started.Token,
			SendPackLifecyclePhaseTerminal,
			ackRequired,
			false,
		)
	}
	requireNoSendPackLifecycleObservations(t, events, "coalesced Packs")
}

// Reliable coalescing retains two identities through one peer Ack.
func TestSendPackLifecycleObserverPairsCoalescedAckPacks(t *testing.T) {
	testSendPackLifecycleCoalescing(t, true)
}

// Unreliable coalescing completes two identities at one writer disposition.
func TestSendPackLifecycleObserverPairsCoalescedNoAckPacks(t *testing.T) {
	testSendPackLifecycleCoalescing(t, false)
}

// A raw internal sender uses an Ack target rather than a callback. Its target
// result must precede the same Pack's terminal lifecycle publication.
func TestSendPackLifecycleObserverCoversRawAckTarget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	destinationId := NewId()
	observer, events := sendPackLifecycleTestObserver(destinationId)
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.SendBufferSettings.SendPackLifecycleObserver = observer
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer client.Cancel()
	client.ContractManager().AddNoContractPeer(destinationId)
	route := make(chan []byte, 1)
	client.RouteManager().UpdateTransport(
		NewSendClientTransport(DestinationId(destinationId)),
		[]Route{route},
	)
	target := &pendingSendPackAckTarget{
		results: make(chan pendingSendPackAckResult, 1),
	}
	messageBytes := MessagePoolCopy([]byte("raw lifecycle"))
	const ackValue = ByteCount(73)
	success, err := client.sendRawWithTimeoutDetailed(
		protocol.MessageType_IpIpPacketFromProvider,
		messageBytes,
		destinationId,
		target,
		ackValue,
		time.Second,
	)
	if !success || err != nil {
		MessagePoolReturn(messageBytes)
		t.Fatalf("raw lifecycle admission success=%t err=%v", success, err)
	}
	started := waitSendPackLifecycleObservation(t, ctx, events)
	requireSendPackLifecycleObservation(
		t,
		started,
		client.ClientId(),
		destinationId,
		started.Token,
		SendPackLifecyclePhaseStarted,
		true,
		false,
	)
	var transferFrameBytes []byte
	select {
	case transferFrameBytes = <-route:
	case <-ctx.Done():
		t.Fatalf("wait for raw lifecycle wire Pack: %v", ctx.Err())
	}
	defer MessagePoolReturn(transferFrameBytes)
	firstWrite := waitSendPackLifecycleObservation(t, ctx, events)
	requireSendPackLifecycleObservation(
		t,
		firstWrite,
		client.ClientId(),
		destinationId,
		started.Token,
		SendPackLifecyclePhaseFirstRouteWrite,
		true,
		false,
	)
	acknowledgeSendPackLifecycleWirePack(
		t,
		client,
		destinationId,
		decodeSendPackLifecycleWirePack(t, transferFrameBytes),
	)
	terminal := waitSendPackLifecycleObservation(t, ctx, events)
	requireSendPackLifecycleObservation(
		t,
		terminal,
		client.ClientId(),
		destinationId,
		started.Token,
		SendPackLifecyclePhaseTerminal,
		true,
		false,
	)
	if resultCount := len(target.results); resultCount != 1 {
		t.Fatalf("raw Ack target result count=%d before terminal, want 1", resultCount)
	}
	result := <-target.results
	if result.value != ackValue || result.err != nil {
		t.Fatalf("raw Ack target result=%+v", result)
	}
	if envelopeCount := len(client.rawSendPacks); envelopeCount != 1 {
		t.Fatalf("raw lifecycle envelope count=%d, want 1", envelopeCount)
	}
	requireNoSendPackLifecycleObservations(t, events, "raw Ack target")
}

// Observer and Ack callback panics are isolated independently. Neither may
// terminate the send sequence or suppress the later lifecycle phases.
func TestSendPackLifecycleObserverAndAckCallbackPanicsAreIsolated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	destinationId := NewId()
	events := make(chan SendPackLifecycleObservation, 4)
	observer := func(observation SendPackLifecycleObservation) {
		if observation.DestinationId != destinationId {
			return
		}
		if observation.Phase == SendPackLifecyclePhaseStarted {
			panic("started observer panic")
		}
		events <- observation
	}
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.SendBufferSettings.SendPackLifecycleObserver = observer
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer client.Cancel()
	client.ContractManager().AddNoContractPeer(destinationId)
	route := make(chan []byte, 1)
	client.RouteManager().UpdateTransport(
		NewSendClientTransport(DestinationId(destinationId)),
		[]Route{route},
	)
	frame := RequireToFrameWithDefaultProtocolVersion(
		&protocol.SimpleMessage{Content: "panic isolation"},
	)
	if !client.SendWithTimeout(
		frame,
		destinationId,
		func(error) { panic("Ack callback panic") },
		time.Second,
	) {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatal("panic-isolation Pack was not admitted")
	}
	var transferFrameBytes []byte
	select {
	case transferFrameBytes = <-route:
	case <-ctx.Done():
		t.Fatalf("wait for panic-isolation wire Pack: %v", ctx.Err())
	}
	defer MessagePoolReturn(transferFrameBytes)
	firstWrite := waitSendPackLifecycleObservation(t, ctx, events)
	requireSendPackLifecycleObservation(
		t, firstWrite, client.ClientId(), destinationId, firstWrite.Token,
		SendPackLifecyclePhaseFirstRouteWrite, true, false,
	)
	acknowledgeSendPackLifecycleWirePack(
		t,
		client,
		destinationId,
		decodeSendPackLifecycleWirePack(t, transferFrameBytes),
	)
	terminal := waitSendPackLifecycleObservation(t, ctx, events)
	requireSendPackLifecycleObservation(
		t, terminal, client.ClientId(), destinationId, firstWrite.Token,
		SendPackLifecyclePhaseTerminal, true, false,
	)
	requireNoSendPackLifecycleObservations(t, events, "panic-isolation Pack")
}

// Admission rejection emits both remaining error phases synchronously while
// preserving caller ownership and excluding the user's Ack callback.
func TestSendPackLifecycleObserverCompletesRejectedPack(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	destinationId := NewId()
	observer, events := sendPackLifecycleTestObserver(destinationId)
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.SendBufferSettings.SendPackLifecycleObserver = observer
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer client.Cancel()
	client.ContractManager().AddNoContractPeer(destinationId)
	sendCtx, sendCancel := context.WithCancel(ctx)
	sendCancel()
	callbackResults := make(chan error, 1)
	frame := RequireToFrameWithDefaultProtocolVersion(
		&protocol.SimpleMessage{Content: "rejected lifecycle"},
	)
	success, err := client.SendWithTimeoutDetailed(
		frame,
		destinationId,
		func(callbackErr error) { callbackResults <- callbackErr },
		0,
		Ctx(sendCtx),
	)
	if success || err == nil {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatalf("canceled lifecycle admission success=%t err=%v", success, err)
	}
	MessagePoolReturn(frame.MessageBytes)
	started := waitSendPackLifecycleObservation(t, ctx, events)
	firstWrite := waitSendPackLifecycleObservation(t, ctx, events)
	terminal := waitSendPackLifecycleObservation(t, ctx, events)
	requireSendPackLifecycleObservation(
		t, started, client.ClientId(), destinationId, started.Token,
		SendPackLifecyclePhaseStarted, true, false,
	)
	requireSendPackLifecycleObservation(
		t, firstWrite, client.ClientId(), destinationId, started.Token,
		SendPackLifecyclePhaseFirstRouteWrite, true, true,
	)
	requireSendPackLifecycleObservation(
		t, terminal, client.ClientId(), destinationId, started.Token,
		SendPackLifecyclePhaseTerminal, true, true,
	)
	if !errors.Is(firstWrite.Err, err) || !errors.Is(terminal.Err, err) {
		t.Fatalf("rejection phases first=%v terminal=%v send=%v", firstWrite.Err, terminal.Err, err)
	}
	if callbackCount := len(callbackResults); callbackCount != 0 {
		t.Fatalf("rejected Pack invoked %d Ack callbacks", callbackCount)
	}
	requireNoSendPackLifecycleObservations(t, events, "rejected Pack")
}

// A Pack admitted before sequence startup remains sequence-owned. Closing the
// client must report no-write and terminal errors before lifecycle join.
func TestSendPackLifecycleObserverCompletesQueuedClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	destinationId := NewId()
	observer, events := sendPackLifecycleTestObserver(destinationId)
	sequenceEntered := make(chan struct{})
	releaseSequence := make(chan struct{})
	sequenceDone := make(chan struct{})
	var enteredOnce sync.Once
	var releaseOnce sync.Once
	var doneOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSequence) }) }
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.SendBufferSettings.SendPackLifecycleObserver = observer
	settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
		if id.Destination != destinationId {
			return
		}
		enteredOnce.Do(func() { close(sequenceEntered) })
		<-releaseSequence
	}
	settings.SendBufferSettings.afterRunSendSequenceForTest = func(id sendSequenceId) {
		if id.Destination == destinationId {
			doneOnce.Do(func() { close(sequenceDone) })
		}
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer cleanupSendPackLifecycleSequence(t, ctx, client, release, sequenceDone, nil)
	client.ContractManager().AddNoContractPeer(destinationId)
	callbackResults := make(chan error, 1)
	frame := RequireToFrameWithDefaultProtocolVersion(
		&protocol.SimpleMessage{Content: "queued lifecycle close"},
	)
	if !client.SendWithTimeout(
		frame,
		destinationId,
		func(err error) { callbackResults <- err },
		time.Second,
	) {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatal("queued lifecycle close Pack was not admitted")
	}
	waitPendingSendPackBarrier(t, ctx, sequenceEntered, "queued lifecycle startup")
	started := waitSendPackLifecycleObservation(t, ctx, events)
	client.Cancel()
	release()
	waitPendingSendPackBarrier(t, ctx, sequenceDone, "queued lifecycle completion")
	firstWrite := waitSendPackLifecycleObservation(t, ctx, events)
	terminal := waitSendPackLifecycleObservation(t, ctx, events)
	requireSendPackLifecycleObservation(
		t, started, client.ClientId(), destinationId, started.Token,
		SendPackLifecyclePhaseStarted, true, false,
	)
	requireSendPackLifecycleObservation(
		t, firstWrite, client.ClientId(), destinationId, started.Token,
		SendPackLifecyclePhaseFirstRouteWrite, true, true,
	)
	requireSendPackLifecycleObservation(
		t, terminal, client.ClientId(), destinationId, started.Token,
		SendPackLifecyclePhaseTerminal, true, true,
	)
	requirePendingSendPackError(t, callbackResults, "queued lifecycle close Pack")
	requireNoSendPackLifecycleObservations(t, events, "queued close Pack")
}

// The Ack-timeout exit is forced at its exact branch instead of depending on
// a short timer. It must publish a terminal error and join the sequence.
func TestSendPackLifecycleObserverCompletesAckTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	destinationId := NewId()
	observer, events := sendPackLifecycleTestObserver(destinationId)
	sequenceDone := make(chan struct{})
	var doneOnce sync.Once
	var forcedCount atomic.Uint64
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.SendBufferSettings.SendPackLifecycleObserver = observer
	settings.SendBufferSettings.AckTimeout = time.Minute
	settings.SendBufferSettings.forceAckTimeoutForTest = func(id sendSequenceId) bool {
		if id.Destination != destinationId {
			return false
		}
		forcedCount.Add(1)
		return true
	}
	settings.SendBufferSettings.afterRunSendSequenceForTest = func(id sendSequenceId) {
		if id.Destination == destinationId {
			doneOnce.Do(func() { close(sequenceDone) })
		}
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	route := make(chan []byte, 1)
	defer cleanupSendPackLifecycleSequence(t, ctx, client, nil, sequenceDone, route)
	client.ContractManager().AddNoContractPeer(destinationId)
	client.RouteManager().UpdateTransport(
		NewSendClientTransport(DestinationId(destinationId)),
		[]Route{route},
	)
	callbackResults := make(chan error, 1)
	frame := RequireToFrameWithDefaultProtocolVersion(
		&protocol.SimpleMessage{Content: "forced Ack timeout"},
	)
	if !client.SendWithTimeout(
		frame,
		destinationId,
		func(err error) { callbackResults <- err },
		time.Second,
	) {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatal("forced Ack-timeout Pack was not admitted")
	}
	started := waitSendPackLifecycleObservation(t, ctx, events)
	var transferFrameBytes []byte
	select {
	case transferFrameBytes = <-route:
	case <-ctx.Done():
		t.Fatalf("wait for forced Ack-timeout wire Pack: %v", ctx.Err())
	}
	MessagePoolReturn(transferFrameBytes)
	firstWrite := waitSendPackLifecycleObservation(t, ctx, events)
	terminal := waitSendPackLifecycleObservation(t, ctx, events)
	waitPendingSendPackBarrier(t, ctx, sequenceDone, "forced Ack-timeout completion")
	requireSendPackLifecycleObservation(
		t, started, client.ClientId(), destinationId, started.Token,
		SendPackLifecyclePhaseStarted, true, false,
	)
	requireSendPackLifecycleObservation(
		t, firstWrite, client.ClientId(), destinationId, started.Token,
		SendPackLifecyclePhaseFirstRouteWrite, true, false,
	)
	requireSendPackLifecycleObservation(
		t, terminal, client.ClientId(), destinationId, started.Token,
		SendPackLifecyclePhaseTerminal, true, true,
	)
	if forcedCount.Load() == 0 {
		t.Fatal("Ack-timeout branch was not forced")
	}
	requirePendingSendPackError(t, callbackResults, "forced Ack-timeout Pack")
	requireNoSendPackLifecycleObservations(t, events, "forced Ack-timeout Pack")
}

// Contract acquisition failure is a sequence-owned pre-write exit. It emits
// one writer error and one Ack-owned terminal error, never two terminals.
func TestSendPackLifecycleObserverCompletesContractFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	destinationId := NewId()
	observer, events := sendPackLifecycleTestObserver(destinationId)
	sequenceDone := make(chan struct{})
	var doneOnce sync.Once
	var forcedCount atomic.Uint64
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.SendBufferSettings.PrewarmOpeningContract = false
	settings.SendBufferSettings.SendPackLifecycleObserver = observer
	settings.SendBufferSettings.forceContractFailureForTest = func(id sendSequenceId) bool {
		if id.Destination != destinationId {
			return false
		}
		forcedCount.Add(1)
		return true
	}
	settings.SendBufferSettings.afterRunSendSequenceForTest = func(id sendSequenceId) {
		if id.Destination == destinationId {
			doneOnce.Do(func() { close(sequenceDone) })
		}
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer cleanupSendPackLifecycleSequence(t, ctx, client, nil, sequenceDone, nil)
	callbackResults := make(chan error, 1)
	frame := RequireToFrameWithDefaultProtocolVersion(
		&protocol.SimpleMessage{Content: "forced contract failure"},
	)
	if !client.SendWithTimeout(
		frame,
		destinationId,
		func(err error) { callbackResults <- err },
		time.Second,
	) {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatal("forced contract-failure Pack was not admitted")
	}
	started := waitSendPackLifecycleObservation(t, ctx, events)
	firstWrite := waitSendPackLifecycleObservation(t, ctx, events)
	terminal := waitSendPackLifecycleObservation(t, ctx, events)
	waitPendingSendPackBarrier(t, ctx, sequenceDone, "contract-failure completion")
	requireSendPackLifecycleObservation(
		t, started, client.ClientId(), destinationId, started.Token,
		SendPackLifecyclePhaseStarted, true, false,
	)
	requireSendPackLifecycleObservation(
		t, firstWrite, client.ClientId(), destinationId, started.Token,
		SendPackLifecyclePhaseFirstRouteWrite, true, true,
	)
	requireSendPackLifecycleObservation(
		t, terminal, client.ClientId(), destinationId, started.Token,
		SendPackLifecyclePhaseTerminal, true, true,
	)
	if forcedCount.Load() != 1 {
		t.Fatalf("forced contract-failure count=%d, want 1", forcedCount.Load())
	}
	requirePendingSendPackError(t, callbackResults, "forced contract-failure Pack")
	requireNoSendPackLifecycleObservations(t, events, "contract-failure Pack")
}

// Holding final-Ack publication exposes the exact ownership ordering: the
// resend queue and user callback are complete before Terminal becomes visible.
func TestSendPackLifecycleTerminalAckFollowsResendRemoval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	destinationId := NewId()
	events := make(chan SendPackLifecycleObservation, 8)
	terminalEntered := make(chan struct{})
	releaseTerminal := make(chan struct{})
	var terminalEnteredOnce sync.Once
	var releaseTerminalOnce sync.Once
	release := func() { releaseTerminalOnce.Do(func() { close(releaseTerminal) }) }
	defer release()
	observer := func(observation SendPackLifecycleObservation) {
		if observation.DestinationId != destinationId {
			return
		}
		if observation.Phase == SendPackLifecyclePhaseTerminal {
			terminalEnteredOnce.Do(func() { close(terminalEntered) })
			<-releaseTerminal
		}
		events <- observation
	}
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.SendBufferSettings.SendPackLifecycleObserver = observer
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer client.Cancel()
	client.ContractManager().AddNoContractPeer(destinationId)
	route := make(chan []byte, 1)
	client.RouteManager().UpdateTransport(
		NewSendClientTransport(DestinationId(destinationId)),
		[]Route{route},
	)
	callbackResults := make(chan error, 1)
	frame := RequireToFrameWithDefaultProtocolVersion(
		&protocol.SimpleMessage{Content: "held final Ack"},
	)
	if !client.SendWithTimeout(
		frame,
		destinationId,
		func(err error) { callbackResults <- err },
		time.Second,
	) {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatal("held-final-Ack Pack was not admitted")
	}
	started := waitSendPackLifecycleObservation(t, ctx, events)
	var transferFrameBytes []byte
	select {
	case transferFrameBytes = <-route:
	case <-ctx.Done():
		t.Fatalf("wait for held-final-Ack wire Pack: %v", ctx.Err())
	}
	defer MessagePoolReturn(transferFrameBytes)
	firstWrite := waitSendPackLifecycleObservation(t, ctx, events)
	acknowledgeSendPackLifecycleWirePack(
		t,
		client,
		destinationId,
		decodeSendPackLifecycleWirePack(t, transferFrameBytes),
	)
	waitPendingSendPackBarrier(t, ctx, terminalEntered, "held terminal Ack")
	queueCount, _, _ := client.ResendQueueSize(destinationId, MultiHopId{}, false, false)
	if queueCount != 0 {
		t.Fatalf("terminal Ack entered with %d resend items still owned", queueCount)
	}
	if callbackCount := len(callbackResults); callbackCount != 1 {
		t.Fatalf("terminal Ack entered before callback result count=%d", callbackCount)
	}
	if err := <-callbackResults; err != nil {
		t.Fatalf("held final Ack callback: %v", err)
	}
	release()
	terminal := waitSendPackLifecycleObservation(t, ctx, events)
	requireSendPackLifecycleObservation(
		t, started, client.ClientId(), destinationId, started.Token,
		SendPackLifecyclePhaseStarted, true, false,
	)
	requireSendPackLifecycleObservation(
		t, firstWrite, client.ClientId(), destinationId, started.Token,
		SendPackLifecyclePhaseFirstRouteWrite, true, false,
	)
	requireSendPackLifecycleObservation(
		t, terminal, client.ClientId(), destinationId, started.Token,
		SendPackLifecyclePhaseTerminal, true, false,
	)
	requireNoSendPackLifecycleObservations(t, events, "held final Ack Pack")
}

// A failed initial writer does not determine reliable terminal success. One
// forced resend can use a newly published route, and its peer Ack completes
// the same token successfully after resend ownership is removed.
func TestSendPackLifecycleInitialWriteErrorCanResendAndAck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	destinationId := NewId()
	events := make(chan SendPackLifecycleObservation, 8)
	firstWriteEntered := make(chan struct{})
	releaseFirstWrite := make(chan struct{})
	var firstWriteEnteredOnce sync.Once
	var releaseFirstWriteOnce sync.Once
	release := func() { releaseFirstWriteOnce.Do(func() { close(releaseFirstWrite) }) }
	defer release()
	observer := func(observation SendPackLifecycleObservation) {
		if observation.DestinationId != destinationId {
			return
		}
		if observation.Phase == SendPackLifecyclePhaseFirstRouteWrite && observation.Err != nil {
			firstWriteEnteredOnce.Do(func() { close(firstWriteEntered) })
			<-releaseFirstWrite
		}
		events <- observation
	}
	var forcedResend atomic.Bool
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.SendBufferSettings.SendPackLifecycleObserver = observer
	settings.SendBufferSettings.WriteTimeout = 0
	settings.SendBufferSettings.MinResendInterval = time.Minute
	settings.SendBufferSettings.RttMinResendInterval = time.Minute
	settings.SendBufferSettings.MaxResendInterval = time.Minute
	settings.SendBufferSettings.forceResendForTest = func(id sendSequenceId) bool {
		return id.Destination == destinationId && forcedResend.CompareAndSwap(false, true)
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer client.Cancel()
	client.ContractManager().AddNoContractPeer(destinationId)
	blockedInitialRoute := make(chan []byte)
	client.RouteManager().UpdateTransport(
		NewSendClientTransport(DestinationId(destinationId)),
		[]Route{blockedInitialRoute},
	)
	callbackResults := make(chan error, 1)
	frame := RequireToFrameWithDefaultProtocolVersion(
		&protocol.SimpleMessage{Content: "recover initial writer failure"},
	)
	if !client.SendWithTimeout(
		frame,
		destinationId,
		func(err error) { callbackResults <- err },
		time.Second,
	) {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatal("recoverable writer-failure Pack was not admitted")
	}
	started := waitSendPackLifecycleObservation(t, ctx, events)
	waitPendingSendPackBarrier(t, ctx, firstWriteEntered, "failed first writer")
	resendRoute := make(chan []byte, 1)
	client.RouteManager().UpdateTransport(
		NewSendClientTransport(DestinationId(destinationId)),
		[]Route{resendRoute},
	)
	release()
	firstWrite := waitSendPackLifecycleObservation(t, ctx, events)
	var resendTransferFrameBytes []byte
	select {
	case resendTransferFrameBytes = <-resendRoute:
	case <-ctx.Done():
		t.Fatalf("wait for successful lifecycle resend: %v", ctx.Err())
	}
	defer MessagePoolReturn(resendTransferFrameBytes)
	acknowledgeSendPackLifecycleWirePack(
		t,
		client,
		destinationId,
		decodeSendPackLifecycleWirePack(t, resendTransferFrameBytes),
	)
	terminal := waitSendPackLifecycleObservation(t, ctx, events)
	requireSendPackLifecycleObservation(
		t, started, client.ClientId(), destinationId, started.Token,
		SendPackLifecyclePhaseStarted, true, false,
	)
	requireSendPackLifecycleObservation(
		t, firstWrite, client.ClientId(), destinationId, started.Token,
		SendPackLifecyclePhaseFirstRouteWrite, true, true,
	)
	requireSendPackLifecycleObservation(
		t, terminal, client.ClientId(), destinationId, started.Token,
		SendPackLifecyclePhaseTerminal, true, false,
	)
	if !forcedResend.Load() {
		t.Fatal("successful retry did not use the deterministic resend branch")
	}
	queueCount, _, _ := client.ResendQueueSize(destinationId, MultiHopId{}, false, false)
	if queueCount != 0 {
		t.Fatalf("successful resend terminal retained %d resend items", queueCount)
	}
	if callbackCount := len(callbackResults); callbackCount != 1 {
		t.Fatalf("successful resend callback count=%d, want 1", callbackCount)
	}
	if err := <-callbackResults; err != nil {
		t.Fatalf("successful resend callback: %v", err)
	}
	requireNoSendPackLifecycleObservations(t, events, "successful resend Pack")
}

// Tokens intentionally restart with a rebuilt Client, even when its logical
// ClientId is unchanged. A shared tracker must include observer registration
// identity in its key; ClientId plus token alone is ambiguous.
func TestSendPackLifecycleRebuiltClientTokensNeedObserverNamespace(t *testing.T) {
	type registeredObservation struct {
		instance    uint64
		observation SendPackLifecycleObservation
	}
	logicalClientId := NewId()
	destinationId := NewId()
	events := make(chan registeredObservation, 6)
	for instance := uint64(1); instance <= 2; instance += 1 {
		instance := instance
		observer := func(observation SendPackLifecycleObservation) {
			events <- registeredObservation{instance: instance, observation: observation}
		}
		settings := &ClientSettings{
			SendBufferSettings: &SendBufferSettings{
				SendPackLifecycleObserver: observer,
			},
		}
		client := &Client{clientId: logicalClientId, settings: settings}
		pack := &SendPack{
			TransferOptions: TransferOptions{Ack: true},
			Destination:     destinationId,
		}
		client.startSendPackLifecycle(pack)
		pack.completeLifecycleWithoutRouteWrite(errors.New("test disposition"))
	}

	phases := map[uint64][]SendPackLifecycleObservation{}
	for range 6 {
		event := <-events
		phases[event.instance] = append(phases[event.instance], event.observation)
	}
	if len(phases) != 2 {
		t.Fatalf("rebuilt observer instances=%v", phases)
	}
	for instance, observations := range phases {
		if len(observations) != 3 {
			t.Fatalf("rebuilt instance %d phase count=%d", instance, len(observations))
		}
		for phaseIndex, observation := range observations {
			wantPhase := SendPackLifecyclePhase(phaseIndex + 1)
			requireSendPackLifecycleObservation(
				t,
				observation,
				logicalClientId,
				destinationId,
				1,
				wantPhase,
				true,
				phaseIndex != 0,
			)
		}
	}
}

// A later NoAck Pack can complete while an earlier reliable Pack remains
// unacknowledged, but its distinct token cannot represent the earlier Pack.
func TestSendPackLifecycleLaterTerminalCannotSatisfyEarlierPack(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	firstDestinationId := NewId()
	laterDestinationId := NewId()
	observer, events := sendPackLifecycleTestObserver(firstDestinationId, laterDestinationId)
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.SendBufferSettings.SendPackLifecycleObserver = observer
	settings.SendBufferSettings.MinResendInterval = time.Minute
	settings.SendBufferSettings.MaxResendInterval = time.Minute
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer client.Cancel()
	firstRoute := make(chan []byte, 1)
	laterRoute := make(chan []byte, 1)
	for destinationId, route := range map[Id]chan []byte{
		firstDestinationId: firstRoute,
		laterDestinationId: laterRoute,
	} {
		client.ContractManager().AddNoContractPeer(destinationId)
		client.RouteManager().UpdateTransport(
			NewSendClientTransport(DestinationId(destinationId)),
			[]Route{route},
		)
	}
	firstFrame := RequireToFrameWithDefaultProtocolVersion(
		&protocol.SimpleMessage{Content: "earlier reliable Pack"},
	)
	if !client.SendWithTimeout(firstFrame, firstDestinationId, nil, time.Second) {
		MessagePoolReturn(firstFrame.MessageBytes)
		t.Fatal("earlier reliable Pack was not admitted")
	}
	firstStarted := waitSendPackLifecycleObservation(t, ctx, events)
	select {
	case transferFrameBytes := <-firstRoute:
		MessagePoolReturn(transferFrameBytes)
	case <-ctx.Done():
		t.Fatalf("wait for earlier reliable Pack route: %v", ctx.Err())
	}
	firstWrite := waitSendPackLifecycleObservation(t, ctx, events)
	requireSendPackLifecycleObservation(
		t, firstStarted, client.ClientId(), firstDestinationId, firstStarted.Token,
		SendPackLifecyclePhaseStarted, true, false,
	)
	requireSendPackLifecycleObservation(
		t, firstWrite, client.ClientId(), firstDestinationId, firstStarted.Token,
		SendPackLifecyclePhaseFirstRouteWrite, true, false,
	)

	laterFrame := RequireToFrameWithDefaultProtocolVersion(
		&protocol.SimpleMessage{Content: "later unreliable Pack"},
	)
	if !client.SendWithTimeout(laterFrame, laterDestinationId, nil, time.Second, NoAck()) {
		MessagePoolReturn(laterFrame.MessageBytes)
		t.Fatal("later NoAck Pack was not admitted")
	}
	laterStarted := waitSendPackLifecycleObservation(t, ctx, events)
	select {
	case transferFrameBytes := <-laterRoute:
		MessagePoolReturn(transferFrameBytes)
	case <-ctx.Done():
		t.Fatalf("wait for later NoAck Pack route: %v", ctx.Err())
	}
	laterFirstWrite := waitSendPackLifecycleObservation(t, ctx, events)
	laterTerminal := waitSendPackLifecycleObservation(t, ctx, events)
	if laterStarted.Token == firstStarted.Token {
		t.Fatalf("later Pack reused held token %d", laterStarted.Token)
	}
	requireSendPackLifecycleObservation(
		t, laterStarted, client.ClientId(), laterDestinationId, laterStarted.Token,
		SendPackLifecyclePhaseStarted, false, false,
	)
	requireSendPackLifecycleObservation(
		t, laterFirstWrite, client.ClientId(), laterDestinationId, laterStarted.Token,
		SendPackLifecyclePhaseFirstRouteWrite, false, false,
	)
	requireSendPackLifecycleObservation(
		t, laterTerminal, client.ClientId(), laterDestinationId, laterStarted.Token,
		SendPackLifecyclePhaseTerminal, false, false,
	)
	// No path in this fixture can manufacture a peer Ack for the earlier Pack.
	// After the later terminal is consumed, any terminal for the earlier token
	// would therefore be an identity substitution rather than scheduler timing.
	requireNoSendPackLifecycleObservations(t, events, "later completed Pack")
}
