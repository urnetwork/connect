package connect

import "testing"

func TestDatagramAdmissionObservationRequiresExactOwnerCompletion(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		owner := &sendPackAdmissionObservations{}
		var completed []NoAckSendObservation
		observer := owner.wrapNoAck(func(observation NoAckSendObservation) { completed = append(completed, observation) })
		observer(NoAckSendObservation{Phase: NoAckSendPhaseCompleted, Token: 1, Err: ErrNoAckSendNotAdmitted})
		if len(completed) != 0 {
			t.Fatal("refused input reported before its owner finished")
		}
		// An independent accepted write failure cannot be recovered by this owner.
		observer(NoAckSendObservation{Phase: NoAckSendPhaseCompleted, Token: 2, Err: errTransferRouteWriteTimeout})
		if len(completed) != 1 || completed[0].RecoveredByOwner {
			t.Fatal("accepted write failure hidden by a refused admission")
		}
		owner.complete(accepted)
		if len(completed) != 2 || completed[1].Token != 1 || completed[1].RecoveredByOwner != accepted {
			t.Fatalf("exact owner disposition lost: %+v", completed)
		}
	}
}

func TestDatagramAdmissionObservationOverflowRemainsFailure(t *testing.T) {
	owner := &sendPackAdmissionObservations{}
	var completed []NoAckSendObservation
	observer := owner.wrapNoAck(func(observation NoAckSendObservation) { completed = append(completed, observation) })
	for index := range sendPackAdmissionObservationCapacity + 1 {
		observer(NoAckSendObservation{Phase: NoAckSendPhaseCompleted, Token: uint64(index + 1), Err: ErrNoAckSendNotAdmitted})
	}
	if len(completed) != 1 || !completed[0].OwnerTrackingOverflow || completed[0].RecoveredByOwner {
		t.Fatal("unknown overflow history received successful recovery credit")
	}
	owner.complete(true)
	if len(completed) != sendPackAdmissionObservationCapacity+1 {
		t.Fatal("bounded observer lost terminal pairing")
	}
	for _, observation := range completed[1:] {
		if !observation.RecoveredByOwner || observation.OwnerTrackingOverflow {
			t.Fatal("retained owner history lost recovery credit")
		}
	}
}
