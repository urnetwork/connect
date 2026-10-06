// tests for the RFC 9420 section 9 secret tree: the descent, and the deletions that are
// the forward secrecy.
//
// Half of this file asks what a value IS, and that half is cheap: every implementation of
// section 9 that derives correctly agrees on it, including one that never deletes anything.
// The other half asks what is NO LONGER REACHABLE, which is the property the section exists
// for and the one nothing else in this package has needed yet. Where that question runs into
// what go can actually observe — a copy the runtime made, a value left in a register — the
// limit is written down in the comment rather than papered over with an assertion that would
// pass either way.
//
// What the plan's own six tests were measured unable to fail, run verbatim against fifteen
// single-edit mutants of secret_tree.go, each mutant confirmed applied by diff before its
// result was believed:
//
//   - TestSecretTreeLeafCount never failed against any of the fifteen. It builds one tree of
//     eight leaves and asks for eight back, so an accessor whose body is "return 8" satisfies
//     it. Replaced here by a sweep over every swept width, which fails against exactly that
//     mutant.
//   - TestSecretTreeRejectsOutOfRangeLeaf never failed against any of the fifteen, INCLUDING
//     the one that deletes the leaf-count range check outright. Its two probes, 8 and 1<<20,
//     are both refused by the node-array bound instead, so the check it exists to exercise can
//     be removed with the test still green -- and with it removed, leaf 2^31 wraps to node 0
//     and the tree hands out leaf 0's secret to a sender claiming an index no tree can hold.
//     The version here probes 1<<31, (1<<31)+4 and 0xffffffff, and fails against that mutant.
//   - TestSecretTreeDescentDerivesBothChildren does fail against a swapped label and a mirrored
//     descent, but not against the erasure moved one step too early -- the mutation its own
//     last assertion names. "leaf 7 is still reachable" is checked as err == nil, and a right
//     subtree derived from a run of zeros is still perfectly reachable. The version here
//     compares leaf 7's bytes.
//   - TestSecretTreeLeafSecretIsTakenOnce sees the map delete but not the erasure: it passes
//     against a tree that never calls zeroizeSecret at all, which is the half of the forward
//     secrecy claim in its own doc comment that it does not observe.
//
// No plan test failed against any of: never zeroizing a node secret, hardcoding the KDF width
// to 32, ignoring Root's error, or hardcoding the node-array width.
//
// A second round of measurement, over the file as it then stood, found the SCOPE of the two
// forward secrecy tests to be the defect rather than their assertions. Both asked what remains
// by naming the field tree.nodes, so a SecretTree that kept an un-zeroized copy of every
// destroyed node secret in a SECOND field passed all 750 tests of this tree -- the exact
// negation of the property this file exists for, and not hypothetical: p4 task 22 adds a
// second secret bearing field to this very struct. What replaces the field name is
// secretTreeRetainedBytes, which derives "what the tree still holds" from the TYPE, and the
// four further mutants that round measured as surviving are answered by
// TestSecretTreeCachedGeometryIsDerivedFromTheLeafCount (a width or a depth that is not the
// leaf count's) and TestSecretTreeTakesTheDeepestHeldAncestorAndNotTheShallowest. The two
// that remain unkillable are unreachable rather than unobserved, are labelled as such in
// secret_tree.go, and each has the invariant that makes it unreachable asserted here:
// TestSecretTreePathToLeafLandsOnTheTargetOfEveryLeafOfEveryWidth for the post descent landing
// check, and TestSecretTreeASecondTakeIsAlwaysTheConsumedRefusalAndNeverTheInvariantOne for
// the post loop consumed return.
package mls

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"go/ast"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

// stTestCrypto returns the ciphersuite 0x0003 provider the secret tree KATs use.
func stTestCrypto(t *testing.T) CryptoProvider {
	t.Helper()
	crypto, err := NewCryptoProvider(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("NewCryptoProvider: %v", err)
	}
	return crypto
}

const stVectorEncryptionSecret = "59227ed552e4a6db0779d43aea694fd1b2c2540e605a099b95cf852b41e8ea66"

// stSweptLeafCounts is every full leaf count this file sweeps. It is written as a range
// rather than a list so that "the sizes covered" is a bound and not a set somebody has to
// keep in step with the assertions: every power of two from one leaf to sixty-four.
func stSweptLeafCounts() []LeafCount {
	counts := []LeafCount{}
	for n := LeafCount(1); n <= 64; n *= 2 {
		counts = append(counts, n)
	}
	return counts
}

// stLeavesOf returns the leaf indices of a tree, derived from its node array's width rather
// than picked. Leaves are the even node indices below the width, so this is the same set the
// implementation has to reach and it is obtained from tree math rather than from a literal.
//
// The node to leaf step is p3's NodeIndex.LeafIndex and not a node/2 written here. The two
// agree on every even index and disagree on every odd one -- p3's REFUSES a parent index and
// a local division silently truncates it to the leaf on its left -- so a width that ever
// started at an odd node, or a step that stopped being two, is a failure here rather than a
// leaf set that looks the right size and names the wrong leaves.
func stLeavesOf(t *testing.T, leafCount LeafCount) []LeafIndex {
	t.Helper()
	width := NodeWidth(leafCount)
	if width == 0 {
		t.Fatalf("NodeWidth(%d) is zero, so this sweep would cover nothing", leafCount)
	}
	leaves := []LeafIndex{}
	for node := uint32(0); node < width; node += 2 {
		leaf, err := NodeIndex(node).LeafIndex()
		if err != nil {
			t.Fatalf("NodeIndex(%d).LeafIndex(): %v", node, err)
		}
		leaves = append(leaves, leaf)
	}
	if LeafCount(len(leaves)) != leafCount {
		t.Fatalf("width %d yields %d leaves for a count of %d", width, len(leaves), leafCount)
	}
	return leaves
}

// stExpectedNodeSecret derives one node's secret from the encryption secret independently of
// the code under test.
//
// It shares no arithmetic with the descent. pathToLeaf decides direction by comparing the
// target index against the node it is standing on; this walks p3's DirectPath UPWARD to
// learn the ancestry, replays it downward, and asks Left and Right at each step which child
// the next node actually is. So a descent that went to the sibling, or that labelled the
// left child "right", disagrees with this and cannot disagree with it in the same direction.
//
// The expansion length is read off the provider for the same reason the implementation reads
// it: both registered suites fix Nh at 32, so a literal here would agree with a literal there
// and the pair would be wrong together.
func stExpectedNodeSecret(t *testing.T, crypto CryptoProvider, encryptionSecret []byte,
	leafCount LeafCount, node NodeIndex) []byte {
	t.Helper()
	upward, err := DirectPath(node, leafCount)
	if err != nil {
		t.Fatalf("DirectPath(%d, %d): %v", node, leafCount, err)
	}
	chain := make([]NodeIndex, 0, len(upward)+1)
	for i := len(upward) - 1; i >= 0; i-- {
		chain = append(chain, upward[i])
	}
	chain = append(chain, node)
	root, err := Root(leafCount)
	if err != nil {
		t.Fatalf("Root(%d): %v", leafCount, err)
	}
	if chain[0] != root {
		t.Fatalf("the replay starts at node %d, want the root %d", chain[0], root)
	}
	secret := append([]byte(nil), encryptionSecret...)
	for i := 0; i+1 < len(chain); i++ {
		parent, child := chain[i], chain[i+1]
		left, err := Left(parent)
		if err != nil {
			t.Fatalf("Left(%d): %v", parent, err)
		}
		right, err := Right(parent)
		if err != nil {
			t.Fatalf("Right(%d): %v", parent, err)
		}
		switch child {
		case left:
			secret = crypto.ExpandWithLabel(secret, "tree", []byte("left"), crypto.HashSize())
		case right:
			secret = crypto.ExpandWithLabel(secret, "tree", []byte("right"), crypto.HashSize())
		default:
			t.Fatalf("node %d is neither child of %d", child, parent)
		}
	}
	return secret
}

// ---------------------------------------------------------------------------
// what the tree still holds, derived from its type rather than from a field name
// ---------------------------------------------------------------------------

// secretTreeHop is one step the byte walk took through a struct: the type that DECLARES the
// field, and the field's own name.
//
// The pair is what a gate over this walk needs and the rendered path cannot give it. A gate
// that reads this walk and then enforces over SOURCE has to enforce over the spelling the
// source uses, and the source spells a field of an inner type by that type's own field name:
// the ratchet secret is self.secret in every one of its seven methods and the word ratchets
// appears in none of them. Recording only where a path STARTS is how a class derived at the
// byte level ends up enforced one type short of the bytes it was derived from.
type secretTreeHop struct {
	holder string
	field  string
}

// secretTreeHeld is one byte bearing value a SecretTree still reaches, named by the path
// reflect walked to get to it and by every struct hop that path took.
type secretTreeHeld struct {
	where   string
	path    []secretTreeHop
	carried []byte
}

// secretTreeByteWalk is one walk in progress: what it has read, what it declined to read,
// and the pointers and maps it has already been through.
type secretTreeByteWalk struct {
	held     []secretTreeHeld
	unwalked []string
	seen     map[uintptr]bool
}

// secretTreeWalkDepthLimit bounds the walk so a cyclic shape the seen set does not cover --
// a slice holding itself, say -- reports rather than hangs. It is far past the depth of
// anything this package declares.
const secretTreeWalkDepthLimit = 24

// secretTreeIntegerRun reads a slice or array of a fixed width integer kind as one buffer of
// its elements' little endian bytes, and reports whether the element kind was one.
//
// It exists so that a secret held as []byte and a secret held as []uint32 -- or as [32]byte,
// which is neither -- are read the same way, rather than the first being read and the other
// two falling through a case written for byte slices. The bytes are copied out one element at
// a time because reflect refuses Bytes and Interface on a value reached through an unexported
// field; Uint and Int on an element are permitted, are reads and not handles, and need no
// unsafe.
func secretTreeIntegerRun(value reflect.Value) ([]byte, bool) {
	element := value.Type().Elem()
	signed := false
	switch element.Kind() {
	case reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uint, reflect.Uintptr:
	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Int:
		signed = true
	default:
		return nil, false
	}
	width := int(element.Size())
	out := make([]byte, 0, value.Len()*width)
	for at := range value.Len() {
		held := uint64(0)
		if signed {
			held = uint64(value.Index(at).Int())
		} else {
			held = value.Index(at).Uint()
		}
		for octet := range width {
			out = append(out, byte(held>>(8*octet)))
		}
	}
	return out, true
}

// at walks one value, recording the bytes it carries and reporting anything it could not walk
// at all.
//
// The kinds are handled as a class rather than as the shapes SecretTree happens to have
// today, which is the whole point of the helper this belongs to: a gate that reads the field
// named nodes reads nothing about a second field, and the reason to derive is that a second
// field is coming. Strings are read because a secret held in one is held no less. Scalars
// carry nothing that could hold a node secret and say so below. Anything left -- a chan, a
// func, an unsafe pointer -- is REPORTED rather than skipped, so a field of a kind this walk
// was not written for fails the gate instead of quietly leaving the walk smaller than the
// type it is meant to cover.
func (self *secretTreeByteWalk) at(where string, path []secretTreeHop, value reflect.Value, depth int) {
	if depth > secretTreeWalkDepthLimit {
		self.unwalked = append(self.unwalked, where+" is deeper than this walk goes")
		return
	}
	switch value.Kind() {
	case reflect.Slice, reflect.Array:
		if value.Kind() == reflect.Slice && value.IsNil() {
			return
		}
		if run, ok := secretTreeIntegerRun(value); ok {
			self.held = append(self.held, secretTreeHeld{where: where, path: path, carried: run})
			return
		}
		for at := range value.Len() {
			self.at(fmt.Sprintf("%s[%d]", where, at), path, value.Index(at), depth+1)
		}
	case reflect.String:
		self.held = append(self.held, secretTreeHeld{where: where, path: path, carried: []byte(value.String())})
	case reflect.Struct:
		for at := range value.NumField() {
			field := value.Type().Field(at)
			// the hop is recorded as the pair (declaring type, field name), and the slice is
			// clipped so the append allocates rather than writing into a sibling's backing
			// array. Two paths off one struct share this slice's array otherwise, and the
			// second field walked overwrites the first field's last hop.
			self.at(where+"."+field.Name,
				append(slices.Clip(path), secretTreeHop{holder: value.Type().String(), field: field.Name}),
				value.Field(at), depth+1)
		}
	case reflect.Pointer, reflect.Interface:
		if value.IsNil() {
			return
		}
		if value.Kind() == reflect.Pointer {
			if self.seen[value.Pointer()] {
				return
			}
			self.seen[value.Pointer()] = true
		}
		self.at(where+"->", path, value.Elem(), depth+1)
	case reflect.Map:
		if value.IsNil() {
			return
		}
		if self.seen[value.Pointer()] {
			return
		}
		self.seen[value.Pointer()] = true
		// KEYS as well as values: a table that remembered a destroyed secret by keying on it
		// holds that secret exactly as much as one that stored it. Sorted by the rendering
		// providerStructByteFields sorts by, because go randomises map iteration on purpose
		// and a walk that reported one value two different ways would make every count
		// derived from it flap.
		keys := value.MapKeys()
		slices.SortFunc(keys, func(a reflect.Value, b reflect.Value) int {
			return strings.Compare(secretTreeMapKeyOrder(a), secretTreeMapKeyOrder(b))
		})
		for _, key := range keys {
			rendered := secretTreeMapKeyOrder(key)
			self.at(where+"{"+rendered+"}", path, key, depth+1)
			self.at(where+"["+rendered+"]", path, value.MapIndex(key), depth+1)
		}
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		// a scalar is narrower than one node secret and cannot hold one. The claim is checked
		// rather than asserted: secretTreeRetainedBytes refuses a provider whose hash size
		// would fit inside a scalar, so this sentence cannot go quietly stale.
	default:
		self.unwalked = append(self.unwalked,
			where+" is a "+value.Kind().String()+" of type "+value.Type().String())
	}
}

// secretTreeMapKeyOrder renders one map key for the walk's ordering, widening
// providerMapKeyOrder over STRUCT keys.
//
// providerMapKeyOrder answers a struct with its type name, which is one string for every key
// of the map -- so the sort below has nothing to separate them by, and go randomises map
// iteration on purpose. p4 task 22's ratchets map is keyed by a struct, so without this the
// walk over a SecretTree reports its values in a different order on every run and every count
// derived from it flaps. The fields are read with reflect rather than through Interface,
// which panics on a value reached through an unexported field.
func secretTreeMapKeyOrder(key reflect.Value) string {
	if key.Kind() != reflect.Struct {
		return providerMapKeyOrder(key)
	}
	rendered := []string{key.Type().String()}
	for at := range key.NumField() {
		rendered = append(rendered, secretTreeMapKeyOrder(key.Field(at)))
	}
	return strings.Join(rendered, "/")
}

// secretTreeWalkOf reads every byte a value reaches, and reports what it could not walk.
func secretTreeWalkOf(value reflect.Value) ([]secretTreeHeld, []string) {
	walk := &secretTreeByteWalk{seen: map[uintptr]bool{}}
	walk.at(value.Type().String(), nil, value, 0)
	return walk.held, walk.unwalked
}

// secretTreeRetainedBytes is every byte a SecretTree still reaches, derived from its TYPE.
//
// This is the scope both forward secrecy tests below take "what remains" to mean, and it is
// written this way because the version that read tree.nodes was measured wrong. A SecretTree
// carrying an archive map[NodeIndex][]byte, filled with a copy of every parent secret one
// line before that parent is zeroized, keeps every ancestor of every consumed leaf alive for
// the whole epoch -- and passed all 750 tests of this package byte for byte, because both
// tests asked one named field what it held. p4 task 22 adds ratchets map[ratchetKey]*ratchet
// to this struct and each ratchet holds a secret, so the second secret bearing field is not a
// thought experiment; it is the next commit.
//
// The honest limit, stated at the same length as the claim. This reads what the IMPLEMENTATION
// still reaches, which is what go can observe and what an implementation can choose. It cannot
// observe a copy the RUNTIME made -- a growth that moved an array, a spill to a stack frame
// that is now garbage, a value still in a register -- and no go program can; secret_zeroize.go
// says the same thing at length. The comment these tests used to carry named only that second
// limit, which is true and was never the one that mattered: a copy the implementation kept is
// the one go observes trivially, and it was the one nothing looked for.
func secretTreeRetainedBytes(t *testing.T, tree *SecretTree) []secretTreeHeld {
	t.Helper()
	if tree == nil {
		t.Fatal("no tree to read, so this walk would report an empty retained state for the best reason and the worst one alike")
	}
	// the scalar claim in at(), checked rather than trusted: if a provider's hash size ever
	// fit inside one scalar, "a scalar cannot hold a node secret" would be false and this
	// walk would be skipping a container that could hold one.
	if nh := tree.crypto.HashSize(); nh <= 8 {
		t.Fatalf("the provider's hash size is %d bytes, which fits inside one scalar; this walk skips scalars on the grounds that it does not", nh)
	}
	held, unwalked := secretTreeWalkOf(reflect.ValueOf(tree).Elem())
	if len(unwalked) != 0 {
		t.Fatalf("this walk could not read %v, so what it reports as retained is smaller than the type; widen it, or say in its comment why that kind cannot carry a secret",
			unwalked)
	}
	if len(held) == 0 {
		t.Fatal("the walk read no bytes at all out of a secret tree, so every gate scoped by it would be green over a tree that retained everything")
	}
	return held
}

// secretTreeWalkControlLeaf is a struct behind a pointer, which is the shape a secret takes
// when it is held in a map of pointers -- p4 task 22's ratchets, one commit from now.
type secretTreeWalkControlLeaf struct {
	secret []byte
}

// secretTreeWalkControl hides one distinct needle in every container the walk claims to read,
// so that what the walk can SEE is measured rather than assumed.
//
// A walk that returned nothing reports exactly what a walk over a tree that retained nothing
// reports, which is the one outcome a gate like this must never reach by accident. Every field
// here is unexported, because the fields of SecretTree are and reflect refuses Bytes and
// Interface on values reached through them; a control written with exported fields would
// exercise a path the real walk never takes.
type secretTreeWalkControl struct {
	plain            []byte
	nested           struct{ inner []byte }
	fixed            [4]byte
	behindAPointer   *secretTreeWalkControlLeaf
	inAMap           map[NodeIndex][]byte
	inAMapOfPointers map[string]*secretTreeWalkControlLeaf
	asAMapKey        map[string]bool
	inASliceOfSlices [][]byte
	asText           string
	asWideIntegers   []uint32
	behindAnAny      any
	aScalar          int
	absent           []byte
	andAMutex        sync.Mutex
}

// TestSecretTreeRetainedByteWalkReadsEveryContainerItClaimsTo is the control on the walk every
// forward secrecy claim in this file is scoped by.
//
// The needles are distinct per container, so a walk that found one of them and reported it
// eleven times satisfies nothing here, and one needle is planted NOWHERE and required to be
// absent, so a walk that answered every query yes fails too.
func TestSecretTreeRetainedByteWalkReadsEveryContainerItClaimsTo(t *testing.T) {
	needle := func(tag byte) []byte { return bytes.Repeat([]byte{tag}, 32) }
	wide := []uint32{}
	for range 8 {
		wide = append(wide, 0x0b0b0b0b)
	}
	control := &secretTreeWalkControl{
		plain:            needle(0x01),
		fixed:            [4]byte{0x03, 0x03, 0x03, 0x03},
		behindAPointer:   &secretTreeWalkControlLeaf{secret: needle(0x04)},
		inAMap:           map[NodeIndex][]byte{7: needle(0x05)},
		inAMapOfPointers: map[string]*secretTreeWalkControlLeaf{"r": {secret: needle(0x06)}},
		asAMapKey:        map[string]bool{string(needle(0x07)): true},
		inASliceOfSlices: [][]byte{needle(0x08)},
		asText:           string(needle(0x09)),
		asWideIntegers:   wide,
		behindAnAny:      needle(0x0c),
		aScalar:          42,
	}
	control.nested.inner = needle(0x02)

	held, unwalked := secretTreeWalkOf(reflect.ValueOf(control).Elem())
	if len(unwalked) != 0 {
		t.Fatalf("the walk declined %v of a fixture built entirely out of the kinds it claims to handle", unwalked)
	}
	for _, want := range []struct {
		what    string
		carried []byte
	}{
		{what: "a plain byte slice", carried: needle(0x01)},
		{what: "a byte slice inside a nested struct", carried: needle(0x02)},
		{what: "a byte array", carried: []byte{0x03, 0x03, 0x03, 0x03}},
		{what: "a byte slice behind a pointer to a struct", carried: needle(0x04)},
		{what: "a value of a map of byte slices", carried: needle(0x05)},
		{what: "a byte slice behind a map of pointers to structs", carried: needle(0x06)},
		{what: "the key of a map keyed by the secret itself", carried: needle(0x07)},
		{what: "a byte slice inside a slice of byte slices", carried: needle(0x08)},
		{what: "a string", carried: needle(0x09)},
		{what: "a run of wider integers", carried: needle(0x0b)},
		{what: "a byte slice behind an interface", carried: needle(0x0c)},
	} {
		found := ""
		for _, one := range held {
			if bytes.Contains(one.carried, want.carried) {
				found = one.where
				break
			}
		}
		if found == "" {
			t.Errorf("the walk did not read %s, so a SecretTree holding a destroyed secret there would pass every gate scoped by it", want.what)
		}
	}
	// and the walk is not answering yes to everything: this needle is planted nowhere.
	for _, one := range held {
		if bytes.Contains(one.carried, needle(0xff)) {
			t.Fatalf("the walk reported a needle nothing planted, at %s, so it is matching rather than reading", one.where)
		}
	}
	// the order is the walk's own and must not depend on go's map iteration: two walks of one
	// value that disagreed would make every count derived from a walk flap.
	second, _ := secretTreeWalkOf(reflect.ValueOf(control).Elem())
	if len(second) != len(held) {
		t.Fatalf("two walks of one value read %d values and %d", len(held), len(second))
	}
	for at := range held {
		if held[at].where != second[at].where || !bytes.Equal(held[at].carried, second[at].carried) {
			t.Fatalf("two walks of one value disagree at position %d: %s and %s", at, held[at].where, second[at].where)
		}
	}
}

// TestNewSecretTreeRejectsBadInput asserts a wrong-length root secret, a zero leaf count and
// a leaf count that is not a power of two are refused, so a tree can never exist in a shape
// no leaf can be reached in.
//
// The non-full count is the one that is only refused because Root's error is handled rather
// than shimmed away (convention C3): a shim answering node zero for it would build a tree
// with a leaf for a root.
func TestNewSecretTreeRejectsBadInput(t *testing.T) {
	crypto := stTestCrypto(t)
	good := MustHex(t, stVectorEncryptionSecret)
	if _, err := NewSecretTree(crypto, 8, good[:31]); !errors.Is(err, ErrSecretLength) {
		t.Fatalf("short encryption secret err = %v, want ErrSecretLength", err)
	}
	if _, err := NewSecretTree(crypto, 8, append(append([]byte(nil), good...), 0x00)); !errors.Is(err, ErrSecretLength) {
		t.Fatalf("long encryption secret err = %v, want ErrSecretLength", err)
	}
	if _, err := NewSecretTree(crypto, 8, nil); !errors.Is(err, ErrSecretLength) {
		t.Fatalf("nil encryption secret err = %v, want ErrSecretLength", err)
	}
	if _, err := NewSecretTree(crypto, 0, good); !errors.Is(err, ErrSecretTreeLeafOutOfRange) {
		t.Fatalf("zero leaf count err = %v, want ErrSecretTreeLeafOutOfRange", err)
	}
	// every non-power-of-two count below the sweep's ceiling, derived rather than listed.
	refused := 0
	for n := LeafCount(1); n <= 64; n++ {
		if IsFullLeafCount(n) {
			continue
		}
		_, err := NewSecretTree(crypto, n, good)
		if !errors.Is(err, ErrSecretTreeLeafOutOfRange) {
			t.Fatalf("leaf count %d err = %v, want ErrSecretTreeLeafOutOfRange", n, err)
		}
		// p3's own sentinel survives the wrap, so a caller can ask which of the two range
		// failures this was rather than only that it was one of them.
		if !errors.Is(err, ErrLeafCountNotFull) {
			t.Fatalf("leaf count %d err = %v, want it to wrap ErrLeafCountNotFull too", n, err)
		}
		refused++
	}
	// 64 counts in 1..64, of which 1, 2, 4, 8, 16, 32 and 64 are full.
	if refused != 57 {
		t.Fatalf("refused %d non-full counts in 1..64, want 57", refused)
	}
	// the nil provider, at a sentinel of its own. It answered ErrSecretLength once, and this
	// test locked that in: a caller branching on a length failure re-derives and re-passes
	// its encryption_secret, which is the wrong repair for an argument it never supplied.
	if _, err := NewSecretTree(nil, 8, good); !errors.Is(err, ErrNilCryptoProvider) {
		t.Fatalf("nil provider err = %v, want ErrNilCryptoProvider rather than a nil dereference", err)
	}
	if _, err := NewSecretTree(nil, 8, good); errors.Is(err, ErrSecretLength) {
		t.Fatalf("nil provider err = %v, which still answers to ErrSecretLength; the two conditions "+
			"call for different repairs and a caller cannot tell them apart", err)
	}
}

// TestSecretTreeLeafCount asserts the accessor reports what was built, at the count type
// tree math defines. A LeafIndex here would compile and be wrong.
func TestSecretTreeLeafCount(t *testing.T) {
	crypto := stTestCrypto(t)
	encryptionSecret := MustHex(t, stVectorEncryptionSecret)
	for _, n := range stSweptLeafCounts() {
		tree, err := NewSecretTree(crypto, n, encryptionSecret)
		if err != nil {
			t.Fatalf("NewSecretTree(%d): %v", n, err)
		}
		var got LeafCount = tree.LeafCount()
		if got != n {
			t.Fatalf("LeafCount = %d, want %d", got, n)
		}
	}
}

// TestSecretTreeSingleLeafRootIsTheLeaf asserts that in a one-leaf tree the root node and
// leaf 0 are the same node, so the encryption secret is the leaf secret with no intervening
// "tree"/"left" derivation.
func TestSecretTreeSingleLeafRootIsTheLeaf(t *testing.T) {
	crypto := stTestCrypto(t)
	encryptionSecret := MustHex(t, stVectorEncryptionSecret)
	tree, err := NewSecretTree(crypto, 1, encryptionSecret)
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	leafSecret, err := tree.takeLeafSecret(0)
	if err != nil {
		t.Fatalf("takeLeafSecret: %v", err)
	}
	if !bytes.Equal(leafSecret, encryptionSecret) {
		t.Fatalf("leaf secret = %x, want the encryption secret %x", leafSecret, encryptionSecret)
	}
}

// TestSecretTreeDescentDerivesBothChildren asserts a descent toward leaf 0 in an eight-leaf
// tree produces exactly the RFC's "tree"/"left" and "tree"/"right" expansions, and that
// leaf 7 is still reachable afterwards — and holds the right value — because the sibling
// subtree secret was retained and was derived before its parent was erased.
func TestSecretTreeDescentDerivesBothChildren(t *testing.T) {
	crypto := stTestCrypto(t)
	encryptionSecret := MustHex(t, stVectorEncryptionSecret)
	tree, err := NewSecretTree(crypto, 8, encryptionSecret)
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}

	nh := crypto.HashSize()
	left := crypto.ExpandWithLabel(encryptionSecret, "tree", []byte("left"), nh)
	leftLeft := crypto.ExpandWithLabel(left, "tree", []byte("left"), nh)
	wantLeaf0 := crypto.ExpandWithLabel(leftLeft, "tree", []byte("left"), nh)

	got, err := tree.takeLeafSecret(0)
	if err != nil {
		t.Fatalf("takeLeafSecret(0): %v", err)
	}
	if !bytes.Equal(got, wantLeaf0) {
		t.Fatalf("leaf 0 secret = %x, want %x", got, wantLeaf0)
	}

	// the plan's version of this test asked only that leaf 7 still answered. That passes
	// against a descent that erased the root between its two children, because the right
	// subtree would then be a consistent tree derived from zeros. What separates them is the
	// value, so the value is what is compared.
	rightRight := crypto.ExpandWithLabel(
		crypto.ExpandWithLabel(encryptionSecret, "tree", []byte("right"), nh),
		"tree", []byte("right"), nh)
	wantLeaf7 := crypto.ExpandWithLabel(rightRight, "tree", []byte("right"), nh)
	leaf7, err := tree.takeLeafSecret(7)
	if err != nil {
		t.Fatalf("leaf 7 became unreachable after descending to leaf 0: %v", err)
	}
	if !bytes.Equal(leaf7, wantLeaf7) {
		t.Fatalf("leaf 7 secret = %x, want %x", leaf7, wantLeaf7)
	}
}

// TestSecretTreeDescentReachesEveryLeafOfEveryWidth is the descent's real coverage: every
// leaf of every full tree from one to sixty-four leaves, with the leaf set taken from the
// node array's width rather than chosen, and every answer compared against an independent
// replay of the ancestry p3's DirectPath reports.
//
// The pairwise distinctness at the end is what catches a descent that reaches A leaf rather
// than THE leaf: two leaves answering the same bytes is the shape a swapped Left/Right or a
// mis-signed comparison takes when the expectation happens to be symmetric.
func TestSecretTreeDescentReachesEveryLeafOfEveryWidth(t *testing.T) {
	crypto := stTestCrypto(t)
	encryptionSecret := MustHex(t, stVectorEncryptionSecret)
	compared := 0
	for _, n := range stSweptLeafCounts() {
		seen := map[string]LeafIndex{}
		for _, leaf := range stLeavesOf(t, n) {
			// a fresh tree per leaf, so this measures the descent and not the order the
			// previous leaves happened to consume the path in. The order is measured
			// separately below.
			tree, err := NewSecretTree(crypto, n, encryptionSecret)
			if err != nil {
				t.Fatalf("NewSecretTree(%d): %v", n, err)
			}
			got, err := tree.takeLeafSecret(leaf)
			if err != nil {
				t.Fatalf("takeLeafSecret(%d) in a %d leaf tree: %v", leaf, n, err)
			}
			want := stExpectedNodeSecret(t, crypto, encryptionSecret, n, leaf.NodeIndex())
			if !bytes.Equal(got, want) {
				t.Fatalf("leaf %d of %d = %x, want %x", leaf, n, got, want)
			}
			if other, ok := seen[string(got)]; ok {
				t.Fatalf("leaves %d and %d of a %d leaf tree have the same secret", other, leaf, n)
			}
			seen[string(got)] = leaf
			compared++
		}
	}
	// one comparison per leaf of each swept width: 1+2+4+8+16+32+64.
	if compared != 127 {
		t.Fatalf("compared %d leaves, want 127", compared)
	}
}

// stTakeOrders is every order this file consumes a tree's leaves in: forward, reverse, and
// outside in, which interleaves the two halves and so alternates which subtree consecutive
// takes descend into.
//
// One helper rather than one loop per test, so a test written later cannot silently cover
// fewer orders than the one beside it. Each order is checked to be a permutation of the leaf
// set rather than assumed to be one: an order that named a leaf twice would consume one leaf
// and report a sweep of the tree.
func stTakeOrders(t *testing.T, leaves []LeafIndex) [][]LeafIndex {
	t.Helper()
	forward := append([]LeafIndex(nil), leaves...)
	reverse := []LeafIndex{}
	for i := len(leaves) - 1; i >= 0; i-- {
		reverse = append(reverse, leaves[i])
	}
	interleaved := []LeafIndex{}
	for i, j := 0, len(leaves)-1; i <= j; i, j = i+1, j-1 {
		interleaved = append(interleaved, leaves[i])
		if i != j {
			interleaved = append(interleaved, leaves[j])
		}
	}
	orders := [][]LeafIndex{forward, reverse, interleaved}
	for _, order := range orders {
		if len(order) != len(leaves) {
			t.Fatalf("an order over %d leaves has %d entries", len(leaves), len(order))
		}
		seen := map[LeafIndex]bool{}
		for _, leaf := range order {
			if seen[leaf] {
				t.Fatalf("an order names leaf %d twice, so it consumes one leaf and reports a sweep of the tree", leaf)
			}
			seen[leaf] = true
		}
	}
	return orders
}

// TestSecretTreeEveryLeafSurvivesEveryTakeOrder asserts that consuming the leaves of one
// tree in any order gives every leaf the same secret a fresh tree gives it.
//
// This is the assertion the deletion has to survive. An erasure one step too early, or a
// parent erased before its second child is written, leaves the tree internally consistent —
// the remaining subtree still derives cleanly from whatever it was seeded with — so the only
// thing that sees it is a comparison against a derivation that never deleted anything.
func TestSecretTreeEveryLeafSurvivesEveryTakeOrder(t *testing.T) {
	crypto := stTestCrypto(t)
	encryptionSecret := MustHex(t, stVectorEncryptionSecret)
	compared := 0
	for _, n := range stSweptLeafCounts() {
		leaves := stLeavesOf(t, n)
		for _, order := range stTakeOrders(t, leaves) {
			tree, err := NewSecretTree(crypto, n, encryptionSecret)
			if err != nil {
				t.Fatalf("NewSecretTree(%d): %v", n, err)
			}
			for _, leaf := range order {
				got, err := tree.takeLeafSecret(leaf)
				if err != nil {
					t.Fatalf("takeLeafSecret(%d) of %d: %v", leaf, n, err)
				}
				want := stExpectedNodeSecret(t, crypto, encryptionSecret, n, leaf.NodeIndex())
				if !bytes.Equal(got, want) {
					t.Fatalf("leaf %d of %d taken in this order = %x, want %x", leaf, n, got, want)
				}
				compared++
			}
			// the tree is empty once every leaf has been taken: nothing was retained that
			// no leaf will ever ask for again.
			if len(tree.nodes) != 0 {
				t.Fatalf("a %d leaf tree retained %d node secrets after every leaf was taken", n, len(tree.nodes))
			}
		}
	}
	if compared != 3*127 {
		t.Fatalf("compared %d takes, want %d", compared, 3*127)
	}
}

// TestSecretTreeParentSecretIsGoneOnceBothChildrenExist is the deletion, observed rather
// than assumed.
//
// What "gone" can honestly mean in go, stated because the name of this test promises more
// than the language delivers. The assertion below reads the SAME backing array the tree
// handed to zeroizeSecret, so what it observes is that the bytes at that address are zero
// and that the map no longer names them. It cannot observe a copy the runtime made — a
// growth that moved the array, a spill to a stack frame that is now garbage, a value still
// in a register — and no go program can. secret_zeroize.go says the same thing at length and
// this test claims exactly what that file claims and nothing more.
//
// The other half of the claim is that both children existed BEFORE the parent went, and that
// is what the two value comparisons hold: the retained right subtree and the left subtree's
// surviving leaf both equal a derivation from the live root secret, which they could not if
// the root had been zeroized between the two stores.
func TestSecretTreeParentSecretIsGoneOnceBothChildrenExist(t *testing.T) {
	crypto := stTestCrypto(t)
	encryptionSecret := MustHex(t, stVectorEncryptionSecret)
	const leafCount = LeafCount(8)
	tree, err := NewSecretTree(crypto, leafCount, encryptionSecret)
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	held, ok := tree.nodes[tree.root]
	if !ok {
		t.Fatalf("the constructor did not seed the root")
	}
	if len(held) != crypto.HashSize() {
		t.Fatalf("the root secret is %d bytes, want %d", len(held), crypto.HashSize())
	}
	// the control. Without it "held is all zeros" is satisfied by a tree that never held
	// anything, and the assertion would be measuring nothing.
	if bytes.Equal(held, make([]byte, len(held))) {
		t.Fatalf("the root secret was already zero before the descent, so this test measures nothing")
	}
	// what the root secret WAS, kept for the sweep at the end. held is about to be erased in
	// place, so a comparison against it afterwards would be a comparison against zeroes.
	rootSecret := append([]byte(nil), held...)

	if _, err := tree.takeLeafSecret(0); err != nil {
		t.Fatalf("takeLeafSecret(0): %v", err)
	}

	if _, ok := tree.nodes[tree.root]; ok {
		t.Fatalf("the root secret is still held after both of its children were derived")
	}
	if !bytes.Equal(held, make([]byte, len(held))) {
		t.Fatalf("the root's bytes are %x, want them erased in place", held)
	}
	// and gone from the TYPE, not only from the map and the one backing array this test
	// happened to keep a handle on. The handle says the bytes at that address were erased; it
	// says nothing about a copy the tree took before erasing them, which is one field away and
	// which every correctness test in this file agrees with. Measured: a tree that archived
	// each parent one line before zeroizing it passed all 750 tests of this package.
	for _, one := range secretTreeRetainedBytes(t, tree) {
		if bytes.Contains(one.carried, rootSecret) {
			t.Fatalf("the root secret is still held at %s after both of its children were derived", one.where)
		}
	}

	right, err := Right(tree.root)
	if err != nil {
		t.Fatalf("Right(root): %v", err)
	}
	retained, ok := tree.nodes[right]
	if !ok {
		t.Fatalf("the root's right child was not retained, so the erasure took the subtree with it")
	}
	if want := stExpectedNodeSecret(t, crypto, encryptionSecret, leafCount, right); !bytes.Equal(retained, want) {
		t.Fatalf("the retained right child = %x, want %x — it was not derived from the live root secret", retained, want)
	}
	// the left child is on the path and was itself expanded and erased, so what stands for
	// it is a leaf underneath it.
	leaf1, err := tree.takeLeafSecret(1)
	if err != nil {
		t.Fatalf("takeLeafSecret(1): %v", err)
	}
	if want := stExpectedNodeSecret(t, crypto, encryptionSecret, leafCount, LeafIndex(1).NodeIndex()); !bytes.Equal(leaf1, want) {
		t.Fatalf("leaf 1 = %x, want %x", leaf1, want)
	}
}

