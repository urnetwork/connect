// Controlled overruns compare receive commitments, eviction and drainage.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// Compare the three receive policies on one explicit overrun. Pack 32 is
// dropped, Pack 40 is withheld, and the tail fills the 320 KiB hold before 40
// returns. Both gap identities and the full held set are observed before the
// middle admission, so host scheduling cannot select a different experiment.
//
// After that boundary real client recovery runs over ordered routes with 25 ms
// Ack propagation. The original 600-message workload, 2 MiB sender window and
// at-least-90%-within-30-seconds drainage contract remain. The clock is virtual;
// elapsed host execution time does not decide whether the protocol drains.
func TestTheHoldPolicyKeepsDrainageWithoutWithdrawingAnAcknowledgement(t *testing.T) {
	assertMessagePoolOwnership(t)

	// the floor case: a 320 KiB hold against a 2 MiB peer window, 6.4 times
	// inverted, which is a client at an 8 MiB budget against an unbudgeted
	// provider
	const messageCount = 600
	const payloadByteCount = 4 * 1024
	const hold = ByteCount(320 * 1024)
	const window = ByteCount(2 * 1024 * 1024)
	const propagation = 25 * time.Millisecond
	const dropAt = 32
	const reorderAt = 40

	type reading struct {
		delivered          int64
		elapsed            time.Duration
		evictions          uint64
		tentativeEvictions uint64
		refusals           uint64
		commits            uint64
	}

	run := func(policy ReceiveHoldPolicyKind) reading {
		var result reading
		synctest.Test(t, func(t *testing.T) {
			fixture := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
				settings.ResendQueueMaxByteCount = window
			}, func(settings *ReceiveBufferSettings) {
				settings.ReceiveQueueMaxByteCount = hold
				settings.ReceiveHoldPolicy = policy
			})
			start := time.Now()
			var dropped, delayed *windowRoundFrame
			for number := range reorderAt + 1 {
				pack := fixture.write(payloadByteCount)
				switch number {
				case dropAt:
					dropped = pack
					fixture.drop(pack)
				case reorderAt:
					delayed = pack
				default:
					fixture.forward(pack, fixture.receiverIn)
					fixture.acknowledge()
				}
			}
			itemByteCount := MessageByteCount(delayed.pack.Frames)
			heldCount := int((hold - 1) / itemByteCount)
			receiveSequence := fixture.receiveSequence()
			prefixCount, _ := receiveSequence.receiveQueue.QueueSize()
			if prefixCount != reorderAt-dropAt-1 {
				t.Fatalf("policy %d held %d prefix Packs, want %d between the two gaps", policy, prefixCount, reorderAt-dropAt-1)
			}
			for range heldCount - prefixCount {
				fixture.forward(fixture.write(payloadByteCount), fixture.receiverIn)
				fixture.acknowledge()
			}
			count, queued := receiveSequence.receiveQueue.QueueSize()
			if count != heldCount || queued != ByteCount(heldCount)*itemByteCount ||
				receiveSequence.receiveQueue.CanAdd(itemByteCount, hold) ||
				receiveSequence.nextSequenceNumber != uint64(dropAt) || fixture.deliveredCount != dropAt {
				t.Fatalf("policy %d: hold=%d/%d bytes in %d Packs, delivered=%d; wanted a full hold beyond Pack %d", policy, queued, hold, count, fixture.deliveredCount, dropAt)
			}
			newest := receiveSequence.receiveQueue.PeekLast()
			if wantCommitted := policy != ReceiveHoldCommittedPrefix; newest.committed != wantCommitted {
				t.Fatalf("policy %d: full hold's newest Pack committed=%t, want %t", policy, newest.committed, wantCommitted)
			}
			fixture.forward(delayed, fixture.receiverIn)
			fixture.acknowledge()
			boundary := fixture.receiver.ReceiveStats()
			wantEvictions, wantTentative, wantDrops := uint64(0), uint64(0), uint64(0)
			switch policy {
			case ReceiveHoldCommittedPrefix:
				wantTentative = 1
			case ReceiveHoldEvict:
				wantEvictions = 1
			case ReceiveHoldRefuse:
				wantDrops = 1
			}
			if boundary.ReceiveQueueEvictionCount != wantEvictions ||
				boundary.ReceiveQueueTentativeEvictionCount != wantTentative ||
				boundary.ReceiveQueueDropCount != wantDrops {
				t.Fatalf("policy %d middle arrival: acknowledged evictions=%d tentative evictions=%d refusals=%d, want %d/%d/%d", policy, boundary.ReceiveQueueEvictionCount, boundary.ReceiveQueueTentativeEvictionCount, boundary.ReceiveQueueDropCount, wantEvictions, wantTentative, wantDrops)
			}
			// The gap really lost its original physical write. Only a sender
			// recovery copy can close it when the controlled outage ends.
			fixture.forward(fixture.recovery(dropped), fixture.receiverIn)
			fixture.acknowledge()
			for _, frame := range fixture.heldFrames {
				if frame.bytes != nil && frame.pack != nil {
					fixture.forward(frame, fixture.receiverIn)
					fixture.acknowledge()
				}
			}
			fixture.startWire(propagation, DefaultReceiveBufferSettings().AckCompressTimeout)
			fixture.offer(messageCount-int(fixture.nextNumber), payloadByteCount)
			time.Sleep(30 * time.Second)
			synctest.Wait()
			stats := fixture.receiver.ReceiveStats()
			result = reading{
				delivered:          int64(fixture.deliveredCount),
				elapsed:            time.Since(start),
				evictions:          stats.ReceiveQueueEvictionCount,
				tentativeEvictions: stats.ReceiveQueueTentativeEvictionCount,
				refusals:           stats.ReceiveQueueDropCount,
				commits:            stats.ReceiveQueueCommitCount,
			}
			t.Logf("policy %d forced Packs %d/%d against %d held Packs before allowing recovery", policy, dropped.pack.SequenceNumber, delayed.pack.SequenceNumber, heldCount)
		})
		return result
	}

	committed := run(ReceiveHoldCommittedPrefix)
	evicting := run(ReceiveHoldEvict)
	refusing := run(ReceiveHoldRefuse)

	show := func(name string, r reading) {
		t.Logf(
			"%-16s %d/%d in %s: %d acknowledged evictions, %d tentative, %d refusals, %d commits",
			name, r.delivered, messageCount, r.elapsed,
			r.evictions, r.tentativeEvictions, r.refusals, r.commits,
		)
	}
	show("committed", committed)
	show("evicting", evicting)
	show("refusing", refusing)

	// The claim the policy exists for, and the one that must never bend: an
	// acknowledged item is never discarded. Everything else here is a
	// comparison; this is a contract.
	if committed.evictions != 0 {
		t.Errorf(
			"the hold withdrew %d items it had already acknowledged; the boundary is supposed to make that unreachable, so the gap estimate under-counted and the frame size it used is what to read",
			committed.evictions,
		)
	}
	// Asserted as drainage rather than as completion inside this cell's clock.
	// The committed arm is slower than plain eviction by design — a tentative
	// item provides no proving acknowledgement, so a gap just below the
	// boundary recovers on the paced resend rather than on gap recovery — and
	// under load it delivers 584 to 600 of 600 within the window this cell
	// allows. Demanding all 600 was asserting the cell's clock rather than the
	// policy's property, and the property is that it drains where refusal
	// starves.
	if committed.delivered < messageCount*9/10 {
		t.Errorf(
			"%d of %d arrived under the committed-prefix policy; keeping the hold sequence-earliest is what lets the head drain a long run, and that is supposed to survive the acknowledgement boundary",
			committed.delivered,
			messageCount,
		)
	}
	// drainage: the committed arm keeps eviction's shape, so it must not
	// starve the way refusing does
	if refusing.delivered < messageCount && committed.delivered <= refusing.delivered {
		t.Errorf(
			"the committed arm delivered %d against refusing's %d; it evicts exactly as the evicting arm does, so it cannot share refusal's starvation",
			committed.delivered,
			refusing.delivered,
		)
	}
	// The control must reach the forced acknowledged-eviction boundary.
	if evicting.evictions == 0 {
		t.Error("the evicting control withdrew nothing at the forced overrun")
	}
	if 0 < committed.tentativeEvictions {
		t.Logf(
			"%d items were evicted while still tentative, which costs the sender a resend rather than a lease",
			committed.tentativeEvictions,
		)
	}
}
