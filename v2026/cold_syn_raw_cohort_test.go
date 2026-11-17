// A new candidate SYN may be accepted but still await its source FIFO dequeue.
package connect

import (
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// Current/newer source authority can prove a matching accepted raw SYN even
// while the candidate's last dequeued semantic tail still names the prior ISN.
func TestColdSynCandidateNewGenerationRawOwnerSuppressesDuplicate(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var hold atomic.Bool
		var entered atomic.Int32
		release := make(chan struct{})
		var releaseOnce sync.Once
		unpark := func() { releaseOnce.Do(func() { close(release) }) }
		configure := func(settings *ClientSettings) {
			settings.SendBufferSettings.beforeGroupDequeueForTest = func(source *SendSequence) {
				if hold.Load() {
					entered.Add(1)
					select {
					case <-release:
					case <-source.ctx.Done():
					}
				}
			}
		}
		first := newGroupDispositionQueueFixture(t, nil, configure)
		second := newGroupDispositionQueueFixture(t, nil, configure)
		t.Cleanup(unpark)
		first.update.client.Store(nil)
		first.parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			return []*multiClientChannel{first.selected, second.selected}
		}
		oldSyn := groupDispositionControlPacket(100, 0, 64, tcpFlagSyn)
		freshSyn := groupDispositionControlPacket(900, 0, 64, tcpFlagSyn)
		if first.offer(oldSyn) != 1 {
			t.Fatal("initial candidate SYN did not acquire real source ownership")
		}
		first.requireParked()
		second.requireParked()
		first.unpark()
		second.unpark()
		first.acknowledgeAll(oldSyn)
		second.acknowledgeAll(oldSyn)
		hold.Store(true)
		if first.offer(freshSyn) != 1 {
			t.Fatal("different SYN did not acquire real candidate queues")
		}
		synctest.Wait()
		if entered.Load() != 2 || first.started.Load() != 2 || second.started.Load() != 2 {
			t.Fatal("both actual new-generation source consumers did not reach the raw dequeue hold")
		}
		if first.offer(freshSyn) != 0 || first.started.Load() != 2 || second.started.Load() != 2 {
			t.Fatal("accepted current-source raw SYN was duplicated while semantic tail still named the older ISN")
		}
		unpark()
		synctest.Wait()
		first.acknowledgeAll(freshSyn)
		second.acknowledgeAll(freshSyn)
		if first.offer(freshSyn) != 0 {
			t.Fatal("actual newly retained candidate SYN lost its duplicate gate after peer ACK")
		}
	})
}
