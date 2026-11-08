// Live diagnostics are tested against actual queue/ack ownership and virtual
// time. Missing and stale samples never masquerade as a drained sequence.
package connect

import (
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A quiescence barrier, not a polling timeout, waits for the existing send
// owner to fulfill the request. The second read also queues the next sample.
func requireSendLiveSample(t *testing.T, client *Client, destination Id) SendSequenceLiveSnapshot {
	t.Helper()
	client.DestinationSendStats(destination)
	synctest.Wait()
	stats := client.DestinationSendStats(destination)
	if !stats.LiveDiagnosticsEnabled || stats.SequenceCount != 1 || len(stats.LiveSequences) != 1 {
		t.Fatalf("live destination membership = %+v", stats)
	}
	sample := stats.LiveSequences[0]
	if !sample.Sampled || sample.UnavailableReason != "" || sample.SampleAge < 0 {
		t.Fatalf("owner sample unavailable or invalid: %+v", sample)
	}
	return sample
}

// Real initial writes own exactly one encoded original each. Selective feedback
// changes delivery progress without freeing its original; the cumulative prefix
// frees both originals and their lifetime reservations.
func TestSendLiveSnapshotActualSelectiveAndCumulativeOwnership(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var senderBudget *TransferMemoryBudget
		fixture, _, _ := newAckRetirementFixture(t, TransportTypeH1, 2, NewNoopLogger(), func(settings *ClientSettings) {
			settings.SendBufferSettings.LiveDiagnostics = true
			settings.ReceiveBufferSettings.AdvertiseReceiveWindow = true
			settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
			budget := NewTransferMemoryBudget(1024 * 1024)
			settings.SendBufferSettings.ResendQueueBudget = budget
			if senderBudget == nil {
				senderBudget = budget
			}
		})
		first := fixture.write(64)
		encoded := ByteCount(len(first.bytes))
		sequence := fixture.sequence()
		initial := requireSendLiveSample(t, fixture.sender, fixture.receiver.ClientId())
		if initial.SequenceId != sequence.sequenceId || initial.SourceId != fixture.sender.ClientId() ||
			initial.DestinationId != fixture.receiver.ClientId() || initial.OriginalItemCount != 1 ||
			initial.UnacknowledgedItemCount != 1 || initial.OriginalEncodedByteCount != encoded ||
			!initial.MonotonicTimeBase || !initial.RetainedAccountingEnabled ||
			initial.OriginalRetainedChargeByteCount != senderBudget.UsedByteCount() ||
			initial.OriginalRetainedChargeByteCount <= encoded || !initial.OldestFirstWriteAgeSampled {
			t.Fatalf("initial owner sample = %+v; encoded=%d budget=%d", initial, encoded, senderBudget.UsedByteCount())
		}

		fixture.forward(fixture.receive(first), fixture.senderIn)
		second, third := fixture.write(80), fixture.write(96)
		encoded = ByteCount(len(second.bytes) + len(third.bytes))
		queued := requireSendLiveSample(t, fixture.sender, fixture.receiver.ClientId())
		if queued.OriginalItemCount != 2 || queued.OriginalEncodedByteCount != encoded || queued.UnacknowledgedItemCount != 2 {
			t.Fatalf("two current original owners = %+v; encoded=%d", queued, encoded)
		}
		time.Sleep(100 * time.Millisecond)
		selective := fixture.receive(third)
		if !selective.ack.Selective {
			t.Fatal("out-of-order original must produce selective feedback")
		}
		fixture.forward(selective, fixture.senderIn)
		selected := requireSendLiveSample(t, fixture.sender, fixture.receiver.ClientId())
		if selected.OriginalItemCount != 2 || selected.UnacknowledgedItemCount != 1 ||
			selected.SelectiveAckCount != 1 || selected.CumulativeAckCount != 1 ||
			selected.CumulativeAckItemCount != 1 || !selected.PeerWindowAdvertised ||
			!selected.OldestFirstWriteAgeSampled || selected.OldestFirstWriteAge != 100*time.Millisecond ||
			!selected.SelectiveAckAgeSampled || selected.SelectiveAckAge != 0 {
			t.Fatalf("selective owner sample = %+v", selected)
		}
		if accepted, err := sequence.Ack(selective.ack, 0); !accepted || err != nil {
			t.Fatalf("duplicate selective feedback: accepted=%t err=%v", accepted, err)
		}
		duplicate := requireSendLiveSample(t, fixture.sender, fixture.receiver.ClientId())
		if duplicate.SelectiveAckCount != 1 || duplicate.OriginalItemCount != 2 {
			t.Fatalf("duplicate became delivery progress: %+v", duplicate)
		}

		fixture.forward(fixture.receive(second), fixture.senderIn)
		drained := requireSendLiveSample(t, fixture.sender, fixture.receiver.ClientId())
		if drained.OriginalItemCount != 0 || drained.OriginalEncodedByteCount != 0 ||
			drained.OriginalRetainedChargeByteCount != 0 || drained.UnacknowledgedItemCount != 0 ||
			drained.OldestFirstWriteAgeSampled || drained.CumulativeAckCount != 2 ||
			drained.CumulativeAckItemCount != 3 || drained.CumulativeAckSequenceNumber != third.pack.SequenceNumber ||
			senderBudget.UsedByteCount() != 0 || fixture.ackedCount != 3 {
			t.Fatalf("cumulative retirement sample = %+v budget=%d callbacks=%d", drained, senderBudget.UsedByteCount(), fixture.ackedCount)
		}
	})
}

