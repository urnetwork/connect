package connect

import (
	"context"
	"time"
)

// The custom carrier drains the same ACK/ordinary lanes and limits as H1 WS,
// then encodes their ready payloads once through Framer's shared scratch API.
// The batch installs a write deadline of timeout for itself alone; the
// platform writer shares one lazily re-armed deadline across all its writes
// through writeH1FramedReadyBatchWithDeadline.
func writeH1FramedReadyBatch(ctx context.Context, writer *FramedMessageConn, send, prioritySend <-chan []byte, first []byte, firstPriority bool, timeout time.Duration, onSent func(), retained ...*platformH1RetainedBudget) (open bool, err error) {
	writeDeadline := newLazyDeadline(timeout, 0)
	return writeH1FramedReadyBatchWithDeadline(ctx, writer, send, prioritySend, first, firstPriority, &writeDeadline, onSent, retained...)
}

// writeH1FramedReadyBatch with the connection's write deadline, which the batch
// re-arms only when less than its timeout is left (lazyDeadline).
func writeH1FramedReadyBatchWithDeadline(ctx context.Context, writer *FramedMessageConn, send, prioritySend <-chan []byte, first []byte, firstPriority bool, writeDeadline *lazyDeadline, onSent func(), retained ...*platformH1RetainedBudget) (open bool, err error) {
	var owned [platformWebSocketWriteBatchMaxMessages][]byte
	var priorityOwned [platformWebSocketWriteBatchMaxMessages]bool
	owned[0] = first
	priorityOwned[0] = firstPriority
	count, byteCount, priorityCount := 1, len(first), 0
	if firstPriority {
		priorityCount = 1
	}
	open = true
	defer func() {
		for i, message := range owned[:count] {
			var lane *platformH1RetainedLane
			if len(retained) != 0 && retained[0] != nil {
				lane = &retained[0].send
				if priorityOwned[i] {
					lane = &retained[0].priority
				}
			}
			returnH1RetainedMessage(message, lane)
		}
	}()
	for platformWebSocketWriteBatchCanDrain(count, byteCount) {
		select {
		case <-ctx.Done():
			return false, nil
		default:
		}
		message, priority, nextOpen, ready := platformWebSocketWriteBatchNextReady(prioritySend, send, priorityCount)
		if !ready {
			break
		}
		if !nextOpen {
			open = false
			break
		}
		owned[count] = message
		priorityOwned[count] = priority
		count++
		byteCount += len(message)
		if priority {
			priorityCount++
		} else {
			priorityCount = 0
		}
	}
	select {
	case <-ctx.Done():
		return false, nil
	default:
	}
	var payloads [platformWebSocketWriteBatchMaxMessages][]byte
	n := 0
	for _, message := range owned[:count] {
		if len(message) > 16 {
			payloads[n] = message
			n++
		}
	}
	if deadline, rearm := writeDeadline.rearm(time.Now()); rearm {
		if err = writer.SetWriteDeadline(deadline); err != nil {
			return open, err
		}
	}
	if err = writer.WriteMessages(payloads[:n]); err != nil {
		return open, err
	}
	if onSent != nil {
		for range n {
			onSent()
		}
	}
	return open, nil
}
