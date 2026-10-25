// The producer and source barriers independently pin queue admission, source
// close control and candidate commitment without a scheduler or timer premise.
package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

func nativeSynRetryCloseOrderCase(t *testing.T, candidateFirst bool, fin bool) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var armed atomic.Bool
		producerEntered, sourceEntered, sourceWritten := make(chan struct{}), make(chan struct{}), make(chan struct{})
		releaseProducer, releaseSource := make(chan struct{}), make(chan struct{})
		var producerOnce, sourceOnce, writtenOnce, producerReleaseOnce, sourceReleaseOnce sync.Once
		var sourceWriteFailed atomic.Bool
		var harness *reraceHarness
		configureB := func(settings *ClientSettings) {
			settings.SendBufferSettings.beforeGroupAdmissionForTest = func() {
				if !armed.Load() || !candidateFirst {
					return
				}
				producerOnce.Do(func() { close(producerEntered) })
				select {
				case <-releaseProducer:
				case <-harness.ctx.Done():
				}
			}
			settings.SendBufferSettings.beforeGroupDequeueForTest = func(source *SendSequence) {
				if !armed.Load() {
					return
				}
				sourceOnce.Do(func() { close(sourceEntered) })
				if !candidateFirst {
					select {
					case <-releaseSource:
					case <-harness.ctx.Done():
					}
				}
			}
			settings.SendBufferSettings.SendPackLifecycleObserver = func(observation SendPackLifecycleObservation) {
				if armed.Load() && observation.Phase == SendPackLifecyclePhaseFirstRouteWrite &&
					observation.MessageType == protocol.MessageType_IpIpPacketToProvider {
					sourceWriteFailed.Store(observation.Err != nil)
					writtenOnce.Do(func() { close(sourceWritten) })
				}
			}
		}
		path := reraceFlow(4, 0, 51313)
		harness = newSilenceInferenceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: reraceAlways(reraceSilent)},
			{name: "b", tier: 1, answer: reraceAlways(reraceSilent), clientSettings: configureB},
		}, nil)
		if !privateStaleBoundNativeSyn(t, harness, path) {
			t.Fatal("original native SYN did not acquire A source ownership")
		}
		update := harness.update(path)
		owner := synRetryControlOwner(t, update)
		update.stateLock.Lock()
		credit := owner.credit
		update.stateLock.Unlock()
		retryEntered, releaseRetry := synRetryControlHold(t, harness)
		harness.inject("a", reraceProviderSignal(t, path))
		synRetryControlEntered(t, retryEntered)
		if update.client.Load() != nil {
			t.Fatal("explicit failure did not unbind the tested generation")
		}
		type offerResult struct {
			Count    int
			Accepted bool
		}
		offered := make(chan offerResult, 1)
		offerDone := make(chan struct{})
		offerStarted := false
		t.Cleanup(func() {
			producerReleaseOnce.Do(func() { close(releaseProducer) })
			sourceReleaseOnce.Do(func() { close(releaseSource) })
			releaseRetry()
			if offerStarted {
				<-offerDone
			}
		})
		flags := byte(tcpFlagRst)
		if fin {
			flags = tcpFlagFin | tcpFlagAck
		}
		packet := MessagePoolCopy(reraceTcpPacket(path, flags, path.SequenceNumber+1, reraceServerIsn+1, nil))
		armed.Store(true)
		offerStarted = true
		go func() {
			accepted := [1]bool{}
			count := harness.multiClient.SendNativePacketBatchWithResults(harness.source, protocol.ProvideMode_Network, [][]byte{packet}, 0, accepted[:])
			offered <- offerResult{Count: count, Accepted: accepted[0]}
			close(offerDone)
		}()
		if candidateFirst {
			<-producerEntered
			<-sourceEntered
			<-sourceWritten
			select {
			case <-offerDone:
				t.Fatal("candidate RST producer committed before its forced source close")
			default:
			}
		} else {
			<-sourceEntered
			<-offerDone
			harness.settle()
			update.stateLock.Lock()
			queuedOwner := update.synRetry == owner && owner.references == 2 && owner.packet != nil && owner.credit != nil
			selected := update.client.Load()
			live := !update.IsDone() && !update.sourceReset.Load()
			blocked := selected != nil || !owner.validWithLock(update)
			pendingReset := update.pendingCollapseResetWithLock()
			update.stateLock.Unlock()
			if !queuedOwner || !live || !blocked || !fin && !pendingReset || credit.released.Load() {
				t.Fatal("queued close did not retain exact custody while barring SYN dispatch")
			}
			sourceReleaseOnce.Do(func() { close(releaseSource) })
			<-sourceWritten
		}
		harness.settle()
		update.stateLock.Lock()
		cachePresent, refs, done := update.synRetry != nil, owner.references, update.IsDone()
		cacheReleased := !cachePresent && refs == 1 && owner.packet != nil && owner.credit != nil
		selected, halfClosed := update.client.Load(), update.egressFinSeen
		update.stateLock.Unlock()
		if sourceWriteFailed.Load() || !fin && !done || fin && (done || !halfClosed) || candidateFirst && selected != nil || harness.update(path) != update {
			t.Fatal("source close did not reach the exact candidate/generation ordering")
		}
		if credit.released.Load() {
			t.Fatal("source close returned credit still held by the stopped retry worker")
		}
		producerReleaseOnce.Do(func() { close(releaseProducer) })
		<-offerDone
		result := <-offered
		if result.Count != 1 || !result.Accepted {
			t.Fatal("the real native close did not transfer exactly one source owner")
		}
		// The initial health connection on A survives this other flow's close
		// even while the shared parent reaper remains stopped at retry dispatch.
		sibling := reraceFlow(4, 1, 49111)
		before := len(harness.delivered(sibling))
		if !harness.send(reraceSegment(sibling, 24, []byte("after-close"))) ||
			len(harness.delivered(sibling)) != before+1 || harness.boundTo(sibling) != harness.channel("a") {
			t.Fatal("closing the retry generation interrupted its healthy sibling")
		}
		releaseRetry()
		harness.settle()
		releasedBeforeJoin := credit.released.Load()
		if err := harness.multiClient.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		synRetryControlReleased(t, owner)
		if !credit.released.Load() || len(harness.exit("b").syns(path)) != 0 {
			t.Fatal("closed SYN authority replayed or retained its final credit")
		}
		if !cacheReleased {
			t.Errorf("CANDIDATE_RST_RETIRED_WITH_LIVE_SYN_CACHE: cache_present=%t refs=%d done=%t producer_pending=%t", cachePresent, refs, done, candidateFirst)
		} else if !releasedBeforeJoin {
			t.Fatal("the released retry worker retained credit until parent shutdown")
		}
	})
}

func TestNativeSynRetryCandidateRstRetirementReleasesCache(t *testing.T) {
	nativeSynRetryCloseOrderCase(t, true, false)
}

func TestNativeSynRetryQueuedRstDefersFinalCreditUntilDequeue(t *testing.T) {
	nativeSynRetryCloseOrderCase(t, false, false)
}

func TestNativeSynRetryQueuedFinKeepsHalfCloseAndSibling(t *testing.T) {
	nativeSynRetryCloseOrderCase(t, false, true)
}
