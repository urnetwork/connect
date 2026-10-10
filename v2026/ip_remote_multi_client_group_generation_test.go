// Source-generation close controls use real public admission, source workers,
// wire decoding and peer acknowledgements. No test fabricates materialization.
package connect

import (
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"
)

// A SYN already in the channel can precede its producer's admission observer.
// Closing the earlier RST must wait for this exact source-owned transition.
func TestTcpGroupRecoveryResetSynPostEnqueueObserverGap(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var offers atomic.Int64
		var releaseOnce sync.Once
		unpark := func() { releaseOnce.Do(func() { close(release) }) }
		budget := NewTransferMemoryBudget(kib(64))
		f := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			settings.SendBufferSettings.beforeGroupAdmissionForTest = func() {
				if offers.Add(1) == 2 {
					close(entered)
					<-release
				}
			}
		})
		t.Cleanup(unpark)
		f.update.updateSequence(groupRecoveryParsed(t, groupDispositionControlPacket(100, 500, 64, tcpFlagSyn)))
		rst := groupDispositionControlPacket(101, 700, 64, tcpFlagRst|tcpFlagAck)
		syn := groupDispositionControlPacket(900, 800, 64, tcpFlagSyn)
		if f.offer(rst) != 1 {
			t.Fatal("reset admission refused")
		}
		result := make(chan int, 1)
		go func() { result <- f.offer(syn) }()
		f.requireParked()
		select {
		case <-entered:
		default:
			t.Fatal("SYN producer did not pause after irreversible enqueue")
		}
		if f.update.sequenceSynOffers != 1 || f.update.sequenceClaims == nil || f.update.sequenceClaims.collapseNext != nil {
			t.Fatal("post-enqueue gap registered packet ownership instead of only a scalar offer")
		}
		f.unpark()
		f.acknowledgeAll(rst, syn)
		if f.update.IsDone() || f.update.synGenerationNumber != 900 || f.update.sourceRstSequence() != 800 ||
			f.update.sequenceSynOffers != 1 || f.terminal.Load() != 2 || budget.UsedByteCount() == 0 {
			t.Fatal("earlier reset canceled the queued SYN or released its held public-return lifetime")
		}
		if f.offer(syn) != 0 {
			t.Fatal("source-owned SYN did not gate its identical retransmission while its producer was paused")
		}
		unpark()
		if <-result != 1 {
			t.Fatal("queued SYN lost its successful public result")
		}
		if f.update.sequenceSynOffers != 0 || f.update.sequenceClaims != nil || budget.UsedByteCount() != 0 {
			t.Fatal("finished SYN offer retained its scalar, range charge or descriptor link")
		}
	})
}

// A failed offer cannot hold a reset open or install a new generation. The
// one-slot real channel makes the refusal independent of scheduling luck.
func TestTcpGroupRecoveryRefusedSynDoesNotHoldResetClose(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(kib(64))
		f := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			settings.SendBufferSettings.SequenceBufferSize = 1
		})
		f.update.updateSequence(groupRecoveryParsed(t, groupDispositionControlPacket(100, 500, 64, tcpFlagSyn)))
		rst := groupDispositionControlPacket(101, 700, 64, tcpFlagRst|tcpFlagAck)
		syn := groupDispositionControlPacket(900, 800, 64, tcpFlagSyn)
		if f.offer(rst) != 1 {
			t.Fatal("reset did not fill the actual one-slot queue")
		}
		f.requireParked()
		if f.offer(syn) != 0 {
			t.Fatal("new SYN bypassed the occupied source queue")
		}
		if f.update.sequenceSynOffers != 0 || f.update.synAdmissions != nil || f.update.synGenerationNumber != 100 ||
			f.update.sourceRstSequence() != 700 || f.update.sequenceClaims == nil || f.update.sequenceClaims.collapseNext != nil ||
			budget.UsedByteCount() != 0 {
			t.Fatal("refused new SYN changed generation, successful control or pending ownership")
		}
		f.unpark()
		f.acknowledgeAll(rst)
		if !f.update.IsDone() || f.update.sequenceClaims != nil || f.update.sequenceSynOffers != 0 || budget.UsedByteCount() != 0 {
			t.Fatal("refused SYN left the genuine source reset or its owners pending")
		}
	})
}

// Refusing a new cohort leaves it eligible for an actual later retry. Only
// successful source admission may replace the old identical-SYN gate.
func TestTcpGroupRecoveryRefusedSynRetriesAsNewCohort(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(kib(64))
		f := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			settings.SendBufferSettings.SequenceBufferSize = 1
		})
		old := groupDispositionControlPacket(100, 500, 64, tcpFlagSyn)
		f.update.updateSequence(groupRecoveryParsed(t, old))
		data := groupRecoveryPacket(101, 500, 64, tcpFlagAck, []byte{1})
		syn := groupDispositionControlPacket(900, 800, 64, tcpFlagSyn)
		if f.offer(old) != 0 || f.offer(data) != 1 {
			t.Fatal("old SYN gate or source queue setup failed")
		}
		f.requireParked()
		if f.offer(syn) != 0 || f.update.sequenceSynOffers != 0 || f.update.synGenerationNumber != 100 ||
			!f.update.canUpdateSequenceForClient(groupRecoveryParsed(t, syn), f.selected) {
			t.Fatal("refused SYN acquired new-generation proof or lost retry eligibility")
		}
		f.unpark()
		f.acknowledgeAll(data)
		if f.offer(syn) != 1 {
			t.Fatal("same public provider refused the eligible new-SYN retry")
		}
		f.acknowledgeAll(syn)
		if f.update.synGenerationNumber != 900 || f.offer(syn) != 0 {
			t.Fatal("successful retry did not replace the old cohort's identical-SYN gate")
		}
		next := groupDispositionControlPacket(1000, 900, 64, tcpFlagSyn)
		if f.offer(next) != 1 {
			t.Fatal("different SYN inherited the previous cohort's gate")
		}
		f.acknowledgeAll(next)
		if f.update.synGenerationNumber != 1000 || f.offer(next) != 0 || f.update.sequenceSynOffers != 0 ||
			f.update.synAdmissions != nil || budget.UsedByteCount() != 0 {
			t.Fatal("new cohort failed to settle its own ownership and duplicate gate")
		}
	})
}

