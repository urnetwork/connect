// A selected retry can wait behind an older physical owner before it is
// attempted. Its next cadence starts at that retry entry, not at selection.
package connect

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Real H3/P2P acceptances separate selection at 2s, retry entry at 7s and
// successful acceptance at 7.25s. No H1 timer or lifetime setting is changed.
func TestWindowPacingOwnerSelectionDelayedCarrierKeepsRetryCadence(t *testing.T) {
	for _, row := range []struct {
		carrier    TransportType
		unreliable bool
	}{
		{carrier: TransportTypeH3},
		{carrier: TransportTypeP2p},
		{carrier: TransportTypeH3, unreliable: true},
		{carrier: TransportTypeP2p, unreliable: true},
	} {
		runWindowOwnerProgressFixture(t, false, false, 5*time.Second, func(t *testing.T, fixture *windowOwnerProgressFixture) {
			sequence, head, younger := fixture.sequence, fixture.head, fixture.younger
			headId, youngerId := head.messageId, younger.messageId
			time.Sleep(time.Until(fixture.due))
			synctest.Wait()
			if sequence.pendingRecovery == nil || sequence.pendingRecovery.at != fixture.due ||
				sequence.pendingRecovery.messageId != headId || len(fixture.route) != 0 {
				t.Fatal("fixture did not select the exact older recovery at its unchanged 2s boundary")
			}
			selectedAt := sequence.pendingRecovery.at
			selector := sequence.contractMultiRouteWriter.(*MultiRouteSelector)
			var h1 Transport
			selector.mutex.Lock()
			for transport := range selector.transportRoutes {
				h1 = transport
			}
			selector.mutex.Unlock()
			// Remove the ready old route before publishing another eligible
			// carrier; a transient H1/P2P set may really write through H1.
			fixture.client.RouteManager().UpdateTransport(h1, nil)
			synctest.Wait()
			if selector.hasTransport(h1) || len(selector.activeRoutesSnapshot.Load().routes) != 0 ||
				younger.transportWriteObserved || len(fixture.route) != 0 ||
				sequence.pendingRecovery == nil || sequence.pendingRecovery.at != selectedAt {
				t.Fatal("H1 withdrawal did not preserve the unwritten younger owner through the empty generation")
			}
			replacement := make(Route)
			carrier := NewSendGatewayTransportWithType(row.carrier)
			fixture.client.RouteManager().UpdateTransportWithProperties(carrier, []Route{replacement},
				TransferCarrierProperties{Unreliable: row.unreliable})
			type accepted struct {
				at   time.Time
				wire []byte
			}
			events := make(chan accepted, 8)
			var readerDone chan struct{}
			overflow := false
			defer func() {
				fixture.cancel()
				if readerDone != nil {
					<-readerDone
				}
				for len(events) > 0 {
					MessagePoolReturn((<-events).wire)
				}
				if overflow {
					t.Error("fixed physical acceptance census overflowed")
				}
			}()
			synctest.Wait()
			service := sequence.windowPacer.service
			service.stateLock.Lock()
			held := service.pacingReservations == 1 && service.reservedByteCount == younger.pacingByteCount &&
				service.waiterHead == &sequence.windowPacer.waiter && service.waiterTail == &sequence.windowPacer.waiter &&
				service.next == fixture.chargedNext && service.probeSent == fixture.chargedProbe
			reservations, reserved := service.pacingReservations, service.reservedByteCount
			fifoHead, fifoTail := service.waiterHead == &sequence.windowPacer.waiter, service.waiterTail == &sequence.windowPacer.waiter
			next, probe := service.next, service.probeSent
			service.stateLock.Unlock()
			policy := sequence.transferFlightPolicy()
			var pendingAt time.Time
			pendingIdMatches := false
			if sequence.pendingRecovery != nil {
				pendingAt = sequence.pendingRecovery.at
				pendingIdMatches = sequence.pendingRecovery.messageId == headId
			}
			t.Logf("owner_carrier_premise carrier=%s unreliable=%t now_after_due=%s h1_only=%t h1_write_only=%t generation=%d pending=%t pending_at_same=%t pending_id_matches=%t pending_at_after_due=%s younger_written=%t held=%t reservations=%d reserved=%d younger_bytes=%d fifo_head_same=%t fifo_tail_same=%t next_same=%t next_after_due=%s charged_next_after_due=%s probe_same=%t probe=%d charged_probe=%d admissions=%d",
				row.carrier, row.unreliable, time.Since(fixture.due), policy.h1Only, policy.h1WriteOnly, policy.generation,
				sequence.pendingRecovery != nil, pendingAt == selectedAt, pendingIdMatches, pendingAt.Sub(fixture.due),
				younger.transportWriteObserved, held, reservations, reserved, younger.pacingByteCount, fifoHead, fifoTail,
				next == fixture.chargedNext, next.Sub(fixture.due), fixture.chargedNext.Sub(fixture.due),
				probe == fixture.chargedProbe, probe, fixture.chargedProbe, fixture.observation().admissions)
			if policy.h1Only || policy.h1WriteOnly || sequence.pendingRecovery == nil ||
				sequence.pendingRecovery.at != selectedAt || younger.transportWriteObserved || !held || fixture.observation().admissions != 0 {
				t.Fatal("blocked replacement did not retain the same original FIFO reservation and debt")
			}
			entry := fixture.due.Add(5 * time.Second)
			time.Sleep(time.Until(entry))
			wire := <-replacement
			pack := decodeSendPackLifecycleWirePack(t, wire)
			MessagePoolReturn(wire)
			synctest.Wait()
			if string(pack.MessageId) != string(youngerId[:]) || len(fixture.written) != 1 ||
				<-fixture.written != younger.sequenceNumber || sequence.pendingRecovery == nil ||
				sequence.pendingRecovery.at != selectedAt || sequence.resendWriteCount.Load() != 0 {
				t.Fatal("actual delayed younger acceptance did not preserve its selected older owner")
			}
			service.stateLock.Lock()
			released := service.pacingReservations == 0 && service.reservedByteCount == 0 &&
				service.waiterHead == nil && service.waiterTail == nil &&
				service.next == fixture.chargedNext && service.probeSent == fixture.chargedProbe
			service.stateLock.Unlock()
			if !released || fixture.observation().admissions != 0 {
				t.Fatal("accepted non-H1 original did not release exactly its held reservation without refund")
			}
			retryEntry := make(chan time.Time, 1)
			// The younger initial-write hook still owns Run. Hand off the
			// completed older retry before Run can dispatch overdue younger work;
			// a retained item's old deadline may already describe an active write.
			completedRetry, resumeCompletedRetry := make(chan time.Time, 1), make(chan struct{})
			completionObserved := false
			sequence.sendBuffer.afterApplyAckSnapshotForTest = func(id sendSequenceId) {
				if id != sequence.id() || completionObserved {
					return
				}
				item := sequence.resendQueue.GetByMessageId(headId)
				if item != head || item.sendCount != 2 || sequence.resendWriteCount.Load() != 1 {
					return
				}
				completionObserved = true
				completedRetry <- time.Now()
				select {
				case <-sequence.ctx.Done():
				case <-resumeCompletedRetry:
				}
			}
			sequence.sendBufferSettings.afterTransferWriteDeadlineForTest = func(time.Time) {
				select {
				case retryEntry <- time.Now():
				default:
				}
			}
			close(fixture.resumeYounger)
			synctest.Wait()
			if len(retryEntry) != 1 || <-retryEntry != entry || sequence.resendWriteCount.Load() != 0 {
				t.Fatal("selected retry did not enter its own physical writer at 7s")
			}
			acceptedAt := entry.Add(250 * time.Millisecond)
			time.Sleep(time.Until(acceptedAt))
			wire = <-replacement
			pack = decodeSendPackLifecycleWirePack(t, wire)
			MessagePoolReturn(wire)
			synctest.Wait()
			select {
			case completedAt := <-completedRetry:
				if completedAt != acceptedAt || time.Now() != acceptedAt {
					t.Fatal("completed older retry moved its actual 7.25s inspection boundary")
				}
			default:
				t.Fatal("actual older retry did not hand off its completed retained identity")
			}
			interval := sequence.resendIntervalForItem(head, 2)
			nextDue := entry.Add(interval)
			lifetime := fixture.lifetime
			if row.unreliable {
				lifetime = head.sendTime.Add(max(fixture.lifetime.Sub(head.sendTime), sequence.sendBufferSettings.UnreliableAckTimeout))
			}
			if time.Now() != acceptedAt || string(pack.MessageId) != string(headId[:]) ||
				pack.SequenceNumber != head.sequenceNumber || head.carrierRoute != replacement ||
				head.unreliableCarrierObserved != row.unreliable || head.sendCount != 2 ||
				sequence.resendWriteCount.Load() != 1 || head.resendTime != nextDue ||
				!nextDue.After(acceptedAt) || head.sendTime.Add(head.ackTimeout) != lifetime {
				t.Fatalf("carrier=%s unreliable=%t delayed selected retry reused selection time: selected=%s entry=%s accepted=%s next=%s want=%s",
					row.carrier, row.unreliable, selectedAt.Sub(head.sendTime), entry.Sub(head.sendTime),
					acceptedAt.Sub(head.sendTime), head.resendTime.Sub(head.sendTime), nextDue.Sub(head.sendTime))
			}
			readerDone = make(chan struct{})
			go func() {
				defer close(readerDone)
				for {
					select {
					case <-fixture.ctx.Done():
						return
					case wire := <-replacement:
						select {
						case events <- accepted{at: time.Now(), wire: wire}:
						default:
							overflow = true
							MessagePoolReturn(wire)
						}
					}
				}
			}()
			if time.Now() != acceptedAt {
				t.Fatal("completed older retry release moved its actual 7.25s boundary")
			}
			close(resumeCompletedRetry)
			synctest.Wait()
			time.Sleep(time.Until(nextDue.Add(-time.Nanosecond)))
			synctest.Wait()
			for len(events) > 0 {
				event := <-events
				pack := decodeSendPackLifecycleWirePack(t, event.wire)
				MessagePoolReturn(event.wire)
				if string(pack.MessageId) != string(youngerId[:]) {
					t.Fatal("older physical copy preceded the new retry-entry cadence")
				}
			}
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			copies := 0
			for len(events) > 0 {
				event := <-events
				pack := decodeSendPackLifecycleWirePack(t, event.wire)
				MessagePoolReturn(event.wire)
				if string(pack.MessageId) == string(headId[:]) {
					copies++
					if event.at != nextDue || pack.SequenceNumber != head.sequenceNumber {
						t.Fatal("next physical recovery changed its exact cadence or identity")
					}
				}
			}
			if copies != 1 || sequence.pendingRecovery != nil || head.sendTime.Add(head.ackTimeout) != lifetime {
				t.Fatal("new retry-entry deadline did not produce exactly one real older copy")
			}
		})
	}
}

