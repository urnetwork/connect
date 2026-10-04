// S2-2: the nonce that moves, the one field it moves, and the two files that may not move it.
//
// WHAT THE DEFECT WAS. GroupSession.serverNonce was fixed at construction with no setter, and
// spec A section 5.7 has the server draw a fresh nonce at EVERY Hello and carry it in
// HelloResponse. write_auth is a mac over that nonce, so the first reconnect invalidated every
// record the session had sealed since it opened: any session that outlived one connection was
// wrong. RebindServerNonce is the setter and ReauthRecord is what makes a rebind observable on a
// record that was already sealed.
//
// THE BLAST RADIUS, DERIVED AGAINST THE TREE RATHER THAN TRANSCRIBED FROM THE PLAN. The query is
//
//	git grep -n 'serverNonce' -- 'messagegroup/*.go' | grep -v _test
//
// and its production READS are one: seal.go's authenticate, handing the field to
// message.ComputeWriteAuth, whose answer lands in record.WriteAuth. The nonce is not an input to
// AADHead, AADBody, RecordAeadHead, RecordAeadBody, StorageRoot, DeriveClassKeys, message.WriteKey,
// message.ReadKey, SenderHandle or StreamKey, and openRecordOnLoop never reads write_auth at all.
// So exactly ONE sealed value binds the nonce, a rebind must recompute exactly that one, and
// nothing already sealed becomes unopenable -- which is the last clause held as a property below
// and not as a sentence here.
//
// WHAT IS OBSERVED THROUGH WHAT. The setter's principal clause cannot be seen without the re-auth:
// there is no route on which the nonce is the sole free variable that does not re-authenticate one
// record, because sealing a second record instead moves stream_index, which moves record_key[i],
// which moves both ciphertexts and the handle -- a difference no assertion could attribute to the
// nonce. That is why the two land together and why the setter's own commit could not claim it.
package messagegroup

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/message"
)

// A second nonce, distinct from testServerNonce() in both its octets and its length, so a case
// that rebinds to it is changing something a mac can see and a case that compares the two cannot
// be satisfied by a reslice.
func noncerebindSecondNonce() []byte {
	return []byte("second-connection-hello-nonce")
}

// ---------------------------------------------------------------------------
// Task 3 Property 1: the setter replaces the field and refuses what the
// constructor refuses, with the constructor's own sentinel
// ---------------------------------------------------------------------------

// noncerebindConstructorTakes answers whether NewGroupSession accepts a nonce of these octets,
// asked WITHOUT building a session.
//
// The constructor checks the nonce and then checks pq_secret, so a nil pq_secret makes the NEXT
// refusal the answer to this question: ErrSessionServerNonce means the nonce was refused and
// ErrNilPqSecret means it was accepted. Anything else is this probe having stopped measuring what
// it claims, and is a fatal rather than a false.
//
// The handle is never touched on either path -- both returns are above the constructor's first
// handle.GroupId() call -- which is what makes it legal to ask this about a handle a live session
// already owns, and what keeps the probe from being a second session over one group.
func noncerebindConstructorTakes(t *testing.T, handle GroupHandle, nonce []byte) bool {
	t.Helper()
	session, err := NewGroupSession(handle, nil, nil, newStreamIndexMemory(), testClock(), nonce)
	if session != nil {
		session.Close()
		t.Fatalf("the width probe built a session over a nil pq_secret, so it is no longer measuring the nonce door")
	}
	switch {
	case errors.Is(err, ErrSessionServerNonce):
		return false
	case errors.Is(err, ErrNilPqSecret):
		return true
	}
	t.Fatalf("the width probe got %v, which is neither of the two refusals it reads the constructor's answer off; it has stopped measuring the nonce door", err)
	return false
}

// Property 1, the emptiness half: the two spellings of "no nonce" are refused with the sentinel
// NewGroupSession refuses an empty one with.
func TestTheRebindRefusesAnEmptyNonceWithTheConstructorsOwnSentinel(t *testing.T) {
	fixture := newTestSession(t, "rebind-empty")
	for _, empty := range []struct {
		name  string
		nonce []byte
	}{
		{name: "nil", nonce: nil},
		{name: "a zero length slice", nonce: []byte{}},
	} {
		err := fixture.session.RebindServerNonce(empty.nonce)
		if !errors.Is(err, ErrSessionServerNonce) {
			t.Errorf("RebindServerNonce(%s) = %v, want %v; the field write_auth is macced over cannot be emptied, and without this refusal message.ComputeWriteAuth panics on the next seal rather than answering a bad mac",
				empty.name, err, ErrSessionServerNonce)
		}
	}
	// and the field is untouched: a good nonce still seals, which is what says the refusal
	// returned before the erase rather than after it.
	if err := fixture.session.RebindServerNonce(noncerebindSecondNonce()); err != nil {
		t.Fatalf("RebindServerNonce after two refusals: %v; the refusal reached the field", err)
	}
}

