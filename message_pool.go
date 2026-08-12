package connect

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/maphash"
	"io"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"maps"

	"google.golang.org/protobuf/proto"
)

// new byte allocations in the connect package use pooled message buffers,
// either via `MessagePoolCopy` or `MessagePoolGet`.
// There are three rules for pooled messages:
// - an owner of a message should return the message to the pool with `MessagePoolReturn`
//   when no longer used.
// - message ownership is handed off on send/channel write.
//   If the caller wants to retain the passed message, it should call `MessagePoolShareReadOnly`
//   before calling send/channel write.
// - messages are valid only for duration of a receive callback.
//   If the receiver wants to keep the message longer, it shoudl call `MessagePoolShareReadOnly`
//   before the callback returns.
// Shared messages are returned to the pool the same as normal messages.
// `MessagePoolReturn`/`MessagePoolShareReadOnly` is a noop when using a `[]byte` that is not part of the pool.

// set this to true to tag messages with useful debugging information e.g. the creation site
const debugTags = false

// [8 byte id][1 byte tag][1 byte flags][2 byte ref count]
const MessagePoolMetaByteCount = 12
const MessagePoolFlagShared = uint8(0x01)

// InitialMessagePoolByteCount is the initial total free-list byte budget,
// split evenly across the pool size classes (see ResizeMessagePools)
var InitialMessagePoolByteCount = mib(2)

const (
	// Four shards cut the measured pool-return convoy while adding only about
	// 54 KiB of fixed per-tag counter state across the three size classes.
	// The count is a power of two because the immutable buffer id carries the
	// shard in its low bits.
	messagePoolShardCount = 4
	messagePoolShardBits  = 2
	messagePoolShardMask  = messagePoolShardCount - 1
)

type messagePoolShard struct {
	stateLock    sync.Mutex
	pool         [][]byte
	count        int
	takenTags    [256]uint64
	returnedTags [256]uint64
	createdTags  [256]uint64
	nextId       uint64
}

type messagePool struct {
	size int

	// Administrative operations are rare and serialize capacity redistribution
	// across shards. Packet-path Get/Return operations never take this lock.
	adminLock sync.Mutex
	shards    [messagePoolShardCount]messagePoolShard
	nextShard atomic.Uint64
}

func newMessagePool(size int, maxCount int) *messagePool {
	mp := &messagePool{size: size}
	mp.Resize(maxCount)
	return mp
}

func (self *messagePool) Resize(maxCount int) {
	self.adminLock.Lock()
	defer self.adminLock.Unlock()

	for shardIndex := range messagePoolShardCount {
		shardCapacity := maxCount / messagePoolShardCount
		if shardIndex < maxCount%messagePoolShardCount {
			shardCapacity += 1
		}
		shard := &self.shards[shardIndex]
		shard.stateLock.Lock()
		newPool := make([][]byte, shardCapacity)
		newCount := copy(newPool, shard.pool[:shard.count])
		shard.pool = newPool
		shard.count = newCount
		shard.stateLock.Unlock()
	}
}

func (self *messagePool) capacity() int {
	capacity := 0
	for shardIndex := range messagePoolShardCount {
		shard := &self.shards[shardIndex]
		shard.stateLock.Lock()
		capacity += len(shard.pool)
		shard.stateLock.Unlock()
	}
	return capacity
}

type messagePoolSnapshot struct {
	capacity     int
	retained     int
	takenTags    [256]uint64
	returnedTags [256]uint64
	createdTags  [256]uint64
}

// snapshot locks shards in index order and copies the whole class in one
// pass. Observability therefore costs four locks per class rather than four
// locks per tag, and capacity/retention/counters describe one consistent
// instant instead of being sampled across packet-path mutations.
func (self *messagePool) snapshot() messagePoolSnapshot {
	for shardIndex := range messagePoolShardCount {
		self.shards[shardIndex].stateLock.Lock()
	}
	defer func() {
		for shardIndex := messagePoolShardCount - 1; 0 <= shardIndex; shardIndex -= 1 {
			self.shards[shardIndex].stateLock.Unlock()
		}
	}()

	var snapshot messagePoolSnapshot
	for shardIndex := range messagePoolShardCount {
		shard := &self.shards[shardIndex]
		snapshot.capacity += len(shard.pool)
		snapshot.retained += shard.count
		for tag := range 256 {
			snapshot.takenTags[tag] += shard.takenTags[tag]
			snapshot.returnedTags[tag] += shard.returnedTags[tag]
			snapshot.createdTags[tag] += shard.createdTags[tag]
		}
	}
	return snapshot
}

