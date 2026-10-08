// The gate over what this package does when a construction that derives everything through a
// CryptoProvider is handed none.
//
// ErrNilCryptoProvider is declared for exactly that condition and, before this file, was
// returned from ONE of the twenty one declarations that take a provider. The other twenty
// raised a nil pointer dereference out of whatever line first read a width off it -- most of
// them the very first statement of the body. Measured, not supposed: SenderDataKeyNonce(nil,
// nil, nil) panicked with "invalid memory address or nil pointer dereference", and so did
// WelcomeKeyNonce, which has the same shape and shipped with the same omission. A panic out of
// a library is not a refusal: it takes the caller's process rather than its call, and it says
// nothing about which of the arguments was wrong.
//
// The class is derived twice over rather than listed. providerConstructions reads the package
// level half off the type checker and providerDrivenMethods reads the method half, and
// TestEveryDeclarationTakingAProviderIsHeldByExactlyOneOfTheTwoClasses compares the two
// against the whole, so a declaration cannot fall between them. What each member has to do is
// derived too: a signature with an error result must return ErrNilCryptoProvider, and one
// without has nothing to report with and must not answer at all.
package mls

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"
)

// providerNilMethodRow is one method of the class, with a call that hands it no provider.
//
// A method needs a receiver and a construction does not, which is the whole reason this half
// is a table and the other half is reflect over the function value. Every receiver here is the
// zero value or the package's own constructor for one: what the method does with a nil
// provider must not depend on what its receiver holds, and a row that had to build a valid
// receiver first would be asserting something else.
type providerNilMethodRow struct {
	name string
	call func() error
}

