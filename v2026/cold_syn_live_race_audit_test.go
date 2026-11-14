// Exact native retransmissions must not acquire another reliable source owner
// for a live candidate that already accepted the same TCP SYN generation.
package connect

import (
	"bytes"
	"sync"
	"testing"
	"testing/synctest"
)

// The provider callback runs after real reliable Transfer delivery. Verify its
// local source identity as well as the tuple and SYN generation before counting.
func privateLiveRaceSynCount(t *testing.T, harness *reraceHarness, name string, path *IpPath, client *multiClientChannel) int {
	t.Helper()
	count := 0
	for _, packet := range harness.exit(name).syns(path) {
		if packet.path.SequenceNumber != path.SequenceNumber {
			continue
		}
		if packet.source != client.ClientId() {
			t.Fatalf("provider %s changed local source identity for pinned SYN: got=%s want=%s", name, packet.source, client.ClientId())
		}
		count++
	}
	return count
}

// No explicit failure, elapsed-time escape, selection, or source reset separates
// these actual native inputs. A different ISN remains an eligible new input.
func TestColdSynNativeUnansweredWideRaceDoesNotReadmitSameIsn(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ackedA, ackedB := make(chan struct{}), make(chan struct{})
		observeAck := func(acked chan struct{}) func(*ClientSettings) {
			var once sync.Once
			return func(settings *ClientSettings) {
				settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
					if event.MessageType.String() == "IpIpPacketToProvider" && event.Phase == SendPackLifecyclePhaseTerminal && event.AckRequired && event.Err == nil {
						once.Do(func() { close(acked) })
					}
				}
			}
		}
		harness := newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality-a", answer: reraceAlways(reraceSilent), clientSettings: observeAck(ackedA)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "quality-b", answer: reraceAlways(reraceSilent), clientSettings: observeAck(ackedB)}, windowType: WindowTypeQuality},
		}, func(settings *MultiClientSettings) { settings.MultiRaceClientCount = 2 })
		a := privateCrossWindowChannel(t, harness, "quality-a")
		b := privateCrossWindowChannel(t, harness, "quality-b")
		path := privateCrossWindowFlow(4, 77, 48477)
		if !privateCrossWindowNativeSyn(harness, path) {
			t.Fatal("first actual native SYN was not admitted")
		}
		update := harness.update(path)
		update.stateLock.Lock()
		race := update.race
		initial := race != nil && !race.single && len(race.clientStates) == 2 &&
			race.clientStates[a] != nil && race.clientStates[b] != nil &&
			race.pendingSendCount == 0 && update.client.Load() == nil &&
			len(update.dialFailedClients) == 0 && !update.receivedInbound.Load()
		update.stateLock.Unlock()
		if !initial || privateLiveRaceSynCount(t, harness, "quality-a", path, a) != 1 ||
			privateLiveRaceSynCount(t, harness, "quality-b", path, b) != 1 {
			t.Fatal("first source input did not form the exact unanswered reliable two-provider race")
		}
		admitted := privateCrossWindowNativeSyn(harness, path)
		countA := privateLiveRaceSynCount(t, harness, "quality-a", path, a)
		countB := privateLiveRaceSynCount(t, harness, "quality-b", path, b)
		update.stateLock.Lock()
		unchanged := update.race == race && len(race.clientStates) == 2 &&
			race.clientStates[a] != nil && race.clientStates[b] != nil &&
			race.pendingSendCount == 0 && update.client.Load() == nil &&
			len(update.dialFailedClients) == 0 && !update.receivedInbound.Load()
		update.stateLock.Unlock()
		if !unchanged {
			t.Fatal("duplicate control changed candidate identity, race ownership, or failure premise")
		}
		t.Logf("LIVE_RACE_DELIVERY_PROOF: initial_each=1 repeated_admitted=%t a=%d b=%d local_source_ids_unchanged=true race_unchanged=true", admitted, countA, countB)
		if admitted || countA != 1 || countB != 1 {
			t.Errorf("LIVE_RACE_DUPLICATE_PROVIDER_DELIVERY: case=unanswered repeated_admitted=%t a=%d b=%d want_admitted=false want_each=1", admitted, countA, countB)
		}
		// Wait for the actual reliable Transfer terminal acknowledgements, not
		// a sleep or inferred provider delivery. The first duplicate above had
		// no added clock wait; this variant remains redundant after peer ACK.
		<-ackedA
		<-ackedB
		harness.settle()
		afterAckAdmitted := privateCrossWindowNativeSyn(harness, path)
		afterAckA := privateLiveRaceSynCount(t, harness, "quality-a", path, a)
		afterAckB := privateLiveRaceSynCount(t, harness, "quality-b", path, b)
		t.Logf("LIVE_RACE_AFTER_PEER_ACK_PROOF: before_a=%d before_b=%d repeated_admitted=%t after_a=%d after_b=%d", countA, countB, afterAckAdmitted, afterAckA, afterAckB)
		if afterAckAdmitted || afterAckA != countA || afterAckB != countB {
			t.Errorf("LIVE_RACE_DUPLICATE_PROVIDER_DELIVERY: case=unanswered phase=after_peer_ack repeated_admitted=%t a_delta=%d b_delta=%d want_admitted=false want_deltas=0", afterAckAdmitted, afterAckA-countA, afterAckB-countB)
		}
		fresh := *path
		fresh.SequenceNumber++
		if !privateCrossWindowNativeSyn(harness, &fresh) ||
			privateLiveRaceSynCount(t, harness, "quality-a", &fresh, a) != 1 ||
			privateLiveRaceSynCount(t, harness, "quality-b", &fresh, b) != 1 {
			t.Error("distinct SYN generation was suppressed with the old generation")
		}
	})
}

