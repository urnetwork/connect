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
				byteCount += self.h1BudgetByteCount()
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
// The fixed rows sum to 1600 KiB; they are not unused receive permission. Up
// through a 24-MiB target the 3-MiB claim therefore caps connection/stream
// credit at 1472/1104 KiB. In particular the 20-MiB iOS target must retain that
// claim so a nested DNS extender and pending H1 fit the 5-MiB child. Above
// 24 MiB through 32 MiB the claim instead adds those fixed rows to the base
// T/8 connection credit: Android's 28-MiB target keeps 3.5 MiB receive credit
// in a 5184-KiB claim inside its 7-MiB child. This is an explicit piecewise
// admission policy, not a process-target scaling cap. Larger/server targets
// retain their existing window policy, and unbudgeted defaults are unchanged.
func applyPlatformH3MemoryPolicy(settings *PlatformTransportSettings, target ByteCount) {
	settings.h3RetainedByteAccounting = 0 < target && target <= mib(32)
	if !settings.h3RetainedByteAccounting {
		return
	}
	settings.H3SocketReadBufferByteCount = min(settings.H3SocketReadBufferByteCount, platformH3SocketByteCount)
	settings.H3SocketWriteBufferByteCount = min(settings.H3SocketWriteBufferByteCount, platformH3SocketByteCount)
	fixed := platformH3FixedMemoryByteCount()
	if target <= mib(24) {
		connection := min(settings.H3MaxConnectionReceiveWindowByteCount, settings.H3BudgetByteCount-fixed)
		settings.H3MaxConnectionReceiveWindowByteCount = connection
		settings.H3MaxStreamReceiveWindowByteCount = min(settings.H3MaxStreamReceiveWindowByteCount, connection*3/4)
	} else {
		settings.H3BudgetByteCount = max(settings.H3BudgetByteCount,
			settings.H3MaxConnectionReceiveWindowByteCount+fixed)
	}
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
