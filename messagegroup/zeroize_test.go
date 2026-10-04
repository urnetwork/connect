package messagegroup

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Property 1: after the call every octet of the BACKING ARRAY is zero.
//
// Read through a second slice header over the same array rather than through the one that was
// passed, which is the whole point of the reading: a helper that reassigned its parameter to a
// fresh allocation satisfies every check made through its own header and leaves the secret
// exactly where it was. The witness is taken before the call and is never handed to it.
func TestZeroizeWritesThroughToTheBackingArray(t *testing.T) {
	for _, length := range []int{1, 2, 7, 32, 56, 1216} {
		backing := make([]byte, length)
		for i := range backing {
			backing[i] = byte(i%255) + 1
		}
		// a second header over the same array, taken now and never passed to zeroize
		witness := backing[:len(backing):len(backing)]
		before := 0
		for _, octet := range witness {
			if octet != 0 {
				before++
			}
		}
		if before != length {
			t.Fatalf("the %d octet fixture starts with %d non zero octets, so this reading would clear a helper that did nothing", length, before)
		}
		zeroize(backing)
		for i, octet := range witness {
			if octet != 0 {
				t.Fatalf("after zeroize of %d octets the backing array holds %#02x at index %d; the write did not reach the array the caller's other headers see",
					length, octet, i)
			}
		}
		// and the array the caller kept is the same array, not a replacement
		if len(backing) != len(witness) || (0 < len(backing) && &backing[0] != &witness[0]) {
			t.Fatalf("zeroize left the caller holding a different array than the one the witness reads")
		}
	}
}

// Property 1, the other half: a slice that is a WINDOW into a larger array erases its own
// octets and no others, so an erase cannot silently blank a neighbouring secret.
func TestZeroizeErasesTheSliceAndNotTheArrayAroundIt(t *testing.T) {
	backing := make([]byte, 96)
	for i := range backing {
		backing[i] = 0xEE
	}
	zeroize(backing[32:64])
	for i, octet := range backing {
		want := byte(0xEE)
		if 32 <= i && i < 64 {
			want = 0
		}
		if octet != want {
			t.Fatalf("index %d is %#02x, want %#02x; the erase reached outside the slice it was handed", i, octet, want)
		}
	}
}

// Property 2: nil and empty are no-ops and neither panics.
func TestZeroizeAcceptsNilAndEmpty(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("zeroize panicked on an empty secret with %v; a double erase on the receive path would then be a crash", recovered)
		}
	}()
	zeroize(nil)
	zeroize([]byte{})
	backing := make([]byte, 8)
	zeroize(backing[:0])
	for i, octet := range backing {
		if octet != 0 {
			t.Errorf("a zero length slice erased index %d of the array behind it", i)
		}
	}
	// erasing twice is the shape every drop site produces, and the second call must be as quiet
	// as the first
	secret := []byte{1, 2, 3}
	zeroize(secret)
	zeroize(secret)
	for i, octet := range secret {
		if octet != 0 {
			t.Errorf("index %d is %#02x after a double erase", i, octet)
		}
	}
}

// The directive this package's erasure rests on, spelled once.
const zeroizeNoinlineDirective = "//go:noinline"

// ---------------------------------------------------------------------------
// property 3: every erase helper of this package carries the pragma
// ---------------------------------------------------------------------------

