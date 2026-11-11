//go:build urnetwork_synthetic_fixture && urnetwork_receive_observation

package connect

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// The diagnostic parses borrowed headers, not checksums or payload contents.
func privateReceivePipelineTestPacket(source, destination netip.AddrPort, flags byte, sequence, acknowledgment uint32, payload int) []byte {
	header := 20
	if source.Addr().Is6() {
		header = 40
	}
	packet := make([]byte, header+20+payload)
	if header == 20 {
		packet[0], packet[9] = 0x45, 6
		binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
		src, dst := source.Addr().As4(), destination.Addr().As4()
		copy(packet[12:16], src[:])
		copy(packet[16:20], dst[:])
	} else {
		packet[0], packet[6] = 0x60, 6
		binary.BigEndian.PutUint16(packet[4:6], uint16(len(packet)-40))
		src, dst := source.Addr().As16(), destination.Addr().As16()
		copy(packet[8:24], src[:])
		copy(packet[24:40], dst[:])
	}
	tcp := packet[header:]
	binary.BigEndian.PutUint16(tcp[:2], source.Port())
	binary.BigEndian.PutUint16(tcp[2:4], destination.Port())
	binary.BigEndian.PutUint32(tcp[4:8], sequence)
	binary.BigEndian.PutUint32(tcp[8:12], acknowledgment)
	tcp[12], tcp[13] = 0x50, flags
	binary.BigEndian.PutUint16(tcp[14:16], 16384)
	return packet
}

