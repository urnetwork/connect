// Tests for the RFC 9420 section 8 key schedule. This task covers only the two pieces
// every later one rests on: the typed errors and the zeroize helper.
//
// On what the zeroize tests do and do not claim. Reading a secret back through the same
// slice header proves a write happened; it says nothing about whether the caller's
// secret is gone, because a secret that was copied, appended or converted to a string
// lives in a second array this helper was never given. The tests below therefore assert
// the three things go can actually observe — every byte of the given slice becomes zero,
// a slice header taken over the same array BEFORE the call sees those zeros, and nothing
// outside the given slice is touched — and the limit beyond that is written down in
// secret_zeroize.go rather than dressed up as a test. See that file's comment.
//
// The fourth thing that file's argument rests on is the //go:noinline directive, and it
// is read out of the source here rather than left to prose. No go test can observe a
// store being elided; what a test can observe is whether the one mechanism the file says
// prevents the elision is still written down.
//
// On the error list. keyScheduleOwnedErrors below is a transcription, and a transcription
// of a class that exists in the tree is the failure mode this repository has paid for
// fourteen times. It is held to errors_key_schedule.go's own declarations by
// TestKeyScheduleOwnedErrorsIsEveryDeclarationOfItsFile, so the two sweeps that run over
// it — distinctness, and the exclusivity of syntax.ErrTrailingBytes — run over the class
// rather than over a copy of it that an eleventh error can be left out of.
package mls

import (
	"bytes"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// TestZeroizeSecretClearsEveryByteItWasGiven sweeps lengths rather than testing one,
// because the mistakes this helper invites are off by one at an end: a loop stopping a
// byte short, a loop starting at one. Every byte starts non zero, so "already zero"
// cannot be mistaken for "cleared".
func TestZeroizeSecretClearsEveryByteItWasGiven(t *testing.T) {
	for _, length := range []int{0, 1, 2, 3, 7, 8, 15, 16, 31, 32, 33, 64, 255} {
		secret := make([]byte, length)
		for i := range secret {
			secret[i] = byte(i%251) + 1
		}
		zeroizeSecret(secret)
		for i, b := range secret {
			if b != 0 {
				t.Fatalf("length %d: byte %d is %d after zeroize, want 0", length, i, b)
			}
		}
	}
}

// TestZeroizeSecretIsVisibleThroughASliceTakenBeforeTheCall is the closest go lets a
// test come to "the caller's secret is gone": a second header over the same backing
// array, created before the call, must observe the zeros. An implementation that
// erased a copy of its argument — zeroing append([]byte(nil), secret...) rather than
// secret — passes a read back through the argument and fails here.
func TestZeroizeSecretIsVisibleThroughASliceTakenBeforeTheCall(t *testing.T) {
	backing := make([]byte, 8)
	for i := range backing {
		backing[i] = byte(i) + 1
	}
	alias := backing[2:6]
	zeroizeSecret(backing)
	for i, b := range alias {
		if b != 0 {
			t.Fatalf("alias byte %d is %d; the caller's own view of the secret survived the erase", i, b)
		}
	}
}

// TestZeroizeSecretTouchesNothingOutsideTheSliceItWasGiven pins the other direction.
// The window has capacity past its length, so an implementation reaching for
// secret[:cap(secret)] — a plausible "erase everything the array could hold" — would
// clobber bytes the caller still owns. Erasing too much is a data corruption bug, not
// an over eager security measure.
func TestZeroizeSecretTouchesNothingOutsideTheSliceItWasGiven(t *testing.T) {
	backing := make([]byte, 8)
	for i := range backing {
		backing[i] = byte(i) + 1
	}
	window := backing[2:6]
	if cap(window) <= len(window) {
		t.Fatalf("window has cap %d and len %d, so the reslice past len this test is about is unreachable",
			cap(window), len(window))
	}
	zeroizeSecret(window)
	for i := range backing {
		want := byte(i) + 1
		if 2 <= i && i < 6 {
			want = 0
		}
		if backing[i] != want {
			t.Fatalf("backing byte %d is %d, want %d; zeroize did not stop at the slice it was given",
				i, backing[i], want)
		}
	}
}

// TestZeroizeSecretAcceptsNilAndEmpty asserts the no-guard-at-the-call-site contract.
// An implementation indexing before it checks the length panics here.
func TestZeroizeSecretAcceptsNilAndEmpty(t *testing.T) {
	zeroizeSecret(nil)
	zeroizeSecret([]byte{})
	zeroizeSecret(make([]byte, 0, 16))
}

// noInlineDirective is the compiler directive, matched as a whole line. secret_zeroize.go
// argues from the directive by name twice in prose, so a substring search over the file is
// answered by the argument for the directive rather than by the directive, and deleting
// the line leaves both mentions behind. That is why this is compared against a comment of
// the syntax tree, and against the whole of it.
const noInlineDirective = "//go:noinline"

// carriesTheNoInlineDirective reports whether a doc group holds the directive as one of
// its own lines. The text is trimmed because a carriage return on the end of the line is
// what makes an exact comparison silently answer no on windows and yes everywhere else.
//
// core.autocrlf being on is no longer how one gets there. It is set at system scope on the
// windows boxes that build this repository and `*.go text eol=lf` exempts every Go file from
// it, so the trim stays for the route that pin does not close: a tool writing crlf into the
// working tree.
func carriesTheNoInlineDirective(doc *ast.CommentGroup) bool {
	if doc == nil {
		return false
	}
	for _, comment := range doc.List {
		if strings.TrimSpace(comment.Text) == noInlineDirective {
			return true
		}
	}
	return false
}

// mustParseCommented is the shared parse with the doc comments kept.
//
// mustParseSource drops them, because every other gate in this package reads statements
// and types rather than prose. The matcher below reads a compiler directive, which lives
// in a doc comment and nowhere else, and over a tree parsed without comments it would
// answer "absent" for every function in the package -- a gate that reports the whole
// source in violation rather than one that reads it. Its own control is what says the
// difference is being seen.
func mustParseCommented(t *testing.T, name string, source string) parsedSource {
	t.Helper()
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, name, source, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return parsedSource{fileSet: fileSet, file: file}
}

// mustReadCommented is the same over one file of this package's source on disk.
func mustReadCommented(t *testing.T, path string) parsedSource {
	t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return mustParseCommented(t, path, string(source))
}

// rootIdentifierOf reduces the target of a write to the name whose storage is written
// through, so secret[i], secret[1:][i], (secret)[i] and self.secrets.InitSecret[i] all report
// the name they hang off.
//
// Without it a write spelled through a reslice, or through a field of the receiver, sits
// outside the matcher below while landing on exactly the same bytes -- and those are the two
// shapes an erase helper actually takes: a free one is handed the slice, and a method erases
// what its own receiver holds.
func rootIdentifierOf(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.ParenExpr:
		return rootIdentifierOf(typed.X)
	case *ast.IndexExpr:
		return rootIdentifierOf(typed.X)
	case *ast.SliceExpr:
		return rootIdentifierOf(typed.X)
	case *ast.SelectorExpr:
		return rootIdentifierOf(typed.X)
	case *ast.StarExpr:
		return rootIdentifierOf(typed.X)
	}
	return ""
}

// storageOutlivingTheCall names the receiver and parameters of one declaration through which
// a write reaches bytes that are still there when the call returns.
//
// Two sources. A parameter whose type is a []byte, or one of this package's own names for a
// []byte -- the named types are read rather than the spelling alone, because HpkePrivateKey
// is the same array to the compiler and an eraser written over one is the same eraser. And
// the receiver, whatever its type, because a method that erases what its own object holds is
// the other shape an erase helper takes -- the (*KeySchedule).Zeroize this plan's task 12
// adds is exactly it, and a class that stopped at parameters would let it land undirected.
func storageOutlivingTheCall(parsed parsedSource, function *ast.FuncDecl, named []string) []string {
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
		rendered := parsed.render(field.Type)
		if rendered != "[]byte" && !slices.Contains(named, rendered) {
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

// namesReachingTheSameStorage extends a set of names by the locals cut from them, to a
// fixed point.
//
// A local assigned from a parameter, or from a reslice of one, is the caller's array under
// another name, and an erase written through it is the same erase. One hop is the shape
// that actually gets written -- window := secret[n:] -- and the fixed point follows a chain
// of them. A local built by make or by bytes.Clone roots at a call rather than at a name and
// is correctly not added: that is storage of the function's own.
//
// Two further spellings bind a name to storage the call does not own, and both are how this
// package's own erasers are written rather than shapes imagined for a control. A comma ok
// read of a map the receiver holds -- keys, ok := self.window[generation] -- puts two names
// on the left of one expression, which the pairwise walk below steps straight past; that is
// eraseKey. And the value of a range over one -- for _, secret := range self.nodes -- binds
// the same storage with no assignment at all; that is (*SecretTree).Zeroize. Measured, not
// supposed: without these two neither function reached the class the gate below derives,
// while that gate's own comment named the second shape as the one it exists for.
func namesReachingTheSameStorage(function *ast.FuncDecl, handed []string) []string {
	reaching := map[string]bool{}
	for _, name := range handed {
		reaching[name] = true
	}
	// bind answers whether it added a name, so the fixed point below terminates on the pass
	// that adds nothing rather than on a count of hops.
	bind := func(target ast.Expr, source ast.Expr) bool {
		root := rootIdentifierOf(source)
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
				// one expression destructured across several names: the comma ok read. The
				// storage is the first name, and what follows it is the ok -- a bool, which
				// carries none.
				if len(typed.Rhs) == 1 && len(typed.Lhs) != 0 {
					grew = bind(typed.Lhs[0], typed.Rhs[0]) || grew
				}
			case *ast.RangeStmt:
				// the VALUE of a range names the storage; the key is an index or a map key
				// and names none.
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

// namesWrittenThrough is the subset of those a body writes INTO rather than reads,
// reslices or passes on.
//
// Three spellings reach somebody else's array: an index assignment or an increment of one,
// the clear builtin, and copy with the name as its destination. Rebinding the header --
// secret = something -- is deliberately not one of them: it moves the local name and leaves
// the caller's bytes exactly as they were.
func namesWrittenThrough(function *ast.FuncDecl, reaching []string) []string {
	written := map[string]bool{}
	mark := func(target ast.Expr) {
		if root := rootIdentifierOf(target); root != "" && slices.Contains(reaching, root) {
			written[root] = true
		}
	}
	ast.Inspect(function, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.AssignStmt:
			for _, target := range typed.Lhs {
				if _, isIndex := target.(*ast.IndexExpr); isIndex {
					mark(target)
				}
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

// namesHandedThatStorage is the functions this body CALLS with one of those names as an
// argument.
//
// It is the other half of "writes through storage outliving the call". A body that hands the
// caller's array to an eraser has erased it just as surely as one that spells the loop, and
// in this package that is how erasure is nearly always written: one zeroizeSecret, called
// with the storage. A matcher reading only the spelled writes put eraseKey, (*ratchet).step,
// (*ratchet).zeroize, (*KeySchedule).Zeroize and (*SecretTree).Zeroize in no class at all --
// three of them carrying a comment claiming membership of this one, and two of them carrying
// no directive.
//
// An argument and NOT a receiver. A method called on the same object -- self.pruneRetained()
// -- reaches that object's storage too, but so does every exported method of a type that
// erases anything anywhere, so counting receivers closes the class over the whole type and
// ends by demanding the directive of MessageKey and receiverKey. What separates those from an
// erase helper is intent, and no matcher reads intent. The line is drawn where a name is
// handed over, which is the shape zeroizeSecret is called in everywhere here.
//
// The callee is read by its bare name, so a method and a package level function sharing one
// name are one entry. That can only WIDEN the class -- it admits a body whose callee merely
// shares a name with an eraser -- and a wider class demands the directive of more
// declarations rather than fewer, which is the direction a gate is allowed to be wrong in.
func namesHandedThatStorage(function *ast.FuncDecl, reaching []string) []string {
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
			if root := rootIdentifierOf(argument); root != "" && slices.Contains(reaching, root) {
				handed[callee] = true
			}
		}
		return true
	})
	return slices.Sorted(maps.Keys(handed))
}

// eraseHelperCandidate is one declaration as the matchers read it: whether its own body
// spells a write through storage that outlives the call, and which functions it hands that
// storage to.
//
// The two are kept apart because only the first can be decided by inspecting the declaration
// alone. Whether handing the storage on is an erasure depends on what the callee does, which
// is a question about the package rather than about this file, and it is answered by the
// closure below.
type eraseHelperCandidate struct {
	file      string
	name      string
	spelled   bool
	handsOn   []string
	directive bool
}

// eraseHelperCandidatesIn reads every declaration of one parsed file that is handed storage
// outliving the call.
func eraseHelperCandidatesIn(parsed parsedSource, path string, named []string) []eraseHelperCandidate {
	candidates := []eraseHelperCandidate{}
	for _, declaration := range parsed.file.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Body == nil {
			continue
		}
		handed := storageOutlivingTheCall(parsed, function, named)
		if len(handed) == 0 {
			continue
		}
		reaching := namesReachingTheSameStorage(function, handed)
		candidates = append(candidates, eraseHelperCandidate{
			file:      path,
			name:      function.Name.Name,
			spelled:   len(namesWrittenThrough(function, reaching)) != 0,
			handsOn:   namesHandedThatStorage(function, reaching),
			directive: carriesTheNoInlineDirective(function.Doc),
		})
	}
	return candidates
}

// eraseHelperClass closes the base class -- the declarations that spell the write -- under
// "hands that storage to a member", and answers the members in declaration order together
// with the members carrying no directive.
//
// The closure is a fixed point rather than one hop, so a helper reached through a chain of
// them is a member. There is no seed written down anywhere: the base is whatever spells a
// write, which today is zeroizeSecret and the four control shapes, and a package that erased
// through some other primitive would derive that one instead.
func eraseHelperClass(candidates []eraseHelperCandidate) ([]string, []eraseHelperCandidate) {
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
	missing := []eraseHelperCandidate{}
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

// eraseHelperControl holds one of each shape the matchers have to tell apart: the four
// spellings that reach a caller's array, the same write made through a field of the receiver,
// the three ways a body hands that storage to an eraser instead of writing it itself, a write
// into storage the function made for itself, storage of its own handed to an eraser, the
// caller's array handed to something that is not one, a read of a parameter, a read of the
// receiver, a rebinding of a parameter, a function handed no bytes at all, the directive
// present, and the directive named only in prose.
//
// Without it a matcher that had stopped matching -- a parse that dropped comments, a walk
// that stopped descending into bodies, a type filter that stopped seeing named storage, a
// closure that stopped following the hand-off -- would report the real source clean and pass,
// which is the one outcome a gate must never be able to reach by accident.
const eraseHelperControl = "package control\n" +
	"\n" +
	"type ControlKey []byte\n" +
	"\n" +
	"// erasedWithTheDirective is what this gate wants: a write through a parameter, with\n" +
	"// the directive on a line of its own.\n" +
	"//\n" +
	"//go:noinline\n" +
	"func erasedWithTheDirective(secret []byte) {\n" +
	"\tfor i := range secret {\n" +
	"\t\tsecret[i] = 0\n" +
	"\t}\n" +
	"}\n" +
	"\n" +
	"// erasedWithOnlyProse argues from the noinline directive and does not carry it. This\n" +
	"// is the shape a search for the text noinline cannot tell from the one above.\n" +
	"func erasedWithOnlyProse(secret []byte) {\n" +
	"\tfor i := range secret {\n" +
	"\t\tsecret[i] = 0\n" +
	"\t}\n" +
	"}\n" +
	"\n" +
	"func erasedThroughALocalCutFromTheParameter(secret []byte) {\n" +
	"\twindow := secret[1:]\n" +
	"\twindow[0] = 0\n" +
	"}\n" +
	"\n" +
	"func erasedWithClearOverNamedStorage(key ControlKey) {\n" +
	"\tclear(key)\n" +
	"}\n" +
	"\n" +
	"func erasedWithCopy(secret []byte) {\n" +
	"\tcopy(secret, make([]byte, len(secret)))\n" +
	"}\n" +
	"\n" +
	"// handsTheParameterToAnEraser spells no write at all and erases the caller's array\n" +
	"// just the same. It is the shape every erasure in this package is actually written in.\n" +
	"func handsTheParameterToAnEraser(secret []byte) {\n" +
	"\terasedWithTheDirective(secret)\n" +
	"}\n" +
	"\n" +
	"// handsAnEraserStorageOfItsOwn calls the same eraser with an array it made, which\n" +
	"// erases nothing anybody else can see.\n" +
	"func handsAnEraserStorageOfItsOwn(secret []byte) {\n" +
	"\tout := make([]byte, len(secret))\n" +
	"\terasedWithTheDirective(out)\n" +
	"}\n" +
	"\n" +
	"// handsTheParameterToSomethingThatDoesNotErase is the other half: the storage is the\n" +
	"// caller's and the callee is not a member, so neither is this.\n" +
	"func handsTheParameterToSomethingThatDoesNotErase(secret []byte) int {\n" +
	"\treturn readsAParameter(secret)\n" +
	"}\n" +
	"\n" +
	"func writesOnlyIntoStorageOfItsOwn(secret []byte) []byte {\n" +
	"\tout := make([]byte, len(secret))\n" +
	"\tcopy(out, secret)\n" +
	"\tout[0] = 0\n" +
	"\treturn out\n" +
	"}\n" +
	"\n" +
	"func readsAParameter(secret []byte) int {\n" +
	"\treturn int(secret[0])\n" +
	"}\n" +
	"\n" +
	"func rebindsAParameter(secret []byte) []byte {\n" +
	"\tsecret = nil\n" +
	"\treturn secret\n" +
	"}\n" +
	"\n" +
	"func takesNoBytesAtAll(count int) int {\n" +
	"\treturn count + 1\n" +
	"}\n" +
	"\n" +
	"type ControlHolder struct {\n" +
	"\tsecret []byte\n" +
	"\twindow map[int][]byte\n" +
	"}\n" +
	"\n" +
	"// erasedThroughTheReceiver is the shape a Zeroize method takes: the storage is the\n" +
	"// object's own rather than a caller's, and the write is the same write.\n" +
	"func (self *ControlHolder) erasedThroughTheReceiver() {\n" +
	"\tfor i := range self.secret {\n" +
	"\t\tself.secret[i] = 0\n" +
	"\t}\n" +
	"}\n" +
	"\n" +
	"// erasedThroughACommaOkReadOfItsOwnMap is eraseKey: two names on the left of one\n" +
	"// expression, and the erase written through the first of them.\n" +
	"func (self *ControlHolder) erasedThroughACommaOkReadOfItsOwnMap(at int) {\n" +
	"\theld, ok := self.window[at]\n" +
	"\tif !ok {\n" +
	"\t\treturn\n" +
	"\t}\n" +
	"\terasedWithTheDirective(held)\n" +
	"}\n" +
	"\n" +
	"// erasedThroughARangeOverItsOwnMap is (*SecretTree).Zeroize: the name is bound by a\n" +
	"// range rather than by an assignment, and it is the same storage.\n" +
	"func (self *ControlHolder) erasedThroughARangeOverItsOwnMap() {\n" +
	"\tfor _, held := range self.window {\n" +
	"\t\terasedWithTheDirective(held)\n" +
	"\t}\n" +
	"}\n" +
	"\n" +
	"func (self *ControlHolder) readsItsOwnStorageOnly() int {\n" +
	"\treturn len(self.secret)\n" +
	"}\n"

// TestEveryEraseHelperCarriesTheNoInlineDirective observes the one mechanism
// secret_zeroize.go's argument rests on, over every function this package declares rather
// than over one file of it.
//
// What that file claims is that the stores reach memory. A compiler may delete a write to
// memory it can prove is never read again, and in a caller that drops the secret straight
// afterwards these writes are exactly that; across a call it cannot inline it cannot make
// the proof. So the directive is not decoration, it is the whole mechanism.
//
// The class is what a function DOES and not where it is written. The version this replaces
// parsed secret_zeroize.go by name, while its own comment claimed that "a second erase
// helper landing beside it is held to the same rule without anyone remembering to extend a
// list" -- true of a helper landing beside it and false of one landing anywhere else.
// Measured, not supposed: the identical helper declared in key_schedule.go and used to
// erase member_secret left that gate silent, and with the one line exemption a contributor
// would copy verbatim from zeroizeSecret's own row in packageConstructionsOverBorrowedBytes
// the whole package was green -- with member_secret erased by a helper the compiler is
// entitled to inline and elide.
//
// "What a function does" then has to include HANDING the storage to an eraser, which is the
// second understatement this gate has been through. A body that spells the write is one
// shape of erasure and this package barely uses it: there is one zeroizeSecret, and every
// other eraser here calls it. A matcher reading only the spelled writes derived a class of
// exactly one member out of the real source, and left outside it every function whose
// comment claims membership -- eraseKey, (*ratchet).step, (*ratchet).zeroize -- along with
// the two Zeroize methods, one of which this gate's own comment named as the shape it exists
// for. Measured: with the //go:noinline line deleted from (*ratchet).eraseKey, all 526 tests
// of this package passed. The class is therefore closed under the hand-off, by
// eraseHelperClass, from a base nobody writes down.
//
// Why "writes through storage its caller owns" is the same set as "erases a secret" here
// rather than a wider one: a construction of this package that writes into an array it was
// handed and is NOT an eraser is already forbidden, by
// TestEveryConstructionInThisPackageLeavesItsInputAlone, whose own excuse for zeroizeSecret
// is that writing into the caller's array is the function. The two gates meet on exactly
// this class, which is why neither of them has to name it.
//
// Two honest limits, stated rather than hidden. No go test can observe an elision, so this
// asserts the presence of the mechanism and not the effect -- the proxy the file's own
// argument is made of. And the hand-off is followed by the ARGUMENT and not by the receiver:
// a method that erases through a member call on its own object -- evictOldest, which chooses
// a generation and leaves the erase to eraseKey -- is outside this class, because counting
// receivers closes it over every exported method of a type that erases anything anywhere.
// Those declarations carry the directive by the convention this package keeps rather than
// because anything here demands it, and their comments say so.
func TestEveryEraseHelperCarriesTheNoInlineDirective(t *testing.T) {
	const controlName = "the erase helper control"
	control := mustParseCommented(t, controlName, eraseHelperControl)
	helpers, missing := eraseHelperClass(eraseHelperCandidatesIn(control, controlName, []string{"ControlKey"}))
	wantHelpers := []string{
		"erasedWithTheDirective",
		"erasedWithOnlyProse",
		"erasedThroughALocalCutFromTheParameter",
		"erasedWithClearOverNamedStorage",
		"erasedWithCopy",
		"handsTheParameterToAnEraser",
		"erasedThroughTheReceiver",
		"erasedThroughACommaOkReadOfItsOwnMap",
		"erasedThroughARangeOverItsOwnMap",
	}
	if !slices.Equal(helpers, wantHelpers) {
		t.Fatalf("the matcher read %v out of the control as erase helpers, want %v; it is not telling a write through the caller's array -- spelled, or handed to an eraser -- from a read of one or from a write into storage of the function's own",
			helpers, wantHelpers)
	}
	missingNames := []string{}
	for _, candidate := range missing {
		missingNames = append(missingNames, candidate.name)
	}
	wantMissing := []string{
		"erasedWithOnlyProse",
		"erasedThroughALocalCutFromTheParameter",
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

	named := packageByteSliceTypeNames(t)
	candidates := []eraseHelperCandidate{}
	for _, path := range packageLevelFunctions(t).files {
		candidates = append(candidates, eraseHelperCandidatesIn(mustReadCommented(t, path), path, named)...)
	}
	found, unprotected := eraseHelperClass(candidates)
	// the positive controls on the real source, one per half of the derivation. This package
	// certainly declares one helper that spells the write and several that hand the storage
	// to it, and a scan that had stopped finding either would report the same clean run a
	// complete one reports.
	if !slices.Contains(found, "zeroizeSecret") {
		t.Fatalf("the scan read %v as this package's erase helpers and zeroizeSecret is not among them, so it is not reading what it claims to",
			found)
	}
	if !slices.Contains(found, "eraseKey") {
		t.Fatalf("the scan read %v as this package's erase helpers and eraseKey is not among them, so the class is not closed under the hand-off and every erasure written as one zeroizeSecret call is outside it",
			found)
	}
	if len(unprotected) != 0 {
		reported := []string{}
		for _, candidate := range unprotected {
			reported = append(reported, candidate.file+": "+candidate.name)
		}
		t.Errorf("%v erase storage that outlives the call -- a caller's array or the receiver's own -- without a %s line of their own; that directive is the only thing between these stores and a compiler entitled to delete them, and secret_zeroize.go's own comment says so",
			reported, noInlineDirective)
	}
	t.Logf("%d erase helpers read out of this package's source: %v", len(found), found)
}

// keyScheduleErrorsFile is the single file this plan declares its typed errors in. Every
// gate below derives its class from that file rather than from the list, which is the
// difference between sweeping the class and sweeping a copy of it.
const keyScheduleErrorsFile = "errors_key_schedule.go"

// keyScheduleOwnedErrors is registry section 5.6's ten plus ErrNilGroupContext and
// ErrEpochErased, keyed by the name each is declared under so the derivation below can
// compare the two sets by name.
// ErrPskNonceLength, ErrPskType and ErrDuplicatePsk are deliberately absent — they are
// ValSem401, ValSem402 and ValSem403 and belong to the validation plan's errors.go.
//
// ErrNilGroupContext is the one name here the registry does not carry. It is not a protocol
// condition; it is the refusal that stands where DeriveJoinerSecret used to raise a nil
// pointer dereference out of syntax.Marshal, and it is declared beside the others because
// this file is where the key schedule's errors live and a second declaration site is how
// two sentinels for one condition happen.
//
// Nothing here is trusted: TestKeyScheduleOwnedErrorsIsEveryDeclarationOfItsFile holds
// this map to what errors_key_schedule.go actually declares, in both directions.
var keyScheduleOwnedErrors = map[string]error{
	"ErrSecretLength":                          ErrSecretLength,
	"ErrExportLength":                          ErrExportLength,
	"ErrEpochErased":                           ErrEpochErased,
	"ErrGroupContextTrailingBytes":             ErrGroupContextTrailingBytes,
	"ErrNilGroupContext":                       ErrNilGroupContext,
	"ErrNilCryptoProvider":                     ErrNilCryptoProvider,
	"ErrTranscriptHashLength":                  ErrTranscriptHashLength,
	"ErrPskCount":                              ErrPskCount,
	"ErrSecretTreeLeafOutOfRange":              ErrSecretTreeLeafOutOfRange,
	"ErrSecretTreeConsumed":                    ErrSecretTreeConsumed,
	"errSecretTreeDescentDidNotStoreTheTarget": errSecretTreeDescentDidNotStoreTheTarget,
	"ErrRatchetGenerationConsumed":             ErrRatchetGenerationConsumed,
	"ErrRatchetGenerationTooFarAhead":          ErrRatchetGenerationTooFarAhead,
	"ErrRatchetExhausted":                      ErrRatchetExhausted,
	"errRatchetTypeHasNoRoot":                  errRatchetTypeHasNoRoot,
	"ErrUnknownContentType":                    ErrUnknownContentType,
}

// TestKeyScheduleOwnedErrorsIsEveryDeclarationOfItsFile derives the class the two sweeps
// below run over instead of trusting the transcription of it.
//
// This is the half that was missing and it is the half with the consequence. An eleventh
// error declared in errors_key_schedule.go and left off the list is judged by neither
// sweep: distinctness never sees it, and the exclusivity sweep — the one that says only
// ErrGroupContextTrailingBytes may answer to syntax.ErrTrailingBytes — counts one match
// and passes while two errors in production answer to that sentinel, which is exactly the
// condition it exists to forbid. A count assertion cannot close that, because it has the
// wrong polarity: it fails on adding an error and remembering the list, and stays quiet on
// adding one and forgetting it.
//
// The derivation is every package level name the file declares, not every name that looks
// like an error, because that file holds nothing else by design and a name this gate
// cannot classify should be loud. A helper or a type landing there fails here too, which
// says either move it or widen the rule on purpose.
func TestKeyScheduleOwnedErrorsIsEveryDeclarationOfItsFile(t *testing.T) {
	declared := packageLevelDeclarations(t, ".")
	fromFile := map[string]bool{}
	for name, file := range declared {
		if file == keyScheduleErrorsFile {
			fromFile[name] = true
		}
	}
	if len(fromFile) == 0 {
		t.Fatalf("the scan found nothing declared in %s, so this gate compared the list against an empty set",
			keyScheduleErrorsFile)
	}
	if !fromFile["ErrSecretLength"] {
		t.Fatalf("the scan did not find ErrSecretLength among the declarations of %s, which certainly declares it, so it is reading something other than that file",
			keyScheduleErrorsFile)
	}
	for _, name := range slices.Sorted(maps.Keys(fromFile)) {
		if _, listed := keyScheduleOwnedErrors[name]; !listed {
			t.Errorf("%s declares %s and keyScheduleOwnedErrors does not list it, so neither the distinctness sweep nor the syntax.ErrTrailingBytes exclusivity sweep judges it; add it there",
				keyScheduleErrorsFile, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(keyScheduleOwnedErrors)) {
		if !fromFile[name] {
			t.Errorf("keyScheduleOwnedErrors lists %s and %s does not declare it, so the sweeps run over a name this file does not own",
				name, keyScheduleErrorsFile)
		}
	}
}

// TestKeyScheduleErrorsAreDistinct asserts no two of the typed errors alias each other,
// so a test asserting a specific failure cannot be answered yes by the wrong one. Two
// vars sharing one errors.New value make a length failure indistinguishable from an
// exhaustion failure at every call site that branches on them.
//
// The count assertion pins the file: a name added to it is a change somebody has to make
// deliberately, in the same commit, with a reason. What stops the list shrinking, and what
// stops it lagging behind the file, is the derivation above rather than this number.
//
// Sixteen rather than the ten of registry section 5.6, and all six extras are here for one
// reason: a second declaration site is how two sentinels for one condition happen.
// ErrNilGroupContext and ErrNilCryptoProvider name an argument that was missing rather than a
// protocol condition. ErrEpochErased names a state of the epoch itself. ErrUnknownContentType
// names a framing code point with no ratchet behind it, and is declared here rather than
// beside the ContentType it refuses for the same reason: this file is the secret tree's
// declaration site, and the refusal is the secret tree's.
// errSecretTreeDescentDidNotStoreTheTarget and errRatchetTypeHasNoRoot are the file's two
// unexported names, and both are unexported on purpose: each is wrapped alongside an exported
// sentinel so no caller has to know it exists, and each exists so a test can tell one
// function's two same-sentinel returns apart -- takeLeafSecret's two consumed returns, and
// ratchetFor's two refusals of a ratchet type. Unexported or not, both are judged by the
// sweeps below like every other declaration of that file, which is what the derivation above
// is for.
func TestKeyScheduleErrorsAreDistinct(t *testing.T) {
	if len(keyScheduleOwnedErrors) != 16 {
		t.Fatalf("this plan owns %d errors, want the 10 of registry section 5.6 plus ErrNilGroupContext, ErrEpochErased, ErrNilCryptoProvider, ErrUnknownContentType and the two unexported invariant names errSecretTreeDescentDidNotStoreTheTarget and errRatchetTypeHasNoRoot", len(keyScheduleOwnedErrors))
	}
	names := slices.Sorted(maps.Keys(keyScheduleOwnedErrors))
	for i, name := range names {
		first := keyScheduleOwnedErrors[name]
		if first == nil {
			t.Fatalf("%s is nil", name)
		}
		if first.Error() == "" {
			t.Fatalf("%s has an empty message", name)
		}
		for j, other := range names {
			if i == j {
				continue
			}
			second := keyScheduleOwnedErrors[other]
			if errors.Is(first, second) {
				t.Errorf("%s aliases %s: %v", name, other, first)
			}
			if first.Error() == second.Error() {
				t.Errorf("%s and %s carry the same message %q, so a log cannot tell them apart",
					name, other, first.Error())
			}
		}
	}
}

// TestGroupContextTrailingBytesWrapsTheSyntaxError asserts the group context trailing
// byte condition is reachable through the syntax package's own sentinel, so a caller
// that only knows syntax.ErrTrailingBytes still matches it. Two names exist because
// syntax.Unmarshal is what enforces full consumption while this value is what names the
// condition for the group context specifically.
func TestGroupContextTrailingBytesWrapsTheSyntaxError(t *testing.T) {
	if !errors.Is(ErrGroupContextTrailingBytes, syntax.ErrTrailingBytes) {
		t.Fatal("ErrGroupContextTrailingBytes does not wrap syntax.ErrTrailingBytes")
	}
}

// TestOnlyTheGroupContextErrorAnswersToTheSyntaxSentinel is the other half, and it is
// the half that has teeth. Wrapping is useful only while it stays exclusive: a second
// error of this file wrapping syntax.ErrTrailingBytes would make a caller asking "was
// this a trailing byte problem" get yes for a length mistake. The sweep is over the same
// map as the distinctness test, and that map is held to errors_key_schedule.go's own
// declarations, so what is judged here is what production declares.
func TestOnlyTheGroupContextErrorAnswersToTheSyntaxSentinel(t *testing.T) {
	matched := 0
	for _, name := range slices.Sorted(maps.Keys(keyScheduleOwnedErrors)) {
		err := keyScheduleOwnedErrors[name]
		if !errors.Is(err, syntax.ErrTrailingBytes) {
			continue
		}
		matched++
		if err != ErrGroupContextTrailingBytes {
			t.Errorf("%s (%v) also answers to syntax.ErrTrailingBytes; only ErrGroupContextTrailingBytes may",
				name, err)
		}
	}
	if matched != 1 {
		t.Fatalf("%d of the errors %s declares answer to syntax.ErrTrailingBytes, want exactly 1",
			matched, keyScheduleErrorsFile)
	}
}

// One test this task's plan asked for is not here, and this is the note that says so
// rather than letting its absence look like an oversight.
//
// TestPskSentinelsBelongToTheValidationPlan asserts that ErrPskNonceLength, ErrPskType
// and ErrDuplicatePsk resolve to the validation plan's declarations and that
// ValSem(ValSem401, detail) preserves its detail under errors.Is. None of those five
// names exists in this package yet, so the test cannot compile, and an undefined name
// takes the whole package down rather than showing up as one red test.
//
// It is not forgotten: all five are in crossPlanSymbolsNotYetLanded in
// key_schedule_deps_test.go, so the moment the validation plan lands them that gate fails
// and names them. Whoever answers it writes their pins and this test in the same commit.

// ---------------------------------------------------------------------------
// task 5: ZeroSecret and DeriveJoinerSecret
// ---------------------------------------------------------------------------

// TestZeroSecretIsKdfNhZeroBytesForEverySuite asserts the three things a test can honestly
// observe about the all-zero secret: it is KDF.Nh bytes long, every one of them is zero,
// and two calls do not share storage.
//
// The suites are read out of the registry rather than listed, so a third registered suite
// with a different Nh is judged here instead of being quietly skipped.
//
// What is deliberately NOT claimed, for the reason key_schedule.go's comment gives: that
// this is the RIGHT zero. One run of Nh zero bytes is indistinguishable from another, so
// nothing about the returned value says it is the substitute RFC 9420 makes for a missing
// commit secret or a missing psk. The published corpora that expand over it say that, and
// they belong to the tasks that consume this function. A test here asserting more would be
// reassuring rather than checking, which is the shape task 2 wrote down instead of faking.
func TestZeroSecretIsKdfNhZeroBytesForEverySuite(t *testing.T) {
	suites := Suites()
	if len(suites) == 0 {
		t.Fatal("the registry named no suite, so this test examined nothing")
	}
	for _, suite := range suites {
		crypto := mustProvider(t, suite)
		if crypto.HashSize() == 0 {
			t.Fatalf("suite %#04x reports a hash size of 0, so an empty slice would satisfy every assertion below",
				uint16(suite))
		}
		zero := ZeroSecret(crypto)
		if len(zero) != crypto.HashSize() {
			t.Errorf("suite %#04x: ZeroSecret is %d bytes, want KDF.Nh = %d",
				uint16(suite), len(zero), crypto.HashSize())
			continue
		}
		for i, b := range zero {
			if b != 0 {
				t.Errorf("suite %#04x: byte %d of ZeroSecret is %d, want 0", uint16(suite), i, b)
				break
			}
		}
		// the key schedule erases what it has finished with, so a shared constant would
		// come back cleared on every later call with nothing to say why.
		zero[0] = 0xff
		again := ZeroSecret(crypto)
		if again[0] != 0 {
			t.Errorf("suite %#04x: ZeroSecret hands out storage a previous caller can write through",
				uint16(suite))
		}
		if &again[0] == &zero[0] {
			t.Errorf("suite %#04x: two calls to ZeroSecret returned the same backing array", uint16(suite))
		}
	}
}

// keyScheduleKatFile is the mlswg family the published joiner secrets live in, and
// keyScheduleKatJoinerComparisons is how many of them this package's registered suites
// account for: two suites, five epochs each.
//
// The count is asserted rather than assumed. A filter that stopped matching — a suite
// renumbered, a json field renamed so every string decodes empty — turns a known answer
// test into a loop that runs zero times and reports PASS, which is the one outcome a known
// answer test must not be able to reach.
const (
	keyScheduleKatFile              = "key-schedule.json"
	keyScheduleKatJoinerComparisons = 10
)

// keyScheduleKatVectors returns the published key schedule entries, having first checked
// that the file on disk is the blob mlswg published at the commit interop/PINS.md pins.
//
// Which file and which entries, written down rather than left to be reconstructed: this is
// testdata/vectors/key-schedule.json from mlswg/mls-implementations test-vectors at the
// commit mlswgVectorUpstreamCommit names, and what is read from it are the entries whose
// cipher_suite this package registers, all five epochs of each, fields initial_init_secret,
// commit_secret, group_context, joiner_secret and init_secret.
//
// The provenance is verified here rather than left to the upstream anchor test. That test
// failing does not stop this one running, and a known answer test that compares against a
// file it did not authenticate is a known answer test that an edit to the file can make
// agree with anything. VECTORS.sha256 would not close it either: it is a digest of the
// local bytes, so re-recording one line of it makes a rewritten corpus verify. The digest
// used here is the one read out of upstream's git object store.
func keyScheduleKatVectors(t *testing.T) []labelKatSchedule {
	t.Helper()
	want, anchored := mlswgVectorUpstreamSha256[keyScheduleKatFile]
	if !anchored {
		t.Fatalf("%s carries no upstream digest, so the answers below would be compared against an unauthenticated file",
			keyScheduleKatFile)
	}
	raw, err := os.ReadFile(filepath.Join(mlswgVectorDirectory, keyScheduleKatFile))
	if err != nil {
		t.Fatalf("read %s: %v", keyScheduleKatFile, err)
	}
	digest := sha256.Sum256(normalisedLineEndings(raw))
	if got := hex.EncodeToString(digest[:]); got != want {
		t.Fatalf("%s hashes to %s with its line endings normalised; %s/%s at %s published %s. These are not the answers mlswg published, so nothing below is a known answer test",
			keyScheduleKatFile, got, mlswgVectorUpstreamRepository, mlswgVectorUpstreamDirectory,
			mlswgVectorUpstreamCommit, want)
	}
	vectors := []labelKatSchedule{}
	loadLabelKat(t, keyScheduleKatFile, &vectors)
	if len(vectors) == 0 {
		t.Fatalf("%s parsed to no entries", keyScheduleKatFile)
	}
	return vectors
}

// TestDeriveJoinerSecretMatchesTheMlswgKeySchedule is the known answer test guardrail 1
// exists for, and it is the deliverable of this task.
//
// joiner_secret is Extract(init_secret_[n-1], commit_secret) expanded under "joiner". Both
// Extract arguments are KDF.Nh secrets, so transposing them compiles, returns 32 bytes, and
// produces a value that is deterministic, that differs for differing inputs, that round
// trips and that satisfies every self consistency check this package could write. Nothing
// separates the two except a value neither side of the swap invented.
//
// Three things stop the comparison passing vacuously. The corpus is authenticated against
// upstream before it is read. The number of comparisons is counted and asserted, and the
// suites the loop matched are compared against the registry. And each epoch's two Extract
// inputs are required to differ, because an epoch whose init secret happened to equal its
// commit secret would agree with a swapped implementation and pin nothing.
//
// The group context goes in as the decoded structure, which is what DeriveJoinerSecret's
// signature takes, so this walks the section 8.1 codec as well. The re-encoding is asserted
// against the published bytes first: a codec that disagreed would otherwise be reported
// here as a key schedule failure, which is the wrong sentence about the wrong file.
func TestDeriveJoinerSecretMatchesTheMlswgKeySchedule(t *testing.T) {
	compared := 0
	matched := []CipherSuite{}
	for _, vector := range keyScheduleKatVectors(t) {
		suite := CipherSuite(vector.CipherSuite)
		if !IsRegisteredSuite(suite) {
			continue
		}
		matched = append(matched, suite)
		crypto := mustProvider(t, suite)
		previousInit := mustDecodeHex(t, "initial_init_secret", vector.InitialInitSecret)
		for index, epoch := range vector.Epochs {
			at := fmt.Sprintf(" suite %#04x epoch %d", uint16(suite), index)
			commitSecret := mustDecodeHex(t, "commit_secret"+at, epoch.CommitSecret)
			if bytes.Equal(previousInit, commitSecret) {
				t.Errorf("%s: the init secret and the commit secret are the same bytes, so this epoch agrees with a swapped Extract and pins nothing about the argument order",
					at)
			}
			publishedContext := mustDecodeHex(t, "group_context"+at, epoch.GroupContext)
			groupContext := &GroupContext{}
			if err := syntax.Unmarshal(publishedContext, groupContext); err != nil {
				t.Fatalf("%s: decode the published group context: %v", at, err)
			}
			reEncoded, err := syntax.Marshal(groupContext)
			if err != nil {
				t.Fatalf("%s: re-encode the decoded group context: %v", at, err)
			}
			if !bytes.Equal(reEncoded, publishedContext) {
				t.Fatalf("%s: the group context codec re-encodes to %x and the corpus published %x, so a joiner mismatch below would be the codec and not the key schedule",
					at, reEncoded, publishedContext)
			}
			joiner, err := DeriveJoinerSecret(crypto, previousInit, commitSecret, groupContext)
			if err != nil {
				t.Fatalf("%s: DeriveJoinerSecret: %v", at, err)
			}
			assertLabelKat(t, "joiner_secret"+at, joiner, epoch.JoinerSecret)
			compared++
			previousInit = mustDecodeHex(t, "init_secret"+at, epoch.InitSecret)
		}
	}
	if compared != keyScheduleKatJoinerComparisons {
		t.Fatalf("compared %d published joiner secrets, want %d; the loop matched %v",
			compared, keyScheduleKatJoinerComparisons, matched)
	}
	if got := slices.Sorted(slices.Values(matched)); !slices.Equal(got, Suites()) {
		t.Fatalf("the corpus answered for %v and this package registers %v", got, Suites())
	}
}

// independentJoinerSecret is RFC 9420's joiner derivation written out from the two RFCs it
// is made of, using crypto/hmac and nothing this package computes.
//
// It is here because the corpus and the implementation could in principle agree for a
// reason other than being right: a vendored file is bytes on disk, and the whole argument
// that they are mlswg's bytes is a digest comparison against a commit this repository only
// records the name of. This is a second statement of the same answer that a reader can
// check line by line without fetching anything:
//
//	RFC 5869 section 2.2      HKDF-Extract(salt, IKM) = HMAC-Hash(key = salt, data = IKM)
//	RFC 9420 section 8        the salt is init_secret_[n-1]; the IKM is commit_secret
//	RFC 9420 section 8        joiner = ExpandWithLabel(prk, "joiner", GroupContext, KDF.Nh)
//	RFC 9420 section 5.1      ExpandWithLabel expands under the serialization of
//	                          struct { uint16 length; opaque label<V>; opaque context<V> },
//	                          the label carrying the "MLS 1.0 " prefix
//	RFC 9420 section 2.1.2    opaque<V> takes a variable length integer prefix: one octet
//	                          with prefix bits 0b00 below 64, two with 0b01 up to 16383
//	RFC 5869 section 2.3      HKDF-Expand's first block is HMAC(prk, info || 0x01), and 32
//	                          octets is exactly that first block under sha256
//
// so for the 14 octet label "MLS 1.0 joiner" and a 112 octet group context the info is
//
//	00 20      the requested output length, 32, big endian uint16
//	0e         label<V> byte length 14; 14 < 64 so one octet, prefix bits 0b00
//	4d 4c ..   the 14 label octets, "MLS 1.0 joiner"
//	40 70      context<V> byte length 112; 112 > 63 so two octets: 0x40|(112>>8), 112&0xff
//	00 01 ..   the 112 group context octets
//
// Every part of that is a hazard with no other witness in this package. A single octet
// length on a 112 byte context, a label without the "MLS 1.0 " prefix, a length field
// holding the label's size instead of the output's, or the two opaque fields transposed all
// produce a perfectly well formed 32 byte secret.
func independentJoinerSecret(t *testing.T, initSecretPrev []byte, commitSecret []byte, groupContext []byte) []byte {
	t.Helper()
	// HKDF-Extract: the salt is the hmac key and the input keying material is the message
	extract := hmac.New(sha256.New, initSecretPrev)
	extract.Write(commitSecret)
	prk := extract.Sum(nil)

	label := []byte("MLS 1.0 joiner")
	if len(label) >= 64 {
		t.Fatalf("the label is %d octets, so the one octet varint written below is the wrong encoding", len(label))
	}
	if len(groupContext) < 64 || len(groupContext) > 16383 {
		t.Fatalf("the group context is %d octets, so the two octet varint written below is the wrong encoding",
			len(groupContext))
	}
	info := []byte{}
	info = append(info, byte(sha256.Size>>8), byte(sha256.Size))
	info = append(info, byte(len(label)))
	info = append(info, label...)
	info = append(info, 0x40|byte(len(groupContext)>>8), byte(len(groupContext)))
	info = append(info, groupContext...)

	// HKDF-Expand, first and only block: 32 octets is one sha256 output
	expand := hmac.New(sha256.New, prk)
	expand.Write(info)
	expand.Write([]byte{0x01})
	return expand.Sum(nil)
}

// TestDeriveJoinerSecretMatchesAnIndependentDerivation runs that derivation against every
// published epoch and requires all three — this package, the corpus, and the hand written
// HKDF — to agree.
//
// The swapped call is checked first on the hand written side. A derivation that gave the
// same answer with its two Extract arguments transposed could not see the defect this whole
// task exists for, and would agree with a swapped implementation while looking like a
// second opinion.
func TestDeriveJoinerSecretMatchesAnIndependentDerivation(t *testing.T) {
	compared := 0
	for _, vector := range keyScheduleKatVectors(t) {
		suite := CipherSuite(vector.CipherSuite)
		if !IsRegisteredSuite(suite) {
			continue
		}
		crypto := mustProvider(t, suite)
		previousInit := mustDecodeHex(t, "initial_init_secret", vector.InitialInitSecret)
		for index, epoch := range vector.Epochs {
			at := fmt.Sprintf(" suite %#04x epoch %d", uint16(suite), index)
			commitSecret := mustDecodeHex(t, "commit_secret"+at, epoch.CommitSecret)
			publishedContext := mustDecodeHex(t, "group_context"+at, epoch.GroupContext)
			groupContext := &GroupContext{}
			if err := syntax.Unmarshal(publishedContext, groupContext); err != nil {
				t.Fatalf("%s: decode the published group context: %v", at, err)
			}
			want := independentJoinerSecret(t, previousInit, commitSecret, publishedContext)
			if swapped := independentJoinerSecret(t, commitSecret, previousInit, publishedContext); bytes.Equal(swapped, want) {
				t.Fatalf("%s: the hand written derivation gives one answer for both argument orders, so it cannot see a transposition",
					at)
			}
			got, err := DeriveJoinerSecret(crypto, previousInit, commitSecret, groupContext)
			if err != nil {
				t.Fatalf("%s: DeriveJoinerSecret: %v", at, err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s: DeriveJoinerSecret = %x and the hand written HKDF gives %x", at, got, want)
			}
			assertLabelKat(t, "the hand written HKDF against the published joiner_secret"+at, want, epoch.JoinerSecret)
			compared++
			previousInit = mustDecodeHex(t, "init_secret"+at, epoch.InitSecret)
		}
	}
	if compared != keyScheduleKatJoinerComparisons {
		t.Fatalf("compared %d epochs, want %d", compared, keyScheduleKatJoinerComparisons)
	}
}

// TestDeriveJoinerSecretRefusesSecretsThatAreNotKdfNh sweeps lengths on both arguments
// rather than testing one short case.
//
// HKDF-Extract accepts every length of either argument, so a truncated init secret produces
// a perfectly well formed pseudorandom key and an epoch nobody else derives; the mistake
// then surfaces epochs later as an undecryptable message. The nil result is asserted
// alongside the error, since a caller reading the slice rather than the error would
// otherwise take a full length secret out of a refused call.
//
// The accepting control runs first, so the refusals below are attributable to the lengths
// rather than to something else about the call.
func TestDeriveJoinerSecretRefusesSecretsThatAreNotKdfNh(t *testing.T) {
	for _, suite := range Suites() {
		crypto := mustProvider(t, suite)
		nh := crypto.HashSize()
		good := bytes.Repeat([]byte{0xa5}, nh)
		other := bytes.Repeat([]byte{0x5a}, nh)
		groupContext := ksVectorEpoch0GroupContext(t)
		if _, err := DeriveJoinerSecret(crypto, good, other, groupContext); err != nil {
			t.Fatalf("suite %#04x: two KDF.Nh secrets were refused: %v", uint16(suite), err)
		}
		for _, length := range []int{0, 1, nh - 1, nh + 1, 2 * nh} {
			wrong := bytes.Repeat([]byte{0xa5}, length)
			for _, testCase := range []struct {
				name   string
				init   []byte
				commit []byte
			}{
				{name: "init secret", init: wrong, commit: other},
				{name: "commit secret", init: good, commit: wrong},
			} {
				secret, err := DeriveJoinerSecret(crypto, testCase.init, testCase.commit, groupContext)
				if !errors.Is(err, ErrSecretLength) {
					t.Errorf("suite %#04x: a %d byte %s gave err = %v, want ErrSecretLength",
						uint16(suite), length, testCase.name, err)
				}
				if secret != nil {
					t.Errorf("suite %#04x: a %d byte %s was refused and %d bytes of secret came back beside the error",
						uint16(suite), length, testCase.name, len(secret))
				}
			}
		}
		// nil is the zero length case arriving with no storage at all, which a length
		// check written as a nil check would answer differently
		for _, testCase := range []struct {
			name   string
			init   []byte
			commit []byte
		}{
			{name: "init secret", init: nil, commit: other},
			{name: "commit secret", init: good, commit: nil},
		} {
			if _, err := DeriveJoinerSecret(crypto, testCase.init, testCase.commit, groupContext); !errors.Is(err, ErrSecretLength) {
				t.Errorf("suite %#04x: a nil %s gave err = %v, want ErrSecretLength",
					uint16(suite), testCase.name, err)
			}
		}
	}
}

// mutatedGroupContexts returns one copy of the context per field of GroupContext, each with
// exactly that field changed, keyed by the field's name.
//
// The fields are read off the type rather than listed. This project has been walked past a
// hand written list of a class fourteen times, and here the consequence is specific: a
// field added to GroupContext by a later plan and left out of a list is a field nothing
// requires to be in the preimage, which is two members deriving different secrets from
// contexts they both consider equal.
//
// An unhandled kind is fatal rather than skipped, because a skipped field is a field this
// gate stops judging in silence.
func mutatedGroupContexts(t *testing.T, base *GroupContext) map[string]*GroupContext {
	t.Helper()
	contextType := reflect.TypeOf(GroupContext{})
	if contextType.NumField() == 0 {
		t.Fatal("GroupContext declares no field, so this gate would compare nothing")
	}
	extensionType := reflect.TypeOf(Extension{})
	mutated := map[string]*GroupContext{}
	for i := range contextType.NumField() {
		name := contextType.Field(i).Name
		clone := base.Clone()
		target := reflect.ValueOf(clone).Elem().Field(i)
		switch {
		case target.CanUint():
			target.SetUint(target.Uint() + 1)
		case target.Kind() == reflect.Slice && target.Type().Elem().Kind() == reflect.Uint8:
			target.Set(reflect.AppendSlice(target, reflect.ValueOf([]byte{0x7f})))
		case target.Kind() == reflect.Slice && target.Type().Elem() == extensionType:
			target.Set(reflect.Append(target, reflect.ValueOf(Extension{
				ExtensionType: ExtensionType(0xf00d),
				ExtensionData: []byte{0x01},
			})))
		default:
			t.Fatalf("GroupContext.%s is a %s and this gate does not know how to change one; teach it rather than letting the field go unjudged",
				name, target.Type())
		}
		mutated[name] = clone
	}
	return mutated
}

// TestDeriveJoinerSecretReadsEveryFieldOfTheGroupContext walks what the published corpus
// cannot.
//
// Every mlswg key schedule vector carries one protocol version, one cipher suite, one group
// id per entry and an empty extensions vector, so four of GroupContext's seven fields never
// move anywhere in the whole corpus. An implementation that dropped the version, the cipher
// suite, the group id or the extensions out of the preimage matches every published joiner
// secret there is, and splits from any peer whose group differs from the vector's in one of
// them.
//
// The two secrets and the argument order are checked here too. The transposition assertion
// is necessary and not sufficient — it says the two arguments are not interchangeable, and
// says nothing about which way round is right. The known answer tests above are what say
// that; this one is what fails first and most legibly when they cannot run.
func TestDeriveJoinerSecretReadsEveryFieldOfTheGroupContext(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	base := ksVectorEpoch0GroupContext(t)
	initSecret := bytes.Repeat([]byte{0xa5}, crypto.HashSize())
	commitSecret := bytes.Repeat([]byte{0x5a}, crypto.HashSize())
	want, err := DeriveJoinerSecret(crypto, initSecret, commitSecret, base)
	if err != nil {
		t.Fatalf("DeriveJoinerSecret over the base context: %v", err)
	}

	cases := mutatedGroupContexts(t, base)
	if fields := reflect.TypeOf(GroupContext{}).NumField(); len(cases) != fields {
		t.Fatalf("GroupContext declares %d fields and this gate built %d cases", fields, len(cases))
	}
	for _, name := range slices.Sorted(maps.Keys(cases)) {
		got, err := DeriveJoinerSecret(crypto, initSecret, commitSecret, cases[name])
		if err != nil {
			t.Errorf("GroupContext.%s changed: DeriveJoinerSecret: %v", name, err)
			continue
		}
		if bytes.Equal(got, want) {
			t.Errorf("changing GroupContext.%s left the joiner secret unchanged, so that field is not in the preimage the epoch is bound to",
				name)
		}
	}

	changedInit := bytes.Clone(initSecret)
	changedInit[0] ^= 0x01
	changedCommit := bytes.Clone(commitSecret)
	changedCommit[len(changedCommit)-1] ^= 0x01
	for _, testCase := range []struct {
		what   string
		init   []byte
		commit []byte
	}{
		{what: "the init secret", init: changedInit, commit: commitSecret},
		{what: "the commit secret", init: initSecret, commit: changedCommit},
		{what: "the two Extract arguments transposed", init: commitSecret, commit: initSecret},
	} {
		got, err := DeriveJoinerSecret(crypto, testCase.init, testCase.commit, base)
		if err != nil {
			t.Errorf("%s: DeriveJoinerSecret: %v", testCase.what, err)
			continue
		}
		if bytes.Equal(got, want) {
			t.Errorf("%s left the joiner secret unchanged", testCase.what)
		}
	}
}

// TestDeriveJoinerSecretLeavesTheCallersInputsAlone. The function erases the pseudorandom
// key it built, which is right, and the three things it was handed belong to the caller,
// which means it must not.
//
// init_secret_[n-1] in particular is still needed if the commit that produced this joiner
// secret turns out to be invalid: a caller whose init secret came back as KDF.Nh zero bytes
// would derive the next epoch from them, and every derivation downstream would look fine.
// The aliasing half is the same hazard arriving later — a joiner secret that is a window
// onto one of its inputs is one caller's zeroize away from erasing a secret it does not own.
func TestDeriveJoinerSecretLeavesTheCallersInputsAlone(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	initSecret := bytes.Repeat([]byte{0xa5}, crypto.HashSize())
	commitSecret := bytes.Repeat([]byte{0x5a}, crypto.HashSize())
	initBefore := bytes.Clone(initSecret)
	commitBefore := bytes.Clone(commitSecret)
	groupContext := ksVectorEpoch0GroupContext(t)
	contextBefore := groupContext.Clone()

	joiner, err := DeriveJoinerSecret(crypto, initSecret, commitSecret, groupContext)
	if err != nil {
		t.Fatalf("DeriveJoinerSecret: %v", err)
	}
	if !bytes.Equal(initSecret, initBefore) {
		t.Errorf("the caller's init secret is %x after the call and was %x", initSecret, initBefore)
	}
	if !bytes.Equal(commitSecret, commitBefore) {
		t.Errorf("the caller's commit secret is %x after the call and was %x", commitSecret, commitBefore)
	}
	if !reflect.DeepEqual(groupContext, contextBefore) {
		t.Errorf("the group context handed in came back as %+v, having been %+v", groupContext, contextBefore)
	}
	if len(joiner) != crypto.HashSize() {
		t.Fatalf("the joiner secret is %d bytes, want %d", len(joiner), crypto.HashSize())
	}
	// snapshotted after the call rather than compared against initBefore, so this
	// reports aliasing and not an erase the two checks above have already named
	initAfter := bytes.Clone(initSecret)
	commitAfter := bytes.Clone(commitSecret)
	for i := range joiner {
		joiner[i] ^= 0xff
	}
	if !bytes.Equal(initSecret, initAfter) || !bytes.Equal(commitSecret, commitAfter) {
		t.Errorf("writing through the joiner secret changed one of the secrets it was derived from, so it is a window onto storage the caller owns")
	}
}

// extractCapturingProvider keeps the pseudorandom key DeriveJoinerSecret builds, so a test
// can read that storage back after the call has returned.
//
// The slice is returned unchanged rather than copied, and that is the whole mechanism this
// observes: zeroizeSecret writes through the backing array its argument points at, so a
// wrapper that handed the caller a clone would be handing it a different array the erase
// never reaches, and the property would read as absent however the production code behaved.
// The count is kept too, because "the key was erased" means nothing if no key was made.
type extractCapturingProvider struct {
	CryptoProvider
	extracted [][]byte
}

func (self *extractCapturingProvider) Extract(salt []byte, ikm []byte) []byte {
	prk := self.CryptoProvider.Extract(salt, ikm)
	self.extracted = append(self.extracted, prk)
	return prk
}

// TestDeriveJoinerSecretErasesThePseudorandomKey observes the sentence key_schedule.go
// writes above DeriveJoinerSecret: "The pseudorandom key is erased before returning."
//
// Nothing observed it before. Deleting the zeroizeSecret call failed exactly one assertion
// in the whole package -- TestNoStubShapesRemainInSource reporting that zeroizeSecret then
// has no caller -- which is a statement about the call graph and not about the key. That
// catch expires on the next production caller of the helper, and the plan lands six of
// them; this one does not expire, because what it reads is the storage.
//
// The control matters as much as the assertion. A run of Nh zero bytes is what a stub
// answers, so "every byte is zero afterwards" is only evidence if the same Extract over the
// same inputs produces something that is not already zero, and that is asserted first.
func TestDeriveJoinerSecretErasesThePseudorandomKey(t *testing.T) {
	for _, suite := range Suites() {
		inner := mustProvider(t, suite)
		nh := inner.HashSize()
		initSecret := bytes.Repeat([]byte{0xa5}, nh)
		commitSecret := bytes.Repeat([]byte{0x5a}, nh)
		groupContext := ksVectorEpoch0GroupContext(t)

		// the control: the key this call will erase is not zero to begin with
		fresh := inner.Extract(initSecret, commitSecret)
		if !slices.ContainsFunc(fresh, func(b byte) bool { return b != 0 }) {
			t.Fatalf("suite %#04x: Extract over these inputs is already %d zero bytes, so an all zero reading below would say nothing",
				uint16(suite), len(fresh))
		}

		crypto := &extractCapturingProvider{CryptoProvider: inner}
		joiner, err := DeriveJoinerSecret(crypto, initSecret, commitSecret, groupContext)
		if err != nil {
			t.Fatalf("suite %#04x: DeriveJoinerSecret: %v", uint16(suite), err)
		}
		if len(crypto.extracted) != 1 {
			t.Fatalf("suite %#04x: Extract was called %d times, want 1; this gate reads the key that one call returned",
				uint16(suite), len(crypto.extracted))
		}
		prk := crypto.extracted[0]
		if len(prk) != nh {
			t.Fatalf("suite %#04x: the pseudorandom key is %d bytes, want %d", uint16(suite), len(prk), nh)
		}
		for i, b := range prk {
			if b != 0 {
				t.Errorf("suite %#04x: byte %d of the pseudorandom key is %#02x after the call, want 0; it is one HKDF-Expand away from every key of the epoch and nothing downstream needs it",
					uint16(suite), i, b)
				break
			}
		}
		// and the erase reached the key rather than the answer: a joiner secret that came
		// back zero would satisfy the loop above for the wrong reason
		if len(joiner) != nh {
			t.Fatalf("suite %#04x: the joiner secret is %d bytes, want %d", uint16(suite), len(joiner), nh)
		}
		if !slices.ContainsFunc(joiner, func(b byte) bool { return b != 0 }) {
			t.Errorf("suite %#04x: the joiner secret came back as %d zero bytes, so the erase reached the value that was returned",
				uint16(suite), len(joiner))
		}
	}
}

// wideKdfHashSize is a KDF.Nh neither registered suite has.
const wideKdfHashSize = sha512.Size384

// wideKdfProvider is a provider whose KDF.Nh is not 32.
//
// Both registered suites fix Nh at 32, so nothing already in this tree separates a body
// that reads KDF.Nh off the provider it was handed from one that writes 32 down. Measured,
// not supposed: nh := crypto.HashSize() replaced by nh := 32 in DeriveJoinerSecret passed
// every test of this package. This is the input that separates them, and it is the same
// limit the ZeroSecret note in crypto_test.go records.
//
// It is a real kdf rather than a fake answering the right number of bytes: HKDF-SHA384,
// whose Nh is 48, expanding over the same mlsKdfLabel preimage the production provider
// builds. Only the three methods DeriveJoinerSecret reaches are overridden; everything else
// is the registered suite's, so a derivation that grew a fourth call would get a working
// method rather than a nil dereference that reads as this gate's own bug.
//
// The requested expansion lengths are recorded because KDF.Nh governs two separate things
// here -- which lengths are refused, and how many bytes are asked for -- and a body that
// read the provider for the first and wrote 32 for the second would pass a length check
// alone.
type wideKdfProvider struct {
	CryptoProvider
	expandedLengths []int
}

func (self *wideKdfProvider) HashSize() int {
	return wideKdfHashSize
}

// (salt, ikm) in, (ikm, salt) out, which is the swap crypto.go makes and the reason
// guardrail 1 confines the call. Calling the library here is allowed: the confinement gate
// runs over productionSources, which drops every _test.go file.
func (self *wideKdfProvider) Extract(salt []byte, ikm []byte) []byte {
	prk, err := hkdf.Extract(sha512.New384, ikm, salt)
	if err != nil {
		panic("mls test: hkdf-sha384 extract: " + err.Error())
	}
	return prk
}

func (self *wideKdfProvider) ExpandWithLabel(secret []byte, label string, context []byte, length int) []byte {
	self.expandedLengths = append(self.expandedLengths, length)
	out, err := hkdf.Expand(sha512.New384, secret, string(mlsKdfLabel(label, context, length)), length)
	if err != nil {
		panic("mls test: hkdf-sha384 expand: " + err.Error())
	}
	return out
}

// The rest of the hash and kdf surface, at the same width.
//
// A provider that answered KDF.Nh 48 and went on hashing at 32 would be incoherent, and the
// differential gate below reads output LENGTHS: over such a provider RefHash would answer 32
// bytes while the provider claimed 48, and the gate would report a construction that reads
// its length off the provider as one that writes 32 down. Everything Nh governs therefore
// moves together, and nothing here is a fake answering the right number of bytes -- it is
// SHA-384 and HMAC-SHA384 and HKDF-SHA384, the same primitives one width up.
//
// KeySize and NonceSize are deliberately NOT overridden. Nk and Nn are the aead's and have
// nothing to do with the kdf, and a suite whose hash grew does not thereby get a wider aead
// key; moving them would make this provider incoherent in the other direction.
func (self *wideKdfProvider) Hash(data []byte) []byte {
	digest := sha512.Sum384(data)
	return digest[:]
}

func (self *wideKdfProvider) Mac(key []byte, data []byte) []byte {
	mac := hmac.New(sha512.New384, key)
	mac.Write(data)
	return mac.Sum(nil)
}

func (self *wideKdfProvider) MacVerify(key []byte, data []byte, tag []byte) bool {
	expected := self.Mac(key, data)
	if len(tag) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare(expected, tag) == 1
}

func (self *wideKdfProvider) Expand(prk []byte, info []byte, length int) []byte {
	out, err := hkdf.Expand(sha512.New384, prk, string(info), length)
	if err != nil {
		panic("mls test: hkdf-sha384 expand: " + err.Error())
	}
	return out
}

// DeriveSecret is ExpandWithLabel over no context at KDF.Nh, which is the definition
// RFC 9420 gives it and what crypto_labels.go writes. Routed through this type's own
// ExpandWithLabel so the width follows HashSize rather than being written down twice, and so
// the requested lengths are recorded here as well.
func (self *wideKdfProvider) DeriveSecret(secret []byte, label string) []byte {
	return self.ExpandWithLabel(secret, label, nil, self.HashSize())
}

// TestDeriveJoinerSecretReadsKdfNhFromTheProvider is the input the registered suites cannot
// supply.
//
// KDF.Nh governs three things in this derivation: whether the init secret is the right
// length, whether the commit secret is, and how many bytes the expansion is asked for. A
// literal 32 answers all three correctly for both registered suites and incorrectly for
// every suite whose hash is not sha256, and the failure would arrive as an epoch that looks
// valid on this side and matches nobody.
//
// Both directions are asserted. A provider whose Nh is 48 must accept 48 byte secrets --
// which a hardcoded 32 refuses -- and must refuse 32 byte ones, which a hardcoded 32
// accepts. Either half alone is satisfiable by a different mistake.
func TestDeriveJoinerSecretReadsKdfNhFromTheProvider(t *testing.T) {
	inner := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	if inner.HashSize() == wideKdfHashSize {
		t.Fatalf("the registered suite's KDF.Nh is already %d, so this gate separates nothing", wideKdfHashSize)
	}
	crypto := &wideKdfProvider{CryptoProvider: inner}
	if crypto.HashSize() != wideKdfHashSize {
		t.Fatalf("the wide provider answers KDF.Nh %d, want %d", crypto.HashSize(), wideKdfHashSize)
	}
	groupContext := ksVectorEpoch0GroupContext(t)

	wideInit := bytes.Repeat([]byte{0xa5}, wideKdfHashSize)
	wideCommit := bytes.Repeat([]byte{0x5a}, wideKdfHashSize)
	joiner, err := DeriveJoinerSecret(crypto, wideInit, wideCommit, groupContext)
	if err != nil {
		t.Fatalf("two %d byte secrets over a provider whose KDF.Nh is %d were refused: %v",
			wideKdfHashSize, wideKdfHashSize, err)
	}
	if len(joiner) != wideKdfHashSize {
		t.Errorf("the joiner secret is %d bytes and the provider's KDF.Nh is %d", len(joiner), wideKdfHashSize)
	}
	if !slices.Equal(crypto.expandedLengths, []int{wideKdfHashSize}) {
		t.Errorf("the expansion was asked for %v bytes, want [%d]; the output length is KDF.Nh as much as the refusals are",
			crypto.expandedLengths, wideKdfHashSize)
	}

	// the other half: 32 bytes is the wrong length for THIS provider, and a hardcoded 32
	// is exactly what takes it
	narrowInit := bytes.Repeat([]byte{0xa5}, inner.HashSize())
	narrowCommit := bytes.Repeat([]byte{0x5a}, inner.HashSize())
	for _, testCase := range []struct {
		name   string
		init   []byte
		commit []byte
	}{
		{name: "init secret", init: narrowInit, commit: wideCommit},
		{name: "commit secret", init: wideInit, commit: narrowCommit},
	} {
		secret, err := DeriveJoinerSecret(crypto, testCase.init, testCase.commit, groupContext)
		if !errors.Is(err, ErrSecretLength) {
			t.Errorf("a %d byte %s over a provider whose KDF.Nh is %d gave err = %v, want ErrSecretLength",
				inner.HashSize(), testCase.name, wideKdfHashSize, err)
		}
		if secret != nil {
			t.Errorf("a %d byte %s was refused and %d bytes of secret came back beside the error",
				inner.HashSize(), testCase.name, len(secret))
		}
	}
}

// TestZeroSecretReadsKdfNhFromTheProvider closes, for ZeroSecret, the limit the registry
// note in crypto_test.go records against it.
//
// ZeroSecret's whole contract is KDF.Nh zero bytes, and both registered suites fix Nh at
// 32, so make([]byte, 32) answers correctly for every input already in this tree. The
// perturbation gate cannot separate the two either: the one argument ZeroSecret takes is a
// provider, and the tagging provider passes a length through unchanged, which is exactly
// why that gate routes ZeroSecret to a registry comparison rather than to a moved input. A
// provider whose Nh is not 32 is the input that separates them, and it is the same one
// DeriveJoinerSecret is held to below.
func TestZeroSecretReadsKdfNhFromTheProvider(t *testing.T) {
	inner := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	if inner.HashSize() == wideKdfHashSize {
		t.Fatalf("the registered suite's KDF.Nh is already %d, so this gate separates nothing", wideKdfHashSize)
	}
	zero := ZeroSecret(&wideKdfProvider{CryptoProvider: inner})
	if len(zero) != wideKdfHashSize {
		t.Errorf("ZeroSecret over a provider whose KDF.Nh is %d answered %d bytes", wideKdfHashSize, len(zero))
	}
	if slices.ContainsFunc(zero, func(b byte) bool { return b != 0 }) {
		t.Errorf("ZeroSecret answered %x, which is not the all zero secret", zero)
	}
}

// joinerSecretRecovering calls DeriveJoinerSecret with a panic caught rather than taken.
//
// A refusal that is not a refusal is exactly what the gate below is looking for, and a test
// binary that died on it would report nothing about the assertions after it -- and would
// report a nil pointer dereference raised inside the syntax package, which names neither
// this function nor the argument that was missing.
func joinerSecretRecovering(
	crypto CryptoProvider,
	initSecretPrev []byte,
	commitSecret []byte,
	groupContext *GroupContext,
) (secret []byte, err error, recovered any) {
	defer func() { recovered = recover() }()
	secret, err = DeriveJoinerSecret(crypto, initSecretPrev, commitSecret, groupContext)
	return secret, err, nil
}

// TestDeriveJoinerSecretRefusesANilGroupContext pins the missing argument as a typed error.
//
// syntax.Marshal is handed a non nil interface holding a nil pointer, so MarshalMLS
// dereferences it and the caller gets a runtime panic out of the syntax package. Every
// caller of this derivation takes its context off a struct field, so an unset field is how
// a nil arrives, and the epoch that field belongs to is the thing a reader needs named.
//
// The accepting control runs first, so the refusals below are attributable to the nil
// rather than to something else about the call.
func TestDeriveJoinerSecretRefusesANilGroupContext(t *testing.T) {
	for _, suite := range Suites() {
		crypto := mustProvider(t, suite)
		nh := crypto.HashSize()
		initSecret := bytes.Repeat([]byte{0xa5}, nh)
		commitSecret := bytes.Repeat([]byte{0x5a}, nh)
		if _, err := DeriveJoinerSecret(crypto, initSecret, commitSecret, ksVectorEpoch0GroupContext(t)); err != nil {
			t.Fatalf("suite %#04x: a real group context was refused: %v", uint16(suite), err)
		}
		// the literal and a nil valued variable of the type are the same argument, and
		// the second is how an unset struct field arrives
		var unset *GroupContext
		for _, testCase := range []struct {
			name    string
			context *GroupContext
		}{
			{name: "a nil literal", context: nil},
			{name: "an unset field", context: unset},
		} {
			secret, err, recovered := joinerSecretRecovering(crypto, initSecret, commitSecret, testCase.context)
			if recovered != nil {
				t.Errorf("suite %#04x: %s panicked with %v rather than being refused",
					uint16(suite), testCase.name, recovered)
				continue
			}
			if !errors.Is(err, ErrNilGroupContext) {
				t.Errorf("suite %#04x: %s gave err = %v, want ErrNilGroupContext", uint16(suite), testCase.name, err)
			}
			if secret != nil {
				t.Errorf("suite %#04x: %s was refused and %d bytes of secret came back beside the error",
					uint16(suite), testCase.name, len(secret))
			}
		}
	}
}

// ---------------------------------------------------------------------------
// the epoch key schedule
//
// Nine secrets come out of one epoch_secret. They are all KDF.Nh bytes, they are all
// indistinguishable from random, and the only thing that separates any two of them is the
// string handed to DeriveSecret. So a test that reads a length, a non zero-ness, a
// determinism or a round trip passes for all nine no matter which label produced which,
// and two labels swapped between two fields is a change no such test can see.
//
// What sees it is a known answer nothing in this package computed, which is the corpus the
// sweeps below run over: every epoch of every registered suite, driven through the
// KeySchedule type rather than through the primitives — crypto_labels_test.go already holds
// the primitives to the same file, and a schedule that agreed with the primitives while
// wiring them together wrongly would pass that and fail every peer.
//
// The set of nine is read off EpochSecrets by reflection everywhere below rather than
// written out as a list. A list is the shape this repository has paid for fourteen times:
// the tenth secret, or the field renamed, drops out of the class and every sweep goes on
// reporting the clean run a complete sweep reports.
// ---------------------------------------------------------------------------

// keyScheduleKatEpochs is how many epochs of the published corpus this package's registered
// suites account for, and keyScheduleKatEpochComparisons is how many published answers that
// is: joiner_secret, welcome_secret and the nine derived secrets, per epoch.
//
// Both are asserted rather than assumed, for the reason keyScheduleKatJoinerComparisons is.
// A filter that stopped matching — a suite renumbered, a json field renamed so every string
// decodes empty — turns every sweep below into a loop that runs zero times and reports
// PASS, which is the one outcome a known answer test must not be able to reach.
const (
	keyScheduleKatEpochs           = 10
	keyScheduleKatEpochComparisons = 110
)

// One epoch of the authenticated corpus, with the inputs the KeySchedule constructors take
// already decoded and the previous epoch's init secret already chained in.
//
// The published epoch is carried alongside so a sweep can reach whichever answers it needs
// without re-deriving the chain, and the suite and the position travel with it because a
// failure that cannot say which epoch it came from sends a reader to the wrong file.
type ksVectorEpoch struct {
	suite        CipherSuite
	crypto       CryptoProvider
	at           string
	groupContext *GroupContext
	initPrev     []byte
	commitSecret []byte
	pskSecret    []byte
	published    labelKatEpoch
}

// ksVectorEpochs is every epoch of every suite this package registers, taken from the
// corpus keyScheduleKatVectors has already authenticated against upstream's git object
// store.
//
// Three things here stop the sweeps that read this from passing vacuously, and each is a
// property of the corpus rather than of the code under test:
//
//   - the number of epochs is counted and asserted, and the suites that matched are
//     compared against the registry, so a filter that stopped matching is loud;
//   - the group context is re-encoded and compared against the published bytes before it is
//     handed on, so a codec disagreement is reported as a codec disagreement rather than as
//     a key schedule failure in the wrong file;
//   - each of the two Extract calls in the section 8 chain has its two arguments compared,
//     and an epoch whose arguments happened to be equal is reported. Extract(a, b) and
//     Extract(b, a) agree exactly when a == b, so such an epoch pins nothing about the
//     argument order — which is guardrail 1, and the one mistake in this file that a
//     self consistent implementation reproduces perfectly.
func ksVectorEpochs(t *testing.T) []ksVectorEpoch {
	t.Helper()
	epochs := []ksVectorEpoch{}
	matched := []CipherSuite{}
	for _, vector := range keyScheduleKatVectors(t) {
		suite := CipherSuite(vector.CipherSuite)
		if !IsRegisteredSuite(suite) {
			continue
		}
		matched = append(matched, suite)
		crypto := mustProvider(t, suite)
		previousInit := mustDecodeHex(t, "initial_init_secret", vector.InitialInitSecret)
		for index, published := range vector.Epochs {
			at := fmt.Sprintf(" suite %#04x epoch %d", uint16(suite), index)
			commitSecret := mustDecodeHex(t, "commit_secret"+at, published.CommitSecret)
			pskSecret := mustDecodeHex(t, "psk_secret"+at, published.PskSecret)
			publishedJoiner := mustDecodeHex(t, "joiner_secret"+at, published.JoinerSecret)
			if bytes.Equal(previousInit, commitSecret) {
				t.Errorf("%s: init_secret_[n-1] and commit_secret are the same bytes, so this epoch agrees with a transposed Extract in the joiner step and pins nothing about the argument order",
					at)
			}
			if bytes.Equal(publishedJoiner, pskSecret) {
				t.Errorf("%s: joiner_secret and psk_secret are the same bytes, so this epoch agrees with a transposed Extract in the member step and pins nothing about the argument order",
					at)
			}
			publishedContext := mustDecodeHex(t, "group_context"+at, published.GroupContext)
			groupContext := &GroupContext{}
			if err := syntax.Unmarshal(publishedContext, groupContext); err != nil {
				t.Fatalf("%s: decode the published group context: %v", at, err)
			}
			reEncoded, err := syntax.Marshal(groupContext)
			if err != nil {
				t.Fatalf("%s: re-encode the decoded group context: %v", at, err)
			}
			if !bytes.Equal(reEncoded, publishedContext) {
				t.Fatalf("%s: the group context codec re-encodes to %x and the corpus published %x, so a secret mismatch below would be the codec and not the key schedule",
					at, reEncoded, publishedContext)
			}
			epochs = append(epochs, ksVectorEpoch{
				suite:        suite,
				crypto:       crypto,
				at:           at,
				groupContext: groupContext,
				initPrev:     previousInit,
				commitSecret: commitSecret,
				pskSecret:    pskSecret,
				published:    published,
			})
			previousInit = mustDecodeHex(t, "init_secret"+at, published.InitSecret)
		}
	}
	if len(epochs) != keyScheduleKatEpochs {
		t.Fatalf("the corpus answered for %d epochs, want %d; the loop matched %v",
			len(epochs), keyScheduleKatEpochs, matched)
	}
	if got := slices.Sorted(slices.Values(matched)); !slices.Equal(got, Suites()) {
		t.Fatalf("the corpus answered for %v and this package registers %v", got, Suites())
	}
	return epochs
}

// schedule builds the committer's key schedule for this epoch.
func (self ksVectorEpoch) schedule(t *testing.T) *KeySchedule {
	t.Helper()
	schedule, err := NewKeySchedule(
		self.crypto, self.initPrev, self.commitSecret, self.pskSecret, self.groupContext)
	if err != nil {
		t.Fatalf("%s: NewKeySchedule: %v", self.at, err)
	}
	return schedule
}

// epochSecretsByField reads EpochSecrets field by field off the struct rather than off a
// list written here, so a tenth secret added to the type joins every sweep that calls this
// instead of quietly falling outside all of them.
//
// The type of each field is checked as it is read. reflect.Value.Bytes panics on a field
// that is not a byte slice, and a panic inside a helper is a worse sentence than a name and
// a type, so the refusal is written out.
func epochSecretsByField(t *testing.T, secrets *EpochSecrets) map[string][]byte {
	t.Helper()
	if secrets == nil {
		t.Fatal("Secrets() answered nil, so every sweep over the nine would run over nothing")
	}
	value := reflect.ValueOf(*secrets)
	byteSlice := reflect.TypeOf([]byte(nil))
	fields := map[string][]byte{}
	for i := range value.NumField() {
		name := value.Type().Field(i).Name
		field := value.Field(i)
		if field.Type() != byteSlice {
			t.Fatalf("EpochSecrets.%s is %s rather than []byte, so the sweeps over the derived secrets do not cover it",
				name, field.Type())
		}
		fields[name] = field.Bytes()
	}
	if len(fields) == 0 {
		t.Fatal("EpochSecrets read as no fields at all, so every sweep below runs over nothing")
	}
	return fields
}

// publishedEpochSecret is the corpus answer for one field of EpochSecrets.
//
// The switch is exhaustive over the type rather than over a list, because the caller drives
// it with the field names reflection read: a field added to EpochSecrets with no published
// answer arrives at the default case and is reported as unpinned, which is exactly the
// state a new secret is in until somebody finds the corpus field that answers it.
func publishedEpochSecret(t *testing.T, field string, epoch labelKatEpoch) string {
	t.Helper()
	switch field {
	case "SenderData":
		return epoch.SenderDataSecret
	case "Encryption":
		return epoch.EncryptionSecret
	case "Exporter":
		return epoch.ExporterSecret
	case "External":
		return epoch.ExternalSecret
	case "Confirmation":
		return epoch.ConfirmationKey
	case "Membership":
		return epoch.MembershipKey
	case "ResumptionPsk":
		return epoch.ResumptionPsk
	case "EpochAuthenticator":
		return epoch.EpochAuthenticator
	case "InitSecret":
		return epoch.InitSecret
	default:
		t.Fatalf("EpochSecrets.%s has no published answer in %s, so this package derives a secret nobody else's implementation pins",
			field, keyScheduleKatFile)
		return ""
	}
}

// TestKeyScheduleMatchesTheMlswgKeySchedule is the deliverable of this task: every secret
// RFC 9420 section 8 derives for an epoch, compared against the answer mlswg published for
// that epoch, through the type rather than through the primitives.
//
// This is the only test in this file that can tell the nine apart. Each is 32 pseudorandom
// bytes, so "membership_key holds the value confirm produced" is a state in which every
// length check, every distinctness check, every aliasing check and every round trip in this
// package still passes. Two labels transposed between two fields is a two character edit
// that no peer would ever interoperate with and that nothing else here would notice.
//
// The vacuity controls live in ksVectorEpochs, which authenticates the corpus, counts the
// epochs, compares the matched suites against the registry, re-encodes each group context
// against the published bytes and refuses an epoch whose Extract arguments coincide. What
// is counted here is the answers compared, because a sweep whose inner loop stopped
// producing rows reports the clean run a full one reports.
func TestKeyScheduleMatchesTheMlswgKeySchedule(t *testing.T) {
	compared := 0
	for _, epoch := range ksVectorEpochs(t) {
		schedule := epoch.schedule(t)
		assertLabelKat(t, "joiner_secret"+epoch.at, schedule.JoinerSecret(), epoch.published.JoinerSecret)
		assertLabelKat(t, "welcome_secret"+epoch.at, schedule.WelcomeSecret(), epoch.published.WelcomeSecret)
		compared += 2
		fields := epochSecretsByField(t, schedule.Secrets())
		for _, name := range slices.Sorted(maps.Keys(fields)) {
			assertLabelKat(t, "EpochSecrets."+name+epoch.at,
				fields[name], publishedEpochSecret(t, name, epoch.published))
			compared++
		}
	}
	if compared != keyScheduleKatEpochComparisons {
		t.Fatalf("compared %d published answers, want %d", compared, keyScheduleKatEpochComparisons)
	}
}

// TestKeyScheduleGroupContextBytesAreThePublishedEncoding asserts the schedule expanded over
// the bytes the corpus published, and hands back those same bytes.
//
// Both halves matter and they are different claims. The first is that every key of the
// epoch was derived over the encoding a peer will use. The second is that a caller which
// signs or MACs under GroupContextBytes signs under that same encoding rather than under a
// re-encoding of the struct, which is where a second serialization could drift.
func TestKeyScheduleGroupContextBytesAreThePublishedEncoding(t *testing.T) {
	for _, epoch := range ksVectorEpochs(t) {
		published := mustDecodeHex(t, "group_context"+epoch.at, epoch.published.GroupContext)
		if got := epoch.schedule(t).GroupContextBytes(); !bytes.Equal(got, published) {
			t.Errorf("%s: GroupContextBytes = %x, want %x", epoch.at, got, published)
		}
	}
}

// TestNoTwoEpochSecretsAreEqual sweeps the set reflection read rather than nine hand
// written pair comparisons, which is the difference between a gate and a transcription: a
// tenth secret, or a field renamed, joins this sweep by existing.
//
// A label pasted onto the line below it produces two identical secrets, and every
// individual known answer still passes if both published values came from the same run of a
// reference implementation that made the same mistake. Two derived secrets being equal is
// not a coincidence at 32 bytes; it is a copied label.
func TestNoTwoEpochSecretsAreEqual(t *testing.T) {
	for _, epoch := range ksVectorEpochs(t) {
		fields := epochSecretsByField(t, epoch.schedule(t).Secrets())
		names := slices.Sorted(maps.Keys(fields))
		if len(names) < 2 {
			t.Fatalf("%s: EpochSecrets read as %d fields, so no pair exists to compare", epoch.at, len(names))
		}
		for i, first := range names {
			for _, second := range names[i+1:] {
				if bytes.Equal(fields[first], fields[second]) {
					t.Errorf("%s: EpochSecrets.%s and EpochSecrets.%s are the same %d bytes, which at this length is a copied DeriveSecret label rather than a collision",
						epoch.at, first, second, len(fields[first]))
				}
			}
		}
	}
}

// TestEpochSecretsDoNotAliasEachOther is the storage half of the sweep above: two fields
// that hold the same backing array are equal today and are also a pair where erasing one
// silently erases the other, which is what Zeroize will do to this struct.
//
// Distinct values do not imply distinct storage — a field assigned a subslice of another
// starting at a different offset differs in value and still shares the array — so the
// comparison is on the address of the first element rather than on the bytes.
func TestEpochSecretsDoNotAliasEachOther(t *testing.T) {
	for _, epoch := range ksVectorEpochs(t) {
		fields := epochSecretsByField(t, epoch.schedule(t).Secrets())
		names := slices.Sorted(maps.Keys(fields))
		// every field is checked for storage BEFORE any pair is compared, because the
		// comparison reads the address of a first element and an empty field has none.
		// Written as a guard on the outer name alone it fired too late: a field left empty
		// by a later task was reached as the INNER name first and indexing it took the whole
		// test binary down, which is every other gate of this package reporting nothing
		// instead of one gate reporting the empty field.
		for _, name := range names {
			if len(fields[name]) == 0 {
				t.Fatalf("%s: EpochSecrets.%s is empty, so it has no storage to compare", epoch.at, name)
			}
		}
		for i, first := range names {
			for _, second := range names[i+1:] {
				if &fields[first][0] == &fields[second][0] {
					t.Errorf("%s: EpochSecrets.%s and EpochSecrets.%s start in the same array, so erasing one erases the other",
						epoch.at, first, second)
				}
			}
		}
	}
}

// TestEveryEpochSecretIsKdfNhBytes sweeps the reflected set for the one property a
// truncation breaks and a known answer would report as a value mismatch.
//
// It is separate from the known answer test because it says which mistake was made. A
// secret sliced to the AEAD key length is a well formed key that a peer's AEAD will accept
// the wrong number of bytes of, and "want 32 bytes, got 16" is a sentence a reader can act
// on where "these two hex strings differ" is not.
func TestEveryEpochSecretIsKdfNhBytes(t *testing.T) {
	for _, epoch := range ksVectorEpochs(t) {
		nh := epoch.crypto.HashSize()
		fields := epochSecretsByField(t, epoch.schedule(t).Secrets())
		for _, name := range slices.Sorted(maps.Keys(fields)) {
			if len(fields[name]) != nh {
				t.Errorf("%s: EpochSecrets.%s is %d bytes, want KDF.Nh = %d",
					epoch.at, name, len(fields[name]), nh)
			}
		}
	}
}

// deriveSecretRecordingProvider keeps what every DeriveSecret call was handed and what it
// answered, so a test can read the shape of the derivation off the implementation's own
// behaviour instead of transcribing the nine labels a second time.
//
// A second transcription is what makes a label test vacuous: a test holding its own copy of
// the list agrees with whatever the implementation spells as long as both were edited
// together, which is exactly how a swapped pair survives. What is asserted from these
// records is structure — one parent for all nine, distinct labels, a label that matters —
// and the spelling itself is left to the published corpus.
//
// The secret is cloned because member_secret is erased in place immediately after the
// welcome derivation, so a record holding the caller's slice would read back as zeros. The
// answer is NOT cloned: it is the storage the schedule retains, and comparing the address
// of its first element is how a record is matched to the field it became.
type deriveSecretCall struct {
	secret []byte
	label  string
	answer []byte
}

type deriveSecretRecordingProvider struct {
	CryptoProvider
	calls []deriveSecretCall
}

func (self *deriveSecretRecordingProvider) DeriveSecret(secret []byte, label string) []byte {
	answer := self.CryptoProvider.DeriveSecret(secret, label)
	self.calls = append(self.calls, deriveSecretCall{
		secret: bytes.Clone(secret),
		label:  label,
		answer: answer,
	})
	return answer
}

// TestEveryEpochSecretIsDerivedFromOneParentUnderItsOwnLabel observes the three structural
// claims newKeyScheduleFromParts makes, none of which the published answers separate on
// their own once a value has been pinned.
//
//   - every one of the nine is DeriveSecret of the SAME parent. A secret derived from
//     member_secret, from welcome_secret or from another of the nine is still 32
//     pseudorandom bytes, and the corpus only says what the right answer is, not which
//     parent a wrong one came from.
//   - the nine labels are pairwise distinct. This is the same failure the value sweep
//     catches, read at the input rather than at the output, and it names the label.
//   - the label is what separates them: the same parent under a label differing by one
//     character answers with different bytes.
//
// The parent is checked against an independent statement of epoch_secret rather than merely
// against itself, so "all nine came from one parent" cannot be satisfied by all nine coming
// from the wrong one. That statement is RFC 9420 section 8 written out over the corpus's own
// inputs, and both of its steps are already held to the published answers by
// TestExpandWithLabelMatchesTheKeyScheduleVectors.
func TestEveryEpochSecretIsDerivedFromOneParentUnderItsOwnLabel(t *testing.T) {
	for _, epoch := range ksVectorEpochs(t) {
		recording := &deriveSecretRecordingProvider{CryptoProvider: epoch.crypto}
		schedule, err := NewKeySchedule(
			recording, epoch.initPrev, epoch.commitSecret, epoch.pskSecret, epoch.groupContext)
		if err != nil {
			t.Fatalf("%s: NewKeySchedule: %v", epoch.at, err)
		}
		fields := epochSecretsByField(t, schedule.Secrets())

		// the independent parent: Extract(joiner_secret, psk_secret) expanded under
		// "epoch" over the group context, per RFC 9420 section 8
		wantParent := epoch.crypto.ExpandWithLabel(
			epoch.crypto.Extract(
				mustDecodeHex(t, "joiner_secret"+epoch.at, epoch.published.JoinerSecret), epoch.pskSecret),
			"epoch", mustDecodeHex(t, "group_context"+epoch.at, epoch.published.GroupContext),
			epoch.crypto.HashSize())

		labels := map[string]string{}
		for _, name := range slices.Sorted(maps.Keys(fields)) {
			secret := fields[name]
			if len(secret) == 0 {
				t.Fatalf("%s: EpochSecrets.%s is empty, so no record can be matched to it", epoch.at, name)
			}
			matches := []deriveSecretCall{}
			for _, call := range recording.calls {
				if len(call.answer) > 0 && &call.answer[0] == &secret[0] {
					matches = append(matches, call)
				}
			}
			if len(matches) != 1 {
				t.Fatalf("%s: EpochSecrets.%s is the answer of %d recorded DeriveSecret calls, want 1; it is either not a DeriveSecret answer at all or it was copied out of one",
					epoch.at, name, len(matches))
			}
			if !bytes.Equal(matches[0].secret, wantParent) {
				t.Errorf("%s: EpochSecrets.%s was derived from %x and epoch_secret is %x, so it hangs off the wrong parent",
					epoch.at, name, matches[0].secret, wantParent)
			}
			if owner, taken := labels[matches[0].label]; taken {
				t.Errorf("%s: EpochSecrets.%s and EpochSecrets.%s were both derived under %q, so one label was pasted over the other",
					epoch.at, name, owner, matches[0].label)
			}
			labels[matches[0].label] = name

			// and the label is load bearing: one character of it changes the answer
			if altered := epoch.crypto.DeriveSecret(wantParent, matches[0].label+"x"); bytes.Equal(altered, secret) {
				t.Errorf("%s: EpochSecrets.%s is unchanged by altering only its DeriveSecret label, so the label is not what separates it from the other eight",
					epoch.at, name)
			}
		}
		if len(labels) != len(fields) {
			t.Errorf("%s: %d distinct labels produced %d secrets", epoch.at, len(labels), len(fields))
		}
		// the welcome derivation is the tenth recorded call and hangs off member_secret,
		// so a count of exactly ten says nothing else reached for the parent behind the
		// package's back
		if len(recording.calls) != len(fields)+1 {
			t.Errorf("%s: %d DeriveSecret calls, want %d: the nine and welcome_secret",
				epoch.at, len(recording.calls), len(fields)+1)
		}
		if _, welcomeIsAnEpochSecret := labels["welcome"]; welcomeIsAnEpochSecret {
			t.Errorf("%s: an epoch secret was derived under the welcome label", epoch.at)
		}
	}
}

// keyScheduleMethodsTakingArguments names an exported method of *KeySchedule the epoch
// secret sweep below cannot call, with the reason. It is empty at this task: all four
// accessors take the receiver and nothing else.
//
// It exists so that a later task adding an argument taking method has to write down why that
// method cannot be swept, rather than have it silently fall outside a gate whose whole subject
// is what this type is allowed to hand out. The map is checked against the type, so an entry
// cannot outlive the method it excuses.
//
// It is STILL empty after the five argument taking methods this plan has landed — Export, the
// two tag computations and the two tag verifiers — because every one of them is driven through
// keyScheduleMethodArgumentRows instead. That is the direction to prefer: an excuse buys
// nothing but silence, and each of those five answers something a sweep can compare.
var keyScheduleMethodsTakingArguments = map[string]string{}

// exportSweepContext is the exporter context the argument rows below carry.
//
// Deliberately not KDF.Nh octets and not empty: it travels into the sweeps as one of the
// things the exporter is handed, and a context that happened to be a kdf length would make
// the differential next door report a coincidence belonging to this test's own choice.
var exportSweepContext = bytes.Repeat([]byte{0x5c}, 19)

// keyScheduleMethodArgumentRows drives an exported method of *KeySchedule that takes
// arguments through the sweeps that read what this type hands out.
//
// A method with arguments cannot be called by a reflection sweep that has none, and the two
// ways out of that buy opposite things. Give it arguments and every byte it answers joins
// guardrail 6 exactly as an accessor's does; write it into
// keyScheduleMethodsTakingArguments instead and it joins nothing, which is only honest for
// a method whose arguments a test cannot supply. Export is driven rather than excused,
// because "the exporter cannot be made to answer epoch_secret" is a claim about the label,
// the context and the length a CALLER chooses, and an excuse is precisely a refusal to try
// any of them.
//
// The rows are built from the schedule rather than written out as constants, and that is
// load bearing twice over. One row asks for KDF.Nh bytes read off the provider, so the
// answer is the same size as the secret guardrail 6 is looking for and the comparison can
// mean something — an export of some other length can never equal epoch_secret whatever the
// implementation does. Another asks for a length that is not KDF.Nh, so the differential in
// TestEveryConstructionHandedAProviderReadsKdfNhFromIt sees an exporter length follow the
// caller's number rather than the provider's. Three rows rather than one, because an
// exporter that ignored its label or its context would answer one secret however it was
// called and a single row could not tell.
var keyScheduleMethodArgumentRows = map[string]func(schedule *KeySchedule) [][]reflect.Value{
	"Export": func(schedule *KeySchedule) [][]reflect.Value {
		nh := schedule.crypto.HashSize()
		return [][]reflect.Value{
			// the MASTER section 7 storage call: the seed recovery label, no context
			{reflect.ValueOf("URmessage/v1/storage"), reflect.ValueOf([]byte(nil)), reflect.ValueOf(nh)},
			// the same label with a context, so the pair differs only in the context
			{reflect.ValueOf("URmessage/v1/storage"), reflect.ValueOf(exportSweepContext), reflect.ValueOf(nh)},
			// a different label at a length that is not KDF.Nh
			{reflect.ValueOf("URmessage/v1/other"), reflect.ValueOf(exportSweepContext), reflect.ValueOf(nh + 8)},
		}
	},
	// the two tag computations and the two verifiers, driven rather than excused for the
	// reason Export is. A tag is KDF.Nh bytes — exactly the length guardrail 6 is looking
	// for — so a ConfirmationTag that answered the parent secret rather than a mac over it
	// would be an answer of exactly the right size, and an excuse here would be a refusal to
	// look at the four methods of this type most likely to be handed one.
	//
	// The verifier rows carry the tag the schedule itself computes for the same data, and
	// that is what makes a verifier observable at all: driven with an arbitrary tag a
	// verifier answers false however it is written, so a row that can only answer false says
	// nothing about one that answers true unconditionally. The rows are rebuilt from the
	// schedule on every call, so over an erased epoch the tag they carry is the nil one an
	// erased ConfirmationTag answers — and the verifier still has to refuse it.
	"ConfirmationTag": func(schedule *KeySchedule) [][]reflect.Value {
		return [][]reflect.Value{{reflect.ValueOf(tagSweepTranscriptHash(schedule))}}
	},
	"MembershipTag": func(schedule *KeySchedule) [][]reflect.Value {
		return [][]reflect.Value{{reflect.ValueOf(tagSweepTbm)}}
	},
	"VerifyConfirmationTag": func(schedule *KeySchedule) [][]reflect.Value {
		hash := tagSweepTranscriptHash(schedule)
		return [][]reflect.Value{{reflect.ValueOf(hash), reflect.ValueOf(schedule.ConfirmationTag(hash))}}
	},
	"VerifyMembershipTag": func(schedule *KeySchedule) [][]reflect.Value {
		return [][]reflect.Value{{reflect.ValueOf(tagSweepTbm), reflect.ValueOf(schedule.MembershipTag(tagSweepTbm))}}
	},
	// A *KeySchedule no longer answers a *VerifiedGroupContext at all, and the row that used
	// to drive one is gone with the method. ConfirmGroupContext confirmed a tag against the
	// epoch's own schedule, which is a tautology any package can write -- the context went in
	// through an exported constructor and ConfirmationTag is exported on this same type -- so
	// the verified type's constructor is (*GroupInfo).VerifiedContext now and the authority is
	// a member's signature checked against a tree. The flattener's arm for that result type is
	// kept below, because it is what would read a verified value if a method of this type ever
	// answered one again.
}

// tagSweepTranscriptHash is the confirmed_transcript_hash the sweeps drive the confirmation
// tag with. KDF.Nh octets read off the schedule's own provider rather than written down, so a
// suite with a wider hash is driven with a transcript hash of its own length rather than with
// this file's opinion of one.
func tagSweepTranscriptHash(schedule *KeySchedule) []byte {
	return bytes.Repeat([]byte{0x3b}, schedule.crypto.HashSize())
}

// tagSweepTbm stands in for the serialized AuthenticatedContentTBM p6 will hand the membership
// tag. Deliberately not KDF.Nh octets: the membership tag is taken over a framed message of
// whatever length it happens to be, and a sweep input that was a kdf length would make an
// answer of that length a coincidence of this test's own choosing.
var tagSweepTbm = []byte("AuthenticatedContentTBM sweep bytes, deliberately not a kdf length")

// exposedAnswerAt names one answer of one method: the method, and the position of the result
// it came out of.
//
// The position is what makes an excuse per ANSWER rather than per METHOD. ExternalKeyPair
// answers a private key at Nsk and a public key at Npk, neither of which is a kdf length, and
// an excuse keyed on the method alone would go on covering a THIRD answer that is one. The two
// results it has today each carry their own line, and a result added to it is excused by
// nobody until somebody writes one.
type exposedAnswerAt struct {
	method string
	result int
}

func (self exposedAnswerAt) String() string {
	return fmt.Sprintf("(*KeySchedule).%s result %d", self.method, self.result)
}

// exposedSlice is one byte slice a sweep read off an exported surface, with the method it
// came out of, the position of the result it came out of, and the path inside that result.
//
// The provenance travels with the bytes because the sweeps reading these want different
// subsets of them: guardrail 6 asks about every one, and the KDF.Nh differential next door
// has to leave out the answers that are not kdf lengths at all. Carrying the address is what
// lets that second sweep say what it dropped instead of dropping by position.
type exposedSlice struct {
	method string
	result int
	path   string
	bytes  []byte
	// taken is what bytes held at the moment it was read, and bytes is a VIEW into the
	// schedule's own array rather than a copy of it. The pair is how the sweep says whether
	// a later call rewrote a reading an earlier one had already taken -- one witness per
	// reading, because a sweep collects eleven of them and any one of the eleven can be the
	// one that gets rewritten.
	taken []byte
}

// at is the address the excuse tables below are keyed by.
func (self exposedSlice) at() exposedAnswerAt {
	return exposedAnswerAt{method: self.method, result: self.result}
}

// exposedBytes is the byte slices alone, in order, for a sweep that does not care where each
// of them came from.
func exposedBytes(exposed []exposedSlice) [][]byte {
	raw := make([][]byte, 0, len(exposed))
	for _, one := range exposed {
		raw = append(raw, one.bytes)
	}
	return raw
}

// exposedByteSlices flattens everything one call handed back into the byte slices a caller
// can read. A byte slice result is one; a pointer to a struct is each of its exported byte
// slice fields, which is the shape Secrets() has; a nil error contributes nothing.
//
// A byte slice is recognised by KIND rather than by the spelling []byte. HpkePublicKey and
// HpkePrivateKey are byte slices under names of their own — registry section 3.2, which
// TestConsumedHpkePublicKeyIsASlice holds them to — so a comparison against the unnamed type
// drops exactly the two results ExternalKeyPair answers out of guardrail 6's class, and a
// gate that demands less reports what a complete one reports.
func exposedByteSlices(t *testing.T, what string, result reflect.Value) []exposedSlice {
	t.Helper()
	errorInterface := reflect.TypeOf((*error)(nil)).Elem()
	if result.Type() == errorInterface {
		// every row these sweeps are built from succeeds, so a failure here is the sweep's
		// own bug rather than a place a secret hides
		if !result.IsNil() {
			t.Fatalf("%s answered %v, and the rows this sweep is built from succeed", what, result.Interface())
		}
		return nil
	}
	if result.Kind() == reflect.Slice && result.Type().Elem().Kind() == reflect.Uint8 {
		return []exposedSlice{{path: what, bytes: result.Bytes()}}
	}
	if result.Kind() == reflect.Bool {
		// a verifier answers a question rather than a value, and one bit cannot be KDF.Nh
		// bytes of epoch secret however it is set — so it contributes nothing to guardrail
		// 6's class. It is not nothing to every sweep that reads this: the erased epoch gate
		// reads the bit itself through scheduleAnswer, because a verifier that ACCEPTS over
		// an epoch whose key has become publicly computable is the same defect as a
		// derivation that answers over one.
		return nil
	}
	// a value whose whole storage sits behind an UNEXPORTED field, which is the one shape the
	// field walk below reads as empty rather than as unreadable -- and empty is exactly the
	// clean run a leak reports. VerifiedGroupContext is that shape on purpose: the unexported
	// field is what makes the type unforgeable outside this package, so the sweep goes through
	// the accessor, which is the whole of what any caller can reach through one. Read as an
	// ordinary pointer to a struct it would contribute nothing, and this gate would be silent
	// about every byte an epoch handed out through it.
	if result.Type() == reflect.TypeOf((*VerifiedGroupContext)(nil)) {
		if result.IsNil() {
			return nil
		}
		confirmed, _ := result.Interface().(*VerifiedGroupContext)
		return exposedByteSlices(t, what+".Context()", reflect.ValueOf(confirmed.Context()))
	}
	if result.Kind() == reflect.Pointer && result.Type().Elem().Kind() == reflect.Struct {
		if result.IsNil() {
			return nil
		}
		exposed := []exposedSlice{}
		element := result.Elem()
		for i := range element.NumField() {
			if !element.Type().Field(i).IsExported() {
				continue
			}
			exposed = append(exposed,
				exposedByteSlices(t, what+"."+element.Type().Field(i).Name, element.Field(i))...)
		}
		return exposed
	}
	// a struct held by VALUE, which is the same reading as the pointer above one indirection
	// in. GroupContext's extensions are a vector of them, so without this arm the one answer
	// that reaches this sweep through an accessor would stop at the vector.
	if result.Kind() == reflect.Struct {
		exposed := []exposedSlice{}
		for i := range result.NumField() {
			if !result.Type().Field(i).IsExported() {
				continue
			}
			exposed = append(exposed,
				exposedByteSlices(t, what+"."+result.Type().Field(i).Name, result.Field(i))...)
		}
		return exposed
	}
	// a vector of something that is not octets, read element by element. The octet case is
	// answered above, so what lands here is a vector of structures, and a secret parked in one
	// is as reachable as a secret parked in a field.
	if result.Kind() == reflect.Slice {
		exposed := []exposedSlice{}
		for i := range result.Len() {
			exposed = append(exposed,
				exposedByteSlices(t, fmt.Sprintf("%s[%d]", what, i), result.Index(i))...)
		}
		return exposed
	}
	// a fixed width scalar cannot BE KDF.Nh octets however it is set, so there is nothing here
	// for guardrail 6 to compare. It is read as nothing rather than refused, because refusing
	// it would make an answer that carries a protocol version beside its secrets unreadable as
	// a WHOLE -- which is how a real leak ends up outside the sweep, one field along from a
	// number nobody cared about.
	switch result.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return nil
	}
	t.Fatalf("%s answers a %s, which this sweep cannot read for bytes; extend it rather than letting a new result shape fall outside guardrail 6",
		what, result.Type())
	return nil
}

// recoveringRow runs one row of a package wide sweep with a panic caught rather than taken.
//
// A sweep like the ones below calls production code with inputs the test chose, and a defect
// that panics on one of them takes the test BINARY down: go reports the panicking test as
// the single failure of the run, every test declared after it never runs, and every gate
// those tests hold reports nothing at all. Measured, not supposed: deriving InitSecret from
// the welcome_secret that is nil on the group creation path panicked inside
// TestEveryConstructionHandedAProviderRoutesThroughIt, and a whole package run then reported
// one failure in crypto_labels_test.go, 2325 fewer passes and nothing whatever about the key
// schedule -- while the key schedule's own gates, run on their own, caught that same edit
// three times over. A row that panics is a failure OF THAT ROW, and the rest of the package
// still has to answer for itself.
//
// t.Fatalf inside the call is unaffected: it leaves the goroutine through runtime.Goexit
// rather than through a panic, recover answers nil, and the exit goes on happening.
//
// recoveredPanic in crypto_test.go is the same guard for a call that answers nothing.
func recoveringRow[T any](call func() T) (answer T, raised any) {
	defer func() { raised = recover() }()
	return call(), nil
}

// TestTheSweepRowRecoveryReturnsRatherThanTakingTheBinaryDown is the control on that.
//
// A recovery that had stopped recovering is invisible from every sweep that uses it: the
// binary dies exactly as it did before, and the run reports one failure somewhere else. The
// normal path is asserted alongside, because a helper that swallowed every answer would make
// each of those sweeps compare nothing and report the clean run a working one reports.
func TestTheSweepRowRecoveryReturnsRatherThanTakingTheBinaryDown(t *testing.T) {
	answer, raised := recoveringRow(func() []byte { return []byte{0x11, 0x22} })
	if raised != nil {
		t.Errorf("a call that did not panic reported %v", raised)
	}
	if !bytes.Equal(answer, []byte{0x11, 0x22}) {
		t.Errorf("a call that did not panic answered %x, want 1122", answer)
	}
	answer, raised = recoveringRow(func() []byte { panic("the row panicked") })
	if raised == nil {
		t.Fatal("a call that panicked reported no panic, so every sweep reading this recovery would still take the binary down")
	}
	if raised != "the row panicked" {
		t.Errorf("the recovery reported %v rather than what was raised", raised)
	}
	if answer != nil {
		t.Errorf("a call that panicked answered %x rather than nothing", answer)
	}
}

// epochSecretOfTheEpoch states epoch_secret independently of the type under test, over the
// corpus's own published inputs: Extract(joiner_secret, psk_secret) expanded under "epoch"
// over the published GroupContext bytes.
//
// Independent is the point. A statement read off the schedule would agree with any schedule,
// including one that hands the secret out.
func epochSecretOfTheEpoch(t *testing.T, epoch ksVectorEpoch) []byte {
	t.Helper()
	return epoch.crypto.ExpandWithLabel(
		epoch.crypto.Extract(
			mustDecodeHex(t, "joiner_secret"+epoch.at, epoch.published.JoinerSecret), epoch.pskSecret),
		"epoch", mustDecodeHex(t, "group_context"+epoch.at, epoch.published.GroupContext),
		epoch.crypto.HashSize())
}

// bytesTheScheduleHandsOut is every byte slice reachable through *KeySchedule's own exported
// surface, with the names of the methods that were called.
//
// The class is the type's exported surface read by reflection, not a list of accessors
// written here, so a method added later joins by existing. The exported fields are refused
// outright: storage reachable without going through a method is storage this sweep would
// never call for.
func bytesTheScheduleHandsOut(t *testing.T, at string, schedule *KeySchedule) ([]exposedSlice, []string) {
	t.Helper()
	scheduleType := reflect.TypeOf(schedule)
	valueType := scheduleType.Elem()
	for i := range valueType.NumField() {
		if valueType.Field(i).IsExported() {
			t.Fatalf("%s: KeySchedule has exported field %s, so its storage is reachable without going through a method this sweep reads",
				at, valueType.Field(i).Name)
		}
	}

	exposed := []exposedSlice{}
	swept := []string{}
	notCalled := []string{}
	for i := range scheduleType.NumMethod() {
		method := scheduleType.Method(i)
		// a method handed nothing that answers nothing has nowhere to put a secret, so there
		// is no answer here to read -- and calling it can only change what the REST of this
		// sweep reads, since everything collected below is a view into the schedule's own
		// arrays rather than a copy of them. Zeroize is that shape, and it sorts last of the
		// exported methods. Measured, not supposed: with Zeroize present and called here,
		// every slice this sweep had collected read as zeros by the time the comparison ran,
		// and TestNoExportedSurfaceOfTheKeyScheduleReturnsTheEpochSecret went on PASSING
		// against func (self *KeySchedule) EpochSecretLeak() []byte { return self.epochSecret }
		// appended to key_schedule.go -- the same edit it catches at ten epochs without it.
		//
		// The criterion is the signature and not the name. It is the same shape filter
		// theExportedMethodsHandingOutWhatTheyReach applies to the source, for the same
		// reason, so the two agree about what an eraser is and a second one joins by existing.
		// What such a method can REACH is that gate's question rather than this one's.
		if method.Type.NumIn() == 1 && method.Type.NumOut() == 0 {
			notCalled = append(notCalled, method.Name)
			continue
		}
		// one row of no arguments for an accessor, and whatever
		// keyScheduleMethodArgumentRows supplies for a method that takes some
		rows := [][]reflect.Value{nil}
		if method.Type.NumIn() != 1 {
			build, driven := keyScheduleMethodArgumentRows[method.Name]
			if !driven {
				if reason, excused := keyScheduleMethodsTakingArguments[method.Name]; !excused {
					t.Fatalf("%s: (*KeySchedule).%s takes arguments and this sweep calls with none; give it rows in keyScheduleMethodArgumentRows or write down in keyScheduleMethodsTakingArguments why it cannot surface epoch_secret",
						at, method.Name)
				} else {
					t.Logf("%s: (*KeySchedule).%s not swept: %s", at, method.Name, reason)
				}
				continue
			}
			rows = build(schedule)
			if len(rows) == 0 {
				t.Fatalf("%s: keyScheduleMethodArgumentRows drives (*KeySchedule).%s with no rows at all, so it is swept in name only",
					at, method.Name)
			}
			// the arity and the types are checked rather than left to reflect.Call, whose
			// answer to a row that does not fit is a panic naming neither the method nor
			// the row that did not fit it
			for _, row := range rows {
				if len(row)+1 != method.Type.NumIn() {
					t.Fatalf("%s: keyScheduleMethodArgumentRows drives (*KeySchedule).%s with %d arguments and it takes %d",
						at, method.Name, len(row), method.Type.NumIn()-1)
				}
				for argument, value := range row {
					if want := method.Type.In(argument + 1); !value.Type().AssignableTo(want) {
						t.Fatalf("%s: keyScheduleMethodArgumentRows hands (*KeySchedule).%s a %s in argument %d, which takes %s",
							at, method.Name, value.Type(), argument, want)
					}
				}
			}
		} else if _, driven := keyScheduleMethodArgumentRows[method.Name]; driven {
			t.Errorf("keyScheduleMethodArgumentRows drives %s, which takes no arguments", method.Name)
		}
		swept = append(swept, method.Name)
		for _, row := range rows {
			for index, result := range method.Func.Call(append([]reflect.Value{reflect.ValueOf(schedule)}, row...)) {
				for _, one := range exposedByteSlices(t, "(*KeySchedule)."+method.Name, result) {
					one.method = method.Name
					one.result = index
					one.taken = bytes.Clone(one.bytes)
					exposed = append(exposed, one)
				}
			}
		}
	}
	for name := range keyScheduleMethodsTakingArguments {
		if _, found := scheduleType.MethodByName(name); !found {
			t.Errorf("keyScheduleMethodsTakingArguments excuses %s, which *KeySchedule does not declare", name)
		}
		if _, driven := keyScheduleMethodArgumentRows[name]; driven {
			t.Errorf("%s is both excused from this sweep and driven through it, so which one holds depends on the order these two maps are read",
				name)
		}
	}
	for name := range keyScheduleMethodArgumentRows {
		if _, found := scheduleType.MethodByName(name); !found {
			t.Errorf("keyScheduleMethodArgumentRows drives %s, which *KeySchedule does not declare", name)
		}
	}
	// and no call this sweep made rewrote a reading another of them had already taken. The
	// skip above is what keeps that true today; this is what says so if it stops being, and
	// it is the difference between a sweep that read nothing and a sweep that read zeros.
	//
	// Two witnesses, because a rewrite lands on one side or the other of the reading it
	// spoils and neither witness sees both. A call made AFTER a reading was taken leaves the
	// reading disagreeing with the clone taken with it, since every reading is a view into
	// the schedule's own array rather than a copy of it. A call made BEFORE it leaves nothing
	// to disagree with -- the reading was zeros when it was read -- so what says so is the
	// reading itself: every secret this type hands out is KDF output over inputs no party can
	// steer to a fixed point, and a run of zeros where one belongs is an erase rather than a
	// secret.
	//
	// A clone taken before the loop and compared after it is what this replaced, and it was
	// neither of the two. It watched init_secret alone out of eleven readings, so a call that
	// rewrote any of the other ten said nothing; and reading the accessors up front to widen
	// it does not work, because an accessor with the side effect is called by that pass too
	// and erases what a later accessor of the same pass then reads as its baseline.
	//
	// One limit, measured rather than supposed. An erase landing before an accessor that
	// REFUSES rather than answering zeros is invisible to both: JoinerSecret and
	// WelcomeSecret ask secretIsLive and answer nil, and an empty reading is legitimate here
	// -- the group creation path has neither secret. Closing that would need a witness over
	// the STORAGE rather than over the readings, and the storage is unexported, which is
	// exactly what puts it outside reflection.
	for _, one := range exposed {
		if !bytes.Equal(one.bytes, one.taken) {
			t.Fatalf("%s: %s changed after this sweep read it, so what it collected is not what those methods answered and every comparison over it runs against the rewrite",
				at, one.path)
		}
		// bytes.Clone(nil) is nil and bytes.Equal(nil, nil) is true, so the comparison above
		// says nothing at all about an empty reading. An empty one is legitimate -- the
		// group creation path has no joiner_secret and no welcome_secret -- and it is a
		// RUN of zeros that cannot be a secret.
		if len(one.taken) != 0 && !slices.ContainsFunc(one.taken, func(b byte) bool { return b != 0 }) {
			t.Fatalf("%s: %s answered %d zero bytes; every secret this type hands out is KDF output, so a run of zeros is an erase that happened before this sweep read it and every comparison over these readings runs against that erase",
				at, one.path, len(one.taken))
		}
	}
	if len(notCalled) != 0 {
		t.Logf("%s: %v are handed nothing and answer nothing, so this sweep recorded them rather than calling them", at, notCalled)
	}
	return exposed, swept
}

// TestNoExportedSurfaceOfTheKeyScheduleReturnsTheEpochSecret is guardrail G6 read as
// behaviour rather than as a convention, over the type's own surface.
//
// epoch_secret is the parent of all nine. A caller holding it holds confirmation_key and
// membership_key — the two secrets that authenticate a commit — and every other secret the
// epoch will ever produce, so an accessor returning it is a one line change that gives the
// epoch away while every other test in this file goes on passing. It is also the natural
// thing to add: a constructor wants to return it and a test helper wants to accept it, which
// is why this is written down at the task that first derives it rather than left to the one
// that names the guardrail.
//
// The class is the type's own exported surface read by reflection, not a list of accessors
// written here, so a method added later joins the sweep by existing. What each answer is
// compared against is an independent statement of epoch_secret over the corpus's own inputs,
// and the sweep asserts it found the secrets it should find before concluding it did not
// find the one it should not: a flattener that read nothing reports the same clean run as
// one that read everything.
//
// This half covers what the TYPE hands out. What the PACKAGE hands out is a wider class and
// is covered next door, by TestNoExportedFunctionOfThisPackageHandsOutTheEpochSecret: a free
// function taking a *KeySchedule reaches the same unexported field and is nowhere in this
// reflection.
func TestNoExportedSurfaceOfTheKeyScheduleReturnsTheEpochSecret(t *testing.T) {
	for _, epoch := range ksVectorEpochs(t) {
		schedule := epoch.schedule(t)
		epochSecret := epochSecretOfTheEpoch(t, epoch)
		exposed, swept := bytesTheScheduleHandsOut(t, epoch.at, schedule)

		// the controls: the sweep reached the surface, and the flattener really does
		// return the bytes behind it. Without these an accessor that answered nothing,
		// or a flattener that dropped every struct field, reports the same clean run.
		if len(swept) < 4 {
			t.Fatalf("%s: swept %d exported methods of *KeySchedule (%v), and the type declares four accessors at least",
				epoch.at, len(swept), swept)
		}
		fields := epochSecretsByField(t, schedule.Secrets())
		if len(exposed) < len(fields)+2 {
			t.Fatalf("%s: the sweep read %d byte slices off the exported surface and Secrets() alone carries %d, so it is not reading what it claims to",
				epoch.at, len(exposed), len(fields))
		}
		for _, known := range [][]byte{schedule.JoinerSecret(), schedule.Secrets().InitSecret} {
			if !slices.ContainsFunc(exposed, func(one exposedSlice) bool { return bytes.Equal(one.bytes, known) }) {
				t.Fatalf("%s: the sweep did not find a secret the type certainly exposes, so a secret it must not expose would be missed too",
					epoch.at)
			}
		}
		// and an answer of KDF.Nh bytes really did go through it. The comparison below is
		// against a secret that is KDF.Nh long, so a sweep in which no answer was ever that
		// length could not have found epoch_secret however this type handed it out.
		if !slices.ContainsFunc(exposed, func(one exposedSlice) bool {
			return len(one.bytes) == epoch.crypto.HashSize()
		}) {
			t.Fatalf("%s: no answer the sweep read is KDF.Nh bytes long, so none of them could equal epoch_secret whatever this type did",
				epoch.at)
		}

		for index, one := range exposed {
			if bytes.Equal(one.bytes, epochSecret) {
				t.Errorf("%s: the exported surface hands out epoch_secret through %s at position %d of %d; it is the parent of every secret of this epoch and G6 says no exported symbol returns it",
					epoch.at, one.path, index, len(exposed))
			}
		}
	}
}

// epochSecretStorageField is the unexported field an epoch keeps its parent secret in.
//
// This names the SUBJECT of G6 and not the class G6 covers. The class — every exported
// symbol through which a caller can reach that storage — is derived below, starting from
// whichever type declares this field, so a free function nobody thought of joins it by
// existing. A rename this cannot find is fatal rather than clean: a gate that lost its
// subject reports exactly the clean run a complete one reports.
const epochSecretStorageField = "epochSecret"

// structTypesIn adds the named struct types one parsed file declares.
func structTypesIn(parsed parsedSource, into map[string]*ast.StructType) {
	for _, declaration := range parsed.file.Decls {
		typeDeclaration, isTypeDeclaration := declaration.(*ast.GenDecl)
		if !isTypeDeclaration || typeDeclaration.Tok != token.TYPE {
			continue
		}
		for _, specification := range typeDeclaration.Specs {
			named, isNamed := specification.(*ast.TypeSpec)
			if !isNamed {
				continue
			}
			if structure, isStruct := named.Type.(*ast.StructType); isStruct {
				into[named.Name.Name] = structure
			}
		}
	}
}

// identifiersNamedIn collects every bare identifier a type expression mentions, so *T, []T,
// map[K]T, T[P] and func(T) all report T.
//
// A rendered string compared with strings.Contains would answer yes for a type whose name
// merely contains another's, and no for one spelled across a line break.
func identifiersNamedIn(expr ast.Expr) []string {
	named := []string{}
	ast.Inspect(expr, func(node ast.Node) bool {
		if identifier, isIdentifier := node.(*ast.Ident); isIdentifier {
			named = append(named, identifier.Name)
		}
		return true
	})
	return named
}

// theTypesHoldingTheEpochSecret is the closure over a set of named struct types of "keeps
// the epoch secret": the type that declares the field, and any type with a field that
// mentions one that does.
//
// A closure rather than the one declaring type, because a struct holding a *KeySchedule
// hands its own holder the same storage, and an exported function taking one of those is
// handed the epoch secret just as surely as one taking the schedule itself.
func theTypesHoldingTheEpochSecret(structs map[string]*ast.StructType) []string {
	holding := map[string]bool{}
	for _, name := range theTypesDeclaringTheEpochSecret(structs) {
		holding[name] = true
	}
	for {
		grew := false
		for name, structure := range structs {
			if holding[name] {
				continue
			}
			for _, field := range structure.Fields.List {
				for _, mentioned := range identifiersNamedIn(field.Type) {
					if holding[mentioned] {
						holding[name] = true
						grew = true
					}
				}
			}
		}
		if !grew {
			return slices.Sorted(maps.Keys(holding))
		}
	}
}

// theTypesDeclaringTheEpochSecret is the SEED of the closure above: the structs that DECLARE the
// storage, without the ones that merely reach it through a field of their own.
//
// The two readings answer different questions and both are wanted. "What can hand the epoch secret
// out" is transitive -- a *Group holds a *KeySchedule, so a method on a Group could answer the
// schedule's secret -- and guardrail 6's two surface gates are right to close over that. "Whose
// erase must clear it", "whose methods derive from the nine", and "whose tag verifiers compare
// through MacVerify" are all questions about the type that OWNS the storage: a type that reaches it
// through a pointer has no secrets field to sweep, no Zeroize of its own to read, and its own
// refusals are held where its own state is. Reading the transitive set in those three does not
// widen them, it points them at the wrong type -- the closure answers a SORTED list, so the day
// p7's Group landed the first member became Group and all three would have been asking about a
// struct holding none of the storage they are written over.
//
// It is derived, and it still fails on a second DECLARER: a struct that declares the storage joins
// this reading by declaring it, exactly as it joined the closure, and the three gates over it
// refuse to run with more than one member. What it stops demanding is that a type which merely
// POINTS at a schedule answer for that schedule's erase, which was never a claim any of them made.
func theTypesDeclaringTheEpochSecret(structs map[string]*ast.StructType) []string {
	declaring := map[string]bool{}
	for name, structure := range structs {
		for _, field := range structure.Fields.List {
			for _, declared := range field.Names {
				if declared.Name == epochSecretStorageField {
					declaring[name] = true
				}
			}
		}
	}
	return slices.Sorted(maps.Keys(declaring))
}

// theExportedSurfaceReaching is every exported package level function of the parsed files
// whose signature mentions one of those types, in either direction.
//
// Both directions, because both are ways an exported symbol comes to have an epoch secret
// to give away: a parameter is handed one, and a result hands one out to be read further.
// Methods are excluded and are not uncovered — the exported methods of a holder are swept by
// reflection in the gate above, which is a reading of the compiled type rather than of its
// source and therefore sees an embedded surface this parse would not.
func theExportedSurfaceReaching(files []parsedSource, holders []string) []string {
	reaching := []string{}
	for _, parsed := range files {
		for _, declaration := range parsed.file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Recv != nil || !function.Name.IsExported() {
				continue
			}
			fields := []*ast.Field{}
			if function.Type.Params != nil {
				fields = append(fields, function.Type.Params.List...)
			}
			if function.Type.Results != nil {
				fields = append(fields, function.Type.Results.List...)
			}
			for _, field := range fields {
				if slices.ContainsFunc(identifiersNamedIn(field.Type), func(name string) bool {
					return slices.Contains(holders, name)
				}) {
					reaching = append(reaching, function.Name.Name)
					break
				}
			}
		}
	}
	slices.Sort(reaching)
	return reaching
}

// packageLevelValuesNaming is every package level var or const of the parsed files whose
// declared type, or whose initialiser, mentions one of those names.
//
// A standing value of a holder type would be an epoch secret reachable with no call at all,
// so the sweep below could not see it however complete its rows were. This package declares
// none and this is what says so.
func packageLevelValuesNaming(files []parsedSource, holders []string) []string {
	naming := []string{}
	for _, parsed := range files {
		for _, declaration := range parsed.file.Decls {
			values, isValueDeclaration := declaration.(*ast.GenDecl)
			if !isValueDeclaration || (values.Tok != token.VAR && values.Tok != token.CONST) {
				continue
			}
			for _, specification := range values.Specs {
				value, isValue := specification.(*ast.ValueSpec)
				if !isValue {
					continue
				}
				mentioned := []string{}
				if value.Type != nil {
					mentioned = append(mentioned, identifiersNamedIn(value.Type)...)
				}
				for _, initialiser := range value.Values {
					mentioned = append(mentioned, identifiersNamedIn(initialiser)...)
				}
				if !slices.ContainsFunc(mentioned, func(name string) bool { return slices.Contains(holders, name) }) {
					continue
				}
				for _, declared := range value.Names {
					naming = append(naming, declared.Name)
				}
			}
		}
	}
	slices.Sort(naming)
	return naming
}

// epochSecretHolderControl declares one of each shape the derivation has to tell apart: the
// struct that keeps the secret, a struct that reaches it through a pointer to that one, a
// struct that keeps a byte slice under another name entirely, exported functions taking and
// answering a holder, an unexported one, an exported one over something else, and a package
// level value of a holder type.
//
// Without it a derivation that had stopped deriving — a parse that read no structs, a
// closure that never seeded — would report an empty class, every gate reading it would
// demand nothing, and the run would look exactly like the run of a complete one.
const epochSecretHolderControl = "package control\n" +
	"\n" +
	"type Holder struct {\n" +
	"\tcrypto      int\n" +
	"\tepochSecret []byte\n" +
	"}\n" +
	"\n" +
	"type Wrapper struct {\n" +
	"\tinner *Holder\n" +
	"}\n" +
	"\n" +
	"type Unrelated struct {\n" +
	"\tsecret []byte\n" +
	"}\n" +
	"\n" +
	"var TheStandingHolder = &Holder{}\n" +
	"\n" +
	"func ExportedOverTheHolder(holder *Holder) []byte {\n" +
	"\treturn holder.epochSecret\n" +
	"}\n" +
	"\n" +
	"func ExportedOverTheWrapper(wrapper Wrapper) []byte {\n" +
	"\treturn wrapper.inner.epochSecret\n" +
	"}\n" +
	"\n" +
	"func ExportedAnsweringAHolder(seed []byte) (*Holder, error) {\n" +
	"\treturn nil, nil\n" +
	"}\n" +
	"\n" +
	"func exportedNowhere(holder *Holder) []byte {\n" +
	"\treturn holder.epochSecret\n" +
	"}\n" +
	"\n" +
	"func ExportedOverSomethingElse(unrelated *Unrelated) []byte {\n" +
	"\treturn unrelated.secret\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) Exported() []byte {\n" +
	"\treturn nil\n" +
	"}\n"

// groupMethodsTakingArguments excuses an exported method of *Group from the sweep below.
//
// It is EMPTY and the intention is that it stays that way. Every method this type declares that
// takes arguments can be driven with arguments a caller would choose, and an excuse here is
// precisely a refusal to try them -- which is the shape keyScheduleMethodArgumentRows' own doc
// argues against, one type over.
var groupMethodsTakingArguments = map[string]string{
	// p7 task 18's second half. It answers an ERROR ALONE, so there is nothing here for this sweep
	// to read out of it -- the same shape the no-argument erasers above are skipped for, which the
	// signature filter cannot skip because this one takes a parameter. What it INSTALLS is the
	// staged epoch, and every octet of that becomes reachable through the group's own accessors --
	// EpochAuthenticator, EpochSecret, Export -- which this sweep drives.
	//
	// It is also the one method of *Group whose only real argument is a value ProcessMessage
	// answered, and driving it would mean applying an inbound commit and advancing this group past
	// the epoch whose secret the whole comparison is written against. See
	// epochSecretSweepInboundMessage for why the two rows that ARE driven take care not to.
	"ApplyCommit": "answers an error alone, so this sweep would read no octets out of it whatever " +
		"it were driven with; the epoch it installs is read through the accessors this sweep does drive",
}

// epochSecretSweepInboundMessage builds a message this group can OPEN, for the two inbound rows
// below, WITHOUT there being a second member and WITHOUT advancing the epoch.
//
// BOTH HALVES OF THAT ARE THE POINT. The group this sweep runs over is founded so that its epoch
// secret IS the value every comparison here is made against -- see newGroupOverTheEpochSecret --
// so a row that added a peer, committed and merged would leave the group holding the secrets of an
// epoch the corpus does not name, and every row reflection happens to call afterwards would be
// compared against a value nothing holds. That is a gate that reports a clean run because it has
// stopped looking, which is the one direction this file must not move in.
//
// So the sender is THIS GROUP'S OWN LEAF and the message is sealed under a SECOND secret tree
// derived from the same encryption secret. Both trees ratchet identically, so a message the second
// one seals opens under the first -- and the group's own ratchet is left where it was, because
// nothing spent a generation of it. The second tree is fast-forwarded to the generation the
// group's is parked on, read off the group's own ratchet rather than assumed to be zero: rows are
// called in reflection order, several of them seal, and a fixture that depended on which of them
// ran first would break on the day a method is renamed.
func epochSecretSweepInboundMessage(t *testing.T, group *Group, aad []byte, plaintext []byte) []byte {
	t.Helper()
	encryption, err := group.EpochSecret(EpochSecretEncryption)
	if err != nil {
		t.Fatalf("the encryption secret this row's sender derives from: %v", err)
	}
	// the width the RECEIVER's own secret tree was built at and not the ratchet tree's current
	// one, which is the same fixture-order hazard the fast-forward below answers: the
	// ProposeRemove row splices a second leaf into the ratchet tree, and a sender tree built at
	// the new width derives a different ratchet for the same leaf -- every key well formed, every
	// length right, and nothing opens.
	sender, err := NewSecretTree(group.crypto, group.secretTree.leafCount, encryption)
	if err != nil {
		t.Fatalf("the second secret tree this row seals under: %v", err)
	}
	kind, err := ratchetTypeOf(ContentTypeApplication)
	if err != nil {
		t.Fatalf("the ratchet an application message travels in: %v", err)
	}
	head := uint32(0)
	if r, held := group.secretTree.ratchets[ratchetKey{leaf: group.ownLeaf, kind: kind}]; held {
		head = r.head
	}
	for at := uint32(0); at < head; at += 1 {
		if _, _, _, err := sender.NextMessageKey(ContentTypeApplication, group.ownLeaf); err != nil {
			t.Fatalf("fast-forward this row's sender to generation %d: %v", head, err)
		}
	}
	groupContext, err := group.GroupContext()
	if err != nil {
		t.Fatalf("the group context this row's message is signed against: %v", err)
	}
	content := &FramedContent{
		GroupId:           group.GroupId(),
		Epoch:             group.Epoch(),
		Sender:            Sender{SenderType: SenderTypeMember, LeafIndex: group.ownLeaf},
		AuthenticatedData: aad,
		ContentType:       ContentTypeApplication,
		ApplicationData:   plaintext,
	}
	authenticated, err := SignAuthenticatedContent(group.crypto, group.signer,
		WireFormatPrivateMessage, content, groupContext)
	if err != nil {
		t.Fatalf("sign this row's message: %v", err)
	}
	private, err := SealPrivateMessage(group.crypto, sender, group.senderDataSecretLocked(),
		authenticated, PaddingSizeV1)
	if err != nil {
		t.Fatalf("seal this row's message: %v", err)
	}
	encoded, err := MarshalMLSMessage(&MLSMessage{
		Version:        ProtocolVersionMls10,
		WireFormat:     WireFormatPrivateMessage,
		PrivateMessage: private,
	})
	if err != nil {
		t.Fatalf("frame this row's message: %v", err)
	}
	return encoded
}

// groupMethodArgumentRows drives an exported method of *Group that takes arguments through the
// sweep that reads what a group hands out.
//
// Export carries the same three rows the schedule's does and for the same reason: one of them asks
// for KDF.Nh octets, so the answer is the same SIZE as the secret guardrail 6 is looking for and
// the comparison can mean something, and another asks for a length that is not KDF.Nh so the pair
// differs in the length alone. EpochSecret is driven over the whole of its closed enum, because an
// accessor that already answers secrets is the likeliest place for a leak to be written; the arm
// that REFUSES a name outside the enum is not driven here, since a row that errors is a row this
// sweep refuses to read, and TestEpochSecretAccessorIsClosed is what holds that arm.
var groupMethodArgumentRows = map[string]func(t *testing.T, group *Group) [][]reflect.Value{
	"Export": func(t *testing.T, group *Group) [][]reflect.Value {
		nh := group.crypto.HashSize()
		return [][]reflect.Value{
			{reflect.ValueOf("URmessage/v1/storage"), reflect.ValueOf([]byte(nil)), reflect.ValueOf(nh)},
			{reflect.ValueOf("URmessage/v1/storage"), reflect.ValueOf(exportSweepContext), reflect.ValueOf(nh)},
			{reflect.ValueOf("URmessage/v1/other"), reflect.ValueOf(exportSweepContext), reflect.ValueOf(nh + 8)},
		}
	},
	// ledger item 242's R2 exporter over the STAGED epoch, driven with Export's three rows. It is
	// reachable at all because the CreateCommit row above it in method order stages a commit and
	// nothing between the two clears it -- MergePendingCommit and ClearPendingCommit take nothing
	// and answer only errors, so the signature filter skips both -- and it is worth driving
	// because what it answers is derived from the staged epoch's schedule, whose parent secret is
	// the value a committer must never be handed any more than the live one's.
	"PendingExport": func(t *testing.T, group *Group) [][]reflect.Value {
		nh := group.crypto.HashSize()
		return [][]reflect.Value{
			{reflect.ValueOf("URmessage/v1/storage"), reflect.ValueOf([]byte(nil)), reflect.ValueOf(nh)},
			{reflect.ValueOf("URmessage/v1/storage"), reflect.ValueOf(exportSweepContext), reflect.ValueOf(nh)},
			{reflect.ValueOf("URmessage/v1/other"), reflect.ValueOf(exportSweepContext), reflect.ValueOf(nh + 8)},
		}
	},
	// ledger item 228's pairwise exporter, and it is Export's rows one scope over: one asks for
	// KDF.Nh octets, so the answer is the same SIZE as the secret guardrail 6 is looking for and
	// the comparison can mean something, and the other asks for a length that is not KDF.Nh so the
	// pair differs in the length alone.
	//
	// THE PEER LEAF IS SPLICED, because this sweep's group has one member and a pairwise key needs
	// two: driven at any leaf of a group of one the method answers ErrBlankLeaf, and a row that
	// errors hands this sweep no octets at all -- the shape Export's comment above refuses. The
	// splice is into the ratchet tree alone and it is the move the ProposeRemove row already
	// makes; epochSecretSweepInboundMessage is written against exactly that and says so.
	"PairwiseExport": func(t *testing.T, group *Group) [][]reflect.Value {
		nh := group.crypto.HashSize()
		peer := groupAnswerPairwiseLeaf(t, group)
		return [][]reflect.Value{
			{reflect.ValueOf("URmessage/v1/receipt-pair"), reflect.ValueOf(peer), reflect.ValueOf(nh)},
			{reflect.ValueOf("URmessage/v1/other-pair"), reflect.ValueOf(peer), reflect.ValueOf(nh + 8)},
		}
	},
	"EpochSecret": func(t *testing.T, group *Group) [][]reflect.Value {
		return [][]reflect.Value{
			{reflect.ValueOf(EpochSecretSenderData)},
			{reflect.ValueOf(EpochSecretEncryption)},
		}
	},
	// p7 task 13's commit generation, driven with the ONE row a group of one can be driven with.
	// An empty by-reference vector and no by-value proposals is the shape section 12.4 requires a
	// path for, so the answer carries a commit message, an update path inside it and the
	// post-commit tree -- every octet of which is derived from the epoch this commit OPENS, whose
	// parent secret is exactly what this sweep is looking for. One row and not two, because the
	// second call answers ErrPendingCommitExists and a row that errors is a row this sweep refuses
	// to read.
	"CreateCommit": func(t *testing.T, group *Group) [][]reflect.Value {
		return [][]reflect.Value{{
			reflect.ValueOf([][]byte{}),
			reflect.ValueOf([]Proposal(nil)),
			reflect.ValueOf((*CommitOptions)(nil)),
		}}
	},
	"MemberAt": func(t *testing.T, group *Group) [][]reflect.Value {
		return [][]reflect.Value{
			{reflect.ValueOf(group.OwnLeafIndex())},
			{reflect.ValueOf(LeafIndex(1))},
		}
	},
	// p7 task 18's three message methods that take arguments. Each is driven with the arguments a
	// caller would use rather than into a refusal, for Export's reason.
	//
	// Protect answers the ciphertext of whatever it was handed, which is where a leak into an
	// application message would land; the two inbound rows answer the plaintext of a message this
	// group can really open, and ProcessMessage's answer is a *Processed whose commit arm is a
	// holder this file sweeps in its own right. See epochSecretSweepInboundMessage for how a group
	// of one is given a message to open without the epoch this whole comparison rests on moving.
	"Protect": func(t *testing.T, group *Group) [][]reflect.Value {
		return [][]reflect.Value{{
			reflect.ValueOf([]byte("the aad this sweep protects")),
			reflect.ValueOf([]byte("the plaintext this sweep protects")),
		}}
	},
	// MASTER section 8.4.2 v2's bound seal, driven exactly as Protect is: the AAD is a BUILDER
	// rather than a value, so the row hands over a builder that answers the same octets whatever
	// generation it is called with -- the sweep is about what the ANSWER carries, and the
	// generation is not something epoch_secret could hide in.
	"ProtectBound": func(t *testing.T, group *Group) [][]reflect.Value {
		return [][]reflect.Value{{
			reflect.ValueOf(func(generation uint32) ([]byte, error) {
				return []byte("the aad this sweep protects"), nil
			}),
			reflect.ValueOf([]byte("the plaintext this sweep protects")),
		}}
	},
	"ProcessMessage": func(t *testing.T, group *Group) [][]reflect.Value {
		return [][]reflect.Value{{reflect.ValueOf(epochSecretSweepInboundMessage(t, group,
			[]byte("the aad this sweep processes"), []byte("the plaintext this sweep processes")))}}
	},
	"Unprotect": func(t *testing.T, group *Group) [][]reflect.Value {
		return [][]reflect.Value{{reflect.ValueOf(epochSecretSweepInboundMessage(t, group,
			[]byte("the aad this sweep unprotects"), []byte("the plaintext this sweep unprotects")))}}
	},
	// p7 task 12's three generators that take arguments; ProposeUpdate takes none and is called
	// directly. Each is driven with the arguments a caller would use rather than into a refusal,
	// for Export's reason: a call that answers an error hands this sweep no bytes at all, so a
	// method swept over a refusal is a method swept in name only.
	"ProposeAdd": func(t *testing.T, group *Group) [][]reflect.Value {
		// THE JOINER'S KEYS COME OFF A PROVIDER OF ITS OWN AND NOT OFF THE GROUP'S, and that is
		// this row being driven with the arguments a caller would use rather than into a refusal.
		// The group swept here is founded over a fixedRandomProvider so that its epoch secret is
		// the corpus's -- see newGroupOverTheEpochSecret -- and every draw off that provider
		// answers ONE value, so a key package minted through it publishes the encryption key the
		// founding leaf already publishes. ProposeAdd refuses that as ValSem103's duplicate, and a
		// row driven into a refusal hands this sweep no bytes at all. The suite is the group's, so
		// the joiner is a member this group could really admit.
		joinerCrypto, err := NewCryptoProvider(group.crypto.Suite())
		if err != nil {
			t.Fatalf("NewCryptoProvider for the joiner this sweep adds: %v", err)
		}
		kp, _, _ := testKeyPackage(t, joinerCrypto,
			testIdentity(t, joinerCrypto, "the joiner this sweep adds"))
		encoded, err := syntax.Marshal(kp)
		if err != nil {
			t.Fatalf("marshal the key package this sweep adds: %v", err)
		}
		return [][]reflect.Value{{reflect.ValueOf(encoded)}}
	},
	"ProposeRemove": func(t *testing.T, group *Group) [][]reflect.Value {
		// a remove names a leaf that is not our own, and the group this sweep reads has one
		// member, so a second is spliced into the ratchet tree here.
		//
		// ITS KEYS COME OFF A PROVIDER OF ITS OWN, for the reason the add row's do one row up: a
		// leaf minted through this group's fixedRandomProvider publishes the encryption key the
		// founding leaf already publishes, which is a tree no group could reach -- ValSem103 and
		// ValSem110 both refuse it -- and the ProposeUpdate row that runs after this one is then
		// driven into a refusal by the tree this row left behind.
		memberCrypto, err := NewCryptoProvider(group.crypto.Suite())
		if err != nil {
			t.Fatalf("NewCryptoProvider for the member this sweep removes: %v", err)
		}
		leaf, _ := testLeafNode(t, memberCrypto,
			testIdentity(t, memberCrypto, "the member this sweep removes"))
		at, err := group.tree.AddLeaf(leaf)
		if err != nil {
			t.Fatalf("splice a second leaf into the group this sweep reads: %v", err)
		}
		return [][]reflect.Value{{reflect.ValueOf(at)}}
	},
	"ProposeGroupContextExtensions": func(t *testing.T, group *Group) [][]reflect.Value {
		// the group's own published extensions, which is the smallest wholesale replacement a
		// real caller could send: one that still carries the policy the group runs under
		return [][]reflect.Value{{reflect.ValueOf(testGroupContextOf(t, group).Extensions)}}
	},
}

// answersOnlyErrors is the signature half of the eraser filter below: a method with no result at
// all, or whose every result is an error, hands out no bytes for a sweep to read.
func answersOnlyErrors(signature reflect.Type) bool {
	errorInterface := reflect.TypeOf((*error)(nil)).Elem()
	for i := range signature.NumOut() {
		if signature.Out(i) != errorInterface {
			return false
		}
	}
	return true
}

// bytesTheGroupHandsOut is every byte slice reachable through *Group's own exported surface.
//
// It is bytesTheScheduleHandsOut one type over, and it exists because p7 task 11 made Group the
// second type of this package that can reach an epoch secret: it holds the schedule, so every
// exported method it declares is a place the schedule's storage could be answered from, and
// reflection over the compiled method set is what makes a method added tomorrow join by existing.
//
// THE ERASER IS SKIPPED for the reason the schedule's Zeroize is, and the shape is one result
// wider. Close answers an error and nothing else -- there is no byte in that signature for a
// secret to hide in -- and it sorts FIRST of this type's exported methods, so a sweep that called
// it would read a zeroized epoch through every method after it and report exactly the clean run a
// complete sweep reports. The criterion is the signature and not the name, so a second eraser
// joins the skip by having the shape rather than by being listed.
func bytesTheGroupHandsOut(t *testing.T, at string, group *Group) []exposedSlice {
	t.Helper()
	groupType := reflect.TypeOf(group)
	valueType := groupType.Elem()
	for i := range valueType.NumField() {
		if valueType.Field(i).IsExported() {
			t.Fatalf("%s: Group has exported field %s, so its storage is reachable without going through a method this sweep reads",
				at, valueType.Field(i).Name)
		}
	}
	exposed := []exposedSlice{}
	skipped := []string{}
	for i := range groupType.NumMethod() {
		method := groupType.Method(i)
		if method.Type.NumIn() == 1 && answersOnlyErrors(method.Type) {
			skipped = append(skipped, method.Name)
			continue
		}
		rows := [][]reflect.Value{nil}
		if method.Type.NumIn() != 1 {
			build, driven := groupMethodArgumentRows[method.Name]
			if !driven {
				reason, excused := groupMethodsTakingArguments[method.Name]
				if !excused {
					t.Fatalf("%s: (*Group).%s takes arguments and this sweep calls with none; give it rows in groupMethodArgumentRows or write down in groupMethodsTakingArguments why it cannot surface epoch_secret",
						at, method.Name)
				}
				t.Logf("%s: (*Group).%s not swept: %s", at, method.Name, reason)
				continue
			}
			rows = build(t, group)
			if len(rows) == 0 {
				t.Fatalf("%s: groupMethodArgumentRows drives (*Group).%s with no rows at all, so it is swept in name only",
					at, method.Name)
			}
			for _, row := range rows {
				if len(row)+1 != method.Type.NumIn() {
					t.Fatalf("%s: groupMethodArgumentRows drives (*Group).%s with %d arguments and it takes %d",
						at, method.Name, len(row), method.Type.NumIn()-1)
				}
				for argument, value := range row {
					if want := method.Type.In(argument + 1); !value.Type().AssignableTo(want) {
						t.Fatalf("%s: groupMethodArgumentRows hands (*Group).%s a %s in argument %d, which takes %s",
							at, method.Name, value.Type(), argument, want)
					}
				}
			}
		} else if _, driven := groupMethodArgumentRows[method.Name]; driven {
			t.Errorf("groupMethodArgumentRows drives %s, which takes no arguments", method.Name)
		}
		for _, row := range rows {
			for index, result := range method.Func.Call(append([]reflect.Value{reflect.ValueOf(group)}, row...)) {
				for _, one := range exposedByteSlices(t, "(*Group)."+method.Name, result) {
					one.method = method.Name
					one.result = index
					one.taken = bytes.Clone(one.bytes)
					exposed = append(exposed, one)
				}
			}
		}
	}
	for name := range groupMethodArgumentRows {
		if _, found := groupType.MethodByName(name); !found {
			t.Errorf("groupMethodArgumentRows drives %s, which *Group does not declare", name)
		}
	}
	for name := range groupMethodsTakingArguments {
		if _, found := groupType.MethodByName(name); !found {
			t.Errorf("groupMethodsTakingArguments excuses %s, which *Group does not declare", name)
		}
		if _, driven := groupMethodArgumentRows[name]; driven {
			t.Errorf("%s is both excused from this sweep and driven through it, so which one holds depends on the order these two maps are read",
				name)
		}
	}
	// the skip is a positive control on itself: this type declares an eraser, so a filter that
	// had stopped matching one would sweep it and read zeros everywhere after it
	if len(skipped) == 0 {
		t.Fatalf("%s: no exported method of *Group was read as an eraser, and Close is one; the signature filter is matching nothing and this sweep is reading a closed group",
			at)
	}
	// and it is PRINTED as well as counted. A non-empty check says the exclusion fired; it
	// does not say WHAT it removed, and a member that quietly joined the removed set is
	// exactly the fault GATES.md indexes this line for.
	t.Logf("%s: the %d exported methods of *Group this sweep removes, because they take nothing and answer only errors and so hand nothing out, are %v",
		at, len(skipped), skipped)
	for _, one := range exposed {
		if !bytes.Equal(one.bytes, one.taken) {
			t.Fatalf("%s: %s changed after this sweep read it, so what it collected is not what those methods answered and every comparison over it runs against the rewrite",
				at, one.path)
		}
	}
	return exposed
}

// fixedRandomProvider answers one chosen value for every draw of that value's width.
type fixedRandomProvider struct {
	CryptoProvider
	value []byte
}

func (self *fixedRandomProvider) Random(n int) []byte {
	if n == len(self.value) {
		return bytes.Clone(self.value)
	}
	return self.CryptoProvider.Random(n)
}

// newGroupOverTheEpochSecret founds a group whose epoch secret IS the one this corpus epoch
// publishes, which is the whole of what makes the NewGroup row below a live comparison.
//
// Without it that row could not fail. A group draws its epoch secret at random, so whatever its
// exported surface answered could never equal a value the corpus names, and the row would report
// the clean run a Group that handed its epoch secret out would also report. Fixing the draw is the
// only way to put a KNOWN secret inside a construction that chooses its own.
//
// The v1 suite whatever the corpus epoch runs, because the profile refuses to create a group under
// 0x0001 -- and both registered suites have KDF.Nh 32, so the value handed over is the right width
// for the schedule NewGroup builds and the comparison is over the same octets either way.
func newGroupOverTheEpochSecret(t *testing.T, epoch ksVectorEpoch) (*Group, error) {
	t.Helper()
	base, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider for the group this row founds: %v", err)
	}
	owner := testIdentity(t, base, "owner")
	crypto := &fixedRandomProvider{CryptoProvider: base, value: epochSecretOfTheEpoch(t, epoch)}
	group, err := NewGroup(testGroupConfig(t, crypto, owner, "group"+epoch.at),
		owner.SigPriv, BasicCredential(owner.IdentityPub))
	if err != nil || group == nil {
		return group, err
	}
	// AND THE FIXED DRAW IS PUT BACK ONCE THE CONSTRUCTION IS OVER, which is the scope the
	// paragraph above argues for: the draw is fixed so that NewGroup derives a KNOWN epoch
	// secret, and that is the whole of what it is for. Left standing, EVERY later draw of that
	// width answers the same value -- so the update this sweep drives republishes the encryption
	// key its own leaf already holds, which is the one thing RFC 9420 section 12.1.2 states about
	// an Update's leaf and which (*Group).propose now refuses at generation. A row driven into a
	// refusal hands this sweep no bytes at all.
	//
	// Nothing G6 reads moves with it. The epoch secret this gate compares against is the one the
	// schedule already derived and still holds; what changes is only where the group's LATER keys
	// come from, and a method that hands out epoch_secret hands it out of that schedule.
	group.crypto = base
	return group, nil
}

// restoredGroupOverTheEpochSecret founds a group over this corpus epoch's own epoch secret,
// persists it -- which NewGroup does for itself -- and reads it back through LoadGroup.
//
// It is newGroupOverTheEpochSecret with a round trip on the end, and the round trip is what makes
// the LoadGroup row a live comparison: a restore of a randomly founded group would answer secrets
// this corpus does not name, so whatever its exported surface handed out could never equal the
// value the gate compares against, and the row could not fail.
//
// The fixed draw is put back before the restore, for newGroupOverTheEpochSecret's stated reason:
// it is there so the FOUNDING derives a known epoch secret and for nothing else. Nothing G6 reads
// moves with it -- the epoch secret this gate compares against is the one the restored schedule
// rebuilt out of the persisted state.
func restoredGroupOverTheEpochSecret(t *testing.T, epoch ksVectorEpoch) (*Group, error) {
	t.Helper()
	base, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider for the group this row restores: %v", err)
	}
	owner := testIdentity(t, base, "owner")
	crypto := &fixedRandomProvider{CryptoProvider: base, value: epochSecretOfTheEpoch(t, epoch)}
	cfg := testGroupConfig(t, crypto, owner, "restore"+epoch.at)
	group, err := NewGroup(cfg, owner.SigPriv, BasicCredential(owner.IdentityPub))
	if err != nil || group == nil {
		return group, err
	}
	epochAt := group.Epoch()
	group.Close()
	cfg.Crypto = base
	return LoadGroup(cfg, epochAt, owner.SigPriv)
}

// fixedEpochSecretProvider answers one chosen value for the "epoch" expansion, and for nothing
// else.
//
// It is fixedRandomProvider's trick one derivation further in. A JOINER does not draw its epoch
// secret -- it expands one from the joiner secret a Welcome handed it -- so a fixed Random cannot
// put a known value inside the group the row below builds. What both halves of a join DO share is
// ExpandWithLabel(member_secret, "epoch", GroupContext, KDF.Nh): the committer's key schedule
// expands it and the joiner's expands it again, so fixing that one label puts the SAME known epoch
// secret on both sides and the two go on agreeing about everything derived from it.
type fixedEpochSecretProvider struct {
	CryptoProvider
	value []byte
}

func (self *fixedEpochSecretProvider) ExpandWithLabel(secret []byte, label string, context []byte,
	length int) []byte {

	if label == "epoch" && length == len(self.value) {
		return bytes.Clone(self.value)
	}
	return self.CryptoProvider.ExpandWithLabel(secret, label, context, length)
}

// joinedGroupOverTheEpochSecret joins a group whose epoch secret IS the one this corpus epoch
// publishes, which is what makes the JoinFromWelcome row below a live comparison.
//
// Without it the row could not fail, for newGroupOverTheEpochSecret's reason: the joiner secret a
// real commit produces is a function of a random epoch, so whatever the joined group's exported
// surface answered could never equal a value the corpus names.
//
// The fixed expansion is put back once the join is over, exactly as the fixed draw is next door:
// it is there so the construction derives a KNOWN epoch secret and for nothing else, and left
// standing every later epoch of that group would expand to the same value. Nothing G6 reads moves
// with it -- the epoch secret this gate compares against is the one the schedule already holds.
func joinedGroupOverTheEpochSecret(t *testing.T, epoch ksVectorEpoch) (*Group, error) {
	t.Helper()
	base, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider for the group this row joins: %v", err)
	}
	crypto := &fixedEpochSecretProvider{CryptoProvider: base, value: epochSecretOfTheEpoch(t, epoch)}
	group, joiner, result, keys := joinTestCommit(t, crypto, "join"+epoch.at, nil)
	t.Cleanup(func() { group.Close() })
	if mergeErr := group.MergePendingCommit(); mergeErr != nil {
		t.Fatalf("merge the commit this row's welcome came from: %v", mergeErr)
	}
	joined, err := JoinFromWelcome(testGroupConfig(t, crypto, joiner, "join"+epoch.at),
		result.Welcome, result.RatchetTree, keys)
	if err != nil || joined == nil {
		return joined, err
	}
	joined.crypto = base
	return joined, nil
}

// epochSecretHolderSweeps is how this gate reads a value of a type that keeps the epoch
// secret. It is keyed by type name and checked against the derived closure in both
// directions, so a second holder landing in this package fails here until somebody teaches
// the sweep to read one rather than falling outside it in silence.
var epochSecretHolderSweeps = map[string]func(t *testing.T, at string, value reflect.Value) [][]byte{
	// p7 task 11's group, which reaches the epoch secret by holding the schedule that declares
	// it. Every byte it hands out goes through its own exported surface, read by reflection for
	// the reason the schedule's is: a leak added to this type tomorrow is a method, and a list of
	// accessors is exactly what would not have it.
	"Group": func(t *testing.T, at string, value reflect.Value) [][]byte {
		t.Helper()
		if value.IsNil() {
			return nil
		}
		group, isGroup := value.Interface().(*Group)
		if !isGroup {
			t.Fatalf("%s: the sweep was handed a %s where a *Group belongs", at, value.Type())
		}
		return exposedBytes(bytesTheGroupHandsOut(t, at, group))
	},
	// p7 task 13's staged commit, which reaches the epoch secret the same way the group does: it
	// holds the schedule of the epoch a commit OPENS, for the whole of the window between the
	// commit being sent and the delivery service accepting it. Read through its own exported
	// surface by reflection, for the group's reason -- a leak added to this type tomorrow is a
	// method, and a list of accessors is exactly what would not have it.
	// a named declaration rather than a literal, because the Processed sweep below hands its commit
	// arm to THIS reading rather than writing a second, narrower one of the same type -- and a map
	// literal that indexed itself would be an initialization cycle the compiler refuses.
	"StagedCommit": epochSecretBytesOfAStagedCommit,
	"KeySchedule": func(t *testing.T, at string, value reflect.Value) [][]byte {
		t.Helper()
		if value.IsNil() {
			return nil
		}
		schedule, isSchedule := value.Interface().(*KeySchedule)
		if !isSchedule {
			t.Fatalf("%s: the sweep was handed a %s where a *KeySchedule belongs", at, value.Type())
		}
		exposed, _ := bytesTheScheduleHandsOut(t, at, schedule)
		return exposedBytes(exposed)
	},
	// p7 task 18's result, which reaches the epoch secret by holding the staged commit that holds
	// the schedule of the epoch a received commit OPENS.
	//
	// IT IS READ THROUGH ITS EXPORTED FIELDS AND NOT THROUGH A METHOD SET, because it declares
	// none: it is a discriminated result and every arm of it is a field a caller reads directly.
	// The three sweeps above reflect over methods and assert their type has no exported field,
	// which is right for a type whose storage is reachable only through accessors and would be a
	// sweep over nothing here.
	//
	// The commit arm is handed to the STAGED COMMIT's own sweep rather than read field by field,
	// which is what keeps this entry from being a second, narrower reading of that type: an
	// accessor added to StagedCommit tomorrow joins by existing, on both routes.
	"Processed": func(t *testing.T, at string, value reflect.Value) [][]byte {
		t.Helper()
		if value.IsNil() {
			return nil
		}
		processed, isProcessed := value.Interface().(*Processed)
		if !isProcessed {
			t.Fatalf("%s: the sweep was handed a %s where a *Processed belongs", at, value.Type())
		}
		read := [][]byte{}
		if processed.Application != nil {
			read = append(read, processed.Application.AuthenticatedData, processed.Application.Plaintext)
		}
		if processed.Proposal != nil {
			// the proposal arm carries no []byte field of its own at this level, so it is read as
			// the octets it encodes to: a secret written into any arm of it lands in them
			encoded, err := syntax.Marshal(processed.Proposal)
			if err != nil {
				t.Fatalf("%s: encode the proposal this result carries: %v", at, err)
			}
			read = append(read, encoded)
		}
		if processed.Commit != nil {
			read = append(read, epochSecretBytesOfAStagedCommit(t, at, reflect.ValueOf(processed.Commit))...)
		}
		return read
	},
}

// epochSecretBytesOfAStagedCommit is the staged commit's entry in the table above, written as a
// declaration so that the Processed entry can reach the same reading. See that entry.
func epochSecretBytesOfAStagedCommit(t *testing.T, at string, value reflect.Value) [][]byte {
	t.Helper()
	if value.IsNil() {
		return nil
	}
	staged, isStaged := value.Interface().(*StagedCommit)
	if !isStaged {
		t.Fatalf("%s: the sweep was handed a %s where a *StagedCommit belongs", at, value.Type())
	}
	return exposedBytes(bytesTheStagedCommitHandsOut(t, at, staged))
}

// bytesTheAnswerHandsOut flattens everything one construction answered into the byte slices
// a caller can read off it.
//
// A holder is read through its own exported surface, which is what makes a construction
// answering a *KeySchedule cover the same ground as the reflection sweep. An error is
// required to be nil rather than read: every row here is built out of the published corpus
// and succeeds, so an error is this gate's own bug and not a place a secret hides.
func bytesTheAnswerHandsOut(t *testing.T, at string, name string, results []reflect.Value) [][]byte {
	t.Helper()
	errorInterface := reflect.TypeOf((*error)(nil)).Elem()
	exposed := [][]byte{}
	for _, result := range results {
		if result.Type() == errorInterface {
			if !result.IsNil() {
				t.Fatalf("%s%s answered %v, and the rows here are built out of the published corpus to succeed",
					name, at, result.Interface())
			}
			continue
		}
		held := result.Type()
		if held.Kind() == reflect.Pointer {
			held = held.Elem()
		}
		if sweep, isHolder := epochSecretHolderSweeps[held.Name()]; isHolder {
			exposed = append(exposed, sweep(t, at, result)...)
			continue
		}
		exposed = append(exposed, exposedBytes(exposedByteSlices(t, name+at, result))...)
	}
	return exposed
}

// epochSecretSurfaceRows calls each exported function that can reach an epoch secret and
// hands back what it answered.
//
// The keys are held to the derived class in both directions by the gate below. That is what
// closes the hole the type level sweep alone left: a package level
// func EpochSecretOf(schedule *KeySchedule) []byte compiles, is exported, is exactly the leak
// G6 forbids, and is nowhere in a reflection over (*KeySchedule)'s methods. Measured, not
// supposed: appended to key_schedule.go it left the whole of mls and message green.
//
// NewKeyScheduleFromEpochSecret is driven with the independently derived epoch secret itself
// rather than with an arbitrary sample, so the schedule under it holds the exact value the
// comparison is looking for.
var epochSecretSurfaceRows = map[string]func(t *testing.T, epoch ksVectorEpoch) []reflect.Value{
	"NewKeySchedule": func(t *testing.T, epoch ksVectorEpoch) []reflect.Value {
		schedule, err := NewKeySchedule(
			epoch.crypto, epoch.initPrev, epoch.commitSecret, epoch.pskSecret, epoch.groupContext)
		return []reflect.Value{reflect.ValueOf(schedule), reflect.ValueOf(&err).Elem()}
	},
	"NewKeyScheduleFromJoiner": func(t *testing.T, epoch ksVectorEpoch) []reflect.Value {
		joinerSecret := mustDecodeHex(t, "joiner_secret"+epoch.at, epoch.published.JoinerSecret)
		schedule, err := NewKeyScheduleFromJoiner(
			epoch.crypto, joinerSecret, epoch.pskSecret, epoch.groupContext)
		return []reflect.Value{reflect.ValueOf(schedule), reflect.ValueOf(&err).Elem()}
	},
	"NewKeyScheduleFromEpochSecret": func(t *testing.T, epoch ksVectorEpoch) []reflect.Value {
		schedule, err := NewKeyScheduleFromEpochSecret(
			epoch.crypto, epochSecretOfTheEpoch(t, epoch), epoch.groupContext)
		return []reflect.Value{reflect.ValueOf(schedule), reflect.ValueOf(&err).Elem()}
	},
	// p7 task 11's group creation, the first construction of this package to answer a holder that
	// is not a schedule. It is founded over this epoch's own epoch secret rather than over a fresh
	// draw, so the comparison the gate runs on what the group hands out is a live one -- see
	// newGroupOverTheEpochSecret for why a row over a randomly founded group could not fail.
	"NewGroup": func(t *testing.T, epoch ksVectorEpoch) []reflect.Value {
		group, err := newGroupOverTheEpochSecret(t, epoch)
		return []reflect.Value{reflect.ValueOf(group), reflect.ValueOf(&err).Elem()}
	},
	// p7 task 16's join, the second construction of this package to answer a group. It is driven
	// onto this epoch's own epoch secret for NewGroup's reason -- see joinedGroupOverTheEpochSecret
	// -- and it matters more here than there: a joiner derives epoch_secret from the joiner secret
	// a Welcome handed it, so a group that reached this package's exported surface out of a
	// message rather than out of a draw is exactly the shape G6 is about.
	"JoinFromWelcome": func(t *testing.T, epoch ksVectorEpoch) []reflect.Value {
		group, err := joinedGroupOverTheEpochSecret(t, epoch)
		return []reflect.Value{reflect.ValueOf(group), reflect.ValueOf(&err).Elem()}
	},
	// p7 task 19's restore, the THIRD construction of this package to answer a group and the one
	// that reaches the epoch secret from storage rather than from a draw or a message. It matters
	// here for a reason the other two do not have: what a restore rebuilds the schedule from is
	// the parent secret itself, written to a store and read back, so a group that came out of one
	// is a group whose epoch_secret this process has just held in a buffer. See
	// restoredGroupOverTheEpochSecret for why the state it reads back is this corpus epoch's own.
	"LoadGroup": func(t *testing.T, epoch ksVectorEpoch) []reflect.Value {
		group, err := restoredGroupOverTheEpochSecret(t, epoch)
		return []reflect.Value{reflect.ValueOf(group), reflect.ValueOf(&err).Elem()}
	},
}

// TestNoExportedFunctionOfThisPackageHandsOutTheEpochSecret is the other half of guardrail
// G6: what the PACKAGE hands out, rather than what the type does.
//
// The gate above sweeps (*KeySchedule)'s own reflected surface, and epoch_secret is an
// unexported field, so every symbol declared in package mls can read it — a free function
// most of all. Measured, not supposed: with
// func EpochSecretOf(schedule *KeySchedule) []byte { return schedule.epochSecret } appended
// to key_schedule.go, the whole of mls and message was green. The one other package wide
// gate that might have seen it, TestEveryConstructionInThisPackageLeavesItsInputAlone,
// collects the constructions that are HANDED bytes, and that one takes only a schedule.
//
// So the class here is derived rather than reflected: every exported package level function
// whose signature mentions a type that keeps the epoch secret, read off the parse tree, with
// the holder types themselves derived by closure from whichever struct declares the storage.
// A function added later joins by existing and fails this until somebody writes the row that
// reads what it answers.
func TestNoExportedFunctionOfThisPackageHandsOutTheEpochSecret(t *testing.T) {
	// the control first: the closure seeds, follows one hop, and the surface filter tells
	// an exported function over a holder from an unexported one and from an exported one
	// over something else
	control := mustParseText(t, "the epoch secret holder control", epochSecretHolderControl)
	controlStructs := map[string]*ast.StructType{}
	structTypesIn(control, controlStructs)
	controlHolders := theTypesHoldingTheEpochSecret(controlStructs)
	if want := []string{"Holder", "Wrapper"}; !slices.Equal(controlHolders, want) {
		t.Fatalf("the closure read %v out of the control as holding the epoch secret, want %v; it is not seeding on the storage or not following a reference to it",
			controlHolders, want)
	}
	// and its SEED, which is what the three gates over the declarer read. A seed that had started
	// following the reference would point those three at a type holding none of the storage they
	// are written over, and one that had stopped reading the field leaves all five gates empty.
	if declaring := theTypesDeclaringTheEpochSecret(controlStructs); !slices.Equal(declaring, []string{"Holder"}) {
		t.Fatalf("the seed of that closure read %v out of the control as DECLARING the epoch secret, want [Holder]",
			declaring)
	}
	controlSurface := theExportedSurfaceReaching([]parsedSource{control}, controlHolders)
	wantSurface := []string{"ExportedAnsweringAHolder", "ExportedOverTheHolder", "ExportedOverTheWrapper"}
	if !slices.Equal(controlSurface, wantSurface) {
		t.Fatalf("the surface filter read %v out of the control, want %v; it is not reading both directions of a signature, or not telling exported from not",
			controlSurface, wantSurface)
	}
	if standing := packageLevelValuesNaming([]parsedSource{control}, controlHolders); !slices.Equal(standing, []string{"TheStandingHolder"}) {
		t.Fatalf("the package level value scan read %v out of the control, want [TheStandingHolder]", standing)
	}

	// then this package's own source
	structs := map[string]*ast.StructType{}
	files := []parsedSource{}
	for _, path := range packageLevelFunctions(t).files {
		parsed := mustParseSource(t, path)
		files = append(files, parsed)
		structTypesIn(parsed, structs)
	}
	holders := theTypesHoldingTheEpochSecret(structs)
	if len(holders) == 0 {
		t.Fatalf("no struct of this package's source declares a field named %s, so the class below is empty and this gate demands nothing; if the storage was renamed, rename it here too",
			epochSecretStorageField)
	}
	for _, holder := range holders {
		if _, known := epochSecretHolderSweeps[holder]; !known {
			t.Fatalf("%s keeps the epoch secret and this gate has no way to read a value of one; add it to epochSecretHolderSweeps rather than letting a second holder fall outside G6",
				holder)
		}
	}
	for name := range epochSecretHolderSweeps {
		if !slices.Contains(holders, name) {
			t.Errorf("epochSecretHolderSweeps reads a %s and no struct of this package keeps the epoch secret under that name", name)
		}
	}
	if standing := packageLevelValuesNaming(files, holders); len(standing) != 0 {
		t.Errorf("%v are package level values of a type that keeps the epoch secret, so one is reachable with no call for this sweep to make",
			standing)
	}

	surface := theExportedSurfaceReaching(files, holders)
	if len(surface) == 0 {
		t.Fatalf("no exported function of this package mentions %v, and this package declares three constructors that answer one, so the scan is reading nothing",
			holders)
	}
	for _, name := range surface {
		if _, covered := epochSecretSurfaceRows[name]; !covered {
			t.Errorf("%s is exported and its signature carries %v, so it can hand out epoch_secret and nothing here calls it; write it a row in epochSecretSurfaceRows",
				name, holders)
		}
	}
	for name := range epochSecretSurfaceRows {
		if !slices.Contains(surface, name) {
			t.Errorf("epochSecretSurfaceRows calls %s, which is not an exported function of this package reaching %v", name, holders)
		}
	}
	t.Logf("guardrail 6 swept over %v, holders %v", surface, holders)

	for _, epoch := range ksVectorEpochs(t) {
		epochSecret := epochSecretOfTheEpoch(t, epoch)
		readAcrossTheRows := [][]byte{}
		for _, name := range surface {
			row, covered := epochSecretSurfaceRows[name]
			if !covered {
				continue
			}
			results, raised := recoveringRow(func() []reflect.Value { return row(t, epoch) })
			if raised != nil {
				t.Errorf("%s%s panicked with %v rather than answering", name, epoch.at, raised)
				continue
			}
			exposed := bytesTheAnswerHandsOut(t, epoch.at, name, results)
			// a row that answered nothing observed nothing, and reports the clean run a
			// row that answered everything reports
			if !slices.ContainsFunc(exposed, func(b []byte) bool { return len(b) != 0 }) {
				t.Errorf("%s%s answered no bytes at all, so this row read nothing to compare",
					name, epoch.at)
				continue
			}
			for index, secret := range exposed {
				if bytes.Equal(secret, epochSecret) {
					t.Errorf("%s%s hands out epoch_secret at position %d of %d; it is the parent of every secret of this epoch and G6 says no exported symbol of this package returns it",
						name, epoch.at, index, len(exposed))
				}
			}
			readAcrossTheRows = append(readAcrossTheRows, exposed...)
		}
		// and the control on the flattener rather than on any one row: a secret these
		// constructions certainly hand out has to come back out of it, or a secret they
		// must not hand out would be missed the same way
		known := epoch.schedule(t).Secrets().InitSecret
		if !slices.ContainsFunc(readAcrossTheRows, func(b []byte) bool { return bytes.Equal(b, known) }) {
			t.Fatalf("%s: the sweep read %d byte slices off %v and init_secret is not among them, so it is not reading what it claims to",
				epoch.at, len(readAcrossTheRows), surface)
		}
	}
}

// sourceDeclaration is one function or method this package's own source declares, with the
// receiver it is written on and the shapes a scan below reads.
//
// Methods travel with functions because the class these build is "what can reach the epoch
// secret", and a method reaches it through a helper function exactly as a function reaches it
// through a helper method. A scan that read only one of the two would report a leak spelled
// through the other as absent.
type sourceDeclaration struct {
	receiver string
	name     string
	exported bool
	params   []string
	// paramNames travels beside params because a scan that reads what a declaration DOES with
	// an argument has to be able to find that argument in the body, and a rendered type cannot
	// be found there. An unnamed or blank parameter is carried as the empty string, so a caller
	// can tell "named and unused" from "not named at all" rather than guessing.
	paramNames []string
	results    []string
	body       *ast.BlockStmt
}

// declaredIn is every function and method of one parsed file, with its parameter and result
// types rendered.
//
// Rendered rather than compared as syntax, for the reason packageLevelFunctionsIn renders:
// func f(a, b []byte) and func f(a []byte, b []byte) are the same signature to the compiler
// and a filter over either spelling has to read them the same way.
func declaredIn(parsed parsedSource) []sourceDeclaration {
	declared := []sourceDeclaration{}
	for _, declaration := range parsed.file.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction {
			continue
		}
		one := sourceDeclaration{
			receiver:   parsed.receiverOf(function),
			name:       function.Name.Name,
			exported:   function.Name.IsExported(),
			params:     []string{},
			paramNames: []string{},
			results:    []string{},
			body:       function.Body,
		}
		if function.Type.Params != nil {
			for _, field := range function.Type.Params.List {
				one.params = append(one.params,
					slices.Repeat([]string{parsed.render(field.Type)}, max(len(field.Names), 1))...)
				if len(field.Names) == 0 {
					one.paramNames = append(one.paramNames, "")
					continue
				}
				for _, named := range field.Names {
					one.paramNames = append(one.paramNames, named.Name)
				}
			}
		}
		if function.Type.Results != nil {
			for _, field := range function.Type.Results.List {
				one.results = append(one.results,
					slices.Repeat([]string{parsed.render(field.Type)}, max(len(field.Names), 1))...)
			}
		}
		declared = append(declared, one)
	}
	return declared
}

// declaredAcross is the same over several files.
func declaredAcross(files []parsedSource) []sourceDeclaration {
	declared := []sourceDeclaration{}
	for _, parsed := range files {
		declared = append(declared, declaredIn(parsed)...)
	}
	return declared
}

// theNamesReachingTheStorage is every declared name whose body can reach the named storage
// epochSecretStorageField names: the ones that mention it, closed over the names they call.
//
// One identifier check covers every spelling go has for that storage. ast.Inspect descends
// into a selector's own Sel and into a composite literal's key, so self.epochSecret, the
// epochSecret field of a literal and a local or parameter carrying it are all an *ast.Ident of
// that name. A matcher written against the selector alone would read the first and miss the
// other two.
//
// The closure is over NAMES rather than over resolved callees, which over-approximates: a call
// x.Foo() joins whatever Foo this package declares, on whichever receiver. That is the safe
// direction for a gate -- it can demand too much, and says so by failing with the name it
// objected to -- where a call graph that resolved too little reports exactly the clean run a
// complete one reports. A method value taken without a call is an identifier too, so
// f := self.leak handed somewhere else is read by the same pass.
func theNamesReachingTheStorage(declared []sourceDeclaration, storage string) []string {
	mentions := func(body *ast.BlockStmt, wanted map[string]bool) bool {
		if body == nil {
			return false
		}
		found := false
		ast.Inspect(body, func(node ast.Node) bool {
			if identifier, isIdentifier := node.(*ast.Ident); isIdentifier && wanted[identifier.Name] {
				found = true
			}
			return !found
		})
		return found
	}
	reaching := map[string]bool{storage: true}
	for {
		grew := false
		for _, one := range declared {
			if reaching[one.name] {
				continue
			}
			if mentions(one.body, reaching) {
				reaching[one.name] = true
				grew = true
			}
		}
		if !grew {
			delete(reaching, storage)
			return slices.Sorted(maps.Keys(reaching))
		}
	}
}

// theExportedMethodsHandingOutWhatTheyReach is every exported method of the parsed files that
// can reach the epoch secret AND has somewhere to put it: a result that is not an error, or a
// byte slice parameter it could write through.
//
// The second half is what makes this a property rather than a list of allowed names. An epoch
// has one legitimate reason for an exported method to touch its parent secret -- erasing it --
// and such a method answers nothing and is handed nothing, so it falls outside this class by
// its own shape rather than by an exemption somebody wrote. Everything else that reaches the
// storage is a way out for it.
//
// The receiver travels with the name because the closure resolves callees by name: two types
// declaring a method of one spelling are reported together, and a reader who is told "Export"
// cannot tell which. That over-report is the price of the safe direction, and naming both is
// what keeps it readable rather than mysterious.
// hasSomewhereToPutASecret is the shape half of both G6's method gate and the erased epoch
// gate, written once because the two ask the same question of the same declarations.
//
// A declaration can only hand a secret OUT through a result that is not an error, or through a
// byte slice its caller handed in and it can write over. An eraser reaches every secret the
// epoch holds -- reaching them is what erasing IS -- and is handed nothing and answers
// nothing, so it falls outside both classes by its own shape rather than by a name written
// down anywhere. Zeroize is the first declaration of this package in that position, and a
// second spelling of this rule would be a second thing that could stop agreeing with the first.
func hasSomewhereToPutASecret(one sourceDeclaration, byteSlices []string) bool {
	if slices.ContainsFunc(one.results, func(result string) bool { return result != "error" }) {
		return true
	}
	return slices.ContainsFunc(one.params, func(parameter string) bool {
		return slices.Contains(byteSlices, parameter)
	})
}

func theExportedMethodsHandingOutWhatTheyReach(declared []sourceDeclaration, byteSlices []string) []string {
	reaching := theNamesReachingTheStorage(declared, epochSecretStorageField)
	handingOut := []string{}
	for _, one := range declared {
		if one.receiver == "" || !one.exported || !slices.Contains(reaching, one.name) {
			continue
		}
		if hasSomewhereToPutASecret(one, byteSlices) {
			handingOut = append(handingOut, "("+one.receiver+")."+one.name)
		}
	}
	slices.Sort(handingOut)
	return handingOut
}

// epochSecretMethodControl declares one of each shape the derivation has to tell apart: a
// method that reads the storage outright, one that reads it through an unexported method of
// its own, one that reads it through a package level function, one that writes it into an
// array the caller handed in, the erase shape that reaches it and answers nothing, one that
// reaches it and answers an error alone, one that reaches nothing, and an unexported one that
// reads it.
//
// Without it a derivation that had stopped deriving -- a seed that no longer matched, a
// closure that never followed a call -- would report an empty class, and an empty class is
// exactly the clean run a complete one produces.
const epochSecretMethodControl = "package control\n" +
	"\n" +
	"type Holder struct {\n" +
	"\tepochSecret []byte\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) LeaksItDirectly() []byte {\n" +
	"\treturn self.epochSecret\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) LeaksItThroughAMethod() []byte {\n" +
	"\treturn self.reader()\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) reader() []byte {\n" +
	"\treturn self.epochSecret\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) LeaksItThroughAFunction() ([]byte, error) {\n" +
	"\treturn theSecretOf(self), nil\n" +
	"}\n" +
	"\n" +
	"func theSecretOf(holder *Holder) []byte {\n" +
	"\treturn holder.epochSecret\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) LeaksItIntoTheCallersArray(out []byte) {\n" +
	"\tcopy(out, self.epochSecret)\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) Zeroize() {\n" +
	"\tfor i := range self.epochSecret {\n" +
	"\t\tself.epochSecret[i] = 0\n" +
	"\t}\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) AnswersAnErrorAlone() error {\n" +
	"\tif len(self.epochSecret) == 0 {\n" +
	"\t\treturn nil\n" +
	"\t}\n" +
	"\treturn nil\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) ReachesNothing() []byte {\n" +
	"\treturn nil\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) leaksItAndIsUnexported() []byte {\n" +
	"\treturn self.epochSecret\n" +
	"}\n"

// epochSecretHolderTypes is the compiled type behind each source type that keeps the epoch
// secret, so a source reading of a holder's surface can be compared against a reflection of
// the same type. It is checked against the derived closure in both directions, like
// epochSecretHolderSweeps, so a second holder cannot land here unread.
var epochSecretHolderTypes = map[string]reflect.Type{
	"Group":        reflect.TypeOf((*Group)(nil)),
	"KeySchedule":  reflect.TypeOf((*KeySchedule)(nil)),
	"Processed":    reflect.TypeOf((*Processed)(nil)),
	"StagedCommit": reflect.TypeOf((*StagedCommit)(nil)),
}

// TestNoExportedMethodOfThisPackageCanReachTheEpochSecret is the half of guardrail G6 that
// covers a method whose ARGUMENTS a sweep cannot exhaust.
//
// The two gates above read what a call ANSWERS and compare it against epoch_secret. For a
// method that takes no arguments that is complete: reflection calls it the one way it can be
// called and there is no second way. The moment Export joined them it stopped being complete,
// because "the exporter cannot be made to answer epoch_secret" is a claim over every (label,
// context, length) a caller may choose and keyScheduleMethodArgumentRows supplies three of
// them. Measured, not supposed: with
//
//	if label == "recovery" { return bytes.Clone(self.epochSecret), nil }
//
// inserted at the top of Export, the whole of mls and message was green, and a direct probe
// confirmed Export("recovery", nil, 32) then answered epoch_secret verbatim. Sampling an
// unbounded argument space is Standing Rule 5's shape one level in: the class is the
// arguments, and seventeen of them had been enumerated.
//
// So this gate reads the SOURCE, where the argument space does not appear at all. An exported
// method that never mentions the storage cannot answer it under any label, and the class of
// "mentions the storage" is derived by closure over the package's own call graph rather than
// written down -- a leak spelled through a helper is a leak.
//
// It covers every exported method of the package rather than the holder's own, because a
// method on some other type that takes a *KeySchedule reaches the same unexported field and is
// nowhere in a reflection over (*KeySchedule). The two behavioural gates stay: this one says
// the bytes cannot travel, and they say the bytes that DO travel are not the secret.
func TestNoExportedMethodOfThisPackageCanReachTheEpochSecret(t *testing.T) {
	// the control first, on both halves: the closure follows a call into a method and into a
	// function, and the shape filter tells a method with somewhere to put the secret from the
	// erase that has nowhere
	control := []parsedSource{mustParseText(t, "the epoch secret method control", epochSecretMethodControl)}
	controlDeclarations := declaredAcross(control)
	wantReaching := []string{
		"AnswersAnErrorAlone",
		"LeaksItDirectly",
		"LeaksItIntoTheCallersArray",
		"LeaksItThroughAFunction",
		"LeaksItThroughAMethod",
		"Zeroize",
		"leaksItAndIsUnexported",
		"reader",
		"theSecretOf",
	}
	if reaching := theNamesReachingTheStorage(controlDeclarations, epochSecretStorageField); !slices.Equal(reaching, wantReaching) {
		t.Fatalf("the closure read %v out of the control as reaching the epoch secret, want %v; it is not seeding on the storage or not following a call",
			reaching, wantReaching)
	}
	wantHandingOut := []string{
		"(*Holder).LeaksItDirectly",
		"(*Holder).LeaksItIntoTheCallersArray",
		"(*Holder).LeaksItThroughAFunction",
		"(*Holder).LeaksItThroughAMethod",
	}
	if handingOut := theExportedMethodsHandingOutWhatTheyReach(controlDeclarations, []string{"[]byte"}); !slices.Equal(handingOut, wantHandingOut) {
		t.Fatalf("the shape filter read %v out of the control, want %v; it is not telling an answer from an erase, or not telling exported from not",
			handingOut, wantHandingOut)
	}

	// then this package's own source
	files := []parsedSource{}
	for _, path := range packageLevelFunctions(t).files {
		files = append(files, mustParseSource(t, path))
	}
	declared := declaredAcross(files)
	reaching := theNamesReachingTheStorage(declared, epochSecretStorageField)
	// the positive control on the real source: this package certainly assembles an epoch out
	// of a parent secret, and a derivation that had stopped deriving would say it does not
	if len(reaching) == 0 {
		t.Fatalf("no declaration of this package's source mentions %s, so the class below is empty and this gate demands nothing; if the storage was renamed, rename it in epochSecretStorageField too",
			epochSecretStorageField)
	}
	byteSlices := slices.Concat([]string{"[]byte"}, packageByteSliceTypeNames(t))
	handingOut := []string{}
	for _, name := range theExportedMethodsHandingOutWhatTheyReach(declared, byteSlices) {
		if _, swept := epochSecretMethodsTheSweepDrivesInstead[name]; swept {
			continue
		}
		handingOut = append(handingOut, name)
	}
	if len(handingOut) != 0 {
		t.Errorf("%v are exported methods that can reach %s and have somewhere to put it -- a result that is not an error, or a byte slice to write through; G6 says no exported symbol of this package returns the parent secret, and no sweep over a method's arguments can rule out the label it answers under",
			handingOut, epochSecretStorageField)
	}
	// and the exemption expires by failing: an entry the source reading no longer reports is a
	// line to delete, and one whose method the behavioural sweep does not drive is an exemption
	// resting on a sweep that is not running
	reported := theExportedMethodsHandingOutWhatTheyReach(declared, byteSlices)
	for name, why := range epochSecretMethodsTheSweepDrivesInstead {
		if !slices.Contains(reported, name) {
			t.Errorf("%s is exempted from this source reading (%s) and the reading no longer reports it; delete the entry",
				name, why)
		}
		bare := name[strings.LastIndex(name, ".")+1:]
		method, declaredOnGroup := reflect.TypeOf((*Group)(nil)).MethodByName(bare)
		if !declaredOnGroup {
			t.Errorf("%s is exempted here and *Group declares no method of that name, so the sweep this entry rests on drives nothing",
				name)
			continue
		}
		if _, driven := groupMethodArgumentRows[bare]; !driven && method.Type.NumIn() != 1 {
			t.Errorf("%s is exempted here because the behavioural sweep drives it, and groupMethodArgumentRows drives nothing of that name",
				name)
		}
	}
	t.Logf("%d declarations of this package reach %s: %v", len(reaching), epochSecretStorageField, reaching)

	// and the source reading covers every exported method the compiled holder has. An embedded
	// field's methods are promoted onto the type and declared somewhere else entirely, so a
	// holder that grew an embedded surface would be swept by the reflection gate next door and
	// invisible to this one.
	structs := map[string]*ast.StructType{}
	for _, parsed := range files {
		structTypesIn(parsed, structs)
	}
	holders := theTypesHoldingTheEpochSecret(structs)
	for _, holder := range holders {
		if _, known := epochSecretHolderTypes[holder]; !known {
			t.Fatalf("%s keeps the epoch secret and epochSecretHolderTypes has no compiled type for it, so this gate cannot check that its source reading is complete",
				holder)
		}
	}
	for name := range epochSecretHolderTypes {
		if !slices.Contains(holders, name) {
			t.Errorf("epochSecretHolderTypes names a %s and no struct of this package keeps the epoch secret under that name", name)
		}
	}
	for _, holder := range holders {
		compiled := epochSecretHolderTypes[holder]
		reflected := []string{}
		for i := range compiled.NumMethod() {
			reflected = append(reflected, compiled.Method(i).Name)
		}
		slices.Sort(reflected)
		inSource := []string{}
		for _, one := range declared {
			if one.exported && (one.receiver == holder || one.receiver == "*"+holder) {
				inSource = append(inSource, one.name)
			}
		}
		slices.Sort(inSource)
		if !slices.Equal(reflected, inSource) {
			t.Errorf("%s has exported methods %v compiled and %v declared in this package's source; the difference is promoted from an embedded field and this source gate never reads it",
				holder, reflected, inSource)
		}
	}
}

// TestNewKeyScheduleErasesTheMemberSecret observes the sentence key_schedule.go writes above
// NewKeyScheduleFromJoiner: member_secret is erased once it has produced the two things it
// feeds.
//
// member_secret is not one of the epoch's secrets and nothing downstream reads it, and it
// reproduces welcome_secret and epoch_secret — which is to say all nine — from one
// HKDF-Expand each. Deleting the zeroizeSecret call changes no answer this package
// computes, so nothing but a read of that storage sees it.
//
// The control is the same one TestDeriveJoinerSecretErasesThePseudorandomKey carries: an all
// zero reading only means something if the key was not already zero, and a returned secret
// that came back zero would satisfy the loop for the wrong reason.
func TestNewKeyScheduleErasesTheMemberSecret(t *testing.T) {
	for _, epoch := range ksVectorEpochs(t) {
		joinerSecret := mustDecodeHex(t, "joiner_secret"+epoch.at, epoch.published.JoinerSecret)

		fresh := epoch.crypto.Extract(joinerSecret, epoch.pskSecret)
		if !slices.ContainsFunc(fresh, func(b byte) bool { return b != 0 }) {
			t.Fatalf("%s: Extract over these inputs is already %d zero bytes, so an all zero reading below would say nothing",
				epoch.at, len(fresh))
		}

		crypto := &extractCapturingProvider{CryptoProvider: epoch.crypto}
		schedule, err := NewKeyScheduleFromJoiner(crypto, joinerSecret, epoch.pskSecret, epoch.groupContext)
		if err != nil {
			t.Fatalf("%s: NewKeyScheduleFromJoiner: %v", epoch.at, err)
		}
		if len(crypto.extracted) != 1 {
			t.Fatalf("%s: Extract was called %d times, want 1; this gate reads the key that one call returned",
				epoch.at, len(crypto.extracted))
		}
		member := crypto.extracted[0]
		for i, b := range member {
			if b != 0 {
				t.Errorf("%s: byte %d of member_secret is %#02x after the call, want 0; it reproduces every secret of the epoch",
					epoch.at, i, b)
				break
			}
		}
		// and the erase reached member_secret rather than what it produced
		if !slices.ContainsFunc(schedule.WelcomeSecret(), func(b byte) bool { return b != 0 }) {
			t.Errorf("%s: welcome_secret came back as %d zero bytes, so the erase reached the values that were kept",
				epoch.at, len(schedule.WelcomeSecret()))
		}
	}
}

// expandCapturingProvider keeps every ExpandWithLabel answer, for the same reason and with
// the same no-copy discipline extractCapturingProvider has: what is read afterwards is the
// storage the production code held, not a clone of it.
type expandCapturingProvider struct {
	CryptoProvider
	expanded [][]byte
}

func (self *expandCapturingProvider) ExpandWithLabel(
	secret []byte, label string, context []byte, length int,
) []byte {
	answer := self.CryptoProvider.ExpandWithLabel(secret, label, context, length)
	self.expanded = append(self.expanded, answer)
	return answer
}

// TestNewKeyScheduleErasesTheJoinerSecretItDerived observes both outcomes of the one
// derivation NewKeySchedule makes on its own account.
//
// On the refusal the joiner secret reached no caller: it is derived before psk_secret is
// looked at, so a psk of the wrong length leaves a live secret behind. On success the
// schedule holds a copy of its own, so the storage this one was computed into is again a
// live secret nothing will ever come back for. Returning without erasing it is the shape
// a reader would not think twice about, and it changes no answer this package computes:
// joiner_secret is one Extract and one Expand from the whole epoch, and a Welcome is
// sealed under what it produces.
//
// The success half also reads the schedule's own joiner secret back, because an erase
// aimed one line wrong would clear that instead and the epoch would go on looking fine
// until a Welcome sealed under zeros.
func TestNewKeyScheduleErasesTheJoinerSecretItDerived(t *testing.T) {
	for _, epoch := range ksVectorEpochs(t) {
		for _, testCase := range []struct {
			what      string
			pskSecret []byte
			refused   bool
		}{
			{what: "refused for a short psk secret", pskSecret: epoch.pskSecret[:len(epoch.pskSecret)-1], refused: true},
			{what: "accepted", pskSecret: epoch.pskSecret},
		} {
			at := epoch.at + " " + testCase.what
			crypto := &expandCapturingProvider{CryptoProvider: epoch.crypto}
			schedule, err := NewKeySchedule(
				crypto, epoch.initPrev, epoch.commitSecret, testCase.pskSecret, epoch.groupContext)
			if testCase.refused {
				if !errors.Is(err, ErrSecretLength) {
					t.Fatalf("%s: err = %v, want ErrSecretLength", at, err)
				}
				if schedule != nil {
					t.Errorf("%s: a schedule came back beside the refusal", at)
				}
			} else if err != nil {
				t.Fatalf("%s: NewKeySchedule: %v", at, err)
			}
			// the joiner expansion is the first, whether or not the epoch expansion follows
			if len(crypto.expanded) == 0 {
				t.Fatalf("%s: ExpandWithLabel was never called, so this row read no joiner secret", at)
			}
			derived := crypto.expanded[0]
			if len(derived) != epoch.crypto.HashSize() {
				t.Fatalf("%s: the captured expansion is %d bytes, want KDF.Nh = %d",
					at, len(derived), epoch.crypto.HashSize())
			}
			for i, b := range derived {
				if b != 0 {
					t.Errorf("%s: byte %d of the derived joiner secret is %#02x afterwards, want 0; nothing will ever come back for that storage",
						at, i, b)
					break
				}
			}
			// and on the accepted path the erase reached that storage rather than the copy
			// the epoch keeps, which is the value a Welcome is sealed under
			if schedule != nil {
				assertLabelKat(t, "joiner_secret"+at, schedule.JoinerSecret(), epoch.published.JoinerSecret)
			}
		}
	}
}

// TestKeyScheduleConstructorsRefuseSecretsThatAreNotKdfNh sweeps every wrong length of every
// secret argument of every constructor, rather than the one short case.
//
// HKDF-Extract accepts any length of either argument and answers with a well formed
// pseudorandom key, so none of these is refused by the arithmetic. A secret one byte short,
// one byte long, empty or absent all produce an epoch that is internally consistent and
// that no peer agrees with, which surfaces as an undecryptable message rather than as the
// length mistake it is.
func TestKeyScheduleConstructorsRefuseSecretsThatAreNotKdfNh(t *testing.T) {
	for _, epoch := range ksVectorEpochs(t) {
		nh := epoch.crypto.HashSize()
		joinerSecret := mustDecodeHex(t, "joiner_secret"+epoch.at, epoch.published.JoinerSecret)
		wrongLengths := map[string][]byte{
			"one byte short": bytes.Repeat([]byte{0x11}, nh-1),
			"one byte long":  bytes.Repeat([]byte{0x11}, nh+1),
			"empty":          {},
			"nil":            nil,
		}
		for _, name := range slices.Sorted(maps.Keys(wrongLengths)) {
			wrong := wrongLengths[name]
			for _, testCase := range []struct {
				what string
				call func() (*KeySchedule, error)
			}{
				{what: "NewKeySchedule init_secret", call: func() (*KeySchedule, error) {
					return NewKeySchedule(epoch.crypto, wrong, epoch.commitSecret, epoch.pskSecret, epoch.groupContext)
				}},
				{what: "NewKeySchedule commit_secret", call: func() (*KeySchedule, error) {
					return NewKeySchedule(epoch.crypto, epoch.initPrev, wrong, epoch.pskSecret, epoch.groupContext)
				}},
				{what: "NewKeySchedule psk_secret", call: func() (*KeySchedule, error) {
					return NewKeySchedule(epoch.crypto, epoch.initPrev, epoch.commitSecret, wrong, epoch.groupContext)
				}},
				{what: "NewKeyScheduleFromJoiner joiner_secret", call: func() (*KeySchedule, error) {
					return NewKeyScheduleFromJoiner(epoch.crypto, wrong, epoch.pskSecret, epoch.groupContext)
				}},
				{what: "NewKeyScheduleFromJoiner psk_secret", call: func() (*KeySchedule, error) {
					return NewKeyScheduleFromJoiner(epoch.crypto, joinerSecret, wrong, epoch.groupContext)
				}},
				{what: "NewKeyScheduleFromEpochSecret epoch_secret", call: func() (*KeySchedule, error) {
					return NewKeyScheduleFromEpochSecret(epoch.crypto, wrong, epoch.groupContext)
				}},
			} {
				schedule, err := testCase.call()
				if !errors.Is(err, ErrSecretLength) {
					t.Errorf("%s: %s %s gave err = %v, want ErrSecretLength",
						epoch.at, testCase.what, name, err)
				}
				if schedule != nil {
					t.Errorf("%s: %s %s was refused and a schedule came back beside the error",
						epoch.at, testCase.what, name)
				}
			}
		}
	}
}

// keyScheduleConstructorsOverAGroupContext is every exported package level function that
// takes a *GroupContext, read off this package's own source rather than listed.
//
// A list would be the fourteenth understatement in this repository. Every one of these
// serializes the context it is handed, syntax.Marshal receives a non nil interface holding a
// nil pointer, and MarshalMLS dereferences it — so the missing one is not a function without
// the guard, it is a function whose caller gets a panic out of the syntax package naming
// neither the function nor the argument that was absent. Every one of them takes its context
// off a struct field, which is exactly where an unset one comes from.
func keyScheduleConstructorsOverAGroupContext(t *testing.T) []string {
	t.Helper()
	exported := []string{}
	for _, name := range packageLevelFunctionsTaking(t, "*GroupContext") {
		if ast.IsExported(name) {
			exported = append(exported, name)
		}
	}
	if len(exported) == 0 {
		t.Fatal("no exported package level function of this package takes a *GroupContext, so the gate below demands nothing")
	}
	return exported
}

// TestEveryConstructorOverAGroupContextRefusesANilOne holds the class the scan above found,
// so a fourth constructor added to this file is refused entry until it is covered here.
func TestEveryConstructorOverAGroupContextRefusesANilOne(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	nh := crypto.HashSize()
	secret := bytes.Repeat([]byte{0x5c}, nh)
	// a nil literal and a nil valued variable of the type are the same argument, and the
	// second is how an unset struct field arrives
	var unset *GroupContext
	// built once, against the real context, so the rows that hand this door a nil context are
	// handing it a nil context and nothing else
	commitPath := commitPathValidUnder(t, crypto, ksVectorEpoch0GroupContext(t), LeafIndex(1))
	covered := map[string]func(context *GroupContext) error{
		"DeriveJoinerSecret": func(context *GroupContext) error {
			_, err := DeriveJoinerSecret(crypto, secret, secret, context)
			return err
		},
		"NewKeySchedule": func(context *GroupContext) error {
			_, err := NewKeySchedule(crypto, secret, secret, secret, context)
			return err
		},
		"NewKeyScheduleFromJoiner": func(context *GroupContext) error {
			_, err := NewKeyScheduleFromJoiner(crypto, secret, secret, context)
			return err
		},
		"NewKeyScheduleFromEpochSecret": func(context *GroupContext) error {
			_, err := NewKeyScheduleFromEpochSecret(crypto, secret, context)
			return err
		},
		// p7 task 8's section 12.3 application. The context is what a
		// GroupContextExtensions proposal replaces, so a nil one is an application with
		// nothing to replace rather than an application over an empty extension set. The
		// tree and the list are live: an empty tree and an empty list are both legal, and
		// what this row observes is the context.
		"ApplyProposals": func(context *GroupContext) error {
			_, err := ApplyProposals(NewRatchetTree(), context, LeafIndex(0), &ProposalList{})
			return err
		},
		// RFC 9420 section 7.3's commit door. The context is where the group id, the suite and
		// the extensions the leaf is judged against all come from, so a nil one leaves this
		// door with no expectation to state rather than with an empty one -- and the PATH here
		// is live, built against the real context once, so the refusal is the context's.
		"ValidateUpdatePathLeafNode": func(context *GroupContext) error {
			return ValidateUpdatePathLeafNode(crypto, context, LeafIndex(1), commitPath)
		},
		// p7 task 10's erratum 8745 door. The context is where the extensions the update
		// path's leaf owes support for come from, so a nil one leaves this rule with nothing
		// to hold the leaf against -- which is a check skipped rather than a check passed.
		// The PATH is live, built against the real context once, so the refusal is the
		// context's.
		"CheckErrata8745": func(context *GroupContext) error {
			return CheckErrata8745(commitPath, context)
		},
		// p7 task 6's proposal cache used to be in this class and is deliberately not any
		// more. It takes a *VerifiedGroupContext now -- a group context whose authority has
		// been established, whose only constructor is (*GroupInfo).VerifiedContext --
		// so it is no longer a constructor over a *GroupContext at all, and the class this
		// gate derives is what says so rather than this comment. Its own nil refusal is
		// still held, by the derived nil argument sweep next door.
	}
	found := keyScheduleConstructorsOverAGroupContext(t)
	if got := slices.Sorted(maps.Keys(covered)); !slices.Equal(got, slices.Sorted(slices.Values(found))) {
		t.Fatalf("this gate covers %v and the package's source declares %v as exported functions over a *GroupContext",
			got, found)
	}
	for _, name := range slices.Sorted(maps.Keys(covered)) {
		// the control: a real context is accepted, so a refusal below is the nil and not
		// the other arguments. The control is itself made with a panic caught, because a
		// defect on the accepting path is a failure of this test and not a reason for the
		// test BINARY to stop -- every gate declared after this one would report nothing.
		var accepted error
		if recovered := recoveredPanic(func() { accepted = covered[name](ksVectorEpoch0GroupContext(t)) }); recovered != nil {
			t.Errorf("%s: a real group context panicked with %v", name, recovered)
			continue
		}
		if accepted != nil {
			t.Errorf("%s: a real group context was refused: %v", name, accepted)
			continue
		}
		for _, testCase := range []struct {
			what    string
			context *GroupContext
		}{
			{what: "a nil literal", context: nil},
			{what: "an unset field", context: unset},
		} {
			var err error
			recovered := recoveredPanic(func() { err = covered[name](testCase.context) })
			if recovered != nil {
				t.Errorf("%s: %s panicked with %v rather than being refused", name, testCase.what, recovered)
				continue
			}
			if !errors.Is(err, ErrNilGroupContext) {
				t.Errorf("%s: %s gave err = %v, want ErrNilGroupContext", name, testCase.what, err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// the group creation entry point
// ---------------------------------------------------------------------------

// scheduleFromEpochSecretRecovering builds a group creation schedule with a panic caught
// rather than taken.
//
// This is the one entry point whose joiner_secret and welcome_secret are nil, so it is where
// a derivation reaching for the wrong parent raises out of the kdf instead of answering. An
// uncaught raise stops the test BINARY, and every gate declared after it in this package
// then reports nothing at all -- see recoveringRow, which records what that cost. A nil
// answer means this helper already said what went wrong and the caller moves on to the next
// row rather than to the next test.
func scheduleFromEpochSecretRecovering(
	t *testing.T,
	at string,
	crypto CryptoProvider,
	epochSecret []byte,
	groupContext *GroupContext,
) *KeySchedule {
	t.Helper()
	var schedule *KeySchedule
	var err error
	recovered := recoveredPanic(func() {
		schedule, err = NewKeyScheduleFromEpochSecret(crypto, epochSecret, groupContext)
	})
	if recovered != nil {
		t.Errorf("%s: NewKeyScheduleFromEpochSecret panicked with %v rather than answering", at, recovered)
		return nil
	}
	if err != nil {
		t.Errorf("%s: NewKeyScheduleFromEpochSecret: %v", at, err)
		return nil
	}
	return schedule
}

// TestNewKeyScheduleFromEpochSecretDerivesTheSameNineSecrets asserts the creation path
// reaches the same nine secrets as the commit path, given the epoch_secret the commit path
// computed. Anything else means a group's creator and its first joiner are in different
// epochs from the first message.
//
// The epoch secret is recomputed here from the corpus's own inputs rather than taken off the
// schedule, because the type deliberately never exports it — see
// TestNoExportedSurfaceOfTheKeyScheduleReturnsTheEpochSecret. Widening a signature to make
// this test easier is the thing G6 is about, so the test is written against what the type is
// allowed to expose.
//
// The set compared is the one reflection read, so the two paths are held to agreeing on
// every field EpochSecrets has rather than on the nine that were current when this was
// written.
func TestNewKeyScheduleFromEpochSecretDerivesTheSameNineSecrets(t *testing.T) {
	for _, epoch := range ksVectorEpochs(t) {
		epochSecret := epoch.crypto.ExpandWithLabel(
			epoch.crypto.Extract(
				mustDecodeHex(t, "joiner_secret"+epoch.at, epoch.published.JoinerSecret), epoch.pskSecret),
			"epoch", mustDecodeHex(t, "group_context"+epoch.at, epoch.published.GroupContext),
			epoch.crypto.HashSize())

		fromEpoch := scheduleFromEpochSecretRecovering(t, epoch.at, epoch.crypto, epochSecret, epoch.groupContext)
		if fromEpoch == nil {
			continue
		}
		fromCommit := epochSecretsByField(t, epoch.schedule(t).Secrets())
		created := epochSecretsByField(t, fromEpoch.Secrets())
		if got, want := slices.Sorted(maps.Keys(created)), slices.Sorted(maps.Keys(fromCommit)); !slices.Equal(got, want) {
			t.Fatalf("%s: the creation path answers with fields %v and the commit path with %v", epoch.at, got, want)
		}
		for _, name := range slices.Sorted(maps.Keys(fromCommit)) {
			if !bytes.Equal(fromCommit[name], created[name]) {
				t.Errorf("%s: EpochSecrets.%s is %x from the commit path and %x from the creation path",
					epoch.at, name, fromCommit[name], created[name])
			}
		}
		if !bytes.Equal(fromEpoch.GroupContextBytes(), mustDecodeHex(t, "group_context"+epoch.at, epoch.published.GroupContext)) {
			t.Errorf("%s: the creation path expanded over %x", epoch.at, fromEpoch.GroupContextBytes())
		}
	}
}

// TestNewKeyScheduleFromEpochSecretHasNoJoinerOrWelcomeSecret asserts the two secrets that
// are undefined on this path read as nil.
//
// A group created from a sampled epoch_secret was never joined, so there is no joiner_secret
// and no welcome_secret to be had. Substituting KDF.Nh zero bytes for either would keep the
// signatures the registry fixes and would seal a Welcome under a run of zeros — a key an
// attacker also has — while every length check in this package passed. Nil is what makes the
// caller's mistake a refusal at the first length check downstream.
func TestNewKeyScheduleFromEpochSecretHasNoJoinerOrWelcomeSecret(t *testing.T) {
	for _, suite := range Suites() {
		crypto := mustProvider(t, suite)
		at := fmt.Sprintf("suite %#04x", uint16(suite))
		schedule := scheduleFromEpochSecretRecovering(
			t, at, crypto, crypto.Random(crypto.HashSize()), ksVectorEpoch0GroupContext(t))
		if schedule == nil {
			continue
		}
		if schedule.JoinerSecret() != nil {
			t.Errorf("%s: JoinerSecret = %x, want nil on the creation path", at, schedule.JoinerSecret())
		}
		if schedule.WelcomeSecret() != nil {
			t.Errorf("%s: WelcomeSecret = %x, want nil on the creation path", at, schedule.WelcomeSecret())
		}
		// and the consequence the nil is FOR, which is what makes the two assertions above
		// about a Welcome rather than about a struct field. Task 6a could not assert this
		// because WelcomeKeyNonce did not exist yet; it does now. A creation path that
		// answered KDF.Nh zero bytes instead of nil would satisfy this construction's length
		// check and seal encrypted_group_info under an expansion of a run of zeros -- a key
		// every party can recompute -- and nothing would report an error.
		welcomeKey, welcomeNonce, welcomeErr := WelcomeKeyNonce(crypto, schedule.WelcomeSecret())
		if !errors.Is(welcomeErr, ErrSecretLength) {
			t.Errorf("%s: WelcomeKeyNonce over the creation path's welcome secret answered err = %v, want %v",
				at, welcomeErr, ErrSecretLength)
		}
		if welcomeKey != nil || welcomeNonce != nil {
			t.Errorf("%s: the creation path derived a %d byte welcome key and a %d byte welcome nonce out of a welcome secret it does not have",
				at, len(welcomeKey), len(welcomeNonce))
		}
		// and the nine are there, so the nils above are the two that are undefined rather
		// than a schedule that derived nothing at all
		for _, name := range slices.Sorted(maps.Keys(epochSecretsByField(t, schedule.Secrets()))) {
			if got := epochSecretsByField(t, schedule.Secrets())[name]; len(got) != crypto.HashSize() {
				t.Errorf("%s: EpochSecrets.%s is %d bytes on the creation path, want KDF.Nh = %d",
					at, name, len(got), crypto.HashSize())
			}
		}
	}
}

// TestNewKeyScheduleFromEpochSecretCopiesTheSample observes the sentence
// NewKeyScheduleFromEpochSecret writes: the sample is copied rather than retained.
//
// Retaining it would make the epoch's parent secret a window onto a buffer the caller
// still owns, and the caller of this is a NewGroup that just sampled KDF.Nh bytes and is
// about to erase them — which would erase the live epoch's parent instead of its own copy.
//
// What this reads is the unexported field, because nothing else can see the property. The
// nine are derived at construction, so they are already their own storage whether the
// parent was copied or not: a test that erased the sample and compared the nine passes
// over both implementations, which is what the first version of this test did. Reading
// the field is not a widening of anything — G6 is about what the type EXPORTS, and
// TestNoExportedSurfaceOfTheKeyScheduleReturnsTheEpochSecret is what holds that; a test
// in the same package is where a white box property like this one belongs.
func TestNewKeyScheduleFromEpochSecretCopiesTheSample(t *testing.T) {
	for _, suite := range Suites() {
		crypto := mustProvider(t, suite)
		at := fmt.Sprintf("suite %#04x", uint16(suite))
		sample := crypto.Random(crypto.HashSize())
		kept := bytes.Clone(sample)
		schedule := scheduleFromEpochSecretRecovering(t, at, crypto, sample, ksVectorEpoch0GroupContext(t))
		if schedule == nil {
			continue
		}
		// the control: the sample was read at all, so the comparisons below are not
		// satisfied by a constructor that never looked at its argument
		if len(schedule.epochSecret) != crypto.HashSize() {
			t.Fatalf("%s: the epoch holds %d bytes as its parent, want KDF.Nh = %d",
				at, len(schedule.epochSecret), crypto.HashSize())
		}
		if !bytes.Equal(schedule.epochSecret, kept) {
			t.Fatalf("%s: the epoch's parent is not the sample it was handed", at)
		}
		if &schedule.epochSecret[0] == &sample[0] {
			t.Errorf("%s: the epoch retained the caller's sample rather than copying it", at)
		}
		// and the consequence, read the way a caller would produce it: whoever sampled the
		// secret erases its own copy, and the live epoch is unmoved
		zeroizeSecret(sample)
		if !bytes.Equal(schedule.epochSecret, kept) {
			t.Errorf("%s: erasing the caller's sample cleared the epoch's own parent secret", at)
		}
	}
}

// ---------------------------------------------------------------------------
// KDF.Nh is read off the provider, over every construction handed one
//
// Both registered suites fix Nh at 32, so nothing already in this tree separates a body
// that reads KDF.Nh off the provider it was handed from one that writes 32 down. The two
// gates above supply that input for two hand picked functions. This supplies it for the
// class.
//
// Measured, not supposed: nh := crypto.HashSize() replaced by nh := 32 in
// NewKeyScheduleFromJoiner left the whole of mls and message green, and so did the same
// substitution in NewKeyScheduleFromJoiner and NewKeyScheduleFromEpochSecret at once. Not a
// wrong answer today — it is a gate that would not fire on the day a third suite lands,
// which is the day it exists for, and suite.go's own file comment asserts the class it
// violates: "Every length check in the package reads a field of it rather than a literal, so
// a suite added later cannot leave a hardcoded 32 behind in code that was only ever
// exercised at 32."
// ---------------------------------------------------------------------------

// kdfNhCoincidences is the positions at which two runs of one construction disagree about
// whether a length is KDF.Nh.
//
// The property is stated as an equivalence rather than as a length: an answer that is KDF.Nh
// bytes over one provider must be KDF.Nh bytes over the other, whichever provider that is.
// A body that writes 32 down answers 32 over both, so over the narrow provider the length
// IS Nh and over the wide one it is not, and the position is reported. A body that reads the
// provider answers 32 and then 48 and reports nothing. Both directions are compared, because
// a length hardcoded at 48 is the same defect written the other way round.
func kdfNhCoincidences(narrow [][]byte, wide [][]byte, narrowNh int, wideNh int) []int {
	found := []int{}
	for i := range min(len(narrow), len(wide)) {
		if (len(narrow[i]) == narrowNh) != (len(wide[i]) == wideNh) {
			found = append(found, i)
		}
	}
	return found
}

// constructionsWhoseAnswerOnlyCoincidesWithKdfNh is a construction the equivalence above
// cannot hold, named with the reason. The gate checks every name here against the derived
// class, so an entry cannot outlive the construction it excuses.
var constructionsWhoseAnswerOnlyCoincidesWithKdfNh = map[string]string{
	// the first of its two answers is the KEM output, which is Nenc bytes. X25519 fixes
	// Nenc at 32 and the narrow suite's KDF.Nh is also 32, so the equality is the suite's
	// coincidence rather than anything this construction did; the kdf getting wider does
	// not make an X25519 public key wider. Its other answer is the ciphertext, which is
	// the plaintext plus the aead tag and is not a kdf length either.
	"EncryptWithLabel": "answers a KEM output at Nenc and a ciphertext at Nt, neither of which is KDF.Nh; Nenc coincides with Nh at 32 under the narrow suite",
	// the same two answers in one structure, so the same coincidence and the same excuse.
	// What holds the two lengths instead is
	// TestSealWithLabelPutsTheKemOutputAndTheCiphertextInTheirOwnFields, which reads Nenc and
	// Nt off the suite registry rather than off the kdf.
	"SealWithLabel": "answers a KEM output at Nenc and a ciphertext at Nt, neither of which is KDF.Nh; Nenc coincides with Nh at 32 under the narrow suite",
	// its two answers are the aead key at Nk and the aead nonce at Nn. Nk is 32 under the
	// narrow suite and KDF.Nh is 32 there too, so the equality is that suite's coincidence
	// rather than anything this construction did -- and a kdf getting wider does not widen
	// a ChaCha20-Poly1305 key, which is why the wide provider deliberately leaves KeySize
	// and NonceSize alone. What holds these two lengths to the provider instead is
	// TestWelcomeKeyNonceReadsBothLengthsOffTheProviderItWasHanded, whose provider moves Nk
	// and Nn rather than Nh.
	"WelcomeKeyNonce": "answers an aead key at Nk and an aead nonce at Nn, neither of which is KDF.Nh; Nk coincides with Nh at 32 under the narrow suite",
	// the same two answers under the same coincidence, and the same remedy: what holds
	// these lengths to the provider is
	// TestSenderDataKeyNonceReadsBothLengthsOffTheProviderItWasHanded, whose provider
	// moves Nk and Nn rather than Nh. That test also holds the length this one cannot
	// reach at all -- the ciphertext SAMPLE is KDF.Nh bytes and appears in no answer.
	"SenderDataKeyNonce": "answers an aead key at Nk and an aead nonce at Nn, neither of which is KDF.Nh; Nk coincides with Nh at 32 under the narrow suite",
	// its two answers are the KEM private and public halves, at Nsk and Npk. X25519 fixes
	// both at 32 and the narrow suite's KDF.Nh is 32 too, so the equality is that suite's
	// coincidence rather than anything this construction did -- a kdf getting wider does not
	// widen an X25519 key. What holds the node_secret in front of the KEM to KDF.Nh is the
	// row itself, which must work at either width, together with
	// TestTheNodeKeyPairIsTheKemKeyOfTheNodeSecretAndNotOfThePathSecret and the published
	// TreeKEM corpus, which compare the BYTES this row only measures.
	"DeriveNodeKeyPair": "answers a KEM private key at Nsk and a public key at Npk, neither of which is KDF.Nh; both coincide with Nh at 32 under the narrow suite",
}

// scheduleAnswersThatAreNotKdfLengths names one ANSWER of an exported method of *KeySchedule
// that the KDF.Nh equivalence below cannot hold, with the reason. It is the same excuse
// constructionsWhoseAnswerOnlyCoincidesWithKdfNh carries for a package level construction,
// one level down: the schedule's answers reach that gate through the rows that build a
// schedule, so a method answering something that is not a kdf length cannot be excused by
// naming the construction without excusing the whole epoch with it.
//
// Keyed per answer rather than per method, which is the tighter form exposedSlice was given a
// position for. Keyed on ExternalKeyPair alone this excused EVERY answer of that method, so a
// third result that IS a kdf length would have joined the excuse by existing and left the
// differential silent about it. Two results today, two lines.
//
// Every entry is checked against the type, against the method's own arity and against the
// sweep's own output, so an excuse cannot outlive the method it names, cannot name a result
// position the method does not have, and cannot sit here excusing nothing.
var scheduleAnswersThatAreNotKdfLengths = map[exposedAnswerAt]string{
	{method: "ExternalKeyPair", result: 0}: "the hpke private key, which is Nsk; X25519 fixes it at 32 and the narrow suite's KDF.Nh is also 32, so the equality is that suite's coincidence rather than anything this method did, and a kdf getting wider does not make an X25519 key wider",
	{method: "ExternalKeyPair", result: 1}: "the hpke public key, which is Npk; the same coincidence at 32 and the same reason a wider kdf does not widen it",
}

// scheduleStorageReaders is how this gate reads each byte slice a *KeySchedule keeps behind
// its accessors.
//
// epoch_secret is where a hardcoded KDF.Nh sits unobserved from outside the type. The nine
// secrets derived from it are expanded at the provider's own width whatever width their
// parent had, and the parent itself must never reach an exported symbol -- that is guardrail
// G6 -- so a parent truncated to 32 bytes under a 48 byte kdf is invisible to every gate that
// reads only what the epoch hands out. This is the reading that sees it, and it is available
// because these tests are the package's own.
//
// The keys are checked against the type's own fields by reflection in both directions, so a
// fifth byte slice field cannot land unread.
var scheduleStorageReaders = map[string]func(schedule *KeySchedule) []byte{
	"groupContextBytes": func(schedule *KeySchedule) []byte { return schedule.groupContextBytes },
	"joinerSecret":      func(schedule *KeySchedule) []byte { return schedule.joinerSecret },
	"welcomeSecret":     func(schedule *KeySchedule) []byte { return schedule.welcomeSecret },
	"epochSecret":       func(schedule *KeySchedule) []byte { return schedule.epochSecret },
}

// bytesTheScheduleKeeps is everything the type holds: what its exported surface hands out,
// followed by the storage behind it, in a fixed order so two runs of one construction compare
// position by position.
func bytesTheScheduleKeeps(t *testing.T, at string, schedule *KeySchedule) [][]byte {
	t.Helper()
	byteSlice := reflect.TypeOf([]byte(nil))
	fields := []string{}
	valueType := reflect.TypeOf(schedule).Elem()
	for i := range valueType.NumField() {
		if valueType.Field(i).Type != byteSlice {
			continue
		}
		name := valueType.Field(i).Name
		fields = append(fields, name)
		if _, read := scheduleStorageReaders[name]; !read {
			t.Fatalf("KeySchedule keeps a []byte field %s that scheduleStorageReaders has no reader for, so its length falls outside every comparison this gate makes",
				name)
		}
	}
	for name := range scheduleStorageReaders {
		if !slices.Contains(fields, name) {
			t.Errorf("scheduleStorageReaders reads %s, which KeySchedule does not declare as a []byte field", name)
		}
	}
	slices.Sort(fields)
	handedOut, _ := bytesTheScheduleHandsOut(t, at, schedule)
	kept := [][]byte{}
	dropped := map[exposedAnswerAt]bool{}
	for _, one := range handedOut {
		if _, notAKdfLength := scheduleAnswersThatAreNotKdfLengths[one.at()]; notAKdfLength {
			dropped[one.at()] = true
			continue
		}
		kept = append(kept, one.bytes)
	}
	for answer, reason := range scheduleAnswersThatAreNotKdfLengths {
		method, found := reflect.TypeOf(schedule).MethodByName(answer.method)
		if !found {
			t.Errorf("scheduleAnswersThatAreNotKdfLengths excuses %s, which *KeySchedule does not declare", answer)
			continue
		}
		// an excuse for a result position the method does not have is an excuse that can
		// never fire, and the sweep below would drop nothing while the table looked complete
		if answer.result >= method.Type.NumOut() {
			t.Errorf("scheduleAnswersThatAreNotKdfLengths excuses %s and that method answers %d results",
				answer, method.Type.NumOut())
			continue
		}
		// an excuse that dropped nothing is an excuse for an answer the sweep is no longer
		// reading, which is the shape that leaves this gate looking complete while covering
		// less than it did
		if !dropped[answer] {
			t.Errorf("scheduleAnswersThatAreNotKdfLengths excuses %s (%s) and the sweep read no such answer to drop",
				answer, reason)
		}
	}
	for _, name := range fields {
		kept = append(kept, scheduleStorageReaders[name](schedule))
	}
	return kept
}

// TestEveryConstructionHandedAProviderReadsKdfNhFromIt is the differential the registered
// suites cannot supply, over the class rather than over two functions of it.
//
// The class is every package level construction of this package that takes a CryptoProvider,
// read off the parse tree by the same scan TestEveryConstructionHandedAProviderRoutesThroughIt
// reads, and each has to have a row here. A construction added later fails this until
// somebody writes one, which is what makes the class the package's rather than this test's.
//
// The two providers are the registered suite and the same suite with its whole hash and kdf
// surface one width up. Coherence matters: see the wide provider's own comment. What is
// compared is only whether a length IS the provider's Nh, so a construction whose answers
// have nothing to do with the kdf reports nothing without needing to be excused for it.
func TestEveryConstructionHandedAProviderReadsKdfNhFromIt(t *testing.T) {
	// the constant reader is what makes EncryptWithLabel answer the same ciphertext twice;
	// over the process entropy source its rows would compare two unrelated messages
	narrow := mustProviderOver(t, CipherSuiteX25519ChaCha20Sha256Ed25519, constantReader{value: 0x35})
	wide := &wideKdfProvider{CryptoProvider: narrow}
	if narrow.HashSize() == wide.HashSize() {
		t.Fatalf("both providers answer KDF.Nh %d, so every row below compares a length against itself",
			narrow.HashSize())
	}

	value := bytes.Repeat([]byte{0x21}, 96)
	priv, pub, err := narrow.DeriveKeyPair(bytes.Repeat([]byte{0x22}, 32))
	if err != nil {
		t.Fatalf("derive the key pair the labelled rows are built over: %v", err)
	}
	// deliberately neither 32 nor 48 long, so the labelled rows do not report a coincidence
	// that belongs to the plaintext this test chose
	plaintext := []byte("seven..")
	sealedKemOutput, sealedCiphertext, err := EncryptWithLabel(narrow, pub, "UpdatePathNode", value, plaintext)
	if err != nil {
		t.Fatalf("seal the message the DecryptWithLabel rows read: %v", err)
	}
	// the serialized group context travels through the schedule rows as one of the answers
	// the epoch hands out, and it is the same bytes over both providers. A context that
	// happened to be KDF.Nh octets long would therefore read as a written down length, so
	// the coincidence is ruled out here rather than discovered as a failure in a row.
	encodedGroupContext, err := syntax.Marshal(ksVectorEpoch0GroupContext(t))
	if err != nil {
		t.Fatalf("encode the group context the schedule rows are built over: %v", err)
	}
	if len(encodedGroupContext) == narrow.HashSize() || len(encodedGroupContext) == wide.HashSize() {
		t.Fatalf("the published epoch 0 group context encodes to %d octets, which is one of the two KDF.Nh values this gate compares against",
			len(encodedGroupContext))
	}

	// every schedule row answers through the type's own exported surface, so the nine
	// derived secrets, the joiner secret and the welcome secret are all read
	scheduleAnswers := func(t *testing.T, at string, schedule *KeySchedule, err error) [][]byte {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", at, err)
		}
		return bytesTheScheduleKeeps(t, " over a provider whose KDF.Nh is "+
			strconv.Itoa(schedule.crypto.HashSize()), schedule)
	}

	covered := []string{}
	compared := 0
	coincidences := 0
	for _, testCase := range []struct {
		name string
		call func(t *testing.T, crypto CryptoProvider) [][]byte
	}{
		{name: "RefHash", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			return [][]byte{mustRefHash(t, crypto, "MLS 1.0 a label", value)}
		}},
		{name: "MakeKeyPackageRef", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			return [][]byte{mustKeyPackageRef(t, crypto, value)}
		}},
		{name: "MakeProposalRef", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			return [][]byte{mustProposalRef(t, crypto, value)}
		}},
		{name: "EncryptWithLabel", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			kemOutput, ciphertext, sealErr := EncryptWithLabel(crypto, pub, "UpdatePathNode", value, plaintext)
			if sealErr != nil {
				t.Fatalf("EncryptWithLabel: %v", sealErr)
			}
			return [][]byte{kemOutput, ciphertext}
		}},
		{name: "DecryptWithLabel", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			opened, openErr := DecryptWithLabel(crypto, priv, "UpdatePathNode", value,
				sealedKemOutput, sealedCiphertext)
			if openErr != nil {
				t.Fatalf("DecryptWithLabel: %v", openErr)
			}
			return [][]byte{opened}
		}},
		// the HpkeCiphertext shaped forms of the pair above. The open is a row rather than an
		// excuse because its answer IS the plaintext it was handed, which is not a kdf length
		// at either width; the seal is excused below for the reason EncryptWithLabel is.
		{name: "SealWithLabel", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			sealed, sealErr := SealWithLabel(crypto, pub, "UpdatePathNode", value, plaintext)
			if sealErr != nil {
				t.Fatalf("SealWithLabel: %v", sealErr)
			}
			return [][]byte{sealed.KemOutput, sealed.Ciphertext}
		}},
		{name: "OpenWithLabel", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			opened, openErr := OpenWithLabel(crypto, priv, "UpdatePathNode", value,
				&HpkeCiphertext{KemOutput: sealedKemOutput, Ciphertext: sealedCiphertext})
			if openErr != nil {
				t.Fatalf("OpenWithLabel: %v", openErr)
			}
			return [][]byte{opened}
		}},
		{name: "ZeroSecret", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			return [][]byte{ZeroSecret(crypto)}
		}},
		// psk_secret for an epoch with no pre shared keys, which is that same all zero
		// string. The row is here rather than excused because the LENGTH is exactly what
		// this gate compares, and a body that answered a hardcoded 32 zero bytes reads as
		// correct at every registered suite and fails here.
		{name: "EmptyPskSecret", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			return [][]byte{EmptyPskSecret(crypto)}
		}},
		// and the recurrence, in both of the shapes whose LENGTH this gate can hold.
		//
		// Not the expansion, and the distinction is the whole reason this comment is long.
		// psk_secret is Extract's output and Extract answers the provider's Nh whatever
		// width psk_input was expanded to, so a psk_input pinned to a literal 32 moves the
		// value of every non empty psk_secret and moves no length at all -- this gate would
		// report it clean, and did. What holds that width is
		// TestPskSecretReadsKdfNhFromTheProvider, which reads the requested expansion
		// lengths off the provider rather than the answer's.
		//
		// The two answers here are the folded list and psk_secret_[0] for the empty one.
		// The empty case is a row of its own under EmptyPskSecret, and it is here as well
		// because PskSecret writes that value at its own line rather than calling that
		// function: a literal in either is invisible from the other, and the two then
		// disagree at the first suite whose hash is not sha256.
		//
		// Two entries, so the fold is exercised and not only the first step; the nonces are
		// the provider's Nh or ValSem401 refuses the list, which is itself a KDF.Nh read
		// that would fail under a wider kdf.
		{name: "PskSecret", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			nh := crypto.HashSize()
			secret, pskErr := PskSecret(crypto, []PreSharedKeyInput{
				{
					Id: PreSharedKeyId{
						PskType:  PskTypeExternal,
						PskId:    bytes.Repeat([]byte{0x7e}, 16),
						PskNonce: bytes.Repeat([]byte{0x7f}, nh),
					},
					Secret: bytes.Repeat([]byte{0x80}, nh),
				},
				{
					Id: PreSharedKeyId{
						PskType:    PskTypeResumption,
						Usage:      ResumptionPskUsageApplication,
						PskGroupId: bytes.Repeat([]byte{0x81}, 12),
						PskEpoch:   9,
						PskNonce:   bytes.Repeat([]byte{0x82}, nh),
					},
					Secret: bytes.Repeat([]byte{0x83}, nh),
				},
			})
			if pskErr != nil {
				t.Fatalf("PskSecret: %v", pskErr)
			}
			overTheEmptyList, emptyErr := PskSecret(crypto, nil)
			if emptyErr != nil {
				t.Fatalf("PskSecret over the empty list: %v", emptyErr)
			}
			return [][]byte{secret, overTheEmptyList}
		}},
		// p7 task 15's welcome builder, over an EMPTY joiner set: what this row is about is the
		// group info seal, whose key and nonce come out of WelcomeKeyNonce and whose secret is
		// KDF.Nh bytes off the provider under test rather than 32 written down. A body that
		// compared against a literal 32 refuses the wide provider's welcome secret outright and
		// is reported here rather than passing at the one width where the two coincide.
		{name: "BuildWelcome", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			nh := crypto.HashSize()
			welcome, welcomeErr := BuildWelcome(crypto, crypto.Suite(), &GroupInfo{
				GroupContext: GroupContext{
					Version:                 ProtocolVersionMls10,
					CipherSuite:             crypto.Suite(),
					GroupId:                 []byte("the group this row describes"),
					Epoch:                   5,
					TreeHash:                bytes.Repeat([]byte{0x84}, nh),
					ConfirmedTranscriptHash: bytes.Repeat([]byte{0x85}, nh),
				},
				ConfirmationTag: bytes.Repeat([]byte{0x86}, nh),
			}, bytes.Repeat([]byte{0x87}, nh), bytes.Repeat([]byte{0x8a}, nh), nil)
			if welcomeErr != nil {
				t.Fatalf("BuildWelcome over a provider whose KDF.Nh is %d: %v", nh, welcomeErr)
			}
			return [][]byte{welcome.EncryptedGroupInfo}
		}},
		{name: "DeriveJoinerSecret", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			nh := crypto.HashSize()
			joiner, joinerErr := DeriveJoinerSecret(crypto,
				bytes.Repeat([]byte{0x71}, nh), bytes.Repeat([]byte{0x72}, nh),
				ksVectorEpoch0GroupContext(t))
			if joinerErr != nil {
				t.Fatalf("DeriveJoinerSecret: %v", joinerErr)
			}
			return [][]byte{joiner}
		}},
		{name: "NewKeySchedule", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			nh := crypto.HashSize()
			schedule, scheduleErr := NewKeySchedule(crypto,
				bytes.Repeat([]byte{0x73}, nh), bytes.Repeat([]byte{0x74}, nh),
				bytes.Repeat([]byte{0x75}, nh), ksVectorEpoch0GroupContext(t))
			return scheduleAnswers(t, "NewKeySchedule", schedule, scheduleErr)
		}},
		{name: "NewKeyScheduleFromJoiner", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			nh := crypto.HashSize()
			schedule, scheduleErr := NewKeyScheduleFromJoiner(crypto,
				bytes.Repeat([]byte{0x76}, nh), bytes.Repeat([]byte{0x77}, nh),
				ksVectorEpoch0GroupContext(t))
			return scheduleAnswers(t, "NewKeyScheduleFromJoiner", schedule, scheduleErr)
		}},
		{name: "NewKeyScheduleFromEpochSecret", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			schedule, scheduleErr := NewKeyScheduleFromEpochSecret(crypto,
				bytes.Repeat([]byte{0x78}, crypto.HashSize()), ksVectorEpoch0GroupContext(t))
			return scheduleAnswers(t, "NewKeyScheduleFromEpochSecret", schedule, scheduleErr)
		}},
		{name: "newKeyScheduleFromParts", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			nh := crypto.HashSize()
			schedule := newKeyScheduleFromParts(crypto, encodedGroupContext,
				bytes.Repeat([]byte{0x79}, nh), bytes.Repeat([]byte{0x7a}, nh),
				bytes.Repeat([]byte{0x7b}, nh))
			return scheduleAnswers(t, "newKeyScheduleFromParts", schedule, nil)
		}},
		// the welcome key and nonce. The secret it is handed is KDF.Nh bytes off the
		// provider under test rather than 32 written down, so this row also exercises the
		// refusal: a body that compared against a literal 32 refuses the wide provider's
		// secret outright and is reported as a panic rather than as a length.
		{name: "WelcomeKeyNonce", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			key, nonce, welcomeErr := WelcomeKeyNonce(crypto,
				bytes.Repeat([]byte{0x7d}, crypto.HashSize()))
			if welcomeErr != nil {
				t.Fatalf("WelcomeKeyNonce over a provider whose KDF.Nh is %d: %v", crypto.HashSize(), welcomeErr)
			}
			return [][]byte{key, nonce}
		}},
		// the secret tree's descent. Every node secret of the tree is KDF.Nh wide at either
		// width, and the encryption secret it is seeded with is read off the provider under
		// test rather than written 32, so this row exercises the constructor's own length
		// refusal as well: a body comparing against a literal 32 refuses the wide provider's
		// secret outright and is reported as a panic rather than as a length. The leaf is
		// what is read because the constructor's answer is a struct, and a leaf three levels
		// down is read rather than the root's own copy so the expansions are in the answer.
		{name: "NewSecretTree", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			tree, treeErr := NewSecretTree(crypto, 8, bytes.Repeat([]byte{0x7e}, crypto.HashSize()))
			if treeErr != nil {
				t.Fatalf("NewSecretTree over a provider whose KDF.Nh is %d: %v", crypto.HashSize(), treeErr)
			}
			leafSecret, takeErr := tree.takeLeafSecret(3)
			if takeErr != nil {
				t.Fatalf("takeLeafSecret: %v", takeErr)
			}
			return [][]byte{leafSecret}
		}},
		// the sender data key and nonce. Its secret is read off the provider under test
		// rather than written 32, so this row exercises the length refusal as well: a body
		// comparing against a literal 32 refuses the wide provider's secret outright and is
		// reported as a panic rather than as a length. The two ANSWERS are Nk and Nn and
		// are excused from the equivalence below for the reason WelcomeKeyNonce is; what
		// this row still holds is that the construction survives the wider kdf at all,
		// which a written down sample length does not.
		{name: "SenderDataKeyNonce", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			key, nonce, senderErr := SenderDataKeyNonce(crypto,
				bytes.Repeat([]byte{0x85}, crypto.HashSize()), value)
			if senderErr != nil {
				t.Fatalf("SenderDataKeyNonce over a provider whose KDF.Nh is %d: %v", crypto.HashSize(), senderErr)
			}
			return [][]byte{key, nonce}
		}},
		// the two transcript hash arithmetics, which answer the provider's hash and nothing
		// else, so both are KDF.Nh wide at either width. What this equivalence reads here is
		// a truncation: a body ending in [:32], or one that built its answer into a written
		// down 32 byte array, answers 32 over both providers. A truncated transcript hash is
		// not a weak hash -- it is a shorter preimage that every member of the group would
		// agree on and no other implementation would.
		{name: "ConfirmedTranscriptHash", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			return [][]byte{ConfirmedTranscriptHash(crypto, value, plaintext)}
		}},
		{name: "InterimTranscriptHash", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			interim, interimErr := InterimTranscriptHash(crypto, value,
				bytes.Repeat([]byte{0x84}, crypto.HashSize()))
			if interimErr != nil {
				t.Fatalf("InterimTranscriptHash over a provider whose KDF.Nh is %d: %v", crypto.HashSize(), interimErr)
			}
			return [][]byte{interim}
		}},
		// the key_package leaf constructor. It reads no length off the provider at all --
		// what it takes from one is a signature and a verification -- so what this row
		// states is the other half of the equivalence: the constructor must WORK over a
		// provider whose KDF.Nh is not 32, and a body that had cut anything to a written
		// down 32 would refuse there. The signature is the answer read, and at 64 octets it
		// is neither provider's Nh, so it reports no coincidence belonging to a length this
		// gate chose.
		{name: "NewLeafNode", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			leaf, leafErr := NewLeafNode(crypto, SignaturePrivateKey(bytes.Repeat([]byte{0x5c}, 32)),
				BasicCredential([]byte("alice")), pub, leafNodeStubCapabilities(), nil)
			if leafErr != nil {
				t.Fatalf("NewLeafNode over a provider whose KDF.Nh is %d: %v", crypto.HashSize(), leafErr)
			}
			return [][]byte{leaf.Signature}
		}},
		// the key package constructor, which DOES read a length off the provider -- its two
		// entropy draws are KDF.Nh wide -- so what this row states is the half NewLeafNode's
		// cannot: it must WORK over a provider whose KDF.Nh is not 32, and a body that drew a
		// written down 32 into DeriveKeyPair would key every member of every group at that
		// suite from less entropy than the suite asks for, invisibly.
		//
		// The signature is the answer read and the two private halves are not, because an
		// X25519 scalar is 32 octets at either width: reading them here would report the KEM's
		// own fixed length as a length this constructor wrote down.
		{name: "NewKeyPackage", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			kp, _, _, kpErr := NewKeyPackage(crypto, CipherSuiteX25519ChaCha20Sha256Ed25519,
				BasicCredential([]byte("alice")), leafNodeStubCapabilities(), nil)
			if kpErr != nil {
				t.Fatalf("NewKeyPackage over a provider whose KDF.Nh is %d: %v", crypto.HashSize(), kpErr)
			}
			return [][]byte{kp.Signature}
		}},
		// the sibling constructor, which reads the SAME two KDF.Nh entropy draws off the provider
		// and takes its signature key from the caller instead of drawing one. The provider's
		// KDF.Nh is still what both DeriveKeyPair inputs are cut to, so the row states exactly
		// what NewKeyPackage's states: it must WORK over a provider whose KDF.Nh is not 32, and
		// a body that drew a written down 32 into DeriveKeyPair would key every member of every
		// group at that suite from less entropy than the suite asks for, invisibly.
		//
		// The signature is the answer read, and the two private halves are not, for the row
		// above's reason: an X25519 scalar is 32 octets at either width.
		{name: "NewKeyPackageWithSigner", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			kp, _, _, kpErr := NewKeyPackageWithSigner(crypto, CipherSuiteX25519ChaCha20Sha256Ed25519,
				SignaturePrivateKey(bytes.Repeat([]byte{0x3e}, 32)),
				BasicCredential([]byte("alice")), leafNodeStubCapabilities(), nil)
			if kpErr != nil {
				t.Fatalf("NewKeyPackageWithSigner over a provider whose KDF.Nh is %d: %v", crypto.HashSize(), kpErr)
			}
			return [][]byte{kp.Signature}
		}},
		// the path secret ladder, whose every rung is DeriveSecret at KDF.Nh. A body that cut
		// a rung to a written down 32 answers correctly at both registered suites and hands a
		// short secret to every derivation above it at the first suite whose hash is wider.
		{name: "DerivePathSecrets", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			return DerivePathSecrets(crypto, bytes.Repeat([]byte{0x86}, crypto.HashSize()), 3)
		}},
		// the node key pair, which reads no length off the provider -- what it takes from one
		// is a derivation and a KEM key -- so what this row states is the other half of the
		// equivalence: it must WORK over a provider whose KDF.Nh is not 32, which a body that
		// cut its node_secret to a written down 32 would refuse or silently truncate.
		{name: "DeriveNodeKeyPair", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			nodePriv, nodePub, keyErr := DeriveNodeKeyPair(crypto,
				bytes.Repeat([]byte{0x87}, crypto.HashSize()))
			if keyErr != nil {
				t.Fatalf("DeriveNodeKeyPair over a provider whose KDF.Nh is %d: %v",
					crypto.HashSize(), keyErr)
			}
			return [][]byte{nodePriv, nodePub}
		}},
		// section 6.1's framed content signature and its verify. Neither reads a length off the
		// provider at all -- what they take from one is a signature and a verification -- so
		// what these two rows state is the other half of the equivalence: both must WORK over a
		// provider whose KDF.Nh is not 32. The group context they are built over carries hashes
		// at the provider's own width, so a body that had cut anything about the preimage to a
		// written down 32 refuses here. The signature is the answer read, and at 64 octets it
		// is neither provider's Nh, so it reports no coincidence belonging to a length this
		// gate chose; the verify answers no bytes at all and is named as such.
		{name: "SignAuthenticatedContent", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			authContent, signErr := SignAuthenticatedContent(crypto, framingStubSignaturePriv(),
				WireFormatPrivateMessage, framingStubFramedContent(), framingStubGroupContext(t, crypto))
			if signErr != nil {
				t.Fatalf("SignAuthenticatedContent over a provider whose KDF.Nh is %d: %v",
					crypto.HashSize(), signErr)
			}
			return [][]byte{authContent.Auth.Signature}
		}},
		// RFC 9420 section 7.3's commit door. It reads no length off the provider -- what it
		// takes from one is a verification -- so what this row states is the other half of the
		// equivalence: it must WORK over a provider whose KDF.Nh is not 32, which a body that
		// had cut anything about the LeafNodeTBS to a written down 32 would refuse. It answers
		// no bytes and is named as such.
		{name: "ValidateUpdatePathLeafNode", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			context := &GroupContext{Version: ProtocolVersionMls10, CipherSuite: crypto.Suite(),
				GroupId: []byte("kdf-nh-group"), Epoch: 1}
			path := commitPathValidUnder(t, crypto, context, LeafIndex(1))
			if err := ValidateUpdatePathLeafNode(crypto, context, LeafIndex(1), path); err != nil {
				t.Fatalf("ValidateUpdatePathLeafNode over a provider whose KDF.Nh is %d refused a leaf signed over the same one: %v",
					crypto.HashSize(), err)
			}
			return nil
		}},
		{name: "VerifyAuthenticatedContent", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			groupContext := framingStubGroupContext(t, crypto)
			authContent, signErr := SignAuthenticatedContent(crypto, framingStubSignaturePriv(),
				WireFormatPrivateMessage, framingStubFramedContent(), groupContext)
			if signErr != nil {
				t.Fatalf("sign the message the verify row reads, over a provider whose KDF.Nh is %d: %v",
					crypto.HashSize(), signErr)
			}
			verifyPub, keyErr := signaturePublicKeyOf(framingStubSignaturePriv())
			if keyErr != nil {
				t.Fatalf("the public half of the key the verify row is built over: %v", keyErr)
			}
			if verifyErr := VerifyAuthenticatedContent(crypto, verifyPub, authContent, groupContext); verifyErr != nil {
				t.Fatalf("VerifyAuthenticatedContent over a provider whose KDF.Nh is %d refused a signature made over the same one: %v",
					crypto.HashSize(), verifyErr)
			}
			return nil
		}},
		// section 6.2's membership tag and its verifier. AuthenticatedContentTBMBytes is not
		// here and must not be: it takes no provider, so it is outside the class this gate reads
		// off the package, and a row for it would be a name the coverage comparison at the end
		// cannot account for.
		//
		// The tag is the row this gate is really for. A mac IS KDF.Nh octets, so a body that cut
		// its answer to a written down 32 is indistinguishable from a correct one at both
		// registered suites and separates only here, over a provider whose hash is wider: the
		// tag has to come back Nh long over each, and those two lengths differ. The verifier
		// answers no bytes and is named as such, so what its row states is the other half --
		// that it WORKS at a width neither registered suite has.
		{name: "ComputeMembershipTag", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			groupContext := framingStubGroupContext(t, crypto)
			authContent, signErr := SignAuthenticatedContent(crypto, framingStubSignaturePriv(),
				WireFormatPrivateMessage, framingStubFramedContent(), groupContext)
			if signErr != nil {
				t.Fatalf("sign the message the membership tag row reads, over a provider whose KDF.Nh is %d: %v",
					crypto.HashSize(), signErr)
			}
			tag, tagErr := ComputeMembershipTag(crypto,
				bytes.Repeat([]byte{0x6b}, crypto.HashSize()), authContent, groupContext)
			if tagErr != nil {
				t.Fatalf("ComputeMembershipTag over a provider whose KDF.Nh is %d: %v",
					crypto.HashSize(), tagErr)
			}
			return [][]byte{tag}
		}},
		{name: "verifyMembershipTag", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			groupContext := framingStubGroupContext(t, crypto)
			membershipKey := bytes.Repeat([]byte{0x6b}, crypto.HashSize())
			authContent, signErr := SignAuthenticatedContent(crypto, framingStubSignaturePriv(),
				WireFormatPrivateMessage, framingStubFramedContent(), groupContext)
			if signErr != nil {
				t.Fatalf("sign the message the membership verify row reads, over a provider whose KDF.Nh is %d: %v",
					crypto.HashSize(), signErr)
			}
			tag, tagErr := ComputeMembershipTag(crypto, membershipKey, authContent, groupContext)
			if tagErr != nil {
				t.Fatalf("the tag the membership verify row reads, over a provider whose KDF.Nh is %d: %v",
					crypto.HashSize(), tagErr)
			}
			if verifyErr := verifyMembershipTag(crypto, membershipKey, authContent,
				groupContext, tag); verifyErr != nil {
				t.Fatalf("verifyMembershipTag over a provider whose KDF.Nh is %d refused a tag taken over the same message: %v",
					crypto.HashSize(), verifyErr)
			}
			return nil
		}},
		// section 6.2's seal and open, on the pair above's terms. The seal's answer is the
		// membership tag, which is a mac and therefore KDF.Nh bytes at whatever width the provider
		// runs at, so it is held to the equivalence rather than excused from it; the open answers a
		// view over its own argument and is named as answering no bytes, so what its row states is
		// the other half -- that section 6.2 WORKS at a width neither registered suite has.
		{name: "SealPublicMessage", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			groupContext := framingStubGroupContext(t, crypto)
			content := framingStubFramedContent()
			content.ContentType = ContentTypeProposal
			content.ApplicationData = nil
			content.Proposal = &Proposal{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: 5}}
			authContent, signErr := SignAuthenticatedContent(crypto, framingStubSignaturePriv(),
				WireFormatPublicMessage, content, groupContext)
			if signErr != nil {
				t.Fatalf("sign the message the seal row reads, over a provider whose KDF.Nh is %d: %v",
					crypto.HashSize(), signErr)
			}
			message, sealErr := SealPublicMessage(crypto,
				bytes.Repeat([]byte{0x6b}, crypto.HashSize()), authContent, groupContext)
			if sealErr != nil {
				t.Fatalf("SealPublicMessage over a provider whose KDF.Nh is %d: %v",
					crypto.HashSize(), sealErr)
			}
			return [][]byte{message.MembershipTag}
		}},
		{name: "OpenPublicMessage", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			groupContext := framingStubGroupContext(t, crypto)
			membershipKey := bytes.Repeat([]byte{0x6b}, crypto.HashSize())
			priv, pub, keyErr := crypto.SignatureKeyPair()
			if keyErr != nil {
				t.Fatalf("a key pair for the open row, over a provider whose KDF.Nh is %d: %v",
					crypto.HashSize(), keyErr)
			}
			content := framingStubFramedContent()
			content.ContentType = ContentTypeProposal
			content.ApplicationData = nil
			content.Proposal = &Proposal{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: 5}}
			authContent, signErr := SignAuthenticatedContent(crypto, priv, WireFormatPublicMessage,
				content, groupContext)
			if signErr != nil {
				t.Fatalf("sign the message the open row reads, over a provider whose KDF.Nh is %d: %v",
					crypto.HashSize(), signErr)
			}
			message, sealErr := SealPublicMessage(crypto, membershipKey, authContent, groupContext)
			if sealErr != nil {
				t.Fatalf("seal the message the open row reads, over a provider whose KDF.Nh is %d: %v",
					crypto.HashSize(), sealErr)
			}
			if _, openErr := OpenPublicMessage(crypto, membershipKey, message,
				StaticSignatureKey(pub), groupContext); openErr != nil {
				t.Fatalf("OpenPublicMessage over a provider whose KDF.Nh is %d refused a message it had just sealed: %v",
					crypto.HashSize(), openErr)
			}
			return nil
		}},
		// section 6.3.2's seal and open. Neither answer is a KDF length -- the seal's is twelve
		// octets of sender data plus the aead tag, and the open's is the four octet reuse guard --
		// so the equivalence passes over both without an excuse, and what these rows state is the
		// half that has bitten this construction before: the sender_data_secret is refused against
		// KDF.Nh and the SAMPLE is KDF.Nh octets of the ciphertext, so a body carrying a written
		// down 32 anywhere in either refuses its own secret, or samples past the ciphertext it was
		// handed, the moment the kdf is 48 wide.
		{name: "sealSenderData", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			sealed, sealErr := sealSenderData(crypto,
				bytes.Repeat([]byte{0x6d}, crypto.HashSize()),
				&SenderData{LeafIndex: 2, Generation: 5, ReuseGuard: [4]byte{0x21, 0x22, 0x23, 0x24}},
				&PrivateMessage{GroupId: []byte{0x11, 0x12}, Epoch: 4,
					ContentType: ContentTypeApplication, AuthenticatedData: []byte{0x13}},
				bytes.Repeat([]byte{0x6e}, crypto.HashSize()))
			if sealErr != nil {
				t.Fatalf("sealSenderData over a provider whose KDF.Nh is %d: %v",
					crypto.HashSize(), sealErr)
			}
			return [][]byte{sealed}
		}},
		{name: "openSenderData", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			secret := bytes.Repeat([]byte{0x6d}, crypto.HashSize())
			ciphertext := bytes.Repeat([]byte{0x6e}, crypto.HashSize())
			header := &PrivateMessage{GroupId: []byte{0x11, 0x12}, Epoch: 4,
				ContentType: ContentTypeApplication, AuthenticatedData: []byte{0x13}}
			senderData := &SenderData{LeafIndex: 2, Generation: 5,
				ReuseGuard: [4]byte{0x21, 0x22, 0x23, 0x24}}
			sealed, sealErr := sealSenderData(crypto, secret, senderData, header, ciphertext)
			if sealErr != nil {
				t.Fatalf("seal the sender data the open row reads, over a provider whose KDF.Nh is %d: %v",
					crypto.HashSize(), sealErr)
			}
			opened, openErr := openSenderData(crypto, secret, sealed, header, ciphertext)
			if openErr != nil {
				t.Fatalf("openSenderData over a provider whose KDF.Nh is %d refused a header it had just sealed: %v",
					crypto.HashSize(), openErr)
			}
			return [][]byte{opened.ReuseGuard[:]}
		}},
		// section 6.3's seal, in both its forms, and its open. What the seal answers here is the
		// encrypted sender data rather than the content ciphertext, and that is deliberate: the
		// content ciphertext is a plaintext plus a tag and its length moves with the signature
		// inside it, which is a length this differential has nothing to say about. The sealed
		// sender data is twelve octets and a tag at every width.
		//
		// What these rows state is the half that has bitten this construction's neighbours: the
		// sender_data_secret is refused against KDF.Nh and the ciphertext SAMPLE is KDF.Nh octets
		// of the content ciphertext, so a body carrying a written down 32 anywhere refuses its own
		// secret, or samples past the ciphertext it was handed, the moment the kdf is 48 wide. The
		// message key and nonce are the KEY SOURCE's to size, and this file's double reads both
		// widths off the provider it was built over, so a seal that wrote either down is refused
		// by the AEAD at the wider suite rather than passing quietly.
		{name: "SealPrivateMessage", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			signed := framingPrivateSignedContent(t, crypto, framingTestMemberContent())
			message, sealErr := SealPrivateMessage(crypto, framingNewKeySource(crypto, 0x4b, 0),
				signed.senderDataSecret, signed.authContent, PaddingSizeV1)
			if sealErr != nil {
				t.Fatalf("SealPrivateMessage over a provider whose KDF.Nh is %d: %v",
					crypto.HashSize(), sealErr)
			}
			return [][]byte{message.EncryptedSenderData}
		}},
		{name: "sealPrivateMessage", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			signed := framingPrivateSignedContent(t, crypto, framingTestMemberContent())
			message, sealErr := sealPrivateMessage(crypto, framingNewKeySource(crypto, 0x4b, 0),
				signed.senderDataSecret, signed.authContent, bytes.Repeat([]byte{0x71}, 16))
			if sealErr != nil {
				t.Fatalf("sealPrivateMessage over a provider whose KDF.Nh is %d: %v",
					crypto.HashSize(), sealErr)
			}
			return [][]byte{message.EncryptedSenderData}
		}},
		{name: "OpenPrivateMessage", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			signed := framingPrivateSignedContent(t, crypto, framingTestMemberContent())
			message, sealErr := SealPrivateMessage(crypto, framingNewKeySource(crypto, 0x4b, 0),
				signed.senderDataSecret, signed.authContent, PaddingSizeV1)
			if sealErr != nil {
				t.Fatalf("seal the message the open row reads, over a provider whose KDF.Nh is %d: %v",
					crypto.HashSize(), sealErr)
			}
			if _, openErr := OpenPrivateMessage(crypto, framingNewKeySource(crypto, 0x4b, 0),
				signed.senderDataSecret, message, StaticSignatureKey(signed.pub),
				signed.groupContext); openErr != nil {
				t.Fatalf("OpenPrivateMessage over a provider whose KDF.Nh is %d refused a message it had just sealed: %v",
					crypto.HashSize(), openErr)
			}
			return nil
		}},
		// MASTER section 8.4.2 v2's three bodies, each on the row above it's terms: the seal one
		// layer down, the seal under the S3 pin, and the open one layer down. What is at risk for
		// all three is the same thing as for their delegating names -- the sender data key and
		// nonce come off ExpandWithLabel, so a body that read a width from anywhere but the
		// provider works at 32 and produces a key nobody else computes at 48.
		{name: "sealPrivateMessageAt", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			signed := framingPrivateSignedContent(t, crypto, framingTestMemberContent())
			message, _, sealErr := sealPrivateMessageAt(crypto, framingNewKeySource(crypto, 0x4b, 0),
				signed.senderDataSecret, signed.authContent, bytes.Repeat([]byte{0x71}, 16))
			if sealErr != nil {
				t.Fatalf("sealPrivateMessageAt over a provider whose KDF.Nh is %d: %v",
					crypto.HashSize(), sealErr)
			}
			return [][]byte{message.EncryptedSenderData}
		}},
		{name: "sealPrivateMessageBound", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			signed := framingPrivateSignedContent(t, crypto, framingTestMemberContent())
			message, sealErr := sealPrivateMessageBound(crypto, framingNewKeySource(crypto, 0x4b, 0),
				signed.senderDataSecret, signed.authContent, bytes.Repeat([]byte{0x71}, 16), 0)
			if sealErr != nil {
				t.Fatalf("sealPrivateMessageBound over a provider whose KDF.Nh is %d: %v",
					crypto.HashSize(), sealErr)
			}
			return [][]byte{message.EncryptedSenderData}
		}},
		{name: "openPrivateMessageAt", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			signed := framingPrivateSignedContent(t, crypto, framingTestMemberContent())
			message, sealErr := SealPrivateMessage(crypto, framingNewKeySource(crypto, 0x4b, 0),
				signed.senderDataSecret, signed.authContent, PaddingSizeV1)
			if sealErr != nil {
				t.Fatalf("seal the message the bound open row reads, over a provider whose KDF.Nh is %d: %v",
					crypto.HashSize(), sealErr)
			}
			if _, _, openErr := openPrivateMessageAt(crypto, framingNewKeySource(crypto, 0x4b, 0),
				signed.senderDataSecret, message, StaticSignatureKey(signed.pub),
				signed.groupContext); openErr != nil {
				t.Fatalf("openPrivateMessageAt over a provider whose KDF.Nh is %d refused a message it had just sealed: %v",
					crypto.HashSize(), openErr)
			}
			return nil
		}},
		// the pre-ratchet peek. What it answers is the caller's own authenticated_data, whose
		// width is the caller's and not KDF.Nh -- so what this row states is openSenderData's
		// row's statement and not more: that the call WORKS at either hash width, which for a
		// construction whose key and nonce come off ExpandWithLabel is the property at risk.
		{name: "PeekPrivateMessageSender", call: func(t *testing.T, crypto CryptoProvider) [][]byte {
			signed := framingPrivateSignedContent(t, crypto, framingTestMemberContent())
			message, sealErr := SealPrivateMessage(crypto, framingNewKeySource(crypto, 0x4b, 0),
				signed.senderDataSecret, signed.authContent, PaddingSizeV1)
			if sealErr != nil {
				t.Fatalf("seal the message the peek row reads, over a provider whose KDF.Nh is %d: %v",
					crypto.HashSize(), sealErr)
			}
			marshalled, marshalErr := MarshalMLSMessage(&MLSMessage{
				Version:        ProtocolVersionMls10,
				WireFormat:     WireFormatPrivateMessage,
				PrivateMessage: message,
			})
			if marshalErr != nil {
				t.Fatalf("marshal the message the peek row reads, over a provider whose KDF.Nh is %d: %v",
					crypto.HashSize(), marshalErr)
			}
			_, aad, _, peekErr := PeekPrivateMessageSender(crypto, signed.senderDataSecret, marshalled)
			if peekErr != nil {
				t.Fatalf("PeekPrivateMessageSender over a provider whose KDF.Nh is %d refused a message it had just sealed: %v",
					crypto.HashSize(), peekErr)
			}
			return [][]byte{aad}
		}},
	} {
		covered = append(covered, testCase.name)
		overTheNarrowProvider, raised := recoveringRow(func() [][]byte { return testCase.call(t, narrow) })
		if raised != nil {
			t.Errorf("%s panicked with %v over a provider whose KDF.Nh is %d", testCase.name, raised, narrow.HashSize())
			continue
		}
		overTheWideProvider, raised := recoveringRow(func() [][]byte { return testCase.call(t, wide) })
		if raised != nil {
			t.Errorf("%s panicked with %v over a provider whose KDF.Nh is %d; a construction that reads its lengths off the provider it was handed works at either width",
				testCase.name, raised, wide.HashSize())
			continue
		}
		if len(overTheNarrowProvider) != len(overTheWideProvider) {
			t.Errorf("%s answered %d results at KDF.Nh %d and %d at KDF.Nh %d",
				testCase.name, len(overTheNarrowProvider), narrow.HashSize(),
				len(overTheWideProvider), wide.HashSize())
			continue
		}
		// a construction that answers no bytes has no length for this differential to read, and
		// the class of those is packageConstructionsAnsweringNoBytes rather than a second list:
		// the aliasing gate already holds that table to the package's own declarations in both
		// directions, so a name cannot be here without being a construction that really answers
		// none. What this row still states is that the call WORKS at either width.
		if _, answersNoBytes := packageConstructionsAnsweringNoBytes[testCase.name]; answersNoBytes {
			if len(overTheNarrowProvider) != 0 {
				t.Errorf("%s is named as answering no bytes and answered %d results",
					testCase.name, len(overTheNarrowProvider))
			}
			continue
		}
		if len(overTheNarrowProvider) == 0 {
			t.Errorf("%s answered nothing, so this row compared no length", testCase.name)
			continue
		}
		for _, answer := range overTheNarrowProvider {
			compared++
			if len(answer) == narrow.HashSize() {
				coincidences++
			}
		}
		if reason, excused := constructionsWhoseAnswerOnlyCoincidesWithKdfNh[testCase.name]; excused {
			t.Logf("%s not held to the KDF.Nh equivalence: %s", testCase.name, reason)
			continue
		}
		for _, at := range kdfNhCoincidences(overTheNarrowProvider, overTheWideProvider,
			narrow.HashSize(), wide.HashSize()) {
			t.Errorf("%s answered %d bytes in result %d over a provider whose KDF.Nh is %d and %d bytes over one whose KDF.Nh is %d; exactly one of those is KDF.Nh, so that length is written down rather than read off the provider",
				testCase.name, len(overTheNarrowProvider[at]), at, narrow.HashSize(),
				len(overTheWideProvider[at]), wide.HashSize())
		}
	}

	// the control on the comparison itself. A gate that reported the package clean having
	// compared nothing reports exactly what a complete one reports, so the shape it exists
	// to catch is run through it here and has to be caught, and a shape it must not catch
	// is run through it too.
	writtenDown := func(crypto CryptoProvider) [][]byte { return [][]byte{make([]byte, 32)} }
	if found := kdfNhCoincidences(writtenDown(narrow), writtenDown(wide),
		narrow.HashSize(), wide.HashSize()); !slices.Equal(found, []int{0}) {
		t.Fatalf("the comparison reported %v for a construction that answers a written down 32 bytes whatever provider it is handed, want [0]",
			found)
	}
	readOff := func(crypto CryptoProvider) [][]byte { return [][]byte{ZeroSecret(crypto)} }
	if found := kdfNhCoincidences(readOff(narrow), readOff(wide),
		narrow.HashSize(), wide.HashSize()); len(found) != 0 {
		t.Fatalf("the comparison reported %v for a construction that reads its length off the provider, so every row above is reporting a defect that is not there",
			found)
	}
	// and the rows really did put KDF.Nh lengths through it: a sweep in which no answer was
	// ever Nh long would satisfy the equivalence for every possible implementation
	if coincidences == 0 {
		t.Fatalf("%d answers were compared and none of them was KDF.Nh bytes long over either provider, so this gate held nothing",
			compared)
	}
	t.Logf("%d answers compared across %d constructions, %d of them KDF.Nh bytes at %d",
		compared, len(covered), coincidences, narrow.HashSize())

	// and the table names every construction of this package that takes a provider rather
	// than the ones this test happened to think of
	declared := packageLevelFunctionsTaking(t, providerInterfaceName)
	slices.Sort(covered)
	if !slices.Equal(covered, declared) {
		t.Errorf("this gate covers %v, and the package declares %v", covered, declared)
	}
	for name := range constructionsWhoseAnswerOnlyCoincidesWithKdfNh {
		if !slices.Contains(declared, name) {
			t.Errorf("the gate excuses %s from the KDF.Nh equivalence, and no construction of this package declares it", name)
		}
	}
}

// TestEveryAccessorAnsweringAPointerAnswersIntoTheSchedulesOwnStorage observes the sentence
// above Secrets(): "The pointer is into the schedule's own storage, which is what lets the
// epoch erase them in place."
//
// A body that answered a pointer to a COPY of the struct — copied := self.secrets; return
// &copied — still shares the nine backing arrays, so an erase that overwrites bytes stays
// visible and every other test of this package passes. Measured, not supposed: the whole of
// mls and message was green over that edit. What it breaks is the other spelling of the erase
// this plan's task 12 will write: a Zeroize that NILS the fields leaves the
// returned copy holding nine live keys, and the caller reading through it has an epoch's
// worth of secrets the group believes are gone.
//
// The class is read off the type rather than named: every exported method of *KeySchedule
// with a POINTER among its results, so an accessor added later joins by existing.
//
// It used to read "takes no argument, answers exactly one result, and that result is a
// pointer", which is the same narrowing-by-arity the provisional epoch's accessor class in
// connect/messagegroup was found carrying on 2026-09-09 -- a class derived from the
// PROPERTY and then narrowed by the shape its members happen to have. Measured at 81b97ca
// the complement of that narrowing was EMPTY: no method this type declares was removed by
// it, which is what a filter written for the members that exist looks like from outside. A
// Secrets-shaped accessor taking an epoch, or answering (*scheduleSecrets, error), would
// have dropped out of this gate on the commit that added it and taken the schedule's own
// storage with it. An argument-taking member is now REFUSED rather than skipped, every
// pointer among a method's results is a row rather than only the first, and the complement
// is printed. GATES.md carries the rule and the query.
//
// Three things are asserted, because each is satisfiable by a different wrong
// implementation — two calls answer the same pointer, a write through it is visible to the
// next call, and two schedules answer different pointers, which is what separates "into the
// schedule's own storage" from "into a package level singleton".
func TestEveryAccessorAnsweringAPointerAnswersIntoTheSchedulesOwnStorage(t *testing.T) {
	epochs := ksVectorEpochs(t)
	if len(epochs) < 2 {
		t.Fatalf("the corpus answered for %d epochs and the last assertion here needs two schedules", len(epochs))
	}
	answered := []string{}
	excluded := []string{}
	scheduleType := reflect.TypeOf((*KeySchedule)(nil))
	for i := range scheduleType.NumMethod() {
		method := scheduleType.Method(i)
		pointers := []int{}
		for at := range method.Type.NumOut() {
			if method.Type.Out(at).Kind() == reflect.Pointer {
				pointers = append(pointers, at)
			}
		}
		if len(pointers) == 0 {
			excluded = append(excluded, method.Name+" "+method.Type.String())
			continue
		}
		// an argument-taking member is REFUSED and not skipped, which is the difference
		// between a gate that narrows and a gate that fails closed: this one cannot drive
		// it, and a method that hands out the schedule's own storage must not leave the
		// class by growing a parameter
		if method.Type.NumIn() != 1 {
			t.Errorf("(*KeySchedule).%s answers a pointer and takes %d arguments, so this gate cannot drive it; give it a row rather than letting a method that hands out the schedule's own storage fall outside this class",
				method.Name, method.Type.NumIn()-1)
			continue
		}
		for _, at := range pointers {
			where := fmt.Sprintf("(*KeySchedule).%s result %d", method.Name, at)
			answered = append(answered, where)
			pointerOf := func(schedule *KeySchedule) uintptr {
				return method.Func.Call([]reflect.Value{reflect.ValueOf(schedule)})[at].Pointer()
			}
			schedule := epochs[0].schedule(t)
			first, second := pointerOf(schedule), pointerOf(schedule)
			if first == 0 {
				t.Errorf("%s answered nil, so this row observed nothing", where)
				continue
			}
			if first != second {
				t.Errorf("%s answered a different address on each call, so it is handing back a copy rather than the schedule's own storage; an erase that nils the fields would leave that copy holding live keys",
					where)
			}
			if other := pointerOf(epochs[1].schedule(t)); other == first {
				t.Errorf("%s answered the same address for two different schedules, so it is not answering into either one's own storage",
					where)
			}
		}
	}
	if len(answered) == 0 {
		t.Fatal("no exported method of *KeySchedule answers a pointer, so this gate swept nothing; Secrets() is one")
	}
	t.Logf("the pointer answering class of *KeySchedule is %v; excluded because no result of theirs is a pointer: %v",
		answered, excluded)

	// and the write through, which is the property the aliasing exists for. Reflection can
	// say two calls agree on an address; only a write says the address is the storage the
	// schedule itself reads.
	schedule := epochs[0].schedule(t)
	secrets := schedule.Secrets()
	kept := secrets.InitSecret
	secrets.InitSecret = nil
	if schedule.Secrets().InitSecret != nil {
		t.Error("a field nilled through the pointer Secrets() answered is still set when the schedule is asked again, so that pointer is not into the schedule's own storage")
	}
	secrets.InitSecret = kept
	if !bytes.Equal(schedule.Secrets().InitSecret, kept) {
		t.Error("restoring the field through the same pointer did not reach the schedule, so this row proved nothing about either direction")
	}
}

// TestPastEpochWindowIsThePromisedThirtyTwoEpochs pins the exported constant.
//
// Nothing consumes it yet — the retention it bounds is this plan's task 12 — so there is no
// behaviour to observe and until that task lands the value can be changed to anything with
// the whole package green. Measured, not supposed: 32 replaced by 8 left green the whole of
// mls and message.
//
// It is pinned rather than left because it is already exported, and because the number is a
// product promise rather than a tuning constant: key_schedule.go's own comment argues for it
// against exactly the value a mutation restores — "Thirty-two rather than eight because the
// window is a product promise about how long a laptop may stay closed, and an active group
// can burn eight epochs in a day." A task that means to change it changes this line in the
// same commit, which is a line a reviewer sees.
func TestPastEpochWindowIsThePromisedThirtyTwoEpochs(t *testing.T) {
	if PastEpochWindow != 32 {
		t.Errorf("PastEpochWindow is %d and the promise written above it is thirty-two epochs; if the window is meant to change, change the comment that argues for it and this line together",
			PastEpochWindow)
	}
}

// Task 7: the committer's path and the joiner's path reach one epoch.
//
// This is the first property in this file about two derivations rather than one, and the
// shape it invites cannot fail. The two paths share code -- NewKeySchedule is
// DeriveJoinerSecret followed by NewKeyScheduleFromJoiner -- so
//
//	committer, _ := NewKeySchedule(crypto, initPrev, commitSecret, pskSecret, groupContext)
//	joiner, _ := NewKeyScheduleFromJoiner(crypto, committer.JoinerSecret(), pskSecret, groupContext)
//
// compares one deterministic function against itself on one argument. It reports PASS
// against every implementation of NewKeyScheduleFromJoiner there is, including one that
// derives the epoch from the joiner secret rather than the member secret, one that drops
// psk_secret, and one that returns nine copies of the same byte. That is the shape the plan
// hands this task, and it is written down here rather than fixed silently because the same
// shape will be reachable again in p5 and p6 wherever a joiner is checked against a
// committer.
//
// What separates the two paths is a third derivation neither of them performs: RFC 9420
// section 8 written out below in terms of crypto/hmac, which this package does not derive
// with. The joiner is handed a joiner_secret that reference produced rather than one the
// committer produced, and both schedules are compared against the reference as well as
// against each other -- so an epoch reached by the wrong route disagrees with it even when
// both paths take that route together.
//
// The reference is admissible only because it is anchored outside this tree:
// TestTheHandWrittenSectionEightDerivationReproducesThePublishedEpochs requires it to
// reproduce all 110 answers mlswg published before any comparison below rests on it. A
// second copy of the same mistake agrees with the first, and only somebody else's numbers
// separate them.

// ksGeneratedEpochCases is how many generated epochs each registered suite contributes to
// the space the two paths are compared over.
//
// A worked example pins the epoch it was taken from. What the RFC lets vary here is the
// previous init secret, the commit secret, the psk secret and every field of the
// GroupContext -- and the published corpus holds four of the context's seven fixed across
// all ten of its epochs, so the space below is where an epoch number in the high half, an
// unregistered version, a non empty extensions vector and a group id of another length
// reach the joiner path at all.
const ksGeneratedEpochCases = 24

// ksStream is the deterministic byte source the generated space is drawn from: sha256 over
// a seed and a counter.
//
// Deterministic rather than random, because a failing case has to reproduce. A generated
// space drawn from crypto/rand finds the defect once, on somebody's machine, and the next
// run is a different space that says nothing about whether the fix worked.
type ksStream struct {
	seed    string
	counter uint64
	buffer  []byte
}

// take answers the next n bytes of the stream. Blocks are consumed in order and a partial
// block is carried, so the same seed yields the same bytes whatever sizes the caller asks
// for them in.
func (self *ksStream) take(n int) []byte {
	out := make([]byte, 0, n)
	for len(out) < n {
		if len(self.buffer) == 0 {
			block := sha256.Sum256(fmt.Appendf(nil, "%s/%d", self.seed, self.counter))
			self.counter++
			self.buffer = block[:]
		}
		taken := min(n-len(out), len(self.buffer))
		out = append(out, self.buffer[:taken]...)
		self.buffer = self.buffer[taken:]
	}
	return out
}

// ksHandVarint is the RFC 9420 section 2.1.2 variable length prefix, written here rather
// than reached through syntax so the preimages below owe nothing to the encoder this
// package derives with.
//
// Only the one and two octet forms are written and a longer field is fatal rather than
// truncated. A prefix written at the wrong width produces a perfectly well formed secret
// that no peer derives, which is the failure mode this file exists to catch, and a
// reference that could commit it silently would agree with an implementation that had.
func ksHandVarint(t *testing.T, what string, length int) []byte {
	t.Helper()
	switch {
	case length < 64:
		return []byte{byte(length)}
	case length < 16384:
		return []byte{0x40 | byte(length>>8), byte(length)}
	default:
		t.Fatalf("%s is %d octets, past the two octet prefix this hand written encoder writes", what, length)
		return nil
	}
}

// ksHandExtract is HKDF-Extract: HMAC with the salt as the key and the input keying
// material as the message, in the (salt, ikm) order every spec text writes.
//
// Written out rather than delegated to crypto/hkdf, which takes those two the other way
// round. That transposition is guardrail 1 and it is the one mistake in this package a
// self consistent implementation reproduces perfectly, so a reference that called the
// library would reproduce it too and report agreement.
func ksHandExtract(salt []byte, ikm []byte) []byte {
	extract := hmac.New(sha256.New, salt)
	extract.Write(ikm)
	return extract.Sum(nil)
}

// ksHandExpandWithLabel is ExpandWithLabel: HKDF-Expand over the serialized
// struct { uint16 length; opaque label<V>; opaque context<V> }, with the "MLS 1.0 " prefix
// spelled out here rather than read from MlsLabelPrefix.
//
// One output block only. Every expansion section 8 performs asks for KDF.Nh bytes and
// KDF.Nh is one sha256 output, so the RFC 5869 counter loop would be untested code in a
// reference whose only job is to be obviously right; a longer request is refused instead.
func ksHandExpandWithLabel(t *testing.T, secret []byte, label string, context []byte, length int) []byte {
	t.Helper()
	if length <= 0 || length > sha256.Size {
		t.Fatalf("the hand written expansion was asked for %d octets and writes one sha256 block of %d",
			length, sha256.Size)
	}
	labelled := "MLS 1.0 " + label
	info := []byte{byte(length >> 8), byte(length)}
	info = append(info, ksHandVarint(t, "the label "+labelled, len(labelled))...)
	info = append(info, labelled...)
	info = append(info, ksHandVarint(t, "the context", len(context))...)
	info = append(info, context...)
	expand := hmac.New(sha256.New, secret)
	expand.Write(info)
	expand.Write([]byte{0x01})
	return expand.Sum(nil)[:length]
}

// ksHandDeriveSecret is DeriveSecret: ExpandWithLabel at KDF.Nh over an EMPTY context,
// which is one zero length prefix octet and not an absent field. The difference is one
// byte of preimage and nothing but a published answer can see it.
func ksHandDeriveSecret(t *testing.T, secret []byte, label string) []byte {
	t.Helper()
	return ksHandExpandWithLabel(t, secret, label, nil, sha256.Size)
}

// ksHandJoinerSecret is the half of the section 8 chain only a committer performs:
//
//	joiner_secret = ExpandWithLabel(
//	    KDF.Extract(init_secret_[n-1], commit_secret), "joiner", GroupContext_[n], KDF.Nh)
func ksHandJoinerSecret(t *testing.T, initSecretPrev []byte, commitSecret []byte, groupContext []byte) []byte {
	t.Helper()
	return ksHandExpandWithLabel(
		t, ksHandExtract(initSecretPrev, commitSecret), "joiner", groupContext, sha256.Size)
}

// ksHandEpochLabels is the RFC 9420 section 8 label each field of EpochSecrets is expanded
// under, keyed by the field name so the map is comparable with the type rather than with a
// reading of key_schedule.go.
//
// This is a transcription and it is meant to be one: it is a SECOND independent reading of
// the RFC, which is what a differential comparison is made of. What stops it drifting into
// a copy of the implementation's list is that neither side reads the other, and that both
// are held to the published corpus by the anchor test below. What stops it going stale is
// ksHandEpochSecrets, which requires the two key sets to match exactly in both directions.
var ksHandEpochLabels = map[string]string{
	"SenderData":         "sender data",
	"Encryption":         "encryption",
	"Exporter":           "exporter",
	"External":           "external",
	"Confirmation":       "confirm",
	"Membership":         "membership",
	"ResumptionPsk":      "resumption",
	"EpochAuthenticator": "authentication",
	"InitSecret":         "init",
}

// ksHandEpoch is one epoch derived by hand: the welcome secret and every derived secret,
// keyed by the EpochSecrets field the answer belongs to.
//
// epoch_secret is deliberately not a field, for the reason EpochSecrets does not carry one
// (guardrail 6). The reference has no more need to hand it out than the implementation
// does.
type ksHandEpoch struct {
	welcomeSecret []byte
	secrets       map[string][]byte
}

// ksHandEpochSecrets is the half of the section 8 chain a member added by Welcome
// performs, and the only half it can:
//
//	member_secret  = KDF.Extract(joiner_secret, psk_secret)
//	welcome_secret = DeriveSecret(member_secret, "welcome")
//	epoch_secret   = ExpandWithLabel(member_secret, "epoch", GroupContext_[n], KDF.Nh)
//
// The nine are driven by the field names reflection read off EpochSecrets, and a field
// with no label here is fatal rather than skipped: a tenth secret added to the type would
// otherwise fall outside every comparison below while all of them still reported PASS. The
// count is checked in the other direction too, because a label left behind by a field that
// was renamed is a derivation nothing compares against anything.
func ksHandEpochSecrets(t *testing.T, joinerSecret []byte, pskSecret []byte, groupContext []byte) ksHandEpoch {
	t.Helper()
	memberSecret := ksHandExtract(joinerSecret, pskSecret)
	epochSecret := ksHandExpandWithLabel(t, memberSecret, "epoch", groupContext, sha256.Size)
	epoch := ksHandEpoch{
		welcomeSecret: ksHandDeriveSecret(t, memberSecret, "welcome"),
		secrets:       map[string][]byte{},
	}
	for _, field := range epochSecretFieldNames(t) {
		label, written := ksHandEpochLabels[field]
		if !written {
			t.Fatalf("EpochSecrets.%s has no label in ksHandEpochLabels, so the hand written derivation answers nothing for it and every comparison below would pass over the field in silence",
				field)
		}
		epoch.secrets[field] = ksHandDeriveSecret(t, epochSecret, label)
	}
	if len(epoch.secrets) != len(ksHandEpochLabels) {
		t.Fatalf("EpochSecrets declares %d secrets and ksHandEpochLabels holds %d labels; a label with no field is a derivation nothing is compared against",
			len(epoch.secrets), len(ksHandEpochLabels))
	}
	return epoch
}

// epochSecretFieldNames is the set of secrets an epoch holds, read off the type in sorted
// order so a sweep runs over the class rather than over a list of it. A tenth secret joins
// every sweep that calls this by existing.
func epochSecretFieldNames(t *testing.T) []string {
	t.Helper()
	return slices.Sorted(maps.Keys(epochSecretsByField(t, &EpochSecrets{})))
}

// TestTheHandWrittenSectionEightDerivationReproducesThePublishedEpochs is what makes the
// reference admissible as a second opinion, and it runs before anything rests on it.
//
// A hand written derivation is only a second opinion if it can be wrong in a different way
// from the implementation. Anchoring it on the mlswg corpus is what says it is not a
// paraphrase of key_schedule.go: it reproduces all 110 answers somebody else published, or
// no comparison in the two tests below means anything.
//
// Both Extract calls are checked for symmetry first. A reference that answered the same for
// (a, b) and (b, a) could not see the transposition guardrail 1 exists for, and would agree
// with a swapped implementation while looking like an independent opinion.
func TestTheHandWrittenSectionEightDerivationReproducesThePublishedEpochs(t *testing.T) {
	compared := 0
	for _, epoch := range ksVectorEpochs(t) {
		publishedContext := mustDecodeHex(t, "group_context"+epoch.at, epoch.published.GroupContext)
		joinerSecret := ksHandJoinerSecret(t, epoch.initPrev, epoch.commitSecret, publishedContext)
		if swapped := ksHandJoinerSecret(t, epoch.commitSecret, epoch.initPrev, publishedContext); bytes.Equal(swapped, joinerSecret) {
			t.Fatalf("%s: the hand written joiner derivation answers the same for both Extract argument orders, so it cannot see a transposition",
				epoch.at)
		}
		assertLabelKat(t, "the hand written joiner_secret"+epoch.at, joinerSecret, epoch.published.JoinerSecret)
		compared++

		hand := ksHandEpochSecrets(t, joinerSecret, epoch.pskSecret, publishedContext)
		swapped := ksHandEpochSecrets(t, epoch.pskSecret, joinerSecret, publishedContext)
		if bytes.Equal(swapped.welcomeSecret, hand.welcomeSecret) {
			t.Fatalf("%s: the hand written member derivation answers the same for both Extract argument orders, so it cannot see a transposition",
				epoch.at)
		}
		assertLabelKat(t, "the hand written welcome_secret"+epoch.at, hand.welcomeSecret, epoch.published.WelcomeSecret)
		compared++

		for _, field := range epochSecretFieldNames(t) {
			assertLabelKat(t, "the hand written EpochSecrets."+field+epoch.at,
				hand.secrets[field], publishedEpochSecret(t, field, epoch.published))
			compared++
		}
	}
	if compared != keyScheduleKatEpochComparisons {
		t.Fatalf("compared %d published answers, want %d", compared, keyScheduleKatEpochComparisons)
	}
}

// One generated epoch: the inputs both paths are given, and the encoding of the group
// context they must both expand over.
//
// The encoding is carried rather than re-derived at each use, because the hand written
// reference and the implementation have to be handed the same context bytes for the
// comparison to be about the key schedule. syntax.Marshal is the one thing the reference
// shares with the code under test, and deliberately: the section 8.1 codec has its own
// known answer tests, and a second hand written encoder here would report a codec
// disagreement as a key schedule failure in the wrong file.
type ksGeneratedEpoch struct {
	at           string
	crypto       CryptoProvider
	groupContext *GroupContext
	encoded      []byte
	initPrev     []byte
	commitSecret []byte
	pskSecret    []byte
}

// ksGeneratedEpochs is the space the two paths are compared over: every registered suite,
// crossed with ksGeneratedEpochCases epochs whose secrets and whose whole group context are
// drawn from the deterministic stream.
//
// The suites are taken from the registry rather than named, and the one assumption the
// hand written reference makes -- that the suite's kdf is HKDF-SHA256 at Nh 32 -- is
// asserted against each suite's own parameters. A third suite with another kdf fails here
// and says what to do about it, rather than being compared against a reference computing a
// different primitive.
//
// Two vacuity controls travel with the space. Every case's inputs are required to be
// distinct from every other's, since a generator that had stopped advancing would run the
// same epoch 48 times and report the clean run a varied space reports. And each epoch's two
// Extract arguments are required to differ, because Extract(a, b) and Extract(b, a) agree
// exactly when a == b, so a case whose init and commit secrets coincided would pin nothing
// about the argument order.
func ksGeneratedEpochs(t *testing.T) []ksGeneratedEpoch {
	t.Helper()
	epochs := []ksGeneratedEpoch{}
	for _, suite := range Suites() {
		params, err := LookupSuite(suite)
		if err != nil {
			t.Fatalf("look up suite %#04x: %v", uint16(suite), err)
		}
		if params.KdfId != HpkeKdfHkdfSha256 || params.Nh != sha256.Size {
			t.Fatalf("suite %#04x names kdf %#04x at KDF.Nh %d and the hand written derivation these epochs are compared against is HKDF-SHA256 at %d; teach the reference the new kdf rather than letting the suite go unjudged",
				uint16(suite), uint16(params.KdfId), params.Nh, sha256.Size)
		}
		crypto := mustProvider(t, suite)
		nh := crypto.HashSize()
		stream := &ksStream{seed: fmt.Sprintf("mls/key_schedule/task7/%#04x", uint16(suite))}
		for index := range ksGeneratedEpochCases {
			// the epoch number, big endian off the stream, with the three the corpus can
			// never reach written in: zero, one, and one whose high half is not zero
			epochNumber := uint64(0)
			for _, b := range stream.take(8) {
				epochNumber = epochNumber<<8 | uint64(b)
			}
			switch index {
			case 0:
				epochNumber = 0
			case 1:
				epochNumber = 1
			case 2:
				epochNumber = ^uint64(0)
			}
			// an unregistered version and an unregistered suite code point on some cases:
			// the context carries what it was given, and a preimage that normalised either
			// would still match every published vector, all of which are 0x0001 and 0x0003
			version := ProtocolVersionMls10
			if index%5 == 4 {
				version = ProtocolVersion(0xfafb)
			}
			contextSuite := suite
			if index%7 == 3 {
				contextSuite = CipherSuite(0xf00d)
			}
			// absent, empty, one and two extensions. Every published key schedule vector
			// carries an empty extensions vector, so this is the only place the extension
			// codec reaches an epoch derivation at all.
			var extensions []Extension
			switch index % 4 {
			case 1:
				extensions = []Extension{}
			case 2:
				extensions = []Extension{
					{ExtensionType: ExtensionTypeUrmessageGroupPolicy, ExtensionData: stream.take(1 + index%9)},
				}
			case 3:
				extensions = []Extension{
					{ExtensionType: ExtensionTypeRatchetTree, ExtensionData: stream.take(index % 5)},
					{ExtensionType: ExtensionType(0xf00d), ExtensionData: stream.take(1 + index%13)},
				}
			}
			groupContext := &GroupContext{
				Version:                 version,
				CipherSuite:             contextSuite,
				GroupId:                 stream.take(1 + index%23),
				Epoch:                   epochNumber,
				TreeHash:                stream.take(nh),
				ConfirmedTranscriptHash: stream.take(nh),
				Extensions:              extensions,
			}
			encoded, err := syntax.Marshal(groupContext)
			if err != nil {
				t.Fatalf("suite %#04x generated epoch %d: marshal the group context: %v",
					uint16(suite), index, err)
			}
			epochs = append(epochs, ksGeneratedEpoch{
				at:           fmt.Sprintf(" suite %#04x generated epoch %d", uint16(suite), index),
				crypto:       crypto,
				groupContext: groupContext,
				encoded:      encoded,
				initPrev:     stream.take(nh),
				commitSecret: stream.take(nh),
				pskSecret:    stream.take(nh),
			})
		}
	}
	if want := len(Suites()) * ksGeneratedEpochCases; len(epochs) != want {
		t.Fatalf("the generated space holds %d epochs, want %d", len(epochs), want)
	}
	seen := map[string]string{}
	for _, epoch := range epochs {
		if bytes.Equal(epoch.initPrev, epoch.commitSecret) {
			t.Fatalf("%s: init_secret_[n-1] and commit_secret are the same bytes, so this case agrees with a transposed Extract and pins nothing about the argument order",
				epoch.at)
		}
		key := fmt.Sprintf("%x/%x/%x/%x", epoch.encoded, epoch.initPrev, epoch.commitSecret, epoch.pskSecret)
		if first, repeated := seen[key]; repeated {
			t.Fatalf("%s and %s were generated with identical inputs, so this space is smaller than the count above says",
				first, epoch.at)
		}
		seen[key] = epoch.at
	}
	return epochs
}

// TestTheJoinerPathAndTheCommitterPathReachOneEpoch is the deliverable of this task: a
// member who was in the group at the commit and a member who arrived by Welcome hold the
// same nine secrets, over a generated space rather than at one worked example.
//
// The joiner is handed a joiner_secret the hand written reference produced, NOT the one the
// committer's schedule carries. That is the difference between this test and one that
// cannot fail: NewKeySchedule ends in NewKeyScheduleFromJoiner, so feeding the joiner the
// committer's own answer runs one deterministic function twice on one argument and compares
// the results.
//
// Three comparisons are made per secret and they fail on different mistakes. Committer
// against joiner is the property in the name. Joiner against the reference is what sees a
// mistake INSIDE the shared constructor -- a parent that is not member_secret, a psk secret
// dropped, a label pasted from the line above -- which both paths would otherwise commit
// together and agree about. Committer against the reference is the same for the half of the
// chain only the committer runs, so a joiner derivation that agreed with the reference
// while the committer reached it by another route is still reported.
//
// The nine are driven by the field names reflection read off EpochSecrets rather than by a
// list, so a tenth secret is compared by existing rather than by somebody remembering to
// add it here. The number of comparisons is counted against that field count and against
// the size of the space, because a sweep whose inner loop stopped producing rows reports
// exactly what a complete one reports.
func TestTheJoinerPathAndTheCommitterPathReachOneEpoch(t *testing.T) {
	names := epochSecretFieldNames(t)
	if len(names) == 0 {
		t.Fatal("EpochSecrets read as no fields, so this sweep compares nothing")
	}
	generated := ksGeneratedEpochs(t)
	compared := 0
	for _, epoch := range generated {
		joinerSecret := ksHandJoinerSecret(t, epoch.initPrev, epoch.commitSecret, epoch.encoded)
		if len(joinerSecret) != epoch.crypto.HashSize() {
			t.Fatalf("%s: the hand written joiner secret is %d bytes and KDF.Nh is %d",
				epoch.at, len(joinerSecret), epoch.crypto.HashSize())
		}
		committer, err := NewKeySchedule(
			epoch.crypto, epoch.initPrev, epoch.commitSecret, epoch.pskSecret, epoch.groupContext)
		if err != nil {
			t.Fatalf("%s: NewKeySchedule: %v", epoch.at, err)
		}
		// the joiner is given its own copy of the context, as a real one would be: it
		// decodes the GroupInfo it was sent rather than sharing the committer's struct
		joiner, err := NewKeyScheduleFromJoiner(
			epoch.crypto, joinerSecret, epoch.pskSecret, epoch.groupContext.Clone())
		if err != nil {
			t.Fatalf("%s: NewKeyScheduleFromJoiner: %v", epoch.at, err)
		}
		hand := ksHandEpochSecrets(t, joinerSecret, epoch.pskSecret, epoch.encoded)

		// the committer must have derived the very joiner secret the joiner was handed, or
		// the agreement below would be two schedules agreeing about different epochs
		if !bytes.Equal(committer.JoinerSecret(), joinerSecret) {
			t.Fatalf("%s: the committer derived joiner_secret %x and section 8 gives %x, so the schedules compared below are not two paths into one epoch",
				epoch.at, committer.JoinerSecret(), joinerSecret)
		}
		if !bytes.Equal(committer.WelcomeSecret(), joiner.WelcomeSecret()) {
			t.Errorf("%s: welcome_secret differs between the paths: committer %x, joiner %x",
				epoch.at, committer.WelcomeSecret(), joiner.WelcomeSecret())
		}
		if !bytes.Equal(joiner.WelcomeSecret(), hand.welcomeSecret) {
			t.Errorf("%s: the joiner path derived welcome_secret %x and section 8 gives %x",
				epoch.at, joiner.WelcomeSecret(), hand.welcomeSecret)
		}
		// both paths must have expanded over the same context bytes, and over the ones the
		// codec produces for the context they were given: two schedules that agreed on nine
		// secrets derived over two different preimages would be a coincidence worth hearing
		// about rather than the property in the name
		if !bytes.Equal(committer.GroupContextBytes(), joiner.GroupContextBytes()) {
			t.Errorf("%s: the two paths expanded over different group context bytes: %x and %x",
				epoch.at, committer.GroupContextBytes(), joiner.GroupContextBytes())
		}
		if !bytes.Equal(joiner.GroupContextBytes(), epoch.encoded) {
			t.Errorf("%s: the joiner expanded over %x and the context encodes to %x",
				epoch.at, joiner.GroupContextBytes(), epoch.encoded)
		}

		committerSecrets := epochSecretsByField(t, committer.Secrets())
		joinerSecrets := epochSecretsByField(t, joiner.Secrets())
		for _, name := range names {
			want, derived := hand.secrets[name]
			if !derived {
				t.Fatalf("%s: the hand written derivation answers nothing for EpochSecrets.%s", epoch.at, name)
			}
			if !bytes.Equal(committerSecrets[name], joinerSecrets[name]) {
				t.Errorf("%s: %s differs between the paths: committer %x, joiner %x",
					epoch.at, name, committerSecrets[name], joinerSecrets[name])
			}
			if !bytes.Equal(joinerSecrets[name], want) {
				t.Errorf("%s: the joiner path derived %s = %x and section 8 gives %x",
					epoch.at, name, joinerSecrets[name], want)
			}
			if !bytes.Equal(committerSecrets[name], want) {
				t.Errorf("%s: the committer path derived %s = %x and section 8 gives %x",
					epoch.at, name, committerSecrets[name], want)
			}
			compared++
		}
	}
	if want := len(generated) * len(names); compared != want {
		t.Fatalf("compared %d secrets, want %d", compared, want)
	}
}

// TestTheJoinerPathBindsEveryFieldOfTheGroupContext walks what the corpus cannot: a joiner
// handed the right joiner_secret and a group context that differs in one field must derive
// a different epoch.
//
// This is the failure a joiner cannot detect for itself. It has no previous epoch to
// compare against and no commit to reprocess; the GroupInfo it was sent is the whole of
// what it knows about the group. If a field of that context is not in the preimage, a
// joiner handed a swapped tree hash or a stale epoch number derives the epoch anyway, is
// admitted, and reads traffic from a group whose membership it never verified.
//
// The fields come from mutatedGroupContexts, which builds one case per declared field, so a
// field added to GroupContext by a later plan is judged by existing. Each is required to
// move ALL nine secrets rather than one of them, since epoch_secret is the parent of all
// nine and a field that moved only some of them would mean something stranger than an
// unbound field.
//
// welcome_secret is required NOT to move, which is the same claim from the other side.
// member_secret is Extract(joiner_secret, psk_secret) and the group context enters only at
// the epoch expansion, so a welcome secret that followed the context would be a Welcome no
// peer could open -- and mixing the context in "for safety" is exactly the shape that would
// pass a test asserting only that things change.
func TestTheJoinerPathBindsEveryFieldOfTheGroupContext(t *testing.T) {
	names := epochSecretFieldNames(t)
	fields := reflect.TypeOf(GroupContext{}).NumField()
	if fields == 0 {
		t.Fatal("GroupContext declares no field, so this gate compares nothing")
	}
	generated := ksGeneratedEpochs(t)
	observed := 0
	for _, epoch := range generated {
		joinerSecret := ksHandJoinerSecret(t, epoch.initPrev, epoch.commitSecret, epoch.encoded)
		base, err := NewKeyScheduleFromJoiner(
			epoch.crypto, joinerSecret, epoch.pskSecret, epoch.groupContext)
		if err != nil {
			t.Fatalf("%s: NewKeyScheduleFromJoiner over the base context: %v", epoch.at, err)
		}
		baseSecrets := epochSecretsByField(t, base.Secrets())
		cases := mutatedGroupContexts(t, epoch.groupContext)
		if len(cases) != fields {
			t.Fatalf("%s: GroupContext declares %d fields and this gate built %d cases",
				epoch.at, fields, len(cases))
		}
		for _, field := range slices.Sorted(maps.Keys(cases)) {
			changed, err := NewKeyScheduleFromJoiner(
				epoch.crypto, joinerSecret, epoch.pskSecret, cases[field])
			if err != nil {
				t.Errorf("%s: GroupContext.%s changed: NewKeyScheduleFromJoiner: %v", epoch.at, field, err)
				continue
			}
			changedSecrets := epochSecretsByField(t, changed.Secrets())
			for _, name := range names {
				if bytes.Equal(changedSecrets[name], baseSecrets[name]) {
					t.Errorf("%s: changing GroupContext.%s left %s unchanged, so a joiner handed a swapped %s derives the epoch anyway and joins a group it never agreed to",
						epoch.at, field, name, field)
				}
				observed++
			}
			if !bytes.Equal(changed.WelcomeSecret(), base.WelcomeSecret()) {
				t.Errorf("%s: changing GroupContext.%s moved welcome_secret from %x to %x; member_secret is Extract(joiner_secret, psk_secret) and the context enters only at the epoch expansion, so a Welcome sealed under this key opens for nobody",
					epoch.at, field, base.WelcomeSecret(), changed.WelcomeSecret())
			}
		}
	}
	if want := len(generated) * fields * len(names); observed != want {
		t.Fatalf("observed %d field comparisons, want %d", observed, want)
	}
}

// ---------------------------------------------------------------------------
// Task 8: MLS-Exporter, RFC 9420 section 8.5.
//
// This is the one primitive of the key schedule that carries a PRODUCT feature rather than
// only the protocol. MLS specifies no delivery service and no long term storage, so
// URmessage layers seed recovery on the exporter: each epoch's exported secret is wrapped to
// the member's recovery key, and a defect here is a defect in the ability to restore an
// account from a seedphrase. Its known answer therefore matters exactly as much as the key
// schedule's own, and the three separations it rests on — the label, the context and the
// length — are each observed on their own below, because each is a different mistake and
// each is invisible to the other two.
//
// The corpus pins one exporter answer per epoch, at a length that IS KDF.Nh for both
// registered suites. So the known answer alone cannot see a body that hands back KDF.Nh
// bytes whatever it was asked for, and TestKeyScheduleExportHonoursTheRequestedLengthExactly
// is what does.
// ---------------------------------------------------------------------------

// exporterOf is the corpus's published exporter question and its answer for one epoch.
//
// The label is read as the ASCII STRING the mlswg format documents it to be. It happens to be
// spelled in hex digits and every sibling field of it is hex, so reading it as hex is the
// natural mistake; it builds a different preimage and matches nothing.
// TestKeyScheduleExportReadsTheVectorLabelAsAStringAndNotAsHex is what says the two readings
// really do differ, rather than leaving that to this comment.
//
// Both halves of the question are checked as they are decoded. An epoch whose label were
// empty would agree with an implementation that ignored the label, and one whose context were
// empty would be weaker against an implementation that ignored the context, so a corpus that
// stopped carrying either is reported here rather than quietly making the sweep below softer.
func exporterOf(t *testing.T, epoch ksVectorEpoch) (label string, context []byte, length int, want string) {
	t.Helper()
	published := epoch.published.Exporter
	if published.Label == "" {
		t.Fatalf("%s: the corpus exporter label is empty, so an implementation that ignored the label would agree with this epoch",
			epoch.at)
	}
	if published.Length <= 0 {
		t.Fatalf("%s: the corpus exporter length is %d, so this epoch pins nothing about how many bytes are produced",
			epoch.at, published.Length)
	}
	// mustDecodeHex refuses a context that decodes to nothing, which is the other half of
	// the same vacuity check
	return published.Label,
		mustDecodeHex(t, "exporter context"+epoch.at, published.Context),
		published.Length,
		published.Secret
}

// TestKeyScheduleExportMatchesTheMlswgKeySchedule is the deliverable of this task: the
// exporter answer mlswg published for every epoch of the corpus, through the KeySchedule type
// rather than through the primitives.
//
// crypto_labels_test.go already holds the same answers to the primitives. This is a different
// claim: that the type wires them together in the right order, over the right one of its nine
// secrets, with the caller's context hashed and the inner "exported" label supplied. Every one
// of those is a change of two characters or fewer that produces a perfectly well formed
// secret, and the only thing that separates the right one from the wrong one is a value
// somebody else published.
func TestKeyScheduleExportMatchesTheMlswgKeySchedule(t *testing.T) {
	compared := 0
	for _, epoch := range ksVectorEpochs(t) {
		label, context, length, want := exporterOf(t, epoch)
		exported, err := epoch.schedule(t).Export(label, context, length)
		if err != nil {
			t.Fatalf("%s: Export: %v", epoch.at, err)
		}
		assertLabelKat(t, "exporter"+epoch.at, exported, want)
		compared++
	}
	if compared != keyScheduleKatEpochs {
		t.Fatalf("compared %d published exporter answers, want %d", compared, keyScheduleKatEpochs)
	}
}

// TestKeyScheduleExportReadsTheVectorLabelAsAStringAndNotAsHex is what makes the known answer
// above mean something.
//
// The corpus label is 64 ASCII characters that are all hex digits, sitting in a json object
// where every other string is hex. An implementation — or a test — that decoded it would build
// a 32 byte label, expand over a different preimage and get a different answer, and the whole
// weight of the vector rests on which of the two readings is the right one. This asserts they
// really are different answers, so a KAT that passed under one reading could not also have
// passed under the other.
func TestKeyScheduleExportReadsTheVectorLabelAsAStringAndNotAsHex(t *testing.T) {
	rows := 0
	for _, epoch := range ksVectorEpochs(t) {
		label, context, length, _ := exporterOf(t, epoch)
		decoded, err := hex.DecodeString(label)
		if err != nil {
			t.Fatalf("%s: the corpus label %q does not decode as hex at all, so this gate separates nothing; it exists because the label IS spelled in hex digits",
				epoch.at, label)
		}
		schedule := epoch.schedule(t)
		asString, err := schedule.Export(label, context, length)
		if err != nil {
			t.Fatalf("%s: Export under the label as a string: %v", epoch.at, err)
		}
		asHex, err := schedule.Export(string(decoded), context, length)
		if err != nil {
			t.Fatalf("%s: Export under the hex decoded label: %v", epoch.at, err)
		}
		if bytes.Equal(asString, asHex) {
			t.Errorf("%s: the label read as a string and the same label hex decoded produce one answer, so the label is not reaching the derivation and the published exporter value pins nothing",
				epoch.at)
		}
		rows++
	}
	if rows == 0 {
		t.Fatal("no epoch reached the comparison, so this gate held nothing")
	}
}

// exportGridLabels and exportGridContexts are the grid the separation gate below sweeps.
//
// The labels include a pair differing in one character and a pair where one is a prefix of the
// other, because ExpandWithLabel length prefixes its fields and a body that concatenated them
// instead would collide exactly on the second pair. The contexts include one byte, two bytes
// and ninety seven bytes for the same reason on the other side, and a one byte difference so
// the property is not satisfied by a body that only reads the context's length.
var (
	exportGridLabels = []string{
		"",
		"URmessage/v1/storage",
		"URmessage/v1/storagf",
		"URmessage/v1/storage/",
	}
	exportGridContexts = [][]byte{
		{0x00},
		{0x01},
		{0x01, 0x00},
		bytes.Repeat([]byte{0xa5}, 97),
	}
)

// TestKeyScheduleExportSeparatesItsLabelAndItsContextIndependently observes the property the
// known answer cannot: that BOTH inputs reach the derivation, and each on its own.
//
// A body that ignored the context hands every caller under one label the same secret however
// they varied it, which is a set of exports that look distinct at every call site and are one
// key. A body that ignored the label does the same thing one level up. Neither shows up in a
// vector that varies both at once, and neither shows up in a round trip, a length check or a
// distinctness check over a single call — which is why the grid varies one at a time and every
// pair is compared.
//
// The nil and empty contexts are asserted to AGREE rather than to differ, because the context
// is hashed: Hash(nil) and Hash([]byte{}) are one value, so a caller writing the MASTER
// section 7 storage call either way gets one secret. A body that passed the context through
// instead of hashing it would separate those two, which is the opposite failure.
func TestKeyScheduleExportSeparatesItsLabelAndItsContextIndependently(t *testing.T) {
	type gridKey struct {
		label   int
		context int
	}
	compared := 0
	for _, epoch := range ksVectorEpochs(t) {
		schedule := epoch.schedule(t)
		length := epoch.crypto.HashSize()
		exported := map[gridKey][]byte{}
		for i, label := range exportGridLabels {
			for j, context := range exportGridContexts {
				answer, err := schedule.Export(label, context, length)
				if err != nil {
					t.Fatalf("%s: Export(%q, %x, %d): %v", epoch.at, label, context, length, err)
				}
				if len(answer) != length {
					t.Fatalf("%s: Export(%q, %x, %d) answered %d bytes",
						epoch.at, label, context, length, len(answer))
				}
				exported[gridKey{label: i, context: j}] = answer
			}
		}
		for first, firstAnswer := range exported {
			for second, secondAnswer := range exported {
				if first == second {
					continue
				}
				compared++
				if !bytes.Equal(firstAnswer, secondAnswer) {
					continue
				}
				switch {
				case first.label == second.label:
					t.Errorf("%s: two exports under label %q with contexts %x and %x are the same secret, so the context is not reaching the derivation and every 'different' export under this label is one key",
						epoch.at, exportGridLabels[first.label],
						exportGridContexts[first.context], exportGridContexts[second.context])
				case first.context == second.context:
					t.Errorf("%s: two exports under labels %q and %q with one context are the same secret, so the label is not reaching the derivation",
						epoch.at, exportGridLabels[first.label], exportGridLabels[second.label])
				default:
					t.Errorf("%s: Export(%q, %x) and Export(%q, %x) are the same secret",
						epoch.at, exportGridLabels[first.label], exportGridContexts[first.context],
						exportGridLabels[second.label], exportGridContexts[second.context])
				}
			}
		}

		// the other direction: the context is HASHED, so its two spellings of "no context"
		// are one context
		none, err := schedule.Export("URmessage/v1/storage", nil, length)
		if err != nil {
			t.Fatalf("%s: Export over a nil context: %v", epoch.at, err)
		}
		empty, err := schedule.Export("URmessage/v1/storage", []byte{}, length)
		if err != nil {
			t.Fatalf("%s: Export over an empty context: %v", epoch.at, err)
		}
		if !bytes.Equal(none, empty) {
			t.Errorf("%s: a nil context and an empty one answered different secrets, so the context is being passed through rather than hashed and the MASTER section 7 storage call depends on which spelling its caller used",
				epoch.at)
		}
	}
	// the grid really ran: a sweep whose inner loops produced no pair reports the clean run a
	// full one reports
	if want := keyScheduleKatEpochs * len(exportGridLabels) * len(exportGridContexts) *
		(len(exportGridLabels)*len(exportGridContexts) - 1); compared != want {
		t.Fatalf("compared %d pairs of exports, want %d", compared, want)
	}
}

// TestKeyScheduleExportHashesItsContextSoACallerMayPassAnyLength observes the sentence
// Export's own comment writes, and it is the one claim about the context that nothing in the
// corpus can make: every published exporter context is KDF.Nh octets long.
//
// The context reaches the expansion as an opaque<V> field of the KDFLabel preimage, and that
// field's length prefix is bounded by syntax.MaxVectorLength. So a body that passed the
// caller's context THROUGH instead of hashing it reproduces nothing published — the known
// answer catches it — but a body that hashed it in the corpus's range and passed it through
// outside would not be caught by any comparison against the corpus at all, and would panic out
// of the syntax package the first time a caller passed a context bigger than a megabyte.
// For URmessage's seed recovery that context is a caller's data and not this package's.
//
// Hashing first makes the field KDF.Nh octets whatever arrived, and the second half asserts
// the whole of the caller's context reached the hash rather than a prefix of it.
func TestKeyScheduleExportHashesItsContextSoACallerMayPassAnyLength(t *testing.T) {
	epoch := ksVectorEpochs(t)[0]
	schedule := epoch.schedule(t)
	length := epoch.crypto.HashSize()
	// one octet past the longest vector the preimage encoder will write, so a context that
	// reached that encoder unhashed could not be encoded at all
	oversized := bytes.Repeat([]byte{0x77}, syntax.MaxVectorLength+1)
	// with the panic caught rather than taken: the failure this row exists for IS a panic,
	// raised inside the syntax package, and taking it would end the test binary and every
	// gate declared after this one with it
	answer, raised := recoveringRow(func() exportAnswer {
		exported, err := schedule.Export("URmessage/v1/storage", oversized, length)
		return exportAnswer{exported: exported, err: err}
	})
	if raised != nil {
		t.Fatalf("Export over a %d octet context panicked with %v; the context is reaching the KDFLabel preimage unhashed, where syntax.MaxVectorLength of %d bounds it",
			len(oversized), raised, syntax.MaxVectorLength)
	}
	if answer.err != nil {
		t.Fatalf("Export over a %d octet context: %v", len(oversized), answer.err)
	}
	if len(answer.exported) != length {
		t.Fatalf("Export over a %d octet context answered %d bytes, want %d",
			len(oversized), len(answer.exported), length)
	}
	// and the whole of it was hashed: one octet changed at the far end changes the answer,
	// which a body that hashed a prefix of the caller's context would not
	altered := bytes.Clone(oversized)
	altered[len(altered)-1] ^= 0x01
	other, err := schedule.Export("URmessage/v1/storage", altered, length)
	if err != nil {
		t.Fatalf("Export over the altered context: %v", err)
	}
	if bytes.Equal(answer.exported, other) {
		t.Error("changing the last octet of the context did not change the export, so only a prefix of the caller's context reaches the derivation")
	}
}

// TestKeyScheduleExportHonoursTheRequestedLengthExactly is the property the corpus cannot
// hold, because both registered suites publish their exporter answer at exactly KDF.Nh bytes.
//
// So a body that expanded at KDF.Nh whatever it was asked for reproduces all ten published
// answers, passes every separation above, and hands a caller asking for 64 bytes of key
// material 32 bytes of it — or, spelled the other way, truncates a longer request to the hash
// size and returns short. URmessage's seed recovery asks for what its own format needs, which
// is a caller's number and not the suite's.
//
// The lengths are read off the provider so the row set moves with the suite, and they
// deliberately straddle KDF.Nh in both directions and include the 255*KDF.Nh ceiling, which is
// the largest HKDF-Expand can produce.
//
// The prefix comparison at the end is a separate claim: MLS binds the length INTO the
// expansion's preimage, so two lengths under one label and context are unrelated secrets
// rather than one stream cut to two sizes. A body that expanded once at the ceiling and
// sliced would satisfy every length check above and disagree with every peer.
func TestKeyScheduleExportHonoursTheRequestedLengthExactly(t *testing.T) {
	compared := 0
	for _, epoch := range ksVectorEpochs(t) {
		schedule := epoch.schedule(t)
		nh := epoch.crypto.HashSize()
		if nh < 4 {
			t.Fatalf("%s: KDF.Nh is %d, and the rows below assume a hash with room either side of it", epoch.at, nh)
		}
		answers := map[int][]byte{}
		for _, length := range []int{0, 1, nh - 1, nh, nh + 1, 2 * nh, 2*nh + 7, 255 * nh} {
			exported, err := schedule.Export("URmessage/v1/storage", exportSweepContext, length)
			if err != nil {
				t.Fatalf("%s: Export at %d bytes: %v", epoch.at, length, err)
			}
			if len(exported) != length {
				t.Errorf("%s: Export asked for %d bytes answered %d; a caller of the exporter gets the length it asked for or an error, never a different length",
					epoch.at, length, len(exported))
			}
			answers[length] = exported
			compared++
		}
		short, long := answers[nh], answers[2*nh]
		if len(short) != nh || len(long) != 2*nh {
			continue
		}
		if bytes.Equal(long[:nh], short) {
			t.Errorf("%s: the %d byte export is the first %d bytes of the %d byte one, so the length is not bound into the expansion's preimage and two lengths under one label are one secret",
				epoch.at, nh, nh, 2*nh)
		}
	}
	if want := keyScheduleKatEpochs * 8; compared != want {
		t.Fatalf("compared %d lengths, want %d", compared, want)
	}
}

// exportAnswer is one Export result carried through recoveringRow, which is generic over a
// single value and cannot carry a pair.
type exportAnswer struct {
	exported []byte
	err      error
}

// TestKeyScheduleExportRefusesALengthOutsideTheKdfRange asserts the two ends of the range are
// typed refusals rather than a panic out of the provider or a silently different length.
//
// CryptoProvider.Expand panics on both, which is right for the call sites that ask for a length
// their suite fixes and wrong for this one: the exporter's length is a CALLER's number, so it
// arrives as an error a caller can handle. The negative side matters as much as the positive
// one — crypto/hkdf reaches make([]byte, 0, keyLen) before any check of its own, so a negative
// length that got past here is a makeslice panic from inside the standard library.
func TestKeyScheduleExportRefusesALengthOutsideTheKdfRange(t *testing.T) {
	refused := 0
	for _, epoch := range ksVectorEpochs(t) {
		schedule := epoch.schedule(t)
		nh := epoch.crypto.HashSize()
		for _, length := range []int{-1, -nh, 255*nh + 1, 1 << 30} {
			// with the panic caught rather than taken, for the reason recoveringRow gives.
			// A length gate that stopped gating does not answer a wrong error here: the
			// negative rows reach make([]byte, 0, keyLen) inside crypto/hkdf, which is a
			// panic, and a panic in this row would take the test BINARY down and with it
			// every gate declared after it. Measured, not supposed: dropping the negative
			// half of the gate ran 8 of the 14 tests of this sweep before the binary died.
			answer, raised := recoveringRow(func() exportAnswer {
				exported, err := schedule.Export("URmessage/v1/storage", exportSweepContext, length)
				return exportAnswer{exported: exported, err: err}
			})
			refused++
			if raised != nil {
				t.Errorf("%s: Export at %d bytes panicked with %v; a length outside the kdf's range is a caller's number and comes back as ErrExportLength",
					epoch.at, length, raised)
				continue
			}
			if !errors.Is(answer.err, ErrExportLength) {
				t.Errorf("%s: Export at %d bytes answered %v, want ErrExportLength", epoch.at, length, answer.err)
			}
			if answer.exported != nil {
				t.Errorf("%s: Export at %d bytes refused and answered %d bytes as well",
					epoch.at, length, len(answer.exported))
			}
		}
		// and the boundary itself is inside the range rather than one past it
		if exported, err := schedule.Export("URmessage/v1/storage", exportSweepContext, 255*nh); err != nil {
			t.Errorf("%s: Export at 255*KDF.Nh = %d bytes was refused with %v, and that is exactly what HKDF-Expand can produce",
				epoch.at, 255*nh, err)
		} else if len(exported) != 255*nh {
			t.Errorf("%s: Export at 255*KDF.Nh answered %d bytes, want %d", epoch.at, len(exported), 255*nh)
		}
	}
	if want := keyScheduleKatEpochs * 4; refused != want {
		t.Fatalf("%d lengths were refused, want %d", refused, want)
	}
}

// TestKeyScheduleExportReadsItsCeilingFromTheProvider is the input the registered suites
// cannot supply: both fix KDF.Nh at 32, so 255*KDF.Nh is 8160 for both and a body that wrote
// 8160 down — or wrote 32 down and multiplied — answers correctly for every suite this package
// registers and incorrectly for the first one it does not.
//
// Both directions are asserted, because either alone is satisfiable by a different mistake. A
// length inside the wide provider's range and outside the narrow one's must be ACCEPTED over
// the wide provider, which a written down 8160 refuses; and the wide provider's own ceiling
// must still be refused one past it, so the number moved rather than disappearing.
func TestKeyScheduleExportReadsItsCeilingFromTheProvider(t *testing.T) {
	narrow := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	wide := &wideKdfProvider{CryptoProvider: narrow}
	if narrow.HashSize() == wide.HashSize() {
		t.Fatalf("both providers answer KDF.Nh %d, so this gate separates nothing", narrow.HashSize())
	}
	groupContext := ksVectorEpoch0GroupContext(t)
	narrowSchedule, err := NewKeyScheduleFromEpochSecret(
		narrow, bytes.Repeat([]byte{0x61}, narrow.HashSize()), groupContext)
	if err != nil {
		t.Fatalf("build the schedule over the narrow provider: %v", err)
	}
	wideSchedule, err := NewKeyScheduleFromEpochSecret(
		wide, bytes.Repeat([]byte{0x62}, wide.HashSize()), groupContext)
	if err != nil {
		t.Fatalf("build the schedule over the wide provider: %v", err)
	}

	beyondTheNarrowCeiling := 255*narrow.HashSize() + 1
	if _, err := narrowSchedule.Export("URmessage/v1/storage", nil, beyondTheNarrowCeiling); !errors.Is(err, ErrExportLength) {
		t.Errorf("a provider whose KDF.Nh is %d accepted %d bytes, which is past its own 255*KDF.Nh, answering %v",
			narrow.HashSize(), beyondTheNarrowCeiling, err)
	}
	exported, err := wideSchedule.Export("URmessage/v1/storage", nil, beyondTheNarrowCeiling)
	if err != nil {
		t.Errorf("a provider whose KDF.Nh is %d refused %d bytes with %v, and that is inside its own 255*KDF.Nh of %d; the ceiling is written down rather than read off the provider",
			wide.HashSize(), beyondTheNarrowCeiling, err, 255*wide.HashSize())
	} else if len(exported) != beyondTheNarrowCeiling {
		t.Errorf("the wide provider answered %d bytes for a request of %d", len(exported), beyondTheNarrowCeiling)
	}
	if _, err := wideSchedule.Export("URmessage/v1/storage", nil, 255*wide.HashSize()+1); !errors.Is(err, ErrExportLength) {
		t.Errorf("a provider whose KDF.Nh is %d accepted %d bytes, one past its own 255*KDF.Nh, answering %v; the ceiling moved with the provider in one direction and disappeared in the other",
			wide.HashSize(), 255*wide.HashSize()+1, err)
	}
}

// TestKeyScheduleExportLeavesItsContextAloneAndAnswersStorageOfItsOwn covers the three
// aliasing claims the package wide gate next door cannot make about this one, because that
// gate's class is package level FUNCTIONS handed a caller's bytes and Export is a method.
//
// Each is a different wrong implementation. A context written through is a caller's buffer
// changed by a read only operation. Two calls answering out of one array is an earlier export
// overwritten by a later one, which a caller holding both would discover as a key that changed
// under it. An answer that aliased one of the epoch's own secrets would be handed to a caller
// that then owns storage the epoch erases when it ages out of PastEpochWindow.
func TestKeyScheduleExportLeavesItsContextAloneAndAnswersStorageOfItsOwn(t *testing.T) {
	epochs := ksVectorEpochs(t)
	epoch := epochs[0]
	schedule := epoch.schedule(t)
	length := epoch.crypto.HashSize()
	context := bytes.Repeat([]byte{0x3d}, 40)
	handed := bytes.Clone(context)

	first, err := schedule.Export("URmessage/v1/storage", context, length)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if !bytes.Equal(context, handed) {
		t.Errorf("Export changed the storage behind the context it was handed: %x, was %x", context, handed)
	}
	second, err := schedule.Export("URmessage/v1/storage", context, length)
	if err != nil {
		t.Fatalf("Export a second time: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("two calls with one label, context and length answered %x and %x", first, second)
	}
	if len(first) == 0 || len(second) == 0 {
		t.Fatal("Export answered nothing, so the aliasing checks below observe nothing")
	}
	if &first[0] == &second[0] {
		t.Error("two calls to Export answered out of one array, so a caller holding an earlier export has it overwritten by a later one")
	}
	if &context[0] == &first[0] {
		t.Error("Export answered over the context it was handed")
	}
	for name, secret := range epochSecretsByField(t, schedule.Secrets()) {
		if len(secret) != 0 && &secret[0] == &first[0] {
			t.Errorf("Export answered over EpochSecrets.%s, which the epoch erases when it ages out of PastEpochWindow", name)
		}
	}
}

// TestKeyScheduleExportNeverAnswersASecretTheEpochKeeps is guardrail G6 read at the one
// exported surface a caller controls the arguments of.
//
// The sweep in bytesTheScheduleHandsOut drives Export through rows this file chose and holds
// its answers to G6 alongside the accessors'. This is the complement: a wider set of labels and
// contexts, compared against the whole of what the epoch holds rather than against epoch_secret
// alone. Both are needed — a body that answered self.epochSecret is caught by either, and one
// that answered confirmation_key under some particular label is caught only here.
//
// The length is KDF.Nh so the comparisons can bite at all: every secret compared against is
// KDF.Nh bytes, and an export of any other length could not equal one however wrong the body
// was.
func TestKeyScheduleExportNeverAnswersASecretTheEpochKeeps(t *testing.T) {
	compared := 0
	for _, epoch := range ksVectorEpochs(t) {
		schedule := epoch.schedule(t)
		length := epoch.crypto.HashSize()
		forbidden := map[string][]byte{
			"epoch_secret":   epochSecretOfTheEpoch(t, epoch),
			"joiner_secret":  schedule.JoinerSecret(),
			"welcome_secret": schedule.WelcomeSecret(),
		}
		for name, secret := range epochSecretsByField(t, schedule.Secrets()) {
			forbidden["EpochSecrets."+name] = secret
		}
		if len(forbidden) < 12 {
			t.Fatalf("%s: the epoch reads as %d secrets, and it keeps the nine derived ones plus three more",
				epoch.at, len(forbidden))
		}
		for _, label := range []string{"", "exported", "exporter", "epoch", "URmessage/v1/storage"} {
			for _, context := range [][]byte{nil, {}, exportSweepContext} {
				exported, err := schedule.Export(label, context, length)
				if err != nil {
					t.Fatalf("%s: Export(%q, %x, %d): %v", epoch.at, label, context, length, err)
				}
				for name, secret := range forbidden {
					if len(secret) == 0 {
						continue
					}
					compared++
					if bytes.Equal(exported, secret) {
						t.Errorf("%s: Export(%q, %x, %d) answered %s itself; the exporter hands out material derived FROM the epoch and never the epoch's own secrets",
							epoch.at, label, context, length, name)
					}
				}
			}
		}
	}
	if compared == 0 {
		t.Fatal("no export was compared against any secret, so this gate held nothing")
	}
}

// ---------------------------------------------------------------------------
// Task 9: the external key pair.
//
// This is the only place in the key schedule that reaches into HPKE, and deliberately the only
// one: everything else in key_schedule.go is arithmetic over byte slices, which is what keeps
// it auditable against RFC 9420 section 8 line by line.
//
// v1 refuses external commits, so this key pair is never advertised in a GroupInfo and never
// used to accept an ExternalInit. It is derived anyway because key-schedule.json checks
// external_pub, and because a DeriveKeyPair that disagrees here disagrees everywhere else it is
// used. p7's TestExternalPubIsNotAdvertised is the counterpart assertion, on the other side of
// the boundary.
// ---------------------------------------------------------------------------

// TestKeyScheduleExternalKeyPairMatchesTheMlswgKeySchedule pins external_pub against the
// answer mlswg published for every epoch of the corpus.
//
// external_secret is 32 pseudorandom bytes and so is every other secret of the epoch, so
// DeriveKeyPair over the wrong one answers a perfectly well formed public key of the right
// length. Nothing about the value says which secret it came from, which is why the published
// answer is what holds this.
func TestKeyScheduleExternalKeyPairMatchesTheMlswgKeySchedule(t *testing.T) {
	compared := 0
	for _, epoch := range ksVectorEpochs(t) {
		_, pub, err := epoch.schedule(t).ExternalKeyPair()
		if err != nil {
			t.Fatalf("%s: ExternalKeyPair: %v", epoch.at, err)
		}
		assertLabelKat(t, "external_pub"+epoch.at, pub, epoch.published.ExternalPub)
		compared++
	}
	if compared != keyScheduleKatEpochs {
		t.Fatalf("compared %d published external_pub answers, want %d", compared, keyScheduleKatEpochs)
	}
}

// TestKeyScheduleExternalKeyPairIsDerivedFromTheExternalSecretAndNothingElse states the same
// claim independently of the corpus, and states it as an exclusion rather than as an equality.
//
// The candidate set is read off the type by reflection — the nine derived secrets, plus the
// three other byte slices the epoch keeps — so a tenth secret joins by existing rather than by
// somebody remembering to add it here. Exactly one candidate may reproduce the answered public
// key, and it must be external_secret. That is what a KAT alone cannot say: a KAT compares one
// value and is silent about which of twelve equally well formed inputs produced it, so on the
// day the corpus is re-vendored or a suite is added, this is the statement that still holds.
func TestKeyScheduleExternalKeyPairIsDerivedFromTheExternalSecretAndNothingElse(t *testing.T) {
	for _, epoch := range ksVectorEpochs(t) {
		schedule := epoch.schedule(t)
		_, pub, err := schedule.ExternalKeyPair()
		if err != nil {
			t.Fatalf("%s: ExternalKeyPair: %v", epoch.at, err)
		}
		candidates := map[string][]byte{
			"epoch_secret":        epochSecretOfTheEpoch(t, epoch),
			"joiner_secret":       schedule.JoinerSecret(),
			"welcome_secret":      schedule.WelcomeSecret(),
			"group_context_bytes": schedule.GroupContextBytes(),
		}
		for name, secret := range epochSecretsByField(t, schedule.Secrets()) {
			candidates["EpochSecrets."+name] = secret
		}
		if len(candidates) < 13 {
			t.Fatalf("%s: the epoch reads as %d candidate inputs, and it keeps the nine derived secrets plus four more",
				epoch.at, len(candidates))
		}
		matched := []string{}
		for name, ikm := range candidates {
			if len(ikm) == 0 {
				continue
			}
			_, candidatePub, deriveErr := epoch.crypto.DeriveKeyPair(ikm)
			if deriveErr != nil {
				t.Fatalf("%s: DeriveKeyPair over %s: %v", epoch.at, name, deriveErr)
			}
			if bytes.Equal(candidatePub, pub) {
				matched = append(matched, name)
			}
		}
		if want := []string{"EpochSecrets.External"}; !slices.Equal(slices.Sorted(slices.Values(matched)), want) {
			t.Errorf("%s: external_pub is DeriveKeyPair of %v, and RFC 9420 section 8 derives it from external_secret and nothing else",
				epoch.at, matched)
		}
	}
}

// TestKeyScheduleExternalKeyPairIsDeterministicAndItsTwoHalvesAgree covers the half of this
// method the corpus is silent about.
//
// key-schedule.json publishes external_pub and no private key, so a body that answered the
// right public key beside an unrelated private one passes every comparison above. What says the
// two halves are halves of one pair is a seal to the public key opened with the private one,
// and that is not a round trip of this function — it goes out through HPKE and comes back.
//
// Determinism is the other claim and it is not implied by the first: a body that sampled a
// fresh pair per call would still seal and open, and would answer a different external_pub
// every time it was asked. Nothing in this derivation may read entropy.
func TestKeyScheduleExternalKeyPairIsDeterministicAndItsTwoHalvesAgree(t *testing.T) {
	info := []byte("the info every external key pair row carries")
	aad := []byte("the aad every external key pair row carries")
	plaintext := []byte("sealed to external_pub, opened with the half nobody published")
	for _, epoch := range ksVectorEpochs(t) {
		schedule := epoch.schedule(t)
		firstPriv, firstPub, err := schedule.ExternalKeyPair()
		if err != nil {
			t.Fatalf("%s: ExternalKeyPair: %v", epoch.at, err)
		}
		secondPriv, secondPub, err := schedule.ExternalKeyPair()
		if err != nil {
			t.Fatalf("%s: ExternalKeyPair a second time: %v", epoch.at, err)
		}
		if !bytes.Equal(firstPub, secondPub) || !bytes.Equal(firstPriv, secondPriv) {
			t.Errorf("%s: two calls answered different key pairs, so something in this derivation reads entropy and every member of the epoch would hold a different external_pub",
				epoch.at)
			continue
		}
		if len(firstPriv) == 0 || len(firstPub) == 0 {
			t.Fatalf("%s: ExternalKeyPair answered a %d byte private key and a %d byte public key",
				epoch.at, len(firstPriv), len(firstPub))
		}
		// the private half is the private half OF THAT public key
		kemOutput, ciphertext, err := epoch.crypto.HpkeSeal(firstPub, info, aad, plaintext)
		if err != nil {
			t.Fatalf("%s: seal to external_pub: %v", epoch.at, err)
		}
		opened, err := epoch.crypto.HpkeOpen(secondPriv, kemOutput, info, aad, ciphertext)
		if err != nil {
			t.Errorf("%s: the private key this method answered does not open a message sealed to the public key it answered beside it: %v",
				epoch.at, err)
			continue
		}
		if !bytes.Equal(opened, plaintext) {
			t.Errorf("%s: opening with the answered private key produced %x, want %x", epoch.at, opened, plaintext)
		}
		// and DeriveKeyPair was applied rather than external_secret handed back under a
		// different name
		external := schedule.Secrets().External
		if bytes.Equal(firstPriv, external) {
			t.Errorf("%s: the external private key IS external_secret, so DeriveKeyPair is not being applied to it", epoch.at)
		}
		if len(external) != 0 && &external[0] == &firstPriv[0] {
			t.Errorf("%s: the external private key is a view over external_secret, which the epoch erases when it ages out of PastEpochWindow",
				epoch.at)
		}
	}
}

// deriveSecretCapturingProvider keeps every secret DeriveSecret answers, so a test can read
// that storage back after the call that computed it has returned.
//
// The slice is returned unchanged rather than copied, and that is the whole mechanism:
// zeroizeSecret writes through the backing array its argument points at, so a wrapper handing
// the caller a clone would be handing it a different array the erase never reaches, and the
// property would read as absent however the production code behaved. This is
// extractCapturingProvider one derivation over.
type deriveSecretCapturingProvider struct {
	CryptoProvider
	derived [][]byte
}

func (self *deriveSecretCapturingProvider) DeriveSecret(secret []byte, label string) []byte {
	answer := self.CryptoProvider.DeriveSecret(secret, label)
	self.derived = append(self.derived, answer)
	return answer
}

// TestKeyScheduleExportErasesThePerLabelSecret observes the sentence key_schedule.go writes
// inside Export: "the per label secret is one HKDF-Expand away from every export under that
// label and nothing downstream needs it, so the storage it was computed into is erased rather
// than left for the collector."
//
// Nothing observed it. Measured, not supposed: deleting the zeroizeSecret(derived) line left
// the whole of mls and message green, while the package tests every other erase it performs --
// TestNewKeyScheduleErasesTheMemberSecret, TestNewKeyScheduleErasesTheJoinerSecretItDerived
// and TestDeriveJoinerSecretErasesThePseudorandomKey. A claim the code makes in prose that no
// test observes is a claim the next edit deletes for free.
//
// What that secret is worth to whoever finds it: DeriveSecret(exporter_secret, Label) is the
// parent of every export under that label, at every length and over every context, so a reader
// of it reproduces the seed recovery key URmessage wraps to this epoch without ever seeing the
// epoch.
//
// The nine the constructor derived are dropped before the call, because they are the epoch's
// own and stay; what this reads is the one derivation Export itself makes. The control is the
// one TestDeriveJoinerSecretErasesThePseudorandomKey carries: an all zero reading only means
// something if the secret was not already zero, and an export that came back zero would
// satisfy the loop for the wrong reason.
func TestKeyScheduleExportErasesThePerLabelSecret(t *testing.T) {
	const label = "URmessage/v1/storage"
	for _, epoch := range ksVectorEpochs(t) {
		crypto := &deriveSecretCapturingProvider{CryptoProvider: epoch.crypto}
		schedule, err := NewKeySchedule(
			crypto, epoch.initPrev, epoch.commitSecret, epoch.pskSecret, epoch.groupContext)
		if err != nil {
			t.Fatalf("%s: NewKeySchedule: %v", epoch.at, err)
		}
		// the control: the secret this call will erase is not zero to begin with
		fresh := epoch.crypto.DeriveSecret(schedule.Secrets().Exporter, label)
		if !slices.ContainsFunc(fresh, func(b byte) bool { return b != 0 }) {
			t.Fatalf("%s: DeriveSecret over exporter_secret under %q is already %d zero bytes, so an all zero reading below would say nothing",
				epoch.at, label, len(fresh))
		}

		crypto.derived = nil
		exported, err := schedule.Export(label, exportSweepContext, epoch.crypto.HashSize())
		if err != nil {
			t.Fatalf("%s: Export: %v", epoch.at, err)
		}
		if len(crypto.derived) != 1 {
			t.Fatalf("%s: Export made %d DeriveSecret calls, want 1; this gate reads the secret that one call answered",
				epoch.at, len(crypto.derived))
		}
		derived := crypto.derived[0]
		if len(derived) != epoch.crypto.HashSize() {
			t.Fatalf("%s: the per label secret is %d bytes, want %d",
				epoch.at, len(derived), epoch.crypto.HashSize())
		}
		for i, b := range derived {
			if b != 0 {
				t.Errorf("%s: byte %d of the per label secret is %#02x after Export returned, want 0; it is one HKDF-Expand away from every export under %q and nothing downstream needs it",
					epoch.at, i, b, label)
				break
			}
		}
		// and the erase reached that secret rather than the answer: an export that came back
		// zero would satisfy the loop above for the wrong reason
		if !slices.ContainsFunc(exported, func(b byte) bool { return b != 0 }) {
			t.Errorf("%s: Export answered %d zero bytes, so the erase reached the value that was returned",
				epoch.at, len(exported))
		}
		// and exporter_secret itself is the epoch's and stays, which is what Export's own
		// comment says one line further on
		if !slices.ContainsFunc(schedule.Secrets().Exporter, func(b byte) bool { return b != 0 }) {
			t.Errorf("%s: exporter_secret is all zero after one Export, so the erase reached the epoch's own secret rather than the per label one",
				epoch.at)
		}
	}
}

// epochSecretsStorageFieldIn is the field of a holder that carries the nine derived secrets,
// found rather than written down: the field whose declared type is the compiled name of
// EpochSecrets.
//
// Both halves are derived. The type name comes off reflection, so renaming the type moves this
// with it, and the field name comes off the holder's own declaration, so renaming the field
// does too. A gate anchored on the spelling "secrets" would go on reading nothing after either
// rename and would report the clean run a working one reports.
func epochSecretsStorageFieldIn(t *testing.T, structs map[string]*ast.StructType, holder string) string {
	t.Helper()
	wanted := reflect.TypeOf(EpochSecrets{}).Name()
	declared, isDeclared := structs[holder]
	if !isDeclared {
		t.Fatalf("this package's source declares no struct named %s", holder)
	}
	for _, field := range declared.Fields.List {
		if !slices.Contains(identifiersNamedIn(field.Type), wanted) {
			continue
		}
		for _, name := range field.Names {
			return name.Name
		}
	}
	t.Fatalf("no field of %s is declared as a %s, so the class below would be empty and this gate would demand nothing",
		holder, wanted)
	return ""
}

// theMethodsDerivingFromOneOfTheNine is every exported method of the parsed files whose body
// reaches PAST the storage that carries the epoch's nine secrets and into one of them.
//
// One selector down is the whole distinction. Secrets() answers the struct and derives nothing
// from it — a caller that erased what it was handed did that to itself — while Export and
// ExternalKeyPair read a named secret out of it and expand something over the value. Only the
// second kind can answer a derivation of a secret that is no longer there, so only the second
// kind is what the gate below holds.
func theMethodsDerivingFromOneOfTheNine(declared []sourceDeclaration, storage string, byteSlices []string) []string {
	deriving := []string{}
	for _, one := range declared {
		if !one.exported || one.body == nil {
			continue
		}
		// an eraser reads every one of the nine BY NAME and derives from none of them: it is
		// handed nothing and answers nothing, so there is no value for an erased epoch to
		// refuse and nothing for the live control below to observe. Zeroize is that shape.
		// Told apart by the shape rather than by the name, through the same predicate G6's
		// method gate uses, so the two cannot come to disagree about what an eraser is.
		if !hasSomewhereToPutASecret(one, byteSlices) {
			continue
		}
		found := false
		ast.Inspect(one.body, func(node ast.Node) bool {
			outer, isSelector := node.(*ast.SelectorExpr)
			if !isSelector {
				return true
			}
			if inner, isNested := outer.X.(*ast.SelectorExpr); isNested && inner.Sel.Name == storage {
				found = true
			}
			return !found
		})
		if found {
			deriving = append(deriving, one.name)
		}
	}
	slices.Sort(deriving)
	return deriving
}

// erasedEpochControl declares one of each shape that derivation has to tell apart: a method
// that reads a named secret out of the storage, one that answers the storage whole, one that
// reads something else entirely, an unexported one that reads a named secret, and an ERASER,
// which reads every one of them by name and is handed nothing and answers nothing.
//
// The eraser is the shape this class has to DROP rather than demand a refusal from. Zeroize is
// it. Without this row the filter that drops it could be deleted with the control still
// matching exactly, and the gate would then ask a method that answers nothing at all to satisfy
// a live control it cannot satisfy.
const erasedEpochControl = "package control\n" +
	"\n" +
	"type EpochSecrets struct {\n" +
	"\tExporter []byte\n" +
	"}\n" +
	"\n" +
	"type Holder struct {\n" +
	"\tsecrets EpochSecrets\n" +
	"\tother   []byte\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) DerivesFromOneOfThem() []byte {\n" +
	"\treturn expand(self.secrets.Exporter)\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) AnswersTheStorageWhole() *EpochSecrets {\n" +
	"\treturn &self.secrets\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) ReadsSomethingElse() []byte {\n" +
	"\treturn self.other\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) derivesButIsUnexported() []byte {\n" +
	"\treturn expand(self.secrets.Exporter)\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) ErasesOneOfThem() {\n" +
	"\tzero(self.secrets.Exporter)\n" +
	"}\n" +
	"\n" +
	"func zero(secret []byte) {\n" +
	"\tfor i := range secret {\n" +
	"\t\tsecret[i] = 0\n" +
	"\t}\n" +
	"}\n" +
	"\n" +
	"func expand(secret []byte) []byte {\n" +
	"\treturn secret\n" +
	"}\n"

// scheduleAnswer is what one row of one exported method handed back: the byte slices a caller
// can read, and the bits a verifier answered with.
//
// The two travel together because the gate below asks one question of both kinds of method.
// "Did this call hand something back that a caller can act on" is BYTES for a derivation and an
// ACCEPTANCE for a verifier, and a reading that saw only the bytes would report a verifier
// which accepts over an erased epoch as a call that answered nothing at all — which is exactly
// the reading a correctly refusing verifier produces, and so exactly the clean run.
type scheduleAnswer struct {
	read     [][]byte
	accepted []bool
}

// handedSomethingBack is whether this row produced anything a caller could act on.
func (self scheduleAnswer) handedSomethingBack() bool {
	return slices.ContainsFunc(self.read, func(one []byte) bool { return len(one) != 0 }) ||
		slices.Contains(self.accepted, true)
}

// scheduleMethodResults calls one exported method of *KeySchedule with every row the sweeps
// drive it by — or once with no arguments if it takes none — and splits each answer into the
// bytes a caller can read, the bits a verifier answered, and the error beside them.
//
// The rows are keyScheduleMethodArgumentRows, the same ones guardrail 6 is driven through, so
// a method that gains an argument is driven here by writing that row once rather than twice.
func scheduleMethodResults(t *testing.T, at string, schedule *KeySchedule, name string) []scheduleAnswer {
	t.Helper()
	method, found := reflect.TypeOf(schedule).MethodByName(name)
	if !found {
		t.Fatalf("%s: *KeySchedule declares no method %s", at, name)
	}
	rows := [][]reflect.Value{nil}
	if method.Type.NumIn() != 1 {
		build, driven := keyScheduleMethodArgumentRows[name]
		if !driven {
			t.Fatalf("%s: %s takes arguments and keyScheduleMethodArgumentRows has no rows for it, so this gate cannot call it",
				at, name)
		}
		rows = build(schedule)
	}
	errorInterface := reflect.TypeOf((*error)(nil)).Elem()
	answers := []scheduleAnswer{}
	for _, row := range rows {
		answer := scheduleAnswer{read: [][]byte{}, accepted: []bool{}}
		refused := false
		for _, result := range method.Func.Call(append([]reflect.Value{reflect.ValueOf(schedule)}, row...)) {
			if result.Type() == errorInterface {
				if !result.IsNil() {
					refused = true
					if !errors.Is(result.Interface().(error), ErrEpochErased) {
						t.Errorf("%s: %s answered %v, and the only refusal this gate expects is ErrEpochErased",
							at, name, result.Interface())
					}
				}
				continue
			}
			if result.Kind() == reflect.Bool {
				answer.accepted = append(answer.accepted, result.Bool())
				continue
			}
			answer.read = append(answer.read, exposedBytes(exposedByteSlices(t, "(*KeySchedule)."+name, result))...)
		}
		if refused {
			// a refusal that came back with bytes beside it is the leak this gate is about,
			// wearing an error
			for _, secret := range answer.read {
				if len(secret) != 0 {
					t.Errorf("%s: %s refused and %d bytes came back beside the refusal", at, name, len(secret))
				}
			}
			answers = append(answers, scheduleAnswer{})
			continue
		}
		answers = append(answers, answer)
	}
	return answers
}

// TestEveryMethodDerivingFromTheEpochsSecretsRefusesAnErasedEpoch is the erased epoch read as
// behaviour, over the class of methods that can hit it rather than over the one that was found
// hitting it.
//
// An epoch leaving PastEpochWindow has its nine secrets zeroized in place — key_schedule.go
// says so, and Secrets() hands out the pointer that makes it reachable today. A derivation over
// KDF.Nh zero bytes is not a weakened secret, it is a PUBLIC one: measured, not supposed, with
// the nine erased in place Export("URmessage/v1/storage", nil, 32) returned err == nil and a
// value byte identical to MLS-Exporter over 32 zero bytes. URmessage wraps seed recovery to
// each epoch's export, so a recovery blob taken from an aged out epoch would be wrapped to a
// key the attacker also holds and nothing would report an error.
//
// The class is every exported method whose body reads one of the nine by name, derived off the
// package's own source. Enumerating it would be the mistake this project has paid for
// repeatedly: the defect was reported against Export, and ExternalKeyPair — which derives an
// HPKE key pair from external_secret and would answer a key pair whose private half every party
// can recompute — is the same defect one method over and was in no report.
//
// Both directions are asserted. A live epoch must answer, or a method that refused everything
// would satisfy the erased half for the wrong reason; and the nine really are zero after the
// erase, or the refusal would be about some other condition.
//
// A VERIFIER is in the same class and for the same reason, which is why what a row answered is
// read as bytes and as acceptances together. ConfirmationTag and MembershipTag hand back a mac
// under a key that has become publicly computable; VerifyConfirmationTag and VerifyMembershipTag
// hand back TRUE for a tag anybody could have forged under that same key. The second is the
// worse half — a caller reading the bool has been told the message is authentic — and it is
// invisible to a gate that only counts bytes, because a bool is not one.
func TestEveryMethodDerivingFromTheEpochsSecretsRefusesAnErasedEpoch(t *testing.T) {
	// the control first: the matcher tells a read of a named secret from an answer of the
	// storage whole, and exported from not
	control := mustParseText(t, "the erased epoch control", erasedEpochControl)
	controlStructs := map[string]*ast.StructType{}
	structTypesIn(control, controlStructs)
	if storage := epochSecretsStorageFieldIn(t, controlStructs, "Holder"); storage != "secrets" {
		t.Fatalf("the storage field scan read %q out of the control, want \"secrets\"", storage)
	}
	controlDeriving := theMethodsDerivingFromOneOfTheNine(declaredIn(control), "secrets", []string{"[]byte"})
	if want := []string{"DerivesFromOneOfThem"}; !slices.Equal(controlDeriving, want) {
		t.Fatalf("the matcher read %v out of the control as deriving from one of the nine, want %v; it is not telling a read of a named secret from an answer of the storage whole",
			controlDeriving, want)
	}

	// then this package's own source
	structs := map[string]*ast.StructType{}
	files := []parsedSource{}
	for _, path := range packageLevelFunctions(t).files {
		parsed := mustParseSource(t, path)
		files = append(files, parsed)
		structTypesIn(parsed, structs)
	}
	holders := theTypesDeclaringTheEpochSecret(structs)
	if len(holders) != 1 {
		t.Fatalf("this package's source has %v DECLARING the epoch secret storage and this gate reads one declarer; a type that merely POINTS at one is the transitive reading and belongs to guardrail 6's surface gates rather than to this one",
			holders)
	}
	storage := epochSecretsStorageFieldIn(t, structs, holders[0])
	deriving := theMethodsDerivingFromOneOfTheNine(declaredAcross(files), storage,
		slices.Concat([]string{"[]byte"}, packageByteSliceTypeNames(t)))
	if len(deriving) == 0 {
		t.Fatalf("no exported method of this package reads %s.<one of the nine>, and this package declares Export and ExternalKeyPair, so the scan is reading nothing",
			storage)
	}
	for _, name := range deriving {
		if _, found := reflect.TypeOf(&KeySchedule{}).MethodByName(name); !found {
			t.Fatalf("%s derives from one of the nine and *KeySchedule does not declare it, so this gate cannot call it", name)
		}
	}
	t.Logf("%d exported methods derive from one of the nine: %v", len(deriving), deriving)

	for _, epoch := range ksVectorEpochs(t) {
		schedule := epoch.schedule(t)
		// the live control: every one of them answers bytes before the erase, so a refusal
		// afterwards is the erase and not a method that refuses everything
		for _, name := range deriving {
			for index, answer := range scheduleMethodResults(t, epoch.at, schedule, name) {
				if !answer.handedSomethingBack() {
					t.Fatalf("%s: %s row %d answered nothing over a live epoch — no bytes and no acceptance — so a refusal after the erase would say nothing",
						epoch.at, name, index)
				}
			}
		}

		// the erase the epoch performs on itself when it leaves PastEpochWindow, over the nine
		// read by reflection rather than named here
		secrets := reflect.ValueOf(schedule.Secrets()).Elem()
		for i := range secrets.NumField() {
			zeroizeSecret(secrets.Field(i).Bytes())
		}
		for name, secret := range epochSecretsByField(t, schedule.Secrets()) {
			if slices.ContainsFunc(secret, func(b byte) bool { return b != 0 }) {
				t.Fatalf("%s: EpochSecrets.%s is not zero after the erase, so what the calls below answer is not an erased epoch",
					epoch.at, name)
			}
		}

		for _, name := range deriving {
			for index, answer := range scheduleMethodResults(t, epoch.at, schedule, name) {
				for _, secret := range answer.read {
					if len(secret) != 0 {
						t.Errorf("%s: %s row %d answered %d bytes over an epoch whose secrets are erased; a derivation over KDF.Nh zero bytes is publicly computable, so this is a secret handed out with err == nil",
							epoch.at, name, index, len(secret))
					}
				}
				for _, accepted := range answer.accepted {
					if accepted {
						t.Errorf("%s: %s row %d ACCEPTED over an epoch whose secrets are erased; the key it verified under is KDF.Nh zero bytes, which every party can compute, so what it accepted is a tag anybody could have forged",
							epoch.at, name, index)
					}
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Task 10: the confirmation tag and the membership tag
// ---------------------------------------------------------------------------
//
// Two of the four functions this task adds return a bool, and this project has already
// shipped a total authentication bypass behind that shape twice, both times with everything
// green: a write_auth whose tag error was discarded returned the ZERO tag for a wrong length
// key — and an all zero write_auth is the normal state of every record on the read path — and
// a truncated comparison reached through a comparator no ban list held.
//
// So the tests below are deliberately not "the tag is KDF.Nh bytes" and not "verify accepts
// what compute produced". Both of those pass against a verifier that returns true
// unconditionally, which is a one line edit and the whole of the bypass. What is asserted
// instead is the class of things that must be REFUSED, derived over the length rather than
// sampled at a position somebody chose, and the identity of the KEY, which no round trip can
// see because the two keys this task uses are adjacent DeriveSecret calls of the same length.
//
// The key identity is held to known answers nobody here computed:
//
//   - the confirmation_tag mlswg published in transcript-hashes.json, which that corpus
//     defines as MAC(confirmation_key, confirmed_transcript_hash_after);
//   - the membership_tag mlswg published inside the public messages of
//     message-protection.json, taken over the AuthenticatedContentTBM rebuilt from that
//     entry's own bytes.
//
// Both corpora are authenticated against upstream's git object store before a byte of either
// is read, for the reason keyScheduleKatVectors gives: a known answer test that compares
// against a file an edit can change is a known answer test that can be made to agree with
// anything.
//
// Measured, not supposed, and this is why none of the sampling shapes survives here. The three
// tests p4 task 10 supplies were ported to the helpers this package actually declares — the
// plan's MustHex, ksTestCrypto, ksVectorEpoch0Schedule, ksVectorConfirmationKey and
// ksVectorMembershipKey do not exist, so as written they do not compile — and run against
// seven edits to the four functions under test. Four of the seven left all three GREEN:
//
//   - a verifier comparing only the FIRST BYTE of a 32 byte tag and ignoring the other 31,
//     which is a forgery found in 256 tries;
//   - the same over the first 16;
//   - the comparison moved off CryptoProvider.MacVerify onto bytes.Equal, which answers
//     identically and gives up the constant time property no behavioural test can see;
//   - the erased epoch refusals deleted, so a tag is taken and accepted under KDF.Nh zero bytes.
//
// The plan's own verifier test refuses a tag with bit zero of byte zero flipped, and that is the
// whole of what it samples, so a comparison that reads byte zero and stops satisfies it exactly.
// Its membership test carries no length case at all and survived a verifier that accepts every
// truncation. What the three did catch is a verifier returning true unconditionally, and the two
// keys swapped. That is the reason every refusal below is derived over the length of the thing
// it alters, and the reason the routing gate at the end of this file exists at all.

// The two further mlswg families this task reads, and how many comparisons each contributes
// once the entries for suites this package does not register are dropped: one confirmation
// tag per registered suite, and one membership tag per registered suite per public message.
//
// The counts are asserted rather than assumed, for the reason keyScheduleKatJoinerComparisons
// is: a filter that stopped matching — a suite renumbered, a json field renamed so every
// string decodes empty — turns a known answer test into a loop that runs zero times and
// reports PASS, which is the one outcome a known answer test must not be able to reach.
const (
	transcriptHashKatFile      = "transcript-hashes.json"
	messageProtectionKatFile   = "message-protection.json"
	confirmationTagComparisons = 2
	membershipTagComparisons   = 4
)

// transcriptHashKatEntry is the part of one transcript-hashes.json entry this file reads.
//
// authenticated_content is the serialized AuthenticatedContent of a Commit, and the last field
// of its FramedContentAuthData is the confirmation_tag — which is where the published answer
// is read from, since that corpus carries no separate confirmation_tag field.
type transcriptHashKatEntry struct {
	CipherSuite                  uint16 `json:"cipher_suite"`
	ConfirmationKey              string `json:"confirmation_key"`
	AuthenticatedContent         string `json:"authenticated_content"`
	ConfirmedTranscriptHashAfter string `json:"confirmed_transcript_hash_after"`
}

// messageProtectionKatEntry is the part of one message-protection.json entry this file reads:
// the membership_key of the epoch, the four GroupContext fields that epoch was framed under,
// and the two public messages with the bare proposal and commit bodies that sit inside them.
type messageProtectionKatEntry struct {
	CipherSuite             uint16 `json:"cipher_suite"`
	GroupId                 string `json:"group_id"`
	Epoch                   uint64 `json:"epoch"`
	TreeHash                string `json:"tree_hash"`
	ConfirmedTranscriptHash string `json:"confirmed_transcript_hash"`
	MembershipKey           string `json:"membership_key"`
	SignaturePub            string `json:"signature_pub"`
	Proposal                string `json:"proposal"`
	ProposalPub             string `json:"proposal_pub"`
	Commit                  string `json:"commit"`
	CommitPub               string `json:"commit_pub"`
}

// mustLoadAuthenticatedCorpus decodes one vendored mlswg family, having first checked that the
// bytes on disk are the blob mlswg published at the commit interop/PINS.md pins.
//
// This is keyScheduleKatVectors' provenance check, written once for the families this task
// adds. The check is made here rather than left to vectors_upstream_test.go for the reason
// that file gives about VECTORS.sha256: a manifest computed over the local bytes verifies a
// rewritten corpus the moment the manifest is rewritten with it, and that test failing does
// not stop this one running. The digest compared against is the one read out of upstream's git
// object store.
func mustLoadAuthenticatedCorpus(t *testing.T, name string, into any) {
	t.Helper()
	want, anchored := mlswgVectorUpstreamSha256[name]
	if !anchored {
		t.Fatalf("%s carries no upstream digest, so the answers below would be compared against an unauthenticated file",
			name)
	}
	raw, err := os.ReadFile(filepath.Join(mlswgVectorDirectory, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	digest := sha256.Sum256(normalisedLineEndings(raw))
	if got := hex.EncodeToString(digest[:]); got != want {
		t.Fatalf("%s hashes to %s with its line endings normalised; %s/%s at %s published %s. These are not the answers mlswg published, so nothing below is a known answer test",
			name, got, mlswgVectorUpstreamRepository, mlswgVectorUpstreamDirectory,
			mlswgVectorUpstreamCommit, want)
	}
	loadLabelKat(t, name, into)
}

// publishedVectorPrefix is vectorLengthPrefix with this file's fatal error policy: the
// length prefix a <V> vector of n octets carries, spelled by the codec rather than written
// out here, and a corpus this package cannot read is a failure rather than a verdict.
//
// The derivation itself moved to vectors_runner_test.go when the transcript-hashes runner
// landed and became the third reader of these published structures. Two implementations of
// "how wide is the prefix" is how the two stop agreeing at the first SHA-512 suite, which is
// the exact failure the derivation exists to prevent.
func publishedVectorPrefix(t *testing.T, what string, n int) []byte {
	t.Helper()
	prefix, err := vectorLengthPrefix(n)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	return prefix
}

// TestThePublishedVectorPrefixWidensWithTheVector is the control on the helper above.
//
// The whole point of asking the codec is that the prefix is not always one octet, and the
// registered suites cannot show that: KDF.Nh is 32 for both and 32 <= 63, so a helper that
// answered a single octet holding the length agrees with the codec on every input this
// package has and disagrees at the first suite whose hash is SHA-512. Both widths are read
// here, and the boundary is read from both sides, so a helper that had gone back to writing
// one octet -- or that widened one value too early -- fails rather than waiting for the suite
// registry to grow.
//
// The expected bytes are RFC 9420 section 2.1.2's own spelling, written out rather than taken
// from the codec: a control built by the thing it controls holds for any encoding at all.
func TestThePublishedVectorPrefixWidensWithTheVector(t *testing.T) {
	for _, testCase := range []struct {
		n    int
		want []byte
	}{
		{n: 0, want: []byte{0x00}},
		{n: 32, want: []byte{0x20}},
		{n: 63, want: []byte{0x3f}},
		{n: 64, want: []byte{0x40, 0x40}},
		{n: 48, want: []byte{0x30}},
		{n: 16383, want: []byte{0x7f, 0xff}},
	} {
		if found := publishedVectorPrefix(t, "the vector prefix control", testCase.n); !bytes.Equal(found, testCase.want) {
			t.Errorf("a <V> vector of %d octets carries the prefix %x, and RFC 9420 section 2.1.2 writes %x",
				testCase.n, found, testCase.want)
		}
	}
}

// publishedTagAtTheTail reads the mac one of these serialized structures ends with.
//
// splitTrailingOpaqueTag with this file's fatal error policy. The split, and the assertion on
// the length prefix in front of the tail that makes it a tag rather than whatever bytes sit
// there, are in vectors_runner_test.go because family 5, family 7 and the transcript known
// answer tests all read the same shape.
func publishedTagAtTheTail(t *testing.T, what string, blob []byte, nh int) []byte {
	t.Helper()
	_, tag, err := splitTrailingOpaqueTag(blob, nh)
	if err != nil {
		t.Fatalf("%s: %v, so the tail this reads is not a tag", what, err)
	}
	return tag
}

// ksScheduleForSuite builds a real key schedule for one registered suite out of the published
// key schedule corpus.
//
// A real one, rather than a hand assembled struct, because everything the tag methods reach —
// the provider, the nine secrets, the storage Secrets() answers into — has to be the thing
// under test and not a fixture that resembles it.
func ksScheduleForSuite(t *testing.T, epochs []ksVectorEpoch, suite CipherSuite) *KeySchedule {
	t.Helper()
	for _, epoch := range epochs {
		if epoch.suite == suite {
			return epoch.schedule(t)
		}
	}
	t.Fatalf("the key schedule corpus carries no epoch for suite %#04x, so no schedule can be built for it",
		uint16(suite))
	return nil
}

// installTheCorpusKey writes one of the corpus's own keys into a schedule's storage, having
// first established that doing so changes the answer.
//
// The write goes through the pointer Secrets() answers, which key_schedule.go documents as
// being into the schedule's own storage. That is what lets a published (key, data, tag) triple
// be put through the real type: a key schedule derives its own confirmation_key from its own
// epoch and cannot be asked to derive somebody else's.
//
// The two refusals are the controls, and they matter more than they look. If the schedule's own
// secret already equalled the corpus key, or if the tag it produced before the write already
// equalled the published one, then the comparison that follows would hold whichever key the
// method under test actually read — including the other one of the two adjacent secrets this
// whole file exists to tell apart. A comparison that would have passed without the installation
// is not a comparison against the corpus at all.
func installTheCorpusKey(t *testing.T, at string, storage *[]byte, key []byte, before []byte, want []byte) {
	t.Helper()
	if bytes.Equal(*storage, key) {
		t.Fatalf("%s: the schedule's own secret already equals the corpus key, so installing it changes nothing and the comparison below holds for either one",
			at)
	}
	if bytes.Equal(before, want) {
		t.Fatalf("%s: the tag over this schedule's own secret already equals the published one, so the comparison below holds whichever key the method reads",
			at)
	}
	*storage = key
}

// TestConfirmationTagMatchesTheMlswgTranscriptHashes is the known answer test for
// ConfirmationTag, against a value this package did not compute.
//
// mlswg's transcript-hashes corpus defines the confirmation_tag carried by the
// authenticated_content of each entry as MAC(confirmation_key, confirmed_transcript_hash_after),
// and publishes the key, the hash and the AuthenticatedContent the tag sits in. All three are
// somebody else's numbers, which is the only thing that can separate a tag keyed by
// confirmation_key from one keyed by membership_key: the two are adjacent DeriveSecret calls
// over one parent, so they are the same length and indistinguishable from random, and an
// implementation that swapped them returns a perfectly well formed KDF.Nh tag that it would
// also accept back from itself.
//
// Three things stop this passing vacuously. The corpus is authenticated against upstream before
// it is read. The number of comparisons is counted and the suites the loop matched are compared
// against the registry, so a filter that stopped matching is loud rather than green. And the tag
// the schedule's OWN confirmation_key produces over the same hash is computed first and required
// to differ from the published one, so a comparison that would have held without the corpus key
// being installed is reported instead of passing.
func TestConfirmationTagMatchesTheMlswgTranscriptHashes(t *testing.T) {
	entries := []transcriptHashKatEntry{}
	mustLoadAuthenticatedCorpus(t, transcriptHashKatFile, &entries)
	if len(entries) == 0 {
		t.Fatalf("%s parsed to no entries, so every comparison below would run over nothing", transcriptHashKatFile)
	}
	epochs := ksVectorEpochs(t)
	compared := 0
	matched := []CipherSuite{}
	for _, entry := range entries {
		suite := CipherSuite(entry.CipherSuite)
		if !IsRegisteredSuite(suite) {
			continue
		}
		matched = append(matched, suite)
		at := fmt.Sprintf("%s suite %#04x", transcriptHashKatFile, uint16(suite))
		crypto := mustProvider(t, suite)
		nh := crypto.HashSize()
		confirmationKey := mustDecodeHex(t, at+" confirmation_key", entry.ConfirmationKey)
		transcriptHash := mustDecodeHex(t, at+" confirmed_transcript_hash_after", entry.ConfirmedTranscriptHashAfter)
		authenticatedContent := mustDecodeHex(t, at+" authenticated_content", entry.AuthenticatedContent)
		if len(confirmationKey) != nh {
			t.Fatalf("%s: the published confirmation_key is %d bytes and this suite's KDF.Nh is %d",
				at, len(confirmationKey), nh)
		}
		if len(transcriptHash) != nh {
			t.Fatalf("%s: the published confirmed_transcript_hash_after is %d bytes and this suite's KDF.Nh is %d",
				at, len(transcriptHash), nh)
		}
		want := publishedTagAtTheTail(t, at+" authenticated_content", authenticatedContent, nh)

		schedule := ksScheduleForSuite(t, epochs, suite)
		installTheCorpusKey(t, at, &schedule.Secrets().Confirmation, confirmationKey,
			schedule.ConfirmationTag(transcriptHash), want)
		got := schedule.ConfirmationTag(transcriptHash)
		if !bytes.Equal(got, want) {
			t.Errorf("%s: ConfirmationTag = %x, and mlswg published %x. The tag is MAC(confirmation_key, confirmed_transcript_hash) and nothing else; membership_key is the same length and produces an answer just as well formed",
				at, got, want)
		}
		if !schedule.VerifyConfirmationTag(transcriptHash, want) {
			t.Errorf("%s: VerifyConfirmationTag refused the tag mlswg published for this key and this transcript hash",
				at)
		}
		compared++
	}
	if compared != confirmationTagComparisons {
		t.Fatalf("%d confirmation tags were compared against %s, want %d; the loop matched %v",
			compared, transcriptHashKatFile, confirmationTagComparisons, matched)
	}
	if got := slices.Sorted(slices.Values(matched)); !slices.Equal(got, Suites()) {
		t.Fatalf("%s answered for %v and this package registers %v", transcriptHashKatFile, got, Suites())
	}
}

// authenticatedContentTbm rebuilds the AuthenticatedContentTBM bytes RFC 9420 section 6.1 takes
// the membership tag over, out of one message-protection.json public message.
//
// This plan owns no framing types — p6 does — so the structure is not decoded. It does not have
// to be. RFC 9420 writes
//
//	FramedContentTBS   = version || wire_format || FramedContent || GroupContext
//	AuthenticatedContentTBM = FramedContentTBS || FramedContentAuthData
//	PublicMessage      = FramedContent || FramedContentAuthData || membership_tag
//
// and the corpus hands over an MLSMessage whose first four octets are exactly version and
// wire_format, followed by that PublicMessage. So the only thing needed is where FramedContent
// ends, and the corpus publishes that too: the bare `proposal` and `commit` fields are the
// serialized bodies that sit at the END of the FramedContent of the matching public message.
// Locating those bytes gives the boundary, the GroupContext is spliced in there, and the auth
// data is what is left over once the trailing membership_tag is removed.
//
// The splice is required to be unambiguous rather than assumed to be: the body must occur
// exactly once in the message, or the boundary this reads is a guess. And nothing here is
// circular — the reconstruction is a function of published bytes alone, and a reconstruction
// that were wrong would make the comparison FAIL rather than pass, since only the right
// preimage under the right key reproduces a mac somebody else published.
//
// The GroupContext is encoded by this package's own codec, which the group context task already
// holds to this same corpus family; if it disagreed, this test would fail rather than pass.
func authenticatedContentTbm(
	t *testing.T,
	at string,
	mlsMessage []byte,
	framedBody []byte,
	groupContext []byte,
	nh int,
) []byte {
	t.Helper()
	if !bytes.HasPrefix(mlsMessage, mlsMessagePublicMessageHeader) {
		t.Fatalf("%s: the message opens with %x and an MLSMessage carrying a PublicMessage opens with %x, so the first four octets are not the version and wire format this splices",
			at, mlsMessage[:min(len(mlsMessage), len(mlsMessagePublicMessageHeader))], mlsMessagePublicMessageHeader)
	}
	found := bytes.Index(mlsMessage, framedBody)
	if found < 0 {
		t.Fatalf("%s: the published body of %d bytes is not inside the public message, so where FramedContent ends cannot be read off it",
			at, len(framedBody))
	}
	if bytes.Index(mlsMessage[found+1:], framedBody) >= 0 {
		t.Fatalf("%s: the published body occurs more than once in the public message, so the boundary this reads is a guess", at)
	}
	endOfFramedContent := found + len(framedBody)
	membershipTagWithPrefix := nh + 1
	if endOfFramedContent+membershipTagWithPrefix > len(mlsMessage) {
		t.Fatalf("%s: FramedContent ends %d bytes into a %d byte message, leaving no room for the auth data and the membership tag",
			at, endOfFramedContent, len(mlsMessage))
	}
	authData := mlsMessage[endOfFramedContent : len(mlsMessage)-membershipTagWithPrefix]
	if len(authData) == 0 {
		t.Fatalf("%s: the FramedContentAuthData between the content and the membership tag is empty, and it carries a signature at least",
			at)
	}
	return joinBytes(mlsMessage[:endOfFramedContent], groupContext, authData)
}

// TestMembershipTagMatchesTheMlswgMessageProtection is the known answer test for MembershipTag,
// against a value this package did not compute.
//
// mlswg's message-protection corpus publishes an epoch's membership_key and two PublicMessages
// framed under it, and the membership_tag each of those carries is MAC(membership_key,
// AuthenticatedContentTBM). Rebuilding that preimage out of the corpus's own bytes and comparing
// is what separates a membership tag keyed by membership_key from one keyed by confirmation_key,
// for the reason the confirmation tag's own known answer test gives: the two are adjacent
// DeriveSecret calls over one parent, the same length, and a swap is well formed.
//
// Two public messages per suite rather than one, because they differ in the shape of what they
// authenticate: a proposal's FramedContentAuthData is a signature alone and a commit's is a
// signature followed by a confirmation_tag. A tag taken over a truncated preimage — the content
// without the auth data, say — would reproduce neither.
func TestMembershipTagMatchesTheMlswgMessageProtection(t *testing.T) {
	entries := []messageProtectionKatEntry{}
	mustLoadAuthenticatedCorpus(t, messageProtectionKatFile, &entries)
	if len(entries) == 0 {
		t.Fatalf("%s parsed to no entries, so every comparison below would run over nothing", messageProtectionKatFile)
	}
	epochs := ksVectorEpochs(t)
	compared := 0
	matched := []CipherSuite{}
	for _, entry := range entries {
		suite := CipherSuite(entry.CipherSuite)
		if !IsRegisteredSuite(suite) {
			continue
		}
		matched = append(matched, suite)
		crypto := mustProvider(t, suite)
		nh := crypto.HashSize()
		suiteAt := fmt.Sprintf("%s suite %#04x", messageProtectionKatFile, uint16(suite))
		membershipKey := mustDecodeHex(t, suiteAt+" membership_key", entry.MembershipKey)
		if len(membershipKey) != nh {
			t.Fatalf("%s: the published membership_key is %d bytes and this suite's KDF.Nh is %d",
				suiteAt, len(membershipKey), nh)
		}
		groupContext, err := syntax.Marshal(&GroupContext{
			Version:                 ProtocolVersionMls10,
			CipherSuite:             suite,
			GroupId:                 mustDecodeHex(t, suiteAt+" group_id", entry.GroupId),
			Epoch:                   entry.Epoch,
			TreeHash:                mustDecodeHex(t, suiteAt+" tree_hash", entry.TreeHash),
			ConfirmedTranscriptHash: mustDecodeHex(t, suiteAt+" confirmed_transcript_hash", entry.ConfirmedTranscriptHash),
			Extensions:              nil,
		})
		if err != nil {
			t.Fatalf("%s: encode the group context these messages were framed under: %v", suiteAt, err)
		}
		for _, message := range []struct {
			what string
			body string
			pub  string
		}{
			{what: "proposal_pub", body: entry.Proposal, pub: entry.ProposalPub},
			{what: "commit_pub", body: entry.Commit, pub: entry.CommitPub},
		} {
			at := suiteAt + " " + message.what
			publicMessage := mustDecodeHex(t, at, message.pub)
			tbm := authenticatedContentTbm(t, at, publicMessage,
				mustDecodeHex(t, at+" body", message.body), groupContext, nh)
			want := publishedTagAtTheTail(t, at, publicMessage, nh)

			schedule := ksScheduleForSuite(t, epochs, suite)
			installTheCorpusKey(t, at, &schedule.Secrets().Membership, membershipKey,
				schedule.MembershipTag(tbm), want)
			got := schedule.MembershipTag(tbm)
			if !bytes.Equal(got, want) {
				t.Errorf("%s: MembershipTag = %x, and mlswg published %x. The tag is MAC(membership_key, AuthenticatedContentTBM) and nothing else; confirmation_key is the same length and produces an answer just as well formed",
					at, got, want)
			}
			if !schedule.VerifyMembershipTag(tbm, want) {
				t.Errorf("%s: VerifyMembershipTag refused the tag mlswg published for this key and this message", at)
			}
			compared++
		}
	}
	if compared != membershipTagComparisons {
		t.Fatalf("%d membership tags were compared against %s, want %d; the loop matched %v",
			compared, messageProtectionKatFile, membershipTagComparisons, matched)
	}
	if got := slices.Sorted(slices.Values(matched)); !slices.Equal(got, Suites()) {
		t.Fatalf("%s answered for %v and this package registers %v", messageProtectionKatFile, got, Suites())
	}
}

// tagPair is one (compute, verify) pair of the key schedule's tag surface, with the sweep
// input the other gates already drive that pair by.
type tagPair struct {
	compute    string
	verify     string
	tagOf      func(schedule *KeySchedule, data []byte) []byte
	verifiedBy func(schedule *KeySchedule, data []byte, tag []byte) bool
	dataFor    func(schedule *KeySchedule) []byte
}

// tagVerifierPairs derives the tag surface off *KeySchedule's own method set rather than
// listing it: an exported method named Verify<X> answering a bool over two byte slices, whose
// <X> is an exported method answering a byte slice over one. A third tag added to this type
// joins every sweep below by existing, which is the shape this repository has understated
// fourteen times when it was written as a list.
//
// The derivation also has to ACCOUNT FOR every exported method of the type that answers a bool,
// and reports one it cannot pair rather than skipping it. Guardrail 7 is that these verifiers
// are among the only functions in this package permitted to return one at all, so a bool
// answering method that is not a verifier — or a verifier that did not follow the naming — is
// either a new thing to reason about or a way out of every sweep below, and both are worth a
// failure. The sweep input is read out of keyScheduleMethodArgumentRows, the same rows guardrail
// 6 and the erased epoch gate are driven through, so a pair is described in one place.
func tagVerifierPairs(t *testing.T) []tagPair {
	t.Helper()
	scheduleType := reflect.TypeOf((*KeySchedule)(nil))
	byteSlice := reflect.TypeOf([]byte(nil))
	pairs := []tagPair{}
	answeringABool := []string{}
	for i := range scheduleType.NumMethod() {
		verify := scheduleType.Method(i)
		if verify.Type.NumOut() != 1 || verify.Type.Out(0).Kind() != reflect.Bool {
			continue
		}
		answeringABool = append(answeringABool, verify.Name)
		if !strings.HasPrefix(verify.Name, "Verify") {
			t.Errorf("(*KeySchedule).%s answers a bool and is not named Verify<something>, so it is outside the tag verifier class every sweep below is derived from; guardrail 7 says a bool is a shape this package uses only where a caller must return on false",
				verify.Name)
			continue
		}
		if verify.Type.NumIn() != 3 || verify.Type.In(1) != byteSlice || verify.Type.In(2) != byteSlice {
			t.Errorf("(*KeySchedule).%s is %s and a tag verifier takes the data and the tag, both byte slices",
				verify.Name, verify.Type)
			continue
		}
		name := strings.TrimPrefix(verify.Name, "Verify")
		compute, declared := scheduleType.MethodByName(name)
		if !declared {
			t.Errorf("(*KeySchedule).%s verifies a tag and this type declares no %s to compute one, so nothing pins the two against each other",
				verify.Name, name)
			continue
		}
		if compute.Type.NumIn() != 2 || compute.Type.In(1) != byteSlice ||
			compute.Type.NumOut() != 1 || compute.Type.Out(0) != byteSlice {
			t.Errorf("(*KeySchedule).%s is %s and the computing half of a tag pair takes the data and answers the tag",
				name, compute.Type)
			continue
		}
		rows, driven := keyScheduleMethodArgumentRows[name]
		if !driven {
			t.Errorf("keyScheduleMethodArgumentRows has no rows for (*KeySchedule).%s, so the sweeps below have no data to drive the pair with",
				name)
			continue
		}
		pairs = append(pairs, tagPair{
			compute: name,
			verify:  verify.Name,
			tagOf: func(schedule *KeySchedule, data []byte) []byte {
				answered := compute.Func.Call([]reflect.Value{reflect.ValueOf(schedule), reflect.ValueOf(data)})
				return answered[0].Bytes()
			},
			verifiedBy: func(schedule *KeySchedule, data []byte, tag []byte) bool {
				answered := verify.Func.Call([]reflect.Value{
					reflect.ValueOf(schedule), reflect.ValueOf(data), reflect.ValueOf(tag)})
				return answered[0].Bool()
			},
			dataFor: func(schedule *KeySchedule) []byte {
				built := rows(schedule)
				if len(built) == 0 {
					t.Fatalf("keyScheduleMethodArgumentRows drives (*KeySchedule).%s with no rows, so this sweep has no input", name)
				}
				return built[0][0].Bytes()
			},
		})
	}
	if len(pairs) < 2 {
		t.Fatalf("the tag surface derived to %d pairs out of the bool answering methods %v, and this task lands two",
			len(pairs), answeringABool)
	}
	return pairs
}

// TestEveryTagVerifierRefusesEveryAlterationOfWhatItWasGiven is the half of this task that a
// verifier returning true unconditionally cannot survive.
//
// "The tag verifies" is not a property worth asserting on its own: it holds for a verifier that
// looks at nothing, and that verifier is a one line edit and a total authentication bypass. The
// property is the REFUSALS, and every class of them here is derived over the length rather than
// sampled at a position somebody picked — because the two defects this project has actually
// shipped were a comparison that stopped early and a comparison that never happened, and both
// are invisible to a test that flips byte zero.
//
// Five classes, each satisfiable by a different wrong implementation:
//
//   - every single bit of the tag, all 8*len of them, because a comparison that ignores the
//     tail accepts every alteration in it;
//   - every single bit of the data, because a verifier that macs the wrong bytes — or a fixed
//     string — accepts a tag over content nobody sent;
//   - every truncation of the tag from empty upwards, because a prefix comparison accepts a one
//     byte tag an attacker finds in 256 tries, and because a length mismatch must be a refusal
//     rather than a panic;
//   - the tag lengthened by one octet, and the all zero tag, which is what an uninitialised or
//     erased tag field arrives as;
//   - the tag another EPOCH produces over the same data, and the tag the other PAIR produces
//     over the same data. The second is what says confirmation_key and membership_key have not
//     been made the same key: a verifier reading the wrong one of the two accepts its sibling's
//     tag, and every length and shape assertion in this file goes on holding.
func TestEveryTagVerifierRefusesEveryAlterationOfWhatItWasGiven(t *testing.T) {
	epochs := ksVectorEpochs(t)
	if len(epochs) < 2 {
		t.Fatalf("the corpus answered for %d epochs and the wrong key row here needs two", len(epochs))
	}
	pairs := tagVerifierPairs(t)
	for index, epoch := range epochs {
		schedule := epoch.schedule(t)
		otherEpoch := epochs[(index+1)%len(epochs)].schedule(t)
		for _, pair := range pairs {
			at := fmt.Sprintf("%s %s", epoch.at, pair.verify)
			data := pair.dataFor(schedule)
			tag := pair.tagOf(schedule, data)
			if len(tag) != epoch.crypto.HashSize() {
				t.Fatalf("%s: %s answered %d bytes and the mac of this suite is %d, so the sweeps below would run over the wrong thing",
					at, pair.compute, len(tag), epoch.crypto.HashSize())
			}
			// the control: without it every refusal below is satisfied by a verifier that
			// refuses everything, which authenticates nothing and breaks no test that only
			// looks for false
			if !pair.verifiedBy(schedule, data, tag) {
				t.Fatalf("%s: the tag this schedule just computed did not verify, so every refusal below says nothing", at)
			}

			refused := 0
			for i := range tag {
				for bit := range 8 {
					altered := bytes.Clone(tag)
					altered[i] ^= 1 << bit
					if !pair.verifiedBy(schedule, data, altered) {
						refused++
					}
				}
			}
			if want := 8 * len(tag); refused != want {
				t.Errorf("%s: %d of %d single bit alterations of the TAG were refused; a comparison that stops early accepts every alteration past where it stopped",
					at, refused, want)
			}

			refused = 0
			for i := range data {
				for bit := range 8 {
					altered := bytes.Clone(data)
					altered[i] ^= 1 << bit
					if !pair.verifiedBy(schedule, altered, tag) {
						refused++
					}
				}
			}
			if want := 8 * len(data); refused != want {
				t.Errorf("%s: %d of %d single bit alterations of the DATA were refused; a verifier that macs anything but the bytes it was handed authenticates content nobody sent",
					at, refused, want)
			}

			for n := range len(tag) {
				if pair.verifiedBy(schedule, data, tag[:n]) {
					t.Errorf("%s: the first %d bytes of a %d byte tag verified; a prefix comparison accepts every truncation of a valid tag",
						at, n, len(tag))
				}
			}
			for _, extra := range []byte{0x00, 0xff} {
				if pair.verifiedBy(schedule, data, append(bytes.Clone(tag), extra)) {
					t.Errorf("%s: a tag with a trailing %#02x verified, so the length is not part of the comparison", at, extra)
				}
			}
			if pair.verifiedBy(schedule, data, nil) {
				t.Errorf("%s: a nil tag verified", at)
			}
			if pair.verifiedBy(schedule, data, []byte{}) {
				t.Errorf("%s: an empty tag verified", at)
			}
			if pair.verifiedBy(schedule, data, make([]byte, len(tag))) {
				t.Errorf("%s: an all zero tag verified, which is what an unset tag field arrives as", at)
			}

			// the wrong key, from two directions
			fromAnotherEpoch := pair.tagOf(otherEpoch, data)
			if bytes.Equal(fromAnotherEpoch, tag) {
				t.Fatalf("%s: another epoch's schedule produced the same tag over the same data, so the row below pins nothing about the key", at)
			}
			if pair.verifiedBy(schedule, data, fromAnotherEpoch) {
				t.Errorf("%s: a tag another epoch's key produced verified in this one", at)
			}
			for _, other := range pairs {
				if other.compute == pair.compute {
					continue
				}
				sibling := other.tagOf(schedule, data)
				if bytes.Equal(sibling, tag) {
					t.Errorf("%s: %s and %s answer the same tag over the same data, so the two are keyed by one secret; confirmation_key and membership_key are adjacent DeriveSecret calls and collapsing them is a one line edit",
						at, pair.compute, other.compute)
					continue
				}
				if pair.verifiedBy(schedule, data, sibling) {
					t.Errorf("%s: the tag %s produced over the same data verified as a %s, so the two are not keyed apart",
						at, other.compute, pair.compute)
				}
			}
		}
	}
}

// TestTheTagsAreKeyedByTheSecretsTheyName pins WHICH of the nine each tag is taken under, at
// every epoch of the corpus rather than at the two the published tags cover.
//
// The two known answer tests above hold the key identity against somebody else's numbers, and
// they are the anchor. This is the same claim at every epoch and stated as the derivation: the
// confirmation tag is the mac under EpochSecrets.Confirmation and the membership tag is the mac
// under EpochSecrets.Membership, and each of those secrets is itself held to the published
// corpus by TestKeyScheduleMatchesTheMlswgKeySchedule. A swap keeps both answers KDF.Nh bytes
// and keeps both verifiers agreeing with their own computation.
func TestTheTagsAreKeyedByTheSecretsTheyName(t *testing.T) {
	for _, epoch := range ksVectorEpochs(t) {
		schedule := epoch.schedule(t)
		secrets := schedule.Secrets()
		transcriptHash := tagSweepTranscriptHash(schedule)
		if bytes.Equal(secrets.Confirmation, secrets.Membership) {
			t.Fatalf("%s: confirmation_key and membership_key are the same bytes, so nothing below can tell the two apart", epoch.at)
		}
		if got, want := schedule.ConfirmationTag(transcriptHash),
			epoch.crypto.Mac(secrets.Confirmation, transcriptHash); !bytes.Equal(got, want) {
			t.Errorf("%s: ConfirmationTag = %x and MAC(confirmation_key, hash) = %x", epoch.at, got, want)
		}
		if got, want := schedule.MembershipTag(tagSweepTbm),
			epoch.crypto.Mac(secrets.Membership, tagSweepTbm); !bytes.Equal(got, want) {
			t.Errorf("%s: MembershipTag = %x and MAC(membership_key, tbm) = %x", epoch.at, got, want)
		}
	}
}

// TestTheTagsRefuseAnEpochWhoseSecretsHaveBeenErased states the refusal in its own terms, beside
// the derived gate that also covers it.
//
// An epoch leaving PastEpochWindow is zeroized in place. A mac under KDF.Nh zero bytes is not a
// weak tag, it is a PUBLIC one: every party can compute it with no knowledge of the group. So a
// tag taken from an erased epoch authenticates nobody while looking exactly like a tag, and a
// verifier that still compares accepts a forgery anybody could have produced — with err == nil,
// because these signatures have no error to carry.
//
// TestEveryMethodDerivingFromTheEpochsSecretsRefusesAnErasedEpoch derives this class off the
// package's own source and covers all four of these methods. This is the same property written
// where a reader of the tag surface will find it, and it does not rest on that derivation still
// including them.
func TestTheTagsRefuseAnEpochWhoseSecretsHaveBeenErased(t *testing.T) {
	pairs := tagVerifierPairs(t)
	for _, epoch := range ksVectorEpochs(t) {
		schedule := epoch.schedule(t)
		live := map[string][]byte{}
		for _, pair := range pairs {
			data := pair.dataFor(schedule)
			live[pair.compute] = pair.tagOf(schedule, data)
			if len(live[pair.compute]) == 0 {
				t.Fatalf("%s: %s answered nothing over a LIVE epoch, so a refusal after the erase would say nothing",
					epoch.at, pair.compute)
			}
		}
		secrets := reflect.ValueOf(schedule.Secrets()).Elem()
		for i := range secrets.NumField() {
			zeroizeSecret(secrets.Field(i).Bytes())
		}
		for _, pair := range pairs {
			data := pair.dataFor(schedule)
			if answered := pair.tagOf(schedule, data); len(answered) != 0 {
				t.Errorf("%s: %s answered %d bytes over an erased epoch; a mac under KDF.Nh zero bytes is a value every party can compute, so it is a tag that authenticates nobody",
					epoch.at, pair.compute, len(answered))
			}
			if pair.verifiedBy(schedule, data, live[pair.compute]) {
				t.Errorf("%s: %s accepted over an erased epoch", epoch.at, pair.verify)
			}
			if pair.verifiedBy(schedule, data, epoch.crypto.Mac(make([]byte, epoch.crypto.HashSize()), data)) {
				t.Errorf("%s: %s accepted the tag anybody can compute under KDF.Nh zero bytes, which is what the erase leaves the key as",
					epoch.at, pair.verify)
			}
		}
	}
}

// boolAnsweringDerivations is the intersection of two classes this file already derives: an
// exported declaration that reaches PAST the storage holding the nine and into one of them, and
// one whose single result is a bool.
//
// That intersection is what "compares a tag" is, mechanically. A method cannot compare a tag
// under an epoch key without reading one of the nine, and a method that answers anything but a
// bool is not answering a comparison. Deriving it beats naming the two verifiers for the reason
// standing rule 5 gives: a third one lands inside the class rather than beside it.
func boolAnsweringDerivations(declared []sourceDeclaration, storage string, byteSlices []string) []string {
	deriving := theMethodsDerivingFromOneOfTheNine(declared, storage, byteSlices)
	names := []string{}
	for _, one := range declared {
		if !one.exported || one.body == nil {
			continue
		}
		if len(one.results) != 1 || one.results[0] != "bool" {
			continue
		}
		if !slices.Contains(deriving, one.name) {
			continue
		}
		names = append(names, one.name)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// returnsNotRoutedThroughMacVerify reads one declaration for every value it can hand back, and
// answers the ones that are neither the literal false nor a call to MacVerify on this receiver's
// own provider, together with how many of the sanctioned calls it found.
//
// A return statement rather than a token search, because guardrail 8 is about what the answer IS
// and not about which words appear. The three shapes a token list misses are all returns: `return
// true` inserted at the top of the body carries no banned comparator at all; a byte loop that
// leaks the position of the first differing byte carries none either and ends in `return true`;
// and a comparison made by some helper of this package's own is a call the list was never written
// for. All three are a returned value that is not the sanctioned call.
//
// The provider is required to be the RECEIVER's, spelled through its own field, because a
// MacVerify reached on anything else is a mac under a key this epoch did not choose.
func returnsNotRoutedThroughMacVerify(parsed parsedSource, function *ast.FuncDecl) ([]string, int) {
	receiver := ""
	if function.Recv != nil && len(function.Recv.List) == 1 && len(function.Recv.List[0].Names) == 1 {
		receiver = function.Recv.List[0].Names[0].Name
	}
	sanctioned := receiver + ".crypto.MacVerify"
	offending := []string{}
	viaMacVerify := 0
	ast.Inspect(function.Body, func(node ast.Node) bool {
		returns, isReturn := node.(*ast.ReturnStmt)
		if !isReturn {
			return true
		}
		for _, result := range returns.Results {
			if parsed.render(result) == "false" {
				continue
			}
			if call, isCall := result.(*ast.CallExpr); isCall && parsed.render(call.Fun) == sanctioned {
				viaMacVerify++
				continue
			}
			offending = append(offending, parsed.render(result))
		}
		return true
	})
	return offending, viaMacVerify
}

// tagVerifierRoutingControl declares one of each shape the two rules either side of it have to
// tell apart: the sanctioned body, the four comparators a ban list would have to have thought
// of, the byte loop that carries no comparator at all, the verifier that decides nothing, the
// one that reaches a provider it was not given, and an unexported one that is outside the class.
//
// Six of these are here because a control that does not DISCRIMINATE its own rule is a control
// that issues a broken matcher the clean bill a working one issues. Every half of every rule
// has to be the only thing reporting some member of this fixture, or that half can be deleted
// outright with the control still matching exactly what it wants:
//
//   - RefusesEverything answers the literal false and nothing else, so its offending list is
//     empty and only "answers no MacVerify of its own" reports it.
//   - VerifiesThroughTheProviderAfterAFastPath answers a real MacVerify AND something else, so
//     only the offending list reports it. Without it that whole accumulation can be replaced by
//     a discard and the control goes on matching, which is measured rather than supposed.
//   - RewritesTheTagAheadOfTheProvider and ComparesByteByByteAheadOfTheProvider answer the
//     sanctioned call and, besides it, nothing but false. That is the ROUTING rule's blind spot
//     and the whole reason the parameter rule exists: the first is a total authentication
//     bypass and the second is the guardrail 8 timing leak, and the routing rule is silent on
//     both. They sit in the routing gate's own wants as shapes it must be seen NOT to report.
//   - VerifiesTheTagAgainstItself and VerifiesAPrefixOfTheTagThroughTheProvider answer the
//     sanctioned call over bytes that are not the ones they were handed, and touch no parameter
//     outside it, so only the argument half of the parameter rule reports them.
//
// hmac.Equal is in here deliberately. It is constant time and it is still wrong, because
// guardrail 8 names crypto/subtle.ConstantTimeCompare reached through CryptoProvider.MacVerify
// specifically: a second comparison site is a second place the length check can be dropped, and
// this package has already shipped a comparator outside a ban list once.
const tagVerifierRoutingControl = "package control\n" +
	"\n" +
	"type EpochSecrets struct {\n" +
	"\tConfirmation []byte\n" +
	"}\n" +
	"\n" +
	"type Holder struct {\n" +
	"\tcrypto  CryptoProvider\n" +
	"\tsecrets EpochSecrets\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) VerifiesThroughTheProvider(data []byte, tag []byte) bool {\n" +
	"\tif !self.live(self.secrets.Confirmation) {\n" +
	"\t\treturn false\n" +
	"\t}\n" +
	"\treturn self.crypto.MacVerify(self.secrets.Confirmation, data, tag)\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) VerifiesWithBytesEqual(data []byte, tag []byte) bool {\n" +
	"\treturn bytes.Equal(self.crypto.Mac(self.secrets.Confirmation, data), tag)\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) VerifiesWithHmacEqual(data []byte, tag []byte) bool {\n" +
	"\treturn hmac.Equal(self.crypto.Mac(self.secrets.Confirmation, data), tag)\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) VerifiesWithAPrefix(data []byte, tag []byte) bool {\n" +
	"\treturn bytes.HasPrefix(self.crypto.Mac(self.secrets.Confirmation, data), tag)\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) VerifiesWithASubtleCallOfItsOwn(data []byte, tag []byte) bool {\n" +
	"\treturn subtle.ConstantTimeCompare(self.crypto.Mac(self.secrets.Confirmation, data), tag) == 1\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) VerifiesWithAByteLoop(data []byte, tag []byte) bool {\n" +
	"\texpected := self.crypto.Mac(self.secrets.Confirmation, data)\n" +
	"\tfor i := range expected {\n" +
	"\t\tif expected[i] != tag[i] {\n" +
	"\t\t\treturn false\n" +
	"\t\t}\n" +
	"\t}\n" +
	"\treturn true\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) AcceptsEverything(data []byte, tag []byte) bool {\n" +
	"\t_ = self.secrets.Confirmation\n" +
	"\treturn true\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) RefusesEverything(data []byte, tag []byte) bool {\n" +
	"\t_ = self.secrets.Confirmation\n" +
	"\treturn false\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) VerifiesThroughAProviderItWasNotGiven(data []byte, tag []byte) bool {\n" +
	"\treturn elsewhere.MacVerify(self.secrets.Confirmation, data, tag)\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) VerifiesThroughTheProviderAfterAFastPath(data []byte, tag []byte) bool {\n" +
	"\tif len(tag) == 64 {\n" +
	"\t\treturn true\n" +
	"\t}\n" +
	"\treturn self.crypto.MacVerify(self.secrets.Confirmation, data, tag)\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) RewritesTheTagAheadOfTheProvider(data []byte, tag []byte) bool {\n" +
	"\tif bytes.HasSuffix(data, chosenSuffix) {\n" +
	"\t\ttag = self.crypto.Mac(self.secrets.Confirmation, data)\n" +
	"\t}\n" +
	"\treturn self.crypto.MacVerify(self.secrets.Confirmation, data, tag)\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) ComparesByteByByteAheadOfTheProvider(data []byte, tag []byte) bool {\n" +
	"\texpected := self.crypto.Mac(self.secrets.Confirmation, data)\n" +
	"\tif len(tag) != len(expected) {\n" +
	"\t\treturn false\n" +
	"\t}\n" +
	"\tfor i := range expected {\n" +
	"\t\tif expected[i] != tag[i] {\n" +
	"\t\t\treturn false\n" +
	"\t\t}\n" +
	"\t}\n" +
	"\treturn self.crypto.MacVerify(self.secrets.Confirmation, data, tag)\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) VerifiesTheTagAgainstItself(data []byte, tag []byte) bool {\n" +
	"\treturn self.crypto.MacVerify(self.secrets.Confirmation, tag, tag)\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) VerifiesAPrefixOfTheTagThroughTheProvider(data []byte, tag []byte) bool {\n" +
	"\treturn self.crypto.MacVerify(self.secrets.Confirmation, data, tag[:8])\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) verifiesBadlyAndIsUnexported(data []byte, tag []byte) bool {\n" +
	"\treturn bytes.Equal(self.crypto.Mac(self.secrets.Confirmation, data), tag)\n" +
	"}\n"

// tagVerifierRoutingControlClass is exactly what boolAnsweringDerivations must read out of the
// fixture above, and both gates over that fixture assert it before they assert anything else.
//
// Exact rather than a floor. A class that widened to take in the unexported member, or narrowed
// to drop one of the bad shapes, would go on to read the real source the same wrong way and
// report the clean bill a working one reports.
var tagVerifierRoutingControlClass = []string{
	"AcceptsEverything",
	"ComparesByteByByteAheadOfTheProvider",
	"RefusesEverything",
	"RewritesTheTagAheadOfTheProvider",
	"VerifiesAPrefixOfTheTagThroughTheProvider",
	"VerifiesTheTagAgainstItself",
	"VerifiesThroughAProviderItWasNotGiven",
	"VerifiesThroughTheProvider",
	"VerifiesThroughTheProviderAfterAFastPath",
	"VerifiesWithAByteLoop",
	"VerifiesWithAPrefix",
	"VerifiesWithASubtleCallOfItsOwn",
	"VerifiesWithBytesEqual",
	"VerifiesWithHmacEqual",
}

// TestEveryTagVerifierComparesThroughMacVerifyAndNothingElse is guardrail 8 over this task's two
// bools, read as a shape rather than as a word list.
//
// The behavioural sweep next door catches a comparison that stops early, because a truncated
// comparison ACCEPTS things. It cannot catch a comparison that answers correctly and leaks where
// the first difference was: a byte loop written ahead of, or instead of, MacVerify refuses
// exactly what MacVerify refuses and returns sooner on a tag that differs early. No behavioural
// test in go can see that, and a timing measurement over a 32 byte comparison on this machine is
// noise. So what is asserted is what was actually verified — by inspection, mechanically, so it
// survives an edit nobody reruns this for.
//
// Both halves are derived rather than listed. The class is the intersection two other gates in
// this file already produce, so a third verifier joins it by existing; and the rule is over
// returned VALUES, so it holds against a comparison spelled as control flow, as a helper of this
// package's own, or as a comparator nobody thought to ban. The control fixture commits every one
// of those shapes and each must be reported, because a matcher that had stopped matching issues
// the real source exactly the clean bill a correct one issues.
func TestEveryTagVerifierComparesThroughMacVerifyAndNothingElse(t *testing.T) {
	// the control first, on both halves
	control := mustParseText(t, "the tag verifier routing control", tagVerifierRoutingControl)
	controlDeclared := declaredIn(control)
	controlClass := boolAnsweringDerivations(controlDeclared, "secrets", []string{"[]byte"})
	wantClass := tagVerifierRoutingControlClass
	if !slices.Equal(controlClass, wantClass) {
		t.Fatalf("the class read %v out of the control, want %v; it is not intersecting a bool result with a read of one of the nine, or it is reading the unexported one",
			controlClass, wantClass)
	}
	reported := []string{}
	for _, name := range controlClass {
		offending, viaMacVerify := returnsNotRoutedThroughMacVerify(control, control.declarationOf(t, "*Holder", name))
		if len(offending) != 0 || viaMacVerify == 0 {
			reported = append(reported, name)
		}
	}
	// the four the routing rule must be seen NOT to report are absent on purpose: each of them
	// answers the sanctioned call and, besides it, nothing but false. They are this rule's blind
	// spot, they are caught by the parameter gate below, and naming them here is what keeps a
	// reader from believing this rule covers them.
	wantReported := []string{
		"AcceptsEverything",
		"RefusesEverything",
		"VerifiesThroughAProviderItWasNotGiven",
		"VerifiesThroughTheProviderAfterAFastPath",
		"VerifiesWithAByteLoop",
		"VerifiesWithAPrefix",
		"VerifiesWithASubtleCallOfItsOwn",
		"VerifiesWithBytesEqual",
		"VerifiesWithHmacEqual",
	}
	if !slices.Equal(reported, wantReported) {
		t.Fatalf("the rule reported %v out of the control, want %v; a shape it lets through is a shape the real source can be written in",
			reported, wantReported)
	}

	// then this package's own source
	for _, verifier := range theTagVerifiersOfThisPackage(t) {
		offending, viaMacVerify := returnsNotRoutedThroughMacVerify(verifier.host, verifier.function)
		if len(offending) != 0 {
			t.Errorf("%s can answer %v, and guardrail 8 says a tag comparison answers CryptoProvider.MacVerify and nothing else: that is where crypto/subtle.ConstantTimeCompare and the length refusal ahead of it live",
				verifier.name, offending)
		}
		if viaMacVerify == 0 {
			t.Errorf("%s never answers a call to MacVerify on its own provider, so whatever it decides with is not the sanctioned comparison",
				verifier.name)
		}
	}
}

// tagVerifierSourceDeclaration is one member of the verifier class together with the parsed file
// it was read out of, because every rule below renders nodes of it back to source and a node
// rendered against the wrong file set gives the wrong positions.
type tagVerifierSourceDeclaration struct {
	name     string
	host     parsedSource
	function *ast.FuncDecl
}

// theTagVerifiersOfThisPackage is the class both gates over this surface run, derived twice and
// required to agree.
//
// Off the SOURCE it is the exported bool answering declarations that reach past the storage
// holding the epoch's nine secrets and into one of them. Off the COMPILED type it is the verify
// half of every (compute, verify) pair *KeySchedule's own method set produces. Either derivation
// on its own can go quiet — the source one if a verifier is declared in a file this scan does not
// read, the compiled one if a verifier stops following the naming — and a gate that has gone
// quiet issues the real source exactly the clean bill a working one issues. So the difference
// between the two is a failure rather than a smaller class.
func theTagVerifiersOfThisPackage(t *testing.T) []tagVerifierSourceDeclaration {
	t.Helper()
	files := []parsedSource{}
	structs := map[string]*ast.StructType{}
	for _, path := range packageLevelFunctions(t).files {
		parsed := mustParseSource(t, path)
		files = append(files, parsed)
		structTypesIn(parsed, structs)
	}
	holders := theTypesDeclaringTheEpochSecret(structs)
	if len(holders) != 1 {
		t.Fatalf("this package's source has %v DECLARING the epoch secret storage and this gate reads one declarer; a type that merely POINTS at one is the transitive reading and belongs to guardrail 6's surface gates rather than to this one",
			holders)
	}
	class := boolAnsweringDerivations(declaredAcross(files), epochSecretsStorageFieldIn(t, structs, holders[0]),
		slices.Concat([]string{"[]byte"}, packageByteSliceTypeNames(t)))
	if len(class) == 0 {
		t.Fatalf("no exported declaration of this package answers a bool off one of the nine, and this task lands two, so this gate is demanding nothing")
	}
	compiled := []string{}
	for _, pair := range tagVerifierPairs(t) {
		compiled = append(compiled, pair.verify)
	}
	slices.Sort(compiled)
	if !slices.Equal(class, compiled) {
		t.Errorf("this gate reads %v out of the source and *KeySchedule compiles the verifiers %v; the difference is a comparison this gate never looked at",
			class, compiled)
	}
	t.Logf("%d exported declarations answer a bool off one of the nine: %v", len(class), class)
	verifiers := []tagVerifierSourceDeclaration{}
	for _, name := range class {
		one := tagVerifierSourceDeclaration{name: name}
		for _, parsed := range files {
			for _, declaration := range parsed.file.Decls {
				candidate, isFunction := declaration.(*ast.FuncDecl)
				if isFunction && candidate.Name.Name == name && candidate.Body != nil {
					one.host, one.function = parsed, candidate
				}
			}
		}
		if one.function == nil {
			t.Fatalf("%s is in the class and no file of this package declares it, so this gate cannot read it", name)
		}
		verifiers = append(verifiers, one)
	}
	return verifiers
}

// tagVerifierParameterUse is what one verifier does with the bytes it was handed.
type tagVerifierParameterUse struct {
	// the parameter identifiers, in the order the signature writes them
	declared []string
	// every mention of one of them that is not a bare argument of the sanctioned call, with the
	// position, because the failure has to say where to look
	touched []string
	// the trailing arguments of the first sanctioned call, as they are written
	answered []string
	// how many calls to MacVerify on this receiver's own provider the body holds
	sanctioned int
}

// answersOverWhatItWasGivenUntouched is the whole rule in one line: the parameters are mentioned
// in exactly one place, that place is the argument list of the one MacVerify call on this
// receiver's own provider, and they are in the order the signature handed them over.
func (self tagVerifierParameterUse) answersOverWhatItWasGivenUntouched() bool {
	return len(self.touched) == 0 && self.sanctioned == 1 && slices.Equal(self.answered, self.declared)
}

// howTheVerifierUsesItsParameters reads one declaration for every mention of its own parameters,
// and for the arguments the sanctioned comparison is answered over.
//
// OCCURRENCE rather than a list of the ways a slice can be written to, because that list cannot
// be written: `tag = x` is an assignment, `tag[0] = 1` is an index, `copy(tag, x)`
// and `io.ReadFull(r, tag)` rewrite the same array through a call that assigns nothing, and
// a closure can do it with the parameter named nowhere near the write. A rule that had to name
// those shapes is the hand written class this repository has understated fourteen times. One
// mention outside the comparison covers every one of them at once, and it covers the READ half
// in the same stroke: a byte loop over the tag writes nothing at all and hands the index of the
// first differing octet to anyone who can time the call.
//
// Nothing is excused for looking like something else. A field or a composite literal key that
// shares a parameter's name is reported, because the alternative is a rule that can be walked
// past by choosing names, and a spurious report is a loud failure while a masked one is silence.
//
// The sanctioned mention has to be the BARE identifier. `MacVerify(key, data, tag[:8])`
// mentions the parameter and compares a prefix of it, which accepts every truncation of a valid
// tag; a slice expression is not the identifier, so it is reported.
func howTheVerifierUsesItsParameters(parsed parsedSource, function *ast.FuncDecl) tagVerifierParameterUse {
	receiver := ""
	if function.Recv != nil && len(function.Recv.List) == 1 && len(function.Recv.List[0].Names) == 1 {
		receiver = function.Recv.List[0].Names[0].Name
	}
	sanctioned := receiver + ".crypto.MacVerify"
	use := tagVerifierParameterUse{declared: []string{}, touched: []string{}}
	if function.Type.Params != nil {
		for _, field := range function.Type.Params.List {
			for _, name := range field.Names {
				if name.Name != "_" {
					use.declared = append(use.declared, name.Name)
				}
			}
		}
	}
	// the one place a parameter is allowed to be written: a bare argument of the sanctioned
	// call. Identity rather than name, so a second identifier spelled the same elsewhere in the
	// body is still a mention.
	allowed := map[*ast.Ident]bool{}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall || parsed.render(call.Fun) != sanctioned {
			return true
		}
		use.sanctioned++
		for _, argument := range call.Args {
			if bare, isIdent := argument.(*ast.Ident); isIdent {
				allowed[bare] = true
			}
		}
		// the TRAILING arguments, so the rule does not have to know how many arguments ahead of
		// the data and the tag the key takes
		if use.sanctioned == 1 && len(call.Args) >= len(use.declared) {
			for _, argument := range call.Args[len(call.Args)-len(use.declared):] {
				use.answered = append(use.answered, parsed.render(argument))
			}
		}
		return true
	})
	ast.Inspect(function.Body, func(node ast.Node) bool {
		identifier, isIdent := node.(*ast.Ident)
		if !isIdent || allowed[identifier] || !slices.Contains(use.declared, identifier.Name) {
			return true
		}
		use.touched = append(use.touched,
			identifier.Name+" at "+parsed.fileSet.Position(identifier.Pos()).String())
		return true
	})
	return use
}

// TestEveryTagVerifierAnswersOverTheBytesItWasGivenUntouched is the half of guardrail 8 the
// routing gate above cannot reach.
//
// That gate reads returned VALUES, so it sees every way a verifier can hand back the wrong
// answer. What it cannot see is a verifier that hands back the RIGHT call over the wrong bytes.
// Two shapes live in exactly that gap, and both were measured leaving the whole suite green:
//
//   - the bypass. `if bytes.HasSuffix(data, chosen) { tag = self.crypto.Mac(key, data) }`
//     written ahead of an untouched `return self.crypto.MacVerify(key, data, tag)` replaces
//     the tag with the one that is about to verify. Every return is still the sanctioned call, so
//     the routing rule reports nothing; every behavioural sweep still watches a verifier refuse
//     altered tags, because the rewrite fires only on data the forger chose. It is a total
//     authentication bypass on both verifiers at once, proved live against all ten corpus epochs.
//   - the leak. A byte by byte comparison written AHEAD of the sanctioned call answers exactly
//     what MacVerify answers, so no vector, no bit flip sweep and no KAT can see it, and it hands
//     the index of the first differing octet to anyone who can time the call. Its returns are the
//     literal false and the sanctioned call, so the routing rule reports nothing there either.
//     That is the timing leak guardrail 8 exists for.
//
// What the two have in common is not a shape of statement. It is that the verifier TOUCHED the
// bytes it was handed. So that is the rule: a verifier mentions each of its parameters exactly
// once, as a bare argument of the one MacVerify call on its own provider, in the order it was
// handed them, and does nothing else with them at all. No list of the ways to write to a slice
// has to be right for that to hold, which is the point of writing it this way round.
//
// A length check of the verifier's own is refused by this too, and deliberately: MacVerify
// refuses a length mismatch ahead of the comparison, so a check up here is a second place that
// refusal can be dropped and the first statement a prefix comparison would grow out of.
func TestEveryTagVerifierAnswersOverTheBytesItWasGivenUntouched(t *testing.T) {
	// the control first, and on each half SEPARATELY, because a half no member of the fixture
	// is the only witness for is a half that can be deleted with the control still matching
	// exactly — which is how the routing gate beside this one came to carry a dead one
	control := mustParseText(t, "the tag verifier routing control", tagVerifierRoutingControl)
	controlClass := boolAnsweringDerivations(declaredIn(control), "secrets", []string{"[]byte"})
	if !slices.Equal(controlClass, tagVerifierRoutingControlClass) {
		t.Fatalf("the class read %v out of the control, want %v; it is not intersecting a bool result with a read of one of the nine, or it is reading the unexported one",
			controlClass, tagVerifierRoutingControlClass)
	}
	touching, answeringOverSomethingElse, reported := []string{}, []string{}, []string{}
	for _, name := range controlClass {
		use := howTheVerifierUsesItsParameters(control, control.declarationOf(t, "*Holder", name))
		if len(use.touched) != 0 {
			touching = append(touching, name)
		}
		if use.sanctioned != 1 || !slices.Equal(use.answered, use.declared) {
			answeringOverSomethingElse = append(answeringOverSomethingElse, name)
		}
		if !use.answersOverWhatItWasGivenUntouched() {
			reported = append(reported, name)
		}
	}
	// three of these are the ones the OTHER half never sees: the rewrite, the byte loop and the
	// fast path all answer the sanctioned call over the right identifiers in the right order,
	// and get at the bytes on the way
	wantTouching := []string{
		"ComparesByteByByteAheadOfTheProvider",
		"RewritesTheTagAheadOfTheProvider",
		"VerifiesAPrefixOfTheTagThroughTheProvider",
		"VerifiesThroughAProviderItWasNotGiven",
		"VerifiesThroughTheProviderAfterAFastPath",
		"VerifiesWithAByteLoop",
		"VerifiesWithAPrefix",
		"VerifiesWithASubtleCallOfItsOwn",
		"VerifiesWithBytesEqual",
		"VerifiesWithHmacEqual",
	}
	if !slices.Equal(touching, wantTouching) {
		t.Fatalf("the touch half reported %v out of the control, want %v; a mention it lets through is a statement the real verifiers can be written with",
			touching, wantTouching)
	}
	// and three of these are the ones the touch half never sees: the two that decide without
	// comparing anything, and the one that verifies the tag against itself, all touch nothing
	// outside the call and none of them compares what it was handed
	wantAnsweringOverSomethingElse := []string{
		"AcceptsEverything",
		"RefusesEverything",
		"VerifiesAPrefixOfTheTagThroughTheProvider",
		"VerifiesTheTagAgainstItself",
		"VerifiesThroughAProviderItWasNotGiven",
		"VerifiesWithAByteLoop",
		"VerifiesWithAPrefix",
		"VerifiesWithASubtleCallOfItsOwn",
		"VerifiesWithBytesEqual",
		"VerifiesWithHmacEqual",
	}
	if !slices.Equal(answeringOverSomethingElse, wantAnsweringOverSomethingElse) {
		t.Fatalf("the argument half reported %v out of the control, want %v; a comparison it lets through is a comparison over bytes the caller never handed in",
			answeringOverSomethingElse, wantAnsweringOverSomethingElse)
	}
	// and exactly one member of that fixture obeys the whole rule: the sanctioned body
	wantReported := []string{}
	for _, name := range controlClass {
		if name != "VerifiesThroughTheProvider" {
			wantReported = append(wantReported, name)
		}
	}
	if !slices.Equal(reported, wantReported) {
		t.Fatalf("the rule reported %v out of the control, want %v; the one body it must clear is the sanctioned one, and every other shape in there is a way this surface has been broken",
			reported, wantReported)
	}

	// then this package's own source
	for _, verifier := range theTagVerifiersOfThisPackage(t) {
		use := howTheVerifierUsesItsParameters(verifier.host, verifier.function)
		if len(use.declared) < 2 {
			t.Errorf("%s declares the parameters %v, and a tag verifier is handed the data and the tag, so this gate has nothing to hold it to",
				verifier.name, use.declared)
			continue
		}
		if len(use.touched) != 0 {
			t.Errorf("%s mentions the bytes it was handed at %v, outside the comparison that is supposed to decide its answer; a statement that can reach the data or the tag ahead of MacVerify can rewrite either one into the pair that verifies, or read the tag an octet at a time",
				verifier.name, use.touched)
		}
		if use.sanctioned != 1 {
			t.Errorf("%s holds %d calls to MacVerify on its own provider and this gate holds it to exactly one; none is no sanctioned comparison at all, and a second one is a second answer this rule cannot tell from the first",
				verifier.name, use.sanctioned)
		}
		if !slices.Equal(use.answered, use.declared) {
			t.Errorf("%s answers MacVerify over %v and was handed %v; the comparison decides the answer only if what it compares is what the caller gave it, unaltered and in that order",
				verifier.name, use.answered, use.declared)
		}
	}
}

// ---------------------------------------------------------------------------
// Task 11: welcome_key and welcome_nonce.
//
// The hazard here is nonce reuse and it does not look like a bug. A key and a nonce derived
// together out of one secret are two byte slices that both look like random, and every cheap
// assertion passes for a wrong pair: the lengths are right, the values are non zero, the
// derivation is deterministic, the round trip round trips. This project has already shipped a
// nonce defect that passed 69 tests, because nothing observed the property the names claimed.
//
// One shape is deliberately NOT asserted below, and it is worth writing down because it is the
// obvious one. "The nonce is not the key's first Nn bytes" reads like the collision check this
// pair needs and is a comparison no implementation can fail: ExpandWithLabel binds the requested
// LENGTH into its own preimage, so a body that expanded both halves under ONE label answers a
// nonce that is not the key's prefix either, and two unrelated 12 byte pseudorandom values
// collide at 2^-96. What stands in its place is the published Welcome the pair has to open and
// the hand written expansion, which can tell one label from another.
// ---------------------------------------------------------------------------

// ksWelcomeVector is one entry of the mlswg welcome.json corpus.
//
// init_priv is the field labelKatWelcome does not carry and the one this section needs: it
// opens the EncryptedGroupSecrets the Welcome is addressed to, and joiner_secret is inside it.
type ksWelcomeVector struct {
	CipherSuite uint16 `json:"cipher_suite"`
	InitPriv    string `json:"init_priv"`
	KeyPackage  string `json:"key_package"`
	SignerPub   string `json:"signer_pub"`
	Welcome     string `json:"welcome"`
}

// ksWelcomeGroupSecrets is one EncryptedGroupSecrets of a published Welcome, decoded:
//
//	struct { KeyPackageRef new_member; HPKECiphertext encrypted_group_secrets; }
//	struct { opaque kem_output<V>; opaque ciphertext<V>; } HPKECiphertext
type ksWelcomeGroupSecrets struct {
	newMember  []byte
	kemOutput  []byte
	ciphertext []byte
}

// ksWelcomeWireFormat is mls_welcome, RFC 9420 section 6.1.
const ksWelcomeWireFormat = uint16(0x0003)

// ksWelcomeKatVectors is how many published Welcomes this package's registered suites account
// for. Asserted rather than assumed, for the reason keyScheduleKatEpochs is: a filter that
// stopped matching turns the sweep below into a loop that runs zero times and reports the clean
// run a complete sweep reports.
const ksWelcomeKatVectors = 2

// ksWelcomeMessage decodes a published Welcome far enough to reach the two things this section
// needs: the group secrets addressed to each new member, and the encrypted_group_info that
// welcome_key and welcome_nonce protect.
//
//	MLSMessage = version | wire_format | Welcome
//	Welcome    = cipher_suite | EncryptedGroupSecrets secrets<V> | opaque encrypted_group_info<V>
//
// Decoded by hand rather than through a Welcome type, because p7 owns that type and this task
// must not grow one. Every field goes through the syntax reader and the whole message is
// required to be consumed, so a mis-slicing is reported here rather than surfacing as an aead
// failure that would read as a defect in the derivation under test. The version, the wire format
// and the cipher suite are all checked for the same reason.
func ksWelcomeMessage(t *testing.T, at string, suite CipherSuite, encoded []byte) ([]ksWelcomeGroupSecrets, []byte) {
	t.Helper()
	reader := syntax.NewReader(encoded)
	version, err := reader.ReadUint16()
	if err != nil {
		t.Fatalf("%s: read the protocol version of the published welcome: %v", at, err)
	}
	if ProtocolVersion(version) != ProtocolVersionMls10 {
		t.Fatalf("%s: the published welcome opens at version %#04x, want mls10", at, version)
	}
	wireFormat, err := reader.ReadUint16()
	if err != nil {
		t.Fatalf("%s: read the wire format of the published welcome: %v", at, err)
	}
	if wireFormat != ksWelcomeWireFormat {
		t.Fatalf("%s: the published message is wire format %#04x, want mls_welcome %#04x",
			at, wireFormat, ksWelcomeWireFormat)
	}
	onTheWire, err := reader.ReadUint16()
	if err != nil {
		t.Fatalf("%s: read the welcome's cipher suite: %v", at, err)
	}
	if CipherSuite(onTheWire) != suite {
		t.Fatalf("%s: the published welcome names suite %#04x, so this row is decoding a message for another suite",
			at, onTheWire)
	}
	secrets, err := syntax.ReadVector(reader, func(r *syntax.Reader) (ksWelcomeGroupSecrets, error) {
		one := ksWelcomeGroupSecrets{}
		var readErr error
		if one.newMember, readErr = r.ReadOpaque(); readErr != nil {
			return one, readErr
		}
		if one.kemOutput, readErr = r.ReadOpaque(); readErr != nil {
			return one, readErr
		}
		one.ciphertext, readErr = r.ReadOpaque()
		return one, readErr
	})
	if err != nil {
		t.Fatalf("%s: read the encrypted group secrets: %v", at, err)
	}
	if len(secrets) == 0 {
		t.Fatalf("%s: the published welcome carries no encrypted group secrets", at)
	}
	encryptedGroupInfo, err := reader.ReadOpaque()
	if err != nil {
		t.Fatalf("%s: read encrypted_group_info: %v", at, err)
	}
	if err := reader.Done(); err != nil {
		t.Fatalf("%s: the published welcome has bytes left after encrypted_group_info: %v", at, err)
	}
	return secrets, encryptedGroupInfo
}

// ksWelcomeGroupSecretsJoiner reads joiner_secret out of a decrypted GroupSecrets, and reports
// how many bytes its psks vector carries:
//
//	struct { opaque joiner_secret<V>; optional<PathSecret> path_secret; PreSharedKeyID psks<V>; }
//
// The psks vector is MEASURED rather than decoded. PreSharedKeyID is a discriminated union this
// plan's task 13 owns and this section must not grow one either; all the caller needs to know is
// whether the epoch's psk_secret is the zero secret, and a vector with bytes in it is reported
// rather than assumed empty.
func ksWelcomeGroupSecretsJoiner(t *testing.T, at string, groupSecrets []byte) ([]byte, int) {
	t.Helper()
	reader := syntax.NewReader(groupSecrets)
	joinerSecret, err := reader.ReadOpaque()
	if err != nil {
		t.Fatalf("%s: read joiner_secret out of the decrypted group secrets: %v", at, err)
	}
	if _, err := reader.ReadOptional(func(r *syntax.Reader) error {
		_, pathErr := r.ReadOpaque()
		return pathErr
	}); err != nil {
		t.Fatalf("%s: read the optional path_secret: %v", at, err)
	}
	psks, err := reader.ReadSub()
	if err != nil {
		t.Fatalf("%s: read the psks vector: %v", at, err)
	}
	if err := reader.Done(); err != nil {
		t.Fatalf("%s: the group secrets have bytes left after the psks vector: %v", at, err)
	}
	return joinerSecret, psks.Remaining()
}

// ksFlippedFirstByte is one bit out, in storage of its own so the caller's slice is unmoved.
//
// An empty argument answers one byte rather than indexing into nothing. It is reached from a
// negative control over a key and a nonce a DEFECTIVE derivation produced, so "the derivation
// answered nothing" is one of the inputs it has to survive: a panic here would take the test
// binary down and every gate declared after it would report nothing at all.
func ksFlippedFirstByte(value []byte) []byte {
	if len(value) == 0 {
		return []byte{0x01}
	}
	wrong := bytes.Clone(value)
	wrong[0] ^= 0x01
	return wrong
}

// TestWelcomeKeyNonceOpensThePublishedWelcomes is the known answer this task rests on.
//
// Every other assertion in this section compares the derivation with itself or with this file's
// own reference. This one does not: the pair is used to open an encrypted_group_info that
// mlswg's generator sealed, and an aead tag verifies only if the key and the nonce are exactly
// the bytes that generator derived. A label misspelled, the two labels swapped, either label
// used for both halves, either length wrong by one, the nonce truncated, the key and the nonce
// cut from one expansion -- every one of those moves the key or the nonce, and every one of them
// fails the tag. Nothing this package could compute for itself can say that.
//
// The walk down to welcome_secret is this file's HAND WRITTEN reference rather than the key
// schedule, so a failure here names WelcomeKeyNonce rather than the twelve derivations in front
// of it: joiner_secret is decrypted out of the published Welcome with the published init_priv,
// and member_secret and welcome_secret are the two hand written steps that
// TestTheHandWrittenSectionEightDerivationReproducesThePublishedEpochs already anchors on the
// corpus.
func TestWelcomeKeyNonceOpensThePublishedWelcomes(t *testing.T) {
	vectors := []ksWelcomeVector{}
	loadLabelKat(t, "welcome.json", &vectors)
	opened := 0
	for _, vector := range vectors {
		suite := CipherSuite(vector.CipherSuite)
		if !IsRegisteredSuite(suite) {
			continue
		}
		at := fmt.Sprintf("suite %#04x", uint16(suite))
		crypto := mustProvider(t, suite)
		secrets, encryptedGroupInfo := ksWelcomeMessage(
			t, at, suite, mustDecodeHex(t, "the published welcome "+at, vector.Welcome))

		// the entry this Welcome is addressed to, found by the reference of the published key
		// package rather than by taking the first. A Welcome to two joiners carries two, and
		// opening the wrong one with this init key fails in a way that would read as a defect
		// in the derivation under test.
		message := mustDecodeHex(t, "the published key package "+at, vector.KeyPackage)
		if !bytes.HasPrefix(message, mlsMessageKeyPackageHeader) {
			t.Fatalf("%s: the published key package is not headed with the mls10 key package header %x",
				at, mlsMessageKeyPackageHeader)
		}
		reference := mustKeyPackageRef(t, crypto, message[len(mlsMessageKeyPackageHeader):])
		addressed := -1
		for i, one := range secrets {
			if bytes.Equal(one.newMember, reference) {
				addressed = i
			}
		}
		if addressed < 0 {
			t.Fatalf("%s: none of the %d encrypted group secrets is addressed to the published key package",
				at, len(secrets))
		}

		// EncryptWithLabel(init_key, "Welcome", encrypted_group_info, GroupSecrets), RFC 9420
		// section 12.4.3.1: the context is the ciphertext this test is about to open, which is
		// what binds the two halves of a Welcome together.
		groupSecrets, err := DecryptWithLabel(crypto,
			HpkePrivateKey(mustDecodeHex(t, "init_priv "+at, vector.InitPriv)),
			"Welcome", encryptedGroupInfo, secrets[addressed].kemOutput, secrets[addressed].ciphertext)
		if err != nil {
			t.Fatalf("%s: open the encrypted group secrets with the published init_priv: %v", at, err)
		}
		joinerSecret, pskBytes := ksWelcomeGroupSecretsJoiner(t, at, groupSecrets)
		if pskBytes != 0 {
			t.Fatalf("%s: the published welcome carries a %d byte psks vector, so psk_secret is not the zero secret and this row cannot reach welcome_secret",
				at, pskBytes)
		}
		if len(joinerSecret) != crypto.HashSize() {
			t.Fatalf("%s: the decrypted joiner_secret is %d bytes, want KDF.Nh = %d",
				at, len(joinerSecret), crypto.HashSize())
		}

		// the two hand written steps. psk_secret is KDF.Nh zero bytes because the Welcome names
		// no psks, which the measurement above is what establishes.
		memberSecret := ksHandExtract(joinerSecret, make([]byte, sha256.Size))
		welcomeSecret := ksHandDeriveSecret(t, memberSecret, "welcome")
		key, nonce, err := WelcomeKeyNonce(crypto, welcomeSecret)
		if err != nil {
			t.Fatalf("%s: WelcomeKeyNonce over the welcome secret of a published welcome: %v", at, err)
		}
		groupInfo, err := crypto.AeadOpen(key, nonce, nil, encryptedGroupInfo)
		if err != nil {
			t.Fatalf("%s: the published encrypted_group_info does not open under the derived welcome key and nonce: %v",
				at, err)
		}

		// and what came out is a GroupInfo rather than a nil error taken on trust: its first
		// field is the GroupContext, whose version and cipher suite are both fixed and neither
		// of which this test supplied to the decoder
		context := &GroupContext{}
		if err := context.UnmarshalMLS(syntax.NewReader(groupInfo)); err != nil {
			t.Errorf("%s: the opened group info does not begin with a group context: %v", at, err)
		} else if context.Version != ProtocolVersionMls10 || context.CipherSuite != suite {
			t.Errorf("%s: the opened group info carries version %#04x and suite %#04x",
				at, uint16(context.Version), uint16(context.CipherSuite))
		}

		// the control on the comparison itself. What decides this row is the aead tag, so a key
		// or a nonce one bit out has to be refused; without this, the open above is satisfied by
		// an aead that ignored both of them.
		for _, wrong := range []struct {
			what  string
			key   []byte
			nonce []byte
		}{
			{what: "a welcome key one bit out", key: ksFlippedFirstByte(key), nonce: nonce},
			{what: "a welcome nonce one bit out", key: key, nonce: ksFlippedFirstByte(nonce)},
		} {
			if _, err := crypto.AeadOpen(wrong.key, wrong.nonce, nil, encryptedGroupInfo); err == nil {
				t.Errorf("%s: the published encrypted_group_info opened under %s, so the open above pins nothing",
					at, wrong.what)
			}
		}
		opened++
	}
	if opened != ksWelcomeKatVectors {
		t.Fatalf("opened %d published welcomes, want %d", opened, ksWelcomeKatVectors)
	}
}

// ksWelcomeKatComparisons is two answers -- the key and the nonce -- for every epoch of the
// published key schedule corpus.
const ksWelcomeKatComparisons = 2 * keyScheduleKatEpochs

// TestWelcomeKeyNonceReproducesTheHandWrittenExpansion is the differential, over every
// welcome_secret mlswg published rather than over one this test invented.
//
// The reference shares nothing with the implementation: it spells the "MLS 1.0 " prefix itself,
// writes its own length prefixes and expands with hmac directly, so it can be wrong in a
// different way. What it separates that a self comparison cannot: the two LABELS, since a body
// that expanded both halves under one of them agrees with itself perfectly; and Nk, since the
// two registered suites disagree there -- 16 for the aes suite, 32 for the chacha one -- so a
// written down 32 is a key twice the length the aes suite fixes.
//
// What it cannot separate is Nn, because both suites fix it at 12. That is what
// TestWelcomeKeyNonceReadsBothLengthsOffTheProviderItWasHanded is for.
func TestWelcomeKeyNonceReproducesTheHandWrittenExpansion(t *testing.T) {
	compared := 0
	for _, epoch := range ksVectorEpochs(t) {
		params, err := LookupSuite(epoch.suite)
		if err != nil {
			t.Fatalf("%s: look up the suite the lengths are read from: %v", epoch.at, err)
		}
		welcomeSecret := mustDecodeHex(t, "welcome_secret"+epoch.at, epoch.published.WelcomeSecret)
		// the reference can tell one label from another, at one length so the length is not
		// what separates them. A reference that answered the same under both would agree with
		// an implementation that expanded both halves under one label while looking like an
		// independent opinion.
		if bytes.Equal(ksHandExpandWithLabel(t, welcomeSecret, "key", nil, params.Nn),
			ksHandExpandWithLabel(t, welcomeSecret, "nonce", nil, params.Nn)) {
			t.Fatalf("%s: the hand written expansion answers the same under key and under nonce, so it cannot see the two labels collapsed into one",
				epoch.at)
		}
		wantKey := ksHandExpandWithLabel(t, welcomeSecret, "key", nil, params.Nk)
		wantNonce := ksHandExpandWithLabel(t, welcomeSecret, "nonce", nil, params.Nn)
		key, nonce, err := WelcomeKeyNonce(epoch.crypto, welcomeSecret)
		if err != nil {
			t.Fatalf("%s: WelcomeKeyNonce over the published welcome_secret: %v", epoch.at, err)
		}
		if !bytes.Equal(key, wantKey) {
			t.Errorf("%s: welcome_key = %x, want %x", epoch.at, key, wantKey)
		}
		if !bytes.Equal(nonce, wantNonce) {
			t.Errorf("%s: welcome_nonce = %x, want %x", epoch.at, nonce, wantNonce)
		}
		compared += 2
	}
	if compared != ksWelcomeKatComparisons {
		t.Fatalf("compared %d hand written answers, want %d", compared, ksWelcomeKatComparisons)
	}
}

// ksWelcomeSyntheticParams is a suite whose Nk and Nn are numbers no registered suite has.
//
// Both registered suites fix Nn at 12, and one of them fixes Nk at 32 -- which is also KDF.Nh and
// also the literal a body would have written down. So inside this registry a written down 12 and
// a read of NonceSize() are the same number, and nothing already in this tree separates them.
// Measured, not supposed: with crypto.NonceSize() replaced by 12 in WelcomeKeyNonce, every other
// test of this package passed. This is the input that separates them, and it is the same device
// labelKatSyntheticParams is, one field over.
//
// Nh is moved as well so the same provider also separates a written down 32 in the length check.
// The kdf underneath is still sha256, which is incoherent with an Nh of 48 -- that is deliberate
// and harmless here, because what this provider is asked for is LENGTHS and never a value
// compared against a published one.
var ksWelcomeSyntheticParams = SuiteParams{
	Suite:       CipherSuite(0xfffd),
	Name:        "synthetic_nk20_nn7",
	KemId:       HpkeKemX25519HkdfSha256,
	KdfId:       HpkeKdfHkdfSha256,
	AeadId:      HpkeAeadChaCha20Poly1305,
	SignatureId: SignatureSchemeEd25519,
	Nh:          48,
	Nk:          20,
	Nn:          7,
	Nt:          16,
	Nsecret:     17,
	Nenc:        18,
	Npk:         19,
	Nsk:         21,
	NsigPub:     22,
	NsigPriv:    23,
}

// TestWelcomeKeyNonceReadsBothLengthsOffTheProviderItWasHanded is the differential the registry
// cannot supply, for the two lengths this construction reads and for the one it refuses against.
func TestWelcomeKeyNonceReadsBothLengthsOffTheProviderItWasHanded(t *testing.T) {
	crypto := &suiteCryptoProvider{params: &ksWelcomeSyntheticParams, random: constantReader{value: 0x40}}
	// the substitutions this provider has to be able to see. A length here that coincided with
	// Nk or Nn would leave every assertion below satisfied by the very literal it exists to
	// catch, which is how a differential goes quiet without failing.
	for _, other := range []struct {
		name  string
		value int
	}{
		{name: "this suite's Nh", value: ksWelcomeSyntheticParams.Nh},
		{name: "this suite's Nt", value: ksWelcomeSyntheticParams.Nt},
		{name: "the aes suite's Nk", value: 16},
		{name: "the chacha suite's Nk", value: 32},
		{name: "the registry's Nn", value: 12},
		{name: "the registry's KDF.Nh", value: sha256.Size},
	} {
		if other.value == ksWelcomeSyntheticParams.Nk {
			t.Errorf("%s is %d, the same as this suite's Nk, so a body writing it down would pass here",
				other.name, other.value)
		}
		if other.value == ksWelcomeSyntheticParams.Nn {
			t.Errorf("%s is %d, the same as this suite's Nn, so a body writing it down would pass here",
				other.name, other.value)
		}
	}
	if ksWelcomeSyntheticParams.Nk == ksWelcomeSyntheticParams.Nn {
		t.Fatalf("this suite's Nk and Nn are both %d, so the two assertions below are one",
			ksWelcomeSyntheticParams.Nk)
	}

	secret := bytes.Repeat([]byte{0x5b}, ksWelcomeSyntheticParams.Nh)
	key, nonce, err := WelcomeKeyNonce(crypto, secret)
	if err != nil {
		t.Fatalf("WelcomeKeyNonce over a suite whose KDF.Nh is %d: %v", ksWelcomeSyntheticParams.Nh, err)
	}
	if len(key) != ksWelcomeSyntheticParams.Nk {
		t.Errorf("welcome_key is %d bytes for a suite whose Nk is %d",
			len(key), ksWelcomeSyntheticParams.Nk)
	}
	if len(nonce) != ksWelcomeSyntheticParams.Nn {
		t.Errorf("welcome_nonce is %d bytes for a suite whose Nn is %d",
			len(nonce), ksWelcomeSyntheticParams.Nn)
	}

	// the third length: the secret is measured against the provider's KDF.Nh and not against 32
	if _, _, err := WelcomeKeyNonce(crypto, bytes.Repeat([]byte{0x5b}, sha256.Size)); !errors.Is(err, ErrSecretLength) {
		t.Errorf("a %d byte secret was accepted by a suite whose KDF.Nh is %d: err = %v",
			sha256.Size, ksWelcomeSyntheticParams.Nh, err)
	}

	// and the same defect one level down, which the two length assertions above cannot see: a
	// body that asked the kdf for a written down length and TRUNCATED to the provider's answers
	// Nk and Nn bytes either way. ExpandWithLabel binds the requested length into its own
	// preimage, so the truncation is a different value and this is what separates them.
	// each comparison is skipped when the answer is LONGER than the written down expansion it
	// would have been cut from, because there is then nothing to cut and the length assertions
	// above have already reported it. Indexing anyway would panic, and a panic in a test takes
	// the whole binary down along with every gate declared after it -- which is the shape
	// recoveringRow exists for elsewhere in this file.
	if truncated := crypto.ExpandWithLabel(secret, "key", nil, 32); len(key) <= len(truncated) &&
		bytes.Equal(key, truncated[:len(key)]) {
		t.Errorf("welcome_key is the first %d bytes of a 32 byte expansion rather than an expansion of %d",
			len(key), ksWelcomeSyntheticParams.Nk)
	}
	if truncated := crypto.ExpandWithLabel(secret, "nonce", nil, 12); len(nonce) <= len(truncated) &&
		bytes.Equal(nonce, truncated[:len(nonce)]) {
		t.Errorf("welcome_nonce is the first %d bytes of a 12 byte expansion rather than an expansion of %d",
			len(nonce), ksWelcomeSyntheticParams.Nn)
	}
}

// ksSharesStorage answers whether two byte slices overlap anywhere in their backing arrays.
//
// Written as a WRITE rather than as a pointer comparison, because the pointer comparison this
// package can write without unsafe -- &first[0] == &second[0] -- sees only a shared FIRST byte,
// and the shape that matters for a key and a nonce cut from one expansion is two windows that
// overlap at an end. Complementing every byte of the first slice makes the observation exact: an
// overlapping byte necessarily changes and a byte that is not overlapped necessarily does not,
// whatever either of them held. The first slice is restored before returning.
func ksSharesStorage(first []byte, second []byte) bool {
	before := bytes.Clone(second)
	for i := range first {
		first[i] ^= 0xff
	}
	shared := !bytes.Equal(second, before)
	for i := range first {
		first[i] ^= 0xff
	}
	return shared
}

// TestWelcomeKeyNonceAnswersTwoArraysThatDoNotOverlap is the aliasing half.
//
// Deriving both halves from one expansion and slicing one backing array is the natural
// implementation and is fine; handing the caller two HEADERS over those bytes is not. The
// difference is invisible everywhere else -- the values are right, the lengths are right, the
// corpus opens -- and it surfaces the first time a caller erases its welcome key, which zeroes
// the nonce it is about to seal the next Welcome under.
func TestWelcomeKeyNonceAnswersTwoArraysThatDoNotOverlap(t *testing.T) {
	// the detector first, on a pair that really does overlap and on a pair that really does
	// not. A detector that cannot fire reports two independent arrays exactly as it reports two
	// views over one, which is the whole finding this test exists to make.
	backing := bytes.Repeat([]byte{0x11}, 40)
	if !ksSharesStorage(backing[:32], backing[28:40]) {
		t.Fatal("the overlap detector reported two headers over one array as separate storage, so every row below passes for an implementation that aliases")
	}
	if ksSharesStorage(bytes.Repeat([]byte{0x11}, 32), bytes.Repeat([]byte{0x11}, 12)) {
		t.Fatal("the overlap detector reported two separate arrays as shared storage")
	}
	for _, one := range backing {
		if one != 0x11 {
			t.Fatal("the overlap detector did not restore the slice it wrote into")
		}
	}

	for _, suite := range Suites() {
		crypto := mustProvider(t, suite)
		at := fmt.Sprintf("suite %#04x", uint16(suite))
		key, nonce, err := WelcomeKeyNonce(crypto, crypto.Random(crypto.HashSize()))
		if err != nil {
			t.Fatalf("%s: WelcomeKeyNonce: %v", at, err)
		}
		if len(key) == 0 || len(nonce) == 0 {
			t.Fatalf("%s: welcome_key is %d bytes and welcome_nonce is %d, so this row observed nothing",
				at, len(key), len(nonce))
		}
		if ksSharesStorage(key, nonce) {
			t.Errorf("%s: welcome_key and welcome_nonce are two headers over one array; erasing the key erases the nonce",
				at)
		}
	}
}

// TestWelcomeKeyNonceMovesBothHalvesWithTheSecret asserts a different welcome_secret gives a
// different key AND a different nonce.
//
// Both, not either. A body whose nonce did not depend on the secret it was handed -- a package
// level constant, an expansion of the label alone, a second read of a variable that never moved
// -- answers a key that moves and a nonce that does not, and an assertion that reported "the pair
// changed" would pass over it. That is one nonce for every group in the world, under a key that
// is different per group, which is exactly the reuse this section exists for.
//
// One bit at a time rather than a fresh random secret, so a body that reads only part of the
// secret is caught wherever the part it ignores happens to be.
func TestWelcomeKeyNonceMovesBothHalvesWithTheSecret(t *testing.T) {
	for _, suite := range Suites() {
		crypto := mustProvider(t, suite)
		at := fmt.Sprintf("suite %#04x", uint16(suite))
		base := make([]byte, crypto.HashSize())
		baseKey, baseNonce, err := WelcomeKeyNonce(crypto, base)
		if err != nil {
			t.Fatalf("%s: WelcomeKeyNonce over the all zero secret: %v", at, err)
		}
		moved := 0
		for bit := 0; bit < 8*len(base); bit += 7 {
			changed := bytes.Clone(base)
			changed[bit/8] ^= 1 << (bit % 8)
			key, nonce, err := WelcomeKeyNonce(crypto, changed)
			if err != nil {
				t.Fatalf("%s: WelcomeKeyNonce with bit %d of welcome_secret set: %v", at, bit, err)
			}
			if bytes.Equal(key, baseKey) {
				t.Errorf("%s: welcome_key did not move when bit %d of welcome_secret did", at, bit)
			}
			if bytes.Equal(nonce, baseNonce) {
				t.Errorf("%s: welcome_nonce did not move when bit %d of welcome_secret did", at, bit)
			}
			moved++
		}
		// a sweep whose inner loop stopped producing rows reports the clean run a full one
		// reports
		if moved == 0 {
			t.Fatalf("%s: no bit of welcome_secret was moved, so this row compared nothing", at)
		}
	}
}

// TestWelcomeKeyNonceRefusesASecretThatIsNotKdfNh is the refusal the group creation path needs.
//
// HKDF-Expand takes a pseudorandom key of any length at or above the hash and answers a perfectly
// well formed key and nonce, so nothing downstream of a short welcome_secret can see that it was
// short. The caller this exists for is a group being created: WelcomeSecret() is nil there
// because the group was never joined, and a nil stretched into a Welcome key seals
// encrypted_group_info under an expansion every party can recompute, with err == nil.
func TestWelcomeKeyNonceRefusesASecretThatIsNotKdfNh(t *testing.T) {
	for _, suite := range Suites() {
		crypto := mustProvider(t, suite)
		nh := crypto.HashSize()
		at := fmt.Sprintf("suite %#04x", uint16(suite))
		for _, length := range []int{0, 1, nh / 2, nh - 1, nh + 1, 2 * nh, 255} {
			if length == nh {
				t.Fatalf("%s: the refusal sweep includes KDF.Nh itself, which is the one length that must be accepted", at)
			}
			key, nonce, err := WelcomeKeyNonce(crypto, bytes.Repeat([]byte{0x2c}, length))
			if !errors.Is(err, ErrSecretLength) {
				t.Errorf("%s: a %d byte welcome secret answered err = %v, want %v", at, length, err, ErrSecretLength)
			}
			if key != nil || nonce != nil {
				t.Errorf("%s: a %d byte welcome secret was refused and answered a %d byte key and a %d byte nonce alongside the refusal",
					at, length, len(key), len(nonce))
			}
		}
		// nil is the shape the creation path produces, and it is the one this refusal is for
		key, nonce, err := WelcomeKeyNonce(crypto, nil)
		if !errors.Is(err, ErrSecretLength) {
			t.Errorf("%s: a nil welcome secret answered err = %v, want %v", at, err, ErrSecretLength)
		}
		if key != nil || nonce != nil {
			t.Errorf("%s: a nil welcome secret answered a %d byte key and a %d byte nonce", at, len(key), len(nonce))
		}
		// and the control: KDF.Nh is accepted, so the sweep above is not satisfied by a body
		// that refuses everything
		if _, _, err := WelcomeKeyNonce(crypto, bytes.Repeat([]byte{0x2c}, nh)); err != nil {
			t.Errorf("%s: a KDF.Nh welcome secret was refused with %v, so every refusal above is vacuous", at, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Task 12: Zeroize, and the shape of G6 the three gates above leave open
// ---------------------------------------------------------------------------
//
// The plan's own task 12 supplies TestKeyScheduleDoesNotExportEpochSecret, which reflects over
// KeySchedule for an exported field, checks the exported method names against a list written
// beside it, and checks that EpochSecrets declares no field spelled EpochSecret. Every one of
// those three is already covered here and covered harder, so it is not copied in:
//
//   - the exported field check is bytesTheScheduleHandsOut's own first act, and it is fatal
//     there;
//   - an allowlist of method names is standing rule 5's mistake written out. The three gates
//     above sweep the methods that exist and compare what each ANSWERS against an independent
//     statement of epoch_secret, so a method added later is read rather than merely noticed,
//     and one that leaks under a single label -- which is what a signature cannot show -- is
//     caught by the source reachability gate;
//   - "no field named EpochSecret" is a NAME. The sweep compares the bytes of every field of
//     EpochSecrets against the real epoch secret, so a field carrying it under any other
//     spelling fails, which is the property the name was standing in for.
//
// What the three do NOT cover is the shape they each, for their own reason, have to let past:
// a declaration that reaches the parent secret and puts it somewhere the call does not end.
// TestNoExportedMethodOfThisPackageCanReachTheEpochSecret says so in its own comment -- an
// eraser reaches every secret the epoch holds and is exempt because it "answers nothing and is
// handed nothing" -- and until Zeroize landed, that exemption covered nothing at all. It now
// covers a method, so the exemption has to be worth what it claims: the two gates below are
// what makes it so.

// zeroizeSourceControl declares one of each shape the byte slice class has to tell apart: a
// field the erase reaches, a field it does not, a byte slice one struct hop away that it does
// reach, and storage that is not a byte slice at all.
//
// Without it a derivation that had stopped deriving -- a parse that read no structs, a walk
// that never followed the nested type -- would report an empty class, the gate reading it
// would demand nothing of Zeroize, and the run would look exactly like the run of a complete
// one. The forgotten field is the load bearing row: a class that reported only what the erase
// already touches is a class that can never fail.
const zeroizeSourceControl = "package control\n" +
	"\n" +
	"type Inner struct {\n" +
	"\tnested []byte\n" +
	"}\n" +
	"\n" +
	"type Holder struct {\n" +
	"\tprovider  int\n" +
	"\terased    []byte\n" +
	"\tforgotten []byte\n" +
	"\tinner     Inner\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) Zeroize() {\n" +
	"\twipe(self.erased)\n" +
	"\twipe(self.inner.nested)\n" +
	"}\n" +
	"\n" +
	"func wipe(secret []byte) {\n" +
	"\tfor i := range secret {\n" +
	"\t\tsecret[i] = 0\n" +
	"\t}\n" +
	"}\n"

// theByteSlicesHeldBy is every byte slice a named struct type reaches: the ones it declares
// itself, and the ones declared by every named struct type it holds a field of.
//
// The walk is what makes this the class rather than a copy of it. The nine live one hop away
// inside EpochSecrets, so a scan that read KeySchedule's own fields would report three secrets
// out of twelve and pass Zeroize with the nine untouched -- and the batch B report's point
// about this plan's three nine row tables is that a tenth secret has to drop out of the class
// by ARRIVING, not by somebody remembering to add a row for it.
//
// A named byte slice type counts as a byte slice, for the reason exposedByteSlices reads kinds
// rather than spellings: HpkePrivateKey is the same array to the compiler, and a secret held
// under one would be exempted by a comparison against the literal []byte alone.
func theByteSlicesHeldBy(structs map[string]*ast.StructType, root string, named []string) []string {
	held := []string{}
	seen := map[string]bool{}
	var walk func(name string)
	walk = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		structure, isDeclared := structs[name]
		if !isDeclared {
			return
		}
		for _, field := range structure.Fields.List {
			isByteSlice := false
			if slice, isSlice := field.Type.(*ast.ArrayType); isSlice && slice.Len == nil {
				if element, isIdentifier := slice.Elt.(*ast.Ident); isIdentifier && element.Name == "byte" {
					isByteSlice = true
				}
			}
			if identifier, isIdentifier := field.Type.(*ast.Ident); isIdentifier && slices.Contains(named, identifier.Name) {
				isByteSlice = true
			}
			if isByteSlice {
				for _, declared := range field.Names {
					held = append(held, declared.Name)
				}
			}
			for _, mentioned := range identifiersNamedIn(field.Type) {
				walk(mentioned)
			}
		}
	}
	walk(root)
	slices.Sort(held)
	return slices.Compact(held)
}

// theByteSlicesErasedBy is the field names one declaration hands to one of this package's erase
// helpers.
//
// It reads the ARGUMENT of a call to a helper rather than any mention of the field, which is
// the difference between an erase and a read. A body that merely named every secret -- a length
// check over each, a log line -- would satisfy a mention count while erasing nothing, and that
// is precisely the shape "make Zeroize a no-op" takes once the calls are gone.
//
// The helpers are derived by eraseHelperClass rather than named here, so an erase spelled through
// a second helper counts -- and that second helper is held to the noinline directive by
// TestEveryEraseHelperCarriesTheNoInlineDirective, which is what makes delegating to one an
// erase at all.
func theByteSlicesErasedBy(parsed parsedSource, function *ast.FuncDecl, helpers []string) []string {
	erased := []string{}
	if function == nil || function.Body == nil {
		return erased
	}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		if !slices.Contains(helpers, parsed.render(call.Fun)) {
			return true
		}
		for _, argument := range call.Args {
			if selector, isSelector := argument.(*ast.SelectorExpr); isSelector {
				erased = append(erased, selector.Sel.Name)
			}
		}
		return true
	})
	slices.Sort(erased)
	return slices.Compact(erased)
}

// methodByName is the parsed declaration of one METHOD of one named type, with the file it was
// read out of, so a scan can render its expressions in the right token.FileSet.
//
// THE RECEIVER IS PART OF THE LOOKUP, and the bare-name version this replaces was doing real
// work by accident: while (*KeySchedule).Zeroize was the only Zeroize in this package, "the
// declaration named Zeroize" and "the key schedule's erase" were the same thing. They stopped
// being the same the moment a second type declared an erase -- (*StagedCommit).Zeroize sorts
// ahead of it by file name -- and what the gate below then read was another type's body,
// reported as the key schedule erasing none of its own secrets. A reading keyed on a name a
// second declaration can carry is the same defect as a gate that names a position, and the
// repair is the same: ask for the receiver.
func methodByName(files []parsedSource, receiver string, name string) (parsedSource, *ast.FuncDecl) {
	for _, parsed := range files {
		for _, declaration := range parsed.file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Name.Name != name {
				continue
			}
			if function.Recv == nil || len(function.Recv.List) != 1 {
				continue
			}
			if receiverTypeName(function.Recv.List[0].Type) == receiver {
				return parsed, function
			}
		}
	}
	return parsedSource{}, nil
}

// scheduleByteSlicesThatAreNotSecrets is the storage of the key schedule that Zeroize is not
// required to erase, with the reason written out for each.
//
// The CLASS is derived off the type's own source and this table is checked against it in both
// directions, so a field cannot fall outside the erase by being forgotten and a row cannot
// outlive the field it excuses. That is the shape standing rule 5 asks for: derive the members,
// and make every exception carry a sentence somebody had to write.
var scheduleByteSlicesThatAreNotSecrets = map[string]string{
	"groupContextBytes": "the serialized GroupContext is not a secret. Every member of the group holds it, " +
		"it is what framing signs and MACs over, and it is public the moment a Welcome is sent, so erasing " +
		"it would take away nothing an attacker lacks while leaving GroupContextBytes() answering a run of " +
		"zeros that a caller could frame under",
}

// TestZeroizeErasesEveryByteSliceThisTypeDeclares is the completeness half of the erase, read
// off the source because the behavioural half cannot see all of it.
//
// epoch_secret is unreachable through every exported symbol of this package -- that is G6, and
// three gates above hold it -- so no test can observe whether Zeroize erased it. That leaves
// exactly one way to know, which is to read the erase. And the same reading is what turns "a
// tenth secret is added to EpochSecrets" from a silent omission into a failure: the class is
// walked off the struct declarations, so the tenth joins it by arriving and Zeroize fails until
// it is erased.
func TestZeroizeErasesEveryByteSliceThisTypeDeclares(t *testing.T) {
	// the control first: the walk crosses a struct hop, the erase reading tells a call from a
	// mention, and the difference between the two is the field nobody erased
	control := mustParseText(t, "the zeroize source control", zeroizeSourceControl)
	controlStructs := map[string]*ast.StructType{}
	structTypesIn(control, controlStructs)
	controlHeld := theByteSlicesHeldBy(controlStructs, "Holder", []string{})
	if want := []string{"erased", "forgotten", "nested"}; !slices.Equal(controlHeld, want) {
		t.Fatalf("the byte slice walk read %v out of the control, want %v; it is not reading the type's own fields or not following the struct it holds",
			controlHeld, want)
	}
	_, controlZeroize := methodByName([]parsedSource{control}, "Holder", "Zeroize")
	if controlZeroize == nil {
		t.Fatal("the control declares no (*Holder).Zeroize, so the erase reading below is over nothing")
	}
	controlErased := theByteSlicesErasedBy(control, controlZeroize, []string{"wipe"})
	if want := []string{"erased", "nested"}; !slices.Equal(controlErased, want) {
		t.Fatalf("the erase reading read %v out of the control, want %v; it is not reading the argument of a call to an erase helper",
			controlErased, want)
	}

	// then this package's own source
	files := []parsedSource{}
	structs := map[string]*ast.StructType{}
	candidates := []eraseHelperCandidate{}
	named := packageByteSliceTypeNames(t)
	for _, path := range packageLevelFunctions(t).files {
		parsed := mustParseSource(t, path)
		files = append(files, parsed)
		structTypesIn(parsed, structs)
		candidates = append(candidates, eraseHelperCandidatesIn(mustReadCommented(t, path), path, named)...)
	}
	helpers, _ := eraseHelperClass(candidates)
	if len(helpers) == 0 {
		t.Fatal("this package's source declares no erase helper, so the reading below finds no erase however Zeroize is written")
	}
	holders := theTypesDeclaringTheEpochSecret(structs)
	if len(holders) != 1 {
		t.Fatalf("this package's source has %v DECLARING the epoch secret storage and this gate reads one declarer; a type that merely POINTS at one is the transitive reading and belongs to guardrail 6's surface gates rather than to this one",
			holders)
	}
	held := theByteSlicesHeldBy(structs, holders[0], named)
	// the positive control on the real source. The nine live one struct hop away, so a walk
	// that had stopped crossing that hop reports three and passes an erase that touches none
	// of them -- which is the clean run a complete walk also reports.
	if len(held) < reflect.TypeOf(EpochSecrets{}).NumField()+3 {
		t.Fatalf("the walk read %d byte slices off %s and the type reaches %d fields of EpochSecrets alone, so it is not reading what it claims to",
			len(held), holders[0], reflect.TypeOf(EpochSecrets{}).NumField())
	}

	// the erase OF THE TYPE THAT DECLARES THE STORAGE, and not whichever declaration named
	// Zeroize a directory walk reached first. This package now declares four of them -- the key
	// schedule, the secret tree, the private tree state and the staged commit -- and the bare
	// name answers the wrong one.
	parsed, zeroize := methodByName(files, holders[0], "Zeroize")
	if zeroize == nil {
		t.Fatalf("%s declares no Zeroize, and an epoch that leaves PastEpochWindow has to have one",
			holders[0])
	}
	erased := theByteSlicesErasedBy(parsed, zeroize, helpers)
	t.Logf("%s reaches %d byte slices %v; Zeroize erases %v through %v", holders[0], len(held), held, erased, helpers)

	for _, field := range held {
		if slices.Contains(erased, field) {
			if reason, excused := scheduleByteSlicesThatAreNotSecrets[field]; excused {
				t.Errorf("Zeroize erases %s and scheduleByteSlicesThatAreNotSecrets excuses it as %q; one of the two is wrong and which one holds cannot be read off either",
					field, reason)
			}
			continue
		}
		if _, excused := scheduleByteSlicesThatAreNotSecrets[field]; excused {
			continue
		}
		t.Errorf("%s reaches byte slice %s and Zeroize hands it to none of %v; an epoch that leaves PastEpochWindow with a secret still in it is the whole of what this erase is for, and a secret that is not a secret needs a row in scheduleByteSlicesThatAreNotSecrets saying why",
			holders[0], field, helpers)
	}
	for field := range scheduleByteSlicesThatAreNotSecrets {
		if !slices.Contains(held, field) {
			t.Errorf("scheduleByteSlicesThatAreNotSecrets excuses %s, which is not a byte slice %s reaches; a row that outlived its field excuses nothing and hides that it does",
				field, holders[0])
		}
	}
}

// TestZeroizeLeavesNothingReadableOfWhatTheScheduleHeld is the behavioural half: every secret a
// live epoch hands out reads as zeros afterwards, through the same slice the caller was given.
//
// The field set is read off EpochSecrets by reflection rather than written out, for the reason
// epochSecretsByField exists at all: this plan supplies three separate nine row tables, and a
// tenth secret or a renamed field drops out of all three at once while every one of them goes
// on reporting the clean run a complete sweep reports.
//
// Two controls, and each is the difference between observing the erase and observing nothing.
// Every secret is read before the call and required to be non zero, or "all zero afterwards"
// would be satisfied by a secret that was already zero; and the erase is required to reach the
// slice the CALLER holds rather than some copy, which is the property (*KeySchedule).Secrets'
// own comment rests on and the one a schedule that erased a clone of its storage would fail
// while every length and every value check still passed.
func TestZeroizeLeavesNothingReadableOfWhatTheScheduleHeld(t *testing.T) {
	for _, epoch := range ksVectorEpochs(t) {
		schedule := epoch.schedule(t)
		held := map[string][]byte{
			"joiner_secret":  schedule.JoinerSecret(),
			"welcome_secret": schedule.WelcomeSecret(),
		}
		for name, secret := range epochSecretsByField(t, schedule.Secrets()) {
			held["EpochSecrets."+name] = secret
		}
		if len(held) != reflect.TypeOf(EpochSecrets{}).NumField()+2 {
			t.Fatalf("%s: this sweep collected %d secrets and the epoch holds %d reachable ones",
				epoch.at, len(held), reflect.TypeOf(EpochSecrets{}).NumField()+2)
		}
		// the live control: each of them is KDF.Nh bytes and none of them is already zero, so
		// an all zero reading afterwards is the erase and not the state they started in
		for name, secret := range held {
			if len(secret) != epoch.crypto.HashSize() {
				t.Fatalf("%s: %s is %d bytes before the erase and KDF.Nh is %d, so it is not a secret this reading covers",
					epoch.at, name, len(secret), epoch.crypto.HashSize())
			}
			if !slices.ContainsFunc(secret, func(b byte) bool { return b != 0 }) {
				t.Fatalf("%s: %s is already %d zero bytes before Zeroize, so an all zero reading after it would say nothing",
					epoch.at, name, len(secret))
			}
		}

		schedule.Zeroize()

		for name, secret := range held {
			for i, b := range secret {
				if b != 0 {
					t.Errorf("%s: byte %d of %s is %#02x after Zeroize, want 0; the slice a caller was handed is a view into the epoch's own array and the erase has to reach it there",
						epoch.at, i, name, b)
					break
				}
			}
		}

		// and a second call is a no-op rather than a fault. An epoch may be dropped from the
		// window by one path and released by another, and a Zeroize that panicked the second
		// time would turn an ordinary double release into a crash on the message path.
		schedule.Zeroize()
	}
}

// scheduleMethodsThatStillAnswerAfterAnErase is every exported method of *KeySchedule that goes
// on answering over an erased epoch, with the reason written out for each.
//
// The class is the type's exported surface read by reflection and this table is checked against
// it in both directions, so a method cannot go on answering an erased epoch by being forgotten,
// and a row cannot outlive the method it excuses.
var scheduleMethodsThatStillAnswerAfterAnErase = map[string]string{
	"GroupContextBytes": "the serialized GroupContext is not a secret and Zeroize does not erase it, " +
		"for the reason scheduleByteSlicesThatAreNotSecrets gives",
	"Secrets": "this is the pointer the erase is performed THROUGH -- the type comment says an aged out " +
		"epoch is zeroized in place and Secrets() is what makes that reachable -- so it cannot refuse on " +
		"the state it exists to create. What it answers afterwards is nine runs of zeros, and every method " +
		"of this type that DERIVES from one of them refuses instead: see " +
		"TestEveryMethodDerivingFromTheEpochsSecretsRefusesAnErasedEpoch",
}

// TestAnErasedScheduleRefusesRatherThanAnsweringFromZeros is what the schedule DOES after
// Zeroize, over the whole exported surface rather than over the six methods that read one of
// the nine by name.
//
// TestEveryMethodDerivingFromTheEpochsSecretsRefusesAnErasedEpoch already covers those six, and
// it erases the nine by hand through Secrets(). This one differs in the two ways that matter.
// It erases through the production call, so the refusals are held to the erase the Group will
// actually perform rather than to a test's imitation of it. And its class is every exported
// method, which is what reaches the two secrets that are NOT among the nine: joiner_secret and
// welcome_secret are the schedule's own storage and no method reads them out of the nine, so
// they sit outside that gate's derivation entirely.
//
// Measured, not supposed. With the liveness checks removed from JoinerSecret and WelcomeSecret,
// the whole of mls and message was green while WelcomeSecret() answered KDF.Nh zero bytes --
// which is the length WelcomeKeyNonce requires, so a creator holding an aged out epoch seals
// encrypted_group_info under an expansion of a secret every party can compute, and
// NewKeyScheduleFromJoiner rebuilds the entire epoch from the other one. Neither has an epoch to
// ask secretIsLive, because both are package level functions handed bare bytes.
func TestAnErasedScheduleRefusesRatherThanAnsweringFromZeros(t *testing.T) {
	scheduleType := reflect.TypeOf(&KeySchedule{})
	class := []string{}
	erasers := []string{}
	for i := range scheduleType.NumMethod() {
		method := scheduleType.Method(i)
		// an eraser answers nothing over a live epoch either, so there is no refusal for it
		// to make and nothing for the live control to observe. Told apart by the shape, which
		// is the same rule hasSomewhereToPutASecret states for the source.
		if method.Type.NumOut() == 0 {
			erasers = append(erasers, method.Name)
			continue
		}
		class = append(class, method.Name)
	}
	if len(class) < 4 {
		t.Fatalf("this gate reads %d exported methods of *KeySchedule (%v) and the type declares four accessors at least, so it is not reading what it claims to",
			len(class), class)
	}
	// and the COMPLEMENT is printed. An arity-shaped exclusion whose removed members nobody
	// names is the shape GATES.md indexes this line for, and empty here would mean the filter
	// matched none of the erasers this type declares -- which is a class one member too wide
	// reporting a clean bill over a method that has no refusal to make.
	if len(erasers) == 0 {
		t.Fatal("no exported method of *KeySchedule answers nothing, and Zeroize is one; this exclusion removed nothing, so it is not reading the type it claims to")
	}
	t.Logf("the class this gate holds to a refusal is %v; the %d methods it removes, because they answer nothing at all and so have no refusal to observe, are %v",
		class, len(erasers), erasers)
	for name := range scheduleMethodsThatStillAnswerAfterAnErase {
		if !slices.Contains(class, name) {
			t.Errorf("scheduleMethodsThatStillAnswerAfterAnErase excuses %s, which is not an exported method of *KeySchedule that answers anything",
				name)
		}
	}

	for _, epoch := range ksVectorEpochs(t) {
		schedule := epoch.schedule(t)
		// the live control first: every one of them hands something back before the erase, so
		// a refusal afterwards is the erase rather than a method that refuses everything
		for _, name := range class {
			for index, answer := range scheduleMethodResults(t, epoch.at, schedule, name) {
				if !answer.handedSomethingBack() {
					t.Fatalf("%s: %s row %d answered nothing over a LIVE epoch -- no bytes and no acceptance -- so a refusal after Zeroize would say nothing",
						epoch.at, name, index)
				}
			}
		}

		schedule.Zeroize()

		for _, name := range class {
			reason, excused := scheduleMethodsThatStillAnswerAfterAnErase[name]
			answered := false
			for index, answer := range scheduleMethodResults(t, epoch.at, schedule, name) {
				if excused {
					if answer.handedSomethingBack() {
						answered = true
					}
					continue
				}
				for _, secret := range answer.read {
					if len(secret) != 0 {
						t.Errorf("%s: %s row %d answered %d bytes over an epoch Zeroize has erased; every derivation left is over KDF.Nh zero bytes, which any party can compute with no knowledge of the group, so this is a publicly computable value handed back with err == nil",
							epoch.at, name, index, len(secret))
					}
				}
				for _, accepted := range answer.accepted {
					if accepted {
						t.Errorf("%s: %s row %d ACCEPTED over an epoch Zeroize has erased; the key it verified under is KDF.Nh zero bytes, so what it accepted is a tag anybody could have forged",
							epoch.at, name, index)
					}
				}
			}
			// an excuse that stopped being true is a row that hides a refusal nobody asked
			// for, so it is checked rather than trusted
			if excused && !answered {
				t.Errorf("%s: scheduleMethodsThatStillAnswerAfterAnErase excuses %s as %q and it answered nothing over an erased epoch, so the row excuses a refusal",
					epoch.at, name, reason)
			}
		}
	}
}

// epochSecretEscapeControl declares one of each shape the escape scan has to tell apart.
//
// Eight ways out, and every one of them is the SAME leak written differently: the secret put
// into an exported package level variable and into an unexported one, sent down a channel,
// copied into package level storage by the copy builtin rather than by an assignment, handed
// to a callback the caller supplied, to one held in a package level variable, to one held in a
// field of the holder itself, and handed to a goroutine that outlives the call that started
// it. The first version of this scan matched three of the eight by shape and the other five
// walked past it.
//
// And four legitimate shapes, which are what stop this from being a scan that objects to
// Zeroize: handing the storage to a function this package declares, cutting a local from it,
// writing it back into the receiver's own storage, and refusing with a sentinel error. The
// last of those is the nearest legitimate shape to the copy row -- it mentions a package level
// value and hands it to an imported function -- and it is here because a scan that read it as
// an escape would fail on this package's own constructors on the first run and be weakened
// until it read nothing.
const epochSecretEscapeControl = "package control\n" +
	"\n" +
	"import (\n" +
	"\t\"bytes\"\n" +
	"\t\"errors\"\n" +
	"\t\"fmt\"\n" +
	"\t\"sync\"\n" +
	")\n" +
	"\n" +
	"var ErrControl = errors.New(\"control\")\n" +
	"\n" +
	"type Holder struct {\n" +
	"\tepochSecret []byte\n" +
	"\tkept        []byte\n" +
	"\tobserver    func([]byte)\n" +
	"\tguard       sync.Mutex\n" +
	"}\n" +
	"\n" +
	"type BufferedHolder struct {\n" +
	"\tepochSecret bytes.Buffer\n" +
	"}\n" +
	"\n" +
	"type TableHolder struct {\n" +
	"\tepochSecret map[string][]byte\n" +
	"}\n" +
	"\n" +
	"var Escaped []byte\n" +
	"\n" +
	"var escapedUnexported []byte\n" +
	"\n" +
	"var escapeChannel = make(chan []byte, 1)\n" +
	"\n" +
	"var stashed = make([]byte, 32)\n" +
	"\n" +
	"var packageObserver func([]byte)\n" +
	"\n" +
	"var registerControl func(func() []byte)\n" +
	"\n" +
	"var theControlSink = &bytes.Buffer{}\n" +
	"\n" +
	"func (self *Holder) LeaksIntoAnExportedVariable() {\n" +
	"\tEscaped = self.epochSecret\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) LeaksIntoAnUnexportedVariable() {\n" +
	"\tescapedUnexported = self.epochSecret\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) LeaksIntoAChannel() {\n" +
	"\tescapeChannel <- self.epochSecret\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) LeaksByCopyingIntoPackageStorage() {\n" +
	"\tcopy(stashed, self.epochSecret)\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) LeaksThroughACallerCallback(hand func([]byte)) {\n" +
	"\thand(self.epochSecret)\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) LeaksThroughAPackageCallback() {\n" +
	"\tpackageObserver(self.epochSecret)\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) LeaksThroughAFieldCallback() {\n" +
	"\tself.observer(self.epochSecret)\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) LeaksIntoAGoroutine() {\n" +
	"\tgo wipeControl(self.epochSecret)\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) LeaksThroughAClosureHandedToAForeignCall() {\n" +
	"\tregisterControl(func() []byte {\n" +
	"\t\treturn self.epochSecret\n" +
	"\t})\n" +
	"}\n" +
	"\n" +
	"func (self *BufferedHolder) LeaksThroughAMethodOnWhatItReaches() {\n" +
	"\tself.epochSecret.WriteTo(theControlSink)\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) LocksWhileItReadsIt() int {\n" +
	"\tself.guard.Lock()\n" +
	"\tdefer self.guard.Unlock()\n" +
	"\treturn len(self.epochSecret)\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) Zeroize() {\n" +
	"\twipeControl(self.epochSecret)\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) CutsALocalFromIt() int {\n" +
	"\tlocal := self.epochSecret\n" +
	"\treturn len(local)\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) KeepsItInItsOwnStorage() {\n" +
	"\tself.kept = self.epochSecret\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) RefusesWithASentinel() error {\n" +
	"\treturn fmt.Errorf(\"%w: %d bytes\", ErrControl, len(self.epochSecret))\n" +
	"}\n" +
	"\n" +
	"func (self *TableHolder) LeaksARangeValueThroughAForeignCall() {\n" +
	"\tfor _, one := range self.epochSecret {\n" +
	"\t\tpackageObserver(one)\n" +
	"\t}\n" +
	"}\n" +
	"\n" +
	"func (self *TableHolder) LeaksARangeKeyThroughAForeignCall() {\n" +
	"\tfor named := range self.epochSecret {\n" +
	"\t\tpackageObserver([]byte(named))\n" +
	"\t}\n" +
	"}\n" +
	"\n" +
	"func (self *TableHolder) CountsWhatARangeYields() int {\n" +
	"\ttotal := 0\n" +
	"\tfor _, one := range self.epochSecret {\n" +
	"\t\ttotal += len(one)\n" +
	"\t}\n" +
	"\treturn total\n" +
	"}\n" +
	"\n" +
	"func (self *Holder) RangesOverSomethingElseWhileItReadsIt(other [][]byte) int {\n" +
	"\ttotal := len(self.epochSecret)\n" +
	"\tfor _, one := range other {\n" +
	"\t\tpackageObserver(one)\n" +
	"\t}\n" +
	"\treturn total\n" +
	"}\n" +
	"\n" +
	"func wipeControl(secret []byte) {\n" +
	"\tfor i := range secret {\n" +
	"\t\tsecret[i] = 0\n" +
	"\t}\n" +
	"}\n"

// epochSecretEscapeControlReaching and epochSecretEscapeControlEscaping are what the closure
// and the escape scan must read out of the fixture above, exactly.
//
// They are package level rather than local to the gate that owns the fixture because the
// SECRET TREE's gate reads them too. Both gates run the same two functions, over different
// storage, and a refinement made for one of them silently weakens the other: the whole reason
// the secret tree could not be gated until now is that the scan reported eight false positives
// on correct code, and the fix for that was a change to this shared machinery. One committed
// set read by both is what makes "the analysis still reports every way out" a thing each gate
// asserts for itself rather than a thing a reader has to check by hand.
//
// Exact rather than a floor, in both directions. A scan that widened to report a legitimate
// shape would be weakened until it reported nothing, and a scan that narrowed would issue the
// clean bill a working one issues.
var (
	epochSecretEscapeControlReaching = []string{
		"CountsWhatARangeYields",
		"CutsALocalFromIt",
		"KeepsItInItsOwnStorage",
		"LeaksARangeKeyThroughAForeignCall",
		"LeaksARangeValueThroughAForeignCall",
		"LeaksByCopyingIntoPackageStorage",
		"LeaksIntoAChannel",
		"LeaksIntoAGoroutine",
		"LeaksIntoAnExportedVariable",
		"LeaksIntoAnUnexportedVariable",
		"LeaksThroughACallerCallback",
		"LeaksThroughAClosureHandedToAForeignCall",
		"LeaksThroughAFieldCallback",
		"LeaksThroughAMethodOnWhatItReaches",
		"LeaksThroughAPackageCallback",
		"LocksWhileItReadsIt",
		"RangesOverSomethingElseWhileItReadsIt",
		"RefusesWithASentinel",
		"Zeroize",
	}

	epochSecretEscapeControlEscaping = []string{
		"LeaksARangeKeyThroughAForeignCall: calls packageObserver, which this package does not declare",
		"LeaksARangeValueThroughAForeignCall: calls packageObserver, which this package does not declare",
		"LeaksByCopyingIntoPackageStorage: hands copy both what it reaches and stashed, which outlives it",
		"LeaksIntoAChannel: sends on escapeChannel",
		"LeaksIntoAGoroutine: starts a goroutine, which outlives the call that started it",
		"LeaksIntoAnExportedVariable: writes to Escaped, which outlives it",
		"LeaksIntoAnUnexportedVariable: writes to escapedUnexported, which outlives it",
		"LeaksThroughACallerCallback: calls hand, which this package does not declare",
		"LeaksThroughAClosureHandedToAForeignCall: calls registerControl, which this package does not declare",
		"LeaksThroughAFieldCallback: calls self.observer, a function value this package does not declare",
		"LeaksThroughAMethodOnWhatItReaches: calls self.epochSecret.WriteTo, which this package does not declare",
		"LeaksThroughAPackageCallback: calls packageObserver, which this package does not declare",
	}
)

// declaredNames is what the escape scan needs in order to tell code this package wrote from
// code it did not.
//
// Four sets, each read off the source rather than written down. functions is every func and
// method declaration plus every method an interface of this package names, because a call
// landing in one of those lands in a body this same scan can be run over. types is every named
// type, because Foo(x) with Foo a type is a conversion and not a call at all. imports is every
// package the files import, under the name they refer to it by, because subtle.ConstantTimeCompare
// is not a function value somebody handed in. funcFields is every struct field of func type,
// because self.observer(x) is a call into code this package never declared however much it
// reads like a method.
type declaredNames struct {
	functions  map[string]bool
	types      map[string]bool
	imports    map[string]bool
	funcFields map[string]bool
}

// fieldsOf is a nil safe read of a field list, since a struct with no fields and an interface
// with no methods both carry a nil one.
func fieldsOf(list *ast.FieldList) []*ast.Field {
	if list == nil {
		return nil
	}
	return list.List
}

func namesTheseFilesDeclare(files []parsedSource) declaredNames {
	names := declaredNames{
		functions:  map[string]bool{},
		types:      map[string]bool{},
		imports:    map[string]bool{},
		funcFields: map[string]bool{},
	}
	for _, parsed := range files {
		for _, imported := range parsed.file.Imports {
			if imported.Name != nil {
				names.imports[imported.Name.Name] = true
				continue
			}
			path := strings.Trim(imported.Path.Value, "\"")
			names.imports[path[strings.LastIndex(path, "/")+1:]] = true
		}
		// ast.Inspect rather than a walk over file.Decls, because a struct type declared
		// inside a function body carries a func typed field exactly as a package level one
		// does, and the leak through it reads the same
		ast.Inspect(parsed.file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.FuncDecl:
				names.functions[typed.Name.Name] = true
			case *ast.TypeSpec:
				names.types[typed.Name.Name] = true
			case *ast.InterfaceType:
				for _, method := range fieldsOf(typed.Methods) {
					if _, isFunctionType := method.Type.(*ast.FuncType); !isFunctionType {
						continue
					}
					for _, declared := range method.Names {
						names.functions[declared.Name] = true
					}
				}
			case *ast.StructType:
				for _, field := range fieldsOf(typed.Fields) {
					if _, isFunctionType := field.Type.(*ast.FuncType); !isFunctionType {
						continue
					}
					for _, declared := range field.Names {
						names.funcFields[declared.Name] = true
					}
				}
			}
			return true
		})
	}
	return names
}

// namesBoundInside is every name one declaration introduces: its receiver, its parameters and
// its results, and everything its body declares.
//
// The COMPLEMENT is what the scan is after, which is why this is written as the set of names
// the declaration created rather than as a list of package level names. Storage a declaration
// did not introduce was there before the call and is there after it, so a write that lands
// there is the secret put somewhere the call does not end -- and derived this way the class
// covers a package level name in a file the scan was never pointed at, and covers
// otherPackage.Global, neither of which any list of THIS package's own values holds.
func namesBoundInside(function *ast.FuncDecl) map[string]bool {
	bound := map[string]bool{}
	add := func(list *ast.FieldList) {
		for _, field := range fieldsOf(list) {
			for _, declared := range field.Names {
				bound[declared.Name] = true
			}
		}
	}
	add(function.Recv)
	add(function.Type.Params)
	add(function.Type.Results)
	if function.Body == nil {
		return bound
	}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.AssignStmt:
			if typed.Tok != token.DEFINE {
				break
			}
			for _, target := range typed.Lhs {
				if declared, isIdentifier := target.(*ast.Ident); isIdentifier {
					bound[declared.Name] = true
				}
			}
		case *ast.ValueSpec:
			for _, declared := range typed.Names {
				bound[declared.Name] = true
			}
		case *ast.TypeSpec:
			bound[typed.Name.Name] = true
		case *ast.RangeStmt:
			for _, over := range []ast.Expr{typed.Key, typed.Value} {
				if declared, isIdentifier := over.(*ast.Ident); isIdentifier {
					bound[declared.Name] = true
				}
			}
		case *ast.FuncLit:
			add(typed.Type.Params)
			add(typed.Type.Results)
		}
		return true
	})
	return bound
}

// theAliasesOfWhatItReaches is every name inside one declaration that carries the storage: the
// storage itself, and every local an assignment or a RANGE has put it in.
//
// Only a plain name joins, never a field of one. self.kept = self.epochSecret puts the secret
// in the receiver's own storage, which is the holder this gate is about rather than somewhere
// beyond it, and reading the receiver as an alias would make every later mention of any of its
// fields read as the secret.
//
// The range half is not a second spelling of the assignment half, it is a second STATEMENT
// that binds a name to the storage, and leaving it out was a one-sided hole. namesBoundInside
// next door already has a *ast.RangeStmt case, so a range variable was correctly local for the
// rule about writes and invisible to the rule about aliases -- and the shape that fell through
// is the erase loop, which is what this whole scan is pointed at:
//
//	for _, secret := range self.nodes {
//		foreign(secret)
//	}
//
// hands code this package did not write a node secret with no name the alias set had ever
// seen. Measured: that exact shape in SecretTree.Zeroize survived all 824 tests of this
// package and ../message, while the same callee handed self.nodes[0] on the line above it was
// reported. The KEY is bound as well as the value, because the byte walk this package's other
// gates are built on reads a map's keys as secret bearing for the reason its comment gives --
// a table that remembered a destroyed secret by keying on it holds that secret exactly as much
// as one that stored it -- and go gives no syntax to tell ranging a map from ranging a slice.
func theAliasesOfWhatItReaches(function *ast.FuncDecl, storage string) map[string]bool {
	aliases := map[string]bool{storage: true}
	if function.Body == nil {
		return aliases
	}
	join := func(target ast.Expr, grew *bool) {
		identifier, isIdentifier := target.(*ast.Ident)
		if !isIdentifier || identifier.Name == "_" || aliases[identifier.Name] {
			return
		}
		aliases[identifier.Name] = true
		*grew = true
	}
	for {
		grew := false
		ast.Inspect(function.Body, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.AssignStmt:
				for at, right := range typed.Rhs {
					if at < len(typed.Lhs) && carriesWhatItReaches(right, aliases) {
						join(typed.Lhs[at], &grew)
					}
				}
			case *ast.RangeStmt:
				if !carriesWhatItReaches(typed.X, aliases) {
					break
				}
				join(typed.Key, &grew)
				join(typed.Value, &grew)
			}
			return true
		})
		if !grew {
			return aliases
		}
	}
}

// carriesWhatItReaches answers whether one expression evaluates to the storage or to a view
// into it.
//
// It walks the selector, index, slice, star and unary chain, because self.epochSecret,
// epochSecret, self.epochSecret[:8] and &self.epochSecret all name the same array. It walks
// INTO a call's arguments, because bytes.Clone(self.epochSecret) is the secret in a second
// array and handing THAT on is the same escape -- with one exception, a call to a function the
// language predeclares, which is where len and cap live. len(self.epochSecret) is an int the
// secret cannot be recovered from, and reading it as the secret would make every length check
// in this package's constructors an escape.
func carriesWhatItReaches(expr ast.Expr, aliases map[string]bool) bool {
	switch typed := expr.(type) {
	case *ast.Ident:
		return aliases[typed.Name]
	case *ast.ParenExpr:
		return carriesWhatItReaches(typed.X, aliases)
	case *ast.StarExpr:
		return carriesWhatItReaches(typed.X, aliases)
	case *ast.UnaryExpr:
		return carriesWhatItReaches(typed.X, aliases)
	case *ast.IndexExpr:
		return carriesWhatItReaches(typed.X, aliases)
	case *ast.SliceExpr:
		return carriesWhatItReaches(typed.X, aliases)
	case *ast.SelectorExpr:
		return aliases[typed.Sel.Name] || carriesWhatItReaches(typed.X, aliases)
	case *ast.CallExpr:
		if callee, isIdentifier := typed.Fun.(*ast.Ident); isIdentifier && types.Universe.Lookup(callee.Name) != nil {
			return false
		}
		return slices.ContainsFunc(typed.Args, func(argument ast.Expr) bool {
			return carriesWhatItReaches(argument, aliases)
		})
	case *ast.FuncLit:
		// a literal carries whatever its body CAPTURES, and the capture is the one thing an
		// argument walk cannot see: register(func() []byte { return self.epochSecret })
		// names no alias at the argument position at all, and handing that literal to code
		// this package did not write hands over the secret just as directly as passing the
		// slice would. Any mention of a name the storage reaches is enough, in the same
		// safe direction the reaching closure next door over-approximates in.
		captured := false
		ast.Inspect(typed.Body, func(node ast.Node) bool {
			if identifier, isIdentifier := node.(*ast.Ident); isIdentifier && aliases[identifier.Name] {
				captured = true
			}
			return !captured
		})
		return captured
	}
	return false
}

// handsAForeignCalleeWhatItReaches answers whether one call puts what the declaration reaches
// inside the callee's reach, which is the question "is this an escape" reduces to once the
// callee is known to be code this package did not write.
//
// Two ways in, because a go call has two: an argument, and the receiver the callee is reached
// on. The receiver half is not decoration -- self.nodes.Store(index, secret) hands a foreign
// method the whole storage without any argument of the call carrying it -- and it is the half
// that keeps this refinement from being a hole where the storage is a value of a foreign type.
//
// What is deliberately NOT here is the callee's own package qualifier: subtle in
// subtle.ConstantTimeCompare is an *ast.Ident that names an import, and theForeignCallee has
// already answered that such a call is not foreign, so this is never asked about one.
func handsAForeignCalleeWhatItReaches(call *ast.CallExpr, aliases map[string]bool) bool {
	if selector, isSelector := call.Fun.(*ast.SelectorExpr); isSelector && carriesWhatItReaches(selector.X, aliases) {
		return true
	}
	return slices.ContainsFunc(call.Args, func(argument ast.Expr) bool {
		return carriesWhatItReaches(argument, aliases)
	})
}

// theForeignCallee names the callee of one call when that callee is code this package did not
// write, and answers the empty string otherwise.
//
// Four things a callee can be that are NOT foreign, and each is a question the source answers.
// A name the language predeclares is copy or len or append. A name this package declares as a
// func, a method or an interface method is a body this same scan runs over. A name this
// package declares as a TYPE is a conversion rather than a call. A selector rooted at an
// imported package name is that package's function.
//
// Everything else is a function value: a func typed parameter, a package level func variable,
// a func typed field, an element of a map or slice of funcs, a local holding any of those.
// Those are one class and not four, they are the class the three reflection sweeps next door
// cannot see -- none of them appears in a signature -- and a scan that named the parameter
// form alone let the other three past.
//
// The field case is asked FIRST, ahead of the method case, because a func typed field named
// the same as some method of this package would otherwise read as that method and the leak
// through it would be invisible.
func theForeignCallee(parsed parsedSource, callee ast.Expr, names declaredNames) string {
	switch typed := callee.(type) {
	case *ast.ParenExpr:
		return theForeignCallee(parsed, typed.X, names)
	case *ast.IndexExpr:
		return theForeignCallee(parsed, typed.X, names)
	case *ast.IndexListExpr:
		return theForeignCallee(parsed, typed.X, names)
	case *ast.FuncLit:
		return ""
	case *ast.Ident:
		if types.Universe.Lookup(typed.Name) != nil || names.functions[typed.Name] || names.types[typed.Name] {
			return ""
		}
		return "calls " + typed.Name + ", which this package does not declare"
	case *ast.SelectorExpr:
		if names.funcFields[typed.Sel.Name] {
			return "calls " + parsed.render(callee) + ", a function value this package does not declare"
		}
		if names.imports[rootIdentifierOf(typed.X)] || names.functions[typed.Sel.Name] {
			return ""
		}
		return "calls " + parsed.render(callee) + ", which this package does not declare"
	}
	return ""
}

// whereOneDeclarationPutsIt is the derivation itself, over one declaration.
//
// A value inside a go call has exactly three kinds of destination that outlive the call, and
// each is a question about the source that can be asked without naming any spelling of it:
//
//   - storage the declaration did not introduce. Read as a write whose target is rooted at a
//     name namesBoundInside did not report: a package level variable of this package or of
//     another. The VERB does not matter, which is the whole lesson of the copy row in the
//     control -- an assignment is one spelling of that write and the copy builtin is another,
//     so a call handed BOTH what the declaration reaches and storage it did not introduce is
//     the same escape as the assignment.
//   - another goroutine. A channel send, and the go statement that starts one.
//   - code this package did not write, which is theForeignCallee above.
//
// Handing the storage to a function this package DECLARES is none of the three, and that is
// what keeps Zeroize outside the class: zeroizeSecret is a body, and what that body does with
// the bytes is answered by running this same scan over IT.
func whereOneDeclarationPutsIt(parsed parsedSource, function *ast.FuncDecl, names declaredNames, storage string) []string {
	bound := namesBoundInside(function)
	aliases := theAliasesOfWhatItReaches(function, storage)
	// outlivesTheCall answers, of one expression, the name it is rooted at when that name is
	// storage this declaration did not introduce, and the empty string otherwise. A
	// predeclared name is nil or true rather than storage, and an imported name is a package.
	outlivesTheCall := func(expr ast.Expr) string {
		root := rootIdentifierOf(expr)
		if root == "" || root == "_" || bound[root] || types.Universe.Lookup(root) != nil || names.imports[root] {
			return ""
		}
		return root
	}
	put := []string{}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.SendStmt:
			put = append(put, "sends on "+parsed.render(typed.Chan))
		case *ast.GoStmt:
			put = append(put, "starts a goroutine, which outlives the call that started it")
		case *ast.AssignStmt:
			// a define introduces a local, and a local is gone when the call returns
			if typed.Tok == token.DEFINE {
				break
			}
			for _, target := range typed.Lhs {
				if outlivesTheCall(target) != "" {
					put = append(put, "writes to "+parsed.render(target)+", which outlives it")
				}
			}
		case *ast.CallExpr:
			// a callee this package did not write carries the storage out only when it is
			// HANDED it: as one of the arguments, or as the receiver it is reached on. The
			// rule this replaced read "calls something foreign" and had no notion of what
			// the call was given, which is right for a holder that calls nothing it does
			// not declare and wrong the moment one holds a lock: sync.Mutex's Lock and
			// Unlock are foreign, are handed nothing, answer nothing and can carry nothing,
			// and they put every locked method of the secret tree in the report.
			if handsAForeignCalleeWhatItReaches(typed, aliases) {
				if why := theForeignCallee(parsed, typed.Fun, names); why != "" {
					put = append(put, why)
				}
			}
			if !slices.ContainsFunc(typed.Args, func(argument ast.Expr) bool {
				return carriesWhatItReaches(argument, aliases)
			}) {
				break
			}
			for _, argument := range typed.Args {
				if root := outlivesTheCall(argument); root != "" {
					put = append(put, "hands "+parsed.render(typed.Fun)+" both what it reaches and "+root+", which outlives it")
				}
			}
		}
		return true
	})
	slices.Sort(put)
	return slices.Compact(put)
}

// theDeclarationsPuttingWhatTheyReachBeyondTheCall answers, of the declarations named in
// reaching, the ones that put what they reach somewhere the call does not end.
//
// The class is DERIVED by whereOneDeclarationPutsIt from where a value can go, rather than
// written down as a list of shapes. The list this replaced held three -- an assignment to a
// package level name, a channel send, and a call to a func typed PARAMETER -- and each of the
// three was defeated by writing the same leak a different way: copy() instead of =, a package
// level func variable instead of a parameter, a func typed field instead of either. All three
// mutants passed the gate that advertised catching exactly those shapes.
//
// Which DECLARATIONS are scanned is the closure's own admission test re-applied to each body,
// and not a match on the name alone. The closure resolves callees by name on purpose, which
// over-approximates in the safe direction -- x.Foo() joins whatever Foo this package declares
// -- but reading the resulting NAME set back as a set of declarations is a different use of
// it, and one that is over-approximate in a direction that is not safe at all: it reports a
// declaration that shares a SPELLING with a reaching one, on another type, whose body cannot
// reach the storage by any path. p4 task 22 lands the second Zeroize of this package, on
// *SecretTree, whose body touches nothing of the key schedule and which this gate reported
// as leaking the epoch secret through sync.Mutex.Lock. A false report a reader has to learn
// to ignore is how a gate stops being read.
//
// Nothing is lost by it: a body reaches the storage only by naming it or by calling something
// that does, and either one puts a name from reaching -- or the storage itself -- inside that
// body, which is exactly what is tested here. The over-approximation for CALLEES is untouched.
func theDeclarationsPuttingWhatTheyReachBeyondTheCall(files []parsedSource, reaching []string, storage string) []string {
	names := namesTheseFilesDeclare(files)
	admitted := map[string]bool{storage: true}
	for _, name := range reaching {
		admitted[name] = true
	}
	mentionsAnAdmittedName := func(body *ast.BlockStmt) bool {
		found := false
		ast.Inspect(body, func(node ast.Node) bool {
			if identifier, isIdentifier := node.(*ast.Ident); isIdentifier && admitted[identifier.Name] {
				found = true
			}
			return !found
		})
		return found
	}
	escaping := []string{}
	for _, parsed := range files {
		for _, declaration := range parsed.file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil || !slices.Contains(reaching, function.Name.Name) {
				continue
			}
			if !mentionsAnAdmittedName(function.Body) {
				continue
			}
			for _, why := range whereOneDeclarationPutsIt(parsed, function, names, storage) {
				escaping = append(escaping, function.Name.Name+": "+why)
			}
		}
	}
	slices.Sort(escaping)
	return slices.Compact(escaping)
}

// TestNoDeclarationReachingTheEpochSecretPutsItBeyondTheCall is the half of guardrail G6 the
// three gates above each have to let past, and which Zeroize is the first declaration of this
// package to land inside.
//
// TestNoExportedMethodOfThisPackageCanReachTheEpochSecret exempts a declaration that "answers
// nothing and is handed nothing" on the ground that it has nowhere to put the secret. That is
// true of a SIGNATURE and false of a BODY, and the body is what this reads. Neither of the two
// behavioural gates can see any of it either, for the same reason they cannot see an argument
// conditioned leak: they read what a CALL ANSWERS, and a declaration of this shape answers
// nothing.
//
// packageLevelValuesNaming next door is the nearest thing that existed, and it is a different
// question -- it reads a package level value whose TYPE or initialiser names a holder, so
// var TheSchedule *KeySchedule is caught and var Escaped []byte, filled in by a method that
// reaches the storage, is not.
func TestNoDeclarationReachingTheEpochSecretPutsItBeyondTheCall(t *testing.T) {
	// the control first: the closure reads the shapes that reach the storage, and the escape
	// scan tells the eight ways out from the four legitimate shapes that look like them
	control := []parsedSource{mustParseText(t, "the epoch secret escape control", epochSecretEscapeControl)}
	controlReaching := theNamesReachingTheStorage(declaredAcross(control), epochSecretStorageField)
	wantReaching := epochSecretEscapeControlReaching
	if !slices.Equal(controlReaching, wantReaching) {
		t.Fatalf("the closure read %v out of the control as reaching the epoch secret, want %v",
			controlReaching, wantReaching)
	}
	controlEscaping := theDeclarationsPuttingWhatTheyReachBeyondTheCall(
		control, controlReaching, epochSecretStorageField)
	wantEscaping := epochSecretEscapeControlEscaping
	if !slices.Equal(controlEscaping, wantEscaping) {
		t.Fatalf("the escape scan read\n%v\nout of the control, want\n%v\nit is not telling a write that outlives the call from one that does not, or it is reading an erase or a length check as an escape",
			controlEscaping, wantEscaping)
	}

	// then this package's own source
	files := []parsedSource{}
	for _, path := range packageLevelFunctions(t).files {
		files = append(files, mustParseSource(t, path))
	}
	reaching := theNamesReachingTheStorage(declaredAcross(files), epochSecretStorageField)
	if len(reaching) == 0 {
		t.Fatalf("no declaration of this package's source mentions %s, so the class below is empty and this gate demands nothing; if the storage was renamed, rename it in epochSecretStorageField too",
			epochSecretStorageField)
	}
	// the four sets the scan tells this package's own code by have to be populated out of
	// this package's own source, or every callee below reads as foreign and what this gate
	// reports is noise a reader would learn to ignore
	names := namesTheseFilesDeclare(files)
	if len(names.functions) == 0 || len(names.types) == 0 || len(names.imports) == 0 {
		t.Fatalf("the scan read %d functions, %d types and %d imports out of this package's source; it is not reading the source it is about to judge",
			len(names.functions), len(names.types), len(names.imports))
	}
	// the shape this gate exists for has to be present, or it is a gate over an empty
	// exemption again: an eraser is a declaration that reaches the storage and answers
	// nothing, and it is the one the two gates above let past by design.
	// the receiver travels with the name, because two types of this package declare a Zeroize
	// and this list is collected across files by name. Reported bare, it read "[Zeroize
	// Zeroize]" -- one entry per declaration, both spelled the same, and a reader has no way
	// to tell a genuine pair from a bug that counted one declaration twice.
	byteSlices := slices.Concat([]string{"[]byte"}, packageByteSliceTypeNames(t))
	exempt := []string{}
	for _, one := range declaredAcross(files) {
		if !one.exported || !slices.Contains(reaching, one.name) || hasSomewhereToPutASecret(one, byteSlices) {
			continue
		}
		if one.receiver != "" {
			exempt = append(exempt, "("+one.receiver+")."+one.name)
			continue
		}
		exempt = append(exempt, one.name)
	}
	if len(exempt) == 0 {
		t.Fatalf("no exported declaration of this package reaches %s and is exempted by its shape from TestNoExportedMethodOfThisPackageCanReachTheEpochSecret, so this gate is holding an exemption that covers nothing; Zeroize is that shape and it should be here",
			epochSecretStorageField)
	}
	// every declaration in reaching is scanned, exempt or not. exempt is logged rather than
	// filtered on, because it names the declarations the OTHER gates cannot see and so the
	// ones this scan is the only cover for.
	t.Logf("%d declarations reach %s (%v); of those %v answer nothing and are handed nothing, so the scan below is the only gate over them",
		len(reaching), epochSecretStorageField, reaching, exempt)

	escaping := theDeclarationsPuttingWhatTheyReachBeyondTheCall(files, reaching, epochSecretStorageField)
	for _, one := range escaping {
		t.Errorf("%s -- it reaches %s and puts it somewhere the call does not end: storage this package's own call did not introduce, another goroutine, or code this package did not write. G6 says no exported symbol of this package hands out the parent secret, and none of those appears in a signature for the gates above to read",
			one, epochSecretStorageField)
	}
}

// epochSecretMethodsTheSweepDrivesInstead is the one exemption the source reading above admits,
// and what an entry owes is a SWEEP rather than a sentence.
//
// The reading is a proxy. It asks whether an exported method can REACH the storage, because the
// argument space of a method that can is not exhaustible and the behavioural gates cannot close it
// -- that argument is on the test itself and it is right. What the proxy cannot tell apart is a
// method that could ANSWER the parent secret from one whose whole job is to build the epoch AFTER
// it: p7 task 13's (*Group).CreateCommit calls NewKeySchedule, which is four bare names away from
// the field, and answers a *CommitResult, so it reaches the storage and has somewhere to put it.
// It is not a way out for the secret at all, and it is the first declaration of this package in
// that position. Two more of the same shape are already written down in the plan -- task 16's join
// and task 18's commit processing -- so the mechanism is here rather than the name.
//
// WHAT MAKES AN ENTRY SAFE IS THAT THE BYTES ARE STILL COMPARED. Every method named here is driven
// by bytesTheGroupHandsOut through groupMethodArgumentRows, which calls it and holds every byte it
// answers against the epoch secret of the group it was called on -- so "this method does not hand
// out epoch_secret" is measured on the real answer rather than argued from the source. The gate
// above holds each entry to exactly that: an entry whose method the sweep does not drive fails, and
// so does an entry the source reading has stopped reporting.
//
// It is NOT a general exemption for a method that reaches the storage. The proxy exists because a
// method's arguments cannot be swept, and the entries here are the ones whose arguments this
// package can enumerate: CreateCommit takes a proposal vector, a proposal list and an options
// struct, and the one row that drives it is the one a group of one can be driven with.
var epochSecretMethodsTheSweepDrivesInstead = map[string]string{
	"(*Group).CreateCommit": "builds the NEXT epoch's key schedule, so it reaches the parent secret " +
		"through NewKeySchedule and answers a *CommitResult of octets derived from that epoch; " +
		"bytesTheGroupHandsOut drives it and compares every one of them",
	// p7 task 18's receive path, which reaches the parent secret the same way and from the other
	// direction: a commit it processes builds the epoch that commit opens, through the same
	// NewKeySchedule. What it answers is a *Processed, whose commit arm is a holder this file
	// sweeps in its own right -- so the bytes a leak could travel in are read twice over, once
	// through the reflection walk this row drives and once through the staged commit's own sweep.
	"(*Group).ProcessMessage": "reaches the parent secret through the key schedule a received commit " +
		"builds, and answers a *Processed whose arms are octets off the wire and a staged commit; " +
		"bytesTheGroupHandsOut drives it over a real inbound message and compares every one of them",
	"(*Group).Unprotect": "is (*Group).ProcessMessage with the application arm taken out of it, so it " +
		"reaches the parent secret through the same call and answers the plaintext that call " +
		"decrypted; bytesTheGroupHandsOut drives it over a real inbound message and compares it",
	// THE FIVE SENDING DOORS, and they are here because of what a seal now costs. Sealing a message
	// spends a generation of this leaf's ratchet, and a generation spent and not RECORDED is a
	// generation a restored member draws again -- two plaintexts under one key and one base nonce.
	// So (*Group).sealAndRecordLocked persists before the ciphertext leaves, persist reaches
	// marshalState, and marshalState asks the schedule for the secret this epoch is rebuilt from.
	// Every one of these five is on that path and none of them was before.
	//
	// The exemption is the same one CreateCommit rests on and it rests on the same sweep: each of
	// these answers the message it sealed, bytesTheGroupHandsOut drives all five with the arguments
	// a caller would use -- ProposeUpdate takes none and is called directly -- and compares every
	// octet each of them hands out against the epoch secret the fixture was founded over.
	"(*Group).Protect": "seals an application message and records the generation it spent, so it " +
		"reaches the parent secret through persist and answers the ciphertext; " +
		"bytesTheGroupHandsOut drives it and compares every octet of that",
	// the SIXTH sending door, MASTER section 8.4.2 v2's, on exactly Protect's terms: it seals the
	// same message under an aad the caller BUILDS from the generation the seal is about to spend,
	// and it records that generation through the same persist. What it answers is the same
	// ciphertext, and the same sweep drives it with a builder and compares every octet.
	"(*Group).ProtectBound": "seals an application message under a caller-built aad and records the " +
		"generation it spent, so it reaches the parent secret through persist and answers the " +
		"ciphertext; bytesTheGroupHandsOut drives it and compares every octet of that",
	"(*Group).ProposeAdd": "seals a proposal and records the generation it spent, so it reaches " +
		"the parent secret through persist and answers the encoded message; " +
		"bytesTheGroupHandsOut drives it and compares every octet of that",
	"(*Group).ProposeRemove": "seals a proposal and records the generation it spent, so it reaches " +
		"the parent secret through persist and answers the encoded message; " +
		"bytesTheGroupHandsOut drives it and compares every octet of that",
	"(*Group).ProposeUpdate": "seals a proposal and records the generation it spent, so it reaches " +
		"the parent secret through persist and answers the encoded message; it takes no arguments " +
		"and bytesTheGroupHandsOut calls it directly and compares every octet of that",
	"(*Group).ProposeGroupContextExtensions": "seals a proposal and records the generation it " +
		"spent, so it reaches the parent secret through persist and answers the encoded message; " +
		"bytesTheGroupHandsOut drives it and compares every octet of that",
}

// stagedCommitMethodArgumentRows is groupMethodArgumentRows one type over: the argument rows the
// G6 sweep drives an argument-taking method of *StagedCommit through, so that an accessor which
// takes a parameter is swept over every value of it a caller could reach rather than falling
// outside the class by having one. A method with no row here and an argument is a FATAL in the
// sweep, and a row for a method that takes none is an error, so the table and the type cannot
// drift apart silently.
var stagedCommitMethodArgumentRows = map[string]func(t *testing.T, staged *StagedCommit) [][]reflect.Value{
	"LeafIdentityAfter": stagedCommitLeafRows,
	// the second leaf-taking accessor, 2026-09-21's R1 follow-up, over the same domain: it
	// reads the same tree at the same index and answers a bool, so the rows are the rows.
	"LeafHasKeysAfter": stagedCommitLeafRows,
}

// stagedCommitLeafRows is EVERY occupied leaf of the staged tree, which is the whole domain a
// caller can name something at, plus one blank leaf inside the tree and one outside it, which
// are the two shapes a leaf-taking accessor answers false for. Read off the accessor's own
// sibling rather than off a fixed count, so a fixture with more leaves is swept over all of them.
func stagedCommitLeafRows(t *testing.T, staged *StagedCommit) [][]reflect.Value {
	rows := [][]reflect.Value{}
	occupied := staged.OccupiedLeavesAfter()
	for _, leaf := range occupied {
		rows = append(rows, []reflect.Value{reflect.ValueOf(leaf)})
	}
	var last LeafIndex
	if len(occupied) != 0 {
		last = occupied[len(occupied)-1]
	}
	rows = append(rows, []reflect.Value{reflect.ValueOf(last + 1)})
	rows = append(rows, []reflect.Value{reflect.ValueOf(LeafIndex(1 << 20))})
	return rows
}

// bytesTheStagedCommitHandsOut is every byte slice reachable through *StagedCommit's own exported
// surface.
//
// It is bytesTheGroupHandsOut one type over and it exists for that function's reason: p7 task 13
// made StagedCommit the third type of this package that can reach an epoch secret, because it holds
// the schedule of the epoch a commit opens for the whole window between sending the commit and the
// delivery service accepting it. Reflection over the compiled method set is what makes an accessor
// added tomorrow join by existing.
//
// AN ACCESSOR THAT TAKES ARGUMENTS IS DRIVEN THROUGH stagedCommitMethodArgumentRows AND NEVER
// SKIPPED. Until 2026-09-21 every exported method of this type took none and this sweep fataled on
// one that did; LeafIdentityAfter is the first to take one, and the answer to it is the one
// bytesTheGroupHandsOut already gives for Export: a table of rows, held in both directions, so the
// method is swept over the values a caller can name rather than left outside the class.
func bytesTheStagedCommitHandsOut(t *testing.T, at string, staged *StagedCommit) []exposedSlice {
	t.Helper()
	stagedType := reflect.TypeOf(staged)
	valueType := stagedType.Elem()
	for i := range valueType.NumField() {
		if valueType.Field(i).IsExported() {
			t.Fatalf("%s: StagedCommit has exported field %s, so its storage is reachable without going through a method this sweep reads",
				at, valueType.Field(i).Name)
		}
	}
	exposed := []exposedSlice{}
	for i := range stagedType.NumMethod() {
		method := stagedType.Method(i)
		rows := [][]reflect.Value{nil}
		if method.Type.NumIn() != 1 {
			build, driven := stagedCommitMethodArgumentRows[method.Name]
			if !driven {
				t.Fatalf("%s: (*StagedCommit).%s takes arguments and this sweep calls with none; give it rows in stagedCommitMethodArgumentRows rather than letting an accessor fall outside G6",
					at, method.Name)
			}
			rows = build(t, staged)
			if len(rows) == 0 {
				t.Fatalf("%s: stagedCommitMethodArgumentRows drives (*StagedCommit).%s with no rows at all, so it is swept in name only",
					at, method.Name)
			}
			for _, row := range rows {
				if len(row)+1 != method.Type.NumIn() {
					t.Fatalf("%s: stagedCommitMethodArgumentRows drives (*StagedCommit).%s with %d arguments and it takes %d",
						at, method.Name, len(row), method.Type.NumIn()-1)
				}
				for argument, value := range row {
					if want := method.Type.In(argument + 1); !value.Type().AssignableTo(want) {
						t.Fatalf("%s: stagedCommitMethodArgumentRows hands (*StagedCommit).%s a %s in argument %d, which takes %s",
							at, method.Name, value.Type(), argument, want)
					}
				}
			}
		} else if _, driven := stagedCommitMethodArgumentRows[method.Name]; driven {
			t.Errorf("stagedCommitMethodArgumentRows drives %s, which takes no arguments", method.Name)
		}
		for _, row := range rows {
			for index, result := range method.Func.Call(append([]reflect.Value{reflect.ValueOf(staged)}, row...)) {
				for _, one := range exposedByteSlices(t, "(*StagedCommit)."+method.Name, result) {
					one.method = method.Name
					one.result = index
					one.taken = bytes.Clone(one.bytes)
					exposed = append(exposed, one)
				}
			}
		}
	}
	for name := range stagedCommitMethodArgumentRows {
		if _, found := stagedType.MethodByName(name); !found {
			t.Errorf("stagedCommitMethodArgumentRows drives %s, which *StagedCommit does not declare", name)
		}
	}
	for _, one := range exposed {
		if !bytes.Equal(one.bytes, one.taken) {
			t.Fatalf("%s: %s changed after this sweep read it, so what it collected is not what those methods answered",
				at, one.path)
		}
	}
	return exposed
}

// TestNoAccessorOfAStagedCommitAnswersTheEpochSecret is the behavioural half over the third holder,
// driven on a real staged commit rather than on a value assembled here.
//
// The sweep above is reachable only from a construction that ANSWERS a staged commit, and this
// package has none -- (*Group).CreateCommit answers a *CommitResult and files the staged commit on
// the group. So without this the sweep would be a declaration nothing runs, which is the shape a
// gate has when it has stopped checking anything.
func TestNoAccessorOfAStagedCommitAnswersTheEpochSecret(t *testing.T) {
	crypto := testCrypto(t)
	owner := testIdentity(t, crypto, "owner")
	group := testNewGroup(t, crypto, owner, "staged-secret")
	defer group.Close()
	if _, err := group.CreateCommit([][]byte{}, nil, nil); err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	staged := group.stagedForTest()
	if staged == nil {
		t.Fatal("the commit staged nothing, so this gate swept nothing")
	}
	secret := staged.schedule.epochSecret
	if len(secret) == 0 {
		t.Fatal("the staged epoch holds no parent secret, so the comparison below states nothing")
	}
	exposed := bytesTheStagedCommitHandsOut(t, "a staged commit", staged)
	if len(exposed) == 0 {
		t.Fatal("the staged commit handed out no octet at all, so this sweep compared nothing")
	}
	for _, one := range exposed {
		if bytes.Equal(one.bytes, secret) {
			t.Errorf("(*StagedCommit).%s result %d answers the epoch secret of the epoch this commit opens; guardrail 6 says no exported symbol of this package does",
				one.method, one.result)
		}
	}
	t.Logf("%d octet run(s) read off a staged commit, none of them the epoch secret", len(exposed))
}

// TestACommitWhoseConfirmationTagIsTheWrongWidthIsRefused is errCommitConfirmationTag's own test,
// and it is the commit side of TestNewGroupRefusesAConfirmationTagOfTheWrongWidth.
//
// The provider is swapped AFTER the group is founded, because a group founded under a truncating
// MAC is refused at creation by the other rule and this one would never be reached. The commit's
// new key schedule is built through the group's provider, so the tag that schedule answers is a
// byte short -- and a tag of the wrong width folded into the interim transcript hash is an epoch
// whose successor nobody can compute, which is why this is a refusal and not a value passed on.
func TestACommitWhoseConfirmationTagIsTheWrongWidthIsRefused(t *testing.T) {
	base := testCrypto(t)
	owner := testIdentity(t, base, "owner")
	group := testNewGroup(t, base, owner, "short-tag")
	defer group.Close()
	group.crypto = &truncatingMacProvider{CryptoProvider: base}
	if _, err := group.CreateCommit([][]byte{}, nil, nil); !errors.Is(err, errCommitConfirmationTag) {
		t.Fatalf("CreateCommit over a provider whose MAC is a byte short = %v, want errCommitConfirmationTag", err)
	}
	if group.stagedForTest() != nil {
		t.Fatal("the refused commit was staged anyway")
	}
}
