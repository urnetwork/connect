package connect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

func TestSecurityPolicyHashIdentifiesEffectiveRules(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first := SecurityPolicyHash(DefaultProviderSecurityPolicy(ctx))
	second := SecurityPolicyHash(DefaultProviderSecurityPolicy(ctx))
	disabled := SecurityPolicyHash(DisableSecurityPolicy())
	if first != second {
		t.Fatalf("identical built-in policies hashed differently: %q != %q", first, second)
	}
	if first == disabled {
		t.Fatal("enabled and disabled policies have the same identity")
	}
	decoded, err := hex.DecodeString(first)
	if err != nil || len(decoded) != 32 {
		t.Fatalf("policy identity %q is not one SHA-256 digest: bytes=%d err=%v", first, len(decoded), err)
	}
}

func TestProviderDiagnosticsAreSourceScopedAndGenerationGated(t *testing.T) {
	sourceA := NewId()
	sourceB := NewId()
	provider := &RemoteUserNatProvider{
		buildVersion:       "android-provider-321",
		securityPolicyHash: "policy-abc",
		sourceDiagnostics:  map[Id]*providerSourceDiagnostics{},
	}

	provider.recordProviderBlock(sourceA, true, 2, 1200)
	provider.recordProviderBlock(sourceA, false, 1, 800)

	a := provider.providerDiagnosticsMessage(sourceA)
	b := provider.providerDiagnosticsMessage(sourceB)
	if a.BlockIngressPacketCount != 2 || a.BlockIngressByteCount != 1200 ||
		a.BlockEgressPacketCount != 1 || a.BlockEgressByteCount != 800 {
		t.Fatalf("source A counters = %+v", a)
	}
	if b.BlockIngressPacketCount != 0 || b.BlockEgressPacketCount != 0 {
		t.Fatalf("source B observed source A blocks: %+v", b)
	}
	if a.BuildVersion != "android-provider-321" || a.SecurityPolicyHash != "policy-abc" {
		t.Fatalf("provider identity missing: %+v", a)
	}

	provider.markProviderDiagnosticsPublished(sourceA, a.Sequence)
	if duplicate := provider.providerDiagnosticsMessage(sourceA); duplicate != nil {
		t.Fatalf("unchanged generation republished: %+v", duplicate)
	}
	provider.recordProviderBlock(sourceA, true, 1, 64)
	if changed := provider.providerDiagnosticsMessage(sourceA); changed == nil || changed.Sequence <= a.Sequence {
		t.Fatalf("counter change did not advance publication: old=%d new=%+v", a.Sequence, changed)
	}
}

func TestProviderDiagnosticsFrameAndChannelOrdering(t *testing.T) {
	message := &protocol.IpProviderDiagnostics{
		BuildVersion:            "provider-44",
		SecurityPolicyHash:      "hash-44",
		BlockIngressPacketCount: 7,
		BlockIngressByteCount:   700,
		BlockEgressPacketCount:  3,
		BlockEgressByteCount:    300,
		Sequence:                44,
	}
	frame := RequireToFrameWithDefaultProtocolVersion(message)
	defer MessagePoolReturn(frame.MessageBytes)
	if frame.MessageType != protocol.MessageType_IpIpProviderDiagnostics {
		t.Fatalf("message type = %v", frame.MessageType)
	}
	roundTrip, err := FromFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if got := roundTrip.(*protocol.IpProviderDiagnostics); got.Sequence != 44 || got.SecurityPolicyHash != "hash-44" {
		t.Fatalf("round trip = %+v", got)
	}

	channel := &multiClientChannel{ctx: context.Background()}
	channel.clientReceive(TransferPath{}, []*protocol.Frame{frame}, Peer{})
	snapshot := channel.providerDiagnosticsSnapshot()
	if snapshot == nil || snapshot.Sequence != 44 || snapshot.BlockIngressPacketCount != 7 {
		t.Fatalf("channel snapshot = %+v", snapshot)
	}

	older := RequireToFrameWithDefaultProtocolVersion(&protocol.IpProviderDiagnostics{
		BuildVersion:       "stale",
		SecurityPolicyHash: "stale",
		Sequence:           43,
	})
	defer MessagePoolReturn(older.MessageBytes)
	channel.clientReceive(TransferPath{}, []*protocol.Frame{older}, Peer{})
	if got := channel.providerDiagnosticsSnapshot(); got.Sequence != 44 || got.BuildVersion != "provider-44" {
		t.Fatalf("older reordered diagnostics replaced current snapshot: %+v", got)
	}

	equal := RequireToFrameWithDefaultProtocolVersion(&protocol.IpProviderDiagnostics{
		BuildVersion:       "same-generation-spoof",
		SecurityPolicyHash: "same-generation-spoof",
		Sequence:           44,
	})
	defer MessagePoolReturn(equal.MessageBytes)
	channel.clientReceive(TransferPath{}, []*protocol.Frame{equal}, Peer{})
	if got := channel.providerDiagnosticsSnapshot(); got.BuildVersion != "provider-44" {
		t.Fatalf("equal-generation diagnostics replaced immutable snapshot: %+v", got)
	}
}