func (self *messagePool) resetStats() {
	for shardIndex := range messagePoolShardCount {
		shard := &self.shards[shardIndex]
		shard.stateLock.Lock()
		clear(shard.takenTags[:])
		clear(shard.returnedTags[:])
		clear(shard.createdTags[:])
		shard.stateLock.Unlock()
	}
}

func (self *messagePool) warm(count int) {
	self.adminLock.Lock()
	defer self.adminLock.Unlock()

	count = min(count, self.capacity())
	for shardIndex := range messagePoolShardCount {
		targetCount := count / messagePoolShardCount
		if shardIndex < count%messagePoolShardCount {
			targetCount += 1
		}
		shard := &self.shards[shardIndex]
		shard.stateLock.Lock()
		targetCount = min(targetCount, len(shard.pool))
		for shard.count < targetCount {
			poolMessage := make([]byte, self.size+MessagePoolMetaByteCount)
			shard.nextId += 1
			id := shard.nextId<<messagePoolShardBits | uint64(shardIndex)
			binary.BigEndian.PutUint64(poolMessage[self.size:], id)
			poolMessage[self.size+8] = 255
			shard.pool[shard.count] = poolMessage
			shard.count += 1
		}
		shard.stateLock.Unlock()
	}
}

func (self *messagePool) Clear() {
	self.adminLock.Lock()
	defer self.adminLock.Unlock()

	for shardIndex := range messagePoolShardCount {
		shard := &self.shards[shardIndex]
		shard.stateLock.Lock()
		for i := range shard.count {
			shard.pool[i] = nil
		}
		shard.count = 0
		shard.stateLock.Unlock()
	}
}

func (self *messagePool) shardFor(poolMessage []byte) (*messagePoolShard, uint64) {
	id := binary.BigEndian.Uint64(poolMessage[self.size:])
	return &self.shards[id&messagePoolShardMask], id
}

// take removes and initializes one buffer under one of four bounded shard
// locks. Metadata and stats stay in the same critical section; allocation on
// a miss remains outside it. The payload is intentionally not zeroed.
func (self *messagePool) take(n int, tag uint8) []byte {
	shardIndex := int((self.nextShard.Add(1) - 1) & messagePoolShardMask)
	shard := &self.shards[shardIndex]
	shard.stateLock.Lock()
	if 0 < shard.count {
		poolMessage := shard.pool[shard.count-1]
		shard.pool[shard.count-1] = nil
		shard.count -= 1

		id := binary.BigEndian.Uint64(poolMessage[self.size:])
		count := binary.BigEndian.Uint16(poolMessage[self.size+10:])
		if count != 0 {
			shard.stateLock.Unlock()
			err := fmt.Errorf("message[%d] already taken", id)
			DefaultLogger().Errorf("[mp]%s", ErrorJson(err, debug.Stack()))
			panic(err)
		}
		if poolMessage[self.size+8] == 255 {
			shard.createdTags[tag] += 1
		}
		shard.takenTags[tag] += 1
		poolMessage[self.size+8] = tag
		poolMessage[self.size+9] = 0
		binary.BigEndian.PutUint16(poolMessage[self.size+10:], 1)
		shard.stateLock.Unlock()
		return poolMessage[:n]
	}

	// Reserve the unique id and update stats under the lock, but allocate after
	// unlocking: a pool miss must not convoy unrelated returns behind the
	// runtime allocator or a GC assist.
	shard.nextId += 1
	id := shard.nextId<<messagePoolShardBits | uint64(shardIndex)
	shard.createdTags[tag] += 1
	shard.takenTags[tag] += 1
	shard.stateLock.Unlock()

	poolMessage := make([]byte, self.size+MessagePoolMetaByteCount)
	binary.BigEndian.PutUint64(poolMessage[self.size:], id)
	poolMessage[self.size+8] = tag
	binary.BigEndian.PutUint16(poolMessage[self.size+10:], 1)
	return poolMessage[:n]
}