// A blocked send owner leaves a timestamped stale sample. Readers touch neither
// its queue lock nor its receive callback, and accepted feedback remains pending
// until the owner applies it. Wrong-generation feedback never changes progress.
func TestSendLiveSnapshotBlockedOwnerStaysExplicitlyStale(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		barrier := newAckHandoffLifetimeBarrier()
		defer barrier.close()
		var pause atomic.Bool
		var destination Id
		fixture, _, _ := newAckRetirementFixture(t, TransportTypeH1, 2, NewNoopLogger(), func(settings *ClientSettings) {
			settings.SendBufferSettings.LiveDiagnostics = true
			settings.SendBufferSettings.afterApplyAckSnapshotForTest = func(id sendSequenceId) {
				if id.Destination == destination && pause.Load() {
					barrier.wait()
				}
			}
		})
		destination = fixture.receiver.ClientId()
		original := fixture.write(64)
		sequence := fixture.sequence()
		requireSendLiveSample(t, fixture.sender, destination)
		synctest.Wait()
		prior := sequence.liveDiagnostics.latest.Load()
		pause.Store(true)
		sequence.readSendLiveSnapshot()
		barrier.requireReached(t)
		time.Sleep(250 * time.Millisecond)
		sequence.resendQueue.stateLock.Lock()
		stale := sequence.readSendLiveSnapshot()
		sequence.resendQueue.stateLock.Unlock()
		if !stale.Sampled || stale.SampledAt != prior.SampledAt || stale.SampleAge != 250*time.Millisecond ||
			stale.OriginalItemCount != 1 || stale.OldestFirstWriteAge != prior.OldestFirstWriteAge {
			t.Fatalf("blocked-owner sample invented freshness: %+v prior=%+v", stale, prior)
		}
		wrong := &protocol.Ack{SequenceId: NewId().Bytes(), MessageId: original.pack.MessageId}
		if accepted, err := sequence.Ack(wrong, 0); accepted || err != nil {
			t.Fatalf("wrong-generation feedback: accepted=%t err=%v", accepted, err)
		}
		reply := fixture.receive(original)
		zeroWindow := uint64(0)
		reply.ack.ReceiveWindowByteCount = &zeroWindow
		if accepted, err := sequence.Ack(reply.ack, 0); !accepted || err != nil {
			t.Fatalf("receive callback could not hand off feedback while owner paused: %t %v", accepted, err)
		}
		if snapshot := sequence.readSendLiveSnapshot(); snapshot.CumulativeAckCount != 0 || snapshot.OriginalItemCount != 1 {
			t.Fatalf("unapplied feedback rewrote the published owner sample: %+v", snapshot)
		}
		pause.Store(false)
		barrier.close()
		synctest.Wait()
		fresh := requireSendLiveSample(t, fixture.sender, destination)
		if fresh.OriginalItemCount != 0 || fresh.CumulativeAckCount != 1 || fresh.SampleAge != 0 ||
			!fresh.PeerWindowAdvertised || fresh.PeerWindowByteCount != 0 {
			t.Fatalf("resumed owner failed to publish applied retirement: %+v", fresh)
		}
	})
}

