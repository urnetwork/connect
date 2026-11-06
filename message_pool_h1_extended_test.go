// Extended H1 roots retain explicit class charges and final-share ownership.
// Every aggregate assertion owns a fresh process; no pool or counter is replaced.
package connect

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"io"
	"math"
	"testing"
	"testing/iotest"
	"unsafe"
)

// The new class must not change old packet/eight-KiB roots or silently pool an
// arbitrarily large message. The twelve metadata bytes belong to admission.
func TestMessagePoolExtendedClassOrderingAndCharges(t *testing.T) {
	if messagePoolSnapshotInFreshProcess(t) {
		return
	}
	before := encodingPoolBoundaryNow()
	t.Cleanup(func() { encodingPoolRequireBalanced(t, before) })
	wantClasses := []int{256, 2048, 4096, 8192, 12288}
	pools := orderedMessagePools()
	if len(pools) != len(wantClasses) {
		t.Fatalf("EXTENDED_H1_POOL_CLASS_MISSING: classes=%d want=%d", len(pools), len(wantClasses))
	}
	for index, want := range wantClasses {
		if pools[index].size != want {
			t.Fatalf("pool class %d=%d want=%d", index, pools[index].size, want)
		}
	}
	t.Logf("added_class_fixed_object_bytes=%d counter_bytes=%d payload_class=12288 allocation_request=12300", unsafe.Sizeof(messagePool{}), messagePoolShardCount*3*256*8)
	for _, row := range []struct {
		size   int
		root   int
		pooled bool
	}{
		{size: 256, root: 256, pooled: true},
		{size: 257, root: 2048, pooled: true},
		{size: 4097, root: 8192, pooled: true},
		{size: 8192, root: 8192, pooled: true},
		{size: 8193, root: 12288, pooled: true},
		{size: 9000, root: 12288, pooled: true},
		{size: 12288, root: 12288, pooled: true},
		{size: 12289, root: 12289, pooled: false},
		{size: 16 * 1024, root: 16 * 1024, pooled: false},
	} {
		message, pooled := MessagePoolGetDetailed(row.size)
		capacity := cap(message)
		charge := MessagePoolRootByteCount(message)
		retained := retainedMessageCapacity(ByteCount(row.size))
		returned := MessagePoolReturn(message)
		if pooled != row.pooled || returned != row.pooled || capacity != row.root+MessagePoolMetaByteCount ||
			charge != ByteCount(row.root) || retained != ByteCount(row.root+MessagePoolMetaByteCount) {
			t.Fatalf("size=%d pooled=%t returned=%t cap=%d rootcharge=%d retained=%d wantroot=%d", row.size, pooled, returned, capacity, charge, retained, row.root)
		}
	}
}

// A custom tiny H1 base must fund the actual final read scratch rather than
// retaining the old eight-KiB assumption. Mobile targets retain their gates.
func TestMessagePoolExtendedH1ScratchUsesFinalClass(t *testing.T) {
	if messagePoolSnapshotInFreshProcess(t) {
		return
	}
	for _, target := range []ByteCount{32 * 1024 * 1024, 64 * 1024 * 1024} {
		settings := testingDefaultPlatformTransportSettingsWithMemoryTarget(target)
		// An explicit 64-MiB target keeps desktop/raw semantics until its
		// owner selects the same public mobile policy used by the SDK.
		if target == 64*1024*1024 {
			settings.H1BudgetByteCount = 1
			if got := settings.H1MemoryReservationByteCount(); got != 1 {
				t.Fatalf("explicit raw64 H1 base changed before mobile selection: %d", got)
			}
		}
		ApplyMobilePlatformTransportMemoryPolicy(settings, target)
		settings.TransportBufferSize = 0
		settings.H1AckPriorityBufferSize = 0
		settings.H1MaxMessageByteCount = 16 * 1024
		settings.H1BudgetByteCount = 1
		settings.FramerSettings = DefaultFramerSettings(16 * 1024)
		want := ByteCount(4*(16*1024+1)+12300+webSocketWriteBatchMaxByteCount+8*1024) + retainedMessageCapacity(16*1024+4)
		if got := settings.h1RetainedScratchMinimum(); got != want {
			t.Fatalf("extended H1 scratch target=%d got=%d want=%d", target, got, want)
		}
		if got := settings.H1MemoryReservationByteCount(); got != want+3*retainedMessageCapacity(16*1024) {
			t.Fatalf("extended H1 graph target=%d claim=%d", target, got)
		}
		settings.H1MaxMessageByteCount = math.MaxInt64
		if got := settings.h1RetainedScratchMinimum(); got != ByteCount(math.MaxInt64) {
			t.Fatal("extended scratch overflow wrapped its claim")
		}
	}
}

