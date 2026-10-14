package connect

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"testing"
	"time"
)

// A global pressure notification must reach the actual strategy owner, not
// only an explicitly called transport helper. The response body remains an
// active owner through Close, even after ReadAll has consumed its bytes.
func TestClientStrategyMemoryShedReleasesIdleAltOnly(t *testing.T) {
	client, hellos := newAltMemoryFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	transport := client.Transport.(*altQuicBoundedTransport)
	request := func() *http.Response {
		response, err := client.Get("https://" + testAltApiHost + "/shed")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			response.Body.Close()
			t.Fatal(err)
		}
		return response
	}
	response := request()
	first := transport.connection(testAltApiHost + ":443")
	if first == nil {
		t.Fatal("completed request did not establish an owned carrier")
	}
	ShedMemory()
	if first.Context().Err() != nil {
		t.Fatal("pressure interrupted an active response body")
	}
	response.Body.Close()
	waitAltMemorySlot(t, client)
	ShedMemory()
	if first.Context().Err() == nil {
		t.Fatal("global pressure left an idle API QUIC carrier alive")
	}
	response = request()
	response.Body.Close()
	waitAltMemorySlot(t, client)
	if hellos.Load() != 2 {
		t.Fatalf("pressure should retire only the idle carrier, got %d handshakes", hellos.Load())
	}
	ShedMemory()
}

// Keep the existing client/transport object: an active stream excluded from
// the first sweep must still be discoverable by the next sweep after Close.
// Discarding the HTTP client would strand that returning active owner.
func TestClientStrategyMemoryShedPreservesPoolOwner(t *testing.T) {
	server := newTestAltServer(t, false)
	strategy := newTestAltStrategy(t, server)
	dialer := testAltDialer(t, strategy, "alt h3")
	client := dialer.HttpClient()
	if got := testAltGet(t, dialer); got != testAltBodyText {
		t.Fatal("loopback request failed")
	}
	waitAltMemorySlot(t, client)
	first := client.Transport.(*altQuicBoundedTransport).connection(testAltApiHost + ":443")
	ShedMemory()
	if dialer.HttpClient() != client {
		t.Fatal("pressure discarded the pool owner and its TLS/session policy")
	}
	if first == nil || first.Context().Err() == nil {
		t.Fatal("pressure did not release the idle carrier")
	}
	if got := testAltGet(t, dialer); got != testAltBodyText {
		t.Fatal("pressure made the strategy terminal")
	}
}

func TestClientStrategyMemoryShedRegistrationLifetime(t *testing.T) {
	before := len(memoryShedders.Get())
	settings := DefaultClientStrategySettings()
	settings.EnableNormal, settings.EnableResilient = false, false
	settings.ExpandExtenderProfileCount = 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := NewClientStrategy(ctx, settings)
	defer strategy.Close()
	if got := len(memoryShedders.Get()); got != before+1 {
		t.Fatalf("strategy pressure registrations = %d, want %d", got, before+1)
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 50 {
				ShedMemory()
			}
		})
	}
	cancel()
	strategy.Close()
	workers.Wait()
	if got := len(memoryShedders.Get()); got != before {
		t.Fatalf("closed strategy retained its pressure callback: %d, want %d", got, before)
	}
}

func TestClientStrategyMemoryShedClosesIdleHTTP(t *testing.T) {
	server, states := newClientStrategyLifecycleServer(t, 4)
	settings := DefaultClientStrategySettings()
	settings.EnableResilient = false
	strategy := NewClientStrategy(t.Context(), settings)
	t.Cleanup(strategy.Close)
	if _, err := strategy.HttpParallel(newClientStrategyLifecycleRequest(t, t.Context(), server.URL)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-states.idle:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP owner did not become idle")
	}
	ShedMemory()
	select {
	case <-states.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("global pressure retained the idle HTTP owner")
	}
	if _, err := strategy.HttpParallel(newClientStrategyLifecycleRequest(t, t.Context(), server.URL)); err != nil {
		t.Fatalf("HTTP strategy did not recover after pressure: %v", err)
	}
}