// providerNilMethodRows is the table, one row per member of the derived method class.
//
// It is not what decides the class: providerDrivenMethodRowsFor holds it against
// providerDrivenMethodNames in both directions, so a method with no row fails this gate rather
// than being left out of it.
func providerNilMethodRows() []providerNilMethodRow {
	return []providerNilMethodRow{
		{name: "(*PreSharedKeyId).Validate", call: func() error {
			return (&PreSharedKeyId{}).Validate(nil)
		}},
		{name: "(*TranscriptHashes).Update", call: func() error {
			hashes := InitialTranscriptHashes()
			return hashes.Update(nil, nil, nil)
		}},
		{name: "(*TranscriptHashes).SetFromGroupInfo", call: func() error {
			hashes := InitialTranscriptHashes()
			return hashes.SetFromGroupInfo(nil, nil, nil)
		}},
		// framing's proposal reference, on a ZERO valued AuthenticatedContent, which is the
		// receiver that separates the two orders this body could be written in. A zero
		// AuthenticatedContent carries content type 0, which is not a content type this package
		// registers, so a body that looked at its receiver before its provider would answer
		// ErrContentArmMismatch -- sending the caller to fix a message whose content type was
		// never the problem, over a provider it never passed.
		{name: "(*AuthenticatedContent).ProposalRef", call: func() error {
			_, err := (&AuthenticatedContent{}).ProposalRef(nil)
			return err
		}},
		// the leaf's two signature methods, on a zero valued receiver. The zero LeafNode
		// carries source 0, which is not a source this package reads, so a body that built
		// its preimage before it looked at the provider would answer ErrTreeMalformed here
		// -- a refusal for a reason the caller did not cause, over a provider it never
		// passed. The provider comes first in both, and this is what says so.
		{name: "(*LeafNode).Sign", call: func() error {
			return (&LeafNode{}).Sign(nil, nil, nil, 0)
		}},
		{name: "(*LeafNode).VerifySignature", call: func() error {
			return (&LeafNode{}).VerifySignature(nil, nil, 0)
		}},
		// key_package.go's two, on a ZERO valued key package, which is the receiver that
		// separates the two orders these bodies could be written in. A zero KeyPackage names
		// protocol version 0, so a Validate that judged its receiver before its provider would
		// answer ErrUnsupportedVersion -- sending the caller to fix a version it never chose,
		// over a provider it never passed -- and it carries a leaf whose CREDENTIAL TYPE is 0,
		// which the encoder Ref goes through refuses with errProfileCredentialType for the same
		// kind of reason: Credential.MarshalMLS refuses anything that is not basic before it
		// writes an octet, and the leaf is inside the structure Ref marshals.
		//
		// The credential and not the leaf source, and the refusal spelled out because this
		// project reads a justification comment as a claim. Measured on this tree:
		// (&KeyPackage{}).Ref(crypto) answers errProfileCredentialType, and
		// errors.Is(err, ErrTreeMalformed) is FALSE. The two rows were right; the reason
		// written above them named a refusal that does not happen.
		// TestTheZeroKeyPackageIsRefusedOnItsCredentialAndNotItsLeafSource is what holds this
		// paragraph to what the code does, so the next reader inherits a checked claim rather
		// than a plausible one.
		{name: "(*KeyPackage).Ref", call: func() error {
			_, err := (&KeyPackage{}).Ref(nil)
			return err
		}},
		{name: "(*KeyPackage).Validate", call: func() error {
			return (&KeyPackage{}).Validate(nil, 0, time.Time{})
		}},
		// the four tree hashes, on the ZERO valued tree rather than on NewRatchetTree's one
		// leaf tree. A tree of no nodes is the receiver that separates the two orders these
		// bodies could be written in: rootOf refuses a leaf width of zero with
		// ErrTreeMalformed and the node loop of TreeHashes never runs at all, so a body that
		// looked at its receiver before it looked at its provider answers "your tree is
		// malformed", or an empty slice and no error, to a caller whose actual mistake was
		// passing no provider.
		{name: "(*RatchetTree).treeHash", call: func() error {
			_, err := (&RatchetTree{}).treeHash(nil, 0, nil)
			return err
		}},
		{name: "(*RatchetTree).NodeTreeHash", call: func() error {
			_, err := (&RatchetTree{}).NodeTreeHash(nil, 0)
			return err
		}},
		{name: "(*RatchetTree).TreeHash", call: func() error {
			_, err := (&RatchetTree{}).TreeHash(nil)
			return err
		}},
		{name: "(*RatchetTree).TreeHashes", call: func() error {
			_, err := (&RatchetTree{}).TreeHashes(nil)
			return err
		}},
		// section 7.9's three, on the same zero valued tree and for the same reason. The
		// receiver separates the two orders more sharply here than it does above, because
		// every one of these three has a range check or a blank-node refusal that answers a
		// tree-shaped error: ParentHash refuses a parent index outside the tree with
		// ErrNodeIndexOutOfRange, and VerifyParentHashes refuses a leaf width of zero with
		// ErrTreeMalformed. A body that read its receiver first would answer either of those
		// to a caller whose actual mistake was passing no provider, and node 0 of a tree with
		// no nodes reaches both of them.
		{name: "(*RatchetTree).ParentHash", call: func() error {
			_, err := (&RatchetTree{}).ParentHash(nil, 0, 0)
			return err
		}},
		{name: "(*RatchetTree).parentHashClaimsUnder", call: func() error {
			_, err := (&RatchetTree{}).parentHashClaimsUnder(nil, &ParentNode{}, 0, 0, 0)
			return err
		}},
		{name: "(*RatchetTree).VerifyParentHashes", call: func() error {
			return (&RatchetTree{}).VerifyParentHashes(nil)
		}},
		// task 18's path generation, on the same zero valued tree and for the same reason. A
		// tree with no nodes has no leaf 0, so Leaf(sender) answers nil and a body that read its
		// receiver before its provider would answer ErrLeafIndexOutOfRange -- which sends the
		// caller to re-derive an index that was never the problem, over a provider it never
		// passed. It is also the one member of this class that MUTATES its receiver, so an order
		// that reached the tree first would blank a member's direct path on the way to reporting
		// a fault the caller could not have caused.
		{name: "(*RatchetTree).CreateUpdatePathSecrets", call: func() error {
			_, err := (&RatchetTree{}).CreateUpdatePathSecrets(nil, 0, nil, nil)
			return err
		}},
		// task 20's sealing, on the same zero valued tree. A tree with no nodes gives leaf 0
		// no filtered direct path to read, so EncryptionTargets refuses it with
		// ErrLeafIndexOutOfRange, and the nil plan one argument along is errNilUpdatePathPlan:
		// a body that looked at either before its provider would answer one of those to a
		// caller whose actual mistake was passing no provider, and send it to fix an index or a
		// plan that was never the problem.
		{name: "(*RatchetTree).EncryptUpdatePath", call: func() error {
			_, err := (&RatchetTree{}).EncryptUpdatePath(nil, nil, 0, nil, nil)
			return err
		}},
		// tasks 21 and 22's receiving half, on the zero valued tree for the reason task 18's
		// generating half is. A tree with no nodes gives leaf 0 no width to be inside, so
		// MergeUpdatePath's own range check answers ErrLeafIndexOutOfRange and
		// DecryptUpdatePath's filteredPathSteps answers the same -- and the nil path and the nil
		// private state one and two arguments along are refusals of their own. A body that looked
		// at any of the three before its provider would answer one of those to a caller whose
		// actual mistake was passing no provider, and send it to fix an index, a path or a state
		// that was never the problem. The merge is also the second member of this class that
		// MUTATES its receiver, so an order that reached the tree first would blank a member's
		// direct path on the way to reporting a fault the caller could not have caused.
		{name: "(*RatchetTree).MergeUpdatePath", call: func() error {
			return (&RatchetTree{}).MergeUpdatePath(nil, 0, nil)
		}},
		// p7 task 16's joiner ladder, on the zero valued tree for the reason the three above
		// are: filteredPathSteps refuses a leaf index outside a tree of no leaves with
		// ErrLeafIndexOutOfRange, so a body that read its receiver before its provider answers a
		// refusal about a leaf index the caller never chose.
		{name: "(*RatchetTree).installJoinerPathSecrets", call: func() error {
			return (&RatchetTree{}).installJoinerPathSecrets(nil, nil, 0, 0, nil)
		}},
		{name: "(*RatchetTree).DecryptUpdatePath", call: func() error {
			_, err := (&RatchetTree{}).DecryptUpdatePath(nil, 0, nil, nil, nil, nil)
			return err
		}},
		// treekem.go's two, on the zero valued private state, which is the receiver that
		// separates the two orders these bodies could be written in. A zero TreeKEMPrivate has
		// leaf index 0, so node 0 IS its own leaf: a NodePrivateKey that looked at its receiver
		// before its provider would answer a key and no error to a caller whose actual mistake
		// was passing no provider. Consistent reaches a nil tree at its first statement for the
		// same reason and would answer ErrPathSecretMismatch, which sends the caller to compare
		// an epoch it never got wrong.
		{name: "(*TreeKEMPrivate).NodePrivateKey", call: func() error {
			_, _, err := (&TreeKEMPrivate{}).NodePrivateKey(nil, 0)
			return err
		}},
		{name: "(*TreeKEMPrivate).Consistent", call: func() error {
			return (&TreeKEMPrivate{}).Consistent(nil, nil)
		}},
		// p7 task 6's cache, on the ZERO valued receiver, which is the one that separates the
		// orders these bodies could be written in. A zero ProposalCache holds no entry and no
		// epoch binding, so a Store that judged its ARGUMENT first would answer
		// errNilAuthenticatedContent -- sending the caller to fix a message it never passed,
		// over a provider it never passed either. Resolve is handed no vector for the matching
		// reason: an empty commit resolves to an empty list and no error at all, so the refusal
		// below is the provider's and can be nothing else.
		{name: "(*ProposalCache).Store", call: func() error {
			_, err := (&ProposalCache{}).Store(nil, nil, nil)
			return err
		}},
		{name: "(*ProposalCache).Resolve", call: func() error {
			_, err := (&ProposalCache{}).Resolve(nil, nil, 0, nil)
			return err
		}},
		// p7 task 14's group info pair, on a ZERO valued GroupInfo, which is the receiver that
		// separates the orders these bodies could be written in. A zero GroupInfo names signer
		// leaf 0 and carries an empty tree hash, so a Verify that judged its receiver against
		// its tree before it looked at its provider would answer ErrWelcomeTreeHashMismatch --
		// sending the caller to look for a fork over a provider it never passed.
		//
		// Both are handed a nil TREE as well, deliberately. Verify's tree is not one input among
		// several -- a GroupInfo carries no verification key, so the tree is the only thing that
		// can say who the members are -- and with a real tree here the gate would pass over a
		// body with no provider check at all, because (*RatchetTree).TreeHash makes the same
		// refusal one frame down. With no tree, only a body that reads its provider FIRST
		// answers ErrNilCryptoProvider; one that reads its tree first answers ErrTreeMalformed.
		{name: "(*GroupInfo).Sign", call: func() error {
			return (&GroupInfo{}).Sign(nil, nil)
		}},
		{name: "(*GroupInfo).Verify", call: func() error {
			return (&GroupInfo{}).Verify(nil, nil)
		}},
		// p7 task 15's welcome builder, on a ZERO valued staged commit -- the receiver that
		// separates the two orders this body could be written in. A zero StagedCommit added
		// nobody, so a body that judged its receiver before its provider answers nil and NO
		// ERROR AT ALL: a caller that passed no provider would be told its commit adds nobody,
		// which is true of the zero value and is not the fault it made.
		{name: "(*StagedCommit).welcomeMessage", call: func() error {
			_, err := (&StagedCommit{}).welcomeMessage(nil, nil)
			return err
		}},
		// and the door built on that verifier, which must make the same refusal for the same
		// reason: it is Verify and nothing else, so a provider check that stopped happening
		// there would stop happening here, and this row is what says the delegation is real
		// rather than a body that grew a check of its own beside it.
		{name: "(*GroupInfo).VerifiedContext", call: func() error {
			_, err := (&GroupInfo{}).VerifiedContext(nil, nil)
			return err
		}},
	}
}

