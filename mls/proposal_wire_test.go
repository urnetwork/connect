// The proposal wire codecs, and the two things about them that are not round trips.
//
// The arm check is derived off the struct rather than driven from a list of the arms somebody
// remembered, because the failure it prevents is a NEW arm added with no line in the count: the
// proposal encodes, the new arm is dropped on the floor, and the ProposalRef over it is the
// reference of a proposal that says something else.
//
// The other is the key package stand-in guard. This file is the reason mls/key_package.go
// exists ahead of the plan that owns it, so this is where the notice that says so is held to
// still being true.
package mls

import (
	"bytes"
	"errors"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

func TestProposalTypeIsSixteenBitsOnTheWire(t *testing.T) {
	proposal := Proposal{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: 3}}
	encoded, err := syntax.Marshal(&proposal)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := []byte{0x00, 0x03, 0x00, 0x00, 0x00, 0x03}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("encoded %x, want %x -- ProposalType must be uint16", encoded, want)
	}
}

func TestProposalRoundTripRemove(t *testing.T) {
	proposal := Proposal{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: 7}}
	encoded, err := syntax.Marshal(&proposal)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	decoded := Proposal{}
	if err := syntax.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Remove == nil || decoded.Remove.Removed != 7 {
		t.Fatalf("decoded %+v", decoded.Remove)
	}
	reencoded, err := syntax.Marshal(&decoded)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !bytes.Equal(encoded, reencoded) {
		t.Fatalf("re-encoded %x, want %x", reencoded, encoded)
	}
}

// The codec is not the profile gate. (*Profile).CheckProposalType refuses psk, reinit and
// external_init under the v1 profile; the codec must round trip all three or the `messages`
// vector family, which carries every registered type, fails for a reason that is a policy.
func TestProposalCodecAcceptsProfileRefusedTypes(t *testing.T) {
	proposal := Proposal{
		ProposalType: ProposalTypeExternalInit,
		ExternalInit: &ExternalInit{KemOutput: []byte{0x01, 0x02, 0x03}},
	}
	encoded, err := syntax.Marshal(&proposal)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded := Proposal{}
	if err := syntax.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.ExternalInit == nil || !bytes.Equal(decoded.ExternalInit.KemOutput, []byte{0x01, 0x02, 0x03}) {
		t.Fatalf("decoded %+v", decoded.ExternalInit)
	}
}

// GREASE: parsed and ignored, never generated.
func TestProposalPreservesUnknownTypeVerbatim(t *testing.T) {
	encoded := []byte{0x0a, 0x0a, 0xde, 0xad, 0xbe, 0xef}
	decoded := Proposal{}
	if err := syntax.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.ProposalType != ProposalType(0x0a0a) {
		t.Fatalf("proposal type %04x", decoded.ProposalType)
	}
	if decoded.UnknownType != ProposalType(0x0a0a) {
		t.Fatalf("unknown type %04x", decoded.UnknownType)
	}
	if !bytes.Equal(decoded.UnknownBody, []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Fatalf("unknown body %x", decoded.UnknownBody)
	}
	reencoded, err := syntax.Marshal(&decoded)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !bytes.Equal(reencoded, encoded) {
		t.Fatalf("re-encoded %x, want %x", reencoded, encoded)
	}
}

// TestAnUnregisteredProposalConsumesNothingOutsideItsOwnVectorRegion is the bound on the one
// decision in proposal_wire.go that reads to the end of its reader.
//
// Proposal.UnmarshalMLS keeps an unregistered type's body verbatim with ReadRaw(Remaining()),
// which is what makes GREASE round trip, and that is a real consumption: inside a Commit's
// proposals<V> the body ABSORBS the elements standing after it. A receiver applies one proposal
// where a peer that registered the type applies two, and because the absorbed bytes are
// re-emitted verbatim the commit re-encodes byte identically -- so no round trip and no
// canonicality property in this package can see it. Nothing here can fix that: a Proposal
// carries no length of its own, so an implementation that does not know the type does not know
// where it ends. The first half of this test states the consequence so that it is a decision
// somebody reads rather than a surprise somebody rediscovers.
//
// The second half is the part that CAN be lost. syntax.ReadVector hands its element decoder a
// ReadSub over exactly the declared region, so the blast radius is bounded by the vector's own
// length prefix and the commit's path -- the field standing after the vector -- is read from
// where it actually is. A change to how vectors are read could widen a swallowed proposal into
// a swallowed path with everything else in this package still green, which is why the bound is
// asserted here rather than assumed from the shape of ReadVector.
func TestAnUnregisteredProposalConsumesNothingOutsideItsOwnVectorRegion(t *testing.T) {
	commit := Commit{
		Proposals: []ProposalOrRef{
			{Type: ProposalOrRefTypeProposal, Proposal: &Proposal{
				ProposalType: ProposalType(0x0a0a),
				UnknownType:  ProposalType(0x0a0a),
				UnknownBody:  []byte{0xff},
			}},
			{Type: ProposalOrRefTypeReference, Reference: ProposalRef{0x01, 0x02, 0x03}},
		},
		Path: &UpdatePath{LeafNode: *testLeafNodeOfSource(LeafNodeSourceCommit)},
	}
	encoded, err := syntax.Marshal(&commit)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded := Commit{}
	if err := syntax.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(decoded.Proposals) != 1 {
		t.Fatalf("an unregistered proposal standing before a reference decoded to %d elements; how far an unregistered body reads has changed and the note above no longer describes this codec",
			len(decoded.Proposals))
	}

	if decoded.Path == nil {
		t.Fatal("the commit decoded with no path at all; the unregistered proposal body read past its own vector and took the presence octet with it")
	}
	if !bytes.Equal(decoded.Path.LeafNode.Signature, commit.Path.LeafNode.Signature) {
		t.Fatalf("the decoded path's leaf signature is %x and the commit was built with %x; the field after proposals<V> was not read from where that vector's own length prefix says it ends",
			decoded.Path.LeafNode.Signature, commit.Path.LeafNode.Signature)
	}

	// and the verbatim half: what was swallowed is re-emitted, which is exactly why nothing
	// else in this package can see the swallowing
	reencoded, err := syntax.Marshal(&decoded)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !bytes.Equal(reencoded, encoded) {
		t.Fatalf("re-encoded %x, want %x", reencoded, encoded)
	}
}

// The forge's malformed arm: a registered body under an unregistered discriminant, which is what
// the validation plan's ValSem tests need and what stops a second encoder being written to
// produce it.
func TestProposalUnknownTypeOverridesTheDiscriminant(t *testing.T) {
	proposal := Proposal{
		ProposalType: ProposalTypeRemove,
		Remove:       &Remove{Removed: 3},
		UnknownType:  ProposalType(0x0a0a),
	}
	encoded, err := syntax.Marshal(&proposal)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := []byte{0x0a, 0x0a, 0x00, 0x00, 0x00, 0x03}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("encoded %x, want %x", encoded, want)
	}
}

func TestProposalRejectsArmMismatch(t *testing.T) {
	proposal := Proposal{ProposalType: ProposalTypeRemove}
	if _, err := syntax.Marshal(&proposal); !errors.Is(err, ErrContentArmMismatch) {
		t.Fatalf("got %v, want ErrContentArmMismatch", err)
	}
}

// proposalArmFields is every field of Proposal that carries a BODY, derived off the type.
//
// The derivation is "a field that can hold something" -- a pointer or a slice -- and not a list
// of the seven arms plus UnknownBody, for the reason this project keeps rediscovering: a list
// written today understates the class the day somebody adds to it, and the way a dropped arm
// fails is silent. The two discriminants are ProposalType values and are excluded by the same
// reading rather than by name, so a discriminant renamed does not fall into the class and an
// eighth arm added does.
func proposalArmFields(t *testing.T) []string {
	t.Helper()
	proposal := reflect.TypeOf(Proposal{})
	found := []string{}
	for i := 0; i < proposal.NumField(); i += 1 {
		field := proposal.Field(i)
		switch field.Type.Kind() {
		case reflect.Pointer, reflect.Slice:
			found = append(found, field.Name)
		}
	}
	if len(found) < 2 {
		t.Fatalf("the derivation read %v out of Proposal, which is fewer arms than the type has, so the gate below would run over almost nothing", found)
	}
	slices.Sort(found)
	return found
}

// TestEveryArmOfAProposalIsCountedByItsArmCheck holds checkArm's hand written count to the type
// it is counting.
//
// The count is eight lines of source and the class it stands for is derived here, so an eighth
// arm added to Proposal with no line beside it fails on the commit that adds it. What that
// failure prevents is not a compile error and not a bad encoding: a proposal carrying two arms
// encodes as ONE of them, so two distinct Proposal values serialize identically and therefore
// carry the SAME ProposalRef -- and a reference two different proposals share is a commit that
// applies a change its proposer did not publish.
func TestEveryArmOfAProposalIsCountedByItsArmCheck(t *testing.T) {
	base := func() Proposal {
		return Proposal{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: 4}}
	}
	// the control: the base itself must encode, or every refusal below is a refusal of
	// something this test broke rather than of the second arm it added
	valid := base()
	if _, err := syntax.Marshal(&valid); err != nil {
		t.Fatalf("the one armed base proposal did not encode: %v", err)
	}
	refused := 0
	for _, name := range proposalArmFields(t) {
		if name == "Remove" {
			continue
		}
		second := base()
		field := reflect.ValueOf(&second).Elem().FieldByName(name)
		switch field.Kind() {
		case reflect.Pointer:
			field.Set(reflect.New(field.Type().Elem()))
		case reflect.Slice:
			field.Set(reflect.ValueOf([]byte{0x00}).Convert(field.Type()))
		default:
			t.Fatalf("%s came out of the derivation and is neither a pointer nor a slice", name)
		}
		if _, err := syntax.Marshal(&second); !errors.Is(err, ErrContentArmMismatch) {
			t.Errorf("a proposal carrying Remove and %s encoded with %v, and the %s it carries is nowhere in those bytes; the two values share one encoding and therefore one ProposalRef",
				name, err, name)
			continue
		}
		refused += 1
	}
	if refused == 0 {
		t.Fatal("no second arm was refused, so this gate observed nothing")
	}
	t.Logf("%d second arms refused, derived off the fields of Proposal", refused)
}

