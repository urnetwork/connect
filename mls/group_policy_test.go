package mls

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"math/rand"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// the file whose refusals the derived sweep at the bottom of this file reads. A CONSTANT and not
// a base name match, for the reason the hkdf confinement writes down: a base name excuses a
// group_policy.go in any subdirectory, and this one is the path the package scan collects.
const groupPolicySourceFile = "group_policy.go"

func testPolicy(t *testing.T, owner *testMember, admin *testMember, member *testMember) *GroupPolicyExtension {
	t.Helper()
	policy := &GroupPolicyExtension{
		Roles: []RoleEntry{
			{MemberId: owner.IdentityPub, Role: RoleOwner},
			{MemberId: admin.IdentityPub, Role: RoleAdmin},
			{MemberId: member.IdentityPub, Role: RoleMember},
		},
		RetentionPolicy:     RetentionPolicy{DurableMs: 0, MediaMs: 2592000000},
		DisappearingBuckets: []uint8{0},
		ServerId:            []byte("urmessage-v1-server"),
	}
	if err := policy.Canonicalize(); err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	return policy
}

func TestGroupPolicyRoundTrip(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	admin := testIdentity(t, crypto, "admin")
	member := testIdentity(t, crypto, "member")

	policy := testPolicy(t, owner, admin, member)
	encoded, err := policy.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if encoded.ExtensionType != ExtensionTypeUrmessageGroupPolicy {
		t.Fatalf("ExtensionType = %#x, want %#x", encoded.ExtensionType, ExtensionTypeUrmessageGroupPolicy)
	}
	parsed, err := ParseGroupPolicyExtension(encoded.ExtensionData)
	if err != nil {
		t.Fatalf("ParseGroupPolicyExtension: %v", err)
	}
	reencoded, err := parsed.Encode()
	if err != nil {
		t.Fatalf("re-Encode: %v", err)
	}
	if !bytes.Equal(reencoded.ExtensionData, encoded.ExtensionData) {
		t.Fatal("re-encode is not byte identical")
	}
	if role, ok := parsed.RoleOf(admin.IdentityPub); !ok || role != RoleAdmin {
		t.Fatalf("RoleOf(admin) = %v %v, want admin true", role, ok)
	}
	if parsed.AdminCount() != 1 {
		t.Fatalf("AdminCount = %d, want 1", parsed.AdminCount())
	}
	ownerId, ok := parsed.OwnerId()
	if !ok || !bytes.Equal(ownerId, owner.IdentityPub) {
		t.Fatal("OwnerId did not return the owner")
	}
}

// TestGroupPolicyRoundTripCarriesEveryFieldAndNotOnlyTheRoles is what the plan's round trip cannot
// say on its own.
//
// A codec that DROPPED the retention pair, the buckets and the server id on write and defaulted
// them on read round trips perfectly: the encode of the decode of the encode is byte identical,
// because all three encodes wrote the same nothing. So the decoded VALUE is compared field by
// field against what went in, which is the assertion a dropped field fails.
func TestGroupPolicyRoundTripCarriesEveryFieldAndNotOnlyTheRoles(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	admin := testIdentity(t, crypto, "admin")
	member := testIdentity(t, crypto, "member")

	policy := testPolicy(t, owner, admin, member)
	policy.RetentionPolicy = RetentionPolicy{DurableMs: 604800000, MediaMs: 2592000000}
	policy.DisappearingBuckets = []uint8{0, 3, 255}
	encoded, err := policy.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	parsed, err := ParseGroupPolicyExtension(encoded.ExtensionData)
	if err != nil {
		t.Fatalf("ParseGroupPolicyExtension: %v", err)
	}
	if parsed.RetentionPolicy != policy.RetentionPolicy {
		t.Errorf("RetentionPolicy = %+v, want %+v; a codec that drops this pair and defaults it round trips perfectly",
			parsed.RetentionPolicy, policy.RetentionPolicy)
	}
	if !slices.Equal(parsed.DisappearingBuckets, policy.DisappearingBuckets) {
		t.Errorf("DisappearingBuckets = %v, want %v", parsed.DisappearingBuckets, policy.DisappearingBuckets)
	}
	if !bytes.Equal(parsed.ServerId, policy.ServerId) {
		t.Errorf("ServerId = %q, want %q", parsed.ServerId, policy.ServerId)
	}
	if len(parsed.Roles) != len(policy.Roles) {
		t.Fatalf("Roles = %d entries, want %d", len(parsed.Roles), len(policy.Roles))
	}
	for i := range policy.Roles {
		if !bytes.Equal(parsed.Roles[i].MemberId, policy.Roles[i].MemberId) ||
			parsed.Roles[i].Role != policy.Roles[i].Role {
			t.Errorf("Roles[%d] = %v/%v, want %v/%v", i,
				parsed.Roles[i].MemberId, parsed.Roles[i].Role,
				policy.Roles[i].MemberId, policy.Roles[i].Role)
		}
	}
}

// TestGroupPolicyEncodesAsBytesAssembledOutsideTheEncoder holds the wire format against a run
// written out by hand rather than against the encoder's own output.
//
// This is the assertion a round trip cannot make. A codec that wrote the role byte before the
// member id, or the retention pair as little endian, or the vector prefix as an ELEMENT count
// rather than the byte count syntax.WriteVector writes, round trips its own output perfectly and
// disagrees with every other implementation on the wire -- and this body is inside the confirmed
// transcript hash, so that disagreement is a permanent fork rather than a rejected message. The
// bytes below are read off RFC 9420 section 2.1.2 and MASTER section 6 and assembled with no call
// into mls/syntax at all.
func TestGroupPolicyEncodesAsBytesAssembledOutsideTheEncoder(t *testing.T) {
	policy := &GroupPolicyExtension{
		Roles: []RoleEntry{
			{MemberId: []byte{0x01, 0x02}, Role: RoleMember},
			{MemberId: []byte{0x03}, Role: RoleOwner},
		},
		RetentionPolicy:     RetentionPolicy{DurableMs: 0x0102030405060708, MediaMs: 2592000000},
		DisappearingBuckets: []uint8{0x00, 0x07},
		ServerId:            []byte("srv"),
	}
	want := []byte{
		// roles<V>: a varint byte count of 7, then the two entries
		0x07,
		// entry 0: opaque member_id<V> of two octets, then the role byte 1 (member)
		0x02, 0x01, 0x02, 0x01,
		// entry 1: opaque member_id<V> of one octet, then the role byte 3 (owner)
		0x01, 0x03, 0x03,
		// retention_durable_ms, uint64 big endian
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		// retention_media_ms, uint64 big endian: 2592000000 == 0x9a7ec800, thirty days
		0x00, 0x00, 0x00, 0x00, 0x9a, 0x7e, 0xc8, 0x00,
		// disappearing_buckets<V>: a varint byte count of 2, then the two bucket octets
		0x02, 0x00, 0x07,
		// server_id<V>: a varint byte count of 3, then "srv"
		0x03, 0x73, 0x72, 0x76,
	}
	encoded, err := policy.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !bytes.Equal(encoded.ExtensionData, want) {
		t.Fatalf("the encoded body is\n\t%x\nand the bytes assembled by hand are\n\t%x", encoded.ExtensionData, want)
	}
	// and the same run decodes back to the same value, so the hand assembled bytes are the ones
	// this package READS as well as the ones it writes
	parsed, err := ParseGroupPolicyExtension(want)
	if err != nil {
		t.Fatalf("ParseGroupPolicyExtension over the hand assembled bytes: %v", err)
	}
	if len(parsed.Roles) != 2 || parsed.Roles[0].Role != RoleMember || parsed.Roles[1].Role != RoleOwner {
		t.Fatalf("the hand assembled bytes decode to roles %+v", parsed.Roles)
	}
	if parsed.RetentionPolicy.MediaMs != 2592000000 {
		t.Fatalf("MediaMs = %d, want 2592000000; the retention pair is not being read big endian", parsed.RetentionPolicy.MediaMs)
	}
}

// TestGroupPolicyIsStampedWithTheCodePointTheRegistryAssignsIt pins 0xF001 itself.
//
// The value is a compatibility surface and not an implementation detail: it is checked here
// against the interface registry (section 8.1) and spec A section 3.4, both of which write 0xF001,
// and it sits one digit from urmessage_leaf_keys and urmessage_owner_successor in the same const
// block. A code point off by one encodes, signs, travels, and is read at the far end as a
// different extension entirely.
func TestGroupPolicyIsStampedWithTheCodePointTheRegistryAssignsIt(t *testing.T) {
	if uint16(ExtensionTypeUrmessageGroupPolicy) != 0xF001 {
		t.Fatalf("ExtensionTypeUrmessageGroupPolicy = %#04x, want 0xF001", uint16(ExtensionTypeUrmessageGroupPolicy))
	}
	policy := &GroupPolicyExtension{Roles: []RoleEntry{{MemberId: []byte("owner"), Role: RoleOwner}}}
	encoded, err := policy.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if uint16(encoded.ExtensionType) != 0xF001 {
		t.Fatalf("Encode stamped %#04x, want 0xF001", uint16(encoded.ExtensionType))
	}
	for _, neighbour := range []ExtensionType{ExtensionTypeUrmessageLeafKeys, ExtensionTypeUrmessageOwnerSuccessor} {
		if _, err := ParseGroupPolicyFrom(Extension{ExtensionType: neighbour, ExtensionData: encoded.ExtensionData}); !errors.Is(err, ErrMalformedExtension) {
			t.Errorf("ParseGroupPolicyFrom under %#04x error = %v, want ErrMalformedExtension", uint16(neighbour), err)
		}
	}
}

func TestGroupPolicyCanonicalOrdering(t *testing.T) {
	crypto := testCrypto(t)
	a := testIdentity(t, crypto, "a")
	b := testIdentity(t, crypto, "b")
	c := testIdentity(t, crypto, "c")

	first := &GroupPolicyExtension{Roles: []RoleEntry{
		{MemberId: a.IdentityPub, Role: RoleOwner},
		{MemberId: b.IdentityPub, Role: RoleAdmin},
		{MemberId: c.IdentityPub, Role: RoleMember},
	}}
	second := &GroupPolicyExtension{Roles: []RoleEntry{
		{MemberId: c.IdentityPub, Role: RoleMember},
		{MemberId: a.IdentityPub, Role: RoleOwner},
		{MemberId: b.IdentityPub, Role: RoleAdmin},
	}}
	if err := first.Canonicalize(); err != nil {
		t.Fatalf("Canonicalize first: %v", err)
	}
	if err := second.Canonicalize(); err != nil {
		t.Fatalf("Canonicalize second: %v", err)
	}
	e1, err := first.Encode()
	if err != nil {
		t.Fatalf("Encode first: %v", err)
	}
	e2, err := second.Encode()
	if err != nil {
		t.Fatalf("Encode second: %v", err)
	}
	if !bytes.Equal(e1.ExtensionData, e2.ExtensionData) {
		t.Fatal("two insertion orders of the same role set encode differently")
	}
}

