// The same real call site runs against both source sets. Totals include every
// allocation made by admission; no estimated frame or closure cost is removed.
package connect

import (
	"bytes"
	"context"
	"net"
	"sync"
	"testing"
)

// An actual unbuffered source with its reader held before Run guarantees
// refusal. The callback must escape through production send construction in
// both versions, even though no source may take the caller's packet owner.
func BenchmarkTcpGroupPrequeueAllocation(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	settings := closeWaitClientSettings()
	settings.SendBufferSettings.SequenceBufferSize = 0
	settings.SendBufferSettings.PrewarmOpeningContract = false
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	entered := make(chan struct{})
	var once sync.Once
	peer := NewId()
	settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
		if id.Destination == peer {
			once.Do(func() { close(entered) })
			<-ctx.Done()
		}
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	client.ContractManager().AddNoContractPeer(peer)
	selected := newPacketTransferTestChannel()
	selected.ctx, selected.client = ctx, client
	selected.args = &multiClientChannelArgs{Destination: RequireMultiHopId(peer)}
	path := &IpPath{Version: 4, Protocol: IpProtocolTcp,
		SourceIp: net.IPv4(192, 0, 2, 101), DestinationIp: net.IPv4(198, 51, 100, 102),
		SourcePort: 45000, DestinationPort: 443}
	template := ipOosTcpPacketSequence(path, tcpFlagAck, 100, []byte("synthetic!"))
	packet := MessagePoolCopy(template)
	witness := MessagePoolShareReadOnly(packet)
	parsedPath, payload, err := ParseIpPathWithPayload(packet)
	if err != nil {
		b.Fatal(err)
	}
	update := newMultiClientChannelUpdate(ctx, parsedPath)
	update.client.Store(selected)
	b.Cleanup(func() {
		cancel()
		if err := client.CloseAndWait(context.Background()); err != nil {
			b.Errorf("join allocation source: %v", err)
		}
		update.Close()
		if !bytes.Equal(packet, template) {
			b.Error("refused caller packet changed")
		}
		if MessagePoolReturn(packet) {
			b.Error("refusal consumed the caller's shared ownership")
		}
		if !MessagePoolReturn(witness) {
			b.Error("refused original retained an extra production owner")
		}
	})
	offer := func() {
		group := &parsedPacketGroup{
			packets: []parsedPacket{{packet: packet, ipPath: parsedPath, payload: payload}},
			ipPath:  parsedPath, byteCount: ByteCount(len(packet)),
		}
		group.prepareCollapseAdmission(update)
		accepted, err := selected.SendGroupDetailedWithAck(group, 0, true)
		if accepted || err != nil {
			b.Fatalf("unbuffered source without reader admitted=%t err=%v", accepted, err)
		}
	}
	offer()
	<-entered
	// Warm the ordinary pooled/frame and scheduler paths before counting.
	for range 100 {
		offer()
	}
	if update.sequencePacketCount != 0 || update.sequenceCovered {
		b.Fatal("refused warmup published coverage")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		offer()
	}
	b.StopTimer()
	if client.initialSendFrameCount.Load() != 0 {
		b.Fatal("parked source published a frame")
	}
}
