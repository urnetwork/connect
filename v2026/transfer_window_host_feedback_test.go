// Source pauses in inner TCP recovery must not replace physical service
// evidence, while waiting pacing producers retain their serialization clock.
package connect

import (
	"context"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Inner TCP recovery can emit one small packet while its application is
// waiting for feedback. That isolated reply must not price the preceding
// source idle as serialization and delay the resumed bulk write for seconds.
func TestTcpReturnReplayPreservesPacingServiceAcrossFeedbackIdle(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		service := &windowPacingService{}
		sequenceId, tail := NewId(), NewId()
		pacer := &windowBurstPacer{service: service, serviceSequenceId: sequenceId, rate: 125000000, estimateRate: 125000000}
		defer pacer.close()
		replayed := make(chan int, 1)
		settings := DefaultTcpBufferSettingsWithBufferSize(16)
		settings.Log = NewNoopLogger()
		settings.ReturnResendTimeout = time.Second
		sequence := NewTcpSequence(ctx, func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
			messageId := NewId()
			if err := pacer.waitForServiceMessage(ctx, len(packet), false, sequenceId, messageId, 2); err != nil {
				t.Errorf("replay pacing: %v", err)
				return
			}
			service.finishWrite(sequenceId, messageId, true)
			time.Sleep(time.Millisecond)
			service.acknowledgeWrite(sequenceId, messageId, 2, false, 10*time.Millisecond, time.Now())
			service.observe(ByteCount(len(packet)), time.Now())
			replayed <- len(packet)
		}, SourceId(NewId()), protocol.ProvideMode_Network, 4,
			net.IPv4(192, 0, 2, 2).To4(), 41000, net.IPv4(198, 51, 100, 2).To4(), 443, 100, settings)
		sequence.receiveSeq, sequence.receiveSeqAck = 1161, 101
		sequence.receiveWindowSize = 1060
		if !sequence.retainReturnChunk(make([]byte, 1060), 101, false) {
			t.Fatal("return cache did not retain the unacknowledged inner segment")
		}
		defer sequence.releaseReturnChunks()
		done := make(chan struct{})
		go func() { defer close(done); sequence.runReturnRecovery() }()
		defer func() { cancel(); <-done }()

		// The inner ACK clock predates the last ordinary Transfer delivery.
		// This leaves the recovery ACK inside the bounded measurement ring.
		time.Sleep(680 * time.Millisecond)
		service.observeRoundTrip(time.Millisecond, 10*time.Millisecond, time.Now())
		service.beginWrite(sequenceId, tail, 1, time.Now(), false)
		service.finishWrite(sequenceId, tail, true)
		service.observe(1250000, time.Now())
		time.Sleep(10 * time.Millisecond)
		service.acknowledgeWrite(sequenceId, tail, 1, false, 10*time.Millisecond, time.Now())
		service.observe(1250000, time.Now())
		service.stateLock.Lock()
		service.sent = service.total
		idle := service.pendingWrites == 0 && service.pacingReservations == 0
		service.stateLock.Unlock()
		if rate, _, _ := service.measured(time.Second, time.Now()); rate != 125000000 {
			t.Fatalf("ordinary train did not establish its service: %d", rate)
		}
		if !idle {
			t.Fatal("the ordinary source did not enter a feedback idle")
		}
		if count := <-replayed; count != 1100 {
			t.Fatalf("unexpected replay packet: %d", count)
		}
		// Only after the replay crossed Transfer does the delayed inner ACK
		// reopen the NAT window. No missing inner bytes remain to recover.
		sequence.mutex.Lock()
		changed := sequence.applySendAckWithLock(&parsedTcp{ack: true, ackNumber: 1161, windowSize: 4096})
		available := int64(sequence.receiveWindowSize) - int64(sequence.receiveSeq-sequence.receiveSeqAck)
		sequence.mutex.Unlock()
		if !changed || available <= 0 {
			t.Fatalf("the inner ACK did not reopen receive space: %d", available)
		}
		cancel()
		<-done
		rate, _, latest := service.measured(time.Second, time.Now())
		estimate := SendWindowEstimate{ServiceByteRate: max(rate, latest), ServiceEstablished: true}
		if estimate.ServiceByteRate != 125000000 {
			t.Errorf("isolated inner replay replaced established service: %d", estimate.ServiceByteRate)
		}
		pacer.estimateRate = estimate.ServiceByteRate
		pacer.rate = windowPacingRate(estimate, 125000000)
		before := time.Now()
		if err := pacer.waitForServiceMessage(context.Background(), 70*1024, false, sequenceId, NewId(), 3); err != nil {
			t.Fatal(err)
		}
		if delay := time.Since(before); delay > 50*time.Millisecond {
			t.Fatalf("a healthy inner replay priced feedback idle as service: service=%d replay=1100 bulk-wait=%s", estimate.ServiceByteRate, delay)
		}
		t.Logf("replay retained service=%d B/s; next bulk write waited %s", estimate.ServiceByteRate, time.Since(before))
	})
}

