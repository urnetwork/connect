package connect

import (
	"errors"
	"fmt"
)

// This failure-only, compile-gated diagnostic joins an exact downstream
// refusal to its Transfer item. It never retains packet bytes or addresses,
// changes an admission result, or allocates/reads clocks with a nil observer.
// The bounded caller-owned recorder remains diagnostic, not PERF evidence.
func (owner *providerReliablePacket) traceFinalRejection(tcp *parsedTcp, err error) {
	if !ackLineageTraceEnabled {
		return
	}
	receipt := owner.peer.deliveryReceipt()
	if receipt == nil || receipt.queue == nil || receipt.queue.sequence == nil {
		return
	}
	sequence := receipt.queue.sequence
	observer := sequence.receiveBufferSettings.ProgressObserver
	if observer == nil {
		return
	}
	packet := owner.packet
	if len(packet) == 0 {
		return
	}
	version := packet[0] >> 4
	var transport []byte
	var valid bool
	if version == 4 {
		_, _, _, transport, valid = parseIpv4(packet)
	} else if version == 6 {
		_, _, _, transport, valid = parseIpv6(packet)
	}
	if !valid || len(transport) < TcpHeaderSizeWithoutExtensions {
		return
	}
	errorKind := transferProgressErrorKind(err)
	if errors.Is(err, errReliableIngressNoFlow) {
		errorKind = "no_flow"
	}
	beginTransferProgress(observer, TransferProgressEvent{
		Stage: "provider_tcp_rejected", ClientId: sequence.client.ClientId(), PeerId: sequence.source.SourceId,
		SequenceId: sequence.sequenceId, MessageId: receipt.ack.messageId, SequenceNumber: receipt.ack.sequenceNumber,
		ErrorKind: errorKind, Outcome: fmt.Sprintf("ip=%d flags=%02x payload=%d", version, transport[13], len(tcp.payload)),
	}, packet)
}
