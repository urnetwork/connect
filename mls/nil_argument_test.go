// The gate over what this package does when a declaration is handed nil at a POINTER argument it
// is going to read.
//
// provider_nil_test.go holds this rule for one argument TYPE and records what it was worth:
// ErrNilCryptoProvider was returned by ONE of the twenty one declarations that take a provider,
// and the other twenty raised a nil pointer dereference out of whatever line first read a width
// off it. A panic out of a library is not a refusal. It takes the caller's process rather than its
// call, and it says nothing about which argument was wrong.
//
// A provider is one argument of many, and two refusals of the framing layer sat outside that gate
// and outside everything else. errNilAuthenticatedContent and errNilFramedContent were declared,
// documented, and raised by code no test executed: deleting either guard -- which turns
// VerifyAuthenticatedContent(crypto, pub, nil, gc) and FramedContentTBSBytes(wf, nil, gc) from
// refusals into nil dereferences -- left every one of the 6339 tests of ./mls/... and
// ./message/... passing.
//
// The class is read off the source rather than listed. nilArgumentRefusalsIn finds every
// declaration that refuses a nil PARAMETER by answering a variable this package declares, and the
// row table below is held against it in BOTH directions, so a refusal with no row fails here
// rather than being quietly left out of the sweep. ErrNilCryptoProvider is the single exclusion,
// by name and with its reason: that whole class has the derived gate next door, and every row it
// would add here would be a second copy of one that already runs.
package mls

import (
	"bytes"
	"crypto/rand"
	"errors"
	"go/ast"
	"go/token"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// The one sentinel this class excludes, named rather than filtered out by shape.
//
// Excluded because it is covered and not because it is uninteresting:
// TestEveryDeclarationHandedANilProviderRefusesRatherThanDereferencingIt derives the provider
// class twice over -- off the type checker for the constructions and off the method set for the
// methods -- and runs every member. A row here for each would restate that gate's twenty one
// rows in a table this one keeps by hand, which is the worse of the two ways to hold it.
const nilArgumentProviderSentinel = "ErrNilCryptoProvider"

// nilArgumentRefusal is one parameter of one declaration that this package refuses a nil at,
// together with the sentinel its own source answers.
type nilArgumentRefusal struct {
	declaration string
	parameter   string
	sentinel    string
}

// key names the refusal the way the row table below is keyed.
func (self nilArgumentRefusal) key() string {
	return self.declaration + "(" + self.parameter + ")"
}

// nilArgumentGuardedParameters collects the identifiers an if condition compares against nil.
//
// It walks && and || rather than matching a single comparison, because a guard that refuses two
// arguments at once is one a matcher written for the bare shape does not see -- and this package
// writes them: EncryptUpdatePath's is `plan == nil || plan.LeafNode == nil || plan.Private == nil`
// and Consistent's is `tree == nil || tree.Leaf(self.LeafIndex) == nil`. A class that cannot see a
// compound guard is a class the next one drops out of without saying so.
//
// Only a bare identifier on the left is collected. `self.field == nil` is a fact about a value the
// CALLER assembled and not about the argument it passed, and a local compared against nil is a
// fact about what the body derived; neither is this gate's subject, and the parameter filter below
// is what keeps them out.
func nilArgumentGuardedParameters(condition ast.Expr, found *[]string) {
	switch node := condition.(type) {
	case *ast.ParenExpr:
		nilArgumentGuardedParameters(node.X, found)
	case *ast.BinaryExpr:
		if node.Op != token.EQL {
			nilArgumentGuardedParameters(node.X, found)
			nilArgumentGuardedParameters(node.Y, found)
			return
		}
		right, isIdentifier := node.Y.(*ast.Ident)
		if !isIdentifier || right.Name != "nil" {
			return
		}
		if left, isIdentifier := node.X.(*ast.Ident); isIdentifier {
			*found = append(*found, left.Name)
		}
	}
}

// nilArgumentSentinelsReturned answers the package level variables a return statement names, at
// ANY depth of the expressions it returns.
//
// At any depth rather than as a bare identifier, because half of this package's nil refusals are
// wrapped: CheckUpdatePathKeyUniqueness answers fmt.Errorf("%w: ...", ErrTreeMalformed) and
// ValidateAgainstContext answers fmt.Errorf("%w: ...", ErrTreeHashMismatch). A reader matching
// only `return ErrX` sees neither, and the members it does see are the ones whose sentence
// happened to be short enough to fit on the return -- a class whose membership is decided by
// formatting.
func nilArgumentSentinelsReturned(statement *ast.ReturnStmt, declared []string) []string {
	named := []string{}
	for _, result := range statement.Results {
		ast.Inspect(result, func(node ast.Node) bool {
			identifier, isIdentifier := node.(*ast.Ident)
			if isIdentifier && slices.Contains(declared, identifier.Name) &&
				!slices.Contains(named, identifier.Name) {
				named = append(named, identifier.Name)
			}
			return true
		})
	}
	return named
}

// packageLevelVarNamesIn is every package level variable one parsed file declares.
//
// Every one, rather than the ones whose name reads like an error. What makes a returned
// identifier a sentinel is that this package declares it -- nil and false do not, and neither
// does a local -- and a filter on the spelling "err" would drop a refusal named any other way and
// shrink this class without anybody editing it.
func packageLevelVarNamesIn(parsed parsedSource) []string {
	names := []string{}
	for _, declaration := range parsed.file.Decls {
		generic, isGeneric := declaration.(*ast.GenDecl)
		if !isGeneric || generic.Tok != token.VAR {
			continue
		}
		for _, specification := range generic.Specs {
			value, isValue := specification.(*ast.ValueSpec)
			if !isValue {
				continue
			}
			for _, name := range value.Names {
				if name.Name != "_" {
					names = append(names, name.Name)
				}
			}
		}
	}
	return names
}

// packageSentinelTextsIn is the message of every sentinel one file declares as errors.New of a
// literal, keyed by name.
//
// This is the join between the two halves of the gate. The derived class carries a sentinel's
// NAME and the row table carries a VALUE, and without something holding those together a row
// naming errNilUpdatePath while asserting errNilTreeKEMPrivate passes on any declaration that
// answers either -- the shape assertIsTheFunctionNamed exists for one file over.
// errors.New of a literal is the form every sentinel this class reaches is declared in, and one
// declared some other way is reported by the gate rather than silently unjoined.
func packageSentinelTextsIn(parsed parsedSource) map[string]string {
	texts := map[string]string{}
	for _, declaration := range parsed.file.Decls {
		generic, isGeneric := declaration.(*ast.GenDecl)
		if !isGeneric || generic.Tok != token.VAR {
			continue
		}
		for _, specification := range generic.Specs {
			value, isValue := specification.(*ast.ValueSpec)
			if !isValue || len(value.Names) != 1 || len(value.Values) != 1 {
				continue
			}
			call, isCall := value.Values[0].(*ast.CallExpr)
			if !isCall || len(call.Args) != 1 || parsed.render(call.Fun) != "errors.New" {
				continue
			}
			literal, isLiteral := call.Args[0].(*ast.BasicLit)
			if !isLiteral || literal.Kind != token.STRING {
				continue
			}
			unquoted, err := strconv.Unquote(literal.Value)
			if err != nil {
				continue
			}
			texts[value.Names[0].Name] = unquoted
		}
	}
	return texts
}

// nilArgumentRefusalsIn reads one parsed file for the refusals it declares.
func nilArgumentRefusalsIn(parsed parsedSource, declared []string) []nilArgumentRefusal {
	found := []nilArgumentRefusal{}
	for _, declaration := range parsed.file.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Body == nil {
			continue
		}
		parameters := []string{}
		if function.Type.Params != nil {
			for _, field := range function.Type.Params.List {
				for _, name := range field.Names {
					parameters = append(parameters, name.Name)
				}
			}
		}
		name := function.Name.Name
		if function.Recv != nil && len(function.Recv.List) == 1 {
			name = "(" + parsed.render(function.Recv.List[0].Type) + ")." + name
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			branch, isBranch := node.(*ast.IfStmt)
			if !isBranch {
				return true
			}
			guarded := []string{}
			nilArgumentGuardedParameters(branch.Cond, &guarded)
			for _, parameter := range guarded {
				if !slices.Contains(parameters, parameter) {
					continue
				}
				for _, statement := range branch.Body.List {
					returned, isReturn := statement.(*ast.ReturnStmt)
					if !isReturn {
						continue
					}
					for _, sentinel := range nilArgumentSentinelsReturned(returned, declared) {
						if sentinel == nilArgumentProviderSentinel {
							continue
						}
						refusal := nilArgumentRefusal{
							declaration: name, parameter: parameter, sentinel: sentinel}
						if !slices.Contains(found, refusal) {
							found = append(found, refusal)
						}
					}
				}
			}
			return true
		})
	}
	return found
}

