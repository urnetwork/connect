// The erase obligation, stated over the CLASS of types this package holds key material inside
// rather than over the two calls that were missing from it.
//
// What was missing, and what it cost. (*Group).Close zeroized self.schedule and self.secretTree
// and never touched self.pending, and (*Group).ClearPendingCommit was literally self.pending =
// nil. A StagedCommit is a complete second epoch -- its own key schedule, holding the init
// secret, the confirmation key, the encryption secret, the epoch authenticator, the exporter and
// the resumption PSK; its own secret tree, holding every message key derived from that encryption
// secret; and the leaf private key the committer drew for the epoch its commit would open -- and
// nothing in this package called Zeroize on any of it. Measured before the repair: snapshot the
// staged epoch's init secret, its confirmation key and its leaf private key, call Close, and all
// three read back byte for byte.
//
// AND CLEARING ONE IS THE ORDINARY PATH. ClearPendingCommit exists for MASTER section 9.3's
// lost-commit race: the delivery service accepts at most one commit per (group, epoch), so a
// client whose commit lost that race drops a fully derived epoch nobody ever entered. That is not
// an edge case, it is what happens whenever two members commit at once.
//
// So the fix is not two calls. Every type here that holds key material owes an erase, and every
// path that drops such a value has to take it, and both halves are read off the source below:
//
//   - the CLASS is derived. Its seed is the types that declare an erase -- read off the erase
//     helper class this package already derives, so a type joins by erasing rather than by being
//     listed -- and it is closed upward over holders: a type with a field whose type names a
//     member is a member, which is exactly how a StagedCommit holding a *KeySchedule and
//     declaring no erase becomes a failure rather than an omission.
//   - the FIELDS are derived: every field of a member whose type reaches byte storage, with the
//     ones that are not key material carrying a row that says why, checked in both directions so
//     a field cannot fall out of the class by being forgotten and a row cannot outlive its field.
//   - and the DROP SITES are derived: every assignment a member's own method makes to one of
//     those fields, which must erase the value it drops, install it, or refuse to overwrite a
//     live one.
//
// What this cannot see, said out loud. The assignment reading is rooted in the positions a
// CALLER fills -- the receiver and the parameters -- so a body that constructs a member itself
// and writes over a field of it is outside this, deliberately: nothing was handed to it and
// nothing of anybody else's is dropped. A value handed OUT of this package and dropped by its
// new holder is that holder's obligation and not this one's, which is what PathDecryptResult's
// row says. And the erase of a SUB-field of a value being moved out is read by the callee's bare
// name, so a namesake of an erase would certify one -- the install half beside it is resolved
// exactly, and it is the half that says a merge moved rather than dropped.
//
// Two further holes in the same reading are measured, and each is written down beside the code
// that has it rather than only here: a method call on the receiver is resolved by NAME AND ARITY
// and not by the receiver's type, and the widened position reaches one hop rather than a closure.
// Both are entries in mls/UNOBSERVED.md, with the mutation that survives each.
package mls

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// ---------------------------------------------------------------------------
// what a staged epoch is holding, and what happens to it
// ---------------------------------------------------------------------------

// stagedEpochStorage is every secret the staged epoch is holding, as the slices the epoch itself
// is using rather than copies of them.
//
// The slices and not their contents, because that is the only reading that can observe an erase
// at all: zeroizeSecret writes through the backing array, so a test holding a copy would see the
// copy afterwards and report a clean erase over a schedule that never ran one.
//
// Every entry is checked to be non-zero before it is answered, which is the live control. "All
// zero afterwards" is satisfied by storage that was already zero, and a fixture that had stopped
// deriving an epoch would pass every assertion below while observing nothing.
func stagedEpochStorage(t *testing.T, staged *StagedCommit) map[string][]byte {
	t.Helper()
	if staged == nil {
		t.Fatal("no commit is staged, so the erase below is over nothing")
	}
	held := map[string][]byte{
		"schedule.init_secret":         staged.schedule.Secrets().InitSecret,
		"schedule.confirmation_key":    staged.schedule.Secrets().Confirmation,
		"schedule.encryption_secret":   staged.schedule.Secrets().Encryption,
		"schedule.epoch_authenticator": staged.schedule.Secrets().EpochAuthenticator,
		"schedule.resumption_psk":      staged.schedule.Secrets().ResumptionPsk,
		"ownPriv.encryption_priv":      staged.ownPriv.EncryptionPriv,
	}
	for x, secret := range staged.secretTree.nodes {
		held[fmt.Sprintf("secretTree.node[%d]", x)] = secret
	}
	for x, secret := range staged.ownPriv.PathSecrets {
		held[fmt.Sprintf("ownPriv.path_secret[%d]", x)] = secret
	}
	if staged.plan != nil {
		held["plan.commit_secret"] = staged.plan.CommitSecret
		for i, secret := range staged.plan.PathSecrets {
			held[fmt.Sprintf("plan.path_secret[%d]", i)] = secret
		}
	}
	for name, secret := range held {
		if len(secret) == 0 {
			t.Fatalf("%s is empty in the staged epoch, so an all zero reading afterwards would say nothing", name)
		}
		if !slices.ContainsFunc(secret, func(b byte) bool { return b != 0 }) {
			t.Fatalf("%s is already all zero before the erase, so an all zero reading afterwards would say nothing", name)
		}
	}
	return held
}

// requireErased fails on the first byte of any entry that survived.
func requireErased(t *testing.T, at string, held map[string][]byte) {
	t.Helper()
	for _, name := range slices.Sorted(maps.Keys(held)) {
		for i, b := range held[name] {
			if b != 0 {
				t.Errorf("%s: byte %d of %s is %#02x, want 0; the staged epoch was dropped with its key material still in it",
					at, i, name, b)
				break
			}
		}
	}
}

// TestClosingAGroupErasesTheEpochItStagedAndNeverEntered is the first half of the measurement
// this file exists for, run the other way round.
func TestClosingAGroupErasesTheEpochItStagedAndNeverEntered(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")
	if _, err := group.CreateCommit(nil, nil, nil); err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	staged := group.stagedForTest()
	held := stagedEpochStorage(t, staged)

	if err := group.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	requireErased(t, "Close", held)
	// and the staged secret tree refuses rather than answering out of zeros, which is the half
	// (*SecretTree).Zeroize's own comment calls load bearing.
	if !staged.secretTree.erased {
		t.Error("the staged secret tree is not marked erased, so every method of it still answers -- out of Nh zero bytes, with no error, which is a key every party in the world can compute")
	}
	if !staged.ownPriv.erased {
		t.Error("the staged private tree state is not marked erased, so its leaf arm still answers a key")
	}
}

// TestClearPendingCommitErasesTheStagedEpoch is the same erase on the path it is actually taken
// on: not a close, but the lost-commit race of MASTER section 9.3.
func TestClearPendingCommitErasesTheStagedEpoch(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")
	defer group.Close()

	if _, err := group.CreateCommit(nil, nil, nil); err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	staged := group.stagedForTest()
	held := stagedEpochStorage(t, staged)

	group.ClearPendingCommit()
	requireErased(t, "ClearPendingCommit", held)
	if group.stagedForTest() != nil {
		t.Fatal("ClearPendingCommit left a commit staged")
	}
	// and the group is still usable in the epoch it never left, which is the whole point of the
	// method: a clear that took the live epoch with it would turn a lost race into a dead client.
	if len(group.EpochAuthenticator()) == 0 {
		t.Error("the group answers no epoch authenticator after clearing a staged commit")
	}
	if _, err := group.CreateCommit(nil, nil, nil); err != nil {
		t.Errorf("CreateCommit after ClearPendingCommit: %v; the group cannot build another commit against the epoch it is still in", err)
	}
}

// TestClearingAnAddOnlyCommitLeavesTheLiveEpochsLeafKeyIntact is the control on the erase above,
// and it is the input that separates the two programs.
//
// An add-only commit needs no update path (RFC 9420 section 12.4), so the staged epoch carries
// this client's leaf private state FORWARD rather than replacing it. Before CreateCommit cloned
// it, the staged commit and the live group held ONE *TreeKEMPrivate -- so an erase of the staged
// epoch on this path erased the key the group is still decrypting update paths with, on the
// ordinary lost-commit race, and every commit this client received afterwards failed to open
// with a message about a ciphertext that was perfectly well formed.
func TestClearingAnAddOnlyCommitLeavesTheLiveEpochsLeafKeyIntact(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")
	defer group.Close()

	joiner := testIdentity(t, crypto, "bob")
	kp, _, _ := testKeyPackage(t, crypto, joiner)
	encoded, err := syntax.Marshal(kp)
	if err != nil {
		t.Fatalf("marshal the joiner's key package: %v", err)
	}
	if _, err := group.ProposeAdd(encoded); err != nil {
		t.Fatalf("ProposeAdd: %v", err)
	}
	if _, err := group.CreateCommit(nil, nil, nil); err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	staged := group.stagedForTest()
	if staged.plan != nil {
		t.Fatal("the add-only commit carries an update path, so it is not the shape this control is about")
	}
	if staged.ownPriv == group.ownPriv {
		t.Fatal("the staged commit and the live group hold the same *TreeKEMPrivate; erasing the staged epoch erases the epoch the group is still in")
	}
	live := append([]byte(nil), group.ownPriv.EncryptionPriv...)
	if !slices.ContainsFunc(live, func(b byte) bool { return b != 0 }) {
		t.Fatal("the live leaf private key is already all zero, so this control observes nothing")
	}

	group.ClearPendingCommit()

	if !slices.Equal(group.ownPriv.EncryptionPriv, live) {
		t.Errorf("clearing the staged commit changed the LIVE epoch's leaf private key; the staged epoch was sharing storage with the group it was staged against")
	}
	if group.ownPriv.erased {
		t.Error("clearing the staged commit marked the live epoch's private tree state erased")
	}
	if _, _, err := group.ownPriv.NodePrivateKey(crypto, group.ownLeaf.NodeIndex()); err != nil {
		t.Errorf("the live epoch's own leaf answers %v after a staged commit was cleared", err)
	}
}

// TestMergePendingCommitErasesTheEpochItClosed is the third drop site, and the one nothing was
// asking about: a merge REPLACES the schedule, the secret tree and the leaf private state, and
// there is no past-epoch window in this build to hand the outgoing ones to.
func TestMergePendingCommitErasesTheEpochItClosed(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "group-1")
	defer group.Close()

	closing := map[string][]byte{
		"schedule.init_secret":       group.schedule.Secrets().InitSecret,
		"schedule.confirmation_key":  group.schedule.Secrets().Confirmation,
		"schedule.encryption_secret": group.schedule.Secrets().Encryption,
		"ownPriv.encryption_priv":    append([]byte(nil), group.ownPriv.EncryptionPriv...),
	}
	closingPriv := group.ownPriv
	for name, secret := range closing {
		if !slices.ContainsFunc(secret, func(b byte) bool { return b != 0 }) {
			t.Fatalf("%s is already all zero before the merge, so an all zero reading afterwards would say nothing", name)
		}
	}
	// read the live slice back for the leaf key: the copy above is what the erase must NOT reach,
	// and the slice itself is what it must.
	closing["ownPriv.encryption_priv"] = closingPriv.EncryptionPriv

	if _, err := group.CreateCommit(nil, nil, nil); err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	staged := group.stagedForTest()
	plan := staged.plan
	if plan == nil {
		t.Fatal("the commit carries no update path, so the plan this merge has to erase does not exist")
	}
	planSecret := plan.CommitSecret
	if err := group.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}

	requireErased(t, "MergePendingCommit", closing)
	for i, b := range planSecret {
		if b != 0 {
			t.Errorf("byte %d of the update path plan's commit secret is %#02x after the merge, want 0; the plan is the one piece of the staged commit no field of the group receives",
				i, b)
			break
		}
	}
	// and the epoch the merge ENTERED is intact, which is what says the erase reached the right
	// side of the swap. A merge that erased what it installed would pass every assertion above.
	if len(group.EpochAuthenticator()) == 0 {
		t.Fatal("the group answers no epoch authenticator after a merge")
	}
	if group.ownPriv.erased {
		t.Fatal("the merge erased the private tree state it installed")
	}
	if _, _, err := group.ownPriv.NodePrivateKey(crypto, group.ownLeaf.NodeIndex()); err != nil {
		t.Errorf("the merged epoch's own leaf answers %v", err)
	}
	if _, err := group.CreateCommit(nil, nil, nil); err != nil {
		t.Errorf("CreateCommit in the merged epoch: %v", err)
	}
}

// TestAnErasedPrivateTreeStateRefusesRatherThanAnsweringZeros holds the half of
// (*TreeKEMPrivate).Zeroize that is not the write.
//
// (*SecretTree).Zeroize states the rule this follows: erasing and refusing are one operation,
// because a state that answered out of erased storage would hand its caller a zero-length leaf
// private key with a nil error, and every failure downstream of that names a message rather than
// the key that opened it.
func TestAnErasedPrivateTreeStateRefusesRatherThanAnsweringZeros(t *testing.T) {
	crypto := testCrypto(t)
	priv, pub, err := crypto.DeriveKeyPair(crypto.Random(crypto.HashSize()))
	if err != nil {
		t.Fatalf("DeriveKeyPair: %v", err)
	}
	_ = pub
	state := NewTreeKEMPrivate(0, priv)
	state.PathSecrets[NodeIndex(1)] = crypto.Random(crypto.HashSize())
	leafKey := state.EncryptionPriv
	pathSecret := state.PathSecrets[NodeIndex(1)]
	if !slices.ContainsFunc(leafKey, func(b byte) bool { return b != 0 }) {
		t.Fatal("the leaf private key is already all zero, so this reads nothing")
	}

	state.Zeroize()

	for i, b := range leafKey {
		if b != 0 {
			t.Errorf("byte %d of the leaf private key is %#02x after Zeroize, want 0", i, b)
			break
		}
	}
	for i, b := range pathSecret {
		if b != 0 {
			t.Errorf("byte %d of the path secret is %#02x after Zeroize, want 0", i, b)
			break
		}
	}
	if len(state.PathSecrets) != 0 {
		t.Errorf("the erased state still reports %d path secret(s); a map of zeroed entries says this member can derive a key for every node of its direct path",
			len(state.PathSecrets))
	}
	if _, _, err := state.NodePrivateKey(crypto, LeafIndex(0).NodeIndex()); !errors.Is(err, errTreeKEMPrivateErased) {
		t.Errorf("an erased state answers %v for its own leaf, want errTreeKEMPrivateErased; without the refusal it hands back a zero length private key with no error",
			err)
	}
	// a second erase is a no-op rather than a fault, for (*KeySchedule).Zeroize's reason: a value
	// may be dropped by one path and released by another.
	state.Zeroize()
	// and a clone of an erased state is erased, or the copy answers what the original refuses.
	if !state.Clone().erased {
		t.Error("a clone of an erased private tree state is not erased, so the copy answers out of storage the original refuses to answer out of")
	}
}

