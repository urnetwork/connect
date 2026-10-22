// A discovery backoff delays acquisition, not the active configured target.
// These real constructor/resize fixtures hold discovery before identity mint,
// use the production deferral log as a phase edge, and join all owned calls.
package connect

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The production log is emitted after the retry state is committed. It is an
// existing phase boundary, independent of whether the size event changes.
type privateDeferredTargetLogger struct {
	Logger
	deferred chan struct{}
}

func (self *privateDeferredTargetLogger) Infof(_ string, args ...any) {
	if len(args) != 0 && strings.Contains(fmt.Sprint(args[0]), "event=discovery_expand_deferred") {
		select {
		case self.deferred <- struct{}{}:
		default:
		}
	}
}

// Must run in a synctest bubble. Source capacities model the SDK's four-quality,
// one-speed default and its one-per-class H3 capacity cap; explicit profiles
// still enter the same public setter and bypass that default-only cap.
func privateNewDeferredTargetMulti(t *testing.T, limit int, profile *PerformanceProfile) (*RemoteUserNatMultiClient, *privateDeferredTargetLogger, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	log := &privateDeferredTargetLogger{Logger: NewNoopLogger(), deferred: make(chan struct{}, 8)}
	settings := DefaultMultiClientSettings()
	settings.Log = log
	settings.StandingReserve = false
	settings.StrictWindowSizeHardMax = true
	settings.WindowSizeLimit = limit
	settings.DefaultPerformanceProfile = profile
	settings.WindowSizes[WindowTypeQuality] = WindowSizeSettings{
		WindowSizeMin: 4, WindowSizeMax: 4, WindowSizeHardMax: 4,
	}
	settings.WindowSizes[WindowTypeSpeed] = WindowSizeSettings{
		WindowSizeMin: 1, WindowSizeMax: 1, WindowSizeHardMax: 1, FixedWindowSize: 1,
	}
	generator := &readinessHeldGenerator{ctx: ctx}
	multi := NewRemoteUserNatMultiClient(ctx, generator, nil, protocol.ProvideMode_Public, settings)
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			join, stopJoin := context.WithTimeout(context.Background(), 5*time.Second)
			defer stopJoin()
			if err := multi.CloseAndWait(join); err != nil {
				t.Error(err)
			}
			synctest.Wait()
			if generator.entered.Load() == 0 || generator.returned.Load() != generator.entered.Load() {
				t.Errorf("held discovery ownership: entered=%d returned=%d", generator.entered.Load(), generator.returned.Load())
			}
			for kind, window := range multi.windows {
				select {
				case args, open := <-window.clientChannelArgs:
					if open || args != nil {
						t.Errorf("%s retained enumeration/identity after join", kind.RankMode())
					}
				default:
					t.Errorf("%s enumerator did not close its owned channel", kind.RankMode())
				}
			}
		})
	}
	return multi, log, stop
}

// Log wakeups and quiescence prove every required window's committed phase;
// event counts do not stand in for window identity or target-size polling.
func privateAwaitDiscoveryDeferrals(t *testing.T, multi *RemoteUserNatMultiClient, log *privateDeferredTargetLogger, kinds ...WindowType) {
	t.Helper()
	watchdog := time.After(time.Minute)
	for {
		synctest.Wait()
		allDeferred := true
		for _, kind := range kinds {
			window := multi.windows[kind]
			_, deferred := window.discoveryExpandDeferral(time.Now())
			window.stateLock.Lock()
			failures := window.discoveryRetry.failureCount
			clients := len(window.clients)
			window.stateLock.Unlock()
			if clients != 0 || len(window.monitor.ProviderEvents()) != 0 {
				t.Fatalf("%s held discovery manufactured clients=%d providers=%d",
					kind.RankMode(), clients, len(window.monitor.ProviderEvents()))
			}
			allDeferred = allDeferred && deferred && 0 < failures
		}
		if allDeferred {
			return
		}
		select {
		case <-log.deferred:
		case <-watchdog:
			t.Fatal("required windows never reached committed discovery deferral")
		}
	}
}

func privateRequireDeferredTarget(t *testing.T, multi *RemoteUserNatMultiClient, want int, marker string) {
	t.Helper()
	event, providers := multi.Monitor().Events()
	if event.TargetSize != want || event.MinSatisfied || len(providers) != 0 {
		t.Fatalf("%s: target=%d want=%d ready=%t providers=%d reason=%s",
			marker, event.TargetSize, want, event.MinSatisfied, len(providers), event.Reason)
	}
}

