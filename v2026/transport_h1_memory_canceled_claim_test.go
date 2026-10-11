// Exercise the actual transport owner's failed-admission cleanup with a
// canceled parent. No socket factory may run and no claim may remain pending.
package connect

import (
	"context"
	"errors"
	"net"
	"runtime/debug"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// Available capacity must not let an already-canceled constructor enter a
// carrier mode; the joined run path also releases its pending priority claim.
func TestPlatformH1CanceledClaimJoinsWithoutOpeningGraph(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	previousTarget := MemoryBudget()
	previousSoftLimit := debug.SetMemoryLimit(32 * 1024 * 1024)
	SetMemoryBudget(mib(32))
	t.Cleanup(func() {
		SetMemoryBudget(previousTarget)
		debug.SetMemoryLimit(previousSoftLimit)
	})

	synctest.Test(t, func(t *testing.T) {
		root := NewPlatformTransportBudgetForMemoryTarget(mib(32))
		budget := newDefaultPlatformTransportBudgetWithParent(mib(32), root)
		settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(32))
		settings.PlatformTransportBudget = budget
		if MemoryBudget() != mib(32) || debug.SetMemoryLimit(-1) != 32*1024*1024 ||
			budget.Stats().TotalByteCount != mib(8) || root.Stats().TotalByteCount != mib(8) ||
			settings.H1BudgetByteCount != kib(256) {
			t.Fatal("canceled H1 constructor did not select the exact current profile")
		}

		var socketOpenCount atomic.Int32
		strategySettings := DefaultClientStrategySettings()
		strategySettings.EnableNormal = true
		strategySettings.EnableResilient = false
		strategySettings.ParallelBlockSize = 1
		strategySettings.DialContextSettings = &DialContextSettings{
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				socketOpenCount.Add(1)
				return nil, errors.New("canceled H1 constructor reached socket factory")
			},
		}
		strategy := NewClientStrategy(context.Background(), strategySettings)
		defer strategy.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		routes := NewRouteManager(ctx, "canceled-h1-admission")
		transport := NewPlatformTransportWithTargetMode(ctx, strategy, routes,
			"ws://192.0.2.1:1", &ClientAuth{ByJwt: "synthetic-test-token", InstanceId: NewId()},
			TransportModeH1, settings)
		// Do not call Close or CloseAndWait until the canceled parent alone has
		// completed the actual owner's run, deferred release, and worker join.
		<-transport.Done()
		if transport.h1BudgetReservation == nil {
			t.Error("canceled constructor did not register the actual H1 claim")
		}
		if socketOpenCount.Load() != 0 || transport.IsConnected() || routes.HasActiveTransport() {
			t.Error("canceled constructor opened or published a carrier graph")
		}
		for _, current := range []*PlatformTransportBudget{budget, root} {
			stats := current.Stats()
			if stats.PendingH1Count != 0 || stats.PendingH1ByteCount != 0 ||
				stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 ||
				stats.ReservedByteCount != 0 || stats.ReleasedByteCount != 0 ||
				stats.PendingHandoffCount != 0 || stats.ActiveHandoffCount != 0 ||
				stats.ActiveHandoffByteCount != 0 || stats.ActiveHandoffTransportCount != 0 {
				t.Errorf("canceled constructor retained or acquired a claim: %+v", stats)
			}
		}
		if err := transport.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		strategy.Close()
		t.Log("H1 canceled constructor joined without opening or reserving: target=33554432 carrier=8388608")
	})
}
