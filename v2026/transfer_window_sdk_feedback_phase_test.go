// Synthetic shared FIFO feedback recovery regression. The fixed initial
// ACK barrier forces the previously observed ordering without tracing reads.
package connect

import (
	"testing"
	"time"
)

// Once normal feedback resumes, the original per-direction capacity gate
// still applies after the original warmup and with the constructor budgets.
func TestWindowPathSdkDuplexRecoversAfterInitialFeedbackBarrier(t *testing.T) {
	assertMessagePoolOwnership(t)
	profiles, digest := windowSdkProfiles(t)
	checkWindowSdkCell(t, windowPathCell{
		SenderProfile: &profiles[1], ReceiverProfile: &profiles[10],
		ProfileFixtureSha256: digest, Bidirectional: true,
		RoundTrip: 300 * time.Microsecond, Flows: 8, RoundRobinOffer: true,
		Payload: 1280, Rate: 125000000,
		InitialLogicalAckRelease: 50*time.Millisecond + 100*time.Microsecond,
	})
}
