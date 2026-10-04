// The order every decoder in this package writes its receiver in.
//
// welcome_wire.go states the rule four times over -- "nothing is assigned until every field has
// been read", "publishing the receiver whole" -- and psk.go, group_context.go, proposal_wire.go
// and tree.go state it again in their own words. What it costs when it stops being true is not
// tidiness: a decoder that assigned as it read leaves a REFUSED value holding some fields out of
// the sender's bytes and the rest out of whatever the caller's value held before, which for a
// GroupInfo is a well formed object describing an epoch nobody published and carrying a
// signature taken over a different one, and for an EncryptedGroupSecrets is an entry a joiner
// can still MATCH its own key package reference against.
//
// TestARefusedDecodeLeavesTheCallersValueAlone in welcome_wire_test.go observes that
// behaviourally, and it observes it for FOUR types. This package declares thirty one decoders.
// A hand written table of four is the shape rule 5 exists for, and it understated the class the
// way every other one on this project did: inserting `self.Proposals = proposals` ahead of
// Commit.UnmarshalMLS's optional path read -- the receiver published before the decode has
// succeeded, so a refused Commit leaves the caller holding the sender's proposal list -- was a
// CONFIRMED SURVIVOR of the whole of ./mls/... and ./message/..., 6457 tests, while the
// identical edit in GroupInfo failed immediately. The difference between the two was a table row.
//
// So the class is DERIVED here, off the source, over every UnmarshalMLS the package declares,
// and the verdict for each is pinned. The reading is not positional: a decoder whose switch arms
// each read and then publish is staged, and one that publishes and then reads INSIDE an arm is
// not, so the analysis follows branches, loops and the one delegation this package makes rather
// than comparing the offsets of a write and a read. Five controls below hold the reader itself,
// because a matcher that had stopped matching would issue thirty one decoders a clean bill.
//
// Two of the verdicts are "publishes before its last read" and both are that way on purpose,
// with the source's own reason quoted at the row. A gate that could not express those would be
// one somebody deletes rather than one somebody keeps in step.
package mls

import (
	"fmt"
	"go/ast"
	"slices"
	"strings"
	"testing"
)

// The two verdicts, spelled once so that a row and the reader cannot disagree about the word.
const (
	decoderStagesItsReceiver     = "stages"
	decoderPublishesBeforeItsEnd = "publishes before its last read"
)

// decoderDeclaration is one method of this package together with the file it was read from, so
// that a delegate reached from another declaration is still rendered through its own file's
// positions.
type decoderDeclaration struct {
	path         string
	parsed       parsedSource
	receiverType string
	function     *ast.FuncDecl
}

// key names a method the way a delegation reaches it.
func (self decoderDeclaration) key() string {
	return self.receiverType + "." + self.function.Name.Name
}

// receiverName is the identifier the body calls its receiver, or the empty string for a
// declaration with none.
func (self decoderDeclaration) receiverName() string {
	if self.function.Recv == nil || len(self.function.Recv.List) != 1 {
		return ""
	}
	names := self.function.Recv.List[0].Names
	if len(names) != 1 {
		return ""
	}
	return names[0].Name
}

// readerName is the identifier the body calls its *syntax.Reader parameter.
//
// Read off the TYPE rather than assumed to be "r", because the one delegation in this package
// calls it "body" and a reader keyed on the spelling would find no reads in it at all -- which
// is not a missing feature but the silent shrink: a decoder whose reads are invisible reads as
// staged whatever it does.
func (self decoderDeclaration) readerName() string {
	if self.function.Type.Params == nil {
		return ""
	}
	for _, field := range self.function.Type.Params.List {
		if self.parsed.render(field.Type) != "*syntax.Reader" {
			continue
		}
		if len(field.Names) == 1 {
			return field.Names[0].Name
		}
	}
	return ""
}

// publishScan reads one body for whether a read from the reader can happen after the receiver
// has been written.
//
// The state carried through the walk is one bool -- has the receiver been written yet -- and the
// answer is one string: the first read reached while that was true. Branches are analysed from
// the state at the branch rather than from each other's outcome, which is the whole of what
// separates this from comparing two source offsets.
type publishScan struct {
	methods    map[string]decoderDeclaration
	visiting   map[string]bool
	firstWrite string
	lateRead   string
	unmodeled  []string
}