// Called inside a virtual-time bubble. The acknowledged train establishes
// a rate below the target, so falling back to optimistic startup is detectable.
func newWindowPacingSourceIdleFixture(t *testing.T) (*windowPacingService, *windowBurstPacer) {
	t.Helper()
	service := &windowPacingService{}
	sequenceId, tail := NewId(), NewId()
	pacer := &windowBurstPacer{service: service, serviceSequenceId: sequenceId, rate: 11000000, estimateRate: 10000000}
	service.observeRoundTrip(time.Millisecond, 10*time.Millisecond, time.Now())
	service.beginWrite(sequenceId, tail, 1, time.Now(), false)
	service.finishWrite(sequenceId, tail, true)
	service.observe(100000, time.Now())
	time.Sleep(10 * time.Millisecond)
	service.acknowledgeWrite(sequenceId, tail, 1, false, 10*time.Millisecond, time.Now())
	service.observe(100000, time.Now())
	service.stateLock.Lock()
	service.sent = service.total
	service.stateLock.Unlock()
	if rate, _, _ := service.measured(time.Second, time.Now()); rate != 10000000 {
		t.Fatalf("ordinary train did not establish service: %d", rate)
	}
	return service, pacer
}

// An idle-delimited reply keeps its earlier service in either ACK/write order.
// Failed physical confirmation rolls back the provisional delimiter, and a
// fresh slower train replaces the held rate without an optimistic restart.
func TestWindowPacingSourceIdleRequiresConfirmationAndFreshEvidence(t *testing.T) {
	for _, ackFirst := range []bool{false, true} {
		for _, h1 := range []bool{false, true} {
			synctest.Test(t, func(t *testing.T) {
				service, pacer := newWindowPacingSourceIdleFixture(t)
				defer pacer.close()
				previousAckAt := time.Now()
				time.Sleep(80 * time.Millisecond)
				probe := NewId()
				if err := pacer.waitForServiceMessage(context.Background(), 1000, false, pacer.serviceSequenceId, probe, 2); err != nil {
					t.Fatal(err)
				}
				if !ackFirst {
					service.finishWrite(pacer.serviceSequenceId, probe, h1)
				}
				time.Sleep(time.Millisecond)
				ackedAt := time.Now()
				service.acknowledgeWrite(pacer.serviceSequenceId, probe, 2, false, 10*time.Millisecond, ackedAt)
				service.observe(1000, ackedAt)
				if ackFirst {
					if rate, _, latest := service.measured(time.Second, ackedAt); max(rate, latest) != 10000000 {
						t.Errorf("ack-first=%t h1=%t: provisional idle reply lost service: %d/%d", ackFirst, h1, rate, latest)
					}
					service.finishWrite(pacer.serviceSequenceId, probe, h1)
				}
				want := ByteCount(float64(1000) / ackedAt.Sub(previousAckAt).Seconds())
				if h1 {
					want = 10000000
				}
				if rate, total, latest := service.measured(time.Second, ackedAt); max(rate, latest) != want || total != 201000 {
					t.Fatalf("ack-first=%t h1=%t: confirmation left service=%d/%d total=%d want=%d/201000", ackFirst, h1, rate, latest, total, want)
				}
				if !h1 {
					if !service.serviceEpochAt.IsZero() {
						t.Fatal("a failed physical write committed the source-idle epoch")
					}
					return
				}
				time.Sleep(50 * time.Millisecond)
				if rate, _, latest := service.measured(time.Second, time.Now()); rate != 0 || latest != 10000000 {
					t.Fatalf("missing fresh evidence lost the measured hold: %d/%d", rate, latest)
				}
				for range 2 {
					time.Sleep(10 * time.Millisecond)
					service.observe(5000, time.Now())
				}
				if rate, _, _ := service.measured(time.Second, time.Now()); rate != 500000 {
					t.Fatalf("fresh slower delivery did not replace the held service: %d", rate)
				}
			})
		}
	}
}

