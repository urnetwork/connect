// Force admission, failed-attempt unwind, and cancellation at the constructor. The
// current-profile loopback graphs are separate tests; these paths open no
// network because their final socket factories return a synthetic failure.
package connect

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// A full current owner must park every intended graph before socket creation.
// A failed socket releases its attempt claims while the owner remains live;
// only later cancellation releases the owner's retained reconnect allowance.
// Synctest makes these states explicit without sleeps or queue polling.
func TestIosMemoryCarrierAdmissionRefusalAndFailure(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	for _, mode := range []TransportMode{TransportModeH1, TransportModeH3, TransportModeH3Dns, TransportModeH3DnsPump} {
		for _, outer := range []string{"direct", ExtenderCarrierTcp, ExtenderCarrierQuic, ExtenderCarrierDns} {
			for _, refuse := range []bool{true, false} {
				synctest.Test(t, func(t *testing.T) {
					settings := newIosMemoryLiveSettings(t)
					settings.AltUrl = "https://192.0.2.53:443"
					settings.DnsPumpHost = "192.0.2.53"
					settings.DnsTlds = [][]byte{[]byte("carrier.example.")}
					settings.ModeInitialDelay = 0
					budget := settings.PlatformTransportBudget
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					want := settings.H1BudgetByteCount
					if mode != TransportModeH1 {
						want = settings.H3BudgetByteCount
						if mode != TransportModeH3 {
							want += kib(144)
						}
					}
					if outer == ExtenderCarrierTcp {
						want += settings.H1BudgetByteCount
					} else if outer != "direct" {
						want += kib(1408)
						if outer == ExtenderCarrierDns {
							want += kib(144)
						}
					}
					var opened atomic.Int32
					socketReached := make(chan struct{})
					marker := errors.New("synthetic current-profile socket failure")
					observeOpen := func() error {
						if opened.Add(1) == 1 {
							close(socketReached)
						}
						stats := budget.StatsWithRoot()
						if stats.Budget.UsedByteCount != want || stats.Budget.UsedTransportCount != 1 || stats.Root != stats.Budget {
							t.Errorf("%s/%s refused=%t: socket boundary claim want=%d got=%+v", mode, outer, refuse, want, stats)
						}
						return marker
					}
					strategySettings := DefaultClientStrategySettings()
					strategySettings.EnableResilient = false
					strategySettings.ParallelBlockSize = 1
					strategySettings.ExpandExtenderProfileCount = 0
					strategySettings.ExtenderConfigs = nil
					strategySettings.DnsTlds = nil
					strategySettings.InternalDohDomains = nil
					strategySettings.MinNextConnectDelay = 0
					strategySettings.MaxNextConnectDelay = 0
					strategySettings.ConnectSettings.DialContextSettings = &DialContextSettings{
						DialContext: func(context.Context, string, string) (net.Conn, error) {
							return nil, observeOpen()
						},
						PacketConnFactory: func(context.Context) (net.PacketConn, error) {
							return nil, observeOpen()
						},
					}
					if outer == "direct" {
						settings.H3PacketConnFactory = func(context.Context) (net.PacketConn, error) {
							return nil, observeOpen()
						}
					} else {
						strategySettings.EnableNormal = false
						carrier := ExtenderConnectModeTcpTls
						if outer == ExtenderCarrierQuic {
							carrier = ExtenderConnectModeQuic
						} else if outer == ExtenderCarrierDns {
							carrier = ExtenderConnectModeDns
						}
						strategySettings.ExtenderConfigs = []*ExtenderConfig{{
							Profile: ExtenderProfile{ConnectMode: carrier, ServerName: "extender.example", Port: 1443},
							Ip:      netip.MustParseAddr("192.0.2.10"), Secret: "synthetic-memory-secret",
						}}
					}
					strategy := NewClientStrategy(ctx, strategySettings)
					defer strategy.Close()
					var filler *platformTransportBudgetReservation
					if refuse {
						filler = budget.register(platformTransportBudgetExtender, mib(8), true)
						if !filler.TryAcquire() {
							t.Fatal("could not fill the current owner")
						}
						defer filler.Release()
					}
					assertIosMemoryLiveSettings(t, settings)
					routes := NewRouteManager(ctx, "ios-memory-admission")
					transport := NewPlatformTransportWithTargetMode(ctx, strategy, routes, "wss://192.0.2.53/ws",
						&ClientAuth{InstanceId: NewId()}, mode, settings)
					defer transport.CloseAndWait(context.Background())
					if refuse {
						synctest.Wait()
						if opened.Load() != 0 || !transport.IsWaitingForBudget() || budget.Stats().UsedByteCount != mib(8) {
							t.Fatalf("%s/%s: full owner escaped admission: opened=%d waiting=%t stats=%+v", mode, outer, opened.Load(), transport.IsWaitingForBudget(), budget.Stats())
						}
						cancel()
					} else {
						<-socketReached
						synctest.Wait()
						if opened.Load() != 1 {
							t.Fatalf("%s/%s: admitted graph did not reach its socket exactly once: %d", mode, outer, opened.Load())
						}
						if ctx.Err() != nil || transport.ctx.Err() != nil || transport.IsConnected() || routes.HasActiveTransport() {
							t.Fatalf("%s/%s: failed attempt did not leave an uncanceled, disconnected owner", mode, outer)
						}
						select {
						case <-transport.Done():
							t.Fatalf("%s/%s: socket failure terminated the owner before its explicit cancellation", mode, outer)
						default:
						}
						retained := want
						if mode == TransportModeH1 {
							retained = settings.H1BudgetByteCount
						} else if nested := transport.h3NestedBudget; nested != nil {
							stats := nested.Stats()
							if stats.TotalByteCount != want-settings.H3BudgetByteCount || stats.UsedByteCount != 0 ||
								stats.UsedTransportCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount ||
								stats.ReservedByteCount != stats.TotalByteCount {
								t.Fatalf("%s/%s: failed attempt retained nested ownership while owner was live: %+v", mode, outer, stats)
							}
						} else if want != settings.H3BudgetByteCount {
							t.Fatalf("%s/%s: composite H3 graph omitted its nested budget", mode, outer)
						}
						stats := budget.StatsWithRoot()
						if stats.Budget.UsedByteCount != retained || stats.Budget.UsedTransportCount != 1 ||
							stats.Budget.ReservedByteCount != want || stats.Budget.ReleasedByteCount != want-retained ||
							stats.Root != stats.Budget {
							t.Fatalf("%s/%s: failed socket did not unwind only attempt claims: retained=%d stats=%+v", mode, outer, retained, stats)
						}
						cancel()
					}
					// Cancellation must join without an active Close masking it.
					<-transport.Done()
					if err := transport.CloseAndWait(context.Background()); err != nil {
						t.Fatal(err)
					}
					filler.Release()
					stats := budget.StatsWithRoot()
					if stats.Budget.UsedByteCount != 0 || stats.Budget.UsedTransportCount != 0 ||
						stats.Budget.ReservedByteCount != stats.Budget.ReleasedByteCount || stats.Root != stats.Budget {
						t.Fatalf("%s/%s refused=%t: joined failure retained claims: %+v", mode, outer, refuse, stats)
					}
				})
			}
		}
	}
}
