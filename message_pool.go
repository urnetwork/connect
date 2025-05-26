package connect

import (
	"sync"
	"sync/atomic"
	"time"
	// "fmt"
	"io"

	"github.com/golang/glog"
)

type messagePool struct {
	size         int
	pool         *sync.Pool
	takenTags    [256]atomic.Uint64
	returnedTags [256]atomic.Uint64
}

func newMessagePool(size int) *messagePool {
	return &messagePool{
		size: size,
		pool: &sync.Pool{
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
					ratio := float32(returned) / float32(taken)

					glog.Infof("pool[%d] tag=%d r=%d/t=%d = %.2f%%\n", pool.size, tag, returned, taken, 100*ratio)
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
	for range len(orderedMessagePools) {
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

		b2 := MessagePoolGetWithTag(i+1, tag)
		copy(b2, b)
		MessagePoolReturn(b)
		b = b2
	}

	out := make([]byte, i, 2*i)
	copy(out, b)
	for {
		n, err := r.Read(b)
		if n == 0 {
			MessagePoolReturn(b)
			return out, nil
		}
		if err != nil {
			MessagePoolReturn(b)
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
			tag := poolMessage[pool.size]
			pool.pool.Put(poolMessage)
			pool.returnedTags[tag].Add(1)
			return
		}
	}
	// else drop the message, let it gc
}