// ---------------------------------------------------------------------------
// the class: read off the source, because two calls are not the property
// ---------------------------------------------------------------------------

// eraseSourceReading is one parse of the production source the erase obligation is stated over,
// with everything derived from it that both gates below need.
type eraseSourceReading struct {
	files   []parsedSource
	structs map[string]*ast.StructType
	// the named byte slice types, so storage held under HpkePrivateKey is the same storage as
	// storage held under []byte, which is what the compiler thinks too.
	named []string
	// the types this source PUTS ON THE WIRE, which is what separates octets a peer already has
	// from octets only this process holds.
	wire map[string]bool
	// and, per wire type, WHICH OF ITS FIELDS the encoding actually writes. The type-level
	// reading is not enough on its own: a wire type may hold storage no encoder ever reaches,
	// and KeyPackage.signPriv -- the seed the leaf's signing key was minted from -- is exactly
	// that.
	encoded map[string]map[string]bool
	// this package's erase helpers, derived by eraseHelperClass and not named here.
	helpers []string
	// per type, the method names of it that erase storage the type itself declares.
	erasers map[string]map[string]bool
	// the class, and the types the closure reaches that owe no erase.
	members []string
}

// eraseReadingOf parses the production half of the roots the guardrails walk.
//
// BOTH ROOTS, for the seam gate's reason: a type declared in connect/message holding one of this
// package's erasable values is a holder, and scoping the class to the directory its first members
// happen to sit in is the defect this project has paid for more than once.
func eraseReadingOf(t *testing.T) eraseSourceReading {
	t.Helper()
	reading := eraseSourceReading{structs: map[string]*ast.StructType{}}
	scan := mustScanSources(t, forbiddenScanRoots)
	candidates := []eraseHelperCandidate{}
	paths := slices.Sorted(maps.Keys(productionSources(scan.sourceTexts)))
	for _, path := range paths {
		parsed := mustParseSource(t, path)
		reading.files = append(reading.files, parsed)
		structTypesIn(parsed, reading.structs)
		reading.named = append(reading.named, packageByteSliceTypeNamesIn(parsed)...)
	}
	slices.Sort(reading.named)
	reading.named = slices.Compact(reading.named)
	for _, path := range paths {
		candidates = append(candidates,
			eraseHelperCandidatesIn(mustReadCommented(t, path), path, reading.named)...)
	}
	reading.wire = theTypesThisSourceSerializes(reading.files, reading.structs)
	reading.encoded = theFieldsEachWireTypeEncodes(reading.files, reading.structs, reading.wire)
	reading.helpers, _ = eraseHelperClass(candidates)
	if len(reading.helpers) == 0 {
		t.Fatal("this source declares no erase helper, so every reading below finds no erase however an erase is written")
	}
	reading.erasers = eraseMethodsIn(reading)
	reading.members = eraseClassOf(reading)
	return reading
}

// The codec methods a wire type of this source declares. ANCHORS, and they fail in the safe
// direction rather than being asserted: see theTypesThisSourceSerializes.
const (
	eraseWireEncoder = "MarshalMLS"
	eraseWireDecoder = "UnmarshalMLS"
)

// theTypesThisSourceSerializes is every type this source PUTS ON THE WIRE: one declaring a codec
// of its own, and every type one of those carries in a field, to a fixed point.
//
// It is what tells KEY MATERIAL from octets a peer already has, and it is the reason the class
// below can be seeded on storage at all. A type this source serializes publishes everything it
// holds -- that is what its codec does -- so the closure runs DOWNWARD here, the opposite
// direction from the holder closure, and for the opposite reason: a holder inherits its held
// value's obligation, and a carried field inherits its carrier's publicity.
//
// THE ANCHOR FAILS SAFE, which is what makes it safe to anchor. A rename that emptied this set
// certifies nothing: every field in the source becomes key material, the class grows from forty
// members to eighty-four, and the table below fails in both directions at once.
func theTypesThisSourceSerializes(files []parsedSource,
	structs map[string]*ast.StructType) map[string]bool {

	wire := map[string]bool{}
	for _, parsed := range files {
		for _, declaration := range parsed.file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Recv == nil || len(function.Recv.List) != 1 {
				continue
			}
			if function.Name.Name != eraseWireEncoder && function.Name.Name != eraseWireDecoder {
				continue
			}
			wire[receiverTypeName(function.Recv.List[0].Type)] = true
		}
	}
	for grew := true; grew; {
		grew = false
		for typeName := range wire {
			structure, isStruct := structs[typeName]
			if !isStruct {
				continue
			}
			for _, field := range structure.Fields.List {
				for _, mentioned := range identifiersNamedIn(field.Type) {
					if _, isStruct := structs[mentioned]; isStruct && !wire[mentioned] {
						wire[mentioned] = true
						grew = true
					}
				}
			}
		}
	}
	return wire
}

// eraseWireWriter is the encoder every writer in this source is handed.
//
// A DECLARATION TAKING ONE IS PART OF THE ENCODING, and that is the property the reading below is
// rooted in rather than a method name. KeyPackage's marshalCore takes a Writer and writes five of
// the six fields MarshalMLS publishes; group_policy.go writes a RoleEntry through a free function
// that takes one by value and is a method of nothing. A reading rooted in MarshalMLS would call
// five published key package fields this process's own storage, and would have no name at all for
// the role entry.
const eraseWireWriter = "Writer"

// theFieldsEachWireTypeEncodes is, for every type this source puts on the wire, WHICH OF ITS
// FIELDS the encoding writes.
//
// It is the half theTypesThisSourceSerializes cannot supply, and the gap it closes was measured:
// the class seed excused a wire type WHOLESALE, on the argument that everything such a type holds
// is about to be octets. That argument is true of every field the encoding writes and of no other,
// and KeyPackage.signPriv is the counterexample the package already documents -- the seed the
// leaf's signature key was minted from, which marshalCore stops above and UnmarshalMLS clears. A
// signature private key sat outside the erase class entirely for as long as the reading was taken
// at the type.
//
// EVERY DECLARATION HANDED A WRITER IS READ, and the values it writes out of are the ones its own
// SIGNATURE names -- its receiver and its parameters. That is what reaches the types with no codec
// of their own: Proposal.MarshalMLS spells out self.ReInit.GroupId and self.ExternalInit.KemOutput
// and self.Add.KeyPackage, one selector deeper than its own fields, and each of those is a field
// of a type this source serializes and never encodes for itself.
//
// AND WHAT SUCH A DECLARATION ASKS OF THE VALUE IT IS ENCODING IS PART OF THE ENCODING TOO, which
// is the other hop the reading has to make: (*GroupInfo).MarshalMLS writes nothing of its own but
// the signature -- the other four fields go through self.toBeSigned(), which reads them into a
// preimage view that is then encoded. A reading that stopped at the encoder's own body would call
// a group context, an extension list and a confirmation tag this process's private storage.
//
// A LOCAL IS NOT A POSITION, which is the same boundary the drop reading draws and it is drawn for
// the weaker reason here: this reading credits a field as PUBLISHED, so every name it resolves
// wrongly is an erase obligation dropped. Only names whose type the signature states are followed,
// and the hop above is a call on one of those rather than on anything a body constructed.
func theFieldsEachWireTypeEncodes(files []parsedSource, structs map[string]*ast.StructType,
	wire map[string]bool) map[string]map[string]bool {

	// per type, its fields, and for each the struct type this source declares that the field's
	// own type mentions -- which is where a selector one level deeper lands.
	fieldsOf := map[string]map[string]string{}
	for typeName, structure := range structs {
		fieldsOf[typeName] = map[string]string{}
		for _, field := range structure.Fields.List {
			held := ""
			for _, mentioned := range identifiersNamedIn(field.Type) {
				if _, isStruct := structs[mentioned]; isStruct {
					held = mentioned
				}
			}
			for _, name := range eraseFieldNamesOf([]*ast.Field{field}) {
				fieldsOf[typeName][name] = held
			}
		}
	}
	// the positions a declaration is handed, by the name the body spells each under, and whether
	// one of them is the encoder.
	positionsOf := func(function *ast.FuncDecl) (map[string]string, bool) {
		positions := []*ast.Field{}
		if function.Recv != nil {
			positions = append(positions, function.Recv.List...)
		}
		if function.Type.Params != nil {
			positions = append(positions, function.Type.Params.List...)
		}
		bound := map[string]string{}
		handedTheWriter := false
		for _, position := range positions {
			mentioned := identifiersNamedIn(position.Type)
			if slices.Contains(mentioned, eraseWireWriter) {
				handedTheWriter = true
			}
			for _, name := range position.Names {
				for _, one := range mentioned {
					if _, isStruct := structs[one]; isStruct {
						bound[name.Name] = one
					}
				}
			}
		}
		return bound, handedTheWriter
	}
	// the methods, so a call the encoder makes on the value it is encoding can be followed.
	type declaration struct {
		self string
		decl *ast.FuncDecl
	}
	methods := map[string]map[string]declaration{}
	for _, parsed := range files {
		for _, node := range parsed.file.Decls {
			function, isFunction := node.(*ast.FuncDecl)
			if !isFunction || function.Body == nil ||
				function.Recv == nil || len(function.Recv.List) != 1 {

				continue
			}
			names := function.Recv.List[0].Names
			if len(names) != 1 || names[0].Name == "_" {
				continue
			}
			owner := receiverTypeName(function.Recv.List[0].Type)
			if methods[owner] == nil {
				methods[owner] = map[string]declaration{}
			}
			methods[owner][function.Name.Name] = declaration{self: names[0].Name, decl: function}
		}
	}
	encoded := map[string]map[string]bool{}
	credit := func(typeName string, field string) {
		if !wire[typeName] {
			return
		}
		if _, isField := fieldsOf[typeName][field]; !isField {
			return
		}
		if encoded[typeName] == nil {
			encoded[typeName] = map[string]bool{}
		}
		encoded[typeName][field] = true
	}
	walked := map[string]bool{}
	var walk func(body *ast.BlockStmt, bound map[string]string)
	walk = func(body *ast.BlockStmt, bound map[string]string) {
		ast.Inspect(body, func(node ast.Node) bool {
			if call, isCall := node.(*ast.CallExpr); isCall {
				// what the encoder ASKS of the value it is encoding, followed into the method
				// that answers it. self.toBeSigned() is where a GroupInfo's four signed fields
				// are read, and nothing in MarshalMLS names them.
				if callee, isSelector := call.Fun.(*ast.SelectorExpr); isSelector {
					if base, isBare := callee.X.(*ast.Ident); isBare {
						if typeName, isBound := bound[base.Name]; isBound {
							held, isMethod := methods[typeName][callee.Sel.Name]
							if isMethod && !walked[typeName+"."+callee.Sel.Name] {
								walked[typeName+"."+callee.Sel.Name] = true
								inner, _ := positionsOf(held.decl)
								inner[held.self] = typeName
								walk(held.decl.Body, inner)
							}
						}
					}
				}
				return true
			}
			selector, isSelector := node.(*ast.SelectorExpr)
			if !isSelector {
				return true
			}
			switch base := selector.X.(type) {
			case *ast.Ident:
				if typeName, isBound := bound[base.Name]; isBound {
					credit(typeName, selector.Sel.Name)
				}
			case *ast.SelectorExpr:
				// one selector deeper, which is how a carrier writes a type that declares no
				// codec: self.ReInit.GroupId is Proposal's encoder writing a field of ReInit.
				outer, isBare := base.X.(*ast.Ident)
				if !isBare {
					return true
				}
				typeName, isBound := bound[outer.Name]
				if !isBound {
					return true
				}
				if held := fieldsOf[typeName][base.Sel.Name]; held != "" {
					credit(held, selector.Sel.Name)
				}
			}
			return true
		})
	}
	for _, parsed := range files {
		for _, node := range parsed.file.Decls {
			function, isFunction := node.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			bound, handedTheWriter := positionsOf(function)
			if !handedTheWriter {
				continue
			}
			walk(function.Body, bound)
		}
	}
	return encoded
}

// eraseFieldNamesOf is every field in a list by the name a body spells it under, embedded fields
// included -- an embedded field's name is its type's, which is how a body reaches it.
func eraseFieldNamesOf(list []*ast.Field) []string {
	names := []string{}
	for _, field := range list {
		if len(field.Names) == 0 {
			for _, mentioned := range identifiersNamedIn(field.Type) {
				names = append(names, mentioned)
				break
			}
			continue
		}
		for _, declared := range field.Names {
			names = append(names, declared.Name)
		}
	}
	return names
}

// eraseTypeReachesStorage answers whether a field of this type can hold octets: the type is the
// language's own byte slice, or one of this source's named byte slice types, or a struct declared
// here that reaches one through a field of its own.
//
// It is the reading that says which fields an erase has to account for. A LeafIndex cannot hold a
// secret and an interface is somebody else's storage; a *KeySchedule, a map[NodeIndex][]byte and
// a [][]byte all can.
func eraseTypeReachesStorage(structs map[string]*ast.StructType, named []string, expr ast.Expr,
	seen map[string]bool) bool {

	for _, mentioned := range identifiersNamedIn(expr) {
		if mentioned == "byte" || slices.Contains(named, mentioned) {
			return true
		}
		structure, isStruct := structs[mentioned]
		if !isStruct || seen[mentioned] {
			continue
		}
		seen[mentioned] = true
		for _, field := range structure.Fields.List {
			if eraseTypeReachesStorage(structs, named, field.Type, seen) {
				return true
			}
		}
	}
	return false
}

