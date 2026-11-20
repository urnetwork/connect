package connect

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// The degradation state is package-level, because the flood it guards against
// is across every transport and sequence at once. These tests therefore reset
// it rather than constructing an instance, and must not run in parallel.
func resetBackendDegraded() {
	consecutiveBackendFails.Store(0)
	lastBackendFailNano.Store(0)
}

func TestBackendDegraded_CleanStateIsNotDegraded(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	if isBackendDegraded() {
		t.Fatal("a provider that has never seen a failure must not be degraded")
	}
}

func TestBackendDegraded_BelowThresholdIsNotDegraded(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	// One or two stray timeouts are normal churn on a busy provider.
	for i := 0; i < backendDegradedFailThreshold-1; i++ {
		noteBackendFailure()
		if isBackendDegraded() {
			t.Fatalf("degraded after %d failures; threshold is %d", i+1, backendDegradedFailThreshold)
		}
	}
}

func TestBackendDegraded_ThresholdReachedIsDegraded(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	for i := 0; i < backendDegradedFailThreshold; i++ {
		noteBackendFailure()
	}
	if !isBackendDegraded() {
		t.Fatalf("not degraded after %d consecutive recent failures", backendDegradedFailThreshold)
	}
}

// The counter alone is not enough: a stale count left by an old blip on an
// otherwise idle provider must not read as a live outage.
func TestBackendDegraded_StaleFailuresAreNotDegraded(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	for i := 0; i < backendDegradedFailThreshold; i++ {
		noteBackendFailure()
	}
	if !isBackendDegraded() {
		t.Fatal("precondition: should be degraded before aging the failure")
	}

	// Age the last failure past the recency window.
	lastBackendFailNano.Store(time.Now().Add(-backendDegradedWindow - time.Second).UnixNano())

	if isBackendDegraded() {
		t.Fatalf("still degraded with the last failure older than %s", backendDegradedWindow)
	}
}

// Recovery must be immediate on the first good round-trip, not on a timer.
func TestBackendDegraded_SuccessClearsImmediately(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	for i := 0; i < backendDegradedFailThreshold+5; i++ {
		noteBackendFailure()
	}
	if !isBackendDegraded() {
		t.Fatal("precondition: should be degraded")
	}

	noteBackendSuccess()

	if isBackendDegraded() {
		t.Fatal("a successful round-trip must clear the degraded state immediately")
	}
	if got := consecutiveBackendFails.Load(); got != 0 {
		t.Fatalf("consecutive failures = %d after success, want 0", got)
	}
}

// This is the false-positive guard that matters most: an intermittently failing
// path that still succeeds sometimes is NOT an outage, however many failures it
// accumulates in total.
func TestBackendDegraded_InterleavedSuccessNeverAccumulates(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	for i := 0; i < 50; i++ {
		noteBackendFailure()
		noteBackendFailure()
		noteBackendSuccess()
		if isBackendDegraded() {
			t.Fatalf("degraded on iteration %d despite an interleaved success", i)
		}
	}
}

// Regression: a streak that aged out must not be resumed by a later failure.
// Before the fix, three stale failures plus one fresh one totalled four with a
// current timestamp, so the backend read as degraded on the strength of a
// single recent failure.
func TestBackendDegraded_StaleStreakIsNotResumedByANewFailure(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	for i := 0; i < backendDegradedFailThreshold; i++ {
		noteBackendFailure()
	}
	// Age the whole streak out of the window, as an idle provider that simply
	// stopped retrying would.
	lastBackendFailNano.Store(time.Now().Add(-backendDegradedWindow - time.Minute).UnixNano())
	if isBackendDegraded() {
		t.Fatal("precondition: an aged-out streak must not read as degraded")
	}

	// One new failure. This is the first failure of a fresh streak, not the
	// fourth of the old one.
	noteBackendFailure()

	if got := consecutiveBackendFails.Load(); got != 1 {
		t.Fatalf("consecutive failures = %d after a stale streak plus one failure, want 1", got)
	}
	if isBackendDegraded() {
		t.Fatal("degraded after a single recent failure; the stale streak was resumed instead of reset")
	}
}

