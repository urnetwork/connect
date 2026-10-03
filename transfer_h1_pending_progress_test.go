// The real receiver produces the SACK and cumulative ACK; the fixture only
// controls bounded route handoffs and drops two originals at the endpoint.
package connect

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func TestH1DueTimeoutPendingLowerCumulativeProgressPreemptsWrite(t *testing.T) {
	runH1DueLowerPrefix(t, "cumulative", false)
}

func TestH1DueTimeoutNoPendingProgressStillRecovers(t *testing.T) {
	runH1DueLowerPrefix(t, "none", false)
}

func TestH1DueTimeoutLowerSelectiveFeedbackStillRecovers(t *testing.T) {
	runH1DueLowerPrefix(t, "selective", false)
}

func TestH1DueTimeoutPendingProgressPreservesRetainedTailLifetime(t *testing.T) {
	runH1DueLowerPrefix(t, "cumulative", true)
}

func runH1DueLowerPrefix(t *testing.T, pendingFeedback string, retainTail bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		senderID, receiverID := NewId(), NewId()
		senderOut, senderIn := make(Route, 32), make(Route, 32)
		receiverOut, receiverIn := make(Route, 32), make(Route, 32)
		due, releaseDue := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(releaseDue) }) }
		tailInspected := make(chan struct{})
		var tailInspectedOnce sync.Once
		releaseTailInspection := func() { tailInspectedOnce.Do(func() { close(tailInspected) }) }
		var intercepted, prefixApplied atomic.Bool
		var tailAttemptsBeforePrefix atomic.Uint64
		var sender *Client
		var duePrecondition string
		var dueAt time.Time

		newSettings := func() *ClientSettings {
			settings := DefaultClientSettings()
			settings.EncryptionSettings.Mode = EncryptionModeOff
			settings.Log = NewNoopLogger()
			settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
			return settings
		}
		senderSettings := newSettings()
		defaults := DefaultSendBufferSettings()
		if !senderSettings.SendBufferSettings.DeferTimeoutResendWhileCumulativeProgress ||
			!senderSettings.SendBufferSettings.DeferTimeoutResendBackoff ||
			senderSettings.SendBufferSettings.ReliableTimerUsesDeviation ||
			senderSettings.SendBufferSettings.ReliableLaneProvenRecovery ||
			senderSettings.SendBufferSettings.MinResendInterval != defaults.MinResendInterval ||
			senderSettings.SendBufferSettings.RttMinResendInterval != defaults.RttMinResendInterval ||
			senderSettings.SendBufferSettings.AckTimeout != defaults.AckTimeout {
			t.Fatal("fixture must retain production timeout policy")
		}
		senderSettings.SendBufferSettings.afterAckSendItemForTest = func(id sendSequenceId, number uint64) {
			if id.Destination == receiverID && number == 2 {
				prefixApplied.Store(true)
			}
		}
		senderSettings.SendBufferSettings.TransferWireMessageObserver = func(observation TransferWireMessageObservation) {
			if observation.SequenceNumber == 3 && observation.Resend && !prefixApplied.Load() {
				tailAttemptsBeforePrefix.Add(1)
			}
		}
		senderSettings.SendBufferSettings.beforeDueResendForTest = func(id sendSequenceId, number uint64) {
			if id.Destination != receiverID || number != 3 {
				return
			}
			if intercepted.Load() {
				// Fake-time sleeping is not a race synchronization edge.
				// Publish completion of the deferred-owner inspection before
				// a subsequent timer is allowed to mutate the same retained item.
				if time.Now().After(dueAt) {
					<-tailInspected
				}
				return
			}
			sequence := sender.sendBuffer.lookupSendSequence(id, nil)
			if sequence == nil {
				return
			}
			item := sequence.resendQueue.PeekFirst()
			if item == nil || item.sequenceNumber != number {
				return
			}
			now := time.Now()
			// Let existing residence protection run naturally. Advancing fake
			// time while parked would not update Run's captured sendTime.
			deadline := time.Time{}
			if service := sequence.windowPacer.service; service != nil {
				deadline = service.probeRecoveryDeadline(sequence.sequenceId, item.messageId,
					sequence.sendBufferSettings.RttScale, sequence.sendBufferSettings.MaxResendInterval)
				if interval := sequence.sharedRawRecoveryInterval(item, now); interval > 0 && item.pacingSentAtNanos != 0 {
					physical := sequence.firstPhysicalRecoveryTime(item).Add(max(interval, sequence.resendIntervalForItem(item, 1)))
					if deadline.Before(physical) {
						deadline = physical
					}
				}
			}
			if now.Before(deadline) {
				return
			}
			if !intercepted.CompareAndSwap(false, true) {
				return
			}
			dueAt = now
			if item.recoveryKind != sendRecoveryNone || item.sendCount != 1 ||
				!item.reliableCarrierObserved || item.unreliableCarrierObserved || item.carrierChanged ||
				!sequence.transferFlightPolicy().h1Only ||
				sequence.laneTimerVerdictFor(item) != laneTimerNotApplicable ||
				sequence.shouldDeferTimeoutResend(item, sequence.rttWindow.ScaledRtt()) ||
				!now.Before(item.sendTime.Add(item.ackTimeout)) {
				duePrecondition = fmt.Sprintf("not an unprotected ordinary H1 tail: kind=%d count=%d reliable=%t unreliable=%t changed=%t",
					item.recoveryKind, item.sendCount, item.reliableCarrierObserved, item.unreliableCarrierObserved, item.carrierChanged)
			}
			close(due)
			<-releaseDue
		}
		sender = NewClient(ctx, senderID, NewNoContractClientOob(), senderSettings)
		receiver := NewClient(ctx, receiverID, NewNoContractClientOob(), newSettings())
		sender.ContractManager().AddNoContractPeer(receiverID)
		receiver.ContractManager().AddNoContractPeer(senderID)
		sender.RouteManager().UpdateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{senderOut})
		sender.RouteManager().UpdateTransport(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{senderIn})
		receiver.RouteManager().UpdateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{receiverOut})
		receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{receiverIn})
		defer func() {
			release()
			releaseTailInspection()
			cancel()
			for _, client := range []*Client{sender, receiver} {
				if err := client.CloseAndWait(context.Background()); err != nil {
					t.Errorf("join H1 fixture: %v", err)
				}
			}
			for _, route := range []Route{senderOut, senderIn, receiverOut, receiverIn} {
				drainFlightGateRoute(route)
			}
		}()

		callbacks := make(chan error, 4)
		delivered := make(chan string, 4)
		var callbackOverflow, deliveryOverflow atomic.Bool
		receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				message, err := FromFrame(frame)
				if err == nil {
					if simple, ok := message.(*protocol.SimpleMessage); ok {
						select {
						case delivered <- simple.Content:
						default:
							deliveryOverflow.Store(true)
						}
					}
				}
			}
		})
		take := func(route Route, label string) []byte {
			select {
			case wire := <-route:
				return wire
			case <-time.After(10 * time.Second):
				t.Fatalf("H1 fixture could not reach %s", label)
				return nil
			}
		}
		send := func(content string, number uint64) ([]byte, *protocol.Pack) {
			frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: content})
			if !sender.SendWithTimeout(frame, receiverID, func(err error) {
				select {
				case callbacks <- err:
				default:
					callbackOverflow.Store(true)
				}
			}, time.Second, sendPackRecoveryOption{retainAfterAckTimeout: retainTail && number == 3}) {
				MessagePoolReturn(frame.MessageBytes)
				t.Fatalf("admit %s", content)
			}
			wire := take(senderOut, content)
			pack := decodeSendPackLifecycleWirePack(t, wire)
			if pack.SequenceNumber != number || len(pack.Frames) != 1 {
				MessagePoolReturn(wire)
				t.Fatalf("%s did not retain its separate sequence position", content)
			}
			return wire, pack
		}
		takeAck := func(pack *protocol.Pack, selective bool) []byte {
			wire := take(receiverOut, "real receiver ACK")
			var envelope protocol.TransferFrame
			if err := ProtoUnmarshal(wire, &envelope); err != nil {
				MessagePoolReturn(wire)
				t.Fatalf("decode receiver ACK: %v", err)
			}
			ack := envelope.Ack
			if ack == nil || Id(ack.MessageId) != Id(pack.MessageId) || ack.Selective != selective {
				MessagePoolReturn(wire)
				t.Fatalf("unexpected receiver feedback, want number=%d selective=%t", pack.SequenceNumber, selective)
			}
			return wire
		}

		// Establish the receiver's real head before dropping a later original.
		warmWire, warm := send("warm", 0)
		receiverIn <- warmWire
		senderIn <- takeAck(warm, false)
		synctest.Wait()
		// Immediate loopback ACKs have no nonzero RTT sample, so retain the
		// production cold timer and let prior progress grow stale beyond it.
		time.Sleep(2 * senderSettings.SendBufferSettings.MinResendInterval)
		gapWire, gap := send("gap", 1)
		MessagePoolReturn(gapWire) // endpoint loses only the original G
		time.Sleep(time.Millisecond)
		prefixWire, prefix := send("prefix", 2)
		receiverIn <- prefixWire
		selectiveAck := takeAck(prefix, true) // real out-of-order receiver storage
		selectiveReplay := MessagePoolCopy(selectiveAck)
		defer func() {
			if selectiveReplay != nil {
				MessagePoolReturn(selectiveReplay)
			}
		}()
		senderIn <- selectiveAck
		synctest.Wait()
		time.Sleep(time.Millisecond)
		tailWire, tail := send("tail", 3)
		MessagePoolReturn(tailWire) // endpoint loses only the original Y
		synctest.Wait()

		// One later SACK is below the unchanged three-ACK gap threshold.
		// G's natural ordinary retry closes the actual receive gap through P.
		gapRetry := take(senderOut, "natural G timeout retry")
		if retry := decodeSendPackLifecycleWirePack(t, gapRetry); Id(retry.MessageId) != Id(gap.MessageId) {
			MessagePoolReturn(gapRetry)
			t.Fatal("first natural recovery was not the missing lower head")
		}
		receiverIn <- gapRetry
		prefixAck := takeAck(prefix, false)
		defer func() {
			if prefixAck != nil {
				MessagePoolReturn(prefixAck)
			}
		}()
		select {
		case <-due:
		case <-time.After(10 * time.Second):
			t.Fatal("no naturally due, residence-eligible Y before the original lifetime")
		}
		if duePrecondition != "" {
			t.Fatal(duePrecondition)
		}
		sequence := flightGateSendSequence(t, sender, receiverID)
		tailID := Id(tail.MessageId)
		before := sender.SendRecoveryStats()
		switch pendingFeedback {
		case "cumulative":
			senderIn <- prefixAck
			prefixAck = nil
		case "selective":
			senderIn <- selectiveReplay
			selectiveReplay = nil
		case "none":
		default:
			t.Fatalf("unknown pending feedback %q", pendingFeedback)
		}
		synctest.Wait() // real Client ACK ingress and coalescer; owner is parked
		if sequence.ackWindow.PendingCumulativeProgress() != (pendingFeedback == "cumulative") ||
			sequence.ackWindow.PendingDispositionFor(tail.SequenceNumber, tailID) || prefixApplied.Load() {
			t.Fatalf("fixture failed to establish %s lower feedback after the owner snapshot", pendingFeedback)
		}
		// The ACK coalescer can publish shared timing before Run applies its
		// prefix. A new raw-residence bound must not mask the intended RED.
		parkedTail := sequence.resendQueue.GetByMessageId(tailID)
		if parkedTail == nil {
			t.Fatal("pending lower feedback lost the unacknowledged tail owner")
		}
		originalTailLifetime := parkedTail.sendTime.Add(parkedTail.ackTimeout)
		if service := sequence.windowPacer.service; service != nil {
			deadline := service.probeRecoveryDeadline(sequence.sequenceId, tailID,
				sequence.sendBufferSettings.RttScale, sequence.sendBufferSettings.MaxResendInterval)
			if interval := sequence.sharedRawRecoveryInterval(parkedTail, dueAt); interval > 0 && parkedTail.pacingSentAtNanos != 0 {
				physical := sequence.firstPhysicalRecoveryTime(parkedTail).Add(max(interval, sequence.resendIntervalForItem(parkedTail, 1)))
				if deadline.Before(physical) {
					deadline = physical
				}
			}
			if dueAt.Before(deadline) {
				t.Fatal("fixture ACK refreshed raw residence after parking; this is not the predicted stale-progress RED")
			}
		}
		release()
		synctest.Wait()
		extraTailWrites := 0
		var tailRetry []byte
		defer func() {
			if tailRetry != nil {
				MessagePoolReturn(tailRetry)
			}
		}()
		for len(senderOut) > 0 {
			wire := <-senderOut
			pack := decodeSendPackLifecycleWirePack(t, wire)
			if Id(pack.MessageId) == tailID {
				extraTailWrites++
				if pendingFeedback != "cumulative" && tailRetry == nil {
					tailRetry = wire
					continue
				}
			}
			MessagePoolReturn(wire)
		}
		after := sender.SendRecoveryStats()
		if pendingFeedback == "cumulative" {
			if extraTailWrites != 0 || tailAttemptsBeforePrefix.Load() != 0 {
				t.Fatalf("pending lower-prefix ACK lost to ordinary H1 retry at %s: actual extra Y route writes=%d, Y attempts before prefix applied=%d (want0/0)",
					dueAt, extraTailWrites, tailAttemptsBeforePrefix.Load())
			}
			if !prefixApplied.Load() || after.AckPendingResendPreemptCount != before.AckPendingResendPreemptCount+1 ||
				after.TimeoutResendWriteCount != before.TimeoutResendWriteCount {
				t.Fatalf("lower prefix did not preempt exactly once before the unchanged deferral: before=%+v after=%+v", before, after)
			}
			item := sequence.resendQueue.GetByMessageId(tailID)
			if item == nil || item.sendCount != 1 || !item.resendTime.After(time.Now()) ||
				sequence.resendQueue.Len() != 1 || len(callbacks) != 3 {
				t.Fatal("lower ACK incorrectly retired Y or failed to preserve its ordinary future retry")
			}
			// No fresh progress follows. A truly missing tail must still retry.
			resendAt := item.resendTime
			releaseTailInspection()
			time.Sleep(time.Until(resendAt))
			synctest.Wait()
			tailRetry = take(senderOut, "unchanged lost-tail recovery")
		} else {
			if extraTailWrites != 1 || tailAttemptsBeforePrefix.Load() != 1 || prefixApplied.Load() ||
				after.AckPendingResendPreemptCount != before.AckPendingResendPreemptCount ||
				after.TimeoutResendWriteCount != before.TimeoutResendWriteCount+1 {
				t.Fatalf("%s feedback postponed ordinary recovery: Y actual writes=%d attempts=%d before=%+v after=%+v",
					pendingFeedback, extraTailWrites, tailAttemptsBeforePrefix.Load(), before, after)
			}
		}
		if retainTail {
			// The prefix deferral must not remove the existing ownership
			// exception for bytes that their upstream cannot regenerate.
			// Keep the first retry in the route fixture and hold every reply
			// beyond the unchanged production ACK lifetime.
			time.Sleep(time.Until(originalTailLifetime.Add(time.Nanosecond)))
			synctest.Wait()
			if sequence.ctx.Err() != nil || sequence.resendQueue.GetByMessageId(tailID) != parkedTail ||
				sequence.resendQueue.Len() != 1 || len(callbacks) != 3 {
				t.Fatal("pending-prefix recovery lost retained ownership at the ordinary ACK lifetime")
			}
			for len(senderOut) > 0 {
				wire := <-senderOut
				pack := decodeSendPackLifecycleWirePack(t, wire)
				MessagePoolReturn(wire)
				if Id(pack.MessageId) != tailID {
					t.Fatal("retained recovery changed the original tail identity")
				}
			}
		}
		if retry := decodeSendPackLifecycleWirePack(t, tailRetry); Id(retry.MessageId) != tailID {
			t.Fatal("lost-tail recovery changed ownership identity")
		}
		receiverIn <- tailRetry
		tailRetry = nil
		senderIn <- takeAck(tail, false)
		synctest.Wait()
		if sequence.ctx.Err() != nil || sequence.resendQueue.Len() != 0 || len(sequence.ackLifetimes.items) != 0 || len(callbacks) != 4 ||
			callbackOverflow.Load() || deliveryOverflow.Load() {
			t.Fatal("lost-tail ACK failed to settle all original owners exactly once")
		}
		for range 4 {
			if err := <-callbacks; err != nil {
				t.Fatalf("original send failed: %v", err)
			}
		}
		for _, want := range []string{"warm", "gap", "prefix", "tail"} {
			select {
			case got := <-delivered:
				if got != want {
					t.Fatalf("ordered receiver content=%q, want%q", got, want)
				}
			default:
				t.Fatalf("missing actual receiver content %q", want)
			}
		}
	})
}
