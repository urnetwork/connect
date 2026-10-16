package connect

import (
	"bytes"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

// A synthetic TCP flow between documentation addresses.
func reliableDeliveryTestPath(version int) *IpPath {
	if version == 4 {
		return &IpPath{Version: 4, Protocol: IpProtocolTcp,
			SourceIp: net.ParseIP("192.0.2.10"), SourcePort: 49152,
			DestinationIp: net.ParseIP("198.51.100.20"), DestinationPort: 443}
	}
	return &IpPath{Version: 6, Protocol: IpProtocolTcp,
		SourceIp: net.ParseIP("2001:db8::10"), SourcePort: 49152,
		DestinationIp: net.ParseIP("2001:db8:1::20"), DestinationPort: 443}
}

// One in-order TCP segment of the upload, as its own Pack.
func (f *reliableTcpIngressFixture) uploadPack(sequence *ReceiveSequence, number uint64, tcpSequenceNumber uint32, payload []byte) (*ReceivePack, Id) {
	f.t.Helper()
	packet := MessagePoolCopy(ipOosTcpPacketSequence(f.path, tcpFlagAck, tcpSequenceNumber, payload))
	frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
	if err != nil || !frame.Raw {
		MessagePoolReturn(packet)
		f.t.Fatalf("raw upload frame: frame=%+v err=%v", frame, err)
	}
	messageId := NewId()
	return &ReceivePack{Source: f.source, SequenceId: sequence.sequenceId,
		Pack: &protocol.Pack{MessageId: messageId.Bytes(), SequenceId: sequence.sequenceId.Bytes(),
			SequenceNumber: number, Head: number == 0, Frames: []*protocol.Frame{frame}},
		ReceiveCallback: f.client.receiveCallback, Ctx: f.ctx, Unwrapped: true,
		EncryptionRole: sequenceTlsRoleServer, MessageByteCount: ByteCount(len(packet)),
	}, messageId
}

// The receipt ledger is bounded by count (the sequence buffer), while a
// conforming sender is bounded only by the advertised receive window, in bytes
// from the cumulative ACK. A healthy upload whose final TCP queue drains slower
// than segments arrive therefore has more segments outstanding than the ledger
// has receipts. The head after a full ledger was refused only after the
// sequence had delivered past it, and that refusal canceled the whole source
// sequence. It must wait in the receive hold instead: no cumulative ACK, no
// cancellation, and every segment reaches the final TCP queue in order once it
// drains. The upload stays far inside the advertised window throughout.
func TestProviderReliableTcpFullReceiptLedgerHoldsTheHead(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		synctest.Test(t, func(t *testing.T) {
			f := newReliableTcpIngressFixtureWithPath(t, reliableDeliveryTestPath(version), nil)
			// Every segment passes the NAT shard; its construction-only signal
			// has a fixed buffer and would otherwise block the shard.
			f.nat.afterSendPacketForTest = nil
			sequence := f.runningReceiveSequence()
			ledgerCount := sequence.receiveBufferSettings.SequenceBufferSize
			count := ledgerCount + 1
			payload := []byte("healthy upload segment")
			tcpSequenceNumber := func(number int) uint32 { return uint32(101 + number*len(payload)) }
			ids := make([]Id, count)
			outstanding := ByteCount(0)
			for number := range count {
				pack, messageId := f.uploadPack(sequence, uint64(number), tcpSequenceNumber(number), payload)
				ids[number] = messageId
				outstanding += pack.MessageByteCount
				if ok, err := sequence.Pack(pack, 0); !ok || err != nil {
					pack.messagePoolReturn()
					t.Fatalf("ipv%d segment %d was refused: ok=%t err=%v sequence=%v", version, number, ok, err, sequence.ctx.Err())
				}
				synctest.Wait()
			}
			if window := ByteCount(sequence.receiveWindowAdvertisement()); window < outstanding {
				t.Fatalf("ipv%d upload exceeds the advertised window: outstanding=%d window=%d", version, outstanding, window)
			}
			if err := sequence.ctx.Err(); err != nil {
				t.Fatalf("ipv%d a full receipt ledger (%d) canceled a healthy upload of %d bytes: %v", version, ledgerCount, outstanding, err)
			}
			if held := sequence.receiveQueue.GetBySequenceNumber(uint64(ledgerCount)); held == nil || held.messageId != ids[ledgerCount] {
				t.Fatalf("ipv%d the head after the full ledger was not held", version)
			}
			if present, head := reliableIngressCumulativeHead(sequence); present {
				t.Fatalf("ipv%d pending TCP data was cumulatively ACKed: head=%s", version, head.messageId)
			}

			// Drain the final TCP queue: the SYN, then every segment in order.
			(<-f.tcp.sendItems).release()
			for number := range count {
				synctest.Wait()
				select {
				case item := <-f.tcp.sendItems:
					if item.tcp.seq != tcpSequenceNumber(number) || !bytes.Equal(item.tcp.payload, payload) {
						t.Errorf("ipv%d segment %d reached TCP out of order: seq=%d want=%d", version, number, item.tcp.seq, tcpSequenceNumber(number))
					}
					item.release()
				default:
					t.Fatalf("ipv%d segment %d did not reach the final TCP queue", version, number)
				}
			}
			synctest.Wait()
			if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != ids[count-1] {
				t.Errorf("ipv%d the drained upload was not cumulatively ACKed: present=%t head=%s want=%s", version, present, head.messageId, ids[count-1])
			}
			if err := sequence.ctx.Err(); err != nil || sequence.receiveQueue.Len() != 0 {
				t.Errorf("ipv%d the drained sequence ended or kept its hold: err=%v held=%d", version, err, sequence.receiveQueue.Len())
			}
		})
	}
}

