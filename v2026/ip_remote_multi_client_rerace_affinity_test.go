// Dial-failure forwarding and affinity eligibility are separate decisions.
// These controls keep real source admission, provider traffic and public return.
package connect

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// A successful unrelated connection and data echo supply positive health.
// Four real traffic buckets leave two closed buckets with positive duration;
// the latest two are deliberately excluded by WindowStats.
func reraceHealthySibling(t *testing.T, harness *reraceHarness, version int) *IpPath {
	t.Helper()
	path := reraceFlow(version, 1, 49111)
	a := harness.channel("a")
	if !harness.syn(path) {
		t.Fatal("healthy sibling SYN was not admitted")
	}
	if _, ok := harness.synAck(path); !ok || harness.boundTo(path) != a {
		t.Fatal("healthy sibling did not establish on A")
	}
	payload := []byte("health")
	for index := range 4 {
		if index != 0 {
			// This advances the stats contract's clock, not a completion
			// wait. Each subsequent actual send creates the next bucket.
			time.Sleep(a.settings.StatsWindowBucketDuration + time.Nanosecond)
		}
		if !harness.send(reraceSegment(path, uint32(index*len(payload)), payload)) {
			t.Fatalf("healthy sibling data packet %d was not admitted", index)
		}
		if got := len(harness.delivered(path)); got != index+2 {
			t.Fatalf("healthy sibling data packet %d delivered %d packets, want %d", index, got, index+2)
		}
	}
	if got := len(harness.exit("a").received(path)); got != 5 {
		t.Fatalf("healthy sibling provider received %d packets, want one SYN and four data packets", got)
	}
	stats, err := a.WindowStats()
	sendBytes, receiveBytes := stats.EffectiveByteCount()
	a.stateLock.Lock()
	buckets, initialized, liveHealthy := len(a.eventBuckets), !a.firstEventTime.IsZero(), a.healthy
	a.stateLock.Unlock()
	if err != nil || !stats.healthy || !liveHealthy || !initialized || stats.bucketCount < 2 ||
		stats.windowDuration <= 0 || sendBytes <= 0 || receiveBytes <= 0 ||
		stats.receiveAckCount != 5 || stats.receiveSynCount != 1 {
		t.Fatalf("real sibling health: err=%v snapshot=%t live=%t initialized=%t buckets=%d closed=%d duration=%s send=%d/%dB receive=%d/%dB syn=%d effective=%d/%dB",
			err, stats.healthy, liveHealthy, initialized, buckets, stats.bucketCount, stats.windowDuration,
			stats.sendAckCount, stats.sendAckByteCount, stats.receiveAckCount, stats.receiveAckByteCount,
			stats.receiveSynCount, sendBytes, receiveBytes)
	}
	return path
}