// nilArgumentRefusalsDeclared is the class over the whole package, sorted.
//
// The variable roster is collected over every file before any file is read for refusals, because
// a sentinel is regularly declared in one file and answered in another -- ErrTreeMalformed is
// tree.go's and CheckUpdatePathKeyUniqueness answers it from tree_sync.go. A per file roster would
// read those refusals as returning an unknown identifier and drop them, which is the silent
// shrink every derivation in this package is written against.
func nilArgumentRefusalsDeclared(t *testing.T) []nilArgumentRefusal {
	t.Helper()
	names := []string{}
	for _, path := range packageLevelFunctions(t).files {
		names = append(names, packageLevelVarNamesIn(mustParseSource(t, path))...)
	}
	if len(names) == 0 {
		t.Fatal("this package's non test source declares no package level variable, so every sentinel below reads as an unknown identifier and the class is empty")
	}
	found := []nilArgumentRefusal{}
	for _, path := range packageLevelFunctions(t).files {
		found = append(found, nilArgumentRefusalsIn(mustParseSource(t, path), names)...)
	}
	if len(found) == 0 {
		t.Fatal("no declaration of this package refuses a nil argument, so the gate below demands nothing")
	}
	slices.SortFunc(found, func(first nilArgumentRefusal, second nilArgumentRefusal) int {
		return strings.Compare(first.key(), second.key())
	})
	return found
}

// nilArgumentSentinelTexts is the same roster of messages over the whole package.
func nilArgumentSentinelTexts(t *testing.T) map[string]string {
	t.Helper()
	texts := map[string]string{}
	for _, path := range packageLevelFunctions(t).files {
		for name, text := range packageSentinelTextsIn(mustParseSource(t, path)) {
			texts[name] = text
		}
	}
	if len(texts) == 0 {
		t.Fatal("no sentinel of this package was read as errors.New of a literal, so nothing joins a row's value to the name the class carries")
	}
	return texts
}

// nilArgumentRow is one member of the class, with a call that hands it nil at that parameter and
// something VALID everywhere else.
//
// Valid everywhere else is the whole of what separates this from the provider gate next door.
// That one hands the zero value at every position, and can, because the provider is refused
// before any other argument is judged so nothing else is reachable. Here the nil argument is
// never the first thing checked -- the provider is, wherever there is one -- so a row that zeroed
// its neighbours would be answered about one of THEM and would report a refusal it did not ask
// for.
type nilArgumentRow struct {
	sentinel error
	call     func(t *testing.T) error
}

