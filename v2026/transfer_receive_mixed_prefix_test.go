// Mixed paid descriptors exercise the exact-budget prefix union. The separate
// signed Client control proves real decoder publication and DATA delivery.
package connect

import "testing"

// Real allocations and returns retain both budget identities and exact holes.
func TestReceiveConsumerRecoveryPrefixAcceptsMixedExactRxAndHeadCustody(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	fixture := newReceiveRecoveryPrefixBoundsFixture(t, 17)
	first := fixture.add(t, 17, fixture.rx)
	middle := fixture.add(t, 18, fixture.head)
	fixture.add(t, 19, fixture.rx)
	if !fixture.allows(20) || fixture.allows(21) || fixture.rx.UsedByteCount() != 2*decodedPackOwnerQueueByteCount ||
		fixture.head.UsedByteCount() != decodedPackOwnerQueueByteCount || fixture.root.UsedByteCount() != 3*decodedPackOwnerQueueByteCount {
		t.Fatal("exact mixed RX/Head custody lost its consecutive prefix or physical accounting")
	}
	fixture.release(middle)
	if fixture.allows(20) {
		t.Fatal("returned Head predecessor remained in the mixed prefix")
	}
	fixture.add(t, 18, fixture.head)
	if !fixture.allows(20) {
		t.Fatal("new independently paid middle position did not restore the mixed prefix")
	}
	fixture.release(first)
	if fixture.allows(20) {
		t.Fatal("returned RX predecessor remained in the mixed prefix")
	}
	fixture.add(t, 17, fixture.other)
	if fixture.allows(20) {
		t.Fatal("an arbitrary sibling budget supplied the missing mixed-prefix position")
	}
}
