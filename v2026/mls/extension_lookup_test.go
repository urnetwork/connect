// The one rule this package has about a repeated extension type, and the derived class it is
// stated over.
//
// The rule: extensions<V> is a vector and the wire permits two entries of one type, RFC 9420
// forbids it, and NOTHING in this build refuses it except the lookup. ValSem209 is named by the
// validation plan and implemented nowhere; LeafNode.Validate -- the door three comments used to
// hand the refusal to -- walks every entry and range checks each, which is a different rule and
// accepts a leaf carrying two well formed entries on purpose. So FindExtensionEntry refuses, once,
// for every caller.
//
// Why this file exists rather than three assertions beside the three accessors. Before it, one
// branch held BOTH positions at once: two accessors refused a repeat and said in as many words
// that they did so because ValSem209 does not exist, while two lookups and a group context
// reconciliation answered by position and said a repeat would be refused by ValSem209. Both were
// committed together, and the test that pinned first-wins argued in its failure text that
// refusing "hides the input ValSem209 is stated over". Nothing derived the class, so the way that
// contradiction was found was by reading five comments -- and a FOURTH accessor, of which p7 has
// several still to write, would have landed on whichever side its author read first.
//
// So the class is derived from what a declaration DOES, not from a list of the ones that exist:
// every declaration that reads an extension's TYPE off something other than an Extension it was
// handed whole is reading one out of a vector, and every one of those has to be classified here.
// The classification is a table, and the table is held EQUAL to the derived class in both
// directions, which is what stops it being the enumeration rule 5 forbids: a declaration with no
// row fails rather than going unclassified, and a row for a declaration that no longer exists
// fails rather than outliving it.
//
// The rule is stated over the READ and not over the loop deliberately. A lookup written with
// slices.IndexFunc and a closure carries no for statement at all, and a rule that recognised the
// loop would report a clean run over it -- the same understatement this project has been walked
// past fourteen times, one level down.
package mls

import (
	"bytes"
	"errors"
	"go/ast"
	"go/types"
	"maps"
	"slices"
	"testing"
)

// The three names the derivation is written against, read out of the compiler's types rather
// than matched as text: the struct one entry is, the field that carries its type, and the type
// that field has. All three are this package's own declarations, and a control below declares
// its own copies so the matcher is proven to read a package it has never seen.
const (
	extensionStructTypeName = "Extension"
	extensionTypeFieldName  = "ExtensionType"
	extensionTypeTypeName   = "ExtensionType"
)

// extensionTypeSelectionRoots is where the rule is stated: it IS forbiddenScanRoots, not a copy
// of it. The scope is wider than the place the demand comes from on purpose -- mls must not
// import message, so message is where a second reader of a group context extension can be
// written without any gate of this package noticing -- and ../message declares nothing of the
// kind today, which is not a reason to leave it out: a scope that covers only what is already
// written is a scope that stops covering the first thing added.
//
// It is an alias rather than a restatement because a restatement is not held by anything. This
// was measured: written as []string{".", messagePackageDir}, narrowing it to []string{"."}
// left the whole of ./mls/... ./message/... green, so the paragraph above was an argument no
// test could lose. Narrowing forbiddenScanRoots itself FAILS TestHkdfExtractHasOnlyTwoCallSites,
// which is G1 and predates this gate -- so borrowing that value borrows a scope something
// already pins. Deriving the class and then writing the scope down beside it is the defect
// ledger 21 names, and this gate was written to fix an instance of it.
var extensionTypeSelectionRoots = forbiddenScanRoots

// extensionTypeSelectionNamedAs answers whether the compiler reads a type as the named type
// spelled name, through any number of pointers.
//
// The compiler's reading and not the source's, so an alias, a type declared in another package
// and referred to as mls.Extension, and a value reached through a pointer all answer the same.
func extensionTypeSelectionNamedAs(found types.Type, name string) bool {
	for {
		if found == nil {
			return false
		}
		pointer, isPointer := found.(*types.Pointer)
		if !isPointer {
			break
		}
		found = pointer.Elem()
	}
	named, isNamed := found.(*types.Named)
	return isNamed && named.Obj() != nil && named.Obj().Name() == name
}

// extensionTypeSelectionUnparenthesised strips the parentheses a selector's base may be written
// with, because (exts[i]).ExtensionType and exts[i].ExtensionType are the same read.
func extensionTypeSelectionUnparenthesised(expr ast.Expr) ast.Expr {
	for {
		parens, isParens := expr.(*ast.ParenExpr)
		if !isParens {
			return expr
		}
		expr = parens.X
	}
}

// extensionTypeSelectionDeclarationName names one declaration the way the tables of this package
// name declarations: a plain function by its name, a method as (*T).Name or T.Name.
func extensionTypeSelectionDeclarationName(checked checkedBodies, function *ast.FuncDecl) string {
	if function.Recv == nil || len(function.Recv.List) != 1 {
		return function.Name.Name
	}
	receiver := checked.render(function.Recv.List[0].Type)
	if len(receiver) > 0 && receiver[0] == '*' {
		receiver = "(" + receiver + ")"
	}
	return receiver + "." + function.Name.Name
}

// The objects a declaration was HANDED: its receiver and its parameters.
//
// Results are not among them, and neither is any local. A value a declaration was handed is one
// its caller chose; a value it read out of a slice, bound with a range clause, assigned to a
// local or took as a function literal's parameter is one it selected for itself, and selecting
// is the thing this rule is about. The distinction is drawn over the type checker's OBJECTS
// rather than over names, so a local shadowing a parameter is a different object and is not
// mistaken for the parameter.
func extensionTypeSelectionHandedTo(checked checkedBodies, function *ast.FuncDecl) map[types.Object]bool {
	handed := map[types.Object]bool{}
	fields := []*ast.Field{}
	if function.Recv != nil {
		fields = append(fields, function.Recv.List...)
	}
	if function.Type.Params != nil {
		fields = append(fields, function.Type.Params.List...)
	}
	for _, field := range fields {
		for _, name := range field.Names {
			if object := checked.info.Defs[name]; object != nil {
				handed[object] = true
			}
		}
	}
	return handed
}

// What one scan read: the declarations that select, and how many extension type reads were seen
// at all.
//
// The read count is carried for the reason packageLevelScan carries its file list. A matcher
// that stopped resolving its subject reports an EMPTY set of selections, and an empty set
// satisfies a table equality gate whose table was emptied with it -- so "nothing selects" and
// "nothing was read" have to be distinguishable, and only the second is a broken gate.
type extensionTypeSelectionScan struct {
	selecting map[string]string
	reads     int
}

