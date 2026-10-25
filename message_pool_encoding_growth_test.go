package connect

import (
	"bytes"
	"errors"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/runtime/protoiface"

	"github.com/urnetwork/connect/protocol"
)

// These roots use the established exact-root child helper: unrelated async
// packet returns cannot change the process-wide ownership baseline.
type encodingPoolBoundary struct {
	roots    uint64
	bytes    ByteCount
	taken    uint64
	returned uint64
}

func encodingPoolBoundaryNow() encodingPoolBoundary {
	roots, classBytes := messagePoolOutstandingSnapshot()
	taken, returned, _ := MessagePoolCounts()
	return encodingPoolBoundary{roots: roots, bytes: classBytes, taken: taken, returned: returned}
}

func encodingPoolRequireBalanced(t *testing.T, before encodingPoolBoundary) {
	t.Helper()
	after := encodingPoolBoundaryNow()
	if after.roots != before.roots || after.bytes != before.bytes || after.taken-before.taken != after.returned-before.returned {
		t.Fatalf("encoding ownership did not balance: before=%+v after=%+v", before, after)
	}
}

func encodingPoolAlignSmallShard(t *testing.T, index uint64) {
	t.Helper()
	pool := orderedMessagePools()[0]
	for range messagePoolShardCount {
		if pool.nextShard.Load()&messagePoolShardMask == index {
			return
		}
		if !MessagePoolReturn(MessagePoolGet(1)) {
			t.Fatal("balanced shard alignment did not return its root")
		}
	}
	t.Fatal("small pool did not reach the requested acquisition shard")
}

type encodingGrowthCodec struct {
	estimate    int
	err         error
	replace     bool
	shorten     bool
	nilOutput   bool
	destination []byte
}

func (self *encodingGrowthCodec) Size(payload []byte) int { return self.estimate }

func (self *encodingGrowthCodec) MarshalAppend(b []byte, payload []byte) ([]byte, error) {
	self.destination = b
	if self.nilOutput {
		return nil, self.err
	}
	if self.replace {
		next := MessagePoolGet(len(b) + len(payload))
		copy(next, b)
		copy(next[len(b):], payload)
		b = next
	} else {
		b = append(b, payload...)
	}
	if self.shorten {
		b = b[:0]
	}
	return b, self.err
}

func (self *encodingGrowthCodec) Unmarshal(encoded []byte, payload []byte) error { return nil }

func encodingPoolCheckSubprotocol(t *testing.T, encoded, payload []byte) {
	t.Helper()
	id, actual, ok := decodeSubprotocolHeader(encoded)
	if !ok || id != 2048 || !bytes.Equal(actual, payload) {
		t.Fatalf("encoded wrapper changed: ok=%v id=%d actual=%x want=%x", ok, id, actual, payload)
	}
}

// A supported Size mismatch writes eight extra payload bytes across the old
// 256-byte scratch root's id. Both returns succeed and aggregate counts agree,
// yet the old per-shard positive reconciliation reports two roots/512 bytes.
// No test writes metadata or resets/replaces the process's pool or counters.
func TestSubprotocolEncodingGrowthKeepsShardAccounting(t *testing.T) {
	if messagePoolSnapshotInFreshProcess(t) {
		return
	}
	encodingPoolAlignSmallShard(t, 1)
	before := encodingPoolBoundaryNow()
	for range 2 {
		payload := make([]byte, 258)
		codec := &encodingGrowthCodec{estimate: 250}
		encoded, overrun, err := marshalSubprotocol[[]byte](codec, 2048, payload)
		if err != nil || !overrun {
			t.Fatalf("growth premise failed: overrun=%v err=%v", overrun, err)
		}
		encodingPoolCheckSubprotocol(t, encoded, payload)
		if !MessagePoolReturn(encoded) {
			t.Fatal("rewrapped owner was not returned")
		}
	}
	after := encodingPoolBoundaryNow()
	if after.taken-before.taken != after.returned-before.returned {
		t.Fatalf("aggregate-return premise failed: before=%+v after=%+v", before, after)
	}
	if after.roots != before.roots || after.bytes != before.bytes {
		t.Fatalf("OLD_ENCODING_GROWTH_ACCOUNTING: estimate=250 actual=258 final_returns=2 aggregate_taken=%d aggregate_returned=%d before_roots=%d before_bytes=%d after_roots=%d after_bytes=%d", after.taken-before.taken, after.returned-before.returned, before.roots, before.bytes, after.roots, after.bytes)
	}
}

