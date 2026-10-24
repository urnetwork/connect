package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// Actual source IPv6 SYN/refusal/late-SYNACK path; four established IPv4
// siblings remain healthy while a V4-only late decoy cannot satisfy demand.
func TestDiscoveryDemandIpv6FindsProviderAfterEmptyRead(t *testing.T) {
	testDiscoveryDemandIpv6LateVisibility(t, false)
}

func TestDiscoveryDemandIpv6ReassertsDuringInflightEmptyRead(t *testing.T) {
	testDiscoveryDemandIpv6LateVisibility(t, true)
}

func testDiscoveryDemandIpv6LateVisibility(t *testing.T, holdEmpty bool) {
	t.Helper()
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		failedPath := reraceFlow(6, 80, 52860)
		specs := make([]reraceExitSpec, 4)
		for index := range specs {
			specs[index] = reraceExitSpec{name: string(rune('a' + index)), answer: func(path *IpPath) reraceAnswer {
				if path.DestinationIp.Equal(failedPath.DestinationIp) {
					return reraceSignal
				}
				return reraceAccept
			}}
		}
		h, generator := newDiscoveryDemandTestHarness(t, specs)
		window, paths, owners := discoveryDemandTestCarryFour(t, h)
		publication := discoveryDemandTestObservePublication(window)
		payload := []byte("held original CSS sibling")
		synctest.Wait()
		window.stateLock.Lock()
		premise := window.discoveryDemand.owner != nil && window.discoveryDemand.slotOpen && time.Until(window.discoveryDemand.deadline) > 4*time.Second
		deadline := window.discoveryDemand.deadline
		window.stateLock.Unlock()
		if !premise || len(h.liveChannels()) != 4 || h.multiClient.backendDegraded() {
			t.Fatal("control premise lacks the ordinary vacant live acquisition")
		}
		readsBefore, emptyBefore, mintsBefore, retiredBefore := generator.counts()
		generator.mutex.Lock()
		generator.holdNextEmpty = holdEmpty
		generator.mutex.Unlock()
		start := time.Now()
		if !h.syn(failedPath) {
			t.Fatal("original source SYN was not admitted")
		}
		update := h.update(failedPath)
		update.stateLock.Lock()
		failed, inbound, bound := len(update.dialFailedClients), update.receivedInbound.Load(), update.client.Load()
		update.stateLock.Unlock()
		readsFirst, emptyFirst, _, _ := generator.counts()
		if failed != 4 || inbound || bound != nil || len(h.liveChannels()) != 4 || h.metrics().DialFailuresIntercepted != 4 {
			t.Fatal("control premise lacks the actual same-SYN four-provider refusal")
		}
		if readsFirst != readsBefore+1 || emptyFirst != emptyBefore+1 {
			t.Fatalf("control premise: first exhausted demand was not a real empty API read: before=%d/%d after=%d/%d", readsBefore, emptyBefore, readsFirst, emptyFirst)
		}
		if holdEmpty {
			select {
			case <-generator.emptyEntered:
			default:
				t.Fatal("in-flight empty read did not reach its owned barrier")
			}
		}
		// Strictly after the empty snapshot, construction changes availability
		// only. The next real source retransmit must reassert discovery demand.
		time.Sleep(100 * time.Millisecond)
		h.addExit(reraceExitSpec{name: "v4-only-decoy", family: IpFamilyV4Only})
		h.addExit(reraceExitSpec{name: "late", family: IpFamilyV6Only})
		h.sleep(start.Add(reraceSynSchedule[1]).Sub(time.Now()))
		if !h.syn(failedPath) {
			t.Fatal("same-SYN retransmit was refused at source")
		}
		if holdEmpty {
			close(generator.emptyRelease)
			synctest.Wait()
		}
		publicationBound := discoveryDemandTestEarlierTime(deadline, start.Add(reraceSynSchedule[2]))
		publicationReady := discoveryDemandTestAwaitPublication(t, publication, publicationBound)
		reads, empty, mints, retired := generator.counts()
		owned := len(h.liveChannels())
		t.Logf("late-visibility before_deadline=%t elapsed=%s owned=%d failed_first=%d explicit_signals=%d reads=%d->%d empty=%d->%d mints=%d->%d retired=%d->%d held_empty=%t", time.Now().Before(deadline), time.Since(start), owned, failed, h.metrics().DialFailuresIntercepted, readsBefore, reads, emptyBefore, empty, mintsBefore, mints, retiredBefore, retired, holdEmpty)
		if owned != 5 || !publicationReady || !time.Now().Before(publicationBound) {
			t.Fatalf("DISCOVERY_DEMAND_LATE_VISIBLE_NOT_RELISTED: owned=%d with an available fifth provider before the unchanged next SYN; publication_event=%t", owned, publicationReady)
		}
		for _, candidate := range window.unorderedClients() {
			if candidate.Destination() == h.exit("v4-only-decoy").destination {
				t.Fatal("IPv6 demand consumed its sole slot/mint on the available V4-only decoy")
			}
		}
		if h.channel("late").IpFamily() != IpFamilyV6Only {
			t.Fatal("public late channel lost its IPv6-only family evidence")
		}
		if !time.Now().Before(deadline) || reads != readsBefore+2 || mints != mintsBefore+1 || retired != retiredBefore {
			t.Fatal("late admission exceeded its original read/mint/custody budget")
		}
		h.sleep(start.Add(reraceSynSchedule[2]).Sub(time.Now()))
		if !h.syn(failedPath) {
			t.Fatal("original next SYN was not admitted")
		}
		if _, ok := h.synAck(failedPath); !ok || h.boundTo(failedPath) != h.channel("late") {
			t.Fatal("newly available exit did not answer the unchanged connect attempt")
		}
		for index, path := range paths {
			if h.boundTo(path) != owners[index] || owners[index].IsDone() || owners[index].isWarning() || owners[index].dialStarved() ||
				!h.send(reraceSegment(path, uint32(4*len(payload)), payload)) || len(h.delivered(path)) != 6 {
				t.Fatal("level demand disturbed an original healthy sibling")
			}
		}
	})
}
