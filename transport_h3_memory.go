package connect

import (
	"context"
	"io"
	"time"
)

type h3StreamWriter interface {
	io.Writer
	SetWriteDeadline(time.Time) error
}

// All figures describe simultaneously retained ownership, not interchangeable
// alternatives to the receive window. DNS translation and any outer extender
// add to the carrier's atomic claim against the same child and process root.
const (
	platformH3SocketByteCount        ByteCount = 64 * 1024
	platformH3DatagramQueueBytes     ByteCount = 352 * 1024
	platformH3ReassemblyBytes        ByteCount = 96 * 1024
	platformH3ControlBytes           ByteCount = 512 * 1024
	platformH3ApplicationQueueBytes  ByteCount = 256 * 1024
	platformH3MemoryQueueCount                 = 4
	platformH3DialTransientByteCount ByteCount = 1408 * 1024
)

// Nested owners spend capacity already reserved with their inner carrier.
// The local allocator enforces the subdivision; it is not another allowance
// against the process. Its sockets are joined before the outer claim releases.
type platformTransportNestedBudgetContextKey struct{}

// The same complete retained graph charged
// before the carrier starts, including DNS translation and a configured
// outer carrier. It neither acquires memory nor starts transport work.
// Owners use it to compose feasible window sizes, not to bypass admission.
func (self *PlatformTransportSettings) H3MemoryReservationByteCount(
	mode TransportMode,
	clientStrategy *ClientStrategy,
) ByteCount {
	if self == nil {
		return 0
	}
	transport := &PlatformTransport{
		settings:        self,
		targetMode:      mode,
		modePreferences: normalizeTransportModePreferences(self.ModePreferences),
		clientStrategy:  clientStrategy,
	}
	return transport.h3BudgetByteCount() + transport.h3NestedMemoryByteCount()
}

// Reserve one complete graph before starting its runners. Acquiring only the
// inner QUIC claim can fill the aggregate with carriers that all still need
// translation/outer memory, while pending explicit peers block those additions.
// Auto serializes its H3 modes, so one largest nested graph covers every mode.
func (self *PlatformTransport) h3NestedMemoryByteCount() ByteCount {
	byteCount := ByteCount(0)
	if self.targetMode == TransportModeH3Dns || self.targetMode == TransportModeH3DnsPump ||
		(self.targetMode == TransportModeAuto &&
			(self.modePreference(TransportModeH3Dns) != modePreferenceNone ||
				self.modePreference(TransportModeH3DnsPump) != modePreferenceNone)) {
		_, byteCount = boundedQuicPacketTranslationSettings()
	}
	if self.clientStrategy != nil && self.settings.H3PacketConnFactory == nil {
		if extender := self.clientStrategy.H3ExtenderConfig(); extender != nil {
			if extender.Profile.ConnectMode == ExtenderConnectModeTcpTls {
				byteCount += self.h1BaseBudgetByteCount()
			} else {
				policy := newExtenderQuicMemoryPolicy(self.dialContext(context.Background()), self.clientStrategy.ConnectSettings())
				if extender.Profile.ConnectMode == ExtenderConnectModeDns {
					policy.packetTranslationSettings()
				}
				byteCount += policy.byteCount
			}
		}
	}
	return byteCount
}

func platformH3FixedMemoryByteCount() ByteCount {
	return 2*platformH3SocketByteCount + extenderQuicSendMemoryByteCount +
		platformH3DatagramQueueBytes + platformH3ReassemblyBytes +
		platformH3ControlBytes + platformH3ApplicationQueueBytes
}

// quic-go v0.61 has 32 send and 128 receive DATAGRAM slots. Already-sent
// DATAGRAM roots stay in its ACK history until ACK/loss, so the send controller
// admits at most 32 roots across queued AND sent state, not 32 of each. Each
// packet is at most 1452 bytes; charge a 2-KiB allocation class for 160 slots
// plus one incoming producer, and 30 KiB for frame/slice metadata. DATAGRAMs
// are not retransmitted. The application reassembler has a separate 64-KiB peer
// payload cap, with 32 KiB for its bounded fragment/replay maps and descriptors.
// Application routes hold at most four 8-KiB messages in each direction, the
// hybrid stream queue holds 64 KiB, and the batch writer holds 64 KiB. The rest
// of the 256-KiB row covers producer/reader holds, pool headers, and descriptors.
//
// The fixed rows sum to 1600 KiB; they are not unused receive permission, and
// receive credit is charged on top of them (ApplyMobilePlatformTransportMemoryPolicy).
// Targets through 32 MiB take the mobile policy on every surface; larger and
// server targets retain their existing window policy unless their embedder
// selects the mobile one, and unbudgeted defaults are unchanged.
func applyPlatformH3MemoryPolicy(settings *PlatformTransportSettings, target ByteCount) {
	settings.h3RetainedByteAccounting = 0 < target && target <= mib(32)
	settings.h1RetainedByteAccounting = settings.h3RetainedByteAccounting
	if !settings.h3RetainedByteAccounting {
		return
	}
	ApplyMobilePlatformTransportMemoryPolicy(settings, target)
}