func TestSubprotocolEncodingGrowthPreservesMetadata(t *testing.T) {
	if messagePoolSnapshotInFreshProcess(t) {
		return
	}
	before := encodingPoolBoundaryNow()
	// The final ordinary payload byte would previously reach the scratch
	// root's flags byte. It must remain payload, never device-egress state.
	payload := make([]byte, 260)
	payload[len(payload)-1] = messagePoolFlagDeviceTunEgress
	encoded, overrun, err := marshalSubprotocol[[]byte](&encodingGrowthCodec{estimate: 250}, 2048, payload)
	if err != nil || !overrun {
		t.Fatalf("metadata boundary premise failed: overrun=%v err=%v", overrun, err)
	}
	encodingPoolCheckSubprotocol(t, encoded, payload)
	if !MessagePoolReturn(encoded) {
		t.Fatal("grown output did not retain its pool owner")
	}
	encodingPoolRequireBalanced(t, before)
}

func TestSubprotocolEncodingPooledOutputOwnership(t *testing.T) {
	if messagePoolSnapshotInFreshProcess(t) {
		return
	}
	for _, testCase := range []struct {
		estimate int
		actual   int
	}{
		{estimate: 0, actual: 0}, {estimate: 0, actual: 1},
		{estimate: 250, actual: 250}, {estimate: 250, actual: 249},
		{estimate: 250, actual: 251}, {estimate: 250, actual: 263},
		{estimate: 250, actual: 300}, {estimate: 300, actual: 1},
		{estimate: 2042, actual: 2042}, {estimate: 2042, actual: 2050},
	} {
		before := encodingPoolBoundaryNow()
		payload := bytes.Repeat([]byte{0x5a}, testCase.actual)
		codec := &encodingGrowthCodec{estimate: testCase.estimate}
		encoded, overrun, err := marshalSubprotocol[[]byte](codec, 2048, payload)
		if err != nil || overrun != (testCase.estimate != testCase.actual) {
			t.Fatalf("estimate=%d actual=%d: overrun=%v err=%v", testCase.estimate, testCase.actual, overrun, err)
		}
		encodingPoolCheckSubprotocol(t, encoded, payload)
		if testCase.estimate == testCase.actual && &encoded[0] != &codec.destination[0] {
			t.Fatal("honest encoder replaced the original destination")
		}
		if pooled, _ := MessagePoolCheck(encoded); !pooled || !MessagePoolReturn(encoded) {
			t.Fatalf("estimate=%d actual=%d: full pooled ownership was not restored", testCase.estimate, testCase.actual)
		}
		after := encodingPoolBoundaryNow()
		if testCase.estimate == testCase.actual && after.taken-before.taken != 1 {
			t.Fatalf("honest encoding took %d roots, want one", after.taken-before.taken)
		}
		encodingPoolRequireBalanced(t, before)
	}
}

func TestSubprotocolEncodingErrorReturnsAllOwnedBuffers(t *testing.T) {
	if messagePoolSnapshotInFreshProcess(t) {
		return
	}
	encodeErr := errors.New("encoding stopped after append")
	for _, replace := range []bool{false, true} {
		for _, size := range []int{1, 260} {
			input := MessagePoolGet(size)
			for index := range input {
				input[index] = 0x5a
			}
			before := encodingPoolBoundaryNow()
			codec := &encodingGrowthCodec{estimate: 250, err: encodeErr, replace: replace}
			encoded, overrun, err := marshalSubprotocol[[]byte](codec, 2048, input)
			if encoded != nil || overrun || !errors.Is(err, encodeErr) {
				t.Fatalf("error result changed: replace=%v size=%d out=%v overrun=%v err=%v", replace, size, encoded, overrun, err)
			}
			if pooled, _ := MessagePoolCheck(input); !pooled || !bytes.Equal(input, bytes.Repeat([]byte{0x5a}, size)) {
				t.Fatal("encoding error consumed or changed borrowed input")
			}
			encodingPoolRequireBalanced(t, before)
			if !MessagePoolReturn(input) {
				t.Fatal("caller lost borrowed input ownership")
			}
		}
	}
}