func TestProposalOrRefRoundTrip(t *testing.T) {
	cases := []ProposalOrRef{
		{Type: ProposalOrRefTypeProposal, Proposal: &Proposal{
			ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: 1}}},
		{Type: ProposalOrRefTypeReference, Reference: ProposalRef{0xaa, 0xbb, 0xcc}},
	}
	for i := range cases {
		encoded, err := syntax.Marshal(&cases[i])
		if err != nil {
			t.Fatalf("case %d: marshal: %v", i, err)
		}
		decoded := ProposalOrRef{}
		if err := syntax.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("case %d: unmarshal: %v", i, err)
		}
		reencoded, err := syntax.Marshal(&decoded)
		if err != nil {
			t.Fatalf("case %d: re-marshal: %v", i, err)
		}
		if !bytes.Equal(encoded, reencoded) {
			t.Fatalf("case %d: re-encoded %x, want %x", i, reencoded, encoded)
		}
	}
}

func TestProposalOrRefRejectsReservedType(t *testing.T) {
	decoded := ProposalOrRef{}
	err := syntax.Unmarshal([]byte{0x00}, &decoded)
	if !errors.Is(err, ErrUnknownProposalOrRefType) {
		t.Fatalf("got %v, want ErrUnknownProposalOrRefType", err)
	}
}

// An empty reference is wire legal and is the encoding of "no reference", so a commit carrying
// one names a proposal that cannot be looked up -- and names it identically to every other
// commit that made the same mistake, which is worse than naming nothing.
func TestProposalOrRefRefusesAnEmptyReference(t *testing.T) {
	empty := ProposalOrRef{Type: ProposalOrRefTypeReference, Reference: ProposalRef{}}
	if _, err := syntax.Marshal(&empty); !errors.Is(err, ErrContentArmMismatch) {
		t.Fatalf("got %v, want ErrContentArmMismatch", err)
	}
	// and the decoder agrees with the encoder about it, so what one refuses to write the
	// other refuses to read
	decoded := ProposalOrRef{}
	if err := syntax.Unmarshal([]byte{0x02, 0x00}, &decoded); err == nil {
		t.Fatalf("a reference of no octets decoded to %+v", decoded)
	}
}

// TestTheKeyPackageStandInDoesNotOutliveItsOwnersLanding is the self-deleting half of the notice
// at the top of mls/key_package.go.
//
// That file is the TreeKEM plan's task 7A and it is here early and INCOMPLETE, because the Add
// arm above carries a KeyPackage by value and package mls is one package. The notice says so and
// says that task 7A fills the file in rather than creating it. A notice describing a file it no
// longer describes is worse than none -- the next reader believes it -- so the moment anything
// beyond the codec lands in that file, this fails and asks for the notice to go.
//
// What counts as "the rest has landed" is DERIVED: every package level declaration of
// key_package.go that is not one of the four the stand-in declares. NewKeyPackage, Ref and
// Validate all trip it, and so does whatever else task 7A brings, without this test having to
// name them.
func TestTheKeyPackageStandInDoesNotOutliveItsOwnersLanding(t *testing.T) {
	const file = "key_package.go"
	const notice = "task 7A does not create this file, it FILLS IT IN"
	standIn := []string{"KeyPackage", "KeyPackage.marshalCore", "KeyPackage.MarshalMLS", "KeyPackage.UnmarshalMLS"}

	declared := packageLevelDeclarations(t, ".")
	for _, name := range standIn {
		if declared[name] != file {
			t.Fatalf("%s is declared in %q and this guard expects it in %s; the stand-in has been renamed or moved and this gate is reading the wrong file",
				name, declared[name], file)
		}
	}
	source, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	landed := []string{}
	for name, at := range declared {
		if at != file || slices.Contains(standIn, name) {
			continue
		}
		landed = append(landed, name)
	}
	slices.Sort(landed)
	if len(landed) > 0 {
		if strings.Contains(string(source), notice) {
			t.Errorf("%s now declares %v beyond the codec stand-in, so task 7A has landed and the notice saying it has not is still at the top of the file",
				file, landed)
		}
		return
	}
	if !strings.Contains(string(source), notice) {
		t.Fatalf("%s declares only the codec stand-in and no longer carries the notice saying whose file it is and what is missing from it",
			file)
	}
}

// TestKeyPackageOpensWithItsVersionAndThenItsCiphersuite is the field-order statement the codec
// stand-in owes, and it is here rather than in the file it is about because that file is another
// plan's and is filled in rather than created.
//
// Version and CipherSuite are ADJACENT uint16s. An encoder that writes them the other way round
// round trips its own output perfectly, produces a key package of exactly the right length, and
// disagrees with every other implementation -- which arrives as a joiner nobody can add, at the
// first Welcome shared with anybody. A round trip cannot see it and nothing else in this package
// was looking: the two fields swapped survived the whole of mls and message green, measured.
//
// The golden is derived from RFC 9420 section 10 rather than read back through the encoder: the
// version, the ciphersuite, then init_key as an opaque<V>. Only the prefix is written out, because
// what follows is a LeafNode whose own golden leaf_node_test.go already holds.
func TestKeyPackageOpensWithItsVersionAndThenItsCiphersuite(t *testing.T) {
	keyPackage := KeyPackage{
		Version:     ProtocolVersionMls10,
		CipherSuite: CipherSuiteX25519ChaCha20Sha256Ed25519,
		InitKey:     HpkePublicKey{0xaa, 0xbb},
		LeafNode:    *testLeafNodeOfSource(LeafNodeSourceKeyPackage),
		Signature:   []byte{0xcc},
	}
	encoded, err := syntax.Marshal(&keyPackage)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := []byte{
		0x00, 0x01, // version: mls10
		0x00, 0x03, // cipher_suite: X25519/ChaCha20/SHA-256/Ed25519
		0x02, 0xaa, 0xbb, // init_key<V>
	}
	if !bytes.HasPrefix(encoded, want) {
		t.Fatalf("a key package opens with %x, and RFC 9420 section 10 writes %x -- version, then cipher_suite, then init_key",
			encoded[:min(len(encoded), len(want))], want)
	}

	decoded := KeyPackage{}
	if err := syntax.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Version != keyPackage.Version || decoded.CipherSuite != keyPackage.CipherSuite {
		t.Fatalf("decoded version %d suite %d, want %d and %d",
			decoded.Version, decoded.CipherSuite, keyPackage.Version, keyPackage.CipherSuite)
	}
	reencoded, err := syntax.Marshal(&decoded)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !bytes.Equal(reencoded, encoded) {
		t.Fatalf("re-encoded %x, want %x", reencoded, encoded)
	}
}

// ---------------------------------------------------------------------------
// the layout RFC 9420 section 12.1 gives each registered arm
// ---------------------------------------------------------------------------

// proposalArmSample is one registered proposal arm: the field of Proposal it populates, a value
// carrying exactly that arm, and the octets section 12.1 writes it as.
type proposalArmSample struct {
	field    string
	proposal Proposal
	golden   []byte
}

// proposalArmDelegated is the encoding of a structure a proposal arm hands over to WHOLE.
//
// It is the one part of a golden below that is not written out octet by octet, and what it
// claims is a delegation and an offset rather than a layout: an Add carries a KeyPackage exactly
// as key_package.go writes one, beginning at the octet after the discriminant, with nothing of
// this file's own wrapped around it. Those structures' layouts are held by their own goldens --
// TestKeyPackageOpensWithItsVersionAndThenItsCiphersuite above, leaf_node_test.go's hand derived
// forms, psk_test.go's -- and transcribing them again here would be a second opinion that agrees
// with whichever of the two was written later.
func proposalArmDelegated(t *testing.T, codec syntax.Codec) []byte {
	t.Helper()
	encoded, err := syntax.Marshal(codec)
	if err != nil {
		t.Fatalf("encode the structure a proposal arm delegates to: %v", err)
	}
	return encoded
}

