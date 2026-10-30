// The send, receive and forward loops keep one lazily armed deadline: a pass
// that leaves the next deadline where it was, or moves it later, does not
// touch the timer. These tests count the arms per Pack and pin the deadlines
// that must never be late: resend, gap and idle.
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

// Two no-contract clients joined by routes the test owns. a's writes reach b
// through a relay that drops the data Packs drop names; b's writes,
// its acknowledgements, reach a directly.
type deadlineTimerTestPair struct {
	a        *Client
	b        *Client
	aId      Id
	bId      Id
	received atomic.Int64
	// Unset forwards everything. Reports whether to drop one data Pack.
	drop atomic.Pointer[func(pack *protocol.Pack) bool]
}

func newDeadlineTimerTestPair(
	t *testing.T,
	configure func(a *ClientSettings, b *ClientSettings),
) *deadlineTimerTestPair {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	newSettings := func() *ClientSettings {
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		return settings
	}
	aSettings, bSettings := newSettings(), newSettings()
	if configure != nil {
		configure(aSettings, bSettings)
	}
	pair := &deadlineTimerTestPair{aId: NewId(), bId: NewId()}
	aOut := make(Route, 256)
	bIn := make(Route, 256)
	bOut := make(Route, 256)
	pair.a = NewClient(ctx, pair.aId, NewNoContractClientOob(), aSettings)
	pair.b = NewClient(ctx, pair.bId, NewNoContractClientOob(), bSettings)
	pair.a.ContractManager().AddNoContractPeer(pair.bId)
	pair.b.ContractManager().AddNoContractPeer(pair.aId)
	pair.a.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{aOut})
	pair.b.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{bIn})
	pair.b.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{bOut})
	pair.a.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{bOut})
	pair.b.AddReceiveCallback(func(TransferPath, []*protocol.Frame, Peer) {
		pair.received.Add(1)
	})
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		for {
			select {
			case <-ctx.Done():
				return
			case wire := <-aOut:
				if drop := pair.drop.Load(); drop != nil {
					if pack := deadlineTimerTestDataPack(wire); pack != nil && (*drop)(pack) {
						MessagePoolReturn(wire)
						continue
					}
				}
				select {
				case bIn <- wire:
				case <-ctx.Done():
					MessagePoolReturn(wire)
					return
				}
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-relayDone
		for _, client := range []*Client{pair.a, pair.b} {
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Errorf("close deadline timer test client: %v", err)
			}
		}
		for _, route := range []Route{aOut, bIn, bOut} {
			drainFlightGateRoute(route)
		}
	})
	return pair
}

// Decodes a data Pack, or nil for anything else on the wire.
func deadlineTimerTestDataPack(wire []byte) *protocol.Pack {
	var transferFrame protocol.TransferFrame
	if err := ProtoUnmarshal(wire, &transferFrame); err != nil {
		return nil
	}
	pack := transferFrame.Pack
	if pack == nil {
		frame := transferFrame.GetFrame()
		if frame == nil || frame.GetMessageType() != protocol.MessageType_TransferPack {
			return nil
		}
		pack = &protocol.Pack{}
		if err := ProtoUnmarshal(frame.MessageBytes, pack); err != nil {
			return nil
		}
	}
	if len(pack.Frames) == 0 {
		return nil
	}
	return pack
}

