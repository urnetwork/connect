// Memory clamps govern both the sampled permission and actual queue admission.
package connect

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Real send and receive workers fill the memory share before each Ack round.
// Virtual propagation fixes the measured round trip, and each full delivered
// round supplies the rate that keeps the sampled window memory-clamped.
//
// The former live offer assumed a small pool guaranteed this regime on every
// host. Valid delivery samples can still choose a smaller window. A capacity
// event establishes occupancy here; no periodic peak poll or host-rate premise
// chooses the branch being asserted.
func TestAClampedWindowIsTheCeilingAndFillsIt(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const propagation = 25 * time.Millisecond
		const poolByteCount = ByteCount(192 * 1024)
		const floor = ByteCount(32 * 1024)
		const ceiling = poolByteCount - 2*floor
		const payloadByteCount = 4 * 1024
		const rounds = 8
		budget := NewTransferMemoryBudget(poolByteCount)
		// Two real attached queues reserve their floors for the whole transfer.
		for range 2 {
			other := newResendQueue(budget, floor)
			t.Cleanup(func() { other.Clear() })
		}
		capacityReached := make(chan struct{}, 1)
		fixture := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
			settings.DeliverySizedWindowScale = deliverySizedWindowScale
			settings.ResendQueueBudget = budget
			settings.ResendQueueMinByteCount = floor
			settings.beforeResendCapacityWaitForTest = func(sendSequenceId) {
				select {
				case capacityReached <- struct{}{}:
				default:
				}
			}
		}, func(settings *ReceiveBufferSettings) {
			settings.ReceiveQueueMaxByteCount = 16 * 1024 * 1024
			settings.AdvertiseReceiveWindow = true
		})
		for round := range rounds {
			var lastAck *windowRoundFrame
			var maxFrameByteCount ByteCount
			full := false
			for range int(poolByteCount/payloadByteCount) + 1 {
				pack := fixture.write(payloadByteCount)
				maxFrameByteCount = max(maxFrameByteCount, ByteCount(len(pack.bytes)))
				lastAck = fixture.receive(pack)
				select {
				case <-capacityReached:
					full = true
				default:
				}
				if full {
					break
				}
			}
			if !full {
				t.Fatalf("round %d did not reach the resend capacity boundary", round)
			}
			sequence := fixture.sequence()
			estimate := sequence.sendWindowEstimate(time.Now())
			_, queued := sequence.resendQueue.QueueSize()
			if estimate.Ceiling != ceiling || estimate.Window != ceiling {
				t.Fatalf("round %d estimate=%+v, want the full %d byte memory share", round, estimate, ceiling)
			}
			if queued < ceiling || ceiling+maxFrameByteCount <= queued {
				t.Fatalf("round %d occupancy=%d, want the %d byte window with less than one %d byte frame of overshoot", round, queued, ceiling, maxFrameByteCount)
			}
			if 4 <= round {
				if !estimate.Sized || estimate.Reason != "the memory budget's share" ||
					estimate.RoundTrip != propagation {
					t.Fatalf("round %d did not establish a sampled memory clamp: %+v", round, estimate)
				}
				deliveryWindow := ByteCount(deliverySizedWindowScale) * ByteCount(
					int64(estimate.DeliveredByteCount)*estimate.RoundTrip.Nanoseconds()/estimate.Interval.Nanoseconds())
				if deliveryWindow <= ceiling {
					t.Fatalf("round %d delivery term=%d, want evidence strictly above the %d byte clamp", round, deliveryWindow, ceiling)
				}
			}
			probe := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: "capacity probe"})
			admitted, _ := fixture.sender.SendWithTimeoutDetailed(probe, fixture.receiver.ClientId(), nil, 0)
			if !admitted {
				MessagePoolReturn(probe.MessageBytes)
			} else {
				t.Fatalf("round %d admitted new work while the sampled window was full", round)
			}

			// Only the completed full flight may return its cumulative Ack.
			// The virtual delay is propagation, independent of execution speed.
			time.Sleep(propagation)
			fixture.forward(lastAck, fixture.senderIn)
			if count, _ := sequence.resendQueue.QueueSize(); count != 0 ||
				fixture.ackedCount != fixture.deliveredCount {
				t.Fatalf("round %d did not release the full flight: retained=%d delivered/acknowledged=%d/%d", round, count, fixture.deliveredCount, fixture.ackedCount)
			}
			t.Logf("round %d: ceiling/window %d, occupancy %d, sampled=%t, reason=%q, delivered/acknowledged=%d/%d", round, ceiling, queued, estimate.Sized, estimate.Reason, fixture.deliveredCount, fixture.ackedCount)
		}
		if stats := fixture.receiver.ReceiveStats(); stats.ReceiveQueueEvictionCount != 0 || stats.ReceiveQueueDropCount != 0 {
			t.Fatalf("controlled full flights caused evictions=%d drops=%d", stats.ReceiveQueueEvictionCount, stats.ReceiveQueueDropCount)
		}
	})
}

