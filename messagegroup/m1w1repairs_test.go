// The defects batch C's review reproduced, each held by the property it escaped through.
//
// It is a file of its own for ratchetrepairs_test.go's reason: every case here is a REGRESSION
// with a measurement behind it, and a reader asking "what stopped this" should find the
// measurement beside the assertion rather than three files away.
package messagegroup

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/message"
)

// ---------------------------------------------------------------------------
// the epoch zero handle key: one parameter, one meaning, two branches
// ---------------------------------------------------------------------------

// repairEpochZeroRoot recomputes storage_root[0] from the handle's own exporter output, which is
// the only value outside this session that both branches of installEpochOnLoop can be judged
// against.
func repairEpochZeroRoot(t *testing.T, handle GroupHandle, pqSecret []byte) []byte {
	t.Helper()
	mlsSecret, err := handle.Export(mlsSecretLabel, nil, mlsSecretBytes)
	if err != nil {
		t.Fatalf("export the epoch's mls_secret: %v", err)
	}
	return StorageRoot(mlsSecret, pqSecret)
}

// installEpochOnLoop took its argument VERBATIM as group_handle_key on one branch and expanded a
// root through GroupHandleKey on the other, while the parameter's name and the constructor's doc
// both said "storage root". Both values are thirty two octets, so nothing refused the
// disagreement.
//
// Reproduced by the review: a session restored at epoch 1 with storage_root[0] answered
// sender_handle dc272587... where the group computes 3e774ae1..., so the device's records carry a
// handle no peer computes and no peer's ReceiverRatchetKey matches.
//
// The case holds the property from BOTH sides, because either branch alone can be made to agree
// with a wrong reading of the other. The epoch zero branch must expand -- the handle key is
// GroupHandleKey(storage_root[0]) and is NOT storage_root[0] -- and the restore branch must accept
// exactly what the epoch zero branch produced. A fix that made both branches take the argument
// verbatim passes the second assertion and fails the first.
func TestTheEpochZeroHandleKeyIsTheSameKindOfValueOnBothBranches(t *testing.T) {
	fixture := newTestSession(t, "handle-key-branches")
	root0 := repairEpochZeroRoot(t, fixture.handle, testPqSecret())
	want := GroupHandleKey(root0)

	// the epoch zero branch EXPANDS, and the two candidate values are distinguishable
	if bytes.Equal(want, root0) {
		t.Fatal("GroupHandleKey answers its own argument, so this case cannot tell the root from its expansion and neither could the defect")
	}
	if !bytes.Equal(fixture.session.groupHandleKey, want) {
		t.Errorf("a session opened at epoch 0 holds group_handle_key %x and GroupHandleKey(storage_root[0]) is %x",
			fixture.session.groupHandleKey, want)
	}
	if bytes.Equal(fixture.session.groupHandleKey, root0) {
		t.Error("a session opened at epoch 0 holds storage_root[0] itself as its group handle key, unexpanded")
	}
	senderHandle, err := fixture.session.SenderHandle()
	if err != nil {
		t.Fatalf("SenderHandle: %v", err)
	}
	if senderHandle != SenderHandle(want, fixture.handle.OwnLeafIndex()) {
		t.Errorf("the session routes on %x and the group computes %x", senderHandle,
			SenderHandle(want, fixture.handle.OwnLeafIndex()))
	}

	// and the OTHER branch takes exactly that value. A second session over the same handle, at the
	// same epoch, handed what the first one persisted, computes the same handle.
	restored, err := NewGroupSession(fixture.handle, testPqSecret(), want, newStreamIndexMemory(),
		testClock(), testServerNonce())
	if err != nil {
		t.Fatalf("a session restored with the persisted group handle key: %v", err)
	}
	defer restored.Close()
	restoredHandle, err := restored.SenderHandle()
	if err != nil {
		t.Fatalf("SenderHandle of the restored session: %v", err)
	}
	if restoredHandle != senderHandle {
		t.Errorf("a session restored from what the constructor's doc says to persist routes on %x and the original routes on %x; the two branches of the install disagree about what the parameter is",
			restoredHandle, senderHandle)
	}
	if !bytes.Equal(restored.groupHandleKey, want) {
		t.Errorf("the restored session holds %x as its group handle key, want the %x it was handed",
			restored.groupHandleKey, want)
	}
}

// A persisted value comes out of durable storage, so a wrong width is its plausible shape --
// GroupHandleKey's own comment is written about "a root of sixty four octets, which is the
// plausible shape of a value decoded out of durable storage". It used to reach SenderHandle, whose
// refusal is a PANIC carrying the sentinel, on the caller's goroutine, out of a constructor whose
// every other refusal is a typed error.
func TestAPersistedGroupHandleKeyOfTheWrongWidthIsATypedRefusalAndNotAPanic(t *testing.T) {
	engine := newTestEngine(t)
	handle := engine.createGroup(t, "wide-handle-key")
	defer handle.Close()
	for _, width := range []int{1, 16, 31, 33, 64} {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Errorf("a %d octet persisted group handle key panicked out of the constructor with %v; every other refusal there is a typed error",
						width, recovered)
				}
			}()
			session, err := NewGroupSession(handle, testPqSecret(), make([]byte, width),
				newStreamIndexMemory(), testClock(), testServerNonce())
			if !errors.Is(err, ErrGroupHandleKeyLength) {
				t.Errorf("a %d octet persisted group handle key answered %v, want ErrGroupHandleKeyLength", width, err)
			}
			if session != nil {
				session.Close()
			}
		}()
	}
}

// Epoch() swallowed ErrSessionClosed and answered 0, which is the epoch every group spends its
// first commit in -- so "closed" and "epoch 0" were the same answer over exactly the value a
// caller is most likely to meet.
func TestAClosedSessionsEpochIsARefusalRatherThanEpochZero(t *testing.T) {
	fixture := newTestSession(t, "closed-epoch")
	epoch, err := fixture.session.Epoch()
	if err != nil {
		t.Fatalf("Epoch on an open session: %v", err)
	}
	if epoch != 0 {
		t.Fatalf("a fresh session is at epoch %d, want 0; this case cannot tell a closed session's answer from an open one's unless the open one is 0", epoch)
	}
	if err := fixture.session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closedEpoch, err := fixture.session.Epoch()
	if !errors.Is(err, ErrSessionClosed) {
		t.Errorf("a closed session answered epoch %d and error %v, want ErrSessionClosed", closedEpoch, err)
	}
}

// ---------------------------------------------------------------------------
// the storage root actually depends on the pq_secret it is given
// ---------------------------------------------------------------------------