// proposalArmSamples is one Proposal per registered arm together with the octets RFC 9420
// section 12.1 writes it as.
//
// The goldens are derived from the RFC and stated here rather than read back through the encoder
// under test, and that is the whole point of the gate below. Five of the seven registered arms
// were never encoded AND decoded by anything in this package: ReInit's Version and CipherSuite,
// two ADJACENT uint16s, could be swapped on BOTH the encode and the decode side -- a layout that
// round trips its own output perfectly, produces a proposal of exactly the right length, and
// disagrees with every other implementation in the world -- and the whole of ./mls/... and
// ./message/... stayed green. Measured, not supposed.
//
// It is the same defect KeyPackage's adjacent Version and CipherSuite carried, and the reason it
// is answered here as a TABLE over the derived arm set rather than as a second one-off test is
// that a one-off test is what the first one was. A ProposalRef is a hash of the whole encoding,
// so two implementations disagreeing about any arm's layout compute different references for one
// proposal -- the "stable, self consistent and wrong" fork framing_preimage.go's own comment says
// the reference exists to prevent.
func proposalArmSamples(t *testing.T) map[ProposalType]proposalArmSample {
	t.Helper()
	// one extensions<V> for the two arms that carry one, and the octets RFC 9420 section 6.3.1
	// writes it as. The vector prefix counts BYTES and not entries, which is the single easiest
	// thing in this encoding to get wrong and the thing a round trip cannot see.
	extensions := []Extension{{ExtensionType: ExtensionTypeApplicationId, ExtensionData: []byte{0x42}}}
	extensionOctets := []byte{
		0x04,       // extensions<V>: four octets of entries follow
		0x00, 0x01, // extension_type: application_id
		0x01, 0x42, // extension_data<V>
	}
	keyPackage := KeyPackage{
		Version:     ProtocolVersionMls10,
		CipherSuite: CipherSuiteX25519ChaCha20Sha256Ed25519,
		InitKey:     HpkePublicKey{0xaa, 0xbb},
		LeafNode:    *testLeafNodeOfSource(LeafNodeSourceKeyPackage),
		Signature:   []byte{0xcc},
	}
	leafNode := *testLeafNodeOfSource(LeafNodeSourceUpdate)
	psk := PreSharedKeyId{PskType: PskTypeExternal, PskId: []byte{0x51, 0x52}, PskNonce: []byte{0x61}}
	return map[ProposalType]proposalArmSample{
		ProposalTypeAdd: {
			field:    "Add",
			proposal: Proposal{ProposalType: ProposalTypeAdd, Add: &Add{KeyPackage: keyPackage}},
			golden:   append([]byte{0x00, 0x01}, proposalArmDelegated(t, &keyPackage)...),
		},
		ProposalTypeUpdate: {
			field:    "Update",
			proposal: Proposal{ProposalType: ProposalTypeUpdate, Update: &Update{LeafNode: leafNode}},
			golden:   append([]byte{0x00, 0x02}, proposalArmDelegated(t, &leafNode)...),
		},
		ProposalTypeRemove: {
			field:    "Remove",
			proposal: Proposal{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: 7}},
			golden: []byte{
				0x00, 0x03, // proposal_type: remove
				0x00, 0x00, 0x00, 0x07, // removed: uint32, and not the leaf index's own width
			},
		},
		ProposalTypePreSharedKey: {
			field:    "PreSharedKey",
			proposal: Proposal{ProposalType: ProposalTypePreSharedKey, PreSharedKey: &PreSharedKey{Psk: psk}},
			golden:   append([]byte{0x00, 0x04}, proposalArmDelegated(t, &psk)...),
		},
		ProposalTypeReInit: {
			field: "ReInit",
			proposal: Proposal{ProposalType: ProposalTypeReInit, ReInit: &ReInit{
				GroupId:     []byte{0x9a, 0x9b, 0x9c},
				Version:     ProtocolVersionMls10,
				CipherSuite: CipherSuiteX25519ChaCha20Sha256Ed25519,
				Extensions:  extensions,
			}},
			// section 12.1.5: group_id<V>, then version, then cipher_suite, then extensions.
			// The two uint16s are the pair this whole table exists for -- mls10 is 0x0001 and
			// the suite is 0x0003, chosen to differ, because a version and a ciphersuite that
			// happened to be equal would let the swap through.
			golden: joinBytes(
				[]byte{0x00, 0x05},
				[]byte{0x03, 0x9a, 0x9b, 0x9c},
				[]byte{0x00, 0x01},
				[]byte{0x00, 0x03},
				extensionOctets,
			),
		},
		ProposalTypeExternalInit: {
			field: "ExternalInit",
			proposal: Proposal{ProposalType: ProposalTypeExternalInit,
				ExternalInit: &ExternalInit{KemOutput: []byte{0x01, 0x02, 0x03}}},
			golden: []byte{
				0x00, 0x06, // proposal_type: external_init
				0x03, 0x01, 0x02, 0x03, // kem_output<V>
			},
		},
		ProposalTypeGroupContextExtensions: {
			field: "GroupContextExtensions",
			proposal: Proposal{ProposalType: ProposalTypeGroupContextExtensions,
				GroupContextExtensions: &GroupContextExtensions{Extensions: extensions}},
			golden: append([]byte{0x00, 0x07}, extensionOctets...),
		},
	}
}

// TestEveryRegisteredProposalArmEncodesToTheLayoutSection121Writes holds every arm of this
// codec to the RFC, in both directions, over a class derived twice.
//
// The table is joined to the ProposalType registry AND to the arm fields of the struct, so an
// eighth arm added to Proposal, or an eighth code point registered in extension.go, fails here
// on the commit that lands it rather than shipping as an arm nothing ever encoded. Before this
// gate only two of the seven were ever both encoded and decoded, and TestProposalCodecAccepts-
// ProfileRefusedTypes -- the one test that reaches past Remove -- covers ExternalInit alone.
//
// The re-encode is not redundant with the encode. A swap present on both sides of the codec is
// caught by the first comparison; a swap present on the DECODE side only round trips nothing and
// is caught by the second; and an arm the decoder drops on the floor is caught by the arm check
// between them.
func TestEveryRegisteredProposalArmEncodesToTheLayoutSection121Writes(t *testing.T) {
	samples := proposalArmSamples(t)
	covered := slices.Sorted(maps.Keys(samples))

	// the code point join. The reserved zero is excluded by its VALUE and not by its name: it is
	// not an arm, because checkArm's default clause treats it as an unregistered type whose body
	// is carried verbatim, which is the GREASE path and not a layout.
	declared := []ProposalType{}
	for _, value := range registryConstantsOfType(t, "ProposalType") {
		if value == uint64(ProposalTypeReserved) {
			continue
		}
		declared = append(declared, ProposalType(value))
	}
	slices.Sort(declared)
	if !slices.Equal(declared, covered) {
		t.Fatalf("package mls registers the proposal types %v and this table lays out %v; an arm with no golden is an arm no test has ever encoded and decoded, and its layout is whatever the encoder happens to do",
			declared, covered)
	}

	// the arm join, off the same derivation TestEveryArmOfAProposalIsCountedByItsArmCheck uses.
	// UnknownBody is excluded because it is not a registered arm: it is the verbatim body an
	// unregistered type carries, and it has no layout of its own to be wrong about.
	arms := []string{}
	for _, name := range proposalArmFields(t) {
		if name == "UnknownBody" {
			continue
		}
		arms = append(arms, name)
	}
	laid := []string{}
	for _, proposalType := range covered {
		laid = append(laid, samples[proposalType].field)
	}
	slices.Sort(laid)
	if !slices.Equal(arms, laid) {
		t.Fatalf("Proposal carries the arms %v and this table populates %v; the two derivations disagree, so one of them has stopped describing the type",
			arms, laid)
	}

	for _, proposalType := range covered {
		sample := samples[proposalType]
		// the row is what it says it is, before anything is read off it
		field := reflect.ValueOf(sample.proposal).FieldByName(sample.field)
		if !field.IsValid() || field.IsNil() {
			t.Fatalf("the %s row does not populate the arm it names, so whatever it encodes to is not that arm", sample.field)
		}
		encoded, err := syntax.Marshal(&sample.proposal)
		if err != nil {
			t.Errorf("%s: marshal: %v", sample.field, err)
			continue
		}
		if !bytes.Equal(encoded, sample.golden) {
			t.Errorf("a %s proposal encodes to %x and RFC 9420 section 12.1 writes %x; a layout that disagrees with the RFC and agrees with itself produces a different ProposalRef for the same proposal, so every peer computes a name for it that this one does not",
				sample.field, encoded, sample.golden)
			continue
		}
		decoded := Proposal{}
		if err := syntax.Unmarshal(sample.golden, &decoded); err != nil {
			t.Errorf("%s: the published layout did not decode: %v", sample.field, err)
			continue
		}
		if decoded.ProposalType != proposalType {
			t.Errorf("%s: the layout decoded as proposal type %04x, want %04x", sample.field, decoded.ProposalType, proposalType)
			continue
		}
		if decodedArm := reflect.ValueOf(decoded).FieldByName(sample.field); !decodedArm.IsValid() || decodedArm.IsNil() {
			t.Errorf("%s: the layout decoded with that arm empty, so the decoder read the discriminant and dropped the body", sample.field)
			continue
		}
		reencoded, err := syntax.Marshal(&decoded)
		if err != nil {
			t.Errorf("%s: re-marshal: %v", sample.field, err)
			continue
		}
		if !bytes.Equal(reencoded, sample.golden) {
			t.Errorf("a %s proposal decoded from %x re-encodes to %x; the two halves of this codec disagree about that arm's layout, which is the one shape a round trip over the encoder's own output cannot see",
				sample.field, sample.golden, reencoded)
		}
	}
	t.Logf("%d registered proposal arms, each encoded to and decoded from the layout section 12.1 gives it", len(covered))
}

// ---------------------------------------------------------------------------
// the code points, read twice out of the RFC
// ---------------------------------------------------------------------------

// rfc9420Section175ProposalTypes is the "MLS Proposal Types" registry of RFC 9420 section 17.5,
// transcribed out of its Initial Contents table:
//
//	| Value    | Name                     | R | Ext | Path | Ref      |
//	| 0x0000   | RESERVED                 | - | -   | -    | RFC 9420 |
//	| 0x0001   | add                      | Y | Y   | N    | RFC 9420 |
//	| 0x0002   | update                   | Y | N   | Y    | RFC 9420 |
//	| 0x0003   | remove                   | Y | Y   | Y    | RFC 9420 |
//	| 0x0004   | psk                      | Y | Y   | N    | RFC 9420 |
//	| 0x0005   | reinit                   | Y | Y   | N    | RFC 9420 |
//	| 0x0006   | external_init            | Y | N   | Y    | RFC 9420 |
//	| 0x0007   | group_context_extensions | Y | Y   | Y    | RFC 9420 |
//
// The table's own RESERVED is folded to lower case so the whole map is one spelling convention
// and a Go constant name can be folded onto it; no other character of any row is touched. The
// GREASE rows -- 0x0A0A, 0x1A1A and the rest of that ladder -- are deliberately absent: they are
// registered as reserved-for-GREASE rather than as proposals, they name no arm, and the sweep
// below covers them along with every other code point that names no arm.
var rfc9420Section175ProposalTypes = map[string]uint64{
	"reserved":                 0x0000,
	"add":                      0x0001,
	"update":                   0x0002,
	"remove":                   0x0003,
	"psk":                      0x0004,
	"reinit":                   0x0005,
	"external_init":            0x0006,
	"group_context_extensions": 0x0007,
}

// rfc9420Section72DefaultProposalTypes is the SECOND reading of the same seven assignments, out
// of RFC 9420 section 7.2's default list:
//
//	The following proposal and extension types are considered "default" and MUST NOT be
//	listed:
//
//	*  Proposal types:
//	   -  0x0001 - add
//	   -  0x0002 - update
//	   -  0x0003 - remove
//	   -  0x0004 - psk
//	   -  0x0005 - reinit
//	   -  0x0006 - external_init
//	   -  0x0007 - group_context_extensions
//
// Two transcriptions rather than one, for the reason the extension registry already carries one:
// a code point pinned ONCE agrees with whatever the transcriber believed, and this package has
// already shipped a constant declared at its neighbour's code point defended by a pin written
// from the same misreading. Section 7.2 and section 17.5 are different pages written for
// different purposes, so a hand that slipped on one of them did not slip the same way on both.
var rfc9420Section72DefaultProposalTypes = map[string]uint64{
	"add":                      0x0001,
	"update":                   0x0002,
	"remove":                   0x0003,
	"psk":                      0x0004,
	"reinit":                   0x0005,
	"external_init":            0x0006,
	"group_context_extensions": 0x0007,
}

