// A controlled pause must cover an actually observed long propagation path.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// The tail has exactly 1.2 seconds of physical flight left when the successor
// asks to drain. Virtual-time barriers force expiry before the covering ACK.
func testWindowPacingConfiguredArrivalDuringDrain(t *testing.T) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
		settings.SendBufferSettings.ApplyWindowSizing()
		settings.SendBufferSettings.AckTimeout = time.Minute
		settings.SendBufferSettings.beforeRunSendSequenceForTest = func(sendSequenceId) { <-ctx.Done() }
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		defer func() { cancel(); client.CloseAndWait(context.Background()) }()
		destination := NewId()
		sequence := client.sendBuffer.createSendSequence(sendSequenceId{Destination: destination}, &SendPack{Destination: destination})
		if sequence == nil || sequence.windowPacer.service == nil {
			t.Fatal("configured delivery-sized sequence did not acquire a shared service")
		}
		start := time.Now()
		service := sequence.windowPacer.service
		service.sent = 1000
		service.observeRoundTrip(time.Millisecond, 0, start.Add(-90*time.Millisecond))
		residence := 10 * time.Millisecond
		for ago := 8; ago > 0; ago-- {
			service.observeRoundTrip(residence, 0, start.Add(-time.Duration(ago)*10*time.Millisecond))
		}
		sequenceId, tail, resumed := sequence.sequenceId, NewId(), NewId()
		service.beginWrite(sequenceId, tail, 1, start, false)
		service.finishWrite(sequenceId, tail, true)
		pacer := &windowBurstPacer{service: service, serviceSequenceId: sequenceId, rate: 1000000}
		defer pacer.close()
		type admission struct {
			at  time.Time
			err error
		}
		admitted := make(chan admission, 1)
		go func() {
			err := pacer.waitForServiceMessage(context.Background(), 1000, false, sequenceId, resumed, 2)
			admitted <- admission{at: time.Now(), err: err}
		}()
		synctest.Wait()
		service.stateLock.Lock()
		drainUntil, waiterHead := service.drainUntil, service.waiterHead
		service.stateLock.Unlock()
		if drainUntil.IsZero() || waiterHead != &pacer.waiter {
			t.Fatal("the actual FIFO head did not enter its controlled drain")
		}
		time.Sleep(20 * time.Millisecond)
		service.observeRoundTrip(1200*time.Millisecond, 0, time.Now())
		time.Sleep(980 * time.Millisecond)
		synctest.Wait()
		var result admission
		early := false
		select {
		case result = <-admitted:
			early = true
			t.Errorf("observed 1.2 s flight was released at %s before its covering ACK", result.at.Sub(start))
			service.finishWrite(sequenceId, resumed, true)
		default:
		}
		time.Sleep(200 * time.Millisecond)
		service.acknowledgeWrite(sequenceId, tail, 1, false, 0, time.Now())
		service.observe(1000, time.Now())
		synctest.Wait()
		if !early {
			result = <-admitted
			service.finishWrite(sequenceId, resumed, true)
		}
		if result.err != nil {
			t.Fatal(result.err)
		}
		at := result.at.Add(1200 * time.Millisecond)
		time.Sleep(time.Until(at))
		service.acknowledgeWrite(sequenceId, resumed, 2, false, 0, time.Now())
		service.observe(1000, time.Now())
		pacer.serviceAcked = 1000
		if got := service.roundTrip(); got != 1200*time.Millisecond {
			t.Errorf("expired pause lost the unqueued RTT probe: floor=%s, want 1.2s", got)
		}
	})
}

// Newly arrived long-path evidence can update an already-active short drain,
// without extending its fixed configured lifetime or restarting cooldown.
func TestWindowPacingLongResidenceArrivesDuringControlledDrain(t *testing.T) {
	testWindowPacingConfiguredArrivalDuringDrain(t)
}
