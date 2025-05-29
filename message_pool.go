package connect

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
	// "runtime/debug"

	"google.golang.org/protobuf/proto"

	"github.com/golang/glog"
)

// [8 byte id][1 byte tag][1 byte flags]
const MessagePoolMetaByteCount = 10
const MessagePoolFlagShared = uint8(0x01)

var nextId atomic.Uint64

type messagePool struct {
	size         int
	pool         *sync.Pool
	takenTags    [256]atomic.Uint64
	returnedTags [256]atomic.Uint64
	createdTags  [256]atomic.Uint64

	stateLock sync.Mutex
	counts    map[uint64]int
}

func newMessagePool(size int) *messagePool {
	return &messagePool{
		size: size,
		pool: &sync.Pool{
			New: func() any {
				poolMessage := make([]byte, size+MessagePoolMetaByteCount)
				binary.BigEndian.PutUint64(poolMessage[size:], nextId.Add(1))
				poolMessage[size+8] = 255
				return poolMessage
			},
		},
		counts: map[uint64]int{},
	}
}

var orderedMessagePools = []*messagePool{
	newMessagePool(2048),
	newMessagePool(4096),
}

func init() {
	go poolStats()
}

func poolStats() {
	// print stats from all pools on a regular interval
	for {
		for _, pool := range orderedMessagePools {
			for tag := range 256 {
				taken := pool.takenTags[tag].Load()
				if 0 < taken {
					returned := pool.returnedTags[tag].Load()
					created := pool.createdTags[tag].Load()
					ratio := float32(returned) / float32(taken)
					reuse := float32(taken-created) / float32(taken)

					glog.Infof("pool[%d] tag=%d r=%d/t=%d/c=%d = %.2f%% return / %.2f%% reuse\n", pool.size, tag, returned, taken, created, 100*ratio, 100*reuse)
				}
			}
		}

		select {
		case <-time.After(60 * time.Second):
		}
	}
}

func ResetMessagePoolStats() {
	for _, pool := range orderedMessagePools {
		for tag := range 256 {
			pool.takenTags[tag].Store(0)
			pool.returnedTags[tag].Store(0)
			pool.createdTags[tag].Store(0)
		}
	}
}

func MessagePoolStats() map[int]map[int]float32 {
	sizeTagRatios := map[int]map[int]float32{}
	for _, pool := range orderedMessagePools {
		tagRatios := map[int]float32{}
		for tag := range 256 {
			taken := pool.takenTags[tag].Load()
			if 0 < taken {
				returned := pool.returnedTags[tag].Load()
				ratio := float32(returned) / float32(taken)
				tagRatios[tag] = ratio
			}
		}
		sizeTagRatios[pool.size] = tagRatios
	}
	return sizeTagRatios
}

func MessagePool(targetSize int) (*sync.Pool, int) {
	for _, pool := range orderedMessagePools {
		if targetSize <= pool.size {
			return pool.pool, pool.size
		}
	}
	// return the largest
	pool := orderedMessagePools[len(orderedMessagePools)-1]
	return pool.pool, pool.size
}

func MessagePoolReadAll(r io.Reader) ([]byte, error) {
	return MessagePoolReadAllWithTag(r, 0)
}

func MessagePoolReadAllWithTag(r io.Reader, tag uint8) ([]byte, error) {
	b := MessagePoolGetWithTag(orderedMessagePools[0].size, tag)
	i := 0
	for j := 0; j < len(orderedMessagePools); j += 1 {
		for i < len(b) {
			n, err := r.Read(b[i:])
			if n == 0 {
				return b[:i], nil
			}
			if err != nil {
				MessagePoolReturn(b)
				return nil, err
			}
			i += n
		}

		if len(orderedMessagePools) <= j+1 {
			break
		}

		b2 := MessagePoolGetWithTag(orderedMessagePools[j+1].size, tag)
		copy(b2, b)
		MessagePoolReturn(b)
		b = b2
	}

	out := make([]byte, i, 2*i)
	copy(out, b)
	defer MessagePoolReturn(b)
	for {
		n, err := r.Read(b)
		if n == 0 {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, b[:n]...)
	}
}

func MessagePoolCopy(message []byte) []byte {
	return MessagePoolCopyWithTag(message, 0)
}

func MessagePoolCopyWithTag(message []byte, tag uint8) []byte {
	poolMessage := MessagePoolGetWithTag(len(message), tag)
	copy(poolMessage, message)
	return poolMessage
}

func MessagePoolGet(n int) []byte {
	return MessagePoolGetWithTag(n, 0)
}

