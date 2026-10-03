// SealRecord and OpenRecord: the construction order as a type, the padder no document states,
// and the two failures section 5.5 and section 5.11 say are not errors.
package messagegroup

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/message"
)

// ---------------------------------------------------------------------------
// Property 1: the order is unrepresentable-otherwise
// ---------------------------------------------------------------------------

// MASTER section 8 fixes the order and section 5.2 calls it a type rather than a convention:
//
//	build server_attachment -> encrypt ct_body -> compute body_hash -> encrypt ct_head ->
//	compute write_auth. Every dependency is acyclic, and getting it wrong produces a circular
//	AAD that appears to work until two implementations disagree.
//
// A test that only checked the output bytes cannot tell "computed in order" from "computed in any
// order and assembled", so this reads the DEPENDENCY EDGES off the syntax tree.
//
// The scope question (R3a), answered separately: the SCOPE is this package's production source,
// because the staging types are unexported and no other package can hold one. The CLASS is the
// chain of staging types, derived by following each stage's transition method to the type it
// answers, starting at whatever newRecordBuilderOnLoop returns and never at a name written down
// here; and beside it, every call into connect/message's two aad builders and its write_auth mac.
func TestTheSealConstructionOrderIsAChainOfTypesAndNotASequenceOfStatements(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	transitions := map[string][]sealTransition{}
	enclosing := map[string]string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			receiver := sessionReceiverName(function)
			enclosing[function.Name.Name] = receiver
			if receiver == "" || function.Type.Results == nil || len(function.Type.Results.List) == 0 {
				continue
			}
			answered := sealPointerResultName(function.Type.Results.List[0].Type)
			if answered == "" || answered == receiver {
				continue
			}
			transitions[receiver] = append(transitions[receiver],
				sealTransition{method: function.Name.Name, answers: answered})
		}
	}
	start := ""
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Name.Name != "newRecordBuilderOnLoop" || function.Type.Results == nil {
				continue
			}
			start = sealPointerResultName(function.Type.Results.List[0].Type)
		}
	}
	if start == "" {
		t.Fatal("nothing named newRecordBuilderOnLoop answers a staging type, so this gate has no chain to walk and would report clean over any order at all")
	}
	chain := []string{start}
	position := map[string]int{start: 0}
	for {
		last := chain[len(chain)-1]
		outbound := transitions[last]
		if len(outbound) == 0 {
			break
		}
		if len(outbound) != 1 {
			names := []string{}
			for _, one := range outbound {
				names = append(names, one.method+" -> "+one.answers)
			}
			t.Fatalf("%s has %d transitions (%v); the order is a type exactly because each stage answers ONE next stage, and a second one is a branch a caller chooses",
				last, len(outbound), names)
		}
		next := outbound[0].answers
		if _, isSeen := position[next]; isSeen {
			t.Fatalf("the stage chain returns to %s, so it is a cycle rather than an order", next)
		}
		chain = append(chain, next)
		position[next] = len(chain) - 1
		if next == "Record" {
			break
		}
	}
	if chain[len(chain)-1] != "Record" {
		t.Fatalf("the stage chain is %v and does not end at a Record; every stage but the last answers the next, and the last answers the record", chain)
	}
	if len(chain) < 5 {
		t.Fatalf("the stage chain is %v; MASTER section 8's order has four steps after the attachment, so a chain shorter than five types has collapsed two of them into one body",
			chain)
	}
	// where each of the three landed, read off the call sites rather than off the chain.
	where := map[string]int{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			for _, callee := range sealMessageCalleesIn(function.Body) {
				at, isStage := position[enclosing[function.Name.Name]]
				if !isStage {
					continue
				}
				if seen, isSeen := where[callee]; isSeen && seen != at {
					t.Errorf("%s is called from two stages of the chain (%d and %d); the order rests on each step happening once, in one place",
						callee, seen, at)
				}
				where[callee] = at
			}
		}
	}
	// EncodeRecord is in this list for Property 5 -- "the record EncodeRecord refuses is the record
	// SealRecord refuses" -- which was held by a comment. Measured: deleting the internal
	// message.EncodeRecord call from the last stage survived all three trees, because
	// TestASealedRecordIsExactlyItsRungAndIsOneTheCodecAccepts calls EncodeRecord itself on the
	// output and so passes identically with or without it, and no case drives SealRecord to a
	// record the codec would refuse. Whether one is reachable TODAY is not the property: the
	// property is that the sealer asks the codec rather than reimplementing checkRecord, and that
	// is a fact about the source, so it is asserted here where the stage each call lands in is
	// already being read.
	for _, ordered := range []string{"AADBody", "AADHead", "ComputeWriteAuth", "EncodeRecord"} {
		if _, isCalled := where[ordered]; !isCalled {
			t.Fatalf("no stage of the chain calls message.%s; this gate is asserting an order over a step that is not there, which is the vacuous shape it exists to avoid",
				ordered)
		}
	}
	if !(where["AADBody"] < where["AADHead"] && where["AADHead"] < where["ComputeWriteAuth"]) {
		t.Errorf("the order read off the chain is AADBody at %d, AADHead at %d, ComputeWriteAuth at %d; MASTER section 8 fixes it as body, then head, then mac, and a circular aad appears to work until two implementations disagree",
			where["AADBody"], where["AADHead"], where["ComputeWriteAuth"])
	}
	// and the codec is asked LAST, in the stage that answers the record, so what SealRecord hands
	// back has been through the encoder it will be encoded by.
	if where["EncodeRecord"] != len(chain)-2 {
		t.Errorf("message.EncodeRecord is called from stage %d of %v and the last staging type is stage %d; Property 5 rests on the finished record going through the codec rather than on checkRecord being reimplemented here",
			where["EncodeRecord"], chain, len(chain)-2)
	}
	t.Logf("stage chain: %v; AADBody@%d AADHead@%d ComputeWriteAuth@%d EncodeRecord@%d",
		chain, where["AADBody"], where["AADHead"], where["ComputeWriteAuth"], where["EncodeRecord"])
}

type sealTransition struct {
	method  string
	answers string
}

// sealPointerResultName is the type name behind a *T result, whether T is this package's or
// connect/message's.
func sealPointerResultName(result ast.Expr) string {
	star, isStar := result.(*ast.StarExpr)
	if !isStar {
		return ""
	}
	switch named := star.X.(type) {
	case *ast.Ident:
		return named.Name
	case *ast.SelectorExpr:
		return named.Sel.Name
	}
	return ""
}

// sealMessageCalleesIn is every message.X call one body makes.
func sealMessageCalleesIn(body ast.Node) []string {
	callees := []string{}
	ast.Inspect(body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		selector, isSelector := call.Fun.(*ast.SelectorExpr)
		if !isSelector {
			return true
		}
		qualifier, isIdentifier := selector.X.(*ast.Ident)
		if !isIdentifier || qualifier.Name != "message" {
			return true
		}
		callees = append(callees, selector.Sel.Name)
		return true
	})
	return callees
}

// ---------------------------------------------------------------------------
// Property 3: the reservation precedes the first AEAD derivation
// ---------------------------------------------------------------------------

