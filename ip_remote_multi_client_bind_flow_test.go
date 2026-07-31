package connect

import (
	"context"
	"strings"
	"testing"
)

func bindFlowTestParent() *RemoteUserNatMultiClient {
	settings := DefaultMultiClientSettings()
	return &RemoteUserNatMultiClient{
		settings:      settings,
		clientUpdates: map[*multiClientChannel]map[*multiClientChannelUpdate]bool{},
	}
}

// bindFlowTestChannel carries a live ctx: bindClientFlow asks IsDone(), which
// reads it, and a bare struct would panic where production never can.
func bindFlowTestChannel(parent *RemoteUserNatMultiClient) *multiClientChannel {
	return &multiClientChannel{ctx: context.Background(), settings: parent.settings}
}

// A flow that wins its exit from an async race is committed without the send
// path ever seeing a transition, so it was never entered in clientUpdates.
// That map is read by the flow cap, by removeClient's teardown, and by the
// blast-radius metric -- an uncounted flow is uncapped, gets no teardown, and
// is invisible in the numbers. On device this showed as 14 exits with flows
// reported on one.
func TestBindClientFlowRecordsRaceAssignedFlow(t *testing.T) {
	parent := bindFlowTestParent()
	client := bindFlowTestChannel(parent)
	update := &multiClientChannelUpdate{}

	// the race stores the winner directly, as clientReceivePacket and
	// scheduleCompleteRace both do
	update.client.Store(client)

	if 0 != len(parent.clientUpdates[client]) {
		t.Fatal("baseline: the flow should not be recorded before binding")
	}

	parent.bindClientFlow(update, client)

	if 1 != len(parent.clientUpdates[client]) {
		t.Errorf("race-assigned flow not recorded: %d entries, want 1", len(parent.clientUpdates[client]))
	}
	if !parent.clientUpdates[client][update] {
		t.Error("the recorded entry is not this flow")
	}
}

// A re-raced flow moves between exits. A stale entry on the old exit would
// inflate its flow count, count against its cap, and hand it a teardown for a
// flow it no longer carries.
func TestBindClientFlowMovesFlowOffThePreviousExit(t *testing.T) {
	parent := bindFlowTestParent()
	oldClient := bindFlowTestChannel(parent)
	newClient := bindFlowTestChannel(parent)
	update := &multiClientChannelUpdate{}

	update.client.Store(oldClient)
	parent.bindClientFlow(update, oldClient)

	// re-raced onto another exit
	update.client.Store(newClient)
	parent.bindClientFlow(update, newClient)

	if _, ok := parent.clientUpdates[oldClient]; ok {
		t.Errorf("stale entry left on the previous exit: %d entries", len(parent.clientUpdates[oldClient]))
	}
	if 1 != len(parent.clientUpdates[newClient]) {
		t.Errorf("flow not recorded on the new exit: %d entries, want 1", len(parent.clientUpdates[newClient]))
	}
}

// The race stores its winner under the per-flow leaf lock and binds after
// releasing it, so the flow can move or die in between. Binding must not
// resurrect a flow that has since moved on.
func TestBindClientFlowIgnoresStaleWinner(t *testing.T) {
	parent := bindFlowTestParent()
	raceWinner := bindFlowTestChannel(parent)
	actualClient := bindFlowTestChannel(parent)
	update := &multiClientChannelUpdate{}

	// the flow has already moved on by the time the bind runs
	update.client.Store(actualClient)
	parent.bindClientFlow(update, raceWinner)

	if 0 != len(parent.clientUpdates[raceWinner]) {
		t.Errorf("stale race winner was recorded: %d entries, want 0", len(parent.clientUpdates[raceWinner]))
	}
}

// nil arguments are the normal case: both call sites pass whatever the closure
// left behind, and most packets commit nothing.
func TestBindClientFlowNilIsNoop(t *testing.T) {
	parent := bindFlowTestParent()
	client := bindFlowTestChannel(parent)

	parent.bindClientFlow(nil, client)
	parent.bindClientFlow(&multiClientChannelUpdate{}, nil)
	parent.bindClientFlow(nil, nil)

	if 0 != len(parent.clientUpdates) {
		t.Errorf("nil bind recorded something: %d clients", len(parent.clientUpdates))
	}
}

// Binding must decline an exit that was torn down while the race ran. The
// winner is chosen under the per-flow leaf lock and bound after releasing it,
// so removeClient can land in between -- recording onto a dead exit would hand
// it a teardown list it will never process.
func TestBindClientFlowDeclinesDoneClient(t *testing.T) {
	parent := bindFlowTestParent()
	ctx, cancel := context.WithCancel(context.Background())
	client := &multiClientChannel{ctx: ctx, settings: parent.settings}
	update := &multiClientChannelUpdate{}
	update.client.Store(client)

	cancel() // the exit is removed while the race is completing
	parent.bindClientFlow(update, client)

	if 0 != len(parent.clientUpdates[client]) {
		t.Errorf("bound a flow onto a removed exit: %d entries, want 0", len(parent.clientUpdates[client]))
	}
}

// The bind is only worth anything if the assignment sites call it. Both are
// async race paths whose winner the send path never sees as a transition, so
// nothing else records them -- and a helper that is correct but uncalled is
// the failure mode this codebase has shipped more than once. This reads the
// call sites directly rather than trusting that they exist.
func TestRaceAssignmentSitesBindTheFlow(t *testing.T) {
	source, err := readSource("ip_remote_multi_client.go")
	if err != nil {
		t.Fatal(err)
	}

	for _, site := range []struct{ fn, desc string }{
		{"func (self *RemoteUserNatMultiClient) clientReceivePacket(", "receive-path race lock-in"},
		{"func (self *RemoteUserNatMultiClient) scheduleCompleteRace(", "race completion"},
	} {
		body, ok := functionBody(source, site.fn)
		if !ok {
			t.Fatalf("could not find %s", site.fn)
		}
		if !strings.Contains(body, "bindClientFlow(") {
			t.Errorf("%s does not call bindClientFlow: flows it commits are never recorded, so they are uncapped and get no teardown", site.desc)
		}
	}

	// and the send path must route through the same helper rather than adding
	// locally, or a flow can end up recorded under two clients at once
	body, ok := functionBody(source, "func (self *RemoteUserNatMultiClient) sendClientPath(")
	if !ok {
		t.Fatal("could not find sendClientPath")
	}
	if !strings.Contains(body, "bindClientFlow(") {
		t.Error("sendClientPath does not use bindClientFlow, so bookkeeping is not single-sourced")
	}
}

// The flow cap reads clientUpdates, so a flow the bookkeeping never saw is a
// flow the cap cannot bound. This is why exits exceeded a cap of 16 in the
// field.
func TestBindClientFlowMakesRacedFlowsVisibleToTheCap(t *testing.T) {
	parent := bindFlowTestParent()
	parent.settings.MaxFlowsPerExit = 2
	client := bindFlowTestChannel(parent)

	for range 3 {
		update := &multiClientChannelUpdate{}
		update.client.Store(client)
		parent.bindClientFlow(update, client)
	}

	parent.stateLock.Lock()
	atCap := parent.clientAtFlowCapWithLock(client)
	parent.stateLock.Unlock()

	if !atCap {
		t.Errorf("exit carrying 3 raced flows is not at a cap of 2: counted %d", len(parent.clientUpdates[client]))
	}
}