// The CLASS is derived from the PROPERTY -- "a write that reaches storage outliving the call" --
// and not from the one loop this package happens to spell. That distinction is the whole of this
// gate's history and it was measured rather than argued.
//
// The version this replaces matched an assignment through an index expression whose right hand
// side was a BasicLit with the exact text "0". Three mutants walked past it, each applied to this
// package's own source and each run to completion: a second production zeroizer written
// secret[i] = 0x00, one written clear(secret), and one written copy(secret, make(...)) -- all
// three called from (*ClassKeys).Zeroize, none carrying the directive, all three green. And the
// same reading put (*ClassKeys).Zeroize itself in no class at all, because a method that hands
// three arrays to an eraser spells no write of its own; the batch that shipped the narrow gate
// argued for a second zeroizer on the grounds that "the two copies cannot drift apart without the
// noinline gate below noticing", and, measured, the gate did not notice.
//
// So the class is derived the way connect/mls derives its own, which settled this boundary first:
//
//   - the storage that outlives the call is the receiver and every []byte parameter, plus this
//     package's own names for a []byte, read off the type declarations rather than listed;
//   - the names REACHING that storage are closed to a fixed point over reslices, locals cut from
//     it, the value of a range over it and the first name of a comma ok read of it -- because
//     window := secret[n:] is the same array, and secret, ok := self.window[i] is how this
//     package's own window eraser is written;
//   - a body WRITES THROUGH one of those names if it assigns an integer literal whose VALUE is
//     zero in any base, or increments through an index of it, or hands it to clear, or hands it
//     to copy as the destination;
//   - and the class is closed under the HAND-OFF: a body that passes one of those names to a
//     member has erased it just as surely as one that spells the loop, which in this package is
//     how erasure is nearly always written.
//
// Two limits, stated rather than hidden. The value filter on the spelled half is what keeps a map
// store -- self.window[index] = secret, which RETAINS a rung rather than erasing one -- outside
// the class, and its price is that a zeroizer writing a zero held in a variable is invisible to
// the spelled half; the hand-off closure covers the shapes this package actually writes, and the
// control below holds both halves. And the hand-off is followed by ARGUMENT and not by receiver:
// a method that erases through a call on its own object is outside the class, because counting
// receivers would close it over every exported method of a type that erases anything anywhere.
// Those declarations carry the directive by the convention connect/mls keeps, and their comments
// say which side of the line they are on.
//
// The SCOPE (R3a) is this package's own directory. connect/mls holds the same rule over its own
// source in its own suite, and connect/message can neither call this helper nor be called by it.

// One declaration as the matchers read it. spelled and handsOn are kept apart because only the
// first can be decided by inspecting the declaration alone: whether handing the storage on is an
// erasure depends on what the callee does, which the closure below answers.
type zeroizeCandidate struct {
	file      string
	name      string
	spelled   bool
	handsOn   []string
	directive bool
}

// The names through which a write reaches bytes that are still there when the call returns: the
// receiver, whatever its type, and every parameter that is a []byte or one of this package's own
// names for one.
func zeroizeStorageOutlivingTheCall(function *ast.FuncDecl, rendered func(ast.Expr) string, named []string) []string {
	handed := []string{}
	if function.Recv != nil {
		for _, field := range function.Recv.List {
			for _, name := range field.Names {
				if name.Name != "_" {
					handed = append(handed, name.Name)
				}
			}
		}
	}
	if function.Type.Params == nil {
		return handed
	}
	for _, field := range function.Type.Params.List {
		text := rendered(field.Type)
		if text != "[]byte" && !slices.Contains(named, text) {
			continue
		}
		for _, name := range field.Names {
			if name.Name != "_" {
				handed = append(handed, name.Name)
			}
		}
	}
	return handed
}

// The name a write's target hangs off, so secret[i], secret[1:][i], (secret)[i] and
// self.window[index] all report the name whose storage is reached.
func zeroizeRootIdentifierOf(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.ParenExpr:
		return zeroizeRootIdentifierOf(typed.X)
	case *ast.IndexExpr:
		return zeroizeRootIdentifierOf(typed.X)
	case *ast.SliceExpr:
		return zeroizeRootIdentifierOf(typed.X)
	case *ast.SelectorExpr:
		return zeroizeRootIdentifierOf(typed.X)
	case *ast.StarExpr:
		return zeroizeRootIdentifierOf(typed.X)
	}
	return ""
}

// The set of names bound to that same storage, to a fixed point.
func zeroizeNamesReachingTheSameStorage(function *ast.FuncDecl, handed []string) []string {
	reaching := map[string]bool{}
	for _, name := range handed {
		reaching[name] = true
	}
	bind := func(target ast.Expr, source ast.Expr) bool {
		root := zeroizeRootIdentifierOf(source)
		if root == "" || !reaching[root] {
			return false
		}
		name, isBare := target.(*ast.Ident)
		if !isBare || name.Name == "_" || reaching[name.Name] {
			return false
		}
		reaching[name.Name] = true
		return true
	}
	for {
		grew := false
		ast.Inspect(function, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.AssignStmt:
				if len(typed.Lhs) == len(typed.Rhs) {
					for i, right := range typed.Rhs {
						grew = bind(typed.Lhs[i], right) || grew
					}
					return true
				}
				// one expression destructured across several names: the comma ok read.
				// The storage is the first name and what follows it is the ok, a bool,
				// which carries none.
				if len(typed.Rhs) == 1 && len(typed.Lhs) != 0 {
					grew = bind(typed.Lhs[0], typed.Rhs[0]) || grew
				}
			case *ast.RangeStmt:
				// the VALUE of a range names the storage; the key is an index or a map
				// key and names none.
				if typed.Value != nil {
					grew = bind(typed.Value, typed.X) || grew
				}
			}
			return true
		})
		if !grew {
			return slices.Sorted(maps.Keys(reaching))
		}
	}
}

