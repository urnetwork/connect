package connect

import (
	"testing"
)

func TestMemoryBudgetUnsetDefaults(t *testing.T) {
	// no budget leaves every default unscaled
	SetMemoryBudget(0)
	defer SetMemoryBudget(0)

	AssertEqual(t, DefaultSendBufferSettings().ResendQueueMaxByteCount, mib(2))
	AssertEqual(t, DefaultReceiveBufferSettings().ReceiveQueueMaxByteCount, mib(2)+kib(512))
	AssertEqual(t, DefaultWebRtcSettings().ReceiveBufferSize, mib(4))
	AssertEqual(t, DefaultDohSettings().CacheMaxEntries, 4096)
	tunSettings := DefaultTunSettings()
	AssertEqual(t, tunSettings.UdpReceiveBufferByteCount, int(mib(1)))
	AssertEqual(t, tunSettings.TcpReceiveBuffer.Default, int(kib(256)))
	AssertEqual(t, tunSettings.TcpReceiveBuffer.Max, int(mib(1)))
}

func TestMemoryBudgetScaledSettings(t *testing.T) {
	// half the reference budget scales the memory-dominant defaults by half
	SetMemoryBudget(mib(32))
	defer SetMemoryBudget(0)

	AssertEqual(t, DefaultSendBufferSettings().ResendQueueMaxByteCount, mib(1))
	AssertEqual(t, DefaultReceiveBufferSettings().ReceiveQueueMaxByteCount, (mib(2)+kib(512))/2)
	AssertEqual(t, DefaultWebRtcSettings().ReceiveBufferSize, mib(2))
	AssertEqual(t, DefaultDohSettings().CacheMaxEntries, 2048)
	tunSettings := DefaultTunSettings()
	AssertEqual(t, tunSettings.UdpReceiveBufferByteCount, int(kib(512)))
	AssertEqual(t, tunSettings.TcpReceiveBuffer.Default, int(kib(128)))
	AssertEqual(t, tunSettings.TcpReceiveBuffer.Max, int(kib(512)))
}

func TestMemoryBudgetFloors(t *testing.T) {
	// a tiny budget clamps every scaled setting to its working floor
	SetMemoryBudget(mib(1))
	defer SetMemoryBudget(0)

	AssertEqual(t, DefaultSendBufferSettings().ResendQueueMaxByteCount, kib(256))
	AssertEqual(t, DefaultReceiveBufferSettings().ReceiveQueueMaxByteCount, kib(320))
	AssertEqual(t, DefaultWebRtcSettings().ReceiveBufferSize, mib(1))
	AssertEqual(t, DefaultDohSettings().CacheMaxEntries, 512)
	AssertEqual(t, DefaultTunSettings().TcpReceiveBuffer.Max, int(kib(256)))

	// budgets above the reference never scale up
	SetMemoryBudget(mib(1024))
	AssertEqual(t, DefaultSendBufferSettings().ResendQueueMaxByteCount, mib(2))
}

func TestResizeMessagePoolsSplitsBudget(t *testing.T) {
	// the free-list byte budget is split evenly across the size classes
	defer ResizeMessagePools(InitialMessagePoolByteCount)

	ResizeMessagePools(mib(4))
	pools := orderedMessagePools()
	AssertEqual(t, len(pools), 2)
	AssertEqual(t, len(pools[0].pool), int(mib(2))/pools[0].size)
	AssertEqual(t, len(pools[1].pool), int(mib(2))/pools[1].size)
}
