package connect

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The provider probe pass through the shared carrier walk (GEOMAP §2.3,
// §2.5): every probe that attested is reported whatever the target answered,
// and only a co-signed one marks the sample attested.

// One probe the seam fabricates per carrier call: what the target is taken
// to have answered, and whether the carrier dial fails.
type testLatencyProbes struct {
	stateLock sync.Mutex
	// by ip, the outcome of each successive probe of that ip; the last repeats
	outcomes map[string][]ExtenderPingOutcome
	rtts     map[string][]time.Duration
	// by ip and carrier, a dial that fails
	failCarriers map[string]ExtenderConnectMode
	calls        []string
	attestors    []*ExtenderProbeAttestor
}

// One fabricated probe of one carrier, as the test scripted it for the ip.
func (self *testLatencyProbes) probe(
	ctx context.Context,
	extenderConfig *ExtenderConfig,
	attestor *ExtenderProbeAttestor,
) (*ExtenderLatencyProbe, error) {
	ip := extenderConfig.Ip.String()
	self.stateLock.Lock()
	self.calls = append(self.calls, fmt.Sprintf("%s/%s", ip, extenderConfig.Profile.ConnectMode))
	self.attestors = append(self.attestors, attestor)
	callIndex := 0
	for _, call := range self.calls {
		if call == fmt.Sprintf("%s/%s", ip, extenderConfig.Profile.ConnectMode) {
			callIndex += 1
		}
	}
	failMode, fails := self.failCarriers[ip]
	outcomes := self.outcomes[ip]
	rtts := self.rtts[ip]
	self.stateLock.Unlock()

	if fails && failMode == extenderConfig.Profile.ConnectMode {
		return nil, fmt.Errorf("no %s route to %s in this test", failMode, ip)
	}
	pick := func(i int) int {
		return min(i, max(len(outcomes), len(rtts))-1)
	}
	i := pick(callIndex - 1)
	rtt := 10 * time.Millisecond
	if 0 < len(rtts) {
		rtt = rtts[min(i, len(rtts)-1)]
	}
	probe := &ExtenderLatencyProbe{
		Rtt:      rtt,
		Response: &protocol.ExtenderResponse{PublicKey: slices.Clone(extenderConfig.PublicKey)},
	}
	outcome := ExtenderPingUnattested
	if 0 < len(outcomes) {
		outcome = outcomes[min(i, len(outcomes)-1)]
	}
	if attestor == nil || outcome == ExtenderPingUnattested {
		return probe, nil
	}
	nonce, err := NewExtenderProbeNonce()
	if err != nil {
		return nil, err
	}
	attestation := &protocol.ExtenderProbeAttestation{
		ExtenderPublicKey: slices.Clone(extenderConfig.PublicKey),
		ProbeNonce:        nonce,
		RttMs:             extenderProbeRttMs(rtt),
		TimestampMs:       uint64(time.Now().UnixMilli()),
	}
	switch attestor.Kind() {
	case ExtenderPingerKindProvider:
		attestation.ProbeClientId = attestor.ClientId.Bytes()
	case ExtenderPingerKindExtender:
		attestation.PingerExtenderPublicKey = slices.Clone(attestor.ExtenderPublicKey)
	}
	if err := SignExtenderProbeAttestation(attestor, attestation); err != nil {
		return nil, err
	}
	probe.Attested = true
	probe.Attestation = attestation
	probe.Outcome = outcome
	switch outcome {
	case ExtenderPingCosigned:
		probe.Cosigned = true
		probe.Verdict = &protocol.ExtenderProbeVerdict{Accepted: true, Cosignature: make([]byte, 64)}
	case ExtenderPingRejected:
		probe.Verdict = &protocol.ExtenderProbeVerdict{Reason: ExtenderProbeVerdictReasonRttBelowObserved}
		probe.Reason = ExtenderProbeVerdictReasonRttBelowObserved
	case ExtenderPingUnknown:
		probe.VerdictErr = fmt.Errorf("no verdict in this test")
	}
	return probe, nil
}

// The ips probed so far, in the order the probes started.
func (self *testLatencyProbes) callsValue() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.calls)
}

