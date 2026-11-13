// Exercises one-probe custody independently of bounded evaluation, including
// callback ordering, exact data episodes, shutdown, and late terminal decisions.
package connect

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"
)

// An unanswered admitted probe owns one callback, not one new admission per
// watchdog pass. A late answer keeps its actual timestamp and cannot clear a
// newer owner through a duplicate callback.
func TestBusyProbeOneOwnerSurvivesBoundedWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var callbacks []func(error)
		client := busyProbeTestChannel(t, func(_ time.Duration, callback func(error)) (bool, error) {
			callbacks = append(callbacks, callback)
			return true, nil
		})
		metrics := newReliabilityMetrics()
		client.reliabilityMetricsFunc = func() *reliabilityMetrics { return metrics }
		client.addSend(1440, udpTestPath(4))
		client.stateLock.Lock()
		pendingAt, generation := client.pendingSendTime, client.pendingSendGeneration
		client.stateLock.Unlock()
		started := time.Now()
		verdict := client.busyLivenessProbe(50 * time.Millisecond)
		if verdict.convict || !verdict.unanswered || time.Since(started) != 50*time.Millisecond {
			t.Fatalf("bounded evaluation=%+v elapsed=%s", verdict, time.Since(started))
		}
		client.stateLock.Lock()
		firstOwner := client.busyProbe
		client.stateLock.Unlock()
		for range 8 {
			if got := client.busyLivenessProbe(50 * time.Millisecond); got.convict || !got.unanswered {
				t.Fatalf("pending owner changed its evidence: %+v", got)
			}
		}
		client.stateLock.Lock()
		unchanged := client.busyProbe == firstOwner && firstOwner != nil && !client.busyProbeOutstanding &&
			client.pendingSendTime.Equal(pendingAt) && client.pendingSendGeneration == generation &&
			client.busyProbeAckTime.IsZero() && client.packetStats.sendNackCount == 1 && client.packetStats.sendAckCount == 0
		client.stateLock.Unlock()
		if !unchanged || len(callbacks) != 1 || metrics.busyProbesSent.Load() != 1 || metrics.busyProbesAcquitted.Load() != 0 {
			t.Fatal("bounded timeout accumulated probes, retained a tier penalty, or forged progress")
		}
		time.Sleep(10 * time.Millisecond)
		callbackAt := time.Now()
		callbacks[0](nil)
		time.Sleep(20 * time.Millisecond)
		if got := client.busyLivenessProbe(50 * time.Millisecond); got.convict || got.detail != "liveness probe answered" {
			t.Fatalf("late real answer was not consumed: %+v", got)
		}
		client.stateLock.Lock()
		ackAt, owner := client.busyProbeAckTime, client.busyProbe
		client.stateLock.Unlock()
		if !ackAt.Equal(callbackAt) || owner != nil || len(callbacks) != 1 || metrics.busyProbesAcquitted.Load() != 1 {
			t.Fatal("late answer was restamped, duplicated, or retained after consumption")
		}
		if got := client.busyLivenessProbe(50 * time.Millisecond); got.convict || !got.unanswered {
			t.Fatalf("second bounded evaluation=%+v", got)
		}
		client.stateLock.Lock()
		secondOwner := client.busyProbe
		client.stateLock.Unlock()
		callbacks[0](errors.New("synthetic duplicate old failure"))
		client.stateLock.Lock()
		preserved := secondOwner != nil && secondOwner != firstOwner && client.busyProbe == secondOwner && !secondOwner.completed
		client.stateLock.Unlock()
		if !preserved || len(callbacks) != 2 {
			t.Fatal("duplicate callback stole a newer probe owner")
		}
		client.Cancel()
		callbacks[1](nil)
		client.stateLock.Lock()
		owner, ackAt = client.busyProbe, client.busyProbeAckTime
		client.stateLock.Unlock()
		if owner != nil || !ackAt.Equal(callbackAt) || metrics.busyProbesAcquitted.Load() != 1 {
			t.Fatal("shutdown or its late callback retained an owner or fabricated an ACK")
		}
	})
}

