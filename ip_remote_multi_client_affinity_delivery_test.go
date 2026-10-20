// Delivery roots exercise the real app callback and removal worker. Explicit
// channels force callback/map order; no elapsed-time or polling proof is used.
package connect

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The path is in the multi-client's canonical outbound orientation, including
// when the borrowed bytes are a returning packet or locally generated reset.
func affinityDeliveryTestPacket(update *multiClientChannelUpdate) *receivePacket {
	return &receivePacket{IpPath: update.ipPath, generation: update, Packet: []byte{1}}
}

// Infer the existing public callback's mode type without a parent-to-child
// import. The root observes only its canonical path and actual invocation.
func affinityDeliveryTestCallback[Mode ~int32](
	_ func(TransferPath, Mode, *IpPath, []byte),
	callback func(*IpPath),
) func(TransferPath, Mode, *IpPath, []byte) {
	return func(_ TransferPath, _ Mode, path *IpPath, _ []byte) { callback(path) }
}

func TestAffinityRenewalActiveDeliverySurvivesRemovedMap(t *testing.T) {
	for _, version := range []int{4, 6} {
		parent := affinityTimeTestParent(t)
		old, _ := affinityTimeTestDonor(t, parent, version)
		entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		resume := func() { releaseOnce.Do(func() { close(release) }) }
		var delivered atomic.Bool
		parent.SetReceivePacketCallback(affinityDeliveryTestCallback(ReceivePacketFunction(nil), func(_ *IpPath) {
			close(entered)
			<-release
		}))
		go func() {
			defer close(returned)
			delivered.Store(parent.deliverAffinityReceivePacket(affinityDeliveryTestPacket(old)))
		}()
		defer func() { resume(); <-returned }()
		<-entered
		if transition := parent.reserveAffinityTransition(old, old.ipPath, flowPin{}, time.Now().Add(time.Minute)); transition != nil {
			t.Fatal("renewal overtook an executing incumbent callback")
		}
		old.cancel()
		retired, _, _ := parent.detachIdleFlows(time.Now())
		if len(retired) != 1 || retired[0].update != old {
			t.Fatal("fixture did not remove the old canonical generation")
		}
		if next, _, _ := parent.sendUpdate(old.ipPath, flowPin{}); next != nil {
			t.Fatal("absent map hid an executing old callback from tuple creation")
		}
		resume()
		<-returned
		if !delivered.Load() || len(parent.affinityDeliveries) != 0 || old.affinityDelivery.Load() != 0 {
			t.Fatal("callback return did not release its exact active owner")
		}
		if next, _, _ := parent.sendUpdate(old.ipPath, flowPin{}); next == nil || next == old {
			t.Fatal("joined old callback prevented a later new generation")
		}
		old.Close()
	}
}

func TestAffinityRenewalQueuedOldControlRejectsNewCanonical(t *testing.T) {
	for _, version := range []int{4, 6} {
		parent := affinityTimeTestParent(t)
		old, _ := affinityTimeTestDonor(t, parent, version)
		transition := parent.reserveAffinityTransition(old, old.ipPath, flowPin{}, time.Now().Add(time.Minute))
		if transition == nil {
			t.Fatal("idle incumbent could not reserve renewal")
		}
		transition.admit()
		transition.finish()
		called := 0
		parent.SetReceivePacketCallback(affinityDeliveryTestCallback(ReceivePacketFunction(nil), func(_ *IpPath) { called++ }))
		if parent.deliverAffinityReceivePacket(affinityDeliveryTestPacket(old)) || called != 0 {
			t.Fatal("queued old generation reached the renewed application tuple")
		}
		if !parent.deliverAffinityReceivePacket(affinityDeliveryTestPacket(transition.next)) || called != 1 || len(parent.affinityDeliveries) != 0 {
			t.Fatal("fresh generation lost its callback or retained an active lease")
		}
		parent.detachIdleFlows(time.Now())
		if parent.receiveUpdate(old.ipPath) != transition.next {
			t.Fatal("old-generation cleanup removed the admitted replacement")
		}
	}
}