// eraseField is one field an erase has to account for: its name, and the types this source
// declares that its type mentions, which is where an erase of its own would be declared.
type eraseField struct {
	name  string
	types []string
	// whether the octets this field holds are ones this source puts on the wire, read off the
	// codec closure. A published field is not key material, and a type holding nothing else is
	// not in the class.
	published bool
}

// theFieldsReachingStorageOf is every field of one type that reaches octets, in declaration
// order. Embedded fields are read too -- an embedded struct's storage is this type's storage.
func theFieldsReachingStorageOf(reading eraseSourceReading, typeName string) []eraseField {
	structure, isDeclared := reading.structs[typeName]
	if !isDeclared {
		return nil
	}
	fields := []eraseField{}
	for _, field := range structure.Fields.List {
		if !eraseTypeReachesStorage(reading.structs, reading.named, field.Type, map[string]bool{}) {
			continue
		}
		held := []string{}
		published := false
		for _, mentioned := range identifiersNamedIn(field.Type) {
			if _, isStruct := reading.structs[mentioned]; isStruct {
				held = append(held, mentioned)
			}
			if reading.wire[mentioned] {
				published = true
			}
		}
		names := field.Names
		if len(names) == 0 {
			// an embedded field: its name is its type's, which is how a body spells it.
			for _, mentioned := range identifiersNamedIn(field.Type) {
				fields = append(fields,
					eraseField{name: mentioned, types: held, published: published})
				break
			}
			continue
		}
		for _, declared := range names {
			fields = append(fields,
				eraseField{name: declared.Name, types: held, published: published})
		}
	}
	return fields
}

// eraseFieldNameOf resolves an expression to the receiver's own field it reads: self.x, x[i],
// *x, self.x.y, and any local bound from one of those.
//
// The ROOT field is the answer, because that is the storage: self.secrets.SenderData is storage
// of the field secrets, and a body erasing it has accounted for that field.
func eraseFieldNameOf(expr ast.Expr, receiver string, alias map[string]string) string {
	switch typed := expr.(type) {
	case *ast.ParenExpr:
		return eraseFieldNameOf(typed.X, receiver, alias)
	case *ast.StarExpr:
		return eraseFieldNameOf(typed.X, receiver, alias)
	case *ast.IndexExpr:
		return eraseFieldNameOf(typed.X, receiver, alias)
	case *ast.SliceExpr:
		return eraseFieldNameOf(typed.X, receiver, alias)
	case *ast.SelectorExpr:
		if base, isBare := typed.X.(*ast.Ident); isBare && base.Name == receiver {
			return typed.Sel.Name
		}
		return eraseFieldNameOf(typed.X, receiver, alias)
	case *ast.Ident:
		return alias[typed.Name]
	}
	return ""
}

// eraseAliasesIn binds the local names of one body to the receiver's field they read, to a fixed
// point.
//
// Without it every erase written as a loop reads as erasing nothing: `for _, secret := range
// self.nodes { zeroizeSecret(secret) }` hands a helper a local, and the local is the field.
func eraseAliasesIn(function *ast.FuncDecl, receiver string) map[string]string {
	alias := map[string]string{}
	for grew := true; grew; {
		grew = false
		bind := func(target ast.Expr, source ast.Expr) {
			field := eraseFieldNameOf(source, receiver, alias)
			if field == "" {
				return
			}
			name, isBare := target.(*ast.Ident)
			if !isBare || name.Name == "_" || alias[name.Name] != "" {
				return
			}
			alias[name.Name] = field
			grew = true
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.AssignStmt:
				if len(typed.Lhs) == len(typed.Rhs) {
					for i := range typed.Rhs {
						bind(typed.Lhs[i], typed.Rhs[i])
					}
					return true
				}
				if len(typed.Rhs) == 1 && len(typed.Lhs) != 0 {
					bind(typed.Lhs[0], typed.Rhs[0])
				}
			case *ast.RangeStmt:
				// the VALUE of a range names the storage; the key is an index or a map key.
				if typed.Value != nil {
					bind(typed.Value, typed.X)
				}
			}
			return true
		})
	}
	return alias
}

// eraseMethod is one method of one type, as both readings below ask about it.
type eraseMethod struct {
	receiver string
	name     string
	// the receiver's own name in the body, which every field reading is rooted in.
	self   string
	parsed parsedSource
	decl   *ast.FuncDecl
}

// methodsWithNoParametersIn is every method of the source that takes nothing.
//
// AN ERASE TAKES NO ARGUMENTS, and that is a property rather than a convention: an erase is a
// total operation on storage the receiver already holds, so a method that has to be TOLD what to
// erase is erasing part of something rather than all of it. Without this line
// (*SecretTree).takeLeafSecret -- which hands one node's secret to a helper and answers it --
// certifies a caller that names it as having erased the whole tree.
func methodsWithNoParametersIn(files []parsedSource) []eraseMethod {
	methods := []eraseMethod{}
	for _, parsed := range files {
		for _, declaration := range parsed.file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			if function.Recv == nil || len(function.Recv.List) != 1 {
				continue
			}
			if function.Type.Params != nil && len(function.Type.Params.List) != 0 {
				continue
			}
			self := ""
			if names := function.Recv.List[0].Names; len(names) == 1 {
				self = names[0].Name
			}
			if self == "" || self == "_" {
				continue
			}
			methods = append(methods, eraseMethod{
				receiver: receiverTypeName(function.Recv.List[0].Type),
				name:     function.Name.Name,
				self:     self,
				parsed:   parsed,
				decl:     function,
			})
		}
	}
	return methods
}

// theFieldsErasedBy is which of a method's own fields it hands to an erase, and -- for a field
// erased through one of its PARTS rather than whole -- which part.
//
// THE PART IS READ AND NOT COLLAPSED ONTO THE ROOT, and what collapsing it cost was measured:
// self.pending.plan.Zeroize() erases the update path plan a staged commit holds and leaves that
// commit's key schedule, secret tree and leaf private state in the heap -- and it read as "self
// .pending was erased", because eraseFieldNameOf resolves the call's receiver to the ROOT field
// and the root's own type happened to declare a method of that name. The empty string is the
// field itself; eraseWholeFieldsOf below is what turns a set of parts back into a claim about
// the whole.
//
// Two shapes and both are this package's: storage handed to an erase HELPER as an argument
// (zeroizeSecret(self.epochSecret)), and an erase METHOD called on the field
// (self.schedule.Zeroize()). The second is the one a holder uses and the one the erase helper
// class cannot see, because the storage arrives as a receiver rather than as an argument.
//
// A MENTION IS NOT AN ERASE. The call is read and its argument or its receiver is resolved to a
// field, so a body that named every secret -- a length check over each, a log line -- accounts
// for none of them, which is precisely the shape "make Zeroize a no-op" takes once the calls are
// gone.
func theFieldsErasedBy(reading eraseSourceReading, method eraseMethod,
	fields []eraseField) map[string]map[string]bool {

	alias := eraseAliasesIn(method.decl, method.self)
	byName := map[string]eraseField{}
	for _, field := range fields {
		byName[field.name] = field
	}
	erased := map[string]map[string]bool{}
	credit := func(field string, part string) {
		if erased[field] == nil {
			erased[field] = map[string]bool{}
		}
		erased[field][part] = true
	}
	ast.Inspect(method.decl.Body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		switch callee := call.Fun.(type) {
		case *ast.Ident:
			if !slices.Contains(reading.helpers, callee.Name) {
				return true
			}
			for _, argument := range call.Args {
				if field := eraseFieldNameOf(argument, method.self, alias); field != "" {
					credit(field, eraseSubFieldOn(argument, method.self, alias, field))
				}
			}
		case *ast.SelectorExpr:
			// an erase run ON the field, or on one part of it. The method has to be an erase of
			// THE TYPE IT IS RUN ON -- the field's own when the call names the field, the part's
			// own when it names a part -- so a namesake declared by some other type certifies
			// nothing. Reading the ROOT's methods for a call one level in is what made an erase of
			// a sub-field stand for the whole held value.
			field := eraseFieldNameOf(callee.X, method.self, alias)
			if field == "" {
				return true
			}
			held, isField := byName[field]
			if !isField {
				return true
			}
			part := eraseSubFieldOn(callee.X, method.self, alias, field)
			holders := held.types
			if part != "" {
				holders = nil
				for _, typeName := range held.types {
					for _, sub := range theFieldsReachingStorageOf(reading, typeName) {
						if sub.name == part {
							holders = append(holders, sub.types...)
						}
					}
				}
			}
			for _, typeName := range holders {
				if reading.erasers[typeName][callee.Sel.Name] {
					credit(field, part)
				}
			}
			// and an erase helper reached through a package qualifier or through storage of the
			// field, which is how a helper of another package would be spelled.
			if slices.Contains(reading.helpers, callee.Sel.Name) {
				for _, argument := range call.Args {
					if name := eraseFieldNameOf(argument, method.self, alias); name != "" {
						credit(name, eraseSubFieldOn(argument, method.self, alias, name))
					}
				}
			}
		}
		return true
	})
	return erased
}

// eraseWholeFieldsOf is which fields an erase reading accounts for IN FULL.
//
// A field is accounted for when the erase was run on THE FIELD -- the empty part -- or when every
// part of it that reaches octets was erased in its own right. Anything else is a field one of
// whose parts was erased, and that is a different claim: (*KeySchedule).Zeroize names
// self.secrets.SenderData and eight more and has therefore erased the whole of secrets, while a
// body naming self.pending.plan alone has erased one of four things a staged commit holds.
//
// The parts that count are the parts that reach OCTETS, so a held type with one secret and four
// indices is accounted for by an erase of the secret.
func eraseWholeFieldsOf(reading eraseSourceReading, fields []eraseField,
	erased map[string]map[string]bool) map[string]bool {

	whole := map[string]bool{}
	for _, field := range fields {
		parts, wasErased := erased[field.name]
		if !wasErased {
			continue
		}
		if parts[""] {
			whole[field.name] = true
			continue
		}
		held := []string{}
		for _, typeName := range field.types {
			for _, sub := range theFieldsReachingStorageOf(reading, typeName) {
				held = append(held, sub.name)
			}
		}
		if len(held) == 0 {
			continue
		}
		covered := true
		for _, sub := range held {
			if !parts[sub] {
				covered = false
			}
		}
		if covered {
			whole[field.name] = true
		}
	}
	return whole
}

// eraseMethodsIn derives, per type, which of its no-argument methods erase storage it declares.
//
// A fixed point, because a holder's erase is an erase only once the erase it calls on its field
// is one: (*Group).Close erases through (*StagedCommit).Zeroize, which erases through
// (*KeySchedule).Zeroize, which erases through the helper this package derives.
func eraseMethodsIn(reading eraseSourceReading) map[string]map[string]bool {
	methods := methodsWithNoParametersIn(reading.files)
	erasers := map[string]map[string]bool{}
	// the fixed point is over a reading whose erasers map IS the one being built, so a holder's
	// erase becomes an erase on the pass after the erase it calls does.
	working := reading
	working.erasers = erasers
	for grew := true; grew; {
		grew = false
		for _, method := range methods {
			if erasers[method.receiver][method.name] {
				continue
			}
			fields := theFieldsReachingStorageOf(working, method.receiver)
			// ANY storage of its own handed to an erase, whole or in parts, which is what makes
			// a method an erase at all. Whether it accounts for a field IN FULL is a different
			// question and is asked where it belongs -- of the completeness gate, and of the
			// drop sites -- through eraseWholeFieldsOf.
			if len(theFieldsErasedBy(working, method, fields)) == 0 {
				continue
			}
			if erasers[method.receiver] == nil {
				erasers[method.receiver] = map[string]bool{}
			}
			erasers[method.receiver][method.name] = true
			grew = true
		}
	}
	return erasers
}

// eraseClassOf is the class: every type that HOLDS KEY MATERIAL -- octets this source does not put
// on the wire -- and every type holding one of those in a field of its own, to a fixed point.
//
// THE SEED IS THE PROPERTY AND NOT A DECLARATION, and the difference was measured. It was the
// types that DECLARE an erase, closed upward over holders, and the closure was doing its job
// while the seed was the gap: a production type holding an initSecret []byte and an
// HpkePrivateKey and declaring no Zeroize was not in the class at all, so nothing asked it for
// one and the whole suite stayed green. The same type holding a *KeySchedule WAS caught, which is
// what said the fault was the seed. It is also the shape task 19's past-epoch window has.
//
// Seeded on the storage, such a type is a member the day it is written, and it owes either an
// erase or a sentence somebody had to write in the table below.
//
// A WIRE TYPE IS NOT A SEED however much storage it holds, because everything it holds is about
// to be octets on the wire -- that is what its codec does. This is the one place the two
// questions are told apart at the TYPE rather than at the field: a raw []byte field of a wire
// type carries the encoding of something public, and no reading of the field's own type could
// see that.
func eraseClassOf(reading eraseSourceReading) []string {
	member := map[string]bool{}
	for typeName := range reading.structs {
		for _, field := range theFieldsReachingStorageOf(reading, typeName) {
			if eraseFieldIsPublished(reading, typeName, field.name, field.published) {
				continue
			}
			member[typeName] = true
		}
	}
	for grew := true; grew; {
		grew = false
		for typeName, structure := range reading.structs {
			if member[typeName] {
				continue
			}
			for _, field := range structure.Fields.List {
				// and the closure runs through the fields this type does NOT publish, asking the
				// same question the seed asks: a holder inherits its held value's obligation, and
				// a holder that PUBLISHES that value is putting those octets on the wire rather
				// than holding them. Without this, the day KeyPackage joins the class every Add,
				// every Proposal and every Commit joins it behind KeyPackage.
				published := false
				for _, name := range eraseFieldNamesOf([]*ast.Field{field}) {
					onTheWire := false
					for _, mentioned := range identifiersNamedIn(field.Type) {
						if reading.wire[mentioned] {
							onTheWire = true
						}
					}
					if eraseFieldIsPublished(reading, typeName, name, onTheWire) {
						published = true
					}
				}
				if published {
					continue
				}
				for _, mentioned := range identifiersNamedIn(field.Type) {
					if member[mentioned] {
						member[typeName] = true
						grew = true
					}
				}
			}
		}
	}
	return slices.Sorted(maps.Keys(member))
}