// A same-ISN control admitted amid an unresolved reset may fail open at the
// public gate, but it is still not a generation reset when source order arrives.
func TestTcpGroupRecoveryResetSameSynCannotReopen(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newGroupDispositionQueueFixture(t, nil, nil)
		old := groupDispositionControlPacket(100, 500, 64, tcpFlagSyn)
		f.update.updateSequence(groupRecoveryParsed(t, old))
		if f.offer(old) != 0 {
			t.Fatal("established identical SYN was not gated")
		}
		rst := groupDispositionControlPacket(101, 700, 64, tcpFlagRst|tcpFlagAck)
		data := groupRecoveryPacket(101, 800, 64, tcpFlagAck, []byte{1})
		same := groupDispositionControlPacket(100, 800, 64, tcpFlagSyn)
		if f.offer(rst) != 1 || f.offer(data, same) != 2 {
			t.Fatal("reset and progressing same-SYN group did not both reach the source queue")
		}
		f.requireParked()
		f.unpark()
		f.acknowledgeAll(rst, data, same)
		if !f.update.IsDone() || f.update.synGenerationNumber != 100 || f.update.sequenceSynOffers != 0 ||
			f.update.sequenceClaims != nil || f.update.sourceRstSequence() != 800 {
			t.Fatal("same SYN reopened a reset cohort or retained its close barrier")
		}
	})
}

// Cancel the second actual receive after the first physical reset has a real
// applied peer ACK. The optional producer pause covers callback-before-return.
func testTcpGroupRecoveryResetSynCancellation(t *testing.T, producerPaused bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		first, releaseFirst := make(chan struct{}), make(chan struct{})
		producerEntered, releaseProducer := make(chan struct{}), make(chan struct{})
		terminal := make(chan struct{}, 1)
		var firstOnce, producerOnce sync.Once
		unparkFirst := func() { firstOnce.Do(func() { close(releaseFirst) }) }
		unparkProducer := func() { producerOnce.Do(func() { close(releaseProducer) }) }
		var offers, receives, appliedAcks atomic.Int64
		budget := NewTransferMemoryBudget(kib(64))
		f := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			observer := settings.SendBufferSettings.SendPackLifecycleObserver
			settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
				observer(event)
				if event.Phase == SendPackLifecyclePhaseTerminal && event.Err != nil {
					select {
					case terminal <- struct{}{}:
					default:
					}
				}
			}
			settings.SendBufferSettings.beforeGroupAdmissionForTest = func() {
				if offers.Add(1) == 2 && producerPaused {
					close(producerEntered)
					<-releaseProducer
				}
			}
			settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(_ sendSequenceId, sequence uint64) {
				if sequence == 0 {
					close(first)
					<-releaseFirst
				}
			}
			settings.SendBufferSettings.afterAckSendItemForTest = func(_ sendSequenceId, sequence uint64) {
				if sequence == 0 {
					appliedAcks.Add(1)
				}
			}
			settings.SendBufferSettings.beforeGroupDequeueForTest = func(source *SendSequence) {
				if receives.Add(1) == 2 {
					if appliedAcks.Load() != 1 {
						t.Error("first physical reset's ACK was not applied before second-receive cancellation")
					}
					source.Cancel()
				}
			}
		})
		t.Cleanup(unparkFirst)
		t.Cleanup(unparkProducer)
		f.update.updateSequence(groupRecoveryParsed(t, groupDispositionControlPacket(100, 500, 64, tcpFlagSyn)))
		rst := groupDispositionControlPacket(101, 700, 64, tcpFlagRst|tcpFlagAck)
		syn := groupDispositionControlPacket(900, 800, 64, tcpFlagSyn)
		if f.offer(rst) != 1 {
			t.Fatal("reset admission refused")
		}
		result := make(chan int, 1)
		go func() { result <- f.offer(syn) }()
		f.requireParked()
		if producerPaused {
			select {
			case <-producerEntered:
			default:
				t.Fatal("producer did not reach the actual post-enqueue gap")
			}
		} else if <-result != 1 {
			t.Fatal("second successful public admission refused")
		}
		f.unpark()
		synctest.Wait()
		select {
		case <-first:
		default:
			t.Fatal("source did not reach the reset's first physical write")
		}
		if !producerPaused && f.update.sourceRstSequence() != 800 {
			t.Fatal("older reset replay overwrote the later successful admission before serialization")
		}
		f.acknowledgeAll(rst)
		unparkFirst()
		// This actual terminal follows the typed target callback. Do not wait
		// for source Close while the deliberately paused producer holds Pack.
		<-terminal
		if f.update.sourceRstSequence() != 800 || f.update.synGenerationNumber != 100 ||
			f.update.sequenceClaims != nil || f.terminal.Load() != 2 || len(f.route) != 0 {
			t.Fatal("canceled receive reset the cohort, lost accepted ACK, or forwarded the raw SYN")
		}
		if producerPaused {
			if f.update.IsDone() || f.update.sequenceSynOffers != 1 {
				t.Fatal("source teardown passed the unfinished public SYN offer")
			}
			unparkProducer()
			if <-result != 1 {
				t.Fatal("canceled retained source rewrote the already-successful channel handoff")
			}
		}
		synctest.Wait()
		if !f.update.IsDone() || f.update.sourceRstSequence() != 800 || f.update.sequenceSynOffers != 0 ||
			f.update.synAdmissions != nil || budget.UsedByteCount() != 0 {
			t.Fatal("completed cancellation retained its close barrier or lost the latest accepted ACK")
		}
	})
}

// Successful public return precedes cancellation, but not SYN serialization.
func TestTcpGroupRecoveryResetSynCancellationKeepsAcceptedAck(t *testing.T) {
	testTcpGroupRecoveryResetSynCancellation(t, false)
}

// The source reconciles admission even when cancellation beats its producer.
func TestTcpGroupRecoveryResetSynCanceledObserverGap(t *testing.T) {
	testTcpGroupRecoveryResetSynCancellation(t, true)
}