func privateReceivePipelineTestBegin(t *testing.T) *PrivateReceivePipelineObservation {
	t.Helper()
	observation, err := PrivateBeginReceivePipelineObservationAt(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(observation.Close)
	return observation
}

func privateReceivePipelineTestMarshal(t *testing.T, message proto.Message) []byte {
	t.Helper()
	value, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestPrivateReceivePipelineOwnedTcpHeadersAndReverseFlow(t *testing.T) {
	observation := privateReceivePipelineTestBegin(t)
	source, destination := netip.MustParseAddrPort("10.0.0.2:40001"), netip.MustParseAddrPort("203.0.113.10:443")
	if !observation.WatchTcpFlow(source, destination) {
		t.Fatal("owned tuple registration refused")
	}
	packet := privateReceivePipelineTestPacket(source, destination, 2, 100, 0, 0)
	token := observation.CapturePacket(packet)
	observation.ObserveCaptured(PrivateReceivePipelineContext{Stage: "native_offer_enter"}, token)
	// Model a send that consumes/recycles the entire original before returning.
	// Its aggregate result carries no per-packet delivery authority.
	send := func() int { clear(packet); return 1 }
	accepted := send()
	observation.ObserveCaptured(PrivateReceivePipelineContext{Stage: "native_offer_return", ResultKnown: true, Success: accepted == 1}, token)
	PrivateObserveReceivePipelinePacket(PrivateReceivePipelineContext{Stage: "native_receive"}, privateReceivePipelineTestPacket(destination, source, 18, 200, 101, 3))
	source6, destination6 := netip.MustParseAddrPort("[2001:db8::1]:40002"), netip.MustParseAddrPort("[2001:db8::2]:443")
	if !observation.WatchTcpFlow(source6, destination6) {
		t.Fatal("owned IPv6 tuple registration refused")
	}
	PrivateObserveReceivePipelinePacket(PrivateReceivePipelineContext{Stage: "native_offer"}, privateReceivePipelineTestPacket(source6, destination6, 16, 301, 201, 9))
	report := observation.Snapshot()
	if len(report.Rows) != 4 || report.Attempts != 4 || report.Recorded != 4 || report.LostRecordLock != 0 {
		t.Fatalf("unexpected header census: rows=%d attempts=%d recorded=%d lost=%d", len(report.Rows), report.Attempts, report.Recorded, report.LostRecordLock)
	}
	first, returned, second, third := report.Rows[0], report.Rows[1], report.Rows[2], report.Rows[3]
	if first.FlowOrdinal != 1 || first.Reverse || first.TcpFlags != 2 || first.TcpSequence != 100 || first.TcpPayloadBytes != 0 || first.ResultKnown || first.TransferIdentityKnown {
		t.Fatal("forward SYN or attempted-edge semantics changed")
	}
	if returned.Stage != "native_offer_return" || returned.FlowOrdinal != first.FlowOrdinal || returned.TcpFlags != first.TcpFlags || returned.TcpSequence != first.TcpSequence || returned.TcpPayloadBytes != first.TcpPayloadBytes || !returned.ResultKnown || !returned.Success {
		t.Fatal("consumed/poisoned source changed the copied result header")
	}
	if second.FlowOrdinal != 1 || !second.Reverse || second.TcpFlags != 18 || second.TcpAcknowledgment != 101 || second.TcpWindow != 16384 || second.TcpPayloadBytes != 3 || second.ResultKnown {
		t.Fatal("reverse SYNACK did not join exact owned tuple")
	}
	if third.FlowOrdinal != 2 || third.IpVersion != 6 || third.TcpSequence != 301 || third.TcpPayloadBytes != 9 {
		t.Fatal("IPv6 header copy changed")
	}
}

func TestPrivateReceivePipelineActualTransferIdentityRawAndLegacy(t *testing.T) {
	observation := privateReceivePipelineTestBegin(t)
	source, destination := netip.MustParseAddrPort("10.0.0.2:40003"), netip.MustParseAddrPort("203.0.113.11:443")
	if !observation.WatchTcpFlow(source, destination) {
		t.Fatal("owned tuple registration refused")
	}
	packet := privateReceivePipelineTestPacket(destination, source, 24, 901, 701, 12)
	client, peer, generation, message, contract := NewId(), NewId(), NewId(), NewId(), NewId()
	raw := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, MessageBytes: packet, Raw: true}
	wrapped := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, MessageBytes: privateReceivePipelineTestMarshal(t, &protocol.IpPacketFromProvider{IpPacket: &protocol.IpPacket{PacketBytes: packet}})}
	pack := &protocol.Pack{MessageId: message[:], SequenceId: generation[:], Head: true, LogicalLane: 3, ContractId: contract[:], Frames: []*protocol.Frame{raw}}
	context := PrivateReceivePipelineContext{Stage: "wire_attempt", ClientId: client, PeerId: peer, SequenceNumber: 999}
	privateObserveReceivePipelineTransfer(context, privateReceivePipelineTestMarshal(t, &protocol.TransferFrame{Pack: pack}))
	pack.Frames = []*protocol.Frame{wrapped}
	privateObserveReceivePipelineTransfer(context, privateReceivePipelineTestMarshal(t, &protocol.TransferFrame{Pack: pack}))
	pack.SequenceNumber = 7
	pack.ContractId = nil
	pack.ContractFrame = &protocol.Frame{MessageType: protocol.MessageType_TransferContract}
	privateObserveReceivePipelineTransfer(context, privateReceivePipelineTestMarshal(t, &protocol.TransferFrame{Frame: &protocol.Frame{MessageType: protocol.MessageType_TransferPack, MessageBytes: privateReceivePipelineTestMarshal(t, pack)}}))
	context.SequenceId, context.MessageId, context.SequenceNumber = generation, message, 11
	context.Stage = "decoded_callback"
	privateObserveReceivePipelineFrame(context, raw)
	context.Stage = "worker_without_carrier"
	context.MessageId, context.SequenceId, context.SequenceNumber = NewId(), NewId(), 999
	pack.SequenceNumber = 13
	privateObserveReceivePipelinePack(context, pack)
	report := observation.Snapshot()
	if len(report.Rows) != 5 || report.InvalidWire != 0 || report.UnsupportedWire != 0 || report.WireTruncated != 0 {
		t.Fatalf("actual raw/legacy transfer coverage incomplete: rows=%d invalid=%d unsupported=%d truncated=%d", len(report.Rows), report.InvalidWire, report.UnsupportedWire, report.WireTruncated)
	}
	for index, row := range report.Rows {
		if !row.TransferIdentityKnown || row.GenerationToken != privateReceivePipelineToken(generation) || row.MessageToken != privateReceivePipelineToken(message) || row.ClientToken != privateReceivePipelineToken(client) || row.PeerToken != privateReceivePipelineToken(peer) || row.TcpSequence != 901 || !row.Reverse {
			t.Fatalf("actual identity/TCP join changed at row %d", index)
		}
	}
	if report.Rows[0].SequenceNumber != 0 || report.Rows[1].SequenceNumber != 0 || !report.Rows[0].CompactContract || !report.Rows[0].Head || report.Rows[0].LogicalLane != 3 || report.Rows[2].SequenceNumber != 7 || !report.Rows[2].FullContract || report.Rows[2].CompactContract || report.Rows[3].SequenceNumber != 11 || report.Rows[4].SequenceNumber != 13 || !report.Rows[4].FullContract || !report.Rows[4].Head {
		t.Fatal("wire defaults, contract form or caller-edge identity was inferred incorrectly")
	}
}

