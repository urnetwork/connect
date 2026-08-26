package connect

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReceiveAckH1HandoffWaitRescuesFullCompactQueue(t *testing.T) {
	settings := DefaultReceiveBufferSettingsWithBufferSize(1)
	settings.H1AckHandoffTimeout = 200 * time.Millisecond
	if got := settings.ackHandoffTimeout(TransportTypeH1); got != 200*time.Millisecond {
		t.Fatalf("H1 ACK handoff timeout = %s, want 200ms", got)
	}
	for _, transportType := range []TransportType{TransportTypeUnknown, TransportTypeH3} {
		if got := settings.ackHandoffTimeout(transportType); got != 0 {
			t.Fatalf("non-H1 ACK handoff timeout = %s, want zero", got)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sequenceId := NewId()
	sequence := &SendSequence{
		ctx:        ctx,
		sequenceId: sequenceId,
		acks:       make(chan receiveAckMessage, 1),
	}
	sequence.acks <- receiveAckMessage{sequenceId: sequenceId, messageId: NewId()}

	result := make(chan receiveAckHandoffResult, 1)
	go func() {
		handoff, _ := sequence.ackMessageDetailed(
			receiveAckMessage{sequenceId: sequenceId, messageId: NewId()},
			settings.ackHandoffTimeout(TransportTypeH1),
		)
		result <- handoff
	}()
	select {
	case premature := <-result:
		t.Fatalf("H1 ACK wait completed before queue space: %v", premature)
	case <-time.After(10 * time.Millisecond):
	}
	<-sequence.acks
	select {
	case got := <-result:
		if got != receiveAckHandoffAcceptedAfterWait {
			t.Fatalf("H1 ACK handoff result = %v, want accepted after wait", got)
		}
		client := &Client{}
		client.recordReceiveAckHandoff(got)
		stats := client.ReceiveStats()
		if stats.AckHandoffWaitCount != 1 || stats.AckHandoffWaitSuccess != 1 ||
			stats.AckHandoffDropCount != 0 {
			t.Fatalf("rescued ACK handoff stats = %+v", stats)
		}
	case <-time.After(time.Second):
		t.Fatal("H1 ACK handoff did not wake after queue space")
	}
}

func TestReceiveAckFullHandoffCoalescesWithoutWaitingOrAllocatingDepth(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sequenceId := NewId()
	olderMessageId := NewId()
	messageId := NewId()
	ackWindow := newSequenceAckWindow()
	resendQueue := newResendQueue(nil, 0)
	resendQueue.Add(&sendItem{transferItem: transferItem{
		messageId:        olderMessageId,
		messageByteCount: 1,
		sequenceNumber:   6,
	}})
	resendQueue.Add(&sendItem{transferItem: transferItem{
		messageId:        messageId,
		messageByteCount: 1,
		sequenceNumber:   7,
	}})
	sequence := &SendSequence{
		ctx:         ctx,
		client:      &Client{},
		sequenceId:  sequenceId,
		acks:        make(chan receiveAckMessage, 1),
		ackWindow:   ackWindow,
		resendQueue: resendQueue,
	}
	sequence.acks <- receiveAckMessage{sequenceId: sequenceId, messageId: olderMessageId}

	start := time.Now()
	handoff, err := sequence.ackMessageDetailed(
		receiveAckMessage{sequenceId: sequenceId, messageId: messageId},
		200*time.Millisecond,
	)
	if err != nil || handoff != receiveAckHandoffAccepted {
		t.Fatalf("coalesced ACK handoff = (%v, %v), want accepted", handoff, err)
	}
	if elapsed := time.Since(start); elapsed >= 100*time.Millisecond {
		t.Fatalf("coalesced ACK handoff waited %s on a full channel", elapsed)
	}
	if len(sequence.acks) != 1 {
		t.Fatalf("coalesced ACK changed compact queue depth to %d, want 1", len(sequence.acks))
	}
	// The worker may process the older queued cumulative ACK after the newer
	// fallback update. The shared window must remain monotonic in that order.
	sequence.coalesceReceivedAck(ackWindow, <-sequence.acks)
	snapshot := ackWindow.Snapshot(true)
	if snapshot.ackUpdateCount != 2 || snapshot.headAck.messageId != messageId ||
		snapshot.headAck.sequenceNumber != 7 {
		t.Fatalf("coalesced ACK snapshot = %+v", snapshot)
	}
}

func TestReceiveAckHandoffClassifiesQueueFullAndMissingSequence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	destination := NewId()
	sequenceId := NewId()
	sequence := &SendSequence{
		ctx:         ctx,
		destination: destination,
		sequenceId:  sequenceId,
		acks:        make(chan receiveAckMessage, 1),
	}
	sequence.acks <- receiveAckMessage{sequenceId: sequenceId, messageId: NewId()}
	buffer := &SendBuffer{
		ctx: ctx,
		log: NewNoopLogger(),
		sendSequencesBySequenceId: map[Id]*SendSequence{
			sequenceId: sequence,
		},
	}
	client := &Client{log: NewNoopLogger()}
	full := buffer.ackMessageDetailed(
		destination,
		receiveAckMessage{sequenceId: sequenceId, messageId: NewId()},
		0,
	)
	client.recordReceiveAckHandoff(full)
	missing := buffer.ackMessageDetailed(
		destination,
		receiveAckMessage{sequenceId: NewId(), messageId: NewId()},
		0,
	)
	client.recordReceiveAckHandoff(missing)
	stats := client.ReceiveStats()
	if full != receiveAckHandoffQueueFull ||
		missing != receiveAckHandoffSequenceMissing ||
		stats.AckHandoffDropCount != 2 ||
		stats.AckHandoffQueueFullCount != 1 ||
		stats.AckHandoffMissCount != 1 {
		t.Fatalf("classified ACK handoff stats = %+v (full=%v missing=%v)", stats, full, missing)
	}
}

func TestReceiveAckRouteWriteStatsSeparateBlockedWaitsAndErrors(t *testing.T) {
	client := &Client{}
	client.recordReceiveAckRouteWrite(2*time.Millisecond, false, true, nil)
	client.recordReceiveAckRouteWrite(5*time.Millisecond, true, false, nil)
	client.recordReceiveAckRouteWrite(7*time.Millisecond, true, false, errors.New("write"))
	stats := client.ReceiveStats()
	if stats.AckRouteWriteCount != 3 ||
		stats.AckRoutePriorityWriteCount != 1 ||
		stats.AckRouteWriteBlockedCount != 2 ||
		stats.AckRouteWriteErrorCount != 1 ||
		stats.AckRouteWriteWaitDuration != 12*time.Millisecond ||
		stats.AckRouteWriteMaxWait != 7*time.Millisecond {
		t.Fatalf("ACK route-write stats = %+v", stats)
	}
}

func BenchmarkReceiveAckRouteWriteStatsImmediate(b *testing.B) {
	client := &Client{}
	b.ReportAllocs()
	for range b.N {
		client.recordReceiveAckRouteWrite(0, false, false, nil)
	}
}

func TestSequenceAckWindowCoalescesReusableNotification(t *testing.T) {
	window := newSequenceAckWindow()
	empty := window.Snapshot(false)

	window.Update(sequenceAck{sequenceNumber: 1, messageId: NewId()})
	select {
	case <-empty.ackNotify:
	case <-time.After(time.Second):
		t.Fatal("ack update did not notify snapshot waiter")
	}

	first := window.Snapshot(true)
	if first.ackUpdateCount != 1 {
		t.Fatalf("first snapshot update count = %d, want 1", first.ackUpdateCount)
	}
	select {
	case <-first.ackNotify:
		t.Fatal("reset left a stale ack notification")
	default:
	}

	window.Update(sequenceAck{sequenceNumber: 2, messageId: NewId()})
	window.Update(sequenceAck{sequenceNumber: 3, messageId: NewId()})
	select {
	case <-first.ackNotify:
	default:
		t.Fatal("coalesced updates did not notify")
	}
	select {
	case <-first.ackNotify:
		t.Fatal("coalesced updates queued more than one notification")
	default:
	}

	coalesced := window.Snapshot(true)
	if coalesced.ackUpdateCount != 2 {
		t.Fatalf("coalesced snapshot update count = %d, want 2", coalesced.ackUpdateCount)
	}
}

func TestSequenceAckWindowSteadyStateDoesNotAllocate(t *testing.T) {
	window := newSequenceAckWindow()
	ack := sequenceAck{
		sequenceNumber: 1,
		messageId:      NewId(),
		tag:            sequenceTag{sendTime: 1_700_000_000_000, set: true},
	}

	allocs := testing.AllocsPerRun(1000, func() {
		window.Update(ack)
		if !window.Pending() {
			t.Fatal("updated ack window is not pending")
		}
		window.Snapshot(true)
	})
	if allocs != 0 {
		t.Fatalf("ack update + reset allocated %.0f times, want 0", allocs)
	}
}

func TestSequenceAckWindowPendingDoesNotCloneSelectiveAcks(t *testing.T) {
	window := newSequenceAckWindow()
	for i := range 64 {
		window.Update(sequenceAck{
			sequenceNumber: uint64(i + 2),
			messageId:      NewId(),
			selective:      true,
		})
	}

	allocs := testing.AllocsPerRun(1000, func() {
		if !window.Pending() {
			t.Fatal("selective ack window is not pending")
		}
	})
	if allocs != 0 {
		t.Fatalf("pending check allocated %.0f times, want 0", allocs)
	}
	if got := len(window.Snapshot(true).selectiveAcks); got != 64 {
		t.Fatalf("selective snapshot size = %d, want 64", got)
	}
	if window.Pending() {
		t.Fatal("reset ack window remained pending")
	}
}

func TestSequenceAckWindowPreservesCompactRecoveryCapability(t *testing.T) {
	window := newSequenceAckWindow()
	window.Update(sequenceAck{
		sequenceNumber:                   1,
		messageId:                        NewId(),
		compactContractRecoverySupported: true,
	})
	window.Update(sequenceAck{
		sequenceNumber: 2,
		messageId:      NewId(),
	})

	snapshot := window.Snapshot(true)
	if snapshot.ackUpdateCount != 2 ||
		!snapshot.headAck.compactContractRecoverySupported {
		t.Fatalf("coalesced capability snapshot=%+v", snapshot)
	}
}

func TestSequenceAckWindowTracksCarrierOfLatestAckTrigger(t *testing.T) {
	window := newSequenceAckWindow()
	window.Update(sequenceAck{
		sequenceNumber: 1,
		messageId:      NewId(),
		transportType:  TransportTypeH1,
	})
	window.Update(sequenceAck{
		sequenceNumber: 2,
		messageId:      NewId(),
		transportType:  TransportTypeH3,
	})
	if got := window.Snapshot(true).headAck.transportType; got != TransportTypeH3 {
		t.Fatalf("coalesced head carrier=%s, want H3", got)
	}

	// A retransmit below the cumulative head causes that head ACK to be sent
	// again, but cannot move an ACK covering newer H3 data onto the old H1 lane.
	window.Update(sequenceAck{
		sequenceNumber: 1,
		messageId:      NewId(),
		transportType:  TransportTypeH1,
	})
	if got := window.Snapshot(true).headAck.transportType; got != TransportTypeH3 {
		t.Fatalf("retransmit-triggered head carrier=%s, want preserved H3", got)
	}

	selectiveId := NewId()
	window.Update(sequenceAck{
		sequenceNumber: 3,
		messageId:      selectiveId,
		selective:      true,
		transportType:  TransportTypeH3,
	})
	window.Update(sequenceAck{
		sequenceNumber: 3,
		messageId:      selectiveId,
		selective:      true,
		transportType:  TransportTypeUnknown,
	})
	snapshot := window.Snapshot(true)
	if got := snapshot.selectiveAcks[selectiveId].transportType; got != TransportTypeH3 {
		t.Fatalf("selective ACK carrier=%s, want preserved H3", got)
	}
}
