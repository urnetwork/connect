package connect

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	quic "github.com/urnetwork/quic-go"
	"net"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"
)

// The phone sizes and what each ledger row charges for them, row by row, from
// the configured sizes alone (server LBALTOPT2 R20). Before R20 the rows were
// fixed at 1600 KiB, and the 20-MiB claim was 3 MiB with 1472/1104 KiB of
// connection/stream credit; 28 MiB's was 5184 KiB.
func TestPlatformH3MobileComposedLedger(t *testing.T) {
	const (
		claim     = ByteCount(923532)
		fixed     = claim - 256*1024
		transient = ByteCount(665280)
	)
	for _, c := range []struct {
		target, process, total ByteCount
	}{
		{mib(20), mib(32), mib(5)},
		{mib(28), mib(40), mib(7)},
	} {
		t.Run(fmt.Sprint(c.target), func(t *testing.T) {
			old := MemoryBudget()
			defer SetMemoryBudget(old)
			SetMemoryBudget(c.process)
			settings := testingDefaultPlatformTransportSettingsWithMemoryTarget(c.target)
			config := newPlatformQuicConfig(settings, 1)
			if config.Allow0RTT {
				t.Fatal("mobile retained-send policy permits untracked early data")
			}
			if config.InitialStreamReceiveWindow != uint64(kib(128)) || config.InitialConnectionReceiveWindow != uint64(kib(256)) ||
				config.MaxStreamReceiveWindow != uint64(kib(192)) || config.MaxConnectionReceiveWindow != uint64(kib(256)) {
				t.Fatalf("receive credit escaped the phone sizes: %+v", config)
			}
			if config.MaxUnprocessedPackets != 32 || config.MaxDatagramSendQueueLen != 8 || config.MaxDatagramReceiveQueueLen != 32 {
				t.Fatalf("quic-go's queues escaped the phone sizes: unprocessed=%d datagram send=%d receive=%d",
					config.MaxUnprocessedPackets, config.MaxDatagramSendQueueLen, config.MaxDatagramReceiveQueueLen)
			}
			if settings.H3SocketReadBufferByteCount != kib(32) || settings.H3SocketWriteBufferByteCount != kib(16) ||
				settings.H3SendFlightFrameCount != 48 || settings.H3StreamWriteBatchMaxByteCount != 16*1024 ||
				settings.PlatformTransportBudget.Stats().TotalByteCount != c.total {
				t.Fatal("socket, send or child policy escaped the phone sizes")
			}
			if transport := (&PlatformTransport{settings: settings}); transport.h3TransportBufferSize() != 4 || settings.TransportBufferSize != 32 {
				t.Fatal("H3 route depth must be four without reducing H1's depth")
			}
			datagrams := settings.H3DatagramSettings
			if datagrams.MaxMessageByteCount != 8192 || datagrams.MaxFragmentCount != 1 || datagrams.MaxReassemblyMessageCount != 8 ||
				datagrams.MaxReassemblyByteCount != 16*1024 || datagrams.ProcessReassemblyByteCount != 16*1024 ||
				datagrams.HybridStreamQueueMessageCount != 4 || datagrams.HybridStreamQueuePayloadByteCount != 32*1024 {
				t.Fatalf("DATAGRAM ownership escaped the phone sizes: %+v", datagrams)
			}
			// the stream lane's largest message, the framer's 16 KiB, is never
			// refused as larger than the whole hybrid stream queue
			if ByteCount(datagrams.HybridStreamQueueByteCount()) < retainedMessageCapacity(kib(16)) {
				t.Fatal("the hybrid stream queue refuses the stream lane's largest message")
			}

			// each row, from the sizes alone
			ledger := settings.h3Ledger()
			flightTables := ByteCount(48+8)*ByteCount(unsafe.Sizeof(quicSendFlightFrame{})) +
				256*ByteCount(unsafe.Sizeof(quicSendFlightPacket{})) + 8*ByteCount(unsafe.Sizeof(uintptr(0)))
			for _, row := range []struct {
				name      string
				got, want ByteCount
			}{
				{"sockets", ledger.sockets, kib(32) + kib(16)},
				{"STREAM roots", ledger.sendRoots, 48 * kib(2)},
				{"send state", ledger.sendState, (48+8+16)*192 + flightTables + kib(8)},
				{"DATAGRAM queues", ledger.datagrams, (8+32+1)*kib(2) + (8+32)*192},
				{"reassembly", ledger.reassembly, kib(16) + kib(16) + 8*512},
				{"unprocessed packets and connection state", ledger.control, 32*kib(2) + kib(128)},
				{"application", ledger.application, 13*(8192+12) + (32*1024 + 4*12) + kib(16) + kib(4)},
			} {
				if row.got != row.want {
					t.Errorf("%s row %d, want %d", row.name, row.got, row.want)
				}
			}
			if ledger.total() != fixed || settings.H3BudgetByteCount != claim ||
				settings.H3MemoryReservationByteCount(TransportModeH3, nil) != claim {
				t.Fatalf("claim %d (fixed %d), want %d (fixed %d)", settings.H3BudgetByteCount, ledger.total(), claim, fixed)
			}
			// a racing candidate holds its initial credit, sockets, quic-go's
			// queues and state, the reassembler and the send state, but no
			// STREAM roots or routes
			if got := settings.h3DialTransientByteCount(); got != transient ||
				got != kib(256)+ledger.sockets+ledger.control+ledger.datagrams+ledger.reassembly+ledger.sendState {
				t.Fatalf("dial transient %d, want %d", got, transient)
			}
			if c.total < claim+2*kib(144)+transient+settings.H1BudgetByteCount {
				t.Fatal("one claim, two DNS candidates, the transient dial and a pending H1 do not fit the profile")
			}
		})
	}
}

