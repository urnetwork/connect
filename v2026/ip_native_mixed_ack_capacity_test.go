// Public native custody is distinct from physical retained capacity. An owned
// route with no peer ACK reader makes the exact flight boundary deterministic.
package connect

import (
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Pins the original census's 128-KiB limit without scheduler timing or a timer.
func TestNativeMixedAckAllocationOriginalBudgetDisposesRawSuffix(t *testing.T) {
	checkNativeMixedAckAllocationCapacity(t, "original")
}

// One complete 32-member flight fits exactly when every owner is funded.
func TestNativeMixedAckAllocationExactFlightBudgetCompletesAllMembers(t *testing.T) {
	checkNativeMixedAckAllocationCapacity(t, "exact")
}

// One missing retained byte refuses the last raw chunk, preserving its prefix.
func TestNativeMixedAckAllocationOneByteShortDisposesOnlyRawSuffix(t *testing.T) {
	checkNativeMixedAckAllocationCapacity(t, "one-byte-short")
}

// The fixture owns the real source, route and witnesses. No peer ACK exists
// until independently decoded written members have passed every charge check.
func checkNativeMixedAckAllocationCapacity(t *testing.T, mode string) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(128 * 1024)
		var retainCalls, capacityErrors, otherErrors atomic.Int64
		fixture := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			settings.SendBufferSettings.beforeGroupRetainForTest = func() { retainCalls.Add(1) }
			observer := settings.SendBufferSettings.SendPackLifecycleObserver
			settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
				observer(event)
				if event.Phase == SendPackLifecyclePhaseTerminal && event.Err != nil {
					if sendGroupCapacityFailure(event.Err) {
						capacityErrors.Add(1)
					} else {
						otherErrors.Add(1)
					}
				}
			}
		})
		beforeAckCount, beforeAckBytes := func() (int, ByteCount) {
			fixture.selected.stateLock.Lock()
			defer fixture.selected.stateLock.Unlock()
			return fixture.selected.packetStats.sendAckCount, fixture.selected.packetStats.sendAckByteCount
		}()
		templates := make([][]byte, 32)
		packets := make([][]byte, len(templates))
		membership := make([]bool, len(templates))
		for index := range templates {
			// Distinct controls make the decoded prefix identify exact original
			// indices even though every member is a pure acknowledgement.
			templates[index] = groupRecoveryPacket(100, 500, uint16(64+index), tcpFlagAck, nil)
			packets[index] = MessagePoolCopy(templates[index])
			fixture.witnesses = append(fixture.witnesses, MessagePoolShareReadOnly(packets[index]))
		}
		before := time.Now()
		count := fixture.parent.SendNativePacketBatchWithResults(fixture.source, fixture.parent.provideMode,
			packets, 5*time.Second, membership)
		if count != len(templates) {
			t.Fatalf("capacity control did not admit its whole native group: count=%d membership=%v", count, membership)
		}
		for index, admitted := range membership {
			if !admitted {
				t.Fatalf("queued native original %d lost its exact admission receipt", index)
			}
		}
		fixture.requireParked()
		group := fixture.update.sequenceClaims
		if group == nil || group.collapseSource == nil || group.collapseOwner != nil || budget.UsedByteCount() != 0 ||
			group.completionFlags.Load()&(groupCompletionTracked|groupCompletionAdmitted|groupCompletionReturned) !=
				groupCompletionTracked|groupCompletionAdmitted|groupCompletionReturned {
			t.Fatal("raw admission allocated retention or lost original tracked custody")
		}
		source := group.collapseSource
		flightCost := nativeMixedAckFlightRetainedCost(t, source, templates)
		if len(flightCost.chunks) != 16 || flightCost.metadata != group.sendGroupMemoryByteCount() {
			t.Fatal("actual source no longer has the census's two-member chunks and once-only metadata")
		}
		for index, end := range flightCost.ends {
			if end != 2*(index+1) {
				t.Fatal("actual chunk projection lost exact original member positions")
			}
		}
		limit := budget.TotalByteCount()
		switch mode {
		case "original":
		case "exact":
			limit = flightCost.total
		case "one-byte-short":
			limit = flightCost.total - 1
		default:
			t.Fatalf("unknown retained-capacity control %q", mode)
		}
		budget.SetTotalByteCount(limit)
		wantBytes, wantChunks, wantMembers := flightCost.metadata, 0, 0
		for index, bytes := range flightCost.chunks {
			if limit < wantBytes+bytes {
				break
			}
			wantBytes += bytes
			wantChunks++
			wantMembers = flightCost.ends[index]
		}
		if mode == "exact" && wantMembers != 32 || mode != "exact" && wantMembers != 30 {
			t.Fatalf("capacity precondition changed: mode=%s limit=%d flight=%d funded_members=%d", mode, limit, flightCost.total, wantMembers)
		}
		fixture.unpark()
		synctest.Wait()
		owner := group.collapseOwner
		written := (uint64(1) << uint(wantMembers)) - 1
		all := (uint64(1) << uint(len(templates))) - 1
		if owner == nil || owner.budget != budget || owner.budgetBytes != flightCost.metadata ||
			owner.remaining != 0 || owner.materialized != written || owner.disposed != all&^written ||
			budget.UsedByteCount() != wantBytes || budget.Available() != limit-wantBytes ||
			len(fixture.route) != wantChunks || retainCalls.Load() != int64(wantChunks) || fixture.terminal.Load() != 0 ||
			source.sendContract != nil || source.transferFlightPolicy().h1Only || source.resendMemoryGrant != 0 {
			t.Fatalf("capacity boundary lost its retained prefix or raw suffix: mode=%s retain_calls=%d route_chunks=%d used=%d want=%d terminal=%d",
				mode, retainCalls.Load(), len(fixture.route), budget.UsedByteCount(), wantBytes, fixture.terminal.Load())
		}
		queued, _, _ := fixture.client.ResendQueueSize(fixture.peer, MultiHopId{}, false, false)
		if queued != wantChunks || group.completionFlags.Load()&groupCompletionTerminal != 0 ||
			fixture.update.sequenceClaims != nil || time.Since(before) != 0 {
			t.Fatal("capacity control used a timer or completed live retained custody")
		}
		// Decoding validates each distinct original index before feeding the
		// real source ACK worker. This does not turn queue count into delivery.
		fixture.acknowledgeAll(templates[:wantMembers]...)
		wantCapacityErrors := int64(0)
		if wantMembers < len(templates) {
			wantCapacityErrors = 1
		}
		reserved, released := budget.Counts()
		queued, queuedBytes, _ := fixture.client.ResendQueueSize(fixture.peer, MultiHopId{}, false, false)
		if fixture.terminal.Load() != 1 || capacityErrors.Load() != wantCapacityErrors || otherErrors.Load() != 0 ||
			budget.UsedByteCount() != 0 || reserved != released || queued != 0 || queuedBytes != 0 ||
			group.completionFlags.Load()&groupCompletionTerminal == 0 || time.Since(before) != 0 {
			t.Fatalf("actual prefix ACK did not settle exact local refusal: mode=%s capacity=%d other=%d terminal=%d used=%d queue=%d/%d elapsed=%s",
				mode, capacityErrors.Load(), otherErrors.Load(), fixture.terminal.Load(), budget.UsedByteCount(), queued, queuedBytes, time.Since(before))
		}
		if wantCapacityErrors != 0 {
			fixture.requireLocalRefusalHealthy()
		} else {
			// A successful peer ACK keeps its progress stamp even at zero
			// outstanding. The refusal-only zero-stamp oracle does not apply.
			stats, err := fixture.selected.WindowStats()
			if err != nil {
				t.Fatalf("successful native peer ACK left a provider error: %v", err)
			}
			pending, lastAck := func() (time.Time, time.Time) {
				fixture.selected.stateLock.Lock()
				defer fixture.selected.stateLock.Unlock()
				return fixture.selected.pendingSendTime, fixture.selected.lastSendAckTime
			}()
			if stats.sendAckCount-beforeAckCount != len(templates) || stats.sendAckByteCount-beforeAckBytes != group.byteCount ||
				stats.sendNackCount != 0 || stats.sendNackByteCount != 0 || !stats.sendProgressKnown ||
				!hasPeerSendAckInWindow(stats) || lastAck.IsZero() || !pending.Equal(lastAck) ||
				!stats.pendingSendTime.Equal(lastAck) || lastAck.Before(before) || time.Now().Before(lastAck) ||
				fixture.selected.hasOutstandingSends() || fixture.selected.sendStalled(time.Nanosecond) ||
				!unacknowledgedSendSince(stats).IsZero() || !unansweredConnectSince(stats).IsZero() {
				t.Fatalf("successful native ACK lost idle progress semantics: ack=%d/%d nack=%d/%d pending=%v last_ack=%v",
					stats.sendAckCount-beforeAckCount, stats.sendAckByteCount-beforeAckBytes,
					stats.sendNackCount, stats.sendNackByteCount, pending, lastAck)
			}
			// Age only the real classifier's now argument. The source and its
			// timers remain at the original fake time; no receive or connect
			// attempt can supply another verdict in this pure-ACK fixture.
			noAckBar := time.Second
			future := lastAck.Add(2 * noAckBar)
			reason, held := blackholeReasonFromStats(future, stats, noAckBar, 0, 0, blackholeGates{})
			if reason != blackholeNone || held != blackholeNone {
				t.Fatalf("historical peer ACK convicted an idle channel: reason=%s held=%s", reason, held)
			}
			// A copy is a classifier positive control, not a fabricated source
			// admission. A genuinely outstanding count must use the same stamp
			// and reach the ordinary no-send-ACK verdict at this future instant.
			outstanding := *stats
			outstanding.sendNackCount = 1
			outstanding.sendNackByteCount = ByteCount(len(templates[0]))
			reason, held = blackholeReasonFromStats(future, &outstanding, noAckBar, 0, 0, blackholeGates{})
			if reason != blackholeNoSendAck || held != blackholeNone {
				t.Fatalf("historical progress classifier control missed an outstanding send: reason=%s held=%s", reason, held)
			}
			t.Logf("NATIVE_MIXED_ACK_SUCCESSFUL_PEER_ACK_IDLE acknowledged_members=%d acknowledged_bytes=%d historical_stamp=true outstanding=false unacknowledged_since_zero=true aged_idle_verdict=none aged_outstanding_control=no-send-ack elapsed=%s",
				stats.sendAckCount-beforeAckCount, stats.sendAckByteCount-beforeAckBytes, time.Since(before))
		}
		if err := fixture.closeClient(t.Context()); err != nil {
			t.Fatal(err)
		}
		for index, witness := range fixture.witnesses {
			fixture.witnesses[index] = nil
			if !MessagePoolReturn(witness) {
				t.Errorf("joined capacity control kept original packet owner %d", index)
			}
		}
		t.Logf("NATIVE_MIXED_ACK_RETAINED_CAPACITY mode=%s offered=32 queued=32 wire_members=%d wire_chunks=%d retain_calls=%d limit=%d full_flight=%d metadata=%d retained_prefix=%d local_capacity_errors=%d elapsed=%s",
			mode, wantMembers, wantChunks, retainCalls.Load(), limit, flightCost.total, flightCost.metadata, wantBytes, capacityErrors.Load(), time.Since(before))
	})
}
