// The two halves of the codec are one set of records, and the refusals that make that
// true are the ones checkRecord actually has.
//
// codec_test.go pins the layout and walks the bytes of records this package can build.
// This file observes the property that spans the two directions and that no round trip
// can see: the set of records EncodeRecord will write and the set ParseRecord will read
// are the same set. codec.go states it in its own words — "there is no record this
// package will write and then fail to read back" — and it is the claim the whole
// checkRecord shape exists to make true, so it is asserted here rather than left to the
// three cases somebody thought to write down.
//
// It is asserted over a space computed from the parser's own answers: every class tag and
// every eph bucket the split hands out, each with the first value past the top of its own
// range, crossed with the size ladder and one rung past it, crossed with the blob id
// lengths either side of the one legal width and the ct_body lengths either side of every
// rung. A record shape nobody thought of is in the space as soon as the parser admits the
// byte that reaches it, and the two illegal edges are where the join's and the ladder's
// refusals live.
//
// A universal property over a computed space is only as strong as its coverage, and a
// space that never reaches a refusal says nothing about it. So the refusals are derived
// as well, and from the syntax tree rather than from the space: every refusal reachable
// from checkRecord is read out of the call graph — its own, and those of every function it
// calls that can return an error — and each is turned into the message it renders by
// pairing its format string with the sentinel's own text out of errors.go. The gate is
// then that every one of them is a refusal some record in the space provokes, and every
// refusal the space provokes is one of them. Two sites whose messages cannot be told apart
// fail the gate rather than silently witnessing each other.
//
// The call graph and not the forward, on purpose. checkRecord's own error returns are what
// a reader sees, but three of the seven refusals belong to the join it calls, and a gate
// that followed the forwarded error would lose all three the moment somebody dropped it —
// which is the edit that makes the encoder write a record with the join's 0xFF sentinel in
// the retention byte. Deriving from the call — the join is still called, so its refusals
// are still owed a witness — is what makes dropping the error a failure here rather than a
// smaller set of requirements.
//
// The last two tests are about the four length prefixed fields, and both read the field
// list out of the codec rather than naming it. The fields EncodeRecord writes through
// WriteOpaqueLP have to be the fields decodeRecord canonicalises through absentIfEmpty,
// an empty one of each has to parse back nil rather than as a zero length slice — spec A
// section 5.1's shape, which bytes.Equal cannot tell from an empty slice and a caller's
// nil test can — and none of them can be written past the writer's vector ceiling without
// EncodeRecord saying so.
package message

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// ── the record space, computed from the parser ──────────────────────────────────────

// A record offered to the encoder, and the name a failure reports it by.
type encodeCandidate struct {
	name   string
	record Record
}

// Every class tag and every eph bucket the split hands out, crossed, plus one value past
// the top of each range.
//
// Derived rather than written down, and the illegal edge is derived too: the largest tag
// the split ever answers plus one is a class this package does not define, and the largest
// bucket it ever answers plus one is a bucket off the eph ladder. Those two are where the
// join's three refusals live, and a package that grew a fifth class or a seventh bucket
// moves both edges with it instead of leaving this space probing the old ones.
func candidateClassBuckets(t testing.TB) []classBucket {
	t.Helper()
	classSeen := map[RetentionClass]bool{}
	bucketSeen := map[uint8]bool{}
	for _, pair := range acceptedWireBytes(t) {
		classSeen[pair.class] = true
		bucketSeen[pair.bucket] = true
	}
	classes := []RetentionClass{}
	for class := range classSeen {
		classes = append(classes, class)
	}
	slices.Sort(classes)
	classes = append(classes, classes[len(classes)-1]+1)
	buckets := []uint8{}
	for bucket := range bucketSeen {
		buckets = append(buckets, bucket)
	}
	slices.Sort(buckets)
	buckets = append(buckets, buckets[len(buckets)-1]+1)
	pairs := []classBucket{}
	for _, class := range classes {
		for _, bucket := range buckets {
			pairs = append(pairs, classBucket{class: class, bucket: bucket})
		}
	}
	return pairs
}

