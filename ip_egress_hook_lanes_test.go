// Every lane a provider's ingress takes carries the source's identity to the
// egress hook (CODESTYLE: every delivery path must carry the key): the ordinary
// callback, the reliable TCP lane, and the small-control and ingress fallbacks
// under memory pressure, for TCP, UDP and ICMP, v4 and v6, a fragmented first
// datagram included. The hook verifies each contract once, and a hook installed
// on a live NAT closes the flows it did not decide. No upstream is dialed but
// loopback listeners on ports the kernel picks.
package connect

import (
	"context"
	"crypto/ed25519"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The seals of one test contract, as a provider's receive callback sees them.
type egressLanePeer struct {
	subject    Id
	network    Id
	contractId Id
	key        [ed25519.PublicKeySize]byte
	delegation []byte
}

func newEgressLanePeer() *egressLanePeer {
	peer := &egressLanePeer{
		subject:    NewId(),
		network:    NewId(),
		contractId: NewId(),
		delegation: []byte("test window delegation"),
	}
	copy(peer.key[:], "egress lane test session key....")
	return peer
}

func (self *egressLanePeer) peer(contractClass protocol.ContractClass) Peer {
	return Peer{
		ProvideMode:       protocol.ProvideMode_Network,
		ContractClass:     contractClass,
		SourceSubject:     self.subject,
		SourceNetwork:     self.network,
		WindowDelegation:  self.delegation,
		verifiedClientKey: &self.key,
		contractId:        self.contractId,
	}
}

// Checks that a flow reached the hook with this contract's seals.
func (self *egressLanePeer) checkFlow(t *testing.T, flow EgressFlow, source TransferPath, ipProtocol IpProtocol) {
	t.Helper()
	if flow.Source != source || flow.ProvideMode != protocol.ProvideMode_Network ||
		flow.SourceSubject != self.subject || flow.SourceNetwork != self.network ||
		flow.ContractClass != ContractClassData || !flow.Sealed || flow.Protocol != ipProtocol {
		t.Fatalf("a %s flow reached the hook without its seals: %+v", ipProtocol, flow)
	}
}

// Checks that an egress source carries this contract's seals.
func (self *egressLanePeer) checkSource(t *testing.T, source *egressSource) {
	t.Helper()
	if source.sourceSubject != self.subject || source.sourceNetwork != self.network ||
		source.contractId != self.contractId || source.contractClass != ContractClassData ||
		source.verifiedClientKey == nil || *source.verifiedClientKey != self.key ||
		string(source.windowDelegation) != string(self.delegation) {
		t.Fatalf("a lane dropped the seals: %+v", source)
	}
}

// A real provider over a hooked NAT whose dials always fail, so no flow leaves
// the process.
type egressLaneProvider struct {
	nat      *LocalUserNat
	client   *Client
	provider *RemoteUserNatProvider
	cancel   context.CancelFunc
}

func newEgressLaneProvider(t *testing.T, hook EgressHook, budget *TransferMemoryBudget, configure func(*LocalUserNatSettings)) *egressLaneProvider {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	self := &egressLaneProvider{cancel: cancel}
	clientSettings := DefaultClientSettings()
	clientSettings.Log = NewNoopLogger()
	clientSettings.EncryptionSettings.Mode = EncryptionModeOff
	self.client = NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
	settings := DefaultLocalUserNatSettingsWithBufferSize(16)
	settings.Log = NewNoopLogger()
	settings.MemoryBudget = budget
	settings.EgressHook = hook
	refuse := &DialContextSettings{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Err: net.UnknownNetworkError("egress lane test")}
		},
	}
	settings.TcpBufferSettings.DialContextSettings = refuse
	settings.UdpBufferSettings.DialContextSettings = refuse
	if configure != nil {
		configure(settings)
	}
	var err error
	self.nat, err = TryNewLocalUserNat(ctx, "egress lane test", settings)
	if err != nil {
		t.Fatal(err)
	}
	providerSettings := DefaultRemoteUserNatProviderSettings()
	providerSettings.WriteTimeout = time.Millisecond
	self.provider, err = TryNewRemoteUserNatProvider(self.client, self.nat, providerSettings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		self.provider.Close()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), egressTestTimeout)
		defer closeCancel()
		if err := self.nat.CloseAndWait(closeCtx); err != nil {
			t.Error(err)
		}
		if err := self.client.CloseAndWait(closeCtx); err != nil {
			t.Error(err)
		}
	})
	return self
}