func deadlineTimerTestFrame(t *testing.T, index int) *protocol.Frame {
	t.Helper()
	frame, err := ToFrame(&protocol.SimpleMessage{Content: fmt.Sprintf("deadline-%d", index)}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

// Sends one acknowledged message from a to b and waits for its ACK.
func (self *deadlineTimerTestPair) sendAcked(t *testing.T, index int) {
	t.Helper()
	acked := make(chan error, 1)
	frame := deadlineTimerTestFrame(t, index)
	if !self.a.SendWithTimeout(frame, self.bId, func(err error) { acked <- err }, -1) {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatalf("message %d was not admitted", index)
	}
	select {
	case err := <-acked:
		if err != nil {
			t.Fatalf("message %d failed: %v", index, err)
		}
	case <-time.After(time.Minute):
		t.Fatalf("message %d was not acknowledged", index)
	}
}

// Sends one acknowledged message from a to b without waiting for its ACK.
func (self *deadlineTimerTestPair) send(t *testing.T, index int) {
	t.Helper()
	frame := deadlineTimerTestFrame(t, index)
	if !self.a.SendWithTimeout(frame, self.bId, func(error) {}, -1) {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatalf("message %d was not admitted", index)
	}
}

// A Pack and its ACK each take the send loop around once. A deadline that
// stays put, or moves later, must not re-arm the timer: the stop-and-wait
// exchange arms it for the first deadlines and then about once per resend
// interval, when the armed deadline fires early, not twice per message.
func TestSendSequenceDeadlineTimerArmsPerDeadlineNotPerPack(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		arms := atomic.Int64{}
		var settings *SendBufferSettings
		var pair *deadlineTimerTestPair
		pair = newDeadlineTimerTestPair(t, func(a *ClientSettings, b *ClientSettings) {
			settings = a.SendBufferSettings
			a.SendBufferSettings.afterDeadlineTimerArmForTest = func(id sendSequenceId) {
				if id.Destination == pair.bId {
					arms.Add(1)
				}
			}
		})
		const messageCount = 64
		start := time.Now()
		for index := range messageCount {
			pair.sendAcked(t, index)
		}
		synctest.Wait()
		elapsed := time.Since(start)
		t.Logf("%d messages over %s armed the send loop's timer %d times", messageCount, elapsed, arms.Load())
		if got := pair.received.Load(); got != messageCount {
			t.Fatalf("b received %d messages, want %d", got, messageCount)
		}
		// A resend deadline fires early about once per resend interval, and
		// its recompute can arm twice: for the idle deadline it finds, then for
		// the next Pack's resend deadline.
		if limit := int64(4 + 2*(elapsed/settings.RttMinResendInterval)); limit < arms.Load() {
			t.Fatalf("the send loop armed its timer %d times for %d messages over %s, want at most %d", arms.Load(), messageCount, elapsed, limit)
		}
	})
}

// A sequence whose timer waits on its far idle deadline still resends on time:
// the first resend deadline after it moves the timer earlier.
func TestSendSequenceResendIsNotLateBehindAnArmedIdleDeadline(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		const idleTimeout = 10 * time.Minute
		var settings *SendBufferSettings
		pair := newDeadlineTimerTestPair(t, func(a *ClientSettings, b *ClientSettings) {
			a.SendBufferSettings.IdleTimeout = idleTimeout
			settings = a.SendBufferSettings
		})
		// the sequence is idle, its timer armed for the idle deadline
		pair.sendAcked(t, 0)
		time.Sleep(time.Minute)

		// every copy of the next message is lost
		writes := make(chan time.Time, 16)
		drop := func(pack *protocol.Pack) bool {
			if pack.SequenceNumber == 1 {
				select {
				case writes <- time.Now():
				default:
				}
				return true
			}
			return false
		}
		pair.drop.Store(&drop)
		synctest.Wait()
		pair.send(t, 1)
		var first time.Time
		select {
		case first = <-writes:
		case <-time.After(time.Minute):
			t.Fatal("the message was never written")
		}
		select {
		case resent := <-writes:
			if wait := resent.Sub(first); wait <= 0 || settings.MaxResendInterval < wait {
				t.Fatalf("the first resend came %s after the write, want within MaxResendInterval %s", wait, settings.MaxResendInterval)
			}
		case <-time.After(idleTimeout):
			t.Fatal("the message was not resent before the idle deadline")
		}
	})
}

// The idle close comes exactly IdleTimeout after the last activity. A timer
// still armed for an earlier idle deadline fires early and only re-arms.
func TestSendSequenceIdleCloseFollowsTheLastActivity(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		const idleTimeout = 2 * time.Minute
		closes := make(chan time.Time, 4)
		var pair *deadlineTimerTestPair
		pair = newDeadlineTimerTestPair(t, func(a *ClientSettings, b *ClientSettings) {
			a.SendBufferSettings.IdleTimeout = idleTimeout
			a.SendBufferSettings.afterIdleCloseForTest = func(id sendSequenceId, done bool) {
				if id.Destination == pair.bId && done {
					closes <- time.Now()
				}
			}
		})
		pair.sendAcked(t, 0)
		time.Sleep(idleTimeout / 2)
		pair.sendAcked(t, 1)
		lastActivity := time.Now()
		select {
		case closed := <-closes:
			if got := closed.Sub(lastActivity); got != idleTimeout {
				t.Fatalf("the idle sequence closed %s after its last activity, want %s", got, idleTimeout)
			}
		case <-time.After(10 * idleTimeout):
			t.Fatal("the idle sequence never closed")
		}
	})
}

