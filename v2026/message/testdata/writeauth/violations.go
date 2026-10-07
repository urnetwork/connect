//go:build ignore

// The positive control for the two syntax tree gates in message/writeauth_test.go: one
// file that commits every act they ban, so each rule can be shown to catch one rather
// than assumed to. A rule that reports nothing because it is broken and a rule that
// reports nothing because the tree is clean look identical from the outside, and both
// gates found nothing on the day they were written.
//
// It cannot reach the gates and the gates cannot reach it. The scans skip any directory
// named testdata unless a root names one outright, which only the controls do; the go
// tool never builds a testdata directory at all; and the build constraint above says so
// a second time for a reader who has only this file open. None of it compiles and none
// of it is how anything here is really written.
//
// The other halves of the control are here beside the violations and in the sibling file.
// Every rule has a function below that does the same thing correctly, so "not reported"
// means "allowed" rather than "the rules are asleep"; and documented.go holds every
// banned thing in prose alone, so a gate that matched comments would fail there instead
// of passing quietly.
package writeauth

import (
	"bytes"
	"crypto/subtle"
	"fmt"
	"slices"
	"strings"
)

// ── the read path reaching the write key ──

// The two labels, as constants, because a deriver that names one through a constant is
// the shape a grep misses and the shape the walk has to resolve.
const (
	fixtureWriteInfo = "write/v1"
	fixtureReadInfo  = "read/v1"
)

// The deriver the walk must find. Nothing tells the gate this function is the write key
// deriver: the class is computed from the label it names, so a fixture that renamed it
// would still be found and a real function that derived the write key under another name
// would be found too.
func WriteKey(storageRoot []byte) []byte { return fixtureExpand(storageRoot, "write/v1") }

// The read key, which names the other label and must therefore not be in the class.
func ReadKey(storageRoot []byte) []byte { return fixtureExpand(storageRoot, fixtureReadInfo) }

// The expansion both go through, naming neither label itself.
func fixtureExpand(secret []byte, info string) []byte { return append(secret, info...) }

// Three hops from the root to the deriver, which is the case a one level check misses.
func ComputeRequestAuthViaHelper(readKey []byte) []byte { return fixtureMac(fixturePickKey(readKey)) }

func fixturePickKey(readKey []byte) []byte { return WriteKey(readKey) }

func fixtureMac(key []byte) []byte { return key }

// The label inlined at the root, with no deriver called at all.
func ComputeRequestAuthByLabel(readKey []byte) []byte { return fixtureExpand(readKey, "write/v1") }

// The label reached through the package level constant, which is the same defect spelled
// so that the literal never appears in the function.
func ComputeRequestAuthByConst(readKey []byte) []byte {
	return fixtureExpand(readKey, fixtureWriteInfo)
}

// The clean root: the same three hop shape, ending at the read key. It is what says the
// walk reports a root because of what that root reaches rather than because it reports
// every root it is given.
func ComputeRequestAuthClean(readKey []byte) []byte { return fixtureMac(fixturePickReadKey(readKey)) }

func fixturePickReadKey(readKey []byte) []byte { return ReadKey(readKey) }

// ── verifiers that decide equality in variable time ──

// The named comparator, which is guardrail G8's own example.
func VerifyByBytesEqual(tag []byte, carried []byte) bool { return bytes.Equal(tag, carried) }

// The comparison the compiler offers for free on a fixed width tag, which is the shape a
// ban list of function names never sees.
func VerifyByArrayComparison(tag [32]byte, carried [32]byte) bool { return tag == carried }

// A verifier that reaches no constant time comparison anywhere, which is the other half
// of the rule: banning the wrong comparator is not the same as requiring the right one.
func VerifyWithNoComparisonAtAll(tag []byte) bool { return len(tag) == 32 }

// A verifier that is clean under both rules, so the ban is shown not to fire on every
// function it reads.
func VerifyClean(tag []byte, carried []byte) bool {
	return subtle.ConstantTimeCompare(tag, carried) == 1
}

// The same, with the comparison one hop away, which is what the transitive half of the
// rule is for: a verifier that delegates is still a verifier that compares.
func VerifyCleanThroughAHelper(tag []byte, carried []byte) bool { return fixtureSameTag(tag, carried) }

func fixtureSameTag(a []byte, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }

// ── the three the enumeration let through ──

// The mutant that lived. The constant time comparison still decides the answer, so this
// verifier agrees with VerifyClean on every input there is and no behavioural test can tell
// them apart; the fast path in front of it answers early on the first octet that differs,
// which is an oracle for how many leading octets of a forged tag were right. bytes.HasPrefix
// was on no ban list anyone wrote, and bytes.Equal was on all of them.
func VerifyByPrefixFastPath(tag []byte, carried []byte) bool {
	return bytes.HasPrefix(carried, tag) && subtle.ConstantTimeCompare(tag, carried) == 1
}

// The same act through a second package, because what makes a comparator is the shape and
// not the spelling: a generic equality over any slice answers exactly what bytes.Equal does.
func VerifyBySlicesEqual(tag []byte, carried []byte) bool { return slices.Equal(tag, carried) }

// And through a third, over the text of a tag rather than its octets, which is what a
// verifier that hex encoded both sides first would reach for.
func VerifyByStringsContains(tag string, carried string) bool {
	return strings.Contains(carried, tag)
}

// ── the one no comparator class can see ──

// A verifier that decides with the constant time comparison and then asks another package
// for a second opinion. fmt.Sprint answers no question about two byte strings, so it is in
// no derived comparator class and never will be; it is still a call out of the package
// inside the function whose answer is the authentication decision, and its argument is the
// tag. It is what says the rule over a verifier is that nothing foreign is called at all
// rather than that no comparator is.
func VerifyByForeignHelper(tag []byte, carried []byte) bool {
	return subtle.ConstantTimeCompare(tag, carried) == 1 && fmt.Sprint(tag) != ""
}
