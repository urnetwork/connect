// The constrained sdk opening can drain in one cumulative reply, leaving no
// measured service. Its next feedback turn must retain that physical boundary.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// One physically confirmed opening and resumed train share a real pacer.
// Every byte in service accounting belongs to a confirmed fixture write.
type windowSdkInitialServiceFixture struct {
	service          *windowPacingService
	pacer            *windowBurstPacer
	start            time.Time
	firstFreshId     Id
	firstFreshAt     time.Time
	firstFreshNumber uint64
	target           ByteCount
	initial          ByteCount
	openingAckBytes  ByteCount
}

// Called inside virtual time. The trace's 123 opening writes total 328656
// wire bytes; one small resumed reply arrives exactly 400.011072 ms later.
func newWindowSdkInitialServiceFixture(t *testing.T, openingDrained bool) *windowSdkInitialServiceFixture {
	t.Helper()
	target := ByteCount(125000000)
	fixture := &windowSdkInitialServiceFixture{
		service: &windowPacingService{}, start: time.Now(),
		target: ByteCount(float64(target) / goodputFactor), initial: 512 * 1024,
	}
	fixture.pacer = &windowBurstPacer{service: fixture.service, serviceSequenceId: NewId(), rate: fixture.target}
	var openingIds [123]Id
	for number := range openingIds {
		messageId := NewId()
		openingIds[number] = messageId
		if err := fixture.pacer.waitForServiceMessage(context.Background(), 2672, false, fixture.pacer.serviceSequenceId, messageId, uint64(number)); err != nil {
			t.Fatal(err)
		}
		fixture.service.finishWrite(fixture.pacer.serviceSequenceId, messageId, true)
	}
	if time.Now() != fixture.start || fixture.service.sent != 328656 {
		t.Fatal("opening flight did not fit its configured discovery burst")
	}
	time.Sleep(405 * time.Millisecond)
	lastAckNumber := len(openingIds) - 1
	if !openingDrained {
		lastAckNumber--
	}
	fixture.openingAckBytes = ByteCount(lastAckNumber+1) * 2672
	fixture.service.observeRoundTrip(405*time.Millisecond, 10*time.Millisecond, time.Now())
	fixture.service.acknowledgeWrite(fixture.pacer.serviceSequenceId, openingIds[lastAckNumber], uint64(lastAckNumber), false, 10*time.Millisecond, time.Now())
	fixture.service.observe(fixture.openingAckBytes, time.Now())
	fixture.pacer.serviceAcked += fixture.openingAckBytes
	if fixture.service.drained != openingDrained {
		t.Fatal("opening delivery did not establish the requested physical boundary")
	}
	if rate, _, latest := fixture.service.measured(time.Second, time.Now()); rate != 0 || latest != 0 {
		t.Fatal("one opening checkpoint unexpectedly established service")
	}
	fixture.pacer.rate = windowPacingRate(SendWindowEstimate{Initial: fixture.initial, WindowRoundTrip: 415 * time.Millisecond}, fixture.target)
	probeId := NewId()
	if err := fixture.pacer.waitForServiceMessage(context.Background(), 1384, false, fixture.pacer.serviceSequenceId, probeId, 123); err != nil {
		t.Fatal(err)
	}
	fixture.service.finishWrite(fixture.pacer.serviceSequenceId, probeId, true)
	if time.Now() != fixture.start.Add(405*time.Millisecond) {
		t.Fatal("first resumed write did not immediately follow opening delivery")
	}
	for number := uint64(124); number < 224; number++ {
		messageId := NewId()
		if err := fixture.pacer.waitForServiceMessage(context.Background(), 2673, false, fixture.pacer.serviceSequenceId, messageId, number); err != nil {
			t.Fatal(err)
		}
		fixture.service.finishWrite(fixture.pacer.serviceSequenceId, messageId, true)
		if number == 124 {
			fixture.firstFreshId, fixture.firstFreshAt, fixture.firstFreshNumber = messageId, time.Now(), number
		}
	}
	probeAckAt := fixture.start.Add(805*time.Millisecond + 11072*time.Nanosecond)
	if !time.Now().Before(probeAckAt) {
		t.Fatal("resumed fixture writes overtook their forced first reply")
	}
	time.Sleep(time.Until(probeAckAt))
	fixture.service.observeRoundTrip(400*time.Millisecond+11072*time.Nanosecond, 10*time.Millisecond, time.Now())
	fixture.service.acknowledgeWrite(fixture.pacer.serviceSequenceId, probeId, 123, !openingDrained, 10*time.Millisecond, time.Now())
	fixture.service.observe(1384, time.Now())
	fixture.pacer.serviceAcked += 1384
	return fixture
}

