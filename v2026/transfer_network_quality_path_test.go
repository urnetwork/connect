// A hard network change starts a new quality measurement even inside the
// quiet interval a signal hint just started: the path that measurement was
// sizing is gone. Hints on one path still coalesce.
package connect

import (
	"testing"
	"time"
)

func TestNetworkChangedAdvancesThePathGeneration(t *testing.T) {
	before := networkPathGeneration.Load()
	NetworkQualityChanged()
	if networkPathGeneration.Load() != before {
		t.Fatal("a quality hint advanced the network path generation")
	}
	NetworkChanged()
	if networkPathGeneration.Load() <= before {
		t.Fatal("a hard network change did not advance the network path generation")
	}
}

func TestHardChangeStartsALocalQualityGenerationInsideTheQuietInterval(t *testing.T) {
	client := newNetworkQualityTestClient(t)
	state := client.networkQuality
	read := func() (generation uint64, pathGeneration uint64) {
		state.stateLock.Lock()
		defer state.stateLock.Unlock()
		return state.localGeneration, state.lastLocalPathGeneration
	}
	at := time.Unix(1700000000, 0)
	state.dispatchLocal(at, 0)
	state.dispatchLocal(at.Add(time.Second), 0)
	if generation, _ := read(); generation != 1 {
		t.Fatalf("a hint inside the quiet interval started generation %d", generation)
	}
	state.dispatchLocal(at.Add(2*time.Second), 1)
	if generation, pathGeneration := read(); generation != 2 || pathGeneration != 1 {
		t.Fatalf("a hard change inside the quiet interval: generation=%d path=%d, want 2 and 1", generation, pathGeneration)
	}
	state.dispatchLocal(at.Add(3*time.Second), 1)
	if generation, _ := read(); generation != 2 {
		t.Fatalf("a hint on the new path renewed the generation: %d", generation)
	}
}

func TestHardChangeStartsASequenceMeasurementInsideTheQuietInterval(t *testing.T) {
	sequence, at := newRetainedWindowFixture(t)
	sequence.networkQualityChangedOnPath(at, 0)
	sequence.networkQualityChangedOnPath(at.Add(time.Second), 0)
	if sequence.windowQualityAfterNanos != at.UnixNano() {
		t.Fatal("a hint inside the quiet interval started a new measurement")
	}
	hard := at.Add(2 * time.Second)
	sequence.networkQualityChangedOnPath(hard, 1)
	if sequence.windowQualityAfterNanos != hard.UnixNano() || sequence.windowSize.remeasureUntil != hard.Add(sequence.sendBufferSettings.WindowQualityRemeasureInterval) {
		t.Fatal("a hard change inside the quiet interval did not start a new measurement")
	}
	sequence.networkQualityChangedOnPath(hard.Add(time.Second), 1)
	if sequence.windowQualityAfterNanos != hard.UnixNano() {
		t.Fatal("a hint on the new path renewed the measurement")
	}
}

func TestHardChangeResetsASharedServiceOnceForEverySibling(t *testing.T) {
	at := time.Unix(1700000000, 0)
	peer := NewId()
	shared := newWindowPacingService(DefaultSendBufferSettings())
	one := &SendSequence{windowPacer: windowBurstPacer{service: shared}}
	two := &SendSequence{windowPacer: windowBurstPacer{service: shared}}
	buffer := &SendBuffer{sendSequences: map[sendSequenceId]*SendSequence{
		{Destination: peer}: one, {Destination: peer, LogicalLane: 1}: two,
	}}
	buffer.networkQualityChangedOnPath(Id{}, at, 0)
	buffer.networkQualityChangedOnPath(Id{}, at.Add(time.Second), 0)
	if !shared.qualityChangedAt.Equal(at) {
		t.Fatal("a hint inside the quiet interval reset the shared service")
	}

	shared.observeRoundTrip(time.Millisecond, 0, at.Add(1500*time.Millisecond))
	hard := at.Add(2 * time.Second)
	buffer.networkQualityChangedOnPath(Id{}, hard, 1)
	if !shared.qualityChangedAt.Equal(hard) || !shared.qualityRoundTripPending {
		t.Fatal("a hard change inside the quiet interval did not reset the shared service")
	}
	if one.windowQualityAfterNanos != hard.UnixNano() || two.windowQualityAfterNanos != hard.UnixNano() {
		t.Fatal("a sibling did not take the hard change's boundary")
	}

	// the second sibling's call carried the same generation and coalesced into
	// the first one's reset; so does a later hint on the new path
	shared.observeRoundTrip(time.Millisecond, 0, hard.Add(500*time.Millisecond))
	buffer.networkQualityChangedOnPath(Id{}, hard.Add(time.Second), 1)
	if !shared.qualityChangedAt.Equal(hard) || shared.qualityRoundTripPending {
		t.Fatal("the same path generation reset the shared service again")
	}
}

// End to end: a client that took a hint takes the hard change a second later.
func TestNetworkChangedStartsAClientQualityGenerationAfterAHint(t *testing.T) {
	client := newNetworkQualityTestClient(t)
	state := client.networkQuality
	read := func() uint64 {
		state.stateLock.Lock()
		defer state.stateLock.Unlock()
		return state.localGeneration
	}
	NetworkQualityChanged()
	afterHint := read()
	NetworkQualityChanged()
	if read() != afterHint {
		t.Fatal("a second hint inside the quiet interval started a generation")
	}
	NetworkChanged()
	if read() != afterHint+1 {
		t.Fatal("a hard change after a hint did not start a client generation")
	}
}

// Through the client's worker: the hard change's path generation reaches its
// send sequences, so they start a new measurement inside the quiet interval
// too, not only the client's own generation.
func TestHardChangeReachesTheClientsSequencesInsideTheQuietInterval(t *testing.T) {
	client := newNetworkQualityTestClient(t)
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
	defer func() {
		client.sendBuffer.mutex.Lock()
		delete(client.sendBuffer.sendSequences, sendSequenceId{Destination: destinationId})
		client.sendBuffer.mutex.Unlock()
		sequence.resendQueue.Clear()
	}()
	state := client.networkQuality
	applied := make(chan uint64, 4)
	state.afterLocalApplyForTest = func(generation uint64) { applied <- generation }

	at := time.Unix(1700000000, 0)
	state.dispatchLocal(at, 0)
	awaitNetworkQualityTestValue(t, applied)
	if sequence.windowQualityAfterNanos != at.UnixNano() {
		t.Fatal("the first notification did not reach the sequence")
	}
	hard := at.Add(2 * time.Second)
	state.dispatchLocal(hard, 1)
	awaitNetworkQualityTestValue(t, applied)
	if sequence.windowQualityAfterNanos != hard.UnixNano() {
		t.Fatalf("the hard change did not start the sequence's measurement: %d, want %d",
			sequence.windowQualityAfterNanos, hard.UnixNano())
	}
}
