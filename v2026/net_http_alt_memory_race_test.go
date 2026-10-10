package connect

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAltMemoryRaceRetainsBudgetRefusedFallback(t *testing.T) {
	for _, mode := range []string{"h3-family", "whodis-ports"} {
		for _, concurrent := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "/one-claim", true: "/two-claims"}[concurrent], func(t *testing.T) {
				const claimBytes = 4096
				capacity := ByteCount(claimBytes)
				if concurrent {
					capacity *= 2
				}
				budget := NewPlatformTransportBudget(capacity, 2)
				policy := extenderQuicMemoryPolicy{budget: budget, byteCount: claimBytes, usesSlot: true}
				candidates := []*net.UDPAddr{{IP: net.ParseIP("::1"), Port: 443}, {IP: net.ParseIP("127.0.0.1"), Port: 443}}
				if mode == "whodis-ports" {
					candidates = []*net.UDPAddr{{IP: net.ParseIP("127.0.0.1"), Port: 53}, {IP: net.ParseIP("127.0.0.1"), Port: 4053}}
				}
				var sockets, peak, opened atomic.Int32
				var firstClosed atomic.Bool
				ctx, cancel := context.WithTimeout(t.Context(), 4*platformH3FamilyRaceStagger+time.Second)
				defer cancel()
				start := time.Now()
				winner, err := raceAltQuicDial(ctx, candidates, policy, func(ctx context.Context, address *net.UDPAddr, claim *platformTransportBudgetReservation) (*h3DialAttempt, error) {
					if address == candidates[1] && !concurrent && !firstClosed.Load() {
						t.Error("fallback socket opened before previous claim closed")
					}
					// No socket exists until this candidate owns its complete claim.
					if got := budget.Stats().UsedByteCount; got < claimBytes {
						t.Error("socket opened without full claim")
					}
					raw, err := net.ListenPacket("udp4", "127.0.0.1:0")
					if err != nil {
						claim.Release()
						return nil, err
					}
					opened.Add(1)
					live := sockets.Add(1)
					for old := peak.Load(); old < live && !peak.CompareAndSwap(old, live); old = peak.Load() {
					}
					attempt := &h3DialAttempt{udpAddr: address, budgetReservation: claim, packetConn: &memoryRaceSocket{PacketConn: raw, onClose: func() {
						if budget.Stats().UsedByteCount < claimBytes {
							t.Error("claim released before socket")
						}
						sockets.Add(-1)
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
					t.Fatalf("fallback did not win: %v, %v", winner, err)
				}
				defer func() {
					if winner != nil {
						winner.close()
					}
				}()
				if !firstClosed.Load() || sockets.Load() != 1 || opened.Load() != 2 {
					t.Fatalf("loser not joined: closed=%v live=%d opens=%d", firstClosed.Load(), sockets.Load(), opened.Load())
				}
				if want := int32(1 + map[bool]int{false: 0, true: 1}[concurrent]); peak.Load() != want {
					t.Fatalf("peak sockets=%d, want %d", peak.Load(), want)
				}
				if time.Since(start) > 4*platformH3FamilyRaceStagger {
					t.Fatal("fallback remained behind blackhole")
				}
				winner.close()
				winner = nil
				stats := budget.Stats()
				if sockets.Load() != 0 || stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
					t.Fatalf("claims/sockets leaked: %+v live=%d", stats, sockets.Load())
				}
			})
		}
	}
}

type altMemoryRacePacketConn struct {
	net.PacketConn
	remote, visible *net.UDPAddr
	whodis          bool
	dropped         *atomic.Int32
	closeOnce       sync.Once
	onClose         func()
}

func (self *altMemoryRacePacketConn) WriteTo(b []byte, address net.Addr) (int, error) {
	udpAddress := address.(*net.UDPAddr)
	if self.whodis && udpAddress.Port == 53 || !self.whodis && udpAddress.IP.To4() == nil {
		self.dropped.Add(1)
		return len(b), nil // a live socket whose first candidate never answers
	}
	return self.PacketConn.WriteTo(b, self.remote)
}

func (self *altMemoryRacePacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, _, err := self.PacketConn.ReadFrom(b)
	return n, self.visible, err
}

func (self *altMemoryRacePacketConn) Close() error {
	err := self.PacketConn.Close()
	self.closeOnce.Do(self.onClose)
	return err
}

