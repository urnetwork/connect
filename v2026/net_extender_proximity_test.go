package connect

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The transport form of a ping, and the pinger's batching reporter
// (GEOMAP §2.5).

// One report of each outcome for both pinger kinds, with what the operator
// needs to check it: the pinger's key and the target's.
type testPingReportCase struct {
	name            string
	attestation     *protocol.ExtenderProbeAttestation
	verdict         *protocol.ExtenderProbeVerdict
	outcome         ExtenderPingOutcome
	pingerPublicKey []byte
	targetPublicKey []byte
}

// A ping of each outcome from each kind of pinger, signed.
func newTestPingReportCases(t *testing.T) []*testPingReportCase {
	t.Helper()
	cases := []*testPingReportCase{}
	provider, providerPublicKey := newTestProbeAttestor(t)
	extender, extenderPublicKey := newTestPeerProbeAttestor(t)
	for _, pinger := range []struct {
		attestor  *ExtenderProbeAttestor
		publicKey []byte
	}{
		{attestor: provider, publicKey: providerPublicKey},
		{attestor: extender, publicKey: extenderPublicKey},
	} {
		for _, outcome := range []ExtenderPingOutcome{ExtenderPingCosigned, ExtenderPingRejected, ExtenderPingUnknown} {
			targetPublicKey, targetPrivateKey, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			attestation := newTestProbeAttestation(t, pinger.attestor)
			attestation.ExtenderPublicKey = slices.Clone(targetPublicKey)
			if err := SignExtenderProbeAttestation(pinger.attestor, attestation); err != nil {
				t.Fatal(err)
			}
			var verdict *protocol.ExtenderProbeVerdict
			switch outcome {
			case ExtenderPingCosigned:
				cosignature, err := SignExtenderProbeVerdict(testTargetSign(targetPrivateKey), attestation)
				if err != nil {
					t.Fatal(err)
				}
				verdict = &protocol.ExtenderProbeVerdict{Accepted: true, Cosignature: cosignature}
			case ExtenderPingRejected:
				verdict = &protocol.ExtenderProbeVerdict{Reason: ExtenderProbeVerdictReasonRttBelowObserved}
			}
			cases = append(cases, &testPingReportCase{
				name:            fmt.Sprintf("%s %s", pinger.attestor.Kind(), outcome),
				attestation:     attestation,
				verdict:         verdict,
				outcome:         outcome,
				pingerPublicKey: pinger.publicKey,
				targetPublicKey: targetPublicKey,
			})
		}
	}
	return cases
}