// A standing explicit refusal may fill its vacated slot from the next quality
// tier. The remaining live owners still own their same-ISN reliable admissions.
func TestColdSynNativePartialFailureDoesNotReadmitLiveCandidates(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality-a", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "quality-b", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "quality-c", tier: 1, answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeSpeed},
		}, func(settings *MultiClientSettings) { settings.MultiRaceClientCount = 2 })
		a := privateCrossWindowChannel(t, harness, "quality-a")
		b := privateCrossWindowChannel(t, harness, "quality-b")
		c := privateCrossWindowChannel(t, harness, "quality-c")
		path := privateCrossWindowFlow(4, 78, 48478)
		if !privateCrossWindowNativeSyn(harness, path) ||
			privateLiveRaceSynCount(t, harness, "quality-a", path, a) != 1 ||
			privateLiveRaceSynCount(t, harness, "quality-b", path, b) != 1 ||
			privateLiveRaceSynCount(t, harness, "quality-c", path, c) != 0 {
			t.Fatal("initial SYN did not use exactly the preferred two quality candidates")
		}
		update := harness.update(path)
		update.stateLock.Lock()
		race := update.race
		update.stateLock.Unlock()
		harness.exit("quality-a").sendTo(a.ClientId(), reraceProviderSignal(t, path))
		harness.settle()
		privateCrossWindowAssertFailed(t, harness, path, "quality-a")
		update.stateLock.Lock()
		continued := race != nil && update.race == race && !race.single &&
			len(race.clientStates) == 2 && race.clientStates[a] == nil &&
			race.clientStates[b] != nil && race.clientStates[c] != nil &&
			race.pendingSendCount == 0 && update.client.Load() == nil
		update.stateLock.Unlock()
		if !continued || privateLiveRaceSynCount(t, harness, "quality-b", path, b) != 1 ||
			privateLiveRaceSynCount(t, harness, "quality-c", path, c) != 1 ||
			len(harness.exit("speed").syns(path)) != 0 {
			t.Fatal("explicit failure did not fill only the vacated slot from the next quality tier")
		}
		admitted := privateCrossWindowNativeSyn(harness, path)
		countB := privateLiveRaceSynCount(t, harness, "quality-b", path, b)
		countC := privateLiveRaceSynCount(t, harness, "quality-c", path, c)
		update.stateLock.Lock()
		unchanged := update.race == race && len(race.clientStates) == 2 &&
			race.clientStates[a] == nil && race.clientStates[b] != nil && race.clientStates[c] != nil &&
			race.pendingSendCount == 0 && update.client.Load() == nil && !update.receivedInbound.Load()
		update.stateLock.Unlock()
		if !unchanged || privateLiveRaceSynCount(t, harness, "quality-a", path, a) != 1 ||
			len(harness.exit("speed").syns(path)) != 0 || a.dialFailureCount() != 1 ||
			b.dialFailureCount() != 0 || c.dialFailureCount() != 0 {
			t.Fatal("native control changed slot ownership, quality priority, or exact failure standing")
		}
		t.Logf("LIVE_RACE_PARTIAL_FAILURE_DELIVERY_PROOF: initial_live_each=1 repeated_admitted=%t b=%d c=%d local_source_ids_unchanged=true race_unchanged=true", admitted, countB, countC)
		if admitted || countB != 1 || countC != 1 {
			t.Errorf("LIVE_RACE_DUPLICATE_PROVIDER_DELIVERY: case=partial_failure repeated_admitted=%t b=%d c=%d want_admitted=false want_each=1", admitted, countB, countC)
		}
	})
}

