// Direct ownership controls for cached source controls; no timer or network fixture.
package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"
	"weak"

	"github.com/urnetwork/connect/protocol"
)

func retainedControlTestQueue() *transferQueue[*transferItem] {
	return newTransferQueue[*transferItem](func(a, b *transferItem) int {
		if a.SequenceNumber() < b.SequenceNumber() {
			return -1
		}
		if b.SequenceNumber() < a.SequenceNumber() {
			return 1
		}
		return 0
	})
}

func retainedControlTestSendQueue() *transferQueue[*sendItem] {
	return newTransferQueue[*sendItem](func(a, b *sendItem) int {
		if a.SequenceNumber() < b.SequenceNumber() {
			return -1
		}
		if b.SequenceNumber() < a.SequenceNumber() {
			return 1
		}
		return 0
	})
}

// Held control bytes compete with ordinary and prepared admission. Removal of
// live data reopens control credit without a separately mirrored queue count.
func TestTransferRetainedControlSharesLegacyWindow(t *testing.T) {
	queue := retainedControlTestQueue()
	item := &transferItem{messageId: NewId(), sequenceNumber: 1, messageByteCount: 40}
	queue.Add(item)
	credit, ok := queue.tryReserveRetainedControl(50, 100, nil)
	if !ok || credit == nil || queue.CanAdd(10, 100) || !queue.CanAdd(9, 100) {
		t.Fatal("retained control did not reduce the same ordinary local window")
	}
	defer credit.Release()
	if _, ok := queue.tryReserveRetainedControl(11, 100, nil); ok {
		t.Fatal("control reservation overran live queue plus existing credit")
	}
	tail, ok := queue.tryReserveRetainedControl(10, 100, nil)
	if !ok {
		t.Fatal("exact remaining local permission was refused")
	}
	defer tail.Release()
	if queue.CanAdd(0, 100) {
		t.Fatal("ordinary admission ignored full retained control credit")
	}
	// Prepared custody requires a matching nonnil budget and is never selected
	// by this legacy queue. Preserve its original protocol-only predicate.
	if !queue.preparedWindowAvailable(100) {
		t.Fatal("legacy control changed unrelated prepared-window semantics")
	}
	if queue.RemoveByMessageId(item.messageId) != item {
		t.Fatal("remove did not return the queued data owner")
	}
	if !queue.preparedWindowAvailable(100) {
		t.Fatal("data release did not reopen the shared local window")
	}
	if _, ok := queue.tryReserveRetainedControl(41, 100, nil); ok {
		t.Fatal("live credit was forgotten when data drained")
	}
	last, ok := queue.tryReserveRetainedControl(40, 100, nil)
	if !ok {
		t.Fatal("control did not observe current zero queued occupancy")
	}
	last.Release()
	tail.Release()
	credit.Release()
	if !queue.preparedWindowAvailable(1) || queue.retainedControl.retainedByteCount != 0 {
		t.Fatal("final control releases did not restore local permission")
	}
}

// Legacy data can still put its one item into an otherwise empty queue,
// including its historical approximate overdraw. New controls cannot use it.
func TestTransferRetainedControlPreservesLegacyEmptyItemException(t *testing.T) {
	queue := retainedControlTestQueue()
	credit, ok := queue.tryReserveRetainedControl(100, 100, nil)
	if !ok || !queue.CanAdd(500, 100) {
		t.Fatal("legacy empty-queue one-item admission was changed")
	}
	defer credit.Release()
	if _, ok := queue.tryReserveRetainedControl(1, 100, nil); ok {
		t.Fatal("new control used the legacy empty-item exception")
	}
	queue.Add(&transferItem{messageId: NewId(), sequenceNumber: 1, messageByteCount: 500})
	if queue.CanAdd(0, 100) || queue.preparedWindowAvailable(100) {
		t.Fatal("legacy overdraw admitted a second queued item")
	}
	if items := queue.Clear(); len(items) != 1 {
		t.Fatal("clear lost the admitted ordinary item")
	}
	if queue.retainedControl.retainedByteCount != 100 {
		t.Fatal("clear prematurely returned live retained control permission")
	}
	credit.Release()
	if _, ok := queue.tryReserveRetainedControl(1, 100, nil); ok {
		t.Fatal("cleared source accepted a new retained control")
	}
	if !queue.CanAdd(500, 100) {
		t.Fatal("control retirement changed legacy data queue semantics")
	}
}

