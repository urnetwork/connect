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

// Silence-inference roots isolate a destination that fails on an otherwise
// healthy exit. An unrelated successful flow supplies that positive health
// through real provider traffic before the tested connection's clock starts.
// The target flow's answer function, SYN schedule and inference bar are intact.
func newSilenceInferenceHarness(t *testing.T, specs []reraceExitSpec, configure func(*MultiClientSettings)) *reraceHarness {
	t.Helper()
	specs = append([]reraceExitSpec(nil), specs...)
	healthPath := reraceFlow(4, 1, 49111)
	found := false
	for index := range specs {
		if specs[index].name != "a" {
			continue
		}
		found = true
		answer := specs[index].answer
		specs[index].answer = func(path *IpPath) reraceAnswer {
			if reraceSameFlow(path, healthPath) || answer == nil {
				return reraceAccept
			}
			return answer(path)
		}
	}
	if !found {
		t.Fatal("silence-inference fixture requires its initial provider a")
	}
	harness := newReraceHarness(t, specs, configure)
	reraceHealthySibling(t, harness, 4)
	return harness
}

// Assert the prerequisite immediately before the retransmit under test: an
// unrelated unhealthy-affinity renewal must not replace its original clock.
func requireSilenceInferenceAffinity(t *testing.T, harness *reraceHarness, path *IpPath) {
	t.Helper()
	update, client := harness.update(path), harness.channel("a")
	if update == nil || update.client.Load() != client || update.receivedInbound.Load() ||
		!harness.multiClient.reuseAffinitySyn(update, client, path) {
		t.Fatal("silence-inference premise lacks an unanswered flow on reusable healthy affinity")
	}
}

// The 3s retransmit begins as a reuse of healthy A, then the silence inference
// releases A. Its same public offer must reach real new source queues, without
// carrying A's reuse claim into their final admission guard. A test-only send
// callback would bypass that guard and could not expose this failure.
func TestSilenceInferenceRetransmitAdmitsFreshCandidates(t *testing.T) {
	for _, twoCandidates := range []bool{false, true} {
		name := "one-answering-candidate"
		if twoCandidates {
			name = "two-silent-candidates"
		}
		t.Run(name, func(t *testing.T) {
			GetMessagePoolAggregateStats()
			synctest.Test(t, func(t *testing.T) {
				specs := []reraceExitSpec{
					{name: "a", tier: 0, answer: reraceAlways(reraceSilent)},
					{name: "b", tier: 1},
				}
				candidates := []string{"b"}
				if twoCandidates {
					specs[1].answer = reraceAlways(reraceSilent)
					specs = append(specs, reraceExitSpec{name: "c", tier: 1, answer: reraceAlways(reraceSilent)})
					candidates = append(candidates, "c")
				}
				harness := newSilenceInferenceHarness(t, specs, nil)
				path := reraceFlow(4, 0, 49112)
				a := harness.channel("a")
				start := time.Now()
				if !harness.syn(path) || harness.boundTo(path) != a {
					t.Fatal("initial public SYN did not bind its silent destination to healthy A")
				}
				update := harness.update(path)
				selection := update.client.loadSelection()
				update.stateLock.Lock()
				firstClient, firstClock, firstCount := update.synWaitClient, update.synWaitStart, update.synWaitSendCount
				update.stateLock.Unlock()
				if firstClient != a || firstClock.IsZero() || firstCount != 1 || len(harness.exit("a").syns(path)) != 1 {
					t.Fatal("first real SYN did not start A's silence clock exactly once")
				}

				harness.sleep(start.Add(reraceSynSchedule[1]).Sub(time.Now()))
				requireSilenceInferenceAffinity(t, harness, path)
				harness.syn(path)
				update.stateLock.Lock()
				waitClient, waitClock, waitCount := update.synWaitClient, update.synWaitStart, update.synWaitSendCount
				update.stateLock.Unlock()
				if harness.update(path) != update || update.client.loadSelection() != selection ||
					waitClient != a || !waitClock.Equal(firstClock) || waitCount != 2 || harness.metrics().FlowsReraced != 0 {
					t.Fatal("the 1s retransmit replaced the binding or reset the first SYN's silence clock")
				}

				var mutex sync.Mutex
				admissions := map[string]int{}
				wrongClaim := false
				for _, candidate := range candidates {
					client := harness.channel(candidate)
					if client.sendGroupForTest != nil {
						t.Fatal("fresh candidate has a test-only send callback")
					}
					settings := client.client.sendBuffer.sendBufferSettings
					previous := settings.afterGroupAdmissionForTest
					settings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
						if previous != nil {
							previous(target)
						}
						group, ok := target.(*parsedPacketGroup)
						if !ok || !reraceSameFlow(group.ipPath, path) || !group.ipPath.Syn || group.ipPath.Ack {
							return
						}
						mutex.Lock()
						defer mutex.Unlock()
						admissions[candidate]++
						wrongClaim = wrongClaim || group.affinitySynReuse || group.completionClient != client ||
							group.collapseAdmission.update != update || group.ipPath.SequenceNumber != path.SequenceNumber
					}
					t.Cleanup(func() { settings.afterGroupAdmissionForTest = previous })
				}
				harness.sleep(start.Add(reraceSynSchedule[2]).Sub(time.Now()))
				requireSilenceInferenceAffinity(t, harness, path)
				beforeA := len(harness.exit("a").syns(path))
				if !harness.syn(path) {
					t.Error("the 3s inference retransmit was refused by every real candidate")
				}
				if harness.update(path) != update || harness.metrics().FlowsReraced != 1 ||
					len(harness.exit("a").syns(path)) != beforeA {
					t.Error("the inference did not release A exactly once on the original flow")
				}
				mutex.Lock()
				for _, candidate := range candidates {
					if admissions[candidate] != 1 {
						t.Errorf("candidate %s had %d real source admissions at 3s, want one", candidate, admissions[candidate])
					}
				}
				if wrongClaim {
					t.Error("fresh source admission retained A's reuse claim or the wrong flow ownership")
				}
				mutex.Unlock()
				for _, candidate := range candidates {
					syns := harness.exit(candidate).syns(path)
					if len(syns) != 1 || syns[0].path.SequenceNumber != path.SequenceNumber ||
						syns[0].at.Sub(start) < reraceSynSchedule[2] || reraceSynSchedule[2]+reraceSettle < syns[0].at.Sub(start) {
						t.Errorf("candidate %s did not physically receive the same retransmit at 3s", candidate)
					}
				}
				if twoCandidates {
					update.stateLock.Lock()
					racing := update.client.Load() == nil && update.race != nil && len(update.race.clientStates) == 2 &&
						update.race.clientStates[harness.channel("b")] != nil && update.race.clientStates[harness.channel("c")] != nil
					update.stateLock.Unlock()
					if !racing || update.receivedInbound.Load() {
						t.Error("two unanswered candidates did not remain a real response race")
					}
				} else if _, ok := harness.synAck(path); !ok || harness.boundTo(path) != harness.channel("b") {
					t.Error("the answering fresh candidate did not complete the connection on the 3s retransmit")
				}
			})
		})
	}
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
