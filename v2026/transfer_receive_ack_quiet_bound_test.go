// Sub-nanosecond fractions must round up to preserve the configured turn cap.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// Keep supplying one paid data quantum after every response. Every virtual
// tick is explicit, so floor division cannot hide extra head turns in timer
// scheduling slack.
func TestReceiveSequenceBurstTailSmallIntervalTurnBound(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, timeout := range []time.Duration{19 * time.Nanosecond, 29 * time.Nanosecond} {
		synctest.Test(t, func(t *testing.T) {
			sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
				settings.AckCompressTimeout = timeout
				settings.AckGapWakeSelectiveCount = 0
			})
			sequence.prime(t)
			start := time.Now()
			number, quietHeads := uint64(1), 0
			deliverBurstTailTestHead(t, sequence, number, 100*ackResponseEntryMaxByteCount)
			for time.Since(start) < timeout {
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				if len(sequence.route) == 0 {
					continue
				}
				readBurstTailTestAck(t, sequence)
				if time.Since(start) < timeout {
					quietHeads++
				}
				number++
				deliverBurstTailTestHead(t, sequence, number, 100*ackResponseEntryMaxByteCount)
			}
			if quietHeads > 10 {
				t.Fatalf("compression %s allowed %d extra head turns before its full deadline; maximum is ten", timeout, quietHeads)
			}
		})
	}
}