// nilArgumentRows is the table, keyed the way nilArgumentRefusal.key spells a member.
//
// It is not what decides the class: the sweep holds it against the derived refusals in both
// directions, so a declaration with no row fails rather than being left out, and a row naming a
// refusal this package no longer makes fails rather than outliving it.
//
// One provider and one tree for the whole table, cloned by every row that mutates. A row that
// built its own would pay for four leaves of key generation each time, and two of these
// declarations -- SetLeaf and MergeUpdatePath -- write into the receiver they are handed, so a
// shared fixture that was not cloned would leave a later row running against a tree an earlier
// row had already changed.
func nilArgumentRows(t *testing.T) map[string]nilArgumentRow {
	t.Helper()
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	tree, members := newTestTree(t, crypto, 4)
	secret := bytes.Repeat([]byte{0x5c}, crypto.HashSize())
	return map[string]nilArgumentRow{
		"(*RatchetTree).SetLeaf(leaf)": {sentinel: ErrTreeMalformed, call: func(t *testing.T) error {
			return tree.Clone().SetLeaf(members[0].LeafIndex, nil)
		}},
		// an ODD index, because SetParent refuses a leaf position before it looks at the
		// node it was handed and this row is about the node
		"(*RatchetTree).SetParent(parent)": {sentinel: ErrTreeMalformed, call: func(t *testing.T) error {
			return tree.Clone().SetParent(NodeIndex(1), nil)
		}},
		"(*RatchetTree).ValidateAgainstContext(gc)": {sentinel: ErrTreeHashMismatch, call: func(t *testing.T) error {
			return tree.ValidateAgainstContext(&TreeValidationContext{
				Crypto: crypto, Suite: crypto.Suite(), GroupId: testGroupId()}, nil)
		}},
		// p7 task 14's group info verifier, whose tree is not one input among several: the
		// signer's public key is not in a GroupInfo at all, so the tree is the only thing that
		// can say who the members are. A nil one would otherwise be dereferenced at LeafWidth.
		// ErrTreeMalformed and none of that method's own four sentinels, because no tree is not
		// a tree whose hash disagrees, an index outside one, a blank leaf or a bad signature.
		"(*GroupInfo).Verify(tree)": {sentinel: ErrTreeMalformed, call: func(t *testing.T) error {
			return (&GroupInfo{}).Verify(crypto, nil)
		}},
		"(*TreeKEMPrivate).Consistent(tree)": {sentinel: ErrPathSecretMismatch, call: func(t *testing.T) error {
			return NewTreeKEMPrivate(members[0].LeafIndex, members[0].EncryptionPriv).Consistent(crypto, nil)
		}},
		"(*RatchetTree).EncryptUpdatePath(plan)": {sentinel: errNilUpdatePathPlan, call: func(t *testing.T) error {
			_, err := tree.Clone().EncryptUpdatePath(crypto, nil, members[0].LeafIndex,
				[]byte("context"), nil)
			return err
		}},
		"(*RatchetTree).MergeUpdatePath(path)": {sentinel: errNilUpdatePath, call: func(t *testing.T) error {
			return tree.Clone().MergeUpdatePath(crypto, members[0].LeafIndex, nil)
		}},
		"(*RatchetTree).DecryptUpdatePath(path)": {sentinel: errNilUpdatePath, call: func(t *testing.T) error {
			_, err := tree.Clone().DecryptUpdatePath(crypto, members[0].LeafIndex, nil,
				[]byte("context"), NewTreeKEMPrivate(members[1].LeafIndex, members[1].EncryptionPriv), nil)
			return err
		}},
		"(*RatchetTree).DecryptUpdatePath(priv)": {sentinel: errNilTreeKEMPrivate, call: func(t *testing.T) error {
			_, err := tree.Clone().DecryptUpdatePath(crypto, members[0].LeafIndex, &UpdatePath{},
				[]byte("context"), nil, nil)
			return err
		}},
		"CheckUpdatePathKeyUniqueness(path)": {sentinel: errNilUpdatePath, call: func(t *testing.T) error {
			return CheckUpdatePathKeyUniqueness(tree, nil)
		}},
		"CheckUpdatePathKeyUniqueness(tree)": {sentinel: ErrTreeMalformed, call: func(t *testing.T) error {
			return CheckUpdatePathKeyUniqueness(nil, &UpdatePath{})
		}},
		"DeriveJoinerSecret(groupContext)": {sentinel: ErrNilGroupContext, call: func(t *testing.T) error {
			_, err := DeriveJoinerSecret(crypto, secret, secret, nil)
			return err
		}},
		"NewKeyScheduleFromJoiner(groupContext)": {sentinel: ErrNilGroupContext, call: func(t *testing.T) error {
			_, err := NewKeyScheduleFromJoiner(crypto, secret, secret, nil)
			return err
		}},
		"NewKeyScheduleFromEpochSecret(groupContext)": {sentinel: ErrNilGroupContext, call: func(t *testing.T) error {
			_, err := NewKeyScheduleFromEpochSecret(crypto, secret, nil)
			return err
		}},
		"OpenWithLabel(ct)": {sentinel: errNilHpkeCiphertext, call: func(t *testing.T) error {
			priv, _, err := crypto.DeriveKeyPair(secret)
			if err != nil {
				t.Fatalf("a key pair for the open row: %v", err)
			}
			_, err = OpenWithLabel(crypto, priv, "Welcome", []byte("context"), nil)
			return err
		}},
		// the outbound entry point of section 6's outermost structure, which would otherwise
		// dereference its message at MarshalMLS's first statement
		"MarshalMLSMessage(message)": {sentinel: errNilMLSMessage, call: func(t *testing.T) error {
			_, err := MarshalMLSMessage(nil)
			return err
		}},
		// p7 task 15's builder, whose group info would otherwise be dereferenced inside
		// marshalBoundedComposition at its first statement after the provider check. The row
		// hands VALID secrets, because a welcome secret of the wrong length is WelcomeKeyNonce's
		// ErrSecretLength and would report a refusal this row did not ask for -- and it is
		// errNilWelcomeGroupInfo rather than any of welcome.go's other sentinels because no
		// group info is not a group info this provider cannot check, nor one whose preimage will
		// not encode: nothing whatever has been decided about an object that is not here.
		"BuildWelcome(info)": {sentinel: errNilWelcomeGroupInfo, call: func(t *testing.T) error {
			_, err := BuildWelcome(crypto, crypto.Suite(), nil, secret, secret, nil)
			return err
		}},
		// the framing pair, which is what this file was written for
		"FramedContentTBSBytes(content)": {sentinel: errNilFramedContent, call: func(t *testing.T) error {
			_, err := FramedContentTBSBytes(WireFormatPrivateMessage, nil, framingTestGroupContext(t))
			return err
		}},
		// the membership tag preimage, which dereferences its message at its first statement
		// and so is the same shape one layer over
		"AuthenticatedContentTBMBytes(authContent)": {sentinel: errNilAuthenticatedContent, call: func(t *testing.T) error {
			_, err := AuthenticatedContentTBMBytes(nil, framingTestGroupContext(t))
			return err
		}},
		"VerifyAuthenticatedContent(authContent)": {sentinel: errNilAuthenticatedContent, call: func(t *testing.T) error {
			_, pub, err := crypto.SignatureKeyPair()
			if err != nil {
				t.Fatalf("a key pair for the verify row: %v", err)
			}
			return VerifyAuthenticatedContent(crypto, pub, nil, framingTestGroupContext(t))
		}},
		// the cache's own constructor, which is the first half of where the epoch binding
		// comes from. A nil context here would otherwise answer a cache bound to nothing,
		// which is the one state in which a message can supply the epoch. It takes a
		// *VerifiedGroupContext rather than a *GroupContext, so the OTHER half of that
		// question -- whose context it is -- is the compiler's rather than a gate's
		// p7 task 10's two rules that are stated over something other than a validation
		// input, so their nil guards are argument rules rather than the input rule's.
		// CheckErrata8745 takes a live path and no context; ValSem300 takes no tree at all,
		// and answering nil at either would be a rule that reports clean by having nothing
		// to read.
		"CheckErrata8745(context)": {sentinel: ErrNilGroupContext, call: func(t *testing.T) error {
			return CheckErrata8745(&UpdatePath{}, nil)
		}},
		"ValSem300NoTrailingBlankNodes(tree)": {sentinel: ErrTreeMalformed, call: func(t *testing.T) error {
			return ValSem300NoTrailingBlankNodes(nil)
		}},
		"NewProposalCache(verified)": {sentinel: ErrNilGroupContext, call: func(t *testing.T) error {
			_, err := NewProposalCache(nil)
			return err
		}},
		// p7 task 6's cache store, which would otherwise dereference its content at the
		// content type check -- the first statement after the provider
		"(*ProposalCache).Store(content)": {sentinel: errNilAuthenticatedContent, call: func(t *testing.T) error {
			_, err := testCache(t).Store(crypto, testResolveContext(), nil)
			return err
		}},
		// the same store's group context, which is the parameter the epoch every rule of
		// that body runs in is taken from. It is read before the message, so this row
		// passes no content at all: what it observes is the refusal, and a body that
		// dereferenced it would do so at the CheckEpoch two statements later.
		"(*ProposalCache).Store(groupContext)": {sentinel: ErrNilGroupContext, call: func(t *testing.T) error {
			_, err := testCache(t).Store(crypto, nil, nil)
			return err
		}},
		// the boundary question asked with no message in hand, and the rebind that answers
		// it. A Rebind handed nothing would otherwise leave the cache bound to nothing,
		// which is the one state in which the epoch could come from somewhere else.
		"(*ProposalCache).CheckEpoch(groupContext)": {sentinel: ErrNilGroupContext, call: func(t *testing.T) error {
			return testCache(t).CheckEpoch(nil)
		}},
		"(*ProposalCache).Rebind(verified)": {sentinel: ErrNilGroupContext, call: func(t *testing.T) error {
			return testCache(t).Rebind(nil)
		}},
		// the same cache's resolution, whose group context is the parameter that lets it
		// refuse a reference cached in an epoch that has closed. It is dereferenced inside
		// the by-reference branch of the loop, so a commit carrying nothing by reference
		// would reach no dereference at all -- which is why the guard is up front and why
		// this row passes an empty vector: what it observes is the refusal and not the one
		// input shape that happens to walk past the read.
		"(*ProposalCache).Resolve(groupContext)": {sentinel: ErrNilGroupContext, call: func(t *testing.T) error {
			_, err := testCache(t).Resolve(crypto, nil, LeafIndex(0), nil)
			return err
		}},
		"NewCryptoProviderWithRandom(random)": {sentinel: ErrNilRandomSource, call: func(t *testing.T) error {
			_, err := NewCryptoProviderWithRandom(CipherSuiteX25519ChaCha20Sha256Ed25519, nil)
			return err
		}},
		"X25519GenerateKey(random)": {sentinel: ErrNilRandomSource, call: func(t *testing.T) error {
			_, err := X25519GenerateKey(nil)
			return err
		}},
		"X25519DH(priv)": {sentinel: ErrBadKeyLength, call: func(t *testing.T) error {
			standing, err := X25519GenerateKey(rand.Reader)
			if err != nil {
				t.Fatalf("a key pair for the dh rows: %v", err)
			}
			_, err = X25519DH(nil, standing.PublicKey())
			return err
		}},
		"X25519DH(pub)": {sentinel: ErrBadKeyLength, call: func(t *testing.T) error {
			standing, err := X25519GenerateKey(rand.Reader)
			if err != nil {
				t.Fatalf("a key pair for the dh rows: %v", err)
			}
			_, err = X25519DH(standing, nil)
			return err
		}},
		// the epoch really carrying a required_capabilities, which is the arm whose nil
		// half is a refusal. With no such extension in the list the nil is legal and the
		// row would be observing the other branch entirely.
		"reconcileRequiredCapabilities(required)": {sentinel: errGroupContextDisagreement, call: func(t *testing.T) error {
			encoded, err := syntax.Marshal(&RequiredCapabilities{})
			if err != nil {
				t.Fatalf("encode the required capabilities the epoch carries: %v", err)
			}
			return reconcileRequiredCapabilities(nil, []Extension{{
				ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: encoded}})
		}},
		// section 6.2's seal and open, which read their message at the first statement after the
		// provider check. The membership key is a live one of the right width in every row, so what
		// is being observed is the nil argument and not a key refusal standing in front of it.
		"SealPublicMessage(authContent)": {sentinel: errNilAuthenticatedContent, call: func(t *testing.T) error {
			_, err := SealPublicMessage(crypto, secret, nil, framingTestGroupContext(t))
			return err
		}},
		"OpenPublicMessage(message)": {sentinel: errNilPublicMessage, call: func(t *testing.T) error {
			_, err := OpenPublicMessage(crypto, secret, nil, StaticSignatureKey(nil),
				framingTestGroupContext(t))
			return err
		}},
		// the resolver, which is a func and not a pointer and is refused for the same reason: an
		// open that called it would take the caller's process rather than its call.
		"OpenPublicMessage(resolve)": {sentinel: errNilSignatureKeyResolver, call: func(t *testing.T) error {
			_, err := OpenPublicMessage(crypto, secret, &PublicMessage{}, nil,
				framingTestGroupContext(t))
			return err
		}},
		// section 6.3.2's seal and open, which read their two pointer arguments at the first
		// statements after the provider check. Every OTHER argument of every row here is a live
		// one -- the secret is at the provider's own hash width, the header is one the aad
		// accepts, the ciphertext is real -- so what is being observed is the nil argument and
		// not a length refusal or an unregistered content type standing in front of it.
		"sealSenderData(senderData)": {sentinel: errNilSenderData, call: func(t *testing.T) error {
			_, err := sealSenderData(crypto, secret, nil, senderDataTestHeader(),
				bytes.Repeat([]byte{0xab}, 64))
			return err
		}},
		"sealSenderData(header)": {sentinel: errNilPrivateMessage, call: func(t *testing.T) error {
			_, err := sealSenderData(crypto, secret, &SenderData{LeafIndex: 1, Generation: 7}, nil,
				bytes.Repeat([]byte{0xab}, 64))
			return err
		}},
		"openSenderData(header)": {sentinel: errNilPrivateMessage, call: func(t *testing.T) error {
			_, err := openSenderData(crypto, secret, bytes.Repeat([]byte{0xcd}, 28), nil,
				bytes.Repeat([]byte{0xab}, 64))
			return err
		}},
		// section 6.3.1's seal and open. Every other argument of every row is a live one -- the
		// wire format is the one the seal admits, the sender is a member, the secret is at the
		// provider's own hash width -- so what is observed is the nil argument and not a wire
		// format or sender type refusal standing in front of it.
		//
		// The key source is refused rather than defaulted, and that is the sharpest of these six:
		// a default would either derive message keys from nothing, which every party in the world
		// can compute, or answer one key for every generation, which is the AEAD nonce reuse the
		// whole of section 9 exists to prevent.
		//
		// THE ROWS NAME sealPrivateMessageAt AND openPrivateMessageAt SINCE 2026-09-17, which is
		// where the guards moved when MASTER section 8.4.2 v2 made the seal report the generation
		// it consumed and the open report the generation it opened at. sealPrivateMessage and
		// OpenPrivateMessage are one-line delegations now, so a row naming either would be a row
		// about a body that refuses nothing -- which is exactly what this gate's derived class
		// says when a row and a declaration come apart.
		"sealPrivateMessageAt(keys)": {sentinel: errNilMessageKeySource, call: func(t *testing.T) error {
			_, _, err := sealPrivateMessageAt(crypto, nil, secret, &AuthenticatedContent{
				WireFormat: WireFormatPrivateMessage,
				Content:    *framingTestMemberContent(),
			}, nil)
			return err
		}},
		"sealPrivateMessageAt(authContent)": {sentinel: errNilAuthenticatedContent, call: func(t *testing.T) error {
			_, _, err := sealPrivateMessageAt(crypto, framingNewKeySource(crypto, 0x01, 0), secret, nil, nil)
			return err
		}},
		"openPrivateMessageAt(keys)": {sentinel: errNilMessageKeySource, call: func(t *testing.T) error {
			_, _, err := openPrivateMessageAt(crypto, nil, secret, &PrivateMessage{},
				StaticSignatureKey(nil), framingTestGroupContext(t))
			return err
		}},
		"openPrivateMessageAt(message)": {sentinel: errNilPrivateMessage, call: func(t *testing.T) error {
			_, _, err := openPrivateMessageAt(crypto, framingNewKeySource(crypto, 0x01, 0), secret, nil,
				StaticSignatureKey(nil), framingTestGroupContext(t))
			return err
		}},
		"openPrivateMessageAt(resolve)": {sentinel: errNilSignatureKeyResolver, call: func(t *testing.T) error {
			_, _, err := openPrivateMessageAt(crypto, framingNewKeySource(crypto, 0x01, 0), secret,
				&PrivateMessage{}, nil, framingTestGroupContext(t))
			return err
		}},
		// MASTER section 8.4.2's aad BUILDER. It is refused rather than defaulted to an empty aad
		// for the reason the key source above is refused: an empty aad is a LEGAL value, so a
		// default here would seal a frame naming no position and no generation and answer it as
		// though it had been bound.
		"(*Group).ProtectBound(aad)": {sentinel: errNilAadBuilder, call: func(t *testing.T) error {
			group := &Group{}
			_, err := group.ProtectBound(nil, []byte("no builder"))
			return err
		}},
		// MASTER section 8.4.6's framed length. Nil parameters are refused rather than read as a
		// suite with zero-width everything, because the answer would be a length short by the
		// signature and the tag -- an early size refusal that ADMITS a body the seal must then
		// refuse after spending a write once index and a write once generation.
		"framedApplicationLengthFor(params)": {sentinel: errFramedLengthSuiteWidth, call: func(t *testing.T) error {
			_, err := framedApplicationLengthFor(nil, 32, 32, 0)
			return err
		}},
		// the decoder, whose header is not an option but a third of its input: the group id, the
		// epoch, the content type and the authenticated data are all reassembled from it, and
		// there is no default header that could stand in without producing a FramedContent that
		// describes a different message.
		// section 6's context and sender rules. Every other argument of both rows is a live one
		// -- the group id and the epoch are the ones this content actually carries, and the
		// sender is a member at an occupied leaf -- so what is observed is the nil argument and
		// not a ValSem002, ValSem003 or ValSem004 refusal standing in front of it.
		"CheckFramedContentContext(content)": {sentinel: errNilFramedContent, call: func(t *testing.T) error {
			live := framingTestMemberContent()
			return CheckFramedContentContext(nil, live.GroupId, live.Epoch)
		}},
		// the occupancy test, which is a func and not a pointer and is refused for the reason
		// OpenPublicMessage's resolver is: a rule that called it would take the caller's process
		// rather than its call.
		"CheckSenderLeaf(leafOccupied)": {sentinel: errNilLeafOccupancyTest, call: func(t *testing.T) error {
			return CheckSenderLeaf(Sender{SenderType: SenderTypeMember, LeafIndex: 1}, nil)
		}},
		// the group lifecycle's leaf keys accessor. A nil leaf is refused rather than
		// dereferenced, and the sentinel is the one the lifecycle asks this question with:
		// "there is no urmessage_leaf_keys here" is the same answer for a leaf that is not
		// there as for a leaf that carries no such extension, and a caller repairs both the
		// same way -- by not wrapping an epoch secret to that device.
		"LeafKeysOf(leaf)": {sentinel: ErrMalformedExtension, call: func(t *testing.T) error {
			_, err := LeafKeysOf(nil)
			return err
		}},
		// the v1 proposal profile gate. A nil proposal has no type to judge, and the refusal is
		// its OWN and not the one an unregistered type gets: a peer sending a code point this
		// build does not implement is routine traffic to drop, and a caller reaching this holding
		// nothing is a commit path bug, and the two are repaired at opposite ends. The
		// alternative to refusing is a dereference, and the caller that reaches this with nil is
		// a commit path holding an arm the codec left empty.
		"checkProposalProfile(proposal)": {sentinel: errNilProposal, call: func(t *testing.T) error {
			return checkProposalProfile(defaultProfile(), nil)
		}},
		// p7 task 8's section 12.3 application, whose three pointer arguments are each read
		// before any proposal is applied. Every OTHER argument of every row is a live one --
		// a real two leaf tree, this profile's own group context, an empty but present list
		// -- so what each observes is the nil argument rather than a refusal standing in
		// front of it. The tree and the list answer separate values because a caller that
		// passed no tree and a caller that passed no proposals have made different mistakes
		// and repair them in different places; the context answers the key schedule's own,
		// which is the value every other declaration of this package asks that question
		// with.
		"ApplyProposals(tree)": {sentinel: errNilRatchetTree, call: func(t *testing.T) error {
			_, err := ApplyProposals(nil, testApplyContext(), LeafIndex(0), &ProposalList{})
			return err
		}},
		"ApplyProposals(ctx)": {sentinel: ErrNilGroupContext, call: func(t *testing.T) error {
			tree, _ := testTreeWith(t, crypto, "alice", "bob")
			_, err := ApplyProposals(tree, nil, LeafIndex(0), &ProposalList{})
			return err
		}},
		"ApplyProposals(list)": {sentinel: errNilProposalList, call: func(t *testing.T) error {
			tree, _ := testTreeWith(t, crypto, "alice", "bob")
			_, err := ApplyProposals(tree, testApplyContext(), LeafIndex(0), nil)
			return err
		}},
		// RFC 9420 section 7.3's commit door. Its group context is where the group id, the suite
		// and the extensions the leaf is judged against all come from, so a nil one is a door
		// with no expectation to state rather than a door over an empty group -- and the PATH is
		// live in that row, which is what makes the refusal the context's.
		"ValidateUpdatePathLeafNode(context)": {sentinel: ErrNilGroupContext, call: func(t *testing.T) error {
			live := commitDoorGroupContext()
			return ValidateUpdatePathLeafNode(crypto, nil, LeafIndex(1),
				commitPathValidUnder(t, crypto, live, LeafIndex(1)))
		}},
		"ValidateUpdatePathLeafNode(path)": {sentinel: errNilUpdatePath, call: func(t *testing.T) error {
			return ValidateUpdatePathLeafNode(crypto, commitDoorGroupContext(), LeafIndex(1), nil)
		}},
		// p7 task 11's group creation. The config is the only argument carrying a group at all
		// -- the suite, the group id, the extensions, the provider and the store are all in it --
		// so a nil one is a caller that reached the constructor holding nothing, and every other
		// argument of this row is live so that what it observes is the config rather than a
		// refusal standing in front of it. Its own value and not ErrNilCryptoProvider: a caller
		// that passed no config has not passed a config with no provider, and the two are
		// repaired in different places.
		"NewGroup(cfg)": {sentinel: errNilGroupConfig, call: func(t *testing.T) error {
			owner := testIdentity(t, crypto, "owner")
			_, err := NewGroup(nil, owner.SigPriv, BasicCredential(owner.IdentityPub))
			return err
		}},
		// p7 task 16's join, whose two nil refusals name two different missing OBJECTS. The config
		// carries the provider, the store and the profile, so a nil one is a caller that reached
		// the door holding nothing at all; the key material carries the key package a Welcome is
		// addressed to and the three private halves that open it, so a nil one is a caller that
		// knows which group it wants and has nothing to join it AS. Every other argument of both
		// rows is a live Welcome and a live tree, so what each observes is the argument it nils
		// rather than a refusal standing in front of it.
		// p7 task 19's restore, whose nil config is NewGroup's refusal read on the way back in:
		// the config carries the store the state is read from, the provider the schedule is
		// rebuilt through and the group id the lookup is keyed by, so a nil one is a caller with
		// nothing to restore from rather than a caller who named a state that is missing.
		"LoadGroup(cfg)": {sentinel: errNilGroupConfig, call: func(t *testing.T) error {
			owner := testIdentity(t, crypto, "owner")
			_, err := LoadGroup(nil, 0, owner.SigPriv)
			return err
		}},
		"JoinFromWelcome(cfg)": {sentinel: errNilGroupConfig, call: func(t *testing.T) error {
			welcome, ratchetTree, keys := nilArgumentJoinInputs(t, crypto)
			_, err := JoinFromWelcome(nil, welcome, ratchetTree, keys)
			return err
		}},
		"JoinFromWelcome(keys)": {sentinel: errNilJoinKeyMaterial, call: func(t *testing.T) error {
			welcome, ratchetTree, _ := nilArgumentJoinInputs(t, crypto)
			joiner := testGroupConfig(t, crypto, testIdentity(t, crypto, "the joiner"), "join-nil-argument")
			_, err := JoinFromWelcome(joiner, welcome, ratchetTree, nil)
			return err
		}},
		// the leaf pairing's own refusal, which is reachable only from a tree that changed under
		// the joiner between the signature key lookup and the read: the lookup skips blank leaves.
		// It answers the ABSENCE sentinel and not the substitution one, because a caller told its
		// leaf is not the leaf it published would go looking at a key package that is fine.
		"leafIsTheOneThisJoinerPublished(inTree)": {sentinel: ErrWelcomeLeafNotFound, call: func(t *testing.T) error {
			member := testIdentity(t, crypto, "the joiner whose leaf is not there")
			leaf, _ := testLeafNode(t, crypto, member)
			return leafIsTheOneThisJoinerPublished(nil, leaf)
		}},
		// p7 task 18's second half. A nil result is one arm of a refusal whose other two arms are a
		// result of the wrong kind and a commit kind with no commit inside it, and all three answer
		// ONE value on purpose: each of them is the same caller mistake -- promoting something
		// (*Group).ProcessMessage did not answer as a commit -- and a caller that split them would
		// be told which field of its own bug to look at rather than that it has one. The group is
		// live rather than closed, so what this row observes is the argument and not errGroupClosed
		// standing in front of it.
		"(*Group).ApplyCommit(processed)": {sentinel: errApplyCommitNotACommit, call: func(t *testing.T) error {
			group := testNewGroup(t, crypto, testIdentity(t, crypto, "the owner"), "apply-nil-argument")
			defer group.Close()
			return group.ApplyCommit(nil)
		}},
		"unmarshalPrivateMessageContent(header)": {sentinel: errNilPrivateMessage, call: func(t *testing.T) error {
			plaintext, err := marshalPrivateMessageContent(framingTestMemberContent(),
				&FramedContentAuthData{Signature: bytes.Repeat([]byte{0x51}, 64)}, 0)
			if err != nil {
				t.Fatalf("the plaintext this row hands the decoder: %v", err)
			}
			_, _, err = unmarshalPrivateMessageContent(plaintext, nil,
				Sender{SenderType: SenderTypeMember, LeafIndex: 1})
			return err
		}},
	}
}