// Whether an expression is an integer literal whose VALUE is zero, in any base.
//
// Read as a number and not as the text "0", which is the axis this gate's previous version was
// walked past on: 0x00, 0b0 and 000 are the same store and the same defect.
func zeroizeIsAZeroLiteral(expr ast.Expr) bool {
	literal, isLiteral := expr.(*ast.BasicLit)
	if !isLiteral || literal.Kind != token.INT {
		return false
	}
	value, err := strconv.ParseUint(strings.ReplaceAll(literal.Value, "_", ""), 0, 64)
	return err == nil && value == 0
}

// The subset of those names a body writes INTO rather than reads, reslices or passes on.
//
// Four spellings reach somebody else's array: an assignment of a zero through an index of it, an
// increment of one, the clear builtin, and copy with the name as its destination. Rebinding the
// header -- secret = something -- is deliberately not one of them: it moves the local name and
// leaves the caller's bytes exactly as they were. An index assignment of a NON zero value is not
// one either: that is a store into a container, which is what this package's receiver window does
// when it retains a rung.
func zeroizeNamesWrittenThrough(function *ast.FuncDecl, reaching []string) []string {
	written := map[string]bool{}
	mark := func(target ast.Expr) {
		if root := zeroizeRootIdentifierOf(target); root != "" && slices.Contains(reaching, root) {
			written[root] = true
		}
	}
	ast.Inspect(function, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.AssignStmt:
			for i, target := range typed.Lhs {
				if _, isIndex := target.(*ast.IndexExpr); !isIndex {
					continue
				}
				if len(typed.Rhs) <= i || !zeroizeIsAZeroLiteral(typed.Rhs[i]) {
					continue
				}
				mark(target)
			}
		case *ast.IncDecStmt:
			if _, isIndex := typed.X.(*ast.IndexExpr); isIndex {
				mark(typed.X)
			}
		case *ast.CallExpr:
			builtin, isName := typed.Fun.(*ast.Ident)
			if isName && len(typed.Args) != 0 && (builtin.Name == "clear" || builtin.Name == "copy") {
				mark(typed.Args[0])
			}
		}
		return true
	})
	return slices.Sorted(maps.Keys(written))
}

// The functions this body CALLS with one of those names as an argument.
//
// The callee is read by its bare name, so a method and a package level function sharing one name
// are one entry. That can only WIDEN the class, and a wider class demands the directive of more
// declarations rather than fewer, which is the direction a gate may be wrong in.
func zeroizeNamesHandedThatStorage(function *ast.FuncDecl, reaching []string) []string {
	handed := map[string]bool{}
	ast.Inspect(function, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		callee := ""
		switch named := call.Fun.(type) {
		case *ast.Ident:
			callee = named.Name
		case *ast.SelectorExpr:
			callee = named.Sel.Name
		}
		if callee == "" {
			return true
		}
		for _, argument := range call.Args {
			if root := zeroizeRootIdentifierOf(argument); root != "" && slices.Contains(reaching, root) {
				handed[callee] = true
			}
		}
		return true
	})
	return slices.Sorted(maps.Keys(handed))
}

func zeroizeCandidatesIn(fileSet *token.FileSet, parsed *ast.File, path string, named []string) []zeroizeCandidate {
	rendered := func(expr ast.Expr) string {
		text := strings.Builder{}
		if err := printer.Fprint(&text, fileSet, expr); err != nil {
			return ""
		}
		return text.String()
	}
	candidates := []zeroizeCandidate{}
	for _, declaration := range parsed.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Body == nil {
			continue
		}
		handed := zeroizeStorageOutlivingTheCall(function, rendered, named)
		if len(handed) == 0 {
			continue
		}
		reaching := zeroizeNamesReachingTheSameStorage(function, handed)
		candidates = append(candidates, zeroizeCandidate{
			file:      path,
			name:      function.Name.Name,
			spelled:   len(zeroizeNamesWrittenThrough(function, reaching)) != 0,
			handsOn:   zeroizeNamesHandedThatStorage(function, reaching),
			directive: zeroizeCarriesTheDirective(function.Doc),
		})
	}
	return candidates
}

func zeroizeCarriesTheDirective(doc *ast.CommentGroup) bool {
	if doc == nil {
		return false
	}
	for _, line := range doc.List {
		if strings.TrimSpace(line.Text) == zeroizeNoinlineDirective {
			return true
		}
	}
	return false
}

