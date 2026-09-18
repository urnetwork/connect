// Replay the two short-path compressed-feedback failures directly. The
// compressed-flight and successful-drain roots isolate their gap accounting;
// this fixture checks that the actual workers retain the known service rate.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// Keep the original matrix's serializer, queues, warmup and acceptance gates.
// A low final rate is a failure even if previously queued data masks it briefly.
func TestWindowPacingShortCompressedServiceKeepsMeasuredCapacity(t *testing.T) {
	assertMessagePoolOwnership(t)
	const rate ByteCount = 1250000
	const rtt = 300 * time.Microsecond
	const compression = 50 * time.Millisecond
	warmup := max(300*time.Millisecond+5*rtt, time.Duration(2*int64(mib(2))*int64(time.Second)/int64(rate))+5*rtt)
	for _, flows := range []int{1, 8} {
		var reference, candidate windowPathReading
		for _, arm := range []string{"ceiling", "delivery"} {
			synctest.Test(t, func(t *testing.T) {
				reading := measureWindowPathCell(t, windowPathCell{
					Arm: arm, RoundTrip: rtt, Compression: compression,
					Flows: flows, RoundRobinOffer: true, Payload: 1280,
					Budget: mib(48), Rate: rate, Drop: arm == "delivery", Warmup: warmup,
				}, max(time.Second, 2*rtt+4*compression))
				logWindowServiceReading(t, reading)
				if arm == "ceiling" {
					reference = reading
				} else {
					candidate = reading
				}
			})
		}
		if reference.Mbps < .90*float64(rate)*8/1e6 {
			t.Fatalf("flows=%d serializer reference missed its capacity: %.6f Mb/s", flows, reference.Mbps)
		}
		if candidate.Mbps < .90*reference.Mbps || candidate.MinFlowMbps == 0 || candidate.MeasurementRelayDrops != 0 || candidate.MaxRelayQueued > 4096 || candidate.MaxRelayQueuedBytes > int64(mib(8)) {
			t.Errorf("flows=%d compressed feedback lost capacity or a finite bound: rate=%.6f reference=%.6f min-flow=%.6f drops=%d queue=%d/%d", flows, candidate.Mbps, reference.Mbps, candidate.MinFlowMbps, candidate.MeasurementRelayDrops, candidate.MaxRelayQueued, candidate.MaxRelayQueuedBytes)
		}
		if candidate.Window.ServiceByteRate < rate*9/10 {
			t.Errorf("flows=%d compressed feedback replaced the known serializer: service=%d physical=%d", flows, candidate.Window.ServiceByteRate, rate)
		}
	}
}
