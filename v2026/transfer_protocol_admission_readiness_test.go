// Directed protocol readiness observes actual count transitions while ordinary
// blocked producers retain their existing reserved-slot FIFO ownership.
package connect

import "testing"

func TestProtocolAdmissionReadinessKeepsOrdinaryWaiterPriority(t *testing.T) {
	admission := newSendPackAdmission(1)
	key := sendSchedulingKey{}
	if acquired, closed, _ := admission.tryAcquire(key, false); !acquired || closed {
		t.Fatal("ordinary original did not acquire the sole real count slot")
	}
	gate := admission.protocolReadinessGate()
	wake := make(chan struct{}, 1)
	slot := new(resendCapacityWaiter)
	epoch := gate.watch(slot, wake)
	defer gate.abandonWatch(slot, epoch)
	_, _, ordinary := admission.tryAcquire(key, true)
	if ordinary == nil || epoch == 0 || testingCapacityReady(wake) {
		t.Fatal("full source did not retain ordinary and directed waiters")
	}
	admission.release(key)
	if !ordinary.granted || !testingCapacityReady(ordinary.ready) || !gate.Load() || testingCapacityReady(wake) || admission.count != 1 {
		t.Fatal("directed notification stole an ordinary producer's reserved slot")
	}
	admission.release(key)
	if gate.Load() || !testingCapacityReady(wake) || !gate.watchGranted(slot, epoch) || !gate.passWatch(slot, epoch) || admission.count != 0 {
		t.Fatal("ordinary completion failed to publish the remaining real vacancy")
	}
	testingCapacityEmpty(t, gate)
}

func TestProtocolAdmissionReadinessGrantCancellationPublishesVacancy(t *testing.T) {
	admission := newSendPackAdmission(1)
	key := sendSchedulingKey{}
	admission.tryAcquire(key, false)
	gate := admission.protocolReadinessGate()
	wake := make(chan struct{}, 1)
	slot := new(resendCapacityWaiter)
	epoch := gate.watch(slot, wake)
	defer gate.abandonWatch(slot, epoch)
	_, _, ordinary := admission.tryAcquire(key, true)
	admission.release(key)
	if ordinary == nil || !ordinary.granted || !gate.Load() {
		t.Fatal("ordinary waiter did not own its grant before cancellation")
	}
	admission.abandon(ordinary)
	if admission.count != 0 || gate.Load() || !testingCapacityReady(wake) || !gate.passWatch(slot, epoch) {
		t.Fatal("canceled ordinary grant stranded protocol readiness")
	}
	testingCapacityEmpty(t, gate)
}

func TestProtocolAdmissionReadinessReclosePreservesDirectedOrder(t *testing.T) {
	admission := newSendPackAdmission(1)
	key := sendSchedulingKey{}
	admission.tryAcquire(key, false)
	gate := admission.protocolReadinessGate()
	wakeFirst, wakeSecond := make(chan struct{}, 1), make(chan struct{}, 1)
	first, second := new(resendCapacityWaiter), new(resendCapacityWaiter)
	firstEpoch, secondEpoch := gate.watch(first, wakeFirst), gate.watch(second, wakeSecond)
	defer gate.abandonWatch(first, firstEpoch)
	defer gate.abandonWatch(second, secondEpoch)
	admission.release(key)
	if !testingCapacityReady(wakeFirst) || testingCapacityReady(wakeSecond) {
		t.Fatal("one vacancy granted more than the first directed turn")
	}
	if acquired, _, _ := admission.tryAcquire(key, false); !acquired {
		t.Fatal("existing zero-time protocol offer lost its available count slot")
	}
	if gate.passWatch(first, firstEpoch) || testingCapacityReady(wakeSecond) || gate.waiterCount.Load() != 2 {
		t.Fatal("stolen vacancy admitted or discarded the first directed place")
	}
	admission.release(key)
	if !testingCapacityReady(wakeFirst) || !gate.passWatch(first, firstEpoch) || !testingCapacityReady(wakeSecond) || !gate.passWatch(second, secondEpoch) {
		t.Fatal("next real vacancy failed to preserve directed turn order")
	}
	testingCapacityEmpty(t, gate)
}

func TestProtocolAdmissionReadinessCloseRevokesAndLateSubscribeStaysClosed(t *testing.T) {
	admission := newSendPackAdmission(1)
	key := sendSchedulingKey{}
	admission.tryAcquire(key, false)
	gate := admission.protocolReadinessGate()
	wake := make(chan struct{}, 1)
	slot := new(resendCapacityWaiter)
	epoch := gate.watch(slot, wake)
	_, _, ordinary := admission.tryAcquire(key, true)
	admission.close()
	if !gate.closed.Load() || !testingCapacityReady(wake) || !testingCapacityReady(ordinary.ready) ||
		ordinary.granted || gate.watchGranted(slot, epoch) || gate.passWatch(slot, epoch) {
		t.Fatal("terminal count source granted capacity or failed to wake withdrawal")
	}
	gate.abandonWatch(slot, epoch)
	admission.release(key)
	admission.close()
	if !gate.Load() || gate.watch(new(resendCapacityWaiter), wake) != 0 {
		t.Fatal("released count reopened terminal readiness")
	}
	testingCapacityEmpty(t, gate)
	late := newSendPackAdmission(1)
	late.close()
	if !late.protocolReadinessGate().closed.Load() {
		t.Fatal("first registration after terminal close published a false opening")
	}
}

func TestProtocolAdmissionReadinessOrdinaryPathKeepsLazyGate(t *testing.T) {
	admission := newSendPackAdmission(3)
	key := sendSchedulingKey{}
	allocations := testing.AllocsPerRun(1000, func() {
		if acquired, closed, _ := admission.tryAcquire(key, false); !acquired || closed {
			t.Fatal("ordinary protocol admission changed without a subscriber")
		}
		admission.release(key)
	})
	if allocations != 0 || admission.protocolReadiness != nil || admission.count != 0 || len(admission.waiters) != 0 {
		t.Fatalf("ordinary protocol traffic created readiness ownership: allocations=%v gate=%p count=%d", allocations, admission.protocolReadiness, admission.count)
	}
}