// Property 1, the agreement half: TWO DOORS ONTO ONE FIELD, ONE RULE.
//
// The class is derived rather than listed: every width from zero to sixty four is put to the
// constructor and to the setter, and the two answers must agree at every one of them. A setter
// that refused a nonce the constructor accepts -- a width check, say -- is two rules over one
// field, and a reader meeting a thirty one octet nonce would have to derive which door it arrived
// through.
//
// WHAT THIS DELIBERATELY DOES NOT ASSERT is that a nonce is thirty two octets. MASTER section 7
// and spec A section 5.7 both fix the width there; this package's constructor checks only
// emptiness, and making the setter stricter than the constructor is the defect above. The
// disagreement between the specification and the package is real, is not this gate's to rule, and
// is open item K1-2 -- which is why it is written down here beside the gate that would otherwise
// look like it had ruled it.
func TestTheRebindAndTheConstructorRefuseExactlyTheSameNonces(t *testing.T) {
	fixture := newTestSession(t, "rebind-agreement")
	disagreed := []int{}
	accepted := []int{}
	for width := 0; width <= 64; width += 1 {
		nonce := make([]byte, width)
		for i := range nonce {
			nonce[i] = byte(0x40 + i)
		}
		byTheConstructor := noncerebindConstructorTakes(t, fixture.handle, nonce)
		refusal := fixture.session.RebindServerNonce(nonce)
		byTheSetter := refusal == nil
		if !byTheSetter && !errors.Is(refusal, ErrSessionServerNonce) {
			t.Fatalf("RebindServerNonce of %d octets refused with %v, which is not the nonce sentinel at all", width, refusal)
		}
		if byTheConstructor != byTheSetter {
			disagreed = append(disagreed, width)
		}
		if byTheSetter {
			accepted = append(accepted, width)
		}
	}
	if len(disagreed) != 0 {
		t.Errorf("the constructor and the setter disagree about %d width(s): %v; two doors onto serverNonce with two rules is two rules, and open item K1-2 is where the specification's thirty two octets get ruled -- not here",
			len(disagreed), disagreed)
	}
	if len(accepted) == 0 {
		t.Fatal("neither door accepted a nonce of any width from 0 to 64, so this gate agreed about nothing")
	}
	if slices.Contains(accepted, 0) {
		t.Error("width 0 was accepted by both doors, so the agreement this gate reports is agreement that the field may be emptied")
	}
	t.Logf("widths 0..64: %d accepted by both doors (%v), %d disagreement(s)", len(accepted), accepted, len(disagreed))
}

// ---------------------------------------------------------------------------
// Task 3 Property 3: a closed session refuses the rebind
// ---------------------------------------------------------------------------

// A rebind on a closed session is refused, and what the refusal is standing in front of is worth
// naming: zeroizeOnLoop has already erased writeKey, so the next ReauthRecord would reach
// message.ComputeWriteAuth with a zero length key and PANIC rather than refuse, on whichever
// goroutine posted it.
//
// The refusal comes back out of do, whose send sees stopped closed. The self.closing clause in the
// posted body is unreachable in this tree for the reason EpochKeys's comment measures, and this
// case does not claim to drive it.
func TestAClosedSessionRefusesTheRebind(t *testing.T) {
	fixture := newTestSession(t, "rebind-closed")
	if err := fixture.session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := fixture.session.RebindServerNonce(noncerebindSecondNonce()); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("RebindServerNonce after Close = %v, want %v", err, ErrSessionClosed)
	}
	// and an empty nonce on a closed session is still a closed session: the refusals are
	// ordered, and a door that answered the argument's problem first would be a door that
	// reached a closed session's state to find it.
	if err := fixture.session.RebindServerNonce(nil); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("RebindServerNonce(nil) after Close = %v, want %v", err, ErrSessionClosed)
	}
}

// ---------------------------------------------------------------------------
// Task 3 Property 3, the half a behavioural case cannot reach: the write to
// the field stands on the loop
// ---------------------------------------------------------------------------

// A rebind whose WRITE stood off the loop would be a write racing the goroutine that seals, and
// nothing behavioural in this package would say so: the closed-session refusal comes back out of
// do whether or not the body that do posts is the body that writes, so Property 3 stays green over
// a setter that posts an empty command and then assigns the field on the caller's goroutine.
//
// MEASURED, AND IT IS WHY THIS GATE EXISTS RATHER THAN BEING LEFT TO THE LANDED ONE.
// TestEveryMethodOfAGroupSessionReachesItsStateOnlyOnTheLoop derives its loop-owned field set from
// the fields some on-loop body TOUCHES, so a field that no on-loop body touches is in no class at
// all -- and serverNonce was exactly that field before this slice: the constructor writes it in a
// composite literal and seal.go's authenticate reads it through builder.session, neither of which
// is a self.<field> mention. Moving this setter's write out of its posted closure therefore
// survived an unfiltered run of this package. It does not survive this one.
//
// THE CLASS IS THE WRITES AND THE COMPLEMENT IS THE READS, printed. The scope is this package's
// production source; the class is every assignment in it whose left hand side is a selector named
// serverNonce; the complement is every other mention of that name, which this gate does NOT hold
// and names so that a reader knows it.
func TestEveryWriteToTheSessionsServerNonceStandsOnTheLoop(t *testing.T) {
	fileSet, sources := messagegroupProductionSources(t)
	onLoop := []string{}
	offLoop := []string{}
	notWrites := []string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			inside := map[ast.Node]bool{}
			for _, closure := range sessionDoClosures(function.Body) {
				ast.Inspect(closure, func(node ast.Node) bool {
					inside[node] = true
					return true
				})
			}
			wholeBodyOnLoop := sessionRunsOnTheLoop(function)
			ast.Inspect(function.Body, func(node ast.Node) bool {
				assignment, isAssignment := node.(*ast.AssignStmt)
				if !isAssignment {
					if selector, isSelector := node.(*ast.SelectorExpr); isSelector && selector.Sel.Name == "serverNonce" {
						notWrites = append(notWrites, noncerebindWhere(fileSet, source.path, function.Name.Name, selector.Pos()))
					}
					return true
				}
				for _, target := range assignment.Lhs {
					selector, isSelector := target.(*ast.SelectorExpr)
					if !isSelector || selector.Sel.Name != "serverNonce" {
						continue
					}
					where := noncerebindWhere(fileSet, source.path, function.Name.Name, selector.Pos())
					if wholeBodyOnLoop || inside[node] {
						onLoop = append(onLoop, where)
					} else {
						offLoop = append(offLoop, where)
					}
				}
				return true
			})
		}
	}
	// the left hand side of a write is itself a selector, so it is cut from the complement here
	// rather than counted on both sides of the split the gate is reporting.
	for _, where := range append(append([]string{}, onLoop...), offLoop...) {
		notWrites = slices.DeleteFunc(notWrites, func(mention string) bool { return mention == where })
	}
	if len(onLoop)+len(offLoop) == 0 {
		t.Fatal("no assignment to a serverNonce field was read out of this package's production source, so this gate is holding an empty class and would report the same clean run over a setter that writes the field from any goroutine at all")
	}
	slices.Sort(notWrites)
	t.Logf("%d write(s) to serverNonce stand on the loop: %v; %d stand off it: %v; the COMPLEMENT this gate does not hold is the %d mention(s) that are not writes: %v",
		len(onLoop), onLoop, len(offLoop), offLoop, len(notWrites), slices.Compact(notWrites))
	for _, where := range offLoop {
		t.Errorf("%s writes serverNonce without posting a command; seal.go's authenticate reads that field on the loop goroutine, so a write anywhere else is a write racing a seal -- and the landed loop gate cannot see this one, because it derives its field set from the fields an on-loop body touches and this is the write that would have put serverNonce in it",
			where)
	}
}