// Every ping survives its transport form: the claim, the outcome and the
// verdict come back from the json as they went in.
func TestExtenderPingReportJsonRoundTrip(t *testing.T) {
	for _, c := range newTestPingReportCases(t) {
		report := ExtenderPingReportFromProto(c.attestation, c.outcome, c.verdict)
		kind := ExtenderProbeAttestationPingerKind(c.attestation)
		if report.PingerKind != kind || report.Outcome != c.outcome {
			t.Fatalf("%s: kind %q outcome %q", c.name, report.PingerKind, report.Outcome)
		}
		switch kind {
		case ExtenderPingerKindProvider:
			clientId, err := IdFromBytes(c.attestation.ProbeClientId)
			if err != nil {
				t.Fatal(err)
			}
			if report.PingerClientId != clientId.String() || report.PingerExtenderPublicKeyHex != "" {
				t.Fatalf("%s: pinger = %q / %q", c.name, report.PingerClientId, report.PingerExtenderPublicKeyHex)
			}
		case ExtenderPingerKindExtender:
			if report.PingerExtenderPublicKeyHex != hex.EncodeToString(c.pingerPublicKey) || report.PingerClientId != "" {
				t.Fatalf("%s: pinger = %q / %q", c.name, report.PingerClientId, report.PingerExtenderPublicKeyHex)
			}
		}
		if report.TargetExtenderPublicKeyHex != hex.EncodeToString(c.targetPublicKey) {
			t.Fatalf("%s: target = %q", c.name, report.TargetExtenderPublicKeyHex)
		}
		// only a co-signed ping carries the co-signature
		if (report.Cosignature != "") != (c.outcome == ExtenderPingCosigned) {
			t.Fatalf("%s: cosignature = %q", c.name, report.Cosignature)
		}
		if c.outcome == ExtenderPingRejected && report.Reason != ExtenderProbeVerdictReasonRttBelowObserved {
			t.Fatalf("%s: reason = %d", c.name, report.Reason)
		}

		// the json is the server's contract: every field, under its name
		reportBytes, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		fields := map[string]any{}
		if err := json.Unmarshal(reportBytes, &fields); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{
			"pinger_kind",
			"pinger_client_id",
			"pinger_extender_public_key_hex",
			"target_extender_public_key_hex",
			"probe_nonce",
			"rtt_ms",
			"timestamp_ms",
			"signature",
			"outcome",
			"reason",
			"cosignature",
			"hop_count",
		} {
			if _, ok := fields[name]; !ok {
				t.Fatalf("%s: the json has no %q: %s", c.name, name, reportBytes)
			}
		}
		if len(fields) != 12 {
			t.Fatalf("%s: the json has %d fields: %s", c.name, len(fields), reportBytes)
		}
		if fields["pinger_kind"] != string(kind) || fields["outcome"] != string(c.outcome) {
			t.Fatalf("%s: the json kind or outcome is wrong: %s", c.name, reportBytes)
		}
		decoded := &ExtenderPingReport{}
		if err := json.Unmarshal(reportBytes, decoded); err != nil {
			t.Fatal(err)
		}

		// what the operator does with it
		rebuilt, err := decoded.Proto()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !proto.Equal(rebuilt, c.attestation) {
			t.Fatalf("%s: the transport form did not round trip", c.name)
		}
		if !VerifyExtenderProbeAttestation(c.pingerPublicKey, rebuilt) {
			t.Fatalf("%s: the rebuilt attestation does not verify", c.name)
		}
		verdict, err := decoded.VerdictProto()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		switch c.outcome {
		case ExtenderPingCosigned:
			if !VerifyExtenderProbeVerdict(c.targetPublicKey, rebuilt, verdict) {
				t.Fatalf("%s: the rebuilt co-signature does not verify", c.name)
			}
		case ExtenderPingRejected:
			if verdict == nil || verdict.Accepted || verdict.Reason != ExtenderProbeVerdictReasonRttBelowObserved || verdict.Cosignature != nil {
				t.Fatalf("%s: the rebuilt refusal is %v", c.name, verdict)
			}
		case ExtenderPingUnknown:
			if verdict != nil {
				t.Fatalf("%s: an unknown ping rebuilt a verdict %v", c.name, verdict)
			}
		}
	}
}

// An acceptance whose co-signature did not verify is reported as the refusal
// it amounts to, without the co-signature: the pinger cannot show it.
func TestExtenderPingReportDropsAnUnverifiedCosignature(t *testing.T) {
	attestation, _, _ := newTestCosignFixture(t, ExtenderPingerKindExtender)
	verdict := &protocol.ExtenderProbeVerdict{Accepted: true, Cosignature: make([]byte, 64)}
	report := ExtenderPingReportFromProto(attestation, ExtenderPingRejected, verdict)
	if report.Cosignature != "" || report.Reason != 0 || report.Outcome != ExtenderPingRejected {
		t.Fatalf("report = %+v", report)
	}
	rebuilt, err := report.VerdictProto()
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Accepted {
		t.Fatal("an unverified acceptance was rebuilt as an acceptance")
	}
}