// The smallest ct_body length that is no rung's, computed by walking the ladder rather
// than by picking a number that looks wrong. It is the length that exercises the ct_body
// rule on a rung with no inline body at all, where there is no rung length to sit beside.
func offEveryRungBodyLength(t testing.TB) int {
	t.Helper()
	rungs := map[int]bool{}
	for _, sizeBucket := range sortedByteKeys(acceptedSizeBuckets(t)) {
		if length := SizeBucketCtBodyBytes(SizeBucket(sizeBucket)); 0 <= length {
			rungs[length] = true
		}
	}
	for length := 1; ; length++ {
		if !rungs[length] {
			return length
		}
	}
}

// The space the agreement is asserted over: every class and bucket pair crossed with the
// size ladder and one rung past it, crossed with the blob id lengths either side of the
// legal width, crossed with the ct_body lengths either side of every rung.
//
// The blob id lengths are the one place a width appears, and it is blobIdBytes itself with
// its two neighbours — the constant codec.go checks against, not a second copy of it, so a
// change to the width moves these probes with it.
func encodeCandidates(t testing.TB) []encodeCandidate {
	t.Helper()
	shapes := acceptedSizeBuckets(t)
	sizeBuckets := sortedByteKeys(shapes)
	sizeBuckets = append(sizeBuckets, sizeBuckets[len(sizeBuckets)-1]+1)
	offRung := offEveryRungBodyLength(t)
	blobLengths := []int{0, blobIdBytes - 1, blobIdBytes, blobIdBytes + 1}
	candidates := []encodeCandidate{}
	for _, pair := range candidateClassBuckets(t) {
		for _, sizeBucket := range sizeBuckets {
			bodyLengths := []int{0, offRung}
			if rung := SizeBucketCtBodyBytes(SizeBucket(sizeBucket)); 0 < rung {
				bodyLengths = append(bodyLengths, rung-1, rung, rung+1)
			}
			for _, blobLength := range blobLengths {
				for _, bodyLength := range bodyLengths {
					record := Record{
						Header: RecordHeader{
							Epoch:            1,
							StreamIndex:      2,
							RetentionClass:   pair.class,
							EphBucket:        pair.bucket,
							SizeBucket:       SizeBucket(sizeBucket),
							ExpireAt:         3,
							BlobId:           fillBytes(blobIdTag, blobLength),
							ServerAttachment: fillBytes(serverAttachmentTag, 40),
						},
						CtHead: fillBytes(ctHeadTag, 96),
						CtBody: ctBodyFiller(bodyLength),
					}
					copy(record.Header.GroupId[:], fillBytes(groupIdTag, 32))
					copy(record.Header.SenderHandle[:], fillBytes(senderHandleTag, 16))
					copy(record.Header.BodyHash[:], fillBytes(bodyHashTag, 32))
					copy(record.WriteAuth[:], fillBytes(writeAuthTag, 32))
					candidates = append(candidates, encodeCandidate{
						name: fmt.Sprintf("class=%d bucket=%d size=%d blob=%d body=%d",
							pair.class, pair.bucket, sizeBucket, blobLength, bodyLength),
						record: record,
					})
				}
			}
		}
	}
	if len(candidates) == 0 {
		t.Fatal("the candidate space is empty, so every property asserted over it would hold vacuously")
	}
	return candidates
}

// The smallest record the ladder and the wire alphabet admit, as the starting point for
// the tests below that then break exactly one thing about it.
func smallestLegalRecord(t testing.TB) Record {
	t.Helper()
	shapes := acceptedSizeBuckets(t)
	smallest := sortedByteKeys(shapes)[0]
	return corpusRecord(acceptedWireBytes(t)[probeRetentionWire], smallest, shapes[smallest], false, false, false, 0, u64Triple{})
}

// ── the one set of records ──────────────────────────────────────────────────────────

