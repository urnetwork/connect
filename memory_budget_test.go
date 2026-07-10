package connect

import (
	"testing"

	"github.com/go-playground/assert/v2"
)

func TestMemoryBudgetUnsetDefaults(t *testing.T) {
	// no budget leaves every default unscaled
	SetMemoryBudget(0)
	defer SetMemoryBudget(0)

	assert.Equal(t, DefaultSendBufferSettings().ResendQueueMaxByteCount, mib(2))
	assert.Equal(t, DefaultReceiveBufferSettings().ReceiveQueueMaxByteCount, mib(2)+kib(512))
	assert.Equal(t, DefaultWebRtcSettings().ReceiveBufferSize, mib(4))
	assert.Equal(t, DefaultDohSettings().CacheMaxEntries, 4096)
	tunSettings := DefaultTunSettings()
	assert.Equal(t, tunSettings.UdpReceiveBufferByteCount, int(mib(1)))
	assert.Equal(t, tunSettings.TcpReceiveBuffer.Default, int(kib(256)))
	assert.Equal(t, tunSettings.TcpReceiveBuffer.Max, int(mib(1)))
}

func TestMemoryBudgetScaledSettings(t *testing.T) {
	// half the reference budget scales the memory-dominant defaults by half
	SetMemoryBudget(mib(32))
	defer SetMemoryBudget(0)

	assert.Equal(t, DefaultSendBufferSettings().ResendQueueMaxByteCount, mib(1))
	assert.Equal(t, DefaultReceiveBufferSettings().ReceiveQueueMaxByteCount, (mib(2)+kib(512))/2)
	assert.Equal(t, DefaultWebRtcSettings().ReceiveBufferSize, mib(2))
	assert.Equal(t, DefaultDohSettings().CacheMaxEntries, 2048)
	tunSettings := DefaultTunSettings()
	assert.Equal(t, tunSettings.UdpReceiveBufferByteCount, int(kib(512)))
	assert.Equal(t, tunSettings.TcpReceiveBuffer.Default, int(kib(128)))
	assert.Equal(t, tunSettings.TcpReceiveBuffer.Max, int(kib(512)))
}

func TestMemoryBudgetFloors(t *testing.T) {
	// a tiny budget clamps every scaled setting to its working floor
	SetMemoryBudget(mib(1))
	defer SetMemoryBudget(0)

	assert.Equal(t, DefaultSendBufferSettings().ResendQueueMaxByteCount, kib(256))
	assert.Equal(t, DefaultReceiveBufferSettings().ReceiveQueueMaxByteCount, kib(320))
	assert.Equal(t, DefaultWebRtcSettings().ReceiveBufferSize, mib(1))
	assert.Equal(t, DefaultDohSettings().CacheMaxEntries, 512)
	assert.Equal(t, DefaultTunSettings().TcpReceiveBuffer.Max, int(kib(256)))

	// budgets above the reference never scale up
	SetMemoryBudget(mib(1024))
	assert.Equal(t, DefaultSendBufferSettings().ResendQueueMaxByteCount, mib(2))
}

func TestResizeMessagePoolsSplitsBudget(t *testing.T) {
	// the free-list byte budget is split evenly across the size classes
	defer ResizeMessagePools(InitialMessagePoolByteCount)

	ResizeMessagePools(mib(4))
	pools := orderedMessagePools()
	assert.Equal(t, len(pools), 2)
	assert.Equal(t, len(pools[0].pool), int(mib(2))/pools[0].size)
	assert.Equal(t, len(pools[1].pool), int(mib(2))/pools[1].size)
}
