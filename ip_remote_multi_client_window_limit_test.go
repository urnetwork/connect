package connect

import (
	"context"
	"maps"
	"net/netip"
	"reflect"
	"testing"
	"time"
)

// Every healthy allocation of the shared capacity must satisfy at least one
// default class; neither a zero-provider ready flag nor a larger owner is used.
func TestDefaultWindowLimitPreservesFeasibleClassSplit(t *testing.T) {
	settings := DefaultMultiClientSettings()
	settings.WindowSizes = map[WindowType]WindowSizeSettings{
		WindowTypeQuality: {WindowSizeMin: 10, WindowSizeMax: 10, WindowSizeHardMax: 10, WindowSizeReconnectScale: 1},
		WindowTypeSpeed:   {WindowSizeMin: 2, WindowSizeMax: 2, WindowSizeHardMax: 2, FixedWindowSize: 2, WindowSizeReconnectScale: 1},
	}
	before := maps.Clone(settings.WindowSizes)
	for limit := 1; limit <= 12; limit++ {
		quality := (&multiClientWindow{settings: settings, windowType: WindowTypeQuality, windowSizeLimit: limit}).defaultWindowSizeWithLock()
		speed := (&multiClientWindow{settings: settings, windowType: WindowTypeSpeed, windowSizeLimit: limit}).defaultWindowSizeWithLock()
		if quality.WindowSizeMin <= 0 || speed.FixedWindowSize <= 0 ||
			quality.WindowSizeMax != min(10, limit) || speed.FixedWindowSize != min(2, limit) {
			t.Fatalf("limit %d changed diversity or created an empty minimum: quality=%+v speed=%+v", limit, quality, speed)
		}
		for qualityCount := 0; qualityCount <= limit; qualityCount++ {
			speedCount := limit - qualityCount
			qualityReady := windowMinSatisfied(quality.WindowSizeMin, qualityCount, 0, false, true, quality.WindowSizeHardMax)
			speedReady := windowMinSatisfied(speed.FixedWindowSize, speedCount, 0, false, true, speed.WindowSizeHardMax)
			if !qualityReady && !speedReady {
				t.Fatalf("limit %d stranded healthy allocation %d/%d", limit, qualityCount, speedCount)
			}
		}
		if windowMinSatisfied(quality.WindowSizeMin, 0, 0, false, true, quality.WindowSizeHardMax) ||
			windowMinSatisfied(speed.FixedWindowSize, 0, 0, false, true, speed.WindowSizeHardMax) {
			t.Fatal("no healthy provider became ready")
		}
		if limit >= 11 && (!reflect.DeepEqual(quality, before[WindowTypeQuality]) ||
			!reflect.DeepEqual(speed, before[WindowTypeSpeed])) {
			t.Fatal("sufficient capacity did not preserve original class settings")
		}
	}
	if !reflect.DeepEqual(settings.WindowSizes, before) {
		t.Fatal("capacity sizing mutated caller settings")
	}
}

// The real setter publishes one synchronous state change to each owned window.
// Returning to zero restores the original values instead of scaling a copy twice.
func TestDefaultWindowLimitNotifiesAndKeepsOwnersIndependent(t *testing.T) {
	settings := DefaultMultiClientSettings()
	makeOwner := func() *RemoteUserNatMultiClient {
		return &RemoteUserNatMultiClient{windows: map[WindowType]*multiClientWindow{
			WindowTypeQuality: {settings: settings, windowType: WindowTypeQuality, resizeMonitor: NewMonitor()},
			WindowTypeSpeed:   {settings: settings, windowType: WindowTypeSpeed, resizeMonitor: NewMonitor()},
		}}
	}
	first, second := makeOwner(), makeOwner()
	changes := make(map[WindowType]<-chan struct{})
	for kind, window := range first.windows {
		changes[kind] = window.resizeMonitor.NotifyChannel()
	}
	first.SetWindowSizeLimit(1)
	for kind, window := range first.windows {
		if window.windowSizeLimit != 1 {
			t.Fatal("live window did not receive its limit")
		}
		select {
		case <-changes[kind]:
		default:
			t.Fatal("changed live window did not notify")
		}
	}
	for _, window := range second.windows {
		if window.windowSizeLimit != 0 || window.defaultWindowSizeWithLock() != settings.WindowSizes[window.windowType] {
			t.Fatal("one owner changed a second owner sharing immutable defaults")
		}
	}
	for _, window := range first.windows {
		changed := window.resizeMonitor.NotifyChannel()
		first.SetWindowSizeLimit(1)
		select {
		case <-changed:
			t.Fatal("unchanged limit generated a wake cycle")
		default:
		}
	}
	first.SetWindowSizeLimit(0)
	for _, window := range first.windows {
		if window.defaultWindowSizeWithLock() != settings.WindowSizes[window.windowType] {
			t.Fatal("return to H1/Auto did not restore the original size")
		}
	}
}

// Compare the composition query with the real constructor's registered atomic
// reservation, including nested DNS/extender graphs. Canceled ownership prevents
// any dial; this is constructor accounting, not a live transport assertion.
func TestH3WindowClaimMatchesActualCarrierReservation(t *testing.T) {
	for _, target := range []ByteCount{32 * 1024 * 1024, 64 * 1024 * 1024} {
		for _, mode := range []TransportMode{TransportModeH3, TransportModeH3Dns, TransportModeH3DnsPump} {
			for _, outer := range []ExtenderConnectMode{"", ExtenderConnectModeTcpTls, ExtenderConnectModeQuic, ExtenderConnectModeDns} {
				settings := DefaultPlatformTransportSettingsWithMemoryTarget(target)
				ApplyMobilePlatformTransportMemoryPolicy(settings, target)
				settings.StartDisabled = true
				settings.Log = NewNoopLogger()
				settings.DnsPumpHost = "pump.example"
				strategySettings := DefaultClientStrategySettings()
				strategySettings.EnableNormal = outer == ""
				strategySettings.EnableResilient = false
				strategySettings.ExtenderConfigs = nil
				if outer != "" {
					strategySettings.ExtenderConfigs = []*ExtenderConfig{{
						Profile: ExtenderProfile{ConnectMode: outer, ServerName: "outer.example", Port: 443},
						Ip:      netip.MustParseAddr("192.0.2.1"),
					}}
				}
				strategy := &ClientStrategy{settings: strategySettings}
				before := settings.PlatformTransportBudget.Stats()
				claim := settings.H3MemoryReservationByteCount(mode, strategy)
				if after := settings.PlatformTransportBudget.Stats(); after != before {
					t.Fatal("sizing query consumed owner capacity")
				}
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				carrier := NewPlatformTransportWithTargetMode(ctx, strategy, nil,
					"wss://carrier.example", &ClientAuth{}, mode, settings)
				if carrier.h3BudgetReservation == nil || carrier.h3BudgetReservation.byteCount != claim {
					t.Fatalf("target=%d mode=%s outer=%s query=%d disagrees with constructor", target, mode, outer, claim)
				}
				join, stop := context.WithTimeout(context.Background(), 5*time.Second)
				err := carrier.CloseAndWait(join)
				stop()
				if err != nil {
					t.Fatal(err)
				}
				if after := settings.PlatformTransportBudget.Stats(); after.UsedByteCount != 0 || after.UsedTransportCount != 0 {
					t.Fatal("canceled constructor retained a carrier claim")
				}
			}
		}
	}
}