func TestAffinityRenewalRefusalRestoresDeliveryWithoutAba(t *testing.T) {
	parent := affinityTimeTestParent(t)
	old, client := affinityTimeTestDonor(t, parent, 4)
	parent.SetReceivePacketCallback(affinityDeliveryTestCallback(ReceivePacketFunction(nil), func(_ *IpPath) {}))
	first := parent.reserveAffinityTransition(old, old.ipPath, flowPin{}, time.Now().Add(time.Minute))
	if first == nil || parent.deliverAffinityReceivePacket(affinityDeliveryTestPacket(old)) {
		t.Fatal("pending admission did not close old callback enrollment")
	}
	first.finish()
	if old.IsDone() || old.client.Load() != client || parent.receiveUpdate(old.ipPath) != old ||
		!parent.deliverAffinityReceivePacket(affinityDeliveryTestPacket(old)) {
		t.Fatal("all-refused renewal changed incumbent custody or delivery authority")
	}
	second := parent.reserveAffinityTransition(old, old.ipPath, flowPin{}, time.Now().Add(time.Minute))
	if second == nil {
		t.Fatal("all-refused renewal prevented its retry")
	}
	first.finish()
	first.admit()
	if parent.deliverAffinityReceivePacket(affinityDeliveryTestPacket(old)) || parent.receiveUpdate(old.ipPath) != old {
		t.Fatal("stale completion reopened or replaced a newer reservation")
	}
	second.admit()
	second.finish()
	if parent.receiveUpdate(old.ipPath) != second.next || !old.IsDone() || len(parent.affinityTransitions) != 0 || len(parent.affinityDeliveries) != 0 {
		t.Fatal("retry did not finish exactly its own generation")
	}
}

func TestAffinityRenewalDeliveryPanicReleasesOwner(t *testing.T) {
	parent := affinityTimeTestParent(t)
	old, _ := affinityTimeTestDonor(t, parent, 4)
	parent.SetReceivePacketCallback(affinityDeliveryTestCallback(ReceivePacketFunction(nil), func(_ *IpPath) { panic("synthetic callback panic") }))
	panicked := func() (panicked bool) {
		defer func() { panicked = recover() != nil }()
		parent.deliverAffinityReceivePacket(affinityDeliveryTestPacket(old))
		return
	}()
	if !panicked || len(parent.affinityDeliveries) != 0 || old.affinityDelivery.Load() != 0 {
		t.Fatal("panic retained delivery custody")
	}
	transition := parent.reserveAffinityTransition(old, old.ipPath, flowPin{}, time.Now().Add(time.Minute))
	if transition == nil {
		t.Fatal("panicked callback stranded tuple admission")
	}
	transition.finish()
}

func TestAffinityRenewalDeliveryMapBoundAndUnknownOwner(t *testing.T) {
	parent := affinityTimeTestParent(t)
	leases := make([]multiClientAffinityDeliveryLease, 0, affinityDeliveryMaxTuples)
	defer func() {
		for _, lease := range leases {
			lease.release()
		}
	}()
	for index := range affinityDeliveryMaxTuples {
		lease, admitted := parent.acquireAffinityDelivery(affinityTimeTestPath(4, 20000+index), nil)
		if !admitted {
			t.Fatalf("live callback capacity refused entry %d", index)
		}
		leases = append(leases, lease)
	}
	if _, admitted := parent.acquireAffinityDelivery(affinityTimeTestPath(4, 40000), nil); admitted || len(parent.affinityDeliveries) != affinityDeliveryMaxTuples {
		t.Fatal("active-callback table exceeded its owner-local bound")
	}
	if update, _, _ := parent.sendUpdate(affinityTimeTestPath(4, 20000), flowPin{}); update != nil {
		t.Fatal("unmatched callback was ignored during canonical creation")
	}
	for _, lease := range leases {
		lease.release()
	}
	leases = nil
	if len(parent.affinityDeliveries) != 0 {
		t.Fatal("completed callbacks became historical table entries")
	}
	if update, _, _ := parent.sendUpdate(affinityTimeTestPath(4, 20000), flowPin{}); update == nil {
		t.Fatal("empty active table retained a failed creation")
	}
}

