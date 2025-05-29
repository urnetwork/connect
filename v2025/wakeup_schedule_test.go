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
		w := WakeupEpoch * ((time.Duration(now.UnixMilli())*time.Millisecond + WakeupEpoch - 1) / WakeupEpoch)
		assert.Equal(t, time.Duration(WakeupTime(now).UnixMilli())*time.Millisecond, w)
		select {
		case <-time.After(time.Duration(mathrand.Intn(1024)) * time.Millisecond):
		}
	}
}
