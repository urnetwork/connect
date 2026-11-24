package connect

// transport_h3_race_stagger_test.go — the Happy Eyeballs stagger of the H3
// and alt quic races is a setting of each race's owner (QD-702), the platform
// transport's H3FamilyRaceStagger and the client strategy's
// AltQuicRaceStagger, where it was the package constant
// platformH3FamilyRaceStagger.

import (
	"context"
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"
)

// The defaults are the constant the stagger was, 250 ms.
func TestH3RaceStaggerDefaultsAreTheFormerConstant(t *testing.T) {
	AssertEqual(t, DefaultPlatformTransportSettings().H3FamilyRaceStagger, 250*time.Millisecond)
	AssertEqual(t, DefaultClientStrategySettings().AltQuicRaceStagger, 250*time.Millisecond)
	AssertEqual(t, DefaultPlatformTransportSettingsWithMemoryTarget(mib(64)).H3FamilyRaceStagger, 250*time.Millisecond)
}

// Each race launches its second candidate its stagger after the first, while
// the first still dials, and a non-positive stagger launches it at once:
// the plain and the budgeted H3 races and the alt quic race, on the bubble's
// clock.
func TestH3RacesLaunchTheNextCandidateTheirStaggerLater(t *testing.T) {
	for _, kind := range []string{"plain", "memory", "alt"} {
		for _, stagger := range []time.Duration{-time.Second, 0, 100 * time.Millisecond, 400 * time.Millisecond} {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				candidates := []*net.UDPAddr{
					{IP: net.ParseIP("192.0.2.1"), Port: 1443},
					{IP: net.ParseIP("2001:db8::1"), Port: 1443},
				}
				budget := NewPlatformTransportBudget(mib(64), 8)
				launches := make(chan time.Time, 2)
				dial := func(ctx context.Context, _ *net.UDPAddr, _ *platformTransportBudgetReservation) (*h3DialAttempt, error) {
					launches <- time.Now()
					<-ctx.Done()
					return nil, ctx.Err()
				}
				h3Dial := func(ctx context.Context, address *net.UDPAddr) (*h3DialAttempt, error) {
					return dial(ctx, address, nil)
				}
				done := make(chan error, 1)
				go func() {
					var err error
					switch kind {
					case "plain":
						_, err = raceH3Dial(ctx, candidates, stagger, h3Dial)
					case "memory":
						_, err = raceH3DialWithMemory(ctx, candidates, budget, 4096, 0, stagger, h3Dial)
					case "alt":
						_, err = raceAltQuicDial(ctx, candidates,
							extenderQuicMemoryPolicy{budget: budget, byteCount: 4096, usesSlot: true}, stagger, dial)
					default:
						err = errors.New("invalid synthetic race")
					}
					done <- err
				}()

				first := <-launches
				second := <-launches
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Errorf("%s race with stagger %s = %v, want its context's end", kind, stagger, err)
				}
				want := max(stagger, 0)
				if got := second.Sub(first); got != want {
					t.Errorf("%s race with stagger %s launched its second candidate %s after the first, want %s", kind, stagger, got, want)
				}
			})
		}
	}
}
