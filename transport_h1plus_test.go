package connect

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func h1PlusOwnedTestMessage(label byte, size int) []byte {
	message := MessagePoolGet(size)
	for i := range message {
		message[i] = label
	}
	return message
}

func h1PlusDrainTestOwnedMessages(channel <-chan []byte) {
	for {
		select {
		case message, open := <-channel:
			if !open {
				return
			}
			MessagePoolReturn(message)
		default:
			return
		}
	}
}

func TestWriteH1FramedReadyBatchAckFairnessAndOwnership(t *testing.T) {
	baseline := MessagePoolOutstandingCount()
	priority := make(chan []byte, 40)
	ordinary := make(chan []byte, 4)
	defer h1PlusDrainTestOwnedMessages(priority)
	defer h1PlusDrainTestOwnedMessages(ordinary)
	for i := range 40 {
		priority <- h1PlusOwnedTestMessage(byte(i+1), 64)
	}
	for i := range 4 {
		ordinary <- h1PlusOwnedTestMessage(byte(0xa0+i), 64)
	}
	raw := newH1UpgradeScriptConn(nil)
	conn, err := NewFramedMessageConn(raw, H1FramerProtocol, 1200, nil)
	if err != nil {
		t.Fatal(err)
	}
	sent := 0
	open, err := writeH1FramedReadyBatch(context.Background(), conn, ordinary, priority, h1PlusOwnedTestMessage(0, 64), true, time.Second, func() { sent++ })
	if err != nil || !open || sent != platformWebSocketWriteBatchMaxMessages || raw.writeCalls != 1 {
		t.Fatalf("ready flush: open=%v err=%v sent=%d writes=%d", open, err, sent, raw.writeCalls)
	}
	// The first ACK counts toward its eight-message quantum. The complete
	// flush is ACK[0:8],ordinary0,ACK[8:16],ordinary1,ACK[16:24],ordinary2,
	// ACK[24:29], keeping ordinary turns inside the same physical bulk write.
	var want []byte
	ack := byte(0)
	for i := range platformWebSocketWriteBatchMaxMessages {
		if i == 8 || i == 17 || i == 26 {
			want = append(want, byte(0xa0+i/9))
		} else {
			want = append(want, ack)
			ack++
		}
	}
	reader := bytes.NewReader(raw.output.Bytes())
	framer := NewFramer(DefaultFramerSettings(1200))
	for i, label := range want {
		message, err := framer.Read(reader)
		if err != nil {
			t.Fatal(err)
		}
		match := len(message) == 64 && bytes.Equal(message, bytes.Repeat([]byte{label}, 64))
		MessagePoolReturn(message)
		if !match {
			t.Fatalf("bulk position %d violated ACK fairness; want label %d", i, label)
		}
	}
	if reader.Len() != 0 || len(priority) != 12 || len(ordinary) != 1 {
		t.Fatalf("collector exceeded32-message bound: unreadwire=%d priority=%d ordinary=%d", reader.Len(), len(priority), len(ordinary))
	}
	h1PlusDrainTestOwnedMessages(priority)
	h1PlusDrainTestOwnedMessages(ordinary)
	if MessagePoolOutstandingCount() != baseline {
		t.Fatal("bulk flush failed to release input ownership")
	}
}