// The constructor REFUSES an empty pq_secret and its doc argues that a default "would produce a
// perfectly good storage root, both clients would agree, every test would pass, and the PQ half of
// the design would be silently gone" -- and that is exactly what dropping the value one line
// inside the function the guard protects did. Measured: StorageRoot(mlsSecret, self.pqSecret) ->
// StorageRoot(mlsSecret, mlsSecret) survived all three trees.
//
// Two halves, because either alone is weak. The derivation is recomputed from the handle's own
// exporter output and the secret that was injected, which pins WHICH value goes in; and two
// sessions differing in nothing but the pq_secret must not agree, which is this project's own
// question about an injected secret -- "does the value actually depend on the source?"
func TestTheStorageRootDependsOnTheInjectedPqSecret(t *testing.T) {
	engine := newTestEngine(t)
	handle := engine.createGroup(t, "pq-dependence")
	defer handle.Close()

	first, err := NewGroupSession(handle, testPqSecret(), nil, newStreamIndexMemory(),
		testClock(), testServerNonce())
	if err != nil {
		t.Fatalf("NewGroupSession: %v", err)
	}
	defer first.Close()
	if want := repairEpochZeroRoot(t, handle, testPqSecret()); !bytes.Equal(first.storageRoot, want) {
		t.Errorf("the session's storage root is %x and HKDF-Extract(salt = mls_secret, ikm = pq_secret) is %x; the injected secret is not the one the root is extracted with",
			first.storageRoot, want)
	}

	other := make([]byte, 32)
	for i := range other {
		other[i] = byte(0x5C ^ i)
	}
	if bytes.Equal(other, testPqSecret()) {
		t.Fatal("the two pq_secrets of this case are equal, so it would pass against a session that ignored the argument")
	}
	second, err := NewGroupSession(handle, other, nil, newStreamIndexMemory(),
		testClock(), testServerNonce())
	if err != nil {
		t.Fatalf("NewGroupSession with a second pq_secret: %v", err)
	}
	defer second.Close()
	if bytes.Equal(first.storageRoot, second.storageRoot) {
		t.Errorf("two sessions over ONE group handle at ONE epoch, differing only in pq_secret, derived the same storage root %x; the post quantum half of the derivation is not in it",
			first.storageRoot)
	}
	// and the difference reaches the wire, rather than stopping at a field nothing reads
	firstHandle, err := first.SenderHandle()
	if err != nil {
		t.Fatalf("SenderHandle: %v", err)
	}
	secondHandle, err := second.SenderHandle()
	if err != nil {
		t.Fatalf("SenderHandle: %v", err)
	}
	if firstHandle == secondHandle {
		t.Errorf("both sessions route on %x, so the pq_secret reaches no value a peer or a server can see", firstHandle)
	}
}

// mls_secret's exporter label is the one wire visible constant the whole key schedule is founded
// on -- storage_root, the three class keys, the ladder and both record aeads all hang off it --
// and it was pinned by nothing: changing "URmessage/v1/storage" by one character survived all
// three trees. Every other label in the package has a KAT.
//
// The literal is written out here rather than referenced, which is the whole point: a test that
// compared the constant against itself would pass against any drift at all. Two clients disagreeing
// here derive different storage roots and nothing they exchange ever opens.
func TestTheMlsSecretExporterLabelIsPinned(t *testing.T) {
	const fromMaster = "URmessage/v1/storage"
	if mlsSecretLabel != fromMaster {
		t.Errorf("this package exports mls_secret under %q and MASTER section 7 fixes %q", mlsSecretLabel, fromMaster)
	}
	if mlsSecretBytes != 32 {
		t.Errorf("this package exports %d octets of mls_secret and MASTER section 7 gives 32", mlsSecretBytes)
	}
	// and the label is load bearing rather than decorative: a neighbouring one answers other bytes
	fixture := newTestSession(t, "exporter-label")
	under, err := fixture.handle.Export(fromMaster, nil, mlsSecretBytes)
	if err != nil {
		t.Fatalf("export under the pinned label: %v", err)
	}
	drifted, err := fixture.handle.Export(fromMaster+"X", nil, mlsSecretBytes)
	if err != nil {
		t.Fatalf("export under a drifted label: %v", err)
	}
	if bytes.Equal(under, drifted) {
		t.Error("two exporter labels one character apart answer the same octets, so this case could not see a drifted constant even if it were pinned")
	}
}

// ---------------------------------------------------------------------------
// the adapter's exporter is the group's own
// ---------------------------------------------------------------------------