// Cleanup publishes a terminal zero only after real ownership drains, and the
// buffer then removes the generation rather than retaining sticky live values.
func TestSendLiveSnapshotCleanupAndAdmissionRecovery(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, _, _ := newAckRetirementFixture(t, TransportTypeH1, 2, NewNoopLogger(), func(settings *ClientSettings) {
			settings.SendBufferSettings.LiveDiagnostics = true
			settings.SendBufferSettings.ResendQueueMaxByteCount = 1
		})
		original := fixture.write(64)
		sequence := fixture.sequence()
		blocked := requireSendLiveSample(t, fixture.sender, fixture.receiver.ClientId())
		if blocked.AdmissionReason != "resend_capacity" || blocked.OriginalItemCount != 1 {
			t.Fatalf("actual closed resend gate was not observed: %+v", blocked)
		}
		fixture.forward(fixture.receive(original), fixture.senderIn)
		open := requireSendLiveSample(t, fixture.sender, fixture.receiver.ClientId())
		if open.OriginalItemCount != 0 || open.AdmissionReason != "awaiting_work_or_ack" {
			t.Fatalf("current admission reason remained sticky: %+v", open)
		}
		fixture.write(64)
		sequence.cancel()
		synctest.Wait()
		select {
		case <-sequence.done:
		default:
			t.Fatal("canceled send owner did not finish its indexed lifecycle")
		}
		terminal := sequence.readSendLiveSnapshot()
		if !terminal.Sampled || !terminal.Closed || terminal.AdmissionReason != "closed" || terminal.OriginalItemCount != 0 {
			t.Fatalf("cleanup published retained ownership: %+v", terminal)
		}
		stats := fixture.sender.DestinationSendStats(fixture.receiver.ClientId())
		if stats.SequenceCount != 0 || len(stats.LiveSequences) != 0 {
			t.Fatalf("closed generation remains in live destination membership: %+v", stats)
		}
		fixture.nextNumber = 0
		fixture.write(64)
		successor := requireSendLiveSample(t, fixture.sender, fixture.receiver.ClientId())
		if successor.SequenceId == terminal.SequenceId || successor.Closed || successor.OriginalItemCount != 1 ||
			successor.CumulativeAckCount != 0 || successor.SelectiveAckCount != 0 {
			t.Fatalf("new actual sequence inherited closed ownership or progress: %+v", successor)
		}
	})
}

// The first physical-write clock is independent of recovery's refreshed
// sendTime. Missing, future and zero-age samples retain their distinct meanings.
func TestSendLiveSnapshotTimestampAndGenerationSemantics(t *testing.T) {
	now := time.Now()
	sequence := &SendSequence{
		client: &Client{feedbackTimeBase: now.Add(-time.Hour)}, sequenceId: NewId(), destination: NewId(),
		logicalLane: 2, forceStream: true, companionContract: true,
		encryptionRole: sequenceTlsRoleServer, encryptionCompanion: true,
		liveDiagnostics: newSendLiveDiagnostics(true),
	}
	missing := sequence.readSendLiveSnapshot()
	if missing.Sampled || missing.UnavailableReason != "awaiting_owner" || missing.SequenceId != sequence.sequenceId {
		t.Fatalf("missing owner became zero data: %+v", missing)
	}
	sequence.sendItems = []*sendItem{
		{sendTime: now, pacingSentAtNanos: now.Add(-time.Second).UnixNano(), selectiveAcked: true, deliveryObserved: true},
		{sendTime: now.Add(-time.Hour)},
		{sendTime: now.Add(-time.Hour), pacingSentAtNanos: now.Add(time.Second).UnixNano()},
		{sendTime: now.Add(-time.Hour), pacingSentAtNanos: now.UnixNano()},
	}
	sequence.publishSendLiveSnapshot("resend_capacity", now, false)
	sample := sequence.readSendLiveSnapshot()
	if !sample.Sampled || sample.OriginalItemCount != 4 || sample.UnacknowledgedItemCount != 3 ||
		!sample.MonotonicTimeBase ||
		!sample.OldestFirstWriteAgeSampled || sample.OldestFirstWriteAge != time.Second ||
		sample.FirstWriteMissingCount != 1 || sample.FirstWriteInvalidCount != 1 ||
		sample.PeerWindowAdvertised || sample.CumulativeAckAgeSampled || sample.SelectiveAckAgeSampled {
		t.Fatalf("first-write or missing-data semantics = %+v", sample)
	}
	for _, invalid := range []int64{0, -1, now.Add(time.Second).UnixNano()} {
		if age, valid := sendLiveAge(now.UnixNano(), invalid); valid || age != 0 {
			t.Fatalf("invalid timestamp %d produced age %s sampled=%t", invalid, age, valid)
		}
	}
	if age, valid := sendLiveAge(now.UnixNano(), now.UnixNano()); !valid || age != 0 {
		t.Fatalf("a measured zero age became missing: %s sampled=%t", age, valid)
	}
	priorId := sequence.sequenceId
	sequence.sequenceId = NewId()
	if stale := sequence.readSendLiveSnapshot(); stale.Sampled || stale.UnavailableReason != "generation_mismatch" || stale.SequenceId == priorId {
		t.Fatalf("prior-generation sample attached to successor: %+v", stale)
	}
	sequence.liveDiagnostics = newSendLiveDiagnostics(true)
	sequence.sendItems = nil
	sequence.readSendLiveSnapshot()
	sequence.publishSendLiveSnapshot("awaiting_work_or_ack", now, false)
	reset := sequence.readSendLiveSnapshot()
	if !reset.Sampled || reset.SequenceId == priorId || reset.OriginalItemCount != 0 || reset.SelectiveAckCount != 0 {
		t.Fatalf("successor did not reset its ownership/progress: %+v", reset)
	}
	future := reset
	future.SampledAt = now.Add(time.Hour)
	sequence.liveDiagnostics.latest.Store(&future)
	if invalid := sequence.readSendLiveSnapshot(); invalid.Sampled || invalid.UnavailableReason != "invalid_sample_time" {
		t.Fatalf("future sample timestamp became fresh data: %+v", invalid)
	}
}

