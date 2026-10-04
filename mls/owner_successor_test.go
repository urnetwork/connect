// urmessage_owner_successor, extension type 0xF003.
//
// The plan supplied six tests for this task. Four of them observe what their names claim; two do
// not, and both are here in a repaired form with the reason at the declaration:
// TestOwnerSuccessorRoundTrip agrees with whatever the encoder wrote, so it passes over a codec
// that drops a field on write and defaults it on read, and TestOwnerSuccessorRejectsShortFloor
// asks only the ENCODE side of a rule this profile has to enforce in both directions. What
// separates the repaired versions from the originals is that the bytes are assembled here, by
// hand, out of the RFC 9420 section 2.1.2 varint and big endian integers -- so the encoder has
// something to be wrong against rather than only itself.
package mls

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// ---------------------------------------------------------------------------
// the octets, assembled independently of the encoder
// ---------------------------------------------------------------------------

// handAssembledOwnerSuccessorBody lays out the body of extension 0xF003 the way spec A section
// 3.4 and RFC 9420 section 2.1.2 say to, using encoding/binary and a varint written out in full
// rather than anything this package declares.
//
// The varint is spelled here rather than reached through syntax.WriteVarint on purpose: this
// package's encoder and this package's varint are the same code, so a body assembled through the
// second would agree with the first about a length prefix that was wrong in both.
func handAssembledOwnerSuccessorBody(t *testing.T, enabled byte, successorMemberId []byte,
	nominatedAtMs uint64, floorMs uint64) []byte {

	t.Helper()
	body := []byte{enabled}
	switch n := len(successorMemberId); {
	case n <= 0x3f:
		// one octet, prefix 0b00
		body = append(body, byte(n))
	case n <= 0x3fff:
		// two octets, prefix 0b01
		body = append(body, byte(n>>8)|0x40, byte(n))
	default:
		t.Fatalf("this helper spells the one and two octet varint forms and was handed a %d octet successor id", n)
	}
	body = append(body, successorMemberId...)
	body = binary.BigEndian.AppendUint64(body, nominatedAtMs)
	body = binary.BigEndian.AppendUint64(body, floorMs)
	return body
}

// TestOwnerSuccessorBodyIsTheOctetsAnIndependentEncoderProduces is what the plan's round trip
// test cannot be.
//
// A round trip proves that this package's decoder undoes this package's encoder, which a codec
// that dropped NominatedAtMs on write and left it zero on read satisfies perfectly -- and that
// codec would be a nomination whose countersignature preimage no admin could reproduce. So the
// body is compared against octets laid out here, and the three successor lengths are the three
// the varint has to get right: none, one octet of length prefix, and two.
func TestOwnerSuccessorBodyIsTheOctetsAnIndependentEncoderProduces(t *testing.T) {
	for _, one := range []struct {
		name              string
		enabled           bool
		enabledByte       byte
		successorMemberId []byte
		nominatedAtMs     uint64
		floorMs           uint64
	}{
		{
			name: "no successor nominated", enabled: true, enabledByte: 0x01,
			successorMemberId: nil, nominatedAtMs: 0, floorMs: SuccessionFloorMinMs,
		},
		{
			name: "a 32 octet identity key, the one octet varint", enabled: true, enabledByte: 0x01,
			successorMemberId: bytes.Repeat([]byte{0xa7}, 32),
			nominatedAtMs:     1770000000000, floorMs: SuccessionFloorMinMs,
		},
		{
			name:    "a 64 octet successor id, which is where the varint grows a second octet",
			enabled: false, enabledByte: 0x00,
			successorMemberId: bytes.Repeat([]byte{0x5c}, 64),
			nominatedAtMs:     0xfedcba9876543210, floorMs: SuccessionFloorMinMs + 1,
		},
	} {
		t.Run(one.name, func(t *testing.T) {
			nomination := &OwnerSuccessorExtension{
				Enabled:           one.enabled,
				SuccessorMemberId: one.successorMemberId,
				NominatedAtMs:     one.nominatedAtMs,
				FloorMs:           one.floorMs,
			}
			encoded, err := nomination.Encode()
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			want := handAssembledOwnerSuccessorBody(t, one.enabledByte, one.successorMemberId,
				one.nominatedAtMs, one.floorMs)
			if !bytes.Equal(encoded.ExtensionData, want) {
				t.Fatalf("body = %x, want %x", encoded.ExtensionData, want)
			}
			// and the same octets are what the decoder reads, so the two halves are held to
			// one layout rather than to each other
			parsed, err := ParseOwnerSuccessorExtension(want)
			if err != nil {
				t.Fatalf("ParseOwnerSuccessorExtension over the hand assembled body: %v", err)
			}
			if parsed.Enabled != one.enabled || parsed.NominatedAtMs != one.nominatedAtMs ||
				parsed.FloorMs != one.floorMs ||
				!bytes.Equal(parsed.SuccessorMemberId, one.successorMemberId) {
				t.Fatalf("the hand assembled body decoded to %+v, want %+v", parsed, nomination)
			}
		})
	}
}

