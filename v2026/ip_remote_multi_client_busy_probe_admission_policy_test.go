// Separates known local refusals and owner cancellation from the retained
// compatibility policy for nonnil send errors. No callback fabricates an ACK.
package connect

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// A local refusal neither adds an error strike nor heals an existing unknown
// send error. Only the existing real-progress paths may clear that episode.
func TestBusyProbeLocalAdmissionRefusalPreservesErrorEpisode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		attempt := 0
		client := busyProbeTestChannel(t, func(_ time.Duration, _ func(error)) (bool, error) {
			attempt++
			if attempt == 1 || attempt == 6 {
				return false, errors.New("synthetic unknown send error")
			}
			return false, nil
		})
		stallPast(client, 20*time.Millisecond)
		client.stateLock.Lock()
		pendingAt := client.pendingSendTime
		client.stateLock.Unlock()
		sibling := receivingSibling()
		sibling.ctx = context.Background()
		window := busyProbeTestWindow(40*time.Millisecond, client, sibling)
		for pass := range 5 {
			if window.convictSendStalls(20 * time.Millisecond) {
				t.Fatalf("local refusal promoted a compatibility error at pass %d", pass)
			}
			client.stateLock.Lock()
			failures, ack, clock := client.busyProbeSendFailures, client.busyProbeAckTime, client.pendingSendTime
			client.stateLock.Unlock()
			if failures != 1 || !ack.IsZero() || !clock.Equal(pendingAt) {
				t.Fatalf("local refusal changed the existing error episode at pass %d", pass)
			}
		}
		if !window.convictSendStalls(20*time.Millisecond) || !client.IsDone() || sibling.IsDone() {
			t.Fatal("second nonnil error lost its separate compatibility policy or canceled the sibling")
		}
		client.stateLock.Lock()
		endErr := client.endErr
		client.stateLock.Unlock()
		if endErr == nil || !strings.Contains(endErr.Error(), "liveness probe unsendable 2x") {
			t.Fatalf("unexpected compatibility verdict: %v", endErr)
		}
	})
}

// A synchronous return can race owner cancellation just as an asynchronous
// callback can. Neither nil nor nonnil returns may judge the closing owner.
func TestBusyProbeCanceledAdmissionDoesNotJudgePeer(t *testing.T) {
	for _, success := range []bool{false, true} {
		for _, withError := range []bool{false, true} {
			synctest.Test(t, func(t *testing.T) {
				var cancel context.CancelFunc
				client := busyProbeTestChannel(t, func(_ time.Duration, _ func(error)) (bool, error) {
					cancel()
					if withError {
						return success, errors.New("synthetic canceled send")
					}
					return success, nil
				})
				cancel = client.cancel
				metrics := newReliabilityMetrics()
				client.reliabilityMetricsFunc = func() *reliabilityMetrics { return metrics }
				stallPast(client, 20*time.Millisecond)
				client.stateLock.Lock()
				pendingAt := client.pendingSendTime
				client.stateLock.Unlock()
				sibling := receivingSibling()
				sibling.ctx = context.Background()
				window := busyProbeTestWindow(40*time.Millisecond, client, sibling)
				log := &busyExpiryTestLog{Logger: NewNoopLogger()}
				window.log = log
				if window.convictSendStalls(20 * time.Millisecond) {
					t.Fatalf("canceled admission judged the peer: success=%t error=%t", success, withError)
				}
				client.stateLock.Lock()
				endErr, clock, ack := client.endErr, client.pendingSendTime, client.busyProbeAckTime
				pending, pendingBytes, acked := client.packetStats.sendNackCount, client.packetStats.sendNackByteCount, client.packetStats.sendAckCount
				failures, outstanding := client.busyProbeSendFailures, client.busyProbeOutstanding
				client.stateLock.Unlock()
				if client.ctx.Err() != context.Canceled || endErr != nil || !clock.Equal(pendingAt) || !ack.IsZero() || pending != 1 || pendingBytes != 1440 || acked != 0 || failures != 0 || outstanding {
					t.Fatalf("canceled admission changed peer evidence: success=%t error=%t", success, withError)
				}
				if sibling.IsDone() || log.snapshot() != "" || metrics.busyProbesSent.Load() != 0 || metrics.busyProbesAcquitted.Load() != 0 {
					t.Fatalf("canceled admission changed sibling, narration or probe metrics: success=%t error=%t", success, withError)
				}
			})
		}
	}
}