// noncerebindWhere is one mention, spelled file:line (declaration), so a failure names the site
// rather than the field.
func noncerebindWhere(fileSet *token.FileSet, path string, declaration string, at token.Pos) string {
	return fmt.Sprintf("%s:%d (%s)", path, fileSet.Position(at).Line, declaration)
}

// ---------------------------------------------------------------------------
// Task 3 Property 4: no exported method of GroupSession answers the nonce
// ---------------------------------------------------------------------------

// The number of exported methods on *GroupSession this slice's commits make, pinned so that a
// method added without a thought about this gate moves a number rather than sliding in under a
// class the gate derives.
//
// The query is
//
//	git grep -n 'func (self \*GroupSession) [A-Z]' -- 'messagegroup/*.go' | grep -v _test
//
// and R6 clause (a) beside it: piping that through grep -c 'RebindServerNonce' returns 1 and
// through grep -c 'ReauthRecord' returns 1, so the answer contains the two members this slice
// added rather than merely counting to a number.
//
// It was ten until the seal lift of 2026-09-13 added InstallEphRoot, which is the eleventh: a
// setter for eph_root, the one key of a session that no derivation can produce. It answers no
// octets at all -- it takes them -- so it joins the exported class this count pins and does not
// join the octet answering subset the ban is over.
//
// MASTER section 8.4's second pass of 2026-09-15 adds the twelfth and thirteenth, and BOTH answer
// octets, which is why the count and the ban are stated separately. OpenCeremonyRecord is the arm
// split's other door -- the arm that carries no inner frame had been sharing OpenRecord's, which is
// what made MASTER section 8.4.3 opt-out. MessageIdOf is the door MASTER section 8.4.5's message_id
// had none of: the derivation was exported and had no caller anywhere. Neither reaches
// self.serverNonce, so the banned subset is still empty and the complement this gate prints is
// still what it narrowed away.
//
// Ledger item 241's multi-epoch open adds the fourteenth and fifteenth, and NEITHER answers octets:
// InstallPastEpochLoader takes the door a prior epoch's schedule is rebuilt through, and
// TrackSenderAt is TrackSender for a prior epoch. Neither reaches self.serverNonce -- a prior
// epoch's schedule is a read schedule and holds no write key for the nonce to be mac'd under --
// so the banned subset stays empty and the complement is unchanged.
//
// Ledger item 242's R4 adds the sixteenth, and this one DOES answer octets: RoleAt answers the
// credential identity standing at one leaf at one epoch, beside that identity's role. It joins the
// complement this gate prints rather than the banned subset, and for a reason stronger than "its
// body does not mention the field": the identity it answers is read off a RATCHET TREE -- the
// session's own handle for its own epoch, a prior epoch's rebuilt one otherwise -- and the write
// key the nonce is mac'd under is on neither. So the banned subset stays empty and the complement
// grows by one.
//
// Ledger item 251's ruling 40 adds the seventeenth and eighteenth, and NEITHER answers octets:
// InstallPqSecret takes a past epoch's pq_secret and answers an error, DeclarePqSecretRotated takes
// nothing and answers an error. Both are doors INTO the session rather than readings out of it,
// which is exactly the shape this narrowing is about -- the hazard is a getter that hands a fixture
// the value the subject holds, and a door answering only error can hand nothing back. So the banned
// subset stays empty and the complement is unchanged. Worth saying out loud rather than leaving to
// the count, because InstallPqSecret does move secret octets: they move IN, and nothing on this
// surface answers them again.
const noncerebindExportedSessionMethods = 18