// A waiting reservation proves continued demand before the previous tail
// drains. A delayed timer dispatch cannot turn that pacing gap into source idle.
func TestWindowPacingQueuedDemandPreservesNaturalSerialization(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service, pacer := newWindowPacingSourceIdleFixture(t)
		defer pacer.close()
		tail := NewId()
		service.beginWrite(pacer.serviceSequenceId, tail, 2, time.Now(), false)
		service.finishWrite(pacer.serviceSequenceId, tail, true)
		pacer.rate, pacer.estimateRate = 1000000, 1000000
		dispatched, release := make(chan struct{}), make(chan struct{})
		pacer.afterWaitForTest = func() {
			select {
			case <-dispatched:
			default:
				close(dispatched)
			}
			<-release
		}
		ctx, cancel := context.WithCancel(context.Background())
		probe, done := NewId(), make(chan struct{})
		var err error
		go func() {
			defer close(done)
			err = pacer.waitForServiceMessage(ctx, 64*1024, false, pacer.serviceSequenceId, probe, 3)
		}()
		defer func() {
			cancel()
			select {
			case <-release:
			default:
				close(release)
			}
			<-done
		}()
		synctest.Wait()
		service.stateLock.Lock()
		waiting := service.pacingReservations == 1 && service.pendingWrites == 1
		service.stateLock.Unlock()
		if !waiting {
			t.Fatal("the next producer did not wait before tail delivery")
		}
		previousAckAt := time.Now()
		service.acknowledgeWrite(pacer.serviceSequenceId, tail, 2, false, 10*time.Millisecond, previousAckAt)
		time.Sleep(100 * time.Millisecond)
		<-dispatched
		close(release)
		<-done
		if err != nil {
			t.Fatal(err)
		}
		service.finishWrite(pacer.serviceSequenceId, probe, true)
		time.Sleep(time.Millisecond)
		service.acknowledgeWrite(pacer.serviceSequenceId, probe, 3, false, 10*time.Millisecond, time.Now())
		service.observe(64*1024, time.Now())
		want := ByteCount(float64(64*1024) / time.Since(previousAckAt).Seconds())
		if rate, _, _ := service.measured(time.Second, time.Now()); rate != want || !service.serviceEpochAt.IsZero() {
			t.Fatalf("late paced demand lost natural serialization: rate=%d want=%d epoch=%s", rate, want, service.serviceEpochAt)
		}
	})
}

// A canceled reservation has no physical write to consume the idle boundary.
// The first surviving FIFO producer inherits it, including an interior cancel.
func TestWindowPacingSourceIdleCancellationPreservesSuccessor(t *testing.T) {
	for _, canceled := range []int{0, 1} {
		synctest.Test(t, func(t *testing.T) {
			service, original := newWindowPacingSourceIdleFixture(t)
			defer original.close()
			time.Sleep(80 * time.Millisecond)
			var pacers [2]*windowBurstPacer
			var cancels [2]context.CancelFunc
			var errs [2]error
			done := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
			messages := [2]Id{NewId(), NewId()}
			for i := range pacers {
				ctx, cancel := context.WithCancel(context.Background())
				cancels[i] = cancel
				pacers[i] = &windowBurstPacer{service: service, serviceSequenceId: NewId(), rate: 10000, estimateRate: 10000}
				go func() {
					defer close(done[i])
					errs[i] = pacers[i].waitForServiceMessage(ctx, 1000, false, pacers[i].serviceSequenceId, messages[i], 1)
				}()
				synctest.Wait()
			}
			defer func() {
				for _, cancel := range cancels {
					cancel()
				}
				for i := range pacers {
					<-done[i]
					pacers[i].close()
				}
			}()
			cancels[canceled]()
			<-done[canceled]
			if errs[canceled] != context.Canceled {
				t.Fatalf("canceled=%d: reservation returned %v", canceled, errs[canceled])
			}
			survivor := 1 - canceled
			<-done[survivor]
			if errs[survivor] != nil {
				t.Fatal(errs[survivor])
			}
			service.finishWrite(pacers[survivor].serviceSequenceId, messages[survivor], true)
			time.Sleep(time.Millisecond)
			ackedAt := time.Now()
			service.acknowledgeWrite(pacers[survivor].serviceSequenceId, messages[survivor], 1, false, 10*time.Millisecond, ackedAt)
			service.observe(1000, ackedAt)
			if rate, _, latest := service.measured(time.Second, ackedAt); max(rate, latest) != 10000000 || service.serviceEpochAt != ackedAt {
				t.Fatalf("canceled=%d: successor lost source-idle evidence: %d/%d epoch=%s", canceled, rate, latest, service.serviceEpochAt)
			}
		})
	}
}

