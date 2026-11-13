// Private eight-frame candidate boundaries. Real client/peer ACK tests cover
// custody and the 9 KiB wire root; larger codec and framer paths stay unpooled.
// These are not process-heap or physical-device performance qualifications.
package connect

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

// The experiment changes H1 physical chunking, retaining callback capacity and
// the conservative mixed/H3/unknown bounds. An owned oversized frame advances.
func TestCanonicalH1EightPolicyKeepsCompatibilityAndCallbackStorage(t *testing.T) {
	if DefaultMtu != 1280 || sendPackH1GroupMaxFrames != 16 || sendPackH1EstablishedMaxMessageByteCount != 3*DefaultMtu {
		t.Fatal("candidate changed MTU, callback storage or unknown-H1 established policy")
	}
	for _, row := range []struct {
		policy  transferFlightPolicySnapshot
		frames  int
		payload ByteCount
	}{
		{transferFlightPolicySnapshot{}, 2, DefaultMtu},
		{transferFlightPolicySnapshot{h1Only: true}, 16, 3 * 1024},
		{transferFlightPolicySnapshot{h1Only: true, h1MaxMessageByteCount: 16 * 1024}, 16, 3 * 1024},
		{transferFlightPolicySnapshot{h1Only: true, h1MaxMessageByteCount: 16 * 1024, h1MessageLimitsKnown: true}, 8, 8 * DefaultMtu},
		{transferFlightPolicySnapshot{h1Only: true, h1MaxMessageByteCount: 32 * 1024, h1MessageLimitsKnown: true}, 8, 8 * DefaultMtu},
	} {
		frames, payload := sendPackChunkLimits(row.policy)
		readyFrames, readyPayload := sendPackReadyDrainLimits(row.policy)
		if frames != row.frames || payload != row.payload || readyFrames != frames || readyPayload != payload {
			t.Fatalf("physical policy=%+v logical=%d/%d ready=%d/%d want=%d/%d", row.policy, frames, payload, readyFrames, readyPayload, row.frames, row.payload)
		}
		oversized := []*protocol.Frame{{MessageBytes: make([]byte, int(payload)+1)}, {MessageBytes: []byte{1}}}
		if end := nextSendGroupChunkEndWithLimits(oversized, 0, frames, payload); end != 1 {
			t.Fatal("owned oversized original failed to advance alone", row.policy, end)
		}
	}
}