// Reentrant callbacks are ordinary: completing before Send returns must neither
// deadlock nor publish a second owner, including exact local-expiry completion.
func TestBusyProbeSynchronousCallbackHasExactCustody(t *testing.T) {
	for _, callbackErr := range []error{nil, errSendPackExpiredUnwritten, errors.New("synthetic terminal failure")} {
		synctest.Test(t, func(t *testing.T) {
			client := busyProbeTestChannel(t, func(_ time.Duration, callback func(error)) (bool, error) {
				callback(callbackErr)
				callback(errors.New("synthetic duplicate callback"))
				return true, nil
			})
			client.addSend(1000, udpTestPath(4))
			started := time.Now()
			verdict := client.busyLivenessProbe(50 * time.Millisecond)
			client.stateLock.Lock()
			owner, outstanding, ackAt := client.busyProbe, client.busyProbeOutstanding, client.busyProbeAckTime
			pending, acked := client.packetStats.sendNackCount, client.packetStats.sendAckCount
			client.stateLock.Unlock()
			wantConvict := callbackErr != nil && !packetTransferExpiredUnwritten(callbackErr)
			if verdict.convict != wantConvict || owner != nil || outstanding || pending != 1 || acked != 0 || time.Now() != started {
				t.Fatalf("reentrant callback lost custody: error=%v verdict=%+v", callbackErr, verdict)
			}
			if (callbackErr == nil) != ackAt.Equal(started) {
				t.Fatalf("callback=%v produced wrong ACK timestamp=%v", callbackErr, ackAt)
			}
		})
	}
}

// A same-clock ACK and new send are a different episode. The old completion
// must not be allowed to judge it, even when timestamps cannot distinguish them.
func TestBusyProbeCompletedOldEpisodeCannotJudgeNewPendingData(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var callbacks []func(error)
		client := busyProbeTestChannel(t, func(_ time.Duration, callback func(error)) (bool, error) {
			callbacks = append(callbacks, callback)
			return true, nil
		})
		client.addSend(1000, udpTestPath(4))
		if got := client.busyLivenessProbe(50 * time.Millisecond); got.convict || !got.unanswered {
			t.Fatal("first probe did not remain unknown")
		}
		client.stateLock.Lock()
		oldGeneration := client.pendingSendGeneration
		client.stateLock.Unlock()
		client.addSendAck(1000)
		client.addSend(1000, udpTestPath(4))
		client.stateLock.Lock()
		newGeneration := client.pendingSendGeneration
		client.stateLock.Unlock()
		callbacks[0](errors.New("synthetic old terminal failure"))
		if got := client.busyLivenessProbe(50 * time.Millisecond); got.convict || !got.unanswered || oldGeneration == newGeneration {
			t.Fatalf("old terminal completion judged new data: %+v", got)
		}
		if len(callbacks) != 1 {
			t.Fatal("consuming an old completion admitted another probe")
		}
		client.Cancel()
	})
}

// The window waits for every probe. Progress after one terminal result, but
// before another evaluator finishes, must invalidate that destructive result.
func TestBusyProbeWindowCommitsOnlyTheJudgedEpisode(t *testing.T) {
	for _, progress := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			first := busyProbeTestChannel(t, func(_ time.Duration, callback func(error)) (bool, error) {
				callback(errors.New("synthetic terminal probe failure"))
				return true, nil
			})
			var releaseSecond func(error)
			second := busyProbeTestChannel(t, func(_ time.Duration, callback func(error)) (bool, error) {
				releaseSecond = callback
				return true, nil
			})
			first.addSend(1000, udpTestPath(4))
			second.addSend(1000, udpTestPath(4))
			time.Sleep(30 * time.Millisecond)
			sibling := receivingSibling()
			sibling.ctx = context.Background()
			window := busyProbeTestWindow(500*time.Millisecond, first, second, sibling)
			finished := make(chan bool, 1)
			go func() { finished <- window.convictSendStalls(20 * time.Millisecond) }()
			synctest.Wait()
			first.stateLock.Lock()
			returned := first.busyProbe == nil && first.endErr == nil
			first.stateLock.Unlock()
			if releaseSecond == nil || !returned {
				t.Fatal("window did not hold the first returned verdict behind another evaluation")
			}
			if progress {
				first.addSendAck(1000)
				first.addSend(1000, udpTestPath(4))
			}
			releaseSecond(nil)
			synctest.Wait()
			convicted := <-finished
			first.stateLock.Lock()
			err := first.endErr
			first.stateLock.Unlock()
			if convicted == progress || first.IsDone() == progress || (err != nil) == progress || second.IsDone() || sibling.IsDone() {
				t.Fatalf("final window epoch check: progress=%t convicted=%t error=%v", progress, convicted, err)
			}
		})
	}
}