// Property 4 -- THE NARROWING, AND IT IS THE ONE PLACE IN THIS FILE WHERE AN EMPTINESS IS THE
// PROPERTY RATHER THAN A DEFECT IN IT.
//
// The class is every exported method of *GroupSession, read off the syntax tree. Out of it is
// carved the set that ANSWERS OCTETS -- a result that is a []byte or a [N]byte -- and out of THAT
// the set whose body also reaches self.serverNonce. The last set must be empty.
//
// The gate fatals on an empty enclosing class and on an empty octet-answering set, and both
// fatals are the point: a gate that derived no exported method at all, or that found no method
// answering octets at all, would report exactly the same clean run over a surface that had grown a
// getter. The complement is PRINTED -- the exported methods that do answer octets and do not
// answer the nonce -- because a narrowing whose complement is empty is a narrowing that has
// covered everything, and a reader must not have to infer which reading this is.
//
// WHAT THE BAN BUYS, stated so it is not mistaken for tidiness: keysource_test.go's reproduction is
// handed server_nonce as one of the three values THE TEST INJECTED, and a getter is the one thing
// that would let a fixture hand it the value the session HOLDS instead -- at which point the third
// input stops being independent and the subject starts agreeing with itself.
//
// The reach is a FIELD MENTION and not a dataflow, which widens the answering set rather than
// narrowing it: a method that answers octets and merely names the nonce is reported. That is the
// direction a gate may be wrong in.
func TestNoExportedMethodOfAGroupSessionAnswersTheServerNonce(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	exported := []string{}
	answersOctets := []string{}
	answersTheNonce := []string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil || sessionReceiverName(function) != "GroupSession" {
				continue
			}
			if !ast.IsExported(function.Name.Name) {
				continue
			}
			exported = append(exported, function.Name.Name)
			if !noncerebindAnswersOctets(function) {
				continue
			}
			answersOctets = append(answersOctets, function.Name.Name)
			if noncerebindMentionsTheNonce(function.Body) {
				answersTheNonce = append(answersTheNonce, function.Name.Name)
			}
		}
	}
	slices.Sort(exported)
	slices.Sort(answersOctets)
	slices.Sort(answersTheNonce)
	if len(exported) == 0 {
		t.Fatal("no exported method of *GroupSession was read out of this package's production source, so the subset this gate requires to be empty is empty because the gate read nothing")
	}
	if len(answersOctets) == 0 {
		t.Fatal("no exported method of *GroupSession answers a []byte or a [N]byte at all, so the nonce answering subset is empty for a reason that has nothing to do with the nonce; this gate would report the same clean run over a surface carrying a getter")
	}
	complement := []string{}
	for _, name := range answersOctets {
		if !slices.Contains(answersTheNonce, name) {
			complement = append(complement, name)
		}
	}
	if len(complement) == 0 {
		t.Fatal("every exported method of *GroupSession that answers octets answers the nonce, so the complement of this narrowing is empty -- which is the reading a gate must say out loud rather than leave a reader to infer")
	}
	t.Logf("%d exported method(s) on *GroupSession: %v; %d answer octets: %v; the COMPLEMENT of the ban is those %d (%v) and the banned subset is %v",
		len(exported), exported, len(answersOctets), answersOctets, len(complement), complement, answersTheNonce)
	for _, name := range answersTheNonce {
		t.Errorf("(*GroupSession).%s is exported, answers octets and reaches self.serverNonce; there is no getter for the nonce on purpose -- a fixture that could ask the session for it would hand keysource_test.go's reproduction the value the subject holds instead of the value the test injected, and the third of its three independent inputs would become the subject agreeing with itself",
			name)
	}
	if len(exported) != noncerebindExportedSessionMethods {
		t.Errorf("%d exported methods are declared on *GroupSession and this slice's commits make it %d; the number moves by one per exported method and a method that arrived without moving it arrived without a thought about this narrowing",
			len(exported), noncerebindExportedSessionMethods)
	}
	for _, added := range []string{"RebindServerNonce", "ReauthRecord"} {
		if !slices.Contains(exported, added) {
			t.Errorf("no exported method named %s is declared on *GroupSession, so this gate is holding a class that does not contain the members this slice added and would report clean having read some other surface",
				added)
		}
	}
}

// noncerebindAnswersOctets is whether any result of this declaration is a []byte or a [N]byte.
//
// It is read off the type and not off a name, so message.RetentionClass and every other named
// type stays outside: what the ban is about is a method that hands a caller the octets, and
// [16]byte -- SenderHandle's answer -- is as much a hand-off as []byte is.
func noncerebindAnswersOctets(function *ast.FuncDecl) bool {
	if function.Type.Results == nil {
		return false
	}
	for _, result := range function.Type.Results.List {
		array, isArray := result.Type.(*ast.ArrayType)
		if !isArray {
			continue
		}
		element, isName := array.Elt.(*ast.Ident)
		if !isName || element.Name != "byte" {
			continue
		}
		if array.Len == nil {
			return true
		}
		if _, isLiteral := array.Len.(*ast.BasicLit); isLiteral {
			return true
		}
	}
	return false
}

// noncerebindMentionsTheNonce is whether a body names self.serverNonce anywhere, inside a posted
// closure or out of one.
func noncerebindMentionsTheNonce(body ast.Node) bool {
	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		selector, isSelector := node.(*ast.SelectorExpr)
		if !isSelector || selector.Sel.Name != "serverNonce" {
			return true
		}
		if name, isName := selector.X.(*ast.Ident); isName && name.Name == "self" {
			found = true
		}
		return true
	})
	return found
}

// ---------------------------------------------------------------------------
// Task 3 Property 5: the two test files whose own correctness needs the
// INJECTED nonce may not rebind it
// ---------------------------------------------------------------------------

// The two files, and each is named for what its own correctness rests on rather than by taste.
//
// keysource_test.go's reproduction is handed server_nonce as one of THREE INJECTED VALUES and its
// header says so in as many words -- "the value the constructor was injected with". Before this
// slice that sentence was true because no setter existed. After it, it is true because of this
// gate.
//
// sessionfixture_test.go is the constructor every session in this package is built through, so a
// rebind in it is a rebind in every case in the package at once, including the five of
// keysource_test.go.
var noncerebindBannedCallers = []string{"keysource_test.go", "sessionfixture_test.go"}

// Property 5, held mechanically off the test source.
//
// THE SCOPE IS DERIVED SEPARATELY FROM THE CLASS: the scope is every _test.go file of this
// package, counted at run time rather than transcribed, because the plan's own count for it was
// taken at an older commit and is stale by three. The CLASS is the two files above. THE COMPLEMENT
// IS EVERY OTHER TEST FILE AND THIS GATE PRINTS IT BY NAME -- an empty complement would mean the
// ban had grown to cover the package, and that is the reading a gate must say rather than a reader
// infer.
//
// The gate is vacuous unless something calls the setter at all, so it fatals when no test file
// does, and errors when the file this slice added is not among the callers.
func TestTheTwoTestFilesWhoseCorrectnessNeedsTheInjectedNonceNeverRebindIt(t *testing.T) {
	fileSet := token.NewFileSet()
	scope := []string{}
	callers := []string{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read this package's directory: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		scope = append(scope, name)
		parsed, parseErr := parser.ParseFile(fileSet, filepath.ToSlash(filepath.Join(".", name)), nil,
			parser.SkipObjectResolution)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		if noncerebindCallsTheSetter(parsed) {
			callers = append(callers, name)
		}
	}
	slices.Sort(scope)
	slices.Sort(callers)
	if len(scope) == 0 {
		t.Fatal("no _test.go file was read out of this package, so every rule this gate holds over that source cleared its subject having read nothing")
	}
	for _, banned := range noncerebindBannedCallers {
		if !slices.Contains(scope, banned) {
			t.Fatalf("%s is not in this package's test source, so the ban this gate holds names a file that is not there", banned)
		}
	}
	if len(callers) == 0 {
		t.Fatal("no test file of this package calls RebindServerNonce, so a ban on two of them is a ban over an empty class and would report clean whatever those two did")
	}
	complement := []string{}
	for _, name := range scope {
		if !slices.Contains(noncerebindBannedCallers, name) {
			complement = append(complement, name)
		}
	}
	if len(complement) == 0 {
		t.Fatal("every test file of this package is banned from calling RebindServerNonce, so the complement of this ban is empty and the ban has grown to cover the package")
	}
	t.Logf("scope: %d test file(s); the ban is %d of them (%v); the COMPLEMENT is the other %d, which may call the setter freely: %v; the callers today are %v",
		len(scope), len(noncerebindBannedCallers), noncerebindBannedCallers, len(complement), complement, callers)
	for _, banned := range noncerebindBannedCallers {
		if slices.Contains(callers, banned) {
			t.Errorf("%s calls RebindServerNonce; its own correctness rests on server_nonce being the value the constructor was INJECTED with, and a rebind there makes the third of the reproduction's three independent inputs a value the subject chose",
				banned)
		}
	}
	if !slices.Contains(callers, "noncerebind_test.go") {
		t.Error("noncerebind_test.go does not call RebindServerNonce, so this gate's non-vacuity rests on some other file and the properties above are no longer observing the setter")
	}
}

