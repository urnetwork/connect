package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// A completed ACK round must immediately release admission. A polling tick
// spends a substantial fraction of a short path idle after capacity is free.
func TestResendCapacityReleaseWakesAdmissionImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sequence := &SendSequence{ctx: ctx}
		sequence.resendCapacityUnavailable.Store(true)
		done := make(chan bool, 1)
		go func() {
			admitted, err, _ := sequence.awaitResendCapacity(&SendPack{Ctx: ctx}, time.Second)
			done <- admitted && err == nil
		}()
		synctest.Wait()
		sequence.resendCapacityUnavailable.Store(false)
		synctest.Wait()
		select {
		case admitted := <-done:
			if !admitted {
				t.Fatal("released capacity was refused")
			}
		default:
			t.Error("free capacity still waits for the 2 ms admission poll")
		}
	})
}
