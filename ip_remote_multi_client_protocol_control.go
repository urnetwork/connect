package connect

import "github.com/urnetwork/connect/protocol"

// The IP producer chooses trusted local policy once for the complete offered
// group. Transfer ACK/reliability, route, context and ownership options retain
// their ordinary values; a control sibling never exempts an application frame.
func multiClientProtocolTransferOpts(opts []any, frames []*protocol.Frame) []any {
	if len(frames) != 0 && protocolOnlyFrames(frames) {
		opts = append(opts, NoBudget())
		if immediateIpAckFrames(frames) {
			opts = append(opts, DeliverImmediately())
		}
		return opts
	}
	return opts
}