// noncerebindCallsTheSetter is whether one parsed file contains a CALL of RebindServerNonce.
//
// It is a call site and not a mention, so the name appearing in this file's own failure messages
// and in its banned-caller list is not a call -- which is the difference between a gate that reads
// the source and one that greps it.
func noncerebindCallsTheSetter(parsed *ast.File) bool {
	found := false
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		if selector, isSelector := call.Fun.(*ast.SelectorExpr); isSelector {
			if selector.Sel.Name == "RebindServerNonce" {
				found = true
			}
			return true
		}
		if name, isName := call.Fun.(*ast.Ident); isName && name.Name == "RebindServerNonce" {
			found = true
		}
		return true
	})
	return found
}

// ---------------------------------------------------------------------------
// The instruments the record-level properties below are observed through
// ---------------------------------------------------------------------------

// noncerebindSeal is one durable record over fixed plaintexts, and the plaintexts, so a case that
// compares what came back out of OpenRecord is comparing it against what went in and not against
// its own earlier answer.
func noncerebindSeal(t *testing.T, fixture *testSession) (*message.Record, []byte, []byte) {
	t.Helper()
	return noncerebindSealAt(t, fixture.session)
}

// noncerebindSealAt is noncerebindSeal over a bare session, for the cases that seal at one member
// of a pair and open at the other. MASTER section 8.4 makes that the only shape a DURABLE round
// trip has -- a member cannot open the application frame it sealed itself, open item MG-4.
func noncerebindSealAt(t *testing.T, session *GroupSession) (*message.Record, []byte, []byte) {
	t.Helper()
	headPlain := []byte("a head that the record layer seals")
	bodyPlain := []byte("a body that the record layer seals, and it is longer than the head")
	record, err := session.SealRecord(message.RetentionDurable, 0, false, headPlain, bodyPlain, 0, nil)
	if err != nil {
		t.Fatalf("SealRecord: %v", err)
	}
	return record, headPlain, bodyPlain
}

// noncerebindDeepCopy is a snapshot of a record whose octets are its OWN.
//
// The copy is DERIVED off the type and not written as a list of the four slice fields: a snapshot
// that aliased one of them would be a snapshot that agreed with whatever the subject did to it,
// which is this project's rule about aliases read from the other direction. A sixth field that is
// a []byte is copied here with no edit.
func noncerebindDeepCopy(record *message.Record) *message.Record {
	copied := *record
	noncerebindCopyOctetsInto(reflect.ValueOf(&copied).Elem())
	return &copied
}

// noncerebindCopyOctetsInto replaces every []byte reachable through this struct's fields, and
// through any struct field of it, with a copy. A nil slice stays nil, because nil and empty are
// different values to reflect.DeepEqual and a copy that conflated them would hide a field that
// moved between them.
func noncerebindCopyOctetsInto(value reflect.Value) {
	for i := 0; i < value.NumField(); i += 1 {
		field := value.Field(i)
		if !field.CanSet() {
			continue
		}
		if field.Kind() == reflect.Struct {
			noncerebindCopyOctetsInto(field)
			continue
		}
		if field.Kind() != reflect.Slice || field.Type().Elem().Kind() != reflect.Uint8 || field.IsNil() {
			continue
		}
		fresh := reflect.MakeSlice(field.Type(), field.Len(), field.Len())
		reflect.Copy(fresh, field)
		field.Set(fresh)
	}
}

// noncerebindRecordFields is every field of message.Record, READ OFF THE TYPE rather than listed.
//
// The query beside it, R6 clause (a):
//
//	git show HEAD:message/record.go | sed -n '/^type Record struct/,/^}/p'
//
// piped through grep -c 'WriteAuth' returns 1. The class is what that block declares and this
// gate counts it at run time, so a sixth field added to message.Record next month is in the class
// with no edit here and the gate says which side of the line it landed on.
func noncerebindRecordFields() []string {
	typed := reflect.TypeOf(message.Record{})
	names := []string{}
	for i := 0; i < typed.NumField(); i += 1 {
		names = append(names, typed.Field(i).Name)
	}
	return names
}

