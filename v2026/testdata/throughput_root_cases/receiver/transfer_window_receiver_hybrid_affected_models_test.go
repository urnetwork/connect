// Select previously failing cells without changing their physical model,
// offered traffic, warmup, measurement duration, or acceptance gates.
package connect

import (
	"testing"
	"time"
)

// Every short static mismatch that failed the full receiver prototype remains
// an independent calibrated pair under the narrower discovery handoff.
func TestWindowPathReceiverHybridStaticShortWindows(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, receiveWindow := range []ByteCount{kib(256), mib(2), mib(48)} {
		for _, flows := range []int{1, 8} {
			stop := startIngressCounterDiagnostic(t)
			checkWindowMismatchCell(t, windowPathCell{
				SendWindow: kib(256), ReceiveWindow: receiveWindow,
				RoundTrip: 300 * time.Microsecond, Compression: 10 * time.Millisecond,
				Flows: flows, Rate: 125000000,
			})
			stop()
		}
	}
}

// Preserve the exact SDK cell whose reverse direction failed the full matrix.
func receiverHybridSdkDuplexCell(t *testing.T) windowPathCell {
	t.Helper()
	profiles, digest := windowSdkProfiles(t)
	for i := range profiles {
		profile := &profiles[i]
		if profile.Name == "sdk-device-h1" && profile.MobilePolicy && profile.Providing {
			return windowPathCell{
				SenderProfile: &profiles[1], ReceiverProfile: profile,
				ProfileFixtureSha256: digest, Bidirectional: true,
				RoundTrip: 300 * time.Microsecond, Flows: 1,
				RoundRobinOffer: true, Payload: 1280, Rate: 125000000,
			}
		}
	}
	t.Fatal("missing exact synthetic SDK constructor profile")
	return windowPathCell{}
}

// The current ordinary sampler is a same-input control for receiver ownership.
func TestWindowPathReceiverHybridSdkDuplexOrdinary(t *testing.T) {
	assertMessagePoolOwnership(t)
	checkWindowSdkCell(t, receiverHybridSdkDuplexCell(t))
}

// Only the receiver evidence is enabled relative to the ordinary control.
func TestWindowPathReceiverHybridSdkDuplexDiagnostic(t *testing.T) {
	assertMessagePoolOwnership(t)
	defer startIngressCounterDiagnostic(t)()
	checkWindowSdkCell(t, receiverHybridSdkDuplexCell(t))
}
