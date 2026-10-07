package connect

import (
	"fmt"
	"testing"
	"testing/synctest"
	"time"
)

// The sender's working floor cannot override a smaller receiver's explicit
// limit. Exercise both directions before RTT sampling and after delivery.
func TestWindowMismatchRespectsBothEndpoints(t *testing.T) {
	for _, sendWindow := range []ByteCount{kib(64), kib(256), mib(2), mib(48)} {
		for _, receiveWindow := range []ByteCount{kib(64), kib(256), mib(2), mib(48)} {
			t.Logf("case: %s", fmt.Sprintf("send=%d/receive=%d", sendWindow, receiveWindow))
			synctest.Test(t, func(t *testing.T) {
				sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
					settings.DeliverySizedWindowScale = 2
					settings.ResendQueueMaxByteCount = sendWindow
					settings.DeliverySizedWindowCeilingByteCount = sendWindow
					settings.ResendQueueBudget = NewTransferMemoryBudget(sendWindow)
					settings.TargetGoodputByteRate = 0
				})
				for _, hold := range []ByteCount{receiveWindow, kib(32), receiveWindow} {
					sequence.observeReceiveWindowAdvertisement(receiveAckMessage{
						receiveWindowSet: true, receiveWindowByteCount: uint32(hold),
						ackCompressTimeoutSet: true, ackCompressTimeoutMicros: 10000,
					})
					for phase := range 3 {
						if phase > 0 {
							sampleRoundTrip(sequence, 100*time.Millisecond)
						}
						if phase == 2 {
							for range 30 {
								sequence.observeDeliveredBytes(1000, time.Now())
								time.Sleep(10 * time.Millisecond)
							}
						}
						estimate := sequence.sendWindowEstimate(time.Now())
						if estimate.Window <= 0 || estimate.Window > min(sendWindow, hold) {
							t.Errorf("phase=%d sender=%d receiver=%d window=%d reason=%s", phase, sendWindow, hold, estimate.Window, estimate.Reason)
						}
					}
					// Start the next advertised limit without delivery history.
					sequence.deliveredBytesCount = 0
				}
			})
		}
	}
}

// A matched constant-window control measures the smaller endpoint's actual
// ceiling. Requiring line rate would be wrong when that window cannot cover
// one bandwidth-delay product; losing capacity to a second pacing ramp is not.
func TestWindowPathWindowMismatchMatrix(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, cell := range windowMismatchMatrixCells() {
		t.Logf("case: send=%d/receive=%d/rtt=%s/compression=%s/flows=%d", cell.SendWindow, cell.ReceiveWindow, cell.RoundTrip, cell.Compression, cell.Flows)
		checkWindowMismatchCellWithMinimumDuration(t, cell, 100*time.Millisecond)
	}
}

// Covers every pair of endpoint/path dimension values and the explicit
// boundary corners checked by TestWindowPathWindowMismatchMatrixCoverage.
// The full product repeated millions of real packet operations under race;
// advancing a synthetic clock does not eliminate that work.
func windowMismatchMatrixCells() []windowPathCell {
	cells := []windowPathCell{
		{SendWindow: kib(256), ReceiveWindow: kib(256), RoundTrip: 300 * time.Microsecond, Flows: 1},
		{SendWindow: mib(48), ReceiveWindow: mib(48), RoundTrip: 400 * time.Millisecond, Compression: 10 * time.Millisecond, Flows: 8},
		{SendWindow: mib(48), ReceiveWindow: kib(256), RoundTrip: 400 * time.Millisecond, Compression: 10 * time.Millisecond, Flows: 8},
		{SendWindow: kib(256), ReceiveWindow: mib(48), RoundTrip: 400 * time.Millisecond, Compression: 10 * time.Millisecond, Flows: 8},
		{SendWindow: mib(48), ReceiveWindow: kib(256), RoundTrip: 300 * time.Microsecond, Flows: 1},
		{SendWindow: kib(256), ReceiveWindow: mib(48), RoundTrip: 300 * time.Microsecond, Flows: 1},
		{SendWindow: mib(48), ReceiveWindow: mib(48), RoundTrip: 300 * time.Microsecond, Compression: 10 * time.Millisecond, Flows: 8},
		{SendWindow: kib(256), ReceiveWindow: kib(256), RoundTrip: 400 * time.Millisecond, Flows: 8},
		{SendWindow: mib(2), ReceiveWindow: mib(2), RoundTrip: 100 * time.Millisecond, Compression: 10 * time.Millisecond, Flows: 1},
		{SendWindow: kib(256), ReceiveWindow: mib(2), RoundTrip: 100 * time.Millisecond, Flows: 8},
		{SendWindow: mib(2), ReceiveWindow: kib(256), RoundTrip: 300 * time.Microsecond, Flows: 8},
		{SendWindow: mib(2), ReceiveWindow: mib(2), RoundTrip: 400 * time.Millisecond, Flows: 1},
		{SendWindow: mib(2), ReceiveWindow: mib(48), RoundTrip: 100 * time.Millisecond, Flows: 1},
		{SendWindow: mib(48), ReceiveWindow: kib(256), RoundTrip: 100 * time.Millisecond, Flows: 1},
		{SendWindow: mib(48), ReceiveWindow: mib(2), RoundTrip: 300 * time.Microsecond, Flows: 1},
	}
	for i := range cells {
		cells[i].Rate = 125000000
	}
	return cells
}