// Task 7 Property 1's reachability half, landing here because this is the commit where the class
// first has a member: measured 2026-09-06, nothing in either half of the record layer called
// either aead derivation before SealRecord existed.
//
// IT IS STATED OVER THE STAGING TYPES RATHER THAN AS A REACHABILITY CUT, and the difference is
// the whole reason the staging types exist. A cut over the call graph cannot hold this shape at
// all: sealRecordOnLoop calls the builder's constructor and then the builder's sealBody, so
// removing every caller of Reserve leaves sealBody reachable by NAME while leaving it
// unreachable in fact -- sealBody is a method on *recordBuilder, and the only production
// declaration that answers a *recordBuilder is the one that reserves. That is not a convention
// the walk has to be careful about; it is a type, and this gate reads it as one.
//
// Three assertions, each of which is a way the property can be lost:
//
//  1. every declaration reachable from SealRecord that calls a record aead derivation is a
//     method on a stage of the chain. A free function on that path would be a door to a record
//     key with no builder in front of it.
//  2. exactly ONE production declaration answers the chain's first stage. A second would be a
//     second door, and it is the edit somebody makes to "reuse the builder".
//  3. that declaration reaches Reserve.
//
// What this cannot prove stays at Task 7 Property 1: that the path passes through a Reserve whose
// error was checked. Error handling and ordering are invisible to a walk over the syntax tree,
// which is why ratchet_test.go keeps the statement order check on Next and the injected failing
// reserver beside it.
func TestEveryPathFromSealRecordToARecordAeadPassesThroughTheReservation(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	calls := map[string]map[string]bool{}
	receivers := map[string]string{}
	answers := map[string][]string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			name := function.Name.Name
			receivers[name] = sessionReceiverName(function)
			if function.Type.Results != nil && 0 < len(function.Type.Results.List) {
				if answered := sealPointerResultName(function.Type.Results.List[0].Type); answered != "" {
					answers[answered] = append(answers[answered], name)
				}
			}
			if calls[name] == nil {
				calls[name] = map[string]bool{}
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall {
					return true
				}
				switch found := call.Fun.(type) {
				case *ast.Ident:
					calls[name][found.Name] = true
				case *ast.SelectorExpr:
					calls[name][found.Sel.Name] = true
				}
				return true
			})
		}
	}
	reachable := map[string]bool{"SealRecord": true}
	for grew := true; grew; {
		grew = false
		for name := range reachable {
			for callee := range calls[name] {
				if _, isDeclared := calls[callee]; isDeclared && !reachable[callee] {
					reachable[callee] = true
					grew = true
				}
			}
		}
	}
	derivations := map[string]bool{"RecordAeadHead": true, "RecordAeadBody": true}
	stages := sealStageChain(t, sources)
	staged := map[string]bool{}
	for _, stage := range stages {
		staged[stage] = true
	}
	deriving := []string{}
	for name := range reachable {
		reachesADerivation := false
		for callee := range calls[name] {
			if derivations[callee] {
				reachesADerivation = true
			}
		}
		if !reachesADerivation {
			continue
		}
		deriving = append(deriving, name)
		if !staged[receivers[name]] {
			t.Errorf("%s is reachable from SealRecord, derives a record aead key and is not a method on a stage of the builder chain %v; a door to a record key with no builder in front of it is a key handed out before the stream index it will be used at was reserved",
				name, stages)
		}
	}
	slices.Sort(deriving)
	if len(deriving) == 0 {
		t.Fatal("nothing reachable from SealRecord derives a record aead key at all, so this gate is reporting clean having read nothing")
	}
	if len(stages) == 0 {
		t.Fatal("no builder chain was read, so assertions 2 and 3 have nothing to stand on")
	}
	producers := answers[stages[0]]
	if len(producers) != 1 {
		t.Fatalf("%d production declarations answer a *%s (%v); the reservation is in front of the chain exactly because there is ONE way into it",
			len(producers), stages[0], producers)
	}
	fromTheDoor := map[string]bool{producers[0]: true}
	for grew := true; grew; {
		grew = false
		for name := range fromTheDoor {
			for callee := range calls[name] {
				if _, isDeclared := calls[callee]; isDeclared && !fromTheDoor[callee] {
					fromTheDoor[callee] = true
					grew = true
				}
			}
		}
	}
	if !fromTheDoor["Reserve"] && !calls[producers[0]]["Reserve"] {
		reserves := false
		for name := range fromTheDoor {
			if calls[name]["Reserve"] {
				reserves = true
			}
		}
		if !reserves {
			t.Errorf("%s is the one door into the builder chain and nothing it reaches calls Reserve; section 5.6 requires the index to be durably recorded BEFORE anything is encrypted",
				producers[0])
		}
	}
	t.Logf("%d declaration(s) derive a record aead key on the seal path: %v; the chain is %v behind %s",
		len(deriving), deriving, stages, producers[0])
}

// sealStageChain walks the builder chain the order gate derives, so the two gates read one chain
// rather than two that agree until the day one is edited.
func sealStageChain(t *testing.T, sources []messagegroupSource) []string {
	t.Helper()
	transitions := map[string][]sealTransition{}
	start := ""
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil || function.Type.Results == nil ||
				len(function.Type.Results.List) == 0 {
				continue
			}
			answered := sealPointerResultName(function.Type.Results.List[0].Type)
			if answered == "" {
				continue
			}
			if function.Name.Name == "newRecordBuilderOnLoop" {
				start = answered
			}
			receiver := sessionReceiverName(function)
			if receiver == "" || receiver == answered {
				continue
			}
			transitions[receiver] = append(transitions[receiver],
				sealTransition{method: function.Name.Name, answers: answered})
		}
	}
	if start == "" {
		return nil
	}
	chain := []string{start}
	seen := map[string]bool{start: true}
	for {
		outbound := transitions[chain[len(chain)-1]]
		if len(outbound) != 1 || seen[outbound[0].answers] {
			break
		}
		chain = append(chain, outbound[0].answers)
		seen[outbound[0].answers] = true
	}
	return chain
}

// ---------------------------------------------------------------------------
// Property 7: every aad call passes the record aead's own algorithm identifier
// ---------------------------------------------------------------------------

// Task 1 Property 1's derived-class half, landing here because this is the commit where the class
// first has a member.
//
// The scope question (R3a), answered separately from the class question, because the split
// separated them. The CLASS is the call sites read off the syntax tree and never a list. The
// SCOPE is TWO directories, and it must be both: connect/message is where the builders are
// declared and where a future server-side call would appear, and connect/messagegroup is where
// every call is today. A gate rooted at messagegroup alone reports clean over a server-side call
// passing a literal; one rooted at message alone reads an empty class and fatals.
func TestEveryAadCallInEitherHalfPassesTheRecordAeadAlgId(t *testing.T) {
	roots := []string{".", "../message"}
	calls := 0
	byRoot := map[string]int{}
	for _, root := range roots {
		for _, source := range sealProductionSourcesUnder(t, root) {
			ast.Inspect(source.parsed, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall || len(call.Args) == 0 {
					return true
				}
				callee := ""
				switch found := call.Fun.(type) {
				case *ast.Ident:
					callee = found.Name
				case *ast.SelectorExpr:
					callee = found.Sel.Name
				}
				if callee != "AADHead" && callee != "AADBody" {
					return true
				}
				calls += 1
				byRoot[root] += 1
				algId := ""
				switch first := call.Args[0].(type) {
				case *ast.Ident:
					algId = first.Name
				case *ast.SelectorExpr:
					algId = first.Sel.Name
				}
				if algId != "RecordAeadAlgId" {
					t.Errorf("%s calls %s with %q as its alg_id; the record aead's identifier is RecordAeadAlgId, and a literal, an X-Wing identifier or an attachment identifier is an aad no second implementation reconstructs",
						source.path, callee, algId)
				}
				return true
			})
		}
	}
	if calls == 0 {
		t.Fatalf("no production call of AADHead or AADBody was found under %v, so this gate is reporting clean having read nothing", roots)
	}
	t.Logf("%d aad call(s) across %d root(s): %v", calls, len(roots), byRoot)
}