// eraseFieldIsPublished answers whether the octets one field holds are octets this source puts on
// the wire.
//
// TWO READINGS, AND THE TYPE DECIDES WHICH.
//
//  1. a type this source PUTS ON THE WIRE is published field by field, exactly where the encoding
//     writes it. This is the reading the whole change is for, and it is why no category is
//     excused wholesale: the seed used to step over a wire type entirely, and KeyPackage.signPriv
//     -- a signature private key that marshalCore stops above and UnmarshalMLS clears -- was
//     therefore outside the erase class. A wire type whose encoding writes none of some field is
//     a type holding storage of its own, whatever else it publishes;
//  2. a type this source does not serialize is published field by field by the field's own TYPE,
//     which is the reading that was here before and is the only one available where there is no
//     encoding to read.
func eraseFieldIsPublished(reading eraseSourceReading, typeName string, field string,
	fieldTypeIsOnTheWire bool) bool {

	if reading.wire[typeName] {
		return reading.encoded[typeName][field]
	}
	return fieldTypeIsOnTheWire
}

// typesTheEraseClassReachesThatOweNoErase is every member of the class above that owes no erase of
// its own, with the reason written out for each.
//
// The class is derived and this table is checked against it in both directions, so a type cannot
// escape the obligation by being forgotten and a row cannot outlive the type it excuses. What the
// rows say, over and over, is one of four things, and the sameness is the point rather than a
// smell: the octets are on the wire, the value is an ARGUMENT or an ANSWER that no declaration
// retains, the storage belongs to a holder that erases it part by part, or the field is not
// storage at all.
//
// IT GREW FROM ONE ROW TO THIRTY-THREE when the seed moved from "declares an erase" to "holds key
// material", and that is the price of the seed rather than a weakening of it. A row is a sentence
// somebody had to write about a type that holds octets; before, such a type simply was not asked.
var typesTheEraseClassReachesThatOweNoErase = map[string]string{
	"ApplicationMessage": "the plaintext and the authenticated data of one decrypted application message, " +
		"ANSWERED to the caller. It is Writer's row read the other way round: an erase here would blank the " +
		"message the caller asked this package to open, and the caller that opened a secret through it owns " +
		"what came back. The AAD travelled in the clear beside the ciphertext it bound. One production " +
		"declaration holds one in a field -- Processed.Application -- and that type is an answer nothing " +
		"retains either, excused directly below on the same terms",
	"BodyBinding": "the additional authenticated data of a record: a group id and a sender handle, both " +
		"of which travel in the clear beside the ciphertext they bind and are what the server routes on",
	"Processed": "an ANSWER and not storage, like PathDecryptResult. (*Group).ProcessMessage builds one per " +
		"message and hands it to the caller, and no production declaration holds one in a field, so there is " +
		"no drop site here an erase could be reachable from. The one arm of it that carries key material is " +
		"the staged epoch, which is a member of this class in its own right: (*Group).ApplyCommit MOVES that " +
		"epoch into the group's own pending -- erasing what it drops as it goes -- and a caller that never " +
		"applies the result owns it, exactly as PathDecryptResult's row says of the state it hands out",
	"CachedProposal": "a proposal this member received and the reference it is keyed by. Both went to every " +
		"member of the group and to the delivery service; ProposalCache.byRef is excused for the same reason",
	"epochRole": "one leaf's credential identity and its role at one epoch, ledger item 242's R4 and its " +
		"ruling 18. It is ReceiverRatchetKey's row read for a different pair: both halves travel in the " +
		"clear and every member of the group already holds them. The identity is published in the leaf " +
		"node it stands in -- in the ratchet tree beside every Welcome, and answered on request by " +
		"(*Group).Members, which is where this value is read FROM -- and the role is one row of " +
		"urmessage_group_policy, an entry of the group context extension list the transcript covers. An " +
		"erase here would blank octets an attacker reading the wire already has. What the two tables " +
		"holding these owe is a LIFETIME and not an erase, and GroupSession.roles says what it is",
	"ReceiverRatchetKey": "the two values one receiver ratchet is TRACKED under: the sender handle the message " +
		"server routes on, which every member of the group computes and which is in the cleartext header of " +
		"every record, and the retention class wire byte, which is one octet of that same header. It is " +
		"BodyBinding one row up read again: both halves travel in the clear beside the ciphertext they " +
		"address, so an erase here would remove nothing an attacker lacks. The key material is in the " +
		"ReceiverRatchet the key points at, and that type is a member of this class in its own right",
	"CommitResult": "the ENCODINGS a committer hands its caller to send -- the commit message, the welcome and " +
		"the ratchet tree. Every octet of all three is about to be on the wire, which is the whole purpose of " +
		"the structure. The key material this commit holds is in the staged epoch beside it, and that is a " +
		"member of this class in its own right",
	"CommitValidationInput": "an ARGUMENT and not storage. validate_commit.go builds one per call out of state " +
		"its caller already holds -- the confirmation key it carries is the key schedule's, and the key " +
		"schedule erases it -- so there is no drop site here an erase could be reachable from",
	"EpochAttachment": "the write key and read key a commit DELIVERS TO THE SERVER, inside the record that " +
		"carries the commit. They are octets the server is being handed on purpose; erasing them here would " +
		"erase what the message layer exists to send",
	"EpochDigestAttachment": "what attachment kind 0x0005 carries, and the one row in this family that does " +
		"NOT have to say 'the server is handed these on purpose'. It is EpochAttachment one row up with " +
		"write_key and read_key STRUCK, which is the whole of ruling 27 of 2026-09-22: the two fields it does " +
		"hold are a hash of the group context and H(epoch_keys), both of them served and neither of them " +
		"invertible to a key. There is nothing here an erase would take from an attacker, because the " +
		"amendment took it out of the structure instead. THE ROW WAS OWED BY ffda0ab2, which declared the " +
		"type and did not write one, and this gate has been red since -- measured in a worktree at that " +
		"commit, where this test fails on this type alone and the drop-site gate beside it passes at 52 sites",
	"EpochSecrets": "the key schedule's own storage, grouped. Exactly one production declaration holds one -- " +
		"KeySchedule.secrets -- and (*KeySchedule).Zeroize erases every one of its nine fields by name, which " +
		"this reading checks rather than infers: the key schedule is read here like every other member, an " +
		"erase of self.secrets.SenderData credits that part alone, and eraseWholeFieldsOf accounts for the " +
		"field only once every part of it that reaches octets has been named",
	"GroupConfig": "the configuration a caller hands NewGroup: a group id and the leaf keys extension it " +
		"publishes, both of which end up in the group context and in this member's own leaf node",
	"HpkeContext": "an ANSWER and not storage, like PathDecryptResult. hpkeKeySchedule builds one per seal or " +
		"open and no production declaration holds one in a field, so there is no drop site an erase could be " +
		"reachable from. The secrets it derives are gone with the frame",
	"LeafKeysExtension": "the device's PUBLISHED X-Wing public key. It travels in this member's own leaf node, " +
		"which every member of the group holds and every joiner is handed in its Welcome",
	"LeafValidationContext": "an argument naming the group a leaf is being judged against; the group id is what " +
		"the leaf itself carries",
	"Member": "the public view of one member this package hands its caller: an identity, a signature public " +
		"key and the leaf keys extension. Every field is read out of that member's own published leaf node",
	"PreSharedKeyInput": "an ARGUMENT. PskSecret takes a list of them, extracts each into the chain and erases " +
		"its own intermediates; the secret in an entry is storage the CALLER owns and is still holding, and " +
		"erasing it here would erase a value the caller passed by value",
	"ProposalCache": "proposals this member received, by reference. Every one of them arrived as a message the " +
		"delivery service also saw; Group.proposals is excused for the same reason",
	"ProposalList": "the resolved proposals a commit names, which is exactly what the commit puts on the wire",
	"ProposalValidationInput": "an argument. It carries the tree, the context and the proposal list a door is " +
		"about to judge, all of them public and all of them owned by the caller",
	"Reader": "the decoder's cursor over octets its CALLER owns. It holds no copy: the slice is the caller's " +
		"buffer, an erase here would blank the message somebody is still parsing, and every secret ever read " +
		"through it lands in a structure that owes its own erase",
	"Record": "a record ON THE WIRE. Its header, its two ciphertexts and its write authenticator are the " +
		"octets the delivery service carries",
	"RecordHeader": "the cleartext header of a record on the wire: the routing the server reads without " +
		"holding any group key",
	"RecoveryTag": "the recovery handle and the recovery VERIFY key, which are the public half of the recovery " +
		"pair and travel in the record's own attachment",
	"ServerAttachment": "the attachment a record carries FOR THE SERVER. Every arm of it is delivered to a " +
		"party this product does not trust with the group's content and does trust with these octets",
	"TranscriptHashes": "the confirmed and interim transcript hashes, which are hashes over messages every " +
		"member received and every member of the group computes for itself",
	"TreeValidationContext": "an argument naming the group a tree is being judged against; the group id is what " +
		"the tree's own leaves are signed over",
	"WrapTag": "the handle a wrapped record names, which is routing the server reads",
	"Writer": "the encoder's growing buffer, which is ANSWERED to the caller rather than retained -- Bytes() " +
		"hands it out. An erase here would blank the message a caller is about to send, and the caller that " +
		"marshalled a secret through it owns what came back",
	"XwingPrivateKey": "an ANSWER. XwingGenerateKey and XwingKeyGenFromSeed build one per call and no " +
		"production declaration holds one in a field, which messagegroup's " +
		"TestNoDeclarationOfThisPackageHoldsAnXwingPrivateKeyInAField measures. THE SECOND CLAUSE OF " +
		"THIS ROW USED TO READ 'the seed inside it is the caller's to keep or to drop' AND NOTHING " +
		"MEASURED IT, because it was not true: the type declares no erase and every field of it is " +
		"unexported, so no caller anywhere -- this package included -- has a way to drop what it " +
		"holds. What is true is the weaker sentence: the erase this class would demand has nowhere " +
		"to land, so the obligation is unmeetable rather than unmet, and the residual is named " +
		"instead of argued away. A dropped one leaves its own thirty-two octet seed and the parsed " +
		"ML-KEM and x25519 halves for the collector, on every sdk Device.DecapsulateToOwnLeaf, and " +
		"closing that is ledger item 243's and not this gate's. The two facts the clause now rests " +
		"on are measured by messagegroup's TestXwingPrivateKeyOffersNoWayToDropWhatItHolds, which " +
		"goes red the day either one changes, so this sentence has to change with the code",
	"XwingPublicKey": "the public half, whose two components are exactly what a peer is sent",
	"cachedProposalFieldJoin": "the octets and the field names of one proposal's encoding, assembled so a " +
		"commit's list can be compared field by field against what arrived on the wire",
	"generationKeys": "one generation's AEAD key and nonce, held only by ratchet.window, and (*ratchet).zeroize " +
		"erases both by name. The obligation lands on the ratchet, which is a member of this class in its own " +
		"right and whose erase this reading now checks part by part",
	"proposalBucket": "the entries one sender holds in one epoch's cache, which are proposals already on the wire",
	"proposalCacheBinding": "the group id and epoch a cache is bound to, which is the group context every " +
		"member of the group holds",
	"suiteCryptoProvider": "the suite parameters and the entropy SOURCE. The source is an io.Reader and holds " +
		"none of this process's storage; it is in this class only because the field reading resolves the bare " +
		"name Reader to the decoder this package declares, which is a collision across two packages and not a " +
		"byte slice anybody could erase",
	"senderLadderKey": "the retention class wire byte and the eph window one of a session's own sender " +
		"ladders is held under. It is ReceiverRatchetKey one row up read from the sending side, and the " +
		"same two sentences answer it: the retention byte is one octet of the cleartext header of every " +
		"record, and the eph window is a u64 that MASTER section 8's presence rule puts on the wire IN " +
		"THE CLEAR beside it -- ruled 2026-09-13, and the plaintext of it is the whole reason an opener " +
		"can take the sender's window rather than recompute one. Neither half is anything an attacker " +
		"lacks. The key material is in the SenderRatchet the key points at, and that type is a member of " +
		"this class in its own right and erases what it holds",
	"StreamKey": "the group and the sender handle a reservation belongs to, which is what spec B's " +
		"schema, spec B's Q7 and the shipped message server all key the stream index counter by. Both are " +
		"in the CLEARTEXT header of every record: the server keys its rows on the group id and routes on " +
		"the sender handle. There is nothing here to erase, which is why the row is a whole-type one " +
		"rather than two field rows -- a type all of whose fields are excused is a type this class swept " +
		"in, and this one holds no secret at all. It carried a third field, the retention class wire " +
		"byte, until the owner's ruling of 2026-09-07 made the counter class blind",
	"EngineProcessed": "it is an ANSWER and not storage. (*connectMlsHandle).Process builds one per call " +
		"and hands it to a caller that owns the plaintext it carries; no production declaration of either " +
		"package holds one in a field, so there is no drop site an erase could be reachable from. Raw is " +
		"the message these values were read out of -- octets the delivery service and every member saw -- " +
		"and the staged commit is behind an unexported field whose own type, mls.Processed, is a member of " +
		"this class in its own right. PathDecryptResult is excused in the same words for the same shape",
	"recordBodySealed": "one stage of the sealer's construction order. Every octet it declares is a " +
		"CIPHERTEXT already destined for the wire, and the one value on the chain that is key material -- " +
		"record_key -- is recordBuilder's, which erases it on every exit including the refusals. A stage " +
		"that erased its own ciphertext would blank the record its caller is about to send",
	"recordBodyBound":  "one stage of the sealer's construction order; see recordBodySealed",
	"recordHeadSealed": "one stage of the sealer's construction order; see recordBodySealed",
	"stagedProcessed": "what the connect/mls adapter puts in EngineProcessed.stagedRef: the handle that " +
		"staged a commit and the mls.Processed it staged. It declares no octets of its own -- both fields " +
		"are pointers -- and both types it points at are members of this class in their own right, which " +
		"is where the obligation belongs",
	"ProcessedMember": "one occupied leaf of the tree a commit enters, with the CREDENTIAL IDENTITY that leaf " +
		"carries: an Ed25519 identity public key read out of a published leaf node, which every member of " +
		"the group holds, every joiner is handed in its Welcome, and Member.IdentityPub answers one type " +
		"over. It is an ANSWER carried on EngineProcessed's commit arm and no production declaration holds " +
		"one in a field, so there is no drop site an erase could be reachable from",
	"ExtensionBytes": "one group-context extension as octets, the RFC 9420 extension type and its body, " +
		"which is the seam's spelling of Extension -- a wire type. The lists it travels in are the group " +
		"context's, which framing signs and MACs over and which every Welcome carries, so every octet of " +
		"it is one every member of the group holds; Group.context and StagedCommit.context are excused in " +
		"the same words. It is an answer on EngineProcessed's commit arm and an argument to " +
		"CommitContextExtensions, and no production declaration holds one in a field",
	"PendingEpoch": "the epoch a handle's OWN staged commit would open, as the committer reads it before the " +
		"delivery service has answered (ledger item 242's R2, 2026-09-22): the epoch number, the member count " +
		"of the staged tree, and the serialized post-commit GroupContext -- which is the value GroupContext " +
		"answers once the commit is merged, which framing signs and MACs over, and which every member of the " +
		"group derives for itself from the same commit; Group.context and StagedCommit.context are excused in " +
		"the same words. The one secret an announcement needs, the staged epoch's exporter, is deliberately " +
		"NOT a field of it: PendingExport is a call that answers a fresh derivation the caller erases, exactly " +
		"as Export's is. It is an ANSWER built by (*connectMlsHandle).PendingEpoch per call and no production " +
		"declaration holds one in a field, so there is no drop site an erase could be reachable from",
	"PathDecryptResult": "it is an ANSWER and not storage. DecryptUpdatePath builds one per call and hands " +
		"it to a caller that installs both halves into the epoch it is entering, and no production " +
		"declaration holds one in a field, so there is no drop site an erase could be reachable from. " +
		"The obligation lands on the types that RETAIN this material: the key schedule the commit " +
		"secret is derived into, and the private tree state the group installs -- both of which are " +
		"members of this class in their own right",
}

