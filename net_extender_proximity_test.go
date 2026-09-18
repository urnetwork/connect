package connect

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// The transport form of an attestation, and the extender's batching reporter
// (DESIGNNOTES4.md §3).

func TestExtenderLatencyAttestationJsonRoundTrip(t *testing.T) {
	attestor, providerPublicKey := newTestProbeAttestor(t)
	attestation := newTestProbeAttestation(t, attestor)
	if err := SignExtenderProbeAttestation(attestor, attestation); err != nil {
		t.Fatal(err)
	}

	transport := ExtenderLatencyAttestationFromProto(attestation)
	if transport.ClientId != attestor.ClientId.String() {
		t.Fatalf("client id = %q", transport.ClientId)
	}
	rebuilt, err := transport.Proto()
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(rebuilt, attestation) {
		t.Fatal("the transport form did not round trip")
	}
	// what the operator does with it
	if !VerifyExtenderProbeAttestation(providerPublicKey, rebuilt) {
		t.Fatal("the rebuilt attestation does not verify")
	}

	// a field that does not decode is an error, not a bad signature
	for name, mutate := range map[string]func(a *ExtenderLatencyAttestation){
		"client id":  func(a *ExtenderLatencyAttestation) { a.ClientId = "not-an-id" },
		"public key": func(a *ExtenderLatencyAttestation) { a.ExtenderPublicKeyHex = "zz" },
		"nonce":      func(a *ExtenderLatencyAttestation) { a.ProbeNonce = "!" },
		"signature":  func(a *ExtenderLatencyAttestation) { a.Signature = "!" },
	} {
		mutated := *transport
		mutate(&mutated)
		if _, err := mutated.Proto(); err == nil {
			t.Fatalf("%s: a malformed field decoded", name)
		}
	}
}

// A reporter over a post seam that records every batch.
type testLatencyPosts struct {
	stateLock sync.Mutex
	batches   [][]*ExtenderLatencyAttestation
	failCount int
	posted    chan struct{}
}

func newTestLatencyPosts() *testLatencyPosts {
	return &testLatencyPosts{
		posted: make(chan struct{}, 64),
	}
}

func (self *testLatencyPosts) post(ctx context.Context, args *ExtenderLatencyReportArgs) (*ExtenderLatencyReportResult, error) {
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
	self.batches = append(self.batches, args.Attestations)
	return &ExtenderLatencyReportResult{Accepted: len(args.Attestations)}, nil
}

func (self *testLatencyPosts) batchSizes() []int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	sizes := []int{}
	for _, batch := range self.batches {
		sizes = append(sizes, len(batch))
	}
	return sizes
}

func (self *testLatencyPosts) waitForPost(t *testing.T) {
	t.Helper()
	select {
	case <-self.posted:
	case <-time.After(5 * time.Second):
		t.Fatal("no post arrived")
	}
}

func newTestLatencyReporter(t *testing.T, posts *testLatencyPosts, configure func(settings *ExtenderLatencyReporterSettings)) *ExtenderLatencyReporter {
	t.Helper()
	settings := DefaultExtenderLatencyReporterSettings()
	settings.Post = posts.post
	settings.FlushTimeout = 50 * time.Millisecond
	settings.MaxBatchCount = 3
	if configure != nil {
		configure(settings)
	}
	ctx, cancel := context.WithCancel(context.Background())
	reporter := NewExtenderLatencyReporter(ctx, settings)
	t.Cleanup(func() {
		reporter.Close()
		cancel()
	})
	return reporter
}

func testAttestationWithRtt(t *testing.T, rttMs uint32) *protocol.ExtenderProbeAttestation {
	t.Helper()
	attestor, _ := newTestProbeAttestor(t)
	attestation := newTestProbeAttestation(t, attestor)
	attestation.RttMs = rttMs
	return attestation
}

// A full batch posts at once; a partial one posts on the flush clock.
func TestExtenderLatencyReporterBatchesAndFlushes(t *testing.T) {
	posts := newTestLatencyPosts()
	reporter := newTestLatencyReporter(t, posts, nil)

	for i := range 3 {
		reporter.Report(testAttestationWithRtt(t, uint32(i+1)))
	}
	posts.waitForPost(t)
	if sizes := posts.batchSizes(); len(sizes) != 1 || sizes[0] != 3 {
		t.Fatalf("batches = %v, expected one of three", sizes)
	}

	start := time.Now()
	reporter.Report(testAttestationWithRtt(t, 4))
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
	if posts.batches[0][0].RttMs != 1 || posts.batches[0][2].RttMs != 3 || posts.batches[1][0].RttMs != 4 {
		t.Fatal("the batches are out of order")
	}
}

// A failed post keeps the batch and retries it after the flush timeout.
func TestExtenderLatencyReporterRetriesAFailedPost(t *testing.T) {
	posts := newTestLatencyPosts()
	posts.failCount = 1
	reporter := newTestLatencyReporter(t, posts, nil)

	for i := range 3 {
		reporter.Report(testAttestationWithRtt(t, uint32(i+1)))
	}
	// the failure: the batch goes back to pending once the post returns
	posts.waitForPost(t)
	if len(posts.batchSizes()) != 0 {
		t.Fatal("a failed post was recorded as a batch")
	}
	waitForPendingCount(t, reporter, 3)
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
func waitForPendingCount(t *testing.T, reporter *ExtenderLatencyReporter, count int) {
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
// that cannot be reached never grows the extender. The full batch posts,
// fails, and is requeued; the two that arrive after it push the oldest out.
func TestExtenderLatencyReporterDropsTheOldestBeyondTheCap(t *testing.T) {
	posts := newTestLatencyPosts()
	posts.failCount = 1000
	reporter := newTestLatencyReporter(t, posts, func(settings *ExtenderLatencyReporterSettings) {
		// one post, on the full batch; the retry is an hour out
		settings.FlushTimeout = time.Hour
		settings.MaxBatchCount = 10
		settings.MaxPendingCount = 10
	})
	for i := range 12 {
		reporter.Report(testAttestationWithRtt(t, uint32(i+1)))
	}
	posts.waitForPost(t)
	waitForPendingCount(t, reporter, 10)
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

// An operator refusal is not retried: the extender cannot fix a provider's
// signature, and retrying would only repeat the refusal.
func TestExtenderLatencyReporterDoesNotRetryARefusal(t *testing.T) {
	refused := make(chan struct{}, 4)
	settings := DefaultExtenderLatencyReporterSettings()
	settings.FlushTimeout = 20 * time.Millisecond
	settings.Post = func(ctx context.Context, args *ExtenderLatencyReportArgs) (*ExtenderLatencyReportResult, error) {
		select {
		case refused <- struct{}{}:
		default:
		}
		return &ExtenderLatencyReportResult{Error: "not an extender"}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	reporter := NewExtenderLatencyReporter(ctx, settings)
	t.Cleanup(func() {
		reporter.Close()
		cancel()
	})
	reporter.Report(testAttestationWithRtt(t, 1))
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

func TestExtenderLatencyReporterIgnoresNil(t *testing.T) {
	posts := newTestLatencyPosts()
	reporter := newTestLatencyReporter(t, posts, nil)
	reporter.Report(nil)
	if reporter.PendingCount() != 0 {
		t.Fatal("a nil attestation was queued")
	}
}