// Canceling the last queued producer ends demand without emitting a write.
// After the delivered flight drains, a later source must delimit that idle.
func TestWindowPacingLastCanceledDemandStartsSourceIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service, pacer := newWindowPacingSourceIdleFixture(t)
		defer pacer.close()
		tail := NewId()
		service.beginWrite(pacer.serviceSequenceId, tail, 2, time.Now(), false)
		service.finishWrite(pacer.serviceSequenceId, tail, true)
		pacer.rate, pacer.estimateRate = 10000, 10000
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- pacer.waitForServiceMessage(ctx, 1000, false, pacer.serviceSequenceId, NewId(), 3)
		}()
		synctest.Wait()
		service.stateLock.Lock()
		waiting := service.pacingReservations == 1 && service.pendingWrites == 1
		service.stateLock.Unlock()
		if !waiting {
			cancel()
			<-done
			t.Fatal("the last producer was not waiting before the tail ACK")
		}
		service.acknowledgeWrite(pacer.serviceSequenceId, tail, 2, false, 10*time.Millisecond, time.Now())
		cancel()
		if err := <-done; err != context.Canceled {
			t.Fatalf("canceling the last producer returned %v", err)
		}
		time.Sleep(80 * time.Millisecond)
		pacer.rate, pacer.estimateRate = 11000000, 10000000
		probe := NewId()
		if err := pacer.waitForServiceMessage(context.Background(), 1000, false, pacer.serviceSequenceId, probe, 4); err != nil {
			t.Fatal(err)
		}
		service.finishWrite(pacer.serviceSequenceId, probe, true)
		time.Sleep(time.Millisecond)
		ackedAt := time.Now()
		service.acknowledgeWrite(pacer.serviceSequenceId, probe, 4, false, 10*time.Millisecond, ackedAt)
		service.observe(1000, ackedAt)
		if rate, _, latest := service.measured(time.Second, ackedAt); max(rate, latest) != 10000000 || service.serviceEpochAt != ackedAt {
			t.Fatalf("last canceled demand lost the idle boundary: %d/%d epoch=%s", rate, latest, service.serviceEpochAt)
		}
	})
}

// The source resumes as soon as the local tail observer proves a drain.
// An old wire timestamp or delayed writer callback must not backdate idle.
func TestWindowPacingDelayedDrainObservationKeepsSerialization(t *testing.T) {
	for _, lateConfirmation := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			service, pacer := newWindowPacingSourceIdleFixture(t)
			defer pacer.close()
			tail := NewId()
			service.beginWrite(pacer.serviceSequenceId, tail, 2, time.Now(), false)
			if !lateConfirmation {
				service.finishWrite(pacer.serviceSequenceId, tail, true)
			}
			time.Sleep(time.Millisecond)
			previousAckAt := time.Now()
			if lateConfirmation {
				service.acknowledgeWrite(pacer.serviceSequenceId, tail, 2, false, 10*time.Millisecond, previousAckAt)
			}
			time.Sleep(80 * time.Millisecond)
			if lateConfirmation {
				service.finishWrite(pacer.serviceSequenceId, tail, true)
			} else {
				service.acknowledgeWrite(pacer.serviceSequenceId, tail, 2, false, 10*time.Millisecond, previousAckAt)
			}
			service.observe(10000, previousAckAt)
			probe := NewId()
			if err := pacer.waitForServiceMessage(context.Background(), 1000, false, pacer.serviceSequenceId, probe, 3); err != nil {
				t.Fatal(err)
			}
			service.finishWrite(pacer.serviceSequenceId, probe, true)
			time.Sleep(time.Millisecond)
			service.acknowledgeWrite(pacer.serviceSequenceId, probe, 3, false, 10*time.Millisecond, time.Now())
			service.observe(1000, time.Now())
			want := ByteCount(float64(1000) / time.Since(previousAckAt).Seconds())
			if rate, _, _ := service.measured(time.Second, time.Now()); rate != want || !service.serviceEpochAt.IsZero() {
				t.Fatalf("late confirmation=%t: delayed observation invented idle: %d want=%d epoch=%s", lateConfirmation, rate, want, service.serviceEpochAt)
			}
		})
	}
}