// Shared admission charges full copied-packet backing and the credit envelope,
// through the actual parent. Neither a floor nor empty queue waives that cost.
func TestTransferRetainedControlSourceChargesFullBacking(t *testing.T) {
	required := retainedMessageCapacity(257) + 512
	overhead := transferRetainedControlCreditByteCount()
	if overhead < ByteCount(unsafe.Sizeof(transferRetainedControlCredit{}))+ByteCount(unsafe.Sizeof(transferQueueRetainedControl{})) {
		t.Fatal("credit envelope omits its own allocation backing")
	}
	total := required + overhead
	root := NewTransferMemoryBudget(total - 1)
	budget := NewTransferMemoryBudgetWithParent(total, root)
	queue := retainedControlTestSendQueue()
	queue.setBudget(budget, total)
	source := &SendSequence{ctx: context.Background(), resendQueue: queue}
	if _, ok := source.reserveRetainedControlMemory(required); ok || budget.UsedByteCount() != 0 || root.UsedByteCount() != 0 {
		t.Fatal("retained control escaped the exact parent cap through its floor")
	}
	root.SetTotalByteCount(total)
	credit, ok := source.reserveRetainedControlMemory(required)
	if !ok || credit == nil || budget.UsedByteCount() != total || root.UsedByteCount() != total {
		t.Fatal("full copied backing plus credit metadata was not reserved")
	}
	if budget.TryReserve(1) {
		t.Fatal("control admission left uncharged ownership")
	}
	credit.Release()
	credit.Release()
	if budget.UsedByteCount() != 0 || root.UsedByteCount() != 0 {
		t.Fatal("final shared credit return is not exact-once")
	}
	for _, b := range []*TransferMemoryBudget{budget, root} {
		reserved, released := b.Counts()
		if reserved != total || released != total {
			t.Fatal("retained control reserve/release totals do not balance")
		}
	}
	queue.Clear()
}

// Queue teardown returns only queue membership; a late cache/worker owner is
// charged until its final release, and cannot reopen control admission.
func TestTransferRetainedControlClearAndLateRelease(t *testing.T) {
	budget := NewTransferMemoryBudget(100)
	queue := retainedControlTestQueue()
	queue.setBudget(budget, 0)
	queue.Add(&transferItem{messageId: NewId(), sequenceNumber: 1, messageByteCount: 40})
	credit, ok := queue.tryReserveRetainedControl(60, 100, nil)
	if !ok || budget.UsedByteCount() != 100 {
		t.Fatal("queued and retained owners did not share budget permission")
	}
	queue.Clear()
	if budget.UsedByteCount() != 60 {
		t.Fatal("clear released a still-live retained control")
	}
	credit.Release()
	if budget.UsedByteCount() != 0 {
		t.Fatal("late release lost the detached budget reservation")
	}
	if _, ok := queue.tryReserveRetainedControl(1, 100, nil); ok {
		t.Fatal("retired queue accepted a new control lease")
	}
}

// Cancellation refuses fresh custody, while an already admitted owner remains
// charged through queue clear until its existing references are disposed.
func TestTransferRetainedControlSourceCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	total := ByteCount(100) + transferRetainedControlCreditByteCount()
	budget := NewTransferMemoryBudget(total)
	queue := retainedControlTestSendQueue()
	queue.setBudget(budget, 0)
	source := &SendSequence{ctx: ctx, resendQueue: queue}
	credit, ok := source.reserveRetainedControlMemory(100)
	if !ok {
		t.Fatal("live source did not acquire control permission")
	}
	cancel()
	if _, ok := source.reserveRetainedControlMemory(1); ok {
		t.Fatal("canceled source acquired fresh control permission")
	}
	queue.Clear()
	if budget.UsedByteCount() != total {
		t.Fatal("cancel/clear released an owner still held by its worker")
	}
	credit.Release()
	if budget.UsedByteCount() != 0 {
		t.Fatal("canceled source leaked final control credit")
	}
}

