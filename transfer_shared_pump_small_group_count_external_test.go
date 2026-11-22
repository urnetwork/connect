// Public consumer callbacks decode protocol frames outside the parent package.
package connect_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// Borrowed packet views remain inside the synchronous public callback. The
// private fixture receives scalar validity and never imports its child package.
func installSmallGroupCountReceiver(client *connect.Client, receive func(connect.Peer, [][]byte, bool)) {
	client.AddReceiveCallback(func(_ connect.TransferPath, frames []*protocol.Frame, peer connect.Peer) {
		packets := make([][]byte, len(frames))
		valid := true
		for index, frame := range frames {
			if frame == nil {
				valid = false
				continue
			}
			valid = valid && frame.Raw && frame.MessageType == protocol.MessageType_IpIpPacketFromProvider
			packets[index] = frame.MessageBytes
		}
		receive(peer, packets, valid)
	})
}

// Submit one reverse application TCP ACK and report each exact peer delivery.
func sendSmallGroupReverseTcpAck(from, to *connect.Client, packet []byte, delivered func(), ack connect.AckFunction) bool {
	to.AddReceiveCallback(func(source connect.TransferPath, frames []*protocol.Frame, peer connect.Peer) {
		if source.SourceId == from.ClientId() && peer.TransferKey.LogicalLane == 2 {
			for _, frame := range frames {
				if frame.GetMessageType() == protocol.MessageType_IpIpPacketToProvider && frame.GetRaw() && bytes.Equal(frame.GetMessageBytes(), packet) {
					delivered()
				}
			}
		}
	})
	frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: connect.MessagePoolCopy(packet)}
	if from.SendMultiWithTimeout([]*protocol.Frame{frame}, to.ClientId(), ack, time.Second, connect.TransferKey{LogicalLane: 2}) {
		return true
	}
	connect.MessagePoolReturn(frame.MessageBytes)
	return false
}

// A full 32 MiB H1 queue preserves reverse control and natural DATA recovery.
func TestSharedPumpSmallH1Groups32FixedCountRefusesAndRecovers(t *testing.T) {
	connect.RunSmallGroupFixedCountFixture(t, installSmallGroupCountReceiver, sendSmallGroupReverseTcpAck)
}

// A paused 32 MiB consumer uses its one configured finite growth step.
func TestSharedPumpSmallH1Groups32ConfiguredAdaptiveGrowth(t *testing.T) {
	connect.RunSmallGroupAdaptiveCountFixture(t, 32, true, installSmallGroupCountReceiver)
}

// A paused 64 MiB consumer keeps the same paid-owner admission boundary.
func TestSharedPumpSmallH1Groups64ConfiguredAdaptiveGrowth(t *testing.T) {
	connect.RunSmallGroupAdaptiveCountFixture(t, 64, true, installSmallGroupCountReceiver)
}

// An unpaused 32 MiB consumer drains without using optional growth.
func TestSharedPumpSmallH1Groups32AdaptiveUnpausedControl(t *testing.T) {
	connect.RunSmallGroupAdaptiveCountFixture(t, 32, false, installSmallGroupCountReceiver)
}

// An unpaused 64 MiB consumer preserves exact receipt and packet ownership.
func TestSharedPumpSmallH1Groups64AdaptiveUnpausedControl(t *testing.T) {
	connect.RunSmallGroupAdaptiveCountFixture(t, 64, false, installSmallGroupCountReceiver)
}