// Nothing distinguished the real RFC 9420 exporter from a second assembly. Measured: replacing the
// delegation with a loop over the group's epoch authenticator XORed with the label, the index and
// len(context) survived every test in all three trees -- no new imports, every parameter read, so
// neither the production import pin nor mls's stub shape gate fired.
//
// The record layer's whole key schedule hangs off this one call, which is what makes it the seam a
// second assembly costs the most at. What this case asserts is the DELEGATION: the adapter answers
// what the group it wraps answers, for labels, contexts and lengths that differ in each of the
// three arguments. It deliberately does not re-derive RFC 9420 section 8.5 here -- connect/mls's
// own suite holds its exporter against the RFC, and a second derivation in this package would be
// the very thing this case exists to refuse.
func TestTheAdaptersExporterIsTheGroupsOwnAndNotASecondAssembly(t *testing.T) {
	fixture := newTestSession(t, "exporter-delegation")
	adapter, isAdapter := fixture.handle.(*connectMlsHandle)
	if !isAdapter {
		t.Fatalf("the fixture's handle is %T and not the connect/mls adapter, so this case is judging something else", fixture.handle)
	}
	cases := []struct {
		label   string
		context []byte
		length  int
	}{
		{mlsSecretLabel, nil, mlsSecretBytes},
		{mlsSecretLabel, []byte("a"), mlsSecretBytes},
		{mlsSecretLabel, []byte("b"), mlsSecretBytes},
		{mlsSecretLabel + "/other", nil, mlsSecretBytes},
		{mlsSecretLabel, nil, 16},
		{mlsSecretLabel, nil, 64},
	}
	answers := map[string][]byte{}
	for _, one := range cases {
		want, err := adapter.group.Export(one.label, one.context, one.length)
		if err != nil {
			t.Fatalf("the group's own exporter refused %q/%x/%d: %v", one.label, one.context, one.length, err)
		}
		got, err := adapter.Export(one.label, one.context, one.length)
		if err != nil {
			t.Fatalf("the adapter refused %q/%x/%d: %v", one.label, one.context, one.length, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("the adapter answers %x for %q/%x/%d and the group it wraps answers %x; the record layer's whole key schedule hangs off this call",
				got, one.label, one.context, one.length, want)
		}
		if len(got) != one.length {
			t.Errorf("the adapter answered %d octets for a length of %d", len(got), one.length)
		}
		answers[string(got)] = got
	}
	// six calls differing in the label, in the context and in the length answer six values, so a
	// body that read fewer of its arguments than it declares is visible here as well as in mls's
	// own stub shape gate.
	if len(answers) != len(cases) {
		t.Errorf("%d calls differing in one argument each answered %d distinct values", len(cases), len(answers))
	}
	// and it is not a function of the one value section 6 publishes beside it
	authenticator := adapter.EpochAuthenticator()
	if len(authenticator) == 0 {
		t.Fatal("the group answers no epoch authenticator, so the negative below is about nothing")
	}
	for _, answer := range answers {
		if bytes.Equal(answer, authenticator) {
			t.Error("an exporter answer is the epoch authenticator itself")
		}
	}
}

// ---------------------------------------------------------------------------
// the construction order inside this package
// ---------------------------------------------------------------------------

// Section 5.2's title is "Construction order is a type, not a convention", and it was a type only
// ACROSS the package boundary. Inside it the four stages wrapped one shared message.RecordHeader
// and bindBodyHash's whole effect was a mutation of it, so an in-package assembly of a
// recordBodyBound over an unbound header sealed the head first and produced a record
// message.EncodeRecord accepted with body_hash all zero.
//
// The stage carries the hash now, and the stage after it refuses one that is not the hash of the
// ct_body it wraps. This case builds the skipped shape the review built and asserts the refusal;
// without it the same statements answer a record.
func TestTheHeadCannotBeSealedBeforeTheBodyHashIsBound(t *testing.T) {
	fixture := newTestSession(t, "stage-order")
	bodyPlain := []byte("the body this record carries")
	var builder *recordBuilder
	var err error
	if postErr := fixture.session.do(func() {
		builder, err = fixture.session.newRecordBuilderOnLoop(message.RetentionDurable, 0, 0, false,
			[]byte("the head this record carries"), bodyPlain, 0, nil)
	}); postErr != nil {
		t.Fatalf("post the builder command: %v", postErr)
	}
	if err != nil {
		t.Fatalf("newRecordBuilderOnLoop: %v", err)
	}
	defer builder.zeroize()
	bodySealed, err := builder.sealBody()
	if err != nil {
		t.Fatalf("sealBody: %v", err)
	}

	// the shape the review assembled: the stage after the body, with the stage between them
	// skipped. It is a keyed composite literal of an unexported type, which is legal go inside
	// this package and is what tasks 13 to 16 can write.
	skipped := &recordBodyBound{sealed: bodySealed}
	if _, err := skipped.sealHead([]byte("a head sealed before the body was bound")); !errors.Is(err, ErrRecordStageOrder) {
		t.Errorf("sealing the head over a stage that bound no body hash answered %v, want ErrRecordStageOrder", err)
	}
	// and one carrying the WRONG hash is the same refusal, so the check is over the value and not
	// over whether the field was written at all
	wrong := &recordBodyBound{sealed: bodySealed, bodyHash: sha256.Sum256([]byte("some other ciphertext"))}
	if _, err := wrong.sealHead([]byte("a head over a hash of something else")); !errors.Is(err, ErrRecordStageOrder) {
		t.Errorf("sealing the head over a stage carrying the wrong body hash answered %v, want ErrRecordStageOrder", err)
	}

	// the real chain still runs, and the header a reader meets carries the hash the stage produced
	bound := bodySealed.bindBodyHash()
	headSealed, err := bound.sealHead([]byte("the head"))
	if err != nil {
		t.Fatalf("sealHead over the bound stage: %v", err)
	}
	record, err := headSealed.authenticate()
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if record.Header.BodyHash != sha256.Sum256(record.CtBody) {
		t.Errorf("the finished record's body_hash is %x and H(ct_body) is %x", record.Header.BodyHash,
			sha256.Sum256(record.CtBody))
	}
	if record.Header.BodyHash == ([32]byte{}) {
		t.Error("the finished record's body_hash is all zero, which is the value the skipped stage produced")
	}
}

// The other half of the same property, read off the source: a later stage is reachable only
// through the method of the one before it.
//
// The scope question (R3a): the SCOPE is this package's production source, because the staging
// types are unexported and no other package can name one. The CLASS is every composite literal of
// a staging type, where "a staging type" is derived by walking the same chain the ordering gate
// walks -- from whatever newRecordBuilderOnLoop answers, along each stage's single transition --
// and never from a list of names written here. The assertion is that each stage after the first
// is built in exactly one function, and that that function is its predecessor's transition.
func TestEveryStageOfTheSealChainIsBuiltOnlyByItsPredecessor(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	chain, producer := repairStageChain(t, sources)
	if len(chain) < 3 {
		t.Fatalf("the stage chain read off the source is %v; there is nothing here to hold", chain)
	}
	built := map[string][]string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				literal, isLiteral := node.(*ast.CompositeLit)
				if !isLiteral {
					return true
				}
				named, isNamed := literal.Type.(*ast.Ident)
				if !isNamed || !slices.Contains(chain, named.Name) {
					return true
				}
				built[named.Name] = append(built[named.Name], function.Name.Name)
				return true
			})
		}
	}
	for at, stage := range chain {
		if stage == "Record" {
			continue
		}
		sites := built[stage]
		if len(sites) == 0 {
			t.Errorf("no production declaration builds a %s, so the chain has a stage nothing produces", stage)
			continue
		}
		if len(sites) != 1 {
			t.Errorf("%s is built in %v; a second construction site is a second door into the middle of the order, which is what makes it a convention again",
				stage, sites)
			continue
		}
		if at == 0 {
			continue
		}
		if want := producer[stage]; sites[0] != want {
			t.Errorf("%s is built in %s and the transition that answers it is %s; a stage built anywhere but in its predecessor's method is a stage the order does not gate",
				stage, sites[0], want)
		}
	}
	// AND ONE WRITER OF body_hash, which is the sentence seal.go now makes about sealHead. The
	// stage before it produces the value and this stage is where the header receives it; a second
	// writer anywhere would put the field back to being whatever the last statement left there,
	// which is the shape the skipped stage exploited.
	writers := []string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				assignment, isAssignment := node.(*ast.AssignStmt)
				if !isAssignment {
					return true
				}
				for _, target := range assignment.Lhs {
					if selector, isSelector := target.(*ast.SelectorExpr); isSelector &&
						selector.Sel.Name == "BodyHash" {
						writers = append(writers, function.Name.Name)
					}
				}
				return true
			})
		}
	}
	if len(writers) != 1 {
		t.Errorf("%v write a header's body_hash; there is one stage that has the value and one place the header receives it, and a second writer is what makes the field whatever the last statement left there",
			writers)
	} else if receiver := producer[chain[len(chain)-2]]; writers[0] != receiver {
		t.Errorf("body_hash is written in %s and the stage that answers %s is %s; the writer is the stage that was handed the value",
			writers[0], chain[len(chain)-2], receiver)
	}
	t.Logf("stage chain %v built at %v, body_hash written in %v", chain, built, writers)
}

// repairStageChain walks the seal chain the way the ordering gate does and answers, beside it, the
// method that answers each stage.
func repairStageChain(t *testing.T, sources []messagegroupSource) ([]string, map[string]string) {
	t.Helper()
	answers := map[string][]string{}
	producer := map[string]string{}
	start := ""
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Type.Results == nil || len(function.Type.Results.List) == 0 {
				continue
			}
			answered := sealPointerResultName(function.Type.Results.List[0].Type)
			if function.Name.Name == "newRecordBuilderOnLoop" {
				start = answered
			}
			receiver := sessionReceiverName(function)
			if receiver == "" || answered == "" || answered == receiver {
				continue
			}
			answers[receiver] = append(answers[receiver], answered)
			producer[answered] = function.Name.Name
		}
	}
	if start == "" {
		t.Fatal("nothing named newRecordBuilderOnLoop answers a staging type, so this gate has no chain to walk")
	}
	chain := []string{start}
	for {
		last := chain[len(chain)-1]
		if len(answers[last]) != 1 {
			return chain, producer
		}
		next := answers[last][0]
		if slices.Contains(chain, next) {
			t.Fatalf("the stage chain returns to %s, so it is a cycle rather than an order", next)
		}
		chain = append(chain, next)
		if next == "Record" {
			return chain, producer
		}
	}
}