// release decrements the reference count and, on its final return, inserts the
// buffer into its acquisition shard in the same critical section. The
// immutable id chooses the shard without a global lock.
func (self *messagePool) release(poolMessage []byte) bool {
	shard, id := self.shardFor(poolMessage)
	shard.stateLock.Lock()
	tag := poolMessage[self.size+8]
	count := binary.BigEndian.Uint16(poolMessage[self.size+10:])
	if count == 0 {
		shard.stateLock.Unlock()
		// Double-return: log unconditionally so production sees it, but do the
		// stack capture/log write outside the hot pool lock.
		err := fmt.Errorf("[mp]return message[%d] not taken", id)
		DefaultLogger().Errorf("[mp]%s", ErrorJson(err, debug.Stack()))
		return false
	}
	if 1 < count {
		binary.BigEndian.PutUint16(poolMessage[self.size+10:], count-1)
		shard.stateLock.Unlock()
		return false
	}

	poolMessage[self.size+8] = 0
	poolMessage[self.size+9] = 0
	binary.BigEndian.PutUint16(poolMessage[self.size+10:], 0)
	shard.returnedTags[tag] += 1
	if shard.count < len(shard.pool) {
		// The payload does not need to be zeroed.
		shard.pool[shard.count] = poolMessage
		shard.count += 1
	}
	shard.stateLock.Unlock()
	return true
}

// the packet class, sized to hold a device mtu packet (see `DefaultMtu`)
// plus headers. the first (smallest) size class.
const packetPoolSize = 2048

// free list retention floors preserve a working reuse set on the hot paths
// even under a tiny budget. there is no in-flight cap here; `Get` always
// allocates on an empty list, so a zeroed class costs reuse, not liveness.
const packetPoolFloorCount = 128

// Keep the large-class floor constant in bytes, not buffers. That prevents a
// newly-added larger class from multiplying the minimum retained footprint.
const largeObjectPoolFloorByteCount = 256 * 1024

func largeObjectPoolFloorCount(poolSize int) int {
	return max(1, largeObjectPoolFloorByteCount/poolSize)
}

var orderedMessagePools = sync.OnceValue(func() []*messagePool {
	// the initial byte budget is split evenly across the size classes
	// (rebounded by ResizeMessagePools). 4096 fits a two-packet transfer
	// batch without retaining an 8 KiB buffer for a ~3 KiB message; 8192 is
	// sized so a full `MinimumMessageLenLimit` (4 KiB) message still fits
	// after the pack/transfer frame proto envelope. Larger frames remain
	// unpooled.
	poolSizes := []int{packetPoolSize, 4096, 8192}
	poolByteCount := InitialMessagePoolByteCount / ByteCount(len(poolSizes))
	pools := []*messagePool{}
	for _, poolSize := range poolSizes {
		pools = append(pools, newMessagePool(poolSize, int(poolByteCount/ByteCount(poolSize))))
	}

	go HandleError(func() {
		poolStats(pools)
	})

	return pools
})

func poolStats(pools []*messagePool) {
	// print stats from all pools on a regular interval
	for {
		// Per-tag pool efficiency is developer diagnostics: one line per pool
		// per tag per cycle, which made this the largest single log source in
		// the embedding services. Skip the whole pass unless the operator opts
		// in, so the snapshots, the caller joins, and the debug lock are not
		// paid for either. Embedders that need the aggregate without the
		// volume read `MessagePoolCounts`/`MessagePoolUnpooledCounts`.
		if v := DefaultLogger().V(1); v.Enabled() {
			for _, pool := range pools {
				snapshot := pool.snapshot()
				for tag := range 256 {
					taken := snapshot.takenTags[tag]
					returned := snapshot.returnedTags[tag]
					created := snapshot.createdTags[tag]
					if 0 < taken {
						ratio := float32(returned) / float32(taken)
						reuse := float32(taken-created) / float32(taken)
						var caller string
						func() {
							debugStateLock.Lock()
							defer debugStateLock.Unlock()
							caller = strings.Join(slices.Collect(maps.Keys(tagCallers[uint8(tag)])), "/")
						}()

						v.Infof("pool[%d] tag=%d [%s] r=%d/t=%d/c=%d = %.2f%% return / %.2f%% reuse\n", pool.size, tag, caller, returned, taken, created, 100*ratio, 100*reuse)
					}
				}
			}

			if taken, byteCount := MessagePoolUnpooledCounts(); 0 < taken {
				v.Infof("pool[unpooled] t=%d bytes=%d\n", taken, byteCount)
			}
		}

		select {
		case <-time.After(60 * time.Second):
		}
	}
}

