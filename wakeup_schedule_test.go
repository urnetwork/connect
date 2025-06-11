package connect

import (
	mathrand "math/rand"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
)

func TestWakeupTime(t *testing.T) {
	for range 32 {
		now := time.Now()
		w := WakeupEpoch * ((time.Duration(now.UnixNano())*time.Nanosecond + WakeupEpoch - 1) / WakeupEpoch)
		assert.Equal(t, time.Duration(WakeupTime(now).UnixMilli())*time.Millisecond, w)
		select {
		case <-time.After(time.Duration(mathrand.Intn(1000)) * time.Millisecond):
		}
	}
}