// An older canceled receive can also beat its paused producer while a later
// accepted ACK is already known. Disposal must not replace that later state.
func TestTcpGroupRecoveryCanceledOlderObserverKeepsFollowingAck(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		terminal := make(chan struct{}, 1)
		var releaseOnce sync.Once
		unpark := func() { releaseOnce.Do(func() { close(release) }) }
		var offers atomic.Int64
		budget := NewTransferMemoryBudget(kib(64))
		f := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			observer := settings.SendBufferSettings.SendPackLifecycleObserver
			settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
				observer(event)
				if event.Phase == SendPackLifecyclePhaseTerminal && event.Err != nil {
					select {
					case terminal <- struct{}{}:
					default:
					}
				}
			}
			settings.SendBufferSettings.beforeGroupAdmissionForTest = func() {
				if offers.Add(1) == 1 {
					close(entered)
					<-release
				}
			}
			settings.SendBufferSettings.beforeGroupDequeueForTest = func(source *SendSequence) { source.Cancel() }
		})
		t.Cleanup(unpark)
		f.update.updateSequence(groupRecoveryParsed(t, groupDispositionControlPacket(100, 500, 64, tcpFlagSyn)))
		rst := groupDispositionControlPacket(101, 700, 64, tcpFlagRst|tcpFlagAck)
		ack := groupDispositionControlPacket(101, 800, 64, tcpFlagAck)
		result := make(chan int, 1)
		go func() { result <- f.offer(rst) }()
		f.requireParked()
		select {
		case <-entered:
		default:
			t.Fatal("older producer did not pause after irreversible enqueue")
		}
		if f.offer(ack) != 1 || f.update.sourceRstSequence() != 800 {
			t.Fatal("following ACK was not successfully accepted before cancellation")
		}
		f.unpark()
		<-terminal
		if f.update.sourceRstSequence() != 800 || f.update.synGenerationNumber != 100 ||
			f.update.sequenceControlOrder != 0 || f.update.IsDone() || len(f.route) != 0 {
			t.Fatal("canceled older receive replayed its reset or overwrote the following accepted ACK")
		}
		unpark()
		if <-result != 1 {
			t.Fatal("canceled source changed the earlier successful channel handoff")
		}
		synctest.Wait()
		if f.update.sourceRstSequence() != 800 || f.update.sequenceClaims != nil ||
			f.update.sequenceSynOffers != 0 || f.terminal.Load() != 2 || budget.UsedByteCount() != 0 {
			t.Fatal("canceled source failed to release both claims while preserving accepted state")
		}
	})
}

// Established drain policies receive both groups before disposing either.
// A cold flow-isolating destination first receives ordinarily, opens its writer,
// and disposes that canceled group before its next iteration drains the second.
func testTcpGroupRecoveryCanceledDrain(t *testing.T, flowIsolation, coldStart bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var receives atomic.Int64
		var first *parsedPacketGroup
		budget := NewTransferMemoryBudget(kib(64))
		f := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			settings.SendBufferSettings.beforeGroupDequeueForTest = func(source *SendSequence) {
				switch receives.Add(1) {
				case 1:
					if source.ctx.Err() != nil || source.flowIsolation.Load() != (flowIsolation && !coldStart) {
						t.Error("first receive did not use the required live source and initial carrier policy")
					}
					// Cancellation is reentrant at the actual receive, before the
					// typed target can replay any source-generation controls.
					source.Cancel()
				case 2:
					flags := first.completionFlags.Load()
					if source.ctx.Err() == nil || source.flowIsolation.Load() != flowIsolation ||
						flags&groupCompletionReceived == 0 || flags&(groupCompletionDequeued|groupCompletionSourceControls) != 0 {
						t.Error("second receive lost cancellation or replayed earlier source controls")
					}
					if coldStart {
						if flags&groupCompletionDisposed == 0 || flags&groupCompletionLinked != 0 || first.collapseSource != nil {
							t.Error("cold writer startup did not dispose the first ordinary receive before the later drain")
						}
					} else if flags&groupCompletionDisposed != 0 || flags&groupCompletionLinked == 0 {
						t.Error("drain did not retain the earlier cleanup-received group without control replay")
					}
				default:
					t.Error("unexpected extra source receive")
				}
			}
		})
		if flowIsolation {
			f.client.RouteManager().UpdateTransportWithProperties(f.transport, []Route{f.route}, TransferCarrierProperties{
				Unreliable: true, UnreliableFlowIsolation: true,
			})
		}
		first = groupRecoveryCallerGroup(t, f, groupDispositionControlPacket(100, 700, 64, tcpFlagRst|tcpFlagAck))
		second := groupRecoveryCallerGroup(t, f, groupDispositionControlPacket(100, 800, 64, tcpFlagAck))
		for _, group := range []*parsedPacketGroup{first, second} {
			if !f.selected.SendGroupWithAck(group, 0, true) {
				MessagePoolReturn(group.packets[0].packet)
				t.Fatal("control did not reach the actual source queue")
			}
		}
		f.requireParked()
		source := first.collapseSource
		if source == nil || source != second.collapseSource || f.update.sourceRstSequence() != 800 {
			t.Fatal("source and latest accepted ACK setup failed")
		}
		if source.contractMultiRouteWriter != nil || source.transferFlightPolicy().flowIsolation || receives.Load() != 0 {
			t.Fatal("source was not cold and unreceived at the owned pre-Run barrier")
		}
		if flowIsolation && !coldStart {
			// Both public offers returned and Run is parked: its writer state
			// has one fixture owner until unpark. Normal Run cleanup closes it.
			if source.openContractMultiRouteWriter() == nil || !source.transferFlightPolicy().flowIsolation {
				t.Fatal("real destination selector did not establish the flow-isolating drain policy")
			}
		}
		if !flowIsolation {
			source.preparedHandoffWake <- struct{}{}
		}
		f.unpark()
		synctest.Wait()
		if receives.Load() != 2 || source.flowIsolation.Load() != flowIsolation {
			t.Fatal("actual carrier branch did not drain both cleanup receives")
		}
		if f.update.sourceRstSequence() != 800 || f.update.IsDone() || f.update.sequenceControlOrder != 0 ||
			f.update.sequenceClaims != nil || f.terminal.Load() != 2 || budget.UsedByteCount() != 0 || len(f.route) != 0 {
			t.Fatal("cleanup-received predecessor overwrote the later ACK or replayed canceled controls")
		}
		for _, group := range []*parsedPacketGroup{first, second} {
			if flags := group.completionFlags.Load(); flags&groupCompletionReceived == 0 ||
				flags&(groupCompletionDequeued|groupCompletionSourceControls) != 0 || group.collapseSource != nil {
				t.Fatal("cleanup receive acquired a live control ordinal or retained its source worker")
			}
		}
		select {
		case <-source.done:
		default:
			t.Fatal("canceled source worker did not join")
		}
		if source.contractMultiRouteWriter != nil {
			t.Fatal("canceled source retained its destination selector")
		}
	})
}

// Ordinary carriers drain raw ownership on the prepared-cancellation wake.
func TestTcpGroupRecoveryCanceledOrdinaryDrainKeepsLatestAck(t *testing.T) {
	testTcpGroupRecoveryCanceledDrain(t, false, false)
}

