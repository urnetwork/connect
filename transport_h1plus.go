package connect

import (
	"context"
	"time"
)

// The custom carrier drains the same ACK/ordinary lanes and limits as H1 WS,
// then encodes their ready payloads once through Framer's shared scratch API.
// The batch installs a write deadline of timeout for itself alone.
func writeH1FramedReadyBatch(ctx context.Context, writer *FramedMessageConn, send, prioritySend <-chan []byte, first []byte, firstPriority bool, timeout time.Duration, onSent func(), retained ...*platformH1RetainedBudget) (open bool, err error) {
	return writeH1FramedReadyBatchWatched(ctx, writer, send, prioritySend, first, firstPriority, timeout, nil, onSent, retained...)
}

// writeH1FramedReadyBatch, with the platform writer's write watchdog in place
// of the deadline when it is not nil. The watchdog bounds the batch's write to
// its timeout from where the deadline would have been installed, and the
// socket write deadline stays as it is.
func writeH1FramedReadyBatchWatched(ctx context.Context, writer *FramedMessageConn, send, prioritySend <-chan []byte, first []byte, firstPriority bool, timeout time.Duration, writeWatchdog *ioWatchdog, onSent func(), retained ...*platformH1RetainedBudget) (open bool, err error) {
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
	if writeWatchdog != nil {
		writeWatchdog.begin(time.Now())
		err = writer.WriteMessages(payloads[:n])
		writeWatchdog.end()
	} else if err = writer.SetWriteDeadline(time.Now().Add(timeout)); err == nil {
		err = writer.WriteMessages(payloads[:n])
	}
	if err != nil {
		return open, err
	}
	if onSent != nil {
		for range n {
			onSent()
		}
	}
	return open, nil
}
