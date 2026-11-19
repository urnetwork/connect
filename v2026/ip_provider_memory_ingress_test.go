package connect

import (
	"context"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

func TestNatProviderMemoryIngressDoesNotFinanceNewFlow(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(mib(2))
		settings := DefaultLocalUserNatSettings()
		settings.MemoryBudget, settings.Log = budget, NewNoopLogger()
		local, origin := net.Pipe()
		defer origin.Close()
		dialed := make(chan struct{}, 1)
		settings.TcpBufferSettings.DialContextSettings = &DialContextSettings{
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				dialed <- struct{}{}
				return local, nil
			},
		}
		nat, err := TryNewLocalUserNat(t.Context(), "new-flow-cap", settings)
		if err != nil {
			t.Fatal(err)
		}
		defer nat.CloseAndWait(context.Background())
		provider := newNatProviderMemory(t, newNatProviderMemoryClient(t), nat, func(settings *RemoteUserNatProviderSettings) {
			settings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
		})
		provider.localUserNatUnsub()
		path := &IpPath{Version: 4, Protocol: IpProtocolTcp,
			SourceIp: net.IPv4(10, 0, 0, 1).To4(), SourcePort: 40000,
			DestinationIp: net.IPv4(203, 0, 113, 1).To4(), DestinationPort: 443}
		packet := MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagSyn, 100, nil))
		defer MessagePoolReturn(packet)
		frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: packet}
		source, peer := SourceId(NewId()), Peer{ProvideMode: protocol.ProvideMode_Network}
		fill := budget.Available() - 7_000
		if !budget.TryReserve(fill) {
			t.Fatal("could not fill parent")
		}
		defer func() { budget.Release(fill) }()
		provider.ClientReceive(source, []*protocol.Frame{frame}, peer)
		synctest.Wait()
		select {
		case <-dialed:
			t.Fatal("scratch reserve financed an unadmitted new TCP flow")
		default:
		}
		if budget.Available() != 7_000 {
			t.Fatal("refused new flow leaked transient packet capacity")
		}
		// Releasing genuine flow capacity, not scratch capacity, enables the
		// same SYN. New connection limits are deliberately not relaxed.
		budget.Release(fill)
		fill = 0
		provider.ClientReceive(source, []*protocol.Frame{frame}, peer)
		synctest.Wait()
		select {
		case <-dialed:
		default:
			t.Fatal("available flow capacity did not permit the control SYN")
		}
	})
}

func TestNatProviderMemoryIngressWorkspaceEnvelope(t *testing.T) {
	for _, size := range []int{0, 40, 256, 512, 1100, 1500, packetPoolSize} {
		frame := &protocol.Frame{MessageBytes: make([]byte, size)}
		if required := providerFrameOperationBytes([]*protocol.Frame{frame}); required > natProviderIngressBytes {
			t.Fatalf("%d-byte frame needs %d bytes, reserved slot=%d", size, required, natProviderIngressBytes)
		}
	}
	if natProviderMemoryByteCount(natProviderSourceLimit) != kib(576) {
		t.Fatal("ordinary workspace is not charged in the fixed provider claim")
	}
}