// nilArgumentRefusalOf runs one row and turns a panic into the error it should have been, so one
// member dereferencing its argument is a failure of its own row rather than the end of the sweep.
func nilArgumentRefusalOf(t *testing.T, key string, row nilArgumentRow) (answered error) {
	t.Helper()
	defer func() {
		if panicked := recover(); panicked != nil {
			t.Errorf("%s panicked with %v when handed nil; a panic out of a library takes the caller's process rather than its call, and says nothing about which argument was wrong",
				key, panicked)
			answered = nil
		}
	}()
	return row.call(t)
}

// TestEveryDeclarationRefusingANilArgumentAnswersItsOwnSentinel sweeps the derived class.
//
// Three things per member, and they are different claims. The call must not PANIC, which is the
// whole difference between a refusal and taking the caller's process. It must answer the sentinel
// its own source names, and not merely some error: a length refusal reached because a nil
// argument's width came back zero satisfies err != nil while having dereferenced the argument on
// the way. And the sentinel the row asserts must be the one the class read off the declaration,
// held by the message the source declares it with, so a row cannot cover one refusal while naming
// another.
func TestEveryDeclarationRefusingANilArgumentAnswersItsOwnSentinel(t *testing.T) {
	declared := nilArgumentRefusalsDeclared(t)
	texts := nilArgumentSentinelTexts(t)
	rows := nilArgumentRows(t)
	joined := 0
	keys := []string{}
	for _, refusal := range declared {
		keys = append(keys, refusal.key())
		row, written := rows[refusal.key()]
		if !written {
			t.Errorf("%s refuses a nil %s with %s and has no row, so nothing here runs it",
				refusal.declaration, refusal.parameter, refusal.sentinel)
			continue
		}
		if row.sentinel == nil {
			t.Errorf("the row for %s holds no sentinel", refusal.key())
			continue
		}
		// the row's value against the name the source answers, wherever the sentinel is
		// declared in the form that can be read
		if text, readable := texts[refusal.sentinel]; readable {
			joined++
			if row.sentinel.Error() != text {
				t.Errorf("%s answers %s, which reads %q, and its row holds %q",
					refusal.key(), refusal.sentinel, text, row.sentinel.Error())
			}
		} else {
			t.Logf("%s answers %s, which is not declared as errors.New of a literal, so this row's value is joined to that name by nothing",
				refusal.key(), refusal.sentinel)
		}
	}
	if joined == 0 {
		t.Error("no row was joined to the sentinel its declaration names, so the value half of this gate is held by nothing")
	}
	// every row is CALLED, whether or not the class still names it. A guard deleted from the
	// source leaves its row orphaned, and an orphan reported without being run says the table
	// is out of date rather than that the declaration now panics -- which is the fact worth
	// having. Measured: with the nil authenticated content guard deleted, the join alone
	// reported an unmatched row and nothing observed the nil dereference it had become.
	for _, key := range slices.Sorted(maps.Keys(rows)) {
		row := rows[key]
		if !slices.Contains(keys, key) {
			t.Errorf("nilArgumentRows names %s, and no declaration of this package refuses a nil argument under that name",
				key)
		}
		if row.sentinel == nil {
			continue
		}
		if answered := nilArgumentRefusalOf(t, key, row); !errors.Is(answered, row.sentinel) {
			t.Errorf("%s handed nil answered %v, want %v", key, answered, row.sentinel)
		}
	}
}