// providerErrorResult is the position of the error a signature answers, or -1 for one that
// answers none.
//
// This is the derivation that decides what the gate demands of a member, and it is read off
// the compiled signature rather than off a table of exemptions. A construction that cannot
// report is exempt from reporting BECAUSE of its signature, and it stops being exempt the
// moment somebody gives it an error to return.
func providerErrorResult(signature reflect.Type) int {
	errorType := reflect.TypeOf((*error)(nil)).Elem()
	for i := 0; i < signature.NumOut(); i++ {
		if signature.Out(i) == errorType {
			return i
		}
	}
	return -1
}

// providerCallWithNoProvider calls one construction with the zero value of every parameter,
// which is a nil provider at the provider's position, and answers what came back or what it
// panicked with.
//
// Zero values everywhere and not just at the provider. A construction handed a nil provider
// and otherwise valid arguments would still be reading a length off the provider to check
// them, so there is no "otherwise valid" to hand: what the zero arguments make this gate
// assert is that the provider is refused BEFORE any argument is judged, which is the only
// order that does not dereference it.
func providerCallWithNoProvider(t *testing.T, name string, function reflect.Value) (results []reflect.Value, panicked any) {
	t.Helper()
	signature := function.Type()
	if signature.IsVariadic() {
		t.Fatalf("%s is variadic, so this gate cannot build its argument list", name)
	}
	arguments := make([]reflect.Value, signature.NumIn())
	for i := range arguments {
		arguments[i] = reflect.Zero(signature.In(i))
	}
	defer func() {
		panicked = recover()
	}()
	return function.Call(arguments), nil
}

