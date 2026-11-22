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
// Direct H3 ends the preceding attempt at the stagger for it. A dns carrier
// (a whodis race) keeps the preceding attempt while it hears from alt, and
// starts the candidate once that attempt fails or stalls (quic_dial_race.go).
func raceH3DialWithMemory(
	ctx context.Context,
	candidates []*net.UDPAddr,
	budget *PlatformTransportBudget,
	extraByteCount ByteCount,
	nestedByteCount ByteCount,
	schedule quicDialRaceSchedule,
	dial func(context.Context, *net.UDPAddr) (*h3DialAttempt, error),
) (*h3DialAttempt, error) {
	return raceH3DialWithMemoryProgressive(ctx, candidates, nil, budget, extraByteCount, nestedByteCount, schedule, dial)
}

// Pending DNS is another source of candidates, not an admission bypass. A
// refused candidate still waits for complete prior teardown before launching.
func raceH3DialWithMemoryProgressive(
	ctx context.Context,
	candidates []*net.UDPAddr,
	source *udpDialCandidateSource,
	budget *PlatformTransportBudget,
	extraByteCount ByteCount,
	nestedByteCount ByteCount,
	schedule quicDialRaceSchedule,
	dial func(context.Context, *net.UDPAddr) (*h3DialAttempt, error),
) (*h3DialAttempt, error) {
	defer source.close()
	resolution := source.resultChannel()
	if len(candidates) == 0 {
		return nil, errors.New("h3 race: no candidates")
	}
	type result struct {
		index   int
		attempt *h3DialAttempt
		claim   *platformTransportBudgetReservation
		err     error
	}
	results := make(chan result, len(candidates))
	attempts := &quicDialRaceAttempts{schedule: schedule}
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
		attemptCtx, index := attempts.launch(dialCtx)
		address := candidates[launched]
		launched++
		pending++
		go func() {
			attempt, err := dial(attemptCtx, address)
			results <- result{index: index, attempt: attempt, claim: claim, err: err}
		}()
		return true
	}
	finish := func() {
		attempts.cancelAll()
		// Join cleanup before the winner may allocate its steady application
		// graph or the caller may return the base carrier reservation.
		for pending > 0 {
			loser := <-results
			pending--
			loser.attempt.close()
			loser.claim.Release()
		}
	}
	// Armed while a dns carrier candidate the budget refused waits for an
	// attempt out to stall.
	stall := time.NewTimer(0)
	stall.Stop()
	defer stall.Stop()
	armStall := func(stallTime time.Time) {
		stall.Stop()
		if !stallTime.IsZero() {
			stall.Reset(time.Until(stallTime))
		}
	}
	advance := func() {
		if launch() {
			stall.Stop()
			return
		}
		if schedule.whodis {
			// A whodis attempt hearing from alt keeps the socket; one that has
			// stalled gives it up, and the deferred candidate launches from its
			// fully closed result.
			armStall(attempts.cancelStalled(time.Now()))
			return
		}
		// There is no admitted speculative socket. End the current attempt
		// at this family/port fallback boundary, then launch the deferred
		// candidate from its fully closed result using the base lease. Merely
		// disabling the stagger would strand a healthy fallback behind a
		// blackholed first endpoint until the long QUIC handshake timeout.
		attempts.cancelAll()
	}
	launch()
	stagger := time.NewTimer(schedule.stagger)
	defer stagger.Stop()
	var firstErr error
	staggerReady := false
	for {
		select {
		case completed := <-results:
			pending--
			attempts.ended(completed.index)
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
				staggerReady = false
				stagger.Reset(schedule.stagger)
			} else if pending == 0 && resolution == nil {
				finish()
				return nil, firstErr
			}
		case <-stagger.C:
			staggerReady = true
			if launched < len(candidates) {
				advance()
				staggerReady = false
				stagger.Reset(schedule.stagger)
			}
		case <-stall.C:
			if launched < len(candidates) {
				armStall(attempts.cancelStalled(time.Now()))
			}
		case resolved := <-resolution:
			candidates = source.appendReady(candidates, resolved)
			resolution = source.resultChannel()
			if launched < len(candidates) && (pending == 0 || staggerReady) {
				advance()
				staggerReady = false
				stagger.Reset(schedule.stagger)
			} else if pending == 0 && resolution == nil {
				finish()
				return nil, firstErr
			}
		case <-ctx.Done():
			finish()
			return nil, ctx.Err()
		}
	}
}
