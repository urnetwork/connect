// Concurrent statistics must not borrow a send worker's mutable writer handle.
package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Uses real route publication with an inert sequence: only each test's named
// readers and lifecycle operation run concurrently, and no packet is sent.
func newWindowStatsWriterFixture(t *testing.T, open bool) *SendSequence {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
		settings.DeliverySizedWindowScale = 2
		settings.ResendQueueMaxByteCount = 512 * 1024
		settings.ResendQueueMinByteCount = 64 * 1024
		settings.ResendQueueBudget = NewTransferMemoryBudget(16 * 1024 * 1024)
		settings.TargetGoodputByteRate = 125000000
	})
	sequence.destination = NewId()
	manager := NewRouteManagerWithLogger(ctx, "window-stats-writer", NewNoopLogger())
	sequence.client = &Client{
		routeManager: manager,
		contractManager: &ContractManager{
			sendNoContractClientIds: map[Id]bool{sequence.destination: true},
		},
	}
	sequence.sendBuffer = &SendBuffer{
		sendSequences:              map[sendSequenceId]*SendSequence{sequence.id(): sequence},
		sendSequencesByDestination: map[Id]map[*SendSequence]bool{},
		sendSequenceDestinations:   map[*SendSequence]map[Id]bool{},
	}
	sequence.windowPacer.service = newWindowPacingService(sequence.sendBufferSettings)
	now := time.Now()
	sequence.rttWindow.closeSendTime(uint64(now.Add(-10*time.Millisecond).UnixMilli()), now)
	sequence.observeReceiveWindowAdvertisement(receiveAckMessage{
		receiveWindowSet: true, receiveWindowByteCount: 8 * 1024 * 1024,
		ackCompressTimeoutSet: true,
	})
	manager.UpdateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{make(Route, 1)})
	if open {
		sequence.openContractMultiRouteWriter()
		if !sequence.transferFlightPolicy().h1Only {
			t.Fatal("fixture did not publish the H1 writer")
		}
	}
	t.Cleanup(func() {
		sequence.closeContractMultiRouteWriter()
		sequence.resendQueue.Clear()
	})
	return sequence
}

// Wait at each handle's first read/publication after preceding external calls.
// Separate once guards order no read against a publication; releasing the
// common barrier leaves only the production leaf lock to order those accesses.
func holdWindowStatsWriterAccesses(sequence *SendSequence) (<-chan bool, func()) {
	arrived := make(chan bool, 2)
	resume := make(chan struct{})
	var readOnce, publishOnce sync.Once
	sequence.beforeContractWriterAccessForTest = func(publish bool) {
		once := &readOnce
		if publish {
			once = &publishOnce
		}
		once.Do(func() {
			arrived <- publish
			<-resume
		})
	}
	return arrived, func() { close(resume) }
}

// The access barrier orders neither the statistics read nor teardown after
// the other. Each executes exactly once; selector retirement is already done
// before publication, so it cannot accidentally synchronize away the race.
func TestWindowStatsConcurrentWriterTeardown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence := newWindowStatsWriterFixture(t, true)
		arrived, resume := holdWindowStatsWriterAccesses(sequence)
		start := make(chan struct{})
		closed := make(chan struct{})
		read := make(chan SendDestinationStats, 1)
		go func() {
			<-start
			sequence.closeContractMultiRouteWriter()
			close(closed)
		}()
		go func() {
			<-start
			read <- sequence.sendBuffer.DestinationSendStats(sequence.destination)
		}()
		close(start)
		first, second := <-arrived, <-arrived
		resume()
		stats := <-read
		<-closed
		if first == second {
			t.Fatal("the barrier did not stop both a reader and publisher")
		}
		if stats.SequenceCount != 1 || stats.SendWindow.Window != 512*1024 || stats.SendWindow.Sized || stats.SendWindow.ServiceSized {
			t.Fatalf("teardown corrupted the statistics snapshot: %+v", stats)
		}
		if sequence.contractMultiRouteWriter != nil || sequence.transferFlightPolicy().h1Only {
			t.Fatal("closed writer remained published")
		}
	})
}

// Opening has the same publication boundary as closing. The policy consumer
// may see either side of the publication, then must see the live H1 policy
// after both operations have joined.
func TestWindowStatsConcurrentWriterPublication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence := newWindowStatsWriterFixture(t, false)
		arrived, resume := holdWindowStatsWriterAccesses(sequence)
		start := make(chan struct{})
		opened := make(chan struct{})
		read := make(chan transferFlightPolicySnapshot, 1)
		go func() {
			<-start
			sequence.openContractMultiRouteWriter()
			close(opened)
		}()
		go func() {
			<-start
			read <- sequence.transferFlightPolicy()
		}()
		close(start)
		first, second := <-arrived, <-arrived
		resume()
		policy := <-read
		<-opened
		if first == second {
			t.Fatal("the barrier did not stop both a reader and publisher")
		}
		if policy.limited || !sequence.transferFlightPolicy().h1Only {
			t.Fatalf("writer publication lost the current carrier: %+v", policy)
		}
	})
}

// A physical writer reference can keep selector retirement waiting. A stats
// reader must still finish while that reference is deliberately retained;
// holding the publication lock around CloseMultiRouteWriter would deadlock.
func TestWindowStatsWriterRetirementDoesNotBlockSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence := newWindowStatsWriterFixture(t, true)
		selector := sequence.contractMultiRouteWriter.(*MultiRouteSelector)
		snapshot := selector.activeRoutesSnapshot.Load()
		if !snapshot.acquireWriter() {
			t.Fatal("fixture could not retain the live writer generation")
		}
		pause := &routeSnapshotWriterPause{waiting: make(chan struct{})}
		snapshot.writerPause.Store(pause)
		closed := make(chan struct{})
		go func() {
			sequence.closeContractMultiRouteWriter()
			close(closed)
		}()
		<-pause.waiting
		read := make(chan SendDestinationStats, 1)
		go func() { read <- sequence.sendBuffer.DestinationSendStats(sequence.destination) }()
		synctest.Wait()
		var stats SendDestinationStats
		ready := false
		select {
		case stats = <-read:
			ready = true
		default:
		}
		snapshot.releaseWriter()
		<-closed
		if !ready {
			<-read
			t.Fatal("statistics waited for external writer retirement")
		}
		if stats.SequenceCount != 1 || stats.SendWindow.Window != 512*1024 {
			t.Fatalf("retirement lost the remaining sequence statistics: %+v", stats)
		}
	})
}