// A network client whose probe pass the test runs itself, over a directory of
// verified records -- so the candidates carry the identity keys the claims
// name -- and the per-probe seam.
func newTestReportingProbeClient(
	t *testing.T,
	probes *testLatencyProbes,
	ips ...string,
) (*ExtenderNetworkClient, *ExtenderDirectory, map[string][]byte) {
	t.Helper()
	clock := newTestClock()
	directory, rootPrivateKey := newTestExtenderDirectory(t, clock, nil)
	keys := map[string][]byte{}
	for _, ip := range ips {
		publicKey := newTestExtenderKey(t)
		record := signTestRecord(
			t,
			rootPrivateKey,
			publicKey,
			clock.Now(),
			clock.Now().Add(24*time.Hour),
			testExtenderAddress(ip, ExtenderCarrierTcp, ExtenderCarrierQuic),
		)
		if _, err := directory.ApplyRecord(record, ExtenderSourceFeed); err != nil {
			t.Fatal(err)
		}
		keys[ip] = publicKey
	}
	settings := DefaultExtenderNetworkClientSettings()
	settings.Now = clock.Now
	settings.ProbeWindowCount = len(ips)
	settings.ProbeMaxCandidateCount = 8
	settings.ProbeCountPerExtender = 2
	settings.ProbeTimeout = time.Second
	settings.IpVersionSupported = func(ipVersion int) bool { return ipVersion == 4 }
	settings.ProbeLatency = probes.probe
	networkClient := &ExtenderNetworkClient{
		ctx:           t.Context(),
		log:           NewNoopLogger(),
		directory:     directory,
		settings:      settings,
		statusMonitor: NewMonitorValue[ExtenderNetworkClientStatus](ExtenderNetworkClientStatus{}),
		probeWake:     NewMonitor(),
	}
	return networkClient, directory, keys
}

// The directory's candidate at the ip, failing the test when it has none.
func testCandidateByIp(t *testing.T, directory *ExtenderDirectory, ip string) *ExtenderCandidate {
	t.Helper()
	for _, candidate := range directory.Candidates(0, 64) {
		if candidate.Ip.String() == ip {
			return candidate
		}
	}
	t.Fatalf("%s is not a candidate", ip)
	return nil
}

