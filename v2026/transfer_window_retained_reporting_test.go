// Performance reports must distinguish qualified service growth from an
// opening that has never measured capacity.
package connect

import "testing"

// Current cumulative history may be too short even though a physical ACK
// train has independently established the serialization rate.
func TestWindowRetainedCampaignAcceptsServiceQualification(t *testing.T) {
	arm := chainArm{mode: WindowSizingFromDelivery}
	for _, estimate := range []SendWindowEstimate{
		{Window: 4 * 1024 * 1024, Sized: true},
		{Window: 4 * 1024 * 1024, ServiceSized: true},
		{Window: 4 * 1024 * 1024, Sized: true, ServiceSized: true},
		{Window: 2 * 1024 * 1024},
	} {
		readings := []map[string]chainReading{{
			arm.key(): {rate: 12500000, loadPeak: -1, window: estimate},
		}}
		summary := summarize(arm, readings, []bool{false}, 125000000)
		if summary.unsized != (!estimate.Sized && !estimate.ServiceSized) || len(summary.rates) != 1 || summary.windows[0] != estimate.Window {
			t.Errorf("campaign misclassified measured capacity: estimate=%+v summary=%+v", estimate, summary)
		}
	}
}