// TestGroupPolicyCanonicalOrderIsAscendingByMemberIdAndNotMerelyDeterministic is the half the
// agreement test above cannot see.
//
// Two clients that both sorted by the ROLE byte, or both reversed the member ids, agree with each
// other perfectly and disagree with every other implementation, and "the two encodings match" is
// blind to that. Interoperability is a claim about a specific order, so the order is compared
// against an independent one -- lexicographic over the member id, spelled with bytes.Compare,
// which is a comparator this package's production source may not call and a test may.
func TestGroupPolicyCanonicalOrderIsAscendingByMemberIdAndNotMerelyDeterministic(t *testing.T) {
	crypto := testCrypto(t)
	roles := []RoleEntry{}
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		member := testIdentity(t, crypto, name)
		role := RoleMember
		if name == "a" {
			role = RoleOwner
		}
		roles = append(roles, RoleEntry{MemberId: member.IdentityPub, Role: role})
	}
	policy := &GroupPolicyExtension{Roles: slices.Clone(roles)}
	if err := policy.Canonicalize(); err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	independent := slices.Clone(roles)
	slices.SortStableFunc(independent, func(left RoleEntry, right RoleEntry) int {
		return bytes.Compare(left.MemberId, right.MemberId)
	})
	for i := range independent {
		if !bytes.Equal(policy.Roles[i].MemberId, independent[i].MemberId) {
			t.Fatalf("Canonicalize put %x at position %d and lexicographic order puts %x there; the canonical order is a wire commitment and not merely a repeatable one",
				policy.Roles[i].MemberId, i, independent[i].MemberId)
		}
	}
	if err := policy.Validate(); err != nil {
		t.Fatalf("a canonicalized policy must satisfy the order Validate enforces: %v", err)
	}
}

// TestCompareMemberIdsAnswersWhatBytesCompareAnswers is the oracle for the one piece of this file
// that is written out by hand rather than taken from the standard library.
//
// compareMemberIds is spelled with crypto/subtle because guardrail 8's derived gate reports every
// other comparator this package's imports can reach, so the ordering the whole canonical form
// rests on is an open coded fold over four subtle primitives. A fold that got the ordering
// backwards, or that latched on the wrong octet, or that ordered a prefix AFTER what extends it,
// still produces a total order -- so Canonicalize would still be deterministic, the two insertion
// orders would still agree, and only a second implementation would ever notice. bytes.Compare is
// that second implementation, and it is reachable here because this gate's scan skips test files.
func TestCompareMemberIdsAnswersWhatBytesCompareAnswers(t *testing.T) {
	corner := [][]byte{
		nil, {}, {0x00}, {0x01}, {0xff}, {0x00, 0x00}, {0x00, 0x01}, {0x01, 0x00},
		{0x7f, 0xff}, {0x80, 0x00}, {0xff, 0xfe}, {0xff, 0xff}, {0x01, 0x02, 0x03},
		[]byte("a"), []byte("A"), []byte("ab"), []byte("aB"), []byte("b"),
	}
	for _, left := range corner {
		for _, right := range corner {
			if got, want := compareMemberIds(left, right), bytes.Compare(left, right); got != want {
				t.Fatalf("compareMemberIds(%x, %x) = %d, bytes.Compare says %d", left, right, got, want)
			}
		}
	}
	// and over random runs, because the corners are what somebody thought of
	random := rand.New(rand.NewSource(0x9420))
	for i := 0; i < 4000; i += 1 {
		left := make([]byte, random.Intn(6))
		right := make([]byte, random.Intn(6))
		random.Read(left)
		random.Read(right)
		// half the pairs share a prefix, so the fold's latch is exercised rather than decided by
		// the first octet every time
		if i%2 == 0 && len(left) > 0 && len(right) > 0 {
			right[0] = left[0]
		}
		if got, want := compareMemberIds(left, right), bytes.Compare(left, right); got != want {
			t.Fatalf("compareMemberIds(%x, %x) = %d, bytes.Compare says %d", left, right, got, want)
		}
		if sameMemberId(left, right) != bytes.Equal(left, right) {
			t.Fatalf("sameMemberId(%x, %x) = %v, bytes.Equal says %v", left, right,
				sameMemberId(left, right), bytes.Equal(left, right))
		}
	}
}

func TestGroupPolicyRejectsUnsortedOnParse(t *testing.T) {
	crypto := testCrypto(t)
	a := testIdentity(t, crypto, "a")
	b := testIdentity(t, crypto, "b")
	lo, hi := a, b
	if bytes.Compare(a.IdentityPub, b.IdentityPub) > 0 {
		lo, hi = b, a
	}
	unsorted := &GroupPolicyExtension{Roles: []RoleEntry{
		{MemberId: hi.IdentityPub, Role: RoleOwner},
		{MemberId: lo.IdentityPub, Role: RoleMember},
	}}
	encoded, err := unsorted.encodeUnchecked()
	if err != nil {
		t.Fatalf("encodeUnchecked: %v", err)
	}
	entry := Extension{ExtensionType: ExtensionTypeUrmessageGroupPolicy, ExtensionData: encoded}
	_, err = ParseGroupPolicyFrom(entry)
	if !errors.Is(err, ErrRolesNotCanonical) {
		t.Fatalf("ParseGroupPolicyFrom error = %v, want ErrRolesNotCanonical", err)
	}
	// and the refusal is a REFUSAL rather than a silent re-sort: nothing usable comes back
	policy, err := ParseGroupPolicyFrom(entry)
	if policy != nil {
		t.Fatalf("ParseGroupPolicyFrom answered %+v beside its refusal; a receiver that hands back a re-sorted policy accepts two spellings of one group", policy)
	}
	// and out of a group context, which is where every consumer in this package reads one
	if _, err := GroupPolicyOf([]Extension{entry}); !errors.Is(err, ErrRolesNotCanonical) {
		t.Fatalf("GroupPolicyOf error = %v, want ErrRolesNotCanonical", err)
	}
}

func TestGroupPolicyRejectsDuplicateMember(t *testing.T) {
	crypto := testCrypto(t)
	a := testIdentity(t, crypto, "a")
	policy := &GroupPolicyExtension{Roles: []RoleEntry{
		{MemberId: a.IdentityPub, Role: RoleOwner},
		{MemberId: a.IdentityPub, Role: RoleMember},
	}}
	if err := policy.Canonicalize(); !errors.Is(err, ErrDuplicateRoleEntry) {
		t.Fatalf("Canonicalize error = %v, want ErrDuplicateRoleEntry", err)
	}
}

func TestGroupPolicyRequiresExactlyOneOwner(t *testing.T) {
	crypto := testCrypto(t)
	a := testIdentity(t, crypto, "a")
	b := testIdentity(t, crypto, "b")

	none := &GroupPolicyExtension{Roles: []RoleEntry{{MemberId: a.IdentityPub, Role: RoleMember}}}
	if err := none.Canonicalize(); err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if err := none.Validate(); !errors.Is(err, ErrNoOwner) {
		t.Fatalf("Validate error = %v, want ErrNoOwner", err)
	}

	two := &GroupPolicyExtension{Roles: []RoleEntry{
		{MemberId: a.IdentityPub, Role: RoleOwner},
		{MemberId: b.IdentityPub, Role: RoleOwner},
	}}
	if err := two.Canonicalize(); err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if err := two.Validate(); !errors.Is(err, ErrMultipleOwners) {
		t.Fatalf("Validate error = %v, want ErrMultipleOwners", err)
	}
}

// TestGroupPolicyRejectsUnknownRoleByte is the plan's test with the reason its refusal names
// asserted, and the assertion is not a nicety.
//
// As the plan wrote it the policy held ONE entry carrying role byte 9 and no owner at all, so
// Encode refused it with ErrNoOwner and "err != nil" was satisfied by the missing owner. Measured:
// with Role.valid rewritten to answer true for every byte -- the whole role gate, both sides of
// the wire, deleted -- the plan's test still passed. So the policy here carries a real owner
// beside the undefined byte, and the sentinel and the clause are both named.
func TestGroupPolicyRejectsUnknownRoleByte(t *testing.T) {
	crypto := testCrypto(t)
	a := testIdentity(t, crypto, "a")
	b := testIdentity(t, crypto, "b")
	policy := &GroupPolicyExtension{Roles: []RoleEntry{
		{MemberId: a.IdentityPub, Role: RoleOwner},
		{MemberId: b.IdentityPub, Role: Role(9)},
	}}
	if err := policy.Canonicalize(); err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	_, err := policy.Encode()
	if err == nil {
		t.Fatal("Encode accepted role byte 9")
	}
	if !errors.Is(err, ErrMalformedExtension) {
		t.Fatalf("Encode answered %v, want ErrMalformedExtension", err)
	}
	if !strings.Contains(err.Error(), "role byte 9") {
		t.Fatalf("Encode answered %q, which does not name the role byte; this refusal has to be the role gate and not the owner count standing in front of it", err)
	}
}

