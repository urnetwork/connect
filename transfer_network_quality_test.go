// Public and wire-level quality notification roots use explicit worker
// barriers; no scheduler delay stands in for event ordering.
package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func newNetworkQualityTestClient(t *testing.T) *Client {
	t.Helper()
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	client := NewClient(context.Background(), NewId(), NewNoContractClientOob(), settings)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.CloseAndWait(ctx); err != nil {
			t.Errorf("close quality client: %v", err)
		}
	})
	return client
}

func awaitNetworkQualityTestValue[T any](t *testing.T, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("network-quality worker did not reach the deterministic barrier")
		var zero T
		return zero
	}
}

func TestNetworkQualityAndHardNetworkNotificationsStayDistinct(t *testing.T) {
	qualityChanges := 0
	networkChanges := 0
	removeQuality := AddNetworkQualityChangeListener(func() { qualityChanges++ })
	defer removeQuality()
	removeNetwork := AddNetworkChangeListener(func() { networkChanges++ })
	defer removeNetwork()

	NetworkQualityChanged()
	if qualityChanges != 1 || networkChanges != 0 {
		t.Fatalf("quality notification reconnected: quality=%d network=%d", qualityChanges, networkChanges)
	}
	NetworkChanged()
	if qualityChanges != 2 || networkChanges != 1 {
		t.Fatalf("hard change did not invalidate each layer once: quality=%d network=%d", qualityChanges, networkChanges)
	}
}

func TestClientNetworkQualityCallbackIsNonblockingAndQuietCoalesced(t *testing.T) {
	client := newNetworkQualityTestClient(t)
	state := client.networkQuality
	entered := make(chan struct{})
	release := make(chan struct{})
	applied := make(chan uint64, 4)
	var blockOnce sync.Once
	state.beforeApplyForTest = func() {
		blockOnce.Do(func() {
			close(entered)
			<-release
		})
	}
	state.afterLocalApplyForTest = func(generation uint64) { applied <- generation }

	at := time.Unix(1700000000, 0)
	returned := make(chan struct{})
	go func() {
		state.dispatchLocal(at)
		close(returned)
	}()
	awaitNetworkQualityTestValue(t, entered)
	awaitNetworkQualityTestValue(t, returned)

	state.dispatchLocal(at.Add(time.Second))
	state.dispatchLocal(at.Add(4 * time.Second))
	state.dispatchLocal(at.Add(10 * time.Second))
	close(release)
	if generation := awaitNetworkQualityTestValue(t, applied); generation != 1 {
		t.Fatalf("first generation = %d", generation)
	}
	if generation := awaitNetworkQualityTestValue(t, applied); generation != 2 {
		t.Fatalf("post-quiet generation = %d", generation)
	}

	state.stateLock.Lock()
	generation := state.localGeneration
	last := state.lastLocalNotification
	state.stateLock.Unlock()
	if generation != 2 || !last.Equal(at.Add(10*time.Second)) {
		t.Fatalf("noise renewed a generation: generation=%d last=%s", generation, last)
	}
}

