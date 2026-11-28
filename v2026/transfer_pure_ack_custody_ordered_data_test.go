// Independent TCP feedback never grants ordinary DATA permission to reorder.
package connect

import (
	"bytes"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Both ordinary owners are admitted while the real send worker is parked.
// Passing full feedback custody must publish the older DATA before its sibling.
func TestPureTcpAckCustodyPreservesEarlierOrdinaryData(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		limit := DefaultClientSettings().SendBufferSettings.SequenceBufferSize
		var parkNext atomic.Bool
		parked, release := make(chan struct{}), make(chan struct{})
		var f *windowRoundFixture
		f = newWindowRoundFixture(t, func(s *SendBufferSettings) {
			s.SequenceBufferSize = limit
			s.afterApplyAckSnapshotForTest = func(sendSequenceId) {
				if parkNext.CompareAndSwap(true, false) {
					close(parked)
					select {
					case <-release:
					case <-f.ctx.Done():
					}
				}
			}
		}, nil, TransportTypeH1)
		released := false
		t.Cleanup(func() {
			if !released {
				close(release)
			}
		})
		path := &IpPath{Version: 4, Protocol: IpProtocolTcp,
			SourceIp: net.IPv4(192, 0, 2, 12).To4(), SourcePort: 41002,
			DestinationIp: net.IPv4(198, 51, 100, 12).To4(), DestinationPort: 443}
		const next = uint32(1014)
		offer := func(payload []byte, sequence uint32) {
			t.Helper()
			packet := MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagAck, sequence, payload))
			actual, actualPayload, err := ParseIpPathWithPayload(packet)
			if err != nil || actual == nil || !bytes.Equal(actualPayload, payload) {
				MessagePoolReturn(packet)
				t.Fatal("ordinary-order control packet failed parsing")
			}
			frame, err := ipPacketFromProviderFrame(packet, 2)
			if err != nil {
				MessagePoolReturn(packet)
				t.Fatal(err)
			}
			options := multiClientProtocolTransferOpts([]any{scheduleIpFlow(actual)}, []*protocol.Frame{frame})
			accepted, err := f.sender.SendWithTimeoutDetailed(frame, f.receiver.ClientId(), nil, time.Second, options...)
			if !accepted || err != nil {
				MessagePoolReturn(packet)
				t.Fatalf("ordinary-order handoff: admitted=%t error=%v", accepted, err)
			}
		}
		offer([]byte("synthetic hdr"), next-13)
		f.forward(f.take(f.senderOut), f.receiverIn)
		f.acknowledge()
		sequence := f.sequence()
		held := make([]*windowRoundFrame, 0, 2*limit)
		for index := range limit {
			offer(nil, next)
			wire := f.take(f.senderOut)
			f.forward(wire, f.receiverIn)
			held = append(held, takePureAckCustodyReceipts(t, f, wire)...)
			if sequence.immediateSendCount != index+1 || f.deliveredCount != index+2 {
				t.Fatalf("ordinary-order custody premise: custody=%d want=%d delivered=%d want=%d",
					sequence.immediateSendCount, index+1, f.deliveredCount, index+2)
			}
		}
		offer(nil, next)
		synctest.Wait()
		sequence.protocolAdmission.mutex.Lock()
		protocolPending := sequence.protocolAdmission.count
		sequence.protocolAdmission.mutex.Unlock()
		if protocolPending != 1 || sequence.immediateSendCount != limit || sequence.flowIsolation.Load() {
			t.Fatal("ordinary-order control did not retain its blocked FIFO feedback owner")
		}
		started := time.Now()
		parkNext.Store(true)
		offer([]byte{2}, next)
		synctest.Wait()
		select {
		case <-parked:
		default:
			t.Fatal("real send worker did not park before selecting the earlier ordinary DATA")
		}
		offer([]byte{1}, next+1)
		synctest.Wait()
		sequence.packAdmission.mutex.Lock()
		ordinaryPending := sequence.packAdmission.count
		sequence.packAdmission.mutex.Unlock()
		if ordinaryPending != 2 || sequence.immediateSendCount != limit || !time.Now().Equal(started) {
			t.Fatalf("both ordinary owners were not admitted behind the parked worker: owners=%d custody=%d want=%d",
				ordinaryPending, sequence.immediateSendCount, limit)
		}
		close(release)
		released = true
		seen := 0
		for range 2 {
			wire := f.take(f.senderOut)
			if wire.pack == nil || wire.pack.Nack || wire.pack.DeliverImmediately {
				t.Fatal("ordinary DATA inherited the held feedback's immediate policy")
			}
			for _, frame := range wire.pack.Frames {
				message, err := FromFrame(frame)
				if err != nil {
					t.Fatal(err)
				}
				ip, ok := message.(*protocol.IpPacketFromProvider)
				if !ok {
					t.Fatal("ordinary-order control published another frame type")
				}
				actual, payload, err := ParseIpPathWithPayload(ip.GetIpPacket().GetPacketBytes())
				if err != nil || actual == nil || seen >= 2 || actual.SequenceNumber != next+uint32(seen) ||
					!actual.SourceIp.Equal(path.SourceIp) || actual.SourcePort != path.SourcePort ||
					!actual.DestinationIp.Equal(path.DestinationIp) || actual.DestinationPort != path.DestinationPort ||
					!bytes.Equal(payload, []byte{byte(2 - seen)}) {
					t.Fatal("later ordinary DATA crossed the exact earlier ordinary owner")
				}
				seen++
			}
			f.forward(wire, f.receiverIn)
			if seen == 2 {
				break
			}
		}
		if seen != 2 || f.deliveredCount != limit+3 || sequence.immediateSendCount != limit || !time.Now().Equal(started) {
			t.Fatal("ordered DATA delivery spent an immediate receipt, omitted a sibling or advanced virtual time")
		}
		f.forward(held[0], f.senderIn)
		feedback := f.take(f.senderOut)
		if feedback.pack == nil || !feedback.pack.DeliverImmediately || feedback.pack.Nack {
			t.Fatal("ordinary DATA order control did not resume queued feedback after its receipt")
		}
		f.forward(feedback, f.receiverIn)
		f.acknowledge()
		if sequence.immediateSendCount != 0 || sequence.resendQueue.Len() != 0 || !time.Now().Equal(started) {
			t.Fatal("ordinary DATA order control did not retire its exact real owners")
		}
	})
}
