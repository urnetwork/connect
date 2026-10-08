// Hold the actual H1 owner's native-close boundary while cancellation joins
// its workers. A claim cannot become reusable before those workers finish.
package connect

import (
	"context"
	"runtime/debug"
	"sync"
	"testing"
	"testing/synctest"
)

// One bubble contains both profile iterations: a failed bubble calls FailNow
// on its parent, so two separate bubbles would hide the second RED profile.
// The independent peer and explicit close barrier require no host sockets.
func TestPlatformH1MemoryReservationSurvivesNativeCloseJoin(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	previousTarget := MemoryBudget()
	previousSoftLimit := debug.SetMemoryLimit(32 * 1024 * 1024)
	SetMemoryBudget(mib(32))
	t.Cleanup(func() {
		SetMemoryBudget(previousTarget)
		debug.SetMemoryLimit(previousSoftLimit)
	})
	synctest.Test(t, func(t *testing.T) {
		for _, target := range []ByteCount{mib(20), mib(32)} {
			root := NewPlatformTransportBudgetForMemoryTarget(mib(32))
			budget := newDefaultPlatformTransportBudgetWithParent(target, root)
			settings := DefaultPlatformTransportSettingsWithMemoryTarget(target)
			claimBytes := settings.H1BudgetByteCount
			capacity := target / 4
			if claimBytes != kib(256) || budget.Stats().TotalByteCount != capacity || root.Stats().TotalByteCount != mib(8) ||
				MemoryBudget() != mib(32) || debug.SetMemoryLimit(-1) != 32*1024*1024 {
				t.Fatal("H1 join regression did not select its exact historical/current profile")
			}
			filler := budget.register(platformTransportBudgetExtender, capacity-claimBytes, false)
			defer filler.Release()
			if !filler.TryAcquire() {
				t.Fatal("could not reserve the owner's non-H1 capacity")
			}
			closeEntered := make(chan struct{})
			releaseClose := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseClose) }) }
			defer release()
			fixture := newH1LivenessFixture(t, func(fixtureSettings *PlatformTransportSettings) {
				fixtureSettings.PlatformTransportBudget = budget
				fixtureSettings.H1BudgetByteCount = claimBytes
				fixtureSettings.afterRoutesRemovedForTest = func() {
					close(closeEntered)
					<-releaseClose
				}
			})
			competing := budget.register(platformTransportBudgetH1, claimBytes, true)
			defer competing.Release()
			fixture.cancel()
			<-closeEntered
			synctest.Wait()
			select {
			case <-fixture.client.closed:
				t.Error("native H1 socket closed before the held close boundary")
			default:
			}
			select {
			case <-fixture.transport.Done():
				t.Error("H1 lifecycle completed before native close and worker join")
			default:
			}
			stats := budget.StatsWithRoot()
			if stats.Budget.UsedByteCount != capacity || stats.Root.UsedByteCount != capacity ||
				stats.Budget.UsedTransportCount != 1 || stats.Root.UsedTransportCount != 1 ||
				stats.Budget.ReleasedByteCount != 0 || stats.Root.ReleasedByteCount != 0 {
				t.Errorf("H1 lease released before native close: target=%d claim=%d stats=%+v", target, claimBytes, stats)
			}
			if competing.TryAcquire() {
				t.Errorf("competing H1 acquired capacity while the old native close was held: target=%d", target)
				competing.Release()
				// Recreate only the failed competitor so the old implementation
				// can still finish cleanup and report its deterministic failure.
				competing = budget.register(platformTransportBudgetH1, claimBytes, true)
				defer competing.Release()
			}
			release()
			// Parent cancellation must finish independently. Calling CloseAndWait
			// before this receive would actively close and mask that contract.
			<-fixture.transport.Done()
			select {
			case <-fixture.client.closed:
			default:
				t.Error("joined H1 lifecycle retained the native socket")
			}
			stats = budget.StatsWithRoot()
			if stats.Budget.UsedByteCount != capacity-claimBytes || stats.Root.UsedByteCount != capacity-claimBytes ||
				stats.Budget.UsedTransportCount != 0 || stats.Root.UsedTransportCount != 0 {
				t.Fatalf("H1 join did not release exactly its own claim: %+v", stats)
			}
			if !competing.TryAcquire() {
				t.Fatal("joined H1 graph did not make capacity reusable")
			}
			competing.Release()
			filler.Release()
			for _, current := range []*PlatformTransportBudget{budget, root} {
				stats := current.Stats()
				if stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
					t.Fatalf("H1 close regression retained ownership: %+v", stats)
				}
			}
			if err := fixture.transport.CloseAndWait(context.Background()); err != nil {
				t.Fatal(err)
			}
			fixture.strategy.Close()
			<-fixture.peerDone
			fixture.assertWithdrawn(t)
			t.Logf("H1 native-close regression joined and balanced: target=%d carrier=%d", target, capacity)
		}
	})
}
