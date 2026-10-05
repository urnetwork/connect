// GroupSession: the command loop section 3.6 requires, the epoch zero root that must not move,
// and the four things a session cannot be built without.
package messagegroup

import (
	"bytes"
	"errors"
	"go/ast"
	"maps"
	"slices"
	"testing"

	"github.com/urnetwork/connect/message"
)

// ---------------------------------------------------------------------------
// Property 1: every mutation of the handle happens on the loop goroutine
// ---------------------------------------------------------------------------

// Section 3.6 is quoted in session.go and its second paragraph is the property:
//
//	a lock around each public method would not prevent an interleaving where two goroutines
//	both build a commit for epoch n. One goroutine per group, commands on a channel.
//
// So a stateLock around the public methods satisfies a race detector and NOT this, which is why
// the gate is over the SHAPE rather than over a race.
//
// The scope question (R3a), answered separately from the class question. The SCOPE is this
// package's production source, because GroupSession is declared here and its fields are
// unexported, so no other package can reach one at all. The CLASS is every method on
// *GroupSession, read off the syntax tree, minus the two that ARE the loop -- do, which posts,
// and run, which is the goroutine. Naming those two is not an exemption by name in the sense
// rule 5 warns about: they are the mechanism the property is stated over, and a gate that
// required the loop to post to itself would be requiring a deadlock.
//
// The loop owned FIELD set is derived and never listed: a field is loop owned if any body that
// runs on the loop -- a closure handed to do, or a method whose name ends OnLoop -- touches it.
// A field nothing on the loop touches is not a field this property is about.
func TestEveryMethodOfAGroupSessionReachesItsStateOnlyOnTheLoop(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	loopOwned := map[string]bool{}
	offLoop := map[string][]string{}
	methods := []string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil || sessionReceiverName(function) != "GroupSession" {
				continue
			}
			methods = append(methods, function.Name.Name)
			onLoop := sessionRunsOnTheLoop(function)
			for _, mention := range sessionFieldMentions(function.Body, sessionDoClosures(function.Body), onLoop) {
				if mention.onLoop {
					loopOwned[mention.field] = true
				}
			}
		}
	}
	if len(methods) == 0 {
		t.Fatal("no method on *GroupSession was read out of this package's production source, so this gate held nothing to section 3.6")
	}
	if len(loopOwned) == 0 {
		t.Fatal("no field of GroupSession is touched by anything running on the loop, so either the loop is gone or this gate is reading nothing")
	}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil || sessionReceiverName(function) != "GroupSession" {
				continue
			}
			if function.Name.Name == "do" || function.Name.Name == "run" {
				continue
			}
			onLoop := sessionRunsOnTheLoop(function)
			for _, mention := range sessionFieldMentions(function.Body, sessionDoClosures(function.Body), onLoop) {
				if mention.onLoop || !loopOwned[mention.field] {
					continue
				}
				offLoop[function.Name.Name] = append(offLoop[function.Name.Name], mention.field)
			}
		}
	}
	for name, fields := range offLoop {
		t.Errorf("%s reaches %v without posting a command; section 3.6 makes the loop the mechanism and a lock around this method would satisfy a race detector while leaving two goroutines free to build a commit for one epoch",
			name, slices.Compact(slices.Sorted(slices.Values(fields))))
	}
	t.Logf("%d method(s) on *GroupSession, %d loop owned field(s): %v",
		len(methods), len(loopOwned), slices.Sorted(maps.Keys(loopOwned)))
}

