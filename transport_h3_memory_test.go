package connect

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	quic "github.com/quic-go/quic-go"
	"net"
	"testing"
	"time"
)

func TestPlatformH3MobileComposedLedger(t *testing.T) {
	if got := platformH3FixedMemoryByteCount(); got != kib(1600) {
		t.Fatalf("fixed H3 ledger = %d, want 1600 KiB", got)
	}
	if actual := kib(256) + platformH3ControlBytes + 2*platformH3SocketByteCount + platformH3DatagramQueueBytes; platformH3DialTransientByteCount-actual != kib(160) {
		t.Fatal("pre-auth receive, DATAGRAM, socket, and control state escaped the transient lease")
	}
	for _, c := range []struct {
		target, process, claim, stream, connection, total ByteCount
	}{
		{mib(20), mib(32), kib(3072), kib(1104), kib(1472), mib(5)},
		{mib(28), mib(40), kib(5184), kib(2688), kib(3584), mib(7)},
	} {
		t.Run(fmt.Sprint(c.target), func(t *testing.T) {
			old := MemoryBudget()
			defer SetMemoryBudget(old)
			SetMemoryBudget(c.process)
			settings := DefaultPlatformTransportSettingsWithMemoryTarget(c.target)
			config := newPlatformQuicConfig(settings, 1)
			if config.Allow0RTT {
				t.Fatal("mobile retained-send policy permits untracked early data")
			}
			if config.InitialStreamReceiveWindow != uint64(kib(128)) || config.InitialConnectionReceiveWindow != uint64(kib(256)) {
				t.Fatal("speculative dial can grow beyond the transient receive envelope before an application read")
			}
			if settings.H3BudgetByteCount != c.claim || config.MaxStreamReceiveWindow != uint64(c.stream) ||
				config.MaxConnectionReceiveWindow != uint64(c.connection) || c.connection+platformH3FixedMemoryByteCount() != c.claim {
				t.Fatalf("composed H3 claim=%d stream=%d conn=%d", settings.H3BudgetByteCount, config.MaxStreamReceiveWindow, config.MaxConnectionReceiveWindow)
			}
			if settings.PlatformTransportBudget.Stats().TotalByteCount != c.total ||
				settings.H3SocketReadBufferByteCount != kib(64) || settings.H3SocketWriteBufferByteCount != kib(64) {
				t.Fatal("child/socket policy escaped the profile")
			}
			wantRaceSlack := kib(96)
			if c.target == mib(28) {
				wantRaceSlack = kib(32)
			}
			if c.total-(c.claim+2*kib(144)+platformH3DialTransientByteCount+settings.H1BudgetByteCount) != wantRaceSlack {
				t.Fatal("two DNS candidates plus pending H1 do not fit the exact profile")
			}
			if transport := (&PlatformTransport{settings: settings}); transport.h3TransportBufferSize() != 4 || settings.TransportBufferSize != 32 {
				t.Fatal("H3 route depth must be four without reducing H1's depth")
			}
			datagrams := settings.H3DatagramSettings
			if datagrams.MaxMessageByteCount != 8192 || datagrams.MaxFragmentCount != 1 || datagrams.MaxReassemblyMessageCount != 32 ||
				datagrams.MaxReassemblyByteCount != 64*1024 || datagrams.ProcessReassemblyByteCount != 64*1024 {
				t.Fatalf("DATAGRAM ownership escaped its ledger: %+v", datagrams)
			}
			if want := ByteCount((32 + 128 + 1) * 2048); platformH3DatagramQueueBytes < want+kib(30) {
				t.Fatal("quic-go's send/receive DATAGRAM roots and descriptors are not fully charged")
			}
			// Route ownership (both directions), dispatcher/read/pending holds,
			// hybrid retained queue, and batch scratch fit the separate row.
			root := retainedMessageCapacity(8192)
			app := ByteCount(2*platformH3MemoryQueueCount+5)*root + H3HybridStreamQueueByteCount + platformH3WriteBatchMaxByteCount
			if platformH3ApplicationQueueBytes < app {
				t.Fatalf("application ownership=%d escaped row=%d", app, platformH3ApplicationQueueBytes)
			}
		})
	}
}

func TestH3DatagramFlightFullFallsBackWithoutErrorOrBusyRetry(t *testing.T) {
	for _, afterMtuFeedback := range []bool{false, true} {
		t.Run(fmt.Sprint(afterMtuFeedback), func(t *testing.T) {
			stats := &H3DatagramStats{}
			fragmenter, err := NewH3DatagramFragmenter(DefaultH3DatagramSettings(), stats)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			stream, next, err := fragmenter.SendHybrid(make([]byte, 500), 1360, func([]byte) error {
				calls++
				if afterMtuFeedback && calls == 1 {
					return &quic.DatagramTooLargeError{MaxDatagramPayloadSize: 1100}
				}
				return errQuicDatagramFlightFull
			})
			wantCalls, wantMax := 1, 1360
			if afterMtuFeedback {
				wantCalls, wantMax = 2, 1100
			}
			if !stream || err != nil || calls != wantCalls || next != wantMax || stats.Snapshot().SendErrorCount != 0 {
				t.Fatalf("flight-full disposition stream=%t max=%d calls=%d err=%v stats=%+v", stream, next, calls, err, stats.Snapshot())
			}
		})
	}
}

