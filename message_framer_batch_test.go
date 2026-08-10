// This file verifies that stream batching preserves the established Framer
// wire format, bounds malformed input, and performs one underlying write.
package connect

import (
	"bytes"
	"errors"
	"testing"
)

// One recording writer counts complete stream handoffs.
type framerBatchTestWriter struct {
	bytes.Buffer
	writeCount int
}

// Write records one underlying handoff while retaining its bytes.
func (self *framerBatchTestWriter) Write(buffer []byte) (int, error) {
	self.writeCount += 1
	return self.Buffer.Write(buffer)
}

// TestFramerWriteBatchPreservesMessages verifies compatibility with the
// existing singular reader and exactly one underlying stream write.
func TestFramerWriteBatchPreservesMessages(t *testing.T) {
	settings := DefaultFramerSettings(64 * 1024)
	framer := NewFramer(settings)
	writer := &framerBatchTestWriter{}
	messages := [][]byte{
		[]byte("one"),
		bytes.Repeat([]byte{0x22}, 2048),
		[]byte("three"),
	}
	if err := framer.WriteBatch(writer, messages); err != nil {
		t.Fatal(err)
	}
	if writer.writeCount != 1 {
		t.Fatalf("write count=%d want=1", writer.writeCount)
	}
	for messageIndex, expected := range messages {
		actual, err := framer.Read(&writer.Buffer)
		if err != nil {
			t.Fatalf("read message %d: %s", messageIndex, err)
		}
		if !bytes.Equal(actual, expected) {
			MessagePoolReturn(actual)
			t.Fatalf("message %d changed", messageIndex)
		}
		MessagePoolReturn(actual)
	}
	if writer.Buffer.Len() != 0 {
		t.Fatalf("unread wire bytes=%d", writer.Buffer.Len())
	}
}

// TestFramerWriteBatchRejectsOversizedMessage verifies validation occurs
// before any prefix from the batch reaches the stream.
func TestFramerWriteBatchRejectsOversizedMessage(t *testing.T) {
	framer := NewFramer(DefaultFramerSettings(8))
	writer := &framerBatchTestWriter{}
	err := framer.WriteBatch(writer, [][]byte{
		[]byte("small"),
		bytes.Repeat([]byte{0x33}, 9),
	})
	if err == nil {
		t.Fatal("oversized batch was accepted")
	}
	if writer.writeCount != 0 || writer.Buffer.Len() != 0 {
		t.Fatalf(
			"rejected batch wrote count=%d bytes=%d",
			writer.writeCount,
			writer.Buffer.Len(),
		)
	}
}

// framerFullWriteErrorWriter models the legal io.Writer result that consumes
// every byte while still reporting a terminal error.
type framerFullWriteErrorWriter struct {
	err error
}

// Write reports the configured error together with full byte progress.
func (self *framerFullWriteErrorWriter) Write(buffer []byte) (int, error) {
	return len(buffer), self.err
}

// TestFramerWritesPreserveFullProgressError ensures batching does not hide a
// stream failure merely because the final write also consumed its bytes.
func TestFramerWritesPreserveFullProgressError(t *testing.T) {
	writeErr := errors.New("injected full-progress write failure")
	writer := &framerFullWriteErrorWriter{err: writeErr}
	framer := NewFramer(DefaultFramerSettings(1024))
	if err := framer.Write(writer, []byte("one")); !errors.Is(err, writeErr) {
		t.Fatalf("singular write error=%v", err)
	}
	if err := framer.WriteBatch(writer, [][]byte{
		[]byte("one"),
		[]byte("two"),
	}); !errors.Is(err, writeErr) {
		t.Fatalf("batch write error=%v", err)
	}
}
