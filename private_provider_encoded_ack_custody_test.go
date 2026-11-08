// Encoded Transfer admission precedes the inner TCP acknowledgement which
// retires a provider's actual retained origin chunk. Component controls use
// authorized plaintext peers and owned channel routes; no hosted-provider,
// cryptographic, physical-H1, phone-policy or throughput parity is claimed.
package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Both arms hold ten actual one-ACK originals and their missing-head promise.
// Only the head's frame count changes. TCP's worker is held before its run;
// the established pure-ACK entry still applies directly through the real NAT.
func testPrivateProviderEncodedAckCustody(t *testing.T, headFrameCount int) {
	testPrivateProviderEncodedAckCustodyWithCancel(t, headFrameCount, false)
}

// Cancellation occurs after the real provider owner reaches the NAT barrier.
func testPrivateProviderEncodedAckCustodyWithCancel(t *testing.T, headFrameCount int, cancelAfterAdmission bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		root := NewTransferMemoryBudget(mib(4))
		rx := NewTransferMemoryBudgetWithParent(kib(64), root)
		recovery := NewTransferMemoryBudgetWithParent(kib(64), root)
		natBudget := NewTransferMemoryBudgetWithParent(mib(3), root)
		f := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
			settings.ProtocolVersion = 2
			settings.ResendQueueMaxByteCount = kib(64)
		}, func(settings *ReceiveBufferSettings) {
			settings.ReceiveQueueBudget = rx
			settings.ReceiveHeadRecoveryBudget = recovery
			settings.ReceiveQueueRetainedByteAccounting = true
			settings.ReceiveQueueMinByteCount = 0
			settings.ReceiveQueueMaxByteCount = kib(64)
			settings.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
			settings.AdvertiseReceiveWindow = false
			settings.PackQueueBudget = nil
			settings.PackQueueRetainedByteAccounting = false
		}, TransportTypeH1)
		path := &IpPath{Version: 4, Protocol: IpProtocolTcp,
			SourceIp: net.IPv4(192, 0, 2, 2).To4(), SourcePort: 47001,
			DestinationIp: net.IPv4(203, 0, 113, 10).To4(), DestinationPort: 443}
		warm := f.write(1280)
		f.forward(warm, f.receiverIn)
		f.acknowledge()
		sequence := func() *ReceiveSequence {
			f.receiver.receiveBuffer.mutex.Lock()
			defer f.receiver.receiveBuffer.mutex.Unlock()
			for _, candidate := range f.receiver.receiveBuffer.receiveSequences {
				return candidate
			}
			t.Fatal("warm prefix did not create an actual receive sequence")
			return nil
		}()
		if root.UsedByteCount() != 0 || f.ackedCount != 1 || f.deliveredCount != 1 {
			t.Fatal("warm prefix did not retire before pressure")
		}
		natSettings := DefaultLocalUserNatSettingsWithBufferSize(1)
		natSettings.Log, natSettings.MemoryBudget = NewNoopLogger(), natBudget
		natSettings.TcpBufferSettings.WriteTimeout = 0
		captured := make(chan *TcpSequence, 1)
		natSettings.TcpBufferSettings.beforeSequenceRunWithStateForTest = func(tcp *TcpSequence) {
			captured <- tcp
			<-tcp.ctx.Done()
		}
		natSettings.TcpBufferSettings.DialContextSettings = &DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, context.Canceled
		}}
		nat := NewLocalUserNat(f.ctx, "encoded pure ACK custody", natSettings)
		providerSettings := DefaultRemoteUserNatProviderSettings()
		providerSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
		provider := NewRemoteUserNatProvider(f.receiver, nat, providerSettings)
		closed := false
		closeOwners := func() {
			if closed {
				return
			}
			closed = true
			f.cancel()
			provider.Close()
			if err := nat.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			for _, client := range []*Client{f.sender, f.receiver} {
				if err := client.CloseAndWait(context.Background()); err != nil {
					t.Error(err)
				}
			}
			for _, budget := range []*TransferMemoryBudget{root, rx, recovery, natBudget} {
				stats := budget.Stats()
				if stats.UsedByteCount != 0 || stats.QuotaByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
					t.Errorf("encoded ACK owner survived join: %+v", stats)
				}
			}
		}
		defer closeOwners()
		processed := make(chan struct{}, 32)
		nat.afterSendPacketForTest = func() { processed <- struct{}{} }
		syn := MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagSyn, 100, nil))
		if !nat.SendPacket(SourceId(f.sender.ClientId()), protocol.ProvideMode_Public, syn, 0) {
			MessagePoolReturn(syn)
			t.Fatal("initial SYN did not create the held real TCP queue")
		}
		tcp := <-captured
		<-processed
		tcp.receiveAckCondition()
		tcp.mutex.Lock()
		tcp.established, tcp.receiveWindowEndSet = true, true
		tcp.receiveSeqAck, tcp.receiveSeq = 500, 1780
		tcp.receiveWindowSize, tcp.receiveWindowEnd = 4096, 4596
		tcp.mutex.Unlock()
		if !tcp.retainReturnChunk(bytes.Repeat([]byte{0x63}, 1280), 500, false) {
			t.Fatal("actual return chunk could not acquire NAT/root ownership")
		}
		retainedBefore := tcp.returnByteCount
		if retainedBefore <= 0 || tcp.returnHead != 0 || len(tcp.returnChunks) != 1 || len(tcp.sendItems) != 1 {
			t.Fatal("return chunk and full TCP queue were not live before encoded ingress")
		}
		gate := make(chan struct{})
		entered := make(chan *providerReliablePacket, 32)
		nat.beforeSendPacketForTest = func(packet *SendPacket) {
			if packet.reliable != nil {
				entered <- packet.reliable
				select {
				case <-gate:
				case <-f.ctx.Done():
				}
			}
		}
		frames := func(count int) []*protocol.Frame {
			result := make([]*protocol.Frame, count)
			for index := range result {
				packet := ipOosTcpPacketSequence(path, tcpFlagAck, 101, nil)
				segment := packet[Ipv4HeaderSizeWithoutExtensions:]
				binary.BigEndian.PutUint32(segment[8:12], 1780)
				binary.BigEndian.PutUint16(segment[16:18], 0)
				binary.BigEndian.PutUint16(segment[16:18], ipPathTransportChecksum(path, ipProtocolNumberTcp, segment))
				var err error
				result[index], err = ipPacketToProviderFrame(MessagePoolCopy(packet), DefaultProtocolVersion)
				if err != nil || !result[index].Raw || !reliableFrameControl(result[index]) {
					t.Fatal("fixture did not construct a genuine pure ACK frame")
				}
			}
			return result
		}
		originals := make(map[uint64]*windowRoundFrame, 11)
		ackCounts := make(map[uint64]int, 11)
		for number := uint64(1); number <= 11; number++ {
			count := 1
			if number == 1 {
				count = headFrameCount
			}
			batch := frames(count)
			if !f.sender.SendMultiWithTimeout(batch, f.receiver.ClientId(), func(err error) {
				if err == nil {
					f.ackedCount++
					ackCounts[number]++
				}
			}, time.Second) {
				for _, frame := range batch {
					MessagePoolReturn(frame.MessageBytes)
				}
				t.Fatal("actual sender refused encoded pure ACK original")
			}
			original := f.takePack(number)
			if original.pack.Nack || original.pack.SequenceNumber != number || len(original.pack.Frames) != count ||
				!bytes.Equal(original.pack.SequenceId, warm.pack.SequenceId) {
				t.Fatal("pure ACK batch lost its reliable source sequence or frame count")
			}
			originals[number] = original
		}
		cost := func(original *windowRoundFrame) (ByteCount, ByteCount) {
			scan, status := scanReceiveMemoryPack(original.bytes, TransferPath{SourceId: f.sender.ClientId(), DestinationId: f.receiver.ClientId()})
			probe := &receiveItem{transferItem: transferItem{queueByteCount: scan.retainedByteCount}, ack: true, frames: original.pack.Frames}
			if status != receiveMemoryPackScanReady || !scan.loanSafe || scan.nack || !sequence.prepareReceiveDeliveryCredit(probe) || !probe.deliveryPrepared {
				t.Fatal("encoded ACK did not have a paid decode and downstream-delivery shape")
			}
			return scan.retainedByteCount, probe.QueueByteCount()
		}
		headDecode, headFull := cost(originals[1])
		tailDecode, tailFull := cost(originals[2])
		if headDecode <= 0 || headFull <= headDecode || tailFull <= tailDecode ||
			(headFrameCount == 1 && headFull != tailFull) ||
			(headFrameCount == 2 && !(headDecode <= tailFull && tailFull < headFull)) ||
			(headFrameCount > 2 && !(tailFull < headDecode && headFull <= 11*tailFull)) {
			t.Fatalf("ACK C/D geometry does not isolate the intended admission stage: head=%d/%d tail=%d/%d", headDecode, headFull, tailDecode, tailFull)
		}
		repairMetadata, rxTarget := privateProviderRepairFixtureGeometry(t, sequence, tailFull, 10)
		rx.SetTotalByteCount(rxTarget)
		recovery.SetTotalByteCount(rx.TotalByteCount())
		owners := make(map[uint64]*receiveItem, 10)
		for number := uint64(2); number <= 11; number++ {
			f.forward(originals[number], f.receiverIn)
			item := sequence.receiveQueue.GetBySequenceNumber(number)
			if item == nil || !item.committed || !item.ack || item.decodedOwner == nil ||
				item.QueueByteCount() != tailFull || !item.deliveryPrepared || item.deliveryPrepaid <= 0 {
				t.Fatal("later pure ACK was not a real committed fully prepaid owner")
			}
			owners[number] = item
			for len(f.receiverOut) != 0 {
				ack := f.take(f.receiverOut)
				if ack.ack == nil || !ack.ack.Selective || !bytes.Equal(ack.ack.MessageId, originals[number].pack.MessageId) {
					t.Fatal("later pure ACK did not acquire its actual selective promise")
				}
				f.forward(ack, f.senderIn)
			}
		}
		privateProviderAssertRepairFixtureCustody(t, sequence, repairMetadata, 10)
		if rx.UsedByteCount() != rx.TotalByteCount() || sequence.holdPromise.reservedByteCount != tailFull ||
			sequence.nextSequenceNumber != 1 || sequence.receiveQueue.Len() != 10 || len(entered) != 0 ||
			f.ackedCount != 1 || tcp.returnByteCount != retainedBefore || root.Available() < headFull {
			t.Fatal("committed ACK tails did not isolate RX from the shared root and actual retained return chunk")
		}
		t.Logf("[provider-encoded-ack-before] head_frames=%d head_c=%d head_d=%d tail_c=%d tail_d=%d prefix=%d rx_used=%d rx_cap=%d root_spare=%d return_chunk=%d committed=10", headFrameCount, headDecode, headFull, tailDecode, tailFull, tailFull, rx.UsedByteCount(), rx.TotalByteCount(), root.Available(), retainedBefore)
		before := f.receiver.ReceiveStats()
		f.forward(originals[1], f.receiverIn)
		if len(entered) == 0 || tcp.returnByteCount != retainedBefore || f.ackedCount != 1 {
			t.Fatal("provider recovery did not admit the original encoded head while retaining its inner chunk; manual capacity rescue is forbidden")
		}
		if rx.TotalByteCount() != rxTarget || recovery.TotalByteCount() != rx.TotalByteCount() ||
			(headFrameCount == 1 && recovery.UsedByteCount() != 0) ||
			(headFrameCount > 1 && recovery.UsedByteCount() <= 0) ||
			root.UsedByteCount() != rx.UsedByteCount()+recovery.UsedByteCount()+natBudget.UsedByteCount() {
			t.Fatal("provider recovery changed a ceiling or omitted/double-charged the child owner")
		}
		for _, item := range owners {
			if !item.committed || item.memoryBudget != rx || item.decodedOwner == nil || !item.decodedOwner.receivePack.receiveMemoryOwned {
				t.Fatal("provider head recovery evicted, replaced or released an accepted ACK tail before actual delivery")
			}
		}
		if cancelAfterAdmission {
			closeOwners()
			if f.ackedCount != 1 || root.UsedByteCount() != 0 || recovery.UsedByteCount() != 0 || tcp.returnByteCount != 0 {
				t.Fatal("canceled provider head falsely acknowledged delivery or retained its real return chunk/child")
			}
			t.Logf("[provider-encoded-ack-canceled] head_frames=%d cumulative=1 root=0 child=0 nat=0 return_chunk=0 joined=true", headFrameCount)
			return
		}
		t.Logf("[provider-encoded-ack-natural-recovery] head_frames=%d head_c=%d head_d=%d child_used=%d retries=0 manual_rescue=false", headFrameCount, headDecode, headFull, recovery.UsedByteCount())
		close(gate)
		synctest.Wait()
		// Provider diagnostics is a separate reverse reliable Pack. Accept only
		// that known telemetry while applying every real Transfer ACK.
		for len(f.receiverOut) != 0 {
			feedback := f.take(f.receiverOut)
			if feedback.ack == nil {
				if feedback.pack == nil || len(feedback.pack.Frames) != 1 ||
					feedback.pack.Frames[0].MessageType != protocol.MessageType_IpIpProviderDiagnostics {
					t.Fatal("unexpected reverse provider feedback")
				}
				continue
			}
			f.forward(feedback, f.senderIn)
		}
		if f.ackedCount != 12 || f.deliveredCount != 11+headFrameCount || sequence.nextSequenceNumber != 12 ||
			sequence.receiveQueue.Len() != 0 || rx.UsedByteCount() != 0 || tcp.returnByteCount != 0 ||
			tcp.returnHead != len(tcp.returnChunks) || len(tcp.sendItems) != 1 ||
			f.receiver.ReceiveStats().ReceiveQueueEvictionCount != before.ReceiveQueueEvictionCount ||
			f.receiver.ReceiveStats().ReceiveQueueTentativeEvictionCount != before.ReceiveQueueTentativeEvictionCount {
			t.Fatalf("pure ACKs did not retire the actual return chunk and their Transfer owners: ack=%d delivered=%d next=%d rx=%d chunk=%d queued=%d", f.ackedCount, f.deliveredCount, sequence.nextSequenceNumber, rx.UsedByteCount(), tcp.returnByteCount, len(tcp.sendItems))
		}
		for number := uint64(1); number <= 11; number++ {
			if ackCounts[number] != 1 {
				t.Fatal("encoded ACK recovery skipped or duplicated an original cumulative callback")
			}
		}
		closeOwners()
		t.Logf("[provider-encoded-ack-complete] head_frames=%d returned_chunk=%d callbacks=12 committed_evictions=0 root=0 rx=0 nat=0 joined=true", headFrameCount, retainedBefore)
	})
}

// An equal-size pure ACK head uses the promised bytes without new permission.
func TestPrivateProviderEncodedSingleAckRetiresReturnChunk(t *testing.T) {
	testPrivateProviderEncodedAckCustody(t, 1)
}

// The historical arm is expected to fail only after full rescue/ownership proof.
func TestPrivateProviderEncodedDoubleAckGrowthProgress(t *testing.T) {
	testPrivateProviderEncodedAckCustody(t, 2)
}

// A larger raw-TCP head also recovers when C exceeds the committed promise.
func TestPrivateProviderEncodedEightAckPredecodeProgress(t *testing.T) {
	testPrivateProviderEncodedAckCustody(t, 8)
}

// Real provider cancellation ends the child-funded receipt before TCP applies it.
func TestPrivateProviderEncodedDoubleAckCancellationReturnsOwners(t *testing.T) {
	testPrivateProviderEncodedAckCustodyWithCancel(t, 2, true)
}