// The base class -- the declarations that spell the write -- closed under "hands that storage to
// a member", to a fixed point.
//
// There is no seed written down anywhere: the base is whatever spells a write, which today is
// zeroize and the control's own shapes, and a package that erased through some other primitive
// would derive that one instead.
func zeroizeEraseClass(candidates []zeroizeCandidate) ([]string, []zeroizeCandidate) {
	member := map[string]bool{}
	for _, candidate := range candidates {
		if candidate.spelled {
			member[candidate.name] = true
		}
	}
	for grew := true; grew; {
		grew = false
		for _, candidate := range candidates {
			if member[candidate.name] {
				continue
			}
			for _, callee := range candidate.handsOn {
				if member[callee] {
					member[candidate.name] = true
					grew = true
					break
				}
			}
		}
	}
	helpers := []string{}
	missing := []zeroizeCandidate{}
	for _, candidate := range candidates {
		if !member[candidate.name] {
			continue
		}
		helpers = append(helpers, candidate.name)
		if !candidate.directive {
			missing = append(missing, candidate)
		}
	}
	return helpers, missing
}

// Every name this package declares for a []byte, so an eraser written over one is read as the
// same eraser.
func zeroizeByteSliceTypeNames(sources []messagegroupSource) []string {
	names := []string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			general, isGeneral := declaration.(*ast.GenDecl)
			if !isGeneral || general.Tok != token.TYPE {
				continue
			}
			for _, spec := range general.Specs {
				typed, isTyped := spec.(*ast.TypeSpec)
				if !isTyped {
					continue
				}
				array, isArray := typed.Type.(*ast.ArrayType)
				if !isArray || array.Len != nil {
					continue
				}
				if element, isName := array.Elt.(*ast.Ident); isName && element.Name == "byte" {
					names = append(names, typed.Name.Name)
				}
			}
		}
	}
	slices.Sort(names)
	return names
}

// One file holding one of each shape, so a matcher that stopped matching fails HERE rather than
// reporting the package clean.
//
// The last three are the negative half. A read of the storage is not an erasure; a write into an
// array the function made itself is not one; and a store of a VALUE into a container the receiver
// holds is not one either -- that last is this package's own window retaining a rung, and it is
// the shape the value filter exists for.
const zeroizeEraseControl = "package control\n" +
	"\n" +
	"type ControlKey []byte\n" +
	"\n" +
	"type ControlHolder struct {\n" +
	"\tsecret []byte\n" +
	"\twindow map[uint64][]byte\n" +
	"}\n" +
	"\n" +
	"//go:noinline\n" +
	"func erasedWithTheDirective(secret []byte) {\n" +
	"\tfor i := range secret {\n" +
	"\t\tsecret[i] = 0\n" +
	"\t}\n" +
	"}\n" +
	"\n" +
	"// this one only says it carries the directive\n" +
	"func erasedWithOnlyProse(secret []byte) {\n" +
	"\tfor i := range secret {\n" +
	"\t\tsecret[i] = 0\n" +
	"\t}\n" +
	"}\n" +
	"\n" +
	"func erasedThroughALocalCutFromTheParameter(secret []byte) {\n" +
	"\twindow := secret[8:]\n" +
	"\tfor i := range window {\n" +
	"\t\twindow[i] = 0\n" +
	"\t}\n" +
	"}\n" +
	"\n" +
	"func erasedWithZeroWrittenInHex(secret []byte) {\n" +
	"\tfor i := range secret {\n" +
	"\t\tsecret[i] = 0x00\n" +
	"\t}\n" +
	"}\n" +
	"\n" +
	"func erasedWithClearOverNamedStorage(secret ControlKey) {\n" +
	"\tclear(secret)\n" +
	"}\n" +
	"\n" +
	"func erasedWithCopy(secret []byte) {\n" +
	"\tcopy(secret, make([]byte, len(secret)))\n" +
	"}\n" +
	"\n" +
	"func handsTheParameterToAnEraser(secret []byte) {\n" +
	"\terasedWithTheDirective(secret)\n" +
	"}\n" +
	"\n" +
	"func (self *ControlHolder) erasedThroughTheReceiver() {\n" +
	"\tfor i := range self.secret {\n" +
	"\t\tself.secret[i] = 0\n" +
	"\t}\n" +
	"}\n" +
	"\n" +
	"func (self *ControlHolder) erasedThroughACommaOkReadOfItsOwnMap(index uint64) {\n" +
	"\tsecret, ok := self.window[index]\n" +
	"\tif !ok {\n" +
	"\t\treturn\n" +
	"\t}\n" +
	"\terasedWithTheDirective(secret)\n" +
	"}\n" +
	"\n" +
	"func (self *ControlHolder) erasedThroughARangeOverItsOwnMap() {\n" +
	"\tfor _, secret := range self.window {\n" +
	"\t\terasedWithTheDirective(secret)\n" +
	"\t}\n" +
	"}\n" +
	"\n" +
	"func (self *ControlHolder) readsItsOwnStorageOnly() int {\n" +
	"\treturn len(self.secret)\n" +
	"}\n" +
	"\n" +
	"func writesIntoStorageOfItsOwn(length int) []byte {\n" +
	"\tlocal := make([]byte, length)\n" +
	"\tlocal[0] = 0\n" +
	"\treturn local\n" +
	"}\n" +
	"\n" +
	"func (self *ControlHolder) retainsARungInItsOwnWindow(index uint64, secret []byte) {\n" +
	"\tself.window[index] = secret\n" +
	"}\n"