// The other half of the same property, and it is a separate assertion because the two fail for
// different reasons: a session that grew a mutex has answered section 3.6's question the way
// section 3.6 says is wrong, whether or not the methods still post.
func TestAGroupSessionHoldsNoLock(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	read := false
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			general, isGeneral := declaration.(*ast.GenDecl)
			if !isGeneral {
				continue
			}
			for _, spec := range general.Specs {
				typeSpec, isType := spec.(*ast.TypeSpec)
				if !isType || typeSpec.Name.Name != "GroupSession" {
					continue
				}
				structure, isStruct := typeSpec.Type.(*ast.StructType)
				if !isStruct {
					t.Fatal("GroupSession is not a struct")
				}
				read = true
				for _, field := range structure.Fields.List {
					selector, isSelector := field.Type.(*ast.SelectorExpr)
					if !isSelector {
						continue
					}
					if selector.Sel.Name == "Mutex" || selector.Sel.Name == "RWMutex" {
						t.Errorf("GroupSession declares a %s; section 3.6 says in as many words that a lock around each public method would not prevent two goroutines both building a commit for epoch n",
							selector.Sel.Name)
					}
				}
			}
		}
	}
	if !read {
		t.Fatal("no GroupSession struct was read, so this gate examined nothing")
	}
}

// sessionReceiverName is the type name a method's receiver names, or the empty string.
func sessionReceiverName(function *ast.FuncDecl) string {
	if function.Recv == nil || len(function.Recv.List) != 1 {
		return ""
	}
	switch receiver := function.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if named, isNamed := receiver.X.(*ast.Ident); isNamed {
			return named.Name
		}
	case *ast.Ident:
		return receiver.Name
	}
	return ""
}

// sessionRunsOnTheLoop answers whether a whole declaration's body runs on the loop goroutine,
// which is read off the naming convention session.go states rather than off a list: a method
// whose name ends OnLoop says in its own header that the caller is the loop.
func sessionRunsOnTheLoop(function *ast.FuncDecl) bool {
	return len(function.Name.Name) > 6 && function.Name.Name[len(function.Name.Name)-6:] == "OnLoop"
}

// sessionDoClosures is every function literal this body hands to self.do, which is the only place
// a body may reach the session's own state from.
func sessionDoClosures(body ast.Node) []*ast.FuncLit {
	closures := []*ast.FuncLit{}
	ast.Inspect(body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		callee, isSelector := call.Fun.(*ast.SelectorExpr)
		if !isSelector || callee.Sel.Name != "do" {
			return true
		}
		for _, argument := range call.Args {
			if literal, isLiteral := argument.(*ast.FuncLit); isLiteral {
				closures = append(closures, literal)
			}
		}
		return true
	})
	return closures
}

// sessionFieldMention is one read or write of self.<field> and whether it stands on the loop.
type sessionFieldMention struct {
	field  string
	onLoop bool
}

// sessionFieldMentions reads every self.<field> of one body and says, for each, whether it is
// inside one of the closures the body hands to do.
func sessionFieldMentions(body ast.Node, closures []*ast.FuncLit, wholeBodyOnLoop bool) []sessionFieldMention {
	inside := map[*ast.SelectorExpr]bool{}
	for _, closure := range closures {
		ast.Inspect(closure, func(node ast.Node) bool {
			if selector, isSelector := node.(*ast.SelectorExpr); isSelector {
				inside[selector] = true
			}
			return true
		})
	}
	mentions := []sessionFieldMention{}
	ast.Inspect(body, func(node ast.Node) bool {
		selector, isSelector := node.(*ast.SelectorExpr)
		if !isSelector {
			return true
		}
		receiver, isIdentifier := selector.X.(*ast.Ident)
		if !isIdentifier || receiver.Name != "self" {
			return true
		}
		mentions = append(mentions, sessionFieldMention{
			field:  selector.Sel.Name,
			onLoop: wholeBodyOnLoop || inside[selector],
		})
		return true
	})
	return mentions
}

// ---------------------------------------------------------------------------
// Property 3: a session cannot be constructed without what it needs
// ---------------------------------------------------------------------------

