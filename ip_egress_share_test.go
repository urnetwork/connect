// C18 at the provider (sidecar design §17.9, D-BRIDGE-14): with a hook that
// takes them, FriendsAndFamily flows to private destinations go to the hook in
// place of the public rules, a share's replies return from its alias for the
// flows the hook passed or mapped and for no other, its returns keep the share's
// mode, and a full flow table sheds foreign flows before local ones. Off, a
// FriendsAndFamily flow to a private destination is an incident as before.
// Aliases are in 100.64.0.0/10 and RFC 1918; upstreams are loopback listeners
// on ports the kernel picks.
package connect

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A share's peer: the seals of a contract from another network.
func newEgressSharePeer() Peer {
	sealed := newEgressLanePeer()
	peer := sealed.peer(ContractClassData)
	peer.ProvideMode = protocol.ProvideMode_FriendsAndFamily
	return peer
}

func egressShareUdpPacket(destination net.IP, destinationPort int) []byte {
	return ipOosUdpPacket(&IpPath{
		Version: 4, Protocol: IpProtocolUdp,
		SourceIp: net.IPv4(192, 0, 2, 2).To4(), SourcePort: 42100,
		DestinationIp: destination.To4(), DestinationPort: destinationPort,
	}, []byte("datagram"))
}

// Off, as today: a FriendsAndFamily datagram to a private destination is an
// incident and never reaches the hook.
func TestEgressShareOffKeepsPrivateDestinationsAnIncident(t *testing.T) {
	assertMessagePoolOwnership(t)
	hook := newEgressDenyHook()
	rig := newEgressLaneProvider(t, hook, nil, nil)
	rig.receive(t, SourceId(NewId()), newEgressSharePeer(), egressShareUdpPacket(net.IPv4(10, 20, 0, 5), 8443))
	if incidents := rig.provider.SecurityPolicyStats(false)[SecurityPolicyResultIncident]; len(incidents) == 0 {
		t.Fatal("a FriendsAndFamily datagram to RFC 1918 was not an incident with the switch off")
	}
	if len(hook.flows) != 0 {
		t.Fatal("the hook was asked about a flow the switch keeps from it")
	}
}

// On: FriendsAndFamily flows to an RFC 1918 or shared-space alias reach the
// hook as FriendsAndFamily and are never incidents; a public destination still
// meets the public rules.
func TestEgressShareOnHandsPrivateDestinationsToTheHook(t *testing.T) {
	assertMessagePoolOwnership(t)
	hook := newEgressDenyHook()
	hook.friendsAndFamily.Store(true)
	rig := newEgressLaneProvider(t, hook, nil, nil)
	source := SourceId(NewId())
	for _, alias := range []net.IP{net.IPv4(10, 20, 0, 5), net.IPv4(100, 80, 1, 30)} {
		rig.receive(t, source, newEgressSharePeer(), egressShareUdpPacket(alias, 22))
		flow := hook.takeFlow(t)
		if flow.ProvideMode != protocol.ProvideMode_FriendsAndFamily || flow.To.Addr() != netip.AddrFrom4([4]byte(alias.To4())) {
			t.Fatalf("the hook saw %+v for a share to %s", flow, alias)
		}
	}
	rig.checkNoIncident(t)
	// a bittorrent port on a public address is still the public rules' drop
	before := rig.provider.packetStatsCounters.blockIngressPacketCount.Load()
	rig.receive(t, source, newEgressSharePeer(), egressShareUdpPacket(net.IPv4(203, 0, 113, 9), 6881))
	if rig.provider.packetStatsCounters.blockIngressPacketCount.Load() != before+1 {
		t.Fatal("a share's public destination skipped the public rules")
	}
}