// The encoder and the parser admit the same records, over the whole computed space and in
// both directions.
//
// One direction is codec.go's own claim: a record the encoder writes is a record the
// parser reads, and reads back to the record that was encoded. The other is the claim that
// makes the first one worth anything: a record the encoder refuses is one the parser would
// have refused too, so the encoder is not merely stricter than its own reader — asked by
// laying the refused record out raw, at the layout codec_test.go states independently of
// the encoder, and offering those bytes to the parser.
//
// A class and bucket pair the join has no wire byte for is skipped on that second half
// alone, and only there: there is no byte string that carries such a pair, so there is no
// input the parser could ever be handed. The gate below is what keeps that skip from
// hiding the join's refusals, by requiring each of them to be witnessed on the encode side.
func TestTheEncoderAndTheParserAdmitTheSameRecords(t *testing.T) {
	accepted := 0
	refused := 0
	for _, candidate := range encodeCandidates(t) {
		bs, err := EncodeRecord(&candidate.record)
		if err == nil {
			accepted++
			if bs == nil {
				t.Fatalf("%s: EncodeRecord answered no error and no bytes", candidate.name)
			}
			parsed, parseErr := parseBoth(t, candidate.name, bs)
			if parseErr != nil {
				t.Fatalf("%s: EncodeRecord wrote %d bytes its own parser refuses: %v", candidate.name, len(bs), parseErr)
			}
			if difference := recordDifference(&candidate.record, parsed); difference != "" {
				t.Fatalf("%s: the parsed record differs from the encoded one: %s", candidate.name, difference)
			}
			again := mustEncode(t, candidate.name, parsed)
			if !bytes.Equal(bs, again) {
				t.Fatalf("%s: re-encoding the parsed record produced %d bytes, want the same %d", candidate.name, len(again), len(bs))
			}
			continue
		}
		refused++
		if bs != nil {
			t.Fatalf("%s: EncodeRecord refused with %v and still handed back %d bytes", candidate.name, err, len(bs))
		}
		retentionWire, joinErr := RetentionClassWire(candidate.record.Header.RetentionClass, candidate.record.Header.EphBucket)
		if joinErr != nil {
			continue
		}
		raw := rawRecordFields(&candidate.record, retentionWire)
		what := fmt.Sprintf("%s laid out raw", candidate.name)
		if _, parseErr := parseBoth(t, what, raw.encode(t)); parseErr == nil {
			t.Fatalf("%s: EncodeRecord refused with %v a record its own parser accepts", candidate.name, err)
		}
	}
	if accepted == 0 {
		t.Fatal("the encoder accepted no candidate at all, so the round trip half of this test held vacuously")
	}
	if refused == 0 {
		t.Fatal("the encoder refused no candidate at all, so the refusal half of this test held vacuously")
	}
	t.Logf("%d records accepted and %d refused, and the parser agreed about every one", accepted, refused)
}

// ── the refusal paths, read out of the syntax tree ──────────────────────────────────

// One place a refusal is constructed, as the syntax tree gives it: the function it is in,
// the sentinel it wraps, the format it renders, and the message that pair produces.
type refusalSite struct {
	function string
	where    string
	sentinel string
	format   string
	pattern  *regexp.Regexp
}

// A verb in a format string, including the doubled percent that is not one. Used both to
// find which argument the %w takes and to turn a format into the message it renders.
var refusalVerb = regexp.MustCompile("%[-+# 0-9.*]*[a-zA-Z%]")

// The package's own syntax tree: its top level functions by name, and its error sentinels
// by name with the text each was constructed with.
//
// Non test files only. These gates are about the code that ships, and a helper in a test
// file that happens to call fmt.Errorf is not a refusal path of anything.
func packageSyntax(t testing.TB) (*token.FileSet, map[string]*ast.FuncDecl, map[string]string) {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("cannot list this package's go files: %v", err)
	}
	fileSet := token.NewFileSet()
	funcs := map[string]*ast.FuncDecl{}
	sentinels := map[string]string{}
	parsed := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatalf("cannot parse %s: %v", path, err)
		}
		parsed++
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Recv == nil {
					funcs[decl.Name.Name] = decl
				}
			case *ast.GenDecl:
				if decl.Tok != token.VAR {
					continue
				}
				for _, spec := range decl.Specs {
					value, isValue := spec.(*ast.ValueSpec)
					if !isValue {
						continue
					}
					for i, name := range value.Names {
						if len(value.Values) <= i {
							continue
						}
						if message, isSentinel := errorsNewMessage(value.Values[i]); isSentinel {
							sentinels[name.Name] = message
						}
					}
				}
			}
		}
	}
	if parsed == 0 {
		t.Fatal("no non test go file was parsed, so every gate that reads the syntax tree would hold vacuously")
	}
	if len(sentinels) == 0 {
		t.Fatal("no error sentinel was found, so no refusal site could be given the text it renders")
	}
	return fileSet, funcs, sentinels
}