// Receiver budgets can change while a sequence is live. Keep the same
// serializer and compare to its final window in both change directions.
func TestWindowPathWindowMismatchChanges(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, windows := range [][2]ByteCount{{kib(64), mib(2)}, {mib(2), kib(64)}} {
		for _, compression := range []time.Duration{0, 10 * time.Millisecond, 50 * time.Millisecond} {
			t.Logf("case: %s", fmt.Sprintf("receive=%d-to-%d/compression=%s", windows[0], windows[1], compression))
			checkWindowMismatchCell(t, windowPathCell{SendWindow: mib(48), ReceiveWindow: windows[0], ReceiveWindowAfter: windows[1], WindowChangeAfter: 2 * time.Second, Warmup: 4 * time.Second, RoundTrip: 100 * time.Millisecond, Compression: compression, Flows: 8, Rate: 12500000})
		}
	}
}

// The reference and delivery arms use identical endpoint limits and offered
// traffic. Only the window and pacing rule differs.
func checkWindowMismatchCell(t *testing.T, cell windowPathCell) {
	t.Helper()
	checkWindowMismatchCellWithMinimumDuration(t, cell, time.Second)
}

// The pairwise sweep uses the same short-path floor as the adjacent path
// matrix. Window-limited flights retain twenty full residences; callers that
// measure changing budgets keep their original one-second minimum.
func checkWindowMismatchCellWithMinimumDuration(t *testing.T, cell windowPathCell, minimumDuration time.Duration) {
	t.Helper()
	cell.Budget, cell.Payload = mib(48), 1280
	cell.RoundRobinOffer = true
	finalReceive := cell.ReceiveWindow
	if cell.ReceiveWindowAfter > 0 {
		finalReceive = cell.ReceiveWindowAfter
	}
	var ceiling, fixed windowPathReading
	duration := max(minimumDuration, 2*cell.RoundTrip)
	residence := cell.RoundTrip + cell.Compression
	windowRate := float64(min(cell.SendWindow, finalReceive)) / residence.Seconds()
	if windowRate < float64(cell.Rate)/2 {
		// Short windows deliver in flights. Twenty residences keep one
		// flight's phase at the measurement edge below five percent.
		duration = max(duration, 20*residence)
	}
	for _, arm := range []string{"ceiling", "delivery"} {
		started := time.Now()
		synctest.Test(t, func(t *testing.T) {
			trial := cell
			trial.Arm, trial.Drop = arm, arm == "delivery"
			if arm == "ceiling" {
				trial.CalibrationWindow = min(cell.SendWindow, finalReceive)
				trial.ReceiveWindow, trial.ReceiveWindowAfter = finalReceive, 0
			}
			reading := measureWindowPathCell(t, trial, duration)
			logWindowServiceReading(t, reading)
			if arm == "ceiling" {
				ceiling = reading
			} else {
				fixed = reading
			}
		})
		t.Logf("mismatch arm=%s wall-elapsed=%s", arm, time.Since(started))
	}
	t.Logf("mismatch send=%d receive=%d final-receive=%d rtt=%s compression=%s flows=%d ceiling=%.3f fixed=%.3f min-flow=%.3f model Mb/s window=%d pace=%d drops=%d/%d", cell.SendWindow, cell.ReceiveWindow, finalReceive, cell.RoundTrip, cell.Compression, cell.Flows, ceiling.Mbps, fixed.Mbps, fixed.MinFlowMbps, fixed.Window.Window, fixed.Window.PacingByteRate, fixed.RelayDrops, fixed.MeasurementRelayDrops)
	if ceiling.Mbps < .85*min(float64(cell.Rate), windowRate)*8/1e6 || fixed.Mbps < .90*ceiling.Mbps || fixed.MinFlowMbps == 0 || fixed.Window.Window > min(cell.SendWindow, finalReceive) || fixed.MeasurementRelayDrops != 0 || fixed.Receiver.ReceiveQueueDropCount != 0 || fixed.Receiver.ReceiveQueueEvictionCount != 0 {
		t.Error("mismatched windows lost attainable capacity, violated a limit, dropped data or stalled a flow")
	}
}
