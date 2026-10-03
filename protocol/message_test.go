package protocol_test

// Round-trip and canonical-encoding tests for the URmessage control plane.
//
// Like message_op_test.go, everything here enumerates the arms and the messages
// FROM THE COMPILED DESCRIPTOR. A oneof arm added to message.proto is covered by
// these tests the moment it is added, rather than being silently untested until
// somebody remembers to extend a list.

import (
	"bytes"
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/urnetwork/connect/protocol"
)

// sampleScalar returns a distinctive non-zero value for a scalar field, so that a
// field that fails to survive a round trip shows up as a difference rather than as
// two matching zeros.
func sampleScalar(t *testing.T, fd protoreflect.FieldDescriptor, seed int) protoreflect.Value {
	t.Helper()
	switch fd.Kind() {
	case protoreflect.BoolKind:
		// Distinct across seeds so that a bool map key yields two entries rather
		// than one; a lone entry cannot exhibit an unstable iteration order, which
		// would make TestDeterministicMarshalIsStable pass vacuously.
		return protoreflect.ValueOfBool(seed%2 == 0)
	case protoreflect.Uint32Kind:
		return protoreflect.ValueOfUint32(uint32(1000 + seed))
	case protoreflect.Uint64Kind:
		return protoreflect.ValueOfUint64(uint64(1_000_000 + seed))
	case protoreflect.Int32Kind:
		return protoreflect.ValueOfInt32(int32(1000 + seed))
	case protoreflect.Int64Kind:
		return protoreflect.ValueOfInt64(int64(1_000_000 + seed))
	case protoreflect.StringKind:
		// The seed MUST be part of the value. Without it every generated map key
		// collides, the map holds one entry, and a deterministic-marshal test over
		// it can never fail.
		return protoreflect.ValueOfString(fmt.Sprintf("sample-%s-%d", fd.Name(), seed))
	case protoreflect.BytesKind:
		b := make([]byte, 8)
		for i := range b {
			b[i] = byte(seed + i + 1)
		}
		return protoreflect.ValueOfBytes(b)
	case protoreflect.EnumKind:
		vals := fd.Enum().Values()
		// Prefer a non-zero value so the field is actually serialized.
		idx := 0
		if vals.Len() > 1 {
			idx = 1 + (seed % (vals.Len() - 1))
		}
		return protoreflect.ValueOfEnum(vals.Get(idx).Number())
	default:
		t.Fatalf("message.proto uses field kind %v (%s), which this test does not know how to "+
			"populate; extend sampleScalar rather than letting the field go untested", fd.Kind(), fd.FullName())
		return protoreflect.Value{}
	}
}

// populate fills every non-oneof field of m, descending depth levels into message
// fields. Oneof fields are skipped: the tests choose the arm themselves.
func populate(t *testing.T, m protoreflect.Message, depth int) {
	t.Helper()
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if fd.ContainingOneof() != nil {
			continue
		}
		switch {
		case fd.IsMap():
			mp := m.Mutable(fd).Map()
			// Enough entries that a non-deterministic iteration order is all but
			// certain to show itself. Two entries in one Go map bucket agree by
			// chance often enough to make a stability check flaky.
			want := 8
			if fd.MapKey().Kind() == protoreflect.BoolKind {
				want = 2 // a bool key admits no more
			}
			for k := 0; k < want; k++ {
				key := sampleScalar(t, fd.MapKey(), k).MapKey()
				val := fd.MapValue()
				if val.Kind() == protoreflect.MessageKind || val.Kind() == protoreflect.GroupKind {
					sub := mp.NewValue()
					if depth > 0 {
						populate(t, sub.Message(), depth-1)
					}
					mp.Set(key, sub)
				} else {
					mp.Set(key, sampleScalar(t, val, k))
				}
			}
			if mp.Len() != want {
				t.Fatalf("populating map field %s produced %d entries, wanted %d: the generated "+
					"keys collide, so any ordering test over this map would pass vacuously",
					fd.FullName(), mp.Len(), want)
			}
		case fd.IsList():
			list := m.Mutable(fd).List()
			for k := 0; k < 3; k++ {
				if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
					sub := list.NewElement()
					if depth > 0 {
						populate(t, sub.Message(), depth-1)
					}
					list.Append(sub)
				} else {
					list.Append(sampleScalar(t, fd, k))
				}
			}
		case fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind:
			sub := m.NewField(fd)
			if depth > 0 {
				populate(t, sub.Message(), depth-1)
			}
			m.Set(fd, sub)
		default:
			m.Set(fd, sampleScalar(t, fd, i))
		}
	}
}

