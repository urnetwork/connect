// The mechanical half of convention C1 for this package: the byte level codec of an MLS
// structure is mls/syntax, reached through the two hooks syntax's own interfaces declare, and
// never a per type wrapper of this package's own.
//
// What this file is NOT is the whole of task 27's list, and the reason is worth writing down
// where the next person reads it. Most of what that task proposed to ban is already banned,
// by gates that DERIVE their class where the proposal enumerated one:
//
//   - hkdf.Extract and hkdf.Expand: crypto_forbidden_test.go confines every entry point
//     crypto/hkdf declares, by path, with a control fixture that calls each of them from an
//     allowed file, from a nested twin of one and from a file allowed nothing. That gate held
//     one name until this task and hkdf.Expand walked past it in ../message/writeauth.go.
//   - curve25519.ScalarMult, box.Precompute, GenerateSharedSecret, math/rand: the banned
//     primitive scan next door, plus TestTheCryptoIsBuiltFromExactlyThesePackages, which pins
//     the complete import set of mls AND message rather than banning a list of packages.
//   - bytes.Equal on a tag: TestEveryTagVerifierComparesThroughMacVerifyAndNothingElse reads
//     what a verifier ANSWERS rather than which words it contains, so a byte loop carrying no
//     comparator at all is reported too; and connect/message's G8 gate derives its comparator
//     class from the imports of the code it scans and finds eighteen, bytes.HasPrefix and
//     hmac.Equal among them.
//   - a redeclared ValSem sentinel: two things, neither of them a scan. psk.go carries the
//     three refusals as UNEXPORTED values precisely so the exported names stay p8's, and a
//     second declaration of an exported one in this package is a compile error the day p8
//     lands it. Until then all seven names sit in crossPlanSymbolsNotYetLanded, and
//     TestEveryCrossPlanSymbolThatHasLandedIsPinnedHere fails on the merge that lands any of
//     them rather than on somebody remembering.
//   - w.WriteBytes: TestConsumedSyntaxWriterShape pins syntax.Writer's method set, and a call
//     to a method that does not exist is a compile error either way.
//
// A fifth gate that duplicates a fourth reads as coverage and is not. What was left uncovered
// is the codec half, and that is what is here.
package mls

import (
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The verbs that name a byte level codec entry point in this tree.
//
// A PREFIX and not a substring: ConfirmedTranscriptHash is not a parser because it has the
// letters of one in the middle of it, and a rule that fired on the substring would be
// switched off by the first person it inconvenienced.
var keyScheduleCodecVerbs = []string{"Marshal", "Unmarshal", "Parse"}

// The subpackage the codec lives in, relative to this package's directory.
const keyScheduleCodecPackageDir = "syntax"

// syntaxCodecHooks is the sanctioned half of the rule: the codec entry points a type of this
// package MAY declare, read off the interfaces mls/syntax declares rather than written down.
//
// Derived because the exemption is the half that has been understated fourteen times on this
// project. If syntax grows a third hook -- an interface method that a structure implements to
// be encoded -- the types implementing it are sanctioned by that declaration rather than by
// somebody remembering to come back here, and if syntax RENAMES one, this stops recognising
// the old spelling and the rule starts objecting to it, which is the correct direction.
func syntaxCodecHooks(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(keyScheduleCodecPackageDir)
	if err != nil {
		t.Fatalf("read %s, where the sanctioned hooks are derived from: %v", keyScheduleCodecPackageDir, err)
	}
	hooks := []string{}
	read := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		read++
		parsed := mustParseSource(t, filepath.Join(keyScheduleCodecPackageDir, name))
		ast.Inspect(parsed.file, func(node ast.Node) bool {
			declared, isInterface := node.(*ast.InterfaceType)
			if !isInterface {
				return true
			}
			for _, method := range fieldsOf(declared.Methods) {
				if _, isFunctionType := method.Type.(*ast.FuncType); !isFunctionType {
					continue
				}
				for _, named := range method.Names {
					if keyScheduleIsCodecEntryPoint(named.Name) {
						hooks = append(hooks, named.Name)
					}
				}
			}
			return true
		})
	}
	slices.Sort(hooks)
	hooks = slices.Compact(hooks)
	if read == 0 {
		t.Fatalf("no non test file of %s was read, so the sanctioned set is empty and this gate would object to the codec itself",
			keyScheduleCodecPackageDir)
	}
	// the derivation's own positive control. These two are the hooks syntax.Marshaler and
	// syntax.Unmarshaler certainly declare, and a derivation that stopped deriving would
	// report every structure of this package as carrying a codec of its own.
	for _, hook := range []string{"MarshalMLS", "UnmarshalMLS"} {
		if !slices.Contains(hooks, hook) {
			t.Fatalf("the derivation read %v out of %s and %s is not among them, so it is reading no interface at all",
				hooks, keyScheduleCodecPackageDir, hook)
		}
	}
	return hooks
}

// Whether one declared name opens with a codec verb.
func keyScheduleIsCodecEntryPoint(name string) bool {
	return slices.ContainsFunc(keyScheduleCodecVerbs, func(verb string) bool {
		return strings.HasPrefix(name, verb)
	})
}

