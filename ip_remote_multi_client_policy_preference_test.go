package connect

// Tests of the provider policy preference
// (ip_remote_multi_client_policy_preference.go): the generation order, how the
// preference arms and lapses, the demerit on the selection path, the optional
// generation field in both directions, and the whole path over in-memory
// transports.

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/urnetwork/connect/protocol"
)

// An injectable now for the preference's ttl.
type testPolicyClock struct {
	stateLock sync.Mutex
	now       time.Time
}

// The fake time.
func (self *testPolicyClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

// Moves the fake time on by d.
func (self *testPolicyClock) Advance(d time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(d)
}

// A bare channel of the given static tier that shares the preference, the way
// every window channel does through its args.
func policyPreferenceTestChannel(tier int, preference *providerPolicyPreference) *multiClientChannel {
	return &multiClientChannel{
		ctx: context.Background(),
		args: &multiClientChannelArgs{
			MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: NewId()},
			DestinationStats:               DestinationStats{Tier: tier},
			providerPolicyPreference:       preference,
		},
		settings: DefaultMultiClientSettings(),
	}
}

// Delivers one diagnostics message to the channel the way its provider's
// frame arrives.
func receiveProviderDiagnostics(t *testing.T, channel *multiClientChannel, message *protocol.IpProviderDiagnostics) {
	t.Helper()
	frame := RequireToFrameWithDefaultProtocolVersion(message)
	defer MessagePoolReturn(frame.MessageBytes)
	channel.clientReceive(TransferPath{}, []*protocol.Frame{frame}, Peer{})
}

// The channels of an offer, as a set.
func keptSet(clients []*multiClientChannel) map[*multiClientChannel]bool {
	kept := map[*multiClientChannel]bool{}
	for _, client := range clients {
		kept[client] = true
	}
	return kept
}

// The order is the generation alone: at least the client's own is current,
// older and unknown (0) are not, and while armed the demerit never ranks an
// older or unknown generation above a newer one.
func TestProviderPolicyGenerationOrdering(t *testing.T) {
	clock := &testPolicyClock{now: time.Unix(1_000_000, 0)}
	preference := newProviderPolicyPreference(5, time.Minute, clock.Now)

	for _, c := range []struct {
		generation uint64
		current    bool
	}{
		{generation: 0, current: false},
		{generation: 1, current: false},
		{generation: 4, current: false},
		{generation: 5, current: true},
		{generation: 6, current: true},
		{generation: 1 << 40, current: true},
	} {
		if got := preference.current(c.generation); got != c.current {
			t.Errorf("current(%d) = %t, want %t (own generation 5)", c.generation, got, c.current)
		}
		// disarmed, nobody moves
		if got := preference.demerit(c.generation); got != 0 {
			t.Errorf("demerit(%d) = %d before any block, want 0", c.generation, got)
		}
	}

	if !preference.observe(nil, &ProviderDiagnostics{SecurityPolicyGeneration: 4, BlockIngressPacketCount: 1}) {
		t.Fatal("a block from an older provider did not arm the preference")
	}
	// unknown (0) is the lowest generation, so the numeric order is the rank
	for a := uint64(0); a <= 8; a += 1 {
		for b := uint64(0); b < a; b += 1 {
			if preference.demerit(b) < preference.demerit(a) {
				t.Errorf("armed preference ranks generation %d (demerit %d) above newer %d (demerit %d)", b, preference.demerit(b), a, preference.demerit(a))
			}
		}
	}
	AssertEqual(t, preference.demerit(0), providerPolicyDemerit)
	AssertEqual(t, preference.demerit(4), providerPolicyDemerit)
	AssertEqual(t, preference.demerit(5), 0)
	AssertEqual(t, preference.demerit(6), 0)

	// a client with no generation of its own, or no ttl, has no preference:
	// every provider is current and nothing ever arms
	for _, disabled := range []*providerPolicyPreference{
		newProviderPolicyPreference(0, time.Minute, clock.Now),
		newProviderPolicyPreference(5, 0, clock.Now),
	} {
		if disabled != nil {
			t.Fatal("a disabled preference was constructed")
		}
		if !disabled.current(0) || disabled.observe(nil, &ProviderDiagnostics{BlockIngressPacketCount: 1}) || disabled.demerit(0) != 0 {
			t.Error("a nil preference moved a provider")
		}
	}
}

