package connect

import (
	"context"
	"errors"
	"net"
	"time"
)

// Each alt candidate owns a complete independent carrier claim. If memory
// permits only one, keep the next address queued, cancel the preceding dial
// at the family/port stagger, and join its teardown before trying that address.
// A local refusal is not evidence that a healthy fallback is unreachable.
func raceAltQuicDial(
	ctx context.Context,
	candidates []*net.UDPAddr,
	policy extenderQuicMemoryPolicy,
	dial func(context.Context, *net.UDPAddr, *platformTransportBudgetReservation) (*h3DialAttempt, error),
) (*h3DialAttempt, error) {
	if len(candidates) == 0 {
		return nil, errors.New("alt QUIC race: no candidates")
	}
	type result struct {
		attempt *h3DialAttempt
		claim   *platformTransportBudgetReservation
		err     error
	}
	results := make(chan result, len(candidates))
	cancels := make([]context.CancelFunc, 0, len(candidates))
	launched, pending := 0, 0
	launch := func() error {
		claim, err := policy.acquire(ctx)
		if err != nil {
			return err
		}
		attemptCtx, cancel := context.WithCancel(ctx)
		cancels = append(cancels, cancel)
		address := candidates[launched]
		launched++
		pending++
		go func() {
			attempt, err := dial(attemptCtx, address, claim)
			results <- result{attempt: attempt, claim: claim, err: err}
		}()
		return nil
	}
	cancelAttempts := func() {
		for _, cancel := range cancels {
			cancel()
		}
	}
	finish := func() {
		cancelAttempts()
		for pending > 0 {
			loser := <-results
			pending--
			loser.attempt.close()
			loser.claim.Release()
		}
	}
	defer finish()
	advance := func() error {
		err := launch()
		if errors.Is(err, errExtenderMemoryBudget) && pending > 0 {
			cancelAttempts()
			return nil // the candidate index advances only after admission
		}
		return err
	}
	if err := advance(); err != nil {
		return nil, err
	}
	stagger := time.NewTimer(platformH3FamilyRaceStagger)
	defer stagger.Stop()
	var firstErr error
	for {
		select {
		case completed := <-results:
			pending--
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
				stagger.Reset(platformH3FamilyRaceStagger)
			} else if pending == 0 {
				if firstErr == nil {
					firstErr = context.Canceled
				}
				return nil, firstErr
			}
		case <-stagger.C:
			if launched < len(candidates) {
				if err := advance(); err != nil {
					return nil, err
				}
				stagger.Reset(platformH3FamilyRaceStagger)
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