// The generation names the built-in rules, not settings or memory: the
// memory-bounded provider policy differs from the default in its hash
// (MaxFlows) and not in its generation. A disabled or custom policy is
// unknown.
func TestSecurityPolicyGenerationIdentifiesBuiltinRules(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	provider := DefaultProviderSecurityPolicy(ctx)
	bounded := newNatProviderSecurityPolicy(ctx, DefaultRemoteUserNatProviderSettings(), true)
	AssertEqual(t, SecurityPolicyGeneration(provider), SecurityPolicyRulesGeneration)
	AssertEqual(t, SecurityPolicyGeneration(DefaultSecurityPolicy(ctx)), SecurityPolicyRulesGeneration)
	AssertEqual(t, SecurityPolicyGeneration(bounded), SecurityPolicyRulesGeneration)
	if SecurityPolicyHash(provider) == SecurityPolicyHash(bounded) {
		t.Fatal("the memory-bounded policy hashed like the default; the hash no longer shows why it cannot order providers")
	}
	AssertEqual(t, SecurityPolicyGeneration(DisableSecurityPolicy()), uint64(0))
	AssertEqual(t, SecurityPolicyGeneration(&olderProviderTestPolicy{stats: DefaultSecurityPolicyStatsCollector()}), uint64(0))
	if SecurityPolicyRulesGeneration == 0 {
		t.Fatal("generation 0 is reserved for unknown")
	}
}

// A real provider computes its generation at construction and sends it with
// its identity; a provider whose generation is unknown leaves the field absent,
// as a provider from before the field does.
func TestProviderDiagnosticsReportSecurityPolicyGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	newProvider := func(settings *RemoteUserNatProviderSettings) *RemoteUserNatProvider {
		clientSettings := DefaultClientSettings()
		clientSettings.SendBufferSettings.SequenceBufferSize = 0
		clientSettings.SendBufferSettings.AckBufferSize = 0
		clientSettings.ReceiveBufferSettings.SequenceBufferSize = 0
		clientSettings.ForwardBufferSettings.SequenceBufferSize = 0
		providerClient := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
		t.Cleanup(providerClient.Cancel)
		provider := NewRemoteUserNatProvider(providerClient, NewLocalUserNatWithDefaults(ctx, "test-exit"), settings)
		t.Cleanup(provider.Close)
		return provider
	}

	builtin := newProvider(DefaultRemoteUserNatProviderSettings())
	identity := builtin.providerDiagnosticsMessage(NewId())
	if identity == nil || identity.SecurityPolicyGeneration == nil {
		t.Fatalf("the provider identity has no generation: %+v", identity)
	}
	AssertEqual(t, identity.GetSecurityPolicyGeneration(), SecurityPolicyRulesGeneration)
	frame := RequireToFrameWithDefaultProtocolVersion(identity)
	defer MessagePoolReturn(frame.MessageBytes)
	roundTrip, err := FromFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	AssertEqual(t, providerDiagnosticsFromProtocol(roundTrip.(*protocol.IpProviderDiagnostics)).SecurityPolicyGeneration, SecurityPolicyRulesGeneration)

	customSettings := DefaultRemoteUserNatProviderSettings()
	customSettings.SecurityPolicyGenerator = func(ctx context.Context, stats *SecurityPolicyStatsCollector) SecurityPolicy {
		return &olderProviderTestPolicy{stats: stats}
	}
	custom := newProvider(customSettings)
	if identity := custom.providerDiagnosticsMessage(NewId()); identity == nil || identity.SecurityPolicyGeneration != nil {
		t.Fatalf("a provider of unknown generation reported one: %+v", identity)
	}
}