// Every attested probe reaches the reporter with the outcome the target gave,
// and only a co-signed candidate is recorded as attested.
func TestExtenderNetworkClientReportsEveryAttestedProbe(t *testing.T) {
	probes := &testLatencyProbes{
		outcomes: map[string][]ExtenderPingOutcome{
			"192.0.2.10": {ExtenderPingCosigned},
			"192.0.2.11": {ExtenderPingRejected},
			"192.0.2.12": {ExtenderPingUnknown},
			// an old target: no nonce, nothing to report
			"192.0.2.13": {ExtenderPingUnattested},
		},
		rtts: map[string][]time.Duration{
			"192.0.2.10": {30 * time.Millisecond, 20 * time.Millisecond},
		},
	}
	networkClient, directory, keys := newTestReportingProbeClient(
		t, probes, "192.0.2.10", "192.0.2.11", "192.0.2.12", "192.0.2.13")
	posts := newTestPingPosts()
	reporter := newTestPingReporter(t, posts, func(settings *ExtenderPingReporterSettings) {
		settings.MaxBatchCount = 64
		settings.FlushTimeout = 10 * time.Millisecond
	})
	attestor, providerPublicKey := newTestProbeAttestor(t)
	networkClient.SetProbeAttestor(attestor, reporter)

	networkClient.probePass()

	// two probes of each of the four, on the first carrier
	if calls := probes.callsValue(); len(calls) != 8 {
		t.Fatalf("calls = %v, expected two probes of each candidate", calls)
	}
	for _, carrierAttestor := range probes.attestors {
		if carrierAttestor != attestor {
			t.Fatal("a probe did not carry the provider's attestor")
		}
	}
	// the three that attested report both of their probes; the old target
	// reports nothing. The pass queued every report before it returned, so
	// once six are posted and nothing is pending there is nothing more.
	reports := posts.waitForReports(t, 6)
	waitForPingPendingCount(t, reporter, 0)
	if reports = posts.allReports(); len(reports) != 6 {
		t.Fatalf("reports = %d, expected 6", len(reports))
	}
	outcomeCounts := map[string]map[ExtenderPingOutcome]int{}
	for _, report := range reports {
		if report.PingerKind != ExtenderPingerKindProvider || report.PingerClientId != attestor.ClientId.String() {
			t.Fatalf("a report names another pinger: %+v", report)
		}
		attestation, err := report.Proto()
		if err != nil {
			t.Fatal(err)
		}
		if !VerifyExtenderProbeAttestation(providerPublicKey, attestation) {
			t.Fatal("a reported claim does not verify under the provider's key")
		}
		var ip string
		for keyIp, key := range keys {
			if hex.EncodeToString(key) == report.TargetExtenderPublicKeyHex {
				ip = keyIp
			}
		}
		if outcomeCounts[ip] == nil {
			outcomeCounts[ip] = map[ExtenderPingOutcome]int{}
		}
		outcomeCounts[ip][report.Outcome] += 1
		if report.Outcome == ExtenderPingRejected && report.Reason != ExtenderProbeVerdictReasonRttBelowObserved {
			t.Fatalf("a refusal lost its reason: %+v", report)
		}
		if (report.Cosignature != "") != (report.Outcome == ExtenderPingCosigned) {
			t.Fatalf("a %s report carries cosignature %q", report.Outcome, report.Cosignature)
		}
	}
	for ip, outcome := range map[string]ExtenderPingOutcome{
		"192.0.2.10": ExtenderPingCosigned,
		"192.0.2.11": ExtenderPingRejected,
		"192.0.2.12": ExtenderPingUnknown,
	} {
		if outcomeCounts[ip][outcome] != 2 || len(outcomeCounts[ip]) != 1 {
			t.Fatalf("%s reported %v, expected two %s", ip, outcomeCounts[ip], outcome)
		}
	}
	if _, ok := outcomeCounts["192.0.2.13"]; ok {
		t.Fatal("an unattested probe was reported")
	}

	// the directory keeps the lowest rtt, and marks only the co-signed
	cosigned := testCandidateByIp(t, directory, "192.0.2.10")
	if !cosigned.LatencyAttested || cosigned.Latency != 20*time.Millisecond {
		t.Fatalf("the co-signed sample is %s attested=%t", cosigned.Latency, cosigned.LatencyAttested)
	}
	for _, ip := range []string{"192.0.2.11", "192.0.2.12", "192.0.2.13"} {
		candidate := testCandidateByIp(t, directory, ip)
		if candidate.LatencyAttested || candidate.Latency <= 0 {
			t.Fatalf("%s sample is %s attested=%t", ip, candidate.Latency, candidate.LatencyAttested)
		}
	}

	// the next attesting pass measures again only what the targets did not
	// co-sign: a refusal or a missing verdict is not an attested sample
	before := len(probes.callsValue())
	networkClient.probePass()
	calls := probes.callsValue()[before:]
	for _, call := range calls {
		if call == "192.0.2.10/"+string(ExtenderConnectModeTcpTls) {
			t.Fatalf("the co-signed candidate was probed again: %v", calls)
		}
	}
	if len(calls) != 6 {
		t.Fatalf("second pass calls = %v, expected the three uncosigned again", calls)
	}
}

// One co-signed probe is enough: a candidate whose first probe was refused and
// second co-signed is attested, and both claims are reported.
func TestExtenderNetworkClientAttestsWhenAnyProbeIsCosigned(t *testing.T) {
	probes := &testLatencyProbes{
		outcomes: map[string][]ExtenderPingOutcome{
			"192.0.2.10": {ExtenderPingRejected, ExtenderPingCosigned},
		},
		rtts: map[string][]time.Duration{
			"192.0.2.10": {15 * time.Millisecond, 25 * time.Millisecond},
		},
	}
	networkClient, directory, _ := newTestReportingProbeClient(t, probes, "192.0.2.10")
	posts := newTestPingPosts()
	reporter := newTestPingReporter(t, posts, func(settings *ExtenderPingReporterSettings) {
		settings.MaxBatchCount = 2
	})
	attestor, _ := newTestProbeAttestor(t)
	networkClient.SetProbeAttestor(attestor, reporter)
	networkClient.probePass()

	candidate := testCandidateByIp(t, directory, "192.0.2.10")
	// the lowest rtt is the refused probe's, and the sample is attested by
	// the co-signed one
	if !candidate.LatencyAttested || candidate.Latency != 15*time.Millisecond {
		t.Fatalf("sample = %s attested=%t", candidate.Latency, candidate.LatencyAttested)
	}
	reports := posts.waitForReports(t, 2)
	if reports[0].Outcome != ExtenderPingRejected || reports[1].Outcome != ExtenderPingCosigned {
		t.Fatalf("reports = %s, %s", reports[0].Outcome, reports[1].Outcome)
	}
}