func MessagePoolGetWithTag(n int, tag uint8) []byte {
	for _, pool := range orderedMessagePools {
		if n <= pool.size {
			poolMessage := pool.pool.Get().([]byte)
			if poolMessage[pool.size+8] == 255 {
				pool.createdTags[tag].Add(1)
			}
			poolMessage[pool.size+8] = tag
			pool.takenTags[tag].Add(1)

			func() {
				pool.stateLock.Lock()
				defer pool.stateLock.Unlock()

				id := binary.BigEndian.Uint64(poolMessage[pool.size:])

				c := pool.counts[id]
				if c != 0 {
					panic(fmt.Errorf("message[%d] already taken", id))
				} else {
					pool.counts[id] = 1
				}
			}()

			return poolMessage[:n]
		}
	}
	// allocate a new message
	poolMessage := make([]byte, n+MessagePoolMetaByteCount)
	return poolMessage[:n]
}

func MessagePoolReturn(message []byte) bool {
	c := cap(message)
	for _, pool := range orderedMessagePools {
		if c == pool.size+MessagePoolMetaByteCount {
			poolMessage := message[:c]

			r := false
			func() {
				pool.stateLock.Lock()
				defer pool.stateLock.Unlock()

				id := binary.BigEndian.Uint64(poolMessage[pool.size:])

				c := pool.counts[id]
				if c == 0 {
					glog.Warningf("[mp]return message[%d] not taken", id)
				} else if c == 1 {
					delete(pool.counts, id)
					r = true
				} else {
					pool.counts[id] = c - 1
				}
			}()

			if r {
				tag := poolMessage[pool.size+8]
				poolMessage[pool.size+8] = 0
				poolMessage[pool.size+9] = 0
				pool.pool.Put(poolMessage)
				pool.returnedTags[tag].Add(1)
				return true
			}
			return false
		}
	}
	// else drop the message, let it gc
	return false
}

func MessagePoolShareReadOnly(message []byte) []byte {
	c := cap(message)
	for _, pool := range orderedMessagePools {
		if c == pool.size+MessagePoolMetaByteCount {
			poolMessage := message[:c]

			func() {
				pool.stateLock.Lock()
				defer pool.stateLock.Unlock()

				id := binary.BigEndian.Uint64(poolMessage[pool.size:])

				c := pool.counts[id]
				if c == 0 {
					glog.Warningf("[mp]share message[%d] not taken", id)
				} else {
					pool.counts[id] = c + 1
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
	c := cap(message)
	for _, pool := range orderedMessagePools {
		if c == pool.size+MessagePoolMetaByteCount {
			poolMessage := message[:c]

			func() {
				pool.stateLock.Lock()
				defer pool.stateLock.Unlock()

				id := binary.BigEndian.Uint64(poolMessage[pool.size:])

				c := pool.counts[id]
				if 0 < c {
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

/*
// returns if the optional bit is set
func MessagePoolOptionalReturn(message []byte) {
	c := cap(message)
	for _, pool := range orderedMessagePools {
		if c == pool.size+MessagePoolMetaByteCount {
			poolMessage := message[:c]
			if poolMessage[pool.size+9]&MessagePoolFlagOptionalReturn != 0 {
				if DebugMessagePool {
					func() {
						pool.debugStateLock.Lock()
						defer pool.debugStateLock.Unlock()

						id := binary.BigEndian.Uint64(poolMessage[pool.size:])

						// fmt.Printf("RETURN[%d]\n", id)
						// debug.PrintStack()

						if pool.debugInventory[id] {
							panic(fmt.Errorf("message[%d] already returned", id))
						}
						pool.debugInventory[id] = true
					}()
				}
				tag := poolMessage[pool.size+8]
				poolMessage[pool.size+8] = 0
				poolMessage[pool.size+9] = 0
				pool.pool.Put(poolMessage)
				pool.returnedTags[tag].Add(1)
				return
			}
		}
	}
	// else do nothing
}

func MessagePoolSetOptionalReturn(message []byte, optionalReturn bool) {
	c := cap(message)
	for _, pool := range orderedMessagePools {
		if c == pool.size+MessagePoolMetaByteCount {
			poolMessage := message[:c]
			poolMessage[pool.size+9] |= MessagePoolFlagOptionalReturn
			return
		}
	}
}
*/

func ProtoMarshal(m proto.Message) ([]byte, error) {
	return ProtoMarshalWithTag(m, 0)
}

func ProtoMarshalWithTag(m proto.Message, tag uint8) ([]byte, error) {
	if m == nil {
		return nil, nil
	}

	buf := MessagePoolGetWithTag(proto.Size(m), tag)

	out, err := proto.MarshalOptions{}.MarshalAppend(buf[:0], m)
	if err != nil {
		MessagePoolReturn(buf)
		return nil, err
	}
	return out, nil
}

func ProtoUnmarshal(b []byte, m proto.Message) error {
	return proto.Unmarshal(b, m)
}