// Every input is ready before the real worker starts. No artificial ACK,
// replacement reservation, future producer, timer or batching wait is used.
func runH1EightOriginalCustody(t *testing.T, logical bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	previousCopy := DebugTransferCopyOnWrite
	DebugTransferCopyOnWrite = false
	defer func() { DebugTransferCopyOnWrite = previousCopy }()
	synctest.Test(t, func(t *testing.T) {
		root := reliableCustodyRoot(t)
		const count = 17
		size := 64
		if logical {
			size = DefaultMtu
		}
		frames, witnesses := transferGroupTestFrames(t, count+1, size)
		for _, frame := range frames {
			frame.MessageType = protocol.MessageType_IpIpPacketFromProvider
		}
		admitted := 0
		t.Cleanup(func() {
			for _, frame := range frames[admitted:] {
				MessagePoolReturn(frame.MessageBytes)
			}
			releaseTransferGroupTestWitnesses(t, frames, witnesses)
		})
		entered, released := make(chan struct{}), make(chan struct{})
		var enterOnce, releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(released) }) }
		leaf := NewTransferMemoryBudgetWithParent(2*1024*1024, root)
		fixture := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
			settings.ResendQueueBudget = leaf
			settings.ResendQueueRetainedByteAccounting = true
			settings.ResendQueueMaxByteCount = 2 * 1024 * 1024
			settings.ResendQueueMinByteCount = 0
			settings.SequenceBufferSize = 32
			settings.beforeRunSendSequenceForTest = func(sendSequenceId) {
				enterOnce.Do(func() { close(entered) })
				<-released
			}
		}, nil, TransportTypeH1)
		manager := fixture.sender.RouteManager()
		transport := privateH1FixtureSendTransport(t, fixture)
		manager.UpdateTransportWithProperties(transport, []Route{fixture.senderOut}, TransferCarrierProperties{MaxMessageByteCount: 16 * 1024})
		t.Cleanup(release)
		var received []int
		fixture.receiver.AddReceiveCallback(func(_ TransferPath, decoded []*protocol.Frame, _ Peer) {
			for _, frame := range decoded {
				if len(frame.MessageBytes) != size || !bytes.Equal(frame.MessageBytes, bytes.Repeat(frame.MessageBytes[:1], size)) {
					t.Error("real receiver changed an original's bytes")
					continue
				}
				received = append(received, int(frame.MessageBytes[0])-1)
			}
		})
		results := make(chan int, count+2)
		if logical {
			accepted, err := fixture.sender.sendGroupWithTimeoutDetailed(frames[:count], fixture.receiver.ClientId(), func(err error) {
				if err != nil {
					results <- -1
				} else {
					results <- count
				}
			}, time.Second)
			if !accepted || err != nil {
				t.Fatalf("logical originals not admitted: %t %v", accepted, err)
			}
			admitted = count
			<-entered
		} else {
			for index := range count {
				accepted, err := fixture.sender.SendWithTimeoutDetailed(frames[index], fixture.receiver.ClientId(), func(err error) {
					if err != nil {
						results <- -index - 1
					} else {
						results <- index
					}
				}, time.Second)
				if !accepted || err != nil {
					t.Fatalf("independent original%d not admitted: %t %v", index, accepted, err)
				}
				admitted++
				if index == 0 {
					<-entered
				}
			}
		}
		before := time.Now()
		release()
		wantIndex := 0
		for chunk, wantCount := range []int{8, 8, 1} {
			wire := fixture.take(fixture.senderOut)
			if wire.pack == nil || wire.pack.Nack || wire.pack.SequenceNumber != uint64(chunk) || len(wire.pack.Frames) != wantCount {
				t.Fatalf("actual chunk%d=%+v want%d originals", chunk, wire.pack, wantCount)
			}
			pooled, _ := MessagePoolCheck(wire.bytes)
			if logical && wantCount == 8 {
				rootBytes := MessagePoolRootByteCount(wire.bytes)
				if !pooled || len(wire.bytes) <= 8192 || 12288 < len(wire.bytes) ||
					cap(wire.bytes) != 12288+MessagePoolMetaByteCount || rootBytes != 12288 {
					t.Fatalf("eight full-MTU originals did not use the 12288-byte pooled wire root: len=%d cap=%d pooled=%t root=%d",
						len(wire.bytes), cap(wire.bytes), pooled, rootBytes)
				}
				t.Logf("h1_eight_wire chunk=%d payloadBytes=%d wireBytes=%d rootCapacity=%d retainedRootBytes=%d pooled=%t",
					chunk, wantCount*size, len(wire.bytes), cap(wire.bytes), rootBytes, pooled)
			}
			for offset, frame := range wire.pack.Frames {
				if !bytes.Equal(frame.MessageBytes, bytes.Repeat([]byte{byte(wantIndex + offset + 1)}, size)) {
					t.Fatal("physical chunk changed FIFO order or bytes", chunk, offset)
				}
			}
			if leaf.UsedByteCount() <= 0 || root.UsedByteCount() != leaf.UsedByteCount() {
				t.Fatal("actual pending chunk escaped its unchanged parented retained budget")
			}
			fixture.forward(wire, fixture.receiverIn)
			if len(results) != 0 {
				t.Fatal("original completed before its real peer ACK was forwarded")
			}
			fixture.acknowledge()
			if logical {
				if chunk == 2 {
					if got := <-results; got != count {
						t.Fatal("one logical completion did not cover every chunk", got)
					}
				} else if len(results) != 0 {
					t.Fatal("logical completion preceded a suffix's peer ACK")
				}
			} else {
				for offset := range wantCount {
					if got := <-results; got != wantIndex+offset {
						t.Fatal("independent callback order/exactly-once changed", got, wantIndex+offset)
					}
				}
			}
			wantIndex += wantCount
		}
		wantReceived := make([]int, count)
		for index := range wantReceived {
			wantReceived[index] = index
		}
		if !reflect.DeepEqual(received, wantReceived) || len(results) != 0 || len(fixture.senderOut) != 0 || !time.Now().Equal(before) {
			t.Fatal("chunk census, exactly-once receiver order, extra wire or no-wait boundary changed", received)
		}
		assertRetainedBudgetBalance(t, leaf)
		assertRetainedBudgetBalance(t, root)
		if !logical {
			// No future producer exists. A sparse input must reach the real wire
			// at the same synthetic instant without waiting to fill eight slots.
			accepted, err := fixture.sender.SendWithTimeoutDetailed(frames[count], fixture.receiver.ClientId(), func(err error) {
				if err != nil {
					results <- -1
				} else {
					results <- count
				}
			}, time.Second)
			if !accepted || err != nil {
				t.Fatal("sparse original refused", accepted, err)
			}
			admitted++
			wire := fixture.take(fixture.senderOut)
			if wire.pack == nil || len(wire.pack.Frames) != 1 || !time.Now().Equal(before) {
				t.Fatal("sparse original waited for a future batching event")
			}
			fixture.forward(wire, fixture.receiverIn)
			fixture.acknowledge()
			if got := <-results; got != count || len(received) != count+1 || received[count] != count {
				t.Fatal("sparse original did not complete once through the real peer", got, received)
			}
			assertRetainedBudgetBalance(t, leaf)
			assertRetainedBudgetBalance(t, root)
		}
		t.Logf("h1_eight_originals logical=%t frames=%d chunks=[8 8 1] frameBytes=%d parentLimit=%d zeroTimerAdvance=%t", logical, count, size, root.TotalByteCount(), time.Now().Equal(before))
	})
}

