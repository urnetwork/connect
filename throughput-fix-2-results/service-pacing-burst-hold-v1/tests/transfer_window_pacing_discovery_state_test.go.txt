// The discovery and hold state of the shared pacing service, driven with
// explicit times so every threshold is hand-checked rather than simulated.
package connect

import (
	"testing"
	"time"
)

// Discovery ends once, on the drain check's margin, and a recovery
// reservation both ends it and releases the held pace. A later queue beyond
// the permitted burst duration releases it too, while an unqueued reply
// keeps it. A minute of silence replaces the baseline and restarts discovery.
func TestWindowPacingServiceObservesQueueOnce(t *testing.T) {
	service := newWindowPacingService(DefaultSendBufferSettings())
	start := time.Now()
	if discovering, held := service.pacingHold(); !discovering || held != 0 {
		t.Fatalf("a new service is not discovering: %t %d", discovering, held)
	}
	service.observeRoundTrip(100*time.Millisecond, 10*time.Millisecond, start)
	if discovering, _ := service.pacingHold(); !discovering {
		t.Fatal("the unloaded minimum ended discovery")
	}
	// The legacy threshold is the minimum plus compression plus the drain
	// check's margin: 100 + 10 + 25 ms.
	service.observeRoundTrip(111*time.Millisecond, 10*time.Millisecond, start.Add(200*time.Millisecond))
	if discovering, _ := service.pacingHold(); !discovering {
		t.Fatal("a sample inside the margin ended discovery")
	}
	service.observeRoundTrip(140*time.Millisecond, 10*time.Millisecond, start.Add(400*time.Millisecond))
	if discovering, _ := service.pacingHold(); discovering {
		t.Fatal("a queued sample did not end discovery")
	}
	service.observeRoundTrip(100*time.Millisecond, 10*time.Millisecond, start.Add(600*time.Millisecond))
	if discovering, _ := service.pacingHold(); discovering {
		t.Fatal("an unloaded sample restarted discovery")
	}
	service.holdPacing(5000000)
	if discovering, held := service.pacingHold(); discovering || held != 5000000 {
		t.Fatalf("admission did not record its pace: %t %d", discovering, held)
	}
	waiter := &windowPacingWaiter{}
	service.reserve(start.Add(700*time.Millisecond), 1000, 5000000, 5000000, 0, 0, true, waiter)
	func() {
		service.stateLock.Lock()
		defer service.stateLock.Unlock()
		service.removeWaiterWithLock(waiter)
		service.pacingReservations--
	}()
	if discovering, held := service.pacingHold(); discovering || held != 0 {
		t.Fatalf("a recovery write did not release the held pace: %t %d", discovering, held)
	}
	fresh := newWindowPacingService(DefaultSendBufferSettings())
	freshWaiter := &windowPacingWaiter{}
	fresh.reserve(start.Add(700*time.Millisecond), 1000, 5000000, 5000000, 0, 0, true, freshWaiter)
	func() {
		fresh.stateLock.Lock()
		defer fresh.stateLock.Unlock()
		fresh.removeWaiterWithLock(freshWaiter)
		fresh.pacingReservations--
	}()
	if discovering, _ := fresh.pacingHold(); discovering {
		t.Fatal("a recovery write before any round trip left the service discovering")
	}
	// Discovery is over, so a later queued reply can only act on the hold:
	// the path is the limit again and the previous pace must not floor it.
	service.holdPacing(6000000)
	service.observeRoundTrip(150*time.Millisecond, 10*time.Millisecond, start.Add(800*time.Millisecond))
	if discovering, held := service.pacingHold(); discovering || held != 0 {
		t.Fatalf("a queued reply did not release the held pace: %t %d", discovering, held)
	}
	service.observeRoundTrip(100*time.Millisecond, 10*time.Millisecond, start.Add(900*time.Millisecond))
	service.holdPacing(7000000)
	if discovering, held := service.pacingHold(); discovering || held != 7000000 {
		t.Fatalf("an unqueued reply did not keep the held pace: %t %d", discovering, held)
	}
	service.observeRoundTrip(100*time.Millisecond, 10*time.Millisecond, start.Add(70*time.Second))
	if discovering, held := service.pacingHold(); !discovering || held != 0 {
		t.Fatalf("a minute of silence did not restart discovery: %t %d", discovering, held)
	}
}
