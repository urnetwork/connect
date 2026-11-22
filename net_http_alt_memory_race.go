package connect

import (
	"context"
	"errors"
	"net"
	"time"
)

// Each alt candidate owns a complete independent carrier claim. If memory
// permits only one, keep the next address queued until a claim frees. Alt h3
// ends the preceding dial at the family/port stagger and joins its teardown
// before trying that address. Alt whodis keeps the preceding dial while it
// hears from alt, and tries the next address once that dial fails or stalls
// (quic_dial_race.go). A local refusal is not evidence that a healthy
// fallback is unreachable.
func raceAltQuicDial(
	ctx context.Context,
	candidates []*net.UDPAddr,
	policy extenderQuicMemoryPolicy,
	schedule quicDialRaceSchedule,
	dial func(context.Context, *net.UDPAddr, *platformTransportBudgetReservation) (*h3DialAttempt, error),
) (*h3DialAttempt, error) {
	return raceAltQuicDialProgressive(ctx, candidates, nil, policy, schedule, dial)
}

// The first usable family starts immediately. Later results join the same
// budgeted race; exhaustion requires both attempts and resolution to finish.
func raceAltQuicDialProgressive(
	ctx context.Context,
	candidates []*net.UDPAddr,
	source *udpDialCandidateSource,
	policy extenderQuicMemoryPolicy,
	schedule quicDialRaceSchedule,
	dial func(context.Context, *net.UDPAddr, *platformTransportBudgetReservation) (*h3DialAttempt, error),
) (*h3DialAttempt, error) {
	defer source.close()
	resolution := source.resultChannel()
	if len(candidates) == 0 {
		return nil, errors.New("alt QUIC race: no candidates")
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
	launch := func() error {
		claim, err := policy.acquire(ctx)
		if err != nil {
			return err
		}
		attemptCtx, index := attempts.launch(ctx)
		address := candidates[launched]
		launched++
		pending++
		go func() {
			attempt, err := dial(attemptCtx, address, claim)
			results <- result{index: index, attempt: attempt, claim: claim, err: err}
		}()
		return nil
	}
	finish := func() {
		attempts.cancelAll()
		for pending > 0 {
			loser := <-results
			pending--
			loser.attempt.close()
			loser.claim.Release()
		}
	}
	defer finish()
	// Armed while a whodis candidate the budget refused waits for an attempt
	// out to stall.
	stall := time.NewTimer(0)
	stall.Stop()
	defer stall.Stop()
	armStall := func(stallTime time.Time) {
		stall.Stop()
		if !stallTime.IsZero() {
			stall.Reset(time.Until(stallTime))
		}
	}
	advance := func() error {
		err := launch()
		if errors.Is(err, errExtenderMemoryBudget) && pending > 0 {
			if schedule.whodis {
				// an attempt hearing from alt keeps its claim; one that has
				// stalled gives it up, and the candidate launches on it
				armStall(attempts.cancelStalled(time.Now()))
			} else {
				attempts.cancelAll()
			}
			return nil // the candidate index advances only after admission
		}
		stall.Stop()
		return err
	}
	if err := advance(); err != nil {
		return nil, err
	}
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
				// The returned attempt retains its claim. Deferred finish joins
				// every loser before the caller begins its application graph.
				return completed.attempt, nil
			}
			completed.attempt.close()
			completed.claim.Release()
			if firstErr == nil && !errors.Is(completed.err, context.Canceled) {
				firstErr = completed.err
			}
			if launched < len(candidates) {
				if err := advance(); err != nil {
					return nil, err
				}
				staggerReady = false
				stagger.Reset(schedule.stagger)
			} else if pending == 0 && resolution == nil {
				if firstErr == nil {
					firstErr = context.Canceled
				}
				return nil, firstErr
			}
		case <-stagger.C:
			staggerReady = true
			if launched < len(candidates) {
				if err := advance(); err != nil {
					return nil, err
				}
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
				if err := advance(); err != nil {
					return nil, err
				}
				staggerReady = false
				stagger.Reset(schedule.stagger)
			} else if pending == 0 && resolution == nil {
				if firstErr == nil {
					firstErr = context.Canceled
				}
				return nil, firstErr
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