// TestTheOwnerSuccessorTagOnTheWireIsF003 pins the code point at the one place a peer reads it:
// the two octets an encoded Extension starts with.
//
// extension_test.go pins the CONSTANT against the literal 0xF003, which is the registry half.
// This is the other half -- that the encoder puts that constant on the wire and not some
// neighbouring one -- and 0xF001 is two identifiers away.
func TestTheOwnerSuccessorTagOnTheWireIsF003(t *testing.T) {
	nomination := &OwnerSuccessorExtension{Enabled: true, FloorMs: SuccessionFloorMinMs}
	encoded, err := nomination.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	onTheWire, err := marshalBytes(encoded.MarshalMLS)
	if err != nil {
		t.Fatalf("encode the whole extension entry: %v", err)
	}
	if len(onTheWire) < 2 || onTheWire[0] != 0xF0 || onTheWire[1] != 0x03 {
		t.Fatalf("the entry starts %x, want the two octets f003", onTheWire[:min(2, len(onTheWire))])
	}
}

// TestSuccessionFloorMinIsNinetyDaysOfMilliseconds gives the constant something to be wrong
// against.
//
// Every other statement of 7776000000 in this tree is prose or a copy of prose: MASTER section
// 11 says ninety days, spec A section 3.4 writes the digits out, and the plan writes them again.
// A digit copied wrong is invisible to every test in this file, because all of them build their
// inputs out of SuccessionFloorMinMs and would agree with 777600000 exactly as well.
func TestSuccessionFloorMinIsNinetyDaysOfMilliseconds(t *testing.T) {
	const msPerSecond = 1000
	const secondsPerDay = 24 * 60 * 60
	if want := uint64(90 * secondsPerDay * msPerSecond); SuccessionFloorMinMs != want {
		t.Fatalf("SuccessionFloorMinMs = %d, and ninety days of milliseconds is %d",
			SuccessionFloorMinMs, want)
	}
}

// ---------------------------------------------------------------------------
// the plan's tests
// ---------------------------------------------------------------------------

func TestOwnerSuccessorRoundTrip(t *testing.T) {
	crypto := testCrypto(t)
	successor := testIdentity(t, crypto, "successor")

	ext := &OwnerSuccessorExtension{
		Enabled:           true,
		SuccessorMemberId: successor.IdentityPub,
		NominatedAtMs:     1770000000000,
		FloorMs:           SuccessionFloorMinMs,
	}
	encoded, err := ext.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if encoded.ExtensionType != ExtensionTypeUrmessageOwnerSuccessor {
		t.Fatalf("ExtensionType = %#x, want %#x", encoded.ExtensionType, ExtensionTypeUrmessageOwnerSuccessor)
	}
	parsed, err := ParseOwnerSuccessorExtension(encoded.ExtensionData)
	if err != nil {
		t.Fatalf("ParseOwnerSuccessorExtension: %v", err)
	}
	if !parsed.Enabled || parsed.NominatedAtMs != 1770000000000 || parsed.FloorMs != SuccessionFloorMinMs {
		t.Fatalf("fields did not survive: %+v", parsed)
	}
	if !bytes.Equal(parsed.SuccessorMemberId, successor.IdentityPub) {
		t.Fatal("SuccessorMemberId did not survive the round trip")
	}
	reencoded, err := parsed.Encode()
	if err != nil {
		t.Fatalf("re-Encode: %v", err)
	}
	if !bytes.Equal(reencoded.ExtensionData, encoded.ExtensionData) {
		t.Fatal("re-encode is not byte identical")
	}
}

