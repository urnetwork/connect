// A preimage-compatible window barrier proves that completed probe evidence
// cannot retire data that made real ACK progress before terminal commitment.
package connect

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// Uses only pre-existing APIs/fields. The second probe's explicit callback
// barrier keeps the real window from applying the first returned verdict.
func busyProbeStaleVerdictWitness(t *testing.T, progress bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		firstCalled := false
		first := busyProbeTestChannel(t, func(_ time.Duration, callback func(error)) (bool, error) {
			firstCalled = true
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
		endBefore := first.endErr
		pendingBefore, ackedBefore := first.packetStats.sendNackCount, first.packetStats.sendAckCount
		first.stateLock.Unlock()
		if !firstCalled || releaseSecond == nil || first.busyProbeOutstandingNow() || !second.busyProbeOutstandingNow() || endBefore != nil || pendingBefore != 1 || ackedBefore != 0 {
			t.Fatal("first completed probe was not held behind the second evaluator")
		}
		select {
		case <-finished:
			t.Fatal("window bypassed the second probe's callback barrier")
		default:
		}
		if progress {
			first.addSendAck(1000)
			first.addSend(1000, udpTestPath(4))
		}
		releaseSecond(nil)
		synctest.Wait()
		convicted := <-finished
		first.stateLock.Lock()
		err, pending, acked := first.endErr, first.packetStats.sendNackCount, first.packetStats.sendAckCount
		first.stateLock.Unlock()
		if second.IsDone() || sibling.IsDone() || pending != 1 || acked != map[bool]int{false: 0, true: 1}[progress] {
			t.Fatal("window barrier changed unrelated owners or data accounting")
		}
		if progress {
			if convicted || first.IsDone() || err != nil {
				t.Fatalf("BUSY_PROBE_STALE_VERDICT_RETIRED_NEW_EPISODE convicted=%t canceled=%t pending=%d acked=%d error=%v", convicted, first.IsDone(), pending, acked, err)
			}
		} else if !convicted || !first.IsDone() || err == nil {
			t.Fatal("unchanged terminal-error evidence lost its original conviction authority")
		}
	})
}

// OLD must fail only after the real data ACK and new pending send are counted.
func TestBusyProbeCompletedVerdictCannotRetireNewEpisode(t *testing.T) {
	busyProbeStaleVerdictWitness(t, true)
}

// Preserves the existing nonnil terminal-callback policy for unchanged data.
func TestBusyProbeUnchangedVerdictStillRetiresOldEpisode(t *testing.T) {
	busyProbeStaleVerdictWitness(t, false)
}