func TestWriteH1FramedReadyBatchByteBoundAndReadyOnly(t *testing.T) {
	baseline := MessagePoolOutstandingCount()
	// Three tunnel-MTU packets per established H1 Transfer message. The drain
	// takes messages until the batch reaches the byte bound.
	const dataByteCount = 3 * DefaultMtu
	want := 1
	for platformWebSocketWriteBatchCanDrain(want, want*dataByteCount) {
		want++
	}
	queued := want + 8
	ordinary := make(chan []byte, queued)
	defer h1PlusDrainTestOwnedMessages(ordinary)
	for i := range queued {
		ordinary <- h1PlusOwnedTestMessage(byte(i+1), dataByteCount)
	}
	raw := newH1UpgradeScriptConn(nil)
	conn, err := NewFramedMessageConn(raw, H1FramerProtocol, 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetWriteBatchByteLimit(platformH1WriteBatchMaxByteCount)
	sent := 0
	_, err = writeH1FramedReadyBatch(context.Background(), conn, ordinary, nil, h1PlusOwnedTestMessage(0, dataByteCount), false, time.Second, func() { sent++ })
	wire := want * (dataByteCount + 4)
	if err != nil || sent != want || raw.writeCalls != 1 || len(ordinary) != queued+1-want ||
		raw.output.Len() != wire || platformH1WriteBatchMaxByteCount < wire {
		t.Fatalf("3-packet ready batch left the drain bound or one write: sent=%d/%d writes=%d queued=%d wire=%d err=%v", sent, want, raw.writeCalls, len(ordinary), raw.output.Len(), err)
	}
	if framedMessageWriteStorageByteCount < wire && len(conn.storage) != platformH1WriteBatchMaxByteCount {
		t.Fatalf("bulk batch scratch=%d, want the %d-byte write bound", len(conn.storage), platformH1WriteBatchMaxByteCount)
	}
	h1PlusDrainTestOwnedMessages(ordinary)
	// An open, empty lane must not delay a singleton while trying to fill a
	// batch. Completion on this call has no producer or channel close to wake it.
	empty := make(chan []byte)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, err := writeH1FramedReadyBatch(ctx, conn, empty, nil, h1PlusOwnedTestMessage(0x81, 1200), false, time.Second, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("singleton waited for a future packet")
	}
	if raw.writeCalls != 2 || MessagePoolOutstandingCount() != baseline {
		t.Fatal("singleton lost bounded write or message ownership")
	}
}

func TestWriteH1FramedReadyBatchCancellationFailureAndClosedLane(t *testing.T) {
	for _, mode := range []string{"canceled", "short-write", "write-error", "closed-lane", "ignored-control"} {
		t.Run(mode, func(t *testing.T) {
			baseline := MessagePoolOutstandingCount()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ordinary := make(chan []byte, 4)
			defer h1PlusDrainTestOwnedMessages(ordinary)
			raw := newH1UpgradeScriptConn(nil)
			conn, err := NewFramedMessageConn(raw, H1FramerProtocol, 1200, nil)
			if err != nil {
				t.Fatal(err)
			}
			first := h1PlusOwnedTestMessage(0x41, 1200)
			for range 4 {
				ordinary <- h1PlusOwnedTestMessage(0x42, 1200)
			}
			var wantErr error
			switch mode {
			case "canceled":
				cancel()
			case "short-write":
				raw.writeLimit = 5
				wantErr = io.ErrShortWrite
			case "write-error":
				wantErr = errors.New("synthetic write failure")
				raw.writeErr = wantErr
			case "closed-lane":
				close(ordinary)
			case "ignored-control":
				MessagePoolReturn(first)
				first = MessagePoolGet(16)
				h1PlusDrainTestOwnedMessages(ordinary)
			}
			sent := 0
			open, err := writeH1FramedReadyBatch(ctx, conn, ordinary, nil, first, false, time.Second, func() { sent++ })
			if !errors.Is(err, wantErr) {
				t.Fatalf("err=%v want=%v", err, wantErr)
			}
			if (mode == "canceled" || mode == "closed-lane") && open {
				t.Fatal("canceled/closed lane reported open")
			}
			if mode == "canceled" || mode == "ignored-control" {
				if sent != 0 || raw.writeCalls != 0 {
					t.Fatal("cancellation/ignored short control emitted application bytes")
				}
			} else if wantErr != nil && (sent != 0 || !raw.closed.Load()) {
				t.Fatal("failed batch reported sent messages or kept partial stream open")
			}
			h1PlusDrainTestOwnedMessages(ordinary)
			if MessagePoolOutstandingCount() != baseline {
				t.Fatal("termination leaked queued/owned messages")
			}
		})
	}
}

// H1+ is opt-out: the default platform settings try the custom upgrade, with
// or without an explicit owner memory target.
func TestDefaultPlatformTransportSettingsEnableH1Plus(t *testing.T) {
	if !DefaultPlatformTransportSettings().EnableH1Plus {
		t.Fatal("default platform settings opted out of H1+")
	}
	if !DefaultPlatformTransportSettingsWithMemoryTarget(mib(64)).EnableH1Plus {
		t.Fatal("memory-targeted platform settings opted out of H1+")
	}
}