// Actual rotating shards must reuse returned twelve-KiB roots. A live read-only
// carrier share prevents reuse, even after trim, resize and free-list clearing.
func TestMessagePoolExtendedReuseAndSharedLifetime(t *testing.T) {
	if messagePoolSnapshotInFreshProcess(t) {
		return
	}
	before := encodingPoolBoundaryNow()
	t.Cleanup(func() { encodingPoolRequireBalanced(t, before) })
	ResizeMessagePools(0, 2*1024*1024)
	ClearMessagePools()
	oldRoots := make([][]byte, messagePoolShardCount)
	oldLive := make([]bool, messagePoolShardCount)
	t.Cleanup(func() {
		for index, live := range oldLive {
			if live {
				MessagePoolReturn(oldRoots[index])
			}
		}
	})
	for index := range oldRoots {
		oldRoots[index] = MessagePoolGet(10800)
		oldLive[index] = true
	}
	for index, message := range oldRoots {
		returned := MessagePoolReturn(message)
		oldLive[index] = false
		if !returned {
			t.Fatal("twelve-KiB owner was not pooled")
		}
	}
	_, _, created := MessagePoolCounts()
	for index := range oldRoots {
		message := MessagePoolGet(10800)
		matched := &message[0] == &oldRoots[index][0]
		MessagePoolReturn(message)
		if !matched {
			t.Fatal("actual acquisition shard did not reuse its returned extended root")
		}
	}
	if _, _, after := MessagePoolCounts(); after != created {
		t.Fatalf("bounded extended reuse allocated %d new roots", after-created)
	}
	owner := MessagePoolGet(10800)
	for index := range owner {
		owner[index] = byte(index)
	}
	share := MessagePoolShareReadOnly(owner)
	ownerLive, shareLive := true, true
	t.Cleanup(func() {
		if ownerLive {
			MessagePoolReturn(owner)
		}
		if shareLive {
			MessagePoolReturn(share)
		}
	})
	if pooled, shared := MessagePoolCheck(owner); !pooled || !shared {
		t.Fatal("extended carrier did not acquire a real shared owner")
	}
	TrimMessagePoolsTo(0)
	ResizeMessagePools(0, 0)
	ClearMessagePools()
	for range messagePoolShardCount {
		other := MessagePoolGet(10800)
		aliased := &other[0] == &share[0]
		MessagePoolReturn(other)
		if aliased {
			t.Fatal("administrative trim reused a live carrier root")
		}
	}
	returnedOwner := MessagePoolReturn(owner)
	ownerLive = false
	if returnedOwner {
		t.Fatal("first return prematurely released the shared root")
	}
	for index, value := range share {
		if value != byte(index) {
			t.Fatal("live carrier bytes changed before final return")
		}
	}
	returnedShare := MessagePoolReturn(share)
	shareLive = false
	if !returnedShare {
		t.Fatal("last carrier share did not release the extended root")
	}
	for _, pool := range orderedMessagePools() {
		if pool.size == 12288 && (pool.capacity() != 0 || pool.snapshot().retained != 0) {
			t.Fatal("zero extended free allowance retained a returned root")
		}
	}
}