// proposalTypeRfcNameAliases is the two constants whose Go spelling does not fold to the RFC's
// own name for the same code point. It is a NAME map and carries no value, which is what makes
// it safe to write by hand: an alias that is wrong joins a constant to a different registry row
// and fails the comparison, rather than agreeing with an error the way a value pin would.
//
// The gate below holds it to being exactly the set of names that do not fold on their own, so a
// stale entry and a missing one both fail on the commit that creates them.
var proposalTypeRfcNameAliases = map[string]string{
	"pre_shared_key": "psk",
	"re_init":        "reinit",
}

// rfcProposalTypeName is a declared constant's name folded to the RFC's spelling of the same
// code point, through the shared fold and then the alias.
func rfcProposalTypeName(constantName string) string {
	folded := rfcNameOfRegistryConstant("ProposalType", constantName)
	if alias, aliased := proposalTypeRfcNameAliases[folded]; aliased {
		return alias
	}
	return folded
}

// TestEveryProposalTypeCodePointIsPinnedByTwoIndependentReadingsOfTheRfc joins the proposal type
// registry to the RFC twice, on the NAME, in both directions, over the package's constants and
// over the single-transcription pin that already stands beside them.
//
// The pin in extension_test.go is one person reading one page and typing values beside Go
// identifiers. It catches a constant edited afterwards and nothing else: a constant declared at
// its neighbour's code point from the beginning is defended by a pin written from the same
// belief, which is exactly the state ExtensionTypeExternalSenders was in at external_pub's
// 0x0004. So the pin is joined here too, against a reading of a different page, and a swap
// present in BOTH the source and the pin fails.
//
// A swapped proposal code point is not a compile error and not a bad encoding. It makes this
// implementation write reinit where every peer writes external_init, and read a peer's
// external_init as a reinit -- a proposal whose body is a different structure, under a
// discriminant that decides which. Nothing that round trips its own output can see it.
func TestEveryProposalTypeCodePointIsPinnedByTwoIndependentReadingsOfTheRfc(t *testing.T) {
	// the two readings against each other first. Every name section 7.2 lists must be in the
	// section 17.5 registry at the same value, and the only row 17.5 may hold beyond them is
	// the reserved zero, which is not a "default proposal type" and has no section 7.2 line.
	for _, name := range slices.Sorted(maps.Keys(rfc9420Section72DefaultProposalTypes)) {
		listed := rfc9420Section72DefaultProposalTypes[name]
		registered, held := rfc9420Section175ProposalTypes[name]
		if !held {
			t.Fatalf("RFC 9420 section 7.2 names the default proposal type %s at %#04x and the section 17.5 transcription has no row spelling it; one of the two readings has been mistyped and the join below would run one code point short",
				name, listed)
		}
		if registered != listed {
			t.Fatalf("%s is %#04x read out of section 7.2 and %#04x read out of section 17.5; the two transcriptions of one registry disagree",
				name, listed, registered)
		}
	}
	beyondTheDefaults := []string{}
	for name := range rfc9420Section175ProposalTypes {
		if _, isDefault := rfc9420Section72DefaultProposalTypes[name]; !isDefault {
			beyondTheDefaults = append(beyondTheDefaults, name)
		}
	}
	slices.Sort(beyondTheDefaults)
	if !slices.Equal(beyondTheDefaults, []string{"reserved"}) {
		t.Fatalf("the section 17.5 transcription holds %v beyond section 7.2's default list, and the reserved zero is the only row that may stand outside it",
			beyondTheDefaults)
	}

	// the alias table, held to exactly the names that do not fold on their own
	declared := registryConstantsOfType(t, "ProposalType")
	needed := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		folded := rfcNameOfRegistryConstant("ProposalType", name)
		if _, listed := rfc9420Section175ProposalTypes[folded]; listed {
			continue
		}
		alias, aliased := proposalTypeRfcNameAliases[folded]
		if !aliased {
			t.Fatalf("%s folds to the RFC name %s, which RFC 9420 section 17.5 does not register, and no alias says what the RFC calls that code point; an unjoinable constant is one this gate cannot cross check",
				name, folded)
		}
		if _, listed := rfc9420Section175ProposalTypes[alias]; !listed {
			t.Fatalf("the alias %s -> %s names no row of the section 17.5 registry", folded, alias)
		}
		needed[folded] = alias
	}
	if !maps.Equal(needed, proposalTypeRfcNameAliases) {
		t.Fatalf("the constants of ProposalType need the aliases %v and this file holds %v; an alias for a name nothing folds to is a join key nothing uses, and a missing one drops a constant out of the join",
			needed, proposalTypeRfcNameAliases)
	}

	// and the join, over both the declarations and the pin that guards them
	for _, source := range []struct {
		what      string
		constants map[string]uint64
	}{
		{what: "package mls", constants: declared},
		{what: "the registryCodePoints pin", constants: registryCodePoints["ProposalType"]},
	} {
		if len(source.constants) == 0 {
			t.Fatalf("%s holds no ProposalType constant, so this join ran over nothing", source.what)
		}
		byRfcName := map[string]uint64{}
		for name, value := range source.constants {
			rfcName := rfcProposalTypeName(name)
			if other, repeated := byRfcName[rfcName]; repeated {
				t.Fatalf("%s: two constants fold to the RFC name %s, at %#04x and %#04x, so the join is ambiguous",
					source.what, rfcName, other, value)
			}
			byRfcName[rfcName] = value
		}
		if !maps.Equal(byRfcName, rfc9420Section175ProposalTypes) {
			t.Fatalf("%s reads the proposal type registry as\n %v\nand RFC 9420 section 17.5 assigns\n %v",
				source.what, byRfcName, rfc9420Section175ProposalTypes)
		}
	}
	t.Logf("%d proposal type code points joined on the name across two readings of RFC 9420, over the declarations and over the pin",
		len(rfc9420Section175ProposalTypes))
}

// ---------------------------------------------------------------------------
// the whole code point space, on both halves
// ---------------------------------------------------------------------------

// TestEveryProposalTypeWithNoArmIsCarriedVerbatimOverTheWholeSixteenBitSpace sweeps all 65536
// proposal type code points rather than the one GREASE value a case would have probed.
//
// The class is DERIVED: the width of the registry against the code points that name an arm, so
// the ladder of GREASE values, the reserved zero, and every unassigned point in between are all
// in it without being listed. p6 task 1 did this for SenderType over its 252 undeclared octets
// and this is the same derivation one registry wider -- the registry is 16 bits, which is why
// an 8-bit implementation of it encodes every proposal one octet short.
//
// What is swept is the opposite property from SenderType's, and that is the point of stating it.
// A sender type outside the registry is REFUSED; a proposal type outside it is CARRIED, verbatim
// and re-encoded exactly, because GREASE is parsed and ignored rather than rejected. A codec
// that refused one of them refuses a peer that generates it, and a codec that carried a
// REGISTERED one -- the complement at the end -- drops a real proposal's arm on the floor while
// re-encoding byte identically, which no round trip anywhere in this package can see.
func TestEveryProposalTypeWithNoArmIsCarriedVerbatimOverTheWholeSixteenBitSpace(t *testing.T) {
	armed := map[ProposalType]bool{}
	for _, value := range registryConstantsOfType(t, "ProposalType") {
		// the reserved zero is excluded by its VALUE and not by its name, on the terms the arm
		// table above already uses: it names no arm, so it belongs to the verbatim class.
		if value == uint64(ProposalTypeReserved) {
			continue
		}
		armed[ProposalType(value)] = true
	}
	if len(armed) == 0 {
		t.Fatal("no proposal type was derived as naming an arm, so this sweep would run over the whole space and say nothing about the seven arms")
	}

	body := []byte{0xde, 0xad}
	carried := 0
	for candidate := 0; candidate <= 0xffff; candidate += 1 {
		proposalType := ProposalType(candidate)
		if armed[proposalType] {
			continue
		}
		encoded := []byte{byte(candidate >> 8), byte(candidate), body[0], body[1]}

		decoded := Proposal{}
		if err := syntax.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("the proposal type %#04x, which names no arm, was refused with %v; GREASE is parsed and ignored, and a codec that refuses an unregistered type cannot round trip a peer that generates one",
				candidate, err)
		}
		if decoded.ProposalType != proposalType {
			t.Fatalf("%#04x decoded as proposal type %#04x", candidate, decoded.ProposalType)
		}
		if decoded.UnknownType != proposalType {
			t.Fatalf("%#04x decoded with unknown type %#04x; without it the re-encode falls back to the arm discriminant and the code point the peer sent is lost",
				candidate, decoded.UnknownType)
		}
		if !bytes.Equal(decoded.UnknownBody, body) {
			t.Fatalf("%#04x decoded with the verbatim body %x, want %x", candidate, decoded.UnknownBody, body)
		}
		reencoded, err := syntax.Marshal(&decoded)
		if err != nil {
			t.Fatalf("%#04x: re-marshal: %v", candidate, err)
		}
		if !bytes.Equal(reencoded, encoded) {
			t.Fatalf("%#04x re-encoded to %x, want %x", candidate, reencoded, encoded)
		}

		// the encode half, built rather than read back: the discriminant override is what emits
		// a body under a code point this package does not register, and it is stated over the
		// whole space rather than at one GREASE value for the same reason the decode half is.
		built := Proposal{ProposalType: proposalType, UnknownType: proposalType, UnknownBody: body}
		builtEncoded, err := syntax.Marshal(&built)
		if err != nil {
			t.Fatalf("%#04x: marshal: %v", candidate, err)
		}
		if !bytes.Equal(builtEncoded, encoded) {
			t.Fatalf("%#04x encoded to %x, want %x", candidate, builtEncoded, encoded)
		}
		carried += 1
	}
	if carried+len(armed) != 1<<16 {
		t.Fatalf("the derivation split the %d proposal type code points into %d naming an arm and %d carried verbatim",
			1<<16, len(armed), carried)
	}

	// the complement, over the arms' own published layouts: a registered type must be read AS
	// its arm and never as a verbatim body.
	for proposalType, sample := range proposalArmSamples(t) {
		decoded := Proposal{}
		if err := syntax.Unmarshal(sample.golden, &decoded); err != nil {
			t.Fatalf("%s: the published layout did not decode: %v", sample.field, err)
		}
		if decoded.UnknownType != ProposalTypeReserved || decoded.UnknownBody != nil {
			t.Fatalf("a %s proposal decoded with unknown type %#04x and the verbatim body %x; the registered code point %#04x was read as an unregistered one, and a proposal read that way re-encodes byte identically with its arm dropped",
				sample.field, decoded.UnknownType, decoded.UnknownBody, proposalType)
		}
	}
	t.Logf("%d proposal type code points carried verbatim on both halves, %d read as arms", carried, len(armed))
}