func TestOwnerSuccessorEnabledIsOneWireByte(t *testing.T) {
	ext := &OwnerSuccessorExtension{Enabled: true, FloorMs: SuccessionFloorMinMs}
	encoded, err := ext.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if encoded.ExtensionData[0] != 0x01 {
		t.Fatalf("enabled byte = %#x, want 0x01", encoded.ExtensionData[0])
	}
	off := &OwnerSuccessorExtension{Enabled: false, FloorMs: SuccessionFloorMinMs}
	encodedOff, err := off.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if encodedOff.ExtensionData[0] != 0x00 {
		t.Fatalf("disabled byte = %#x, want 0x00", encodedOff.ExtensionData[0])
	}
	// the two differ in that octet and in nothing else, which is what says the octet is the
	// flag rather than something the rest of the body happens to move with
	if len(encoded.ExtensionData) != len(encodedOff.ExtensionData) ||
		!bytes.Equal(encoded.ExtensionData[1:], encodedOff.ExtensionData[1:]) {
		t.Fatalf("enabled encodes to %x and disabled to %x, which differ past the first octet",
			encoded.ExtensionData, encodedOff.ExtensionData)
	}
}

// TestOwnerSuccessorRejectsNonBooleanByte is the plan's, widened from the single byte 0x02 to
// every one of the 254 the rule is about.
//
// A decoder that refused 0x02 and accepted 0xff would pass the plan's version and would still
// give this profile 255 encodings of true, which inside the group context is 255 transcript
// hashes for one nomination.
func TestOwnerSuccessorRejectsNonBooleanByte(t *testing.T) {
	ext := &OwnerSuccessorExtension{Enabled: true, FloorMs: SuccessionFloorMinMs}
	encoded, err := ext.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for enabled := 2; enabled <= 0xff; enabled += 1 {
		mutated := bytes.Clone(encoded.ExtensionData)
		mutated[0] = byte(enabled)
		parsed, err := ParseOwnerSuccessorExtension(mutated)
		if !errors.Is(err, ErrMalformedExtension) {
			t.Fatalf("Parse accepted %#02x as a boolean with %v (%+v), which makes two encodings of true",
				enabled, err, parsed)
		}
	}
}

func TestOwnerSuccessorRejectsShortFloor(t *testing.T) {
	ext := &OwnerSuccessorExtension{Enabled: true, FloorMs: SuccessionFloorMinMs - 1}
	if _, err := ext.Encode(); !errors.Is(err, ErrSuccessionFloorTooShort) {
		t.Fatalf("Encode error = %v, want ErrSuccessionFloorTooShort", err)
	}
}

func TestOwnerSuccessorOfAbsentIsNotAnError(t *testing.T) {
	ext, present, err := OwnerSuccessorOf(nil)
	if err != nil {
		t.Fatalf("OwnerSuccessorOf: %v", err)
	}
	if present || ext != nil {
		t.Fatal("OwnerSuccessorOf reported a nomination in an empty extension list")
	}
	// and an extension list that carries OTHER entries and no nomination is the same answer,
	// which is the half a nil argument cannot state: a lookup that answered its first entry
	// whatever its type would pass the nil case and fail this one
	ext, present, err = OwnerSuccessorOf([]Extension{
		{ExtensionType: ExtensionTypeUrmessageGroupPolicy, ExtensionData: []byte{0x01}},
		{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: []byte{0x02}},
	})
	if err != nil || present || ext != nil {
		t.Fatalf("OwnerSuccessorOf over a list carrying two other types answered (%+v, %v, %v), want no nomination and no error",
			ext, present, err)
	}
}

func TestSuccessionPreimageIsStable(t *testing.T) {
	got, err := successionPreimage([]byte("gid"), 7, []byte("sid"), 42)
	if err != nil {
		t.Fatalf("successionPreimage: %v", err)
	}
	want := []byte("URmessage/v1/succession")
	want = append(want, 0, 0, 0, 3, 'g', 'i', 'd')
	want = append(want, 0, 0, 0, 0, 0, 0, 0, 7)
	want = append(want, 0, 0, 0, 3, 's', 'i', 'd')
	want = append(want, 0, 0, 0, 0, 0, 0, 0, 42)
	if !bytes.Equal(got, want) {
		t.Fatalf("preimage = %x, want %x", got, want)
	}
}

// ---------------------------------------------------------------------------
// the rules, one sentinel at a time
// ---------------------------------------------------------------------------

