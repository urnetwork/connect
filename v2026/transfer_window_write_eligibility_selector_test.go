// Candidate-only structural control ties pacing facts to existing route filters.
package connect

import (
	"context"
	"testing"
)

// Compare both facts with the selector's actual ordinary/reliable-only write
// sets, including the reliable-only fallback and the limits of direct affinity.
func TestWindowPacingWriteEligibilityMatchesSelectedWriteSets(t *testing.T) {
	for _, test := range []struct {
		name       string
		transports []TransportType
		unreliable []bool
		priorities []int
	}{
		{name: "empty"},
		{name: "h1", transports: []TransportType{TransportTypeH1}, unreliable: []bool{false}},
		{name: "h3 stream", transports: []TransportType{TransportTypeH3}, unreliable: []bool{false}},
		{name: "h3 datagram fallback", transports: []TransportType{TransportTypeH3}, unreliable: []bool{true}},
		{name: "h1 preferred over h3 datagram", transports: []TransportType{TransportTypeH1, TransportTypeH3}, unreliable: []bool{false, true}},
		{name: "h3 datagram preferred over h1", transports: []TransportType{TransportTypeH3, TransportTypeH1}, unreliable: []bool{true, false}},
		{name: "h3 stream preferred over h1", transports: []TransportType{TransportTypeH3, TransportTypeH1}, unreliable: []bool{false, false}},
		{name: "h1 and datagram p2p", transports: []TransportType{TransportTypeH1, TransportTypeP2p}, unreliable: []bool{false, true}},
		{name: "h1 and reliable p2p", transports: []TransportType{TransportTypeH1, TransportTypeP2p}, unreliable: []bool{false, false}},
		{name: "unknown and h1", transports: []TransportType{TransportTypeUnknown, TransportTypeH1}, unreliable: []bool{false, false}},
		{name: "unequal direct priorities", transports: []TransportType{TransportTypeH1, TransportTypeH3}, unreliable: []bool{false, true}, priorities: []int{10, 20}},
	} {
		func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			selector := NewMultiRouteSelector(ctx, "write-eligibility", nil, TransferPath{}, true)
			defer selector.Close()
			for i, transportType := range test.transports {
				priority := 100
				if len(test.priorities) > 0 {
					priority = test.priorities[i]
				}
				selector.updateTransportWithProperties(
					newTypedPriorityRouteTestTransport(transportType, priority),
					[]Route{make(Route, 1)},
					TransferCarrierProperties{Unreliable: test.unreliable[i]},
				)
			}
			snapshot := selector.activeRoutesSnapshot.Load()
			policy := selector.transferFlightPolicy()
			if policy.generation != snapshot.generation || policy.notify != snapshot.notify {
				t.Fatalf("%s eligibility lost its generation/notification pair", test.name)
			}
			for _, reliableOnly := range []bool{false, true} {
				routes := snapshot.writeRoutesFor(TransportTypeUnknown, reliableOnly)
				want := len(routes) > 0
				for _, route := range routes {
					want = want && snapshot.transportType(route) == TransportTypeH1
				}
				got := policy.h1WriteOnly
				if reliableOnly {
					got = policy.h1ReliableWriteOnly
				}
				if got != want {
					t.Fatalf("%s reliableOnly=%t H1 eligibility=%t, actual write set requires %t", test.name, reliableOnly, got, want)
				}
			}
			allH1 := len(snapshot.routes) > 0
			for _, route := range snapshot.routes {
				allH1 = allH1 && snapshot.transportType(route) == TransportTypeH1
			}
			if policy.h1Only != allH1 {
				t.Fatalf("%s write eligibility changed aggregate MTU/window policy", test.name)
			}
			selector.updateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{make(Route, 1)})
			if next := selector.transferFlightPolicy(); next.generation == policy.generation ||
				snapshot.writeH1Only(false) != policy.h1WriteOnly || snapshot.writeH1Only(true) != policy.h1ReliableWriteOnly {
				t.Fatalf("%s new publication mutated the old eligibility facts", test.name)
			}
		}()
	}
}