// A recovery copy cannot delimit idle because its ACK is ambiguous. The
// first physical attempt consumes the boundary even when it cannot use it.
func TestWindowPacingRecoveryCannotCarrySourceIdleToLaterWrites(t *testing.T) {
	for _, recovery := range []string{"retry", "unpaced"} {
		synctest.Test(t, func(t *testing.T) {
			service, pacer := newWindowPacingSourceIdleFixture(t)
			defer pacer.close()
			time.Sleep(80 * time.Millisecond)
			if recovery == "unpaced" {
				service.invalidateProbe(pacer.serviceSequenceId)
			}
			first := NewId()
			if err := pacer.waitForServiceMessage(context.Background(), 1000, recovery == "retry", pacer.serviceSequenceId, first, 2); err != nil {
				t.Fatal(err)
			}
			service.finishWrite(pacer.serviceSequenceId, first, true)
			time.Sleep(time.Millisecond)
			service.acknowledgeWrite(pacer.serviceSequenceId, first, 2, false, 10*time.Millisecond, time.Now())
			service.observe(1000, time.Now())
			for number := uint64(3); number <= 4; number++ {
				messageId := NewId()
				if err := pacer.waitForServiceMessage(context.Background(), 1000, false, pacer.serviceSequenceId, messageId, number); err != nil {
					t.Fatal(err)
				}
				service.finishWrite(pacer.serviceSequenceId, messageId, true)
				time.Sleep(10 * time.Millisecond)
				service.acknowledgeWrite(pacer.serviceSequenceId, messageId, number, false, 10*time.Millisecond, time.Now())
				service.observe(1000, time.Now())
			}
			if rate, _, _ := service.measured(time.Second, time.Now()); rate != 100000 || !service.serviceEpochAt.IsZero() {
				t.Fatalf("%s carried the idle marker to a later natural probe: rate=%d epoch=%s", recovery, rate, service.serviceEpochAt)
			}
		})
	}
}

// A Pack can wait before pacing while the ACK worker drains its prior tail.
// Excluding that local delay must still allow a real slower train to replace
// the held rate once the send window admits continuous physical demand.
func TestWindowPacingPackWaitingBeforeReservationAcceptsFreshService(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		service, pacer := newWindowPacingSourceIdleFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
			settings.DeliverySizedWindowScale = 2
		})
		sequence.ctx, sequence.client, sequence.log = ctx, &Client{}, NewNoopLogger()
		sequence.sequenceId, sequence.windowPacer = pacer.serviceSequenceId, *pacer
		sequence.contractMultiRouteWriter = &windowPacingHandoffWriter{windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{h1Only: true}}}
		sequence.ackWindow, sequence.idleCondition = newSequenceAckWindow(), NewIdleCondition()
		sequence.packs = make(chan *SendPack, 1)
		sequence.flightController = newSendFlightController(sequence.sendBufferSettings)
		defer sequence.windowPacer.close()
		defer func() {
			for _, item := range sequence.resendQueue.Clear() {
				item.messagePoolReturn()
			}
		}()
		write := func(number uint64, bytes int) *sendItem {
			item := &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: number},
				sendTime: time.Now(), sendCount: 1, expectsAck: true, transferFrameBytes: MessagePoolGet(bytes)}
			sequence.sendItems = append(sequence.sendItems, item)
			sequence.resendQueue.Add(item)
			sequence.windowPacer.rateUpdated = time.Now()
			if _, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, false, false); err != nil {
				t.Fatal(err)
			}
			return item
		}
		coalesce := func(item *sendItem) {
			sequence.coalesceReceivedAck(sequence.ackWindow, receiveAckMessage{messageId: item.messageId,
				receivedAtNanos: time.Now().UnixNano(), tag: sequenceTag{set: true, sendTime: uint64(item.sendTime.UnixMilli())}})
		}
		apply := func() {
			ack := sequence.ackWindow.Snapshot(true).headAck
			sequence.receiveAckAt(ack.messageId, false, ack.tag, false, ack.receivedAtNanos)
		}
		tail := write(2, 10000)
		sequence.resendCapacityUnavailable.Store(true)
		pack := &SendPack{TransferOptions: TransferOptions{Ack: true}, Ctx: ctx}
		accepted, done := false, make(chan error, 1)
		go func() {
			var err error
			accepted, err = sequence.Pack(pack, -1)
			done <- err
		}()
		synctest.Wait()
		sequence.idleCondition.mutex.Lock()
		waiting := sequence.idleCondition.updateOpenCount == 1
		sequence.idleCondition.mutex.Unlock()
		if !waiting || service.pacingReservations != 0 || len(sequence.packs) != 0 {
			cancel()
			<-done
			t.Fatal("the producer did not block ahead of pacing at resend capacity")
		}
		time.Sleep(time.Millisecond)
		coalesce(tail)
		time.Sleep(80 * time.Millisecond)
		apply()
		sequence.resendCapacityUnavailable.Store(false)
		if err := <-done; err != nil || !accepted {
			t.Fatalf("releasing the send window did not admit its waiting Pack: %t %v", accepted, err)
		}
		if queued := <-sequence.packs; queued != pack {
			t.Fatal("the send window admitted another producer")
		}
		reply := write(3, 1000)
		time.Sleep(time.Millisecond)
		coalesce(reply)
		apply()
		if rate, _, latest := service.measured(time.Second, time.Now()); max(rate, latest) != 10000000 {
			t.Fatalf("local ACK application wait replaced the physical rate: %d/%d", rate, latest)
		}
		// Both messages enter the physical writer before either reply. The
		// slower ten-millisecond ACK spacing is fresh serialization evidence.
		first, second := write(4, 5000), write(5, 5000)
		for _, item := range []*sendItem{first, second} {
			time.Sleep(10 * time.Millisecond)
			coalesce(item)
			apply()
		}
		if rate, _, _ := service.measured(time.Second, time.Now()); rate != 500000 {
			t.Fatalf("the fresh physical train could not replace held service: %d", rate)
		}
	})
}

