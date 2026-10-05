// The provenance questions asked from OUTSIDE package mls, which is where they were asked when
// they were answered wrongly.
//
// WHY AN EXTERNAL TEST PACKAGE AT ALL. Every other test of this package is package mls and can
// reach `inner` directly, so none of them can tell what an ATTACKER can reach. The two bypasses
// that defeated the previous round were both written in three lines of an importing package, and a
// gate that only ran with the field in scope was never going to see either of them. This file has
// exactly the reach a peer's code has: the exported identifiers and nothing else.
//
// THE TWO BYPASSES, AND WHAT THIS FILE SAYS ABOUT EACH.
//
//	// BYPASS A -- self-confirming, three lines
//	s, _ := mls.NewKeyScheduleFromEpochSecret(crypto, myOwn32Bytes, &decoded.GroupContext)
//	v, _ := s.ConfirmGroupContext(s.ConfirmationTag(decoded.GroupContext.ConfirmedTranscriptHash))
//	cache, _ := mls.NewProposalCache(v)
//
// A is CLOSED, and it is closed as a class rather than by the removal of a name. A key schedule is
// derived from a secret the same caller supplied over a context the same caller supplied, so
// nothing it can answer is authority; the test below asks the compiled method set of *KeySchedule
// whether ANY method answers a verified context, and the door enumeration asks the whole package's
// exported surface the same question. A second self-confirming door added anywhere fails those,
// where a test naming ConfirmGroupContext would have gone on passing.
//
//	// BYPASS B -- the honest joiner flow over an attacker-chosen joiner_secret
//
// B is NOT CLOSED, and this file says so by RUNNING it rather than by describing it. A Welcome is
// HPKE-sealed to a PUBLISHED init key, so anyone holding the victim's KeyPackage supplies the
// joiner_secret AND the group context, and the resulting key schedule, confirmation tag and
// GroupInfo signature are all perfectly consistent with each other. What (*GroupInfo).Verify adds
// is that the signature must be by a member of THE TREE THE CALLER PASSED -- so the attack now
// requires the attacker to supply the tree as well, which the same message hands it for free. The
// PARAMETER the tree arrives in is not the question: p7 task 16's JoinFromWelcome takes it as its
// own ratchetTree []byte and the attacker supplies that byte string too. The test drives both
// readings: against the joiner's own tree the forgery is refused, and against the tree the attacker
// supplied it verifies and a cache binds to epoch 1<<40 of ATTACKER-CHOSEN-GROUP.
//
// WHAT WOULD ACTUALLY CLOSE B is not a shape of this constructor. It is (*RatchetTree)
// .ValidateAgainstContext run by whoever obtained the tree, plus at least one leaf whose credential
// matches an identity the joiner ALREADY TRUSTS -- an authentication service, which this build does
// not have. Until that lands, the caller of VerifiedContext is the party making the authority
// claim, and the honest thing is a test that says which half is held.
package mls_test

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls"
	"github.com/urnetwork/connect/mls/syntax"
)

// The group an attacker names when it gets to choose, and the epoch it chooses. Written down once
// because three tests below assert the cache did or did not end up bound to this pair.
const (
	attackersGroupId = "ATTACKER-CHOSEN-GROUP"
	attackersEpoch   = uint64(1) << 40
)

// externalMember is one leaf's key material, kept because a signer outside this package has to hold
// its own private key: the tree publishes only the public half.
type externalMember struct {
	signaturePriv mls.SignaturePrivateKey
	signaturePub  mls.SignaturePublicKey
}

// externalTree builds an n member ratchet tree using nothing but the exported surface.
//
// A rebuild rather than a reach into the package's own fixture, and the duplication is the point:
// what this file is for is what a party outside the package can construct, and a helper it could
// not have written would be testing a reach it does not have. Every leaf is signed at its own index
// under LeafNodeSourceUpdate, which is the source that binds group id and leaf index into the
// signature, so a tree built here is one (*GroupInfo).Verify will accept signatures against.
func externalTree(t *testing.T, crypto mls.CryptoProvider, groupId []byte,
	n uint32) (*mls.RatchetTree, []externalMember) {
	t.Helper()
	tree := mls.NewRatchetTree()
	members := []externalMember{}
	for i := uint32(0); i < n; i += 1 {
		signaturePriv, signaturePub, err := crypto.SignatureKeyPair()
		if err != nil {
			t.Fatalf("SignatureKeyPair(%d): %v", i, err)
		}
		_, encryptionPub, err := crypto.DeriveKeyPair(crypto.Random(crypto.HashSize()))
		if err != nil {
			t.Fatalf("DeriveKeyPair(%d): %v", i, err)
		}
		leafKeys := &mls.LeafKeysExtension{
			AlgId:          mls.AlgIdXwing,
			DeviceXwingPub: crypto.Random(mls.XwingPublicKeyLen),
		}
		leafKeysExtension, err := leafKeys.Encode()
		if err != nil {
			t.Fatalf("LeafKeysExtension.Encode(%d): %v", i, err)
		}
		leaf := &mls.LeafNode{
			EncryptionKey: encryptionPub,
			SignatureKey:  signaturePub,
			Credential:    mls.BasicCredential([]byte("outsider")),
			Capabilities: mls.Capabilities{
				Versions:     []mls.ProtocolVersion{mls.ProtocolVersionMls10},
				CipherSuites: []mls.CipherSuite{mls.CipherSuiteX25519ChaCha20Sha256Ed25519},
				Extensions: []mls.ExtensionType{
					mls.ExtensionTypeUrmessageGroupPolicy,
					mls.ExtensionTypeUrmessageLeafKeys,
					mls.ExtensionTypeUrmessageOwnerSuccessor,
				},
				Credentials: []mls.CredentialType{mls.CredentialTypeBasic},
			},
			LeafNodeSource: mls.LeafNodeSourceUpdate,
			Extensions:     []mls.Extension{leafKeysExtension},
		}
		if err := leaf.Sign(crypto, signaturePriv, groupId, mls.LeafIndex(i)); err != nil {
			t.Fatalf("leaf Sign(%d): %v", i, err)
		}
		if err := tree.SetLeaf(mls.LeafIndex(i), leaf); err != nil {
			t.Fatalf("SetLeaf(%d): %v", i, err)
		}
		members = append(members, externalMember{signaturePriv: signaturePriv, signaturePub: signaturePub})
	}
	return tree, members
}