// The text an errors.New call was constructed with, for a value that is one.
func errorsNewMessage(expr ast.Expr) (string, bool) {
	call, isCall := expr.(*ast.CallExpr)
	if !isCall || len(call.Args) != 1 {
		return "", false
	}
	selector, isSelector := call.Fun.(*ast.SelectorExpr)
	if !isSelector || selector.Sel.Name != "New" {
		return "", false
	}
	pkg, isIdent := selector.X.(*ast.Ident)
	if !isIdent || pkg.Name != "errors" {
		return "", false
	}
	literal, isLiteral := call.Args[0].(*ast.BasicLit)
	if !isLiteral || literal.Kind != token.STRING {
		return "", false
	}
	message, err := strconv.Unquote(literal.Value)
	if err != nil {
		return "", false
	}
	return message, true
}

// Whether a function can refuse at all: whether its last result is an error. A function
// that cannot is not walked, so the scan stays on the refusal paths and does not wander
// into the ladders, whose returns look like forwarded values and are not.
func returnsError(decl *ast.FuncDecl) bool {
	if decl == nil || decl.Type.Results == nil || len(decl.Type.Results.List) == 0 {
		return false
	}
	last := decl.Type.Results.List[len(decl.Type.Results.List)-1]
	ident, isIdent := last.Type.(*ast.Ident)
	return isIdent && ident.Name == "error"
}

// Every refusal reachable from the named function: the ones it constructs itself, and the
// ones belonging to every function of this package it calls that can return an error, and
// so on down.
//
// The call and not the forwarded error is what carries a callee's refusals in here, and
// that choice is the whole strength of the gate. checkRecord forwards the join's error
// today, but a checkRecord that dropped it would still call the join, so the join's three
// refusals stay in the set and go unwitnessed — where following the forward instead would
// have quietly reduced what the gate asks for by exactly the three paths the edit broke.
//
// Every error return is classified, and a shape this gate cannot account for is a failure
// rather than a skip, because a silent skip is the hole this file exists to close.
func refusalSitesOf(
	t testing.TB,
	fileSet *token.FileSet,
	funcs map[string]*ast.FuncDecl,
	sentinels map[string]string,
	name string,
	seen map[string]bool,
) []refusalSite {
	t.Helper()
	if seen[name] {
		return nil
	}
	seen[name] = true
	decl, found := funcs[name]
	if !found {
		t.Fatalf("%s is not a function of this package, so its refusals cannot be read", name)
	}
	sites := []refusalSite{}
	ast.Inspect(decl.Body, func(node ast.Node) bool {
		if call, isCall := node.(*ast.CallExpr); isCall {
			if fun, isIdent := call.Fun.(*ast.Ident); isIdent && returnsError(funcs[fun.Name]) {
				sites = append(sites, refusalSitesOf(t, fileSet, funcs, sentinels, fun.Name, seen)...)
			}
		}
		ret, isReturn := node.(*ast.ReturnStmt)
		if !isReturn || len(ret.Results) == 0 {
			return true
		}
		where := fileSet.Position(ret.Pos()).String()
		switch result := ret.Results[len(ret.Results)-1].(type) {
		case *ast.Ident:
			if result.Name == "nil" {
				return true
			}
			if message, isSentinel := sentinels[result.Name]; isSentinel {
				// a bare sentinel, which renders as its own text and nothing else.
				sites = append(sites, refusalSite{
					function: name,
					where:    where,
					sentinel: result.Name,
					format:   "%w",
					pattern:  refusalPattern(t, "%w", message),
				})
				return true
			}
			callee := forwardedCallee(t, decl, result.Name, where)
			if !returnsError(funcs[callee]) {
				t.Fatalf("%s: the error forwarded at %s comes from %s, which this gate cannot read the refusals of", name, where, callee)
			}
		case *ast.CallExpr:
			if fun, isIdent := result.Fun.(*ast.Ident); isIdent && returnsError(funcs[fun.Name]) {
				return true
			}
			sites = append(sites, errorfSite(t, sentinels, name, where, result))
		default:
			t.Fatalf("%s: the error returned at %s is a shape this gate cannot classify", name, where)
		}
		return true
	})
	return sites
}

