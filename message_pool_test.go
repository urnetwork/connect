package mp

import (
	"fmt"
	mathrand "math/rand"
	"testing"

	"github.com/go-playground/assert/v2"
)

func TestMessagePool(t *testing.T) {

	for n := range 1024 * 8 {
		if n%32 == 0 {
			fmt.Printf("mem[%d]\n", n)
		}
		for range 1024 {
			message := make([]byte, n)
			mathrand.Read(message)

			messageCopy := MessagePoolCopy(message)
			assert.Equal(t, len(messageCopy), n)
			assert.Equal(t, message, messageCopy)

			MessagePoolReturn(messageCopy)
		}
	}
	stats := MessagePoolStats()
	for _, tagRatios := range stats {
		for _, ratio := range tagRatios {
			assert.Equal(t, ratio, float32(1.0))
		}
	}
}
