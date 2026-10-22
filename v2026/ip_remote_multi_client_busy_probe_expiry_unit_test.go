// Pins health judgments for callbacks that distinguish local refusal from
// remote failure. The composed fixture separately exercises real Transfer.
package connect

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"
)

// Exact pre-wire proof must preserve the shared channel without inventing
// peer progress, even when a live receiving sibling permits a conviction.
func TestBusyProbeLocalQueuedExpiryDoesNotConvict(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stallTimeout := 20 * time.Millisecond
		client := busyProbeTestChannel(t, func(_ time.Duration, callback func(error)) (bool, error) {
			callback(fmt.Errorf("synthetic probe: %w", errSendPackExpiredUnwritten))
			return true, nil
		})
		stallPast(client, stallTimeout)
		client.stateLock.Lock()
		pendingBefore := client.pendingSendTime
		countBefore := client.packetStats.sendNackCount
		client.stateLock.Unlock()
		sibling := receivingSibling()
		sibling.ctx = context.Background()
		window := busyProbeTestWindow(500*time.Millisecond, client, sibling)
		for range 2 {
			if window.convictSendStalls(stallTimeout) {
				t.Fatal("local queued expiry convicted an unwritten probe")
			}
		}
		client.stateLock.Lock()
		endErr := client.endErr
		ackTime := client.busyProbeAckTime
		pendingAfter := client.pendingSendTime
		countAfter := client.packetStats.sendNackCount
		outstanding := client.busyProbeOutstanding
		client.stateLock.Unlock()
		if endErr != nil || client.IsDone() || sibling.IsDone() {
			t.Fatal("local expiry retired shared work")
		}
		if !ackTime.IsZero() || !pendingBefore.Equal(pendingAfter) || countBefore != countAfter || outstanding {
			t.Fatal("local expiry forged progress or changed pending ownership")
		}
		if !client.sendStalled(stallTimeout) {
			t.Fatal("local expiry refreshed the remote-progress clock")
		}
	})
}

// One proven local leaf cannot hide an ambiguous or remote sibling failure.
func TestBusyProbeMixedQueuedExpiryStillConvicts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stallTimeout := 20 * time.Millisecond
		client := busyProbeTestChannel(t, func(_ time.Duration, callback func(error)) (bool, error) {
			callback(errors.Join(errSendPackExpiredUnwritten, errors.New("synthetic remote sequence failure")))
			return true, nil
		})
		stallPast(client, stallTimeout)
		window := busyProbeTestWindow(500*time.Millisecond, client, receivingSibling())
		if !window.convictSendStalls(stallTimeout) {
			t.Fatal("local leaf masked a distinct failed probe")
		}
	})
}