// Each received Pack takes the receive loop around once. With every deadline
// unchanged or later, the loop arms its timer once, not once per Pack.
func TestReceiveSequenceDeadlineTimerArmsPerDeadlineNotPerPack(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		arms := atomic.Int64{}
		var pair *deadlineTimerTestPair
		pair = newDeadlineTimerTestPair(t, func(a *ClientSettings, b *ClientSettings) {
			b.ReceiveBufferSettings.afterDeadlineTimerArmForTest = func(id receiveSequenceId) {
				if id.Source.SourceId == pair.aId {
					arms.Add(1)
				}
			}
		})
		const messageCount = 64
		for index := range messageCount {
			pair.sendAcked(t, index)
		}
		synctest.Wait()
		t.Logf("%d messages armed the receive loop's timer %d times", messageCount, arms.Load())
		if got := pair.received.Load(); got != messageCount {
			t.Fatalf("b received %d messages, want %d", got, messageCount)
		}
		if limit := int64(4); limit < arms.Load() {
			t.Fatalf("the receive loop armed its timer %d times for %d messages, want at most %d", arms.Load(), messageCount, limit)
		}
	})
}

// A receive sequence for a no-contract source, run directly so that the test
// decides every Pack it receives. Its acknowledgements leave on a route the
// test drains.
func newDeadlineTimerTestReceiveSequence(
	t *testing.T,
	configure func(settings *ReceiveBufferSettings),
) *ReceiveSequence {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := closeWaitClientSettings()
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	sourceId := NewId()
	client.ContractManager().AddNoContractPeer(sourceId)
	ackRoute := make(Route, 256)
	client.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{ackRoute})
	receiveSettings := DefaultReceiveBufferSettings()
	configure(receiveSettings)
	sequence := newReceiveSequence(ctx, client, SourceId(sourceId), NewId(), TransferKey{}, receiveSettings)
	go sequence.Run()
	t.Cleanup(func() {
		sequence.Cancel()
		<-sequence.exit
		cancel()
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Errorf("close receive test client: %v", err)
		}
		drainFlightGateRoute(ackRoute)
	})
	return sequence
}

// Hands the sequence one acknowledged single-frame Pack.
func receiveDeadlineTimerTestPack(t *testing.T, sequence *ReceiveSequence, number uint64) {
	t.Helper()
	frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(16)}
	clear(frame.MessageBytes)
	pack := &ReceivePack{
		Source:     sequence.source,
		SequenceId: sequence.sequenceId,
		Pack: &protocol.Pack{
			MessageId:      NewId().Bytes(),
			SequenceId:     sequence.sequenceId.Bytes(),
			SequenceNumber: number,
			Head:           number == 0,
			Frames:         []*protocol.Frame{frame},
		},
		ReceiveCallback:  sequence.client.receiveCallback,
		Ctx:              sequence.ctx,
		Unwrapped:        true,
		EncryptionRole:   sequenceTlsRoleServer,
		MessageByteCount: ByteCount(len(frame.MessageBytes)),
	}
	if success, err := sequence.Pack(pack, -1); !success || err != nil {
		pack.messagePoolReturn()
		t.Fatalf("Pack %d refused: %t, %v", number, success, err)
	}
}

// A hole's gap deadline is absolute: receive time plus GapTimeout, even when
// the loop's timer was armed for a later idle deadline before the hole.
func TestReceiveSequenceGapExitsAtItsAbsoluteDeadline(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		const gapTimeout = time.Minute
		sequence := newDeadlineTimerTestReceiveSequence(t, func(settings *ReceiveBufferSettings) {
			settings.GapTimeout = gapTimeout
			settings.IdleTimeout = time.Hour
		})
		delivered := atomic.Int64{}
		sequence.client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			delivered.Add(int64(len(frames)))
		})
		// Pack 0 is delivered, and the loop waits on its idle deadline
		receiveDeadlineTimerTestPack(t, sequence, 0)
		synctest.Wait()
		time.Sleep(time.Minute)

		// Pack 2 opens a hole at 1 that never fills
		receiveDeadlineTimerTestPack(t, sequence, 2)
		synctest.Wait()
		holeAt := time.Now()
		select {
		case <-sequence.exit:
			if got := time.Since(holeAt); got != gapTimeout {
				t.Fatalf("the receive sequence exited %s after the hole opened, want GapTimeout %s", got, gapTimeout)
			}
		case <-time.After(time.Hour):
			t.Fatal("the receive sequence never exited on its hole")
		}
		if sequence.nextSequenceNumber != 1 || delivered.Load() != 1 {
			t.Fatalf("Pack 2 did not wait behind the hole: next=%d delivered=%d", sequence.nextSequenceNumber, delivered.Load())
		}
	})
}

