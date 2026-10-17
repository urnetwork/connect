package connect

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// This compact component fixture uses the same actual SendSequence writer,
// pacer, H1 confirmation and cumulative ACK coalescer as the worker. It does
// not replay the nondeterministic eight-producer path, and never assigns a
// service hold, drain deadline, progress checkpoint, or measurement result.
type windowSettlementDrainFixture struct {
	t         *testing.T
	ctx       context.Context
	service   *windowPacingService
	barrier   *windowPathSettlement
	sequences [8]*SendSequence
	next      [8]uint64
}

// The component regression above isolates the controller timeline. This
// adjacent control reaches the fixture's installed callbacks through an actual
// public Client admission and real Run owner, including joined retirement.
func TestWindowPathSettlementObserverTracksRealOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		settings := func() *ClientSettings {
			s := DefaultClientSettings()
			s.Log = NewNoopLogger()
			s.EncryptionSettings.Mode = EncryptionModeOff
			s.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
			s.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
			s.SendBufferSettings.ApplyWindowSizing()
			s.SendBufferSettings.LogicalDataLaneCount = 8
			return s
		}
		sender, receiver := NewClient(ctx, NewId(), NewNoContractClientOob(), settings()), NewClient(ctx, NewId(), NewNoContractClientOob(), settings())
		sender.ContractManager().AddNoContractPeer(receiver.ClientId())
		out := make(Route, 8)
		sender.RouteManager().UpdateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{out})
		defer func() {
			cancel()
			closeCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			for _, client := range []*Client{sender, receiver} {
				if err := client.CloseAndWait(closeCtx); err != nil {
					t.Errorf("settlement owner cleanup: %v", err)
				}
			}
			for len(out) != 0 {
				MessagePoolReturn(<-out)
			}
		}()
		cell := windowLongRingGrowthCell("delivery", 0)
		cell.Settlement = &windowPathSettlementPolicy{Version: "paired-circulation-v1", ReferenceBytes: 12_000_000,
			ReferenceDuration: time.Second, CapBytes: int64(cell.Budget), AckLifetime: sender.sendBuffer.sendBufferSettings.AckTimeout}
		o, err := newWindowPathSettlement(ctx, cell, time.Now().Add(-10*time.Second), cell.Settlement.AckLifetime)
		if err != nil {
			t.Fatal(err)
		}
		var runs, admissions, initial, acknowledgments, applications, wires atomic.Int64
		written := make(chan sendSequenceId, 2)
		sender.sendBuffer.beforeRunSendSequenceForTest = func(id sendSequenceId) {
			if id.Destination == receiver.ClientId() && id.LogicalLane == 1 {
				runs.Add(1)
				sequence := sender.sendBuffer.lookupSendSequence(id, nil)
				sequence.windowPacer.afterAdmissionForTest = func() { admissions.Add(1) }
			}
		}
		sender.sendBuffer.afterInitialWriteQueuedForTest = func(id sendSequenceId, _ uint64) {
			if id.LogicalLane == 1 {
				initial.Add(1)
				written <- id
			}
		}
		sender.sendBuffer.afterAckCoalescedForTest = func(sendSequenceId, uint64) { acknowledgments.Add(1) }
		sender.sendBuffer.afterApplyAckSnapshotForTest = func(sendSequenceId) { applications.Add(1) }
		sender.sendBuffer.sendBufferSettings.TransferWireMessageObserver = func(TransferWireMessageObservation) { wires.Add(1) }
		o.install(sender, receiver)
		payload := MessagePoolGet(1280)
		ok, sendErr := sender.SendWithTimeoutDetailed(&protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider,
			MessageBytes: payload, Raw: true}, receiver.ClientId(), nil, -1, TransferKey{LogicalLane: 1})
		if !ok {
			MessagePoolReturn(payload)
		}
		if !ok || sendErr != nil {
			t.Fatalf("public settlement admission: accepted=%t error=%v", ok, sendErr)
		}
		var id sendSequenceId
		select {
		case id = <-written:
		case <-ctx.Done():
			t.Fatal("actual initial-write callback did not complete")
		}
		var wire []byte
		select {
		case wire = <-out:
		case <-ctx.Done():
			t.Fatal("actual H1 wire did not arrive")
		}
		func() {
			defer MessagePoolReturn(wire)
			pack := decodeSendPackLifecycleWirePack(t, wire)
			if pack.LogicalLane != 1 {
				t.Fatal("physical wire lost lane identity")
			}
		}()
		synctest.Wait()
		row := o.snapshot().Lanes[0]
		if runs.Load() != 1 || admissions.Load() != 1 || initial.Load() != 1 || wires.Load() != 1 ||
			row.Owner == (Id{}) || row.Original.Sequence != row.Owner || !row.Original.Success || !row.Original.H1 {
			t.Fatalf("actual hooks lost original ownership or chained callbacks: %+v run=%d admission=%d initial=%d wire=%d", row, runs.Load(), admissions.Load(), initial.Load(), wires.Load())
		}
		sequence := sender.sendBuffer.lookupSendSequence(id, nil)
		time.Sleep(1200 * time.Millisecond)
		sequence.coalesceReceivedAck(sequence.ackWindow, receiveAckMessage{messageId: row.Original.Message,
			receivedAtNanos: time.Now().UnixNano(), tag: sequenceTag{set: true, sendTime: row.Original.Tag},
			receiverAckDelaySet: true, ackCompressTimeoutSet: true, ackCompressTimeoutMicros: 10_000})
		synctest.Wait()
		if acknowledgments.Load() != 1 || applications.Load() == 0 || o.snapshot().Error != "" {
			t.Fatalf("actual ACK/Run callbacks were replaced: coalesced=%d applied=%d state=%+v", acknowledgments.Load(), applications.Load(), o.snapshot())
		}
	})
}

