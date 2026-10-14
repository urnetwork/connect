// Completion checks keep a lossy quic exchange alive until both receivers have
// validated their payloads. Synctest fixes the close/read ordering without timeouts.
package connect

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
)

// A successful Write can leave queued and unacknowledged response frames.
// The connection owner must retain them until the peer finishes its read.
func TestPacketTranslationExchangeWaitsForPeerRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		writeComplete := make(chan struct{})
		peerReadComplete := make(chan struct{})
		close(writeComplete)
		connectionClosed := make(chan error, 1)
		go func() {
			connectionClosed <- waitForPacketTranslationTestExchange(context.Background(), writeComplete, peerReadComplete)
		}()
		synctest.Wait()
		select {
		case err := <-connectionClosed:
			t.Fatalf("connection cleanup allowed before the peer completed its read: %v", err)
		default:
		}

		close(peerReadComplete)
		synctest.Wait()
		select {
		case err := <-connectionClosed:
			if err != nil {
				t.Fatalf("completed exchange: %v", err)
			}
		default:
			t.Fatal("connection cleanup stayed blocked after both directions completed")
		}
	})
}

// Peer receipt does not transfer ownership of a writer still using the stream.
func TestPacketTranslationExchangeWaitsForLocalWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		writeComplete := make(chan struct{})
		peerReadComplete := make(chan struct{})
		close(peerReadComplete)
		connectionClosed := make(chan error, 1)
		go func() {
			connectionClosed <- waitForPacketTranslationTestExchange(context.Background(), writeComplete, peerReadComplete)
		}()
		synctest.Wait()
		select {
		case err := <-connectionClosed:
			t.Fatalf("connection cleanup allowed before the writer completed: %v", err)
		default:
		}

		close(writeComplete)
		synctest.Wait()
		select {
		case err := <-connectionClosed:
			if err != nil {
				t.Fatalf("completed exchange: %v", err)
			}
		default:
			t.Fatal("connection cleanup stayed blocked after both directions completed")
		}
	})
}

// A failed attempt must still release ownership if the peer cannot finish.
func TestPacketTranslationExchangeCancellationWakesPeerRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		writeComplete := make(chan struct{})
		peerReadComplete := make(chan struct{})
		close(writeComplete)
		connectionClosed := make(chan error, 1)
		go func() {
			connectionClosed <- waitForPacketTranslationTestExchange(ctx, writeComplete, peerReadComplete)
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case err := <-connectionClosed:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled exchange = %v, want context cancellation", err)
			}
		default:
			t.Fatal("connection cleanup stayed blocked after cancellation")
		}
	})
}

// Cancellation also releases a writer that cannot finish after a failed read.
func TestPacketTranslationExchangeCancellationWakesLocalWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		writeComplete := make(chan struct{})
		peerReadComplete := make(chan struct{})
		close(peerReadComplete)
		connectionClosed := make(chan error, 1)
		go func() {
			connectionClosed <- waitForPacketTranslationTestExchange(ctx, writeComplete, peerReadComplete)
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case err := <-connectionClosed:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled exchange = %v, want context cancellation", err)
			}
		default:
			t.Fatal("connection cleanup stayed blocked after cancellation")
		}
	})
}

// Ready completion channels cannot turn an already failed attempt into success.
func TestPacketTranslationExchangeAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	writeComplete := make(chan struct{})
	peerReadComplete := make(chan struct{})
	close(writeComplete)
	close(peerReadComplete)
	if err := waitForPacketTranslationTestExchange(ctx, writeComplete, peerReadComplete); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled exchange = %v, want context cancellation", err)
	}
}
