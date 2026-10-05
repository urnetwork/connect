// P2's aead half: flip eph_window ON THE WIRE and the record does not open.
//
// The other half -- flip it and the write_auth mac fails -- is message/ephwindow_test.go's,
// and the two are deliberately not one test. They are two mechanisms with two different
// jobs and two different holders. write_auth is a mac under the group's WRITE key, which
// spec A hands to the message server, and it is the term the server's plus or minus one
// window check rests on; the aead is under the record's own ladder position, which the
// server never holds, and it is what binds the window to the header a member opens. A test
// that proved one would have proved half: a builder that put the field in the preimage and
// not in the aad passes the mac half and fails here, and the reverse passes here and leaves
// the server checking a field anybody in the path may rewrite.
//
// The surgery is on the ENCODED record. Mutating the go struct before the open would assert
// something weaker and already covered -- TestEveryFieldOfARecordIsAuthenticatedByTheOpen
// moves every field of the header by reflection and requires the open to fail, and eph_window
// joined that class the moment it joined the struct, without an edit there. What this adds is
// the wire: the octets really move, the codec really reads them back, and the record built out
// of the mutated octets really fails the aead.
//
// WHAT THIS FILE DOES NOT SEE, AND IT IS NOW A CHOICE RATHER THAN A WALL. The records here are
// DURABLE, so the window under test is the presence rule's ZERO and the mutation moves it OFF
// zero. That is the case worth having on its own terms -- binding the zero is what stops a
// header being spliced across classes, which is the aad term's stated job. The sentence that
// stood here said the eph case was unreachable: "SealRecord refuses every other class until m1
// open item M1-6's successor lands ... no test anywhere in this module can be the eph case until
// sealing one is allowed." That successor landed on 2026-09-13 -- ledger item 152 ruled, M1-6's
// 2026-09-07 ruling reversed -- and the eph case is reachable now. ephkey_test.go is where it is
// taken, on the ladder rather than on the wire: this file's subject is the octets moving, and a
// DURABLE record moves the same octets.
package messagegroup

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/urnetwork/connect/message"
)

// Where eph_window's octets begin in record_bytes, written as the sum of the fields in front
// of it in connect/message's layout: format_version, group_id, sender_handle, epoch,
// stream_index, is_commit, retention_class_wire. It is transcribed here rather than reached
// for because the constant in connect/message is unexported and because a reproduction that
// asks the package under test where its own field is has asked the wrong question -- the same
// reason keysource_test.go transcribes its labels. ephWindowOffsetAgreesWithTheCodec is what
// holds the transcription to the package.
const (
	ephWindowWireOffset = 1 + 32 + 16 + 8 + 8 + 1 + 1
	ephWindowWireBytes  = 8
)

// The transcribed offset is where connect/message really puts the field.
//
// Without this the byte surgery below could be aimed eight octets off, land in expire_at,
// and report a covered field with perfect confidence. The check is the same one from both
// ends: the octets at the offset read back as the header's window, and a record encoded with
// a different window differs from this one in exactly those octets and nowhere else.
func TestTheEphWindowOffsetAgreesWithTheCodec(t *testing.T) {
	header := message.RecordHeader{
		RetentionClass: message.RetentionDurable,
		SizeBucket:     message.SizeBucket256,
		Epoch:          3,
		StreamIndex:    5,
	}
	record := message.Record{Header: header, CtHead: []byte{0x01, 0x02, 0x03}}
	zeroed, err := message.EncodeRecord(&record)
	if err != nil {
		t.Fatalf("EncodeRecord: %v", err)
	}
	if len(zeroed) < ephWindowWireOffset+ephWindowWireBytes {
		t.Fatalf("a %d octet record does not reach the window at %d", len(zeroed), ephWindowWireOffset)
	}
	if got := binary.BigEndian.Uint64(zeroed[ephWindowWireOffset : ephWindowWireOffset+ephWindowWireBytes]); got != 0 {
		t.Fatalf("a durable record's window octets read back as %d, want the presence rule's zero", got)
	}

	const window uint64 = 0x0123456789ABCDEF
	record.Header.EphWindow = window
	moved, err := message.EncodeRecord(&record)
	if err != nil {
		t.Fatalf("EncodeRecord: %v", err)
	}
	if len(moved) != len(zeroed) {
		t.Fatalf("moving the window changed the record's length from %d to %d, so the field is not fixed width", len(zeroed), len(moved))
	}
	differing := []int{}
	for i := range moved {
		if moved[i] != zeroed[i] {
			differing = append(differing, i)
		}
	}
	want := []int{}
	for i := ephWindowWireOffset; i < ephWindowWireOffset+ephWindowWireBytes; i++ {
		want = append(want, i)
	}
	// the top octet of this window is 0x01 and the zeroed record's is 0x00, so all eight
	// differ and the comparison is exact rather than a subset
	if !slices.Equal(differing, want) {
		t.Fatalf("moving the window changed octets %v and the transcribed field is %v", differing, want)
	}
	if got := binary.BigEndian.Uint64(moved[ephWindowWireOffset : ephWindowWireOffset+ephWindowWireBytes]); got != window {
		t.Fatalf("the window octets read back as %#x, want %#x, so the field is not big endian at this offset", got, window)
	}
	t.Logf("the window is the eight octets at %d, big endian, and moving it moves nothing else", ephWindowWireOffset)
}