func envelopes() []struct {
	name string
	msg  protoreflect.ProtoMessage
} {
	return []struct {
		name string
		msg  protoreflect.ProtoMessage
	}{
		{"MessageServerRequest", (*protocol.MessageServerRequest)(nil)},
		{"MessageServerResponse", (*protocol.MessageServerResponse)(nil)},
		{"MessageServerPush", (*protocol.MessageServerPush)(nil)},
	}
}

// TestEnvelopeOneofArmsRoundTrip marshals and unmarshals each envelope once per
// oneof arm, with that arm populated, and asserts the arm and its payload survive.
func TestEnvelopeOneofArmsRoundTrip(t *testing.T) {
	total := 0
	for _, env := range envelopes() {
		d := env.msg.ProtoReflect().Descriptor()
		od := d.Oneofs().ByName("body")
		if od == nil {
			t.Fatalf("%s has no `oneof body`", d.FullName())
		}
		arms := od.Fields()
		if arms.Len() == 0 {
			t.Fatalf("%s.body has no arms", d.FullName())
		}
		for i := 0; i < arms.Len(); i++ {
			arm := arms.Get(i)
			total++
			t.Run(env.name+"/"+string(arm.Name()), func(t *testing.T) {
				src := env.msg.ProtoReflect().New()
				// Populate the envelope's own non-oneof fields too (request_id,
				// protocol_version, reason), so the arm is not the only thing on
				// the wire.
				populate(t, src, 0)
				v := src.NewField(arm)
				populate(t, v.Message(), 2)
				src.Set(arm, v)

				wire, err := proto.Marshal(src.Interface())
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				dst := env.msg.ProtoReflect().New()
				if err := proto.Unmarshal(wire, dst.Interface()); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}

				got := dst.WhichOneof(od)
				if got == nil {
					t.Fatalf("after round trip no arm of %s.body is set; arm %q (number %d) did not survive",
						d.FullName(), arm.Name(), arm.Number())
				}
				if got.Number() != arm.Number() {
					t.Fatalf("arm %q (number %d) decoded as arm %q (number %d)",
						arm.Name(), arm.Number(), got.Name(), got.Number())
				}
				if !proto.Equal(src.Interface(), dst.Interface()) {
					t.Errorf("arm %q payload did not survive the round trip:\n src=%v\n dst=%v",
						arm.Name(), src.Interface(), dst.Interface())
				}
			})
		}
	}
	if total == 0 {
		t.Fatal("no oneof arms were exercised at all")
	}
}

// TestDeterministicMarshalIsStable checks that a deterministic marshal really is
// repeatable for every message in message.proto that carries a repeated or a map
// field — the two constructions whose encoding order is not fixed by the schema.
//
// Spec A §5.7 defines canonical_request_bytes as "the deterministically-marshaled
// request body message (protobuf deterministic marshal, fields ascending) with its
// own req_auth field set to zero length". If that marshal were not stable, two
// calls on the same message would produce two different MACs.
//
// The set of messages tested is derived from the descriptor, so a map field added
// to message.proto later is covered without anybody editing this test.
func TestDeterministicMarshalIsStable(t *testing.T) {
	fd := (*protocol.MessageServerRequest)(nil).ProtoReflect().Descriptor().ParentFile()
	msgs := fd.Messages()
	covered, withMaps := 0, 0
	for i := 0; i < msgs.Len(); i++ {
		md := msgs.Get(i)
		hasRepeated, hasMap := false, false
		fields := md.Fields()
		for j := 0; j < fields.Len(); j++ {
			f := fields.Get(j)
			if f.IsMap() {
				hasMap = true
			} else if f.IsList() {
				hasRepeated = true
			}
		}
		if !hasRepeated && !hasMap {
			continue
		}
		covered++
		if hasMap {
			withMaps++
		}
		t.Run(string(md.Name()), func(t *testing.T) {
			m := dynamicNew(t, md)
			populate(t, m, 2)
			opts := proto.MarshalOptions{Deterministic: true}
			first, err := opts.Marshal(m.Interface())
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			for k := 0; k < 32; k++ {
				again, err := opts.Marshal(m.Interface())
				if err != nil {
					t.Fatalf("marshal %d: %v", k, err)
				}
				if !bytes.Equal(first, again) {
					t.Fatalf("deterministic marshal of %s is not stable across calls: "+
						"call 0 produced %d bytes, call %d produced %d bytes that differ. "+
						"canonical_request_bytes (Spec A §5.7) would then MAC differently "+
						"on every attempt.", md.FullName(), len(first), k+1, len(again))
				}
			}
		})
	}
	if covered == 0 {
		t.Fatal("no message in message.proto has a repeated or map field; the descriptor walk is wrong")
	}
	t.Logf("checked deterministic stability over %d messages with repeated or map fields "+
		"(%d of them with map fields)", covered, withMaps)
}

