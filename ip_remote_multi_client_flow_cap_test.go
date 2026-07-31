package connect

import (
	"testing"
)

// flowCapTestClient builds a channel with n flows pinned to it in the parent's
// bookkeeping, which is what the cap reads.
func flowCapTestParent(t *testing.T, maxFlowsPerExit int, flowCounts ...int) (*RemoteUserNatMultiClient, []*multiClientChannel) {
	t.Helper()

	settings := DefaultMultiClientSettings()
	settings.MaxFlowsPerExit = maxFlowsPerExit

	parent := &RemoteUserNatMultiClient{
		settings:      settings,
		clientUpdates: map[*multiClientChannel]map[*multiClientChannelUpdate]bool{},
	}

	clients := []*multiClientChannel{}
	for _, count := range flowCounts {
		client := &multiClientChannel{settings: settings}
		updates := map[*multiClientChannelUpdate]bool{}
		for range count {
			updates[&multiClientChannelUpdate{}] = true
		}
		parent.clientUpdates[client] = updates
		clients = append(clients, client)
	}
	return parent, clients
}

// 0 is the shipped default and must leave selection exactly as it was.
func TestFlowCapZeroIsUnbounded(t *testing.T) {
	parent, clients := flowCapTestParent(t, 0, 500, 1, 0)

	under := parent.underFlowCap(clients)
	if len(under) != len(clients) {
		t.Errorf("cap 0 filtered %d of %d clients; it must be unbounded", len(clients)-len(under), len(clients))
	}
}

// The point of the cap: an exit already carrying its share stops attracting
// new flows, so one removal cannot take out everything at once.
func TestFlowCapExcludesFullExits(t *testing.T) {
	// cap 32: first is over, second is exactly at, third and fourth are under
	parent, clients := flowCapTestParent(t, 32, 157, 32, 31, 0)

	under := parent.underFlowCap(clients)
	if len(under) != 2 {
		t.Fatalf("got %d candidates, want 2 (only the two under the cap)", len(under))
	}
	if under[0] != clients[2] || under[1] != clients[3] {
		t.Error("wrong clients kept, or the caller's ordering was not preserved")
	}
}

// A cap bounds blast radius; it is not admission control. When everything is
// full the flow still has to go somewhere -- refusing would turn a slower page
// into a broken one.
func TestFlowCapNeverMakesAFlowUnroutable(t *testing.T) {
	parent, clients := flowCapTestParent(t, 8, 100, 50, 9)

	under := parent.underFlowCap(clients)
	if len(under) != len(clients) {
		t.Fatalf("got %d candidates, want all %d: every exit was full, so the flow must still be placed", len(under), len(clients))
	}
}

func TestFlowCapEmptyCandidates(t *testing.T) {
	parent, _ := flowCapTestParent(t, 32)

	if under := parent.underFlowCap([]*multiClientChannel{}); len(under) != 0 {
		t.Errorf("got %d candidates from an empty list", len(under))
	}
}

// The runtime override has to reach the cap, or the developer control silently
// does nothing -- a failure mode this code has already shipped more than once.
func TestFlowCapReadsTheRuntimeOverride(t *testing.T) {
	// four exits, two of them full: filtering leaves two, which is the
	// minimum the race needs, so the override's effect is observable
	parent, clients := flowCapTestParent(t, 0, 100, 100, 1, 2)

	// unbounded by settings
	if under := parent.underFlowCap(clients); len(under) != 4 {
		t.Fatalf("baseline: got %d, want 4", len(under))
	}

	// override installs a cap
	parent.SetReliabilitySettings(&ReliabilitySettings{MaxFlowsPerExit: 8})
	under := parent.underFlowCap(clients)
	if len(under) != 2 || under[0] != clients[2] || under[1] != clients[3] {
		t.Errorf("override ignored: got %d candidates, want the two under the cap", len(under))
	}

	// clearing it restores unbounded
	parent.SetReliabilitySettings(nil)
	if under := parent.underFlowCap(clients); len(under) != 4 {
		t.Errorf("clearing the override did not restore unbounded selection: got %d, want 4", len(under))
	}
}

// Filtering must never narrow the field to a single candidate. The send path
// takes a no-race single-client branch when offered exactly one, so a cap that
// leaves one exit standing silently disables the multi-exit race -- the
// mechanism the whole design rests on -- precisely when exits are busiest.
// Letting the cap slip is the cheaper failure.
func TestFlowCapNeverNarrowsToASingleCandidate(t *testing.T) {
	// only one exit is under the cap
	parent, clients := flowCapTestParent(t, 8, 100, 100, 1)

	under := parent.underFlowCap(clients)
	if len(under) != len(clients) {
		t.Errorf("cap narrowed to %d candidate(s), which disables racing; want all %d", len(under), len(clients))
	}
}
