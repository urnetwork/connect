// A payload in flight across its contract's deadline, across the real sender,
// receiver, retry and renewal paths.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// A payload written in its contract's last moments must not depend on beating
// the deadline across the wire. The receiver checks the deadline by its own
// clock on arrival and refuses a contract past it for good, so a payload in
// flight across the deadline was refused on every resend until the sender's
// ACK lifetime ended the sequence (measured: a quiet tunnel died exactly one
// hour after its contract was made). Inside the active contract's expiration
// margin the payload takes a fresh contract instead, and lands.
func TestSignedContractExpirationPayloadHeldAcrossTheDeadlineLands(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newExpirationLifecycleFixture(t, false)
		defer f.close()
		f.finishSend(f.startSend("opening"))
		sender, _ := f.sequences()
		active := sender.sendContract
		if delivered, refused := expirationHoldAcrossTheDeadline(t, f); !delivered || refused {
			t.Fatal("a payload held across the deadline was refused, though it was written inside the margin")
		}
		if sender.sendContract == active {
			t.Fatal("the payload inside the margin was written under the retiring contract")
		}
	})
}

// The same payload without the margin: written under the active contract, it is
// refused once the deadline passes in flight. This is the failure the margin
// exists for, held here so the fixture keeps reproducing it.
func TestSignedContractExpirationPayloadHeldAcrossTheDeadlineWithoutMarginIsRefused(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newExpirationLifecycleFixture(t, false, func(settings *ClientSettings) {
			settings.SendBufferSettings.ContractExpirationMargin = 0
		})
		defer f.close()
		f.finishSend(f.startSend("opening"))
		if delivered, refused := expirationHoldAcrossTheDeadline(t, f); delivered || !refused {
			t.Fatal("without the margin the payload held across the deadline was not refused")
		}
	})
}

// Sends "held" a tenth of a second before the fixture's deadline, inside a
// one-second grant's margin, holds it on the wire past the deadline, then
// releases it and allows ten seconds of resends. Renewals from inside the
// margin get a fresh hour, as the platform grants every contract.
func expirationHoldAcrossTheDeadline(t *testing.T, f *expirationLifecycleFixture) (delivered bool, refused bool) {
	t.Helper()
	f.authority.renewalsFrom.Store(f.deadline.Add(-200 * time.Millisecond).UnixNano())
	time.Sleep(time.Until(f.deadline) - 100*time.Millisecond)
	synctest.Wait()
	barrier := &expirationLifecycleWireBarrier{
		content: "held", entered: make(chan struct{}), release: make(chan struct{}),
	}
	f.forwardBarrier.Store(barrier)
	ack := f.startSend("held")
	select {
	case <-barrier.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the held payload never reached the wire")
	}
	time.Sleep(200 * time.Millisecond)
	if time.Now().Before(f.deadline) {
		t.Fatal("the payload was released before the deadline")
	}
	f.forwardBarrier.Store(nil)
	close(barrier.release)
	select {
	case err := <-ack:
		if err != nil {
			t.Fatal("the held payload failed", err)
		}
		synctest.Wait()
		delivered = true
	case <-time.After(10 * time.Second):
		refused = true
	}
	f.stateLock.Lock()
	defer f.stateLock.Unlock()
	if delivered && f.deliveries["held"] != 1 {
		t.Fatal("the held payload was acknowledged without being delivered exactly once")
	}
	if refused && f.deliveries["held"] != 0 {
		t.Fatal("the held payload was delivered but never acknowledged")
	}
	return delivered, refused
}