// One logical owner spans all three physical chunks and completes on peer ACKs.
func TestCanonicalH1EightLogicalGroupKeepsOrderAndPeerAckCustody(t *testing.T) {
	runH1EightOriginalCustody(t, true)
}

// Already-ready originals retain seventeen independent callbacks; sparse work
// still writes immediately after the cohort has drained.
func TestCanonicalH1EightReadyDrainKeepsOriginalsAndSparseLatency(t *testing.T) {
	runH1EightOriginalCustody(t, false)
}

// Real rewrite/AEAD/protobuf components exercise the larger wire root under
// the unchanged three/five-root sender envelope. The synthetic contract is
// an encoding fixture; real contract authentication belongs to the SDK gate.
func TestCanonicalH1EightFullContractRewriteAndEncryptionKeepRetainedCharge(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{1, 2} {
		func() {
			parent := NewTransferMemoryBudget(mib(1))
			leaf := NewTransferMemoryBudgetWithParent(mib(1), parent)
			settings := DefaultSendBufferSettings()
			settings.ProtocolVersion = version
			settings.ResendQueueBudget = leaf
			settings.ResendQueueRetainedByteAccounting = true
			sequence := NewSendSequence(context.Background(), &Client{log: NewNoopLogger()}, nil, NewId(), MultiHopId{}, false, false, false, sequenceTlsRoleClient, false, settings)
			defer sequence.Close()
			contractId := NewId()
			contract := &sequenceContract{contractId: contractId, contract: &protocol.Contract{StoredContractBytes: bytes.Repeat([]byte{0x31}, 4096)}}
			sequence.sendContract = contract
			sequence.openSendContracts[contractId] = contract
			frames := make([]*protocol.Frame, 8)
			for index := range frames {
				frames[index] = &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, MessageBytes: bytes.Repeat([]byte{byte(index + 1)}, DefaultMtu)}
			}
			item := takeSendItem()
			*item = sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 1}, contractId: &contractId, expectsAck: true}
			estimate := sequence.retainedSendFrameByteCount(MessageByteCount(frames), len(frames))
			charge := item.retainedMemoryByteCount(estimate, len(frames), version < 2)
			if !item.reserveMemory(leaf, charge) {
				t.Fatal("full future-contract envelope was not prepaid")
			}
			pack := &protocol.Pack{MessageId: item.messageId.Bytes(), SequenceId: sequence.sequenceId.Bytes(), SequenceNumber: 1, Frames: frames}
			transfer := &protocol.TransferFrame{Pack: pack}
			if version < 2 {
				encoded, err := ProtoMarshal(pack)
				if err != nil {
					t.Fatal(err)
				}
				defer MessagePoolReturn(encoded)
				transfer.Pack = nil
				transfer.Frame = &protocol.Frame{MessageType: protocol.MessageType_TransferPack, MessageBytes: encoded}
			}
			var err error
			item.transferFrameBytes, err = ProtoMarshal(transfer)
			if err != nil {
				t.Fatal(err)
			}
			original := item.transferFrameBytes
			originalCopy := bytes.Clone(original)
			sequence.sendItems = []*sendItem{item}
			sequence.resendQueue.Add(item)
			sequence.resendQueue.RemoveByMessageId(item.messageId)
			leaf.SetTotalByteCount(charge)
			rewritten, fullContract, err := sequence.setHead(item, true)
			if err != nil || !fullContract {
				t.Fatal("full-budget actual rewrite failed", fullContract, err)
			}
			sequence.replaceSendItemFrame(item, rewritten)
			if !bytes.Equal(original, originalCopy) || &original[0] == &rewritten[0] || leaf.UsedByteCount() != charge || parent.UsedByteCount() != charge {
				t.Fatal("rewrite mutated a live old root or released/reborrowed its prepaid charge")
			}
			block, err := aes.NewCipher(bytes.Repeat([]byte{byte(version)}, 32))
			if err != nil {
				t.Fatal(err)
			}
			aead, err := cipher.NewGCM(block)
			if err != nil {
				t.Fatal(err)
			}
			codec := &sequenceCipher{aead: aead}
			outer, err := codec.SealOuterFrame(NewTransferPath(NewId(), NewId(), Id{}), rewritten, protocol.SequenceRole_SequenceRoleClient, false)
			if err != nil {
				t.Fatal(err)
			}
			defer MessagePoolReturn(outer)
			var wrapped protocol.TransferFrame
			if err := proto.Unmarshal(outer, &wrapped); err != nil {
				t.Fatal(err)
			}
			opened, err := codec.Open(wrapped.EncryptedTransferFrame)
			if err != nil {
				t.Fatal(err)
			}
			defer MessagePoolReturn(opened)
			pooled, _ := MessagePoolCheck(outer)
			if !bytes.Equal(opened, rewritten) || pooled || len(outer) <= 12288 ||
				MessagePoolRootByteCount(outer) != ByteCount(len(outer)) ||
				ByteCount(len(outer)) > DefaultClientSettings().MinimumMessageLenLimit() {
				t.Fatal("actual encrypted full-contract bytes or admitted unpooled class changed", len(outer), pooled)
			}
			if leaf.UsedByteCount() != charge || parent.UsedByteCount() != charge {
				t.Fatal("larger unpooled codec changed the exact prepaid retained charge")
			}
			t.Logf("h1_eight_codec version=%d originalBytes=%d rewriteBytes=%d encryptedBytes=%d encryptedRootCapacity=%d estimatedFullContractBytes=%d senderCharge=%d pooled=%t", version, len(original), len(rewritten), len(outer), cap(outer), estimate, charge, pooled)
			sequence.releaseRetainedSendItems(context.Canceled)
			assertRetainedBudgetBalance(t, leaf)
			assertRetainedBudgetBalance(t, parent)
		}()
	}
}

