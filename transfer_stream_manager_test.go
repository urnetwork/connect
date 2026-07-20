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

	// reset reconciles: relisted streams keep their sequences,
	// unlisted streams close, and newly listed streams open
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
	// the relisted stream keeps its live sequence across the reset,
	// so its p2p transports survive a resident migration
	AssertEqual(t, true, sequence == getSequence(endpointStreamId))
	AssertEqual(t, true, sequence.ctx.Err() == nil)

	// an empty reset cancels everything (the legacy reset behavior)
	receiveControl(&protocol.StreamReset{})
	AssertEqual(t, true, eventually(func() bool {
		return !streamManager.IsStreamOpen(endpointStreamId) && !streamManager.IsStreamOpen(resetStreamId)
	}))
	AssertEqual(t, true, sequence.ctx.Err() != nil)
}

// TestStreamManagerResetSkipsBadEntries: a reset whose stream list contains
// un-openable entries (malformed ids) must still open every valid listed
// stream. Previously the re-open loop aborted on the first failed open,
// stranding the later listed streams until the next reset.
func TestStreamManagerResetSkipsBadEntries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := NewClient(ctx, NewId(), NewNoContractClientOob(), DefaultClientSettings())
	defer client.Close()

	streamManager := client.streamManager

	destinationId := NewId()
	streamIdA := NewId()
	streamIdB := NewId()

	valid := func(streamId Id) *protocol.StreamOpen {
		return &protocol.StreamOpen{
			StreamId:      streamId.Bytes(),
			DestinationId: destinationId.Bytes(),
		}
	}

	frame, err := ToFrame(&protocol.StreamReset{
		Streams: []*protocol.StreamOpen{
			// malformed: a truncated stream id fails IdFromBytes
			{
				StreamId:      []byte{0x01, 0x02, 0x03},
				DestinationId: destinationId.Bytes(),
			},
			valid(streamIdA),
			// malformed: a truncated source id fails IdFromBytes
			{
				StreamId: NewId().Bytes(),
				SourceId: []byte{0x04},
			},
			valid(streamIdB),
		},
	}, DefaultProtocolVersion)
	AssertEqual(t, err, nil)
	streamManager.Receive(SourceId(ControlId), []*protocol.Frame{frame}, Peer{})

	// both valid streams opened despite the malformed entries around them
	AssertEqual(t, true, streamManager.IsStreamOpen(streamIdA))
	AssertEqual(t, true, streamManager.IsStreamOpen(streamIdB))
}