// Explicit delivery above both shares holds this case in the memory-clamped
// regime. Resizing changes admission immediately while retained items survive
// the shrink and drain normally. A live one-second offer did not establish
// that premise: valid delivery sizing sometimes put its window below the share.
func TestAClampedWindowFollowsItsShareDownAndUp(t *testing.T) {
	const wide = ByteCount(384 * 1024)
	const narrow = ByteCount(192 * 1024)
	const payloadByteCount = 4 * 1024
	sequence, budget, at := newSampledShareWindowFixture(t, 128*1024)
	estimate := func(total ByteCount) SendWindowEstimate {
		t.Helper()
		window := sequence.sendWindowEstimate(at)
		if !window.Sized || window.Ceiling != total-64*1024 ||
			window.Window != window.Ceiling || window.Reason != "the memory budget's share" {
			t.Fatalf("total %d: estimate=%+v, want a sampled memory clamp after two other floors", total, window)
		}
		return window
	}
	atWide := estimate(wide)
	fill := func(window ByteCount) {
		t.Helper()
		for index := 0; index < int(wide/payloadByteCount); index += 1 {
			if !sequence.resendQueue.CanAdd(payloadByteCount, window) {
				return
			}
			sequence.resendQueue.Add(&sendItem{
				transferItem: transferItem{
					messageId:      NewId(),
					sequenceNumber: sequence.nextSequenceNumber,
				},
				transferFrameBytes: make([]byte, payloadByteCount),
			})
			sequence.nextSequenceNumber += 1
		}
		t.Fatal("admission did not stop at the window")
	}
	fill(atWide.Window)
	wideCount, wideQueued := sequence.resendQueue.QueueSize()
	wideReserved := budget.UsedByteCount()
	if wideQueued < atWide.Window-payloadByteCount {
		t.Fatalf("wide occupancy=%d, want the full %d byte window less one item", wideQueued, atWide.Window)
	}
	budget.SetTotalByteCount(narrow)
	atNarrow := estimate(narrow)
	if count, queued := sequence.resendQueue.QueueSize(); count != wideCount || queued != wideQueued || budget.UsedByteCount() != wideReserved {
		t.Fatalf("shrinking evicted retained work: count=%d bytes=%d reserved=%d; before=%d/%d/%d", count, queued, budget.UsedByteCount(), wideCount, wideQueued, wideReserved)
	}
	if sequence.resendQueue.CanAdd(payloadByteCount, atNarrow.Window) {
		t.Fatal("a full wide window admitted more work after its share shrank")
	}
	for range wideCount {
		sequence.resendQueue.RemoveFirst()
		if sequence.resendQueue.CanAdd(payloadByteCount, atNarrow.Window) {
			break
		}
	}
	fill(atNarrow.Window)
	_, narrowQueued := sequence.resendQueue.QueueSize()
	if narrowQueued < atNarrow.Window-payloadByteCount || atNarrow.Window <= narrowQueued {
		t.Fatalf("drained occupancy=%d, want the new %d byte window less one item", narrowQueued, atNarrow.Window)
	}
	budget.SetTotalByteCount(wide)
	atWideAgain := estimate(wide)
	if atWideAgain.Window != atWide.Window || !sequence.resendQueue.CanAdd(payloadByteCount, atWideAgain.Window) {
		t.Fatalf("restored share did not reopen the full window: before=%+v after=%+v", atWide, atWideAgain)
	}
	fill(atWideAgain.Window)
	_, wideAgainQueued := sequence.resendQueue.QueueSize()
	if wideAgainQueued != wideQueued {
		t.Fatalf("restored occupancy=%d, want %d", wideAgainQueued, wideQueued)
	}
	t.Logf("windows %d -> %d -> %d; retained bytes %d -> %d -> %d without eviction", atWide.Window, atNarrow.Window, atWideAgain.Window, wideQueued, narrowQueued, wideAgainQueued)
}

// The rule and delivery-bounded reliable admission do not coexist, and the tree
// refuses rather than documents it.
//
// That admission bound computes its limit as what the lane delivered over a
// round trip, so it derives from delivery rather than from the window. With it
// on the sender sits below the ceiling whatever the ceiling says, which is
// precisely the failure the clamped regime exists to rule out: the ceiling
// reads correctly and the sender is at a fraction of it.
//
// A comment saying two settings conflict is the weakest protection there is,
// and this program has already found a setting pair recorded only in a comment
// separated by whoever came next. So the refusal is structural, at both the
// point of configuration and the point of use.
//
// Prediction, recorded before the run: with delivery-bounded admission on, the
// rule does not engage and says why, whether it was configured through the
// switch or by setting the fields directly.
func TestTheRuleRefusesToCoexistWithDeliveryBoundedAdmission(t *testing.T) {
	restore := MemoryBudget()
	t.Cleanup(func() { SetMemoryBudget(restore) })
	SetMemoryBudget(mib(256))

	// through the switch
	settings := DefaultSendBufferSettings()
	settings.ReliableAdmissionBoundedByDelivery = true
	settings.WindowSizing = WindowSizingFromDelivery
	settings.ApplyWindowSizing()
	if settings.WindowSizingActive() {
		t.Error("the switch engaged the rule alongside delivery-bounded admission")
	}
	if 0 < settings.DeliverySizedWindowScale {
		t.Errorf("the switch left the scale at %d", settings.DeliverySizedWindowScale)
	}

	// and set directly, which is how a cell configures it
	direct := DefaultSendBufferSettings()
	direct.ReliableAdmissionBoundedByDelivery = true
	direct.DeliverySizedWindowScale = deliverySizedWindowScale
	direct.ResendQueueBudget = NewTransferMemoryBudget(mib(8))
	estimate := windowEstimateForSettings(direct)
	t.Logf("configured directly: window %d, reason %q", estimate.Window, estimate.Reason)
	if estimate.Sized {
		t.Errorf(
			"the rule sized a window with delivery-bounded admission on; that bound derives its limit from delivery rather than from this window, so the sender would sit below the ceiling whatever the ceiling says",
		)
	}
	if estimate.Window != direct.ResendQueueMaxByteCount {
		t.Errorf(
			"the window is %d rather than the %d byte constant it should hold at",
			estimate.Window, direct.ResendQueueMaxByteCount,
		)
	}
}