func TestAffinityRenewalCancellationJoinsExecutingDelivery(t *testing.T) {
	parent := affinityTimeTestParent(t)
	old, _ := affinityTimeTestDonor(t, parent, 4)
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	resume := func() { releaseOnce.Do(func() { close(release) }) }
	parent.SetReceivePacketCallback(affinityDeliveryTestCallback(ReceivePacketFunction(nil), func(_ *IpPath) {
		close(entered)
		<-release
	}))
	go func() {
		defer close(returned)
		parent.deliverAffinityReceivePacket(affinityDeliveryTestPacket(old))
	}()
	defer func() { resume(); <-returned }()
	<-entered
	parent.cancel()
	if len(parent.affinityDeliveries) != 1 {
		t.Fatal("cancellation forgot an application callback that had not returned")
	}
	if _, admitted := parent.acquireAffinityDelivery(old.ipPath, old); admitted {
		t.Fatal("canceled parent admitted new delivery custody")
	}
	resume()
	<-returned
	if len(parent.affinityDeliveries) != 0 || old.affinityDelivery.Load() != 0 {
		t.Fatal("canceled callback owner did not join cleanly")
	}
}

// The first callback parks the actual removal worker. A queued old reset is
// then superseded; a fresh marker proves the worker processed that stale item.
func TestAffinityRenewalRemovalWorkerDropsOldOwnedPacket(t *testing.T) {
	parent := affinityTimeTestParent(t)
	parent.removalReceiveQueue = make(chan receivePacket, 3)
	old, _ := affinityTimeTestDonor(t, parent, 4)
	sibling, _, _ := parent.sendUpdate(affinityTimeTestPath(4, 44002), flowPin{})
	entered, release, marker, joined := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	resume := func() { releaseOnce.Do(func() { close(release) }) }
	var callbacks atomic.Int32
	parent.SetReceivePacketCallback(affinityDeliveryTestCallback(ReceivePacketFunction(nil), func(path *IpPath) {
		callbacks.Add(1)
		if path.SourcePort == sibling.ipPath.SourcePort {
			close(entered)
			<-release
		} else {
			close(marker)
		}
	}))
	go func() { defer close(joined); parent.runRemovalReceive() }()
	defer func() { parent.cancel(); resume(); <-joined }()
	parent.removalReceiveQueue <- *affinityDeliveryTestPacket(sibling)
	<-entered
	pooled := MessagePoolCopy([]byte{1, 2, 3})
	witness := MessagePoolShareReadOnly(pooled)
	defer func() {
		if witness != nil {
			MessagePoolReturn(witness)
		}
	}()
	stale := affinityDeliveryTestPacket(old)
	stale.Packet, stale.releaseAfterDelivery = pooled, true
	parent.removalReceiveQueue <- *stale
	transition := parent.reserveAffinityTransition(old, old.ipPath, flowPin{}, time.Now().Add(time.Minute))
	if transition == nil {
		t.Fatal("unrelated active callback blocked tuple renewal")
	}
	transition.admit()
	transition.finish()
	parent.removalReceiveQueue <- *affinityDeliveryTestPacket(transition.next)
	resume()
	<-marker
	parent.cancel()
	<-joined
	released := MessagePoolReturn(witness)
	witness = nil
	if callbacks.Load() != 2 || len(parent.affinityDeliveries) != 0 || !released {
		t.Fatal("stale queued packet delivered, leaked its pool owner, or retained a callback lease")
	}
}