// TestTheFloorRefusalIsThisRulesSentinelAndNoOtherSuccessionSentinel is ledger 17 for
// ErrSuccessionFloorTooShort, which gains its first caller in this task.
//
// errors_lifecycle.go declares five succession sentinels because MASTER section 11 makes five
// conditions, and this project's most repeated defect is a set of refusals that all reduce to
// one comparison. Four of the five are conditions on a promotion commit and are succession.go's
// -- nothing in this task can return them -- so what this states is the half that IS reachable
// here: the floor refusal answers its own value and answers to none of its four neighbours, so a
// later task that wired the wrong one in cannot pass a test that asked only whether an error came
// back.
func TestTheFloorRefusalIsThisRulesSentinelAndNoOtherSuccessionSentinel(t *testing.T) {
	short := &OwnerSuccessorExtension{Enabled: true, FloorMs: SuccessionFloorMinMs - 1}
	_, err := short.Encode()
	if !errors.Is(err, ErrSuccessionFloorTooShort) {
		t.Fatalf("Encode error = %v, want ErrSuccessionFloorTooShort", err)
	}
	for name, other := range map[string]error{
		"ErrSuccessionDisabled":   ErrSuccessionDisabled,
		"ErrSuccessionNotNominee": ErrSuccessionNotNominee,
		"ErrSuccessionQuorum":     ErrSuccessionQuorum,
		"ErrSuccessionFloor":      ErrSuccessionFloor,
		"ErrMalformedExtension":   ErrMalformedExtension,
	} {
		if errors.Is(err, other) {
			t.Errorf("the floor refusal answers to %s as well as to its own sentinel; a caller asking which of the five section 11 conditions failed would be told two of them",
				name)
		}
	}
	// the detail names both numbers, because a caller told only that the floor is too short has
	// to go and find out what it is too short by
	if got := err.Error(); !bytes.Contains([]byte(got), []byte("7776000000")) {
		t.Errorf("the refusal reads %q and does not name the minimum it enforced", got)
	}
}

// TestAShortFloorIsRefusedByTheConditionAndNotByTheCodec is what replaced the decode side floor
// refusal, and it states the three things that decision has to keep true.
//
// The floor exists so a group cannot shorten its own succession delay after the fact, and a
// refusal on the encode side alone would stop THIS client writing one and stop nothing else. What
// stops a modified client is spec A section 3.4 condition 5, which refuses the PROMOTION -- so the
// ninety day delay holds for every member whether or not the nomination decodes, and it holds at
// the one place a promotion is judged rather than at every place a group context is read.
//
// Refusing at the codec instead cost three things and each is asserted here: the nomination has to
// REACH a caller so that condition 1, the owner's opt out, can be asked of it first; the group
// context has to stay readable to a client doing something else entirely; and Validate has to go
// on refusing the value, so Encode and condition 5 share one comparison rather than each carrying
// its own copy of the threshold.
func TestAShortFloorIsRefusedByTheConditionAndNotByTheCodec(t *testing.T) {
	successor := bytes.Repeat([]byte{0x11}, 32)
	for _, floorMs := range []uint64{0, 1, SuccessionFloorMinMs - 1} {
		body := handAssembledOwnerSuccessorBody(t, 0x01, successor, 1770000000000, floorMs)
		parsed, err := ParseOwnerSuccessorExtension(body)
		if err != nil {
			t.Fatalf("a body carrying a %d ms floor was refused at the decode with %v; the group context that carries it is then unreadable, and the refusal names succession to a caller that was not asking about it",
				floorMs, err)
		}
		if parsed.FloorMs != floorMs {
			t.Fatalf("the nomination came back carrying a %d ms floor and the octets said %d; a value the codec normalises is one condition 5 can never see",
				parsed.FloorMs, floorMs)
		}
		// the rule is still stated, once, where both doors that ACT on a nomination reach it
		if err := parsed.Validate(); !errors.Is(err, ErrSuccessionFloorTooShort) {
			t.Fatalf("Validate over a %d ms floor answered %v, want ErrSuccessionFloorTooShort; spec A section 3.4 condition 5 and Encode both call this, and a rule nothing states is one neither of them can enforce",
				floorMs, err)
		}
		// and this build still refuses to WRITE one, which is the direction the encode side owns
		if _, err := parsed.Encode(); !errors.Is(err, ErrSuccessionFloorTooShort) {
			t.Fatalf("Encode of a %d ms floor answered %v, want ErrSuccessionFloorTooShort", floorMs, err)
		}
	}
	// and the floor exactly at the minimum is accepted, so the comparison is the boundary the
	// spec states and not one either side of it
	atTheFloor := handAssembledOwnerSuccessorBody(t, 0x01, successor, 1770000000000, SuccessionFloorMinMs)
	nomination, err := ParseOwnerSuccessorExtension(atTheFloor)
	if err != nil {
		t.Fatalf("a body carrying exactly the ninety day floor was refused: %v", err)
	}
	if err := nomination.Validate(); err != nil {
		t.Fatalf("Validate over exactly the ninety day floor answered %v, want nil", err)
	}
}

