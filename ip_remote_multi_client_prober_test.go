package connect

import (
	"context"
	"encoding/binary"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- dns answer parsing ---

// dnsTestAnswer builds a dns response for name with the given answer records,
// using the same question encoding the query builder uses -- so the parser is
// tested against the wire form the prober actually produces and receives.
type dnsTestRecord struct {
	// compressed: name as a pointer to the question (the common resolver form)
	recordType  uint16
	recordClass uint16
	rdata       []byte
}

func dnsTestAnswer(t *testing.T, id uint16, name string, flags uint16, records []dnsTestRecord) []byte {
	t.Helper()
	question, ok := dnsQuestion(name)
	if !ok {
		t.Fatalf("could not encode question for %q", name)
	}
	payload := make([]byte, 12, 12+len(question)+16*len(records))
	binary.BigEndian.PutUint16(payload[0:2], id)
	binary.BigEndian.PutUint16(payload[2:4], flags)
	binary.BigEndian.PutUint16(payload[4:6], 1)
	binary.BigEndian.PutUint16(payload[6:8], uint16(len(records)))
	payload = append(payload, question...)
	for _, record := range records {
		// name: compression pointer to offset 12 (the question name)
		payload = append(payload, 0xC0, 0x0C)
		payload = binary.BigEndian.AppendUint16(payload, record.recordType)
		payload = binary.BigEndian.AppendUint16(payload, record.recordClass)
		// ttl
		payload = append(payload, 0, 0, 0, 60)
		payload = binary.BigEndian.AppendUint16(payload, uint16(len(record.rdata)))
		payload = append(payload, record.rdata...)
	}
	return payload
}

func dnsTestARecord(ip net.IP) dnsTestRecord {
	return dnsTestRecord{recordType: 1, recordClass: 1, rdata: ip.To4()}
}

// The parser's decision table: it must read exactly the A records out of a
// well-formed response, ignore record types it does not want, and treat every
// malformed shape as no answer -- never as a partial one.
func TestDnsAResponseParser(t *testing.T) {
	const id = 0x5150
	name := "www.example.com"

	// a single A record
	answer := dnsTestAnswer(t, id, name, 0x8180, []dnsTestRecord{
		dnsTestARecord(net.IPv4(93, 184, 216, 34)),
	})
	ips, ok := parseDnsAResponse(answer, id)
	if !ok || len(ips) != 1 || !ips[0].Equal(net.IPv4(93, 184, 216, 34)) {
		t.Errorf("single A: ips=%v ok=%v", ips, ok)
	}

	// multiple A records mixed with CNAME and AAAA: the As come back in
	// order, everything else is skipped without error
	answer = dnsTestAnswer(t, id, name, 0x8180, []dnsTestRecord{
		{recordType: 5, recordClass: 1, rdata: []byte{3, 'w', 'w', 'w', 0xC0, 0x0C}},
		dnsTestARecord(net.IPv4(93, 184, 216, 34)),
		{recordType: 28, recordClass: 1, rdata: net.ParseIP("2606:2800:220:1::1").To16()},
		dnsTestARecord(net.IPv4(93, 184, 216, 35)),
	})
	ips, ok = parseDnsAResponse(answer, id)
	if !ok || len(ips) != 2 || !ips[0].Equal(net.IPv4(93, 184, 216, 34)) || !ips[1].Equal(net.IPv4(93, 184, 216, 35)) {
		t.Errorf("mixed records: ips=%v ok=%v, want the two As in order", ips, ok)
	}

	// a non-A-only response (AAAA): well-formed, zero records
	answer = dnsTestAnswer(t, id, name, 0x8180, []dnsTestRecord{
		{recordType: 28, recordClass: 1, rdata: net.ParseIP("2606:2800:220:1::1").To16()},
	})
	ips, ok = parseDnsAResponse(answer, id)
	if !ok || len(ips) != 0 {
		t.Errorf("aaaa only: ips=%v ok=%v, want ok with no records", ips, ok)
	}

	// NXDOMAIN: the resolver answered; the name yielded nothing
	answer = dnsTestAnswer(t, id, name, 0x8183, nil)
	ips, ok = parseDnsAResponse(answer, id)
	if !ok || len(ips) != 0 {
		t.Errorf("nxdomain: ips=%v ok=%v, want ok with no records", ips, ok)
	}

	// a query echoed back (QR unset) is not a response -- the shape a
	// middlebox or an echoing test harness produces
	query := dnsTestAnswer(t, id, name, 0x0100, nil)
	if _, ok = parseDnsAResponse(query, id); ok {
		t.Error("an echoed query parsed as a response")
	}

	// a foreign transaction id is not our answer
	answer = dnsTestAnswer(t, id+1, name, 0x8180, []dnsTestRecord{
		dnsTestARecord(net.IPv4(93, 184, 216, 34)),
	})
	if _, ok = parseDnsAResponse(answer, id); ok {
		t.Error("a foreign transaction id parsed as our answer")
	}

	// malformed shapes: truncated header, truncated question, truncated
	// rdata. Each must read as no answer, with no partial records.
	if _, ok = parseDnsAResponse([]byte{0, 1, 0x81}, id); ok {
		t.Error("a truncated header parsed")
	}
	whole := dnsTestAnswer(t, id, name, 0x8180, []dnsTestRecord{
		dnsTestARecord(net.IPv4(93, 184, 216, 34)),
	})
	for _, cut := range []int{13, len(whole) - 10, len(whole) - 2} {
		if ips, ok := parseDnsAResponse(whole[:cut], id); ok && 0 < len(ips) {
			t.Errorf("truncation at %d yielded records %v", cut, ips)
		}
	}
}

// --- resolution through the probed channel ---

// probeFlowsOfProtocol snapshots the registered probe flows matching an ip
// protocol, so the two stages of a pass can be observed separately.
func probeFlowsOfProtocol(parent *RemoteUserNatMultiClient, protocol IpProtocol) []*probeFlow {
	parent.stateLock.Lock()
	defer parent.stateLock.Unlock()
	probes := []*probeFlow{}
	for _, update := range parent.ip4PathUpdates {
		if update.isProbe() && update.probe.ipPath.Protocol == protocol {
			probes = append(probes, update.probe)
		}
	}
	return probes
}

func waitForProbeFlows(t *testing.T, parent *RemoteUserNatMultiClient, protocol IpProtocol, n int) []*probeFlow {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if probes := probeFlowsOfProtocol(parent, protocol); n <= len(probes) {
			return probes
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("did not observe %d probe flows of protocol %v", n, protocol)
	return nil
}

// The full F-2 pass against a cooperating provider: the resolution queries go
// to the sampled resolver THROUGH the channel, their answers resolve the
// sampled hostnames, tcp syns follow to exactly the answered addresses, and
// the pass qualifies the provider.
func TestProbeResolutionThroughChannel(t *testing.T) {
	parent, client, forwarded := probeTestParent(t)

	// the pass's sample is deterministic from the destination and pass index,
	// so the test can predict what will be asked
	destination := client.probeDestination()
	hosts, resolver := sampleProbeTargets(probeSeedBase(destination), probeSampleHostCount)
	expectedNames := []string{}
	expectedLiterals := 0
	for _, host := range hosts {
		if net.ParseIP(host) != nil {
			expectedLiterals += 1
		} else if len(expectedNames) < probeResolveNameCount {
			expectedNames = append(expectedNames, host)
		}
	}
	if len(expectedNames) == 0 {
		t.Fatal("the first sample block holds no hostnames; the fixture cannot exercise resolution")
	}

	resultCh := make(chan probeResult, 1)
	go func() {
		resultCh <- parent.probeProviderPass(client)
	}()

	// stage A: one A query per sampled hostname, udp/53, to the sampled
	// resolver, each a probe flow in the reserved source range
	dnsProbes := waitForProbeFlows(t, parent, IpProtocolUdp, len(expectedNames))
	answeredIps := map[string]net.IP{}
	for i, probe := range dnsProbes {
		if got := probe.ipPath.DestinationIp.String(); got != resolver {
			t.Errorf("resolution query went to %s, want the sampled resolver %s", got, resolver)
		}
		if !probe.target.CaptureAnswer {
			t.Error("a resolution query is not capture-marked; its answer would be dropped")
		}
		ip := net.IPv4(198, 51, 100, byte(10+i))
		answeredIps[probe.target.QueryName] = ip
		payload := dnsTestAnswer(t, uint16(probe.synSequence), probe.target.QueryName, 0x8180, []dnsTestRecord{
			dnsTestARecord(ip),
		})
		answerPacket := ipOosUdpPacket(probe.ipPath.Reverse(), payload)
		ingressPath, err := ParseIpPath(answerPacket)
		if err != nil {
			t.Fatal(err)
		}
		parent.clientReceivePacket(client, TransferPath{}, 0, ingressPath, answerPacket)
	}

	// stage B: tcp syns to exactly the answered addresses (plus any literal
	// hosts the sample held). Answer each with a SynAck.
	tcpProbes := waitForProbeFlows(t, parent, IpProtocolTcp, len(expectedNames)+expectedLiterals)
	resolvedSeen := 0
	for _, probe := range tcpProbes {
		if expected, ok := answeredIps[probe.target.Host]; ok {
			if !probe.ipPath.DestinationIp.Equal(expected) {
				t.Errorf("tcp probe for %s dialed %s, want the resolved %s",
					probe.target.Host, probe.ipPath.DestinationIp, expected)
			}
			resolvedSeen += 1
		}
		ingressPath, packet := probeTestSynAck(t, probe.ipPath, probe.synSequence)
		parent.clientReceivePacket(client, TransferPath{}, 0, ingressPath, packet)
	}
	if resolvedSeen != len(expectedNames) {
		t.Errorf("saw %d tcp probes for resolved names, want %d", resolvedSeen, len(expectedNames))
	}

	var result probeResult
	select {
	case result = <-resultCh:
	case <-time.After(10 * time.Second):
		t.Fatal("the pass did not complete after every probe was answered")
	}

	if !result.Passed {
		t.Errorf("a fully answered pass failed: %d/%d", result.Answered, result.Sent)
	}
	if result.Sent != len(expectedNames)+expectedLiterals {
		t.Errorf("stage B sent %d, want %d (resolved + literal)", result.Sent, len(expectedNames)+expectedLiterals)
	}
	if !parent.providerQualified(destination) {
		t.Error("a passed pass did not qualify the provider")
	}
	// the resolution queries count in the raw metrics alongside the pass's own
	wantSent := uint64(len(expectedNames) + result.Sent)
	if got := parent.reliabilityMetrics.probesSent.Load(); got != wantSent {
		t.Errorf("probesSent = %d, want %d (resolution + stage B)", got, wantSent)
	}
	if n := len(*forwarded); n != 0 {
		t.Errorf("%d probe packet(s) reached the application", n)
	}
}

// A silent resolver must not read as an unqualifiable provider: the pass falls
// back to the table's literal-ip targets, and answering those still qualifies.
func TestProbeResolverDownFallsBackToLiterals(t *testing.T) {
	parent, client, _ := probeTestParent(t)
	// short resolution stage so the fallback is reached quickly; long enough
	// that the fallback's own probes can be answered without racing the pass
	// deadline on a loaded ci machine
	parent.settings.ProbeTimeout = 1 * time.Second

	resultCh := make(chan probeResult, 1)
	go func() {
		resultCh <- parent.probeProviderPass(client)
	}()

	// the resolution stage registers its udp flows... and the test answers
	// nothing: the resolver is down
	waitForProbeFlows(t, parent, IpProtocolUdp, 1)

	// stage B arrives after the resolution deadline, carrying literal-ip
	// targets only
	literalTargets := map[string]bool{}
	for _, target := range probeFallbackLiteralTargets() {
		literalTargets[target.Host] = true
	}
	if len(literalTargets) < 3 {
		t.Fatalf("the table holds %d literal-ip hosts; the fallback needs several", len(literalTargets))
	}
	tcpProbes := waitForProbeFlows(t, parent, IpProtocolTcp, len(literalTargets))
	for _, probe := range tcpProbes {
		if !literalTargets[probe.target.Host] {
			t.Errorf("fallback pass probed %q, not a literal-ip host", probe.target.Host)
		}
		ingressPath, packet := probeTestSynAck(t, probe.ipPath, probe.synSequence)
		parent.clientReceivePacket(client, TransferPath{}, 0, ingressPath, packet)
	}

	var result probeResult
	select {
	case result = <-resultCh:
	case <-time.After(10 * time.Second):
		t.Fatal("the fallback pass did not complete")
	}

	if !result.Passed {
		t.Errorf("the literal-only fallback pass failed: %d/%d", result.Answered, result.Sent)
	}
	if !parent.providerQualified(client.probeDestination()) {
		t.Error("a provider with a dead resolver could not be qualified: the fallback is broken")
	}
}

// --- the prober plan ---

// The planner's decision table, driven pure: which clients get probed given
// their qualification ages, flow counts, and attempt history.
func TestProberPlanTable(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name      string
		candidate proberCandidate
		want      bool
	}{
		{"never probed: the startup sweep and the joiner probe",
			proberCandidate{}, true},
		{"in flight is never doubled",
			proberCandidate{inFlight: true}, false},
		{"attempt floor holds even when never recorded",
			proberCandidate{lastAttemptAt: now.Add(-proberAttemptMinInterval / 2)}, false},
		{"attempt floor releases",
			proberCandidate{lastAttemptAt: now.Add(-proberAttemptMinInterval - time.Second)}, true},
		{"fresh qualification needs nothing",
			proberCandidate{
				lastProbeAt: now.Add(-time.Minute),
				qualifiedAt: now.Add(-time.Minute),
			}, false},
		{"stale idle: re-probed past the reprobe interval",
			proberCandidate{
				lastProbeAt: now.Add(-proberReprobeInterval - time.Second),
				qualifiedAt: now.Add(-QualificationMaxAge - time.Second),
			}, true},
		{"stale but recently probed: waits out the interval",
			proberCandidate{
				lastProbeAt: now.Add(-time.Minute),
				qualifiedAt: now.Add(-QualificationMaxAge - time.Second),
			}, false},
		{"stale and LOADED: never re-probed, receive progress refreshes it",
			proberCandidate{
				flowCount:   3,
				lastProbeAt: now.Add(-proberReprobeInterval - time.Second),
				qualifiedAt: now.Add(-QualificationMaxAge - time.Second),
			}, false},
		{"failed before, idle, past the interval: asked again",
			proberCandidate{
				lastProbeAt: now.Add(-proberReprobeInterval - time.Second),
			}, true},
		{"a NEW loaded client is still swept: never probed wins over loaded",
			proberCandidate{flowCount: 5}, true},
	}
	for _, c := range cases {
		picks := proberPlan(now, []proberCandidate{c.candidate})
		if got := len(picks) == 1; got != c.want {
			t.Errorf("%s: probe=%v, want %v", c.name, got, c.want)
		}
	}

	// indexes come back aligned to the input
	picks := proberPlan(now, []proberCandidate{
		{},
		{inFlight: true},
		{},
	})
	if len(picks) != 2 || picks[0] != 0 || picks[1] != 2 {
		t.Errorf("picks = %v, want [0 2]", picks)
	}
}

// The loop must consult the plan and run passes through the bounded semaphore
// -- a planner that is correct but unconsulted probes nothing (or everything).
func TestProberLoopSourceAnchors(t *testing.T) {
	source, err := readSource("ip_remote_multi_client_prober.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := functionBody(source, "func (self *RemoteUserNatMultiClient) runProber()")
	if !ok {
		t.Fatal("could not find runProber")
	}
	for _, required := range []string{
		"proberPlan(",
		"self.probeProviderPass(",
		"proberConcurrency",
		"self.clientFlowCount(",
		"self.qualificationSnapshot(",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("runProber does not contain %s: the loop is not consulting the machinery it exists for", required)
		}
	}

	// and the constructor starts it, gated on the setting
	mainSource, err := readSource("ip_remote_multi_client.go")
	if err != nil {
		t.Fatal(err)
	}
	ctorBody, ok := functionBody(mainSource, "func NewRemoteUserNatMultiClient(")
	if !ok {
		t.Fatal("could not find NewRemoteUserNatMultiClient")
	}
	if !strings.Contains(ctorBody, "runProber") {
		t.Error("the constructor does not start the prober loop")
	}
	if !strings.Contains(ctorBody, "settings.ProviderProbe") {
		t.Error("the prober loop start is not gated on ProviderProbe")
	}
}

// --- the effectiveTier demerit ---

// The qualification demerit's decision table: +1 for unproven, gone when
// proven, gone when the kill switch is off, and -- the bare-fixture invariant
// every injected func here obeys -- absent entirely when the lookup is not
// wired.
func TestEffectiveTierUnprovenDemerit(t *testing.T) {
	qualifiedAs := func(qualified bool) func(MultiHopId) bool {
		return func(MultiHopId) bool { return qualified }
	}

	// unproven: +1
	unproven := effectiveTierTestChannel(0)
	unproven.providerQualifiedFunc = qualifiedAs(false)
	AssertEqual(t, unproven.effectiveTier(), 1)

	// proven: clean
	proven := effectiveTierTestChannel(0)
	proven.providerQualifiedFunc = qualifiedAs(true)
	AssertEqual(t, proven.effectiveTier(), 0)

	// nil func (bare fixture): no demerit, the probe machinery's absence must
	// never demote anyone
	bare := effectiveTierTestChannel(0)
	AssertEqual(t, bare.effectiveTier(), 0)

	// the kill switch removes the mechanism's every effect
	off := effectiveTierTestChannel(0)
	off.settings.ProviderProbe = false
	off.providerQualifiedFunc = qualifiedAs(false)
	AssertEqual(t, off.effectiveTier(), 0)

	// EffectiveTierSelection off is the static A/B point, demerit included
	static := effectiveTierTestChannel(0)
	static.settings.EffectiveTierSelection = false
	static.providerQualifiedFunc = qualifiedAs(false)
	AssertEqual(t, static.effectiveTier(), 0)

	// it stacks with the evidence demerits, one step behind a starved +2
	stacked := effectiveTierTestChannel(1)
	stacked.providerQualifiedFunc = qualifiedAs(false)
	starveChannel(stacked)
	AssertEqual(t, stacked.effectiveTier(), 4)

	// and the quantum is +1 on purpose: an unproven tier-0 ties a proven
	// tier-1 rather than falling behind it -- unqualified is a starting
	// state, not evidence of failure
	unprovenBest := effectiveTierTestChannel(0)
	unprovenBest.providerQualifiedFunc = qualifiedAs(false)
	provenNext := effectiveTierTestChannel(1)
	provenNext.providerQualifiedFunc = qualifiedAs(true)
	kept := minTierClients([]*multiClientChannel{unprovenBest, provenNext})
	if len(kept) != 2 {
		t.Fatalf("got %d clients, want both: unproven tier-0 must tie proven tier-1, not lose to it", len(kept))
	}
}

// --- the receive refresh ---

// Receive progress refreshes qualification through the atomic interval gate:
// the first ack refreshes, the packets after it do not, an aged stamp does,
// and the kill switch stops it.
func TestProbeReceiveRefreshOnAck(t *testing.T) {
	settings := DefaultMultiClientSettings()
	var refreshes atomic.Int64
	client := &multiClientChannel{
		settings:    settings,
		packetStats: &clientWindowStats{log: loggerOrDefault(settings.Log)},
		qualificationRefreshFunc: func(MultiHopId) {
			refreshes.Add(1)
		},
	}

	// first ack ever: refresh
	client.addReceiveAck(1440)
	AssertEqual(t, refreshes.Load(), int64(1))

	// inside the interval: the gate holds, whatever the traffic
	for i := 0; i < 100; i += 1 {
		client.addReceiveAck(1440)
	}
	AssertEqual(t, refreshes.Load(), int64(1))

	// past the interval: the next ack refreshes again
	client.qualificationRefreshedNanos.Store(
		time.Now().Add(-qualificationReceiveRefreshInterval - time.Second).UnixNano(),
	)
	client.addReceiveAck(1440)
	AssertEqual(t, refreshes.Load(), int64(2))

	// the kill switch: no refresh, however stale the stamp
	offSettings := DefaultMultiClientSettings()
	offSettings.ProviderProbe = false
	client.settings = offSettings
	client.qualificationRefreshedNanos.Store(0)
	client.addReceiveAck(1440)
	AssertEqual(t, refreshes.Load(), int64(2))

	// nil func (bare fixture): no panic -- pinned by every addReceiveAck
	// fixture in the suite, but cheap to say here too
	bare := &multiClientChannel{
		settings:    settings,
		packetStats: &clientWindowStats{log: loggerOrDefault(settings.Log)},
	}
	bare.addReceiveAck(1440)
}

// The refresh must be wired at addReceiveAck, outside its locked section --
// the parent lock the refresh takes must never nest inside the channel's.
func TestProbeReceiveRefreshSiteAnchor(t *testing.T) {
	source, err := readSource("ip_remote_multi_client.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := functionBody(source, "func (self *multiClientChannel) addReceiveAck(")
	if !ok {
		t.Fatal("could not find addReceiveAck")
	}
	if !strings.Contains(body, "touchQualificationOnReceive()") {
		t.Error("addReceiveAck does not touch the qualification: loaded exits go stale and get re-probed for nothing")
	}
}

// --- qualification readouts ---

func TestQualificationSnapshotAndPassIndex(t *testing.T) {
	parent, client, _ := probeTestParent(t)
	destination := client.probeDestination()

	lastProbeAt, qualifiedAt, passIndex := parent.qualificationSnapshot(destination)
	if !lastProbeAt.IsZero() || !qualifiedAt.IsZero() || passIndex != 0 {
		t.Error("an unknown destination must snapshot as all zeroes")
	}

	parent.recordProbeFail(destination)
	lastProbeAt, qualifiedAt, passIndex = parent.qualificationSnapshot(destination)
	if lastProbeAt.IsZero() || !qualifiedAt.IsZero() || passIndex != 1 {
		t.Errorf("after a fail: lastProbeAt zero=%v qualifiedAt zero=%v passIndex=%d",
			lastProbeAt.IsZero(), qualifiedAt.IsZero(), passIndex)
	}

	parent.recordProbePass(destination)
	_, qualifiedAt, passIndex = parent.qualificationSnapshot(destination)
	if qualifiedAt.IsZero() || passIndex != 2 {
		t.Errorf("after a pass: qualifiedAt zero=%v passIndex=%d", qualifiedAt.IsZero(), passIndex)
	}
}

// Exits carries the proven state and the probe age, so the dev screen can
// show the chip.
func TestExitsReportProvenAndProbeAge(t *testing.T) {
	settings := DefaultMultiClientSettings()
	client := &multiClientChannel{
		ctx:      context.Background(),
		args:     &multiClientChannelArgs{DestinationStats: DestinationStats{Tier: 0}},
		settings: settings,
	}

	mc := &RemoteUserNatMultiClient{
		settings: settings,
		windows: map[WindowType]*multiClientWindow{
			WindowTypeQuality: &multiClientWindow{
				settings: settings,
				clients:  map[Id]*multiClientChannel{{}: client},
			},
		},
	}

	// never probed: not proven, age -1
	exits := mc.Exits()
	AssertEqual(t, len(exits), 1)
	AssertEqual(t, exits[0].Proven, false)
	AssertEqual(t, exits[0].ProbeAge, time.Duration(-1))

	// proven: the chip and a young age
	mc.recordProbePass(client.probeDestination())
	exits = mc.Exits()
	AssertEqual(t, exits[0].Proven, true)
	if exits[0].ProbeAge < 0 || QualificationMaxAge <= exits[0].ProbeAge {
		t.Errorf("ProbeAge = %s, want a young age", exits[0].ProbeAge)
	}

	// stale: the age survives, the chip does not
	func() {
		mc.stateLock.Lock()
		defer mc.stateLock.Unlock()
		mc.qualification[client.probeDestination()].qualifiedAt =
			time.Now().Add(-QualificationMaxAge - time.Minute)
	}()
	exits = mc.Exits()
	AssertEqual(t, exits[0].Proven, false)
	if exits[0].ProbeAge < QualificationMaxAge {
		t.Errorf("ProbeAge = %s, want past QualificationMaxAge", exits[0].ProbeAge)
	}
}

// --- pooling ---

// The admit chooser: qualified first, arrival order within each class, count
// respected.
func TestPoolAdmitOrder(t *testing.T) {
	// nothing qualified (the cold start): plain arrival order
	order := poolAdmitOrder([]bool{false, false, false}, 2)
	if len(order) != 2 || order[0] != 0 || order[1] != 1 {
		t.Errorf("cold start order = %v, want [0 1]", order)
	}

	// qualified candidates jump the line, stable within each class
	order = poolAdmitOrder([]bool{false, true, false, true}, 4)
	want := []int{1, 3, 0, 2}
	if len(order) != 4 {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}

	// the count truncates after the preference is applied
	order = poolAdmitOrder([]bool{false, false, true}, 1)
	if len(order) != 1 || order[0] != 2 {
		t.Errorf("count 1 order = %v, want [2]", order)
	}

	// degenerate inputs
	if order := poolAdmitOrder(nil, 3); order != nil {
		t.Errorf("nil input yielded %v", order)
	}
	if order := poolAdmitOrder([]bool{true}, 0); order != nil {
		t.Errorf("count 0 yielded %v", order)
	}
}

// The expand wiring: the multiple applies to the candidate-request count and
// only there; every admission routes through the pure chooser; the surplus is
// cancelled politely into the monitor's NotAdded terminal state; and the
// standing-reserve / hard-max size math stays outside pooling's reach.
func TestPoolExpandSourceAnchor(t *testing.T) {
	source, err := readSource("ip_remote_multi_client.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := functionBody(source, "func (self *multiClientWindow) expand(")
	if !ok {
		t.Fatal("could not find expand")
	}
	for _, required := range []string{
		// the multiple lands on the request count...
		"requestCount := n * evaluationPoolMultiple",
		// ...never on the admit budget, which stays the needed count
		"admitBudget := n",
		"EvaluationPoolMultiple",
		// every admission goes through the single pure chooser
		"poolAdmitOrder(",
		// the surplus terminal state
		"ProviderStateNotAdded",
		// fixed-destination generators skip the multiple
		"fixedDestination",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("expand does not contain %q: the pooling contract is not anchored", required)
		}
	}
	if !strings.Contains(body, "for i := 0; i < requestCount; i += 1") {
		t.Error("expand's request loop no longer iterates the multiplied count")
	}

	// the resize size math still applies the standing reserve and the hard max
	// to admitted counts, untouched by pooling
	resizeBody, ok := functionBody(source, "func (self *multiClientWindow) resize()")
	if !ok {
		t.Fatal("could not find resize")
	}
	if !strings.Contains(resizeBody, "standingReserveTarget(") {
		t.Error("resize no longer applies the standing reserve")
	}
	if !strings.Contains(resizeBody, "WindowSizeHardMax") {
		t.Error("resize no longer bounds by WindowSizeHardMax")
	}
}

// --- settings ---

func TestReliabilitySettingsEvaluationPoolDefaults(t *testing.T) {
	settings := DefaultMultiClientSettings()
	// mainnet-aggressive default: evaluate double, admit the needed count
	AssertEqual(t, settings.EvaluationPoolMultiple, 2)
	AssertEqual(t, settings.ProviderProbe, true)

	// the round trip through the override type -- a missed field zeroes on
	// every settings write, silently turning the behavior off
	reliabilitySettings := ReliabilitySettingsFrom(settings)
	AssertEqual(t, reliabilitySettings.EvaluationPoolMultiple, settings.EvaluationPoolMultiple)
	AssertEqual(t, reliabilitySettings.ProviderProbe, settings.ProviderProbe)
	AssertEqual(t, reliabilitySettings.ProbeTimeout, settings.ProbeTimeout)

	// nil (the bare-fixture state): 0, which expand clamps to 1 -- today's
	// behavior, so fixtures see no pooling
	bare := ReliabilitySettingsFrom(nil)
	AssertEqual(t, bare.EvaluationPoolMultiple, 0)
}

// --- metrics ---

// The probe counters must survive into the snapshot the sdk mirrors.
func TestReliabilityMetricsProbeSnapshot(t *testing.T) {
	metrics := newReliabilityMetrics()
	metrics.probeSent()
	metrics.probeSent()
	metrics.probeAnswered()
	metrics.providerQualified()

	snapshot := metrics.snapshot()
	AssertEqual(t, snapshot.ProbesSent, uint64(2))
	AssertEqual(t, snapshot.ProbesAnswered, uint64(1))
	AssertEqual(t, snapshot.ProvidersQualified, uint64(1))

	metrics.reset()
	snapshot = metrics.snapshot()
	AssertEqual(t, snapshot.ProbesSent, uint64(0))
	AssertEqual(t, snapshot.ProbesAnswered, uint64(0))
	AssertEqual(t, snapshot.ProvidersQualified, uint64(0))
}