// keyScheduleCodecWrappersIn is every declaration of one file that is a byte level codec
// entry point and is not one of the sanctioned hooks, named with its receiver so a reader is
// told which type grew one.
//
// bodies is the second sanctioned exemption, and it is DERIVED rather than written down: the
// extension body types of the same source, read off the Encode() (Extension, error) signature
// by extensionBodyTypesIn. Extension.ExtensionData is opaque, so a concrete extension body has
// to convert bytes to and from a struct somewhere, and the spelling this package sanctions for
// that is Encode answering the whole Extension paired with ParseXExtension taking the bytes.
//
// The exemption is over the PAIR and not over the Parse name, which is the difference between
// a guarantee and a naming convention. A ParseFooExtension is waved through only when Foo's own
// Encode answers an Extension -- tag and body together, so the package never hands a loose body
// to a call site that could pair it with another extension's type -- and only when the Parse
// names the type it answers and takes nothing but the bytes. An Encode answering []byte hands
// the tag choice back to the caller and exempts nothing; a Parse whose name does not name its
// own result is not the pair either. The control below declares one of each, so an exemption
// that widened to cover them fails there rather than in a review.
//
// There is a THIRD sanctioned spelling by the time p7 lands, and it is not a read side at all.
// ParseRole maps the sdk's role NAME onto the wire byte MASTER section 11 assigns it; the
// interface registry pins that signature, so the name is not this file's to change, and a prefix
// rule reports it for the letters at the front of it. What is exempted is not the name: it is
// that the declaration is handed no byte run, answers none, and names no type this package has a
// codec for anywhere in its signature. A byte level codec has bytes on one side of it and a
// layout on the other, and this has neither, so there is nothing for it to be a second copy OF
// whatever somebody called it. The near miss is the same runless signature answering a structure
// syntax.Unmarshal already decodes; the control declares it and it stays reported.
//
// There are TWO sanctioned read side spellings and not one, and the second is exempt on a
// stricter reading than the first. ParseLeafKeysFrom is handed the whole Extension and answers
// the body, so the tag is in its hands and it is the only entry point that can refuse a body
// arriving under the wrong one. That shape carries no byte run in its signature at all, so it
// is not a byte level codec by any reading and only the NAME rule here was ever going to
// object to it. The body it belongs to is therefore read off its RESULT rather than off its
// name -- what sanctions the shape is the types, not what somebody called it -- and the near
// miss the control declares is the same shape answering a structure that is not an extension
// body, which is a second decoder for something whose codec is syntax.Unmarshal.
func keyScheduleCodecWrappersIn(parsed parsedSource, sanctioned []string, bodies []string,
	structures []string, byteRuns []string) []string {
	// the types this package HAS a codec for, in every spelling a signature can name one in.
	// A declaration that mentions one of these is laying that type out or taking it apart,
	// which is the thing convention C1 says happens in exactly one place.
	namesACodecType := func(rendered string) bool {
		return slices.ContainsFunc(slices.Concat(structures, bodies), func(one string) bool {
			return rendered == one || rendered == "*"+one ||
				rendered == "[]"+one || rendered == "[]*"+one
		})
	}
	exempt := map[string]bool{}
	for _, one := range declaredIn(parsed) {
		if one.receiver != "" || !strings.HasPrefix(one.name, "Parse") {
			continue
		}
		// the runless half: no byte run on either side of the signature and no type this
		// package has a codec for anywhere in it. Nothing byte level can be happening in a
		// declaration with no bytes to read and no structure to lay out, so the prefix is
		// all this rule ever had against it. The second clause is what keeps this off
		// ParseGroupContextFrom, whose input is not a run either and which IS a second
		// decoder for a structure syntax.Unmarshal owns.
		if !slices.ContainsFunc(slices.Concat(one.params, one.results), func(rendered string) bool {
			return keyScheduleIsByteRun(rendered, byteRuns) || namesACodecType(rendered)
		}) {
			exempt[one.name] = true
			continue
		}
		// the bytes taking half: the name states the body it answers, and it is handed
		// nothing but that body's bytes
		if body := strings.TrimPrefix(one.name, "Parse"); slices.Contains(bodies, body) &&
			slices.Equal(one.params, []string{"[]byte"}) &&
			slices.Equal(one.results, []string{"*" + body, "error"}) {
			exempt[one.name] = true
			continue
		}
		// the tag checked half: handed the whole entry and answering one of the derived
		// body types, with no byte run anywhere in the signature
		if len(one.results) == 2 && one.results[1] == "error" &&
			slices.Equal(one.params, []string{"Extension"}) &&
			strings.HasPrefix(one.results[0], "*") &&
			slices.Contains(bodies, strings.TrimPrefix(one.results[0], "*")) {
			exempt[one.name] = true
		}
	}
	found := []string{}
	for _, declaration := range parsed.file.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || !keyScheduleIsCodecEntryPoint(function.Name.Name) {
			continue
		}
		if slices.Contains(sanctioned, function.Name.Name) {
			continue
		}
		if function.Recv == nil && exempt[function.Name.Name] {
			continue
		}
		if receiver := parsed.receiverOf(function); receiver != "" {
			found = append(found, "("+receiver+")."+function.Name.Name)
			continue
		}
		found = append(found, function.Name.Name)
	}
	slices.Sort(found)
	return found
}

// mlsStructuresIn is the class the shape rule below is over: the types of the scanned source
// that are MLS structures, read off the source as the types that DECLARE a sanctioned hook.
//
// This is what convention C1 is actually about. A GroupContext has a wire encoding because it
// goes on the wire, and a KeySchedule does not; the difference is not a name and not a field
// list, it is whether the type implements the codec interfaces mls/syntax declares. Derived
// this way, a structure added later is covered by the commit that gives it MarshalMLS, and a
// type that is not on the wire is left alone -- which is what keeps this from reporting every
// accessor in the package that happens to answer a byte run.
func mlsStructuresIn(files []parsedSource, sanctioned []string) []string {
	structures := []string{}
	for _, parsed := range files {
		for _, one := range declaredIn(parsed) {
			if one.receiver != "" && slices.Contains(sanctioned, one.name) {
				structures = append(structures, strings.TrimPrefix(one.receiver, "*"))
			}
		}
	}
	slices.Sort(structures)
	return slices.Compact(structures)
}

// keyScheduleStructFieldsIn is the field names of every struct type the scanned source
// declares, which is what tells a codec from an accessor below.
func keyScheduleStructFieldsIn(files []parsedSource) map[string][]string {
	fields := map[string][]string{}
	for _, parsed := range files {
		ast.Inspect(parsed.file, func(node ast.Node) bool {
			named, isNamed := node.(*ast.TypeSpec)
			if !isNamed {
				return true
			}
			structure, isStruct := named.Type.(*ast.StructType)
			if !isStruct {
				return true
			}
			for _, field := range fieldsOf(structure.Fields) {
				for _, declared := range field.Names {
					fields[named.Name.Name] = append(fields[named.Name.Name], declared.Name)
				}
			}
			return true
		})
	}
	return fields
}

// keyScheduleIsByteRun answers whether one rendered type is a run of bytes: []byte itself, or
// a type the scanned source declares as one.
func keyScheduleIsByteRun(rendered string, named []string) bool {
	return rendered == "[]byte" || slices.Contains(named, rendered)
}