// The carrier walk is the one the feed dial takes: a carrier that fails is a
// recorded failure and the next is probed, and only the probes of the carrier
// that answered are reported.
func TestExtenderNetworkClientProbeWalksToTheNextCarrier(t *testing.T) {
	probes := &testLatencyProbes{
		outcomes: map[string][]ExtenderPingOutcome{
			"192.0.2.10": {ExtenderPingCosigned},
		},
		failCarriers: map[string]ExtenderConnectMode{
			"192.0.2.10": ExtenderConnectModeTcpTls,
		},
	}
	networkClient, directory, _ := newTestReportingProbeClient(t, probes, "192.0.2.10")
	posts := newTestPingPosts()
	reporter := newTestPingReporter(t, posts, func(settings *ExtenderPingReporterSettings) {
		settings.MaxBatchCount = 2
	})
	attestor, _ := newTestProbeAttestor(t)
	networkClient.SetProbeAttestor(attestor, reporter)
	networkClient.probePass()

	calls := probes.callsValue()
	expected := []string{
		"192.0.2.10/" + string(ExtenderConnectModeTcpTls),
		"192.0.2.10/" + string(ExtenderConnectModeQuic),
		"192.0.2.10/" + string(ExtenderConnectModeQuic),
	}
	if !slices.Equal(calls, expected) {
		t.Fatalf("calls = %v, expected %v", calls, expected)
	}
	posts.waitForReports(t, 2)
	entry := testDirectoryEntry(t, directory, netip.MustParseAddr("192.0.2.10"))
	if entry.FailureCount != 1 || entry.SuccessCount != 1 {
		t.Fatalf("evidence = %d failures, %d successes", entry.FailureCount, entry.SuccessCount)
	}
}

// Without a reporter the pass still attests and records, and reports nothing;
// an attestor that names no single identity is refused, which leaves the
// client ranking.
func TestExtenderNetworkClientProbeAttestorInstall(t *testing.T) {
	probes := &testLatencyProbes{
		outcomes: map[string][]ExtenderPingOutcome{
			"192.0.2.10": {ExtenderPingCosigned},
		},
	}
	networkClient, directory, _ := newTestReportingProbeClient(t, probes, "192.0.2.10")
	attestor, _ := newTestProbeAttestor(t)
	networkClient.SetProbeAttestor(attestor, nil)
	networkClient.probePass()
	if !testCandidateByIp(t, directory, "192.0.2.10").LatencyAttested {
		t.Fatal("a pass with no reporter did not record the co-signed sample")
	}

	posts := newTestPingPosts()
	reporter := newTestPingReporter(t, posts, nil)
	malformed := &ExtenderProbeAttestor{
		ClientId:          NewId(),
		ExtenderPublicKey: newTestExtenderKey(t),
		Sign:              attestor.Sign,
	}
	networkClient.SetProbeAttestor(malformed, reporter)
	if installed, installedReporter := networkClient.probeAttestorValue(); installed != nil || installedReporter != nil {
		t.Fatal("a malformed attestor was installed")
	}
	// clearing the attestor clears the reporter with it
	networkClient.SetProbeAttestor(attestor, reporter)
	networkClient.SetProbeAttestor(nil, reporter)
	if installed, installedReporter := networkClient.probeAttestorValue(); installed != nil || installedReporter != nil {
		t.Fatal("clearing the attestor left a reporter")
	}
}