// Exercise the production resolver, candidate race, socket factory, DNS
// translation, QUIC handshake and HTTP request path together. The first
// candidate consumes its complete claim but drops every outgoing packet;
// only closing it can admit the actual loopback server behind the fallback.
func TestAltMemoryRaceLiveFallbackWithOnlyOneCarrierClaim(t *testing.T) {
	for _, whodis := range []bool{false, true} {
		name := "alt h3"
		if whodis {
			name = "alt whodis"
		}
		t.Run(name, func(t *testing.T) {
			server := newTestAltServer(t, whodis)
			remote, err := net.ResolveUDPAddr("udp4", strings.TrimPrefix(server.altUrl, "https://"))
			if err != nil {
				t.Fatal(err)
			}
			settings := DefaultClientStrategySettings()
			settings.EnableNormal = false
			settings.EnableResilient = false
			settings.ExpandExtenderProfileCount = 0
			settings.AltUrl = "https://alt-memory-race.test"
			settings.DnsTlds = [][]byte{[]byte(testAltDnsTld)}
			settings.ConnectSettings.TlsConfig = &tls.Config{RootCAs: server.rootCAs, MinVersion: tls.VersionTLS13}
			addresses := []netip.Addr{netip.MustParseAddr("127.0.0.1")}
			visible := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 4053}
			if !whodis {
				addresses = append(addresses, netip.MustParseAddr("::1"))
				visible.Port = 443
			}
			settings.ConnectSettings.Resolver = newFamilyTestResolver(t, addresses...)
			platformSettings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(20))
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			ctx = (&PlatformTransport{settings: platformSettings}).dialContext(ctx)
			policy := newExtenderQuicMemoryPolicy(ctx, &settings.ConnectSettings)
			if whodis {
				policy.packetTranslationSettings()
			}
			budget := NewPlatformTransportBudget(policy.byteCount, 1)
			platformSettings.PlatformTransportBudget = budget
			var opened, live, dropped atomic.Int32
			var firstClosed atomic.Bool
			settings.ConnectSettings.DialContextSettings = &DialContextSettings{
				PacketConnFactory: func(context.Context) (net.PacketConn, error) {
					if budget.Stats().UsedByteCount != policy.byteCount {
						t.Error("socket factory ran without the full carrier claim")
					}
					index := opened.Add(1)
					if index > 2 || index == 2 && !firstClosed.Load() {
						t.Error("fallback opened before the first socket closed")
					}
					raw, err := net.ListenPacket("udp4", "127.0.0.1:0")
					if err != nil {
						return nil, err
					}
					if live.Add(1) != 1 {
						t.Error("socket lifetime exceeded one full carrier claim")
					}
					return &altMemoryRacePacketConn{
						PacketConn: raw, remote: remote, visible: visible,
						whodis: whodis, dropped: &dropped,
						onClose: func() {
							if budget.Stats().UsedByteCount != policy.byteCount {
								t.Error("claim released before socket teardown")
							}
							live.Add(-1)
							if index == 1 {
								firstClosed.Store(true)
							}
						},
					}, nil
				},
			}
			strategy := NewClientStrategy(t.Context(), settings)
			t.Cleanup(strategy.Close)
			client := testAltDialer(t, strategy, name).HttpClient()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+testAltApiHost+"/hello", nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || string(body) != testAltBodyText {
				t.Fatalf("fallback response = %q, %v", body, err)
			}
			if !firstClosed.Load() || opened.Load() != 2 || live.Load() != 1 || dropped.Load() == 0 {
				t.Fatalf("race evidence: firstClosed=%v opened=%d live=%d dropped=%d", firstClosed.Load(), opened.Load(), live.Load(), dropped.Load())
			}
			client.Transport.(*altQuicBoundedTransport).Close()
			deadline := time.Now().Add(2 * time.Second)
			for (live.Load() != 0 || budget.Stats().UsedByteCount != 0) && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			stats := budget.Stats()
			if live.Load() != 0 || stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
				t.Fatalf("live fallback cleanup leaked: sockets=%d budget=%+v", live.Load(), stats)
			}
		})
	}
}

func TestAltMemoryRaceRefusesBeforeDialAndReturnsCancellation(t *testing.T) {
	budget := NewPlatformTransportBudget(1, 1)
	policy := extenderQuicMemoryPolicy{budget: budget, byteCount: 2, usesSlot: true}
	candidates := []*net.UDPAddr{{IP: net.ParseIP("127.0.0.1"), Port: 443}}
	opened := false
	_, err := raceAltQuicDial(t.Context(), candidates, policy, func(context.Context, *net.UDPAddr, *platformTransportBudgetReservation) (*h3DialAttempt, error) {
		opened = true
		return nil, errors.New("must not dial")
	})
	if !errors.Is(err, errExtenderMemoryBudget) {
		t.Fatal(err)
	}
	if opened {
		t.Fatal("budget refusal opened a socket")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = raceAltQuicDial(ctx, candidates, policy, func(context.Context, *net.UDPAddr, *platformTransportBudgetReservation) (*h3DialAttempt, error) {
		t.Error("canceled race opened a socket")
		return nil, errors.New("must not dial")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
}