// This is an admission/lifetime matrix, not a throughput claim. Every arm
// first acquires its inner carrier and a pending sibling H1, then acquires the
// actual translation and outer policy. Live payload tests accompany it.
func TestPlatformMobileNestedCarrierAdmissionMatrix(t *testing.T) {
	old := MemoryBudget()
	defer SetMemoryBudget(old)
	for _, profile := range []struct{ target, process ByteCount }{{mib(20), mib(32)}, {mib(28), mib(40)}} {
		for _, mode := range []TransportMode{TransportModeH1, TransportModeH3, TransportModeH3Dns, TransportModeH3DnsPump} {
			for _, carrier := range []string{"direct", ExtenderCarrierTcp, ExtenderCarrierQuic, ExtenderCarrierDns} {
				t.Run(fmt.Sprintf("%d/%s/%s", profile.target, mode, carrier), func(t *testing.T) {
					SetMemoryBudget(profile.process)
					settings := DefaultPlatformTransportSettingsWithMemoryTarget(profile.target)
					settings.AltUrl = "https://127.0.0.1:443"
					settings.DnsPumpHost = "127.0.0.1"
					transport := newTestAltTransport(t, settings)
					ctx := transport.dialContext(t.Context())
					budget := settings.PlatformTransportBudget
					root := DefaultPlatformTransportBudget()
					base := root.Stats().UsedByteCount
					class, innerBytes := platformTransportBudgetH3, settings.H3BudgetByteCount
					if mode == TransportModeH1 {
						class, innerBytes = platformTransportBudgetH1, settings.H1BudgetByteCount
					}
					inner := budget.register(class, innerBytes, true)
					if !inner.TryAcquire() {
						t.Fatal("inner carrier did not fit")
					}
					defer inner.Release()
					sibling := budget.register(platformTransportBudgetH1, settings.H1BudgetByteCount, true)
					defer sibling.Release()
					var translated net.PacketConn
					if mode == TransportModeH3Dns || mode == TransportModeH3DnsPump {
						_, wrap, err := transport.h3DialCandidates(ctx, mode, "127.0.0.1")
						if err != nil {
							t.Fatal(err)
						}
						socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
						if err != nil {
							t.Fatal(err)
						}
						translated, err = wrap(ctx, socket)
						if err != nil {
							socket.Close()
							t.Fatal(err)
						}
						defer translated.Close()
					}
					var outer *platformTransportBudgetReservation
					var err error
					if carrier == ExtenderCarrierTcp {
						outer, err = acquireExtenderTcpMemory(ctx)
					} else if carrier != "direct" {
						policy := newExtenderQuicMemoryPolicy(ctx, DefaultConnectSettings())
						if carrier == ExtenderCarrierDns {
							policy.packetTranslationSettings()
						}
						outer, err = policy.acquire(ctx)
					}
					if err != nil {
						t.Fatalf("complete inner/outer graph did not fit: %v", err)
					}
					defer outer.Release()
					if !sibling.TryAcquire() {
						t.Fatal("outer graph stole the pending sibling H1")
					}
					stats := budget.Stats()
					if stats.TotalByteCount < stats.UsedByteCount || root.Stats().UsedByteCount != base+stats.UsedByteCount {
						t.Fatalf("child/process accounting mismatch: child=%+v root=%+v", stats, root.Stats())
					}
					if (mode == TransportModeH3Dns || mode == TransportModeH3DnsPump) && carrier == ExtenderCarrierDns {
						wantSlack := kib(96)
						if profile.target == mib(28) {
							wantSlack = kib(32)
						}
						if stats.TotalByteCount-stats.UsedByteCount != wantSlack {
							t.Fatalf("nested graph slack=%d want=%d", stats.TotalByteCount-stats.UsedByteCount, wantSlack)
						}
					}
					outer.Release()
					if translated != nil {
						translated.Close()
					}
					sibling.Release()
					inner.Release()
					stats = budget.Stats()
					if stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount || root.Stats().UsedByteCount != base {
						t.Fatalf("teardown imbalance: child=%+v root=%+v", stats, root.Stats())
					}
				})
			}
		}
	}
}