// Diagnostics do not manufacture activity. Repeated requests cannot postpone
// the owner's existing idle deadline or leave a live generation after expiry.
func TestSendLiveSnapshotRequestsDoNotRenewIdleLife(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, _, _ := newAckRetirementFixture(t, TransportTypeH1, 2, NewNoopLogger(), func(settings *ClientSettings) {
			settings.SendBufferSettings.LiveDiagnostics = true
			settings.SendBufferSettings.IdleTimeout = 300 * time.Millisecond
		})
		original := fixture.write(64)
		sequence := fixture.sequence()
		fixture.forward(fixture.receive(original), fixture.senderIn)
		for range 2 {
			time.Sleep(100 * time.Millisecond)
			requireSendLiveSample(t, fixture.sender, fixture.receiver.ClientId())
			synctest.Wait()
		}
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		if stats := fixture.sender.DestinationSendStats(fixture.receiver.ClientId()); stats.SequenceCount != 0 {
			t.Fatalf("diagnostic requests renewed idle owner: %+v", stats)
		}
		if sample := sequence.readSendLiveSnapshot(); !sample.Sampled || !sample.Closed {
			t.Fatalf("idle retirement omitted terminal sample: %+v", sample)
		}
	})
}

// Disabled hooks and enabled-but-unrequested owner turns add no heap work.
// Allocation is reserved for explicit diagnostic samples, not packet events.
func TestSendLiveDiagnosticsNoUnrequestedAllocations(t *testing.T) {
	settings := DefaultSendBufferSettings()
	if settings.LiveDiagnostics || newSendLiveDiagnostics(false) != nil {
		t.Fatal("live diagnostics must be disabled by default")
	}
	now := time.Now()
	sequence := &SendSequence{sequenceId: NewId()}
	item := &sendItem{}
	if allocated := testing.AllocsPerRun(100, func() {
		sequence.publishSendLiveSnapshot("ready", now, false)
		sequence.observeLiveSelectiveAck(item, now.UnixNano())
		sequence.observeLiveCumulativeAck(0, 1, now.UnixNano())
		sequence.readSendLiveSnapshot()
	}); allocated != 0 {
		t.Fatalf("disabled diagnostic hooks allocate %g objects", allocated)
	}
	sequence.liveDiagnostics = newSendLiveDiagnostics(true)
	if allocated := testing.AllocsPerRun(100, func() {
		sequence.publishSendLiveSnapshot("ready", now, false)
		sequence.observeLiveSelectiveAck(item, now.UnixNano())
		sequence.observeLiveCumulativeAck(0, 1, now.UnixNano())
	}); allocated != 0 {
		t.Fatalf("unrequested owner turns allocate %g objects", allocated)
	}
	if sequence.liveDiagnostics.latest.Load() != nil {
		t.Fatal("unrequested owner turn published a sample")
	}
}

// The ordinary constructor leaves the optional surface absent through real
// writes and retirement; existing destination counters keep their own meaning.
func TestSendLiveDiagnosticsDisabledConstructor(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, _, _ := newAckRetirementFixture(t, TransportTypeH1, 2, NewNoopLogger())
		original := fixture.write(64)
		sequence := fixture.sequence()
		stats := fixture.sender.DestinationSendStats(fixture.receiver.ClientId())
		if sequence.liveDiagnostics != nil || stats.LiveDiagnosticsEnabled || stats.LiveSequences != nil || stats.WriteCount != 1 {
			t.Fatalf("ordinary constructor enabled instrumentation: %+v", stats)
		}
		fixture.forward(fixture.receive(original), fixture.senderIn)
		stats = fixture.sender.DestinationSendStats(fixture.receiver.ClientId())
		if stats.LiveSequences != nil || stats.WriteCount != 1 || fixture.ackedCount != 1 {
			t.Fatalf("disabled instrumentation changed ordinary delivery: %+v callbacks=%d", stats, fixture.ackedCount)
		}
	})
}