// TestEveryDeclarationHandedANilProviderRefusesRatherThanDereferencingIt sweeps both halves of
// the class.
//
// What it demands of a member that can report an error is the sentinel this package declares
// for the condition, and not merely "some error": a length refusal raised because
// crypto.HashSize() answered zero would satisfy a bare err != nil while still having read a
// method off a nil interface, and an ErrSecretLength for a secret nobody could have got right
// sends the caller to check its arguments over a provider it never passed.
//
// What it demands of a member that CANNOT report is that it does not answer. Those three
// constructions hand back bytes and nothing else -- ZeroSecret, EmptyPskSecret and
// ConfirmedTranscriptHash -- so the only alternative to stopping is a plausibly shaped value
// derived from no provider at all, which is worse than a panic and is the outcome this half
// rules out. Their exemption from the sentinel is read off their signatures rather than
// written down here, so it lapses the moment one of them grows an error to return.
//
// IT HAS ALREADY LAPSED ONCE, which is why that sentence is worth keeping rather than tidying
// into the present tense: this paragraph named six, and RefHash and the two reference makers
// were three of them. They grew an error the day the labelled bound closed the last exported
// panic, and this gate moved them across on its own and failed until RefHash refused a nil
// provider the way every other reporting construction does.
func TestEveryDeclarationHandedANilProviderRefusesRatherThanDereferencingIt(t *testing.T) {
	reporting, silent := 0, 0
	for _, construction := range providerConstructions(t) {
		function := construction.bind(nil)
		at := providerErrorResult(function.Type())
		if at < 0 {
			// it has no way to say no, so what it must not do is say yes.
			_, panicked := providerCallWithNoProvider(t, construction.name, function)
			if panicked == nil {
				t.Errorf("%s answers no error and returned a value when handed no provider; every byte of that value was derived from nothing, and a caller cannot tell it from a real one",
					construction.name)
			}
			silent++
			continue
		}
		results, panicked := providerCallWithNoProvider(t, construction.name, function)
		if panicked != nil {
			t.Errorf("%s panicked with %v when handed no provider, and it answers an error at result %d; %v is what the caller has to be given",
				construction.name, panicked, at, ErrNilCryptoProvider)
			reporting++
			continue
		}
		answered, _ := results[at].Interface().(error)
		if !errors.Is(answered, ErrNilCryptoProvider) {
			t.Errorf("%s handed no provider answered %v, want %v", construction.name, answered, ErrNilCryptoProvider)
		}
		reporting++
	}
	if reporting == 0 || silent == 0 {
		t.Fatalf("the sweep read %d constructions that can report and %d that cannot; with either half empty this gate is holding one rule rather than the two it states",
			reporting, silent)
	}

	rows := providerNilMethodRowsFor(t, providerDrivenMethodNames(t))
	if len(rows) == 0 {
		t.Fatal("no method of this package was driven with a nil provider, so the method half of this gate demands nothing")
	}
	for _, row := range rows {
		answered := providerNilRefusalOf(t, row)
		if !errors.Is(answered, ErrNilCryptoProvider) {
			t.Errorf("%s handed no provider answered %v, want %v", row.name, answered, ErrNilCryptoProvider)
		}
	}
}