// The function whose error the named variable holds, found by the assignment that produced
// it.
func forwardedCallee(t testing.TB, decl *ast.FuncDecl, name string, where string) string {
	t.Helper()
	callee := ""
	ast.Inspect(decl.Body, func(node ast.Node) bool {
		assign, isAssign := node.(*ast.AssignStmt)
		if !isAssign || len(assign.Rhs) != 1 {
			return true
		}
		holds := false
		for _, lhs := range assign.Lhs {
			if ident, isIdent := lhs.(*ast.Ident); isIdent && ident.Name == name {
				holds = true
			}
		}
		if !holds {
			return true
		}
		if call, isCall := assign.Rhs[0].(*ast.CallExpr); isCall {
			if fun, isIdent := call.Fun.(*ast.Ident); isIdent {
				callee = fun.Name
			}
		}
		return true
	})
	if callee == "" {
		t.Fatalf("%s: the error forwarded at %s comes from no call this gate can follow", decl.Name.Name, where)
	}
	return callee
}

// One fmt.Errorf refusal, with the message it renders worked out from its format string
// and the text of the sentinel its %w expands.
func errorfSite(t testing.TB, sentinels map[string]string, function string, where string, call *ast.CallExpr) refusalSite {
	t.Helper()
	selector, isSelector := call.Fun.(*ast.SelectorExpr)
	if !isSelector || selector.Sel.Name != "Errorf" {
		t.Fatalf("%s: the error constructed at %s is not an fmt.Errorf, and this gate reads no other shape", function, where)
	}
	if len(call.Args) == 0 {
		t.Fatalf("%s: the fmt.Errorf at %s has no format string", function, where)
	}
	literal, isLiteral := call.Args[0].(*ast.BasicLit)
	if !isLiteral || literal.Kind != token.STRING {
		t.Fatalf("%s: the fmt.Errorf at %s does not format a string constant", function, where)
	}
	format, err := strconv.Unquote(literal.Value)
	if err != nil {
		t.Fatalf("%s: the format string at %s does not unquote: %v", function, where, err)
	}
	argument := 1 + wrapArgument(t, function, where, format)
	if len(call.Args) <= argument {
		t.Fatalf("%s: the fmt.Errorf at %s wraps an argument it does not pass", function, where)
	}
	name, isIdent := call.Args[argument].(*ast.Ident)
	if !isIdent {
		t.Fatalf("%s: the error wrapped at %s is not a named sentinel, which errors.go requires of every refusal", function, where)
	}
	message, isSentinel := sentinels[name.Name]
	if !isSentinel {
		t.Fatalf("%s: %s wraps %s, which is not one of this package's errors.New sentinels", function, where, name.Name)
	}
	return refusalSite{
		function: function,
		where:    where,
		sentinel: name.Name,
		format:   format,
		pattern:  refusalPattern(t, format, message),
	}
}

// Which of the formatted arguments the %w verb takes.
func wrapArgument(t testing.TB, function string, where string, format string) int {
	t.Helper()
	index := 0
	for _, verb := range refusalVerb.FindAllString(format, -1) {
		if verb == "%%" {
			continue
		}
		if strings.HasSuffix(verb, "w") {
			return index
		}
		index++
	}
	t.Fatalf("%s: the fmt.Errorf at %s formats %q, which wraps nothing, and errors.go requires every refusal to wrap its sentinel", function, where, format)
	return 0
}

