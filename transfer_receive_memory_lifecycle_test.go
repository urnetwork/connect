// Portable lifecycle controls preserve real source retries and terminal ownership.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// The complete owned path returns local encoded permission as well as its
// physical roots; this new quota API is intentionally outside the old witness.
func joinReceiveMemoryLifecycleOwnedFlow(t *testing.T, flow *receiveMemoryLifecycleFlow) {
	t.Helper()
	joinReceiveMemoryLifecycleFlow(t, flow)
	if budget := flow.fixture.receiver.receiveBuffer.receiveBufferSettings.PackQueueBudget; budget != nil && budget.Stats().QuotaByteCount != 0 {
		t.Fatal("terminal receive handoff leaked encoded quota")
	}
}

// Pack refusal is ordinary no-ACK loss with exact same-identity natural retry.
// Actual roots at refusal are observed, then their physical return is checked.
func TestReceiveMemoryRefusalReturnsDecodedRoots(t *testing.T) {
	settings := receiveMemoryLifecycleTestSettings()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		rx, pack, client, root := receiveMemoryLifecycleBudgets(t, settings)
		flow := newReceiveMemoryLifecycleFlow(t, settings, rx, pack)
		flow.warm(t)
		sourceSequence := flow.fixture.sequence()
		sourceKey, sourceNumber := sourceSequence.id(), flow.fixture.nextNumber
		type sourceState struct {
			item          *sendItem // Borrowed only while the source owner is parked.
			messageId     Id
			number        uint64
			due, deadline time.Time
			selective     bool
		}
		sourceEntered := make(chan sourceState, 1)
		sourceResume := make(chan struct{})
		releaseSource := func() {
			select {
			case <-sourceResume:
			default:
				close(sourceResume)
			}
		}
		t.Cleanup(releaseSource)
		if sourceSequence.sendBuffer.afterInitialWriteQueuedForTest != nil {
			t.Fatal("refusal fixture already owns the initial-write hook")
		}
		// The genuine warm ACK retired its source before this installation.
		// Background key sends are parked; the next real Pack handoff publishes
		// this hook before its next read. The two channels order both directions
		// of the borrowed source-state inspection across natural timer recovery.
		sourceSequence.sendBuffer.afterInitialWriteQueuedForTest = func(key sendSequenceId, number uint64) {
			if key != sourceKey || number != sourceNumber {
				return
			}
			state := sourceState{number: number}
			state.item = sourceSequence.resendQueue.GetBySequenceNumber(number)
			if state.item != nil {
				state.messageId = state.item.messageId
				state.due = state.item.resendTime
				state.deadline = sourceSequence.ackLifetimeDeadline(state.item)
				state.selective = state.item.selectiveAcked
			}
			sourceEntered <- state
			select {
			case <-sourceResume:
			case <-flow.fixture.ctx.Done():
			}
		}
		resident := flow.fixture.receiveSequence()
		receiveSettings := flow.fixture.receiver.settings.ReceiveBufferSettings
		receiveSettings.ReliablePackHandoffTimeout = 0
		receiveSettings.H1PackHandoffTimeout = 0
		gate := newReceiveMemoryLifecycleGate(t, flow, root, client, rx, pack, "handoff_refused")
		wire := flow.group(t, 8)
		gate.before(t, wire)
		id := gate.target
		flow.retryTarget = id
		flow.retryNumber = wire.pack.SequenceNumber
		var sourceSnapshot sourceState
		select {
		case sourceSnapshot = <-sourceEntered:
		default:
			t.Fatal("refusal source did not park after its actual initial write")
		}
		item := sourceSnapshot.item
		if item == nil || sourceSnapshot.messageId != id || sourceSnapshot.number != wire.pack.SequenceNumber || sourceSnapshot.selective ||
			sourceSequence.resendQueue.GetByMessageId(id) != item {
			t.Fatal("refusal test lacks the exact parked retained source owner")
		}
		due, deadline := sourceSnapshot.due, sourceSnapshot.deadline
		foreign := pack.TotalByteCount()
		if !pack.TryReserve(foreign) {
			t.Fatal("could not saturate exact Pack leaf")
		}
		t.Cleanup(func() {
			if foreign != 0 {
				pack.Release(foreign)
			}
		})
		offeredWireBytes := MessagePoolRootByteCount(wire.bytes)
		copiedWrite := DebugTransferCopyOnWrite
		// connect_test.go enables independent outgoing copies. The before
		// snapshot includes this exact wire root, which refusal must return.
		if !copiedWrite || offeredWireBytes <= 0 || gate.beforeRoots < 1 || gate.beforeBytes < offeredWireBytes {
			t.Fatal("refusal fixture requires a separately copied outgoing wire root")
		}
		flow.fixture.forward(wire, flow.fixture.receiverIn)
		synctest.Wait()
		observed := false
		select {
		case sample := <-gate.stages[0].Entered:
			observed = true
			assertReceiveMemoryLifecycleOwner(t, sample)
			if sample.Pack != foreign || sample.PackClaim != 0 || len(flow.fixture.receiverOut) != 0 {
				t.Fatal("refused copy took handoff quota or gained an acknowledgement")
			}
			close(gate.stages[0].Resume)
			synctest.Wait()
		default:
			// A fixed path may refuse before allocating/handing off a decoded
			// owner. Net ownership and actual loss/retry remain authoritative.
		}
		roots, bytes := messagePoolOutstandingSnapshot()
		if roots != gate.beforeRoots-1 || bytes != gate.beforeBytes-offeredWireBytes || root.UsedByteCount() != foreign || resident.ctx.Err() != nil || flow.fixture.receiveSequence() != resident || flow.fixture.receiver.ReceiveStats().PackHandoffDropCount != 1 ||
			flow.fixture.deliveredCount != 8 || flow.fixture.ackedCount != 1 || len(flow.fixture.receiverOut) != 0 || flow.fixture.sequence().resendQueue.GetByMessageId(id) != item || item.selectiveAcked {
			t.Fatal("safe Pack refusal lost decoded/source ownership or closed its resident")
		}
		pack.Release(foreign)
		foreign = 0
		releaseSource()
		bound := due.Add(time.Duration(flow.fixture.sequence().sendBufferSettings.TimeoutResendDeferLimit+1) * flow.fixture.sequence().sendBufferSettings.MaxResendInterval)
		if deadline.Before(bound) {
			bound = deadline
		}
		select {
		case <-flow.retryWritten:
		case <-time.After(time.Until(bound)):
			t.Fatal("refused original did not use unchanged natural retry owner")
		}
		synctest.Wait()
		retry := flow.fixture.recovery(wire)
		flow.fixture.forward(retry, flow.fixture.receiverIn)
		time.Sleep(2 * settings.ackCompressTimeout)
		flow.fixture.acknowledge()
		if flow.fixture.receiveSequence() != resident || flow.fixture.deliveredCount != 16 || flow.fixture.ackedCount != 2 || flow.failed != 0 || flow.fixture.sequence().resendQueue.Len() != 0 {
			t.Fatal("natural refusal retry changed resident/identity or terminal delivery")
		}
		joinReceiveMemoryLifecycleOwnedFlow(t, flow)
		t.Logf("[receive-memory-owner-lifecycle] refusal=true decoded_stage_observed=%t roots_returned=true same_identity_natural_retry=true no_false_ack=true resident_alive=true source_owner_parked=true source_released_before_natural_retry=true copy_on_write=%t offered_wire_bytes=%d roots_before=%d roots_after=%d bytes_before=%d bytes_after=%d", observed, copiedWrite, offeredWireBytes, gate.beforeRoots, roots, gate.beforeBytes, bytes)
	})
}