// theFieldsOfTheEraseClassThatAreNotKeyMaterial is every field of a member that reaches octets and
// is not a secret, with the reason written out for each.
//
// Checked in both directions against the derived field set, which is the shape standing rule 5
// asks for: derive the members, and make every exception carry a sentence somebody had to write.
var theFieldsOfTheEraseClassThatAreNotKeyMaterial = map[string]string{
	"Group.cred": "the credential is this member's identity and it is published in its own leaf node, " +
		"which every member of the group holds and every joiner is handed in its Welcome",
	"Group.tree": "the ratchet tree is public. It carries encryption PUBLIC keys, signature public keys " +
		"and hashes, it travels beside every Welcome, and (*Group).RatchetTree hands it out on request",
	"Group.context": "the group context is what framing signs and MACs over and what a Welcome carries; " +
		"scheduleByteSlicesThatAreNotSecrets excuses the schedule's serialized copy of it for the same reason",
	"Group.verified": "the same context with its authority established. It holds no storage the context " +
		"does not, and a Clone of it is what the type hands out",
	"Group.transcript": "the confirmed and interim transcript hashes are hashes over messages every member " +
		"received, and every member of the group computes both",
	"Group.proposals": "the cache holds proposals this member received and their references, which travelled " +
		"to every member of the group as messages the delivery service also saw",
	"StagedCommit.context": "the group context of the epoch this commit opens; see Group.context",
	"StagedCommit.groupId": "the id of the group that staged this commit, which is the same octets " +
		"StagedCommit.context carries one row down and Group.context carries one type over: a group id " +
		"is in every GroupContext, in every FramedContent this group frames, and in the GroupInfo of " +
		"every Welcome it sends. It is here as a field of its own because the staged value a REMOVED " +
		"member is handed carries no context, and (*Group).ApplyCommit reads it on that path too",
	"StagedCommit.verified":   "see Group.verified",
	"StagedCommit.tree":       "the post-commit ratchet tree, which the committer PUBLISHES: CommitResult carries it",
	"StagedCommit.transcript": "see Group.transcript",
	"StagedCommit.list": "the resolved proposal list, which is the proposals this commit names -- every one " +
		"of them already on the wire",
	"StagedCommit.commit": "the Commit structure as it was signed and sealed, which is what went to every " +
		"member of the group",
	"StagedCommit.confirmTag": "the confirmation tag travels in the commit's own auth data and every receiver " +
		"recomputes it; the confirmation KEY it is taken under is the secret, and the schedule erases that",
	"KeySchedule.groupContextBytes": "the serialized GroupContext this epoch's secrets were expanded " +
		"over. It is what framing signs and MACs over and what a Welcome carries, so every member of the " +
		"group holds it; key_schedule.go's scheduleByteSlicesThatAreNotSecrets excuses the same field to " +
		"the field-by-field gate, in the same words and for the same reason",
	"WelcomeJoiner.KeyPackage": "the joiner's PUBLISHED key package. It arrived in an Add proposal that went to " +
		"every member of the group and to the delivery service, every field of its encoding is public, and the " +
		"one field of the Go struct that is not encoded -- signPriv -- is written only by this file's two " +
		"constructors, NewKeyPackage and NewKeyPackageWithSigner, and " +
		"cleared by UnmarshalMLS, so a committer's copy of somebody else's key package holds no private half. " +
		"Erasing it would remove nothing an attacker lacks and would destroy a value the caller still owns; " +
		"the path secret beside it is the key material, and (*WelcomeJoiner).Zeroize erases that",
	"SenderRatchet.stream": "the group and the sender handle this ratchet allocates its stream indices " +
		"under. Both are in the cleartext header of every record: the message server keys its store rows " +
		"on the group id and routes on the sender handle. StagedCommit.groupId is excused three rows up in " +
		"the same words. The ratchet holds a StreamKey by VALUE -- no field of it is a reference a caller " +
		"can write through -- so a caller reusing its buffers cannot move which row the reservations land " +
		"in, and the copy is as public as the one the caller kept",
	"GroupSession.groupId": "the id of the group this session is a view of, which is in the cleartext " +
		"header of every record it seals and is what the message server keys every row of its store on; " +
		"see SenderRatchet.stream and StagedCommit.groupId",
	"GroupSession.senderHandle": "the handle this session's own records are ROUTED by. It is in the " +
		"cleartext header of every record it seals, which is the whole reason it exists -- the server has " +
		"to be able to route on it without being able to link it to a leaf. The value it is derived FROM, " +
		"group_handle_key, is the secret, and zeroizeOnLoop erases that",
	"GroupSession.serverNonce": "the submitting connection's nonce, which the SERVER chose and sent in " +
		"the clear. write_auth is a mac over it under the group's write key; the key is the secret and the " +
		"nonce is the challenge",
	"recordBuilder.session": "a pointer to the session this record is being sealed by, which is a member " +
		"of this class in its own right and erases everything it holds",
	"recordBuilder.header": "the record's own cleartext header. Every field of it is one the message " +
		"server reads and acts on -- MASTER invariant I6 -- and it is what aad_head and the write_auth " +
		"preimage are taken over",
	"recordBuilder.bodySeal": "the octets ct_body is sealed OVER, which are a PLAINTEXT rather than a " +
		"key and are not always this package's to erase. For an application record they are the MLS " +
		"PrivateMessage frameBodyOnLoop produced -- already a ciphertext, under the sender's own MLS " +
		"ratchet -- and for a commit, a wrap, an epoch fan out and a completion marker they are the " +
		"CALLER'S OWN ARRAY, handed to SealRecord and still the caller's after it returns. An erase here " +
		"would blank a buffer its owner is holding, which is the defect WelcomeJoiner.KeyPackage is " +
		"excused against one table over. These are the same octets sealBody took as a PARAMETER before " +
		"MASTER section 8.4 moved the framing in front of the rung, where no field-based reading could " +
		"see them: what changed is the representation and not the exposure. The one value on this chain " +
		"that is key material is recordKey, and zeroize erases it on every exit including the refusals",
	"GroupSession.roles": "this epoch's leaf -> (identity, role) table, ledger item 242's R4 and its " +
		"ruling 18. Both halves of an entry are values every member of the group already holds: a " +
		"credential identity is published in the leaf node it stands in, which every member holds and " +
		"every joiner is handed in its Welcome -- Group.cred and connectMlsEngine.cred are excused in " +
		"the same words -- and a role is one row of urmessage_group_policy, an entry of the group " +
		"context extension list the transcript covers and every Welcome carries, excused as " +
		"Group.context is. What this table owes is not an erase but a LIFETIME: it is keyed by LEAF " +
		"INDEX and a role change moves nothing else about a leaf, so a table that survived an epoch " +
		"install would answer the new epoch's question with the role that leaf held at the old one. " +
		"installEpochOnLoop drops it on the line beside the one re-making pastEpochs, and zeroizeOnLoop " +
		"with the rest",
	"pastEpoch.roles": "the same table for one PRIOR epoch, held as a field of that epoch's schedule " +
		"rather than in a map keyed by epoch precisely so that the schedule's death is its death; see " +
		"GroupSession.roles for why nothing in it is key material",
	"connectMlsEngine.cred": "this device's credential, which is its identity public key. It is published " +
		"in the leaf node of every group this engine founds, every member holds it and every joiner is " +
		"handed it in its Welcome; Group.cred is excused in the same words",
	"connectMlsEngine.leafKeys": "the encoded urmessage_leaf_keys body: an algorithm identifier and this " +
		"device's X-Wing ENCAPSULATION key. It is the public half, it travels in this device's leaf into " +
		"every group's ratchet tree, and it is exactly what a peer needs in order to wrap to this device",
	"UpdatePathPlan.PublicKeys": "the public half of the path this commit publishes, which is exactly what the " +
		"UpdatePath on the wire carries",
	"UpdatePathPlan.LeafNode": "the re-signed leaf node this commit installs in the tree, which is public the " +
		"moment the commit is sent",
	"UpdatePathPlan.Private": "the leaf private state a MERGE INSTALLS as the group's own. A plan that erased " +
		"it would erase the key the epoch it opened runs on, so the holder that DROPS the plan erases it " +
		"instead: (*StagedCommit).Zeroize hands ownPriv -- the same pointer -- to (*TreeKEMPrivate).Zeroize, " +
		"and (*Group).MergePendingCommit erases the leaf state it REPLACED rather than the one it installed",
}

// TestEveryTypeHoldingErasableKeyMaterialErasesAllOfIt is the completeness half, read off the
// source because no behavioural test can reach every field of every holder.
func TestEveryTypeHoldingErasableKeyMaterialErasesAllOfIt(t *testing.T) {
	reading := eraseReadingOf(t)
	// the control first, over a package holding one of each shape this reading must separate.
	eraseControlReading(t)

	// the declarer of the epoch secret storage, derived rather than named. It is no longer SKIPPED
	// here, and that is what closes the second measured hole: while this gate stepped over it, the
	// EpochSecrets row below said the reading "now checks rather than infers" that all nine
	// secrets are named, and deleting one of the nine passed both gates in this file. The check is
	// eraseWholeFieldsOf's: the field secrets is accounted for only when every part of it that
	// reaches octets was erased in its own right, so a deleted zeroizeSecret leaves the whole
	// field uncovered.
	//
	// The single-declarer assertion stays, because it is what says there is one place the nine can
	// live -- a second declarer is storage the field-by-field gate one file over, which reads one,
	// would never be pointed at.
	declaring := theTypesDeclaringTheEpochSecret(reading.structs)
	if len(declaring) != 1 {
		t.Fatalf("%v declare the epoch secret storage and the field-by-field gate beside this one reads a single declarer; a second one is a type nothing holds to that reading",
			declaring)
	}
	if len(reading.members) < 4 {
		t.Fatalf("the erase class read %v; this package holds a key schedule, a secret tree, a private tree state and the staged epoch that carries all three, so a class this small is not reading what it claims to",
			reading.members)
	}
	t.Logf("erase class: %v", reading.members)

	excused := map[string]bool{}
	for _, member := range reading.members {
		if reason, isExcused := typesTheEraseClassReachesThatOweNoErase[member]; isExcused {
			excused[member] = true
			if len(reading.erasers[member]) != 0 {
				t.Errorf("%s declares an erase and typesTheEraseClassReachesThatOweNoErase excuses it as %q; one of the two is wrong and which one holds cannot be read off either",
					member, reason)
			}
			continue
		}
		fields := theFieldsReachingStorageOf(reading, member)
		wanted := []string{}
		for _, field := range fields {
			// the fields a member's OWN CODEC writes are excused by the encoder rather than by a
			// row, which is the whole of what the per-field wire reading buys here: KeyPackage
			// owes an erase for signPriv and for nothing else, and nobody has to write four
			// sentences saying that an init key, a leaf node, an extension list and a signature
			// are public.
			if reading.encoded[member][field.name] {
				continue
			}
			if _, isExcused := theFieldsOfTheEraseClassThatAreNotKeyMaterial[member+"."+field.name]; isExcused {
				continue
			}
			wanted = append(wanted, field.name)
		}
		if len(wanted) == 0 {
			t.Errorf("%s is in the erase class and every field of it that reaches octets is excused; a member with nothing to erase is a closure that swept in a type holding no key material",
				member)
			continue
		}
		best, covered := "", map[string]bool{}
		for _, method := range methodsWithNoParametersIn(reading.files) {
			if method.receiver != member || !reading.erasers[member][method.name] {
				continue
			}
			erased := eraseWholeFieldsOf(reading, fields, theFieldsErasedBy(reading, method, fields))
			if best == "" || len(erased) > len(covered) {
				best, covered = method.name, erased
			}
		}
		if best == "" {
			t.Errorf("%s holds %v and declares no erase; every type here that holds key material owes one, and a value dropped by any path is dropped with all of that still in it",
				member, wanted)
			continue
		}
		missing := []string{}
		for _, field := range wanted {
			if !covered[field] {
				missing = append(missing, field)
			}
		}
		if len(missing) != 0 {
			t.Errorf("(*%s).%s erases %v and leaves %v; a field this erase does not reach is key material the drop sites below cannot erase however carefully they call it",
				member, best, slices.Sorted(maps.Keys(covered)), missing)
		}
	}

	// and the tables in the other direction, so no row outlives what it excuses.
	for typeName := range typesTheEraseClassReachesThatOweNoErase {
		if !slices.Contains(reading.members, typeName) {
			t.Errorf("typesTheEraseClassReachesThatOweNoErase excuses %s, which the erase class does not reach; a row that outlived its type excuses nothing and hides that it does",
				typeName)
		}
	}
	for row := range theFieldsOfTheEraseClassThatAreNotKeyMaterial {
		typeName, fieldName, split := strings.Cut(row, ".")
		if !split {
			t.Errorf("theFieldsOfTheEraseClassThatAreNotKeyMaterial names %q, which is not a Type.field row", row)
			continue
		}
		if !slices.Contains(reading.members, typeName) || excused[typeName] {
			t.Errorf("theFieldsOfTheEraseClassThatAreNotKeyMaterial excuses %s, and %s is not a member of the erase class this gate reads fields of",
				row, typeName)
			continue
		}
		found := false
		for _, field := range theFieldsReachingStorageOf(reading, typeName) {
			if field.name == fieldName {
				found = true
			}
		}
		if !found {
			t.Errorf("theFieldsOfTheEraseClassThatAreNotKeyMaterial excuses %s, which is not a field of %s that reaches octets; a row that outlived its field excuses nothing",
				row, typeName)
		}
	}
}

