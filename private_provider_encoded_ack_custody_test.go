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

// Every pure ACK uses the actual IP producer policy: reliable, no DATA byte
// charge and immediate delivery. A separate actual DATA callback fills RX;
// the held NAT boundary retains one encoded owner and one queued successor
// until its real ACK recycles the unchanged immediate count-one slot.
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
		ackCounts := make(map[uint64]int, 11)
		enqueue := func(number uint64, count int) {
			batch := frames(count)
			if !protocolOnlyFrames(batch) || !immediateIpAckFrames(batch) {
				for _, frame := range batch {
					MessagePoolReturn(frame.MessageBytes)
				}
				t.Fatal("actual pure ACK batch does not qualify for the production protocol policy")
			}
			if !f.sender.SendMultiWithTimeout(batch, f.receiver.ClientId(), func(err error) {
				if err == nil {
					f.ackedCount++
					ackCounts[number]++
				}
			}, time.Second, NoBudget(), DeliverImmediately()) {
				for _, frame := range batch {
					MessagePoolReturn(frame.MessageBytes)
				}
				t.Fatal("actual sender refused its pure ACK owner")
			}
		}
		capture := func(number uint64, count int) *windowRoundFrame {
			original := f.takePack(number)
			if original.pack.Nack || !original.pack.DeliverImmediately || !protocolOnlyPack(original.pack) ||
				original.pack.SequenceNumber != number || len(original.pack.Frames) != count ||
				!bytes.Equal(original.pack.SequenceId, warm.pack.SequenceId) {
				t.Fatal("pure ACK original lost its actual source identity, policy or frame count")
			}
			privateProviderAssertOriginalPending(t, f, original.pack, true, true)
			privateProviderAssertImmediateCount(t, f, original.pack, 1)
			return original
		}
		feedback := func() {
			for len(f.receiverOut) != 0 {
				wire := f.take(f.receiverOut)
				if wire.ack == nil {
					if wire.pack == nil || len(wire.pack.Frames) != 1 ||
						wire.pack.Frames[0].MessageType != protocol.MessageType_IpIpProviderDiagnostics {
						t.Fatal("unexpected reverse provider feedback")
					}
					continue
				}
				f.forward(wire, f.senderIn)
			}
		}
		pressure := newPrivateProviderProtocolDataPressure(t, rx)
		defer pressure.close()
		before, childBefore := f.receiver.ReceiveStats(), recovery.Stats()
		enqueue(1, headFrameCount)
		head := capture(1, headFrameCount)
		f.forward(head, f.receiverIn)
		privateProviderAssertProtocolReceiptCustody(t, sequence, 1)
		enqueue(2, 1)
		privateProviderAssertQueuedImmediateSuccessor(t, f, head.pack)
		pressure.check(t)
		if len(entered) == 0 || tcp.returnByteCount != retainedBefore || f.ackedCount != 1 ||
			sequence.nextSequenceNumber != 1 || recovery.Stats() != childBefore ||
			root.UsedByteCount() != rx.UsedByteCount()+natBudget.UsedByteCount() {
			t.Fatal("count-one protocol/NAT barrier lost actual custody, spent DATA credit or falsely ACKed")
		}
		if cancelAfterAdmission {
			pressure.close()
			closeOwners()
			if f.ackedCount != 1 || root.UsedByteCount() != 0 || recovery.UsedByteCount() != 0 || tcp.returnByteCount != 0 {
				t.Fatal("cancellation falsely completed the encoded first owner or its accepted unencoded successor")
			}
			t.Logf("[provider-encoded-ack-canceled] head_frames=%d encoded=1 queued=1 immediate_limit=1 cumulative=1 root=0 child=0 nat=0 joined=true", headFrameCount)
			return
		}
		close(gate)
		synctest.Wait()
		privateProviderAssertReturnFrontier(t, tcp, 1780)
		privateProviderAssertOriginalPending(t, f, head.pack, true, true)
		pressure.check(t)
		feedback()
		if f.ackedCount != 2 || ackCounts[1] != 1 {
			t.Fatal("only the actual head delivery ACK may recycle the occupied immediate slot")
		}
		privateProviderAssertOriginalRetired(t, f, head.pack)
		for number := uint64(2); number <= 11; number++ {
			if number > 2 {
				enqueue(number, 1)
			}
			original := capture(number, 1)
			f.forward(original, f.receiverIn)
			privateProviderAssertReturnFrontier(t, tcp, 1780)
			privateProviderAssertOriginalPending(t, f, original.pack, true, true)
			pressure.check(t)
			feedback()
			if ackCounts[number] != 1 || f.ackedCount != int(number)+1 {
				t.Fatal("real ACK train skipped or duplicated a terminal callback while recycling count one")
			}
			privateProviderAssertOriginalRetired(t, f, original.pack)
			privateProviderAssertImmediateCount(t, f, original.pack, 0)
		}
		if f.deliveredCount != 11+headFrameCount || sequence.nextSequenceNumber != 12 ||
			sequence.receiveQueue.Len() != 0 || rx.Stats() != pressure.before || recovery.Stats() != childBefore ||
			len(tcp.sendItems) != 1 || f.receiver.ReceiveStats().ReceiveDecodePressureDropCount != before.ReceiveDecodePressureDropCount ||
			f.receiver.ReceiveStats().ReceiveQueueEvictionCount != before.ReceiveQueueEvictionCount ||
			f.receiver.ReceiveStats().ReceiveQueueTentativeEvictionCount != before.ReceiveQueueTentativeEvictionCount {
			t.Fatal("actual count-one ACK train did not close protocol ownership without changing DATA custody")
		}
		pressure.close()
		closeOwners()
		t.Logf("[provider-encoded-ack-complete] head_frames=%d returned_chunk=%d callbacks=12 committed_evictions=0 root=0 rx=0 nat=0 joined=true", headFrameCount, retainedBefore)
	})
}

// One real pure ACK remains uncharged but owned until its actual delivery ACK.
func TestPrivateProviderEncodedSingleAckRetiresReturnChunk(t *testing.T) {
	testPrivateProviderEncodedAckCustody(t, 1)
}

// A two-frame pure-ACK original has the same zero-DATA policy and real custody.
func TestPrivateProviderEncodedDoubleAckGrowthProgress(t *testing.T) {
	testPrivateProviderEncodedAckCustody(t, 2)
}

// Eight actual ACK frames remain bounded by normal count and ownership rules.
func TestPrivateProviderEncodedEightAckPredecodeProgress(t *testing.T) {
	testPrivateProviderEncodedAckCustody(t, 8)
}

// Cancellation before actual NAT admission returns every uncharged ACK owner.
func TestPrivateProviderEncodedDoubleAckCancellationReturnsOwners(t *testing.T) {
	testPrivateProviderEncodedAckCustodyWithCancel(t, 2, true)
}