// Hands one packet to the provider as its receive callback would.
func (self *egressLaneProvider) receive(t *testing.T, source TransferPath, peer Peer, packet []byte) {
	t.Helper()
	frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	self.provider.ClientReceive(source, []*protocol.Frame{frame}, peer)
	if !frame.Raw {
		MessagePoolReturn(frame.MessageBytes)
	}
}

// No refusal of a hooked flow is ever a security incident.
func (self *egressLaneProvider) checkNoIncident(t *testing.T) {
	t.Helper()
	if incidents := self.provider.SecurityPolicyStats(false)[SecurityPolicyResultIncident]; 0 < len(incidents) {
		t.Fatalf("hooked flows were reported as incidents: %v", incidents)
	}
}

// One packet of each protocol, for a family.
func egressLanePackets(version int) map[IpProtocol][]byte {
	destination := net.IPv4(192, 0, 2, 90).To4()
	echo := testingIcmp4EchoPacket("192.0.2.2", "192.0.2.90", 0x3333, 1, []byte("ping"))
	if version == 6 {
		destination = net.ParseIP("2001:db8::90")
		echo = testingIcmp6EchoPacket("2001:db8::2", "2001:db8::90", 0x3333, 1, []byte("ping"))
	}
	path := func(ipProtocol IpProtocol) *IpPath {
		path := &IpPath{
			Version:         version,
			Protocol:        ipProtocol,
			SourceIp:        net.IPv4(192, 0, 2, 2).To4(),
			SourcePort:      42000,
			DestinationIp:   destination,
			DestinationPort: 8443,
		}
		if version == 6 {
			path.SourceIp = net.ParseIP("2001:db8::2")
		}
		return path
	}
	return map[IpProtocol][]byte{
		IpProtocolTcp:  ipOosTcpPacketSequence(path(IpProtocolTcp), tcpFlagSyn, 100, nil),
		IpProtocolUdp:  ipOosUdpPacket(path(IpProtocolUdp), []byte("datagram")),
		IpProtocolIcmp: echo,
	}
}

// A hook that denies everything, so the lane tests dial nothing.
func newEgressDenyHook() *egressTestHook {
	hook := newEgressTestHook()
	hook.setResolver(func(*EgressFlow) EgressDecision {
		return EgressDecision{Verdict: EgressVerdictDeny}
	})
	return hook
}

// The ordinary lane: a SYN, a datagram and an echo of each family reach the
// hook with the contract's seals, and the contract is verified once.
func TestEgressHookOrdinaryLaneCarriesSeals(t *testing.T) {
	assertMessagePoolOwnership(t)
	hook := newEgressDenyHook()
	rig := newEgressLaneProvider(t, hook, nil, nil)
	sealed := newEgressLanePeer()
	source := SourceId(NewId())
	for _, version := range []int{4, 6} {
		for ipProtocol, packet := range egressLanePackets(version) {
			rig.receive(t, source, sealed.peer(ContractClassData), packet)
			sealed.checkFlow(t, hook.takeFlow(t), source, ipProtocol)
		}
	}
	if count := hook.verifyCount(); count != 1 {
		t.Fatalf("six flows of one contract were verified %d times, want once", count)
	}
	verified := hook.verifyPeers[0]
	if key, ok := verified.VerifiedClientPublicKey(); !ok || key != sealed.key ||
		string(verified.WindowDelegation) != string(sealed.delegation) || verified.SourceSubject != sealed.subject {
		t.Fatalf("the hook verified %+v, not the contract's delegation and key", verified)
	}
	rig.checkNoIncident(t)
}

// A control-class contract reaches the hook marked control, and a peer
// without a session key reaches it unsealed.
func TestEgressHookCarriesClassAndSessionAsSent(t *testing.T) {
	assertMessagePoolOwnership(t)
	hook := newEgressDenyHook()
	rig := newEgressLaneProvider(t, hook, nil, nil)
	sealed := newEgressLanePeer()
	packets := egressLanePackets(4)
	rig.receive(t, SourceId(NewId()), sealed.peer(ContractClassControl), packets[IpProtocolUdp])
	if flow := hook.takeFlow(t); flow.ContractClass != ContractClassControl {
		t.Fatalf("a control contract reached the hook as %v", flow.ContractClass)
	}
	plaintext := sealed.peer(ContractClassData)
	plaintext.verifiedClientKey = nil
	rig.receive(t, SourceId(NewId()), plaintext, packets[IpProtocolTcp])
	if flow := hook.takeFlow(t); flow.Sealed {
		t.Fatal("a flow with no session key reached the hook sealed")
	}
}

