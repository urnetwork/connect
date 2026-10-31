package connect

import (
	"context"
	"errors"
	"testing"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/qlog"
)

type flightResetTestStream struct {
	flightTestStream
	id quic.StreamID
}

func (s *flightResetTestStream) StreamID() quic.StreamID { return s.id }

func flightStop(flight *quicSendFlight, stream quic.StreamID) {
	flight.received(qlog.PacketReceived{
		Header: qlog.PacketHeader{PacketType: qlog.PacketType1RTT},
		Frames: []qlog.Frame{{Frame: &qlog.StopSendingFrame{StreamID: stream, ErrorCode: 0x100}}},
	})
}

func flightReset(flight *quicSendFlight, packet int64, stream quic.StreamID, extra ...qlog.Frame) {
	flight.sent(qlog.PacketSent{
		Header: qlog.PacketHeader{PacketType: qlog.PacketType1RTT, PacketNumber: qlog.PacketNumber(packet)},
		Frames: append([]qlog.Frame{{Frame: &qlog.ResetStreamFrame{StreamID: stream, ErrorCode: 0x100}}}, extra...),
	})
}

func assertFlightFinished(t *testing.T, flight *quicSendFlight, stream quic.StreamID, want bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := flight.waitFinished(ctx, stream)
	if want && err != nil || !want && !errors.Is(err, context.Canceled) {
		t.Fatalf("stream %d finished=%v: %v, ownership=%+v", stream, want, err, flight.snapshot())
	}
}

func TestQuicSendFlightResetRetainsPacketRootsUntilAckOrLoss(t *testing.T) {
	for _, release := range []string{"ack", "loss"} {
		t.Run(release, func(t *testing.T) {
			flight := newQuicSendFlight()
			flight.newWriter(&flightResetTestStream{flightTestStream: flightTestStream{ctx: t.Context()}, id: 0})
			flight.newWriter(&flightResetTestStream{flightTestStream: flightTestStream{ctx: t.Context()}, id: 4})
			flightSent(flight, 10, &qlog.StreamFrame{StreamID: 0, Length: 10})
			flightSent(flight, 11, &qlog.StreamFrame{StreamID: 0, Offset: 10, Length: 20})
			flightSent(flight, 12, &qlog.StreamFrame{StreamID: 4, Length: 30, Fin: true})
			flight.lost(qlog.PacketLost{Header: qlog.PacketHeader{PacketType: qlog.PacketType1RTT, PacketNumber: 11}})
			flightStop(flight, 0)
			if got := flight.snapshot(); got.Frames != 2 || got.Bytes != 40 || got.Failed {
				t.Fatalf("STOP must release only the queued, abandoned root: %+v", got)
			}
			flightReset(flight, 13, 0)
			flightAck(flight, 13, 13)
			assertFlightFinished(t, flight, 0, false)
			if got := flight.snapshot(); got.Frames != 2 || got.Bytes != 40 {
				t.Fatalf("RESET ACK released packet-owned STREAM roots: %+v", got)
			}
			if release == "ack" {
				flightAck(flight, 10, 10)
			} else {
				flight.lost(qlog.PacketLost{Header: qlog.PacketHeader{PacketType: qlog.PacketType1RTT, PacketNumber: 10}})
			}
			flightAck(flight, 11, 11) // a stale ACK owns no queued or sibling root
			if got := flight.snapshot(); got.Frames != 1 || got.Bytes != 30 {
				t.Fatalf("abandoned packet retirement changed a sibling owner: %+v", got)
			}
			flightAck(flight, 12, 12)
			assertFlightFinished(t, flight, 0, true)
		})
	}
}

