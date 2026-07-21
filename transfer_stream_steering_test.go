package connect

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// TestSendSequenceContractStreamSteering pins the contract-path steering of
// the send writer — the client half of the 2026-07-20 same-network stream
// report. A reply send sequence is stream-bound (its destination comes from
// `source.Reverse()`, which keeps the stream id), but once a contract is set,
// `openContractMultiRouteWriter` follows the CONTRACT's destination mask:
//   - a contract stamped with the stream id keeps the writer on the stream;
//   - an unstamped contract steers the writer OFF the stream onto the direct
//     path.
//
// This is why the platform must stamp every contract of an actively
// streaming pair (server controller `newContract`): an unstamped reply
// contract both hid the stream from the contract stats on each end and moved
// the reply traffic off the p2p stream while the forward direction stayed on
// it.
func TestSendSequenceContractStreamSteering(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultClientSettings()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer client.Cancel()

	peerId := NewId()
	streamId := NewId()
	// the reply-sequence shape: the destination carries the stream id
	destination := TransferPath{DestinationId: peerId, StreamId: streamId}

	sendBufferSettings := DefaultSendBufferSettings()
	sendBuffer := NewSendBuffer(ctx, client, sendBufferSettings)
	seq := NewSendSequence(
		ctx,
		client,
		sendBuffer,
		destination,
		MultiHopId{},
		false,
		false,
		sequenceTlsRoleClient,
		false,
		sendBufferSettings,
	)

	// no contract yet: the writer follows the sequence destination (stream)
	seq.openContractMultiRouteWriter()
	AssertEqual(t, destination, seq.contractMultiRouteWriterDestination)

	newContract := func(withStream bool) *sequenceContract {
		stored := &protocol.StoredContract{
			ContractId:        NewId().Bytes(),
			TransferByteCount: uint64(1024 * 1024),
			SourceId:          client.ClientId().Bytes(),
			DestinationId:     peerId.Bytes(),
		}
		if withStream {
			stored.StreamId = streamId.Bytes()
		}
		storedBytes, err := proto.Marshal(stored)
		AssertEqual(t, err, nil)
		contract, err := newSequenceContract(
			client.log,
			"s",
			&protocol.Contract{StoredContractBytes: storedBytes},
			sendBufferSettings.MinMessageByteCount,
			1.0,
		)
		AssertEqual(t, err, nil)
		return contract
	}

	// an unstamped contract steers the writer OFF the stream (direct path).
	// With the platform stamping pair-stream contracts, this only happens
	// when the pair genuinely has no active stream
	seq.sendContract = newContract(false)
	seq.openContractMultiRouteWriter()
	AssertEqual(t, TransferPath{DestinationId: peerId}, seq.contractMultiRouteWriterDestination)

	// a stream-stamped contract keeps the writer on the stream mask, and the
	// stats registration derives its stream flag from the same contract path
	seq.sendContract = newContract(true)
	seq.openContractMultiRouteWriter()
	AssertEqual(t, TransferPath{DestinationId: peerId, StreamId: streamId}, seq.contractMultiRouteWriterDestination)
	AssertEqual(t, true, seq.sendContract.path.IsStream())
}
