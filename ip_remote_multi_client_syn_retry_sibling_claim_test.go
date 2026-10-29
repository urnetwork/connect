// A real queued sibling pins retry admission before either source can dequeue.
package connect

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

func TestNativeSynRetryQueuedSiblingDoesNotRejectItsOwnField(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness, path := synRetryControlHarness(t, 51421, nil, DefaultProtocolVersion)
		if !privateStaleBoundNativeSyn(t, harness, path) {
			t.Fatal("initial native SYN refused")
		}
		update := harness.update(path)
		owner := synRetryControlOwner(t, update)
		original := bytes.Clone(owner.packet)
		first := newGroupDispositionQueueFixture(t, nil, nil)
		admitted := make(chan *parsedPacketGroup, 1)
		second := newGroupDispositionQueueFixture(t, nil, func(settings *ClientSettings) {
			settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
				select {
				case admitted <- target.(*parsedPacketGroup):
				default:
				}
			}
		})
		first.selected.settings, second.selected.settings = first.parent.settings, second.parent.settings
		entered, release := make(chan struct{}, 1), make(chan struct{})
		var once sync.Once
		finish := func() { once.Do(func() { close(release) }) }
		t.Cleanup(finish)
		first.selected.beforeGroupCompletionForTest = func() {
			select {
			case entered <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-harness.multiClient.ctx.Done():
			}
		}
		harness.multiClient.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			return []*multiClientChannel{first.selected, second.selected}
		}
		harness.inject("a", reraceProviderSignal(t, path))
		synRetryControlEntered(t, entered)
		second.requireParked()
		var sibling *parsedPacketGroup
		select {
		case sibling = <-admitted:
		default:
			t.Fatal("second candidate never acquired real raw source admission")
		}
		update.stateLock.Lock()
		flags := sibling.completionFlags.Load()
		const raw = groupCompletionTracked | groupCompletionAdmitted | groupCompletionLinked | groupCompletionCandidate
		premise := update.synRetry == owner && owner.references == 2 && !owner.queued && update.client.Load() == nil &&
			update.race != nil && !update.race.single && len(update.race.clientStates) == 2 &&
			update.race.clientStates[first.selected] != nil && update.race.clientStates[second.selected] != nil &&
			flags&raw == raw && flags&(groupCompletionDequeued|groupCompletionDone|groupCompletionAbandoned) == 0 &&
			flags>>16 == update.race.collapseOrder && sibling.completionClient == second.selected && sibling.collapseSource != nil &&
			sibling.collapseAdmission.update == update && sibling.collapseAdmission.epoch == update.sequenceAdmissionEpoch &&
			sibling.synRetry != nil && sibling.synRetry.update == update && sibling.synRetry.serial == owner.serial &&
			len(sibling.packets) == 1 && sibling.packets[0].ipPath.Syn && !sibling.packets[0].ipPath.Ack &&
			sibling.packets[0].ipPath.SequenceNumber == owner.sequence && bytes.Equal(sibling.packets[0].packet, original) &&
			update.pendingCollapseCloseWithLock()
		validBefore := owner.validWithLock(update)
		update.stateLock.Unlock()
		if !premise || first.started.Load() != 0 || second.started.Load() != 1 || len(first.route) != 0 || len(second.route) != 0 {
			t.Fatal("exact same-owner/current-race undequeued sibling premise was not exercised")
		}
		finish()
		harness.settle()
		if first.started.Load() != 1 || second.started.Load() != 1 {
			t.Fatalf("RETAINED_SYN_SIBLING_CLAIM_REJECTED: exact same-owner sibling prevented first Pack admission; first=%d second=%d owner_valid_while_raw=%t", first.started.Load(), second.started.Load(), validBefore)
		}
		first.requireParked()
		// Materialize one source while its sibling remains raw. Retention uses
		// the same validity guard and must keep the exact original cache.
		first.unpark()
		first.acknowledgeAll(original)
		update.stateLock.Lock()
		retained := update.synRetry == owner && bytes.Equal(owner.packet, original) && owner.references == 1
		update.stateLock.Unlock()
		if !retained {
			t.Fatal("source materialization behind its raw sibling replaced or retired original custody")
		}
		second.unpark()
		second.acknowledgeAll(original)
		packet := MessagePoolCopy(reraceSynAck(path))
		defer MessagePoolReturn(packet)
		reply, err := ParseIpPath(packet)
		if err != nil {
			t.Fatal(err)
		}
		harness.multiClient.clientReceivePacket(second.selected, SourceId(second.peer), protocol.ProvideMode_Network, TransportTypeUnknown, reply, packet)
		harness.settle()
		if harness.boundTo(path) != second.selected || first.selected.dialFailureCount() != 0 || second.selected.dialFailureCount() != 0 {
			t.Fatal("real returned proof lost winner ownership or struck a healthy candidate")
		}
		synRetryControlReleased(t, owner)
		if err := harness.multiClient.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