// ResizeMessagePools bounds the free lists. With two arguments, the packet
// class (`packetPoolSize`) retains at most `packetByteCount`, and the large
// object classes (all the others, serving protocol frames) split the second
// byte count evenly. The one-argument form preserves the historical API and
// gives every size class that byte cap.
//
// Buffers in use are unaffected; this only bounds what the free lists retain,
// with per-class floors that preserve a working reuse set. Safe to call at
// runtime.
func ResizeMessagePools(packetByteCount ByteCount, largeObjectByteCounts ...ByteCount) {
	pools := orderedMessagePools()
	largeObjectPoolCount := 0
	for _, pool := range pools {
		if pool.size != packetPoolSize {
			largeObjectPoolCount += 1
		}
	}
	largeObjectByteCount := packetByteCount * ByteCount(largeObjectPoolCount)
	if 0 < len(largeObjectByteCounts) {
		largeObjectByteCount = largeObjectByteCounts[0]
	}
	for _, pool := range pools {
		if pool.size == packetPoolSize {
			pool.Resize(max(packetPoolFloorCount, int(packetByteCount/ByteCount(pool.size))))
		} else {
			poolByteCount := largeObjectByteCount / ByteCount(largeObjectPoolCount)
			pool.Resize(max(largeObjectPoolFloorCount(pool.size), int(poolByteCount/ByteCount(pool.size))))
		}
	}
}

// WarmMessagePools pre-allocates up to 1 MiB of the PACKET class's free-list,
// capped at a quarter of the configured class capacity. This is enough to
// absorb the measured first-burst allocation storm without making warm
// retention grow linearly with a host's (potentially much larger) pool cap.
// Only the packet (2048) class is warmed — the large object (protocol frame)
// classes are not on the same cold-start hot path, so warming them would only
// inflate early retention for no first-burst benefit. Call once at process
// start after sizing the pools; do not call from memory-pressure paths —
// re-warming immediately after a shed fights the host's request for memory.
func WarmMessagePools() {
	for _, pool := range orderedMessagePools() {
		if pool.size == packetPoolSize {
			const maxWarmByteCount = 1024 * 1024
			pool.warm(min(pool.capacity()/4, maxWarmByteCount/pool.size))
		}
	}
}

func ClearMessagePools() {
	for _, pool := range orderedMessagePools() {
		pool.Clear()
	}
	// ACK-lifetime metadata is also a bounded, recoverable packet-path pool.
	// Drop it under the same host memory-pressure signal; it repopulates
	// lazily and never affects in-flight items.
	clearSendItemPool()
}

var seed = maphash.MakeSeed()
var debugStateLock sync.Mutex
var tagCallers = map[uint8]map[string]bool{}

func debugTag() uint8 {
	_, file2, line2, ok := runtime.Caller(2)
	if !ok {
		return 0
	}
	_, file3, line3, ok := runtime.Caller(3)
	if !ok {
		return 0
	}
	caller := fmt.Sprintf("%s:%d->%s:%d", file3, line3, file2, line2)
	tag := uint8(maphash.String(seed, caller))
	func() {
		debugStateLock.Lock()
		defer debugStateLock.Unlock()

		callers, ok := tagCallers[tag]
		if !ok {
			callers = map[string]bool{}
			tagCallers[tag] = callers
		}
		callers[caller] = true
	}()
	return tag
}

func ResetMessagePoolStats() {
	for _, pool := range orderedMessagePools() {
		pool.resetStats()
	}
	unpooledTakenCount.Store(0)
	unpooledTakenByteCount.Store(0)
}

// MessagePoolCounts returns the cumulative taken/returned/created message counts summed
// across all pools and tags. taken-returned is the number of pool messages currently held
// by consumers: it returns to a stable baseline when every taken buffer is eventually
// returned, so growth across a load-then-teardown cycle attributes a lost return (a buffer
// leak) even though the heap does not move (the GC quietly collects lost buffers). This is
// always tracked (independent of debugTags), so tests can assert pool balance in any build.
func MessagePoolCounts() (taken uint64, returned uint64, created uint64) {
	for _, pool := range orderedMessagePools() {
		snapshot := pool.snapshot()
		for tag := range 256 {
			taken += snapshot.takenTags[tag]
			returned += snapshot.returnedTags[tag]
			created += snapshot.createdTags[tag]
		}
	}
	return
}

// MessagePoolClassStats is a point-in-time view of one size class. Capacity
// and Retained count free-list buffers; Taken/Returned/Created are cumulative
// since process start or ResetMessagePoolStats.
type MessagePoolClassStats struct {
	Size     int
	Capacity int
	Retained int
	Taken    uint64
	Returned uint64
	Created  uint64
}