// statements walks a straight line block, carrying the write state forward.
func (self *publishScan) statements(at decoderDeclaration, list []ast.Stmt, wrote bool) bool {
	for _, statement := range list {
		wrote = self.statement(at, statement, wrote)
	}
	return wrote
}

// statement walks one statement and answers the write state after it.
//
// Anything this reader does not model lands in unmodeled rather than being read as neither a
// write nor a read, and the gate below fails on a non empty unmodeled list: a decoder written in
// a shape nobody anticipated has to be looked at, not passed over.
func (self *publishScan) statement(at decoderDeclaration, statement ast.Stmt, wrote bool) bool {
	if statement == nil {
		return wrote
	}
	switch node := statement.(type) {
	case *ast.AssignStmt:
		// the right hand side is evaluated before the left hand side is written, so a read
		// here happens before this statement's own write and after every earlier one
		wrote = self.expressions(at, node.Rhs, wrote)
		return self.writes(at, node.Lhs, wrote)
	case *ast.ExprStmt:
		return self.expressions(at, []ast.Expr{node.X}, wrote)
	case *ast.ReturnStmt:
		return self.expressions(at, node.Results, wrote)
	case *ast.IncDecStmt:
		wrote = self.expressions(at, []ast.Expr{node.X}, wrote)
		return self.writes(at, []ast.Expr{node.X}, wrote)
	case *ast.DeclStmt:
		return self.declaration(at, node, wrote)
	case *ast.BlockStmt:
		return self.statements(at, node.List, wrote)
	case *ast.LabeledStmt:
		return self.statement(at, node.Stmt, wrote)
	case *ast.IfStmt:
		wrote = self.statement(at, node.Init, wrote)
		wrote = self.expressions(at, []ast.Expr{node.Cond}, wrote)
		body := self.statements(at, node.Body.List, wrote)
		other := wrote
		if node.Else != nil {
			other = self.statement(at, node.Else, wrote)
		}
		return wrote || body || other
	case *ast.SwitchStmt:
		wrote = self.statement(at, node.Init, wrote)
		if node.Tag != nil {
			wrote = self.expressions(at, []ast.Expr{node.Tag}, wrote)
		}
		return self.clauses(at, node.Body, wrote)
	case *ast.TypeSwitchStmt:
		wrote = self.statement(at, node.Init, wrote)
		wrote = self.statement(at, node.Assign, wrote)
		return self.clauses(at, node.Body, wrote)
	case *ast.ForStmt:
		wrote = self.statement(at, node.Init, wrote)
		wrote = self.expressions(at, []ast.Expr{node.Cond}, wrote)
		return self.loop(at, node.Body.List, node.Post, wrote)
	case *ast.RangeStmt:
		wrote = self.expressions(at, []ast.Expr{node.X}, wrote)
		return self.loop(at, node.Body.List, nil, wrote)
	case *ast.EmptyStmt, *ast.BranchStmt:
		return wrote
	}
	self.unmodeled = append(self.unmodeled, fmt.Sprintf("%s: %T", at.key(), statement))
	return wrote
}

// loop walks a body twice: once from the state at the loop, and once from the state the first
// pass left.
//
// A decoder that appends to its receiver and then reads the next element is one no single pass
// can see, because the read that follows the write is the NEXT iteration's. The second pass is
// skipped when the first changed nothing, so a loop that never writes costs one walk.
func (self *publishScan) loop(at decoderDeclaration, body []ast.Stmt, post ast.Stmt, wrote bool) bool {
	after := self.statements(at, body, wrote)
	after = self.statement(at, post, after)
	if after != wrote {
		self.statement(at, post, self.statements(at, body, after))
	}
	return after
}

// clauses analyses every arm of a switch from the state at the switch, not from the arm before.
func (self *publishScan) clauses(at decoderDeclaration, body *ast.BlockStmt, wrote bool) bool {
	if body == nil {
		return wrote
	}
	after := wrote
	for _, statement := range body.List {
		clause, isClause := statement.(*ast.CaseClause)
		if !isClause {
			after = self.statement(at, statement, wrote) || after
			continue
		}
		after = self.statements(at, clause.Body, self.expressions(at, clause.List, wrote)) || after
	}
	return after
}

