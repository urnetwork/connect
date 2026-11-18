package connect

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/v2026/protocol"
)

// This is the pre-serialization disposal boundary, not a delivery or wire
// authority fixture. It uses owned pooled roots and the actual tracked raw
// descriptor; public signed delivery/ACK coverage lives in the pressure root.
func TestProtocolRawGroupDoesNotInheritDataSuffixDisposal(t *testing.T) {
	assertMessagePoolOwnership(t)
	data := MessagePoolCopy(protocolClassTestPacket(false, 0x10, 0, 1))
	charge := MessagePoolPacketRootByteCount(data)
	budget := NewTransferMemoryBudget(charge)
	var retained transferItem
	if charge <= 0 || !retained.reserveMemory(budget, charge) {
		MessagePoolReturn(data)
		t.Fatal("actual pooled DATA owner did not saturate its retained leaf")
	}
	t.Cleanup(func() {
		retained.releaseMemory()
		MessagePoolReturn(data)
		assertRetainedBudgetBalance(t, budget)
	})
	control := MessagePoolCopy(protocolClassTestPacket(false, 0x10, 12, 0))
	t.Cleanup(func() { MessagePoolReturn(control) })
	group := &parsedPacketGroup{}
	group.completionFlags.Store(groupCompletionTracked)
	pack := &SendPack{Frames: []*protocol.Frame{
		{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: control},
		{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: control},
	}, logicalGroup: true, ackTarget: group}
	sequence := &SendSequence{resendQueue: newResendQueue(budget, 0), sendBufferSettings: DefaultSendBufferSettings()}
	sequence.resendQueue.lifetimeBudget = true
	before := budget.Stats()
	flags := group.completionFlags.Load()
	if !sequence.unfundedRawGroup(pack) {
		t.Fatal("ordinary raw suffix did not retain its DATA capacity failure")
	}
	client := &Client{ctx: context.Background(), settings: DefaultClientSettings()}
	pack.TransferOptions = client.resolveSendOptions(multiClientProtocolTransferOpts(nil, pack.Frames)).transferOptions
	if !pack.NoBudget || sequence.unfundedRawGroup(pack) {
		t.Fatal("all-control raw suffix inherited DATA memory disposal")
	}
	if budget.Stats() != before || group.completionFlags.Load() != flags || pack.groupFrameIndex != 0 || pack.serialized || len(pack.Frames) != 2 || pack.Frames[0].MessageBytes == nil {
		t.Fatal("disposal classification changed owner, range or completion custody")
	}
	// A DATA sibling prevents the IP producer from selecting the exemption.
	pack.Frames[1] = &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: data}
	pack.TransferOptions = client.resolveSendOptions(multiClientProtocolTransferOpts(nil, pack.Frames)).transferOptions
	if pack.NoBudget || !sequence.unfundedRawGroup(pack) || budget.Stats() != before {
		t.Fatal("mixed raw suffix bypassed its ordinary DATA refusal")
	}
}
