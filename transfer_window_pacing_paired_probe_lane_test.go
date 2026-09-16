// A controlled RTT probe still obeys explicit delivery evidence on its lane.
package connect

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The worker must recover a proved hole at its original due time, even when
// the item also carries the service's protected RTT probe.
func TestWindowPacingPairedProbeRecoveryPreservesProvenLaneLoss(t *testing.T) {
	testWindowPacingPairedProbeRecoveryFixture(t, func(settings *SendBufferSettings) {
		settings.ReliableLaneProvenRecovery = true
	}, func(sequence *SendSequence) {
		preparedGeneration := sequence.laneAckGeneration
		fired := false
		sequence.sendBuffer.beforeDueResendForTest = func(sendSequenceId, uint64) {
			if fired {
				return
			}
			fired = true
			t.Logf("lane proof generation: prepare=%d due=%d", preparedGeneration, sequence.laneAckGeneration)
			item := sequence.resendQueue.PeekFirst()
			if item.carrierRoute == nil || item.recoveryKind != sendRecoveryNone {
				t.Fatal("fixture must retain an ordinary due timer on a physical lane")
			}
			sequence.observeLaneAck(&sendItem{
				carrierRoute: item.carrierRoute,
				transferItem: transferItem{sequenceNumber: item.sequenceNumber + 1},
			}, time.Now())
			if verdict := sequence.laneTimerVerdictFor(item); verdict != laneTimerEndpointDrop {
				t.Fatalf("later same-lane delivery did not prove the hole: %d", verdict)
			}
		}
	}, func(t *testing.T, sequence *SendSequence, client *Client, route Route, pack *protocol.Pack, at time.Time, _ <-chan error) {
		// The controlled pause ends 100 ms after local offer; it does not
		// consume any part of the configured 300 ms physical RTO.
		time.Sleep(time.Until(at.Add(300*time.Millisecond - time.Nanosecond)))
		synctest.Wait()
		select {
		case bytes := <-route:
			MessagePoolReturn(bytes)
			t.Fatal("recovery fired before its unchanged 300ms physical interval")
		default:
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		select {
		case bytes := <-route:
			retry := decodeSendPackLifecycleWirePack(t, bytes)
			MessagePoolReturn(bytes)
			retryId, _ := IdFromBytes(retry.MessageId)
			originalId, _ := IdFromBytes(pack.MessageId)
			if retryId != originalId || client.laneProvenTimeoutWriteCount.Load() != 1 {
				t.Fatal("proved lane loss did not recover the original probe once")
			}
		default:
			t.Fatal("probe protection delayed a hole proved by later same-lane delivery")
		}
		time.Sleep(time.Until(at.Add(1210 * time.Millisecond)))
		delay, compression := uint32(10000), uint32(10000)
		if ok, err := sequence.Ack(&protocol.Ack{MessageId: pack.MessageId, SequenceId: pack.SequenceId, Tag: pack.Tag, ReceiverAckDelayMicros: &delay, AckCompressTimeoutMicros: &compression}, 0); !ok || err != nil {
			t.Fatalf("exact reply refused: %t %v", ok, err)
		}
		synctest.Wait()
		if got := sequence.windowPacer.service.roundTripEvidence(time.Now()).minimum; got != 300*time.Microsecond {
			t.Fatalf("ambiguous probe reply changed the RTT floor to %s", got)
		}
	})
}

// Another lane's progress cannot prove this probe was lost or shorten its
// protected physical residence. A later single-copy reply still refreshes RTT.
func TestWindowPacingPairedProbeRecoveryIgnoresUnrelatedLaneAck(t *testing.T) {
	testWindowPacingPairedProbeRecoveryFixture(t, func(settings *SendBufferSettings) {
		settings.ReliableLaneProvenRecovery = true
	}, func(sequence *SendSequence) {
		fired := false
		sequence.sendBuffer.beforeDueResendForTest = func(sendSequenceId, uint64) {
			if fired {
				return
			}
			fired = true
			item := sequence.resendQueue.PeekFirst()
			sequence.observeLaneAck(&sendItem{
				carrierRoute: make(Route),
				transferItem: transferItem{sequenceNumber: item.sequenceNumber + 1},
			}, time.Now())
			if verdict := sequence.laneTimerVerdictFor(item); verdict != laneTimerSilent {
				t.Fatalf("unrelated lane changed the probe's delivery evidence: %d", verdict)
			}
		}
	}, func(t *testing.T, sequence *SendSequence, client *Client, route Route, pack *protocol.Pack, at time.Time, _ <-chan error) {
		time.Sleep(time.Until(at.Add(1210 * time.Millisecond)))
		synctest.Wait()
		select {
		case bytes := <-route:
			MessagePoolReturn(bytes)
			t.Fatal("unrelated lane progress caused a premature probe retry")
		default:
		}
		delay, compression := uint32(10000), uint32(10000)
		if ok, err := sequence.Ack(&protocol.Ack{MessageId: pack.MessageId, SequenceId: pack.SequenceId, Tag: pack.Tag, ReceiverAckDelayMicros: &delay, AckCompressTimeoutMicros: &compression}, 0); !ok || err != nil {
			t.Fatalf("exact reply refused: %t %v", ok, err)
		}
		synctest.Wait()
		if got := sequence.windowPacer.service.roundTripEvidence(time.Now()).minimum; got != 1200*time.Millisecond {
			t.Fatalf("unambiguous physical probe did not refresh RTT: %s", got)
		}
	})
}
