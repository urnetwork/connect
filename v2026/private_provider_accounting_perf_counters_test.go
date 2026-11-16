//go:build urnetwork_synthetic_fixture

// These finite controls exercise observation availability and lock independence.
// Counter values are isolated test inputs, never packet or contract authority.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
)

// A missing endpoint serializes nil groups, while a real zero counter remains
// available. Client/provider values must not be interchanged or summed.
func TestProviderAccountingPerfCountersDistinguishUnavailableFromZero(t *testing.T) {
	missing := PrivateCopyProviderAccountingPerfCounters(nil)
	if missing.Available || missing.UnavailableReason == "" || missing.Receive != nil || missing.Recovery != nil || missing.PackHandoff != nil || missing.AckHandoff != nil || missing.Admission != nil || missing.AckRoute != nil {
		t.Fatal("unavailable endpoint became observed zero counters")
	}
	client := &Client{clientId: NewId(), ctx: context.Background()}
	provider := &Client{clientId: NewId(), ctx: context.Background()}
	zero := PrivateCopyProviderAccountingPerfCounters(client)
	if !zero.Available || zero.Receive == nil || zero.Recovery == nil || zero.PackHandoff == nil || zero.AckHandoff == nil || zero.Admission == nil || zero.AckRoute == nil || zero.PackHandoff.DropCount != 0 || zero.AckHandoff.DropCount != 0 || zero.Admission.DecodePressureDropCount != 0 {
		t.Fatal("actual endpoint zero counters were unavailable")
	}
	client.receivePackHandoffDropCount.Store(17)
	client.receivePackHandoffDropByteCount.Store(1700)
	client.receiveAckHandoffDropCount.Store(19)
	client.receiveAckHandoffQueueFullCount.Store(23)
	client.receiveDecodePressureDropCount.Store(29)
	client.receiveQueueDropCount.Store(31)
	client.receiveQueueCommitShortfallCount.Store(37)
	client.receiveAckRouteWriteErrorCount.Store(41)
	provider.receivePackHandoffDropCount.Store(53)
	provider.receivePackHandoffDropByteCount.Store(5300)
	provider.receiveAckHandoffDropCount.Store(59)
	provider.receiveAckHandoffQueueFullCount.Store(61)
	provider.receiveDecodePressureDropCount.Store(67)
	provider.receiveQueueDropCount.Store(71)
	provider.receiveQueueCommitShortfallCount.Store(73)
	provider.receiveAckRouteWriteErrorCount.Store(79)
	consumerCut, providerCut := PrivateCopyProviderAccountingPerfCounters(client), PrivateCopyProviderAccountingPerfCounters(provider)
	if consumerCut.ClientId != client.ClientId() || providerCut.ClientId != provider.ClientId() || consumerCut.ClientId == providerCut.ClientId ||
		consumerCut.PackHandoff.DropCount != 17 || consumerCut.PackHandoff.DropByteCount != 1700 || consumerCut.AckHandoff.DropCount != 19 || consumerCut.AckHandoff.QueueFullCount != 23 || consumerCut.Admission.DecodePressureDropCount != 29 || consumerCut.Admission.QueueDropCount != 31 || consumerCut.Admission.CommitShortfallCount != 37 || consumerCut.AckRoute.ErrorCount != 41 ||
		providerCut.PackHandoff.DropCount != 53 || providerCut.PackHandoff.DropByteCount != 5300 || providerCut.AckHandoff.DropCount != 59 || providerCut.AckHandoff.QueueFullCount != 61 || providerCut.Admission.DecodePressureDropCount != 67 || providerCut.Admission.QueueDropCount != 71 || providerCut.Admission.CommitShortfallCount != 73 || providerCut.AckRoute.ErrorCount != 79 {
		t.Fatal("client/provider ingress boundaries lost their separate exact counters")
	}
}

// The ordinary pump/queue locks stay held at real barriers while the capture
// finishes; no timer or sleep lets a blocking implementation pass.
func TestProviderAccountingPerfCounterCaptureDoesNotWaitForPumpLocks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &Client{clientId: NewId(), ctx: context.Background(), sendBuffer: &SendBuffer{}, receiveBuffer: &ReceiveBuffer{}}
		queue := newResendQueue(nil, 0)
		sequence := &SendSequence{resendQueue: queue}
		client.sendBuffer.activeSendSequences = map[*SendSequence]bool{sequence: true}
		client.sendBuffer.mutex.Lock()
		client.receiveBuffer.mutex.Lock()
		queue.stateLock.Lock()
		captured := make(chan PrivateProviderAccountingPerfCounterSnapshot, 1)
		go func() { captured <- PrivateCopyProviderAccountingPerfCounters(client) }()
		synctest.Wait()
		var result PrivateProviderAccountingPerfCounterSnapshot
		completed := false
		select {
		case result = <-captured:
			completed = true
		default:
			t.Error("counter capture waited for packet-needed locks")
		}
		queue.stateLock.Unlock()
		client.receiveBuffer.mutex.Unlock()
		client.sendBuffer.mutex.Unlock()
		synctest.Wait()
		if !completed {
			result = <-captured
		}
		if !result.Available || result.ClientId != client.ClientId() {
			t.Fatal("counter capture lost actual endpoint availability")
		}
	})
}

// Removing SDK/window membership must not erase the known endpoint's final
// atomics. Cancellation remains distinct from proof that an owner was joined.
func TestProviderAccountingPerfRetiredClientCountersRemainAvailable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &Client{clientId: NewId(), ctx: ctx}
	client.receivePackHandoffDropCount.Store(17)
	client.receiveAckHandoffQueueFullCount.Store(23)
	prior := []PrivateProviderAccountingPerfWindow{{ClientId: client.ClientId(), ProviderId: NewId(), Active: true, client: client}}
	cancel()
	var closed *RemoteUserNatMultiClient
	rows, complete := closed.PrivateProviderAccountingPerfWindowStats(prior)
	if complete || len(rows) != 1 || rows[0].Active || !rows[0].Counters.Available || !rows[0].Counters.ContextCanceled || rows[0].ClientId != client.ClientId() || rows[0].Receive == nil || rows[0].Receive.PackHandoffDropCount != 17 || rows[0].Counters.AckHandoff.QueueFullCount != 23 || rows[0].SenderUnavailableReason != "send_buffer_unavailable" {
		t.Fatal("retired endpoint became unavailable/zero or was mislabeled as active membership")
	}
}