// The preference arms only on a rise of the provider's count of this client's
// outbound packets it dropped, only for a provider that is not current, and
// lapses after its ttl unless a further block re-arms it.
func TestProviderPolicyPreferenceArming(t *testing.T) {
	clock := &testPolicyClock{now: time.Unix(1_000_000, 0)}
	preference := newProviderPolicyPreference(SecurityPolicyRulesGeneration+1, time.Minute, clock.Now)
	older := SecurityPolicyRulesGeneration

	identity := &ProviderDiagnostics{SecurityPolicyGeneration: older, Sequence: 1}
	if preference.observe(nil, identity) || preference.active() {
		t.Fatal("an identity snapshot with no blocks armed the preference")
	}

	// return packets dropped on the way back are not the client's flow being
	// refused by older rules
	returnBlock := &ProviderDiagnostics{SecurityPolicyGeneration: older, BlockEgressPacketCount: 4, Sequence: 2}
	if preference.observe(identity, returnBlock) || preference.active() {
		t.Fatal("a return-path drop armed the preference")
	}

	// a current provider's drop is a verdict the client's own rules share
	currentBlock := &ProviderDiagnostics{SecurityPolicyGeneration: SecurityPolicyRulesGeneration + 1, BlockIngressPacketCount: 3, Sequence: 2}
	newerBlock := &ProviderDiagnostics{SecurityPolicyGeneration: SecurityPolicyRulesGeneration + 2, BlockIngressPacketCount: 3, Sequence: 2}
	if preference.observe(nil, currentBlock) || preference.observe(nil, newerBlock) || preference.active() {
		t.Fatal("a drop by a current or newer provider armed the preference")
	}

	outboundBlock := &ProviderDiagnostics{SecurityPolicyGeneration: older, BlockIngressPacketCount: 3, BlockEgressPacketCount: 4, Sequence: 3}
	if !preference.observe(returnBlock, outboundBlock) {
		t.Fatal("an older provider dropping the client's outbound packets did not arm the preference")
	}
	if !preference.active() {
		t.Fatal("the armed preference is not active")
	}
	// the same count again (a republish) is not a new block and re-arms nothing
	repeat := *outboundBlock
	repeat.Sequence = 4
	clock.Advance(30 * time.Second)
	if preference.observe(outboundBlock, &repeat) {
		t.Fatal("an unchanged count re-armed the preference")
	}
	clock.Advance(31 * time.Second)
	if preference.active() {
		t.Fatal("the preference outlived its ttl without a further block")
	}

	// an unknown generation (a provider from before the field) arms it too,
	// and a further block while armed extends it without reporting a new arm
	unknownBlock := &ProviderDiagnostics{BlockIngressPacketCount: 1, Sequence: 1}
	if !preference.observe(nil, unknownBlock) {
		t.Fatal("a block from a provider of unknown generation did not arm the lapsed preference")
	}
	clock.Advance(50 * time.Second)
	unknownBlockAgain := &ProviderDiagnostics{BlockIngressPacketCount: 2, Sequence: 2}
	if preference.observe(unknownBlock, unknownBlockAgain) {
		t.Fatal("re-arming an active preference reported a new arm")
	}
	clock.Advance(50 * time.Second)
	if !preference.active() {
		t.Fatal("a further block did not extend the preference")
	}
}