// ---------------------------------------------------------------------------
// what the record layer does NOT authenticate
// ---------------------------------------------------------------------------

// THIS CASE IS NARROWED AND NOT DELETED, and the narrowing is the whole of what MASTER section 8.4
// bought and did not buy.
//
// WHAT IT USED TO ASSERT, verbatim: "a member seals a record attributed to a leaf it does not own
// and every other member opens it." That was true and is not: TestOneMemberCannotForgeAMessageFromAnother
// in mlsframe_test.go is the same construction refused, by name, at the sender binding. An
// unchanged case here would be asserting something no longer true.
//
// WHAT SURVIVES IS THE ENVELOPE, and it survives because nothing in the ruling touched it. Every
// symbol below is still one any member holds -- the class key expands from the storage root every
// member derives, RecordKeyZero takes a leaf index as an INPUT rather than as a credential,
// SenderHandle likewise, and write_auth is a mac under a group wide key -- so a member still
// assembles a record at another member's handle and another member's next stream index that
// message.EncodeRecord accepts and message.VerifyWriteAuth verifies. Every check the SERVER makes
// passes. What fails is downstream of the server, at an opener, where the body refuses.
//
// SO THE RESIDUE IS A DENIAL AND NOT A FORGERY, and that is ledger open item 205: a server that
// accepts this record has advanced last_stream_index for that (group_id, sender_handle), and its
// own monotonicity rule then refuses the true sender's next write. The true sender is squatted out
// of its own stream by a record nobody will ever read. This case is that sentence, so the day the
// envelope stops being forgeable -- which needs something the body-only ruling explicitly did not
// do -- this is the paragraph that fails.
func TestAnyMemberCanStillSquatAnotherLeafsStreamIndex(t *testing.T) {
	fixture := newTestSession(t, "index-squatting")
	const otherLeaf = uint32(3)
	if fixture.handle.OwnLeafIndex() == otherLeaf {
		t.Fatalf("this device owns leaf %d, which is the leaf this case squats", otherLeaf)
	}
	if err := fixture.session.TrackSender(otherLeaf, message.RetentionDurable, 0, 0, 0); err != nil {
		t.Fatalf("TrackSender for a leaf this device does not own: %v", err)
	}
	record := repairForgeRecord(t, fixture.session, otherLeaf, 0,
		[]byte("squatted head"), []byte("sealed by another member"))

	// THE SERVER'S WHOLE CHECK, and it passes. repairForgeRecord already required
	// message.EncodeRecord to accept the record, which is the codec's half; this is the mac,
	// which is the half that is supposed to say who may write.
	if !message.VerifyWriteAuth(fixture.session.writeKey, fixture.session.serverNonce, record) {
		t.Fatal("the squatted record's write_auth does not verify, so a server would refuse it and this case is about nothing")
	}
	if record.Header.SenderHandle != SenderHandle(fixture.session.groupHandleKey, otherLeaf) {
		t.Fatal("the squatted record does not carry the other leaf's sender_handle")
	}

	// AND THE OPENER REFUSES IT, which is the half MASTER section 8.4 closed. The two clauses
	// together are the finding: the record reaches the stream and never reaches a reader.
	headPlain, bodyPlain, err := fixture.session.OpenRecord(record)
	if err == nil {
		t.Fatalf("a record attributed to a leaf its writer does not own OPENED, to %q/%q; MASTER section 8.4's sender binding is what refuses it and mlsframe_test.go is where that is asserted by name",
			headPlain, bodyPlain)
	}
	if headPlain != nil || bodyPlain != nil {
		t.Errorf("the refusal returned %d octets of head and %d of body", len(headPlain), len(bodyPlain))
	}
	t.Logf("the envelope is still forgeable and the body is not: a server accepts this record (write_auth verifies, the codec accepts it) and every opener refuses it -- %v. Ledger open item 205: the true sender's own next write is then refused for a stream index it never used",
		err)
}

// repairLiftFrame takes the inner MLS frame back out of a record its sender really sealed.
//
// It is the attacker's half of re-enveloping, and it needs nothing an ordinary member does not
// hold: the record key is RecordKeyZero(class_key, leaf) walked to the record's own stream index,
// and the class key is group shared by construction. That is the same sentence repairForgeRecord
// makes from the sealing side, read backwards, which is why the two live together.
func repairLiftFrame(t *testing.T, session *GroupSession, leaf uint32, record *message.Record) []byte {
	t.Helper()
	recordKey := RecordKeyZero(append([]byte(nil), session.classKeys.Durable...), leaf)
	for walked := uint64(0); walked < record.Header.StreamIndex; walked += 1 {
		recordKey = stepRecordKey(recordKey)
	}
	defer zeroize(recordKey)
	aadBody, err := message.AADBody(RecordAeadAlgId, record.Header.BodyBinding())
	if err != nil {
		t.Fatalf("AADBody: %v", err)
	}
	bodyKey, bodyNonce := RecordAeadBody(recordKey)
	defer zeroize(bodyKey)
	defer zeroize(bodyNonce)
	padded, err := openRecordAead(bodyKey, bodyNonce, aadBody, record.CtBody)
	if err != nil {
		t.Fatalf("a member could not open the record it is lifting a frame out of: %v", err)
	}
	frame, err := unpadBody(record.Header.SizeBucket, padded)
	if err != nil {
		t.Fatalf("unpadBody: %v", err)
	}
	return frame
}

// repairForgeRecord seals one record for any leaf, out of the symbols a group member holds.
//
// It is the open path's own inputs assembled by hand, which is what makes both the sender
// authentication case above and the two refusal cases below writable: SealRecord will produce a
// record only for THIS device's leaf, only under DURABLE and never on the blob rung, so a case
// about any of those three has to build the record rather than ask for one.
func repairForgeRecord(t *testing.T, session *GroupSession, leaf uint32, streamIndex uint64,
	headPlain []byte, bodyPlain []byte) *message.Record {

	t.Helper()
	return repairForgeRecordArm(t, session, leaf, streamIndex, false, nil, headPlain, bodyPlain)
}

