//go:build !js && !windows

package connect

// The storm test of RR-8 (see ip_remote_multi_client_rerace_test.go) also
// bounds the cpu the client spends idle after the storm. It reads this
// process's cpu time through syscall.Getrusage, which js/wasm and windows do
// not have, so it builds only where net_extender_scale_cpu_test.go does.

import (
	"runtime"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

// reraceProcessCpu is this process's user plus system cpu time.
func reraceProcessCpu(t *testing.T) time.Duration {
	t.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatal(err)
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// RR-8. When every exit fails a destination there is no storm: each SYN the
// app sends reaches each exit at most once and is re-raced at most once, the
// exits see no SYN the app did not send, nothing reaches the app, and the
// connect fails at the app's own timeout. Many such flows at once leave no
// goroutines behind, and once the app gives up nothing more is sent for them
// and the client idles.
func TestDialFailureEveryExitFailsReracesOncePerRetransmit(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: reraceAlways(reraceSignal)},
			{name: "b", tier: 1, answer: reraceAlways(reraceSignal)},
			{name: "c", tier: 2, answer: reraceAlways(reraceSignal)},
		}, nil)
		exits := []*reraceExit{harness.exit("a"), harness.exit("b"), harness.exit("c")}
		// the client's own idle cost over ten minutes, before any flow
		cpuBefore := reraceProcessCpu(t)
		harness.sleep(10 * time.Minute)
		baselineIdleCpu := reraceProcessCpu(t) - cpuBefore
		baselineGoroutines := runtime.NumGoroutine()

		const flowCount = 32
		paths := []*IpPath{}
		for i := range flowCount {
			paths = append(paths, reraceFlow(4, 1+i%4, 44000+i))
		}
		synCounts := make([]int, flowCount)
		completed := make([]bool, flowCount)
		harness.concurrent = true
		var flows sync.WaitGroup
		for i, path := range paths {
			flows.Add(1)
			go func() {
				defer flows.Done()
				synCounts[i], _, completed[i] = harness.connect(path)
			}()
		}
		flows.Wait()
		harness.concurrent = false
		end := time.Now()

		appSyns, exitSyns := 0, 0
		for i, path := range paths {
			if completed[i] {
				t.Errorf("flow %d completed although every exit fails", i)
			}
			if synCounts[i] != len(reraceSynSchedule) {
				t.Errorf("flow %d: the app sent %d SYNs, want its whole schedule", i, synCounts[i])
			}
			appSyns += synCounts[i]
			if got := len(harness.delivered(path)); got != 0 {
				t.Errorf("flow %d: the app received %d packets, want none before its own timeout", i, got)
			}
			sent := harness.sentSyns(path)
			for _, exit := range exits {
				perSyn := make([]int, len(sent))
				for _, packet := range exit.syns(path) {
					exitSyns += 1
					// the app SYN this one forwards: the last sent before it
					j := len(sent) - 1
					for 0 <= j && packet.at.Before(sent[j]) {
						j -= 1
					}
					if j < 0 || reraceSendTimeout < packet.at.Sub(sent[j]) {
						t.Errorf("flow %d: %s received a SYN at +%s that the app did not send", i, exit.name, packet.at.Sub(harness.start))
						continue
					}
					perSyn[j] += 1
					if 1 < perSyn[j] {
						t.Errorf("flow %d: %s received app SYN %d %d times", i, exit.name, j, perSyn[j])
					}
				}
			}
		}
		metrics := harness.metrics()
		if uint64(appSyns) < metrics.FlowsReraced {
			t.Errorf("FlowsReraced = %d for %d app SYNs, want at most one re-race each", metrics.FlowsReraced, appSyns)
		}
		if metrics.DialFailuresIntercepted != uint64(exitSyns) {
			t.Errorf("DialFailuresIntercepted = %d, want one per failed dial (%d)", metrics.DialFailuresIntercepted, exitSyns)
		}

		// the app has given up: ten idle minutes send nothing more for the
		// flows, and the flows' goroutines are gone
		cpuBefore = reraceProcessCpu(t)
		harness.sleep(10 * time.Minute)
		idleCpu := reraceProcessCpu(t) - cpuBefore
		for i, path := range paths {
			for _, exit := range exits {
				for _, packet := range exit.received(path) {
					if packet.at.After(end) && !packet.path.Rst {
						t.Errorf("flow %d: %s received %+v after the app gave up", i, exit.name, packet.path)
					}
				}
			}
		}
		goroutines := runtime.NumGoroutine()
		t.Logf("%d app SYNs, %d exit SYNs, %d re-races; goroutines %d before, %d after; cpu over ten idle minutes %s before, %s after",
			appSyns, exitSyns, metrics.FlowsReraced, baselineGoroutines, goroutines, baselineIdleCpu, idleCpu)
		if baselineGoroutines+16 < goroutines {
			t.Errorf("%d goroutines after the storm, want at most the %d before it plus 16", goroutines, baselineGoroutines)
		}
		// idle after the storm costs what idle cost before it; a loop over the
		// dead flows, however slow, would run for all ten minutes
		if 2*baselineIdleCpu+500*time.Millisecond < idleCpu {
			t.Errorf("ten idle minutes after the storm cost %s of cpu, against %s before it", idleCpu, baselineIdleCpu)
		}
	})
}