// Each of these is a value that cannot be supplied later, and a default for any one of them is
// the placeholder hazard the CP3a rule forbids: a nil reserver defaulting to an in-memory one
// loses every reservation at a restart and re-issues every stream index under an unmoved class
// key, and a pq_secret defaulting to zeros produces a working messenger with the post quantum
// half of the design silently gone.
func TestAGroupSessionRefusesEveryThingItCannotBeBuiltWithout(t *testing.T) {
	fixture := newTestEngine(t)
	handle := fixture.createGroup(t, "refusals")
	defer handle.Close()
	for _, refusal := range []struct {
		name string
		call func() (*GroupSession, error)
		want error
	}{
		{name: "no group handle", want: ErrNilGroupHandle, call: func() (*GroupSession, error) {
			return NewGroupSession(nil, testPqSecret(), nil, newStreamIndexMemory(), testClock(), testServerNonce())
		}},
		{name: "no reserver", want: ErrNilStreamIndexReserver, call: func() (*GroupSession, error) {
			return NewGroupSession(handle, testPqSecret(), nil, nil, testClock(), testServerNonce())
		}},
		{name: "no clock", want: ErrNilClock, call: func() (*GroupSession, error) {
			return NewGroupSession(handle, testPqSecret(), nil, newStreamIndexMemory(), nil, testServerNonce())
		}},
		{name: "no server nonce", want: ErrSessionServerNonce, call: func() (*GroupSession, error) {
			return NewGroupSession(handle, testPqSecret(), nil, newStreamIndexMemory(), testClock(), nil)
		}},
		{name: "no pq_secret", want: ErrNilPqSecret, call: func() (*GroupSession, error) {
			return NewGroupSession(handle, nil, nil, newStreamIndexMemory(), testClock(), testServerNonce())
		}},
	} {
		session, err := refusal.call()
		if !errors.Is(err, refusal.want) {
			t.Errorf("a session built with %s answered %v, want %v", refusal.name, err, refusal.want)
		}
		if session != nil {
			t.Errorf("a session built with %s answered a session beside its error", refusal.name)
			session.Close()
		}
	}
}

// ---------------------------------------------------------------------------
// Property 4: the epoch zero root is persisted, not recomputed
// ---------------------------------------------------------------------------

// group_handle_key is fixed at group creation and every sender_handle in the group hangs off it.
// A session that recomputed it from the current epoch's root would give every member a different
// handle at every commit -- the stream would end, and the server would route the next record
// nowhere.
//
// This is the property that fails when somebody simplifies GroupHandleKey's argument to the
// current root, which is exactly the shape of edit that reads as a tidy-up.
func TestTheGroupHandleKeyDoesNotMoveWhenTheEpochDoes(t *testing.T) {
	fixture := newTestSession(t, "epoch-zero-root")
	before, err := fixture.session.SenderHandle()
	if err != nil {
		t.Fatalf("SenderHandle: %v", err)
	}
	beforeKey := append([]byte(nil), fixture.session.groupHandleKey...)
	beforeRoot := append([]byte(nil), fixture.session.storageRoot...)

	if _, _, _, err := fixture.handle.Commit(nil); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := fixture.handle.MergePendingCommit(); err != nil {
		t.Fatalf("MergePendingCommit: %v", err)
	}
	if err := fixture.session.AdvanceEpoch(testPqSecret()); err != nil {
		t.Fatalf("AdvanceEpoch: %v", err)
	}
	epoch, err := fixture.session.Epoch()
	if err != nil {
		t.Fatalf("Epoch: %v", err)
	}
	if epoch != 1 {
		t.Fatalf("the session is at epoch %d after an advance, want 1", epoch)
	}
	after, err := fixture.session.SenderHandle()
	if err != nil {
		t.Fatalf("SenderHandle: %v", err)
	}
	if after != before {
		t.Errorf("the sender handle moved from %x to %x across an epoch; section 5.3 makes it epoch stable and the server routes on it",
			before, after)
	}
	if !bytes.Equal(fixture.session.groupHandleKey, beforeKey) {
		t.Errorf("group_handle_key moved across an epoch, so it was recomputed from the current root rather than persisted")
	}
	// and the epoch keyed material DID move, which is what says the advance happened at all.
	if bytes.Equal(fixture.session.storageRoot, beforeRoot) {
		t.Error("the storage root is unchanged across an epoch, so every key of the record layer is too and this case would pass against a session that ignored the advance")
	}
	// a session opened at a later epoch with NO epoch zero key is refused rather than inventing
	// one, which is the other direction of the same property.
	orphan, err := NewGroupSession(fixture.handle, testPqSecret(), nil, newStreamIndexMemory(),
		testClock(), testServerNonce())
	if !errors.Is(err, ErrEpochZeroHandleKeyMissing) {
		t.Errorf("a session opened at epoch 1 with no epoch zero group handle key answered %v, want ErrEpochZeroHandleKeyMissing", err)
	}
	if orphan != nil {
		orphan.Close()
	}
	// and one opened WITH it computes the same handle, which is what makes the value portable
	// rather than merely stable within one process.
	restored, err := NewGroupSession(fixture.handle, testPqSecret(), beforeKey, newStreamIndexMemory(),
		testClock(), testServerNonce())
	if err != nil {
		t.Fatalf("a session restored with its epoch zero group handle key: %v", err)
	}
	defer restored.Close()
	restoredHandle, err := restored.SenderHandle()
	if err != nil {
		t.Fatalf("SenderHandle: %v", err)
	}
	if restoredHandle != before {
		t.Errorf("a restored session computes sender handle %x and the original computes %x", restoredHandle, before)
	}
}

