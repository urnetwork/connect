// Whole native lifecycles retain half-closed flows, retire admitted closes,
// and preserve established siblings during ordinary full-window renewal.
package connect

import (
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestAffinityRenewalPublicHalfCloseAndFinalAckLifetime(t *testing.T) {
	for _, version := range []int{4, 6} {
		assertMessagePoolOwnership(t)
		synctest.Test(t, func(t *testing.T) {
			f := newAffinitySynQueueFixture(t, version, nil)
			update := f.initial(t)
			f.receive(t, f.first.selected, tcpFlagSyn|tcpFlagAck, 9000, 1001)
			fin := reraceTcpPacket(f.path, tcpFlagFin|tcpFlagAck, 1001, 9001, nil)
			if f.first.offer(fin) != 1 {
				t.Fatal("outbound half-close was refused")
			}
			f.first.acknowledgeAll(fin)
			f.receive(t, f.first.selected, tcpFlagAck, 9001, 1002)
			if update.IsDone() {
				t.Fatal("one acknowledged FIN retired a legal half-close")
			}
			time.Sleep(5 * time.Minute)
			retired, _, _ := f.parent.detachIdleFlows(time.Now())
			if len(retired) != 0 || update.IsDone() {
				t.Fatal("TCP half-close did not retain its default ten-minute lifetime")
			}
			f.receive(t, f.first.selected, tcpFlagFin|tcpFlagAck, 9001, 1002)
			if update.IsDone() {
				t.Fatal("peer FIN retired before the final source acknowledgement")
			}
			last := reraceTcpPacket(f.path, tcpFlagAck, 1002, 9002, nil)
			if f.first.offer(last) != 1 {
				t.Fatal("final FIN acknowledgement was refused")
			}
			f.first.acknowledgeAll(last)
			if !update.IsDone() {
				t.Fatal("both acknowledged FIN edges failed to retire the admitted flow")
			}
			retired, _, _ = f.parent.detachIdleFlows(time.Now())
			if len(retired) != 1 || retired[0].update != update || f.parent.receiveUpdate(f.path) != nil {
				t.Fatal("complete FIN lifecycle did not remove exactly its canonical update")
			}
		})
	}
}

func TestAffinityRenewalPublicRefusedFinAndRstKeepOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		resume := func() { once.Do(func() { close(release) }) }
		var holdData atomic.Bool
		var parkedSource atomic.Pointer[SendSequence]
		var admittedData atomic.Pointer[parsedPacketGroup]
		f := newAffinitySynQueueFixture(t, 4, nil, func(settings *ClientSettings) {
			settings.SendBufferSettings.SequenceBufferSize = 0
			settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
				group := target.(*parsedPacketGroup)
				if group.ipPath.SequenceNumber == 1001 && group.ipPath.TcpPayloadByteCount == 4 {
					admittedData.Store(group)
				}
			}
			settings.SendBufferSettings.beforeGroupDequeueForTest = func(source *SendSequence) {
				if holdData.CompareAndSwap(true, false) {
					parkedSource.Store(source)
					close(entered)
					<-release
				}
			}
		})
		t.Cleanup(resume)
		// This lifecycle has one provider for every initial retry. The shared
		// renewal fixture otherwise widens on its second candidate enumeration.
		f.parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			f.races++
			return []*multiClientChannel{f.first.selected}
		}
		// Release the pre-Run gate; the initial zero-wait offer may still need
		// its ordinary retry before the unbuffered source starts receiving.
		f.first.unpark()
		initial := reraceTcpPacket(f.path, tcpFlagSyn, 1000, 0, nil)
		if f.first.offerTimeout(-1, initial) != 1 {
			t.Fatal("unbuffered initial SYN failed")
		}
		f.first.acknowledgeAll(initial)
		update := f.parent.receiveUpdate(f.path)
		if update == nil || update.client.Load() != f.first.selected || !update.synGenerationSeen ||
			f.second.started.Load() != 0 {
			t.Fatal("single-source initial admission did not commit its actual SYN owner")
		}
		f.receive(t, f.first.selected, tcpFlagSyn|tcpFlagAck, 9000, 1001)
		synctest.Wait()
		if !update.receivedInbound.Load() || update.client.Load() != f.first.selected ||
			f.parent.receiveUpdate(f.path) != update || update.IsDone() {
			t.Fatal("single-source response did not join its established canonical owner")
		}
		holdData.Store(true)
		data := reraceTcpPacket(f.path, tcpFlagAck, 1001, 9001, []byte("park"))
		if f.first.offer(data) != 1 {
			t.Fatal("callback-barrier data was not admitted")
		}
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("established data did not reach its actual source dequeue barrier")
		}
		group, source := admittedData.Load(), parkedSource.Load()
		if group == nil || source == nil || source.client != f.first.client || source.destination != f.first.peer ||
			group.ipPath.SequenceNumber != 1001 || group.ipPath.TcpPayloadByteCount != 4 ||
			len(group.packets) != 1 || group.completionClient != f.first.selected {
			t.Fatal("dequeue barrier did not identify the admitted data's exact source")
		}
		update.stateLock.Lock()
		exactOwner := group.collapseAdmission.update == update && group.collapseSource == source &&
			group.completionFlags.Load()&groupCompletionAdmitted != 0
		update.stateLock.Unlock()
		if !exactOwner || f.second.started.Load() != 0 {
			t.Fatal("parked data belongs to another generation or provider")
		}
		selection := update.client.loadSelection()
		for _, flags := range []byte{tcpFlagFin | tcpFlagAck, tcpFlagRst | tcpFlagAck} {
			packet := MessagePoolCopy(reraceTcpPacket(f.path, flags, 1005, 9001, nil))
			accepted := f.parent.SendPacket(f.first.source, affinityRenewalProvideModeNetwork, packet, 0)
			if !accepted {
				MessagePoolReturn(packet)
			}
			if accepted || update.IsDone() || update.client.loadSelection() != selection || update.egressFinSeen || f.parent.receiveUpdate(f.path) != update {
				t.Fatal("refused FIN/RST consumed caller ownership or retired the incumbent")
			}
		}
		resume()
		f.first.acknowledgeAll(data)
		rst := reraceTcpPacket(f.path, tcpFlagRst|tcpFlagAck, 1005, 9001, nil)
		if f.first.offer(rst) != 1 {
			t.Fatal("RST did not retry after source capacity returned")
		}
		f.first.acknowledgeAll(rst)
		if !update.IsDone() {
			t.Fatal("actually admitted RST did not retire its flow")
		}
	})
}

