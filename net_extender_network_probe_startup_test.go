package connect

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// The client owns both loops, but every external operation is a seam and the
// refresh loop stays parked after its first attempt unless explicitly woken.
func newTestExtenderStartupProbeClient(
	t *testing.T,
	clock *testClock,
	directory *ExtenderDirectory,
	probes *testProbeLog,
	configure func(*ExtenderNetworkClientSettings),
) *ExtenderNetworkClient {
	t.Helper()
	settings := DefaultExtenderNetworkClientSettings()
	settings.Log = NewNoopLogger()
	settings.Now = clock.Now
	settings.ExtenderDnsName = "probe-startup.example"
	settings.LowWaterCount = 0
	settings.PassAfter = func(time.Duration) <-chan time.Time { return nil }
	settings.ProbeWindowCount = 2
	settings.ProbeMaxCandidateCount = 8
	settings.ProbeCountPerExtender = 1
	settings.ProbeCloseFactor = 2
	settings.ProbeCloseFloor = 10 * time.Millisecond
	settings.IpVersionSupported = func(ipVersion int) bool { return ipVersion == 4 }
	settings.Hello = func(context.Context) (*ExtenderHelloResult, error) { return nil, nil }
	settings.Hint = func(context.Context) (string, error) { return "eu", nil }
	settings.ResolveDns = func(context.Context, string) ([]netip.Addr, error) { return nil, nil }
	settings.ResolveDnsTxt = func(context.Context, string) ([]string, error) { return nil, nil }
	settings.Probe = func(ctx context.Context, candidate *ExtenderCandidate, attestor *ExtenderProbeAttestor) (time.Duration, ExtenderPingOutcome, error) {
		clock.advance(time.Second)
		return probes.probe(ctx, candidate, attestor)
	}
	if configure != nil {
		configure(settings)
	}
	strategy := newTestDeadDialStrategy(t, t.Context())
	client := NewExtenderNetworkClient(t.Context(), strategy, directory, settings)
	t.Cleanup(client.Close)
	return client
}

// A hint and an attestor can wake the probe loop while bootstrap TXT is still
// blocked. Stored NA records must not fill the probe window before the hinted
// EU records arrive, even though those stored records are already usable.
func TestExtenderNetworkClientInitialProbeWaitsForBootstrap(t *testing.T) {
	// Feed dials use the process-owned message-pool diagnostics goroutine.
	// Initialize it outside the bubble so its lifetime is not this test's.
	MessagePoolReturn(MessagePoolGet(1))
	for _, attesting := range []bool{false, true} {
		name := "ranking"
		if attesting {
			name = "attesting"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				clock := newTestClock()
				directory, rootPrivateKey := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
					settings.HoldTimeout = 0
					settings.MaxHoldTimeout = 0
				})
				txts := []string{
					testExtenderDnsRecordTxtWithContinent(t, rootPrivateKey, clock, "NA", "192.0.2.20"),
					testExtenderDnsRecordTxtWithContinent(t, rootPrivateKey, clock, "NA", "192.0.2.21"),
					testExtenderDnsRecordTxtWithContinent(t, rootPrivateKey, clock, "EU", "192.0.2.10"),
					testExtenderDnsRecordTxtWithContinent(t, rootPrivateKey, clock, "EU", "192.0.2.11"),
				}
				for _, txt := range txts[:2] {
					message, err := DecodeExtenderDnsRecord(txt)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := directory.ApplySource(message, ExtenderSourceFeed); err != nil {
						t.Fatal(err)
					}
				}
				probes := newTestProbeLog(map[string]time.Duration{
					"192.0.2.10": 20 * time.Millisecond,
					"192.0.2.11": 25 * time.Millisecond,
					"192.0.2.20": 120 * time.Millisecond,
					"192.0.2.21": 130 * time.Millisecond,
				})
				bootstrapEntered := make(chan struct{})
				releaseBootstrap := make(chan struct{})
				client := newTestExtenderStartupProbeClient(t, clock, directory, probes, func(settings *ExtenderNetworkClientSettings) {
					settings.ResolveDnsTxt = func(ctx context.Context, _ string) ([]string, error) {
						close(bootstrapEntered)
						select {
						case <-releaseBootstrap:
							return txts, nil
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}
				})
				<-bootstrapEntered
				if attesting {
					attestor, _ := newTestProbeAttestor(t)
					client.SetProbeAttestor(attestor, nil)
				}
				synctest.Wait()
				if got := probes.count(); got != 0 {
					t.Fatalf("%d probes ran before bootstrap TXT completed", got)
				}
				if directory.ContinentHint() != "EU" || client.Status().ContinentHint != "EU" {
					t.Fatal("the operator hint was not applied while bootstrap was held")
				}
				if !client.Status().LastProbeTime.IsZero() {
					t.Fatal("a probe pass completed before bootstrap readiness")
				}

				close(releaseBootstrap)
				synctest.Wait()
				probes.stateLock.Lock()
				ips := slices.Clone(probes.ips)
				attested := slices.Clone(probes.attested)
				probes.stateLock.Unlock()
				slices.Sort(ips)
				if !slices.Equal(ips, []string{"192.0.2.10", "192.0.2.11"}) {
					t.Fatalf("probes = %v, want exactly the two hinted EU extenders", ips)
				}
				for _, got := range attested {
					if got != attesting {
						t.Fatalf("probe attestor = %t, want %t", got, attesting)
					}
				}
				if !client.Status().LastProbeTime.Equal(clock.Now()) {
					t.Fatal("the released probe pass did not publish completion")
				}
			})
		})
	}
}