// repairForgeRecordArm is repairForgeRecord with MASTER section 8.4.1's arm chosen by the caller.
//
// The arm is a PARAMETER here for the same reason it is not one in production: the two fields that
// pick it are sealed under a key every member derives, so choosing them is exactly what an attacker
// can do, and a case about the arm has to be able to do it too. attachment is the ENCODED server
// attachment, which is what the production predicate reads.
func repairForgeRecordArm(t *testing.T, session *GroupSession, leaf uint32, streamIndex uint64,
	isCommit bool, attachment []byte, headPlain []byte, bodyPlain []byte) *message.Record {

	t.Helper()
	classKey := append([]byte(nil), session.classKeys.Durable...)
	recordKey := RecordKeyZero(classKey, leaf)
	for walked := uint64(0); walked < streamIndex; walked += 1 {
		recordKey = stepRecordKey(recordKey)
	}
	defer zeroize(recordKey)
	bucket, err := bucketForBody(len(bodyPlain))
	if err != nil {
		t.Fatalf("bucketForBody: %v", err)
	}
	if attachment == nil {
		attachment, err = message.EncodeServerAttachment(nil)
		if err != nil {
			t.Fatalf("EncodeServerAttachment: %v", err)
		}
	}
	header := message.RecordHeader{
		GroupId:          session.groupId,
		SenderHandle:     SenderHandle(session.groupHandleKey, leaf),
		Epoch:            session.epoch,
		StreamIndex:      streamIndex,
		IsCommit:         isCommit,
		RetentionClass:   message.RetentionDurable,
		SizeBucket:       bucket,
		ServerAttachment: attachment,
	}
	padded, err := padBody(bucket, bodyPlain)
	if err != nil {
		t.Fatalf("padBody: %v", err)
	}
	aadBody, err := message.AADBody(RecordAeadAlgId, header.BodyBinding())
	if err != nil {
		t.Fatalf("AADBody: %v", err)
	}
	bodyKey, bodyNonce := RecordAeadBody(recordKey)
	defer zeroize(bodyKey)
	defer zeroize(bodyNonce)
	ctBody, err := sealRecordAead(bodyKey, bodyNonce, aadBody, padded)
	if err != nil {
		t.Fatalf("seal ct_body: %v", err)
	}
	header.BodyHash = sha256.Sum256(ctBody)
	aadHead, err := message.AADHead(RecordAeadAlgId, &header, header.ServerAttachment)
	if err != nil {
		t.Fatalf("AADHead: %v", err)
	}
	headKey, headNonce := RecordAeadHead(recordKey)
	defer zeroize(headKey)
	defer zeroize(headNonce)
	ctHead, err := sealRecordAead(headKey, headNonce, aadHead, headPlain)
	if err != nil {
		t.Fatalf("seal ct_head: %v", err)
	}
	record := &message.Record{Header: header, CtHead: ctHead, CtBody: ctBody}
	record.WriteAuth = message.ComputeWriteAuth(session.writeKey, session.serverNonce,
		&record.Header, record.CtHead, record.Header.ServerAttachment)
	if _, err := message.EncodeRecord(record); err != nil {
		t.Fatalf("the forged record is not one the codec accepts: %v", err)
	}
	return record
}

// OpenRecord's refusals were dead to the suite: deleting one left everything green, because the
// only case that could produce a non-DURABLE or a blob record drives SealRecord, and SealRecord
// refused both. The report's claim "Non-DURABLE is refused pending M1-6 on both arms" -- M1-6 was
// ruled 2026-09-07 and reversed 2026-09-13, and that claim predates both -- was held on
// the seal arm only, and that test's own comment names the hazard: "a refusal tested on one arm is
// a refusal tested on half of itself".
//
// The other arm is writable, in about thirty lines, from exported symbols -- so it is written.
//
// WHAT THIS CASE ASKS CHANGED WITH THE 2026-09-13 RULING AND THE SHAPE DID NOT. It used to assert
// that OpenRecord refuses a PERMANENT and a MEDIA record with the blanket class sentinel. Ledger
// item 152 is ruled and that refusal is lifted in full, so those two are no longer refusals at
// all and asserting them would be asserting the reversed rule. What remains on this arm is the
// blob rung, which is still task 20's, and what ARRIVES on it is the two refusals the ruling
// created: an EPH record at a session holding no eph_root, and an EPH record whose window is more
// than one ahead of this opener's clock. Both are reached the same way this case always reached
// the class arm -- by moving one header field of a record that opens as built.
func TestOpenRecordRefusesTheBlobRungAndTheTwoEphValuesTheRulingCreated(t *testing.T) {
	fixture := newTestSession(t, "open-refusals")
	fixture.trackOwn(t)
	// THE CONTROL IS A COMMIT RECORD SINCE MASTER SECTION 8.4, and the substitution is what this
	// case's own subject needs rather than a convenience. Every refusal below is taken BEFORE the
	// body is opened -- the blob rung and the ahead window before any key is derived, the missing
	// ladder at the ratchet -- so what the control has to be is a record this session can open at
	// all, and after 2026-09-15 that is a record with no application frame in it (MASTER section
	// 8.4.1's first row; open item MG-4 is the unruled half). A control that no longer opened
	// would make every refusal below unfalsifiable.
	//
	// AND IT GOES THROUGH OpenCeremonyRecord, which is the arm split of the second pass: a record
	// with no inner frame is not what the message door serves. Every refusal below is still taken
	// through the same openRecordOnLoop body, which is what keeps this case about the header
	// fields rather than about the door.
	genuine, err := fixture.session.SealRecord(message.RetentionDurable, 0, true,
		[]byte("head"), []byte("body"), 0, nil)
	if err != nil {
		t.Fatalf("SealRecord of the control: %v", err)
	}
	// as built, this record opens. Every case below is that record with one header field moved,
	// so a refusal below is about the field and not about the fixture.
	if _, _, err := fixture.session.OpenCeremonyRecord(genuine); err != nil {
		t.Fatalf("the control record does not open: %v", err)
	}

	blob := *genuine
	blob.Header.SizeBucket = message.SizeBucketBlob
	blob.Header.BlobId = make([]byte, 32)
	if _, _, err := fixture.session.OpenCeremonyRecord(&blob); !errors.Is(err, ErrBlobRecordUnsupported) {
		t.Errorf("OpenRecord of a record on the blob rung answered %v, want ErrBlobRecordUnsupported", err)
	}

	// an EPH record at a session that holds no eph_root, and WHERE that refusal lives is worth
	// writing down because it is not where a reader would guess. OpenRecord never derives a
	// class key: it reads a receiver ratchet the caller installed earlier, so a session with no
	// eph_root has no eph ratchet to find and the refusal it meets on this path is
	// ErrNoReceiverRatchet. ErrNoEphRoot is what TrackSender answers, which is the call that
	// would have derived the key -- so the eph_root requirement on the OPEN side is a
	// requirement on installing the ladder, and both halves are asserted rather than one.
	noRoot := *genuine
	noRoot.Header.RetentionClass = message.RetentionEph
	noRoot.Header.EphBucket = 1
	noRoot.Header.EphWindow = ephWindowNow(t, 1)
	if _, _, err := fixture.session.OpenCeremonyRecord(&noRoot); !errors.Is(err, ErrNoReceiverRatchet) {
		t.Errorf("OpenRecord of an EPH record at a session with no eph ladder answered %v, want ErrNoReceiverRatchet", err)
	}
	if err := fixture.session.TrackSender(fixture.handle.OwnLeafIndex(), message.RetentionEph, 1,
		ephWindowNow(t, 1), 0); !errors.Is(err, ErrNoEphRoot) {
		t.Errorf("TrackSender of an eph ladder at a session with no eph_root answered %v, want ErrNoEphRoot", err)
	}

	// and the same record with a window far in the future, at a session that DOES hold a root,
	// which is the refusal spec A section 5.3 requires an opener to make.
	fixture.installEphRoot(t)
	ahead := *genuine
	ahead.Header.RetentionClass = message.RetentionEph
	ahead.Header.EphBucket = 1
	ahead.Header.EphWindow = 1 << 40
	if _, _, err := fixture.session.OpenCeremonyRecord(&ahead); !errors.Is(err, ErrEphWindowAhead) {
		t.Errorf("OpenRecord of an EPH record a trillion windows in the future answered %v, want ErrEphWindowAhead", err)
	}
}