// End to end on the selection path: diagnostics frames arrive on each
// channel, a block from an older provider arms the shared preference, and the
// min-tier offer the race places new flows over (the app's retry) holds only
// current providers. A newer provider of the next tier outranks the older
// ones; nothing moves before the block or after the ttl.
func TestProviderPolicyPreferenceSteersRetry(t *testing.T) {
	clock := &testPolicyClock{now: time.Unix(1_000_000, 0)}
	own := SecurityPolicyRulesGeneration + 1
	preference := newProviderPolicyPreference(own, time.Minute, clock.Now)

	older := policyPreferenceTestChannel(0, preference)
	current := policyPreferenceTestChannel(0, preference)
	newer := policyPreferenceTestChannel(1, preference)
	// never reported (an idle spare, or a provider from before diagnostics)
	unreported := policyPreferenceTestChannel(0, preference)

	receiveProviderDiagnostics(t, older, &protocol.IpProviderDiagnostics{
		BuildVersion:             "older",
		SecurityPolicyGeneration: proto.Uint64(own - 1),
		Sequence:                 1,
	})
	receiveProviderDiagnostics(t, current, &protocol.IpProviderDiagnostics{
		BuildVersion:             "current",
		SecurityPolicyGeneration: proto.Uint64(own),
		Sequence:                 1,
	})
	receiveProviderDiagnostics(t, newer, &protocol.IpProviderDiagnostics{
		BuildVersion:             "newer",
		SecurityPolicyGeneration: proto.Uint64(own + 1),
		Sequence:                 1,
	})
	AssertEqual(t, older.providerPolicyGeneration(), own-1)
	AssertEqual(t, unreported.providerPolicyGeneration(), uint64(0))

	all := []*multiClientChannel{older, current, newer, unreported}
	if kept := keptSet(minTierClients(all)); len(kept) != 3 || !kept[older] || !kept[current] || !kept[unreported] {
		t.Fatalf("before any block the offer is %d channels, want the three tier-0 providers unchanged", len(kept))
	}

	// the older provider drops this client's outbound packets
	receiveProviderDiagnostics(t, older, &protocol.IpProviderDiagnostics{
		BuildVersion:             "older",
		SecurityPolicyGeneration: proto.Uint64(own - 1),
		BlockIngressPacketCount:  3,
		BlockIngressByteCount:    1500,
		Sequence:                 2,
	})
	if !preference.active() {
		t.Fatal("the older provider's block did not arm the preference")
	}

	AssertEqual(t, older.effectiveTier(), 0+providerPolicyDemerit)
	AssertEqual(t, unreported.effectiveTier(), 0+providerPolicyDemerit)
	AssertEqual(t, current.effectiveTier(), 0)
	AssertEqual(t, newer.effectiveTier(), 1)
	if kept := minTierClients(all); len(kept) != 1 || kept[0] != current {
		t.Fatalf("armed offer = %d channels, want only the current provider", len(kept))
	}
	// without a current provider of its tier, the newer provider of the next
	// tier takes the retry, never an older or unknown one
	if kept := minTierClients([]*multiClientChannel{older, newer, unreported}); len(kept) != 1 || kept[0] != newer {
		t.Fatalf("armed offer without the current provider = %d channels, want only the newer provider", len(kept))
	}
	// with only older or unknown providers the preference changes nothing
	if kept := keptSet(minTierClients([]*multiClientChannel{older, unreported})); len(kept) != 2 {
		t.Fatalf("armed offer of older and unknown providers = %d channels, want both: the preference must not empty the field", len(kept))
	}

	// a reordered (older sequence) snapshot neither replaces the block nor re-arms
	receiveProviderDiagnostics(t, older, &protocol.IpProviderDiagnostics{
		SecurityPolicyGeneration: proto.Uint64(own + 7),
		Sequence:                 1,
	})
	AssertEqual(t, older.providerPolicyGeneration(), own-1)

	// EffectiveTierSelection off is the static A/B point, demerit included
	static := policyPreferenceTestChannel(0, preference)
	static.settings.EffectiveTierSelection = false
	AssertEqual(t, static.effectiveTier(), 0)

	clock.Advance(time.Minute)
	if kept := keptSet(minTierClients(all)); len(kept) != 3 || !kept[older] {
		t.Fatalf("after the ttl the offer is %d channels, want the three tier-0 providers again", len(kept))
	}
}

