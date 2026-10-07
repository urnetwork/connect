// Deterministic ownership regressions for the camouflage dial race. Barriers
// force completed losers to outlive a terminal result without real network I/O.
package connect

import (
	"context"
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Blocks cleanup of one failed attempt so the other attempt can finish while
// the race is processing its terminal answer.
type camouflageLifecycleCloseBarrierConn struct {
	net.Conn
	entered chan struct{}
	release chan struct{}
}

// Holds result processing until the other attempt has returned its connection.
func (self *camouflageLifecycleCloseBarrierConn) Close() error {
	close(self.entered)
	<-self.release
	return nil
}

// The production race owns all successful connections, including an answer
// already buffered when a different attempt terminates the race.
func TestExtenderTcpCamouflageRaceClosesQueuedSuccessfulLoser(t *testing.T) {
	for _, terminalErr := range []error{
		&ExtenderLimitedError{RetryAfter: time.Second},
		&ExtenderRefusedError{StatusCode: 403},
	} {
		synctest.Test(t, func(t *testing.T) {
			stagger := make(chan time.Time)
			close(stagger)
			legacyEntered := make(chan struct{})
			failedConn := &camouflageLifecycleCloseBarrierConn{
				entered: make(chan struct{}),
				release: make(chan struct{}),
			}
			loserConn := &closeOnlyConn{}
			done := make(chan error, 1)
			go func() {
				_, _, err := raceExtenderTcpCamouflage(
					context.Background(), nil, func() <-chan time.Time { return stagger },
					func(ctx context.Context, roundTrip *ExtenderRoundTrip) (net.Conn, *protocol.ExtenderResponse, error) {
						<-legacyEntered
						return failedConn, nil, terminalErr
					},
					func(ctx context.Context, roundTrip *ExtenderRoundTrip) (net.Conn, *protocol.ExtenderResponse, error) {
						close(legacyEntered)
						<-failedConn.entered
						return loserConn, &protocol.ExtenderResponse{}, nil
					},
				)
				done <- err
			}()
			<-failedConn.entered
			// With the race blocked in Close, Wait proves the legacy dial has
			// completed before cancellation. Its result must still have an owner.
			synctest.Wait()
			close(failedConn.release)
			if err := <-done; !errors.Is(err, terminalErr) {
				t.Fatalf("race error = %v, want %v", err, terminalErr)
			}
			if !loserConn.closed.Load() {
				t.Fatal("the race returned with its completed losing connection still open")
			}
		})
	}
}

// A successful result transfers only the winner. A canceled loser returning
// either a successful connection or a connection with an error is still joined.
func TestExtenderTcpCamouflageRaceTransfersOnlyWinner(t *testing.T) {
	for _, loserErr := range []error{nil, context.Canceled} {
		stagger := make(chan time.Time)
		close(stagger)
		legacyEntered := make(chan struct{})
		winnerConn := &closeOnlyConn{}
		loserConn := &closeOnlyConn{}
		conn, _, err := raceExtenderTcpCamouflage(
			context.Background(), nil, func() <-chan time.Time { return stagger },
			func(ctx context.Context, roundTrip *ExtenderRoundTrip) (net.Conn, *protocol.ExtenderResponse, error) {
				<-legacyEntered
				return winnerConn, &protocol.ExtenderResponse{}, nil
			},
			func(ctx context.Context, roundTrip *ExtenderRoundTrip) (net.Conn, *protocol.ExtenderResponse, error) {
				close(legacyEntered)
				<-ctx.Done()
				return loserConn, &protocol.ExtenderResponse{}, loserErr
			},
		)
		if err != nil || conn != winnerConn {
			t.Fatalf("race = %v, %v, want the camouflaged winner", conn, err)
		}
		if winnerConn.closed.Load() {
			t.Fatal("the returned winner was closed by race cleanup")
		}
		winnerConn.Close()
		if !loserConn.closed.Load() {
			t.Fatalf("loser returning error %v remained open after the race returned", loserErr)
		}
	}
}

// Parent cancellation transfers no connection, even when a pending dial returns
// a connection as it observes cancellation.
func TestExtenderTcpCamouflageRaceCancellationClosesPendingConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dialEntered := make(chan struct{})
	loserConn := &closeOnlyConn{}
	done := make(chan error, 1)
	go func() {
		_, _, err := raceExtenderTcpCamouflage(
			ctx, nil, neverStagger,
			func(ctx context.Context, roundTrip *ExtenderRoundTrip) (net.Conn, *protocol.ExtenderResponse, error) {
				close(dialEntered)
				<-ctx.Done()
				return loserConn, nil, ctx.Err()
			},
			func(ctx context.Context, roundTrip *ExtenderRoundTrip) (net.Conn, *protocol.ExtenderResponse, error) {
				return nil, nil, ctx.Err()
			},
		)
		done <- err
	}()
	<-dialEntered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("race error = %v, want parent cancellation", err)
	}
	if !loserConn.closed.Load() {
		t.Fatal("a canceled dial returned a connection with no owner")
	}
}
