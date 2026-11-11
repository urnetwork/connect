package connect

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func TestNatProviderMemoryAckIngressAtFullDataBudget(t *testing.T) {
	assertMessagePoolOwnership(t)
	root := NewTransferMemoryBudget(mib(13))
	budget := NewTransferMemoryBudgetWithParent(mib(2), root)
	nat := newNatProviderMemoryNat(t, budget)
	provider := newNatProviderMemory(t, newNatProviderMemoryClient(t), nat, func(settings *RemoteUserNatProviderSettings) {
		settings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
	})
	// This isolates ingress: the NAT's no-flow RST is irrelevant to admission.
	provider.localUserNatUnsub()
	path := &IpPath{Version: 4, Protocol: IpProtocolTcp,
		SourceIp: net.IPv4(10, 0, 0, 1).To4(), SourcePort: 40000,
		DestinationIp: net.IPv4(203, 0, 113, 1).To4(), DestinationPort: 443}
	packet := MessagePoolCopy(ipOosTcpPacket(path, tcpFlagAck, nil))
	defer MessagePoolReturn(packet)
	frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: packet}
	forwarded := 0
	provider.beforeLocalUserNatAdmissionForTest = func(_ Id, packets [][]byte) { forwarded += len(packets) }
	fill := budget.Available()
	if !budget.TryReserve(fill) {
		t.Fatal("could not saturate NAT data budget")
	}
	defer budget.Release(fill)
	provider.ClientReceive(SourceId(NewId()), []*protocol.Frame{frame}, Peer{ProvideMode: protocol.ProvideMode_Network})
	if forwarded != 1 {
		t.Fatal("full data budget prevented a releasing ACK from reaching the prepaid NAT control path")
	}
	if budget.UsedByteCount() > mib(2) || root.UsedByteCount() > mib(2) {
		t.Fatal("control progress overdrew its admitted parent")
	}
}

func TestNatProviderMemoryMixedControlPacksNormalizeBorrowedRoots(t *testing.T) {
	for _, encoding := range []string{"raw", "legacy"} {
		for _, pressure := range []string{"full", "scratch-only"} {
			t.Run(encoding+"/"+pressure, func(t *testing.T) {
				assertMessagePoolOwnership(t)
				root := NewTransferMemoryBudget(mib(13))
				budget := NewTransferMemoryBudgetWithParent(mib(2), root)
				nat := newNatProviderMemoryNat(t, budget)
				provider := newNatProviderMemory(t, newNatProviderMemoryClient(t), nat, func(settings *RemoteUserNatProviderSettings) {
					settings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
				})
				provider.localUserNatUnsub()
				path := &IpPath{Version: 4, Protocol: IpProtocolTcp,
					SourceIp: net.IPv4(10, 0, 0, 1).To4(), SourcePort: 40000,
					DestinationIp: net.IPv4(203, 0, 113, 1).To4(), DestinationPort: 443}
				makeFrame := func(flags uint8, payload []byte) *protocol.Frame {
					// Physical raw frames often borrow part of a larger Pack root.
					root := MessagePoolGet(2048)
					packet := root[:copy(root, ipOosTcpPacket(path, flags, payload))]
					t.Cleanup(func() { MessagePoolReturn(packet) })
					version := 2
					if encoding == "legacy" {
						version = 1
					}
					frame, err := ipPacketToProviderFrame(packet, version)
					if err != nil {
						t.Fatal(err)
					}
					if !frame.Raw {
						t.Cleanup(func() { MessagePoolReturn(frame.MessageBytes) })
					}
					return frame
				}
				frames := []*protocol.Frame{makeFrame(tcpFlagAck, []byte{1}), makeFrame(tcpFlagAck, nil)}
				path.Version, path.SourceIp, path.DestinationIp = 6, net.ParseIP("fd00::1"), net.ParseIP("2001:db8::1")
				frames = append(frames, makeFrame(tcpFlagRst, nil))
				controls := 0
				provider.beforeLocalUserNatAdmissionForTest = func(_ Id, packets [][]byte) {
					for _, packet := range packets {
						if !smallNatControlPacket(packet) {
							continue
						}
						controls++
						if cap(packet) > 512 {
							t.Errorf("small control retained borrowed %d-byte Pack root", cap(packet))
						}
					}
				}
				fill := budget.Available()
				if pressure == "scratch-only" {
					fill -= providerFrameOperationBytes(frames)
				}
				if !budget.TryReserve(fill) {
					t.Fatal("could not fill data budget")
				}
				defer budget.Release(fill)
				provider.ClientReceive(SourceId(NewId()), frames, Peer{ProvideMode: protocol.ProvideMode_Network})
				if controls != 2 || provider.PacketStats().RemoteIngressPacketCount != 2 {
					t.Fatalf("mixed Pack controls did not reach NAT without admitting data: controls=%d stats=%+v", controls, provider.PacketStats())
				}
				if provider.ingressControlMemory.UsedByteCount() != 0 || budget.UsedByteCount() > mib(2) || root.UsedByteCount() > mib(2) {
					t.Fatal("control workspace leaked or escaped its prepaid parent")
				}
			})
		}
	}
}