func TestEveryEraseHelperOfThisPackageCarriesTheNoinlineDirective(t *testing.T) {
	const controlName = "the erase helper control"
	controlSet := token.NewFileSet()
	control, err := parser.ParseFile(controlSet, controlName, zeroizeEraseControl, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse the control: %v", err)
	}
	helpers, missing := zeroizeEraseClass(zeroizeCandidatesIn(controlSet, control, controlName, []string{"ControlKey"}))
	wantHelpers := []string{
		"erasedWithTheDirective",
		"erasedWithOnlyProse",
		"erasedThroughALocalCutFromTheParameter",
		"erasedWithZeroWrittenInHex",
		"erasedWithClearOverNamedStorage",
		"erasedWithCopy",
		"handsTheParameterToAnEraser",
		"erasedThroughTheReceiver",
		"erasedThroughACommaOkReadOfItsOwnMap",
		"erasedThroughARangeOverItsOwnMap",
	}
	if !slices.Equal(helpers, wantHelpers) {
		t.Fatalf("the matcher read %v out of the control as erase helpers, want %v; it is not telling a write through storage that outlives the call -- spelled, or handed to an eraser -- from a read of one, from a write into storage of its own, or from a store of a value into a container",
			helpers, wantHelpers)
	}
	missingNames := []string{}
	for _, candidate := range missing {
		missingNames = append(missingNames, candidate.name)
	}
	wantMissing := []string{
		"erasedWithOnlyProse",
		"erasedThroughALocalCutFromTheParameter",
		"erasedWithZeroWrittenInHex",
		"erasedWithClearOverNamedStorage",
		"erasedWithCopy",
		"handsTheParameterToAnEraser",
		"erasedThroughTheReceiver",
		"erasedThroughACommaOkReadOfItsOwnMap",
		"erasedThroughARangeOverItsOwnMap",
	}
	if !slices.Equal(missingNames, wantMissing) {
		t.Fatalf("the matcher read %v out of the control as missing the directive, want %v; it is not telling the directive from the prose that argues for it",
			missingNames, wantMissing)
	}

	fileSet, sources := messagegroupProductionSources(t)
	named := zeroizeByteSliceTypeNames(sources)
	candidates := []zeroizeCandidate{}
	for _, source := range sources {
		candidates = append(candidates, zeroizeCandidatesIn(fileSet, source.parsed, source.path, named)...)
	}
	found, unprotected := zeroizeEraseClass(candidates)
	// the positive controls on the real source, one per half of the derivation. This package
	// certainly declares one helper that spells the write and several that hand the storage to
	// it, and a scan that had stopped finding either would report the same clean run a complete
	// one reports.
	if !slices.Contains(found, "zeroize") {
		t.Fatalf("the scan read %v as this package's erase helpers and zeroize is not among them, so it is not reading what it claims to", found)
	}
	if !slices.Contains(found, "Zeroize") {
		t.Fatalf("the scan read %v as this package's erase helpers and no Zeroize method is among them, so the class is not closed under the hand-off and every erasure written as one zeroize call is outside it",
			found)
	}
	if len(unprotected) != 0 {
		reported := []string{}
		for _, candidate := range unprotected {
			reported = append(reported, candidate.file+": "+candidate.name)
		}
		t.Errorf("%v erase storage that outlives the call -- a caller's array or the receiver's own -- without a %s line of their own; that directive is the only thing between these stores and a compiler entitled to delete them, and zeroize.go's own comment says so",
			reported, zeroizeNoinlineDirective)
	}
	t.Logf("%d erase helpers read out of this package's source: %v", len(found), found)
}