// ---------------------------------------------------------------------------
// the drop sites
// ---------------------------------------------------------------------------

// eraseDropSite is one assignment a member's own method makes to a field of itself that holds key
// material, and what the body does about the value it is dropping.
type eraseDropSite struct {
	method  eraseMethod
	field   eraseField
	at      int
	erased  bool
	refused bool
	// the sub-fields of the dropped value this body reads out into storage of its own, and the
	// ones it erases: between them, a value that is being MOVED rather than dropped.
	installed map[string]bool
	movedOut  map[string]bool
	// the WHOLE dropped value read out, by pointer, into storage the body was handed -- its
	// receiver's or a parameter's -- ABOVE the assignment. That is the other shape of a move, and
	// it is the one (*Group).MergePendingCommit makes over the schedule, the secret tree and
	// the leaf private state: `self.schedule = staged.schedule` and then, so that the staged
	// value is the shell the move promises, `staged.schedule = nil`. The second assignment drops
	// nothing -- the value stands in self.schedule -- and a reading over sub-fields alone
	// reported it as a schedule left in the heap. Read at a POSITION for the order's usual
	// reason: an install written below the nil installs the nil.
	movedWhole bool
}

// theDropSitesIn reads every assignment to a key-material field of the receiver's own type.
//
// The three ways a body may drop such a value, and every one of them is read off the body rather
// than off a list of method names:
//
//   - it ERASES the value first. The order is read, because an erase written after the assignment
//     erases whatever was just installed and leaves what it replaced in the heap;
//   - it REFUSES to overwrite a live one, which is (*Group).CreateCommit: a body comparing the
//     field against nil and returning is a body whose assignment can only ever land on nothing.
//     The order is read here too, and for the same reason it is read on the erase: a comparison
//     written BELOW the assignment says nothing about it, the value it would have refused having
//     already been dropped by the time it is reached;
//   - or it MOVES the value, which is (*Group).MergePendingCommit, in one of two shapes. Field
//     by field: every field of the dropped value is either read out into the holder's own
//     storage or erased, and what is left is a shell. Or WHOLE: the value itself is read out,
//     by pointer, into storage the body was handed -- its receiver's or a parameter's -- above
//     the assignment, and the assignment is then what MAKES the shell rather than a drop. The
//     merge does the first over its pending commit and the second over the commit's schedule,
//     secret tree and leaf private state, and it must do the second: measured on 2026-09-21, a
//     merge that left those three standing in the staged value left a caller's ordinary
//     cleanup erasing the epoch the group had just entered. A merge that erased what it had
//     just installed would be the same defect pointing the other way.
func theDropSitesIn(reading eraseSourceReading, member string) []eraseDropSite {
	fields := theFieldsReachingStorageOf(reading, member)
	byName := map[string]eraseField{}
	for _, field := range fields {
		byName[field.name] = field
	}
	// every name this source erases through, so an erase of a sub-field is told from any other
	// method call on it. Derived from the helper class and from the types' own erases rather
	// than written down.
	eraseNames := map[string]bool{}
	for _, helper := range reading.helpers {
		eraseNames[helper] = true
	}
	for _, methods := range reading.erasers {
		for name := range methods {
			eraseNames[name] = true
		}
	}
	sites := []eraseDropSite{}
	for _, method := range declarationsHolding(reading.files, member) {
		alias := eraseAliasesIn(method.decl, method.self)
		// IN FULL, because this is the gate that says a value was erased before it was
		// dropped, and a body that erased one part of it dropped the rest.
		erased := eraseWholeFieldsOf(reading, fields, theFieldsErasedBy(reading, method, fields))
		erasedAt := map[string]int{}
		// WHERE the refusal stands and not merely that one is written, for the reason the erase
		// is positioned: a refusal is a claim about the assignment BELOW it. This gate read the
		// erase at a position from the day it was written and read the refusal as a bare flag,
		// and the hole that left was measured -- a body dropping a live staged epoch and
		// comparing the field against nil AFTERWARDS was excused, the whole suite stayed green,
		// and the site count did not move because a covered site had been swapped for an excused
		// one.
		refusedAt := map[string]int{}
		installed := map[string]map[string]bool{}
		movedOut := map[string]map[string]bool{}
		// where the WHOLE held value was read out into storage the body was handed, per
		// field: the receiver's storage or a parameter's, which is storage that outlives the
		// frame. A local is not a position here for the reason it is not one below.
		handed := eraseHandedNamesOf(method.decl)
		movedWholeAt := map[string]int{}
		ast.Inspect(method.decl.Body, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.CallExpr:
				// where the erase of a field stands, so the order can be read.
				for _, argument := range typed.Args {
					if field := eraseFieldNameOf(argument, method.self, alias); field != "" && erased[field] {
						if at, seen := erasedAt[field]; !seen || int(typed.Pos()) < at {
							erasedAt[field] = int(typed.Pos())
						}
					}
				}
				if selector, isSelector := typed.Fun.(*ast.SelectorExpr); isSelector {
					if field := eraseFieldNameOf(selector.X, method.self, alias); field != "" && erased[field] {
						if at, seen := erasedAt[field]; !seen || int(typed.Pos()) < at {
							erasedAt[field] = int(typed.Pos())
						}
					}
					// an erase run on one SUB-field of a value this body is moving out, which is
					// the other half of a move: what the holder does not take, it erases.
					if eraseNames[selector.Sel.Name] {
						for held := range byName {
							if sub := eraseSubFieldOn(selector.X, method.self, alias, held); sub != "" {
								if movedOut[held] == nil {
									movedOut[held] = map[string]bool{}
								}
								movedOut[held][sub] = true
							}
						}
					}
				}
			case *ast.IfStmt:
				// A REFUSAL IS A DIRECTION AND NOT A COMPARISON. Both shapes compare the field
				// against nil, and only one of them says the assignment below can never land on a
				// live value: `if self.f != nil { return ... }` LEAVES when there is something to
				// drop, and `if self.f == nil { return }` leaves when there is NOTHING to drop and
				// then drops a live value every time it is reached -- a presence guard, which is
				// the opposite claim and is (*Group).MergePendingCommit's own opening line. Read as
				// any nil comparison, a production holder dropping a live *UpdatePathPlan behind a
				// presence guard left this whole gate green.
				comparison, isComparison := typed.Cond.(*ast.BinaryExpr)
				if !isComparison || comparison.Op != token.NEQ || !eraseBranchLeaves(typed.Body) {
					return true
				}
				for _, side := range [][2]ast.Expr{
					{comparison.X, comparison.Y}, {comparison.Y, comparison.X}} {

					if name, isBare := side[1].(*ast.Ident); !isBare || name.Name != "nil" {
						continue
					}
					if field := eraseFieldNameOf(side[0], method.self, alias); field != "" {
						if at, seen := refusedAt[field]; !seen || int(typed.Pos()) < at {
							refusedAt[field] = int(typed.Pos())
						}
					}
				}
			case *ast.AssignStmt:
				// a sub-field read out INTO STORAGE OF THE HOLDER, which is what an install is.
				// A read into a local is not one: the value would go out of scope with the frame.
				if len(typed.Lhs) == len(typed.Rhs) {
					for i, right := range typed.Rhs {
						left, isSelector := typed.Lhs[i].(*ast.SelectorExpr)
						if !isSelector {
							continue
						}
						// the whole held value, by pointer, into storage the body was handed
						if base, isBare := left.X.(*ast.Ident); isBare && handed[base.Name] {
							if field := eraseWholeFieldOn(right, method.self); field != "" {
								if at, seen := movedWholeAt[field]; !seen || int(typed.Pos()) < at {
									movedWholeAt[field] = int(typed.Pos())
								}
							}
						}
						if base, isBare := left.X.(*ast.Ident); !isBare || base.Name != method.self {
							continue
						}
						for held := range byName {
							if sub := eraseSubFieldOn(right, method.self, alias, held); sub != "" {
								if installed[held] == nil {
									installed[held] = map[string]bool{}
								}
								installed[held][sub] = true
							}
						}
					}
				}
			}
			return true
		})
		ast.Inspect(method.decl.Body, func(node ast.Node) bool {
			assign, isAssign := node.(*ast.AssignStmt)
			if !isAssign {
				return true
			}
			for _, left := range assign.Lhs {
				selector, isSelector := left.(*ast.SelectorExpr)
				if !isSelector {
					continue
				}
				base, isBare := selector.X.(*ast.Ident)
				if !isBare || base.Name != method.self {
					continue
				}
				field, isHeld := byName[selector.Sel.Name]
				if !isHeld {
					continue
				}
				erasedPos, wasErased := erasedAt[field.name]
				refusedPos, wasRefused := refusedAt[field.name]
				movedPos, wasMoved := movedWholeAt[field.name]
				sites = append(sites, eraseDropSite{
					method:     method,
					field:      field,
					at:         int(assign.Pos()),
					erased:     wasErased && erasedPos < int(assign.Pos()),
					refused:    wasRefused && refusedPos < int(assign.Pos()),
					installed:  installed[field.name],
					movedOut:   movedOut[field.name],
					movedWhole: wasMoved && movedPos < int(assign.Pos()),
				})
			}
			return true
		})
	}
	return sites
}

// eraseHandedNamesOf is the names a declaration holds what it was HANDED under: its receiver and
// its parameters. Storage reached through one of them outlives the frame, which is what makes an
// assignment into it an install and an assignment into a local not one.
func eraseHandedNamesOf(function *ast.FuncDecl) map[string]bool {
	handed := map[string]bool{}
	if function.Recv != nil {
		for _, field := range function.Recv.List {
			for _, name := range field.Names {
				handed[name.Name] = true
			}
		}
	}
	if function.Type.Params != nil {
		for _, field := range function.Type.Params.List {
			for _, name := range field.Names {
				handed[name.Name] = true
			}
		}
	}
	delete(handed, "_")
	return handed
}

// eraseWholeFieldOn answers which field of the holder an expression IS, whole: staged.schedule,
// where staged is the name the holder is read under, answers "schedule", and staged.schedule.x
// answers nothing, because a sub-field read out is the other reading's.
func eraseWholeFieldOn(expr ast.Expr, receiver string) string {
	selector, isSelector := expr.(*ast.SelectorExpr)
	if !isSelector {
		return ""
	}
	if base, isBare := selector.X.(*ast.Ident); isBare && base.Name == receiver {
		return selector.Sel.Name
	}
	return ""
}

// eraseBranchLeaves reports whether a branch LEAVES rather than falling through to the
// assignment below it.
//
// It is the half of a refusal the comparison cannot supply. `if self.f != nil { self.f
// .Zeroize() }` compares in the refusing direction and then goes on to the drop, and a
// reading that stopped at the operator would call that a refusal -- (*Group).Close and
// (*StagedCommit).Zeroize both open with exactly that shape.
func eraseBranchLeaves(body *ast.BlockStmt) bool {
	if body == nil || len(body.List) == 0 {
		return false
	}
	switch last := body.List[len(body.List)-1].(type) {
	case *ast.ReturnStmt:
		return true
	case *ast.BranchStmt:
		return last.Tok != token.FALLTHROUGH
	case *ast.ExprStmt:
		// a panic leaves as surely as a return, and it is how a package that treats the
		// condition as a programming error rather than as a caller's fault writes it.
		call, isCall := last.X.(*ast.CallExpr)
		if !isCall {
			return false
		}
		name, isBare := call.Fun.(*ast.Ident)
		return isBare && name.Name == "panic"
	}
	return false
}

// eraseSubFieldOn answers which sub-field of `field` an expression reads: staged.schedule, where
// a local named staged was bound from self.pending, answers "schedule".
func eraseSubFieldOn(expr ast.Expr, receiver string, alias map[string]string, field string) string {
	selector, isSelector := expr.(*ast.SelectorExpr)
	if !isSelector {
		return ""
	}
	if eraseFieldNameOf(selector.X, receiver, alias) != field {
		return ""
	}
	// self.field itself resolves to the field; a sub-field of it is one selector further in.
	if base, isBare := selector.X.(*ast.Ident); isBare && base.Name == receiver {
		return ""
	}
	return selector.Sel.Name
}