func TestPrivateReceivePipelineNeverWaitsAndRetainsBoundedFirstLatest(t *testing.T) {
	observation := privateReceivePipelineTestBegin(t)
	source, destination := netip.MustParseAddrPort("10.0.0.2:40004"), netip.MustParseAddrPort("203.0.113.12:443")
	if !observation.WatchTcpFlow(source, destination) {
		t.Fatal("owned tuple registration refused")
	}
	packet := privateReceivePipelineTestPacket(source, destination, 16, 1, 1, 0)
	observation.stateLock.Lock()
	PrivateObserveReceivePipelinePacket(PrivateReceivePipelineContext{Stage: "contended"}, packet)
	watchAccepted := observation.WatchTcpFlow(destination, source)
	observation.stateLock.Unlock()
	if watchAccepted {
		t.Fatal("contended registration unexpectedly claimed success")
	}
	for index := 0; index < privateReceivePipelineCapacity+1; index++ {
		PrivateObserveReceivePipelinePacket(PrivateReceivePipelineContext{Stage: "bounded"}, packet)
	}
	report := observation.Snapshot()
	if report.LostRecordLock != 1 || report.LostWatchLock != 1 || report.Recorded != 513 || report.Attempts != 514 || report.Overwritten != 1 || len(report.Rows) != 512 {
		t.Fatalf("bounded/loss census changed: lost=%d/%d recorded=%d attempts=%d overwritten=%d rows=%d", report.LostRecordLock, report.LostWatchLock, report.Recorded, report.Attempts, report.Overwritten, len(report.Rows))
	}
	if report.Rows[0].EventOrdinal != 2 || report.Rows[127].EventOrdinal != 129 || report.Rows[128].EventOrdinal != 131 || report.Rows[511].EventOrdinal != 514 {
		t.Fatal("first128/latest384 retention boundary changed")
	}
}