// TestADisabledNominationIsReadEvenWhenItsFloorIsShort is spec A section 3.4's opt out, at the
// only door this task owns.
//
// Section 3.4 lists five conditions on a promotion, puts Enabled first, and states that the opt
// out is absolute: TestSuccessionOptOutIsAbsolute -- p7 task 21's -- asserts a promotion in a
// group with Enabled false fails even when the other four conditions hold. A codec that answered
// the FLOOR decided a nomination whose owner had switched succession off before anything asked
// whether it was on. For the pair that is both disabled and short floored, OwnerSuccessorOf
// answered ErrSuccessionFloorTooShort and the promotion path never saw Enabled at all -- and task
// 21's drafted tests never construct that pair, so the divergence would have landed green.
//
// What this states is the half that has to be true for condition 1 to be askable at all: the pair
// reaches a caller, and it reaches it saying it is switched off.
func TestADisabledNominationIsReadEvenWhenItsFloorIsShort(t *testing.T) {
	const shortFloor = uint64(60000)
	body := handAssembledOwnerSuccessorBody(t, 0x00, nil, 0, shortFloor)
	nomination, found, err := OwnerSuccessorOf([]Extension{
		{ExtensionType: ExtensionTypeUrmessageOwnerSuccessor, ExtensionData: body},
	})
	if err != nil {
		t.Fatalf("a nomination that is both disabled and short floored was answered %v out of a group context; the opt out is condition 1 and the floor is condition 5, so the owner's own decision lost to one they may never have looked at",
			err)
	}
	if !found {
		t.Fatal("OwnerSuccessorOf reported no nomination in a list that carries one")
	}
	if nomination.Enabled {
		t.Fatal("the nomination came back enabled")
	}
	if nomination.FloorMs != shortFloor {
		t.Fatalf("FloorMs = %d, want %d", nomination.FloorMs, shortFloor)
	}
	// and the floor is still a refusal, so this is about WHICH DOOR answers rather than about
	// the rule having gone away
	if err := nomination.Validate(); !errors.Is(err, ErrSuccessionFloorTooShort) {
		t.Fatalf("Validate over the short floor answered %v, want ErrSuccessionFloorTooShort", err)
	}
}

// TestTheNominationCodecRoundTripsEveryValueItsEncoderWrites is the syntax.Codec contract the pin
// beside this type claims, stated over the values that used to break it.
//
// syntax.Marshal(&OwnerSuccessorExtension{FloorMs: 0}) succeeded and syntax.Unmarshal of its own
// output failed, so the declared codec was not total over the type it is declared for. That is not
// an abstract complaint about a contract: cloneProposal in proposal_list.go copies a value through
// Marshal and Unmarshal precisely because a round trip makes the copy exactly what the octets say,
// and a codec of this package that refuses its own output is a copy that fails on one value and
// no other, inside a cache whose whole job is to answer what the group agreed to.
func TestTheNominationCodecRoundTripsEveryValueItsEncoderWrites(t *testing.T) {
	for _, one := range []OwnerSuccessorExtension{
		{},
		{FloorMs: 0},
		{Enabled: true, SuccessorMemberId: bytes.Repeat([]byte{0x11}, 32), NominatedAtMs: 1, FloorMs: 1},
		{Enabled: false, NominatedAtMs: 1770000000000},
		{Enabled: true, SuccessorMemberId: bytes.Repeat([]byte{0x22}, 32),
			NominatedAtMs: 1770000000000, FloorMs: SuccessionFloorMinMs},
	} {
		encoded, err := syntax.Marshal(&one)
		if err != nil {
			t.Errorf("syntax.Marshal(%+v) = %v", one, err)
			continue
		}
		back := OwnerSuccessorExtension{}
		if err := syntax.Unmarshal(encoded, &back); err != nil {
			t.Errorf("the codec refused its own output for %+v: %v; the syntax.Codec pinned beside this type claims a round trip the pair does not have",
				one, err)
			continue
		}
		again, err := syntax.Marshal(&back)
		if err != nil {
			t.Errorf("syntax.Marshal of the decoded %+v = %v", back, err)
			continue
		}
		if !bytes.Equal(encoded, again) {
			t.Errorf("%+v encodes to %x and the decode of that re-encodes to %x", one, encoded, again)
		}
	}
}