// A field that does not decode, or fields that contradict the pinger kind,
// are an error, not a signature that does not verify.
func TestExtenderPingReportRefusesMalformed(t *testing.T) {
	cases := newTestPingReportCases(t)
	var providerReport, extenderReport, cosignedReport *ExtenderPingReport
	for _, c := range cases {
		report := ExtenderPingReportFromProto(c.attestation, c.outcome, c.verdict)
		switch {
		case report.PingerKind == ExtenderPingerKindProvider && c.outcome == ExtenderPingRejected:
			providerReport = report
		case report.PingerKind == ExtenderPingerKindExtender && c.outcome == ExtenderPingCosigned:
			extenderReport = report
			cosignedReport = report
		}
	}
	for name, c := range map[string]struct {
		report *ExtenderPingReport
		mutate func(r *ExtenderPingReport)
	}{
		"unknown kind":           {report: providerReport, mutate: func(r *ExtenderPingReport) { r.PingerKind = "consumer" }},
		"no kind":                {report: providerReport, mutate: func(r *ExtenderPingReport) { r.PingerKind = "" }},
		"provider with a key":    {report: providerReport, mutate: func(r *ExtenderPingReport) { r.PingerExtenderPublicKeyHex = extenderReport.PingerExtenderPublicKeyHex }},
		"extender with a client": {report: extenderReport, mutate: func(r *ExtenderPingReport) { r.PingerClientId = providerReport.PingerClientId }},
		"bad client id":          {report: providerReport, mutate: func(r *ExtenderPingReport) { r.PingerClientId = "not-an-id" }},
		"no client id":           {report: providerReport, mutate: func(r *ExtenderPingReport) { r.PingerClientId = "" }},
		"bad pinger key":         {report: extenderReport, mutate: func(r *ExtenderPingReport) { r.PingerExtenderPublicKeyHex = "zz" }},
		"short pinger key":       {report: extenderReport, mutate: func(r *ExtenderPingReport) { r.PingerExtenderPublicKeyHex = "abcd" }},
		"no pinger key":          {report: extenderReport, mutate: func(r *ExtenderPingReport) { r.PingerExtenderPublicKeyHex = "" }},
		"bad target key":         {report: providerReport, mutate: func(r *ExtenderPingReport) { r.TargetExtenderPublicKeyHex = "zz" }},
		"bad nonce":              {report: providerReport, mutate: func(r *ExtenderPingReport) { r.ProbeNonce = "!" }},
		"bad signature":          {report: extenderReport, mutate: func(r *ExtenderPingReport) { r.Signature = "!" }},
	} {
		mutated := *c.report
		c.mutate(&mutated)
		if _, err := mutated.Proto(); err == nil {
			t.Fatalf("%s: a malformed report decoded", name)
		}
	}
	for name, mutate := range map[string]func(r *ExtenderPingReport){
		"unknown outcome":           func(r *ExtenderPingReport) { r.Outcome = "accepted" },
		"no outcome":                func(r *ExtenderPingReport) { r.Outcome = ExtenderPingUnattested },
		"cosigned without":          func(r *ExtenderPingReport) { r.Cosignature = "" },
		"cosigned bad base64":       func(r *ExtenderPingReport) { r.Cosignature = "!" },
		"rejected with cosignature": func(r *ExtenderPingReport) { r.Outcome = ExtenderPingRejected },
		"unknown with cosignature":  func(r *ExtenderPingReport) { r.Outcome = ExtenderPingUnknown },
	} {
		mutated := *cosignedReport
		mutate(&mutated)
		if _, err := mutated.VerdictProto(); err == nil {
			t.Fatalf("%s: a malformed verdict decoded", name)
		}
	}
}

// A probe reports only once it attested.
func TestExtenderPingReportFromProbe(t *testing.T) {
	if ExtenderPingReportFromProbe(nil) != nil {
		t.Fatal("no probe made a report")
	}
	if ExtenderPingReportFromProbe(&ExtenderLatencyProbe{Rtt: time.Millisecond}) != nil {
		t.Fatal("a ranking probe made a report")
	}
	attestation, _, _ := newTestCosignFixture(t, ExtenderPingerKindProvider)
	notSent := &ExtenderLatencyProbe{
		Rtt:         time.Millisecond,
		Attestation: attestation,
		AttestErr:   fmt.Errorf("the write failed"),
	}
	if ExtenderPingReportFromProbe(notSent) != nil {
		t.Fatal("an attestation that was never sent made a report")
	}
	sent := &ExtenderLatencyProbe{
		Rtt:         time.Millisecond,
		Attested:    true,
		Attestation: attestation,
		Outcome:     ExtenderPingUnknown,
	}
	report := ExtenderPingReportFromProbe(sent)
	if report == nil || report.Outcome != ExtenderPingUnknown || report.RttMs != attestation.RttMs {
		t.Fatalf("report = %+v", report)
	}
	if report.Signature != base64.StdEncoding.EncodeToString(attestation.Signature) {
		t.Fatal("the report carries another signature")
	}
}

// A reporter over a post seam that records every batch.
type testPingPosts struct {
	stateLock sync.Mutex
	batches   [][]*ExtenderPingReport
	failCount int
	posted    chan struct{}
}

// An operator that accepts every post and keeps it.
func newTestPingPosts() *testPingPosts {
	return &testPingPosts{
		posted: make(chan struct{}, 64),
	}
}

// Accepts one batch, or fails it while failures are owed.
func (self *testPingPosts) post(ctx context.Context, args *ExtenderPingReportArgs) (*ExtenderPingReportResult, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	defer func() {
		select {
		case self.posted <- struct{}{}:
		default:
		}
	}()
	if 0 < self.failCount {
		self.failCount -= 1
		return nil, fmt.Errorf("the operator is away")
	}
	self.batches = append(self.batches, args.Pings)
	return &ExtenderPingReportResult{Accepted: len(args.Pings)}, nil
}

// The size of each batch posted so far, in order.
func (self *testPingPosts) batchSizes() []int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	sizes := []int{}
	for _, batch := range self.batches {
		sizes = append(sizes, len(batch))
	}
	return sizes
}

