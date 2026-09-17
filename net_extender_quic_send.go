package connect

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/qlog"
	"github.com/quic-go/quic-go/qlogwriter"
)

// This is part of the carrier's lease, not another independently spendable
// budget. The 96 STREAM roots have a 2-KiB charge each (quic-go v0.61 uses
// 1452-byte pooled buffers, even for a one-byte STREAM). The remaining 64 KiB
// covers the fixed ownership tables, one buffered frame, write scratch, and
// packet/frame metadata. Receive windows and QUIC/TLS control state are charged
// separately by the carrier policy.
const extenderQuicSendMemoryByteCount ByteCount = 256 * 1024

const (
	quicSendFlightFrameLimit = 96
	quicSendFlightWriteLimit = 64
	quicSendFlightPackets    = 256
	quicSendFlightChunk      = 4096
	quicSendFlightStreams    = 8
	quicSendFlightDatagrams  = 32
)

var errQuicSendFlight = errors.New("QUIC retained send-flight limit reached")
var errQuicDatagramFlightFull = errors.New("QUIC retained datagram flight is full")

type quicSendFlightStream interface {
	io.Writer
	StreamID() quic.StreamID
	Context() context.Context
	CancelWrite(quic.StreamErrorCode)
	SetWriteDeadline(time.Time) error
}

type quicSendFlightFrame struct {
	stream quic.StreamID
	start  int64
	end    int64
	packet int64  // -1: retained in the stream's retransmission queue
	order  uint64 // admission order of a DATAGRAM still in quic-go's FIFO
	fin    bool
	used   bool
}

type quicSendFlightPacket struct {
	packet int64
	used   bool
}

type quicSendFlightSnapshot struct {
	Frames, PeakFrames, PendingFrames int
	Bytes, PeakBytes                  int64
	DatagramFrames                    int
	DatagramBytes                     int64
	Closed, Failed                    bool
}

// quicSendFlight follows ownership, not congestion's bytes-in-flight: a lost
// frame is still retained, and an ACK of its former packet does not release it.
// All storage is fixed. The callbacks never wait for credit or close a QUIC
// connection. Only application writers wait, outside QUIC's event loop.
type quicSendFlight struct {
	mutex         sync.Mutex
	datagramMu    sync.Mutex
	frames        [quicSendFlightFrameLimit + quicSendFlightDatagrams]quicSendFlightFrame
	packets       [quicSendFlightPackets]quicSendFlightPacket
	writers       [quicSendFlightStreams]*quicSendFlightWriter
	frameCount    int
	peakFrames    int
	byteCount     int64
	peakBytes     int64
	pending       int
	datagrams     int
	datagramBytes int64
	datagramOrder uint64
	mtu           int
	ptoProbes     int
	ptoPending    bool
	ptoBefore     int
	ptoPacket     int64
	latestStream  quic.StreamID
	ackedFin      quic.StreamID
	closed        bool
	err           error
	notify        chan struct{}
	failed        chan struct{}
}

func newQuicSendFlight() *quicSendFlight {
	return &quicSendFlight{notify: make(chan struct{}, 1), failed: make(chan struct{}), latestStream: -1, ackedFin: -1}
}

// installQuicSendFlight preserves diagnostics and creates a distinct owner
// for each connection, including each candidate of a happy-eyeballs race.
func installQuicSendFlight(config *quic.Config) {
	previous := config.Tracer
	config.Tracer = func(ctx context.Context, isClient bool, id quic.ConnectionID) qlogwriter.Trace {
		var next qlogwriter.Trace
		if previous != nil {
			next = previous(ctx, isClient, id)
		}
		flight := newQuicSendFlight()
		flight.mtu = max(1200, int(config.InitialPacketSize))
		return &quicSendFlightTrace{flight: flight, next: next}
	}
}

func quicSendFlightForConn(conn *quic.Conn) *quicSendFlight {
	if trace, ok := conn.QlogTrace().(*quicSendFlightTrace); ok {
		return trace.flight
	}
	return nil
}