// extensionTypeSelectionsIn is the rule: every declaration of one checked package that reads an
// extension's TYPE off something other than an Extension it was handed whole.
//
// Stated over the READ rather than over the comparison, and over the comparison's absence rather
// than its shape, because neither is where the class lives. A lookup that assigns
// exts[i].ExtensionType to a local and compares the local later is the same lookup; one written
// with slices.IndexFunc and a closure has no loop at all; one that switches on the value makes no
// binary comparison. All three read an extension's type out of a vector, and that read is the
// step none of them can be written without.
//
// The exemption is narrow on purpose: only a direct field read off an identifier the declaration
// was handed. A function literal's parameter is not the declaration's, so the closure a
// slices.IndexFunc lookup is written with is reported; a local assigned from a parameter is not
// the parameter, so a tag check laundered through one is reported too. That last one is a false
// positive and it is the safe direction -- it costs a row in the table saying what it is, and the
// table is read by a human, while the other direction costs a lookup nobody classified.
func extensionTypeSelectionsIn(checked checkedBodies) extensionTypeSelectionScan {
	scan := extensionTypeSelectionScan{selecting: map[string]string{}}
	for _, file := range checked.files {
		for _, declaration := range file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			handed := extensionTypeSelectionHandedTo(checked, function)
			name := extensionTypeSelectionDeclarationName(checked, function)
			ast.Inspect(function.Body, func(node ast.Node) bool {
				selector, isSelector := node.(*ast.SelectorExpr)
				if !isSelector || selector.Sel.Name != extensionTypeFieldName {
					return true
				}
				if !extensionTypeSelectionNamedAs(checked.info.TypeOf(selector), extensionTypeTypeName) ||
					!extensionTypeSelectionNamedAs(checked.info.TypeOf(selector.X), extensionStructTypeName) {
					return true
				}
				scan.reads++
				if base, isIdent := extensionTypeSelectionUnparenthesised(selector.X).(*ast.Ident); isIdent {
					if object := checked.info.Uses[base]; object != nil && handed[object] {
						// the whole entry was handed to this declaration by its caller: a tag
						// check, which selects nothing
						return true
					}
				}
				if _, already := scan.selecting[name]; !already {
					scan.selecting[name] = checked.where(selector)
				}
				return true
			})
		}
	}
	return scan
}

// extensionTypeSelectionControl is a package the matcher has never seen, declaring its own
// Extension and ExtensionType, written so that every half of the rule has something to report
// and something to walk past.
//
// A control rather than a second opinion about the real source: a matcher that resolved nothing
// reports an empty set over mls too, and an empty set is exactly what a package with no lookups
// would produce. The only way to tell the two apart is to run the matcher on source known to
// hold both answers.
const extensionTypeSelectionControl = `package control

import "slices"

type ExtensionType uint16

type Extension struct {
	ExtensionType ExtensionType
	ExtensionData []byte
}

type Holder struct {
	Extensions []Extension
}

const ExtensionTypeSomething ExtensionType = 0xF00D

// handed the whole entry by its caller: a tag check, and not a selection
func ParseSomethingFrom(ext Extension) []byte {
	if ext.ExtensionType != ExtensionTypeSomething {
		return nil
	}
	return ext.ExtensionData
}

// the receiver is handed too
func (self Extension) IsSomething() bool {
	return self.ExtensionType == ExtensionTypeSomething
}

// the fourth accessor, hand rolling the walk the package has one door for
func SomethingOf(exts []Extension) []byte {
	for i := range exts {
		if exts[i].ExtensionType == ExtensionTypeSomething {
			return exts[i].ExtensionData
		}
	}
	return nil
}

// the same lookup with no for statement anywhere in it, which is why the rule is stated over the
// read and not over the loop
func SomethingElseOf(exts []Extension) []byte {
	at := slices.IndexFunc(exts, func(e Extension) bool {
		return e.ExtensionType == ExtensionTypeSomething
	})
	if at < 0 {
		return nil
	}
	return exts[at].ExtensionData
}

// the range form, whose subject is a loop variable and not a parameter, and which makes no
// comparison of its own at all
func (self *Holder) ThirdOf() map[ExtensionType][]byte {
	out := map[ExtensionType][]byte{}
	for _, ext := range self.Extensions {
		out[ext.ExtensionType] = ext.ExtensionData
	}
	return out
}
`

// What the matcher must report out of the control, and by omission what it must walk past.
var extensionTypeSelectionControlReports = []string{"(*Holder).ThirdOf", "SomethingElseOf", "SomethingOf"}

// One classified member of the derived class.
//
// The prose is what a reader gets; refusesTheRepeat is the package's position stated as a
// property of the set rather than of any one member -- exactly one declaration may own the
// refusal, because two doors is two rules and neither would be reached by every caller; and the
// probe is what stops the row being a label. A row asserting that a declaration visits every
// entry, on a declaration that stops at the first, has to fail.
type extensionTypeSelection struct {
	what             string
	refusesTheRepeat bool
	probe            func(t *testing.T)
}