// ---------------------------------------------------------------------------
// Property 5: an aged out epoch is reported, not silently zero
// ---------------------------------------------------------------------------

// A storage root computed over an empty exporter output is thirty two well formed octets that no
// other member of the group ever reproduces, and every key of the epoch would hang off it.
func TestAnEpochWhoseSecretsAreGoneIsReportedAndNotComputedOver(t *testing.T) {
	fixture := newTestSession(t, "aged-out")
	if err := fixture.handle.Close(); err != nil {
		t.Fatalf("Close the handle: %v", err)
	}
	err := fixture.session.AdvanceEpoch(testPqSecret())
	if err == nil {
		t.Fatal("advancing over a handle whose secrets are gone answered no error, so the session computed a storage root over an empty exporter output")
	}
	// and a session opened over the same handle is refused for the same reason rather than
	// answering one whose every key is derived from nothing.
	orphan, buildErr := NewGroupSession(fixture.handle, testPqSecret(), nil, newStreamIndexMemory(),
		testClock(), testServerNonce())
	if buildErr == nil {
		t.Error("a session opened over a closed handle answered no error")
		orphan.Close()
	}
}

// ---------------------------------------------------------------------------
// Property 2: Close is idempotent, stops the loop, and erases every key
// ---------------------------------------------------------------------------