// keyScheduleSecondCodecsIn is the half of this rule that reads SHAPE rather than spelling:
// every declaration that converts an MLS structure to or from a byte run under a name
// carrying no codec verb at all.
//
// The verb rule next door is a prefix match, so a hand rolled codec written as
// groupContextFromBytes and encodeToWire is invisible to it -- confirmed by adding exactly
// that pair to group_context.go and watching the gate pass. Shape is the half that does not
// depend on what somebody called it.
//
// Two shapes, and each carries the discriminator that keeps it off legitimate code, because a
// signature test alone was measured to report JoinerSecret, WelcomeSecret, GroupContextBytes
// and (*HpkeContext).nonce:
//
//   - an ENCODER answers a byte run, is reached on an MLS structure, and names MORE THAN ONE
//     of that structure's fields. The field count is the discriminator and it is the property
//     itself: what makes a second encoder dangerous is that it lays several fields out in an
//     order, and what an accessor does is hand back one. (*GroupContext).treeHash is the
//     second shape and must not be reported; a method that reads the group id AND the tree
//     hash to build bytes is the first.
//   - a DECODER takes ONE byte run and nothing else, answers an MLS structure, and TAKES THE
//     RUN APART. The arity was the whole discriminator until p5 task 5: a decoder is handed
//     one opaque run where a constructor is handed the fields already separated, and
//     NewGroupContext(groupId, epoch, treeHash) is a constructor by that reading. Arity alone
//     stopped separating them the moment a structure arrived whose one variable field IS a
//     byte run: BasicCredential(identity) stores what it is handed and interprets nothing, and
//     it has the decoder's exact signature. keyScheduleTakesItsRunApart is the second half,
//     and it is three derived signals rather than a name: an error result, a branch, or a
//     subscript of the argument. Interpreting a run somebody else wrote requires at least one
//     of the three, and storing it whole matches none.
//
// What this still cannot see is a decoder that takes a byte run AND something else -- a
// provider, a version -- under a name with no verb in it. That shape is left uncovered rather
// than bought with a rule that reports every constructor in the package, and it is written
// here rather than left for the next reader to rediscover.
func keyScheduleSecondCodecsIn(parsed parsedSource, structures []string, byteRuns []string, fields map[string][]string) []string {
	namesAnMlsStructure := func(rendered string) bool {
		return slices.ContainsFunc(structures, func(one string) bool {
			return rendered == one || rendered == "*"+one || rendered == "[]"+one || rendered == "[]*"+one
		})
	}
	found := []string{}
	for _, one := range declaredIn(parsed) {
		answers := []string{}
		for _, result := range one.results {
			if result != "error" {
				answers = append(answers, result)
			}
		}
		if len(answers) != 1 {
			continue
		}
		receiver := strings.TrimPrefix(one.receiver, "*")
		encodes := slices.Contains(structures, receiver) &&
			keyScheduleIsByteRun(answers[0], byteRuns) &&
			keyScheduleFieldsNamedIn(one.body, fields[receiver]) > 1
		decodes := len(one.params) == 1 && keyScheduleIsByteRun(one.params[0], byteRuns) &&
			namesAnMlsStructure(answers[0]) && keyScheduleTakesItsRunApart(one)
		if !encodes && !decodes {
			continue
		}
		if one.receiver != "" {
			found = append(found, "("+one.receiver+")."+one.name)
			continue
		}
		found = append(found, one.name)
	}
	return found
}

// keyScheduleTakesItsRunApart answers whether one declaration does something with its argument
// that INTERPRETING an opaque run requires, as opposed to storing it.
//
// This is the half of the decoder shape that is not the signature, and it exists because the
// signature stopped separating a decoder from a constructor. It is stated as ONE property with
// four signals and not as three symptoms with a hole under them, because the first three were
// measured to leave one: a straight line body carrying no error, no branch and no subscript can
// still take a run apart, by handing it to something else that does the interpreting.
//
// Three of the signals are things reading a run somebody else wrote needs, any one of which is
// enough:
//
//   - an error result. A run arriving from the wire can be malformed and a decoder has to be
//     able to say so; convention C2 is that it says so by returning. Every hand rolled decoder
//     the control below declares returns one.
//   - a branch -- an if, a for, a range, a switch. Taking a structure out of a run means
//     deciding something about its content.
//   - a subscript or a slice expression. That is a field being cut out of the run by offset,
//     which is the straight line spelling of a decoder that reports nothing and branches on
//     nothing.
//
// The fourth is the COMPLEMENT of the exemption rather than another symptom, and it is what
// closes the hole the other three leave: unless the declaration stores its run whole, it is
// taking it apart. Two shapes walked past the first three and are declared in the control
// below -- Credential{CredentialType: binary.BigEndian.Uint16(b), Identity: b}, which reads a
// field out of the front of a run somebody else wrote while carrying none of the three, and a
// body that hands the run to syntax.Unmarshal and drops the error, which mentions the run once
// and stores nothing.
//
// A body this scan cannot read counts as taking the run apart, which is the safe direction: an
// unreadable body is reported rather than waved through.
func keyScheduleTakesItsRunApart(one sourceDeclaration) bool {
	if slices.Contains(one.results, "error") || one.body == nil {
		return true
	}
	apart := false
	ast.Inspect(one.body, func(node ast.Node) bool {
		switch node.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt,
			*ast.IndexExpr, *ast.SliceExpr:
			apart = true
		}
		return true
	})
	return apart || !keyScheduleStoresItsRunWhole(one)
}