// Public native batches take all originals. Witnesses prove each source owner
// returned after actual provider delivery or a synchronous collapse refusal.
func privateLiveRaceNativeTemplates(t *testing.T, harness *reraceHarness, templates ...[]byte) (int, []bool) {
	t.Helper()
	packets := make([][]byte, len(templates))
	for index, template := range templates {
		packets[index] = MessagePoolCopy(template)
	}
	witnesses := groupTestPacketWitnesses(t, packets)
	defer requireGroupTestWitnessesReleased(t, packets, witnesses)
	accepted := make([]bool, len(packets))
	count := harness.multiClient.SendNativePacketBatchWithResults(harness.source, harness.multiClient.provideMode, packets, 0, accepted)
	harness.settle()
	return count, accepted
}

// A reader backlog may hand several identical retransmissions to one complete
// native group. Prior accepted candidate ownership covers every member.
func TestColdSynNativeCoveredBulkSynsKeepOriginalOwners(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality-a", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "quality-b", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
		}, func(settings *MultiClientSettings) { settings.MultiRaceClientCount = 2 })
		a := privateCrossWindowChannel(t, harness, "quality-a")
		b := privateCrossWindowChannel(t, harness, "quality-b")
		path := privateCrossWindowFlow(4, 79, 48479)
		if !privateCrossWindowNativeSyn(harness, path) {
			t.Fatal("initial native source was refused")
		}
		drops := harness.multiClient.TcpCollapseDropCount()
		template := reraceTcpPacket(path, tcpFlagSyn, path.SequenceNumber, 0, nil)
		count, accepted := privateLiveRaceNativeTemplates(t, harness, template, template)
		if count != 0 || accepted[0] || accepted[1] ||
			privateLiveRaceSynCount(t, harness, "quality-a", path, a) != 1 ||
			privateLiveRaceSynCount(t, harness, "quality-b", path, b) != 1 ||
			harness.multiClient.TcpCollapseDropCount() != drops+2 {
			t.Fatalf("covered native bulk group acquired new ownership: accepted=%d results=%v drops=%d", count, accepted, harness.multiClient.TcpCollapseDropCount()-drops)
		}
	})
}

// Complete-group admission preserves a fresh payload member even when another
// member is a covered SYN. Every accepted original returns its source owner.
func TestColdSynNativeMixedSynFreshDataKeepsWholeGroup(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality-a", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "quality-b", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
		}, func(settings *MultiClientSettings) { settings.MultiRaceClientCount = 2 })
		path := privateCrossWindowFlow(4, 80, 48480)
		if !privateCrossWindowNativeSyn(harness, path) {
			t.Fatal("initial native source was refused")
		}
		payload := []byte("fresh complete-group TCP bytes")
		syn := reraceTcpPacket(path, tcpFlagSyn, path.SequenceNumber, 0, nil)
		data := reraceTcpPacket(path, tcpFlagAck, path.SequenceNumber+1, 0, payload)
		drops := harness.multiClient.TcpCollapseDropCount()
		count, accepted := privateLiveRaceNativeTemplates(t, harness, syn, data)
		delivered := 0
		for _, name := range []string{"quality-a", "quality-b"} {
			packets := harness.exit(name).received(path)
			for index, packet := range packets {
				if !bytes.Equal(packet.payload, payload) {
					continue
				}
				if packet.source != privateCrossWindowChannel(t, harness, name).ClientId() || index == 0 ||
					!packets[index-1].path.Syn || packets[index-1].path.SequenceNumber != path.SequenceNumber ||
					packet.path.SequenceNumber != path.SequenceNumber+1 {
					t.Fatal("fresh TCP data lost its actual local source or complete ordered SYN prefix")
				}
				delivered++
			}
		}
		if count != 2 || !accepted[0] || !accepted[1] || delivered == 0 || harness.multiClient.TcpCollapseDropCount() != drops {
			t.Fatalf("mixed SYN/data group was partially collapsed: accepted=%d results=%v physical_payloads=%d", count, accepted, delivered)
		}
	})
}

