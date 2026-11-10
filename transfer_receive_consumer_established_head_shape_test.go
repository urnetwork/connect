// These controls cover physical admission of the two actual current Head
// representations. Their worker-free metadata never claims signed authority;
// the live wire roots separately exercise verification, delivery and ACK.
package connect

import (
	"testing"

	"github.com/urnetwork/connect/protocol"
)

func TestReceiveConsumerEstablishedCurrentHeadPaysBothWireForms(t *testing.T) {
	for _, shape := range []string{"compact", "full", "switch"} {
		func() {
			f := newEstablishedHeadGuardFixtureWithShape(t, shape, 1)
			owner := f.take(f.scan, false)
			if owner == nil {
				t.Fatalf("%s current Head lost independent physical admission", shape)
			}
			wire := f.wire
			f.wire = nil
			f.decode(t, owner, wire)
			if owner.receiveItem.memoryBudget != f.head || owner.receiveItem.QueueByteCount() != f.scan.retainedByteCount ||
				owner.receivePack.ingressSequence != f.sequence || owner.receivePack.ingressLoanByteCount != 0 ||
				owner.receiveItem.receiveFreshOpening || f.sequence.holdPromise.set ||
				f.sequence.holdPromise.reservedByteCount != 0 || f.sequence.holdPromise.ingressFloor != f.scan.sequenceNumber ||
				f.sequence.receiveContract != nil || len(f.sequence.openReceiveContracts) != 0 ||
				f.rx.UsedByteCount() != f.scan.retainedByteCount || f.head.UsedByteCount() != f.scan.retainedByteCount ||
				f.parent.UsedByteCount() != 2*f.scan.retainedByteCount || f.root.UsedByteCount() != f.parent.UsedByteCount() {
				t.Fatalf("%s Head changed authority or lost exclusive whole-C ownership", shape)
			}
			if !f.sequence.bindReceiveMemoryOwner(&owner.receivePack) {
				t.Fatalf("%s paid current Head did not retain its exact original ingress binding", shape)
			}
		}()
	}
}

func TestReceiveConsumerEstablishedCurrentHeadRejectsOtherWireClasses(t *testing.T) {
	for _, shape := range []string{"compact", "full", "switch"} {
		for _, row := range []struct {
			name   string
			change func(*establishedHeadGuardFixture, *receiveMemoryPackScan)
		}{
			{"unestablished", func(f *establishedHeadGuardFixture, _ *receiveMemoryPackScan) {
				f.sequence.holdPromise.establishedContract = false
			}},
			{"unsafe", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) { s.loanSafe = false }},
			{"legacy", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) { s.legacy = true }},
			{"compatibility", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) { s.compatibility = true }},
			{"no-ack", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) { s.nack = true }},
			{"ahead", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) { s.contractAhead = true }},
			{"encrypted-control", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) { s.hasEncryptedControl = true }},
			{"invalid-role", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) {
				s.sessionRoleSet, s.sessionRole = true, 99
			}},
			{"bare-head", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) {
				s.head = true
				s.contractIdSet, s.contractFrameSet = false, false
			}},
			{"both-representations", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) {
				s.contractIdSet, s.contractFrameSet = true, true
				s.contractFrameType = int32(protocol.MessageType_TransferContract)
			}},
			{"wrong-proof-type", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) {
				s.contractIdSet, s.contractFrameSet = false, true
				s.contractFrameType = int32(protocol.MessageType_TestSimpleMessage)
			}},
			{"non-head-reference", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) {
				s.head = false
				s.contractIdSet, s.contractFrameSet = true, false
			}},
			{"empty", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) {
				s.frameCount, s.consumerFrameCount = 0, 0
			}},
			{"mixed", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) { s.consumerFrameCount-- }},
			{"provider", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) {
				s.consumerFrameCount = 0
				s.providerFrameCount = s.frameCount
			}},
			{"zero-cost", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) { s.retainedByteCount = 0 }},
			{"future", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) { s.sequenceNumber++ }},
			{"wrapping-current", func(f *establishedHeadGuardFixture, s *receiveMemoryPackScan) {
				s.sequenceNumber = ^uint64(0)
				f.sequence.holdPromise.ingressFloor = s.sequenceNumber
				f.sequence.nextSequenceNumber = s.sequenceNumber
			}},
		} {
			func() {
				t.Logf("ESTABLISHED_CURRENT_HEAD_EXCLUSION shape=%s case=%s", shape, row.name)
				f := newEstablishedHeadGuardFixtureWithShape(t, shape, 1)
				scan := f.scan
				row.change(f, &scan)
				f.reject(t, scan, false)
			}()
		}
	}
}