// The retained-send controller of a platform H3 dial takes the settings'
// limits: its STREAM roots, and as many DATAGRAM roots as quic-go's send
// queue, with the frame table sized for them.
func TestPlatformH3SendFlightTakesSettingsLimits(t *testing.T) {
	settings := testingDefaultPlatformTransportSettingsWithMemoryTarget(mib(20))
	ApplyMobilePlatformTransportMemoryPolicy(settings, mib(20))
	config := newPlatformQuicConfig(settings, 1)
	installQuicSendFlightWithLimits(config, settings.h3SendFlightLimits())
	trace, ok := config.Tracer(t.Context(), true, quic.ConnectionID{}).(*quicSendFlightTrace)
	if !ok {
		t.Fatal("the dial's tracer does not own a send flight")
	}
	flight := trace.flight
	if flight.limits.frames != 48 || flight.limits.datagrams != 8 || len(flight.frames) != 48+8 || flight.limits.writeLimit() != 23 {
		t.Fatalf("flight limits %+v with %d frame slots", flight.limits, len(flight.frames))
	}
	for i := range 8 {
		if err := flight.trySendDatagram(func([]byte) error { return nil }, []byte{byte(i)}); err != nil {
			t.Fatalf("DATAGRAM root %d refused: %v", i, err)
		}
	}
	if err := flight.trySendDatagram(func([]byte) error { return nil }, []byte{8}); !errors.Is(err, errQuicDatagramFlightFull) {
		t.Fatalf("a ninth DATAGRAM root was admitted: %v", err)
	}

	// unset limits keep the defaults
	defaults := (&PlatformTransportSettings{}).h3SendFlightLimits()
	if defaults != defaultQuicSendFlightLimits() || newQuicSendFlightWithLimits(quicSendFlightLimits{}).limits != defaultQuicSendFlightLimits() {
		t.Fatalf("unset limits %+v, want the defaults", defaults)
	}
}

// A frame limit of one or two would leave writers a writeLimit of zero, so
// every write would wait forever. The flight raises it to three, a
// writeLimit of one, and the H3 ledger charges the three roots it keeps.
func TestPlatformH3SendFlightRaisesAFrameLimitThatWouldBlockWriters(t *testing.T) {
	for _, frames := range []int{1, 2} {
		settings := testingDefaultPlatformTransportSettingsWithMemoryTarget(mib(20))
		ApplyMobilePlatformTransportMemoryPolicy(settings, mib(20))
		settings.H3SendFlightFrameCount = frames
		limits := settings.h3SendFlightLimits()
		flight := newQuicSendFlightWithLimits(limits)
		if limits != (quicSendFlightLimits{frames: 3, datagrams: 8}) || flight.limits != limits ||
			flight.limits.writeLimit() != 1 || len(flight.frames) != 3+8 {
			t.Fatalf("a frame limit of %d: ledger %+v, flight %+v with %d frame slots",
				frames, limits, flight.limits, len(flight.frames))
		}
		if roots := settings.h3Ledger().sendRoots; roots != 3*platformH3RootByteCount {
			t.Fatalf("a frame limit of %d charges %d bytes of STREAM roots, want three roots", frames, roots)
		}
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if err := flight.wait(ctx, false, nil); err != nil {
				t.Fatalf("a frame limit of %d blocks the first write: %v", frames, err)
			}
		})
	}
}