// ---------------------------------------------------------------------------
// ruling A1 at the session's own call site: one class blind counter per sender
// ---------------------------------------------------------------------------

// EVERY LADDER OF ONE SESSION RESERVES IN ONE STREAM, AND NO TWO OF THEM TAKE ONE INDEX.
//
// This case is the same call site the wave 1 case guarded and the OPPOSITE assertion, which is
// recorded here rather than left for a reader to notice from the name. Wave 1 required the three
// classes' stream keys to DIFFER in the retention byte; the owner's ruling of 2026-09-07 -- items
// 143 and 169 together, shape A1 -- requires them to be identical, because
// (group_id, sender_handle) is what spec B's schema, spec B's Q7 and the shipped message server
// key the counter by. The wave 1 shape was a client/server split: the server would have refused
// the second class's first record as a stream index regression.
//
// The class of ladders is DERIVED and not listed, which the wave 1 case did not do. The three
// class names it wrote out were the three that had class keys at the time; a class ruled onto a
// class key later -- which is exactly what item 152 did to EPH on 2026-09-13 -- would have been
// outside a gate nobody would have remembered to widen. So this walks every wire byte
// connect/message accepts, asks the session for the ladder, and judges every one it gets.
//
// AND THAT DAY ARRIVED, WHICH IS WHY THIS CASE NOW REFUSES A REFUSAL. It used to record the eph
// bytes as expected refusals and REQUIRE at least one, on the reading that "MASTER invariant I4
// keeps the eph classes out of ClassKeys, so a session cannot build a ladder for one". I4 is
// untouched -- eph_root is still not derived from storage_root and ClassKeys still has three
// fields -- but the conclusion never followed from it: a session that HOLDS an eph_root builds
// the eph ladders from EphKey, and ledger item 152's ruling of 2026-09-13 is what put that call
// there. So this case
// installs one and requires EVERY accepted byte to build, with the empty refusal set asserted
// rather than assumed. A session's refusal to build an eph ladder is now about a VALUE it was
// never handed, and TestASessionWithNoEphRootRefusesExactlyTheEphWireBytes is what holds that.
func TestEveryRetentionClassOfOneSessionReservesInOneStream(t *testing.T) {
	fixture := newTestSession(t, "one-counter-per-sender")
	fixture.installEphRoot(t)
	type ladder struct {
		wire   byte
		stream StreamKey
		sender *SenderRatchet
	}
	ladders := []ladder{}
	refused := []byte{}
	var err error
	if postErr := fixture.session.do(func() {
		for candidate := 0; candidate <= 0xff; candidate += 1 {
			wire := byte(candidate)
			class, bucket, wireErr := message.RetentionClassOf(wire)
			if wireErr != nil {
				// not a legal retention byte at all, so there is no ladder to ask for
				continue
			}
			ratchet, buildErr := fixture.session.senderRatchetOnLoop(class, wire, bucket, 0)
			if buildErr != nil {
				refused = append(refused, wire)
				err = fmt.Errorf("wire %#02x (class %d bucket %d): %w", wire, class, bucket, buildErr)
				return
			}
			ladders = append(ladders, ladder{wire: wire, stream: ratchet.stream, sender: ratchet})
		}
	}); postErr != nil {
		t.Fatalf("post the ratchet command: %v", postErr)
	}
	if err != nil {
		t.Fatalf("build one sender ratchet per accepted retention byte: %v", err)
	}
	if len(ladders) < 2 {
		t.Fatalf("this session built %d ladders, so nothing here could observe two of them sharing a counter", len(ladders))
	}
	if len(refused) != 0 {
		t.Errorf("a session holding an eph_root refused a ladder for %#x; since ledger item 152 was ruled 2026-09-13 every retention byte the wire admits has a class key, and a refusal here is a class this session cannot seal at all", refused)
	}
	t.Logf("%d ladders, one per accepted retention byte, all on one stream key", len(ladders))
	// ONE stream, whole and entire. It is compared as a value rather than field by field, so a
	// field added to StreamKey later is inside this assertion without anybody widening it.
	for _, built := range ladders[1:] {
		if built.stream != ladders[0].stream {
			t.Errorf("the ladder for wire %#02x reserves in a different stream from the one for %#02x; ruling A1 makes the counter class blind, and a client counting per class has its second class's first record refused by the server as a stream index regression",
				built.wire, ladders[0].wire)
		}
	}
	// and the indices are the counter's, so they are distinct and cover a contiguous run: no
	// ladder wedges another, and none of them is handed a number another already has.
	taken := map[uint64]byte{}
	for _, built := range ladders {
		index, key, nextErr := built.sender.Next()
		if nextErr != nil {
			t.Fatalf("the ladder for wire %#02x could not take an index: %v", built.wire, nextErr)
		}
		if earlier, isRepeat := taken[index]; isRepeat {
			t.Errorf("wire %#02x and wire %#02x were both handed index %d; one index under two class keys is two records the server cannot tell apart on the counter it keeps",
				earlier, built.wire, index)
		}
		taken[index] = built.wire
		zeroize(key)
	}
	for want := uint64(1); want <= uint64(len(ladders)); want += 1 {
		if _, isTaken := taken[want]; !isTaken {
			t.Errorf("index %d was not taken by any ladder; %d ladders sharing one counter take %d consecutive indices",
				want, len(ladders), len(ladders))
		}
	}
}

// ---------------------------------------------------------------------------
// the pad fill, open item M1-7
// ---------------------------------------------------------------------------

// The fill byte is wire visible: two clients padding with different bytes produce different
// ct_body and different body_hash for one message. Nothing recorded which byte it is -- filling
// the rung with 0xFF before the length prefixed plaintext survived all three trees.
func TestThePadTailIsZeroAndIsPinnedByThisCase(t *testing.T) {
	for _, bodyPlain := range [][]byte{nil, []byte("x"), []byte("a body of some length")} {
		bucket, err := bucketForBody(len(bodyPlain))
		if err != nil {
			t.Fatalf("bucketForBody(%d): %v", len(bodyPlain), err)
		}
		padded, err := padBody(bucket, bodyPlain)
		if err != nil {
			t.Fatalf("padBody: %v", err)
		}
		rung := message.SizeBucketBytes(bucket)
		if len(padded) != rung {
			t.Fatalf("a padded body is %d octets and the rung is %d", len(padded), rung)
		}
		head := lpPrefixBytes + len(bodyPlain)
		if rung <= head {
			t.Fatalf("a %d octet body fills the whole %d octet rung, so there is no tail to read", len(bodyPlain), rung)
		}
		for at := head; at < rung; at += 1 {
			if padded[at] != 0 {
				t.Fatalf("the pad tail of a %d octet body holds %#02x at index %d, want 0; the fill is inside the aead, so two clients choosing differently produce two ct_body for one message",
					len(bodyPlain), padded[at], at)
			}
		}
		// and the round trip does not depend on the tail being anything, which is unpadBody's
		// own sentence: the reader recovers the length from inside the aead
		recovered, err := unpadBody(bucket, padded)
		if err != nil {
			t.Fatalf("unpadBody: %v", err)
		}
		if !bytes.Equal(recovered, bodyPlain) && !(len(recovered) == 0 && len(bodyPlain) == 0) {
			t.Errorf("a %d octet body unpadded to %d octets", len(bodyPlain), len(recovered))
		}
	}
}