// rfc9420Section124ProposalOrRefTypes is the enum of RFC 9420 section 12.4, transcribed:
//
//	enum {
//	  reserved(0),
//	  proposal(1),
//	  reference(2),
//	  (255)
//	} ProposalOrRefType;
var rfc9420Section124ProposalOrRefTypes = map[string]uint64{
	"reserved":  0,
	"proposal":  1,
	"reference": 2,
}

// rfc9420Section124ProposalOrRefSelectCases is the second reading of the same registry, out of
// the structure that consumes it four lines further down the same section:
//
//	struct {
//	  ProposalOrRefType type;
//	  select (ProposalOrRef.type) {
//	    case proposal:  Proposal proposal;
//	    case reference: ProposalRef reference;
//	  };
//	} ProposalOrRef;
//
// It carries no values, and that is what it is for: it says WHICH members of the enum name a
// body, which is the difference between the two the enum itself does not state. reserved(0) is
// a member of the registry and names no case, so it is refused like every unassigned octet
// rather than being special.
var rfc9420Section124ProposalOrRefSelectCases = []string{"proposal", "reference"}

// TestProposalOrRefRefusesEveryCodePointThatNamesNoArmOnBothHalves sweeps the whole octet space
// of ProposalOrRefType on both halves of the codec.
//
// Two things this replaces. The reserved zero was the only refusal stated, on the DECODE half
// only, which left 253 octets and the entire encode half unobserved -- and the encode half is
// the one with teeth here, because a ProposalOrRef goes inside a Commit inside a FramedContent
// that is signed, so an encoder that wrote an unregistered discriminant produces signed bytes
// no peer can attribute to any proposal.
//
// The accepted class is derived from the RFC and deliberately NOT from this package's own
// switch. A class read off the code under test shrinks by exactly the case a mutation adds,
// which is a gate that agrees with whatever the code does.
func TestProposalOrRefRefusesEveryCodePointThatNamesNoArmOnBothHalves(t *testing.T) {
	accepted := map[uint64]bool{}
	for _, name := range rfc9420Section124ProposalOrRefSelectCases {
		value, held := rfc9420Section124ProposalOrRefTypes[name]
		if !held {
			t.Fatalf("the section 12.4 select names the case %s and the enum transcription beside it has no member spelling that; the two readings describe different registries",
				name)
		}
		accepted[value] = true
	}
	if len(accepted) != len(rfc9420Section124ProposalOrRefSelectCases) {
		t.Fatalf("the %d select cases resolved to %d distinct code points", len(rfc9420Section124ProposalOrRefSelectCases), len(accepted))
	}

	// this package's constants joined to the enum by name, both directions at once, so the
	// sweep below runs over the octets the RFC leaves without a body rather than over the
	// octets this package happens not to have declared.
	byRfcName := map[string]uint64{}
	for name, value := range registryConstantsOfType(t, "ProposalOrRefType") {
		rfcName := rfcNameOfRegistryConstant("ProposalOrRefType", name)
		if other, repeated := byRfcName[rfcName]; repeated {
			t.Fatalf("two constants fold to the RFC name %s, at %d and %d", rfcName, other, value)
		}
		byRfcName[rfcName] = value
	}
	if !maps.Equal(byRfcName, rfc9420Section124ProposalOrRefTypes) {
		t.Fatalf("package mls reads ProposalOrRefType as %v and RFC 9420 section 12.4 writes %v", byRfcName, rfc9420Section124ProposalOrRefTypes)
	}
	if bits := framingRegistryBits(t, "ProposalOrRefType"); bits != 8 {
		t.Fatalf("ProposalOrRefType is %d bits wide and section 12.4 writes it as one octet, so this sweep is over the wrong space", bits)
	}

	refused := 0
	for candidate := 0; candidate <= 0xff; candidate += 1 {
		if accepted[uint64(candidate)] {
			continue
		}
		// BOTH arms are populated, so what the encoder refuses is the TYPE. With neither arm
		// set an encoder that had lost its discriminant check would still refuse, for the
		// missing body, and this sweep would pass over an encoder that writes anything.
		unencodable := ProposalOrRef{
			Type:      ProposalOrRefType(candidate),
			Proposal:  &Proposal{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: 1}},
			Reference: ProposalRef{0x01, 0x02},
		}
		if _, err := syntax.Marshal(&unencodable); !errors.Is(err, ErrUnknownProposalOrRefType) {
			t.Fatalf("encoding ProposalOrRef type %#02x with both arms populated: got %v, want ErrUnknownProposalOrRefType", candidate, err)
		}
		// each octet twice, bare and with a tail, so a refusal that was really a truncation
		// cannot pass for a refusal of the discriminant
		for _, encoded := range [][]byte{{byte(candidate)}, {byte(candidate), 0xde, 0xad, 0xbe, 0xef}} {
			decoded := ProposalOrRef{}
			if err := syntax.Unmarshal(encoded, &decoded); !errors.Is(err, ErrUnknownProposalOrRefType) {
				t.Fatalf("decoding %x: got %v, want ErrUnknownProposalOrRefType", encoded, err)
			}
		}
		refused += 1
	}
	if refused+len(accepted) != 1<<8 {
		t.Fatalf("the derivation split the %d ProposalOrRefType code points into %d with a body and %d refused",
			1<<8, len(accepted), refused)
	}
	t.Logf("%d ProposalOrRefType code points refused on both halves of the codec", refused)
}

// ---------------------------------------------------------------------------
// full consumption
// ---------------------------------------------------------------------------

// TestProposalAndProposalOrRefConsumeExactlyTheirOwnOctets is the full consumption statement
// both codecs owe, over every registered arm rather than over the one somebody picked.
//
// A Proposal is hashed WHOLE to make a ProposalRef, and a ProposalOrRef sits inside a Commit
// inside a signed FramedContent. A decoder that tolerated a tail would accept two octet strings
// as one proposal, which is two encodings under one reference and one signature covering only
// one of them. The refusal comes from syntax.UnmarshalLimit joining r.Done() rather than from
// anything in proposal_wire.go, which is exactly why it is stated here: that join has been
// dropped before, and the files whose tests noticed were somebody else's.
//
// The proper prefixes are the other side of the same length. They are swept whole rather than
// cut at the boundaries somebody thought of, because a decoder that read an arm short leaves a
// partly populated structure that then re-encodes to something the sender never wrote.
func TestProposalAndProposalOrRefConsumeExactlyTheirOwnOctets(t *testing.T) {
	type consuming struct {
		name   string
		golden []byte
		fresh  func() syntax.Codec
	}
	cases := []consuming{}
	for _, sample := range proposalArmSamples(t) {
		cases = append(cases, consuming{
			name:   "Proposal/" + sample.field,
			golden: sample.golden,
			fresh:  func() syntax.Codec { return &Proposal{} },
		})
	}
	for _, one := range []struct {
		name  string
		value ProposalOrRef
	}{
		{name: "ProposalOrRef/proposal", value: ProposalOrRef{Type: ProposalOrRefTypeProposal,
			Proposal: &Proposal{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: 7}}}},
		{name: "ProposalOrRef/reference", value: ProposalOrRef{Type: ProposalOrRefTypeReference,
			Reference: ProposalRef{0xaa, 0xbb, 0xcc}}},
	} {
		encoded, err := syntax.Marshal(&one.value)
		if err != nil {
			t.Fatalf("%s: marshal: %v", one.name, err)
		}
		cases = append(cases, consuming{
			name:   one.name,
			golden: encoded,
			fresh:  func() syntax.Codec { return &ProposalOrRef{} },
		})
	}
	slices.SortFunc(cases, func(a consuming, b consuming) int { return strings.Compare(a.name, b.name) })

	tails, cuts := 0, 0
	for _, one := range cases {
		// the control: the golden itself decodes, or every refusal below is a refusal of
		// something this table built wrong
		if err := syntax.Unmarshal(one.golden, one.fresh()); err != nil {
			t.Fatalf("%s: the golden itself was refused (%v), so nothing below proves anything", one.name, err)
		}
		for _, tail := range [][]byte{{0x00}, {0xff}, {0x00, 0x00}, repeatByte(0x5a, 17)} {
			longer := joinBytes(one.golden, tail)
			if err := syntax.Unmarshal(longer, one.fresh()); !errors.Is(err, syntax.ErrTrailingBytes) {
				t.Errorf("%s with %d trailing octets (%x): err = %v, want syntax.ErrTrailingBytes",
					one.name, len(tail), longer, err)
				continue
			}
			tails += 1
		}
		for cut := 0; cut < len(one.golden); cut += 1 {
			if err := syntax.Unmarshal(one.golden[:cut], one.fresh()); err == nil {
				t.Errorf("%s: %d octets of %d decoded rather than being refused", one.name, cut, len(one.golden))
				continue
			}
			cuts += 1
		}
	}
	if tails == 0 || cuts == 0 {
		t.Fatalf("%d tails and %d truncations were judged, so one half of this observed nothing", tails, cuts)
	}

	// the one exception, stated so it is a decision somebody reads rather than a hole somebody
	// finds. Under a code point that names no arm there is no full consumption to enforce: the
	// body has no length of its own, so everything left IS the body. That is the same
	// ReadRaw(Remaining()) whose blast radius the vector region test above bounds.
	absorbed := Proposal{}
	tailAbsorbing := []byte{0x0a, 0x0a, 0xde, 0xad, 0x00}
	if err := syntax.Unmarshal(tailAbsorbing, &absorbed); err != nil {
		t.Fatalf("a proposal under an unregistered code point with a trailing octet was refused: %v", err)
	}
	if !bytes.Equal(absorbed.UnknownBody, []byte{0xde, 0xad, 0x00}) {
		t.Fatalf("the unregistered body came back as %x, want the whole of the remainder", absorbed.UnknownBody)
	}
	t.Logf("%d tails refused and %d truncations refused across %d encodings", tails, cuts, len(cases))
}

