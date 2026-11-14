//go:build urnetwork_synthetic_fixture && urnetwork_receive_observation

// Persistent per-flow cells complement the chronological ring; they preserve
// last observed facts without asserting complete delivery or ACK application.
package connect

import (
	"encoding/binary"
	"net/netip"
	"reflect"
	"testing"

	"github.com/urnetwork/connect/v2026/protocol"
)

// A stalled flow's data identity survives100000 actual unrelated observations
// after it has fallen out of both the first and latest chronological segments.
func TestPrivateReceivePipelineLatestSurvivesUnrelatedBulk(t *testing.T) {
	observation := privateReceivePipelineTestBegin(t)
	source, destination := netip.MustParseAddrPort("192.0.2.10:40010"), netip.MustParseAddrPort("203.0.113.10:443")
	hotSource, hotDestination := netip.MustParseAddrPort("192.0.2.11:40011"), netip.MustParseAddrPort("203.0.113.11:443")
	if !observation.WatchTcpFlow(source, destination) || !observation.WatchTcpFlow(hotSource, hotDestination) {
		t.Fatal("tuple registration failed")
	}
	hotPacket := privateReceivePipelineTestPacket(hotDestination, hotSource, 24, 1, 1, 1280)
	hotContext := PrivateReceivePipelineContext{Stage: "native_tun_write_return", ResultKnown: true, Success: true}
	for range privateReceivePipelineFirstCapacity {
		PrivateObserveReceivePipelinePacket(hotContext, hotPacket)
	}
	generation, message, client, peer := NewId(), NewId(), NewId(), NewId()
	packet := privateReceivePipelineTestPacket(destination, source, 24, 9001, 7001, 713)
	pack := &protocol.Pack{SequenceId: generation[:], MessageId: message[:], SequenceNumber: 37, LogicalLane: 2,
		Frames: []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketFromProvider, MessageBytes: packet, Raw: true}}}
	context := PrivateReceivePipelineContext{Stage: "client_decode_paid", ClientId: client, PeerId: peer, SequenceId: NewId(), MessageId: NewId(), SequenceNumber: 999, ResultKnown: true, Success: true, TransportType: TransportTypeH1}
	privateObserveReceivePipelineTransfer(context, privateReceivePipelineTestMarshal(t, &protocol.TransferFrame{Pack: pack}))
	ack := privateReceivePipelineTestPacket(source, destination, 16, 7001, 9714, 0)
	PrivateObserveReceivePipelinePacket(PrivateReceivePipelineContext{Stage: "native_sdk_offer_enter"}, ack)
	for range 100000 {
		PrivateObserveReceivePipelinePacket(hotContext, hotPacket)
	}
	report := observation.Snapshot()
	if report.Schema != 2 || report.Recorded != 100130 || report.Overwritten != report.Recorded-privateReceivePipelineCapacity || len(report.Rows) != 512 || len(report.LatestByFlow) != 3 || report.LostRecordLock != 0 || report.UntrackedLatestStage != 0 {
		t.Fatalf("bounded coverage changed: recorded=%d overwritten=%d rows=%d cells=%d loss=%d untracked=%d", report.Recorded, report.Overwritten, len(report.Rows), len(report.LatestByFlow), report.LostRecordLock, report.UntrackedLatestStage)
	}
	for _, row := range report.Rows {
		if row.FlowOrdinal == 1 {
			t.Fatal("control failed to overwrite the stalled flow's chronological rows")
		}
	}
	var dataCell, ackCell *PrivateReceivePipelineLatest
	for index := range report.LatestByFlow {
		cell := &report.LatestByFlow[index]
		if cell.Latest.FlowOrdinal == 1 && cell.Class == "data" {
			dataCell = cell
		}
		if cell.Latest.FlowOrdinal == 1 && cell.Class == "ack_only" {
			ackCell = cell
		}
	}
	if dataCell == nil || ackCell == nil {
		t.Fatal("stalled flow's last data or ACK header was lost to unrelated bulk")
	}
	row := dataCell.Latest
	if dataCell.Count != 1 || dataCell.FirstAtNs != row.AtNs || !row.Reverse || row.Stage != "client_decode_paid" ||
		!row.TransferIdentityKnown || row.GenerationToken != privateReceivePipelineToken(generation) || row.MessageToken != privateReceivePipelineToken(message) ||
		row.ClientToken != privateReceivePipelineToken(client) || row.PeerToken != privateReceivePipelineToken(peer) || row.SequenceNumber != 37 || row.LogicalLane != 2 ||
		row.TcpSequence != 9001 || row.TcpAcknowledgment != 7001 || row.TcpPayloadBytes != 713 || !row.ResultKnown || !row.Success {
		t.Fatalf("last data lost actual wire identity or copied TCP facts: %+v", dataCell)
	}
	if ackCell.Count != 1 || ackCell.Latest.Reverse || ackCell.Latest.TcpAcknowledgment != 9714 || ackCell.Latest.ResultKnown || ackCell.Latest.TransferIdentityKnown {
		t.Fatal("observed native ACK gained delivery or Transfer authority")
	}
	// A later regression is a later observation, never replaced with a maximum.
	binary.BigEndian.PutUint32(ack[28:32], 8000)
	PrivateObserveReceivePipelinePacket(PrivateReceivePipelineContext{Stage: "native_sdk_offer_enter"}, ack)
	for _, cell := range observation.Snapshot().LatestByFlow {
		if cell.Latest.FlowOrdinal == 1 && cell.Class == "ack_only" && (cell.Count != 2 || cell.Latest.TcpAcknowledgment != 8000) {
			t.Fatal("latest observation was incorrectly promoted to a monotonic ACK frontier")
		}
	}
}

