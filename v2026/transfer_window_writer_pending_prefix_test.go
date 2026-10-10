// A covered timer head must not hide an uncovered tail while an original owns
// its already-shared, unconsumed route buffer. ACK application remains in Run;
// the full real relay channel supplies the physical backpressure.
package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The selective-only case isolates pending-disposition masking without a
// pending cumulative-progress term on either the prefix or the due tail.
func TestWindowPacingSharedWriterPendingSelectivePrefixDoesNotMaskDueTail(t *testing.T) {
	runWindowWriterPendingPrefixTail(t, true)
}

// Mixed reliable-only overflow must not inherit the H1-only pending cumulative
// preemption rule. The prefix covers A, never the separately retained tail B.
func TestWindowPacingSharedWriterPendingCumulativePrefixDoesNotMaskDueTail(t *testing.T) {
	runWindowWriterPendingPrefixTail(t, false)
}

// A and B are real originals one nanosecond apart. B's physical route buffer
// stays in its full channel while C pays pacing and waits in the real selector.
func runWindowWriterPendingPrefixTail(t *testing.T, selective bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	if !DebugTransferCopyOnWrite {
		t.Fatal("share lifetime control requires the ordinary copy-on-write test mode")
	}
	synctest.Test(t, func(t *testing.T) {
		type observation struct {
			at time.Time
			ids [3]Id
			sent, physical, due, lifetime [3]time.Time
			present, written, tracked, reliable, unreliable, writePending [3]bool
			frameBytes, frameCapacity [3]int
			pacingBytes [3]ByteCount
			kinds [3]sendRecoveryKind
			pendingA, pendingB, pendingCumulative bool
			h1Only, reliableOnly, h1ReliableWriteOnly bool
			selected bool
			selectedId Id
			reservations int
			reserved, probe, sentBytes ByteCount
			next, currentDeadline time.Time
			currentOwnsFifo bool
			retained int
			retries uint64
		}
		type writerObservation struct {
			state observation
			wire []byte
		}
		ctx, cancel := context.WithCancel(context.Background())
		destination := NewId()
		direct, relay := make(Route, 1), make(Route, 1)
		initials := make(chan observation, 3)
		resumeInitial := [3]chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
		wireReady := make(chan writerObservation, 1)
		var currentWitness []byte
		prefixDue, resumePrefixDue := make(chan observation, 1), make(chan struct{})
		callbacks := make(chan error, 3)
		offerResults := make(chan bool, 3)
		var producers sync.WaitGroup
		var stateLock sync.Mutex
		var tailFirst observation
		var tailSeen, initialCallbacks, terminalCallbacks int
		var initialOverflows, terminalOverflows, wireOverflows, wireObservations int
		prefixObserved := false
		var client *Client

		// Capture is called only on the actual source worker. The selector's
		// pure provider read avoids recursively entering its test-access hook.
		capture := func(sequence *SendSequence) observation {
			value := observation{at: time.Now(), retained: len(sequence.sendItems), retries: sequence.resendWriteCount.Load()}
			for _, item := range sequence.sendItems {
				if item == nil || item.sequenceNumber >= 3 {
					continue
				}
				n := item.sequenceNumber
				value.ids[n], value.present[n], value.written[n] = item.messageId, true, item.transportWriteObserved
				value.tracked[n], value.reliable[n], value.unreliable[n] = item.unreliableFlightTracked, item.reliableCarrierObserved, item.unreliableCarrierObserved
				value.frameBytes[n], value.frameCapacity[n] = len(item.transferFrameBytes), cap(item.transferFrameBytes)
				value.writePending[n] = item.rttState == sendItemRttWritePending
				value.sent[n], value.due[n] = item.sendTime, item.resendTime
				value.physical[n] = sequence.firstPhysicalRecoveryTime(item)
				value.lifetime[n], value.pacingBytes[n] = item.sendTime.Add(item.ackTimeout), item.pacingByteCount
				value.kinds[n] = item.recoveryKind
			}
			value.pendingA = sequence.ackWindow.PendingDispositionFor(0, value.ids[0])
			value.pendingB = sequence.ackWindow.PendingDispositionFor(1, value.ids[1])
			value.pendingCumulative = sequence.ackWindow.PendingCumulativeProgress()
			if provider, ok := sequence.contractMultiRouteWriter.(transferFlightPolicyProvider); ok {
				policy := provider.transferFlightPolicy()
				value.h1Only, value.h1ReliableWriteOnly = policy.h1Only, policy.h1ReliableWriteOnly
				value.reliableOnly = sequence.projectedReliableOnlyWrite(policy)
			}
			if selection := sequence.pendingRecovery; selection != nil {
				value.selected, value.selectedId = true, selection.messageId
			}
			if service := sequence.windowPacer.service; service != nil {
				func() {
					service.stateLock.Lock()
					defer service.stateLock.Unlock()
					value.reservations, value.reserved = service.pacingReservations, service.reservedByteCount
					value.next, value.probe, value.sentBytes = service.next, service.probeSent, service.sent
					value.currentOwnsFifo = service.waiterHead == &sequence.windowPacer.waiter &&
						service.waiterTail == &sequence.windowPacer.waiter
				}()
			}
			value.currentDeadline = sequence.windowPacer.waiter.deadline
			return value
		}
		settings := DefaultClientSettings()
		if settings.ControlPingTimeout != 0 {
			t.Fatal("fixture requires the ordinary disabled control-ping default")
		}
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
		settings.SendBufferSettings.ApplyWindowSizing()
		settings.SendBufferSettings.TransferWireMessageObserver = func(wire TransferWireMessageObservation) {
			if wire.Resend || wire.SequenceNumber != 2 {
				return
			}
			sequence := client.sendBuffer.lookupSendSequence(sendSequenceId{Destination: destination}, nil)
			value := writerObservation{
				state: capture(sequence), wire: MessagePoolShareReadOnly(wire.WireMessageBytes),
			}
			stateLock.Lock()
			wireObservations++
			stateLock.Unlock()
			select {
			case wireReady <- value:
			default:
				MessagePoolReturn(value.wire)
				stateLock.Lock()
				wireOverflows++
				stateLock.Unlock()
			}
		}
		settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
			if id.Destination != destination || number >= 3 {
				return
			}
			sequence := client.sendBuffer.lookupSendSequence(id, nil)
			value := capture(sequence)
			stateLock.Lock()
			initialCallbacks++
			stateLock.Unlock()
			select {
			case initials <- value:
			default:
				stateLock.Lock()
				initialOverflows++
				stateLock.Unlock()
			}
			select {
			case <-ctx.Done():
			case <-resumeInitial[number]:
			}
		}
		settings.SendBufferSettings.beforeDueResendForTest = func(id sendSequenceId, number uint64) {
			if id.Destination != destination || number > 1 {
				return
			}
			value := capture(client.sendBuffer.lookupSendSequence(id, nil))
			if number == 1 {
				stateLock.Lock()
				tailSeen++
				if tailSeen == 1 {
					tailFirst = value
				}
				stateLock.Unlock()
				return
			}
			if prefixObserved {
				return
			}
			prefixObserved = true
			select {
			case prefixDue <- value:
				select {
				case <-ctx.Done():
				case <-resumePrefixDue:
				}
			default:
			}
		}
		client = NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		defer func() {
			cancel()
			producers.Wait()
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			drainFlightGateRoute(direct)
			drainFlightGateRoute(relay)
			if currentWitness != nil {
				MessagePoolReturn(currentWitness)
				currentWitness = nil
			}
			for len(wireReady) > 0 {
				MessagePoolReturn((<-wireReady).wire)
			}
			stateLock.Lock()
			initialOverflow, terminalOverflow, wireOverflow := initialOverflows, terminalOverflows, wireOverflows
			stateLock.Unlock()
			if initialOverflow != 0 || terminalOverflow != 0 || wireOverflow != 0 {
				t.Errorf("bounded observation overflow after joined cleanup: initial=%d terminal=%d wire=%d", initialOverflow, terminalOverflow, wireOverflow)
			}
		}()
		client.ContractManager().AddNoContractPeer(destination)
		properties := TransferCarrierProperties{Unreliable: true}
		properties.unreliableFlightMessageLimit = 1
		client.RouteManager().UpdateTransportWithProperties(
			NewSendGatewayTransportWithType(TransportTypeP2p), []Route{direct}, properties)
		send := func() {
			producers.Add(1)
			go func() {
				defer producers.Done()
				frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(4000)}
				clear(frame.MessageBytes)
				ok := client.SendWithTimeout(frame, destination, func(err error) {
					stateLock.Lock()
					terminalCallbacks++
					stateLock.Unlock()
					select {
					case callbacks <- err:
					default:
						stateLock.Lock()
						terminalOverflows++
						stateLock.Unlock()
					}
				}, -1)
				if !ok {
					MessagePoolReturn(frame.MessageBytes)
				}
				offerResults <- ok
			}()
		}
		// The source must already own its exact initial-result barrier at
		// these boundaries; do not wait for a different fake-clock instant.
		takeInitial := func() observation {
			select {
			case value := <-initials:
				return value
			default:
				t.Fatal("source missed the actual initial-result barrier")
				return observation{}
			}
		}
		requireOffer := func() {
			select {
			case ok := <-offerResults:
				if !ok {
					t.Fatal("actual public offer was refused")
				}
			default:
				t.Fatal("public offer did not return at the owned source barrier")
			}
		}
		// Decode while the route share is owned here and return it even if a
		// malformed fixture wire fails. No protobuf or slice survives this call.
		take := func(route Route, wantNumber uint64, keep bool) (Id, Id) {
			var wire []byte
			select {
			case wire = <-route:
			default:
				t.Fatalf("written original has no physical route buffer: number=%d", wantNumber)
			}
			defer func() {
				if keep {
					// B's initial-result barrier still owns Run; the slot we
					// just removed is free and no other writer can take it.
					route <- wire
				} else {
					MessagePoolReturn(wire)
				}
			}()
			pack := decodeSendPackLifecycleWirePack(t, wire)
			id, err := IdFromBytes(pack.MessageId)
			sequenceId, sequenceErr := IdFromBytes(pack.SequenceId)
			if err != nil || sequenceErr != nil || pack.SequenceNumber != wantNumber {
				t.Fatalf("physical identity: number=%d want=%d message=%v sequence=%v", pack.SequenceNumber, wantNumber, err, sequenceErr)
			}
			return id, sequenceId
		}

		send()
		synctest.Wait()
		first := takeInitial()
		requireOffer()
		if !first.written[0] || first.retained != 1 {
			t.Fatal("prefix A did not receive its actual original result")
		}
		prefixId, sourceId := take(direct, 0, false)
		sequence := client.sendBuffer.lookupSendSequence(sendSequenceId{Destination: destination}, nil)
		client.RouteManager().UpdateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{relay})
		time.Sleep(time.Nanosecond)
		send()
		close(resumeInitial[0])
		synctest.Wait()
		second := takeInitial()
		requireOffer()
		if !second.written[1] || second.retained != 2 {
			t.Fatal("tail B did not receive its actual original result")
		}
		tailId, tailSourceId := take(relay, 1, true)
		if prefixId != second.ids[0] || tailId != second.ids[1] || sourceId != tailSourceId ||
			first.due[0] != first.sent[0].Add(2*time.Second) ||
			second.due[1] != second.physical[1].Add(2*time.Second) ||
			second.physical[1].Sub(first.sent[0]) != time.Nanosecond ||
			first.lifetime[0] != first.sent[0].Add(90*time.Second) ||
			second.lifetime[1] != second.sent[1].Add(60*time.Second) ||
			second.h1Only || !second.reliableOnly || !second.h1ReliableWriteOnly ||
			!second.tracked[0] || !second.unreliable[0] || second.reliable[0] ||
			second.tracked[1] || !second.reliable[1] || second.unreliable[1] {
			t.Fatalf("fixture changed cold clocks/lifetimes or mixed overflow: A=%+v B=%+v", first, second)
		}

		service := sequence.windowPacer.service
		service.stateLock.Lock()
		expiry := service.burst.start.Add(windowPacingBurstMaximumTime(service.burstEstimateTime))
		service.stateLock.Unlock()
		if !expiry.After(time.Now()) || !expiry.Before(first.due[0]) {
			t.Fatal("opening burst cannot expire before the real prefix deadline")
		}
		time.Sleep(time.Until(expiry))
		releaseAt := second.due[1].Add(time.Second)
		send()
		close(resumeInitial[1])
		synctest.Wait()
		var ready writerObservation
		select {
		case ready = <-wireReady:
		default:
			t.Fatal("C missed its real shared-write observation before selector backpressure")
		}
		currentWitness = ready.wire
		ready.wire = nil
		held := ready.state
		requireOffer()
		if held.at != expiry || held.written[2] || !held.writePending[2] || held.retained != 3 ||
			held.reservations != 0 || held.reserved != 0 || held.currentOwnsFifo ||
			held.pacingBytes[2] != ByteCount(held.frameBytes[2]) || held.h1Only || !held.reliableOnly ||
			held.frameBytes[2] <= 0 || len(currentWitness) != held.frameBytes[2] ||
			len(direct) != 0 || len(relay) != 1 || held.retries != 0 {
			t.Fatalf("C did not own its already-shared route write after one paid admission: %+v", held)
		}
		// This is a real, explicitly shared witness, not a borrowed pointer.
		// The original writer still owns its distinct unconsumed route share.
		witnessPack := decodeSendPackLifecycleWirePack(t, currentWitness)
		witnessId, witnessErr := IdFromBytes(witnessPack.MessageId)
		witnessSource, witnessSourceErr := IdFromBytes(witnessPack.SequenceId)
		if witnessErr != nil || witnessSourceErr != nil || witnessId != held.ids[2] ||
			witnessSource != sourceId || witnessPack.SequenceNumber != 2 {
			t.Fatal("borrowed wire witness did not identify the exact current original")
		}
		witnessPack = nil
		ack := &protocol.Ack{MessageId: prefixId.Bytes(), SequenceId: sourceId.Bytes(), Selective: selective}
		if ok, err := sequence.Ack(ack, 0); !ok || err != nil {
			t.Fatalf("prefix feedback was refused: %t %v", ok, err)
		}
		if !sequence.ackWindow.PendingDispositionFor(0, prefixId) ||
			sequence.ackWindow.PendingDispositionFor(1, tailId) ||
			sequence.ackWindow.PendingCumulativeProgress() == selective {
			t.Fatal("pending prefix feedback covered the tail or changed its kind")
		}
		time.Sleep(time.Until(first.due[0]))
		synctest.Wait()
		var atPrefix observation
		select {
		case atPrefix = <-prefixDue:
		default:
			t.Fatal("source missed A's exact original due hook before the masked-tail comparison")
		}
		if atPrefix.at != first.due[0] || !atPrefix.pendingA || atPrefix.pendingB ||
			atPrefix.pendingCumulative == selective || atPrefix.h1Only ||
			atPrefix.ids[1] != tailId || atPrefix.due[1] != second.due[1] ||
			atPrefix.lifetime[1] != second.lifetime[1] || atPrefix.tracked[1] ||
			!atPrefix.reliable[1] || atPrefix.unreliable[1] || atPrefix.written[2] || !atPrefix.writePending[2] ||
			atPrefix.retained != 3 || atPrefix.ids[2] != held.ids[2] ||
			atPrefix.frameBytes[2] != held.frameBytes[2] || atPrefix.frameCapacity[2] != held.frameCapacity[2] {
			t.Fatal("prefix due boundary lost the uncovered tail or held original")
		}
		close(resumePrefixDue)
		// This exact1ns clock advance is stimulus; the by-value owner-hook
		// witness, not a missing-event timeout, decides whether B was serviced.
		time.Sleep(time.Until(second.due[1]))
		synctest.Wait()
		stateLock.Lock()
		seen, atTail, terminals := tailSeen, tailFirst, terminalCallbacks
		stateLock.Unlock()
		if seen != 1 || atTail.at != second.due[1] || atTail.ids[1] != tailId ||
			!atTail.pendingA || atTail.pendingB || atTail.pendingCumulative == selective ||
			atTail.kinds[1] != sendRecoveryNone || atTail.tracked[1] ||
			!atTail.reliable[1] || atTail.unreliable[1] || atTail.written[2] || !atTail.writePending[2] ||
			atTail.retained != 3 || atTail.ids[2] != held.ids[2] ||
			atTail.frameBytes[2] != held.frameBytes[2] || atTail.frameCapacity[2] != held.frameCapacity[2] {
			t.Errorf("pending-covered heap prefix masked the uncovered tail's exact due boundary: selective=%t seen=%d observed=%s want=%s",
				selective, seen, atTail.at, second.due[1])
		}
		service.stateLock.Lock()
		kept := service.next == held.next && service.probeSent == held.probe && service.sent == held.sentBytes &&
			service.pacingReservations == 0 && service.reservedByteCount == 0 &&
			service.waiterHead == nil && service.waiterTail == nil
		service.stateLock.Unlock()
		if !kept || terminals != 0 || len(relay) != 1 || len(direct) != 0 ||
			!sequence.ackWindow.PendingDispositionFor(0, prefixId) {
			t.Fatal("nested policy consumed ACK ownership, the blocked route buffer or C's already-paid debt")
		}

		time.Sleep(time.Until(releaseAt))
		// Release actual B's occupied channel slot, not a fabricated writer.
		if id, sequenceId := take(relay, 1, false); id != tailId || sequenceId != sourceId {
			t.Fatal("the relay hold did not remain B's exact physical original")
		}
		synctest.Wait()
		current := takeInitial()
		if current.at != releaseAt || !current.written[2] ||
			current.lifetime[1] != second.lifetime[1] || current.retries != 0 ||
			!current.pendingA || current.pendingB ||
			current.retained != 3 || current.ids[2] != held.ids[2] ||
			current.frameBytes[2] != held.frameBytes[2] || current.frameCapacity[2] != held.frameCapacity[2] {
			t.Fatal("C did not complete at actual route release before physical recovery")
		}
		if seen > 0 && (!current.selected || current.selectedId != tailId) {
			t.Fatal("eligible B was observed but its retained selection did not survive C's original write")
		}
		var currentWire []byte
		select {
		case currentWire = <-relay:
		default:
			t.Fatal("C's actual successful route share was absent")
		}
		currentId := func() Id {
			defer func() {
				if MessagePoolReturn(currentWire) {
					t.Error("route share was the last reference while the explicit witness remained owned")
				}
			}()
			if len(currentWire) != len(currentWitness) || &currentWire[0] != &currentWitness[0] {
				t.Fatal("blocked selector accepted a replacement instead of the exact original share")
			}
			pack := decodeSendPackLifecycleWirePack(t, currentWire)
			id, err := IdFromBytes(pack.MessageId)
			wireSource, sourceErr := IdFromBytes(pack.SequenceId)
			if err != nil || sourceErr != nil || id != held.ids[2] ||
				wireSource != sourceId || pack.SequenceNumber != 2 {
				t.Fatal("current original changed physical identity")
			}
			return id
		}()
		currentWire = nil
		lastReference := MessagePoolReturn(currentWitness)
		currentWitness = nil
		if !lastReference {
			t.Fatal("completed copied write retained an extra original/route/witness reference")
		}
		// The causal boundary is complete. Keep C's later timer from tying
		// B's legitimate post-cumulative deferral; this feedback never covers B.
		if ok, err := sequence.Ack(&protocol.Ack{MessageId: currentId.Bytes(), SequenceId: sourceId.Bytes(), Selective: true}, 0); !ok || err != nil {
			t.Fatalf("current selective ACK was refused: %t %v", ok, err)
		}
		if sequence.ackWindow.PendingDispositionFor(1, tailId) {
			t.Fatal("current selective feedback incorrectly covered B")
		}
		close(resumeInitial[2])
		var retryWire []byte
		select {
		case retryWire = <-relay:
		case retryWire = <-direct:
		case <-sequence.ctx.Done():
			t.Fatalf("source exited before B's actual recovery: %v", sequence.ctx.Err())
		}
		func() {
			defer MessagePoolReturn(retryWire)
			pack := decodeSendPackLifecycleWirePack(t, retryWire)
			id, err := IdFromBytes(pack.MessageId)
			retrySource, sourceErr := IdFromBytes(pack.SequenceId)
			if err != nil || sourceErr != nil || id != tailId || retrySource != sourceId || pack.SequenceNumber != 1 {
				t.Fatal("first recovery after C was not the exact uncovered B original")
			}
		}()
		if ok, err := sequence.Ack(&protocol.Ack{MessageId: currentId.Bytes(), SequenceId: sourceId.Bytes()}, 0); !ok || err != nil {
			t.Fatalf("terminal cumulative ACK was refused: %t %v", ok, err)
		}
		for range 3 {
			select {
			case err := <-callbacks:
				if err != nil {
					t.Fatalf("original terminal result: %v", err)
				}
			case <-sequence.ctx.Done():
				t.Fatalf("source exited before all terminal results: %v", sequence.ctx.Err())
			}
		}
		synctest.Wait()
		count, bytes := sequence.resendQueue.QueueSize()
		stateLock.Lock()
		initialCount, terminalCount, wireCount := initialCallbacks, terminalCallbacks, wireObservations
		finalTailSeen, finalTailAt := tailSeen, tailFirst.at
		stateLock.Unlock()
		if count != 0 || bytes != 0 || initialCount != 3 || terminalCount != 3 || wireCount != 1 {
			t.Fatal("original terminal ownership did not retire exactly once")
		}
		t.Logf("shared_writer_masked_tail selective=%t first_tail=%s want=%s observations=%d initial=%d terminal=%d retained=%d bytes=%d",
			selective, finalTailAt, second.due[1], finalTailSeen, initialCount, terminalCount, count, bytes)
	})
}