// sealProductionSourcesUnder parses one root's non test go files.
//
// It exists because this package's own reader is scoped to this directory by design, and this one
// gate is the one whose scope is two of them.
func sealProductionSourcesUnder(t *testing.T, root string) []messagegroupSource {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	fileSet := token.NewFileSet()
	sources := []messagegroupSource{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(root, name)
		parsed, err := parser.ParseFile(fileSet, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		sources = append(sources, messagegroupSource{path: path, parsed: parsed})
	}
	if len(sources) == 0 {
		t.Fatalf("no production source was read under %s, so a gate rooted there is reading nothing", root)
	}
	return sources
}

// ---------------------------------------------------------------------------
// Property 2: body_hash is H(ct_body), and it is not in aad_body
// ---------------------------------------------------------------------------

// The second half is unrepresentable -- AADBody takes a BodyBinding and there is no hash within
// its reach -- and it is asserted anyway, because the assertion is what survives a refactor of
// BodyBinding. It is derived off the TYPE rather than off a list of its fields.
func TestBodyHashIsTheHashOfTheSealedBodyAndIsNotInTheBodyAad(t *testing.T) {
	binding := reflect.TypeOf(message.BodyBinding{})
	if binding.NumField() == 0 {
		t.Fatal("message.BodyBinding declares no fields, so this half read nothing")
	}
	for i := range binding.NumField() {
		field := binding.Field(i)
		if strings.Contains(strings.ToLower(field.Name), "hash") {
			t.Errorf("message.BodyBinding declares %s; guardrail G4 makes aad_body a function of a value with no hash in reach, because body_hash placed in aad_body is the body's own ciphertext hashed into the aad the body is sealed under",
				field.Name)
		}
	}
	fixture := newTestSession(t, "body-hash")
	record, err := fixture.session.SealRecord(message.RetentionDurable, 0, false,
		[]byte("head"), []byte("a body of some length"), 0, nil)
	if err != nil {
		t.Fatalf("SealRecord: %v", err)
	}
	want := sha256.Sum256(record.CtBody)
	if record.Header.BodyHash != want {
		t.Errorf("body_hash is %x and H(ct_body) is %x; it is taken over the sealed and PADDED ciphertext, which is what a pruned record still says about what it carried",
			record.Header.BodyHash, want)
	}
	// and it is not the hash of the plaintext, nor of the padded plaintext, which are the two
	// values an edit computing it one step early would produce.
	if record.Header.BodyHash == sha256.Sum256([]byte("a body of some length")) {
		t.Error("body_hash is the hash of the PLAINTEXT; a holder of the record cannot recompute that")
	}
	padded, err := padBody(record.Header.SizeBucket, []byte("a body of some length"))
	if err != nil {
		t.Fatalf("padBody: %v", err)
	}
	if record.Header.BodyHash == sha256.Sum256(padded) {
		t.Error("body_hash is the hash of the padded plaintext rather than of the ciphertext")
	}
}

// ---------------------------------------------------------------------------
// Property 5 and 6: the rung, the codec, and the classes M1-6's reversal admits
// (M1-6 ruled 2026-09-07, REVERSED 2026-09-13 together with ledger item 152)
// ---------------------------------------------------------------------------

// The record EncodeRecord refuses is the record SealRecord refuses, held by CALLING EncodeRecord
// rather than by reimplementing checkRecord here.
func TestASealedRecordIsExactlyItsRungAndIsOneTheCodecAccepts(t *testing.T) {
	fixture := newTestSession(t, "rungs")
	// THE LENGTHS MOVED WITH MASTER SECTION 8.4 AND THE PROPERTY DID NOT. They used to be the
	// old capacities and their successors -- 252/253, 1020, 4092, 16380 -- and the frame is 193
	// to 198 octets, so every one of those now lands a rung higher. What is walked is the same
	// thing it always was: each rung's capacity and the first length that does not fit it, taken
	// from mlsframe_test.go's MEASURED column rather than from arithmetic on the ladder.
	lengths := []int{0, 1}
	for bucket := message.SizeBucket(0); bucket < message.SizeBucketBlob-1; bucket += 1 {
		lengths = append(lengths, applicationBodyCapacity[bucket], applicationBodyCapacity[bucket]+1)
	}
	for _, bodyLength := range lengths {
		body := bytes.Repeat([]byte{0x5a}, bodyLength)
		record, err := fixture.session.SealRecord(message.RetentionDurable, 0, false, []byte("head"), body, 0, nil)
		if err != nil {
			t.Fatalf("SealRecord over a %d octet body: %v", bodyLength, err)
		}
		want := message.SizeBucketCtBodyBytes(record.Header.SizeBucket)
		if len(record.CtBody) != want {
			t.Errorf("a %d octet body sealed to %d octets of ct_body on rung %d, want %d",
				bodyLength, len(record.CtBody), record.Header.SizeBucket, want)
		}
		if _, err := message.EncodeRecord(record); err != nil {
			t.Errorf("a record SealRecord answered is one EncodeRecord refuses: %v", err)
		}
		// the rung is the SMALLEST that fits, which is what keeps padding from disclosing more
		// than the ladder already does. "Fits" is now a statement about the FRAME and not about
		// the caller's plaintext, which is why the bound is the measured capacity of the rung
		// below rather than that rung's octet count minus a length prefix.
		if 0 < record.Header.SizeBucket {
			if bodyLength <= applicationBodyCapacity[record.Header.SizeBucket-1] {
				t.Errorf("a %d octet body went to rung %d and fits rung %d, whose measured capacity is %d",
					bodyLength, record.Header.SizeBucket, record.Header.SizeBucket-1,
					applicationBodyCapacity[record.Header.SizeBucket-1])
			}
		}
	}
	// a body longer than the ladder is a blob, and a blob is task 20's.
	tooLong := bytes.Repeat([]byte{0x01}, message.SizeBucketBytes(message.SizeBucket64K)+1)
	if _, err := fixture.session.SealRecord(message.RetentionDurable, 0, false, []byte("head"), tooLong, 0, nil); !errors.Is(err, ErrBodyTooLong) {
		t.Errorf("a body longer than the largest rung answered %v, want ErrBodyTooLong", err)
	}
}

// Property 6, REPLACED BY ITS OWN SUBJECT'S REVERSAL: a session that holds no eph_root refuses
// exactly the eph wire bytes and nothing else.
//
// WHAT THIS CASE USED TO BE, because the sentence it asserted is the one that was overturned. It
// was TestOnlyTheDurableClassIsSealedUntilM16IsRuled, and it required PERMANENT, MEDIA and every
// EPH bucket to be refused with the blanket class sentinel, on the reading that MASTER section
// 8.1 and spec A section 5.3 disagreed about which record key seals ct_head. M1-6 was ruled on
// 2026-09-07 and its ruling was REVERSED on 2026-09-13 (ledger items 152 and 128, spec A revision
// A-25): ct_head takes the record's OWN class key, head and body take one ladder at one position,
// and spec A section 5.3 says in as many words that "THE REFUSAL IS NOW LIFTED IN FULL". Keeping
// this case would have been keeping the rule that was overturned.
//
// WHAT REPLACES IT IS THE HALF OF THE OLD REFUSAL THAT SURVIVED AS A DIFFERENT SENTENCE. The eph
// classes are still not sealable by a session that holds no eph_root -- not because a class is
// unruled, but because K_eph has an input that is neither derived nor defaulted (MASTER I4). The
// SPLIT is what this case pins, and it pins both halves of it and their complement rather than
// one side: exactly the eph bytes refuse, exactly the non-eph bytes seal.
func TestASessionWithNoEphRootRefusesExactlyTheEphWireBytes(t *testing.T) {
	fixture := newTestSession(t, "no-eph-root")
	refusedWire := []byte{}
	sealedWire := []byte{}
	for candidate := 0; candidate <= 0xFF; candidate += 1 {
		wire := byte(candidate)
		class, bucket, wireErr := message.RetentionClassOf(wire)
		if wireErr != nil {
			continue
		}
		record, err := fixture.session.SealRecord(class, bucket, false,
			[]byte("head"), []byte("body"), 0, nil)
		switch {
		case err == nil:
			sealedWire = append(sealedWire, wire)
			if record == nil {
				t.Errorf("wire %#02x sealed and answered no record", wire)
			}
		case errors.Is(err, ErrNoEphRoot):
			refusedWire = append(refusedWire, wire)
			if record != nil {
				t.Errorf("wire %#02x was refused and answered a record beside its error", wire)
			}
		default:
			t.Errorf("wire %#02x answered %v, which is neither a sealed record nor the one refusal a session with no eph_root owes", wire, err)
		}
	}
	// CLASS: the eph half is derived off connect/message's own split, never off a list of
	// bytes. SCOPE: all 256 octets, offered to RetentionClassOf, whose acceptances are the
	// alphabet. Both halves are named and both are pinned, because "some refused and some
	// sealed" is satisfied by twelve readings when there are thirteen.
	wantRefused, wantSealed := []byte{}, []byte{}
	for candidate := 0; candidate <= 0xFF; candidate += 1 {
		wire := byte(candidate)
		class, _, wireErr := message.RetentionClassOf(wire)
		if wireErr != nil {
			continue
		}
		if class == message.RetentionEph {
			wantRefused = append(wantRefused, wire)
		} else {
			wantSealed = append(wantSealed, wire)
		}
	}
	if len(wantRefused) == 0 || len(wantSealed) == 0 {
		t.Fatalf("the wire alphabet split into %d eph and %d non eph bytes, so one half of this case read nothing",
			len(wantRefused), len(wantSealed))
	}
	if !bytes.Equal(refusedWire, wantRefused) {
		t.Errorf("a session with no eph_root refused %#x, want exactly the eph bytes %#x", refusedWire, wantRefused)
	}
	if !bytes.Equal(sealedWire, wantSealed) {
		t.Errorf("a session with no eph_root sealed %#x, want exactly the non eph bytes %#x; ledger item 152 is ruled 2026-09-13 and the blanket class refusal is lifted in full, so PERMANENT and MEDIA seal here",
			sealedWire, wantSealed)
	}
	t.Logf("no eph_root: %d wire bytes seal %#x; complement is the %d eph bytes %#x, refused with ErrNoEphRoot",
		len(sealedWire), sealedWire, len(refusedWire), refusedWire)
}

// ---------------------------------------------------------------------------
// Task 12 Property 1: the round trip, at both ends of the padding range
// ---------------------------------------------------------------------------

// The two endpoints of the padding range are where an unpadder is wrong.
func TestASealedRecordOpensToExactlyWhatWentIn(t *testing.T) {
	pair := newTestPair(t, "round-trip")
	pair.trackDurable(t)
	for bucket := message.SizeBucket(0); bucket < message.SizeBucketBlob; bucket += 1 {
		// the two endpoints of the range, where an unpadder is wrong. The upper one is the
		// rung's MEASURED application capacity since MASTER section 8.4 put a 193 to 198 octet
		// frame inside it; rung - lpPrefixBytes is what ct_body's PLAINTEXT holds and is no
		// longer what a caller may hand in.
		for _, bodyLength := range []int{0, 1, applicationBodyCapacity[bucket]} {
			if bodyLength < 0 {
				continue
			}
			body := make([]byte, bodyLength)
			for i := range body {
				body[i] = byte(i*7 + 3)
			}
			head := []byte(fmt.Sprintf("head for rung %d length %d", bucket, bodyLength))
			record, err := pair.sender.SealRecord(message.RetentionDurable, 0, false, head, body, 0, nil)
			if err != nil {
				t.Fatalf("SealRecord rung %d length %d: %v", bucket, bodyLength, err)
			}
			if record.Header.SizeBucket != bucket {
				// the smallest rung that fits, so only the exact-fit case lands here
				if bodyLength == applicationBodyCapacity[bucket] {
					t.Errorf("a body of exactly rung %d's measured capacity landed on rung %d", bucket, record.Header.SizeBucket)
				}
			}
			gotHead, gotBody, err := pair.opener.OpenRecord(record)
			if err != nil {
				t.Fatalf("OpenRecord rung %d length %d: %v", bucket, bodyLength, err)
			}
			if !bytes.Equal(gotHead, head) {
				t.Errorf("rung %d length %d: head came back %q, want %q", bucket, bodyLength, gotHead, head)
			}
			if !bytes.Equal(gotBody, body) {
				t.Errorf("rung %d length %d: body came back %d octets, want %d", bucket, bodyLength, len(gotBody), len(body))
			}
		}
	}
}

// The padder and the unpadder are one pair and the round trip is what binds them, so the pair is
// also exercised directly at the edges the sealer cannot reach through SealRecord.
func TestThePadderAndTheUnpadderAreInverses(t *testing.T) {
	for bucket := message.SizeBucket(0); bucket < message.SizeBucketBlob; bucket += 1 {
		rung := message.SizeBucketBytes(bucket)
		for _, length := range []int{0, 1, rung - lpPrefixBytes} {
			body := bytes.Repeat([]byte{byte(length)}, length)
			padded, err := padBody(bucket, body)
			if err != nil {
				t.Fatalf("padBody rung %d length %d: %v", bucket, length, err)
			}
			if len(padded) != rung {
				t.Errorf("padBody rung %d answered %d octets, want %d", bucket, len(padded), rung)
			}
			back, err := unpadBody(bucket, padded)
			if err != nil {
				t.Fatalf("unpadBody rung %d length %d: %v", bucket, length, err)
			}
			if !bytes.Equal(back, body) {
				t.Errorf("rung %d length %d did not round trip", bucket, length)
			}
		}
		if _, err := padBody(bucket, bytes.Repeat([]byte{0x01}, rung-lpPrefixBytes+1)); !errors.Is(err, ErrBodyTooLong) {
			t.Errorf("padding one octet past rung %d's capacity answered %v, want ErrBodyTooLong", bucket, err)
		}
		if _, err := unpadBody(bucket, make([]byte, rung-1)); !errors.Is(err, ErrBodyPadding) {
			t.Errorf("unpadding a short buffer at rung %d answered %v, want ErrBodyPadding", bucket, err)
		}
		if _, err := unpadBody(bucket, make([]byte, rung+1)); !errors.Is(err, ErrBodyPadding) {
			t.Errorf("unpadding a long buffer at rung %d answered %v, want ErrBodyPadding", bucket, err)
		}
	}
	// a length prefix that overruns its rung is refused rather than read as a longer message.
	overrun := make([]byte, message.SizeBucketBytes(message.SizeBucket256))
	overrun[0], overrun[1], overrun[2], overrun[3] = 0xFF, 0xFF, 0xFF, 0xFF
	if _, err := unpadBody(message.SizeBucket256, overrun); !errors.Is(err, ErrBodyPadding) {
		t.Errorf("a length prefix past the end of the rung answered %v, want ErrBodyPadding", err)
	}
}

// ---------------------------------------------------------------------------
// Task 12 Property 2: two sessions
// ---------------------------------------------------------------------------

// THIS IS NOT CP3b AND MUST NOT BE READ AS IT.
//
// CP3b is two CLIENTS: two devices, two MLS leaves, a real join, and the message server between
// them. What this case has is two GroupSessions over ONE group handle, constructed from one
// storage root inside one process -- which is the most this plan's wave 1 can reach, because the
// join is wave 2's and the submit path belongs to sdk plans that do not exist. A passing two
// session round trip is exactly the result somebody will mistake for the milestone, so it is
// stated here rather than in a report nobody reads beside the test.
//
// What it DOES establish is real and is the record layer's half: a record sealed by a session
// that holds only its own ratchets opens in a session that was never handed them, because both
// derived the same ladder from the same epoch.
//
// AND SINCE MASTER SECTION 8.4 THE RECORD IT CARRIES IS A COMMIT RECORD, which is a narrowing and
// is stated rather than quietly done. Two sessions over ONE group handle share one MLS group, so
// the second session's Unprotect of the first's frame meets the same exhausted sending ratchet the
// first would -- open item MG-4. A commit record carries no application frame at all (MASTER
// section 8.4.1's first row), so it is the shape in which this case's own subject, the RECORD
// LAYER's ladder, is still observable through two sessions over one handle. The second clause
// below asserts the application arm's new refusal, so the narrowing is visible here rather than
// only in this comment, and TestADurableRecordSealedByTheFounderOpensAtTheJoiner is where the
// application arm's round trip lives now: two devices, two leaves, a real join.
func TestARecordSealedByOneSessionOpensInASecondOneOverTheSameEpoch(t *testing.T) {
	fixture := newTestSession(t, "two-sessions")
	sender := fixture.session
	receiver, err := NewGroupSession(fixture.handle, testPqSecret(), nil, newStreamIndexMemory(),
		testClock(), testServerNonce())
	if err != nil {
		t.Fatalf("the second session: %v", err)
	}
	defer receiver.Close()
	if err := receiver.TrackSender(fixture.handle.OwnLeafIndex(), message.RetentionDurable, 0, 0, 0); err != nil {
		t.Fatalf("TrackSender at the receiver: %v", err)
	}
	for index := range 3 {
		body := []byte(fmt.Sprintf("message %d", index))
		record, err := sender.SealRecord(message.RetentionDurable, 0, true, []byte("head"), body, 0, nil)
		if err != nil {
			t.Fatalf("SealRecord %d: %v", index, err)
		}
		gotHead, gotBody, err := receiver.OpenCeremonyRecord(record)
		if err != nil {
			t.Fatalf("the second session could not open record %d: %v", index, err)
		}
		if !bytes.Equal(gotHead, []byte("head")) || !bytes.Equal(gotBody, body) {
			t.Errorf("record %d came back %q / %q", index, gotHead, gotBody)
		}
	}

	// AND THE APPLICATION ARM, which is what this case used to carry and no longer can. Two
	// sessions over ONE handle are one MLS member, so the frame the first sealed is one the
	// second has no receiving ratchet for -- and the refusal names the frame rather than the
	// ladder, which is what says the record layer's half above is still doing its work.
	application, err := sender.SealRecord(message.RetentionDurable, 0, false, []byte("head"), []byte("body"), 0, nil)
	if err != nil {
		t.Fatalf("SealRecord of an application record: %v", err)
	}
	if _, _, err := receiver.OpenRecord(application); !errors.Is(err, ErrRecordInnerFrame) {
		t.Errorf("a second session over the same handle opened an application record with %v; two sessions over one handle are one MLS member, so the answer is ErrRecordInnerFrame and open item MG-4 is what is unruled about it",
			err)
	}
}

// ---------------------------------------------------------------------------
// Task 12 Property 3: every field of the record is authenticated
// ---------------------------------------------------------------------------

// The header field class is derived off the TYPE and never off a list, so a field added to
// RecordHeader joins this case without an edit here.
//
// Every field must make the open fail. Some fail at the session's own checks before a key is
// derived and some fail inside an aead; the property is that none of them opens, and which of the
// two refuses is not what is being asserted.
func TestEveryFieldOfARecordIsAuthenticatedByTheOpen(t *testing.T) {
	pair := newTestPair(t, "authenticated")
	header := reflect.TypeOf(message.RecordHeader{})
	if header.NumField() == 0 {
		t.Fatal("message.RecordHeader declares no fields, so this case read nothing")
	}
	moved := 0
	for i := range header.NumField() {
		field := header.Field(i)
		if !field.IsExported() {
			continue
		}
		pair.trackDurable(t)
		record, err := pair.sender.SealRecord(message.RetentionDurable, 0, false,
			[]byte("head"), []byte("body"), 0, nil)
		if err != nil {
			t.Fatalf("SealRecord: %v", err)
		}
		if _, _, err := pair.opener.OpenRecord(record); err != nil {
			t.Fatalf("the unmutated record does not open, so every mutation below would pass over a broken fixture: %v", err)
		}
		pair.trackDurable(t)
		mutated, err := pair.sender.SealRecord(message.RetentionDurable, 0, false,
			[]byte("head"), []byte("body"), 0, nil)
		if err != nil {
			t.Fatalf("SealRecord: %v", err)
		}
		if !sealMoveHeaderField(reflect.ValueOf(&mutated.Header).Elem().Field(i)) {
			t.Errorf("no mutation is defined for RecordHeader.%s of kind %s, so this field is outside a class derived off the type",
				field.Name, field.Type.Kind())
			continue
		}
		moved += 1
		if _, _, err := pair.opener.OpenRecord(mutated); err == nil {
			t.Errorf("a record whose %s was moved still opened; every field of the header is covered by aad_head, which is MASTER invariant I6",
				field.Name)
		}
	}
	if moved == 0 {
		t.Fatal("no header field was moved at all, so this case asserted nothing")
	}
	// and the two ciphertexts, one bit at a time.
	pair.trackDurable(t)
	record, err := pair.sender.SealRecord(message.RetentionDurable, 0, false, []byte("head"), []byte("body"), 0, nil)
	if err != nil {
		t.Fatalf("SealRecord: %v", err)
	}
	for _, part := range []struct {
		name string
		at   func(r *message.Record) []byte
	}{
		{name: "ct_head", at: func(r *message.Record) []byte { return r.CtHead }},
		{name: "ct_body", at: func(r *message.Record) []byte { return r.CtBody }},
	} {
		for _, offset := range []int{0, 1, len(part.at(record)) - 1} {
			for _, bit := range []uint{0, 3, 7} {
				pair.trackDurable(t)
				fresh, err := pair.sender.SealRecord(message.RetentionDurable, 0, false, []byte("head"), []byte("body"), 0, nil)
				if err != nil {
					t.Fatalf("SealRecord: %v", err)
				}
				octets := part.at(fresh)
				octets[offset] ^= 1 << bit
				if _, _, err := pair.opener.OpenRecord(fresh); err == nil {
					t.Errorf("a record with bit %d of %s[%d] flipped still opened", bit, part.name, offset)
				}
			}
		}
	}
}

// sealMoveHeaderField changes one field of a record header to a different legal value, answering
// whether it knew how.
//
// It is keyed on the reflect KIND rather than on the field name, so a field added to
// RecordHeader is moved by whichever arm its type falls in and only a genuinely new kind is
// reported as outside the class.
func sealMoveHeaderField(field reflect.Value) bool {
	switch field.Kind() {
	case reflect.Array:
		if field.Len() == 0 || field.Type().Elem().Kind() != reflect.Uint8 {
			return false
		}
		field.Index(0).SetUint(field.Index(0).Uint() ^ 0xFF)
		return true
	case reflect.Slice:
		if field.Type().Elem().Kind() != reflect.Uint8 {
			return false
		}
		field.SetBytes(append(append([]byte(nil), field.Bytes()...), 0x01))
		return true
	case reflect.Bool:
		field.SetBool(!field.Bool())
		return true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		field.SetUint(field.Uint() + 1)
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Task 12 Properties 4, 5 and 6
// ---------------------------------------------------------------------------

// An out of window index is ErrOutOfWindow, distinguishable by errors.Is, and it does not move
// the receiver's head.
//
// The second half is the one the two phase read exists for: nothing authenticates a stream index
// before a key is derived from it, so a peek that moved the head would let a forged header
// destroy the honest rungs behind it.
func TestAnIndexOutsideTheWindowIsRefusedAndMovesNothing(t *testing.T) {
	pair := newTestPair(t, "out-of-window")
	pair.trackDurable(t)
	first, err := pair.sender.SealRecord(message.RetentionDurable, 0, false, []byte("head"), []byte("body"), 0, nil)
	if err != nil {
		t.Fatalf("SealRecord: %v", err)
	}
	forged := *first
	forged.Header = first.Header
	forged.Header.StreamIndex = first.Header.StreamIndex + uint64(DefaultRecordWindowSize) + 1
	if _, _, err := pair.opener.OpenRecord(&forged); !errors.Is(err, ErrOutOfWindow) {
		t.Errorf("a record naming an index past the window answered %v, want ErrOutOfWindow", err)
	}
	// and the honest record still opens, which is what says the refusal moved nothing.
	head, body, err := pair.opener.OpenRecord(first)
	if err != nil {
		t.Fatalf("the honest record no longer opens after a forged header was refused: %v", err)
	}
	if !bytes.Equal(head, []byte("head")) || !bytes.Equal(body, []byte("body")) {
		t.Error("the honest record opened to the wrong plaintext")
	}
	// a hundred forged headers cost nothing durable either, which is the cumulative half: the
	// committing form moved the head on every ACCEPTED jump, so a hundred of them walked it
	// thousands of rungs past every honest record behind it.
	pair.trackDurable(t)
	honest := []*message.Record{}
	for range 4 {
		record, err := pair.sender.SealRecord(message.RetentionDurable, 0, false, []byte("head"), []byte("body"), 0, nil)
		if err != nil {
			t.Fatalf("SealRecord: %v", err)
		}
		honest = append(honest, record)
	}
	for attempt := range 100 {
		jump := *honest[0]
		jump.Header = honest[0].Header
		jump.Header.StreamIndex = honest[0].Header.StreamIndex + uint64(attempt)*16 + 8
		if _, _, err := pair.opener.OpenRecord(&jump); err == nil {
			t.Fatalf("a forged header at index %d opened", jump.Header.StreamIndex)
		}
	}
	for i, record := range honest {
		if _, _, err := pair.opener.OpenRecord(record); err != nil {
			t.Errorf("honest record %d at index %d no longer opens after a hundred forged headers: %v",
				i, record.Header.StreamIndex, err)
		}
	}
}

// A partial plaintext is never returned beside an error. A caller that rendered whatever came
// back would be rendering attacker chosen octets.
func TestNoPlaintextIsReturnedBesideAnError(t *testing.T) {
	fixture := newTestSession(t, "no-partial")
	fixture.trackOwn(t)
	record, err := fixture.session.SealRecord(message.RetentionDurable, 0, false, []byte("head"), []byte("body"), 0, nil)
	if err != nil {
		t.Fatalf("SealRecord: %v", err)
	}
	for _, broken := range []struct {
		name string
		make func() *message.Record
	}{
		{name: "a nil record", make: func() *message.Record { return nil }},
		{name: "a moved ct_head", make: func() *message.Record {
			copy := *record
			copy.CtHead = append([]byte(nil), record.CtHead...)
			copy.CtHead[0] ^= 0xFF
			return &copy
		}},
		{name: "a moved ct_body", make: func() *message.Record {
			copy := *record
			copy.CtBody = append([]byte(nil), record.CtBody...)
			copy.CtBody[0] ^= 0xFF
			return &copy
		}},
		{name: "another group", make: func() *message.Record {
			copy := *record
			copy.Header.GroupId[0] ^= 0xFF
			return &copy
		}},
		{name: "another epoch", make: func() *message.Record {
			copy := *record
			copy.Header.Epoch += 1
			return &copy
		}},
		{name: "a sender no ratchet is tracked for", make: func() *message.Record {
			copy := *record
			copy.Header.SenderHandle[0] ^= 0xFF
			return &copy
		}},
		{name: "another class, whose sender ladder this session tracks no ratchet on", make: func() *message.Record {
			copy := *record
			copy.Header.RetentionClass = message.RetentionPermanent
			return &copy
		}},
		{name: "the blob rung", make: func() *message.Record {
			copy := *record
			copy.Header.SizeBucket = message.SizeBucketBlob
			return &copy
		}},
	} {
		head, body, err := fixture.session.OpenRecord(broken.make())
		if err == nil {
			t.Errorf("%s opened", broken.name)
			continue
		}
		if head != nil || body != nil {
			t.Errorf("%s answered %d octets of head and %d of body beside its error", broken.name, len(head), len(body))
		}
	}
	// and the sentinels are distinguishable, which is what open item M1-15 leaves sdk to match on.
	copy := *record
	copy.Header.SenderHandle[0] ^= 0xFF
	if _, _, err := fixture.session.OpenRecord(&copy); !errors.Is(err, ErrNoReceiverRatchet) {
		t.Errorf("a record from an untracked sender answered %v, want ErrNoReceiverRatchet", err)
	}
}

// Property 5, held off the SOURCE because no input can reach the path it is about.
//
// Measured while mutation testing this batch: returning the head plaintext beside a body refusal
// survives every behavioural case here, and so does removing the outer guard as well, and the
// reason is that the two are unreachable rather than untested. body_hash is checked before either
// aead runs, and aad_head covers body_hash, so a record whose ct_body was moved is refused before
// the head is opened and one whose body_hash was moved fails at the HEAD. There is no input that
// opens ct_head and then fails ct_body.
//
// A guard nothing can reach is still a guard worth keeping -- the reachability argument above
// rests on two other checks, and either of them moving would make this one load bearing again --
// so it is held by a reading of the source instead. The scope question (R3a): the SCOPE is this
// package's production source. The CLASS is every declaration answering exactly ([]byte, []byte,
// error), derived off the signature and never listed, and the assertion is that no return
// statement in one of them answers a non-nil error beside a plaintext.
func TestNoDeclarationAnsweringTwoPlaintextsReturnsOneBesideAnError(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	judged := 0
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil || !sealAnswersTwoPlaintextsAndAnError(function) {
				continue
			}
			judged += 1
			ast.Inspect(function.Body, func(node ast.Node) bool {
				returned, isReturn := node.(*ast.ReturnStmt)
				if !isReturn || len(returned.Results) != 3 {
					return true
				}
				if sealIsNilLiteral(returned.Results[2]) {
					return true
				}
				for at, result := range returned.Results[:2] {
					if !sealIsNilLiteral(result) {
						t.Errorf("%s returns result %d beside a non-nil error; a caller that rendered whatever came back would be rendering attacker chosen octets",
							function.Name.Name, at)
					}
				}
				return true
			})
		}
	}
	if judged == 0 {
		t.Fatal("no production declaration answers two plaintexts and an error, so this gate is reporting clean having read nothing")
	}
	t.Logf("%d declaration(s) answer two plaintexts and an error", judged)
}

// sealAnswersTwoPlaintextsAndAnError reads the signature rather than the name: exactly three
// results, the first two byte slices and the last an error.
func sealAnswersTwoPlaintextsAndAnError(function *ast.FuncDecl) bool {
	if function.Type.Results == nil {
		return false
	}
	kinds := []string{}
	for _, field := range function.Type.Results.List {
		rendered := ""
		switch result := field.Type.(type) {
		case *ast.ArrayType:
			if element, isIdentifier := result.Elt.(*ast.Ident); isIdentifier && result.Len == nil {
				rendered = "[]" + element.Name
			}
		case *ast.Ident:
			rendered = result.Name
		}
		count := max(len(field.Names), 1)
		for range count {
			kinds = append(kinds, rendered)
		}
	}
	return len(kinds) == 3 && kinds[0] == "[]byte" && kinds[1] == "[]byte" && kinds[2] == "error"
}

// sealIsNilLiteral answers whether one expression is the identifier nil.
func sealIsNilLiteral(expression ast.Expr) bool {
	identifier, isIdentifier := expression.(*ast.Ident)
	return isIdentifier && identifier.Name == "nil"
}

// OpenRecord never trusts RecordId. It is server assigned and authenticated by nothing.
//
// Both halves: the open path's source names no such field, derived off the syntax tree; and a
// record whose RecordId is anything at all still opens.
func TestOpenRecordNeverTrustsTheRecordId(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	reachable := map[string]bool{"openRecordOnLoop": true}
	calls := map[string]map[string]bool{}
	bodies := map[string]*ast.FuncDecl{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			bodies[function.Name.Name] = function
			calls[function.Name.Name] = map[string]bool{}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall {
					return true
				}
				switch found := call.Fun.(type) {
				case *ast.Ident:
					calls[function.Name.Name][found.Name] = true
				case *ast.SelectorExpr:
					calls[function.Name.Name][found.Sel.Name] = true
				}
				return true
			})
		}
	}
	if _, isDeclared := bodies["openRecordOnLoop"]; !isDeclared {
		t.Fatal("nothing named openRecordOnLoop is declared, so this gate is walking an empty open path")
	}
	for grew := true; grew; {
		grew = false
		for name := range reachable {
			for callee := range calls[name] {
				if bodies[callee] != nil && !reachable[callee] {
					reachable[callee] = true
					grew = true
				}
			}
		}
	}
	for name := range reachable {
		ast.Inspect(bodies[name].Body, func(node ast.Node) bool {
			selector, isSelector := node.(*ast.SelectorExpr)
			if isSelector && selector.Sel.Name == "RecordId" {
				t.Errorf("%s is on the open path and names RecordId; it is server assigned, it is in neither aad and in neither preimage, and section 5.1 says the server populates it on read",
					name)
			}
			return true
		})
	}
	t.Logf("%d declaration(s) on the open path: %v", len(reachable), slices.Sorted(maps.Keys(reachable)))

	pair := newTestPair(t, "record-id")
	pair.trackDurable(t)
	record, err := pair.sender.SealRecord(message.RetentionDurable, 0, false, []byte("head"), []byte("body"), 0, nil)
	if err != nil {
		t.Fatalf("SealRecord: %v", err)
	}
	if record.RecordId != 0 {
		t.Errorf("SealRecord set RecordId to %d; it is the server's to assign", record.RecordId)
	}
	record.RecordId = 1 << 40
	if _, _, err := pair.opener.OpenRecord(record); err != nil {
		t.Errorf("a record whose RecordId the server had assigned did not open: %v", err)
	}
}