// Younger and retry can share one physical burst timestamp. That equality
// must not turn the older selection into an already-expired next interval.
func TestWindowPacingOwnerSelectionDelayedH1KeepsPaidClock(t *testing.T) {
	runWindowOwnerProgressFixture(t, false, false, 5*time.Second, func(t *testing.T, fixture *windowOwnerProgressFixture) {
		sequence, head := fixture.sequence, fixture.head
		time.Sleep(time.Until(fixture.due))
		synctest.Wait()
		if sequence.pendingRecovery == nil || sequence.pendingRecovery.at != fixture.due {
			t.Fatal("fixture did not preserve the early H1 recovery selection")
		}
		time.Sleep(time.Until(fixture.youngerDeadline))
		synctest.Wait()
		if len(fixture.route) != 1 || len(fixture.written) != 1 || <-fixture.written != fixture.younger.sequenceNumber {
			t.Fatal("paid younger H1 original did not finish its own reservation")
		}
		youngerWire := <-fixture.route
		youngerPack := decodeSendPackLifecycleWirePack(t, youngerWire)
		MessagePoolReturn(youngerWire)
		if string(youngerPack.MessageId) != string(fixture.younger.messageId[:]) ||
			youngerPack.SequenceNumber != fixture.younger.sequenceNumber {
			t.Fatal("paid younger H1 original changed its actual accepted identity")
		}
		close(fixture.resumeYounger)
		select {
		case <-fixture.retryAdmitted:
		case <-sequence.ctx.Done():
			t.Fatal("selected H1 retry lost its paid service")
		}
		synctest.Wait()
		if len(fixture.route) != 1 || sequence.resendWriteCount.Load() != 1 {
			t.Fatal("paid H1 retry did not physically progress")
		}
		wire := <-fixture.route
		pack := decodeSendPackLifecycleWirePack(t, wire)
		MessagePoolReturn(wire)
		// Reading the first copy frees the real route slot. Quiescence exposes
		// any already-due duplicate before inspecting counts and the next clock.
		synctest.Wait()
		physical := sequence.windowPacer.waiter.sentAt
		youngerPhysical := sequence.firstPhysicalRecoveryTime(fixture.younger)
		nextDue := youngerPhysical.Add(sequence.resendIntervalForItem(head, 2))
		arrived, resume := fixture.inspection.arm(nextDue)
		if time.Now() != fixture.due.Add(5*time.Second) || string(pack.MessageId) != string(head.messageId[:]) ||
			!physical.Equal(youngerPhysical) || !physical.Equal(fixture.youngerDeadline) ||
			nextDue != fixture.due.Add(9*time.Second) || head.resendTime != nextDue ||
			head.sendCount != 2 || sequence.resendWriteCount.Load() != 1 || fixture.observation().admissions != 2 ||
			len(fixture.route) != 0 || head.sendTime.Add(head.ackTimeout) != fixture.lifetime {
			t.Fatalf("equal-time paid H1 retry duplicated or reused selection: copies=%d admissions=%d send_count=%d next=%s want=%s lifetime=%s",
				sequence.resendWriteCount.Load(), fixture.observation().admissions, head.sendCount,
				head.resendTime.Sub(head.sendTime), nextDue.Sub(head.sendTime), head.ackTimeout)
		}
		// The younger original has its own 9s retry. Acknowledge only that
		// accepted identity after the 7s causal check, leaving the old hole due.
		if ok, err := sequence.Ack(&protocol.Ack{
			MessageId: youngerPack.MessageId, SequenceId: sequence.sequenceId.Bytes(), Selective: true,
		}, 0); !ok || err != nil {
			t.Fatalf("exact younger selective ACK was refused: %t %v", ok, err)
		}
		synctest.Wait()
		if !fixture.younger.selectiveAcked || !fixture.younger.deliveryObserved ||
			sequence.resendQueue.GetByMessageId(head.messageId) != head || head.selectiveAcked || head.deliveryObserved ||
			sequence.ackWindow.PendingDispositionFor(head.sequenceNumber, head.messageId) ||
			head.resendTime != nextDue || head.sendCount != 2 || head.recoveryKind != sendRecoveryNone ||
			sequence.resendWriteCount.Load() != 1 || fixture.observation().admissions != 2 ||
			head.sendTime.Add(head.ackTimeout) != fixture.lifetime {
			t.Fatal("younger selective ACK changed the separate older recovery or ACK ownership")
		}
		time.Sleep(time.Until(nextDue.Add(-time.Nanosecond)))
		synctest.Wait()
		if len(fixture.route) != 0 || sequence.resendWriteCount.Load() != 1 || head.sendCount != 2 {
			t.Fatal("equal-time H1 selection emitted an older copy before its exact next deadline")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		resumeWindowOwnerInspection(t, arrived, resume, nextDue)
		if len(fixture.route) != 1 || sequence.resendWriteCount.Load() != 2 || head.sendCount != 3 ||
			!sequence.windowPacer.waiter.sentAt.Equal(nextDue) {
			t.Fatal("equal-time H1 selection lost its next exact paid physical copy")
		}
		wire = <-fixture.route
		pack = decodeSendPackLifecycleWirePack(t, wire)
		MessagePoolReturn(wire)
		if string(pack.MessageId) != string(head.messageId[:]) || head.sendTime.Add(head.ackTimeout) != fixture.lifetime {
			t.Fatal("next H1 copy changed identity or extended its original ACK lifetime")
		}
	})
}

// The shared retry tail also serves the synchronous announcement continuation;
// no intervening Run pass may lend that copy its stale 2s selection clock.
func TestWindowPacingOwnerSelectionDelayedContractKeepsRetryCadence(t *testing.T) {
	for _, carrier := range []TransportType{TransportTypeH3, TransportTypeP2p} {
		var entry time.Time
		var announcementId Id
		var announcementNumber uint64
		runWindowOwnerContractContinuation(t, false,
			func(t *testing.T, sequence *SendSequence, older *sendItem, route Route) {
				time.Sleep(time.Until(older.resendTime.Add(5 * time.Second)))
				entry = time.Now()
				selector := sequence.contractMultiRouteWriter.(*MultiRouteSelector)
				var h1 Transport
				selector.mutex.Lock()
				for transport := range selector.transportRoutes {
					h1 = transport
				}
				selector.mutex.Unlock()
				selector.updateTransport(h1, nil)
				selector.updateTransport(NewSendGatewayTransportWithType(carrier), []Route{route})
				if sequence.pendingRecovery == nil || sequence.pendingRecovery.at != older.sendTime.Add(2*time.Second) {
					t.Fatal("contract continuation lost its earlier selected recovery")
				}
				// The real announcement was accepted at 3s and is itself due
				// at 5s. Its exact selective ACK suppresses only that copy:
				// cumulative delivery here would falsely acknowledge the old hole.
				var announcement *sendItem
				for _, item := range sequence.sendItems {
					if item.sequenceNumber == older.sequenceNumber+1 {
						announcement = item
						break
					}
				}
				if announcement == nil || !announcement.contractControl || !announcement.transportWriteObserved ||
					announcement.sendCount != 1 || !announcement.resendTime.Before(entry) {
					t.Fatal("delayed continuation lost its actual accepted announcement")
				}
				announcementId, announcementNumber = announcement.messageId, announcement.sequenceNumber
				if ok, err := sequence.Ack(&protocol.Ack{
					MessageId: announcementId.Bytes(), SequenceId: sequence.sequenceId.Bytes(), Selective: true,
				}, 0); !ok || err != nil {
					t.Fatalf("exact announcement selective ACK was refused: %t %v", ok, err)
				}
				if !sequence.ackWindow.PendingDispositionFor(announcementNumber, announcementId) ||
					sequence.ackWindow.PendingDispositionFor(older.sequenceNumber, older.messageId) ||
					sequence.pendingRecovery == nil || sequence.pendingRecovery.messageId != older.messageId {
					t.Fatal("announcement ACK consumed or acknowledged the separate selected older copy")
				}
			},
			func(t *testing.T, sequence *SendSequence, older *sendItem, _ Route) {
				if older.resendTime != entry.Add(sequence.resendIntervalForItem(older, 2)) {
					t.Fatal("contract continuation reused selection time instead of its delayed retry entry")
				}
				announcement := sequence.resendQueue.GetByMessageId(announcementId)
				if announcement == nil || announcement.sendCount != 1 ||
					!sequence.ackWindow.PendingDispositionFor(announcementNumber, announcementId) ||
					sequence.ackWindow.PendingDispositionFor(older.sequenceNumber, older.messageId) {
					t.Fatal("contract cadence check lost distinct announcement and older ACK ownership")
				}
			},
		)
	}
}

// A blocked P2P publication does not withdraw an already-ready H1 route.
// Force that mixed generation to settle before removal and decode its real wire.
func TestWindowPacingOwnerSelectionMixedP2pCanUseLiveH1(t *testing.T) {
	for _, unreliable := range []bool{false, true} {
		runWindowOwnerProgressFixture(t, false, false, 5*time.Second, func(t *testing.T, fixture *windowOwnerProgressFixture) {
			sequence, head, younger := fixture.sequence, fixture.head, fixture.younger
			time.Sleep(time.Until(fixture.due))
			synctest.Wait()
			if sequence.pendingRecovery == nil || sequence.pendingRecovery.at != fixture.due ||
				sequence.pendingRecovery.messageId != head.messageId || len(fixture.route) != 0 {
				t.Fatal("fixture did not select the original older recovery before mixed publication")
			}
			selection := *sequence.pendingRecovery
			selector := sequence.contractMultiRouteWriter.(*MultiRouteSelector)
			var h1 Transport
			selector.mutex.Lock()
			for transport := range selector.transportRoutes {
				h1 = transport
			}
			selector.mutex.Unlock()
			replacement := make(Route)
			p2p := NewSendGatewayTransportWithType(TransportTypeP2p)
			fixture.client.RouteManager().UpdateTransportWithProperties(p2p, []Route{replacement},
				TransferCarrierProperties{Unreliable: unreliable})
			// No P2P reader and no H1 withdrawal exist yet. The real younger
			// write must settle at its existing post-write fixture barrier.
			synctest.Wait()
			policy := sequence.transferFlightPolicy()
			snapshot := selector.activeRoutesSnapshot.Load()
			service := sequence.windowPacer.service
			service.stateLock.Lock()
			released := service.pacingReservations == 0 && service.reservedByteCount == 0 &&
				service.waiterHead == nil && service.waiterTail == nil &&
				service.next == fixture.chargedNext && service.probeSent == fixture.chargedProbe &&
				service.sent == fixture.chargedSent && service.total == 0 && !service.drained
			service.stateLock.Unlock()
			if time.Now() != fixture.due || policy.h1Only || policy.h1WriteOnly || policy.limited != unreliable ||
				!policy.reliableRouteAvailable || len(snapshot.routes) != 2 || snapshot.preferDirectRoute != nil ||
				!selector.hasTransport(h1) || !selector.hasTransport(p2p) || !released || fixture.observation().admissions != 0 ||
				!younger.transportWriteObserved || younger.carrierRoute != fixture.route || !younger.rttH1 ||
				!younger.reliableCarrierObserved || younger.unreliableCarrierObserved ||
				sequence.pendingRecovery == nil || *sequence.pendingRecovery != selection ||
				head.sendTime.Add(head.ackTimeout) != fixture.lifetime || head.deliveryObserved || younger.deliveryObserved ||
				len(fixture.route) != 1 || len(fixture.written) != 1 ||
				<-fixture.written != younger.sequenceNumber || sequence.resendWriteCount.Load() != 0 {
				t.Fatalf("unreliable=%t mixed publication did not preserve actual H1 acceptance and selected ownership", unreliable)
			}
			wire := <-fixture.route
			defer MessagePoolReturn(wire)
			pack := decodeSendPackLifecycleWirePack(t, wire)
			if string(pack.MessageId) != string(younger.messageId[:]) || pack.SequenceNumber != younger.sequenceNumber ||
				string(pack.SequenceId) != string(sequence.sequenceId[:]) ||
				sequence.firstPhysicalRecoveryTime(younger) != fixture.due {
				t.Fatal("mixed generation's real H1 wire changed the younger identity or physical clock")
			}
			t.Logf("mixed P2P unreliable=%t accepted exact younger on live H1 at due; reservation released once, debt and older selection unchanged", unreliable)
			fixture.client.RouteManager().UpdateTransport(h1, nil)
			if selector.hasTransport(h1) || sequence.pendingRecovery == nil || *sequence.pendingRecovery != selection {
				t.Fatal("later H1 withdrawal consumed the already accepted original or older selection")
			}
			fixture.cancel()
			<-sequence.done
			synctest.Wait()
			service.stateLock.Lock()
			kept := service.next == fixture.chargedNext && service.probeSent == fixture.chargedProbe &&
				service.pacingReservations == 0 && service.reservedByteCount == 0 && !service.drained
			service.stateLock.Unlock()
			if !kept || sequence.pendingRecovery != nil || len(sequence.sendItems) != 0 ||
				len(fixture.route) != 0 || sequence.resendWriteCount.Load() != 0 {
				t.Fatal("mixed acceptance cancellation leaked ownership or refunded physical debt")
			}
		})
	}
}