// keyScheduleStoresItsRunWhole answers whether the ONLY use one declaration makes of the byte
// run it was handed is to put that run, whole, into a single field of a structure it builds.
//
// That is what BasicCredential does, and it is the entire reason the exemption exists: a
// structure whose one variable field IS a byte run has a constructor with a decoder's exact
// signature. Stating the exemption as what a constructor DOES, rather than as the absence of
// the ways somebody thought a decode could be spelled, is the difference between a class and a
// list -- there is no fourth and fifth symptom to keep adding here, because everything that is
// not storing the run whole is reported.
//
// Two clauses, and each is the property rather than a proxy for it:
//
//   - the run reaches at most ONE field. A run landing in two fields is being laid out across
//     them, which is a decode however it is spelled, and it is the mirror of the encoder half's
//     "names more than one of that structure's fields".
//   - every mention of the run in the body is inside that field's value. A mention anywhere
//     else is the run going somewhere this scan cannot follow -- into a call whose result is
//     dropped, into a second structure, into a local assembled by hand -- and none of those is
//     storing it.
//
// A run that is never mentioned stores nothing AND interprets nothing, so it is not taking its
// run apart: a declaration that ignores the bytes it was handed is not a decoder of them,
// whatever else it may be.
//
// The deliberate over report: a constructor that assembles its field from the run by hand, a
// make and a copy rather than this package's cloneBytes, mentions the run outside the field's
// value and is reported. That direction is correct and is not a wart. Nothing this scan can
// read separates such a constructor from a decoder that assembles a field from the run by
// hand, and the house spelling -- cloneBytes(run) as the field's value, which is what
// BasicCredential is written as -- is exempt, so the report is a request to write it the way
// this package already writes it.
func keyScheduleStoresItsRunWhole(one sourceDeclaration) bool {
	if len(one.paramNames) != 1 {
		return false
	}
	run := one.paramNames[0]
	if run == "" || run == "_" {
		// never named and therefore never used: nothing is taken apart
		return true
	}
	mentions := map[token.Pos]bool{}
	ast.Inspect(one.body, func(node ast.Node) bool {
		if identifier, isIdentifier := node.(*ast.Ident); isIdentifier && identifier.Name == run {
			mentions[identifier.Pos()] = true
		}
		return true
	})
	if len(mentions) == 0 {
		return true
	}
	// the composite literal fields whose value mentions the run, and the mentions those values
	// account for. A field is keyed by the literal it belongs to as well as by its name, so a
	// run stored into two different structures counts as two fields rather than collapsing into
	// one when both spell the field the same way.
	fields := map[string]bool{}
	stored := map[token.Pos]bool{}
	ast.Inspect(one.body, func(node ast.Node) bool {
		literal, isLiteral := node.(*ast.CompositeLit)
		if !isLiteral {
			return true
		}
		for at, element := range literal.Elts {
			name := fmt.Sprintf("%d", at)
			value := element
			if pair, isPair := element.(*ast.KeyValueExpr); isPair {
				if key, isIdentifier := pair.Key.(*ast.Ident); isIdentifier {
					name = key.Name
				}
				value = pair.Value
			}
			mentioned := false
			ast.Inspect(value, func(inner ast.Node) bool {
				if identifier, isIdentifier := inner.(*ast.Ident); isIdentifier && identifier.Name == run {
					stored[identifier.Pos()] = true
					mentioned = true
				}
				return true
			})
			if mentioned {
				fields[fmt.Sprintf("%d.%s", literal.Pos(), name)] = true
			}
		}
		return true
	})
	return len(fields) <= 1 && len(stored) == len(mentions)
}

// keyScheduleFieldsNamedIn counts how many DISTINCT fields of a type one body mentions.
func keyScheduleFieldsNamedIn(body *ast.BlockStmt, fields []string) int {
	if body == nil {
		return 0
	}
	named := map[string]bool{}
	ast.Inspect(body, func(node ast.Node) bool {
		if identifier, isIdentifier := node.(*ast.Ident); isIdentifier && slices.Contains(fields, identifier.Name) {
			named[identifier.Name] = true
		}
		return true
	})
	return len(named)
}

// syntaxCodecEntryPoints is the third sanctioned exemption to convention C1, and like the
// first two it is DERIVED: the top level functions mls/syntax declares that are handed a
// Marshaler or an Unmarshaler.
//
// It exists because the rule next door reads "byte level codec entry point" off a name and a
// shape, and both of those describe a function that is not a codec at all. p5 task 11's
// UnmarshalRatchetTree is the case that surfaced it: the ratchet_tree array of RFC 9420
// section 12.4.3.3 is the one structure p1 allows past MaxVectorLength, so its two entry
// points are syntax.MarshalLimit and syntax.UnmarshalLimit with the limit fixed -- one
// argument, chosen once, so no caller has to remember it. That is the sanctioned codec called
// correctly, and reporting it as a second one would leave the only alternatives a raised limit
// spelled out at every call site or a gate switched off.
//
// Derived off the SIGNATURE rather than off a list of four names, for syntaxCodecHooks'
// reason: if syntax grows or renames a top level entry point, the delegations to it move with
// the declaration rather than with somebody remembering to come back here. Marshaler and
// Unmarshaler are the interfaces a structure of this package implements, so a syntax function
// taking one of them is by construction a function that runs THIS package's own MarshalMLS or
// UnmarshalMLS, which is the whole of what C1 asks for.
func syntaxCodecEntryPoints(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(keyScheduleCodecPackageDir)
	if err != nil {
		t.Fatalf("read %s, where the sanctioned entry points are derived from: %v", keyScheduleCodecPackageDir, err)
	}
	points := []string{}
	read := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		read += 1
		parsed := mustParseSource(t, filepath.Join(keyScheduleCodecPackageDir, name))
		for _, one := range declaredIn(parsed) {
			if one.receiver != "" || !one.exported {
				continue
			}
			if slices.ContainsFunc(one.params, func(rendered string) bool {
				return rendered == "Marshaler" || rendered == "Unmarshaler"
			}) {
				points = append(points, one.name)
			}
		}
	}
	if read == 0 {
		t.Fatalf("no non test file of %s was read, so the delegation exemption is over an empty set",
			keyScheduleCodecPackageDir)
	}
	slices.Sort(points)
	points = slices.Compact(points)
	// the derivation's own positive control: these two certainly take a Marshaler and an
	// Unmarshaler, and a derivation that stopped deriving would exempt nothing and report
	// exactly what a complete one reports on this package as it stands today.
	for _, entry := range []string{"Marshal", "Unmarshal"} {
		if !slices.Contains(points, entry) {
			t.Fatalf("the derivation read %v out of %s and %s is not among them, so it is reading no signature at all",
				points, keyScheduleCodecPackageDir, entry)
		}
	}
	return points
}

