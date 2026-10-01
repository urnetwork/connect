// Late-family results remain part of one bounded race, including when the
// first handshake fails or the speculative carrier reservation is refused.
package connect

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The synthetic socket has only a lifecycle: no network operation is needed.
type progressiveUdpTestSocket struct {
	net.PacketConn
	closeOnce sync.Once
	onClose   func()
}

func (self *progressiveUdpTestSocket) Close() error {
	self.closeOnce.Do(self.onClose)
	return nil
}

// The same dial contract is exercised under all three existing schedulers.
func runUdpProgressiveTestRace(
	ctx context.Context,
	kind string,
	candidates []*net.UDPAddr,
	source *udpDialCandidateSource,
	budget *PlatformTransportBudget,
	dial func(context.Context, *net.UDPAddr, *platformTransportBudgetReservation) (*h3DialAttempt, error),
) (*h3DialAttempt, error) {
	switch kind {
	case "plain":
		return raceH3DialProgressive(ctx, candidates, source, func(ctx context.Context, address *net.UDPAddr) (*h3DialAttempt, error) {
			return dial(ctx, address, nil)
		})
	case "memory":
		return raceH3DialWithMemoryProgressive(ctx, candidates, source, budget, 4096, 0,
			func(ctx context.Context, address *net.UDPAddr) (*h3DialAttempt, error) {
				return dial(ctx, address, nil)
			})
	case "alt":
		return raceAltQuicDialProgressive(ctx, candidates, source,
			extenderQuicMemoryPolicy{budget: budget, byteCount: 4096, usesSlot: true}, dial)
	default:
		return nil, errors.New("invalid synthetic scheduler")
	}
}

// A single owned resolver worker publishes only after the explicit barrier.
func newUdpProgressiveTestSource(ctx context.Context, release <-chan struct{}, lateAddr netip.Addr) (*udpDialCandidateSource, <-chan struct{}) {
	queryCtx, cancel := context.WithCancel(ctx)
	results := make(chan dohDialQueryResult, 1)
	exited := make(chan struct{})
	source := &udpDialCandidateSource{results: results, pending: 1, ports: []int{1443}, cancel: cancel}
	source.queryWorkers.Add(1)
	go func() {
		defer source.queryWorkers.Done()
		defer close(exited)
		select {
		case <-release:
			results <- dohDialQueryResult{addrs: []netip.Addr{lateAddr}, authoritative: true}
		case <-queryCtx.Done():
			results <- dohDialQueryResult{}
		}
	}()
	return source, exited
}

// No usable winner exists yet when the first handshake fails. The race must
// retain the pending family, then launch it without an extra fallback timer.
func checkUdpProgressiveLateFallback(t *testing.T, kind string) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		firstAddr := netip.MustParseAddr("192.0.2.131")
		lateAddr := netip.MustParseAddr("2001:db8::131")
		candidates := []*net.UDPAddr{{IP: net.IP(firstAddr.AsSlice()), Port: 1443}}
		releaseFirst := make(chan struct{})
		releaseDns := make(chan struct{})
		releaseFirstOnce := sync.OnceFunc(func() { close(releaseFirst) })
		releaseDnsOnce := sync.OnceFunc(func() { close(releaseDns) })
		defer releaseFirstOnce()
		defer releaseDnsOnce()
		source, resolverExited := newUdpProgressiveTestSource(ctx, releaseDns, lateAddr)
		defer source.close()
		budget := NewPlatformTransportBudget(4096, 1)
		if kind == "memory" {
			base := budget.register(platformTransportBudgetH3Explicit, 4096, true)
			if !base.TryAcquire() {
				t.Fatal("synthetic H3 base admission")
			}
			defer base.Release()
		}
		var live atomic.Int64
		dialed := make(chan netip.Addr, 2)
		dial := func(dialCtx context.Context, address *net.UDPAddr, claim *platformTransportBudgetReservation) (*h3DialAttempt, error) {
			addr, _ := netip.AddrFromSlice(address.IP)
			addr = addr.Unmap()
			live.Add(1)
			attempt := &h3DialAttempt{
				udpAddr: address, budgetReservation: claim,
				packetConn: &progressiveUdpTestSocket{onClose: func() { live.Add(-1) }},
			}
			dialed <- addr
			if addr == firstAddr {
				select {
				case <-releaseFirst:
				case <-dialCtx.Done():
				}
				// Real dialH3/dialAltQuicAttempt also clean rejected attempts
				// before returning an error to their scheduler.
				attempt.close()
				return nil, errors.New("synthetic first handshake failed")
			}
			return attempt, nil
		}
		type outcome struct {
			attempt *h3DialAttempt
			err     error
		}
		done := make(chan outcome, 1)
		go func() {
			attempt, err := runUdpProgressiveTestRace(ctx, kind, candidates, source, budget, dial)
			done <- outcome{attempt: attempt, err: err}
		}()
		started := time.Now()
		synctest.Wait()
		select {
		case addr := <-dialed:
			if addr != firstAddr {
				t.Errorf("first attempt=%s", addr)
			}
		default:
			t.Error("first candidate waited for the pending family")
		}
		releaseFirstOnce()
		synctest.Wait()
		var result outcome
		returnedEarly := false
		select {
		case result = <-done:
			returnedEarly = true
			t.Error("race declared exhaustion while a DNS family remained pending")
		default:
		}
		releaseDnsOnce()
		synctest.Wait()
		if !returnedEarly {
			result = <-done
		}
		if result.err != nil || result.attempt == nil || !result.attempt.udpAddr.IP.Equal(net.IP(lateAddr.AsSlice())) {
			t.Errorf("late family did not win: attempt=%v error=%v", result.attempt, result.err)
		}
		result.attempt.close()
		if !time.Now().Equal(started) {
			t.Errorf("late fallback consumed a timer: %s", time.Since(started))
		}
		if live.Load() != 0 {
			t.Errorf("live sockets after close=%d", live.Load())
		}
		select {
		case <-resolverExited:
		default:
			t.Error("returned race did not join its resolver worker")
		}
	})
}