// Every declaration of this package and of ../message that reads an extension's type out of a
// vector, with what it does about a repeated type.
//
// Held EQUAL to the derived class in both directions by the gate below, so this is a
// classification and not a list: the commit that writes a fourth accessor either routes it
// through FindExtensionEntry -- in which case it reads no extension type of its own and is not
// in the class at all -- or hand rolls a walk and fails here until somebody writes down which of
// the two things it does.
var extensionTypeSelectionsOfBothPackages = map[string]extensionTypeSelection{
	"FindExtensionEntry": {
		what: "the package's ONE lookup of an extension by type, and so the one door that refuses a repeated one. " +
			"RFC 9420 forbids two entries of a type and nothing else in this build refuses it: ValSem209 is not " +
			"implemented, and LeafNode.Validate accepts a leaf carrying two well formed entries on purpose. Every " +
			"accessor reaches an entry through here, so the refusal reaches every caller without any of them " +
			"restating it",
		refusesTheRepeat: true,
		probe: func(t *testing.T) {
			entry := Extension{ExtensionType: ExtensionTypeUrmessageGroupPolicy, ExtensionData: []byte{0x01}}
			second := Extension{ExtensionType: ExtensionTypeUrmessageGroupPolicy, ExtensionData: []byte{0x02}}
			other := Extension{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: []byte{0x03}}
			got, found, err := FindExtensionEntry([]Extension{other, entry}, entry.ExtensionType)
			if err != nil || !found || !bytes.Equal(got.ExtensionData, entry.ExtensionData) {
				t.Fatalf("FindExtensionEntry over a vector holding the type once answered (%v, %v, %v), want the entry; the refusal below would otherwise be a lookup that answers nothing at all",
					got, found, err)
			}
			for _, held := range [][]Extension{
				{entry, second},
				{second, entry},
				{other, entry, other, second},
			} {
				answered, found, err := FindExtensionEntry(held, entry.ExtensionType)
				if !errors.Is(err, ErrMalformedExtension) || found || answered.ExtensionData != nil {
					t.Errorf("FindExtensionEntry over %v answered (%v, %v, %v), want ErrMalformedExtension and no entry; a lookup that answers one of two picks the group's wrap target, its role list and its required_capabilities by iteration order",
						held, answered, found, err)
				}
			}
		},
	},
	"NewGroup": {
		what: "reads the type of EVERY entry of the config's extensions vector and selects none. RFC 9420 " +
			"section 11 lets the creator choose the group's extensions, and this is where that choice is " +
			"held to the v1 profile's group context set, one entry at a time. A repeated type is not a " +
			"question it answers: two entries of one type are both judged and both must be admitted, and " +
			"the door that refuses the repeat is the lookup every accessor reaches -- which the creation " +
			"path reaches one statement later, through GroupPolicyOf. What matters here is the property " +
			"(*LeafNode).Validate's row names: the walk covers every entry, so an offending extension in " +
			"the MIDDLE or at the END is refused rather than walked past",
		refusesTheRepeat: false,
		probe: func(t *testing.T) {
			crypto := testCrypto(t)
			owner := testIdentity(t, crypto, "owner")
			// the offending entry LAST, which is what separates "every entry" from "the first"
			trailing := testGroupConfig(t, crypto, owner, "group-1")
			trailing.Extensions = append(trailing.Extensions,
				Extension{ExtensionType: ExtensionTypeUrmessageLeafKeys, ExtensionData: []byte{0x01}})
			if group, err := NewGroup(trailing, owner.SigPriv, BasicCredential(owner.IdentityPub)); err == nil {
				group.Close()
				t.Error("a config whose LAST extension is a leaf extension founded a group; the profile rule is being applied to the first entry alone")
			}
			// and the repeat is somebody else's rule. Both entries are admitted here, and what
			// refuses the group is the lookup GroupPolicyOf reaches one statement later.
			repeated := testGroupConfig(t, crypto, owner, "group-1")
			repeated.Extensions = append(repeated.Extensions, repeated.Extensions[0])
			group, err := NewGroup(repeated, owner.SigPriv, BasicCredential(owner.IdentityPub))
			if err == nil {
				group.Close()
				t.Fatal("a config carrying the group policy twice founded a group; the repeat reaches no door at all")
			}
			if !errors.Is(err, ErrMalformedExtension) {
				t.Errorf("a config carrying the group policy twice was refused with %v, want ErrMalformedExtension; the repeat is the lookup's refusal and not this loop's",
					err)
			}
		},
	},
	"JoinFromWelcome": {
		what: "reads the type of EVERY entry of the group context a Welcome describes and selects none, which " +
			"is NewGroup's row one epoch later and from the other side of the wire: the creator holds its own " +
			"choice to the v1 group context set, and a joiner holds a set somebody else chose to the same one. " +
			"A repeated type is not a question it answers -- two entries of one type are both judged and both " +
			"must be admitted -- and the door that refuses the repeat is the lookup every accessor reaches, " +
			"which this path reaches twice: requiredCapabilitiesOf before the walk and GroupPolicyOf after it. " +
			"What matters here is the property NewGroup's row names: the walk covers every entry, so an " +
			"offending extension in the MIDDLE or at the END is refused rather than walked past",
		refusesTheRepeat: false,
		probe: func(t *testing.T) {
			crypto := testCrypto(t)
			group, joiner, result, keys := joinTestCommit(t, crypto, "join-extension-walk", nil)
			defer group.Close()
			staged := group.stagedForTest()
			if staged == nil {
				t.Fatal("this fixture staged no commit, so there is no group context to describe")
			}
			// the offending entry LAST, which is what separates "every entry" from "the first".
			// urmessage_leaf_keys is a LEAF extension: every fixture leaf lists it in its
			// capabilities, so section 13.4's rule admits it and the only door that refuses it
			// is the profile walk this row is about.
			trailing := staged.context.Clone()
			trailing.Extensions = append(trailing.Extensions,
				Extension{ExtensionType: ExtensionTypeUrmessageLeafKeys, ExtensionData: []byte{0x01}})
			spec := joinTestWelcomeSpec{
				joinerSecret: staged.schedule.JoinerSecret(),
				context:      trailing,
				signer:       group.OwnLeafIndex(),
				signPriv:     group.signer,
				keyPackage:   &keys.KeyPackage,
				leafIndex:    LeafIndex(1),
			}
			if joined, err := JoinFromWelcome(testGroupConfig(t, crypto, joiner, "join-extension-walk"),
				joinTestSealWelcome(t, crypto, spec), result.RatchetTree, keys); err == nil {
				joined.Close()
				t.Error("a welcome whose LAST group context extension is a leaf extension was joined; the profile rule is being applied to the first entry alone")
			}
			// and the repeat is somebody else's rule. Both entries are admitted by the walk, and
			// what refuses the join is the lookup GroupPolicyOf reaches one statement later.
			repeated := staged.context.Clone()
			repeated.Extensions = append(repeated.Extensions, repeated.Extensions[len(repeated.Extensions)-1])
			spec.context = repeated
			joined, err := JoinFromWelcome(testGroupConfig(t, crypto, joiner, "join-extension-walk"),
				joinTestSealWelcome(t, crypto, spec), result.RatchetTree, keys)
			if err == nil {
				joined.Close()
				t.Fatal("a welcome whose group context carries the group policy twice was joined; the repeat reaches no door at all")
			}
			if !errors.Is(err, ErrMalformedExtension) {
				t.Errorf("a welcome whose group context carries the group policy twice was refused with %v, want ErrMalformedExtension; the repeat is the lookup's refusal and not this walk's",
					err)
			}
		},
	},
	"(*CommitValidationInput).checkExtensionsAreTheSetThisCommitInstalls": {
		what: "reads the type of EVERY entry of TWO vectors positionally and selects none. It is the commit " +
			"door's join between the extension set a caller announces and the one the commit installs, so a " +
			"repeated type is not a question it answers: two entries of one type in both vectors agree, and " +
			"the door that refuses the repeat is the lookup every accessor reaches. What matters here is the " +
			"property (*GroupContext).Clone's row names -- the walk is positional and covers every entry, so " +
			"a swapped pair or an altered body in the MIDDLE is a disagreement rather than a match",
		refusesTheRepeat: false,
		probe: func(t *testing.T) {
			crypto := testCrypto(t)
			tree, _ := testTreeWith(t, crypto, "alice", "bob")
			installed := []Extension{
				{ExtensionType: ExtensionTypeUrmessageGroupPolicy, ExtensionData: []byte{0x01}},
				{ExtensionType: ExtensionTypeRatchetTree, ExtensionData: []byte{0x0a}},
				{ExtensionType: ExtensionTypeUrmessageGroupPolicy, ExtensionData: []byte{0x02}},
			}
			in := testCommitInput(t, crypto, tree,
				testProposalList(t, testGceOf(installed...)), &Commit{})
			in.Extensions = slices.Clone(installed)
			if err := in.checkExtensionsAreTheSetThisCommitInstalls(); err != nil {
				t.Fatalf("two identical extension vectors, one type carried twice, were refused: %v; the repeat is the lookup's refusal and not this one, and every refusal below could then be this",
					err)
			}
			for _, one := range []struct {
				name  string
				entry Extension
			}{
				{"a type", Extension{ExtensionType: ExtensionTypeUrmessageOwnerSuccessor,
					ExtensionData: []byte{0x0a}}},
				{"a body", Extension{ExtensionType: ExtensionTypeRatchetTree,
					ExtensionData: []byte{0x0b}}},
			} {
				differing := slices.Clone(installed)
				differing[1] = one.entry
				in.Extensions = differing
				if err := in.checkExtensionsAreTheSetThisCommitInstalls(); !errors.Is(err, errCommitExtensionsNotApplied) {
					t.Errorf("%s disagreement in the middle of three entries answered %v, want errCommitExtensionsNotApplied; a comparison that stops at the first entry lets a swapped pair through and nothing behind it can see one",
						one.name, err)
				}
			}
		},
	},
	"cloneExtensions": {
		what: "reads the type of EVERY entry and writes each one into the copy, which is " +
			"(*GroupContext).Clone's row one file over and holds for the same reason: a leaf's extensions " +
			"vector must clone to the same entries in the same order, repeats included, or a leaf the lookup " +
			"refuses becomes one it answers. It selects nothing by type. The property this probe is really " +
			"for is the PAIRING -- the entry a body is taken from is the entry it is written into -- which is " +
			"what the index paired loop it replaced could get wrong on any leaf carrying more than one " +
			"extension, and what no fixture in this package could observe while every one of them carried one",
		refusesTheRepeat: false,
		probe: func(t *testing.T) {
			held := []Extension{
				{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: []byte{0x01}},
				{ExtensionType: ExtensionTypeUrmessageLeafKeys, ExtensionData: []byte{0x02, 0x02}},
				{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: []byte{0x03, 0x03, 0x03}},
			}
			clone := cloneExtensions(held)
			if len(clone) != len(held) {
				t.Fatalf("a vector carrying %d extensions, two of them one type, cloned to %d; a clone that drops a repeat turns a leaf the lookup refuses into one it answers",
					len(held), len(clone))
			}
			for i := range held {
				if clone[i].ExtensionType != held[i].ExtensionType ||
					!bytes.Equal(clone[i].ExtensionData, held[i].ExtensionData) {
					t.Errorf("entry %d cloned to %v, want %v; an entry carrying another entry's body is a leaf whose required_capabilities holds the urmessage_leaf_keys octets, correctly tagged and correctly length prefixed",
						i, clone[i], held[i])
				}
			}
			// the repeat survives the copy, so the lookup still refuses over the clone
			if _, _, err := FindExtensionEntry(clone, ExtensionTypeRequiredCapabilities); !errors.Is(err, ErrMalformedExtension) {
				t.Errorf("the clone of a vector carrying required_capabilities twice is answered by the lookup with %v, want ErrMalformedExtension",
					err)
			}
			// and every entry is a copy rather than an alias, entry one included: a walk that
			// checked entry zero alone is the one element fixture again
			for i := range clone {
				clone[i].ExtensionData[0] ^= 0xff
			}
			for i := range held {
				if bytes.Equal(clone[i].ExtensionData, held[i].ExtensionData) {
					t.Errorf("writing through the clone's entry %d changed the original's body, so the copy aliases the vector it copied",
						i)
				}
			}
			// nil clones to nil and an empty vector to an empty one, which is cloneBytes' rule at
			// the level of the vector: an absent extensions field and a present empty one are
			// different octets on the wire
			if cloneExtensions(nil) != nil {
				t.Error("a nil extensions vector cloned to a non nil one, which changes the encoding of the leaf it was asked to duplicate")
			}
			if empty := cloneExtensions([]Extension{}); empty == nil {
				t.Error("an empty extensions vector cloned to nil, which changes the encoding of the leaf it was asked to duplicate")
			}
		},
	},
	"(*GroupContext).Clone": {
		what: "reads the type of EVERY entry and writes each one into the copy. It selects none, and the " +
			"property that matters here is the one the rule reports it for: the clone must carry the same " +
			"entries in the same order, repeats included. A clone that collapsed two entries of one type -- " +
			"which is the tidy thing to do and is one line -- would make a group context the lookup refuses " +
			"become one it answers, and the epoch a member is still decrypting with would differ from the one " +
			"it was validated as. This row is the false positive the rule's exemption is deliberately too " +
			"narrow to drop, and it is worth the row",
		refusesTheRepeat: false,
		probe: func(t *testing.T) {
			held := &GroupContext{Extensions: []Extension{
				{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: []byte{0x01}},
				{ExtensionType: ExtensionTypeUrmessageGroupPolicy, ExtensionData: []byte{0x02}},
				{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: []byte{0x03}},
			}}
			clone := held.Clone()
			if len(clone.Extensions) != len(held.Extensions) {
				t.Fatalf("a context carrying %d extensions, two of them one type, cloned to %d; a clone that drops a repeat turns a context the lookup refuses into one it answers",
					len(held.Extensions), len(clone.Extensions))
			}
			for i := range held.Extensions {
				if clone.Extensions[i].ExtensionType != held.Extensions[i].ExtensionType ||
					!bytes.Equal(clone.Extensions[i].ExtensionData, held.Extensions[i].ExtensionData) {
					t.Errorf("entry %d cloned to %v, want %v; the order and the bodies are what the transcript hash covers",
						i, clone.Extensions[i], held.Extensions[i])
				}
			}
			// the repeat survives the copy, so the lookup still refuses over the clone
			if _, _, err := FindExtensionEntry(clone.Extensions, ExtensionTypeRequiredCapabilities); !errors.Is(err, ErrMalformedExtension) {
				t.Errorf("the clone of a context carrying required_capabilities twice is answered by the lookup with %v, want ErrMalformedExtension",
					err)
			}
			// and it is a copy rather than an alias, which is what the doc on Clone is for
			clone.Extensions[0].ExtensionData[0] ^= 0xff
			if bytes.Equal(clone.Extensions[0].ExtensionData, held.Extensions[0].ExtensionData) {
				t.Error("writing to the clone's first extension body changed the original's, so a rewritten epoch reaches back into the one a member is still decrypting with")
			}
		},
	},
	"(*LeafNode).Validate": {
		what: "walks every extension of a leaf and applies RFC 9420 section 7.3 to each. It SELECTS none -- the " +
			"walk exists precisely so the rule is not applied to element zero alone -- so the repeat is not its " +
			"rule and a leaf carrying two well formed urmessage_leaf_keys entries passes it, to be refused at the " +
			"lookup by whoever reads one. TestLeafNodeValidateRangeChecksEveryUrmessageLeafKeysEntry is the wide " +
			"statement of this; the probe here is the one row that separates a walk from a lookup",
		refusesTheRepeat: false,
		probe: func(t *testing.T) {
			crypto := leafValidationCrypto(t)
			good := leafValidationLeafKeysEntry(t)
			bad := Extension{ExtensionType: ExtensionTypeUrmessageLeafKeys, ExtensionData: []byte{}}
			// the second entry is the one that separates a walk from a lookup: a clause that
			// stopped at the first match would accept this leaf
			behind := leafValidationSignedLeaf(t, crypto, LeafNodeSourceCommit, func(leaf *LeafNode) {
				leaf.Extensions = []Extension{good, bad}
			})
			if err := behind.Validate(leafValidationContextFor(crypto, LeafNodeSourceCommit)); !errors.Is(err, ErrLeafKeysExtensionInvalid) {
				t.Errorf("a leaf whose SECOND urmessage_leaf_keys entry is malformed validated with %v, want ErrLeafKeysExtensionInvalid; a clause that stops at the first match carries the rest into an accepted leaf unlooked at",
					err)
			}
			// and the other half of "it selects nothing": two well formed entries are accepted
			// here, because refusing the repeat is the lookup's rule and not this one's
			both := leafValidationSignedLeaf(t, crypto, LeafNodeSourceCommit, func(leaf *LeafNode) {
				leaf.Extensions = []Extension{good, good}
			})
			if err := both.Validate(leafValidationContextFor(crypto, LeafNodeSourceCommit)); err != nil {
				t.Errorf("a leaf carrying two well formed urmessage_leaf_keys entries was refused here with %v; if this clause starts refusing the repeat then the package has two doors for one rule and this row is the wrong classification",
					err)
			}
			if _, err := LeafKeysOf(both); !errors.Is(err, ErrMalformedExtension) {
				t.Errorf("the leaf this clause accepted is answered by LeafKeysOf with %v, want ErrMalformedExtension; accepting the repeat here is only correct because the lookup refuses it",
					err)
			}
		},
	},
	"(*LeafNode).checkGroupContextExtensions": {
		what: "walks every extension of a GROUP CONTEXT and applies RFC 9420 section 13.4, as erratum 8745 " +
			"corrects it, to each. It SELECTS none -- the walk exists precisely so the rule is not applied to " +
			"element zero, and ERRATA.md records that narrowing it to element zero once passed the whole suite " +
			"because a real context is LED by the types section 7.2 exempts. So the repeat is not its rule: a " +
			"context carrying one type twice is judged twice here and refused, if at all, by whoever reads one " +
			"through the lookup",
		refusesTheRepeat: false,
		probe: func(t *testing.T) {
			crypto := testCrypto(t)
			leaf, _ := testLeafNode(t, crypto, testIdentity(t, crypto, "group-extension-walk"))
			narrowed := leaf.Clone()
			narrowed.Capabilities = testNarrowedCapabilities()
			// the offender BEHIND an entry section 7.2 exempts, which is the one arrangement
			// that separates a walk from a read of element zero
			exempt := Extension{ExtensionType: ExtensionTypeRequiredCapabilities}
			offender := Extension{ExtensionType: ExtensionTypeUrmessageOwnerSuccessor}
			if err := narrowed.checkGroupContextExtensions([]Extension{exempt, offender}); !errors.Is(err,
				errGroupContextExtensionNotListed) {
				t.Errorf("a leaf that does not list the SECOND group context extension was judged %v, want errGroupContextExtensionNotListed; a clause that stops at the first entry steps over the exempt one and never reaches the rule",
					err)
			}
			// and the other half of "it selects nothing": one type carried twice is accepted
			// here when the leaf lists it, because refusing the repeat is the lookup's rule
			if err := leaf.checkGroupContextExtensions([]Extension{offender, offender}); err != nil {
				t.Errorf("a group context carrying one type twice was refused here with %v; if this clause starts refusing the repeat then the package has two doors for one rule and this row is the wrong classification",
					err)
			}
		},
	},
	"(*Group).ProposeGroupContextExtensions": {
		what: "reads the type of EVERY entry of the extension set a caller wants installed and selects none. " +
			"It is NewGroup's row at the other end of a group's life -- section 12.1.6's proposal replaces the " +
			"group context's extensions wholesale, so the set is held to the v1 profile's group context set one " +
			"entry at a time, and it is the SENDING side of the gate ValSem209 applies to the same proposal on " +
			"arrival. A repeated type is not a question it answers: both entries are judged and both must be " +
			"admitted here, and the doors that refuse the repeat are the lookups GroupPolicyOf and " +
			"requiredCapabilitiesOf reach one statement later -- the same two ValSem209 reaches when it judges " +
			"the same proposal on arrival, which is what keeps this method from publishing a set its own " +
			"receiving side refuses. What matters is that the walk covers every entry, so an offending " +
			"extension in the MIDDLE or at the END is refused rather than walked past",
		refusesTheRepeat: false,
		probe: func(t *testing.T) {
			crypto := testCrypto(t)
			owner := testIdentity(t, crypto, "owner")
			group := testNewGroup(t, crypto, owner, "group-1")
			defer group.Close()
			published := testGroupContextOf(t, group).Extensions
			if len(published) == 0 {
				t.Fatal("the group publishes no extensions, so neither probe below carries an admitted entry")
			}
			// the offending entry LAST, which is what separates "every entry" from "the first".
			// urmessage_leaf_keys is a LEAF extension, so the v1 profile refuses it in a group
			// context whichever position it sits in.
			trailing := append(slices.Clone(published),
				Extension{ExtensionType: ExtensionTypeUrmessageLeafKeys, ExtensionData: []byte{0x01}})
			if _, err := group.ProposeGroupContextExtensions(trailing); !errors.Is(err, errProfileGroupExtension) {
				t.Errorf("an extension set whose LAST entry is a leaf extension was proposed with %v, want errProfileGroupExtension; the profile rule is being applied to the first entry alone",
					err)
			}
			// and the repeat is somebody else's rule. Both copies are admitted by the walk, and
			// what refuses the proposal is a lookup one statement later.
			//
			// EVERY entry this group publishes is repeated in turn rather than the first one,
			// which is the difference between a probe over a type an accessor happens to read and
			// a probe over the set. Measured: repeating entry 0 -- required_capabilities -- was
			// accepted, because GroupPolicyOf looks up the policy and nothing looked up the
			// capabilities, so this method published a set ValSem209 refuses. The fix is in
			// group.go and this loop is what would have found it.
			for at := range published {
				repeated := append(slices.Clone(published), published[at])
				if _, err := group.ProposeGroupContextExtensions(repeated); !errors.Is(err, ErrMalformedExtension) {
					t.Errorf("an extension set carrying entry %d (type %#04x) twice was proposed with %v, want ErrMalformedExtension; the repeat is a lookup's refusal and not this loop's",
						at, uint16(published[at].ExtensionType), err)
				}
			}
		},
	},
	"ValSem209GroupExtensionsSupported": {
		what: "walks every extension a GroupContextExtensions proposal installs and applies the v1 profile gate " +
			"to each, then section 13.4 and section 7.3 to every member the commit leaves in the group. It " +
			"SELECTS none of them -- the one extension it reads BY TYPE, required_capabilities, it reads through " +
			"requiredCapabilitiesOf and therefore through the lookup, which is what makes a proposal carrying two " +
			"of them a refusal rather than a choice the committer gets to make",
		refusesTheRepeat: false,
		probe: func(t *testing.T) {
			crypto := testCrypto(t)
			tree, _ := testTreeWith(t, crypto, "alice")
			// the offending type at entry 1, behind an admitted one
			outside := testProposalList(t, testGceOf(
				Extension{ExtensionType: ExtensionTypeRatchetTree},
				Extension{ExtensionType: ExtensionTypeExternalSenders}))
			if err := ValSem209GroupExtensionsSupported(
				testCommitInput(t, crypto, tree, outside, &Commit{})); !errors.Is(err, errProfileExternalSender) {
				t.Errorf("a group_context_extensions proposal whose SECOND entry is outside the v1 profile was judged %v, want errProfileExternalSender",
					err)
			}
			// one admitted type carried twice is accepted here, because the repeat is the
			// lookup's rule and this walk applies the profile to every entry
			twice := testProposalList(t, testGceOf(
				Extension{ExtensionType: ExtensionTypeUrmessageGroupPolicy},
				Extension{ExtensionType: ExtensionTypeUrmessageGroupPolicy}))
			if err := ValSem209GroupExtensionsSupported(
				testCommitInput(t, crypto, tree, twice, &Commit{})); err != nil {
				t.Errorf("a group_context_extensions proposal carrying one admitted type twice was refused here with %v; if this rule starts refusing the repeat then the package has two doors for one rule",
					err)
			}
			// and the type it DOES read by type is read through the lookup, so two
			// required_capabilities entries are refused rather than one of them chosen
			repeated := testProposalList(t, testGceOf(
				testRequiredCapabilitiesNaming(t, ExtensionTypeUrmessageGroupPolicy),
				testRequiredCapabilitiesNaming(t, ExtensionTypeUrmessageOwnerSuccessor)))
			if err := ValSem209GroupExtensionsSupported(
				testCommitInput(t, crypto, tree, repeated, &Commit{})); !errors.Is(err, ErrMalformedExtension) {
				t.Errorf("a group_context_extensions proposal carrying required_capabilities twice was judged %v, want ErrMalformedExtension; the committer would otherwise choose which body every leaf of this group is held to",
					err)
			}
		},
	},
	"(*StagedCommit).GroupContextExtensions": {
		what: "COPIES every entry of the post-commit extension vector and selects none. It is an accessor that " +
			"answers the whole vector, so a repeat is not a question it decides: two entries of one type are " +
			"both copied out, and the door that refuses the repeat is the lookup every reader of that vector " +
			"reaches. What it reads the type FOR is the copy -- an Extension copied by value goes on pointing " +
			"at the octets the new epoch's group context was built over, so the entries are rebuilt field by " +
			"field and the type is one of the two fields",
		refusesTheRepeat: false,
		probe: func(t *testing.T) {
			crypto := testCrypto(t)
			owner := testIdentity(t, crypto, "owner")
			group := testNewGroup(t, crypto, owner, "staged-extensions")
			defer group.Close()
			if _, err := group.CreateCommit([][]byte{}, nil, nil); err != nil {
				t.Fatalf("CreateCommit: %v", err)
			}
			staged := group.stagedForTest()
			if staged == nil {
				t.Fatal("the commit staged nothing, so this probe reads nothing")
			}
			answered := staged.GroupContextExtensions()
			if len(answered) != len(staged.context.Extensions) {
				t.Fatalf("the accessor answered %d entry/entries and the staged context carries %d; a walk that selected by type would answer fewer",
					len(answered), len(staged.context.Extensions))
			}
			if len(answered) == 0 {
				t.Fatal("the staged context carries no extension, so this probe compared nothing")
			}
			for i := range answered {
				if answered[i].ExtensionType != staged.context.Extensions[i].ExtensionType {
					t.Errorf("entry %d is answered as %#04x and the staged context holds %#04x; the vector is answered in order and not selected from",
						i, uint16(answered[i].ExtensionType), uint16(staged.context.Extensions[i].ExtensionType))
				}
			}
			// and the bodies are COPIES, which is what the type read is for
			scribbled := false
			for i := range answered {
				for at := range answered[i].ExtensionData {
					answered[i].ExtensionData[at] ^= 0xFF
					scribbled = true
				}
			}
			if !scribbled {
				t.Fatal("no extension body was answered, so the copy this probe checks was never made")
			}
			again := staged.GroupContextExtensions()
			for i := range again {
				if !bytes.Equal(again[i].ExtensionData, staged.context.Extensions[i].ExtensionData) {
					t.Errorf("a caller writing through entry %d changed the extension the staged epoch was derived over",
						i)
				}
			}
		},
	},
	"ExtensionsWithGroupPolicy": {
		what: "walks every entry and REPLACES the one carrying urmessage_group_policy with the body it was " +
			"handed, in that entry's own position, copying every other entry where it stands; a list with " +
			"no policy gains one at the end. It selects by type and it is the one helper both policy doors of " +
			"the seam go through, so that a policy change leaves required_capabilities and every other entry " +
			"standing -- ledger item 242's P4. The repeat is NOT decided here: FindExtensionEntry runs first " +
			"and refuses a list carrying the type twice, so the walk never chooses between two policies and " +
			"never collapses them, and the body is judged through GroupPolicyOf on the way out",
		refusesTheRepeat: false,
		probe: func(t *testing.T) {
			crypto := testCrypto(t)
			owner := testIdentity(t, crypto, "owner")
			admin := testIdentity(t, crypto, "admin")
			member := testIdentity(t, crypto, "member")
			policy := testPolicy(t, owner, admin, member)
			encoded, err := policy.Encode()
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			policy.SetRole(member.IdentityPub, RoleAdmin)
			replaced, err := policy.Encode()
			if err != nil {
				t.Fatalf("Encode the replacement: %v", err)
			}
			caps := Extension{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: []byte{0x01}}
			other := Extension{ExtensionType: ExtensionTypeRatchetTree, ExtensionData: []byte{0x02, 0x02}}
			held := []Extension{caps, encoded, other}
			got, err := ExtensionsWithGroupPolicy(held, replaced.ExtensionData)
			if err != nil {
				t.Fatalf("ExtensionsWithGroupPolicy over a three entry list: %v", err)
			}
			if len(got) != 3 || got[0].ExtensionType != caps.ExtensionType || got[1].ExtensionType != ExtensionTypeUrmessageGroupPolicy || got[2].ExtensionType != other.ExtensionType {
				t.Fatalf("the list came back as %d entries in an order that is not the caller's; the policy is replaced IN POSITION and nothing else moves", len(got))
			}
			if !bytes.Equal(got[0].ExtensionData, caps.ExtensionData) || !bytes.Equal(got[2].ExtensionData, other.ExtensionData) {
				t.Error("an entry that is not the policy changed across the replacement")
			}
			if !bytes.Equal(got[1].ExtensionData, replaced.ExtensionData) {
				t.Error("the policy entry does not carry the body it was replaced with")
			}
			// every body is a copy, the policy's included
			for i := range got {
				got[i].ExtensionData[0] ^= 0xff
			}
			if !bytes.Equal(held[0].ExtensionData, caps.ExtensionData) || !bytes.Equal(held[2].ExtensionData, other.ExtensionData) || replaced.ExtensionData[0] == got[1].ExtensionData[0] {
				t.Error("writing through the answer changed the list or the policy it was built from")
			}
			// a list with no policy gains one at the end
			appended, err := ExtensionsWithGroupPolicy([]Extension{caps}, replaced.ExtensionData)
			if err != nil || len(appended) != 2 || appended[1].ExtensionType != ExtensionTypeUrmessageGroupPolicy {
				t.Fatalf("a list with no policy answered (%d entries, %v), want the caps entry and the policy after it", len(appended), err)
			}
			// the repeat is the lookup's refusal and not a choice made here
			if _, err := ExtensionsWithGroupPolicy([]Extension{encoded, caps, encoded}, replaced.ExtensionData); !errors.Is(err, ErrMalformedExtension) {
				t.Errorf("a list carrying the policy twice answered %v, want ErrMalformedExtension from the lookup; a helper that replaced both or the first would be a second door on the repeat", err)
			}
			// and a body that is not a policy is refused before it is on the wire
			if _, err := ExtensionsWithGroupPolicy(held, []byte{0xff}); err == nil {
				t.Error("a body that does not decode as a policy was accepted into the list")
			}
		},
	},
	"reconcileWithGroupContext": {
		what: "compares the leaves' extensions vector against the epoch's POSITIONALLY, entry by entry, after " +
			"pinning their lengths equal. It selects nothing by type -- both operands of its comparison are read " +
			"at the same index -- so a repeat is not decided here. The one extension it does read BY TYPE, " +
			"required_capabilities, it reads through the lookup, which is what makes a peer sending two of them a " +
			"refusal at this validation entry point rather than a choice the peer gets to make",
		refusesTheRepeat: false,
		probe: func(t *testing.T) {
			crypto := testCrypto(t)
			body := []byte{0x00, 0x00, 0x00}
			caps := &RequiredCapabilities{}
			encoded, err := marshalBytes(caps.MarshalMLS)
			if err != nil {
				t.Fatalf("encode the required_capabilities this probe reconciles: %v", err)
			}
			contextOf := func(exts []Extension) *GroupContext {
				return &GroupContext{
					CipherSuite: CipherSuiteX25519ChaCha20Sha256Ed25519,
					GroupId:     []byte("reconcile"),
					Extensions:  exts,
				}
			}
			validation := func(exts []Extension, required *RequiredCapabilities) *TreeValidationContext {
				return &TreeValidationContext{
					Crypto:          crypto,
					Suite:           CipherSuiteX25519ChaCha20Sha256Ed25519,
					GroupId:         []byte("reconcile"),
					RequiredCaps:    required,
					GroupExtensions: exts,
				}
			}
			// required_capabilities is LAST and the disagreement is planted in the MIDDLE, which
			// is what makes this observe the property it claims. Planted in the
			// required_capabilities entry instead, it is refused by the body reconciliation at
			// the end whatever the positional loop did -- so the first version of this probe
			// passed over a loop narrowed to entry zero, and mutation m14 was caught only by
			// TestValidateAgainstContextComparesEveryEntryOfTheExtensionsVectorAndBothHalvesOfEach.
			agreeing := []Extension{
				{ExtensionType: ExtensionTypeApplicationId, ExtensionData: body},
				{ExtensionType: ExtensionTypeRatchetTree, ExtensionData: []byte{0x0a}},
				{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: encoded},
			}
			if err := reconcileWithGroupContext(validation(agreeing, caps), contextOf(agreeing)); err != nil {
				t.Fatalf("three identical extension vectors were refused: %v; every refusal below could then be this", err)
			}
			for _, one := range []struct {
				name  string
				entry Extension
			}{
				{"a type", Extension{ExtensionType: ExtensionTypeApplicationId, ExtensionData: []byte{0x0a}}},
				{"a body", Extension{ExtensionType: ExtensionTypeRatchetTree, ExtensionData: []byte{0x0b}}},
			} {
				differing := slices.Clone(agreeing)
				differing[1] = one.entry
				if err := reconcileWithGroupContext(validation(agreeing, caps), contextOf(differing)); !errors.Is(err, errGroupContextDisagreement) {
					t.Errorf("%s disagreement in the middle of three entries answered %v, want errGroupContextDisagreement; a comparison that stops at the first entry lets a swapped pair through, and nothing behind it can see one",
						one.name, err)
				}
			}
			// and the type it DOES read by type is read through the lookup, so a peer sending
			// two required_capabilities is refused here rather than choosing which one this
			// client holds every leaf to
			repeated := append(slices.Clone(agreeing), Extension{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: encoded})
			if err := reconcileWithGroupContext(validation(repeated, caps), contextOf(repeated)); !errors.Is(err, ErrMalformedExtension) {
				t.Errorf("a group context carrying required_capabilities twice reconciled with %v, want ErrMalformedExtension; the *GroupContext here came off the wire, so a peer would otherwise choose which body every leaf of this group is judged against",
					err)
			}
		},
	},
}