// An established H3 selector drains the same fixed ingress owner set.
func TestTcpGroupRecoveryCanceledFlowIsolatingDrainKeepsLatestAck(t *testing.T) {
	testTcpGroupRecoveryCanceledDrain(t, true, false)
}

// A cold selector starts with ordinary ingress, then drains after opening.
// Reentrant cancellation must preserve the latest accepted ACK across both.
func TestTcpGroupRecoveryCanceledColdFlowIsolatingStartupKeepsLatestAck(t *testing.T) {
	testTcpGroupRecoveryCanceledDrain(t, true, true)
}

// Two real race candidates may replay the same source cohorts at different
// times. The newest open cohort cannot be canceled by an older candidate RST.
func testTcpGroupRecoverySeparateRaceReset(t *testing.T, sameSyn, refuseOtherSyn bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		firstBudget, secondBudget := NewTransferMemoryBudget(kib(64)), NewTransferMemoryBudget(kib(64))
		f := newGroupDispositionQueueFixture(t, firstBudget, nil)
		other := newGroupDispositionQueueFixture(t, secondBudget, func(settings *ClientSettings) {
			if refuseOtherSyn {
				settings.SendBufferSettings.SequenceBufferSize = 2
			}
		})
		f.update.updateSequence(groupRecoveryParsed(t, groupDispositionControlPacket(100, 500, 64, tcpFlagSyn)))
		f.update.client.Store(nil)
		f.parent.settings.MultiRaceSetOnResponseTimeout = time.Hour
		f.parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			return []*multiClientChannel{f.selected, other.selected}
		}
		syn := groupDispositionControlPacket(900, 500, 64, tcpFlagSyn)
		rst := groupDispositionControlPacket(901, 700, 64, tcpFlagRst|tcpFlagAck)
		next := groupDispositionControlPacket(1000, 800, 64, tcpFlagSyn)
		if f.offer(syn) != 1 || f.offer(rst) != 1 {
			t.Fatal("candidate SYN/reset did not reach both pre-Run sources")
		}
		firstTemplates := [][]byte{syn, rst}
		otherTemplates := [][]byte{syn, rst}
		if sameSyn {
			data := groupRecoveryPacket(901, 800, 64, tcpFlagAck, []byte{1})
			if f.offer(data, syn) != 2 {
				t.Fatal("progressing candidate same-SYN group refused")
			}
			firstTemplates = append(firstTemplates, data, syn)
			otherTemplates = append(otherTemplates, data, syn)
		} else {
			if f.offer(next) != 1 {
				t.Fatal("separate candidate new-SYN admission refused")
			}
			firstTemplates = append(firstTemplates, next)
			if !refuseOtherSyn {
				otherTemplates = append(otherTemplates, next)
			}
		}
		f.requireParked()
		other.requireParked()
		f.unpark()
		f.acknowledgeAll(firstTemplates...)
		if f.update.IsDone() {
			t.Fatal("first source retired the unresolved candidate controls")
		}
		other.unpark()
		other.acknowledgeAll(otherTemplates...)
		if sameSyn {
			if !f.update.IsDone() || f.update.race != nil || f.update.synGenerationNumber != 100 {
				t.Fatal("identical candidate SYN reopened the reset cohort")
			}
		} else {
			if f.update.IsDone() || f.update.race == nil {
				t.Fatal("older candidate reset orphaned the separately accepted newer cohort")
			}
			f.update.stateLock.Lock()
			f.update.commitRaceClientWithLock(f.selected)
			f.update.stateLock.Unlock()
			f.update.releaseCollapseClaims()
			if f.update.IsDone() || f.update.synGenerationNumber != 1000 || f.offer(next) != 0 {
				t.Fatal("winning candidate failed to retain its source-ordered new cohort")
			}
		}
		if f.update.sequenceSynOffers != 0 || f.update.sequenceClaims != nil ||
			firstBudget.UsedByteCount() != 0 || secondBudget.UsedByteCount() != 0 {
			t.Fatal("candidate close/promotion retained source metadata or charged ownership")
		}
	})
}

// Separate new-SYN groups supersede the prior RST on each real race source.
func TestTcpGroupRecoverySeparateRaceResetThenNewSynKeepsCohort(t *testing.T) {
	testTcpGroupRecoverySeparateRaceReset(t, false, false)
}

// Identical SYNs in progressing groups do not count as fresh source cohorts.
func TestTcpGroupRecoverySeparateRaceResetSameSynCloses(t *testing.T) {
	testTcpGroupRecoverySeparateRaceReset(t, true, false)
}

// One candidate's failed new-SYN admission cannot let its late old RST erase
// the different candidate that actually accepted and retained the new cohort.
func TestTcpGroupRecoveryLateCandidateResetCannotCloseNewSynWinner(t *testing.T) {
	testTcpGroupRecoverySeparateRaceReset(t, false, true)
}