// A first datagram that arrives in fragments reaches the hook with its seals.
func TestEgressHookFragmentedFirstDatagramCarriesSeals(t *testing.T) {
	assertMessagePoolOwnership(t)
	hook := newEgressDenyHook()
	rig := newEgressLaneProvider(t, hook, nil, nil)
	sealed := newEgressLanePeer()
	source := SourceId(NewId())
	path := &IpPath{
		Version: 4, Protocol: IpProtocolUdp,
		SourceIp: net.IPv4(192, 0, 2, 2).To4(), SourcePort: 42001,
		DestinationIp: net.IPv4(192, 0, 2, 91).To4(), DestinationPort: 8443,
	}
	datagram := MessagePoolCopy(ipOosUdpPacket(path, make([]byte, 3000)))
	fragments, err := fragmentIpv4Packet(datagram, 1280)
	if err != nil || len(fragments) < 2 {
		t.Fatalf("fragmenting gave %d fragments, err=%v", len(fragments), err)
	}
	for _, fragment := range fragments {
		rig.receive(t, source, sealed.peer(ContractClassData), fragment)
		MessagePoolReturn(fragment)
	}
	sealed.checkFlow(t, hook.takeFlow(t), source, IpProtocolUdp)
}

// The reliable TCP lane: a SYN delivered through a reliable receipt reaches
// the hook with its seals.
func TestEgressHookReliableLaneCarriesSeals(t *testing.T) {
	assertMessagePoolOwnership(t)
	hook := newEgressDenyHook()
	rig := newEgressLaneProvider(t, hook, nil, nil)
	sealed := newEgressLanePeer()
	source := SourceId(NewId())
	packet := MessagePoolCopy(egressLanePackets(4)[IpProtocolTcp])
	frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
	if err != nil {
		MessagePoolReturn(packet)
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sequence := NewReceiveSequence(ctx, rig.client, source, NewId(), sequenceTlsRoleServer, false, DefaultReceiveBufferSettings())
	sequence.deliverItems = []*receiveItem{{transferItem: transferItem{messageId: NewId(), sequenceNumber: 0},
		receiveCallback: rig.client.receiveCallback, ack: true, frames: []*protocol.Frame{frame}}}
	sequence.deliverFrames = []*protocol.Frame{frame}
	sequence.deliverPeer = sealed.peer(ContractClassData)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runReliableIngressFixtureDelivery(sequence)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(egressTestTimeout):
			t.Error("reliable delivery did not join")
		}
	})
	sealed.checkFlow(t, hook.takeFlow(t), source, IpProtocolTcp)
}

// The small-control and ingress fallbacks, taken when the provider's ordinary
// workspace is exhausted, hand the NAT each packet with the contract's seals.
// What a NAT does with a carried source past its admission is the ordinary
// lane's, which the tests above follow to the hook.
func TestEgressHookFallbackLanesCarrySeals(t *testing.T) {
	assertMessagePoolOwnership(t)
	budget := NewTransferMemoryBudget(mib(2))
	hook := newEgressDenyHook()
	admitted := make(chan egressSource, 8)
	rig := newEgressLaneProvider(t, hook, budget, nil)
	rig.nat.beforeSendPacketForTest = func(sendPacket *SendPacket) {
		admitted <- newEgressSource(sendPacket.egressPeer)
	}
	// leave the NAT its packet admission but not the provider's ordinary
	// workspace, so every frame takes a fallback
	fill := budget.Available() - 12_000
	if !budget.TryReserve(fill) {
		t.Fatal("could not fill the budget")
	}
	defer budget.Release(fill)
	sealed := newEgressLanePeer()
	source := SourceId(NewId())
	ackPath := &IpPath{
		Version: 4, Protocol: IpProtocolTcp,
		SourceIp: net.IPv4(192, 0, 2, 2).To4(), SourcePort: 42002,
		DestinationIp: net.IPv4(192, 0, 2, 92).To4(), DestinationPort: 8443,
	}
	packets := egressLanePackets(4)
	sends := [][]byte{
		// the small-control lane
		ipOosTcpPacketSequence(ackPath, tcpFlagAck, 100, nil),
		// the ingress lane
		packets[IpProtocolTcp],
		packets[IpProtocolUdp],
		packets[IpProtocolIcmp],
	}
	var forwarded atomic.Int64
	rig.provider.beforeLocalUserNatAdmissionForTest = func(Id, [][]byte) {
		forwarded.Add(1)
	}
	for _, packet := range sends {
		rig.receive(t, source, sealed.peer(ContractClassData), packet)
	}
	if forwarded.Load() != int64(len(sends)) {
		t.Fatalf("%d of %d fallback packets were offered to the NAT", forwarded.Load(), len(sends))
	}
	for range sends {
		select {
		case egressSource := <-admitted:
			sealed.checkSource(t, &egressSource)
		case <-time.After(egressTestTimeout):
			t.Fatal("a fallback packet never reached the NAT's dispatch")
		}
	}
}

