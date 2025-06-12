package connect

import (
	mathrand "math/rand"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
)

func TestWakeupTime(t *testing.T) {
	wakeupEpoch := 1 * time.Second

	for range 32 {
		now := time.Now()
		w := wakeupEpoch * ((time.Duration(now.UnixNano())*time.Nanosecond + wakeupEpoch - 1) / wakeupEpoch)
		assert.Equal(t, time.Duration(WakeupTime(now, wakeupEpoch).UnixMilli())*time.Millisecond, w)
		select {
		case <-time.After(time.Duration(mathrand.Intn(1000)) * time.Millisecond):
		}
	}
}