func TestSubprotocolEncodingMovedOutputWithEqualCapacity(t *testing.T) {
	if messagePoolSnapshotInFreshProcess(t) {
		return
	}
	before := encodingPoolBoundaryNow()
	payload := bytes.Repeat([]byte{0x5a}, 100)
	codec := &encodingGrowthCodec{estimate: len(payload), replace: true}
	encoded, overrun, err := marshalSubprotocol[[]byte](codec, 2048, payload)
	if err != nil || !overrun {
		t.Fatalf("replacement buffer was not recovered: overrun=%v err=%v", overrun, err)
	}
	encodingPoolCheckSubprotocol(t, encoded, payload)
	if !MessagePoolReturn(encoded) {
		t.Fatal("recovered output is not an owned pool root")
	}
	encodingPoolRequireBalanced(t, before)
}

func TestSubprotocolEncodingShortOutputReturnsOwners(t *testing.T) {
	if messagePoolSnapshotInFreshProcess(t) {
		return
	}
	for _, codec := range []*encodingGrowthCodec{
		{estimate: 1, nilOutput: true},
		{estimate: 1, shorten: true},
		{estimate: 1, replace: true, shorten: true},
	} {
		before := encodingPoolBoundaryNow()
		encoded, overrun, err := marshalSubprotocol[[]byte](codec, 2048, []byte{1})
		if encoded != nil || overrun || (err == nil || err.Error() != "subprotocol codec discarded the wrapper header") {
			t.Fatalf("discarded header was not rejected: encoded=%v overrun=%v err=%v", encoded, overrun, err)
		}
		encodingPoolRequireBalanced(t, before)
	}
}

// Test-only protobuf methods exercise Size/MarshalAppend's public extension
// boundary without mutating a message concurrently or adding a production hook.
type encodingGrowthProto struct {
	protoreflect.Message
	methods      protoiface.Methods
	sizeCalls    int
	marshalCalls int
	destination  []byte
}

func (self *encodingGrowthProto) ProtoReflect() protoreflect.Message   { return self }
func (self *encodingGrowthProto) Interface() protoreflect.ProtoMessage { return self }
func (self *encodingGrowthProto) ProtoMethods() *protoiface.Methods    { return &self.methods }

func newEncodingGrowthProto(estimate int, wire []byte, replace, nilOutput bool, encodeErr error) *encodingGrowthProto {
	m := &encodingGrowthProto{Message: (&protocol.IpPing{}).ProtoReflect()}
	m.methods = protoiface.Methods{
		Size: func(protoiface.SizeInput) protoiface.SizeOutput {
			m.sizeCalls += 1
			return protoiface.SizeOutput{Size: estimate}
		},
		Marshal: func(input protoiface.MarshalInput) (protoiface.MarshalOutput, error) {
			m.marshalCalls += 1
			m.destination = input.Buf
			if nilOutput {
				return protoiface.MarshalOutput{Buf: nil}, encodeErr
			}
			out := input.Buf
			if replace {
				out = MessagePoolGet(len(input.Buf) + len(wire))
				copy(out, input.Buf)
				copy(out[len(input.Buf):], wire)
			} else {
				out = append(out, wire...)
			}
			return protoiface.MarshalOutput{Buf: out}, encodeErr
		},
	}
	return m
}

func encodingGrowthWire(payloadSize int) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), make([]byte, payloadSize))
}

func encodingPoolRequireProtoHooks(t *testing.T, message *encodingGrowthProto) {
	t.Helper()
	if message.sizeCalls != 2 || message.marshalCalls != 1 {
		t.Fatalf("protobuf extension premise failed: Size calls=%d Marshal calls=%d", message.sizeCalls, message.marshalCalls)
	}
}