// A candidate's final SYN is successfully dequeued but loses its first joint
// reservation. Its earlier retained RST cannot become the winner's final cohort.
func TestTcpGroupRecoveryUnwrittenNewSynPromotionKeepsSemanticTail(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(kib(64))
		var reservations atomic.Int64
		admitted := make(chan *parsedPacketGroup, 2)
		f := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			settings.SendBufferSettings.beforeGroupRetainForTest = func() {
				if reservations.Add(1) == 2 {
					budget.SetTotalByteCount(0)
				}
			}
			settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
				select {
				case admitted <- target.(*parsedPacketGroup):
				default:
				}
			}
		})
		other := newGroupDispositionQueueFixture(t, NewTransferMemoryBudget(kib(64)), nil)
		f.update.updateSequence(groupRecoveryParsed(t, groupDispositionControlPacket(100, 500, 64, tcpFlagSyn)))
		f.update.egressFinSeen, f.update.ingressFinSeen = true, true
		f.update.egressFinSequence, f.update.ingressFinSequence = 102, 600
		f.update.client.Store(nil)
		f.parent.settings.MultiRaceSetOnResponseTimeout = time.Hour
		f.parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			return []*multiClientChannel{f.selected, other.selected}
		}
		rst := groupDispositionControlPacket(101, 700, 64, tcpFlagRst|tcpFlagAck)
		syn := groupDispositionControlPacket(1000, 800, 96, tcpFlagSyn)
		if f.offer(rst) != 1 || f.offer(syn) != 1 {
			t.Fatal("separate candidate reset/new-SYN admissions refused")
		}
		f.requireParked()
		other.requireParked()
		first, second := <-admitted, <-admitted
		source := first.collapseSource
		f.unpark()
		f.acknowledgeAll(rst)
		if reservations.Load() != 2 || f.terminal.Load() != 2 || second.collapseOwner != nil ||
			second.completionFlags.Load()&groupCompletionLinked != 0 || second.collapseSource != nil ||
			f.update.IsDone() || f.update.race == nil {
			t.Fatal("unwritten new-SYN admission lost its scalar semantics or retained an unfunded descriptor")
		}
		state := f.update.race.clientStates[f.selected]
		if state.collapseControl.synNumber != 1000 || state.collapseControl.closed || state.collapseControl.synOrder == 0 {
			t.Fatal("existing race owner did not retain the successfully dequeued new cohort")
		}
		f.update.stateLock.Lock()
		f.update.commitRaceClientWithLock(f.selected)
		f.update.stateLock.Unlock()
		f.update.releaseCollapseClaims()
		if f.update.IsDone() || f.update.synGenerationNumber != 1000 || f.update.sourceRstSequence() != 800 ||
			f.update.sequenceAdmissionWindow != 96 || f.update.egressFinSeen || f.update.ingressFinSeen ||
			f.update.sequenceCovered || f.update.sequenceSynSeen || f.update.sequenceClaims != nil ||
			budget.UsedByteCount() != 0 || state.collapseControl != (tcpCandidateControlState{}) ||
			!f.update.canUpdateSequenceForClient(groupRecoveryParsed(t, syn), f.selected) {
			t.Fatal("promotion replayed the old reset, fabricated SYN proof, or retained scalar race history")
		}
		budget.SetTotalByteCount(kib(64))
		synctest.Wait()
		if source.resendCapacityUnavailable.Load() || f.offer(syn) != 1 {
			t.Fatal("same public winner did not admit the unwritten SYN after capacity publication")
		}
		f.acknowledgeAll(syn)
		if f.offer(syn) != 0 || budget.UsedByteCount() != 0 || f.update.sequenceSynOffers != 0 {
			t.Fatal("actual retained retry did not install its own identical-SYN gate and release ownership")
		}
	})
}

// Equal group ordinals do not order members. A wholly unwritten group still
// keeps its final semantic action on the existing race owner until promotion.
func testTcpGroupRecoveryUnwrittenMixedControls(t *testing.T, resetLast bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(1)
		f := newGroupDispositionQueueFixture(t, budget, nil)
		other := newGroupDispositionQueueFixture(t, nil, nil)
		f.update.updateSequence(groupRecoveryParsed(t, groupDispositionControlPacket(100, 500, 64, tcpFlagSyn)))
		f.update.client.Store(nil)
		f.parent.settings.MultiRaceSetOnResponseTimeout = time.Hour
		f.parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			return []*multiClientChannel{f.selected, other.selected}
		}
		syn := groupDispositionControlPacket(900, 800, 96, tcpFlagSyn)
		rst := groupDispositionControlPacket(901, 900, 128, tcpFlagRst|tcpFlagAck)
		packets := [][]byte{rst, syn}
		wantAck, wantWindow := uint32(800), uint16(96)
		if resetLast {
			packets = [][]byte{syn, rst}
			wantAck, wantWindow = 900, 128
		}
		if f.offer(packets...) != 2 {
			t.Fatal("mixed control group did not reach public admission")
		}
		f.requireParked()
		other.requireParked()
		f.unpark()
		synctest.Wait()
		if len(f.route) != 0 || f.terminal.Load() != 1 || budget.UsedByteCount() != 0 || f.update.race == nil {
			t.Fatal("unfunded mixed group retained physical ownership or lost its race")
		}
		state := f.update.race.clientStates[f.selected]
		if state.collapseControl.synOrder != state.collapseControl.rstOrder ||
			state.collapseControl.closed != resetLast {
			t.Fatal("equal-ordinal semantic state discarded member order")
		}
		f.update.stateLock.Lock()
		f.update.commitRaceClientWithLock(f.selected)
		f.update.stateLock.Unlock()
		f.update.releaseCollapseClaims()
		if f.update.IsDone() != resetLast || f.update.synGenerationNumber != 900 ||
			f.update.sourceRstSequence() != wantAck || f.update.sequenceAdmissionWindow != wantWindow ||
			f.update.sequenceCovered || f.update.sequenceSynSeen || f.update.sequenceClaims != nil ||
			state.collapseControl != (tcpCandidateControlState{}) || budget.UsedByteCount() != 0 {
			t.Fatal("unwritten mixed-control promotion guessed ordinal ties or fabricated retained proof")
		}
	})
}

// A final reset remains terminal even after a new SYN in the same raw group.
func TestTcpGroupRecoveryUnwrittenSynThenResetPromotesClosed(t *testing.T) {
	testTcpGroupRecoveryUnwrittenMixedControls(t, true)
}

// A final genuine new SYN supersedes the earlier reset in that raw group.
func TestTcpGroupRecoveryUnwrittenResetThenSynPromotesOpen(t *testing.T) {
	testTcpGroupRecoveryUnwrittenMixedControls(t, false)
}