// The candidate-level seam returns the outcome, and only co-signed marks the
// sample attested; it hands back no claim, so nothing is reported for it.
func TestExtenderNetworkClientProbeSeamOutcome(t *testing.T) {
	for _, c := range []struct {
		outcome  ExtenderPingOutcome
		attested bool
	}{
		{outcome: ExtenderPingCosigned, attested: true},
		{outcome: ExtenderPingRejected, attested: false},
		{outcome: ExtenderPingUnknown, attested: false},
		{outcome: ExtenderPingUnattested, attested: false},
	} {
		probes := &testLatencyProbes{}
		networkClient, directory, _ := newTestReportingProbeClient(t, probes, "192.0.2.10")
		networkClient.settings.Probe = func(
			ctx context.Context,
			candidate *ExtenderCandidate,
			attestor *ExtenderProbeAttestor,
		) (time.Duration, ExtenderPingOutcome, error) {
			return 12 * time.Millisecond, c.outcome, nil
		}
		posts := newTestPingPosts()
		reporter := newTestPingReporter(t, posts, nil)
		attestor, _ := newTestProbeAttestor(t)
		networkClient.SetProbeAttestor(attestor, reporter)
		networkClient.probePass()
		candidate := testCandidateByIp(t, directory, "192.0.2.10")
		if candidate.LatencyAttested != c.attested || candidate.Latency != 12*time.Millisecond {
			t.Fatalf("%q: sample %s attested=%t", c.outcome, candidate.Latency, candidate.LatencyAttested)
		}
		if calls := probes.callsValue(); len(calls) != 0 {
			t.Fatalf("%q: the per-probe seam was called under the candidate seam", c.outcome)
		}
		if reporter.PendingCount() != 0 || reporter.PostCount() != 0 {
			t.Fatalf("%q: the candidate seam reported", c.outcome)
		}
	}
}

// An attestor cleared under a pass ends the pass at the next candidate: the
// candidate in flight is probed as its probes began, no further candidate is
// attested, and the pass the clear wakes probes the rest to rank only, so a
// provider that stops providing stops attesting (DESIGNNOTES4.md §1).
func TestExtenderNetworkClientPassEndsWhenTheAttestorIsCleared(t *testing.T) {
	probes := &testLatencyProbes{
		outcomes: map[string][]ExtenderPingOutcome{
			"192.0.2.10": {ExtenderPingUnattested},
			"192.0.2.11": {ExtenderPingUnattested},
		},
	}
	networkClient, _, _ := newTestReportingProbeClient(t, probes, "192.0.2.10", "192.0.2.11")
	// the first probe holds the pass until the test has cleared the attestor
	started := make(chan struct{})
	release := make(chan struct{})
	var gateOnce sync.Once
	networkClient.settings.ProbeLatency = func(
		ctx context.Context,
		extenderConfig *ExtenderConfig,
		attestor *ExtenderProbeAttestor,
	) (*ExtenderLatencyProbe, error) {
		gateOnce.Do(func() {
			close(started)
			<-release
		})
		return probes.probe(ctx, extenderConfig, attestor)
	}
	attestor, _ := newTestProbeAttestor(t)
	networkClient.SetProbeAttestor(attestor, nil)

	passDone := make(chan struct{})
	go func() {
		defer close(passDone)
		networkClient.probePass()
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the pass probed nothing")
	}
	networkClient.SetProbeAttestor(nil, nil)
	close(release)
	select {
	case <-passDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the pass did not end")
	}

	probeValues := func() ([]string, []*ExtenderProbeAttestor) {
		probes.stateLock.Lock()
		defer probes.stateLock.Unlock()
		return slices.Clone(probes.calls), slices.Clone(probes.attestors)
	}
	calls, attestors := probeValues()
	if len(calls) == 0 {
		t.Fatal("the pass made no probe")
	}
	for i, call := range calls {
		if !strings.HasPrefix(call, "192.0.2.10/") {
			t.Fatalf("the pass went on to %s after the attestor was cleared", call)
		}
		if attestors[i] != attestor {
			t.Fatalf("probe %d of the candidate in flight carried %p, expected the attestor it began with", i, attestors[i])
		}
	}

	// the pass the clear woke ranks the rest only
	networkClient.probePass()
	laterCalls, laterAttestors := probeValues()
	if len(laterCalls) <= len(calls) {
		t.Fatal("the next pass probed nothing")
	}
	for i := len(calls); i < len(laterCalls); i += 1 {
		if !strings.HasPrefix(laterCalls[i], "192.0.2.11/") || laterAttestors[i] != nil {
			t.Fatalf("the next pass made %s with attestor %p, expected the rest ranked only", laterCalls[i], laterAttestors[i])
		}
	}
}