// declaration walks a var or const declaration for the reads its initialisers make.
func (self *publishScan) declaration(at decoderDeclaration, node *ast.DeclStmt, wrote bool) bool {
	generic, isGeneric := node.Decl.(*ast.GenDecl)
	if !isGeneric {
		self.unmodeled = append(self.unmodeled, fmt.Sprintf("%s: %T", at.key(), node.Decl))
		return wrote
	}
	for _, specification := range generic.Specs {
		value, isValue := specification.(*ast.ValueSpec)
		if !isValue {
			continue
		}
		wrote = self.expressions(at, value.Values, wrote)
	}
	return wrote
}

// expressions reads a list of expressions for three things, in the order they happen: the reads
// they make from the reader, the methods of the same receiver they hand off to, and the writes a
// closure inside them makes.
func (self *publishScan) expressions(at decoderDeclaration, list []ast.Expr, wrote bool) bool {
	for _, expression := range list {
		if expression == nil {
			continue
		}
		if read := self.readsReader(at, expression); read != "" && wrote && self.lateRead == "" {
			self.lateRead = at.key() + ": " + firstLineOf(read)
		}
		wrote = self.delegates(at, expression, wrote)
		wrote = self.closureWrites(at, expression, wrote)
	}
	return wrote
}

// readsReader answers this expression rendered, when the reader identifier appears anywhere in
// it, and the empty string otherwise.
//
// Any appearance, rather than a call whose receiver it is. syntax.ReadVector(r, ...),
// ReadExtensions(r), value.UnmarshalMLS(r) and r.ReadOptional(func(r *syntax.Reader) ...) are all
// reads and only the last is a method call ON the reader; a matcher that wanted that narrower
// shape would find no reads at all in half this package's decoders and report them staged.
func (self *publishScan) readsReader(at decoderDeclaration, expression ast.Expr) string {
	reader := at.readerName()
	if reader == "" {
		return ""
	}
	found := ""
	ast.Inspect(expression, func(node ast.Node) bool {
		identifier, isIdentifier := node.(*ast.Ident)
		if isIdentifier && identifier.Name == reader && found == "" {
			found = at.parsed.render(expression)
		}
		return true
	})
	return found
}

// delegates follows a reference to another method of the same receiver type.
//
// RatchetTree.UnmarshalMLS is `return r.ReadNested(self.readNodeArray)` and every write it makes
// is in that other method. A reader that stopped at the method VALUE would report RatchetTree
// staged for a reason that has nothing to do with what readNodeArray does, which is a clean bill
// issued to a body nobody read.
func (self *publishScan) delegates(at decoderDeclaration, expression ast.Expr, wrote bool) bool {
	receiver := at.receiverName()
	if receiver == "" {
		return wrote
	}
	ast.Inspect(expression, func(node ast.Node) bool {
		selector, isSelector := node.(*ast.SelectorExpr)
		if !isSelector {
			return true
		}
		base, isIdentifier := selector.X.(*ast.Ident)
		if !isIdentifier || base.Name != receiver {
			return true
		}
		target, declared := self.methods[at.receiverType+"."+selector.Sel.Name]
		if !declared || target.function.Body == nil || self.visiting[target.key()] {
			return true
		}
		self.visiting[target.key()] = true
		wrote = self.statements(target, target.function.Body.List, wrote)
		delete(self.visiting, target.key())
		return true
	})
	return wrote
}

// closureWrites answers whether a function literal inside this expression writes the receiver.
//
// The statement walker never reaches a closure body, because a closure is an expression; a write
// made in one is still a write that the caller's later reads happen after.
func (self *publishScan) closureWrites(at decoderDeclaration, expression ast.Expr, wrote bool) bool {
	ast.Inspect(expression, func(node ast.Node) bool {
		switch inner := node.(type) {
		case *ast.AssignStmt:
			wrote = self.writes(at, inner.Lhs, wrote)
		case *ast.IncDecStmt:
			wrote = self.writes(at, []ast.Expr{inner.X}, wrote)
		}
		return true
	})
	return wrote
}