// secretTreeExercised builds one tree and drives it through each thing the type does, so a
// derivation over the state it is then in reads every storage the type HAS rather than the
// one a freshly constructed tree happens to have filled.
//
// A tree straight out of the constructor holds node secrets and no ratchets, and a class
// derived from that state would be one field wide -- which is the shape of the defect this
// whole gate exists for, one level up: a question answered about the state somebody looked
// at rather than about the type.
func secretTreeExercised(t *testing.T, encryptionSecret []byte) *SecretTree {
	t.Helper()
	tree, err := NewSecretTree(stTestCrypto(t), LeafCount(8), encryptionSecret)
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	// a descent, which leaves the node map holding the antichain the take did not consume
	if _, err := tree.takeLeafSecret(0); err != nil {
		t.Fatalf("takeLeafSecret(0): %v", err)
	}
	// and a sender step of each kind, which is what creates a leaf's two ratchets
	for _, kind := range []RatchetType{RatchetHandshake, RatchetApplication} {
		if _, _, _, err := tree.NextSenderKey(1, kind); err != nil {
			t.Fatalf("NextSenderKey(1, %d): %v", kind, err)
		}
	}
	// and an ACCEPTED receive of a skipped generation, which is the only thing that leaves a
	// ratchet's retention window holding anything. Without it every window of the exercised tree
	// is empty, the walk reports no location under one, and a class derived by differencing this
	// state is silent about the AEAD key and nonce a window holds -- which is the storage
	// eraseKey zeroizes and the storage the commit exists to spend.
	//
	// IT IS THE COMMIT AND NOT THE LOOKUP THAT RETAINS, and the lookup alone no longer does:
	// MessageKey is a peek that writes nothing, so a version of this helper that stopped at the
	// lookup would leave every window empty and take the whole window class out of the gate
	// below without failing anything itself.
	if _, _, err := tree.MessageKey(ContentTypeApplication, 2, 3); err != nil {
		t.Fatalf("MessageKey(application, leaf 2, generation 3): %v", err)
	}
	if err := tree.CommitMessageKey(ContentTypeApplication, 2, 3); err != nil {
		t.Fatalf("CommitMessageKey(application, leaf 2, generation 3): %v", err)
	}
	return tree
}

// secretTreeSecretStorage is the class the gate below is over, in the two halves a SOURCE
// scan needs it in: the field spellings a secret bearing location hangs off, and the types of
// this package that declare them.
//
// Both halves come off the same differing paths. fields is what the source names the storage
// by -- every hop of the path, not the first one -- and holders is what tells a declaration
// that can hold one of those fields from one that merely shares a spelling with it.
type secretTreeSecretStorage struct {
	fields  []string
	holders []string
}

// secretTreeSecretBearingStorage is the storage a *SecretTree still reaches whose retained
// bytes are a FUNCTION OF the encryption secret.
//
// Derived by differencing two trees rather than named, and differenced rather than merely
// walked, because the walk on its own over-reports. crypto reaches the suite's parameters,
// whose Name is a run of bytes no walk can tell from a key, and putting the provider in this
// class would drag every method that touches self.crypto into the reaching set and turn the
// report into noise a reader learns to ignore. Two trees built from two different encryption
// secrets and driven through the same sequence differ in exactly the storage that carries key
// material and agree everywhere else, which is what "holds a secret" actually means -- asked
// of the implementation rather than guessed from a field name.
//
// A field added later that holds derived key material joins this class on the run that adds
// it, with no edit here. One that holds a constant does not, and should not.
//
// EVERY HOP of a differing path is read, and that is the correction this function is. The
// version it replaces cut each path at the first field off *SecretTree, so a location inside
// a *ratchet was reported under the spelling "ratchets" -- a word that appears in none of
// that type's seven methods. The class was derived at the byte level and enforced at the
// identifier level and the two did not meet: those seven methods, holding six of the nine
// erase sites in secret_tree.go, sat outside a class the byte walk had already put inside it.
// The holders are read off the same hops, because without them the deep spellings collide
// with unrelated parameters elsewhere in the package.
func secretTreeSecretBearingStorage(t *testing.T) secretTreeSecretStorage {
	t.Helper()
	nh := stTestCrypto(t).HashSize()
	carriedBy := func(fill byte) map[string]secretTreeHeld {
		held := map[string]secretTreeHeld{}
		for _, one := range secretTreeRetainedBytes(t, secretTreeExercised(t, bytes.Repeat([]byte{fill}, nh))) {
			held[one.where] = one
		}
		return held
	}
	first, second := carriedBy(0x11), carriedBy(0x22)
	if len(first) == 0 || len(first) != len(second) {
		t.Fatalf("the two exercised trees retained %d and %d locations; they are driven identically and must agree on WHERE they hold bytes",
			len(first), len(second))
	}
	// the package qualifier the walk renders a type with, asked of a type rather than written
	// down, so a package rename leaves this trimming a qualifier rather than trimming nothing
	qualifier := strings.TrimSuffix(reflect.TypeOf((*SecretTree)(nil)).Elem().String(), "SecretTree")
	storage := secretTreeSecretStorage{}
	for where, held := range first {
		other, alsoHeld := second[where]
		if !alsoHeld {
			t.Fatalf("the second tree holds nothing at %s and the first one does, so the two runs are not the same sequence and what differs below would be about the driving rather than about the secret",
				where)
		}
		if bytes.Equal(held.carried, other.carried) {
			continue
		}
		if len(held.path) == 0 {
			t.Fatalf("the byte walk reported %s with no struct hop on it, so no field name can be read out of it and the class below would be missing this location",
				where)
		}
		for _, hop := range held.path {
			storage.fields = append(storage.fields, hop.field)
			// a hop through a type of ANOTHER package is not storage this package's source
			// can be scanned for. None exists today; a field of an imported struct type
			// holding derived key material would be one, and leaving it out here is what
			// keeps the holder set to types whose declarations this scan can actually read.
			if name, ofThisPackage := strings.CutPrefix(hop.holder, qualifier); ofThisPackage {
				storage.holders = append(storage.holders, name)
			}
		}
	}
	slices.Sort(storage.fields)
	storage.fields = slices.Compact(storage.fields)
	slices.Sort(storage.holders)
	storage.holders = slices.Compact(storage.holders)
	return storage
}

// theDeclarationsThatCanHoldTheseTypes is every declaration of this package's source that can
// hold a value of one of the named types, and everything those declarations run.
//
// It is the other half of "can this declaration reach the tree's storage", and the half
// theNamesReachingTheStorage cannot supply. That closure runs CALLER-ward over a spelling, so
// seeding it with a field of an inner type pulls in every unrelated parameter of that name in
// the package: measured, suiteCryptoProvider.Mac comes back as leaking the secret tree's key,
// because its own key parameter is spelled the same and it hands what it reaches to hmac.
// This closure runs CALLEE-ward from the values instead, and the intersection of the two is
// the honest reading of the question -- a declaration that can neither be handed one of these
// values nor be run by something that was cannot put one beyond its call.
//
// The seed is a SIGNATURE test rather than a receiver test, because a constructor holds the
// value it is about to answer with and never has it as a receiver: NewSecretTree is what puts
// the encryption secret into the node map, and a seed of methods alone leaves it outside
// every scan. Receiver, parameters and results are all read, so a free function handed a
// *ratchet is inside it too.
func theDeclarationsThatCanHoldTheseTypes(declared []sourceDeclaration, holders []string) []string {
	inside := map[string]bool{}
	for _, one := range declared {
		rendered := slices.Concat([]string{one.receiver}, one.params, one.results)
		if slices.ContainsFunc(holders, func(holder string) bool {
			return slices.ContainsFunc(rendered, func(one string) bool { return secretTreeNamesType(one, holder) })
		}) {
			inside[one.name] = true
		}
	}
	declares := map[string]bool{}
	for _, one := range declared {
		declares[one.name] = true
	}
	for {
		grew := false
		for _, one := range declared {
			if !inside[one.name] || one.body == nil {
				continue
			}
			ast.Inspect(one.body, func(node ast.Node) bool {
				identifier, isIdentifier := node.(*ast.Ident)
				if isIdentifier && declares[identifier.Name] && !inside[identifier.Name] {
					inside[identifier.Name] = true
					grew = true
				}
				return true
			})
		}
		if !grew {
			return slices.Sorted(maps.Keys(inside))
		}
	}
}

// secretTreeNamesType answers whether one rendered type expression names a given type.
//
// A WHOLE WORD match over the rendering, because map[ratchetKey]*ratchet names two types and
// a substring test cannot tell ratchet from ratchetKey. The rendering is walked rather than
// compared, because a type appears inside a map, a slice, a pointer and a variadic without
// being the whole of what was written.
func secretTreeNamesType(rendered string, wanted string) bool {
	for at := 0; at+len(wanted) <= len(rendered); at++ {
		found := strings.Index(rendered[at:], wanted)
		if found < 0 {
			return false
		}
		at += found
		before := at == 0 || !secretTreeIsIdentifierByte(rendered[at-1])
		after := at+len(wanted) == len(rendered) || !secretTreeIsIdentifierByte(rendered[at+len(wanted)])
		if before && after {
			return true
		}
	}
	return false
}

// secretTreeIsIdentifierByte answers whether one byte can appear inside a go identifier,
// which is what the whole-word match above needs and strings has no test for.
func secretTreeIsIdentifierByte(b byte) bool {
	return b == '_' || ('0' <= b && b <= '9') || ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z')
}

// TestNoDeclarationReachingTheSecretTreeStoragePutsItBeyondTheCall is section 9's forward
// secrecy asked about the PROCESS rather than about the type.
//
// TestSecretTreeParentSecretIsGoneOnceBothChildrenExist above asks what remains reachable
// through *SecretTree. That is the right question about the type and it is only about the
// type: a copy taken on the way past and parked in a PACKAGE LEVEL variable answers it
// exactly as a correct tree does. The map no longer names the parent, the bytes at that
// address are zero, the walk over the type finds nothing -- and the secret is still in the
// process for as long as the program runs. Measured rather than supposed: a two line archive
// beside the zeroize, with the archive declared at package scope, passes all 750 tests of
// this package.
//
// So this gate reads the SOURCE, and asks of every declaration that can reach the tree's key
// storage whether it puts what it reaches somewhere the call does not end. It is the same
// scan guardrail 6 runs over the key schedule's epoch secret, pointed at different storage,
// and that is why the control it runs first is that gate's control: the two share the
// machinery, so a refinement made for either one that stopped reporting a way out would leave
// both of them green. The machinery could not be pointed here until that refinement landed --
// the scan read ANY call to a foreign callee as an escape, which is right for a key schedule
// that calls nothing it does not declare and reports eight false positives on a secret tree,
// whose stateLock's Lock and Unlock are foreign, are handed nothing and can carry nothing.
//
// WHICH DECLARATIONS ARE SCANNED is two closures intersected, and the first version of this
// gate had only one of them. The class is derived at the BYTE level, by a walk that follows
// the tree's map of pointers down into *ratchet and on into *generationKeys; it was then
// enforced at the IDENTIFIER level over the single spelling "ratchets", a word that appears in
// none of that inner type's seven methods. Six of the nine erase sites in secret_tree.go were
// outside the class the walk had already put inside it, and three archive mutants -- one on
// the ratchet secret in step, one in zeroize, one on the AEAD key in eraseKey -- survived all
// 824 tests of this package and ../message. Reading every hop of the path closes it, and the
// second closure is what makes reading every hop affordable: the deep spellings collide with
// unrelated parameters across the package, and a caller-ward closure over "key" alone reports
// suiteCryptoProvider.Mac handing an hmac what it reaches.
func TestNoDeclarationReachingTheSecretTreeStoragePutsItBeyondTheCall(t *testing.T) {
	// the shared analysis first, over the shared control, on both halves. Without it the
	// refinement that made this gate possible is also the thing that could make it vacuous.
	control := []parsedSource{mustParseText(t, "the epoch secret escape control", epochSecretEscapeControl)}
	controlReaching := theNamesReachingTheStorage(declaredAcross(control), epochSecretStorageField)
	if !slices.Equal(controlReaching, epochSecretEscapeControlReaching) {
		t.Fatalf("the closure read %v out of the shared control, want %v",
			controlReaching, epochSecretEscapeControlReaching)
	}
	controlEscaping := theDeclarationsPuttingWhatTheyReachBeyondTheCall(
		control, controlReaching, epochSecretStorageField)
	if !slices.Equal(controlEscaping, epochSecretEscapeControlEscaping) {
		t.Fatalf("the escape scan read\n%v\nout of the shared control, want\n%v\na way out it no longer reports there is a way out it no longer reports over the secret tree either",
			controlEscaping, epochSecretEscapeControlEscaping)
	}

	storage := secretTreeSecretBearingStorage(t)
	// the derivation's own positive control: a FLOOR on each half, not the class.
	//
	// nodes and ratchets are what a secret tree IS. secret is what a ratchet is, and window is
	// where a ratchet holds the message keys the receive path retains -- both of them one type
	// down, and both of them absent from this gate's first version, which cut every path at
	// its first hop. Naming them is what stops it going back: the mutation that reads
	// held.path[:1] again derives [nodes ratchets] and fails here rather than passing over a
	// *ratchet nothing scans. window is also the one member the exercise has to work for, so a
	// tree driven without a skipped receive fails here too.
	//
	// ratchet is the same floor on the other half: the type the walk has to descend INTO to
	// reach any of it.
	//
	// None of these is the class. The class is whatever differs, and a fifth field or a fourth
	// type joins it by existing rather than by an edit here.
	for _, known := range []string{"nodes", "ratchets", "secret", "window"} {
		if !slices.Contains(storage.fields, known) {
			t.Fatalf("the differential derived %v as the secret bearing storage of a secret tree and %s is not among them, so this gate is over a class that has stopped deriving",
				storage.fields, known)
		}
	}
	if !slices.Contains(storage.holders, "ratchet") {
		t.Fatalf("the differential crossed %v on its way to the secret bearing bytes of a secret tree and ratchet is not among them, so the walk is no longer descending past the tree's own fields and every spelling below it is outside this gate",
			storage.holders)
	}

	files := []parsedSource{}
	for _, path := range packageLevelFunctions(t).files {
		files = append(files, mustParseSource(t, path))
	}
	declared := declaredAcross(files)
	holding := theDeclarationsThatCanHoldTheseTypes(declared, storage.holders)
	t.Logf("%d declarations can hold one of %v, or are run by one that can: %v",
		len(holding), storage.holders, holding)
	// and the scope's own floor, one member for each of the two things it does. NewSecretTree
	// has no receiver and is the declaration that puts the encryption secret into the node
	// map, so a seed narrowed to receivers loses it and every archive written there stops
	// being scanned. zeroizeSecret names no type of this package at all and is inside only
	// because something that holds one runs it, so a closure that stopped closing loses it --
	// and it is where every erase in this file ends up.
	for _, known := range []string{"NewSecretTree", "zeroizeSecret"} {
		if !slices.Contains(holding, known) {
			t.Fatalf("the scope read %v as the declarations that can hold one of %v, and %s is not among them, so it is no longer reading both the values a signature names and the bodies those declarations run",
				holding, storage.holders, known)
		}
	}

	scanned := map[string]bool{}
	escaping := map[string][]string{}
	for _, field := range storage.fields {
		reaching := theNamesReachingTheStorage(declared, field)
		if len(reaching) == 0 {
			t.Fatalf("no declaration of this package's source mentions %s, so the class below is empty and this gate demands nothing for it",
				field)
		}
		scoped := []string{}
		for _, one := range reaching {
			if slices.Contains(holding, one) {
				scoped = append(scoped, one)
				scanned[one] = true
			}
		}
		if len(scoped) == 0 {
			t.Fatalf("%d declarations mention %s and none of them can hold one of %v, so this gate scans nothing for that storage; the two closures are no longer meeting",
				len(reaching), field, storage.holders)
		}
		t.Logf("%d declarations reach the secret tree's %s and can hold one: %v", len(scoped), field, scoped)
		escaping[field] = theDeclarationsPuttingWhatTheyReachBeyondTheCall(files, scoped, field)
	}

	// what the two closures together ended up covering, logged rather than asserted on. It is
	// the reader's check that the finding this gate was rewritten for is closed: every method
	// of *ratchet is here, and none of them was in the version that read the first hop of each
	// path. It is not written as an assertion because there is no non-circular one to write --
	// a method that names a class field is in the caller-ward closure by naming it and in the
	// scope by its receiver, so any test over the class's own members passes by construction.
	// The floors above are what actually hold the derivation in place.
	t.Logf("%d declarations are scanned for at least one of %v: %v",
		len(scanned), storage.fields, slices.Sorted(maps.Keys(scanned)))

	for _, field := range storage.fields {
		for _, one := range escaping[field] {
			t.Errorf("%s -- it reaches the secret tree's %s and puts what it reaches somewhere the call does not end: storage this package's own call did not introduce, another goroutine, or code this package did not write. Section 9's forward secrecy is that a consumed secret is gone from the PROCESS, and a copy parked outside the tree is invisible to every gate that reads the tree",
				one, field)
		}
	}
}

// TestSecretTreeDeletedSecretsAreNotDerivableFromWhatRemains is the property the deletion
// exists for: after a leaf has been taken, no sequence of derivations over the secrets the
// tree still holds reaches any secret it destroyed.
//
// The closure is computed rather than sampled. Derivation in section 9 goes one way — a node
// secret expands into its two children and nothing expands into a parent — so the full set an
// attacker holding the tree's remaining state can compute is exactly the subtrees under the
// retained nodes, and that set is walked in full and compared against every destroyed value.
func TestSecretTreeDeletedSecretsAreNotDerivableFromWhatRemains(t *testing.T) {
	crypto := stTestCrypto(t)
	encryptionSecret := MustHex(t, stVectorEncryptionSecret)
	const leafCount = LeafCount(8)
	tree, err := NewSecretTree(crypto, leafCount, encryptionSecret)
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	taken, err := tree.takeLeafSecret(0)
	if err != nil {
		t.Fatalf("takeLeafSecret(0): %v", err)
	}

	// everything the take was supposed to destroy: leaf 0's own node and every ancestor of
	// it, named by tree math rather than by hand.
	target := LeafIndex(0).NodeIndex()
	ancestors, err := DirectPath(target, leafCount)
	if err != nil {
		t.Fatalf("DirectPath: %v", err)
	}
	destroyed := map[string]NodeIndex{}
	for _, node := range append([]NodeIndex{target}, ancestors...) {
		destroyed[string(stExpectedNodeSecret(t, crypto, encryptionSecret, leafCount, node))] = node
	}
	if len(destroyed) != len(ancestors)+1 {
		t.Fatalf("two of the destroyed nodes have the same secret, so this test cannot tell them apart")
	}
	// the taken leaf's own secret is one of them, which is what says the map holds the real
	// values and not a parallel mistake.
	if _, ok := destroyed[string(taken)]; !ok {
		t.Fatalf("the secret takeLeafSecret returned is not the one this test derived for leaf 0")
	}

	// what remains, derived from the TYPE and not from the name of a field. The version of
	// this test that walked `for node := range tree.nodes` passed against a tree that kept an
	// un-zeroized copy of every destroyed secret in a second field; secretTreeRetainedBytes
	// carries the measurement.
	held := secretTreeRetainedBytes(t, tree)
	nh := crypto.HashSize()

	// first, the trivial half the field-scoped version could not ask: nothing the tree still
	// reaches CONTAINS a destroyed secret. Containment and not equality, because a buffer
	// that concatenated several of them would hold every one.
	for _, one := range held {
		for secret, node := range destroyed {
			if bytes.Contains(one.carried, []byte(secret)) {
				t.Fatalf("node %d's secret is still held, at %s", node, one.where)
			}
		}
	}
	// the control on that sweep: the walk reaches the node map. Without it the loop above is
	// satisfied by a walk that read nothing at all.
	for node, secret := range tree.nodes {
		found := false
		for _, one := range held {
			if bytes.Equal(one.carried, secret) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("the walk did not report node %d's retained secret, so what it calls the retained state is not this tree's", node)
		}
	}

	// and then the closure over it. Derivation in section 9 goes one way -- a node secret
	// expands into its two children and nothing expands into a parent -- so expanding every
	// retained buffer with both labels, to the tree's own depth, is the whole of what an
	// attacker holding this state can compute.
	//
	// The seeds are the retained buffers whose length is the PROVIDER's hash size, which is
	// the only length a node secret has and the only length this provider expands from. A
	// retained buffer of any other length is judged by the containment sweep above and not by
	// this closure, which is a real limit of the closure and is why both halves are here.
	reachable := map[string]string{}
	frontier := []secretTreeHeld{}
	for _, one := range held {
		if len(one.carried) != nh {
			continue
		}
		reachable[string(one.carried)] = one.where
		frontier = append(frontier, one)
	}
	if len(frontier) == 0 {
		t.Fatal("no retained buffer is the provider's hash length, so this closure was seeded with nothing")
	}
	for range tree.depth {
		next := []secretTreeHeld{}
		for _, from := range frontier {
			for _, label := range []string{"left", "right"} {
				child := secretTreeHeld{
					where:   from.where + "/" + label,
					carried: crypto.ExpandWithLabel(from.carried, "tree", []byte(label), nh),
				}
				reachable[string(child.carried)] = child.where
				next = append(next, child)
			}
		}
		frontier = next
	}

	// the control on the closure, named by tree math rather than counted: node x roots the
	// subtree SubtreeSpan delimits, and every secret in it must be in the closure. A closure
	// that expanded nothing would otherwise satisfy the comparison below by being empty,
	// which is the one outcome this test must be unable to reach.
	legitimate := map[string]NodeIndex{}
	for node := range tree.nodes {
		first, last := SubtreeSpan(node)
		for at := first; at <= last; at++ {
			legitimate[string(stExpectedNodeSecret(t, crypto, encryptionSecret, leafCount, at))] = at
		}
	}
	// 11 for an eight leaf tree with leaf 0 taken: leaf 1, the level-1 node over leaves 2
	// and 3 with its two children, and the level-2 node over leaves 4 to 7 with its six.
	if len(legitimate) != 11 {
		t.Fatalf("the retained state roots %d derivable secrets, want 11", len(legitimate))
	}
	for secret, node := range legitimate {
		if _, ok := reachable[secret]; !ok {
			t.Fatalf("node %d lies under a node the tree retained and the closure did not reach it, so the closure is not walking", node)
		}
	}
	for secret, node := range destroyed {
		if where, ok := reachable[secret]; ok {
			t.Fatalf("node %d's secret is derivable from what the tree retained, as %s", node, where)
		}
	}
}

// TestSecretTreeLeafSecretIsTakenOnce asserts the second call for one leaf fails, in every
// swept width and for every leaf of it.
//
// Retaining it would keep a secret alive that has already produced both ratchet roots, which
// is exactly the forward secrecy RFC 9420 section 9 is for. The sweep is what stops this
// holding only for the first leaf of the first tree: an implementation that deleted the leaf
// but kept an ancestor would answer the second call from the ancestor, and only a leaf whose
// ancestors are still held shows it.
func TestSecretTreeLeafSecretIsTakenOnce(t *testing.T) {
	crypto := stTestCrypto(t)
	encryptionSecret := MustHex(t, stVectorEncryptionSecret)
	refused := 0
	for _, n := range stSweptLeafCounts() {
		for _, leaf := range stLeavesOf(t, n) {
			tree, err := NewSecretTree(crypto, n, encryptionSecret)
			if err != nil {
				t.Fatalf("NewSecretTree(%d): %v", n, err)
			}
			if _, err := tree.takeLeafSecret(leaf); err != nil {
				t.Fatalf("takeLeafSecret(%d): %v", leaf, err)
			}
			if _, err := tree.takeLeafSecret(leaf); !errors.Is(err, ErrSecretTreeConsumed) {
				t.Fatalf("second take of leaf %d of %d err = %v, want ErrSecretTreeConsumed", leaf, n, err)
			}
			refused++
		}
	}
	if refused != 127 {
		t.Fatalf("refused %d second takes, want 127", refused)
	}
}

// TestSecretTreeRejectsOutOfRangeLeaf asserts a leaf beyond the tree is a typed error and not
// an index panic on a message from a peer.
//
// The last two are the ones a smaller test misses. LeafIndex.NodeIndex is total and wraps at
// 2^31, so leaf 2^31 sits at node 0 — indistinguishable from leaf 0 to anything that range
// checks the NODE and not the LEAF. A tree that accepted it would hand out leaf 0's secret to
// a sender claiming an index no tree can hold.
func TestSecretTreeRejectsOutOfRangeLeaf(t *testing.T) {
	crypto := stTestCrypto(t)
	tree, err := NewSecretTree(crypto, 8, MustHex(t, stVectorEncryptionSecret))
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	for _, leaf := range []LeafIndex{8, 9, 1 << 20, 1 << 31, (1 << 31) + 4, 0xffffffff} {
		if _, err := tree.takeLeafSecret(leaf); !errors.Is(err, ErrSecretTreeLeafOutOfRange) {
			t.Fatalf("leaf %d err = %v, want ErrSecretTreeLeafOutOfRange", leaf, err)
		}
	}
	// and the refusals cost nothing: leaf 0 is still there afterwards, so an out of range
	// index from a peer cannot be used to consume a leaf it does not own.
	if _, err := tree.takeLeafSecret(0); err != nil {
		t.Fatalf("leaf 0 after six refused indices: %v", err)
	}
}

// TestSecretTreeReadsItsSecretWidthOffTheProviderItWasHanded is the differential the registry
// cannot supply.
//
// Both registered suites fix Nh at 32, and 32 is also Nk on the chacha suite, the length of
// every vector in this tree, and the literal a body would have written down — so inside the
// registry a read of HashSize() and a written 32 are the same number and no test here can
// separate them. This provider is assembled instead, at the Nh 48 five registered suites
// carry, and every other length it reports differs from 48, so each substitution is a
// different number rather than the same one.
func TestSecretTreeReadsItsSecretWidthOffTheProviderItWasHanded(t *testing.T) {
	crypto := &suiteCryptoProvider{params: &labelKatSyntheticParams, random: rand.Reader}
	nh := crypto.HashSize()
	if nh != labelKatSyntheticParams.Nh {
		t.Fatalf("the fixture reports Nh %d, want %d", nh, labelKatSyntheticParams.Nh)
	}
	// the substitutions this provider has to be able to see. A length here equal to Nh would
	// leave every assertion below satisfied by the literal it exists to catch.
	for _, other := range []struct {
		name  string
		value int
	}{
		{name: "the registry's own hash size", value: 32},
		{name: "this suite's Nk", value: crypto.KeySize()},
		{name: "this suite's Nn", value: crypto.NonceSize()},
		{name: "this suite's Nt", value: labelKatSyntheticParams.Nt},
	} {
		if other.value == nh {
			t.Fatalf("%s is %d, the same as this fixture's Nh, so this differential is blind to it",
				other.name, other.value)
		}
	}

	encryptionSecret := bytes.Repeat([]byte{0x5a}, nh)
	// the length both registered suites fix is refused here, which is the length half of the
	// same claim: the constructor sizes its input by the provider and not by a literal.
	if _, err := NewSecretTree(crypto, 8, encryptionSecret[:32]); !errors.Is(err, ErrSecretLength) {
		t.Fatalf("a 32 byte secret for an Nh 48 suite err = %v, want ErrSecretLength", err)
	}
	tree, err := NewSecretTree(crypto, 8, encryptionSecret)
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	got, err := tree.takeLeafSecret(5)
	if err != nil {
		t.Fatalf("takeLeafSecret(5): %v", err)
	}
	if len(got) != nh {
		t.Fatalf("leaf secret is %d bytes for a suite whose Nh is %d", len(got), nh)
	}
	// and the length moved inside the preimage with it, not only in the size of the answer:
	// KDFLabel carries the length, so a body expanding to 32 and a body expanding to 48
	// disagree in every byte rather than in a prefix.
	want := stExpectedNodeSecret(t, crypto, encryptionSecret, 8, LeafIndex(5).NodeIndex())
	if !bytes.Equal(got, want) {
		t.Fatalf("leaf 5 = %x, want %x", got, want)
	}
	atThirtyTwo := crypto.ExpandWithLabel(encryptionSecret, "tree", []byte("right"), 32)
	atNh := crypto.ExpandWithLabel(encryptionSecret, "tree", []byte("right"), nh)
	if bytes.Equal(atThirtyTwo, atNh[:32]) {
		t.Fatalf("expanding to 32 and to %d agree on their first 32 bytes, so a hardcoded length "+
			"would be a truncation this test could not see", nh)
	}
	// worth recording, because it is why the assertion above is a prefix comparison and not
	// a whole tree derivation: with a hardcoded 32 the SECOND expansion of this suite is
	// handed a 32 byte pseudorandom key for a 48 byte hash, which the provider refuses
	// outright. So on this fixture a hardcoded width does not produce a wrong secret, it
	// produces no secret at all — a louder failure than the one being guarded against, but
	// only on a suite whose Nh exceeds 32, which is exactly what the registry does not have.

}

// TestSecretTreeDoesNotEraseTheCallersEncryptionSecret asserts the constructor copies what it
// is handed.
//
// The tree erases the secrets it holds, and the encryption secret it is seeded with is one of
// the nine the key schedule owns and zeroizes on its own schedule. Seeding the root with the
// caller's slice would mean building a secret tree silently destroyed the epoch's
// encryption_secret, and every existing test would still pass because the tree itself would
// be correct.
func TestSecretTreeDoesNotEraseTheCallersEncryptionSecret(t *testing.T) {
	crypto := stTestCrypto(t)
	encryptionSecret := MustHex(t, stVectorEncryptionSecret)
	original := append([]byte(nil), encryptionSecret...)
	tree, err := NewSecretTree(crypto, 4, encryptionSecret)
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	if _, err := tree.takeLeafSecret(0); err != nil {
		t.Fatalf("takeLeafSecret: %v", err)
	}
	if !bytes.Equal(encryptionSecret, original) {
		t.Fatalf("the caller's encryption secret is now %x, want it untouched at %x", encryptionSecret, original)
	}
	// and the copy goes the other way too: mutating the caller's slice after construction
	// does not move the tree, so the tree is not aliasing it.
	fresh, err := NewSecretTree(crypto, 4, encryptionSecret)
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	for i := range encryptionSecret {
		encryptionSecret[i] ^= 0xff
	}
	got, err := fresh.takeLeafSecret(2)
	if err != nil {
		t.Fatalf("takeLeafSecret: %v", err)
	}
	want := stExpectedNodeSecret(t, crypto, original, 4, LeafIndex(2).NodeIndex())
	if !bytes.Equal(got, want) {
		t.Fatalf("leaf 2 = %x, want %x — the tree aliased the caller's slice", got, want)
	}
}

// ---------------------------------------------------------------------------
// the properties the mutation round found nothing observing
// ---------------------------------------------------------------------------

