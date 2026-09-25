package connect

import (
	"context"
	"time"
)

// The custom carrier drains the same ACK/ordinary lanes and limits as H1 WS,
// then encodes their ready payloads once through Framer's shared scratch API.
func writeH1FramedReadyBatch(ctx context.Context, writer *FramedMessageConn, send, prioritySend <-chan []byte, first []byte, firstPriority bool, timeout time.Duration, onSent func()) (open bool, err error) {
	var owned [platformWebSocketWriteBatchMaxMessages][]byte
	owned[0] = first
	count, byteCount, priorityCount := 1, len(first), 0
	if firstPriority {
		priorityCount = 1
	}
	open = true
	defer func() {
		for _, message := range owned[:count] {
			MessagePoolReturn(message)
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
	if err = writer.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return open, err
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