// Large ordinary messages may overshoot the drain threshold by one atomic
// message. They must not change the eight-ACK quantum or grow writer scratch.
func TestCanonicalH1EightLargeFramedFlushPreservesByteBoundAndAckFairness(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const size = 8*DefaultMtu + 768
		priority, ordinary := make(chan []byte, 40), make(chan []byte, 4)
		defer h1PlusDrainTestOwnedMessages(priority)
		defer h1PlusDrainTestOwnedMessages(ordinary)
		for index := range 40 {
			priority <- h1PlusOwnedTestMessage(byte(index+1), 64)
		}
		for index := range 4 {
			ordinary <- h1PlusOwnedTestMessage(byte(0xa0+index), size)
		}
		raw := newH1UpgradeScriptConn(nil)
		maximum := int(DefaultClientSettings().MinimumMessageLenLimit())
		conn, err := NewFramedMessageConn(raw, H1FramerProtocol, maximum, nil)
		if err != nil {
			t.Fatal(err)
		}
		before := time.Now()
		sent := 0
		open, err := writeH1FramedReadyBatch(context.Background(), conn, ordinary, priority, h1PlusOwnedTestMessage(0, 64), true, time.Second, func() { sent++ })
		if err != nil || !open || sent != 18 || len(priority) != 25 || len(ordinary) != 2 || cap(conn.storage) != webSocketWriteBatchMaxByteCount || raw.writeCalls != 2 {
			t.Fatalf("large framed batch changed its bound/queue ownership: open=%t sent=%d priority=%d ordinary=%d scratch=%d writes=%d err=%v", open, sent, len(priority), len(ordinary), cap(conn.storage), raw.writeCalls, err)
		}
		reader := bytes.NewReader(raw.output.Bytes())
		framer := NewFramer(DefaultFramerSettings(maximum))
		payloadBytes := 0
		for index := range 18 {
			message, err := framer.Read(reader)
			if err != nil {
				t.Fatal(err)
			}
			wantLabel, wantSize := byte(index-index/9), 64
			if index == 8 || index == 17 {
				wantLabel, wantSize = byte(0xa0+index/9), size
			}
			ok := len(message) == wantSize && bytes.Equal(message, bytes.Repeat([]byte{wantLabel}, wantSize))
			payloadBytes += len(message)
			MessagePoolReturn(message)
			if !ok {
				t.Fatal("large ordinary frame or ACK quantum changed", index)
			}
		}
		if reader.Len() != 0 || payloadBytes < platformWebSocketWriteBatchDrainByteCount || payloadBytes >= platformWebSocketWriteBatchDrainByteCount+size || !time.Now().Equal(before) {
			t.Fatal("framed drain exceeded one atomic-message overshoot or waited for future work", payloadBytes)
		}
		t.Logf("h1_eight_flush ordinaryBytes=%d payloadBytes=%d messages=%d physicalWrites=%d scratchBytes=%d ackQuantum=%d", size, payloadBytes, sent, raw.writeCalls, cap(conn.storage), platformWebSocketAckPriorityBurstMaxMessages)
	})
}
