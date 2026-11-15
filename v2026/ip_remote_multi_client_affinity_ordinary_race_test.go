// Expiry starts an ordinary provider race; prior participation is not a ban.
// Public packet admission, actual source writes and response selection stay real.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// Every candidate is an actual pre-existing Client. Initial wide fanout is
// optional; established siblings make reclaiming any source an invalid shortcut.
func testAffinityOrdinaryRenewal(t *testing.T, version, count, winnerIndex int,
	maxTime, age, responseDelay time.Duration, sameIsn, initialWide, siblings bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixtures := make([]*groupDispositionQueueFixture, count)
		clients := make([]*multiClientChannel, count)
		sourceKeys := map[multiClientAffinitySourceKey]bool{}
		for index := range fixtures {
			fixtures[index] = newGroupDispositionQueueFixture(t, nil, nil)
			clients[index] = fixtures[index].selected
			clients[index].healthy = true
			clients[index].args.DestinationStats.IpFamily = IpFamilyDualstack
			key, valid := clients[index].affinitySourceKey()
			if !valid || sourceKeys[key] {
				t.Fatal("ordinary race fixture lacks distinct actual existing sources")
			}
			sourceKeys[key] = true
			fixtures[index].unpark()
		}
		parent := fixtures[0].parent
		parent.sendClientPathForTest = nil
		parent.settings.DestinationAffinity = false
		parent.ip4PathUpdates = map[Ip4Path]*multiClientChannelUpdate{}
		parent.ip6PathUpdates = map[Ip6Path]*multiClientChannelUpdate{}
		parent.affinityIp4Paths = map[Ip4Path]map[Ip4Path]time.Time{}
		parent.affinityIp6Paths = map[Ip6Path]map[Ip6Path]time.Time{}
		parent.flowUpdates = map[*multiClientChannelUpdate]bool{}
		settings := ReliabilitySettingsFrom(parent.settings)
		settings.AffinityMaxTime = maxTime
		parent.SetReliabilitySettings(settings)
		enumerations := 0
		offer := clients[:1]
		if initialWide {
			offer = clients
		}
		parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			enumerations++
			return offer
		}
		t.Cleanup(func() {
			for update := range parent.flowUpdates {
				update.Close()
			}
		})
		receive := func(client *multiClientChannel, path *IpPath, ack uint32) {
			packet := reraceTcpPacket(path.Reverse(), tcpFlagSyn|tcpFlagAck, 9000, ack, nil)
			ingress, err := ParseIpPath(packet)
			if err != nil {
				t.Fatal(err)
			}
			parent.clientReceivePacket(client, TransferPath{}, affinityRenewalProvideModeNetwork,
				TransportTypeUnknown, ingress, packet)
			synctest.Wait()
		}
		path := affinityTimeTestPath(version, 44100)
		path.SequenceNumber = 1000
		initial := reraceTcpPacket(path, tcpFlagSyn, 1000, 0, nil)
		if fixtures[0].offer(initial) != 1 {
			t.Fatal("ordinary race fixture failed its initial public SYN")
		}
		for index := range offer {
			fixtures[index].acknowledgeAll(initial)
		}
		old := parent.receiveUpdate(path)
		if old == nil {
			t.Fatal("initial public SYN has no canonical update")
		}
		if initialWide {
			old.stateLock.Lock()
			actualFanout := old.race != nil && old.race.sentPacketCount == count &&
				len(old.race.clientStates) == count && old.client.Load() == nil
			old.stateLock.Unlock()
			if !actualFanout {
				t.Fatal("initial full-window race did not admit every actual source")
			}
			receive(clients[0], path, 1001)
			if !old.receivedInbound.Load() || old.client.Load() != clients[0] {
				t.Fatal("initial actual response did not choose the intended incumbent")
			}
			reset, _ := ipOosRst(path)
			for index := 1; index < count; index++ {
				fixtures[index].acknowledgeAll(reset)
			}
		} else if old.client.Load() != clients[0] || old.receivedInbound.Load() {
			t.Fatal("single-provider initial retry is not the pending incumbent")
		}
		oldOrigin := old.client.loadSelection().createTime
		var siblingUpdates []*multiClientChannelUpdate
		if siblings {
			for index, client := range clients {
				offer = clients[index : index+1]
				siblingPath := affinityTimeTestPath(version, 44200+index)
				syn := reraceTcpPacket(siblingPath, tcpFlagSyn, siblingPath.SequenceNumber, 0, nil)
				if fixtures[0].offer(syn) != 1 {
					t.Fatal("existing source did not admit its public sibling SYN")
				}
				fixtures[index].acknowledgeAll(syn)
				receive(client, siblingPath, siblingPath.SequenceNumber+1)
				sibling := parent.receiveUpdate(siblingPath)
				if sibling == nil || !sibling.receivedInbound.Load() || sibling.client.Load() != client ||
					parent.clientFlowCount(client) == 0 {
					t.Fatal("renewal fixture did not hold an established sibling on every source")
				}
				siblingUpdates = append(siblingUpdates, sibling)
			}
		}
		time.Sleep(age)
		offer = clients
		beforeEnumerations := enumerations
		born := time.Now()
		sequence := uint32(2000)
		if sameIsn {
			sequence = 1000
		}
		syn := reraceTcpPacket(path, tcpFlagSyn, sequence, 0, nil)
		if fixtures[0].offer(syn) != 1 {
			t.Fatal("ordinary renewal refused its healthy existing candidate set")
		}
		current := parent.receiveUpdate(path)
		if current == nil || current == old || !old.IsDone() || enumerations <= beforeEnumerations ||
			!current.affinitySources.renewal || !current.client.originTime.Equal(born) {
			t.Fatal("expiry reused the old binding instead of admitting a fresh ordinary race")
		}
		// Buffered public admission does not join source dequeue. All sources
		// are unparked, so quiescence joins their actual admitted SYN evidence.
		synctest.Wait()
		if count > 1 {
			current.stateLock.Lock()
			actualFanout := current.client.Load() == nil && current.race != nil &&
				current.race.sentPacketCount == count && len(current.race.clientStates) == count
			for _, client := range clients {
				if current.race == nil {
					actualFanout = false
					break
				}
				state := current.race.clientStates[client]
				actualFanout = actualFanout && state != nil && state.sentPacketCount == 1 &&
					state.collapseControl.synSeen && state.collapseControl.synNumber == sequence
			}
			current.stateLock.Unlock()
			if !actualFanout {
				t.Fatal("renewal omitted a previously admitted healthy race candidate")
			}
		} else if current.client.Load() != clients[0] {
			t.Fatal("sole healthy incumbent did not win its ordinary single-candidate race")
		}
		for _, fixture := range fixtures {
			fixture.acknowledgeAll(syn)
		}
		if !sameIsn {
			before := current.client.Load()
			receive(clients[0], path, 1001)
			if current.receivedInbound.Load() || current.IsDone() || current.client.Load() != before {
				t.Fatal("prior-ISN answer changed the newly admitted handshake")
			}
		}
		time.Sleep(responseDelay)
		receive(clients[winnerIndex], path, sequence+1)
		selection := current.client.loadSelection()
		if !current.receivedInbound.Load() || selection == nil || selection.client != clients[winnerIndex] ||
			!selection.createTime.Equal(born) || !current.client.originTime.Equal(born) {
			t.Fatal("valid ordinary candidate response lost winner or immutable new creation age")
		}
		if age > 0 && !oldOrigin.Before(selection.createTime) {
			t.Fatal("actual fresh race inherited its predecessor's expired donor age")
		}
		reset, _ := ipOosRst(path)
		for index, fixture := range fixtures {
			if index != winnerIndex {
				fixture.acknowledgeAll(reset)
			}
		}
		for index, client := range clients {
			key, valid := client.affinitySourceKey()
			if !valid || !sourceKeys[key] || client.client.IsDone() ||
				siblings && (siblingUpdates[index].IsDone() || siblingUpdates[index].client.Load() != client) {
				t.Fatal("ordinary renewal minted a source, canceled a sibling, or changed an established owner")
			}
		}
		if len(sourceKeys) != count || len(parent.affinityTransitions) != 0 || len(parent.affinityDeliveries) != 0 {
			t.Fatal("ordinary race retained a transition or changed the existing source set")
		}
		retired, _, _ := parent.detachIdleFlows(time.Now())
		if len(retired) != 1 || retired[0].update != old || retired[0].shouldSignal {
			t.Fatal("renewed predecessor did not retire without resetting the live tuple")
		}
		parent.finishRetiredFlows(retired)
		if parent.receiveUpdate(path) != current || current.IsDone() || current.client.Load() != clients[winnerIndex] {
			t.Fatal("old update cleanup changed the ordinary race winner")
		}
	})
}