// The phone sizes hold at every mobile target: a larger target keeps the
// window and the claim, so the budget holds more carriers rather than larger
// ones, and nothing exceeds the desktop window. Before R20 the window above
// 24 MiB was the desktop window, so one claim filled more than half the budget.
func TestPlatformExplicitMobilePolicyKeepsPhoneSizesAtLargerTargets(t *testing.T) {
	old := MemoryBudget()
	defer SetMemoryBudget(old)
	for _, target := range []ByteCount{mib(32), mib(64), mib(128)} {
		t.Run(fmt.Sprint(target), func(t *testing.T) {
			SetMemoryBudget(target)
			settings := testingDefaultPlatformTransportSettingsWithMemoryTarget(target)
			desktopWindow := settings.H3MaxConnectionReceiveWindowByteCount
			if target > mib(32) && settings.h3RetainedByteAccounting {
				t.Fatal("an ordinary desktop target was reclassified as mobile")
			}
			ApplyMobilePlatformTransportMemoryPolicy(settings, target)
			config := newPlatformQuicConfig(settings, 1)
			if config.Allow0RTT || !settings.h3RetainedByteAccounting {
				t.Fatal("explicit mobile profile lost retained-send accounting")
			}
			if config.MaxConnectionReceiveWindow != uint64(kib(256)) || desktopWindow < kib(256) ||
				settings.H3BudgetByteCount != kib(256)+settings.h3FixedMemoryByteCount() ||
				settings.H3BudgetByteCount != ByteCount(923532) {
				t.Fatalf("mobile window %d or claim %d left the phone sizes (desktop window %d)",
					config.MaxConnectionReceiveWindow, settings.H3BudgetByteCount, desktopWindow)
			}
			if (&PlatformTransport{settings: settings}).h3TransportBufferSize() != platformH3MemoryQueueCount ||
				config.InitialStreamReceiveWindow != uint64(kib(128)) || config.InitialConnectionReceiveWindow != uint64(kib(256)) {
				t.Fatal("mobile application/dial ownership escaped its fixed rows")
			}
			// Applying from both shared construction and a destination override
			// must not add the fixed rows twice or replace an existing owner.
			claim, budget := settings.H3BudgetByteCount, settings.PlatformTransportBudget
			window, stream := settings.H3MaxConnectionReceiveWindowByteCount, settings.H3MaxStreamReceiveWindowByteCount
			ApplyMobilePlatformTransportMemoryPolicy(settings, target)
			if settings.H3BudgetByteCount != claim || settings.PlatformTransportBudget != budget ||
				settings.H3MaxConnectionReceiveWindowByteCount != window || settings.H3MaxStreamReceiveWindowByteCount != stream {
				t.Fatal("mobile policy was not idempotent")
			}
		})
	}
}

// An embedder's unset sizes resolve to defaults outside the claim: quic-go's
// 256, 32 and 128 queue lengths, 256- and 512-KiB initial windows and the
// desktop's receive windows. The mobile policy gives each the phone's size,
// so the claim still bounds every queue and window the connection may fill.
func TestPlatformMobilePolicyChargesWhatUnsetSizesResolveTo(t *testing.T) {
	settings := testingDefaultPlatformTransportSettingsWithMemoryTarget(mib(64))
	settings.H3SocketReadBufferByteCount = 0
	settings.H3SocketWriteBufferByteCount = 0
	settings.H3MaxUnprocessedPacketCount = 0
	settings.H3DatagramSendQueueCount = 0
	settings.H3DatagramReceiveQueueCount = 0
	settings.H3SendFlightFrameCount = 0
	settings.H3DatagramSettings = nil
	settings.H3InitialStreamReceiveWindowByteCount = 0
	settings.H3InitialConnectionReceiveWindowByteCount = 0
	settings.H3MaxStreamReceiveWindowByteCount = 0
	settings.H3MaxConnectionReceiveWindowByteCount = 0
	ApplyMobilePlatformTransportMemoryPolicy(settings, mib(64))
	config := newPlatformQuicConfig(settings, 1)
	if config.MaxUnprocessedPackets != 32 || config.MaxDatagramSendQueueLen != 8 || config.MaxDatagramReceiveQueueLen != 32 ||
		config.InitialStreamReceiveWindow != uint64(kib(128)) || config.MaxStreamReceiveWindow != uint64(kib(192)) ||
		config.InitialConnectionReceiveWindow != uint64(kib(256)) || config.MaxConnectionReceiveWindow != uint64(kib(256)) {
		t.Fatalf("unset sizes resolved outside the phone's: %+v", config)
	}
	if settings.H3SocketReadBufferByteCount != kib(32) || settings.H3SocketWriteBufferByteCount != kib(16) ||
		settings.h3SendFlightLimits() != (quicSendFlightLimits{frames: 48, datagrams: 8}) ||
		settings.H3DatagramSettings == nil || settings.H3DatagramSettings.MaxReassemblyByteCount != 16*1024 ||
		settings.H3BudgetByteCount != ByteCount(923532) {
		t.Fatalf("unset sizes charged %d, not the phone's claim: %+v", settings.H3BudgetByteCount, settings)
	}
}

