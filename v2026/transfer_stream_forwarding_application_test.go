// Endpoint authentication keeps native frame identity, and replay belongs to
// the enrolled grant across raw transport replacements.
package connect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

// Test-only directional keys and complete grant. The endpoint enrollment tests
// independently exercise the verified platform exchange that yields these.
func streamForwardingApplicationPair(t *testing.T) (*streamForwardingAssociation, *streamForwardingAssociation) {
	t.Helper()
	grant := streamForwardingGrant{path: TransferPath{SourceId: NewId(), DestinationId: NewId(), StreamId: NewId()},
		sequenceId: NewId(), contractId: NewId(), introductionId: NewId(),
		contractDigest: sha256.Sum256([]byte("existing signed work")), expirationTimeUnixMillis: time.Now().Add(time.Hour).UnixMilli(),
	}
	return streamForwardingApplicationPairForGrant(t, grant)
}

// Uses already authenticated synthetic keys on the supplied exact grant.
func streamForwardingApplicationPairForGrant(t *testing.T, grant streamForwardingGrant) (*streamForwardingAssociation, *streamForwardingAssociation) {
	t.Helper()
	keys := streamForwardingKeys{transcript: [32]byte{1}, forwardReadiness: [32]byte{2}, reverseReadiness: [32]byte{3},
		forwardApplication: [32]byte{4}, reverseApplication: [32]byte{5},
	}
	makeEndpoint := func(destination bool) *streamForwardingAssociation {
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		association, ok := newStreamForwardingAssociation(ctx, grant, keys, destination, newP2pStreamReadiness(ctx))
		if !ok || !association.installReadiness() {
			t.Fatal("valid synthetic grant did not bind its endpoint")
		}
		return association
	}
	return makeEndpoint(false), makeEndpoint(true)
}