// No provider is fabricated by preserving the configured active goal. Each
// class has actually finished a failed pass and is waiting on its own retry.
func TestDiscoveryDeferralPreservesActiveTargetWithoutProviders(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		multi, log, stop := privateNewDeferredTargetMulti(t, 1, nil)
		defer stop()
		privateAwaitDiscoveryDeferrals(t, multi, log, WindowTypeQuality, WindowTypeSpeed)
		privateRequireDeferredTarget(t, multi, 2, "DEFERRED_ACTIVE_TARGET_ERASED")
	})
}

// This is the Connect boundary reached by the failing SDK Speed->nil/H3
// setter. The inactive quality class starts work; the active speed class is
// already deferred. The merged goal must not require overlapping expansions.
func TestDiscoveryDeferralDefaultProfileResetKeepsOtherOwner(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		profile := readinessProfile(WindowTypeSpeed)
		first, firstLog, stopFirst := privateNewDeferredTargetMulti(t, 1, profile)
		defer stopFirst()
		second, secondLog, stopSecond := privateNewDeferredTargetMulti(t, 1, nil)
		defer stopSecond()
		privateAwaitDiscoveryDeferrals(t, first, firstLog, WindowTypeSpeed)
		privateAwaitDiscoveryDeferrals(t, second, secondLog, WindowTypeQuality, WindowTypeSpeed)
		privateAwaitDiscoveryDeferrals(t, first, firstLog, WindowTypeSpeed)
		quality := first.windows[WindowTypeQuality].monitor.WindowExpandEvent()
		if quality.TargetSize != 0 || quality.MinSatisfied || quality.Failed {
			t.Fatal("inactive quality class acquired an admission vote before profile reset")
		}
		secondWindows := map[WindowType]*multiClientWindow{
			WindowTypeQuality: second.windows[WindowTypeQuality],
			WindowTypeSpeed:   second.windows[WindowTypeSpeed],
		}
		secondRetryCounts := make(map[WindowType]int, len(secondWindows))
		for kind, window := range secondWindows {
			window.stateLock.Lock()
			secondRetryCounts[kind] = window.discoveryRetry.failureCount
			window.stateLock.Unlock()
		}
		if err := first.SetPerformanceProfile(nil); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		for _, window := range first.windows {
			window.stateLock.Lock()
			currentProfile := window.performanceProfile
			limit := window.windowSizeLimit
			window.stateLock.Unlock()
			if currentProfile != nil || limit != 1 {
				t.Fatal("public profile reset changed the carrier capacity or retained the explicit profile")
			}
		}
		privateRequireDeferredTarget(t, first, 2, "PROFILE_RESET_TARGET_DEPENDS_ON_EXPANSION_OVERLAP")
		privateRequireDeferredTarget(t, second, 2, "OTHER_OWNER_TARGET_CHANGED")
		for kind, window := range secondWindows {
			if second.windows[kind] != window {
				t.Fatal("another owner's window generation was replaced")
			}
			window.stateLock.Lock()
			failures, limit := window.discoveryRetry.failureCount, window.windowSizeLimit
			window.stateLock.Unlock()
			if failures != secondRetryCounts[kind] || limit != 1 || second.config.Load().performanceProfile != nil {
				t.Fatal("profile reset crossed the independent owner boundary")
			}
		}
		stopFirst()
		if second.ctx.Err() != nil {
			t.Fatal("closing the changed owner canceled its independent sibling")
		}
		privateRequireDeferredTarget(t, second, 2, "OTHER_OWNER_CLOSED_WITH_SIBLING")
	})
}

// Clearing the H3-only cap (H1/default capacity) changes the real default
// target, not the discovery/readiness contract. Deferred empty classes keep it.
func TestDiscoveryDeferralUncappedDefaultTargetStaysUnready(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		multi, log, stop := privateNewDeferredTargetMulti(t, 1, nil)
		defer stop()
		privateAwaitDiscoveryDeferrals(t, multi, log, WindowTypeQuality, WindowTypeSpeed)
		multi.SetWindowSizeLimit(0)
		synctest.Wait()
		privateRequireDeferredTarget(t, multi, 5, "UNCAPPED_DEFERRED_TARGET_ERASED")
		multi.SetWindowSizeLimit(1)
		synctest.Wait()
		privateRequireDeferredTarget(t, multi, 2, "RESTORED_H3_CAP_LOST_TARGET")
	})
}