// The only surviving strong reference is the lease. Neither source nor queue
// can remain live through that thin independent ledger.
func retainedControlGraphWitness(t *testing.T) (*transferRetainedControlCredit, weak.Pointer[SendSequence], weak.Pointer[transferQueue[*sendItem]]) {
	t.Helper()
	queue := retainedControlTestSendQueue()
	source := &SendSequence{ctx: context.Background(), resendQueue: queue,
		sendBufferSettings:    &SendBufferSettings{ResendQueueMaxByteCount: 4096},
		retainedAdmissionWake: make(chan struct{}, 1)}
	credit, ok := source.reserveRetainedControlMemory(100)
	if !ok || credit.local == nil || credit.budget != nil {
		t.Fatal("legacy witness did not retain independent local credit")
	}
	return credit, weak.Make(source), weak.Make(queue)
}

func TestTransferRetainedControlLeaseDoesNotRetainSourceGraph(t *testing.T) {
	credit, source, queue := retainedControlGraphWitness(t)
	defer credit.Release()
	runtime.GC()
	if source.Value() != nil || queue.Value() != nil {
		t.Fatal("retained control credit pins the source/queue graph")
	}
	credit.Release()
	runtime.KeepAlive(credit)
}

// Final cleanup may be observed twice; only one caller may return permission.
// A shared start barrier exercises the release edge without scheduler polling.
func TestTransferRetainedControlConcurrentFinalRelease(t *testing.T) {
	for _, shared := range []bool{false, true} {
		queue := retainedControlTestQueue()
		var budget *TransferMemoryBudget
		if shared {
			budget = NewTransferMemoryBudget(100)
			queue.setBudget(budget, 0)
		}
		credit, ok := queue.tryReserveRetainedControl(100, 100, nil)
		if !ok {
			t.Fatal("release fixture did not acquire exact credit")
		}
		start := make(chan struct{})
		var joined sync.WaitGroup
		for range 8 {
			joined.Add(1)
			go func() {
				defer joined.Done()
				<-start
				credit.Release()
			}()
		}
		close(start)
		joined.Wait()
		if shared {
			reserved, released := budget.Counts()
			if reserved != 100 || released != 100 || budget.UsedByteCount() != 0 {
				t.Fatal("concurrent shared release returned credit more than once")
			}
		} else if queue.retainedControl.retainedByteCount != 0 {
			t.Fatal("concurrent local release returned credit more than once")
		}
		queue.Clear()
	}
}

// Invalid/overflowed caller ownership must not turn into a saturated but smaller
// reservation, even when a directly constructed shared budget admits MaxInt64.
func TestTransferRetainedControlRejectsOverflowAndInvalidAdmission(t *testing.T) {
	queue := retainedControlTestSendQueue()
	budget := NewTransferMemoryBudget(math.MaxInt64)
	queue.setBudget(budget, 0)
	source := &SendSequence{ctx: context.Background(), resendQueue: queue}
	for _, required := range []ByteCount{-1, 0, math.MaxInt64} {
		if _, ok := source.reserveRetainedControlMemory(required); ok || budget.UsedByteCount() != 0 {
			t.Fatal("invalid retained-control size acquired shared permission")
		}
	}
	local := retainedControlTestQueue()
	if _, ok := local.tryReserveRetainedControl(1, 0, nil); ok || local.retainedControl != nil {
		t.Fatal("failed zero-cap admission allocated a local ledger")
	}
	local.Clear()
	queue.Clear()
}