// A candidate which refused the RST can win while its later same-SYN group
// is still raw. Winner replay must preserve the other candidate's matching reset.
func TestTcpGroupRecoverySameCohortResetSurvivesWinnerPromotion(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var receives atomic.Int64
		var releaseOnce sync.Once
		unpark := func() { releaseOnce.Do(func() { close(release) }) }
		firstBudget, secondBudget := NewTransferMemoryBudget(kib(64)), NewTransferMemoryBudget(kib(64))
		f := newGroupDispositionQueueFixture(t, firstBudget, nil)
		other := newGroupDispositionQueueFixture(t, secondBudget, func(settings *ClientSettings) {
			settings.SendBufferSettings.SequenceBufferSize = 1
			settings.SendBufferSettings.beforeGroupDequeueForTest = func(*SendSequence) {
				if receives.Add(1) == 2 {
					close(entered)
					<-release
				}
			}
		})
		t.Cleanup(unpark)
		f.update.updateSequence(groupRecoveryParsed(t, groupDispositionControlPacket(100, 500, 64, tcpFlagSyn)))
		f.update.client.Store(nil)
		f.parent.settings.MultiRaceSetOnResponseTimeout = time.Hour
		f.parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			return []*multiClientChannel{f.selected, other.selected}
		}
		syn := groupDispositionControlPacket(900, 500, 64, tcpFlagSyn)
		rst := groupDispositionControlPacket(901, 700, 64, tcpFlagRst|tcpFlagAck)
		data := groupRecoveryPacket(901, 800, 64, tcpFlagAck, []byte{1})
		if f.offer(syn) != 1 || f.offer(rst) != 1 {
			t.Fatal("initial race admission setup refused")
		}
		f.requireParked()
		other.requireParked()
		if other.started.Load() != 2 || other.terminal.Load() != 1 {
			t.Fatal("one-slot winner did not actually refuse its separate RST offer")
		}
		other.unpark()
		other.acknowledgeAll(syn)
		if f.offer(data, syn) != 2 {
			t.Fatal("progressing same-SYN group did not reach both candidates")
		}
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("winner's same-SYN group did not pause at its real receive boundary")
		}
		f.unpark()
		f.acknowledgeAll(syn, rst, data, syn)
		if f.update.IsDone() || !f.update.sequenceCloseReady.Load() {
			t.Fatal("same-cohort reset did not wait for the winner's pending raw control")
		}
		f.update.stateLock.Lock()
		f.update.commitRaceClientWithLock(other.selected)
		f.update.stateLock.Unlock()
		f.update.releaseCollapseClaims()
		if f.update.IsDone() || !f.update.sequenceCloseReady.Load() || f.update.synGenerationNumber != 900 {
			t.Fatal("winner's earlier open SYN erased the later matching-cohort reset")
		}
		unpark()
		other.acknowledgeAll(data, syn)
		if !f.update.IsDone() || f.update.race != nil || f.update.synGenerationNumber != 900 ||
			f.update.sequenceClaims != nil || f.update.sequenceSynOffers != 0 ||
			firstBudget.UsedByteCount() != 0 || secondBudget.UsedByteCount() != 0 {
			t.Fatal("same-SYN winner reopened the reset cohort or retained discarded race ownership")
		}
	})
}

// A retained return to the old ISN is a real new cohort when an intervening
// admitted SYN was wholly unwritten. Its exact member index cuts off old proof.
func TestTcpGroupRecoveryRetainedSynAfterUnwrittenAbaResetsCohort(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		failed, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		unpark := func() { releaseOnce.Do(func() { close(release) }) }
		var reservations atomic.Int64
		budget := NewTransferMemoryBudget(kib(64))
		f := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			settings.SendBufferSettings.beforeGroupRetainForTest = func() {
				if reservations.Add(1) == 2 {
					budget.SetTotalByteCount(0)
				}
			}
			observer := settings.SendBufferSettings.SendPackLifecycleObserver
			settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
				observer(event)
				if event.Phase == SendPackLifecyclePhaseTerminal && sendGroupCapacityFailure(event.Err) {
					close(failed)
					<-release
				}
			}
		})
		t.Cleanup(unpark)
		other := newGroupDispositionQueueFixture(t, nil, nil)
		syn := groupDispositionControlPacket(100, 500, 64, tcpFlagSyn)
		f.update.updateSequence(groupRecoveryParsed(t, syn))
		f.update.egressFinSeen, f.update.ingressFinSeen = true, true
		f.update.egressFinSequence, f.update.ingressFinSequence = 130, 600
		f.update.client.Store(nil)
		f.parent.settings.MultiRaceSetOnResponseTimeout = time.Hour
		f.parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			return []*multiClientChannel{f.selected, other.selected}
		}
		old := groupRecoveryPacket(101, 500, 64, tcpFlagAck, make([]byte, 10))
		middle := groupDispositionControlPacket(900, 700, 64, tcpFlagSyn)
		prefix := groupRecoveryPacket(101, 500, 64, tcpFlagAck, make([]byte, 20))
		if f.offer(old) != 1 || f.offer(middle) != 1 || f.offer(prefix, syn) != 2 {
			t.Fatal("candidate ABA groups did not all reach successful public admission")
		}
		f.requireParked()
		other.requireParked()
		f.unpark()
		synctest.Wait()
		select {
		case <-failed:
		default:
			t.Fatal("intervening SYN did not reach actual first-reservation refusal")
		}
		if reservations.Load() != 2 || len(f.route) != 1 {
			t.Fatal("reservation barrier did not isolate retained old data from wholly unwritten SYN")
		}
		budget.SetTotalByteCount(kib(64))
		unpark()
		f.acknowledgeAll(old, prefix, syn)
		if reservations.Load() != 3 || f.terminal.Load() != 3 || f.update.race == nil {
			t.Fatal("final retained A group did not follow the actual unfunded B")
		}
		f.update.stateLock.Lock()
		f.update.commitRaceClientWithLock(f.selected)
		f.update.stateLock.Unlock()
		f.update.releaseCollapseClaims()
		if f.update.IsDone() || f.update.synGenerationNumber != 100 || f.update.egressFinSeen || f.update.ingressFinSeen ||
			!f.update.canUpdateSequenceForClient(groupRecoveryParsed(t, prefix), f.selected) || f.offer(syn) != 0 ||
			f.update.sequenceClaims != nil || budget.UsedByteCount() != 0 {
			t.Fatal("retained final A was mistaken for identical old A after the unwritten B cohort")
		}
		if f.offer(prefix) != 1 {
			t.Fatal("pre-reset retained prefix falsely suppressed the actual new-cohort public offer")
		}
		f.acknowledgeAll(prefix)
		if f.offer(prefix) != 0 || budget.UsedByteCount() != 0 {
			t.Fatal("actual new-cohort data did not retain its own duplicate proof")
		}
	})
}