// knownFailure excludes A through the existing destination-service filter;
// without that evidence A remains an ordinary race candidate, not an affinity
// donor. The healthy control instead reuses its young canonical update.
func testReraceOffAdmittedAttempt(t *testing.T, version int, newIsn, healthy, knownFailure bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		path := reraceFlow(version, 0, 47110)
		var nextAttempt atomic.Bool
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: func(candidate *IpPath) reraceAnswer {
				if !reraceSameFlow(candidate, path) {
					return reraceAccept
				}
				if knownFailure || nextAttempt.Load() {
					return reraceSignal
				}
				return reraceSilent
			}},
			{name: "b", tier: 1, answer: reraceAlways(reraceSignal)},
		}, func(settings *MultiClientSettings) {
			settings.DialFailureRerace = false
		})
		a, b := harness.channel("a"), harness.channel("b")
		var sibling *IpPath
		var siblingOwner *multiClientChannelUpdate
		if healthy {
			sibling = reraceHealthySibling(t, harness, version)
			siblingOwner = harness.update(sibling)
		}
		if !harness.syn(path) || harness.boundTo(path) != a {
			t.Fatal("initial public SYN did not select A")
		}
		old := harness.update(path)
		if old == nil || old.receivedInbound.Load() {
			t.Fatal("initial tested connection is not an unanswered SYN")
		}
		stats, err := a.WindowStats()
		if err != nil || stats.healthy != healthy {
			t.Fatalf("provider health: err=%v healthy=%t want=%t closed=%d duration=%s send=%d/%dB receive=%d/%dB",
				err, stats.healthy, healthy, stats.bucketCount, stats.windowDuration,
				stats.sendAckCount, stats.sendAckByteCount, stats.receiveAckCount, stats.receiveAckByteCount)
		}
		next := *path
		if newIsn {
			next.SequenceNumber += 100000
		}
		if harness.multiClient.reuseAffinitySyn(old, a, &next) != healthy {
			t.Fatal("health-gated affinity reuse did not match the actual provider evidence")
		}
		if harness.multiClient.destinationServiceFailurePresent([]*multiClientChannel{a}, path) != knownFailure {
			t.Fatal("known-failure premise did not come from the actual provider signal")
		}
		want, wantName := a, "a"
		if knownFailure && !healthy {
			want, wantName = b, "b"
		}
		beforeErrors := len(harness.errorsDelivered(path))
		beforeMetrics := harness.metrics()
		wantInitialErrors := 0
		if knownFailure {
			wantInitialErrors = 1
		}
		if beforeErrors != int(beforeMetrics.DialFailuresIntercepted) ||
			beforeErrors != wantInitialErrors || beforeMetrics.FlowsReraced != 0 {
			t.Fatal("initial knob-off signal lost its immediate forwarding outcome")
		}

		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		resume := func() { releaseOnce.Do(func() { close(release) }) }
		var admitted atomic.Pointer[parsedPacketGroup]
		synctest.Wait()
		sendBuffer := want.client.sendBuffer
		settings := sendBuffer.sendBufferSettings
		previous := settings.afterGroupAdmissionForTest
		previousAck := sendBuffer.afterAckSendItemForTest
		acknowledged := make(chan struct{})
		var acknowledgeOnce sync.Once
		sendBuffer.afterAckSendItemForTest = func(id sendSequenceId, number uint64) {
			if previousAck != nil {
				previousAck(id, number)
			}
			group := admitted.Load()
			if group == nil {
				return
			}
			update := group.collapseAdmission.update
			update.stateLock.Lock()
			matches := group.collapseSource != nil && group.collapseSource.client == want.client &&
				group.collapseSource.id() == id && group.completionFlags.Load()&groupCompletionTerminal != 0
			update.stateLock.Unlock()
			if matches {
				acknowledgeOnce.Do(func() { close(acknowledged) })
			}
		}
		settings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
			if previous != nil {
				previous(target)
			}
			group, ok := target.(*parsedPacketGroup)
			if !ok || group.completionClient != want || !reraceSameFlow(group.ipPath, &next) ||
				!group.ipPath.Syn || group.ipPath.Ack || group.ipPath.SequenceNumber != next.SequenceNumber {
				return
			}
			if !admitted.CompareAndSwap(nil, group) {
				t.Error("one public attempt acquired more than one producer barrier")
				return
			}
			close(entered)
			select {
			case <-release:
			case <-harness.ctx.Done():
			}
		}
		results := make(chan bool, 1)
		joined, accepted := false, false
		join := func() {
			resume()
			if !joined {
				accepted = <-results
				joined = true
			}
			settings.afterGroupAdmissionForTest = previous
			synctest.Wait()
			sendBuffer.afterAckSendItemForTest = previousAck
		}
		t.Cleanup(join)
		nextAttempt.Store(true)
		go func() {
			packet := MessagePoolCopy(reraceTcpPacket(&next, tcpFlagSyn, next.SequenceNumber, 0, nil))
			sent := harness.multiClient.SendPacket(harness.source, affinityRenewalProvideModeNetwork, packet, reraceSendTimeout)
			if !sent {
				MessagePoolReturn(packet)
			}
			results <- sent
		}()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Errorf("ipv%d newISN=%t healthy=%t knownFailure=%t: eligible ordinary candidate never reached real source admission",
				version, newIsn, healthy, knownFailure)
			return
		}
		select {
		case accepted = <-results:
			joined = true
			t.Fatal("public producer returned before its actual admission barrier was released")
		default:
		}
		current, group := harness.update(path), admitted.Load()
		if current == nil || group == nil || group.collapseAdmission.update != current ||
			(current == old) != healthy || old.IsDone() == healthy {
			t.Fatal("real admission did not publish the expected canonical owner before public return")
		}
		if !healthy && (!current.affinitySources.renewal || old.affinityDelivery.Load() != -1) {
			t.Fatal("renewal did not retain its exact old-delivery fence")
		}
		// A same-source second SYN may await the receiver's compression
		// timer. Quiescence alone does not mean that peer ACK has arrived.
		if group.completionFlags.Load()&groupCompletionTerminal == 0 {
			select {
			case <-acknowledged:
			case <-time.After(reraceSendTimeout):
				current.stateLock.Lock()
				flags, sourceState := group.completionFlags.Load(), group.affinitySourceState
				current.stateLock.Unlock()
				t.Fatalf("ipv%d newISN=%t healthy=%t knownFailure=%t: peer ACK did not complete while producer held: flags=%#x source=%#x compression=%s",
					version, newIsn, healthy, knownFailure, flags, sourceState,
					harness.exit(wantName).provider.settings.ReceiveBufferSettings.AckCompressTimeout)
			}
		}
		synctest.Wait()
		current.stateLock.Lock()
		flags := group.completionFlags.Load()
		source := group.collapseSource
		sourceState, registration := group.affinitySourceState, group.affinitySourceRegistration
		pending := current.affinitySources.first.pending
		exactOwner := group.affinitySourceRegistration != 0 && source != nil && source.client == want.client &&
			sourceState&affinitySourceMaterialized != 0 && sourceState&affinitySourceFailed == 0
		selectionReady := current.client.Load() == want
		selected, racePresent, single, candidates, pendingSends, sentPackets := selectionReady, false, false, 0, 0, 0
		if !healthy {
			// Admission publishes the generation, not the ordinary race's
			// winner. This one-candidate commitment follows public send return.
			race := current.race
			selectionReady = current.client.Load() == nil && race != nil && race.single &&
				len(race.clientStates) == 1 && race.clientStates[want] != nil &&
				race.clientStates[want].pendingSendCount == 1 && race.sentPacketCount == 0
			if race != nil {
				racePresent, single, candidates, sentPackets = true, race.single, len(race.clientStates), race.sentPacketCount
				if state := race.clientStates[want]; state != nil {
					pendingSends = state.pendingSendCount
				}
			}
		}
		current.stateLock.Unlock()
		if !exactOwner {
			t.Fatalf("held source owner: registration=%d source=%t materialized=%t failed=%t flags=%#x state=%#x",
				registration, source != nil, sourceState&affinitySourceMaterialized != 0,
				sourceState&affinitySourceFailed != 0, flags, sourceState)
		}
		if !selectionReady {
			t.Fatalf("held selection: healthy=%t selected=%t race=%t single=%t candidates=%d pendingSends=%d sentPackets=%d",
				healthy, selected, racePresent, single, candidates, pendingSends, sentPackets)
		}
		if flags&(groupCompletionTracked|groupCompletionAdmitted|groupCompletionDequeued|groupCompletionTerminal) !=
			groupCompletionTracked|groupCompletionAdmitted|groupCompletionDequeued|groupCompletionTerminal ||
			flags&groupCompletionReturned != 0 || pending != 1 {
			t.Fatalf("held completion: flags=%#x tracked=%t admitted=%t dequeued=%t terminal=%t returned=%t pending=%d source=%#x",
				flags, flags&groupCompletionTracked != 0, flags&groupCompletionAdmitted != 0,
				flags&groupCompletionDequeued != 0, flags&groupCompletionTerminal != 0,
				flags&groupCompletionReturned != 0, pending, sourceState)
		}
		errorsDelivered := harness.errorsDelivered(path)
		if len(errorsDelivered) != beforeErrors+1 {
			t.Fatalf("real provider delivered %d errors, want exactly %d before public return", len(errorsDelivered), beforeErrors+1)
		}
		delivered := errorsDelivered[len(errorsDelivered)-1]
		embedded, ok := ipParseIcmpUnreachable(delivered.packet)
		wantPath := reraceProviderFlowPath(&next)
		wantPath.SequenceNumber = next.SequenceNumber
		wantPacket, valid := ipOosUnreachable(wantPath)
		if !ok || !valid || embedded.SequenceNumber != next.SequenceNumber || !bytes.Equal(delivered.packet, wantPacket) {
			t.Fatal("actual admitted provider error did not embed the current SYN sequence")
		}
		attempts := harness.exit(wantName).syns(path)
		if len(attempts) == 0 || attempts[len(attempts)-1].path.SequenceNumber != next.SequenceNumber ||
			!delivered.at.Equal(attempts[len(attempts)-1].at) {
			t.Fatal("forwarded error lacks its immediate actual provider SYN")
		}
		if metrics := harness.metrics(); metrics.FlowsReraced != 0 ||
			metrics.DialFailuresIntercepted != beforeMetrics.DialFailuresIntercepted+1 {
			t.Fatal("affinity renewal changed the knob-off dial-failure accounting")
		}
		wantBStrikes := 0
		if want == b {
			wantBStrikes = 1
		}
		if a.dialFailureCount() != 1 || b.dialFailureCount() != wantBStrikes || current.receivedInbound.Load() {
			t.Fatal("a forwarded dial failure became data health or lost its exact provider strike")
		}
		join()
		synctest.Wait()
		if !accepted {
			t.Fatal("actual admitted attempt did not return public success")
		}
		current.stateLock.Lock()
		settled := group.affinitySourceState&affinitySourceSettled != 0
		pending = current.affinitySources.first.pending
		ambiguous := current.affinitySources.first.ambiguous
		current.stateLock.Unlock()
		if !settled || pending != 0 || ambiguous || current.client.Load() != want ||
			len(harness.multiClient.affinityTransitions) != 0 ||
			len(harness.multiClient.affinityDeliveries) != 0 || a.IsDone() || b.IsDone() {
			t.Fatal("joined public return retained custody or tore down an unrelated provider")
		}
		if healthy {
			if harness.update(sibling) != siblingOwner || siblingOwner.IsDone() || siblingOwner.client.Load() != a ||
				!harness.send(reraceSegment(sibling, 24, []byte("still live"))) || len(harness.delivered(sibling)) != 6 {
				t.Fatal("tested attempt disturbed its healthy established sibling")
			}
		}
	})
}