func newWindowSettlementDrainFixture(t *testing.T, ctx context.Context, start time.Time) *windowSettlementDrainFixture {
	t.Helper()
	cell := windowLongRingGrowthCell("delivery", 0)
	cell.Settlement = &windowPathSettlementPolicy{Version: "paired-circulation-v1", ReferenceBytes: 12_000_000,
		ReferenceDuration: time.Second, CapBytes: int64(cell.Budget), AckLifetime: DefaultSendBufferSettings().AckTimeout}
	barrier, err := newWindowPathSettlement(ctx, cell, start, cell.Settlement.AckLifetime)
	if err != nil {
		t.Fatal(err)
	}
	f := &windowSettlementDrainFixture{t: t, ctx: ctx, service: newWindowPacingService(DefaultSendBufferSettings()), barrier: barrier}
	barrier.service = f.service
	for lane := range f.sequences {
		sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
			settings.DeliverySizedWindowScale = deliverySizedWindowScale
		})
		sequence.ctx, sequence.cancel = context.WithCancel(ctx)
		sequence.client, sequence.log = &Client{}, NewNoopLogger()
		sequence.sequenceId, sequence.logicalLane = NewId(), uint32(lane+1)
		sequence.ackWindow = newSequenceAckWindow()
		sequence.contractMultiRouteWriter = &windowPacingHandoffWriter{windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{h1Only: true}}}
		sequence.windowPacer = windowBurstPacer{service: f.service, serviceSequenceId: sequence.sequenceId,
			rate: 12_500_000, estimateRate: 12_500_000, afterAdmissionForTest: func() { barrier.capture(time.Now()) }}
		f.sequences[lane] = sequence
		barrier.reading.Lanes[lane].Owner = sequence.sequenceId
	}
	t.Cleanup(func() {
		for _, sequence := range f.sequences {
			sequence.cancel()
			sequence.windowPacer.close()
			sequence.releaseRetainedSendItems(context.Canceled)
		}
	})
	return f
}