// writes answers whether any of these assignment targets writes the receiver, recording the
// first one for the failure message.
func (self *publishScan) writes(at decoderDeclaration, targets []ast.Expr, wrote bool) bool {
	receiver := at.receiverName()
	if receiver == "" {
		return wrote
	}
	for _, target := range targets {
		if publishTargetRoot(target) != receiver {
			continue
		}
		if self.firstWrite == "" {
			self.firstWrite = at.key() + ": " + firstLineOf(at.parsed.render(target))
		}
		wrote = true
	}
	return wrote
}

// firstLineOf keeps a rendered node to one line, so a composite literal spanning eight lines
// does not spread a failure message over a screen.
func firstLineOf(rendered string) string {
	line, _, cut := strings.Cut(rendered, "\n")
	if cut {
		return line + " ..."
	}
	return line
}

// publishTargetRoot walks an assignment target down to the identifier it is rooted at, so that
// `*self`, `self.Field` and `self.nodes[x]` all answer "self".
func publishTargetRoot(target ast.Expr) string {
	for {
		switch node := target.(type) {
		case *ast.Ident:
			return node.Name
		case *ast.StarExpr:
			target = node.X
		case *ast.ParenExpr:
			target = node.X
		case *ast.SelectorExpr:
			target = node.X
		case *ast.IndexExpr:
			target = node.X
		default:
			return ""
		}
	}
}

// decoderVerdict runs the scan over one declaration and answers its verdict, the read that
// decided it, and anything the reader could not model.
func decoderVerdict(methods map[string]decoderDeclaration, at decoderDeclaration) (string, string, []string) {
	scan := &publishScan{methods: methods, visiting: map[string]bool{at.key(): true}}
	scan.statements(at, at.function.Body.List, false)
	if scan.lateRead != "" {
		return decoderPublishesBeforeItsEnd, scan.lateRead, scan.unmodeled
	}
	return decoderStagesItsReceiver, "", scan.unmodeled
}

// decoderMethodsOf collects every method one parsed file declares, keyed the way a delegation
// reaches it.
func decoderMethodsOf(path string, parsed parsedSource, into map[string]decoderDeclaration) {
	for _, declaration := range parsed.file.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Body == nil || function.Recv == nil {
			continue
		}
		receiverType := strings.TrimPrefix(parsed.receiverOf(function), "*")
		if receiverType == "" {
			continue
		}
		one := decoderDeclaration{
			path: path, parsed: parsed, receiverType: receiverType, function: function}
		into[one.key()] = one
	}
}

// decoderDeclarationsIn is the class over one roster of parsed files: every UnmarshalMLS they
// declare, together with every other method, so that a delegation can be followed.
func decoderDeclarationsIn(files map[string]parsedSource) ([]decoderDeclaration, map[string]decoderDeclaration) {
	methods := map[string]decoderDeclaration{}
	for path, parsed := range files {
		decoderMethodsOf(path, parsed, methods)
	}
	decoders := []decoderDeclaration{}
	for _, one := range methods {
		if one.function.Name.Name == "UnmarshalMLS" {
			decoders = append(decoders, one)
		}
	}
	slices.SortFunc(decoders, func(first decoderDeclaration, second decoderDeclaration) int {
		return strings.Compare(first.path+": "+first.key(), second.path+": "+second.key())
	})
	return decoders, methods
}

// decoderSourceOfThisPackage parses this package's non test source, once, for both derivations
// below.
func decoderSourceOfThisPackage(t *testing.T) map[string]parsedSource {
	t.Helper()
	files := map[string]parsedSource{}
	for _, path := range packageSourcePaths(t) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		files[path] = mustParseSource(t, path)
	}
	if len(files) == 0 {
		t.Fatal("this package has no non test source, so the class below is empty and every assertion over it holds vacuously")
	}
	return files
}