func TestNatProviderMemoryProtocolCallbacksOmitByteClaimsAndJoinClose(t *testing.T) {
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
	packet := MessagePoolCopy(ipOosTcpPacket(path, tcpFlagAck, nil))
	defer MessagePoolReturn(packet)
	frames := []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: packet}}
	source, peer := SourceId(NewId()), Peer{ProvideMode: protocol.ProvideMode_Network}
	fill := budget.Available()
	if !budget.TryReserve(fill) {
		t.Fatal("could not fill data budget")
	}
	defer budget.Release(fill)
	before := budget.Stats()
	entered, release, received := make(chan struct{}, 2), make(chan struct{}), make(chan struct{}, 2)
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	provider.beforeLocalUserNatAdmissionForTest = func(Id, [][]byte) { entered <- struct{}{}; <-release }
	for range 2 {
		go func() { provider.ClientReceive(source, frames, peer); received <- struct{}{} }()
	}
	joined := false
	defer func() {
		unblock()
		if !joined {
			for range 2 {
				select {
				case <-received:
				case <-time.After(5 * time.Second):
					t.Error("protocol callback did not join after releasing the barrier")
				}
			}
		}
	}()
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("full DATA budget prevented an independent protocol callback")
		}
	}
	if provider.ingressControlMemory.UsedByteCount() != 0 || budget.Stats() != before {
		t.Fatal("protocol callbacks acquired DATA or reserved protocol bytes")
	}
	oversized := make([]*protocol.Frame, providerReturnBatchMaxFrames+1)
	for index := range oversized {
		oversized[index] = frames[0]
	}
	if allocations := testing.AllocsPerRun(100, func() { provider.ClientReceive(source, oversized, peer) }); allocations != 0 || len(entered) != 0 || budget.Stats() != before {
		t.Fatalf("oversized protocol callback escaped the count bound: allocations=%g", allocations)
	}
	// Ordinary DATA return workspace stays independently owned while protocol
	// callback lifetimes are held; protocol progress creates no extra capacity.
	dataPacket := MessagePoolCopy(ipOosTcpPacket(path, tcpFlagAck, []byte("data")))
	defer MessagePoolReturn(dataPacket)
	returnMemory, admitted := provider.startReturnMemoryOperation([][]byte{dataPacket}, receiveRecoveryModeTcpSocket)
	if !admitted {
		t.Fatal("ingress control consumed the synchronous return workspace")
	}
	provider.finishMemoryOperation(&returnMemory)
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
		t.Fatal("provider released its fixed claim while control work remained")
	default:
	}
	unblock()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("provider close did not join ingress control")
	}
	for range 2 {
		<-received
	}
	joined = true
	if provider.ingressControlMemory.UsedByteCount() != 0 || budget.UsedByteCount() != natMemoryFixedBytes+fill {
		t.Fatalf("closed control owner leaked claims: %+v", budget.Stats())
	}
	if err := nat.CloseAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stats := budget.Stats(); stats.UsedByteCount != fill || stats.ReservedByteCount-stats.ReleasedByteCount != fill {
		t.Fatalf("control teardown accounting imbalance: %+v", stats)
	}
}