// declarationsHolding is every declaration of the source that is HANDED one of these values --
// in the receiver, or in a parameter -- with the name it holds it under, one entry per position.
//
// THE POSITION IS DERIVED AND NOT NAMED, which is the defect the seam gate next door records
// having fixed and the generator rule beside it records having reintroduced. A reading rooted in
// the receiver alone asks the erase obligation of methods and of nothing else, and a free
// function in this package handed a *Group -- or a declaration in connect/message handed an
// *UpdatePathPlan, whose secret fields are exported -- drops exactly what a method drops.
//
// A LOCAL A BODY CONSTRUCTED IS NOT A POSITION, AND A LOCAL IT TOOK OUT OF THE RECEIVER IS. The
// boundary used to sit at "locals are not positions" full stop, justified as "a value a body
// CONSTRUCTED is not a value anybody handed it" -- and that justification does not cover the case
// the boundary dropped. (*SecretTree).RestoreSenderRatchets writes a fresh ratchet secret over
// r.secret, where r came out of self.ratchetFor: the body did not construct it, it is the
// RECEIVER'S own storage reached one call away, and dropping it drops a live ratchet secret.
// Measured both ways. With this widening, deleting the zeroizeSecret in front of that assignment
// fails the gate below naming the site. With the widening taken out again, the reading finds
// thirteen drop sites, that assignment is not one of them, and the gate reports the source clean
// with the erase gone -- which is the state this package shipped in.
//
// What is still excluded is what the old sentence was actually protecting.
// CreateUpdatePathSecrets writes private.EncryptionPriv over the nil a constructor two statements
// up put there, and that local IS constructed here, so it is sourced from no receiver and stays
// out. The widening reaches ONE HOP AND NOT PROVENANCE, and the difference is the whole of what
// it does not see. The local has to be bound from the RECEIVER itself -- a call on it, a field of
// it, or a walk over one of those -- and a local taken out of THAT local is outside the class
// again. Measured on 2026-09-05 at the site this widening was written for: delete the
// zeroizeSecret in front of RestoreSenderRatchets' assignment and the gate below names it, at
// fourteen sites; delete it and write the same assignment as `holder := r` followed by
// `holder.secret = ...`, which is the same pointer and the same store, and the gate passes and
// the whole of ./mls/... and ./message/... comes back 7,485 passing. Taking the reading to the
// fixpoint -- a local out of a tracked local is tracked -- is what would close it, and until
// somebody does, mls/UNOBSERVED.md carries it as a hole rather than this comment carrying it as a
// property.
func declarationsHolding(files []parsedSource, typeName string) []eraseMethod {
	held := []eraseMethod{}
	structs := map[string]*ast.StructType{}
	answers := map[string][][]ast.Expr{}
	for _, parsed := range files {
		structTypesIn(parsed, structs)
		for _, declaration := range parsed.file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Type.Results == nil {
				continue
			}
			results := []ast.Expr{}
			for _, result := range function.Type.Results.List {
				for repeat := 0; repeat < max(len(result.Names), 1); repeat += 1 {
					results = append(results, result.Type)
				}
			}
			answers[function.Name.Name] = append(answers[function.Name.Name], results)
		}
	}
	for _, parsed := range files {
		for _, declaration := range parsed.file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			positions := []*ast.Field{}
			receiver, receiverType := "", ""
			if function.Recv != nil && len(function.Recv.List) == 1 {
				receiverType = receiverTypeName(function.Recv.List[0].Type)
				if len(function.Recv.List[0].Names) == 1 {
					receiver = function.Recv.List[0].Names[0].Name
				}
				if receiverType == typeName {
					positions = append(positions, function.Recv.List[0])
				}
			}
			if function.Type.Params != nil {
				for _, parameter := range function.Type.Params.List {
					if slices.Contains(identifiersNamedIn(parameter.Type), typeName) {
						positions = append(positions, parameter)
					}
				}
			}
			for _, position := range positions {
				for _, name := range position.Names {
					if name.Name == "_" {
						continue
					}
					held = append(held, eraseMethod{
						receiver: typeName, name: function.Name.Name, self: name.Name,
						parsed: parsed, decl: function,
					})
				}
			}
			for _, name := range localsTakenOutOfTheReceiver(function, receiver, receiverType,
				typeName, structs, answers) {

				held = append(held, eraseMethod{
					receiver: typeName, name: function.Name.Name, self: name,
					parsed: parsed, decl: function,
				})
			}
		}
	}
	return held
}

// localsTakenOutOfTheReceiver is every local of one body bound from the receiver's own storage
// whose type is the one being asked about.
//
// THREE SHAPES, AND THEY ARE THE THREE WAYS STORAGE LEAVES A RECEIVER: a method of it answering
// the value, a field of it read directly, and a walk over one of those. Each is read off the
// syntax, and the TYPE is read off the declaration the value came from -- a method's result at the
// position the local binds, a field's declared type, or the element type of what is walked --
// rather than guessed from the name, because a reading that guessed would sweep unrelated locals
// into the class and the class would then demand erases nothing owes.
func localsTakenOutOfTheReceiver(function *ast.FuncDecl, receiver string, receiverType string,
	typeName string, structs map[string]*ast.StructType, answers map[string][][]ast.Expr) []string {

	if receiver == "" || receiver == "_" {
		return nil
	}
	names := []string{}
	seen := map[string]bool{}
	take := func(target ast.Expr) {
		bare, isBare := target.(*ast.Ident)
		if !isBare || bare.Name == "_" || bare.Name == receiver || seen[bare.Name] {
			return
		}
		seen[bare.Name] = true
		names = append(names, bare.Name)
	}
	holds := func(typed ast.Expr) bool {
		return typed != nil && slices.Contains(identifiersNamedIn(typed), typeName)
	}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.AssignStmt:
			if len(typed.Rhs) == 1 {
				for at, produced := range eraseReceiverSourcedTypes(typed.Rhs[0], receiver,
					receiverType, structs, answers, len(typed.Lhs)) {

					if at < len(typed.Lhs) && holds(produced) {
						take(typed.Lhs[at])
					}
				}
				return true
			}
			if len(typed.Lhs) != len(typed.Rhs) {
				return true
			}
			for i, right := range typed.Rhs {
				produced := eraseReceiverSourcedTypes(right, receiver, receiverType, structs,
					answers, 1)
				if len(produced) == 1 && holds(produced[0]) {
					take(typed.Lhs[i])
				}
			}
		case *ast.RangeStmt:
			// the VALUE of a range is the element; the key is an index or a map key, which is
			// eraseAliasesIn's own reading of the same statement.
			if typed.Value == nil {
				return true
			}
			for _, over := range eraseReceiverSourcedTypes(typed.X, receiver, receiverType,
				structs, answers, 1) {

				if holds(eraseElementTypeOf(over)) {
					take(typed.Value)
				}
			}
		}
		return true
	})
	return names
}

// eraseReceiverSourcedTypes is the declared type of what an expression rooted in the receiver
// produces, one entry per result position. An expression rooted anywhere else answers nothing,
// which is what keeps a local this body constructed out of the class.
func eraseReceiverSourcedTypes(expr ast.Expr, receiver string, receiverType string,
	structs map[string]*ast.StructType, answers map[string][][]ast.Expr, want int) []ast.Expr {

	switch typed := expr.(type) {
	case *ast.ParenExpr:
		return eraseReceiverSourcedTypes(typed.X, receiver, receiverType, structs, answers, want)
	case *ast.StarExpr:
		return eraseReceiverSourcedTypes(typed.X, receiver, receiverType, structs, answers, want)
	case *ast.CallExpr:
		selector, isSelector := typed.Fun.(*ast.SelectorExpr)
		if !isSelector || selector.Sel == nil || rootIdentifierOf(selector.X) != receiver {
			return nil
		}
		// NAME AND ARITY, AND NOT THE RECEIVER'S TYPE. Every declaration of that name is tried and
		// the first whose RESULT COUNT matches is taken, so where two members share a method name
		// this answers for whichever declaration's file sorts first. Clone is declared on eight
		// types in this package and every one of them answers one result, so a Clone called on any
		// of them reads *GroupContext's, out of group_context.go. Measured on 2026-09-05: the same
		// two line drop of a live leaf private key placed inside (*TreeKEMPrivate).Consistent is
		// reported by the gate below when the local comes out of a uniquely named method, and NOT
		// reported when the only change is that it comes out of self.Clone() -- and the second
		// leaves the whole of ./mls/... and ./message/... at 7,485 passing. Clone is not the only
		// shared name that answers a result: Encode, Epoch, EpochAuthenticator, Export, LeafCount,
		// Members, Validate, MarshalMLS and UnmarshalMLS are each declared by more than one
		// production type, and each of them binding a local is resolved the same way. Zeroize is
		// declared eight times as well and answers nothing, so it binds no local and is not a hazard
		// here.
		//
		// AND THIS IS WHY A BARE NAME IS SAFE NEXT DOOR AND NOT HERE. labelledDeclarationsIn and
		// newCommitSourceReader index by bare name too and say so, and both WALK EVERY match, so
		// their over-reach costs work rather than accuracy. This one RETURNS THE FIRST, which is a
		// choice between declarations rather than a union of them, and a choice made on arity alone
		// can be the wrong one. The hole is carried in mls/UNOBSERVED.md; resolving the receiver
		// expression's own type and keying the lookup on (type, name) is what would close it.
		for _, results := range answers[selector.Sel.Name] {
			if len(results) == want {
				return results
			}
		}
		return nil
	case *ast.IndexExpr:
		for _, over := range eraseReceiverSourcedTypes(typed.X, receiver, receiverType, structs,
			answers, 1) {

			if element := eraseElementTypeOf(over); element != nil {
				return []ast.Expr{element}
			}
		}
		return nil
	case *ast.SelectorExpr:
		base, isBare := typed.X.(*ast.Ident)
		if !isBare || base.Name != receiver {
			return nil
		}
		structure, isDeclared := structs[receiverType]
		if !isDeclared {
			return nil
		}
		for _, field := range structure.Fields.List {
			for _, name := range field.Names {
				if name.Name == typed.Sel.Name {
					return []ast.Expr{field.Type}
				}
			}
		}
	}
	return nil
}

// eraseElementTypeOf is what one entry of a map or a slice holds.
func eraseElementTypeOf(expr ast.Expr) ast.Expr {
	switch typed := expr.(type) {
	case *ast.MapType:
		return typed.Value
	case *ast.ArrayType:
		return typed.Elt
	}
	return nil
}

// TestEveryPathThatDropsHeldKeyMaterialErasesItFirst is the half a completeness reading cannot
// see: an erase that exists and is not called at the site that drops the value.
//
// That is exactly what this package had. (*KeySchedule).Zeroize and (*SecretTree).Zeroize were
// both written, both complete, both held by their own gates -- and the staged epoch carrying one
// of each was dropped by two paths that called neither.
func TestEveryPathThatDropsHeldKeyMaterialErasesItFirst(t *testing.T) {
	reading := eraseReadingOf(t)
	sites := 0
	for _, member := range reading.members {
		if _, isExcused := typesTheEraseClassReachesThatOweNoErase[member]; isExcused {
			continue
		}
		for _, site := range theDropSitesIn(reading, member) {
			if _, isExcused := theFieldsOfTheEraseClassThatAreNotKeyMaterial[member+"."+site.field.name]; isExcused {
				continue
			}
			sites += 1
			if site.erased || site.refused || site.movedWhole {
				continue
			}
			held := []string{}
			for _, typeName := range site.field.types {
				for _, sub := range theFieldsReachingStorageOf(reading, typeName) {
					if _, isExcused := theFieldsOfTheEraseClassThatAreNotKeyMaterial[typeName+"."+sub.name]; isExcused {
						continue
					}
					held = append(held, sub.name)
				}
			}
			moved := []string{}
			for _, sub := range held {
				if site.installed[sub] || site.movedOut[sub] {
					continue
				}
				moved = append(moved, sub)
			}
			if len(held) != 0 && len(moved) == 0 {
				continue
			}
			t.Errorf("%s writes to %s.%s, which is storage of a %s it was handed or took out of its own receiver, and neither erases what it drops, refuses to overwrite a live one, nor moves it: %v is dropped with the value. A staged epoch dropped this way is a complete second epoch left in the heap for the collector to move around",
				site.method.name, site.method.self, site.field.name, member, moved)
		}
	}
	if sites < 4 {
		t.Fatalf("this reading found %d drop site(s); (*Group) alone closes, clears and merges a staged commit and swaps its own schedule, so a reading this thin is not finding the assignments it is stated over",
			sites)
	}
	t.Logf("%d drop site(s) of held key material, every one of them erased, refused or moved", sites)
}

// ---------------------------------------------------------------------------
// the control
// ---------------------------------------------------------------------------