// externalCrypto is the provider every test here runs under.
func externalCrypto(t *testing.T) mls.CryptoProvider {
	t.Helper()
	crypto, err := mls.NewCryptoProvider(mls.CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	return crypto
}

// externalGroupContextOver is the group context an attacker writes when nothing stops it: the group
// it named, the epoch it chose, and the tree hash of whatever tree it means the victim to use.
func externalGroupContextOver(t *testing.T, crypto mls.CryptoProvider,
	tree *mls.RatchetTree) *mls.GroupContext {
	t.Helper()
	treeHash, err := tree.TreeHash(crypto)
	if err != nil {
		t.Fatalf("TreeHash: %v", err)
	}
	return &mls.GroupContext{
		Version:                 mls.ProtocolVersionMls10,
		CipherSuite:             mls.CipherSuiteX25519ChaCha20Sha256Ed25519,
		GroupId:                 []byte(attackersGroupId),
		Epoch:                   attackersEpoch,
		TreeHash:                treeHash,
		ConfirmedTranscriptHash: bytes.Repeat([]byte{0x99}, crypto.HashSize()),
	}
}

// externalRoundTrip puts a GroupInfo on the wire and takes it off again, which is how a joiner
// actually comes by one.
//
// Not a convenience: a test that handed the constructor the very structure it had just built in
// memory would be checking a path no peer's message takes, and the decode is where a *GroupContext
// with an attacker's group id and epoch enters this process at all.
func externalRoundTrip(t *testing.T, info *mls.GroupInfo) *mls.GroupInfo {
	t.Helper()
	octets, err := syntax.Marshal(info)
	if err != nil {
		t.Fatalf("marshal the group info: %v", err)
	}
	decoded := &mls.GroupInfo{}
	if err := syntax.Unmarshal(octets, decoded); err != nil {
		t.Fatalf("unmarshal the group info: %v", err)
	}
	return decoded
}

// ---------------------------------------------------------------------------
// bypass A: a key schedule the caller built cannot vouch for the caller's context
// ---------------------------------------------------------------------------

// TestNoKeyScheduleAnswersAVerifiedGroupContext is bypass A closed as a CLASS.
//
// The three line laundering worked because the context went in through an exported KeySchedule
// constructor and came back out vouched for by a tag computed from the same exported type -- one
// party agreeing with itself. Naming the method that did it would let the next one through, so what
// is asked here is the question the bypass is an instance of: does any method of a key schedule
// answer a verified context at all, in any result position? A key schedule is derived from secrets
// its caller supplied over a context its caller supplied, so the answer must be no whatever the
// method is called.
func TestNoKeyScheduleAnswersAVerifiedGroupContext(t *testing.T) {
	crypto := externalCrypto(t)
	tree, _ := externalTree(t, crypto, []byte(attackersGroupId), 1)
	context := externalGroupContextOver(t, crypto, tree)
	schedule, err := mls.NewKeyScheduleFromEpochSecret(crypto,
		bytes.Repeat([]byte{0x4a}, crypto.HashSize()), context)
	if err != nil {
		t.Fatalf("the attacker's own key schedule over its own context: %v", err)
	}
	verified := reflect.TypeOf((*mls.VerifiedGroupContext)(nil))
	held := reflect.TypeOf(schedule)
	if held.NumMethod() == 0 {
		t.Fatal("*KeySchedule exposes no methods at all, so this gate asked nothing")
	}
	for i := range held.NumMethod() {
		method := held.Method(i)
		for at := range method.Type.NumOut() {
			if method.Type.Out(at) != verified {
				continue
			}
			t.Errorf("(*KeySchedule).%s answers a %s at result %d; a key schedule is expanded over a group context ITS CALLER SUPPLIED from a secret ITS CALLER SUPPLIED, so anything it vouches for is the caller vouching for itself -- which is the three line laundering this whole line of work exists to stop",
				method.Name, verified, at)
		}
	}
}

// TestTheZeroVerifiedGroupContextIsTheOnlyOneAnOutsiderCanSpell is what makes the door enumeration
// below the whole story.
//
// mls.VerifiedGroupContext{} compiles anywhere. What stops it being an authority is that the field
// is unexported -- so the literal carries nothing -- and that every consumer refuses the value that
// results. Both halves are asserted here, from outside, because a field that became exported would
// make every gate in this file watch a door that no longer matters.
func TestTheZeroVerifiedGroupContextIsTheOnlyOneAnOutsiderCanSpell(t *testing.T) {
	held := reflect.TypeOf(mls.VerifiedGroupContext{})
	for i := range held.NumField() {
		if held.Field(i).IsExported() {
			t.Fatalf("VerifiedGroupContext.%s is exported, so any package can build one carrying whatever it decoded and the type says nothing at all",
				held.Field(i).Name)
		}
	}
	zero := &mls.VerifiedGroupContext{}
	if got := zero.Context(); got != nil {
		t.Errorf("the zero verified context answered %+v, want nil", got)
	}
	if _, err := mls.NewProposalCache(zero); !errors.Is(err, mls.ErrNilGroupContext) {
		t.Errorf("NewProposalCache(the zero verified context) = %v, want ErrNilGroupContext", err)
	}
}

// TestADecodedGroupInfoIsNotVouchedForByTheTreeTheJoinerIsAlreadyIn is bypass A driven end to end
// with the attacker in the position it is actually in.
//
// The attacker writes a GroupInfo naming its own group at epoch 1<<40, computes a confirmation tag
// its own key schedule agrees with, and puts it on the wire. The victim decodes it and asks the one
// door there is, holding the tree of the group it is really in. The refusal is the tree hash rule,
// which is the FIRST of Verify's four and the one that means "this is not about my group at all".
func TestADecodedGroupInfoIsNotVouchedForByTheTreeTheJoinerIsAlreadyIn(t *testing.T) {
	crypto := externalCrypto(t)
	attackersTree, attackers := externalTree(t, crypto, []byte(attackersGroupId), 1)
	victimsTree, _ := externalTree(t, crypto, []byte("the group the victim is in"), 4)

	context := externalGroupContextOver(t, crypto, attackersTree)
	schedule, err := mls.NewKeyScheduleFromEpochSecret(crypto,
		bytes.Repeat([]byte{0x4a}, crypto.HashSize()), context)
	if err != nil {
		t.Fatalf("the attacker's own key schedule: %v", err)
	}
	info := &mls.GroupInfo{
		GroupContext:    *context,
		ConfirmationTag: schedule.ConfirmationTag(context.ConfirmedTranscriptHash),
		Signer:          mls.LeafIndex(0),
	}
	if err := info.Sign(crypto, attackers[0].signaturePriv); err != nil {
		t.Fatalf("the attacker signing its own group info: %v", err)
	}
	decoded := externalRoundTrip(t, info)
	if !bytes.Equal(decoded.GroupContext.GroupId, []byte(attackersGroupId)) ||
		decoded.GroupContext.Epoch != attackersEpoch {
		t.Fatalf("the decoded group info names epoch %d of group %x, so this test is not driving the attack it describes",
			decoded.GroupContext.Epoch, decoded.GroupContext.GroupId)
	}

	verified, err := decoded.VerifiedContext(crypto, victimsTree)
	if verified != nil {
		t.Errorf("a group info naming epoch %d of %s was vouched for against the tree of a group the attacker is not in",
			attackersEpoch, attackersGroupId)
	}
	if !errors.Is(err, mls.ErrWelcomeTreeHashMismatch) {
		t.Errorf("the attacker's group info answered %v against the victim's own tree, want ErrWelcomeTreeHashMismatch",
			err)
	}
}

// ---------------------------------------------------------------------------
// bypass B: still open, and open in exactly one place
// ---------------------------------------------------------------------------

// TestBypassBIsClosedOnlyAgainstATreeTheJoinerAlreadyTrusts is the honest joiner flow run over an
// attacker-chosen joiner_secret, and it is written to FAIL if anybody makes this file's account of
// it stop being true in either direction.
//
// The attacker's position is real and not weakened for the test: a Welcome is HPKE-sealed to a
// PUBLISHED init key, so the party that seals it chooses the joiner_secret and the group context
// both, and the schedule it derives agrees with the confirmation tag it publishes. The first
// assertion measures exactly that -- the premise the deleted constructor rested on still holds, so
// what changed is the door and not the attacker's reach.
//
// Then the two readings of the new door, and the second is the residual gap stated as a fact:
//
//  1. Against the tree of the group the joiner is really in, the forgery is refused.
//  2. Against the tree the ATTACKER supplied, it VERIFIES, and a proposal cache binds to epoch
//     1<<40 of ATTACKER-CHOSEN-GROUP. Verify establishes that a member of the tree the caller
//     passed signed this -- and when the caller passed the attacker's tree, the attacker is that
//     member. A joiner has done this to itself whenever the tree it passed came out of the same
//     message as the group info -- lifted from the ratchet_tree extension, or handed over as a
//     parameter beside it, which is p7 task 16's shape and is the same octets from the same
//     sender.
//
// It asserts the SUCCESS in case 2 rather than papering over it. A test that only asserted case 1
// would let this file read as though B were closed, which is the thing the constructor must not be
// shipped reading as.
func TestBypassBIsClosedOnlyAgainstATreeTheJoinerAlreadyTrusts(t *testing.T) {
	crypto := externalCrypto(t)
	attackersTree, attackers := externalTree(t, crypto, []byte(attackersGroupId), 1)
	victimsTree, _ := externalTree(t, crypto, []byte("the group the victim is in"), 4)
	context := externalGroupContextOver(t, crypto, attackersTree)

	// the joiner secret is the ATTACKER'S, because the Welcome carrying it was sealed to a
	// published init key: whoever holds the victim's key package supplies it
	joinerSecret := bytes.Repeat([]byte{0x11}, crypto.HashSize())
	pskSecret := bytes.Repeat([]byte{0x22}, crypto.HashSize())
	attackersSchedule, err := mls.NewKeyScheduleFromJoiner(crypto, joinerSecret, pskSecret, context)
	if err != nil {
		t.Fatalf("the attacker's joiner schedule: %v", err)
	}
	tag := attackersSchedule.ConfirmationTag(context.ConfirmedTranscriptHash)

	// the premise, measured: the victim deriving the SAME schedule from the secret it was sent
	// finds the tag perfectly good. Nothing about a confirmation tag was ever authority
	victimsSchedule, err := mls.NewKeyScheduleFromJoiner(crypto, joinerSecret, pskSecret, context)
	if err != nil {
		t.Fatalf("the victim's joiner schedule over the same secret: %v", err)
	}
	if !victimsSchedule.VerifyConfirmationTag(context.ConfirmedTranscriptHash, tag) {
		t.Fatal("the joiner flow's own confirmation tag did not verify, so this test is not driving bypass B at all -- the whole point is that it does")
	}

	info := &mls.GroupInfo{
		GroupContext:    *context,
		ConfirmationTag: tag,
		Signer:          mls.LeafIndex(0),
	}
	if err := info.Sign(crypto, attackers[0].signaturePriv); err != nil {
		t.Fatalf("the attacker signing the group info it is sending: %v", err)
	}
	decoded := externalRoundTrip(t, info)

	// 1. the half that IS closed
	if _, err := decoded.VerifiedContext(crypto, victimsTree); !errors.Is(err,
		mls.ErrWelcomeTreeHashMismatch) {
		t.Errorf("the attacker's group info against the joiner's own tree = %v, want ErrWelcomeTreeHashMismatch; a joiner that already knows its group's tree is the case this door does hold",
			err)
	}

	// 2. the half that is NOT, asserted as the fact it is
	verified, err := decoded.VerifiedContext(crypto, attackersTree)
	if err != nil {
		t.Fatalf("the attacker's group info against the attacker's OWN tree = %v; this file's account of the residual gap says it verifies, and an account that has stopped being true is worse than no account -- if a later change closed this, rewrite the header rather than deleting the case",
			err)
	}
	cache, err := mls.NewProposalCache(verified)
	if err != nil {
		t.Fatalf("NewProposalCache over the attacker's verified context: %v", err)
	}
	if cache == nil {
		t.Fatal("no cache was built, so the reading below observes nothing")
	}
	answered := verified.Context()
	if !bytes.Equal(answered.GroupId, []byte(attackersGroupId)) || answered.Epoch != attackersEpoch {
		t.Errorf("the verified context names epoch %d of group %x, want epoch %d of %s; the point of this case is that the attacker's OWN choices came through, so a different answer means the case has drifted off the attack it documents",
			answered.Epoch, answered.GroupId, attackersEpoch, attackersGroupId)
	}
}

// ---------------------------------------------------------------------------
// every exported door onto the type, derived from the compiler
// ---------------------------------------------------------------------------

// externalDoorsOntoAVerifiedGroupContext is every exported declaration of package mls whose
// signature ANSWERS a verified group context, read off the type checker.
//
// The class and not a list of names, which is rule 5 at the only boundary that matters here: what
// stops bypass A coming back is not that ConfirmGroupContext was deleted, it is that there is one
// door and its parameters carry authority. A second door added anywhere in the package -- a method
// of any exported type, a package level function, a package level var holding a function -- lands
// in this class and fails the table until somebody says what it is.
//
// The three shapes are enumerated rather than one, because an exported symbol answering a value can
// be any of them and a rule reading only funcs would report a clean run over the other two.
func externalDoorsOntoAVerifiedGroupContext(t *testing.T) (map[string]string, int) {
	t.Helper()
	found, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list the source of package mls: %v", err)
	}
	paths := []string{}
	for _, path := range found {
		if !strings.HasSuffix(path, "_test.go") {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		t.Fatal("package mls holds no non test source, so this gate scanned nothing")
	}
	fileSet := token.NewFileSet()
	files := []*ast.File{}
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		file, err := parser.ParseFile(fileSet, path, body, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		files = append(files, file)
	}
	refused := []string{}
	config := types.Config{
		Importer: importer.ForCompiler(fileSet, "source", nil),
		Error:    func(err error) { refused = append(refused, err.Error()) },
	}
	checked, err := config.Check("github.com/urnetwork/connect/mls", fileSet, files, nil)
	if err != nil || len(refused) != 0 {
		t.Fatalf("type check package mls from outside it: %v %s", err, strings.Join(refused, "; "))
	}

	doors := map[string]string{}
	judged := 0
	answersOne := func(signature *types.Signature) bool {
		results := signature.Results()
		if results == nil {
			return false
		}
		for at := 0; at < results.Len(); at += 1 {
			if externalNamedAs(results.At(at).Type(), "VerifiedGroupContext") {
				return true
			}
		}
		return false
	}
	scope := checked.Scope()
	for _, name := range scope.Names() {
		object := scope.Lookup(name)
		if !object.Exported() {
			continue
		}
		switch held := object.(type) {
		case *types.Func:
			signature, isSignature := held.Type().(*types.Signature)
			if !isSignature {
				continue
			}
			judged += 1
			if answersOne(signature) {
				doors[name] = signature.String()
			}
		case *types.Var:
			signature, isSignature := held.Type().(*types.Signature)
			if !isSignature {
				continue
			}
			judged += 1
			if answersOne(signature) {
				doors["var "+name] = signature.String()
			}
		case *types.TypeName:
			named, isNamed := held.Type().(*types.Named)
			if !isNamed {
				continue
			}
			for i := 0; i < named.NumMethods(); i += 1 {
				method := named.Method(i)
				if !method.Exported() {
					continue
				}
				signature, isSignature := method.Type().(*types.Signature)
				if !isSignature {
					continue
				}
				judged += 1
				if !answersOne(signature) {
					continue
				}
				receiver := name
				if _, isPointer := signature.Recv().Type().(*types.Pointer); isPointer {
					receiver = "(*" + name + ")"
				}
				doors[receiver+"."+method.Name()] = signature.String()
			}
		}
	}
	return doors, judged
}

// externalNamedAs answers whether the compiler reads a type as the named type spelled name, through
// any number of pointers. The package's own gates keep the same helper for the same reason: a value
// reached through a pointer and one reached directly are the same type to a caller.
func externalNamedAs(found types.Type, name string) bool {
	for {
		pointer, isPointer := found.(*types.Pointer)
		if !isPointer {
			break
		}
		found = pointer.Elem()
	}
	named, isNamed := found.(*types.Named)
	return isNamed && named.Obj() != nil && named.Obj().Name() == name
}

// The exported doors onto a verified group context, with why each is entitled to be one.
var externalVerifiedGroupContextDoors = map[string]string{
	"(*GroupInfo).VerifiedContext": "the one door. It is (*GroupInfo).Verify: a member of the ratchet " +
		"tree the CALLER passed signed this GroupInfoTBS under the GroupInfoTBS label, and the signer's " +
		"public key came out of that tree rather than out of the message -- which is why the tree is in " +
		"the signature and why a door without one would be a caller vouching for itself again",
}

// TestTheOnlyExportedDoorOntoAVerifiedGroupContextIsAVerifiedGroupInfo holds the class above to its
// table in both directions, and then holds the one door to carrying the parameter that makes it a
// door at all.
//
// The parameter assertion is not ceremony. Four rounds of this work produced constructors whose
// signatures carried the VALUE and not the AUTHORITY -- a *GroupContext, then a *KeySchedule the
// caller had just built -- and each read as a check while checking the caller's own say-so. A tree
// is the only thing that can say who a group's members are, so a door that stopped taking one would
// be that mistake for the fifth time, and it fails here.
func TestTheOnlyExportedDoorOntoAVerifiedGroupContextIsAVerifiedGroupInfo(t *testing.T) {
	doors, judged := externalDoorsOntoAVerifiedGroupContext(t)
	if judged == 0 {
		t.Fatal("no exported signature of package mls was resolved, so this gate would report a clean run over any package at all")
	}
	classified := slices.Sorted(maps.Keys(externalVerifiedGroupContextDoors))
	if names := slices.Sorted(maps.Keys(doors)); !slices.Equal(names, classified) {
		t.Fatalf("%v answer a *VerifiedGroupContext to an outside caller and this table names %v; a door with no row is a new way for a peer's octets to acquire this client's vouching, and a row with no door is a claim that outlived what it described",
			names, classified)
	}
	for _, name := range classified {
		if externalVerifiedGroupContextDoors[name] == "" {
			t.Errorf("%s is classified with no account of what entitles it", name)
		}
		t.Logf("%s is %s", name, doors[name])
	}

	door, held := reflect.TypeOf(&mls.GroupInfo{}).MethodByName("VerifiedContext")
	if !held {
		t.Fatal("(*GroupInfo).VerifiedContext is not a method of *GroupInfo, so the table above names a door that is not there")
	}
	tree := reflect.TypeOf((*mls.RatchetTree)(nil))
	takesATree := false
	for at := range door.Type.NumIn() {
		if door.Type.In(at) == tree {
			takesATree = true
		}
	}
	if !takesATree {
		t.Errorf("(*GroupInfo).VerifiedContext takes no %s; the signer's public key is not a field of a GroupInfo, so without a tree there is nothing in the call that can say who the members are and the door is the caller vouching for its own octets",
			tree)
	}
}

// ---------------------------------------------------------------------------
// the external boundary, compiled rather than described
// ---------------------------------------------------------------------------

// The synthetic package the gate below compiles and the file its diagnostics are reported against.
// Both appear in the failure text, so they are named for what they are.
const (
	externalForgingPackage = "outside"
	externalForgingFile    = "outside.go"
	// the struct declared OUT THERE with this type's shape, and the variable the field-write
	// spelling writes through. Both are named once because the compiler's DIAGNOSTICS name
	// them, and the words each refusal is held to below are taken from these constants rather
	// than typed a second time beside the source that spells them.
	externalForgingShadow = "shadow"
	externalForgingForged = "forged"
	// the package under test, as the synthetic source imports it and as the importer resolves
	// it. One spelling, so the source this gate compiles and the type it compares that source's
	// shadow against cannot become two different packages.
	externalMlsImportPath = "github.com/urnetwork/connect/mls"
	// the words the compiler opens a refused CONVERSION with, spelled once because the gate reads
	// them two ways: they are among the words each conversion spelling is held to, and they are
	// how a row says of itself that its refusal is the conversion one.
	externalConversionDiagnostic = "cannot convert"
)

// externalSpelling is one way of writing an mls.VerifiedGroupContext in a package that imports mls,
// as source that package could actually be written with, together with what the compiler must
// answer about it.
type externalSpelling struct {
	// what this spelling is called in the failure text.
	name string
	// the body, appended to a prologue every spelling shares. Sharing the prologue is what makes
	// the control a control: the file that compiles and the files that do not differ in the
	// construction and in nothing else, so a diagnostic can only be about the construction.
	body string
	// whether the compiler must refuse this file. Exactly one spelling is written to compile.
	refused bool
	// substrings every diagnostic must carry. A file that failed to compile for an unrelated
	// reason -- a moved exported symbol, a typo in the generator below -- reads to a gate that
	// counts only "was there an error" exactly like the refusal this gate exists to observe. And
	// the two conversions do NOT fail for the reason the two field spellings fail, so one shared
	// expectation would let either refusal stand in for the other, which is the overclaim the
	// paragraph in group_context_verified.go was corrected for. That they really are separate is
	// DERIVED rather than claimed: the gate below reads every refused spelling's diagnostics
	// against every OTHER refused spelling's words and requires them to fall short.
	mentions []string
}

// convertsFromTheShadow reports whether this spelling's body converts the shadow struct into this
// type, which is what makes its refusal the CROSS-PACKAGE STRUCT IDENTITY one -- and what obliges
// the gate to show that the shadow really has this type's shape. A shadow that had stopped having
// it is refused too, on the ordinary conversion rule, with a diagnostic that reads the same way.
//
// DERIVED off the body this gate compiles, and it used to be a bool set by hand on two rows. That
// bool was a rule 5 shape inside a rule 5 gate and it was measured: clearing it on both rows left
// the whole suite green, and clearing it TOGETHER with one field appended to the generated shadow
// restored the exact defect the shape assertion exists to catch -- both conversions still refused,
// every word they are held to still present, the derived count of refusals still what the field
// list makes it, and nothing anywhere failing. Read off the body, a row that stopped converting
// stops claiming to, and a row that converts cannot stop being asked.
func (self externalSpelling) convertsFromTheShadow() bool {
	return strings.Contains(self.body, externalForgingShadow+"{")
}

// isHeldToAConversionRefusal is the SECOND reading of the same row: not what it compiles but what
// its diagnostics are required to say.
//
// The gate requires the two to agree. They are derived from different halves of the row, so a row
// that stopped being a conversion in one half -- a body rewritten, or the conversion words dropped
// from what it is held to -- fails there rather than quietly taking the shape assertion with it.
func (self externalSpelling) isHeldToAConversionRefusal() bool {
	return slices.Contains(self.mentions, externalConversionDiagnostic)
}

// externalForgingSuite is the whole synthetic corpus, derived once.
type externalForgingSuite struct {
	// the type name as the compiler carries it, for the failure text.
	typeName string
	// the declarations every spelling is compiled on top of.
	prologue string
	// how many unexported fields the type holds, so the caller can say how many refusals it
	// should have seen without writing the number down.
	unexported int
	spellings  []externalSpelling
}

// externalForgingSpellings derives, from the compiled type, every spelling of a forged verified
// group context this gate compiles.
//
// FROM THE TYPE AND NOT FROM A LIST. What the compiler refuses is a reference to a field of THIS
// struct, so the two field spellings are generated per field: a field renamed, added, or made
// exported changes what is compiled here rather than leaving the gate watching a name that has
// moved. The shadow struct the two conversions convert FROM is rendered off the same field list for
// the same reason -- a hand written shadow stops having "the same shape" the day a field is added,
// and a conversion refused because the shapes differ looks exactly like a conversion refused for
// the reason this gate is about.
//
// The field names come from the type linked into this test binary and the refusals come from the
// source on disk, which are two readings of one package; a build where those had drifted apart
// fails here rather than reporting on the older of the two.
func externalForgingSpellings(t *testing.T) externalForgingSuite {
	t.Helper()
	held := reflect.TypeOf(mls.VerifiedGroupContext{})
	if held.Kind() != reflect.Struct {
		t.Fatalf("mls.%s is a %s rather than a struct, so there is no field for any of these spellings to name",
			held.Name(), held.Kind())
	}
	if held.NumField() == 0 {
		t.Fatalf("mls.%s holds no field at all, so no spelling an outsider writes could carry a group context in and this gate would compile files about nothing",
			held.Name())
	}
	declarations := []string{}
	elements := []string{}
	unexported := []reflect.StructField{}
	stolenOf := map[string]string{}
	shadow := []string{"type " + externalForgingShadow + " struct {"}
	for at := range held.NumField() {
		field := held.Field(at)
		stolen := fmt.Sprintf("stolen%d", at)
		stolenOf[field.Name] = stolen
		declarations = append(declarations, fmt.Sprintf(
			"// octets an attacker decoded, for field %s. What it holds is immaterial: the\n"+
				"// refusals below are decided at the reference to the field, before anything is\n"+
				"// assigned to it.\nvar %s %s\n", field.Name, stolen, field.Type))
		elements = append(elements, fmt.Sprintf("%s: %s", field.Name, stolen))
		shadow = append(shadow, fmt.Sprintf("\t%s %s", field.Name, field.Type))
		if !field.IsExported() {
			unexported = append(unexported, field)
		}
	}
	if len(unexported) == 0 {
		t.Fatalf("every field of mls.%s is exported, so any package that imports mls can build one carrying whatever it decoded and the type establishes nothing whatever",
			held.Name())
	}
	shadow = append(shadow, "}")
	shadowLiteral := externalForgingShadow + "{" + strings.Join(elements, ", ") + "}"

	prologue := fmt.Sprintf(`package %s

import %q

%s
// a struct declared OUT HERE with this type's exact shape. Its unexported field is package %s's
// and not package mls's -- an unexported field name carries the package that declared it -- which
// is the whole of why the conversions are refused, and it is a DIFFERENT refusal from the two that
// name the field directly. That the shape really is this type's is not left to this comment:
// externalShadowHasThisTypesShape asks go/types, in the package this gate type checks it to.
%s

// building one is ordinary go out here and is not the thing under test; the control compiles this
// line too. It says NOTHING about the shape above -- a KEYED literal naming some of the fields
// compiles over any superset of them -- which is why the shape is asserted separately.
var _ = %s

`, externalForgingPackage, externalMlsImportPath, strings.Join(declarations, "\n"),
		externalForgingPackage, strings.Join(shadow, "\n"), shadowLiteral)

	// the control first, so a reader of the failure text meets the file that must compile before
	// the ones that must not.
	spellings := []externalSpelling{{
		name: "the legitimate spellings, which must compile",
		body: fmt.Sprintf(`func spelling(info *mls.GroupInfo, crypto mls.CryptoProvider,
	tree *mls.RatchetTree) (*mls.GroupContext, error) {
	// the zero value, which any package may spell and which every door refuses
	zero := mls.%s{}
	if zero.Context() != nil {
		return nil, nil
	}
	// the one exported door there is
	verified, err := info.VerifiedContext(crypto, tree)
	if err != nil {
		return nil, err
	}
	return verified.Context(), nil
}
`, held.Name()),
		refused: false,
	}}
	for _, field := range unexported {
		spellings = append(spellings, externalSpelling{
			name: fmt.Sprintf("a composite literal naming %s", field.Name),
			body: fmt.Sprintf(`func spelling() mls.%s {
	return mls.%s{%s: %s}
}
`, held.Name(), held.Name(), field.Name, stolenOf[field.Name]),
			refused: true,
			// "struct literal" and the type's own name are what the WRITE's diagnostic does
			// not carry -- that one is "forged.inner undefined (cannot refer to unexported
			// field inner)" and names neither -- so these are the words that separate the two
			// refusals that both begin "cannot refer to unexported field".
			mentions: []string{"unexported field", field.Name, "struct literal", "mls." + held.Name()},
		}, externalSpelling{
			name: fmt.Sprintf("a write of %s", field.Name),
			body: fmt.Sprintf(`func spelling() mls.%s {
	%s := mls.%s{}
	%s.%s = %s
	return %s
}
`, held.Name(), externalForgingForged, held.Name(), externalForgingForged, field.Name,
				stolenOf[field.Name], externalForgingForged),
			refused: true,
			// the SELECTOR, which is what makes this one this one: the composite literal names
			// the field on its own and never through a variable, so "forged.inner" is a word
			// only this refusal carries. It is spelled off the same constant the body above
			// writes, so a variable renamed renames both rather than leaving them disagreeing.
			mentions: []string{"unexported field", externalForgingForged + "." + field.Name},
		})
	}
	spellings = append(spellings, externalSpelling{
		name: "a conversion from an identically shaped struct",
		body: fmt.Sprintf(`func spelling() mls.%s {
	return mls.%s(%s)
}
`, held.Name(), held.Name(), shadowLiteral),
		refused: true,
		// the DESTINATION is what separates the two conversions. This one converts to the VALUE
		// type and the next to the POINTER type, so the two diagnostics end "to type mls.X" and
		// "to type *mls.X", and neither of those texts appears in the other's.
		mentions: []string{externalConversionDiagnostic, externalForgingShadow,
			"to type mls." + held.Name()},
	}, externalSpelling{
		name: "a conversion from a pointer to an identically shaped struct",
		body: fmt.Sprintf(`func spelling() *mls.%s {
	return (*mls.%s)(&%s)
}
`, held.Name(), held.Name(), shadowLiteral),
		refused: true,
		mentions: []string{externalConversionDiagnostic, "*" + externalForgingShadow,
			"to type *mls." + held.Name()},
	})
	return externalForgingSuite{
		typeName:   held.Name(),
		prologue:   prologue,
		unexported: len(unexported),
		spellings:  spellings,
	}
}

// externalDiagnosticsMissing answers, for one spelling's diagnostics and the words its refusal
// must carry, every (diagnostic, word) pair the diagnostic does not carry -- as sentences a
// failure can print, and empty when every word is in every diagnostic.
//
// ONE PREDICATE, TWO READINGS. The gate below asks it twice: once of a spelling against its OWN
// words, which is the refusal being observed, and once of a spelling against every OTHER
// spelling's words, which is the separation between refusals being observed. Those two have to
// mean the same thing by "satisfies", so there is one function and not two loops that drifted.
func externalDiagnosticsMissing(diagnostics []string, mentions []string) []string {
	missing := []string{}
	for _, one := range diagnostics {
		for _, want := range mentions {
			if !strings.Contains(one, want) {
				missing = append(missing, fmt.Sprintf("%q does not mention %q", one, want))
			}
		}
	}
	return missing
}

// externalShadowHasThisTypesShape holds the struct the conversions convert FROM to this type's
// exact shape, asked of go/types in the very package whose refusal is being read.
//
// WHY THE ARM CANNOT DO WITHOUT IT. The two conversions are named for the CROSS-PACKAGE STRUCT
// IDENTITY rule -- a struct declared out there with a field spelled inner is not this struct
// type, because an unexported field name carries the package that declared it. A shadow that had
// stopped having this type's shape is refused too, on the ORDINARY conversion rule, and the
// diagnostic reads the same way -- word for word the same, on this toolchain. Measured: one extra
// field appended to the generated shadow left both conversion arms refused, every word they are
// held to still present, the derived count of refusals still what the field list makes it, and
// this gate green. The arm was named for a refusal it could not tell from a different one.
//
// THE POSITIVE CONTROL DOES NOT COVER THIS. Its `var _ = shadow{inner: stolen0}` is a KEYED
// PARTIAL literal, which compiles over any superset of the fields it names, so a shadow that had
// grown a field compiles there exactly as it did before.
//
// WHAT IS ASSERTED is the shape field for field -- name, type, tag and embedding, in order --
// and then the two questions that make the refusal the identity one rather than a shape one:
// go/types must read the two structs as NOT identical, and the conversion in both its value and
// its pointer form as NOT legal. Field NAMES are compared and the package an unexported name
// carries is not, which is the entire difference between the two structs and the entire reason
// the conversions are refused.
func externalShadowHasThisTypesShape(t *testing.T, spelling string, outside *types.Package,
	shared types.Importer, typeName string, source string) {
	t.Helper()
	if outside == nil {
		t.Errorf("%q type checked to no package at all, so the shadow it converts from could not be read and this arm cannot tell the identity refusal from a shape one.\n%s",
			spelling, source)
		return
	}
	shadowObject := outside.Scope().Lookup(externalForgingShadow)
	if shadowObject == nil {
		t.Errorf("%q declares no %s in package %s, so the conversion this arm reports on converts from something this gate never saw.\n%s",
			spelling, externalForgingShadow, externalForgingPackage, source)
		return
	}
	shadowNamed, isNamed := shadowObject.Type().(*types.Named)
	if !isNamed {
		t.Errorf("%q's %s is a %s rather than a defined type", spelling, externalForgingShadow,
			shadowObject.Type())
		return
	}
	held, err := shared.Import(externalMlsImportPath)
	if err != nil {
		t.Fatalf("import %s to compare the shadow against: %v", externalMlsImportPath, err)
	}
	realObject := held.Scope().Lookup(typeName)
	if realObject == nil {
		t.Fatalf("package %s exports no %s, so this gate compiles files about a type that has moved",
			externalMlsImportPath, typeName)
	}
	realNamed, isNamed := realObject.Type().(*types.Named)
	if !isNamed {
		t.Fatalf("%s.%s is a %s rather than a defined type", externalMlsImportPath, typeName,
			realObject.Type())
	}
	shadowStruct, isStruct := shadowNamed.Underlying().(*types.Struct)
	if !isStruct {
		t.Errorf("%q's %s is a %s rather than a struct", spelling, externalForgingShadow,
			shadowNamed.Underlying())
		return
	}
	realStruct, isStruct := realNamed.Underlying().(*types.Struct)
	if !isStruct {
		t.Fatalf("mls.%s is a %s rather than a struct", typeName, realNamed.Underlying())
	}
	if shadowStruct.NumFields() != realStruct.NumFields() {
		t.Errorf("%q converts from a %s holding %d fields and mls.%s holds %d, so what the compiler refused is a conversion between two SHAPES and not the cross-package identity refusal this arm is named for. Both read the same way in the diagnostic, which is why the shape is asserted here.\n%s",
			spelling, externalForgingShadow, shadowStruct.NumFields(), typeName,
			realStruct.NumFields(), source)
		return
	}
	for at := range shadowStruct.NumFields() {
		mine, theirs := shadowStruct.Field(at), realStruct.Field(at)
		if mine.Name() != theirs.Name() || mine.Embedded() != theirs.Embedded() ||
			shadowStruct.Tag(at) != realStruct.Tag(at) ||
			!types.Identical(mine.Type(), theirs.Type()) {
			t.Errorf("%q converts from a %s whose field %d is %s %s %q and mls.%s's is %s %s %q; a conversion refused because the shapes differ is not the refusal this arm is named for.\n%s",
				spelling, externalForgingShadow, at, mine.Name(), mine.Type(),
				shadowStruct.Tag(at), typeName, theirs.Name(), theirs.Type(),
				realStruct.Tag(at), source)
		}
	}
	// the shape being equal field for field and the two types NOT being identical is the whole of
	// the rule this arm is named for: what separates them is the package an unexported field name
	// carries, and nothing else about them differs.
	if types.Identical(shadowStruct, realStruct) {
		t.Errorf("%q converts from a struct go/types reads as IDENTICAL to mls.%s, so the conversion is legal and whatever was refused above was refused for some other reason entirely.\n%s",
			spelling, typeName, source)
	}
	if types.ConvertibleTo(shadowNamed, realNamed) {
		t.Errorf("go/types reads %s as convertible to mls.%s, so an outsider may convert one into the other and the field being unexported establishes nothing.\n%s",
			externalForgingShadow, typeName, source)
	}
	if types.ConvertibleTo(types.NewPointer(shadowNamed), types.NewPointer(realNamed)) {
		t.Errorf("go/types reads *%s as convertible to *mls.%s, so an outsider may convert one into the other through a pointer.\n%s",
			externalForgingShadow, typeName, source)
	}
}

// TestEachForgedSpellingThisGateCompilesIsRefusedForItsOwnReason is the external boundary
// ASSERTED rather than described.
//
// THE NAME SAYS WHAT IS OBSERVED, which the previous one did not. It was
// TestEveryExternalSpellingOfAForgedVerifiedGroupContextIsRefusedByTheCompiler, and the paragraph
// under it said the opposite in capitals -- "IT DOES NOT ENUMERATE THE SPELLINGS and must not be
// read as doing so". A green run shows a reader the NAME and not the doc, so the name is now the
// weaker and true claim: the spellings THIS GATE COMPILES, each refused for a reason that no
// other one of them satisfies.
//
// It exists because a paragraph written to stop this package overclaiming claimed LESS than the
// code delivers: it said a compile error was not a thing a test in this build could observe, and
// gave that as the reason the property the entire guarantee rests on had no gate of its own.
// go/types is in the standard library, this package already type checks source through it in
// four other gates, and the whole of this one costs about two and a half seconds.
//
// WHAT IT IS NOT is a matcher over this package's AST. The language already decides whether an
// outsider may name an unexported field, and a walk asking the same question of syntax would be
// re-deriving the compiler's answer badly. What this gate is for is the day that answer CHANGES
// FOR THE SPELLINGS IT COMPILES: the field RENAMED, which renames the spellings with it; an
// UNEXPORTED field ADDED, which gets a pair of spellings of its own and raises the count that must
// come back refused; and the LAST unexported field EXPORTED, which the generator refuses outright,
// because a type all of whose fields are exported is one any importer can build in one line. An
// EXPORTED field added beside the unexported one is NOT one of these -- see the list below.
//
// AND ITS REACH STOPS AT WHAT AN OUTSIDER MAY WRITE. THE NEIGHBOURING CASES ARE LISTED BECAUSE
// THEY WERE MEASURED, and because the paragraph they replace named exactly one of them while
// claiming a second inside this gate's reach:
//
//   - AN EXPORTED FIELD ADDED BESIDE the unexported one. Every spelling here is still refused --
//     the shadow is generated off the field list, so it grows the same field and the conversions
//     go on failing on identity -- and this gate passes. What fails is
//     TestTheZeroVerifiedGroupContextIsTheOnlyOneAnOutsiderCanSpell above, which refuses any
//     exported field at all: the stronger rule, and the older one. The two are not redundant --
//     that one reads the TYPE, this one reads what a package importing it may WRITE.
//   - A CONSTRUCTOR ADDED THAT HANDS THE INNER POINTER OUT. An earlier version of this paragraph
//     gave this as something this gate notices, and both readings of it leave this gate GREEN.
//     func NewVerifiedGroupContext(context *GroupContext) *VerifiedGroupContext appended to
//     group_context_verified.go: this gate passes, and what fails is
//     TestTheOnlyExportedDoorOntoAVerifiedGroupContextIsAVerifiedGroupInfo above,
//     TestEveryConstructionOfAVerifiedGroupContextIsClassifiedHere and
//     TestEveryConstructorOverAGroupContextRefusesANilOne.
//   - AN ACCESSOR THAT HANDS THE STORAGE OUT. (*VerifiedGroupContext).Context changed from
//     self.inner.Clone() to self.inner: this gate passes, and what fails is
//     TestNoMethodOfAVerifiedGroupContextHandsOutTheStorageItVouchesFor and
//     TestAVerifiedContextIsNotChangeableThroughWhatItHandsBack.
//   - UNSAFE. Nothing here reaches it and nothing here should; group_context_verified.go's header
//     measures what it does and says why it is out of scope rather than unnoticed.
//
// None of the middle two is a spelling an OUTSIDER writes, and that is the whole of why this gate
// cannot see them: every file it compiles is package outside, and a door or an accessor added to
// package mls changes what that package may CALL rather than what it may WRITE.
//
// THE POSITIVE CONTROL IS THE LOAD BEARING PART. Files that fail to compile are also what a gate
// whose import path had gone stale, whose synthetic source held a typo, or whose importer could
// not resolve this package at all would produce, and every one of those reads as a set of clean
// refusals. So the control shares the whole prologue with the rest, differs from them only in the
// construction, and MUST compile: it exercises the one exported door and the zero value, so a
// build where either had moved fails here rather than passing quietly. What it does NOT establish
// is the shadow's shape -- see externalShadowHasThisTypesShape.
//
// EACH REFUSAL IS HELD TO WORDS NO OTHER ONE OF THEM CARRIES, AND THE SEPARATION IS DERIVED. The
// two field spellings fail on the field reference and the two conversions on cross-package struct
// identity, which is different text about a different rule; a gate that asked only "was there an
// error" would let any one of the four stand in for any other. Holding each to its own words is
// not enough on its own either, and that was the defect this pass closed: both field spellings
// were held to {"unexported field", "inner"}, which BOTH diagnostics carry, so the composite
// literal's arm would have accepted the write's refusal and the write's arm the literal's. So the
// last thing this gate does is ask each spelling's diagnostics against every OTHER spelling's
// words and require them to fall short. That is the separation measured rather than claimed, and
// it is what entitles this test's name to "for its own reason".
//
// THE TWO CONVERSIONS CARRY ONE MORE OBLIGATION, because their rule is about the shadow they
// convert from and not only about this type: externalShadowHasThisTypesShape, above, holds that
// shadow to this type's exact shape in the package the refusal came out of.
func TestEachForgedSpellingThisGateCompilesIsRefusedForItsOwnReason(t *testing.T) {
	suite := externalForgingSpellings(t)
	fileSet := token.NewFileSet()
	// one importer for every check: it carries a cache, so package mls and everything it imports
	// are read off disk once rather than once per spelling. Measured, that is about two seconds
	// for the first check and about a tenth of a second for each of the rest.
	shared := importer.ForCompiler(fileSet, "source", nil)
	compiled, refused, shapeAsserted := 0, 0, 0
	// what each refused spelling was actually told, kept so the separation below is read off the
	// same diagnostics the assertions above were read off
	diagnosticsOf := map[string][]string{}
	for _, spelling := range suite.spellings {
		source := suite.prologue + spelling.body
		file, err := parser.ParseFile(fileSet, externalForgingFile, source, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%q did not parse, so this gate compiled nothing about it: %v\n%s",
				spelling.name, err, source)
		}
		diagnostics := []string{}
		config := types.Config{
			Importer: shared,
			Error:    func(err error) { diagnostics = append(diagnostics, err.Error()) },
		}
		outside, checkErr := config.Check(externalForgingPackage, fileSet, []*ast.File{file}, nil)
		// the two readings of the row, from its body and from its words, held against each other
		// before either is acted on
		if spelling.convertsFromTheShadow() != spelling.isHeldToAConversionRefusal() {
			t.Errorf("%q converts from %s in its body (%v) and is held to a %q diagnostic (%v); the two readings of what this row's refusal is about disagree, and the shape assertion follows the first of them",
				spelling.name, externalForgingShadow, spelling.convertsFromTheShadow(),
				externalConversionDiagnostic, spelling.isHeldToAConversionRefusal())
		}
		if spelling.convertsFromTheShadow() {
			externalShadowHasThisTypesShape(t, spelling.name, outside, shared, suite.typeName, source)
			shapeAsserted += 1
		}
		if !spelling.refused {
			if checkErr != nil || len(diagnostics) != 0 {
				t.Fatalf("%q is this gate's positive control and must compile from outside mls; it answered %v %s. Every refusal this gate reports would read the same way if the harness had stopped resolving package mls at all, so a control that does not compile makes the rest of the run mean nothing.\n%s",
					spelling.name, checkErr, strings.Join(diagnostics, "; "), source)
			}
			compiled += 1
			continue
		}
		if len(diagnostics) == 0 {
			t.Errorf("%q COMPILED from outside mls. A package importing this one can now build an mls.%s carrying whatever it decoded off the wire, so the type says nothing about anybody's authority and every door that takes one is back to trusting its caller's octets.\n%s",
				spelling.name, suite.typeName, source)
			continue
		}
		if len(spelling.mentions) == 0 {
			t.Errorf("%q is written to be refused and names no words its refusal must carry, so any diagnostic at all satisfies it and the separation below holds vacuously.\n%s",
				spelling.name, source)
			continue
		}
		if missing := externalDiagnosticsMissing(diagnostics, spelling.mentions); len(missing) != 0 {
			for _, one := range missing {
				t.Errorf("%q was refused, and %s. A file refused for some other reason counts as a refusal to a gate that only asks whether the compiler complained, and this one is here to observe THIS refusal.\n%s",
					spelling.name, one, source)
			}
			continue
		}
		diagnosticsOf[spelling.name] = diagnostics
		refused += 1
		for _, one := range diagnostics {
			t.Logf("%s: %s", spelling.name, one)
		}
	}
	if compiled != 1 {
		t.Errorf("%d of this gate's %d files were written to compile, want exactly 1; without one that compiles a harness that resolves nothing reports a clean run",
			compiled, len(suite.spellings))
	}
	// the floor under the derivation above. Every refusal here is a compile error, and a gate that
	// stopped recognising its conversion rows would go on reporting four clean refusals while the
	// one rule that is about the SHADOW rather than about a field name was observed over nothing.
	if shapeAsserted == 0 {
		t.Errorf("no spelling this gate compiled was read as converting from %s, so the cross-package struct identity rule was asserted over nothing and a shadow that had stopped having mls.%s's shape would be refused, counted and reported clean",
			externalForgingShadow, suite.typeName)
	}
	// the count is derived rather than written down: two spellings per unexported field, plus the
	// two conversions, which is what the generator above builds off the type's own field list.
	if want := 2*suite.unexported + 2; refused != want {
		t.Errorf("%d spellings were refused with the diagnostic this gate expects and mls.%s holds %d unexported fields, which is %d spellings; a field with no refusal of its own is a field nothing here watched",
			refused, suite.typeName, suite.unexported, want)
	}
	// and the separation, DERIVED off the diagnostics rather than asserted in prose. Two refusals
	// that satisfy each other's words are one expectation written twice, and either could stand
	// in for the other without this gate noticing -- which is what both field spellings did until
	// this pass.
	for _, one := range suite.spellings {
		mine := diagnosticsOf[one.name]
		if len(mine) == 0 {
			continue
		}
		for _, other := range suite.spellings {
			if !other.refused || other.name == one.name || len(other.mentions) == 0 {
				continue
			}
			if len(externalDiagnosticsMissing(mine, other.mentions)) != 0 {
				continue
			}
			t.Errorf("%q was refused with %v, and every one of those diagnostics carries all of %v, which is what %q is held to. Those two refusals are about DIFFERENT rules and each would satisfy the other's expectation, so either could stand in for the other and this gate would report a clean run over a spelling it never observed.",
				one.name, mine, other.mentions, other.name)
		}
	}
}
