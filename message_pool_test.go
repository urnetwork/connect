package connect

import (
	"bytes"
	"encoding/base64"
	"fmt"
	mathrand "math/rand"
	"testing"
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
			AssertEqual(t, len(messageCopy), n)
			AssertEqual(t, message, messageCopy)

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
		AssertEqual(t, err, nil)
		AssertEqual(t, b, bCopy)
		MessagePoolReturn(bCopy)
	}
	stats := MessagePoolStats()
	for _, tagRatios := range stats {
		for _, ratio := range tagRatios {
			AssertEqual(t, ratio, float32(1.0))
		}
	}
}

func TestMessagePoolShare(t *testing.T) {
	holdCount := 16
	holdMessages := make([][][]byte, holdCount)

	for range 1024 {
		message := MessagePoolGet(mathrand.Intn(4096))
		pooled, shared := MessagePoolCheck(message)
		AssertEqual(t, pooled, true)
		AssertEqual(t, shared, false)
		holdMessages[0] = append(holdMessages[0], message)
		k := mathrand.Intn(holdCount)
		for i := 1; i < k; i += 1 {
			MessagePoolShareReadOnly(message)
			pooled, shared = MessagePoolCheck(message)
			AssertEqual(t, pooled, true)
			AssertEqual(t, shared, true)
			holdMessages[i] = append(holdMessages[i], message)
		}
	}

	// add large messages that will not be shared
	for range 1024 {
		message := MessagePoolGet(mathrand.Intn(32 * 1024))
		pooled, shared := MessagePoolCheck(message)
		AssertEqual(t, pooled, len(message) <= 4096)
		AssertEqual(t, shared, false)
		k := mathrand.Intn(holdCount)
		for i := 1; i < k; i += 1 {
			MessagePoolShareReadOnly(message)
			pooled, shared = MessagePoolCheck(message)
			AssertEqual(t, pooled, len(message) <= 4096)
			AssertEqual(t, shared, len(message) <= 4096)
		}
		for i := 1; i < k; i += 1 {
			MessagePoolReturn(message)
			pooled, shared = MessagePoolCheck(message)
			AssertEqual(t, pooled, len(message) <= 4096)
			AssertEqual(t, shared, len(message) <= 4096)
		}
		MessagePoolReturn(message)
		pooled, shared = MessagePoolCheck(message)
		AssertEqual(t, pooled, false)
		AssertEqual(t, shared, false)
	}

	for i := holdCount - 1; 1 <= i; i -= 1 {
		for _, message := range holdMessages[i] {
			pooled, shared := MessagePoolCheck(message)
			AssertEqual(t, pooled, len(message) <= 4096)
			AssertEqual(t, shared, len(message) <= 4096)
			r := MessagePoolReturn(message)
			AssertEqual(t, r, false)
		}
	}
	for _, message := range holdMessages[0] {
		r := MessagePoolReturn(message)
		AssertEqual(t, r, true)
		pooled, shared := MessagePoolCheck(message)
		AssertEqual(t, pooled, false)
		AssertEqual(t, shared, false)
	}
}

func TestBase64(t *testing.T) {
	for range 128 {
		n := mathrand.Intn(512)
		b := make([]byte, n)
		mathrand.Read(b)
		b2, err := DecodeBase64(base64.StdEncoding, EncodeBase64(base64.StdEncoding, b))
		AssertEqual(t, err, nil)
		AssertEqual(t, b, b2)
	}
}