func TestUdpProgressivePlainRetainsLateFamily(t *testing.T) {
	checkUdpProgressiveLateFallback(t, "plain")
}
func TestUdpProgressiveMemoryRetainsLateFamily(t *testing.T) {
	checkUdpProgressiveLateFallback(t, "memory")
}
func TestUdpProgressiveAltRetainsLateFamily(t *testing.T) { checkUdpProgressiveLateFallback(t, "alt") }

// Only one carrier claim fits. A late candidate must retain its position,
// cancel the blackholed first attempt at the existing stagger, and wait until
// its held teardown releases before opening the replacement.
func checkUdpProgressiveBudgetFallback(t *testing.T, kind string) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		firstAddr := netip.MustParseAddr("192.0.2.132")
		lateAddr := netip.MustParseAddr("2001:db8::132")
		releaseDns := make(chan struct{})
		releaseTail := make(chan struct{})
		releaseDnsOnce := sync.OnceFunc(func() { close(releaseDns) })
		releaseTailOnce := sync.OnceFunc(func() { close(releaseTail) })
		defer releaseDnsOnce()
		defer releaseTailOnce()
		source, resolverExited := newUdpProgressiveTestSource(ctx, releaseDns, lateAddr)
		defer source.close()
		budget := NewPlatformTransportBudget(4096, 1)
		var base *platformTransportBudgetReservation
		if kind == "memory" {
			base = budget.register(platformTransportBudgetH3Explicit, 4096, true)
			if !base.TryAcquire() {
				t.Fatal("synthetic H3 base admission")
			}
			defer base.Release()
		}
		firstEntered := make(chan struct{})
		firstCanceled := make(chan struct{})
		firstClosed := make(chan struct{})
		lateEntered := make(chan struct{})
		var live atomic.Int64
		dial := func(dialCtx context.Context, address *net.UDPAddr, claim *platformTransportBudgetReservation) (*h3DialAttempt, error) {
			addr, _ := netip.AddrFromSlice(address.IP)
			if count := live.Add(1); count != 1 {
				t.Errorf("socket opened before prior teardown: live=%d", count)
			}
			if stats := budget.Stats(); stats.UsedByteCount != 4096 {
				t.Errorf("socket lacked its bounded claim: %+v", stats)
			}
			attempt := &h3DialAttempt{
				udpAddr: address, budgetReservation: claim,
				packetConn: &progressiveUdpTestSocket{onClose: func() { live.Add(-1) }},
			}
			if addr.Unmap() == firstAddr {
				close(firstEntered)
				<-dialCtx.Done()
				close(firstCanceled)
				<-releaseTail
				attempt.close()
				close(firstClosed)
				return nil, dialCtx.Err()
			}
			select {
			case <-firstClosed:
			default:
				t.Error("replacement opened before first owner closed")
			}
			close(lateEntered)
			return attempt, nil
		}
		type outcome struct {
			attempt *h3DialAttempt
			err     error
		}
		done := make(chan outcome, 1)
		go func() {
			attempt, err := runUdpProgressiveTestRace(ctx, kind,
				[]*net.UDPAddr{{IP: net.IP(firstAddr.AsSlice()), Port: 1443}}, source, budget, dial)
			done <- outcome{attempt: attempt, err: err}
		}()
		synctest.Wait()
		select {
		case <-firstEntered:
		default:
			t.Fatal("first admitted candidate did not start")
		}
		releaseDnsOnce()
		synctest.Wait()
		// This advances the scheduler's specified positive timer; readiness
		// and cleanup assertions use barriers, not a short negative sleep.
		time.Sleep(platformH3FamilyRaceStagger)
		synctest.Wait()
		select {
		case <-firstCanceled:
		default:
			t.Error("budget-refused late candidate did not cancel its predecessor")
			cancel()
			synctest.Wait()
		}
		select {
		case <-lateEntered:
			t.Error("budget-refused candidate bypassed the teardown barrier")
		default:
		}
		releaseTailOnce()
		synctest.Wait()
		result := <-done
		if result.err != nil || result.attempt == nil || !result.attempt.udpAddr.IP.Equal(net.IP(lateAddr.AsSlice())) {
			t.Errorf("budget-refused late candidate was lost: attempt=%v error=%v", result.attempt, result.err)
		}
		result.attempt.close()
		base.Release()
		if stats := budget.Stats(); live.Load() != 0 || stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 {
			t.Errorf("completed race retained socket/claim: live=%d stats=%+v", live.Load(), stats)
		}
		select {
		case <-resolverExited:
		default:
			t.Error("budgeted race did not join DNS")
		}
	})
}

func TestUdpProgressiveMemoryRetainsBudgetRefusedFamily(t *testing.T) {
	checkUdpProgressiveBudgetFallback(t, "memory")
}
func TestUdpProgressiveAltRetainsBudgetRefusedFamily(t *testing.T) {
	checkUdpProgressiveBudgetFallback(t, "alt")
}
