package connect

import (
	"context"
	"errors"
	"net"
	"time"
)

// A second mobile dial cannot reuse the live candidate's socket/QUIC claim.
// Before a winner is selected no application stream is opened or read, so
// connection receive credit cannot auto-grow beyond its initial 256 KiB.
// The extra 1408-KiB lease covers that credit + 512 KiB TLS/QUIC/control/tracker
// state + two 64-KiB sockets + 352 KiB for negotiated DATAGRAM receive queues,
// with 160 KiB slack. Unsized carriers retain their larger base working set.
// Include any prepaid nested graph in the same speculative claim so a port
// fallback cannot open QUIC and then lose admission for its DNS translation.
//
// Preserve Happy Eyeballs when the extra lease fits. When it does not, retain
// the candidate and start it after the preceding owner has completely closed;
// a refused speculative dial must not permanently hide a working fallback.
func raceH3DialWithMemory(
	ctx context.Context,
	candidates []*net.UDPAddr,
	budget *PlatformTransportBudget,
	extraByteCount ByteCount,
	nestedByteCount ByteCount,
	dial func(context.Context, *net.UDPAddr) (*h3DialAttempt, error),
) (*h3DialAttempt, error) {
	if len(candidates) == 0 {
		return nil, errors.New("h3 race: no candidates")
	}
	type result struct {
		attempt *h3DialAttempt
		claim   *platformTransportBudgetReservation
		err     error
	}
	results := make(chan result, len(candidates))
	cancels := make([]context.CancelFunc, 0, len(candidates))
	launched, pending := 0, 0
	launch := func() bool {
		var claim *platformTransportBudgetReservation
		dialCtx := ctx
		if pending != 0 {
			var err error
			claim, err = (extenderQuicMemoryPolicy{budget: budget, byteCount: extraByteCount + nestedByteCount}).acquire(ctx)
			if err != nil {
				return false
			}
			if 0 < nestedByteCount {
				dialCtx = context.WithValue(dialCtx, platformTransportNestedBudgetContextKey{}, NewPlatformTransportBudget(nestedByteCount, 0))
			}
		}
		attemptCtx, cancel := context.WithCancel(dialCtx)
		cancels = append(cancels, cancel)
		address := candidates[launched]
		launched++
		pending++
		go func() {
			attempt, err := dial(attemptCtx, address)
			results <- result{attempt, claim, err}
		}()
		return true
	}
	finish := func() {
		for _, cancel := range cancels {
			cancel()
		}
		// Join cleanup before the winner may allocate its steady application
		// graph or the caller may return the base carrier reservation.
		for pending > 0 {
			loser := <-results
			pending--
			loser.attempt.close()
			loser.claim.Release()
		}
	}
	advance := func() {
		if launch() {
			return
		}
		// There is no admitted speculative socket. End the current attempt
		// at this family/port fallback boundary, then launch the deferred
		// candidate from its fully closed result using the base lease. Merely
		// disabling the stagger would strand a healthy fallback behind a
		// blackholed first endpoint until the long QUIC handshake timeout.
		for _, cancel := range cancels {
			cancel()
		}
	}
	launch()
	stagger := time.NewTimer(platformH3FamilyRaceStagger)
	defer stagger.Stop()
	var firstErr error
	for {
		select {
		case completed := <-results:
			pending--
			if completed.err == nil {
				finish()
				completed.claim.Release()
				return completed.attempt, nil
			}
			completed.attempt.close()
			completed.claim.Release()
			if firstErr == nil {
				firstErr = completed.err
			}
			if launched < len(candidates) {
				advance()
				stagger.Reset(platformH3FamilyRaceStagger)
			} else if pending == 0 {
				finish()
				return nil, firstErr
			}
		case <-stagger.C:
			if launched < len(candidates) {
				advance()
				stagger.Reset(platformH3FamilyRaceStagger)
			}
		case <-ctx.Done():
			finish()
			return nil, ctx.Err()
		}
	}
}
