// Tcp shutdown publishes cancellation before waking condition waiters, so an
// idle close joins its workers even when socket cleanup cannot cancel first.
package connect

import (
	"context"
	"net"
	"testing"
	"testing/synctest"
	"time"
)

// Holds socket cleanup after Run wakes its condition waiters. The upload is
// blocked in net.Pipe.Write, so closing its queue cannot cancel the flow first.
type tcpShutdownCloseTestConn struct {
	net.Conn
	closeStarted chan struct{}
	allowClose   chan struct{}
}

// Makes the ordering observable before any socket worker can exit on close.
func (self *tcpShutdownCloseTestConn) Close() error {
	close(self.closeStarted)
	<-self.allowClose
	return self.Conn.Close()
}

// A condition wake while the context is live lets the ack worker wait again.
// Holding socket close forces that worker to consume the wake before later
// cleanup can cancel, reproducing the idle-reap deadlock without scheduler luck.
func TestTcpFlowIdleCloseCancelsBeforeConditionWake(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultTcpBufferSettingsWithBufferSize(16)
		settings.ReadTimeout = time.Hour
		settings.WriteTimeout = time.Hour
		closeStarted := make(chan struct{})
		allowClose := make(chan struct{})
		ackWorkerStopped := make(chan struct{})
		f := newTcpReapTestFlowWithSetup(t, settings, func(sequence *TcpSequence) {
			dial := settings.DialContextSettings.DialContext
			settings.DialContextSettings.DialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
				conn, err := dial(ctx, network, address)
				if err != nil {
					return nil, err
				}
				return &tcpShutdownCloseTestConn{
					Conn:         conn,
					closeStarted: closeStarted,
					allowClose:   allowClose,
				}, nil
			}
			sequence.afterPureAckWorkerStopForTest = func() { close(ackWorkerStopped) }
		})
		f.send(f.newSendItem([]byte("pending upload")))
		synctest.Wait()

		time.Sleep(settings.IdleTimeout)
		<-closeStarted
		synctest.Wait()
		if f.sequence.ctx.Err() == nil {
			t.Error("idle close woke condition waiters before publishing cancellation")
		}
		select {
		case <-ackWorkerStopped:
		default:
			t.Error("idle close left the ack worker waiting after its terminal wake")
		}
		close(allowClose)
		synctest.Wait()
		if !f.done() {
			t.Fatal("idle close did not join its condition waiters after socket cleanup")
		}
	})
}

// Explicit cancellation also wakes the receive condition waiting for a fin's
// acknowledgement and the idle ack condition; neither needs another packet.
func TestTcpFlowCancelWakesFinAndAckConditionWaiters(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultTcpBufferSettingsWithBufferSize(16)
		ackWorkerStopped := make(chan struct{})
		f := newTcpReapTestFlowWithSetup(t, settings, func(sequence *TcpSequence) {
			sequence.afterPureAckWorkerStopForTest = func() { close(ackWorkerStopped) }
		})
		f.upstream.Close()
		synctest.Wait()
		f.sequence.mutex.Lock()
		pendingFin := f.sequence.returnByteCount != 0
		f.sequence.mutex.Unlock()
		if !pendingFin {
			t.Fatal("the upstream fin did not enter the receive condition wait")
		}
		f.sequence.Cancel()
		synctest.Wait()
		select {
		case <-ackWorkerStopped:
		default:
			t.Error("cancellation left the ack condition waiting")
		}
		if !f.done() {
			t.Fatal("cancellation did not join the fin and ack condition waiters")
		}
	})
}
