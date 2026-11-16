// Cancellation at the real serialization boundary must preserve unwritten
// custody with either retention policy while a sibling source remains live.
package connect

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// The cancelled destination owns one group; its client also serves an
// independent destination that writes after the cancelled source has joined.
func testAffinitySourceCancellationBeforeRetention(t *testing.T, retained bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var budget *TransferMemoryBudget
		if retained {
			budget = NewTransferMemoryBudget(64 * 1024)
		}
		var source *SendSequence
		var group *parsedPacketGroup
		joined := make(chan struct{})
		boundaryReached := false
		var f *groupDispositionQueueFixture
		f = newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
				group = target.(*parsedPacketGroup)
			}
			settings.SendBufferSettings.beforeGroupDequeueForTest = func(selected *SendSequence) {
				source = selected
			}
			settings.SendBufferSettings.beforeGroupRetainForTest = func() {
				boundaryReached = true
				source.Cancel()
			}
			settings.SendBufferSettings.afterRunSendSequenceForTest = func(id sendSequenceId) {
				if id.Destination == f.peer {
					close(joined)
				}
			}
		})
		if f.offer(groupDispositionControlPacket(100, 0, 64, tcpFlagSyn)) != 1 {
			t.Fatal("initial public SYN was not admitted")
		}
		f.requireParked()
		affinitySourcesTestCloseEnrollment(f.update)
		if f.update.affinitySourceFenced(f.selected) || group == nil {
			t.Fatal("fixture did not retain its admitted source")
		}
		f.unpark()
		<-joined
		synctest.Wait()
		if !boundaryReached || source.ctx.Err() == nil {
			t.Fatal("source cancellation missed the serialization boundary")
		}
		if !f.update.affinitySourceFenced(f.selected) {
			t.Errorf("unwritten canceled source retained custody: pending=%d ambiguous=%v materialized=%v",
				f.update.affinitySources.first.pending, f.update.affinitySources.first.ambiguous,
				group.affinitySourceState&affinitySourceMaterialized != 0)
		}
		if group.affinitySourceState&affinitySourceMaterialized != 0 {
			t.Error("canceled source materialized an original before its refusal")
		}
		if f.started.Load() != 1 || f.terminal.Load() != 1 || len(f.route) != 0 {
			t.Errorf("canceled source started=%d terminal=%d written=%d, want 1, 1, 0",
				f.started.Load(), f.terminal.Load(), len(f.route))
		}
		if budget != nil && budget.UsedByteCount() != 0 {
			t.Errorf("canceled source retained %d charged bytes", budget.UsedByteCount())
		}
		if f.client.IsDone() || f.update.IsDone() {
			t.Fatal("one source cancellation closed its shared parent or flow")
		}
		affinitySourceCancellationHealthySibling(t, f)
	})
}

// This is a real same-client route write and terminal completion after the
// canceled source has joined, not a test-only channel health flag.
func affinitySourceCancellationHealthySibling(t *testing.T, f *groupDispositionQueueFixture) {
	t.Helper()
	peer := NewId()
	route := make(Route, 1)
	transport := NewSendClientTransport(DestinationId(peer))
	f.client.ContractManager().AddNoContractPeer(peer)
	f.client.RouteManager().UpdateTransport(transport, []Route{route})
	frames, witnesses := transferGroupTestFrames(t, 1, 16)
	t.Cleanup(func() {
		if err := f.closeClient(context.Background()); err != nil {
			t.Error(err)
		}
		drainFlightGateRoute(route)
		releaseTransferGroupTestWitnesses(t, frames, witnesses)
	})
	results := make(chan error, 1)
	accepted := f.client.SendWithTimeout(frames[0], peer, func(err error) { results <- err }, time.Second, NoAck())
	if !accepted {
		MessagePoolReturn(frames[0].MessageBytes)
		t.Fatal("healthy sibling source refused its packet")
	}
	synctest.Wait()
	select {
	case err := <-results:
		if err != nil {
			t.Fatalf("healthy sibling source failed: %v", err)
		}
	default:
		t.Fatal("healthy sibling source did not complete")
	}
	if f.client.IsDone() || len(route) != 1 {
		t.Fatal("healthy sibling did not keep its shared client and physical write")
	}
}