// noncerebindMovedFields is the members of that class whose value differs between two records.
//
// Header is reported by the member of ITS OWN field set that moved -- Header.BodyHash and not
// Header -- because a gate that said "one of the five moved" would not tell a re-mac from a
// re-hash. A Header that differs in no named member is still reported, as Header itself, so an
// unnamed difference cannot be swallowed by the loop that names them.
func noncerebindMovedFields(before *message.Record, after *message.Record) []string {
	typed := reflect.TypeOf(message.Record{})
	beforeValue := reflect.ValueOf(*before)
	afterValue := reflect.ValueOf(*after)
	moved := []string{}
	for i := 0; i < typed.NumField(); i += 1 {
		name := typed.Field(i).Name
		if reflect.DeepEqual(beforeValue.Field(i).Interface(), afterValue.Field(i).Interface()) {
			continue
		}
		if beforeValue.Field(i).Kind() != reflect.Struct {
			moved = append(moved, name)
			continue
		}
		named := 0
		inner := beforeValue.Field(i).Type()
		for j := 0; j < inner.NumField(); j += 1 {
			if reflect.DeepEqual(beforeValue.Field(i).Field(j).Interface(), afterValue.Field(i).Field(j).Interface()) {
				continue
			}
			moved = append(moved, name+"."+inner.Field(j).Name)
			named += 1
		}
		if named == 0 {
			moved = append(moved, name)
		}
	}
	return moved
}

// noncerebindEpochKeys is this session's write_key[n] and read_key[n], taken out of Task 2's door.
//
// The two come out of EpochKeys and not off the session's own field, which is the whole of why
// the expected tag below is an independent computation: taking it from the value the sealer used
// would be comparing the method to itself.
func noncerebindEpochKeys(t *testing.T, fixture *testSession) ([]byte, []byte) {
	t.Helper()
	keys, err := fixture.session.EpochKeys()
	if err != nil {
		t.Fatalf("EpochKeys: %v", err)
	}
	defer keys.Destroy()
	writeKey, err := keys.WriteKey()
	if err != nil {
		t.Fatalf("WriteKey: %v", err)
	}
	readKey, err := keys.ReadKey()
	if err != nil {
		t.Fatalf("ReadKey: %v", err)
	}
	return append([]byte(nil), writeKey...), append([]byte(nil), readKey...)
}

// ---------------------------------------------------------------------------
// Task 3 Property 2 and Task 4 Property 2: one field moves, and the class is
// message.Record's own field set
// ---------------------------------------------------------------------------

// A rebind moves write_auth and moves NOTHING ELSE, and the observation is a re-authentication of
// THE SAME RECORD.
//
// THE ROUTE RUNS THROUGH ReauthRecord AND THAT IS STATED RATHER THAN HIDDEN: there is no route on
// which the nonce is the sole free variable that does not re-auth one record. Sealing a second
// record instead moves stream_index, which moves record_key[i], which moves both ciphertexts and
// the handle -- a difference no assertion could attribute to the nonce.
//
// AND THE CONTROL THAT MAKES THE OBSERVATION MEAN ANYTHING: a ReauthRecord with NO intervening
// rebind must leave every field byte-identical, write_auth included. Without it, "the tag changed"
// is consistent with a re-auth that is simply nondeterministic.
//
// THE CLASS IS DERIVED AND THE COMPLEMENT IS PRINTED. The class is every field of message.Record,
// read off the type; one member moves and the complement is the other four, named on every run.
// This is the blast-radius measurement of S2-2 held as a standing property rather than written
// down once in a plan.
func TestARebindMovesWriteAuthAndMovesNoOtherFieldOfTheRecord(t *testing.T) {
	fixture := newTestSession(t, "reauth-one-field")
	record, _, _ := noncerebindSeal(t, fixture)
	fields := noncerebindRecordFields()
	if len(fields) < 2 {
		t.Fatalf("message.Record declares %v, so the complement of a one member subset is empty and this gate would report the same clean run over a re-auth that moved everything",
			fields)
	}
	if !slices.Contains(fields, "WriteAuth") {
		t.Fatalf("message.Record declares %v and WriteAuth is not among them, so this gate is reading some other type", fields)
	}

	// the control: a re-auth with no rebind in front of it moves nothing at all.
	control := noncerebindDeepCopy(record)
	if err := fixture.session.ReauthRecord(record); err != nil {
		t.Fatalf("ReauthRecord with no intervening rebind: %v", err)
	}
	if moved := noncerebindMovedFields(control, record); len(moved) != 0 {
		t.Fatalf("a re-auth with NO rebind in front of it moved %v; without this control, a tag that changed after a rebind is consistent with a re-auth that is simply nondeterministic",
			moved)
	}

	before := noncerebindDeepCopy(record)
	if err := fixture.session.RebindServerNonce(noncerebindSecondNonce()); err != nil {
		t.Fatalf("RebindServerNonce: %v", err)
	}
	if moved := noncerebindMovedFields(before, record); len(moved) != 0 {
		t.Fatalf("the rebind alone moved %v of the record; a session that reached into a record the caller holds would be doing the re-auth's work without being asked",
			moved)
	}
	if err := fixture.session.ReauthRecord(record); err != nil {
		t.Fatalf("ReauthRecord after the rebind: %v", err)
	}
	moved := noncerebindMovedFields(before, record)
	complement := []string{}
	for _, name := range fields {
		if !slices.ContainsFunc(moved, func(one string) bool { return one == name || strings.HasPrefix(one, name+".") }) {
			complement = append(complement, name)
		}
	}
	t.Logf("message.Record declares %d field(s): %v; a rebind plus a re-auth moved %d of them (%v); the COMPLEMENT is the other %d: %v",
		len(fields), fields, len(moved), moved, len(complement), complement)
	if len(complement) == 0 {
		t.Fatal("every field of message.Record moved across the re-auth, so the complement of this narrowing is empty and 'one field moves' has become 'the record is rebuilt'")
	}
	if slices.Equal(moved, []string{"WriteAuth"}) {
		return
	}
	if len(moved) == 0 {
		t.Error("no field of THE CALLER'S RECORD moved across a rebind and a re-auth; the caller holds the record, so a method that answered a fresh one or that mutated a copy has left the outbox exactly as wrong as it was")
		return
	}
	t.Errorf("a rebind plus a re-auth moved %v of message.Record; write_auth is the ONE sealed value the nonce binds and every other field is the one the seal produced",
		moved)
}

// ---------------------------------------------------------------------------
// Task 4 Properties 1 and 3: the mac the server would verify, under the WRITE
// key
// ---------------------------------------------------------------------------

