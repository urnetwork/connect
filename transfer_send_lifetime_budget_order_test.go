// Send sequences of one client share its exact lifetime resend budget. The
// retained bytes an acknowledgement frees belong to the oldest sequence
// waiting for them, so neither the sequence whose acknowledgement freed them
// nor the runtime's wake order decides whose Packs reach the wire.
package connect

import (
	"context"
	"runtime"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

// Real send loops of one client sharing one lifetime budget that retains two
// items. Used only inside a synctest bubble; the test stands in for the
// receiver, so written Packs stay outstanding until it acknowledges them.
type lifetimeBudgetSendFixture struct {
	t         *testing.T
	round     *windowRoundFixture
	budget    *TransferMemoryBudget
	sequences []*SendSequence
	dones     []chan struct{}
	indexes   map[Id]int
	// Written and not yet acknowledged, in write order.
	outstanding []*windowRoundFrame
	writers     []int
}

// Starts the sequences in index order behind a quiescence barrier, so they
// first wait for retained bytes in that order. Each queues backlogCount
// reliable Packs, two of which exceed one wire Pack, so every item holds one
// Pack. The budget is sized for two items once the first backlog is queued,
// and the first sequence then writes both.
func newLifetimeBudgetSendFixture(
	t *testing.T,
	sequenceCount int,
	backlogCount int,
	configure func(index int, settings *SendBufferSettings),
) *lifetimeBudgetSendFixture {
	t.Helper()
	fixture := &lifetimeBudgetSendFixture{
		t:       t,
		round:   newWindowRoundFixture(t, nil, nil),
		budget:  NewTransferMemoryBudget(0),
		indexes: map[Id]int{},
	}
	newPack := func() *SendPack {
		frame := budgetTestFrame(800)
		return &SendPack{
			TransferOptions:  TransferOptions{Ack: true},
			Frame:            frame,
			Ctx:              fixture.round.ctx,
			Destination:      fixture.round.receiver.ClientId(),
			MessageByteCount: ByteCount(len(frame.MessageBytes)),
		}
	}
	for index := range sequenceCount {
		settings := DefaultSendBufferSettings()
		settings.SequenceBufferSize = backlogCount
		settings.ResendQueueBudget = fixture.budget
		settings.ResendQueueRetainedByteAccounting = true
		if configure != nil {
			configure(index, settings)
		}
		sequence := NewSendSequence(fixture.round.ctx, fixture.round.sender, fixture.round.sender.sendBuffer,
			fixture.round.receiver.ClientId(), MultiHopId{}, false, false, false,
			sequenceTlsRoleClient, false, settings)
		for range backlogCount {
			pack := newPack()
			if admitted, err := sequence.Pack(pack, 0); !admitted || err != nil {
				pack.returnFrames()
				t.Fatalf("sequence %d backlog admission = %t, %v", index, admitted, err)
			}
		}
		if index == 0 {
			pack := newPack()
			fixture.budget.SetTotalByteCount(2 * sequence.retainedSendPackByteCount(pack))
			pack.returnFrames()
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			sequence.Run()
		}()
		fixture.sequences = append(fixture.sequences, sequence)
		fixture.dones = append(fixture.dones, done)
		fixture.indexes[sequence.sequenceId] = index
		synctest.Wait()
	}
	if writers := fixture.take(); len(writers) != 2 || writers[0] != 0 || writers[1] != 0 {
		t.Fatalf("sequences that wrote before any acknowledgement = %v, want the first sequence's two items", writers)
	}
	return fixture
}

// Captures every Pack written since the last call, by writer index.
func (self *lifetimeBudgetSendFixture) take() []int {
	self.t.Helper()
	var writers []int
	for len(self.round.senderOut) != 0 {
		frame := self.round.take(self.round.senderOut)
		if frame.pack == nil {
			self.t.Fatal("a sequence wrote something other than a Pack")
		}
		sequenceId, err := IdFromBytes(frame.pack.SequenceId)
		if err != nil {
			self.t.Fatalf("written Pack has no sequence id: %v", err)
		}
		index, ok := self.indexes[sequenceId]
		if !ok {
			self.t.Fatal("a Pack was written by a sequence the test did not start")
		}
		self.outstanding = append(self.outstanding, frame)
		writers = append(writers, index)
	}
	self.writers = append(self.writers, writers...)
	return writers
}

// Acknowledges the oldest outstanding item of sequence index, or of any
// sequence for a negative index, waits for every loop to settle, and returns
// the writers of the Packs that the freed bytes let through.
func (self *lifetimeBudgetSendFixture) acknowledge(index int) []int {
	self.t.Helper()
	for position, frame := range self.outstanding {
		sequenceId, _ := IdFromBytes(frame.pack.SequenceId)
		if 0 <= index && self.indexes[sequenceId] != index {
			continue
		}
		self.outstanding = append(self.outstanding[:position:position], self.outstanding[position+1:]...)
		ack := &protocol.Ack{MessageId: frame.pack.MessageId, SequenceId: frame.pack.SequenceId}
		if accepted, err := self.sequences[self.indexes[sequenceId]].Ack(ack, 0); !accepted || err != nil {
			self.t.Fatalf("acknowledgement of a written item = %t, %v", accepted, err)
		}
		synctest.Wait()
		return self.take()
	}
	self.t.Fatalf("sequence %d has no outstanding item to acknowledge", index)
	return nil
}

// Closes every sequence and checks that their items, places in line and
// reserved turns all returned to the budget.
func (self *lifetimeBudgetSendFixture) close() {
	self.t.Helper()
	for _, sequence := range self.sequences {
		sequence.Close()
	}
	for _, done := range self.dones {
		<-done
	}
	assertRetainedBudgetBalance(self.t, self.budget)
}

// The root cause of one lane taking every byte of a shared lifetime budget.
// Three sequences of one client each hold a backlog of reliable Packs and
// share a budget that retains two items. The test acknowledges the oldest
// written item one at a time, and each sequence must get an equal share of
// the items the acknowledgements free. A sequence applies its own
// acknowledgement in its send loop, and the loop used to release the item's
// bytes, wake every blocked sibling and, in the same pass, take the bytes back
// for its next Pack before any sibling ran: the first sequence wrote every
// freed item and the other two none.
func TestSendSequencesSharingALifetimeBudgetTakeTurnsAtItsReleases(t *testing.T) {
	assertMessagePoolOwnership(t)
	// Keep the process-global pool statistics worker outside the bubble.
	MessagePoolReturn(MessagePoolGet(1))
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	synctest.Test(t, func(t *testing.T) {
		const sequenceCount = 3
		const ackCount = 12
		fixture := newLifetimeBudgetSendFixture(t, sequenceCount, 16, nil)
		writeCounts := make([]int, sequenceCount)
		for range ackCount {
			// One freed item lets exactly one Pack through.
			writers := fixture.acknowledge(-1)
			if len(writers) != 1 {
				t.Fatalf("one freed item let %d Packs reach the wire, want exactly 1: %v", len(writers), writers)
			}
			writeCounts[writers[0]] += 1
		}
		fixture.close()

		t.Logf("writers in order: %v", fixture.writers)
		for index, count := range writeCounts {
			if count != ackCount/sequenceCount {
				t.Errorf("sequence %d wrote %d of the %d items acknowledgements freed, want %d each: %v", index, count, ackCount, ackCount/sequenceCount, writeCounts)
			}
		}
	})
}

// A sequence that closes while it waits leaves the line, so the next release
// serves the sequence behind it rather than a place nobody will take.
func TestSendSequenceThatClosesWhileWaitingLeavesTheLifetimeBudgetLine(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newLifetimeBudgetSendFixture(t, 3, 4, nil)
		// The first sequence waited first, so the first release is its own.
		if writers := fixture.acknowledge(0); len(writers) != 1 || writers[0] != 0 {
			t.Fatalf("Packs written after the first release = %v, want the first sequence's", writers)
		}
		fixture.sequences[1].Cancel()
		<-fixture.dones[1]
		if writers := fixture.acknowledge(0); len(writers) != 1 || writers[0] != 2 {
			t.Fatalf("Packs written after the release behind a closed waiter = %v, want the third sequence's", writers)
		}
		fixture.close()
	})
}