func TestAffinitySourceCancellationBeforeRetentionLegacy(t *testing.T) {
	testAffinitySourceCancellationBeforeRetention(t, false)
}

func TestAffinitySourceCancellationBeforeRetentionBudgeted(t *testing.T) {
	testAffinitySourceCancellationBeforeRetention(t, true)
}

// The same source interface is used for NoAck logical groups, whose
// disposition does not itself distinguish disposal from a wire allocation.
type sourceCancellationGroupTarget struct {
	sendUnwindGroupTarget
	source *SendSequence
}

// Admission and dequeue can race; both identify the same owner under its lock.
func (self *sourceCancellationGroupTarget) sendGroupAdmitted(source *SendSequence) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.source = source
}

// The dequeue hook identifies the source before the serialization boundary.
func (self *sourceCancellationGroupTarget) sendGroupDequeued(source *SendSequence) {
	self.sendGroupAdmitted(source)
}

// NoAck bypasses the retained budget even when that budget is configured.
// Cancellation still precedes wire construction and terminal completion.
func TestSourceCancellationBeforeSerializationNoAck(t *testing.T) {
	for _, retained := range []bool{false, true} {
		assertMessagePoolOwnership(t)
		synctest.Test(t, func(t *testing.T) {
			var budget *TransferMemoryBudget
			if retained {
				budget = NewTransferMemoryBudget(64 * 1024)
			}
			target := &sourceCancellationGroupTarget{}
			joined := make(chan struct{})
			boundaryReached := false
			var f *groupDispositionQueueFixture
			f = newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
				settings.SendBufferSettings.beforeGroupRetainForTest = func() {
					target.stateLock.Lock()
					source := target.source
					target.stateLock.Unlock()
					boundaryReached = true
					source.Cancel()
				}
				settings.SendBufferSettings.afterRunSendSequenceForTest = func(id sendSequenceId) {
					if id.Destination == f.peer {
						close(joined)
					}
				}
			})
			frames, witnesses := transferGroupTestFrames(t, 2, 16)
			t.Cleanup(func() {
				if err := f.closeClient(context.Background()); err != nil {
					t.Error(err)
				}
				releaseTransferGroupTestWitnesses(t, frames, witnesses)
			})
			accepted, err := f.client.sendGroupToTargetWithTimeoutDetailed(
				frames, f.peer, MultiHopId{}, nil, target, 0, NoAck())
			if !accepted || err != nil {
				for _, frame := range frames {
					MessagePoolReturn(frame.MessageBytes)
				}
				t.Fatalf("NoAck group admission failed: %v", err)
			}
			f.requireParked()
			f.unpark()
			<-joined
			synctest.Wait()
			if !boundaryReached || len(target.results) != 1 || !errors.Is(target.results[0], context.Canceled) {
				t.Errorf("retained=%v NoAck cancellation did not complete exactly once at its boundary", retained)
			}
			if writes := f.client.initialSendWriteCount.Load(); writes != 0 {
				t.Errorf("retained=%v canceled NoAck group constructed %d writes", retained, writes)
			}
			if f.started.Load() != 1 || f.terminal.Load() != 1 || len(f.route) != 0 || f.client.IsDone() {
				t.Errorf("retained=%v canceled NoAck group changed lifecycle, wire or shared client", retained)
			}
			if budget != nil && budget.UsedByteCount() != 0 {
				t.Errorf("retained=%v canceled NoAck group retained charged bytes", retained)
			}
			affinitySourceCancellationHealthySibling(t, f)
		})
	}
}