// One idle lane cannot reset service while any sibling still has physical
// flight. Once all eight tails drain, closing an already delivered sibling
// must leave the shared source boundary available to the next active lane.
func TestWindowPacingSharedSourceIdleRequiresEveryTail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service, original := newWindowPacingSourceIdleFixture(t)
		defer original.close()
		var pacers [8]*windowBurstPacer
		var tails [8]Id
		for i := range pacers {
			pacers[i] = &windowBurstPacer{service: service, serviceSequenceId: NewId(), rate: 11000000, estimateRate: 10000000}
			tails[i] = NewId()
			service.beginWrite(pacers[i].serviceSequenceId, tails[i], 1, time.Now(), false)
			service.finishWrite(pacers[i].serviceSequenceId, tails[i], true)
		}
		defer func() {
			for _, pacer := range pacers {
				pacer.close()
			}
		}()
		for i := range 7 {
			service.acknowledgeWrite(pacers[i].serviceSequenceId, tails[i], 1, false, 10*time.Millisecond, time.Now())
		}
		previousAckAt := time.Now()
		time.Sleep(80 * time.Millisecond)
		first := NewId()
		if err := pacers[0].waitForServiceMessage(context.Background(), 1000, false, pacers[0].serviceSequenceId, first, 2); err != nil {
			t.Fatal(err)
		}
		service.finishWrite(pacers[0].serviceSequenceId, first, true)
		time.Sleep(time.Millisecond)
		service.acknowledgeWrite(pacers[0].serviceSequenceId, first, 2, false, 10*time.Millisecond, time.Now())
		service.observe(1000, time.Now())
		want := ByteCount(float64(1000) / time.Since(previousAckAt).Seconds())
		if rate, _, _ := service.measured(time.Second, time.Now()); rate != want || !service.serviceEpochAt.IsZero() {
			t.Fatalf("one lane ignored its sibling's flight: rate=%d want=%d epoch=%s", rate, want, service.serviceEpochAt)
		}
		service.acknowledgeWrite(pacers[7].serviceSequenceId, tails[7], 1, false, 10*time.Millisecond, time.Now())
		time.Sleep(40 * time.Millisecond)
		pacers[2].close()
		time.Sleep(40 * time.Millisecond)
		resumed := NewId()
		if err := pacers[7].waitForServiceMessage(context.Background(), 100, false, pacers[7].serviceSequenceId, resumed, 2); err != nil {
			t.Fatal(err)
		}
		service.finishWrite(pacers[7].serviceSequenceId, resumed, true)
		time.Sleep(time.Millisecond)
		ackedAt := time.Now()
		service.acknowledgeWrite(pacers[7].serviceSequenceId, resumed, 2, false, 10*time.Millisecond, ackedAt)
		service.observe(100, ackedAt)
		if rate, _, latest := service.measured(time.Second, ackedAt); max(rate, latest) != want || service.serviceEpochAt != ackedAt {
			t.Fatalf("delivered sibling cleanup lost shared idle: rate=%d/%d want=%d epoch=%s", rate, latest, want, service.serviceEpochAt)
		}
	})
}