func (f *windowSettlementDrainFixture) write(lane int) (*sendItem, error) {
	sequence := f.sequences[lane]
	item := &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: f.next[lane]},
		expectsAck: true, sendCount: 1, sendTime: time.Now(), ackTimeout: sequence.sendBufferSettings.AckTimeout,
		transferFrameBytes: MessagePoolGet(1000)}
	f.next[lane]++
	sequence.resendQueue.Add(item)
	// The component supplies the unchanged 12.5 MB/s carrier input. It does
	// not consult or alter any service estimator or controller checkpoint.
	sequence.windowPacer.rateUpdated = time.Now()
	disposition, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, false, false)
	if err != nil {
		return item, err
	}
	// Mirror only the successful outer initial-write bookkeeping at the
	// processSendPack boundary. The lower writer performs pacing, exact RTT
	// enrollment and H1 completion; it does not set this caller-owned flag.
	item.transportWriteObserved = true
	item.acks.observeTransportWrite(disposition.transportType)
	sequence.writeCount.Add(1)
	sequence.writeByteCount.Add(uint64(len(item.transferFrameBytes)))
	sequence.observeCarrierWrite(item, disposition)
	if disposition.transportType != TransportTypeH1 || !item.transportWriteObserved || !item.rttH1 || item.pacingByteCount != 1000 {
		return item, fmt.Errorf("actual H1/paced original premise failed: disposition=%+v item=%+v", disposition, item)
	}
	// Copy the same confirmed fields as afterInitialWriteQueuedForTest; this
	// direct component owns the item and has no competing Run/pool owner.
	f.barrier.capture(time.Now())
	f.barrier.original(uint32(lane+1), windowPathSettlementOriginal{Sequence: sequence.sequenceId, Message: item.messageId,
		Number: item.sequenceNumber, Tag: uint64(item.sendTime.UnixMilli()), PhysicalAtNanos: item.pacingSentAtNanos,
		Success: item.transportWriteObserved, H1: item.rttH1, SendCount: item.sendCount})
	return item, nil
}

func (f *windowSettlementDrainFixture) acknowledge(lane int, item *sendItem, selective bool) {
	sequence := f.sequences[lane]
	sequence.coalesceReceivedAck(sequence.ackWindow, receiveAckMessage{messageId: item.messageId,
		receivedAtNanos: time.Now().UnixNano(), selective: selective,
		tag: sequenceTag{set: true, sendTime: uint64(item.sendTime.UnixMilli())}, receiverAckDelaySet: true,
		ackCompressTimeoutSet: true, ackCompressTimeoutMicros: 10_000})
	f.barrier.capture(time.Now())
	sequence.ackWindow.ackLock.Lock()
	head, present := sequence.ackWindow.headAck, sequence.ackWindow.hasHeadAck
	sequence.ackWindow.ackLock.Unlock()
	if present {
		f.barrier.cumulative(uint32(lane+1), sequence.sequenceId, head)
	}
}

func (f *windowSettlementDrainFixture) writeTrain() [8]*sendItem {
	f.t.Helper()
	var items [8]*sendItem
	for lane := range items {
		item, err := f.write(lane)
		if err != nil {
			f.t.Fatal(err)
		}
		items[lane] = item
	}
	return items
}

func (f *windowSettlementDrainFixture) acknowledgeTrain(items [8]*sendItem, residence time.Duration) {
	f.t.Helper()
	var latest int64
	for _, item := range items {
		latest = max(latest, item.pacingSentAtNanos)
	}
	if until := time.Until(time.Unix(0, latest).Add(residence)); until > 0 {
		time.Sleep(until)
	}
	for lane, item := range items {
		f.acknowledge(lane, item, false)
	}
}