// An idle sequence only watches capacity for producer admission. Without a
// reliable Pack to retain it must not take a release ahead of an actual waiter.
func TestIdleSendSequenceDoesNotReserveAReleasedLifetimeBudgetTurn(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newWindowRoundFixture(t, nil, nil)
		budget := NewTransferMemoryBudget(kib(64))
		if !budget.TryReserve(budget.TotalByteCount()) {
			t.Fatal("fixture could not fill the shared budget")
		}
		settings := DefaultSendBufferSettings()
		settings.ResendQueueBudget = budget
		settings.ResendQueueRetainedByteAccounting = true
		sequence := NewSendSequence(fixture.ctx, fixture.sender, fixture.sender.sendBuffer,
			fixture.receiver.ClientId(), MultiHopId{}, false, false, false,
			sequenceTlsRoleClient, false, settings)
		done := make(chan struct{})
		go func() {
			defer close(done)
			sequence.Run()
		}()
		synctest.Wait()

		reserved, waiter := budget.tryReserveOrQueue(budget.TotalByteCount())
		if reserved || waiter == nil {
			t.Fatal("fixture's live writer did not wait behind the full budget")
		}
		budget.Release(budget.TotalByteCount())
		// A release assigns its turn synchronously, before any waiter runs.
		// The idle sender used to consume its minimum retained-item grant here,
		// leaving too few bytes for this real writer until it ran again.
		select {
		case <-waiter.ready:
		default:
			t.Error("an idle sequence took a released lifetime-budget turn from a live writer")
		}
		waiter.abandon()
		sequence.Close()
		<-done
		assertRetainedBudgetBalance(t, budget)
	})
}