// extensionTypeSelectionsOfEveryRoot is the derived class over both scanned roots, with the
// place each was found.
//
// A name that appears in both roots is fatal rather than merged: the table is keyed by name, and
// two declarations sharing one row would let a row assert one of them while covering neither.
func extensionTypeSelectionsOfEveryRoot(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{}
	reads := 0
	for _, root := range extensionTypeSelectionRoots {
		checked := typeCheckedBodiesOf(t, root)
		if len(checked.paths) == 0 {
			t.Fatalf("%s holds no non test source, so this gate scanned nothing there", root)
		}
		scan := extensionTypeSelectionsIn(checked)
		reads += scan.reads
		for name, where := range scan.selecting {
			if held, already := found[name]; already {
				t.Fatalf("%s selects an extension by type at %s and at %s; this table is keyed by name, so the two would share one row and one of them would go unclassified",
					name, held, where)
			}
			found[name] = where
		}
	}
	if reads == 0 {
		t.Fatalf("no read of an extension's type was resolved across %v, so this gate would report a clean run over a package that hand rolled every lookup it has",
			extensionTypeSelectionRoots)
	}
	t.Logf("%d extension type reads across %v, of which %d are selections out of a vector",
		reads, extensionTypeSelectionRoots, len(found))
	return found
}

