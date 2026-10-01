// A drain extension retains the original attempt's deadline and ownership.
package connect

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// A timer-dispatch barrier orders new feedback or tail delivery exactly at
// the former expiry, without relying on which runnable goroutine wins.
func TestWindowPacingDrainExtensionOrdersExpiryAndDelivery(t *testing.T) {
	for _, event := range []string{"just-before", "at-expiry", "tail-at-expiry", "no-evidence", "cancel"} {
		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			service := &windowPacingService{drainMaximumTime: 3 * time.Second, sent: 1000}
			service.observeRoundTrip(time.Millisecond, 0, start.Add(-90*time.Millisecond))
			for ago := 8; ago > 0; ago-- {
				service.observeRoundTrip(10*time.Millisecond, 0, start.Add(-time.Duration(ago)*10*time.Millisecond))
			}
			sequence, tail, resumed := NewId(), NewId(), NewId()
			service.beginWrite(sequence, tail, 1, start, false)
			service.finishWrite(sequence, tail, true)
			pacer := &windowBurstPacer{service: service, serviceSequenceId: sequence, rate: 1000000}
			defer pacer.close()
			woke, release := make(chan struct{}), make(chan struct{})
			var first sync.Once
			pacer.afterWaitForTest = func() { first.Do(func() { close(woke); <-release }) }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type result struct {
				at  time.Time
				err error
			}
			done := make(chan result, 1)
			go func() {
				err := pacer.waitForServiceMessage(ctx, 1000, false, sequence, resumed, 2)
				done <- result{at: time.Now(), err: err}
			}()
			synctest.Wait()
			if event == "just-before" {
				time.Sleep(40*time.Millisecond - time.Nanosecond)
				service.observeRoundTrip(1200*time.Millisecond, 0, time.Now())
				time.Sleep(time.Nanosecond)
			} else {
				time.Sleep(40 * time.Millisecond)
			}
			<-woke
			if event == "at-expiry" || event == "cancel" {
				service.observeRoundTrip(1200*time.Millisecond, 0, time.Now())
			}
			if event == "tail-at-expiry" {
				service.acknowledgeWrite(sequence, tail, 1, false, 0, time.Now())
				service.observe(1000, time.Now())
			}
			close(release)
			synctest.Wait()
			if event == "cancel" {
				cancel()
				synctest.Wait()
				got := <-done
				if !errors.Is(got.err, context.Canceled) || got.at != start.Add(40*time.Millisecond) {
					t.Fatalf("cancellation result=%+v", got)
				}
				service.stateLock.Lock()
				head, count := service.waiterHead, service.pacingReservations
				service.stateLock.Unlock()
				if head != nil || count != 0 {
					t.Fatal("canceled extension retained a reservation")
				}
				return
			}
			if event == "no-evidence" || event == "tail-at-expiry" {
				got := <-done
				if got.err != nil || got.at != start.Add(40*time.Millisecond) {
					t.Fatalf("event=%s: result=%+v", event, got)
				}
				service.finishWrite(sequence, resumed, true)
				service.stateLock.Lock()
				hasProbe := !service.roundTripProbe.sentAt.IsZero()
				service.stateLock.Unlock()
				if hasProbe != (event == "tail-at-expiry") {
					t.Fatalf("event=%s: expired timer invented or lost physical drain proof", event)
				}
				return
			}
			select {
			case got := <-done:
				t.Fatalf("event=%s: new physical evidence did not extend the old pause: %+v", event, got)
			default:
			}
			time.Sleep(time.Until(start.Add(1200 * time.Millisecond)))
			service.acknowledgeWrite(sequence, tail, 1, false, 0, time.Now())
			service.observe(1000, time.Now())
			synctest.Wait()
			got := <-done
			if got.err != nil || got.at != start.Add(1200*time.Millisecond) {
				t.Fatalf("event=%s: tail did not end the extended pause: %+v", event, got)
			}
			service.finishWrite(sequence, resumed, true)
			time.Sleep(1200 * time.Millisecond)
			service.acknowledgeWrite(sequence, resumed, 2, false, 0, time.Now())
			if floor := service.roundTrip(); floor != 1200*time.Millisecond {
				t.Fatalf("event=%s: floor=%s", event, floor)
			}
		})
	}
}

// Repeated larger observations can extend one attempt only to its original
// configured bound. Its cooldown remains anchored there after missing delivery.
func TestWindowPacingRepeatedDrainEvidenceCannotRenewLifetime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		service := &windowPacingService{drainMaximumTime: 500 * time.Millisecond, sent: 1000}
		service.observeRoundTrip(time.Millisecond, 0, start.Add(-90*time.Millisecond))
		for ago := 8; ago > 0; ago-- {
			service.observeRoundTrip(10*time.Millisecond, 0, start.Add(-time.Duration(ago)*10*time.Millisecond))
		}
		sequence, tail := NewId(), NewId()
		service.beginWrite(sequence, tail, 1, start, false)
		service.finishWrite(sequence, tail, true)
		pacer := &windowBurstPacer{service: service, serviceSequenceId: sequence, rate: 1000000}
		defer pacer.close()
		done := make(chan time.Time, 1)
		go func() {
			if err := pacer.waitForServiceMessage(context.Background(), 1000, false, sequence, NewId(), 2); err != nil {
				t.Error(err)
			}
			done <- time.Now()
		}()
		synctest.Wait()
		for _, sample := range []struct{ after, residence time.Duration }{{after: 20 * time.Millisecond, residence: 100 * time.Millisecond}, {after: 190 * time.Millisecond, residence: 200 * time.Millisecond}, {after: 390 * time.Millisecond, residence: time.Second}, {after: 490 * time.Millisecond, residence: 2 * time.Second}} {
			time.Sleep(time.Until(start.Add(sample.after)))
			service.observeRoundTrip(sample.residence, 0, time.Now())
			synctest.Wait()
			select {
			case at := <-done:
				t.Fatalf("larger evidence lost its existing pause at%s", at.Sub(start))
			default:
			}
		}
		time.Sleep(time.Until(start.Add(500 * time.Millisecond)))
		synctest.Wait()
		if at := <-done; at != start.Add(500*time.Millisecond) {
			t.Fatalf("repeated evidence renewed configured500ms lifetime: %s", at.Sub(start))
		}
		service.stateLock.Lock()
		checkAt, started := service.drainCheckAt, service.drainStartedAt
		service.stateLock.Unlock()
		if started != start || checkAt != start.Add(5*time.Second) {
			t.Fatalf("drain/cooldown moved its original anchor: start=%s cooldown=%s", started.Sub(start), checkAt.Sub(start))
		}
	})
}