func TestPrivateReceivePipelineUnsupportedCoverageIsExplicit(t *testing.T) {
	observation := privateReceivePipelineTestBegin(t)
	source, destination := netip.MustParseAddrPort("10.0.0.2:40005"), netip.MustParseAddrPort("203.0.113.13:443")
	if !observation.WatchTcpFlow(source, destination) {
		t.Fatal("owned tuple registration refused")
	}
	PrivateObserveReceivePipelinePacket(PrivateReceivePipelineContext{}, []byte{0x45})
	fragment := privateReceivePipelineTestPacket(source, destination, 16, 1, 1, 0)
	fragment[6] = 0x20
	PrivateObserveReceivePipelinePacket(PrivateReceivePipelineContext{}, fragment)
	source6, destination6 := netip.MustParseAddrPort("[2001:db8::1]:40005"), netip.MustParseAddrPort("[2001:db8::2]:443")
	extension := privateReceivePipelineTestPacket(source6, destination6, 16, 1, 1, 0)
	extension[6] = 44
	PrivateObserveReceivePipelinePacket(PrivateReceivePipelineContext{}, extension)
	privateObserveReceivePipelineTransfer(PrivateReceivePipelineContext{}, []byte{0xff})
	privateObserveReceivePipelineTransfer(PrivateReceivePipelineContext{}, privateReceivePipelineTestMarshal(t, &protocol.TransferFrame{EncryptedTransferFrame: []byte{1}}))
	var fields []byte
	for index := 0; index < privateReceivePipelineWireFieldLimit+1; index++ {
		fields = protowire.AppendTag(fields, 99, protowire.VarintType)
		fields = protowire.AppendVarint(fields, 0)
	}
	privateObserveReceivePipelineTransfer(PrivateReceivePipelineContext{}, fields)
	generation, message := NewId(), NewId()
	frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider, MessageBytes: privateReceivePipelineTestPacket(source, destination, 16, 1, 1, 0), Raw: true}
	pack := &protocol.Pack{MessageId: message[:], SequenceId: generation[:], Frames: make([]*protocol.Frame, privateReceivePipelineFrameLimit+1)}
	for index := range pack.Frames {
		pack.Frames[index] = frame
	}
	privateObserveReceivePipelineTransfer(PrivateReceivePipelineContext{}, privateReceivePipelineTestMarshal(t, &protocol.TransferFrame{Pack: pack}))
	privateObserveReceivePipelinePack(PrivateReceivePipelineContext{}, pack)
	report := observation.Snapshot()
	if len(report.Rows) != 0 || report.InvalidPackets != 1 || report.UnsupportedPackets != 2 || report.InvalidWire != 1 || report.UnsupportedWire != 1 || report.WireTruncated != 3 {
		t.Fatalf("incomplete coverage was hidden: rows=%d invalidPacket=%d unsupportedPacket=%d invalidWire=%d unsupportedWire=%d truncated=%d", len(report.Rows), report.InvalidPackets, report.UnsupportedPackets, report.InvalidWire, report.UnsupportedWire, report.WireTruncated)
	}
}