// ---------------------------------------------------------------------------
// a rung held in a function local is erased before that function returns
// ---------------------------------------------------------------------------

// connect/mls's erase reading is over TYPES -- every type holding erasable key material erases all
// of it -- and the residual it leaves is the locals on the open path, which belong to no type.
// Measured: deleting openRecordOnLoop's `defer zeroize(recordKey)` survived all three trees, while
// the parallel mutation making (*recordBuilder).zeroize a no-op is caught over there. So the class
// is real and derived and its hole is exactly this shape.
//
// The scope question (R3a): the SCOPE is this package's production source, because a local belongs
// to the body it is declared in and mls's reading already covers every field. The CLASS is derived
// in two steps and never listed:
//
//   - a RUNG CONSUMER is a declaration that names ErrRecordKeyLength -- this package's own name
//     for "that argument is a rung of the ladder" -- plus every declaration that hands one of its
//     own parameters to a rung consumer in that position. The seed is the SENTINEL and not the
//     refusal helper's name, so a second refusal spelled inline, or the helper renamed, is the
//     same class; and it is not a list of the four ladder derivations, which is what the class
//     would have been if it had been read off the instance in front of it.
//   - a RUNG LOCAL is a name bound from a CALL inside some body and then passed to a rung consumer
//     in a rung position. A parameter is deliberately not one: a parameter belongs to the caller,
//     and every derivation of the ladder takes one.
//
// The assertion is that every rung local is handed to a member of the erase class zeroize_test.go
// derives -- zeroize itself, or a helper that erases what it is given, which is how stepRecordKey
// discharges the obligation for the two resume walks.
func TestEveryLocalHoldingARungOfTheLadderIsErasedBeforeItsFunctionReturns(t *testing.T) {
	controlSet := token.NewFileSet()
	const controlName = "the rung local control"
	control, err := parser.ParseFile(controlSet, controlName, repairRungLocalControl,
		parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse the control: %v", err)
	}
	controlSources := []messagegroupSource{{path: controlName, parsed: control}}
	unerased := repairRungLocalsMissingAnErase(controlSet, controlSources)
	want := []string{"heldAndNeverErased", "walkedAndNeverErased"}
	if !slices.Equal(unerased, want) {
		t.Fatalf("the matcher read %v out of the control as rungs left live, want %v; it is not telling a local bound from a call and handed to the ladder from a parameter, from a local that is erased, from one handed to an eraser, or from one that never reaches the ladder at all",
			unerased, want)
	}

	fileSet, sources := messagegroupProductionSources(t)
	consumers := repairRungConsumers(sources)
	if len(consumers) == 0 {
		t.Fatal("no production declaration of this package refuses a wrong width record key, so this gate derived an empty class and would report clean over any body at all")
	}
	locals := repairRungLocals(sources, consumers)
	if len(locals) == 0 {
		t.Fatal("no production body binds a rung of the ladder to a local, so this gate is reporting clean having read nothing")
	}
	for _, left := range repairRungLocalsMissingAnErase(fileSet, sources) {
		t.Errorf("%s holds a rung of the record key ladder in a local and hands it to no eraser; the ladder's forward secrecy is the erasure of the rung a body has finished with, and a rung left live in a function local is one an attacker who takes the process reads",
			left)
	}
	t.Logf("%d rung consumer(s) %v; %d rung local(s) %v", len(consumers), slices.Sorted(maps.Keys(consumers)),
		len(locals), locals)
}

// One body of each shape, so a matcher that stopped matching fails HERE rather than reporting the
// package clean.
const repairRungLocalControl = "package control\n" +
	"\n" +
	"var ErrRecordKeyLength = errors.New(\"width\")\n" +
	"\n" +
	"func refuseWrongWidthRecordKey(recordKey []byte) {\n" +
	"\tif len(recordKey) != 32 {\n" +
	"\t\tpanic(ErrRecordKeyLength)\n" +
	"\t}\n" +
	"}\n" +
	"\n" +
	"//go:noinline\n" +
	"func zeroize(secret []byte) {\n" +
	"\tfor i := range secret {\n" +
	"\t\tsecret[i] = 0\n" +
	"\t}\n" +
	"}\n" +
	"\n" +
	"func ladderNext(recordKey []byte) []byte {\n" +
	"\trefuseWrongWidthRecordKey(recordKey)\n" +
	"\treturn recordKey\n" +
	"}\n" +
	"\n" +
	"//go:noinline\n" +
	"func stepAndErase(recordKey []byte) []byte {\n" +
	"\tsuccessor := ladderNext(recordKey)\n" +
	"\tzeroize(recordKey)\n" +
	"\treturn successor\n" +
	"}\n" +
	"\n" +
	"func heldAndErased(source func() []byte) []byte {\n" +
	"\trung := source()\n" +
	"\tdefer zeroize(rung)\n" +
	"\treturn ladderNext(rung)\n" +
	"}\n" +
	"\n" +
	"func heldAndNeverErased(source func() []byte) []byte {\n" +
	"\trung := source()\n" +
	"\treturn ladderNext(rung)\n" +
	"}\n" +
	"\n" +
	"func walkedAndHandedToAnEraser(source func() []byte) []byte {\n" +
	"\trung := source()\n" +
	"\trung = stepAndErase(rung)\n" +
	"\treturn rung\n" +
	"}\n" +
	"\n" +
	"func walkedAndNeverErased(source func() []byte) []byte {\n" +
	"\trung := source()\n" +
	"\treturn ladderNext(rung)\n" +
	"}\n" +
	"\n" +
	"func aParameterIsTheCallersAndNotALocal(recordKey []byte) []byte {\n" +
	"\treturn ladderNext(recordKey)\n" +
	"}\n" +
	"\n" +
	"func neverReachesTheLadder(source func() []byte) int {\n" +
	"\tnotARung := source()\n" +
	"\treturn len(notARung)\n" +
	"}\n"

// repairRungConsumers is the class of declarations one of whose arguments is a rung, by argument
// position, derived off the refusal the ladder makes and closed under the hand-off.
func repairRungConsumers(sources []messagegroupSource) map[string][]int {
	consumers := map[string][]int{}
	functions := []*ast.FuncDecl{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			if function, isFunction := declaration.(*ast.FuncDecl); isFunction && function.Body != nil {
				functions = append(functions, function)
			}
		}
	}
	mark := func(name string, at int) bool {
		if slices.Contains(consumers[name], at) {
			return false
		}
		consumers[name] = append(consumers[name], at)
		slices.Sort(consumers[name])
		return true
	}
	// the seed: whatever names the ladder's own width sentinel is refusing a record key, and the
	// record key is whatever []byte it was handed
	for _, function := range functions {
		if !repairNamesTheRecordKeySentinel(function.Body) {
			continue
		}
		for at, kind := range repairParameterKinds(function) {
			if kind == "[]byte" {
				mark(function.Name.Name, at)
			}
		}
	}
	// and the closure: a body that hands its own parameter to a consumer in a rung position takes
	// a rung in the position that parameter stands at
	for grew := true; grew; {
		grew = false
		for _, function := range functions {
			for _, call := range repairCallsIn(function.Body) {
				positions := consumers[repairCalleeName(call)]
				for _, at := range positions {
					if len(call.Args) <= at {
						continue
					}
					if own := repairParameterPosition(function, call.Args[at]); 0 <= own {
						grew = mark(function.Name.Name, own) || grew
					}
				}
			}
		}
	}
	return consumers
}