// A replay of already delivered data takes no second delivery or source owner.
// Captured input bytes are independent test custody, returned by actual ingress.
func TestReceiveMemoryDeliveredDuplicateReturnsRoots(t *testing.T) {
	settings := receiveMemoryLifecycleTestSettings()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		rx, pack, client, root := receiveMemoryLifecycleBudgets(t, settings)
		flow := newReceiveMemoryLifecycleFlow(t, settings, rx, pack)
		gate := newReceiveMemoryLifecycleGate(t, flow, root, client, rx, pack)
		wire := flow.group(t, 8)
		duplicate := &windowRoundFrame{bytes: MessagePoolCopy(wire.bytes), pack: wire.pack}
		flow.fixture.heldFrames = append(flow.fixture.heldFrames, duplicate)
		flow.fixture.forward(wire, flow.fixture.receiverIn)
		time.Sleep(2 * settings.ackCompressTimeout)
		flow.fixture.acknowledge()
		resident := flow.fixture.receiveSequence()
		next := resident.nextSequenceNumber
		if flow.fixture.deliveredCount != 8 || flow.fixture.ackedCount != 1 || flow.fixture.sequence().resendQueue.Len() != 0 {
			t.Fatal("first delivery did not establish genuine cumulative ownership")
		}
		gate.stages = []*receiveMemoryLifecycleStage{{Name: "pack_released", Entered: make(chan receiveMemoryLifecycleSample, 1), Resume: make(chan struct{})}}
		gate.before(t, duplicate)
		flow.fixture.forward(duplicate, flow.fixture.receiverIn)
		synctest.Wait()
		observed := false
		select {
		case sample := <-gate.stages[0].Entered:
			observed = true
			assertReceiveMemoryLifecycleOwner(t, sample)
			if sample.PackClaim != 0 || resident.receiveQueue.Len() != 0 || resident.nextSequenceNumber != next || flow.fixture.sequence().resendQueue.Len() != 0 {
				t.Fatal("past duplicate changed logical/source custody before validation")
			}
			// This second physical copy may own a bounded transient claim;
			// the invariant is its return, not an uncharged decoded copy.
			close(gate.stages[0].Resume)
			synctest.Wait()
		default:
			// A fixed decoder may reject an already-delivered identity before
			// creating the second owner; no false delivery is implied.
		}
		time.Sleep(2 * settings.ackCompressTimeout)
		flow.fixture.acknowledge()
		if flow.fixture.deliveredCount != 8 || flow.fixture.ackedCount != 1 || flow.fixture.receiveSequence() != resident || resident.ctx.Err() != nil || resident.nextSequenceNumber != next || resident.receiveQueue.Len() != 0 {
			t.Fatal("past duplicate redelivered data, altered terminal source custody or closed its resident")
		}
		joinReceiveMemoryLifecycleOwnedFlow(t, flow)
		t.Logf("[receive-memory-owner-lifecycle] delivered_duplicate=true decoded_stage_observed=%t roots_returned=true delivery_once=true resident_alive=true", observed)
	})
}