// providerNilRefusalOf runs one method row and turns a panic into the error it should have
// been, so one member dereferencing the nil interface is a failure of its own row rather than
// the end of the sweep.
func providerNilRefusalOf(t *testing.T, row providerNilMethodRow) (answered error) {
	t.Helper()
	defer func() {
		if panicked := recover(); panicked != nil {
			t.Errorf("%s panicked with %v when handed no provider", row.name, panicked)
			answered = nil
		}
	}()
	return row.call()
}

// providerNilMethodRowsFor holds this file's rows against the derived class in both directions.
//
// It is providerDrivenMethodRowsFor's check over this file's own row type: a member with no row
// is a method nothing here runs, and a row naming a method this package does not declare is a
// row that outlived what it covered.
func providerNilMethodRowsFor(t *testing.T, class []string) []providerNilMethodRow {
	t.Helper()
	byName := map[string]providerNilMethodRow{}
	for _, row := range providerNilMethodRows() {
		if _, repeated := byName[row.name]; repeated {
			t.Fatalf("providerNilMethodRows declares %s twice, so one of the two is never run", row.name)
		}
		byName[row.name] = row
	}
	for name := range byName {
		if !slices.Contains(class, name) {
			t.Errorf("providerNilMethodRows names %s, and no method of this package takes a %s under that name",
				name, providerInterfaceName)
		}
	}
	rows := []providerNilMethodRow{}
	for _, name := range class {
		row, written := byName[name]
		if !written {
			t.Errorf("the nil provider gate: %s is handed a %s and has no row, so nothing holds it",
				name, providerInterfaceName)
			continue
		}
		rows = append(rows, row)
	}
	return rows
}