// Many flows of one contract verify it once: a second flow that arrives while
// the first is verifying waits for that answer rather than asking again. A new
// contract is verified again.
func TestEgressHookVerifiesEachContractOnce(t *testing.T) {
	assertMessagePoolOwnership(t)
	hook := newEgressDenyHook()
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	hook.setVerifier(func(TransferPath, Peer) *BridgePrincipal {
		entered <- struct{}{}
		<-release
		return &BridgePrincipal{Subject: NewId()}
	})
	waiting := make(chan struct{}, 4)
	rig := newEgressLaneProvider(t, hook, nil, func(settings *LocalUserNatSettings) {
		settings.beforeEgressPrincipalWaitForTest = func() {
			waiting <- struct{}{}
		}
	})
	sealed := newEgressLanePeer()
	source := SourceId(NewId())
	path := func(sourcePort int) []byte {
		return ipOosUdpPacket(&IpPath{
			Version: 4, Protocol: IpProtocolUdp,
			SourceIp: net.IPv4(192, 0, 2, 2).To4(), SourcePort: sourcePort,
			DestinationIp: net.IPv4(192, 0, 2, 93).To4(), DestinationPort: 8443,
		}, []byte("datagram"))
	}
	awaitSignal := func(signal chan struct{}, what string) {
		t.Helper()
		select {
		case <-signal:
		case <-time.After(egressTestTimeout):
			t.Fatalf("%s never happened", what)
		}
	}
	rig.receive(t, source, sealed.peer(ContractClassData), path(43001))
	awaitSignal(entered, "the first flow's verify")
	rig.receive(t, source, sealed.peer(ContractClassData), path(43002))
	awaitSignal(waiting, "the second flow's wait")
	close(release)
	for range 2 {
		if flow := hook.takeFlow(t); flow.Principal == nil {
			t.Fatal("a flow resolved without its contract's principal")
		}
	}
	for sourcePort := 43003; sourcePort < 43008; sourcePort += 1 {
		rig.receive(t, source, sealed.peer(ContractClassData), path(sourcePort))
		hook.takeFlow(t)
	}
	if count := hook.verifyCount(); count != 1 {
		t.Fatalf("seven flows of one contract were verified %d times", count)
	}
	renewed := sealed.peer(ContractClassData)
	renewed.contractId = NewId()
	rig.receive(t, source, renewed, path(43008))
	awaitSignal(entered, "the new contract's verify")
	hook.takeFlow(t)
	if count := hook.verifyCount(); count != 2 {
		t.Fatalf("a new contract made %d verifies, want 2", count)
	}
}

// A refusal is asked again once the published generation moves, since the hook
// may not have known the subject yet; a principal is kept.
func TestEgressHookAsksAgainAfterARefusalUnderAnOlderPolicy(t *testing.T) {
	assertMessagePoolOwnership(t)
	hook := newEgressDenyHook()
	rig := newEgressLaneProvider(t, hook, nil, nil)
	sealed := newEgressLanePeer()
	source := SourceId(NewId())
	packets := map[int][]byte{}
	for sourcePort := 44001; sourcePort <= 44003; sourcePort += 1 {
		packets[sourcePort] = ipOosUdpPacket(&IpPath{
			Version: 4, Protocol: IpProtocolUdp,
			SourceIp: net.IPv4(192, 0, 2, 2).To4(), SourcePort: sourcePort,
			DestinationIp: net.IPv4(192, 0, 2, 94).To4(), DestinationPort: 8443,
		}, []byte("datagram"))
	}
	rig.receive(t, source, sealed.peer(ContractClassData), packets[44001])
	hook.takeFlow(t)
	rig.receive(t, source, sealed.peer(ContractClassData), packets[44002])
	hook.takeFlow(t)
	if count := hook.verifyCount(); count != 1 {
		t.Fatalf("a refusal under one policy was asked %d times", count)
	}
	hook.generation.Add(1)
	rig.receive(t, source, sealed.peer(ContractClassData), packets[44003])
	hook.takeFlow(t)
	if count := hook.verifyCount(); count != 2 {
		t.Fatalf("a refusal under an older policy was asked %d times, want 2", count)
	}
}