// codecPinnedTypesIn is every type these files pin as a syntax.Codec, read off the
// `var _ syntax.Codec = (*T)(nil)` line each one carries.
//
// This is the cross check on the scan's own reach. A decoder that moved to a file the glob does
// not read, or one whose receiver is spelled some way receiverOf does not render, would leave
// the class SMALLER while the pinned list below stayed a list -- so the two derivations are held
// against each other rather than each being trusted on its own.
func codecPinnedTypesIn(files map[string]parsedSource) []string {
	named := []string{}
	for _, parsed := range files {
		for _, declaration := range parsed.file.Decls {
			generic, isGeneric := declaration.(*ast.GenDecl)
			if !isGeneric {
				continue
			}
			for _, specification := range generic.Specs {
				value, isValue := specification.(*ast.ValueSpec)
				if !isValue || value.Type == nil || parsed.render(value.Type) != "syntax.Codec" {
					continue
				}
				for _, initialiser := range value.Values {
					call, isCall := initialiser.(*ast.CallExpr)
					if !isCall {
						continue
					}
					parenthesised, isParenthesised := call.Fun.(*ast.ParenExpr)
					if !isParenthesised {
						continue
					}
					pointer, isPointer := parenthesised.X.(*ast.StarExpr)
					if !isPointer {
						continue
					}
					if identifier, isIdentifier := pointer.X.(*ast.Ident); isIdentifier {
						named = append(named, identifier.Name)
					}
				}
			}
		}
	}
	slices.Sort(named)
	return named
}

// ---------------------------------------------------------------------------
// the controls on the reader itself
// ---------------------------------------------------------------------------

// A decoder written the way this package's convention says: every field read into a local, the
// receiver assigned once at the end. The reader must call this staged, or every verdict below
// is one it hands out for free.
const decoderStagedControl = `package mls

func (self *Thing) UnmarshalMLS(r *syntax.Reader) error {
	first, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	second, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	*self = Thing{First: first, Second: second}
	return nil
}
`

// The same decoder with the receiver published as it reads: a truncation between the two fields
// leaves the caller holding the sender's first field and its own second. This is the edit the
// gate exists to see, and the reader must call it what it is.
const decoderEagerControl = `package mls

func (self *Thing) UnmarshalMLS(r *syntax.Reader) error {
	first, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	self.First = first
	second, err := r.ReadOpaque()
	if err != nil {
		return err
	}
	self.Second = second
	return nil
}
`

// A discriminated decoder whose arms each read and then publish.
//
// This is the control that says the reading is about CONTROL FLOW rather than about offsets. The
// second arm's read sits after the first arm's write in the file, so a matcher comparing
// positions calls this eager -- and Node, FramedContentAuthData and PreSharedKeyId are all
// written this way, so such a matcher would report three staged decoders as eager and the pinned
// list would be wrong in the direction that looks like diligence.
const decoderBranchControl = `package mls

func (self *Thing) UnmarshalMLS(r *syntax.Reader) error {
	kind, err := r.ReadUint8()
	if err != nil {
		return err
	}
	switch kind {
	case 1:
		first, err := r.ReadOpaque()
		if err != nil {
			return err
		}
		*self = Thing{First: first}
		return nil
	case 2:
		second, err := r.ReadOpaque()
		if err != nil {
			return err
		}
		*self = Thing{Second: second}
		return nil
	}
	return errUnknownKind
}
`

// A decoder that appends to the receiver and then reads the next element.
//
// The read that follows the write is the NEXT iteration's, so a single walk of the body sees a
// read and then a write and calls it staged. The loop is the one shape where source order and
// execution order come apart in the other direction, and a tree body decoded straight into the
// receiver is exactly this shape.
const decoderLoopControl = `package mls

func (self *Thing) UnmarshalMLS(r *syntax.Reader) error {
	for !r.Empty() {
		item, err := r.ReadOpaque()
		if err != nil {
			return err
		}
		self.Items = append(self.Items, item)
	}
	return nil
}
`