// Platform listeners may repeat the same radio observation from many callback
// threads. Hold one statistics snapshot across the estimator reset so every
// ordering is explicit: the notification burst must return without waiting
// for statistics, collapse to one generation, and leave both the public
// snapshot and the next quiet-interval generation usable.
func TestClientNetworkQualityFrequentCallbacksAreIdempotentWithStatistics(t *testing.T) {
	client := newNetworkQualityTestClient(t)
	// Keep this statistics/reset root independent of peer notification sends.
	// A local loopback destination is included in estimator resets and public
	// snapshots, but must never receive a redundant wire quality hint.
	destinationId := client.clientId
	sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
		settings.DeliverySizedWindowScale = 2
		settings.ResendQueueBudget = NewTransferMemoryBudget(16 * 1024 * 1024)
	})
	sequence.client = client
	sequence.windowPacer.service = newWindowPacingService(sequence.sendBufferSettings)
	client.sendBuffer.mutex.Lock()
	client.sendBuffer.sendSequences[sendSequenceId{Destination: destinationId}] = sequence
	client.sendBuffer.mutex.Unlock()
	if peers := client.sendBuffer.networkQualityPeers(); len(peers) != 0 {
		t.Fatalf("local loopback entered quality fanout: %v", peers)
	}
	defer func() {
		client.sendBuffer.mutex.Lock()
		delete(client.sendBuffer.sendSequences, sendSequenceId{Destination: destinationId})
		client.sendBuffer.mutex.Unlock()
		sequence.resendQueue.Clear()
	}()

	state := client.networkQuality
	applyEntered := make(chan struct{})
	applyRelease := make(chan struct{})
	statisticsEntered := make(chan struct{})
	statisticsRelease := make(chan struct{})
	appliedGenerations := make(chan uint64, 4)
	var blockApplyOnce sync.Once
	var blockStatisticsOnce sync.Once
	state.beforeApplyForTest = func() {
		blockApplyOnce.Do(func() {
			close(applyEntered)
			<-applyRelease
		})
	}
	state.afterLocalApplyForTest = func(generation uint64) {
		appliedGenerations <- generation
	}
	sequence.beforeWindowRetentionForTest = func() {
		blockStatisticsOnce.Do(func() {
			close(statisticsEntered)
			<-statisticsRelease
		})
	}

	callbackAt := time.Unix(1700000000, 0)
	state.dispatchLocal(callbackAt)
	awaitNetworkQualityTestValue(t, applyEntered)

	const statisticsReaderCount = 32
	statistics := make(chan SendDestinationStats, statisticsReaderCount)
	statisticsStart := make(chan struct{})
	var statisticsReaders sync.WaitGroup
	for range statisticsReaderCount {
		statisticsReaders.Add(1)
		go func() {
			defer statisticsReaders.Done()
			<-statisticsStart
			statistics <- client.DestinationSendStats(destinationId)
			_ = client.ReceiveStats()
			_ = client.SendRecoveryStats()
		}()
	}
	close(statisticsStart)
	awaitNetworkQualityTestValue(t, statisticsEntered)
	close(applyRelease)

	// Every timestamp is inside the first generation. Dispatch order is
	// deliberately concurrent, so idempotency cannot depend on callback order.
	const callbackCount = 128
	var callbacks sync.WaitGroup
	for index := range callbackCount {
		callbacks.Add(1)
		go func() {
			defer callbacks.Done()
			state.dispatchLocal(callbackAt.Add(time.Duration(index+1) * time.Nanosecond))
		}()
	}
	callbacks.Wait()
	state.stateLock.Lock()
	stormGeneration := state.localGeneration
	stormLastNotification := state.lastLocalNotification
	state.stateLock.Unlock()
	if stormGeneration != 1 || !stormLastNotification.Equal(callbackAt.Add(callbackCount*time.Nanosecond)) {
		t.Fatalf("callback storm renewed generation: generation=%d last=%s", stormGeneration, stormLastNotification)
	}

	close(statisticsRelease)
	statisticsReaders.Wait()
	close(statistics)
	if generation := awaitNetworkQualityTestValue(t, appliedGenerations); generation != 1 {
		t.Fatalf("callback storm applied generation %d, want 1", generation)
	}
	for snapshot := range statistics {
		if snapshot.SequenceCount != 1 || snapshot.SendWindow.Window <= 0 ||
			snapshot.SendWindow.Initial <= 0 || snapshot.SendWindow.Floor <= 0 ||
			snapshot.SendWindow.Interval < 0 || snapshot.SendWindow.RoundTrip < 0 {
			t.Fatalf("callback reset exposed invalid statistics: %+v", snapshot)
		}
	}

	quietAt := stormLastNotification.Add(windowQualityRemeasureInterval + time.Nanosecond)
	state.dispatchLocal(quietAt)
	if generation := awaitNetworkQualityTestValue(t, appliedGenerations); generation != 2 {
		t.Fatalf("post-quiet callback applied generation %d, want 2", generation)
	}
	state.stateLock.Lock()
	finalGeneration := state.localGeneration
	finalLastNotification := state.lastLocalNotification
	state.stateLock.Unlock()
	if finalGeneration != 2 || !finalLastNotification.Equal(quietAt) ||
		sequence.windowQualityAfterNanos != quietAt.UnixNano() {
		t.Fatalf("post-quiet generation was not applied once: generation=%d last=%s sequence=%d",
			finalGeneration, finalLastNotification, sequence.windowQualityAfterNanos)
	}
}