// R20's arithmetic at the phone memory targets, by real admission on a
// phone's carrier budget: the SDK default (20 MiB), the steady target (24),
// iOS (32), 48, and Android (64). Each row pins how many complete h3 graphs
// the budget admitted before R20 and admits now, and a phone that provides on
// h3: its two pinned control paths acquired and its standby held, then its
// consumer's window transports. Every claim charges what its configured
// queues and buffers bound, about 0.88 MiB, so a phone holds an h3 carrier
// for each of six to ten peers, and its provider's three control paths and
// two consumers fit at every target.
func TestMobileH3ClaimsFitMoreCarriersThanBefore(t *testing.T) {
	old := MemoryBudget()
	defer SetMemoryBudget(old)
	SetMemoryBudget(0)
	const claim = ByteCount(923532)
	for _, c := range []struct {
		target ByteCount
		// complete h3 graphs an empty budget admits, before R20 and now
		before, fits int
		// consumer h3 graphs admitted beside the two pins and the held standby
		consumersBesidePins int
		// whether a consumer's h1 fallback still fits beside the pins and
		// those consumers
		h1BesidePins bool
	}{
		{mib(20), 1, 5, 3, false},
		{mib(24), 2, 6, 4, true},
		{mib(32), 1, 9, 7, false},
		{mib(48), 1, 13, 11, false},
		{mib(64), 1, 18, 16, false},
	} {
		settings := testingDefaultPlatformTransportSettingsWithMemoryTarget(c.target)
		settings.PlatformTransportBudget = NewMobilePlatformTransportBudgetForMemoryTarget(c.target)
		ApplyMobilePlatformTransportMemoryPolicy(settings, c.target)
		// the sdk's ack lane, which its h1 claim carries on a phone
		settings.H1AckPriorityBufferSize = 8
		budget := settings.PlatformTransportBudget
		h1Claim := settings.H1MemoryReservationByteCount()
		config := newPlatformQuicConfig(settings, 1)
		if got := settings.H3MemoryReservationByteCount(TransportModeH3, nil); got != claim ||
			config.MaxConnectionReceiveWindow != uint64(kib(256)) || config.MaxStreamReceiveWindow != uint64(kib(192)) {
			t.Fatalf("target %d: claim %d, connection %d, stream %d; want %d with 256/192 KiB of credit",
				c.target, got, config.MaxConnectionReceiveWindow, config.MaxStreamReceiveWindow, claim)
		}
		if stats := budget.Stats(); stats.MaxTransportCount < c.fits+3 {
			t.Fatalf("target %d: %d carrier slots bind before the bytes", c.target, stats.MaxTransportCount)
		}

		claims := []*platformTransportBudgetReservation{}
		admit := func(class platformTransportBudgetClass, byteCount ByteCount) bool {
			reservation := budget.register(class, byteCount, true)
			if !reservation.TryAcquire() {
				reservation.Release()
				return false
			}
			claims = append(claims, reservation)
			return true
		}
		releaseAll := func() {
			for _, reservation := range claims {
				reservation.Release()
			}
			claims = nil
			if stats := budget.Stats(); stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
				t.Fatalf("target %d: unbalanced budget %+v", c.target, stats)
			}
		}

		fits := 0
		for admit(platformTransportBudgetH3Explicit, claim) {
			fits += 1
		}
		releaseAll()
		if fits != c.fits || fits < c.before {
			t.Fatalf("target %d: %d complete h3 graphs fit, want %d (before R20: %d)", c.target, fits, c.fits, c.before)
		}

		// a phone providing on h3: both pins, and its standby held
		if !admit(platformTransportBudgetH3Explicit, claim) || !admit(platformTransportBudgetH3Explicit, claim) {
			t.Fatalf("target %d: the provider's pins did not fit", c.target)
		}
		standby := budget.registerWithPriorityDeferred(platformTransportBudgetH3Explicit, claim, true,
			PlatformTransportBudgetPriorityForeground, true)
		consumers := 0
		for admit(platformTransportBudgetH3Explicit, claim) {
			consumers += 1
		}
		h1 := admit(platformTransportBudgetH1, h1Claim)
		if consumers != c.consumersBesidePins || h1 != c.h1BesidePins {
			t.Fatalf("target %d: beside the pins %d consumer h3 graphs fit and h1 %t, want %d and %t",
				c.target, consumers, h1, c.consumersBesidePins, c.h1BesidePins)
		}
		standby.Release()
		releaseAll()

		// the provider's three control paths and two consumers, at once
		for i := range 5 {
			if !admit(platformTransportBudgetH3Explicit, claim) {
				t.Fatalf("target %d: graph %d of three control paths and two consumers did not fit", c.target, i+1)
			}
		}
		releaseAll()
		t.Logf("target %d: budget %d, slots %d, claim %d, h3 graphs %d (before %d), h1 %d",
			c.target, budget.Stats().TotalByteCount, budget.Stats().MaxTransportCount, claim, fits, c.before, h1Claim)
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
	// The slack beside the inner claim, a sibling H1, the inner translation
	// and the outer DNS extender's graph: R20's phone-sized inner claim of
	// 923,532 bytes, and an outer graph of a 256-KiB window, 512 KiB of
	// control, 256 KiB of send roots, 48 KiB of sockets and 144 KiB of
	// translation. The sibling H1 is 256 KiB through 32 MiB and 512 at 64.
	outer := kib(256 + 512 + 256 + 48 + 144)
	for _, profile := range []struct{ target, process, nestedDnsSlack ByteCount }{
		{mib(20), mib(32), mib(5) - (923532 + kib(256) + kib(144) + outer)},
		{mib(28), mib(40), mib(7) - (923532 + kib(256) + kib(144) + outer)},
		{mib(32), mib(32), mib(8) - (923532 + kib(256) + kib(144) + outer)},
		{mib(64), mib(64), mib(16) - (923532 + kib(512) + kib(144) + outer)},
	} {
		for _, mode := range []TransportMode{TransportModeH1, TransportModeH3, TransportModeH3Dns, TransportModeH3DnsPump} {
			for _, carrier := range []string{"direct", ExtenderCarrierTcp, ExtenderCarrierQuic, ExtenderCarrierDns} {
				t.Run(fmt.Sprintf("%d/%s/%s", profile.target, mode, carrier), func(t *testing.T) {
					SetMemoryBudget(profile.process)
					settings := testingDefaultPlatformTransportSettingsWithMemoryTarget(profile.target)
					ApplyMobilePlatformTransportMemoryPolicy(settings, profile.target)
					settings.AltUrl = "https://127.0.0.1:443"
					settings.DnsPumpHost = "127.0.0.1"
					transport := newTestAltTransport(t, settings)
					ctx := transport.dialContext(t.Context())
					budget := settings.PlatformTransportBudget
					root := budget.root
					if root != budget || root.Stats().TotalByteCount != profile.target/4 {
						t.Fatal("default carrier budget did not retain its independent owner target")
					}
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
						t.Fatalf("carrier/owner accounting mismatch: carrier=%+v root=%+v", stats, root.Stats())
					}
					if (mode == TransportModeH3Dns || mode == TransportModeH3DnsPump) && carrier == ExtenderCarrierDns {
						wantSlack := profile.nestedDnsSlack
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
	settings := testingDefaultPlatformTransportSettingsWithMemoryTarget(mib(20))
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
			settings := testingDefaultPlatformTransportSettingsWithMemoryTarget(mib(20))
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
			settings := testingDefaultPlatformTransportSettingsWithMemoryTarget(mib(20))
			inner := settings.PlatformTransportBudget.register(platformTransportBudgetH3, settings.H3BudgetByteCount, true)
			if !inner.TryAcquire() {
				t.Fatal("device admission")
			}
			defer inner.Release()
			deviceBefore := settings.PlatformTransportBudget.StatsWithRoot()
			// An API request has its own lifecycle owner, not an implicit
			// process parent shared with the device. Use the existing request
			// budget seam so this real dial's claim is directly observable.
			apiBudget := DefaultPlatformTransportBudget()
			if apiBudget == settings.PlatformTransportBudget || apiBudget.root != apiBudget {
				t.Fatal("API and device unexpectedly share their default admission owner")
			}
			ctx := context.WithValue(t.Context(), platformTransportNestedBudgetContextKey{}, apiBudget)
			server := newTestAltServer(t, whodis)
			strategy := newTestAltStrategy(t, server)
			name := "alt h3"
			if whodis {
				name = "alt whodis"
			}
			if body := testAltGetWithContext(t, ctx, testAltDialer(t, strategy, name)); body != testAltBodyText {
				t.Fatal(body)
			}
			policy := newExtenderQuicMemoryPolicy(ctx, DefaultConnectSettings())
			if whodis {
				policy.packetTranslationSettings()
			}
			if policy.budget != apiBudget || !policy.usesSlot || policy.unbudgeted {
				t.Fatal("standalone API policy lost its explicit owner, inner TLS charge, or carrier slot")
			}
			if got := apiBudget.Stats(); got.UsedByteCount != policy.byteCount || got.UsedTransportCount != 1 {
				t.Fatalf("live %s claim=%+v want=%d bytes and one slot", name, got, policy.byteCount)
			}
			if got := settings.PlatformTransportBudget.StatsWithRoot(); got != deviceBefore {
				t.Fatalf("API request changed the independent device owner: before=%+v after=%+v", deviceBefore, got)
			}
			strategy.Close()
			deadline := time.Now().Add(5 * time.Second)
			for apiBudget.Stats().UsedByteCount != 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if got := apiBudget.Stats(); got.UsedByteCount != 0 || got.UsedTransportCount != 0 || got.ReservedByteCount != got.ReleasedByteCount {
				t.Fatalf("alt close leaked API owner claim: %+v", got)
			}
			if got := settings.PlatformTransportBudget.StatsWithRoot(); got != deviceBefore {
				t.Fatalf("API teardown changed the independent device owner: before=%+v after=%+v", deviceBefore, got)
			}
		})
	}
}

// An untagged API dial deliberately creates one independent policy owner.
// Filling one such owner must neither consume nor provide capacity to another.
func TestMobileAltApiDefaultDialBudgetsAreIndependent(t *testing.T) {
	old := MemoryBudget()
	SetMemoryBudget(mib(32))
	defer SetMemoryBudget(old)
	for _, whodis := range []bool{false, true} {
		t.Run(fmt.Sprint(whodis), func(t *testing.T) {
			first := newExtenderQuicMemoryPolicy(t.Context(), DefaultConnectSettings())
			second := newExtenderQuicMemoryPolicy(t.Context(), DefaultConnectSettings())
			if whodis {
				first.packetTranslationSettings()
				second.packetTranslationSettings()
			}
			if first.budget == second.budget || first.budget.root != first.budget || second.budget.root != second.budget ||
				first.budget.Stats().TotalByteCount != mib(8) || second.budget.Stats().TotalByteCount != mib(8) ||
				!first.usesSlot || !second.usesSlot || first.unbudgeted || second.unbudgeted {
				t.Fatal("default standalone API policies share or omit their admission owner")
			}
			filler := first.budget.register(platformTransportBudgetExtender, mib(8), true)
			defer filler.Release()
			if !filler.TryAcquire() {
				t.Fatal("could not fill the first API owner")
			}
			claim, err := first.acquire(t.Context())
			claim.Release()
			if !errors.Is(err, errExtenderMemoryBudget) {
				t.Fatalf("full API owner admitted another claim: %v", err)
			}
			claim, err = second.acquire(t.Context())
			if err != nil {
				t.Fatalf("another API owner's saturation prevented admission: %v", err)
			}
			defer claim.Release()
			if got := second.budget.Stats(); got.UsedByteCount != second.byteCount || got.UsedTransportCount != 1 {
				t.Fatalf("independent API claim=%+v want=%d bytes and one slot", got, second.byteCount)
			}
			claim.Release()
			if got := first.budget.Stats().UsedByteCount; got != mib(8) {
				t.Fatalf("second owner's release changed first owner: %d", got)
			}
			filler.Release()
			for _, budget := range []*PlatformTransportBudget{first.budget, second.budget} {
				if got := budget.Stats(); got.UsedByteCount != 0 || got.UsedTransportCount != 0 || got.ReservedByteCount != got.ReleasedByteCount {
					t.Fatalf("independent API owner teardown retained a claim: %+v", got)
				}
			}
		})
	}
}