// The old harness admitted unconditionally at the twelve-second edge. Here
// the actual controller is in a valid drain at that exact edge: warmup has
// supplied a real 1.2s baseline and later paired 1.7s residence, with confirmed
// tails still pending. The test asks the SAME measurement gate used by the
// performance harness, not a reconstructed "drain ought to be active" formula.
func TestWindowPathSettlementWaitsForActualControlledDrain(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		start := time.Now()
		f := newWindowSettlementDrainFixture(t, ctx, start)
		time.Sleep(8 * time.Second)
		opening := f.writeTrain()
		tails := f.writeTrain() // keep flight occupied while the opening returns
		f.acknowledgeTrain(opening, 1200*time.Millisecond)
		late := tails[7]
		if until := time.Until(time.Unix(0, late.pacingSentAtNanos).Add(1700 * time.Millisecond)); until > 0 {
			time.Sleep(until)
		}
		f.acknowledge(7, late, false)
		windowPathAwaitGrid(t, start.Add(11990*time.Millisecond))
		type completion struct {
			item *sendItem
			err  error
		}
		done, joined := make(chan completion, 1), make(chan struct{})
		go func() { defer close(joined); item, err := f.write(7); done <- completion{item, err} }()
		defer func() { cancel(); <-joined }()
		synctest.Wait()
		windowPathAwaitGrid(t, start.Add(12*time.Second))
		state := windowPathCopySettlementState(f.service, time.Now())
		f.service.stateLock.Lock()
		pending, until, started := f.service.pendingWrites, f.service.drainUntil, f.service.drainStartedAt
		f.service.stateLock.Unlock()
		if pending != 7 || !time.Now().Before(until) || !started.After(start.Add(11*time.Second)) || !state.DrainEpoch {
			t.Fatalf("actual unchanged controller did not cross nominal edge: pending=%d start=%s until=%s state=%+v", pending, started.Sub(start), until.Sub(start), state)
		}
		if ready, err := windowPathMeasurementReady(f.barrier, time.Now()); err != nil || ready {
			t.Fatalf("nominal warmup admitted an actual controlled drain: ready=%t error=%v", ready, err)
		}
		windowPathAwaitGrid(t, start.Add(12100*time.Millisecond))
		f.acknowledge(0, tails[0], true)
		synctest.Wait()
		select {
		case result := <-done:
			t.Fatalf("selective tail released physical pause: %+v", result)
		default:
		}
		windowPathAwaitGrid(t, start.Add(12200*time.Millisecond))
		for lane := 0; lane < 7; lane++ {
			f.acknowledge(lane, tails[lane], false)
		}
		result := <-done
		if result.err != nil {
			t.Fatal(result.err)
		}
		synctest.Wait()
		state = windowPathCopySettlementState(f.service, time.Now())
		if !state.DrainUntil.IsZero() || !state.ProbePending {
			t.Fatalf("confirmed final tail did not become a reset probe: %+v", state)
		}
		if ready, err := windowPathMeasurementReady(f.barrier, time.Now()); err != nil || ready {
			t.Fatal("tail completion alone admitted measurement")
		}
		if until := time.Until(time.Unix(0, result.item.pacingSentAtNanos).Add(1200 * time.Millisecond)); until > 0 {
			time.Sleep(until)
		}
		f.acknowledge(7, result.item, false)
		f.barrier.capture(time.Now())
		if ready, err := windowPathMeasurementReady(f.barrier, time.Now()); err != nil || ready {
			t.Fatal("probe ACK alone admitted missing owner circulation")
		}
		// Keep later real tails pending before each prefix returns. Staged
		// send-all/ACK-all trains would repeatedly create cold empty-flight
		// reset probes, which are correctly new epochs rather than settlement.
		time.Sleep(time.Microsecond)
		first := f.writeTrain()
		time.Sleep(100 * time.Millisecond)
		second := f.writeTrain()
		f.acknowledgeTrain(first, 1200*time.Millisecond)
		time.Sleep(time.Microsecond)
		fresh := f.writeTrain()
		f.acknowledgeTrain(second, 1200*time.Millisecond)
		windowPathAwaitGrid(t, start.Add(15*time.Second))
		fourth := f.writeTrain()
		f.acknowledgeTrain(fresh, 1200*time.Millisecond)
		if ready, err := windowPathMeasurementReady(f.barrier, time.Now()); err != nil || ready {
			t.Fatal("new prefix receipts skipped the full observed turn")
		}
		windowPathAwaitGrid(t, start.Add(16*time.Second))
		fifth := f.writeTrain()
		f.acknowledgeTrain(fourth, 1200*time.Millisecond)
		time.Sleep(time.Microsecond)
		sixth := f.writeTrain()
		f.acknowledgeTrain(fifth, 1200*time.Millisecond)
		f.acknowledgeTrain(sixth, 1200*time.Millisecond)
		windowPathAwaitGrid(t, start.Add(18*time.Second))
		if ready, err := windowPathMeasurementReady(f.barrier, time.Now()); err != nil || !ready {
			t.Fatalf("confirmed post-drain circulation failed to admit: ready=%t error=%v state=%+v", ready, err, f.barrier.snapshot())
		}
		t.Logf("nominal=%s actual_drain=%s..%s admitted=%s", 12*time.Second, started.Sub(start), until.Sub(start), f.barrier.snapshot().AdmittedAt.Sub(start))
	})
}