func TestClientStrategyMemoryShedPreservesActiveHTTP(t *testing.T) {
	finish := make(chan struct{})
	var finishOnce sync.Once
	release := func() { finishOnce.Do(func() { close(finish) }) }
	defer release()
	server := newTestingLoopbackHttpServer(t, 4, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "first")
		w.(http.Flusher).Flush()
		select {
		case <-finish:
			_, _ = io.WriteString(w, "last")
		case <-r.Context().Done():
		}
	}), false)
	t.Cleanup(server.Close)
	settings := DefaultClientStrategySettings()
	settings.EnableResilient = false
	strategy := NewClientStrategy(t.Context(), settings)
	t.Cleanup(strategy.Close)
	client := testAltDialer(t, strategy, "normal").HttpClient()
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	ShedMemory()
	release()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "firstlast" {
		t.Fatalf("pressure interrupted active HTTP: body=%q err=%v", body, err)
	}
}

type memoryShedSessionCache struct {
	tls.ClientSessionCache
	stored chan struct{}
}

func TestClientStrategyMemoryShedPreservesNativeTLSResumption(t *testing.T) {
	server := newTestingLoopbackHttpServer(t, 4, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}), true)
	t.Cleanup(server.Close)
	settings := DefaultClientStrategySettings()
	settings.EnableResilient = false
	settings.ConnectSettings.TlsConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	strategy := NewClientStrategy(t.Context(), settings)
	t.Cleanup(strategy.Close)
	client := testAltDialer(t, strategy, "normal").HttpClient()
	request := func() bool {
		response, err := client.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			t.Fatal(err)
		}
		if response.ProtoMajor != 1 || response.TLS == nil {
			t.Fatal("native TLS fixture did not negotiate H1")
		}
		return response.TLS.DidResume
	}
	if request() {
		t.Fatal("first native TLS connection unexpectedly resumed")
	}
	ShedMemory()
	if !request() {
		t.Fatal("pressure did not retire and resume the native TLS carrier")
	}
}

func (self *memoryShedSessionCache) Put(key string, state *tls.ClientSessionState) {
	self.ClientSessionCache.Put(key, state)
	if state != nil {
		select {
		case self.stored <- struct{}{}:
		default:
		}
	}
}

func TestClientStrategyMemoryShedPreservesTLSResumption(t *testing.T) {
	client, _ := newAltMemoryFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	transport := client.Transport.(*altQuicBoundedTransport)
	cache := &memoryShedSessionCache{transport.transport.TLSClientConfig.ClientSessionCache, make(chan struct{}, 1)}
	if cache.ClientSessionCache == nil {
		t.Fatal("strategy did not create a bounded TLS cache")
	}
	transport.transport.TLSClientConfig.ClientSessionCache = cache
	request := func() bool {
		response, err := client.Get("https://" + testAltApiHost + "/resumption")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			t.Fatal(err)
		}
		if response.TLS == nil {
			t.Fatal("response omitted TLS state")
		}
		return response.TLS.DidResume
	}
	if request() {
		t.Fatal("first carrier unexpectedly resumed a session")
	}
	waitAltMemorySlot(t, client)
	select {
	case <-cache.stored:
	case <-time.After(3 * time.Second):
		t.Fatal("first carrier did not publish a TLS ticket")
	}
	first := transport.connection(testAltApiHost + ":443")
	ShedMemory()
	if first == nil || first.Context().Err() == nil {
		t.Fatal("pressure did not retire the carrier")
	}
	if !request() {
		t.Fatal("pressure discarded the path-local TLS resumption ticket")
	}
	waitAltMemorySlot(t, client)
}

