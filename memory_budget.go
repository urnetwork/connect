package connect

import (
	"sync/atomic"
)

// An advisory process-wide default SIZING TARGET, not an admission budget.
// Constructors copy its current value into new per-instance queue, receive,
// socket and cache limits. Independent instances never share a mutable
// admission root through this target. It is separate from the Go runtime soft
// memory limit; hosts set both through sdk.SetMemoryLimit.
//
// A zero target (the default) leaves every setting at its unscaled default.
// Settings sample it when a Default*Settings constructor runs, so it must be
// set before constructing the objects it should size. The
// mobile hosts set it at process start, before any device exists.

// budgets at or above the reference use the unscaled defaults; smaller
// budgets scale the memory-dominant settings proportionally, down to
// per-setting floors
var referenceMemoryBudgetByteCount = mib(64)

var memoryBudgetByteCount atomic.Int64

func newDefaultPlatformTransportBudget(budgetByteCount ByteCount) *PlatformTransportBudget {
	return newDefaultPlatformTransportBudgetWithParent(budgetByteCount, nil)
}

func newDefaultPlatformTransportBudgetWithParent(
	budgetByteCount ByteCount,
	parent *PlatformTransportBudget,
) *PlatformTransportBudget {
	return newPlatformTransportBudget(
		platformTransportBudgetByteCountForMemoryTarget(budgetByteCount),
		16,
		parent,
	)
}

// platformTransportBudgetByteCountForMemoryTarget is the carrier budget one
// owner's memory target gets. Keep the normal share at one quarter, but leave
// room for one H3 carrier at the supported low-memory floor. Without this
// matching floor, an explicit H3 selection on the 8 MiB legacy host target
// would wait forever on a 2 MiB aggregate budget for a 3 MiB claim. The mobile
// H3 policy sizes its claims from this same total
// (ApplyMobilePlatformTransportMemoryPolicy).
func platformTransportBudgetByteCountForMemoryTarget(budgetByteCount ByteCount) ByteCount {
	return min(budgetByteCount, max(mib(3), budgetByteCount/4))
}

// NewPlatformTransportBudgetForMemoryTarget creates an independent carrier
// limit for one lifecycle owner. The owner passes the returned pointer to its
// descendant transports. A nonpositive target uses the unscaled default
// values, never a process-shared admission root.
func NewPlatformTransportBudgetForMemoryTarget(
	memoryTargetByteCount ByteCount,
) *PlatformTransportBudget {
	if memoryTargetByteCount <= 0 {
		return DefaultPlatformTransportBudget()
	}
	return newDefaultPlatformTransportBudget(memoryTargetByteCount)
}

// A phone's carrier budget has a carrier slot for each this many of its bytes
// (NewMobilePlatformTransportBudgetForMemoryTarget).
const mobilePlatformTransportBudgetSlotByteCount ByteCount = 512 * 1024

// NewMobilePlatformTransportBudgetForMemoryTarget is the carrier limit of a
// phone owner: the bytes NewPlatformTransportBudgetForMemoryTarget gives the
// target, and a carrier slot for each 512 KiB of them, never fewer than its
// 16. The mobile policy's carriers claim under a MiB each
// (ApplyMobilePlatformTransportMemoryPolicy), so 16 slots would bind before
// the bytes on a large target: the 16 MiB of a 64-MiB phone holds 18 H3
// carriers and 32 slots. A nonpositive target uses the unscaled default.
func NewMobilePlatformTransportBudgetForMemoryTarget(
	memoryTargetByteCount ByteCount,
) *PlatformTransportBudget {
	if memoryTargetByteCount <= 0 {
		return DefaultPlatformTransportBudget()
	}
	byteCount := platformTransportBudgetByteCountForMemoryTarget(memoryTargetByteCount)
	return newPlatformTransportBudget(
		byteCount,
		max(16, int(byteCount/mobilePlatformTransportBudgetSlotByteCount)),
		nil,
	)
}

// SetMemoryBudget sets only the process-wide default sizing target. It never
// creates or changes a shared admission budget. Zero disables scaling.
func SetMemoryBudget(budgetByteCount ByteCount) {
	memoryBudgetByteCount.Store(budgetByteCount)
}

func MemoryBudget() ByteCount {
	return memoryBudgetByteCount.Load()
}

// DefaultPlatformTransportBudget returns a fresh budget with default limit
// values. Callers that construct multiple transports for one lifecycle owner
// must retain and pass this pointer explicitly; separate calls never share
// admission capacity.
func DefaultPlatformTransportBudget() *PlatformTransportBudget {
	budgetByteCount := MemoryBudget()
	if budgetByteCount <= 0 {
		budgetByteCount = referenceMemoryBudgetByteCount
	}
	return newDefaultPlatformTransportBudget(budgetByteCount)
}

// memoryScale returns the budget scale in (0, 1]
func memoryTargetScale(budgetByteCount ByteCount) float64 {
	if budgetByteCount <= 0 || referenceMemoryBudgetByteCount <= budgetByteCount {
		return 1
	}
	return float64(budgetByteCount) / float64(referenceMemoryBudgetByteCount)
}

func memoryScale() float64 {
	return memoryTargetScale(memoryBudgetByteCount.Load())
}

// MemoryTargetScaledByteCount scales one default byte count from an explicit
// owner target instead of the process-global target. A nonpositive target
// retains the unscaled default; floorByteCount preserves the working minimum.
func MemoryTargetScaledByteCount(
	memoryTargetByteCount ByteCount,
	unscaledByteCount ByteCount,
	floorByteCount ByteCount,
) ByteCount {
	scaledByteCount := ByteCount(
		memoryTargetScale(memoryTargetByteCount) * float64(unscaledByteCount),
	)
	return max(floorByteCount, scaledByteCount)
}

// MemoryScaledByteCount scales a default byte count by the memory budget,
// with a floor that preserves a working minimum
func MemoryScaledByteCount(unscaledByteCount ByteCount, floorByteCount ByteCount) ByteCount {
	scaledByteCount := ByteCount(memoryScale() * float64(unscaledByteCount))
	return max(floorByteCount, scaledByteCount)
}

// MemoryScaledCount scales a default count by the memory budget,
// with a floor that preserves a working minimum
func MemoryScaledCount(unscaledCount int, floorCount int) int {
	return memoryTargetScaledCount(MemoryBudget(), unscaledCount, floorCount)
}

// Use the same count floor without consulting another owner's process target.
func memoryTargetScaledCount(memoryTargetByteCount ByteCount, unscaledCount int, floorCount int) int {
	scaledCount := int(memoryTargetScale(memoryTargetByteCount) * float64(unscaledCount))
	return max(floorCount, scaledCount)
}
