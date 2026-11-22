package connect

import (
	"context"
	"io"
	"time"
	"unsafe"
)

type h3StreamWriter interface {
	io.Writer
	SetWriteDeadline(time.Time) error
}

// The charges the H3 ledger is built from (platformH3Ledger). All figures
// describe simultaneously retained ownership, not interchangeable alternatives
// to the receive window. DNS translation and any outer extender add to the
// carrier's atomic claim against the same child and process root.
const (
	// One retained packet: a quic-go packet buffer, a DATAGRAM payload copy or
	// a STREAM root. quic-go v0.61's 1452-byte buffers round to the 1536-byte
	// allocation class; with their descriptors each is charged 2 KiB.
	platformH3RootByteCount ByteCount = 2 * 1024
	// frame and slice descriptors of one DATAGRAM queue slot
	platformH3DatagramSlotMetaByteCount ByteCount = 192
	// quic-go's sent-packet record for one unacknowledged packet
	platformH3SentPacketRecordByteCount ByteCount = 192
	// packets in flight carrying no root: ACKs, PINGs and other control
	platformH3ControlPacketCount = 16
	// TLS and QUIC connection, stream and worker state besides queued packets
	platformH3ConnectionStateByteCount ByteCount = 128 * 1024
	// the reassembler's replay window of retired message ids
	platformH3ReplayWindowByteCount ByteCount = 16 * 1024
	// the descriptor of one message pending reassembly
	platformH3ReassemblyDescriptorByteCount ByteCount = 512
	// the stream writer's 4-KiB chunk scratch and one buffered frame
	platformH3SendScratchByteCount ByteCount = 8 * 1024
	// channel slots and pool headers of the application routes
	platformH3ApplicationDescriptorByteCount ByteCount = 4 * 1024
	// the depth of each H3 route under retained accounting
	platformH3MemoryQueueCount = 4
	// quic-go's own queue lengths, which a setting of zero keeps
	platformH3DefaultUnprocessedPacketCount    = 256
	platformH3DefaultDatagramReceiveQueueCount = 128
	// URnetwork's quic-go raises a smaller unprocessed-packet limit to this,
	// so a server's buffered 0-RTT packets always fit
	// (../quic-go/URNETWORK.md)
	platformH3MinUnprocessedPacketCount = 32
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

// platformH3Ledger is one H3 graph's fixed retained ownership, row by row,
// besides its receive credit. Each row is what the settings' configured queue
// and buffer sizes bound, so smaller sizes charge less and no row charges
// capacity its queues cannot hold. The rows are not unused receive permission:
// receive credit is charged on top of them.
type platformH3Ledger struct {
	// the UDP read and write socket buffers
	sockets ByteCount
	// unacknowledged STREAM roots (H3SendFlightFrameCount)
	sendRoots ByteCount
	// quic-go's sent-packet records for every root and control packet in
	// flight, the retained-send controller's tables, and its write scratch
	sendState ByteCount
	// quic-go's DATAGRAM send and receive queues, one payload copy a slot,
	// plus the frame being parsed. Already-sent DATAGRAM roots stay in
	// quic-go's ACK history until ACK or loss, and the retained-send
	// controller admits as many roots across queued and sent state as the
	// send queue holds, not that many of each. DATAGRAMs are not retransmitted.
	datagrams ByteCount
	// the application reassembler's peer payload cap, its replay window and
	// the descriptors of the messages pending reassembly
	reassembly ByteCount
	// quic-go's received packets not yet processed, and the connection state
	control ByteCount
	// the routes, which hold the route depth of messages in each direction,
	// five dispatcher, read and pending holds, the hybrid stream queue and the
	// stream write batch, with their channel slots and pool headers
	application ByteCount
}

func (self platformH3Ledger) total() ByteCount {
	return self.sockets + self.sendRoots + self.sendState + self.datagrams +
		self.reassembly + self.control + self.application
}

// h3SendFlightLimits are the retained-send controller's limits on one H3
// connection, as the flight normalizes them: its STREAM roots, and its
// DATAGRAM roots, which match quic-go's DATAGRAM send queue.
func (self *PlatformTransportSettings) h3SendFlightLimits() quicSendFlightLimits {
	return quicSendFlightLimits{
		frames:    self.H3SendFlightFrameCount,
		datagrams: self.H3DatagramSendQueueCount,
	}.normalized()
}

// h3Ledger charges what these settings' sizes bound (platformH3Ledger).
func (self *PlatformTransportSettings) h3Ledger() platformH3Ledger {
	transport := &PlatformTransport{settings: self}
	limits := self.h3SendFlightLimits()
	unprocessed := platformH3DefaultUnprocessedPacketCount
	if 0 < self.H3MaxUnprocessedPacketCount {
		unprocessed = max(self.H3MaxUnprocessedPacketCount, platformH3MinUnprocessedPacketCount)
	}
	datagramReceive := platformH3DefaultDatagramReceiveQueueCount
	if 0 < self.H3DatagramReceiveQueueCount {
		datagramReceive = self.H3DatagramReceiveQueueCount
	}
	datagrams := self.H3DatagramSettings
	if datagrams == nil {
		datagrams = DefaultH3DatagramSettings()
	}
	flightTables := ByteCount(limits.frames+limits.datagrams)*ByteCount(unsafe.Sizeof(quicSendFlightFrame{})) +
		quicSendFlightPackets*ByteCount(unsafe.Sizeof(quicSendFlightPacket{})) +
		quicSendFlightStreams*ByteCount(unsafe.Sizeof((*quicSendFlightWriter)(nil)))
	return platformH3Ledger{
		sockets:   transport.h3SocketReadBufferByteCount() + transport.h3SocketWriteBufferByteCount(),
		sendRoots: ByteCount(limits.frames) * platformH3RootByteCount,
		sendState: ByteCount(limits.frames+limits.datagrams+platformH3ControlPacketCount)*platformH3SentPacketRecordByteCount +
			flightTables + platformH3SendScratchByteCount,
		datagrams: ByteCount(limits.datagrams+datagramReceive+1)*platformH3RootByteCount +
			ByteCount(limits.datagrams+datagramReceive)*platformH3DatagramSlotMetaByteCount,
		reassembly: ByteCount(datagrams.MaxReassemblyByteCount) + platformH3ReplayWindowByteCount +
			ByteCount(datagrams.MaxReassemblyMessageCount)*platformH3ReassemblyDescriptorByteCount,
		control: ByteCount(unprocessed)*platformH3RootByteCount + platformH3ConnectionStateByteCount,
		application: ByteCount(2*transport.h3TransportBufferSize()+5)*retainedMessageCapacity(ByteCount(datagrams.MaxMessageByteCount)) +
			ByteCount(datagrams.HybridStreamQueueByteCount()) +
			ByteCount(max(0, self.H3StreamWriteBatchMaxByteCount)) +
			platformH3ApplicationDescriptorByteCount,
	}
}

// h3FixedMemoryByteCount is the H3 claim besides its receive credit.
func (self *PlatformTransportSettings) h3FixedMemoryByteCount() ByteCount {
	return self.h3Ledger().total()
}

// h3DialTransientByteCount is what one speculative dial candidate of a race
// holds before authentication: its initial connection credit, its sockets,
// quic-go's queues and connection state, the reassembler and the send state,
// but no STREAM roots or application routes yet.
func (self *PlatformTransportSettings) h3DialTransientByteCount() ByteCount {
	ledger := self.h3Ledger()
	initialConnection := min(
		max(0, self.H3InitialConnectionReceiveWindowByteCount),
		max(0, self.H3MaxConnectionReceiveWindowByteCount),
	)
	return initialConnection + ledger.sockets + ledger.control + ledger.datagrams +
		ledger.reassembly + ledger.sendState
}

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

// The phone sizes ApplyMobilePlatformTransportMemoryPolicy installs (server
// LBALTOPT2 R20). A phone has six to ten peers, each with an H3 carrier of
// its own, beside its provider's control paths, so each carrier's queues and
// buffers are sized for a share of the budget rather than for one carrier's
// peak: with them every claim is about 0.88 MiB, nine fit the 8 MiB budget of
// iOS's 32-MiB target and eighteen the 16 MiB of Android's 64. They are the
// policy's values for the settings it is given, not settings of their own; an
// embedder's smaller values stand.
const (
	// Connection receive credit, three quarters of it stream credit: about
	// 21 Mbit/s of connection and 16 of stream data a carrier at 100 ms RTT.
	// DATAGRAMs are not flow controlled and are not bounded by it.
	mobilePlatformH3ConnectionReceiveWindowByteCount ByteCount = 256 * 1024
	mobilePlatformH3SocketReadBufferByteCount        ByteCount = 32 * 1024
	mobilePlatformH3SocketWriteBufferByteCount       ByteCount = 16 * 1024
	// quic-go's queues: 32 unprocessed packets, the least it keeps room for
	// a server's 0-RTT packets, and 8 and 32 DATAGRAM slots
	mobilePlatformH3UnprocessedPacketCount    = 32
	mobilePlatformH3DatagramSendQueueCount    = 8
	mobilePlatformH3DatagramReceiveQueueCount = 32
	// Unacknowledged STREAM roots. Writers hold at most half of them, the
	// rest absorbing retransmission splits, so about 2.6 Mbit/s of stream
	// upload a carrier at 100 ms RTT, besides about 0.9 on the 8 DATAGRAM
	// roots.
	mobilePlatformH3SendFlightFrameCount = 48
	// The reassembler's peer payload cap holds two whole 8-KiB messages.
	mobilePlatformH3ReassemblyByteCount    = 16 * 1024
	mobilePlatformH3ReassemblyMessageCount = 8
	// The hybrid stream queue holds as many messages as a route, and two of
	// the 16-KiB framer maximum, so no message the stream lane carries is
	// refused as oversize.
	mobilePlatformH3HybridStreamQueueMessageCount     = platformH3MemoryQueueCount
	mobilePlatformH3HybridStreamQueuePayloadByteCount = 32 * 1024
	mobilePlatformH3StreamWriteBatchByteCount         = 16 * 1024
)

// ApplyMobilePlatformTransportMemoryPolicy keeps mobile carrier ownership
// accounted at every positive target and installs the phone sizes of an H3
// carrier's queues, buffers and receive credit (the mobilePlatformH3*
// values), keeping an embedder's smaller ones. The claim is then exactly the
// connection receive credit plus what the configured sizes bound
// (platformH3Ledger), the same at every target, so a phone's budget holds as
// many carriers as its bytes allow. A transport the budget cannot admit
// carries on h1 meanwhile (transport_h3_budget_fallback.go). It must not turn
// off retained-send, speculative-dial, socket, or application-queue
// accounting. Call after target sizing and before starting transports.
// Applying it twice changes nothing. Desktop/server constructors retain their
// existing policy unless their embedder explicitly selects this one.
func ApplyMobilePlatformTransportMemoryPolicy(settings *PlatformTransportSettings, target ByteCount) {
	if settings == nil || target <= 0 {
		return
	}
	// a nonpositive value resolves to a default outside the claim, so it
	// takes the phone's size instead
	phone := func(value ByteCount, size ByteCount) ByteCount {
		if value <= 0 {
			return size
		}
		return min(value, size)
	}
	phoneCount := func(value int, count int) int {
		if value <= 0 {
			return count
		}
		return min(value, count)
	}
	settings.h3RetainedByteAccounting = true
	settings.h1RetainedByteAccounting = true
	settings.H3SocketReadBufferByteCount = phone(settings.H3SocketReadBufferByteCount, mobilePlatformH3SocketReadBufferByteCount)
	settings.H3SocketWriteBufferByteCount = phone(settings.H3SocketWriteBufferByteCount, mobilePlatformH3SocketWriteBufferByteCount)
	settings.H3MaxUnprocessedPacketCount = phoneCount(settings.H3MaxUnprocessedPacketCount, mobilePlatformH3UnprocessedPacketCount)
	settings.H3DatagramSendQueueCount = phoneCount(settings.H3DatagramSendQueueCount, mobilePlatformH3DatagramSendQueueCount)
	settings.H3DatagramReceiveQueueCount = phoneCount(settings.H3DatagramReceiveQueueCount, mobilePlatformH3DatagramReceiveQueueCount)
	settings.H3SendFlightFrameCount = phoneCount(settings.H3SendFlightFrameCount, mobilePlatformH3SendFlightFrameCount)
	if 0 < settings.H3StreamWriteBatchMaxByteCount {
		settings.H3StreamWriteBatchMaxByteCount = min(settings.H3StreamWriteBatchMaxByteCount, mobilePlatformH3StreamWriteBatchByteCount)
	}
	if settings.H3DatagramSettings == nil {
		settings.H3DatagramSettings = DefaultH3DatagramSettings()
	}
	datagrams := settings.H3DatagramSettings
	// The admitted policy uses the existing single-fragment production
	// limits. Multiple live generations share this bound as well.
	datagrams.MaxReassemblyByteCount = max(datagrams.MaxMessageByteCount,
		min(datagrams.MaxReassemblyByteCount, mobilePlatformH3ReassemblyByteCount))
	datagrams.MaxReassemblyMessageCount = min(datagrams.MaxReassemblyMessageCount, mobilePlatformH3ReassemblyMessageCount)
	datagrams.ProcessReassemblyByteCount = int64(datagrams.MaxReassemblyByteCount)
	datagrams.HybridStreamQueueMessageCount = min(datagrams.HybridStreamQueueMessageCount, mobilePlatformH3HybridStreamQueueMessageCount)
	datagrams.HybridStreamQueuePayloadByteCount = min(datagrams.HybridStreamQueuePayloadByteCount, mobilePlatformH3HybridStreamQueuePayloadByteCount)
	connection := phone(settings.H3MaxConnectionReceiveWindowByteCount, mobilePlatformH3ConnectionReceiveWindowByteCount)
	stream := connection * 3 / 4
	if 0 < settings.H3MaxStreamReceiveWindowByteCount {
		stream = min(stream, settings.H3MaxStreamReceiveWindowByteCount)
	}
	settings.H3MaxConnectionReceiveWindowByteCount = connection
	settings.H3MaxStreamReceiveWindowByteCount = stream
	// An unset initial window would take 256 or 512 KiB in the QUIC config,
	// above the credit charged, so it takes the phone's 128 or 256 KiB too.
	settings.H3InitialStreamReceiveWindowByteCount = min(
		phone(settings.H3InitialStreamReceiveWindowByteCount, kib(128)), stream)
	settings.H3InitialConnectionReceiveWindowByteCount = min(
		phone(settings.H3InitialConnectionReceiveWindowByteCount, kib(256)), connection)
	// last, from every size above
	settings.H3BudgetByteCount = connection + settings.h3FixedMemoryByteCount()
}

func (self *PlatformTransport) h3TransportBufferSize() int {
	if self.settings.h3RetainedByteAccounting {
		return min(self.settings.TransportBufferSize, platformH3MemoryQueueCount)
	}
	return self.settings.TransportBufferSize
}