// A real Raw IP send takes caller ownership only on successful public admission.
func retainedControlPublicPacket(t *testing.T, fixture *windowRoundFixture, port uint16, completed *atomic.Int32, timeout time.Duration) (bool, error) {
	t.Helper()
	packet := MessagePoolGet(40)
	clear(packet)
	packet[0], packet[8], packet[9] = 0x45, 64, 6
	binary.BigEndian.PutUint16(packet[2:4], 40)
	copy(packet[12:16], []byte{10, 0, 0, 2})
	copy(packet[16:20], []byte{192, 0, 2, 9})
	binary.BigEndian.PutUint16(packet[20:22], port)
	binary.BigEndian.PutUint16(packet[22:24], 443)
	packet[32], packet[33] = 0x50, 0x02
	binary.BigEndian.PutUint16(packet[34:36], 65535)
	frame := RequireToFrameWithDefaultProtocolVersion(&protocol.IpPacketToProvider{
		IpPacket: &protocol.IpPacket{PacketBytes: packet},
	})
	if !frame.Raw {
		MessagePoolReturn(packet)
		t.Fatal("public control fixture requires current Raw packet ownership")
	}
	accepted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(), func(err error) {
		if err != nil {
			t.Errorf("healthy peer completion: %v", err)
		} else {
			completed.Add(1)
		}
	}, timeout)
	if !accepted {
		MessagePoolReturn(packet)
	}
	return accepted, err
}

// One actual non-retained source shared by independently keyed Raw TCP flows.
func retainedControlPublicSource(t *testing.T, budget *TransferMemoryBudget) (*windowRoundFixture, *SendSequence, *atomic.Int32, *windowRoundFrame) {
	t.Helper()
	fixture := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
		settings.ResendQueueBudget = budget
		settings.ResendQueueRetainedByteAccounting = false
		settings.ResendQueueMaxByteCount = 8192
		settings.ResendQueueMinByteCount = 8192
		if budget != nil {
			settings.ResendQueueMinByteCount = 0
		}
		settings.LogicalDataLaneCount = 0
	}, nil)
	completed := &atomic.Int32{}
	// Establish the exact source with the first TCP flow and return its ACK.
	if accepted, err := retainedControlPublicPacket(t, fixture, 40001, completed, time.Second); !accepted || err != nil {
		t.Fatalf("initial TCP flow admission: %t %v", accepted, err)
	}
	first := fixture.takePack(0)
	fixture.forward(first, fixture.receiverIn)
	fixture.acknowledge()
	var source *SendSequence
	fixture.sender.sendBuffer.mutex.Lock()
	count := len(fixture.sender.sendBuffer.sendSequences)
	for _, candidate := range fixture.sender.sendBuffer.sendSequences {
		source = candidate
	}
	fixture.sender.sendBuffer.mutex.Unlock()
	if count != 1 || source == nil || source.resendQueue.Budget() != budget || source.resendQueue.lifetimeBudget {
		t.Fatal("public fixture does not share one actual matching-budget legacy source")
	}
	if source.preparedPackFits(&SendPack{}) {
		t.Fatal("legacy source incorrectly selects shared prepared custody")
	}
	return fixture, source, completed, first
}

// Full control custody must not strand another TCP flow behind a prepared gate:
// nil-budget legacy sources cannot select that gate and keep one-item progress.
func TestTransferRetainedControlFullLegacyCreditKeepsOtherFlowRunnable(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, source, completed, first := retainedControlPublicSource(t, nil)
		maximum := source.sendWindowSnapshot(time.Now()).Window
		credit, ok := source.resendQueue.tryReserveRetainedControl(maximum, maximum, source.retainedAdmissionWake)
		if !ok {
			t.Fatal("could not fill the empty local window with exact retained credit")
		}
		defer credit.Release()
		if source.resendQueue.preparedWindowAvailable(maximum) != true || !source.resendQueue.CanAdd(0, maximum) {
			t.Fatal("full credit changed the legacy empty-source progress premise")
		}
		// Different five-tuple, same actual source queue, no clock advance/reoffer.
		if accepted, err := retainedControlPublicPacket(t, fixture, 40002, completed, 0); !accepted || err != nil {
			t.Fatalf("healthy other TCP flow was stranded by full control credit: %t %v", accepted, err)
		}
		healthy := fixture.takePack(1)
		if !bytes.Equal(first.pack.SequenceId, healthy.pack.SequenceId) {
			t.Fatal("healthy flow bypassed the tested source queue")
		}
		if accepted, err := retainedControlPublicPacket(t, fixture, 40003, completed, 0); accepted || err != nil {
			t.Fatalf("legacy one-item exception admitted a second queued item: %t %v", accepted, err)
		}
		fixture.forward(healthy, fixture.receiverIn)
		fixture.acknowledge()
		if completed.Load() != 2 || fixture.deliveredCount != 2 || source.resendQueue.retainedControl.retainedByteCount != maximum {
			t.Fatal("healthy delivery/ACK required consuming retained control permission")
		}
	})
}

