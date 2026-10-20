// Whole native lifecycles retain half-closed flows, retire admitted closes,
// and explicitly expose the no-spare-identity limitation of a full window.
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
		var receives atomic.Int32
		f := newAffinitySynQueueFixture(t, 4, nil, func(settings *ClientSettings) {
			settings.SendBufferSettings.SequenceBufferSize = 0
			settings.SendBufferSettings.beforeGroupDequeueForTest = func(*SendSequence) {
				if receives.Add(1) == 2 {
					close(entered)
					<-release
				}
			}
		})
		t.Cleanup(resume)
		// The unbuffered initial source must be running before the first offer.
		f.first.unpark()
		initial := reraceTcpPacket(f.path, tcpFlagSyn, 1000, 0, nil)
		if f.first.offerTimeout(-1, initial) != 1 {
			t.Fatal("unbuffered initial SYN failed")
		}
		f.first.acknowledgeAll(initial)
		update := f.parent.receiveUpdate(f.path)
		f.receive(t, f.first.selected, tcpFlagSyn|tcpFlagAck, 9000, 1001)
		data := reraceTcpPacket(f.path, tcpFlagAck, 1001, 9001, []byte("park"))
		if f.first.offer(data) != 1 {
			t.Fatal("callback-barrier data was not admitted")
		}
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("source did not stop after accepting its second group")
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

// This is a required safe refusal, not successful renewal. The first public
// SYN really reaches all six source queues; each also carries an independently
// established sibling, so no capacity can be reclaimed without disruption.
func TestAffinityRenewalPublicFullSixOccupiedRefusesWithoutMigration(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixtures := make([]*groupDispositionQueueFixture, 6)
		clients := make([]*multiClientChannel, 6)
		for index := range fixtures {
			fixtures[index] = newGroupDispositionQueueFixture(t, nil, nil)
			clients[index] = fixtures[index].selected
			clients[index].healthy = true
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
		parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel { return clients }
		t.Cleanup(func() {
			for update := range parent.flowUpdates {
				update.Close()
			}
		})
		path := affinityTimeTestPath(4, 44100)
		path.SequenceNumber = 1000
		syn := reraceTcpPacket(path, tcpFlagSyn, 1000, 0, nil)
		if fixtures[0].offer(syn) != 1 {
			t.Fatal("initial full-window public SYN failed")
		}
		for _, fixture := range fixtures {
			fixture.acknowledgeAll(syn)
		}
		old := parent.receiveUpdate(path)
		if old == nil || old.client.Load() != nil {
			t.Fatal("initial six-source race did not wait for actual response evidence")
		}
		for _, client := range clients {
			packet := reraceSynAck(path)
			ingress, err := ParseIpPath(packet)
			if err != nil {
				t.Fatal(err)
			}
			parent.clientReceivePacket(client, TransferPath{}, affinityRenewalProvideModeNetwork, TransportTypeUnknown, ingress, packet)
		}
		synctest.Wait()
		winner := old.client.Load()
		if winner == nil || !old.receivedInbound.Load() {
			t.Fatal("real first-response race did not commit a provider")
		}
		reset, _ := ipOosRst(path)
		for index, client := range clients {
			if client != winner {
				fixtures[index].acknowledgeAll(reset)
			}
		}
		siblings := make([]*multiClientChannelUpdate, len(clients))
		for index, client := range clients {
			siblingPath := affinityTimeTestPath(4, 44200+index)
			parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel { return []*multiClientChannel{client} }
			siblingSyn := reraceTcpPacket(siblingPath, tcpFlagSyn, siblingPath.SequenceNumber, 0, nil)
			if fixtures[0].offer(siblingSyn) != 1 {
				t.Fatalf("sibling %d public SYN failed", index)
			}
			fixtures[index].acknowledgeAll(siblingSyn)
			packet := reraceSynAck(siblingPath)
			ingress, err := ParseIpPath(packet)
			if err != nil {
				t.Fatal(err)
			}
			parent.clientReceivePacket(client, TransferPath{}, affinityRenewalProvideModeNetwork, TransportTypeUnknown, ingress, packet)
			siblings[index] = parent.receiveUpdate(siblingPath)
			if siblings[index] == nil || !siblings[index].receivedInbound.Load() || parent.clientFlowCount(client) == 0 {
				t.Fatal("fixture does not hold an established sibling on every source")
			}
		}
		parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel { return clients }
		time.Sleep(time.Minute)
		selection, generation := old.client.loadSelection(), old.synGenerationNumber
		packet := MessagePoolCopy(reraceTcpPacket(path, tcpFlagSyn, 2000, 0, nil))
		accepted := parent.SendPacket(fixtures[0].source, affinityRenewalProvideModeNetwork, packet, 0)
		if !accepted {
			MessagePoolReturn(packet)
		}
		if accepted || parent.receiveUpdate(path) != old || old.IsDone() || old.client.loadSelection() != selection || old.synGenerationNumber != generation ||
			len(parent.affinityTransitions) != 0 || len(parent.flowUpdates) != 7 {
			t.Fatal("full occupied window consumed the new SYN or mutated its preserved incumbent")
		}
		for index, client := range clients {
			if client.client.IsDone() || siblings[index].IsDone() || siblings[index].client.Load() != client {
				t.Fatalf("renewal reclaimed occupied source %d or migrated its established sibling", index)
			}
		}
	})
}