func TestQuicSendFlightResetLossRetransmissionAndLateAck(t *testing.T) {
	for _, loss := range []string{"explicit", "silent-pto"} {
		t.Run(loss, func(t *testing.T) {
			flight := newQuicSendFlight()
			flight.newWriter(&flightResetTestStream{flightTestStream: flightTestStream{ctx: t.Context()}, id: 0})
			flight.newWriter(&flightResetTestStream{flightTestStream: flightTestStream{ctx: t.Context()}, id: 4})
			flightStop(flight, 0)
			flightReset(flight, 20, 0, qlog.Frame{Frame: &qlog.StreamFrame{StreamID: 4, Length: 30, Fin: true}})
			if loss == "explicit" {
				flight.lost(qlog.PacketLost{Header: qlog.PacketHeader{PacketType: qlog.PacketType1RTT, PacketNumber: 20}})
			}
			flightReset(flight, 21, 0)
			flightAck(flight, 20, 20)
			assertFlightFinished(t, flight, 0, false)
			if got := flight.snapshot(); got.Frames != 1 || got.Bytes != 30 {
				t.Fatalf("late RESET packet ACK released its retransmission sibling: %+v", got)
			}
			flightSent(flight, 22, &qlog.StreamFrame{StreamID: 4, Length: 30, Fin: true})
			flightAck(flight, 22, 22)
			assertFlightFinished(t, flight, 0, false) // the current reset is still unacknowledged
			flightAck(flight, 21, 21)
			assertFlightFinished(t, flight, 0, true)
		})
	}
}

func TestQuicSendFlightResetReusesWriterSlots(t *testing.T) {
	flight := newQuicSendFlight()
	for generation := range 1024 {
		stream := quic.StreamID(generation * 4)
		packet := int64(generation * 2)
		writer := flight.newWriter(&flightResetTestStream{flightTestStream: flightTestStream{ctx: t.Context()}, id: stream})
		flightSent(flight, packet, &qlog.StreamFrame{StreamID: stream, Length: 1})
		flightStop(flight, stream)
		flightReset(flight, packet+1, stream)
		if packet > 0 {
			flightAck(flight, packet-2, packet-1)
		}
		assertFlightFinished(t, flight, stream, false)
		flightAck(flight, packet, packet+1)
		assertFlightFinished(t, flight, stream, true)
		flight.retireWriter(writer)
		if got := flight.snapshot(); got.Frames != 0 || got.PendingFrames != 0 || got.Failed || got.Closed {
			t.Fatalf("reset generation %d leaked fixed ownership: %+v", generation, got)
		}
	}
}

func TestQuicSendFlightResetKeepsPendingReservationsStreamLocal(t *testing.T) {
	flight := newQuicSendFlight()
	first := flight.newWriter(&flightResetTestStream{flightTestStream: flightTestStream{ctx: t.Context()}, id: 0})
	second := flight.newWriter(&flightResetTestStream{flightTestStream: flightTestStream{ctx: t.Context()}, id: 4})
	// Two native WriteWithLimit reservations, not yet packetized.
	first.pending, second.pending, flight.pending = 1, 1, 2
	flightStop(flight, 0)
	if first.pending != 0 || second.pending != 1 || flight.pending != 1 {
		t.Fatal("STOP refunded another stream's pending reservation")
	}
	flightSent(flight, 1, &qlog.StreamFrame{StreamID: 4, Length: 1, Fin: true})
	flightAck(flight, 1, 1)
	assertFlightFinished(t, flight, 0, false) // another stream's FIN is not ours
	flightReset(flight, 2, 0)
	flightAck(flight, 2, 2)
	assertFlightFinished(t, flight, 0, true)
}

func TestQuicSendFlightLateStopAfterFinAckNeedsNoReset(t *testing.T) {
	flight := newQuicSendFlight()
	flight.newWriter(&flightResetTestStream{flightTestStream: flightTestStream{ctx: t.Context()}, id: 0})
	flightSent(flight, 1, &qlog.StreamFrame{StreamID: 0, Length: 1, Fin: true})
	flightAck(flight, 1, 1)
	assertFlightFinished(t, flight, 0, true)
	flightStop(flight, 0) // quic-go already deleted the completed send stream
	assertFlightFinished(t, flight, 0, true)
}