// Detached classifier inputs pin each exception boundary without modifying a
// live admission, race, or source. The test above supplies the real raw claim.
func TestNativeSynRetryQueuedSiblingKeepsControlFences(t *testing.T) {
	type controlCase struct {
		name  string
		valid bool
		alter func(*multiClientChannelUpdate, *tcpSynRetry, *parsedPacketGroup)
	}
	const order = uint64(9)
	const raw = groupCompletionAck | groupCompletionTracked | groupCompletionAdmitted | groupCompletionLinked | groupCompletionCandidate
	for _, test := range []controlCase{
		{name: "same cached retry", valid: true},
		{name: "same partial race continuation", valid: true, alter: func(_ *multiClientChannelUpdate, _ *tcpSynRetry, group *parsedPacketGroup) {
			group.synRetry.raceOrder = order
		}},
		{name: "ordinary native serial", alter: func(_ *multiClientChannelUpdate, _ *tcpSynRetry, group *parsedPacketGroup) { group.synRetry.serial = 0 }},
		{name: "different cache serial", alter: func(_ *multiClientChannelUpdate, _ *tcpSynRetry, group *parsedPacketGroup) { group.synRetry.serial++ }},
		{name: "different request update", alter: func(_ *multiClientChannelUpdate, _ *tcpSynRetry, group *parsedPacketGroup) {
			group.synRetry.update = &multiClientChannelUpdate{}
		}},
		{name: "different claim update", alter: func(update *multiClientChannelUpdate, _ *tcpSynRetry, group *parsedPacketGroup) {
			// Keep this detached update eligible so the generic pending-control
			// predicate still sees the claim; only exact ownership must reject it.
			group.collapseAdmission.update = &multiClientChannelUpdate{race: update.race}
		}},
		{name: "stale admission epoch", alter: func(_ *multiClientChannelUpdate, _ *tcpSynRetry, group *parsedPacketGroup) {
			group.collapseAdmission.epoch--
		}},
		{name: "stale candidate race stamp", alter: func(_ *multiClientChannelUpdate, _ *tcpSynRetry, group *parsedPacketGroup) {
			group.completionFlags.Store(raw | (order-1)<<16)
		}},
		{name: "stale partial race request", alter: func(_ *multiClientChannelUpdate, _ *tcpSynRetry, group *parsedPacketGroup) {
			group.synRetry.raceOrder = order - 1
		}},
		{name: "single candidate race", alter: func(update *multiClientChannelUpdate, _ *tcpSynRetry, _ *parsedPacketGroup) {
			update.race.single = true
		}},
		{name: "different native SYN", alter: func(_ *multiClientChannelUpdate, _ *tcpSynRetry, group *parsedPacketGroup) {
			group.packets[0].ipPath.SequenceNumber++
		}},
		{name: "reset control", alter: func(_ *multiClientChannelUpdate, _ *tcpSynRetry, group *parsedPacketGroup) {
			group.packets[0].ipPath.Syn, group.packets[0].ipPath.Rst = false, true
		}},
		{name: "SYN with ACK", alter: func(_ *multiClientChannelUpdate, _ *tcpSynRetry, group *parsedPacketGroup) {
			group.packets[0].ipPath.Ack = true
		}},
		{name: "SYN with FIN", alter: func(_ *multiClientChannelUpdate, _ *tcpSynRetry, group *parsedPacketGroup) {
			group.packets[0].ipPath.Fin = true
		}},
		{name: "multiple packets", alter: func(_ *multiClientChannelUpdate, _ *tcpSynRetry, group *parsedPacketGroup) {
			group.packets = append(group.packets, group.packets[0])
		}},
		{name: "non TCP control", alter: func(_ *multiClientChannelUpdate, _ *tcpSynRetry, group *parsedPacketGroup) {
			group.packets[0].ipPath.Protocol = IpProtocolUdp
		}},
		{name: "missing retry identity", alter: func(_ *multiClientChannelUpdate, _ *tcpSynRetry, group *parsedPacketGroup) { group.synRetry = nil }},
		{name: "missing source", alter: func(_ *multiClientChannelUpdate, _ *tcpSynRetry, group *parsedPacketGroup) {
			group.collapseSource = nil
		}},
		{name: "cancelled source", alter: func(_ *multiClientChannelUpdate, _ *tcpSynRetry, group *parsedPacketGroup) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			group.collapseSource.ctx = ctx
		}},
		{name: "unreliable claim", alter: func(_ *multiClientChannelUpdate, _ *tcpSynRetry, group *parsedPacketGroup) {
			group.completionFlags.And(^groupCompletionAck)
		}},
		{name: "untracked claim", alter: func(_ *multiClientChannelUpdate, _ *tcpSynRetry, group *parsedPacketGroup) {
			group.completionFlags.And(^groupCompletionTracked)
		}},
		{name: "unadmitted claim", alter: func(_ *multiClientChannelUpdate, _ *tcpSynRetry, group *parsedPacketGroup) {
			group.completionFlags.And(^groupCompletionAdmitted)
		}},
		{name: "unlinked claim", alter: func(_ *multiClientChannelUpdate, _ *tcpSynRetry, group *parsedPacketGroup) {
			group.completionFlags.And(^groupCompletionLinked)
		}},
		{name: "disposed claim", alter: func(_ *multiClientChannelUpdate, _ *tcpSynRetry, group *parsedPacketGroup) {
			group.completionFlags.Or(groupCompletionDisposed)
		}},
		{name: "terminal claim", alter: func(_ *multiClientChannelUpdate, _ *tcpSynRetry, group *parsedPacketGroup) {
			group.completionFlags.Or(groupCompletionDone)
		}},
	} {
		ctx := context.Background()
		client := &multiClientChannel{ctx: ctx}
		update := &multiClientChannelUpdate{
			ctx: ctx, sequenceAdmissionEpoch: 7, synGenerationSeen: true, synGenerationNumber: 1321,
			race: &multiClientChannelUpdateRace{
				collapseOrder: order,
				clientStates:  map[*multiClientChannel]*multiClientChannelRaceClientState{client: {}},
			},
		}
		owner := &tcpSynRetry{update: update, serial: 17, sequence: 1321}
		update.synRetry = owner
		path := &IpPath{Protocol: IpProtocolTcp, Syn: true, SequenceNumber: owner.sequence}
		group := &parsedPacketGroup{
			packets: []parsedPacket{{ipPath: path}}, ipPath: path,
			collapseAdmission: tcpCollapseAdmission{update: update, epoch: update.sequenceAdmissionEpoch},
			completionClient:  client,
			collapseSource:    &SendSequence{ctx: ctx},
			synRetry:          &tcpSynRetryRequest{update: update, serial: owner.serial},
		}
		group.completionFlags.Store(raw | order<<16)
		update.sequenceClaims = group
		if test.alter != nil {
			test.alter(update, owner, group)
		}
		if !update.pendingCollapseCloseWithLock() {
			t.Fatalf("%s: detached case did not preserve the generic pending control fence", test.name)
		}
		if valid := owner.validWithLock(update); valid != test.valid {
			t.Fatalf("%s: retained owner validity=%t, want %t", test.name, valid, test.valid)
		}
	}
}