// TestOwnerSuccessorRefusesANominationTimeWithNoSuccessorNamed is the second of Validate's two
// rules, and it is refused with a different sentinel from the first.
func TestOwnerSuccessorRefusesANominationTimeWithNoSuccessorNamed(t *testing.T) {
	orphan := &OwnerSuccessorExtension{
		Enabled:       true,
		NominatedAtMs: 1770000000000,
		FloorMs:       SuccessionFloorMinMs,
	}
	_, err := orphan.Encode()
	if !errors.Is(err, ErrMalformedExtension) {
		t.Fatalf("Encode error = %v, want ErrMalformedExtension", err)
	}
	if errors.Is(err, ErrSuccessionFloorTooShort) {
		t.Error("a nomination time with no successor answers the FLOOR sentinel, so the two rules of Validate are one comparison")
	}
	// and the same rule reached through the read path, which is Validate and not the decode: the
	// octets decode, and what refuses is the one statement of the rule that Encode and spec A
	// section 3.4's condition 5 both reach
	body := handAssembledOwnerSuccessorBody(t, 0x01, nil, 1770000000000, SuccessionFloorMinMs)
	decoded, err := ParseOwnerSuccessorExtension(body)
	if err != nil {
		t.Fatalf("a nomination time with no successor was refused at the decode with %v; a codec that judges takes the whole group context down with it",
			err)
	}
	if err := decoded.Validate(); !errors.Is(err, ErrMalformedExtension) {
		t.Fatalf("Validate over a nomination time with no successor answered %v, want ErrMalformedExtension", err)
	}
	// and the two legal shapes either side of it: nobody nominated and no time, and a successor
	// named at time zero, which is a nomination made by a client whose clock read the epoch
	for _, legal := range []*OwnerSuccessorExtension{
		{Enabled: false, FloorMs: SuccessionFloorMinMs},
		{Enabled: true, SuccessorMemberId: []byte{0x01}, FloorMs: SuccessionFloorMinMs},
	} {
		if _, err := legal.Encode(); err != nil {
			t.Errorf("%+v was refused with %v; the rule is about a time with no subject and not about either half on its own",
				legal, err)
		}
	}
}

// ---------------------------------------------------------------------------
// the lookup
// ---------------------------------------------------------------------------

func TestOwnerSuccessorOfParsesTheNominationOutOfAContextList(t *testing.T) {
	crypto := testCrypto(t)
	successor := testIdentity(t, crypto, "successor")
	nomination := &OwnerSuccessorExtension{
		Enabled:           true,
		SuccessorMemberId: successor.IdentityPub,
		NominatedAtMs:     1770000000000,
		FloorMs:           SuccessionFloorMinMs,
	}
	encoded, err := nomination.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// behind two other entries, so a lookup that answered position zero would fail here
	found, present, err := OwnerSuccessorOf([]Extension{
		{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: []byte{0x00, 0x00, 0x00}},
		{ExtensionType: ExtensionTypeUrmessageGroupPolicy, ExtensionData: []byte{0x01}},
		encoded,
	})
	if err != nil || !present {
		t.Fatalf("OwnerSuccessorOf answered (%v, %v), want the nomination", present, err)
	}
	if !found.Enabled || found.NominatedAtMs != 1770000000000 ||
		!bytes.Equal(found.SuccessorMemberId, successor.IdentityPub) {
		t.Fatalf("the nomination read out of the list is %+v", found)
	}
}

// TestOwnerSuccessorOfRefusesAListCarryingTwoNominations is the repeated type rule reaching this
// accessor without this accessor restating it.
//
// RFC 9420 forbids two entries of one type and nothing in this build refuses one except the
// lookup: ValSem209 is named by the validation plan and implemented nowhere. What a first-wins
// answer would cost here is specific -- the extension list is inside the confirmed transcript
// hash, so both nominations are what the group agreed to by the transcript's reckoning, and which
// identity a member believes is the successor would be decided by iteration order.
func TestOwnerSuccessorOfRefusesAListCarryingTwoNominations(t *testing.T) {
	first, err := (&OwnerSuccessorExtension{
		Enabled: true, SuccessorMemberId: bytes.Repeat([]byte{0x01}, 32),
		NominatedAtMs: 1, FloorMs: SuccessionFloorMinMs,
	}).Encode()
	if err != nil {
		t.Fatalf("Encode the first nomination: %v", err)
	}
	second, err := (&OwnerSuccessorExtension{
		Enabled: true, SuccessorMemberId: bytes.Repeat([]byte{0x02}, 32),
		NominatedAtMs: 2, FloorMs: SuccessionFloorMinMs,
	}).Encode()
	if err != nil {
		t.Fatalf("Encode the second nomination: %v", err)
	}
	other := Extension{ExtensionType: ExtensionTypeUrmessageGroupPolicy, ExtensionData: []byte{0x01}}
	for _, held := range [][]Extension{
		{first, second},
		{second, first},
		{other, first, other, second},
	} {
		found, present, err := OwnerSuccessorOf(held)
		if !errors.Is(err, ErrMalformedExtension) || present || found != nil {
			t.Errorf("a list carrying two nominations answered (%+v, %v, %v), want ErrMalformedExtension and no nomination",
				found, present, err)
		}
	}
}