// Every ping posted so far, in the order posted.
func (self *testPingPosts) allReports() []*ExtenderPingReport {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	reports := []*ExtenderPingReport{}
	for _, batch := range self.batches {
		reports = append(reports, batch...)
	}
	return reports
}

// Waits for the next post.
func (self *testPingPosts) waitForPost(t *testing.T) {
	t.Helper()
	select {
	case <-self.posted:
	case <-time.After(5 * time.Second):
		t.Fatal("no post arrived")
	}
}

// Waits until the posts hold `count` reports in total.
func (self *testPingPosts) waitForReports(t *testing.T, count int) []*ExtenderPingReport {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		reports := self.allReports()
		if count <= len(reports) {
			return reports
		}
		select {
		case <-self.posted:
		case <-deadline:
			t.Fatalf("reports = %d, expected %d", len(reports), count)
		}
	}
}

// A reporter posting to the posts on a short flush, with configure run on its
// settings first, closed with the test.
func newTestPingReporter(t *testing.T, posts *testPingPosts, configure func(settings *ExtenderPingReporterSettings)) *ExtenderPingReporter {
	t.Helper()
	settings := DefaultExtenderPingReporterSettings()
	settings.Post = posts.post
	settings.FlushTimeout = 50 * time.Millisecond
	settings.MaxBatchCount = 3
	if configure != nil {
		configure(settings)
	}
	ctx, cancel := context.WithCancel(context.Background())
	reporter := NewExtenderPingReporter(ctx, settings)
	t.Cleanup(func() {
		reporter.Close()
		cancel()
	})
	return reporter
}

// A signed provider ping of the rtt.
func testPingReportWithRtt(t *testing.T, rttMs uint32) *ExtenderPingReport {
	t.Helper()
	attestor, _ := newTestProbeAttestor(t)
	attestation := newTestProbeAttestation(t, attestor)
	attestation.RttMs = rttMs
	if err := SignExtenderProbeAttestation(attestor, attestation); err != nil {
		t.Fatal(err)
	}
	return ExtenderPingReportFromProto(attestation, ExtenderPingUnknown, nil)
}

// A full batch posts at once; a partial one posts on the flush clock.
func TestExtenderPingReporterBatchesAndFlushes(t *testing.T) {
	posts := newTestPingPosts()
	reporter := newTestPingReporter(t, posts, nil)

	for i := range 3 {
		reporter.Report(testPingReportWithRtt(t, uint32(i+1)))
	}
	posts.waitForPost(t)
	if sizes := posts.batchSizes(); len(sizes) != 1 || sizes[0] != 3 {
		t.Fatalf("batches = %v, expected one of three", sizes)
	}

	start := time.Now()
	reporter.Report(testPingReportWithRtt(t, 4))
	posts.waitForPost(t)
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("a partial batch posted after %s, before the flush timeout", elapsed)
	}
	if sizes := posts.batchSizes(); len(sizes) != 2 || sizes[1] != 1 {
		t.Fatalf("batches = %v, expected a second of one", sizes)
	}
	if reporter.PendingCount() != 0 {
		t.Fatalf("pending = %d after the flush", reporter.PendingCount())
	}
	// the order is the arrival order
	reports := posts.allReports()
	if reports[0].RttMs != 1 || reports[2].RttMs != 3 || reports[3].RttMs != 4 {
		t.Fatal("the batches are out of order")
	}
}

// A failed post keeps the batch and retries it after the flush timeout.
func TestExtenderPingReporterRetriesAFailedPost(t *testing.T) {
	posts := newTestPingPosts()
	posts.failCount = 1
	reporter := newTestPingReporter(t, posts, nil)

	for i := range 3 {
		reporter.Report(testPingReportWithRtt(t, uint32(i+1)))
	}
	// the failure: the batch goes back to pending once the post returns
	posts.waitForPost(t)
	if len(posts.batchSizes()) != 0 {
		t.Fatal("a failed post was recorded as a batch")
	}
	waitForPingPendingCount(t, reporter, 3)
	// the retry
	posts.waitForPost(t)
	if sizes := posts.batchSizes(); len(sizes) != 1 || sizes[0] != 3 {
		t.Fatalf("batches = %v, expected the retried batch", sizes)
	}
	if reporter.PostCount() != 2 {
		t.Fatalf("posts = %d", reporter.PostCount())
	}
}

