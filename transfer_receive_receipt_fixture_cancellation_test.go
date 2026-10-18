// Private, uncompiled/unrun fixture control: ancestor cancellation is not
// proof that a held TCP worker's own context is canceled.
package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

// Holds the supported AfterFunc propagation callback, not Err or Done.
// Value hides the parent's private cancelCtx key so WithCancel uses AfterFunc.
type receiptFixtureDelayedCancelContext struct {
	context.Context
	entered       chan struct{}
	release       chan struct{}
	enteredOnce   sync.Once
	callbacks     sync.WaitGroup
	registrations atomic.Int64
}

// No application values are needed by this synthetic fixture context.
func (self *receiptFixtureDelayedCancelContext) Value(any) any {
	return nil
}

// Owns each registered callback until it either finishes or is stopped.
// Registrations finish during construction, before the test joins callbacks.
func (self *receiptFixtureDelayedCancelContext) AfterFunc(cancelChild func()) func() bool {
	self.registrations.Add(1)
	self.callbacks.Add(1)
	finish := sync.OnceFunc(self.callbacks.Done)
	stop := context.AfterFunc(self.Context, func() {
		defer finish()
		self.enteredOnce.Do(func() { close(self.entered) })
		<-self.release
		cancelChild()
	})
	return func() bool {
		stopped := stop()
		if stopped {
			finish()
		}
		return stopped
	}
}

// Reuses the actual registered fixture hook in a second constructor-owned NAT.
// The old hook starts Run with a queued SYN while child cancellation is held;
// the corrected hook cannot start until that exact TCP context is canceled.
func TestReceiveReceiptFixtureWaitsForOwnTcpCancellation(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReceiptAdmissionFixtureWithProvider(t, 1, false)
		// The base owner is not the challenged owner; release its exact SYN
		// normally so only the second NAT can trip the uncanceled dial guard.
		f.releaseTcp(f.firstTcp, 41000, nil)

		delayed := &receiptFixtureDelayedCancelContext{
			Context: f.ctx,
			entered: make(chan struct{}),
			release: make(chan struct{}),
		}
		release := sync.OnceFunc(func() { close(delayed.release) })
		runEntered := make(chan struct{})
		settings := DefaultLocalUserNatSettingsWithBufferSize(1)
		settings.Log = NewNoopLogger()
		settings.TcpBufferSettings.WriteTimeout = 0
		settings.TcpBufferSettings.beforeSequenceRunWithStateForTest =
			f.nat.settings.TcpBufferSettings.beforeSequenceRunWithStateForTest
		settings.TcpBufferSettings.beforeSequenceRunForTest = func() { close(runEntered) }
		settings.TcpBufferSettings.DialContextSettings = f.nat.settings.TcpBufferSettings.DialContextSettings
		nat := NewLocalUserNat(delayed, "synthetic receipt cancellation ordering", settings)
		var tcp *TcpSequence
		join := sync.OnceFunc(func() {
			f.cancel()
			release()
			if err := nat.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			delayed.callbacks.Wait()
			if tcp != nil {
				select {
				case <-tcp.retirementDone:
				default:
					t.Error("second NAT returned before its actual TCP owner retired")
				}
				if tcp.ctx.Err() != context.Canceled || len(tcp.sendItems) != 0 {
					t.Error("joined TCP owner retained a live context or queued SYN")
				}
			}
		})
		t.Cleanup(join)

		packet := MessagePoolCopy(receiptAdmissionPacket(41001, tcpFlagSyn, 100, nil))
		f.witnesses = append(f.witnesses, MessagePoolShareReadOnly(packet))
		if !nat.SendPacket(f.source, protocol.ProvideMode_Network, packet, 0) {
			MessagePoolReturn(packet)
			t.Fatal("second real NAT refused the synthetic SYN before the fixture hook")
		}
		synctest.Wait()
		tcp = f.takeTcp()
		if delayed.registrations.Load() != 1 || nat.ctx.Err() != nil || tcp.ctx.Err() != nil ||
			cap(tcp.sendItems) != 1 || len(tcp.sendItems) != 1 {
			t.Fatal("actual constructor did not establish a live, held one-SYN TCP owner")
		}
		select {
		case <-runEntered:
			t.Fatal("fixture did not hold the actual TCP worker before Run")
		default:
		}

		f.cancel()
		<-delayed.entered
		synctest.Wait()
		if f.ctx.Err() != context.Canceled || nat.ctx.Err() != nil {
			t.Fatal("parent cancellation was not separated from actual child propagation")
		}
		select {
		case <-runEntered:
			t.Error("fixture released TCP worker before its own cancellation")
		default:
		}
		if f.dials.Load() != 0 {
			t.Error("fixture allowed a live-context dial while child cancellation was held")
		}
		if len(tcp.sendItems) != 1 {
			t.Error("fixture consumed its queued SYN before child cancellation")
		}

		join()
		select {
		case <-runEntered:
		default:
			t.Error("fixture did not release Run after its own TCP cancellation")
		}
		if f.dials.Load() != 0 {
			t.Error("joined fixture attempted a live upstream dial")
		}
	})
}