// A hook installed on a live NAT resets a flow opened without one and closes
// its upstream socket; the next flow is the hook's.
func TestSetEgressHookClosesFlowsItDidNotDecide(t *testing.T) {
	assertMessagePoolOwnership(t)
	listener, port := newEgressTestListener(t)
	rig := newEgressTestNat(t, nil, nil)
	path := egressTestFlowPath(IpProtocolTcp, net.IPv4(127, 0, 0, 1), port)
	rig.send(egressTestTcpPacket(path, tcpFlagSyn, 100, 0, nil))
	receiveEgressTestSynAck(t, rig, path)
	upstream, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()

	hook := newEgressTestHook()
	rig.nat.SetEgressHook(hook)
	reset := egressTestParse(t, rig.receive())
	if !reset.Rst || reset.SourcePort != port {
		t.Fatalf("the legacy flow got %+v, want a reset", reset)
	}
	upstream.SetReadDeadline(time.Now().Add(egressTestTimeout))
	if _, err := upstream.Read(make([]byte, 1)); err == nil {
		t.Fatal("the legacy flow's upstream socket stayed open")
	}

	next := egressTestFlowPath(IpProtocolTcp, net.IPv4(127, 0, 0, 1), port)
	next.SourcePort = 42001
	rig.send(egressTestTcpPacket(next, tcpFlagSyn, 200, 0, nil))
	if flow := hook.takeFlow(t); int(flow.From.Port()) != 42001 {
		t.Fatalf("the hook saw %+v, not the new flow", flow)
	}
}

// Without a hook, resolving a flow costs no allocation.
func TestEgressHookNilHookResolvesWithoutAllocation(t *testing.T) {
	egress := newLocalUserNatEgress(DefaultLocalUserNatSettings())
	state := &egressFlowState{nat: egress}
	path := egressTestFlowPath(IpProtocolTcp, net.IPv4(192, 0, 2, 95), 8443)
	source := SourceId(NewId())
	ctx := context.Background()
	var outcome egressOutcome
	if allocations := testing.AllocsPerRun(100, func() {
		outcome, _ = state.resolve(ctx, source, TransferKey{}, protocol.ProvideMode_Network, path)
	}); allocations != 0 || outcome != egressOutcomeLegacy {
		t.Fatalf("a hookless resolve allocated %g and gave %d", allocations, outcome)
	}
	// a NAT without a hook is handed no peer, and a flow reads the nil one
	if allocations := testing.AllocsPerRun(100, func() {
		_ = newEgressSource(nil)
	}); allocations != 0 {
		t.Fatalf("reading a flow's source allocated %g", allocations)
	}
}

// The principal answers stay within their bound and leave with their source.
func TestEgressHookPrincipalAnswersAreBounded(t *testing.T) {
	settings := DefaultLocalUserNatSettings()
	settings.EgressPrincipalMaxCount = 4
	egress := newLocalUserNatEgress(settings)
	hook := newEgressTestHook()
	var verifies atomic.Int64
	hook.setVerifier(func(TransferPath, Peer) *BridgePrincipal {
		verifies.Add(1)
		return nil
	})
	box := &egressHookBox{hook: hook}
	retired := SourceId(NewId())
	for i := range 16 {
		source := SourceId(NewId())
		if i == 0 {
			source = retired
		}
		egressSource := &egressSource{contractId: NewId()}
		egress.verifyPrincipal(context.Background(), box, source, TransferKey{}, protocol.ProvideMode_Network, egressSource)
	}
	if count := len(egress.principals); 4 < count {
		t.Fatalf("%d answers kept past a bound of 4", count)
	}
	egress.dropSource(retired.SourceId)
	for key := range egress.principals {
		if key.source.SourceId == retired.SourceId {
			t.Fatal("a retired source's answer was kept")
		}
	}
	if verifies.Load() != 16 {
		t.Fatalf("%d verifies for 16 contracts", verifies.Load())
	}
}