// keyScheduleDelegationsIn is every declaration of one file that reaches the wire ONLY through
// one of those entry points, rendered the way the two rules above render what they report so
// the two sets can be subtracted.
//
// Three clauses, and each is a way of NOT delegating rather than a symptom of delegating, which
// is the difference between an exemption and a hole:
//
//   - it reaches one of the entry points at all. A declaration that never reaches syntax is
//     doing the encoding itself whatever it is called, which is every hand rolled codec the
//     control declares.
//   - it does nothing to the bytes but PLUMB them, on either side of THE call it delegates to.
//     A SECOND entry point call is caught by this same clause rather than by a count of its
//     own, and that is measured rather than assumed: spelled as a separate "exactly one"
//     clause it was a clause nothing could make fail, since a declaration that decodes one run
//     as two structures still has a call in it that is not the delegation. This is the
//     clause with the history, and it is stated as its COMPLEMENT for that reason. It was
//     first written as "carries no index and no slice expression", a lexical scan of the
//     declaration's own body, and one level of indirection walked straight past it:
//     syntax.Unmarshal(afterHeader(data), out) with func afterHeader(b []byte) []byte
//     { return b[4:] } next door is bit for bit the control's own reported near miss with the
//     slice moved one call away. It also said nothing about the ENCODE side at all -- a body
//     that comes back from syntax.Marshal and is then wrapped in a header of this
//     declaration's own carries an ellipsis rather than a slice expression, and framing an
//     encoder's output is the same defect as framing a decoder's input. So what is recognised
//     here is the set of things that MOVE a value without doing anything to it, and every
//     other node is reported. There is no fifth spelling of a cut to remember to add: a call
//     this declaration makes on its own account is not plumbing whatever it computes, and a
//     value it computes and throws away is not plumbing either -- which is the reporting half
//     of the rule, since the value a delegation must not throw away is the codec's refusal.
//   - it names no field of any MLS structure. Reading or writing a field around a delegation
//     is the structure being laid out or patched up outside its own codec, which is the
//     encoder half's own discriminator read the other way.
//
// The over report this is stated to accept: a delegation that legitimately calls something
// else -- a length check, a logger, a second helper -- is reported. That direction is correct
// and is not a wart. Nothing this scan can read separates such a call from one that frames the
// bytes, and the house spelling of a raised limit pair, which is the shape the exemption
// exists for, calls nothing at all.
//
// What this still cannot see is a delegation that checks the codec's error and then answers
// nil anyway. Separating that from a delegation that reports needs the value flow rather than
// the shape, and it is left uncovered here rather than bought with a rule that reports the
// sanctioned pair, which is written down rather than left for the next reader to rediscover.
func keyScheduleDelegationsIn(parsed parsedSource, entryPoints []string, structureFields []string) []string {
	delegating := []string{}
	for _, one := range declaredIn(parsed) {
		if one.body == nil || !slices.Contains(one.results, "error") {
			continue
		}
		reaches := keyScheduleEntryPointCallsIn(one.body, entryPoints)
		if len(reaches) == 0 || !keyScheduleOnlyPlumbsAround(one.body, reaches[0]) {
			continue
		}
		if keyScheduleFieldsNamedIn(one.body, structureFields) > 0 {
			continue
		}
		if one.receiver != "" {
			delegating = append(delegating, "("+one.receiver+")."+one.name)
			continue
		}
		delegating = append(delegating, one.name)
	}
	slices.Sort(delegating)
	return delegating
}

// keyScheduleEntryPointCallsIn is every call one body makes to a sanctioned syntax entry point,
// the calls themselves rather than a count, because the clause below has to be able to tell the
// one delegation apart from everything else in the same body.
func keyScheduleEntryPointCallsIn(body *ast.BlockStmt, entryPoints []string) []*ast.CallExpr {
	calls := []*ast.CallExpr{}
	ast.Inspect(body, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		selector, isSelector := call.Fun.(*ast.SelectorExpr)
		if !isSelector {
			return true
		}
		base, isIdentifier := selector.X.(*ast.Ident)
		if isIdentifier && base.Name == keyScheduleCodecPackageDir &&
			slices.Contains(entryPoints, selector.Sel.Name) {
			calls = append(calls, call)
		}
		return true
	})
	return calls
}

// keyScheduleOnlyPlumbsAround answers whether everything one body does APART from the entry
// point call is plumbing: moving a value without doing anything to it.
//
// The recognised set is the plumbing and everything else is reported, which is what makes this
// a class rather than a list -- the failure mode of the clause it replaces was that a list of
// the ways somebody had thought a cut could be spelled understated the ways it can be. Each
// entry is here because a delegation cannot be written without it:
//
//   - the statements a delegation is made of: a block, an if, a return, an assignment, and the
//     declarations an assignment may be spelled as. NOT an expression statement, which is a
//     value computed and dropped on the floor -- and the value dropped by the one call that
//     matters here is the codec's refusal.
//   - the expressions that NAME something: an identifier, a selector, a literal, parentheses,
//     a pointer or array type. The blank identifier is not among them, since assigning to it
//     is the other spelling of dropping a value.
//   - & applied to a composite literal with NO elements, which is how a decode target is made.
//     A literal with elements is a structure being laid out around the delegation, and every
//     other prefix operator computes something.
//   - == and != , which is the error check a delegation reports through. Every other binary
//     operator combines two values into a third, which is arithmetic on somebody's bytes.
//
// An index, a slice expression, an append, a helper call, a closure and a type conversion all
// fall through to the report, and none of them had to be named to get there.
func keyScheduleOnlyPlumbsAround(body *ast.BlockStmt, entry *ast.CallExpr) bool {
	plumbs := true
	ast.Inspect(body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case nil:
			return false
		case *ast.BlockStmt, *ast.IfStmt, *ast.ReturnStmt, *ast.AssignStmt, *ast.DeclStmt,
			*ast.GenDecl, *ast.ValueSpec, *ast.BasicLit, *ast.SelectorExpr, *ast.ParenExpr,
			*ast.StarExpr, *ast.ArrayType:
			return true
		case *ast.Ident:
			// the blank identifier is a value being discarded, which is what the reporting
			// half of this rule is about
			if typed.Name == "_" {
				plumbs = false
			}
		case *ast.UnaryExpr:
			if typed.Op != token.AND {
				plumbs = false
			}
		case *ast.BinaryExpr:
			if typed.Op != token.EQL && typed.Op != token.NEQ {
				plumbs = false
			}
		case *ast.CompositeLit:
			if len(typed.Elts) != 0 {
				plumbs = false
			}
		case *ast.CallExpr:
			if typed != entry {
				plumbs = false
			}
		default:
			plumbs = false
		}
		return plumbs
	})
	return plumbs
}