// UDP carries independent datagrams even when every byte and the exact flow
// repeat while its providers are still unanswered.
func TestColdSynNativeIdenticalUdpDatagramsKeepBothAdmissions(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality-a", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "quality-b", answer: reraceAlways(reraceSilent)}, windowType: WindowTypeQuality},
		}, func(settings *MultiClientSettings) { settings.MultiRaceClientCount = 2 })
		path := privateCrossWindowFlow(4, 81, 48481)
		path.Protocol, path.Syn, path.SequenceNumber = IpProtocolUdp, false, 0
		payload := []byte("identical UDP datagram")
		template := ipOosUdpPacket(path, payload)
		for range 2 {
			count, accepted := privateLiveRaceNativeTemplates(t, harness, template)
			if count != 1 || !accepted[0] {
				t.Fatal("identical UDP input was collapsed")
			}
		}
		for _, name := range []string{"quality-a", "quality-b"} {
			packets := harness.exit(name).received(path)
			if len(packets) != 2 {
				t.Fatalf("provider %s delivered %d UDP datagrams, want2", name, len(packets))
			}
			for _, packet := range packets {
				if !bytes.Equal(packet.payload, payload) || packet.source != privateCrossWindowChannel(t, harness, name).ClientId() {
					t.Fatal("UDP datagram changed its bytes or local source identity")
				}
			}
		}
	})
}

// A real source can accept a SYN and then close before its first write.
// Only that candidate must be eligible on retry;
// another candidate's still-accepted raw owner must not be duplicated.
func TestColdSynUnwrittenRaceCandidateCanRetryExactSyn(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(0)
		var cancelOnce sync.Once
		var failedSource *SendSequence
		var admittedGroups []*parsedPacketGroup
		failed := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
				admittedGroups = append(admittedGroups, target.(*parsedPacketGroup))
			}
			settings.SendBufferSettings.beforeGroupDequeueForTest = func(source *SendSequence) {
				cancelOnce.Do(func() { failedSource = source; source.Cancel() })
			}
		})
		owned := newGroupDispositionQueueFixture(t, nil, nil)
		failed.update.client.Store(nil)
		failed.parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			return []*multiClientChannel{failed.selected, owned.selected}
		}
		syn := groupDispositionControlPacket(100, 0, 64, tcpFlagSyn)
		if count, accepted := failed.offerWithResults(syn); count != 1 || !accepted[0] {
			t.Fatal("initial exact SYN was not admitted by both source queues")
		}
		failed.requireParked()
		owned.requireParked()
		failed.unpark()
		synctest.Wait()
		if failedSource == nil {
			t.Fatal("pure SYN did not reach its actual source cancellation boundary")
		}
		select {
		case <-failedSource.done:
		default:
			t.Fatal("canceled source did not join its wholly-unwritten cleanup")
		}
		if len(failed.route) != 0 || failed.started.Load() != 1 || failed.terminal.Load() != 1 || budget.UsedByteCount() != 0 || failed.update.race == nil {
			t.Fatal("first candidate did not complete the actual wholly-unwritten source failure")
		}
		first := admittedGroups[0]
		if failed.client.IsDone() || failed.update.IsDone() || first.collapseOwner != nil || first.collapseSource != nil ||
			first.completionFlags.Load()&groupCompletionDequeued != 0 || owned.started.Load() != 1 || owned.terminal.Load() != 0 {
			t.Fatal("source cancellation changed the shared lifetime or another candidate's raw custody")
		}
		if count, accepted := failed.offerWithResults(syn); count != 1 || !accepted[0] {
			t.Fatal("live candidate could not acquire the exact pure-SYN retry on its next source generation")
		}
		synctest.Wait()
		if len(admittedGroups) != 2 || admittedGroups[1].collapseSource == nil ||
			admittedGroups[1].collapseSource == failedSource || budget.TotalByteCount() != 0 {
			t.Fatal("pure-SYN retry did not use a fresh source under the unchanged full DATA budget")
		}
		failed.acknowledgeAll(syn)
		owned.unpark()
		owned.acknowledgeAll(syn)
		if failed.started.Load() != 2 || owned.started.Load() != 1 || failed.offer(syn) != 0 {
			t.Fatal("retry duplicated the still-owned candidate or failed to install the recovered candidate's own gate")
		}
		failed.update.stateLock.Lock()
		failed.update.commitRaceClientWithLock(failed.selected)
		failed.update.stateLock.Unlock()
		failed.update.releaseCollapseClaims()
		if budget.UsedByteCount() != 0 || failed.update.sequenceClaims != nil {
			t.Fatal("recovered winner retained a discarded candidate or range owner")
		}
	})
}