// The normal API dialer uses the actual production TLS and HTTP transport on
// loopback. Compare warm requests with the bounded re-dial tax after pressure;
// the latter uses the same transport-only close as the pressure callback so
// baseline and candidate binaries measure identical request workloads.
func BenchmarkClientStrategyApiLoopback(b *testing.B) {
	for _, closeIdle := range []bool{false, true} {
		name := "warm"
		if closeIdle {
			name = "idle-close-resumed"
		}
		b.Run(name, func(b *testing.B) {
			server := newTestingLoopbackHttpServer(b, 4, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "ok")
			}), true)
			b.Cleanup(server.Close)
			settings := DefaultClientStrategySettings()
			settings.EnableResilient = false
			settings.Log = NewNoopLogger()
			settings.ConnectSettings.TlsConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
			strategy := NewClientStrategy(b.Context(), settings)
			b.Cleanup(strategy.Close)
			var client *http.Client
			for dialer := range strategy.dialers {
				if dialer.description == "normal" {
					client = dialer.HttpClient()
				}
			}
			if client == nil {
				b.Fatal("normal API owner missing")
			}
			request := func() {
				response, err := client.Get(server.URL)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := io.Copy(io.Discard, response.Body); err != nil {
					response.Body.Close()
					b.Fatal(err)
				}
				response.Body.Close()
			}
			request()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if closeIdle {
					client.CloseIdleConnections()
				}
				request()
			}
		})
	}
}

// Diagnostic only: isolate one or four complete idle API graphs in a fresh
// process. The loopback server is in this process, so runtime deltas include
// both ends; the client carrier reservation and connection counts are exact.
// Retain each request's actual independent owner; constructing another default
// budget at sampling time would silently report zero for a live connection.
func TestClientStrategyMemoryShedFootprintSample(t *testing.T) {
	value := os.Getenv("URNETWORK_API_MEMORY_SAMPLE")
	if value == "" {
		t.Skip("isolated idle API footprint measurement")
	}
	count, err := strconv.Atoi(value)
	if err != nil || count < 1 || count > 4 {
		t.Fatal("invalid sample count")
	}
	previousBudget := MemoryBudget()
	SetMemoryBudget(32 << 20)
	defer SetMemoryBudget(previousBudget)
	server := newTestAltServer(t, false)
	clients := make([]*http.Client, 0, count)
	budgets := make([]*PlatformTransportBudget, 0, count)
	for range count {
		strategy := newTestAltStrategy(t, server)
		dialer := testAltDialer(t, strategy, "alt h3")
		budget := DefaultPlatformTransportBudget()
		ctx := context.WithValue(t.Context(), platformTransportNestedBudgetContextKey{}, budget)
		if testAltGetWithContext(t, ctx, dialer) != testAltBodyText {
			t.Fatal("loopback response")
		}
		client := dialer.HttpClient()
		waitAltMemorySlot(t, client)
		clients = append(clients, client)
		budgets = append(budgets, budget)
	}
	sample := func() runtime.MemStats {
		debug.FreeOSMemory()
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		return stats
	}
	before := sample()
	beforeClaims := testAltMemoryClaimSnapshot(budgets)
	wantClaim := kib(512 + 512 + 256 + 128 + 256)
	if beforeClaims.UsedByteCount != ByteCount(count)*wantClaim || beforeClaims.UsedTransportCount != count {
		t.Fatalf("footprint sample lost live API owners: %+v; want %d claims of %d bytes", beforeClaims, count, wantClaim)
	}
	beforeGoroutines := runtime.NumGoroutine()
	started := time.Now()
	ShedMemory()
	// On unchanged source this deliberately remains a no-op control, not a
	// test timeout. Join asynchronous carrier teardown only when pressure
	// actually canceled a connection.
	closed := 0
	for _, client := range clients {
		conn := client.Transport.(*altQuicBoundedTransport).connection(testAltApiHost + ":443")
		if conn == nil || conn.Context().Err() != nil {
			closed++
		}
	}
	if closed != 0 {
		deadline := time.Now().Add(2 * time.Second)
		for testAltMemoryClaimSnapshot(budgets).UsedTransportCount != count-closed && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
	}
	after := sample()
	afterClaims := testAltMemoryClaimSnapshot(budgets)
	if afterClaims.UsedByteCount != ByteCount(count-closed)*wantClaim || afterClaims.UsedTransportCount != count-closed ||
		afterClaims.ReservedByteCount-afterClaims.ReleasedByteCount != afterClaims.UsedByteCount {
		t.Fatalf("footprint sample lost closed API ownership: closed=%d stats=%+v", closed, afterClaims)
	}
	result := struct {
		Count, Closed, BeforeGoroutines, AfterGoroutines                           int
		BeforeClaims, AfterClaims, HeapReclaimed, StackReclaimed, RuntimeReclaimed int64
		ElapsedMicros                                                              int64
	}{count, closed, beforeGoroutines, runtime.NumGoroutine(),
		int64(beforeClaims.UsedByteCount), int64(afterClaims.UsedByteCount),
		int64(before.HeapAlloc) - int64(after.HeapAlloc), int64(before.StackInuse) - int64(after.StackInuse),
		int64(before.Sys-before.HeapReleased) - int64(after.Sys-after.HeapReleased), time.Since(started).Microseconds()}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("API_MEMORY_SAMPLE %s\n", encoded)
}

