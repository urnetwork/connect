package connect

import (
	"context"
	"errors"
	"net"
	"sync"

	quic "github.com/quic-go/quic-go"
)

// Extender carriers may run below either H1 or H3. Carry the actual owner's
// policy through the dial context, including strategy races, instead of
// creating another budget that cannot see the already-admitted inner graph.
type extenderTransportSettingsContextKey struct{}

var errExtenderMemoryBudget = errors.New("extender carrier memory budget is full")

// A TCP extender adds one TLS/socket graph beneath the destination's existing
// TLS carrier. Charge another H1-sized claim to that owner as well; the inner
// H1 claim alone describes only the direct path.
func acquireExtenderTcpMemory(ctx context.Context) (*platformTransportBudgetReservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	settings, owned := ctx.Value(extenderTransportSettingsContextKey{}).(*PlatformTransportSettings)
	if !owned || settings == nil {
		if MemoryBudget() <= 0 {
			return nil, nil
		}
		settings = DefaultPlatformTransportSettings()
		owned = false
	}
	transport := &PlatformTransport{settings: settings}
	budget := settings.PlatformTransportBudget
	if budget == nil {
		budget = DefaultPlatformTransportBudget()
	}
	if nested, _ := ctx.Value(platformTransportNestedBudgetContextKey{}).(*PlatformTransportBudget); nested != nil {
		budget = nested
	}
	byteCount := transport.h1BudgetByteCount()
	if !owned {
		// A standalone HTTPS API call also owns the inner TLS graph. Device
		// callers already acquired that graph against their child budget.
		byteCount += transport.h1BudgetByteCount()
	}
	return (extenderQuicMemoryPolicy{budget: budget, byteCount: byteCount, usesSlot: !owned}).acquire(ctx)
}

type extenderBudgetConn struct {
	net.Conn
	reservation *platformTransportBudgetReservation
	closeOnce   sync.Once
	closeErr    error
}

type extenderBudgetPacketConn struct {
	net.PacketConn
	reservation *platformTransportBudgetReservation
	closeOnce   sync.Once
	closeErr    error
}

func (self *extenderBudgetPacketConn) Close() error {
	self.closeOnce.Do(func() {
		self.closeErr = self.PacketConn.Close()
		self.reservation.Release()
	})
	return self.closeErr
}

func (self *extenderBudgetPacketConn) SetReadBuffer(n int) error {
	if conn, ok := self.PacketConn.(interface{ SetReadBuffer(int) error }); ok {
		return conn.SetReadBuffer(n)
	}
	return nil
}

func (self *extenderBudgetPacketConn) SetWriteBuffer(n int) error {
	if conn, ok := self.PacketConn.(interface{ SetWriteBuffer(int) error }); ok {
		return conn.SetWriteBuffer(n)
	}
	return nil
}

func (self *extenderBudgetConn) Close() error {
	self.closeOnce.Do(func() {
		self.closeErr = self.Conn.Close()
		self.reservation.Release()
	})
	return self.closeErr
}

type extenderQuicMemoryPolicy struct {
	budget               *PlatformTransportBudget
	byteCount            ByteCount
	usesSlot             bool
	readBufferByteCount  ByteCount
	writeBufferByteCount ByteCount
	quicConfig           *quic.Config
	unbudgeted           bool
}

func newExtenderQuicMemoryPolicy(ctx context.Context, connectSettings *ConnectSettings) extenderQuicMemoryPolicy {
	settings, owned := ctx.Value(extenderTransportSettingsContextKey{}).(*PlatformTransportSettings)
	if !owned || settings == nil {
		settings = DefaultPlatformTransportSettings()
		owned = false
	}
	transport := &PlatformTransport{settings: settings}
	config := newPlatformQuicConfig(settings, 1)
	config.HandshakeIdleTimeout = connectSettings.ConnectTimeout + connectSettings.TlsTimeout + connectSettings.HandshakeTimeout
	// The outer carrier is one HTTP/3 request stream, never a QUIC DATAGRAM
	// lane. Keep its flow-control limits explicit even when the inner lane is H1.
	config.EnableDatagrams = false
	// An extender is one ordered request stream, not a second platform
	// transport. Copying the inner carrier's M/8 window consumes the entire
	// M/4 aggregate twice before socket/translation costs. Bound this outer
	// graph independently, for every target (including an unscaled server).
	config.InitialStreamReceiveWindow = min(config.InitialStreamReceiveWindow, uint64(kib(128)))
	config.MaxStreamReceiveWindow = min(config.MaxStreamReceiveWindow, uint64(kib(256)))
	config.InitialConnectionReceiveWindow = min(config.InitialConnectionReceiveWindow, uint64(kib(256)))
	config.MaxConnectionReceiveWindow = min(config.MaxConnectionReceiveWindow, uint64(kib(512)))
	budget := settings.PlatformTransportBudget
	if budget == nil {
		budget = DefaultPlatformTransportBudget()
	}
	if nested, _ := ctx.Value(platformTransportNestedBudgetContextKey{}).(*PlatformTransportBudget); nested != nil {
		budget = nested
	}
	readBufferByteCount := min(transport.h3SocketReadBufferByteCount(), kib(64))
	writeBufferByteCount := min(transport.h3SocketWriteBufferByteCount(), kib(64))
	// Flow-control capacity, an explicit QUIC/TLS packet/stream bookkeeping
	// envelope, and both raw socket capacities are additive—not interchangeable.
	byteCount := ByteCount(config.MaxConnectionReceiveWindow) + kib(512) + extenderQuicSendMemoryByteCount +
		readBufferByteCount + writeBufferByteCount
	if !owned {
		byteCount += transport.h1BudgetByteCount()
	}
	return extenderQuicMemoryPolicy{
		budget:    budget,
		byteCount: byteCount,
		// A platform caller already counts its physical carrier slot. A
		// standalone API/feed/probe dial must also acquire its own slot.
		usesSlot:             !owned,
		readBufferByteCount:  readBufferByteCount,
		writeBufferByteCount: writeBufferByteCount,
		quicConfig:           config,
		// Shared NetworkSpace API/feed strategies acquire against the process
		// root, which also owns mobile child claims. They are not assigned to
		// an arbitrary device and cannot create an additive private allowance.
		unbudgeted: !owned && MemoryBudget() <= 0,
	}
}

