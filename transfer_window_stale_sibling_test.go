// Valid raw residence on a sibling can precede an ordinary lane's stale firing.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// An exact sibling reply arrives before this lane's stale ordinary timer.
func TestWindowPacingOrdinaryRecoveryUsesConfirmedSiblingRawResidence(t *testing.T) {
	testWindowPacingSiblingRawRecovery(t, windowSiblingRecoveryCase{name: "reply", reply: true})
}

// Without a reply, retained evidence permits exactly one fixed raw-residence
// wait. The existing configured maximum still bounds the physical retry.
func TestWindowPacingSharedRawRecoveryHasFiniteConfiguredDeadline(t *testing.T) {
	for _, maximum := range []time.Duration{4 * time.Second, 600 * time.Millisecond} {
		testWindowPacingSiblingRawRecovery(t, windowSiblingRecoveryCase{name: "no reply", maximum: maximum})
	}
}

// An ordinary item's original lifetime still wins over a later physical
// residence deadline, even while sibling metadata remains valid.
func TestWindowPacingSharedRawRecoveryCannotExtendAckLifetime(t *testing.T) {
	testWindowPacingSiblingRawRecovery(t, windowSiblingRecoveryCase{name: "lifetime", lifetime: 500 * time.Millisecond})
}

// One bounded setup varies only the reply and existing configured limits.
type windowSiblingRecoveryCase struct {
	name     string
	maximum  time.Duration
	lifetime time.Duration
	reply    bool
}