// The receive loop's idle close comes exactly IdleTimeout after its last
// Pack, past an early fire of a timer armed for an earlier idle deadline.
func TestReceiveSequenceIdleCloseFollowsTheLastPack(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		const idleTimeout = 2 * time.Minute
		closes := make(chan time.Time, 4)
		var pair *deadlineTimerTestPair
		pair = newDeadlineTimerTestPair(t, func(a *ClientSettings, b *ClientSettings) {
			b.ReceiveBufferSettings.IdleTimeout = idleTimeout
			// no re-check of a non-empty queue lands between the idle deadlines
			b.ReceiveBufferSettings.GapTimeout = time.Hour
			b.ReceiveBufferSettings.afterIdleCloseForTest = func(id receiveSequenceId, done bool) {
				if id.Source.SourceId == pair.aId && done {
					closes <- time.Now()
				}
			}
		})
		pair.sendAcked(t, 0)
		time.Sleep(idleTimeout / 2)
		pair.sendAcked(t, 1)
		lastPack := time.Now()
		select {
		case closed := <-closes:
			if got := closed.Sub(lastPack); got != idleTimeout {
				t.Fatalf("the idle receive sequence closed %s after its last Pack, want %s", got, idleTimeout)
			}
		case <-time.After(10 * idleTimeout):
			t.Fatal("the idle receive sequence never closed")
		}
	})
}

// A forward sequence whose route always has room.
func newDeadlineTimerTestForwardSequence(
	t *testing.T,
	configure func(settings *ForwardBufferSettings),
) *ForwardSequence {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), closeWaitClientSettings())
	route := make(Route, 1024)
	client.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{route})
	settings := DefaultForwardBufferSettings()
	configure(settings)
	sequence := NewForwardSequence(ctx, client, DestinationId(NewId()), settings)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sequence.Run()
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		sequence.Close()
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Errorf("close forward test client: %v", err)
		}
		drainFlightGateRoute(route)
	})
	return sequence
}

func forwardDeadlineTimerTestPack(t *testing.T, sequence *ForwardSequence) {
	t.Helper()
	transferFrameBytes := MessagePoolGet(64)
	pack := &ForwardPack{Destination: sequence.destination, TransferFrameBytes: transferFrameBytes, Ctx: sequence.ctx}
	if success, err := sequence.Pack(pack, -1); !success || err != nil {
		MessagePoolReturn(transferFrameBytes)
		t.Fatalf("forward Pack refused: %t, %v", success, err)
	}
}

// The forward loop arms its idle timer once, not once per Pack.
func TestForwardSequenceIdleTimerArmsOnceNotPerPack(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		arms := atomic.Int64{}
		sequence := newDeadlineTimerTestForwardSequence(t, func(settings *ForwardBufferSettings) {
			settings.afterDeadlineTimerArmForTest = func(TransferPath) {
				arms.Add(1)
			}
		})
		const packCount = 64
		for range packCount {
			forwardDeadlineTimerTestPack(t, sequence)
			// the loop forwards it and waits again
			synctest.Wait()
		}
		t.Logf("%d forwarded Packs armed the forward loop's idle timer %d times", packCount, arms.Load())
		if limit := int64(2); limit < arms.Load() {
			t.Fatalf("the forward loop armed its idle timer %d times for %d Packs, want at most %d", arms.Load(), packCount, limit)
		}
	})
}

// The forward loop's idle close comes exactly IdleTimeout after its last
// Pack; the timer armed at the first Pack fires early and only re-arms.
func TestForwardSequenceIdleCloseFollowsTheLastPack(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		const idleTimeout = 2 * time.Minute
		closes := make(chan time.Time, 4)
		sequence := newDeadlineTimerTestForwardSequence(t, func(settings *ForwardBufferSettings) {
			settings.IdleTimeout = idleTimeout
			settings.afterIdleCloseForTest = func(_ TransferPath, done bool) {
				if done {
					closes <- time.Now()
				}
			}
		})
		forwardDeadlineTimerTestPack(t, sequence)
		synctest.Wait()
		time.Sleep(idleTimeout / 2)
		forwardDeadlineTimerTestPack(t, sequence)
		synctest.Wait()
		lastPack := time.Now()
		select {
		case closed := <-closes:
			if got := closed.Sub(lastPack); got != idleTimeout {
				t.Fatalf("the idle forward sequence closed %s after its last Pack, want %s", got, idleTimeout)
			}
		case <-time.After(10 * idleTimeout):
			t.Fatal("the idle forward sequence never closed")
		}
	})
}