// One source accepted an old A/reset while the other accepted A/B/new A.
// Completed receipts stay charged until selection, including repeated cohorts.
func testTcpGroupRecoveryAmbiguousRaceAba(t *testing.T, repeat, closeAtEnd, capacityRefusal bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var finalPhase atomic.Bool
		var finalReceives atomic.Int64
		releaseFinal := make(chan struct{})
		var finalOnce sync.Once
		unparkFinal := func() { finalOnce.Do(func() { close(releaseFinal) }) }
		holdFinal := func(*SendSequence) {
			if finalPhase.Load() {
				finalReceives.Add(1)
				<-releaseFinal
			}
		}
		// Ten candidate receipts survive the repeat arm's ACKs. Also fund
		// the final reset item; this control must not test local capacity.
		firstLimit := kib(256)
		if capacityRefusal {
			firstLimit = kib(64)
		}
		firstBudget, secondBudget := NewTransferMemoryBudget(firstLimit), NewTransferMemoryBudget(kib(64))
		var admissions, capacityErrors, otherErrors atomic.Int64
		var firstGroups [4]*parsedPacketGroup
		f := newGroupDispositionQueueFixture(t, firstBudget, func(settings *ClientSettings) {
			settings.SendBufferSettings.beforeGroupDequeueForTest = holdFinal
			settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
				if index := int(admissions.Add(1)) - 1; index < len(firstGroups) {
					firstGroups[index] = target.(*parsedPacketGroup)
				}
			}
			observer := settings.SendBufferSettings.SendPackLifecycleObserver
			settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
				observer(event)
				if event.Phase == SendPackLifecyclePhaseTerminal && event.Err != nil {
					if sendGroupCapacityFailure(event.Err) {
						capacityErrors.Add(1)
					} else {
						otherErrors.Add(1)
					}
				}
			}
		})
		other := newGroupDispositionQueueFixture(t, secondBudget, func(settings *ClientSettings) {
			settings.SendBufferSettings.SequenceBufferSize = 2
			settings.SendBufferSettings.beforeGroupDequeueForTest = holdFinal
		})
		t.Cleanup(unparkFinal)
		f.update.updateSequence(groupRecoveryParsed(t, groupDispositionControlPacket(100, 500, 64, tcpFlagSyn)))
		f.update.client.Store(nil)
		f.parent.settings.MultiRaceSetOnResponseTimeout = time.Hour
		f.parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			return []*multiClientChannel{f.selected, other.selected}
		}
		syn := groupDispositionControlPacket(900, 500, 64, tcpFlagSyn)
		rst := groupDispositionControlPacket(901, 700, 64, tcpFlagRst|tcpFlagAck)
		middle := groupDispositionControlPacket(1000, 800, 64, tcpFlagSyn)
		if f.offer(syn) != 1 || f.offer(rst) != 1 || f.offer(middle) != 1 || f.offer(syn) != 1 {
			t.Fatal("partial race ABA admissions refused")
		}
		f.requireParked()
		other.requireParked()
		if other.started.Load() != 4 || other.terminal.Load() != 2 {
			t.Fatal("old-A candidate did not actually refuse both later ABA offers")
		}
		if admissions.Load() != 4 || f.started.Load() != 4 || f.terminal.Load() != 0 ||
			firstBudget.UsedByteCount() != 0 || secondBudget.UsedByteCount() != 0 {
			t.Fatal("four successful raw admissions acquired prequeue charge or completed before source ownership")
		}
		source := firstGroups[0].collapseSource
		var prefixPeerAck time.Time
		f.unpark()
		if capacityRefusal {
			synctest.Wait()
			last := firstGroups[3]
			flags := last.completionFlags.Load()
			if capacityErrors.Load() != 1 || otherErrors.Load() != 0 || f.terminal.Load() != 1 ||
				len(f.route) != 3 || last.collapseOwner != nil || last.collapseSource != nil ||
				flags&(groupCompletionDisposed|groupCompletionTerminal|groupCompletionReturned|groupCompletionDequeued) !=
					groupCompletionDisposed|groupCompletionTerminal|groupCompletionReturned|groupCompletionDequeued ||
				flags&groupCompletionLinked != 0 || f.update.IsDone() {
				t.Fatal("64 KiB did not isolate an admitted, source-ordered fourth group refused only at local retention")
			}
			f.acknowledgeAll(syn, rst, middle)
			if _, err := f.selected.WindowStats(); err != nil {
				t.Fatalf("local refusal poisoned the ACKed-prefix provider: %v", err)
			}
			f.selected.stateLock.Lock()
			nackCount, nackBytes := f.selected.packetStats.sendNackCount, f.selected.packetStats.sendNackByteCount
			ackCount, ackBytes := f.selected.packetStats.sendAckCount, f.selected.packetStats.sendAckByteCount
			pending, lastAck := f.selected.pendingSendTime, f.selected.lastSendAckTime
			f.selected.stateLock.Unlock()
			// The refusal completed before three independent successful ACKs.
			// Peer progress keeps its timestamp even when the channel is idle;
			// only the local-only completion path promises a zero clock.
			if nackCount != 0 || nackBytes != 0 || ackCount != 3 ||
				ackBytes != ByteCount(len(syn)+len(rst)+len(middle)) || lastAck.IsZero() ||
				!pending.Equal(lastAck) || f.selected.sendStalled(time.Nanosecond) {
				t.Fatalf("ACKed prefix/refusal accounting differs: outstanding=%d/%d acked=%d/%d pending=%v last_ack=%v",
					nackCount, nackBytes, ackCount, ackBytes, pending, lastAck)
			}
			prefixPeerAck = lastAck
			retainedBytes := ByteCount(0)
			for _, group := range firstGroups[:3] {
				if group.collapseOwner == nil || group.collapseOwner.materialized != 1 ||
					group.completionFlags.Load()&(groupCompletionTerminal|groupCompletionReturned|groupCompletionLinked) !=
						groupCompletionTerminal|groupCompletionReturned|groupCompletionLinked ||
					group.packets[0].packet != nil || group.packets[0].payload != nil || group.collapseSource != nil {
					t.Fatal("ACKed prefix lost its exact funded receipt or retained callback-owned roots")
				}
				retainedBytes += group.collapseOwner.budgetBytes
			}
			state := f.update.race.clientStates[f.selected].collapseControl
			if firstBudget.UsedByteCount() != retainedBytes || state.synNumber != 900 ||
				state.synOrder != last.collapseOrder() || state.closed || f.update.synGenerationNumber != 100 ||
				!f.update.sequenceSynSeen || f.update.sequenceSynNumber != 100 {
				t.Fatal("unwritten final A lost semantic admission or acquired durable proof")
			}
			// One public group's observation scope is shared by its candidates.
			// Native return clears both optional arrays; the reservation still
			// conservatively includes their full possible capacity per receipt.
			scope := firstGroups[0].admissionObservations
			if scope == nil {
				t.Fatal("real lifecycle-observed candidate lost its shared scope")
			}
			scope.mutex.Lock()
			completed, pendingCapacity, noAckCapacity := scope.completed, cap(scope.pending), cap(scope.noAckPending)
			scope.mutex.Unlock()
			if !completed || pendingCapacity != 0 || noAckCapacity != 0 {
				t.Fatal("returned public offer retained pending observer arrays")
			}
			shared := false
			for group := f.update.sequenceClaims; group != nil; group = group.collapseNext {
				if group.completionClient == other.selected && group.admissionObservations == scope {
					shared = true
				}
			}
			if !shared {
				t.Fatal("candidate copies did not share the original public observation scope")
			}
			t.Logf("candidate promotion accounting: receipt_extra_bytes=%d acked_receipts=%d scope_fixed_type_bytes=%d pending_capacity=%d no_ack_capacity=%d conservative_array_bytes=%d shared_scope=%t",
				firstGroups[0].collapseOwner.budgetBytes, retainedBytes, unsafe.Sizeof(sendPackAdmissionObservations{}),
				pendingCapacity, noAckCapacity, sendPackAdmissionObservationCapacity*
					(unsafe.Sizeof(pendingSendPackAdmissionObservation{})+unsafe.Sizeof(pendingNoAckAdmissionObservation{})), shared)
		} else {
			f.acknowledgeAll(syn, rst, middle, syn)
		}
		if repeat {
			for range 3 {
				if f.offer(middle) != 1 || f.offer(syn) != 1 {
					t.Fatal("repeated source-local transitions refused on the live candidate")
				}
				f.acknowledgeAll(middle, syn)
			}
		}
		other.unpark()
		other.acknowledgeAll(syn, rst)
		if f.update.IsDone() || f.update.sequenceCloseReady.Load() || f.update.race == nil {
			t.Fatal("old-A reset falsely matched the different source's ABA cohort")
		}
		state := f.update.race.clientStates[f.selected]
		if closeAtEnd {
			finalPhase.Store(true)
			if f.offer(rst) != 1 {
				t.Fatal("real final reset did not reach the candidate sources")
			}
			synctest.Wait()
			if finalReceives.Load() != 2 {
				t.Fatal("both final reset offers did not reach their actual receive barriers")
			}
			unparkFinal()
			f.acknowledgeAll(rst)
			other.acknowledgeAll(rst)
			if !f.update.IsDone() || f.update.race != nil {
				t.Fatal("unknown cross-source lineage prevented all-candidate actual resets from closing")
			}
		} else {
			f.update.stateLock.Lock()
			f.update.commitRaceClientWithLock(f.selected)
			f.update.stateLock.Unlock()
			f.update.releaseCollapseClaims()
			if capacityRefusal {
				if f.update.IsDone() || f.update.synGenerationNumber != 900 || f.update.sourceRstSequence() != 500 ||
					f.update.sequenceCovered || f.update.sequenceSynSeen || firstBudget.UsedByteCount() != 0 ||
					!f.update.canUpdateSequenceForClient(groupRecoveryParsed(t, syn), f.selected) {
					t.Fatal("promotion manufactured final-A proof or lost the latest accepted control")
				}
				// Refund and gate publication are separate source-owned steps.
				// Quiesce the real worker before requiring a zero-timeout retry.
				synctest.Wait()
				if source == nil || source.ctx.Err() != nil || source.resendCapacityUnavailable.Load() ||
					source.preparedFlightUnavailable.Load() || f.update.sequenceSourceId != source.sequenceId ||
					f.update.client.Load() != f.selected {
					t.Fatal("promotion refund did not leave the same live source's admission gates open")
				}
				// Advance only the bubble clock to distinguish this new send
				// from the prior ACK. No asynchronous progress relies on a timer.
				time.Sleep(time.Nanosecond)
				admittedAt := time.Now()
				if f.selected.sendStalled(time.Nanosecond) {
					t.Fatal("aged peer-ACK timestamp classified an idle provider as stalled")
				}
				if f.offer(syn) != 1 {
					t.Fatal("actual public retry of the wholly unwritten final A was suppressed")
				}
				f.selected.stateLock.Lock()
				nackCount, nackBytes := f.selected.packetStats.sendNackCount, f.selected.packetStats.sendNackByteCount
				ackCount, ackBytes := f.selected.packetStats.sendAckCount, f.selected.packetStats.sendAckByteCount
				pending, lastAck := f.selected.pendingSendTime, f.selected.lastSendAckTime
				f.selected.stateLock.Unlock()
				if !admittedAt.After(prefixPeerAck) || nackCount != 1 || nackBytes != ByteCount(len(syn)) ||
					ackCount != 3 || ackBytes != ByteCount(len(syn)+len(rst)+len(middle)) ||
					!pending.Equal(admittedAt) || !lastAck.Equal(prefixPeerAck) {
					t.Fatal("new public admission failed to restart the idle clock or fabricated peer-ACK credit")
				}
				f.acknowledgeAll(syn)
			}
			if f.update.IsDone() || f.update.synGenerationNumber != 900 || f.offer(syn) != 0 {
				t.Fatal("selected ABA source lost its own exact new-SYN generation or durable gate")
			}
		}
		wantCapacityErrors := int64(0)
		if capacityRefusal {
			wantCapacityErrors = 1
		}
		if capacityErrors.Load() != wantCapacityErrors || otherErrors.Load() != 0 ||
			f.started.Load() != f.terminal.Load() {
			t.Fatal("funded ABA control hid an unexpected terminal failure or incomplete callback")
		}
		if f.update.sequenceClaims != nil || state.collapseControl != (tcpCandidateControlState{}) ||
			firstBudget.UsedByteCount() != 0 || secondBudget.UsedByteCount() != 0 {
			t.Fatal("race termination retained semantic state or charged candidate receipts")
		}
	})
}

// Matching an old ISN cannot establish lineage after an intervening generation.
func TestTcpGroupRecoveryAmbiguousRaceAbaFailsOpen(t *testing.T) {
	testTcpGroupRecoveryAmbiguousRaceAba(t, false, false, false)
}

// Unknown lineage remains unknown after any later return to the same ISN.
func TestTcpGroupRecoveryAmbiguousRaceAbaSaturates(t *testing.T) {
	testTcpGroupRecoveryAmbiguousRaceAba(t, true, false, false)
}

// Conservative cross-source ambiguity does not exempt actual terminal controls.
func TestTcpGroupRecoveryAmbiguousRaceAbaFinalResetsClose(t *testing.T) {
	testTcpGroupRecoveryAmbiguousRaceAba(t, true, true, false)
}

// A real full budget disposes the fourth raw group, not its semantic transition.
// Promotion must not synthesize retained proof; only the public retry supplies it.
func TestTcpGroupRecoveryAmbiguousRaceAbaCapacityKeepsSemanticTail(t *testing.T) {
	testTcpGroupRecoveryAmbiguousRaceAba(t, false, false, true)
}