// TestEveryExtensionTypeSelectionOfBothPackagesIsClassifiedHere is rule 5 over the class this
// package had no derivation for: a lookup that selects an extension by type.
//
// The contradiction this file was written for was reachable because nothing derived that class.
// Three comments handed the refusal of a repeated type to ValSem209, two accessors refused it
// themselves and said ValSem209 does not exist, and both were committed together -- and the
// reason nobody had to choose was that adding a lookup required nothing of its author. p7 has
// nineteen tasks left and several of them read an extension off a group context, so the gate has
// to fail on the commit that writes the fourth one rather than on the reader who notices later.
//
// The matcher runs on the control first, which is what says it reads anything at all: an empty
// report over this package and an empty report from a matcher that resolved nothing are the same
// value, and only the control tells them apart.
func TestEveryExtensionTypeSelectionOfBothPackagesIsClassifiedHere(t *testing.T) {
	control := extensionTypeSelectionsIn(typeCheckedBodiesOfText(t, "the extension type selection control",
		extensionTypeSelectionControl))
	if reported := slices.Sorted(maps.Keys(control.selecting)); !slices.Equal(reported, extensionTypeSelectionControlReports) {
		t.Fatalf("the rule reported %v out of the control, want %v; a rule that reports the tag checks demands a row for every parse in this package, and one that misses the closure form reports a clean run over a lookup written with slices.IndexFunc",
			reported, extensionTypeSelectionControlReports)
	}
	if control.reads < len(extensionTypeSelectionControlReports) {
		t.Fatalf("the rule resolved %d extension type reads in the control, which declares more than that; the type resolution is not reading the control at all",
			control.reads)
	}

	derived := extensionTypeSelectionsOfEveryRoot(t)
	classified := slices.Sorted(maps.Keys(extensionTypeSelectionsOfBothPackages))
	if found := slices.Sorted(maps.Keys(derived)); !slices.Equal(found, classified) {
		t.Fatalf("%v select an extension by type and this table classifies %v; a declaration with no row is a lookup nobody decided the repeat question for, and a row with no declaration is a classification that outlived what it classified. Locations: %v",
			found, classified, derived)
	}
	for name, where := range derived {
		t.Logf("%s at %s: %s", name, where, extensionTypeSelectionsOfBothPackages[name].what)
	}

	doors := []string{}
	for name, one := range extensionTypeSelectionsOfBothPackages {
		if one.refusesTheRepeat {
			doors = append(doors, name)
		}
		if one.what == "" || one.probe == nil {
			t.Errorf("%s is classified with no account of what it does or no probe of it; a row that states nothing is the enumeration this gate exists to not be", name)
		}
	}
	slices.Sort(doors)
	if !slices.Equal(doors, []string{"FindExtensionEntry"}) {
		t.Errorf("the declarations refusing a repeated extension type are %v, want exactly [FindExtensionEntry]; two doors for one rule is two rules, and a caller reaching only one of them is a caller the other does not protect",
			doors)
	}
}

// TestEveryClassifiedExtensionTypeSelectionBehavesAsItIsClassified runs each row's probe, so a
// row is an assertion rather than a label.
//
// The gate above compares two sets of names and would pass over a table whose every account was
// wrong. What separates "walks every entry" from "stops at the first" is not visible in any name
// or signature: both are a range statement over the same vector, and the difference is which
// entries the rule is applied to. So each row states its claim as an input and an answer.
func TestEveryClassifiedExtensionTypeSelectionBehavesAsItIsClassified(t *testing.T) {
	if len(extensionTypeSelectionsOfBothPackages) == 0 {
		t.Fatal("the classification table is empty, so this runs nothing")
	}
	for _, name := range slices.Sorted(maps.Keys(extensionTypeSelectionsOfBothPackages)) {
		one := extensionTypeSelectionsOfBothPackages[name]
		t.Run(name, func(t *testing.T) { one.probe(t) })
	}
}