// A package holding one of each shape the two readings must separate.
//
// Holder DECLARES an erase that misses one field; Mover moves a value out rather than erasing it;
// Dropper does neither. The reading has to report the second field of Holder and Dropper's
// assignment, and has to leave Mover alone -- a gate that demanded an erase there would be
// demanding that a merge erase the epoch it just entered.
const eraseObligationControl = `package control

type Held struct {
	secret []byte
	label  []byte
}

func (self *Held) Zeroize() {
	wipe(self.secret)
}

type Holder struct {
	held      *Held
	forgotten *Held
}

func (self *Holder) Zeroize() {
	self.held.Zeroize()
}

type Mover struct {
	pending *Holder
	held    *Held
}

func (self *Mover) merge() {
	staged := self.pending
	self.held = staged.held
	staged.forgotten.Zeroize()
	staged.held = nil
	self.pending = nil
}

// the three neighbours of the shell-making assignment in merge, each ONE thing short of a
// move: no install at all, the install written below the nil, and an install into a local
// that goes out of scope with the frame. Every one of them drops a live Held.
type Detacher struct {
	pending *Holder
}

func (self *Detacher) detach() {
	staged := self.pending
	staged.held = nil
}

type Lagger struct {
	pending *Holder
	held    *Held
}

func (self *Lagger) lag() {
	staged := self.pending
	staged.held = nil
	self.held = staged.held
}

type Leaker struct {
	pending *Holder
}

func (self *Leaker) leak() {
	staged := self.pending
	kept := staged.held
	staged.held = nil
	_ = kept
}

// and an install into storage of a value this frame BUILT, which is a local however many
// fields it has: nothing outside the frame holds fresh, so what is installed into it is
// dropped with it.
type Builder struct {
	pending *Holder
}

func (self *Builder) build() {
	staged := self.pending
	fresh := &Lagger{}
	fresh.held = staged.held
	staged.held = nil
}

type Dropper struct {
	pending *Holder
}

func (self *Dropper) drop() {
	self.pending = nil
}

// the same drop through a position a receiver-rooted reading has no name for.
func abandon(dropper *Dropper) {
	dropper.pending = nil
}

// the value a body took OUT OF THE RECEIVER and dropped through the local it came back in,
// which is the shape a reading of receiver and parameter positions alone has no name for. Keeper
// hands nothing to restore; restore asks Keeper for one of the values Keeper is holding and
// writes over its storage, which is (*SecretTree).RestoreSenderRatchets exactly.
type Keeper struct {
	entries map[int]*Held
}

func (self *Keeper) entryFor(at int) *Held {
	return self.entries[at]
}

func (self *Keeper) restore(at int, secret []byte) {
	entry := self.entryFor(at)
	entry.secret = secret
}

// and the local this body CONSTRUCTED, which is not a position however much storage it holds:
// the value never left this frame, so the assignment below drops a nil and not a live secret.
// This is the case the old boundary was written for and it stays outside the class.
func mint(secret []byte) *Held {
	fresh := &Held{}
	fresh.secret = secret
	return fresh
}

type Refuser struct {
	pending *Holder
}

func (self *Refuser) stage(next *Holder) error {
	if self.pending != nil {
		return errPending
	}
	self.pending = next
	return nil
}

// a type holding KEY MATERIAL and declaring no erase, which nothing else in this package holds.
// It is the shape the class was seeded to miss: the upward closure never reaches it, and the seed
// was the presence of a Zeroize. It is what task 19's past-epoch window looks like on the day it
// is written, and both halves of "key material" are here -- a raw byte slice and one of this
// source's named private key types.
type HpkePrivateKey []byte

type Unerased struct {
	initSecret     []byte
	encryptionPriv HpkePrivateKey
}

// a WIRE type holding a private key its encoding never writes, which is the shape the class seed
// used to step over. "Everything a wire type holds is about to be octets" is true of every field
// the encoding writes and of no other, and the counterexample in the real source is
// KeyPackage.signPriv -- the seed the leaf's signing key was minted from, which marshalCore stops
// above. A reading taken at the TYPE excuses this whole structure; one taken at the field excuses
// Body and asks Published for an erase of priv.
type Published struct {
	Body []byte
	priv HpkePrivateKey
}

func (self *Published) MarshalMLS(w *Writer) error {
	w.WriteOpaque(self.Body)
	return nil
}

func (self *Published) UnmarshalMLS(r *Reader) error {
	*self = Published{}
	return nil
}
// the presence guard, which is the same token pointing the other way: it returns when there
// is NOTHING to drop and drops a live value every time it is reached.
type Presumer struct {
	pending *Holder
}

func (self *Presumer) release() {
	if self.pending == nil {
		return
	}
	self.pending = nil
}

// the refusing comparison written AFTER the drop. It is the same token and the same direction as
// Refuser's and it excuses nothing, because by the time it is reached the live value is gone: the
// assignment above it dropped a whole staged epoch and the branch below can only ever see what
// was just installed.
type Trailer struct {
	pending *Holder
}

func (self *Trailer) release(next *Holder) error {
	self.pending = next
	if self.pending != nil {
		return errPending
	}
	return nil
}

// and the erase of ONE PART of the value being dropped, which is not an erase of the value:
// held stays in the heap. The call resolves to the root field and the root's own type
// declares a method of that name, which is the whole of why this read as an erase.
type SubFielder struct {
	pending *Holder
}

func (self *SubFielder) release() {
	self.pending.forgotten.Zeroize()
	self.pending = nil
}

func wipe(secret []byte) {
	for i := range secret {
		secret[i] = 0
	}
}
`

// eraseControlReading runs both readings over the control, so a reading narrowed or widened by an
// edit fails here rather than reporting the real source clean.
func eraseControlReading(t *testing.T) {
	t.Helper()
	control := mustParseText(t, "erase_obligation_control.go", eraseObligationControl)
	reading := eraseSourceReading{
		files:   []parsedSource{control},
		structs: map[string]*ast.StructType{},
	}
	structTypesIn(control, reading.structs)
	reading.named = packageByteSliceTypeNamesIn(control)
	reading.wire = theTypesThisSourceSerializes(reading.files, reading.structs)
	reading.encoded = theFieldsEachWireTypeEncodes(reading.files, reading.structs, reading.wire)
	reading.helpers = []string{"wipe"}
	reading.erasers = eraseMethodsIn(reading)
	reading.members = eraseClassOf(reading)

	for _, want := range []string{"Held", "Holder", "Mover", "Dropper", "Refuser",
		"Presumer", "Published", "SubFielder", "Trailer", "Unerased"} {
		if !slices.Contains(reading.members, want) {
			t.Errorf("the erase class read %v out of the control and %s is not in it; a type that declares an erase, or that holds one that does, is a member by doing so",
				reading.members, want)
		}
	}
	if !reading.erasers["Holder"]["Zeroize"] {
		t.Error("(*Holder).Zeroize is not read as an erase, and it hands a field to an erase of that field's own type; the holder half of the reading is doing nothing")
	}
	if reading.erasers["Dropper"]["drop"] {
		t.Error("(*Dropper).drop is read as an erase and it erases nothing")
	}
	// the seed, asked on its own: a type that declares no erase and that nothing holds is in the
	// class by HOLDING KEY MATERIAL, or the class is seeded on the declaration again and a new
	// production type carrying an epoch's secrets is invisible to both readings below.
	if len(reading.erasers["Unerased"]) != 0 {
		t.Errorf("(*Unerased) is read as declaring an erase -- %v -- so its membership says nothing about the seed",
			slices.Sorted(maps.Keys(reading.erasers["Unerased"])))
	}
	if held := theFieldsReachingStorageOf(reading, "Unerased"); len(held) != 2 {
		t.Errorf("the field reading found %d field(s) of Unerased reaching octets, want 2; a raw byte slice and a named private key type are the two spellings key material arrives in",
			len(held))
	}
	// the wire reading, asked PER FIELD. Published declares a codec, so a reading taken at the
	// type calls its private key published and the seed steps over the whole structure; one taken
	// at the field credits the octets its MarshalMLS writes and nothing else.
	if !reading.wire["Published"] {
		t.Error("Published is not read as a type this source puts on the wire, and it declares both codec methods; the per-field reading below is then over nothing")
	}
	if !reading.encoded["Published"]["Body"] {
		t.Error("the encoding reading does not credit Published.Body, which Published's own MarshalMLS writes; a reading that credits nothing makes every field of every wire type key material and the class it answers is noise")
	}
	if reading.encoded["Published"]["priv"] {
		t.Error("the encoding reading credits Published.priv, which no declaration handed a Writer ever writes; a wire type excused wholesale again is a private key sitting outside the erase class, which is what KeyPackage.signPriv did")
	}

	// the completeness half: Holder erases held and not forgotten.
	fields := theFieldsReachingStorageOf(reading, "Holder")
	if len(fields) != 2 {
		t.Fatalf("the field reading found %d field(s) of Holder reaching octets, want 2; it is not following the struct the type holds",
			len(fields))
	}
	holder := []eraseMethod{}
	for _, method := range methodsWithNoParametersIn(reading.files) {
		if method.receiver == "Holder" {
			holder = append(holder, method)
		}
	}
	if len(holder) != 1 {
		t.Fatalf("the control declares %d no-argument method(s) on Holder, want 1", len(holder))
	}
	erased := eraseWholeFieldsOf(reading, fields, theFieldsErasedBy(reading, holder[0], fields))
	if !erased["held"] || erased["forgotten"] {
		t.Errorf("(*Holder).Zeroize is read as erasing %v, want held alone; the erase reading is not telling a call from a field it never names",
			slices.Sorted(maps.Keys(erased)))
	}

	// and the drop sites: Dropper is reported, Mover and Refuser are not.
	verdicts := map[string]eraseDropSite{}
	for _, member := range []string{"Mover", "Dropper", "Refuser", "Presumer", "SubFielder",
		"Trailer", "Held", "Holder"} {
		for _, site := range theDropSitesIn(reading, member) {
			verdicts[member+"."+site.method.name] = site
		}
	}
	if _, found := verdicts["Dropper.abandon"]; !found {
		t.Errorf("the drop reading found no assignment in abandon, which is handed a Dropper in a parameter and drops its held value; a reading rooted in the receiver asks this obligation of methods and of nothing else")
	}
	// the widened position: a local the body took out of its own receiver.
	keeper, found := verdicts["Held.restore"]
	if !found {
		t.Error("the drop reading found no assignment in (*Keeper).restore, which writes over the storage of a value it asked its own receiver for; a reading that sees only receivers and parameters reports this source clean while a live secret is dropped through a local")
	} else if keeper.erased || keeper.refused || len(keeper.installed) != 0 || len(keeper.movedOut) != 0 {
		t.Errorf("(*Keeper).restore is read as erasing, refusing or moving what it drops: erased=%v refused=%v",
			keeper.erased, keeper.refused)
	}
	// and the local this body CONSTRUCTED, which the widening must not sweep in.
	if _, found := verdicts["Held.mint"]; found {
		t.Error("the drop reading reports mint, which writes over storage of a value it built two statements earlier; a reading that cannot tell a constructed local from one taken out of a receiver reports every constructor in this package as a leak")
	}
	drop, found := verdicts["Dropper.drop"]
	if !found {
		t.Fatal("the drop reading found no assignment in (*Dropper).drop, so the rule below is over nothing")
	}
	if drop.erased || drop.refused || len(drop.installed) != 0 || len(drop.movedOut) != 0 {
		t.Errorf("(*Dropper).drop is read as erasing, refusing or moving what it drops: erased=%v refused=%v installed=%v moved=%v",
			drop.erased, drop.refused, slices.Sorted(maps.Keys(drop.installed)), slices.Sorted(maps.Keys(drop.movedOut)))
	}
	move, found := verdicts["Mover.merge"]
	if !found {
		t.Fatal("the drop reading found no assignment to pending in (*Mover).merge")
	}
	if !move.installed["held"] || !move.movedOut["forgotten"] {
		t.Errorf("(*Mover).merge is read as installing %v and erasing %v out of the value it drops, want held installed and forgotten erased; a merge whose move goes unread is a merge this gate would demand erase the epoch it just entered",
			slices.Sorted(maps.Keys(move.installed)), slices.Sorted(maps.Keys(move.movedOut)))
	}
	// the WHOLE-VALUE move: merge reads staged.held out into its receiver's storage and then
	// writes nil over the field, and that assignment makes the shell rather than dropping
	// anything. Its three neighbours are each one thing short of it and each is a drop.
	shell, found := verdicts["Holder.merge"]
	if !found {
		t.Fatal("the drop reading found no assignment to held in (*Mover).merge through the local it took out of its receiver, so the whole-value move rule below is over nothing")
	}
	if !shell.movedWhole {
		t.Error("(*Mover).merge writes nil over staged.held AFTER installing staged.held into self.held, and the reading does not call that a move; a merge that cannot leave a shell leaves the caller's value naming the epoch it just entered")
	}
	for name, why := range map[string]string{
		"Holder.detach": "nils the field and installs it nowhere",
		"Holder.lag":    "installs the field BELOW the nil, so what it installs is the nil",
		"Holder.leak":   "reads the field into a local, which is not a position: the value goes out of scope with the frame",
		"Holder.build":  "installs the field into storage of a value the frame built, which is a local however many fields it has",
	} {
		site, found := verdicts[name]
		if !found {
			t.Errorf("the drop reading found no assignment for %s, which %s", name, why)
			continue
		}
		if site.movedWhole || site.erased || site.refused || len(site.installed) != 0 || len(site.movedOut) != 0 {
			t.Errorf("%s is read as moving, erasing or refusing what it drops -- movedWhole=%v erased=%v refused=%v -- and it %s", name, site.movedWhole, site.erased, site.refused, why)
		}
	}
	presume, found := verdicts["Presumer.release"]
	if !found {
		t.Fatal("the drop reading found no assignment to pending in (*Presumer).release")
	}
	if presume.refused || presume.erased || len(presume.installed) != 0 || len(presume.movedOut) != 0 {
		t.Errorf("(*Presumer).release is read as refusing, erasing or moving what it drops -- refused=%v erased=%v -- and its guard RETURNS when there is nothing to drop; a presence guard is the opposite of a refusal and its assignment lands on a live value every time it is reached",
			presume.refused, presume.erased)
	}
	subField, found := verdicts["SubFielder.release"]
	if !found {
		t.Fatal("the drop reading found no assignment to pending in (*SubFielder).release")
	}
	if subField.erased {
		t.Error("(*SubFielder).release is read as having erased what it drops, and it erased ONE PART of it; an erase resolving to the root field certifies every other part of the held value along with it")
	}
	if !subField.movedOut["forgotten"] || subField.movedOut["held"] {
		t.Errorf("the erase in (*SubFielder).release is read as reaching %v of the value it drops, want forgotten alone",
			slices.Sorted(maps.Keys(subField.movedOut)))
	}
	refuse, found := verdicts["Refuser.stage"]
	if !found {
		t.Fatal("the drop reading found no assignment to pending in (*Refuser).stage")
	}
	if !refuse.refused {
		t.Error("(*Refuser).stage is not read as refusing to overwrite a live value, and it returns on exactly that comparison; without it every constructor of a staged value is a drop site")
	}
	trail, found := verdicts["Trailer.release"]
	if !found {
		t.Fatal("the drop reading found no assignment to pending in (*Trailer).release")
	}
	if trail.refused || trail.erased {
		t.Errorf("(*Trailer).release is read as refusing or erasing what it drops -- refused=%v erased=%v -- and both its comparison and any erase stand BELOW the assignment; an excuse written under a drop excuses a value that was already gone when it was reached",
			trail.refused, trail.erased)
	}
}