// Flipping any bit of eph_window on the wire makes the record fail to open.
//
// The order is what makes this evidence rather than a coincidence. One record is sealed and
// encoded; all sixty four single bit mutations of the window are opened and must fail; and
// the UNMUTATED record is opened LAST and must succeed. Opening it last rather than first is
// deliberate: the receiver ratchet commits only on a successful open, so a control opened
// first would move the ratchet and every mutation after it would fail on the stream index
// instead of on the aead -- which is a failure, and the wrong one, and it would look exactly
// like success.
//
// The refusal is required to be the aead's own sentinel and not merely an error. A record
// refused by the session's class check, by its body_hash check or by the ratchet is a record
// the aead was never asked about, and this property is about the aead.
func TestFlippingEphWindowOnTheWireBreaksTheRecordAead(t *testing.T) {
	pair := newTestPair(t, "ephwindow-aead")
	pair.trackDurable(t)

	headPlain := []byte("the header plaintext")
	bodyPlain := []byte("the body plaintext")
	record, err := pair.sender.SealRecord(message.RetentionDurable, 0, false, headPlain, bodyPlain, 0, nil)
	if err != nil {
		t.Fatalf("SealRecord: %v", err)
	}
	if record.Header.EphWindow != 0 {
		t.Fatalf("a durable record was sealed with window %d and master section 8's presence rule is zero off eph", record.Header.EphWindow)
	}
	valid, err := message.EncodeRecord(record)
	if err != nil {
		t.Fatalf("EncodeRecord: %v", err)
	}

	flipped := 0
	for offset := ephWindowWireOffset; offset < ephWindowWireOffset+ephWindowWireBytes; offset++ {
		for bit := range 8 {
			mutated := slices.Clone(valid)
			mutated[offset] ^= 1 << bit
			what := fmt.Sprintf("bit %d of octet %d of eph_window", bit, offset-ephWindowWireOffset)
			parsed, err := message.ParseRecord(mutated)
			if err != nil {
				t.Fatalf("%s: the mutated record does not parse, so the aead is never asked about it: %v", what, err)
			}
			if parsed.Header.EphWindow == 0 {
				t.Fatalf("%s: the mutation left the parsed window at zero, so it did not land on the field", what)
			}
			gotHead, gotBody, err := pair.opener.OpenRecord(parsed)
			if err == nil {
				t.Fatalf("%s: the record still opened, so the window is outside both aads and an EPH record's header could be spliced onto another class",
					what)
			}
			if !errors.Is(err, ErrRecordAeadOpen) {
				t.Errorf("%s: refused with %v, want ErrRecordAeadOpen: a refusal from anywhere else means the aead never ran", what, err)
			}
			if gotHead != nil || gotBody != nil {
				t.Errorf("%s: a refusal came back with %d head octets and %d body octets", what, len(gotHead), len(gotBody))
			}
			flipped++
		}
	}
	if want := ephWindowWireBytes * 8; flipped != want {
		t.Fatalf("%d flips were made and the field is %d octets, which is %d bits", flipped, ephWindowWireBytes, want)
	}

	// the control, LAST, and it is what says the sixty four refusals above are about the
	// window rather than about a record that was never openable
	reparsed, err := message.ParseRecord(valid)
	if err != nil {
		t.Fatalf("the unmutated record does not parse: %v", err)
	}
	gotHead, gotBody, err := pair.opener.OpenRecord(reparsed)
	if err != nil {
		t.Fatalf("the UNMUTATED record does not open, so nothing above is about the window: %v", err)
	}
	if !bytes.Equal(gotHead, headPlain) || !bytes.Equal(gotBody, bodyPlain) {
		t.Fatalf("the unmutated record opened to %q and %q", gotHead, gotBody)
	}
	t.Logf("all %d single bit flips of the window's %d octets fail the aead, and the untouched record opens", flipped, ephWindowWireBytes)
}