// The message a refusal site renders, as a pattern: the sentinel's own text where the %w
// expands, and anything at all where a value is formatted. The literal text between the
// verbs is what tells two sites that share a sentinel apart, which is why it is matched
// rather than skipped over.
func refusalPattern(t testing.TB, format string, sentinelMessage string) *regexp.Regexp {
	t.Helper()
	pattern := "^"
	rest := format
	for {
		found := refusalVerb.FindStringIndex(rest)
		if found == nil {
			pattern += regexp.QuoteMeta(rest)
			break
		}
		pattern += regexp.QuoteMeta(rest[:found[0]])
		verb := rest[found[0]:found[1]]
		switch {
		case verb == "%%":
			pattern += regexp.QuoteMeta("%")
		case strings.HasSuffix(verb, "w"):
			pattern += regexp.QuoteMeta(sentinelMessage)
		default:
			pattern += ".*"
		}
		rest = rest[found[1]:]
	}
	compiled, err := regexp.Compile(pattern + "$")
	if err != nil {
		t.Fatalf("the pattern built from %q does not compile: %v", format, err)
	}
	return compiled
}

// Every refusal reachable from checkRecord is one EncodeRecord reaches, and every refusal
// EncodeRecord reaches over the candidate space is one of them.
//
// This is what stops the property above from holding vacuously on a path nobody built an
// input for, and it is derived on both sides: the paths come out of the call graph and the
// inputs come out of the parser's alphabets, so neither is a list that can understate the
// class. Dropping any one of the refusals from the encode side — by discarding the error
// checkRecord forwards from the join, or by handing checkRecord a value it cannot judge —
// leaves that path with no witness and fails here.
//
// Two paths of EncodeRecord's own are outside what this gate names and are asserted
// elsewhere in this package: the nil record, in codec_test.go, and the writer's latched
// error on an over long field, in the ceiling test below. Both are EncodeRecord's and
// neither is reachable from checkRecord, which is the class this test's name claims.
//
// The witnesses are matched by the message each site renders, so the gate refuses to run
// if two sites cannot be told apart: a site witnessed by another site's refusal would
// report cover it does not have.
func TestEveryRefusalReachableFromCheckRecordIsOneEncodeRecordReaches(t *testing.T) {
	fileSet, funcs, sentinels := packageSyntax(t)
	sites := refusalSitesOf(t, fileSet, funcs, sentinels, "checkRecord", map[string]bool{})
	if len(sites) == 0 {
		t.Fatal("checkRecord has no refusal at all, so this gate would hold vacuously")
	}
	witnesses := make([]int, len(sites))
	for _, candidate := range encodeCandidates(t) {
		_, err := EncodeRecord(&candidate.record)
		if err == nil {
			continue
		}
		matched := []int{}
		for i, site := range sites {
			if site.pattern.MatchString(err.Error()) {
				matched = append(matched, i)
			}
		}
		switch len(matched) {
		case 0:
			t.Fatalf("%s: EncodeRecord refused with %q, which is no refusal checkRecord has", candidate.name, err)
		case 1:
			witnesses[matched[0]]++
		default:
			t.Fatalf("%s: %q matches %d of checkRecord's refusals at once (%s and %s), so a witness proves nothing about which path ran",
				candidate.name, err, len(matched), sites[matched[0]].where, sites[matched[1]].where)
		}
	}
	for i, site := range sites {
		if witnesses[i] == 0 {
			t.Errorf("%s can refuse at %s with %q and no record in the candidate space makes EncodeRecord do it, so that refusal is asserted by nothing",
				site.function, site.where, site.format)
			continue
		}
		t.Logf("%s %s %s witnessed %d times", site.function, site.where, site.sentinel, witnesses[i])
	}
}

// ── the four length prefixed fields ─────────────────────────────────────────────────