func TestNatProviderMemoryIngressWorkspaceIndependentAndJoinsClose(t *testing.T) {
	assertMessagePoolOwnership(t)
	budget := NewTransferMemoryBudget(mib(2))
	nat := newNatProviderMemoryNat(t, budget)
	provider := newNatProviderMemory(t, newNatProviderMemoryClient(t), nat, func(settings *RemoteUserNatProviderSettings) {
		settings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
	})
	provider.localUserNatUnsub()
	path := &IpPath{Version: 4, Protocol: IpProtocolTcp,
		SourceIp: net.IPv4(10, 0, 0, 1).To4(), SourcePort: 40000,
		DestinationIp: net.IPv4(203, 0, 113, 1).To4(), DestinationPort: 443}
	packet := MessagePoolCopy(ipOosTcpPacket(path, tcpFlagAck, make([]byte, 600)))
	defer MessagePoolReturn(packet)
	frames := []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: packet}}
	ack := MessagePoolCopy(ipOosTcpPacket(path, tcpFlagAck, nil))
	defer MessagePoolReturn(ack)
	ackFrames := []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: ack}}
	source, peer := SourceId(NewId()), Peer{ProvideMode: protocol.ProvideMode_Network}
	fill := budget.Available() - 7_000
	if !budget.TryReserve(fill) {
		t.Fatal("could not establish scratch pressure")
	}
	defer budget.Release(fill)
	entered, release, received := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	provider.beforeLocalUserNatAdmissionForTest = func(_ Id, packets [][]byte) {
		if !smallNatControlPacket(packets[0]) {
			close(entered)
			<-release
		}
	}
	go func() { provider.ClientReceive(source, frames, peer); close(received) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("ordinary ingress did not acquire its prepaid workspace")
	}
	if provider.ingressMemory.UsedByteCount() != natProviderIngressBytes || provider.ingressControlMemory.UsedByteCount() != 0 {
		t.Fatal("ordinary input consumed the ACK reserve or failed to retain its own slot")
	}
	if allocations := testing.AllocsPerRun(100, func() { provider.ClientReceive(source, frames, peer) }); allocations != 0 {
		t.Fatalf("full ingress workspace allocated %g objects per refused callback", allocations)
	}
	beforeAck := provider.PacketStats().RemoteIngressPacketCount
	provider.ClientReceive(source, ackFrames, peer)
	if provider.PacketStats().RemoteIngressPacketCount != beforeAck+1 {
		t.Fatal("occupied ordinary workspace blocked an ACK")
	}
	// Both synchronous return slots remain available as well: no borrowed
	// control capacity or return-producer capacity finances ordinary ingress.
	first, ok := provider.startReturnMemoryOperation([][]byte{packet}, receiveRecoveryModeTcpSocket)
	if !ok {
		t.Fatal("first return workspace consumed by ingress")
	}
	second, ok := provider.startReturnMemoryOperation([][]byte{packet}, receiveRecoveryModeTcpSocket)
	if !ok {
		provider.finishMemoryOperation(&first)
		t.Fatal("second return workspace consumed by ingress")
	}
	provider.finishMemoryOperation(&first)
	provider.finishMemoryOperation(&second)
	closing, closed := make(chan struct{}), make(chan struct{})
	provider.beforeReturnAdmissionsWaitForTest = func() { close(closing) }
	go func() { provider.Close(); close(closed) }()
	select {
	case <-closing:
	case <-time.After(5 * time.Second):
		t.Fatal("provider close did not start")
	}
	select {
	case <-closed:
		t.Fatal("provider released its fixed claim while ingress retained the workspace")
	default:
	}
	unblock()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("provider close failed to join ingress")
	}
	<-received
	if provider.ingressMemory.UsedByteCount() != 0 || provider.ingressControlMemory.UsedByteCount() != 0 {
		t.Fatal("closed ingress workspace leaked its subclaim")
	}
	nat.CloseAndWait(context.Background())
	if stats := budget.Stats(); stats.UsedByteCount != fill || stats.ReservedByteCount-stats.ReleasedByteCount != fill {
		t.Fatalf("closed ingress leaked parent ownership: %+v", stats)
	}
}

func TestNatProviderMemoryIngressFallbackNormalizesAndBoundsFrames(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(map[int]string{1: "legacy", 2: "raw"}[version], func(t *testing.T) {
			assertMessagePoolOwnership(t)
			budget := NewTransferMemoryBudget(mib(2))
			nat := newNatProviderMemoryNat(t, budget)
			provider := newNatProviderMemory(t, newNatProviderMemoryClient(t), nat, func(settings *RemoteUserNatProviderSettings) {
				settings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
			})
			provider.localUserNatUnsub()
			path := &IpPath{Version: 4, Protocol: IpProtocolTcp,
				SourceIp: net.IPv4(10, 0, 0, 1).To4(), SourcePort: 40000,
				DestinationIp: net.IPv4(203, 0, 113, 1).To4(), DestinationPort: 443}
			borrowed := MessagePoolGet(8192)
			borrowed = borrowed[:copy(borrowed, ipOosTcpPacket(path, tcpFlagAck, make([]byte, 100)))]
			defer MessagePoolReturn(borrowed)
			frame, err := ipPacketToProviderFrame(borrowed, version)
			if err != nil {
				t.Fatal(err)
			}
			if !frame.Raw {
				defer MessagePoolReturn(frame.MessageBytes)
			}
			fill := budget.Available() - 7_000
			if !budget.TryReserve(fill) {
				t.Fatal("could not fill parent")
			}
			defer budget.Release(fill)
			forwarded := 0
			provider.beforeLocalUserNatAdmissionForTest = func(_ Id, packets [][]byte) {
				forwarded += len(packets)
				for _, packet := range packets {
					if cap(packet) > int(retainedMessageCapacity(ByteCount(len(packet)))) {
						t.Errorf("small packet escaped with %d-byte parent capacity", cap(packet))
					}
				}
			}
			source, peer := SourceId(NewId()), Peer{ProvideMode: protocol.ProvideMode_Network}
			provider.ClientReceive(source, []*protocol.Frame{frame}, peer)
			if forwarded != 1 {
				t.Fatalf("normalized small frame forwarded %d times", forwarded)
			}
			// Do not let an arbitrarily large frame, unrelated message, or
			// malformed legacy packet grow the prepaid workspace.
			invalid := []*protocol.Frame{
				nil,
				{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: make([]byte, packetPoolSize+1)},
				{MessageType: protocol.MessageType_TestSimpleMessage},
				{MessageType: protocol.MessageType_IpIpPacketToProvider, MessageBytes: []byte{255}},
			}
			provider.ClientReceive(source, invalid, peer)
			if forwarded != 1 || provider.ingressMemory.UsedByteCount() != 0 || budget.UsedByteCount() > budget.TotalByteCount() {
				t.Fatal("invalid frame escaped workspace bounds")
			}
		})
	}
}