func TestProtoMarshalEncodingGrowthKeepsShardAccounting(t *testing.T) {
	if messagePoolSnapshotInFreshProcess(t) {
		return
	}
	encodingPoolAlignSmallShard(t, 1)
	before := encodingPoolBoundaryNow()
	for range 2 {
		wire := encodingGrowthWire(261) // valid 264-byte wire; estimate 256
		message := newEncodingGrowthProto(256, wire, false, false, nil)
		encoded, err := ProtoMarshalWithTag(message, 0)
		encodingPoolRequireProtoHooks(t, message)
		if err != nil || !bytes.Equal(encoded, wire) {
			t.Fatalf("protobuf growth premise failed: err=%v", err)
		}
		MessagePoolReturn(encoded)
	}
	after := encodingPoolBoundaryNow()
	if after.taken-before.taken != after.returned-before.returned {
		t.Fatalf("aggregate-return premise failed: before=%+v after=%+v", before, after)
	}
	if after.roots != before.roots || after.bytes != before.bytes {
		t.Fatalf("OLD_PROTO_ENCODING_GROWTH_ACCOUNTING: estimate=256 actual=264 aggregate_taken=%d aggregate_returned=%d before_roots=%d before_bytes=%d after_roots=%d after_bytes=%d", after.taken-before.taken, after.returned-before.returned, before.roots, before.bytes, after.roots, after.bytes)
	}
}

func TestProtoMarshalEncodingPooledAndEmptyOwnership(t *testing.T) {
	if messagePoolSnapshotInFreshProcess(t) {
		return
	}
	for _, testCase := range []struct {
		estimate int
		wire     []byte
	}{
		{estimate: 0, wire: nil},
		{estimate: 0, wire: []byte{8, 1}},
		{estimate: 100, wire: []byte{8, 1}},
		{estimate: 256, wire: encodingGrowthWire(253)},
		{estimate: 2048, wire: encodingGrowthWire(2045)},
	} {
		before := encodingPoolBoundaryNow()
		message := newEncodingGrowthProto(testCase.estimate, testCase.wire, false, false, nil)
		encoded, err := ProtoMarshalWithTag(message, 0)
		encodingPoolRequireProtoHooks(t, message)
		if err != nil || !bytes.Equal(encoded, testCase.wire) {
			t.Fatalf("protobuf result changed: estimate=%d err=%v", testCase.estimate, err)
		}
		if 0 == cap(encoded) || &encoded[:1][0] != &message.destination[:1][0] {
			t.Fatal("non-growing protobuf output replaced its destination")
		}
		if pooled, _ := MessagePoolCheck(encoded); !pooled || !MessagePoolReturn(encoded) {
			t.Fatal("protobuf output lost its full pooled view")
		}
		after := encodingPoolBoundaryNow()
		if after.taken-before.taken != 1 {
			t.Fatalf("non-growing protobuf encoding took %d roots, want one", after.taken-before.taken)
		}
		encodingPoolRequireBalanced(t, before)
	}
	before := encodingPoolBoundaryNow()
	encoded, err := ProtoMarshalWithTag(nil, 0)
	if err != nil || encoded != nil {
		t.Fatalf("nil message result changed: encoded=%v err=%v", encoded, err)
	}
	empty, err := ProtoMarshalWithTag(&protocol.IpPing{}, 0)
	if err != nil || len(empty) != 0 || !MessagePoolReturn(empty) {
		t.Fatalf("generated empty message lost ownership: err=%v", err)
	}
	encodingPoolRequireBalanced(t, before)
}

