// Discovery publication can finish before the source offer that requested it
// selects a provider. That real earlier success must retain collapse ownership.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

func TestDiscoveryDemandPublicationCanServeSameIpv4SourceSyn(t *testing.T) {
	testDiscoveryDemandPublicationBeforeSourceSelection(t, 4)
}

func TestDiscoveryDemandPublicationCanServeSameIpv6SourceSyn(t *testing.T) {
	testDiscoveryDemandPublicationBeforeSourceSelection(t, 6)
}

// The gate barrier orders real publication before the reasserting source's
// provider selection. It does not mint, select, send, or answer for that source.
func testDiscoveryDemandPublicationBeforeSourceSelection(t *testing.T, version int) {
	t.Helper()
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		path := reraceFlow(version, 81, 52880+version)
		specs := make([]reraceExitSpec, 4)
		for index := range specs {
			specs[index] = reraceExitSpec{name: string(rune('a' + index)), answer: func(candidate *IpPath) reraceAnswer {
				if candidate.DestinationIp.Equal(path.DestinationIp) {
					return reraceSignal
				}
				return reraceAccept
			}}
		}
		h, generator := newDiscoveryDemandTestHarness(t, specs)
		window, paths, owners := discoveryDemandTestCarryFour(t, h)
		publication := discoveryDemandTestObservePublication(window)
		synctest.Wait()
		window.stateLock.Lock()
		deadline := window.discoveryDemand.deadline
		liveSlot := window.discoveryDemand.owner != nil && window.discoveryDemand.slotOpen && time.Until(deadline) > 4*time.Second
		window.stateLock.Unlock()
		if !liveSlot || len(h.liveChannels()) != 4 || h.multiClient.backendDegraded() {
			t.Fatal("publication order control lacks the original vacant acquisition")
		}
		beforeReads, beforeEmpty, beforeMints, beforeRetired := generator.counts()
		start := time.Now()
		if !h.syn(path) {
			t.Fatal("original source SYN was not admitted")
		}
		update := h.update(path)
		update.stateLock.Lock()
		failed, inbound, bound := len(update.dialFailedClients), update.receivedInbound.Load(), update.client.Load()
		update.stateLock.Unlock()
		firstReads, firstEmpty, _, _ := generator.counts()
		if failed != 4 || inbound || bound != nil || h.metrics().DialFailuresIntercepted != 4 ||
			firstReads != beforeReads+1 || firstEmpty != beforeEmpty+1 {
			t.Fatal("publication order control lacks four refusals and the completed empty read")
		}
		time.Sleep(100 * time.Millisecond)
		h.addExit(reraceExitSpec{name: "late"})
		h.sleep(start.Add(reraceSynSchedule[1]).Sub(time.Now()))
		selectionVisits := 0
		h.multiClient.beforePacketGroupGateForTest = func(current *multiClientChannelUpdate, snapshot *multiClientChannel, group *parsedPacketGroup) {
			if group.isSynRetry() || !reraceSameFlow(group.ipPath, path) {
				return
			}
			selectionVisits++
			if current != update || snapshot != nil || !group.ipPath.Syn || group.ipPath.Ack {
				t.Fatal("publication barrier did not observe the unchanged unbound source SYN")
			}
			publicationBound := discoveryDemandTestEarlierTime(deadline, start.Add(reraceSynSchedule[2]))
			if !discoveryDemandTestAwaitPublication(t, publication, publicationBound) || !time.Now().Before(publicationBound) || len(h.liveChannels()) != 5 {
				t.Fatal("source gate did not observe real publication before its original next SYN")
			}
		}
		accepted := h.syn(path)
		h.multiClient.beforePacketGroupGateForTest = nil
		reads, empty, mints, retired := generator.counts()
		late := h.channel("late")
		if selectionVisits != 1 || !accepted || !time.Now().Before(start.Add(reraceSynSchedule[2])) ||
			h.boundTo(path) != late || len(h.exit("late").syns(path)) != 1 || h.metrics().DialFailuresIntercepted != 4 {
			t.Fatal("reasserting source SYN did not use its already published real provider")
		}
		if _, answered := h.synAck(path); !answered || !update.receivedInbound.Load() || h.update(path) != update {
			t.Fatal("same source offer did not establish the original connect attempt")
		}
		if reads != beforeReads+2 || empty != beforeEmpty+1 || mints != beforeMints+1 || retired != beforeRetired {
			t.Fatal("earlier source success exceeded its original discovery budget")
		}
		collapseDrops := h.multiClient.TcpCollapseDropCount()
		h.sleep(start.Add(reraceSynSchedule[2]).Sub(time.Now()))
		if h.syn(path) || h.multiClient.TcpCollapseDropCount() != collapseDrops+1 ||
			len(h.exit("late").syns(path)) != 1 || h.boundTo(path) != late {
			t.Fatal("established same-SYN duplicate lost the earlier source's recovery ownership")
		}
		reraceAssertNoErrorDelivered(t, h, path)
		payload := []byte("held original CSS sibling")
		for index, original := range paths {
			if h.boundTo(original) != owners[index] || owners[index].IsDone() || owners[index].isWarning() || owners[index].dialStarved() ||
				!h.send(reraceSegment(original, uint32(4*len(payload)), payload)) || len(h.delivered(original)) != 6 {
				t.Fatal("earlier discovery publication disturbed an original healthy sibling")
			}
		}
	})
}