// A sequence that its own send window holds when its turn comes cannot use
// the bytes reserved for it. It passes them on at once, so the turn does not
// idle while the next sequence in line could write.
func TestSendSequenceHeldByItsWindowPassesItsLifetimeBudgetTurnOn(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	synctest.Test(t, func(t *testing.T) {
		// The second sequence's window takes one item at a time.
		fixture := newLifetimeBudgetSendFixture(t, 3, 8, func(index int, settings *SendBufferSettings) {
			if index == 1 {
				settings.ResendQueueMaxByteCount = 1
			}
		})
		// Turns in arrival order fill the window of the second sequence.
		for index, want := range []int{0, 1, 2, 0} {
			acknowledged := -1
			if index == 3 {
				// Leave the second sequence's item outstanding. It is now
				// the oldest item, so acknowledging any item would free its
				// window and erase the condition this test needs to exercise.
				acknowledged = 2
			}
			if writers := fixture.acknowledge(acknowledged); len(writers) != 1 || writers[0] != want {
				t.Fatalf("Packs written in turn = %v, want sequence %d's", writers, want)
			}
		}
		// The second sequence's turn comes while its only item is still
		// outstanding. The bytes must reach the third sequence at once.
		if writers := fixture.acknowledge(0); len(writers) != 1 || writers[0] != 2 {
			t.Fatalf("Packs written after a window-held turn = %v, want the third sequence's", writers)
		}
		fixture.close()
	})
}