// The goroutine accounting is the LOOP'S OWN: Close returns only after run has exited, so a
// leaked loop is a hang rather than a slow test, and a command posted afterwards answers
// ErrSessionClosed rather than blocking forever. That is the deterministic form of "run it under
// goleak" and it needs no clock, which this package's own rule requires.
func TestCloseIsIdempotentStopsTheLoopAndErasesEveryKey(t *testing.T) {
	fixture := newTestSession(t, "closing")
	fixture.trackOwn(t)
	record, err := fixture.session.SealRecord(message.RetentionDurable, 0, false,
		[]byte("head"), []byte("body"), 0, nil)
	if err != nil {
		t.Fatalf("SealRecord: %v", err)
	}
	// the arrays themselves, so the assertion is about the octets and not about the fields
	// being set to nil.
	held := [][]byte{
		fixture.session.groupHandleKey,
		fixture.session.storageRoot,
		fixture.session.writeKey,
		fixture.session.readKey,
		// pq_secret is an ENTRY of a table keyed by epoch since ledger item 251's ruling 40, so
		// what is aliased here is the entry filed at the epoch this session stands at -- for a
		// fixture that has never advanced, the only one there is. The whole table's erase, over a
		// session holding one per epoch, is pqepoch_test.go's.
		fixture.session.pqSecrets[fixture.session.epoch],
		fixture.session.classKeys.Perm,
		fixture.session.classKeys.Durable,
		fixture.session.classKeys.Media,
	}
	for i, secret := range held {
		if len(secret) == 0 {
			t.Fatalf("the session holds no octets at position %d before it is closed, so this case would pass over a session that held nothing", i)
		}
	}
	if err := fixture.session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// idempotent: a deferred Close beside an explicit one is not a mistake.
	if err := fixture.session.Close(); err != nil {
		t.Errorf("a second Close answered %v, want nil", err)
	}
	for i, secret := range held {
		for _, octet := range secret {
			if octet != 0 {
				t.Errorf("the secret at position %d is not erased: %x", i, secret)
				break
			}
		}
	}
	// the loop is gone, which is what makes a posted command answer rather than block.
	if _, err := fixture.session.SealRecord(message.RetentionDurable, 0, false,
		[]byte("head"), []byte("body"), 0, nil); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("sealing after Close answered %v, want ErrSessionClosed", err)
	}
	if _, _, err := fixture.session.OpenRecord(record); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("opening after Close answered %v, want ErrSessionClosed", err)
	}
	if err := fixture.session.TrackSender(0, message.RetentionDurable, 0, 0, 0); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("tracking after Close answered %v, want ErrSessionClosed", err)
	}
	if err := fixture.session.AdvanceEpoch(testPqSecret()); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("advancing after Close answered %v, want ErrSessionClosed", err)
	}
	if _, err := fixture.session.SenderHandle(); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("asking for the sender handle after Close answered %v, want ErrSessionClosed", err)
	}
	// AND THE HANDLE IS CLOSED WITH IT. Without this the session erased its own octets and left
	// the group's epoch secrets -- its key schedule, its secret tree and the staged epoch behind
	// them -- live in the heap, which is the larger half of what a close is for. Measured:
	// replacing Close's call into the handle with nil survived every other case here.
	if authenticator := fixture.handle.EpochAuthenticator(); authenticator != nil {
		t.Errorf("the group handle is still open after the session closed: its epoch authenticator is %d octets, and mls answers nil for a closed group",
			len(authenticator))
	}
	if secret, err := fixture.handle.Export(mlsSecretLabel, nil, mlsSecretBytes); err == nil {
		t.Errorf("the group handle still exports %d octets after the session closed", len(secret))
	}
}

// The session is safe for concurrent use, which is what the loop is for. This is not a race
// detector case -- the shape gate above is what holds section 3.6's actual property -- it is the
// behavioural half: many goroutines sealing at once produce distinct stream indices and none of
// them is lost.
func TestConcurrentSealsTakeDistinctStreamIndices(t *testing.T) {
	fixture := newTestSession(t, "concurrent")
	const sealers = 8
	const each = 4
	answers := make(chan uint64, sealers*each)
	failures := make(chan error, sealers*each)
	done := make(chan struct{})
	for range sealers {
		go func() {
			defer func() { done <- struct{}{} }()
			for range each {
				record, err := fixture.session.SealRecord(message.RetentionDurable, 0, false,
					[]byte("head"), []byte("body"), 0, nil)
				if err != nil {
					failures <- err
					return
				}
				answers <- record.Header.StreamIndex
			}
		}()
	}
	for range sealers {
		<-done
	}
	close(answers)
	close(failures)
	for err := range failures {
		t.Fatalf("a concurrent seal answered %v", err)
	}
	seen := map[uint64]bool{}
	for index := range answers {
		if seen[index] {
			t.Errorf("stream index %d was handed out twice; a reused index is a reused nonce under a reused record key", index)
		}
		seen[index] = true
	}
	if len(seen) != sealers*each {
		t.Errorf("%d distinct stream indices came back out of %d seals", len(seen), sealers*each)
	}
}
