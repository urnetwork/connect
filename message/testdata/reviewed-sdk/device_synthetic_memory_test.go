//go:build ignore

// Only this range statement is reviewed. Its iterator supplies pool accounting
// counters; the surrounding soak test remains fully subject to the gate.
package sdk

import (
	"testing"

	"github.com/urnetwork/connect"
)

func TestDeviceLocalSyntheticDeviceRemoteMemorySoak(t *testing.T) {
	for _, classStats := range connect.GetMessagePoolClassStats() {
		t.Logf(
			"[synthetic-device-mem] pool-class size=%d taken=%d returned=%d outstanding=%d",
			classStats.Size,
			classStats.Taken,
			classStats.Returned,
			classStats.Taken-classStats.Returned,
		)
	}
}
