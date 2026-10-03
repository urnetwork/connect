package connect

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func BenchmarkTcpCollapseGateCommit(b *testing.B) {
	client, update := collapseTestClient(time.Hour)
	defer update.Close()
	selected := &multiClientChannel{settings: client.settings}
	packet := collapseTestPacket(100, 1000, 1, false, false)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		packet.ipPath.SequenceNumber++
		if !client.canSendPacket(packet, update, selected) {
			b.Fatal("new byte collapsed")
		}
		update.updateSequence(packet)
		if client.canSendPacket(packet, update, selected) {
			b.Fatal("duplicate byte admitted")
		}
	}
}

// Includes fresh parsed packet/group descriptors, the real collapse gate,
// MultiClient accounting, raw frame construction and the existing completion
// closure. Only Transfer's final queue is an immediate ownership-taking seam;
// this is a CPU/allocation microbenchmark, not a network-performance claim.
func BenchmarkTcpCollapseParsedAdmission(b *testing.B) {
	for _, count := range []int{1, 8} {
		b.Run(fmt.Sprintf("packets=%d", count), func(b *testing.B) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			settings := DefaultMultiClientSettings()
			settings.TcpCollapsePrevention = true
			settings.ProtocolVersion = DefaultProtocolVersion
			parent := &RemoteUserNatMultiClient{ctx: ctx, settings: settings, log: NewNoopLogger()}
			update := newMultiClientChannelUpdate(ctx, nil)
			defer update.Close()
			selected := newPacketTransferTestChannel()
			selected.ctx, selected.settings = ctx, settings
			update.client.Store(selected)
			parent.sendClientPathForTest = func(_ *IpPath, _ flowPin, cb func(*multiClientChannelUpdate, *multiClientChannel)) {
				cb(update, selected)
			}
			var owned []parsedPacket
			selected.sendTransferForTest = func(completed func(error)) (bool, error) {
				completed(nil)
				for _, packet := range owned {
					MessagePoolReturn(packet.packet)
				}
				return true, nil
			}
			paths := make([]IpPath, count)
			path := icmpTcpTestPath(4)
			template := ipOosTcpPacketSequence(path, tcpFlagAck, 100, []byte{1})
			for i := range paths {
				paths[i] = *path
				paths[i].Ack = true
			}
			source := SourceId(NewId())
			sequence := uint32(100)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				owned = make([]parsedPacket, count)
				for i := range owned {
					packet := MessagePoolCopy(template)
					binary.BigEndian.PutUint32(packet[24:28], sequence)
					paths[i].SequenceNumber = sequence
					sequence++
					owned[i] = parsedPacket{packet: packet, ipPath: &paths[i], payload: packet[len(packet)-1:]}
				}
				group := &parsedPacketGroup{packets: owned, ipPath: &paths[0], byteCount: ByteCount(len(template) * count)}
				if !parent.sendParsedPacketGroup(source, protocol.ProvideMode_Network, group, 0) {
					b.Fatal("new contiguous data admission failed")
				}
			}
		})
	}
}
