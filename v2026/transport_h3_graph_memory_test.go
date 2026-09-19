// Complete H3 carrier admission must survive queued siblings without relying
// on dial scheduling to leave space for its DNS translation and extender.
package connect

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// A queued explicit carrier must not take priority over the translation needed
// by the carrier whose inner QUIC working set has already been admitted.
func TestPlatformDnsCarrierAdmissionWithPendingSibling(t *testing.T) {
	for _, mode := range []TransportMode{TransportModeH3Dns, TransportModeH3DnsPump, TransportModeAuto} {
		ctx, cancel := context.WithCancel(t.Context())
		settings := DefaultPlatformTransportSettings()
		settings.H3BudgetByteCount = mib(8)
		settings.PlatformTransportBudget = NewPlatformTransportBudget(mib(16), 16)
		settings.ModePreferences = map[TransportMode]int{TransportModeH3Dns: 1}
		settings.DnsPumpHost = "192.0.2.53"
		settings.DnsTlds = [][]byte{[]byte("carrier.example.")}
		marker := errors.New("admitted socket factory")
		settings.H3PacketConnFactory = func(context.Context) (net.PacketConn, error) {
			return nil, marker
		}
		started := make(chan struct{})
		proceed := make(chan struct{})
		result := make(chan error, 1)
		var transport *PlatformTransport
		settings.runH3ModeForTest = func(ctx context.Context, mode TransportMode, _ time.Duration) {
			close(started)
			select {
			case <-proceed:
			case <-ctx.Done():
				return
			}
			addresses, wrap, err := transport.h3DialCandidates(ctx, mode, "192.0.2.53")
			if err == nil {
				_, err = transport.dialH3(ctx, mode, "carrier.example", addresses[0], wrap,
					&tls.Config{}, newPlatformQuicConfig(settings, 1), 1, false)
			}
			result <- err
			<-ctx.Done()
		}
		strategy := NewClientStrategyWithDefaults(ctx)
		routes := NewRouteManager(ctx, "dns-admission")
		transport = NewPlatformTransportWithTargetMode(ctx, strategy, routes,
			"https://carrier.example", &ClientAuth{InstanceId: NewId()}, mode, settings)
		t.Cleanup(func() {
			cancel()
			transport.CloseAndWait(context.Background())
			strategy.Close()
		})
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: first carrier did not acquire", mode)
		}
		// Register after the first group acquires, before it opens a socket.
		// Keeping the sibling disabled makes this ordering entirely synchronous.
		siblingSettings := *settings
		siblingSettings.StartDisabled = true
		sibling := NewPlatformTransportWithTargetMode(ctx, strategy, routes,
			"https://sibling.example", &ClientAuth{InstanceId: NewId()}, mode, &siblingSettings)
		t.Cleanup(func() { sibling.CloseAndWait(context.Background()) })
		close(proceed)
		select {
		case err := <-result:
			if !errors.Is(err, marker) {
				t.Fatalf("%s: queued sibling prevented the admitted carrier's first socket: %v", mode, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: admitted carrier did not reach the socket boundary", mode)
		}
		if sibling.h3BudgetReservation.TryAcquire() {
			t.Fatalf("%s: a second partial graph consumed the first carrier's translation capacity", mode)
		}
		cancel()
		transport.CloseAndWait(context.Background())
		sibling.CloseAndWait(context.Background())
		strategy.Close()
		stats := settings.PlatformTransportBudget.Stats()
		if stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
			t.Fatalf("%s: carrier teardown retained memory: %+v", mode, stats)
		}
	}
}

