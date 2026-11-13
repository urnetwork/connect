//go:build !js

// These regressions force asymmetric setup loss at Pion's RTP packet boundary.
package connect

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// Supplies the negotiated fields used by Pion's real static RTP track.
type p2pFastPathWarmupTrackContext struct {
	webrtc.TrackLocalContext
	writer webrtc.TrackLocalWriter
}

// Offers the production carrier codec and payload type.
func (self *p2pFastPathWarmupTrackContext) CodecParameters() []webrtc.RTPCodecParameters {
	return []webrtc.RTPCodecParameters{{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: p2pFastPathMimeType, ClockRate: p2pFastPathRtpClockRate,
		},
		PayloadType: p2pFastPathRtpPayloadType,
	}}
}

// Uses an independent fixed stream identity for this bound track.
func (self *p2pFastPathWarmupTrackContext) SSRC() webrtc.SSRC { return 1 }

// Leaves retransmission streams unconfigured.
func (self *p2pFastPathWarmupTrackContext) SSRCRetransmission() webrtc.SSRC { return 0 }

// Leaves forward error correction streams unconfigured.
func (self *p2pFastPathWarmupTrackContext) SSRCForwardErrorCorrection() webrtc.SSRC { return 0 }

// Identifies this independent binding without a production peer identity.
func (self *p2pFastPathWarmupTrackContext) ID() string { return "synthetic-warmup" }

// Sends through the test's controlled packet boundary.
func (self *p2pFastPathWarmupTrackContext) WriteStream() webrtc.TrackLocalWriter {
	return self.writer
}

// Copies packets at the same writer used by production TrackLocalStaticRTP.
type p2pFastPathWarmupPacketWriter struct {
	write func(rtp.Packet)
}

// Keeps the packet owned by the test after Pion reuses its write envelope.
func (self *p2pFastPathWarmupPacketWriter) WriteRTP(header *rtp.Header, payload []byte) (int, error) {
	self.write(rtp.Packet{Header: *header, Payload: append([]byte(nil), payload...)})
	return header.MarshalSize() + len(payload), nil
}