// The same TCP handshake may complete on the sole previous healthy provider.
func TestAffinityRenewalOrdinarySoleIncumbentSameIsn(t *testing.T) {
	for _, version := range []int{4, 6} {
		testAffinityOrdinaryRenewal(t, version, 1, 0, time.Minute, time.Minute, 0, true, false, true)
	}
}

// All six previously admitted sources remain usable while their siblings live.
func TestAffinityRenewalOrdinarySixOccupiedIncumbentWins(t *testing.T) {
	for _, version := range []int{4, 6} {
		testAffinityOrdinaryRenewal(t, version, 6, 0, time.Minute, time.Minute, 0, false, true, true)
	}
}

// A fresh ordinary race may instead choose another healthy responder.
func TestAffinityRenewalOrdinaryOtherCandidateWins(t *testing.T) {
	for _, version := range []int{4, 6} {
		testAffinityOrdinaryRenewal(t, version, 2, 1, time.Minute, time.Minute, 0, true, false, false)
	}
}

// Disabled affinity still permits the only provider to win a real new race.
func TestAffinityRenewalOrdinaryNonpositiveCap(t *testing.T) {
	for _, maxTime := range []time.Duration{0, -time.Nanosecond, -time.Minute} {
		testAffinityOrdinaryRenewal(t, 4, 1, 0, maxTime, 0, 0, true, false, false)
	}
}

// Setup delay counts against the new update's age, even with the old winner.
func TestAffinityRenewalOrdinaryWinnerCreationIncludesSetup(t *testing.T) {
	testAffinityOrdinaryRenewal(t, 4, 2, 0, time.Minute, time.Minute, 30*time.Second, true, false, false)
}
