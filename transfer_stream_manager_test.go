package connect

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

func TestStreamManagerStreamLifecycle(t *testing.T) {
	// stream control frames from the platform drive the stream buffer:
	// open with destination only (this client is the stream source),
	// open with source only (this client is the stream destination),
	// open with both (this client is an intermediary hop),
	// duplicate opens are idempotent,
	// reopening a stream id with different endpoints evicts the old sequence,
	// close cancels the stream,
	// and reset closes all open streams and opens the listed set

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := NewClient(ctx, NewId(), NewNoContractClientOob(), DefaultClientSettings())
	defer client.Close()

	streamManager := client.streamManager

	mustFrame := func(message proto.Message) *protocol.Frame {
		frame, err := ToFrame(message, DefaultProtocolVersion)
		AssertEqual(t, err, nil)
		return frame
	}

	receiveControl := func(message proto.Message) {
		streamManager.Receive(SourceId(ControlId), []*protocol.Frame{mustFrame(message)}, Peer{})
	}

	streamOpen := func(sourceId *Id, destinationId *Id, streamId Id) *protocol.StreamOpen {
		streamOpen := &protocol.StreamOpen{
			StreamId: streamId.Bytes(),
		}
		if sourceId != nil {
			streamOpen.SourceId = sourceId.Bytes()
		}
		if destinationId != nil {
			streamOpen.DestinationId = destinationId.Bytes()
		}
		return streamOpen
	}

	getSequence := func(streamId Id) *StreamSequence {
		streamBuffer := streamManager.streamBuffer
		streamBuffer.mutex.Lock()
		defer streamBuffer.mutex.Unlock()
		return streamBuffer.streamSequencesByStreamId[streamId]
	}

	hasStreamSequenceId := func(sourceId *Id, destinationId *Id, streamId Id) bool {
		streamBuffer := streamManager.streamBuffer
		streamBuffer.mutex.Lock()
		defer streamBuffer.mutex.Unlock()
		_, ok := streamBuffer.streamSequences[newStreamSequenceId(sourceId, destinationId, streamId)]
		return ok
	}

	eventually := func(c func() bool) bool {
		endTime := time.Now().Add(5 * time.Second)
		for time.Now().Before(endTime) {
			if c() {
				return true
			}
			select {
			case <-ctx.Done():
				return c()
			case <-time.After(10 * time.Millisecond):
			}
		}
		return c()
	}

	// open with destination only: this client is the stream source
	destinationId := NewId()
	endpointStreamId := NewId()
	receiveControl(streamOpen(nil, &destinationId, endpointStreamId))
	AssertEqual(t, true, streamManager.IsStreamOpen(endpointStreamId))

	// open with source only: this client is the stream destination
	sourceId := NewId()
	sourceStreamId := NewId()
	receiveControl(streamOpen(&sourceId, nil, sourceStreamId))
	AssertEqual(t, true, streamManager.IsStreamOpen(sourceStreamId))

	// open with both: this client is an intermediary hop
	intermediaryStreamId := NewId()
	receiveControl(streamOpen(&sourceId, &destinationId, intermediaryStreamId))
	AssertEqual(t, true, streamManager.IsStreamOpen(intermediaryStreamId))

	// a duplicate open leaves the existing sequence in place
	sequence := getSequence(endpointStreamId)
	AssertEqual(t, true, sequence != nil)
	receiveControl(streamOpen(nil, &destinationId, endpointStreamId))
	AssertEqual(t, true, sequence == getSequence(endpointStreamId))

	// reopening the stream id with different endpoints cancels the old sequence
	otherDestinationId := NewId()
	receiveControl(streamOpen(nil, &otherDestinationId, endpointStreamId))
	AssertEqual(t, true, streamManager.IsStreamOpen(endpointStreamId))
	evictedSequence := sequence
	sequence = getSequence(endpointStreamId)
	AssertEqual(t, true, sequence != nil)
	AssertEqual(t, true, evictedSequence != sequence)
	AssertEqual(t, true, evictedSequence.ctx.Err() != nil)
	// the evicted sequence is asynchronously cleaned up
	AssertEqual(t, true, eventually(func() bool {
		return !hasStreamSequenceId(nil, &destinationId, endpointStreamId)
	}))

	// close cancels the stream
	receiveControl(&protocol.StreamClose{
		StreamId: intermediaryStreamId.Bytes(),
	})
	AssertEqual(t, true, eventually(func() bool {
		return !streamManager.IsStreamOpen(intermediaryStreamId)
	}))

	// reset closes all open streams and opens the listed set.
	// relist the endpoint stream and add a new stream;
	// the source stream is not listed and closes
	resetStreamId := NewId()
	receiveControl(&protocol.StreamReset{
		Streams: []*protocol.StreamOpen{
			streamOpen(nil, &otherDestinationId, endpointStreamId),
			streamOpen(nil, &destinationId, resetStreamId),
		},
	})
	AssertEqual(t, true, streamManager.IsStreamOpen(endpointStreamId))
	AssertEqual(t, true, streamManager.IsStreamOpen(resetStreamId))
	AssertEqual(t, true, eventually(func() bool {
		return !streamManager.IsStreamOpen(sourceStreamId)
	}))
	// the relisted stream reopens with a new sequence since reset cancels all
	AssertEqual(t, true, sequence != getSequence(endpointStreamId))
	AssertEqual(t, true, sequence.ctx.Err() != nil)
}