// Cancellation can happen at either accepted ownership boundary. Existing
// deliver-then-die behavior is preserved; every source callback and pool/root
// claim must retire once regardless of whether that accepted batch is flushed.
func TestReceiveMemoryCancellationReturnsRoots(t *testing.T) {
	settings := receiveMemoryLifecycleTestSettings()
	assertMessagePoolOwnership(t)
	for _, name := range []string{"pack_queued", "head_buffered"} {
		synctest.Test(t, func(t *testing.T) {
			rx, pack, client, root := receiveMemoryLifecycleBudgets(t, settings)
			flow := newReceiveMemoryLifecycleFlow(t, settings, rx, pack)
			var gate *receiveMemoryLifecycleGate
			var entered <-chan struct{}
			var resume func()
			if name == "pack_queued" {
				gate = newReceiveMemoryLifecycleGate(t, flow, root, client, rx, pack)
				entered, resume = pauseReceiveMemoryLifecycleRun(t, flow)
			} else {
				gate = newReceiveMemoryLifecycleGate(t, flow, root, client, rx, pack, name)
				resume = func() { close(gate.stages[0].Resume) }
			}
			initialRoots, initialBytes := messagePoolOutstandingSnapshot()
			wire := flow.group(t, 8)
			gate.before(t, wire)
			flow.fixture.forward(wire, flow.fixture.receiverIn)
			var sample receiveMemoryLifecycleSample
			if name == "pack_queued" {
				<-entered
				sample = receiveMemoryLifecycleQueuedOwner(t, flow, gate)
			} else {
				sample = <-gate.stages[0].Entered
			}
			synctest.Wait()
			assertReceiveMemoryLifecycleOwner(t, sample)
			flow.fixture.cancel()
			resume()
			synctest.Wait()
			joinReceiveMemoryLifecycleOwnedFlow(t, flow)
			roots, bytes := messagePoolOutstandingSnapshot()
			if root.UsedByteCount() != 0 || rx.UsedByteCount() != 0 || pack.UsedByteCount() != 0 || roots != initialRoots || bytes != initialBytes ||
				flow.fixture.ackedCount != 0 || flow.failed != 1 || flow.fixture.deliveredCount < 0 || flow.fixture.deliveredCount > 8 {
				t.Fatal("accepted-owner cancellation leaked/double-returned storage or changed terminal callback custody")
			}
			t.Logf("[receive-memory-owner-lifecycle] canceled_stage=%s actual_owner_before_cancel=%d root_before_cancel=%d all_roots_returned=true terminal_callback_once=true", name, sample.Cost, sample.Root)
		})
	}
}