func TestPrivateReceivePipelineWatchLifecycleAndHashedSerialization(t *testing.T) {
	if observation, err := PrivateBeginReceivePipelineObservationAt(time.Time{}); err == nil || observation != nil {
		t.Fatal("missing monotonic origin accepted")
	}
	observation := privateReceivePipelineTestBegin(t)
	if other, err := PrivateBeginReceivePipelineObservationAt(time.Now()); err == nil || other != nil {
		t.Fatal("second active recorder accepted")
	}
	source, destination := netip.MustParseAddrPort("10.0.0.2:40100"), netip.MustParseAddrPort("203.0.113.14:443")
	for index := 0; index < privateReceivePipelineWatchCapacity; index++ {
		owned := netip.AddrPortFrom(source.Addr(), source.Port()+uint16(index))
		if !observation.WatchTcpFlow(owned, destination) {
			t.Fatal("bounded owned tuple registration refused")
		}
	}
	if !observation.WatchTcpFlow(destination, source) || observation.WatchTcpFlow(netip.AddrPortFrom(source.Addr(), 49999), destination) || observation.WatchTcpFlow(netip.AddrPort{}, destination) {
		t.Fatal("tuple identity/cap/invalid boundary changed")
	}
	id := NewId()
	context := PrivateReceivePipelineContext{Stage: strings.Repeat("s", 65), Detail: strings.Repeat("d", 97), ClientId: id}
	packet := privateReceivePipelineTestPacket(source, destination, 16, 1, 1, 0)
	oldToken := observation.CapturePacket(packet)
	PrivateObserveReceivePipelinePacket(context, packet)
	observation.Close()
	PrivateObserveReceivePipelinePacket(context, privateReceivePipelineTestPacket(source, destination, 16, 2, 1, 0))
	if observation.WatchTcpFlow(source, destination) {
		t.Fatal("closed recorder accepted a new watch")
	}
	report := observation.Snapshot()
	if !report.Closed || len(report.Flows) != 64 || report.WatchOverflow != 1 || report.InvalidWatch != 1 || report.Recorded != 1 || report.TextTruncated != 1 || len(report.Rows[0].Stage) != 64 || len(report.Rows[0].Detail) != 96 {
		t.Fatal("closed/capped/text-truncation census changed")
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(hex.EncodeToString(id[:]))) || bytes.Contains(encoded, []byte(id.String())) || bytes.Contains(encoded, []byte(source.String())) || bytes.Contains(encoded, []byte(destination.String())) {
		t.Fatal("raw private identity or owned tuple escaped serialization")
	}
	if report.Rows[0].ClientToken != privateReceivePipelineToken(id) || len(report.Flows[0].TupleToken) != 64 {
		t.Fatal("opaque identity join is absent")
	}
	replacement, err := PrivateBeginReceivePipelineObservationAt(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(replacement.Close)
	if !replacement.WatchTcpFlow(source, destination) {
		t.Fatal("replacement watch registration refused")
	}
	// Both recorders watch the same tuple. Rejection therefore proves the
	// instance fence, rather than accidentally failing an unrelated tuple join.
	replacement.ObserveCaptured(PrivateReceivePipelineContext{Stage: "stale_token"}, oldToken)
	observation.ObserveCaptured(PrivateReceivePipelineContext{Stage: "closed_token"}, oldToken)
	replacement.ObserveCaptured(PrivateReceivePipelineContext{Stage: "own_token"}, replacement.CapturePacket(packet))
	newReport := replacement.Snapshot()
	if newReport.InvalidCaptureTokens != 1 || len(newReport.Rows) != 1 || newReport.Rows[0].Stage != "own_token" || observation.Snapshot().Recorded != 1 {
		t.Fatal("closed or replacement observation accepted a stale copied token")
	}
}

func TestPrivateReceivePipelineProducerPathsAllocateNoMemory(t *testing.T) {
	observation := privateReceivePipelineTestBegin(t)
	source, destination := netip.MustParseAddrPort("10.0.0.2:40006"), netip.MustParseAddrPort("203.0.113.15:443")
	if !observation.WatchTcpFlow(source, destination) {
		t.Fatal("owned tuple registration refused")
	}
	packet := privateReceivePipelineTestPacket(source, destination, 16, 1, 1, 1280)
	generation, message := NewId(), NewId()
	frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider, MessageBytes: packet, Raw: true}
	pack := &protocol.Pack{MessageId: message[:], SequenceId: generation[:], Frames: []*protocol.Frame{frame}}
	wire := privateReceivePipelineTestMarshal(t, &protocol.TransferFrame{Pack: pack})
	context := PrivateReceivePipelineContext{Stage: "client_decode_paid", SequenceId: generation, MessageId: message, TransportType: TransportTypeH1}
	allocations := testing.AllocsPerRun(100, func() {
		PrivateObserveReceivePipelinePacket(context, packet)
		privateObserveReceivePipelineFrame(context, frame)
		privateObserveReceivePipelineTransfer(context, wire)
		privateObserveReceivePipelinePack(context, pack)
		observation.ObserveCaptured(context, observation.CapturePacket(packet))
	})
	if allocations != 0 {
		t.Fatalf("packet/frame/wire observer allocated: %.2f", allocations)
	}
	if report := observation.Snapshot(); report.Recorded != 505 || report.InvalidPackets != 0 || report.InvalidWire != 0 || report.LostRecordLock != 0 || len(report.LatestByFlow) != 1 || report.LatestByFlow[0].Count != 505 {
		t.Fatal("allocation control did not exercise all real producer paths")
	}
	observation.Close()
	if allocations = testing.AllocsPerRun(100, func() {
		PrivateObserveReceivePipelinePacket(context, packet)
		privateObserveReceivePipelineFrame(context, frame)
		privateObserveReceivePipelineTransfer(context, wire)
		privateObserveReceivePipelinePack(context, pack)
		observation.ObserveCaptured(context, observation.CapturePacket(packet))
	}); allocations != 0 {
		t.Fatalf("disabled observer allocated: %.2f", allocations)
	}
}
