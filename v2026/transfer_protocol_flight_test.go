package connect

import (
	"maps"
	"reflect"
	"testing"
)

func newProtocolFlightRegression(t *testing.T) (*SendSequence, *sendItem, *sendItem) {
	t.Helper()
	settings := DefaultClientSettings().SendBufferSettings
	settings.UnreliableInitialFlightByteCount = 8192
	settings.UnreliableMinimumFlightByteCount = 1024
	settings.UnreliableMaximumFlightByteCount = 16384
	settings.UnreliableInitialFlightMessageCount = 4
	settings.UnreliableMinimumFlightMessageCount = 1
	settings.UnreliableMaximumFlightMessageCount = 8
	controller := newSendFlightController(settings)
	controller.applyPolicy(transferFlightPolicySnapshot{generation: 1, limited: true, flowReserve: true})
	sequence := &SendSequence{client: &Client{}, flightController: controller, nextSequenceNumber: 3}
	dataPath := reliableDeliveryTestPath(4)
	controlPath := *dataPath
	controlPath.SourcePort++
	data := &sendItem{transferItem: transferItem{sequenceNumber: 0}, transferFrameBytes: make([]byte, 8192), schedulingKey: ipSendSchedulingKey(dataPath)}
	control := &sendItem{transferItem: transferItem{sequenceNumber: 1}, noBudget: true, transferFrameBytes: make([]byte, 40), schedulingKey: ipSendSchedulingKey(&controlPath)}
	sequence.trackUnreliableFlight(data)
	if !data.unreliableFlightTracked || controller.byteCount != 8192 || controller.messageCount != 1 || controller.canSend() {
		t.Fatal("ordinary DATA did not fill the actual flight controller")
	}
	return sequence, data, control
}

func TestProtocolNoBudgetDoesNotTakeDataFlightReserve(t *testing.T) {
	sequence, data, control := newProtocolFlightRegression(t)
	before := *sequence.flightController
	before.messageCountByKey = maps.Clone(before.messageCountByKey)
	sequence.trackUnreliableFlight(control)
	if control.unreliableFlightTracked || control.unreliableFlowReserve || !reflect.DeepEqual(before, *sequence.flightController) {
		t.Fatal("zero-payload protocol took DATA flight ownership or its healthy-flow reserve")
	}
	sequence.releaseUnreliableFlight(control)
	sequence.forgetUnreliableFlight(control)
	if !reflect.DeepEqual(before, *sequence.flightController) {
		t.Fatal("protocol completion changed another flow's DATA flight")
	}
	sequence.releaseUnreliableFlight(data)
	if sequence.flightController.byteCount != 0 || sequence.flightController.messageCount != 0 {
		t.Fatal("ordinary DATA delivery did not release its own flight")
	}
}

func TestProtocolNoBudgetLossDoesNotContractDataFlight(t *testing.T) {
	sequence, data, control := newProtocolFlightRegression(t)
	before := *sequence.flightController
	before.messageCountByKey = maps.Clone(before.messageCountByKey)
	sequence.observeUnreliableFlightLoss(control)
	if !reflect.DeepEqual(before, *sequence.flightController) || sequence.client.unreliableFlightReductionCount.Load() != 0 {
		t.Fatal("lost protocol control reduced or altered DATA admission")
	}
	sequence.observeUnreliableFlightLoss(data)
	if sequence.flightController.byteLimit >= before.byteLimit || sequence.client.unreliableFlightReductionCount.Load() != 1 {
		t.Fatal("ordinary DATA loss no longer contracts its own flight")
	}
}