// The reset must not fire on a gap shorter than the window, or a slow but
// genuine outage would never accumulate.
func TestBackendDegraded_StreakSurvivesGapInsideWindow(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	noteBackendFailure()
	noteBackendFailure()
	// A gap well inside the window: still the same outage.
	lastBackendFailNano.Store(time.Now().Add(-backendDegradedWindow / 2).UnixNano())

	noteBackendFailure()

	if got := consecutiveBackendFails.Load(); got != backendDegradedFailThreshold {
		t.Fatalf("consecutive failures = %d, want %d (streak reset inside the window)", got, backendDegradedFailThreshold)
	}
	if !isBackendDegraded() {
		t.Fatal("not degraded after 3 failures spanning a gap inside the window")
	}
}

// A failure stamped after now is not recent. The clock stepped back since it
// was taken, so its age is unknown, and an unknown age must not latch
// "degraded", which only ever stops work. A synctest bubble's clock starts in
// 2000, so inside one every failure an earlier test stamped in real time is
// after now; read as recent, it held the windows of whichever bubbled tests
// ran after it.
func TestBackendDegraded_FailuresStampedAfterNowAreNotDegraded(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	for i := 0; i < backendDegradedFailThreshold; i++ {
		noteBackendFailure()
	}
	if !isBackendDegraded() {
		t.Fatal("precondition: should be degraded")
	}
	synctest.Test(t, func(t *testing.T) {
		if isBackendDegraded() {
			t.Error("failures stamped in real time read as recent inside a synctest bubble")
		}
	})

	lastBackendFailNano.Store(time.Now().Add(time.Minute).UnixNano())
	if isBackendDegraded() {
		t.Error("degraded on failures stamped a minute after now")
	}
}

// A streak stamped after now is discarded by the next failure, not extended:
// one failure after the clock stepped back is the first of a fresh streak,
// not the fourth of one whose age is unknown.
func TestBackendDegraded_StreakStampedAfterNowIsNotResumedByANewFailure(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	for i := 0; i < backendDegradedFailThreshold; i++ {
		noteBackendFailure()
	}
	lastBackendFailNano.Store(time.Now().Add(time.Minute).UnixNano())

	noteBackendFailure()

	if got := consecutiveBackendFails.Load(); got != 1 {
		t.Fatalf("consecutive failures = %d after a streak stamped after now plus one failure, want 1", got)
	}
	if isBackendDegraded() {
		t.Fatal("degraded after a single failure; the streak stamped after now was resumed instead of reset")
	}
}

// Failures and successes arrive concurrently in production: one client's auth
// can succeed while another's contract OOB fails. The pair must never leave a
// positive failure count with a zero timestamp, which isBackendDegraded would
// read as "not degraded" while failures are actually accumulating.
//
// This asserts the invariant; it does not reliably reproduce its violation. The
// window is two adjacent atomic stores, so an unsynchronized noteBackendSuccess
// still passes this test almost always. The serialization in noteBackendSuccess
// is what makes the invariant hold, and this guards against a future change that
// breaks it in a wider, actually-observable way.
func TestBackendDegraded_ConcurrentSuccessAndFailureNeverLeaveInvalidState(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			noteBackendFailure()
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			noteBackendSuccess()
		}()
	}
	wg.Wait()

	// A positive count always implies a real failure timestamp.
	if got := consecutiveBackendFails.Load(); 0 < got {
		if lastBackendFailNano.Load() == 0 {
			t.Fatalf("invalid state: %d consecutive failures recorded with a zero timestamp", got)
		}
	}
	// And a zeroed timestamp always implies no outstanding failures.
	if lastBackendFailNano.Load() == 0 {
		if got := consecutiveBackendFails.Load(); got != 0 {
			t.Fatalf("invalid state: zero timestamp with %d consecutive failures", got)
		}
	}
}

