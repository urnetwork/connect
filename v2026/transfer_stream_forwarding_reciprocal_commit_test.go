// Forces reciprocal retirement at the physical sender's existing liveness
// check, after it has classified the frame and locked its send direction.
package connect

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/connect/v2026/protocol"
)

type streamForwardingSealBarrierContext struct {
	context.Context
	calls   atomic.Int32
	entered chan struct{}
	release <-chan struct{}
}

func (self *streamForwardingSealBarrierContext) Err() error {
	if self.calls.Add(1) == 2 {
		close(self.entered)
		<-self.release
	}
	return self.Context.Err()
}

func TestStreamForwardingReciprocalRetiresBeforePhysicalSendCommit(t *testing.T) {
	path := TransferPath{SourceId: NewId(), DestinationId: NewId(), StreamId: NewId()}
	primary := testStreamForwardingBindingEnrollment(t, path)
	primary.destination = true
	sequence := testStreamForwardingBindingSequence(t, primary, NewId())
	defer sequence.Cancel()
	if !sequence.bindStreamForwarding(primary) {
		t.Fatal("primary grant did not bind")
	}
	association := sequence.forwarding.association.Load()
	reciprocal := testStreamForwardingBindingEnrollment(t, path.Reverse())
	reciprocalContext, cancelNative := context.WithCancel(reciprocal.ctx)
	defer cancelNative()
	reciprocal.ctx = reciprocalContext
	if !association.bindReciprocal(reciprocal) {
		t.Fatal("reciprocal grant did not bind")
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	association.ctx = &streamForwardingSealBarrierContext{Context: association.ctx, entered: entered, release: release}
	packet := streamForwardingTestPack(association, []byte("synthetic return at retirement"), true)
	packet.Pack.SequenceId = reciprocal.grant.sequenceId.Bytes()
	packet.Pack.ContractId = reciprocal.grant.contractId.Bytes()
	frame := marshalStreamForwardingTestFrame(t, packet)
	result := make(chan bool, 1)
	go func() {
		wire, sent := association.seal(frame, 65536)
		if sent {
			MessagePoolReturn(wire)
		}
		result <- sent
	}()
	select {
	case <-entered:
	case <-result:
		t.Fatal("ordinary reciprocal seal did not reach its physical commit")
	}
	cancelNative()
	close(release)
	if <-result || association.send.sendCounter != 0 {
		t.Fatal("retired reciprocal frame received a physical send counter")
	}
	ack := &protocol.TransferFrame{TransferPath: association.sendPath.ToProtobuf(), Ack: &protocol.Ack{
		MessageId: NewId().Bytes(), SequenceId: primary.grant.sequenceId.Bytes(),
	}}
	wire, ok := association.seal(marshalStreamForwardingTestFrame(t, ack), 65536)
	if !ok || association.send.sendCounter != 1 {
		t.Fatal("reciprocal retirement affected primary native acknowledgement")
	}
	MessagePoolReturn(wire)
}