func TestClientNetworkQualityRemoteGenerationScopesWithoutEcho(t *testing.T) {
	client := newNetworkQualityTestClient(t)
	peerId, otherId := NewId(), NewId()
	peerService := newWindowPacingService(DefaultSendBufferSettings())
	otherService := newWindowPacingService(DefaultSendBufferSettings())
	peerSequence := &SendSequence{
		client:             client,
		windowPacer:        windowBurstPacer{service: peerService},
		sendBufferSettings: DefaultSendBufferSettings(),
	}
	otherSequence := &SendSequence{
		client:             client,
		windowPacer:        windowBurstPacer{service: otherService},
		sendBufferSettings: DefaultSendBufferSettings(),
	}
	client.sendBuffer.mutex.Lock()
	client.sendBuffer.sendSequences[sendSequenceId{Destination: peerId}] = peerSequence
	client.sendBuffer.sendSequences[sendSequenceId{Destination: otherId}] = otherSequence
	client.sendBuffer.mutex.Unlock()
	defer func() {
		client.sendBuffer.mutex.Lock()
		delete(client.sendBuffer.sendSequences, sendSequenceId{Destination: peerId})
		delete(client.sendBuffer.sendSequences, sendSequenceId{Destination: otherId})
		client.sendBuffer.mutex.Unlock()
	}()

	remoteApplied := make(chan Id, 4)
	var peerNotifications atomic.Int64
	client.networkQuality.afterRemoteApplyForTest = func(id Id) { remoteApplied <- id }
	client.networkQuality.afterPeerNotificationForTest = func(Id) { peerNotifications.Add(1) }
	oldInstance, instance := NewId(), NewId()
	sendRemote := func(instanceId Id, generation uint64) {
		messageBytes := encodeNetworkQualityMessage(instanceId, generation)
		client.networkQuality.receive(
			TransferPath{SourceId: peerId},
			networkQualitySubprotocolId,
			messageBytes,
			Peer{},
		)
		MessagePoolReturn(messageBytes)
	}

	sendRemote(instance, 2)
	if appliedId := awaitNetworkQualityTestValue(t, remoteApplied); appliedId != peerId {
		t.Fatalf("quality applied to %s, want %s", appliedId, peerId)
	}
	if peerSequence.windowQualityAfterNanos == 0 || otherSequence.windowQualityAfterNanos != 0 {
		t.Fatal("remote quality hint reset an unrelated peer or missed its source")
	}
	firstCutoff := peerSequence.windowQualityAfterNanos
	sendRemote(instance, 2)
	sendRemote(instance, 1)
	sendRemote(oldInstance, 100)

	client.networkQuality.stateLock.Lock()
	remote := client.networkQuality.remoteGenerations[peerId]
	pending := len(client.networkQuality.pendingRemotePeerIdAts)
	client.networkQuality.stateLock.Unlock()
	if remote.instanceId != instance || remote.generation != 2 || pending != 0 {
		t.Fatalf("duplicate or stale generation was accepted: %+v pending=%d", remote, pending)
	}

	restartedInstance := NewId()
	sendRemote(restartedInstance, 1)
	awaitNetworkQualityTestValue(t, remoteApplied)
	if peerSequence.windowQualityAfterNanos != firstCutoff {
		t.Fatal("a duplicate-time remote restart renewed the core generation")
	}
	if peerNotifications.Load() != 0 {
		t.Fatalf("remote signal echoed to %d peers", peerNotifications.Load())
	}
}