// repairNamesTheRecordKeySentinel answers whether one body names ErrRecordKeyLength, which is
// this package's declared name for "this argument is a rung of the record key ladder".
func repairNamesTheRecordKeySentinel(body ast.Node) bool {
	named := false
	ast.Inspect(body, func(node ast.Node) bool {
		if identifier, isIdentifier := node.(*ast.Ident); isIdentifier && identifier.Name == "ErrRecordKeyLength" {
			named = true
		}
		return true
	})
	return named
}

// repairParameterKinds renders one declaration's parameter types, one entry per parameter, so a
// position in this slice is a position in a call's argument list.
func repairParameterKinds(function *ast.FuncDecl) []string {
	kinds := []string{}
	if function.Type.Params == nil {
		return kinds
	}
	for _, field := range function.Type.Params.List {
		rendered := ""
		if array, isArray := field.Type.(*ast.ArrayType); isArray && array.Len == nil {
			if element, isName := array.Elt.(*ast.Ident); isName {
				rendered = "[]" + element.Name
			}
		}
		for range max(len(field.Names), 1) {
			kinds = append(kinds, rendered)
		}
	}
	return kinds
}

// repairRungLocals is every "function.local" that is bound from a call and then handed to the
// ladder in a rung position.
func repairRungLocals(sources []messagegroupSource, consumers map[string][]int) []string {
	found := []string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			for _, name := range repairRungLocalsOf(function, consumers) {
				found = append(found, function.Name.Name+"."+name)
			}
		}
	}
	slices.Sort(found)
	return found
}

// repairRungLocalsOf is one body's rung locals, in declaration order.
func repairRungLocalsOf(function *ast.FuncDecl, consumers map[string][]int) []string {
	fromACall := map[string]bool{}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		assignment, isAssignment := node.(*ast.AssignStmt)
		if !isAssignment {
			return true
		}
		for at, right := range assignment.Rhs {
			if _, isCall := right.(*ast.CallExpr); !isCall {
				continue
			}
			// one call destructured across several names binds all of them; one call per name
			// binds that one.
			targets := assignment.Lhs
			if len(assignment.Lhs) == len(assignment.Rhs) {
				targets = assignment.Lhs[at : at+1]
			}
			for _, target := range targets {
				if name, isBare := target.(*ast.Ident); isBare && name.Name != "_" &&
					repairParameterPosition(function, name) < 0 {
					fromACall[name.Name] = true
				}
			}
		}
		return true
	})
	rungs := []string{}
	for _, call := range repairCallsIn(function.Body) {
		for _, at := range consumers[repairCalleeName(call)] {
			if len(call.Args) <= at {
				continue
			}
			name, isBare := call.Args[at].(*ast.Ident)
			if !isBare || !fromACall[name.Name] || slices.Contains(rungs, name.Name) {
				continue
			}
			rungs = append(rungs, name.Name)
		}
	}
	slices.Sort(rungs)
	return rungs
}

// repairRungLocalsMissingAnErase is every rung local its own body hands to no eraser.
func repairRungLocalsMissingAnErase(fileSet *token.FileSet, sources []messagegroupSource) []string {
	named := zeroizeByteSliceTypeNames(sources)
	candidates := []zeroizeCandidate{}
	for _, source := range sources {
		candidates = append(candidates, zeroizeCandidatesIn(fileSet, source.parsed, source.path, named)...)
	}
	erasers, _ := zeroizeEraseClass(candidates)
	consumers := repairRungConsumers(sources)
	missing := []string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			erased := map[string]bool{}
			for _, call := range repairCallsIn(function.Body) {
				if !slices.Contains(erasers, repairCalleeName(call)) {
					continue
				}
				for _, argument := range call.Args {
					if name, isBare := argument.(*ast.Ident); isBare {
						erased[name.Name] = true
					}
				}
			}
			for _, rung := range repairRungLocalsOf(function, consumers) {
				if !erased[rung] {
					missing = append(missing, function.Name.Name)
				}
			}
		}
	}
	slices.Sort(missing)
	return slices.Compact(missing)
}

// repairCallsIn is every call expression one body makes.
func repairCallsIn(body ast.Node) []*ast.CallExpr {
	calls := []*ast.CallExpr{}
	ast.Inspect(body, func(node ast.Node) bool {
		if call, isCall := node.(*ast.CallExpr); isCall {
			calls = append(calls, call)
		}
		return true
	})
	return calls
}

// repairCalleeName is a call's callee by its bare name, so a method and a package level function
// sharing one name are one entry. That can only WIDEN the class, and a wider class demands the
// erasure of more bodies rather than fewer.
func repairCalleeName(call *ast.CallExpr) string {
	switch named := call.Fun.(type) {
	case *ast.Ident:
		return named.Name
	case *ast.SelectorExpr:
		return named.Sel.Name
	}
	return ""
}

// repairParameterPosition is where one expression stands in a function's parameter list, or -1 if
// it is not one of its parameters.
func repairParameterPosition(function *ast.FuncDecl, expr ast.Expr) int {
	name, isBare := expr.(*ast.Ident)
	if !isBare || function.Type.Params == nil {
		return -1
	}
	at := 0
	for _, field := range function.Type.Params.List {
		if len(field.Names) == 0 {
			at += 1
			continue
		}
		for _, declared := range field.Names {
			if declared.Name == name.Name {
				return at
			}
			at += 1
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// go/types is available to this file for the staged commit reading in engine_test.go
// ---------------------------------------------------------------------------

// repairTypeCheckProduction type checks this package's production files and answers the info a
// gate needs in order to ask what an expression IS rather than what it looks like.
//
// It is the same reading TestNoProductionTypeOfThisPackageSatisfiesTheReserver takes, in one place
// so a second gate over the same question is not a second parse.
func repairTypeCheckProduction(t *testing.T) (*types.Info, []messagegroupSource) {
	t.Helper()
	fileSet := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read this package's directory: %v", err)
	}
	sources := []messagegroupSource{}
	files := []*ast.File{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		sources = append(sources, messagegroupSource{path: name, parsed: parsed})
		files = append(files, parsed)
	}
	if len(files) == 0 {
		t.Fatal("no production file was read, so this gate type checked nothing")
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
	config := types.Config{Importer: importer.ForCompiler(fileSet, "source", nil)}
	if _, err := config.Check("github.com/urnetwork/connect/messagegroup", fileSet, files, info); err != nil {
		t.Fatalf("type check this package's production source: %v", err)
	}
	// the SOURCES ARE THE ONES THAT WERE CHECKED, handed back rather than re-parsed, because the
	// info below is keyed by expression identity: a second parse of the same bytes answers
	// different nodes and every lookup in it would miss, which is the vacuous reading this gate
	// would then report as clean.
	return info, sources
}