// dynamicNew builds a mutable instance of md by finding its Go type through the
// global registry, so the test works for every message in the file without naming
// any of them.
func dynamicNew(t *testing.T, md protoreflect.MessageDescriptor) protoreflect.Message {
	t.Helper()
	mt, err := protoregistry.GlobalTypes.FindMessageByName(md.FullName())
	if err != nil {
		t.Fatalf("no Go type registered for %s: %v", md.FullName(), err)
	}
	return mt.New()
}

// TestCanonicalRequestBytesZeroTheAuthenticator exercises the exact recipe of Spec
// A §5.7 over every request message that carries a req_auth, derived from the
// descriptor rather than listed.
//
// Three properties, each of which would be a cross-implementation MAC failure if it
// did not hold:
//
//  1. the canonical bytes are stable across repeated marshals;
//  2. a req_auth explicitly set to zero length and a req_auth never set at all
//     encode identically, so a client that clears the field and a server that
//     rebuilds the message without it compute the same preimage;
//  3. a non-empty req_auth changes the encoding, which is why it has to be zeroed
//     before the MAC is taken in the first place.
func TestCanonicalRequestBytesZeroTheAuthenticator(t *testing.T) {
	fd := (*protocol.MessageServerRequest)(nil).ProtoReflect().Descriptor().ParentFile()
	msgs := fd.Messages()
	opts := proto.MarshalOptions{Deterministic: true}
	covered := 0
	for i := 0; i < msgs.Len(); i++ {
		md := msgs.Get(i)
		authField := md.Fields().ByName("req_auth")
		if authField == nil {
			continue
		}
		covered++
		t.Run(string(md.Name()), func(t *testing.T) {
			m := dynamicNew(t, md)
			populate(t, m, 2)

			// (2) zero length, explicitly set.
			m.Set(authField, protoreflect.ValueOfBytes([]byte{}))
			zeroSet, err := opts.Marshal(m.Interface())
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			// (1) stable.
			for k := 0; k < 8; k++ {
				again, err := opts.Marshal(m.Interface())
				if err != nil {
					t.Fatalf("marshal %d: %v", k, err)
				}
				if !bytes.Equal(zeroSet, again) {
					t.Fatalf("canonical_request_bytes for %s is not stable across marshals", md.FullName())
				}
			}
			// (2) cleared entirely.
			m.Clear(authField)
			cleared, err := opts.Marshal(m.Interface())
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !bytes.Equal(zeroSet, cleared) {
				t.Errorf("%s: req_auth set to zero length encodes differently from req_auth absent "+
					"(%d bytes vs %d). Spec A §5.7 says canonical_request_bytes has req_auth 'set to "+
					"zero length'; if the two spellings differ on the wire, a client and a server that "+
					"choose different spellings compute different MACs.", md.FullName(), len(zeroSet), len(cleared))
			}
			// (3) a real authenticator changes the bytes.
			m.Set(authField, protoreflect.ValueOfBytes(bytes.Repeat([]byte{0xab}, 32)))
			withAuth, err := opts.Marshal(m.Interface())
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if bytes.Equal(withAuth, cleared) {
				t.Errorf("%s: setting req_auth to 32 bytes did not change the encoding, so zeroing it "+
					"before the MAC would be a no-op and the field would not be excluded at all", md.FullName())
			}
			// and clearing it again reproduces the canonical bytes exactly.
			m.Clear(authField)
			roundTrip, err := opts.Marshal(m.Interface())
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !bytes.Equal(cleared, roundTrip) {
				t.Errorf("%s: clearing req_auth after setting it did not reproduce the canonical bytes", md.FullName())
			}
		})
	}
	if covered != 5 {
		t.Errorf("found %d request messages carrying req_auth; Spec A §5.7 requires it on exactly "+
			"five (Fetch, Subscribe, GroupStatus, BlobGrant, WrapFetch)", covered)
	}
}