// The larger class spends residual permission from the same aggregate. Tiny,
// negative and historical one-argument budgets do not create a new floor.
func TestMessagePoolExtendedFreeRetentionConservesAllowance(t *testing.T) {
	if messagePoolSnapshotInFreshProcess(t) {
		return
	}
	before := encodingPoolBoundaryNow()
	t.Cleanup(func() { encodingPoolRequireBalanced(t, before) })
	for _, allowance := range []ByteCount{math.MinInt64, -1, 0, 256 * 1024, 512 * 1024, 512*1024 + 1, 2 * 1024 * 1024} {
		ResizeMessagePools(0, allowance)
		largeCapacity := ByteCount(0)
		for _, pool := range orderedMessagePools() {
			if isPacketPoolSize(pool.size) {
				continue
			}
			largeCapacity += ByteCount(pool.capacity() * pool.size)
			if pool.size == 12288 && allowance <= 512*1024 && pool.capacity() != 0 {
				t.Fatal("new class took a free-retention floor")
			}
		}
		if largeCapacity > max(ByteCount(512*1024), allowance) {
			t.Fatalf("large free allowance grew: requested=%d capacity=%d", allowance, largeCapacity)
		}
	}
	ResizeMessagePools(0, 2*1024*1024)
	for _, pool := range orderedMessagePools() {
		if isPacketPoolSize(pool.size) {
			continue
		}
		messages := make([][]byte, pool.capacity()+messagePoolShardCount)
		for index := range messages {
			messages[index] = MessagePoolGet(pool.size)
		}
		for _, message := range messages {
			MessagePoolReturn(message)
		}
		if pool.snapshot().retained > pool.capacity() {
			t.Fatal("returns exceeded configured free capacity")
		}
	}
	TrimMessagePoolsTo(0)
	for _, pool := range orderedMessagePools() {
		if pool.size == 12288 && pool.snapshot().retained != 0 {
			t.Fatal("trim retained the extended burst")
		}
	}
	ResizeMessagePools(3 * 1024 * 1024)
	for _, pool := range orderedMessagePools() {
		if pool.size == 12288 && pool.capacity() != 0 {
			t.Fatal("legacy resize silently multiplied its aggregate cap")
		}
		if (pool.size == 4096 || pool.size == 8192) && pool.capacity()*pool.size != 3*1024*1024 {
			t.Fatal("legacy resize changed an existing large-class cap")
		}
	}
}

