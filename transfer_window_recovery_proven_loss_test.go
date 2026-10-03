// Exact shared raw residence cannot override later physical delivery proof.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Two actual H1 writes share one sequence and carrier. A real selective ACK
// for the second proves the first was lost, below the three-SACK threshold.
// The retained shared raw sample contains an earlier receiver wait, while the
// lane's ordinary timer remains 300 ms. The proof must retain its one-interval
// recovery bound even when the shared sample would conservatively extend an
// unproved timeout to 2.4 s.
func TestWindowPacingSharedRawResidencePreservesProvenEndpointLoss(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		destination := NewId()
		startWorker := make(chan struct{})
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
		settings.SendBufferSettings.AckTimeout = time.Minute
		settings.SendBufferSettings.ReliableLaneProvenRecovery = true
		settings.SendBufferSettings.beforeRunSendSequenceForTest = func(sendSequenceId) {
			select {
			case <-startWorker:
			case <-ctx.Done():
			}
		}
		settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
			if id.Destination != destination || number > 1 {
				return
			}
			written, release := firstWritten, releaseFirst
			if number == 1 {
				written, release = secondWritten, releaseSecond
			}
			close(written)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		route := make(Route, 8)
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
		sequence := client.sendBuffer.createSendSequence(sendSequenceId{Destination: destination}, &SendPack{Destination: destination})
		start := time.Now()
		// This earlier exact tuple represents a slow receiver callback, not
		// changed propagation or proof that this new message was delivered.
		sequence.windowPacer.service.observeReceiverRoundTrip(1, 1200*time.Millisecond, 300*time.Microsecond, 0, start)
		sequence.rttWindow.closeSendTime(uint64(start.Add(-time.Millisecond).UnixMilli()), start)
		send := func() {
			frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1280)}
			if !client.SendWithTimeout(frame, destination, func(error) {}, time.Second) {
				MessagePoolReturn(frame.MessageBytes)
				t.Fatal("Pack not admitted")
			}
		}
		readPack := func() *protocol.Pack {
			select {
			case bytes := <-route:
				defer MessagePoolReturn(bytes)
				return decodeSendPackLifecycleWirePack(t, bytes)
			default:
				t.Fatal("physical write missing")
				return nil
			}
		}
		send()
		close(startWorker)
		<-firstWritten
		hole := readPack()
		firstDeadline := sequence.resendQueue.PeekFirst().resendTime
		send()
		close(releaseFirst)
		<-secondWritten
		proof := readPack()
		if proof.SequenceNumber != hole.SequenceNumber+1 {
			t.Fatal("physical proof does not follow the hole")
		}
		time.Sleep(time.Until(start.Add(100 * time.Millisecond)))
		proofAt := time.Now()
		if ok, err := sequence.Ack(&protocol.Ack{SequenceId: proof.SequenceId, MessageId: proof.MessageId, Selective: true}, 0); !ok || err != nil {
			t.Fatalf("physical proof ACK admission: %t %v", ok, err)
		}
		close(releaseSecond)
		synctest.Wait()
		proofId, _ := IdFromBytes(proof.MessageId)
		proofApplied := func() bool {
			sequence.resendQueue.stateLock.Lock()
			defer sequence.resendQueue.stateLock.Unlock()
			item := sequence.resendQueue.messageIdItems[proofId]
			return item != nil && item.selectiveAcked
		}()
		if !proofApplied {
			t.Fatal("sender did not apply the actual same-carrier selective ACK")
		}
		localInterval := sequence.rttWindow.ScaledRtt()
		if localInterval != 300*time.Millisecond {
			t.Fatalf("ordinary lane interval changed: %s", localInterval)
		}
		t.Logf("proof=%s local-rto=%s shared-raw=%s scheduled=%s", proofAt.Sub(start), localInterval, sequence.windowPacer.service.roundTripEvidence(proofAt).latestRaw, firstDeadline.Sub(start))
		time.Sleep(time.Until(proofAt.Add(localInterval)))
		synctest.Wait()
		found := false
		for len(route) > 0 {
			pack := readPack()
			if string(pack.MessageId) == string(hole.MessageId) {
				found = true
			}
		}
		if !found {
			t.Error("confirmed same-carrier endpoint loss borrowed the longer shared raw residence")
		} else if client.laneProvenTimeoutWriteCount.Load() != 1 {
			t.Error("recovery did not use the actual same-carrier endpoint-loss verdict")
		}
	})
}
