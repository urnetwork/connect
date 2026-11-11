// Signed public wire loss recreates an observed cumulative/selective gap before
// natural retries resume. This is a causal candidate, not a claimed root cause.
package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func TestProviderPrefixSevenMissingDataOriginalsRecover(t *testing.T) {
	testProviderPrefixSevenMissingOriginals(t, false)
}

// A retransmitted SYN is ordered protocol, not an immediate pure ACK. It keeps
// the same established TCP generation and exercises the other live-row class.
func TestProviderPrefixMissingOrderedSynAndDataRecover(t *testing.T) {
	testProviderPrefixSevenMissingOriginals(t, true)
}

func testProviderPrefixSevenMissingOriginals(t *testing.T, protocolHead bool) {
	t.Helper()
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		layout := consumerPaidPackByteLayout{label: "32", byteLimit: 174762,
			leafBytes: 2181038, rootBytes: 21810380, countLimit: 85}
		root := NewTransferMemoryBudget(layout.rootBytes)
		rx := NewTransferMemoryBudgetWithParent(layout.leafBytes, root)
		head := NewTransferMemoryBudgetWithParent(layout.leafBytes, root)
		pack := NewTransferMemoryBudgetWithParent(layout.leafBytes, root)
		natBudget := NewTransferMemoryBudgetWithParent(4*1024*1024, root)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{pack, head, rx, natBudget, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("provider prefix test retained quota after joined cleanup")
				}
			}
		})
		fixture, _, _, _ := newConsumerPaidPackByteFixture(t, layout, rx, head, pack, nil,
			func(_ bool, settings *ClientSettings) {
				// Only observations are enabled. Recovery intervals, ACK lifetime,
				// FIFO proof bounds and the default disabled lane rule are intact.
				settings.SendBufferSettings.LiveDiagnostics = true
			})
		if fixture.sender.settings.SendBufferSettings.ReliableLaneProvenRecovery {
			t.Fatal("provider prefix test must use the actual disabled lane rule")
		}

		path := reliableDeliveryTestPath(4)
		socket, upstream := net.Pipe()
		created := make(chan *TcpSequence, 1)
		natSettings := DefaultLocalUserNatSettingsWithBufferSize(64)
		natSettings.Log, natSettings.MemoryBudget = NewNoopLogger(), natBudget
		natSettings.TcpBufferSettings.AckCompressTimeout = 0
		natSettings.TcpBufferSettings.DialContextSettings = &DialContextSettings{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return socket, nil
			},
		}
		natSettings.TcpBufferSettings.beforeSequenceRunWithStateForTest = func(sequence *TcpSequence) {
			select {
			case created <- sequence:
			default:
				t.Error("provider prefix test unexpectedly replaced its TCP generation")
			}
		}
		nat := NewLocalUserNat(fixture.ctx, "signed provider prefix test", natSettings)
		providerSettings := DefaultRemoteUserNatProviderSettings()
		providerSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
		provider := NewRemoteUserNatProvider(fixture.receiver, nat, providerSettings)
		var receivedLock sync.Mutex
		var received []byte
		readerDone := make(chan struct{})
		go func() {
			defer close(readerDone)
			buffer := make([]byte, 1280)
			for {
				count, err := upstream.Read(buffer)
				if count != 0 {
					receivedLock.Lock()
					received = append(received, buffer[:count]...)
					receivedLock.Unlock()
				}
				if err != nil {
					return
				}
			}
		}()
		t.Cleanup(func() {
			fixture.cancel()
			provider.Close()
			upstream.Close()
			socket.Close()
			if err := nat.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			<-readerDone
			for _, done := range fixture.workerDones {
				<-done
			}
		})

		// Both directions carry real provider return packets and their ACKs.
		// Before the loss interval this drains only already-published work.
		drain := func() {
			for range 256 {
				synctest.Wait()
				if len(fixture.receiverOut) != 0 {
					fixture.forward(fixture.take(fixture.receiverOut), fixture.senderIn)
					continue
				}
				if len(fixture.senderOut) != 0 {
					fixture.forward(fixture.take(fixture.senderOut), fixture.receiverIn)
					continue
				}
				return
			}
			t.Fatal("provider prefix warm drain exceeded its finite work bound")
		}
		// Negotiate logical lanes through the actual signed lane-zero receipt.
		// No capability bit or sequence map is installed by the fixture.
		ping := RequireToFrameWithDefaultProtocolVersion(&protocol.IpPing{})
		if sent, err := fixture.sender.SendWithTimeoutDetailed(ping, fixture.receiver.ClientId(), nil, time.Second, NoBudget()); !sent || err != nil {
			MessagePoolReturn(ping.MessageBytes)
			t.Fatalf("provider prefix capability warmup failed: sent=%t err=%v", sent, err)
		}
		drain()
		var callbacks [41]atomic.Int64
		var callbackErrors atomic.Int64
		var expected []byte
		innerNumber := uint32(1001)
		var tcp *TcpSequence
		var wireSequence Id
		send := func(number uint64, syn bool) *windowRoundFrame {
			flags, packetNumber, ackNumber := byte(tcpFlagAck), innerNumber, uint32(0)
			var payload []byte
			if syn {
				flags, packetNumber = tcpFlagSyn, 1000
			} else {
				payload = bytes.Repeat([]byte{byte(number)}, 216)
				expected = append(expected, payload...)
				innerNumber += uint32(len(payload))
				tcp.mutex.Lock()
				ackNumber = tcp.receiveSeq
				tcp.mutex.Unlock()
			}
			packet := MessagePoolCopy(ipOosTcpPacketSequence(path, flags, packetNumber, payload))
			header := packet[Ipv4HeaderSizeWithoutExtensions:]
			binary.BigEndian.PutUint32(header[8:12], ackNumber)
			binary.BigEndian.PutUint16(header[16:18], 0)
			binary.BigEndian.PutUint16(header[16:18], ipPathTransportChecksum(path, ipProtocolNumberTcp, header))
			frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: packet}
			opts := []any{TransferKey{LogicalLane: 7}}
			if syn {
				if !protocolOnlyFrame(frame.MessageType, frame.Raw, frame.MessageBytes) || immediateIpAckFrame(frame.MessageType, frame.Raw, frame.MessageBytes) {
					t.Fatal("SYN control is not the actual ordered protocol class")
				}
				opts = append(opts, NoBudget())
			}
			if !fixture.sender.SendMultiWithTimeout([]*protocol.Frame{frame}, fixture.receiver.ClientId(), func(err error) {
				if err != nil {
					callbackErrors.Add(1)
				} else {
					callbacks[number].Add(1)
				}
			}, time.Second, opts...) {
				MessagePoolReturn(packet)
				t.Fatal("public provider prefix source refused its finite original")
			}
			for range 128 {
				frame := fixture.take(fixture.senderOut)
				if frame.ack != nil {
					fixture.forward(frame, fixture.receiverIn)
					continue
				}
				if frame.pack.LogicalLane != 7 {
					t.Fatal("provider prefix source escaped its exact logical lane")
				}
				if len(frame.pack.Frames) == 0 {
					fixture.forward(frame, fixture.receiverIn)
					fixture.acknowledge()
					continue
				}
				if frame.pack.SequenceNumber < number {
					// Earlier natural repair writes remain held until the
					// observation barrier; they cannot silently fill the gap.
					continue
				}
				if frame.pack.SequenceNumber != number || frame.pack.Nack || frame.pack.DeliverImmediately {
					t.Fatalf("provider prefix original mismatch: got=%d want=%d nack=%t immediate=%t", frame.pack.SequenceNumber, number, frame.pack.Nack, frame.pack.DeliverImmediately)
				}
				id := RequireIdFromBytes(frame.pack.SequenceId)
				if number == 1 {
					wireSequence = id
				} else if id != wireSequence {
					t.Fatal("provider prefix contract/source changed the sequence generation")
				}
				return frame
			}
			t.Fatal("provider prefix original hidden behind bounded wire work")
			return nil
		}

		first := send(1, true)
		fixture.forward(first, fixture.receiverIn)
		drain()
		select {
		case tcp = <-created:
		default:
			t.Fatal("real provider SYN did not construct its TCP worker")
		}
		for number := uint64(2); number <= 21; number++ {
			fixture.forward(send(number, false), fixture.receiverIn)
			drain()
			if callbacks[number].Load() != 1 {
				t.Fatalf("provider prefix warm original %d lacks actual terminal ACK", number)
			}
		}
		var sender *SendSequence
		fixture.sender.sendBuffer.mutex.Lock()
		for _, sequence := range fixture.sender.sendBuffer.sendSequencesBySequenceId {
			if sequence.sequenceId == wireSequence && sequence.logicalLane == 7 {
				sender = sequence
				break
			}
		}
		fixture.sender.sendBuffer.mutex.Unlock()
		if sender == nil {
			t.Fatal("actual provider application has no lane-seven sender generation")
		}
		receiver := consumerForeignHeadSequence(t, fixture, first)
		var originals [41]*windowRoundFrame
		for number := uint64(22); number <= 40; number++ {
			originals[number] = send(number, protocolHead && number == 22)
			if number <= 28 {
				fixture.drop(originals[number])
			} else {
				fixture.forward(originals[number], fixture.receiverIn)
				// Apply real selective receipts. Any sender repair is retained
				// by the route/test owner until the single recovery release.
				fixture.acknowledge()
			}
		}
		read := func() SendSequenceLiveSnapshot {
			sender.readSendLiveSnapshot()
			synctest.Wait()
			return sender.readSendLiveSnapshot()
		}
		before := read()
		if !before.Sampled || before.OriginalItemCount != 19 || before.UnacknowledgedItemCount != 7 ||
			before.ImmediateOriginalCount != 0 || before.ImmediateCapacityWaitCount != 0 ||
			before.CumulativeAckSequenceNumber != 21 || before.SelectiveAckSequenceNumber != 40 {
			t.Fatalf("public wire did not create the observed prefix shape: %+v", before)
		}
		for number := 22; number <= 28; number++ {
			if callbacks[number].Load() != 0 {
				t.Fatal("a physically dropped original acquired a false receipt")
			}
		}
		if receiver.ctx.Err() != nil || fixture.sender.ctx.Err() != nil || fixture.receiver.ctx.Err() != nil {
			t.Fatal("prefix shape depended on closing a shared or sequence owner")
		}
		started := time.Now()
		// Release only genuine retry copies already emitted by the sender.
		for _, held := range fixture.heldFrames {
			if held.bytes != nil && held.pack != nil && RequireIdFromBytes(held.pack.SequenceId) == wireSequence {
				fixture.forward(held, fixture.receiverIn)
			}
		}
		fixture.workerDones = append(fixture.workerDones,
			startLaneAckTestPump(fixture.ctx, fixture.senderOut, fixture.receiverIn, 0),
			startLaneAckTestPump(fixture.ctx, fixture.receiverOut, fixture.senderIn, 40*time.Millisecond))
		time.Sleep(5 * time.Second)
		synctest.Wait()
		after := read()
		for number := 1; number <= 40; number++ {
			if callbacks[number].Load() != 1 {
				t.Errorf("provider prefix original %d terminal receipts=%d, want exactly one", number, callbacks[number].Load())
			}
		}
		receivedLock.Lock()
		got := append([]byte(nil), received...)
		receivedLock.Unlock()
		if !bytes.Equal(got, expected) || callbackErrors.Load() != 0 || after.OriginalItemCount != 0 ||
			after.UnacknowledgedItemCount != 0 || after.ImmediateOriginalCount != 0 ||
			sender.ctx.Err() != nil || receiver.ctx.Err() != nil || tcp.ctx.Err() != nil {
			t.Fatalf("provider prefix failed ordered progress: protocol_head=%t bytes=%d/%d errors=%d after=%+v", protocolHead, len(got), len(expected), callbackErrors.Load(), after)
		}
		t.Logf("PROVIDER_PREFIX_RECOVERED protocol_head=%t before=%d/%d ack=%d selective=%d elapsed=%s receive_handoff_drops=%d receive_queue_drops=%d root=%d rx=%d head=%d", protocolHead,
			before.OriginalItemCount, before.UnacknowledgedItemCount, before.CumulativeAckSequenceNumber, before.SelectiveAckSequenceNumber,
			time.Since(started), fixture.receiver.ReceiveStats().PackHandoffDropCount, fixture.receiver.ReceiveStats().ReceiveQueueDropCount,
			root.UsedByteCount(), rx.UsedByteCount(), head.UsedByteCount())
	})
}