// The ledger can drain on a downstream worker after the sequence refused its
// head and before the sequence chose how to wait. With no receipt pending, the
// sequence used to wait only for Packs and its gap timer, which a sender whose
// window is spent behind the held head never ends: the upload stalled for the
// gap timeout. The idle wait must take the ledger's drain edge as well. The
// race is forced here: the delivered batch's progress event is a barrier
// between the refusal and the wait, inside which the downstream secures the
// ledger's only receipt.
func TestProviderReliableTcpHeldHeadWakesOnLedgerDrainBeforeIdleWait(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		synctest.Test(t, func(t *testing.T) {
			f := newReliableTcpIngressFixtureWithPath(t, reliableDeliveryTestPath(version), nil)
			f.nat.afterSendPacketForTest = nil
			// Free the one-slot final TCP queue, then let each owner leave the
			// NAT shard only on its own token.
			(<-f.tcp.sendItems).release()
			gate := make(chan struct{}, 4)
			f.nat.beforeSendPacketForTest = func(*SendPacket) {
				select {
				case <-gate:
				case <-f.ctx.Done():
				}
			}
			var armed atomic.Bool
			var barrier sync.Once
			var sequence *ReceiveSequence
			var target Id
			drained := make(chan struct{}, 1)
			sequence = f.runningReceiveSequence(func(settings *ReceiveBufferSettings) {
				// A one-receipt ledger: one delivered head fills it.
				settings.SequenceBufferSize = 1
				settings.afterAckWindowUpdateForTest = func(receiveSequenceId) {
					if !armed.Load() {
						return
					}
					if present, head := reliableIngressCumulativeHead(sequence); present && head.messageId == target {
						select {
						case drained <- struct{}{}:
						default:
						}
					}
				}
				settings.ProgressObserver = func(event TransferProgressEvent) {
					if event.Stage != "deliver_end" || !armed.Load() {
						return
					}
					barrier.Do(func() {
						// The second head was just delivered and its next held.
						// Secure the second head before the sequence waits.
						(<-f.tcp.sendItems).release()
						gate <- struct{}{}
						select {
						case <-drained:
						case <-f.ctx.Done():
						}
					})
				}
			})
			payload := []byte("segment behind a full ledger")
			ids := make([]Id, 3)
			for number := range 3 {
				pack, messageId := f.uploadPack(sequence, uint64(number), uint32(101+number*len(payload)), payload)
				ids[number] = messageId
				if ok, err := sequence.Pack(pack, 0); !ok || err != nil {
					pack.messagePoolReturn()
					t.Fatalf("ipv%d segment %d was refused: ok=%t err=%v sequence=%v", version, number, ok, err, sequence.ctx.Err())
				}
				synctest.Wait()
			}
			if held := sequence.receiveQueue.Len(); held != 2 {
				t.Fatalf("ipv%d the full ledger did not hold the later heads: held=%d", version, held)
			}
			target = ids[1]
			armed.Store(true)
			// The first head leaves the NAT and frees the ledger. The sequence
			// delivers the second head, refuses the third, and the barrier then
			// drains the ledger again before the sequence waits.
			gate <- struct{}{}
			synctest.Wait()
			if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != ids[1] {
				t.Fatalf("ipv%d the barrier did not secure the second head: present=%t head=%s", version, present, head.messageId)
			}
			if held := sequence.receiveQueue.Len(); held != 0 {
				t.Fatalf("ipv%d the held head missed the ledger drain and waits for the gap timer: held=%d", version, held)
			}
			if err := sequence.ctx.Err(); err != nil {
				t.Fatalf("ipv%d the sequence ended: %v", version, err)
			}
		})
	}
}