// TestGroupPolicyRefusesEveryUndefinedRoleByteOnBothSidesOfTheWire is the class the test above
// samples one member of.
//
// Four role bytes are defined and 252 are not, and the encoder and the decoder have to refuse the
// same 252: a decoder that accepted one would hand back a policy this build renders as "unknown"
// and treats, wherever a switch has a default, as an observer. The sweep runs the whole uint8
// space rather than the neighbours somebody thought of, and it runs it in BOTH directions off one
// body assembled with encodeUnchecked, which is what a hostile peer would send.
func TestGroupPolicyRefusesEveryUndefinedRoleByteOnBothSidesOfTheWire(t *testing.T) {
	defined := []Role{RoleObserver, RoleMember, RoleAdmin, RoleOwner}
	for value := 0; value <= 0xff; value += 1 {
		role := Role(value)
		policy := &GroupPolicyExtension{Roles: []RoleEntry{{MemberId: groupPolicyLowId, Role: role}}}
		_, encodeErr := policy.encodeUnchecked()
		// the smallest hostile edit there is: one well formed body with its single role octet
		// rewritten in place, so what the decoder sees differs from a legal message by one byte
		body := groupPolicyWellFormedBody(t)
		body[groupPolicyRoleByteOffset] = byte(value)
		_, decodeErr := ParseGroupPolicyExtension(body)
		if slices.Contains(defined, role) {
			if encodeErr != nil {
				t.Errorf("the encoder refused the defined role byte %d: %v", value, encodeErr)
			}
			continue
		}
		if !errors.Is(encodeErr, ErrMalformedExtension) {
			t.Errorf("the encoder answered %v for the undefined role byte %d, want ErrMalformedExtension", encodeErr, value)
		}
		if !errors.Is(decodeErr, ErrMalformedExtension) {
			t.Errorf("the decoder answered %v for the undefined role byte %d, want ErrMalformedExtension; a role this build cannot render is a policy it cannot enforce", decodeErr, value)
		}
	}
}

func TestRoleStrings(t *testing.T) {
	for _, tc := range []struct {
		role Role
		name string
	}{
		{RoleOwner, "owner"},
		{RoleAdmin, "admin"},
		{RoleMember, "member"},
		{RoleObserver, "observer"},
	} {
		if tc.role.String() != tc.name {
			t.Fatalf("Role(%d).String() = %q, want %q", tc.role, tc.role.String(), tc.name)
		}
		parsed, err := ParseRole(tc.name)
		if err != nil || parsed != tc.role {
			t.Fatalf("ParseRole(%q) = %v %v", tc.name, parsed, err)
		}
	}
	if _, err := ParseRole("superuser"); err == nil {
		t.Fatal("ParseRole accepted an unknown role name")
	}
}

// TestRoleNamesAreExactAndTheWireBytesAreTheOnesMasterAssigns is the half the table above leaves
// out, and both halves of it have bitten this project.
//
// The names are a wire surface: spec A section 7.3 exposes them through sdk, so a ParseRole that
// accepted "Owner" or "OWNER" would let one client grant an authority another client's parse of
// the same string refuses. And the four BYTES are a wire surface for the same reason the code
// point is -- they travel inside the transcript hash -- so they are pinned as numbers here rather
// than left to the order somebody happened to declare the constants in.
func TestRoleNamesAreExactAndTheWireBytesAreTheOnesMasterAssigns(t *testing.T) {
	for role, want := range map[Role]uint8{
		RoleObserver: 0, RoleMember: 1, RoleAdmin: 2, RoleOwner: 3,
	} {
		if uint8(role) != want {
			t.Errorf("%s is wire byte %d, want %d", role, uint8(role), want)
		}
	}
	for _, rejected := range []string{"Owner", "OWNER", " owner", "owner ", "", "0", "3", "unknown"} {
		if role, err := ParseRole(rejected); err == nil {
			t.Errorf("ParseRole(%q) answered %v; the role name is a wire surface and a case insensitive parse is two clients disagreeing about who is an admin",
				rejected, role)
		}
	}
	if Role(4).String() != "unknown" || Role(255).String() != "unknown" {
		t.Errorf("Role(4) renders %q and Role(255) renders %q, want unknown for both",
			Role(4).String(), Role(255).String())
	}
}

func TestGroupPolicyOfMissing(t *testing.T) {
	if _, err := GroupPolicyOf(nil); !errors.Is(err, ErrNoGroupPolicy) {
		t.Fatalf("GroupPolicyOf(nil) error = %v, want ErrNoGroupPolicy", err)
	}
}

// TestGroupPolicyOfReadsTheEntryOfItsOwnTypeAndNoOther is GroupPolicyOf's half of the argument
// LeafKeysOf's own type test makes: a lookup that answered the first entry of the vector would
// hand a role list back out of somebody else's body, and every group context in this profile
// carries required_capabilities and ratchet_tree beside the policy.
func TestGroupPolicyOfReadsTheEntryOfItsOwnTypeAndNoOther(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	leafKeys := testLeafKeys(t, owner)
	policy, err := (&GroupPolicyExtension{
		Roles:    []RoleEntry{{MemberId: owner.IdentityPub, Role: RoleOwner}},
		ServerId: []byte("urmessage-v1-server"),
	}).Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	if _, err := GroupPolicyOf([]Extension{
		{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: []byte{0x00, 0x00, 0x00}},
		leafKeys,
	}); !errors.Is(err, ErrNoGroupPolicy) {
		t.Fatalf("GroupPolicyOf over a list with no 0xF001 error = %v, want ErrNoGroupPolicy", err)
	}

	found, err := GroupPolicyOf([]Extension{
		{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: []byte{0x00, 0x00, 0x00}},
		leafKeys,
		policy,
	})
	if err != nil {
		t.Fatalf("GroupPolicyOf over a list whose 0xF001 entry is not first: %v", err)
	}
	if id, ok := found.OwnerId(); !ok || !bytes.Equal(id, owner.IdentityPub) {
		t.Fatal("GroupPolicyOf answered a policy that is not the one in the list")
	}
}

// TestGroupPolicyAccessorsAnswerTheRoleSetTheyAreGiven covers the six accessors the interface
// registry pins, each of which gains its first caller in this task.
func TestGroupPolicyAccessorsAnswerTheRoleSetTheyAreGiven(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	admin := testIdentity(t, crypto, "admin")
	member := testIdentity(t, crypto, "member")
	stranger := testIdentity(t, crypto, "stranger")
	policy := testPolicy(t, owner, admin, member)

	if role, ok := policy.RoleOf(stranger.IdentityPub); ok || role != RoleMember {
		t.Errorf("RoleOf(a member the policy does not name) = %v %v, want member false; MASTER section 11 rules the unnamed member a MEMBER", role, ok)
	}
	if count := policy.AdminCount(); count != 1 {
		t.Errorf("AdminCount = %d, want 1; the owner is not an admin, and counting them would let a group with one admin meet a two admin quorum", count)
	}

	// SetRole on a member already named replaces the role and adds no entry
	before := len(policy.Roles)
	policy.SetRole(member.IdentityPub, RoleAdmin)
	if len(policy.Roles) != before {
		t.Errorf("SetRole over a named member grew the role set from %d to %d", before, len(policy.Roles))
	}
	if role, ok := policy.RoleOf(member.IdentityPub); !ok || role != RoleAdmin {
		t.Errorf("RoleOf after SetRole = %v %v, want admin true", role, ok)
	}
	if count := policy.AdminCount(); count != 2 {
		t.Errorf("AdminCount = %d, want 2", count)
	}

	// SetRole on a stranger inserts, keeps the canonical order, and COPIES the id it was handed
	handed := slices.Clone(stranger.IdentityPub)
	policy.SetRole(handed, RoleObserver)
	for i := range handed {
		handed[i] ^= 0xff
	}
	if role, ok := policy.RoleOf(stranger.IdentityPub); !ok || role != RoleObserver {
		t.Errorf("RoleOf after SetRole over a caller's buffer that was then overwritten = %v %v, want observer true; the policy is covered by a transcript hash and cannot alias a caller's slice",
			role, ok)
	}
	if err := policy.Validate(); err != nil {
		t.Errorf("SetRole left the policy in a state Validate refuses: %v", err)
	}

	policy.RemoveRole(stranger.IdentityPub)
	if _, ok := policy.RoleOf(stranger.IdentityPub); ok {
		t.Error("RemoveRole left the member in the role set")
	}
	// removing somebody the policy never named is a no op rather than a corruption
	sizeBefore := len(policy.Roles)
	policy.RemoveRole([]byte("nobody"))
	if len(policy.Roles) != sizeBefore {
		t.Errorf("RemoveRole over an absent member changed the role set from %d to %d entries", sizeBefore, len(policy.Roles))
	}
	if id, ok := policy.OwnerId(); !ok || !bytes.Equal(id, owner.IdentityPub) {
		t.Error("OwnerId no longer answers the owner")
	}
	if _, ok := (&GroupPolicyExtension{}).OwnerId(); ok {
		t.Error("OwnerId reported an owner on a policy with no roles at all")
	}
}

// TestGroupPolicyCloneAnswersEveryRoleEntryInItsOwnPlace is the half the aliasing sweep below
// cannot make, and the difference between the two is one comparison against the source.
//
// That sweep edits the CLONE and asks whether the original moved. A clone that had written every
// entry into one slot, or written the entries in reverse, passes it completely: nothing is shared,
// every array is fresh, and what comes back is a role list of the right length whose member ids
// stand under the wrong roles -- which is the whole of what this defect class produces.
//
// MEASURED. out.Roles[len(self.Roles)-1-i], an order-preserving mispairing that writes every entry,
// writes none twice and leaves no zero behind, survived the entire ./mls/... and ./message/... at
// 1974 passing before this test existed. The weaker pin-to-zero was caught only INCIDENTALLY, by an
// index-out-of-range panic inside the sweep below, whose stated property is not this one -- so the
// class had a member covered by an accident and nineteen others covered by nothing anyone had
// checked. index_pairing_test.go derives the class and holds each of them to a test by name.
//
// The pairing is what a policy MEANS: a member id under the wrong role is the removal authority
// check reading the wrong answer about who may remove whom, in a structure covered by every
// confirmation tag the group has ever produced.
func TestGroupPolicyCloneAnswersEveryRoleEntryInItsOwnPlace(t *testing.T) {
	crypto := testCrypto(t)
	live := testPolicy(t, testIdentity(t, crypto, "owner"), testIdentity(t, crypto, "admin"),
		testIdentity(t, crypto, "member"))
	if len(live.Roles) < 3 {
		t.Fatalf("the fixture holds %d role entry/entries; over one entry a mispairing pairs element zero with element zero and is unobservable",
			len(live.Roles))
	}
	// the fixture's own control: two entries holding one value are two entries no copy can be
	// caught mispairing, and a reversal of a palindrome is the identity.
	for i := range live.Roles {
		for j := i + 1; j < len(live.Roles); j += 1 {
			if live.Roles[i].Role == live.Roles[j].Role &&
				bytes.Equal(live.Roles[i].MemberId, live.Roles[j].MemberId) {
				t.Fatalf("role entries %d and %d hold one value, so this comparison cannot see a copy that swapped them",
					i, j)
			}
		}
	}

	clone := live.Clone()
	if len(clone.Roles) != len(live.Roles) {
		t.Fatalf("Clone answered %d role entry/entries for %d", len(clone.Roles), len(live.Roles))
	}
	for i := range live.Roles {
		if clone.Roles[i].Role != live.Roles[i].Role ||
			!bytes.Equal(clone.Roles[i].MemberId, live.Roles[i].MemberId) {
			t.Errorf("clone entry %d is %v %x and the source entry is %v %x; a copy that pairs two sequences by hand answers one member's id under another member's role",
				i, clone.Roles[i].Role, clone.Roles[i].MemberId,
				live.Roles[i].Role, live.Roles[i].MemberId)
		}
	}
	// and the three fields beside the roles, since a copy that dropped one of them is the same
	// defect one level up and this is the only place the whole value is compared.
	if clone.RetentionPolicy != live.RetentionPolicy ||
		!slices.Equal(clone.DisappearingBuckets, live.DisappearingBuckets) ||
		!bytes.Equal(clone.ServerId, live.ServerId) {
		t.Errorf("Clone answered %+v for %+v", clone, live)
	}
	// the roles read back through the accessor as well, which is how every caller of a policy asks
	// the question this pairing answers.
	for i := range live.Roles {
		role, found := clone.RoleOf(live.Roles[i].MemberId)
		if !found || role != live.Roles[i].Role {
			t.Errorf("the clone answers %v %v for the member the source gives %v; a mispaired copy is a policy that disagrees with itself about who may remove whom",
				role, found, live.Roles[i].Role)
		}
	}
}