func TestReceiveConsumerEstablishedCurrentHeadKeepsRequiredContext(t *testing.T) {
	for _, shape := range []string{"compact", "full", "switch"} {
		func() {
			f := newEstablishedHeadGuardFixtureWithShape(t, shape, 1)
			f.sequence.session = &peerEncryptionSession{settings: &EncryptionSettings{Mode: EncryptionModeRequired}}
			f.reject(t, f.scan, false)
			before := f.counts()
			if owner := f.buffer.takeReceiveHeadRecoveryOwner(f.sequence, f.scan); owner != nil {
				f.owners = append(f.owners, owner)
				t.Fatalf("%s compatibility wrapper invented authenticated decryption", shape)
			}
			if f.counts() != before {
				t.Fatal("Required refusal changed physical ownership")
			}
			owner := f.take(f.scan, true)
			if owner == nil {
				t.Fatalf("%s actual decrypted current Head lost physical admission", shape)
			}
			wire := f.wire
			f.wire = nil
			f.decode(t, owner, wire)
		}()
	}
}

func TestReceiveConsumerEstablishedCurrentHeadKeepsChildClientRootCaps(t *testing.T) {
	for _, shape := range []string{"compact", "full", "switch"} {
		for _, scope := range []string{"child", "client", "root"} {
			func() {
				f := newEstablishedHeadGuardFixtureWithShape(t, shape, 1)
				switch scope {
				case "child":
					f.allocate(t, f.head)
				case "client":
					f.allocate(t, f.clientPressure)
					f.allocate(t, f.clientPressure)
				case "root":
					for range 3 {
						f.allocate(t, f.rootPressure)
					}
				}
				f.reject(t, f.scan, false)
			}()
		}
	}
}

func TestReceiveConsumerEstablishedCurrentHeadKeepsExactDuplicateWithSpare(t *testing.T) {
	for _, shape := range []string{"compact", "full", "switch"} {
		func() {
			f := newEstablishedHeadGuardFixtureWithShape(t, shape, 2)
			owner := f.take(f.scan, false)
			if owner == nil {
				t.Fatalf("%s duplicate control did not pay its first current Head", shape)
			}
			wire := f.wire
			f.wire = nil
			f.decode(t, owner, wire)
			if f.rx.Available() != 0 || f.head.Available() != f.scan.retainedByteCount ||
				f.parent.Available() < f.scan.retainedByteCount || f.root.Available() < f.scan.retainedByteCount {
				t.Fatal("duplicate refusal must have independently sufficient physical spare")
			}
			before := f.counts()
			for _, messageId := range []Id{f.scan.messageId, NewId()} {
				scan := f.scan
				scan.messageId = messageId
				if duplicate := f.take(scan, false); duplicate != nil {
					t.Fatal("same current position acquired another child owner despite exact identity fence")
				}
			}
			if f.counts() != before || f.sequence.holdPromise.ingressHead != &owner.receivePack {
				t.Fatal("duplicate changed the first current Head's physical claim or intrusive registration")
			}
		}()
	}
}

func TestReceiveConsumerEstablishedCurrentHeadKeepsCanceledAndClosed(t *testing.T) {
	for _, shape := range []string{"compact", "full", "switch"} {
		for _, closed := range []bool{false, true} {
			func() {
				f := newEstablishedHeadGuardFixtureWithShape(t, shape, 1)
				if closed {
					f.sequence.holdPromise.closed = true
				} else {
					f.cancel()
				}
				f.reject(t, f.scan, false)
			}()
		}
	}
}

func TestReceiveConsumerEstablishedCurrentHeadCannotInventSequence(t *testing.T) {
	for _, shape := range []string{"compact", "full", "switch"} {
		func() {
			f := newEstablishedHeadGuardFixtureWithShape(t, shape, 1)
			before := f.counts()
			if owner := f.buffer.takeReceiveHeadRecoveryOwnerInSession(nil, f.scan, false); owner != nil {
				f.owners = append(f.owners, owner)
				t.Fatal("established-current admission invented a missing sequence")
			}
			if f.counts() != before {
				t.Fatal("missing sequence admission changed physical claims")
			}
		}()
	}
}
