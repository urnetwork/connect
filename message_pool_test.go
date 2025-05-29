package connect

import (
	"bytes"
	"fmt"
	mathrand "math/rand"
	"testing"

	"github.com/go-playground/assert/v2"
)

func TestMessagePool(t *testing.T) {
	ResetMessagePoolStats()
	for n := range 1024 * 8 {
		if n%32 == 0 {
			fmt.Printf("mem[%d]\n", n)
		}
		for range 128 {
			message := make([]byte, n)
			mathrand.Read(message)

			messageCopy := MessagePoolCopy(message)
			assert.Equal(t, len(messageCopy), n)
			assert.Equal(t, message, messageCopy)

			MessagePoolReturn(messageCopy)
		}
	}
	for n := range 1024 * 32 {
		if n%32 == 0 {
			fmt.Printf("memr[%d]\n", n)
		}
		b := make([]byte, mathrand.Intn(32*1024))
		mathrand.Read(b)
		bCopy, err := MessagePoolReadAll(bytes.NewReader(b))
		assert.Equal(t, err, nil)
		assert.Equal(t, b, bCopy)
		MessagePoolReturn(bCopy)
	}
	stats := MessagePoolStats()
	for _, tagRatios := range stats {
		for _, ratio := range tagRatios {
			assert.Equal(t, ratio, float32(1.0))
		}
	}
}

func TestMessagePoolShare(t *testing.T) {
	holdCount := 16
	holdMessages := make([][][]byte, holdCount)

	for range 1024 {
		message := MessagePoolGet(mathrand.Intn(4096))
		pooled, shared := MessagePoolCheck(message)
		assert.Equal(t, pooled, true)
		assert.Equal(t, shared, false)
		holdMessages[0] = append(holdMessages[0], message)
		k := mathrand.Intn(holdCount)
		for i := 1; i < k; i += 1 {
			MessagePoolShareReadOnly(message)
			pooled, shared = MessagePoolCheck(message)
			assert.Equal(t, pooled, true)
			assert.Equal(t, shared, true)
			holdMessages[i] = append(holdMessages[i], message)
		}
	}

	// add large messages that will not be shared
	for range 1024 {
		message := MessagePoolGet(mathrand.Intn(32 * 1024))
		pooled, shared := MessagePoolCheck(message)
		assert.Equal(t, pooled, len(message) <= 4096)
		assert.Equal(t, shared, false)
		k := mathrand.Intn(holdCount)
		for i := 1; i < k; i += 1 {
			MessagePoolShareReadOnly(message)
			pooled, shared = MessagePoolCheck(message)
			assert.Equal(t, pooled, len(message) <= 4096)
			assert.Equal(t, shared, len(message) <= 4096)
		}
		for i := 1; i < k; i += 1 {
			MessagePoolReturn(message)
			pooled, shared = MessagePoolCheck(message)
			assert.Equal(t, pooled, len(message) <= 4096)
			assert.Equal(t, shared, len(message) <= 4096)
		}
		MessagePoolReturn(message)
		pooled, shared = MessagePoolCheck(message)
		assert.Equal(t, pooled, false)
		assert.Equal(t, shared, false)
	}

	for i := holdCount - 1; 0 <= i; i -= 1 {
		for _, message := range holdMessages[i] {
			pooled, shared := MessagePoolCheck(message)
			assert.Equal(t, pooled, len(message) <= 4096)
			assert.Equal(t, shared, len(message) <= 4096)
			r := MessagePoolReturn(message)
			assert.Equal(t, r, i == 0)
		}
	}
	for _, message := range holdMessages[0] {
		pooled, shared := MessagePoolCheck(message)
		assert.Equal(t, pooled, false)
		assert.Equal(t, shared, false)
	}
}
