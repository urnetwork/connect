package connect

import (
	"testing"

	"github.com/quic-go/quic-go/qlog"
)

// Retransmission adds roots the writer never admitted. quic-go requeues a lost
// STREAM frame and, when its retransmission packet carries more ACK or control
// bytes than the original packet did, sends the prefix that fits as a second
// pooled root. Admission stopped at the write limit with room for only a third
// of such splits, so a whole lost window retransmitted as splits filled the
// root table and failed the healthy connection closed. Every root the writer
// admits must leave room for its one split: the writer blocks, the connection
// stays open, and each ACK releases its root.
func TestQuicSendFlightLostWindowSplitOnceStaysOpen(t *testing.T) {
	flight := newQuicSendFlight()
	stream := &flightTestStream{ctx: t.Context()}
	flight.newWriter(stream)
	const length = 1200
	window := quicSendFlightWriteLimit
	for i := range window {
		flightSent(flight, int64(i), &qlog.StreamFrame{StreamID: 0, Offset: int64(i * length), Length: length})
	}
	if got := flight.snapshot(); got.Frames+got.PendingFrames < quicSendFlightWriteLimit {
		t.Fatalf("the writer was not at its admission limit: %+v", got)
	}
	for i := range window {
		flight.lost(qlog.PacketLost{Header: qlog.PacketHeader{PacketType: qlog.PacketType1RTT, PacketNumber: qlog.PacketNumber(i)}})
	}
	// Every retransmission packet now carries an ACK frame its original did
	// not, so only a prefix of each lost root fits: one more root apiece.
	const prefix = length - 40
	for i := range window {
		flightSent(flight, int64(window+i), &qlog.StreamFrame{StreamID: 0, Offset: int64(i * length), Length: prefix})
	}
	if got := flight.snapshot(); got.Failed || got.Frames != 2*window || stream.cancelled.Load() != 0 {
		t.Fatalf("a lost window retransmitted as splits failed the connection closed: %+v canceled=%d", got, stream.cancelled.Load())
	}
	if got := flight.snapshot(); got.Frames+got.PendingFrames < quicSendFlightWriteLimit {
		t.Fatalf("the writer was admitted during recovery: %+v", got)
	}
	// The suffixes follow in later packets, and the ACKs release every root.
	for i := range window {
		flightSent(flight, int64(2*window+i), &qlog.StreamFrame{StreamID: 0, Offset: int64(i*length + prefix), Length: length - prefix})
	}
	flightAck(flight, int64(window), int64(3*window-1))
	if got := flight.snapshot(); got.Frames != 0 || got.Bytes != 0 || got.Failed {
		t.Fatalf("ACKs did not release the split roots: %+v", got)
	}
}