// Direction, edge and payload class have independent cells. Contention and
// closure remain explicit losses instead of overwriting a retained fact.
func TestPrivateReceivePipelineLatestPreservesClassEdgeAndClosure(t *testing.T) {
	observation := privateReceivePipelineTestBegin(t)
	source, destination := netip.MustParseAddrPort("192.0.2.20:40020"), netip.MustParseAddrPort("203.0.113.20:443")
	if !observation.WatchTcpFlow(source, destination) {
		t.Fatal("tuple registration failed")
	}
	data := privateReceivePipelineTestPacket(destination, source, 24, 10, 20, 80)
	ack := privateReceivePipelineTestPacket(source, destination, 16, 20, 90, 0)
	first := observation.CapturePacket(data)
	observation.ObserveCaptured(PrivateReceivePipelineContext{Stage: "client_decode_paid", ResultKnown: true, Success: true}, first)
	observation.ObserveCaptured(PrivateReceivePipelineContext{Stage: "native_tun_write_return", ResultKnown: true, Success: false}, first)
	PrivateObserveReceivePipelinePacket(PrivateReceivePipelineContext{Stage: "native_sdk_offer_enter"}, ack)
	for _, flags := range []byte{2, 18, 17, 20, 0} {
		packet := privateReceivePipelineTestPacket(source, destination, flags, 1, 1, 0)
		PrivateObserveReceivePipelinePacket(PrivateReceivePipelineContext{Stage: "native_sdk_offer_enter"}, packet)
	}
	PrivateObserveReceivePipelinePacket(PrivateReceivePipelineContext{Stage: "unsupported_new_edge"}, ack)
	before := observation.Snapshot()
	if len(before.LatestByFlow) != 3 || before.UntrackedLatestStage != 1 {
		t.Fatalf("handshake/control/unknown edge was silently classified as pure ACK: cells=%d unknown=%d", len(before.LatestByFlow), before.UntrackedLatestStage)
	}
	for index := range data {
		data[index] = 0xff
	}
	observation.stateLock.Lock()
	observation.ObserveCaptured(PrivateReceivePipelineContext{Stage: "client_decode_paid"}, first)
	observation.stateLock.Unlock()
	afterLoss := observation.Snapshot()
	if afterLoss.LostRecordLock != before.LostRecordLock+1 || !reflect.DeepEqual(before.LatestByFlow, afterLoss.LatestByFlow) {
		t.Fatal("contended producer changed a cell or hid its observation loss")
	}
	observation.Close()
	observation.ObserveCaptured(PrivateReceivePipelineContext{Stage: "client_decode_paid"}, first)
	afterClose := observation.Snapshot()
	if !afterClose.Closed || !reflect.DeepEqual(before.LatestByFlow, afterClose.LatestByFlow) || afterClose.Recorded != before.Recorded {
		t.Fatal("closed observation accepted a late producer or reread poisoned bytes")
	}
}