// Aggregate only the owners retained by this sample, never a freshly allocated
// default budget or an unrelated device's admission root.
func testAltMemoryClaimSnapshot(budgets []*PlatformTransportBudget) PlatformTransportBudgetStats {
	var total PlatformTransportBudgetStats
	for _, budget := range budgets {
		stats := budget.Stats()
		total.UsedByteCount += stats.UsedByteCount
		total.UsedTransportCount += stats.UsedTransportCount
		total.ReservedByteCount += stats.ReservedByteCount
		total.ReleasedByteCount += stats.ReleasedByteCount
	}
	return total
}

func TestClientStrategyMemoryShedFootprintCountsOwnedClaims(t *testing.T) {
	first, second := NewPlatformTransportBudget(10, 1), NewPlatformTransportBudget(20, 1)
	firstClaim := first.register(platformTransportBudgetExtender, 3, true)
	secondClaim := second.register(platformTransportBudgetExtender, 7, true)
	defer firstClaim.Release()
	defer secondClaim.Release()
	if !firstClaim.TryAcquire() || !secondClaim.TryAcquire() {
		t.Fatal("could not acquire independent sample claims")
	}
	budgets := []*PlatformTransportBudget{first, second}
	if got := testAltMemoryClaimSnapshot(budgets); got.UsedByteCount != 10 || got.UsedTransportCount != 2 ||
		got.ReservedByteCount != 10 || got.ReleasedByteCount != 0 {
		t.Fatalf("sample omitted live owners: %+v", got)
	}
	// This was the old sample's false-zero observation, not a legitimate view
	// of either established carrier. Keep it as a distinct negative control.
	if unrelated := testAltMemoryClaimSnapshot([]*PlatformTransportBudget{DefaultPlatformTransportBudget()}); unrelated.UsedByteCount != 0 {
		t.Fatalf("unrelated default unexpectedly observed live sample claims: %+v", unrelated)
	}
	firstClaim.Release()
	if got := testAltMemoryClaimSnapshot(budgets); got.UsedByteCount != 7 || got.UsedTransportCount != 1 ||
		got.ReservedByteCount != 10 || got.ReleasedByteCount != 3 {
		t.Fatalf("partial sample teardown lost the remaining owner: %+v", got)
	}
	secondClaim.Release()
	if got := testAltMemoryClaimSnapshot(budgets); got.UsedByteCount != 0 || got.UsedTransportCount != 0 ||
		got.ReservedByteCount != 10 || got.ReleasedByteCount != 10 {
		t.Fatalf("sample teardown retained a claim: %+v", got)
	}
}