// Force the shared fixture's initial zero-wait refusal, then let the second
// source's real admission release the first source's retry. One public syn
// therefore has two source owners; enumeration count is not connection count.
func TestAffinityRenewalInitialRetryHasTwoActualSourceOwners(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var firstSyn, secondSyn atomic.Pointer[parsedPacketGroup]
		var f *affinitySynQueueFixture
		f = newAffinitySynQueueFixture(t, 4, func(settings *ClientSettings) {
			settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
				group := target.(*parsedPacketGroup)
				if group.ipPath.Syn && !group.ipPath.Ack {
					secondSyn.CompareAndSwap(nil, group)
					f.first.unpark()
				}
			}
		}, func(settings *ClientSettings) {
			settings.SendBufferSettings.SequenceBufferSize = 0
			settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
				group := target.(*parsedPacketGroup)
				if group.ipPath.Syn && !group.ipPath.Ack {
					firstSyn.CompareAndSwap(nil, group)
				}
			}
		})
		// Keep the first worker at its actual pre-Run barrier until the second
		// source owns its syn. No timer or scheduler choice releases that gate.
		initial := reraceTcpPacket(f.path, tcpFlagSyn, 1000, 0, nil)
		if f.first.offerTimeout(-1, initial) != 1 {
			t.Fatal("initial retry did not transfer its public packet ownership")
		}
		f.second.requireParked()
		update := f.parent.receiveUpdate(f.path)
		first, second := firstSyn.Load(), secondSyn.Load()
		if update == nil || first == nil || second == nil || first == second || f.races != 2 {
			t.Fatal("forced initial retry did not admit two distinct actual groups")
		}
		if f.first.started.Load() != 2 || f.first.terminal.Load() != 1 ||
			f.second.started.Load() != 1 || f.second.terminal.Load() != 0 {
			t.Fatal("forced first refusal and two pending admissions lack their actual source outcomes")
		}
		firstKey, firstValid := f.first.selected.affinitySourceKey()
		secondKey, secondValid := f.second.selected.affinitySourceKey()
		update.stateLock.Lock()
		exactSources := first.collapseAdmission.update == update && second.collapseAdmission.update == update &&
			first.collapseSource != nil && first.collapseSource.client == f.first.client &&
			second.collapseSource != nil && second.collapseSource.client == f.second.client &&
			first.ipPath.SequenceNumber == 1000 && second.ipPath.SequenceNumber == 1000 &&
			first.ipPath.TcpPayloadByteCount == 0 && second.ipPath.TcpPayloadByteCount == 0 &&
			first.completionFlags.Load()&groupCompletionAdmitted != 0 &&
			second.completionFlags.Load()&groupCompletionAdmitted != 0
		uncommitted := update.client.Load() == nil && !update.receivedInbound.Load() &&
			update.race != nil && !update.race.single && len(update.race.clientStates) == 2 &&
			update.race.sentPacketCount == 2
		update.stateLock.Unlock()
		if !firstValid || !secondValid || firstKey == secondKey || !exactSources || !uncommitted {
			t.Fatal("one public SYN did not retain its two-source, uncommitted race")
		}

		f.first.acknowledgeAll(initial)
		f.receive(t, f.first.selected, tcpFlagSyn|tcpFlagAck, 9000, 1001)
		synctest.Wait()
		if update.client.Load() != f.first.selected || !update.receivedInbound.Load() ||
			update.IsDone() || f.parent.receiveUpdate(f.path) != update {
			t.Fatal("actual response did not join the expected established race winner")
		}
		f.second.unpark()
		reset, ok := ipOosRst(f.path)
		if !ok {
			t.Fatal("fixture could not construct its exact loser reset")
		}
		f.second.acknowledgeAll(initial, reset)
		data := reraceTcpPacket(f.path, tcpFlagAck, 1001, 9001, []byte("joined"))
		if f.first.offer(data) != 1 {
			t.Fatal("joined established winner refused its ready data offer")
		}
		f.first.acknowledgeAll(data)
		if len(f.second.route) != 0 || update.client.Load() != f.first.selected || update.IsDone() {
			t.Fatal("post-commit data migrated back to the initial losing source")
		}
	})
}

// Supersedes the prototype's stronger, unrequested "all prior sources banned"
// expectation. The checkpoint source and its actual passing refusal remain
// preserved as evidence; success now means an ordinary race, not a new identity.
func TestAffinityRenewalPublicFullSixOccupiedRacesWithoutMigration(t *testing.T) {
	testAffinityOrdinaryRenewal(t, 4, 6, 0, time.Minute, time.Minute, 0, false, true, true)
}