// The production static track supplies split headers and payloads.
func (self *p2pFastPathWarmupPacketWriter) Write([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

// Owns the real production setup, receive loop, warmup worker and their join.
type p2pFastPathWarmupFixture struct {
	peer       *peerConn
	fastPath   *webRtcFastPath
	reader     *p2pFastPathBlockingPacketReader
	warmupDone chan struct{}
}

// Configures without ICE so a test can force exact packet loss and writes.
func newP2pFastPathWarmupFixture(t *testing.T, write func(rtp.Packet)) *p2pFastPathWarmupFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	pc, err := newTestPionPeerConnection()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	settings := DefaultWebRtcSettings()
	settings.Log = NewNoopLogger()
	settings.DataPlaneStats = &P2pDataPlaneStats{}
	warmupDone := make(chan struct{})
	peer := &peerConn{
		ctx: ctx, cancel: cancel, pc: pc, settings: settings, log: settings.Log,
		beforeWorkerDoneForTest: func(name string) {
			if name == "fast path warmup" {
				close(warmupDone)
			}
		},
	}
	if err := peer.configureFastPath(); err != nil {
		cancel()
		_ = pc.Close()
		t.Fatal(err)
	}
	fastPath := peer.fastPath.Load()
	if _, err := fastPath.track.Bind(&p2pFastPathWarmupTrackContext{
		writer: &p2pFastPathWarmupPacketWriter{write: write},
	}); err != nil {
		cancel()
		_ = pc.Close()
		t.Fatal(err)
	}
	reader := newP2pFastPathBlockingPacketReader()
	reader.packets = make(chan []byte, 64)
	reader.readStarted = make(chan struct{}, 128)
	fastPath.startReceive(reader)
	t.Cleanup(func() {
		cancel()
		peer.lifecycleWorkers().close()
		_ = pc.Close()
		peer.closeFastPath()
		select {
		case <-peer.lifecycleWorkers().Done():
		case <-time.After(5 * time.Second):
			t.Error("native warmup fixture workers did not join")
		}
	})
	return &p2pFastPathWarmupFixture{
		peer: peer, fastPath: fastPath, reader: reader, warmupDone: warmupDone,
	}
}

// Enqueues a copied wire packet before the receiver's next read barrier.
func (self *p2pFastPathWarmupFixture) receive(t *testing.T, packet rtp.Packet) {
	t.Helper()
	raw, err := packet.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	self.reader.packets <- raw
}

// A matching marker can reach one receiver while every reverse marker is
// lost. Local readiness must not retire the responder needed by its peer.
func TestWebRtcFastPathWarmupRecoversAfterAsymmetricSetupLoss(t *testing.T) {
	firstProbe := make(chan struct{})
	releaseProbe := make(chan struct{})
	var releaseOnce sync.Once
	var firstWrite sync.Once
	var passive *p2pFastPathWarmupFixture
	active := newP2pFastPathWarmupFixture(t, func(packet rtp.Packet) {
		initial := false
		firstWrite.Do(func() {
			initial = true
			close(firstProbe)
			<-releaseProbe
		})
		if !initial {
			passive.receive(t, packet)
		}
	})
	passive = newP2pFastPathWarmupFixture(t, func(packet rtp.Packet) {
		active.receive(t, packet)
	})
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseProbe) }) })
	if err := passive.fastPath.writeWarmup(); err != nil {
		t.Fatal(err)
	}
	waitP2pFastPathTestReads(t, active.reader, 2)
	if !active.peer.FastPathReady() || passive.peer.FastPathReady() {
		t.Fatal("fixture did not force one-sided carrier readiness")
	}
	active.peer.startFastPathWarmup()
	select {
	case <-firstProbe:
	case <-time.After(5 * time.Second):
		t.Fatal("locally ready peer did not enter its final setup write")
	}
	// A retry arrives while the final reverse marker is known to be lost.
	if err := passive.fastPath.writeWarmup(); err != nil {
		t.Fatal(err)
	}
	waitP2pFastPathTestReads(t, active.reader, 1)
	releaseOnce.Do(func() { close(releaseProbe) })
	select {
	case <-active.warmupDone:
		t.Fatal("local warmup retired its responder before the peer became ready")
	case <-passive.fastPath.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("peer retry did not recover carrier readiness")
	}
	if !active.peer.FastPathReady() || !passive.peer.FastPathReady() {
		t.Fatal("asymmetric setup recovery did not make both carriers ready")
	}
}

// A stopped carrier writer must not hold the receive worker behind a setup
// response. Duplicate requests coalesce while an ordinary message still drains.
func TestWebRtcFastPathWarmupBlockedWriterKeepsReceivingData(t *testing.T) {
	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	var releaseOnce sync.Once
	var writeOnce sync.Once
	fixture := newP2pFastPathWarmupFixture(t, func(packet rtp.Packet) {
		writeOnce.Do(func() { close(writeStarted) })
		<-releaseWrite
	})
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseWrite) }) })
	fixture.peer.startFastPathWarmup()
	select {
	case <-writeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("warmup writer did not reach the blocking boundary")
	}
	for range 3 {
		fixture.receive(t, rtp.Packet{
			Header:  rtp.Header{Version: 2},
			Payload: []byte{'U', 'R', 'W', p2pFastPathVersion},
		})
	}
	message := []byte("synthetic DATA behind blocked warmup write")
	for _, raw := range encodeP2pFastPathTestRtpPackets(t, 301, message) {
		fixture.reader.packets <- raw
	}
	select {
	case received := <-fixture.fastPath.messages:
		defer MessagePoolReturn(received.message)
		if !bytes.Equal(received.message, message) {
			t.Fatal("blocked warmup writer changed the incoming DATA")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("warmup response blocked incoming DATA behind its carrier write")
	}
}