// bind is called after Dial. A guard failure only signals this goroutine from
// the tracer: CloseWithError waits for QUIC's event loop and cannot run inside
// a packet callback. Registered writers are canceled synchronously first.
func (self *quicSendFlight) bind(conn *quic.Conn) {
	go func() {
		select {
		case <-self.failed:
			conn.CloseWithError(0, errQuicSendFlight.Error())
		case <-conn.Context().Done():
		}
		self.close()
	}()
}

func (self *quicSendFlight) signalLocked() {
	select {
	case self.notify <- struct{}{}:
	default:
	}
}

func (self *quicSendFlight) failLocked() {
	if self.err == nil && !self.closed {
		self.err = errQuicSendFlight
		close(self.failed)
	}
	self.signalLocked()
}

func (self *quicSendFlight) cancelFailedWriters() {
	self.mutex.Lock()
	failed := self.err != nil
	writers := self.writers
	self.mutex.Unlock()
	if failed {
		// v0.61 logs PacketSent after packetization released both the stream
		// and framer locks. CancelWrite is non-waiting and stops further frame
		// allocation synchronously. Do not replace this with CloseWithError.
		for _, writer := range writers {
			if writer != nil {
				writer.stream.CancelWrite(0)
			}
		}
	}
}

func (self *quicSendFlight) close() {
	self.mutex.Lock()
	self.closed = true
	clear(self.frames[:])
	clear(self.packets[:])
	clear(self.writers[:])
	self.frameCount = 0
	self.byteCount = 0
	self.pending = 0
	self.datagrams = 0
	self.datagramBytes = 0
	self.signalLocked()
	self.mutex.Unlock()
}

func (self *quicSendFlight) snapshot() quicSendFlightSnapshot {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return quicSendFlightSnapshot{
		Frames: self.frameCount, PeakFrames: self.peakFrames, PendingFrames: self.pending,
		Bytes: self.byteCount, PeakBytes: self.peakBytes, Closed: self.closed, Failed: self.err != nil,
		DatagramFrames: self.datagrams, DatagramBytes: self.datagramBytes,
	}
}

func (self *quicSendFlight) newWriter(stream quicSendFlightStream) *quicSendFlightWriter {
	writer := &quicSendFlightWriter{flight: self, stream: stream}
	self.mutex.Lock()
	registered := false
	for i := range self.writers {
		if self.writers[i] == nil {
			self.writers[i] = writer
			registered = true
			break
		}
	}
	if !registered {
		self.failLocked()
	}
	self.mutex.Unlock()
	self.cancelFailedWriters()
	return writer
}

// A pooled API connection registers each request before writing its headers.
// Reuse the fixed writer slot only after its FIN and every retained root have
// been acknowledged (or after connection teardown), never merely on Body.Close.
func (self *quicSendFlight) retireWriter(writer *quicSendFlightWriter) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	for i, registered := range self.writers {
		if registered == writer {
			self.writers[i] = nil
			return
		}
	}
}