// DNS wrapping adds its own retained combine budget and fixed packet queues.
// Size it from the same owner's socket policy, then include those bytes in the
// outer claim before admission. These endpoints carry QUIC packets (currently
// <=1452 bytes), so no 64-KiB reconstructed payload may hide behind a queue slot.
func (self *extenderQuicMemoryPolicy) packetTranslationSettings() *PacketTranslationSettings {
	settings, byteCount := boundedQuicPacketTranslationSettings()
	self.byteCount += byteCount
	return settings
}

func boundedQuicPacketTranslationSettings() (*PacketTranslationSettings, ByteCount) {
	settings := DefaultPacketTranslationSettings()
	settings.SequenceBufferSize = min(settings.SequenceBufferSize, 4)
	settings.DnsMaxCombinedPacketByteCount = extenderDatagramMaxSize
	settings.DnsMaxCombineBytesPerAddress = kib(64)
	settings.DnsMaxCombineBytes = kib(64)
	// Four payload queues (in/out/forward/readPipeline), with one producer-held
	// packet each. A 4-KiB charge includes packet roots, wrappers and slice/map
	// overhead; the combine budget separately charges retained fragment roots.
	queueByteCount := ByteCount(4*(settings.SequenceBufferSize+1)) * kib(4)
	return settings, settings.DnsMaxCombineBytes + queueByteCount
}

func (self extenderQuicMemoryPolicy) acquire(ctx context.Context) (*platformTransportBudgetReservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if self.unbudgeted {
		return nil, nil
	}
	reservation := self.budget.register(platformTransportBudgetExtender, self.byteCount, self.usesSlot)
	if !reservation.TryAcquire() {
		reservation.Release()
		return nil, errExtenderMemoryBudget
	}
	return reservation, nil
}

// HTTP/3 prepares its own config for alt API dials. Preserve its protocol and
// timeout options, but never allow zero-valued dependency defaults or a larger
// caller window to exceed the working set the dial actually reserved.
func (self extenderQuicMemoryPolicy) boundReceiveConfig(config *quic.Config) {
	capWindow := func(value, limit uint64) uint64 {
		if value == 0 {
			return limit
		}
		return min(value, limit)
	}
	config.InitialStreamReceiveWindow = capWindow(config.InitialStreamReceiveWindow, self.quicConfig.InitialStreamReceiveWindow)
	config.MaxStreamReceiveWindow = capWindow(config.MaxStreamReceiveWindow, self.quicConfig.MaxStreamReceiveWindow)
	config.InitialConnectionReceiveWindow = capWindow(config.InitialConnectionReceiveWindow, self.quicConfig.InitialConnectionReceiveWindow)
	config.MaxConnectionReceiveWindow = capWindow(config.MaxConnectionReceiveWindow, self.quicConfig.MaxConnectionReceiveWindow)
	if config.MaxIncomingStreams == 0 || self.quicConfig.MaxIncomingStreams < config.MaxIncomingStreams {
		config.MaxIncomingStreams = self.quicConfig.MaxIncomingStreams
	}
	if config.MaxIncomingUniStreams == 0 || self.quicConfig.MaxIncomingUniStreams < config.MaxIncomingUniStreams {
		config.MaxIncomingUniStreams = self.quicConfig.MaxIncomingUniStreams
	}
}
