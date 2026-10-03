// The provider core: the sizes a suite fixes, the four primitives that carry no label,
// the aead wrappers, and the entropy source everything else draws from.
//
// Three of these have a failure mode no round trip can see, and each is pinned here to a
// published answer rather than to this package's own output. A mac that ignores its key
// still verifies against itself. An extract that reads its arguments in the library's
// order rather than the spec's still derives a stable secret both ends agree on. And a
// random that reverses, rotates or sorts the bytes it read still returns thirty two bytes
// that are never zero and never repeat — sorting them collapses a 256 bit key to about
// 141 bits, which is the defect this package shipped once already, in task 4, behind a
// test that fed its key generator a constant reader.
//
// The known answers therefore come from RFC 4231, RFC 5869 and FIPS 180-4 as the pinned
// toolchain's own crypto/hmac, crypto/hkdf and crypto/sha256 test tables carry them, and
// from the vendored RFC 9180 corpus for the aead. The primitives are the standard
// library's and are tested there; what these pin is this file's wiring, which is where
// the argument order, the key length and the reader live.
package mls

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// A provider over the process entropy source, with construction failure fatal: a test
// that carried on with a nil provider would panic somewhere that says less.
func mustProvider(t *testing.T, suite CipherSuite) CryptoProvider {
	t.Helper()
	crypto, err := NewCryptoProvider(suite)
	if err != nil {
		t.Fatalf("NewCryptoProvider(%#04x): %v", uint16(suite), err)
	}
	return crypto
}

// A provider over a caller's reader, for everything that asserts what Random consumed.
func mustProviderOver(t *testing.T, suite CipherSuite, random io.Reader) CryptoProvider {
	t.Helper()
	crypto, err := NewCryptoProviderWithRandom(suite, random)
	if err != nil {
		t.Fatalf("NewCryptoProviderWithRandom(%#04x): %v", uint16(suite), err)
	}
	return crypto
}

// What a call panicked with, or nil if it returned. Used rather than a bare defer so a
// table of inputs that must all be refused reads as a table.
func recoveredPanic(call func()) (recovered any) {
	defer func() { recovered = recover() }()
	call()
	return nil
}

// A hex known answer, decoded, with a malformed constant fatal rather than a comparison
// against nothing.
func mustDecodeHex(t *testing.T, name string, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatalf("%s is not hex: %v", name, err)
	}
	if len(decoded) == 0 {
		t.Fatalf("%s decoded to nothing", name)
	}
	return decoded
}

// The sizes, against literals rather than against the registry. Reading Nk back out of
// SuiteParams would pass for a provider that returned the other suite's parameters, which
// is the one mistake a two entry registry exists to expose.
func TestProviderSizes(t *testing.T) {
	for _, testCase := range []struct {
		suite        CipherSuite
		hash, key, n int
	}{
		{suite: CipherSuiteX25519AesGcm128Sha256Ed25519, hash: 32, key: 16, n: 12},
		{suite: CipherSuiteX25519ChaCha20Sha256Ed25519, hash: 32, key: 32, n: 12},
	} {
		crypto := mustProvider(t, testCase.suite)
		if crypto.Suite() != testCase.suite {
			t.Errorf("Suite() = %#04x, want %#04x", uint16(crypto.Suite()), uint16(testCase.suite))
		}
		if crypto.HashSize() != testCase.hash {
			t.Errorf("suite %#04x HashSize() = %d, want %d", uint16(testCase.suite), crypto.HashSize(), testCase.hash)
		}
		if crypto.KeySize() != testCase.key {
			t.Errorf("suite %#04x KeySize() = %d, want %d", uint16(testCase.suite), crypto.KeySize(), testCase.key)
		}
		if crypto.NonceSize() != testCase.n {
			t.Errorf("suite %#04x NonceSize() = %d, want %d", uint16(testCase.suite), crypto.NonceSize(), testCase.n)
		}
		// and the three lengths that are answers about this file rather than about the
		// registry: what Hash, Mac and Extract actually produce
		if got := len(crypto.Hash(nil)); got != testCase.hash {
			t.Errorf("suite %#04x Hash produced %d bytes for a HashSize of %d", uint16(testCase.suite), got, testCase.hash)
		}
		if got := len(crypto.Mac(nil, nil)); got != testCase.hash {
			t.Errorf("suite %#04x Mac produced %d bytes for a HashSize of %d", uint16(testCase.suite), got, testCase.hash)
		}
		if got := len(crypto.Extract(nil, nil)); got != testCase.hash {
			t.Errorf("suite %#04x Extract produced %d bytes for a HashSize of %d", uint16(testCase.suite), got, testCase.hash)
		}
	}
}

// Hash, Mac, Extract and Expand are written against sha256 directly rather than selected
// from the suite, which is right for the two entries the registry holds and would be
// silently wrong for a third. This is the guard, and it reads the registry rather than
// crypto.go: a suite added later whose kdf is not HKDF-SHA256, or whose Nh is not 32,
// fails here rather than deriving every one of its secrets under the wrong hash.
func TestEverySuiteNamesTheHashTheProviderComputes(t *testing.T) {
	suites := Suites()
	if len(suites) == 0 {
		t.Fatal("the registry is empty, so this guard reads nothing")
	}
	for _, suite := range suites {
		params, err := LookupSuite(suite)
		if err != nil {
			t.Fatalf("LookupSuite(%#04x): %v", uint16(suite), err)
		}
		if params.KdfId != HpkeKdfHkdfSha256 {
			t.Errorf("suite %#04x names kdf %#04x, but the provider computes HKDF-SHA256", uint16(suite), uint16(params.KdfId))
		}
		if params.Nh != sha256.Size {
			t.Errorf("suite %#04x has Nh %d, but the provider computes a %d byte hash", uint16(suite), params.Nh, sha256.Size)
		}
	}
}

// Both halves of the refusal. "Every code point is refused" is the other way this can be
// wrong, and it passes the refusal on its own.
func TestProviderRefusesUnknownSuite(t *testing.T) {
	for _, suite := range []CipherSuite{0x0000, 0x0002, 0x0004, 0x0007, 0xffff} {
		crypto, err := NewCryptoProvider(suite)
		if !errors.Is(err, ErrUnknownCipherSuite) {
			t.Errorf("NewCryptoProvider(%#04x) error = %v, want ErrUnknownCipherSuite", uint16(suite), err)
		}
		if crypto != nil {
			t.Errorf("NewCryptoProvider(%#04x) returned a provider alongside %v", uint16(suite), err)
		}
		if crypto, err := NewCryptoProviderWithRandom(suite, rand.Reader); !errors.Is(err, ErrUnknownCipherSuite) || crypto != nil {
			t.Errorf("NewCryptoProviderWithRandom(%#04x) = %v, %v, want nil and ErrUnknownCipherSuite", uint16(suite), crypto, err)
		}
	}
	for _, suite := range Suites() {
		if _, err := NewCryptoProvider(suite); err != nil {
			t.Errorf("NewCryptoProvider(%#04x) refused a registered suite: %v", uint16(suite), err)
		}
	}
}

// FIPS 180-4, as crypto/sha256's own golden table carries it. Hash takes no key and no
// suite, so the only thing that can be wrong about it is which hash it is — and a
// truncated or doubled digest is exactly the shape of mistake a length check misses.
//
// The last three rows are here for their lengths rather than for their bytes. The rows
// above them are all shorter than one sha256 block and shorter than a digest, so a hash
// that read only the first block, or only the first thirty two bytes, agrees with every
// one of them. FIPS 180-2 appendix B's two multi block messages and the sentence from
// RFC 4634 cover 43, 56 and 112 bytes, which reach the second block and past it.
func TestProviderHashMatchesTheFipsGoldens(t *testing.T) {
	for _, testCase := range []struct {
		in  string
		out string
	}{
		{in: "", out: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{in: "a", out: "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb"},
		{in: "abc", out: "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{in: "abcd", out: "88d4266fd4e6338d13b845fcf289579d209c897823b9217da3e161936f031589"},
		{in: "Nepal premier won't resign.", out: "7102cfd76e2e324889eece5d6c41921b1e142a4ac5a2692be78803097f6a48d8"},
		{
			in:  "The quick brown fox jumps over the lazy dog",
			out: "d7a8fbb307d7809469ca9abcb0082e4f8d5651e46d3cdb762d02d0bf37c9e592",
		},
		{
			in:  "abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq",
			out: "248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1",
		},
		{
			in: "abcdefghbcdefghicdefghijdefghijkefghijklfghijklmghijklmn" +
				"hijklmnoijklmnopjklmnopqklmnopqrlmnopqrsmnopqrstnopqrstu",
			out: "cf5b16a778af8380036ce59e7b0492370b249b11e8f07a51afac45037afee9d1",
		},
	} {
		want := mustDecodeHex(t, "the digest of "+testCase.in, testCase.out)
		for _, suite := range Suites() {
			crypto := mustProvider(t, suite)
			if got := crypto.Hash([]byte(testCase.in)); !bytes.Equal(got, want) {
				t.Errorf("suite %#04x Hash(%q) = %x, want %x", uint16(suite), testCase.in, got, want)
			}
		}
	}
	// nil and empty are the same message and must hash alike, which is the input that
	// separates a hash from one substituting a default for a caller that passed none
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	if !bytes.Equal(crypto.Hash(nil), crypto.Hash([]byte{})) {
		t.Errorf("Hash(nil) = %x and Hash of an empty slice = %x", crypto.Hash(nil), crypto.Hash([]byte{}))
	}
	// and a message of exactly HashSize bytes, which no row above is and which is the
	// length RefHash will hand this in task 13. A copy through or an aliasing defect is
	// most natural exactly where the input and the output are the same size, and every
	// row above is short enough for one to hide in. The message is the digest of the
	// empty string and the answer is therefore the published double sha256 of it.
	digestOfEmpty := mustDecodeHex(t, "the digest of the empty message",
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	doubleDigest := mustDecodeHex(t, "the double digest of the empty message",
		"5df6e0e2761359d30a8275058e299fcc0381534545f55cf43e41983f5d4c9456")
	// the probe has to be able to see what it is aimed at: an all zero or palindromic
	// message of that length would make a reversing hash the identity and this row would
	// pass a defect rather than catch it
	assertProbeIsNotItsOwnPermutation(t, "the thirty two byte message", digestOfEmpty)
	if len(digestOfEmpty) != crypto.HashSize() {
		t.Fatalf("the thirty two byte message is %d bytes and HashSize is %d", len(digestOfEmpty), crypto.HashSize())
	}
	for _, suite := range Suites() {
		if got := mustProvider(t, suite).Hash(digestOfEmpty); !bytes.Equal(got, doubleDigest) {
			t.Errorf("suite %#04x Hash of a %d byte message = %x, want %x",
				uint16(suite), len(digestOfEmpty), got, doubleDigest)
		}
	}
}

// A probe over which sorting, reversing, rotating and collapsing to a constant are all
// visible. Every one of those is the identity on some buffer — an ascending one is
// already sorted, a palindromic one already reads backwards, an all zero one is all four
// — and a probe that happens to be one of them turns a test that looks like it catches a
// weakened primitive into a test that cannot. Task 4 shipped a 256 bit key collapsed to
// about 141 bits behind exactly that, so this is asserted before the probe is used.
func assertProbeIsNotItsOwnPermutation(t *testing.T, name string, probe []byte) {
	t.Helper()
	if len(probe) < 2 {
		t.Fatalf("%s is %d bytes, which no permutation can move", name, len(probe))
	}
	ordered := bytes.Clone(probe)
	slices.Sort(ordered)
	if bytes.Equal(probe, ordered) {
		t.Errorf("%s %x is already sorted, so a sorting defect is invisible in it", name, probe)
	}
	slices.Reverse(ordered)
	if bytes.Equal(probe, ordered) {
		t.Errorf("%s %x is sorted descending", name, probe)
	}
	reversed := bytes.Clone(probe)
	slices.Reverse(reversed)
	if bytes.Equal(probe, reversed) {
		t.Errorf("%s %x reads the same backwards, so a reversing defect is invisible in it", name, probe)
	}
	for shift := 1; shift < len(probe); shift++ {
		rotated := append(bytes.Clone(probe[shift:]), probe[:shift]...)
		if bytes.Equal(probe, rotated) {
			t.Errorf("%s %x equals itself rotated by %d", name, probe, shift)
		}
	}
	if bytes.Equal(probe, bytes.Repeat(probe[:1], len(probe))) {
		t.Errorf("%s %x is constant", name, probe)
	}
}

// One RFC 4231 known answer.
type macVector struct {
	name string
	key  []byte
	data []byte
	tag  string
}

// RFC 4231, as crypto/hmac's own table carries it. Case 6's key is longer than the sha256
// block, which is the one input that reaches hmac's key hashing branch: an implementation
// that truncated a long key instead would match every other row here.
func rfc4231Vectors() []macVector {
	return []macVector{
		{
			name: "RFC 4231 test case 1",
			key:  bytes.Repeat([]byte{0x0b}, 20),
			data: []byte("Hi There"),
			tag:  "b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7",
		},
		{
			name: "RFC 4231 test case 2",
			key:  []byte("Jefe"),
			data: []byte("what do ya want for nothing?"),
			tag:  "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843",
		},
		{
			name: "RFC 4231 test case 3",
			key:  bytes.Repeat([]byte{0xaa}, 20),
			data: bytes.Repeat([]byte{0xdd}, 50),
			tag:  "773ea91e36800e46854db8ebd09181a72959098b3ef8c122d9635514ced565fe",
		},
		{
			name: "RFC 4231 test case 4",
			key:  ascendingBytes(0x01, 25),
			data: bytes.Repeat([]byte{0xcd}, 50),
			tag:  "82558a389a443c0ea4cc819899f2083a85f0faa3e578f8077a2e3ff46729665b",
		},
		{
			name: "RFC 4231 test case 6, a key longer than the block",
			key:  bytes.Repeat([]byte{0xaa}, 131),
			data: []byte("Test Using Larger Than Block-Size Key - Hash Key First"),
			tag:  "60e431591ee0b67f0d8a26aacbf5b77f8e0bc6213728c5140546040f0ee37f54",
		},
		{
			name: "RFC 4231 test case 7, key and data longer than the block",
			key:  bytes.Repeat([]byte{0xaa}, 131),
			data: []byte("This is a test using a larger than block-size key " +
				"and a larger than block-size data. The key needs to " +
				"be hashed before being used by the HMAC algorithm."),
			tag: "9b09ffa71b942fcb27635fbcd5b0e944bfdc63644f0713938a7f51535c3a35e2",
		},
	}
}

// The run of consecutive byte values the published tables write out in full.
func ascendingBytes(first byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = first + byte(i)
	}
	return b
}

// The published tag is what both Mac and MacVerify are held to. Holding MacVerify to
// Mac's own output instead would be satisfied by a pair that both ignored the key, which
// is the classic mac defect and the one a round trip cannot see.
func TestProviderMacMatchesRfc4231(t *testing.T) {
	for _, vector := range rfc4231Vectors() {
		want := mustDecodeHex(t, vector.name, vector.tag)
		for _, suite := range Suites() {
			crypto := mustProvider(t, suite)
			if got := crypto.Mac(vector.key, vector.data); !bytes.Equal(got, want) {
				t.Errorf("suite %#04x %s: Mac = %x, want %x", uint16(suite), vector.name, got, want)
			}
			if !crypto.MacVerify(vector.key, vector.data, want) {
				t.Errorf("suite %#04x %s: MacVerify rejected the published tag", uint16(suite), vector.name)
			}
		}
	}
}

// The lengths RFC 4231 does not carry, against hmac's own answer rather than against a
// table. HKDF-Extract is HMAC keyed on the salt, and this file already pins the argument
// order that way; the same construction pins the inputs the published table has no row
// for, which is where a substituted default hides.
//
// RFC 4231's shortest key is twenty bytes, its shortest message is eight, and none of its
// rows is thirty two bytes of key — the length every key this package derives will be. So
// a mac that substituted a house key for an absent one, or hashed a key of exactly its own
// digest length first, or read an absent message as some default, agrees with all six
// published rows. Under a substituted default a caller handing no key still gets a tag that
// verifies, and authenticates nobody: the silent downgrade this file exists to refuse.
func TestProviderMacMatchesAnIndependentHmac(t *testing.T) {
	independent := func(key []byte, data []byte) []byte {
		mac := hmac.New(sha256.New, key)
		mac.Write(data)
		return mac.Sum(nil)
	}
	for _, testCase := range []struct {
		name string
		key  []byte
		data []byte
	}{
		{name: "no key at all", key: nil, data: []byte("authenticated data")},
		{name: "an empty key", key: []byte{}, data: []byte("authenticated data")},
		{name: "a key of exactly the digest length", key: bytes.Repeat([]byte{0x0d}, 32), data: []byte("authenticated data")},
		{name: "a key of exactly the block length", key: bytes.Repeat([]byte{0x0d}, 64), data: []byte("authenticated data")},
		{name: "no message at all", key: bytes.Repeat([]byte{0x0d}, 32), data: nil},
		{name: "an empty message", key: bytes.Repeat([]byte{0x0d}, 32), data: []byte{}},
		{name: "neither a key nor a message", key: nil, data: nil},
	} {
		want := independent(testCase.key, testCase.data)
		for _, suite := range Suites() {
			crypto := mustProvider(t, suite)
			if got := crypto.Mac(testCase.key, testCase.data); !bytes.Equal(got, want) {
				t.Errorf("suite %#04x Mac with %s = %x, want %x", uint16(suite), testCase.name, got, want)
			}
			if !crypto.MacVerify(testCase.key, testCase.data, want) {
				t.Errorf("suite %#04x MacVerify with %s rejected hmac's own tag", uint16(suite), testCase.name)
			}
		}
	}
	// and nil and empty are the same absent key and the same absent message, the pair a
	// wrapper telling them apart would answer differently for
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	data := []byte("authenticated data")
	if !bytes.Equal(crypto.Mac(nil, data), crypto.Mac([]byte{}, data)) {
		t.Errorf("Mac reads a nil key and an empty one differently")
	}
	key := bytes.Repeat([]byte{0x0d}, 32)
	if !bytes.Equal(crypto.Mac(key, nil), crypto.Mac(key, []byte{})) {
		t.Errorf("Mac reads a nil message and an empty one differently")
	}
}

// The key has to reach the tag. This is the claim the published table already makes, made
// again over inputs it does not carry, because a mac that hashed only its data agrees
// with nothing published while one that mixed its key in weakly might.
func TestProviderMacDependsOnItsKey(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	data := []byte("authenticated data")
	base := bytes.Repeat([]byte{0x0d}, 32)
	baseTag := crypto.Mac(base, data)
	for _, other := range [][]byte{
		nil,
		{},
		bytes.Repeat([]byte{0x0d}, 31),
		bytes.Repeat([]byte{0x0d}, 33),
		bytes.Repeat([]byte{0x0e}, 32),
		append(bytes.Clone(base[:31]), 0x0c),
		append([]byte{0x0c}, base[1:]...),
	} {
		if got := crypto.Mac(other, data); bytes.Equal(got, baseTag) {
			t.Errorf("Mac under the key %x produced the tag of the key %x", other, base)
		}
		if crypto.MacVerify(other, data, baseTag) {
			t.Errorf("MacVerify accepted a tag made under a different key %x", other)
		}
	}
	for _, other := range [][]byte{nil, {}, []byte("authenticated dat"), []byte("authenticated data!"), []byte("Authenticated data")} {
		if got := crypto.Mac(base, other); bytes.Equal(got, baseTag) {
			t.Errorf("Mac over %q produced the tag of %q", other, data)
		}
	}
}

// Every alteration of the tag, enumerated rather than sampled. A hand written list of
// interesting tags is what this project has understated five times running, and the space
// here is small enough to walk: every single bit of the tag, every truncation, and every
// extension by one byte.
func TestProviderMacVerifyRefusesEveryAlteredTag(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	key := bytes.Repeat([]byte{0x0d}, 32)
	data := []byte("authenticated data")
	tag := crypto.Mac(key, data)
	if !crypto.MacVerify(key, data, tag) {
		t.Fatalf("MacVerify rejected its own tag, so every refusal below means nothing")
	}
	refused := 0
	for i := range tag {
		for bit := 0; bit < 8; bit++ {
			altered := bytes.Clone(tag)
			altered[i] ^= 1 << bit
			if crypto.MacVerify(key, data, altered) {
				t.Errorf("MacVerify accepted a tag with bit %d of byte %d flipped", bit, i)
				continue
			}
			refused++
		}
	}
	if want := 8 * len(tag); refused != want {
		t.Errorf("%d of %d single bit alterations were refused", refused, want)
	}
	for n := 0; n < len(tag); n++ {
		if crypto.MacVerify(key, data, tag[:n]) {
			t.Errorf("MacVerify accepted the first %d bytes of a %d byte tag", n, len(tag))
		}
	}
	for _, extra := range []byte{0x00, 0xff} {
		if crypto.MacVerify(key, data, append(bytes.Clone(tag), extra)) {
			t.Errorf("MacVerify accepted a tag with a trailing %#02x", extra)
		}
	}
	if crypto.MacVerify(key, data, nil) {
		t.Errorf("MacVerify accepted a nil tag")
	}
	if crypto.MacVerify(key, data, make([]byte, len(tag))) {
		t.Errorf("MacVerify accepted an all zero tag")
	}
}

// The file the comparison lives in, and the declaration this gate reads out of it.
const (
	macVerifySourcePath = "crypto.go"
	macVerifySignature  = "func (self *suiteCryptoProvider) MacVerify("
)

// The tokens that make a comparison of secret bytes leak where the first difference is,
// and the one that does not. Guardrail 8 names crypto/subtle.ConstantTimeCompare
// specifically, so the marker is that call rather than merely some constant time function.
var (
	variableTimeComparisons = []string{"bytes.Equal(", "bytes.Compare(", "strings.Compare(", "string(tag)", "string(expected)"}
	constantTimeComparison  = "subtle.ConstantTimeCompare("
)

// One method's body, from its signature to the first line holding nothing but a closing
// brace. Comments are stripped first, so the prose in this file and in crypto.go naming
// bytes.Equal is not what either the gate or its control reads.
func methodBody(t *testing.T, source string, signature string) string {
	t.Helper()
	_, after, found := strings.Cut(codeOf(source), signature)
	if !found {
		t.Fatalf("no declaration of %s", signature)
	}
	body := []string{}
	for _, line := range strings.Split(after, "\n") {
		if line == "}" {
			return strings.Join(body, "\n")
		}
		body = append(body, line)
	}
	t.Fatalf("the declaration of %s is not closed by a line holding only a brace", signature)
	return ""
}

// The comparisons in one body that leak the position of the first differing byte.
func variableTimeComparisonsIn(body string) []string {
	found := []string{}
	for _, token := range variableTimeComparisons {
		if strings.Contains(body, token) {
			found = append(found, token)
		}
	}
	return found
}

// One parsed go file, with the positions its statements can be rendered back through.
//
// Reading the parse tree rather than the file's characters is what turns a token list
// into a shape. A hand written list of banned spellings is the thing this project has
// understated five times running, and a comparison written as control flow carries none
// of the tokens such a list holds while leaking exactly what the ban was about: a plain
// byte loop inserted ahead of a subtle call passes a token gate and returns on the first
// differing byte. A statement list does not care how the leak is spelled.
type parsedSource struct {
	fileSet *token.FileSet
	file    *ast.File
}

// One file of this package, parsed. The path is read at test time rather than embedded,
// so the gate reads what a reviewer will read.
func mustParseSource(t *testing.T, path string) parsedSource {
	t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return mustParseText(t, path, string(source))
}

// The same, over text a control holds, so every matcher below runs on a body known to
// violate the rule as well as on the real one.
func mustParseText(t *testing.T, name string, source string) parsedSource {
	t.Helper()
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, name, source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return parsedSource{fileSet: fileSet, file: file}
}

// The declaration of one function, or of one method on a named receiver type. Absence is
// fatal rather than clean: a gate that stopped finding its subject must fail, not report
// the subject it never read as compliant.
func (self parsedSource) declarationOf(t *testing.T, receiver string, name string) *ast.FuncDecl {
	t.Helper()
	for _, declaration := range self.file.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Name.Name != name || self.receiverOf(function) != receiver {
			continue
		}
		if function.Body == nil {
			t.Fatalf("the declaration of %s %s has no body", receiver, name)
		}
		return function
	}
	t.Fatalf("no declaration of %s %s in %s", receiver, name, self.file.Name.Name)
	return nil
}

// The receiver type of a declaration as it is written, or the empty string for a plain
// function.
func (self parsedSource) receiverOf(function *ast.FuncDecl) string {
	if function.Recv == nil || len(function.Recv.List) != 1 {
		return ""
	}
	return self.render(function.Recv.List[0].Type)
}

// One node back as source text. Rendering rather than slicing the original bytes is what
// makes an expected statement a constant this file can hold: whitespace and line breaks
// come out canonical however the file was written.
func (self parsedSource) render(node ast.Node) string {
	out := &bytes.Buffer{}
	if err := printer.Fprint(out, self.fileSet, node); err != nil {
		return "!render failed: " + err.Error()
	}
	return out.String()
}

// The statements of one declaration, each rendered.
func (self parsedSource) statementsOf(t *testing.T, receiver string, name string) []string {
	t.Helper()
	rendered := []string{}
	for _, statement := range self.declarationOf(t, receiver, name).Body.List {
		rendered = append(rendered, self.render(statement))
	}
	return rendered
}

// The names of every method on one receiver type declared in this file. What the gates
// below need it for is finding their own subject: a gate told which file to read reports
// a clean bill on a file the implementation has moved out of, which is exactly what
// happened when task 12 put three provider methods in a second file.
func (self parsedSource) methodsOn(receiver string) []string {
	names := []string{}
	for _, declaration := range self.file.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || self.receiverOf(function) != receiver {
			continue
		}
		names = append(names, function.Name.Name)
	}
	slices.Sort(names)
	return names
}

// packageAliasMarker is the entry the matchers below report instead of a call list when a
// file has DOT imported the package they are asked about.
//
// A dot imported entry point is spelled as a bare identifier -- Unmarshal(data, v) rather
// than syntax.Unmarshal(data, v) -- so it has no selector for either matcher to match, and
// reporting nothing would be reporting a clean file. This entry has no valid spelling, so a
// gate holding an exact list fails on it rather than passing over a whole import.
const packageAliasMarker = " IS DOT IMPORTED, so its entry points are bare identifiers and this matcher cannot see them"

// Every identifier this file can spell one imported package with, and whether it dot
// imported it.
//
// Derived off the file's own import declarations rather than assumed to be the package
// name. A renamed import spells the SAME entry point under a different first identifier,
// and a matcher keyed on the literal name reports it as no call at all -- which for a gate
// holding an exact list is a new entry point that joins nothing. Measured: adding
// `sx "github.com/urnetwork/connect/mls/syntax"` beside the plain import in welcome_wire.go
// together with an sx.UnmarshalLimit(data, welcome, sx.MaxRatchetTreeLength) entry point
// left TestEverySyntaxEncoderInThisPackageUsesTheDefaultLimit passing -- a brand new decode
// at the RAISED limit, unrecorded by the gate whose whole subject is which limit this
// package enters the codec at.
//
// The package's own name is always in the set, so a control source that spells a call
// without importing anything is still read.
func (self parsedSource) namesOfImportedPackage(pkg string) ([]string, bool) {
	names, dotImported := []string{pkg}, false
	for _, imported := range self.file.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil || path[strings.LastIndex(path, "/")+1:] != pkg {
			continue
		}
		if imported.Name == nil {
			continue
		}
		switch imported.Name.Name {
		case ".":
			dotImported = true
		case "_":
		default:
			if !slices.Contains(names, imported.Name.Name) {
				names = append(names, imported.Name.Name)
			}
		}
	}
	return names, dotImported
}

// Every call in this file written as a selector on one package, rendered under the
// package's OWN name whichever identifier the file spelled it with. A gate over which
// constructor a package is entered through reads this rather than the file's characters,
// so a call spelled across two lines or with a differently named import still reports as
// the call it is.
func (self parsedSource) callsToPackage(pkg string) []string {
	names, dotImported := self.namesOfImportedPackage(pkg)
	calls := []string{}
	if dotImported {
		calls = append(calls, pkg+packageAliasMarker)
	}
	ast.Inspect(self.file, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		selector, isSelector := call.Fun.(*ast.SelectorExpr)
		if !isSelector {
			return true
		}
		if base, isIdentifier := selector.X.(*ast.Ident); isIdentifier && slices.Contains(names, base.Name) {
			calls = append(calls, pkg+strings.TrimPrefix(self.render(call), base.Name))
		}
		return true
	})
	slices.Sort(calls)
	return calls
}

// Every assignment in the file that writes a field of a method's own receiver, as
// "Method: statement". This is the mechanical half of the statelessness claim: a field
// nobody writes is safe to share, and this reports the writes rather than trusting the
// struct to stay the shape it is.
func (self parsedSource) receiverFieldWrites(receiver string) []string {
	writes := []string{}
	for _, declaration := range self.file.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Body == nil || self.receiverOf(function) != receiver {
			continue
		}
		names := function.Recv.List[0].Names
		if len(names) != 1 {
			continue
		}
		receiverName := names[0].Name
		ast.Inspect(function.Body, func(node ast.Node) bool {
			targets := []ast.Expr{}
			switch statement := node.(type) {
			case *ast.AssignStmt:
				targets = statement.Lhs
			case *ast.IncDecStmt:
				targets = []ast.Expr{statement.X}
			}
			for _, target := range targets {
				selector, isSelector := target.(*ast.SelectorExpr)
				if !isSelector {
					continue
				}
				base, isIdentifier := selector.X.(*ast.Ident)
				if isIdentifier && base.Name == receiverName {
					writes = append(writes, function.Name.Name+": "+self.render(node))
				}
			}
			return true
		})
	}
	slices.Sort(writes)
	return writes
}

// Guardrail 8, checked by reading the source rather than by measuring, because a timing
// measurement over a 32 byte comparison is noise on this machine and would be a flake in
// continuous integration rather than a gate. What is asserted is therefore exactly what
// was verified: the comparison is the constant time one by inspection, and the inspection
// is mechanical so it survives an edit nobody thinks to rerun it for.
//
// The matcher runs on a control body as well as on the real one. Without that half, a
// matcher that had stopped matching would report the real body clean and pass.
func TestMacVerifyComparesInConstantTime(t *testing.T) {
	source, err := os.ReadFile(macVerifySourcePath)
	if err != nil {
		t.Fatalf("read %s: %v", macVerifySourcePath, err)
	}
	body := methodBody(t, string(source), macVerifySignature)
	if !strings.Contains(body, constantTimeComparison) {
		t.Errorf("the body of MacVerify does not call %s:\n%s", constantTimeComparison, body)
	}
	if found := variableTimeComparisonsIn(body); len(found) != 0 {
		t.Errorf("the body of MacVerify compares with %v", found)
	}
	control := "\texpected := self.Mac(key, data)\n\treturn bytes.Equal(expected, tag)"
	if found := variableTimeComparisonsIn(control); !slices.Equal(found, []string{"bytes.Equal("}) {
		t.Errorf("the matcher reported %v in a body that compares with bytes.Equal", found)
	}
	if strings.Contains(control, constantTimeComparison) {
		t.Errorf("the control body claims to call %s", constantTimeComparison)
	}
}

// The receiver every method of the one implementation hangs off, as it is written.
const providerReceiver = "*suiteCryptoProvider"

// Exactly what MacVerify is allowed to be. The token gate above cannot see a comparison
// spelled as control flow: a plain byte loop inserted ahead of the subtle call leaks the
// position of the first differing byte, carries none of the banned tokens, and still
// contains the required one. So the shape is pinned instead — these three statements and
// nothing else — and a fourth statement of any kind fails here.
var macVerifyStatements = []string{
	"expected := self.Mac(key, data)",
	"if len(tag) != len(expected) {\n\treturn false\n}",
	"return subtle.ConstantTimeCompare(expected, tag) == 1",
}

// A MacVerify that a token gate reports clean and that leaks anyway. Every matcher below
// runs on this as well as on crypto.go, so a matcher that stopped matching fails here
// rather than issuing the real body a clean bill.
const macVerifyLeakingControl = `package mls

func (self *suiteCryptoProvider) MacVerify(key []byte, data []byte, tag []byte) bool {
	expected := self.Mac(key, data)
	if len(tag) != len(expected) {
		return false
	}
	for i := range expected {
		if expected[i] != tag[i] {
			return false
		}
	}
	return subtle.ConstantTimeCompare(expected, tag) == 1
}
`

// Guardrail 8 again, as a shape rather than as a word list. What the token gate above
// asserts is that a named variable time call is absent; what this asserts is that the
// comparison is the only thing in the method, which is the claim the guardrail actually
// makes. The control is a body the token gate passes and this one does not, so the two
// halves are not the same assertion written twice.
func TestMacVerifyIsOnlyTheConstantTimeComparison(t *testing.T) {
	source := mustParseSource(t, macVerifySourcePath)
	got := source.statementsOf(t, providerReceiver, "MacVerify")
	if !slices.Equal(got, macVerifyStatements) {
		t.Errorf("MacVerify is\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(macVerifyStatements, "\n"))
	}
	control := mustParseText(t, "the leaking control", macVerifyLeakingControl)
	leaking := control.statementsOf(t, providerReceiver, "MacVerify")
	if slices.Equal(leaking, macVerifyStatements) {
		t.Errorf("the matcher read a body with a byte loop in it as the shape above")
	}
	// and the control is one the token gate really does pass, or this test is measuring
	// a body the other gate would have caught anyway
	body := methodBody(t, macVerifyLeakingControl, macVerifySignature)
	if !strings.Contains(body, constantTimeComparison) || len(variableTimeComparisonsIn(body)) != 0 {
		t.Errorf("the leaking control does not slip past the token gate, so this shape gate is untested")
	}
}

// One RFC 5869 known answer.
type hkdfVector struct {
	name string
	ikm  []byte
	salt []byte
	prk  string
	info []byte
	okm  string
}

// RFC 5869 appendix A, the sha256 cases, as crypto/hkdf's own table carries them. The
// fourth row is the third with nil where it writes an empty slice: nil and empty are the
// same absent salt, and a wrapper substituting a default for one of them would agree with
// the published answer on only one of the two rows.
func rfc5869Vectors() []hkdfVector {
	return []hkdfVector{
		{
			name: "RFC 5869 test case 1",
			ikm:  bytes.Repeat([]byte{0x0b}, 22),
			salt: ascendingBytes(0x00, 13),
			prk:  "077709362c2e32df0ddc3f0dc47bba6390b6c73bb50f9c3122ec844ad7c2b3e5",
			info: ascendingBytes(0xf0, 10),
			okm:  "3cb25f25faacd57a90434f64d0362f2a2d2d0a90cf1a5a4c5db02d56ecc4c5bf34007208d5b887185865",
		},
		{
			name: "RFC 5869 test case 2, inputs longer than the block",
			ikm:  ascendingBytes(0x00, 80),
			salt: ascendingBytes(0x60, 80),
			prk:  "06a6b88c5853361a06104c9ceb35b45cef760014904671014a193f40c15fc244",
			info: ascendingBytes(0xb0, 80),
			okm: "b11e398dc80327a1c8e7f78c596a49344f012eda2d4efad8a050cc4c19afa97c" +
				"59045a99cac7827271cb41c65e590e09da3275600c2f09b8367793a9aca3db71" +
				"cc30c58179ec3e87c14c01d5c1f3434f1d87",
		},
		{
			name: "RFC 5869 test case 3, empty salt and info",
			ikm:  bytes.Repeat([]byte{0x0b}, 22),
			salt: []byte{},
			prk:  "19ef24a32c717b167f33a91d6f648bdf96596776afdb6377ac434c1c293ccb04",
			info: []byte{},
			okm:  "8da4e775a563c18f715f802a063c5a31b8a11f5c5ee1879ec3454e5f3c738d2d9d201395faa4b61a96c8",
		},
		{
			name: "RFC 5869 test case 3 with nil where it writes empty",
			ikm:  bytes.Repeat([]byte{0x0b}, 22),
			salt: nil,
			prk:  "19ef24a32c717b167f33a91d6f648bdf96596776afdb6377ac434c1c293ccb04",
			info: nil,
			okm:  "8da4e775a563c18f715f802a063c5a31b8a11f5c5ee1879ec3454e5f3c738d2d9d201395faa4b61a96c8",
		},
	}
}

// Guardrail 1, against the published pseudorandom keys rather than against this package.
// crypto/hkdf.Extract is (ikm, salt) and every spec text in this project writes
// HKDF-Extract(salt, ikm); the two are distinguishable only by a value neither side of
// the swap invented, which is what RFC 5869 prints.
func TestProviderExtractMatchesRfc5869(t *testing.T) {
	for _, vector := range rfc5869Vectors() {
		want := mustDecodeHex(t, vector.name, vector.prk)
		for _, suite := range Suites() {
			crypto := mustProvider(t, suite)
			if got := crypto.Extract(vector.salt, vector.ikm); !bytes.Equal(got, want) {
				t.Errorf("suite %#04x %s: Extract(salt, ikm) = %x, want %x — the arguments are swapped",
					uint16(suite), vector.name, got, want)
			}
			if len(vector.salt) == 0 || bytes.Equal(vector.salt, vector.ikm) {
				continue
			}
			if got := crypto.Extract(vector.ikm, vector.salt); bytes.Equal(got, want) {
				t.Errorf("suite %#04x %s: Extract is symmetric in salt and ikm, which is impossible for hmac",
					uint16(suite), vector.name)
			}
		}
	}
}

// The same claim by an independent construction rather than by a table: HKDF-Extract is
// literally HMAC keyed on the salt over the input keying material, so a reader holding
// crypto/hmac can check the order without a vector at all.
func TestProviderExtractArgumentOrder(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	salt := []byte("this is the salt")
	ikm := []byte("this is the input keying material")

	mac := hmac.New(sha256.New, salt)
	mac.Write(ikm)
	want := mac.Sum(nil)

	if got := crypto.Extract(salt, ikm); !bytes.Equal(got, want) {
		t.Fatalf("Extract(salt, ikm) = %x, want %x — the arguments are swapped", got, want)
	}
	if got := crypto.Extract(ikm, salt); bytes.Equal(got, want) {
		t.Fatalf("Extract is symmetric, which is impossible for hmac")
	}
}

// Expand against the published output keying material, at the published lengths. The
// lengths matter as much as the bytes: case 2 asks for 82, which is three sha256 blocks,
// so a counter that restarted or that stopped after one block fails here and passes every
// fixed 32 byte derivation this package otherwise performs.
func TestProviderExpandMatchesRfc5869(t *testing.T) {
	for _, vector := range rfc5869Vectors() {
		prk := mustDecodeHex(t, vector.name+" prk", vector.prk)
		want := mustDecodeHex(t, vector.name+" okm", vector.okm)
		for _, suite := range Suites() {
			crypto := mustProvider(t, suite)
			if got := crypto.Expand(prk, vector.info, len(want)); !bytes.Equal(got, want) {
				t.Errorf("suite %#04x %s: Expand = %x, want %x", uint16(suite), vector.name, got, want)
			}
		}
	}
}

// The property separating HKDF-Expand from a wrapper that folded the length into the
// info: the output for a short length is a prefix of the output for a long one. The
// labelled expand in hpke.go deliberately does not have it, so calling that one here
// would fail this and nothing else in this file.
func TestProviderExpandIsAPrefixStream(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	prk := crypto.Extract([]byte("salt"), []byte("ikm"))
	long := crypto.Expand(prk, []byte("info"), 200)
	for _, length := range []int{0, 1, 16, 32, 33, 64, 199, 200} {
		got := crypto.Expand(prk, []byte("info"), length)
		if len(got) != length {
			t.Errorf("Expand asked for %d bytes returned %d", length, len(got))
			continue
		}
		if !bytes.Equal(got, long[:length]) {
			t.Errorf("Expand at %d = %x, want the first %d bytes of %x", length, got, length, long)
		}
	}
	if bytes.Equal(crypto.Expand(prk, []byte("info"), 32), crypto.Expand(prk, []byte("other"), 32)) {
		t.Errorf("Expand ignores its info")
	}
	if bytes.Equal(crypto.Expand(prk, nil, 32), crypto.Expand(prk, []byte{0x00}, 32)) {
		t.Errorf("Expand reads an empty info and a single zero byte alike, so the conversion to string is not byte preserving")
	}
	if !bytes.Equal(crypto.Expand(prk, nil, 32), crypto.Expand(prk, []byte{}, 32)) {
		t.Errorf("Expand distinguishes a nil info from an empty one")
	}
	if bytes.Equal(crypto.Expand(prk, []byte("info"), 32), crypto.Expand(crypto.Extract([]byte("other salt"), []byte("ikm")), []byte("info"), 32)) {
		t.Errorf("Expand ignores its pseudorandom key")
	}
	// every prk above is exactly the hash length, which is the one a wrapper truncating
	// or padding its key agrees with. A longer one has to reach the hmac whole
	long64 := append(bytes.Clone(prk), crypto.Extract([]byte("second salt"), []byte("ikm"))...)
	if bytes.Equal(crypto.Expand(long64, []byte("info"), 32), crypto.Expand(long64[:32], []byte("info"), 32)) {
		t.Errorf("Expand truncates a pseudorandom key longer than the hash")
	}
	if bytes.Equal(crypto.Expand(long64, []byte("info"), 32), crypto.Expand(long64[32:], []byte("info"), 32)) {
		t.Errorf("Expand reads only the tail of a pseudorandom key longer than the hash")
	}
}

// The other end of the same refusal. RFC 5869 section 2.3 requires a pseudorandom key of
// at least the hash length, crypto/hkdf checks it only in fips140-only mode, and expanding
// from a shorter one hands back every byte that was asked for while deriving all of them
// from less entropy than the suite claims. That is the same silent downgrade as a short
// return arriving from the other side, and a caller passing a stub or an empty slice is
// exactly how it arrives.
func TestProviderExpandRefusesAShortPseudorandomKey(t *testing.T) {
	for _, suite := range Suites() {
		crypto := mustProvider(t, suite)
		full := crypto.Extract([]byte("salt"), []byte("ikm"))
		if len(full) != crypto.HashSize() {
			t.Fatalf("suite %#04x Extract produced %d bytes for a HashSize of %d", uint16(suite), len(full), crypto.HashSize())
		}
		for _, short := range [][]byte{nil, {}, full[:1], full[:16], full[:crypto.HashSize()-1]} {
			recovered := recoveredPanic(func() { crypto.Expand(short, []byte("info"), 32) })
			if recovered == nil {
				t.Errorf("suite %#04x Expand under a %d byte pseudorandom key returned instead of refusing",
					uint16(suite), len(short))
				continue
			}
			if got := fmt.Sprint(recovered); !strings.HasPrefix(got, "mls: hkdf expand pseudorandom key ") {
				t.Errorf("suite %#04x Expand under a %d byte pseudorandom key was refused by %q, not by this package's gate",
					uint16(suite), len(short), got)
			}
		}
		// the control: a key of exactly the hash length, and one longer, are both served,
		// or the table above is satisfied by an Expand that refuses everything
		for _, served := range [][]byte{full, append(bytes.Clone(full), full...)} {
			if recovered := recoveredPanic(func() { crypto.Expand(served, []byte("info"), 32) }); recovered != nil {
				t.Errorf("suite %#04x Expand under a %d byte pseudorandom key was refused with %v",
					uint16(suite), len(served), recovered)
			}
		}
	}
}

// The boundary, at the library's own ceiling rather than at a constant of this package's
// choosing. hpkeMaxExpandLength is 255*Nh and TestHpkeExpandCeilingIsTheLibrarysOwnBoundary
// already pins it against crypto/hkdf itself, so what is left here is that this wrapper
// refuses the same lengths and serves the same ones — a gate one off in either direction
// either kills the process on a length the library serves or serves one it does not.
func TestProviderExpandRefusesUnrepresentableLengths(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	prk := crypto.Extract([]byte("salt"), []byte("ikm"))
	for _, length := range []int{0, 1, 32, hpkeMaxExpandLength - 1, hpkeMaxExpandLength} {
		if got := len(crypto.Expand(prk, []byte("info"), length)); got != length {
			t.Errorf("Expand asked for %d bytes returned %d", length, got)
		}
	}
	for _, length := range []int{-1, -32, hpkeMaxExpandLength + 1, 1 << 16} {
		recovered := recoveredPanic(func() { crypto.Expand(prk, []byte("info"), length) })
		if recovered == nil {
			t.Errorf("Expand returned for a length of %d instead of refusing it", length)
			continue
		}
		// and the refusal is this package's gate rather than a makeslice from inside
		// the kdf, which is what a negative length reaches when the gate is not there
		if got := fmt.Sprint(recovered); !strings.HasPrefix(got, "mls: hkdf expand length ") {
			t.Errorf("Expand at a length of %d was refused by %q, not by this package's gate", length, got)
		}
	}
}

// The lengths the ciphersuite fixes, refused for both suites and in both directions. The
// key gate is the one that has to read the registry: 16 bytes is a whole key under 0x0001
// and a short one under 0x0003, so a gate written against a literal passes one suite.
func TestProviderAeadRejectsWrongSizes(t *testing.T) {
	for _, suite := range Suites() {
		crypto := mustProvider(t, suite)
		key := make([]byte, crypto.KeySize())
		nonce := make([]byte, crypto.NonceSize())
		for _, wrong := range [][]byte{nil, {}, key[:len(key)-1], append(bytes.Clone(key), 0x00), make([]byte, 16), make([]byte, 32)} {
			if len(wrong) == crypto.KeySize() {
				continue
			}
			if _, err := crypto.AeadSeal(wrong, nonce, nil, nil); !errors.Is(err, ErrBadKeyLength) {
				t.Errorf("suite %#04x seal under a %d byte key error = %v, want ErrBadKeyLength", uint16(suite), len(wrong), err)
			}
			if _, err := crypto.AeadOpen(wrong, nonce, nil, make([]byte, 16)); !errors.Is(err, ErrBadKeyLength) {
				t.Errorf("suite %#04x open under a %d byte key error = %v, want ErrBadKeyLength", uint16(suite), len(wrong), err)
			}
		}
		for _, wrong := range [][]byte{nil, {}, nonce[:len(nonce)-1], append(bytes.Clone(nonce), 0x00), make([]byte, 8), make([]byte, 24)} {
			if _, err := crypto.AeadSeal(key, wrong, nil, nil); !errors.Is(err, ErrBadNonceLength) {
				t.Errorf("suite %#04x seal under a %d byte nonce error = %v, want ErrBadNonceLength", uint16(suite), len(wrong), err)
			}
			if _, err := crypto.AeadOpen(key, wrong, nil, make([]byte, 16)); !errors.Is(err, ErrBadNonceLength) {
				t.Errorf("suite %#04x open under a %d byte nonce error = %v, want ErrBadNonceLength", uint16(suite), len(wrong), err)
			}
		}
		ciphertext, err := crypto.AeadSeal(key, nonce, []byte("aad"), []byte("plaintext"))
		if err != nil {
			t.Fatalf("suite %#04x seal: %v", uint16(suite), err)
		}
		if _, err := crypto.AeadOpen(key, nonce, []byte("other aad"), ciphertext); !errors.Is(err, ErrAeadOpen) {
			t.Errorf("suite %#04x open under the wrong aad error = %v, want ErrAeadOpen", uint16(suite), err)
		}
		back, err := crypto.AeadOpen(key, nonce, []byte("aad"), ciphertext)
		if err != nil {
			t.Fatalf("suite %#04x open: %v", uint16(suite), err)
		}
		if !bytes.Equal(back, []byte("plaintext")) {
			t.Fatalf("suite %#04x open returned %q", uint16(suite), back)
		}
	}
}

// The aead against the vendored RFC 9180 corpus, through the provider rather than through
// the hpke context. Two things are pinned that suite.go alone cannot pin: the published
// key is exactly KeySize bytes and the published nonce exactly NonceSize, so a size that
// disagreed with the ciphersuite is refused by AeadSeal's own length gate rather than
// merely disagreeing with a table this repository wrote for itself.
func TestProviderAeadMatchesThePublishedEncryptions(t *testing.T) {
	sealed := 0
	for _, vector := range loadHpkeVectors(t) {
		crypto := mustProvider(t, vector.suite)
		key := decodeVectorField(t, vector.name, "key", vector.Key)
		if len(key) != crypto.KeySize() {
			t.Errorf("%s: the published key is %d bytes and KeySize is %d", vector.name, len(key), crypto.KeySize())
		}
		for _, encryption := range vector.Encryptions {
			nonce := decodeVectorField(t, vector.name, "nonce", encryption.Nonce)
			aad := decodeVectorField(t, vector.name, "aad", encryption.Aad)
			plaintext := decodeVectorField(t, vector.name, "pt", encryption.Pt)
			want := decodeVectorField(t, vector.name, "ct", encryption.Ct)
			if len(nonce) != crypto.NonceSize() {
				t.Fatalf("%s: the published nonce is %d bytes and NonceSize is %d", vector.name, len(nonce), crypto.NonceSize())
			}
			ciphertext, err := crypto.AeadSeal(key, nonce, aad, plaintext)
			if err != nil {
				t.Fatalf("%s: seal at sequence %d: %v", vector.name, encryption.sequence, err)
			}
			if !bytes.Equal(ciphertext, want) {
				t.Errorf("%s: ciphertext at sequence %d = %x, want %x", vector.name, encryption.sequence, ciphertext, want)
			}
			back, err := crypto.AeadOpen(key, nonce, aad, want)
			if err != nil {
				t.Fatalf("%s: open at sequence %d: %v", vector.name, encryption.sequence, err)
			}
			if !bytes.Equal(back, plaintext) {
				t.Errorf("%s: open at sequence %d returned %x, want %x", vector.name, encryption.sequence, back, plaintext)
			}
			sealed++
		}
	}
	if sealed == 0 {
		t.Fatal("no published encryption was sealed, so this known answer asserted nothing")
	}
	t.Logf("%d published encryptions sealed and opened through the provider", sealed)
}

// The empty plaintext, which is the input that makes a successful open and a refusal look
// alike. When the plaintext is empty the ciphertext is exactly a tag and the plaintext
// that comes back is a nil slice, so the only thing separating an authentic empty message
// from a forged one is the error — and an implementation that swallowed it would return
// the same nil, nil for both. That is the total authentication bypass p2 task 10 shipped
// behind a test that carried this very input and then never altered it, so here every
// single bit of the tag is altered.
func TestProviderAeadOpenRefusesAForgedEmptyMessage(t *testing.T) {
	for _, suite := range Suites() {
		crypto := mustProvider(t, suite)
		params, err := LookupSuite(suite)
		if err != nil {
			t.Fatalf("LookupSuite(%#04x): %v", uint16(suite), err)
		}
		key := bytes.Repeat([]byte{0x11}, crypto.KeySize())
		nonce := bytes.Repeat([]byte{0x22}, crypto.NonceSize())
		aad := []byte("aad")
		ciphertext, err := crypto.AeadSeal(key, nonce, aad, nil)
		if err != nil {
			t.Fatalf("suite %#04x seal of an empty plaintext: %v", uint16(suite), err)
		}
		if len(ciphertext) != params.Nt {
			t.Fatalf("suite %#04x sealed an empty plaintext into %d bytes, want a bare %d byte tag", uint16(suite), len(ciphertext), params.Nt)
		}
		back, err := crypto.AeadOpen(key, nonce, aad, ciphertext)
		if err != nil {
			t.Fatalf("suite %#04x open of an authentic empty message: %v", uint16(suite), err)
		}
		if len(back) != 0 {
			t.Fatalf("suite %#04x open of an empty message returned %x", uint16(suite), back)
		}
		refused := 0
		for i := range ciphertext {
			for bit := 0; bit < 8; bit++ {
				forged := bytes.Clone(ciphertext)
				forged[i] ^= 1 << bit
				plaintext, err := crypto.AeadOpen(key, nonce, aad, forged)
				if !errors.Is(err, ErrAeadOpen) {
					t.Errorf("suite %#04x open of a tag with bit %d of byte %d flipped = %v, want ErrAeadOpen", uint16(suite), bit, i, err)
					continue
				}
				if plaintext != nil {
					t.Errorf("suite %#04x open returned %d bytes alongside %v", uint16(suite), len(plaintext), err)
					continue
				}
				refused++
			}
		}
		if want := 8 * len(ciphertext); refused != want {
			t.Errorf("suite %#04x refused %d of %d forged empty messages", uint16(suite), refused, want)
		}
		for _, forged := range [][]byte{nil, {}, make([]byte, params.Nt-1), make([]byte, params.Nt), make([]byte, params.Nt+1), bytes.Repeat([]byte{0xff}, params.Nt)} {
			if plaintext, err := crypto.AeadOpen(key, nonce, aad, forged); !errors.Is(err, ErrAeadOpen) || plaintext != nil {
				t.Errorf("suite %#04x open of a %d byte forgery = %x, %v, want nil and ErrAeadOpen", uint16(suite), len(forged), plaintext, err)
			}
		}
		for _, wrongAad := range [][]byte{nil, {}, []byte("aa"), []byte("aad!"), []byte("AAD")} {
			if _, err := crypto.AeadOpen(key, nonce, wrongAad, ciphertext); !errors.Is(err, ErrAeadOpen) {
				t.Errorf("suite %#04x open of an empty message under the aad %q = %v, want ErrAeadOpen", uint16(suite), wrongAad, err)
			}
		}
		otherKey := bytes.Clone(key)
		otherKey[0] ^= 0x01
		if _, err := crypto.AeadOpen(otherKey, nonce, aad, ciphertext); !errors.Is(err, ErrAeadOpen) {
			t.Errorf("suite %#04x open of an empty message under another key = %v, want ErrAeadOpen", uint16(suite), err)
		}
		otherNonce := bytes.Clone(nonce)
		otherNonce[0] ^= 0x01
		if _, err := crypto.AeadOpen(key, otherNonce, aad, ciphertext); !errors.Is(err, ErrAeadOpen) {
			t.Errorf("suite %#04x open of an empty message under another nonce = %v, want ErrAeadOpen", uint16(suite), err)
		}
	}
}

// Nothing the provider returns may share memory with what it was handed, and nothing it
// was handed may come back changed. The aead is where this bites: cipher.AEAD documents
// that its destination may overlap its input exactly, so sealing into plaintext[:0] and
// opening into ciphertext[:0] both compile and both round trip while destroying a
// caller's buffer — and a caller that reuses a plaintext across two recipients then
// encrypts rubbish to the second.
func TestProviderResultsDoNotShareMemoryWithTheirInputs(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	spare := func(b []byte) []byte { return append(make([]byte, 0, len(b)+64), b...) }

	// both at a length nothing here shares with a result and at exactly HashSize, where
	// the natural mistake is to hand the caller's own buffer back
	for _, data := range [][]byte{
		spare([]byte("the message")),
		spare(crypto.Hash([]byte("a message of exactly the digest length"))),
	} {
		dataBefore := bytes.Clone(data)
		digest := crypto.Hash(data)
		tag := crypto.Mac(spare([]byte("key")), data)
		prk := crypto.Extract(spare([]byte("salt")), data)
		okm := crypto.Expand(prk, data, 48)
		if !bytes.Equal(data, dataBefore) {
			t.Errorf("a primitive changed its %d byte input from %x to %x", len(data), dataBefore, data)
		}
		for _, result := range [][]byte{digest, tag, prk, okm} {
			if len(result) != 0 && &result[0] == &data[0] {
				t.Errorf("a primitive returned a slice aliasing its %d byte input", len(data))
			}
		}
	}

	key := spare(bytes.Repeat([]byte{0x33}, crypto.KeySize()))
	nonce := spare(bytes.Repeat([]byte{0x44}, crypto.NonceSize()))
	aad := spare([]byte("aad"))
	plaintext := spare([]byte("plaintext that is long enough to notice"))
	keyBefore, nonceBefore := bytes.Clone(key), bytes.Clone(nonce)
	aadBefore, plaintextBefore := bytes.Clone(aad), bytes.Clone(plaintext)
	ciphertext, err := crypto.AeadSeal(key, nonce, aad, plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if !bytes.Equal(plaintext, plaintextBefore) {
		t.Errorf("seal changed its plaintext from %x to %x", plaintextBefore, plaintext)
	}
	if !bytes.Equal(aad, aadBefore) || !bytes.Equal(key, keyBefore) || !bytes.Equal(nonce, nonceBefore) {
		t.Errorf("seal changed one of its key, nonce or aad")
	}
	for i := range ciphertext {
		ciphertext[i] ^= 0xff
	}
	if !bytes.Equal(plaintext, plaintextBefore) {
		t.Errorf("the sealed ciphertext shares memory with the plaintext")
	}
	for i := range ciphertext {
		ciphertext[i] ^= 0xff
	}
	ciphertextBefore := bytes.Clone(ciphertext)
	back, err := crypto.AeadOpen(key, nonce, aad, ciphertext)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(ciphertext, ciphertextBefore) {
		t.Errorf("open changed its ciphertext from %x to %x", ciphertextBefore, ciphertext)
	}
	if len(back) != 0 && &back[0] == &ciphertext[0] {
		t.Errorf("open returned a plaintext aliasing the ciphertext")
	}
	assertEveryProviderCallLeavesItsArgumentsAlone(t, crypto)
}

// The same claim over every method of the interface that takes bytes, mechanically.
//
// Two things separate this from the hand written half above. It covers the whole surface
// and says so, so a method added later is not silently left out. And every argument is
// cut from a longer array and compared over the whole array afterwards: a provider that
// appends into a caller's spare capacity to save an allocation leaves len alone, so a
// comparison that stops at len reports nothing while the caller's next read past len
// comes back changed.
func assertEveryProviderCallLeavesItsArgumentsAlone(t *testing.T, crypto CryptoProvider) {
	t.Helper()
	secret := bytes.Repeat([]byte{0x61}, crypto.HashSize())
	message := []byte("a message the provider is handed rather than one it makes")
	tag := crypto.Mac(secret, message)
	key := bytes.Repeat([]byte{0x62}, crypto.KeySize())
	nonce := bytes.Repeat([]byte{0x63}, crypto.NonceSize())
	aad := []byte("aad")
	sealed, err := crypto.AeadSeal(key, nonce, aad, message)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	signaturePriv, signaturePub, err := crypto.SignatureKeyPair()
	if err != nil {
		t.Fatalf("generate the key pair the signature rows are built over: %v", err)
	}
	signature, err := crypto.SignWithLabel(signaturePriv, "label", message)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	hpkePriv, hpkePub, err := crypto.DeriveKeyPair(secret)
	if err != nil {
		t.Fatalf("derive the key pair the hpke rows are built over: %v", err)
	}
	info := []byte("the info the hpke rows carry")
	sealedKemOutput, sealedCiphertext, err := crypto.HpkeSeal(hpkePub, info, aad, message)
	if err != nil {
		t.Fatalf("seal the message the HpkeOpen row reads: %v", err)
	}
	covered := []string{}
	// the results are a list rather than one slice, because a method answering with two of
	// them has two chances to hand back a window onto a caller array, and a row following
	// only the first would not see the second
	for _, testCase := range []struct {
		name string
		call func(take func(content []byte) []byte) [][]byte
	}{
		{name: "Hash", call: func(take func([]byte) []byte) [][]byte {
			return [][]byte{crypto.Hash(take(message))}
		}},
		{name: "Mac", call: func(take func([]byte) []byte) [][]byte {
			return [][]byte{crypto.Mac(take(secret), take(message))}
		}},
		{name: "MacVerify", call: func(take func([]byte) []byte) [][]byte {
			if !crypto.MacVerify(take(secret), take(message), take(tag)) {
				t.Errorf("MacVerify refused a tag it had just produced, so this ran the refusal path")
			}
			return nil
		}},
		{name: "Extract", call: func(take func([]byte) []byte) [][]byte {
			return [][]byte{crypto.Extract(take(secret), take(message))}
		}},
		{name: "Expand", call: func(take func([]byte) []byte) [][]byte {
			return [][]byte{crypto.Expand(take(secret), take(message), 48)}
		}},
		{name: "ExpandWithLabel", call: func(take func([]byte) []byte) [][]byte {
			return [][]byte{crypto.ExpandWithLabel(take(secret), "label", take(message), 48)}
		}},
		{name: "DeriveSecret", call: func(take func([]byte) []byte) [][]byte {
			return [][]byte{crypto.DeriveSecret(take(secret), "label")}
		}},
		{name: "DeriveTreeSecret", call: func(take func([]byte) []byte) [][]byte {
			return [][]byte{crypto.DeriveTreeSecret(take(secret), "label", 7, 48)}
		}},
		{name: "AeadSeal", call: func(take func([]byte) []byte) [][]byte {
			ciphertext, err := crypto.AeadSeal(take(key), take(nonce), take(aad), take(message))
			if err != nil {
				t.Fatalf("seal: %v", err)
			}
			return [][]byte{ciphertext}
		}},
		{name: "AeadOpen", call: func(take func([]byte) []byte) [][]byte {
			plaintext, err := crypto.AeadOpen(take(key), take(nonce), take(aad), take(sealed))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			return [][]byte{plaintext}
		}},
		{name: "SignWithLabel", call: func(take func([]byte) []byte) [][]byte {
			produced, err := crypto.SignWithLabel(SignaturePrivateKey(take(signaturePriv)), "label", take(message))
			if err != nil {
				t.Fatalf("sign: %v", err)
			}
			return [][]byte{produced}
		}},
		{name: "VerifyWithLabel", call: func(take func([]byte) []byte) [][]byte {
			if err := crypto.VerifyWithLabel(SignaturePublicKey(take(signaturePub)), "label",
				take(message), take(signature)); err != nil {
				t.Errorf("VerifyWithLabel refused a signature it had just made, so this ran the refusal path")
			}
			return nil
		}},
		{name: "DeriveKeyPair", call: func(take func([]byte) []byte) [][]byte {
			priv, pub, err := crypto.DeriveKeyPair(take(secret))
			if err != nil {
				t.Fatalf("DeriveKeyPair: %v", err)
			}
			return [][]byte{priv, pub}
		}},
		{name: "HpkeSeal", call: func(take func([]byte) []byte) [][]byte {
			kemOutput, ciphertext, err := crypto.HpkeSeal(HpkePublicKey(take(hpkePub)),
				take(info), take(aad), take(message))
			if err != nil {
				t.Fatalf("HpkeSeal: %v", err)
			}
			return [][]byte{kemOutput, ciphertext}
		}},
		{name: "HpkeOpen", call: func(take func([]byte) []byte) [][]byte {
			plaintext, err := crypto.HpkeOpen(HpkePrivateKey(take(hpkePriv)), take(sealedKemOutput),
				take(info), take(aad), take(sealedCiphertext))
			if err != nil {
				t.Fatalf("HpkeOpen: %v", err)
			}
			return [][]byte{plaintext}
		}},
	} {
		covered = append(covered, testCase.name)
		recorder := &argumentRecorder{}
		results := testCase.call(recorder.take)
		if len(recorder.arrays) == 0 {
			t.Errorf("%s was handed nothing, so this row observed nothing", testCase.name)
			continue
		}
		if changed := recorder.changed(); len(changed) != 0 {
			t.Errorf("%s changed the storage behind arguments %v of the %d it was handed",
				testCase.name, changed, len(recorder.arrays))
		}
		for i, result := range results {
			if recorder.aliases(result) {
				t.Errorf("%s returned result %d over one of its arguments", testCase.name, i)
			}
		}
	}
	assertCoversTheProviderSurface(t, "the argument immutability table", covered, map[string]string{
		"Random":           "is handed no bytes, only a count",
		"SignatureKeyPair": "is handed no bytes, only its provider's own reader",
	})
}

// The arguments one provider call was handed, and the storage behind each of them.
//
// Every argument is cut from an array longer than itself, so len(argument) is strictly
// less than cap(argument) and an append into the spare capacity is a write this can see.
// Recording the array rather than the slice is the whole point: the caller's slice header
// never moves, and the bytes past its length are the ones a defect writes into.
type argumentRecorder struct {
	arrays [][]byte
	before [][]byte
}

// One argument, over storage this recorder keeps a copy of. The trailing bytes are a
// pattern rather than zeros: a mutant that appended a zero into the spare capacity would
// be invisible against an array that was already zero there.
func (self *argumentRecorder) take(content []byte) []byte {
	array := append(bytes.Clone(content), bytes.Repeat([]byte{0x5c, 0xa3}, 32)...)
	self.arrays = append(self.arrays, array)
	self.before = append(self.before, bytes.Clone(array))
	return array[:len(content)]
}

// The indexes of the arguments whose storage came back changed.
func (self *argumentRecorder) changed() []int {
	changed := []int{}
	for i, array := range self.arrays {
		if !bytes.Equal(array, self.before[i]) {
			changed = append(changed, i)
		}
	}
	return changed
}

// Whether a result is cut from the storage behind any argument. Comparing the result's
// first element against every byte of every argument rather than against the first byte
// of each catches a result taken from the middle of a caller's buffer as well as one
// taken from the front.
func (self *argumentRecorder) aliases(result []byte) bool {
	if len(result) == 0 {
		return false
	}
	for _, array := range self.arrays {
		for i := range array {
			if &array[i] == &result[0] {
				return true
			}
		}
	}
	return false
}

// The bytes the scripted reader hands out. Ninety six of them, in windows a mutant has to
// tell apart, so a Random that restarted at the beginning is as visible as one that drew
// more than it returned.
const randomScriptHex = "3f7a01d92c58be04ff1e6b83a7409dcc5210748fe6b3915da0c2ef47368b1da9" +
	"0e94c3f6725ab8d1490fe72c86b35a0d1fc6e849b27d305aef16942c7b8a5d30" +
	"b6027fd4e95c1a83407bcf29d81e63a5f0349d7c2be851a6c4907f3d5eb218ca"

func randomScript(t *testing.T) []byte {
	t.Helper()
	return mustDecodeHex(t, "the random script", randomScriptHex)
}

// The lengths drawn from the script, in order, and the one place they are written. The
// adequacy test and the consumption test both read this, so the windows proved adequate
// are exactly the windows drawn: a list written out twice drifts, and a probe proved over
// bytes nothing asks for proves nothing.
var randomDrawLengths = []int{32, 16, 1, 47}

// The stretches of the script the draws above land in, plus the whole of it. A draw of one
// byte contributes no window: sorting, reversing and rotating a single byte are all the
// identity, so there is nothing to prove about it and claiming otherwise would fail.
func randomScriptWindows(t *testing.T) [][]byte {
	t.Helper()
	script := randomScript(t)
	windows := [][]byte{script}
	taken := 0
	for _, length := range randomDrawLengths {
		if length > 1 {
			windows = append(windows, script[taken:taken+length])
		}
		taken += length
	}
	if taken != len(script) {
		t.Fatalf("the draws cover %d bytes of a %d byte script", taken, len(script))
	}
	return windows
}

// The probe has to be able to see the mutants it is aimed at, and an ascending or
// palindromic script cannot: sorting a script that is already sorted is the identity, and
// so is reversing one that reads the same backwards. Task 4's key generator passed a
// reversed, a rotated and a sorted scalar because its test fed a constant reader, where
// all three are the identity — so the adequacy of this script is asserted before anything
// is asserted with it.
func TestRandomScriptCanSeeAWeakenedReader(t *testing.T) {
	script := randomScript(t)
	if len(script) != 96 {
		t.Fatalf("the script is %d bytes, want 96", len(script))
	}
	windows := randomScriptWindows(t)
	for i, window := range windows {
		assertProbeIsNotItsOwnPermutation(t, fmt.Sprintf("the window %d of the script", i), window)
	}
	for i := 0; i < len(windows); i++ {
		for j := i + 1; j < len(windows); j++ {
			if len(windows[i]) == len(windows[j]) && bytes.Equal(windows[i], windows[j]) {
				t.Errorf("two windows of the script are equal, so a reader that restarted would be invisible")
			}
		}
	}
}

// Random must hand back the bytes it was offered, in order, and consume exactly as many
// as it returns. Each of those three is a separate mutant — a reader that is ignored, a
// reader whose bytes are permuted, and a reader that is over drawn so the next call starts
// in the wrong place — and none of them is visible to a test that only asks whether the
// result is non zero and changes between calls.
func TestProviderRandomConsumesItsReaderInOrder(t *testing.T) {
	script := randomScript(t)
	crypto := mustProviderOver(t, CipherSuiteX25519ChaCha20Sha256Ed25519, bytes.NewReader(script))
	taken := 0
	for _, length := range randomDrawLengths {
		got := crypto.Random(length)
		want := script[taken : taken+length]
		if !bytes.Equal(got, want) {
			t.Fatalf("Random(%d) after %d bytes = %x, want %x", length, taken, got, want)
		}
		taken += length
	}
	if taken != len(script) {
		t.Fatalf("the script is %d bytes and the calls asked for %d", len(script), taken)
	}
	// and the reader is empty now, which is what says nothing beyond each request was
	// drawn from it along the way
	if recovered := recoveredPanic(func() { crypto.Random(1) }); recovered == nil {
		t.Fatalf("Random returned a byte the script did not hold")
	}
}

// A reader that hands back one byte at a time, and stalls a few times first. Both are
// legal io.Reader behaviour, and a bare Read would leave every byte after the first as the
// zero it was allocated with — the silently weak value this whole file is about.
type dribblingReader struct {
	remaining []byte
	stalls    int
}

func (self *dribblingReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if self.stalls > 0 {
		self.stalls--
		return 0, nil
	}
	if len(self.remaining) == 0 {
		return 0, io.EOF
	}
	p[0] = self.remaining[0]
	self.remaining = self.remaining[1:]
	return 1, nil
}

func TestProviderRandomAssemblesAPartialReader(t *testing.T) {
	script := randomScript(t)
	reader := &dribblingReader{remaining: bytes.Clone(script), stalls: 3}
	crypto := mustProviderOver(t, CipherSuiteX25519ChaCha20Sha256Ed25519, reader)
	if got := crypto.Random(len(script)); !bytes.Equal(got, script) {
		t.Fatalf("Random over a one byte at a time reader = %x, want %x", got, script)
	}
}

// A short or failing source must not yield short or zero key material. Random has no
// error return in the interface spec A section 3.3 fixes, so the only refusal available
// is a panic, and it is asserted rather than assumed — the alternative is a caller
// deriving a key from a tail of zeroes and reporting success.
func TestProviderRandomRefusesAShortOrFailingReader(t *testing.T) {
	script := randomScript(t)
	for _, testCase := range []struct {
		name   string
		reader io.Reader
	}{
		{name: "an exhausted reader", reader: bytes.NewReader(nil)},
		{name: "a reader one byte short", reader: bytes.NewReader(script[:31])},
		{name: "a reader that fails at once", reader: failingReader{err: errors.New("entropy source is down")}},
		{name: "a reader that stalls then ends", reader: &dribblingReader{remaining: bytes.Clone(script[:7]), stalls: 2}},
	} {
		crypto := mustProviderOver(t, CipherSuiteX25519ChaCha20Sha256Ed25519, testCase.reader)
		if recovered := recoveredPanic(func() { crypto.Random(32) }); recovered == nil {
			t.Errorf("Random over %s returned instead of refusing", testCase.name)
		}
	}
	// the control: a reader with enough bytes must not be refused, or the table above is
	// satisfied by a Random that refuses everything
	crypto := mustProviderOver(t, CipherSuiteX25519ChaCha20Sha256Ed25519, bytes.NewReader(script))
	if recovered := recoveredPanic(func() { crypto.Random(32) }); recovered != nil {
		t.Errorf("Random over a sufficient reader was refused with %v", recovered)
	}
}

func TestProviderRandomLengths(t *testing.T) {
	script := randomScript(t)
	crypto := mustProviderOver(t, CipherSuiteX25519ChaCha20Sha256Ed25519, bytes.NewReader(script))
	// a request for nothing draws nothing, so the next request still starts at the top
	if got := crypto.Random(0); len(got) != 0 {
		t.Errorf("Random(0) returned %x", got)
	}
	if got := crypto.Random(4); !bytes.Equal(got, script[:4]) {
		t.Errorf("after Random(0) the next four bytes were %x, want %x", got, script[:4])
	}
	if recovered := recoveredPanic(func() { crypto.Random(-1) }); recovered == nil {
		t.Errorf("Random(-1) returned instead of refusing")
	}
}

func TestProviderRandomIsNotConstant(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	first := crypto.Random(32)
	if len(first) != 32 {
		t.Fatalf("Random(32) returned %d bytes", len(first))
	}
	if bytes.Equal(first, make([]byte, 32)) {
		t.Fatalf("Random returned all zeroes")
	}
	if bytes.Equal(first, crypto.Random(32)) {
		t.Fatalf("Random repeated itself")
	}
}

// Two default providers must not repeat each other. This is named for what it can see
// rather than for the property it is near: a stream that varies is all a caller outside
// the process can observe, and a sha256 counter, a linear congruential generator and a
// process wide seed all vary. The claim that the varying stream is the operating system's
// is not observable from here at all — no black box test separates a cryptographic source
// from a seeded one, which is the whole point of a seeded one — so it is held by reading
// the constructor instead, in TestNewCryptoProviderReadsTheProcessEntropySource below.
// What this catches is the degenerate half: a constant or zero stream.
func TestTwoDefaultProvidersDoNotRepeatEachOther(t *testing.T) {
	first := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	second := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	if bytes.Equal(first.Random(32), second.Random(32)) {
		t.Fatalf("two default providers produced the same random bytes")
	}
}

// Exactly what NewCryptoProvider is allowed to be. One statement, naming crypto/rand's
// own reader and constructing nothing: a wrapper around it, a seeded expansion of it or
// any other reader is a different statement and fails.
var newCryptoProviderStatements = []string{
	"return NewCryptoProviderWithRandom(suite, rand.Reader)",
}

// The import that supplies it, asserted separately so a file that no longer reads the
// operating system at all fails on the import as well as on the statement.
const cryptoRandImportPath = `"crypto/rand"`

// A constructor over a stream that varies and is entirely predictable. Deleting
// crypto/rand from crypto.go and substituting this passes every behavioural test in this
// package, because two providers over a counter still disagree with each other — so this
// is the body every matcher below is proved against.
const newCryptoProviderDeterministicControl = `package mls

import (
	"crypto/sha256"
)

func NewCryptoProvider(suite CipherSuite) (CryptoProvider, error) {
	return NewCryptoProviderWithRandom(suite, deterministicDefault{})
}

type deterministicDefault struct{}

var deterministicCounter uint64

func (deterministicDefault) Read(p []byte) (int, error) {
	deterministicCounter++
	block := sha256.Sum256([]byte{byte(deterministicCounter)})
	return copy(p, block[:]), nil
}
`

// The source of every key this package will ever generate, held by reading crypto.go.
//
// This is the one claim in this file that no input can distinguish from its violation.
// A provider whose default source is a counter expanded through sha256 answers every
// behavioural question exactly as the real one does: its output is never constant, never
// zero, never repeated between two providers, and never repeated between two calls. It is
// also entirely predictable, and every key, nonce and signature seed the implementation
// produces would be recoverable from it. So the assertion is the constructor's own text,
// mechanically, and the matcher is proved on a control that is precisely that substitution
// rather than on a body that fails for some easier reason.
func TestNewCryptoProviderReadsTheProcessEntropySource(t *testing.T) {
	source := mustParseSource(t, macVerifySourcePath)
	got := source.statementsOf(t, "", "NewCryptoProvider")
	if !slices.Equal(got, newCryptoProviderStatements) {
		t.Errorf("NewCryptoProvider is\n%s\nwant\n%s",
			strings.Join(got, "\n"), strings.Join(newCryptoProviderStatements, "\n"))
	}
	if !slices.Contains(importPathsOf(source), cryptoRandImportPath) {
		t.Errorf("%s does not import %s, so nothing in it reads the operating system",
			macVerifySourcePath, cryptoRandImportPath)
	}
	control := mustParseText(t, "the deterministic control", newCryptoProviderDeterministicControl)
	if substituted := control.statementsOf(t, "", "NewCryptoProvider"); slices.Equal(substituted, newCryptoProviderStatements) {
		t.Errorf("the matcher read a constructor over a counter stream as the shape above")
	}
	if slices.Contains(importPathsOf(control), cryptoRandImportPath) {
		t.Errorf("the control claims to import %s", cryptoRandImportPath)
	}
}

// The import paths of one parsed file, quoted as they are written.
func importPathsOf(source parsedSource) []string {
	paths := []string{}
	for _, imported := range source.file.Imports {
		paths = append(paths, imported.Path.Value)
	}
	return paths
}

// Nothing is substituted for the source a caller passed. A constructor that quietly
// replaced a nil reader with crypto/rand would make a test believing it had pinned the
// randomness draw from production entropy instead, and the failing case it was written to
// reproduce would stop reproducing.
//
// The refusal is at construction rather than at the first draw, and this test used to be
// the reason it had to be. It asked whether Random over a nil reader refused, Random did,
// and the test passed for a year over a provider whose HpkeSeal and EncryptWithLabel
// reached the process source through X25519GenerateKey — one operation named out of the
// four that draw. What holds the class now is
// TestEveryEntropyTakingFunctionRefusesANilSource, which reads it off the parse tree; what
// this holds is the boundary, where a provider with no source cannot be built at all.
func TestNewCryptoProviderWithRandomSubstitutesNothing(t *testing.T) {
	for _, suite := range Suites() {
		crypto, err := NewCryptoProviderWithRandom(suite, nil)
		if !errors.Is(err, ErrNilRandomSource) {
			t.Errorf("NewCryptoProviderWithRandom(%#04x, nil) error = %v, want ErrNilRandomSource", uint16(suite), err)
		}
		if crypto != nil {
			t.Errorf("NewCryptoProviderWithRandom(%#04x, nil) returned a provider alongside %v", uint16(suite), err)
		}
		// and the control: a reader with bytes in it is not refused, or the rows above are
		// satisfied by a constructor that refuses everything
		if _, err := NewCryptoProviderWithRandom(suite, bytes.NewReader(randomScript(t))); err != nil {
			t.Errorf("NewCryptoProviderWithRandom(%#04x) refused a reader holding bytes: %v", uint16(suite), err)
		}
	}
}

// The fields the one implementation is allowed to hold, and what makes sharing it safe.
// A field added later fails here on purpose: whether the new one is written after
// construction is the question this whole test exists to ask, and answering it is a line
// of thought rather than a line of code.
var providerFields = []string{"params *mls.SuiteParams", "random io.Reader"}

// The files that declare a method on the one implementation.
//
// Every gate below scans all of them rather than one named file. That is the shape of a
// defect this package already paid for: task 12 moved three methods into a second file,
// four whole provider invariants went on reading the first, and the identical receiver
// write failed or passed depending only on which file it was written in. A later task
// adding a fourth file fails here rather than quietly leaving its methods unscanned.
var providerMethodSourcePaths = []string{"crypto.go", "crypto_labels.go"}

// Every go file of this package, test files included. A method on the shared provider
// type is the thing being ruled out whichever file it is written in, and a file the scan
// does not read is a file the rule does not reach.
func packageSourcePaths(t *testing.T) []string {
	t.Helper()
	return packageSourcePathsIn(t, ".")
}

// Every method the interface names, read off the type rather than typed out.
//
// This is what the whole provider invariants below measure their own coverage against. A
// hand written list of methods is the thing that goes stale when the surface grows, and
// an invariant enumerating a surface that has moved on reports exactly what a complete
// one reports.
func providerMethodNames() []string {
	names := []string{}
	providerInterface := reflect.TypeOf((*CryptoProvider)(nil)).Elem()
	for i := 0; i < providerInterface.NumMethod(); i++ {
		names = append(names, providerInterface.Method(i).Name)
	}
	slices.Sort(names)
	return names
}

// The methods that answer with a size or a code point. They take no bytes and return no
// storage, so there is nothing for a buffer, aliasing or immutability invariant to look
// at; TestProviderSizes is what holds them.
var providerValueMethods = []string{"HashSize", "KeySize", "NonceSize", "Suite"}

// The methods that refuse to be called rather than answering, which since task 15 is none
// of them.
//
// The list stays, empty, because it is what assertCoversTheProviderSurface reads to decide
// what a whole provider invariant may skip. A later task that adds a method it cannot
// implement in the same commit writes the name here on purpose, and taking it off again is
// what makes every invariant below demand a row for it — so a method cannot become live and
// uncovered in the same commit. The refusal itself no longer has a subject: task 15
// implemented the last three, and the table that held them to panicking would now be a loop
// over nothing.
//
// Since task 16 it is also read the other way round. TestProviderHasNoRemainingStubs fails
// while this holds any name at all, because a provider with a method it refuses to answer
// is the thing that test is named after -- so writing a name here to let a whole provider
// invariant skip a method is a decision that shows up as a failing completeness gate rather
// than as a quiet exemption.
var providerStubMethods = []string{}

// One whole provider invariant's table, checked against the interface it is meant to
// cover. What is excused is named with its reason rather than left out, so a method that
// stops being excusable is a line somebody has to delete on purpose.
func assertCoversTheProviderSurface(t *testing.T, gate string, covered []string, excused map[string]string) {
	t.Helper()
	surface := providerMethodNames()
	want := []string{}
	for _, name := range surface {
		if slices.Contains(providerValueMethods, name) || slices.Contains(providerStubMethods, name) {
			continue
		}
		if _, isExcused := excused[name]; !isExcused {
			want = append(want, name)
		}
	}
	got := slices.Clone(covered)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("%s covers %v, want %v", gate, got, want)
	}
	for name := range excused {
		if !slices.Contains(surface, name) {
			t.Errorf("%s excuses %s, which the provider does not declare", gate, name)
		}
	}
}

// The lists above name methods this type really has. A misspelling in any of them would
// quietly shrink what the coverage gate demands, which is the same failure as a short
// table written out by hand.
func TestTheProviderSurfaceListsNameRealMethods(t *testing.T) {
	surface := providerMethodNames()
	for _, name := range slices.Concat(providerValueMethods, providerStubMethods) {
		if !slices.Contains(surface, name) {
			t.Errorf("%s is listed as part of the provider surface but is not a method of it", name)
		}
	}
	if len(surface) == 0 {
		t.Fatalf("the provider interface reported no methods, so every gate below excuses everything")
	}
}

// A provider that keeps a call count. Its methods are the real ones with one assignment
// added, which is the smallest version of the defect and the one a matcher aimed at
// something louder would miss.
const providerStatefulControl = `package mls

type suiteCryptoProvider struct {
	params *SuiteParams
	random io.Reader
	calls  int
}

func (self *suiteCryptoProvider) Hash(data []byte) []byte {
	self.calls++
	digest := sha256.Sum256(data)
	return digest[:]
}
`

// Spec A section 3.6, as a structure rather than as a hope. The concurrency test below
// cannot see a data race in this environment — -race needs cgo and there is no C compiler
// on this machine — so a provider carrying two shared fields written by every Hash call
// runs thirty two goroutines over them and reports nothing. What holds the claim instead
// is that the type has exactly the two fields it is documented to have, and that no method
// writes to either: an immutable value shared between goroutines cannot race, whether or
// not the detector is available to say so.
//
// The scan is over every file of the package rather than over one named file, and the
// files that turn out to declare a method are pinned. Reading a single file made this
// gate blind to a whole second half of the implementation, and the blindness was
// invisible: the gate found its subject, found no write, and passed.
func TestTheProviderHoldsNoMutableState(t *testing.T) {
	fields := []string{}
	providerType := reflect.TypeOf(suiteCryptoProvider{})
	for i := 0; i < providerType.NumField(); i++ {
		field := providerType.Field(i)
		fields = append(fields, field.Name+" "+field.Type.String())
	}
	if !slices.Equal(fields, providerFields) {
		t.Errorf("suiteCryptoProvider holds %v, want %v — a new field has to be shown never written",
			fields, providerFields)
	}
	declaring := []string{}
	declared := []string{}
	for _, path := range packageSourcePaths(t) {
		source := mustParseSource(t, path)
		methods := source.methodsOn(providerReceiver)
		if len(methods) == 0 {
			continue
		}
		declaring = append(declaring, path)
		declared = append(declared, methods...)
		if writes := source.receiverFieldWrites(providerReceiver); len(writes) != 0 {
			t.Errorf("a method of %s in %s writes to its receiver, so the provider is not safe to share: %v",
				providerReceiver, path, writes)
		}
	}
	if !slices.Equal(declaring, providerMethodSourcePaths) {
		t.Errorf("%s is implemented across %v, want %v — a file holding provider methods has to be scanned here",
			providerReceiver, declaring, providerMethodSourcePaths)
	}
	// and what those files hold really is the whole implementation, so a scan cannot be
	// complete over a set that has lost half of it
	slices.Sort(declared)
	if missing := missingFrom(providerMethodNames(), declared); len(missing) != 0 {
		t.Errorf("%v are methods of the provider that no scanned file declares", missing)
	}
	control := mustParseText(t, "the stateful control", providerStatefulControl)
	if writes := control.receiverFieldWrites(providerReceiver); len(writes) == 0 {
		t.Errorf("the matcher reported no receiver write in a method whose first statement is one")
	}
	if methods := control.methodsOn(providerReceiver); !slices.Equal(methods, []string{"Hash"}) {
		t.Errorf("the matcher read %v out of a control declaring one method", methods)
	}
}

// The members of want that got does not hold, so a failure names what is absent rather
// than printing two lists for a reader to difference by eye.
func missingFrom(want []string, got []string) []string {
	absent := []string{}
	for _, name := range want {
		if !slices.Contains(got, name) {
			absent = append(absent, name)
		}
	}
	return absent
}

// Every call returns storage of its own. A provider that answered out of one cached buffer
// would pass every equality test in this file — each answer is right when it is read — and
// would hand two goroutines the same array to write, and hand one caller a digest that
// changes under it when the next call is made. The aliasing test above compares a result
// against its input; this compares two results against each other, which is the direction
// a cache is invisible in.
//
// The table covers every method of the interface that answers with bytes, and the gate at
// the end says so: a later task that adds a method and not a row here fails rather than
// leaving the new one uncovered.
func TestProviderResultsAreFreshBuffers(t *testing.T) {
	// the script is repeated far past what the rows below consume. every draw here comes
	// out of it, and a reader that ran dry mid table would fail as a short read rather
	// than as the shared storage this test is about
	crypto := mustProviderOver(t, CipherSuiteX25519ChaCha20Sha256Ed25519,
		bytes.NewReader(bytes.Repeat(randomScript(t), 16)))
	prk := crypto.Extract([]byte("salt"), []byte("ikm"))
	key := bytes.Repeat([]byte{0x33}, crypto.KeySize())
	nonce := bytes.Repeat([]byte{0x44}, crypto.NonceSize())
	aad := []byte("aad")
	sealed, err := crypto.AeadSeal(key, nonce, aad, []byte("plaintext"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	signaturePriv, _, err := crypto.SignatureKeyPair()
	if err != nil {
		t.Fatalf("generate the key pair the signature row is built over: %v", err)
	}
	hpkePriv, hpkePub, err := crypto.DeriveKeyPair(bytes.Repeat([]byte{0x55}, 32))
	if err != nil {
		t.Fatalf("derive the key pair the hpke rows are built over: %v", err)
	}
	sealedKemOutput, sealedCiphertext, err := crypto.HpkeSeal(hpkePub, []byte("info"), aad, []byte("plaintext"))
	if err != nil {
		t.Fatalf("seal the message the HpkeOpen row reads: %v", err)
	}
	covered := []string{}
	for _, testCase := range []struct {
		name string
		call func() []byte
	}{
		{name: "Hash", call: func() []byte { return crypto.Hash([]byte("data")) }},
		{name: "Mac", call: func() []byte { return crypto.Mac([]byte("key"), []byte("data")) }},
		{name: "Extract", call: func() []byte { return crypto.Extract([]byte("salt"), []byte("ikm")) }},
		{name: "Expand", call: func() []byte { return crypto.Expand(prk, []byte("info"), 48) }},
		{name: "ExpandWithLabel", call: func() []byte {
			return crypto.ExpandWithLabel(prk, "label", []byte("context"), 48)
		}},
		{name: "DeriveSecret", call: func() []byte { return crypto.DeriveSecret(prk, "label") }},
		{name: "DeriveTreeSecret", call: func() []byte { return crypto.DeriveTreeSecret(prk, "label", 7, 48) }},
		{name: "AeadSeal", call: func() []byte {
			ciphertext, err := crypto.AeadSeal(key, nonce, aad, []byte("plaintext"))
			if err != nil {
				t.Fatalf("seal: %v", err)
			}
			return ciphertext
		}},
		{name: "AeadOpen", call: func() []byte {
			plaintext, err := crypto.AeadOpen(key, nonce, aad, sealed)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			return plaintext
		}},
		{name: "SignWithLabel", call: func() []byte {
			signature, err := crypto.SignWithLabel(signaturePriv, "label", []byte("content"))
			if err != nil {
				t.Fatalf("sign: %v", err)
			}
			return signature
		}},
		{name: "SignatureKeyPair", call: func() []byte {
			priv, _, err := crypto.SignatureKeyPair()
			if err != nil {
				t.Fatalf("SignatureKeyPair: %v", err)
			}
			return priv
		}},
		{name: "Random", call: func() []byte { return crypto.Random(32) }},
		{name: "DeriveKeyPair", call: func() []byte {
			priv, _, err := crypto.DeriveKeyPair(bytes.Repeat([]byte{0x55}, 32))
			if err != nil {
				t.Fatalf("DeriveKeyPair: %v", err)
			}
			return priv
		}},
		{name: "HpkeSeal", call: func() []byte {
			_, ciphertext, err := crypto.HpkeSeal(hpkePub, []byte("info"), aad, []byte("plaintext"))
			if err != nil {
				t.Fatalf("HpkeSeal: %v", err)
			}
			return ciphertext
		}},
		{name: "HpkeOpen", call: func() []byte {
			plaintext, err := crypto.HpkeOpen(hpkePriv, sealedKemOutput, []byte("info"), aad, sealedCiphertext)
			if err != nil {
				t.Fatalf("HpkeOpen: %v", err)
			}
			return plaintext
		}},
	} {
		covered = append(covered, testCase.name)
		first, second := testCase.call(), testCase.call()
		if len(first) == 0 || len(second) == 0 {
			t.Errorf("%s returned nothing, so this shares nothing either", testCase.name)
			continue
		}
		if &first[0] == &second[0] {
			t.Errorf("two calls to %s returned the same storage", testCase.name)
			continue
		}
		// and holding the first result across the second call does not change it, which
		// is the failure a caller actually sees
		held := bytes.Clone(first)
		testCase.call()
		if !bytes.Equal(first, held) {
			t.Errorf("a result of %s changed from %x to %x when %s was called again",
				testCase.name, held, first, testCase.name)
		}
	}
	// the row above follows the private half, because a row can only follow one slice.
	// the public half is checked here rather than left out: a generator answering out of
	// storage it keeps would hand two callers the same public key as readily as the same
	// seed, and neither is visible in any signature either of them makes.
	firstPriv, firstPub, err := crypto.SignatureKeyPair()
	if err != nil {
		t.Fatalf("SignatureKeyPair: %v", err)
	}
	secondPriv, secondPub, err := crypto.SignatureKeyPair()
	if err != nil {
		t.Fatalf("SignatureKeyPair a second time: %v", err)
	}
	if len(firstPub) == 0 || len(secondPub) == 0 {
		t.Fatalf("SignatureKeyPair answered with no public key, so this shares nothing either")
	}
	if &firstPub[0] == &secondPub[0] {
		t.Errorf("two key pairs answered out of the same public key storage")
	}
	if &firstPub[0] == &firstPriv[0] || &secondPub[0] == &secondPriv[0] {
		t.Errorf("a key pair answered with a public key cut from its own private key")
	}
	// the two rows above follow one slice each for the same reason, so the halves they
	// cannot follow are checked here: a derived public key cut from its own private key,
	// and an encapsulated key answered out of storage the next seal writes over
	firstDerivedPriv, firstDerivedPub, err := crypto.DeriveKeyPair(bytes.Repeat([]byte{0x55}, 32))
	if err != nil {
		t.Fatalf("DeriveKeyPair: %v", err)
	}
	secondDerivedPriv, secondDerivedPub, err := crypto.DeriveKeyPair(bytes.Repeat([]byte{0x55}, 32))
	if err != nil {
		t.Fatalf("DeriveKeyPair a second time: %v", err)
	}
	if len(firstDerivedPub) == 0 || len(secondDerivedPub) == 0 {
		t.Fatalf("DeriveKeyPair answered with no public key, so this shares nothing either")
	}
	if &firstDerivedPub[0] == &secondDerivedPub[0] || &firstDerivedPriv[0] == &secondDerivedPriv[0] {
		t.Errorf("two derived key pairs answered out of the same storage")
	}
	if &firstDerivedPub[0] == &firstDerivedPriv[0] {
		t.Errorf("a derived key pair answered with a public key cut from its own private key")
	}
	firstSealKemOutput, _, err := crypto.HpkeSeal(hpkePub, []byte("info"), aad, []byte("plaintext"))
	if err != nil {
		t.Fatalf("HpkeSeal: %v", err)
	}
	secondSealKemOutput, _, err := crypto.HpkeSeal(hpkePub, []byte("info"), aad, []byte("plaintext"))
	if err != nil {
		t.Fatalf("HpkeSeal a second time: %v", err)
	}
	if len(firstSealKemOutput) == 0 || len(secondSealKemOutput) == 0 {
		t.Fatalf("HpkeSeal answered with no encapsulated key, so this shares nothing either")
	}
	if &firstSealKemOutput[0] == &secondSealKemOutput[0] {
		t.Errorf("two seals answered out of the same encapsulated key storage")
	}
	assertCoversTheProviderSurface(t, "the fresh buffer table", covered, map[string]string{
		"MacVerify":       "answers with a bool, so there is no storage for it to share",
		"VerifyWithLabel": "answers with an error, so there is no storage for it to share",
	})
}

// Two providers over the same byte stream produce the same bytes, which is what makes a
// failing interop or validation case reproducible rather than observed once.
func TestProviderWithRandomIsDeterministic(t *testing.T) {
	script := randomScript(t)
	first := mustProviderOver(t, CipherSuiteX25519ChaCha20Sha256Ed25519, bytes.NewReader(script))
	second := mustProviderOver(t, CipherSuiteX25519ChaCha20Sha256Ed25519, bytes.NewReader(script))
	if !bytes.Equal(first.Random(32), second.Random(32)) {
		t.Fatalf("two providers over the same stream disagreed")
	}
}

// Spec A section 3.6: the provider NewCryptoProvider returns is stateless and safe for
// concurrent use. Under -race this is the whole assertion; without it the race detector is
// absent and this exercises the methods concurrently without being able to report a race,
// so TestTheProviderHoldsNoMutableState is what carries the claim on a machine with no c
// compiler. Both are here because they fail on different things: this one on a method that
// disagrees with itself, that one on the shared field a disagreement would come from.
//
// Every method that answers deterministically is compared against an answer taken before
// the goroutines start, rather than merely called. That comparison is what a machine with
// no detector has: a provider answering out of one package level scratch array hands two
// goroutines the same storage, and what the caller sees is the other goroutine's bytes.
func TestProviderIsSafeForConcurrentUse(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	key := bytes.Repeat([]byte{0x0f}, crypto.KeySize())
	secret := bytes.Repeat([]byte{0x0f}, crypto.HashSize())
	nonce := make([]byte, crypto.NonceSize())
	message := []byte("data")
	tag := crypto.Mac(secret, message)
	sealed, err := crypto.AeadSeal(key, nonce, nil, message)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	signaturePriv, signaturePub, err := crypto.SignatureKeyPair()
	if err != nil {
		t.Fatalf("generate the key pair the signature rows are built over: %v", err)
	}
	signature, err := crypto.SignWithLabel(signaturePriv, "label", message)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	hpkePriv, hpkePub, err := crypto.DeriveKeyPair(secret)
	if err != nil {
		t.Fatalf("derive the key pair the hpke rows are built over: %v", err)
	}
	sealedKemOutput, sealedCiphertext, err := crypto.HpkeSeal(hpkePub, []byte("info"), nil, message)
	if err != nil {
		t.Fatalf("seal the message the HpkeOpen row reads: %v", err)
	}
	operations := []struct {
		name string
		call func() []byte
	}{
		{name: "Hash", call: func() []byte { return crypto.Hash(message) }},
		{name: "Mac", call: func() []byte { return crypto.Mac(secret, message) }},
		{name: "MacVerify", call: func() []byte {
			if crypto.MacVerify(secret, message, tag) {
				return []byte{0x01}
			}
			return []byte{0x00}
		}},
		{name: "Extract", call: func() []byte { return crypto.Extract(secret, message) }},
		{name: "Expand", call: func() []byte { return crypto.Expand(secret, []byte("info"), 32) }},
		{name: "ExpandWithLabel", call: func() []byte {
			return crypto.ExpandWithLabel(secret, "label", []byte("context"), 48)
		}},
		{name: "DeriveSecret", call: func() []byte { return crypto.DeriveSecret(secret, "label") }},
		{name: "DeriveTreeSecret", call: func() []byte { return crypto.DeriveTreeSecret(secret, "label", 7, 48) }},
		{name: "AeadSeal", call: func() []byte {
			ciphertext, err := crypto.AeadSeal(key, nonce, nil, message)
			if err != nil {
				return nil
			}
			return ciphertext
		}},
		{name: "AeadOpen", call: func() []byte {
			plaintext, err := crypto.AeadOpen(key, nonce, nil, sealed)
			if err != nil {
				return nil
			}
			return plaintext
		}},
		{name: "SignWithLabel", call: func() []byte {
			produced, err := crypto.SignWithLabel(signaturePriv, "label", message)
			if err != nil {
				return nil
			}
			return produced
		}},
		{name: "VerifyWithLabel", call: func() []byte {
			if crypto.VerifyWithLabel(signaturePub, "label", message, signature) == nil {
				return []byte{0x01}
			}
			return []byte{0x00}
		}},
		{name: "DeriveKeyPair", call: func() []byte {
			priv, pub, err := crypto.DeriveKeyPair(secret)
			if err != nil {
				return nil
			}
			return concatBytes(priv, pub)
		}},
		{name: "HpkeOpen", call: func() []byte {
			plaintext, err := crypto.HpkeOpen(hpkePriv, sealedKemOutput, []byte("info"), nil, sealedCiphertext)
			if err != nil {
				return nil
			}
			return plaintext
		}},
	}
	covered := []string{}
	want := make([][]byte, len(operations))
	for i, operation := range operations {
		covered = append(covered, operation.name)
		want[i] = operation.call()
		if len(want[i]) == 0 {
			t.Fatalf("%s answered with nothing before any goroutine started", operation.name)
		}
	}
	assertCoversTheProviderSurface(t, "the concurrency table", append(covered, "Random", "SignatureKeyPair", "HpkeSeal"), nil)

	var waitGroup sync.WaitGroup
	for i := 0; i < 32; i++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for j := 0; j < 64; j++ {
				for k, operation := range operations {
					if !bytes.Equal(operation.call(), want[k]) {
						t.Errorf("%s disagreed with itself under concurrency", operation.name)
						return
					}
				}
				crypto.Random(32)
				// the three draws from the provider reader, which answer differently
				// every time and so are exercised rather than compared
				if _, _, err := crypto.SignatureKeyPair(); err != nil {
					t.Errorf("SignatureKeyPair failed under concurrency: %v", err)
					return
				}
				if _, _, err := crypto.HpkeSeal(hpkePub, []byte("info"), nil, message); err != nil {
					t.Errorf("HpkeSeal failed under concurrency: %v", err)
					return
				}
			}
		}()
	}
	waitGroup.Wait()
}

// A provider that answers what the real one answers with every byte flipped, and records
// what it was asked.
//
// This exists for the constructions that take a CryptoProvider as a parameter rather than
// as a receiver. Nothing about their output says which provider computed it: one that
// reached for crypto/sha256 directly, or built a provider of its own out of a hardcoded
// suite, agrees with every published corpus and every known answer in this package,
// because the suite in those corpora is the suite it hardcoded. A provider that is not the
// real one is the only input that tells the two apart — a construction that routed through
// the parameter answers differently over this one, and a construction that ignored the
// parameter answers exactly the same.
//
// The methods are written out rather than promoted from an embedded CryptoProvider on
// purpose. A promoted method is one the compiler supplies, so a method added to the
// interface would arrive here already implemented, tagging nothing and quietly narrowing
// what the gates that use this can see — which is the shape task 12 paid for. Written out,
// the build stops until somebody decides what the new method does here.
type taggingCryptoProvider struct {
	inner CryptoProvider
	calls []string
}

// Drift between the interface and this wrapper fails at build rather than at the gate that
// reads it, which is the whole reason the methods are written out.
var _ CryptoProvider = (*taggingCryptoProvider)(nil)

// One answer, recorded and flipped. Cloning rather than flipping in place matters: what
// the whole provider invariants hold is that the real provider hands out storage nobody
// else writes into, and a wrapper that flipped the array it was handed would break that
// from the test side.
func (self *taggingCryptoProvider) tagged(name string, answer []byte) []byte {
	self.calls = append(self.calls, name)
	flipped := bytes.Clone(answer)
	for i := range flipped {
		flipped[i] ^= 0x5a
	}
	return flipped
}

// One call whose answer carries no bytes to flip, recorded and passed through. A
// construction that reached only for a size still shows up in the call log, so a gate can
// say whether the provider was touched at all separately from whether its answer was used.
func (self *taggingCryptoProvider) passedThrough(name string) {
	self.calls = append(self.calls, name)
}

func (self *taggingCryptoProvider) Suite() CipherSuite {
	self.passedThrough("Suite")
	return self.inner.Suite()
}

func (self *taggingCryptoProvider) HashSize() int {
	self.passedThrough("HashSize")
	return self.inner.HashSize()
}

func (self *taggingCryptoProvider) KeySize() int {
	self.passedThrough("KeySize")
	return self.inner.KeySize()
}

func (self *taggingCryptoProvider) NonceSize() int {
	self.passedThrough("NonceSize")
	return self.inner.NonceSize()
}

func (self *taggingCryptoProvider) Hash(data []byte) []byte {
	return self.tagged("Hash", self.inner.Hash(data))
}

func (self *taggingCryptoProvider) Mac(key []byte, data []byte) []byte {
	return self.tagged("Mac", self.inner.Mac(key, data))
}

func (self *taggingCryptoProvider) MacVerify(key []byte, data []byte, tag []byte) bool {
	self.passedThrough("MacVerify")
	return self.inner.MacVerify(key, data, tag)
}

func (self *taggingCryptoProvider) Extract(salt []byte, ikm []byte) []byte {
	return self.tagged("Extract", self.inner.Extract(salt, ikm))
}

func (self *taggingCryptoProvider) Expand(prk []byte, info []byte, length int) []byte {
	return self.tagged("Expand", self.inner.Expand(prk, info, length))
}

func (self *taggingCryptoProvider) ExpandWithLabel(secret []byte, label string, context []byte, length int) []byte {
	return self.tagged("ExpandWithLabel", self.inner.ExpandWithLabel(secret, label, context, length))
}

func (self *taggingCryptoProvider) DeriveSecret(secret []byte, label string) []byte {
	return self.tagged("DeriveSecret", self.inner.DeriveSecret(secret, label))
}

func (self *taggingCryptoProvider) DeriveTreeSecret(secret []byte, label string, generation uint32, length int) []byte {
	return self.tagged("DeriveTreeSecret", self.inner.DeriveTreeSecret(secret, label, generation, length))
}

func (self *taggingCryptoProvider) AeadSeal(key []byte, nonce []byte, aad []byte, plaintext []byte) ([]byte, error) {
	ciphertext, err := self.inner.AeadSeal(key, nonce, aad, plaintext)
	if err != nil {
		self.passedThrough("AeadSeal")
		return nil, err
	}
	return self.tagged("AeadSeal", ciphertext), nil
}

func (self *taggingCryptoProvider) AeadOpen(key []byte, nonce []byte, aad []byte, ciphertext []byte) ([]byte, error) {
	plaintext, err := self.inner.AeadOpen(key, nonce, aad, ciphertext)
	if err != nil {
		self.passedThrough("AeadOpen")
		return nil, err
	}
	return self.tagged("AeadOpen", plaintext), nil
}

func (self *taggingCryptoProvider) Random(n int) []byte {
	return self.tagged("Random", self.inner.Random(n))
}

func (self *taggingCryptoProvider) SignWithLabel(priv SignaturePrivateKey, label string, content []byte) ([]byte, error) {
	signature, err := self.inner.SignWithLabel(priv, label, content)
	if err != nil {
		self.passedThrough("SignWithLabel")
		return nil, err
	}
	return self.tagged("SignWithLabel", signature), nil
}

func (self *taggingCryptoProvider) VerifyWithLabel(pub SignaturePublicKey, label string, content []byte, sig []byte) error {
	self.passedThrough("VerifyWithLabel")
	return self.inner.VerifyWithLabel(pub, label, content, sig)
}

// Both results are flipped, for the reason SignatureKeyPair's are: a construction reading
// either one of them answers differently over this provider. The encapsulated key is
// flipped as well as the ciphertext because EncryptWithLabel returns both and a gate
// following only one of them would not see a construction that used the other.
func (self *taggingCryptoProvider) HpkeSeal(pub HpkePublicKey, info []byte, aad []byte, plaintext []byte) ([]byte, []byte, error) {
	kemOutput, ciphertext, err := self.inner.HpkeSeal(pub, info, aad, plaintext)
	if err != nil {
		self.passedThrough("HpkeSeal")
		return nil, nil, err
	}
	return self.tagged("HpkeSeal", kemOutput), self.tagged("HpkeSeal", ciphertext), nil
}

func (self *taggingCryptoProvider) HpkeOpen(priv HpkePrivateKey, kemOutput []byte, info []byte, aad []byte, ciphertext []byte) ([]byte, error) {
	plaintext, err := self.inner.HpkeOpen(priv, kemOutput, info, aad, ciphertext)
	if err != nil {
		self.passedThrough("HpkeOpen")
		return nil, err
	}
	return self.tagged("HpkeOpen", plaintext), nil
}

func (self *taggingCryptoProvider) DeriveKeyPair(ikm []byte) (HpkePrivateKey, HpkePublicKey, error) {
	priv, pub, err := self.inner.DeriveKeyPair(ikm)
	if err != nil {
		self.passedThrough("DeriveKeyPair")
		return nil, nil, err
	}
	return HpkePrivateKey(self.tagged("DeriveKeyPair", priv)),
		HpkePublicKey(self.tagged("DeriveKeyPair", pub)), nil
}

// Both halves are flipped, so a construction reading either one of them answers
// differently over this provider. Two entries land in the call log for one call, which the
// log is fine with: it is read to explain a failure and never compared for length.
func (self *taggingCryptoProvider) SignatureKeyPair() (SignaturePrivateKey, SignaturePublicKey, error) {
	priv, pub, err := self.inner.SignatureKeyPair()
	if err != nil {
		self.passedThrough("SignatureKeyPair")
		return nil, nil, err
	}
	return SignaturePrivateKey(self.tagged("SignatureKeyPair", priv)),
		SignaturePublicKey(self.tagged("SignatureKeyPair", pub)), nil
}

// The methods the tagging provider hands back unchanged, named with the reason rather than
// left out. Neither of the two is a stub, and neither answers with bytes: there is nothing
// in a yes, a no or a refusal for a flip to change, so a construction that routed a
// comparison through either cannot be separated by a different answer. The stubs are skipped
// by assertCoversTheProviderSurface itself, so implementing one is what makes the gate
// below demand it be tagged.
var taggingProviderPassesThrough = map[string]string{
	"MacVerify":       "answers a bool, and flipping has nothing to change in a yes or a no",
	"VerifyWithLabel": "answers an error, and flipping has nothing to change in a refusal",
}

// The tagging provider really does answer differently, method by method.
//
// Without this, every gate that reads it is vacuous in the direction that matters. A
// tagged method that stopped tagging would make the constructions agree with the real
// provider, and the routing gate would report that none of them use the provider they were
// handed; drop the flip from only the one method a construction happens to call and that
// gate reports a clean run while observing nothing at all. Each row is one method's answer
// over the tagging provider against the same method's answer over the provider inside it.
//
// The provider underneath draws from a constant reader, because the Random row otherwise
// compares two draws that differ on their own whatever the wrapper does.
func TestTheTaggingProviderAnswersDifferentlyThanTheRealOne(t *testing.T) {
	plain := mustProviderOver(t, CipherSuiteX25519ChaCha20Sha256Ed25519, constantReader{value: 0x3d})
	tagging := &taggingCryptoProvider{inner: plain}
	secret := bytes.Repeat([]byte{0x4b}, plain.HashSize())
	key := bytes.Repeat([]byte{0x11}, plain.KeySize())
	nonce := bytes.Repeat([]byte{0x22}, plain.NonceSize())
	sealed, err := plain.AeadSeal(key, nonce, []byte("aad"), []byte("plaintext"))
	if err != nil {
		t.Fatalf("seal the ciphertext the AeadOpen row reads: %v", err)
	}
	// drawn from the provider underneath rather than written out, so the row below is
	// built over whatever length the suite fixes
	signaturePriv, _, err := plain.SignatureKeyPair()
	if err != nil {
		t.Fatalf("generate the key the SignWithLabel row is built over: %v", err)
	}
	hpkePriv, hpkePub, err := plain.DeriveKeyPair(secret)
	if err != nil {
		t.Fatalf("derive the key pair the hpke rows are built over: %v", err)
	}
	// the provider underneath draws from a constant reader, so the seal below is the same
	// encapsulation every time and the HpkeSeal row compares a tag rather than two
	// unrelated ephemeral keys
	sealedKemOutput, sealedCiphertext, err := plain.HpkeSeal(hpkePub, []byte("info"), []byte("aad"), []byte("plaintext"))
	if err != nil {
		t.Fatalf("seal the message the HpkeOpen row reads: %v", err)
	}
	tagged := []string{}
	for _, testCase := range []struct {
		name string
		call func(crypto CryptoProvider) []byte
	}{
		{name: "Hash", call: func(crypto CryptoProvider) []byte { return crypto.Hash([]byte("data")) }},
		{name: "Mac", call: func(crypto CryptoProvider) []byte { return crypto.Mac(key, []byte("data")) }},
		{name: "Extract", call: func(crypto CryptoProvider) []byte { return crypto.Extract(nil, secret) }},
		{name: "Expand", call: func(crypto CryptoProvider) []byte { return crypto.Expand(secret, []byte("info"), 32) }},
		{name: "ExpandWithLabel", call: func(crypto CryptoProvider) []byte {
			return crypto.ExpandWithLabel(secret, "label", []byte("context"), 32)
		}},
		{name: "DeriveSecret", call: func(crypto CryptoProvider) []byte { return crypto.DeriveSecret(secret, "label") }},
		{name: "DeriveTreeSecret", call: func(crypto CryptoProvider) []byte {
			return crypto.DeriveTreeSecret(secret, "label", 7, 32)
		}},
		{name: "AeadSeal", call: func(crypto CryptoProvider) []byte {
			out, sealErr := crypto.AeadSeal(key, nonce, []byte("aad"), []byte("plaintext"))
			if sealErr != nil {
				t.Fatalf("AeadSeal: %v", sealErr)
			}
			return out
		}},
		{name: "AeadOpen", call: func(crypto CryptoProvider) []byte {
			out, openErr := crypto.AeadOpen(key, nonce, []byte("aad"), sealed)
			if openErr != nil {
				t.Fatalf("AeadOpen: %v", openErr)
			}
			return out
		}},
		{name: "SignWithLabel", call: func(crypto CryptoProvider) []byte {
			signature, signErr := crypto.SignWithLabel(signaturePriv, "label", []byte("content"))
			if signErr != nil {
				t.Fatalf("SignWithLabel: %v", signErr)
			}
			return signature
		}},
		{name: "SignatureKeyPair", call: func(crypto CryptoProvider) []byte {
			priv, pub, keyErr := crypto.SignatureKeyPair()
			if keyErr != nil {
				t.Fatalf("SignatureKeyPair: %v", keyErr)
			}
			return concatBytes(priv, pub)
		}},
		{name: "Random", call: func(crypto CryptoProvider) []byte { return crypto.Random(32) }},
		{name: "DeriveKeyPair", call: func(crypto CryptoProvider) []byte {
			priv, pub, deriveErr := crypto.DeriveKeyPair(secret)
			if deriveErr != nil {
				t.Fatalf("DeriveKeyPair: %v", deriveErr)
			}
			return concatBytes(priv, pub)
		}},
		{name: "HpkeSeal", call: func(crypto CryptoProvider) []byte {
			kemOutput, ciphertext, sealErr := crypto.HpkeSeal(hpkePub, []byte("info"), []byte("aad"), []byte("plaintext"))
			if sealErr != nil {
				t.Fatalf("HpkeSeal: %v", sealErr)
			}
			return concatBytes(kemOutput, ciphertext)
		}},
		{name: "HpkeOpen", call: func(crypto CryptoProvider) []byte {
			plaintext, openErr := crypto.HpkeOpen(hpkePriv, sealedKemOutput, []byte("info"), []byte("aad"), sealedCiphertext)
			if openErr != nil {
				t.Fatalf("HpkeOpen: %v", openErr)
			}
			return plaintext
		}},
	} {
		tagged = append(tagged, testCase.name)
		answer := testCase.call(plain)
		flipped := testCase.call(tagging)
		if len(answer) == 0 {
			t.Errorf("%s answered no bytes, so this row separates nothing", testCase.name)
			continue
		}
		if bytes.Equal(answer, flipped) {
			t.Errorf("%s answers the same over the tagging provider as over the real one, so nothing routed through it is observable",
				testCase.name)
		}
		if len(answer) != len(flipped) {
			t.Errorf("%s answered %d bytes over the tagging provider and %d over the real one; a tag must not change the length",
				testCase.name, len(flipped), len(answer))
		}
	}
	// and the rows are every method the tagging provider claims to tag, so a method that
	// stops being a stub is a row somebody has to write rather than one whose answer goes
	// on being passed through
	assertCoversTheProviderSurface(t, "the tagging provider", tagged, taggingProviderPassesThrough)
}

// One function this package declares at package level, with the file that declares it and
// the parameter types it is written to take.
type packageLevelFunction struct {
	file       string
	name       string
	parameters []string
}

// What one scan of the package's source read: the functions, and the files they came out
// of.
//
// The files are carried because the functions alone cannot say what was missed. A scan
// that stopped reading a file returns a shorter list of functions, and a shorter list of
// functions still satisfies every gate whose table was never extended — which is the
// understatement this package has paid for repeatedly. The file list is what a gate can
// compare against the package's own source.
type packageLevelScan struct {
	files     []string
	functions []packageLevelFunction
}

// The package level functions of one parsed file.
//
// A declaration carrying a receiver is skipped, and that skip is the class boundary rather
// than an oversight: a construction is called by name and a method is called on a receiver,
// so the two need different tables. The other half of the partition is held in
// provider_methods_test.go, and TestEveryDeclarationTakingAProviderIsHeldByExactlyOneOfTheTwo-
// Classes compares both halves against the whole of what the type checker reads, so a
// declaration cannot fall between the two.
//
// Parameter types come back rendered and one per name, so a signature written
// func f(a, b []byte) reads the same here as one written func f(a []byte, b []byte). A
// gate that filters on a type is then filtering on what the compiler sees rather than on
// how the line happened to be spaced, and the grouped form is the one a filter over the
// file's characters would misread.
func packageLevelFunctionsIn(parsed parsedSource, path string) []packageLevelFunction {
	functions := []packageLevelFunction{}
	for _, declaration := range parsed.file.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Recv != nil {
			continue
		}
		parameters := []string{}
		for _, field := range function.Type.Params.List {
			rendered := parsed.render(field.Type)
			written := len(field.Names)
			if written == 0 {
				written = 1
			}
			for i := 0; i < written; i++ {
				parameters = append(parameters, rendered)
			}
		}
		functions = append(functions, packageLevelFunction{
			file:       path,
			name:       function.Name.Name,
			parameters: parameters,
		})
	}
	return functions
}

// Every package level function of the package's non test source.
//
// The scan is every file rather than a named one, which is the shape task 12 paid for: a
// gate that enumerates a filename goes on reporting a clean run while the thing it guards
// is written next door. Test files are skipped because what these gates are about is the
// surface the package ships, and a helper written for a table is not a construction
// anybody outside the tests calls.
//
// TestThePackageLevelFunctionScanReadsEveryNonTestFile writes that same rule out a second
// time and compares. The repetition is deliberate: a rule stated once here and read by
// every gate is a rule that can be narrowed in one place, and every gate reading it would
// narrow with it and report a clean run.
func packageLevelFunctions(t *testing.T) packageLevelScan {
	t.Helper()
	scan := packageLevelScan{files: []string{}, functions: []packageLevelFunction{}}
	for _, path := range packageSourcePaths(t) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		scan.files = append(scan.files, path)
		scan.functions = append(scan.functions, packageLevelFunctionsIn(mustParseSource(t, path), path)...)
	}
	if len(scan.functions) == 0 {
		t.Fatalf("the package's non test source declares no package level functions, so every gate reading this demands nothing")
	}
	return scan
}

// The names of the package level functions whose parameters include one of a given type.
//
// Absence is fatal rather than clean, for the same reason every other enumeration here is:
// a filter that stopped matching leaves the gate reading it demanding nothing, and a gate
// that demands nothing reports exactly what a complete one reports.
func packageLevelFunctionsTaking(t *testing.T, parameter string) []string {
	t.Helper()
	names := []string{}
	for _, function := range packageLevelFunctions(t).functions {
		if slices.Contains(function.parameters, parameter) {
			names = append(names, function.name)
		}
	}
	if len(names) == 0 {
		t.Fatalf("no package level function of this package takes a %s, so the gate reading this demands nothing", parameter)
	}
	slices.Sort(names)
	return names
}

// The scan reads the whole package rather than the part a gate's table already covers.
//
// A scan narrowed by one filename is invisible from the gates that read it: they compare
// their tables against a shorter list of declared functions, find it matches, and report
// the clean run they would report if nothing had been narrowed at all. What says otherwise
// is the package's own file list, written out here a second time rather than taken from
// the scan.
func TestThePackageLevelFunctionScanReadsEveryNonTestFile(t *testing.T) {
	scanned := packageLevelFunctions(t).files
	source := []string{}
	for _, path := range packageSourcePaths(t) {
		if !strings.HasSuffix(path, "_test.go") {
			source = append(source, path)
		}
	}
	if !slices.Equal(scanned, source) {
		t.Errorf("the package level function scan read %v, and this package's non test source is %v", scanned, source)
	}
}

// The named types of one parsed file whose underlying type is a byte slice.
//
// A construction taking an HpkePublicKey is handed a caller's array exactly as one taking
// a []byte is, and to the compiler the two are the same storage. A filter matching the
// spelling []byte alone would drop nine of this package's constructions out of the class
// below and report the clean run a complete filter reports.
func packageByteSliceTypeNamesIn(parsed parsedSource) []string {
	names := []string{}
	for _, declaration := range parsed.file.Decls {
		types, isTypeDeclaration := declaration.(*ast.GenDecl)
		if !isTypeDeclaration || types.Tok != token.TYPE {
			continue
		}
		for _, specification := range types.Specs {
			named, isNamed := specification.(*ast.TypeSpec)
			if !isNamed {
				continue
			}
			slice, isSlice := named.Type.(*ast.ArrayType)
			if !isSlice || slice.Len != nil || parsed.render(slice.Elt) != "byte" {
				continue
			}
			names = append(names, named.Name.Name)
		}
	}
	return names
}

// Every type of this package's non test source that is a byte slice under another name.
func packageByteSliceTypeNames(t *testing.T) []string {
	t.Helper()
	names := []string{}
	for _, path := range packageLevelFunctions(t).files {
		names = append(names, packageByteSliceTypeNamesIn(mustParseSource(t, path))...)
	}
	if len(names) == 0 {
		t.Fatalf("this package names no byte slice type, so the class below is filtered on a spelling")
	}
	slices.Sort(names)
	return names
}

// Every package level construction of this package that is handed a caller's bytes.
//
// This is the class the immutability gate below covers, read off the parse tree rather
// than typed out. A construction taking no byte slice cannot write into an array it was
// handed, so the filter is the property rather than a list somebody keeps in step.
func packageLevelFunctionsTakingCallerBytes(t *testing.T) []string {
	t.Helper()
	byteSlices := slices.Concat([]string{"[]byte"}, packageByteSliceTypeNames(t))
	names := []string{}
	for _, function := range packageLevelFunctions(t).functions {
		for _, parameter := range function.parameters {
			if slices.Contains(byteSlices, parameter) {
				names = append(names, function.name)
				break
			}
		}
	}
	if len(names) == 0 {
		t.Fatalf("no package level function of this package is handed bytes, so the gate below demands nothing")
	}
	slices.Sort(names)
	return names
}

// The constructions whose answer carries no bytes, named with what they answer instead.
//
// The aliasing, determinism and fresh storage halves of the gate below read a construction's
// byte results, and these have none to read. Naming them rather than letting a row quietly
// answer with nothing is what keeps the gate from passing over a row that stopped returning
// its result: a name that is not here must answer with bytes, and a name that is here must
// not.
var packageConstructionsAnsweringNoBytes = map[string]string{
	"hpkeNewAead":      "answers a cipher.AEAD over the key, and the key is what this reads",
	"hpkeKeySchedule":  "answers an *HpkeContext holding the derivations rather than the bytes",
	"HpkeSetupBaseR":   "answers an *HpkeContext, the recipient half having no wire output",
	"X25519PrivateKey": "answers an *ecdh.PrivateKey, which is the library's own storage",
	"X25519PublicKey":  "answers an *ecdh.PublicKey, which is the library's own storage",
	// framing's ValSem010, whose whole answer is whether the signature verified. The aliasing
	// and fresh storage halves have nothing to read; the "leaves its input alone" half still
	// runs, and it is the half that matters for a verifier, since every array it is handed --
	// the public key, the message and the group context -- is read again by the caller after
	// the call.
	"VerifyAuthenticatedContent": "answers an error and no bytes; what it produces is a verdict",
	// framing's ValSem007 and ValSem008, on the same terms and for the same reason.
	"verifyMembershipTag": "answers an error and no bytes; what it produces is a verdict",
	// section 6.2's open, on the same terms with one difference worth naming: it DOES answer a
	// structure, and every byte of that structure is the caller's own message carried through.
	// There is nothing here it produced, so the aliasing and fresh storage halves have nothing to
	// read -- and the "leaves its input alone" half still runs, which is the half that matters,
	// since the key, the message and the group context are all read again after the call.
	"OpenPublicMessage": "answers a verdict and a view over the message it was handed, and no bytes of its own",
	// section 6.3's open, on exactly the same terms: the AuthenticatedContent it answers is the
	// caller's header carried through plus a plaintext it decrypted, and the plaintext's arrays
	// are the decoder's own copies. The half that matters is the one that still runs -- the key
	// source, the secret, the message and the group context are all read again after the call.
	"OpenPrivateMessage": "answers a verdict and a view over the message it was handed, and no bytes of its own",
	// the body OpenPrivateMessage delegates to since MASTER section 8.4.2 v2, which adds one
	// uint32 to the answer and no bytes: the generation the frame was sealed at, read out of the
	// sender data the open already had to decrypt.
	"openPrivateMessageAt": "answers a verdict, a view over the message it was handed and a generation, and no bytes of its own",
	// section 6's ValSem002 and ValSem003, on VerifyAuthenticatedContent's terms exactly.
	"CheckFramedContentContext": "answers an error and no bytes; what it produces is a verdict",
	// RFC 9420 section 7.3's commit door, on VerifyAuthenticatedContent's terms and for its
	// reason: its whole answer is whether the leaf of a received UpdatePath is valid at the
	// sender's position. The half that still runs is the one that matters -- the group context
	// and the path are the caller's own and are read again after the call, and this door is
	// reached before anything in a commit has been trusted.
	"ValidateUpdatePathLeafNode": "answers an error and no bytes; what it produces is a verdict",
	// the group policy's ordering primitive and the equality derived from it. Both answer a
	// verdict about two member ids and produce no storage at all, so the aliasing and fresh
	// storage halves have nothing to read. The half that still runs is the one that matters for
	// a comparator: a sort that normalised a run before comparing it, or that wrote a sentinel
	// into one, would edit the role list of a policy the caller is about to encode into a group
	// context and cover with a transcript hash.
	"compareMemberIds": "answers -1, 0 or 1 and no bytes; what it produces is an ordering",
	"sameMemberId":     "answers a bool and no bytes; what it produces is a verdict",
	// the labelled constructions' length gate, on VerifyAuthenticatedContent's terms. It
	// answers whether a value fits in one length prefixed field and produces no storage at
	// all, so the aliasing and fresh storage halves have nothing to read. The half that
	// still runs is the one that matters and it matters more here than almost anywhere: this
	// runs on a peer's message BEFORE the signature over it is checked, so a gate that
	// normalised or truncated what it was measuring would be editing an unverified message
	// in place and handing the edited bytes to the verify.
	"checkLabelledConstruction": "answers an error and no bytes; what it produces is a verdict about a length",
}

// A construction handed a caller's bytes that this gate does not hold, named with the
// reason. The map exists so that a construction which cannot be held is a line somebody
// writes on purpose rather than one left out of the table, and the gate below already
// checks that every name in it is one this package really declares, so an entry cannot
// outlive the thing it excuses.
var packageConstructionsOverBorrowedBytes = map[string]string{
	// the one construction whose whole purpose is to write into the caller's array.
	// Holding it to "leaves its input alone" would be holding it to not doing its job.
	// It is not unheld: secret_zeroize.go's own tests pin it to clearing every byte of
	// the slice it was given, to being visible through a second header over the same
	// array, and to touching nothing past that slice's length even though the capacity
	// is reachable — which is this gate's "does not scribble on the caller" property,
	// asserted at the one boundary that still applies to an eraser.
	"zeroizeSecret": "erases a secret in place; writing into the caller's array is the function",
	// the key schedule's internal assembler. It retains three of the four arrays it is
	// handed — that is what building an epoch out of parts means — so the aliasing half
	// of this gate would be holding it to not doing its job. It is unexported and no
	// caller's bytes reach it: each of the three exported constructors copies or freshly
	// derives every secret before passing it here, which is exactly the property this
	// gate holds THEM to in the rows below. So the class is covered at the boundary
	// where a caller exists, and excused at the one line inside it where there is none.
	"newKeyScheduleFromParts": "assembles an epoch out of storage its three callers have already copied or derived; retaining it is the function",
}

// Every construction this package hands a caller's array leaves that array alone, answers
// the same thing twice and answers out of storage of its own.
//
// What these are handed is a group's own secret, a serialized key package, somebody's
// framed proposal, a peer's public key or a ciphertext off the wire, and every one of them
// is read again after the call. A construction that wrote into the array its argument was
// cut from would corrupt the object it was asked to name, and one that answered out of
// storage it keeps would hand two callers the same bytes. Neither is visible in any digest,
// any published vector or any round trip: the answer is right, and the caller's buffer is
// wrong afterwards.
//
// The arguments go through a recorder, so each is cut from a longer array whose spare
// capacity holds a pattern rather than zeros: a construction that appends a byte to save an
// allocation leaves len alone, and against a zeroed array it would leave the bytes past len
// looking untouched as well.
//
// The scope is the package and not one file. The gate this replaces enumerated
// crypto_labels.go, which is the defect task 12 paid for one layer over — the identical
// scribble into a caller's spare capacity failed in the file the gate named and passed in
// every other file of the package, and hpke.go's fourteen constructions over caller bytes
// were held by nothing at all. It lives here rather than beside the labelled constructions
// for the same reason: the class is the package's, so the gate is the package's.
//
// Determinism is checkable for the three constructions that draw an ephemeral key because
// they are handed the reader they draw from, and a constant reader hands them the same key
// twice.
func TestEveryConstructionInThisPackageLeavesItsInputAlone(t *testing.T) {
	crypto := mustProvider(t, CipherSuiteX25519ChaCha20Sha256Ed25519)
	params, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("look up the suite these rows are built over: %v", err)
	}
	value := bytes.Repeat([]byte{0x21}, 96)
	suiteId := hpkeSuiteId(params)
	kemSuiteId := hpkeKemSuiteId(params)
	ikm := bytes.Repeat([]byte{0x31}, params.Nsk)
	priv, pub, err := HpkeDeriveKeyPair(params, ikm)
	if err != nil {
		t.Fatalf("derive the key pair these rows are built over: %v", err)
	}
	sharedSecret, kemOutput, err := hpkeEncap(constantReader{value: 0x77}, params, pub)
	if err != nil {
		t.Fatalf("encapsulate the shared secret these rows are built over: %v", err)
	}
	info := []byte("the info every hpke row carries")
	aad := []byte("the aad every hpke row carries")
	plaintext := []byte("the plaintext every hpke row carries")
	sealedKemOutput, sealedCiphertext, err := HpkeSealBase(constantReader{value: 0x66}, params, pub, info, aad, plaintext)
	if err != nil {
		t.Fatalf("seal the ciphertext the open rows read: %v", err)
	}
	key := bytes.Repeat([]byte{0x44}, params.Nk)
	// the key schedule row's two secrets, at exactly KDF.Nh because DeriveJoinerSecret
	// refuses anything else, and different from each other so the row is not satisfied by
	// an implementation that reads one of them twice
	initSecretPrev := bytes.Repeat([]byte{0x71}, params.Nh)
	commitSecret := bytes.Repeat([]byte{0x72}, params.Nh)
	// and the three the epoch constructors take, at KDF.Nh for the same reason and all
	// distinct from each other and from the two above, so no row is satisfied by a
	// construction that read one of its arguments twice
	pskSecret := bytes.Repeat([]byte{0x73}, params.Nh)
	joinerSecret := bytes.Repeat([]byte{0x74}, params.Nh)
	epochSecret := bytes.Repeat([]byte{0x75}, params.Nh)
	// and the welcome secret, at KDF.Nh and distinct from all five above for the same
	// reason
	welcomeSecret := bytes.Repeat([]byte{0x76}, params.Nh)
	// the published key package p7 task 15's welcome row is addressed to, minted ONCE. Its
	// reference is one of the two answers that row compares across two calls, and a key package
	// minted per call carries fresh keys and a fresh wall clock lifetime, so its reference moves
	// for a reason that has nothing to do with the builder.
	welcomeRowKeyPackagePtr, _, _, err := NewKeyPackage(crypto, params.Suite,
		BasicCredential([]byte("the identity the welcome row is addressed to")),
		testCapabilities(), nil)
	if err != nil {
		t.Fatalf("mint the key package the welcome row is addressed to: %v", err)
	}
	welcomeRowKeyPackage := *welcomeRowKeyPackagePtr
	// a second provider over a constant reader, for the one construction here that
	// encapsulates. EncryptWithLabel draws its ephemeral key through the provider it is
	// handed, so over a fixed stream it answers the same twice and the determinism half of
	// this gate can see it; over the process entropy source it would answer differently
	// every call and the row would be a comparison of two unrelated messages.
	deterministic := mustProviderOver(t, CipherSuiteX25519ChaCha20Sha256Ed25519, constantReader{value: 0x55})
	labelledKemOutput, labelledCiphertext, err := EncryptWithLabel(deterministic, pub, "UpdatePathNode", value, plaintext)
	if err != nil {
		t.Fatalf("seal the message the DecryptWithLabel row reads: %v", err)
	}
	// p7 task 16's join, built ONCE for the reason the welcome row's key package is: this row is
	// called twice over two recorders and the two answers are compared, so a Welcome minted per
	// call would carry a fresh joiner secret and the epoch authenticator would move for a reason
	// that has nothing to do with the joiner. The join itself draws nothing -- every value it
	// answers is a function of the message, the tree and the key material it was handed -- which
	// is what makes the comparison a live one.
	joinGroup, joinRowJoiner, joinRowResult, joinRowKeys := joinTestCommit(t, crypto,
		"the group the input gate joins", nil)
	defer joinGroup.Close()
	if err := joinGroup.MergePendingCommit(); err != nil {
		t.Fatalf("merge the commit the join row's welcome came from: %v", err)
	}
	covered := []string{}
	for _, testCase := range []struct {
		name string
		call func(take func(content []byte) []byte) [][]byte
	}{
		{name: "mlsKdfLabel", call: func(take func([]byte) []byte) [][]byte {
			return [][]byte{mlsKdfLabel("label", take(value), 32)}
		}},
		{name: "mlsSignContent", call: func(take func([]byte) []byte) [][]byte {
			return [][]byte{mlsSignContent("label", take(value))}
		}},
		// ledger item 228's pairwise exporter context and the bounded door onto it. Both are
		// driven, and the pair is not one row twice: the encoder is where an argument could be
		// aliased into the answer, and the door is where a copy could be made and then handed back
		// over the caller's array anyway. Three byte arguments each, because all three of the group
		// id, the two public points and the epoch authenticator are the caller's storage and the
		// group goes on holding every one of them after the call.
		{name: "encodePairwiseContext", call: func(take func([]byte) []byte) [][]byte {
			encoded, err := encodePairwiseContext(take(value), 7, 1, 2,
				take(pub), take(pub), take(plaintext))
			if err != nil {
				t.Fatalf("encodePairwiseContext over a context that fits: %v", err)
			}
			return [][]byte{encoded}
		}},
		{name: "marshalPairwiseContext", call: func(take func([]byte) []byte) [][]byte {
			encoded, err := marshalPairwiseContext(take(value), 7, 1, 2,
				take(pub), take(pub), take(plaintext))
			if err != nil {
				t.Fatalf("marshalPairwiseContext over a context that fits: %v", err)
			}
			return [][]byte{encoded}
		}},
		{name: "checkLabelledConstruction", call: func(take func([]byte) []byte) [][]byte {
			if err := checkLabelledConstruction("probe", "label", take(value)); err != nil {
				t.Fatalf("checkLabelledConstruction over a value that fits: %v", err)
			}
			return nil
		}},
		{name: "mlsEncryptContext", call: func(take func([]byte) []byte) [][]byte {
			return [][]byte{mlsEncryptContext("label", take(value))}
		}},
		{name: "EncryptWithLabel", call: func(take func([]byte) []byte) [][]byte {
			kemOutput, ciphertext, encryptErr := EncryptWithLabel(deterministic,
				HpkePublicKey(take(pub)), "UpdatePathNode", take(value), take(plaintext))
			if encryptErr != nil {
				t.Fatalf("EncryptWithLabel: %v", encryptErr)
			}
			return [][]byte{kemOutput, ciphertext}
		}},
		{name: "DecryptWithLabel", call: func(take func([]byte) []byte) [][]byte {
			opened, decryptErr := DecryptWithLabel(crypto, HpkePrivateKey(take(priv)), "UpdatePathNode",
				take(value), take(labelledKemOutput), take(labelledCiphertext))
			if decryptErr != nil {
				t.Fatalf("DecryptWithLabel: %v", decryptErr)
			}
			return [][]byte{opened}
		}},
		// the HpkeCiphertext shaped forms of the pair above. The deterministic provider for
		// the seal, since this gate calls each row twice and requires one answer -- a seal
		// over the process entropy is a different ciphertext every call. The two fields of
		// the open's ciphertext go through the recorder individually, so a body that answered
		// a plaintext over the caller's ciphertext array is caught here rather than at the
		// moment that array is next written.
		{name: "SealWithLabel", call: func(take func([]byte) []byte) [][]byte {
			sealed, sealErr := SealWithLabel(deterministic, HpkePublicKey(take(pub)),
				"UpdatePathNode", take(value), take(plaintext))
			if sealErr != nil {
				t.Fatalf("SealWithLabel: %v", sealErr)
			}
			return [][]byte{sealed.KemOutput, sealed.Ciphertext}
		}},
		{name: "OpenWithLabel", call: func(take func([]byte) []byte) [][]byte {
			opened, openErr := OpenWithLabel(crypto, HpkePrivateKey(take(priv)), "UpdatePathNode",
				take(value), &HpkeCiphertext{
					KemOutput:  take(labelledKemOutput),
					Ciphertext: take(labelledCiphertext),
				})
			if openErr != nil {
				t.Fatalf("OpenWithLabel: %v", openErr)
			}
			return [][]byte{opened}
		}},
		{name: "RefHash", call: func(take func([]byte) []byte) [][]byte {
			return [][]byte{mustRefHash(t, crypto, "MLS 1.0 a label", take(value))}
		}},
		{name: "MakeKeyPackageRef", call: func(take func([]byte) []byte) [][]byte {
			return [][]byte{mustKeyPackageRef(t, crypto, take(value))}
		}},
		{name: "MakeProposalRef", call: func(take func([]byte) []byte) [][]byte {
			return [][]byte{mustProposalRef(t, crypto, take(value))}
		}},
		// the one codec entry in this class, and it is here for a reason beyond covering the
		// class: connect/message reads a device wrap target off every leaf through this call, so
		// a DeviceXwingPub that VIEWED the leaf bytes it was decoded out of would be a wrap
		// target whoever owns that buffer can change after the leaf carrying it was validated
		// and before a commit secret is wrapped to it -- with every signature over the leaf still
		// verifying, because the signature is over the bytes and not over what was read out of
		// them.
		{name: "ParseLeafKeysExtension", call: func(take func([]byte) []byte) [][]byte {
			ext, encodeErr := (&LeafKeysExtension{AlgId: AlgIdXwing, DeviceXwingPub: leafKeysTestKey()}).Encode()
			if encodeErr != nil {
				t.Fatalf("Encode a leaf keys body to parse: %v", encodeErr)
			}
			parsed, parseErr := ParseLeafKeysExtension(take(ext.ExtensionData))
			if parseErr != nil {
				t.Fatalf("ParseLeafKeysExtension: %v", parseErr)
			}
			return [][]byte{parsed.DeviceXwingPub}
		}},
		// the ratchet_tree extension body, in this class for the row above's reason one level
		// up. A whole tree of leaves is decoded here and the group derives its epoch over the
		// result, so a leaf key that VIEWED the buffer it arrived in is a key whoever owns that
		// buffer can change after the tree was validated and before anything is sealed to it --
		// with the tree hash still matching, because the hash is over the bytes and not over
		// what was read out of them. The raised limit does not change the question: it is the
		// same bytes, sixteen times as many of them.
		{name: "UnmarshalRatchetTree", call: func(take func([]byte) []byte) [][]byte {
			leaf, leafErr := NewLeafNode(crypto, SignaturePrivateKey(bytes.Repeat([]byte{0x54}, 32)),
				Credential{CredentialType: CredentialTypeBasic, Identity: []byte("alice")},
				HpkePublicKey(pub), leafNodeStubCapabilities(), nil)
			if leafErr != nil {
				t.Fatalf("NewLeafNode for a tree to decode: %v", leafErr)
			}
			tree := NewRatchetTree()
			if setErr := tree.SetLeaf(LeafIndex(0), leaf); setErr != nil {
				t.Fatalf("SetLeaf: %v", setErr)
			}
			encoded, encodeErr := marshalRatchetTree(tree)
			if encodeErr != nil {
				t.Fatalf("marshalRatchetTree: %v", encodeErr)
			}
			out, parseErr := UnmarshalRatchetTree(take(encoded))
			if parseErr != nil {
				t.Fatalf("UnmarshalRatchetTree: %v", parseErr)
			}
			decoded := out.Leaf(LeafIndex(0))
			if decoded == nil {
				t.Fatalf("the decoded tree has no leaf at index 0")
			}
			// the signature is left out for NewLeafNode's reason: it covers a Lifetime
			// stamped off the wall clock, so two calls a second apart carry different ones
			return [][]byte{decoded.EncryptionKey, decoded.SignatureKey, decoded.Credential.Identity}
		}},
		{name: "hpkeLabeledExtract", call: func(take func([]byte) []byte) [][]byte {
			return [][]byte{hpkeLabeledExtract(take(kemSuiteId), take(ikm), "eae_prk", take(sharedSecret))}
		}},
		{name: "hpkeLabeledExpand", call: func(take func([]byte) []byte) [][]byte {
			expanded, expandErr := hpkeLabeledExpand(take(kemSuiteId), take(sharedSecret), "shared_secret", take(info), 32)
			if expandErr != nil {
				t.Fatalf("hpkeLabeledExpand: %v", expandErr)
			}
			return [][]byte{expanded}
		}},
		{name: "HpkeDeriveKeyPair", call: func(take func([]byte) []byte) [][]byte {
			derivedPriv, derivedPub, deriveErr := HpkeDeriveKeyPair(params, take(ikm))
			if deriveErr != nil {
				t.Fatalf("HpkeDeriveKeyPair: %v", deriveErr)
			}
			return [][]byte{derivedPriv, derivedPub}
		}},
		{name: "hpkeExtractAndExpand", call: func(take func([]byte) []byte) [][]byte {
			secret, expandErr := hpkeExtractAndExpand(params, take(sharedSecret), take(kemOutput))
			if expandErr != nil {
				t.Fatalf("hpkeExtractAndExpand: %v", expandErr)
			}
			return [][]byte{secret}
		}},
		{name: "hpkeEncap", call: func(take func([]byte) []byte) [][]byte {
			secret, encapsulated, encapErr := hpkeEncap(constantReader{value: 0x77}, params, HpkePublicKey(take(pub)))
			if encapErr != nil {
				t.Fatalf("hpkeEncap: %v", encapErr)
			}
			return [][]byte{secret, encapsulated}
		}},
		{name: "hpkeEncapDeterministic", call: func(take func([]byte) []byte) [][]byte {
			secret, encapsulated, encapErr := hpkeEncapDeterministic(params, HpkePublicKey(take(pub)), HpkePrivateKey(take(priv)))
			if encapErr != nil {
				t.Fatalf("hpkeEncapDeterministic: %v", encapErr)
			}
			return [][]byte{secret, encapsulated}
		}},
		{name: "hpkeDecap", call: func(take func([]byte) []byte) [][]byte {
			secret, decapErr := hpkeDecap(params, HpkePrivateKey(take(priv)), take(kemOutput))
			if decapErr != nil {
				t.Fatalf("hpkeDecap: %v", decapErr)
			}
			return [][]byte{secret}
		}},
		{name: "hpkeNewAead", call: func(take func([]byte) []byte) [][]byte {
			if _, aeadErr := hpkeNewAead(params, take(key)); aeadErr != nil {
				t.Fatalf("hpkeNewAead: %v", aeadErr)
			}
			return nil
		}},
		{name: "hpkeKeyScheduleContext", call: func(take func([]byte) []byte) [][]byte {
			return [][]byte{hpkeKeyScheduleContext(take(suiteId), take(info))}
		}},
		{name: "hpkeKeySchedule", call: func(take func([]byte) []byte) [][]byte {
			if _, scheduleErr := hpkeKeySchedule(params, take(sharedSecret), take(info)); scheduleErr != nil {
				t.Fatalf("hpkeKeySchedule: %v", scheduleErr)
			}
			return nil
		}},
		{name: "HpkeSetupBaseS", call: func(take func([]byte) []byte) [][]byte {
			encapsulated, _, setupErr := HpkeSetupBaseS(constantReader{value: 0x66}, params, HpkePublicKey(take(pub)), take(info))
			if setupErr != nil {
				t.Fatalf("HpkeSetupBaseS: %v", setupErr)
			}
			return [][]byte{encapsulated}
		}},
		{name: "HpkeSetupBaseR", call: func(take func([]byte) []byte) [][]byte {
			_, setupErr := HpkeSetupBaseR(params, HpkePrivateKey(take(priv)), take(sealedKemOutput), take(info))
			if setupErr != nil {
				t.Fatalf("HpkeSetupBaseR: %v", setupErr)
			}
			return nil
		}},
		{name: "HpkeSealBase", call: func(take func([]byte) []byte) [][]byte {
			encapsulated, ciphertext, sealErr := HpkeSealBase(constantReader{value: 0x66}, params,
				HpkePublicKey(take(pub)), take(info), take(aad), take(plaintext))
			if sealErr != nil {
				t.Fatalf("HpkeSealBase: %v", sealErr)
			}
			return [][]byte{encapsulated, ciphertext}
		}},
		{name: "HpkeOpenBase", call: func(take func([]byte) []byte) [][]byte {
			opened, openErr := HpkeOpenBase(params, HpkePrivateKey(take(priv)), take(sealedKemOutput),
				take(info), take(aad), take(sealedCiphertext))
			if openErr != nil {
				t.Fatalf("HpkeOpenBase: %v", openErr)
			}
			return [][]byte{opened}
		}},
		{name: "X25519PrivateKey", call: func(take func([]byte) []byte) [][]byte {
			if _, keyErr := X25519PrivateKey(take(priv)); keyErr != nil {
				t.Fatalf("X25519PrivateKey: %v", keyErr)
			}
			return nil
		}},
		{name: "X25519PublicKey", call: func(take func([]byte) []byte) [][]byte {
			if _, keyErr := X25519PublicKey(take(pub)); keyErr != nil {
				t.Fatalf("X25519PublicKey: %v", keyErr)
			}
			return nil
		}},
		// group_context.go's field copier. It is the smallest member of this class and
		// the one where the properties are the whole function: Clone exists so a retained
		// past epoch cannot alias the live one, so a copier that answered a view over its
		// argument would defeat the only thing it is for, and no encoding would change.
		{name: "cloneBytes", call: func(take func([]byte) []byte) [][]byte {
			return [][]byte{cloneBytes(take(plaintext))}
		}},
		// the key schedule's first derivation. Both secrets are the caller's and both are
		// read again after the call — the previous epoch's init secret is still needed if
		// the commit turns out to be invalid — and the function erases a pseudorandom key
		// of its own in between them, so a zeroize aimed one line wrong lands in an array
		// the caller still owns and every derivation after it comes out of zeroes.
		{name: "DeriveJoinerSecret", call: func(take func([]byte) []byte) [][]byte {
			joiner, joinerErr := DeriveJoinerSecret(crypto, take(initSecretPrev), take(commitSecret),
				ksVectorEpoch0GroupContext(t))
			if joinerErr != nil {
				t.Fatalf("DeriveJoinerSecret: %v", joinerErr)
			}
			return [][]byte{joiner}
		}},
		// the three key schedule entry points. Every secret each of them is handed is one
		// the caller reads again — an init secret is still needed if the commit turns out
		// to be invalid, a psk secret is shared by every epoch that names the same psk, and
		// the sampled epoch secret of a group being created is the caller's to erase — and
		// each of these erases a secret of its own somewhere in the middle, so a zeroize
		// aimed one line wrong lands in an array the caller still owns and every derivation
		// after it comes out of zeroes.
		//
		// The aliasing half is the load bearing one here. These retain what they build the
		// epoch from and erase all of it when the epoch leaves PastEpochWindow, so an
		// answer that were a view over an argument is a caller's slice cleared as a side
		// effect of an epoch ageing out — which no vector, digest or round trip can see.
		{name: "NewKeySchedule", call: func(take func([]byte) []byte) [][]byte {
			schedule, scheduleErr := NewKeySchedule(crypto, take(initSecretPrev), take(commitSecret),
				take(pskSecret), ksVectorEpoch0GroupContext(t))
			if scheduleErr != nil {
				t.Fatalf("NewKeySchedule: %v", scheduleErr)
			}
			return [][]byte{
				schedule.JoinerSecret(), schedule.WelcomeSecret(),
				schedule.Secrets().InitSecret, schedule.GroupContextBytes(),
			}
		}},
		{name: "NewKeyScheduleFromJoiner", call: func(take func([]byte) []byte) [][]byte {
			schedule, scheduleErr := NewKeyScheduleFromJoiner(crypto, take(joinerSecret),
				take(pskSecret), ksVectorEpoch0GroupContext(t))
			if scheduleErr != nil {
				t.Fatalf("NewKeyScheduleFromJoiner: %v", scheduleErr)
			}
			return [][]byte{
				schedule.JoinerSecret(), schedule.WelcomeSecret(),
				schedule.Secrets().InitSecret, schedule.GroupContextBytes(),
			}
		}},
		// the creation path answers nil for joiner_secret and welcome_secret by design, so
		// those two are absent here rather than read as empty results — see
		// TestNewKeyScheduleFromEpochSecretHasNoJoinerOrWelcomeSecret, which is what holds
		// them to being nil.
		{name: "NewKeyScheduleFromEpochSecret", call: func(take func([]byte) []byte) [][]byte {
			schedule, scheduleErr := NewKeyScheduleFromEpochSecret(crypto, take(epochSecret),
				ksVectorEpoch0GroupContext(t))
			if scheduleErr != nil {
				t.Fatalf("NewKeyScheduleFromEpochSecret: %v", scheduleErr)
			}
			return [][]byte{schedule.Secrets().InitSecret, schedule.GroupContextBytes()}
		}},
		// the welcome key and nonce. What it is handed is welcome_secret read straight off a
		// live epoch -- (*KeySchedule).WelcomeSecret() answers the schedule's own storage --
		// so a body that wrote into that array would erase a secret the epoch still holds,
		// and one that answered a view over it would hand the caller an aead key that goes
		// to zeros the moment the epoch ages out. Both answers are read: the key and the
		// nonce are cut from one secret, so a defect in either is a defect in the pair.
		{name: "WelcomeKeyNonce", call: func(take func([]byte) []byte) [][]byte {
			key, nonce, welcomeErr := WelcomeKeyNonce(crypto, take(welcomeSecret))
			if welcomeErr != nil {
				t.Fatalf("WelcomeKeyNonce: %v", welcomeErr)
			}
			return [][]byte{key, nonce}
		}},
		// the sender data key and nonce. The CIPHERTEXT is the argument that matters here
		// and it is why this row exists at all: it is the framing layer's outgoing
		// message, which the caller goes on to write into the wire format after this
		// call, and this construction takes a SLICE of it as the kdf context. A body that
		// reused that slice as scratch would corrupt the message it is protecting, and an
		// answer cut from it would be an aead key aliasing bytes that go out on the wire.
		// Both answers are read for the reason WelcomeKeyNonce's row reads both.
		{name: "SenderDataKeyNonce", call: func(take func([]byte) []byte) [][]byte {
			key, nonce, senderErr := SenderDataKeyNonce(crypto, take(welcomeSecret), take(value))
			if senderErr != nil {
				t.Fatalf("SenderDataKeyNonce: %v", senderErr)
			}
			return [][]byte{key, nonce}
		}},
		// the two transcript hash arithmetics. Every argument either one is handed is read
		// again after the call: the previous epoch's interim hash is the group's own and is
		// still needed if the commit turns out to be invalid, the serialized
		// ConfirmedTranscriptHashInput is the framed commit the caller goes on to verify a
		// signature over, and the confirmation tag is compared against a freshly computed one
		// afterwards. An answer cut from an argument would be worse still -- it would alias
		// the transcript the group carries into every later epoch.
		{name: "ConfirmedTranscriptHash", call: func(take func([]byte) []byte) [][]byte {
			return [][]byte{ConfirmedTranscriptHash(crypto, take(initSecretPrev), take(plaintext))}
		}},
		{name: "InterimTranscriptHash", call: func(take func([]byte) []byte) [][]byte {
			interim, interimErr := InterimTranscriptHash(crypto, take(initSecretPrev), take(commitSecret))
			if interimErr != nil {
				t.Fatalf("InterimTranscriptHash: %v", interimErr)
			}
			return [][]byte{interim}
		}},
		// the secret tree's constructor. encryption_secret is one of the nine secrets an
		// epoch holds and zeroizes on its own schedule, and this tree erases what IT holds,
		// so a constructor that seeded the root with the caller's array rather than with a
		// copy of it would destroy the epoch's encryption_secret the first time any leaf was
		// taken -- and every correctness test in this package would still pass, because the
		// tree itself would be right. The answer read is the leaf secret rather than the
		// constructor's own, which is a struct.
		{name: "NewSecretTree", call: func(take func([]byte) []byte) [][]byte {
			tree, treeErr := NewSecretTree(crypto, 8, take(initSecretPrev))
			if treeErr != nil {
				t.Fatalf("NewSecretTree: %v", treeErr)
			}
			leafSecret, takeErr := tree.takeLeafSecret(6)
			if takeErr != nil {
				t.Fatalf("takeLeafSecret: %v", takeErr)
			}
			return [][]byte{leafSecret}
		}},
		// the leaf credential constructor. It is not a cryptographic construction, and it is in
		// this class for exactly the reason the class is derived rather than listed: it is handed
		// a caller's array and it KEEPS what it is handed. A credential that aliased the identity
		// would change under the leaf carrying it the next time that caller wrote into its own
		// buffer, which is a signature that verified when it was made and does not afterwards,
		// with nothing in between to point at. The answer read is the identity, since the
		// constructor's own answer is a struct.
		{name: "BasicCredential", call: func(take func([]byte) []byte) [][]byte {
			credential := BasicCredential(take([]byte("the identity a basic credential carries")))
			return [][]byte{credential.Identity}
		}},
		// the public half of a signature key pair this package was handed. The seed is the
		// caller's secret and is read again -- it is what the caller goes on signing with --
		// and the answer must not be a window onto the expanded key, whose first 32 octets
		// ARE that seed: a caller holding what it was told is a public key would be holding
		// the private one as well.
		{name: "signaturePublicKeyOf", call: func(take func([]byte) []byte) [][]byte {
			signaturePub, keyErr := signaturePublicKeyOf(SignaturePrivateKey(take(bytes.Repeat([]byte{0x53}, 32))))
			if keyErr != nil {
				t.Fatalf("signaturePublicKeyOf: %v", keyErr)
			}
			return [][]byte{signaturePub}
		}},
		// the public half of an hpke key pair this package was handed, and the same argument as
		// the row above with the operand one curve over. The scalar is the caller's secret and is
		// read again -- it is the leaf key that caller goes on opening its update paths with --
		// and the answer must not be a window onto it: both registered suites are DHKEM(X25519),
		// so the private half IS the 32 octet scalar and a shared array would hand a caller its
		// own private key back under the name of a public one.
		{name: "hpkePublicKeyOf", call: func(take func([]byte) []byte) [][]byte {
			hpkePub, keyErr := hpkePublicKeyOf(HpkePrivateKey(take(bytes.Repeat([]byte{0x48}, x25519KeySize))))
			if keyErr != nil {
				t.Fatalf("hpkePublicKeyOf: %v", keyErr)
			}
			return [][]byte{hpkePub}
		}},
		// the key_package leaf constructor. Every vector it is handed ends up INSIDE the
		// value it answers and is covered by that value's signature, so a leaf that aliased
		// any of them changes after it was signed the next time its caller writes into its
		// own buffer -- a signature that verified when it was made and does not afterwards,
		// with nothing in between to point at.
		//
		// The signature is deliberately not among the results. It covers a Lifetime stamped
		// off the wall clock, so two calls a second apart answer different signatures and the
		// determinism half of this gate would report a defect that is not there a few times
		// in a hundred runs. What is read instead is every array the constructor was handed
		// and kept, which is the aliasing property this gate exists for;
		// TestNewLeafNodeReadsEveryArgumentItWasHanded is what holds the leaf to depending on
		// all of them.
		{name: "NewLeafNode", call: func(take func([]byte) []byte) [][]byte {
			leaf, leafErr := NewLeafNode(crypto, SignaturePrivateKey(take(bytes.Repeat([]byte{0x54}, 32))),
				Credential{CredentialType: CredentialTypeBasic, Identity: take([]byte("alice"))},
				HpkePublicKey(take(pub)), leafNodeStubCapabilities(),
				[]Extension{{ExtensionType: ExtensionTypeUrmessageLeafKeys, ExtensionData: take([]byte("k"))}})
			if leafErr != nil {
				t.Fatalf("NewLeafNode: %v", leafErr)
			}
			return [][]byte{leaf.EncryptionKey, leaf.SignatureKey, leaf.Credential.Identity,
				leaf.Extensions[0].ExtensionData}
		}},
		// p7 task 11's group creation, which is handed a caller's array in four places at once:
		// the signature private key, the credential's identity, the group id, and the device's
		// X-Wing public key. Every one of them is KEPT -- in the founding leaf, in the group
		// context, in the leaf_keys extension the leaf carries -- so a construction that viewed
		// any of them is a group whose leaf changes the next time its caller writes into the
		// buffer it was cut from, with every signature still verifying at the moment it was made
		// and nothing in between to point at.
		//
		// FOUR IS THIS ROW'S ARRAYS AND NOT THE CLASS'S, AND A FIFTH NAME DOES NOT BELONG HERE.
		// The extension bodies were the fifth and this row ran clean over them for a year, because
		// what it reads is what a construction ANSWERS and a body kept in the group context is
		// answered by nothing. The retention half is caller_arrays_test.go, which walks the
		// argument TYPES for every route they declare storage down instead of naming arrays, so a
		// sixth is covered on the day it is declared. Anything to be added about a NEW array of a
		// group belongs there; what belongs here is what this row already is.
		//
		// What it ANSWERS is deliberately the four values that are a function of its arguments
		// alone. A group draws two secrets and stamps a leaf Lifetime off the wall clock, so the
		// tree hash, the epoch authenticator and everything downstream of them differ between two
		// calls for reasons that are not the arguments, and the determinism half of this gate
		// would report a defect that is not there. Those values MOVING is the assertion
		// TestNewGroupDrawsEachFoundingSecretFreshAndFromItsOwnDraw makes, which is where the
		// entropy is held.
		{name: "NewGroup", call: func(take func([]byte) []byte) [][]byte {
			identity := []byte("the creator of this group")
			policy := &GroupPolicyExtension{Roles: []RoleEntry{{MemberId: identity, Role: RoleOwner}}}
			if canonicalErr := policy.Canonicalize(); canonicalErr != nil {
				t.Fatalf("Canonicalize the policy this row's group carries: %v", canonicalErr)
			}
			policyExt, policyErr := policy.Encode()
			if policyErr != nil {
				t.Fatalf("Encode the policy this row's group carries: %v", policyErr)
			}
			group, groupErr := NewGroup(&GroupConfig{
				Suite:      CipherSuiteX25519ChaCha20Sha256Ed25519,
				GroupId:    take([]byte("group-1")),
				Extensions: []Extension{policyExt},
				Crypto:     crypto,
				Store:      newTestStore(),
				LeafKeys: LeafKeysExtension{
					AlgId:          AlgIdXwing,
					DeviceXwingPub: take(bytes.Repeat([]byte{0x62}, XwingPublicKeyLen)),
				},
			}, SignaturePrivateKey(take(bytes.Repeat([]byte{0x54}, 32))),
				Credential{CredentialType: CredentialTypeBasic, Identity: take(identity)})
			if groupErr != nil {
				t.Fatalf("NewGroup: %v", groupErr)
			}
			defer group.Close()
			leaf := group.OwnLeafNodeCopy()
			if leaf == nil {
				t.Fatal("the group this row built holds no leaf at its own index")
			}
			members := group.Members()
			if len(members) != 1 || members[0].LeafKeys == nil {
				t.Fatalf("the group this row built answers %d members; the leaf_keys body is one of the four arrays it is read for",
					len(members))
			}
			return [][]byte{group.GroupId(), leaf.Credential.Identity, leaf.SignatureKey,
				members[0].LeafKeys.DeviceXwingPub}
		}},
		// p7 task 16's join, which is handed a caller's arrays in six places at once: the Welcome,
		// the out-of-band tree, the group id its config names, and the three private keys of the
		// joiner's key material. Two of those three are KEPT -- the signing key becomes this
		// group's own and the encryption key becomes its leaf private state -- so a join that
		// viewed either is a member whose signatures and whose path decryptions change the next
		// time its caller reuses the buffer it read them out of.
		//
		// What it ANSWERS here are four values that are a function of its inputs alone, which is
		// the whole of a join: it draws nothing at all, so unlike NewGroup next door even the
		// epoch authenticator is deterministic across two calls and is worth comparing. The
		// authenticator is the one that makes this row a live reading of the derivation rather
		// than of the decode.
		{name: "JoinFromWelcome", call: func(take func([]byte) []byte) [][]byte {
			cfg := testGroupConfig(t, crypto, joinRowJoiner, "the group the input gate joins")
			cfg.GroupId = take(cfg.GroupId)
			joined, joinErr := JoinFromWelcome(cfg, take(joinRowResult.Welcome),
				take(joinRowResult.RatchetTree), &JoinKeyMaterial{
					KeyPackage:     joinRowKeys.KeyPackage,
					InitPrivate:    HpkePrivateKey(take(joinRowKeys.InitPrivate)),
					EncryptPrivate: HpkePrivateKey(take(joinRowKeys.EncryptPrivate)),
					SignPrivate:    SignaturePrivateKey(take(joinRowKeys.SignPrivate)),
				})
			if joinErr != nil {
				t.Fatalf("JoinFromWelcome: %v", joinErr)
			}
			defer joined.Close()
			leaf := joined.OwnLeafNodeCopy()
			if leaf == nil {
				t.Fatal("the group this row joined holds no leaf at its own index")
			}
			return [][]byte{joined.GroupId(), leaf.Credential.Identity, leaf.SignatureKey,
				joined.EpochAuthenticator()}
		}},
		// the path secret ladder of RFC 9420 section 7.4. Its first rung is the caller's own
		// array -- in task 22 it is the plaintext an HpkeOpen just produced -- and the answer
		// becomes the private state of an epoch, so a ladder that VIEWED that buffer is a group
		// whose path secret changes the next time its caller reuses it, with every key still
		// deriving and nothing to point at. Every rung is read, not the last one alone: a body
		// that aliased only the rung it was handed is invisible from the top of the ladder.
		{name: "DerivePathSecrets", call: func(take func([]byte) []byte) [][]byte {
			return DerivePathSecrets(crypto, take(bytes.Repeat([]byte{0x86}, params.Nh)), 3)
		}},
		{name: "DeriveNodeKeyPair", call: func(take func([]byte) []byte) [][]byte {
			nodePriv, nodePub, keyErr := DeriveNodeKeyPair(crypto,
				take(bytes.Repeat([]byte{0x87}, params.Nh)))
			if keyErr != nil {
				t.Fatalf("DeriveNodeKeyPair: %v", keyErr)
			}
			return [][]byte{nodePriv, nodePub}
		}},
		// the private state's constructor, which is in this class for the reason the joiner's
		// transcript seeding is: what it keeps outlives the call by the whole life of the group.
		// A leaf private key held over its caller's buffer is a member that stops being able to
		// decrypt at the moment that buffer is next written, one epoch after the mistake.
		{name: "NewTreeKEMPrivate", call: func(take func([]byte) []byte) [][]byte {
			state := NewTreeKEMPrivate(LeafIndex(0),
				HpkePrivateKey(take(bytes.Repeat([]byte{0x88}, 32))))
			return [][]byte{state.EncryptionPriv}
		}},
		// section 6.1's signature preimage and the two operations over it. Every array these
		// are handed -- the signing key, the group context, and the byte fields of the content
		// -- is read again by the caller after the call, and a preimage or a signature that was
		// a WINDOW onto one of them changes the next time that caller writes into its own
		// buffer: a message that verified when it was built and does not afterwards, with
		// nothing in between to point at.
		//
		// The signature is the answer read and not the content beside it in the
		// AuthenticatedContent, which is deliberate and is why the row names it: that content
		// IS the caller's own, carried through by design, so reading it here would be holding
		// this constructor to not doing its job. What must be fresh is what it produces.
		{name: "FramedContentTBSBytes", call: func(take func([]byte) []byte) [][]byte {
			tbs, tbsErr := FramedContentTBSBytes(WireFormatPrivateMessage,
				framingStubFramedContentOver(take), take(framingStubGroupContext(t, crypto)))
			if tbsErr != nil {
				t.Fatalf("FramedContentTBSBytes: %v", tbsErr)
			}
			return [][]byte{tbs}
		}},
		{name: "SignAuthenticatedContent", call: func(take func([]byte) []byte) [][]byte {
			authContent, signErr := SignAuthenticatedContent(crypto,
				SignaturePrivateKey(take(framingStubSignaturePriv())), WireFormatPrivateMessage,
				framingStubFramedContentOver(take), take(framingStubGroupContext(t, crypto)))
			if signErr != nil {
				t.Fatalf("SignAuthenticatedContent: %v", signErr)
			}
			return [][]byte{authContent.Auth.Signature}
		}},
		{name: "VerifyAuthenticatedContent", call: func(take func([]byte) []byte) [][]byte {
			groupContext := framingStubGroupContext(t, crypto)
			authContent, signErr := SignAuthenticatedContent(crypto, framingStubSignaturePriv(),
				WireFormatPrivateMessage, framingStubFramedContent(), groupContext)
			if signErr != nil {
				t.Fatalf("sign the message the verify row reads: %v", signErr)
			}
			verifyPub, keyErr := signaturePublicKeyOf(framingStubSignaturePriv())
			if keyErr != nil {
				t.Fatalf("the public half of the key the verify row is built over: %v", keyErr)
			}
			if verifyErr := VerifyAuthenticatedContent(crypto, SignaturePublicKey(take(verifyPub)),
				authContent, take(groupContext)); verifyErr != nil {
				t.Fatalf("VerifyAuthenticatedContent refused a signature made over the same message: %v", verifyErr)
			}
			return nil
		}},
		// section 6's ValSem002 and ValSem003, on VerifyAuthenticatedContent's terms: what it
		// answers is a verdict and no bytes. The half that matters is the one that still runs. The
		// group id it is handed is the RECEIVER's own -- one array held for the life of the group
		// and compared against again by every message after this one -- so a rule that sorted it,
		// padded it to a common length, or wrote a sentinel through it would corrupt the value
		// every later comparison is made against, and the corruption would be invisible until some
		// message from the right group was refused.
		{name: "CheckFramedContentContext", call: func(take func([]byte) []byte) [][]byte {
			content := framingStubFramedContentOver(take)
			if checkErr := CheckFramedContentContext(content,
				take(bytes.Clone(content.GroupId)), content.Epoch); checkErr != nil {
				t.Fatalf("CheckFramedContentContext refused a content carrying its own group id and epoch: %v",
					checkErr)
			}
			return nil
		}},
		// section 6.1's membership tag preimage and the two operations over it. All three read
		// the caller's group context and the arrays inside the caller's own FramedContent, and
		// every one of those is read again after the call: the message is about to be
		// serialized and sent.
		{name: "AuthenticatedContentTBMBytes", call: func(take func([]byte) []byte) [][]byte {
			groupContext := take(framingStubGroupContext(t, crypto))
			signed, signErr := SignAuthenticatedContent(crypto, framingStubSignaturePriv(),
				WireFormatPrivateMessage, framingStubFramedContentOver(take), groupContext)
			if signErr != nil {
				t.Fatalf("sign the message the tag preimage row reads: %v", signErr)
			}
			tbm, tbmErr := AuthenticatedContentTBMBytes(signed, groupContext)
			if tbmErr != nil {
				t.Fatalf("AuthenticatedContentTBMBytes: %v", tbmErr)
			}
			return [][]byte{tbm}
		}},
		{name: "ComputeMembershipTag", call: func(take func([]byte) []byte) [][]byte {
			groupContext := take(framingStubGroupContext(t, crypto))
			signed, signErr := SignAuthenticatedContent(crypto, framingStubSignaturePriv(),
				WireFormatPrivateMessage, framingStubFramedContentOver(take), groupContext)
			if signErr != nil {
				t.Fatalf("sign the message the membership tag row reads: %v", signErr)
			}
			tag, tagErr := ComputeMembershipTag(crypto,
				take(bytes.Repeat([]byte{0x6b}, crypto.HashSize())), signed, groupContext)
			if tagErr != nil {
				t.Fatalf("ComputeMembershipTag: %v", tagErr)
			}
			return [][]byte{tag}
		}},
		{name: "verifyMembershipTag", call: func(take func([]byte) []byte) [][]byte {
			groupContext := framingStubGroupContext(t, crypto)
			membershipKey := bytes.Repeat([]byte{0x6b}, crypto.HashSize())
			signed, signErr := SignAuthenticatedContent(crypto, framingStubSignaturePriv(),
				WireFormatPrivateMessage, framingStubFramedContent(), groupContext)
			if signErr != nil {
				t.Fatalf("sign the message the membership verify row reads: %v", signErr)
			}
			tag, tagErr := ComputeMembershipTag(crypto, membershipKey, signed, groupContext)
			if tagErr != nil {
				t.Fatalf("the tag the membership verify row reads: %v", tagErr)
			}
			if verifyErr := verifyMembershipTag(crypto, take(membershipKey), signed,
				take(groupContext), take(tag)); verifyErr != nil {
				t.Fatalf("verifyMembershipTag refused a tag taken over the same message: %v", verifyErr)
			}
			return nil
		}},
		// section 6.3's two AADs. Both are read again by the caller after the call -- the header
		// they cover is the PrivateMessage header that is about to be serialized -- so a builder
		// that wrote through one of its arguments would corrupt the very message it describes.
		{name: "senderDataAAD", call: func(take func([]byte) []byte) [][]byte {
			aad, aadErr := senderDataAAD(take([]byte{0x01, 0x02}), 9, ContentTypeApplication)
			if aadErr != nil {
				t.Fatalf("senderDataAAD: %v", aadErr)
			}
			return [][]byte{aad}
		}},
		{name: "privateContentAAD", call: func(take func([]byte) []byte) [][]byte {
			aad, aadErr := privateContentAAD(take([]byte{0x01, 0x02}), 9, ContentTypeApplication,
				take([]byte{0xa5, 0xa6}))
			if aadErr != nil {
				t.Fatalf("privateContentAAD: %v", aadErr)
			}
			return [][]byte{aad}
		}},
		// section 6.2's seal. The only thing it produces is the membership tag; the rest of the
		// message it answers is the caller's own content carried through, which is the structure
		// working rather than an alias to report.
		{name: "SealPublicMessage", call: func(take func([]byte) []byte) [][]byte {
			groupContext := take(framingStubGroupContext(t, crypto))
			content := framingStubFramedContentOver(take)
			content.ContentType = ContentTypeProposal
			content.ApplicationData = nil
			content.Proposal = &Proposal{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: 5}}
			signed, signErr := SignAuthenticatedContent(crypto, framingStubSignaturePriv(),
				WireFormatPublicMessage, content, groupContext)
			if signErr != nil {
				t.Fatalf("sign the message the seal row reads: %v", signErr)
			}
			message, sealErr := SealPublicMessage(crypto,
				take(bytes.Repeat([]byte{0x6b}, crypto.HashSize())), signed, groupContext)
			if sealErr != nil {
				t.Fatalf("SealPublicMessage: %v", sealErr)
			}
			return [][]byte{message.MembershipTag}
		}},
		// section 6.2's open, which answers a view over the message it was handed rather than bytes
		// it produced and is named as answering none. What its row states is the half that matters
		// for an open: the membership key, the message and the group context are all read again by
		// the caller afterwards.
		{name: "OpenPublicMessage", call: func(take func([]byte) []byte) [][]byte {
			groupContext := framingStubGroupContext(t, crypto)
			membershipKey := bytes.Repeat([]byte{0x6b}, crypto.HashSize())
			priv, pub, keyErr := crypto.SignatureKeyPair()
			if keyErr != nil {
				t.Fatalf("a key pair for the open row: %v", keyErr)
			}
			content := framingStubFramedContent()
			content.ContentType = ContentTypeProposal
			content.ApplicationData = nil
			content.Proposal = &Proposal{ProposalType: ProposalTypeRemove, Remove: &Remove{Removed: 5}}
			signed, signErr := SignAuthenticatedContent(crypto, priv, WireFormatPublicMessage,
				content, groupContext)
			if signErr != nil {
				t.Fatalf("sign the message the open row reads: %v", signErr)
			}
			message, sealErr := SealPublicMessage(crypto, membershipKey, signed, groupContext)
			if sealErr != nil {
				t.Fatalf("seal the message the open row reads: %v", sealErr)
			}
			if _, openErr := OpenPublicMessage(crypto, take(membershipKey), message,
				StaticSignatureKey(pub), take(groupContext)); openErr != nil {
				t.Fatalf("OpenPublicMessage refused a message it had just sealed: %v", openErr)
			}
			return nil
		}},
		// section 6.3.2's seal and open. The seal's answer is the sealed sender data, which is
		// bytes it produced; the open's is the reuse guard out of the plaintext it decrypted,
		// which is the one byte field its answer carries and is fresh storage out of the aead
		// rather than a window onto anything the caller passed. What both rows state beyond
		// that is the half that matters for these two: the secret, the header and the
		// ciphertext are all read again by the caller after the call -- the ciphertext most of
		// all, since it is about to be serialized into the very message this header describes.
		{name: "sealSenderData", call: func(take func([]byte) []byte) [][]byte {
			sealed, sealErr := sealSenderData(crypto, take(bytes.Repeat([]byte{0x6d}, crypto.HashSize())),
				&SenderData{LeafIndex: 2, Generation: 5, ReuseGuard: [4]byte{0x21, 0x22, 0x23, 0x24}},
				&PrivateMessage{GroupId: take([]byte{0x11, 0x12}), Epoch: 4,
					ContentType: ContentTypeApplication, AuthenticatedData: take([]byte{0x13})},
				take(bytes.Repeat([]byte{0x6e}, crypto.HashSize())))
			if sealErr != nil {
				t.Fatalf("sealSenderData: %v", sealErr)
			}
			return [][]byte{sealed}
		}},
		{name: "openSenderData", call: func(take func([]byte) []byte) [][]byte {
			secret := bytes.Repeat([]byte{0x6d}, crypto.HashSize())
			ciphertext := bytes.Repeat([]byte{0x6e}, crypto.HashSize())
			header := &PrivateMessage{GroupId: []byte{0x11, 0x12}, Epoch: 4,
				ContentType: ContentTypeApplication, AuthenticatedData: []byte{0x13}}
			senderData := &SenderData{LeafIndex: 2, Generation: 5,
				ReuseGuard: [4]byte{0x21, 0x22, 0x23, 0x24}}
			sealed, sealErr := sealSenderData(crypto, secret, senderData, header, ciphertext)
			if sealErr != nil {
				t.Fatalf("seal the sender data the open row reads: %v", sealErr)
			}
			opened, openErr := openSenderData(crypto, take(secret), take(sealed),
				&PrivateMessage{GroupId: take(header.GroupId), Epoch: header.Epoch,
					ContentType:       header.ContentType,
					AuthenticatedData: take(header.AuthenticatedData)},
				take(ciphertext))
			if openErr != nil {
				t.Fatalf("openSenderData refused a header it had just sealed: %v", openErr)
			}
			return [][]byte{opened.ReuseGuard[:]}
		}},
		// the pre-ratchet peek, whose one answer of bytes is the caller's own
		// authenticated_data. It is here rather than in the no-bytes table for exactly that
		// reason: it DOES hand back a byte string, and what this gate asks of it is that the
		// string is the peek's own storage -- a view onto the marshalled message it was handed
		// would be a window a caller could edit after the refusal it drove had been taken.
		{name: "PeekPrivateMessageSender", call: func(take func([]byte) []byte) [][]byte {
			signed := framingPrivateSignedContent(t, crypto, framingTestMemberContent())
			message, sealErr := SealPrivateMessage(crypto, framingNewKeySource(crypto, 0x4b, 0),
				signed.senderDataSecret, signed.authContent, PaddingSizeV1)
			if sealErr != nil {
				t.Fatalf("seal the message the peek row reads: %v", sealErr)
			}
			marshalled, marshalErr := MarshalMLSMessage(&MLSMessage{
				Version:        ProtocolVersionMls10,
				WireFormat:     WireFormatPrivateMessage,
				PrivateMessage: message,
			})
			if marshalErr != nil {
				t.Fatalf("marshal the message the peek row reads: %v", marshalErr)
			}
			_, aad, _, peekErr := PeekPrivateMessageSender(crypto,
				take(signed.senderDataSecret), take(marshalled))
			if peekErr != nil {
				t.Fatalf("PeekPrivateMessageSender refused a message it had just sealed: %v", peekErr)
			}
			return [][]byte{aad}
		}},
		// section 6.3.1's serializer, its decoder and its reuse guard, and then section 6.3's
		// seal and open.
		//
		// The seal rows build a provider of their OWN over a constant source rather than using
		// the one above. That is not a way around this gate: it is what makes the rows readable
		// at all, since a seal draws four octets of reuse_guard per message and two calls over
		// crypto/rand answer differently for a reason that has nothing to do with anybody's
		// storage. Over a fixed source the guard is the same octets twice and the answers are
		// comparable, which is the property this gate is about.
		//
		// The decoder's answers are its ApplicationData and its Signature and NOT the header
		// fields it carries through. Those are windows onto the header by construction -- section
		// 6.3.1 does not put a group id or an epoch inside the ciphertext, so a decoder that
		// produced them would be producing them out of nothing -- and the header's arrays are
		// still handed in through the recorder, so the half this row is here for still runs.
		{name: "applyReuseGuard", call: func(take func([]byte) []byte) [][]byte {
			return [][]byte{applyReuseGuard(take(bytes.Repeat([]byte{0x5c}, params.Nn)),
				[senderDataReuseGuardSize]byte{0x11, 0x22, 0x33, 0x44})}
		}},
		{name: "marshalPrivateMessageContentWithPadding", call: func(take func([]byte) []byte) [][]byte {
			content := framingStubFramedContentOver(take)
			encoded, marshalErr := marshalPrivateMessageContentWithPadding(content,
				&FramedContentAuthData{Signature: take(bytes.Repeat([]byte{0x51}, 64))},
				take(make([]byte, 16)))
			if marshalErr != nil {
				t.Fatalf("marshalPrivateMessageContentWithPadding: %v", marshalErr)
			}
			return [][]byte{encoded}
		}},
		{name: "unmarshalPrivateMessageContent", call: func(take func([]byte) []byte) [][]byte {
			content := framingStubFramedContent()
			auth := &FramedContentAuthData{Signature: bytes.Repeat([]byte{0x51}, 64)}
			plaintext, marshalErr := marshalPrivateMessageContent(content, auth, 16)
			if marshalErr != nil {
				t.Fatalf("the plaintext the decoder row reads: %v", marshalErr)
			}
			decoded, decodedAuth, err := unmarshalPrivateMessageContent(take(plaintext),
				&PrivateMessage{
					GroupId:           take(content.GroupId),
					Epoch:             content.Epoch,
					ContentType:       content.ContentType,
					AuthenticatedData: take(content.AuthenticatedData),
				}, content.Sender)
			if err != nil {
				t.Fatalf("unmarshalPrivateMessageContent refused a plaintext it had just encoded: %v", err)
			}
			return [][]byte{decoded.ApplicationData, decodedAuth.Signature}
		}},
		{name: "SealPrivateMessage", call: func(take func([]byte) []byte) [][]byte {
			sealer := mustProviderOver(t, crypto.Suite(), constantReader{value: 0x99})
			message, sealErr := SealPrivateMessage(sealer, framingNewKeySource(sealer, 0x4b, 0),
				take(bytes.Repeat([]byte{0x6d}, sealer.HashSize())), &AuthenticatedContent{
					WireFormat: WireFormatPrivateMessage,
					Content:    *framingStubFramedContentOver(take),
					Auth:       FramedContentAuthData{Signature: take(bytes.Repeat([]byte{0x51}, 64))},
				}, 16)
			if sealErr != nil {
				t.Fatalf("SealPrivateMessage: %v", sealErr)
			}
			return [][]byte{message.EncryptedSenderData, message.Ciphertext}
		}},
		{name: "sealPrivateMessage", call: func(take func([]byte) []byte) [][]byte {
			sealer := mustProviderOver(t, crypto.Suite(), constantReader{value: 0x99})
			message, sealErr := sealPrivateMessage(sealer, framingNewKeySource(sealer, 0x4b, 0),
				take(bytes.Repeat([]byte{0x6d}, sealer.HashSize())), &AuthenticatedContent{
					WireFormat: WireFormatPrivateMessage,
					Content:    *framingStubFramedContentOver(take),
					Auth:       FramedContentAuthData{Signature: take(bytes.Repeat([]byte{0x51}, 64))},
				}, take(bytes.Repeat([]byte{0x71}, 16)))
			if sealErr != nil {
				t.Fatalf("sealPrivateMessage: %v", sealErr)
			}
			return [][]byte{message.EncryptedSenderData, message.Ciphertext}
		}},
		// MASTER section 8.4.2 v2's three bodies, on exactly the two rows above's terms: the seal
		// that reports the generation it consumed, the same seal under the S3 pin, and the open
		// that reports the generation it opened at. Each is the body one of the neighbouring rows
		// delegates to, so each is swept over the same borrowed arrays for the same reason.
		{name: "sealPrivateMessageAt", call: func(take func([]byte) []byte) [][]byte {
			sealer := mustProviderOver(t, crypto.Suite(), constantReader{value: 0x99})
			message, _, sealErr := sealPrivateMessageAt(sealer, framingNewKeySource(sealer, 0x4b, 0),
				take(bytes.Repeat([]byte{0x6d}, sealer.HashSize())), &AuthenticatedContent{
					WireFormat: WireFormatPrivateMessage,
					Content:    *framingStubFramedContentOver(take),
					Auth:       FramedContentAuthData{Signature: take(bytes.Repeat([]byte{0x51}, 64))},
				}, take(bytes.Repeat([]byte{0x71}, 16)))
			if sealErr != nil {
				t.Fatalf("sealPrivateMessageAt: %v", sealErr)
			}
			return [][]byte{message.EncryptedSenderData, message.Ciphertext}
		}},
		{name: "sealPrivateMessageBound", call: func(take func([]byte) []byte) [][]byte {
			sealer := mustProviderOver(t, crypto.Suite(), constantReader{value: 0x99})
			message, sealErr := sealPrivateMessageBound(sealer, framingNewKeySource(sealer, 0x4b, 0),
				take(bytes.Repeat([]byte{0x6d}, sealer.HashSize())), &AuthenticatedContent{
					WireFormat: WireFormatPrivateMessage,
					Content:    *framingStubFramedContentOver(take),
					Auth:       FramedContentAuthData{Signature: take(bytes.Repeat([]byte{0x51}, 64))},
				}, take(bytes.Repeat([]byte{0x71}, 16)), 0)
			if sealErr != nil {
				t.Fatalf("sealPrivateMessageBound: %v", sealErr)
			}
			return [][]byte{message.EncryptedSenderData, message.Ciphertext}
		}},
		{name: "openPrivateMessageAt", call: func(take func([]byte) []byte) [][]byte {
			sealer := mustProviderOver(t, crypto.Suite(), constantReader{value: 0x99})
			priv, pub, keyErr := sealer.SignatureKeyPair()
			if keyErr != nil {
				t.Fatalf("the key pair the bound open row reads: %v", keyErr)
			}
			groupContext := framingStubGroupContext(t, sealer)
			secret := bytes.Repeat([]byte{0x6d}, sealer.HashSize())
			signed, signErr := SignAuthenticatedContent(sealer, priv, WireFormatPrivateMessage,
				framingStubFramedContent(), groupContext)
			if signErr != nil {
				t.Fatalf("sign the message the bound open row reads: %v", signErr)
			}
			message, sealErr := SealPrivateMessage(sealer, framingNewKeySource(sealer, 0x4b, 0),
				secret, signed, 16)
			if sealErr != nil {
				t.Fatalf("seal the message the bound open row reads: %v", sealErr)
			}
			if _, _, openErr := openPrivateMessageAt(sealer, framingNewKeySource(sealer, 0x4b, 0),
				take(secret), &PrivateMessage{
					GroupId:             take(message.GroupId),
					Epoch:               message.Epoch,
					ContentType:         message.ContentType,
					AuthenticatedData:   take(message.AuthenticatedData),
					EncryptedSenderData: take(message.EncryptedSenderData),
					Ciphertext:          take(message.Ciphertext),
				}, StaticSignatureKey(pub), take(groupContext)); openErr != nil {
				t.Fatalf("openPrivateMessageAt refused a message it had just sealed: %v", openErr)
			}
			return nil
		}},
		{name: "OpenPrivateMessage", call: func(take func([]byte) []byte) [][]byte {
			sealer := mustProviderOver(t, crypto.Suite(), constantReader{value: 0x99})
			priv, pub, keyErr := sealer.SignatureKeyPair()
			if keyErr != nil {
				t.Fatalf("the key pair the open row reads: %v", keyErr)
			}
			groupContext := framingStubGroupContext(t, sealer)
			secret := bytes.Repeat([]byte{0x6d}, sealer.HashSize())
			signed, signErr := SignAuthenticatedContent(sealer, priv, WireFormatPrivateMessage,
				framingStubFramedContent(), groupContext)
			if signErr != nil {
				t.Fatalf("sign the message the open row reads: %v", signErr)
			}
			message, sealErr := SealPrivateMessage(sealer, framingNewKeySource(sealer, 0x4b, 0),
				secret, signed, 16)
			if sealErr != nil {
				t.Fatalf("seal the message the open row reads: %v", sealErr)
			}
			if _, openErr := OpenPrivateMessage(sealer, framingNewKeySource(sealer, 0x4b, 0),
				take(secret), &PrivateMessage{
					GroupId:             take(message.GroupId),
					Epoch:               message.Epoch,
					ContentType:         message.ContentType,
					AuthenticatedData:   take(message.AuthenticatedData),
					EncryptedSenderData: take(message.EncryptedSenderData),
					Ciphertext:          take(message.Ciphertext),
				}, StaticSignatureKey(pub), take(groupContext)); openErr != nil {
				t.Fatalf("OpenPrivateMessage refused a message it had just sealed: %v", openErr)
			}
			return nil
		}},
		// the static resolver, whose answer is a COPY of the key it was handed rather than a window
		// onto it. The copy in its body is what this row observes: the aliasing half fails on a
		// resolver that captured the caller's slice, and the fresh storage half fails on one that
		// answered out of a buffer shared between calls.
		{name: "StaticSignatureKey", call: func(take func([]byte) []byte) [][]byte {
			resolve := StaticSignatureKey(SignaturePublicKey(take(bytes.Repeat([]byte{0x7c}, 32))))
			answered, resolveErr := resolve(Sender{SenderType: SenderTypeMember})
			if resolveErr != nil {
				t.Fatalf("StaticSignatureKey: %v", resolveErr)
			}
			return [][]byte{answered}
		}},
		// section 6's outermost decoder, and the one row of this class whose input an attacker
		// chooses. Every byte that arrives from the network reaches ParseMLSMessage, so a decoded
		// field that VIEWED the frame it arrived in is a field whoever owns that buffer can change
		// after the message was accepted -- and the caller handing the bytes over is a transport
		// that reuses its read buffer.
		{name: "ParseMLSMessage", call: func(take func([]byte) []byte) [][]byte {
			encoded, marshalErr := MarshalMLSMessage(&MLSMessage{
				Version:        ProtocolVersionMls10,
				WireFormat:     WireFormatPrivateMessage,
				PrivateMessage: framingTestPrivateMessage(),
			})
			if marshalErr != nil {
				t.Fatalf("encode the message the parse row reads: %v", marshalErr)
			}
			parsed, parseErr := ParseMLSMessage(take(encoded))
			if parseErr != nil {
				t.Fatalf("ParseMLSMessage: %v", parseErr)
			}
			return [][]byte{parsed.PrivateMessage.GroupId, parsed.PrivateMessage.Ciphertext}
		}},
		// the urmessage_group_policy decode and the two comparisons its canonical form rests on.
		//
		// The decode's answers are the member id and the server id it read, which are the two
		// byte runs it produces, and they must be COPIES: the body it is handed is an
		// Extension.ExtensionData a caller owns and may reuse, and a policy holding windows onto
		// it is a role list that changes after the group agreed to it. The two comparisons
		// produce nothing and are here for the half that still runs -- neither may touch the runs
		// it is ordering.
		{name: "ParseGroupPolicyExtension", call: func(take func([]byte) []byte) [][]byte {
			encoded, err := (&GroupPolicyExtension{
				Roles:    []RoleEntry{{MemberId: []byte{0x01, 0x02}, Role: RoleOwner}},
				ServerId: []byte("urmessage-v1-server"),
			}).Encode()
			if err != nil {
				t.Fatalf("the group policy body this row decodes: %v", err)
			}
			policy, err := ParseGroupPolicyExtension(take(encoded.ExtensionData))
			if err != nil {
				t.Fatalf("ParseGroupPolicyExtension over a body it had just encoded: %v", err)
			}
			return [][]byte{policy.Roles[0].MemberId, policy.ServerId}
		}},
		// the one helper every policy change goes through, item 242's P4 repair. It is handed a
		// caller's extension list -- every body in it is an Extension.ExtensionData the caller
		// owns -- and the policy body that replaces one of them, and it answers a list that
		// becomes a group context outliving every one of those buffers. So every body it answers
		// must be a COPY: the entry it keeps as well as the one it replaces, because a list holding
		// a window onto required_capabilities is a context that changes after the group agreed to
		// it. The two answers read are the kept body and the replaced one.
		{name: "ExtensionsWithGroupPolicy", call: func(take func([]byte) []byte) [][]byte {
			encoded, err := (&GroupPolicyExtension{
				Roles:    []RoleEntry{{MemberId: []byte{0x01, 0x02}, Role: RoleOwner}},
				ServerId: []byte("urmessage-v1-server"),
			}).Encode()
			if err != nil {
				t.Fatalf("the group policy body this row replaces with: %v", err)
			}
			replaced, err := ExtensionsWithGroupPolicy([]Extension{
				{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: take([]byte{0x01, 0x02, 0x03})},
				{ExtensionType: ExtensionTypeUrmessageGroupPolicy, ExtensionData: take([]byte{0xff})},
			}, take(encoded.ExtensionData))
			if err != nil {
				t.Fatalf("ExtensionsWithGroupPolicy over a two entry list: %v", err)
			}
			if len(replaced) != 2 {
				t.Fatalf("ExtensionsWithGroupPolicy answered %d entries for a two entry list", len(replaced))
			}
			return [][]byte{replaced[0].ExtensionData, replaced[1].ExtensionData}
		}},
		// the urmessage_owner_successor decode, whose one produced run is the successor member
		// id it read. It must be a COPY for the same reason the policy's two are: the body is an
		// Extension.ExtensionData a caller owns and may reuse, and a nomination holding a window
		// onto it names a successor that changes after the group agreed to it.
		{name: "ParseOwnerSuccessorExtension", call: func(take func([]byte) []byte) [][]byte {
			encoded, err := (&OwnerSuccessorExtension{
				Enabled:           true,
				SuccessorMemberId: []byte{0x01, 0x02},
				NominatedAtMs:     1,
				FloorMs:           SuccessionFloorMinMs,
			}).Encode()
			if err != nil {
				t.Fatalf("the nomination body this row decodes: %v", err)
			}
			nomination, err := ParseOwnerSuccessorExtension(take(encoded.ExtensionData))
			if err != nil {
				t.Fatalf("ParseOwnerSuccessorExtension over a body it had just encoded: %v", err)
			}
			return [][]byte{nomination.SuccessorMemberId}
		}},
		// the countersignature preimage of MASTER section 11. Both of its variable fields are
		// runs a caller owns -- a group id off a group context and a member id off a role list --
		// and the preimage it answers is handed to a signer, so a result that VIEWED either
		// argument would be a preimage whose content changed between signing and verifying.
		{name: "successionPreimage", call: func(take func([]byte) []byte) [][]byte {
			preimage, err := successionPreimage(take([]byte("gid")), 7, take([]byte("sid")), 42)
			if err != nil {
				t.Fatalf("successionPreimage: %v", err)
			}
			return [][]byte{preimage}
		}},
		// p7 task 15's welcome builder. Every run it is handed belongs to its caller -- the
		// epoch's joiner and welcome secrets, one joiner's path secret, and every octet of the
		// group info and of the key package the entry is addressed to -- and a builder that
		// wrote into any of them would be one whose caller's staged epoch changed as a side
		// effect of assembling a message.
		//
		// Only the two DETERMINISTIC answers are returned. Each group secrets entry
		// encapsulates to a fresh ephemeral, so two calls differ there BY DESIGN and returning
		// a ciphertext would fail this gate's "answered %x and then %x for one input" rule
		// against a correct implementation; the sealed group info and the key package
		// reference are what two calls over one input must agree on.
		{name: "BuildWelcome", call: func(take func([]byte) []byte) [][]byte {
			// the key package is minted ONCE for the whole gate, above, and only its init key
			// is handed through take. NewKeyPackage draws fresh keys and stamps a lifetime off
			// the wall clock, so a row that minted one per call would answer a different
			// KeyPackageRef each time and fail this gate's "answered %x and then %x for one
			// input" rule against a correct builder.
			joiner := welcomeRowKeyPackage
			joiner.InitKey = HpkePublicKey(take(bytes.Clone(welcomeRowKeyPackage.InitKey)))
			welcome, welcomeErr := BuildWelcome(crypto, params.Suite, &GroupInfo{
				GroupContext: GroupContext{
					Version:                 ProtocolVersionMls10,
					CipherSuite:             params.Suite,
					GroupId:                 take([]byte("the group this welcome row describes")),
					Epoch:                   3,
					TreeHash:                take(bytes.Repeat([]byte{0x51}, params.Nh)),
					ConfirmedTranscriptHash: take(bytes.Repeat([]byte{0x52}, params.Nh)),
				},
				ConfirmationTag: take(bytes.Repeat([]byte{0x53}, params.Nh)),
			}, take(bytes.Repeat([]byte{0x54}, params.Nh)), take(bytes.Repeat([]byte{0x55}, params.Nh)),
				[]WelcomeJoiner{{
					KeyPackage: joiner,
					LeafIndex:  1,
					PathSecret: take(bytes.Repeat([]byte{0x56}, params.Nh)),
				}})
			if welcomeErr != nil {
				t.Fatalf("BuildWelcome: %v", welcomeErr)
			}
			return [][]byte{welcome.EncryptedGroupInfo, welcome.Secrets[0].NewMember}
		}},
		// p7 task 19's restore. It is handed a caller's arrays in three places -- the group id
		// its config names, the leaf keys body that config carries, and the signing key it
		// rebuilds a verified context with -- and it READS all three: the id keys the state
		// lookup, and the signing key signs a group info this function then verifies against the
		// restored tree. A restore that normalised or wrote through any of them would be editing
		// storage its caller is about to found or restore a second group with.
		//
		// What it ANSWERS here is deliberately not the epoch authenticator. The group it restores
		// is founded by this row, and NewGroup samples its epoch secret, so a comparison across
		// the two calls would be comparing two different epochs; the three values below are a
		// function of the arrays this row handed in and of nothing drawn.
		{name: "LoadGroup", call: func(take func([]byte) []byte) [][]byte {
			identity := []byte("the creator whose state this row restores")
			policy := &GroupPolicyExtension{Roles: []RoleEntry{{MemberId: identity, Role: RoleOwner}}}
			if canonicalErr := policy.Canonicalize(); canonicalErr != nil {
				t.Fatalf("Canonicalize the policy this row's group carries: %v", canonicalErr)
			}
			policyExt, policyErr := policy.Encode()
			if policyErr != nil {
				t.Fatalf("Encode the policy this row's group carries: %v", policyErr)
			}
			cfg := &GroupConfig{
				Suite:      CipherSuiteX25519ChaCha20Sha256Ed25519,
				GroupId:    take([]byte("group-1")),
				Extensions: []Extension{policyExt},
				Crypto:     crypto,
				Store:      newTestStore(),
				LeafKeys: LeafKeysExtension{
					AlgId:          AlgIdXwing,
					DeviceXwingPub: take(bytes.Repeat([]byte{0x62}, XwingPublicKeyLen)),
				},
			}
			signer := SignaturePrivateKey(take(bytes.Repeat([]byte{0x54}, 32)))
			founded, foundErr := NewGroup(cfg, signer,
				Credential{CredentialType: CredentialTypeBasic, Identity: take(identity)})
			if foundErr != nil {
				t.Fatalf("found the group this row restores: %v", foundErr)
			}
			epoch := founded.Epoch()
			founded.Close()
			group, loadErr := LoadGroup(cfg, epoch, signer)
			if loadErr != nil {
				t.Fatalf("LoadGroup: %v", loadErr)
			}
			defer group.Close()
			leaf := group.OwnLeafNodeCopy()
			if leaf == nil {
				t.Fatal("the group this row restored holds no leaf at its own index")
			}
			members := group.Members()
			if len(members) != 1 || members[0].LeafKeys == nil {
				t.Fatalf("the group this row restored answers %d members; the leaf_keys body is one of the arrays it is read for",
					len(members))
			}
			return [][]byte{group.GroupId(), leaf.Credential.Identity, leaf.SignatureKey,
				members[0].LeafKeys.DeviceXwingPub}
		}},
		// the sibling key package constructor, which is in this class because of ONE parameter:
		// signer is a caller's byte run, and it is the device's long-term MLS leaf signature key.
		// A constructor that normalised, padded or wrote through it would be editing the identity
		// its caller signs every other group's leaves with.
		//
		// The leaf's signature_key is the one answer read, and it is read rather than the key
		// package's own storage for two reasons that both matter here. It is a pure function of
		// the array this row handed in, so two calls agree even though the constructor draws two
		// fresh HPKE pairs and stamps a wall clock lifetime into what it signs; and it is the
		// value the whole constructor exists to bind, so a row that answered anything else would
		// pass over a body that read the signer and then published somebody else's key.
		{name: "NewKeyPackageWithSigner", call: func(take func([]byte) []byte) [][]byte {
			kp, _, _, kpErr := NewKeyPackageWithSigner(crypto, params.Suite,
				SignaturePrivateKey(take(bytes.Repeat([]byte{0x2f}, 32))),
				BasicCredential(take([]byte("the device this key package names"))),
				testCapabilities(), nil)
			if kpErr != nil {
				t.Fatalf("NewKeyPackageWithSigner: %v", kpErr)
			}
			return [][]byte{kp.LeafNode.SignatureKey}
		}},
		{name: "compareMemberIds", call: func(take func([]byte) []byte) [][]byte {
			compareMemberIds(take([]byte{0x01, 0x02}), take([]byte{0x01, 0x03}))
			return nil
		}},
		{name: "sameMemberId", call: func(take func([]byte) []byte) [][]byte {
			sameMemberId(take([]byte{0x01, 0x02}), take([]byte{0x01, 0x02}))
			return nil
		}},
	} {
		covered = append(covered, testCase.name)
		recorder := &argumentRecorder{}
		// with a panic caught rather than taken, for the reason recoveringRow gives: a row
		// that panics on inputs this test chose would otherwise take the test binary down,
		// and every gate declared after this one with it.
		first, raised := recoveringRow(func() [][]byte { return testCase.call(recorder.take) })
		if raised != nil {
			t.Errorf("%s panicked with %v rather than answering", testCase.name, raised)
			continue
		}
		if len(recorder.arrays) == 0 {
			t.Errorf("%s was handed nothing, so this row observed nothing", testCase.name)
			continue
		}
		if changed := recorder.changed(); len(changed) != 0 {
			t.Errorf("%s changed the storage behind arguments %v of the %d it was handed",
				testCase.name, changed, len(recorder.arrays))
		}
		_, answersNoBytes := packageConstructionsAnsweringNoBytes[testCase.name]
		if answersNoBytes {
			if len(first) != 0 {
				t.Errorf("%s is named as answering no bytes and answered %d results", testCase.name, len(first))
			}
			continue
		}
		if len(first) == 0 {
			t.Errorf("%s answered with nothing, so this row observed nothing", testCase.name)
			continue
		}
		second, raised := recoveringRow(func() [][]byte { return testCase.call((&argumentRecorder{}).take) })
		if raised != nil {
			t.Errorf("%s answered once and then panicked with %v", testCase.name, raised)
			continue
		}
		if len(second) != len(first) {
			t.Errorf("%s answered %d results and then %d for one input", testCase.name, len(first), len(second))
			continue
		}
		for i, answer := range first {
			if len(answer) == 0 {
				t.Errorf("%s answered nothing in result %d, so that result observed nothing", testCase.name, i)
				continue
			}
			if recorder.aliases(answer) {
				t.Errorf("%s answered result %d over one of its arguments", testCase.name, i)
			}
			if !bytes.Equal(answer, second[i]) {
				t.Errorf("%s answered %x and then %x in result %d for one input",
					testCase.name, answer, second[i], i)
			}
			if len(second[i]) != 0 && &answer[0] == &second[i][0] {
				t.Errorf("two calls to %s answered result %d out of the same array", testCase.name, i)
			}
		}
	}
	// and the table names every construction this package hands a caller's bytes rather
	// than the ones this test happened to think of
	declared := packageLevelFunctionsTakingCallerBytes(t)
	want := []string{}
	for _, name := range declared {
		if _, isExcused := packageConstructionsOverBorrowedBytes[name]; !isExcused {
			want = append(want, name)
		}
	}
	slices.Sort(covered)
	if !slices.Equal(covered, want) {
		t.Errorf("this gate covers %v, and the package hands bytes to %v", covered, want)
	}
	for name := range packageConstructionsOverBorrowedBytes {
		if !slices.Contains(declared, name) {
			t.Errorf("the gate excuses %s, which no construction of this package declares", name)
		}
	}
	// The no-bytes table is read by TWO gates, so its staleness check is held against both
	// classes rather than against this one alone. This gate's class is the constructions handed a
	// caller's bytes; the KDF.Nh differential next door reads the same table over the
	// constructions handed a PROVIDER, and a member of that class answering no bytes -- section
	// 7.3's commit door is one -- takes no caller bytes at all. Held against this class alone, a
	// perfectly live entry is reported as naming a construction the package does not declare, and
	// the repair somebody reaches for is deleting it, which turns the other gate's "answered
	// nothing, so this row compared no length" back on.
	construction := slices.Concat(declared, packageLevelFunctionsTaking(t, providerInterfaceName))
	for name := range packageConstructionsAnsweringNoBytes {
		if !slices.Contains(construction, name) {
			t.Errorf("%s is named as answering no bytes, and no construction of this package declares it", name)
		}
	}
}

// The byte slice type scan reads a name as the storage it is.
//
// Every construction of hpke.go takes its keys under a name rather than as a []byte, so a
// scan that read only the spelling []byte would drop them out of the class above — and a
// class that shrank is a gate that demands less while reporting exactly what it reported
// before. The control names a byte slice, a slice of something else, an array of bytes and
// a name for a name, so a scan that matched on the word byte alone fails here.
func TestTheByteSliceTypeScanReadsNamedStorage(t *testing.T) {
	parsed := mustParseText(t, "the named storage control", namedByteSliceControl)
	found := packageByteSliceTypeNamesIn(parsed)
	slices.Sort(found)
	if want := []string{"AnotherKey", "SomeKey"}; !slices.Equal(found, want) {
		t.Errorf("the byte slice type scan read %v out of the control, want %v", found, want)
	}
	// and the real package's named storage is read the same way
	if named := packageByteSliceTypeNames(t); !slices.Contains(named, "HpkePublicKey") {
		t.Errorf("the byte slice type scan read %v out of this package, which names HpkePublicKey", named)
	}
}

// Four type declarations, one of which is a byte slice under another name and three of
// which are not. Every byte slice scan above runs on this as well, so one that started
// matching a fixed array or a slice of something else fails here rather than widening the
// class in silence.
const namedByteSliceControl = `package mls

type SomeKey []byte

type AnotherKey []byte

type NotAKey []uint16

type NotASlice [32]byte

type NotStorage SomeKey
`

// The recorder can see the smallest write a construction makes into its caller's array.
//
// Every immutability gate in this package is only as good as what the recorder fills the
// spare capacity with. Fill it with zeros and the commonest defect of the class — an
// append that saves an allocation by writing a zero byte past the caller's length —
// changes nothing the recorder can compare, and every row of every gate above reports the
// clean run it reports today. Measured: with the pattern replaced by zeros, a scribble
// into hpkeDecap's kem output survives the whole package.
//
// The aliasing half has the same shape. A result cut from the middle of a caller's array
// shares that array's storage exactly as one cut from the front does, and a check that
// compared only against each argument's first byte would call it fresh.
func TestTheArgumentRecorderSeesTheSmallestWrite(t *testing.T) {
	appended := &argumentRecorder{}
	argument := appended.take(bytes.Repeat([]byte{0x11}, 8))
	_ = append(argument, 0x00)
	if changed := appended.changed(); len(changed) != 1 {
		t.Errorf("the recorder reported %v changed after a zero was appended into the spare capacity behind its one argument, want the argument",
			changed)
	}
	untouched := &argumentRecorder{}
	untouched.take(bytes.Repeat([]byte{0x11}, 8))
	if changed := untouched.changed(); len(changed) != 0 {
		t.Errorf("the recorder reported %v changed after nothing was written, so it cannot tell a write from a read", changed)
	}
	aliasing := &argumentRecorder{}
	borrowed := aliasing.take(bytes.Repeat([]byte{0x11}, 8))
	if !aliasing.aliases(borrowed[4:]) {
		t.Errorf("the recorder read a result cut from the middle of its argument as storage of its own")
	}
	if aliasing.aliases(bytes.Repeat([]byte{0x11}, 8)) {
		t.Errorf("the recorder read a fresh array holding the same bytes as one of its arguments")
	}
}

// The seal draws its ephemeral key from the provider reader and from nothing else.
//
// A seal that reached for crypto/rand behind the caller back seals correctly, opens
// correctly, matches every published message and round trips forever: an ephemeral x25519
// key is an ephemeral x25519 key, and no ciphertext says which stream produced it. So
// nothing in this package can see the difference by comparing answers, and two of its
// enumeration gates quietly stop being able to see anything at all, because a construction
// whose answer differs on every call over one provider is a row that separates nothing.
//
// What says otherwise is the stream. Two providers over one script must agree, the
// encapsulated key must be the public key of the script own first Nsk bytes, and the next
// draw must come from where the seal left off — a seal over the process source fails all
// three, and a seal that consumed the wrong number of bytes fails the third.
//
// This is the property NewCryptoProviderWithRandom exists for, and the reason
// SignatureKeyPair reads self.random rather than calling ed25519.GenerateKey.
func TestProviderHpkeSealDrawsFromItsOwnReader(t *testing.T) {
	params, err := LookupSuite(CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("look up the suite: %v", err)
	}
	script := bytes.Repeat(randomScript(t), 4)
	first := mustProviderOver(t, CipherSuiteX25519ChaCha20Sha256Ed25519, bytes.NewReader(script))
	second := mustProviderOver(t, CipherSuiteX25519ChaCha20Sha256Ed25519, bytes.NewReader(script))
	_, pub, err := first.DeriveKeyPair(bytes.Repeat([]byte{0x23}, 32))
	if err != nil {
		t.Fatalf("DeriveKeyPair: %v", err)
	}
	firstKemOutput, firstCiphertext, err := first.HpkeSeal(pub, []byte("info"), nil, []byte("plaintext"))
	if err != nil {
		t.Fatalf("HpkeSeal: %v", err)
	}
	secondKemOutput, secondCiphertext, err := second.HpkeSeal(pub, []byte("info"), nil, []byte("plaintext"))
	if err != nil {
		t.Fatalf("HpkeSeal over the second provider: %v", err)
	}
	if !bytes.Equal(firstKemOutput, secondKemOutput) || !bytes.Equal(firstCiphertext, secondCiphertext) {
		t.Fatalf("two providers over one script sealed %x/%x and %x/%x",
			firstKemOutput, firstCiphertext, secondKemOutput, secondCiphertext)
	}
	// and the encapsulated key is the public key of the bytes the script opens with, so a
	// seal drawing the right count of bytes from the wrong place fails here as well
	ephemeral, err := X25519PrivateKey(script[:params.Nsk])
	if err != nil {
		t.Fatalf("read the ephemeral key the script opens with: %v", err)
	}
	if want := ephemeral.PublicKey().Bytes(); !bytes.Equal(firstKemOutput, want) {
		t.Errorf("the seal encapsulated %x, and the script first %d bytes are the private key of %x",
			firstKemOutput, params.Nsk, want)
	}
	// and the stream is left where the seal stopped rather than where it started, which is
	// what says the bytes were consumed rather than peeked at
	if drawn := first.Random(32); !bytes.Equal(drawn, script[params.Nsk:params.Nsk+32]) {
		t.Errorf("the draw after a seal read %x, and the script continues %x",
			drawn, script[params.Nsk:params.Nsk+32])
	}
	// and a second seal over the same provider is a second encapsulation, so nothing here
	// is satisfied by a seal that answers one fixed message
	nextKemOutput, _, err := first.HpkeSeal(pub, []byte("info"), nil, []byte("plaintext"))
	if err != nil {
		t.Fatalf("HpkeSeal a second time: %v", err)
	}
	if bytes.Equal(firstKemOutput, nextKemOutput) {
		t.Errorf("two seals over one provider encapsulated the same key %x", nextKemOutput)
	}
}

// The published key pairs the provider must reproduce. The corpus holds one entry per
// registered suite and each publishes two derivations, the recipient key and the ephemeral
// one, so a loader that dropped an entry or a field fails here rather than comparing less.
const providerDeriveKeyPairComparisons = 4

// The provider derives the key pair its input keying material names and not one of some
// other input.
//
// Nothing else in this package can see it. Every other test derives a pair and then uses
// both halves consistently, so a method that hashed its ikm first, or derived under the
// whole suite identifier rather than the kem one, answers with a perfectly good key pair
// and round trips forever. Measured: one such mutant survived every behavioural test in
// this package and was caught only by reading the source. What separates them is a
// published pair, which RFC 9180 gives twice per entry.
//
// The free function beside it is held to the same corpus by hpke_vectors_test.go. Both are
// pinned rather than one, because a method that stopped delegating is exactly the kind of
// edit a delegation invites, and the free function agreeing with the corpus says nothing
// about what the interface answers.
func TestProviderDeriveKeyPairMatchesTheRfcVectors(t *testing.T) {
	compared := 0
	for _, vector := range loadHpkeVectors(t) {
		crypto := mustProvider(t, vector.suite)
		for _, testCase := range []struct {
			name string
			ikm  string
			priv string
			pub  string
		}{
			{name: "the recipient key", ikm: vector.IkmR, priv: vector.SkRm, pub: vector.PkRm},
			{name: "the ephemeral key", ikm: vector.IkmE, priv: vector.SkEm, pub: vector.PkEm},
		} {
			compared++
			what := vector.name + ", " + testCase.name
			priv, pub, err := crypto.DeriveKeyPair(mustDecodeHex(t, what+" ikm", testCase.ikm))
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			if want := mustDecodeHex(t, what+" private key", testCase.priv); !bytes.Equal(priv, want) {
				t.Errorf("%s private key = %x, want %x", what, priv, want)
			}
			if want := mustDecodeHex(t, what+" public key", testCase.pub); !bytes.Equal(pub, want) {
				t.Errorf("%s public key = %x, want %x", what, pub, want)
			}
		}
	}
	if compared != providerDeriveKeyPairComparisons {
		t.Fatalf("compared %d published key pairs, want %d", compared, providerDeriveKeyPairComparisons)
	}
}

// One declared parameter of an operation this gate calls, as the source writes it. The
// name is what an argument is resolved by and the type is what it is converted to, so a
// parameter the source renames or retypes stops resolving rather than quietly keeping
// the value the old spelling had.
type providerParameter struct {
	name     string
	typeName string
}

// One operation a caller reaches through the provider: an interface method bound to one,
// or a package level construction handed one. Both are called through reflect, so the
// argument list is built from the declaration rather than written out per call site.
type providerOperation struct {
	name       string
	parameters []providerParameter
	// The operation over one provider, ready to Call. For a method this is the bound
	// method; for a construction it is the function, whose CryptoProvider parameter the
	// argument builder fills with the same provider.
	bind func(crypto CryptoProvider) reflect.Value
}

// The parameters of one declared signature, refusing an unnamed one rather than skipping
// it. An argument is resolved by name here, so a parameter with no name is a parameter
// this gate would silently hand a zero value to — and a zero argument is exactly the
// input a stub is indistinguishable under.
func parametersOf(t *testing.T, parsed parsedSource, owner string, signature *ast.FuncType) []providerParameter {
	t.Helper()
	parameters := []providerParameter{}
	for _, field := range signature.Params.List {
		if len(field.Names) == 0 {
			t.Fatalf("a parameter of %s is unnamed, so this gate cannot resolve an argument for it", owner)
		}
		for _, name := range field.Names {
			if name.Name == "_" {
				t.Fatalf("a parameter of %s is written _, so this gate cannot resolve an argument for it", owner)
			}
			parameters = append(parameters, providerParameter{name: name.Name, typeName: parsed.render(field.Type)})
		}
	}
	return parameters
}

// The interface's own methods, with the parameter names the declaration gives them.
//
// The file holding the declaration is found rather than named. A gate that opens one
// filename is the defect task 12 paid for, and it is silent: the gate finds its subject,
// reads it, and reports the clean run a complete gate reports while the declaration has
// moved next door. Exactly one file may declare it, so a second declaration is a failure
// rather than a coin toss over which one is read.
//
// The result is cross checked against reflect. The parse tree carries the names and
// reflect carries the types the calls are actually made through; a method in one and not
// the other means this gate is calling a surface that is not the one the compiler sees.
func providerInterfaceMethods(t *testing.T) []providerOperation {
	t.Helper()
	methods := []providerOperation{}
	declaring := []string{}
	for _, path := range packageLevelFunctions(t).files {
		parsed := mustParseSource(t, path)
		for _, declaration := range parsed.file.Decls {
			types, isTypeDeclaration := declaration.(*ast.GenDecl)
			if !isTypeDeclaration || types.Tok != token.TYPE {
				continue
			}
			for _, specification := range types.Specs {
				named, isNamed := specification.(*ast.TypeSpec)
				if !isNamed || named.Name.Name != providerInterfaceName {
					continue
				}
				declared, isInterface := named.Type.(*ast.InterfaceType)
				if !isInterface {
					t.Fatalf("%s in %s is not an interface", providerInterfaceName, path)
				}
				declaring = append(declaring, path)
				for _, field := range declared.Methods.List {
					signature, isSignature := field.Type.(*ast.FuncType)
					if !isSignature || len(field.Names) != 1 {
						t.Fatalf("%s in %s holds an embedded member this gate cannot read", providerInterfaceName, path)
					}
					name := field.Names[0].Name
					methods = append(methods, providerOperation{
						name:       name,
						parameters: parametersOf(t, parsed, name, signature),
						bind: func(crypto CryptoProvider) reflect.Value {
							return reflect.ValueOf(crypto).MethodByName(name)
						},
					})
				}
			}
		}
	}
	if len(declaring) != 1 {
		t.Fatalf("%d files of this package declare %s, want exactly 1: %v", len(declaring), providerInterfaceName, declaring)
	}
	read := []string{}
	for _, method := range methods {
		read = append(read, method.name)
	}
	slices.Sort(read)
	if !slices.Equal(read, providerMethodNames()) {
		t.Fatalf("the parse tree reads %v out of %s and the compiler sees %v",
			read, providerInterfaceName, providerMethodNames())
	}
	return methods
}

// The value each package level construction is called through.
//
// A construction cannot be called through a name the way a method can, so each one needs a
// value written down. This list is not what decides the class -- the class is read off the
// type checker below, and a construction with no value here is a failure rather than a row
// left out -- and it is not trusted to be what it says either: assertIsTheFunctionNamed
// holds every value to the function its key names.
var providerConstructionValues = map[string]any{
	"RefHash":                       RefHash,
	"MakeKeyPackageRef":             MakeKeyPackageRef,
	"MakeProposalRef":               MakeProposalRef,
	"EncryptWithLabel":              EncryptWithLabel,
	"DecryptWithLabel":              DecryptWithLabel,
	"SealWithLabel":                 SealWithLabel,
	"OpenWithLabel":                 OpenWithLabel,
	"ZeroSecret":                    ZeroSecret,
	"DeriveJoinerSecret":            DeriveJoinerSecret,
	"NewKeySchedule":                NewKeySchedule,
	"NewKeyScheduleFromJoiner":      NewKeyScheduleFromJoiner,
	"NewKeyScheduleFromEpochSecret": NewKeyScheduleFromEpochSecret,
	"newKeyScheduleFromParts":       newKeyScheduleFromParts,
	"WelcomeKeyNonce":               WelcomeKeyNonce,
	"BuildWelcome":                  BuildWelcome,
	"PskSecret":                     PskSecret,
	"EmptyPskSecret":                EmptyPskSecret,
	"ConfirmedTranscriptHash":       ConfirmedTranscriptHash,
	"InterimTranscriptHash":         InterimTranscriptHash,
	"NewSecretTree":                 NewSecretTree,
	"SenderDataKeyNonce":            SenderDataKeyNonce,
	"NewLeafNode":                   NewLeafNode,
	"NewKeyPackage":                 NewKeyPackage,
	"NewKeyPackageWithSigner":       NewKeyPackageWithSigner,
	"DerivePathSecrets":             DerivePathSecrets,
	"DeriveNodeKeyPair":             DeriveNodeKeyPair,
	"SignAuthenticatedContent":      SignAuthenticatedContent,
	"ValidateUpdatePathLeafNode":    ValidateUpdatePathLeafNode,
	"VerifyAuthenticatedContent":    VerifyAuthenticatedContent,
	"ComputeMembershipTag":          ComputeMembershipTag,
	"verifyMembershipTag":           verifyMembershipTag,
	"SealPublicMessage":             SealPublicMessage,
	"OpenPublicMessage":             OpenPublicMessage,
	"sealSenderData":                sealSenderData,
	"openSenderData":                openSenderData,
	"SealPrivateMessage":            SealPrivateMessage,
	"sealPrivateMessage":            sealPrivateMessage,
	"sealPrivateMessageAt":          sealPrivateMessageAt,
	"sealPrivateMessageBound":       sealPrivateMessageBound,
	"OpenPrivateMessage":            OpenPrivateMessage,
	"openPrivateMessageAt":          openPrivateMessageAt,
	"PeekPrivateMessageSender":      PeekPrivateMessageSender,
}

// The name of the interface every gate in this file is written about, in one place so a
// rename is one edit rather than a matcher that stops matching.
const providerInterfaceName = "CryptoProvider"

// The value written for one construction is the function its key names, read off the
// compiled program rather than taken on trust.
//
// The map above is this gate's one hand written line. Its keys are held to the parse tree
// in both directions and its values are held to nothing, so two rows pointing at one
// sibling function report full coverage of both while one of them is never called.
// Measured, against the version this replaces: with MakeKeyPackageRef and MakeProposalRef
// swapped, every one of this package's 2287 tests passed; and with one of them aliased to
// the other beside a MakeProposalRef reduced to a placeholder, both halves of this gate
// passed and the placeholder was found only by the published commit vectors.
func assertIsTheFunctionNamed(t *testing.T, name string, implementation any) {
	t.Helper()
	value := reflect.ValueOf(implementation)
	if value.Kind() != reflect.Func {
		t.Fatalf("providerConstructionValues maps %s to a %s rather than to a function", name, value.Kind())
	}
	compiled := runtime.FuncForPC(value.Pointer())
	if compiled == nil {
		t.Fatalf("the value providerConstructionValues maps %s to is not a compiled function", name)
	}
	if !strings.HasSuffix(compiled.Name(), "."+name) {
		t.Fatalf("providerConstructionValues maps %s to %s, so the row written for %s calls something else",
			name, compiled.Name(), name)
	}
}

// No parameter of this package's non test source carries a provider to a position this
// gate cannot hand one to.
//
// The class below is the parameters whose type is the provider, which is the position a
// caller normally writes and is not the only one a provider arrives through. A variadic
// ...CryptoProvider is a slice to the compiler and its own spelling to a renderer, so it
// sits in neither the class nor the package level scan the class is cross checked against,
// and the two agree on a shrunken class exactly as they agree on a complete one. Measured,
// against the version this replaces: a variadic construction added to crypto.go passed all
// 2287 tests of this package while being called by nothing.
//
// Three readings, none of them the source spelling. sameTypeAs is the compiler's own
// identity, which sees through an alias and through a defined type over the interface.
// types.Implements is what catches a wider interface that embeds the provider and the
// concrete type that satisfies it. And the type checker's canonical rendering of a
// composite carries the provider's own qualified name, which is what catches a slice, a
// map, a pointer or a variadic of them; the underlying type is rendered as well, so it
// takes two levels of naming to hide one. The residual is that third level, and it is a
// spelling nothing in this package has any reason to write.
func assertNoProviderArrivesUncallable(t *testing.T, function declaredFunction) {
	t.Helper()
	provider := providerInterfaceType(t)
	declared, isInterface := provider.Underlying().(*types.Interface)
	if !isInterface {
		t.Fatalf("%s is not an interface, so the class below is read off nothing", providerInterfaceName)
	}
	callable := sameTypeAs(provider)
	spelled := types.TypeString(provider, nil)
	for i := 0; i < function.signature.Params().Len(); i++ {
		at := function.signature.Params().At(i).Type()
		if callable(at) {
			continue
		}
		if !types.Implements(at, declared) &&
			!strings.Contains(types.TypeString(at, nil), spelled) &&
			!strings.Contains(types.TypeString(at.Underlying(), nil), spelled) {
			continue
		}
		t.Fatalf("%s takes %s at position %d, which the compiler reads as %s and which carries a %s to a position this gate cannot fill",
			function.name, function.parameters[i].typeName, i, at, providerInterfaceName)
	}
}

// The package level constructions handed a provider, by the same reading.
//
// A method is skipped here for the reason the comment on declaredFunction.method gives, and
// the methods this package hands a provider are held by provider_methods_test.go instead.
// That file's TestEveryDeclarationTakingAProviderIsHeldByExactlyOneOfTheTwoClasses compares
// the two classes against the whole, so a method taking a provider that nothing runs is a
// failure there rather than a shorter class here.
//
// A construction here cannot be called through a name the way a method can, so each one
// needs a value written down. That list is not what decides the class: the class is read
// off the type checker, and a construction with no value is a failure rather than a row
// left out. A later task adding a sixth construction that takes a provider fails here
// until somebody writes its name, which is the point.
//
// The class is the compiler's reading of the signature rather than the spelling the source
// gives the parameter, which is what makes an interface embedding the provider a member of
// it. The two are then cross checked against each other: the spelling based package level
// scan must name the same five, so a construction visible to one reading and not the other
// stops this gate rather than being dropped by both.
func providerConstructions(t *testing.T) []providerOperation {
	t.Helper()
	provider := providerInterfaceType(t)
	constructions := []providerOperation{}
	found := []string{}
	for _, function := range declaredFunctionsOf(t, cryptoOwnRoot) {
		if function.method {
			continue
		}
		assertNoProviderArrivesUncallable(t, function)
		if len(function.takes(provider)) == 0 {
			continue
		}
		name := function.name
		implementation, listed := providerConstructionValues[name]
		if !listed {
			t.Fatalf("%s takes a %s and providerConstructionValues holds no value for it, so this gate cannot call it",
				name, providerInterfaceName)
		}
		assertIsTheFunctionNamed(t, name, implementation)
		found = append(found, name)
		constructions = append(constructions, providerOperation{
			name:       name,
			parameters: function.parameters,
			bind: func(crypto CryptoProvider) reflect.Value {
				return reflect.ValueOf(implementation)
			},
		})
	}
	slices.Sort(found)
	if !slices.Equal(found, packageLevelFunctionsTaking(t, providerInterfaceName)) {
		t.Fatalf("this gate reads %v as the constructions taking a %s and the package level scan reads %v",
			found, providerInterfaceName, packageLevelFunctionsTaking(t, providerInterfaceName))
	}
	for name := range providerConstructionValues {
		if !slices.Contains(found, name) {
			t.Errorf("providerConstructionValues names %s, which is not a construction of this package taking a %s",
				name, providerInterfaceName)
		}
	}
	return constructions
}

// Every operation this gate calls, both surfaces together.
func providerOperations(t *testing.T) []providerOperation {
	t.Helper()
	return slices.Concat(providerInterfaceMethods(t), providerConstructions(t))
}

// The byte stream every provider in this gate draws from. It is a reader over a fixed
// script rather than a constant, because a constant source cannot separate a Random that
// returns what it read from one that sorts, reverses or rotates it -- the defect this
// package shipped once already.
//
// Every call the gate makes is made over a provider built for that call alone. Two calls
// through one provider are two different questions once anything has drawn: the second
// reads the stream on from where the first left it and answers differently whatever its
// arguments were, so a row comparing them would be satisfied by the entropy rather than by
// the argument that moved. Measured, against the version this replaces: a HpkeSeal that
// discarded its plaintext argument outright left both halves of this gate passing.
func providerStubStream(first byte) io.Reader {
	return bytes.NewReader(ascendingBytes(first, 4096))
}

// The base arguments every operation is called with, keyed by the spelling that resolves
// them: a declared type name where the type says which value belongs there, and a
// parameter name where it does not. The lengths come from the registry rather than from
// literals or from the provider, so a suite whose key or nonce is a different size is
// called correctly, and a size method that is itself a stub cannot shrink the arguments
// every other row is built out of.
//
// Four parameter names are deliberately absent from the generic half. A ciphertext, a
// kemOutput, a tag and a sig are answers rather than inputs, and the same spelling means a
// different value in each operation that receives one. Each is written per operation
// below, so a later method taking one of them fails to resolve rather than being handed
// another operation's answer, which would fail for a reason that is not the one this gate
// is about.
// providerStubKeyPackage is the published key package the welcome row is addressed to.
//
// It is minted over a STREAM OF ITS OWN rather than over the provider this gate is about to
// measure. NewKeyPackage draws three times, so building an argument through the subject would
// move the window every later row is counted in -- which is the shape this gate's own comment
// warns about one function down, read back onto the arguments rather than onto the calls.
//
// A real key package and not a hand assembled one, because (*KeyPackage).Ref encodes the whole
// structure: a zero KeyPackage carries credential type 0, which Credential.MarshalMLS refuses,
// so BuildWelcome over one would answer an error and this gate would report the refusal as the
// zero answer a stub gives.
func providerStubKeyPackage(t *testing.T, params *SuiteParams) KeyPackage {
	t.Helper()
	minting := mustProviderOver(t, params.Suite, providerStubStream(0x11))
	kp, _, _, err := NewKeyPackage(minting, params.Suite,
		BasicCredential(ascendingBytes(0xc6, 16)), testCapabilities(), nil)
	if err != nil {
		t.Fatalf("mint the key package the welcome row is addressed to: %v", err)
	}
	return *kp
}

func providerStubArguments(t *testing.T, params *SuiteParams, crypto CryptoProvider) map[string]any {
	t.Helper()
	arguments := map[string]any{
		providerInterfaceName:  crypto,
		"data":                 ascendingBytes(0x10, 48),
		"key":                  ascendingBytes(0x20, params.Nk),
		"nonce":                ascendingBytes(0x30, params.Nn),
		"salt":                 ascendingBytes(0x40, 40),
		"ikm":                  ascendingBytes(0x50, 32),
		"prk":                  ascendingBytes(0x60, params.Nh),
		"secret":               ascendingBytes(0x70, params.Nh),
		"info":                 ascendingBytes(0x80, 24),
		"aad":                  ascendingBytes(0x90, 16),
		"plaintext":            ascendingBytes(0xa0, 20),
		"content":              ascendingBytes(0xb0, 28),
		"context":              ascendingBytes(0xc0, 36),
		"value":                ascendingBytes(0xd0, 44),
		"keyPackage":           ascendingBytes(0xe0, 52),
		"authenticatedContent": ascendingBytes(0xf0, 60),
		// the key schedule's two secrets, at exactly KDF.Nh because DeriveJoinerSecret
		// refuses every other length, and distinct so a row that read one of them twice
		// is not answered by the other
		"initSecretPrev": ascendingBytes(0x11, params.Nh),
		"commitSecret":   ascendingBytes(0x22, params.Nh),
		// the epoch schedule's own four, on the same rule: exactly KDF.Nh, because every
		// constructor refuses any other length and a refused call is a row that observed
		// nothing, and all four distinct so a construction that read one of them where it
		// meant another still moves when the one it did not read is perturbed.
		"pskSecret":     ascendingBytes(0x33, params.Nh),
		"joinerSecret":  ascendingBytes(0x44, params.Nh),
		"welcomeSecret": ascendingBytes(0x55, params.Nh),
		"epochSecret":   ascendingBytes(0x66, params.Nh),
		// the assembler takes the group context already encoded rather than as a struct.
		// It is stored and handed back rather than expanded over, so any bytes will do,
		// and a length no MLS field has keeps it from being read as anything else.
		"encodedGroupContext": ascendingBytes(0x77, 37),
		// the section 8.2 transcript arithmetic's four. The two hashes and the tag are at
		// exactly KDF.Nh because that is what an interim transcript hash and a MAC are, and
		// the serialized ConfirmedTranscriptHashInput at a length no other row carries so a
		// construction that read the wrong one of them is not answered by its neighbour.
		// All four distinct, on the rule the epoch's four secrets are held to: a hash taken
		// over the right number of bytes from the wrong argument is a fork, not an error.
		"interimBefore":                ascendingBytes(0x88, params.Nh),
		"confirmedAfter":               ascendingBytes(0x99, params.Nh),
		"confirmationTag":              ascendingBytes(0xaa, params.Nh),
		"confirmedTranscriptHashInput": ascendingBytes(0xbb, 41),
		// the epoch binding the joiner derivation expands over. Every field carries
		// something, so the perturbation below has a field to move and an encoder that
		// dropped one is not hidden by that field being empty to begin with.
		"groupContext": &GroupContext{
			Version:                 ProtocolVersionMls10,
			CipherSuite:             params.Suite,
			GroupId:                 ascendingBytes(0x01, 32),
			Epoch:                   7,
			TreeHash:                ascendingBytes(0x02, params.Nh),
			ConfirmedTranscriptHash: ascendingBytes(0x03, params.Nh),
			Extensions: []Extension{
				{ExtensionType: ExtensionTypeRatchetTree, ExtensionData: ascendingBytes(0x04, 8)},
			},
		},
		// the psk list psk_secret folds. Two entries and not one, because the recurrence
		// only has an accumulator from the second step and a single entry list would let
		// a fold that dropped it past every row here. One entry per arm, so a field only
		// the resumption arm encodes has somewhere to be moved; the ids are distinct or
		// ValSem403 refuses the list before anything is derived, and the nonces are
		// exactly KDF.Nh or ValSem401 does.
		"psks": []PreSharedKeyInput{
			{
				Id: PreSharedKeyId{
					PskType:  PskTypeExternal,
					PskId:    ascendingBytes(0xa1, 16),
					PskNonce: ascendingBytes(0xa2, params.Nh),
				},
				Secret: ascendingBytes(0xa3, params.Nh),
			},
			{
				Id: PreSharedKeyId{
					PskType:    PskTypeResumption,
					Usage:      ResumptionPskUsageApplication,
					PskGroupId: ascendingBytes(0xb1, 12),
					PskEpoch:   9,
					PskNonce:   ascendingBytes(0xb2, params.Nh),
				},
				Secret: ascendingBytes(0xb3, params.Nh),
			},
		},
		// p7 task 15's welcome builder. Both of these resolve by NAME rather than by type: a
		// group info is a whole structure and a joiner list is a slice of them, so neither is a
		// run this gate's generic byte rows could stand in for.
		//
		// Every field of the group info carries something, for the group context row's reason
		// one entry up: a field left empty is one a perturbation has nothing to move, and a
		// preimage that dropped it would be reported as covered while saying nothing.
		"BuildWelcome.info": &GroupInfo{
			GroupContext: GroupContext{
				Version:                 ProtocolVersionMls10,
				CipherSuite:             params.Suite,
				GroupId:                 ascendingBytes(0xc1, 24),
				Epoch:                   11,
				TreeHash:                ascendingBytes(0xc2, params.Nh),
				ConfirmedTranscriptHash: ascendingBytes(0xc3, params.Nh),
			},
			Extensions:      []Extension{{ExtensionType: ExtensionTypeRatchetTree, ExtensionData: ascendingBytes(0xc7, 8)}},
			ConfirmationTag: ascendingBytes(0xc4, params.Nh),
			Signer:          1,
			Signature:       ascendingBytes(0xc8, 64),
		},
		// ONE joiner and not none, and that is what makes the row measure anything. With an
		// empty list BuildWelcome seals the group info and nothing else: it never reads the
		// joiner secret, never draws an ephemeral, and the perturbation of joinerSecret below
		// would report "does not read the joinerSecret it was handed" against a correct
		// implementation. One joiner is also what fixes the draw this gate counts, at one
		// ephemeral scalar; see providerStreamDraws.
		"BuildWelcome.joiners": []WelcomeJoiner{{
			KeyPackage: providerStubKeyPackage(t, params),
			LeafIndex:  1,
			PathSecret: ascendingBytes(0xc5, params.Nh),
		}},
		"label":  "stub gate label",
		"length": 32,
		"n":      32,
		// the secret tree's two. The count is a full tree because tree math refuses every
		// other shape, and the secret is exactly KDF.Nh because the constructor refuses
		// every other length -- and a refused call is a row that observed nothing. The
		// count is written as a LeafCount and not as an untyped 8: the two are one uint32
		// to the compiler, and a construction that took an index where a count belongs
		// would be handed the right number by an untyped literal and never show it.
		"leafCount":        LeafCount(8),
		"encryptionSecret": ascendingBytes(0xcc, params.Nh),
		"generation":       uint32(7),
		// the sender data pair's two, scoped to the operation so a second construction
		// with a ciphertext parameter cannot be served these bytes by accident. The secret
		// is exactly KDF.Nh because the construction refuses every other length, and a
		// refused call is a row that observed nothing.
		//
		// The CIPHERTEXT is exactly KDF.Nh as well, which is a choice this gate has to make
		// rather than one it can avoid. RFC 9420 section 6.3.2 samples the first KDF.Nh
		// bytes and no more, so a longer ciphertext would put this gate's middle and last
		// perturbations outside the sample -- where an answer that does not move is the RFC
		// working rather than a stub, and would be reported as "does not read the ciphertext
		// it was handed". At exactly KDF.Nh every byte of the argument enters the
		// derivation, which is the property this gate states. Where the sample BOUNDARY is
		// held instead is TestSenderDataKeyNonceSamplesExactlyTheFirstNhBytes, which sweeps
		// every byte of three ciphertext lengths in both directions.
		"SenderDataKeyNonce.senderDataSecret": ascendingBytes(0xdd, params.Nh),
		"SenderDataKeyNonce.ciphertext":       ascendingBytes(0xee, params.Nh),
		// the leaf constructor's three structured arguments, answered by the constructors
		// the perturbation rule for them is built out of, so the base value and the values
		// it is moved away from cannot be written twice and drift.
		// the key package constructor's ciphersuite, keyed by its DECLARED TYPE rather than
		// by the parameter name, because a CipherSuite says which value belongs there and
		// "suite" would not. It is the registry's own code point for the suite this gate is
		// running at, so a construction that stores it and signs over it is called correctly;
		// the uint16 rule moves it one code point higher, which is a suite the key package
		// names and nothing else about the call changes.
		"CipherSuite": params.Suite,
		"cred":        leafNodeStubCredential(),
		"caps":        leafNodeStubCapabilities(),
		"exts":        leafNodeStubExtensions(),
		// the path secret ladder of RFC 9420 section 7.4 and the node key under it. Both
		// are exactly KDF.Nh, because a rung of that ladder IS a KDF output and every rung
		// after the first is derived at that width -- a shorter one would still derive and
		// would put this gate's middle perturbation outside a real secret. They are distinct
		// from each other and from the six secrets above, so a construction that read one of
		// them where it meant another still moves when the one it did not read is perturbed.
		//
		// The count is three rather than one: the ladder answers count+1 rungs, so a body
		// that ignored the count entirely and answered the initial secret alone is separated
		// from one that climbs, and the perturbation -- one higher -- moves the answer by a
		// rung rather than by the whole shape of it.
		"initial":    ascendingBytes(0x12, params.Nh),
		"pathSecret": ascendingBytes(0x13, params.Nh),
		"count":      3,
	}
	// the keys and the answers the receiving operations are handed, computed over a
	// provider of this gate's own so that the operation under test still draws from a
	// stream nothing has consumed
	fixture := mustProviderOver(t, params.Suite, providerStubStream(0x01))
	signaturePriv, signaturePub, err := fixture.SignatureKeyPair()
	if err != nil {
		t.Fatalf("generate the signature key pair every row is built over: %v", err)
	}
	hpkePriv, hpkePub, err := fixture.DeriveKeyPair(arguments["ikm"].([]byte))
	if err != nil {
		t.Fatalf("derive the hpke key pair every row is built over: %v", err)
	}
	arguments["SignaturePrivateKey"] = signaturePriv
	arguments["SignaturePublicKey"] = signaturePub
	// the framing preimage's arguments. They live beside the structures they are built out of,
	// in framing_protect_test.go, for the reason the psk list's and the leaf's do: nothing
	// here knows how to build a FramedContent that encodes, and the verify row needs one that
	// has actually been signed rather than one that resembles it.
	providerStubFramingArguments(t, fixture, signaturePriv, arguments["groupContext"].(*GroupContext), arguments)
	arguments["HpkePrivateKey"] = hpkePriv
	arguments["HpkePublicKey"] = hpkePub
	arguments["MacVerify.tag"] = fixture.Mac(arguments["key"].([]byte), arguments["data"].([]byte))
	signature, err := fixture.SignWithLabel(signaturePriv, arguments["label"].(string), arguments["content"].([]byte))
	if err != nil {
		t.Fatalf("sign the content the VerifyWithLabel row reads: %v", err)
	}
	arguments["VerifyWithLabel.sig"] = signature
	sealed, err := fixture.AeadSeal(arguments["key"].([]byte), arguments["nonce"].([]byte),
		arguments["aad"].([]byte), arguments["plaintext"].([]byte))
	if err != nil {
		t.Fatalf("seal the ciphertext the AeadOpen row reads: %v", err)
	}
	arguments["AeadOpen.ciphertext"] = sealed
	kemOutput, hpkeSealed, err := fixture.HpkeSeal(hpkePub, arguments["info"].([]byte),
		arguments["aad"].([]byte), arguments["plaintext"].([]byte))
	if err != nil {
		t.Fatalf("seal the message the HpkeOpen row reads: %v", err)
	}
	arguments["HpkeOpen.kemOutput"] = kemOutput
	arguments["HpkeOpen.ciphertext"] = hpkeSealed
	labelledKemOutput, labelledCiphertext, err := EncryptWithLabel(fixture, hpkePub,
		arguments["label"].(string), arguments["context"].([]byte), arguments["plaintext"].([]byte))
	if err != nil {
		t.Fatalf("seal the message the DecryptWithLabel row reads: %v", err)
	}
	arguments["DecryptWithLabel.kemOutput"] = labelledKemOutput
	arguments["DecryptWithLabel.ciphertext"] = labelledCiphertext
	// the same message as the one structure OpenWithLabel takes, built out of the two
	// answers above rather than sealed a second time: two seals of one plaintext differ,
	// so a second one here would let the two rows open different messages while looking
	// like one fixture.
	arguments["OpenWithLabel.ct"] = &HpkeCiphertext{
		KemOutput:  labelledKemOutput,
		Ciphertext: labelledCiphertext,
	}
	// RFC 9420 section 7.3's commit door, whose three arguments have to AGREE for the base call
	// to be an ACCEPTANCE: the leaf is signed over this group id at this sender index and lists
	// this suite. A base call that already refuses answers the same refusal under every
	// perturbation, and every row over it would report a parameter nothing observes -- which is
	// this gate reporting a clean bill having compared one error against itself.
	//
	// The group id is not "context" and not any other row's bytes, because the leaf's own
	// LeafNodeTBS covers it: a shared value would tie this fixture's acceptance to a length some
	// other row is free to change.
	commitDoorStubContext := &GroupContext{
		Version: ProtocolVersionMls10, CipherSuite: params.Suite,
		GroupId: ascendingBytes(0x05, 24), Epoch: 3,
	}
	arguments["ValidateUpdatePathLeafNode.context"] = commitDoorStubContext
	arguments["ValidateUpdatePathLeafNode.sender"] = LeafIndex(1)
	arguments["ValidateUpdatePathLeafNode.path"] =
		commitPathValidUnder(t, crypto, commitDoorStubContext, LeafIndex(1))
	return arguments
}

// One parameter's base argument. The most specific spelling wins: an answer written for
// this operation, then the declared type where the type decides which value belongs, then
// the parameter name. A parameter none of the three resolves is fatal rather than skipped,
// which is what makes a method added tomorrow fail this gate without anybody editing it.
func providerStubArgument(t *testing.T, arguments map[string]any, operation string, parameter providerParameter, want reflect.Type) reflect.Value {
	t.Helper()
	for _, key := range []string{operation + "." + parameter.name, parameter.typeName, parameter.name} {
		value, resolved := arguments[key]
		if !resolved {
			continue
		}
		found := reflect.ValueOf(value)
		if found.Type() == want {
			return found
		}
		if !found.Type().ConvertibleTo(want) {
			t.Fatalf("the argument for %s.%s resolved as %s and the parameter is %s",
				operation, parameter.name, found.Type(), want)
		}
		return found.Convert(want)
	}
	t.Fatalf("no base argument resolves %s.%s, declared %s, so this gate cannot call it",
		operation, parameter.name, parameter.typeName)
	return reflect.Value{}
}

// One moved argument, with the words for where it moved, so a row that fails names the
// position rather than only the parameter.
type providerPerturbation struct {
	where string
	value reflect.Value
}

// The positions of a run of bytes this gate moves: the first, the middle and the last,
// deduplicated so a run too short to hold three is probed once rather than three times.
//
// Read off the length rather than written down. One position is a narrower property than
// the one this gate states: an operation reading only the last byte of each argument it
// was handed is a function of two of its eighty bytes, and a perturbation that only ever
// moves that last byte separates it from a complete implementation not at all. Measured,
// against the version this replaces: a mac over key[len-1] and data[len-1] and an extract
// over salt[len-1] and ikm[len-1] each passed both halves of this gate, and were caught
// only by RFC 4231 and RFC 5869.
func perturbedPositions(length int) []int {
	at := []int{0, length / 2, length - 1}
	slices.Sort(at)
	return slices.Compact(at)
}

// One argument's perturbations, each changed in the smallest way that leaves its shape
// alone.
//
// The length is preserved on purpose. An operation that reads only how many bytes it was
// handed is a stub of exactly the kind this gate is about, a correctly sized answer that is
// a function of nothing, and a perturbation that changed the length would separate it from
// a real implementation for the wrong reason. A kind with no rule here is fatal, so a
// method taking an argument this gate does not know how to move fails rather than being
// called twice with the identical value and reported as observing it.
func providerPerturbations(t *testing.T, operation string, parameter providerParameter, argument reflect.Value) []providerPerturbation {
	t.Helper()
	moved := []providerPerturbation{}
	// the leaf constructor's credential, capabilities and extensions. Two of the three are
	// structures and the third is a slice of them, so none of the rules below reaches into
	// any of them; the rule lives beside the structures it moves, in leaf_node_test.go, and
	// derives its moves off the value rather than off a field list.
	if perturbations, handled := providerLeafNodeArgumentPerturbations(t, operation, parameter, argument); handled {
		return perturbations
	}
	// the pre-ratchet peek's marshalled message, whose read region is a PREFIX of the octets it
	// is handed. The rule lives beside the structure it moves, in framing_protect_test.go.
	if perturbations, handled := providerPeekedMessagePerturbations(t, operation, parameter, argument); handled {
		return perturbations
	}
	switch argument.Kind() {
	case reflect.Slice:
		// the psk list is a slice of structs rather than of bytes, so the byte rule below
		// cannot reach into it. Its rule lives beside the structure it moves, in psk_test.go.
		if argument.Type() == reflect.TypeOf([]PreSharedKeyInput(nil)) {
			return providerPskInputPerturbations(t, operation, parameter, argument)
		}
		// p7 task 15's joiner list, a slice of structs for the same reason the psk list is. Its
		// rule lives beside the structure it moves, in welcome_test.go.
		if argument.Type() == reflect.TypeOf([]WelcomeJoiner(nil)) {
			return providerWelcomeJoinerPerturbations(t, operation, parameter, argument)
		}
		if argument.Type().Elem().Kind() != reflect.Uint8 {
			break
		}
		if argument.Len() == 0 {
			t.Fatalf("the base argument for %s.%s is empty, so perturbing it changes nothing", operation, parameter.name)
		}
		for _, at := range perturbedPositions(argument.Len()) {
			value := reflect.MakeSlice(argument.Type(), argument.Len(), argument.Len())
			reflect.Copy(value, argument)
			value.Index(at).SetUint(value.Index(at).Uint() ^ 0xff)
			moved = append(moved, providerPerturbation{
				where: "byte " + strconv.Itoa(at) + " of " + strconv.Itoa(argument.Len()),
				value: value,
			})
		}
		return moved
	case reflect.String:
		text := argument.String()
		if text == "" {
			t.Fatalf("the base argument for %s.%s is empty, so perturbing it changes nothing", operation, parameter.name)
		}
		for _, at := range perturbedPositions(len(text)) {
			letters := []byte(text)
			letters[at] ^= 0x20
			value := reflect.New(argument.Type()).Elem()
			value.SetString(string(letters))
			moved = append(moved, providerPerturbation{
				where: "character " + strconv.Itoa(at) + " of " + strconv.Itoa(len(text)),
				value: value,
			})
		}
		return moved
	case reflect.Int:
		value := reflect.New(argument.Type()).Elem()
		value.SetInt(argument.Int() + 1)
		return append(moved, providerPerturbation{where: "one higher", value: value})
	case reflect.Uint32:
		value := reflect.New(argument.Type()).Elem()
		value.SetUint(argument.Uint() + 1)
		return append(moved, providerPerturbation{where: "one higher", value: value})
	// the framing registries are sixteen bits wide, which is the width the wire format gives
	// them, and moving one to its neighbour moves it to another REGISTERED code point rather
	// than off the registry -- so a preimage that dropped the wire format answers identically
	// here and one that kept it cannot.
	case reflect.Uint16:
		value := reflect.New(argument.Type()).Elem()
		value.SetUint(argument.Uint() + 1)
		return append(moved, providerPerturbation{where: "one higher", value: value})
	case reflect.Pointer:
		// framing's two structured arguments, whose rules live beside the structures they move,
		// in framing_protect_test.go, which is where psk_test.go, leaf_node_test.go and
		// treekem_test.go keep theirs.
		if argument.Type() == reflect.TypeOf((*FramedContent)(nil)) && !argument.IsNil() {
			return providerFramedContentPerturbations(t, operation, parameter, argument)
		}
		if argument.Type() == reflect.TypeOf((*AuthenticatedContent)(nil)) && !argument.IsNil() {
			return providerAuthenticatedContentPerturbations(t, operation, parameter, argument)
		}
		if argument.Type() == reflect.TypeOf((*PublicMessage)(nil)) && !argument.IsNil() {
			return providerPublicMessagePerturbations(t, operation, parameter, argument)
		}
		// section 6.3.2's two, whose rules live beside them in framing_protect_test.go for the
		// reason the three above do.
		if argument.Type() == reflect.TypeOf((*SenderData)(nil)) && !argument.IsNil() {
			return providerSenderDataPerturbations(t, operation, parameter, argument)
		}
		if argument.Type() == reflect.TypeOf((*PrivateMessage)(nil)) && !argument.IsNil() {
			return providerPrivateMessagePerturbations(t, operation, parameter, argument)
		}
		// the labelled open's ciphertext, whose two fields are byte slices this rule cannot
		// reach through a pointer to a struct. Its rule lives beside the structure it moves,
		// in treekem_test.go, which is where psk_test.go and leaf_node_test.go keep theirs.
		if argument.Type() == reflect.TypeOf((*HpkeCiphertext)(nil)) && !argument.IsNil() {
			return providerHpkeCiphertextPerturbations(t, operation, parameter, argument)
		}
		// the commit door's update path, whose judged half is the LEAF inside it and whose rule
		// lives beside that structure in treekem_test.go for the reason the ciphertext's does.
		if argument.Type() == reflect.TypeOf((*UpdatePath)(nil)) && !argument.IsNil() {
			return providerUpdatePathPerturbations(t, operation, parameter, argument)
		}
		// p7 task 15's welcome group info, whose fields are a nested struct, a vector of
		// structs and two byte slices this rule cannot reach through a pointer. Its rule lives
		// beside the structure in welcome_test.go, which is where leaf_node_test.go,
		// psk_test.go and treekem_test.go keep theirs.
		if argument.Type() == reflect.TypeOf((*GroupInfo)(nil)) && !argument.IsNil() {
			return providerGroupInfoPerturbations(t, operation, parameter, argument)
		}
		if argument.Type() != reflect.TypeOf((*GroupContext)(nil)) || argument.IsNil() {
			break
		}
		// the commit door reads the group id, the suite and the extensions off its context and
		// deliberately does NOT read the epoch -- a LeafNodeTBS carries no epoch, so a leaf
		// validator that changed its verdict when the epoch moved would be judging a leaf by a
		// field its signature does not cover. The epoch move below is therefore the one
		// perturbation that must NOT separate this operation, and its rule moves the three
		// fields it does read instead. It lives beside the door in treekem_test.go, which is
		// where the update path's own rule is.
		if operation == "ValidateUpdatePathLeafNode" {
			return providerCommitDoorContextPerturbations(t, operation, parameter, argument)
		}
		// the epoch, because it is the field two contexts of one group differ in and
		// nothing else: an operation that dropped the group context out of its preimage
		// answers identically here, and one that kept it cannot. The copy is a Clone
		// rather than a shallow struct copy, so this perturbation cannot write through
		// into the base argument every other row is built from.
		perturbed := argument.Interface().(*GroupContext).Clone()
		perturbed.Epoch++
		return append(moved, providerPerturbation{where: "epoch one higher", value: reflect.ValueOf(perturbed)})
	case reflect.Func:
		// section 6.2's signature key resolver, whose rule lives beside the open that takes one,
		// in framing_protect_test.go.
		if argument.Type() == reflect.TypeOf(SignatureKeyResolver(nil)) && !argument.IsNil() {
			return providerSignatureKeyResolverPerturbations(t, operation, parameter, argument)
		}
	case reflect.Interface:
		// section 6.3.1's message key source, whose rule lives beside the operations that take
		// one, in framing_protect_test.go, which is where psk_test.go, leaf_node_test.go and
		// treekem_test.go keep theirs.
		if argument.Type() == reflect.TypeOf((*MessageKeySource)(nil)).Elem() && !argument.IsNil() {
			return providerMessageKeySourcePerturbations(t, operation, parameter, argument)
		}
		if argument.Type() != reflect.TypeOf((*CryptoProvider)(nil)).Elem() {
			break
		}
		// the tagging provider answers differently method by method, which
		// TestTheTaggingProviderAnswersDifferentlyThanTheRealOne holds it to, so a
		// construction that computed its answer without the provider it was handed
		// reports here as a parameter nothing observes
		wrapped := CryptoProvider(&taggingCryptoProvider{inner: argument.Interface().(CryptoProvider)})
		return append(moved, providerPerturbation{where: "wrapped in the tagging provider", value: reflect.ValueOf(wrapped)})
	}
	t.Fatalf("no perturbation is written for %s.%s, declared %s", operation, parameter.name, parameter.typeName)
	return nil
}

// Whether a provider perturbation that did not move the answer is evidence of anything.
//
// The perturbation of a provider parameter is the tagging provider, which flips the bytes
// of every answer that HAS bytes and passes a value method -- HashSize, KeySize, NonceSize,
// Suite -- through unchanged, because a size has no bytes to flip. So a construction whose
// whole use of the provider is a length is one this wrapper CANNOT move, and reporting it
// as "does not read the crypto it was handed" says something false about a body that read
// the provider on every call it made. NewSecretTree is that shape: it validates
// encryption_secret against KDF.Nh and then stores it, and the first value derived THROUGH
// the provider exists only once a leaf has been taken.
//
// A construction that reached the provider not at all has an EMPTY call log and is still
// reported, which is the case that report exists for. And this is not a licence to hardcode
// a length: what separates a read HashSize from a written 32 is a provider whose Nh is not
// 32, which is what key_schedule_test.go's wide kdf differential and the secret tree's own
// synthetic suite row are for.
//
// The class is providerValueMethods TOGETHER WITH taggingProviderPassesThrough, both read
// rather than written out again here, so a method that stops being either stops being excused
// by this.
//
// The second half of that union arrived with framing's ValSem010, and enumerating only the
// four size methods understated the class by exactly the two entries the tagging provider
// itself declares. A construction whose whole use of the provider is VerifyWithLabel reaches a
// method the wrapper ALREADY names as unflippable -- "answers an error, and flipping has
// nothing to change in a refusal" -- so reporting it as "does not read the crypto it was
// handed" says something false about a body that routed every call it made. Deriving the union
// rather than adding a name is what keeps the two rosters from drifting: a method that stops
// being passed through starts being held here on the commit that tags it.
func providerReachedOnlyUnflippableAnswers(perturbed reflect.Value) bool {
	return len(providerUnflippableAnswersReached(perturbed)) != 0
}

// The same reading, answering WHICH methods excused the construction rather than only that some
// did, so the gate can report the exemption it granted instead of applying it silently.
//
// One roster and not two: the bool above is this function, so a method that stops being
// unflippable stops excusing and stops being reportable in the same edit. The answer is the
// distinct calls in log order, sorted, and it is empty for a construction that reached a method
// the wrapper CAN move and for one that reached the provider not at all -- the second being the
// case the routing report exists for.
func providerUnflippableAnswersReached(perturbed reflect.Value) []string {
	if !perturbed.IsValid() || !perturbed.CanInterface() {
		return nil
	}
	tagging, isTagging := perturbed.Interface().(*taggingCryptoProvider)
	if !isTagging || len(tagging.calls) == 0 {
		return nil
	}
	excusing := []string{}
	for _, call := range tagging.calls {
		_, unflippable := taggingProviderPassesThrough[call]
		if !unflippable && !slices.Contains(providerValueMethods, call) {
			return nil
		}
		if !slices.Contains(excusing, call) {
			excusing = append(excusing, call)
		}
	}
	slices.Sort(excusing)
	return excusing
}

// The constructions this gate excuses from the routing claim, each named with the answer the
// tagging wrapper cannot move for it.
//
// The table does not DECIDE the exemption -- providerUnflippableAnswersReached decides it off
// the two rosters, which is what keeps those rosters and this reading from drifting. What the
// table does is make each exemption visible: the set actually excused is compared against these
// keys in both directions at the end of every suite, so a construction that starts being
// excused, or stops being, fails here rather than passing quietly.
//
// That is worth having most for the half of the union that is not a size. VerifyWithLabel and
// MacVerify answer an error and a bool, so a later construction whose entire use of the provider
// is a MAC comparison would be excused from the routing claim by a union nobody edited and by a
// gate that reported a clean run. It lands here instead, as a row nothing declares.
var providerExcusedFromTheRoutingClaim = map[string]string{
	"NewSecretTree reaches the provider only through HashSize":                                       "validates encryption_secret against KDF.Nh and stores it, and the first value derived THROUGH the provider exists only once a leaf has been taken",
	"VerifyAuthenticatedContent reaches the provider only through VerifyWithLabel":                   "answers an error, and a wrapper that flips bytes has nothing to change in a refusal; TestProviderHasNoRemainingStubs still holds it by input perturbation, and a verify made to accept unconditionally fails that half",
	"ValidateUpdatePathLeafNode reaches the provider only through VerifyWithLabel":                   "RFC 9420 section 7.3's commit door, on VerifyAuthenticatedContent's terms exactly: its whole answer is a verdict about a received UpdatePath's leaf, and a wrapper that flips bytes has nothing to change in one. It is still held by input perturbation here -- the sender index, the group context and each of five fields of the leaf must change the verdict -- and a door made to accept unconditionally fails that half",
	"OpenPublicMessage reaches the provider only through HashSize and MacVerify and VerifyWithLabel": "answers a message it did not derive: both of its authenticators are VERDICTS, so the union of the two refusals it reaches -- MacVerify for ValSem007 and ValSem008, VerifyWithLabel for ValSem010 -- has nothing a wrapper that flips bytes can change, and HashSize is the width the membership_key is refused against. It is the exact shape the comment above warned would land here, and it is not unheld: TestProviderHasNoRemainingStubs moves its key, its message, its resolver and its group context and requires each to change the verdict; TestPublicMessageRefusesForgedMembershipTag sweeps every bit and every length of the tag; TestOpenPublicMessageRefusesEveryFlippedBitOfTheSignature does the same for the signature with the tag recomputed each time, which is what separates the two authenticators; and TestOpenPublicMessageRefusesEveryKeyButTheSendersOwn sweeps the resolver's answer",
	"verifyMembershipTag reaches the provider only through HashSize and MacVerify":                   "answers an error over a comparison, and MacVerify is the second half of the union that is not a size while HashSize is the width the membership_key is refused against: a wrapper that flips bytes has nothing to change in a bool or in an int the registry fixes. It is the construction that comment above warned would land here, and it is not unheld -- TestProviderHasNoRemainingStubs moves its key, its message, its group context and its tag and requires each to change the verdict, TestVerifyMembershipTagRefusesEveryTagButItsOwn sweeps every bit and every length, TestBothDoorsIntoSection62RefuseEveryKeyNoTagMayBeTakenUnder sweeps every key width and the erased epoch's own key, and TestTheMembershipTagPreimageIsTheOneThePublishedTagsWereTakenOver holds it to tags mlswg published",
}

// One call, with a panic caught rather than taken. A method that still refuses to be
// called is exactly what this gate is looking for, and a test binary that died on the
// first one would report nothing about the operations after it.
func providerStubCall(call reflect.Value, arguments []reflect.Value) (results []reflect.Value, recovered any) {
	defer func() { recovered = recover() }()
	return call.Call(arguments), nil
}

// A call's whole answer as text, so two calls are compared by everything they returned
// rather than by the one result somebody thought to read. A refusal is part of the answer:
// an operation whose argument moved from accepted to rejected has observed that argument
// just as surely as one whose bytes changed.
// The byte slices a structured answer carries, read through unexported fields.
//
// A construction can answer with a type rather than with bytes, and the epoch key
// schedule's entry points do: they answer a *KeySchedule holding twelve secrets behind
// unexported fields. Two readings of such an answer are wrong here and one is right.
//
// Rendering it with fmt is wrong. fmt prints a nested pointer as a machine address, and
// the type holds the provider it was built with, so two calls that agreed on every
// secret render differently -- which fails the control above for every row, and would
// make every perturbation below "differ" for a reason that is not the perturbation.
//
// Reading only exported fields is wrong the other way. This package keeps its secrets
// unexported on purpose, so that reading renders a fully implemented type as no bytes
// at all -- which is exactly the answer a stub gives, and this gate would report it as
// one while being unable to tell the two apart.
//
// So what is read is every byte slice the value reaches, unexported included, in
// declaration order, recursing into struct fields and stopping at everything else. A
// secret is bytes; a field that is neither bytes nor a struct of them is state whose
// identity says nothing about whether the inputs were observed.
//
// The bytes are copied out one at a time because reflect refuses Bytes and Interface on
// a value reached through an unexported field. Uint on an element is permitted, is a
// read and not a handle, and needs no unsafe.
// One entry of a map of byte slices, with the text its key sorts by, so a map answer
// renders in one order rather than in go's randomised one.
type providerMapEntry struct {
	order   string
	carried []byte
}

// providerMapKeyOrder renders one map key for sorting, using only the reads reflect
// permits on a value reached through an unexported field: Interface panics on one of
// those, so fmt.Sprint is not available here and the kinds are switched on directly.
//
// An unhandled kind renders as its type name, which puts every key of that kind in one
// group and leaves their order to the byte tie break beside the call rather than to map
// iteration. That is deterministic, which is all this ordering has to be -- it is a
// rendering for comparison against another rendering of the same type, never a claim
// about which node a secret belonged to.
func providerMapKeyOrder(key reflect.Value) string {
	switch key.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return fmt.Sprintf("%020d", key.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return fmt.Sprintf("%020d", key.Uint())
	case reflect.String:
		return key.String()
	default:
		return key.Type().String()
	}
}

func providerStructByteFields(value reflect.Value) [][]byte {
	carried := [][]byte{}
	for i := range value.NumField() {
		field := value.Field(i)
		switch {
		// a FIXED WIDTH byte array is bytes no less than a slice is, and reading only the slice
		// left section 6.3.2's SenderData rendering as "carrying no bytes at all" -- its
		// reuse_guard is opaque[4] because the RFC fixes that field's width, and "no bytes at
		// all" is exactly what a stub answers, so a complete implementation would have been
		// reported while an actual stub of it sat here indistinguishable. One case and not two,
		// because the reading is the same either way.
		case (field.Kind() == reflect.Slice || field.Kind() == reflect.Array) &&
			field.Type().Elem().Kind() == reflect.Uint8:
			out := make([]byte, field.Len())
			for at := range out {
				out[at] = byte(field.Index(at).Uint())
			}
			carried = append(carried, out)
		case field.Kind() == reflect.Struct:
			carried = append(carried, providerStructByteFields(field)...)
		// a VECTOR of structures carries bytes no less than a map of them does, and RFC 9420's
		// wire types are full of them: a Welcome's secrets<V> is one EncryptedGroupSecrets per
		// joiner and holds every per-member seal the structure has. Without this case the whole
		// of that vector rendered as no bytes at all -- which is the answer a stub gives, so a
		// builder that sealed nothing per joiner was indistinguishable here from a complete one.
		//
		// MEASURED, not supposed: with the vector unread, BuildWelcome answered identically with
		// its joinerSecret moved at every probed position, and this gate reported "does not read
		// the joinerSecret it was handed" against an implementation that reads it into every
		// entry of the vector it had just stopped looking at.
		//
		// IN ORDER and never sorted, which is the one difference from the map case below. A
		// vector has an order the wire fixes, so two renderings of one value already agree
		// without sorting, and sorting would throw away the property a Welcome rests on -- that
		// entry i is joiner i -- by rendering a builder that transposed two entries as identical
		// to one that did not.
		case field.Kind() == reflect.Slice && field.Type().Elem().Kind() == reflect.Struct:
			for at := range field.Len() {
				carried = append(carried, providerStructByteFields(field.Index(at))...)
			}
		case field.Kind() == reflect.Map && field.Type().Elem().Kind() == reflect.Slice &&
			field.Type().Elem().Elem().Kind() == reflect.Uint8:
			// a type that holds its secrets in a MAP holds them no less, and the secret
			// tree holds every undelivered node secret in one. Without this case the whole
			// of that type renders as no bytes at all -- which is the answer a stub gives,
			// so the report above would fire on a complete implementation while an actual
			// stub of it sat here indistinguishable. The file comment's rule is unchanged:
			// a secret is bytes, and this is where a second kind of container of them is
			// read rather than stopped at.
			//
			// Sorted, because go randomises map iteration on purpose. An unsorted walk
			// renders one tree two different ways, and the repeat control above -- the one
			// that fails a row whose two identical calls answered differently -- would then
			// fire on every row of every construction answering a map, for a reason that is
			// not the code under test.
			held := []providerMapEntry{}
			for _, key := range field.MapKeys() {
				entry := field.MapIndex(key)
				out := make([]byte, entry.Len())
				for at := range out {
					out[at] = byte(entry.Index(at).Uint())
				}
				held = append(held, providerMapEntry{order: providerMapKeyOrder(key), carried: out})
			}
			slices.SortFunc(held, func(a, b providerMapEntry) int {
				if by := strings.Compare(a.order, b.order); by != 0 {
					return by
				}
				return bytes.Compare(a.carried, b.carried)
			})
			for _, entry := range held {
				carried = append(carried, entry.carried)
			}
		}
	}
	return carried
}

func providerStubAnswer(results []reflect.Value) string {
	rendered := []string{}
	for _, result := range results {
		if result.Kind() == reflect.Slice && result.Type().Elem().Kind() == reflect.Uint8 {
			rendered = append(rendered, hex.EncodeToString(result.Bytes()))
			continue
		}
		if result.Kind() == reflect.Pointer && result.Type().Elem().Kind() == reflect.Struct {
			if result.IsNil() {
				rendered = append(rendered, "nil")
			} else {
				for _, carried := range providerStructByteFields(result.Elem()) {
					rendered = append(rendered, hex.EncodeToString(carried))
				}
			}
			continue
		}
		rendered = append(rendered, fmt.Sprint(result.Interface()))
	}
	return strings.Join(rendered, " ")
}

// The results of a base call that are indistinguishable from a stub's, named one by one.
//
// This is the half of the gate that a plausible zero value has to get past. A method
// returning a correctly sized run of zeroes, a nil slice with a nil error, a false, or a
// zero length satisfies every type level check and every compile assertion in this
// package, and it is the shape a stub takes when somebody writes one to keep the build
// green rather than to fail loudly.
func providerStubZeroResults(results []reflect.Value) []string {
	zero := []string{}
	for i, result := range results {
		position := "result " + strconv.Itoa(i)
		switch {
		case result.Type() == reflect.TypeOf((*error)(nil)).Elem():
			if !result.IsNil() {
				zero = append(zero, position+" is the error "+fmt.Sprint(result.Interface()))
			}
		case result.Kind() == reflect.Slice && result.Type().Elem().Kind() == reflect.Uint8:
			bytesOut := result.Bytes()
			if len(bytesOut) == 0 {
				zero = append(zero, position+" is empty")
				continue
			}
			if !slices.ContainsFunc(bytesOut, func(b byte) bool { return b != 0 }) {
				zero = append(zero, position+" is "+strconv.Itoa(len(bytesOut))+" zero bytes")
			}
		// a LIST of byte slices, which is how the path secret ladder answers. Read one
		// entry at a time and not as one run: a ladder that climbed correctly for its first
		// rung and answered zeroes for the rest is a stub in every position but one, and a
		// rule that looked for a non-zero byte anywhere in the whole answer would report it
		// as complete. The empty outer slice is its own case, because a construction that
		// answered no rungs at all answers no bytes at all and would otherwise pass by
		// having nothing to inspect.
		case result.Kind() == reflect.Slice && result.Type().Elem().Kind() == reflect.Slice &&
			result.Type().Elem().Elem().Kind() == reflect.Uint8:
			if result.Len() == 0 {
				zero = append(zero, position+" is an empty list")
				continue
			}
			for at := range result.Len() {
				entry := result.Index(at).Bytes()
				if len(entry) == 0 {
					zero = append(zero, position+" entry "+strconv.Itoa(at)+" is empty")
					continue
				}
				if !slices.ContainsFunc(entry, func(b byte) bool { return b != 0 }) {
					zero = append(zero, position+" entry "+strconv.Itoa(at)+" is "+
						strconv.Itoa(len(entry))+" zero bytes")
				}
			}
		case result.Kind() == reflect.Pointer && result.Type().Elem().Kind() == reflect.Struct:
			if result.IsNil() {
				zero = append(zero, position+" is nil")
				continue
			}
			carried := providerStructByteFields(result.Elem())
			if len(carried) == 0 {
				zero = append(zero, position+" is a "+result.Type().String()+" carrying no bytes at all")
				continue
			}
			for at, field := range carried {
				if len(field) == 0 {
					zero = append(zero, position+" field "+strconv.Itoa(at)+" is empty")
					continue
				}
				if !slices.ContainsFunc(field, func(b byte) bool { return b != 0 }) {
					zero = append(zero, position+" field "+strconv.Itoa(at)+" is "+strconv.Itoa(len(field))+" zero bytes")
				}
			}
		case result.Kind() == reflect.Bool:
			if !result.Bool() {
				zero = append(zero, position+" is false")
			}
		case result.CanInt():
			if result.Int() == 0 {
				zero = append(zero, position+" is zero")
			}
		case result.CanUint():
			if result.Uint() == 0 {
				zero = append(zero, position+" is zero")
			}
		default:
			zero = append(zero, position+" is a "+result.Type().String()+", which this gate has no zero value rule for")
		}
	}
	return zero
}

// The operations whose answer moves when the provider's byte stream does, measured rather
// than assumed. Nothing is drawn anywhere else in this package, so this is the whole of
// what randomness reaches, and the gate compares the measurement against this list in both
// directions: an operation that stopped drawing fails here, and so does one that started.
//
// The first direction is what a stub trips. A Random that answered make([]byte, n) is a
// function of its argument and nothing else, so it passes every perturbation this gate
// makes and reports here as an operation that no longer reads the stream it was given.
var providerStreamDependentOperations = []string{
	// p7 task 15's welcome builder, which is stream dependent for exactly one reason: it seals
	// one GroupSecrets per joiner and every one of those seals encapsulates to a fresh
	// ephemeral. The group info half of a Welcome is derived and not drawn, so a build over two
	// streams that answered the same secrets entries would be one where the per-joiner
	// encapsulation is a constant.
	"BuildWelcome",
	"EncryptWithLabel",
	"HpkeSeal",
	// the key package constructor draws three times -- a signature key pair and two
	// entropy draws, one for each of the two HPKE key pairs it must not derive from one
	// seed -- so it is the first CONSTRUCTION of this package to depend on the stream
	// rather than on what it was handed. It refusing an exhausted source is what says it
	// does not fall back onto one of its own.
	"NewKeyPackage",
	// the SIBLING constructor, which draws TWICE and not three times: two entropy draws, one
	// for each of the two HPKE key pairs it must not derive from one seed, and NO signature
	// key pair -- the signing key is the caller's. That difference is the one place its
	// distance from NewKeyPackage is a number rather than a property somebody has to think to
	// compare, and providerStreamDraws is where the number is written down. It refusing an
	// exhausted source is what says it does not fall back onto a stream of its own.
	"NewKeyPackageWithSigner",
	"Random",
	"SealPrivateMessage",
	"SealWithLabel",
	"SignatureKeyPair",
	"sealPrivateMessage",
	// MASTER section 8.4.2 v2's two seal bodies, stream dependent for sealPrivateMessage's
	// reason and for no reason of their own: the reuse guard is four fresh octets per message,
	// and neither the generation report nor the S3 pin draws anything.
	"sealPrivateMessageAt",
	"sealPrivateMessageBound",
}

// The operations with no argument to move and no draw to make, each with the registry
// field it must answer. There is nothing for a perturbation to observe in a method that
// takes nothing and reads nothing, so what separates these from a stub is the suite
// parameters, read out of the registry rather than out of the provider.
//
// The registry is the weaker of the two available gates and it is worth being exact about
// that: it catches a zero, a hardcoded wrong constant and a method answering out of the
// wrong field, and it cannot catch a registry entry that is itself wrong. TestProviderSizes
// is what holds the registry, against literals rather than against itself.
var providerRegistryAnswers = map[string]func(params *SuiteParams) any{
	"HashSize":  func(params *SuiteParams) any { return params.Nh },
	"KeySize":   func(params *SuiteParams) any { return params.Nk },
	"NonceSize": func(params *SuiteParams) any { return params.Nn },
	"Suite":     func(params *SuiteParams) any { return params.Suite },
	// not a size method, and the same shape. ZeroSecret has no input this gate can move
	// — the provider it takes carries a length and nothing else, and the tagging provider
	// passes a length through unchanged — and its whole contract is a value the registry
	// fixes: KDF.Nh zero bytes. Written as the rendering providerStubAnswer makes of a
	// byte slice result, so the comparison below is over the same text.
	//
	// This is stricter than the "not the zero value" report it stands in for rather than
	// weaker: the zero value IS ZeroSecret's correct answer, so that report can only be
	// noise here, while this one fails a wrong length and a byte that is not zero. What
	// it cannot see is an Nh hardcoded rather than read from the provider, which is the
	// same limit the four size methods carry; what closes it for those is the receiver
	// field read below, and for this one it is TestNoStubShapesRemainInSource, whose
	// unread parameter shape fails a body that never reaches for the provider at all.
	//
	// That shape reading is a source shape, and a body that reads the provider and then
	// ignores the value walks past it. What closes the same limit behaviourally is an
	// input this registry cannot supply: a provider whose Nh is not 32. That is what
	// key_schedule_test.go's wideKdfProvider is, and what TestZeroSecretReadsKdfNhFrom-
	// TheProvider holds this construction to. Both registered suites fix Nh at 32, so
	// nothing registered here separates a literal from a provider read.
	"ZeroSecret": func(params *SuiteParams) any { return strings.Repeat("00", params.Nh) },
	// and psk_secret for an epoch with no pre shared keys, which RFC 9420 section 8.4
	// defines as that same all zero string. Same shape as ZeroSecret and the same
	// reason: the only argument is a provider carrying a length, and the tagging
	// provider passes a length through unchanged, so there is nothing here for a
	// perturbation to move. What separates it from a constant is this comparison, plus
	// the published empty vector in psk_test.go and the wide kdf differential in
	// key_schedule_test.go.
	"EmptyPskSecret": func(params *SuiteParams) any { return strings.Repeat("00", params.Nh) },
}

// The positions of a structured answer that are empty on purpose, named with the reason.
//
// An empty byte field is what a stub answers, so the reader above reports every one of
// them. The exception is a field a type's own contract says is UNDEFINED on the path
// under test, and the epoch key schedule has exactly one such path: a group being
// created was never joined, so it has no joiner_secret and no welcome_secret. Answering
// KDF.Nh zero bytes there instead of nil would seal a Welcome under a key an attacker
// also has, so nil is the whole point rather than an omission, and this is where that
// decision is written down.
//
// The positions are the reports they excuse, verbatim, and the gate requires each to
// have actually been reported before removing it. An entry for a field that comes back
// carrying bytes therefore fails rather than sitting here excusing nothing, which is how
// an exemption stops outliving the design it was written for.
var providerConstructionsWithUndefinedResults = map[string][]string{
	"NewKeyScheduleFromEpochSecret": {"result 0 field 1 is empty", "result 0 field 2 is empty"},
	// the key_package leaf's parent hash. RFC 9420 section 7.2 makes parent_hash the COMMIT
	// arm of the leaf's variant, so a leaf built by NewLeafNode carries none and cannot: a
	// key package is minted before there is a tree to hash a path through. Empty is the
	// correct answer here rather than a stub, and it stops being correct the moment this
	// constructor starts producing some other source, at which point the entry fails.
	"NewLeafNode": {"result 0 field 3 is empty"},
	// the same parent hash, one structure out. A KeyPackage carries the leaf whole, so the
	// byte fields of its answer are the init key, then the leaf's five, then the signature
	// and the signing seed -- and field 4 is that leaf's parent_hash, empty for exactly the
	// reason NewLeafNode's is: a key package is minted before there is a tree to hash a path
	// through. The entry stops being correct the moment this constructor starts producing
	// some other leaf source, at which point it fails.
	"NewKeyPackage": {"result 0 field 4 is empty"},
	// the sibling constructor's identical leaf, at the identical position: it answers the same
	// *KeyPackage shape, so the byte fields of its answer are the init key, then the leaf's
	// five, then the signature and the signing seed, and field 4 is that leaf's parent_hash --
	// empty for NewLeafNode's reason, since a key package is minted before there is a tree to
	// hash a path through. The entry stops being correct the moment this constructor starts
	// producing some other leaf source, at which point it fails.
	"NewKeyPackageWithSigner": {"result 0 field 4 is empty"},
	// the FramedContentAuthData's confirmation tag. RFC 9420 section 8.2 takes the confirmed
	// transcript hash over this very signature and the tag is a MAC over that hash, so the tag
	// cannot exist at the moment the signature is made: a commit's caller sets it afterwards.
	// Empty is the correct answer here rather than a stub, and it stops being correct the day
	// this constructor starts producing one, at which point this entry fails.
	"SignAuthenticatedContent": {"result 0 field 4 is empty"},
	// section 6.2's seal and open, whose rows are built over a PROPOSAL because ValSem005 refuses
	// an application message in a public frame. Field 2 is the framed content's application_data
	// and field 4 is the auth data's confirmation_tag: a proposal has neither, by RFC 9420 section
	// 6 and section 8.2 respectively, so empty is the correct answer here rather than a stub. Both
	// entries stop being correct the day these rows carry a commit or an application message, at
	// which point they fail.
	"SealPublicMessage": {"result 0 field 2 is empty", "result 0 field 4 is empty"},
	"OpenPublicMessage": {"result 0 field 2 is empty", "result 0 field 4 is empty"},
	// section 6.3's open, whose row is built over the APPLICATION message the framing rows are
	// signed over. Field 4 is the auth data's confirmation_tag, and RFC 9420 section 8.2 gives
	// one to a commit and to nothing else, so empty is the correct answer here rather than a
	// stub. The entry stops being correct the day this row carries a commit, at which point it
	// fails.
	"OpenPrivateMessage": {"result 0 field 4 is empty"},
	// the body that open delegates to since MASTER section 8.4.2 v2, which carries
	// OpenPrivateMessage's entry plus one of its own: result 1 is the GENERATION the frame was
	// sealed at, and this gate's base call reads a frame sealed at generation ZERO. Zero is the
	// correct answer for it rather than a stub, and it is the first generation of every ratchet
	// this package ever hands out, so a row built over a later one would be a row built over a
	// fixture rather than over the first message of an epoch. The entry stops being correct the
	// day this row's message is sealed at any other generation, at which point it fails.
	"openPrivateMessageAt": {"result 0 field 4 is empty", "result 1 is zero"},
	// the seal one layer down, on the same reading from the other side: result 1 is the
	// generation it CONSUMED, and this gate's pinned key source stands at generation zero.
	"sealPrivateMessageAt": {"result 1 is zero"},
	// the pre-ratchet peek, on exactly that reading: result 2 is the generation, and the frame
	// it peeks is the same one, sealed at generation zero.
	"PeekPrivateMessageSender": {"result 2 is zero"},
}

// A construction whose answer is not a function of its arguments alone, named with the reason
// and with what holds it instead.
//
// Every comparison the stub gate makes -- the repeat control, the second stream, and each
// perturbation -- reads one call's answer against another's, and all of them rest on two calls
// with the same arguments answering the same bytes. A construction that stamps a wall clock
// reading into what it signs breaks that, and breaks it INTERMITTENTLY: two calls agree
// whenever they fall inside one second and disagree when they straddle one. Measured on the
// shape rather than supposed -- NewLeafNode is called some thirty times per suite here, each
// call an ed25519 derive, sign and verify, so the span the comparisons cover is milliseconds
// and a second boundary lands in it a few times in a hundred runs. A gate that reports a
// defect that is not there a few times in a hundred is worse than an exempted one: it is the
// one failure mode that teaches a reader to re-run instead of to look.
//
// The exemption is narrow. Everything above the comparisons still runs for a name here -- the
// call itself, its refusals, and the zero value reading that is what "no stub shapes remain"
// actually means -- and what is skipped is held elsewhere, named per entry.
var providerConstructionsAnsweringOffTheWallClock = map[string]string{
	// the key package constructor inherits the leaf's clock: it builds its leaf through
	// NewLeafNode, so the Lifetime stamped there is inside the KeyPackageTBS this signs, and
	// two calls a second apart answer different signatures for a reason that is not the
	// arguments. Everything above the comparisons still runs for it.
	"NewKeyPackage":           "builds its leaf through NewLeafNode, which stamps a key package Lifetime from the wall clock, so two calls a second apart sign different key packages; TestNewKeyPackageReadsEveryArgumentItWasHanded holds it to reading each of its arguments, with the lifetime normalised out and the parameter list derived off its own declaration, TestNewKeyPackageDrawsTheInitAndEncryptionKeysFromSeparateEntropy to answering two key pairs rather than one, TestNewKeyPackageKeepsTheSigningSeedOffTheWireAndBesideItsOwnLeaf to the seed it keeps, and the routing and KDF.Nh differentials to reaching the provider it was handed",
	"NewKeyPackageWithSigner": "builds its leaf through NewLeafNode, which stamps a key package Lifetime from the wall clock, so two calls a second apart sign different key packages; the sibling row above carries the same argument. Everything above the comparisons still runs for it, and what the comparisons would have held is held by name instead: TestNewKeyPackageWithSignerBindsAllFourToTheCallersSigner to the four bindings the caller's key is put on, TestNewKeyPackageWithSignerDrawsTheInitAndEncryptionKeysFromSeparateEntropy to answering two key pairs rather than one, TestNewKeyPackageWithSignerClonesTheCallersSigner to the copy it keeps, and the routing, KDF.Nh and draw-count differentials to reaching the provider it was handed",
	"NewLeafNode":             "stamps a key package Lifetime from the wall clock, so two calls a second apart sign different leaves; TestNewLeafNodeReadsEveryArgumentItWasHanded holds it to reading each of its arguments, with the lifetime normalised out, and TestNewLeafNodeRoutesThroughTheProviderItWasHanded to routing through the provider",
}

// Every operation on both surfaces is covered, with nothing skipped and nothing excused.
//
// The other coverage gate in this file lets a value method and a stub method out, because
// what it covers is byte behaviour and neither has any. This one is about whether an
// operation is implemented at all, so there is no such thing as a method it need not
// reach, and there is no excusal map to write a name into.
//
// What is passed in is the operations whose rows ran, recorded at the far end of each row
// rather than on entry to it. Recorded on entry this compares an enumeration against
// itself: probed is then the name list of providerOperations, want is the same two readings
// that list is built from, and a t.Fatalf upstream has already refused to go on unless the
// two agree -- so no version of the product could make it fire. Recorded at the end it says
// the row was carried out, and an operation that stopped at a refusal is named here too.
func assertCoversEveryProviderOperation(t *testing.T, gate string, probed []string) {
	t.Helper()
	want := slices.Concat(providerMethodNames(), packageLevelFunctionsTaking(t, providerInterfaceName))
	slices.Sort(want)
	got := slices.Clone(probed)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("%s reached %v, want %v", gate, got, want)
	}
}

// The provider is completely implemented, for every registered suite, and no operation on
// it is a placeholder.
//
// What "not a stub" means here is operational and it is worth writing down, because the
// obvious readings are weaker than they look. Calling every method and checking that none
// panicked -- the shape this gate was planned as -- is satisfied by every method that
// returns a correctly sized run of zeroes, which is what a stub written to keep the build
// green actually looks like. Checking that an answer is not the zero value is satisfied by
// a constant. What is asserted instead is that every declared input of every operation is
// observed in its answer: the operation is called once over a fixed byte stream, then once
// per parameter and probed position with that one position moved and everything else
// identical, and the two answers must differ. A stub cannot pass that, because a stub is by
// construction a function of fewer of its inputs than it declares.
//
// What that misses, stated rather than left for a reader to discover. Three positions of
// each argument are probed rather than all of them, so an operation reading exactly those
// three and nothing else passes -- narrower than the one position the first version of this
// gate probed, and not nothing. And an operation that is a function of all of its inputs
// and computes the wrong one. Every input is observed, the
// answers all differ, and this gate is silent. That class is the published vectors' --
// RFC 4231 for the mac, RFC 5869 for the kdf, FIPS 180-4 for the hash, the vendored RFC
// 9180 corpus for the aead and the whole hpke path, and the labelled constructions against
// crypto-basics when p8 vendors it -- and it is why a stub gate is not a correctness gate.
// It also misses an operation that observes each input separately but not jointly, for the
// same reason and held by the same corpora.
//
// Every call is made over a provider of its own, built over a freshly opened reader on the
// same script, so the four operations that draw are deterministic across any pair of calls
// and a difference is attributable to the argument that moved rather than to the entropy.
// The control at the top of each row is what says so: the base call repeated over a fresh
// provider must give the identical answer, and without it every row below would pass on a
// provider whose answers were simply never twice the same.
//
// Each argument is moved at more than one position, read off its length rather than written
// down. Moving only its last byte states a weaker property than the one above -- that the
// last position of each input is observed -- and an operation narrowed to that one byte
// passes it. perturbedPositions records what that cost when it was the whole of the rule.
func TestProviderHasNoRemainingStubs(t *testing.T) {
	if len(providerStubMethods) != 0 {
		t.Errorf("providerStubMethods names %v, so the provider still refuses to answer somewhere", providerStubMethods)
	}
	for name := range providerConstructionsWithUndefinedResults {
		if _, called := providerConstructionValues[name]; !called {
			t.Errorf("providerConstructionsWithUndefinedResults excuses %s, which this gate does not call", name)
		}
	}
	for _, suite := range Suites() {
		params, err := LookupSuite(suite)
		if err != nil {
			t.Fatalf("look up %#04x: %v", uint16(suite), err)
		}
		newProvider := func(first byte) CryptoProvider {
			return mustProviderOver(t, suite, providerStubStream(first))
		}
		arguments := providerStubArguments(t, params, newProvider(0x80))
		probed := []string{}
		drawing := []string{}
		unobserved := []string{}
		excused := []string{}
		for _, operation := range providerOperations(t) {
			where := fmt.Sprintf("suite %#04x %s", uint16(suite), operation.name)
			subject := newProvider(0x80)
			arguments[providerInterfaceName] = subject
			bound := operation.bind(subject)
			if bound.Type().NumIn() != len(operation.parameters) {
				t.Fatalf("%s is declared with %d parameters and called with %d",
					where, len(operation.parameters), bound.Type().NumIn())
			}
			base := []reflect.Value{}
			for i, parameter := range operation.parameters {
				base = append(base, providerStubArgument(t, arguments, operation.name, parameter, bound.Type().In(i)))
			}
			results, recovered := providerStubCall(bound, base)
			if recovered != nil {
				t.Errorf("%s refused to answer: %v", where, recovered)
				continue
			}
			// an operation whose whole contract is a value the registry fixes answers the
			// zero value on purpose, and ZeroSecret's correct answer is Nh zero bytes. It
			// is held to providerRegistryAnswers below instead, which is the stricter of
			// the two: "not the zero value" would pass a wrong length and a wrong
			// constant, and the registry comparison passes neither.
			_, answersARegistryValue := providerRegistryAnswers[operation.name]
			zero := providerStubZeroResults(results)
			for _, undefined := range providerConstructionsWithUndefinedResults[operation.name] {
				if !slices.Contains(zero, undefined) {
					t.Errorf("%s names %q as undefined on this path and it came back carrying bytes, so the exemption is excusing something that no longer happens",
						where, undefined)
				}
				zero = slices.DeleteFunc(zero, func(report string) bool { return report == undefined })
			}
			if len(zero) != 0 && !answersARegistryValue {
				t.Errorf("%s answered %s, which is what a stub answers", where, strings.Join(zero, ", "))
			}
			if reason, offTheClock := providerConstructionsAnsweringOffTheWallClock[operation.name]; offTheClock {
				t.Logf("%s: no answer of this row is compared against another call's, because it %s", where, reason)
				// the stream dependence is still measured for these rows, and it is measured
				// by COUNTING what came out of the reader rather than by comparing two
				// answers. An answer carrying a wall clock reading moves between two calls
				// for a reason that is not the stream, so the comparison every other row
				// uses would report such a row as stream dependent whatever it drew -- and
				// the two ends of this gate compare that measurement against
				// providerStreamDependentOperations, which is the same list
				// TestEveryProviderOperationDrawsExactlyWhatItUses holds its own counted
				// measurement to. Skipping the measurement instead would leave a
				// construction that draws sitting outside one of the two lists that have to
				// agree, which is how the pair stops being a pair.
				counting := &countingReader{inner: providerStubStream(0x80)}
				counted := mustProviderOver(t, suite, counting)
				arguments[providerInterfaceName] = counted
				drawn := []reflect.Value{}
				for i, parameter := range operation.parameters {
					drawn = append(drawn, providerStubArgument(t, arguments, operation.name, parameter, bound.Type().In(i)))
				}
				if _, refused := providerStubCall(operation.bind(counted), drawn); refused != nil {
					t.Errorf("%s refused the call this gate counts its draws through: %v", where, refused)
					continue
				}
				if counting.drawn != 0 {
					drawing = append(drawing, operation.name)
				}
				probed = append(probed, operation.name)
				continue
			}
			answer := providerStubAnswer(results)
			// the control: nothing about this row means anything if the same call twice
			// does not agree with itself
			repeated := newProvider(0x80)
			arguments[providerInterfaceName] = repeated
			control := []reflect.Value{}
			for i, parameter := range operation.parameters {
				control = append(control, providerStubArgument(t, arguments, operation.name, parameter, bound.Type().In(i)))
			}
			repeatedResults, recovered := providerStubCall(operation.bind(repeated), control)
			if recovered != nil {
				t.Errorf("%s refused the repeated call: %v", where, recovered)
				continue
			}
			if repeat := providerStubAnswer(repeatedResults); repeat != answer {
				t.Errorf("%s answered %s and then %s over the same script, so no row below observes anything",
					where, answer, repeat)
				continue
			}
			observed := 0
			// the stream is an input too, and it is the only one three of these have
			alternate := newProvider(0x40)
			arguments[providerInterfaceName] = alternate
			streamed := []reflect.Value{}
			for i, parameter := range operation.parameters {
				streamed = append(streamed, providerStubArgument(t, arguments, operation.name, parameter, bound.Type().In(i)))
			}
			streamedResults, recovered := providerStubCall(operation.bind(alternate), streamed)
			if recovered != nil {
				t.Errorf("%s refused a provider over another script: %v", where, recovered)
				continue
			}
			if providerStubAnswer(streamedResults) != answer {
				drawing = append(drawing, operation.name)
				observed++
			}
			for i, parameter := range operation.parameters {
				// every operation is perturbed, the registry rows included. The four size
				// methods satisfy "no argument to move" by taking no argument, so this loop
				// never runs for them; ZeroSecret is the shape that has one and still has
				// nothing to move, because the provider it takes carries a length and the
				// tagging provider passes a length through unchanged. That is a fact about
				// ZeroSecret's answer, and it is MEASURED here rather than assumed by
				// skipping the measurement.
				//
				// The version this replaces broke out of the loop for any name the registry
				// held, which left observed at zero by construction for every one of them.
				// The unobserved-against-registered comparison at the end of the suite loop
				// -- the check whose whole job is to fail when a name is added to the
				// registry to quiet a real perturbation failure -- was then satisfied
				// tautologically, and could not fail whatever the code did. Measured, not
				// supposed: a RefHash row added to providerRegistryAnswers passed this gate
				// with all three of RefHash's movable parameters unprobed.
				//
				// What a registry row is excused from is the REPORT below, not the
				// measurement: an answer the suite parameters fix is supposed to stay put
				// under a perturbation, so a row that does not move is not a failure for
				// one. A row that DOES move still counts, still keeps the name out of
				// unobserved, and still fails at the end of the suite loop.
				positions := len(providerPerturbations(t, operation.name, parameter, base[i]))
				for at := 0; at < positions; at++ {
					// a provider of its own for every call. Four of these operations
					// draw, so a moved call sharing the base call's provider would read
					// the stream on from where the base call left it and answer
					// differently whatever the argument did -- which is a row that
					// separates nothing while looking like the strictest one here, and
					// is the shape crypto_labels_test.go names in the labelled routing
					// gate for the same reason.
					movedProvider := newProvider(0x80)
					arguments[providerInterfaceName] = movedProvider
					moved := []reflect.Value{}
					for j, other := range operation.parameters {
						moved = append(moved, providerStubArgument(t, arguments, operation.name, other, bound.Type().In(j)))
					}
					perturbations := providerPerturbations(t, operation.name, parameter, moved[i])
					if len(perturbations) != positions {
						t.Fatalf("%s resolves %s to %d perturbations and then to %d",
							where, parameter.name, positions, len(perturbations))
					}
					moved[i] = perturbations[at].value
					movedResults, recovered := providerStubCall(operation.bind(movedProvider), moved)
					if recovered != nil {
						t.Errorf("%s refused to answer with %s moved at %s: %v",
							where, parameter.name, perturbations[at].where, recovered)
						continue
					}
					if providerStubAnswer(movedResults) == answer {
						// not moving is what an answer the registry fixes is for, so it is
						// not a fault in one of those -- and it is not a pass either:
						// observed stays where it is, which is what routes the operation to
						// the registry comparison below, and that comparison is the stricter
						// of the two readings.
						if !answersARegistryValue {
							excusing := providerUnflippableAnswersReached(perturbations[at].value)
							if len(excusing) == 0 {
								t.Errorf("%s answers the same with %s moved at %s, so it does not read the %s it was handed",
									where, parameter.name, perturbations[at].where, parameter.name)
							} else if report := fmt.Sprintf("%s reaches the provider only through %s",
								operation.name, strings.Join(excusing, " and ")); !slices.Contains(excused, report) {
								excused = append(excused, report)
							}
						}
						continue
					}
					observed++
				}
			}
			if observed == 0 {
				unobserved = append(unobserved, operation.name)
				registry, held := providerRegistryAnswers[operation.name]
				if !held {
					t.Errorf("%s has no input this gate can move and no registry field to answer, so nothing here separates it from a constant", where)
					continue
				}
				if want := fmt.Sprint(registry(params)); answer != want {
					t.Errorf("%s answered %s and the registry holds %s", where, answer, want)
				}
				// the receiver field read is a property of a METHOD of the provider: it
				// is what says the answer came from the suite this provider was built
				// for rather than from a literal. A package level construction has no
				// receiver to read, so the class is taken from the interface rather than
				// assumed — and for one of those the same fact is held by
				// TestNoStubShapesRemainInSource, whose unread parameter shape fails a
				// body that never reaches for the provider it was handed.
				if slices.Contains(providerMethodNames(), operation.name) {
					if reads := providerReceiverFieldReads(t, operation.name); len(reads) == 0 {
						t.Errorf("%s reads nothing of the provider it is a method of, so the registry comparison above compared a literal against the registry", where)
					}
				}
			}
			// recorded here rather than on entry: an operation is covered by this gate
			// when its row has run, and one that stopped at a refusal above is reported
			// as unreached rather than as probed
			probed = append(probed, operation.name)
		}
		// and the wall clock exemption names operations this surface really has, so an entry
		// cannot outlive the construction it excuses
		for name := range providerConstructionsAnsweringOffTheWallClock {
			if !slices.Contains(probed, name) {
				t.Errorf("the gate excuses %s from every comparison across calls, and no operation of this surface is called %s",
					name, name)
			}
		}
		assertCoversEveryProviderOperation(t, "TestProviderHasNoRemainingStubs", probed)
		slices.Sort(excused)
		declaredExcuses := []string{}
		for report := range providerExcusedFromTheRoutingClaim {
			declaredExcuses = append(declaredExcuses, report)
		}
		slices.Sort(declaredExcuses)
		if !slices.Equal(excused, declaredExcuses) {
			t.Errorf("suite %#04x excused %v from the routing claim and this file declares %v; an exemption granted by the unflippable union is one nobody has to edit a list to get",
				uint16(suite), excused, declaredExcuses)
		}
		slices.Sort(drawing)
		if !slices.Equal(drawing, providerStreamDependentOperations) {
			t.Errorf("suite %#04x draws randomness in %v, want %v", uint16(suite), drawing, providerStreamDependentOperations)
		}
		slices.Sort(unobserved)
		registered := []string{}
		for name := range providerRegistryAnswers {
			registered = append(registered, name)
		}
		slices.Sort(registered)
		if !slices.Equal(unobserved, registered) {
			t.Errorf("suite %#04x has no movable input for %v, and the registry rows cover %v",
				uint16(suite), unobserved, registered)
		}
	}
}

// One declaration's stub shapes, read off the parse tree.
//
// The gate this was planned as looked for one literal sentence, "not implemented until
// task", in the text of the package. That is a hand written list of length one, and this
// project has understated a class by writing one eleven times running: panic("todo"),
// panic("unimplemented"), a placeholder somebody worded differently, or a body that returns
// a correctly sized nothing all walk straight past a string match. What is read here is the
// shape instead, and a shape does not care how a placeholder is worded.
//
// Three shapes, each of which a real declaration in this package has none of:
//
//   - a declaration with results whose body never returns one, which is what a body that
//     only panics looks like from the outside;
//   - a panic standing as a whole statement of the body rather than inside a branch, which
//     is a refusal to run rather than a refusal of an input -- every real panic in this
//     package is the else of a check;
//   - a parameter the body never reads, which is the plausible zero value: the body
//     compiles, returns something of the right size, and is a function of less than it
//     declares.
//
// The third is every parameter rather than any one of them. Asking only that some parameter
// be read is a rule a two parameter placeholder satisfies by touching one of them, and it
// is weaker than the behavioural half of this task states for the same declarations.
//
// What the third misses, stated rather than left to be found: a body that names a parameter
// and discards the value, which is a source shape no reading of the parse tree separates
// from a body that uses it. Measured: hpkeSuiteId reduced to "_ = params; return
// make([]byte, 10)" passes every shape here, and is caught by fourteen published vector
// tests. That is where the class sits for the declarations no behavioural gate reaches.
//
// Bodies of function literals are skipped for the RETURN and PANIC halves, so a return or a
// panic written inside a closure is read as the closure's and not as the declaration's.
//
// THE PARAMETER HALF READS THEM, and the difference is not a nicety. A parameter a body hands
// to a closure it builds is a parameter that body read; the two halves want opposite answers
// because a closure's own return is not the declaration's and a closure's own read IS. It was
// one reading until m1 wave 1 landed connect/messagegroup's GroupSession, whose section 3.6
// concurrency contract makes every public method post a command -- so every parameter of
// SealRecord, OpenRecord, TrackSender and AdvanceEpoch is read inside a function literal and
// nowhere else, and all eleven of them were reported as placeholders in a body that reads
// every one.
func providerStubShapesIn(parsed parsedSource) []string {
	shapes := []string{}
	for _, declaration := range parsed.file.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Body == nil {
			continue
		}
		where := parsed.file.Name.Name + "." + function.Name.Name
		returns := false
		outermost(function.Body, func(node ast.Node) {
			if _, isReturn := node.(*ast.ReturnStmt); isReturn {
				returns = true
			}
		})
		read := identifiersReadAnywhereIn(function.Body)
		if function.Type.Results != nil && len(function.Type.Results.List) != 0 && !returns {
			shapes = append(shapes, where+" declares a result and never returns one")
		}
		for _, statement := range function.Body.List {
			expression, isExpression := statement.(*ast.ExprStmt)
			if !isExpression {
				continue
			}
			call, isCall := expression.X.(*ast.CallExpr)
			if !isCall {
				continue
			}
			if identifier, isIdentifier := call.Fun.(*ast.Ident); isIdentifier && identifier.Name == "panic" {
				shapes = append(shapes, where+" panics as a statement of its body rather than inside a check")
			}
		}
		if function.Type.Params != nil {
			for _, field := range function.Type.Params.List {
				for _, name := range field.Names {
					if name.Name != "_" && !read[name.Name] {
						shapes = append(shapes, where+" does not read "+name.Name)
					}
				}
			}
		}
	}
	slices.Sort(shapes)
	return shapes
}

// The identifiers one body reads as values.
//
// The tail of a selector is not one of them, and neither is a field name written in a
// struct type inside the body. A method reading self.params.Nh mentions the identifiers
// params and Nh while reading nothing of either name, so counting every identifier would
// read a declaration taking a parameter called params as reading it whatever the body did
// with the argument. That is the alias defeat this project has already paid for once, in
// task 15, arriving through the other half of the same node.
//
// A key in a composite literal is counted, because telling a struct field name from a map
// key needs the type and this walk has none. Over counting there costs a name that is a
// parameter's name and a field's name at once, which is a narrower miss than the one it
// avoids.
func identifiersReadIn(body *ast.BlockStmt) map[string]bool {
	tails := map[*ast.Ident]bool{}
	outermost(body, func(node ast.Node) {
		switch found := node.(type) {
		case *ast.SelectorExpr:
			tails[found.Sel] = true
		case *ast.Field:
			for _, name := range found.Names {
				tails[name] = true
			}
		}
	})
	read := map[string]bool{}
	outermost(body, func(node ast.Node) {
		if identifier, isIdentifier := node.(*ast.Ident); isIdentifier && !tails[identifier] {
			read[identifier.Name] = true
		}
	})
	return read
}

// identifiersReadAnywhereIn is identifiersReadIn over the WHOLE body, function literals
// included.
//
// It is the parameter half's reading and providerStubShapesIn's comment says why the two
// differ. The tails filter is the same one and for the same reason: a selector's tail and a
// field name in a struct type are not reads of a parameter that shares their spelling.
func identifiersReadAnywhereIn(body *ast.BlockStmt) map[string]bool {
	tails := map[*ast.Ident]bool{}
	ast.Inspect(body, func(node ast.Node) bool {
		switch found := node.(type) {
		case *ast.SelectorExpr:
			tails[found.Sel] = true
		case *ast.Field:
			for _, name := range found.Names {
				tails[name] = true
			}
		}
		return true
	})
	read := map[string]bool{}
	ast.Inspect(body, func(node ast.Node) bool {
		if identifier, isIdentifier := node.(*ast.Ident); isIdentifier && !tails[identifier] {
			read[identifier.Name] = true
		}
		return true
	})
	return read
}

// The unexported declarations of one package that nothing in it names.
//
// A placeholder written to keep a build green is a declaration with a body and no caller,
// and none of the shapes above can see one: it compiles, it returns something of the right
// size, and a body that reads each of its parameters once satisfies every rule there.
// Measured, against the version this replaces: a two parameter placeholder appended to
// crypto.go, reading both of its parameters and called by nobody, passed all 2287 tests of
// this package.
//
// The class is the package's own source rather than a list. A name mentioned anywhere in
// it except at its own declaration is reached, which counts a function passed as a value
// and a method reached through an interface as well as a plain call, so the reading errs
// towards calling a declaration live. Exported declarations are the package's surface and
// are called from outside it; init is called by the runtime and named by nobody, and so is
// a main. What this leaves open is an exported placeholder, which is a different thing from
// a leftover: it is on the surface an auditor reads.
func uncalledDeclarationsIn(files []parsedSource) []string {
	declared := map[string]bool{}
	own := map[*ast.Ident]bool{}
	for _, parsed := range files {
		for _, declaration := range parsed.file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			name := function.Name.Name
			if ast.IsExported(name) || name == "init" || name == "main" {
				continue
			}
			declared[name] = true
			own[function.Name] = true
		}
	}
	named := map[string]bool{}
	for _, parsed := range files {
		ast.Inspect(parsed.file, func(node ast.Node) bool {
			if identifier, isIdentifier := node.(*ast.Ident); isIdentifier && !own[identifier] {
				named[identifier.Name] = true
			}
			return true
		})
	}
	uncalled := []string{}
	for name := range declared {
		if !named[name] {
			uncalled = append(uncalled, name)
		}
	}
	slices.Sort(uncalled)
	return uncalled
}

// Every node of one body except the bodies of the function literals inside it, so a return
// or a panic written in a closure is read as the closure's own.
func outermost(body *ast.BlockStmt, visit func(node ast.Node)) {
	ast.Inspect(body, func(node ast.Node) bool {
		if _, isLiteral := node.(*ast.FuncLit); isLiteral {
			return false
		}
		if node != nil {
			visit(node)
		}
		return true
	})
}

// A file holding one of each shape, so a matcher that stopped matching fails here rather
// than reporting the package clean. The last declaration is the negative half: a real panic
// in this package is the else of a check, and it must not be read as a refusal to run.
//
// stubThatReadsOneOfTwo is what says the parameter rule is every parameter rather than any
// one of them, and stubThatNamesAFieldInstead is what says an identifier in a selector's
// tail is not a read of a parameter that shares its spelling.
const providerStubShapeControl = `package control

func stubThatPanics(secret []byte) []byte {
	panic("mls: not implemented yet")
}

func stubThatAnswersZeroes(secret []byte, length int) []byte {
	return make([]byte, 32)
}

func stubThatReadsOneOfTwo(secret []byte, length int) []byte {
	return make([]byte, length)
}

func stubThatReadsItsParameterOnlyInsideAClosure(secret []byte, length int) []byte {
	answer := []byte(nil)
	run := func() { answer = append(secret, byte(length)) }
	run()
	return answer
}

func stubThatDropsAParameterAndClosesOverTheOther(secret []byte, length int) []byte {
	answer := []byte(nil)
	run := func() { answer = make([]byte, length) }
	run()
	return answer
}

func stubThatNamesAFieldInstead(params []byte, length int) []byte {
	answer := struct{ params []byte }{}
	return append(answer.params, byte(length))
}

func realDeclaration(secret []byte, length int) []byte {
	if length < 0 {
		panic("mls: a negative length")
	}
	answer := make([]byte, length)
	copy(answer, secret)
	return answer
}
`

// A package holding one reached declaration and one nothing names, for the same reason the
// shape control exists: a call graph reading that had stopped reading would report every
// package clean and pass.
const providerUncalledControl = `package control

func used(secret []byte) []byte {
	return secret
}

func notImplementedYet(secret []byte, length int) []byte {
	if len(secret) < 0 {
		return nil
	}
	return make([]byte, length)
}

func Exported(secret []byte) []byte {
	return used(secret)
}
`

// Unexported declarations landed ahead of the first production caller, named with what
// will call them.
//
// The call graph half below is right that a body nothing reaches is a body nothing
// checks, and this map is not an argument with it. It exists because a plan lands its
// shared helper in one task and its callers in the next, and the alternative is either a
// task that cannot be committed on its own or a helper written nine times. What keeps it
// from becoming a hiding place is that the entry expires by failing: the loop below
// reports a name here that no package declares and a name here that something now calls,
// so the excuse survives exactly as long as the condition it names.
//
// A declaration excused here is still held by its own tests. That is the difference
// between "no caller yet" and the placeholder this gate is about, which has no tests
// either.
//
// The key is the package the declaration is in and then its name, because the gate walks
// more than one package and a bare name is not an address in it. Keyed on the name alone,
// this table excused any declaration spelled that way anywhere under the roots -- an
// unrelated uncalled zeroizeSecret written in connect/message would be waved through by an
// entry about mls -- and, worse, it silenced its own expiry: the expiry check asks whether
// the excused name is still uncalled SOMEWHERE, so a foreign declaration of the same
// spelling keeps the excuse alive after the real one has found its caller. Measured in both
// directions before the key was widened. This is the base name exemption this project keeps
// rediscovering, transposed from file names onto symbol names.
// It held one entry before this one, and that entry expired exactly as it was written to:
// p4 task 5 landed key_schedule.go, DeriveJoinerSecret erases its pseudorandom key through
// zeroizeSecret, and the excuse died with the condition it named rather than outliving it.
// awaitingFirstCaller is one excuse: the production declaration whose arrival ends it, and
// the reason the excuse was written.
//
// firstCaller is a FIELD rather than a clause inside why, and that is the whole of what this
// type is for. The map's safety argument is expiry by failure, and the only expiry the loop
// below can observe is a PRODUCTION caller arriving -- so an excuse for a declaration that can
// never gain one never expires, and the map holds it forever while the declaration ships. That
// is not a hypothetical: a construction-bypass seam is a declaration whose whole purpose is
// that production must never call it, so its excuse is exactly the entry this table cannot
// expire, and the note at the foot of the map records the one that was nearly written.
//
// Every entry this table has ever held already stated its first caller, in prose, at the end
// of the why: "p6 task 11's SealPrivateMessage is the first production caller". Moving that
// clause into a field of its own is what turns the promise into something the gate can hold
// the entry to, and TestNoExcuseAwaitingAFirstCallerNamesAnExpiryThatCannotArrive is what does
// the holding. It refuses an entry with no first caller, an entry that names itself, and an
// entry whose named first caller is a declaration of the TEST BINARY -- which is the only
// truthful answer a seam has, because a seam's callers are tests and always will be.
type awaitingFirstCaller struct {
	// the production declaration whose arrival makes this entry expire, spelled the way
	// declarationsIn spells a declaration: a bare name, or Type.Method for a method.
	firstCaller string
	// why the declaration landed ahead of that caller.
	why string
}

var packageDeclarationsAwaitingTheirFirstCaller = map[string]awaitingFirstCaller{
	// It was empty, and that was the state to keep it in. Its one entry was takeLeafSecret,
	// excused between p4 task 21 landing the secret tree's descent and p4 task 22 landing the
	// ratchetFor that calls it, and it expired by FAILING the moment ratchetFor named it,
	// which is the only way an excuse of this kind ever comes back off.
	//
	// It is empty again, and every one of the six entries it has held expired the way this
	// gate is built to make them expire: by FAILING on the commit that gave the declaration a
	// caller, rather than by somebody remembering to come back for it.
	//
	// marshalBytes went first, on p5 task 4, when (*LeafKeysExtension).Encode began assembling
	// the urmessage_leaf_keys body through it. directPathOf followed on task 8, when
	// (*RatchetTree).BlankDirectPath started walking a leaf's direct path through it. The last
	// four were p5 task 3's tree_adapt.go and they all died together on task 12, the tree hash:
	// (*RatchetTree).TreeHash resolves at the root through rootOf, and the recursion under it
	// descends through leftOf and rightOf and tells a leaf slot from a parent slot through
	// leafIndexOf. rootOf's entry named task 10 and outlived it by two tasks, which is the one
	// thing an excuse of this kind is allowed to do: it names the task that was EXPECTED to
	// call it, and what takes it off is a caller arriving, not the task number passing.
	//
	// And it is not empty again. p6 task 6 lands verifyMembershipTag, which is ValSem007 and
	// ValSem008 on the receive path, and p6 task 7's PublicMessage open is the first thing that
	// has a membership tag to check. The declaration lands with this task rather than with that
	// one because the guardrail is about the SHAPE the refusal has -- an error and never a bool
	// -- and a task that landed the tag computation without its verifier would leave the shape
	// to whoever needed it first. This entry comes off by FAILING on the commit that gives it a
	// caller.
	// The verifyMembershipTag entry came off on p6 task 7, by FAILING on the commit that landed
	// OpenPublicMessage -- the caller it named, on the task it named. That is the seventh entry to
	// expire exactly the way this gate is built to make them expire, and none has ever come off
	// because somebody remembered it.
	//
	// The eighth is p6 task 8's. RFC 9420 section 6.3 has two AADs and this package builds the
	// content one BY CALLING the sender data one, so that the header both of them cover has a
	// single assembly; splitting the two across tasks would be two assemblies of one preimage,
	// agreeing until the day one of them changed, and nothing that round trips could see the
	// disagreement. So senderDataAAD has a caller from the moment it lands and privateContentAAD
	// does not. It is not untested -- the section 6.3 goldens, the collision sweep over the header
	// corpus, and the structural pair test all run it -- and this entry comes off by FAILING on the
	// commit that gives it a production caller.
	// The ninth and tenth are p6 task 9's. Section 6.3.2's seal and its open land TOGETHER,
	// because the property that matters about them is not that either works -- it is that the
	// open is the seal's inverse over the RFC's construction rather than over whatever the seal
	// happened to do, and a task that landed one without the other would leave that to whoever
	// needed the second. Neither has a production caller until p6 task 11 assembles the
	// PrivateMessage around them. Both entries come off by FAILING on the commit that gives them
	// one.
	//
	// And all three came off on p6 task 11, by FAILING on the commit that landed
	// SealPrivateMessage and OpenPrivateMessage -- the callers they named, on the task they
	// named. That is ten entries, every one of which expired the way this gate is built to make
	// them expire, and the table is empty again.
	//
	// It stayed empty through task 11, which is worth recording because the draft of that task
	// would have added an eleventh. The plan produces a count form of the section 6.3.1
	// serializer beside the octet form, and nothing in production takes a count at that level, so
	// the count form would have been a declaration excused here with no task to name as its first
	// caller -- an entry that could never expire, which is the one shape this table must not
	// hold. It lives in framing_protect_test.go instead, beside its only callers.
	//
	// The eleventh, twelfth and thirteenth are p7 tasks 4 and 5, and all three are the same
	// shape: a rule that has to exist before the path that runs it, so that the path does not
	// get to invent it. Each names the production declaration whose arrival takes the entry off
	// by FAILING, and none of the three names a task number, because what expires an excuse here
	// is a caller arriving and not a task passing.
	//
	// Two of those three came off on p7 task 6, by FAILING on the commit that landed
	// ProposalCache.Store and ProposalList.PathRequired -- the callers they named, on the task
	// that needed them. checkProposalProfile is now reached from both doors of the cache and
	// proposalTypePathRequired from the list's own predicate, so what is left is the one entry
	// whose caller has not landed.
	//
	// successionPreimage is the bytes an admin countersigns, MASTER section 11. It lands with the
	// nomination it is a preimage OVER, because a countersignature preimage split from the
	// structure it covers is two statements of one layout that agree until one of them changes,
	// and the half that moved is invisible to every round trip. p7 task 21's ValidateSuccession
	// is the first thing with a countersignature to check.
	"./successionPreimage": {firstCaller: "ValidateSuccession",
		why: "the countersignature preimage of MASTER section 11, landed beside the nomination it covers rather than beside the validator that reads it"},
}

// ---------------------------------------------------------------------------
// the excuse that could never expire
// ---------------------------------------------------------------------------

// excuseVerdicts is what one excuse table looks like when it is read against the declarations
// of every package the guardrails scan. Three lists rather than one, because the table can go
// wrong in three directions and a reader answering one verdict twice would report a clean bill
// for another.
type excuseVerdicts struct {
	// entries whose expiry the loop in TestNoStubShapesRemainInSource could never observe.
	cannotExpire []string
	// entries whose named first caller has LANDED in production. That is the sharper expiry:
	// the existing loop comes off when the declaration gains any caller at all, and this one
	// comes off on the commit that lands the caller the entry actually promised, whether or
	// not that caller ended up calling it.
	callerLanded []string
	// entries addressed at a package this reader holds no declarations for. That is neither
	// verdict: it is the reader saying it judged the entry by nothing. Reported rather than
	// skipped, because an address a reader steps over is an excuse nothing reads at all, and a
	// gate that steps over what it cannot judge reports exactly what a clean table reports --
	// which is the shape this whole file exists to refuse.
	unreadable []string
}

// declarationFileOf answers the file one name is declared in, or "".
//
// A method is spelled Type.Method by declarationsIn, and a caller writing an entry has no
// reason to know which of the two shapes a future declaration will take, so a bare name is
// resolved against both. Sorted, so a name that matched two receivers answers the same file on
// every run rather than moving between them.
func declarationFileOf(declaredIn map[string]string, name string) string {
	if file, declared := declaredIn[name]; declared {
		return file
	}
	for _, key := range slices.Sorted(maps.Keys(declaredIn)) {
		if strings.HasSuffix(key, "."+name) {
			return declaredIn[key]
		}
	}
	return ""
}

// readExcuses is the whole rule, and it is DERIVED from what the expiry loop can see rather
// than from any property of the excused declaration itself.
//
// The excused declaration's own name, file and shape are deliberately not read. That is the
// half rule 5 is about: this package holds errNilLeafOccupancyTest, a genuine internal guard
// that reads exactly like a test seam, and a classifier keyed on the spelling would call it
// one. What decides here is the entry's stated EXPIRY -- because an excuse is a promise that a
// production caller is coming, and the three ways that promise can be unkeepable are visible
// without knowing anything about the declaration being excused.
//
//   - no first caller at all. Nothing can arrive, so nothing can expire.
//   - the declaration itself. It would have to call itself to be called.
//   - a declaration of the TEST BINARY. The expiry loop reads production callers, so a test
//     arriving moves nothing it looks at. This is the seam case, and it is the reason the class
//     is read off the _test.go files of the package rather than off a list of names: a seam
//     written tomorrow, under any spelling, in any file, has the same only-truthful answer.
//
// declaredIn is keyed by PACKAGE -- the same key declarationAddress writes an entry under --
// and the resolution happens in the package the entry is ADDRESSED AT. That is not a choice,
// it is derived twice over: uncalledDeclarationsIn reports only unexported functions, and Go's
// own visibility rule makes an unexported function's callers its own package's; and the expiry
// loop groups files by package and asks each group about its own names. So the package in the
// address is the only package a first caller can be declared in and the only one whose arrival
// the expiry loop would notice.
//
// This read ONE package before -- the mls directory -- while forbiddenScanRoots has always held
// two and declarationAddress keys by package precisely so that a ../message address is legal.
// Every entry addressed at any other package resolved to "" and took the branch marked "the
// caller is not written yet", so a seam in connect/message excused by naming a ramp of
// connect/message's own test binary was waved through, and the gate then logged the promise it
// had not checked. Measured end to end before this was widened; the identical entry keyed at
// the mls package was refused, which is the whole of the defect.
//
// An address at a package declaredIn holds nothing for is its own verdict rather than silence,
// because a reader that skipped it would answer, for an entry it never looked at, exactly what
// it answers for a clean one.
//
// The honest limit is unchanged and is one sentence longer. An author who names a caller that
// will never be written in the excused declaration's own package -- including a name only some
// OTHER package declares, which cannot call an unexported declaration of this one -- states
// something false, and no reading of this tree tells that from a caller not yet written. What
// it costs to get past this gate is still a lie in the table rather than a correct entry.
func readExcuses(excuses map[string]awaitingFirstCaller, declaredIn map[string]map[string]string) excuseVerdicts {
	verdicts := excuseVerdicts{}
	for _, address := range slices.Sorted(maps.Keys(excuses)) {
		excuse := excuses[address]
		where, name := "", address
		if cut := strings.LastIndex(address, "/"); cut >= 0 {
			where, name = address[:cut], address[cut+1:]
		}
		declared, readable := declaredIn[where]
		switch {
		case excuse.firstCaller == "":
			verdicts.cannotExpire = append(verdicts.cannotExpire,
				address+": names no first caller")
		case excuse.firstCaller == name:
			verdicts.cannotExpire = append(verdicts.cannotExpire,
				address+": names itself as its own first caller")
		case !readable:
			verdicts.unreadable = append(verdicts.unreadable,
				address+": the scan groups no package under "+where+", so nothing judged this entry's expiry")
		default:
			file := declarationFileOf(declared, excuse.firstCaller)
			switch {
			case file == "":
				// the ordinary shape: the caller is not written yet, which is what the
				// excuse exists to say
			case strings.HasSuffix(file, "_test.go"):
				verdicts.cannotExpire = append(verdicts.cannotExpire,
					address+": "+excuse.firstCaller+" is declared by the test binary ("+file+")")
			default:
				verdicts.callerLanded = append(verdicts.callerLanded,
					address+": "+excuse.firstCaller+" has landed in "+file)
			}
		}
	}
	return verdicts
}

// declarationsOfEveryScannedPackage answers the package level declarations of every package the
// guardrail scan reaches, keyed by the package directory declarationAddress writes into an
// address.
//
// The set of packages is DERIVED from the scan, and from the scan rather than from
// forbiddenScanRoots directly, because the addresses this feeds are minted by
// TestNoStubShapesRemainInSource out of filepath.Dir of a scanned path. A package that is a
// SUBDIRECTORY of a root mints addresses of its own -- mls/syntax already is one -- and a
// reader keyed on the roots alone would hold nothing for them. Same walk, same grouping
// expression, same keys.
func declarationsOfEveryScannedPackage(t *testing.T) map[string]map[string]string {
	t.Helper()
	declared := map[string]map[string]string{}
	for path := range mustScanSources(t, forbiddenScanRoots).sourceTexts {
		where := filepath.ToSlash(filepath.Dir(path))
		if _, read := declared[where]; read {
			continue
		}
		declared[where] = packageLevelDeclarations(t, where)
	}
	// the guard on the walk rather than on any one package. A scan that read nothing groups
	// nothing and answers every entry "unreadable", which is loud; a scan that read one root
	// answers the other root's entries "not written yet", which is the exact silence this
	// reader was widened to end, and is what a walk failing on the second root would produce.
	// Both halves of the rule are decided by which side of _test.go a file falls, so each root
	// is required to contribute some of each -- a root reached with its test files skipped
	// reports every seam of it as awaiting a caller.
	for _, root := range forbiddenScanRoots {
		names, read := declared[filepath.ToSlash(root)]
		if !read {
			t.Fatalf("the scan grouped no package under %q, and it is one of the roots the guardrails walk (%v)",
				root, forbiddenScanRoots)
		}
		production, tests := 0, 0
		for _, file := range names {
			if strings.HasSuffix(file, "_test.go") {
				tests++
				continue
			}
			production++
		}
		if production == 0 || tests == 0 {
			t.Fatalf("%q contributed %d production and %d test declarations; a root missing either half is a root judged by half a rule",
				root, production, tests)
		}
	}
	return declared
}

// The control's production half. One declaration awaiting a caller nobody has written, one
// caller that HAS landed, and one package variable spelled the way a test seam is spelled while
// being production's own -- which is errNilLeafOccupancyTest's shape, put in the control so the
// rule is measured against it rather than asserted about it.
const excuseExpiryProductionControl = `package control

func awaitedByACallerNotWrittenYet() int { return 0 }

func awaitedByACallerThatHasLanded() int { return 1 }

func theCallerThatLanded() int { return awaitedByACallerThatHasLanded() }

var errNamedLikeATestSeamButDeclaredInProduction = 2
`

// The control's test half. Two declarations of the test binary: one a Test function, one a
// helper whose name says nothing about tests at all. Both are answers a seam's excuse could
// truthfully give, and the second is what says the rule is keyed on WHERE a declaration lives
// rather than on how it is spelled.
const excuseExpiryTestControl = `package control

func TestControlDrivesTheSeam() {}

func aHelperOfTheTestBinary() int { return 3 }
`

// The control's SECOND package. It is what says the reader resolves a first caller in the
// package the entry is addressed at rather than in one fixed directory, and it carries the same
// two shapes the first package does -- one production caller, one helper of a test binary --
// under names the first package does not declare. A reader that read only the first package and
// a reader that merged the two into one map are then different from this one, and the whole
// answer below is what separates all three.
const excuseExpiryOtherPackageProductionControl = `package elsewhere

func theCallerThatLandedInTheOtherPackage() int { return 4 }
`

// The second package's test half, whose spelling says nothing about tests for the same reason
// the first package's does: what decides is the file a declaration lives in.
const excuseExpiryOtherPackageTestControl = `package elsewhere

func aRampOfTheOtherPackagesTestBinary() int { return 5 }
`

// The control's excuse table: one row per verdict, plus the rows that must produce none.
var excuseExpiryControlTable = map[string]awaitingFirstCaller{
	"./awaitedByACallerNotWrittenYet": {firstCaller: "aCallerNoFileDeclaresYet",
		why: "the ordinary shape, and the row that must produce no verdict at all"},
	"./awaitedByACallerThatHasLanded": {firstCaller: "theCallerThatLanded",
		why: "the sharper expiry: the promised caller has landed"},
	"./aSeamADeclaringTestNames": {firstCaller: "TestControlDrivesTheSeam",
		why: "the hole: a seam whose only truthful first caller is a Test"},
	"./aSeamAHelperNames": {firstCaller: "aHelperOfTheTestBinary",
		why: "the same hole under a caller whose name says nothing about tests"},
	"./anExcuseNamingNobody": {firstCaller: "",
		why: "no expiry condition at all"},
	"./anExcuseNamingItself": {firstCaller: "anExcuseNamingItself",
		why: "an expiry that is its own arrival"},
	"./anExcuseNamingAProductionVar": {firstCaller: "errNamedLikeATestSeamButDeclaredInProduction",
		why: "a caller spelled like a test seam and declared in production, which is a LANDED verdict and not a refusal"},
	"../elsewhere/aSeamTheOtherPackagesTestNames": {firstCaller: "aRampOfTheOtherPackagesTestBinary",
		why: "the measured hole: the seam shape, one package over from the one this reader used to read"},
	"../elsewhere/awaitedByTheOtherPackagesCaller": {firstCaller: "theCallerThatLandedInTheOtherPackage",
		why: "the sharper expiry, one package over"},
	"./awaitedByANameOnlyTheOtherPackageDeclares": {firstCaller: "theCallerThatLandedInTheOtherPackage",
		why: "a name this package does not declare and the other one does: no verdict, because the lookup is per package and an unexported declaration here is unreachable from there"},
	"../nowhere/anExcuseAddressedAtAPackageNobodyScanned": {firstCaller: "aCallerNoFileDeclaresYet",
		why: "an address at a package the scan does not reach, which is the verdict that says the reader judged nothing"},
}

// The whole answer the control commits, written out rather than derived: an expectation
// computed from the same code it is controlling states nothing.
var excuseExpiryControlCannotExpire = []string{
	"../elsewhere/aSeamTheOtherPackagesTestNames: aRampOfTheOtherPackagesTestBinary is declared by the test binary (control_test.go)",
	"./aSeamADeclaringTestNames: TestControlDrivesTheSeam is declared by the test binary (control_test.go)",
	"./aSeamAHelperNames: aHelperOfTheTestBinary is declared by the test binary (control_test.go)",
	"./anExcuseNamingItself: names itself as its own first caller",
	"./anExcuseNamingNobody: names no first caller",
}

var excuseExpiryControlCallerLanded = []string{
	"../elsewhere/awaitedByTheOtherPackagesCaller: theCallerThatLandedInTheOtherPackage has landed in control.go",
	"./anExcuseNamingAProductionVar: errNamedLikeATestSeamButDeclaredInProduction has landed in control.go",
	"./awaitedByACallerThatHasLanded: theCallerThatLanded has landed in control.go",
}

var excuseExpiryControlUnreadable = []string{
	"../nowhere/anExcuseAddressedAtAPackageNobodyScanned: the scan groups no package under ../nowhere, so nothing judged this entry's expiry",
}

// declarationsOfTheExcuseControl runs the control's halves through declarationsIn, which is the
// same collector packageLevelDeclarations uses on the real packages, so the control is not a
// second reading of the source.
//
// It is keyed by PACKAGE for the same reason the real reader is, and the two packages give
// their halves the SAME file names on purpose: what decides a verdict is which side of _test.go
// a file falls on and which package it is in, never the file's name, and a control whose two
// packages used different names could not say that.
func declarationsOfTheExcuseControl(t *testing.T) map[string]map[string]string {
	t.Helper()
	declared := map[string]map[string]string{}
	for _, half := range []struct {
		where  string
		name   string
		source string
	}{
		{where: ".", name: "control.go", source: excuseExpiryProductionControl},
		{where: ".", name: "control_test.go", source: excuseExpiryTestControl},
		{where: "../elsewhere", name: "control.go", source: excuseExpiryOtherPackageProductionControl},
		{where: "../elsewhere", name: "control_test.go", source: excuseExpiryOtherPackageTestControl},
	} {
		parsed, err := parser.ParseFile(token.NewFileSet(), half.name, half.source,
			parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s/%s: %v", half.where, half.name, err)
		}
		if declared[half.where] == nil {
			declared[half.where] = map[string]string{}
		}
		declarationsIn(parsed, half.name, declared[half.where])
	}
	return declared
}

// TestNoExcuseAwaitingAFirstCallerNamesAnExpiryThatCannotArrive closes the one hole in
// packageDeclarationsAwaitingTheirFirstCaller.
//
// The table's entire safety argument is expiry by failure: an entry dies on the commit that
// gives its declaration a production caller, and the loop in TestNoStubShapesRemainInSource is
// what kills it. Ten entries have died that way and none was ever removed by somebody
// remembering it. What that argument does not cover is an entry for a declaration that can
// never GAIN a production caller -- a construction-bypass seam, whose whole purpose is that
// production must not call it. Its excuse would be correct on the day it was written, correct
// forever after, and the seam would sit in a shipped binary with a table entry saying it was
// being watched. The one entry that must never be written is the only kind the table cannot
// expire.
//
// So the shape of a never-expiring excuse is made unwritable rather than looked for after the
// fact: every entry states the production declaration whose arrival ends it, and readExcuses
// refuses the three ways that statement can be unkeepable. The class of "declaration of the
// test binary" is read off the package's own _test.go files, so it is the package that decides
// it and not a list here -- a seam added tomorrow under any name is refused by the same rule.
//
// The honest limit, recorded because a gate nobody knows the edge of is worse than none: this
// refuses the TRUTHFUL entry for a seam. An author who instead names a production caller that
// will never be written states something false, and no reading of this tree can see that today.
// What it costs to get past this gate is a lie in the table rather than a correct entry, which
// is the difference between an oversight and a decision.
func TestNoExcuseAwaitingAFirstCallerNamesAnExpiryThatCannotArrive(t *testing.T) {
	control := readExcuses(excuseExpiryControlTable, declarationsOfTheExcuseControl(t))
	if !slices.Equal(control.cannotExpire, excuseExpiryControlCannotExpire) {
		t.Errorf("the reader answered %v as unable to expire out of the control, want %v; a shape it lets through is a shape the real table can be written in",
			control.cannotExpire, excuseExpiryControlCannotExpire)
	}
	if !slices.Equal(control.callerLanded, excuseExpiryControlCallerLanded) {
		t.Errorf("the reader answered %v as having a landed caller out of the control, want %v; the three verdicts cross in this control, so a reader answering one of them twice fails here",
			control.callerLanded, excuseExpiryControlCallerLanded)
	}
	if !slices.Equal(control.unreadable, excuseExpiryControlUnreadable) {
		t.Errorf("the reader answered %v as judged by nothing out of the control, want %v; an address it steps over silently is an excuse nothing reads",
			control.unreadable, excuseExpiryControlUnreadable)
	}

	declared := declarationsOfEveryScannedPackage(t)
	// the guard on the SCAN rather than on the class, because a scan that read nothing reports
	// exactly what a scan over a clean table reports. One production declaration and one of the
	// test binary, so both halves of the rule are known to be reachable. The other roots are
	// held by declarationsOfEveryScannedPackage itself, which requires each of them to have
	// contributed declarations of both kinds.
	if file := declared[cryptoOwnRoot]["errNilLeafOccupancyTest"]; file != "framing_protect.go" {
		t.Fatalf("errNilLeafOccupancyTest is declared in %q and framing_protect.go certainly declares it, so this scan read something other than this package",
			file)
	}
	if file := declared[cryptoOwnRoot]["TestNoStubShapesRemainInSource"]; !strings.HasSuffix(file, "_test.go") {
		t.Fatalf("TestNoStubShapesRemainInSource is declared in %q, so the scan is not reading this package's test files and every seam would read as awaiting a caller",
			file)
	}

	verdicts := readExcuses(packageDeclarationsAwaitingTheirFirstCaller, declared)
	for _, verdict := range verdicts.cannotExpire {
		t.Errorf("%s; this excuse can never expire, so it is not an excuse -- if the declaration will never have a production caller it belongs in test source, beside the tests that are its only callers, the way framing_protect_test.go holds the section 6.3.1 count form",
			verdict)
	}
	for _, verdict := range verdicts.callerLanded {
		t.Errorf("%s; the caller this excuse promised has arrived, so the entry is a line to delete",
			verdict)
	}
	for _, verdict := range verdicts.unreadable {
		t.Errorf("%s; this entry was judged by nothing at all, which is the silence this gate exists to end -- either it is addressed at a package the guardrails do not walk, or it is spelled some way declarationAddress does not mint",
			verdict)
	}
	t.Logf("%d excuse(s) awaiting a first caller, each naming a production declaration that has not landed",
		len(packageDeclarationsAwaitingTheirFirstCaller))
}

// TestTheGuardThatReadsLikeATestSeamIsProductionsOwn pins errNilLeafOccupancyTest in both
// directions, the way rulesThisPackageExportsAndNothingApplies is pinned.
//
// It is the near miss the rule above is measured against. Its name ends in Test, it is answered
// only on a path a caller reaches by passing nil, and every test-seam classifier keyed on
// spelling would take it for one. It is not: CheckSenderLeaf returns it from production source,
// which is the thing a construction-bypass seam can never do.
//
// One direction: it is declared in production, so naming it as a first caller is a LANDED
// verdict rather than a refusal -- the rule is keyed on where a declaration lives and not on how
// it is spelled. The other: it really is a refusal a production error result answers, so it sits
// in the refusal roster's watched class rather than among the values nothing returns. If it ever
// became test-only, or stopped being returned, this fails rather than quietly moving the near
// miss into the class the rule above refuses.
func TestTheGuardThatReadsLikeATestSeamIsProductionsOwn(t *testing.T) {
	declared := declarationsOfEveryScannedPackage(t)
	probe := map[string]awaitingFirstCaller{
		"./aDeclarationAwaitingIt": {firstCaller: "errNilLeafOccupancyTest", why: "the near miss"},
	}
	verdicts := readExcuses(probe, declared)
	if len(verdicts.unreadable) != 0 {
		t.Fatalf("the reader judged the near miss by nothing (%v); the probe is addressed at this package and this package is a guardrail root, so the reader is not holding the packages it walked",
			verdicts.unreadable)
	}
	if len(verdicts.cannotExpire) != 0 {
		t.Errorf("naming errNilLeafOccupancyTest as a first caller was refused as unable to expire (%v); the rule is reading the spelling of a name rather than the file it is declared in, and this package's genuine internal guards are spelled that way",
			verdicts.cannotExpire)
	}
	if !slices.Equal(verdicts.callerLanded, []string{
		"./aDeclarationAwaitingIt: errNilLeafOccupancyTest has landed in framing_protect.go"}) {
		t.Errorf("the landed verdict for errNilLeafOccupancyTest is %v, so the reader did not resolve it to production source at all",
			verdicts.callerLanded)
	}

	scan := scanRefusals(refusalSourcesOfThisPackage(t))
	if _, watched := scan.refusals["errNilLeafOccupancyTest"]; !watched {
		t.Errorf("errNilLeafOccupancyTest is not a refusal any production error result answers; it has become the very thing the rule above refuses to excuse, and CheckSenderLeaf has lost its nil occupancy guard")
	}
	if slices.Contains(scan.declaredNotReturned, "errNilLeafOccupancyTest") {
		t.Errorf("errNilLeafOccupancyTest has moved into the values nothing returns, so the guard it names is gone")
	}
}

// aDeclarationOfEachHalfOf answers one bare name the TEST BINARY of a package declares and one
// its PRODUCTION source declares, chosen out of what the scan already read rather than written
// out here.
//
// Derived, and per root, because the property below is the same at every root the guardrails
// walk and a name typed here would be a name about one of them. Sorted keys, so the choice is
// the same on every run rather than whichever one the map happened to hand back first.
//
// Bare names only. A method is spelled Type.Method and declarationFileOf resolves a bare name
// against both shapes, so a name carrying a dot would exercise that resolver's second arm
// rather than the per package lookup this is here to reach.
func aDeclarationOfEachHalfOf(t *testing.T, where string, declared map[string]string) (ofTheTestBinary string, inProduction string) {
	t.Helper()
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		if strings.Contains(name, ".") {
			continue
		}
		if strings.HasSuffix(declared[name], "_test.go") {
			if ofTheTestBinary == "" {
				ofTheTestBinary = name
			}
			continue
		}
		if inProduction == "" {
			inProduction = name
		}
	}
	if ofTheTestBinary == "" || inProduction == "" {
		t.Fatalf("%s answered %q as a declaration of its test binary and %q as one of its production source; a root missing either half cannot say which of the two the reader is keyed on",
			where, ofTheTestBinary, inProduction)
	}
	return ofTheTestBinary, inProduction
}

// TestTheExcuseReaderJudgesASeamAtEveryRootTheGuardrailsWalk is the composition neither of the
// two things already holding readExcuses can state.
//
// The reader resolves an entry's first caller in the package the entry is ADDRESSED AT, and
// that is what was widened this batch, from the one directory it used to read to every package
// the guardrail scan groups. excuseExpiryControlTable states the rule over two SYNTHETIC
// packages, so it says the reader is per package while saying nothing about the packages this
// tree has; and packageDeclarationsAwaitingTheirFirstCaller is empty, and has to stay empty, so
// the real table exercises the reader over nothing at all. Between them sits the join: that the
// map declarationsOfEveryScannedPackage builds out of the real roots is one the reader can
// address.
//
// Measured, which is why this exists rather than being argued for. The arrangement that ought to
// prove the widening -- an uncalled declaration in ../message, excused by an entry naming a
// declaration of ../message's own test binary -- was landed in the real table with the reader
// narrowed back to one directory, and the real table's loop reported nothing at all: the entry
// resolved to "" and took the branch marked "the caller is not written yet", which is the exact
// silence the widening was for. Only the synthetic control failed. So on that arrangement the
// arm saying the widening is load bearing on THIS tree was the control, and the subject was
// judged by nothing.
//
// A probe closes it without landing a seam, which matters because the seam that would exercise
// the real table is a declaration this package must not ship. Both halves at every root: a
// reader answering cannotExpire for everything and one answering callerLanded for everything
// each satisfy one of them, and both are derived off the same map rather than named here.
func TestTheExcuseReaderJudgesASeamAtEveryRootTheGuardrailsWalk(t *testing.T) {
	declared := declarationsOfEveryScannedPackage(t)
	for _, root := range forbiddenScanRoots {
		where := filepath.ToSlash(root)
		ofTheTestBinary, inProduction := aDeclarationOfEachHalfOf(t, where, declared[where])
		seam := declarationAddress(where, "aSeamNoProductionCallerWillEverReach")

		// the seam's only truthful expiry, at this root: a declaration of this root's own
		// test binary, which the expiry loop can never observe arriving
		verdicts := readExcuses(map[string]awaitingFirstCaller{seam: {
			firstCaller: ofTheTestBinary,
			why:         "the seam shape, addressed at this root and naming this root's own test binary",
		}}, declared)
		if want := []string{seam + ": " + ofTheTestBinary + " is declared by the test binary (" +
			declared[where][ofTheTestBinary] + ")"}; !slices.Equal(verdicts.cannotExpire, want) {
			t.Errorf("at %s the reader answered %v as unable to expire, want %v; a seam excused at this root is one this reader is not judging, and its entry would sit in a shipped binary saying the declaration was being watched",
				where, verdicts.cannotExpire, want)
		}
		if len(verdicts.unreadable) != 0 {
			t.Errorf("at %s the reader judged the seam by nothing (%v); it holds no declarations for a package the guardrails walked, so an entry addressed there is read by neither half of the rule",
				where, verdicts.unreadable)
		}

		// and the other half at the same address, so the answer above is about the FILE a
		// declaration lives in rather than about the address or the spelling
		verdicts = readExcuses(map[string]awaitingFirstCaller{seam: {
			firstCaller: inProduction,
			why:         "the same address, naming this root's own production source",
		}}, declared)
		if want := []string{seam + ": " + inProduction + " has landed in " +
			declared[where][inProduction]}; !slices.Equal(verdicts.callerLanded, want) {
			t.Errorf("at %s the reader answered %v as having a landed caller, want %v; the sharper expiry is what takes an entry off on the commit that lands the caller it promised, and at this root it is answering something else",
				where, verdicts.callerLanded, want)
		}
		if len(verdicts.cannotExpire) != 0 {
			t.Errorf("at %s naming a production declaration was refused as unable to expire (%v); the rule is reading the spelling of a name rather than the file it is declared in",
				where, verdicts.cannotExpire)
		}
		t.Logf("%s: %s (%s) is a seam's only truthful expiry there, and %s (%s) is a landed one",
			where, ofTheTestBinary, declared[where][ofTheTestBinary],
			inProduction, declared[where][inProduction])
	}
}

// declarationAddress is the key packageDeclarationsAwaitingTheirFirstCaller is written in:
// the package directory the gate grouped a file under, then the declaration's name. It is
// one function so the table, the sweep and the expiry check cannot spell it three ways.
func declarationAddress(where string, name string) string {
	return where + "/" + name
}

// No declaration anywhere under the guardrail roots is a placeholder.
//
// The roots are the ones the primitive guardrails already walk, which is this package and
// connect/message, so a stub written in either is read. Scanning one package would leave
// the other's placeholders to be found by a caller, and this plan lands code in both.
//
// Two readings, because a placeholder has two ways of being invisible. The shapes are per
// file and say what a body does with what it was handed. The call graph is per package and
// says whether anything reaches the body at all, which is the half that catches a
// declaration whose body is a perfectly well formed function of nothing anybody calls.
//
// Both matchers run on a control as well as on the source. Without that half a matcher that
// had stopped matching -- a parse that silently failed, a shape rule narrowed by an edit --
// would report every file clean and pass, which is the one outcome a gate must never be
// able to produce by accident.
func TestNoStubShapesRemainInSource(t *testing.T) {
	control := providerStubShapesIn(mustParseText(t, "the stub shape control", providerStubShapeControl))
	want := []string{
		"control.stubThatAnswersZeroes does not read length",
		"control.stubThatAnswersZeroes does not read secret",
		// the closure half, in the direction that must still be reported: this one drops
		// secret entirely and closes over length. Its twin,
		// stubThatReadsItsParameterOnlyInsideAClosure, reads BOTH inside a closure and is
		// absent from this list, which is the other direction of the same reading.
		"control.stubThatDropsAParameterAndClosesOverTheOther does not read secret",
		"control.stubThatNamesAFieldInstead does not read params",
		"control.stubThatPanics declares a result and never returns one",
		"control.stubThatPanics does not read secret",
		"control.stubThatPanics panics as a statement of its body rather than inside a check",
		"control.stubThatReadsOneOfTwo does not read secret",
	}
	if !slices.Equal(control, want) {
		t.Errorf("the shape matcher read %v out of the control, want %v", control, want)
	}
	reached := uncalledDeclarationsIn([]parsedSource{mustParseText(t, "the uncalled control", providerUncalledControl)})
	if !slices.Equal(reached, []string{"notImplementedYet"}) {
		t.Errorf("the call graph matcher read %v out of its control, want [notImplementedYet]", reached)
	}
	scanned := 0
	packages := map[string][]parsedSource{}
	for path, text := range productionSources(mustScanSources(t, forbiddenScanRoots).sourceTexts) {
		scanned++
		parsed := mustParseText(t, path, text)
		if shapes := providerStubShapesIn(parsed); len(shapes) != 0 {
			t.Errorf("%s still holds a placeholder: %v", path, shapes)
		}
		where := filepath.ToSlash(filepath.Dir(path))
		packages[where] = append(packages[where], parsed)
	}
	if scanned == 0 {
		t.Fatalf("the scan read no non test source, so this gate examined nothing")
	}
	if len(packages) == 0 {
		t.Fatalf("the scan grouped no package, so the call graph half examined nothing")
	}
	stillUncalled := []string{}
	for where, files := range packages {
		reported := []string{}
		for _, name := range uncalledDeclarationsIn(files) {
			address := declarationAddress(where, name)
			stillUncalled = append(stillUncalled, address)
			if _, isExcused := packageDeclarationsAwaitingTheirFirstCaller[address]; !isExcused {
				reported = append(reported, name)
			}
		}
		if len(reported) != 0 {
			t.Errorf("%s declares %v, which nothing in it names", where, reported)
		}
	}
	// and the excuse expires by failing rather than by being remembered: an address here
	// that no package declares, or that something now calls, is a line to delete. the
	// address carries the package, so an unrelated declaration of the same name in the
	// other root neither excuses this one nor keeps its excuse alive.
	for address, excuse := range packageDeclarationsAwaitingTheirFirstCaller {
		if !slices.Contains(stillUncalled, address) {
			t.Errorf("%s is excused as awaiting its first caller (%s), and it is now either called or gone; delete the entry",
				address, excuse.why)
		}
	}
}

// The receiver fields one method of the provider reads, as "field", found wherever the
// method is declared rather than in a file this gate names.
//
// It is what the four size and code point methods are held by beyond the registry
// comparison. Both registered suites fix Nh at 32 and Nn at 12, so no input separates a
// HashSize that answers the registry from one that answers the literal 32, and the
// comparison above passes over both -- measured, not assumed: a HashSize hardcoded to 32
// and a NonceSize hardcoded to 12 each pass all 2276 tests of this package. Reading the
// declaration is what says the answer came from the suite the provider was built for. It
// is a source shape rather than a behaviour, and the honest limit of it is that a body
// which reads params and then ignores the value still passes.
func providerReceiverFieldReads(t *testing.T, method string) []string {
	t.Helper()
	for _, path := range packageLevelFunctions(t).files {
		parsed := mustParseSource(t, path)
		if !slices.Contains(parsed.methodsOn(providerReceiver), method) {
			continue
		}
		declaration := parsed.declarationOf(t, providerReceiver, method)
		names := declaration.Recv.List[0].Names
		if len(names) != 1 {
			t.Fatalf("the declaration of %s %s has no receiver name to read fields of", providerReceiver, method)
		}
		fields := []string{}
		ast.Inspect(declaration.Body, func(node ast.Node) bool {
			selector, isSelector := node.(*ast.SelectorExpr)
			if !isSelector {
				return true
			}
			if base, isIdentifier := selector.X.(*ast.Ident); isIdentifier && base.Name == names[0].Name {
				fields = append(fields, selector.Sel.Name)
			}
			return true
		})
		slices.Sort(fields)
		return slices.Compact(fields)
	}
	t.Fatalf("no file of this package declares %s %s", providerReceiver, method)
	return nil
}

// The type an entropy source is spelled as, for the messages below and for nothing else.
//
// What decides the class is the type the compiler resolves, not this spelling. A filter
// comparing rendered parameter names to "io.Reader" is a one element list, and a list is
// what this package has been walked past eleven times: a type alias to io.Reader renders
// as its own name, is io.Reader to the compiler, and a function taking one is read by
// such a filter as taking no entropy source at all. The gate reading the filter then
// holds four functions and reports exactly what a gate holding five reports.
const entropySourceTypeName = "io.Reader"

// The root of the package these gates can call into, spelled as forbiddenScanRoots
// spells it so the two can be compared rather than assumed equal.
const cryptoOwnRoot = "."

// One of the crypto's packages as the type checker reads it. The files are parsed here
// rather than through mustParseSource because the type information is keyed by the
// expression nodes of this parse, so a second parse of the same text carries none of it.
type checkedPackage struct {
	root  string
	paths []string
	files []parsedSource
	info  *types.Info
	pkg   *types.Package
}

var cryptoTypeCheckMutex sync.Mutex
var cryptoTypeChecked = map[string]checkedPackage{}
var cryptoTypeImporterOnce sync.Once
var cryptoTypeImporterValue types.Importer

// One importer for every check, so the io package the entropy type is taken from is one
// object rather than one per walk. It resolves a dependency from that dependency's own
// source, which needs no build cache and no go command: measured, the whole walk of this
// package costs about two seconds and completes with the go tool off the path entirely.
func cryptoTypeImporter() types.Importer {
	cryptoTypeImporterOnce.Do(func() {
		cryptoTypeImporterValue = importer.ForCompiler(token.NewFileSet(), "source", nil)
	})
	return cryptoTypeImporterValue
}

// The non test go files of one root, refusing a root that held none for the reason
// cryptoSourcePaths does: a walk that read nothing reports exactly what a walk over clean
// source reports.
func rootSourcePaths(t *testing.T, root string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(root, "*.go"))
	if err != nil {
		t.Fatalf("list the source of %s: %v", root, err)
	}
	paths := []string{}
	for _, path := range found {
		if !strings.HasSuffix(path, "_test.go") {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		t.Fatalf("%s holds no non test go file, so every gate reading it demands nothing", root)
	}
	slices.Sort(paths)
	return paths
}

// The type checker's reading of one root's non test source, computed once.
//
// A failed check is fatal rather than a fall back to reading names. A gate that could not
// resolve its subject must fail; one that quietly reverted to matching the spelling would
// go on reporting the clean run of a complete gate while holding a narrower class, which
// is the failure this whole file is written against.
func typeCheckedRoot(t *testing.T, root string) checkedPackage {
	t.Helper()
	cryptoTypeCheckMutex.Lock()
	defer cryptoTypeCheckMutex.Unlock()
	if checked, done := cryptoTypeChecked[root]; done {
		return checked
	}
	checked := typeCheckedFiles(t, root, rootSourcePaths(t, root), nil)
	cryptoTypeChecked[root] = checked
	return checked
}

// The same, over text a control holds rather than over a root, so every matcher below runs
// on a package known to violate the rule as well as on the real one.
func typeCheckedText(t *testing.T, name string, source string) checkedPackage {
	t.Helper()
	cryptoTypeCheckMutex.Lock()
	defer cryptoTypeCheckMutex.Unlock()
	return typeCheckedFiles(t, name, []string{name}, map[string]string{name: source})
}

// One package checked, from files on disk or from text a control holds. The caller holds
// the importer mutex: the source importer carries a cache and is not safe to enter twice
// at once.
func typeCheckedFiles(t *testing.T, path string, names []string, text map[string]string) checkedPackage {
	t.Helper()
	fileSet := token.NewFileSet()
	parsed := []parsedSource{}
	files := []*ast.File{}
	for _, name := range names {
		var source any
		if held, written := text[name]; written {
			source = held
		} else {
			read, err := os.ReadFile(name)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			source = read
		}
		file, err := parser.ParseFile(fileSet, name, source, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed = append(parsed, parsedSource{fileSet: fileSet, file: file})
		files = append(files, file)
	}
	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Defs:  map[*ast.Ident]types.Object{},
	}
	refused := []string{}
	config := types.Config{
		Importer:         cryptoTypeImporter(),
		IgnoreFuncBodies: true,
		Error:            func(err error) { refused = append(refused, err.Error()) },
	}
	pkg, err := config.Check(path, fileSet, files, info)
	if err != nil || len(refused) != 0 {
		t.Fatalf("type check %s: %v %s", path, err, strings.Join(refused, "; "))
	}
	return checkedPackage{root: path, paths: names, files: parsed, info: info, pkg: pkg}
}

// The type an entropy source is, taken from the io package the crypto is compiled against
// rather than from a name written here.
func entropySourceType(t *testing.T) types.Type {
	t.Helper()
	cryptoTypeCheckMutex.Lock()
	imported, err := cryptoTypeImporter().Import("io")
	cryptoTypeCheckMutex.Unlock()
	if err != nil {
		t.Fatalf("import io to read the type an entropy source is: %v", err)
	}
	reader := imported.Scope().Lookup("Reader")
	if reader == nil {
		t.Fatalf("the io package declares no Reader, so the class below is read off nothing")
	}
	return reader.Type()
}

// The type the crypto answers a provider as, likewise read off the package rather than
// off a spelling.
func providerInterfaceType(t *testing.T) types.Type {
	t.Helper()
	declared := typeCheckedRoot(t, cryptoOwnRoot).pkg.Scope().Lookup(providerInterfaceName)
	if declared == nil {
		t.Fatalf("this package declares no %s, so the constructor class below is read off nothing",
			providerInterfaceName)
	}
	return declared.Type()
}

// One function of a root's non test source as both readings see it: the name the
// declaration gives it, the parameter names an argument is resolved by, and the signature
// the compiler assigned it.
type declaredFunction struct {
	name       string
	parameters []providerParameter
	signature  *types.Signature
	// whether the declaration carries a receiver. The provider's methods are reached
	// through the interface, so the construction class is the package level half, and a
	// filter reading that off the rendered name would depend on how one is spelled.
	method bool
}

// Whether one type is another, as the compiler reads it: the same type, or a type
// declared over the identical underlying one.
//
// Both halves are the same defect twice. An alias is the same type and a rendered name
// filter still walks past it. A defined type over the identical interface is a different
// type carrying the same method set, which a caller converts into and which reads exactly
// the same bytes, and a class matching only the first would report a function taking one
// as taking no entropy source at all.
func sameTypeAs(want types.Type) func(types.Type) bool {
	return func(found types.Type) bool {
		return types.Identical(found, want) || types.Identical(found.Underlying(), want.Underlying())
	}
}

// The positions of the parameters whose type the compiler reads as want.
//
// Positions rather than names or spellings, because what a call has to put a value at is a
// position, and what the line spells the type as is not what decides whether it is one.
func (self declaredFunction) takes(want types.Type) []int {
	is := sameTypeAs(want)
	at := []int{}
	for i := 0; i < self.signature.Params().Len(); i++ {
		if is(self.signature.Params().At(i).Type()) {
			at = append(at, i)
		}
	}
	return at
}

// The position of the first result whose type is want, or -1 for a function that answers
// none.
func (self declaredFunction) answersAt(want types.Type) int {
	is := sameTypeAs(want)
	for i := 0; i < self.signature.Results().Len(); i++ {
		if is(self.signature.Results().At(i).Type()) {
			return i
		}
	}
	return -1
}

// Every function a checked package declares, package level and method alike.
//
// Methods are read too, and that is not tidiness. This class was read off package level
// functions alone, with a skip on every declaration carrying a receiver, and a method on
// the one implementation taking an entropy source and answering a nil one with the
// process source belonged to neither this class nor the provider surface class, which
// derives off the interface. A receiver was the whole of what it took to be invisible to
// both, and the substitution the refusal gate exists to forbid survived the suite when it
// was written that way.
func declaredFunctionsIn(t *testing.T, checked checkedPackage) []declaredFunction {
	t.Helper()
	functions := []declaredFunction{}
	for _, parsed := range checked.files {
		for _, declaration := range parsed.file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction {
				continue
			}
			name := declaredFunctionName(parsed, function)
			defined, isDefined := checked.info.Defs[function.Name]
			if !isDefined {
				t.Fatalf("the type checker read no definition of %s in %s", name, checked.root)
			}
			signature, isSignature := defined.Type().(*types.Signature)
			if !isSignature {
				t.Fatalf("the type checker reads %s as a %s rather than as a function", name, defined.Type())
			}
			parameters := parametersOf(t, parsed, name, function.Type)
			if len(parameters) != signature.Params().Len() {
				t.Fatalf("%s is written with %d parameters and the compiler sees %d",
					name, len(parameters), signature.Params().Len())
			}
			functions = append(functions, declaredFunction{
				name: name, parameters: parameters, signature: signature,
				method: function.Recv != nil,
			})
		}
	}
	return functions
}

// Every function one root declares.
func declaredFunctionsOf(t *testing.T, root string) []declaredFunction {
	t.Helper()
	return declaredFunctionsIn(t, typeCheckedRoot(t, root))
}

// The name one declaration is keyed by: a package level function by its own name, a method
// as (receiver).name. It is the spelling the type checker's own reading below produces, so
// the two readings can be compared rather than trusted to agree.
func declaredFunctionName(parsed parsedSource, function *ast.FuncDecl) string {
	receiver := parsed.receiverOf(function)
	if receiver == "" {
		return function.Name.Name
	}
	return "(" + receiver + ")." + function.Name.Name
}

// The names of the functions of a checked package whose signature a predicate selects,
// read off the type checker's own scope rather than off any declaration.
//
// The repetition is the point. A walk that stopped descending into a file, or a filter that
// stopped matching, returns a shorter class, and every gate reading it demands less while
// reporting exactly what a complete one reports. These two readings share the type check
// and nothing else: this one never looks at a parse tree.
//
// Named interfaces are skipped. An interface method is a declaration with no body and
// nothing to substitute inside it, and it is not a function declaration, so counting it
// here would put the two readings permanently out of step. What implements it is a method
// on a concrete type, which both readings see.
func typeCheckedFunctionsMatching(t *testing.T, checked checkedPackage, matches func(*types.Signature) bool) []string {
	t.Helper()
	names := []string{}
	relative := types.RelativeTo(checked.pkg)
	scope := checked.pkg.Scope()
	for _, name := range scope.Names() {
		switch object := scope.Lookup(name).(type) {
		case *types.Func:
			if matches(object.Signature()) {
				names = append(names, name)
			}
		case *types.TypeName:
			named, isNamed := object.Type().(*types.Named)
			if !isNamed {
				continue
			}
			if _, isInterface := named.Underlying().(*types.Interface); isInterface {
				continue
			}
			for i := 0; i < named.NumMethods(); i++ {
				method := named.Method(i)
				if !matches(method.Signature()) {
					continue
				}
				names = append(names,
					"("+types.TypeString(method.Signature().Recv().Type(), relative)+")."+method.Name())
			}
		}
	}
	slices.Sort(names)
	return names
}

// A signature that takes a want somewhere in its parameters.
func signatureTaking(want types.Type) func(*types.Signature) bool {
	is := sameTypeAs(want)
	return func(signature *types.Signature) bool {
		for i := 0; i < signature.Params().Len(); i++ {
			if is(signature.Params().At(i).Type()) {
				return true
			}
		}
		return false
	}
}

// A signature that answers a want somewhere in its results.
func signatureAnswering(want types.Type) func(*types.Signature) bool {
	is := sameTypeAs(want)
	return func(signature *types.Signature) bool {
		for i := 0; i < signature.Results().Len(); i++ {
			if is(signature.Results().At(i).Type()) {
				return true
			}
		}
		return false
	}
}

// A value one of the entropy gates calls, bound over whatever the call needs. A package
// level function is callable as it stands; a method needs a receiver, which is why every
// entry is a binder rather than a bare function value.
type entropyTakingBinder func(t *testing.T, params *SuiteParams) reflect.Value

// A package level function, which is its own value.
func entropyTakingValue(function any) entropyTakingBinder {
	return func(t *testing.T, params *SuiteParams) reflect.Value { return reflect.ValueOf(function) }
}

// Every value the entropy gates call, keyed by the name the declaration gives it: a
// package level function by its own name, a method as (receiver).name.
//
// The map is not the class. The class is read off the type checked package, and a function
// it names with no value here is fatal rather than a row left out, so a sixth function
// taking an entropy source is covered the day somebody declares it rather than the day
// somebody remembers to add it. A method is written here as a binder that builds its
// receiver over a source of the gate's own and returns
// reflect.ValueOf(receiver).MethodByName(name), so the source under test is still the one
// the call is handed rather than one the receiver is already holding.
var entropyTakingFunctionValues = map[string]entropyTakingBinder{
	"NewCryptoProviderWithRandom": entropyTakingValue(NewCryptoProviderWithRandom),
	"X25519GenerateKey":           entropyTakingValue(X25519GenerateKey),
	"HpkeSetupBaseS":              entropyTakingValue(HpkeSetupBaseS),
	"HpkeSealBase":                entropyTakingValue(HpkeSealBase),
	"hpkeEncap":                   entropyTakingValue(hpkeEncap),
}

// One function of this package handed a caller's entropy source, with the parameter names
// the declaration gives it and the positions the source is passed at.
type entropyTakingFunction struct {
	name       string
	parameters []providerParameter
	sources    []int
	bind       entropyTakingBinder
}

// Every function of this package that takes an entropy source, read off the type checked
// package, cross checked against the type checker's own scope, with a value required for
// each.
func entropyTakingFunctions(t *testing.T) []entropyTakingFunction {
	t.Helper()
	source := entropySourceType(t)
	functions := []entropyTakingFunction{}
	found := []string{}
	for _, declared := range declaredFunctionsOf(t, cryptoOwnRoot) {
		sources := declared.takes(source)
		if len(sources) == 0 {
			continue
		}
		bind, written := entropyTakingFunctionValues[declared.name]
		if !written {
			t.Fatalf("%s takes an %s and entropyTakingFunctionValues holds no value for it, so this gate cannot call it",
				declared.name, entropySourceTypeName)
		}
		found = append(found, declared.name)
		functions = append(functions, entropyTakingFunction{
			name: declared.name, parameters: declared.parameters, sources: sources, bind: bind,
		})
	}
	if len(functions) == 0 {
		t.Fatalf("no function of this package takes an %s, so the gate reading this demands nothing",
			entropySourceTypeName)
	}
	slices.Sort(found)
	scoped := typeCheckedFunctionsMatching(t, typeCheckedRoot(t, cryptoOwnRoot), signatureTaking(source))
	if !slices.Equal(found, scoped) {
		t.Fatalf("this gate reads %v as the functions taking an %s and the type checker's own scope reads %v",
			found, entropySourceTypeName, scoped)
	}
	for name := range entropyTakingFunctionValues {
		if !slices.Contains(found, name) {
			t.Errorf("entropyTakingFunctionValues names %s, which is not a function of this package taking an %s",
				name, entropySourceTypeName)
		}
	}
	return functions
}

// Every value the constructor gate calls, by the same rule: the class is read off the type
// checked package and a constructor with no value here is fatal rather than a row left out,
// so a third way to build a provider is held the day it is declared.
var providerBuildingFunctionValues = map[string]entropyTakingBinder{
	"NewCryptoProvider":           entropyTakingValue(NewCryptoProvider),
	"NewCryptoProviderWithRandom": entropyTakingValue(NewCryptoProviderWithRandom),
}

// One way to build a provider: where a source goes in, and where the provider comes back.
type providerBuildingFunction struct {
	name       string
	parameters []providerParameter
	sources    []int
	answersAt  int
	bind       entropyTakingBinder
}

// Every function of this package that answers a provider.
func providerBuildingFunctions(t *testing.T) []providerBuildingFunction {
	t.Helper()
	provider := providerInterfaceType(t)
	source := entropySourceType(t)
	functions := []providerBuildingFunction{}
	found := []string{}
	for _, declared := range declaredFunctionsOf(t, cryptoOwnRoot) {
		answersAt := declared.answersAt(provider)
		if answersAt < 0 {
			continue
		}
		bind, written := providerBuildingFunctionValues[declared.name]
		if !written {
			t.Fatalf("%s answers a %s and providerBuildingFunctionValues holds no value for it, so this gate cannot call it",
				declared.name, providerInterfaceName)
		}
		found = append(found, declared.name)
		functions = append(functions, providerBuildingFunction{
			name:       declared.name,
			parameters: declared.parameters,
			sources:    declared.takes(source),
			answersAt:  answersAt,
			bind:       bind,
		})
	}
	if len(functions) == 0 {
		t.Fatalf("no function of this package answers a %s, so the gate reading this demands nothing",
			providerInterfaceName)
	}
	slices.Sort(found)
	scoped := typeCheckedFunctionsMatching(t, typeCheckedRoot(t, cryptoOwnRoot), signatureAnswering(provider))
	if !slices.Equal(found, scoped) {
		t.Fatalf("this gate reads %v as the functions answering a %s and the type checker's own scope reads %v",
			found, providerInterfaceName, scoped)
	}
	for name := range providerBuildingFunctionValues {
		if !slices.Contains(found, name) {
			t.Errorf("providerBuildingFunctionValues names %s, which is not a function of this package answering a %s",
				name, providerInterfaceName)
		}
	}
	return functions
}

// The arguments beside the entropy source, keyed the way providerStubArgument resolves
// them: a declared type name where the type decides which value belongs, a parameter name
// where it does not. The recipient key comes from a provider of this gate's own, so the
// call under test still draws from a source nothing has touched.
func entropyTakingArguments(t *testing.T, params *SuiteParams) map[string]any {
	t.Helper()
	fixture := mustProviderOver(t, params.Suite, providerStubStream(0x01))
	_, pub, err := fixture.DeriveKeyPair(ascendingBytes(0x50, 32))
	if err != nil {
		t.Fatalf("derive the recipient key every row seals to: %v", err)
	}
	return map[string]any{
		"CipherSuite":   params.Suite,
		"*SuiteParams":  params,
		"HpkePublicKey": pub,
		"info":          ascendingBytes(0x80, 24),
		"aad":           ascendingBytes(0x90, 16),
		"plaintext":     ascendingBytes(0xa0, 20),
	}
}

// The error a call answered with, and whether it declared one at all. The second half is
// not bookkeeping: an operation with no error result has no way to refuse an input except
// to panic, and a gate that read a missing error as a nil one would report every such
// operation as having answered happily.
func errorResultOf(results []reflect.Value) (failure error, declared bool) {
	for _, result := range results {
		if result.Type() != reflect.TypeOf((*error)(nil)).Elem() {
			continue
		}
		declared = true
		if !result.IsNil() {
			failure = result.Interface().(error)
		}
	}
	return failure, declared
}

// The results beside an error that carry something, so a refusal handing back key material
// alongside its error is visible. A caller that reads the slice rather than the error is
// the reason this is asserted and not assumed.
func resultsBesideTheError(results []reflect.Value) []string {
	answered := []string{}
	for i, result := range results {
		if result.Type() == reflect.TypeOf((*error)(nil)).Elem() {
			continue
		}
		position := "result " + strconv.Itoa(i)
		switch result.Kind() {
		case reflect.Slice, reflect.Pointer, reflect.Interface, reflect.Map, reflect.Func, reflect.Chan:
			if !result.IsNil() {
				answered = append(answered, position+" is a non nil "+result.Type().String())
			}
		default:
			if !result.IsZero() {
				answered = append(answered, position+" is "+fmt.Sprint(result.Interface()))
			}
		}
	}
	return answered
}

// A function that takes an entropy source refuses to run without one, rather than reaching
// for the process's.
//
// A reader parameter is a promise: what this draws is the caller's bytes and nobody else's.
// A function that answers a nil one by substituting crypto/rand keeps the signature and
// breaks the promise silently. The key it hands back is a perfectly good key drawn from a
// source the caller cannot reproduce, every round trip still round trips, and the failing
// interop case the caller was pinning stops reproducing with nothing to say why.
//
// Measured before this gate existed, on this package as it stood: a provider built by
// NewCryptoProviderWithRandom(suite, nil) sealed twice under two different ephemeral keys,
// because HpkeSeal reaches X25519GenerateKey and that substituted the process source for
// the nil one. Random and SignatureKeyPair refused it, and the gate that claimed the
// property named Random alone, one of the four operations that draw.
//
// The class is read off the parse tree rather than written down, so the next function to
// take a source is held the day it is declared.
func TestEveryEntropyTakingFunctionRefusesANilSource(t *testing.T) {
	nothing := func(want reflect.Type) reflect.Value { return reflect.Zero(want) }
	for _, suite := range Suites() {
		params, err := LookupSuite(suite)
		if err != nil {
			t.Fatalf("look up %#04x: %v", uint16(suite), err)
		}
		arguments := entropyTakingArguments(t, params)
		for _, function := range entropyTakingFunctions(t) {
			where := fmt.Sprintf("suite %#04x %s", uint16(suite), function.name)
			value := function.bind(t, params)
			signature := value.Type()
			if signature.NumIn() != len(function.parameters) {
				t.Fatalf("%s is declared with %d parameters and called with %d",
					where, len(function.parameters), signature.NumIn())
			}
			// the source goes in at whatever positions the compiler reads as one, converted
			// to the type the parameter is declared with: an entropy source spelled as a
			// type of its own is still the same bytes and is still called through here
			call := func(given func(want reflect.Type) reflect.Value) []reflect.Value {
				built := []reflect.Value{}
				for i, parameter := range function.parameters {
					if slices.Contains(function.sources, i) {
						built = append(built, given(signature.In(i)))
						continue
					}
					built = append(built, providerStubArgument(t, arguments, function.name, parameter, signature.In(i)))
				}
				return built
			}
			holding := func(want reflect.Type) reflect.Value {
				return reflect.ValueOf(providerStubStream(0x80)).Convert(want)
			}
			// the control first: over a source with bytes in it the call must succeed, or
			// the refusal below is satisfied by a function that refuses everything
			results, recovered := providerStubCall(value, call(holding))
			if recovered != nil {
				t.Errorf("%s refused a source holding bytes: %v", where, recovered)
				continue
			}
			failure, declared := errorResultOf(results)
			if !declared {
				t.Fatalf("%s declares no error result, so it has no way to refuse a source it cannot read", where)
			}
			if failure != nil {
				t.Errorf("%s refused a source holding bytes: %v", where, failure)
				continue
			}
			refused, recovered := providerStubCall(value, call(nothing))
			if recovered != nil {
				continue
			}
			if failure, _ := errorResultOf(refused); failure == nil {
				t.Errorf("%s answered without an entropy source instead of refusing, so it read one nobody handed it", where)
				continue
			}
			for _, answered := range resultsBesideTheError(refused) {
				t.Errorf("%s answered with %s alongside its refusal", where, answered)
			}
		}
	}
}

// A package whose entropy source is spelled every way a line can spell one.
//
// The alias is the whole point of it. A filter comparing the rendered parameter type to
// "io.Reader" reads SeedFrom as taking no entropy source, and the gate reading that filter
// then demands nothing of it while reporting exactly what a complete gate reports. The
// method is the other half: a class that skipped every declaration carrying a receiver
// read the identical fallback as belonging to nothing at all. Neither is a hypothetical --
// both were written into this package, and both passed the whole suite.
const entropyClassControl = `package control

import "io"

type EntropySource = io.Reader

type RandomSource io.Reader

type holder struct{}

func SeedFrom(source EntropySource, length int) ([]byte, error) { return nil, nil }

func (self *holder) SeedFrom(random io.Reader, length int) ([]byte, error) { return nil, nil }

func Wrapped(source RandomSource) ([]byte, error) { return nil, nil }

func NoSource(length int) ([]byte, error) { return nil, nil }
`

// Every name of the control the entropy class must read as taking a source, and no other.
//
// NoSource is the negative half, and it is the half that keeps the row above from being
// satisfied by a matcher that answers yes to everything. Wrapped is in the class on
// purpose: RandomSource is a different type from io.Reader and carries the identical
// interface, so a function taking one reads exactly the bytes a function taking an
// io.Reader reads, and a class that matched only the type itself would report it as taking
// no entropy source at all.
var entropyClassControlMembers = []string{"(*holder).SeedFrom", "SeedFrom", "Wrapped"}

// The entropy class reads a type, not a spelling, and a declaration, not a package level
// declaration.
//
// Both readings are run over the control, because the two are each other's cross check in
// the real gate and a control that proved only one of them would leave the other free to
// stop matching. A matcher that reads the control the way a rendered name filter does --
// SeedFrom absent, the method absent -- fails here rather than passing everything quietly.
func TestTheEntropyClassReadsATypeRatherThanASpelling(t *testing.T) {
	control := typeCheckedText(t, "the entropy class control", entropyClassControl)
	source := entropySourceType(t)
	read := []string{}
	for _, declared := range declaredFunctionsIn(t, control) {
		if len(declared.takes(source)) != 0 {
			read = append(read, declared.name)
		}
	}
	slices.Sort(read)
	if !slices.Equal(read, entropyClassControlMembers) {
		t.Errorf("the class read %v out of the control, want %v", read, entropyClassControlMembers)
	}
	scoped := typeCheckedFunctionsMatching(t, control, signatureTaking(source))
	if !slices.Equal(scoped, entropyClassControlMembers) {
		t.Errorf("the type checker's own scope read %v out of the control, want %v",
			scoped, entropyClassControlMembers)
	}
}

// The type checked walk reads the same source the parse tree walks read.
//
// Two readings of "this package's non test source" that disagree are one gate demanding
// less than the one beside it while both report clean, and the narrower one is invisible
// from inside itself. The root is compared against the guardrails' own list rather than
// assumed to be in it, so a root renamed there does not leave this walking somewhere the
// bans no longer cover.
func TestTheTypeCheckedWalkReadsTheSameSourceAsTheParseTreeWalk(t *testing.T) {
	checked := typeCheckedRoot(t, cryptoOwnRoot)
	if scanned := packageLevelFunctions(t).files; !slices.Equal(checked.paths, scanned) {
		t.Errorf("the type checked walk read %v and the package level scan read %v", checked.paths, scanned)
	}
	if !slices.Contains(forbiddenScanRoots, cryptoOwnRoot) {
		t.Errorf("%q is not one of the roots the guardrails walk, %v", cryptoOwnRoot, forbiddenScanRoots)
	}
}

// A provider holds the source it was built over, and a provider built over none holds the
// operating system's.
//
// This is the frame the constructor pin does not reach.
// TestNewCryptoProviderReadsTheProcessEntropySource holds NewCryptoProvider to a single
// statement, and that statement is a call: what the called constructor then puts in the
// provider's field is a line neither that pin nor any behavioural gate in this file reads.
// Measured, on this package as it stood: three lines inside NewCryptoProviderWithRandom
// answering rand.Reader with a counter expanded through sha256 passed all 2284 tests, and
// every key, nonce and signature seed the implementation produced was recoverable from a
// sixty four bit counter. Two providers over a counter still disagree with each other,
// which is exactly why the same substitution passed all 113 tests in task 11.
//
// So the assertion is identity, and it enumerates nothing: the source a constructor was
// handed must be the value in the field, and a constructor handed none must hold
// crypto/rand's own reader. A wrapper, a buffer, a whitener and a seeded expansion of it
// all fail that, and no list has to be kept in step for it to stay true. Pinning the
// delegate's text instead would have been the same defect one hop lower -- a statement pin
// stops at the next call, and this reads the field however many calls it took to fill.
//
// The two rows are each other's control. A read of the field that always answered
// rand.Reader would fail the row over a caller's source, and one that always answered the
// caller's source would fail the row over none, so neither row can be satisfied by a
// comparison that is not looking at the field.
func TestEveryProviderConstructorHoldsTheSourceItWasGiven(t *testing.T) {
	for _, suite := range Suites() {
		params, err := LookupSuite(suite)
		if err != nil {
			t.Fatalf("look up %#04x: %v", uint16(suite), err)
		}
		arguments := entropyTakingArguments(t, params)
		for _, function := range providerBuildingFunctions(t) {
			where := fmt.Sprintf("suite %#04x %s", uint16(suite), function.name)
			value := function.bind(t, params)
			signature := value.Type()
			if signature.NumIn() != len(function.parameters) {
				t.Fatalf("%s is declared with %d parameters and called with %d",
					where, len(function.parameters), signature.NumIn())
			}
			given := providerStubStream(0x80)
			built := []reflect.Value{}
			for i, parameter := range function.parameters {
				if slices.Contains(function.sources, i) {
					built = append(built, reflect.ValueOf(given).Convert(signature.In(i)))
					continue
				}
				built = append(built, providerStubArgument(t, arguments, function.name, parameter, signature.In(i)))
			}
			results, recovered := providerStubCall(value, built)
			if recovered != nil {
				t.Errorf("%s panicked building a provider: %v", where, recovered)
				continue
			}
			if failure, _ := errorResultOf(results); failure != nil {
				t.Errorf("%s refused to build a provider: %v", where, failure)
				continue
			}
			answered := results[function.answersAt].Interface()
			holding, isTheImplementation := answered.(*suiteCryptoProvider)
			if !isTheImplementation {
				t.Fatalf("%s answered a %T, and this gate reads the entropy source off a %s",
					where, answered, providerReceiver)
			}
			want, what := io.Reader(rand.Reader), "crypto/rand's own reader"
			if len(function.sources) != 0 {
				want, what = given, "the source it was handed"
			}
			if holding.random != want {
				t.Errorf("%s built a provider drawing from a %T rather than from %s",
					where, holding.random, what)
			}
		}
	}
}

// No function taking an entropy source lives where this gate cannot call it.
//
// mls may not import message -- the layering the message package's own doc states, and the
// reason that package exists this early -- so a function there taking an entropy source
// cannot be reached from here at all. What holds it is therefore a tripwire rather than a
// refusal: it fails the day one is declared, rather than passing unobserved.
//
// The residual is written down rather than left to be re-derived, because half of the
// defect is already held there and half is not. TestTheProcessEntropySourceIsReachableFrom
// OneFile walks both roots and lets only the file declaring NewCryptoProvider import
// crypto/rand, so a fallback onto the process source in message fails today; measured, a
// reader taking function written there with a crypto/rand fallback dies on that gate. A
// fallback onto a source that is merely deterministic does not, and in a key encapsulation
// that is the same defect -- measured, the same function with a fixed byte source instead
// passed all 2284 tests. Task 20 lands X-Wing key generation over a seed in exactly that
// package.
//
// Task 20 landed, so the class outside this package is no longer empty and the tripwire is no
// longer the whole of the residual. What answers each member is the table below: the refusal is
// held in the declaring package's own tests, this names which test holds it, and the gate reads
// that package's declarations to check the name resolves. A row that named nothing checkable
// would have moved the problem into a comment, which is what the tripwire was avoiding.
//
// It is not vacuous through a broken walk: rootSourcePaths refuses a root holding no non test
// source, the type check of that root is fatal if it does not complete, and
// packageLevelDeclarations refuses a directory holding no go file.

// Every entropy taking declaration outside this package, with the test in that package's own
// tests that holds its refusal of a nil source.
//
// The CLASS is derived and only the answers are written down. It is read off io.Reader by the
// type checker, over every root the guardrails walk but this one, so a function added there is
// in it whatever its parameter is called and whatever file it is in. This table is held to that
// class in BOTH directions below, and each row's test name is resolved against the declaring
// package, so there are three ways to fail and none of them is a stale label: a member with no
// row, a row with no member, and a row naming a test that package does not declare.
var entropyRefusalsHeldOutsideThisPackage = map[string]string{
	"NewPqSecret":      "TestEveryEntropyTakingFunctionOfThisPackageRefusesANilSource",
	"XwingGenerateKey": "TestEveryEntropyTakingFunctionOfThisPackageRefusesANilSource",
	"XwingEncapsulate": "TestEveryEntropyTakingFunctionOfThisPackageRefusesANilSource",
	// eph_root's draw, landed with the seal lift of 2026-09-13. It is the entropy source
	// MASTER section 8.1 is most emphatic about -- "the most easily broken property here",
	// because a derivation from storage_root would compile, pass every test not looking for
	// it, and make every expired message recoverable forever -- so a draw of it that
	// silently accepted a nil or an exhausted reader is exactly what this row exists to
	// stop. The gate over there failed on the commit that declared it, which is how the row
	// got written.
	"NewEphRoot": "TestEveryEntropyTakingFunctionOfThisPackageRefusesANilSource",
	// The device wrap's two sealing doors, landed with m1 task 14's crypto door. NEITHER
	// DRAWS: both hand the reader they are given straight to XwingEncapsulate, the row three
	// above. They are rows anyway because this gate reads the SIGNATURE and not the body,
	// which is the property -- a door that reached crypto/rand the day its caller's reader ran
	// dry would seal every wrap of every epoch under randomness its caller believed it had
	// supplied, and nothing about the wrap would look wrong. The gate over there failed on the
	// commit that declared these two, which is how both rows got written.
	"SealWrapBody":    "TestEveryEntropyTakingFunctionOfThisPackageRefusesANilSource",
	"SealDeviceWraps": "TestEveryEntropyTakingFunctionOfThisPackageRefusesANilSource",
}

func TestNoEntropyTakingFunctionLivesWhereThisGateCannotCallIt(t *testing.T) {
	source := entropySourceType(t)
	found := map[string]bool{}
	for _, root := range forbiddenScanRoots {
		if root == cryptoOwnRoot {
			continue
		}
		declaredThere := packageLevelDeclarations(t, root)
		for _, declared := range declaredFunctionsOf(t, root) {
			if len(declared.takes(source)) == 0 {
				continue
			}
			found[declared.name] = true
			holder, classified := entropyRefusalsHeldOutsideThisPackage[declared.name]
			if !classified {
				t.Errorf("%s declares %s, which takes an entropy source and is outside the package "+
					"TestEveryEntropyTakingFunctionRefusesANilSource can call into: hold its refusal in "+
					"that package's own tests, and name that test in entropyRefusalsHeldOutsideThisPackage",
					root, declared.name)
				continue
			}
			if _, resolves := declaredThere[holder]; !resolves {
				t.Errorf("entropyRefusalsHeldOutsideThisPackage says %s's refusal is held by %s and %s declares no test under that name, so that row names nothing and the refusal is held by a comment",
					declared.name, holder, root)
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(entropyRefusalsHeldOutsideThisPackage)) {
		if !found[name] {
			t.Errorf("entropyRefusalsHeldOutsideThisPackage carries a row for %s and no root outside this package declares it taking an entropy source, so this table describes a tree that no longer exists",
				name)
		}
	}
	t.Logf("%d entropy taking declarations outside this package, each answered by a test that package declares", len(found))
}

// Nothing on the provider surface reaches another source when the caller's runs dry.
//
// The nil case above is one shape of the substitution and this is the other: a source that
// is present and empty. A consumer that answered a read error by falling back would pass
// every gate in this file, because the key it produced would be a good key, so what is
// asserted is the refusal itself, over every operation the surface declares.
//
// Both directions are compared. The operations that draw must refuse an exhausted source
// and the operations that do not must be untouched by it, so a fallback introduced anywhere
// drops a name out of the refusal set and a refusal introduced for some other reason adds
// one. The measured set is the same list TestProviderHasNoRemainingStubs holds to the
// stream, which is what keeps the two from drifting apart.
func TestNoProviderOperationFallsBackWhenItsSourceRunsDry(t *testing.T) {
	for _, suite := range Suites() {
		params, err := LookupSuite(suite)
		if err != nil {
			t.Fatalf("look up %#04x: %v", uint16(suite), err)
		}
		arguments := providerStubArguments(t, params, mustProviderOver(t, suite, providerStubStream(0x80)))
		probed := []string{}
		refused := []string{}
		for _, operation := range providerOperations(t) {
			where := fmt.Sprintf("suite %#04x %s", uint16(suite), operation.name)
			subject := mustProviderOver(t, suite, bytes.NewReader(nil))
			arguments[providerInterfaceName] = subject
			bound := operation.bind(subject)
			call := []reflect.Value{}
			for i, parameter := range operation.parameters {
				call = append(call, providerStubArgument(t, arguments, operation.name, parameter, bound.Type().In(i)))
			}
			results, recovered := providerStubCall(bound, call)
			// recorded off the call rather than off the enumeration, so an operation this
			// loop never called is reported as unreached
			probed = append(probed, operation.name)
			if recovered != nil {
				refused = append(refused, operation.name)
				continue
			}
			if failure, _ := errorResultOf(results); failure != nil {
				refused = append(refused, operation.name)
				for _, answered := range resultsBesideTheError(results) {
					t.Errorf("%s answered with %s alongside its refusal", where, answered)
				}
			}
		}
		assertCoversEveryProviderOperation(t, "TestNoProviderOperationFallsBackWhenItsSourceRunsDry", probed)
		slices.Sort(refused)
		if !slices.Equal(refused, providerStreamDependentOperations) {
			t.Errorf("suite %#04x refused %v over an exhausted source, and the operations that draw are %v",
				uint16(suite), refused, providerStreamDependentOperations)
		}
	}
}

// The non test source of the two crypto packages, the files themselves rather than the tree
// under them. mls/syntax is a different plan's package with its own constraints, and a gate
// that pinned its imports here would fail on that plan's edits rather than on this one's.
func cryptoSourcePaths(t *testing.T) []string {
	t.Helper()
	paths := []string{}
	for _, root := range forbiddenScanRoots {
		found, err := filepath.Glob(filepath.Join(root, "*.go"))
		if err != nil {
			t.Fatalf("list the source of %s: %v", root, err)
		}
		production := 0
		for _, path := range found {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			paths = append(paths, filepath.ToSlash(path))
			production++
		}
		if production == 0 {
			t.Fatalf("%s holds no non test go file, so this gate read nothing of it", root)
		}
	}
	slices.Sort(paths)
	return paths
}

// Every package the crypto is built from, as a whole rather than as a ban list.
//
// A ban list is the shape this project has been walked past eleven times running: one edit
// outside the list and the gate reports the clean run a complete gate reports. What is
// pinned here is the complete set, so an import added anywhere in either package fails
// until somebody writes it down. os, through which a build could pick its entropy source
// out of the environment, fails exactly as loudly as a third party crypto library the slice
// forbids outright.
//
// fmt is on the list only ever as fmt.Errorf("%w: ...", sentinel, detail): the sentinel
// stays the matchable identity and the wrap carries the octet counts a caller needs to
// see. It is written down rather than argued away because this list is the record of what
// the two packages may reach, and the record is the point. It arrived for connect/message
// and mls now reaches it too, in errors_key_schedule.go and key_schedule.go, under the
// same restriction.
//
// The list is a union over forbiddenScanRoots, which since the 2026-09-06 split is three roots
// and not two. So every production import added anywhere in connect/messagegroup -- the
// package the record layer's key schedule, both ratchets, the sealer and the epoch fan-out
// all land in -- has to be written down here, in mls, on the commit that adds it. That is a
// standing tax on a plan whose files are all in the other package, and it is stated here
// because the failure is a test of THIS package going red over an edit made in that one.
//
// It is the mechanical half of the claim that the deterministic provider is reachable only
// by an explicit caller. Nothing here can consult the environment, and the constraint gate
// below says nothing here can be swapped out by a build tag either, so the only way to a
// provider over a caller's reader is to call the constructor that takes one.
//
// math/bits and sync arrived with the two files that need them and are written down here
// on the same terms as fmt. math/bits is tree math's log2 and its count of trailing ones:
// integer arithmetic over a uint32, no allocation, nothing platform specific, and no way
// to reach anything outside the function. sync is the secret tree's stateLock, which is
// the shape the interface registry gives that type -- a mutex guarding the node secrets a
// concurrent sender would otherwise consume twice.
var cryptoImportPaths = []string{
	`"bytes"`,
	`"crypto/aes"`,
	`"crypto/cipher"`,
	`"crypto/ecdh"`,
	`"crypto/ed25519"`,
	`"crypto/hkdf"`,
	`"crypto/hmac"`,
	// crypto/mlkem and crypto/sha3 are ../messagegroup's X-Wing: ML-KEM-768 with the seed
	// construction draft-connolly-cfrg-xwing-kem section 5.2 needs, the SHAKE-256 that expands
	// that seed, and the SHA3-256 the combiner is. Both are standard library, both are pure go
	// and neither consults the environment, which is the property this list exists to keep true.
	// They arrived with p2 tasks 19 and 20 and are written down here on the same terms as fmt.
	`"crypto/mlkem"`,
	`"crypto/rand"`,
	`"crypto/sha256"`,
	`"crypto/sha3"`,
	`"crypto/subtle"`,
	`"encoding/binary"`,
	`"errors"`,
	`"fmt"`,
	// mls itself, reached from ../messagegroup and in that direction only. It is what keeps the one
	// reviewed ECDH call site in the tree: X-Wing's x25519 half goes through X25519GenerateKey,
	// X25519PrivateKey, X25519PublicKey and X25519DH rather than through crypto/ecdh, so the
	// low order point refusal those wrap cannot be bypassed by writing the exchange again.
	// ../messagegroup's edge onto the server-safe half: the record types, the two aad
	// builders and the write_auth mac the sealer runs over. The direction is one way and it is
	// the ruling -- connect/message never imports connect/messagegroup -- so this row is the
	// client half reaching the shared half and never the reverse.
	`"github.com/urnetwork/connect/message"`,
	`"github.com/urnetwork/connect/mls"`,
	`"github.com/urnetwork/connect/mls/syntax"`,
	`"golang.org/x/crypto/chacha20poly1305"`,
	`"io"`,
	`"math"`,
	`"math/bits"`,
	// validate_commit.go's join between a commit's ProposalOrRef vector and the list resolved
	// from it, which is walked over the FIELDS of a CachedProposal rather than over a list of
	// them. reflect.VisibleFields is what says what those fields are, so a field added to that
	// struct with no comparison beside it refuses every commit carrying a proposal instead of
	// being quietly left out of the join -- and four rounds of that door were each exactly one
	// uncovered field, so the walk is the repair rather than a convenience. What is reached is
	// VisibleFields and TypeFor over one package level type, evaluated once at initialisation:
	// no value is inspected, nothing is constructed from a name, no method is called by name and
	// nothing consults the environment, which is the property this list exists to keep true. It
	// is standard library and pure go, so the nine platform cross build is unaffected.
	// reflect.DeepEqual is now in the class the constant time gate derives over these imports,
	// and this package calls it nowhere.
	`"reflect"`,
	`"slices"`,
	`"strconv"`,
	`"sync"`,
	// leaf_node.go's clock. NewLeafNode has to stamp a Lifetime, which is a wall clock
	// reading and cannot come from anywhere else, and RFC 9420 section 7.2 fixes it as
	// seconds since the unix epoch. time.Now().Unix() is the whole of what is reached: no
	// timer, no location database, nothing that consults the environment, and nothing
	// platform specific -- which is the property this list exists to keep true.
	`"time"`,
}

// The crypto is built from the packages above and no others.
func TestTheCryptoIsBuiltFromExactlyThesePackages(t *testing.T) {
	imported := []string{}
	for _, path := range cryptoSourcePaths(t) {
		imported = append(imported, importPathsOf(mustParseSource(t, path))...)
	}
	slices.Sort(imported)
	imported = slices.Compact(imported)
	if !slices.Equal(imported, cryptoImportPaths) {
		t.Errorf("the crypto imports\n%s\nand this gate holds it to\n%s",
			strings.Join(imported, "\n"), strings.Join(cryptoImportPaths, "\n"))
	}
}

// The two spellings a build constraint has, which is the whole of them: the go command
// reads //go:build and, for files predating go 1.17, // +build, and nothing else steers a
// file into or out of a build.
var buildConstraintPrefixes = []string{"//go:build", "// +build"}

// A file carrying each spelling, so a matcher that stopped matching fails here rather than
// reporting every file clean.
const buildConstraintControl = `//go:build linux
// +build linux

package control
`

// No file of the crypto is selected by a build constraint.
//
// The slice's own constraint says the crypto is cross platform from the first commit with
// no build tags on it, and the reason bites hardest here: a constrained file is a second
// implementation the tests on this machine never see, and an entropy source swapped inside
// one would be invisible to every gate in this package while shipping on another platform.
// The whole file is read rather than the leading comment block alone, because a constraint
// written below the package clause is inert and is exactly what somebody moves back up.
func TestNoCryptoSourceCarriesABuildConstraint(t *testing.T) {
	control := buildConstraintsIn(buildConstraintControl)
	want := []string{"//go:build linux", "// +build linux"}
	if !slices.Equal(control, want) {
		t.Errorf("the constraint matcher read %v out of the control, want %v", control, want)
	}
	for _, path := range cryptoSourcePaths(t) {
		text, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if lines := buildConstraintsIn(string(text)); len(lines) != 0 {
			t.Errorf("%s carries %v; the crypto is built the same way on every platform", path, lines)
		}
	}
}

// The build constraint lines of one file's text. Carriage returns are normalised first for
// the reason codeOf normalises them: a matcher anchored on what a line holds matches
// nothing at all in a file something smudged, and a gate that matches nothing demands
// nothing. Not a checkout any more -- `*.go text eol=lf` closed that route -- but a tool
// that rewrites the working tree still smudges, which is the route codeOf now names.
func buildConstraintsIn(text string) []string {
	found := []string{}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		for _, prefix := range buildConstraintPrefixes {
			if strings.HasPrefix(trimmed, prefix) {
				found = append(found, trimmed)
			}
		}
	}
	return found
}

// The constructor that is allowed to reach the operating system, in one place so a rename
// is one edit rather than a matcher that stops matching.
const defaultConstructorName = "NewCryptoProvider"

// The file of the crypto that declares it, found rather than named. A gate told a filename
// reports the clean bill of a complete gate on a file the declaration has moved out of,
// which is the defect task 12 paid for, and it does it silently.
func defaultConstructorFile(t *testing.T) string {
	t.Helper()
	declaring := []string{}
	for _, path := range cryptoSourcePaths(t) {
		for _, declaration := range mustParseSource(t, path).file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if isFunction && function.Recv == nil && function.Name.Name == defaultConstructorName {
				declaring = append(declaring, path)
			}
		}
	}
	if len(declaring) != 1 {
		t.Fatalf("%d files of the crypto declare %s, want exactly 1: %v",
			len(declaring), defaultConstructorName, declaring)
	}
	return declaring[0]
}

// Exactly one file of the crypto can name the process entropy source, and it is the one
// declaring the constructor whose whole job is to hand it over.
//
// This is the structural half of the refusal gates above, and it is the half that does not
// depend on anybody thinking to call the right function with the right argument. A file
// that cannot name crypto/rand cannot fall back to it, whether the fallback is written on a
// nil reader, on a read error, or behind a package level variable no behavioural gate can
// see. Measured: crypto_x25519.go imported it, for a nil reader fallback that made every
// seal a provider over a caller's stream produced unreproducible.
//
// The allowed file is derived from the declaration rather than written down, so moving the
// constructor moves the permission with it and moving the import does not.
func TestTheProcessEntropySourceIsReachableFromOneFile(t *testing.T) {
	importing := []string{}
	for _, path := range cryptoSourcePaths(t) {
		if slices.Contains(importPathsOf(mustParseSource(t, path)), cryptoRandImportPath) {
			importing = append(importing, path)
		}
	}
	want := []string{defaultConstructorFile(t)}
	if !slices.Equal(importing, want) {
		t.Errorf("%v import %s, and only %v declares %s and may",
			importing, cryptoRandImportPath, want, defaultConstructorName)
	}
}

// A source that counts what was drawn through it. What it exists to see is a draw nobody
// can see in an answer: an operation that reads bytes it does not use leaves a caller's
// script one position further on than the caller believes, and every draw after it comes
// out of the wrong place.
type countingReader struct {
	inner io.Reader
	drawn int
}

// Reads through, counting what came back rather than what was asked for, so a source that
// dribbles is counted by what it actually gave.
func (self *countingReader) Read(p []byte) (int, error) {
	read, err := self.inner.Read(p)
	self.drawn += read
	return read, err
}

// What each operation of the surface draws, in bytes, read out of the registry rather than
// written as a literal so a suite with a different scalar or seed length is asked for the
// right number. An operation absent here draws nothing at all.
//
// These are measurements, and the gate below compares them in both directions: an operation
// that stops drawing fails, an operation that starts drawing fails, and an operation that
// draws a different count fails. That is what separates it from the stream dependence the
// stub gate measures, which reads answers rather than draws — an operation that drew a byte
// and discarded it answers identically over two streams and is invisible there. Measured:
// an AeadSeal with a self.Random(1) in it passed TestProviderHasNoRemainingStubs.
var providerStreamDraws = map[string]func(params *SuiteParams) int{
	// ONE ephemeral scalar PER JOINER, and this gate's row carries one joiner. That number is
	// the property rather than an artefact of the fixture: a builder that drew once and reused
	// the encapsulation across a Welcome's entries draws Nsk however many joiners it was
	// handed, and a builder that drew a seed it then discarded draws more. Both answer a
	// Welcome that decodes, and only a count separates either from a correct one.
	"BuildWelcome":     func(params *SuiteParams) int { return params.Nsk },
	"EncryptWithLabel": func(params *SuiteParams) int { return params.Nsk },
	"HpkeSeal":         func(params *SuiteParams) int { return params.Nsk },
	// the same ephemeral scalar as the call it forwards to, since it IS that call: an
	// adaptation that drew anything of its own would be a second source of ephemeral key
	// material in a path whose whole security rests on the one.
	"SealWithLabel":    func(params *SuiteParams) int { return params.Nsk },
	"SignatureKeyPair": func(params *SuiteParams) int { return params.NsigPriv },
	// the key package constructor: a signature seed, and then one KDF.Nh draw for EACH of
	// the two HPKE key pairs it derives. The two draws are the count that matters here --
	// a constructor that derived the init pair and the encryption pair from ONE seed draws
	// KDF.Nh fewer bytes and answers a key package that encodes, signs, refs and validates,
	// so this is the one place in the package where that substitution is a NUMBER rather
	// than a property somebody has to think to compare.
	"NewKeyPackage": func(params *SuiteParams) int { return params.NsigPriv + 2*params.Nh },
	// the sibling, and the difference from the row above is the WHOLE of what a count can say
	// about it: NsigPriv fewer octets, because the signature key is the caller's and this
	// constructor never calls SignatureKeyPair. The two KDF.Nh draws are still two for the row
	// above's reason -- a body that derived the init pair and the encryption pair from ONE seed
	// draws KDF.Nh fewer and answers a key package that encodes, signs, refs and validates.
	//
	// What this row CANNOT see is a body that draws twice and derives BOTH pairs from the first
	// draw: it draws exactly 2*KDF.Nh and passes here. That half is held by
	// TestNewKeyPackageWithSignerDrawsTheInitAndEncryptionKeysFromSeparateEntropy, over the
	// ANSWERS rather than over the draws.
	"NewKeyPackageWithSigner": func(params *SuiteParams) int { return 2 * params.Nh },
	// section 6.3.1's reuse_guard, which is four octets whatever the suite is: RFC 9420 fixes
	// its width in the SenderData structure rather than deriving it from the AEAD, so this is
	// one of the two entries in this table that is not a registry field.
	"SealPrivateMessage": func(params *SuiteParams) int { return senderDataReuseGuardSize },
	"sealPrivateMessage": func(params *SuiteParams) int { return senderDataReuseGuardSize },
	// the same four octets through MASTER section 8.4.2 v2's two bodies, which are the seal one
	// layer down and the seal under the S3 pin. Neither draws anything of its own: the pin is a
	// comparison of two integers and the generation report is a value the seal already had.
	"sealPrivateMessageAt":    func(params *SuiteParams) int { return senderDataReuseGuardSize },
	"sealPrivateMessageBound": func(params *SuiteParams) int { return senderDataReuseGuardSize },
}

// Every operation draws exactly the bytes it uses and no others.
//
// The count is the part of "the caller's reader is consumed exactly, in order" that reading
// one answer cannot state. TestProviderRandomConsumesItsReaderInOrder holds the order and
// the bytes for Random, and the seal and the key pair are held to the windows they land in;
// what this adds is that nothing else on the surface touches the stream at all, so those
// windows stay where a caller put them.
//
// Random is the one row read from the arguments rather than from the registry: what it draws
// is what it was asked for, and the gate's own argument is where that number lives.
func TestEveryProviderOperationDrawsExactlyWhatItUses(t *testing.T) {
	for _, suite := range Suites() {
		params, err := LookupSuite(suite)
		if err != nil {
			t.Fatalf("look up %#04x: %v", uint16(suite), err)
		}
		arguments := providerStubArguments(t, params, mustProviderOver(t, suite, providerStubStream(0x80)))
		probed := []string{}
		drawing := []string{}
		for _, operation := range providerOperations(t) {
			where := fmt.Sprintf("suite %#04x %s", uint16(suite), operation.name)
			counting := &countingReader{inner: providerStubStream(0x80)}
			subject := mustProviderOver(t, suite, counting)
			arguments[providerInterfaceName] = subject
			bound := operation.bind(subject)
			call := []reflect.Value{}
			for i, parameter := range operation.parameters {
				call = append(call, providerStubArgument(t, arguments, operation.name, parameter, bound.Type().In(i)))
			}
			if _, recovered := providerStubCall(bound, call); recovered != nil {
				t.Errorf("%s refused this gate's arguments: %v", where, recovered)
				continue
			}
			want := 0
			if operation.name == "Random" {
				want = arguments["n"].(int)
			} else if draw, held := providerStreamDraws[operation.name]; held {
				want = draw(params)
			}
			if counting.drawn != want {
				t.Errorf("%s drew %d bytes of the source, want %d", where, counting.drawn, want)
			}
			if counting.drawn != 0 {
				drawing = append(drawing, operation.name)
			}
			// recorded here rather than on entry, for the reason the stub gate records it
			// here: covered means the row ran, not that the name was enumerated
			probed = append(probed, operation.name)
		}
		assertCoversEveryProviderOperation(t, "TestEveryProviderOperationDrawsExactlyWhatItUses", probed)
		slices.Sort(drawing)
		// and what draws is what the stub gate measured as moving with the stream, so an
		// operation that draws without its answer moving fails one of the two
		if !slices.Equal(drawing, providerStreamDependentOperations) {
			t.Errorf("suite %#04x drew from the source in %v, and the operations whose answer moves with it are %v",
				uint16(suite), drawing, providerStreamDependentOperations)
		}
		for name := range providerStreamDraws {
			if !slices.Contains(probed, name) {
				t.Errorf("providerStreamDraws names %s, which is not an operation of this surface", name)
			}
		}
	}
}

// The published seal the provider must reproduce over a source the caller wrote.
//
// The corpus carries one entry per registered suite and each names the ephemeral scalar its
// encapsulation was made with, so a provider handed that scalar as its whole entropy source
// has to answer the published enc and the published sequence zero ciphertext.
const providerHpkeSealComparisons = 2

// The provider seals to the published bytes when the caller supplies the ephemeral key.
//
// TestProviderHpkeSealDrawsFromItsOwnReader already says the seal reads this provider's
// source rather than the process's, and it says so by recomputing the encapsulation with
// this package's own x25519 helper. That is the same code answering the same question
// twice: a package that drew the right bytes and derived them wrongly agrees with itself
// and with nothing else alive. What separates the two is a published pair, and the corpus
// has one — the vector's skEm fed in as the provider's entire stream, the vector's enc and
// ct expected back.
//
// It is the shape this task exists for, stated at the surface a caller actually holds. The
// free function is pinned to the same corpus by TestHpkeSealBaseMatchesThePublishedSingleShot;
// this is the interface method, because a method that stopped delegating is exactly the
// edit a delegation invites and the free function agreeing says nothing about what the
// provider answers.
func TestProviderHpkeSealMatchesThePublishedSealOverItsOwnReader(t *testing.T) {
	compared := 0
	for _, vector := range loadHpkeVectors(t) {
		if len(vector.Encryptions) == 0 || vector.Encryptions[0].sequence != 0 {
			t.Fatalf("%s does not open at sequence 0, so a single shot cannot be compared against it", vector.name)
		}
		first := vector.Encryptions[0]
		ephemeral := decodeVectorField(t, vector.name, "skEm", vector.SkEm)
		crypto := mustProviderOver(t, vector.suite, bytes.NewReader(ephemeral))
		kemOutput, ciphertext, err := crypto.HpkeSeal(
			HpkePublicKey(decodeVectorField(t, vector.name, "pkRm", vector.PkRm)),
			decodePossiblyEmptyVectorField(t, vector.name, "info", vector.Info),
			decodeVectorField(t, vector.name, "aad", first.Aad),
			decodeVectorField(t, vector.name, "pt", first.Pt))
		if err != nil {
			t.Fatalf("%s: HpkeSeal over the published ephemeral key: %v", vector.name, err)
		}
		if want := decodeVectorField(t, vector.name, "enc", vector.Enc); !bytes.Equal(kemOutput, want) {
			t.Errorf("%s: the provider encapsulated %x, want the published %x", vector.name, kemOutput, want)
		}
		if want := decodeVectorField(t, vector.name, "ct", first.Ct); !bytes.Equal(ciphertext, want) {
			t.Errorf("%s: the provider sealed %x, want the published %x", vector.name, ciphertext, want)
		}
		compared++
	}
	if compared != providerHpkeSealComparisons {
		t.Fatalf("compared %d published seals, want %d", compared, providerHpkeSealComparisons)
	}
}