// TestARefusedOwnerSuccessorDecodeLeavesTheCallersValueAlone is what the "stages" row in
// decoder_publish_test.go's pinned order claims, stated as behaviour.
//
// The derived gate there reads the BODY and says this decoder assigns its receiver once, at the
// end. This says what that is worth: a nomination refused part way leaves the caller holding the
// nomination it already had, rather than a composite of the group's real successor and a peer's
// truncated bytes.
func TestARefusedOwnerSuccessorDecodeLeavesTheCallersValueAlone(t *testing.T) {
	held := OwnerSuccessorExtension{
		Enabled:           true,
		SuccessorMemberId: bytes.Repeat([]byte{0xaa}, 32),
		NominatedAtMs:     1770000000000,
		FloorMs:           SuccessionFloorMinMs,
	}
	whole, err := held.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	refusals := 0
	for cut := 0; cut < len(whole.ExtensionData); cut += 1 {
		into := held
		into.SuccessorMemberId = bytes.Clone(held.SuccessorMemberId)
		before := into
		if err := syntax.Unmarshal(whole.ExtensionData[:cut], &into); err == nil {
			t.Fatalf("a body cut to %d of %d octets decoded without a refusal", cut, len(whole.ExtensionData))
		}
		refusals += 1
		if into.Enabled != before.Enabled || into.NominatedAtMs != before.NominatedAtMs ||
			into.FloorMs != before.FloorMs || !bytes.Equal(into.SuccessorMemberId, before.SuccessorMemberId) {
			t.Fatalf("a refused decode of %d octets left the caller holding %+v, want %+v",
				cut, into, before)
		}
	}
	if refusals == 0 {
		t.Fatal("no truncation was refused, so this states nothing about what a refusal leaves behind")
	}

	// and the refusal that is NOT a truncation, which is the one the sweep above cannot reach.
	// This decoder no longer judges a nomination's VALUE -- see the note on UnmarshalMLS for the
	// decision and its three reasons -- so the complete body refusal it still has is the one
	// about the octets themselves: an enabled byte that is neither 0 nor 1 encodes no bool of
	// this type. The body carrying it is FULL LENGTH, which is what makes it a statement the
	// truncation sweep cannot make and what the derived gate in decoder_publish_test.go cannot
	// model either, since it reads writes against reads and not against refusals.
	full := handAssembledOwnerSuccessorBody(t, 0x02, bytes.Repeat([]byte{0x11}, 32),
		1770000000000, SuccessionFloorMinMs)
	into := held
	into.SuccessorMemberId = bytes.Clone(held.SuccessorMemberId)
	before := into
	if err := syntax.Unmarshal(full, &into); !errors.Is(err, ErrMalformedExtension) {
		t.Fatalf("a full length body carrying an enabled byte of 0x02 answered %v, want ErrMalformedExtension", err)
	}
	if into.Enabled != before.Enabled || into.NominatedAtMs != before.NominatedAtMs ||
		into.FloorMs != before.FloorMs || !bytes.Equal(into.SuccessorMemberId, before.SuccessorMemberId) {
		t.Fatalf("a refused decode left the caller holding %+v, want %+v; the nomination its group actually agreed to has been overwritten by octets this build will not read",
			into, before)
	}
}

// ---------------------------------------------------------------------------
// the countersignature preimage
// ---------------------------------------------------------------------------

