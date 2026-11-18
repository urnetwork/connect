// Past-ordinal custody is bounded recovery, never repeat application delivery.
package connect

import "testing"

func TestDeliveredDuplicateRecoveryUsesBoundedExistingChild(t *testing.T) {
	f := newEstablishedHeadGuardFixture(t)
	scan := f.scan
	scan.sequenceNumber--
	owner := f.take(scan, false)
	if owner == nil || owner.receiveItem.memoryBudget != f.head || f.head.UsedByteCount() != scan.retainedByteCount || f.rx.UsedByteCount() != scan.retainedByteCount {
		t.Fatal("past ordinal did not obtain independent, bounded physical custody")
	}
	if next := f.take(scan, false); next != nil {
		t.Fatal("pending duplicate acquired a second owner")
	}
}

func TestDeliveredDuplicateRecoveryPreservesRefusalBoundaries(t *testing.T) {
	for _, name := range []string{"closed", "canceled", "unsafe", "compatibility", "nack", "ahead", "encrypted-control", "legacy", "invalid-role", "wrong-proof", "root-full", "child-full"} {
		t.Run(name, func(t *testing.T) {
			f := newEstablishedHeadGuardFixture(t)
			scan := f.scan
			scan.sequenceNumber--
			switch name {
			case "closed":
				f.sequence.holdPromise.closed = true
			case "canceled":
				f.cancel()
			case "unsafe":
				scan.loanSafe = false
			case "compatibility":
				scan.compatibility = true
			case "nack":
				scan.nack = true
			case "ahead":
				scan.contractAhead = true
			case "encrypted-control":
				scan.hasEncryptedControl = true
			case "legacy":
				scan.legacy = true
			case "invalid-role":
				scan.sessionRoleSet = true
				scan.sessionRole = 99
			case "wrong-proof":
				scan.contractFrameSet = true
				scan.contractFrameType = -1
			case "root-full":
				n := f.root.Available()
				f.root.Reserve(n)
				defer f.root.Release(n)
			case "child-full":
				n := f.head.Available()
				f.head.Reserve(n)
				defer f.head.Release(n)
			}
			f.reject(t, scan, false)
		})
	}
}

func TestDeliveredDuplicateRecoveryRejectsMissingSequence(t *testing.T) {
	f := newEstablishedHeadGuardFixture(t)
	scan := f.scan
	scan.sequenceNumber--
	before := f.counts()
	if owner := f.buffer.takeReceiveHeadRecoveryOwnerInSession(nil, scan, false); owner != nil {
		owner.release()
		t.Fatal("missing sequence gained duplicate recovery custody")
	}
	if f.counts() != before {
		t.Fatal("missing sequence changed memory accounting")
	}
}