// The attachment travels once and is compared in both directions, which is the landed
// ErrServerAttachmentMismatch doing the work rather than a second nil check here.
func TestTheAttachmentTheSealerEncodesIsTheOneBothPreimagesCover(t *testing.T) {
	fixture := newTestSession(t, "attachment")
	fixture.trackOwn(t)
	// a nil attachment and an explicit AttachmentNone contribute the same octets, which is the
	// reading attachment.go already chose and which this file calls rather than re-decides.
	nilRecord, err := fixture.session.SealRecord(message.RetentionDurable, 0, false, []byte("head"), []byte("body"), 0, nil)
	if err != nil {
		t.Fatalf("SealRecord with no attachment: %v", err)
	}
	noneRecord, err := fixture.session.SealRecord(message.RetentionDurable, 0, false, []byte("head"), []byte("body"), 0,
		&message.ServerAttachment{Kind: message.AttachmentNone})
	if err != nil {
		t.Fatalf("SealRecord with an AttachmentNone attachment: %v", err)
	}
	if len(nilRecord.Header.ServerAttachment) != 0 || len(noneRecord.Header.ServerAttachment) != 0 {
		t.Errorf("an ordinary record carries %d and %d octets of attachment, want none from both",
			len(nilRecord.Header.ServerAttachment), len(noneRecord.Header.ServerAttachment))
	}
	// and a record whose header attachment is moved after the seal does not open, which is the
	// disagreement AADHead refuses in both directions.
	moved := *nilRecord
	moved.Header.ServerAttachment = []byte{0x00, 0x01}
	if _, _, err := fixture.session.OpenRecord(&moved); err == nil {
		t.Error("a record whose server attachment was moved after sealing still opened")
	}
}

