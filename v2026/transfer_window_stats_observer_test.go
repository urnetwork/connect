// Destination statistics must not change a later sender's pacing evidence.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// Explicit delivery evidence changes after the controller's last estimate.
// No transfer worker runs; the only optional action is the public stats read.
func windowPacingStatsObserverFixture(t *testing.T, perSample ByteCount, budgeted bool) (*Client, Id, *SendSequence, *windowPacingService) {
	t.Helper()
	service, pacer := newWindowPacingSourceIdleFixture(t)
	t.Cleanup(pacer.close)
	sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
		settings.DeliverySizedWindowScale = 2
		settings.TargetGoodputByteRate = 125000000
		if budgeted {
			settings.ResendQueueBudget = NewTransferMemoryBudget(64 * 1024 * 1024)
		}
	})
	t.Cleanup(func() { sequence.resendQueue.Clear() })
	sequence.windowPacer.service = service
	destinationId := NewId()
	client := &Client{sendBuffer: &SendBuffer{
		sendSequences: map[sendSequenceId]*SendSequence{
			{Destination: destinationId}: sequence,
		},
	}}
	time.Sleep(80 * time.Millisecond)
	service.observe(perSample, time.Now())
	time.Sleep(10 * time.Millisecond)
	service.observe(perSample, time.Now())
	if budgeted {
		sampleRoundTrip(sequence, time.Millisecond)
	}
	return client, destinationId, sequence, service
}

// Statistics may report fresh evidence but cannot commit it to the controller.
// The ordinary controller read must still accept the same slower evidence.
func TestWindowPacingDestinationStatsDoNotUpdateServiceHold(t *testing.T) {
	for _, perSample := range []ByteCount{5000, 200000} {
		for _, configuration := range []struct {
			budgeted bool
			target   ByteCount
		}{
			{budgeted: false, target: 125000000},
			{budgeted: true, target: 125000000},
			{budgeted: true, target: 0},
		} {
			synctest.Test(t, func(t *testing.T) {
				client, destinationId, sequence, service := windowPacingStatsObserverFixture(t, perSample, configuration.budgeted)
				sequence.sendBufferSettings.TargetGoodputByteRate = configuration.target
				want := 100 * perSample
				for range 3 {
					stats := client.DestinationSendStats(destinationId)
					if stats.SendWindow.ServiceByteRate != want {
						t.Fatalf("budgeted=%t target=%d: snapshot service=%d want=%d", configuration.budgeted, configuration.target, stats.SendWindow.ServiceByteRate, want)
					}
					if service.serviceHoldRate != 10000000 {
						t.Errorf("budgeted=%t target=%d: reading stats changed the controller hold: %d", configuration.budgeted, configuration.target, service.serviceHoldRate)
					}
				}
				if estimate := sequence.sendWindowEstimate(time.Now()); estimate.ServiceByteRate != want || service.serviceHoldRate != want {
					t.Fatalf("the controller could not accept fresh service: estimate=%d hold=%d want=%d", estimate.ServiceByteRate, service.serviceHoldRate, want)
				}
			})
		}
	}
}

// A resumed controlled probe captures the owner's existing hold. Polling the
// public stats surface must not make identical later confirmations diverge.
func TestWindowPacingDestinationStatsCannotRepriceConfirmedProbe(t *testing.T) {
	var rates [2]ByteCount
	for poll := range 2 {
		synctest.Test(t, func(t *testing.T) {
			client, destinationId, sequence, service := windowPacingStatsObserverFixture(t, 5000, false)
			if poll != 0 {
				client.DestinationSendStats(destinationId)
			}
			service.stateLock.Lock()
			service.drainServiceEpoch = true
			service.stateLock.Unlock()
			sequenceId, probeId := NewId(), NewId()
			service.beginWrite(sequenceId, probeId, 1, time.Now(), false)
			service.finishWrite(sequenceId, probeId, true)
			time.Sleep(100 * time.Millisecond)
			service.acknowledgeWrite(sequenceId, probeId, 1, false, 10*time.Millisecond, time.Now())
			service.observe(1000, time.Now())
			rates[poll] = sequence.sendWindowEstimate(time.Now()).ServiceByteRate
		})
	}
	if rates[0] != 10000000 || rates[1] != rates[0] {
		t.Fatalf("stats polling changed confirmed probe service: unpolled=%d polled=%d", rates[0], rates[1])
	}
}

// A live sequence can appear in both lookup tables. Statistics deduplicate
// it and inspect each sibling without letting map order choose a new hold.
func TestWindowPacingDestinationStatsKeepSharedServiceEvidence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, destinationId, first, service := windowPacingStatsObserverFixture(t, 5000, true)
		second := newEstimatorFixture(t, func(settings *SendBufferSettings) {
			settings.DeliverySizedWindowScale = 2
			settings.TargetGoodputByteRate = 125000000
		})
		t.Cleanup(func() { second.resendQueue.Clear() })
		second.windowPacer.service = service
		client.sendBuffer.sendSequencesByDestination = map[Id]map[*SendSequence]bool{
			destinationId: {first: true, second: true},
		}
		for range 4 {
			stats := client.DestinationSendStats(destinationId)
			if stats.SequenceCount != 2 || stats.SendWindow.ServiceByteRate != 500000 || service.serviceHoldRate != 10000000 {
				t.Fatalf("shared stats changed evidence or counted a sequence twice: sequences=%d service=%d hold=%d", stats.SequenceCount, stats.SendWindow.ServiceByteRate, service.serviceHoldRate)
			}
		}
	})
}