// Two real H1 workers share one service. The first returns a confirmed 1.21 s
// sample before the second resumes its own stale 300 ms recovery timer.
func testWindowPacingSiblingRawRecovery(t *testing.T, row windowSiblingRecoveryCase) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		destination := NewId()
		firstWritten, secondWritten := make(chan struct{}), make(chan struct{})
		releaseFirst, releaseSecond := make(chan struct{}), make(chan struct{})
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
		settings.SendBufferSettings.ApplyWindowSizing()
		settings.SendBufferSettings.MinResendInterval = 300 * time.Millisecond
		settings.SendBufferSettings.RttMinResendInterval = 300 * time.Millisecond
		settings.SendBufferSettings.MaxResendInterval = 4 * time.Second
		if row.maximum > 0 {
			settings.SendBufferSettings.MaxResendInterval = row.maximum
		}
		settings.SendBufferSettings.AckTimeout = time.Minute
		if row.lifetime > 0 {
			settings.SendBufferSettings.AckTimeout = row.lifetime
		}
		settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
			if number != 0 || id.Destination != destination {
				return
			}
			written, release := firstWritten, releaseFirst
			if id.LogicalLane == 2 {
				written, release = secondWritten, releaseSecond
			}
			close(written)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		route := make(Route, 16)
		defer func() {
			cancel()
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Errorf("client cleanup: %v", err)
			}
			for len(route) > 0 {
				MessagePoolReturn(<-route)
			}
		}()
		client.ContractManager().AddNoContractPeer(destination)
		client.RouteManager().UpdateTransport(&h1SendClientTransportForGroupTest{sendClientTransport: NewSendClientTransport(DestinationId(destination))}, []Route{route})
		secondOutcome := make(chan error, 1)
		send := func(lane uint32) {
			frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1280)}
			completion := func(error) {}
			if lane == 2 {
				completion = func(err error) { secondOutcome <- err }
			}
			if !client.SendWithTimeout(frame, destination, completion, time.Second, TransferKey{LogicalLane: lane}) {
				MessagePoolReturn(frame.MessageBytes)
				t.Fatalf("lane %d Pack not admitted", lane)
			}
		}
		readPack := func() *protocol.Pack {
			select {
			case bytes := <-route:
				defer MessagePoolReturn(bytes)
				return decodeSendPackLifecycleWirePack(t, bytes)
			default:
				t.Fatal("expected physical write missing")
				return nil
			}
		}
		start := time.Now()
		send(1)
		<-firstWritten
		first := readPack()
		time.Sleep(900 * time.Millisecond)
		send(2)
		<-secondWritten
		second := readPack()
		sequence1 := client.sendBuffer.lookupSendSequence(sendSequenceId{Destination: destination, LogicalLane: 1}, nil)
		sequence2 := client.sendBuffer.lookupSendSequence(sendSequenceId{Destination: destination, LogicalLane: 2}, nil)
		if sequence1 == nil || sequence2 == nil || sequence1.windowPacer.service != sequence2.windowPacer.service {
			t.Fatal("workers do not share one service")
		}
		sequence2.rttWindow.closeSendTime(uint64(time.Now().Add(-time.Millisecond).UnixMilli()), time.Now())
		service := sequence1.windowPacer.service
		service.stateLock.Lock()
		probe := service.roundTripProbe
		service.stateLock.Unlock()
		if !probe.sentAt.IsZero() {
			t.Fatal("ordinary second write unexpectedly owns a drained probe")
		}
		time.Sleep(time.Until(start.Add(1210 * time.Millisecond)))
		delay, compression := uint32(10000), uint32(10000)
		ack := func(sequence *SendSequence, pack *protocol.Pack) {
			if ok, err := sequence.Ack(&protocol.Ack{SequenceId: pack.SequenceId, MessageId: pack.MessageId, Tag: pack.Tag, ReceiverAckDelayMicros: &delay, AckCompressTimeoutMicros: &compression}, 0); !ok || err != nil {
				t.Fatalf("ACK admission: %t %v", ok, err)
			}
			synctest.Wait()
		}
		ack(sequence1, first)
		shared := service.roundTripEvidence(time.Now())
		local := sequence2.rttWindow.Estimate()
		if shared.latestRaw != 1210*time.Millisecond || local.Mean != time.Millisecond || sequence2.rttWindow.ScaledRtt() != 300*time.Millisecond {
			t.Fatalf("fresh/stale fixture mismatch: shared=%+v local=%+v", shared, local)
		}
		close(releaseFirst)
		close(releaseSecond)
		synctest.Wait()
		retries := 0
		for len(route) > 0 {
			pack := readPack()
			if string(pack.MessageId) != string(second.MessageId) {
				t.Fatal("unexpected message physically retried")
			}
			retries++
		}
		t.Logf("case=%s at=%s confirmed-shared-raw=%s local-raw=%s local-scaled=%s ordinary-physical-retries=%d probe=false", row.name, time.Since(start), shared.latestRaw, local.Mean, sequence2.rttWindow.ScaledRtt(), retries)
		if retries != 0 {
			t.Error("ordinary reliable recovery ignored the exact raw residence already measured on its sibling before the stale firing")
		}
		if row.reply {
			time.Sleep(time.Until(start.Add(2110 * time.Millisecond)))
			ack(sequence2, second)
			return
		}
		if retries != 0 {
			return
		}
		physicalAt := start.Add(900 * time.Millisecond)
		interval := min(2*shared.latestRaw, settings.SendBufferSettings.MaxResendInterval)
		deadline := physicalAt.Add(interval)
		expires := row.lifetime > 0 && row.lifetime < interval
		if expires {
			deadline = physicalAt.Add(row.lifetime)
		}
		time.Sleep(time.Until(deadline.Add(-time.Nanosecond)))
		synctest.Wait()
		if len(route) != 0 {
			t.Fatalf("case=%s: retry arrived before the fixed physical deadline", row.name)
		}
		select {
		case err := <-secondOutcome:
			t.Fatalf("case=%s: item expired before its original lifetime: %v", row.name, err)
		default:
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if expires {
			select {
			case err := <-secondOutcome:
				if err == nil {
					t.Error("lifetime returned success without a reply")
				}
			default:
				t.Error("raw residence extended the original ACK lifetime")
			}
			if len(route) != 0 {
				t.Error("expired item was physically retried")
			}
		} else {
			if len(route) != 1 {
				t.Fatalf("case=%s maximum=%s: missing reply did not recover at fixed deadline %s: copies=%d", row.name, row.maximum, deadline.Sub(start), len(route))
			}
			retry := readPack()
			if string(retry.MessageId) != string(second.MessageId) {
				t.Error("fixed deadline recovered another message")
			}
		}
		t.Logf("case=%s maximum=%s lifetime=%s physical=%s fixed-deadline=%s expired=%t", row.name, row.maximum, row.lifetime, physicalAt.Sub(start), deadline.Sub(start), expires)
	})
}
