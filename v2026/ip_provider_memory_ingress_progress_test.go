package connect

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The Android provider's failed probe windows had 7,000 and 20,260 bytes
// available in its 2,936,012-byte NAT child. Those snapshots do not identify
// the historical request's packets. This fixture isolates the corresponding
// physical boundary: a previously established TCP flow, ordinary ingress via
// the provider callback, the real NAT socket writer, and a returned response.
// No carrier, Internet endpoint, TLS, device, or host route is involved.
func TestNatProviderMemoryTcpRequestProgressWithIngressHeadroom(t *testing.T) {
	for _, version := range []int{1, 2} {
		for _, headroom := range []ByteCount{7_000, 20_260, 0} {
			t.Run(fmt.Sprintf("v%d/free-%d", version, headroom), func(t *testing.T) {
				assertMessagePoolOwnership(t)
				synctest.Test(t, func(t *testing.T) {
					testNatProviderMemoryTcpRequestProgress(t, version, headroom, headroom != 0)
				})
			})
		}
	}
}

func testNatProviderMemoryTcpRequestProgress(t *testing.T, version int, headroom ByteCount, wantSuccess bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	root := NewTransferMemoryBudget(mib(13))
	budget := NewTransferMemoryBudgetWithParent(2_936_012, root)
	settings := DefaultLocalUserNatSettings()
	settings.MemoryBudget, settings.Log = budget, NewNoopLogger()
	local, origin := net.Pipe()
	defer origin.Close()
	settings.TcpBufferSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(context.Context, string, string) (net.Conn, error) { return local, nil },
	}
	nat, err := TryNewLocalUserNat(ctx, "ingress-headroom", settings)
	if err != nil {
		t.Fatal(err)
	}
	defer nat.CloseAndWait(context.Background())
	clientSettings := DefaultClientSettings()
	clientSettings.Log = NewNoopLogger()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
	defer client.CloseAndWait(context.Background())
	providerSettings := DefaultRemoteUserNatProviderSettings()
	providerSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
	providerSettings.WriteTimeout = 0
	provider, err := TryNewRemoteUserNatProvider(client, nat, providerSettings)
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	// The receiver below represents the tunneled TCP peer. The original
	// provider's return Transfer path is outside this ingress-boundary test.
	provider.localUserNatUnsub()
	source := SourceId(NewId())
	peer := Peer{ProvideMode: protocol.ProvideMode_Network}
	sendPacket := func(packet []byte) {
		defer MessagePoolReturn(packet)
		frame, err := ipPacketToProviderFrame(packet, version)
		if err != nil {
			t.Error(err)
			return
		}
		if !frame.Raw {
			defer MessagePoolReturn(frame.MessageBytes)
		}
		provider.ClientReceive(source, []*protocol.Frame{frame}, peer)
	}
	path := &IpPath{Version: 4, Protocol: IpProtocolTcp,
		SourceIp: net.IPv4(10, 0, 0, 1).To4(), SourcePort: 40000,
		DestinationIp: net.IPv4(203, 0, 113, 1).To4(), DestinationPort: 443}
	request := bytes.Repeat([]byte{0x5a}, 500)
	response := []byte("bounded-origin-response")
	var peerAck, peerSequence atomic.Uint32
	peerSequence.Store(101)
	packet := func(flags byte, payload []byte) []byte {
		state := ConnectionState{ipVersion: 4,
			sourceIp: path.DestinationIp, sourcePort: uint16(path.DestinationPort),
			destinationIp: path.SourceIp, destinationPort: uint16(path.SourcePort),
			sendSeq: peerAck.Load(), windowSize: 65535}
		return state.tcpPacket(flags, peerSequence.Load(), payload)
	}
	handshake, complete := make(chan struct{}), make(chan struct{})
	var handshakeOnce, completeOnce sync.Once
	var responseBytes atomic.Int64
	nat.AddReceivePacketCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
		_, sourceIp, destinationIp, transport, ok := parseIpv4(packet)
		var tcp parsedTcp
		if !ok || !parseTcpPacket(sourceIp, destinationIp, transport, &tcp) || (!tcp.syn && len(tcp.payload) == 0) {
			return
		}
		ack := tcp.seq + uint32(len(tcp.payload))
		if tcp.syn {
			ack++
		}
		peerAck.Store(ack)
		state := ConnectionState{ipVersion: 4,
			sourceIp: path.DestinationIp, sourcePort: uint16(path.DestinationPort),
			destinationIp: path.SourceIp, destinationPort: uint16(path.SourcePort),
			sendSeq: ack, windowSize: 65535}
		sendPacket(state.tcpPacket(tcpFlagAck, peerSequence.Load(), nil))
		if tcp.syn {
			handshakeOnce.Do(func() { close(handshake) })
		} else if bytes.Equal(tcp.payload, response) {
			responseBytes.Add(int64(len(tcp.payload)))
			completeOnce.Do(func() { close(complete) })
		}
	})
	sendPacket(MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagSyn, 100, nil)))
	select {
	case <-handshake:
	case <-time.After(time.Second):
		t.Fatal("unpressured TCP handshake failed")
	}
	var originBytes, decodedPackets atomic.Int64
	provider.beforeLocalUserNatAdmissionForTest = func(_ Id, packets [][]byte) {
		for _, packet := range packets {
			if !smallNatControlPacket(packet) {
				decodedPackets.Add(1)
			}
		}
	}
	written := make(chan error, 1)
	go func() {
		read := make([]byte, len(request))
		n, err := io.ReadFull(origin, read)
		originBytes.Add(int64(n))
		if err == nil && !bytes.Equal(read, request) {
			err = fmt.Errorf("origin received altered request")
		}
		if err == nil {
			_, err = origin.Write(response)
		}
		written <- err
	}()
	synctest.Wait()
	fill := budget.Available() - headroom
	if fill < 0 || !budget.TryReserve(fill) || budget.Available() != headroom {
		t.Fatal("could not establish exact retained-owner pressure")
	}
	defer budget.Release(fill)
	// Even the baseline can deliver ACK-only traffic during the same stall.
	beforeAck := provider.PacketStats().RemoteIngressPacketCount
	sendPacket(packet(tcpFlagAck, nil))
	synctest.Wait()
	if provider.PacketStats().RemoteIngressPacketCount != beforeAck+1 {
		t.Fatal("pressure also broke the independent ACK-only control")
	}
	start := time.Now()
	deadline := start.Add(20 * time.Second)
	attempts := 0
	succeeded := false
	for time.Now().Before(deadline) {
		attempts++
		outbound := packet(tcpFlagAck|tcpFlagPsh, request)
		// Each attempt is the same TCP sequence, not a new application write.
		sendPacket(outbound)
		synctest.Wait()
		select {
		case <-complete:
			succeeded = true
		default:
		}
		if succeeded {
			break
		}
		time.Sleep(time.Second)
	}
	if budget.UsedByteCount() > 2_936_012 || root.UsedByteCount() > 2_936_012 {
		t.Fatal("progress escaped the original NAT cap")
	}
	t.Logf("headroom=%d ordinary_workspace=%d deadline=20s elapsed=%s attempts=%d nat_boundary=%d origin_bytes=%d response_bytes=%d success=%t nat_used=%d root_used=%d",
		headroom, providerFrameOperationBytes([]*protocol.Frame{{MessageBytes: make([]byte, len(request)+44)}}),
		time.Since(start), attempts, decodedPackets.Load(), originBytes.Load(), responseBytes.Load(), succeeded,
		budget.UsedByteCount(), root.UsedByteCount())
	if succeeded != wantSuccess {
		t.Fatalf("bounded established request success=%t, want %t; ACK-only control passed", succeeded, wantSuccess)
	}
	if succeeded {
		if err := <-written; err != nil {
			t.Fatal(err)
		}
		if originBytes.Load() != int64(len(request)) || responseBytes.Load() != int64(len(response)) {
			t.Fatal("response deadline passed without exact request/response completion")
		}
	}
	// Closing the fake origin releases the failed read before joining owners.
	origin.Close()
	nat.CloseAndWait(context.Background())
}