// A late terminal failure remains subject to ordinary window gates; retaining
// a callback is neither a direct callback-to-Cancel path nor an ACK substitute.
func TestBusyProbeLateTerminalResultUsesWindowGates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var callback func(error)
		client := busyProbeTestChannel(t, func(_ time.Duration, next func(error)) (bool, error) {
			callback = next
			return true, nil
		})
		client.addSend(1000, udpTestPath(4))
		time.Sleep(30 * time.Millisecond)
		sibling := receivingSibling()
		sibling.ctx = context.Background()
		window := busyProbeTestWindow(50*time.Millisecond, client, sibling)
		if window.convictSendStalls(20 * time.Millisecond) {
			t.Fatal("unanswered admission was treated as remote failure")
		}
		callback(errors.New("synthetic terminal sequence failure"))
		if client.IsDone() || client.endErr != nil {
			t.Fatal("late callback bypassed the window's conviction gates")
		}
		sibling.addReceiveAck(77)
		if !window.convictSendStalls(20*time.Millisecond) || !client.IsDone() || sibling.IsDone() {
			t.Fatal("unchanged pending episode lost terminal-error authority")
		}
		if !strings.Contains(client.endErr.Error(), "liveness probe failed") {
			t.Fatalf("wrong terminal evidence: %v", client.endErr)
		}
	})
}

// The migrated owner setting keeps the default two-error compatibility policy,
// permits explicit tuning, and defines non-positive as disabled without ACKs.
func TestBusyProbeSendErrorThresholdIsOwnerScoped(t *testing.T) {
	if DefaultMultiClientSettings().BusyProbeSendErrorThreshold != 2 {
		t.Fatal("default compatibility threshold changed")
	}
	for _, threshold := range []int{-1, 0, 1, 3} {
		synctest.Test(t, func(t *testing.T) {
			client := busyProbeTestChannel(t, func(_ time.Duration, _ func(error)) (bool, error) {
				return false, errors.New("synthetic unknown admission error")
			})
			client.settings.BusyProbeSendErrorThreshold = threshold
			for pass := 1; pass <= 4; pass++ {
				got := client.busyLivenessProbe(50 * time.Millisecond)
				if got.convict != (0 < threshold && threshold <= pass) || client.busyProbe != nil || !client.busyProbeAckTime.IsZero() {
					t.Fatalf("threshold=%d pass=%d verdict=%+v", threshold, pass, got)
				}
			}
		})
	}
}

// Repeated evaluation of the same unresolved owner has no new admission or
// allocation. The one owner and one buffered completion have fixed scalar shape.
func TestBusyProbeRetainedEvaluationAllocationBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sends := 0
		client := busyProbeTestChannel(t, func(_ time.Duration, _ func(error)) (bool, error) {
			sends++
			return true, nil
		})
		if got := client.busyLivenessProbe(time.Millisecond); got.convict || !got.unanswered {
			t.Fatal("allocation fixture never retained an actual admitted owner")
		}
		allocs := testing.AllocsPerRun(100, func() {
			if got := client.busyLivenessProbe(time.Millisecond); got.convict || !got.unanswered {
				t.Fatal("retained owner changed its verdict")
			}
		})
		if allocs != 0 || sends != 1 || unsafe.Sizeof(busyProbeAttempt{}) > 32 || unsafe.Sizeof(busyProbeCompletion{}) > 48 {
			t.Fatalf("fixed custody grew: allocs=%g sends=%d owner=%d completion=%d", allocs, sends, unsafe.Sizeof(busyProbeAttempt{}), unsafe.Sizeof(busyProbeCompletion{}))
		}
		t.Logf("busy owner=%d B completion=%d B channel=%d B retained-evaluation allocations=%g", unsafe.Sizeof(busyProbeAttempt{}), unsafe.Sizeof(busyProbeCompletion{}), unsafe.Sizeof(multiClientChannel{}), allocs)
		client.Cancel()
		if client.busyProbe != nil {
			t.Fatal("shutdown retained the callback owner without further traffic")
		}
	})
}
