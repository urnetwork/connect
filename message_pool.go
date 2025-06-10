//go:build !mininit
// +build !mininit

package connect

import (
	"encoding/binary"
	"fmt"
	"io"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/exp/maps"

	"github.com/golang/glog"
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

// [8 byte id][1 byte tag][1 byte flags][2 byte ref count]
const MessagePoolMetaByteCount = 12
const MessagePoolFlagShared = uint8(0x01)

var nextId atomic.Uint64

type messagePool struct {
	size         int
	pool         *sync.Pool
	takenTags    [256]atomic.Uint64
	returnedTags [256]atomic.Uint64
	createdTags  [256]atomic.Uint64
	stateLock    sync.Mutex
}

func newMessagePool(size int) *messagePool {
	mp := &messagePool{
		size: size,
		pool: &sync.Pool{
			New: func() any {
				poolMessage := make([]byte, size+MessagePoolMetaByteCount)
				binary.BigEndian.PutUint64(poolMessage[size:], nextId.Add(1))
				poolMessage[size+8] = 255
				return poolMessage
			},
		},
	}
	return mp
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
					var caller string
					func() {
						debugStateLock.Lock()
						defer debugStateLock.Unlock()
						caller = strings.Join(maps.Keys(debugTagCallers[uint8(tag)]), "/")
					}()

					glog.Infof("pool[%d] tag=%d [%s] r=%d/t=%d/c=%d = %.2f%% return / %.2f%% reuse\n", pool.size, tag, caller, returned, taken, created, 100*ratio, 100*reuse)
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

func MessagePoolReadAllWithTag(r io.Reader, tag uint8) ([]byte, error) {
	b, _ := MessagePoolGetDetailedWithTag(orderedMessagePools[0].size, tag)
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
		if n == 0 {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, b[:n]...)
	}
}

func MessagePoolGetDetailedWithTag(n int, tag uint8) ([]byte, bool) {
	for _, pool := range orderedMessagePools {
		if n <= pool.size {
			poolMessage := pool.pool.Get().([]byte)
			if poolMessage[pool.size+8] == 255 {
				pool.createdTags[tag].Add(1)
			}
			poolMessage[pool.size+8] = tag
			pool.takenTags[tag].Add(1)
			id := binary.BigEndian.Uint64(poolMessage[pool.size:])

			func() {
				pool.stateLock.Lock()
				defer pool.stateLock.Unlock()

				count := binary.BigEndian.Uint16(poolMessage[pool.size+10:])

				if count != 0 {
					err := fmt.Errorf("message[%d] already taken", id)
					glog.Errorf("[mp]%s", ErrorJson(err, debug.Stack()))
					panic(err)
				} else {
					binary.BigEndian.PutUint16(poolMessage[pool.size+10:], 1)
				}
			}()

			return poolMessage[:n], true
		}
	}
	// allocate a new message
	poolMessage := make([]byte, n+MessagePoolMetaByteCount)
	return poolMessage[:n], false
}

func MessagePoolReturn(message []byte) bool {
	c := cap(message)
	for _, pool := range orderedMessagePools {
		if c == pool.size+MessagePoolMetaByteCount {
			poolMessage := message[:c]
			id := binary.BigEndian.Uint64(poolMessage[pool.size:])

			r := false
			func() {
				pool.stateLock.Lock()
				defer pool.stateLock.Unlock()

				count := binary.BigEndian.Uint16(poolMessage[pool.size+10:])
				if count == 0 {
					if debugTags {
						err := fmt.Errorf("[mp]return message[%d] not taken", id)
						glog.Errorf("[mp]%s", ErrorJson(err, debug.Stack()))
					}
				} else if count == 1 {
					r = true
				} else {
					binary.BigEndian.PutUint16(poolMessage[pool.size+10:], count-1)
				}
			}()

			if r {
				tag := poolMessage[pool.size+8]
				poolMessage[pool.size+8] = 0
				poolMessage[pool.size+9] = 0
				binary.BigEndian.PutUint16(poolMessage[pool.size+10:], 0)
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
			id := binary.BigEndian.Uint64(poolMessage[pool.size:])

			func() {
				pool.stateLock.Lock()
				defer pool.stateLock.Unlock()

				count := binary.BigEndian.Uint16(poolMessage[pool.size+10:])
				if count == 0 {
					glog.Warningf("[mp]share message[%d] not taken", id)
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
	c := cap(message)
	for _, pool := range orderedMessagePools {
		if c == pool.size+MessagePoolMetaByteCount {
			poolMessage := message[:c]

			func() {
				pool.stateLock.Lock()
				defer pool.stateLock.Unlock()

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
