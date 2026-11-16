// Empty protocol decoding is not DATA admission or contract verification.
package connect

import (
	"maps"
	"testing"
	"testing/synctest"
)

func requireProtocolOpeningDecodeWithoutDataClaims(t *testing.T, fixture *windowRoundFixture, root, child *TransferMemoryBudget, wire []byte, label string) {
	t.Helper()
	path, err := FilteredTransferPath(wire)
	if err != nil {
		t.Fatal(err)
	}
	path.DestinationId = fixture.receiver.ClientId()
	buffer := fixture.receiver.receiveBuffer
	buffer.mutex.Lock()
	sequences, heads, rejected := maps.Clone(buffer.receiveSequences), maps.Clone(buffer.headReceiveSequenceIds), maps.Clone(buffer.rejectedReceiveSequenceIds)
	buffer.mutex.Unlock()
	type claims struct {
		promise  receiveHoldPromise
		repair   receiveHeldHeadRepair
		pressure receiveMemoryPressure
	}
	prior := make(map[*ReceiveSequence]claims, len(sequences))
	root.admissionLock.Lock()
	for _, sequence := range sequences {
		prior[sequence] = claims{sequence.holdPromise, sequence.heldHeadRepair, sequence.memoryPressure}
	}
	root.admissionLock.Unlock()
	rx := fixture.receiver.settings.ReceiveBufferSettings.ReceiveQueueBudget
	before := root.StatsWithDescendants(rx, child)
	delivered, acked, acks := fixture.deliveredCount, fixture.ackedCount, len(fixture.receiverOut)
	decoded := &decodedTransferFrame{}
	defer decoded.release()
	ok, pressure := fixture.receiver.unmarshalReceiveMemoryFrame(wire, decoded, true, path, false, sequenceTlsRoleServer, false)
	owner := decoded.packOwner
	if !ok || pressure || !protocolOnlyPack(decoded.frame.Pack) || owner == nil || owner.receivePack.receiveMemoryOwned || owner.receivePack.ingressSequence != nil || owner.receiveItem.memoryBudget != nil || owner.receiveItem.QueueByteCount() != 0 {
		t.Fatalf("%s did not remain ordinary protocol decode without DATA custody", label)
	}
	decoded.release()
	synctest.Wait()
	after := root.StatsWithDescendants(rx, child)
	for index := range before {
		if before[index] != after[index] {
			t.Fatalf("%s changed paid DATA accounting", label)
		}
	}
	buffer.mutex.Lock()
	unchanged := maps.Equal(sequences, buffer.receiveSequences) && maps.Equal(heads, buffer.headReceiveSequenceIds) && maps.Equal(rejected, buffer.rejectedReceiveSequenceIds)
	buffer.mutex.Unlock()
	root.admissionLock.Lock()
	for sequence, value := range prior {
		unchanged = unchanged && value == (claims{sequence.holdPromise, sequence.heldHeadRepair, sequence.memoryPressure})
	}
	root.admissionLock.Unlock()
	if !unchanged || fixture.deliveredCount != delivered || fixture.ackedCount != acked || len(fixture.receiverOut) != acks {
		t.Fatalf("%s created receive, delivery or ACK authority during protocol decode", label)
	}
}