// Waits for the pending count to settle at `count`.
func waitForPingPendingCount(t *testing.T, reporter *ExtenderPingReporter, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for reporter.PendingCount() != count {
		if time.Now().After(deadline) {
			t.Fatalf("pending = %d, expected %d", reporter.PendingCount(), count)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The pending set is bounded: beyond the cap the oldest go, so an operator
// that cannot be reached never grows the pinger. The full batch posts, fails,
// and is requeued; the two that arrive after it push the oldest out.
func TestExtenderPingReporterDropsTheOldestBeyondTheCap(t *testing.T) {
	posts := newTestPingPosts()
	posts.failCount = 1000
	reporter := newTestPingReporter(t, posts, func(settings *ExtenderPingReporterSettings) {
		// one post, on the full batch; the retry is an hour out
		settings.FlushTimeout = time.Hour
		settings.MaxBatchCount = 10
		settings.MaxPendingCount = 10
	})
	for i := range 12 {
		reporter.Report(testPingReportWithRtt(t, uint32(i+1)))
	}
	posts.waitForPost(t)
	waitForPingPendingCount(t, reporter, 10)
	reporter.stateLock.Lock()
	first, last := reporter.pending[0].RttMs, reporter.pending[9].RttMs
	reporter.stateLock.Unlock()
	if first != 3 || last != 12 {
		t.Fatalf("pending spans %d..%d, expected the oldest two dropped", first, last)
	}
	if reporter.PostCount() != 1 {
		t.Fatalf("posts = %d, expected the one failed post", reporter.PostCount())
	}
}

// An operator refusal is not retried: the pinger cannot change what it
// measured, and retrying would only repeat the refusal.
func TestExtenderPingReporterDoesNotRetryARefusal(t *testing.T) {
	refused := make(chan struct{}, 4)
	settings := DefaultExtenderPingReporterSettings()
	settings.FlushTimeout = 20 * time.Millisecond
	settings.Post = func(ctx context.Context, args *ExtenderPingReportArgs) (*ExtenderPingReportResult, error) {
		select {
		case refused <- struct{}{}:
		default:
		}
		return &ExtenderPingReportResult{Error: "not a pinger"}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	reporter := NewExtenderPingReporter(ctx, settings)
	t.Cleanup(func() {
		reporter.Close()
		cancel()
	})
	reporter.Report(testPingReportWithRtt(t, 1))
	select {
	case <-refused:
	case <-time.After(5 * time.Second):
		t.Fatal("no post")
	}
	select {
	case <-refused:
		t.Fatal("a refused batch was posted again")
	case <-time.After(100 * time.Millisecond):
	}
	if reporter.PendingCount() != 0 {
		t.Fatalf("pending = %d after a refusal", reporter.PendingCount())
	}
}

// A nil report, and a probe with no claim, is dropped rather than posted.
func TestExtenderPingReporterIgnoresNil(t *testing.T) {
	posts := newTestPingPosts()
	reporter := newTestPingReporter(t, posts, nil)
	reporter.Report(nil)
	reporter.Report(ExtenderPingReportFromProbe(&ExtenderLatencyProbe{Rtt: time.Millisecond}))
	if reporter.PendingCount() != 0 {
		t.Fatal("a nil report was queued")
	}
}

// Close joins the loop and drops what is pending, even with a post in flight
// that waits on its context.
func TestExtenderPingReporterCloseJoins(t *testing.T) {
	entered := make(chan struct{})
	settings := DefaultExtenderPingReporterSettings()
	settings.MaxBatchCount = 1
	settings.Post = func(ctx context.Context, args *ExtenderPingReportArgs) (*ExtenderPingReportResult, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	reporter := NewExtenderPingReporter(context.Background(), settings)
	reporter.Report(testPingReportWithRtt(t, 1))
	<-entered
	closed := make(chan struct{})
	go func() {
		reporter.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("close did not join the post in flight")
	}
	// a second close is a no-op
	reporter.Close()
}

// The reporter posts to the ping report path, and never to the old latency
// path.
func TestExtenderPingReportPath(t *testing.T) {
	if ExtenderPingReportPath != "/network/ping-report" {
		t.Fatalf("path = %q", ExtenderPingReportPath)
	}
	// the defaults the reporter falls back to
	settings := DefaultExtenderPingReporterSettings()
	if settings.MaxBatchCount != 64 || settings.FlushTimeout != 30*time.Second ||
		settings.MaxPendingCount != 1024 || settings.RequestTimeout != 30*time.Second {
		t.Fatalf("defaults = %+v", settings)
	}
	if _, err := PostExtenderPingReport(context.Background(), nil, "https://api.example", "", &ExtenderPingReportArgs{}); err == nil {
		t.Fatal("a post without a strategy was attempted")
	}
}