// TestSuccessionPreimageSeparatesEveryFieldFromItsNeighbour is what the fixed vector next door
// cannot say.
//
// TestSuccessionPreimageIsStable pins one input to one output, which a preimage that
// concatenated its two variable fields with no length prefix at all would satisfy exactly -- and
// that preimage collides, so an admin countersigning the promotion of "sid" in group "gid" would
// have signed the promotion of "id" in group "gids" at the same time. The pairs below are chosen
// so that every one of them is a collision under some plausible layout error: a moved boundary
// between the two length prefixed fields, a swapped epoch and nomination time, and the two
// integers written little endian.
func TestSuccessionPreimageSeparatesEveryFieldFromItsNeighbour(t *testing.T) {
	preimageOf := func(groupId []byte, epoch uint64, successorMemberId []byte, nominatedAtMs uint64) string {
		t.Helper()
		got, err := successionPreimage(groupId, epoch, successorMemberId, nominatedAtMs)
		if err != nil {
			t.Fatalf("successionPreimage(%q, %d, %q, %d): %v", groupId, epoch, successorMemberId, nominatedAtMs, err)
		}
		return string(got)
	}
	seen := map[string]string{}
	for _, one := range []struct {
		name              string
		groupId           []byte
		epoch             uint64
		successorMemberId []byte
		nominatedAtMs     uint64
	}{
		{"the boundary as written", []byte("gid"), 7, []byte("sid"), 42},
		{"the boundary moved one octet right", []byte("gids"), 7, []byte("id"), 42},
		{"the boundary moved one octet left", []byte("gi"), 7, []byte("dsid"), 42},
		{"an empty group id", []byte{}, 7, []byte("gidsid"), 42},
		{"the two integers swapped", []byte("gid"), 42, []byte("sid"), 7},
		{"the epoch alone changed", []byte("gid"), 8, []byte("sid"), 42},
		{"the nomination time alone changed", []byte("gid"), 7, []byte("sid"), 43},
	} {
		got := preimageOf(one.groupId, one.epoch, one.successorMemberId, one.nominatedAtMs)
		if held, already := seen[got]; already {
			t.Errorf("%q and %q produce one preimage, so an admin countersigning either has signed both",
				one.name, held)
		}
		seen[got] = one.name
	}
	// the label is a fixed prefix and is the spec's string, written out here rather than read
	// off the constant the function uses
	got := preimageOf([]byte("gid"), 7, []byte("sid"), 42)
	if want := "URmessage/v1/succession"; got[:len(want)] != want {
		t.Errorf("the preimage begins %q, want the spec A section 3.4 label %q", got[:len(want)], want)
	}
}

// TestSuccessionPreimageRefusesAFieldPastTheVectorLimit is the bound
// syntax.Writer.WriteOpaqueLP would have applied, applied by hand because that writer is
// unreachable from package mls.
//
// What the bound is FOR is not tidiness. Without it the prefix is uint32(len(x)), which on a
// sixty four bit platform truncates rather than refuses, and a truncated length is a preimage
// two different inputs share -- for a countersignature, an admin who signed the promotion of
// somebody they never saw. The refusal answers the codec's own sentinel, because it is the same
// refusal the codec would have made.
func TestSuccessionPreimageRefusesAFieldPastTheVectorLimit(t *testing.T) {
	overlong := make([]byte, syntax.MaxVectorLength+1)
	for _, one := range []struct {
		name              string
		groupId           []byte
		successorMemberId []byte
	}{
		{"the group id", overlong, []byte("sid")},
		{"the successor member id", []byte("gid"), overlong},
	} {
		got, err := successionPreimage(one.groupId, 7, one.successorMemberId, 42)
		if !errors.Is(err, syntax.ErrLengthExceedsMax) {
			t.Errorf("%s at %d octets answered %d octets and %v, want syntax.ErrLengthExceedsMax",
				one.name, syntax.MaxVectorLength+1, len(got), err)
		}
		if got != nil {
			t.Errorf("%s past the limit answered %d octets of preimage as well as a refusal",
				one.name, len(got))
		}
	}
	// and a field exactly AT the limit is written, so the comparison is the bound the codec
	// applies and not one octet either side of it
	atTheLimit := make([]byte, syntax.MaxVectorLength)
	if _, err := successionPreimage(atTheLimit, 7, []byte("sid"), 42); err != nil {
		t.Errorf("a group id of exactly %d octets was refused with %v", syntax.MaxVectorLength, err)
	}
}

// TestSuccessionPreimageAnswersAFreshRunEachCall keeps the two callers Task 21 will have from
// sharing an array.
//
// A preimage handed to a signer and then to a verifier is held across two calls, and a Writer
// reused between them would let the second overwrite what the first is still signing.
func TestSuccessionPreimageAnswersAFreshRunEachCall(t *testing.T) {
	first, err := successionPreimage([]byte("gid"), 7, []byte("sid"), 42)
	if err != nil {
		t.Fatalf("successionPreimage: %v", err)
	}
	second, err := successionPreimage([]byte("gid"), 7, []byte("sid"), 42)
	if err != nil {
		t.Fatalf("successionPreimage: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("two calls with one input answered %x and %x", first, second)
	}
	first[0] ^= 0xff
	if bytes.Equal(first, second) {
		t.Fatal("writing to one preimage changed the other, so the two calls share an array")
	}
}