// GetMessagePoolClassStats returns one snapshot per ordered size class.
func GetMessagePoolClassStats() []*MessagePoolClassStats {
	var stats []*MessagePoolClassStats
	for _, pool := range orderedMessagePools() {
		snapshot := pool.snapshot()
		classStats := &MessagePoolClassStats{
			Size:     pool.size,
			Capacity: snapshot.capacity,
			Retained: snapshot.retained,
		}
		for tag := range 256 {
			classStats.Taken += snapshot.takenTags[tag]
			classStats.Returned += snapshot.returnedTags[tag]
			classStats.Created += snapshot.createdTags[tag]
		}
		stats = append(stats, classStats)
	}
	return stats
}

func MessagePoolStats() map[int]map[int]float32 {
	sizeTagRatios := map[int]map[int]float32{}
	for _, pool := range orderedMessagePools() {
		snapshot := pool.snapshot()
		tagRatios := map[int]float32{}
		for tag := range 256 {
			taken := snapshot.takenTags[tag]
			returned := snapshot.returnedTags[tag]

			if 0 < taken {
				ratio := float32(returned) / float32(taken)
				tagRatios[tag] = ratio
			}
		}
		sizeTagRatios[pool.size] = tagRatios
	}
	return sizeTagRatios
}

/*
func MessagePool(targetSize int) (*messagePool, int) {
	for _, pool := range orderedMessagePools {
		if targetSize <= pool.size {
			return pool, pool.size
		}
	}
	// return the largest
	pool := orderedMessagePools[len(orderedMessagePools)-1]
	return pool, pool.size
}
*/

func MessagePoolReadAll(r io.Reader) ([]byte, error) {
	return MessagePoolReadAllWithTag(r, 0)
}

func MessagePoolReadAllWithTag(r io.Reader, tag uint8) ([]byte, error) {
	orderedMessagePools := orderedMessagePools()

	b, _ := MessagePoolGetDetailedWithTag(orderedMessagePools[0].size, tag)
	i := 0
	for j := 0; j < len(orderedMessagePools); j += 1 {
		for i < len(b) {
			n, err := r.Read(b[i:])
			if n > 0 {
				i += n
			}
			if err != nil {
				if err == io.EOF {
					return b[:i], nil
				}
				MessagePoolReturn(b)
				return nil, err
			}
			if n == 0 {
				return b[:i], nil
			}
		}

		if len(orderedMessagePools) <= j+1 {
			break
		}

		b2, _ := MessagePoolGetDetailedWithTag(orderedMessagePools[j+1].size, tag)
		copy(b2, b)
		MessagePoolReturn(b)
		b = b2
	}

	out := make([]byte, i, 2*i)
	copy(out, b)
	defer MessagePoolReturn(b)
	for {
		n, err := r.Read(b)
		if n > 0 {
			out = append(out, b[:n]...)
		}
		if err != nil {
			if err == io.EOF {
				return out, nil
			}
			// Preserve the historical contract that (non-EOF) errors yield a nil buffer
			// (callers do not expect to MessagePoolReturn on the error path).
			// We still consumed the bytes (preventing reader desync on streams).
			return nil, err
		}
		if n == 0 {
			return out, nil
		}
	}
}

func MessagePoolCopy(message []byte) []byte {
	b, _ := MessagePoolCopyDetailed(message)
	return b
}

func MessagePoolCopyDetailed(message []byte) ([]byte, bool) {
	var tag uint8
	if debugTags {
		tag = debugTag()
	}
	return MessagePoolCopyDetailedWithTag(message, tag)
}

func MessagePoolCopyDetailedWithTag(message []byte, tag uint8) ([]byte, bool) {
	poolMessage, pooled := MessagePoolGetDetailedWithTag(len(message), tag)
	copy(poolMessage, message)
	return poolMessage, pooled
}

func MessagePoolGet(n int) []byte {
	b, _ := MessagePoolGetDetailed(n)
	return b
}

func MessagePoolGetDetailed(n int) ([]byte, bool) {
	var tag uint8
	if debugTags {
		tag = debugTag()
	}
	return MessagePoolGetDetailedWithTag(n, tag)
}