func (self *quicSendFlight) wait(ctx context.Context, idle bool, deadline func() time.Time) error {
	for {
		self.mutex.Lock()
		err := self.err
		if self.closed && err == nil {
			err = net.ErrClosed
		}
		ready := self.frameCount+self.pending < quicSendFlightWriteLimit
		if idle {
			ready = self.frameCount+self.pending+self.datagrams == 0
		}
		self.mutex.Unlock()
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		var until time.Time
		if deadline != nil {
			until = deadline()
			if !until.IsZero() && !time.Now().Before(until) {
				return os.ErrDeadlineExceeded
			}
		}
		if ready {
			return nil
		}
		var timer *time.Timer
		var timeout <-chan time.Time
		if !until.IsZero() {
			timer = time.NewTimer(time.Until(until))
			timeout = timer.C
		}
		select {
		case <-ctx.Done():
		case <-self.notify:
		case <-self.failed:
		case <-timeout:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (self *quicSendFlight) waitIdle(ctx context.Context) error {
	return self.wait(ctx, true, nil)
}

func (self *quicSendFlight) requestStreamID() quic.StreamID {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.latestStream
}

func (self *quicSendFlight) waitFinished(ctx context.Context, stream quic.StreamID) error {
	for {
		self.mutex.Lock()
		done := stream >= 0 && self.ackedFin >= stream && self.frameCount+self.pending == 0
		err := self.err
		if self.closed && err == nil {
			err = net.ErrClosed
		}
		self.mutex.Unlock()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-self.notify:
		case <-self.failed:
		}
	}
}

// A lost packet no longer owns its frames. Its frames move to retransmission
// storage and retain their charges until their new packet is acknowledged.
func (self *quicSendFlight) lostLocked(packet int64) {
	for i := range self.packets {
		if self.packets[i].used && self.packets[i].packet == packet {
			self.packets[i].used = false
		}
	}
	for i := range self.frames {
		if self.frames[i].used && self.frames[i].packet == packet {
			if self.frames[i].stream == -1 {
				self.releaseFrameLocked(i)
			} else {
				self.frames[i].packet = -1
			}
		}
	}
	self.signalLocked()
}

func (self *quicSendFlight) releaseFrameLocked(i int) {
	f := &self.frames[i]
	if f.stream == -1 {
		self.datagrams--
		self.datagramBytes -= f.end
	} else {
		self.byteCount -= f.end - f.start
		self.frameCount--
	}
	*f = quicSendFlightFrame{}
}

// Admission precedes Conn.SendDatagram's payload copy. There are at most 32
// queued + already-sent DATAGRAM roots in total, charged by the direct H3
// datagram envelope. A full flight is deliberately zero-wait so the hybrid
// writer can select its existing reliable stream lane immediately.
func (self *quicSendFlight) trySendDatagram(send func([]byte) error, b []byte) error {
	if !self.datagramMu.TryLock() {
		return errQuicDatagramFlightFull
	}
	defer self.datagramMu.Unlock()
	self.mutex.Lock()
	if self.err != nil || self.closed {
		err := self.err
		if err == nil {
			err = net.ErrClosed
		}
		self.mutex.Unlock()
		return err
	}
	if self.datagrams == quicSendFlightDatagrams {
		self.mutex.Unlock()
		return errQuicDatagramFlightFull
	}
	// Direct H3's datagram policy only admits <= 1452-byte packets. Never
	// admit a bigger allocation merely because the caller bypassed that policy.
	if len(b) > 1452 {
		self.mutex.Unlock()
		return &quic.DatagramTooLargeError{MaxDatagramPayloadSize: 1452}
	}
	index := -1
	for i := range self.frames {
		if !self.frames[i].used {
			index = i
			break
		}
	}
	if index < 0 {
		self.failLocked()
		self.mutex.Unlock()
		self.cancelFailedWriters()
		return errQuicSendFlight
	}
	self.datagramOrder++
	self.frames[index] = quicSendFlightFrame{stream: -1, end: int64(len(b)), packet: -2, order: self.datagramOrder, used: true}
	self.datagrams++
	self.datagramBytes += int64(len(b))
	self.mutex.Unlock()
	err := send(b)
	if err != nil {
		self.mutex.Lock()
		if self.frames[index].used && self.frames[index].packet == -2 {
			self.releaseFrameLocked(index)
		}
		self.signalLocked()
		self.mutex.Unlock()
	}
	return err
}

func (self *quicSendFlight) sentDatagramLocked(packet int64, length int64) {
	first := -1
	for i, f := range self.frames {
		if f.used && f.stream == -1 && f.packet == -2 && (first < 0 || f.order < self.frames[first].order) {
			first = i
		}
	}
	if first < 0 || self.frames[first].end != length {
		self.failLocked()
		return
	}
	self.frames[first].packet = packet
}

func (self *quicSendFlight) addFrameLocked(frame quicSendFlightFrame) {
	for i := range self.frames {
		if !self.frames[i].used {
			frame.used = true
			self.frames[i] = frame
			self.frameCount++
			self.byteCount += frame.end - frame.start
			self.peakFrames = max(self.peakFrames, self.frameCount)
			self.peakBytes = max(self.peakBytes, self.byteCount)
			if self.frameCount == quicSendFlightFrameLimit {
				self.failLocked()
			}
			return
		}
	}
	self.failLocked()
}

func (self *quicSendFlight) sentFrameLocked(packet int64, frame *qlog.StreamFrame) {
	// Client request/reliable streams only. The bounded SETTINGS/control
	// streams belong to the separately reserved QUIC control-state envelope.
	if frame.StreamID%4 != 0 || frame.Length == 0 && !frame.Fin {
		return
	}
	self.latestStream = max(self.latestStream, frame.StreamID)
	start, end := frame.Offset, frame.Offset+frame.Length
	for i := range self.frames {
		old := &self.frames[i]
		finMatch := frame.Fin && frame.Length == 0 && old.fin && old.start == start && old.end == end
		if !old.used || old.stream != frame.StreamID || !finMatch && (end <= old.start || old.end <= start) {
			continue
		}
		// Retransmissions are an entire frame or its prefix. A prefix split
		// allocates a second pooled root. PTO moves an entire old packet,
		// even though v0.61 does not emit PacketLost for that move.
		if start != old.start || old.end < end {
			self.failLocked()
			return
		}
		if old.packet >= 0 && old.packet != packet {
			self.lostLocked(old.packet)
		}
		if old.end == end {
			old.packet = packet
			old.fin = frame.Fin
		} else {
			old.start = end
			self.byteCount -= frame.Length
			self.addFrameLocked(quicSendFlightFrame{stream: frame.StreamID, start: start, end: end, packet: packet})
		}
		return
	}
	if self.pending > 0 {
		self.pending--
	}
	self.addFrameLocked(quicSendFlightFrame{stream: frame.StreamID, start: start, end: end, packet: packet, fin: frame.Fin})
}

func (self *quicSendFlight) sent(event qlog.PacketSent) {
	if event.Header.PacketType != qlog.PacketType1RTT {
		return
	}
	self.mutex.Lock()
	if self.closed {
		self.mutex.Unlock()
		return
	}
	if self.ptoProbes > 0 {
		self.ptoPending = true
		self.ptoBefore = self.packetCountLocked()
		self.ptoPacket = int64(event.Header.PacketNumber)
	}
	ackEliciting := false
	for _, frame := range event.Frames {
		switch frame := frame.Frame.(type) {
		case *qlog.AckFrame, *qlog.ConnectionCloseFrame:
		case *qlog.StreamFrame:
			ackEliciting = true
			self.sentFrameLocked(int64(event.Header.PacketNumber), frame)
		case *qlog.DatagramFrame:
			ackEliciting = true
			self.sentDatagramLocked(int64(event.Header.PacketNumber), frame.Length)
		default:
			ackEliciting = true
		}
	}
	// PMTU probes contain only PING and exceed the currently validated MTU.
	// They are explicitly not outstanding in quic-go's PTO ordering.
	if len(event.Frames) == 1 && self.mtu > 0 && event.Raw.Length > self.mtu {
		if _, ping := event.Frames[0].Frame.(*qlog.PingFrame); ping {
			ackEliciting = false
		}
	}
	if ackEliciting {
		if self.ptoProbes > 0 {
			self.ptoProbes--
		}
		added := false
		for i := range self.packets {
			if !self.packets[i].used {
				self.packets[i] = quicSendFlightPacket{packet: int64(event.Header.PacketNumber), used: true}
				added = true
				break
			}
		}
		if !added {
			self.failLocked()
		}
	} else if self.ptoPending {
		// An ACK-only probe does not emit a sent-packet metrics update. Its
		// silently discarded roots cannot be correlated from public events.
		// Retain their charges and end this generation instead of guessing.
		self.ptoPending = false
		self.failLocked()
	}
	self.signalLocked()
	self.mutex.Unlock()
	self.cancelFailedWriters()
}

func (self *quicSendFlight) received(event qlog.PacketReceived) {
	if event.Header.PacketType != qlog.PacketType1RTT {
		return
	}
	self.mutex.Lock()
	defer self.mutex.Unlock()
	for _, frame := range event.Frames {
		ack, ok := frame.Frame.(*qlog.AckFrame)
		if !ok {
			continue
		}
		for _, r := range ack.AckRanges {
			for i := range self.frames {
				f := &self.frames[i]
				if f.used && f.packet >= 0 && int64(r.Smallest) <= f.packet && f.packet <= int64(r.Largest) {
					if f.fin {
						self.ackedFin = max(self.ackedFin, f.stream)
					}
					self.releaseFrameLocked(i)
				}
			}
			for i := range self.packets {
				p := &self.packets[i]
				if p.used && int64(r.Smallest) <= p.packet && p.packet <= int64(r.Largest) {
					p.used = false
				}
			}
		}
	}
	self.signalLocked()
}

func (self *quicSendFlight) lost(event qlog.PacketLost) {
	if event.Header.PacketType == qlog.PacketType1RTT {
		self.mutex.Lock()
		self.lostLocked(int64(event.Header.PacketNumber))
		self.mutex.Unlock()
	}
}

func (self *quicSendFlight) pto(event qlog.LossTimerUpdated) {
	if event.Type != qlog.LossTimerUpdateTypeExpired || event.TimerType != qlog.TimerTypePTO ||
		qlog.EncryptionLevelToPacketType(event.EncLevel) != qlog.PacketType1RTT {
		return
	}
	self.mutex.Lock()
	self.ptoProbes = 2
	self.mutex.Unlock()
}

func (self *quicSendFlight) packetCountLocked() int {
	count := 0
	for _, p := range self.packets {
		if p.used {
			count++
		}
	}
	return count
}

// QueueProbePacket does not log its ownership moves. After each probe,
// SentPacket emits MetricsUpdated when the outstanding-packet count changes.
// That count identifies exactly how many oldest packets were moved, including
// DATAGRAM-only packets discarded while searching for a retransmittable frame.
// An omitted / zero count is unchanged here: the just-sent probe means the
// actual outstanding count cannot be zero. Any subsequent event finalizes
// this unchanged case; on receive, PTOCountUpdated(0) precedes ACK metrics.
func (self *quicSendFlight) finishPto(metrics *qlog.MetricsUpdated) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if !self.ptoPending {
		return
	}
	self.ptoPending = false
	count := self.ptoBefore
	if metrics != nil && metrics.PacketsInFlight > 0 {
		count = metrics.PacketsInFlight
	}
	if self.packetCountLocked() < count {
		self.failLocked()
		return
	}
	for self.packetCountLocked() > count {
		first := int64(-1)
		for _, p := range self.packets {
			if p.used && p.packet != self.ptoPacket && (first < 0 || p.packet < first) {
				first = p.packet
			}
		}
		if first < 0 {
			self.failLocked()
			return
		}
		self.lostLocked(first)
	}
}

type quicSendFlightWriter struct {
	flight   *quicSendFlight
	stream   quicSendFlightStream
	writeMu  sync.Mutex
	mutex    sync.Mutex
	deadline time.Time
}

func (self *quicSendFlightWriter) writeDeadline() time.Time {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.deadline
}

func (self *quicSendFlightWriter) SetWriteDeadline(deadline time.Time) error {
	self.mutex.Lock()
	self.deadline = deadline
	self.mutex.Unlock()
	self.flight.mutex.Lock()
	self.flight.signalLocked()
	self.flight.mutex.Unlock()
	return self.stream.SetWriteDeadline(deadline)
}

func (self *quicSendFlightWriter) Write(b []byte) (int, error) {
	self.writeMu.Lock()
	defer self.writeMu.Unlock()
	var written int
	for len(b) > 0 {
		if err := self.flight.wait(self.stream.Context(), false, self.writeDeadline); err != nil {
			return written, err
		}
		chunk := b[:min(len(b), quicSendFlightChunk)]
		var n int
		var err error
		if stream, ok := self.stream.(interface {
			WriteWithLimit([]byte, func(int) int) (int, error)
		}); ok {
			n, err = stream.WriteWithLimit(chunk, func(maxBytes int) int {
				self.flight.mutex.Lock()
				defer self.flight.mutex.Unlock()
				if self.flight.closed || self.flight.err != nil ||
					self.flight.frameCount+self.flight.pending >= quicSendFlightWriteLimit {
					return 0
				}
				self.flight.pending++
				return maxBytes
			})
		} else {
			n, err = self.stream.Write(chunk)
		}
		written += n
		b = b[n:]
		if errors.Is(err, quic.ErrWriteLimitReached) {
			continue
		}
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

type quicSendFlightTrace struct {
	flight *quicSendFlight
	next   qlogwriter.Trace
}

func (self *quicSendFlightTrace) SupportsSchemas(schema string) bool {
	return schema == qlog.EventSchema || self.next != nil && self.next.SupportsSchemas(schema)
}

func (self *quicSendFlightTrace) AddProducer() qlogwriter.Recorder {
	var next qlogwriter.Recorder
	if self.next != nil {
		next = self.next.AddProducer()
	}
	return &quicSendFlightRecorder{flight: self.flight, next: next}
}

type quicSendFlightRecorder struct {
	flight *quicSendFlight
	next   qlogwriter.Recorder
}

func (self *quicSendFlightRecorder) RecordEvent(event qlogwriter.Event) {
	switch event := event.(type) {
	case qlog.MetricsUpdated:
		self.flight.finishPto(&event)
	case *qlog.MetricsUpdated:
		self.flight.finishPto(event)
	case qlog.PacketSent, *qlog.PacketSent, qlog.PacketReceived, *qlog.PacketReceived,
		qlog.PacketLost, *qlog.PacketLost, qlog.LossTimerUpdated, *qlog.LossTimerUpdated,
		qlog.PTOCountUpdated, *qlog.PTOCountUpdated, qlog.ConnectionClosed, *qlog.ConnectionClosed,
		qlog.MTUUpdated, *qlog.MTUUpdated:
		// HTTP/3 has a separate producer. Its concurrently created headers
		// and DATA frames must not finalize the QUIC producer's pending PTO.
		self.flight.finishPto(nil)
	}
	switch event := event.(type) {
	case qlog.PacketSent:
		self.flight.sent(event)
	case *qlog.PacketSent:
		self.flight.sent(*event)
	case qlog.PacketReceived:
		self.flight.received(event)
	case *qlog.PacketReceived:
		self.flight.received(*event)
	case qlog.PacketLost:
		self.flight.lost(event)
	case *qlog.PacketLost:
		self.flight.lost(*event)
	case qlog.LossTimerUpdated:
		self.flight.pto(event)
	case *qlog.LossTimerUpdated:
		self.flight.pto(*event)
	case qlog.PTOCountUpdated:
		if event.PTOCount == 0 {
			self.flight.mutex.Lock()
			self.flight.ptoProbes = 0
			self.flight.mutex.Unlock()
		}
	case *qlog.PTOCountUpdated:
		if event.PTOCount == 0 {
			self.flight.mutex.Lock()
			self.flight.ptoProbes = 0
			self.flight.mutex.Unlock()
		}
	case qlog.MTUUpdated:
		self.flight.mutex.Lock()
		self.flight.mtu = int(event.Value)
		self.flight.mutex.Unlock()
	case *qlog.MTUUpdated:
		self.flight.mutex.Lock()
		self.flight.mtu = int(event.Value)
		self.flight.mutex.Unlock()
	case qlog.ConnectionClosed, *qlog.ConnectionClosed:
		self.flight.close()
	}
	self.flight.cancelFailedWriters()
	if self.next != nil {
		self.next.RecordEvent(event)
	}
}

func (self *quicSendFlightRecorder) Close() error {
	if self.next != nil {
		return self.next.Close()
	}
	return nil
}
