package connect

import "testing"

func newTcpAckOrderingTestSequence() *TcpSequence {
	return &TcpSequence{
		ConnectionState: ConnectionState{
			sendSeq:             200,
			receiveSeq:          2000,
			receiveSeqAck:       1000,
			receiveWindowSize:   1024,
			receiveWindowEnd:    2024,
			receiveWindowEndSet: true,
		},
	}
}

func TestTcpSequenceAcceptsPureAckAheadOfDeliveredUpload(t *testing.T) {
	sequence := newTcpAckOrderingTestSequence()
	drop, updated := sequence.applySendItemWithLock(&parsedTcp{
		seq:        300,
		ack:        true,
		ackNumber:  1500,
		windowSize: 4096,
	})
	if drop {
		t.Fatal("pure ACK ahead of delivered upload was discarded")
	}
	if !updated || sequence.receiveSeqAck != 1500 || sequence.receiveWindowSize != 4096 {
		t.Fatalf(
			"ahead ACK did not advance return window: updated=%t ack=%d window=%d",
			updated,
			sequence.receiveSeqAck,
			sequence.receiveWindowSize,
		)
	}
}

func TestTcpSequenceAcceptsPureAckBehindDeliveredUpload(t *testing.T) {
	sequence := newTcpAckOrderingTestSequence()
	drop, updated := sequence.applySendItemWithLock(&parsedTcp{
		seq:        100,
		ack:        true,
		ackNumber:  1500,
		windowSize: 4096,
	})
	if drop {
		t.Fatal("pure ACK behind delivered upload was discarded")
	}
	if !updated || sequence.receiveSeqAck != 1500 || sequence.receiveWindowSize != 4096 {
		t.Fatalf(
			"delayed ACK did not advance return window: updated=%t ack=%d window=%d",
			updated,
			sequence.receiveSeqAck,
			sequence.receiveWindowSize,
		)
	}
}

func TestTcpSequenceRejectsAckBeyondEmittedReturnData(t *testing.T) {
	sequence := newTcpAckOrderingTestSequence()
	drop, updated := sequence.applySendItemWithLock(&parsedTcp{
		seq:        200,
		ack:        true,
		ackNumber:  2001,
		windowSize: 4096,
	})
	if drop {
		t.Fatal("future pure ACK should be ignored without dropping its packet")
	}
	if updated || sequence.receiveSeqAck != 1000 || sequence.receiveWindowSize != 1024 {
		t.Fatalf(
			"future ACK changed return window: updated=%t ack=%d window=%d",
			updated,
			sequence.receiveSeqAck,
			sequence.receiveWindowSize,
		)
	}
}

func TestTcpSequenceDropsOutOfOrderPayloadButAppliesValidAck(t *testing.T) {
	sequence := newTcpAckOrderingTestSequence()
	drop, updated := sequence.applySendItemWithLock(&parsedTcp{
		seq:        201,
		ack:        true,
		ackNumber:  1500,
		windowSize: 4096,
		payload:    []byte{1},
	})
	if !drop {
		t.Fatal("out-of-order payload was accepted")
	}
	if !updated || sequence.receiveSeqAck != 1500 || sequence.receiveWindowSize != 4096 {
		t.Fatal("valid ACK field on out-of-order payload did not advance return window")
	}
}

func TestTcpSequenceZeroWindowAdvancesToExistingRightEdge(t *testing.T) {
	sequence := newTcpAckOrderingTestSequence()
	sequence.receiveWindowSize = 500
	sequence.receiveWindowEnd = 1500
	drop, updated := sequence.applySendItemWithLock(&parsedTcp{
		seq:        200,
		ack:        true,
		ackNumber:  1500,
		windowSize: 0,
	})
	if drop {
		t.Fatal("zero-window ACK was discarded")
	}
	if !updated || sequence.receiveSeqAck != 1500 || sequence.receiveWindowSize != 0 {
		t.Fatalf(
			"zero-window ACK state: updated=%t ack=%d window=%d",
			updated,
			sequence.receiveSeqAck,
			sequence.receiveWindowSize,
		)
	}
}