func TestProtoMarshalEncodingErrorReturnsAllOwnedBuffers(t *testing.T) {
	if messagePoolSnapshotInFreshProcess(t) {
		return
	}
	encodeErr := errors.New("protobuf encoding stopped after append")
	for _, replace := range []bool{false, true} {
		for _, payloadSize := range []int{1, 263} {
			wire := MessagePoolCopy(encodingGrowthWire(payloadSize))
			before := encodingPoolBoundaryNow()
			message := newEncodingGrowthProto(256, wire, replace, false, encodeErr)
			encoded, err := ProtoMarshalWithTag(message, 0)
			encodingPoolRequireProtoHooks(t, message)
			if encoded != nil || !errors.Is(err, encodeErr) {
				t.Fatalf("protobuf error changed: replace=%v payload=%d err=%v", replace, payloadSize, err)
			}
			if pooled, _ := MessagePoolCheck(wire); !pooled || !bytes.Equal(wire, encodingGrowthWire(payloadSize)) {
				t.Fatal("protobuf error consumed or changed borrowed wire input")
			}
			encodingPoolRequireBalanced(t, before)
			if !MessagePoolReturn(wire) {
				t.Fatal("borrowed wire input lost its owner")
			}
		}
	}
}

func TestProtoMarshalEncodingMovedOutputWithEqualCapacity(t *testing.T) {
	if messagePoolSnapshotInFreshProcess(t) {
		return
	}
	before := encodingPoolBoundaryNow()
	wire := []byte{8, 1}
	message := newEncodingGrowthProto(len(wire), wire, true, false, nil)
	encoded, err := ProtoMarshalWithTag(message, 0)
	encodingPoolRequireProtoHooks(t, message)
	if err != nil || !bytes.Equal(encoded, wire) || !MessagePoolReturn(encoded) {
		t.Fatalf("replacement protobuf owner changed: err=%v", err)
	}
	encodingPoolRequireBalanced(t, before)
}

func TestProtoMarshalEncodingNilOutputReturnsScratch(t *testing.T) {
	if messagePoolSnapshotInFreshProcess(t) {
		return
	}
	before := encodingPoolBoundaryNow()
	message := newEncodingGrowthProto(0, nil, false, true, nil)
	encoded, err := ProtoMarshalWithTag(message, 0)
	encodingPoolRequireProtoHooks(t, message)
	if err != nil || encoded != nil {
		t.Fatalf("nil encoder output changed: encoded=%v err=%v", encoded, err)
	}
	encodingPoolRequireBalanced(t, before)
}

func TestEncodedPooledOutputShareOwnership(t *testing.T) {
	if messagePoolSnapshotInFreshProcess(t) {
		return
	}
	before := encodingPoolBoundaryNow()
	subprotocol, _, err := marshalSubprotocol[[]byte](&encodingGrowthCodec{estimate: 2}, 2048, []byte{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	protobuf, err := ProtoMarshalWithTag(&protocol.SimpleMessage{Content: "borrowed input stays borrowed"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, encoded := range [][]byte{subprotocol, protobuf} {
		shared := MessagePoolShareReadOnly(encoded)
		if pooled, readOnly := MessagePoolCheck(shared); !pooled || !readOnly {
			t.Fatal("encoded pooled output could not be shared")
		}
		if MessagePoolReturn(encoded) {
			t.Fatal("first owner returned a root still owned by its share")
		}
		if !MessagePoolReturn(shared) {
			t.Fatal("last shared owner did not return its root")
		}
	}
	encodingPoolRequireBalanced(t, before)
}

// The same honest-path benchmarks compile against old and candidate source.
// They expose CPU/allocation changes; they are not correctness thresholds.
func BenchmarkPooledSubprotocolEncodingExact(b *testing.B) {
	payload := bytes.Repeat([]byte{0x5a}, 40)
	codec := &encodingGrowthCodec{estimate: len(payload)}
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for b.Loop() {
		encoded, overrun, err := marshalSubprotocol[[]byte](codec, 2048, payload)
		if err != nil || overrun {
			b.Fatalf("honest encoding failed: overrun=%v err=%v", overrun, err)
		}
		if !MessagePoolReturn(encoded) {
			b.Fatal("honest encoding lost its pooled owner")
		}
	}
}

func BenchmarkPooledProtoMarshalExact(b *testing.B) {
	message := &protocol.SimpleMessage{Content: "honest generated protobuf encoding"}
	b.ReportAllocs()
	for b.Loop() {
		encoded, err := ProtoMarshalWithTag(message, 0)
		if err != nil || !MessagePoolReturn(encoded) {
			b.Fatalf("honest protobuf encoding lost ownership: err=%v", err)
		}
	}
}