// The record fields EncodeRecord writes with a length prefix, in encode order, read off
// the calls themselves rather than named here.
func lpFieldsWritten(t testing.TB) []string {
	t.Helper()
	_, funcs, _ := packageSyntax(t)
	decl, found := funcs["EncodeRecord"]
	if !found {
		t.Fatal("EncodeRecord is not a function of this package")
	}
	fields := []string{}
	ast.Inspect(decl.Body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall || len(call.Args) != 1 {
			return true
		}
		fun, isSelector := call.Fun.(*ast.SelectorExpr)
		if !isSelector || fun.Sel.Name != "WriteOpaqueLP" {
			return true
		}
		argument, isField := call.Args[0].(*ast.SelectorExpr)
		if !isField {
			t.Fatal("EncodeRecord writes a length prefixed field this gate cannot name")
		}
		fields = append(fields, argument.Sel.Name)
		return true
	})
	if len(fields) == 0 {
		t.Fatal("EncodeRecord writes no length prefixed field, so both gates below would hold vacuously")
	}
	return fields
}

// The record fields decodeRecord hands through absentIfEmpty, and the number of length
// prefixed fields it reads. The two counts are compared by the caller, so a fifth LP field
// that skipped the canonicaliser is a failure rather than a field nobody thought to assert
// about.
func lpFieldsCanonicalised(t testing.TB) ([]string, int) {
	t.Helper()
	_, funcs, _ := packageSyntax(t)
	decl, found := funcs["decodeRecord"]
	if !found {
		t.Fatal("decodeRecord is not a function of this package")
	}
	fields := []string{}
	reads := 0
	ast.Inspect(decl.Body, func(node ast.Node) bool {
		if call, isCall := node.(*ast.CallExpr); isCall {
			if fun, isSelector := call.Fun.(*ast.SelectorExpr); isSelector && fun.Sel.Name == "ReadOpaqueLP" {
				reads++
			}
		}
		assign, isAssign := node.(*ast.AssignStmt)
		if !isAssign || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		call, isCall := assign.Rhs[0].(*ast.CallExpr)
		if !isCall {
			return true
		}
		fun, isIdent := call.Fun.(*ast.Ident)
		if !isIdent || fun.Name != "absentIfEmpty" {
			return true
		}
		target, isField := assign.Lhs[0].(*ast.SelectorExpr)
		if !isField {
			t.Fatal("decodeRecord canonicalises a field this gate cannot name")
		}
		fields = append(fields, target.Sel.Name)
		return true
	})
	return fields, reads
}

// The value of a named record field, wherever in the record it sits. Found by walking the
// struct rather than by knowing which fields live on the header, so a field that moved
// between the two is still found.
func lpFieldValue(t testing.TB, record *Record, name string) reflect.Value {
	t.Helper()
	var find func(value reflect.Value) (reflect.Value, bool)
	find = func(value reflect.Value) (reflect.Value, bool) {
		for i := range value.NumField() {
			field := value.Type().Field(i)
			if field.Name == name {
				return value.Field(i), true
			}
			if field.Type.Kind() == reflect.Struct {
				if found, is := find(value.Field(i)); is {
					return found, true
				}
			}
		}
		return reflect.Value{}, false
	}
	found, is := find(reflect.ValueOf(record).Elem())
	if !is {
		t.Fatalf("a record has no field named %s", name)
	}
	if found.Kind() != reflect.Slice {
		t.Fatalf("%s is a %s and not a length prefixed field", name, found.Kind())
	}
	return found
}

