package connect

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type memoryRaceSocket struct {
	net.PacketConn
	onClose func()
}

func (self *memoryRaceSocket) Close() error {
	err := self.PacketConn.Close()
	self.onClose()
	return err
}

func TestPlatformH3MemoryRaceAdmitsEveryConcurrentSocketAndClosesLoser(t *testing.T) {
	old := MemoryBudget()
	defer SetMemoryBudget(old)
	SetMemoryBudget(mib(32))
	for _, mode := range []TransportMode{TransportModeH3, TransportModeH3Dns} {
		t.Run(string(mode), func(t *testing.T) {
			settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(20))
			settings.AltUrl = "https://127.0.0.1:443"
			transport := newTestAltTransport(t, settings)
			budget := settings.PlatformTransportBudget
			root := DefaultPlatformTransportBudget()
			base := root.Stats().UsedByteCount
			inner := budget.register(platformTransportBudgetH3Explicit, settings.H3BudgetByteCount, true)
			if !inner.TryAcquire() {
				t.Fatal("inner admission")
			}
			defer inner.Release()
			sibling := budget.register(platformTransportBudgetH1, settings.H1BudgetByteCount, true)
			defer sibling.Release()
			_, wrap, err := transport.h3DialCandidates(t.Context(), mode, "127.0.0.1")
			if err != nil {
				t.Fatal(err)
			}
			candidates := []*net.UDPAddr{{IP: net.ParseIP("::1"), Port: 443}, {IP: net.ParseIP("127.0.0.1"), Port: 443}}
			if mode == TransportModeH3Dns {
				candidates = []*net.UDPAddr{{IP: net.ParseIP("127.0.0.1"), Port: 53}, {IP: net.ParseIP("127.0.0.1"), Port: 4053}}
			}
			var sockets, peakSockets atomic.Int32
			var peakBytes atomic.Int64
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			winner, err := raceH3DialWithMemory(ctx, candidates, budget, func(ctx context.Context, address *net.UDPAddr) (*h3DialAttempt, error) {
				if address == candidates[1] && budget.Stats().UsedByteCount < settings.H3BudgetByteCount+platformH3DialTransientByteCount {
					t.Error("second socket opened before its transient claim")
				}
				network, local := "udp4", "127.0.0.1:0"
				if address.IP.To4() == nil {
					network, local = "udp6", "[::1]:0"
				}
				raw, err := net.ListenPacket(network, local)
				if err != nil {
					return nil, err
				}
				n := sockets.Add(1)
				for current := peakSockets.Load(); current < n && !peakSockets.CompareAndSwap(current, n); current = peakSockets.Load() {
				}
				owned := &memoryRaceSocket{PacketConn: raw, onClose: func() { sockets.Add(-1) }}
				conn, err := wrap(ctx, owned)
				if err != nil {
					owned.Close()
					return nil, err
				}
				used := int64(budget.Stats().UsedByteCount)
				for current := peakBytes.Load(); current < used && !peakBytes.CompareAndSwap(current, used); current = peakBytes.Load() {
				}
				attempt := &h3DialAttempt{udpAddr: address, packetConn: conn}
				if address == candidates[0] {
					<-ctx.Done()
					return attempt, ctx.Err()
				}
				return attempt, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if winner.udpAddr != candidates[1] || peakSockets.Load() != 2 || sockets.Load() != 1 {
				t.Fatalf("race identity/cleanup: winner=%v peak=%d live=%d", winner.udpAddr, peakSockets.Load(), sockets.Load())
			}
			wantPeak := settings.H3BudgetByteCount + platformH3DialTransientByteCount
			wantLive := settings.H3BudgetByteCount
			if mode == TransportModeH3Dns {
				wantPeak += 2 * kib(144)
				wantLive += kib(144)
			}
			if peakBytes.Load() != int64(wantPeak) || budget.Stats().UsedByteCount != wantLive || root.Stats().UsedByteCount != base+wantLive {
				t.Fatalf("race peak=%d live=%+v root=%+v", peakBytes.Load(), budget.Stats(), root.Stats())
			}
			winner.close()
			inner.Release()
			sibling.Release()
			if sockets.Load() != 0 || budget.Stats().UsedByteCount != 0 || root.Stats().UsedByteCount != base {
				t.Fatal("race teardown leaked socket/claim")
			}
		})
	}
}

func TestPlatformH3MemoryRaceFullTransientCancelsBlackholeBeforeFallback(t *testing.T) {
	budget := NewPlatformTransportBudget(mib(3), 1)
	inner := budget.register(platformTransportBudgetH3Explicit, mib(3), true)
	if !inner.TryAcquire() {
		t.Fatal("inner admission")
	}
	defer inner.Release()
	candidates := []*net.UDPAddr{{IP: net.ParseIP("127.0.0.1"), Port: 53}, {IP: net.ParseIP("127.0.0.1"), Port: 4053}}
	var firstClosed atomic.Bool
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := time.Now()
	winner, err := raceH3DialWithMemory(ctx, candidates, budget, func(ctx context.Context, address *net.UDPAddr) (*h3DialAttempt, error) {
		if address == candidates[1] && !firstClosed.Load() {
			t.Error("fallback reused the base claim before blackhole closed")
		}
		socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		attempt := &h3DialAttempt{udpAddr: address, packetConn: &memoryRaceSocket{PacketConn: socket, onClose: func() {
			if address == candidates[0] {
				firstClosed.Store(true)
			}
		}}}
		if address == candidates[0] {
			<-ctx.Done()
			return attempt, ctx.Err()
		}
		return attempt, nil
	})
	if err != nil || winner == nil || winner.udpAddr != candidates[1] {
		t.Fatalf("deferred fallback=%v %v", winner, err)
	}
	defer winner.close()
	if time.Since(start) > 4*platformH3FamilyRaceStagger || budget.Stats().UsedByteCount != mib(3) {
		t.Fatal("fallback stalled or escaped base claim")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = raceH3DialWithMemory(canceled, candidates[:1], budget, func(ctx context.Context, _ *net.UDPAddr) (*h3DialAttempt, error) { return nil, ctx.Err() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled race=%v", err)
	}
}