// keyScheduleStructureFieldsOf is the union of the field names of the MLS structures of the
// scanned source, which is the class the delegation exemption's fourth clause is over.
func keyScheduleStructureFieldsOf(structures []string, fields map[string][]string) []string {
	names := []string{}
	for _, structure := range structures {
		names = append(names, fields[structure]...)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// keyScheduleCodecControl declares one of each shape the rule has to tell apart: the two
// sanctioned hooks, the free constructor and the per type wrapper convention C1 bans, the two
// extension vector spellings that do not exist -- the codec is WriteExtensions/ReadExtensions
// -- and one name that carries a verb in the middle rather than at the front.
//
// The last of those is the half that keeps this from being a substring match. Without it the
// prefix test can be replaced by strings.Contains and the control goes on matching exactly,
// which is measured rather than supposed; with it, that edit reports a transcript hash
// accessor as a decoder and a reader learns to ignore the gate.
const keyScheduleCodecControl = `package control

type GroupContext struct {
	GroupId  []byte
	Epoch    uint64
	TreeHash []byte
}

type PreSharedKeyId struct{}

// the sanctioned pair, which is how every structure of this package is encoded
func (self *GroupContext) MarshalMLS(w *syntax.Writer) error { return nil }

func (self *GroupContext) UnmarshalMLS(r *syntax.Reader) error { return nil }

// the free constructor: a byte level decode that is not syntax.Unmarshal
func ParseGroupContext(b []byte) (*GroupContext, error) { return nil, nil }

func ParsePreSharedKeyId(b []byte) (*PreSharedKeyId, error) { return nil, nil }

// the per type wrapper: a byte level encode that is not syntax.Marshal
func (self *GroupContext) Marshal() []byte { return nil }

// the extension vector codec written under a name that does not exist
func MarshalExtensions(extensions []Extension) []byte { return nil }

func ParseExtensions(b []byte) ([]Extension, error) { return nil, nil }

// a verb in the middle of a name, which is not a codec entry point
func (self *GroupContext) ConfirmedTranscriptHashParsedElsewhere() []byte { return nil }

// the same two shapes again under names carrying no verb at all, which is what the prefix
// match cannot see and the shape rule is for
func groupContextFromBytes(b []byte) (*GroupContext, error) { return nil, nil }

func (self *GroupContext) encodeToWire() []byte {
	out := append([]byte(nil), self.GroupId...)
	return append(out, self.TreeHash...)
}

// a one field accessor on the same structure, which answers a byte run and is not a codec
func (self *GroupContext) treeHash() []byte { return self.TreeHash }

// a constructor: the fields already separated, and a structure out
func NewGroupContext(groupId []byte, epoch uint64, treeHash []byte) *GroupContext {
	return &GroupContext{GroupId: groupId, Epoch: epoch, TreeHash: treeHash}
}

// a constructor whose one variable field IS the byte run, so it has the decoder's exact
// signature and interprets nothing: no error to report a malformed run with, no branch on its
// content, no offset cut out of it. Not reported.
type Credential struct {
	CredentialType uint16
	Identity       []byte
}

func (self *Credential) MarshalMLS(w *syntax.Writer) error { return nil }

func (self *Credential) UnmarshalMLS(r *syntax.Reader) error { return nil }

func NewBasicCredential(identity []byte) Credential {
	return Credential{CredentialType: 1, Identity: identity}
}

// the same constructor written the way this package actually writes it: the run is copied
// before it is stored, because the caller's array is usually a slice of a buffer it goes on
// writing into. A defensive copy is still storing the run whole, so this is not reported
// either -- and pinning it here is what keeps a refinement of the exemption from quietly
// making BasicCredential itself a reported shape.
func NewBasicCredentialCopying(identity []byte) Credential {
	return Credential{CredentialType: 1, Identity: cloneBytes(identity)}
}

// and the near misses that constructor must not cover, all under names carrying no verb and
// all answering the same structure from one byte run. Every one of them is a second decoder
// and all of them are reported.
//
// The first three carry one of the decode symptoms each: one can report a malformed run, one
// branches on its content, one cuts a field out of it by offset. The two after them carry NONE
// of the three and are the reason the exemption is stated as what a constructor does rather
// than as the absence of those symptoms -- they were measured walking past the symptom-only
// version of this rule.
func credentialFromBytes(b []byte) (Credential, error) { return Credential{}, nil }

func credentialFromBytesBranching(b []byte) Credential {
	if len(b) == 0 {
		return Credential{}
	}
	return Credential{CredentialType: 1, Identity: b}
}

func credentialFromBytesSlicing(b []byte) Credential {
	return Credential{CredentialType: 1, Identity: b[2:]}
}

// a run read apart by somebody else's arithmetic: the first two octets of a run this package
// did not write become a field, and the rest becomes another. No error, no branch, no
// subscript -- the interpretation is inside a call -- and the run reaches two fields, which is
// laying a structure out and is exactly what a second decoder is.
func credentialFromBytesDecoding(b []byte) Credential {
	return Credential{CredentialType: binary.BigEndian.Uint16(b), Identity: b}
}

// a decode wrapper with the error swallowed: the run is handed to the real codec and the
// structure comes back through the argument, so the body mentions the run once, stores it
// nowhere, and carries none of the three symptoms. Swallowing the error is what removes the
// first of them, which is why the error result cannot be the whole rule.
func credentialFromBytesSwallowing(b []byte) *GroupContext {
	out := &GroupContext{}
	syntax.Unmarshal(b, out)
	return out
}

// the declared over report, kept here so the boundary is measured rather than remembered: a
// constructor that assembles its one field from the run by hand mentions the run outside the
// field's value and is reported, because nothing this scan can read separates it from a
// decoder that assembles a field by hand. cloneBytes is the spelling that is exempt.
func credentialFromBytesAssembling(b []byte) Credential {
	identity := make([]byte, len(b))
	copy(identity, b)
	return Credential{CredentialType: 1, Identity: identity}
}

// the second sanctioned exception: an extension body, whose Encode answers the whole
// Extension rather than the body's bytes, paired with the two Parse spellings that read it
// back -- the one handed the body's bytes, and the one handed the whole entry so that it
// has the tag to check. None of these three may be reported.
type LeafKeysExtension struct{}

func (self *LeafKeysExtension) Encode() (Extension, error) { return Extension{}, nil }

func ParseLeafKeysExtension(data []byte) (*LeafKeysExtension, error) { return nil, nil }

func ParseLeafKeysFrom(ext Extension) (*LeafKeysExtension, error) { return nil, nil }

// the near miss the tag checked exemption must not cover: the same shape answering a
// structure that is not an extension body at all, which is a second decoder for a type
// whose codec is syntax.Unmarshal. Reported.
func ParseGroupContextFrom(ext Extension) (*GroupContext, error) { return nil, nil }

// the third sanctioned spelling, which is not a read side at all: a Parse handed no byte
// run, answering none, and naming no type this package has a codec for. ParseRole maps a
// role NAME onto a wire byte and there is nothing byte level in it to be a second copy of.
// Not reported.
func ParseRole(name string) (Role, error) { return 0, nil }

// and the near miss THAT exemption must not cover: the same runless signature answering a
// structure whose codec is syntax.Unmarshal, which is a second decoder however its input
// happens to be spelled. Reported.
func ParseGroupContextByName(name string) (*GroupContext, error) { return nil, nil }

// the near miss the exemption must not cover: an Encode that answers the body's bytes
// instead of the Extension, which is the tag choice handed back to the caller and is the
// whole thing the exception is written to prevent. Its Parse is a second codec like any
// other and must be reported.
type GroupPolicyExtension struct{}

func (self *GroupPolicyExtension) Encode() ([]byte, error) { return nil, nil }

func ParseGroupPolicyExtension(data []byte) (*GroupPolicyExtension, error) { return nil, nil }

// and the pair broken on the other side: the Encode is the sanctioned one and the Parse
// takes something besides the body's bytes, so what it decodes is not one extension body
// and the exemption's shape does not describe it. Reported.
type OwnerSuccessorExtension struct{}

func (self *OwnerSuccessorExtension) Encode() (Extension, error) { return Extension{}, nil }

func ParseOwnerSuccessorExtension(data []byte, version uint16) (*OwnerSuccessorExtension, error) {
	return nil, nil
}

// the third sanctioned exception: the raised limit pair. The ratchet_tree array of RFC 9420
// section 12.4.3.3 is the one structure p1 allows past MaxVectorLength, so its two entry
// points are syntax.MarshalLimit and syntax.UnmarshalLimit with that limit fixed once. Both
// reach the wire only through syntax, report what it refused, cut nothing out of their
// arguments and name no field, so neither is a second codec. Not reported.
func MarshalGroupContextAtTheRaisedLimit(v *GroupContext) ([]byte, error) {
	return syntax.MarshalLimit(v, 1)
}

func UnmarshalGroupContextAtTheRaisedLimit(data []byte) (*GroupContext, error) {
	out := &GroupContext{}
	if err := syntax.UnmarshalLimit(data, out, 1); err != nil {
		return nil, err
	}
	return out, nil
}

// and the three near misses that exemption must not cover, one per clause it is stated in.
//
// The first frames on its own account: four octets cut off the front before the sanctioned
// decoder ever sees the run is a second length prefix, agreed with by nobody, and invisible
// to every round trip because both halves of it would agree. The second lays the structure
// out around the delegation, which is the field patched up outside its own codec. The third
// never reaches syntax at all and merely looks like it does. All three reported.
func ParseGroupContextAfterItsOwnHeader(data []byte) (*GroupContext, error) {
	out := &GroupContext{}
	if err := syntax.Unmarshal(data[4:], out); err != nil {
		return nil, err
	}
	return out, nil
}

func ParseGroupContextPatchingAField(data []byte) (*GroupContext, error) {
	out := &GroupContext{}
	if err := syntax.Unmarshal(data, out); err != nil {
		return nil, err
	}
	out.TreeHash = nil
	return out, nil
}

func ParseGroupContextByHand(data []byte) (*GroupContext, error) {
	if len(data) == 0 {
		return nil, nil
	}
	return &GroupContext{}, nil
}

// and the near misses the same exemption must not cover once a clause is read as a class
// rather than as a list of spellings. Every one of them reaches the sanctioned entry point,
// answers an error, and carries no index, no slice expression and no field of any MLS
// structure -- which is the whole of what the first version of the exemption asked. All
// reported.
//
// The first is ParseGroupContextAfterItsOwnHeader with the cut moved ONE CALL AWAY, which is
// the shape that measured the old clause: a lexical scan of the delegating declaration's own
// body cannot see b[4:] when b[4:] is next door. afterHeader itself is not reported and must
// not be -- it answers a byte run and no MLS structure, so it is not a codec by either rule --
// which is exactly why the delegation is the only place this can be caught.
func afterHeader(b []byte) []byte { return b[4:] }

func ParseGroupContextAfterItsOwnHeaderIndirect(data []byte) (*GroupContext, error) {
	out := &GroupContext{}
	if err := syntax.Unmarshal(afterHeader(data), out); err != nil {
		return nil, err
	}
	return out, nil
}

// the ENCODE side of the same thing, and the direction the exemption had no near miss for at
// all. A body that comes back from the sanctioned encoder and is then wrapped in a header of
// this declaration's own is a second framing agreed with by nobody, and it is invisible to a
// round trip because the matching read side would agree with it. body... is an ellipsis rather
// than a slice expression, so the clause that scanned for cuts never saw this shape even
// spelled out in one body.
func ownHeader() []byte { return []byte{0, 0, 0, 0} }

func MarshalGroupContextWithItsOwnHeader(v *GroupContext) ([]byte, error) {
	body, err := syntax.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(ownHeader(), body...), nil
}

// two delegations where a delegation is one. Decoding one run twice is this declaration
// deciding that the run holds two structures, which is a layout, and a layout decided outside
// a codec is what C1 is about. It cuts nothing, names no field and reports what either half
// refused; what reports it is that whichever call is read as the delegation, the OTHER one is
// a call this declaration makes on its own account, and that is not plumbing.
func ParseTwoGroupContexts(data []byte) (*GroupContext, error) {
	first := &GroupContext{}
	if err := syntax.Unmarshal(data, first); err != nil {
		return nil, err
	}
	second := &GroupContext{}
	if err := syntax.Unmarshal(data, second); err != nil {
		return nil, err
	}
	return first, nil
}

// and the two spellings of a delegation that does not REPORT. Both reach the sanctioned
// decoder, plumb their run into it untouched and name no field; both answer an error that is
// always nil, so whatever the codec refused is accepted here. credentialFromBytesSwallowing
// above is the same defect under a declaration that carries no error result at all, and it is
// caught by a different clause -- these two are what say the reporting clause is about the
// CALL rather than about the signature.
func ParseGroupContextDroppingTheRefusal(data []byte) (*GroupContext, error) {
	out := &GroupContext{}
	syntax.Unmarshal(data, out)
	return out, nil
}

func ParseGroupContextBlankingTheRefusal(data []byte) (*GroupContext, error) {
	out := &GroupContext{}
	_ = syntax.Unmarshal(data, out)
	return out, nil
}
`

// What the rule must read out of the control, exactly. Exact rather than a floor in both
// directions: a rule that widened to report the accessor would be switched off, and one that
// narrowed to miss the method wrapper would issue this package the clean bill a working one
// issues.
var keyScheduleCodecControlWrappers = []string{
	"(*GroupContext).Marshal",
	"(*GroupContext).encodeToWire",
	"MarshalExtensions",
	"MarshalGroupContextWithItsOwnHeader",
	"ParseExtensions",
	"ParseGroupContext",
	"ParseGroupContextAfterItsOwnHeader",
	"ParseGroupContextAfterItsOwnHeaderIndirect",
	"ParseGroupContextBlankingTheRefusal",
	"ParseGroupContextByHand",
	"ParseGroupContextByName",
	"ParseGroupContextDroppingTheRefusal",
	"ParseGroupContextFrom",
	"ParseGroupContextPatchingAField",
	"ParseGroupPolicyExtension",
	"ParseOwnerSuccessorExtension",
	"ParsePreSharedKeyId",
	"ParseTwoGroupContexts",
	"credentialFromBytes",
	"credentialFromBytesAssembling",
	"credentialFromBytesBranching",
	"credentialFromBytesDecoding",
	"credentialFromBytesSlicing",
	"credentialFromBytesSwallowing",
	"groupContextFromBytes",
}

// The p4-owned production files, held as a FLOOR on what the scan reads rather than as its
// scope.
//
// The plan wrote this list as the scope, and a scope written as seven file names is the
// exemption shape this project keeps rediscovering: the eighth file is outside it, and a
// wrapper written there passes. So the gate reads every non test file of the package and this
// list only asserts that the reading covered what the plan owns -- a file renamed or deleted
// fails here and says so, and a file ADDED is covered without an edit.
var keyScheduleOwnedFiles = []string{
	"errors_key_schedule.go",
	"group_context.go",
	"key_schedule.go",
	"psk.go",
	"secret_tree.go",
	"secret_zeroize.go",
	"transcript.go",
}

// TestNoTypeOfThisPackageCarriesAByteLevelCodecOfItsOwn is convention C1, enforced.
//
// The rule is one implementation of the wire format, and the reason is that a second one
// disagrees with the first eventually and never at the moment it is written. A hand rolled
// ParseGroupContext that reads the fields in the order the struct happens to declare them
// round trips its own output perfectly and disagrees with every other implementation on the
// wire; a Marshal() wrapper that forgets a vector's length prefix produces bytes this package
// decodes and no other does. Neither is visible to a round trip property, which is what makes
// this a source rule rather than a behavioural one.
//
// It is over every non test file of the package rather than over the seven the plan named,
// and the sanctioned exemption is read off mls/syntax rather than written here.
//
// TWO rules, unioned, because each sees what the other cannot. The verb rule reads the NAME
// and catches a wrapper over any type at all, including the free vector codecs no structure
// owns. The shape rule reads the SIGNATURE and the BODY and catches a codec written under a
// name that carries no verb -- groupContextFromBytes and encodeToWire were added to
// group_context.go and the verb rule alone passed. Neither is a superset of the other and
// both run over every file.
func TestNoTypeOfThisPackageCarriesAByteLevelCodecOfItsOwn(t *testing.T) {
	sanctioned := syntaxCodecHooks(t)
	entryPoints := syntaxCodecEntryPoints(t)
	codecsIn := func(parsed parsedSource, files []parsedSource) []string {
		fields := keyScheduleStructFieldsIn(files)
		structures := mlsStructuresIn(files, sanctioned)
		found := slices.Concat(
			keyScheduleCodecWrappersIn(parsed, sanctioned, extensionBodyTypesIn(files),
				structures, packageByteSliceTypeNamesIn(parsed)),
			keyScheduleSecondCodecsIn(parsed, structures,
				packageByteSliceTypeNamesIn(parsed), fields),
		)
		// the third exemption, subtracted from both rules at once because it is a fact
		// about the declaration rather than about which rule noticed it
		delegating := keyScheduleDelegationsIn(parsed, entryPoints,
			keyScheduleStructureFieldsOf(structures, fields))
		kept := []string{}
		for _, one := range found {
			if !slices.Contains(delegating, one) {
				kept = append(kept, one)
			}
		}
		slices.Sort(kept)
		return slices.Compact(kept)
	}

	// the control first, on both halves of both rules: every banned shape reported, and
	// neither the hooks, nor the one field accessor, nor the constructor, nor the name with a
	// verb in the middle of it.
	control := mustParseText(t, "the byte level codec control", keyScheduleCodecControl)
	if reported := codecsIn(control, []parsedSource{control}); !slices.Equal(reported, keyScheduleCodecControlWrappers) {
		t.Fatalf("the rule reported %v out of the control, want %v; a shape it lets through is a shape this package can be written in, and a shape it adds is one a reader will learn to ignore",
			reported, keyScheduleCodecControlWrappers)
	}

	// then this package's own source, every non test file of it
	scanned := packageLevelFunctions(t).files
	for _, owned := range keyScheduleOwnedFiles {
		if !slices.Contains(scanned, owned) {
			t.Fatalf("the scan read %v and %s is not among them, so this gate is not reading the files the key schedule lives in",
				scanned, owned)
		}
	}
	files := []parsedSource{}
	for _, path := range scanned {
		files = append(files, mustParseSource(t, path))
	}
	// the shape rule's class, derived, and checked for having derived something. An empty
	// structure set makes that half of the gate demand nothing, and a gate that demands
	// nothing reports exactly what a complete one reports.
	structures := mlsStructuresIn(files, sanctioned)
	if len(structures) == 0 {
		t.Fatalf("no type of this package declares any of %v, so the shape half of this gate is over an empty class and only the name half is running",
			sanctioned)
	}
	t.Logf("the MLS structures of this package, by the hooks they declare: %v", structures)
	for at, path := range scanned {
		for _, wrapper := range codecsIn(files[at], files) {
			t.Errorf("%s declares %s, and convention C1 says the byte level codec of an MLS structure is syntax.Marshal and syntax.Unmarshal reached through %v: a second encoder agrees with the first until it does not, and no round trip property can see the disagreement",
				path, wrapper, sanctioned)
		}
	}
}