// A final control release wakes the already-blocked public producer through the
// immutable constructor wake. No ACK, timer advancement or repeated offer helps.
func retainedControlReleaseWakesBlockedFlow(t *testing.T, budget *TransferMemoryBudget) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, source, completed, first := retainedControlPublicSource(t, budget)
		maximum := source.sendWindowSnapshot(time.Now()).Window
		if source.retainedAdmissionWake == nil || cap(source.retainedAdmissionWake) != 1 {
			t.Fatal("actual legacy constructor did not publish its immutable bounded wake")
		}
		credit, ok := source.resendQueue.tryReserveRetainedControl(maximum, maximum, source.retainedAdmissionWake)
		if !ok {
			t.Fatal("local control did not acquire the exact existing window")
		}
		defer credit.Release()
		if accepted, err := retainedControlPublicPacket(t, fixture, 40002, completed, 0); !accepted || err != nil {
			t.Fatalf("first empty-queue healthy flow did not progress: %t %v", accepted, err)
		}
		second := fixture.takePack(1)
		// Hold its wire and ACK. A third flow is now an actual waiting public
		// caller; the one-item exception cannot admit it into this nonempty queue.
		type result struct {
			accepted bool
			err      error
		}
		returned := make(chan result, 1)
		go func() {
			accepted, err := retainedControlPublicPacket(t, fixture, 40003, completed, time.Second)
			returned <- result{accepted: accepted, err: err}
		}()
		before := time.Now()
		synctest.Wait()
		select {
		case outcome := <-returned:
			t.Fatalf("full nonempty source did not keep the third caller waiting: %+v", outcome)
		default:
		}
		// A second quiescence barrier verifies that the existing producer demand
		// notification does not create a self-sustaining legacy wake loop.
		synctest.Wait()
		if !time.Now().Equal(before) {
			t.Fatal("waiting premise depended on advancing an existing retry timer")
		}
		credit.Release()
		synctest.Wait()
		select {
		case outcome := <-returned:
			if !outcome.accepted || outcome.err != nil {
				t.Fatalf("released local credit did not admit the waiting flow: %+v", outcome)
			}
		default:
			t.Fatal("retained control release did not wake blocked source admission")
		}
		if !time.Now().Equal(before) {
			t.Fatal("credit recovery waited for an ACK/resend timer")
		}
		third := fixture.takePack(2)
		if !bytes.Equal(first.pack.SequenceId, second.pack.SequenceId) || !bytes.Equal(first.pack.SequenceId, third.pack.SequenceId) {
			t.Fatal("healthy flow escaped the source queue being tested")
		}
		fixture.forward(second, fixture.receiverIn)
		fixture.acknowledge()
		fixture.forward(third, fixture.receiverIn)
		fixture.acknowledge()
		if completed.Load() != 3 || fixture.deliveredCount != 3 {
			t.Fatal("wake-based progress lost an ordinary flow or terminal completion")
		}
	})
}

func TestTransferRetainedControlReleaseWakesBlockedOtherFlow(t *testing.T) {
	retainedControlReleaseWakesBlockedFlow(t, nil)
}

// Negotiated legacy lanes can use a flat shared pool without retained mode.
// They need the same directed edge; global budget notifications are not armed.
func TestTransferRetainedControlReleaseWakesBlockedFlatBudgetFlow(t *testing.T) {
	budget := NewTransferMemoryBudget(8192)
	retainedControlReleaseWakesBlockedFlow(t, budget)
	reserved, released := budget.Counts()
	if budget.UsedByteCount() != 0 || reserved != released {
		t.Fatal("flat-budget public progress left unbalanced retained ownership")
	}
}