// TestGroupPolicyCloneSharesNothingWithWhatItWasClonedFrom is what Clone exists for: a staged
// commit mutates its copy while the live epoch is still running on the original, and a shallow
// copy would rewrite a role inside the epoch the group has not left yet.
func TestGroupPolicyCloneSharesNothingWithWhatItWasClonedFrom(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	admin := testIdentity(t, crypto, "admin")
	member := testIdentity(t, crypto, "member")
	live := testPolicy(t, owner, admin, member)
	staged := live.Clone()

	staged.SetRole(admin.IdentityPub, RoleObserver)
	staged.RemoveRole(member.IdentityPub)
	staged.RetentionPolicy.DurableMs = 1
	staged.DisappearingBuckets[0] = 9
	staged.ServerId[0] = 'X'
	// and the member id array itself, which a Clone that copied the slice header alone shares
	staged.Roles[0].MemberId[0] ^= 0xff

	if role, ok := live.RoleOf(admin.IdentityPub); !ok || role != RoleAdmin {
		t.Errorf("the live policy's admin is now %v %v after the staged copy was edited", role, ok)
	}
	if _, ok := live.RoleOf(member.IdentityPub); !ok {
		t.Error("the live policy lost a member when the staged copy removed one")
	}
	if live.RetentionPolicy.DurableMs != 0 || live.DisappearingBuckets[0] != 0 || live.ServerId[0] != 'u' {
		t.Errorf("the live policy's retention, buckets or server id followed the staged copy: %+v", live)
	}
	if err := live.Validate(); err != nil {
		t.Errorf("the live policy no longer validates after its clone was edited: %v", err)
	}
}

// ---------------------------------------------------------------------------
// every way this file refuses a role list, derived from the file
// ---------------------------------------------------------------------------

// groupPolicyRefusalSite is one place group_policy.go can answer a refusal from: the declaration
// it is written in, the sentinel it answers, an ordinal that separates two sites of one
// declaration answering the same sentinel, and the pattern the MESSAGE at that site must match.
//
// The pattern is what makes this a site and not merely a sentinel. Validate refuses an undefined
// role byte and an empty member id with the same value, so a table row that reached the wrong one
// of the two would satisfy every errors.Is assertion there is; the format string is read off the
// site's own fmt.Errorf call, so a row is held to the clause it claims rather than to the value.
type groupPolicyRefusalSite struct {
	site     string
	sentinel string
	pattern  string
}

// groupPolicyVerb matches one printf verb, so a format string can be turned into the pattern the
// message it produces has to match.
var groupPolicyVerb = regexp.MustCompile(`%[-+# 0]*[0-9]*(\.[0-9]+)?[a-zA-Z]`)

// groupPolicyMessagePattern turns a format string into a regular expression: every literal run of
// it, quoted, joined by what a verb can expand to.
func groupPolicyMessagePattern(format string) string {
	chunks := groupPolicyVerb.Split(format, -1)
	quoted := []string{}
	for _, chunk := range chunks {
		quoted = append(quoted, regexp.QuoteMeta(chunk))
	}
	return strings.Join(quoted, ".*")
}

// groupPolicySentinelClass is what a refusal of this package ANSWERS, read off the type checker.
//
// Every package level variable whose type implements error, exported or not. The predicate below
// used to require the name to begin with "Err", which is the exported registry's convention and
// not the package's: errCredentialTypeNotListed, errMissingRequiredCapability, errPathLength,
// errDuplicatePsk and errLeafExtensionNotListed are all refusals this package makes today, and a
// clause of Validate answering one of them was invisible -- the site never joined the derived
// class, the table below stayed equal to the class, and the gate reported a clause it had run no
// input through. What a refusal answers is not decided by the first three letters of a name. It
// is decided by the type, so go/types decides it here.
func groupPolicySentinelClass(t *testing.T) []string {
	t.Helper()
	errorInterface, isInterface := types.Universe.Lookup("error").Type().Underlying().(*types.Interface)
	if !isInterface {
		t.Fatal("the universe scope's error is not an interface, so this class is read off nothing")
	}
	scope := typeCheckedPackage(t).Scope()
	names := []string{}
	for _, name := range scope.Names() {
		declared, isVariable := scope.Lookup(name).(*types.Var)
		if !isVariable || !types.Implements(declared.Type(), errorInterface) {
			continue
		}
		names = append(names, name)
	}
	// the positive control, on BOTH halves of what was widened. A class holding no exported
	// sentinel is one reading the wrong package; a class holding no unexported one is the class
	// this function was written to replace, reporting exactly what a complete one reports.
	for _, wanted := range []string{"ErrMalformedExtension", "errPathLength"} {
		if !slices.Contains(names, wanted) {
			t.Fatalf("the sentinel class read off the type checker is %v and does not hold %s, so it is not reading this package's error variables",
				names, wanted)
		}
	}
	return names
}

// groupPolicyRefusal is what one returned expression refuses with: the sentinel it answers and
// the format it answers it under. The empty sentinel means the expression is not a refusal.
type groupPolicyRefusal struct {
	sentinel string
	format   string
}

// groupPolicyRefusalIn resolves one returned expression to the refusal it makes.
//
// DERIVED on the axis the sweep below was already derived on AND on the one it was not. The
// sweep reads every return statement of the file off the AST, which is genuinely derived; what
// it then DID with each return was a two entry hand list -- a bare Err-prefixed identifier, or a
// DIRECT fmt.Errorf carrying one -- and two entirely ordinary refusals fell outside it in
// silence. A clause answering an unexported sentinel, which is this package's dominant
// convention for a non-registry error. And a clause that builds its fmt.Errorf into a local and
// returns the variable. Neither grows the derived class, so the table stays equal to it and the
// gate reports a clause it never ran an input through: a gate that derives one axis and
// enumerates another is not a derived gate.
//
// So the question is asked as what a refusal IS rather than as how one is spelled:
//
//   - an identifier naming one of this package's error variables IS the refusal it names;
//   - an identifier naming a LOCAL is whatever that local was built from, resolved in the body it
//     was assigned in, which is what makes `refusal := fmt.Errorf(...); return refusal` the same
//     refusal as returning the call;
//   - a CALL is a refusal when one of its arguments is. That covers fmt.Errorf, errors.Join, a
//     ValSem wrapper and anything a later task writes, rather than the one function name that
//     happens to be used today. Its FORMAT is its first argument when that is a string literal,
//     which is the shape every wrapper of this package has and is what tells two clauses
//     answering one sentinel apart.
//
// A local resolves through EVERY assignment to that name rather than the nearest one, so the
// answer is conservative in the direction that fails loudly: a name ever built from a sentinel is
// reported, and an over-reported site is a site with no row, which is a failure naming it.
// Under-reporting is silent, and is the whole reason this function was rewritten.
func groupPolicyRefusalIn(body *ast.BlockStmt, expression ast.Expr, sentinels []string) groupPolicyRefusal {
	return groupPolicyRefusalOf(body, expression, sentinels, map[string]bool{})
}

// groupPolicyRefusalOf is the recursion, carrying the local names already followed so a body that
// assigns a name from itself -- compareMemberIds folds its running answer through exactly that
// shape -- terminates rather than recursing forever.
func groupPolicyRefusalOf(body *ast.BlockStmt, expression ast.Expr, sentinels []string, seen map[string]bool) groupPolicyRefusal {
	switch node := expression.(type) {
	case *ast.Ident:
		if slices.Contains(sentinels, node.Name) {
			return groupPolicyRefusal{sentinel: node.Name}
		}
		if body == nil || seen[node.Name] {
			return groupPolicyRefusal{}
		}
		followed := maps.Clone(seen)
		followed[node.Name] = true
		for _, assigned := range groupPolicyAssignmentsTo(body, node.Name) {
			if found := groupPolicyRefusalOf(body, assigned, sentinels, followed); found.sentinel != "" {
				return found
			}
		}
	case *ast.CallExpr:
		format := ""
		if len(node.Args) != 0 {
			literal, isLiteral := node.Args[0].(*ast.BasicLit)
			if isLiteral && literal.Kind == token.STRING {
				if unquoted, err := strconv.Unquote(literal.Value); err == nil {
					format = unquoted
				}
			}
		}
		for _, argument := range node.Args {
			if found := groupPolicyRefusalOf(body, argument, sentinels, maps.Clone(seen)); found.sentinel != "" {
				if found.format == "" {
					found.format = format
				}
				return found
			}
		}
	}
	return groupPolicyRefusal{}
}

