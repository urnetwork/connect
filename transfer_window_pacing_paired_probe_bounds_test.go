// Metadata peers obey the same delivery, cancellation, and identity limits.
package connect

import (
	"math"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A shorter configured lifetime closes without adding a probe lease or
// emitting recovery after delivery has already expired.
func TestWindowPacingPairedProbeRecoveryCannotExtendAckLifetime(t *testing.T) {
	testWindowPacingPairedProbeRecoveryFixture(t, func(settings *SendBufferSettings) { settings.AckTimeout = 500 * time.Millisecond }, nil, func(t *testing.T, sequence *SendSequence, _ *Client, route Route, _ *protocol.Pack, at time.Time, callbacks <-chan error) {
		time.Sleep(time.Until(at.Add(400 * time.Millisecond)))
		synctest.Wait()
		select {
		case err := <-callbacks:
			if err == nil {
				t.Fatal("expired unacknowledged Pack completed successfully")
			}
		default:
			t.Fatal("probe postponed configured ACK timeout")
		}
		select {
		case bytes := <-route:
			MessagePoolReturn(bytes)
			t.Fatal("delivery expiry emitted a probe retry")
		default:
		}
		if !sequence.windowPacer.service.roundTripProbe.sentAt.IsZero() {
			t.Fatal("closed sequence retained a live RTT probe")
		}
	})
}

// Cancellation consumes the protected probe. Neither its timer nor a late
// peer reply may recreate the canceled physical timing evidence.
func TestWindowPacingPairedProbeRecoveryCancellationConsumesEvidence(t *testing.T) {
	testWindowPacingPairedProbeRecoveryFixture(t, nil, nil, func(t *testing.T, sequence *SendSequence, _ *Client, route Route, pack *protocol.Pack, at time.Time, callbacks <-chan error) {
		time.Sleep(time.Until(at.Add(250 * time.Millisecond)))
		synctest.Wait()
		sequence.Cancel()
		synctest.Wait()
		select {
		case err := <-callbacks:
			if err == nil {
				t.Fatal("canceled Pack completed successfully")
			}
		default:
			t.Fatal("probe cancellation failed to complete the Pack")
		}
		service := sequence.windowPacer.service
		if !service.roundTripProbe.sentAt.IsZero() {
			t.Fatal("canceled sequence retained a probe")
		}
		id, _ := IdFromBytes(pack.MessageId)
		time.Sleep(time.Until(at.Add(1200 * time.Millisecond)))
		service.acknowledgeWrite(sequence.sequenceId, id, pack.SequenceNumber, false, 0, time.Now())
		if got := service.roundTripEvidence(time.Now()).minimum; got != 300*time.Microsecond {
			t.Fatalf("late canceled ACK raised floor to %s", got)
		}
		select {
		case bytes := <-route:
			MessagePoolReturn(bytes)
			t.Fatal("canceled probe wrote recovery")
		default:
		}
	})
}

// Only the exact live probe borrows its dispatch-pinned raw residence. Later
// samples and unrelated or completed identities cannot move that deadline.
func TestWindowPacingPairedProbeRecoveryMatchesOnlyItsLiveEvidence(t *testing.T) {
	start := time.Now()
	sequence, message := NewId(), NewId()
	for _, state := range []string{"live", "other-message", "other-sequence", "unconfirmed", "natural", "acknowledged", "invalidated"} {
		service := newWindowPacingService(DefaultSendBufferSettings())
		service.observeReceiverRoundTrip(0, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, start)
		service.roundTripProbe = windowPacingRoundTripProbe{sequenceId: sequence, messageId: message, sentAt: start, written: true, resetService: true, observedRoundTrip: 1200 * time.Millisecond}
		querySequence, queryMessage := sequence, message
		switch state {
		case "other-message":
			queryMessage = NewId()
		case "other-sequence":
			querySequence = NewId()
		case "unconfirmed":
			service.roundTripProbe.written = false
		case "natural":
			service.roundTripProbe.resetService = false
		case "acknowledged":
			service.roundTripProbe.ackedAt = start.Add(time.Second)
		case "invalidated":
			service.invalidateMessageProbe(sequence, message)
		}
		got := service.probeRecoveryDeadline(querySequence, queryMessage, 2, 8*time.Second)
		if state != "live" {
			if !got.IsZero() {
				t.Fatalf("%s borrowed another live probe: %s", state, got)
			}
			continue
		}
		want := start.Add(2400 * time.Millisecond)
		if got != want {
			t.Fatalf("live probe deadline=%s, want=%s", got, want)
		}
		service.observeRoundTrip(time.Minute, 0, start.Add(time.Second))
		if again := service.probeRecoveryDeadline(sequence, message, 2, 8*time.Second); again != want {
			t.Fatalf("later shared residence extended the absolute deadline: %s", again)
		}
		service.roundTripProbe.observedRoundTrip = time.Duration(math.MaxInt64)
		if got := service.probeRecoveryDeadline(sequence, message, math.MaxFloat32, 8*time.Second); got != start.Add(8*time.Second) {
			t.Fatalf("large residence overflowed configured recovery maximum: %s", got)
		}
		if got := service.probeRecoveryDeadline(sequence, message, 2, 0); !got.IsZero() {
			t.Fatal("nonpositive maximum granted a probe wait")
		}
	}
}