// The parameters of the mobile H3 claim (mobilePlatformH3ClaimByteCount).
// They are the policy ApplyMobilePlatformTransportMemoryPolicy installs into
// the settings it is given, not settings of their own.
const (
	// A phone's carrier budget is sized to hold this many complete H3 graphs
	// at once: its provider's three control paths (the v4 and v6 pins and
	// the standby) and two consumer window transports (server LBALTOPT2 R20).
	mobilePlatformH3ClaimsPerBudget = 5
	// The least connection receive credit a mobile H3 claim keeps where a
	// fifth of the budget cannot cover it on top of the fixed rows. 448 KiB
	// makes the claim 2 MiB: two fit the 5-MiB budget of a 20-MiB target with
	// room left for an h1 fallback, three fit 24 MiB and four fit 32 MiB. It
	// bounds a transport's reliable stream to about 28 Mbit/s at 100 ms RTT
	// (37 for the connection), near the 38-52 Mbit/s MEMSTEADY measured on
	// the mobile h1 path a transport falls back to. DATAGRAMs are not flow
	// controlled and are not bounded by it.
	mobilePlatformH3MinConnectionReceiveWindowByteCount ByteCount = 448 * 1024
)

// mobilePlatformH3ClaimByteCount is the complete inner H3 claim the mobile
// policy gives a transport of an owner with this memory target: a fifth of
// the owner's carrier budget, so its provider's three control paths and two
// consumer transports fit at once, but never less than the fixed rows plus
// the least receive credit. The fixed rows are charged in full either way;
// only the receive credit shrinks. A fifth of the budget covers the fixed
// rows plus the least credit from a 40-MiB target up, so smaller targets fit
// fewer complete graphs, and a transport the budget cannot admit carries on
// h1 instead (transport_h3_budget_fallback.go).
func mobilePlatformH3ClaimByteCount(target ByteCount) ByteCount {
	return max(
		platformH3FixedMemoryByteCount()+mobilePlatformH3MinConnectionReceiveWindowByteCount,
		platformTransportBudgetByteCountForMemoryTarget(target)/mobilePlatformH3ClaimsPerBudget,
	)
}

// ApplyMobilePlatformTransportMemoryPolicy keeps mobile carrier ownership
// accounted at every positive target and sizes each H3 claim so several fit
// the owner's carrier budget (mobilePlatformH3ClaimByteCount). The claim is
// exactly the connection receive credit plus the fixed rows: the credit is
// what remains of the claim after the fixed rows, never more than the base
// window, and the stream credit stays three quarters of it. A larger mobile
// allowance never lowers the receive window or the claim, and it must not
// turn off retained-send, speculative-dial, socket, or application-queue
// accounting. Call after target sizing and before starting transports.
// Applying it twice changes nothing. Desktop/server constructors retain
// their existing policy unless their embedder explicitly selects this one.
func ApplyMobilePlatformTransportMemoryPolicy(settings *PlatformTransportSettings, target ByteCount) {
	if settings == nil || target <= 0 {
		return
	}
	settings.h3RetainedByteAccounting = true
	settings.h1RetainedByteAccounting = true
	settings.H3SocketReadBufferByteCount = min(settings.H3SocketReadBufferByteCount, platformH3SocketByteCount)
	settings.H3SocketWriteBufferByteCount = min(settings.H3SocketWriteBufferByteCount, platformH3SocketByteCount)
	fixed := platformH3FixedMemoryByteCount()
	connection := mobilePlatformH3ClaimByteCount(target) - fixed
	// A non-positive window would resolve to the unbounded default in the
	// QUIC config, outside the claim; it takes the policy's credit instead.
	if 0 < settings.H3MaxConnectionReceiveWindowByteCount {
		connection = min(connection, settings.H3MaxConnectionReceiveWindowByteCount)
	}
	stream := connection * 3 / 4
	if 0 < settings.H3MaxStreamReceiveWindowByteCount {
		stream = min(stream, settings.H3MaxStreamReceiveWindowByteCount)
	}
	settings.H3MaxConnectionReceiveWindowByteCount = connection
	settings.H3MaxStreamReceiveWindowByteCount = stream
	settings.H3BudgetByteCount = connection + fixed
	settings.H3InitialStreamReceiveWindowByteCount = min(settings.H3InitialStreamReceiveWindowByteCount,
		settings.H3MaxStreamReceiveWindowByteCount, kib(128))
	settings.H3InitialConnectionReceiveWindowByteCount = min(settings.H3InitialConnectionReceiveWindowByteCount,
		settings.H3MaxConnectionReceiveWindowByteCount, kib(256))
	if datagrams := settings.H3DatagramSettings; datagrams != nil {
		// The admitted policy uses the existing single-fragment production
		// limits. Multiple live generations share this bound as well.
		datagrams.ProcessReassemblyByteCount = int64(datagrams.MaxReassemblyByteCount)
	}
}

func (self *PlatformTransport) h3TransportBufferSize() int {
	if self.settings.h3RetainedByteAccounting {
		return min(self.settings.TransportBufferSize, platformH3MemoryQueueCount)
	}
	return self.settings.TransportBufferSize
}