// groupPolicyAssignmentsTo is every expression one name is assigned inside a body, including a
// var declaration's initialiser.
//
// A one expression right hand side is attributed to EVERY name on the left, which is what keeps
// `value, err := wrap(ErrSomething)` resolvable: the call is what both names came from, and a
// reader that only paired them positionally would drop the error half of every two result call.
func groupPolicyAssignmentsTo(body *ast.BlockStmt, name string) []ast.Expr {
	assigned := []ast.Expr{}
	take := func(names []*ast.Ident, values []ast.Expr) {
		for i, target := range names {
			if target.Name != name {
				continue
			}
			if len(values) == 1 {
				assigned = append(assigned, values[0])
				continue
			}
			if i < len(values) {
				assigned = append(assigned, values[i])
			}
		}
	}
	ast.Inspect(body, func(node ast.Node) bool {
		switch statement := node.(type) {
		case *ast.AssignStmt:
			targets := []*ast.Ident{}
			for _, one := range statement.Lhs {
				identifier, isIdentifier := one.(*ast.Ident)
				if !isIdentifier {
					identifier = ast.NewIdent("")
				}
				targets = append(targets, identifier)
			}
			take(targets, statement.Rhs)
		case *ast.ValueSpec:
			take(statement.Names, statement.Values)
		}
		return true
	})
	return assigned
}

// groupPolicyRefusalSitesOf is the class the sweep below runs over: every refusal group_policy.go
// makes, read off the file's own return statements.
//
// DERIVED and not written down, which is the whole point of it. "Every way a role list is
// refused" has been written as a hand list fifteen times on this project and understated itself
// fifteen times, and the shape it understates itself in is always the same: the list holds the
// refusals somebody remembered and the file grew one more. A clause added to Validate arrives in
// this class on the commit that writes it and fails for having no input, rather than being
// covered by a table that says it is.
func groupPolicyRefusalSitesOf(t *testing.T) []groupPolicyRefusalSite {
	t.Helper()
	sentinels := groupPolicySentinelClass(t)
	parsed := mustParseSource(t, groupPolicySourceFile)
	seen := map[string]int{}
	sites := []groupPolicyRefusalSite{}
	for _, one := range declaredIn(parsed) {
		if one.body == nil {
			continue
		}
		where := one.name
		if one.receiver != "" {
			where = "(" + one.receiver + ")." + one.name
		}
		ast.Inspect(one.body, func(node ast.Node) bool {
			returned, isReturn := node.(*ast.ReturnStmt)
			if !isReturn {
				return true
			}
			for _, result := range returned.Results {
				refusal := groupPolicyRefusalIn(one.body, result, sentinels)
				if refusal.sentinel == "" {
					continue
				}
				key := where + ":" + refusal.sentinel
				sites = append(sites, groupPolicyRefusalSite{
					site:     fmt.Sprintf("%s#%d", key, seen[key]),
					sentinel: refusal.sentinel,
					pattern:  groupPolicyMessagePattern(refusal.format),
				})
				seen[key] += 1
			}
			return true
		})
	}
	if len(sites) == 0 {
		t.Fatalf("no declaration of %s answers a sentinel, so the sweep below demands nothing", groupPolicySourceFile)
	}
	slices.SortFunc(sites, func(left groupPolicyRefusalSite, right groupPolicyRefusalSite) int {
		return strings.Compare(left.site, right.site)
	})
	return sites
}

// groupPolicyRefusalRow is one input that reaches one site.
//
// A row states EITHER a policy or a call. The policy form is for the sites inside Validate, and
// it is what lets the encode gate below be held to the same derived class rather than to the two
// refusals somebody thought of: a row that carried only a closure could be run, but nothing else
// could ask what value it was about.
type groupPolicyRefusalRow struct {
	policy *GroupPolicyExtension
	call   func(t *testing.T) error
}

// run is the row's input, whichever form it was written in.
func (self groupPolicyRefusalRow) run(t *testing.T) error {
	t.Helper()
	if self.call != nil {
		return self.call(t)
	}
	if self.policy == nil {
		t.Fatal("a refusal row states neither a policy nor a call, so it observes nothing")
	}
	return self.policy.Validate()
}

// the two member ids the rows order against, fixed rather than minted, so the row that claims an
// ordering failure produces one every run
var (
	groupPolicyLowId  = []byte{0x01, 0x11}
	groupPolicyHighId = []byte{0x02, 0x22}
)

// groupPolicyRoleByteOffset is where the one role octet sits in the body below: past the roles
// vector's length prefix, past the member id's, and past the member id itself. Written as the sum
// of the three rather than as a number, so the rows that rewrite it move with the fixture, and
// checked against the body it indexes by groupPolicyWellFormedBody -- an edit to the wrong offset
// is a body that fails to decode for a reason nobody asked about, which is how a hostile input
// test comes to assert nothing.
var groupPolicyRoleByteOffset = 1 + 1 + len(groupPolicyLowId)

// groupPolicyWellFormedBody is one owner and nothing else, encoded past the policy gate, which is
// the run the hostile rows edit.
func groupPolicyWellFormedBody(t *testing.T) []byte {
	t.Helper()
	body, err := (&GroupPolicyExtension{
		Roles: []RoleEntry{{MemberId: groupPolicyLowId, Role: RoleOwner}},
	}).encodeUnchecked()
	if err != nil {
		t.Fatalf("the well formed body the hostile rows edit: %v", err)
	}
	if body[groupPolicyRoleByteOffset] != uint8(RoleOwner) {
		t.Fatalf("the body %x carries %d at the offset this fixture calls the role octet, want %d; every hostile row below edits that offset",
			body, body[groupPolicyRoleByteOffset], uint8(RoleOwner))
	}
	return body
}

// groupPolicyRefusalRows is the table the derived class above is held against, in both
// directions: a site with no row fails rather than going unrun, and a row for a refusal this file
// no longer makes fails rather than outliving it.
func groupPolicyRefusalRows() map[string]groupPolicyRefusalRow {
	return map[string]groupPolicyRefusalRow{
		// no row for the repeated 0xF001 entry, and that is the derivation working rather than a
		// gap. The class above is every sentinel group_policy.go NAMES at a refusal, and the
		// repeat is refused by FindExtensionEntry now -- one door for the package instead of one
		// per accessor -- so this file names nothing for it. What that row asserted is asserted
		// where the refusal is made and where it arrives:
		// TestFindExtensionRefusesAVectorCarryingARepeatedTypeRatherThanAnsweringByPosition over
		// every declared extension type, and
		// TestGroupPolicyOfRefusesAListCarryingTwoPoliciesRatherThanPickingOne over the accessor,
		// which also holds it to NOT answering ErrNoGroupPolicy -- the exclusivity half this
		// sweep used to give it.
		"GroupPolicyOf:ErrNoGroupPolicy#0": {call: func(t *testing.T) error {
			_, err := GroupPolicyOf([]Extension{
				{ExtensionType: ExtensionTypeUrmessageLeafKeys, ExtensionData: []byte{0x00}},
			})
			return err
		}},
		"ParseGroupPolicyFrom:ErrMalformedExtension#0": {call: func(t *testing.T) error {
			_, err := ParseGroupPolicyFrom(Extension{
				ExtensionType: ExtensionTypeUrmessageLeafKeys,
				ExtensionData: groupPolicyWellFormedBody(t),
			})
			return err
		}},
		"ParseRole:ErrMalformedExtension#0": {call: func(t *testing.T) error {
			_, err := ParseRole("superuser")
			return err
		}},
		// the decode side's own role byte gate, reached by rewriting the one role octet of a
		// body a peer would otherwise have sent
		"readOneRoleEntry:ErrMalformedExtension#0": {call: func(t *testing.T) error {
			body := groupPolicyWellFormedBody(t)
			body[groupPolicyRoleByteOffset] = 9
			_, err := ParseGroupPolicyExtension(body)
			return err
		}},
		// the encode side's, reached through encodeUnchecked because Encode's own Validate
		// refuses the same value one clause earlier
		"writeOneRoleEntry:ErrMalformedExtension#0": {call: func(t *testing.T) error {
			_, err := (&GroupPolicyExtension{
				Roles: []RoleEntry{{MemberId: groupPolicyLowId, Role: Role(9)}},
			}).encodeUnchecked()
			return err
		}},
		"(*GroupPolicyExtension).Validate:ErrMalformedExtension#0": {policy: &GroupPolicyExtension{
			Roles: []RoleEntry{{MemberId: groupPolicyLowId, Role: Role(9)}},
		}},
		"(*GroupPolicyExtension).Validate:ErrMalformedExtension#1": {policy: &GroupPolicyExtension{
			Roles: []RoleEntry{{MemberId: nil, Role: RoleOwner}},
		}},
		"(*GroupPolicyExtension).Validate:ErrRolesNotCanonical#0": {policy: &GroupPolicyExtension{Roles: []RoleEntry{
			{MemberId: groupPolicyHighId, Role: RoleOwner},
			{MemberId: groupPolicyLowId, Role: RoleMember},
		}}},
		"(*GroupPolicyExtension).Validate:ErrDuplicateRoleEntry#0": {policy: &GroupPolicyExtension{Roles: []RoleEntry{
			{MemberId: groupPolicyLowId, Role: RoleOwner},
			{MemberId: groupPolicyLowId, Role: RoleMember},
		}}},
		"(*GroupPolicyExtension).Validate:ErrNoOwner#0": {policy: &GroupPolicyExtension{
			Roles: []RoleEntry{{MemberId: groupPolicyLowId, Role: RoleMember}},
		}},
		"(*GroupPolicyExtension).Validate:ErrMultipleOwners#0": {policy: &GroupPolicyExtension{Roles: []RoleEntry{
			{MemberId: groupPolicyLowId, Role: RoleOwner},
			{MemberId: groupPolicyHighId, Role: RoleOwner},
		}}},
		"(*GroupPolicyExtension).Canonicalize:ErrDuplicateRoleEntry#0": {call: func(t *testing.T) error {
			return (&GroupPolicyExtension{Roles: []RoleEntry{
				{MemberId: groupPolicyHighId, Role: RoleOwner},
				{MemberId: groupPolicyHighId, Role: RoleMember},
			}}).Canonicalize()
		}},
	}
}

