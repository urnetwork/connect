package connect

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// TestRemoteUserNatProviderPacketStats pins the provider's relayed-traffic
// counters: traffic received from remote clients over the tunnel counts as
// remote ingress once handed to the exit local user nat, return traffic into
// the tunnel counts as remote egress once handed to the client send buffer,
// and traffic dropped by the provider security policy counts as blocked.
// Also exercises the epoch packet stats callback.
func TestRemoteUserNatProviderPacketStats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	providerClient := NewClient(ctx, NewId(), NewNoContractClientOob(), DefaultClientSettings())
	defer providerClient.Cancel()

	localUserNat := NewLocalUserNatWithDefaults(ctx, "test-exit")

	providerSettings := DefaultRemoteUserNatProviderSettings()
	// bound the return-path send so a failed enqueue cannot stall the test
	providerSettings.WriteTimeout = 1 * time.Second
	providerSettings.EventEpoch = 50 * time.Millisecond
	provider := NewRemoteUserNatProvider(providerClient, localUserNat, providerSettings)
	defer provider.Close()

	packetStatsChannel := make(chan *PacketStats, 16)
	unsubPacketStats := provider.AddPacketStatsCallback(func(packetStats *PacketStats) {
		select {
		case packetStatsChannel <- packetStats:
		default:
		}
	})
	defer unsubPacketStats()

	source := SourceId(NewId())
	srcIP := net.ParseIP("10.0.0.9")
	dstIP := net.ParseIP("203.0.113.7") // TEST-NET-3: public unicast, unrouteable

	toProviderFrame := func(packet []byte) *protocol.Frame {
		frame, err := ToFrame(&protocol.IpPacketToProvider{
			IpPacket: &protocol.IpPacket{PacketBytes: packet},
		}, DefaultProtocolVersion)
		if err != nil {
			t.Fatalf("to frame: %v", err)
		}
		return frame
	}

	// an allowed outbound flow received from the tunnel is the provider's
	// ingress, counted when handed to the exit local user nat
	outboundPacket := craftSecurityPacket(IpProtocolTcp, srcIP, 42001, dstIP, 8080, false, []byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"))
	provider.ClientReceive(source, []*protocol.Frame{toProviderFrame(outboundPacket)}, Peer{ProvideMode: protocol.ProvideMode_Public})

	stats := provider.PacketStats()
	if stats.RemoteIngressPacketCount != 1 || stats.RemoteIngressByteCount != ByteCount(len(outboundPacket)) {
		t.Fatalf("unexpected ingress stats %+v", stats)
	}

	// a tunneled BitTorrent handshake is dropped by the reversed policy DPI
	// and counts as blocked
	blockedPacket := craftSecurityPacket(IpProtocolTcp, srcIP, 42000, dstIP, 51413, false, bittorrentHandshakePacketPayload())
	provider.ClientReceive(source, []*protocol.Frame{toProviderFrame(blockedPacket)}, Peer{ProvideMode: protocol.ProvideMode_Public})

	stats = provider.PacketStats()
	if stats.BlockIngressPacketCount != 1 || stats.BlockIngressByteCount != ByteCount(len(blockedPacket)) {
		t.Fatalf("unexpected blocked stats %+v", stats)
	}
	if stats.RemoteIngressPacketCount != 1 || stats.BlockEgressPacketCount != 0 {
		t.Fatalf("the blocked packet must only count as block ingress %+v", stats)
	}

	// the return of the outbound flow into the tunnel is the provider's
	// egress, counted when handed to the client send buffer
	returnPacket := craftSecurityPacket(IpProtocolTcp, dstIP, 8080, srcIP, 42001, false, []byte("HTTP/1.1 200 OK\r\n\r\n"))
	returnIpPath, err := ParseIpPath(returnPacket)
	if err != nil {
		t.Fatalf("parse ip path: %v", err)
	}
	provider.Receive(source, protocol.ProvideMode_Public, returnIpPath, returnPacket)

	stats = provider.PacketStats()
	if stats.RemoteEgressPacketCount != 1 || stats.RemoteEgressByteCount != ByteCount(len(returnPacket)) {
		t.Fatalf("unexpected egress stats %+v", stats)
	}

	// a return packet from a statically dropped source endpoint (a bittorrent
	// port) is blocked by the reversed policy's source check
	strayPacket := craftSecurityPacket(IpProtocolTcp, dstIP, 6881, srcIP, 42002, false, []byte("stray"))
	strayIpPath, err := ParseIpPath(strayPacket)
	if err != nil {
		t.Fatalf("parse ip path: %v", err)
	}
	provider.Receive(source, protocol.ProvideMode_Public, strayIpPath, strayPacket)

	stats = provider.PacketStats()
	if stats.BlockEgressPacketCount != 1 || stats.BlockEgressByteCount != ByteCount(len(strayPacket)) {
		t.Fatalf("unexpected stray return stats %+v", stats)
	}
	if stats.BlockIngressPacketCount != 1 || stats.RemoteEgressPacketCount != 1 {
		t.Fatalf("the stray return must only count as block egress %+v", stats)
	}

	// the epoch callback fires with the cumulative counts
	final := *stats
	deadline := time.After(5 * time.Second)
	for {
		select {
		case packetStats := <-packetStatsChannel:
			if *packetStats == final {
				return
			}
			// an earlier epoch's partial snapshot; keep waiting
		case <-deadline:
			t.Fatalf("expected the epoch packet stats callback to reach %+v", final)
		}
	}
}
