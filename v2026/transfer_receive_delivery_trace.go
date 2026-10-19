package connect

// Caller holds the receipt ledger lock. Every call site is compile-gated;
// ordinary builds keep the original admission/ACK path without an observer
// call, clock read or allocation. The opt-in observer must not block or enter
// this ledger. Scalar identities distinguish final admission from cumulative
// progress: a secured later receipt can still wait behind an earlier owner.
func (q *receiveDeliveryQueue) traceReceiptLocked(r *receiveDeliveryReceipt, stage string, success bool) {
	q.traceReceiptStateLocked(r, stage, success, "")
}

func (q *receiveDeliveryQueue) traceReceiptStateLocked(r *receiveDeliveryReceipt, stage string, success bool, outcome string) {
	if !ackLineageTraceEnabled || q.sequence == nil || q.sequence.receiveBufferSettings == nil ||
		q.sequence.client == nil || r == nil {
		return
	}
	sequence := q.sequence
	observer := sequence.receiveBufferSettings.ProgressObserver
	if observer == nil {
		return
	}
	beginTransferProgress(observer, TransferProgressEvent{
		Stage: stage, ClientId: sequence.client.ClientId(), PeerId: sequence.source.SourceId,
		SequenceId: sequence.sequenceId, MessageId: r.ack.messageId, SequenceNumber: r.ack.sequenceNumber,
		TransportType: r.ack.transportType, ByteCount: int(r.messageBytes),
		QueueLength: len(q.items), QueueCapacity: q.maxCount, Success: success, Outcome: outcome,
	}, nil)
}
