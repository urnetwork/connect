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
	AssertEqual(t, DefaultWebRtcSettings().ReceiveBufferSize, kib(512))
	AssertEqual(t, DefaultDohSettings().CacheMaxEntries, 4096)
	platformBudget := DefaultPlatformTransportBudget()
	if platformBudget == nil {
		t.Fatal("unscaled defaults did not install the aggregate platform budget")
	}
	AssertEqual(t, platformBudget.Stats().TotalByteCount, mib(16))
	AssertEqual(t, platformBudget.Stats().MaxTransportCount, 16)
	firstPlatformBudget := DefaultPlatformTransportSettings().PlatformTransportBudget
	secondPlatformBudget := DefaultPlatformTransportSettings().PlatformTransportBudget
	if firstPlatformBudget != secondPlatformBudget {
		t.Fatal("default platform settings did not share one process budget")
	}
	AssertEqual(t, DefaultMultiClientSettings().EvaluationPoolMultiple, 2)
	tunSettings := DefaultTunSettings()
	AssertEqual(t, tunSettings.UdpReceiveBufferByteCount, int(mib(1)))
	// 2026-08: raised so a single stream's auto-tuned window can cover the
	// tunnel's bandwidth-delay product (see DefaultTunSettingsWithBufferSize)
	AssertEqual(t, tunSettings.TcpReceiveBuffer.Default, int(mib(1)))
	AssertEqual(t, tunSettings.TcpReceiveBuffer.Max, int(mib(4)))
}

func TestMemoryBudgetScaledSettings(t *testing.T) {
	// half the reference budget scales the memory-dominant defaults by half
	SetMemoryBudget(mib(32))
	defer SetMemoryBudget(0)

	AssertEqual(t, DefaultSendBufferSettings().ResendQueueMaxByteCount, mib(1))
	AssertEqual(t, DefaultReceiveBufferSettings().ReceiveQueueMaxByteCount, (mib(2)+kib(512))/2)
	AssertEqual(t, DefaultWebRtcSettings().ReceiveBufferSize, kib(256))
	AssertEqual(t, DefaultDohSettings().CacheMaxEntries, 2048)
	platformBudget := DefaultPlatformTransportBudget()
	if platformBudget == nil {
		t.Fatal("scaled process budget did not create a shared platform transport budget")
	}
	platformStats := platformBudget.Stats()
	AssertEqual(t, platformStats.TotalByteCount, mib(8))
	AssertEqual(t, platformStats.MaxTransportCount, 16)
	platformSettings := DefaultPlatformTransportSettings()
	if platformSettings.PlatformTransportBudget != platformBudget {
		t.Fatal("platform settings did not use the current shared budget")
	}
	AssertEqual(t, platformSettings.H1BudgetByteCount, kib(256))
	AssertEqual(t, platformSettings.H3BudgetByteCount, mib(4))
	AssertEqual(t, platformSettings.H3SocketReadBufferByteCount, kib(512))
	AssertEqual(t, platformSettings.H3SocketWriteBufferByteCount, kib(512))
	if platformStats.TotalByteCount <
		platformSettings.H1BudgetByteCount+platformSettings.H3BudgetByteCount {
		t.Fatal("32 MiB Auto budget cannot fit one H1 and one H3 carrier")
	}
	AssertEqual(t, DefaultMultiClientSettings().EvaluationPoolMultiple, 1)
	packetTranslationSettings := DefaultPacketTranslationSettings()
	AssertEqual(t, packetTranslationSettings.DnsMaxCombineBytes, mib(1))
	AssertEqual(t, packetTranslationSettings.DnsMaxCombineBytesPerAddress, kib(128))
	AssertEqual(t, packetTranslationSettings.DnsMaxPumpHostsPerAddress, 512)
	AssertEqual(t, packetTranslationSettings.DnsMaxPumpHosts, int64(4096))
	tunSettings := DefaultTunSettings()
	AssertEqual(t, tunSettings.UdpReceiveBufferByteCount, int(kib(512)))
	AssertEqual(t, tunSettings.TcpReceiveBuffer.Default, int(kib(512)))
	AssertEqual(t, tunSettings.TcpReceiveBuffer.Max, int(mib(2)))
}

func TestMemoryBudgetFloors(t *testing.T) {
	// a tiny budget clamps every scaled setting to its working floor
	SetMemoryBudget(mib(1))
	defer SetMemoryBudget(0)

	AssertEqual(t, DefaultSendBufferSettings().ResendQueueMaxByteCount, kib(256))
	AssertEqual(t, DefaultReceiveBufferSettings().ReceiveQueueMaxByteCount, kib(320))
	AssertEqual(t, DefaultWebRtcSettings().ReceiveBufferSize, kib(256))
	AssertEqual(t, DefaultDohSettings().CacheMaxEntries, 512)
	AssertEqual(t, DefaultTunSettings().TcpReceiveBuffer.Max, int(kib(512)))

	// The smallest supported host target can still admit one explicitly
	// selected H3 carrier; its aggregate budget and reservation share a floor.
	SetMemoryBudget(mib(8))
	legacyPlatformBudget := DefaultPlatformTransportBudget().Stats()
	legacyPlatformSettings := DefaultPlatformTransportSettings()
	AssertEqual(t, legacyPlatformBudget.TotalByteCount, mib(3))
	AssertEqual(t, legacyPlatformSettings.H3BudgetByteCount, mib(3))
	if legacyPlatformSettings.H1BudgetByteCount+legacyPlatformSettings.H3BudgetByteCount <=
		legacyPlatformBudget.TotalByteCount {
		t.Fatal("8 MiB Auto budget unexpectedly admitted H1 and H3 together")
	}

	// budgets above the reference never scale up
	SetMemoryBudget(mib(1024))
	AssertEqual(t, DefaultSendBufferSettings().ResendQueueMaxByteCount, mib(2))
}

