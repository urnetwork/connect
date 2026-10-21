// Pins the difference between successful local Pack admission and the first
// physical route write while the real busy-probe budget expires.
package connect

import (
	"bytes"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A serialized local owner can delay an admitted probe beyond its caller and
// busy-probe budgets. Neither successful admission nor the later timer is a
// remotely unanswered question until the first writer actually accepts it.
func TestBusyProbeAdmittedUnwrittenBudgetPreservesWrittenFlow(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newBusyExpiryFixture(t, true)
		if fixture.channel.busyProbeSendFunc != nil {
			t.Fatal("counterfactual requires production SendDetailedMessage, not a send seam")
		}
		fixture.channel.stateLock.Lock()
		pendingAt := fixture.channel.pendingSendTime
		beforeOutstanding := fixture.channel.busyProbeOutstanding
		fixture.channel.stateLock.Unlock()
		if beforeOutstanding {
			t.Fatal("fixture inherited a prior probe")
		}

		// The first data Pack is already on this exact sequence's real route.
		// Its existing post-write barrier still owns the serializer; it is
		// released by fixture cleanup, not by Client.Cancel.
		var sequence *SendSequence
		matching := 0
		func() {
			fixture.client.sendBuffer.mutex.Lock()
			defer fixture.client.sendBuffer.mutex.Unlock()
			for id, candidate := range fixture.client.sendBuffer.sendSequences {
				if id.Destination == fixture.destination {
					sequence = candidate
					matching++
				}
			}
		}()
		if matching != 1 || sequence == nil || len(sequence.packs) != 0 {
			t.Fatalf("fixture needs one quiescent exact data owner: matches=%d", matching)
		}

		fixture.startVerdict(t)
		waitStarted := time.Now()
		budget := busyProbeBudget(20*time.Millisecond, fixture.window.reliabilitySettings().BusyProbeBudget)
		if budget != 500*time.Millisecond || busyProbeWriteTimeout(budget, fixture.channel.settings.CPingWriteTimeout) != 20*time.Millisecond {
			t.Fatal("unexpected production probe budgets")
		}
		var probeStarted SendPackLifecycleObservation
		probeStarts := 0
		for len(fixture.events) > 0 {
			event := <-fixture.events
			if event.MessageType != protocol.MessageType_IpIpPing {
				continue
			}
			if event.Phase != SendPackLifecyclePhaseStarted || event.Err != nil ||
				event.ClientId != fixture.client.ClientId() || event.DestinationId != fixture.destination || !event.AckRequired {
				t.Fatalf("probe crossed a writer/terminal boundary before the local barrier: %+v", event)
			}
			probeStarted = event
			probeStarts++
		}
		fixture.channel.stateLock.Lock()
		admitted := fixture.channel.busyProbeOutstanding
		pending, pendingBytes, acked := fixture.channel.packetStats.sendNackCount, fixture.channel.packetStats.sendNackByteCount, fixture.channel.packetStats.sendAckCount
		fixture.channel.stateLock.Unlock()
		// Started alone precedes admission. The exact queued Pack plus the
		// production post-success outstanding transition prove acceptance.
		if probeStarts != 1 || probeStarted.Token == 0 || !admitted || len(sequence.packs) != 1 ||
			len(fixture.route) != 0 || fixture.client.ReceiveStats().SendPackDeadlineDropCount != 0 ||
			pending != 1 || pendingBytes != 1000 || acked != 0 {
			t.Fatalf("probe was not really admitted behind written data: starts=%d admitted=%t queued=%d writes=%d pending=%d/%d/%d",
				probeStarts, admitted, len(sequence.packs), len(fixture.route), pending, pendingBytes, acked)
		}

		time.Sleep(budget - time.Nanosecond)
		synctest.Wait()
		select {
		case verdict := <-fixture.verdict:
			t.Fatalf("probe completed before its exact budget: verdict=%t elapsed=%s", verdict, time.Since(waitStarted))
		default:
		}
		if len(fixture.events) != 0 || len(sequence.packs) != 1 || len(fixture.route) != 0 ||
			fixture.client.ReceiveStats().SendPackDeadlineDropCount != 0 || !fixture.channel.busyProbeOutstandingNow() {
			t.Fatal("the serializer did not remain parked through the complete pre-budget interval")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if time.Since(waitStarted) != budget {
			t.Fatal("the verdict did not use the exact synthetic budget boundary")
		}

		// Even after a bad verdict cancels Client, its sequence is still held
		// at our independent barrier. No asynchronous terminal can be mistaken
		// for the cause of the timer decision.
		if len(fixture.events) != 0 || len(sequence.packs) != 1 || len(fixture.route) != 0 ||
			fixture.client.ReceiveStats().SendPackDeadlineDropCount != 0 {
			t.Fatal("probe reached a first-write or terminal before the timer-only observation")
		}
		returned, convicted := false, false
		select {
		case convicted = <-fixture.verdict:
			returned = true
		default:
		}
		fixture.channel.stateLock.Lock()
		endErr := fixture.channel.endErr
		pending, pendingBytes, acked = fixture.channel.packetStats.sendNackCount, fixture.channel.packetStats.sendNackByteCount, fixture.channel.packetStats.sendAckCount
		clock, probeAck := fixture.channel.pendingSendTime, fixture.channel.busyProbeAckTime
		fixture.channel.stateLock.Unlock()
		if pending != 1 || pendingBytes != 1000 || acked != 0 || !clock.Equal(pendingAt) || !probeAck.IsZero() {
			t.Fatal("timer observation forged data or probe progress")
		}
		fixture.sibling.stateLock.Lock()
		siblingErr, receives, receivedBytes := fixture.sibling.endErr, fixture.sibling.packetStats.receiveAckCount, fixture.sibling.packetStats.receiveAckByteCount
		fixture.sibling.stateLock.Unlock()
		if siblingErr != nil || fixture.sibling.ctx.Err() != nil || receives != 1 || receivedBytes != 77 ||
			!fixture.sibling.hasRecentReceive(520*time.Millisecond) {
			t.Fatal("independent receiving sibling no longer proves the window gate")
		}
		if convicted || endErr != nil || fixture.channel.ctx.Err() != nil || fixture.client.ctx.Err() != nil {
			if !returned || !convicted || endErr == nil ||
				!strings.Contains(endErr.Error(), "liveness probe timed out after 500ms") {
				t.Fatalf("unexpected non-timer retirement: returned=%t convicted=%t error=%v", returned, convicted, endErr)
			}
			t.Fatalf("BUSY_PROBE_ADMITTED_UNWRITTEN_BUDGET_RETIRED_WRITTEN_FLOW admitted=%t queued=%d first_write=0 terminal=0 elapsed=%s pending=%d/%d/%d channel_canceled=%t client_canceled=%t",
				admitted, len(sequence.packs), time.Since(waitStarted), pending, pendingBytes, acked,
				fixture.channel.ctx.Err() != nil, fixture.client.ctx.Err() != nil)
		}

		// Once the local barrier opens, the original caller deadline still
		// expires the unwritten probe. A prospective fix need not keep an
		// unbounded waiter or relabel that disposition as a peer ACK.
		close(fixture.releaseFirst)
		synctest.Wait()
		firstWrite, terminal := false, false
		for len(fixture.events) > 0 {
			event := <-fixture.events
			if event.Token != probeStarted.Token {
				continue
			}
			if event.ClientId != probeStarted.ClientId || event.DestinationId != probeStarted.DestinationId ||
				event.MessageType != probeStarted.MessageType || !event.AckRequired || !packetTransferExpiredUnwritten(event.Err) {
				t.Fatalf("released probe lost its exact local-unwritten disposition: %+v", event)
			}
			switch event.Phase {
			case SendPackLifecyclePhaseFirstRouteWrite:
				firstWrite = true
			case SendPackLifecyclePhaseTerminal:
				terminal = true
			default:
				t.Fatal("released probe repeated its Started phase")
			}
		}
		if !firstWrite || !terminal || len(fixture.route) != 0 || fixture.client.ReceiveStats().SendPackDeadlineDropCount != 1 {
			t.Fatal("release did not dispose exactly one queued, unwritten probe")
		}
		if !returned && <-fixture.verdict {
			t.Fatal("local unwritten disposition convicted after the serializer resumed")
		}
		acknowledgeSendPackLifecycleWirePack(t, fixture.client, fixture.destination, fixture.firstPack)
		synctest.Wait()
		packet := MessagePoolGet(1000)
		clear(packet)
		if !fixture.channel.SendWithAck(&parsedPacket{packet: packet, ipPath: icmpTcpTestPath(4)}, time.Second, true) {
			MessagePoolReturn(packet)
			t.Fatal("surviving channel refused data after local probe disposal")
		}
		synctest.Wait()
		if len(fixture.route) != 1 {
			t.Fatalf("recovered route writes=%d, want one", len(fixture.route))
		}
		wire := <-fixture.route
		pack := decodeSendPackLifecycleWirePack(t, wire)
		MessagePoolReturn(wire)
		if pack.Nack || pack.SequenceNumber != 1 || !bytes.Equal(pack.SequenceId, fixture.firstPack.SequenceId) {
			t.Fatal("unwritten probe replaced the surviving data sequence")
		}
		acknowledgeSendPackLifecycleWirePack(t, fixture.client, fixture.destination, pack)
		synctest.Wait()
		fixture.channel.stateLock.Lock()
		pending, pendingBytes, acked = fixture.channel.packetStats.sendNackCount, fixture.channel.packetStats.sendNackByteCount, fixture.channel.packetStats.sendAckCount
		probeAck = fixture.channel.busyProbeAckTime
		fixture.channel.stateLock.Unlock()
		if pending != 0 || pendingBytes != 0 || acked != 2 || !probeAck.IsZero() || fixture.channel.busyProbeOutstandingNow() {
			t.Fatalf("same-sequence recovery accounting=%d/%d/%d, want 0/0/2 without a probe ACK", pending, pendingBytes, acked)
		}
	})
}