// IpProviderDiagnostics as a peer built before security_policy_generation
// knows it.
func oldProviderDiagnosticsDescriptor(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	fileProto := protodesc.ToFileDescriptorProto(protocol.File_ip_proto)
	found := false
	for _, messageProto := range fileProto.MessageType {
		if messageProto.GetName() != "IpProviderDiagnostics" {
			continue
		}
		found = true
		var fields []*descriptorpb.FieldDescriptorProto
		for _, field := range messageProto.Field {
			if field.GetNumber() != 8 {
				fields = append(fields, field)
			}
		}
		messageProto.Field = fields
		// the synthetic oneof of the proto3 optional field
		messageProto.OneofDecl = nil
	}
	if !found {
		t.Fatal("IpProviderDiagnostics is missing from ip.proto")
	}
	file, err := protodesc.NewFile(fileProto, nil)
	if err != nil {
		t.Fatal(err)
	}
	return file.Messages().ByName("IpProviderDiagnostics")
}

// The field is optional both ways: a peer that predates it reads every other
// field of a new message unchanged and carries field 8 as unknown, and a
// message from such a peer decodes with an unknown generation, which is never
// current, and still arms the preference when it blocks.
func TestProviderDiagnosticsGenerationBackwardCompatible(t *testing.T) {
	oldDescriptor := oldProviderDiagnosticsDescriptor(t)
	if oldDescriptor.Fields().ByNumber(8) != nil || oldDescriptor.Fields().Len() != 7 {
		t.Fatalf("old descriptor has %d fields, want the 7 before the generation", oldDescriptor.Fields().Len())
	}
	oldType := dynamicpb.NewMessageType(oldDescriptor)

	// new provider -> old client
	newMessage := &protocol.IpProviderDiagnostics{
		BuildVersion:             "provider-9",
		SecurityPolicyHash:       "hash-9",
		BlockIngressPacketCount:  3,
		BlockIngressByteCount:    300,
		BlockEgressPacketCount:   2,
		BlockEgressByteCount:     200,
		Sequence:                 9,
		SecurityPolicyGeneration: proto.Uint64(7),
	}
	newBytes, err := proto.Marshal(newMessage)
	if err != nil {
		t.Fatal(err)
	}
	oldMessage := oldType.New()
	if err := proto.Unmarshal(newBytes, oldMessage.Interface()); err != nil {
		t.Fatalf("a peer without the field cannot decode the new message: %v", err)
	}
	fields := oldDescriptor.Fields()
	AssertEqual(t, oldMessage.Get(fields.ByName("build_version")).String(), "provider-9")
	AssertEqual(t, oldMessage.Get(fields.ByName("security_policy_hash")).String(), "hash-9")
	AssertEqual(t, oldMessage.Get(fields.ByName("block_ingress_packet_count")).Uint(), uint64(3))
	AssertEqual(t, oldMessage.Get(fields.ByName("block_ingress_byte_count")).Uint(), uint64(300))
	AssertEqual(t, oldMessage.Get(fields.ByName("block_egress_packet_count")).Uint(), uint64(2))
	AssertEqual(t, oldMessage.Get(fields.ByName("block_egress_byte_count")).Uint(), uint64(200))
	AssertEqual(t, oldMessage.Get(fields.ByName("sequence")).Uint(), uint64(9))
	if len(oldMessage.GetUnknown()) == 0 {
		t.Fatal("the old peer did not carry the generation as an unknown field")
	}

	// old provider -> new client
	oldProvider := oldType.New()
	oldProvider.Set(fields.ByName("build_version"), protoreflect.ValueOfString("provider-old"))
	oldProvider.Set(fields.ByName("security_policy_hash"), protoreflect.ValueOfString("hash-old"))
	oldProvider.Set(fields.ByName("sequence"), protoreflect.ValueOfUint64(1))
	oldBytes, err := proto.Marshal(oldProvider.Interface())
	if err != nil {
		t.Fatal(err)
	}
	decoded := &protocol.IpProviderDiagnostics{}
	if err := proto.Unmarshal(oldBytes, decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SecurityPolicyGeneration != nil || decoded.GetSecurityPolicyGeneration() != 0 {
		t.Fatalf("an old provider's message decoded with generation %v, want absent", decoded.SecurityPolicyGeneration)
	}
	diagnostics := providerDiagnosticsFromProtocol(decoded)
	if diagnostics.SecurityPolicyGeneration != 0 || diagnostics.BuildVersion != "provider-old" || diagnostics.Sequence != 1 {
		t.Fatalf("old provider diagnostics = %+v", diagnostics)
	}

	// the channel takes the old provider's frames as-is, as unknown generation
	clock := &testPolicyClock{now: time.Unix(1_000_000, 0)}
	preference := newProviderPolicyPreference(SecurityPolicyRulesGeneration, time.Minute, clock.Now)
	channel := policyPreferenceTestChannel(0, preference)
	channel.clientReceive(TransferPath{}, []*protocol.Frame{{
		MessageType:  protocol.MessageType_IpIpProviderDiagnostics,
		MessageBytes: oldBytes,
	}}, Peer{})
	snapshot := channel.providerDiagnosticsSnapshot()
	if snapshot == nil || snapshot.BuildVersion != "provider-old" || snapshot.SecurityPolicyGeneration != 0 {
		t.Fatalf("channel snapshot of an old provider = %+v", snapshot)
	}
	if preference.current(snapshot.SecurityPolicyGeneration) {
		t.Fatal("an old provider counts as current")
	}
	oldProvider.Set(fields.ByName("block_ingress_packet_count"), protoreflect.ValueOfUint64(3))
	oldProvider.Set(fields.ByName("sequence"), protoreflect.ValueOfUint64(2))
	oldBlockBytes, err := proto.Marshal(oldProvider.Interface())
	if err != nil {
		t.Fatal(err)
	}
	channel.clientReceive(TransferPath{}, []*protocol.Frame{{
		MessageType:  protocol.MessageType_IpIpProviderDiagnostics,
		MessageBytes: oldBlockBytes,
	}}, Peer{})
	if !preference.active() {
		t.Fatal("a block reported by an old provider did not arm the preference")
	}
	AssertEqual(t, channel.effectiveTier(), providerPolicyDemerit)
}

// Stands in for a provider whose rules predate the client's: a custom
// (generation unknown) policy that drops the client's outbound UDP to one port
// in every relationship, which the client's own policy admits.
type olderProviderTestPolicy struct {
	stats *SecurityPolicyStatsCollector
	port  int
}

// The collector the provider passed in.
func (self *olderProviderTestPolicy) Stats() *SecurityPolicyStatsCollector {
	return self.stats
}

// Allows every return packet.
func (self *olderProviderTestPolicy) InspectEgress(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error) {
	return SecurityPolicyResultAllow, nil
}

// Drops the client's outbound UDP to the port: the provider's ingress is the
// client's outbound traffic.
func (self *olderProviderTestPolicy) InspectIngress(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error) {
	if ipPath.Protocol == IpProtocolUdp && ipPath.DestinationPort == self.port {
		return SecurityPolicyResultDrop, nil
	}
	return SecurityPolicyResultAllow, nil
}

// Keeps no flow state.
func (self *olderProviderTestPolicy) RefreshEgress(ipPath *IpPath) {}

// Keeps no flow state.
func (self *olderProviderTestPolicy) RefreshIngress(ipPath *IpPath) {}

// The UDP port the older provider's policy drops.
const providerPolicyPreferenceTestBlockedPort = 40000

// The whole path over in-memory transports: a real provider with the given
// policy reports its generation and its drops of this client's packets in
// diagnostics, and the window hands its channels the multi-client's
// preference. Sends to the destination port until a window channel holds the
// provider's diagnostics, with its drops when the port is the blocked one,
// and returns the multi-client and that channel.
func runProviderPolicyPreferenceTunnel(t *testing.T, providerPolicy func(context.Context, *SecurityPolicyStatsCollector) SecurityPolicy, destinationPort int) (*RemoteUserNatMultiClient, *multiClientChannel) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	providerClient := NewClient(ctx, NewId(), NewNoContractClientOob(), DefaultClientSettingsWithBufferSize(32))
	t.Cleanup(providerClient.Cancel)
	providerLocalUserNat := NewLocalUserNatWithDefaults(ctx, "test-exit")
	providerSettings := DefaultRemoteUserNatProviderSettings()
	providerSettings.SecurityPolicyGenerator = providerPolicy
	provider := NewRemoteUserNatProvider(providerClient, providerLocalUserNat, providerSettings)
	t.Cleanup(provider.Close)

	settings := DefaultMultiClientSettings()
	settings.TcpCollapsePrevention = false
	multi := NewRemoteUserNatMultiClient(
		ctx,
		testMultiClientGenerator(providerClient),
		func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {},
		protocol.ProvideMode_Network,
		settings,
	)
	t.Cleanup(multi.Close)
	if multi.providerPolicyPreference == nil || multi.providerPolicyPreference.generation != SecurityPolicyRulesGeneration {
		t.Fatalf("the multi-client preference does not compare against its own built-in generation: %+v", multi.providerPolicyPreference)
	}

	packet := ipOosPacket(&IpPath{
		Version:         4,
		Protocol:        IpProtocolUdp,
		SourceIp:        net.ParseIP("192.0.2.1"),
		SourcePort:      41000,
		DestinationIp:   net.ParseIP("203.0.113.9"),
		DestinationPort: destinationPort,
	}, encryptedPayload(256))
	source := SourceId(NewId())
	for {
		multi.SendPacket(source, protocol.ProvideMode_Network, packet, time.Second)
		for _, window := range multi.windows {
			for _, client := range window.unorderedClients() {
				if diagnostics := client.providerDiagnosticsSnapshot(); diagnostics != nil &&
					(destinationPort != providerPolicyPreferenceTestBlockedPort || 0 < diagnostics.BlockIngressPacketCount) {
					return multi, client
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("no provider diagnostics reached a window channel")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// The built-in provider reports its own generation and never arms the
// preference.
func TestProviderPolicyPreferenceThroughTunnelCurrentProvider(t *testing.T) {
	multi, client := runProviderPolicyPreferenceTunnel(t, DefaultProviderSecurityPolicyWithStats, providerPolicyPreferenceTestBlockedPort+1)
	if client.policyPreference() != multi.providerPolicyPreference {
		t.Fatal("the window did not hand its channel the multi-client's preference")
	}
	AssertEqual(t, client.providerPolicyGeneration(), SecurityPolicyRulesGeneration)
	if multi.providerPolicyPreference.active() {
		t.Fatal("a current provider armed the preference")
	}
}

// A drop by a provider of unknown generation arms the preference, and the exit
// readout shows the demerit.
func TestProviderPolicyPreferenceThroughTunnelUnknownGenerationDrop(t *testing.T) {
	multi, client := runProviderPolicyPreferenceTunnel(t, func(ctx context.Context, stats *SecurityPolicyStatsCollector) SecurityPolicy {
		return &olderProviderTestPolicy{stats: stats, port: providerPolicyPreferenceTestBlockedPort}
	}, providerPolicyPreferenceTestBlockedPort)
	if client.policyPreference() != multi.providerPolicyPreference {
		t.Fatal("the window did not hand its channel the multi-client's preference")
	}
	AssertEqual(t, client.providerPolicyGeneration(), uint64(0))
	if !multi.providerPolicyPreference.active() {
		t.Fatal("the provider's drop of the client's flow did not arm the preference")
	}
	AssertEqual(t, client.policyPreference().demerit(client.providerPolicyGeneration()), providerPolicyDemerit)
	found := false
	for _, exit := range multi.Exits() {
		if exit.ClientId != client.ClientId() {
			continue
		}
		found = true
		if !exit.ProviderDiagnosticsAvailable || exit.ProviderSecurityPolicyGeneration != 0 || exit.ProviderBlockIngressPackets == 0 {
			t.Fatalf("exit readout diagnostics = %+v, want the unknown generation and its drops", exit)
		}
		if exit.EffectiveTier < exit.Tier+providerPolicyDemerit {
			t.Fatalf("exit readout tier %d -> %d, want the policy demerit applied", exit.Tier, exit.EffectiveTier)
		}
	}
	if !found {
		t.Fatal("the blocking provider is missing from the exit readout")
	}
}
