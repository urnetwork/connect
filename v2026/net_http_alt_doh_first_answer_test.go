// The actual Alt/whodis dial must allocate its first admitted socket after
// one usable protected-name answer, while the other family remains pending.
package connect

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	quic "github.com/quic-go/quic-go"
)

// The socket factory is the real dial boundary. No socket, handshake, outside
// resolver, or production identity is used to establish first-answer readiness.
func checkAltDohFirstAnswer(t *testing.T, readyIpv6 bool, whodis bool) {
	t.Helper()
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		cache, _ := newDohFirstAnswerCache(readyIpv6)
		defer cache.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		entered := make(chan struct{})
		markEntered := sync.OnceFunc(func() { close(entered) })
		strategySettings := DefaultClientStrategySettings()
		strategySettings.AltUrl = "https://first-answer.example:1443"
		strategySettings.ConnectSettings.Log = NewNoopLogger()
		strategySettings.ConnectSettings.DialContextSettings = &DialContextSettings{
			PacketConnFactory: func(attemptCtx context.Context) (net.PacketConn, error) {
				markEntered()
				<-attemptCtx.Done()
				return nil, attemptCtx.Err()
			},
		}
		budget := NewPlatformTransportBudget(64*1024*1024, 4)
		transportSettings := DefaultPlatformTransportSettings()
		transportSettings.PlatformTransportBudget = budget
		dialCtx := context.WithValue(ctx, extenderTransportSettingsContextKey{}, transportSettings)
		strategy := &ClientStrategy{
			ctx:      ctx,
			settings: strategySettings,
			internalDohResolver: &internalDohResolver{
				cache:   cache,
				domains: []string{"first-answer.example"},
			},
		}
		done := make(chan error, 1)
		go func() {
			conn, err := strategy.dialAltQuic(dialCtx, whodis,
				&tls.Config{ServerName: "first-answer.example"}, &quic.Config{})
			if conn != nil {
				_ = conn.CloseWithError(0, "synthetic test done")
			}
			done <- err
		}()
		started := time.Now()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Error("the actual Alt socket boundary waited for the pending DNS family")
		}
		if !time.Now().Equal(started) {
			t.Errorf("the first Alt attempt consumed a timer: %s", time.Since(started))
		}
		cancel()
		if err := <-done; err == nil || !errors.Is(err, context.Canceled) {
			t.Errorf("canceled Alt dial error = %v", err)
		}
		if stats := budget.Stats(); stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 {
			t.Errorf("canceled Alt attempt retained its carrier claim: %+v", stats)
		}
	})
}

func TestAltDohFirstAnswerIpv4(t *testing.T) {
	checkAltDohFirstAnswer(t, false, false)
}

func TestAltDohFirstAnswerIpv6(t *testing.T) {
	checkAltDohFirstAnswer(t, true, false)
}

func TestAltDohFirstAnswerWhodisIpv4(t *testing.T) {
	checkAltDohFirstAnswer(t, false, true)
}

func TestAltDohFirstAnswerWhodisIpv6(t *testing.T) {
	checkAltDohFirstAnswer(t, true, true)
}

// A whodis family expands over both existing carrier ports. The first batch
// must start early without losing the same port choices for the late family.
func TestAltDohFirstAnswerLateFamilyRetainsWhodisPorts(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		cache, firstAddr := newDohFirstAnswerCache(false)
		defer cache.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := DefaultClientStrategySettings()
		settings.AltUrl = "https://first-answer.example"
		strategy := &ClientStrategy{
			ctx: ctx, settings: settings,
			internalDohResolver: &internalDohResolver{cache: cache, domains: []string{"first-answer.example"}},
		}
		type outcome struct {
			candidates []*net.UDPAddr
			source     *udpDialCandidateSource
			err        error
		}
		done := make(chan outcome, 1)
		go func() {
			candidates, source, err := strategy.altDialCandidatesProgressive(ctx, true)
			done <- outcome{candidates: candidates, source: source, err: err}
		}()
		synctest.Wait()
		var result outcome
		select {
		case result = <-done:
		default:
			t.Error("whodis planner waited for its late DNS family")
			cancel()
			result = <-done
		}
		defer result.source.close()
		if result.err != nil || result.source == nil || len(result.candidates) != 2 {
			t.Errorf("whodis first-family plan lost candidates/source: count=%d pending=%v error=%v", len(result.candidates), result.source != nil, result.err)
			return
		}
		for i, port := range []int{DefaultDnsPort, DefaultWhodisPort} {
			if result.candidates[i].Port != port || !result.candidates[i].IP.Equal(net.IP(firstAddr.AsSlice())) {
				t.Errorf("whodis first-family port order[%d]=%v", i, result.candidates[i])
			}
		}
		lateAddr := netip.MustParseAddr("2001:db8::93")
		foreign := cache.inflight[NewDohKey("AAAA", "first-answer.example")]
		foreign.addrs = []netip.Addr{lateAddr}
		foreign.authoritative = true
		close(foreign.done)
		resolved := <-result.source.resultChannel()
		all := result.source.appendReady(result.candidates, resolved)
		if len(all) != 4 {
			t.Fatalf("late whodis family retained %d candidates, want 4", len(all))
		}
		for i, port := range []int{DefaultDnsPort, DefaultWhodisPort} {
			if all[i+2].Port != port || !all[i+2].IP.Equal(net.IP(lateAddr.AsSlice())) {
				t.Errorf("late whodis port order[%d]=%v", i, all[i+2])
			}
		}
	})
}