// TestEveryProposalTypeRefusesEveryArmThatIsNotItsOwn states the other direction of the arm
// check, over the CROSS PRODUCT of the registered types with the registered arms rather than
// over the single pair the round trips probe.
//
// checkArm asks two questions and only one of them was observed anywhere in this file. The count
// answers "exactly one arm", and TestEveryArmOfAProposalIsCountedByItsArmCheck holds that count
// to the type. What no test here said is WHICH one: a proposal whose type names Add and whose
// single populated arm is Remove satisfies the count, and the per-case nil check is the only
// thing standing between it and self.Add.KeyPackage.MarshalMLS(w) on a nil pointer.
//
// Deleting any one of those seven checks leaves every arm test in this file green -- measured --
// and is noticed only by TestProposalRefsAreDistinctAcrossTheGeneratedProposalSpace over in
// framing_test.go, which does not report a missing refusal: it PANICS on the nil dereference,
// from a stack trace in a file about something else. That is the wrong way to learn this, and it
// is the shape p7's group lifecycle will reach, since it assembles proposals rather than
// decoding them. So the refusal is stated here over the cross product, and the panic is caught
// and named, because a crash and a missing refusal are the same defect and the message has to
// say which one happened.
//
// What the catch does NOT do is keep a whole package run alive, and that distinction is stated
// because it was overclaimed once. Go runs a package's tests in file order and framing_test.go
// sorts before this one, so with a per-case check deleted the framing test reaches the same
// dereference FIRST, does not recover, and takes the binary down before this test executes --
// the named message never prints. What the catch buys is that -run over this test alone reports
// which of the two defects happened instead of a stack trace, and that a deletion this test is
// the first to reach is reported rather than fatal.
func TestEveryProposalTypeRefusesEveryArmThatIsNotItsOwn(t *testing.T) {
	samples := proposalArmSamples(t)
	registered := slices.Sorted(maps.Keys(samples))

	// the control: each row encodes when its type and its arm agree, or every refusal below is
	// a refusal of something this table built wrong rather than of the mismatch
	for _, proposalType := range registered {
		matching := samples[proposalType].proposal
		if _, err := syntax.Marshal(&matching); err != nil {
			t.Fatalf("the %s row did not encode with its own type: %v", samples[proposalType].field, err)
		}
	}

	refused := 0
	for _, named := range registered {
		for _, populated := range registered {
			if named == populated {
				continue
			}
			// exactly one arm is populated, so the COUNT is satisfied and the only thing that
			// can refuse this is the per-case check for the arm the type names
			mismatched := samples[populated].proposal
			mismatched.ProposalType = named

			var marshalErr error
			panicked := false
			func() {
				defer func() {
					if raised := recover(); raised != nil {
						panicked = true
						t.Errorf("a proposal typed %#04x carrying the %s arm panicked in the encoder: %v; the arm the discriminant names was dereferenced without being checked for",
							named, samples[populated].field, raised)
					}
				}()
				_, marshalErr = syntax.Marshal(&mismatched)
			}()
			if panicked {
				continue
			}
			if !errors.Is(marshalErr, ErrContentArmMismatch) {
				t.Errorf("a proposal typed %#04x carrying the %s arm encoded with %v, want ErrContentArmMismatch; MLS signs over serialized forms, so an encoder that wrote whichever arm was non-nil would sign a proposal the caller did not build",
					named, samples[populated].field, marshalErr)
				continue
			}
			refused += 1
		}
	}

	// and the third shape: a type that names an arm with nothing populated at all, stated for
	// every registered type rather than for Remove alone
	empty := 0
	for _, named := range registered {
		if _, err := syntax.Marshal(&Proposal{ProposalType: named}); !errors.Is(err, ErrContentArmMismatch) {
			t.Errorf("a proposal typed %#04x with no arm at all encoded with %v, want ErrContentArmMismatch", named, err)
			continue
		}
		empty += 1
	}

	if want := len(registered) * (len(registered) - 1); refused != want {
		t.Fatalf("%d of the %d (type, foreign arm) pairs were refused", refused, want)
	}
	if empty != len(registered) {
		t.Fatalf("%d of the %d armless proposals were refused", empty, len(registered))
	}
	t.Logf("%d (type, foreign arm) pairs and %d armless proposals refused", refused, empty)
}

// ---------------------------------------------------------------------------
// the arms of a ProposalOrRef, and the code points that name them
// ---------------------------------------------------------------------------

// marshalCatchingPanic encodes and turns a panic into a value the caller can name.
//
// An arm check that is missing and an arm check that is wrong are the same defect and they
// surface differently: one comes back as a nil error and one takes the process down. A test that
// only ever sees the second learns it from a stack trace, so this hands back both and lets the
// message say which happened.
func marshalCatchingPanic(codec syntax.Marshaler) (encoded []byte, raised any, err error) {
	defer func() { raised = recover() }()
	encoded, err = syntax.Marshal(codec)
	return
}

// proposalOrRefArmFields is every field of ProposalOrRef that carries a BODY, derived off the
// type by the reading proposalArmFields uses one type down: a field that can hold something --
// a pointer or a slice -- rather than a list of the two arms somebody remembered. The
// discriminant is a ProposalOrRefType and falls out of the class by that same reading rather
// than by name, so a discriminant renamed does not fall in and a third arm added does.
func proposalOrRefArmFields(t *testing.T) []string {
	t.Helper()
	orRef := reflect.TypeOf(ProposalOrRef{})
	found := []string{}
	for i := 0; i < orRef.NumField(); i += 1 {
		field := orRef.Field(i)
		switch field.Type.Kind() {
		case reflect.Pointer, reflect.Slice:
			found = append(found, field.Name)
		}
	}
	if len(found) < 2 {
		t.Fatalf("the derivation read %v out of ProposalOrRef, which is fewer arms than the type has, so the sweeps below would run over almost nothing", found)
	}
	slices.Sort(found)
	return found
}

// proposalOrRefArmValues populates one arm, keyed by the field it fills.
//
// It is written by hand and that is safe here because every gate that uses it first holds it to
// the DERIVED arm set in both directions: an arm added to ProposalOrRef with no row here fails
// asking for one, and a row naming a field that is no longer an arm fails too. What it may not
// be is a source of truth about WHICH arms exist, and it is never read as one.
func proposalOrRefArmValues() map[string]func(*ProposalOrRef) {
	return map[string]func(*ProposalOrRef){
		"Proposal": func(one *ProposalOrRef) {
			one.Proposal = &Proposal{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: 1}}
		},
		"Reference": func(one *ProposalOrRef) {
			one.Reference = ProposalRef{0xaa, 0xbb}
		},
	}
}

// proposalOrRefArmOfEachType joins the ProposalOrRefType code points that name a body to the
// field of ProposalOrRef that holds it, BY NAME and in both directions.
//
// Nothing here is a hand written pair, and that is the point. RFC 9420 section 12.4 writes the
// case "case proposal: Proposal proposal;", so the select case, the Go constant and the Go
// field are three spellings of one name: ProposalOrRefTypeProposal folds to "proposal", the
// field Proposal folds to "proposal", and the case IS "proposal". A pair table written beside
// them would be a fourth place to make the very mistake the join exists to find -- which is how
// ExtensionTypeExternalSenders came to be pinned at its neighbour's code point.
func proposalOrRefArmOfEachType(t *testing.T) map[ProposalOrRefType]string {
	t.Helper()
	// the arms, folded to the RFC's spelling of the case that selects them
	armByRfcName := map[string]string{}
	for _, field := range proposalOrRefArmFields(t) {
		rfcName := rfcNameOfRegistryConstant("", field)
		if other, repeated := armByRfcName[rfcName]; repeated {
			t.Fatalf("the arms %s and %s of ProposalOrRef both fold to the RFC name %s, so the join below is ambiguous", other, field, rfcName)
		}
		armByRfcName[rfcName] = field
	}
	// held to the select the RFC writes, in BOTH directions: an arm the section 12.4 select
	// names no case for is an arm nothing on the wire can select, and a case with no arm is a
	// body this package cannot carry.
	cases := slices.Clone(rfc9420Section124ProposalOrRefSelectCases)
	slices.Sort(cases)
	if !slices.Equal(slices.Sorted(maps.Keys(armByRfcName)), cases) {
		t.Fatalf("the arms of ProposalOrRef fold to %v and RFC 9420 section 12.4's select names the cases %v",
			slices.Sorted(maps.Keys(armByRfcName)), rfc9420Section124ProposalOrRefSelectCases)
	}
	// and the code points, off the declared constants rather than off the transcription, so
	// this map names what the package will actually encode under
	armOf := map[ProposalOrRefType]string{}
	for name, value := range registryConstantsOfType(t, "ProposalOrRefType") {
		field, named := armByRfcName[rfcNameOfRegistryConstant("ProposalOrRefType", name)]
		if !named {
			continue
		}
		armOf[ProposalOrRefType(value)] = field
	}
	if len(armOf) != len(armByRfcName) {
		t.Fatalf("%d arms of ProposalOrRef joined to %d declared code points; a constant and an arm that fold to one name is what this join is made of",
			len(armByRfcName), len(armOf))
	}
	return armOf
}