func TestTcpSequenceReopensWindowAtSameAck(t *testing.T) {
	sequence := newTcpAckOrderingTestSequence()
	sequence.receiveSeqAck = 1500
	sequence.receiveWindowSize = 0
	sequence.receiveWindowEnd = 1500
	drop, updated := sequence.applySendItemWithLock(&parsedTcp{
		seq:        200,
		ack:        true,
		ackNumber:  1500,
		windowSize: 4096,
	})
	if drop {
		t.Fatal("window-reopen ACK was discarded")
	}
	if !updated || sequence.receiveWindowSize != 4096 || sequence.receiveWindowEnd != 5596 {
		t.Fatalf(
			"window-reopen state: updated=%t window=%d end=%d",
			updated,
			sequence.receiveWindowSize,
			sequence.receiveWindowEnd,
		)
	}
}

func TestTcpSequenceStaleDuplicateCannotShrinkReopenedWindow(t *testing.T) {
	sequence := newTcpAckOrderingTestSequence()
	sequence.receiveSeqAck = 1500
	sequence.receiveWindowSize = 4096
	sequence.receiveWindowEnd = 5596
	drop, updated := sequence.applySendItemWithLock(&parsedTcp{
		seq:        100,
		ack:        true,
		ackNumber:  1500,
		windowSize: 0,
	})
	if drop {
		t.Fatal("stale pure ACK should be ignored without dropping its packet")
	}
	if updated || sequence.receiveWindowSize != 4096 || sequence.receiveWindowEnd != 5596 {
		t.Fatalf(
			"stale window update changed reopened edge: updated=%t window=%d end=%d",
			updated,
			sequence.receiveWindowSize,
			sequence.receiveWindowEnd,
		)
	}
}

func TestTcpSequenceSynWindowIsLiteralBeforeScalingBegins(t *testing.T) {
	sequence := newTcpAckOrderingTestSequence()
	sequence.tcpBufferSettings = &TcpBufferSettings{
		MaxWindowSize: 1 << 20,
	}
	tcp := &parsedTcp{
		seq:        9000,
		windowSize: 16384,
		options:    []byte{2, 4, 0x05, 0xb4, 1, 3, 3, 5},
	}
	sequence.mutex.Lock()
	sequence.initializeSynWithLock(tcp)
	sequence.mutex.Unlock()

	if !sequence.enableWindowScale || sequence.receiveWindowScale != 5 {
		t.Fatalf("window scale negotiation enabled=%t scale=%d, want true and 5", sequence.enableWindowScale, sequence.receiveWindowScale)
	}
	if sequence.receiveWindowSize != 16384 {
		t.Fatalf("SYN window became %d, want literal 16384 before scaling", sequence.receiveWindowSize)
	}
	if sequence.receiveWindowEnd != 9000+16384 {
		t.Fatalf("SYN window right edge=%d, want %d", sequence.receiveWindowEnd, 9000+16384)
	}
	if sequence.sendSeq != 9001 || sequence.receiveSeqAck != 9000 {
		t.Fatalf("SYN sequence state send=%d receive_ack=%d, want 9001 and 9000", sequence.sendSeq, sequence.receiveSeqAck)
	}
}

func TestTcpSequenceSynWindowScaleIsClampedToProtocolMaximum(t *testing.T) {
	sequence := newTcpAckOrderingTestSequence()
	sequence.tcpBufferSettings = &TcpBufferSettings{
		MaxWindowSize: 1 << 20,
	}
	tcp := &parsedTcp{
		seq:        100,
		windowSize: 4096,
		options:    []byte{3, 3, 255},
	}
	sequence.mutex.Lock()
	sequence.initializeSynWithLock(tcp)
	sequence.mutex.Unlock()

	if !sequence.enableWindowScale || sequence.receiveWindowScale != 14 {
		t.Fatalf("window scale negotiation enabled=%t scale=%d, want true and 14", sequence.enableWindowScale, sequence.receiveWindowScale)
	}
	if sequence.receiveWindowSize != 4096 {
		t.Fatalf("clamped scale changed literal SYN window to %d", sequence.receiveWindowSize)
	}
}