// nilArgumentDerivationControl is a body carrying every shape the reader must see and every shape
// it must not.
//
// The control is what says the class above is derived rather than merely non empty. A reader that
// had lost the wrapped form reports four of these five, a reader that had lost the compound form
// reports four, and a reader that had stopped filtering on the parameter list reports the receiver
// field and the local as well -- and all four of those readers report a clean run against the real
// package, because the real package's rows would still have their tables.
const nilArgumentDerivationControl = `package control

import "fmt"

var errPlain = fmt.Errorf("plain")
var errWrapped = fmt.Errorf("wrapped")
var errCompound = fmt.Errorf("compound")
var ErrNilCryptoProvider = fmt.Errorf("the provider")

type holder struct{ field *int }

func Plain(argument *int) error {
	if argument == nil {
		return errPlain
	}
	return nil
}

func Wrapped(argument *int) error {
	if argument == nil {
		return fmt.Errorf("%w: with a reason", errWrapped)
	}
	return nil
}

func Compound(first *int, second *int) error {
	if first == nil || second == nil {
		return errCompound
	}
	return nil
}

func Provider(crypto *int) error {
	if crypto == nil {
		return ErrNilCryptoProvider
	}
	return nil
}

func (self *holder) Method(argument *int) error {
	if self.field == nil {
		return errPlain
	}
	local := argument
	if local == nil {
		return errPlain
	}
	if argument == nil {
		return errWrapped
	}
	return nil
}
`

// TestTheNilArgumentDerivationReadsEveryShapeThisPackageWrites runs the reader over that control.
func TestTheNilArgumentDerivationReadsEveryShapeThisPackageWrites(t *testing.T) {
	parsed := mustParseText(t, "control.go", nilArgumentDerivationControl)
	declared := packageLevelVarNamesIn(parsed)
	slices.Sort(declared)
	if want := []string{"ErrNilCryptoProvider", "errCompound", "errPlain", "errWrapped"}; !slices.Equal(declared, want) {
		t.Fatalf("the control declares %v and the reader found %v", want, declared)
	}
	found := []string{}
	for _, refusal := range nilArgumentRefusalsIn(parsed, declared) {
		found = append(found, refusal.key()+" -> "+refusal.sentinel)
	}
	slices.Sort(found)
	want := []string{
		"(*holder).Method(argument) -> errWrapped",
		"Compound(first) -> errCompound",
		"Compound(second) -> errCompound",
		"Plain(argument) -> errPlain",
		"Wrapped(argument) -> errWrapped",
	}
	if !slices.Equal(found, want) {
		t.Errorf("the reader found\n %v\nand the control declares\n %v", found, want)
	}
}