// TestEveryProposalOrRefTypeRefusesEveryArmThatIsNotItsOwn is ProposalOrRef's half of the arm
// check, over the cross product of the code points that name a body with the arms that hold one.
//
// checkArm one type down asks two questions and this type asked neither. The per-case half is
// what stands between ProposalOrRef{Type: ProposalOrRefTypeProposal} and
// self.Proposal.MarshalMLS(w) on a nil pointer -- verbatim the defect the seven per-case checks
// of Proposal.checkArm close, on the sibling type twelve lines further down the same file, and
// deleting it left the WHOLE of ./mls/... and ./message/... green. Measured, not supposed.
//
// The count is the half a per-case check does not give at all. A ProposalOrRef carrying an
// inline proposal AND a reference encoded to the proposal alone: two distinct values wrote
// 01000300000001, the reference the caller set was in neither encoding, and the decode of
// either came back with Reference empty. That is checkArm's own argument for why a Proposal must
// count EXACTLY one arm, and it was applied to Proposal and not to the type that carries the
// reference. A ProposalOrRef sits inside a Commit inside a FramedContent that is SIGNED, so the
// arm that is dropped is dropped out of bytes somebody put a signature over.
//
// The panic is caught rather than left to the runtime, because a missing refusal and a nil
// dereference are the same defect and the message has to say which one happened.
func TestEveryProposalOrRefTypeRefusesEveryArmThatIsNotItsOwn(t *testing.T) {
	armOf := proposalOrRefArmOfEachType(t)
	fields := proposalOrRefArmFields(t)
	populate := proposalOrRefArmValues()
	if !slices.Equal(slices.Sorted(maps.Keys(populate)), fields) {
		t.Fatalf("this file populates %v and the arms derived off ProposalOrRef are %v; an arm with no way to fill it is an arm this sweep would skip in silence",
			slices.Sorted(maps.Keys(populate)), fields)
	}
	named := slices.Sorted(maps.Keys(armOf))

	// the control: each code point encodes when it and its arm agree, or every refusal below
	// is a refusal of something this table built wrong rather than of the mismatch
	for _, one := range named {
		matching := ProposalOrRef{Type: one}
		populate[armOf[one]](&matching)
		if _, err := syntax.Marshal(&matching); err != nil {
			t.Fatalf("a ProposalOrRef typed %#02x carrying its own %s arm did not encode: %v", one, armOf[one], err)
		}
	}

	// the foreign arm: the count is satisfied, so the only thing that can refuse this is the
	// per-case check for the arm the discriminant names
	foreign := 0
	for _, one := range named {
		for _, field := range fields {
			if field == armOf[one] {
				continue
			}
			mismatched := ProposalOrRef{Type: one}
			populate[field](&mismatched)
			encoded, raised, err := marshalCatchingPanic(&mismatched)
			if raised != nil {
				t.Errorf("a ProposalOrRef typed %#02x carrying the %s arm panicked in the encoder: %v; the arm the discriminant names was dereferenced without being checked for",
					one, field, raised)
				continue
			}
			if !errors.Is(err, ErrContentArmMismatch) {
				t.Errorf("a ProposalOrRef typed %#02x carrying the %s arm encoded to %x with %v, want ErrContentArmMismatch; the arm it names is absent from those bytes and from the commit that would carry them",
					one, field, encoded, err)
				continue
			}
			foreign += 1
		}
	}

	// no arm at all, for every code point that names one rather than for the one a case would
	// have probed
	armless := 0
	for _, one := range named {
		empty := ProposalOrRef{Type: one}
		encoded, raised, err := marshalCatchingPanic(&empty)
		if raised != nil {
			t.Errorf("a ProposalOrRef typed %#02x with no arm at all panicked in the encoder: %v", one, raised)
			continue
		}
		if !errors.Is(err, ErrContentArmMismatch) {
			t.Errorf("a ProposalOrRef typed %#02x with no arm at all encoded to %x with %v, want ErrContentArmMismatch", one, encoded, err)
			continue
		}
		armless += 1
	}

	// and more than one arm, over every PAIR the derivation produces. This is the count, and
	// what it prevents is two distinct values sharing one encoding: whichever arm the
	// discriminant does not name is dropped, silently, out of bytes that get signed.
	pairs := 0
	for _, one := range named {
		for i := 0; i < len(fields); i += 1 {
			for j := i + 1; j < len(fields); j += 1 {
				both := ProposalOrRef{Type: one}
				populate[fields[i]](&both)
				populate[fields[j]](&both)
				encoded, raised, err := marshalCatchingPanic(&both)
				if raised != nil {
					t.Errorf("a ProposalOrRef typed %#02x carrying both the %s and the %s arm panicked in the encoder: %v", one, fields[i], fields[j], raised)
					continue
				}
				if !errors.Is(err, ErrContentArmMismatch) {
					t.Errorf("a ProposalOrRef typed %#02x carrying both the %s and the %s arm encoded to %x with %v; one of the two is nowhere in those octets, so it encodes identically to the value that never carried it and the signature over the commit covers neither",
						one, fields[i], fields[j], encoded, err)
					continue
				}
				pairs += 1
			}
		}
	}

	if want := len(named) * (len(fields) - 1); foreign != want {
		t.Fatalf("%d of the %d (code point, foreign arm) pairs were refused", foreign, want)
	}
	if armless != len(named) {
		t.Fatalf("%d of the %d armless ProposalOrRefs were refused", armless, len(named))
	}
	if want := len(named) * len(fields) * (len(fields) - 1) / 2; pairs != want {
		t.Fatalf("%d of the %d (code point, arm pair) combinations were refused", pairs, want)
	}
	t.Logf("%d foreign arms, %d armless values and %d double armed values refused across %d ProposalOrRef code points",
		foreign, armless, pairs, len(named))
}

// TestEveryProposalOrRefTypeCodePointIsPinnedByTwoIndependentReadingsOfTheRfc pins proposal(1)
// and reference(2) against a reading that is not the enum transcription that declares them.
//
// ProposalType one screen up is pinned twice, by section 7.2 and by section 17.5.
// ProposalOrRefType had ONE value reading: rfc9420Section124ProposalOrRefTypes. The list beside
// it deliberately carries no values -- it says which members have a body -- so a hand that
// swapped the two constants in proposal_wire.go and swapped them again in the transcription
// that guards them agreed with itself and nothing here noticed. Measured. That is the exact
// shape ExtensionTypeExternalSenders shipped in at external_pub's code point.
//
// The second reading is the select four lines below the enum on the same page. It is a
// different construct and it carries information the values must agree with: RFC 9420 writes an
// enum's members in ascending code point order and writes the select's cases in the enum's own
// order, so the k-th case is the k-th assigned code point. The ORDER is a fact about the
// document that a transcriber who mistyped the values did not also have to get wrong, and the
// struct it is read out of is quoted verbatim above the list.
//
// A swapped ProposalOrRefType is not a compile error and not a bad encoding. It makes this
// implementation write an inline proposal under the octet every peer reads as a reference, and
// read a peer's reference as an inline proposal -- a different structure entirely, under a
// discriminant that decides which. Nothing that round trips its own output can see it.
func TestEveryProposalOrRefTypeCodePointIsPinnedByTwoIndependentReadingsOfTheRfc(t *testing.T) {
	// the enum's values are the dense octet run it writes -- reserved(0), proposal(1),
	// reference(2) -- with no gap for a name to slide through
	assigned := slices.Sorted(maps.Values(rfc9420Section124ProposalOrRefTypes))
	dense := []uint64{}
	for i := range assigned {
		dense = append(dense, uint64(i))
	}
	if !slices.Equal(assigned, dense) {
		t.Fatalf("the section 12.4 enum transcription assigns %v, and section 12.4 writes reserved(0) followed by one consecutive octet per member; a gap means a member was dropped or mistyped", assigned)
	}

	// and the names, ordered by the value the enum gives them, are the order the RFC writes
	// them in: the reserved zero the select takes no case for, then the select's cases as the
	// struct lists them
	byValue := slices.SortedFunc(maps.Keys(rfc9420Section124ProposalOrRefTypes), func(a string, b string) int {
		return int(rfc9420Section124ProposalOrRefTypes[a]) - int(rfc9420Section124ProposalOrRefTypes[b])
	})
	written := append([]string{"reserved"}, rfc9420Section124ProposalOrRefSelectCases...)
	if !slices.Equal(byValue, written) {
		t.Fatalf("the section 12.4 enum transcription orders the members %v by code point and the select four lines below it writes its cases in the order %v; two readings of one page disagree about which name holds which octet",
			byValue, written)
	}

	// the pin, joined to what the package actually declares. Both directions: a constant with
	// no row is a code point no reading covers, and a row with no constant is a member this
	// package cannot name.
	byRfcName := map[string]uint64{}
	for name, value := range registryConstantsOfType(t, "ProposalOrRefType") {
		rfcName := rfcNameOfRegistryConstant("ProposalOrRefType", name)
		if other, repeated := byRfcName[rfcName]; repeated {
			t.Fatalf("two constants fold to the RFC name %s, at %d and %d", rfcName, other, value)
		}
		byRfcName[rfcName] = value
	}
	if !maps.Equal(byRfcName, rfc9420Section124ProposalOrRefTypes) {
		t.Fatalf("package mls reads ProposalOrRefType as %v and RFC 9420 section 12.4 writes %v", byRfcName, rfc9420Section124ProposalOrRefTypes)
	}
	t.Logf("%d ProposalOrRefType code points pinned by the enum and by the order of the select cases that consume it", len(byRfcName))
}