// The re-auth answers the mac the server would verify, and the expected tag is computed here from
// Task 2's door rather than from the method that produced it.
//
// COMPUTING THE EXPECTED TAG FROM THE SESSION'S OWN SealRecord WOULD BE COMPARING THE METHOD TO
// ITSELF, which is the tautology keysource_test.go's whole gate apparatus exists to prevent one
// level up. write_key comes out of EpochKeys().WriteKey(), the nonce is the value THIS TEST chose
// and handed to the setter, and the remaining three inputs are already on the record.
//
// Property 3 is the second half and is stated as its own clause rather than folded in, because it
// names the mutant Property 1's comparison catches: read_key and write_key are both thirty two
// octets off the same root and a mac under the wrong one is well formed. keysource_test.go
// explicitly cannot see it -- its exclusions put message.ReadKey outside the reproduction
// entirely.
func TestTheReauthAnswersTheMacTheServerWouldVerifyUnderTheWriteKey(t *testing.T) {
	fixture := newTestSession(t, "reauth-mac")
	record, _, _ := noncerebindSeal(t, fixture)
	writeKey, readKey := noncerebindEpochKeys(t, fixture)
	if bytes.Equal(writeKey, readKey) {
		t.Fatal("write_key and read_key are the same octets, so the clause below about which one the tag is taken under is a clause about nothing")
	}

	// the tag the seal already took, under the nonce the constructor was injected with. It is
	// the control for the comparison after the rebind: without it, an expected tag that matched
	// would be consistent with a re-auth that never ran.
	sealed := message.ComputeWriteAuth(writeKey, testServerNonce(), &record.Header, record.CtHead,
		record.Header.ServerAttachment)
	if sealed != record.WriteAuth {
		t.Fatalf("the record as SEALED carries %x and the mac under write_key and the injected nonce is %x; this case cannot say anything about a rebind until the two agree before one",
			record.WriteAuth, sealed)
	}

	rebound := noncerebindSecondNonce()
	handedOver := append([]byte(nil), rebound...)
	if err := fixture.session.RebindServerNonce(rebound); err != nil {
		t.Fatalf("RebindServerNonce: %v", err)
	}
	// THE CALLER'S BUFFER IS THE CALLER'S. Scribbling over it after the call must change
	// nothing the session does with it, and a session that retained the slice header rather
	// than copying it would mac under whatever this argument became. handedOver is what was
	// actually handed over, and the expected tag below is computed from that.
	for i := range rebound {
		rebound[i] ^= 0xFF
	}
	if err := fixture.session.ReauthRecord(record); err != nil {
		t.Fatalf("ReauthRecord: %v", err)
	}
	want := message.ComputeWriteAuth(writeKey, handedOver, &record.Header, record.CtHead,
		record.Header.ServerAttachment)
	if want == sealed {
		t.Fatal("the mac under the new nonce equals the mac under the old one, so the two nonces this case rebinds between are not telling the preimage apart and nothing below is an observation")
	}
	if record.WriteAuth != want {
		t.Errorf("after the rebind the record carries write_auth %x and the mac the server would verify -- write_key[n] off EpochKeys, over the nonce this test handed the setter -- is %x",
			record.WriteAuth, want)
	}
	// Property 3: and it is NOT the mac under read_key, which is well formed and which no
	// assertion about "the tag changed" would tell from the right one.
	underTheReadKey := message.ComputeWriteAuth(readKey, handedOver, &record.Header, record.CtHead,
		record.Header.ServerAttachment)
	if record.WriteAuth == underTheReadKey {
		t.Errorf("the re-auth took the tag under read_key: the record carries %x, which is the mac under read_key[n] rather than write_key[n]; MASTER section 9.2 has the server hold write_key and authenticate a submit on it, and req_auth is the only thing read_key macs",
			record.WriteAuth)
	}
}

// ---------------------------------------------------------------------------
// Task 4 Property 4: every refusal is taken before the mac, and on a refusal
// the caller's record is unchanged
// ---------------------------------------------------------------------------