// TestSecretTreeRetainsNoCopyOfADestroyedSecretInAnyFieldOfItsType is the "never delete"
// half of RFC 9420 section 9, over every leaf of every swept width.
//
// It is the property whose ABSENCE is invisible to every correctness test in this file. An
// implementation that keeps every node secret forever answers every "what is this leaf's
// secret" question identically to one that erases as it goes, so nothing that compares an
// answer separates them; the only thing that does is asking what the tree still holds
// afterwards, and asking it of the whole type rather than of one field.
//
// The scope is secretTreeRetainedBytes for exactly that reason. Measured: a SecretTree with an
// archive map[NodeIndex][]byte, written one line before each zeroizeSecret, keeps every
// ancestor of every consumed leaf for the epoch's lifetime and passed all 750 tests of this
// package before this test existed.
func TestSecretTreeRetainsNoCopyOfADestroyedSecretInAnyFieldOfItsType(t *testing.T) {
	crypto := stTestCrypto(t)
	encryptionSecret := MustHex(t, stVectorEncryptionSecret)
	checked := 0
	for _, n := range stSweptLeafCounts() {
		for _, leaf := range stLeavesOf(t, n) {
			tree, err := NewSecretTree(crypto, n, encryptionSecret)
			if err != nil {
				t.Fatalf("NewSecretTree(%d): %v", n, err)
			}
			taken, err := tree.takeLeafSecret(leaf)
			if err != nil {
				t.Fatalf("takeLeafSecret(%d) of %d: %v", leaf, n, err)
			}
			// everything that take was supposed to destroy: the leaf's own node and every
			// ancestor of it, named by tree math rather than by hand.
			target := leaf.NodeIndex()
			ancestors, err := DirectPath(target, n)
			if err != nil {
				t.Fatalf("DirectPath(%d, %d): %v", target, n, err)
			}
			destroyed := map[string]NodeIndex{}
			for _, node := range append([]NodeIndex{target}, ancestors...) {
				destroyed[string(stExpectedNodeSecret(t, crypto, encryptionSecret, n, node))] = node
			}
			if len(destroyed) != len(ancestors)+1 {
				t.Fatalf("two of the destroyed nodes of leaf %d of %d hold one secret, so this case cannot tell them apart", leaf, n)
			}
			// the taken secret is one of them, which is what says this map holds the real
			// values rather than a parallel mistake agreeing with itself.
			if _, ok := destroyed[string(taken)]; !ok {
				t.Fatalf("the secret takeLeafSecret returned for leaf %d of %d is not the one this test derived for it", leaf, n)
			}

			held := secretTreeRetainedBytes(t, tree)
			for _, one := range held {
				for secret, node := range destroyed {
					if bytes.Contains(one.carried, []byte(secret)) {
						t.Fatalf("node %d of a %d leaf tree was destroyed by taking leaf %d and its secret is still held at %s",
							node, n, leaf, one.where)
					}
				}
			}
			// the control on the walk, per tree: it reaches what the tree DID retain. Without
			// it the sweep above is satisfied by a walk that read nothing.
			for node, secret := range tree.nodes {
				found := false
				for _, one := range held {
					if bytes.Equal(one.carried, secret) {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("the walk over a %d leaf tree did not report node %d's retained secret", n, node)
				}
			}
			checked += len(destroyed)
		}
	}
	// one destroyed node per ancestor plus the leaf, over every leaf of every swept width:
	// 1*1 + 2*2 + 4*3 + 8*4 + 16*5 + 32*6 + 64*7.
	if checked != 769 {
		t.Fatalf("checked %d destroyed node secrets, want 769", checked)
	}
}

// secretTreeGeometryFields is every field of SecretTree the constructor DERIVES from the leaf
// count, mapped to the tree math function that defines it.
//
// These four are cached once and read without the lock for the rest of the tree's life, and
// three of them are bounds: width bounds the node array, depth bounds the descent's runaway
// check, and root is where every descent starts. A cached bound that disagrees with the leaf
// count is invisible to every behavioural test, because the code it guards is unreachable for
// a well formed tree -- measured, on this file: width set to 0xffffffff and depth set to
// TreeDepth(n)+8 each left all 750 tests of this package passing. Comparing the cache against
// the function that defines it is what sees them.
var secretTreeGeometryFields = map[string]func(t *testing.T, n LeafCount) uint64{
	"leafCount": func(t *testing.T, n LeafCount) uint64 { return uint64(n) },
	"width":     func(t *testing.T, n LeafCount) uint64 { return uint64(NodeWidth(n)) },
	"depth":     func(t *testing.T, n LeafCount) uint64 { return uint64(TreeDepth(n)) },
	"root": func(t *testing.T, n LeafCount) uint64 {
		root, err := Root(n)
		if err != nil {
			t.Fatalf("Root(%d): %v", n, err)
		}
		return uint64(root)
	},
}

// secretTreeStateFields is the rest of SecretTree: the fields that are not a function of the
// leaf count, each with the reason it cannot be one.
//
// It exists so the check below can be COMPLETE over the type rather than over the four names
// somebody wrote down. A field added and left out of both maps fails, which is the point: p4
// task 22 adds ratchets to this struct, and the failure is where its author decides whether
// the new field is cached geometry or mutable state.
var secretTreeStateFields = map[string]string{
	"stateLock": "the mutex guarding the mutable fields; it holds no value to derive",
	"crypto":    "the provider the caller handed in, which the tree does not choose",
	"nodes":     "the mutable node secrets, whose contents change with every take",
	"ratchets":  "the per leaf hash ratchets, created on demand and advanced per message",
	"erased":    "whether Zeroize has run, which is a fact about this tree's lifetime and not about its shape",
}

// TestSecretTreeCachedGeometryIsDerivedFromTheLeafCount holds every cached bound of a
// constructed tree to the tree math function that defines it, at every swept width.
func TestSecretTreeCachedGeometryIsDerivedFromTheLeafCount(t *testing.T) {
	// the completeness half first, and it is the half with the consequence: a field of this
	// type judged by neither map is a cached value nothing compares.
	structure := reflect.TypeOf(SecretTree{})
	if structure.NumField() == 0 {
		t.Fatal("SecretTree has no fields, so this gate read the wrong type")
	}
	onTheType := map[string]bool{}
	for at := range structure.NumField() {
		name := structure.Field(at).Name
		onTheType[name] = true
		_, geometry := secretTreeGeometryFields[name]
		_, state := secretTreeStateFields[name]
		if geometry == state {
			t.Errorf("SecretTree.%s is judged by %s of the two maps; a field is either derived from the leaf count or it is not, and this gate cannot tell which",
				name, map[bool]string{true: "both", false: "neither"}[geometry])
		}
	}
	for _, name := range slices.Sorted(maps.Keys(secretTreeGeometryFields)) {
		if !onTheType[name] {
			t.Errorf("secretTreeGeometryFields names %s and SecretTree has no such field, so this sweep compares a field that is gone", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(secretTreeStateFields)) {
		if !onTheType[name] {
			t.Errorf("secretTreeStateFields excuses %s and SecretTree has no such field; delete the entry", name)
		}
	}

	crypto := stTestCrypto(t)
	encryptionSecret := MustHex(t, stVectorEncryptionSecret)
	compared := 0
	for _, n := range stSweptLeafCounts() {
		tree, err := NewSecretTree(crypto, n, encryptionSecret)
		if err != nil {
			t.Fatalf("NewSecretTree(%d): %v", n, err)
		}
		value := reflect.ValueOf(tree).Elem()
		for _, name := range slices.Sorted(maps.Keys(secretTreeGeometryFields)) {
			field := value.FieldByName(name)
			if !field.IsValid() {
				t.Fatalf("SecretTree has no field %s", name)
			}
			if !field.CanUint() {
				t.Fatalf("SecretTree.%s is a %s and this sweep reads it as an unsigned integer; a retyped bound is a comparison this gate can no longer make",
					name, field.Kind())
			}
			if got, want := field.Uint(), secretTreeGeometryFields[name](t, n); got != want {
				t.Errorf("a %d leaf tree caches %s = %d, and tree math says %d", n, name, got, want)
			}
			compared++
		}
	}
	// four cached fields at each of the seven swept widths.
	if compared != 4*len(stSweptLeafCounts()) {
		t.Fatalf("compared %d cached values, want %d", compared, 4*len(stSweptLeafCounts()))
	}
}

// TestSecretTreePathToLeafLandsOnTheTargetOfEveryLeafOfEveryWidth asserts the descent's own
// invariants: it starts at the root, it steps to a child of the node it is standing on, it
// drops exactly one level per step, it is depth+1 nodes long, and it ENDS ON THE LEAF IT WAS
// AIMED AT.
//
// The last of those is why this test exists. secret_tree.go's post descent landing check is
// unreachable for a full tree -- replacing its condition with a constant false leaves every
// test in this package passing -- so no single edit can kill it, and the honest answer is to
// label it there and assert the invariant that makes it redundant here rather than leave the
// claim standing on an argument in a comment.
func TestSecretTreePathToLeafLandsOnTheTargetOfEveryLeafOfEveryWidth(t *testing.T) {
	crypto := stTestCrypto(t)
	encryptionSecret := MustHex(t, stVectorEncryptionSecret)
	walked := 0
	for _, n := range stSweptLeafCounts() {
		tree, err := NewSecretTree(crypto, n, encryptionSecret)
		if err != nil {
			t.Fatalf("NewSecretTree(%d): %v", n, err)
		}
		for _, leaf := range stLeavesOf(t, n) {
			path, err := tree.pathToLeaf(leaf)
			if err != nil {
				t.Fatalf("pathToLeaf(%d) of %d: %v", leaf, n, err)
			}
			if len(path) == 0 {
				t.Fatalf("the path to leaf %d of %d is empty", leaf, n)
			}
			if path[0] != tree.root {
				t.Fatalf("the path to leaf %d of %d starts at node %d, want the root %d", leaf, n, path[0], tree.root)
			}
			if path[len(path)-1] != leaf.NodeIndex() {
				t.Fatalf("the descent to leaf %d of %d ended at node %d, want node %d",
					leaf, n, path[len(path)-1], leaf.NodeIndex())
			}
			if uint32(len(path)) != tree.depth+1 {
				t.Fatalf("the path to leaf %d of %d is %d nodes long, want depth+1 = %d",
					leaf, n, len(path), tree.depth+1)
			}
			for at := 0; at+1 < len(path); at++ {
				left, err := Left(path[at])
				if err != nil {
					t.Fatalf("Left(%d): %v", path[at], err)
				}
				right, err := Right(path[at])
				if err != nil {
					t.Fatalf("Right(%d): %v", path[at], err)
				}
				if path[at+1] != left && path[at+1] != right {
					t.Fatalf("the path to leaf %d of %d steps from node %d to node %d, which is neither of its children %d and %d",
						leaf, n, path[at], path[at+1], left, right)
				}
				if path[at+1].Level()+1 != path[at].Level() {
					t.Fatalf("the path to leaf %d of %d steps from level %d to level %d",
						leaf, n, path[at].Level(), path[at+1].Level())
				}
			}
			walked++
		}
	}
	// one descent per leaf of each swept width: 1+2+4+8+16+32+64.
	if walked != 127 {
		t.Fatalf("walked %d descents, want 127", walked)
	}
}

// TestSecretTreeEveryParentOnEveryPathHasBothChildrenInsideTheNodeArray is the invariant
// behind the other labelled refusal in takeLeafSecret.
//
// The tree is always full -- section 7.7 makes a valid leaf count a power of two and Root
// refuses any other -- so both children of every parent the descent stands on are inside the
// node array and the child bound cannot fire. That used to be two ifs that SKIPPED a store
// when it did fire, which is the failure mode worth naming: a skipped store leaves every leaf
// under that child answering ErrSecretTreeConsumed, telling the caller forward secrecy did its
// job when what actually happened is that the tree dropped a subtree.
func TestSecretTreeEveryParentOnEveryPathHasBothChildrenInsideTheNodeArray(t *testing.T) {
	crypto := stTestCrypto(t)
	encryptionSecret := MustHex(t, stVectorEncryptionSecret)
	checked := 0
	for _, n := range stSweptLeafCounts() {
		tree, err := NewSecretTree(crypto, n, encryptionSecret)
		if err != nil {
			t.Fatalf("NewSecretTree(%d): %v", n, err)
		}
		for _, leaf := range stLeavesOf(t, n) {
			path, err := tree.pathToLeaf(leaf)
			if err != nil {
				t.Fatalf("pathToLeaf(%d) of %d: %v", leaf, n, err)
			}
			for at := 0; at+1 < len(path); at++ {
				left, err := Left(path[at])
				if err != nil {
					t.Fatalf("Left(%d): %v", path[at], err)
				}
				right, err := Right(path[at])
				if err != nil {
					t.Fatalf("Right(%d): %v", path[at], err)
				}
				if uint32(left) >= tree.width || uint32(right) >= tree.width {
					t.Fatalf("the children %d and %d of node %d lie outside a %d leaf tree's node array of width %d",
						left, right, path[at], n, tree.width)
				}
				checked++
			}
		}
	}
	// one parent per level per leaf: 2*1 + 4*2 + 8*3 + 16*4 + 32*5 + 64*6, and none in the
	// one leaf tree whose root is its leaf.
	if checked != 642 {
		t.Fatalf("checked %d parents, want 642", checked)
	}
}

// TestSecretTreeHeldNodesNeverShareARootLeafPath asserts the antichain invariant the deepest
// held ancestor scan rests on.
//
// Expanding a node deletes it and stores both of its children, so the nodes a tree still holds
// always form a frontier and no two of them sit on one root-leaf path. That is what makes
// "the deepest held node" and "the first held node" the same node, and it is why no single
// edit to that scan can be caught by the public surface -- taking the shallowest instead left
// all 750 tests of this package passing. The invariant had no assertion anywhere; it has one
// here, and the rule it makes redundant is measured on its own next door.
func TestSecretTreeHeldNodesNeverShareARootLeafPath(t *testing.T) {
	crypto := stTestCrypto(t)
	encryptionSecret := MustHex(t, stVectorEncryptionSecret)
	observed := 0
	for _, n := range stSweptLeafCounts() {
		leaves := stLeavesOf(t, n)
		for _, order := range stTakeOrders(t, leaves) {
			tree, err := NewSecretTree(crypto, n, encryptionSecret)
			if err != nil {
				t.Fatalf("NewSecretTree(%d): %v", n, err)
			}
			for _, taken := range order {
				if _, err := tree.takeLeafSecret(taken); err != nil {
					t.Fatalf("takeLeafSecret(%d) of %d: %v", taken, n, err)
				}
				for _, leaf := range leaves {
					path, err := tree.pathToLeaf(leaf)
					if err != nil {
						t.Fatalf("pathToLeaf(%d) of %d: %v", leaf, n, err)
					}
					onThePath := []NodeIndex{}
					for _, node := range path {
						if _, ok := tree.nodes[node]; ok {
							onThePath = append(onThePath, node)
						}
					}
					if len(onThePath) > 1 {
						t.Fatalf("after taking leaf %d of a %d leaf tree, nodes %v are all held and all sit on the path to leaf %d",
							taken, n, onThePath, leaf)
					}
					observed++
				}
			}
		}
	}
	// every leaf's path examined after every take, in three orders, at each swept width:
	// 3 * (1 + 4 + 16 + 64 + 256 + 1024 + 4096).
	if observed != 16383 {
		t.Fatalf("examined %d paths, want 16383", observed)
	}
}

// TestSecretTreeTakesTheDeepestHeldAncestorAndNotTheShallowest is the one state in which the
// two rules name different nodes, and it has to be planted because the antichain invariant
// next door says the tree never reaches it on its own.
//
// The planting is the point rather than a shortcut. takeLeafSecret's scan is written as "the
// deepest held node" and the reason it matters is what happens if the invariant ever broke:
// starting from a shallower ancestor re-derives and OVERWRITES the subtree below it from a
// secret the tree has already promised to destroy, so every leaf under that ancestor would
// then be a value an attacker holding the erased secret can compute. Nothing observed that,
// and taking the shallowest instead of the deepest left all 750 tests of this package passing.
func TestSecretTreeTakesTheDeepestHeldAncestorAndNotTheShallowest(t *testing.T) {
	crypto := stTestCrypto(t)
	encryptionSecret := MustHex(t, stVectorEncryptionSecret)
	const leafCount = LeafCount(8)
	nh := crypto.HashSize()
	tree, err := NewSecretTree(crypto, leafCount, encryptionSecret)
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	if _, err := tree.takeLeafSecret(0); err != nil {
		t.Fatalf("takeLeafSecret(0): %v", err)
	}

	// leaf 2's path, and the one node of it the tree still holds. Both are read off the tree
	// rather than written down, so the fixture cannot drift away from the shape it needs.
	const target = LeafIndex(2)
	path, err := tree.pathToLeaf(target)
	if err != nil {
		t.Fatalf("pathToLeaf(%d): %v", target, err)
	}
	stillHeld := []int{}
	for at, node := range path {
		if _, ok := tree.nodes[node]; ok {
			stillHeld = append(stillHeld, at)
		}
	}
	if len(stillHeld) != 1 {
		t.Fatalf("%d nodes of the path to leaf %d are held; this fixture needs exactly one", len(stillHeld), target)
	}
	deeper := stillHeld[0]
	if deeper < 1 {
		t.Fatalf("the held node is at the root of the path, so there is no erased ancestor above it to plant at")
	}
	planted := path[deeper-1]
	if _, ok := tree.nodes[planted]; ok {
		t.Fatalf("node %d is still held, so planting there would not be planting a STALE ancestor", planted)
	}

	stale := bytes.Repeat([]byte{0xa5}, nh)
	tree.nodes[planted] = append([]byte(nil), stale...)

	// what the shallow rule would answer, replayed from the planted node down the same path.
	fromStale := append([]byte(nil), stale...)
	for at := deeper - 1; at+1 < len(path); at++ {
		left, err := Left(path[at])
		if err != nil {
			t.Fatalf("Left(%d): %v", path[at], err)
		}
		label := "right"
		if path[at+1] == left {
			label = "left"
		}
		fromStale = crypto.ExpandWithLabel(fromStale, "tree", []byte(label), nh)
	}
	want := stExpectedNodeSecret(t, crypto, encryptionSecret, leafCount, target.NodeIndex())
	// the control: the two rules answer different values here, so this test can tell them
	// apart. Without it a fixture where they agreed would pass either way.
	if bytes.Equal(fromStale, want) {
		t.Fatalf("the stale ancestor at node %d derives the same secret as the node still held, so this test cannot separate the two rules", planted)
	}

	got, err := tree.takeLeafSecret(target)
	if err != nil {
		t.Fatalf("takeLeafSecret(%d): %v", target, err)
	}
	if bytes.Equal(got, fromStale) {
		t.Fatalf("leaf %d was derived from the stale ancestor at node %d rather than from the deeper node still held; every leaf under node %d is now a value anyone holding the erased ancestor can compute",
			target, planted, planted)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("leaf %d = %x, want %x", target, got, want)
	}
}

// TestSecretTreeASecondTakeIsAlwaysTheConsumedRefusalAndNeverTheInvariantOne holds every
// legitimate refusal to the FIRST of takeLeafSecret's two ErrSecretTreeConsumed returns.
//
// The second one -- reached when the descent did not store the leaf it descended to -- is
// unreachable: the expansion loop stores both children of every node it expands and its last
// iteration expands the target's parent, and when the loop does not run the deepest held node
// was already the target. It was reclassified to an unrelated sentinel with every test in this
// package still passing, which is exactly what happens to an unreachable return that nothing
// names, so it now wraps a sentinel of its own and this test says which of the two fires.
func TestSecretTreeASecondTakeIsAlwaysTheConsumedRefusalAndNeverTheInvariantOne(t *testing.T) {
	// the control on the matcher. The two returns differ only in the sentinel the second
	// wraps, so an errors.Is that could not see it through the wrapping would report every
	// refusal as the ordinary one no matter which return produced it.
	both := fmt.Errorf("%w: leaf 0: %w", ErrSecretTreeConsumed, errSecretTreeDescentDidNotStoreTheTarget)
	if !errors.Is(both, ErrSecretTreeConsumed) || !errors.Is(both, errSecretTreeDescentDidNotStoreTheTarget) {
		t.Fatalf("the two sentinels are not both visible through %v, so this test cannot tell the two returns apart", both)
	}
	if errors.Is(fmt.Errorf("%w: leaf 0", ErrSecretTreeConsumed), errSecretTreeDescentDidNotStoreTheTarget) {
		t.Fatal("the ordinary consumed refusal answers to the invariant sentinel, so every refusal below would read as the invariant one")
	}

	crypto := stTestCrypto(t)
	encryptionSecret := MustHex(t, stVectorEncryptionSecret)
	refused := 0
	for _, n := range stSweptLeafCounts() {
		leaves := stLeavesOf(t, n)
		for _, order := range stTakeOrders(t, leaves) {
			tree, err := NewSecretTree(crypto, n, encryptionSecret)
			if err != nil {
				t.Fatalf("NewSecretTree(%d): %v", n, err)
			}
			for _, leaf := range order {
				if _, err := tree.takeLeafSecret(leaf); err != nil {
					t.Fatalf("takeLeafSecret(%d) of %d: %v", leaf, n, err)
				}
				_, err := tree.takeLeafSecret(leaf)
				if !errors.Is(err, ErrSecretTreeConsumed) {
					t.Fatalf("the second take of leaf %d of %d err = %v, want ErrSecretTreeConsumed", leaf, n, err)
				}
				if errors.Is(err, errSecretTreeDescentDidNotStoreTheTarget) {
					t.Fatalf("the second take of leaf %d of %d was refused by the post descent return, which is reachable only if the descent stopped storing the leaf it descended to: %v",
						leaf, n, err)
				}
				refused++
			}
		}
	}
	// every leaf of every swept width, taken again immediately, in three orders.
	if refused != 3*127 {
		t.Fatalf("refused %d second takes, want %d", refused, 3*127)
	}
}

// ---------------------------------------------------------------------------
// task 22 and 23: the plan's own tests, kept where they observe something
// ---------------------------------------------------------------------------

// TestRatchetRootsUseDistinctLabels asserts the handshake and application ratchets
// are separate expansions of the same leaf secret. Sharing one ratchet between
// handshake and application messages would reuse an AEAD key and nonce pair.
func TestRatchetRootsUseDistinctLabels(t *testing.T) {
	crypto := stTestCrypto(t)
	encryptionSecret := MustHex(t, stVectorEncryptionSecret)
	tree, err := NewSecretTree(crypto, 1, encryptionSecret)
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	handshake, err := tree.ratchetFor(0, RatchetHandshake)
	if err != nil {
		t.Fatalf("ratchetFor handshake: %v", err)
	}
	application, err := tree.ratchetFor(0, RatchetApplication)
	if err != nil {
		t.Fatalf("ratchetFor application: %v", err)
	}
	nh := crypto.HashSize()
	wantHandshake := crypto.ExpandWithLabel(encryptionSecret, "handshake", nil, nh)
	wantApplication := crypto.ExpandWithLabel(encryptionSecret, "application", nil, nh)
	if !bytes.Equal(handshake.secret, wantHandshake) {
		t.Fatalf("handshake root = %x, want %x", handshake.secret, wantHandshake)
	}
	if !bytes.Equal(application.secret, wantApplication) {
		t.Fatalf("application root = %x, want %x", application.secret, wantApplication)
	}
}

// TestRatchetStepDerivesKeyNonceAndSuccessor pins the three DeriveTreeSecret calls of
// RFC 9420 section 9.1 and asserts the ratchet advances by exactly one generation.
func TestRatchetStepDerivesKeyNonceAndSuccessor(t *testing.T) {
	crypto := stTestCrypto(t)
	tree, err := NewSecretTree(crypto, 1, MustHex(t, stVectorEncryptionSecret))
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	r, err := tree.ratchetFor(0, RatchetApplication)
	if err != nil {
		t.Fatalf("ratchetFor: %v", err)
	}
	root := append([]byte(nil), r.secret...)
	wantKey := crypto.DeriveTreeSecret(root, "key", 0, crypto.KeySize())
	wantNonce := crypto.DeriveTreeSecret(root, "nonce", 0, crypto.NonceSize())
	wantNext := crypto.DeriveTreeSecret(root, "secret", 0, crypto.HashSize())

	generation, keys, err := r.step()
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if generation != 0 {
		t.Fatalf("generation = %d, want 0", generation)
	}
	if !bytes.Equal(keys.key, wantKey) {
		t.Fatalf("key = %x, want %x", keys.key, wantKey)
	}
	if !bytes.Equal(keys.nonce, wantNonce) {
		t.Fatalf("nonce = %x, want %x", keys.nonce, wantNonce)
	}
	if !bytes.Equal(r.secret, wantNext) {
		t.Fatalf("successor secret = %x, want %x", r.secret, wantNext)
	}
	if r.head != 1 {
		t.Fatalf("head = %d, want 1", r.head)
	}
}

// TestRatchetStepBindsTheGeneration asserts generation 1 uses generation 1 in the
// DeriveTreeSecret context and not 0, so a copy-paste of the previous call is caught.
func TestRatchetStepBindsTheGeneration(t *testing.T) {
	crypto := stTestCrypto(t)
	tree, err := NewSecretTree(crypto, 1, MustHex(t, stVectorEncryptionSecret))
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	r, err := tree.ratchetFor(0, RatchetApplication)
	if err != nil {
		t.Fatalf("ratchetFor: %v", err)
	}
	if _, _, err := r.step(); err != nil {
		t.Fatalf("step: %v", err)
	}
	secondRoot := append([]byte(nil), r.secret...)
	wantKey := crypto.DeriveTreeSecret(secondRoot, "key", 1, crypto.KeySize())
	generation, keys, err := r.step()
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if generation != 1 {
		t.Fatalf("generation = %d, want 1", generation)
	}
	if !bytes.Equal(keys.key, wantKey) {
		t.Fatalf("generation 1 key is not bound to generation 1")
	}
}

// TestRatchetKeysAreNeverRepeated asserts the first two hundred generations produce
// two hundred distinct key and nonce pairs, which is the AEAD safety property.
//
// Two hundred consecutive generations from zero is the middle of the range and cannot see
// a boundary; TestRatchetKeyNonceAndSuccessorAreDerivedAtEveryGenerationBoundary and
// TestRatchetRefusesToWrapTheGenerationCounter are where the ends are.
func TestRatchetKeysAreNeverRepeated(t *testing.T) {
	tree, err := NewSecretTree(stTestCrypto(t), 1, MustHex(t, stVectorEncryptionSecret))
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	r, err := tree.ratchetFor(0, RatchetApplication)
	if err != nil {
		t.Fatalf("ratchetFor: %v", err)
	}
	seen := map[string]uint32{}
	for i := 0; i < 200; i++ {
		generation, keys, err := r.step()
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		fingerprint := string(keys.key) + "|" + string(keys.nonce)
		if previous, ok := seen[fingerprint]; ok {
			t.Fatalf("generation %d repeats the key and nonce of generation %d", generation, previous)
		}
		seen[fingerprint] = generation
	}
}

// TestNextSenderKeyAdvances asserts the sender path hands out consecutive generations
// and never repeats one.
//
// The two lengths are the plan's literals and are kept as literals on purpose: on this
// suite they are the right numbers, and what they cannot see -- a body that writes 32 and
// 12 down instead of reading them -- is measured on a suite whose Nk and Nn are neither,
// in TestRatchetReadsItsKeyAndNonceWidthsOffTheProviderItWasHanded.
func TestNextSenderKeyAdvances(t *testing.T) {
	tree, err := NewSecretTree(stTestCrypto(t), 8, MustHex(t, stVectorEncryptionSecret))
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	for want := uint32(0); want < 5; want++ {
		generation, key, nonce, err := tree.NextSenderKey(2, RatchetApplication)
		if err != nil {
			t.Fatalf("NextSenderKey: %v", err)
		}
		if generation != want {
			t.Fatalf("generation = %d, want %d", generation, want)
		}
		if len(key) != 32 || len(nonce) != 12 {
			t.Fatalf("key %d bytes, nonce %d bytes", len(key), len(nonce))
		}
	}
	generation, err := tree.SenderGeneration(2, RatchetApplication)
	if err != nil {
		t.Fatalf("SenderGeneration: %v", err)
	}
	if generation != 5 {
		t.Fatalf("SenderGeneration = %d, want 5", generation)
	}
}

// TestReceiverKeyOutOfOrderUsesTheWindow asserts a message that arrives at generation
// 3 before generations 0 to 2 does not destroy them, which is what makes an out-of-
// order delivery a delay rather than three lost messages.
func TestReceiverKeyOutOfOrderUsesTheWindow(t *testing.T) {
	crypto := stTestCrypto(t)
	encryptionSecret := MustHex(t, stVectorEncryptionSecret)

	sender, err := NewSecretTree(crypto, 8, encryptionSecret)
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	expected := map[uint32][]byte{}
	for i := 0; i < 4; i++ {
		generation, key, _, err := sender.NextSenderKey(5, RatchetHandshake)
		if err != nil {
			t.Fatalf("NextSenderKey: %v", err)
		}
		expected[generation] = key
	}

	receiver, err := NewSecretTree(crypto, 8, encryptionSecret)
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	got3, _, err := receiver.receiverKey(5, RatchetHandshake, 3)
	if err != nil {
		t.Fatalf("receiverKey(3): %v", err)
	}
	if !bytes.Equal(got3, expected[3]) {
		t.Fatalf("generation 3 key mismatch")
	}
	for _, generation := range []uint32{0, 1, 2} {
		got, _, err := receiver.receiverKey(5, RatchetHandshake, generation)
		if err != nil {
			t.Fatalf("receiverKey(%d): %v", generation, err)
		}
		if !bytes.Equal(got, expected[generation]) {
			t.Fatalf("generation %d key mismatch", generation)
		}
	}
}

// TestReceiverKeyIsSingleUse asserts a generation cannot be fetched twice, so a
// replayed message cannot be decrypted a second time from the window.
//
// It is the one test in this file that observes a real key and nonce REPEAT rather than a
// derivation defect: everywhere else the generation number is bound into the KDF context,
// so a repeat needs two independent faults, while the window can hand one pair back twice
// with a single missing delete.
func TestReceiverKeyIsSingleUse(t *testing.T) {
	tree, err := NewSecretTree(stTestCrypto(t), 8, MustHex(t, stVectorEncryptionSecret))
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	if _, _, err := tree.receiverKey(1, RatchetApplication, 0); err != nil {
		t.Fatalf("receiverKey: %v", err)
	}
	if _, _, err := tree.receiverKey(1, RatchetApplication, 0); !errors.Is(err, ErrRatchetGenerationConsumed) {
		t.Fatalf("err = %v, want ErrRatchetGenerationConsumed", err)
	}
	// and the same for a generation that came out of the WINDOW rather than off the head,
	// which is the path with the delete in it.
	if _, _, err := tree.receiverKey(1, RatchetApplication, 4); err != nil {
		t.Fatalf("receiverKey(4): %v", err)
	}
	if _, _, err := tree.receiverKey(1, RatchetApplication, 2); err != nil {
		t.Fatalf("receiverKey(2) out of the window: %v", err)
	}
	if _, _, err := tree.receiverKey(1, RatchetApplication, 2); !errors.Is(err, ErrRatchetGenerationConsumed) {
		t.Fatalf("a window generation was handed out twice: err = %v, want ErrRatchetGenerationConsumed", err)
	}
}

// TestReceiverKeyRefusesUnboundedSkip asserts a forged generation number cannot force
// an unbounded KDF loop. Without this bound a single 32-bit field is a denial of
// service that costs the sender nothing.
//
// IT OBSERVES BOTH HALVES, and the second one came back. For a while this case ended by requiring
// the head to have moved by exactly MaxGenerationSkip per refusal, because a refusal used to run a
// catch-up first; the catch-up is gone -- see (*ratchet).classify for why -- so the assertion is
// once again that a REFUSED request leaves the ratchet exactly where it was. The work bound is
// still the half this case is named for: a build that resynchronised to the generation asked for
// would derive four billion generations for the price of one header, and a build that walked
// MaxGenerationSkip before refusing would hand any member a way to advance any other member's head
// by 1,024 a header. The head at zero refuses both.
func TestReceiverKeyRefusesUnboundedSkip(t *testing.T) {
	tree, err := NewSecretTree(stTestCrypto(t), 8, MustHex(t, stVectorEncryptionSecret))
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	refusals := uint32(0)
	_, _, err = tree.receiverKey(1, RatchetApplication, MaxGenerationSkip+1)
	if !errors.Is(err, ErrRatchetGenerationTooFarAhead) {
		t.Fatalf("err = %v, want ErrRatchetGenerationTooFarAhead", err)
	}
	refusals += 1
	if _, _, err := tree.receiverKey(1, RatchetApplication, ^uint32(0)); !errors.Is(err, ErrRatchetGenerationTooFarAhead) {
		t.Fatalf("err = %v, want ErrRatchetGenerationTooFarAhead", err)
	}
	refusals += 1
	// the head has not moved at all. The second call asked for 2^32-1, so a head anywhere near it
	// is a ratchet that derived four billion generations for the price of one header -- and a head
	// at any multiple of MaxGenerationSkip is a ratchet that walked on a number nobody signed.
	generation, err := tree.SenderGeneration(1, RatchetApplication)
	if err != nil {
		t.Fatalf("SenderGeneration: %v", err)
	}
	if generation != 0 {
		t.Fatalf("%d refused requests left the head at %d, want 0: a refusal may cost cpu and it may not cost another member its messages",
			refusals, generation)
	}
}

// TestSecretTreeZeroize asserts every retained node secret and ratchet secret is
// cleared when the epoch is dropped.
func TestSecretTreeZeroize(t *testing.T) {
	tree, err := NewSecretTree(stTestCrypto(t), 8, MustHex(t, stVectorEncryptionSecret))
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	if _, _, _, err := tree.NextSenderKey(0, RatchetApplication); err != nil {
		t.Fatalf("NextSenderKey: %v", err)
	}
	r, err := tree.ratchetFor(0, RatchetApplication)
	if err != nil {
		t.Fatalf("ratchetFor: %v", err)
	}
	retained := append([]byte(nil), r.secret...)
	tree.Zeroize()
	if bytes.Equal(r.secret, retained) {
		t.Fatal("the ratchet secret survived Zeroize")
	}
	for node, secret := range tree.nodes {
		for _, b := range secret {
			if b != 0 {
				t.Fatalf("node %d secret survived Zeroize", node)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// the ratchet properties a middle-of-the-range test cannot see
// ---------------------------------------------------------------------------

// stNewTree builds a tree of the given width over the vector encryption secret.
func stNewTree(t *testing.T, leafCount LeafCount) *SecretTree {
	t.Helper()
	tree, err := NewSecretTree(stTestCrypto(t), leafCount, MustHex(t, stVectorEncryptionSecret))
	if err != nil {
		t.Fatalf("NewSecretTree(%d): %v", leafCount, err)
	}
	return tree
}

// stRatchetTypeCodePoints is every value a RatchetType can hold, derived from the width of
// the underlying type rather than from a written 256. A type widened to uint16 sweeps the
// wider space without anyone editing this.
func stRatchetTypeCodePoints() int {
	return int(^RatchetType(0)) + 1
}

// stRatchetKinds is every ratchet type that reaches a keystream, DERIVED by asking the
// implementation which code points it admits.
//
// The version this replaces bounded a loop with `kind <= RatchetApplication` while its own
// comment claimed to derive the set from the iota run. It did not: a third type added to that
// constant block and wired into ratchetFor was covered by ZERO of the sweeps keyed on this
// helper -- the cross-ratchet key and nonce uniqueness sweep, the sender/receiver routing
// sweep, the spent-secret erasure sweep and the post-Zeroize refusal sweep all silently
// skipped the third live keystream. Measured, by adding one: `stRatchetKinds() = [1 2]` while
// `RatchetFuture = 3` had a ratchet, a root and a keystream of its own.
//
// What is written here probes the whole code point space of the type and keeps the ones
// ratchetFor answers, so the set is exactly "the types that reach a keystream" and a new one
// joins every sweep below with no list to extend. The probe is destructive -- ratchetFor
// consumes the leaf's node secret -- so each code point gets its own tree.
//
// Each admitted kind is required to have a label in this file's own table as well, because
// every independent derivation here needs one; that check is what turns "a third type was
// added and nothing here knows its label" into a failure that names this helper rather than
// into a root expanded under the empty string.
func stRatchetKinds(t *testing.T) []RatchetType {
	t.Helper()
	kinds := []RatchetType{}
	for point := range stRatchetTypeCodePoints() {
		kind := RatchetType(point)
		if _, err := stNewTree(t, 1).ratchetFor(0, kind); err != nil {
			continue
		}
		if _, ok := stRatchetLabelOf(kind); !ok {
			t.Fatalf("ratchet type %d reaches a keystream and has no label in stRatchetLabelOf, so every derivation in this file that is meant to cover it cannot be written",
				kind)
		}
		kinds = append(kinds, kind)
	}
	if len(kinds) == 0 {
		t.Fatal("no ratchet type reaches a keystream, so every sweep scoped by this helper ran over nothing")
	}
	return kinds
}

// stRatchetLabelOf is the RFC 9420 section 9 label a ratchet type expands its leaf secret
// under, and whether this file knows one for it.
//
// It is a switch and not a map so a type added without a label is a decision somebody makes
// here rather than a root expanded under the empty string, and it is INDEPENDENT of the
// implementation on purpose: stRatchetRoot derives the expected root from it, so a label read
// out of secret_tree.go would agree with a swapped pair of labels there.
func stRatchetLabelOf(kind RatchetType) (string, bool) {
	switch kind {
	case RatchetHandshake:
		return "handshake", true
	case RatchetApplication:
		return "application", true
	}
	return "", false
}

// stRatchetLabel is the same, fatal on a type this file has no label for.
func stRatchetLabel(t *testing.T, kind RatchetType) string {
	t.Helper()
	label, ok := stRatchetLabelOf(kind)
	if !ok {
		t.Fatalf("ratchet type %d has no label in this test's own table, so nothing here derives its root", kind)
	}
	return label
}

// stRatchetRoot derives one leaf's ratchet root independently of the code under test: the
// node secret comes from stExpectedNodeSecret, which walks the tree math upward, and the
// expansion is the section 9 label.
func stRatchetRoot(t *testing.T, crypto CryptoProvider, encryptionSecret []byte,
	leafCount LeafCount, leaf LeafIndex, kind RatchetType) []byte {
	t.Helper()
	leafSecret := stExpectedNodeSecret(t, crypto, encryptionSecret, leafCount, leaf.NodeIndex())
	return crypto.ExpandWithLabel(leafSecret, stRatchetLabel(t, kind), nil, crypto.HashSize())
}

// stAllZero reports whether every byte is zero, which is what an erased secret looks like.
func stAllZero(b []byte) bool {
	for _, one := range b {
		if one != 0 {
			return false
		}
	}
	return true
}

// stGenerationBoundaries is the generations this file sweeps, DERIVED from the width of the
// counter rather than typed out.
//
// A nonce defect has already shipped on this project at 2^32 while sixty-nine tests that all
// sat in the middle of the range reported green, so what this covers is the edges: zero, one
// past it, both sides of every byte boundary the counter has, and the last two values it can
// hold. Every entry comes out of the loop, so a counter that was ever widened sweeps the new
// boundaries without anyone editing a list.
func stGenerationBoundaries() []uint32 {
	last := ^uint32(0)
	bits := 0
	for shifted := last; shifted != 0; shifted >>= 1 {
		bits++
	}
	out := []uint32{0, 1, 2}
	for at := 8; at < bits; at += 8 {
		edge := uint32(1) << at
		out = append(out, edge-1, edge, edge+1)
	}
	out = append(out, last-1, last)
	return out
}

// TestRatchetGenerationBoundarySweepReachesTheEndsOfTheCounter is the control on the sweep
// every boundary test below is scoped by. A generator that returned three small numbers
// would leave each of those tests reporting green over the middle of the range, which is
// exactly the shape of the defect this file is guarding against.
func TestRatchetGenerationBoundarySweepReachesTheEndsOfTheCounter(t *testing.T) {
	swept := stGenerationBoundaries()
	last := ^uint32(0)
	for _, want := range []uint32{0, 1, 1 << 8, 1<<16 - 1, 1 << 16, 1 << 24, last - 1, last} {
		if !slices.Contains(swept, want) {
			t.Errorf("the boundary sweep does not reach generation %d", want)
		}
	}
	if len(swept) != len(slices.Compact(slices.Sorted(slices.Values(swept)))) {
		t.Fatalf("the boundary sweep repeats a generation, so its counts are not what they look like")
	}
	// three at each of the three interior byte boundaries, plus zero, one, two, and the last
	// two values the counter holds.
	if len(swept) != 3+3*3+2 {
		t.Fatalf("the boundary sweep has %d entries, want %d", len(swept), 3+3*3+2)
	}
}

// TestRatchetKeyNonceAndSuccessorAreDerivedAtEveryGenerationBoundary asserts that at each
// end of the counter the three derivations are still the RFC 9420 section 9.1 ones and that
// no two generations reach the same key and nonce pair.
//
// The ratchet is PLANTED at each boundary rather than stepped to it. Reaching 2^32-1 one
// step at a time is four billion HKDF calls, and the generations that need covering are
// precisely the ones no loop a test can run ever arrives at -- which is why the shipped
// defect this guards against was invisible: everything anyone ran stopped in the middle.
func TestRatchetKeyNonceAndSuccessorAreDerivedAtEveryGenerationBoundary(t *testing.T) {
	crypto := stTestCrypto(t)
	encryptionSecret := MustHex(t, stVectorEncryptionSecret)
	const leafCount = LeafCount(8)
	const leaf = LeafIndex(3)
	root := stRatchetRoot(t, crypto, encryptionSecret, leafCount, leaf, RatchetApplication)

	pairs := map[string]uint32{}
	nonces := map[string]uint32{}
	keys := map[string]uint32{}
	for _, generation := range stGenerationBoundaries() {
		tree := stNewTree(t, leafCount)
		r, err := tree.ratchetFor(leaf, RatchetApplication)
		if err != nil {
			t.Fatalf("ratchetFor: %v", err)
		}
		// the control on the planting: the fixture below derives from the same root the
		// ratchet is actually holding, so a disagreement is the step and not the fixture.
		if !bytes.Equal(r.secret, root) {
			t.Fatalf("the ratchet root is %x and this test derived %x", r.secret, root)
		}
		r.head = generation

		stepped, got, err := r.step()
		if err != nil {
			t.Fatalf("step at generation %d: %v", generation, err)
		}
		if stepped != generation {
			t.Fatalf("a ratchet at generation %d produced generation %d", generation, stepped)
		}
		wantKey := crypto.DeriveTreeSecret(root, "key", generation, crypto.KeySize())
		wantNonce := crypto.DeriveTreeSecret(root, "nonce", generation, crypto.NonceSize())
		wantNext := crypto.DeriveTreeSecret(root, "secret", generation, crypto.HashSize())
		if !bytes.Equal(got.key, wantKey) {
			t.Fatalf("generation %d key = %x, want %x", generation, got.key, wantKey)
		}
		if !bytes.Equal(got.nonce, wantNonce) {
			t.Fatalf("generation %d nonce = %x, want %x", generation, got.nonce, wantNonce)
		}
		if !bytes.Equal(r.secret, wantNext) {
			t.Fatalf("generation %d successor = %x, want %x", generation, r.secret, wantNext)
		}
		if stAllZero(got.nonce) || stAllZero(got.key) {
			t.Fatalf("generation %d produced a zero key or nonce", generation)
		}

		if previous, ok := pairs[string(got.key)+"|"+string(got.nonce)]; ok {
			t.Fatalf("generation %d reaches the same key and nonce pair as generation %d, which is the AEAD break this sweep exists for",
				generation, previous)
		}
		pairs[string(got.key)+"|"+string(got.nonce)] = generation
		// the halves separately as well. A pair stays unique if only one half moves, so a
		// nonce frozen across generations is invisible to the pair check while being the
		// exact defect that matters if the key ever stops moving too.
		if previous, ok := nonces[string(got.nonce)]; ok {
			t.Fatalf("generation %d repeats the nonce of generation %d", generation, previous)
		}
		nonces[string(got.nonce)] = generation
		if previous, ok := keys[string(got.key)]; ok {
			t.Fatalf("generation %d repeats the key of generation %d", generation, previous)
		}
		keys[string(got.key)] = generation
	}
	if len(pairs) != len(stGenerationBoundaries()) {
		t.Fatalf("swept %d generations and collected %d pairs", len(stGenerationBoundaries()), len(pairs))
	}
}

// TestRatchetKeysAreNeverRepeatedOverAContiguousSweep is the same property over an unbroken
// run rather than over sampled boundaries, so a body that is right at every edge this file
// names and wrong three steps in has nowhere to hide.
func TestRatchetKeysAreNeverRepeatedOverAContiguousSweep(t *testing.T) {
	crypto := stTestCrypto(t)
	encryptionSecret := MustHex(t, stVectorEncryptionSecret)
	const leafCount = LeafCount(8)
	const leaf = LeafIndex(6)
	const generations = 4096

	tree := stNewTree(t, leafCount)
	r, err := tree.ratchetFor(leaf, RatchetHandshake)
	if err != nil {
		t.Fatalf("ratchetFor: %v", err)
	}
	// the independent chain, walked forward from the root this test derived itself.
	secret := stRatchetRoot(t, crypto, encryptionSecret, leafCount, leaf, RatchetHandshake)
	pairs := map[string]uint32{}
	for want := uint32(0); want < generations; want++ {
		wantKey := crypto.DeriveTreeSecret(secret, "key", want, crypto.KeySize())
		wantNonce := crypto.DeriveTreeSecret(secret, "nonce", want, crypto.NonceSize())
		secret = crypto.DeriveTreeSecret(secret, "secret", want, crypto.HashSize())

		generation, got, err := r.step()
		if err != nil {
			t.Fatalf("step %d: %v", want, err)
		}
		if generation != want {
			t.Fatalf("step %d produced generation %d", want, generation)
		}
		if !bytes.Equal(got.key, wantKey) || !bytes.Equal(got.nonce, wantNonce) {
			t.Fatalf("generation %d disagrees with the independently derived chain", generation)
		}
		if previous, ok := pairs[string(got.key)+"|"+string(got.nonce)]; ok {
			t.Fatalf("generation %d repeats the key and nonce pair of generation %d", generation, previous)
		}
		pairs[string(got.key)+"|"+string(got.nonce)] = generation
	}
	if len(pairs) != generations {
		t.Fatalf("collected %d pairs over %d generations", len(pairs), generations)
	}
	if !bytes.Equal(r.secret, secret) {
		t.Fatalf("after %d generations the ratchet secret has drifted from the independent chain", generations)
	}
}

// TestRatchetRefusesToWrapTheGenerationCounter asserts the ratchet stops at 2^32-1 instead
// of rolling the counter back to zero.
//
// A wrap is not a lost message. It is the generation numbers on the wire starting again at
// zero under keys that have moved on, so every one of them collides with a number this
// receiver has already marked consumed, and the four billionth message silently becomes a
// replay of the first. Reaching the wrap is only possible by planting the head, which is
// why this is the boundary that shipped broken elsewhere on this project.
func TestRatchetRefusesToWrapTheGenerationCounter(t *testing.T) {
	last := ^uint32(0)
	tree := stNewTree(t, 8)
	r, err := tree.ratchetFor(4, RatchetApplication)
	if err != nil {
		t.Fatalf("ratchetFor: %v", err)
	}
	r.head = last - 2

	produced := []uint32{}
	var refusal error
	for range 5 {
		generation, keys, err := r.step()
		if err != nil {
			refusal = err
			if keys != nil {
				t.Fatalf("an exhausted step returned key material as well as %v", err)
			}
			break
		}
		produced = append(produced, generation)
	}
	if !errors.Is(refusal, ErrRatchetExhausted) {
		t.Fatalf("the step past the end of the counter answered %v, want ErrRatchetExhausted", refusal)
	}
	if want := []uint32{last - 2, last - 1, last}; !slices.Equal(produced, want) {
		t.Fatalf("the ratchet produced generations %v, want %v -- a zero in that list is the counter wrapping", produced, want)
	}
	if r.head != last {
		t.Fatalf("the head is %d after exhaustion, want it parked at %d; a head below that reclassifies every consumed generation as future", r.head, last)
	}
	if !r.exhausted {
		t.Fatal("the ratchet is not marked exhausted, so the next step will hand out a generation again")
	}
	// and the receiving path agrees rather than looping, with ONE sentinel across the whole
	// range of the counter: every generation this ratchet produced is a replay, including the
	// last one it parked on.
	//
	// That last one is the value the "below the head" test cannot classify -- head stops at
	// 2^32-1, so 2^32-1 is neither below it nor far enough above it to be refused as a skip,
	// and it used to fall through into the loop and come back as ErrRatchetExhausted. A
	// caller reading the sentinel to decide "is this a replay" got a different answer for the
	// last message of an epoch than for every other message of it.
	for _, generation := range []uint32{0, 1, last - 2, last - 1, last} {
		if _, err := r.commitFor(generation); !errors.Is(err, ErrRatchetGenerationConsumed) {
			t.Fatalf("generation %d on an exhausted ratchet answered %v, want ErrRatchetGenerationConsumed", generation, err)
		}
	}
	// exhaustion is still reported, and it is the SENDER's fact: there is no next generation,
	// so rekey. Losing that sentinel entirely would leave a sender looking at a refusal it
	// cannot tell from a transient one.
	if _, _, _, err := tree.NextSenderKey(4, RatchetApplication); !errors.Is(err, ErrRatchetExhausted) {
		t.Fatalf("the sender path on an exhausted ratchet answered %v, want ErrRatchetExhausted", err)
	}
	// and the two sentinels are distinguishable, so the loop above is not satisfied by an
	// error that answers to both.
	if errors.Is(ErrRatchetExhausted, ErrRatchetGenerationConsumed) ||
		errors.Is(ErrRatchetGenerationConsumed, ErrRatchetExhausted) {
		t.Fatal("the exhausted and consumed sentinels answer to each other, so neither assertion above separates them")
	}
}

// TestRatchetSpentSecretIsErasedInPlaceAndTheSuccessorIsNotThePredecessor is the forward
// secrecy half of the ratchet and the half no correctness test can see.
//
// The spent secret is held as an ALIAS and not as a copy. An in place erasure is invisible
// to a copy -- the plan's own TestRatchetStepDerivesKeyNonceAndSuccessor takes one, and
// passes unchanged against a step that never calls zeroizeSecret -- so the only way to
// observe it is to keep the slice header the ratchet itself was holding.
func TestRatchetSpentSecretIsErasedInPlaceAndTheSuccessorIsNotThePredecessor(t *testing.T) {
	observed := 0
	for _, kind := range stRatchetKinds(t) {
		tree := stNewTree(t, 8)
		r, err := tree.ratchetFor(7, kind)
		if err != nil {
			t.Fatalf("ratchetFor: %v", err)
		}
		for step := range 8 {
			spent := r.secret
			before := append([]byte(nil), spent...)
			if stAllZero(before) {
				t.Fatalf("the ratchet secret before step %d is already a run of zeros, so this step could observe no erasure", step)
			}
			if _, _, err := r.step(); err != nil {
				t.Fatalf("step %d: %v", step, err)
			}
			if !stAllZero(spent) {
				t.Fatalf("the spent ratchet secret of generation %d survived the step as %x; anyone taking the process now recomputes every generation from here back",
					step, spent)
			}
			if bytes.Equal(r.secret, before) {
				t.Fatalf("the successor at generation %d is its own predecessor, so the ratchet is not advancing", step)
			}
			if stAllZero(r.secret) {
				t.Fatalf("the successor at generation %d is a run of zeros, which every party in the world can compute", step)
			}
			observed++
		}
	}
	if observed != 8*len(stRatchetKinds(t)) {
		t.Fatalf("observed %d steps, want %d", observed, 8*len(stRatchetKinds(t)))
	}
}

// TestRatchetRetainsNoSpentSecretInAnyFieldOfTheTreesType is the other half of the same
// claim, at the scope this file learned to use: the TYPE and not a field name.
//
// The alias test next door sees a secret that was overwritten. It cannot see one that was
// COPIED somewhere first -- an archive map, a debug slice, a second field added by a later
// task -- and that shape passed all 750 tests of this package before secretTreeRetainedBytes
// existed. This one walks everything the tree still reaches.
func TestRatchetRetainsNoSpentSecretInAnyFieldOfTheTreesType(t *testing.T) {
	crypto := stTestCrypto(t)
	encryptionSecret := MustHex(t, stVectorEncryptionSecret)
	const leafCount = LeafCount(8)
	const leaf = LeafIndex(2)
	const steps = 12

	tree := stNewTree(t, leafCount)
	r, err := tree.ratchetFor(leaf, RatchetApplication)
	if err != nil {
		t.Fatalf("ratchetFor: %v", err)
	}
	// the chain this test derives for itself: chain[i] is the secret generation i is derived
	// from, so chain[0:steps] are all spent by the end and chain[steps] is the live one.
	chain := [][]byte{stRatchetRoot(t, crypto, encryptionSecret, leafCount, leaf, RatchetApplication)}
	for generation := uint32(0); generation < steps; generation++ {
		chain = append(chain, crypto.DeriveTreeSecret(chain[generation], "secret", generation, crypto.HashSize()))
	}
	for at := range steps {
		if _, _, err := r.step(); err != nil {
			t.Fatalf("step %d: %v", at, err)
		}
	}
	if !bytes.Equal(r.secret, chain[steps]) {
		t.Fatalf("the ratchet is not on the chain this test derived, so nothing below is measuring the right values")
	}

	held := secretTreeRetainedBytes(t, tree)
	for spent := range steps {
		for _, one := range held {
			if bytes.Contains(one.carried, chain[spent]) {
				t.Fatalf("the ratchet secret of generation %d is still held at %s after %d further steps",
					spent, one.where, steps-spent)
			}
		}
	}
	// the control: the LIVE secret is reachable, so "not found" above means erased rather
	// than meaning this walk never looked inside the ratchets at all.
	found := ""
	for _, one := range held {
		if bytes.Contains(one.carried, chain[steps]) {
			found = one.where
			break
		}
	}
	if found == "" {
		t.Fatal("the walk did not find the ratchet's live secret anywhere on the tree, so the sweep above holds vacuously")
	}
}

// TestNoTwoRatchetsShareAKeyNoncePairAtAnyGeneration is the cross-sender half of the AEAD
// safety property.
//
// Every generation of every leaf's two ratchets is collected into one table, so a descent
// that gave two leaves the same node secret, a table keyed on less than the leaf and the
// type, or a root expansion that ignored its label all land as a collision here. It is the
// defect that is silent by construction: two senders each producing a perfectly correct
// sequence, which happens to be the same sequence.
//
// It sweeps in two halves, and the second is here because the first is the middle of the
// range. Sixty-four consecutive generations from zero says nothing about the ends of a 32 bit
// counter, and the middle of the range is exactly where the nonce defect this project already
// shipped sat green through sixty-nine tests. The second half plants every ratchet at every
// boundary stGenerationBoundaries derives and adds those keys to the SAME two tables, so a
// collision between two senders at 2^32-1 is a failure here rather than a gap.
func TestNoTwoRatchetsShareAKeyNoncePairAtAnyGeneration(t *testing.T) {
	const leafCount = LeafCount(8)
	const generations = 64
	tree := stNewTree(t, leafCount)
	leaves := stLeavesOf(t, leafCount)
	kinds := stRatchetKinds(t)

	type owner struct {
		leaf       LeafIndex
		kind       RatchetType
		generation uint32
	}
	pairs := map[string]owner{}
	nonces := map[string]owner{}
	for _, leaf := range leaves {
		for _, kind := range kinds {
			for want := uint32(0); want < generations; want++ {
				generation, key, nonce, err := tree.NextSenderKey(leaf, kind)
				if err != nil {
					t.Fatalf("NextSenderKey(%d, %d): %v", leaf, kind, err)
				}
				if generation != want {
					t.Fatalf("leaf %d kind %d produced generation %d, want %d", leaf, kind, generation, want)
				}
				mine := owner{leaf: leaf, kind: kind, generation: generation}
				if previous, ok := pairs[string(key)+"|"+string(nonce)]; ok {
					t.Fatalf("leaf %d kind %d generation %d reaches the same key and nonce pair as leaf %d kind %d generation %d",
						mine.leaf, mine.kind, mine.generation, previous.leaf, previous.kind, previous.generation)
				}
				pairs[string(key)+"|"+string(nonce)] = mine
				// the nonce alone as well. Two senders on one nonce is safe only while their
				// keys differ, and "their keys differ" is the thing above that would already
				// have failed -- so a nonce collision here is a warning that a single further
				// defect completes the break.
				if previous, ok := nonces[string(nonce)]; ok {
					t.Fatalf("leaf %d kind %d generation %d repeats the nonce of leaf %d kind %d generation %d",
						mine.leaf, mine.kind, mine.generation, previous.leaf, previous.kind, previous.generation)
				}
				nonces[string(nonce)] = mine
			}
		}
	}
	if want := len(leaves) * len(kinds) * generations; len(pairs) != want {
		t.Fatalf("collected %d distinct pairs over %d leaves, %d ratchet types and %d generations, want %d",
			len(pairs), len(leaves), len(kinds), generations, want)
	}

	// and the same table carries on to the ENDS of the counter.
	//
	// Sixty-four consecutive generations is the middle of the range, and the middle of the
	// range is exactly where the 2^32 nonce defect on this project sat green through sixty-
	// nine tests. A cross-ratchet collision needs two ratchets to reach one key and nonce
	// pair, and nothing about the low generations says the high ones cannot -- the generation
	// is bound into all three derivations, so the arithmetic that binds it is the thing being
	// swept here, at every boundary the counter has rather than at the ones a loop can walk
	// to. The heads are PLANTED, because stepping to 2^32-1 is four billion HKDF calls.
	planted := 0
	for _, boundary := range stGenerationBoundaries() {
		for _, leaf := range leaves {
			for _, kind := range kinds {
				r, err := tree.ratchetFor(leaf, kind)
				if err != nil {
					t.Fatalf("ratchetFor(%d, %d): %v", leaf, kind, err)
				}
				// the exhausted flag is cleared with the head, because a boundary sweep that
				// stopped at the first ratchet to reach 2^32-1 would cover the last boundary
				// for one ratchet and none of the others.
				r.head = boundary
				r.exhausted = false
				generation, keys, err := r.step()
				if err != nil {
					t.Fatalf("step at boundary %d for leaf %d kind %d: %v", boundary, leaf, kind, err)
				}
				if generation != boundary {
					t.Fatalf("a ratchet planted at %d produced generation %d", boundary, generation)
				}
				mine := owner{leaf: leaf, kind: kind, generation: generation}
				if previous, ok := pairs[string(keys.key)+"|"+string(keys.nonce)]; ok {
					t.Fatalf("leaf %d kind %d generation %d reaches the same key and nonce pair as leaf %d kind %d generation %d",
						mine.leaf, mine.kind, mine.generation, previous.leaf, previous.kind, previous.generation)
				}
				pairs[string(keys.key)+"|"+string(keys.nonce)] = mine
				if previous, ok := nonces[string(keys.nonce)]; ok {
					t.Fatalf("leaf %d kind %d generation %d repeats the nonce of leaf %d kind %d generation %d",
						mine.leaf, mine.kind, mine.generation, previous.leaf, previous.kind, previous.generation)
				}
				nonces[string(keys.nonce)] = mine
				planted++
			}
		}
	}
	if want := len(stGenerationBoundaries()) * len(leaves) * len(kinds); planted != want {
		t.Fatalf("planted %d boundary generations, want %d", planted, want)
	}
	if want := len(leaves)*len(kinds)*generations + planted; len(pairs) != want {
		t.Fatalf("collected %d distinct pairs once the boundaries were swept, want %d", len(pairs), want)
	}
}

// TestSenderAndReceiverAgreeForEveryLeafAndKindAndDisagreeAcrossThem is the routing half:
// the receiving path must reach the SAME key the sending path produced for that leaf and
// that type, and a different one for every other leaf and type.
//
// The negative half is what makes it a routing test rather than a round trip. A receiverKey
// that ignored its leaf argument would agree with a sender for one leaf and be wrong for the
// other seven, and a round trip over a single leaf reports that as green.
func TestSenderAndReceiverAgreeForEveryLeafAndKindAndDisagreeAcrossThem(t *testing.T) {
	crypto := stTestCrypto(t)
	encryptionSecret := MustHex(t, stVectorEncryptionSecret)
	const leafCount = LeafCount(8)
	const generations = 3
	leaves := stLeavesOf(t, leafCount)
	kinds := stRatchetKinds(t)

	sender, err := NewSecretTree(crypto, leafCount, encryptionSecret)
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	type slot struct {
		leaf       LeafIndex
		kind       RatchetType
		generation uint32
	}
	sent := map[slot][]byte{}
	for _, leaf := range leaves {
		for _, kind := range kinds {
			for range generations {
				generation, key, nonce, err := sender.NextSenderKey(leaf, kind)
				if err != nil {
					t.Fatalf("NextSenderKey: %v", err)
				}
				sent[slot{leaf: leaf, kind: kind, generation: generation}] = append(append([]byte(nil), key...), nonce...)
			}
		}
	}

	matched := 0
	for at, want := range sent {
		receiver, err := NewSecretTree(crypto, leafCount, encryptionSecret)
		if err != nil {
			t.Fatalf("NewSecretTree: %v", err)
		}
		key, nonce, err := receiver.receiverKey(at.leaf, at.kind, at.generation)
		if err != nil {
			t.Fatalf("receiverKey(%d, %d, %d): %v", at.leaf, at.kind, at.generation, err)
		}
		if got := append(append([]byte(nil), key...), nonce...); !bytes.Equal(got, want) {
			t.Fatalf("leaf %d kind %d generation %d: the receiver derived %x and the sender produced %x",
				at.leaf, at.kind, at.generation, got, want)
		}
		matched++
		// and every other slot answers something else, so the agreement above is about this
		// leaf and this type rather than about the tree having one keystream.
		for other := range sent {
			if other == at {
				continue
			}
			if bytes.Equal(sent[other], want) {
				t.Fatalf("leaf %d kind %d generation %d and leaf %d kind %d generation %d produce the same key and nonce",
					at.leaf, at.kind, at.generation, other.leaf, other.kind, other.generation)
			}
		}
	}
	if want := len(leaves) * len(kinds) * generations; matched != want {
		t.Fatalf("matched %d slots, want %d", matched, want)
	}
}

// TestRatchetReadsItsKeyAndNonceWidthsOffTheProviderItWasHanded is the differential the
// registry cannot supply, for the two lengths the ratchet reads on every single step.
//
// Both registered suites fix Nn at 12, and 0x0003 fixes Nk at 32 -- which is also Nh, also
// the length of every vector in this file, and also the literal a body would have written
// down. So inside the registry a read of NonceSize() and a written 12 are one number, and
// nothing else in this tree separates them. This provider is assembled at an Nk and an Nn no
// registered suite has, so each substitution is a different number rather than the same one.
func TestRatchetReadsItsKeyAndNonceWidthsOffTheProviderItWasHanded(t *testing.T) {
	crypto := &suiteCryptoProvider{params: &ksWelcomeSyntheticParams, random: rand.Reader}
	nk, nn, nh := crypto.KeySize(), crypto.NonceSize(), crypto.HashSize()
	// the substitutions this provider has to be able to see. A length here that coincided
	// with another would leave the assertions below satisfied by the literal they exist to
	// catch, which is how a differential goes quiet without failing.
	for _, one := range []struct {
		name  string
		value int
	}{
		{name: "this suite's Nn", value: nn},
		{name: "this suite's Nh", value: nh},
		{name: "the registered chacha suite's Nk", value: 32},
		{name: "the registered nonce size", value: 12},
	} {
		if one.value == nk {
			t.Fatalf("%s is %d, the same as this fixture's Nk, so this differential is blind to it", one.name, one.value)
		}
	}
	for _, one := range []struct {
		name  string
		value int
	}{
		{name: "this suite's Nh", value: nh},
		{name: "the registered nonce size", value: 12},
	} {
		if one.value == nn {
			t.Fatalf("%s is %d, the same as this fixture's Nn, so this differential is blind to it", one.name, one.value)
		}
	}

	encryptionSecret := bytes.Repeat([]byte{0x5a}, nh)
	tree, err := NewSecretTree(crypto, 8, encryptionSecret)
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	generation, key, nonce, err := tree.NextSenderKey(5, RatchetApplication)
	if err != nil {
		t.Fatalf("NextSenderKey: %v", err)
	}
	if generation != 0 {
		t.Fatalf("generation = %d, want 0", generation)
	}
	if len(key) != nk {
		t.Fatalf("the key is %d bytes for a suite whose Nk is %d", len(key), nk)
	}
	if len(nonce) != nn {
		t.Fatalf("the nonce is %d bytes for a suite whose Nn is %d", len(nonce), nn)
	}
	r, err := tree.ratchetFor(5, RatchetApplication)
	if err != nil {
		t.Fatalf("ratchetFor: %v", err)
	}
	if len(r.secret) != nh {
		t.Fatalf("the successor secret is %d bytes for a suite whose Nh is %d", len(r.secret), nh)
	}

	// and the width moved INSIDE the preimage with it, not only in the size of the answer:
	// KDFLabel carries the length, so a body deriving 32 bytes and a body deriving Nk bytes
	// disagree in every byte rather than in a prefix.
	root := stRatchetRoot(t, crypto, encryptionSecret, 8, 5, RatchetApplication)
	if want := crypto.DeriveTreeSecret(root, "key", 0, nk); !bytes.Equal(key, want) {
		t.Fatalf("the key is %x, want %x", key, want)
	}
	if want := crypto.DeriveTreeSecret(root, "nonce", 0, nn); !bytes.Equal(nonce, want) {
		t.Fatalf("the nonce is %x, want %x", nonce, want)
	}
	atThirtyTwo := crypto.DeriveTreeSecret(root, "key", 0, 32)
	if bytes.Equal(atThirtyTwo[:nk], key) {
		t.Fatalf("deriving 32 bytes and deriving %d agree on their first %d, so a hardcoded key size would be a truncation this test could not see", nk, nk)
	}
	atTwelve := crypto.DeriveTreeSecret(root, "nonce", 0, 12)
	if bytes.Equal(atTwelve[:nn], nonce) {
		t.Fatalf("deriving 12 bytes and deriving %d agree on their first %d, so a hardcoded nonce size would be a truncation this test could not see", nn, nn)
	}

	// the receiving path reads the same two lengths; it is a separate call site and has
	// silently disagreed with its sending twin elsewhere on this project.
	receiver, err := NewSecretTree(crypto, 8, encryptionSecret)
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	gotKey, gotNonce, err := receiver.receiverKey(5, RatchetApplication, 0)
	if err != nil {
		t.Fatalf("receiverKey: %v", err)
	}
	if len(gotKey) != nk || len(gotNonce) != nn {
		t.Fatalf("the receiver produced a %d byte key and a %d byte nonce, want %d and %d", len(gotKey), len(gotNonce), nk, nn)
	}
}

// ---------------------------------------------------------------------------
// the generation window: its bound, its eviction and its edges
// ---------------------------------------------------------------------------

// TestReceiverKeySkipBoundIsExactlyMaxGenerationSkip pins both sides of the bound, DERIVED
// from the constant rather than typed.
//
// Off by one in the permissive direction is still a bound and costs an extra thousand KDF
// calls; off by one the other way drops a message that legitimately arrived, and the sender
// has no way to learn it happened. The bound is also checked to be measured from the HEAD
// and not from zero, which is the version that silently stops working after the first
// thousand messages of an epoch.
func TestReceiverKeySkipBoundIsExactlyMaxGenerationSkip(t *testing.T) {
	tree := stNewTree(t, 8)
	if _, _, err := tree.receiverKey(1, RatchetApplication, MaxGenerationSkip); err != nil {
		t.Fatalf("a skip of exactly MaxGenerationSkip (%d) was refused: %v", MaxGenerationSkip, err)
	}
	if _, _, err := tree.receiverKey(3, RatchetApplication, MaxGenerationSkip+1); !errors.Is(err, ErrRatchetGenerationTooFarAhead) {
		t.Fatalf("a skip of MaxGenerationSkip+1 (%d) answered %v, want ErrRatchetGenerationTooFarAhead", MaxGenerationSkip+1, err)
	}
	// leaf 1's head is now one past the generation it served, and the same distance ahead of
	// THAT is accepted again.
	head, err := tree.SenderGeneration(1, RatchetApplication)
	if err != nil {
		t.Fatalf("SenderGeneration: %v", err)
	}
	if head != MaxGenerationSkip+1 {
		t.Fatalf("the head is %d after serving generation %d", head, MaxGenerationSkip)
	}
	if _, _, err := tree.receiverKey(1, RatchetApplication, head+MaxGenerationSkip); err != nil {
		t.Fatalf("a skip of MaxGenerationSkip from a head of %d was refused: %v", head, err)
	}
	moved, err := tree.SenderGeneration(1, RatchetApplication)
	if err != nil {
		t.Fatalf("SenderGeneration: %v", err)
	}
	if moved <= head {
		t.Fatalf("the head did not move past %d, so the check below is the same one as above", head)
	}
	if _, _, err := tree.receiverKey(1, RatchetApplication, moved+MaxGenerationSkip+1); !errors.Is(err, ErrRatchetGenerationTooFarAhead) {
		t.Fatalf("a skip of MaxGenerationSkip+1 from a head of %d answered %v, want ErrRatchetGenerationTooFarAhead", moved, err)
	}
}

// TestReceiverKeyWindowIsBoundedAndEvictsTheOldest asserts the retained skipped keys are
// capped at RatchetWindowSize and that what goes when the cap is reached is the oldest.
//
// Both edges matter and they fail in opposite directions. A window that never evicts is a
// memory bound chosen by whoever sends the messages: one silent sender, four billion
// retained keys. A window that evicts too eagerly throws away messages that legitimately
// arrived out of order, which the sender never learns about.
//
// The number of requests is DERIVED from the two constants. The plan's own version of this
// test asked for exactly MaxGenerationSkip and then required generation 0 to be gone -- with
// the two constants equal, that request retains exactly RatchetWindowSize keys and evicts
// nothing, so the assertion could not hold against any implementation. What is written here
// computes how many maximal skips it takes to exceed the window, and refuses to run if that
// count would not exceed it.
func TestReceiverKeyWindowIsBoundedAndEvictsTheOldest(t *testing.T) {
	tree := stNewTree(t, 8)
	const leaf = LeafIndex(1)
	const kind = RatchetApplication

	// each maximal skip retains exactly MaxGenerationSkip keys, so this is how many of them
	// it takes to push the retained set past the window.
	hops := (RatchetWindowSize + int(MaxGenerationSkip)) / int(MaxGenerationSkip)
	retainedWithoutEviction := hops * int(MaxGenerationSkip)
	if retainedWithoutEviction <= RatchetWindowSize {
		t.Fatalf("this sequence retains at most %d keys and the window holds %d, so it could not observe an eviction",
			retainedWithoutEviction, RatchetWindowSize)
	}

	requested := []uint32{}
	head := uint32(0)
	for range hops {
		asked := head + MaxGenerationSkip
		if _, _, err := tree.receiverKey(leaf, kind, asked); err != nil {
			t.Fatalf("receiverKey(%d): %v", asked, err)
		}
		requested = append(requested, asked)
		head = asked + 1
	}

	r, err := tree.ratchetFor(leaf, kind)
	if err != nil {
		t.Fatalf("ratchetFor: %v", err)
	}
	if len(r.window) > RatchetWindowSize {
		t.Fatalf("the window holds %d entries after %d maximal skips, want at most %d; unbounded retention is a memory bound the sender chooses",
			len(r.window), hops, RatchetWindowSize)
	}
	if len(r.window) != RatchetWindowSize {
		t.Fatalf("the window holds %d entries after overflowing, want exactly its bound of %d; evicting more than it has to loses messages that arrived",
			len(r.window), RatchetWindowSize)
	}

	// every generation this sequence skipped and did not consume, oldest first.
	skipped := []uint32{}
	for generation := uint32(0); generation < head; generation++ {
		if !slices.Contains(requested, generation) {
			skipped = append(skipped, generation)
		}
	}
	want := skipped[len(skipped)-RatchetWindowSize:]
	if got := slices.Sorted(maps.Keys(r.window)); !slices.Equal(got, want) {
		t.Fatalf("the window retained generations %d..%d, want the newest %d, %d..%d",
			got[0], got[len(got)-1], RatchetWindowSize, want[0], want[len(want)-1])
	}

	// and the eviction is visible through the API rather than only in the field: the oldest
	// skipped generation now reads as consumed, which is the visible gap the product needs,
	// and the newest retained one is still usable.
	if _, _, err := tree.receiverKey(leaf, kind, skipped[0]); !errors.Is(err, ErrRatchetGenerationConsumed) {
		t.Fatalf("generation %d survived a window that overflowed: err = %v", skipped[0], err)
	}
	if _, _, err := tree.receiverKey(leaf, kind, want[len(want)-1]); err != nil {
		t.Fatalf("the newest retained generation %d was not usable: %v", want[len(want)-1], err)
	}
}

// TestReceiverKeyWindowStaysBoundedUnderARepeatedMaximalSkip is the memory claim over a
// sender that never stops skipping, rather than over the single overflow next door.
func TestReceiverKeyWindowStaysBoundedUnderARepeatedMaximalSkip(t *testing.T) {
	tree := stNewTree(t, 8)
	const leaf = LeafIndex(3)
	const kind = RatchetHandshake
	r, err := tree.ratchetFor(leaf, kind)
	if err != nil {
		t.Fatalf("ratchetFor: %v", err)
	}
	// enough rounds to exceed the window several times over, derived so the count follows the
	// constants rather than standing beside them.
	rounds := 3 * (RatchetWindowSize + int(MaxGenerationSkip)) / int(MaxGenerationSkip)
	head := uint32(0)
	for at := range rounds {
		asked := head + MaxGenerationSkip
		if _, _, err := tree.receiverKey(leaf, kind, asked); err != nil {
			t.Fatalf("receiverKey(%d): %v", asked, err)
		}
		head = asked + 1
		if len(r.window) > RatchetWindowSize {
			t.Fatalf("after %d maximal skips the window holds %d entries, want at most %d", at+1, len(r.window), RatchetWindowSize)
		}
	}
	if rounds*int(MaxGenerationSkip) <= RatchetWindowSize {
		t.Fatalf("this sequence never exceeds the window, so the bound above was never tested")
	}
	if len(r.window) != RatchetWindowSize {
		t.Fatalf("the window settled at %d entries, want its bound of %d", len(r.window), RatchetWindowSize)
	}
}

// TestSecretTreeZeroizeLeavesNoRatchetOrWindowSecretAnywhereOnTheType is Zeroize at the
// scope of the type.
//
// The plan's TestSecretTreeZeroize reads two field names, tree.nodes and one ratchet's
// secret. Neither reaches the RETAINED WINDOW KEYS, which after an out of order delivery
// are live AEAD keys sitting in a map -- and an epoch leaving PastEpochWindow with its
// window keys intact is the same failure as one leaving with its ratchet secrets intact.
func TestSecretTreeZeroizeLeavesNoRatchetOrWindowSecretAnywhereOnTheType(t *testing.T) {
	tree := stNewTree(t, 8)
	// a sender, and a receiver that skipped -- so the tree holds ratchet secrets, retained
	// window keys and node secrets for the leaves nobody has touched.
	if _, _, _, err := tree.NextSenderKey(0, RatchetApplication); err != nil {
		t.Fatalf("NextSenderKey: %v", err)
	}
	if _, _, err := tree.receiverKey(2, RatchetHandshake, 6); err != nil {
		t.Fatalf("receiverKey: %v", err)
	}

	targets := map[string]string{}
	windowKeys := 0
	for at, r := range tree.ratchets {
		targets[string(r.secret)] = fmt.Sprintf("the ratchet secret of leaf %d kind %d", at.leaf, at.kind)
		for generation, keys := range r.window {
			targets[string(keys.key)] = fmt.Sprintf("the retained key of leaf %d kind %d generation %d", at.leaf, at.kind, generation)
			targets[string(keys.nonce)] = fmt.Sprintf("the retained nonce of leaf %d kind %d generation %d", at.leaf, at.kind, generation)
			windowKeys++
		}
	}
	for node, secret := range tree.nodes {
		targets[string(secret)] = fmt.Sprintf("the node secret of node %d", node)
	}
	if windowKeys == 0 {
		t.Fatal("no window keys were retained, so this test cannot say anything about them")
	}
	if len(targets) < 4 {
		t.Fatalf("only %d secrets to check, which is not a tree in the state this test needs", len(targets))
	}

	// the control: every one of them is reachable BEFORE the erasure, so an absence
	// afterwards is the erasure and not a walk that stopped looking.
	before := secretTreeRetainedBytes(t, tree)
	for secret, what := range targets {
		found := false
		for _, one := range before {
			if bytes.Contains(one.carried, []byte(secret)) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("the walk does not reach %s even before Zeroize, so its absence afterwards would prove nothing", what)
		}
	}

	tree.Zeroize()

	for _, one := range secretTreeRetainedBytes(t, tree) {
		for secret, what := range targets {
			if bytes.Contains(one.carried, []byte(secret)) {
				t.Fatalf("%s survived Zeroize and is still held at %s", what, one.where)
			}
		}
	}
}

// TestEveryExportedSecretTreeMethodRefusesAfterZeroize derives the class rather than naming
// it: every exported method of this type that can report an error must report ErrEpochErased
// once the epoch has been erased, and any method that cannot report an error must return
// nothing that could carry a secret.
//
// Enumerating the three methods that exist today is the shape this project keeps having to
// undo -- a table named "every rule" holding five of six. Reading the method set means the
// exported surface p6 and later plans add arrives already covered, and an added method that
// hands back key material with no way to refuse fails HERE rather than at the epoch that
// needed the refusal.
//
// The reason a refusal is required at all: Zeroize overwrites every node secret and every
// ratchet secret with Nh zero bytes, and expanding a run of zeros is not a weak derivation,
// it is one every party in the world can perform. A method that answered from an erased tree
// would return a real looking key, with no error, that offers no confidentiality at all.
func TestEveryExportedSecretTreeMethodRefusesAfterZeroize(t *testing.T) {
	errorType := reflect.TypeOf((*error)(nil)).Elem()
	pointer := reflect.TypeOf(&SecretTree{})
	if pointer.NumMethod() == 0 {
		t.Fatal("*SecretTree has no exported methods, so this gate read the wrong type")
	}

	call := func(tree *SecretTree, method reflect.Method) []reflect.Value {
		arguments := []reflect.Value{reflect.ValueOf(tree)}
		for at := 1; at < method.Type.NumIn(); at++ {
			arguments = append(arguments, reflect.Zero(method.Type.In(at)))
		}
		return method.Func.Call(arguments)
	}

	// the control, on a tree that was never erased. Without it a build where every method
	// returned ErrEpochErased unconditionally -- or where errors.Is answered yes to
	// everything -- would satisfy the sweep below completely.
	live := stNewTree(t, 8)
	for at := range pointer.NumMethod() {
		method := pointer.Method(at)
		for _, result := range call(live, method) {
			if !result.Type().Implements(errorType) || result.IsNil() {
				continue
			}
			if errors.Is(result.Interface().(error), ErrEpochErased) {
				t.Fatalf("%s answered ErrEpochErased on a tree that was never zeroized", method.Name)
			}
		}
	}

	erased := stNewTree(t, 8)
	if _, _, _, err := erased.NextSenderKey(0, RatchetApplication); err != nil {
		t.Fatalf("NextSenderKey: %v", err)
	}
	erased.Zeroize()

	refusals := 0
	for at := range pointer.NumMethod() {
		method := pointer.Method(at)
		refused := false
		carries := []string{}
		for _, result := range call(erased, method) {
			if result.Type().Implements(errorType) {
				if result.IsNil() {
					continue
				}
				if err := result.Interface().(error); !errors.Is(err, ErrEpochErased) {
					t.Errorf("%s on an erased tree answered %v, want ErrEpochErased", method.Name, err)
				} else {
					refused = true
					refusals++
				}
				continue
			}
			switch result.Kind() {
			case reflect.Slice, reflect.Array, reflect.String, reflect.Pointer,
				reflect.Interface, reflect.Map, reflect.Struct:
				carries = append(carries, result.Type().String())
			}
		}
		if !refused && len(carries) != 0 {
			t.Errorf("%s returns %v on an erased tree with no error to refuse through, so it can hand back key material derived from a run of zeros",
				method.Name, carries)
		}
	}
	if refusals == 0 {
		t.Fatal("no exported method refused an erased tree, so this gate is reporting on nothing")
	}
}

// TestZeroizeIsIdempotentAndRefusesEveryLaterDerivation states the two halves of the flag in
// terms a caller sees, so the field is not the only thing observing it.
func TestZeroizeIsIdempotentAndRefusesEveryLaterDerivation(t *testing.T) {
	tree := stNewTree(t, 8)
	if _, _, _, err := tree.NextSenderKey(1, RatchetApplication); err != nil {
		t.Fatalf("NextSenderKey: %v", err)
	}
	tree.Zeroize()
	tree.Zeroize()

	// a leaf that had a ratchet, and one that never did: the refusal must not depend on
	// whether the tree happens to hold a cached ratchet for the leaf being asked about.
	for _, leaf := range []LeafIndex{1, 6} {
		for _, kind := range stRatchetKinds(t) {
			if _, _, _, err := tree.NextSenderKey(leaf, kind); !errors.Is(err, ErrEpochErased) {
				t.Errorf("NextSenderKey(%d, %d) after Zeroize answered %v, want ErrEpochErased", leaf, kind, err)
			}
			if _, _, err := tree.receiverKey(leaf, kind, 0); !errors.Is(err, ErrEpochErased) {
				t.Errorf("receiverKey(%d, %d, 0) after Zeroize answered %v, want ErrEpochErased", leaf, kind, err)
			}
			if _, err := tree.SenderGeneration(leaf, kind); !errors.Is(err, ErrEpochErased) {
				t.Errorf("SenderGeneration(%d, %d) after Zeroize answered %v, want ErrEpochErased", leaf, kind, err)
			}
		}
	}
	// the unexported path underneath refuses too, so a later task reaching for the node
	// secrets directly cannot walk around the flag.
	if _, err := tree.takeLeafSecret(6); !errors.Is(err, ErrEpochErased) {
		t.Errorf("takeLeafSecret after Zeroize answered %v, want ErrEpochErased", err)
	}
}

// TestRatchetForRefusesAnUnknownRatchetType asserts the zero value and every code point past
// the two named ones are refused rather than defaulted.
//
// A default would be the worst kind of correct: a ContentType decoded one layer up that fell
// through to "handshake" would put an application message on the handshake ratchet, and both
// streams would then draw from one keystream while every individual derivation stayed right.
//
// It holds every refusal to the FIRST of ratchetFor's two, the way
// TestSecretTreeASecondTakeIsAlwaysTheConsumedRefusalAndNeverTheInvariantOne does next door.
// The second -- reached when the type check admitted a kind the stores below do not write --
// is unreachable, and it exists because the bare map read that stood in its place answered a
// nil ratchet and a NIL ERROR, which the caller then steps. Measured, on the commit that
// added it: with the type check replaced by a constant false and the two refusals
// indistinguishable, this test passed, because the invariant refusal caught what the type
// check no longer did and answered the same sentinel. Two guards over one property with no
// way to tell which fired is one guard that can be deleted silently.
//
// stRatchetKinds now DERIVES the admitted set by asking this same function, which is what
// makes every sweep in this file cover a third ratchet type the day one is added -- and which
// makes "every code point outside the admitted set is refused" true of any set whatsoever. So
// the membership half of this test is stated at NAMED code points instead: the zero value
// reaches no keystream, and the two the RFC names do. What is left over the whole code point
// space is the part the derivation cannot make vacuous -- which of the two refusals answers.
func TestRatchetForRefusesAnUnknownRatchetType(t *testing.T) {
	// the control on the matcher: the two refusals differ only in the sentinel the second
	// wraps, so an errors.Is that could not see it through the wrapping would report every
	// refusal as the ordinary one no matter which return produced it.
	both := fmt.Errorf("%w: ratchet type 9: %w", ErrSecretTreeLeafOutOfRange, errRatchetTypeHasNoRoot)
	if !errors.Is(both, ErrSecretTreeLeafOutOfRange) || !errors.Is(both, errRatchetTypeHasNoRoot) {
		t.Fatalf("the two sentinels are not both visible through %v, so this test cannot tell the two refusals apart", both)
	}
	if errors.Is(fmt.Errorf("%w: ratchet type 9", ErrSecretTreeLeafOutOfRange), errRatchetTypeHasNoRoot) {
		t.Fatal("the ordinary refusal answers to the invariant sentinel, so every refusal below would read as the invariant one")
	}

	known := stRatchetKinds(t)

	// the anti-default claim, stated at named code points rather than against the derived set,
	// because stRatchetKinds now DERIVES its answer from the same function under test and
	// "every code point outside the admitted set is refused" would be true of any set at all.
	// These three are the assertions that survive the derivation: the zero value has no
	// keystream, and the two the RFC names do.
	if slices.Contains(known, RatchetType(0)) {
		t.Fatal("the zero ratchet type reaches a keystream, so a content type that decoded to nothing routes onto a real one")
	}
	for _, named := range []RatchetType{RatchetHandshake, RatchetApplication} {
		if !slices.Contains(known, named) {
			t.Fatalf("ratchet type %d is one of this package's own and reaches no keystream, so every sweep scoped by the derived set is missing it", named)
		}
	}

	refused := 0
	for probe := range stRatchetTypeCodePoints() {
		kind := RatchetType(probe)
		tree := stNewTree(t, 8)
		_, _, _, err := tree.NextSenderKey(0, kind)
		if slices.Contains(known, kind) {
			if err != nil {
				t.Fatalf("ratchet type %d is one of this package's own and was refused: %v", kind, err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("ratchet type %d was accepted, so a decoded content type that fell through has a keystream to share", kind)
		}
		if !errors.Is(err, ErrSecretTreeLeafOutOfRange) {
			t.Fatalf("ratchet type %d was refused with %v, want ErrSecretTreeLeafOutOfRange", kind, err)
		}
		if errors.Is(err, errRatchetTypeHasNoRoot) {
			t.Fatalf("ratchet type %d was refused by the post store return, which is reachable only if the type check above it stopped admitting exactly the kinds the stores write: %v", kind, err)
		}
		refused++
	}
	if want := stRatchetTypeCodePoints() - len(known); refused != want {
		t.Fatalf("refused %d ratchet types, want %d", refused, want)
	}
	if refused == 0 {
		t.Fatal("every code point of the type reaches a keystream, so the refusal this test is about never fired")
	}
}

// TestSenderAndReceiverPathsShareOneRatchetPerLeafAndKind asserts the two entry points reach
// the SAME ratchet, so a leaf that both sends and receives on one tree cannot end up with two
// heads over one keystream.
func TestSenderAndReceiverPathsShareOneRatchetPerLeafAndKind(t *testing.T) {
	tree := stNewTree(t, 8)
	if _, _, _, err := tree.NextSenderKey(4, RatchetApplication); err != nil {
		t.Fatalf("NextSenderKey: %v", err)
	}
	head, err := tree.SenderGeneration(4, RatchetApplication)
	if err != nil {
		t.Fatalf("SenderGeneration: %v", err)
	}
	if head != 1 {
		t.Fatalf("head = %d after one send, want 1", head)
	}
	// generation 0 has been handed out already, so the receiving path must call it consumed
	// rather than deriving it a second time from a ratchet of its own.
	if _, _, err := tree.receiverKey(4, RatchetApplication, 0); !errors.Is(err, ErrRatchetGenerationConsumed) {
		t.Fatalf("receiverKey(0) after the sender took it answered %v, want ErrRatchetGenerationConsumed", err)
	}
	// and the other type of the same leaf is untouched by all of it.
	if other, err := tree.SenderGeneration(4, RatchetHandshake); err != nil || other != 0 {
		t.Fatalf("the handshake ratchet of leaf 4 is at generation %d (err %v), want 0", other, err)
	}
}

// TestEvictedWindowKeysAreErasedInPlace is the eviction half of the window's erasure, and it
// is written with ALIASES because nothing else can see it.
//
// A key evicted from the window is deleted from the map, so from that moment nothing on the
// tree reaches it and the type-derived walk this file scopes every other forward secrecy
// claim by reports it gone whether it was overwritten or merely dropped. Measured: with both
// zeroizeSecret calls removed from prune, every test in this package still passed. Holding
// the slice header the window itself was holding is the only way to ask the question.
func TestEvictedWindowKeysAreErasedInPlace(t *testing.T) {
	tree := stNewTree(t, 8)
	const leaf = LeafIndex(1)
	const kind = RatchetApplication
	if _, _, err := tree.receiverKey(leaf, kind, MaxGenerationSkip); err != nil {
		t.Fatalf("receiverKey(%d): %v", MaxGenerationSkip, err)
	}
	r, err := tree.ratchetFor(leaf, kind)
	if err != nil {
		t.Fatalf("ratchetFor: %v", err)
	}
	if len(r.window) != RatchetWindowSize {
		t.Fatalf("the window holds %d entries and this test needs it exactly full at %d", len(r.window), RatchetWindowSize)
	}
	held := map[uint32][][]byte{}
	for generation, keys := range r.window {
		held[generation] = [][]byte{keys.key, keys.nonce}
		if stAllZero(keys.key) || stAllZero(keys.nonce) {
			t.Fatalf("the retained key material of generation %d is already zero, so an erasure here would be invisible", generation)
		}
	}

	// a SMALL further skip, so some entries are evicted and some survive. A skip large enough
	// to clear the whole window would leave the survivor half of this test with nothing in it.
	const extra = uint32(4)
	if int(extra) >= RatchetWindowSize || extra > MaxGenerationSkip {
		t.Fatalf("a further skip of %d cannot both overflow a window of %d and leave survivors", extra, RatchetWindowSize)
	}
	head, err := tree.SenderGeneration(leaf, kind)
	if err != nil {
		t.Fatalf("SenderGeneration: %v", err)
	}
	if _, _, err := tree.receiverKey(leaf, kind, head+extra); err != nil {
		t.Fatalf("receiverKey(%d): %v", head+extra, err)
	}

	evicted, survived := 0, 0
	for generation, pair := range held {
		if _, ok := r.window[generation]; ok {
			survived++
			if stAllZero(pair[0]) || stAllZero(pair[1]) {
				t.Fatalf("generation %d is still in the window and its key material has been erased under it", generation)
			}
			continue
		}
		evicted++
		if !stAllZero(pair[0]) {
			t.Fatalf("the key of evicted generation %d survives the eviction as %x", generation, pair[0])
		}
		if !stAllZero(pair[1]) {
			t.Fatalf("the nonce of evicted generation %d survives the eviction as %x", generation, pair[1])
		}
	}
	if evicted != int(extra) {
		t.Fatalf("%d entries were evicted by a further skip of %d, want %d", evicted, extra, extra)
	}
	if survived != RatchetWindowSize-int(extra) {
		t.Fatalf("%d entries survived, want %d; without survivors the control above is vacuous", survived, RatchetWindowSize-int(extra))
	}
}

// TestZeroizeErasesTheRetainedWindowKeysInPlace is the same question for the epoch's end.
//
// Zeroize could satisfy every other test in this file by dropping the window rather than
// erasing it -- measured, with the erase loop replaced by a single clear(), the whole package
// stayed green -- and dropping it leaves live AEAD keys of an epoch that was supposed to be
// gone sitting in whatever the allocator does with them next.
func TestZeroizeErasesTheRetainedWindowKeysInPlace(t *testing.T) {
	tree := stNewTree(t, 8)
	const leaf = LeafIndex(2)
	const kind = RatchetHandshake
	if _, _, err := tree.receiverKey(leaf, kind, 6); err != nil {
		t.Fatalf("receiverKey: %v", err)
	}
	r, err := tree.ratchetFor(leaf, kind)
	if err != nil {
		t.Fatalf("ratchetFor: %v", err)
	}
	aliases := [][]byte{r.secret}
	for _, keys := range r.window {
		aliases = append(aliases, keys.key, keys.nonce)
	}
	// six skipped generations, each with a key and a nonce, plus the ratchet secret.
	if want := 1 + 2*6; len(aliases) != want {
		t.Fatalf("this ratchet holds %d secrets, want %d", len(aliases), want)
	}
	for at, one := range aliases {
		if stAllZero(one) {
			t.Fatalf("retained secret %d is already zero, so its erasure would be invisible", at)
		}
	}

	tree.Zeroize()

	for at, one := range aliases {
		if !stAllZero(one) {
			t.Fatalf("retained secret %d survived Zeroize as %x", at, one)
		}
	}
	// and it is unreachable as well as erased, so neither half stands in for the other.
	if len(r.window) != 0 {
		t.Fatalf("the window still holds %d entries after Zeroize", len(r.window))
	}
}

// ---------------------------------------------------------------------------
// the leaf secret's own erasure, and the memory a peer can make a receiver hold
// ---------------------------------------------------------------------------

// TestRatchetForErasesTheLeafSecretInPlaceOnceBothRootsExist is the erasure ratchetFor
// performs, observed rather than assumed, over every leaf of every swept width.
//
// The leaf secret is the single value that regenerates BOTH of that leaf's ratchet roots --
// that sender's whole epoch keystream -- and secret_tree.go says it "stops existing here".
// Nothing observed that. Measured: with zeroizeSecret(leafSecret) replaced by
// `_ = leafSecret`, every test of this package and of ../message passed, byte identical to
// the clean run. It is the fourth member of a class whose other three each needed a
// purpose-made alias test after surviving a first pass -- the spent ratchet secret, the
// evicted window keys and the window keys Zeroize clears -- and it was the one left out.
//
// It has to be an ALIAS, for the reason the other three do. takeLeafSecret hands the caller
// the slice it had in tree.nodes and deletes the map entry, so from that moment nothing the
// tree reaches names those bytes and the type-derived walk this file scopes its other forward
// secrecy claims by reports the secret gone whether it was overwritten or merely dropped. The
// slice header the map itself was holding is the only thing that can tell the two apart.
//
// The alias is taken from a leaf whose secret is ALREADY IN THE TREE, which is what a sibling
// take arranges: expanding their common parent stores both children, so after the sibling is
// consumed the leaf's own node secret is sitting in tree.nodes waiting for it. A one-leaf tree
// needs no sibling, because there the root IS the leaf.
//
// The two ratchet roots are compared against an independent derivation as well, so the
// opposite edit -- erasing the leaf secret BEFORE the roots are expanded from it -- fails here
// rather than passing the erasure half of this test.
func TestRatchetForErasesTheLeafSecretInPlaceOnceBothRootsExist(t *testing.T) {
	crypto := stTestCrypto(t)
	encryptionSecret := MustHex(t, stVectorEncryptionSecret)
	kinds := stRatchetKinds(t)
	observed := 0
	for _, leafCount := range stSweptLeafCounts() {
		for _, leaf := range stLeavesOf(t, leafCount) {
			for _, asked := range kinds {
				tree := stNewTree(t, leafCount)
				if leafCount > 1 {
					// the leaf-level sibling, which in a full tree is the leaf index with its
					// low bit flipped. Consuming it expands their common parent and leaves
					// this leaf's own node secret held.
					if _, err := tree.ratchetFor(leaf^1, asked); err != nil {
						t.Fatalf("width %d: ratchetFor(sibling of %d, %d): %v", leafCount, leaf, asked, err)
					}
				}
				alias, held := tree.nodes[leaf.NodeIndex()]
				if !held {
					t.Fatalf("width %d: leaf %d's node secret is not in the tree before it is taken, so this test aliases nothing",
						leafCount, leaf)
				}
				want := stExpectedNodeSecret(t, crypto, encryptionSecret, leafCount, leaf.NodeIndex())
				if !bytes.Equal(alias, want) {
					t.Fatalf("width %d: the alias taken for leaf %d is %x, want that leaf's node secret %x",
						leafCount, leaf, alias, want)
				}
				if stAllZero(alias) {
					t.Fatalf("width %d: leaf %d's node secret is already a run of zeros, so its erasure would be invisible",
						leafCount, leaf)
				}

				if _, err := tree.ratchetFor(leaf, asked); err != nil {
					t.Fatalf("width %d: ratchetFor(%d, %d): %v", leafCount, leaf, asked, err)
				}

				// both roots were derived from the live secret before it went.
				for _, kind := range kinds {
					r, ok := tree.ratchets[ratchetKey{leaf: leaf, kind: kind}]
					if !ok {
						t.Fatalf("width %d: leaf %d has no %d ratchet after ratchetFor(%d)", leafCount, leaf, kind, asked)
					}
					if root := stRatchetRoot(t, crypto, encryptionSecret, leafCount, leaf, kind); !bytes.Equal(r.secret, root) {
						t.Fatalf("width %d: leaf %d's %d ratchet root is %x, want %x -- a root expanded from an already erased leaf secret is one every party can compute",
							leafCount, leaf, kind, r.secret, root)
					}
				}
				if !stAllZero(alias) {
					t.Fatalf("width %d: the leaf secret of leaf %d survived ratchetFor as %x; anyone taking the process now rebuilds BOTH of that sender's ratchets from it",
						leafCount, leaf, alias)
				}
				observed++
			}
		}
	}
	sweptLeaves := 0
	for _, leafCount := range stSweptLeafCounts() {
		sweptLeaves += int(leafCount)
	}
	if want := sweptLeaves * len(kinds); observed != want {
		t.Fatalf("observed %d erasures over the swept widths, want %d", observed, want)
	}
}

// stTotalRetainedWindowKeys is the number of skipped generation keys the WHOLE tree holds.
func stTotalRetainedWindowKeys(tree *SecretTree) int {
	total := 0
	for _, r := range tree.ratchets {
		total += len(r.window)
	}
	return total
}

// stRetainedByteCount is every byte the tree still reaches, summed, at the type-derived scope
// the rest of this file's forward secrecy claims use. A field added later that retained key
// material joins this count without anyone naming it.
func stRetainedByteCount(t *testing.T, tree *SecretTree) int {
	t.Helper()
	total := 0
	for _, one := range secretTreeRetainedBytes(t, tree) {
		total += len(one.carried)
	}
	return total
}

// stForgeAHeaderPerRatchet drives one maximal forged receiverKey skip at every leaf of every
// kind, which is what an unauthenticated peer can do with nothing but a leaf index and a
// generation number: ratchetFor -- and so takeLeafSecret -- runs before any generation check
// and before any AEAD, so every one of these is served far enough to fill a window.
//
// It returns how many ratchets it reached, so the callers can state what the retention WOULD
// have been without a tree wide bound rather than assert against a number typed in beside it.
func stForgeAHeaderPerRatchet(t *testing.T, tree *SecretTree, leafCount LeafCount) int {
	t.Helper()
	kinds := stRatchetKinds(t)
	reached := 0
	for _, leaf := range stLeavesOf(t, leafCount) {
		for _, kind := range kinds {
			if _, _, err := tree.receiverKey(leaf, kind, MaxGenerationSkip); err != nil {
				t.Fatalf("a forged header for leaf %d kind %d was refused: %v", leaf, kind, err)
			}
			reached++
		}
	}
	return reached
}

// TestRetainedWindowKeysAreBoundedForTheWholeTreeAndNotPerRatchet is the memory a peer can
// make a receiver hold, bounded.
//
// RatchetWindowSize bounds ONE ratchet, and the number of ratchets is not the receiver's
// choice: a peer picking leaf indices and generation numbers out of the air materialises every
// leaf's two ratchets and fills every one of their windows, and nothing it sent had to be
// authentic. Measured on the shape this replaces: a 64 leaf tree, four forged headers per
// ratchet, 131072 retained generation keys and 17 MB of heap -- linear in the group size, so
// a 1024 leaf group reaches roughly 270 MB from roughly 4096 forged headers.
//
// The control is what makes this more than an assertion that some number is small: the count
// this sequence would retain WITHOUT the tree wide bound is computed from the same run, and
// the test refuses to run if that count does not exceed the bound.
func TestRetainedWindowKeysAreBoundedForTheWholeTreeAndNotPerRatchet(t *testing.T) {
	for _, leafCount := range []LeafCount{8, 64} {
		tree := stNewTree(t, leafCount)
		reached := stForgeAHeaderPerRatchet(t, tree, leafCount)
		if want := int(leafCount) * len(stRatchetKinds(t)); reached != want {
			t.Fatalf("width %d: the forged headers reached %d ratchets, want %d", leafCount, reached, want)
		}
		unbounded := reached * int(MaxGenerationSkip)
		if unbounded <= MaxRetainedWindowKeys {
			t.Fatalf("width %d: this sequence retains at most %d keys without any tree wide bound and the bound is %d, so it could not observe one",
				leafCount, unbounded, MaxRetainedWindowKeys)
		}
		retained := stTotalRetainedWindowKeys(tree)
		if retained > MaxRetainedWindowKeys {
			t.Fatalf("width %d: %d retained generation keys after %d forged headers, want at most %d -- a per ratchet bound multiplied by a ratchet count the peer chooses is not a bound",
				leafCount, retained, reached, MaxRetainedWindowKeys)
		}
		if retained != MaxRetainedWindowKeys {
			t.Fatalf("width %d: %d retained generation keys, want the bound of %d held exactly; evicting past it loses messages that arrived",
				leafCount, retained, MaxRetainedWindowKeys)
		}
	}
}

// TestRetainedKeyMaterialDoesNotGrowWithTheGroupSize is the same claim at the scope of the
// TYPE and as a DIFFERENCE, which is the form the finding was written in: the retention was
// reported as linear in the leaf count, so what has to be shown is that it no longer is.
//
// Two trees get the identical adversarial sequence at two widths, and every byte either one
// still reaches is summed by the type-derived walk. What legitimately differs between them is
// the per sender state -- one ratchet secret per leaf per kind, which IS a function of the
// group and cannot be otherwise -- so the difference is required to be exactly that and
// nothing more. Any retention that scaled with the group in any other field, named or not,
// lands here as a surplus.
func TestRetainedKeyMaterialDoesNotGrowWithTheGroupSize(t *testing.T) {
	const small = LeafCount(8)
	const large = LeafCount(64)
	crypto := stTestCrypto(t)
	kinds := len(stRatchetKinds(t))

	measure := func(leafCount LeafCount) int {
		tree := stNewTree(t, leafCount)
		stForgeAHeaderPerRatchet(t, tree, leafCount)
		if retained := stTotalRetainedWindowKeys(tree); retained != MaxRetainedWindowKeys {
			t.Fatalf("width %d retained %d generation keys rather than saturating the bound at %d, so the two widths below are not being compared under the same pressure",
				leafCount, retained, MaxRetainedWindowKeys)
		}
		// every leaf was consumed by the sequence, so the node array holds nothing and the
		// difference between the widths cannot come from there.
		if len(tree.nodes) != 0 {
			t.Fatalf("width %d still holds %d node secrets after every leaf was reached", leafCount, len(tree.nodes))
		}
		return stRetainedByteCount(t, tree)
	}

	grew := measure(large) - measure(small)
	// one ratchet secret per extra leaf per kind, at the provider's hash size.
	want := int(large-small) * kinds * crypto.HashSize()
	if want == 0 {
		t.Fatal("the two widths differ by no per sender state at all, so the comparison below is vacuous")
	}
	if grew != want {
		t.Fatalf("a group eight times the size retains %d more bytes, want exactly %d -- the extra per sender ratchet secrets and nothing else. A surplus is retention that scales with a number the other members choose",
			grew, want)
	}
}

// TestATreeWideEvictionTakesFromTheFullestRatchetSoOneSenderCannotEvictAnother is the half of
// the tree wide bound that decides whether it is a defence or a new attack.
//
// A bound that evicted the globally oldest generation would hand a flooding sender a way to
// drop other members' messages: the handful of keys an honest out of order sender is holding
// are older than everything the flooder just produced, so they would go first and the honest
// sender's messages would arrive at a receiver that had thrown their keys away. Taking from
// the largest holder puts the pressure on whoever made it.
func TestATreeWideEvictionTakesFromTheFullestRatchetSoOneSenderCannotEvictAnother(t *testing.T) {
	tree := stNewTree(t, 8)
	const honest = LeafIndex(2)
	const flooder = LeafIndex(5)
	const kind = RatchetApplication
	// an ordinary out of order delivery: a few generations skipped, then the one that arrived.
	const honestSkip = uint32(4)
	if _, _, err := tree.receiverKey(honest, kind, honestSkip); err != nil {
		t.Fatalf("the honest sender's first message was refused: %v", err)
	}
	honestRatchet, err := tree.ratchetFor(honest, kind)
	if err != nil {
		t.Fatalf("ratchetFor(honest): %v", err)
	}
	if len(honestRatchet.window) != int(honestSkip) {
		t.Fatalf("the honest sender left %d generations retained, want %d", len(honestRatchet.window), honestSkip)
	}
	kept := map[uint32][][]byte{}
	for generation, keys := range honestRatchet.window {
		kept[generation] = [][]byte{keys.key, keys.nonce}
	}

	// the flooder now asks for maximal skips until the tree wide bound has been reached and
	// overrun several times over.
	rounds := 3 * (MaxRetainedWindowKeys + int(MaxGenerationSkip)) / int(MaxGenerationSkip)
	head := uint32(0)
	for range rounds {
		asked := head + MaxGenerationSkip
		if _, _, err := tree.receiverKey(flooder, kind, asked); err != nil {
			t.Fatalf("the flooder's request for generation %d was refused: %v", asked, err)
		}
		head = asked + 1
		if retained := stTotalRetainedWindowKeys(tree); retained > MaxRetainedWindowKeys {
			t.Fatalf("the tree retains %d generation keys mid flood, want at most %d", retained, MaxRetainedWindowKeys)
		}
	}
	if rounds*int(MaxGenerationSkip) <= MaxRetainedWindowKeys {
		t.Fatal("this flood never exceeds the tree wide bound, so nothing above was ever evicted")
	}
	if retained := stTotalRetainedWindowKeys(tree); retained != MaxRetainedWindowKeys {
		t.Fatalf("the tree settled at %d retained generation keys, want the bound of %d; without the bound reached, surviving it means nothing",
			retained, MaxRetainedWindowKeys)
	}

	// the honest sender's retained keys are all still there, still the same bytes, and still
	// usable through the exported surface.
	for generation := uint32(0); generation < honestSkip; generation++ {
		pair, ok := kept[generation]
		if !ok {
			t.Fatalf("generation %d was never retained, so this test never held it", generation)
		}
		if stAllZero(pair[0]) || stAllZero(pair[1]) {
			t.Fatalf("the honest sender's generation %d was erased under a flood from another leaf", generation)
		}
		key, nonce, err := tree.receiverKey(honest, kind, generation)
		if err != nil {
			t.Fatalf("the honest sender's generation %d was evicted by another leaf's flood: %v", generation, err)
		}
		if !bytes.Equal(key, pair[0]) || !bytes.Equal(nonce, pair[1]) {
			t.Fatalf("the honest sender's generation %d came back as different key material than the window held", generation)
		}
	}
}

// TestTheTreeWideEvictionErasesWhatItDropsInPlace is the erasure of the new eviction path,
// written with ALIASES because nothing else can see it.
//
// A key dropped from a window is gone from everything the tree reaches, so the type-derived
// walk reports it absent whether it was overwritten or merely deleted -- exactly the gap that
// let the per ratchet eviction's own missing erasure pass every test in this package. Both
// paths evict through one helper now, and this is the assertion that says so from the tree
// wide side.
//
// The two ratchets are each set UNDER the per ratchet bound and over the tree wide one when
// summed, so the only eviction that can fire in this test is the tree wide one.
func TestTheTreeWideEvictionErasesWhatItDropsInPlace(t *testing.T) {
	tree := stNewTree(t, 8)
	const kind = RatchetHandshake
	const first = LeafIndex(1)
	const second = LeafIndex(3)
	share := uint32(MaxRetainedWindowKeys * 3 / 4)
	if int(share) > RatchetWindowSize {
		t.Fatalf("a share of %d is over the per ratchet bound of %d, so that eviction would fire too and this test could not say which one ran",
			share, RatchetWindowSize)
	}
	if share > MaxGenerationSkip {
		t.Fatalf("a share of %d is over the skip bound of %d, so the requests below are refused", share, MaxGenerationSkip)
	}
	if 2*int(share) <= MaxRetainedWindowKeys {
		t.Fatalf("two shares of %d do not exceed the tree wide bound of %d, so nothing is evicted", share, MaxRetainedWindowKeys)
	}

	if _, _, err := tree.receiverKey(first, kind, share); err != nil {
		t.Fatalf("receiverKey(first): %v", err)
	}
	firstRatchet, err := tree.ratchetFor(first, kind)
	if err != nil {
		t.Fatalf("ratchetFor(first): %v", err)
	}
	if len(firstRatchet.window) != int(share) {
		t.Fatalf("the first ratchet retained %d generations, want %d", len(firstRatchet.window), share)
	}
	kept := map[uint32][][]byte{}
	for generation, keys := range firstRatchet.window {
		kept[generation] = [][]byte{keys.key, keys.nonce}
		if stAllZero(keys.key) || stAllZero(keys.nonce) {
			t.Fatalf("generation %d is already a run of zeros, so an erasure here would be invisible", generation)
		}
	}

	if _, _, err := tree.receiverKey(second, kind, share); err != nil {
		t.Fatalf("receiverKey(second): %v", err)
	}
	if retained := stTotalRetainedWindowKeys(tree); retained != MaxRetainedWindowKeys {
		t.Fatalf("the tree retains %d generation keys, want the bound of %d", retained, MaxRetainedWindowKeys)
	}

	evicted, survived := 0, 0
	for generation, pair := range kept {
		if _, ok := firstRatchet.window[generation]; ok {
			survived++
			if stAllZero(pair[0]) || stAllZero(pair[1]) {
				t.Fatalf("generation %d is still in the window and its key material has been erased under it", generation)
			}
			continue
		}
		evicted++
		if !stAllZero(pair[0]) {
			t.Fatalf("the key of generation %d survives a tree wide eviction as %x", generation, pair[0])
		}
		if !stAllZero(pair[1]) {
			t.Fatalf("the nonce of generation %d survives a tree wide eviction as %x", generation, pair[1])
		}
	}
	if evicted == 0 {
		t.Fatal("nothing was evicted from the first ratchet, so the erasure above was never asked about")
	}
	if survived == 0 {
		t.Fatal("the whole first ratchet was evicted, so the control that says a live entry is left alone is vacuous")
	}
}

// ---------------------------------------------------------------------------
// the lock discipline, in the one environment where the race detector cannot run
// ---------------------------------------------------------------------------

// The honest limit, stated before the gate rather than after it. `go test -race` is what
// would verify SecretTree's concurrency claim, and it cannot run here: -race requires cgo,
// and with CGO_ENABLED=1 the toolchain answers `cgo: C compiler "gcc" not found`. So the
// claim in the type's own comment -- every exported method takes stateLock and every
// unexported helper is reached only under it -- was verified by reading, and a claim verified
// by reading is a claim nothing re-checks when the file changes.
//
// The two below are what CAN be checked in this environment, and neither is the race
// detector. The first reads the discipline off the syntax tree, deriving both the guarded
// fields and the method set from the source rather than from a list; it sees a lock that was
// dropped from a method, and it cannot see a race that the discipline itself does not prevent.
// The second observes the OUTCOME a dropped lock produces -- two callers handed one generation
// number, which is a key and nonce reuse -- and it is a probabilistic observation of a
// deterministic rule, which is why it stands beside the gate rather than instead of it.

// stReceiverTypeName is the name of the type a method is declared on, through a pointer
// receiver and through a generic one.
func stReceiverTypeName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.StarExpr:
		return stReceiverTypeName(typed.X)
	case *ast.IndexExpr:
		return stReceiverTypeName(typed.X)
	case *ast.IndexListExpr:
		return stReceiverTypeName(typed.X)
	case *ast.Ident:
		return typed.Name
	}
	return ""
}

// stReceiverField reduces an expression to the field of the receiver whose storage it hangs
// off, so self.nodes, self.nodes[node], (self.nodes)[node] and self.stateLock.Lock all report
// the field they reach through.
//
// Without it a read spelled through an index, and the lock spelled through its own method,
// sit outside the matcher while naming exactly the same field -- and those are the two shapes
// the state of this type is actually touched in.
func stReceiverField(receiver string, expr ast.Expr) string {
	if receiver == "" {
		return ""
	}
	switch typed := expr.(type) {
	case *ast.ParenExpr:
		return stReceiverField(receiver, typed.X)
	case *ast.IndexExpr:
		return stReceiverField(receiver, typed.X)
	case *ast.SliceExpr:
		return stReceiverField(receiver, typed.X)
	case *ast.StarExpr:
		return stReceiverField(receiver, typed.X)
	case *ast.SelectorExpr:
		if ident, ok := typed.X.(*ast.Ident); ok && ident.Name == receiver {
			return typed.Sel.Name
		}
		return stReceiverField(receiver, typed.X)
	}
	return ""
}

// stLockRule is what the gate found out about one method of the type under it.
type stLockRule struct {
	name     string
	exported bool
	touches  bool
	locks    bool
}

// stLockDiscipline reads one type's lock discipline off the syntax tree.
//
// Nothing here is written down. The METHOD SET comes from the declarations, so a method added
// in a file added later is covered. The GUARDED FIELDS are derived as the fields the type's own
// methods write -- an assignment, an increment, a delete or a clear whose storage hangs off the
// receiver -- so a mutable field added later joins the rule with no list to extend, and the
// four the constructor writes once and never again stay out of it, which is what lets
// LeafCount answer with no lock while an exported method holds it. The LOCK is derived as the
// field this type calls Lock on.
//
// Reachability is transitive, because it has to be: the three methods that hand out key
// material touch none of the guarded fields themselves, they call ratchetFor. A rule that read
// one body would report all three as touching nothing and pass a version of this file with
// every lock deleted.
func stLockDiscipline(t *testing.T, sources []parsedSource, typeName string) (rules []stLockRule, guarded []string, lockField string) {
	t.Helper()
	type method struct {
		decl     *ast.FuncDecl
		receiver string
	}
	methods := map[string]method{}
	for _, source := range sources {
		for _, decl := range source.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 || fn.Body == nil {
				continue
			}
			if stReceiverTypeName(fn.Recv.List[0].Type) != typeName {
				continue
			}
			receiver := ""
			if len(fn.Recv.List[0].Names) == 1 {
				receiver = fn.Recv.List[0].Names[0].Name
			}
			if previous, ok := methods[fn.Name.Name]; ok {
				t.Fatalf("%s.%s is declared twice (%v), so this gate would judge one of the two",
					typeName, fn.Name.Name, previous.decl.Name.NamePos)
			}
			methods[fn.Name.Name] = method{decl: fn, receiver: receiver}
		}
	}
	if len(methods) == 0 {
		t.Fatalf("no method of %s was found, so this gate read nothing", typeName)
	}

	// the fields the type writes: that is what "mutable" means here, and it is the whole of
	// what the lock exists for.
	guardedSet := map[string]bool{}
	lockSet := map[string]bool{}
	for _, one := range methods {
		ast.Inspect(one.decl.Body, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.AssignStmt:
				for _, target := range typed.Lhs {
					if field := stReceiverField(one.receiver, target); field != "" {
						guardedSet[field] = true
					}
				}
			case *ast.IncDecStmt:
				if field := stReceiverField(one.receiver, typed.X); field != "" {
					guardedSet[field] = true
				}
			case *ast.CallExpr:
				if builtin, ok := typed.Fun.(*ast.Ident); ok && len(typed.Args) > 0 {
					if builtin.Name == "delete" || builtin.Name == "clear" {
						if field := stReceiverField(one.receiver, typed.Args[0]); field != "" {
							guardedSet[field] = true
						}
					}
				}
				if selector, ok := typed.Fun.(*ast.SelectorExpr); ok {
					if selector.Sel.Name == "Lock" || selector.Sel.Name == "Unlock" {
						if field := stReceiverField(one.receiver, selector.X); field != "" {
							lockSet[field] = true
						}
					}
				}
			}
			return true
		})
	}
	guarded = slices.Sorted(maps.Keys(guardedSet))
	locks := slices.Sorted(maps.Keys(lockSet))
	if len(locks) != 1 {
		t.Fatalf("%s calls Lock on %v, and this gate is written for exactly one lock", typeName, locks)
	}
	lockField = locks[0]
	if slices.Contains(guarded, lockField) {
		t.Fatalf("the lock field %s is also written as state, so the gate cannot tell the guard from the guarded", lockField)
	}

	// what each body reaches directly, and which sibling methods it calls.
	touches := map[string]bool{}
	calls := map[string][]string{}
	for name, one := range methods {
		ast.Inspect(one.decl.Body, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.SelectorExpr:
				if ident, ok := typed.X.(*ast.Ident); ok && ident.Name == one.receiver && guardedSet[typed.Sel.Name] {
					touches[name] = true
				}
			case *ast.CallExpr:
				selector, ok := typed.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				ident, ok := selector.X.(*ast.Ident)
				if !ok || ident.Name != one.receiver {
					return true
				}
				if _, isSibling := methods[selector.Sel.Name]; isSibling {
					calls[name] = append(calls[name], selector.Sel.Name)
				}
			}
			return true
		})
	}
	for changed := true; changed; {
		changed = false
		for name := range methods {
			if touches[name] {
				continue
			}
			for _, called := range calls[name] {
				if touches[called] {
					touches[name] = true
					changed = true
					break
				}
			}
		}
	}

	for name, one := range methods {
		rules = append(rules, stLockRule{
			name:     name,
			exported: ast.IsExported(name),
			touches:  touches[name],
			locks:    stTakesTheLockFirst(one.decl, one.receiver, lockField),
		})
	}
	slices.SortFunc(rules, func(a stLockRule, b stLockRule) int { return strings.Compare(a.name, b.name) })
	return rules, guarded, lockField
}

// stTakesTheLockFirst reports whether a body opens with the lock taken and its release
// deferred, in that order and as its first two statements.
//
// Both halves and the position are the rule. A Lock with no deferred Unlock leaks the lock
// down every error return of a function that has five of them, and a Lock that is not first
// leaves whatever ran ahead of it reading guarded state unguarded, which is the defect the
// gate is for rather than a stylistic preference.
func stTakesTheLockFirst(decl *ast.FuncDecl, receiver string, lockField string) bool {
	if decl.Body == nil || len(decl.Body.List) < 2 {
		return false
	}
	isCall := func(expr ast.Expr, name string) bool {
		call, ok := expr.(*ast.CallExpr)
		if !ok {
			return false
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != name {
			return false
		}
		return stReceiverField(receiver, selector.X) == lockField
	}
	taken, ok := decl.Body.List[0].(*ast.ExprStmt)
	if !ok || !isCall(taken.X, "Lock") {
		return false
	}
	released, ok := decl.Body.List[1].(*ast.DeferStmt)
	if !ok {
		return false
	}
	return isCall(released.Call, "Unlock")
}

// stLockViolations applies the rule, so the real type and the control source below are judged
// by one function rather than by two copies of it that could drift apart.
//
// The two halves fail in opposite directions and both are real. An exported method that
// reaches guarded state without the lock is the unguarded access. An unexported helper that
// takes the lock itself deadlocks the moment an exported method that already holds it calls
// down -- which is every path through this type -- so "the caller holds it" has to be uniform
// rather than a habit.
func stLockViolations(rules []stLockRule) []string {
	found := []string{}
	for _, rule := range rules {
		if !rule.touches {
			continue
		}
		if rule.exported && !rule.locks {
			found = append(found, rule.name+" reaches guarded state and does not take the lock first")
		}
		if !rule.exported && rule.locks {
			found = append(found, rule.name+" reaches guarded state and takes the lock itself, so an exported caller holding it deadlocks")
		}
	}
	slices.Sort(found)
	return found
}

// stPackageImplementationSources is every non-test go file of this package, parsed.
//
// The paths come from packageSourcePaths, which globs the directory, so a method moved into a
// file added later is scanned rather than silently unread. That is a defect this package has
// already paid for once: task 12 moved three methods into a second file and four provider
// invariants went on reading the first.
func stPackageImplementationSources(t *testing.T) []parsedSource {
	t.Helper()
	sources := []parsedSource{}
	for _, path := range packageSourcePaths(t) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		sources = append(sources, mustParseSource(t, path))
	}
	if len(sources) == 0 {
		t.Fatal("no implementation file of this package was parsed, so the gate below read nothing")
	}
	return sources
}

// stLockControlSource is a type with the shape of SecretTree and both violations in it, so
// what the gate can SEE is measured rather than assumed.
//
// A gate that reported no violation would report exactly what a gate over a correct type
// reports, which is the one outcome a gate like this must never reach by accident. The
// control also carries the two shapes that must NOT be reported -- an exported method that
// touches no guarded field and takes no lock, and an unexported helper that touches guarded
// state and correctly leaves the locking to its caller -- so a matcher that answered "in
// violation" for everything fails here too.
const stLockControlSource = `package control

import "sync"

type control struct {
	guard    sync.Mutex
	table    map[int][]byte
	settled  bool
	constant int
}

func (self *control) Correct() {
	self.guard.Lock()
	defer self.guard.Unlock()
	self.helper()
}

func (self *control) Unguarded() int {
	return len(self.table)
}

func (self *control) Reads() int {
	return self.constant
}

func (self *control) helper() {
	self.table[0] = nil
	delete(self.table, 1)
	self.settled = true
}

func (self *control) selfLocking() {
	self.guard.Lock()
	defer self.guard.Unlock()
	self.settled = false
}
`

// TestTheLockDisciplineGateSeesBothViolationsItIsWrittenFor is the control on the gate below.
func TestTheLockDisciplineGateSeesBothViolationsItIsWrittenFor(t *testing.T) {
	source := mustParseText(t, "lock_control.go", stLockControlSource)
	rules, guarded, lockField := stLockDiscipline(t, []parsedSource{source}, "control")

	if lockField != "guard" {
		t.Fatalf("the gate found the lock field %q, want guard", lockField)
	}
	// derived from the writes, so the field the control only ever reads is not in it.
	if want := []string{"settled", "table"}; !slices.Equal(guarded, want) {
		t.Fatalf("the gate derived the guarded fields %v, want %v", guarded, want)
	}

	found := stLockViolations(rules)
	want := []string{
		"Unguarded reaches guarded state and does not take the lock first",
		"selfLocking reaches guarded state and takes the lock itself, so an exported caller holding it deadlocks",
	}
	if !slices.Equal(found, want) {
		t.Fatalf("the gate reported %v over the control, want %v", found, want)
	}

	// and the transitive half is seen: Correct touches nothing itself and reaches the guarded
	// table through helper, so a gate that read one body would have nothing to judge it by.
	byName := map[string]stLockRule{}
	for _, rule := range rules {
		byName[rule.name] = rule
	}
	if !byName["Correct"].touches {
		t.Fatal("the gate does not see that Correct reaches guarded state through helper, so it would pass a type whose every exported method delegated")
	}
	if byName["Reads"].touches {
		t.Fatal("the gate reports a method that only reads a write-once field as reaching guarded state, so it would force a lock onto an accessor that must not take one")
	}
}

// TestEveryPathToTheSecretTreesGuardedStateObservesOneLockDiscipline is the discipline the
// type's own comment claims, read off the source rather than off the prose.
//
// See the note above this section for what this does and does not stand in for: the race
// detector cannot run in this environment, and a syntax gate is not one. What it does is stop
// the claim from being true only on the day somebody read it.
func TestEveryPathToTheSecretTreesGuardedStateObservesOneLockDiscipline(t *testing.T) {
	rules, guarded, lockField := stLockDiscipline(t, stPackageImplementationSources(t), "SecretTree")

	if len(guarded) == 0 {
		t.Fatal("no field of SecretTree is written by any of its methods, so the rule below ran over nothing")
	}
	exported, unexported, unguardedReaders := 0, 0, 0
	for _, rule := range rules {
		switch {
		case rule.touches && rule.exported:
			exported++
		case rule.touches:
			unexported++
		default:
			unguardedReaders++
		}
	}
	if exported == 0 {
		t.Fatal("no exported method of SecretTree reaches its guarded state, so half this rule ran over nothing")
	}
	if unexported == 0 {
		t.Fatal("no unexported helper of SecretTree reaches its guarded state, so the other half ran over nothing")
	}
	if unguardedReaders == 0 {
		t.Fatal("every method reaches guarded state, so the gate cannot show it distinguishes the ones that do not")
	}

	if found := stLockViolations(rules); len(found) != 0 {
		t.Fatalf("the %s discipline is broken by: %v", lockField, found)
	}
}

// TestNoTwoCallersAreHandedOneGenerationUnderConcurrentSenders is the outcome half, in the
// absence of the race detector.
//
// What a dropped lock produces here is not a corrupt map, it is two callers handed the SAME
// generation number for one leaf and one ratchet type -- which is one key and one nonce used
// twice, the failure this whole file exists to prevent. So the assertion is on the numbers
// rather than on the mechanism: every generation handed out exactly once, and the set of them
// contiguous from zero, which is what a lost update to the head breaks in both directions at
// the same time.
//
// Its limit, stated rather than left implied: a race that did not happen to interleave on a
// given run is a race this test reports as absent. It stands beside the syntax gate above,
// which is deterministic and sees the missing lock itself, rather than instead of it.
func TestNoTwoCallersAreHandedOneGenerationUnderConcurrentSenders(t *testing.T) {
	const leafCount = LeafCount(8)
	const workers = 8
	const perWorker = 48
	tree := stNewTree(t, leafCount)
	leaves := stLeavesOf(t, leafCount)
	kinds := stRatchetKinds(t)

	type slot struct {
		leaf LeafIndex
		kind RatchetType
	}
	type handout struct {
		slot       slot
		generation uint32
	}
	handed := make(chan handout, workers*perWorker*len(leaves)*len(kinds))
	failures := make(chan error, workers)

	var running sync.WaitGroup
	for range workers {
		running.Add(1)
		go func() {
			defer running.Done()
			for range perWorker {
				for _, leaf := range leaves {
					for _, kind := range kinds {
						generation, key, nonce, err := tree.NextSenderKey(leaf, kind)
						if err != nil {
							failures <- fmt.Errorf("NextSenderKey(%d, %d): %w", leaf, kind, err)
							return
						}
						if len(key) == 0 || len(nonce) == 0 {
							failures <- fmt.Errorf("NextSenderKey(%d, %d) answered %d key bytes and %d nonce bytes", leaf, kind, len(key), len(nonce))
							return
						}
						handed <- handout{slot: slot{leaf: leaf, kind: kind}, generation: generation}
						// a reader on the same lock, so the exported surface is exercised in
						// both directions rather than only by the writer.
						if _, err := tree.SenderGeneration(leaf, kind); err != nil {
							failures <- fmt.Errorf("SenderGeneration(%d, %d): %w", leaf, kind, err)
							return
						}
					}
				}
			}
		}()
	}
	running.Wait()
	close(handed)
	close(failures)
	for err := range failures {
		t.Fatalf("a concurrent sender failed: %v", err)
	}

	seen := map[slot]map[uint32]bool{}
	for one := range handed {
		if seen[one.slot] == nil {
			seen[one.slot] = map[uint32]bool{}
		}
		if seen[one.slot][one.generation] {
			t.Fatalf("leaf %d kind %d handed generation %d to two callers, which is one key and one nonce used twice",
				one.slot.leaf, one.slot.kind, one.generation)
		}
		seen[one.slot][one.generation] = true
	}
	if want := len(leaves) * len(kinds); len(seen) != want {
		t.Fatalf("%d leaf and kind pairs produced generations, want %d", len(seen), want)
	}
	for one, generations := range seen {
		if len(generations) != workers*perWorker {
			t.Fatalf("leaf %d kind %d produced %d generations, want %d", one.leaf, one.kind, len(generations), workers*perWorker)
		}
		for generation := range uint32(workers * perWorker) {
			if !generations[generation] {
				t.Fatalf("leaf %d kind %d never produced generation %d, so the head skipped a value while two callers shared another",
					one.leaf, one.kind, generation)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// task 23a: the MessageKeySource surface, and task 24: the sender data key
// ---------------------------------------------------------------------------

// stContentTypeCodePoints is every value a ContentType can hold, derived from the width of
// the underlying type rather than from a written 256, for the reason stRatchetTypeCodePoints
// is: a type widened sweeps the wider space with nothing here to edit.
func stContentTypeCodePoints() int {
	return int(^ContentType(0)) + 1
}

// stRatchetTypeOfContentType is the mapping RFC 9420 section 9.1 states, written here so the
// sweeps below compare the implementation against the RFC rather than against itself. A table
// derived from ratchetTypeOf would agree with every possible ratchetTypeOf.
var stRatchetTypeOfContentType = map[ContentType]RatchetType{
	ContentTypeApplication: RatchetApplication,
	ContentTypeProposal:    RatchetHandshake,
	ContentTypeCommit:      RatchetHandshake,
}

// TestContentTypeCarriesTheWireValuesTheRegistryGivesIt holds the three code points, which the
// compile time pins in key_schedule_deps_test.go cannot: a constant retyped is a build failure
// there, and a constant whose VALUE moved is a number that still compiles everywhere.
//
// It matters because this package declares ContentType on p6's behalf. Two declarations of one
// wire enum that disagree by a number are the exact drift that arrangement risks, and this is
// the assertion that turns it into a failure -- a p6 that lands with ContentTypeApplication at
// 0 collides with this file rather than quietly re-routing every application message.
//
// The set is DERIVED off the type through the package's own type checker rather than listed,
// so a fourth code point added to framing.go and left out of stRatchetTypeOfContentType
// is reported here instead of being skipped by every sweep below.
func TestContentTypeCarriesTheWireValuesTheRegistryGivesIt(t *testing.T) {
	if got := stContentTypeCodePoints(); got != 256 {
		t.Fatalf("ContentType holds %d code points, and RFC 9420 gives it one octet", got)
	}
	derived := registryConstantsOfType(t, "ContentType")
	want := map[string]uint64{
		"ContentTypeApplication": 1,
		"ContentTypeProposal":    2,
		"ContentTypeCommit":      3,
	}
	if len(derived) != len(want) {
		t.Errorf("the derivation read %v off ContentType, and RFC 9420 section 6 registers %v",
			derived, want)
	}
	for name, value := range want {
		got, declared := derived[name]
		if !declared {
			t.Errorf("this package declares no %s, which the interface registry names", name)
			continue
		}
		if got != value {
			t.Errorf("%s = %d, want %d; a code point that moved re-routes every message of that type",
				name, got, value)
		}
	}
	// and the reserved zero is not one of them, which is what makes an unparsed header a
	// refusal rather than generation 0 of a real ratchet.
	for name, value := range derived {
		if value == 0 {
			t.Errorf("%s is 0, and 0 is the reserved content type", name)
		}
	}
	// the mapping table this file's sweeps run over covers exactly the declared set.
	for name := range derived {
		found := false
		for contentType := range stRatchetTypeOfContentType {
			if uint64(contentType) == derived[name] {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is declared and stRatchetTypeOfContentType has no row for it, so every sweep below skips it",
				name)
		}
	}
	if len(stRatchetTypeOfContentType) != len(derived) {
		t.Errorf("stRatchetTypeOfContentType holds %d rows and ContentType declares %d code points",
			len(stRatchetTypeOfContentType), len(derived))
	}
}

// TestNextMessageKeyMapsContentTypeToRatchet asserts application content reaches the
// application ratchet and both handshake content types reach the handshake ratchet.
//
// Getting this mapping wrong would encrypt a commit under an application key that a receiver
// looks up in the other ratchet, and the failure would read as a bad tag -- that is, as
// tampering rather than as a bug.
//
// Two halves. The first compares NextMessageKey against NextSenderKey at the ratchet type the
// RFC names, over a second tree at the same encryption secret; it is the plan's own test and
// it catches every rerouting of the mapping that was tried against it -- application onto the
// handshake ratchet, the handshake pair onto the application one, and proposal split off
// alone. Measured, not assumed: the plan's version of this test is a real one.
//
// The second half states the sharing DIRECTLY rather than through the ratchet type name. All
// three content types go to ONE leaf, so proposal and commit have to come back as consecutive
// generations of a single keystream -- read off an independent tree -- and application has to
// come back different. That is what "proposals and commits share the handshake ratchet"
// means, and it is a stronger statement than "both name RatchetHandshake": two ratchet types
// seeded from one root would satisfy the first half and hand two senders one keystream. No
// single edit of secret_tree.go separates the two, which is why this is written as the
// property rather than left to the mapping -- the shape it guards against arrives as a THIRD
// ratchet type, and a third ratchet type is how stRatchetKinds came to be derived rather than
// listed.
func TestNextMessageKeyMapsContentTypeToRatchet(t *testing.T) {
	crypto := stTestCrypto(t)
	encryptionSecret := MustHex(t, stVectorEncryptionSecret)

	viaContentType, err := NewSecretTree(crypto, LeafCount(8), encryptionSecret)
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	viaRatchetType, err := NewSecretTree(crypto, LeafCount(8), encryptionSecret)
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}

	leaf := LeafIndex(0)
	for _, contentType := range slices.Sorted(maps.Keys(stRatchetTypeOfContentType)) {
		kind := stRatchetTypeOfContentType[contentType]
		gotKey, gotNonce, gotGeneration, err := viaContentType.NextMessageKey(contentType, leaf)
		if err != nil {
			t.Fatalf("NextMessageKey(%d): %v", contentType, err)
		}
		wantGeneration, wantKey, wantNonce, err := viaRatchetType.NextSenderKey(leaf, kind)
		if err != nil {
			t.Fatalf("NextSenderKey: %v", err)
		}
		if gotGeneration != wantGeneration {
			t.Fatalf("content type %d: generation = %d, want %d", contentType, gotGeneration, wantGeneration)
		}
		if !bytes.Equal(gotKey, wantKey) || !bytes.Equal(gotNonce, wantNonce) {
			t.Fatalf("content type %d does not map to ratchet type %d", contentType, kind)
		}
		leaf++
	}

	// the second half, on one leaf: which content types SHARE a ratchet.
	shared := stNewTree(t, 8)
	const onOneLeaf = LeafIndex(5)
	type drawn struct {
		key        []byte
		nonce      []byte
		generation uint32
	}
	answers := map[ContentType]drawn{}
	for _, contentType := range slices.Sorted(maps.Keys(stRatchetTypeOfContentType)) {
		key, nonce, generation, err := shared.NextMessageKey(contentType, onOneLeaf)
		if err != nil {
			t.Fatalf("NextMessageKey(%d) on one leaf: %v", contentType, err)
		}
		answers[contentType] = drawn{key: key, nonce: nonce, generation: generation}
	}
	if answers[ContentTypeApplication].generation != 0 {
		t.Errorf("application content drew generation %d of its own ratchet, want 0",
			answers[ContentTypeApplication].generation)
	}
	if answers[ContentTypeProposal].generation != 0 || answers[ContentTypeCommit].generation != 1 {
		t.Errorf("proposal drew generation %d and commit drew %d off one leaf; sharing the handshake ratchet means consecutive generations of it, and two 0s would mean two ratchets",
			answers[ContentTypeProposal].generation, answers[ContentTypeCommit].generation)
	}
	// and the keys themselves, so a shared generation COUNTER over two separate keystreams
	// does not read as a shared ratchet. The commit's key has to be the handshake ratchet's
	// generation 1, which this reads independently off a second tree.
	independent := stNewTree(t, 8)
	if _, _, _, err := independent.NextSenderKey(onOneLeaf, RatchetHandshake); err != nil {
		t.Fatalf("NextSenderKey: %v", err)
	}
	wantGeneration, wantKey, wantNonce, err := independent.NextSenderKey(onOneLeaf, RatchetHandshake)
	if err != nil {
		t.Fatalf("NextSenderKey: %v", err)
	}
	if wantGeneration != 1 {
		t.Fatalf("the independent handshake ratchet is at generation %d, want 1", wantGeneration)
	}
	if !bytes.Equal(answers[ContentTypeCommit].key, wantKey) ||
		!bytes.Equal(answers[ContentTypeCommit].nonce, wantNonce) {
		t.Error("a commit sent after a proposal on one leaf did not draw generation 1 of that leaf's handshake ratchet, so the two content types are not sharing one keystream")
	}
	if bytes.Equal(answers[ContentTypeApplication].key, answers[ContentTypeProposal].key) {
		t.Error("application and proposal content drew the same key off one leaf, which is one keystream protecting two message streams")
	}
}

// TestNextMessageKeyAndMessageKeyAgreeOnTheGenerationTheSenderSent is the round trip the
// framing layer actually makes: one member encrypts at the generation NextMessageKey returns,
// another looks that generation up with MessageKey, and the two have to be the same key and
// nonce or the AEAD refuses a message nobody tampered with.
//
// It is the assertion that fails if the two wrappers ever disagree about the ContentType to
// RatchetType mapping -- each is self consistent under a wrong shared mapping, and neither of
// the mapping tests above compares the encrypt path against the decrypt path.
func TestNextMessageKeyAndMessageKeyAgreeOnTheGenerationTheSenderSent(t *testing.T) {
	crypto := stTestCrypto(t)
	encryptionSecret := MustHex(t, stVectorEncryptionSecret)
	sender, err := NewSecretTree(crypto, LeafCount(8), encryptionSecret)
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	receiver, err := NewSecretTree(crypto, LeafCount(8), encryptionSecret)
	if err != nil {
		t.Fatalf("NewSecretTree: %v", err)
	}
	const leaf = LeafIndex(2)
	for _, contentType := range slices.Sorted(maps.Keys(stRatchetTypeOfContentType)) {
		for range 3 {
			key, nonce, generation, err := sender.NextMessageKey(contentType, leaf)
			if err != nil {
				t.Fatalf("NextMessageKey(%d): %v", contentType, err)
			}
			gotKey, gotNonce, err := receiver.MessageKey(contentType, leaf, generation)
			if err != nil {
				t.Fatalf("MessageKey(%d, %d): %v", contentType, generation, err)
			}
			if !bytes.Equal(key, gotKey) || !bytes.Equal(nonce, gotNonce) {
				t.Fatalf("content type %d generation %d: the receiver looked up a different key than the sender used",
					contentType, generation)
			}
			receiver.EraseMessageKey(contentType, leaf, generation)
		}
	}
}

// TestNextMessageKeyRefusesEveryContentTypeWithNoRatchet sweeps the WHOLE code point space
// rather than the two values the plan's version probed.
//
// The class is derived twice over and the two derivations are compared: the code points come
// from the width of the type, and which of them is legal comes from the constants the package
// declares. A fourth content type wired into ratchetTypeOf and left out of framing.go is
// reported here, and so is a content type declared and not wired in -- the plan's version,
// which probed 0 and 9, would report neither.
//
// Both wrappers are swept and so is the erase, because "refused" has to mean the same thing on
// all three: a NextMessageKey that refused and a MessageKey that answered would let a peer draw
// a real ratchet's generation 0 by naming a code point nobody has defined.
func TestNextMessageKeyRefusesEveryContentTypeWithNoRatchet(t *testing.T) {
	legal := map[ContentType]bool{}
	for name, value := range registryConstantsOfType(t, "ContentType") {
		if value >= uint64(stContentTypeCodePoints()) {
			t.Fatalf("%s = %d does not fit the type's own width", name, value)
		}
		legal[ContentType(value)] = true
	}
	if len(legal) == 0 {
		t.Fatal("no ContentType constant was derived, so this sweep would refuse every code point and pass")
	}
	refused, accepted := 0, 0
	for point := range stContentTypeCodePoints() {
		contentType := ContentType(point)
		tree := stNewTree(t, 8)
		_, _, _, nextErr := tree.NextMessageKey(contentType, 0)
		_, _, lookupErr := tree.MessageKey(contentType, 1, 0)
		if legal[contentType] {
			if nextErr != nil {
				t.Errorf("content type %d is declared and NextMessageKey refused it: %v", contentType, nextErr)
			}
			if lookupErr != nil {
				t.Errorf("content type %d is declared and MessageKey refused it: %v", contentType, lookupErr)
			}
			accepted++
			continue
		}
		if !errors.Is(nextErr, ErrUnknownContentType) {
			t.Errorf("NextMessageKey(%d) answered %v, want ErrUnknownContentType", contentType, nextErr)
		}
		if !errors.Is(lookupErr, ErrUnknownContentType) {
			t.Errorf("MessageKey(%d) answered %v, want ErrUnknownContentType", contentType, lookupErr)
		}
		// and the erase, which cannot report and must therefore be observed by what it does
		// NOT do: build a ratchet for a content type that has none.
		erasing := stNewTree(t, 8)
		erasing.EraseMessageKey(contentType, 3, 0)
		if len(erasing.ratchets) != 0 {
			t.Errorf("EraseMessageKey(%d) built %d ratchets for a content type with none",
				contentType, len(erasing.ratchets))
		}
		refused++
	}
	if accepted != len(legal) {
		t.Errorf("%d of the %d declared content types were accepted", accepted, len(legal))
	}
	if refused != stContentTypeCodePoints()-len(legal) {
		t.Errorf("%d code points were refused, want %d", refused, stContentTypeCodePoints()-len(legal))
	}
}

// TestMessageKeyDoesNotConsumeUntilCommitted asserts a lookup can be repeated until the caller
// says the message authenticated.
//
// The framing layer opens the AEAD and verifies the signature between the two calls, so a
// MessageKey that consumed would lose a real message every time a forged one arrived first: one
// packet from anyone who can write to the network, and the generation the honest sender used is
// gone. The commit is the far side of that, and it is the call that moves the head: an erase alone
// says one key is spent and says nothing about where the head stands, which is the half a forged
// header used to move.
func TestMessageKeyDoesNotConsumeUntilCommitted(t *testing.T) {
	tree := stNewTree(t, 8)
	first, firstNonce, err := tree.MessageKey(ContentTypeApplication, 3, 2)
	if err != nil {
		t.Fatalf("MessageKey: %v", err)
	}
	if stAllZero(first) || stAllZero(firstNonce) {
		t.Fatal("the first lookup answered zeros, so the comparison below would hold against an implementation that answers nothing")
	}
	second, secondNonce, err := tree.MessageKey(ContentTypeApplication, 3, 2)
	if err != nil {
		t.Fatalf("second MessageKey: %v", err)
	}
	if !bytes.Equal(first, second) || !bytes.Equal(firstNonce, secondNonce) {
		t.Fatal("two lookups of one generation disagreed")
	}

	if err := tree.CommitMessageKey(ContentTypeApplication, 3, 2); err != nil {
		t.Fatalf("CommitMessageKey: %v", err)
	}
	if _, _, err := tree.MessageKey(ContentTypeApplication, 3, 2); !errors.Is(err, ErrRatchetGenerationConsumed) {
		t.Fatalf("err after commit = %v, want ErrRatchetGenerationConsumed", err)
	}
	// the commit is scoped to the generation it names: the neighbours its own walk retained are
	// still there, so a commit that cleared the window would be caught here rather than read as
	// forward secrecy.
	if _, _, err := tree.MessageKey(ContentTypeApplication, 3, 1); err != nil {
		t.Errorf("committing generation 2 also took generation 1: %v", err)
	}
	// and receiverKey, which shares the ratchet, keeps its single use semantics across the
	// split that made MessageKey repeatable.
	if _, _, err := tree.receiverKey(3, RatchetApplication, 0); err != nil {
		t.Fatalf("receiverKey: %v", err)
	}
	if _, _, err := tree.receiverKey(3, RatchetApplication, 0); !errors.Is(err, ErrRatchetGenerationConsumed) {
		t.Errorf("receiverKey answered generation 0 twice: %v", err)
	}
}

// TestEraseMessageKeyZeroizesTheEntry asserts the erase clears the BYTES rather than only
// dropping the map entry, which is the whole point of it existing.
//
// It reads the window's own slices rather than the pair MessageKey answered with. That pair is
// a COPY -- see MessageKey, where handing the entry's storage out is what let one holder's
// erase turn another holder's key into zeros -- and a copy is the wrong instrument for this
// question: it goes on reading its own untouched bytes whether the entry behind it was erased
// or merely dropped, which is exactly the difference this test exists to see. The alias taken
// out of the window is the only thing that can tell the two apart, for the reason
// TestZeroizeErasesTheRetainedWindowKeysInPlace gives.
//
// The guards before the erase are what stop this from holding against an implementation that
// answers nothing: a key of the wrong width, a key that was already zeros, a key and a nonce
// that are one array, or an answer that turned out to BE the entry would each satisfy a pair
// of "is every byte zero" loops.
func TestEraseMessageKeyZeroizesTheEntry(t *testing.T) {
	crypto := stTestCrypto(t)
	tree := stNewTree(t, 8)
	const leaf = LeafIndex(4)
	const generation = uint32(1)
	// the window is filled by an ACCEPTED message that skipped past this generation, which since
	// the receive path became two phase is the only thing that retains anything: a lookup writes
	// nothing, so without the commit below there is no entry here for the erase to clear.
	if _, _, err := tree.MessageKey(ContentTypeCommit, leaf, generation+2); err != nil {
		t.Fatalf("the skipping lookup: %v", err)
	}
	if err := tree.CommitMessageKey(ContentTypeCommit, leaf, generation+2); err != nil {
		t.Fatalf("the skipping commit: %v", err)
	}
	answered, answeredNonce, err := tree.MessageKey(ContentTypeCommit, leaf, generation)
	if err != nil {
		t.Fatalf("MessageKey: %v", err)
	}
	r, err := tree.ratchetFor(leaf, RatchetHandshake)
	if err != nil {
		t.Fatalf("ratchetFor: %v", err)
	}
	entry, retained := r.window[generation]
	if !retained {
		t.Fatalf("generation %d is not in the window after a lookup that does not consume, so there is nothing here for the erase to clear", generation)
	}
	key, nonce := entry.key, entry.nonce
	if len(key) != crypto.KeySize() || len(nonce) != crypto.NonceSize() {
		t.Fatalf("the retained entry holds a %d byte key and a %d byte nonce, want %d and %d",
			len(key), len(nonce), crypto.KeySize(), crypto.NonceSize())
	}
	if stAllZero(key) || stAllZero(nonce) {
		t.Fatal("the entry's key or nonce was already all zeros before the erase, so the assertions below hold against an implementation that never derived anything")
	}
	if ksSharesStorage(key, nonce) {
		t.Fatal("the key and the nonce share storage, so one erase would read as two")
	}
	if !bytes.Equal(key, answered) || !bytes.Equal(nonce, answeredNonce) {
		t.Fatal("the pair MessageKey answered with is not the entry it was taken from, so this test is erasing something the caller was never given")
	}
	if ksSharesStorage(key, answered) || ksSharesStorage(nonce, answeredNonce) {
		t.Fatal("MessageKey answered with the window's own storage rather than a copy of it; see TestMessageKeyDoesNotHandTwoHoldersOneArrayOfKeyBytes for what that costs")
	}

	tree.EraseMessageKey(ContentTypeCommit, leaf, generation)

	for i, b := range key {
		if b != 0 {
			t.Fatalf("key byte %d = %d after erase, want 0", i, b)
		}
	}
	for i, b := range nonce {
		if b != 0 {
			t.Fatalf("nonce byte %d = %d after erase, want 0", i, b)
		}
	}
	// and the entry is unreachable as well as erased, so neither half stands in for the other.
	if _, still := r.window[generation]; still {
		t.Fatalf("generation %d is still in the window after the erase", generation)
	}
}

// TestEraseMessageKeyIsTotal asserts erasing something that was never derived, a leaf no tree
// holds, or a content type that does not exist is a no-op.
//
// The framing layer calls this on every open path including the failing ones, so it must never
// panic and must never build a ratchet -- building one takes the leaf secret out of the tree,
// and taking it is what destroys it, so an erase that created would destroy a leaf's whole
// epoch on behalf of a message that did not open.
func TestEraseMessageKeyIsTotal(t *testing.T) {
	tree := stNewTree(t, 8)
	tree.EraseMessageKey(ContentTypeApplication, 5, 7)
	tree.EraseMessageKey(ContentType(0), 5, 7)
	tree.EraseMessageKey(ContentTypeApplication, 1<<20, 0)
	tree.EraseMessageKey(ContentTypeCommit, 0, ^uint32(0))
	if len(tree.ratchets) != 0 {
		t.Fatalf("erase built %d ratchets; it must not touch the tree", len(tree.ratchets))
	}
	if held := secretTreeRetainedBytes(t, tree); len(held) == 0 {
		t.Fatal("the tree holds nothing after four erases that were all supposed to be no-ops")
	}
	// and it is a no-op for a leaf whose ratchet DOES exist, at a generation that was never
	// retained: the neighbouring generations survive.
	if _, _, err := tree.MessageKey(ContentTypeApplication, 5, 2); err != nil {
		t.Fatalf("MessageKey: %v", err)
	}
	tree.EraseMessageKey(ContentTypeApplication, 5, 9)
	if _, _, err := tree.MessageKey(ContentTypeApplication, 5, 2); err != nil {
		t.Errorf("erasing a generation that was never derived took one that was: %v", err)
	}
}

// TestAnErasedEpochOutranksAnUnknownContentType states the precedence between the two
// refusals these wrappers can make, which no other test here names.
//
// It is the order ratchetFor already keeps and it exists for ratchetFor's reason: an erased
// tree holds KDF.Nh zero bytes where every secret was, so what the caller has to hear is that
// the epoch is gone. "Your content type is wrong" sends it back to re-read a header that was
// fine, and a caller that trusted that answer would go on presenting the same message to the
// same dead epoch.
//
// The whole code point space is swept, so the precedence holds for the legal content types
// and the reserved ones alike rather than for the one value a zero argument happens to reach.
func TestAnErasedEpochOutranksAnUnknownContentType(t *testing.T) {
	tree := stNewTree(t, 8)
	if _, _, _, err := tree.NextSenderKey(0, RatchetApplication); err != nil {
		t.Fatalf("NextSenderKey: %v", err)
	}
	tree.Zeroize()
	swept := 0
	for point := range stContentTypeCodePoints() {
		contentType := ContentType(point)
		if _, _, _, err := tree.NextMessageKey(contentType, 1); !errors.Is(err, ErrEpochErased) {
			t.Errorf("NextMessageKey(%d) on an erased tree answered %v, want ErrEpochErased", contentType, err)
		}
		if _, _, err := tree.MessageKey(contentType, 1, 0); !errors.Is(err, ErrEpochErased) {
			t.Errorf("MessageKey(%d) on an erased tree answered %v, want ErrEpochErased", contentType, err)
		}
		swept++
	}
	if swept != stContentTypeCodePoints() {
		t.Fatalf("swept %d code points, want %d", swept, stContentTypeCodePoints())
	}
	// the control: on a LIVE tree the two refusals are the other way round, so the sweep above
	// is not satisfied by a build that answers ErrEpochErased to everything.
	live := stNewTree(t, 8)
	if _, _, _, err := live.NextMessageKey(ContentType(0), 1); !errors.Is(err, ErrUnknownContentType) {
		t.Errorf("NextMessageKey(0) on a live tree answered %v, want ErrUnknownContentType", err)
	}
	if _, _, _, err := live.NextMessageKey(ContentTypeApplication, 1); err != nil {
		t.Errorf("NextMessageKey on a live tree answered %v, want a key", err)
	}
}

// TestMessageKeyHoldsTheWholeTreesRetainedKeysToOneBound is the decrypt path's half of the
// bound SecretTree.pruneRetained exists for.
//
// receiverKey applies it and MessageKey has to as well, because the two reach the same
// retention from the same place: a leaf index and a generation number in a header nobody
// authenticated. Without it the retained key memory is RatchetWindowSize multiplied by the
// number of ratchets, and the number of ratchets is the other members' choice rather than this
// receiver's -- which is a number an attacker picks, not a bound.
//
// The sequence below asks a modest skip of every ratchet of an eight leaf tree. What it
// retains without a tree wide bound is written out and checked against the bound first, so a
// run in which the requests were too small to overflow reports that rather than passing.
func TestMessageKeyHoldsTheWholeTreesRetainedKeysToOneBound(t *testing.T) {
	tree := stNewTree(t, 8)
	leaves := stLeavesOf(t, 8)
	kinds := stRatchetKinds(t)
	const skip = uint32(200)

	withoutABound := len(leaves) * len(kinds) * int(skip+1)
	if withoutABound <= MaxRetainedWindowKeys {
		t.Fatalf("this sequence retains at most %d keys and the tree wide bound is %d, so it could not observe the bound",
			withoutABound, MaxRetainedWindowKeys)
	}
	// THE SEQUENCE IS ACCEPTED MESSAGES AND NOT LOOKUPS, which is where the retention moved to
	// when the receive path became two phase. A lookup retains nothing at all -- see
	// TestAPeekRetainsNothingAcrossTheWholeTree, which is the stronger statement about that door --
	// so the bound has to be observed where something is actually kept.
	for _, leaf := range leaves {
		for _, contentType := range []ContentType{ContentTypeApplication, ContentTypeCommit} {
			if _, _, err := tree.MessageKey(contentType, leaf, skip); err != nil {
				t.Fatalf("MessageKey(%d, %d, %d): %v", contentType, leaf, skip, err)
			}
			if err := tree.CommitMessageKey(contentType, leaf, skip); err != nil {
				t.Fatalf("CommitMessageKey(%d, %d, %d): %v", contentType, leaf, skip, err)
			}
		}
	}
	retained := 0
	for _, r := range tree.ratchets {
		retained += len(r.window)
	}
	// the bound is applied on the way IN, so what stands afterwards is at most the bound plus
	// the one ratchet the last call advanced. That is a constant: it does not grow with the
	// group, which is the whole property.
	if ceiling := MaxRetainedWindowKeys + RatchetWindowSize + 1; retained > ceiling {
		t.Fatalf("the tree retains %d generation keys after %d lookups, want at most %d; without the tree wide bound this sequence retains %d and the figure is linear in the group size",
			retained, len(leaves)*2, ceiling, withoutABound)
	}
	if retained == 0 {
		t.Fatal("the tree retains nothing at all, so this bound held against a lookup path that never retained anything")
	}
}

// ---------------------------------------------------------------------------
// task 24: sender data
// ---------------------------------------------------------------------------

// stSenderDataKatComparisons is two answers -- the key and the nonce -- for every entry of
// mlswg's secret-tree corpus at a suite this package registers.
//
// It is a constant rather than a count taken from the sweep, because a sweep that matched no
// entry compares nothing and reports the same clean run a complete one reports. Three entries
// per registered suite, two registered suites.
const stSenderDataKatComparisons = 2 * 3 * 2

// TestSenderDataKeyNonceMatchesTheMlswgSecretTreeCorpus is the known answer test, taken from
// the published corpus rather than from this implementation.
//
// It runs through SenderDataKeyNonce rather than through an ExpandWithLabel written out here.
// That distinction is the point of task 24 existing at all: the same section 6.3.2 derivation
// written twice, with only one of the two covered by a vector, is how a wrong ciphertext_sample
// ships -- a wrong sample is real ciphertext, so it derives a key of exactly the right width
// that opens nothing, and against a peer with the same mistake it even interoperates.
func TestSenderDataKeyNonceMatchesTheMlswgSecretTreeCorpus(t *testing.T) {
	vectors := []labelKatSecretTree{}
	loadLabelKat(t, "secret-tree.json", &vectors)
	if len(vectors) == 0 {
		t.Fatal("secret-tree.json decoded to no entries")
	}
	compared := 0
	for _, vector := range vectors {
		suite := CipherSuite(vector.CipherSuite)
		if !IsRegisteredSuite(suite) {
			continue
		}
		crypto := mustProvider(t, suite)
		at := fmt.Sprintf(" suite %#04x", uint16(suite))
		key, nonce, err := SenderDataKeyNonce(crypto,
			mustDecodeHex(t, "sender_data_secret", vector.SenderData.SenderDataSecret),
			mustDecodeHex(t, "ciphertext", vector.SenderData.Ciphertext))
		if err != nil {
			t.Errorf("SenderDataKeyNonce%s: %v", at, err)
			continue
		}
		assertLabelKat(t, "sender_data_key"+at, key, vector.SenderData.Key)
		assertLabelKat(t, "sender_data_nonce"+at, nonce, vector.SenderData.Nonce)
		compared += 2
	}
	if compared != stSenderDataKatComparisons {
		t.Fatalf("compared %d published sender data answers, want %d", compared, stSenderDataKatComparisons)
	}
}

// TestSenderDataKeyNonceSamplesExactlyTheFirstNhBytes reads the sample boundary off the
// derivation in both directions, one byte at a time and at three ciphertext lengths.
//
// Every byte below KDF.Nh has to change the answer and every byte at or above it has to leave
// it alone, which locates the sample's offset AND its length exactly rather than inferring
// them from three whole-answer comparisons.
//
// The three lengths are the half that a single length cannot do, and the reason is measured
// rather than supposed. A sample rule whose BOUND is wrong -- one that cuts at Nh only once
// the ciphertext is past 2*Nh, say -- behaves correctly at a long ciphertext and takes the
// whole of a short one, so a sweep at one long length reports it clean and so does the plan's
// version of this test, which compares a 77 byte ciphertext against its own first 32 bytes.
// Nh+1 is the shortest input the rule applies to at all, and it is where that family of
// defect is visible.
//
// What the plan's version does catch, measured against the mutants rather than assumed: a
// sample one byte short and a sample taken from offset one both fail it, because the
// truncated arm of that test is a ciphertext of exactly Nh bytes, which the rule leaves alone,
// while the full arm is cut. It is a real test. This one is stricter in where it puts the
// boundary and in how many ciphertext lengths it puts it at.
func TestSenderDataKeyNonceSamplesExactlyTheFirstNhBytes(t *testing.T) {
	crypto := stTestCrypto(t)
	nh := crypto.HashSize()
	secret := bytes.Repeat([]byte{0x5c}, nh)
	swept := 0
	for _, length := range []int{nh + 1, nh + nh/2, 3 * nh} {
		if length <= nh {
			t.Fatalf("a ciphertext of %d bytes is not past KDF.Nh (%d), so this row observes no boundary",
				length, nh)
		}
		ciphertext := make([]byte, length)
		for i := range ciphertext {
			ciphertext[i] = byte(i%251) + 1
		}
		baseKey, baseNonce, err := SenderDataKeyNonce(crypto, secret, ciphertext)
		if err != nil {
			t.Fatalf("SenderDataKeyNonce over %d bytes: %v", length, err)
		}
		inside, outside := 0, 0
		for i := range ciphertext {
			altered := bytes.Clone(ciphertext)
			altered[i] ^= 0xff
			key, nonce, err := SenderDataKeyNonce(crypto, secret, altered)
			if err != nil {
				t.Fatalf("SenderDataKeyNonce with byte %d of %d flipped: %v", i, length, err)
			}
			changed := !bytes.Equal(key, baseKey) || !bytes.Equal(nonce, baseNonce)
			if i < nh {
				if !changed {
					t.Errorf("ciphertext of %d bytes: flipping byte %d changed nothing, and the sample is ciphertext[0..%d]; the sample is shorter than KDF.Nh or starts past the front",
						length, i, nh-1)
				}
				inside++
				continue
			}
			if changed {
				t.Errorf("ciphertext of %d bytes: flipping byte %d changed the answer, and the sample ends at %d; the sample is longer than KDF.Nh, or the rule that cuts it does not fire at this length",
					length, i, nh-1)
			}
			outside++
		}
		if inside != nh || outside != length-nh {
			t.Fatalf("ciphertext of %d bytes: the sweep read %d bytes inside the sample and %d outside, want %d and %d",
				length, inside, outside, nh, length-nh)
		}
		swept += length
	}
	if swept == 0 {
		t.Fatal("no ciphertext length was swept, so this gate located no boundary")
	}
}

// TestSenderDataKeyNonceUsesAShortCiphertextWhole holds the other end of the sample rule: a
// ciphertext shorter than KDF.Nh enters the derivation as it is.
//
// Padding it to Nh is the plausible mistake and it is a real one -- two short ciphertexts that
// differ only in length would then sample identically, which is one key and nonce pair for two
// messages, the reuse the sample exists to prevent reintroduced at the short end. So the
// assertion is that a short ciphertext and the same bytes zero padded to Nh disagree, and that
// two short ciphertexts of different lengths disagree with each other.
func TestSenderDataKeyNonceUsesAShortCiphertextWhole(t *testing.T) {
	crypto := stTestCrypto(t)
	nh := crypto.HashSize()
	secret := bytes.Repeat([]byte{0x5d}, nh)
	short := bytes.Repeat([]byte{0x2a}, nh/2)

	shortKey, _, err := SenderDataKeyNonce(crypto, secret, short)
	if err != nil {
		t.Fatalf("SenderDataKeyNonce over a short ciphertext: %v", err)
	}
	padded := make([]byte, nh)
	copy(padded, short)
	paddedKey, _, err := SenderDataKeyNonce(crypto, secret, padded)
	if err != nil {
		t.Fatalf("SenderDataKeyNonce over the padded ciphertext: %v", err)
	}
	if bytes.Equal(shortKey, paddedKey) {
		t.Error("a short ciphertext derives the same key as itself zero padded to KDF.Nh, so it is being padded rather than used whole")
	}
	shorterKey, _, err := SenderDataKeyNonce(crypto, secret, short[:len(short)-1])
	if err != nil {
		t.Fatalf("SenderDataKeyNonce over a shorter ciphertext: %v", err)
	}
	if bytes.Equal(shortKey, shorterKey) {
		t.Error("two short ciphertexts of different lengths derive one key, which is one keystream over two messages")
	}
	// and the empty ciphertext is answered rather than refused or panicked on: it is a
	// degenerate input the framing layer can reach, and ExpandWithLabel takes an empty context.
	emptyKey, emptyNonce, err := SenderDataKeyNonce(crypto, secret, nil)
	if err != nil {
		t.Fatalf("SenderDataKeyNonce over an empty ciphertext: %v", err)
	}
	if len(emptyKey) != crypto.KeySize() || len(emptyNonce) != crypto.NonceSize() {
		t.Errorf("the empty ciphertext answered %d and %d bytes, want %d and %d",
			len(emptyKey), len(emptyNonce), crypto.KeySize(), crypto.NonceSize())
	}
}

// stRefusalOf calls a construction and answers its error, turning a PANIC into a reported
// failure rather than letting it end the run.
//
// It exists because the refusals these tests assert are the only thing standing between a
// wrong length and a provider that raises on one. Measured, not supposed: with
// SenderDataKeyNonce's length check deleted, the provider panics with "pseudorandom key of 32
// bytes is shorter than the 48 byte hash", and an uncaught panic takes the whole test BINARY
// down -- the run reports one aborted test and every test declared after it never runs, which
// is how a mutation hides the tests that would have caught it. Eight tests went unrun that
// way before this existed.
//
// A caught panic is reported here and answered as the refusal, so the caller's own assertion
// does not fire a second, less accurate message about the same defect.
func stRefusalOf(t *testing.T, what string, call func() error) (err error) {
	t.Helper()
	defer func() {
		if raised := recover(); raised != nil {
			t.Errorf("%s panicked with %v rather than refusing", what, raised)
			err = ErrSecretLength
		}
	}()
	return call()
}

// TestSenderDataKeyNonceRefusesEveryWrongSecretLength sweeps the lengths around KDF.Nh rather
// than probing one.
//
// A secret of the wrong length expands into a well formed key that no peer agrees with, which
// surfaces later as an undecryptable message rather than as the length mistake it is -- so the
// refusal is the whole of what a caller gets, and it has to fire on both sides of Nh.
func TestSenderDataKeyNonceRefusesEveryWrongSecretLength(t *testing.T) {
	crypto := stTestCrypto(t)
	nh := crypto.HashSize()
	for _, length := range []int{0, 1, nh / 2, nh - 1, nh + 1, 2 * nh} {
		if length == nh {
			t.Fatalf("length %d is KDF.Nh, so this row would require a refusal of the legal length", length)
		}
		err := stRefusalOf(t, fmt.Sprintf("a %d byte sender data secret", length), func() error {
			_, _, refusal := SenderDataKeyNonce(crypto, bytes.Repeat([]byte{0x5e}, length), []byte{1})
			return refusal
		})
		if !errors.Is(err, ErrSecretLength) {
			t.Errorf("a %d byte sender data secret answered %v, want ErrSecretLength", length, err)
		}
	}
	if _, _, err := SenderDataKeyNonce(crypto, bytes.Repeat([]byte{0x5e}, nh), []byte{1}); err != nil {
		t.Errorf("a KDF.Nh byte secret was refused: %v", err)
	}
}

// TestSenderDataKeyNonceAnswersTwoArraysThatDoNotOverlap is the aliasing half.
//
// A key and a nonce cut from one expansion, or returned as one slice twice, would satisfy every
// value assertion above -- the KAT compares each against its own published bytes and both would
// be right if the two expansions happened to be written into one array in the right order. What
// it would not be is safe: erasing the key would erase the nonce, and a caller that overwrote
// one would silently move the other.
func TestSenderDataKeyNonceAnswersTwoArraysThatDoNotOverlap(t *testing.T) {
	crypto := stTestCrypto(t)
	key, nonce, err := SenderDataKeyNonce(crypto, bytes.Repeat([]byte{0x5f}, crypto.HashSize()),
		bytes.Repeat([]byte{0x60}, 96))
	if err != nil {
		t.Fatalf("SenderDataKeyNonce: %v", err)
	}
	if len(key) == 0 || len(nonce) == 0 {
		t.Fatal("one of the two answers is empty, so the overlap check below observes nothing")
	}
	if ksSharesStorage(key, nonce) {
		t.Error("the sender data key and nonce share backing storage, so erasing one erases the other")
	}
	if bytes.HasPrefix(key, nonce) || bytes.HasPrefix(nonce, key) {
		t.Error("the sender data key and nonce are one expansion cut in two, so the pair carries the entropy of one")
	}
}

// TestSenderDataKeyNonceReadsBothLengthsOffTheProviderItWasHanded is the differential the
// registry cannot supply, for the two lengths this construction reads and the one it refuses
// against.
//
// Both registered suites fix Nn at 12 and one of them fixes Nk at 32, which is also KDF.Nh and
// also the literal a body would have written down, so inside this registry a written down 12
// and a read of NonceSize() are the same number -- and every assertion in this file, the
// published corpus included, is satisfied by either. The synthetic suite is what separates
// them, and it is ksWelcomeSyntheticParams rather than a second one declared here: that suite
// already carries the argument that its Nk and Nn coincide with nothing in the registry, and
// two synthetic suites for one property is two lists that can drift.
func TestSenderDataKeyNonceReadsBothLengthsOffTheProviderItWasHanded(t *testing.T) {
	crypto := &suiteCryptoProvider{params: &ksWelcomeSyntheticParams, random: constantReader{value: 0x41}}
	if crypto.KeySize() == crypto.NonceSize() {
		t.Fatalf("this suite's Nk and Nn are both %d, so the two assertions below are one", crypto.KeySize())
	}
	for _, other := range []struct {
		name  string
		value int
	}{
		{name: "this suite's KDF.Nh", value: ksWelcomeSyntheticParams.Nh},
		{name: "the chacha suite's Nk", value: 32},
		{name: "the aes suite's Nk", value: 16},
		{name: "the registry's Nn", value: 12},
	} {
		if other.value == crypto.KeySize() || other.value == crypto.NonceSize() {
			t.Errorf("%s is %d, which is this suite's Nk or Nn, so a body writing it down would pass here",
				other.name, other.value)
		}
	}

	secret := bytes.Repeat([]byte{0x62}, ksWelcomeSyntheticParams.Nh)
	ciphertext := bytes.Repeat([]byte{0x63}, 3*ksWelcomeSyntheticParams.Nh)
	key, nonce, err := SenderDataKeyNonce(crypto, secret, ciphertext)
	if err != nil {
		t.Fatalf("SenderDataKeyNonce over a suite whose KDF.Nh is %d: %v", ksWelcomeSyntheticParams.Nh, err)
	}
	if len(key) != ksWelcomeSyntheticParams.Nk {
		t.Errorf("sender_data_key is %d bytes for a suite whose Nk is %d", len(key), ksWelcomeSyntheticParams.Nk)
	}
	if len(nonce) != ksWelcomeSyntheticParams.Nn {
		t.Errorf("sender_data_nonce is %d bytes for a suite whose Nn is %d", len(nonce), ksWelcomeSyntheticParams.Nn)
	}

	// the third length: the secret is measured against the provider's KDF.Nh and not against 32
	shortSecret := stRefusalOf(t, "a secret at the registry's KDF.Nh", func() error {
		_, _, refusal := SenderDataKeyNonce(crypto, bytes.Repeat([]byte{0x62}, sha256.Size), ciphertext)
		return refusal
	})
	if err := shortSecret; !errors.Is(err, ErrSecretLength) {
		t.Errorf("a %d byte secret was accepted by a suite whose KDF.Nh is %d: err = %v",
			sha256.Size, ksWelcomeSyntheticParams.Nh, err)
	}
	// and the fourth, which is this construction's own: the SAMPLE is KDF.Nh bytes of the
	// ciphertext and not 32 of them. A body that wrote 32 down here answers a key of exactly
	// the right width off a sample sixteen bytes short of the one the RFC names.
	sample := ciphertext[:ksWelcomeSyntheticParams.Nh]
	if want := crypto.ExpandWithLabel(secret, "key", sample, ksWelcomeSyntheticParams.Nk); !bytes.Equal(key, want) {
		wrong := crypto.ExpandWithLabel(secret, "key", ciphertext[:sha256.Size], ksWelcomeSyntheticParams.Nk)
		if bytes.Equal(key, wrong) {
			t.Errorf("the sample is the first %d bytes of the ciphertext for a suite whose KDF.Nh is %d",
				sha256.Size, ksWelcomeSyntheticParams.Nh)
		} else {
			t.Errorf("sender_data_key is not ExpandWithLabel over the first %d bytes of the ciphertext",
				ksWelcomeSyntheticParams.Nh)
		}
	}
	// and the same defect one level down, which the length assertions cannot see: a body that
	// asked the kdf for a written down length and TRUNCATED to the provider's answers Nk and Nn
	// bytes either way. ExpandWithLabel binds the requested length into its own preimage, so
	// the truncation is a different value and this is what separates them.
	if truncated := crypto.ExpandWithLabel(secret, "key", sample, 32); len(key) <= len(truncated) &&
		bytes.Equal(key, truncated[:len(key)]) {
		t.Errorf("sender_data_key is the first %d bytes of a 32 byte expansion rather than an expansion of %d",
			len(key), ksWelcomeSyntheticParams.Nk)
	}
	if truncated := crypto.ExpandWithLabel(secret, "nonce", sample, 12); len(nonce) <= len(truncated) &&
		bytes.Equal(nonce, truncated[:len(nonce)]) {
		t.Errorf("sender_data_nonce is the first %d bytes of a 12 byte expansion rather than an expansion of %d",
			len(nonce), ksWelcomeSyntheticParams.Nn)
	}
}

// ---------------------------------------------------------------------------
// the storage a key source hands back, and who is allowed to write through it
// ---------------------------------------------------------------------------

// stKeySourceRow is one exported method of *SecretTree that answers key material, and a call
// that drives it far enough to answer.
type stKeySourceRow struct {
	name string
	call func(t *testing.T, tree *SecretTree) (key []byte, nonce []byte)
}

// stKeySourceRows is the table, one row per member of the class stMethodsAnsweringBytes
// derives.
//
// It is not what decides the class. A method answering bytes with no row here is a failure of
// the gate below rather than a member quietly left out, which is the difference between
// sweeping a class and sweeping a copy of it: this package has measured a hand written list
// understating the real one more times than it has measured anything else.
func stKeySourceRows() []stKeySourceRow {
	return []stKeySourceRow{
		{name: "MessageKey", call: func(t *testing.T, tree *SecretTree) ([]byte, []byte) {
			t.Helper()
			key, nonce, err := tree.MessageKey(ContentTypeApplication, 2, 3)
			if err != nil {
				t.Fatalf("MessageKey: %v", err)
			}
			return key, nonce
		}},
		{name: "NextMessageKey", call: func(t *testing.T, tree *SecretTree) ([]byte, []byte) {
			t.Helper()
			key, nonce, _, err := tree.NextMessageKey(ContentTypeCommit, 2)
			if err != nil {
				t.Fatalf("NextMessageKey: %v", err)
			}
			return key, nonce
		}},
		{name: "NextSenderKey", call: func(t *testing.T, tree *SecretTree) ([]byte, []byte) {
			t.Helper()
			_, key, nonce, err := tree.NextSenderKey(2, RatchetApplication)
			if err != nil {
				t.Fatalf("NextSenderKey: %v", err)
			}
			return key, nonce
		}},
		// receiverKey had a row here until 2026-09-17 and does not any more. MASTER section
		// 8.4.7 (3) ruled it unexported and this package's own stub gate then moved the
		// declaration into test source, so it is not an exported key source at all -- and the
		// class this table is held to is the EXPORTED ones. Its answer is still a caller's own
		// storage (commitFor hands out what it walked, and a retained entry is deleted as it is
		// handed over), and what says so is that nothing in this file writes through it; but
		// that is not a property of the exported surface and this sweep is about the exported
		// surface.
	}
}

// stMethodsAnsweringBytes is every exported method of *SecretTree whose results include a
// []byte, read off the compiled type rather than typed out.
//
// The reading is reflect's rather than the source's on purpose: what a caller can be handed is
// what the METHOD SET offers, so a key source added under a name nobody thought to write down
// here is a member of this class the moment it compiles. Absence is fatal, for the reason
// every other derivation in this package is: a filter that stopped matching leaves the gate
// reading it demanding nothing, and a gate that demands nothing reports exactly what a
// complete one reports.
func stMethodsAnsweringBytes(t *testing.T) []string {
	t.Helper()
	tree := reflect.TypeOf(&SecretTree{})
	byteSlice := reflect.TypeOf([]byte(nil))
	names := []string{}
	for i := 0; i < tree.NumMethod(); i++ {
		method := tree.Method(i)
		for result := 0; result < method.Type.NumOut(); result++ {
			if method.Type.Out(result) == byteSlice {
				names = append(names, method.Name)
				break
			}
		}
	}
	if len(names) == 0 {
		t.Fatal("no exported method of *SecretTree answers a []byte, so the gate reading this demands nothing")
	}
	slices.Sort(names)
	return names
}

// TestEveryExportedKeySourceHandsTheCallerStorageTheTreeNeverWritesThrough sweeps the methods
// that answer key material and holds every one of them to the same rule: what comes back is
// the caller's own storage, and no later call of this type writes through it.
//
// The rule is not a nicety. Three of this type's operations zeroize retained key material IN
// PLACE -- EraseMessageKey, the tree wide bound's eviction, and Zeroize -- so a method that
// answers with a slice the tree can still reach through its own maps has handed the caller
// bytes that a later, unrelated call silently turns into Nk zeros, with the nil error it was
// already given standing. And because every exported method here takes stateLock while the
// caller reads its answer without one, the same aliasing is an unsynchronised write to key
// bytes another goroutine is reading.
//
// Zeroize is the disturbance because it is the widest one the type has: it erases every
// ratchet secret and every retained window entry, so a method answering with storage the tree
// still reaches fails here whichever map that storage is reachable through, rather than only
// for the one path a purpose built probe happened to walk.
//
// Measured, not supposed: with MessageKey answering keys.key and keys.nonce straight out of
// the window entry it deliberately leaves behind, this gate fails on that row and passes on
// the other three -- which is the exact shape of the defect. receiverKey is safe because
// keyFor deletes the entry as it returns it, and the two sender paths are safe because step's
// keys never enter a window at all.
func TestEveryExportedKeySourceHandsTheCallerStorageTheTreeNeverWritesThrough(t *testing.T) {
	crypto := stTestCrypto(t)
	rows := stKeySourceRows()
	driven := []string{}
	for _, row := range rows {
		driven = append(driven, row.name)
	}
	slices.Sort(driven)
	if class := stMethodsAnsweringBytes(t); !slices.Equal(driven, class) {
		t.Fatalf("this gate drives %v and the exported methods of *SecretTree answering a []byte are %v; a key source with no row here is a member of the class that nothing sweeps",
			driven, class)
	}
	for _, row := range rows {
		tree := stNewTree(t, 8)
		key, nonce := row.call(t, tree)
		if len(key) != crypto.KeySize() || len(nonce) != crypto.NonceSize() {
			t.Fatalf("%s answered a %d byte key and a %d byte nonce, want %d and %d",
				row.name, len(key), len(nonce), crypto.KeySize(), crypto.NonceSize())
		}
		if stAllZero(key) || stAllZero(nonce) {
			t.Fatalf("%s answered zeros before anything had been erased, so the comparison below would hold for an implementation that answers nothing", row.name)
		}
		if ksSharesStorage(key, nonce) {
			t.Fatalf("%s answered a key and a nonce over one array, so one erase would reach both", row.name)
		}
		wasKey, wasNonce := bytes.Clone(key), bytes.Clone(nonce)

		tree.Zeroize()

		if !bytes.Equal(key, wasKey) {
			t.Errorf("%s answered with storage Zeroize wrote through: the caller was handed %x and now holds %x",
				row.name, wasKey, key)
		}
		if !bytes.Equal(nonce, wasNonce) {
			t.Errorf("%s answered with nonce storage Zeroize wrote through: the caller was handed %x and now holds %x",
				row.name, wasNonce, nonce)
		}
	}
}

// TestMessageKeyDoesNotHandTwoHoldersOneArrayOfKeyBytes is the repeatable lookup's own half of
// that rule, and it is the one MessageKey alone can fail.
//
// MessageKey is the only key source here that leaves the generation in the window, so it is
// the only one that can answer the same question twice -- and answering it out of the entry
// would give both holders one array. The pair is "look up, open, erase", so two holders is not
// hypothetical: it is a worker pool opening two copies of one message, and the erase either of
// them makes when it is done is then a write into the other's key.
func TestMessageKeyDoesNotHandTwoHoldersOneArrayOfKeyBytes(t *testing.T) {
	tree := stNewTree(t, 8)
	const leaf = LeafIndex(3)
	const generation = uint32(2)
	first, firstNonce, err := tree.MessageKey(ContentTypeApplication, leaf, generation)
	if err != nil {
		t.Fatalf("MessageKey: %v", err)
	}
	second, secondNonce, err := tree.MessageKey(ContentTypeApplication, leaf, generation)
	if err != nil {
		t.Fatalf("second MessageKey: %v", err)
	}
	if stAllZero(first) || stAllZero(firstNonce) {
		t.Fatal("the lookup answered zeros, so every comparison below holds against an implementation that answers nothing")
	}
	if !bytes.Equal(first, second) || !bytes.Equal(firstNonce, secondNonce) {
		t.Fatal("two lookups of one generation disagreed, so the two holders below are not holding one key")
	}
	if ksSharesStorage(first, second) {
		t.Fatal("two lookups of one generation were handed one array of key bytes, so either holder's erase writes into the other's key")
	}
	if ksSharesStorage(firstNonce, secondNonce) {
		t.Fatal("two lookups of one generation were handed one array of nonce bytes")
	}

	// the erase one holder makes when its message has opened, which is the call the pair is
	// built around, must not be a write into what the other holder was told is its key.
	wasKey, wasNonce := bytes.Clone(first), bytes.Clone(firstNonce)
	tree.EraseMessageKey(ContentTypeApplication, leaf, generation)

	if !bytes.Equal(first, wasKey) || !bytes.Equal(second, wasKey) {
		t.Errorf("one holder's erase rewrote the other's key: the holders were handed %x and now hold %x and %x",
			wasKey, first, second)
	}
	if !bytes.Equal(firstNonce, wasNonce) || !bytes.Equal(secondNonce, wasNonce) {
		t.Errorf("one holder's erase rewrote the other's nonce: the holders were handed %x and now hold %x and %x",
			wasNonce, firstNonce, secondNonce)
	}
}

// stFillPastTheRetainedBound takes a tree to exactly one retained key past
// MaxRetainedWindowKeys, with generation 0 of the leaf's APPLICATION ratchet as the oldest
// entry of the fullest window, and answers that ratchet.
//
// The state is built through the ratchets themselves rather than through more lookups, and
// that is the difference between the two tests below observing what they are named for and
// reporting a setup failure. A setup built out of the call under test cannot outlive a change
// to that call: measured, with MessageKey's tree wide bound moved from the way in to the way
// out, the tree never stands over the bound between calls at all, so a setup that filled it
// with lookups failed at its own guard and said nothing about the key material handed back.
//
// IT FILLS THROUGH commitFor AND NOT THROUGH peekFor, which since the receive path became two
// phase is the only thing that retains anything. A peek writes nothing, so a helper built on one
// would leave every window empty and the two cases below would fail at their own guards. The state
// it reaches is still a state a peer can reach: an ACCEPTED run of messages that skipped a gap is
// an ordinary out of order delivery.
func stFillPastTheRetainedBound(t *testing.T, tree *SecretTree, leaf LeafIndex) *ratchet {
	t.Helper()
	application, err := tree.ratchetFor(leaf, RatchetApplication)
	if err != nil {
		t.Fatalf("ratchetFor(application): %v", err)
	}
	// the target of a walk is never stored, so accepting MaxRetainedWindowKeys retains exactly
	// the run 0..MaxRetainedWindowKeys-1 behind it.
	if _, err := application.commitFor(uint32(MaxRetainedWindowKeys)); err != nil {
		t.Fatalf("commitFor(%d): %v", MaxRetainedWindowKeys, err)
	}
	if len(application.window) != MaxRetainedWindowKeys {
		t.Fatalf("this ratchet retains %d generations and the tests reading this need exactly %d",
			len(application.window), MaxRetainedWindowKeys)
	}
	if _, oldest := application.window[0]; !oldest {
		t.Fatal("generation 0 is not retained, so it is not the entry a tree wide eviction would take")
	}
	// one key retained on another ratchet puts the tree past the bound while leaving the
	// application ratchet the fullest, so the eviction takes from it rather than from the
	// window that grew last.
	handshake, err := tree.ratchetFor(leaf, RatchetHandshake)
	if err != nil {
		t.Fatalf("ratchetFor(handshake): %v", err)
	}
	if _, err := handshake.commitFor(1); err != nil {
		t.Fatalf("commitFor(handshake, 1): %v", err)
	}
	if retained := stTotalRetainedWindowKeys(tree); retained != MaxRetainedWindowKeys+1 {
		t.Fatalf("the tree retains %d generation keys and the tests reading this need exactly %d, one past the bound",
			retained, MaxRetainedWindowKeys+1)
	}
	return application
}

// TestARetainedBoundEvictionDoesNotWriteThroughAKeyAlreadyHandedBack is the third writer, and
// the one no caller of MessageKey can see coming.
//
// EraseMessageKey is at least the holder's own call. This one is not: a generation retained
// for a message that has not been opened yet is evicted -- and zeroized -- by a LATER ACCEPTED
// message at an unrelated generation, because the tree wide bound holds the whole tree's retained
// keys to MaxRetainedWindowKeys and takes from the fullest ratchet. A caller handed the entry's own
// storage therefore watches its key turn to zeros between the lookup and the open, with
// nothing having failed and nothing having been reported.
//
// THE EVICTION IS DRIVEN BY A COMMIT AND NOT BY A LOOKUP, which is where the tree wide bound moved
// to. A lookup retains nothing, so it has nothing to make room for and does not prune; what can
// still reach this caller's entry is the next message that is ACCEPTED, and that is the sequence
// below.
func TestARetainedBoundEvictionDoesNotWriteThroughAKeyAlreadyHandedBack(t *testing.T) {
	tree := stNewTree(t, 8)
	const leaf = LeafIndex(0)
	application := stFillPastTheRetainedBound(t, tree, leaf)

	held, heldNonce, err := tree.MessageKey(ContentTypeApplication, leaf, 0)
	if err != nil {
		t.Fatalf("MessageKey(0): %v", err)
	}
	if stAllZero(held) || stAllZero(heldNonce) {
		t.Fatal("the lookup answered zeros, so the comparison below holds against an implementation that answers nothing")
	}
	wasKey, wasNonce := bytes.Clone(held), bytes.Clone(heldNonce)

	// the next ACCEPTED message applies the bound, and what it takes is the oldest generation of
	// the fullest ratchet -- which is the generation still being held above.
	head, err := tree.SenderGeneration(leaf, RatchetApplication)
	if err != nil {
		t.Fatalf("SenderGeneration: %v", err)
	}
	if _, _, err := tree.MessageKey(ContentTypeApplication, leaf, head); err != nil {
		t.Fatalf("MessageKey(%d): %v", head, err)
	}
	if err := tree.CommitMessageKey(ContentTypeApplication, leaf, head); err != nil {
		t.Fatalf("CommitMessageKey(%d): %v", head, err)
	}
	if _, still := application.window[0]; still {
		t.Fatal("generation 0 survived an acceptance made with the tree past the bound, so this test observed no eviction at all")
	}

	if !bytes.Equal(held, wasKey) {
		t.Errorf("the tree wide eviction wrote through a key already handed back: the caller was given %x and now holds %x",
			wasKey, held)
	}
	if !bytes.Equal(heldNonce, wasNonce) {
		t.Errorf("the tree wide eviction wrote through a nonce already handed back: the caller was given %x and now holds %x",
			wasNonce, heldNonce)
	}
}

// TestMessageKeyNeverAnswersWithKeyMaterialTheRetainedBoundHasZeroized is what became of the
// ordering argument MessageKey used to carry, and it is now a stronger statement than the ordering.
//
// THE ORDERING PROBLEM WAS REAL AND IT IS GONE BY CONSTRUCTION. The tree wide bound evicts by
// zeroizing in place. While MessageKey applied that bound, WHERE it applied it decided everything:
// on the way in it could only reach keys that already existed, and on the way out the entry it
// took could be the very one the lookup had just found -- Nk zero bytes and a NIL ERROR, a key
// every party in the world can compute, presented as this sender's. Measured at the time: with the
// prune moved to the line before the return, all 658 tests of this package passed. MessageKey now
// applies no bound at all, because a peek retains nothing and so has nothing to make room for, and
// there is no ordering left to get wrong.
//
// SO WHAT IS ASSERTED IS THE TWO HALVES THAT REPLACE IT. A lookup made with the tree already past
// the bound answers the generation's true key -- compared against an independent tree over the same
// encryption secret, because "the answer is not zeros" is satisfied by some other generation's key
// -- and it leaves the tree exactly as far past the bound as it found it. The second half is the
// one that would go red on a build that started pruning here again.
//
// The state this needs is generation 0 sitting as the oldest entry of the fullest ratchet with
// the tree one key past the bound, so a prune on this path would pick exactly the generation being
// asked for. It is built by stFillPastTheRetainedBound, for the reason that helper gives.
func TestMessageKeyNeverAnswersWithKeyMaterialTheRetainedBoundHasZeroized(t *testing.T) {
	const leaf = LeafIndex(0)
	oracle := stNewTree(t, 8)
	trueKey, trueNonce, err := oracle.receiverKey(leaf, RatchetApplication, 0)
	if err != nil {
		t.Fatalf("the oracle's receiverKey: %v", err)
	}
	if stAllZero(trueKey) || stAllZero(trueNonce) {
		t.Fatal("the oracle answered zeros for generation 0, so every comparison below is against nothing")
	}

	tree := stNewTree(t, 8)
	application := stFillPastTheRetainedBound(t, tree, leaf)
	over := stTotalRetainedWindowKeys(tree)
	if over <= MaxRetainedWindowKeys {
		t.Fatalf("the tree retains %d keys and the bound is %d, so a prune on the lookup path would have nothing to do and this case would observe nothing",
			over, MaxRetainedWindowKeys)
	}

	// the lookup this case is written for. Generation 0 is the oldest entry of the fullest
	// ratchet, so a prune taken here would evict and zeroize precisely the entry being asked for.
	answered, answeredNonce, err := tree.MessageKey(ContentTypeApplication, leaf, 0)
	if err != nil {
		t.Fatalf("MessageKey(0) with the tree past the bound: %v", err)
	}
	if stAllZero(answered) || stAllZero(answeredNonce) {
		t.Fatalf("MessageKey answered key %x and nonce %x with a NIL ERROR; zeros are a key every party in the world can compute",
			answered, answeredNonce)
	}
	if !bytes.Equal(answered, trueKey) || !bytes.Equal(answeredNonce, trueNonce) {
		t.Errorf("MessageKey answered %x and %x for generation 0, and generation 0's key and nonce are %x and %x",
			answered, answeredNonce, trueKey, trueNonce)
	}
	if _, still := application.window[0]; !still {
		t.Fatal("generation 0 left the window over a LOOKUP; a peek may not evict another member's retained keys, whoever chose the leaf and the generation it was asked about")
	}
	if now := stTotalRetainedWindowKeys(tree); now != over {
		t.Fatalf("the tree retained %d keys before the lookup and %d after it: the lookup pruned",
			over, now)
	}
}

// ---------------------------------------------------------------------------
// a receiver that fell behind, and the two halves of the restore's own read
// ---------------------------------------------------------------------------

// TestAReceiverPastTheSkipBoundStaysWhereItIs is the COST of closing the deletion channel, written
// as a case rather than left in a comment, at the level the ratchet lives at.
//
// WHAT USED TO HAPPEN. A refusal past MaxGenerationSkip advanced the head by MaxGenerationSkip
// first, so a receiver behind a busy peer closed the gap by MaxGenerationSkip-1 per message it
// lost and was reading that peer again within a message or two. This case asserted exactly that,
// and it was named for the deafness it prevented.
//
// WHAT HAPPENS NOW, AND WHY THE TRADE GOES THIS WAY. That walk was driven by a generation number
// arriving in sender data sealed under a group shared secret, so every member could aim it at every
// other member: one header at head+MaxGenerationSkip made the victim's next thousand messages
// answer ErrRatchetGenerationConsumed at that receiver, forever, pre-emptively and repeatably. The
// resynchronisation and the deletion channel were the same walk. A receiver that has fallen further
// behind one sender than the bound is now deaf to that sender until the epoch changes, and that is
// a VISIBLE gap -- ErrRatchetGenerationTooFarAhead, which the product acts on -- rather than a
// silent loss.
//
// WHAT THE WINDOW STILL DOES IS NOT THIS. Out of order delivery and lost messages INSIDE the bound
// are unaffected; TestOutOfOrderAndLostMessagesStillOpenWithinTheWindow is that half, and the
// distance here is deliberately outside it.
//
// The deafness is measured over several later generations and not just one, because "the next
// message is refused too" is the whole of what the old catch-up existed to prevent, and a case that
// asked once could not tell a bound from an off-by-one.
func TestAReceiverPastTheSkipBoundStaysWhereItIs(t *testing.T) {
	const ahead = uint32(1026)
	if ahead <= MaxGenerationSkip {
		t.Fatalf("a peer %d generations ahead is inside the bound of %d, so this case observes an ordinary skip",
			ahead, MaxGenerationSkip)
	}
	receiver := stNewTree(t, 8)
	// THE SENDER IS NOT DRIVEN AT ALL, and the deletion of that fixture is part of what this case
	// now says. It used to walk a second tree to generation ahead+1 and keep that generation's key,
	// because the assertion was that the NEXT message opens and a key of the right length is not a
	// key. Nothing here opens any more, so the only sender-side value this case needs is the one it
	// takes from stSenderKeysThrough at the end, for the generation INSIDE the bound that still
	// does.
	if _, _, err := receiver.receiverKey(1, RatchetApplication, ahead); !errors.Is(err, ErrRatchetGenerationTooFarAhead) {
		t.Fatalf("a receiver at head 0 asked for generation %d answered %v, want ErrRatchetGenerationTooFarAhead",
			ahead, err)
	}
	// the refusal cost this receiver nothing, which is the whole reason the catch-up went.
	head, err := receiver.SenderGeneration(1, RatchetApplication)
	if err != nil {
		t.Fatalf("SenderGeneration: %v", err)
	}
	if head != 0 {
		t.Fatalf("a refused request left the head at %d, want 0", head)
	}
	// and the deafness is the disclosed cost: every later generation that peer reaches is refused
	// the same way until the epoch changes.
	for _, later := range []uint32{ahead + 1, ahead + 2, ahead + MaxGenerationSkip} {
		if _, _, err := receiver.receiverKey(1, RatchetApplication, later); !errors.Is(err, ErrRatchetGenerationTooFarAhead) {
			t.Fatalf("generation %d answered %v, want ErrRatchetGenerationTooFarAhead: this case states the cost, and a build that answers something else has changed it",
				later, err)
		}
	}
	// what it is NOT is a ratchet that refuses everything: a generation inside the bound still
	// opens, under the key the sender drew for it.
	inside := MaxGenerationSkip
	senderKeys, senderNonces := stSenderKeysThrough(t, 1, RatchetApplication, inside)
	gotKey, gotNonce, err := receiver.receiverKey(1, RatchetApplication, inside)
	if err != nil {
		t.Fatalf("a generation at the bound answered %v, want it served", err)
	}
	if !bytes.Equal(gotKey, senderKeys[inside]) || !bytes.Equal(gotNonce, senderNonces[inside]) {
		t.Fatalf("the receiver answered key %x nonce %x for generation %d, and the sender drew %x and %x",
			gotKey, gotNonce, inside, senderKeys[inside], senderNonces[inside])
	}
}

// TestRestoreSenderRatchetsRefusesTheSecretAnErasedEpochWouldHaveWritten is the read side of
// SenderRatchets' own refuseIfErased.
//
// THE TWO SIDES ARE ONE RULE AND THE READ HAD HALF OF IT. SenderRatchets refuses to WRITE a vector
// taken off an erased tree, because Zeroize overwrites every ratchet secret in place and leaves the
// ratchets in the table -- so the vector would be one run of Nh zeros per ratchet. The read checked
// the LENGTH of each secret and nothing else, so those same octets, arriving from a store rather
// than from this process, installed cleanly. Measured before the repair: the restore returned a nil
// error and the very next NextSenderKey answered a key expanded from a public constant.
//
// THE CONTROL IS THE SAME VECTOR WITH ITS REAL SECRETS, so a build whose restore refused everything
// would fail here rather than pass as a build that refuses the right thing.
func TestRestoreSenderRatchetsRefusesTheSecretAnErasedEpochWouldHaveWritten(t *testing.T) {
	live := stNewTree(t, 8)
	if _, _, _, err := live.NextSenderKey(1, RatchetApplication); err != nil {
		t.Fatalf("NextSenderKey: %v", err)
	}
	entries, err := live.SenderRatchets(1)
	if err != nil {
		t.Fatalf("SenderRatchets: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the live tree answered no sender ratchets for a leaf that has sent, so there is nothing here to restore")
	}

	// the control first: this vector restores.
	if err := stNewTree(t, 8).RestoreSenderRatchets(1, entries); err != nil {
		t.Fatalf("the live tree's own vector was refused: %v; this case would then observe a restore that refuses everything", err)
	}

	// and the vector an erased epoch would have produced, built by zeroing the secrets exactly as
	// Zeroize does and leaving everything else where it stood.
	erased := make([]senderRatchetEntry, 0, len(entries))
	for _, entry := range entries {
		erased = append(erased, senderRatchetEntry{
			Kind: entry.Kind, Consumed: entry.Consumed, Secret: make([]byte, len(entry.Secret))})
	}
	if err := stNewTree(t, 8).RestoreSenderRatchets(1, erased); !errors.Is(err, ErrEpochErased) {
		t.Fatalf("a vector of %d right-length all-zero ratchet secrets restored with %v, want ErrEpochErased: what comes out of installing it is a sender whose every generation any party in the world can derive",
			len(erased), err)
	}
	// and the write side refuses the same octets at their source, which is the sentence this case
	// pairs with rather than a second copy of it.
	live.Zeroize()
	if _, err := live.SenderRatchets(1); !errors.Is(err, ErrEpochErased) {
		t.Fatalf("SenderRatchets over an erased tree = %v, want ErrEpochErased", err)
	}
}

// TestRestoreSenderRatchetsRefusesAnUnknownKindBeforeItTakesTheLeafSecret is the paragraph
// "every entry is checked before any ratchet is built", held to what the code does.
//
// It was false. The validation loop read the secret's length, the consumed count and the ordering,
// and the field it did not read was the one ratchetFor refuses -- so a vector whose SECOND entry
// named a kind this build does not have walked the whole loop, and the refusal arrived after the
// first entry's ratchetFor had already taken the leaf node secret out of the tree and built both of
// that leaf's ratchets over it. takeLeafSecret is destructive and there is no way to put it back.
//
// SO WHAT IS ASSERTED IS THE TREE AFTERWARDS AND NOT THE ERROR. An assertion written as "the
// restore refused" is satisfied by both builds; only the leaf still being answerable tells them
// apart.
func TestRestoreSenderRatchetsRefusesAnUnknownKindBeforeItTakesTheLeafSecret(t *testing.T) {
	crypto := stTestCrypto(t)
	secret := make([]byte, crypto.HashSize())
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	// an unknown kind DERIVED as one this build does not have rather than typed as a number
	// somebody hopes stays unregistered.
	//
	// THE SWEEP IS BOUNDED BY THE WIDTH OF THE TYPE and reports its own failure, which this file
	// pays for elsewhere too: a loop written as "walk upward until one is refused" runs FOREVER
	// over a build whose door stopped refusing -- RatchetType wraps -- and a fixture that answers
	// a hang where it could answer a finding reports the same thing for a defect and for a broken
	// machine. Measured while writing this: with refuseUnknownRatchetType neutralised, the
	// unbounded version of this loop took the test binary out on the runner's timeout instead of
	// failing on the line below.
	unknown, held := RatchetType(0), false
	for code := 0; code < stRatchetTypeCodePoints(); code += 1 {
		if refuseUnknownRatchetType(RatchetType(code)) != nil {
			unknown, held = RatchetType(code), true
			break
		}
	}
	if !held {
		t.Fatalf("this build admits all %d ratchet type code points, so there is no unknown kind to offer and this case observes nothing",
			stRatchetTypeCodePoints())
	}
	entries := []senderRatchetEntry{
		{Kind: RatchetHandshake, Consumed: 3, Secret: bytes.Clone(secret)},
		{Kind: unknown, Consumed: 1, Secret: bytes.Clone(secret)},
	}
	tree := stNewTree(t, 8)
	if err := tree.RestoreSenderRatchets(1, entries); !errors.Is(err, ErrSecretTreeLeafOutOfRange) {
		t.Fatalf("a vector naming ratchet type %d restored with %v, want ErrSecretTreeLeafOutOfRange", unknown, err)
	}
	// the leaf is still answerable, which is the whole claim: nothing was built and the leaf node
	// secret is still in the tree.
	if _, err := tree.ratchetFor(1, RatchetApplication); err != nil {
		t.Fatalf("the leaf could not be reached after a refused restore: %v; the refusal ran after ratchetFor had consumed the leaf node secret, so this leaf can never answer again",
			err)
	}
	// and it stands where a leaf nothing restored stands, rather than where the accepted first
	// entry would have put it.
	head, err := tree.SenderGeneration(1, RatchetHandshake)
	if err != nil {
		t.Fatalf("SenderGeneration: %v", err)
	}
	if head != 0 {
		t.Fatalf("the handshake ratchet came back at generation %d after a REFUSED restore, so the first entry was installed before the second was judged",
			head)
	}
}

// stOrderingRounds is how many times the case below asks SenderRatchets for one unchanged leaf.
//
// The number is here rather than in the body because both halves of that case read it: the
// assertion runs on every round, and the vacuity guard is stated over the same count.
const stOrderingRounds = 512

// TestSenderRatchetsAnswersOneOrderWhateverTheMapHandsBack pins the ordering that decides whether a
// persisted state can be loaded back at all.
//
// WHY IT IS A CASE OF ITS OWN. RestoreSenderRatchets refuses a vector that is not in strictly
// increasing RatchetType order, and SenderRatchets is the only thing in this build that writes one.
// Its entries come out of a range over a map, so without the sort the order is whatever the
// runtime's iteration randomisation hands back on that call -- and for the two entries a leaf of
// this build holds, that is the descending order about one time in eight, MEASURED. A build that
// lost the sort would write an unloadable state roughly one persist in eight, and any case that
// called SenderRatchets once would report it no more often than that: it would be a test that
// passes seven runs in eight over a state its own package refuses to read.
//
// SO THIS ASKS MANY TIMES AND GUARDS ITS OWN VACUITY. Every answer must be ascending, and the raw
// map iteration must have been seen DESCENDING at least once across the same rounds -- otherwise
// every round happened to hand back the order the sort would have produced anyway, requiring
// ascending observed nothing, and the case says so instead of passing.
func TestSenderRatchetsAnswersOneOrderWhateverTheMapHandsBack(t *testing.T) {
	tree := stNewTree(t, 8)
	if _, _, _, err := tree.NextSenderKey(1, RatchetApplication); err != nil {
		t.Fatalf("NextSenderKey: %v", err)
	}
	entries, err := tree.SenderRatchets(1)
	if err != nil {
		t.Fatalf("SenderRatchets: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("this leaf holds %d ratchet(s), so no two entries can be out of order and this case observes nothing",
			len(entries))
	}
	sawDescending := 0
	for round := 0; round < stOrderingRounds; round += 1 {
		answered, err := tree.SenderRatchets(1)
		if err != nil {
			t.Fatalf("SenderRatchets at round %d: %v", round, err)
		}
		for i := 1; i < len(answered); i += 1 {
			if answered[i].Kind <= answered[i-1].Kind {
				t.Fatalf("round %d answered ratchet type %d after %d, which is the order RestoreSenderRatchets refuses: a state written from it cannot be loaded",
					round, answered[i].Kind, answered[i-1].Kind)
			}
		}
		// the raw order the sort was handed, read the same way SenderRatchets reads it
		raw := []RatchetType{}
		for key := range tree.ratchets {
			if key.leaf == 1 {
				raw = append(raw, key.kind)
			}
		}
		for i := 1; i < len(raw); i += 1 {
			if raw[i] < raw[i-1] {
				sawDescending += 1
				break
			}
		}
	}
	if sawDescending == 0 {
		t.Fatalf("over %d rounds the map never handed this leaf's ratchets back out of order, so every ascending answer above is one the sort did not have to produce and this case observed nothing",
			stOrderingRounds)
	}
	t.Logf("%d of %d rounds saw the map hand back the descending order, and every SenderRatchets answer was ascending",
		sawDescending, stOrderingRounds)
}

// ---------------------------------------------------------------------------
// what a peek costs, and what a receiver past the bound does
//
// THIS SECTION REPLACES THE CATCH-UP FAMILY, and the replacement is a deletion of behaviour rather
// than a rewrite of cases. (*ratchet).peekFor used to ratchet -- it advanced the head to the
// generation asked for, retained the run it passed, pruned, and on a distance over the bound ran a
// catch-up of MaxGenerationSkip and THEN refused. Four cases measured what that retained and what
// it cost a lagging receiver. Every one of those numbers was a number about a walk driven by a
// generation an unauthenticated party chose, and the walk is gone: the peek writes nothing at all
// and (*ratchet).commitFor carries the movement, reached only after a signature has verified.
//
// So what is measured here now is the property in the terms the defect has to be stated in -- no
// input an unauthenticated party controls may advance or prune another member's receiving ratchet
// -- plus the two things a reader will ask next: that an honest out of order or lost message still
// opens, and what a receiver that has fallen further behind than the bound does now that nothing
// catches it up.
// ---------------------------------------------------------------------------

// stSenderKeysThrough draws one leaf's own generations 0..through on a fresh tree and answers them,
// so a receiver's answer can be compared against a KEY rather than against the absence of an error.
//
// A receiver that returns the right LENGTH with a nil error passes every case that only checks the
// error, and a ratchet whose head moved without its secret moving with it is exactly that shape.
func stSenderKeysThrough(t *testing.T, leaf LeafIndex, kind RatchetType, through uint32) (keys map[uint32][]byte, nonces map[uint32][]byte) {
	t.Helper()
	sender := stNewTree(t, 8)
	keys, nonces = map[uint32][]byte{}, map[uint32][]byte{}
	for {
		generation, key, nonce, err := sender.NextSenderKey(leaf, kind)
		if err != nil {
			t.Fatalf("NextSenderKey at generation %d: %v", generation, err)
		}
		keys[generation] = bytes.Clone(key)
		nonces[generation] = bytes.Clone(nonce)
		if generation == through {
			return keys, nonces
		}
	}
}

// stRatchetState is everything about one ratchet a peek is forbidden to change, read out in a form
// two readings can be compared by.
//
// The window is captured by VALUE and not by length. An eviction that replaced one retained
// generation with another leaves the length where it was, and a prune that zeroized an entry in
// place leaves both the length and the key set where they were -- so a case that counted entries
// would pass over the two failures this exists to see.
type stRatchetState struct {
	head      uint32
	exhausted bool
	window    map[uint32][]byte
	retained  int
}

func stReadRatchetState(t *testing.T, tree *SecretTree, leaf LeafIndex, kind RatchetType) stRatchetState {
	t.Helper()
	r, err := tree.ratchetFor(leaf, kind)
	if err != nil {
		t.Fatalf("ratchetFor(%d, %d): %v", leaf, kind, err)
	}
	state := stRatchetState{head: r.head, exhausted: r.exhausted, window: map[uint32][]byte{}}
	for generation, keys := range r.window {
		state.window[generation] = append(bytes.Clone(keys.key), keys.nonce...)
	}
	for _, other := range tree.ratchets {
		state.retained += len(other.window)
	}
	return state
}

func stSameRatchetState(before stRatchetState, after stRatchetState) string {
	if before.head != after.head {
		return fmt.Sprintf("the head moved from %d to %d", before.head, after.head)
	}
	if before.exhausted != after.exhausted {
		return fmt.Sprintf("the exhausted flag moved from %v to %v", before.exhausted, after.exhausted)
	}
	if before.retained != after.retained {
		return fmt.Sprintf("the tree wide retained count moved from %d to %d", before.retained, after.retained)
	}
	if len(before.window) != len(after.window) {
		return fmt.Sprintf("the window holds %d entries and held %d", len(after.window), len(before.window))
	}
	for generation, held := range before.window {
		now, still := after.window[generation]
		if !still {
			return fmt.Sprintf("generation %d left the window", generation)
		}
		if !bytes.Equal(held, now) {
			return fmt.Sprintf("generation %d's retained key material changed", generation)
		}
	}
	return ""
}

// TestAPeekMovesNothingAndRetainsNothing is the CRITICAL property this section exists for, read at
// the exported door a forged header actually arrives through.
//
// THE THREE INPUTS A FORGED HEADER CARRIES ARE ALL HERE. MessageKey takes a content type, a LEAF
// and a GENERATION, and every one of them comes out of sender data sealed under the epoch's
// sender_data_secret, which every member of the group holds. So this case asks for the generations
// a member would choose if it wanted another member's ratchet moved -- the head itself, the head
// plus the whole bound, and a generation already retained for an out of order message that has not
// arrived -- and requires the ratchet to be in the same state afterwards, by value.
//
// IT IS SEEDED WITH A REAL RETAINED WINDOW FIRST, because "changes nothing" is trivially true of a
// ratchet with nothing to change. The seeding commit leaves generations below the head retained,
// which are exactly the entries the eviction half of the old defect destroyed: the walk's retained
// run pushed the tree past MaxRetainedWindowKeys and the next call's pruneRetained zeroized the
// oldest of the fullest ratchet to make room.
//
// AND THE ANSWERS ARE COMPARED AGAINST THE SENDER'S OWN KEYS, so a build that answered the right
// shape out of a ratchet it had quietly moved fails here rather than passing on a nil error.
func TestAPeekMovesNothingAndRetainsNothing(t *testing.T) {
	const leaf = LeafIndex(1)
	const kind = RatchetApplication
	const seeded = uint32(6)
	far := seeded + 1 + MaxGenerationSkip

	senderKeys, senderNonces := stSenderKeysThrough(t, leaf, kind, far)
	receiver := stNewTree(t, 8)

	// a real acceptance first: it leaves generations 0..seeded-1 retained and the head at
	// seeded+1, which is a ratchet with something to lose.
	if _, _, err := receiver.receiverKey(leaf, kind, seeded); err != nil {
		t.Fatalf("the seeding acceptance at generation %d: %v", seeded, err)
	}
	before := stReadRatchetState(t, receiver, leaf, kind)
	if len(before.window) == 0 {
		t.Fatalf("the seeding acceptance retained nothing, so there is no window for a peek to destroy and this case observes only the head")
	}

	for _, generation := range []uint32{before.head, before.head + 1, before.head + MaxGenerationSkip, 0, seeded - 1} {
		key, nonce, err := receiver.MessageKey(ContentTypeApplication, leaf, generation)
		if err != nil {
			t.Fatalf("MessageKey at generation %d: %v", generation, err)
		}
		if !bytes.Equal(key, senderKeys[generation]) || !bytes.Equal(nonce, senderNonces[generation]) {
			t.Fatalf("the peek at generation %d answered key %x nonce %x and the sender drew %x %x",
				generation, key, nonce, senderKeys[generation], senderNonces[generation])
		}
		if moved := stSameRatchetState(before, stReadRatchetState(t, receiver, leaf, kind)); moved != "" {
			t.Fatalf("a peek at generation %d %s; no input an unauthenticated party controls may advance or prune another member's receiving ratchet",
				generation, moved)
		}
	}

	// and the peek is REPEATABLE, which is the half that makes the property hold against a member
	// that sends the same forged header a thousand times. Twice at the far generation, still
	// nothing moved, and the second answer is the first.
	for round := 0; round < 3; round += 1 {
		key, _, err := receiver.MessageKey(ContentTypeApplication, leaf, before.head+MaxGenerationSkip)
		if err != nil {
			t.Fatalf("round %d at the bound: %v", round, err)
		}
		if !bytes.Equal(key, senderKeys[before.head+MaxGenerationSkip]) {
			t.Fatalf("round %d at the bound answered a different key from the sender's", round)
		}
		if moved := stSameRatchetState(before, stReadRatchetState(t, receiver, leaf, kind)); moved != "" {
			t.Fatalf("round %d of the same peek %s", round, moved)
		}
	}
}

// TestOneMemberCannotDestroyAWindowOfAnotherMembersMessages is the same property read from the
// victim's end, which is the end that decides whether it is worth anything.
//
// WHAT THE ATTACK WAS, so a reader can see what this case would look like on the build it closes.
// A member puts its OWN leaf aside, names the VICTIM's leaf in a header's sender data at
// head+MaxGenerationSkip, and sends it. mls reaches MessageKey before the content AEAD and before
// the signature, so the victim's receiving ratchet was asked for that generation and stepped to it;
// the AEAD then failed and the record was refused. Nothing was erased -- and the victim's next
// 1,025 messages answered ErrRatchetGenerationConsumed at that receiver forever, because every
// generation below a head is consumed. Two headers destroyed the whole retained window on top of
// that. It was pre-emptive, it needed nothing of the victim's, and it was repeatable.
//
// SO THE CASE DRIVES EXACTLY THAT and then asks for the victim's own messages. The peeks are not
// followed by a commit, because the forged record never authenticates -- that is the whole shape of
// the attack, and a case that committed would be measuring an accepted message.
func TestOneMemberCannotDestroyAWindowOfAnotherMembersMessages(t *testing.T) {
	const victim = LeafIndex(1)
	const kind = RatchetApplication
	const victimSends = uint32(8)

	senderKeys, senderNonces := stSenderKeysThrough(t, victim, kind, MaxGenerationSkip)
	receiver := stNewTree(t, 8)

	// the forged headers. Each one names the victim's leaf at the furthest generation the bound
	// admits, which is the value that bought the most on the build this closes.
	for header := 0; header < 4; header += 1 {
		if _, _, err := receiver.MessageKey(ContentTypeApplication, victim, MaxGenerationSkip); err != nil {
			t.Fatalf("forged header %d: the peek itself refused with %v, so this case is not driving the attack it names",
				header, err)
		}
	}
	head, err := receiver.SenderGeneration(victim, kind)
	if err != nil {
		t.Fatalf("SenderGeneration: %v", err)
	}
	if head != 0 {
		t.Fatalf("four forged headers left the victim's receiving head at %d, want 0: one header at head+%d used to cost the victim its next message and two used to cost it %d of them",
			head, MaxGenerationSkip, MaxGenerationSkip+1)
	}

	// and now the victim speaks, from generation 0, in order, exactly as it would have.
	for generation := uint32(0); generation < victimSends; generation += 1 {
		key, nonce, err := receiver.MessageKey(ContentTypeApplication, victim, generation)
		if err != nil {
			t.Fatalf("the victim's own message at generation %d is unopenable after the forged headers: %v",
				generation, err)
		}
		if !bytes.Equal(key, senderKeys[generation]) || !bytes.Equal(nonce, senderNonces[generation]) {
			t.Fatalf("the victim's generation %d opened under the wrong key", generation)
		}
		if err := receiver.CommitMessageKey(ContentTypeApplication, victim, generation); err != nil {
			t.Fatalf("committing the victim's generation %d: %v", generation, err)
		}
	}

	// and the commits DID move it, so the case above is not satisfied by a build in which nothing
	// moves anything ever -- which would have no replay guard and no forward secrecy at all.
	head, err = receiver.SenderGeneration(victim, kind)
	if err != nil {
		t.Fatalf("SenderGeneration: %v", err)
	}
	if head != victimSends {
		t.Fatalf("after %d accepted messages the head is at %d, want %d", victimSends, head, victimSends)
	}
	if _, _, err := receiver.MessageKey(ContentTypeApplication, victim, victimSends-1); !errors.Is(err, ErrRatchetGenerationConsumed) {
		t.Fatalf("a replay of an accepted generation answered %v, want ErrRatchetGenerationConsumed", err)
	}
}

// TestAPeekRetainsNothingAcrossTheWholeTree is the tree wide half, and it is about MEMORY rather
// than about another member's messages.
//
// pruneRetained exists because a peer that picks leaf indices and generation numbers out of the air
// used to materialise every leaf's two ratchets AND fill every one of their windows from headers
// that never had to be authentic -- 131072 retained generation keys and 17 MB on a 64 leaf tree,
// measured, and linear in the group size. MessageKey no longer applies that bound, and this is why
// that is not a removed guard: a peek retains nothing, so the sum it bounds cannot grow through
// this door at all. The residual is one ratchet per (leaf, kind), which is bounded by the tree.
func TestAPeekRetainsNothingAcrossTheWholeTree(t *testing.T) {
	tree := stNewTree(t, 8)
	for leaf := LeafIndex(0); leaf < 8; leaf += 1 {
		for _, contentType := range []ContentType{ContentTypeApplication, ContentTypeCommit} {
			for _, generation := range []uint32{0, 1, MaxGenerationSkip} {
				if _, _, err := tree.MessageKey(contentType, leaf, generation); err != nil {
					t.Fatalf("leaf %d content type %d generation %d: %v", leaf, contentType, generation, err)
				}
			}
		}
	}
	retained := 0
	for _, r := range tree.ratchets {
		retained += len(r.window)
	}
	if retained != 0 {
		t.Fatalf("%d peeks over %d ratchets left %d generation keys retained, want 0",
			8*2*3, len(tree.ratchets), retained)
	}
}

// TestARatchetPastTheSkipBoundRefusesAndMovesNothing is the arm the catch-up used to own.
//
// WHAT THE CATCH-UP DID AND WHY IT IS GONE. A distance over MaxGenerationSkip used to advance the
// head by MaxGenerationSkip and then refuse, so a receiver restored behind a busy peer closed the
// gap by MaxGenerationSkip-1 per message it lost instead of going deaf to that peer for the epoch.
// That is a useful behaviour and it was driven by a generation number NOBODY HAD AUTHENTICATED,
// which is the same sentence as the attack the case above drives. Its own disclosure argued it
// granted nothing new because the accepted path advanced the head too; the accepted path no longer
// does, so it became the only grant left and its premise went with it.
//
// WHAT IT COSTS IS NOT HIDDEN: the receiver stays where it is, and
// TestARestoredMemberIsDeafToAPeerThatMovedPastTheSkipBound drives that to its end at the group
// door. What it BUYS is the line below.
func TestARatchetPastTheSkipBoundRefusesAndMovesNothing(t *testing.T) {
	const leaf = LeafIndex(1)
	const kind = RatchetApplication
	tree := stNewTree(t, 8)
	before := stReadRatchetState(t, tree, leaf, kind)

	for _, generation := range []uint32{MaxGenerationSkip + 1, MaxGenerationSkip * 3, ^uint32(0)} {
		if _, _, err := tree.MessageKey(ContentTypeApplication, leaf, generation); !errors.Is(err, ErrRatchetGenerationTooFarAhead) {
			t.Fatalf("generation %d answered %v, want ErrRatchetGenerationTooFarAhead", generation, err)
		}
		if moved := stSameRatchetState(before, stReadRatchetState(t, tree, leaf, kind)); moved != "" {
			t.Fatalf("the refusal at generation %d %s: a refusal that walks is a walk a peer buys with a number nobody signed",
				generation, moved)
		}
	}
	// the exported consuming door answers the same way and moves nothing either, which is what
	// stops the bound from being reachable through the other half of this surface.
	if _, _, err := tree.receiverKey(leaf, kind, MaxGenerationSkip+1); !errors.Is(err, ErrRatchetGenerationTooFarAhead) {
		t.Fatalf("receiverKey past the bound answered %v, want ErrRatchetGenerationTooFarAhead", err)
	}
	if moved := stSameRatchetState(before, stReadRatchetState(t, tree, leaf, kind)); moved != "" {
		t.Fatalf("a refused receiverKey %s", moved)
	}
	// and the generation AT the bound is served, so the refusals above are the bound and not a
	// ratchet that refuses everything.
	if _, _, err := tree.MessageKey(ContentTypeApplication, leaf, MaxGenerationSkip); err != nil {
		t.Fatalf("the generation at the bound answered %v, want it served", err)
	}
}

// TestOutOfOrderAndLostMessagesStillOpenWithinTheWindow is what the window is FOR, and it is here
// because a repair that closed the channel by refusing to retain anything would pass every case
// above and would have traded one defect for another.
//
// THREE DELIVERIES, all of them ordinary. A message that arrives ahead of its predecessors and the
// predecessors afterwards, in a scrambled order; a message that never arrives at all, with the ones
// around it unaffected; and a replay of something already accepted, refused.
func TestOutOfOrderAndLostMessagesStillOpenWithinTheWindow(t *testing.T) {
	const leaf = LeafIndex(2)
	const kind = RatchetApplication
	const ahead = uint32(9)
	senderKeys, senderNonces := stSenderKeysThrough(t, leaf, kind, ahead)
	receiver := stNewTree(t, 8)

	open := func(generation uint32) error {
		key, nonce, err := receiver.MessageKey(ContentTypeApplication, leaf, generation)
		if err != nil {
			return err
		}
		if !bytes.Equal(key, senderKeys[generation]) || !bytes.Equal(nonce, senderNonces[generation]) {
			t.Fatalf("generation %d opened under a key the sender never drew", generation)
		}
		return receiver.CommitMessageKey(ContentTypeApplication, leaf, generation)
	}

	// the newest arrives first and the run behind it is retained.
	if err := open(ahead); err != nil {
		t.Fatalf("the message that arrived first, at generation %d: %v", ahead, err)
	}
	for _, generation := range []uint32{4, 0, 7, 1, 8, 2, 6, 3} {
		if err := open(generation); err != nil {
			t.Fatalf("the out of order message at generation %d: %v; the retained window is what makes an out of order delivery openable",
				generation, err)
		}
	}
	// generation 5 was never delivered, and nothing above depended on it.
	if err := open(5); err != nil {
		t.Fatalf("the late message at generation %d: %v", 5, err)
	}
	// and every one of them is single use.
	for _, generation := range []uint32{0, 5, ahead} {
		if _, _, err := receiver.MessageKey(ContentTypeApplication, leaf, generation); !errors.Is(err, ErrRatchetGenerationConsumed) {
			t.Fatalf("a replay of generation %d answered %v, want ErrRatchetGenerationConsumed", generation, err)
		}
	}
}

// TestOneRatchetsWindowNeverExceedsItsOwnBound is what is left of peekFor's old disclosure about a
// window that peaked at RatchetWindowSize+1.
//
// THAT PEAK IS GONE AND THE DELETION IS THE POINT. It existed because the old peek stored the
// generation it was about to RETURN and deliberately did not prune against it -- a prune after the
// store could evict the target, and eviction zeroizes, so the caller would have been handed Nk zero
// octets with a nil error. commitFor never stores the target at all: a retained one is deleted as
// it is handed over and a walked one is returned without ever entering the window. So the bound is
// the bound, at every moment and not only at the ones an exported caller can see, and this case
// reads it from inside where the old peak used to be.
func TestOneRatchetsWindowNeverExceedsItsOwnBound(t *testing.T) {
	const leaf = LeafIndex(1)
	const kind = RatchetApplication
	tree := stNewTree(t, 8)
	if _, _, err := tree.receiverKey(leaf, kind, MaxGenerationSkip); err != nil {
		t.Fatalf("receiverKey(%d): %v", MaxGenerationSkip, err)
	}
	r, err := tree.ratchetFor(leaf, kind)
	if err != nil {
		t.Fatalf("ratchetFor: %v", err)
	}
	if len(r.window) != RatchetWindowSize {
		t.Fatalf("a maximal in-bound acceptance left %d entries retained and this case needs the window exactly full at %d, or nothing below can take it over",
			len(r.window), RatchetWindowSize)
	}
	head := r.head
	keys, err := r.peekFor(head)
	if err != nil {
		t.Fatalf("peekFor(%d): %v", head, err)
	}
	if len(r.window) != RatchetWindowSize {
		t.Fatalf("a peek onto a full window left %d entries, want %d: the peek stored something",
			len(r.window), RatchetWindowSize)
	}
	if stAllZero(keys.key) || stAllZero(keys.nonce) {
		t.Fatal("the key the peek answered is zero, which is what an eviction of an entry the answer aliased would leave")
	}
	if _, err := r.commitFor(head); err != nil {
		t.Fatalf("commitFor(%d): %v", head, err)
	}
	if len(r.window) > RatchetWindowSize {
		t.Fatalf("the window holds %d entries after the commit, want at most %d", len(r.window), RatchetWindowSize)
	}
}

// TestTheSenderRatchetVectorIsSortedIntoTheOrderTheRestoreDemands holds the ordering that decides
// whether a persisted state can be loaded back, on a DESCENDING input, on every run.
//
// WHY IT IS BESIDE TestSenderRatchetsAnswersOneOrderWhateverTheMapHandsBack rather than instead of
// it. That case drives the door that ships, and it can be handed a descending input only on the
// rounds go's map randomisation supplies one -- measured at 600 rounds in 5000, so it asks hundreds
// of times, guards its own vacuity, and what it finally reports is a probability. This case hands
// the sort the descending vector directly, so the property is observed on run one and on every run
// after it. Neither replaces the other: that one says the door sorts, this one says the sort sorts.
//
// AND IT IS TIED TO THE READ AT BOTH ENDS, which is what makes the ordering load-bearing rather
// than cosmetic: the same vector is refused by RestoreSenderRatchets before the sort and accepted
// after it.
func TestTheSenderRatchetVectorIsSortedIntoTheOrderTheRestoreDemands(t *testing.T) {
	live := stNewTree(t, 8)
	if _, _, _, err := live.NextSenderKey(1, RatchetApplication); err != nil {
		t.Fatalf("NextSenderKey: %v", err)
	}
	entries, err := live.SenderRatchets(1)
	if err != nil {
		t.Fatalf("SenderRatchets: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("this leaf holds %d ratchet(s), so no two entries can be out of order and this case observes nothing",
			len(entries))
	}

	// the descending vector, DERIVED by reversing the one this build actually writes rather than
	// assembled out of kinds typed here: what has to be sorted is whatever SenderRatchets can
	// produce, and a hand-built vector is a second opinion about that.
	descending := make([]senderRatchetEntry, 0, len(entries))
	for i := len(entries) - 1; 0 <= i; i -= 1 {
		descending = append(descending, entries[i])
	}
	if !stHoldsAKindOutOfOrder(descending) {
		t.Fatal("reversing the live vector did not put it out of order, so the sort below is handed the order it would have produced anyway")
	}

	// the read refuses it as it stands. This is the state that cannot be loaded back, and it is
	// what a build with no sort would persist every time the map iterated this way.
	if err := stNewTree(t, 8).RestoreSenderRatchets(1, descending); !errors.Is(err, errGroupStateSenderRatchetOrder) {
		t.Fatalf("the descending vector restored with %v, want errGroupStateSenderRatchetOrder: without that refusal the ordering is not a property and this case measures nothing",
			err)
	}
	sortSenderRatchetEntries(descending)
	if stHoldsAKindOutOfOrder(descending) {
		t.Fatalf("the sort left the vector out of order: %v", stKindsOf(descending))
	}
	if err := stNewTree(t, 8).RestoreSenderRatchets(1, descending); err != nil {
		t.Fatalf("the sorted vector was refused: %v", err)
	}

	// AND IT IS A SORT RATHER THAN A SWAP OF THE ENDS, which the vector above cannot tell apart: a
	// leaf of this build holds two ratchets, so reversing its vector exercises exactly one
	// comparison. The Kind values below are sort INPUTS and not ratchet types this build has --
	// nothing installs this vector, it is only ordered -- and the run is longer than anything
	// SenderRatchets can write precisely so that swapping the ends fails it.
	longer := []senderRatchetEntry{}
	for kind := 5; 0 < kind; kind -= 1 {
		longer = append(longer, senderRatchetEntry{Kind: RatchetType(kind)})
	}
	sortSenderRatchetEntries(longer)
	if stHoldsAKindOutOfOrder(longer) {
		t.Fatalf("a descending run of %d came back as %v: the ordering is a swap of the ends and not a sort",
			len(longer), stKindsOf(longer))
	}
}

// stHoldsAKindOutOfOrder reports whether any entry names a kind at or below the one before it,
// which is the shape RestoreSenderRatchets refuses.
func stHoldsAKindOutOfOrder(entries []senderRatchetEntry) bool {
	for i := 1; i < len(entries); i += 1 {
		if entries[i].Kind <= entries[i-1].Kind {
			return true
		}
	}
	return false
}

// stKindsOf is the kinds of a vector, for a failure message that shows the order it is complaining
// about rather than one position out of it.
func stKindsOf(entries []senderRatchetEntry) []RatchetType {
	kinds := []RatchetType{}
	for _, entry := range entries {
		kinds = append(kinds, entry.Kind)
	}
	return kinds
}

// ---------------------------------------------------------------------------
// receiverKey: a door that was exported, was ruled unexported, and is therefore
// declared HERE, beside the only callers it will ever have
// ---------------------------------------------------------------------------

// WHY IT LIVES IN TEST SOURCE, which is this package's own rule and not a convenience.
//
// MASTER section 8.4.7 (3) ruled ReceiverKey UNEXPORTED on 2026-09-17, ledger item 216: it commits
// a ratchet with no authentication of any kind, and a door whose safety rests on "there is no
// caller to have learned it from" is held by an ABSENCE rather than by a rule. Unexported, it kept
// that property and gained a new problem -- it became an unexported production declaration with no
// production caller, which is exactly the shape TestNoStubShapesRemainInSource refuses, and which
// that gate's excuse table cannot hold: an excuse there expires when a production caller arrives,
// and this declaration must never have one.
//
// That gate states the remedy in as many words: "if the declaration will never have a production
// caller it belongs in test source, beside the tests that are its only callers, the way
// framing_protect_test.go holds the section 6.3.1 count form." So it is here. The ruling asked for
// unexported and this is STRICTLY STRONGER: no package outside mls could reach it either way, and
// this way it is not in the shipped binary at all.
//
// WHAT THE MOVE COSTS, printed rather than left to be noticed. The secret-tree vector family's key
// path class is the exported doors of *SecretTree that answer key material, so this door leaves it:
// the family compares 12 answers per generation where it compared 16, over three paths where it had
// four. The receive side derivation is still compared against mlswg's published vectors through
// MessageKey, which is the same ratchet walk's peek half; CommitMessageKey is the commit half and
// answers no key material to compare. secret_tree_kat_test.go's own constants carry the numbers.
//
// It also leaves the lock discipline gate's scope and the caller storage sweep, both of which read
// production source. What holds it now is this file's own 41 call sites.
//
// receiverKey returns one generation's key and nonce for another member's leaf.
//
// IT IS UNEXPORTED, RULED 2026-09-17, MASTER section 8.4.7 (3), ledger item 216. It was
// ReceiverKey until then, and the reason the rename is a ruling rather than tidying is in the
// paragraph below: this door commits a ratchet with no authentication of any kind, and its safety
// rested entirely on "there is no caller to have learned it from" -- which is an ABSENCE, and an
// absence is not a rule. The next caller is the defect and nothing in the type system was looking
// for it. Unexported, no package outside mls can reach a ratchet commit that is not behind the
// sender data AEAD, which is a property of the build rather than of a comment.
//
// WHAT THE RENAME COST, measured before it was made: grep -rn "\.ReceiverKey(" --include=*.go over
// connect, sdk and msgrepo answered 42 lines in exactly two files, secret_tree_test.go and
// secret_tree_kat_test.go, both package mls, and ZERO production call sites in any of the three
// repositories. It is not a member of MessageKeySource, so no interface moves with it. The two
// legitimate needs it used to serve -- look a generation up, then commit it -- are MessageKey and
// CommitMessageKey, which sit BEHIND the sender data open in the framing path and are where a
// receive path belongs.
//
// A returned error is a visible gap for the product, never a silent skip:
// ErrRatchetGenerationConsumed and ErrRatchetGenerationTooFarAhead both say the key never
// existed or no longer does, which is a different statement from ValSem006 -- that one is
// the AEAD refusing a message whose key was found.
//
// IT BYPASSES SENDER DATA AUTHENTICATION, and this paragraph is what the rename above answers
// rather than a caveat that has to hold on its own. Verified in this package's non test source:
// this method has ZERO production
// callers. The framing path reaches a generation number only after openSenderData has opened an
// AEAD under the epoch's sender_data_secret, and every argument written elsewhere in this file
// about what a peer can buy with one header -- peekFor's "the party choosing it is a member of this
// group", pruneRetained's bound over forged headers -- rests on that AEAD. It is an argument about
// the FRAMING PATH and not about this type's API, and this door is where the two come apart: a
// caller that hands this method a leaf index, a kind and a generation taken off the wire has
// skipped the AEAD, and what it buys per unauthenticated header is ratchetFor -- which takes the
// leaf node secret out of the tree and materialises both of that leaf's ratchets, destructively and
// for any leaf the tree has -- plus up to MaxGenerationSkip steps and the retention that goes with
// them.
//
// TWO OF THOSE ARE BOUNDED AND THE THIRD IS NOT, and the sentence that stood here said "the leaf
// index is the only thing here that is" bounded and then named a second bound in the next breath.
// Both bounds are real: the leaf index by takeLeafSecret's pathToLeaf, and the retained key memory
// by pruneRetained, tree wide. What has NO bound here is who is asking -- this door takes a leaf, a
// kind and a generation and asks nothing about where they came from -- so the work and the
// retention above are bounded PER CALL and unbounded in calls. So the first caller of this method
// owes its own answer to that question before it writes the call; the framing layer's answer is not
// inherited by coming through this door.
func (self *SecretTree) receiverKey(leaf LeafIndex, kind RatchetType, generation uint32) (key []byte, nonce []byte, err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	r, err := self.ratchetFor(leaf, kind)
	if err != nil {
		return nil, nil, err
	}
	keys, err := r.commitFor(generation)
	// the tree wide bound is applied whether or not the request was served. commitFor retains
	// every generation it steps past, and a request that fails partway through -- an
	// exhausted ratchet -- has retained them just the same, so a bound applied only on the
	// success path is one a peer walks around by always failing.
	//
	// The keys just handed out are not at risk from this: commitFor never leaves the target in
	// the window -- a retained one is deleted as it is handed over and a walked one is never
	// stored -- so by the time the bound is applied the answer is no longer an entry anything
	// can evict.
	self.pruneRetained()
	if err != nil {
		return nil, nil, err
	}
	return keys.key, keys.nonce, nil
}