// Both ordinary Read calls and final n>0,EOF must preserve the real twelve-KiB
// stream. Exact-size framers share the same class; the next class stays finite.
func TestMessagePoolExtendedReadAllAndFramedOwnership(t *testing.T) {
	if messagePoolSnapshotInFreshProcess(t) {
		return
	}
	before := encodingPoolBoundaryNow()
	t.Cleanup(func() { encodingPoolRequireBalanced(t, before) })
	for _, size := range []int{8193, 9000, 10800, 12288, 12289, 16 * 1024} {
		payload := bytes.Repeat([]byte{0x56}, size)
		for _, finalEof := range []bool{false, true} {
			var reader io.Reader = bytes.NewReader(payload)
			if finalEof {
				reader = iotest.DataErrReader(reader)
			}
			message, err := MessagePoolReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			pooled, _ := MessagePoolCheck(message)
			// An exact-full final class with deferred EOF retains the existing
			// overflow path; this experiment does not change reader policy.
			wantPooled := size < 12288 || (size == 12288 && finalEof)
			match := bytes.Equal(message, payload)
			capacity := cap(message)
			MessagePoolReturn(message)
			if !match || pooled != wantPooled || (pooled && capacity != 12300) || (!pooled && capacity != 16*1024) {
				t.Fatalf("EXTENDED_H1_READER_ROOT: size=%d finalEof=%t pooled=%t cap=%d", size, finalEof, pooled, capacity)
			}
		}
		raw := newH1UpgradeScriptConn(framedMessageTestWire(H1FramerProtocol, payload))
		conn, err := NewFramedMessageConn(raw, H1FramerProtocol, 16*1024, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, message, err := conn.ReadPooledMessage()
		pooled, _ := MessagePoolCheck(message)
		match := bytes.Equal(message, payload)
		MessagePoolReturn(message)
		conn.Close()
		if err != nil || !match || pooled != (size <= 12288) {
			t.Fatalf("framed size=%d pooled=%t err=%v", size, pooled, err)
		}
	}
	message, err := MessagePoolReadAllLimit(bytes.NewReader(make([]byte, 12289)), 12288)
	if message != nil {
		MessagePoolReturn(message)
	}
	if message != nil || !errors.Is(err, ErrMessageTooLarge) {
		t.Fatal("larger class bypassed the existing complete-message limit")
	}
	readErr := errors.New("synthetic read failure after pooled growth")
	for _, size := range []int{9000, 12 * 1024} {
		message, err = MessagePoolReadAll(io.MultiReader(bytes.NewReader(make([]byte, size)), iotest.ErrReader(readErr)))
		if message != nil {
			MessagePoolReturn(message)
		}
		if message != nil || !errors.Is(err, readErr) {
			t.Fatal("grown reader lost its error/owner disposition")
		}
	}
}

// Actual Pack encoding, complete AEAD wrapping and owned decode exercise all
// three byte-root sites. Eight1280 must retain pooled complete encoded and encrypted owners.
func TestMessagePoolExtendedPackEncryptReadDecodeGeometry(t *testing.T) {
	if messagePoolSnapshotInFreshProcess(t) {
		return
	}
	before := encodingPoolBoundaryNow()
	t.Cleanup(func() { encodingPoolRequireBalanced(t, before) })
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	sequenceCipher := &sequenceCipher{aead: aead}
	for _, packetBytes := range []int{1100, 1280} {
		func() {
			frames, witnesses := transferGroupTestFrames(t, 8, packetBytes)
			defer func() {
				for _, frame := range frames {
					MessagePoolReturn(frame.MessageBytes)
				}
				releaseTransferGroupTestWitnesses(t, frames, witnesses)
			}()
			path := TransferPath{SourceId: NewId(), DestinationId: NewId()}
			contractId := NewId()
			// Maximal metadata is a conservative encoded bound. Full proof
			// payloads remain independently sized and may exceed this class.
			pack := sendPackFrame{path: path, messageId: NewId(), sequenceId: NewId(), sequenceNumber: ^uint64(0), frames: frames,
				head: true, nack: true, tagSendTime: ^uint64(0), contractId: &contractId,
				forceStream: true, companionContract: true, logicalLane: 8, contractAhead: true,
				sessionRole: sequenceTlsRoleServer.toProtobuf(), sessionRoleSet: true, companion: true}
			encoded := marshalSendPackTransferFrame(&pack)
			defer MessagePoolReturn(encoded)
			outer, err := sequenceCipher.SealOuterFrame(path, encoded, sequenceTlsRoleServer.toProtobuf(), true)
			if err != nil {
				t.Fatal(err)
			}
			defer MessagePoolReturn(outer)
			read, err := MessagePoolReadAll(bytes.NewReader(outer))
			if err != nil {
				t.Fatal(err)
			}
			defer MessagePoolReturn(read)
			decodedOuter := &decodedTransferFrame{}
			defer decodedOuter.release()
			if !unmarshalOwnedTransferFrame(read, decodedOuter, true) || len(decodedOuter.frame.EncryptedTransferFrame) == 0 {
				t.Fatal("complete encrypted carrier did not decode")
			}
			plain, err := sequenceCipher.Open(decodedOuter.frame.EncryptedTransferFrame)
			if err != nil {
				t.Fatal(err)
			}
			defer MessagePoolReturn(plain)
			decoded := &decodedTransferFrame{}
			defer decoded.release()
			if !bytes.Equal(plain, encoded) || !unmarshalOwnedTransferFrame(plain, decoded, true) || decoded.frame.Pack == nil || len(decoded.frame.Pack.Frames) != 8 {
				t.Fatal("real decrypt/decode changed the complete eight-frame Pack")
			}
			for index, frame := range decoded.frame.Pack.Frames {
				if !bytes.Equal(frame.MessageBytes, frames[index].MessageBytes) {
					t.Fatal("decoded packet bytes changed")
				}
			}
			wantPooled := true
			for _, owner := range [][]byte{encoded, outer, read, plain} {
				pooled, _ := MessagePoolCheck(owner)
				if pooled != wantPooled || (pooled && (cap(owner) != 12300 || MessagePoolRootByteCount(owner) != 12288)) {
					t.Fatalf("packet=%d encoded=%d outer=%d owner=%d cap=%d pooled=%t", packetBytes, len(encoded), len(outer), len(owner), cap(owner), pooled)
				}
			}
			if packetBytes == 1280 && len(outer) <= 8192 {
				t.Fatal("1280 complete envelope did not exercise the extended class")
			}
			t.Logf("packet_bytes=%d frames=8 encoded_bytes=%d encrypted_bytes=%d root_request_bytes=%d pooled=%t", packetBytes, len(encoded), len(outer), cap(outer), wantPooled)
		}()
	}
}

// Actual append capacity is not ownership. Full-body sentinels make the old
// cap9472 collision mutate visible bytes, without fabricating pool metadata.
func TestMessagePoolExtendedRejectsOrdinaryAppendOwnership(t *testing.T) {
	if messagePoolSnapshotInFreshProcess(t) {
		return
	}
	before := encodingPoolBoundaryNow()
	t.Cleanup(func() { encodingPoolRequireBalanced(t, before) })
	for _, size := range []int{9472, 9000} {
		want := bytes.Repeat([]byte{0x5a}, size)
		ordinary := append([]byte(nil), want...)
		if cap(ordinary) != 9472 {
			t.Fatalf("ordinary append premise changed: size=%d cap=%d", size, cap(ordinary))
		}
		share := MessagePoolShareReadOnly(ordinary)
		pooled, shared := MessagePoolCheck(ordinary)
		returnedShare := MessagePoolReturn(share)
		returnedOwner := MessagePoolReturn(ordinary)
		if pooled || shared || returnedShare || returnedOwner || !bytes.Equal(ordinary, want) || &share[0] != &ordinary[0] {
			t.Fatalf("NONPOOL_APPEND_CAPACITY_COLLISION: size=%d cap=%d pooled=%t shared=%t mutated=%t", size, cap(ordinary), pooled, shared, !bytes.Equal(ordinary, want))
		}
	}
}

// A tail-inclusive twelve-KiB class would collide with ordinary append
// capacity. The chosen payload class preserves a distinct visible capacity.
func TestMessagePoolExtended1280RejectsRoundedAppendOwnership(t *testing.T) {
	if messagePoolSnapshotInFreshProcess(t) {
		return
	}
	before := encodingPoolBoundaryNow()
	t.Cleanup(func() { encodingPoolRequireBalanced(t, before) })
	for _, size := range []int{12288, 12300, 13568} {
		ordinary := append([]byte(nil), bytes.Repeat([]byte{0x5a}, size)...)
		ordinary = ordinary[:cap(ordinary)]
		for index := range ordinary {
			ordinary[index] = 0x5a
		}
		want := append([]byte(nil), ordinary...)
		share := MessagePoolShareReadOnly(ordinary)
		pooled, shared := MessagePoolCheck(ordinary)
		returnedShare := MessagePoolReturn(share)
		returnedOwner := MessagePoolReturn(ordinary)
		if pooled || shared || returnedShare || returnedOwner || !bytes.Equal(ordinary, want) || &share[0] != &ordinary[0] {
			t.Fatalf("EXTENDED1280_APPEND_OWNER_COLLISION: size=%d cap=%d pooled=%t shared=%t", size, cap(ordinary), pooled, shared)
		}
	}
}