// THE SEAL DOOR PRODUCES A KIND 0x0005 COMMIT END TO END, and the two keys are not in it.
//
// This is ledger item 244 measured at the only seal door in the tree. SealRecord encodes
// through message.EncodeServerAttachment, which asks serverAttachmentKindServed; until
// 2026-09-23 that map excluded AttachmentEpochDigest and this call was refused BY NAME, so
// no committer anywhere could produce the record the amendment exists to produce. Measured
// then, with the control in the same call: EncodeEpochDigestAttachment answered 100 octets
// and nil while EncodeServerAttachment on the same body was refused, and a kind 0x0001 body
// answered 136 octets.
//
// WHAT IS ASSERTED IS THE PROPERTY AND NOT THE COUNT. The record seals, ParseRecord answers
// is_commit with the attachment slot identical to the octets that went in, the slot parses
// at spec B section 5.1 check 3's door as kind 0x0005, and CheckEpochKeysDigest binds the
// digest to the two keys the REQUEST would carry (ruling 33) under the record header's own
// group id. Then the thing item 244 is about: the sealed record's octets DO NOT CONTAIN
// either key, and the kind 0x0001 control sealed by the same call in the same test DOES
// contain both. A removed member who can fetch the commit that removed them learns nothing
// from the first record and learns the next epoch's read and write keys from the second.
func TestTheSealDoorProducesAnEpochDigestCommitThatCarriesNeitherKey(t *testing.T) {
	fixture := newTestSession(t, "epoch-digest-seal")
	fixture.trackOwn(t)

	var groupId [32]byte
	copy(groupId[:], fixture.handle.GroupId())
	writeKey := fillKeyBytes(0x40)
	readKey := fillKeyBytes(0x80)
	const opensEpoch = 2

	public := message.EpochDigestAttachment{
		Epoch:             opensEpoch,
		AlgId:             0x0031,
		MediaTtlSeconds:   2592000,
		DurableTtlSeconds: 0xFFFFFFFF,
		GroupContextHash:  fillKeyBytes(0xC0),
		ExpectedWrapCount: 3,
	}
	digest, err := message.NewEpochDigestAttachment(groupId, public, writeKey, readKey)
	if err != nil {
		t.Fatalf("NewEpochDigestAttachment: %v", err)
	}
	sealed, err := fixture.session.SealRecord(message.RetentionDurable, 0, true, []byte("head"), []byte("body"), 0,
		&message.ServerAttachment{Kind: message.AttachmentEpochDigest, EpochDigest: digest})
	if err != nil {
		t.Fatalf("SealRecord refused a kind 0x0005 commit, which is the whole of what this test is for: %v", err)
	}

	// the record round trip, and the two properties a commit is refused for losing
	bs, err := message.EncodeRecord(sealed)
	if err != nil {
		t.Fatalf("the sealed kind 0x0005 commit does not encode: %v", err)
	}
	parsed, err := message.ParseRecord(bs)
	if err != nil {
		t.Fatalf("the sealed kind 0x0005 commit does not parse back: %v", err)
	}
	if !parsed.Header.IsCommit {
		t.Error("the kind 0x0005 record came back with is_commit clear")
	}
	if !bytes.Equal(parsed.Header.ServerAttachment, sealed.Header.ServerAttachment) {
		t.Errorf("the attachment slot came back as %d octets and was sealed as %d",
			len(parsed.Header.ServerAttachment), len(sealed.Header.ServerAttachment))
	}

	// the slot parses at section 5.1 check 3's door -- which is what the server runs -- and
	// the digest it carries is the one the two keys hash to under this record's own group
	attachment, err := message.ParseServerAttachment(parsed.Header.ServerAttachment)
	if err != nil {
		t.Fatalf("section 5.1 check 3's door refused the sealed attachment: %v", err)
	}
	if attachment.Kind != message.AttachmentEpochDigest {
		t.Fatalf("the sealed attachment parses as kind 0x%04x, want the digest kind", uint16(attachment.Kind))
	}
	if err := message.CheckEpochKeysDigest(parsed.Header.GroupId, attachment.EpochDigest, writeKey, readKey); err != nil {
		t.Errorf("the digest in the sealed record is not over the two keys the request carries: %v", err)
	}

	// THE INLINE CONTROL: the same call, the same session, kind 0x0001
	control := &message.ServerAttachment{Kind: message.AttachmentEpoch, Epoch: &message.EpochAttachment{
		Epoch:             opensEpoch,
		AlgId:             0x0031,
		WriteKey:          writeKey,
		ReadKey:           readKey,
		MediaTtlSeconds:   public.MediaTtlSeconds,
		DurableTtlSeconds: public.DurableTtlSeconds,
		GroupContextHash:  public.GroupContextHash,
		ExpectedWrapCount: public.ExpectedWrapCount,
	}}
	controlRecord, err := fixture.session.SealRecord(message.RetentionDurable, 0, true, []byte("head"), []byte("body"), 0, control)
	if err != nil {
		t.Fatalf("SealRecord refused the kind 0x0001 control, so the seal above says nothing about the kind: %v", err)
	}
	controlBytes, err := message.EncodeRecord(controlRecord)
	if err != nil {
		t.Fatalf("the sealed kind 0x0001 control does not encode: %v", err)
	}
	if !controlRecord.Header.IsCommit {
		t.Error("the kind 0x0001 control came back with is_commit clear")
	}

	// ITEM 244, AS A PROPERTY OF THE OCTETS ON THE WIRE. The keys are searched for in the
	// WHOLE record and not in the attachment slot alone, because the question is what a
	// party who fetched this record can read out of it, not where this package put it.
	for _, key := range []struct {
		name string
		key  []byte
	}{{"write_key", writeKey}, {"read_key", readKey}} {
		if bytes.Contains(bs, key.key) {
			t.Errorf("the kind 0x0005 commit's %d octets contain %s, and the amendment is that they do not",
				len(bs), key.name)
		}
		if !bytes.Contains(controlBytes, key.key) {
			t.Errorf("the kind 0x0001 control's %d octets do NOT contain %s, so the search above proves nothing",
				len(controlBytes), key.name)
		}
	}
	t.Logf("kind 0x0005: %d record octets carrying a %d octet attachment and neither key; "+
		"kind 0x0001 control: %d record octets carrying a %d octet attachment and both keys",
		len(bs), len(sealed.Header.ServerAttachment), len(controlBytes), len(controlRecord.Header.ServerAttachment))
}

// One 32 octet key value whose every octet is distinct from every other key's in this file,
// so a test that found one key where it expected the other would say so.
func fillKeyBytes(tag byte) []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = tag + byte(i)
	}
	return key
}

// expire_at is a clock read and the clock is injected, which is what keeps this package free of a
// timing sensitive test.
func TestAnExpireAtThatHasAlreadyPassedIsRefused(t *testing.T) {
	fixture := newTestSession(t, "expire-at")
	now := uint64(testClock()())
	if _, err := fixture.session.SealRecord(message.RetentionDurable, 0, false,
		[]byte("head"), []byte("body"), now-1, nil); !errors.Is(err, ErrRecordExpired) {
		t.Errorf("an expire_at in the past answered %v, want ErrRecordExpired", err)
	}
	if _, err := fixture.session.SealRecord(message.RetentionDurable, 0, false,
		[]byte("head"), []byte("body"), now+1, nil); err != nil {
		t.Errorf("an expire_at in the future was refused: %v", err)
	}
	if _, err := fixture.session.SealRecord(message.RetentionDurable, 0, false,
		[]byte("head"), []byte("body"), 0, nil); err != nil {
		t.Errorf("an unset expire_at was refused: %v", err)
	}
}