// Builds normal Transfer bytes, retaining the native message/sequence fields.
func marshalStreamForwardingTestFrame(t *testing.T, msg *protocol.TransferFrame) []byte {
	t.Helper()
	frame, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

// An ordinary acknowledged or NoAck data frame for this grant.
func streamForwardingTestPack(origin *streamForwardingAssociation, payload []byte, noAck bool) *protocol.TransferFrame {
	pack := &protocol.Pack{MessageId: NewId().Bytes(), SequenceId: origin.grant.sequenceId.Bytes(),
		Nack: noAck, ForceStream: true,
		Frames: []*protocol.Frame{{MessageType: protocol.MessageType_TestSimpleMessage, MessageBytes: payload, Raw: true}},
	}
	if noAck {
		pack.ContractId = origin.grant.contractId.Bytes()
	}
	return &protocol.TransferFrame{TransferPath: origin.sendPath.ToProtobuf(), Pack: pack}
}

// Proves useful bytes and Native Ack identity survive, a wire duplicate cannot
// be admitted twice, and a reliable retry still reaches native deduplication.
func TestStreamForwardingApplicationNativeFramesAndReplay(t *testing.T) {
	origin, destination := streamForwardingApplicationPair(t)
	const limit = 64 * 1024
	for _, noAck := range []bool{false, true} {
		packet := streamForwardingTestPack(origin, []byte("sample application"), noAck)
		frame := marshalStreamForwardingTestFrame(t, packet)
		first, ok := origin.seal(frame, limit)
		if !ok {
			t.Fatal("origin refused the ordinary data frame")
		}
		defer MessagePoolReturn(first)
		second, ok := origin.seal(frame, limit)
		if !ok {
			t.Fatal("origin refused the same native identity on retry")
		}
		defer MessagePoolReturn(second)
		if bytes.Equal(first, second) || len(first) != len(frame)+streamForwardingTrailerByteCount {
			t.Fatal("physical retry reused its counter or changed the fixed overhead")
		}
		for _, wire := range [][]byte{second, first} {
			opened, class, valid := destination.open(wire, limit)
			if !valid || class != streamForwardingPack || !bytes.Equal(opened, frame) ||
				&opened[0] != &wire[0] || cap(opened) != cap(wire) {
				t.Fatal("normal out-of-order data did not preserve its exact owned Transfer root and native bytes")
			}
		}
		before := destination.receive.replayWindow
		if _, _, valid := destination.open(first, limit); valid || destination.receive.replayWindow != before {
			t.Fatal("replayed delivery changed the grant's receive window")
		}
		if noAck {
			continue
		}
		ack := &protocol.TransferFrame{TransferPath: destination.sendPath.ToProtobuf(),
			Ack: &protocol.Ack{MessageId: packet.Pack.MessageId, SequenceId: packet.Pack.SequenceId}}
		originalAck := marshalStreamForwardingTestFrame(t, ack)
		wire, ok := destination.seal(originalAck, limit)
		if !ok {
			t.Fatal("destination refused its matching native delivery acknowledgement")
		}
		defer MessagePoolReturn(wire)
		opened, class, ok := origin.open(wire, limit)
		if !ok || class != streamForwardingAck || !bytes.Equal(opened, originalAck) {
			t.Fatal("origin did not receive the same native acknowledgement under the reverse key")
		}
	}
}

// Auth failure must leave existing enrollment and replay state available for
// the original valid delivery. Control traffic belongs to its ordinary path.
func TestStreamForwardingApplicationScopeAndCustody(t *testing.T) {
	origin, destination := streamForwardingApplicationPair(t)
	const limit = 65536
	frame := streamForwardingTestPack(origin, []byte("original"), false)
	wire, ok := origin.seal(marshalStreamForwardingTestFrame(t, frame), limit)
	if !ok {
		t.Fatal("missing authorized wire")
	}
	defer MessagePoolReturn(wire)
	before := destination.receive.replayWindow
	for _, offset := range []int{1, len(wire) - streamForwardingTrailerByteCount + streamForwardingClassOffset, len(wire) - 1} {
		wire[offset] ^= 1
		if _, _, accepted := destination.open(wire, limit); accepted || destination.receive.replayWindow != before {
			t.Fatal("changed wire advanced the receive grant")
		}
		wire[offset] ^= 1
	}
	counterBytes := wire[len(wire)-streamForwardingTrailerByteCount+streamForwardingCounterOffset:]
	counter := binary.BigEndian.Uint64(counterBytes)
	binary.BigEndian.PutUint64(counterBytes, ^uint64(0))
	if _, _, accepted := destination.open(wire, limit); accepted || destination.receive.replayWindow != before {
		t.Fatal("unchecked counter displaced active receive state")
	}
	binary.BigEndian.PutUint64(counterBytes, counter)
	if _, _, accepted := origin.open(wire, limit); accepted {
		t.Fatal("a forward frame matched the reverse key")
	}
	if _, _, accepted := destination.open(wire, limit); !accepted {
		t.Fatal("rejected observations prevented normal delivery")
	}
	for _, alter := range []func(*protocol.TransferFrame){
		func(p *protocol.TransferFrame) { p.TransferPath.SourceId = NewId().Bytes() },
		func(p *protocol.TransferFrame) { p.TransferPath.DestinationId = NewId().Bytes() },
		func(p *protocol.TransferFrame) { p.Pack.SequenceId = NewId().Bytes() },
		func(p *protocol.TransferFrame) { p.Pack.ContractId = NewId().Bytes() },
		func(p *protocol.TransferFrame) { p.Pack.ContractAhead = true },
		func(p *protocol.TransferFrame) {
			p.Pack.Frames = append(p.Pack.Frames, &protocol.Frame{MessageType: protocol.MessageType_TransferStreamOpen})
		},
		func(p *protocol.TransferFrame) { p.Pack.Frames = nil },
		func(p *protocol.TransferFrame) {
			p.Ack = &protocol.Ack{SequenceId: p.Pack.SequenceId, MessageId: p.Pack.MessageId}
		},
	} {
		invalid := proto.Clone(frame).(*protocol.TransferFrame)
		alter(invalid)
		if rejected, accepted := origin.seal(marshalStreamForwardingTestFrame(t, invalid), limit); accepted {
			MessagePoolReturn(rejected)
			t.Error("unrelated grant, endpoints, body or control entered the forwarding carrier")
		}
	}
}

// Exact maximum inner size depends on the existing physical ceiling and its
// negotiated delay header. A refusal consumes neither a counter nor a root.
func TestStreamForwardingApplicationPhysicalCeilings(t *testing.T) {
	origin, destination := streamForwardingApplicationPair(t)
	for _, physicalLimit := range []int{1500, 64 * 1024, 1188 * 64} {
		for _, delay := range []int{0, transportDelayHeaderByteCount} {
			limit := physicalLimit - delay
			target := limit - streamForwardingTrailerByteCount
			template := streamForwardingTestPack(origin, nil, false)
			payloadSize := target - 96
			var frame []byte
			for range 5 {
				template.Pack.Frames[0].MessageBytes = make([]byte, payloadSize)
				frame = marshalStreamForwardingTestFrame(t, template)
				if len(frame) == target {
					break
				}
				payloadSize += target - len(frame)
			}
			if len(frame) != target {
				t.Fatalf("could not construct exact inner byte count %d", target)
			}
			wire, ok := origin.seal(frame, limit)
			if !ok || len(wire)+delay != physicalLimit {
				t.Fatalf("physical limit %d delay %d refused its exact boundary", physicalLimit, delay)
			}
			if _, _, accepted := destination.open(wire, limit-1); accepted {
				t.Fatal("receiver accepted oversized physical data")
			}
			if _, _, accepted := destination.open(wire, limit); !accepted {
				t.Fatal("exact receiver ceiling rejected current wire")
			}
			MessagePoolReturn(wire)
			before := origin.send.sendCounter
			if rejected, accepted := origin.seal(frame, limit-1); accepted || before != origin.send.sendCounter {
				MessagePoolReturn(rejected)
				t.Fatal("oversize frame consumed send ownership or sequence")
			}
		}
	}
}