// A confirmed empty opening separates two feedback trains even without a
// prior positive rate. The first resumed reply cannot price their turnaround.
func TestWindowPacingSdkInitialDrainedReplyDoesNotEstablishService(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newWindowSdkInitialServiceFixture(t, true)
		defer fixture.pacer.close()
		rate, total, latest := fixture.service.measured(time.Second, time.Now())
		if total != 330040 {
			t.Fatal("the opening or resumed physical bytes were counted incorrectly")
		}
		measured := max(rate, latest)
		if measured != 0 {
			t.Errorf("one resumed reply priced a drained 400.011072 ms turnaround as service: %d B/s", measured)
		}
		residence := 410*time.Millisecond + 11072*time.Nanosecond
		estimate := SendWindowEstimate{
			Initial: fixture.initial, WindowRoundTrip: residence,
			ServiceByteRate: measured, ServiceEstablished: true,
			ServiceBacklogged: fixture.service.backloggedAt(measured, time.Now()),
		}
		fixture.pacer.rate = windowPacingRate(estimate, fixture.target)
		fixture.pacer.estimateRate = measured
		before := time.Now()
		messageId := NewId()
		if err := fixture.pacer.waitForServiceMessage(context.Background(), 2673, false, fixture.pacer.serviceSequenceId, messageId, 224); err != nil {
			t.Fatal(err)
		}
		fixture.service.finishWrite(fixture.pacer.serviceSequenceId, messageId, true)
		fallback := windowPacingRate(SendWindowEstimate{Initial: fixture.initial, WindowRoundTrip: residence}, fixture.target)
		allowed := 2*max(deliverySizedWindowSampleInterval, fixture.service.bucketInterval, fixture.service.compression) + windowPacingSerializationTime(2673, fallback)
		if delay := time.Since(before); delay > allowed {
			t.Errorf("isolated opening reply imposed %s of pacing debt; configured startup permits %s", delay, allowed)
		}
		t.Logf("opening=328656 ack=405ms resumed=1384 ack=805.011072ms service=%d next-write-wait=%s", measured, time.Since(before))
	})
}

// The physical boundary is required: when an older tail remains unproved,
// sparse receipts may be real slow service and must still establish its rate.
func TestWindowPacingSdkInitialOutstandingFlightDiscoversSlowService(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newWindowSdkInitialServiceFixture(t, false)
		defer fixture.pacer.close()
		rate, total, latest := fixture.service.measured(time.Second, time.Now())
		want := ByteCount(float64(1384) / (400*time.Millisecond + 11072*time.Nanosecond).Seconds())
		if max(rate, latest) != want || total != fixture.openingAckBytes+1384 {
			t.Fatalf("continuous outstanding flight lost real sparse service: got=%d/%d total=%d want=%d", rate, latest, total, want)
		}
	})
}

// Once two checkpoints belong to the resumed train, even genuinely slow
// service replaces startup. The drained opening grants no permanent floor.
func TestWindowPacingSdkInitialFreshPairDiscoversSlowService(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newWindowSdkInitialServiceFixture(t, true)
		defer fixture.pacer.close()
		fixture.service.measured(time.Second, time.Now())
		time.Sleep(100 * time.Millisecond)
		fixture.service.observeRoundTrip(time.Since(fixture.firstFreshAt), 10*time.Millisecond, time.Now())
		fixture.service.acknowledgeWrite(fixture.pacer.serviceSequenceId, fixture.firstFreshId, fixture.firstFreshNumber, false, 10*time.Millisecond, time.Now())
		fixture.service.observe(2673, time.Now())
		fixture.pacer.serviceAcked += 2673
		rate, total, latest := fixture.service.measured(time.Second, time.Now())
		if max(rate, latest) != 26730 || total != 332713 {
			t.Fatalf("fresh slow pair failed to establish its own service: got=%d/%d total=%d want=26730", rate, latest, total)
		}
	})
}