// TestEncodeRefusesEveryPolicyValidateRefuses is the encode gate, held to the DERIVED class of
// Validate's refusals rather than to the one or two an encode test would have thought of.
//
// Encode's whole contribution over encodeUnchecked is the Validate call in front of it, and that
// call is one line: delete it and every test in this file still passes, because the codec refuses
// an undefined role byte on its own and nothing else here asks Encode about a policy Validate
// would refuse. What that would ship is an encoder that produces a two owner or non canonical
// group policy, which then goes into a group context, is covered by the confirmed transcript
// hash, and is refused by every peer -- at the far end, with nothing pointing back at the encode.
//
// The class is the Validate sites the scan above reads, so a clause added to Validate is asked of
// Encode too by the commit that writes it.
func TestEncodeRefusesEveryPolicyValidateRefuses(t *testing.T) {
	rows := groupPolicyRefusalRows()
	covered := 0
	for _, one := range groupPolicyRefusalSitesOf(t) {
		if !strings.HasPrefix(one.site, "(*GroupPolicyExtension).Validate:") {
			continue
		}
		row, held := rows[one.site]
		if !held || row.policy == nil {
			t.Errorf("%s has no policy on its row, so Encode is not asked about the value that refusal is about", one.site)
			continue
		}
		covered += 1
		sentinel, named := lifecycleOwnedErrors[one.sentinel]
		if !named {
			t.Errorf("%s answers %s, which is not a lifecycle sentinel", one.site, one.sentinel)
			continue
		}
		encoded, err := row.policy.Encode()
		if !errors.Is(err, sentinel) {
			t.Errorf("Encode over the policy %s is about answered %v, want %s; the gate in front of the encoder is one line and nothing else in this file asks about it",
				one.site, err, one.sentinel)
		}
		if len(encoded.ExtensionData) != 0 || encoded.ExtensionType != 0 {
			t.Errorf("Encode answered the entry %+v beside its refusal for %s; a refused policy must never reach a caller as bytes it could sign",
				encoded, one.site)
		}
	}
	if covered == 0 {
		t.Fatalf("no refusal site of %s sits inside Validate, so this gate asked Encode about nothing", groupPolicySourceFile)
	}
	t.Logf("Encode is held to the %d refusals Validate makes", covered)
}

// TestEveryRefusalTheGroupPolicyMakesHasAnInputThatProducesItAndAnswersNoOther is guardrail 5 over
// this file: the class is read off the enforcement and the table is held to it in both directions,
// so "every way a role list is refused" is a question the SOURCE answers.
//
// Three things are asserted per site and each of them has been the whole defect somewhere on this
// project. The refusal is reachable at all -- a clause no input reaches is a rule nothing enforces.
// It answers the sentinel the site names AND no other sentinel this package declares -- four
// refusals that all reduce to one comparison is the shape that passed every test on the CreateGroup
// carve-out, and errors.Is over the whole lifecycle set is what separates them. And the MESSAGE
// matches the format written at that site, which is what tells the two ErrMalformedExtension
// clauses of Validate apart: a row that reached the wrong one of the two satisfies both of the
// first two assertions.
func TestEveryRefusalTheGroupPolicyMakesHasAnInputThatProducesItAndAnswersNoOther(t *testing.T) {
	// the pattern derivation's own control, on both halves. A derivation that collapsed a format
	// to ".*" would hold every row below to nothing and report exactly what a working one reports,
	// so the pattern has to keep the format's literal text AND has to refuse a message that does
	// not carry it.
	control := groupPolicyMessagePattern("%w: entry %d is out of order")
	if !strings.Contains(control, "is out of order") || strings.TrimLeft(control, ".*") == "" {
		t.Fatalf("the message pattern derivation produced %q out of a two verb format; a pattern that keeps none of the literal text matches everything", control)
	}
	if matched, err := regexp.MatchString(control,
		"mls: group policy roles are not sorted by member id: entry 1 is out of order"); err != nil || !matched {
		t.Fatalf("the derived pattern %q does not match the message that format produces (matched=%v err=%v)", control, matched, err)
	}
	if matched, err := regexp.MatchString(control, "mls: something else entirely"); err != nil || matched {
		t.Fatalf("the derived pattern %q matched an unrelated message (matched=%v err=%v)", control, matched, err)
	}

	sites := groupPolicyRefusalSitesOf(t)
	rows := groupPolicyRefusalRows()
	named := []string{}
	for _, one := range sites {
		named = append(named, one.site)
	}
	if held := slices.Sorted(maps.Keys(rows)); !slices.Equal(named, held) {
		t.Fatalf("%s refuses at %v and this table holds %v; a refusal with no input is one nothing here runs, and a row for a refusal this file no longer makes is one that outlived it",
			groupPolicySourceFile, named, held)
	}
	t.Logf("%d refusal sites derived from %s: %v", len(sites), groupPolicySourceFile, named)

	for _, one := range sites {
		sentinel, held := lifecycleOwnedErrors[one.sentinel]
		if !held {
			t.Errorf("%s answers %s, which is not one of the lifecycle sentinels this package sweeps; nothing holds it apart from any other value",
				one.site, one.sentinel)
			continue
		}
		err := rows[one.site].run(t)
		if err == nil {
			t.Errorf("%s: the input for this refusal was accepted", one.site)
			continue
		}
		if !errors.Is(err, sentinel) {
			t.Errorf("%s answered %v, want %s", one.site, err, one.sentinel)
			continue
		}
		for _, other := range slices.Sorted(maps.Keys(lifecycleOwnedErrors)) {
			if other == one.sentinel {
				continue
			}
			if errors.Is(err, lifecycleOwnedErrors[other]) {
				t.Errorf("%s answered a value that is also %s; two refusals a caller cannot tell apart are one refusal wearing two names",
					one.site, other)
			}
		}
		pattern := one.pattern
		if pattern == "" {
			pattern = regexp.QuoteMeta(sentinel.Error())
		}
		matched, matchErr := regexp.MatchString(pattern, err.Error())
		if matchErr != nil {
			t.Errorf("%s: the pattern derived from its format is not a regular expression: %v", one.site, matchErr)
			continue
		}
		if !matched {
			t.Errorf("%s answered %q, which does not match the message written at that site (%s); this input reaches some OTHER clause answering the same sentinel",
				one.site, err.Error(), pattern)
		}
	}
}

// ---------------------------------------------------------------------------
// the resolver's own control, the repeated entry, the refused decode and the stable sort
// ---------------------------------------------------------------------------

// groupPolicyRefusalShapesControl is a body written to hold the refusal spellings this package
// makes and the plumbing it must not mistake for one, so the resolver above is measured on a
// source whose answer is known rather than only on a file it agrees with.
//
// Two of these shapes are the ones the previous predicate could not see, and they are the reason
// this control exists: production source does not carry them today, so a widening tested only
// against production source is a widening nothing observes.
const groupPolicyRefusalShapesControl = `package control

var (
	ErrExported   = errors.New("control: an exported sentinel")
	errUnexported = errors.New("control: an unexported sentinel")
	notAnError    = 3
)

func answersAnExportedSentinel() error {
	return ErrExported
}

func answersAnUnexportedSentinel() error {
	return errUnexported
}

func wrapsDirectly() error {
	return fmt.Errorf("%w: entry %d is out of order", ErrExported, 1)
}

func buildsIntoALocal() error {
	refusal := fmt.Errorf("%w: built into a local first", errUnexported)
	return refusal
}

func buildsIntoALocalUnderABranch(bad bool) error {
	var refusal error
	if bad {
		refusal = fmt.Errorf("%w: built under a branch", errUnexported)
	}
	return refusal
}

func wrapsThroughSomethingOtherThanErrorf() error {
	return errors.Join(ErrExported, errUnexported)
}

func plumbsSomebodyElsesError(data []byte) error {
	value, err := decodeSomething(data)
	if err != nil {
		return err
	}
	return use(value)
}

func answersAPackageVarThatIsNotAnError() error {
	return notAnError
}

func refusesNothing() error {
	return nil
}
`

// groupPolicyControlRefusalsOf resolves every return of one declaration of a parsed control.
func groupPolicyControlRefusalsOf(t *testing.T, parsed parsedSource, name string, sentinels []string) []groupPolicyRefusal {
	t.Helper()
	found := []groupPolicyRefusal{}
	for _, one := range declaredIn(parsed) {
		if one.name != name || one.body == nil {
			continue
		}
		ast.Inspect(one.body, func(node ast.Node) bool {
			returned, isReturn := node.(*ast.ReturnStmt)
			if !isReturn {
				return true
			}
			for _, result := range returned.Results {
				if refusal := groupPolicyRefusalIn(one.body, result, sentinels); refusal.sentinel != "" {
					found = append(found, refusal)
				}
			}
			return true
		})
		return found
	}
	t.Fatalf("the control declares no %s, so the expectation for it observes nothing", name)
	return nil
}

// TestTheRefusalResolverReadsEveryShapeAPackageRefusalTakes is guardrail 5 applied to the gate
// rather than to the code, which is where this file had it backwards.
//
// The sweep it feeds was derived off the AST and then handed a two entry hand list of spellings,
// and the two shapes missing from that list are not exotic: an unexported sentinel is what five
// refusals of this package answer today, and a fmt.Errorf built into a local is what any clause
// that wants to log before returning looks like. Both left the derived class at twelve sites and
// both left the two sweeps below passing, so the next clause added to Validate would have been
// reported as covered by a gate that ran nothing through it.
//
// The negative half is asserted too. A resolver that answered "refusal" for every return would
// satisfy every positive row here and would put a site on every plumbing return of the file,
// which is a class nobody could write rows for -- so the plumbing shapes must come back empty.
func TestTheRefusalResolverReadsEveryShapeAPackageRefusalTakes(t *testing.T) {
	parsed := mustParseText(t, "the refusal shapes control", groupPolicyRefusalShapesControl)
	// the control's own sentinels, named here rather than type checked, because what this test
	// is about is the RESOLUTION; the class itself has its own control inside
	// groupPolicySentinelClass, which fails if this package's unexported sentinels stop being
	// read.
	sentinels := []string{"ErrExported", "errUnexported"}
	for _, one := range []struct {
		declaration string
		sentinel    string
		format      string
		why         string
	}{
		{declaration: "answersAnExportedSentinel", sentinel: "ErrExported",
			why: "the shape the previous predicate did read"},
		{declaration: "answersAnUnexportedSentinel", sentinel: "errUnexported",
			why: "an unexported sentinel, which is what five refusals of this package answer today"},
		{declaration: "wrapsDirectly", sentinel: "ErrExported", format: "%w: entry %d is out of order",
			why: "the other shape the previous predicate did read"},
		{declaration: "buildsIntoALocal", sentinel: "errUnexported", format: "%w: built into a local first",
			why: "a fmt.Errorf assigned to a local and returned by name"},
		{declaration: "buildsIntoALocalUnderABranch", sentinel: "errUnexported", format: "%w: built under a branch",
			why: "the same, assigned under a branch rather than at the point of return"},
		{declaration: "wrapsThroughSomethingOtherThanErrorf", sentinel: "ErrExported",
			why: "a wrapper that is not fmt.Errorf, which a name match on fmt.Errorf cannot see"},
		{declaration: "plumbsSomebodyElsesError", why: "an error handed back from a call that carries no sentinel"},
		{declaration: "answersAPackageVarThatIsNotAnError", why: "a package level variable that is not an error"},
		{declaration: "refusesNothing", why: "a nil return"},
	} {
		found := groupPolicyControlRefusalsOf(t, parsed, one.declaration, sentinels)
		if one.sentinel == "" {
			if len(found) != 0 {
				t.Errorf("%s (%s) resolved to %v; a resolver that reads a refusal into ordinary plumbing puts a site on returns no row can be written for",
					one.declaration, one.why, found)
			}
			continue
		}
		if len(found) != 1 {
			t.Errorf("%s (%s) resolved to %v, want exactly one refusal answering %s",
				one.declaration, one.why, found, one.sentinel)
			continue
		}
		if found[0].sentinel != one.sentinel {
			t.Errorf("%s (%s) answers %s, want %s", one.declaration, one.why, found[0].sentinel, one.sentinel)
		}
		if found[0].format != one.format {
			t.Errorf("%s (%s) carries the format %q, want %q; the format is what tells two clauses answering one sentinel apart",
				one.declaration, one.why, found[0].format, one.format)
		}
	}
}