func TestPlatformDnsMemoryRefusalDoesNotCreateTranslation(t *testing.T) {
	settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(20))
	settings.AltUrl = "https://127.0.0.1:443"
	settings.PlatformTransportBudget = NewPlatformTransportBudget(settings.H3BudgetByteCount, 1)
	inner := settings.PlatformTransportBudget.register(platformTransportBudgetH3, settings.H3BudgetByteCount, true)
	if !inner.TryAcquire() {
		t.Fatal("inner admission")
	}
	defer inner.Release()
	transport := newTestAltTransport(t, settings)
	_, wrap, err := transport.h3DialCandidates(t.Context(), TransportModeH3Dns, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	socket := &extenderMemoryPacketConn{}
	if conn, err := wrap(t.Context(), socket); conn != nil || !errors.Is(err, errExtenderMemoryBudget) {
		t.Fatalf("full budget created translation: %v %v", conn, err)
	}
	if socket.closeCount.Load() != 0 || settings.PlatformTransportBudget.Stats().UsedByteCount != settings.H3BudgetByteCount {
		t.Fatal("refusal transferred socket ownership or changed inner reservation")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if conn, err := wrap(ctx, socket); conn != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled translation=%v %v", conn, err)
	}
}

func TestPlatformDnsTranslationAdmissionPrecedesEverySocket(t *testing.T) {
	for _, mode := range []TransportMode{TransportModeH3Dns, TransportModeH3DnsPump} {
		t.Run(string(mode), func(t *testing.T) {
			settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(20))
			settings.AltUrl = "https://127.0.0.1:443"
			settings.DnsPumpHost = "127.0.0.1"
			settings.PlatformTransportBudget = NewPlatformTransportBudget(settings.H3BudgetByteCount, 1)
			inner := settings.PlatformTransportBudget.register(platformTransportBudgetH3Explicit, settings.H3BudgetByteCount, true)
			if !inner.TryAcquire() {
				t.Fatal("inner admission")
			}
			defer inner.Release()
			opened := 0
			marker := errors.New("socket factory marker")
			settings.H3PacketConnFactory = func(context.Context) (net.PacketConn, error) {
				opened++
				if got := settings.PlatformTransportBudget.Stats().UsedByteCount; got != settings.H3BudgetByteCount+kib(144) {
					t.Errorf("socket opened before translation admission: %d", got)
				}
				return nil, marker
			}
			transport := newTestAltTransport(t, settings)
			addresses, wrap, err := transport.h3DialCandidates(t.Context(), mode, "127.0.0.1")
			if err != nil {
				t.Fatal(err)
			}
			dial := func(ctx context.Context) error {
				_, err := transport.dialH3(ctx, mode, "127.0.0.1", addresses[0], wrap, &tls.Config{}, newPlatformQuicConfig(settings, 1), 1, false)
				return err
			}
			if err := dial(t.Context()); !errors.Is(err, errExtenderMemoryBudget) || opened != 0 {
				t.Fatalf("refused translation opened socket: %d %v", opened, err)
			}
			inner.Release()
			settings.PlatformTransportBudget = NewPlatformTransportBudget(settings.H3BudgetByteCount+kib(144), 1)
			inner = settings.PlatformTransportBudget.register(platformTransportBudgetH3Explicit, settings.H3BudgetByteCount, true)
			if !inner.TryAcquire() {
				t.Fatal("second inner admission")
			}
			defer inner.Release()
			if err := dial(t.Context()); !errors.Is(err, marker) || opened != 1 {
				t.Fatalf("admitted translation did not reach factory: %d %v", opened, err)
			}
			if settings.PlatformTransportBudget.Stats().UsedByteCount != settings.H3BudgetByteCount {
				t.Fatal("socket failure retained translation claim")
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := dial(ctx); !errors.Is(err, context.Canceled) || opened != 1 {
				t.Fatalf("canceled translation opened socket: %d %v", opened, err)
			}
		})
	}
}

func TestMobileAltApiCoexistsWithDeviceCarrierAndReleases(t *testing.T) {
	old := MemoryBudget()
	defer SetMemoryBudget(old)
	SetMemoryBudget(mib(32))
	for _, whodis := range []bool{false, true} {
		t.Run(fmt.Sprint(whodis), func(t *testing.T) {
			settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(20))
			inner := settings.PlatformTransportBudget.register(platformTransportBudgetH3, settings.H3BudgetByteCount, true)
			if !inner.TryAcquire() {
				t.Fatal("device admission")
			}
			defer inner.Release()
			root := DefaultPlatformTransportBudget()
			base := root.Stats().UsedByteCount
			server := newTestAltServer(t, whodis)
			strategy := newTestAltStrategy(t, server)
			name := "alt h3"
			if whodis {
				name = "alt whodis"
			}
			if body := testAltGet(t, testAltDialer(t, strategy, name)); body != testAltBodyText {
				t.Fatal(body)
			}
			policy := newExtenderQuicMemoryPolicy(t.Context(), DefaultConnectSettings())
			if whodis {
				policy.packetTranslationSettings()
			}
			if got := root.Stats().UsedByteCount; got != base+policy.byteCount {
				t.Fatalf("live %s claim=%d want=%d", name, got, base+policy.byteCount)
			}
			strategy.Close()
			deadline := time.Now().Add(5 * time.Second)
			for root.Stats().UsedByteCount != base && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if root.Stats().UsedByteCount != base {
				t.Fatalf("alt close leaked process claim: %+v", root.Stats())
			}
		})
	}
}