func TestResizeMessagePoolsSplitsBudget(t *testing.T) {
	// the packet class takes the packet byte budget; the large object
	// classes split the large object byte budget evenly
	defer ResizeMessagePools(InitialMessagePoolByteCount/2, InitialMessagePoolByteCount/2)

	ResizeMessagePools(mib(4), mib(2))
	pools := orderedMessagePools()
	AssertEqual(t, len(pools), 3)
	AssertEqual(t, pools[0].size, packetPoolSize)
	AssertEqual(t, pools[0].capacity(), int(mib(4))/pools[0].size)
	for _, pool := range pools[1:] {
		AssertEqual(t, pool.capacity(), int(mib(2))/len(pools[1:])/pool.size)
	}

	// tiny budgets clamp to the retention floors
	ResizeMessagePools(0, 0)
	AssertEqual(t, pools[0].capacity(), packetPoolFloorCount)
	for _, pool := range pools[1:] {
		AssertEqual(t, pool.capacity(), largeObjectPoolFloorCount(pool.size))
	}

	// the historical one-argument API gives every class the supplied cap
	ResizeMessagePools(mib(3))
	for _, pool := range pools {
		AssertEqual(t, pool.capacity(), int(mib(3))/pool.size)
	}
}

func TestTrimMessagePoolsToWarmPreservesCapacity(t *testing.T) {
	defer ResizeMessagePools(InitialMessagePoolByteCount/2, InitialMessagePoolByteCount/2)
	defer ClearMessagePools()

	ResizeMessagePools(mib(4), mib(2))
	pools := orderedMessagePools()
	for _, pool := range pools {
		pool.Clear()
		messages := make([][]byte, pool.capacity())
		for i := range messages {
			messages[i] = pool.take(pool.size, 0)
		}
		for _, message := range messages {
			pool.release(message[:cap(message)])
		}
	}

	capacities := make([]int, len(pools))
	for i, pool := range pools {
		capacities[i] = pool.capacity()
	}
	TrimMessagePoolsToWarm()

	for i, pool := range pools {
		snapshot := pool.snapshot()
		AssertEqual(t, snapshot.capacity, capacities[i])
		if pool.size == packetPoolSize {
			AssertEqual(t, snapshot.retained, min(capacities[i]/4, int(mib(1))/pool.size))
		} else {
			AssertEqual(t, snapshot.retained, largeObjectPoolFloorCount(pool.size))
		}

		// Trimming is not a permanent cap reduction: one take/return can grow
		// the free list again, up to the unchanged configured capacity.
		messages := make([][]byte, snapshot.retained+1)
		for j := range messages {
			messages[j] = pool.take(pool.size, 0)
		}
		for _, message := range messages {
			pool.release(message[:cap(message)])
		}
		AssertEqual(t, pool.snapshot().retained, snapshot.retained+1)
	}
}

func TestMessagePoolCapacitySlotsGrowLazilyAndReclaim(t *testing.T) {
	const logicalCapacity = 1 << 20
	pool := newMessagePool(packetPoolSize, logicalCapacity)
	AssertEqual(t, pool.capacity(), logicalCapacity)
	for shardIndex := range messagePoolShardCount {
		AssertEqual(t, len(pool.shards[shardIndex].pool), 0)
	}

	message := pool.take(pool.size, 0)
	pool.release(message[:cap(message)])
	allocatedSlotCount := 0
	for shardIndex := range messagePoolShardCount {
		allocatedSlotCount += len(pool.shards[shardIndex].pool)
	}
	AssertEqual(t, allocatedSlotCount, 1)

	pool.trim(0)
	AssertEqual(t, pool.capacity(), logicalCapacity)
	for shardIndex := range messagePoolShardCount {
		AssertEqual(t, len(pool.shards[shardIndex].pool), 0)
	}

	message = pool.take(pool.size, 0)
	pool.release(message[:cap(message)])
	pool.Clear()
	AssertEqual(t, pool.capacity(), logicalCapacity)
	for shardIndex := range messagePoolShardCount {
		AssertEqual(t, len(pool.shards[shardIndex].pool), 0)
	}

	boundedPool := newMessagePool(packetPoolSize, messagePoolShardCount)
	messages := make([][]byte, 2*messagePoolShardCount)
	for index := range messages {
		messages[index] = boundedPool.take(boundedPool.size, 0)
	}
	for _, boundedMessage := range messages {
		boundedPool.release(boundedMessage[:cap(boundedMessage)])
	}
	AssertEqual(t, boundedPool.snapshot().retained, messagePoolShardCount)
	AssertEqual(t, boundedPool.capacity(), messagePoolShardCount)
}

func TestMessagePoolAggregateStatsDoesNotAllocate(t *testing.T) {
	var stats MessagePoolAggregateStats
	allocations := testing.AllocsPerRun(100, func() {
		stats = GetMessagePoolAggregateStats()
	})
	if stats.CapacityByteCount <= 0 {
		t.Fatal("aggregate pool stats reported no configured capacity")
	}
	if allocations != 0 {
		t.Fatalf("GetMessagePoolAggregateStats allocated %.0f objects, want 0", allocations)
	}
}
