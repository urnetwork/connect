// The translated H3 modes intentionally select one address, while pins and
// custom resolvers retain their existing policy rather than joining DNS races.
package connect

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func checkH3DohScalarCarrier(t *testing.T, mode TransportMode, readyIpv6 bool) {
	t.Helper()
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		cache, readyAddr := newDohFirstAnswerCache(readyIpv6)
		defer cache.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		transport := newH3DohProgressTransport(ctx, cache)
		transport.settings.DnsPumpHost = "first-answer.example"
		type outcome struct {
			addrs   []*net.UDPAddr
			pending *udpDialCandidateSource
			err     error
		}
		done := make(chan outcome, 1)
		go func() {
			addrs, _, source, err := transport.h3DialCandidatesProgressive(ctx, mode, "first-answer.example")
			done <- outcome{addrs: addrs, pending: source, err: err}
		}()
		started := time.Now()
		synctest.Wait()
		var result outcome
		select {
		case result = <-done:
		default:
			t.Error("translated H3 scalar pick waited for a pending DNS family")
			cancel()
			result = <-done
		}
		defer result.pending.close()
		if result.err != nil || len(result.addrs) != 1 || !result.addrs[0].IP.Equal(net.IP(readyAddr.AsSlice())) {
			t.Errorf("translated H3 first answer lost: addrs=%v error=%v", result.addrs, result.err)
		}
		if result.pending != nil {
			t.Error("translated H3 changed its single-address socket policy")
		}
		if !time.Now().Equal(started) {
			t.Errorf("translated H3 consumed a timer: %s", time.Since(started))
		}
	})
}

func TestH3DohScalarDnsIpv4(t *testing.T) { checkH3DohScalarCarrier(t, TransportModeH3Dns, false) }
func TestH3DohScalarDnsIpv6(t *testing.T) { checkH3DohScalarCarrier(t, TransportModeH3Dns, true) }
func TestH3DohScalarDnsPumpIpv4(t *testing.T) {
	checkH3DohScalarCarrier(t, TransportModeH3DnsPump, false)
}
func TestH3DohScalarDnsPumpIpv6(t *testing.T) {
	checkH3DohScalarCarrier(t, TransportModeH3DnsPump, true)
}

// A pinned carrier resolves only its configured family; a pending sibling
// is neither needed nor inherited as a streaming fallback.
func TestUdpProgressivePinnedPolicyRetainsSingleFamily(t *testing.T) {
	cache, readyAddr := newDohFirstAnswerCache(false)
	defer cache.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	transport := newH3DohProgressTransport(ctx, cache)
	addrs, pending, err := transport.clientStrategy.startControlUdpCandidates(ctx, "first-answer.example:1443", 4)
	defer pending.close()
	if err != nil || len(addrs) != 1 || !addrs[0].IP.Equal(net.IP(readyAddr.AsSlice())) || pending != nil {
		t.Fatalf("pinned policy changed: addrs=%v pending=%v error=%v", addrs, pending != nil, err)
	}
	if _, source, err := transport.clientStrategy.startControlUdpCandidates(ctx, "192.0.2.140:1443", 6); err == nil || source != nil {
		source.close()
		t.Fatal("pinned policy accepted a contradictory literal")
	}
}

// An explicitly installed ordinary resolver remains authoritative. Its dial
// is intercepted, so this test cannot query any configured or public server.
func TestUdpProgressiveCustomResolverRetainsAuthority(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var calls atomic.Int64
	settings := DefaultClientStrategySettings()
	settings.ConnectSettings.Resolver = &net.Resolver{
		PreferGo: true,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			calls.Add(1)
			return nil, errors.New("synthetic custom resolver refusal")
		},
	}
	strategy := &ClientStrategy{ctx: ctx, settings: settings}
	addrs, pending, err := strategy.startControlUdpCandidates(ctx, "custom-resolver.example.:1443", 0)
	defer pending.close()
	if err == nil || len(addrs) != 0 || pending != nil || calls.Load() == 0 {
		t.Fatalf("custom resolver policy bypassed: calls=%d addresses=%d pending=%v error=%v", calls.Load(), len(addrs), pending != nil, err)
	}
}
