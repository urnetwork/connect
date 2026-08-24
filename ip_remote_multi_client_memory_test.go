package connect

import (
	"testing"
)

func TestMultiClientMemorySnapshotIsPrimitiveAndAllocationFree(t *testing.T) {
	qualityClient := &multiClientChannel{}
	speedClient := &multiClientChannel{}
	flowUpdateA := &multiClientChannelUpdate{}
	flowUpdateB := &multiClientChannelUpdate{}
	multi := &RemoteUserNatMultiClient{
		windows: map[WindowType]*multiClientWindow{
			WindowTypeQuality: &multiClientWindow{
				clients: map[Id]*multiClientChannel{NewId(): qualityClient},
			},
			WindowTypeSpeed: &multiClientWindow{
				clients: map[Id]*multiClientChannel{NewId(): speedClient},
			},
		},
		flowUpdates: map[*multiClientChannelUpdate]bool{
			flowUpdateA: true,
			flowUpdateB: true,
		},
	}
	snapshot := multi.MemorySnapshot()
	if snapshot.QualityClientCount != 1 || snapshot.SpeedClientCount != 1 || snapshot.FlowCount != 2 {
		t.Fatalf("memory snapshot = %+v", snapshot)
	}
	if allocations := testing.AllocsPerRun(100, func() {
		_ = multi.MemorySnapshot()
	}); allocations != 0 {
		t.Fatalf("MemorySnapshot allocations/run = %.2f, want 0", allocations)
	}
}