// A known outer extender is mandatory too. Force the constructor, group,
// translation, and real extender dial boundary through the same child/root
// reservation while another explicit carrier remains queued.
func TestPlatformH3ExtenderGraphAdmissionWithPendingSibling(t *testing.T) {
	for _, mode := range []TransportMode{TransportModeH3, TransportModeH3Dns, TransportModeH3DnsPump} {
		for _, carrier := range []ExtenderConnectMode{ExtenderConnectModeTcpTls, ExtenderConnectModeQuic, ExtenderConnectModeDns} {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			root := NewPlatformTransportBudget(mib(16), 16)
			budget := newPlatformTransportBudget(mib(16), 16, root)
			settings := DefaultPlatformTransportSettings()
			settings.H3BudgetByteCount = mib(8)
			settings.PlatformTransportBudget = budget
			settings.DnsPumpHost = "pump.example"
			settings.DnsTlds = [][]byte{[]byte("carrier.example.")}
			settings.StartDisabled = true
			wantNestedBytes := kib(1408)
			if carrier == ExtenderConnectModeTcpTls {
				wantNestedBytes = settings.H1BudgetByteCount
			} else if carrier == ExtenderConnectModeDns {
				wantNestedBytes += kib(144)
			}
			if mode != TransportModeH3 {
				wantNestedBytes += kib(144)
			}
			marker := errors.New("admitted outer socket")
			observeSocket := func() error {
				stats := budget.StatsWithRoot()
				if stats.Budget.UsedByteCount != mib(8)+wantNestedBytes ||
					stats.Root.UsedByteCount != stats.Budget.UsedByteCount ||
					stats.Budget.UsedTransportCount != 1 || stats.Root.UsedTransportCount != 1 {
					t.Errorf("%s/%s: outer socket escaped complete graph admission: %+v", mode, carrier, stats)
				}
				return marker
			}
			strategySettings := DefaultClientStrategySettings()
			strategySettings.EnableNormal = false
			strategySettings.EnableResilient = false
			strategySettings.ExtenderConfigs = []*ExtenderConfig{{
				Profile: ExtenderProfile{ConnectMode: carrier, ServerName: "extender.example", Port: 1443},
				Ip:      netip.MustParseAddr("192.0.2.10"),
			}}
			strategySettings.ConnectSettings.DialContextSettings = &DialContextSettings{
				DialContext: func(context.Context, string, string) (net.Conn, error) {
					return nil, observeSocket()
				},
				PacketConnFactory: func(context.Context) (net.PacketConn, error) {
					return nil, observeSocket()
				},
			}
			strategy := NewClientStrategy(ctx, strategySettings)
			defer strategy.Close()
			routes := NewRouteManager(ctx, "extender-graph-admission")
			result := make(chan error, 1)
			var transport *PlatformTransport
			settings.runH3ModeForTest = func(ctx context.Context, mode TransportMode, _ time.Duration) {
				addresses, wrap, err := transport.h3DialCandidates(ctx, mode, "carrier.example")
				if err == nil {
					// Failed attempts release nested allocations so a reconnect
					// can reuse the same admitted graph without growing it.
					for range 2 {
						_, err = transport.dialH3(ctx, mode, "carrier.example", addresses[0], wrap,
							&tls.Config{}, newPlatformQuicConfig(settings, 1), 1, false)
						if !errors.Is(err, marker) {
							break
						}
					}
				}
				result <- err
				<-ctx.Done()
			}
			transport = NewPlatformTransportWithTargetMode(ctx, strategy, routes,
				"https://carrier.example", &ClientAuth{InstanceId: NewId()}, mode, settings)
			defer transport.CloseAndWait(context.Background())
			sibling := NewPlatformTransportWithTargetMode(ctx, strategy, routes,
				"https://sibling.example", &ClientAuth{InstanceId: NewId()}, mode, settings)
			defer sibling.CloseAndWait(context.Background())
			transport.SetEnabled(true)
			select {
			case err := <-result:
				if !errors.Is(err, marker) {
					t.Fatalf("%s/%s: complete graph could not reach its socket: %v", mode, carrier, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s/%s: complete graph remained blocked", mode, carrier)
			}
			cancel()
			transport.CloseAndWait(context.Background())
			sibling.CloseAndWait(context.Background())
			for _, stats := range []PlatformTransportBudgetStats{budget.Stats(), root.Stats(), transport.h3NestedBudget.Stats()} {
				if stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
					t.Fatalf("%s/%s: graph retained capacity after teardown: %+v", mode, carrier, stats)
				}
			}
		}
	}
}

// The second port must reserve both its speculative QUIC state and its own
// translation. When only the former fits, explicitly cancel/join the first
// attempt and let the second reuse the complete base graph.
func TestPlatformDnsGraphRaceAdmitsCompleteFallback(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			const nestedBytes = ByteCount(144 * 1024)
			const innerBytes = ByteCount(3 * 1024 * 1024)
			limit := innerBytes + 2*nestedBytes + platformH3DialTransientByteCount
			if !concurrent {
				limit--
			}
			budget := NewPlatformTransportBudget(limit, 1)
			base := budget.register(platformTransportBudgetH3Explicit, innerBytes+nestedBytes, true)
			if !base.TryAcquire() {
				t.Fatal("complete base graph did not fit")
			}
			defer base.Release()
			nested := NewPlatformTransportBudget(nestedBytes, 0)
			ctx := context.WithValue(t.Context(), platformTransportNestedBudgetContextKey{}, nested)
			transport := &PlatformTransport{settings: &PlatformTransportSettings{PlatformTransportBudget: budget}}
			candidates := []*net.UDPAddr{{IP: net.ParseIP("192.0.2.53"), Port: 53}, {IP: net.ParseIP("192.0.2.53"), Port: 4053}}
			var live, peak atomic.Int32
			winner, err := raceH3DialWithMemory(ctx, candidates, budget, platformH3DialTransientByteCount, nestedBytes,
				func(ctx context.Context, address *net.UDPAddr) (*h3DialAttempt, error) {
					claim, err := transport.acquireH3TranslationMemory(ctx)
					if err != nil {
						return nil, err
					}
					count := live.Add(1)
					if count > peak.Load() {
						peak.Store(count)
					}
					attempt := &h3DialAttempt{
						udpAddr:                address,
						translationReservation: claim,
						packetConn: &extenderMemoryPacketConn{onClose: func() {
							live.Add(-1)
						}},
					}
					if address == candidates[0] {
						<-ctx.Done()
						return attempt, ctx.Err()
					}
					return attempt, nil
				})
			if err != nil || winner == nil || winner.udpAddr != candidates[1] {
				t.Fatalf("complete fallback did not win: %v %v", winner, err)
			}
			wantPeak := int32(1)
			if concurrent {
				wantPeak = 2
			}
			if peak.Load() != wantPeak || live.Load() != 1 || budget.Stats().UsedByteCount != innerBytes+nestedBytes {
				t.Fatalf("fallback ownership peak=%d live=%d budget=%+v", peak.Load(), live.Load(), budget.Stats())
			}
			winner.close()
			if live.Load() != 0 || nested.Stats().UsedByteCount != 0 || winner.translationReservation.budget.Stats().UsedByteCount != 0 {
				t.Fatal("fallback retained a socket or translation claim")
			}
		})
	}
}
