package mp

import (
	"sync"
	"sync/atomic"
	"time"
	// "fmt"

	"github.com/golang/glog"
)

type messagePool struct {
	size         int
	pool         sync.Pool
	takenTags    [256]atomic.Uint64
	returnedTags [256]atomic.Uint64
}

func newMessagePool(size int) *messagePool {
	return &messagePool{
		size: size,
		pool: sync.Pool{
			New: func() any {
				return make([]byte, size+1)
			},
		},
	}
}

var orderedMessagePools = []*messagePool{
	newMessagePool(2048),
	newMessagePool(4096),
}

func poolStats() {
	// print stats from all pools on a regular interval
	for {
		for _, pool := range orderedMessagePools {
			for tag := range 256 {
				taken := pool.takenTags[tag].Load()
				if 0 < taken {
					returned := pool.returnedTags[tag].Load()
					ratio := float32(returned) / float32(taken)

					glog.Infof("pool[%d] tag=%d r=%d/t=%d = %.2f\n", pool.size, tag, taken, returned, 100*ratio)
				}
			}
		}

		select {
		case <-time.After(60 * time.Second):
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

func init() {
	go poolStats()
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
			poolMessage[pool.size] = tag
			pool.takenTags[tag].Add(1)
			return poolMessage[:n]
		}
	}
	// allocate a new message
	poolMessage := make([]byte, n+1)
	poolMessage[n] = tag
	return poolMessage[:n]
}

func MessagePoolReturn(message []byte) {
	c := cap(message)
	for _, pool := range orderedMessagePools {
		if c == pool.size+1 {
			poolMessage := message[:c]
			pool.pool.Put(poolMessage)
			tag := poolMessage[pool.size]
			pool.returnedTags[tag].Add(1)
			return
		}
	}
	// else drop the message, let it gc
}