// A decoder that hands its whole body to another method of the same receiver, which publishes as
// it reads.
//
// RatchetTree.UnmarshalMLS is `return r.ReadNested(self.readNodeArray)` and writes nothing
// itself. Without this control, "RatchetTree stages" below would be a statement about a body
// nobody read.
const decoderDelegationControl = `package mls

func (self *Thing) UnmarshalMLS(r *syntax.Reader) error {
	return r.ReadNested(self.readBody)
}

func (self *Thing) readBody(body *syntax.Reader) error {
	first, err := body.ReadOpaque()
	if err != nil {
		return err
	}
	self.First = first
	second, err := body.ReadOpaque()
	if err != nil {
		return err
	}
	self.Second = second
	return nil
}
`

// decoderVerdictOfControl runs the whole derivation over one control source and answers the
// verdict it gave the single decoder in it.
func decoderVerdictOfControl(t *testing.T, name string, source string) string {
	t.Helper()
	files := map[string]parsedSource{name: mustParseText(t, name, source)}
	decoders, methods := decoderDeclarationsIn(files)
	if len(decoders) != 1 {
		t.Fatalf("%s: the derivation found %d decoders in a control holding one", name, len(decoders))
	}
	verdict, _, unmodeled := decoderVerdict(methods, decoders[0])
	if len(unmodeled) != 0 {
		t.Fatalf("%s: the reader could not model %v", name, unmodeled)
	}
	return verdict
}

// ---------------------------------------------------------------------------
// the gate
// ---------------------------------------------------------------------------

