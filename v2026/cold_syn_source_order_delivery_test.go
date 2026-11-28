// A native cohort boundary must survive class selection and actual reliable
// provider delivery, including the later duplicate gate and terminal owners.
package connect

import (
	"bytes"
	"context"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Old DATA queued before a new SYN cannot become proof that different bytes
// in the new TCP cohort already reached its provider.
func TestColdSynProtocolQueuePreservesDeliveredCohort(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var groups []*parsedPacketGroup
		acks := make(chan error, 4)
		f := newGroupDispositionQueueFixture(t, nil, func(settings *ClientSettings) {
			observe := settings.SendBufferSettings.SendPackLifecycleObserver
			settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
				observe(event)
				if event.Phase == SendPackLifecyclePhaseTerminal && event.MessageType == protocol.MessageType_IpIpPacketToProvider {
					acks <- event.Err
				}
			}
			settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
				groups = append(groups, target.(*parsedPacketGroup))
			}
		})
		ctx, cancel := context.WithCancel(t.Context())
		settings := closeWaitClientSettings()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		provider := NewClient(ctx, f.peer, NewNoContractClientOob(), settings)
		provider.ContractManager().AddNoContractPeer(f.client.ClientId())
		provider.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{f.route})
		ackRoute := make(Route, 16)
		provider.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(f.client.ClientId())), []Route{ackRoute})
		f.client.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{ackRoute})
		t.Cleanup(func() {
			if err := f.closeClient(context.Background()); err != nil {
				t.Error(err)
			}
			cancel()
			if err := provider.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			for len(ackRoute) != 0 {
				MessagePoolReturn(<-ackRoute)
			}
		})
		var delivered []reraceExitPacket
		provider.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				if frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
					continue
				}
				packet, err := ipPacketToProviderBytes(frame)
				if err != nil {
					t.Error(err)
					continue
				}
				path, payload, err := ParseIpPathWithPayload(packet)
				if err != nil {
					t.Error(err)
					continue
				}
				delivered = append(delivered, reraceExitPacket{source: source.SourceId, path: path, payload: bytes.Clone(payload)})
			}
		})
		offer := func(templates ...[]byte) (int, []bool) {
			packets := make([][]byte, len(templates))
			for index, template := range templates {
				packets[index] = MessagePoolCopy(template)
				f.witnesses = append(f.witnesses, MessagePoolShareReadOnly(packets[index]))
			}
			accepted := make([]bool, len(packets))
			count := f.parent.SendNativePacketBatchWithResults(f.source, protocol.ProvideMode_Network, packets, 0, accepted)
			return count, accepted
		}
		oldPayload := bytes.Repeat([]byte{0x4d}, 500)
		old := groupRecoveryPacket(100, 500, 64, tcpFlagAck, oldPayload)
		syn := groupDispositionControlPacket(500, 500, 64, tcpFlagSyn)
		if count, accepted := offer(old, syn); count != 2 || !accepted[0] || !accepted[1] {
			t.Fatal("native old-DATA/new-SYN batch did not acquire both original owners")
		}
		f.requireParked()
		if len(groups) != 2 || groups[0].collapseSource == nil || groups[0].collapseSource != groups[1].collapseSource || len(delivered) != 0 {
			t.Fatal("both native groups did not remain raw on one undelivered source")
		}
		f.unpark()
		for range 2 {
			if err := <-acks; err != nil {
				t.Fatalf("actual provider ACK failed: %v", err)
			}
		}
		synctest.Wait()
		if len(delivered) != 2 || f.started.Load() != 2 || f.terminal.Load() != 2 {
			t.Fatalf("provider delivery/actual ACK counts differ: delivered=%d started=%d terminal=%d", len(delivered), f.started.Load(), f.terminal.Load())
		}
		for _, packet := range delivered {
			if packet.source != f.client.ClientId() {
				t.Fatal("provider delivery changed the selected local source identity")
			}
		}
		ordered := !delivered[0].path.Syn && delivered[0].path.SequenceNumber == 100 && bytes.Equal(delivered[0].payload, oldPayload) &&
			delivered[1].path.Syn && delivered[1].path.SequenceNumber == 500
		freshPayload := bytes.Repeat([]byte{0x5e}, 20)
		fresh := groupRecoveryPacket(501, 500, 64, tcpFlagAck, freshPayload)
		eligible := f.update.canUpdateSequenceForClient(groupRecoveryParsed(t, fresh), f.selected)
		count, accepted := offer(fresh)
		if count == 1 {
			if err := <-acks; err != nil {
				t.Fatalf("fresh original failed before its actual ACK: %v", err)
			}
		}
		synctest.Wait()
		t.Logf("NATIVE_COHORT_DELIVERY: old_order=%d syn_order=%d first_syn=%t first_sequence=%d last_syn=%t last_sequence=%d ordered=%t eligible=%t fresh_count=%d delivered_after=%d started=%d terminal=%d",
			groups[0].collapseOrder(), groups[1].collapseOrder(), delivered[0].path.Syn, delivered[0].path.SequenceNumber,
			delivered[1].path.Syn, delivered[1].path.SequenceNumber, ordered, eligible, count, len(delivered), f.started.Load(), f.terminal.Load())
		if !ordered {
			t.Error("PROTOCOL_SOURCE_ORDER_REVERSED: pure SYN crossed its actual native DATA predecessor at provider delivery")
		}
		if !eligible || count != 1 || !accepted[0] || len(delivered) != 3 || !bytes.Equal(delivered[2].payload, freshPayload) || f.terminal.Load() != 3 {
			t.Error("UNSENT_NEW_COHORT_RANGE_SUPPRESSED: pre-SYN DATA became duplicate proof for an unsent new-cohort original")
		}
	})
}