// A share's reply from its alias's privileged port returns for a flow the hook
// passed or mapped, in the share's own mode even for a source recorded as
// same-network; the same reply for a flow the hook did not admit meets the
// reversed policy, which drops it.
func TestEgressShareReturnAdmitsOnlyFlowsTheHookAdmitted(t *testing.T) {
	assertMessagePoolOwnership(t)
	flow := &IpPath{
		Version: 4, Protocol: IpProtocolTcp,
		SourceIp: net.IPv4(192, 0, 2, 2).To4(), SourcePort: 42101,
		DestinationIp: net.IPv4(100, 80, 1, 30).To4(), DestinationPort: 22,
	}
	reply := ipOosTcpPacketSequence(flow.Reverse(), tcpFlagAck|tcpFlagPsh, 7, []byte("ssh-banner"))
	for _, admitted := range []bool{true, false} {
		provider, source, transferKey, sequence := newProviderSignalReturnFixture(t)
		provider.recordSourceProvideMode(source.SourceId, protocol.ProvideMode_Network)
		path := *flow
		path.egressAdmitted = admitted
		blocked := provider.packetStatsCounters.blockEgressPacketCount.Load()
		// borrowed by the return path, which keeps a share of what it queues
		packet := MessagePoolCopy(reply)
		provider.receiveTransferWithRecovery(source, transferKey, protocol.ProvideMode_FriendsAndFamily,
			receiveRecoveryModeRegenerableControl, &path, packet)
		MessagePoolReturn(packet)
		if !admitted {
			if provider.packetStatsCounters.blockEgressPacketCount.Load() != blocked+1 {
				t.Fatal("a reply for a flow the hook did not admit was not dropped")
			}
			continue
		}
		queued := waitProviderReturnTestPack(t, sequence)
		if queued.NetworkPeer || !queued.CompanionContract {
			queued.returnFrames()
			t.Fatalf("a share's reply returned as %+v, want the share's companion and never same-network", queued.TransferOptions)
		}
		queued.returnFrames()
	}
}

// At the NAT a share's flow maps from its alias to the target, its replies keep
// the alias as their source, and its path is marked for the provider's return;
// a denied flow's is not.
func TestEgressShareFlowRepliesFromItsAlias(t *testing.T) {
	assertMessagePoolOwnership(t)
	listener, port := newEgressTestListener(t)
	hook := newEgressTestHook()
	hook.friendsAndFamily.Store(true)
	hook.setResolver(func(flow *EgressFlow) EgressDecision {
		if flow.ProvideMode != protocol.ProvideMode_FriendsAndFamily || flow.From.Port() != 42102 {
			return EgressDecision{Verdict: EgressVerdictDeny}
		}
		return EgressDecision{Verdict: EgressVerdictMap, Target: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port))}
	})
	rig := newEgressTestNat(t, hook, nil)
	alias := &IpPath{
		Version: 4, Protocol: IpProtocolTcp,
		SourceIp: net.IPv4(192, 0, 2, 2).To4(), SourcePort: 42102,
		DestinationIp: net.IPv4(100, 80, 1, 30).To4(), DestinationPort: 22,
	}
	sendShare := func(packet []byte) {
		owned := MessagePoolCopy(packet)
		if !rig.nat.SendPacket(rig.source, protocol.ProvideMode_FriendsAndFamily, owned, -1) {
			MessagePoolReturn(owned)
			t.Fatal("the NAT refused a share's packet")
		}
	}
	var admittedPath, deniedPath *IpPath
	rig.nat.addReceiveTransferPacketCallback(func(_ TransferPath, _ TransferKey, _ protocol.ProvideMode, _ receiveRecoveryMode, ipPath *IpPath, _ []byte) {
		if ipPath.SourcePort == 42102 {
			admittedPath = ipPath
		} else {
			deniedPath = ipPath
		}
	})
	sendShare(egressTestTcpPacket(alias, tcpFlagSyn, 100, 0, nil))
	synAck := egressTestParse(t, rig.receive())
	if !synAck.Syn || !synAck.SourceIp.Equal(alias.DestinationIp) || synAck.SourcePort != 22 {
		t.Fatalf("the share's SYN-ACK came from %s:%d, not its alias", synAck.SourceIp, synAck.SourcePort)
	}
	upstream, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	upstream.Close()
	denied := *alias
	denied.SourcePort = 42103
	sendShare(egressTestTcpPacket(&denied, tcpFlagSyn, 100, 0, nil))
	if !egressTestParse(t, rig.receive()).Rst {
		t.Fatal("the denied share flow was not reset")
	}
	rig.close()
	if admittedPath == nil || !admittedPath.egressAdmitted {
		t.Fatal("the mapped share flow's path was not marked admitted")
	}
	if deniedPath == nil || deniedPath.egressAdmitted {
		t.Fatal("the denied share flow's path was marked admitted")
	}
}

// One flow table's resources, for the eviction rule alone.
type egressShedTestFlow struct {
	userLimited
	foreign  bool
	canceled bool
}

func (self *egressShedTestFlow) Cancel() {
	self.canceled = true
}