// An empty length prefixed field parses back as nil and not as a zero length slice.
//
// LP carries no representation for "absent": nil and an empty slice both encode to four
// zero octets, so nothing about the bytes distinguishes them and no round trip and no
// bytes.Equal ever will. What distinguishes them is the go value a caller is handed, and
// spec A section 5.1 says which one that is — BlobId nil off the blob rung, CtBody nil once
// pruned — so a server written against that shape tests for nil and gets the answer the
// spec promised. This is the only place that answer is observed.
//
// The field list is read off decodeRecord and checked against EncodeRecord's, so a length
// prefixed field added to the layout is covered by this the moment it is written.
func TestAnEmptyLengthPrefixedFieldParsesBackAsNilAndNotAnEmptySlice(t *testing.T) {
	written := lpFieldsWritten(t)
	canonicalised, reads := lpFieldsCanonicalised(t)
	if len(canonicalised) == 0 {
		t.Fatal("decodeRecord canonicalises no field, so this test would hold vacuously")
	}
	if len(canonicalised) != reads {
		t.Fatalf("decodeRecord reads %d length prefixed fields and canonicalises %d of them: %v", reads, len(canonicalised), canonicalised)
	}
	if !slices.Equal(slices.Sorted(slices.Values(written)), slices.Sorted(slices.Values(canonicalised))) {
		t.Fatalf("EncodeRecord writes %v with a length prefix and decodeRecord canonicalises %v; the two sides disagree about which fields those are", written, canonicalised)
	}

	empty := smallestLegalRecord(t)
	empty.Header.BlobId = nil
	empty.Header.ServerAttachment = nil
	empty.CtHead = nil
	empty.CtBody = nil
	parsed, err := parseBoth(t, "the empty record", mustEncode(t, "the empty record", &empty))
	if err != nil {
		t.Fatalf("a record whose length prefixed fields are all empty does not parse: %v", err)
	}
	for _, name := range canonicalised {
		value := lpFieldValue(t, parsed, name)
		if !value.IsNil() {
			t.Errorf("%s parsed back as a non nil %d byte slice, and spec A section 5.1 says an absent field is nil", name, value.Len())
		}
	}

	// and the other half of the canonicalisation: a field that carries bytes is handed
	// back holding them, so "absent is nil" cannot be satisfied by making everything nil.
	full := smallestLegalRecord(t)
	full.Header.ServerAttachment = fillBytes(serverAttachmentTag, 40)
	full.CtHead = fillBytes(ctHeadTag, 96)
	parsedFull, err := parseBoth(t, "the populated record", mustEncode(t, "the populated record", &full))
	if err != nil {
		t.Fatalf("a record with populated length prefixed fields does not parse: %v", err)
	}
	if !bytes.Equal(parsedFull.Header.ServerAttachment, full.Header.ServerAttachment) || !bytes.Equal(parsedFull.CtHead, full.CtHead) {
		t.Error("a populated length prefixed field did not survive the round trip")
	}
}

// No length prefixed field can be written past the writer's vector ceiling without
// EncodeRecord saying so, and a refusal never comes with bytes.
//
// The ceiling is the only bound on ct_head and server_attachment in this layer — neither
// has a length this package can derive, and spec A section 2.4 has the server call
// EncodeRecord on every record it hands back on the read path — so the one place an over
// long field is caught is the sticky error the writer latches, and the one place that error
// can be lost is the return at the bottom of EncodeRecord. Discarding it there hands a
// caller nil bytes and a nil error, which reads as a record that encoded to nothing.
//
// The other two fields are bounded by checkRecord long before they reach the writer, which
// is why this asserts a refusal for every field and the ceiling itself for at least one: a
// field list that grew a fifth entry nothing bounds would fail the first half, and a
// checkRecord that started bounding all four would fail the second.
func TestNoLengthPrefixedFieldIsWrittenPastTheVectorCeiling(t *testing.T) {
	fields := lpFieldsWritten(t)
	oversize := make([]byte, syntax.MaxVectorLength+1)
	ceilings := 0
	for _, name := range fields {
		record := smallestLegalRecord(t)
		lpFieldValue(t, &record, name).Set(reflect.ValueOf(oversize))
		bs, err := EncodeRecord(&record)
		if err == nil {
			t.Errorf("%s: EncodeRecord accepted a %d byte field and answered %d bytes, past the writer's %d byte ceiling",
				name, len(oversize), len(bs), syntax.MaxVectorLength)
			continue
		}
		if bs != nil {
			t.Errorf("%s: EncodeRecord refused with %v and still handed back %d bytes", name, err, len(bs))
		}
		if errors.Is(err, syntax.ErrLengthExceedsMax) {
			ceilings++
		}
		t.Logf("%s: refused with %v", name, err)
	}
	if ceilings == 0 {
		t.Errorf("no length prefixed field reached the writer's %d byte ceiling, so the only bound on ct_head is asserted by nothing", syntax.MaxVectorLength)
	}
}
