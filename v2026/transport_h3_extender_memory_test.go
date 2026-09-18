package connect

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
)

// Exercise the actual platform-to-extender boundary with an untagged caller.
// Low-level policy tests that supply dialContext themselves cannot detect a
// caller losing the device owner while preserving the named destination.
func TestPlatformH3ExtenderPacketConnPreservesMemoryOwner(t *testing.T) {
	old := MemoryBudget()
	SetMemoryBudget(mib(32))
	defer SetMemoryBudget(old)
	for _, mode := range []TransportMode{TransportModeH3, TransportModeH3Dns, TransportModeH3DnsPump} {
		for _, carrier := range []ExtenderConnectMode{ExtenderConnectModeTcpTls, ExtenderConnectModeQuic, ExtenderConnectModeDns} {
			t.Run(string(mode)+"/"+ExtenderCarrierForConnectMode(carrier), func(t *testing.T) {
				settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(20))
				settings.AltUrl = "https://alt.invalid:4443"
				settings.DnsPumpHost = "pump.invalid"
				budget := settings.PlatformTransportBudget
				base := budget.StatsWithRoot().Root
				inner := budget.register(platformTransportBudgetH3Explicit, settings.H3BudgetByteCount, true)
				if !inner.TryAcquire() {
					t.Fatal("inner H3 admission failed")
				}
				defer inner.Release()
				transport := &PlatformTransport{settings: settings}
				// dialH3 admits translated inner modes before opening any socket.
				// Recreate that already-held ownership at this narrower boundary.
				var translation *platformTransportBudgetReservation
				innerBytes := settings.H3BudgetByteCount
				if mode != TransportModeH3 {
					var err error
					translation, err = transport.acquireH3TranslationMemory(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					defer translation.Release()
					innerBytes += kib(144)
				}
				outerBytes := kib(256)
				if carrier != ExtenderConnectModeTcpTls {
					outerBytes = kib(512 + 512 + 256 + 128)
					if carrier == ExtenderConnectModeDns {
						outerBytes += kib(144)
					}
				}
				assertOwnership := func(bytes ByteCount, slots int) {
					t.Helper()
					stats := budget.StatsWithRoot()
					if stats.Budget.TotalByteCount != mib(5) || stats.Root.TotalByteCount != mib(8) ||
						stats.Budget.UsedByteCount != bytes || stats.Root.UsedByteCount != base.UsedByteCount+bytes ||
						stats.Budget.UsedTransportCount != slots || stats.Root.UsedTransportCount != base.UsedTransportCount+slots {
						t.Errorf("want composed bytes=%d slots=%d, got %+v", bytes, slots, stats)
					}
				}
				marker := errors.New("outer socket admission observed")
				opened := 0
				observeOpen := func(ctx context.Context) {
					opened++
					if owner, _ := ctx.Value(extenderTransportSettingsContextKey{}).(*PlatformTransportSettings); owner != settings {
						t.Error("outer socket did not inherit the platform owner")
					}
					// Exact root bytes and one slot also reject the standalone
					// fallback's extra 256 KiB and second carrier slot.
					assertOwnership(innerBytes+outerBytes, 1)
				}
				socket := &extenderMemoryPacketConn{onClose: func() {
					assertOwnership(innerBytes+outerBytes, 1)
				}}
				connectSettings := DefaultConnectSettings()
				connectSettings.DialContextSettings = &DialContextSettings{
					DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
						observeOpen(ctx)
						return nil, marker
					},
					PacketConnFactory: func(ctx context.Context) (net.PacketConn, error) {
						observeOpen(ctx)
						return socket, marker
					},
				}
				transport.clientStrategy = &ClientStrategy{settings: &ClientStrategySettings{
					ConnectSettings: *connectSettings,
					ExtenderConfigs: []*ExtenderConfig{{
						Profile: ExtenderProfile{ConnectMode: carrier, ServerName: "spoof.invalid", Port: 443},
						Ip:      netip.MustParseAddr("192.0.2.1"),
					}},
				}}
				open := func() error {
					ctx := t.Context() // deliberately not transport.dialContext
					if ctx.Value(extenderTransportSettingsContextKey{}) != nil {
						t.Fatal("caller unexpectedly carried an extender owner")
					}
					conn, peer, pinned, err := transport.openH3PacketConn(ctx, mode, "platform.invalid", extenderPlaceholderUDPAddr(443))
					if conn != nil || peer != nil || pinned {
						t.Fatalf("failed dial returned a live endpoint: conn=%v peer=%v pinned=%t", conn, peer, pinned)
					}
					return err
				}
				// Exhaust only the 5 MiB device child. The process still has
				// room even for the incorrect root-only outer + 256 KiB claim.
				filler := budget.register(platformTransportBudgetExtender, mib(5)-innerBytes, false)
				if !filler.TryAcquire() {
					t.Fatal("could not fill the device child")
				}
				defer filler.Release()
				full := budget.StatsWithRoot()
				if full.Root.TotalByteCount-full.Root.UsedByteCount < outerBytes+kib(256) {
					t.Fatal("process root lacks headroom to distinguish a child escape")
				}
				if err := open(); !errors.Is(err, errExtenderMemoryBudget) || opened != 0 {
					t.Fatalf("full child reached an outer socket: opened=%d err=%v", opened, err)
				}
				if got := budget.StatsWithRoot(); got != full {
					t.Fatalf("refused outer dial changed ownership: before=%+v after=%+v", full, got)
				}
				filler.Release()
				if err := open(); !errors.Is(err, marker) || opened != 1 {
					t.Fatalf("admitted outer dial did not reach its socket: opened=%d err=%v", opened, err)
				}
				wantClosed := int32(0)
				if carrier != ExtenderConnectModeTcpTls {
					wantClosed = 1
				}
				if socket.closeCount.Load() != wantClosed {
					t.Fatalf("rejected packet endpoint close count=%d want=%d", socket.closeCount.Load(), wantClosed)
				}
				assertOwnership(innerBytes, 1)
				translation.Release()
				inner.Release()
				assertOwnership(0, 0)
				final := budget.StatsWithRoot()
				if final.Budget.ReservedByteCount != final.Budget.ReleasedByteCount ||
					final.Root.ReservedByteCount-base.ReservedByteCount != final.Root.ReleasedByteCount-base.ReleasedByteCount {
					t.Fatalf("outer failure or inner teardown retained a claim: %+v", final)
				}
			})
		}
	}
}