// groupPolicyPositionsNamedBy is the entry positions a repeat refusal reports, read as the
// numbers in its message rather than as its wording.
//
// Read this way on purpose. The wording is already held by the derived message pattern in the
// sweep above, and what this reader is for is the OTHER half: the refusal must name the two
// entries in the order the vector holds them. That is the only thing left that a walk over the
// vector in the wrong direction changes -- with the repeat refused, first and last are the same
// entry and every other observable is identical -- and "the walk was reversed and nothing
// noticed" is the finding this pair of accessors is being repaired for. A reader that matched
// the sentence would fail on a reworded message and pass on a reversed walk, which is backwards.
func groupPolicyPositionsNamedBy(t *testing.T, err error) []int {
	t.Helper()
	found := []int{}
	for _, digits := range regexp.MustCompile(`[0-9]+`).FindAllString(err.Error(), -1) {
		value, convertErr := strconv.Atoi(digits)
		if convertErr != nil {
			t.Fatalf("the refusal %q carries %q where a position was expected: %v", err, digits, convertErr)
		}
		found = append(found, value)
	}
	return found
}

// TestGroupPolicyOfRefusesAListCarryingTwoPoliciesRatherThanPickingOne is finding 8's answer, and
// the answer is a refusal rather than a test pinning which of two illegal entries wins.
//
// The prior question was whether a repeated extension type is refused anywhere. It was not:
// ValSem209 was named in three comments of this package and implemented in none of them, and
// LeafNode.Validate -- the door those comments pointed at -- walks every entry, range checks every
// urmessage_leaf_keys body, and accepts a leaf carrying two of anything. So the accessor was
// picking the group's policy by iteration order, and the list it picks from is inside the
// CONFIRMED TRANSCRIPT HASH: both role sets are covered by every confirmation tag the group ever
// produced, so a member reading the first and a member reading the second disagree about who may
// remove whom while agreeing on every hash.
//
// It is refused now, at the lookup, once for the package. This test is unchanged by where the
// refusal is made, because what it states is that GroupPolicyOf does not ANSWER such a list --
// which is what a caller holds, and what a delegation that swallowed the lookup's error would
// break.
//
// Both orders are run, because a refusal that only fires when the SECOND entry is the odd one is
// an accessor that still answers by position.
func TestGroupPolicyOfRefusesAListCarryingTwoPoliciesRatherThanPickingOne(t *testing.T) {
	crypto := testCrypto(t)
	first := testIdentity(t, crypto, "first-owner")
	second := testIdentity(t, crypto, "second-owner")
	entryOf := func(m *testMember) Extension {
		entry, err := (&GroupPolicyExtension{
			Roles:    []RoleEntry{{MemberId: m.IdentityPub, Role: RoleOwner}},
			ServerId: []byte("urmessage-v1-server"),
		}).Encode()
		if err != nil {
			t.Fatalf("the %s policy entry: %v", m.Name, err)
		}
		return entry
	}
	firstEntry, secondEntry := entryOf(first), entryOf(second)
	if bytes.Equal(firstEntry.ExtensionData, secondEntry.ExtensionData) {
		t.Fatal("the two policy entries encode identically, so this test cannot tell a refusal from a lucky pick")
	}
	// the control: each entry alone is answered, so a refusal below is about the repeat rather
	// than about either body
	for _, one := range []struct {
		name  string
		entry Extension
		owner *testMember
	}{{"first", firstEntry, first}, {"second", secondEntry, second}} {
		policy, err := GroupPolicyOf([]Extension{
			{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: []byte{0x00, 0x00, 0x00}},
			one.entry,
		})
		if err != nil {
			t.Fatalf("GroupPolicyOf over the %s entry alone: %v", one.name, err)
		}
		if id, named := policy.OwnerId(); !named || !bytes.Equal(id, one.owner.IdentityPub) {
			t.Fatalf("GroupPolicyOf over the %s entry alone answered a policy owned by somebody else", one.name)
		}
	}
	for _, one := range []struct {
		name string
		list []Extension
		at   []int
	}{
		{"first then second", []Extension{firstEntry, secondEntry}, []int{0, 1}},
		{"second then first", []Extension{secondEntry, firstEntry}, []int{0, 1}},
		{"with an unrelated entry between them", []Extension{
			firstEntry,
			{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: []byte{0x00, 0x00, 0x00}},
			secondEntry,
		}, []int{0, 2}},
		{"behind two entries of other types", []Extension{
			{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: []byte{0x00, 0x00, 0x00}},
			{ExtensionType: ExtensionTypeApplicationId, ExtensionData: []byte("urmessage")},
			firstEntry,
			secondEntry,
		}, []int{2, 3}},
	} {
		policy, err := GroupPolicyOf(one.list)
		if !errors.Is(err, ErrMalformedExtension) {
			t.Errorf("GroupPolicyOf over a list carrying urmessage_group_policy twice (%s) answered %v, want ErrMalformedExtension",
				one.name, err)
			continue
		}
		// and NOT as absence. The exclusivity used to be held by the refusal sweep above, whose
		// class is derived from the sentinels group_policy.go names and which no longer holds a
		// row for this refusal because the lookup makes it. A repeat answered as ErrNoGroupPolicy
		// is a list carrying two policies reported as a list carrying none, which is the
		// fail open the whole rule is against.
		if errors.Is(err, ErrNoGroupPolicy) {
			t.Errorf("GroupPolicyOf over a list carrying urmessage_group_policy twice (%s) answered ErrNoGroupPolicy as well, so a list holding two is indistinguishable from one holding none",
				one.name)
		}
		if policy != nil {
			owner, _ := policy.OwnerId()
			t.Errorf("GroupPolicyOf over a list carrying urmessage_group_policy twice (%s) answered the policy owned by %x beside its refusal; which of two policies a member enforces cannot be decided by iteration order over a list the transcript hash covers",
				one.name, owner)
		}
		if at := groupPolicyPositionsNamedBy(t, err); !slices.Equal(at, one.at) {
			t.Errorf("GroupPolicyOf over %s named entries %v, want %v; the refusal has to point at the two entries in the order the vector holds them, which is the last thing a walk in the wrong direction changes",
				one.name, at, one.at)
		}
	}
}

// TestEveryContentRefusalOfAGroupPolicyIsMadeAtTheParseDoor is what holds the read side together
// after the rule that a codec decodes and does not judge moved Validate out of UnmarshalMLS.
//
// The risk that change introduces is exactly one: a policy reaching a caller unjudged. RoleOf and
// OwnerId act on whatever they are handed, so a policy naming two owners answers one of them and
// half a group believes the other, inside a structure the confirmed transcript hash covers. So the
// class is DERIVED from the Validate refusals group_policy.go declares -- a clause added to
// Validate is asked this question by the commit that writes it -- and every member of it has to be
// reachable through the entry point a body actually arrives at.
//
// Both halves are asserted per refusal, and the first is the one that says the two rules were
// SEPARATED rather than merely moved: the codec accepts the body, and the parse door refuses it.
// A codec that still refused would be the old arrangement with a second copy of the rule beside it.
func TestEveryContentRefusalOfAGroupPolicyIsMadeAtTheParseDoor(t *testing.T) {
	rows := groupPolicyRefusalRows()
	reached := 0
	for _, one := range groupPolicyRefusalSitesOf(t) {
		if !strings.HasPrefix(one.site, "(*GroupPolicyExtension).Validate:") {
			continue
		}
		row, listed := rows[one.site]
		if !listed || row.policy == nil {
			t.Errorf("%s has no policy on its row, so no body can be built for the refusal it makes", one.site)
			continue
		}
		body, err := row.policy.encodeUnchecked()
		if err != nil {
			// the one Validate clause no wire body can carry: the role byte gate runs on the
			// encode side too, so a body naming an undefined role cannot be assembled at all.
			// Logged rather than skipped silently, because a class that quietly lost every
			// member would report exactly what a complete one reports.
			t.Logf("%s: no body can carry this refusal to a parser, the encoder refuses it first (%v)", one.site, err)
			continue
		}
		sentinel, named := lifecycleOwnedErrors[one.sentinel]
		if !named {
			t.Errorf("%s answers %s, which this gate cannot join to a value; give the sentinel a class entry",
				one.site, one.sentinel)
			continue
		}
		// the codec reads it, because judging is not the codec's job any more
		staged := &GroupPolicyExtension{}
		if err := syntax.Unmarshal(body, staged); err != nil {
			t.Errorf("%s: the CODEC refused the body this refusal is about with %v; a codec that judges is the arrangement this decision moved away from, and the rule is now stated in two places that can disagree",
				one.site, err)
		}
		parsed, err := ParseGroupPolicyFrom(Extension{
			ExtensionType: ExtensionTypeUrmessageGroupPolicy, ExtensionData: body,
		})
		if !errors.Is(err, sentinel) {
			t.Errorf("%s: ParseGroupPolicyFrom over the body that refusal is about answered %v, want %s; a policy that reaches a caller unjudged is one RoleOf and OwnerId answer from",
				one.site, err, one.sentinel)
			continue
		}
		if parsed != nil {
			t.Errorf("%s: the refusal answered a policy as well: %+v", one.site, parsed)
		}
		reached += 1
	}
	if reached == 0 {
		t.Fatalf("no Validate refusal of %s is reachable through the parse door, so every one of these rules has left the read path",
			groupPolicySourceFile)
	}
	t.Logf("%d of Validate's refusals are made at the parse door", reached)

	// and through the two doors above it, so the whole read side is covered rather than the one
	// entry point the loop names. A group context is what a caller actually holds.
	twoOwners := &GroupPolicyExtension{
		Roles: []RoleEntry{
			{MemberId: groupPolicyLowId, Role: RoleOwner},
			{MemberId: groupPolicyHighId, Role: RoleOwner},
		},
		ServerId: []byte("two owners"),
	}
	body, err := twoOwners.encodeUnchecked()
	if err != nil {
		t.Fatalf("encode the two owner policy: %v", err)
	}
	entry := Extension{ExtensionType: ExtensionTypeUrmessageGroupPolicy, ExtensionData: body}
	if _, err := GroupPolicyOf([]Extension{entry}); !errors.Is(err, ErrMultipleOwners) {
		t.Errorf("GroupPolicyOf over a group context carrying a two owner policy answered %v, want ErrMultipleOwners; every consumer of a policy in this package reads it through here",
			err)
	}
	// and the loose body door answers the policy, unjudged, which is what makes the gate below
	// worth having rather than a restatement of the paragraph on it
	unjudged, err := ParseGroupPolicyExtension(body)
	if err != nil {
		t.Fatalf("ParseGroupPolicyExtension refused a body the codec can read: %v", err)
	}
	if owner, named := unjudged.OwnerId(); !named {
		t.Errorf("the unjudged policy names no owner at all: %x", owner)
	}
}