// Within a sample, the oldest foreign flow goes before an older local one;
// with no foreign flow, the oldest goes; with no preference, the rule is the
// plain LRU.
func TestEgressShareShedsForeignFlowsFirst(t *testing.T) {
	newFlow := func(age time.Duration, foreign bool) *egressShedTestFlow {
		flow := &egressShedTestFlow{foreign: foreign}
		flow.lastActivityNanos.Store(monotonicNanos() - int64(age))
		return flow
	}
	prefer := func(flow *egressShedTestFlow) bool { return flow.foreign }
	shed := func(flows map[string]*egressShedTestFlow, prefer func(*egressShedTestFlow) bool) string {
		evicted := ""
		applyLruMapLimitPreferring(flows, len(flows)-1, prefer, func(key string, flow *egressShedTestFlow) bool {
			evicted = key
			delete(flows, key)
			return true
		})
		return evicted
	}
	mixed := func() map[string]*egressShedTestFlow {
		return map[string]*egressShedTestFlow{
			"old local":     newFlow(3*time.Minute, false),
			"older foreign": newFlow(2*time.Minute, true),
			"new foreign":   newFlow(time.Minute, true),
		}
	}
	if evicted := shed(mixed(), prefer); evicted != "older foreign" {
		t.Fatalf("with shares shed %q, want the oldest foreign flow", evicted)
	}
	if evicted := shed(mixed(), nil); evicted != "old local" {
		t.Fatalf("without shares shed %q, want the oldest flow", evicted)
	}
	local := map[string]*egressShedTestFlow{
		"old local": newFlow(3*time.Minute, false),
		"new local": newFlow(time.Minute, false),
	}
	if evicted := shed(local, prefer); evicted != "old local" {
		t.Fatalf("with no foreign flow shed %q, want the oldest", evicted)
	}
}

// Foreign flows churning past a full table never evict a healthy local flow,
// which keeps forwarding.
func TestEgressShareChurnLeavesALocalFlowForwarding(t *testing.T) {
	assertMessagePoolOwnership(t)
	sink, port := newEgressTestUdpSink(t)
	hook := newEgressTestHook()
	hook.friendsAndFamily.Store(true)
	opened := make(chan *UdpSequence, 64)
	rig := newEgressTestNat(t, hook, func(settings *LocalUserNatSettings) {
		settings.UdpBufferSettings.GlobalLimit = 4
		settings.UdpBufferSettings.afterSocketOpenForTest = func(sequence *UdpSequence, _ net.Conn) {
			opened <- sequence
		}
	})
	awaitOpen := func() *UdpSequence {
		t.Helper()
		select {
		case sequence := <-opened:
			return sequence
		case <-time.After(egressTestTimeout):
			t.Fatal("a flow never opened")
			return nil
		}
	}
	local := egressTestFlowPath(IpProtocolUdp, net.IPv4(127, 0, 0, 1), port)
	rig.send(ipOosUdpPacket(local, []byte("local")))
	localSequence := awaitOpen()
	buffer := make([]byte, 16)
	if n, _, err := sink.ReadFromUDP(buffer); err != nil || string(buffer[:n]) != "local" {
		t.Fatalf("the local flow did not forward: %q err=%v", buffer[:n], err)
	}
	for sourcePort := 43000; sourcePort < 43016; sourcePort += 1 {
		foreign := egressTestFlowPath(IpProtocolUdp, net.IPv4(127, 0, 0, 1), port)
		foreign.SourcePort = sourcePort
		owned := MessagePoolCopy(ipOosUdpPacket(foreign, []byte("foreign")))
		if !rig.nat.SendPacket(rig.source, protocol.ProvideMode_FriendsAndFamily, owned, -1) {
			MessagePoolReturn(owned)
			t.Fatal("the NAT refused a foreign packet")
		}
		awaitOpen()
		if udpSequenceDone(localSequence) {
			t.Fatalf("foreign flow %d evicted the local flow", sourcePort-43000)
		}
		if n, _, err := sink.ReadFromUDP(buffer); err != nil || string(buffer[:n]) != "foreign" {
			t.Fatalf("a foreign flow did not forward: %q err=%v", buffer[:n], err)
		}
	}
	rig.send(ipOosUdpPacket(local, []byte("still")))
	if n, _, err := sink.ReadFromUDP(buffer); err != nil || string(buffer[:n]) != "still" {
		t.Fatalf("the local flow stopped forwarding: %q err=%v", buffer[:n], err)
	}
}