func MessagePoolGetDetailedWithTag(n int, tag uint8) ([]byte, bool) {
	orderedMessagePools := orderedMessagePools()

	for _, pool := range orderedMessagePools {
		if n <= pool.size {
			return pool.take(n, tag), true
		}
	}
	// allocate a new message
	unpooledTakenCount.Add(1)
	unpooledTakenByteCount.Add(uint64(n))
	poolMessage := make([]byte, n+MessagePoolMetaByteCount)
	return poolMessage[:n], false
}

// allocations larger than every size class are never pooled. tracked so the
// stats show whether the classes are absorbing the message traffic.
var unpooledTakenCount atomic.Uint64
var unpooledTakenByteCount atomic.Uint64

// MessagePoolUnpooledCounts returns the cumulative count and total bytes of
// allocations that exceeded every pool size class
func MessagePoolUnpooledCounts() (taken uint64, byteCount uint64) {
	return unpooledTakenCount.Load(), unpooledTakenByteCount.Load()
}

func MessagePoolReturn(message []byte) bool {
	orderedMessagePools := orderedMessagePools()

	c := cap(message)
	for _, pool := range orderedMessagePools {
		if c == pool.size+MessagePoolMetaByteCount {
			return pool.release(message[:c])
		}
	}
	// else drop the message, let it gc
	return false
}

func MessagePoolShareReadOnly(message []byte) []byte {
	orderedMessagePools := orderedMessagePools()

	c := cap(message)
	for _, pool := range orderedMessagePools {
		if c == pool.size+MessagePoolMetaByteCount {
			poolMessage := message[:c]
			shard, id := pool.shardFor(poolMessage)

			func() {
				shard.stateLock.Lock()
				defer shard.stateLock.Unlock()

				count := binary.BigEndian.Uint16(poolMessage[pool.size+10:])
				if count == 0 {
					DefaultLogger().Warningf("[mp]share message[%d] not taken", id)
				} else {
					binary.BigEndian.PutUint16(poolMessage[pool.size+10:], count+1)
					poolMessage[pool.size+9] |= MessagePoolFlagShared
				}
			}()

			return message
		}
	}
	// not a pool message
	return message
}

func MessagePoolCheck(message []byte) (pooled bool, shared bool) {
	orderedMessagePools := orderedMessagePools()

	c := cap(message)
	for _, pool := range orderedMessagePools {
		if c == pool.size+MessagePoolMetaByteCount {
			poolMessage := message[:c]
			shard, _ := pool.shardFor(poolMessage)

			func() {
				shard.stateLock.Lock()
				defer shard.stateLock.Unlock()

				count := binary.BigEndian.Uint16(poolMessage[pool.size+10:])
				if 0 < count {
					pooled = true
					shared = poolMessage[pool.size+9]&MessagePoolFlagShared != 0
				}
			}()

			return
		}
	}
	// not a pool message
	return
}

func ProtoMarshal(m proto.Message) ([]byte, error) {
	var tag uint8
	if debugTags {
		tag = debugTag()
	}
	return ProtoMarshalWithTag(m, tag)
}

func ProtoMarshalWithTag(m proto.Message, tag uint8) ([]byte, error) {
	if m == nil {
		return nil, nil
	}

	buf, _ := MessagePoolGetDetailedWithTag(proto.Size(m), tag)

	out, err := proto.MarshalOptions{}.MarshalAppend(buf[:0], m)
	if err != nil {
		MessagePoolReturn(buf)
		return nil, err
	}
	// if proto.Size underestimated, MarshalAppend may have allocated a fresh
	// slice; the pool buffer is then orphaned and must be returned to balance
	// the Get above. detected by a cap change (append only grows cap).
	if cap(out) != cap(buf) {
		MessagePoolReturn(buf)
	}
	return out, nil
}

func ProtoUnmarshal(b []byte, m proto.Message) error {
	return proto.Unmarshal(b, m)
}

func EncodeBase64(enc *base64.Encoding, src []byte) string {
	buf := MessagePoolGet(enc.EncodedLen(len(src)))
	defer MessagePoolReturn(buf)
	enc.Encode(buf, src)
	return string(buf)
}

func DecodeBase64(enc *base64.Encoding, s string) ([]byte, error) {
	sbuf := MessagePoolGet(len(s))
	defer MessagePoolReturn(sbuf)
	copy(sbuf, s)
	buf := MessagePoolGet(enc.DecodedLen(len(s)))
	n, err := enc.Decode(buf, sbuf)
	if err != nil {
		MessagePoolReturn(buf)
		return nil, err
	}
	return buf[:n], nil
}