// decoderPublishOrder is the verdict this package's source gives each of its decoders, pinned
// whole rather than filtered.
//
// Pinned whole for the reason TestEverySyntaxEncoderInThisPackageUsesTheDefaultLimit is: a
// decoder that starts publishing early has to be written down, and one that stops has to be
// taken off, so both directions come out of one comparison instead of one of them being left to
// a reviewer. A new codec joins this list in the commit that writes it.
var decoderPublishOrder = []string{
	// The v1 profile's Commit. Staged, and it is the row this file was added for: the same
	// edit that fails GroupInfo instantly survived the entire suite here, because the
	// behavioural sweep next door is a table and Commit was not in it.
	"commit_wire.go: (*Commit).UnmarshalMLS stages",
	"credential.go: (*Credential).UnmarshalMLS stages",
	// Capabilities and RequiredCapabilities publish as they read, and extension.go says why in
	// its own words: "these are five independent slices, the caller is handed the error, and
	// there is no composite of old and new fields here that describes a leaf which never
	// existed". What a half filled Capabilities describes is a member supporting fewer things,
	// and every predicate on it answers false for what is missing. The rows are here so that
	// the exemption is a decision somebody wrote down rather than an accident nothing reports.
	"extension.go: (*Capabilities).UnmarshalMLS publishes before its last read",
	"extension.go: (*Extension).UnmarshalMLS stages",
	"extension.go: (*RequiredCapabilities).UnmarshalMLS publishes before its last read",
	"framing.go: (*AuthenticatedContent).UnmarshalMLS stages",
	// FramedContent resets the receiver after the fixed fields and before the arm, which
	// framing.go states as a decision: "the three arms write three different fields, so a fully
	// staged decode would be a second FramedContent built only to be copied over the first".
	// What the reset buys is the property the staging buys elsewhere -- no arm of a previous
	// value survives -- at the price this row records: a truncation inside the arm leaves the
	// caller holding this message's group id, epoch, sender and content type.
	"framing.go: (*FramedContent).UnmarshalMLS publishes before its last read",
	"framing.go: (*FramedContentAuthData).UnmarshalMLS stages",
	// section 6's outermost structure. Staged whole rather than stamped with the header before
	// the arm is read, which is what the plan's own draft did: its decoder assigned
	// *self = MLSMessage{version, wireFormat} and then filled the arm into the receiver, so a
	// truncation inside any of the five arms left the caller holding a version and a wire format
	// out of a frame this package refused.
	"framing.go: (*MLSMessage).UnmarshalMLS stages",
	"framing.go: (*PrivateMessage).UnmarshalMLS stages",
	"framing.go: (*PublicMessage).UnmarshalMLS stages",
	"framing.go: (*Sender).UnmarshalMLS stages",
	"framing.go: (*SenderData).UnmarshalMLS stages",
	"framing_preimage.go: (*confirmedTranscriptHashInput).UnmarshalMLS stages",
	// p7 task 19's persisted epoch state, the one codec of this package whose wire is a disk. It
	// STAGES, and here that claim is about a caller's own group rather than about a peer's
	// message: a truncated state read back at start-up must leave the value the caller passed
	// alone rather than filling it with the fields that decoded before the truncation, because
	// a half filled blob names a restore kind with no secret and a tree with no context.
	"group.go: (*groupStateBlob).UnmarshalMLS stages",
	"group_context.go: (*GroupContext).UnmarshalMLS stages",
	// the urmessage_group_policy body. It STAGES: every field is read into a local, the whole
	// value is assembled and validated, and only then is it assigned to the receiver -- so a
	// decode that refuses a non canonical role list leaves the caller's policy as it was rather
	// than holding half of two.
	"group_policy.go: (*GroupPolicyExtension).UnmarshalMLS stages",
	"key_package.go: (*KeyPackage).UnmarshalMLS stages",
	"leaf_node.go: (*LeafNode).UnmarshalMLS stages",
	// the urmessage_owner_successor body. It STAGES, and for group_policy.go's reason one file
	// over: the nomination is read into locals, validated whole and only then assigned, so a
	// decode that refuses a floor shorter than ninety days leaves the caller holding the
	// nomination its group actually agreed to rather than a composite of that one and a peer's
	// refused bytes. TestARefusedOwnerSuccessorDecodeLeavesTheCallersValueAlone is the
	// behavioural half of this row.
	"owner_successor.go: (*OwnerSuccessorExtension).UnmarshalMLS stages",
	// Proposal is FramedContent's arrangement for FramedContent's reason, one layer down: the
	// reset ahead of the arm is what keeps a previous value's arm off a decode of bytes that
	// carry a different one, and TestAProposalDecodeLeavesNoFieldOfThePreviousValueBehind is
	// what holds that half. The cost this row records is the other half: a truncation inside
	// the arm leaves the caller's Proposal carrying this message's proposal type.
	"proposal_wire.go: (*Proposal).UnmarshalMLS publishes before its last read",
	"proposal_wire.go: (*ProposalOrRef).UnmarshalMLS stages",
	"psk.go: (*PreSharedKeyId).UnmarshalMLS stages",
	"tree.go: (*Node).UnmarshalMLS stages",
	"tree.go: (*OptionalNode).UnmarshalMLS stages",
	"tree.go: (*ParentNode).UnmarshalMLS stages",
	// RatchetTree writes nothing itself: its body is `r.ReadNested(self.readNodeArray)`, and
	// this verdict is readNodeArray's, reached through the delegation. That method's own
	// comment claims exactly this -- "staged into locals and assigned whole at the end ... the
	// difference between a rejected Welcome and a half replaced group" -- and until now nothing
	// checked the claim against the body.
	"tree.go: (*RatchetTree).UnmarshalMLS stages",
	"treekem.go: (*HpkeCiphertext).UnmarshalMLS stages",
	"treekem.go: (*UpdatePath).UnmarshalMLS stages",
	"treekem.go: (*UpdatePathNode).UnmarshalMLS stages",
	"welcome_wire.go: (*EncryptedGroupSecrets).UnmarshalMLS stages",
	"welcome_wire.go: (*GroupInfo).UnmarshalMLS stages",
	"welcome_wire.go: (*GroupSecrets).UnmarshalMLS stages",
	"welcome_wire.go: (*PathSecret).UnmarshalMLS stages",
	"welcome_wire.go: (*Welcome).UnmarshalMLS stages",
}

// decodersCarryingNoCodecPin is the one decoder of this package that no `var _ syntax.Codec`
// line names, with the reason.
//
// FramedContentAuthData's UnmarshalMLS takes a ContentType beside the reader -- the auth data's
// shape is selected by a field that is not inside it -- so it cannot satisfy syntax.Codec and
// framing.go says so where the pin would otherwise be. It is still a decoder with a receiver to
// publish, so it belongs to the class above; it is only outside the pin's.
var decodersCarryingNoCodecPin = []string{"FramedContentAuthData"}