// Close must cancel the probe readiness wait and join discovery whether it is
// blocked in foreground TXT or in the owned manual DNS worker. Disabled probes
// still have a joinable loop and must never run on either side of readiness.
func TestExtenderNetworkClientInitialProbeCloseJoinsBeforeReady(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	for _, phase := range []string{"bootstrap", "manual"} {
		for _, enabled := range []bool{false, true} {
			name := phase + "/disabled"
			if enabled {
				name = phase + "/enabled"
			}
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					clock := newTestClock()
					directory, _ := newTestExtenderDirectory(t, clock, nil)
					probes := newTestProbeLog(nil)
					entered := make(chan struct{})
					canceled := make(chan struct{})
					releaseResolver := make(chan struct{})
					returned := make(chan struct{})
					release := sync.OnceFunc(func() { close(releaseResolver) })
					defer release()
					holdResolver := func(ctx context.Context) {
						close(entered)
						defer close(returned)
						<-ctx.Done()
						close(canceled)
						<-releaseResolver
					}
					client := newTestExtenderStartupProbeClient(t, clock, directory, probes, func(settings *ExtenderNetworkClientSettings) {
						if !enabled {
							settings.ProbeWindowCount = 0
						}
						if phase == "bootstrap" {
							// A cached candidate makes an early probe observable.
							directory.AddManual(netip.MustParseAddr("192.0.2.20"))
							settings.ResolveDnsTxt = func(ctx context.Context, _ string) ([]string, error) {
								holdResolver(ctx)
								return nil, ctx.Err()
							}
						} else {
							settings.ExtenderDnsName = ""
							settings.ManualHosts = []string{"manual-startup.example"}
							settings.ResolveDns = func(ctx context.Context, _ string) ([]netip.Addr, error) {
								holdResolver(ctx)
								return nil, ctx.Err()
							}
						}
					})
					<-entered
					synctest.Wait()
					if probes.count() != 0 {
						t.Fatal("a probe ran before discovery was ready")
					}
					closeDone := make(chan struct{})
					go func() {
						client.Close()
						close(closeDone)
					}()
					<-canceled
					synctest.Wait()
					select {
					case <-closeDone:
						t.Fatal("Close returned while the canceled resolver was still held")
					default:
					}
					select {
					case <-client.probeDone:
					default:
						t.Fatal("the probe readiness wait ignored cancellation")
					}
					release()
					<-closeDone
					for name, done := range map[string]<-chan struct{}{
						"resolver": returned, "refresh loop": client.done, "probe loop": client.probeDone,
					} {
						select {
						case <-done:
						default:
							t.Fatalf("Close returned before joining the %s", name)
						}
					}
					if probes.count() != 0 || !client.Status().LastProbeTime.IsZero() {
						t.Fatal("cancellation released a probe before readiness")
					}
				})
			})
		}
	}
}

// Readiness records a completed discovery attempt, not a successful answer.
// An empty/failed/disabled bootstrap must leave later manual publication able
// to wake probes without waiting for the next refresh interval.
func TestExtenderNetworkClientInitialProbeReadinessWithoutBootstrapAnswers(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	for _, mode := range []string{"disabled", "empty", "failed", "disabled_probes"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				clock := newTestClock()
				directory, _ := newTestExtenderDirectory(t, clock, func(settings *ExtenderDirectorySettings) {
					settings.HoldTimeout = 0
					settings.MaxHoldTimeout = 0
				})
				probes := newTestProbeLog(map[string]time.Duration{"192.0.2.10": 20 * time.Millisecond})
				client := newTestExtenderStartupProbeClient(t, clock, directory, probes, func(settings *ExtenderNetworkClientSettings) {
					settings.ProbeWindowCount = 1
					if mode == "disabled_probes" {
						settings.ProbeWindowCount = 0
					}
					if mode == "disabled" {
						settings.ExtenderDnsName = ""
					}
					if mode == "failed" {
						settings.ResolveDnsTxt = func(context.Context, string) ([]string, error) {
							return nil, errors.New("bootstrap TXT failed")
						}
						settings.ResolveDns = func(context.Context, string) ([]netip.Addr, error) {
							return nil, errors.New("bootstrap addresses failed")
						}
					}
				})
				synctest.Wait()
				if !client.Status().InitialAttemptDone || probes.count() != 0 {
					t.Fatal("bootstrap without answers did not complete without probes")
				}
				client.SetManualHosts([]string{"192.0.2.10"})
				synctest.Wait()
				if mode == "disabled_probes" {
					if probes.count() != 0 || !client.Status().LastProbeTime.IsZero() {
						t.Fatal("manual publication ran disabled probes")
					}
					return
				}
				if probes.count() != 1 || !client.Status().LastProbeTime.Equal(clock.Now()) {
					t.Fatal("manual publication after bootstrap failed to wake and complete one probe")
				}
			})
		})
	}
}
