package connect

import (
	"sync/atomic"
)

// a process-wide memory budget that scales the default settings whose values
// dominate the per-connection and per-peer memory ceilings (queue caps,
// receive windows, socket buffers, cache bounds). The budget is advisory
// sizing state, separate from the go runtime soft memory limit; hosts set
// both together through the sdk (see sdk.SetMemoryLimit).
//
// A zero budget (the default) leaves every setting at its unscaled default.
// Settings sample the budget when a Default*Settings constructor runs, so the
// budget must be set before constructing the objects it should size. The
// mobile hosts set it at process start, before any device exists.

// budgets at or above the reference use the unscaled defaults; smaller
// budgets scale the memory-dominant settings proportionally, down to
// per-setting floors
var referenceMemoryBudgetByteCount = mib(64)

var memoryBudgetByteCount atomic.Int64

// SetMemoryBudget sets the process-wide memory budget that scales the
// memory-dominant default settings. 0 (the default) disables scaling.
func SetMemoryBudget(budgetByteCount ByteCount) {
	memoryBudgetByteCount.Store(budgetByteCount)
}

func MemoryBudget() ByteCount {
	return memoryBudgetByteCount.Load()
}

// memoryScale returns the budget scale in (0, 1]
func memoryScale() float64 {
	budgetByteCount := memoryBudgetByteCount.Load()
	if budgetByteCount <= 0 || referenceMemoryBudgetByteCount <= budgetByteCount {
		return 1
	}
	return float64(budgetByteCount) / float64(referenceMemoryBudgetByteCount)
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
	scaledCount := int(memoryScale() * float64(unscaledCount))
	return max(floorCount, scaledCount)
}