// A reliable producer arriving after the shared budget fills waits before
// scheduler admission. It must join the same FIFO line as already queued Packs;
// otherwise its siblings recirculate every release and it never gets admitted.
func TestLateReliableProducerJoinsLifetimeBudgetLine(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newLifetimeBudgetSendFixture(t, 2, 16, nil)
		settings := DefaultSendBufferSettings()
		settings.ResendQueueBudget = fixture.budget
		settings.ResendQueueRetainedByteAccounting = true
		sequence := NewSendSequence(fixture.round.ctx, fixture.round.sender, fixture.round.sender.sendBuffer,
			fixture.round.receiver.ClientId(), MultiHopId{}, false, false, false,
			sequenceTlsRoleClient, false, settings)
		done := make(chan struct{})
		go func() {
			defer close(done)
			sequence.Run()
		}()
		fixture.sequences = append(fixture.sequences, sequence)
		fixture.dones = append(fixture.dones, done)
		fixture.indexes[sequence.sequenceId] = 2
		synctest.Wait()

		frame := budgetTestFrame(800)
		pack := &SendPack{
			TransferOptions: TransferOptions{Ack: true},
			Frame:           frame, Ctx: fixture.round.ctx,
			Destination:      fixture.round.receiver.ClientId(),
			MessageByteCount: ByteCount(len(frame.MessageBytes)),
		}
		producerDone := make(chan struct{})
		go func() {
			defer close(producerDone)
			if admitted, _ := sequence.Pack(pack, -1); !admitted {
				pack.returnFrames()
			}
		}()
		synctest.Wait()
		if sequence.resendCapacityUnavailable.waiterCount.Load() != 1 {
			t.Error("late producer did not reach the closed reliable admission gate")
		}
		wrote := false
		for range 6 {
			for _, writer := range fixture.acknowledge(-1) {
				wrote = wrote || writer == 2
			}
		}
		fixture.close()
		<-producerDone
		if !wrote {
			t.Errorf("late reliable producer never joined the shared budget line: writers %v", fixture.writers)
		}
	})
}

// A producer's cancellation removes its real demand, including a turn already
// awarded by the shared budget but not yet used for scheduler admission.
func TestCanceledReliableProducerReturnsItsLifetimeBudgetTurn(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	for _, awarded := range []bool{false, true} {
		name := "waiting"
		if awarded {
			name = "awarded"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := newWindowRoundFixture(t, nil, nil)
				budget := NewTransferMemoryBudget(kib(64))
				if !budget.TryReserve(budget.TotalByteCount()) {
					t.Fatal("fixture could not fill the shared budget")
				}
				settings := DefaultSendBufferSettings()
				settings.ResendQueueBudget = budget
				settings.ResendQueueRetainedByteAccounting = true
				sequence := NewSendSequence(fixture.ctx, fixture.sender, fixture.sender.sendBuffer,
					fixture.receiver.ClientId(), MultiHopId{}, false, false, false,
					sequenceTlsRoleClient, false, settings)
				done := make(chan struct{})
				go func() {
					defer close(done)
					sequence.Run()
				}()
				synctest.Wait()

				ctx, cancel := context.WithCancel(fixture.ctx)
				frame := budgetTestFrame(800)
				pack := &SendPack{
					TransferOptions: TransferOptions{Ack: true},
					Frame:           frame, Ctx: ctx,
					Destination:      fixture.receiver.ClientId(),
					MessageByteCount: ByteCount(len(frame.MessageBytes)),
				}
				producerDone := make(chan struct{})
				go func() {
					defer close(producerDone)
					if admitted, _ := sequence.Pack(pack, -1); !admitted {
						pack.returnFrames()
					}
				}()
				synctest.Wait()
				if count := len(budget.reserveWaiters); count != 1 {
					t.Errorf("blocked producer owns %d budget waiters, want 1", count)
				}
				if awarded {
					budget.Release(budget.TotalByteCount())
				}
				cancel()
				<-producerDone
				synctest.Wait()
				if !awarded {
					budget.Release(budget.TotalByteCount())
					synctest.Wait()
				}
				if available := budget.Available(); available != budget.TotalByteCount() {
					t.Errorf("canceled producer left %d bytes unavailable", budget.TotalByteCount()-available)
				}
				if count := len(budget.reserveWaiters); count != 0 {
					t.Errorf("canceled producer left %d budget waiters", count)
				}
				sequence.Close()
				<-done
				assertRetainedBudgetBalance(t, budget)
			})
		})
	}
}
