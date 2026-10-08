// One exact receiver-timing observation remains consumed for the lifetime of
// its message, including a selective-ack hold followed by another write.
package connect

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// Retrying a retained selective acknowledgement must not reopen the legacy
// tag sampler for an original reply that the coalescer already measured.
func TestSenderReceiverTimingObservedMessageStaysConsumedAfterRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, kind := range []string{"before-retry", "confirmed-retry", "failed-retry", "unreliable-retry"} {
			sequence, item := newReceiverTimingSendTestSequence()
			sequence.finishReceiverRttWrite(item, transferWriteDisposition{transportType: TransportTypeH1, reliable: true}, nil)
			time.Sleep(10 * time.Millisecond)
			original := receiveAckMessage{
				messageId:              item.messageId,
				tag:                    sequenceTag{sendTime: uint64(item.sendTime.UnixMilli()), set: true},
				receivedAtNanos:        sequence.client.feedbackArrivalNanos(time.Now()),
				selective:              true,
				receiverAckDelaySet:    true,
				receiverAckDelayMicros: 3000,
			}
			sequence.observeReceiverAckRtt(original)
			before := sequence.rttWindow.Estimate()
			if before.SampleCount != 1 || before.Mean != 10*time.Millisecond {
				t.Fatalf("%s: exact original reply was not measured: %+v", kind, before)
			}
			item.selectiveAcked = true
			item.deliveryObserved = true
			sequence.invalidateReceiverRttWrite(item)
			if kind != "before-retry" {
				item.sendCount++
				sequence.beginReceiverRttWrite(item, true)
				disposition := transferWriteDisposition{transportType: TransportTypeH1, reliable: true}
				var writeErr error
				if kind == "failed-retry" {
					writeErr = errors.New("synthetic retry failure")
				} else if kind == "unreliable-retry" {
					disposition = transferWriteDisposition{transportType: TransportTypeH3, unreliable: true}
				}
				sequence.finishReceiverRttWrite(item, disposition, writeErr)
			}
			time.Sleep(10 * time.Millisecond)
			sequence.observeReceiverAckRtt(original)
			// The optional field may be absent on another reply. Exercise the
			// existing fallback with the same original echoed tag.
			sequence.observeAckRtt(item, original.tag)
			if after := sequence.rttWindow.Estimate(); after.SampleCount != before.SampleCount || after.Mean != before.Mean {
				t.Errorf("%s: retry reopened an already consumed RTT observation: before=%+v after=%+v", kind, before, after)
			}
		}
	})
}