// What the built-in rules are made of apart from code: the default settings,
// without the memory-scaled MaxFlows, and the hand-maintained exception
// tables. The feed-generated tables are left out: every release build
// regenerates the CFAA blocklists and the Meta prefixes, and
// SecurityPolicyHash identifies them.
func securityPolicyRulesPreimage(t *testing.T) []byte {
	t.Helper()
	dmca := DefaultDmcaSecurityPolicySettings()
	dmca.MaxFlows = 0
	settings, err := json.Marshal(struct {
		Cfaa *CfaaSecurityPolicySettings `json:"cfaa"`
		Dmca *DmcaSecurityPolicySettings `json:"dmca"`
		Web  *WebStandardSettings        `json:"web"`
	}{
		Cfaa: DefaultCfaaSecurityPolicySettings(),
		Dmca: dmca,
		Web:  DefaultWebStandardSettings(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var preimage bytes.Buffer
	preimage.Write(settings)
	fmt.Fprintf(
		&preimage,
		"\x00steam %v\x00telegram %v %d",
		steamValveNetworkPrefixes,
		telegramCallReflectorIpv4Ranges,
		telegramCallV12TcpFallbackIpv4,
	)
	return preimage.Bytes()
}

// The SHA-256 of the rules preimage, as the pins hold it.
func securityPolicyRulesDigest(t *testing.T) string {
	t.Helper()
	digest := sha256.Sum256(securityPolicyRulesPreimage(t))
	return hex.EncodeToString(digest[:])
}

// The rules digest each generation names. Adding a detector or changing a
// default raises SecurityPolicyRulesGeneration and adds the new generation's
// digest here, the one the failing test prints.
// Earlier pins stay, as the record of what each generation enforced. Both
// pins are of this digest, which leaves out the Meta prefixes since they
// became a feed table; neither generation had shipped then, and the first
// refresh equaled the snapshot generation 2 had. Generation 1's pin is of its
// own default settings (connect 4138f101).
var securityPolicyRulesPins = map[uint64]string{
	1: "265961a0f2b445d75fd5a851ec49198f580e3778f4bbe27315af7f3290c7e8c9",
	2: "2798a1cef586738de34759ff274f084e950cbd09b9c17b35a3baeba57d80141f",
}

// A change to the default settings or a hand-maintained exception table must
// come with a generation the providers can report, or clients cannot tell
// providers with the new rules from older ones. This cannot see a rule change
// made in code alone; SecurityPolicyRulesGeneration says to raise it then too.
func TestSecurityPolicyRulesGenerationPin(t *testing.T) {
	digest := securityPolicyRulesDigest(t)
	if pin := securityPolicyRulesPins[SecurityPolicyRulesGeneration]; digest != pin {
		t.Fatalf(`the built-in security policy rules changed: digest %s, pinned %q for generation %d.
Raise SecurityPolicyRulesGeneration to %d (ip_provider_diagnostics.go) and add %d: %q to
securityPolicyRulesPins, keeping the earlier pins.`,
			digest,
			pin,
			SecurityPolicyRulesGeneration,
			SecurityPolicyRulesGeneration+1,
			SecurityPolicyRulesGeneration+1,
			digest,
		)
	}
}

// The pin covers what only a reviewed change can alter, the default settings
// and the hand-maintained exception tables, and not the feed-generated
// tables, so a release build's refresh of the Meta prefixes needs no new
// generation (and does not fail this pin in the release's own tests).
func TestSecurityPolicyRulesPinCoversHandMaintainedTablesOnly(t *testing.T) {
	preimage := securityPolicyRulesPreimage(t)
	for _, prefix := range steamValveNetworkPrefixes {
		if !bytes.Contains(preimage, []byte(prefix.String())) {
			t.Fatalf("the rules pin does not cover the Steam prefix %s", prefix)
		}
	}
	if !bytes.Contains(preimage, []byte(fmt.Sprint(telegramCallReflectorIpv4Ranges))) {
		t.Fatal("the rules pin does not cover the Telegram reflector ranges")
	}
	for _, prefix := range metaNetworkPrefixes {
		if bytes.Contains(preimage, []byte(prefix.String())) {
			t.Fatalf("the rules pin covers the feed-generated Meta prefix %s", prefix)
		}
	}
}

// The feed-generated tables are identified by the hash instead, in both
// directions: a provider whose release refreshed them reports a different
// SecurityPolicyHash.
func TestSecurityPolicyHashIdentifiesFeedTables(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, policy := range []SecurityPolicy{DefaultSecurityPolicy(ctx), DefaultProviderSecurityPolicy(ctx)} {
		var identity bytes.Buffer
		writeSecurityPolicyIdentity(&identity, policy)
		if !bytes.Contains(identity.Bytes(), []byte(cfaaBlockedPrefixData)) ||
			!bytes.Contains(identity.Bytes(), []byte(cfaaBlockedPrefix6Data)) {
			t.Fatalf("the %T identity does not cover the CFAA tables", policy)
		}
		for _, prefix := range metaNetworkPrefixes {
			if !bytes.Contains(identity.Bytes(), []byte(prefix.String()+"\n")) {
				t.Fatalf("the %T identity does not cover the Meta prefix %s", policy, prefix)
			}
		}
	}
}
