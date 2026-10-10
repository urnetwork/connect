// Auto registers H1 and H3 together. Joining a constructor canceled before
// H1 admission must retire both claims, including the H3 runner never started.
package connect

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// An already-canceled owner cannot leave an optional H3 claim behind Done.
func TestPlatformAutoCanceledBeforeH1AdmissionReleasesPendingH3(t *testing.T) {
	testPlatformAutoCanceledH1AdmissionReleasesPendingH3(t, false)
}

// Cancellation of a blocked required H1 admission must also release the
// optional H3 claim whose mode group has not yet started.
func TestPlatformAutoCanceledWhileH1AdmissionBlockedReleasesPendingH3(t *testing.T) {
	testPlatformAutoCanceledH1AdmissionReleasesPendingH3(t, true)
}

// A synthetic capacity barrier forces both constructor exit orders without
// sockets or timing thresholds. A later background claimant tests admission,
// not merely the private reservation map: a ghost foreground claim denies it.
func testPlatformAutoCanceledH1AdmissionReleasesPendingH3(t *testing.T, blockH1 bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		root := NewPlatformTransportBudget(mib(8), 16)
		budget := newDefaultPlatformTransportBudgetWithParent(mib(32), root)
		settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(32))
		settings.PlatformTransportBudget = budget
		settings.PlatformTransportBudgetPriority = PlatformTransportBudgetPriorityForeground
		settings.ModePreferences = map[TransportMode]int{
			TransportModeH1: 1,
			TransportModeH3: 2,
		}
		var filler *platformTransportBudgetReservation
		if blockH1 {
			filler = budget.register(platformTransportBudgetExtender, mib(8), false)
			defer filler.Release()
			if !filler.TryAcquire() {
				t.Fatal("could not establish the synthetic H1 admission barrier")
			}
		}

		var socketOpenCount atomic.Int32
		strategySettings := DefaultClientStrategySettings()
		strategySettings.EnableNormal = true
		strategySettings.EnableResilient = false
		strategySettings.ParallelBlockSize = 1
		strategySettings.DialContextSettings = &DialContextSettings{
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				socketOpenCount.Add(1)
				return nil, errors.New("synthetic canceled Auto reached socket factory")
			},
		}
		strategy := NewClientStrategy(context.Background(), strategySettings)
		defer strategy.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if !blockH1 {
			cancel()
		}
		routes := NewRouteManager(ctx, "synthetic-auto-canceled-admission")
		transport := NewPlatformTransportWithTargetMode(
			ctx, strategy, routes, "ws://192.0.2.1:1",
			&ClientAuth{ByJwt: "synthetic-test-token", InstanceId: NewId()},
			TransportModeAuto, settings,
		)
		// The broken owner must not leave test-owned state behind after RED.
		defer transport.h3BudgetReservation.Release()
		if transport.h1BudgetReservation == nil || transport.h3BudgetReservation == nil {
			t.Fatal("Auto did not register both carrier claims")
		}
		if blockH1 {
			synctest.Wait()
			if !transport.h1BudgetReservation.IsWaiting() {
				t.Fatal("synthetic full budget did not hold required H1 admission")
			}
			cancel()
		}
		// Parent cancellation alone must complete the real owner before an
		// explicit CloseAndWait can hide which path performed the release.
		<-transport.Done()
		filler.Release()
		if socketOpenCount.Load() != 0 || transport.IsConnected() || routes.HasActiveTransport() {
			t.Error("canceled Auto opened or published a carrier graph")
		}
		for _, current := range []*PlatformTransportBudget{budget, root} {
			stats := current.Stats()
			if stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 ||
				stats.PendingH1ByteCount != 0 || stats.PendingH1Count != 0 ||
				stats.ReservedByteCount != stats.ReleasedByteCount {
				t.Errorf("joined Auto retained accounted capacity: %+v", stats)
			}
			current.root.mutex.Lock()
			remaining := len(current.reservations)
			current.root.mutex.Unlock()
			if remaining != 0 {
				t.Errorf("joined Auto retained %d inactive carrier claims", remaining)
			}
		}

		background := budget.registerWithPriority(
			platformTransportBudgetH3Auto,
			settings.H3BudgetByteCount,
			true,
			PlatformTransportBudgetPriorityBackground,
		)
		defer background.Release()
		if !background.TryAcquire() {
			t.Errorf("ghost foreground H3 denied a live background carrier with zero used capacity: %+v", budget.Stats())
		}
		background.Release()
		if err := transport.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	})
}