func TestBackendDegraded_ConcurrentFailuresReachThreshold(t *testing.T) {
	defer resetBackendDegraded()

	// rounds, because one round of a lost increment is a scheduling accident
	// that a single pass can miss: a failure that read the clock before
	// waiting for the lock found the stamp of the one that overtook it after
	// its now, and restarted the streak
	for round := 0; round < 50; round++ {
		resetBackendDegraded()

		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				noteBackendFailure()
			}()
		}
		wg.Wait()

		if got := consecutiveBackendFails.Load(); got != 32 {
			t.Fatalf("round %d: consecutive failures = %d after 32 concurrent failures, want 32", round, got)
		}
		if !isBackendDegraded() {
			t.Fatalf("round %d: not degraded after 32 concurrent failures", round)
		}
	}
}

// Auth failure accounting must remain inside the actual local-teardown and
// credential-refusal guards. Extra conjuncts must not break this source check.
func TestAuthCancellationIsNotBackendFailure(t *testing.T) {
	source, err := readSource("transport.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		method string
		guards []string
	}{
		{method: "runH1", guards: []string{"self.ctx.Err() == nil", "refusal == 0"}},
		{method: "runH3", guards: []string{"ctx.Err() == nil", "refusal == 0", "!attemptCanceled"}},
	} {
		if err := authFailureSourceGuards(source, test.method, test.guards); err != nil {
			t.Errorf("%s: %v", test.method, err)
		}
	}

	// the runners record through noteDialFailure, which keeps the process-wide
	// degraded signal for a family-agnostic transport and routes a
	// family-pinned transport to its own backoff (IPV6.md A5): a pinned
	// transport failing forever on a network without its family must never
	// gate the process. The indirection must still reach noteBackendFailure,
	// and the pinned early return must come first.
	familySource, err := readSource("transport_family.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := functionBody(familySource, "func (self *PlatformTransport) noteDialFailure(")
	if !ok {
		t.Fatal("could not find noteDialFailure")
	}
	note := strings.Index(body, "noteBackendFailure()")
	if note < 0 {
		t.Fatal("noteDialFailure no longer records backend failures; the degraded signal lost its transport half")
	}
	pinned := strings.Index(body, "if self.pinned() {")
	if pinned < 0 || note < pinned {
		t.Fatal("noteDialFailure counts a family-pinned transport's dial failure as a backend failure")
	}
}

// Inspect only the platform runner's real call ancestors. A required expression
// must be a mandatory conjunction on the true branch containing every call;
// unrelated branches and deferred/asynchronous execution are not authority.
func authFailureSourceGuards(source string, method string, required []string) error {
	file, err := parser.ParseFile(token.NewFileSet(), "transport.go", source, 0)
	if err != nil {
		return fmt.Errorf("cannot parse transport source: %w", err)
	}
	var body *ast.BlockStmt
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != method || function.Recv == nil || len(function.Recv.List) != 1 {
			continue
		}
		receiver := function.Recv.List[0]
		pointer, ok := receiver.Type.(*ast.StarExpr)
		if !ok || len(receiver.Names) != 1 || receiver.Names[0].Name != "self" {
			continue
		}
		name, ok := pointer.X.(*ast.Ident)
		if ok && name.Name == "PlatformTransport" {
			body = function.Body
			break
		}
	}
	if body == nil || len(required) == 0 {
		return fmt.Errorf("missing platform runner or required guards")
	}
	guards := make([]ast.Expr, len(required))
	for index, condition := range required {
		guards[index], err = parser.ParseExpr(condition)
		if err != nil {
			return fmt.Errorf("cannot parse required auth guard: %w", err)
		}
	}
	unparen := func(expression ast.Expr) ast.Expr {
		for {
			parenthesized, ok := expression.(*ast.ParenExpr)
			if !ok {
				return expression
			}
			expression = parenthesized.X
		}
	}
	// Only the expression shapes used by these three auth guards are accepted.
	var sameExpression func(ast.Expr, ast.Expr) bool
	sameExpression = func(left ast.Expr, right ast.Expr) bool {
		left, right = unparen(left), unparen(right)
		switch left := left.(type) {
		case *ast.Ident:
			right, ok := right.(*ast.Ident)
			return ok && left.Name == right.Name
		case *ast.BasicLit:
			right, ok := right.(*ast.BasicLit)
			return ok && left.Kind == right.Kind && left.Value == right.Value
		case *ast.SelectorExpr:
			right, ok := right.(*ast.SelectorExpr)
			return ok && left.Sel.Name == right.Sel.Name && sameExpression(left.X, right.X)
		case *ast.CallExpr:
			right, ok := right.(*ast.CallExpr)
			return ok && len(left.Args) == 0 && len(right.Args) == 0 && sameExpression(left.Fun, right.Fun)
		case *ast.UnaryExpr:
			right, ok := right.(*ast.UnaryExpr)
			return ok && left.Op == right.Op && sameExpression(left.X, right.X)
		case *ast.BinaryExpr:
			right, ok := right.(*ast.BinaryExpr)
			return ok && left.Op == right.Op && sameExpression(left.X, right.X) && sameExpression(left.Y, right.Y)
		default:
			return false
		}
	}
	var mandatoryConjunct func(ast.Expr, ast.Expr) bool
	mandatoryConjunct = func(condition ast.Expr, guard ast.Expr) bool {
		if sameExpression(condition, guard) {
			return true
		}
		conjunction, ok := unparen(condition).(*ast.BinaryExpr)
		return ok && conjunction.Op == token.LAND &&
			(mandatoryConjunct(conjunction.X, guard) || mandatoryConjunct(conjunction.Y, guard))
	}
	callCount, unsafeCount := 0, 0
	var ancestors []ast.Node
	ast.Inspect(body, func(node ast.Node) bool {
		if node == nil {
			ancestors = ancestors[:len(ancestors)-1]
			return true
		}
		if call, ok := node.(*ast.CallExpr); ok {
			selector, selected := unparen(call.Fun).(*ast.SelectorExpr)
			if selected && selector.Sel.Name == "noteDialFailure" {
				self, own := unparen(selector.X).(*ast.Ident)
				if own && self.Name == "self" {
					callCount++
					guarded := make([]bool, len(guards))
				ancestorsLoop:
					for index := len(ancestors) - 1; 0 <= index; index-- {
						switch ancestor := ancestors[index].(type) {
						case *ast.FuncLit, *ast.GoStmt, *ast.DeferStmt:
							break ancestorsLoop
						case *ast.IfStmt:
							if ancestor.Body.Pos() < call.Pos() && call.End() < ancestor.Body.End() {
								for guardIndex, guard := range guards {
									guarded[guardIndex] = guarded[guardIndex] || mandatoryConjunct(ancestor.Cond, guard)
								}
							}
						}
					}
					for _, present := range guarded {
						if !present {
							unsafeCount++
							break
						}
					}
				}
			}
		}
		ancestors = append(ancestors, node)
		return true
	})
	if callCount == 0 || unsafeCount != 0 {
		return fmt.Errorf("auth failure calls=%d without required containing guards=%d", callCount, unsafeCount)
	}
	return nil
}

// The old exact-brace anchor rejected the current stronger conjunction. Both
// that form and a nested/reordered guard must retain their mandatory meaning.
func TestAuthFailureSourceGuardAcceptsMandatoryConjuncts(t *testing.T) {
	for _, test := range []struct {
		name, method, body string
		guards             []string
	}{
		{name: "old bare guard", method: "runH1", body: "if self.ctx.Err() == nil { self.noteDialFailure() }", guards: []string{"self.ctx.Err() == nil"}},
		{name: "current h1 guard", method: "runH1", body: "if self.ctx.Err() == nil && refusal == 0 { self.noteDialFailure() }", guards: []string{"self.ctx.Err() == nil", "refusal == 0"}},
		{name: "reordered and parenthesized", method: "runH1", body: "if ((refusal == 0) && ((self.ctx.Err()) == (nil))) { self.noteDialFailure() }", guards: []string{"self.ctx.Err() == nil", "refusal == 0"}},
		{name: "nested h3 attempt", method: "runH3", body: "if ctx.Err() == nil && refusal == 0 { if !attemptCanceled { self.noteDialFailure() } }", guards: []string{"ctx.Err() == nil", "refusal == 0", "!attemptCanceled"}},
		{name: "both guarded calls", method: "runH1", body: "if self.ctx.Err() == nil && refusal == 0 { self.noteDialFailure(); self.noteDialFailure() }", guards: []string{"self.ctx.Err() == nil", "refusal == 0"}},
	} {
		source := "package connect\nfunc (self *PlatformTransport) " + test.method + "() { " + test.body + " }\n"
		if err := authFailureSourceGuards(source, test.method, test.guards); err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
	}
}

// Syntax near a call is not evidence that local teardown is excluded. These
// controls fail deterministically without dialing, waiting, or changing runtime.
func TestAuthFailureSourceGuardRejectsUnsafeMutants(t *testing.T) {
	for _, test := range []struct{ name, body string }{
		{name: "no guard", body: "self.noteDialFailure()"},
		{name: "or allows cancellation", body: "if self.ctx.Err() == nil || refusal == 0 { self.noteDialFailure() }"},
		{name: "unrelated earlier guard", body: "if self.ctx.Err() == nil && refusal == 0 {}; self.noteDialFailure()"},
		{name: "else is not live", body: "if self.ctx.Err() == nil && refusal == 0 {} else { self.noteDialFailure() }"},
		{name: "missing context", body: "if refusal == 0 { self.noteDialFailure() }"},
		{name: "missing refusal", body: "if self.ctx.Err() == nil { self.noteDialFailure() }"},
		{name: "second unguarded call", body: "if self.ctx.Err() == nil && refusal == 0 { self.noteDialFailure() }; self.noteDialFailure()"},
		{name: "parenthesized unguarded call", body: "if self.ctx.Err() == nil && refusal == 0 { self.noteDialFailure() }; (self.noteDialFailure)()"},
		{name: "late callback", body: "if self.ctx.Err() == nil && refusal == 0 { callback = func() { self.noteDialFailure() } }"},
		{name: "spawned call", body: "if self.ctx.Err() == nil && refusal == 0 { go self.noteDialFailure() }"},
		{name: "deferred call", body: "if self.ctx.Err() == nil && refusal == 0 { defer self.noteDialFailure() }"},
		{name: "guard only in comment", body: "/* if self.ctx.Err() == nil && refusal == 0 { */ self.noteDialFailure()"},
		{name: "no accounting call", body: "if self.ctx.Err() == nil && refusal == 0 {}"},
	} {
		source := "package connect\nfunc (self *PlatformTransport) runH1() { " + test.body + " }\n"
		if err := authFailureSourceGuards(source, "runH1", []string{"self.ctx.Err() == nil", "refusal == 0"}); err == nil {
			t.Fatalf("%s admitted unsafe auth accounting", test.name)
		}
	}
	for _, body := range []string{
		"if ctx.Err() == nil && refusal == 0 { self.noteDialFailure() }",
		"if ctx.Err() == nil && refusal == 0 { if !attemptCanceled || other { self.noteDialFailure() } }",
	} {
		source := "package connect\nfunc (self *PlatformTransport) runH3() { " + body + " }\n"
		if err := authFailureSourceGuards(source, "runH3", []string{"ctx.Err() == nil", "refusal == 0", "!attemptCanceled"}); err == nil {
			t.Fatal("h3 accounting admitted a canceled attempt")
		}
	}
	for _, source := range []string{
		"package connect\nfunc (self *OtherTransport) runH1() { if self.ctx.Err() == nil && refusal == 0 { self.noteDialFailure() } }",
		"package connect\nfunc (self *PlatformTransport) other() { if self.ctx.Err() == nil && refusal == 0 { self.noteDialFailure() } }",
		"package connect\nfunc (",
	} {
		if err := authFailureSourceGuards(source, "runH1", []string{"self.ctx.Err() == nil", "refusal == 0"}); err == nil {
			t.Fatal("missing or malformed runner passed the guard check")
		}
	}
}