func TestClientNetworkQualityReceiveOnlyPeerIsIncludedInFanout(t *testing.T) {
	client := newNetworkQualityTestClient(t)
	peerId := NewId()
	peer := Peer{
		ProvideMode: protocol.ProvideMode_Network,
		TransferKey: TransferKey{
			ForceStream:         true,
			EncryptionRole:      protocol.SequenceRole_SequenceRoleServer,
			EncryptionCompanion: true,
		},
	}
	client.observeNetworkQualityPeer(TransferPath{SourceId: peerId}, peer)
	client.networkQuality.queuePeerNotifications(7)

	client.networkQuality.stateLock.Lock()
	pending, found := client.networkQuality.pendingPeerNotifications[peerId]
	client.networkQuality.stateLock.Unlock()
	if !found || pending.generation != 7 || !pending.peer.transferOptions.Ack ||
		!pending.peer.transferOptions.NetworkPeer || !pending.peer.transferKey.ForceStream {
		t.Fatalf("receive-only provider peer lost its reliable return identity: %+v", pending)
	}
}

func TestClientNetworkQualityRefusedNotificationRemainsPending(t *testing.T) {
	client := newNetworkQualityTestClient(t)
	peerId := NewId()
	client.networkQuality.stateLock.Lock()
	client.networkQuality.pendingPeerNotifications[peerId] = networkQualityPendingPeer{
		peer: networkQualityPeer{
			destinationId: peerId,
		},
		generation: 1,
	}
	client.networkQuality.stateLock.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}

	if retry := client.networkQuality.sendPeerNotifications(); !retry {
		t.Fatal("refused zero-wait notification was not retained for retry")
	}
	client.networkQuality.stateLock.Lock()
	_, retained := client.networkQuality.pendingPeerNotifications[peerId]
	client.networkQuality.stateLock.Unlock()
	if !retained {
		t.Fatal("refused zero-wait notification lost its reliable pending state")
	}
}

func TestClientNetworkQualityWireIsBoundedAndHiddenFromGenericReceive(t *testing.T) {
	client := newNetworkQualityTestClient(t)
	payload := encodeNetworkQualityMessage(NewId(), 1)
	wrapped := wrapSubprotocolBytes(networkQualitySubprotocolId, payload)
	MessagePoolReturn(payload)
	defer MessagePoolReturn(wrapped)
	frame := &protocol.Frame{
		MessageType:  protocol.MessageType_Subprotocol,
		MessageBytes: wrapped,
	}
	if got := MessageByteCount([]*protocol.Frame{frame}); got > DefaultMtu {
		t.Fatalf("quality control frame is %d bytes, mtu=%d", got, DefaultMtu)
	}
	genericCalls := 0
	remove := client.AddReceiveCallback(func(TransferPath, []*protocol.Frame, Peer) { genericCalls++ })
	defer remove()
	client.receive(TransferPath{SourceId: NewId()}, []*protocol.Frame{frame}, Peer{})
	if genericCalls != 0 {
		t.Fatalf("reserved quality frame reached %d generic callbacks", genericCalls)
	}
}

func TestClientNetworkQualityPeerMemoryIsBounded(t *testing.T) {
	client := newNetworkQualityTestClient(t)
	firstId := NewId()
	client.observeNetworkQualityPeer(TransferPath{SourceId: firstId}, Peer{})
	for range networkQualityMaximumRememberedPeerCount {
		client.observeNetworkQualityPeer(TransferPath{SourceId: NewId()}, Peer{})
	}
	client.networkQuality.stateLock.Lock()
	observedCount := len(client.networkQuality.observedPeers)
	_, retainedFirst := client.networkQuality.observedPeers[firstId]
	client.networkQuality.stateLock.Unlock()
	if observedCount != networkQualityMaximumRememberedPeerCount || retainedFirst {
		t.Fatalf("observed peer bound failed: count=%d retainedOldest=%t", observedCount, retainedFirst)
	}
}

func TestClientNetworkQualityCloseUnregistersGlobalListener(t *testing.T) {
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	client := NewClient(context.Background(), NewId(), NewNoContractClientOob(), settings)
	state := client.networkQuality
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}

	NetworkQualityChanged()
	state.stateLock.Lock()
	generation := state.localGeneration
	state.stateLock.Unlock()
	if generation != 0 {
		t.Fatalf("closed client received global generation %d", generation)
	}
}