// TestNothingReachesAGroupPolicyThroughTheUnjudgedDoor is what makes ParseGroupPolicyExtension
// safe to leave unjudged, and it is derived rather than promised.
//
// Convention C1 requires that declaration to be bare plumbing -- a byte run in, an MLS structure
// out, and nothing between them but the one codec call -- so the content rules cannot live there
// and live at ParseGroupPolicyFrom instead. What that leaves is an exported door that answers a
// policy nothing has judged, and a second caller of it inside this package would be a policy
// reaching RoleOf and OwnerId with no owner count behind it.
//
// So the callers are read off the package's own non test source, and the only one allowed is the
// door that judges. A gate written as advice in a doc comment is one the next task walks past.
func TestNothingReachesAGroupPolicyThroughTheUnjudgedDoor(t *testing.T) {
	const unjudged = "ParseGroupPolicyExtension"
	const judges = "ParseGroupPolicyFrom"
	callers := []string{}
	found := 0
	for path, parsed := range decoderSourceOfThisPackage(t) {
		for _, one := range declaredIn(parsed) {
			if one.body == nil {
				continue
			}
			where := one.name
			if one.receiver != "" {
				where = "(" + one.receiver + ")." + one.name
			}
			ast.Inspect(one.body, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall {
					return true
				}
				name, isIdentifier := call.Fun.(*ast.Ident)
				if !isIdentifier || name.Name != unjudged {
					return true
				}
				found += 1
				if where != judges {
					callers = append(callers, path+": "+where)
				}
				return true
			})
		}
	}
	// the positive control: a scan that had stopped resolving would report no caller at all,
	// and no caller is exactly what a clean run reports
	if found == 0 {
		t.Fatalf("the scan found no call to %s in this package, and %s certainly makes one, so it is reading nothing",
			unjudged, judges)
	}
	if len(callers) != 0 {
		t.Errorf("%s is reached from %v as well as from %s; that door answers a policy no rule has judged, and a policy naming two owners answers one of them to RoleOf while half the group enforces the other",
			unjudged, callers, judges)
		return
	}
	t.Logf("%d call to %s in this package, all from %s", found, unjudged, judges)
}

// TestARefusedGroupPolicyDecodeLeavesTheCallersPolicyAlone is the property the decoder's own
// comment claims: a decode that refuses part way leaves the receiver as it was.
//
// The static gate in decoder_publish_test.go pins this decoder as one that stages, and it reads
// the receiver's write against the READER's last read. What it cannot model is what a refused
// decode leaves BEHIND, and that is what this observes: a receiver holding a half read policy is
// one that goes into a group context and a transcript hash while its caller was handed an error
// saying the decode did not happen. p6 shipped this exact shape at the outermost decoder for
// Welcome, GroupInfo and KeyPackage.
//
// Every refusal this decoder still has is a truncation, because the content refusals are the parse
// door's now -- see the test above, which is where a clause added to Validate is judged.
func TestARefusedGroupPolicyDecodeLeavesTheCallersPolicyAlone(t *testing.T) {
	held := func() *GroupPolicyExtension {
		return &GroupPolicyExtension{
			Roles: []RoleEntry{
				{MemberId: groupPolicyLowId, Role: RoleOwner},
				{MemberId: groupPolicyHighId, Role: RoleAdmin},
			},
			RetentionPolicy:     RetentionPolicy{DurableMs: 7, MediaMs: 9},
			DisappearingBuckets: []uint8{1, 2},
			ServerId:            []byte("the policy the caller already holds"),
		}
	}
	heldBytes, err := held().encodeUnchecked()
	if err != nil {
		t.Fatalf("encode the policy the caller already holds: %v", err)
	}
	unchanged := func(t *testing.T, what string, receiver *GroupPolicyExtension) {
		t.Helper()
		after, err := receiver.encodeUnchecked()
		if err != nil {
			t.Fatalf("%s: re-encode the receiver after a refused decode: %v", what, err)
		}
		if !bytes.Equal(after, heldBytes) {
			t.Errorf("%s: a refused decode left the caller's policy as\n  %x\nand it was\n  %x\nthe caller was handed an error saying the decode did not happen",
				what, after, heldBytes)
		}
	}

	valid, err := (&GroupPolicyExtension{
		Roles:               []RoleEntry{{MemberId: groupPolicyLowId, Role: RoleOwner}},
		RetentionPolicy:     RetentionPolicy{DurableMs: 1, MediaMs: 2},
		DisappearingBuckets: []uint8{3},
		ServerId:            []byte("valid"),
	}).encodeUnchecked()
	if err != nil {
		t.Fatalf("encode the body the truncations are cut from: %v", err)
	}
	if bytes.Equal(valid, heldBytes) {
		t.Fatal("the held policy and the decoded one encode identically, so the truncations cannot tell an untouched receiver from a clobbered one")
	}
	refusals := 0
	for cut := 0; cut < len(valid); cut += 1 {
		receiver := held()
		if err := receiver.UnmarshalMLS(syntax.NewReader(valid[:cut])); err == nil {
			t.Fatalf("a policy body truncated to %d of %d octets was accepted", cut, len(valid))
		}
		refusals += 1
		unchanged(t, fmt.Sprintf("truncated to %d of %d octets", cut, len(valid)), receiver)
	}
	if refusals == 0 {
		t.Fatal("no truncation was refused, so this states nothing about what a refusal leaves behind")
	}
}

// TestSortingTheRolesKeepsTwoEntriesForOneMemberInTheOrderTheyArrived is the stability
// sortRolesByMemberId's comment claims. It matters, and this is what says so.
//
// Canonicalize sorts IN PLACE and then refuses the duplicate, so the caller still holds the
// reordered slice; a caller repairing the policy by dropping the later of the two entries keeps
// whichever role the sort left first. With an unstable sort that is arbitrary, and the value it
// decides is a member's AUTHORITY in a structure covered by the confirmed transcript hash.
//
// The fixture is the whole of why this test is worth writing rather than asserting. Go's unstable
// sort runs an insertion sort below thirteen elements, which is stable by accident, and it leaves
// an already ascending input alone -- so a fixture of the size every other test in this file uses
// cannot tell the two sorts apart, and a test written over one passes under both. This one is
// built in DESCENDING member id order and swept from seven duplicated members to twenty, and
// every one of those shapes is one the unstable sort reorders.
func TestSortingTheRolesKeepsTwoEntriesForOneMemberInTheOrderTheyArrived(t *testing.T) {
	arrived := func(members int) []RoleEntry {
		roles := []RoleEntry{}
		for i := members; i >= 1; i -= 1 {
			id := bytes.Repeat([]byte{byte(i)}, 32)
			roles = append(roles,
				RoleEntry{MemberId: id, Role: RoleAdmin},
				RoleEntry{MemberId: id, Role: RoleMember})
		}
		return roles
	}
	for members := 7; members <= 20; members += 1 {
		roles := arrived(members)
		sortRolesByMemberId(roles)
		pairs := 0
		for i := 1; i < len(roles); i += 1 {
			if compareMemberIds(roles[i-1].MemberId, roles[i].MemberId) > 0 {
				t.Fatalf("%d members: the sort left entry %d ahead of entry %d, so it did not sort at all",
					members, i-1, i)
			}
			if !sameMemberId(roles[i-1].MemberId, roles[i].MemberId) {
				continue
			}
			pairs += 1
			if roles[i-1].Role != RoleAdmin || roles[i].Role != RoleMember {
				t.Fatalf("%d members: the two entries for member %x came out %s then %s and they arrived %s then %s; an unstable sort decides which role a caller repairing the duplicate keeps",
					members, roles[i].MemberId[:1], roles[i-1].Role, roles[i].Role, RoleAdmin, RoleMember)
			}
		}
		if pairs != members {
			t.Fatalf("%d members: %d adjacent duplicate pairs survived the sort, want %d; the fixture this property is observed on is not the one this test built",
				members, pairs, members)
		}
	}

	// and the same statement through the API a caller reaches: Canonicalize refuses the
	// duplicate AFTER sorting in place, so what the caller is left holding is this order.
	policy := &GroupPolicyExtension{Roles: arrived(13)}
	if err := policy.Canonicalize(); !errors.Is(err, ErrDuplicateRoleEntry) {
		t.Fatalf("Canonicalize over a role list holding every member twice answered %v, want ErrDuplicateRoleEntry", err)
	}
	for i := 1; i < len(policy.Roles); i += 1 {
		if !sameMemberId(policy.Roles[i-1].MemberId, policy.Roles[i].MemberId) {
			continue
		}
		if policy.Roles[i-1].Role != RoleAdmin || policy.Roles[i].Role != RoleMember {
			t.Fatalf("after a refused Canonicalize the caller holds member %x as %s then %s, and it handed in %s then %s",
				policy.Roles[i].MemberId[:1], policy.Roles[i-1].Role, policy.Roles[i].Role, RoleAdmin, RoleMember)
		}
	}
}