// A retransmitted copy of a head held for ledger capacity is a duplicate of
// an owner the sequence keeps and has selectively acknowledged. Replacing the
// held original would release its exact reservation for another flow to take
// before the copy is admitted again, losing a leased segment. The original
// stays held, and the copy is dropped.
func TestProviderReliableTcpHeldHeadKeepsItsOwnerAgainstADuplicate(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		synctest.Test(t, func(t *testing.T) {
			f := newReliableTcpIngressFixtureWithPath(t, reliableDeliveryTestPath(version), nil)
			sequence := f.runningReceiveSequence(func(settings *ReceiveBufferSettings) {
				settings.SequenceBufferSize = 1
			})
			payload := []byte("segment held behind a full ledger")
			for number := range 2 {
				pack, _ := f.uploadPack(sequence, uint64(number), uint32(101+number*len(payload)), payload)
				if ok, err := sequence.Pack(pack, 0); !ok || err != nil {
					pack.messagePoolReturn()
					t.Fatalf("ipv%d segment %d was refused: ok=%t err=%v sequence=%v", version, number, ok, err, sequence.ctx.Err())
				}
				synctest.Wait()
			}
			held := sequence.receiveQueue.GetBySequenceNumber(1)
			if held == nil || !held.committed {
				t.Fatalf("ipv%d the head after the full ledger was not held and leased", version)
			}
			heldId, heldFrames := held.messageId, held.frames
			if !f.packWithIdentity(sequence, 1, payload, heldId) {
				t.Fatalf("ipv%d the duplicate was refused before the receive sequence", version)
			}
			synctest.Wait()
			if again := sequence.receiveQueue.GetBySequenceNumber(1); again != held || len(again.frames) != len(heldFrames) || again.frames[0] != heldFrames[0] {
				t.Fatalf("ipv%d a duplicate replaced the held, leased owner", version)
			}
			if held := sequence.receiveQueue.Len(); held != 1 {
				t.Fatalf("ipv%d the duplicate was held beside its original: held=%d", version, held)
			}
		})
	}
}

// A head is admitted to the ledger before it is delivered, so an empty ledger
// takes any one owner its memory admits, as the receive hold does. Otherwise a
// head whose receipt is larger than the ledger's byte bound could wait for a
// drain that no receipt can ever make; refused after delivery, it canceled
// the sequence.
func TestProviderReliableTcpEmptyLedgerAdmitsAHeadAboveItsByteBound(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		synctest.Test(t, func(t *testing.T) {
			f := newReliableTcpIngressFixtureWithPath(t, reliableDeliveryTestPath(version), nil)
			payload := bytes.Repeat([]byte{0x42}, 1000)
			sequence := f.runningReceiveSequence(func(settings *ReceiveBufferSettings) {
				settings.ReceiveQueueMaxByteCount = 1024
			})
			pack, messageId := f.uploadPack(sequence, 0, 101, payload)
			if ok, err := sequence.Pack(pack, 0); !ok || err != nil {
				pack.messagePoolReturn()
				t.Fatalf("ipv%d the segment was refused: ok=%t err=%v", version, ok, err)
			}
			synctest.Wait()
			if err := sequence.ctx.Err(); err != nil {
				t.Fatalf("ipv%d a head above the ledger's byte bound canceled the sequence: %v", version, err)
			}
			if held := sequence.receiveQueue.Len(); held != 0 {
				t.Fatalf("ipv%d the empty ledger left the head held: held=%d", version, held)
			}
			(<-f.tcp.sendItems).release()
			synctest.Wait()
			select {
			case item := <-f.tcp.sendItems:
				if !bytes.Equal(item.tcp.payload, payload) {
					t.Errorf("ipv%d the admitted head changed", version)
				}
				item.release()
			default:
				t.Fatalf("ipv%d the admitted head never reached the final TCP queue", version)
			}
			synctest.Wait()
			if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != messageId {
				t.Errorf("ipv%d the head was not ACKed: present=%t head=%s", version, present, head.messageId)
			}
		})
	}
}