// TestEveryDecoderInThisPackagePublishesItsReceiverWhereThisSaysItDoes derives the class the
// behavioural sweep in welcome_wire_test.go states four members of.
//
// The behavioural test is the one that says what the property is WORTH -- it truncates a real
// encoding at every offset and compares the receiver against what the caller held. This one says
// who is in the class, which is the half that was missing: Commit's staging survived being
// removed, and a suite of 6457 tests reported nothing, because no row named Commit.
//
// The verdict is derived per decoder and the whole list is compared, so a decoder that changes
// which side it is on fails here whichever direction it moved in.
func TestEveryDecoderInThisPackagePublishesItsReceiverWhereThisSaysItDoes(t *testing.T) {
	// the reader first, on bodies whose verdict is known, so that a matcher which has stopped
	// matching fails here rather than issuing every decoder below a clean bill
	for _, control := range []struct {
		name   string
		source string
		want   string
	}{
		{name: "a staged control", source: decoderStagedControl, want: decoderStagesItsReceiver},
		{name: "an eager control", source: decoderEagerControl, want: decoderPublishesBeforeItsEnd},
		{name: "a branching control", source: decoderBranchControl, want: decoderStagesItsReceiver},
		{name: "a looping control", source: decoderLoopControl, want: decoderPublishesBeforeItsEnd},
		{name: "a delegating control", source: decoderDelegationControl, want: decoderPublishesBeforeItsEnd},
	} {
		if verdict := decoderVerdictOfControl(t, control.name, control.source); verdict != control.want {
			t.Fatalf("the reader called %s %q, want %q; every verdict below is this reader's answer",
				control.name, verdict, control.want)
		}
	}

	files := decoderSourceOfThisPackage(t)
	decoders, methods := decoderDeclarationsIn(files)
	if len(decoders) == 0 {
		t.Fatal("this package declares no UnmarshalMLS at all, so this gate read nothing; it declares thirty one, and a gate that has stopped finding its subject must fail rather than report it clean")
	}
	verdicts, unmodeled, decoded := []string{}, []string{}, []string{}
	reads := map[string]string{}
	for _, one := range decoders {
		verdict, read, unread := decoderVerdict(methods, one)
		verdicts = append(verdicts, fmt.Sprintf("%s: (*%s).UnmarshalMLS %s", one.path, one.receiverType, verdict))
		unmodeled = append(unmodeled, unread...)
		decoded = append(decoded, one.receiverType)
		if read != "" {
			reads[one.receiverType] = read
		}
	}
	if len(unmodeled) != 0 {
		t.Errorf("the reader could not model %v; a statement it does not model is one it counts as neither a read nor a write, so the verdict for that decoder is not about its body",
			unmodeled)
	}
	if !slices.Equal(verdicts, decoderPublishOrder) {
		t.Errorf("this package's decoders publish as\n  %s\nand the pinned order is\n  %s\nthe read that decided each eager verdict:\n  %v",
			strings.Join(verdicts, "\n  "), strings.Join(decoderPublishOrder, "\n  "), reads)
	}

	// the cross check between the two derivations: every type the source pins as a codec has a
	// decoder above, and every decoder above that is not pinned is named with its reason
	slices.Sort(decoded)
	pinned := codecPinnedTypesIn(files)
	if len(pinned) == 0 {
		t.Fatal("no type of this package carries a var _ syntax.Codec pin, so the cross check demands nothing")
	}
	for _, one := range pinned {
		if !slices.Contains(decoded, one) {
			t.Errorf("%s is pinned as a syntax.Codec and no UnmarshalMLS for it was read, so the scan above did not reach its file",
				one)
		}
	}
	unpinned := []string{}
	for _, one := range decoded {
		if !slices.Contains(pinned, one) {
			unpinned = append(unpinned, one)
		}
	}
	if !slices.Equal(unpinned, decodersCarryingNoCodecPin) {
		t.Errorf("the decoders carrying no syntax.Codec pin are %v, and the ones named here are %v",
			unpinned, decodersCarryingNoCodecPin)
	}
	t.Logf("%d decoders read, %d types pinned as codecs", len(decoders), len(pinned))
}