// TestEveryProposalTypeThatNamesNoArmRefusesEveryRegisteredArm is checkArm's DEFAULT clause,
// which nothing observed.
//
// The cross product test at the end of this file sweeps the seven registered types against the
// seven registered arms and never pairs an UNREGISTERED type with one, so the eighth branch of
// checkArm -- the nil check on UnknownBody that stands in for an arm a GREASE code point does
// not have -- was unobserved: deleting it left the whole of ./mls/... and ./message/... green.
//
// What it prevents is this file's headline failure on the one path the verbatim sweep does not
// reach. Proposal{ProposalType: 0x0a0a, Add: &Add{...}} passes the count with one arm, falls
// through the marshal switch because 0x0a0a is unregistered, and emits a bare two octet
// discriminant with the Add nowhere in the bytes. A populated arm disappears from the encoding
// and the ProposalRef over it names a proposal that says something else.
//
// Both halves of the class are derived. The code points are the 16 bit space against the
// registry's own declarations, the same reading
// TestEveryProposalTypeWithNoArmIsCarriedVerbatimOverTheWholeSixteenBitSpace uses, so GREASE,
// the reserved zero and every unassigned point in between are covered without being listed. The
// arms are reflect over Proposal, less UnknownBody -- excluded by the reading the default clause
// itself uses, since for an unregistered type UnknownBody IS the arm and that shape is the
// verbatim one the sweep above states.
func TestEveryProposalTypeThatNamesNoArmRefusesEveryRegisteredArm(t *testing.T) {
	armed := map[ProposalType]bool{}
	for _, value := range registryConstantsOfType(t, "ProposalType") {
		// the reserved zero is excluded by its VALUE and not by its name: it names no arm, so
		// it belongs to the class swept here rather than to the registered one
		if value == uint64(ProposalTypeReserved) {
			continue
		}
		armed[ProposalType(value)] = true
	}
	if len(armed) == 0 {
		t.Fatal("no proposal type was derived as naming an arm, so this sweep would run over the whole space and say nothing")
	}
	arms := []string{}
	for _, field := range proposalArmFields(t) {
		if field == "UnknownBody" {
			continue
		}
		arms = append(arms, field)
	}
	if len(arms) == 0 {
		t.Fatal("the derivation left no registered arm to carry, so this sweep observed nothing")
	}

	refused, swept := 0, 0
	for candidate := 0; candidate <= 0xffff; candidate += 1 {
		proposalType := ProposalType(candidate)
		if armed[proposalType] {
			continue
		}
		swept += 1
		for _, field := range arms {
			// exactly one arm, and no UnknownBody: the count is satisfied and the only thing
			// that can refuse this is the default clause's own check
			carrying := Proposal{ProposalType: proposalType}
			held := reflect.ValueOf(&carrying).Elem().FieldByName(field)
			switch held.Kind() {
			case reflect.Pointer:
				held.Set(reflect.New(held.Type().Elem()))
			case reflect.Slice:
				held.Set(reflect.ValueOf([]byte{0x00}).Convert(held.Type()))
			default:
				t.Fatalf("%s came out of the derivation and is neither a pointer nor a slice", field)
			}
			encoded, raised, err := marshalCatchingPanic(&carrying)
			if raised != nil {
				t.Fatalf("a proposal typed %#04x, which names no arm, carrying the %s arm panicked in the encoder: %v", candidate, field, raised)
			}
			if !errors.Is(err, ErrContentArmMismatch) {
				t.Fatalf("a proposal typed %#04x, which names no arm, carrying the %s arm encoded to %x with %v, want ErrContentArmMismatch; the arm is nowhere in those octets and the ProposalRef over them names a proposal that says something else",
					candidate, field, encoded, err)
			}
			refused += 1
		}
	}
	if want := swept * len(arms); refused != want {
		t.Fatalf("%d of the %d (unregistered code point, registered arm) pairs were refused", refused, want)
	}
	if swept+len(armed) != 1<<16 {
		t.Fatalf("the derivation split the %d proposal type code points into %d naming an arm and %d naming none", 1<<16, len(armed), swept)
	}
	t.Logf("%d registered arms refused under each of %d code points that name no arm", len(arms), swept)
}

// TestAProposalDecodeLeavesNoFieldOfThePreviousValueBehind is the property
// Proposal.UnmarshalMLS's own comment claims for its receiver reset: no field of a previous
// value survives into a decode of different bytes.
//
// Nothing asserted it. Replacing the whole-receiver reset with an assignment to ProposalType
// alone left the whole of ./mls/... and ./message/... green, and with the reset gone a caller
// that reuses one Proposal across decodes reads a stale arm off the decoded value -- decode a
// Remove into a receiver that last held an Add and decoded.Add is still populated. The
// re-encode of that value then refuses on the arm count rather than round tripping, so the
// bytes a peer sent become a proposal this package will not write back.
//
// The class is the cross product of the published arm layouts with themselves, plus the
// unregistered shape, which carries the two fields no registered arm does. The comparison is
// against a decode of the SAME octets into a fresh receiver, so what it observes is every field
// at once rather than the ones a table thought to name.
func TestAProposalDecodeLeavesNoFieldOfThePreviousValueBehind(t *testing.T) {
	goldens := map[string][]byte{}
	for _, sample := range proposalArmSamples(t) {
		goldens[sample.field] = sample.golden
	}
	// the unregistered shape belongs to the class in both directions: a receiver that last
	// held a verbatim body must not carry UnknownType or UnknownBody into a decode of a
	// registered arm, and a receiver that last held an arm must not carry it into this one.
	goldens["unregistered"] = []byte{0x0a, 0x0a, 0xde, 0xad}
	if len(goldens) < 3 {
		t.Fatalf("the table holds %d layouts, which is too few to state anything about reuse", len(goldens))
	}
	names := slices.Sorted(maps.Keys(goldens))

	reused := 0
	for _, previous := range names {
		for _, next := range names {
			// the receiver is loaded by DECODING rather than by assignment, so what it holds
			// when the second decode arrives is a value this codec actually produces
			receiver := Proposal{}
			if err := syntax.Unmarshal(goldens[previous], &receiver); err != nil {
				t.Fatalf("%s: the golden did not decode: %v", previous, err)
			}
			if err := syntax.Unmarshal(goldens[next], &receiver); err != nil {
				t.Fatalf("%s decoded into a receiver that last held %s: %v", next, previous, err)
			}
			fresh := Proposal{}
			if err := syntax.Unmarshal(goldens[next], &fresh); err != nil {
				t.Fatalf("%s: the golden did not decode into a fresh receiver: %v", next, err)
			}
			if !reflect.DeepEqual(receiver, fresh) {
				t.Errorf("the %s layout decoded into a receiver that last held %s produced\n %+v\nand into a fresh receiver produced\n %+v\na field of the previous value survived the decode",
					next, previous, receiver, fresh)
				continue
			}
			// and the consequence, stated rather than left to be inferred: a value carrying a
			// stale arm no longer re-encodes to the octets it was decoded from
			reencoded, raised, err := marshalCatchingPanic(&receiver)
			if raised != nil {
				t.Errorf("%s decoded into a receiver that last held %s panicked on re-encode: %v", next, previous, raised)
				continue
			}
			if err != nil || !bytes.Equal(reencoded, goldens[next]) {
				t.Errorf("%s decoded into a receiver that last held %s re-encoded to %x with %v, want %x; the bytes a peer sent came back as something this package will not write",
					next, previous, reencoded, err, goldens[next])
				continue
			}
			reused += 1
		}
	}
	if want := len(names) * len(names); reused != want {
		t.Fatalf("%d of the %d (previous, next) reuses were clean", reused, want)
	}
	t.Logf("%d layouts decoded into a receiver holding each of them, %d reuses judged", len(names), reused)
}

// TestAProposalOrRefDecodeThatRefusesLeavesTheReceiverAsItFoundIt is the property
// ProposalOrRef.UnmarshalMLS's doc comment claims when it says it stages into a local --
// Sender's and Credential's staging, for their reason.
//
// Nothing asserted it. Making the receiver write eager left the targeted suite and the whole of
// ./mls/... and ./message/... green, and what an eager write leaves behind is a caller's value
// carrying the DISCRIMINANT of a message that was refused: Type says an inline proposal is here
// and the arm beside it is whatever the value held before. A decoder that refuses has said the
// octets are not a ProposalOrRef, and a caller that keeps its previous value is entitled to
// still have it.
//
// The refused class is derived rather than picked: every proper prefix of every encoding the
// arm table produces, plus every octet that names no arm, bare and with a tail so a refusal
// that was really a truncation cannot pass for a refusal of the discriminant.
func TestAProposalOrRefDecodeThatRefusesLeavesTheReceiverAsItFoundIt(t *testing.T) {
	armOf := proposalOrRefArmOfEachType(t)
	populate := proposalOrRefArmValues()
	named := slices.Sorted(maps.Keys(armOf))

	refusals := [][]byte{}
	for _, one := range named {
		value := ProposalOrRef{Type: one}
		populate[armOf[one]](&value)
		encoded, err := syntax.Marshal(&value)
		if err != nil {
			t.Fatalf("a ProposalOrRef typed %#02x carrying its own %s arm did not encode: %v", one, armOf[one], err)
		}
		for cut := 0; cut < len(encoded); cut += 1 {
			refusals = append(refusals, encoded[:cut])
		}
	}
	for candidate := 0; candidate <= 0xff; candidate += 1 {
		if _, names := armOf[ProposalOrRefType(candidate)]; names {
			continue
		}
		refusals = append(refusals, []byte{byte(candidate)}, []byte{byte(candidate), 0xde, 0xad})
	}
	// and the one refusal that is neither a truncation nor an unknown octet: a reference of no
	// octets, which is wire legal and which this decoder refuses on its encoder's terms
	refusals = append(refusals, []byte{byte(ProposalOrRefTypeReference), 0x00})
	if len(refusals) < 4 {
		t.Fatalf("the derivation produced %d refusable encodings, which is too few to state anything", len(refusals))
	}

	// the value the receiver is loaded with, produced by this codec rather than assembled
	priorOctets, err := syntax.Marshal(&ProposalOrRef{Type: ProposalOrRefTypeReference, Reference: ProposalRef{0x11, 0x22}})
	if err != nil {
		t.Fatalf("the prior value did not encode: %v", err)
	}
	prior := ProposalOrRef{}
	if err := syntax.Unmarshal(priorOctets, &prior); err != nil {
		t.Fatalf("the prior value did not decode: %v", err)
	}

	held := 0
	for _, encoded := range refusals {
		receiver := ProposalOrRef{}
		if err := syntax.Unmarshal(priorOctets, &receiver); err != nil {
			t.Fatalf("the prior value did not decode into the receiver: %v", err)
		}
		if err := syntax.Unmarshal(encoded, &receiver); err == nil {
			t.Errorf("%x decoded to %+v rather than being refused, so this row says nothing about what a refusal leaves behind", encoded, receiver)
			continue
		}
		if !reflect.DeepEqual(receiver, prior) {
			t.Errorf("the refused decode of %x left the caller's value as %+v, and it was %+v; a decoder that refuses has said these octets are not a ProposalOrRef, and the discriminant of one it refused is now on a value somebody else built",
				encoded, receiver, prior)
			continue
		}
		held += 1
	}
	if held != len(refusals) {
		t.Fatalf("%d of the %d refused decodes left the receiver alone", held, len(refusals))
	}
	t.Logf("%d refused ProposalOrRef decodes left the caller's value untouched", held)
}