// Four refusals, each with its own sentinel and each leaving the caller's record exactly as it
// was.
//
// THE "UNCHANGED ON REFUSAL" CLAUSE IS NOT TIDINESS. OpenRecord states the same rule one level
// over -- "a partial plaintext is never returned beside an error" -- and a half-applied re-auth
// hands an outbox a record it believes is fresh. And it is why the checks must PRECEDE the
// ComputeWriteAuth call rather than wrap it: that function panics on a short key and on an empty
// nonce rather than answering an error, so a refusal arriving as a recovered panic would be a
// refusal taken after the damage.
//
// Every arm runs AFTER a rebind, so a body that did not refuse would move the tag: an arm checked
// against a session whose nonce had not moved would pass over a re-auth that ran.
func TestEveryRefusalOfTheReauthComesBeforeTheMacAndLeavesTheRecordUnchanged(t *testing.T) {
	fixture := newTestSession(t, "reauth-refusals")
	record, _, _ := noncerebindSeal(t, fixture)
	if err := fixture.session.RebindServerNonce(noncerebindSecondNonce()); err != nil {
		t.Fatalf("RebindServerNonce: %v", err)
	}
	// the demonstration that a re-auth on this session WOULD move the tag, so each refusal
	// below is a refusal and not a re-auth that happened to answer what was already there.
	witness := noncerebindDeepCopy(record)
	if err := fixture.session.ReauthRecord(witness); err != nil {
		t.Fatalf("ReauthRecord on the witness: %v", err)
	}
	if witness.WriteAuth == record.WriteAuth {
		t.Fatal("a re-auth on this session did not move write_auth at all, so every unchanged assertion below would hold over a body that ran")
	}

	for _, refusal := range []struct {
		name     string
		spoil    func(record *message.Record)
		sentinel error
	}{
		{
			name:     "a record whose group id is not this session's",
			spoil:    func(record *message.Record) { record.Header.GroupId[0] ^= 0xFF },
			sentinel: ErrRecordNotForThisSession,
		},
		{
			name:     "a record whose epoch is not this session's",
			spoil:    func(record *message.Record) { record.Header.Epoch += 1 },
			sentinel: ErrRecordNotForThisSession,
		},
	} {
		spoiled := noncerebindDeepCopy(record)
		refusal.spoil(spoiled)
		before := noncerebindDeepCopy(spoiled)
		err := fixture.session.ReauthRecord(spoiled)
		if !errors.Is(err, refusal.sentinel) {
			t.Errorf("ReauthRecord of %s = %v, want %v", refusal.name, err, refusal.sentinel)
		}
		if moved := noncerebindMovedFields(before, spoiled); len(moved) != 0 {
			t.Errorf("ReauthRecord of %s refused and moved %v of the caller's record anyway; a mutant that returns the right error and mutates the record is the one this clause exists for, and a half applied re-auth hands an outbox a record it believes is fresh",
				refusal.name, moved)
		}
	}

	// the nil arm has no record to leave unchanged, and that is said rather than passed over.
	if err := fixture.session.ReauthRecord(nil); !errors.Is(err, message.ErrRecordNil) {
		t.Errorf("ReauthRecord(nil) = %v, want %v", err, message.ErrRecordNil)
	}

	// and the closed arm, on its own session because a close is not undoable. What the refusal
	// stands in front of is a panic and not a bad mac: zeroizeOnLoop has erased writeKey, and
	// message.ComputeWriteAuth panics on a key that is not thirty two octets.
	closing := newTestSession(t, "reauth-closed")
	closed, _, _ := noncerebindSeal(t, closing)
	if err := closing.session.RebindServerNonce(noncerebindSecondNonce()); err != nil {
		t.Fatalf("RebindServerNonce: %v", err)
	}
	if err := closing.session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	before := noncerebindDeepCopy(closed)
	if err := closing.session.ReauthRecord(closed); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("ReauthRecord on a closed session = %v, want %v", err, ErrSessionClosed)
	}
	if moved := noncerebindMovedFields(before, closed); len(moved) != 0 {
		t.Errorf("ReauthRecord on a closed session moved %v of the caller's record", moved)
	}
}

// ---------------------------------------------------------------------------
// Task 4 Property 5: a rebind and a re-auth change nothing about opening
// ---------------------------------------------------------------------------

// This is the "nothing already sealed becomes unopenable" finding held as a property instead of
// asserted in prose, and it is the clause that makes the blast-radius claim checkable by someone
// who does not believe the greps.
//
// THE SECOND TRACK IS THE INSTRUMENT AND NOT A CONVENIENCE. A receiver ratchet consumes the rung
// it opens -- consumeLocked drops it from the window and the head moves past it -- so one ladder
// opens one stream index exactly once, and "OpenRecord on the SAME record, before and after"
// cannot be asked of a ladder that has already answered it. ReceiverRatchets.Track replaces, so
// TrackSender at head 0 installs a fresh ladder over the same class key and the same leaf, which
// is the view a peer opening this record derives. Both opens are compared against the plaintexts
// that went IN, so a route that opened nothing is not green.
func TestARebindAndAReauthChangeNothingAboutOpening(t *testing.T) {
	// TWO MEMBERS AND TWO RECORDS, and both halves of that changed with MASTER section 8.4.
	//
	// TWO MEMBERS because a member cannot open the application frame it sealed itself -- open
	// item MG-4 -- so the rebind and the re-auth happen at the SENDER, whose write_auth they are
	// about, and both opens happen at the opener.
	//
	// TWO RECORDS because the instrument this case used to run on no longer exists. It opened ONE
	// record twice, re-installing the receiver ratchet in between, on the argument that
	// ReceiverRatchets.Track replaces and a fresh ladder is the view a peer derives. That is still
	// true of the RECORD layer and is now false of the whole open: an MLS generation is consumed
	// once and nothing re-installs it, so the second open of one record answers "ratchet
	// generation already consumed" whatever the ratchet table says. So the pair of observations is
	// two records off ONE sender ladder, sealed before the rebind, opened either side of it --
	// which asks the same question of the same ciphertexts and additionally asks it of a record
	// that was re-MAC'd before it was ever opened.
	pair := newTestPair(t, "reauth-open")
	pair.trackDurable(t)
	before, headPlain, bodyPlain := noncerebindSealAt(t, pair.sender)
	after, afterHeadPlain, afterBodyPlain := noncerebindSealAt(t, pair.sender)
	if !bytes.Equal(headPlain, afterHeadPlain) || !bytes.Equal(bodyPlain, afterBodyPlain) {
		t.Fatal("the two records were sealed over different plaintexts, so the two observations are not comparable")
	}

	beforeHead, beforeBody, err := pair.opener.OpenRecord(before)
	if err != nil {
		t.Fatalf("OpenRecord before the rebind: %v", err)
	}
	if !bytes.Equal(beforeHead, headPlain) || !bytes.Equal(beforeBody, bodyPlain) {
		t.Fatalf("the record did not open to what was sealed before the rebind: head %q body %q; nothing below is about a rebind",
			beforeHead, beforeBody)
	}

	if err := pair.sender.RebindServerNonce(noncerebindSecondNonce()); err != nil {
		t.Fatalf("RebindServerNonce: %v", err)
	}
	if err := pair.sender.ReauthRecord(after); err != nil {
		t.Fatalf("ReauthRecord: %v", err)
	}

	afterHead, afterBody, err := pair.opener.OpenRecord(after)
	if err != nil {
		t.Fatalf("OpenRecord after the rebind and the re-auth: %v; the nonce binds write_auth and NOTHING the open path reads, so a record that stopped opening is a re-auth that touched a ciphertext",
			err)
	}
	if !bytes.Equal(afterHead, beforeHead) {
		t.Errorf("the head opened to %q before the rebind and %q after it", beforeHead, afterHead)
	}
	if !bytes.Equal(afterBody, beforeBody) {
		t.Errorf("the body opened to %q before the rebind and %q after it", beforeBody, afterBody)
	}
}
