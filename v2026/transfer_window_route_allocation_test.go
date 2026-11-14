// Candidate-only concrete entry; the preimage send census is in accounting tests.
package connect

import (
	"context"
	"testing"
)

// Compare the optional ready path with the existing selector entry using the
// actual compiler; source shape alone is not allocation evidence.
func TestWindowPacingRouteGenerationSelectorReadyAllocations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	selector := NewMultiRouteSelector(ctx, "route-allocation", nil, DestinationId(NewId()), true)
	defer selector.Close()
	route := make(Route, 1)
	selector.updateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{route})
	generation := selector.transferFlightPolicy().generation
	wire := make([]byte, 128)
	legacy := testing.AllocsPerRun(1000, func() {
		success, _, err := selector.writeDetailedWithCarrier(ctx, wire, -1)
		if !success || err != nil {
			panic("legacy ready selector failed")
		}
		<-route
	})
	observations := 0
	commit := func() { observations++ }
	protected := testing.AllocsPerRun(1000, func() {
		success, _, err := selector.writeDetailedWithPolicyGeneration(ctx, wire, -1, false, generation, commit)
		if !success || err != nil {
			panic("protected ready selector failed")
		}
		<-route
	})
	t.Logf("ready_selector_allocations legacy=%g protected=%g", legacy, protected)
	if legacy != 0 || protected != legacy || observations != 1001 {
		t.Fatal("generation-bound ready selector added allocation or duplicated dispatch observation")
	}
}
