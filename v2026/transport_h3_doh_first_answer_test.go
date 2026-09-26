// H3's real carrier planner hands first-family candidates to the same
// confirmed-handshake scheduler which consumes late DNS results.
package connect

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"
)

// No global strategy, resolver or carrier budget participates in this planner.
func newH3DohProgressTransport(ctx context.Context, cache *DohCache) *PlatformTransport {
	strategySettings := DefaultClientStrategySettings()
	strategySettings.ConnectSettings.Log = NewNoopLogger()
	strategy := &ClientStrategy{
		ctx: ctx, settings: strategySettings,
		internalDohResolver: &internalDohResolver{cache: cache, domains: []string{"first-answer.example"}},
	}
	settings := DefaultPlatformTransportSettings()
	settings.PlatformTransportBudget = NewPlatformTransportBudget(8*1024*1024, 2)
	settings.H3Port = 1443
	return &PlatformTransport{
		ctx: ctx, log: NewNoopLogger(), clientStrategy: strategy,
		platformUrl: "https://first-answer.example", settings: settings,
	}
}

// Planner readiness must reach an admitted dial without advancing virtual
// time or releasing the sibling. Both scheduler ownership policies are tested.
func checkH3DohFirstAnswer(t *testing.T, readyIpv6 bool, budgeted bool) {
	t.Helper()
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		cache, readyAddr := newDohFirstAnswerCache(readyIpv6)
		defer cache.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		transport := newH3DohProgressTransport(ctx, cache)
		budget := transport.settings.PlatformTransportBudget
		base := budget.register(platformTransportBudgetH3Explicit, 4096, true)
		if !base.TryAcquire() {
			t.Fatal("synthetic base admission")
		}
		defer base.Release()
		dialed := make(chan netip.Addr, 1)
		done := make(chan error, 1)
		go func() {
			candidates, _, pending, err := transport.h3DialCandidatesProgressive(ctx, TransportModeH3, "first-answer.example")
			if err != nil {
				done <- err
				return
			}
			dial := func(ctx context.Context, address *net.UDPAddr) (*h3DialAttempt, error) {
				addr, ok := netip.AddrFromSlice(address.IP)
				if !ok {
					return nil, errors.New("invalid synthetic candidate")
				}
				dialed <- addr.Unmap()
				return &h3DialAttempt{udpAddr: address}, nil
			}
			var attempt *h3DialAttempt
			if budgeted {
				attempt, err = raceH3DialWithMemoryProgressive(ctx, candidates, pending, budget, 4096, 0, dial)
			} else {
				attempt, err = raceH3DialProgressive(ctx, candidates, pending, dial)
			}
			attempt.close()
			done <- err
		}()
		started := time.Now()
		synctest.Wait()
		select {
		case addr := <-dialed:
			if addr != readyAddr {
				t.Errorf("first H3 candidate=%s, want %s", addr, readyAddr)
			}
		default:
			t.Error("H3 planner waited for the pending DNS family before its first dial")
		}
		if !time.Now().Equal(started) {
			t.Errorf("first H3 dial consumed a timer: %s", time.Since(started))
		}
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("H3 dial outcome: %v", err)
		}
		heldType := "AAAA"
		if readyIpv6 {
			heldType = "A"
		}
		cache.stateLock.Lock()
		foreign := cache.inflight[NewDohKey(heldType, "first-answer.example")]
		cache.stateLock.Unlock()
		if foreign == nil {
			t.Error("dial owner removed an unrelated resolver flight")
		} else {
			select {
			case <-foreign.done:
				t.Error("dial owner finished an unrelated resolver flight")
			default:
			}
		}
		base.Release()
		if stats := budget.Stats(); stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 {
			t.Errorf("H3 owner retained a carrier claim: %+v", stats)
		}
	})
}

func TestH3DohFirstAnswerIpv4(t *testing.T) {
	checkH3DohFirstAnswer(t, false, false)
}

func TestH3DohFirstAnswerIpv6(t *testing.T) {
	checkH3DohFirstAnswer(t, true, false)
}

func TestH3DohFirstAnswerMemoryIpv4(t *testing.T) {
	checkH3DohFirstAnswer(t, false, true)
}

func TestH3DohFirstAnswerMemoryIpv6(t *testing.T) {
	checkH3DohFirstAnswer(t, true, true)
}

// An authoritative empty first family is not usable work; it must retain the
// later family rather than returning an empty successful plan.
func TestH3DohFirstAnswerEmptyFamilyWaitsForUsableSibling(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		cache, _ := newDohFirstAnswerCache(false)
		defer cache.Close()
		firstKey := NewDohKey("A", "first-answer.example")
		heldKey := NewDohKey("AAAA", "first-answer.example")
		cache.queryResultExpiration[firstKey] = &DohResult{Time: time.Now(), Miss: true}
		foreign := cache.inflight[heldKey]
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		transport := newH3DohProgressTransport(ctx, cache)
		type result struct {
			addrs []*net.UDPAddr
			err   error
		}
		done := make(chan result, 1)
		go func() {
			addrs, _, source, err := transport.h3DialCandidatesProgressive(ctx, TransportModeH3, "first-answer.example")
			source.close()
			done <- result{addrs: addrs, err: err}
		}()
		synctest.Wait()
		returnedEarly := false
		select {
		case <-done:
			returnedEarly = true
			t.Error("empty first DNS answer completed a usable H3 plan")
		default:
		}
		lateAddr := netip.MustParseAddr("2001:db8::92")
		foreign.addrs = []netip.Addr{lateAddr}
		foreign.authoritative = true
		close(foreign.done)
		if !returnedEarly {
			resolved := <-done
			if resolved.err != nil || len(resolved.addrs) != 1 || !resolved.addrs[0].IP.Equal(net.IP(lateAddr.AsSlice())) {
				t.Errorf("later usable family lost: addrs=%v error=%v", resolved.addrs, resolved.err)
			}
		}
	})
}
