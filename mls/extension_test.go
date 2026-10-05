// The registry enums, the extensions<V> lookup, and the two structures that decide whether a
// member may join a group.
//
// Capabilities and RequiredCapabilities are a validation surface rather than a data structure,
// and the defect they attract is a comparison that is permissive in the wrong direction. A
// required_capabilities check that accepts a member missing a required extension admits a
// member who cannot process the group's messages; one that rejects a member who has everything
// is a group nobody can join. Neither reads as an error where the mistake is, and neither is
// visible to a round trip property: both directions encode and decode perfectly.
//
// So the predicates below are stated in BOTH directions over a class derived from the types
// themselves -- every registry RequiredCapabilities carries, every code point the package
// declares for it, a member with it and a member without it -- rather than over a table of
// cases somebody picked. On this project a hand written class has understated the real one
// fourteen times.
//
// The codec half is held to another implementation's bytes rather than to its own. A golden
// captured from the encoder under test pins nothing at all: five vectors of uint16 in the
// wrong order round trip perfectly, agree with themselves, and disagree with every peer. The
// goldens here are hand derived from RFC 9420 section 7.2 and section 11.1 and then checked
// against the mlswg vectors, which are bytes this package had no part in producing.
package mls

import (
	"bytes"
	"crypto/ecdh"
	"crypto/mlkem"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	"unicode"

	"github.com/urnetwork/connect/mls/syntax"
)

// ---------------------------------------------------------------------------
// the hand derived goldens
// ---------------------------------------------------------------------------

// repeatUint16 is repeatByte's two octet twin: count copies of one code point, big endian, as
// the bytes a registry vector's body is made of.
func repeatUint16(value uint16, count int) []byte {
	out := make([]byte, 0, 2*count)
	for range count {
		out = append(out, byte(value>>8), byte(value))
	}
	return out
}

// handDerivedUpstreamCapabilitiesGolden is RFC 9420 section 7.2 written out by hand:
//
//	versions<V>       01 code point   -> 02 0001
//	cipher_suites<V>  06 code points  -> 0c 000100020003000400050006
//	extensions<V>     empty           -> 00
//	proposals<V>      empty           -> 00
//	credentials<V>    02 code points  -> 04 00010002
//
// 3 + 13 + 1 + 1 + 5 = 23 octets. Each prefix is the MLS varint counting BYTES and not
// elements, which for a fixed width element is the one arithmetic in this encoding that can be
// wrong while every symmetry property still holds.
//
// It is the capabilities of a leaf in the mlswg ratchet_tree vectors, so
// TestCapabilitiesHandDerivedGoldenMatchesTheUpstreamVectors can check this derivation against
// bytes written by another implementation before anything is asserted against it.
func handDerivedUpstreamCapabilitiesGolden() []byte {
	return joinBytes(
		[]byte{0x02}, []byte{0x00, 0x01},
		[]byte{0x0c}, []byte{0x00, 0x01, 0x00, 0x02, 0x00, 0x03, 0x00, 0x04, 0x00, 0x05, 0x00, 0x06},
		[]byte{0x00},
		[]byte{0x00},
		[]byte{0x04}, []byte{0x00, 0x01, 0x00, 0x02},
	)
}

// upstreamCapabilitiesGoldenValue is the structure those bytes describe.
func upstreamCapabilitiesGoldenValue() *Capabilities {
	return &Capabilities{
		Versions: []ProtocolVersion{ProtocolVersionMls10},
		CipherSuites: []CipherSuite{
			CipherSuite(0x0001), CipherSuite(0x0002), CipherSuite(0x0003),
			CipherSuite(0x0004), CipherSuite(0x0005), CipherSuite(0x0006),
		},
		Extensions:  []ExtensionType{},
		Proposals:   []ProposalType{},
		Credentials: []CredentialType{CredentialTypeBasic, CredentialType(0x0002)},
	}
}

// handDerivedProfileCapabilitiesGolden pins what the published vectors cannot: an empty
// leading vector, this profile's own 0xF00x extension code points, a proposals vector holding
// a code point no registry entry of this package declares, and an extensions vector exactly at
// the width where the MLS varint stops being one octet.
//
//	versions<V>       01 code point   -> 02 0001
//	cipher_suites<V>  empty           -> 00
//	extensions<V>     32 code points  -> 4040 then 64 octets of f002
//	proposals<V>      02 code points  -> 04 00070008
//	credentials<V>    01 code point   -> 02 0001
//
// 3 + 1 + 66 + 5 + 3 = 78 octets. The 64 octet body is the point of the third line: 63 is the
// last length a one octet prefix expresses and 64 is the first that needs two, so an encoder
// writing the prefix as a bare uint8 produces 63 correct vectors and then a wrong one.
func handDerivedProfileCapabilitiesGolden() []byte {
	return joinBytes(
		[]byte{0x02}, []byte{0x00, 0x01},
		[]byte{0x00},
		[]byte{0x40, 0x40}, repeatUint16(0xf002, 32),
		[]byte{0x04}, []byte{0x00, 0x07, 0x00, 0x08},
		[]byte{0x02}, []byte{0x00, 0x01},
	)
}

// profileCapabilitiesGoldenValue is the structure those bytes describe.
func profileCapabilitiesGoldenValue() *Capabilities {
	extensions := make([]ExtensionType, 0, 32)
	for range 32 {
		extensions = append(extensions, ExtensionTypeUrmessageLeafKeys)
	}
	return &Capabilities{
		Versions:     []ProtocolVersion{ProtocolVersionMls10},
		CipherSuites: nil,
		Extensions:   extensions,
		// 0x0008 is not a code point this package declares, and it is here on purpose: a
		// peer's capabilities are attacker chosen bytes and an unregistered value has to
		// survive being read and written back, or the signature over the leaf carrying it
		// stops verifying at this implementation and nowhere else.
		Proposals:   []ProposalType{ProposalTypeGroupContextExtensions, ProposalType(0x0008)},
		Credentials: []CredentialType{CredentialTypeBasic},
	}
}

// handDerivedUpstreamRequiredCapabilitiesGolden is RFC 9420 section 11.1 written out by hand
// for the empty requirement: three vectors, each a single zero length prefix.
//
//	extension_types<V>   empty -> 00
//	proposal_types<V>    empty -> 00
//	credential_types<V>  empty -> 00
//
// Three octets, and the count of them is the assertion: a structure encoded with two vectors
// or four is a required_capabilities extension body every peer parses differently, and this
// exact encoding is what the mlswg group_info vectors carry.
func handDerivedUpstreamRequiredCapabilitiesGolden() []byte {
	return []byte{0x00, 0x00, 0x00}
}

// handDerivedProfileRequiredCapabilitiesGolden is the non empty form, which no published
// vector carries:
//
//	extension_types<V>   02 code points -> 04 f001f002
//	proposal_types<V>    empty          -> 00
//	credential_types<V>  01 code point  -> 02 0001
//
// 5 + 1 + 3 = 9 octets. The empty vector in the MIDDLE is what this adds over the one above:
// an encoder that wrote the three vectors in the wrong order still produces three prefixes,
// and only a case where the three differ can tell the orders apart.
func handDerivedProfileRequiredCapabilitiesGolden() []byte {
	return joinBytes(
		[]byte{0x04}, []byte{0xf0, 0x01, 0xf0, 0x02},
		[]byte{0x00},
		[]byte{0x02}, []byte{0x00, 0x01},
	)
}

// profileRequiredCapabilitiesGoldenValue is the structure those bytes describe.
func profileRequiredCapabilitiesGoldenValue() *RequiredCapabilities {
	return &RequiredCapabilities{
		ExtensionTypes:  []ExtensionType{ExtensionTypeUrmessageGroupPolicy, ExtensionTypeUrmessageLeafKeys},
		ProposalTypes:   nil,
		CredentialTypes: []CredentialType{CredentialTypeBasic},
	}
}

// handDerivedRequiredCapabilitiesExtensionGolden is one Extension entry, section 6.3.1: the
// uint16 registry code point then the opaque body.
//
//	extension_type            -> 0003
//	extension_data<V>, 3 body -> 03 000000
//
// Six octets, and it is byte for byte the required_capabilities entry the mlswg group_info
// vectors carry, so the entry framing is pinned to another implementation rather than to this
// file's arithmetic.
func handDerivedRequiredCapabilitiesExtensionGolden() []byte {
	return joinBytes([]byte{0x00, 0x03}, []byte{0x03}, []byte{0x00, 0x00, 0x00})
}

// handDerivedLongExtensionGolden is an entry whose body crosses the varint width boundary:
// type 0xf002, then a 64 octet body, which needs the two octet prefix 0x4040. 2 + 2 + 64 = 68.
func handDerivedLongExtensionGolden() []byte {
	return joinBytes([]byte{0xf0, 0x02}, []byte{0x40, 0x40}, repeatByte(0x5a, 64))
}

// handDerivedExtensionsVectorGolden is the extensions<V> vector holding the entry above: the
// vector's own byte count, then the entry. 01 + 06 = 07 octets, and it is what the mlswg
// group_info vectors carry in the group context's last field.
func handDerivedExtensionsVectorGolden() []byte {
	return joinBytes([]byte{0x06}, handDerivedRequiredCapabilitiesExtensionGolden())
}

// handDerivedMultiEntryExtensionsVectorGolden is the same vector carrying three entries whose
// bodies differ in the way that matters:
//
//	0003 03 000000        -> 06 octets
//	f001 06 "policy"      -> 09 octets
//	f002 00               -> 03 octets
//
// 6 + 9 + 3 = 18 = 0x12, which is the vector's own prefix, and 19 octets in all. The third
// entry is the one worth having: an absent body and an empty body are one encoding, so an
// implementation that wrote nothing at all for a nil body would produce a two entry vector and
// a length prefix that no longer describes it.
func handDerivedMultiEntryExtensionsVectorGolden() []byte {
	return joinBytes(
		[]byte{0x12},
		[]byte{0x00, 0x03}, []byte{0x03}, []byte{0x00, 0x00, 0x00},
		[]byte{0xf0, 0x01}, []byte{0x06}, []byte("policy"),
		[]byte{0xf0, 0x02}, []byte{0x00},
	)
}

// multiEntryExtensionsGoldenValue is the vector those octets describe.
func multiEntryExtensionsGoldenValue() []Extension {
	return []Extension{
		{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: []byte{0x00, 0x00, 0x00}},
		{ExtensionType: ExtensionTypeUrmessageGroupPolicy, ExtensionData: []byte("policy")},
		{ExtensionType: ExtensionTypeUrmessageLeafKeys, ExtensionData: nil},
	}
}

// ---------------------------------------------------------------------------
// the upstream corpus
// ---------------------------------------------------------------------------

// vectorFilePaths is every mlswg vector file, derived from the directory rather than listed.
// A family added to testdata enters the scans below in the commit that vendors it.
func vectorFilePaths(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "vectors", "*.json"))
	if err != nil {
		t.Fatalf("list the vector files: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no vector file was found, so every upstream property below compares against nothing")
	}
	slices.Sort(paths)
	return paths
}

// hexStringsIn walks a decoded JSON document and yields every string in it that is valid hex,
// at any depth. The mlswg families nest differently from each other -- messages.json is a flat
// list of objects, the passive client families carry arrays of epochs -- and a walk over the
// shape is what lets one scan read all of them without a per family reader.
func hexStringsIn(value any, out *[][]byte) {
	switch typed := value.(type) {
	case string:
		if len(typed) == 0 || len(typed)%2 != 0 {
			return
		}
		decoded, err := hex.DecodeString(typed)
		if err != nil {
			return
		}
		*out = append(*out, decoded)
	case []any:
		for _, item := range typed {
			hexStringsIn(item, out)
		}
	case map[string]any:
		for _, name := range slices.Sorted(maps.Keys(typed)) {
			hexStringsIn(typed[name], out)
		}
	}
}

// upstreamLeafCapabilities is every distinct Capabilities encoding the vendored mlswg vectors
// contain, keyed by its hex.
//
// The extraction is written here rather than taken from this package because this package has
// no LeafNode yet -- task 5 adds it -- and because a golden read back through the code under
// test is not a golden. What it uses from the package is syntax.Reader, which is the layer
// below and has its own published vectors, and the capabilities are lifted out as a BYTE RANGE
// rather than decoded, so what the assertions compare is upstream's octets against this
// encoder's.
//
// Every string in every file is tried, and the ones that are not ratchet trees or leaf nodes
// simply fail to parse. Selecting by field name would be a list of upstream's spellings, which
// is the enumeration rule 5 objects to; strict parsing -- exact region consumption, a
// recognised credential type, a recognised leaf node source -- is what separates a tree from a
// blob without one.
func upstreamLeafCapabilities(t *testing.T) map[string][]byte {
	t.Helper()
	found := map[string][]byte{}
	leaves := 0
	for _, path := range vectorFilePaths(t) {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var document any
		if err := json.Unmarshal(body, &document); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		blobs := [][]byte{}
		hexStringsIn(document, &blobs)
		for _, blob := range blobs {
			for _, capabilities := range leafCapabilitiesOfRatchetTree(blob) {
				found[hex.EncodeToString(capabilities)] = capabilities
				leaves++
			}
			if capabilities, ok := leafCapabilitiesOfLeafNode(blob); ok {
				found[hex.EncodeToString(capabilities)] = capabilities
				leaves++
			}
		}
	}
	if leaves == 0 {
		t.Fatal("no leaf node was read out of the vendored vectors, so every upstream comparison below is against an empty set")
	}
	t.Logf("%d upstream leaf nodes read, %d distinct capabilities encodings", leaves, len(found))
	return found
}

// leafCapabilitiesOfRatchetTree reads blob as a ratchet_tree extension body -- optional<Node>
// nodes<V> -- and returns the capabilities byte range of every leaf in it. A blob that is not
// one yields nothing rather than an error, because every string in every vector file is tried.
func leafCapabilitiesOfRatchetTree(blob []byte) [][]byte {
	outer := syntax.NewReader(blob)
	nodes, err := outer.ReadOpaque()
	if err != nil || outer.Done() != nil {
		return nil
	}
	r := syntax.NewReader(nodes)
	found := [][]byte{}
	for !r.Empty() {
		present, err := r.ReadUint8()
		if err != nil || present > 1 {
			return nil
		}
		if present == 0 {
			continue
		}
		nodeType, err := r.ReadUint8()
		if err != nil {
			return nil
		}
		switch nodeType {
		case 1:
			capabilities, ok := readLeafNodeCapabilities(r, nodes)
			if !ok {
				return nil
			}
			found = append(found, capabilities)
		case 2:
			// ParentNode: encryption_key, parent_hash, unmerged_leaves, all opaque<V>
			// as far as this walk is concerned.
			for range 3 {
				if _, err := r.ReadOpaque(); err != nil {
					return nil
				}
			}
		default:
			return nil
		}
	}
	return found
}

// leafCapabilitiesOfLeafNode reads blob as a bare LeafNode, which is what an Update proposal's
// body is, and returns its capabilities byte range.
func leafCapabilitiesOfLeafNode(blob []byte) ([]byte, bool) {
	r := syntax.NewReader(blob)
	capabilities, ok := readLeafNodeCapabilities(r, blob)
	if !ok || r.Done() != nil {
		return nil, false
	}
	return capabilities, true
}

// readLeafNodeCapabilities consumes one RFC 9420 section 7.2 LeafNode from r and returns the
// byte range its capabilities field occupies in raw, which must be the slice r was opened
// over. Only a BasicCredential is handled, which is every credential the vendored vectors
// carry and the only one this profile admits.
func readLeafNodeCapabilities(r *syntax.Reader, raw []byte) ([]byte, bool) {
	// encryption_key and signature_key
	for range 2 {
		if _, err := r.ReadOpaque(); err != nil {
			return nil, false
		}
	}
	credentialType, err := r.ReadUint16()
	if err != nil || credentialType != uint16(CredentialTypeBasic) {
		return nil, false
	}
	if _, err := r.ReadOpaque(); err != nil {
		return nil, false
	}
	start := r.Offset()
	// the five registry vectors, taken as opaque regions so this reader never depends on the
	// decoder it is here to check
	for range 5 {
		if _, err := r.ReadOpaque(); err != nil {
			return nil, false
		}
	}
	end := r.Offset()
	source, err := r.ReadUint8()
	if err != nil {
		return nil, false
	}
	switch source {
	case 1: // key_package: a Lifetime, which is two uint64
		for range 2 {
			if _, err := r.ReadUint64(); err != nil {
				return nil, false
			}
		}
	case 2: // update: nothing further
	case 3: // commit: parent_hash
		if _, err := r.ReadOpaque(); err != nil {
			return nil, false
		}
	default:
		return nil, false
	}
	// extensions<V> and the signature
	for range 2 {
		if _, err := r.ReadOpaque(); err != nil {
			return nil, false
		}
	}
	if start >= end || end > len(raw) {
		return nil, false
	}
	return raw[start:end], true
}

// upstreamRequiredCapabilitiesBodies is every required_capabilities extension body the mlswg
// group_info vectors carry, keyed by hex.
//
// This one goes through GroupContext and FindExtension, which have landed and have their own
// upstream goldens, rather than through a second hand written navigator. The four octets
// skipped are the MLSMessage version and wire_format that wrap a GroupInfo, and
// GroupContext.UnmarshalMLS consuming exactly its own fields is what lets the rest of the
// GroupInfo be ignored.
func upstreamRequiredCapabilitiesBodies(t *testing.T) map[string][]byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "vectors", "messages.json"))
	if err != nil {
		t.Fatalf("read messages.json: %v", err)
	}
	entries := []map[string]string{}
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatalf("parse messages.json: %v", err)
	}
	found := map[string][]byte{}
	contexts := 0
	for _, entry := range entries {
		encoded, err := hex.DecodeString(entry["mls_group_info"])
		if err != nil || len(encoded) < 4 {
			continue
		}
		context := &GroupContext{}
		if err := context.UnmarshalMLS(syntax.NewReader(encoded[4:])); err != nil {
			continue
		}
		contexts++
		data, ok, err := FindExtension(context.Extensions, ExtensionTypeRequiredCapabilities)
		if err != nil {
			t.Fatalf("an upstream group context carries required_capabilities more than once: %v", err)
		}
		if ok {
			found[hex.EncodeToString(data)] = data
		}
	}
	if contexts == 0 {
		t.Fatal("no group context was decoded out of messages.json, so this scan read nothing")
	}
	if len(found) == 0 {
		t.Fatalf("%d upstream group contexts were decoded and none carried a required_capabilities extension, so this comparison is against an empty set",
			contexts)
	}
	t.Logf("%d upstream group contexts, %d distinct required_capabilities bodies", contexts, len(found))
	return found
}

// ---------------------------------------------------------------------------
// the goldens, checked against upstream and then against the encoder
// ---------------------------------------------------------------------------

// TestCapabilitiesHandDerivedGoldenMatchesTheUpstreamVectors checks the hand derivation
// against another implementation's bytes before anything is asserted against it. Both are
// statements of the section 7.2 encoding written without reference to this package -- one here
// from the RFC, one by the implementations that produced the mlswg vectors -- and agreeing is
// what makes either worth quoting.
func TestCapabilitiesHandDerivedGoldenMatchesTheUpstreamVectors(t *testing.T) {
	derived := handDerivedUpstreamCapabilitiesGolden()
	if len(derived) != 23 {
		t.Fatalf("the hand derivation is %d octets, the arithmetic in its comment says 23", len(derived))
	}
	upstream := upstreamLeafCapabilities(t)
	if _, held := upstream[hex.EncodeToString(derived)]; !held {
		t.Fatalf("the hand derived capabilities golden\n %x\nis not among the %d the vendored vectors carry:\n %v",
			derived, len(upstream), slices.Sorted(maps.Keys(upstream)))
	}
}

// TestCapabilitiesMarshalMatchesTheHandDerivedGoldens is the field order and prefix width pin.
// Two of the five vectors swapped, or a byte count written as an element count, changes every
// leaf node signature this implementation produces and verifies, and this is the cheapest
// place to see it.
func TestCapabilitiesMarshalMatchesTheHandDerivedGoldens(t *testing.T) {
	for name, testCase := range map[string]struct {
		value *Capabilities
		want  []byte
		size  int
	}{
		"upstream": {upstreamCapabilitiesGoldenValue(), handDerivedUpstreamCapabilitiesGolden(), 23},
		"profile":  {profileCapabilitiesGoldenValue(), handDerivedProfileCapabilitiesGolden(), 78},
	} {
		if len(testCase.want) != testCase.size {
			t.Fatalf("%s: the hand derivation is %d octets, the arithmetic in its comment says %d",
				name, len(testCase.want), testCase.size)
		}
		encoded, err := syntax.Marshal(testCase.value)
		if err != nil {
			t.Fatalf("%s: syntax.Marshal: %v", name, err)
		}
		if !bytes.Equal(encoded, testCase.want) {
			t.Errorf("%s: syntax.Marshal =\n %x\nwant\n %x", name, encoded, testCase.want)
		}
		decoded := &Capabilities{}
		if err := syntax.Unmarshal(testCase.want, decoded); err != nil {
			t.Fatalf("%s: syntax.Unmarshal: %v", name, err)
		}
		if !sameRegistryVectors(decoded, testCase.value) {
			t.Errorf("%s: the golden decoded to %s, want %s",
				name, describeRegistryVectors(decoded), describeRegistryVectors(testCase.value))
		}
	}
}

// TestCapabilitiesRoundTripsEveryUpstreamEncodingByteExact is the widest statement of the
// codec that costs nothing: every capabilities encoding another implementation wrote must
// decode here and re-encode to the same octets.
//
// It reaches what a golden cannot. The goldens are two structures; this is every one the
// vendored families contain, including a proposals vector naming a code point no registry
// entry of this package declares, which is the GREASE case a decoder that validated registry
// membership would refuse and nothing else here would notice.
func TestCapabilitiesRoundTripsEveryUpstreamEncodingByteExact(t *testing.T) {
	upstream := upstreamLeafCapabilities(t)
	if len(upstream) < 2 {
		t.Fatalf("the vendored vectors yielded %d distinct capabilities encodings; a single one cannot show that the field order is read the same way twice",
			len(upstream))
	}
	unregistered := 0
	declaredProposals := sortedValues(registryConstantsOfType(t, "ProposalType"))
	for _, name := range slices.Sorted(maps.Keys(upstream)) {
		encoded := upstream[name]
		decoded := &Capabilities{}
		if err := syntax.Unmarshal(encoded, decoded); err != nil {
			t.Errorf("%s: syntax.Unmarshal: %v", name, err)
			continue
		}
		reencoded, err := syntax.Marshal(decoded)
		if err != nil {
			t.Errorf("%s: syntax.Marshal: %v", name, err)
			continue
		}
		if !bytes.Equal(reencoded, encoded) {
			t.Errorf("%s: re-encode =\n %x\nwant\n %x", name, reencoded, encoded)
		}
		for _, proposal := range decoded.Proposals {
			if !slices.Contains(declaredProposals, uint64(proposal)) {
				unregistered++
			}
		}
	}
	if unregistered == 0 {
		t.Error("no upstream capabilities carried a proposal type this package does not declare, so the GREASE case this sweep is worth most for was never reached")
	}
}

// TestRequiredCapabilitiesMarshalMatchesTheHandDerivedGoldens pins the section 11.1 field
// order, and the empty case against the mlswg group_info vectors.
func TestRequiredCapabilitiesMarshalMatchesTheHandDerivedGoldens(t *testing.T) {
	empty := handDerivedUpstreamRequiredCapabilitiesGolden()
	upstream := upstreamRequiredCapabilitiesBodies(t)
	if _, held := upstream[hex.EncodeToString(empty)]; !held {
		t.Fatalf("the hand derived empty required_capabilities golden %x is not among the %v the group_info vectors carry",
			empty, slices.Sorted(maps.Keys(upstream)))
	}
	for name, testCase := range map[string]struct {
		value *RequiredCapabilities
		want  []byte
		size  int
	}{
		"empty":   {&RequiredCapabilities{}, empty, 3},
		"profile": {profileRequiredCapabilitiesGoldenValue(), handDerivedProfileRequiredCapabilitiesGolden(), 9},
	} {
		if len(testCase.want) != testCase.size {
			t.Fatalf("%s: the hand derivation is %d octets, the arithmetic in its comment says %d",
				name, len(testCase.want), testCase.size)
		}
		encoded, err := syntax.Marshal(testCase.value)
		if err != nil {
			t.Fatalf("%s: syntax.Marshal: %v", name, err)
		}
		if !bytes.Equal(encoded, testCase.want) {
			t.Errorf("%s: syntax.Marshal =\n %x\nwant\n %x", name, encoded, testCase.want)
		}
		decoded := &RequiredCapabilities{}
		if err := syntax.Unmarshal(testCase.want, decoded); err != nil {
			t.Fatalf("%s: syntax.Unmarshal: %v", name, err)
		}
		if !sameRegistryVectors(decoded, testCase.value) {
			t.Errorf("%s: the golden decoded to %s, want %s",
				name, describeRegistryVectors(decoded), describeRegistryVectors(testCase.value))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(upstream)) {
		decoded := &RequiredCapabilities{}
		if err := syntax.Unmarshal(upstream[name], decoded); err != nil {
			t.Errorf("%s: syntax.Unmarshal an upstream body: %v", name, err)
			continue
		}
		reencoded, err := syntax.Marshal(decoded)
		if err != nil {
			t.Errorf("%s: syntax.Marshal: %v", name, err)
			continue
		}
		if !bytes.Equal(reencoded, upstream[name]) {
			t.Errorf("%s: re-encode = %x, want %x", name, reencoded, upstream[name])
		}
	}
}

// TestExtensionMarshalMatchesTheHandDerivedGoldens pins the section 6.3.1 entry framing and
// the extensions<V> vector's own prefix. The six octet entry is the one the mlswg group_info
// vectors carry, so the entry layout is held to another implementation, and the 68 octet one
// crosses the varint width boundary the published vectors do not reach.
func TestExtensionMarshalMatchesTheHandDerivedGoldens(t *testing.T) {
	for name, testCase := range map[string]struct {
		value *Extension
		want  []byte
		size  int
	}{
		"required capabilities": {
			&Extension{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: []byte{0x00, 0x00, 0x00}},
			handDerivedRequiredCapabilitiesExtensionGolden(), 6,
		},
		"long body": {
			&Extension{ExtensionType: ExtensionTypeUrmessageLeafKeys, ExtensionData: repeatByte(0x5a, 64)},
			handDerivedLongExtensionGolden(), 68,
		},
	} {
		if len(testCase.want) != testCase.size {
			t.Fatalf("%s: the hand derivation is %d octets, the arithmetic in its comment says %d",
				name, len(testCase.want), testCase.size)
		}
		encoded, err := syntax.Marshal(testCase.value)
		if err != nil {
			t.Fatalf("%s: syntax.Marshal: %v", name, err)
		}
		if !bytes.Equal(encoded, testCase.want) {
			t.Errorf("%s: syntax.Marshal =\n %x\nwant\n %x", name, encoded, testCase.want)
		}
	}

	// and the vector's own byte count, which is written by syntax.WriteVector rather than at
	// each of the five call sites that carry an extensions field
	w := syntax.NewWriter()
	if err := WriteExtensions(w, []Extension{
		{ExtensionType: ExtensionTypeRequiredCapabilities, ExtensionData: []byte{0x00, 0x00, 0x00}},
	}); err != nil {
		t.Fatalf("WriteExtensions: %v", err)
	}
	encoded, err := w.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if want := handDerivedExtensionsVectorGolden(); !bytes.Equal(encoded, want) {
		t.Fatalf("WriteExtensions = %x, want %x", encoded, want)
	}
	out, err := ReadExtensions(syntax.NewReader(encoded))
	if err != nil {
		t.Fatalf("ReadExtensions: %v", err)
	}
	if len(out) != 1 || out[0].ExtensionType != ExtensionTypeRequiredCapabilities ||
		!bytes.Equal(out[0].ExtensionData, []byte{0x00, 0x00, 0x00}) {
		t.Fatalf("ReadExtensions = %+v", out)
	}

	// three entries, an absent body among them, and the vector's byte count over all of them
	many := syntax.NewWriter()
	if err := WriteExtensions(many, multiEntryExtensionsGoldenValue()); err != nil {
		t.Fatalf("WriteExtensions: %v", err)
	}
	manyEncoded, err := many.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	want := handDerivedMultiEntryExtensionsVectorGolden()
	if len(want) != 19 {
		t.Fatalf("the hand derivation is %d octets, the arithmetic in its comment says 19", len(want))
	}
	if !bytes.Equal(manyEncoded, want) {
		t.Fatalf("WriteExtensions =\n %x\nwant\n %x", manyEncoded, want)
	}
	back, err := ReadExtensions(syntax.NewReader(want))
	if err != nil {
		t.Fatalf("ReadExtensions: %v", err)
	}
	if len(back) != 3 {
		t.Fatalf("ReadExtensions returned %d entries, want 3: %+v", len(back), back)
	}
	for i, entry := range multiEntryExtensionsGoldenValue() {
		if back[i].ExtensionType != entry.ExtensionType || !bytes.Equal(back[i].ExtensionData, entry.ExtensionData) {
			t.Errorf("entry %d = {%#04x %x}, want {%#04x %x}",
				i, uint16(back[i].ExtensionType), back[i].ExtensionData,
				uint16(entry.ExtensionType), entry.ExtensionData)
		}
	}
}

// TestExtensionCarriesAnUnregisteredTypeUnchanged is the codec-decides-no-policy rule stated
// where it can fail: every code point outside the six this package declares must decode, be
// carried, and re-encode to the same octets.
//
// The class is derived as the complement of the declared one over a sample of the whole uint16
// space rather than as a couple of values somebody picked, because "unknown" is 65530 code
// points and a decoder that refused a RANGE of them -- say everything above 0xF003, which is
// the shape a private-use check takes -- passes a test naming two.
func TestExtensionCarriesAnUnregisteredTypeUnchanged(t *testing.T) {
	declared := sortedValues(registryConstantsOfType(t, "ExtensionType"))
	checked := 0
	for candidate := 0; candidate <= 0xffff; candidate += 7 {
		if slices.Contains(declared, uint64(candidate)) {
			continue
		}
		in := &Extension{ExtensionType: ExtensionType(candidate), ExtensionData: []byte{0xa5}}
		encoded, err := syntax.Marshal(in)
		if err != nil {
			t.Fatalf("%#04x: syntax.Marshal: %v", candidate, err)
		}
		out := &Extension{}
		if err := syntax.Unmarshal(encoded, out); err != nil {
			t.Fatalf("%#04x: syntax.Unmarshal: %v", candidate, err)
		}
		if out.ExtensionType != in.ExtensionType || !bytes.Equal(out.ExtensionData, in.ExtensionData) {
			t.Fatalf("%#04x: round trip = %+v, want %+v", candidate, out, in)
		}
		checked++
	}
	if checked < 9000 {
		t.Fatalf("only %d unregistered extension types were exercised, so the sweep read something other than the uint16 space", checked)
	}
}

// TestFindExtensionAnswersBothDirections states the lookup over the class it is asked about:
// for every extension type this package declares, a vector that holds it and a vector that
// does not.
//
// Both directions, and derived, because the two failures are symmetric and neither is visible
// from the other. A lookup that always reports found hands a caller an empty body for a
// required_capabilities that is not there; one that always reports absent makes every
// extension the group set unreachable. The bodies differ per entry so a lookup returning the
// wrong entry's body is a failure too.
func TestFindExtensionAnswersBothDirections(t *testing.T) {
	declared := registryConstantsOfType(t, "ExtensionType")
	types := []ExtensionType{}
	for _, value := range sortedValues(declared) {
		types = append(types, ExtensionType(value))
	}
	// a code point the package declares none of, so "absent" is also stated for a type the
	// vector could never hold
	types = append(types, ExtensionType(0xbeef))
	if len(types) < 3 {
		t.Fatalf("the derivation found %d extension types, so this sweep states almost nothing", len(types))
	}

	bodyFor := func(t ExtensionType) []byte { return []byte{byte(t >> 8), byte(t), 0x5a} }
	present := []Extension{}
	for _, extensionType := range types {
		present = append(present, Extension{ExtensionType: extensionType, ExtensionData: bodyFor(extensionType)})
	}
	for _, extensionType := range types {
		body, ok, err := FindExtension(present, extensionType)
		if err != nil {
			t.Errorf("%#04x: FindExtension over a vector holding it exactly once refused it: %v",
				uint16(extensionType), err)
			continue
		}
		if !ok {
			t.Errorf("%#04x: FindExtension over a vector holding it reported absent", uint16(extensionType))
			continue
		}
		if !bytes.Equal(body, bodyFor(extensionType)) {
			t.Errorf("%#04x: FindExtension returned %x, want %x", uint16(extensionType), body, bodyFor(extensionType))
		}
		absent := slices.DeleteFunc(slices.Clone(present), func(e Extension) bool {
			return e.ExtensionType == extensionType
		})
		if body, ok, err := FindExtension(absent, extensionType); ok || err != nil {
			t.Errorf("%#04x: FindExtension over a vector without it returned (%x, %v, %v), want absent and no refusal",
				uint16(extensionType), body, ok, err)
		}
	}
	if _, ok, err := FindExtension(nil, ExtensionTypeRatchetTree); ok || err != nil {
		t.Errorf("FindExtension over a nil vector answered (%v, %v), want absent and no refusal", ok, err)
	}
	if _, ok, err := FindExtension([]Extension{}, ExtensionTypeRatchetTree); ok || err != nil {
		t.Errorf("FindExtension over an empty vector answered (%v, %v), want absent and no refusal", ok, err)
	}
	// present with an empty body is not absent, which is the whole reason the bool is
	// separate from the slice, and neither of them is the refusal
	body, ok, err := FindExtension([]Extension{{ExtensionType: ExtensionTypeRatchetTree}}, ExtensionTypeRatchetTree)
	if !ok || len(body) != 0 || err != nil {
		t.Errorf("an entry with an empty body reported (%x, %v, %v), want (empty, true, nil)", body, ok, err)
	}
}

// TestFindExtensionRefusesAVectorCarryingARepeatedTypeRatherThanAnsweringByPosition is the
// selection rule the lookup's comment argues for, stated where it can fail. It is the exact
// reverse of the test that stood here, and that reversal is the point of the commit.
//
// The test that stood here pinned first-wins, and its failure text argued
// that a lookup refusing a repeated type "hides the input ValSem209 is stated over". It was
// committed beside two accessors that refuse a repeat BECAUSE ValSem209 is implemented nowhere,
// so one branch held both positions at once: a repeat is a refusal because the rule does not
// exist, and a repeat is answered by position because the rule will refuse it. The package now
// holds one position -- the lookup refuses -- and hiding an input from a rule that does not
// exist hides it from nothing. When ValSem209 lands it refuses this input earlier; it does not
// make answering by position safe here, because these lookups are reached from paths that
// whole-context validation does not sit in front of.
//
// Derived over every extension type the package declares plus the code points it declares none
// of, in three arrangements, so a lookup that always answered exts[0], or always exts[len-1], or
// only noticed adjacent entries, fails here too. Both the entry lookup and the body wrapper are
// asked, because the wrapper is where a second walk would be written.
func TestFindExtensionRefusesAVectorCarryingARepeatedTypeRatherThanAnsweringByPosition(t *testing.T) {
	types := []ExtensionType{}
	for _, value := range sortedValues(registryConstantsOfType(t, "ExtensionType")) {
		types = append(types, ExtensionType(value))
	}
	for _, probe := range unregisteredProbes {
		types = append(types, ExtensionType(probe))
	}
	if len(types) < 3 {
		t.Fatalf("the derivation found %d extension types, so this sweep states almost nothing", len(types))
	}
	for _, extensionType := range types {
		// some other type, so a repeated pair can be put where neither end of the vector is the
		// answer a broken lookup would reach for
		other := types[0]
		if other == extensionType {
			other = types[1]
		}
		first := []byte{byte(extensionType >> 8), byte(extensionType), 0x01}
		second := []byte{byte(extensionType >> 8), byte(extensionType), 0x02}
		for _, one := range []struct {
			name string
			exts []Extension
			at   []int
		}{
			{"bracketed by other entries", []Extension{
				{ExtensionType: other, ExtensionData: []byte{0xff}},
				{ExtensionType: extensionType, ExtensionData: first},
				{ExtensionType: extensionType, ExtensionData: second},
				{ExtensionType: other, ExtensionData: []byte{0xfe}},
			}, []int{1, 2}},
			{"adjacent at the head", []Extension{
				{ExtensionType: extensionType, ExtensionData: second},
				{ExtensionType: extensionType, ExtensionData: first},
				{ExtensionType: other, ExtensionData: []byte{0xfe}},
			}, []int{0, 1}},
			{"apart, with another entry between them", []Extension{
				{ExtensionType: extensionType, ExtensionData: first},
				{ExtensionType: other, ExtensionData: []byte{0xff}},
				{ExtensionType: extensionType, ExtensionData: second},
			}, []int{0, 2}},
		} {
			body, ok, err := FindExtension(one.exts, extensionType)
			if !errors.Is(err, ErrMalformedExtension) {
				t.Errorf("%#04x, %s: FindExtension over a vector holding it twice answered (%x, %v, %v), want ErrMalformedExtension; which body a peer gets to have enforced is otherwise chosen by the peer",
					uint16(extensionType), one.name, body, ok, err)
				continue
			}
			if ok || body != nil {
				t.Errorf("%#04x, %s: FindExtension answered (%x, %v) beside its refusal; a caller reading the body would be acting on one of two entries picked by iteration order",
					uint16(extensionType), one.name, body, ok)
			}
			// with the repeat refused, the ORDER the two positions are named in is the only
			// observable a walk in the wrong direction still changes, which is what the reader
			// the two accessors share is for
			if at := groupPolicyPositionsNamedBy(t, err); !slices.Equal(at, one.at) {
				t.Errorf("%#04x, %s: the refusal named entries %v, want %v; it has to point at the two entries in the order the vector holds them",
					uint16(extensionType), one.name, at, one.at)
			}
			entry, found, err := FindExtensionEntry(one.exts, extensionType)
			if !errors.Is(err, ErrMalformedExtension) || found ||
				entry.ExtensionData != nil || entry.ExtensionType != 0 {
				t.Errorf("%#04x, %s: FindExtensionEntry answered (%v, %v, %v), want a refusal and no entry",
					uint16(extensionType), one.name, entry, found, err)
			}
		}
		// the control: one entry of the type is still answered, and answered with ITS body, so
		// the refusals above are about the repeat rather than about this sweep's bodies
		once := []Extension{
			{ExtensionType: other, ExtensionData: []byte{0xff}},
			{ExtensionType: extensionType, ExtensionData: first},
		}
		if body, ok, err := FindExtension(once, extensionType); err != nil || !ok || !bytes.Equal(body, first) {
			t.Errorf("%#04x: FindExtension over a vector holding it once answered (%x, %v, %v), want its body and no refusal",
				uint16(extensionType), body, ok, err)
		}
	}
}

// TestReadExtensionsReturnsAnEmptySliceRatherThanNil is the one property ReadExtensions' own
// comment states that nothing else in this file can observe.
//
// The wire has a single spelling for an empty extensions vector, so no encoding round trip can
// distinguish nil from empty: sameRegistryVectors, which every generated round trip property
// here goes through, treats the two as equal by deliberate design. The distinction is a Go one
// and it is the one the comment argues is load bearing -- an absent extensions field and a
// present but empty one are different statements about a group, and a caller that has to tell
// them apart has only the nilness of this slice to do it with.
//
// Stated over an arity sweep rather than over the empty case alone, so "never nil" is a property
// of the function rather than an assertion about one input.
func TestReadExtensionsReturnsAnEmptySliceRatherThanNil(t *testing.T) {
	for count := range 4 {
		exts := []Extension{}
		for i := range count {
			exts = append(exts, Extension{
				ExtensionType: ExtensionType(0xF001 + i),
				ExtensionData: repeatByte(byte(i), i),
			})
		}
		w := syntax.NewWriter()
		if err := WriteExtensions(w, exts); err != nil {
			t.Fatalf("%d entries: WriteExtensions: %v", count, err)
		}
		encoded, err := w.Bytes()
		if err != nil {
			t.Fatalf("%d entries: Bytes: %v", count, err)
		}
		out, err := ReadExtensions(syntax.NewReader(encoded))
		if err != nil {
			t.Fatalf("%d entries: ReadExtensions: %v", count, err)
		}
		if out == nil {
			t.Errorf("%d entries: ReadExtensions returned a nil slice; an empty extensions vector and an absent one are then the same nil in Go and different bytes on the wire",
				count)
		}
		if len(out) != count {
			t.Errorf("%d entries: ReadExtensions returned %d entries", count, len(out))
		}
	}

	// and the empty vector spelled from the encoder's own side: a nil the caller passes in is
	// one octet on the wire, and what comes back from those bytes must still be non nil
	w := syntax.NewWriter()
	if err := WriteExtensions(w, nil); err != nil {
		t.Fatalf("WriteExtensions(nil): %v", err)
	}
	encoded, err := w.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if want := []byte{0x00}; !bytes.Equal(encoded, want) {
		t.Fatalf("WriteExtensions(nil) = %x, want %x", encoded, want)
	}
	out, err := ReadExtensions(syntax.NewReader(encoded))
	if err != nil {
		t.Fatalf("ReadExtensions: %v", err)
	}
	if out == nil {
		t.Error("ReadExtensions over the empty vector returned nil, want an empty non nil slice")
	}
	if len(out) != 0 {
		t.Errorf("ReadExtensions over the empty vector returned %d entries", len(out))
	}
}

// ---------------------------------------------------------------------------
// the refusal properties
// ---------------------------------------------------------------------------

// goldenExtensionEncodings, goldenCapabilitiesEncodings and goldenRequiredCapabilitiesEncodings
// are the valid encodings the refusal properties are stated over. Each set holds a hand
// derivation that another implementation also produced and one that reaches what the published
// vectors do not, so both the ordinary and the boundary shapes are swept.
func goldenExtensionEncodings() map[string][]byte {
	return map[string][]byte{
		"required capabilities entry": handDerivedRequiredCapabilitiesExtensionGolden(),
		"long body entry":             handDerivedLongExtensionGolden(),
	}
}

func goldenCapabilitiesEncodings() map[string][]byte {
	return map[string][]byte{
		"upstream": handDerivedUpstreamCapabilitiesGolden(),
		"profile":  handDerivedProfileCapabilitiesGolden(),
	}
}

func goldenRequiredCapabilitiesEncodings() map[string][]byte {
	return map[string][]byte{
		"empty":   handDerivedUpstreamRequiredCapabilitiesGolden(),
		"profile": handDerivedProfileRequiredCapabilitiesGolden(),
	}
}

// checkCodecRefusals states the three refusal properties every wire structure of this package
// owes, over one type and one set of valid encodings: a trailing octet is refused with
// syntax.ErrTrailingBytes, every proper prefix is refused rather than yielding a partly
// populated struct, and every single octet change is either refused or re-encodes to exactly
// the corrupted bytes.
//
// The third is the canonicality property and the one worth the most: accepted and silently
// changed is a second encoding of a structure whose first encoding somebody signed. Both of
// its outcomes are counted and both are required to occur across the set, because a decoder
// that refused everything satisfies it vacuously and so does a corpus that never reaches the
// accepting branch.
//
// Written once over the type parameter rather than three times, so the three structures cannot
// come to be held to three different versions of the same rule.
func checkCodecRefusals[T any, PT interface {
	*T
	syntax.Codec
}](t *testing.T, goldens map[string][]byte) {
	t.Helper()
	acceptedCorruptions, refusedCorruptions := 0, 0
	for _, name := range slices.Sorted(maps.Keys(goldens)) {
		full := goldens[name]
		if err := syntax.Unmarshal(full, PT(new(T))); err != nil {
			t.Fatalf("%s: the golden itself was refused (%v), so every property below proves nothing", name, err)
		}
		if err := syntax.Unmarshal(append(slices.Clone(full), 0x00), PT(new(T))); !errors.Is(err, syntax.ErrTrailingBytes) {
			t.Errorf("%s: a trailing octet gave err = %v, want ErrTrailingBytes", name, err)
		}
		for n := range len(full) {
			if err := syntax.Unmarshal(full[:n], PT(new(T))); err == nil {
				t.Errorf("%s: the %d octet prefix of %d was accepted, want an error", name, n, len(full))
			}
		}
		familyRefused := 0
		for position := range full {
			for delta := 1; delta < 256; delta++ {
				corrupted := slices.Clone(full)
				corrupted[position] = byte((int(full[position]) + delta) % 256)
				parsed := PT(new(T))
				if err := syntax.Unmarshal(corrupted, parsed); err != nil {
					refusedCorruptions++
					familyRefused++
					continue
				}
				reencoded, err := syntax.Marshal(parsed)
				if err != nil {
					t.Errorf("%s: an accepted corruption at %d failed to re-encode: %v", name, position, err)
					continue
				}
				if !bytes.Equal(reencoded, corrupted) {
					t.Fatalf("%s: the corruption at octet %d was accepted and re-encoded as\n %x\nrather than\n %x",
						name, position, reencoded, corrupted)
				}
				acceptedCorruptions++
			}
		}
		if familyRefused == 0 {
			t.Errorf("%s: no single octet change to it was refused, which cannot be true of a length prefixed encoding",
				name)
		}
	}
	if acceptedCorruptions == 0 {
		t.Error("every corruption was refused, so the canonicality half of this sweep never ran")
	}
	if refusedCorruptions == 0 {
		t.Error("no corruption was refused, so the decoder accepts every octet string this sweep produced")
	}
	t.Logf("%d corruptions accepted and re-encoded exactly, %d refused", acceptedCorruptions, refusedCorruptions)
}

// checkTruncatedRegistryVectorReportsTruncation states which refusal a registry vector whose
// region ends mid code point produces, over every field of one structure.
//
// The condition is narrow and the reason it is stated at all is not. readOneUint16 returns the
// Reader's error, and an element decoder that dropped it instead would still be refused --
// syntax.ReadVector notices that the element consumed nothing and raises ErrZeroLengthElement
// -- so the swallowed error is invisible to every round trip, golden and corruption property in
// this file. It was measured: dropping it changes nothing any other test here can see. What it
// does change is what the caller is told, from "the input is truncated", which is true and says
// where to look, to "a vector element consumed zero bytes", which describes a decoder fault the
// input did not cause.
//
// The encoding is built rather than corrupted so the failing field is the one chosen: every
// other region is present and empty, and only the field under test declares an odd body.
func checkTruncatedRegistryVectorReportsTruncation[T any, PT interface {
	*T
	syntax.Codec
}](t *testing.T) {
	t.Helper()
	fields := reflect.TypeOf((*T)(nil)).Elem().NumField()
	if fields == 0 {
		t.Fatalf("%T declares no field, so this states nothing", *new(T))
	}
	for i := range fields {
		encoded := []byte{}
		for j := range fields {
			switch {
			case j == i:
				// a one octet body, which is half a code point
				encoded = append(encoded, 0x01, 0x00)
			default:
				encoded = append(encoded, 0x00)
			}
		}
		err := syntax.Unmarshal(encoded, PT(new(T)))
		if !errors.Is(err, syntax.ErrTruncated) {
			t.Errorf("field %d of %T: a one octet region gave err = %v, want ErrTruncated; the element decoder has to surface the reader's own failure rather than leave the vector guard to name a different condition",
				i, *new(T), err)
		}
	}
}

func TestExtensionRefusesTrailingTruncatedAndNonCanonicalInput(t *testing.T) {
	checkCodecRefusals[Extension](t, goldenExtensionEncodings())
}

// TestExtensionsVectorReportsTheEntryDecodersOwnFailure states which refusal an extensions
// vector produces when its region is well formed and the ENTRY inside it is not.
//
// Same shape as the code point truncation above and the same reason for existing.
// readOneExtension returns the entry decoder's error, and one that dropped it would still be
// refused: syntax.ReadVector notices either that the element consumed nothing or, at the end
// of the region, that the sub reader is latched. That was measured exhaustively over every
// octet string of four bytes or fewer, and the accept set is identical to the byte -- 16908545
// of them accepted with the error dropped and with it kept -- so no other property in this
// package can see the difference. What does change, for 33554688 of those inputs, is what the
// caller is told: "vector element consumed zero bytes", which describes a decoder fault, in
// place of the truncation or overlong length the input actually carried.
//
// The family is derived from the entry goldens: a vector whose declared region is exactly the
// first k octets of a valid entry, for every k short of the whole, so the region is intact and
// only the entry inside it is cut.
func TestExtensionsVectorReportsTheEntryDecodersOwnFailure(t *testing.T) {
	checked := 0
	for name, entry := range goldenExtensionEncodings() {
		for k := 1; k < len(entry); k++ {
			if k > 60 {
				// the region prefix below is written as one octet, which expresses 63
				break
			}
			encoded := append([]byte{byte(k)}, entry[:k]...)
			out, err := ReadExtensions(syntax.NewReader(encoded))
			if err == nil {
				t.Errorf("%s: a %d octet region holding a cut entry was accepted as %v", name, k, out)
				continue
			}
			if errors.Is(err, syntax.ErrZeroLengthElement) {
				t.Errorf("%s: a %d octet region holding a cut entry reported %v; that names a decoder fault rather than the truncation the input carries, which is what dropping the entry decoder's error looks like from outside",
					name, k, err)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no cut entry was built, so this states nothing")
	}
	t.Logf("%d cut entries refused with the entry decoder's own condition", checked)
}

func TestCapabilitiesReportsTruncationForEveryHalfReadCodePoint(t *testing.T) {
	checkTruncatedRegistryVectorReportsTruncation[Capabilities](t)
}

func TestRequiredCapabilitiesReportsTruncationForEveryHalfReadCodePoint(t *testing.T) {
	checkTruncatedRegistryVectorReportsTruncation[RequiredCapabilities](t)
}

func TestCapabilitiesRefusesTrailingTruncatedAndNonCanonicalInput(t *testing.T) {
	checkCodecRefusals[Capabilities](t, goldenCapabilitiesEncodings())
}

func TestRequiredCapabilitiesRefusesTrailingTruncatedAndNonCanonicalInput(t *testing.T) {
	checkCodecRefusals[RequiredCapabilities](t, goldenRequiredCapabilitiesEncodings())
}

// ---------------------------------------------------------------------------
// the generated round trip corpus
// ---------------------------------------------------------------------------

// registryVectorShapes is the vector shapes one registry field is generated over, derived from
// the code points the package declares for that registry rather than from a list beside it: a
// constant added to a registry enters this corpus in the commit that declares it.
//
// The shapes are the ones the encoding branches on -- absent, empty, one element, every
// declared element plus the two boundary code points, and a body that crosses the octet width
// of the MLS varint at 63 and 64 octets.
func registryVectorShapes(t *testing.T, typeName string) [][]uint64 {
	t.Helper()
	declared := sortedValues(registryConstantsOfType(t, typeName))
	all := append(slices.Clone(declared), 0x0000, 0xffff)
	slices.Sort(all)
	all = slices.Compact(all)

	long := func(count int) []uint64 {
		out := make([]uint64, 0, count)
		for i := range count {
			out = append(out, uint64(0x0100+i))
		}
		return out
	}
	return [][]uint64{
		nil,
		{},
		{declared[0]},
		all,
		// 31 elements is 62 octets, the last body a one octet varint prefix expresses; 32 is
		// 64 and needs two. An encoder writing the prefix as a uint8 is correct for every
		// shape above and wrong for the one below it.
		long(31),
		long(32),
	}
}

// generatedRegistryStructs builds the round trip corpus for one structure whose every field is
// a slice of a registry type, by varying one field at a time over registryVectorShapes with
// the others held at a base, and then a full cross product over the first three shapes.
//
// One at a time plus a bounded cross product rather than the full one: six shapes over five
// fields is 7776 cases and every one of them costs a type check nothing, but the shapes that
// interact are the adjacent ones -- a length prefix read at the wrong width takes its extra
// octet from the field after it -- so the cross product is where the interaction is and the
// long bodies are where the width is.
func generatedRegistryStructs[T any](t *testing.T, typeNames []string) []*T {
	t.Helper()
	structType := reflect.TypeOf((*T)(nil)).Elem()
	if structType.NumField() != len(typeNames) {
		t.Fatalf("%s has %d fields and %d registry names were given, so this corpus varies fields that do not exist or misses ones that do",
			structType.Name(), structType.NumField(), len(typeNames))
	}
	shapes := make([][][]uint64, len(typeNames))
	for i, typeName := range typeNames {
		if got := structType.Field(i).Type.Elem().Name(); got != typeName {
			t.Fatalf("%s field %d is a slice of %s and the name given for it is %s",
				structType.Name(), i, got, typeName)
		}
		shapes[i] = registryVectorShapes(t, typeName)
	}

	build := func(pick []int) *T {
		value := new(T)
		element := reflect.ValueOf(value).Elem()
		for i := range pick {
			shape := shapes[i][pick[i]]
			if shape == nil {
				continue
			}
			field := element.Field(i)
			slice := reflect.MakeSlice(field.Type(), 0, len(shape))
			for _, code := range shape {
				item := reflect.New(field.Type().Elem()).Elem()
				item.SetUint(code)
				slice = reflect.Append(slice, item)
			}
			field.Set(slice)
		}
		return value
	}

	corpus := []*T{}
	base := make([]int, len(typeNames))
	for i := range typeNames {
		for shape := range shapes[i] {
			pick := slices.Clone(base)
			pick[i] = shape
			corpus = append(corpus, build(pick))
		}
	}
	// the bounded cross product: absent, empty and a one element vector in every field
	// combination, which is where a prefix read at the wrong width shows up as the field
	// after it taking the wrong bytes
	pick := make([]int, len(typeNames))
	var cross func(int)
	cross = func(depth int) {
		if depth == len(typeNames) {
			corpus = append(corpus, build(slices.Clone(pick)))
			return
		}
		for shape := 0; shape < 3; shape++ {
			pick[depth] = shape
			cross(depth + 1)
		}
	}
	cross(0)
	return corpus
}

// sameRegistryVectors reports whether two structures whose every field is a slice of a ~uint16
// registry carry the same code points in the same order.
//
// nil and empty count as equal, deliberately: the wire has one spelling for both, so a decoder
// that returns an empty slice where the caller supplied nil has not changed the value it
// describes. reflect.DeepEqual would report those as different and would make every corpus
// case holding a nil field a false failure.
func sameRegistryVectors(left any, right any) bool {
	l := reflect.ValueOf(left).Elem()
	r := reflect.ValueOf(right).Elem()
	if l.Type() != r.Type() {
		return false
	}
	for i := 0; i < l.NumField(); i++ {
		lf, rf := l.Field(i), r.Field(i)
		if lf.Len() != rf.Len() {
			return false
		}
		for j := 0; j < lf.Len(); j++ {
			if lf.Index(j).Uint() != rf.Index(j).Uint() {
				return false
			}
		}
	}
	return true
}

// describeRegistryVectors renders such a structure for a failure message.
func describeRegistryVectors(value any) string {
	element := reflect.ValueOf(value).Elem()
	parts := []string{}
	for i := 0; i < element.NumField(); i++ {
		codes := []string{}
		for j := 0; j < element.Field(i).Len(); j++ {
			codes = append(codes, fmt.Sprintf("%#04x", element.Field(i).Index(j).Uint()))
		}
		parts = append(parts, fmt.Sprintf("%s[%s]", element.Type().Field(i).Name, strings.Join(codes, " ")))
	}
	return strings.Join(parts, " ")
}

// checkRegistryStructRoundTrip states the byte exact round trip over a generated corpus: the
// encoding decodes to a value carrying the same code points, and re-encoding that value
// reproduces the octets exactly.
//
// Both halves, because either alone is satisfied by a defect the other catches. A byte exact
// re-encode alone is satisfied by a codec that dropped one field from both halves, since the
// octets it never wrote are the octets it never reads. A value comparison alone is satisfied by
// a codec whose length prefix is not minimal, since it decodes back to the same code points
// through bytes no peer would have written.
func checkRegistryStructRoundTrip[T any, PT interface {
	*T
	syntax.Codec
}](t *testing.T, corpus []*T) {
	t.Helper()
	if len(corpus) < 20 {
		t.Fatalf("the corpus holds %d cases, which is fewer than the generator's own axes produce, so it was built from a derivation that read nothing",
			len(corpus))
	}
	for _, value := range corpus {
		encoded, err := syntax.Marshal(PT(value))
		if err != nil {
			t.Fatalf("%s: syntax.Marshal: %v", describeRegistryVectors(value), err)
		}
		decoded := PT(new(T))
		if err := syntax.Unmarshal(encoded, decoded); err != nil {
			t.Fatalf("%s: syntax.Unmarshal: %v", describeRegistryVectors(value), err)
		}
		if !sameRegistryVectors(decoded, value) {
			t.Fatalf("round trip of %s gave %s", describeRegistryVectors(value), describeRegistryVectors(decoded))
		}
		reencoded, err := syntax.Marshal(decoded)
		if err != nil {
			t.Fatalf("%s: re-encode: %v", describeRegistryVectors(value), err)
		}
		if !bytes.Equal(reencoded, encoded) {
			t.Fatalf("%s: re-encode =\n %x\nwant\n %x", describeRegistryVectors(value), reencoded, encoded)
		}
	}
	t.Logf("%d generated cases round tripped byte exact", len(corpus))
}

func TestCapabilitiesRoundTripsByteExactOverTheGeneratedCorpus(t *testing.T) {
	corpus := generatedRegistryStructs[Capabilities](t,
		[]string{"ProtocolVersion", "CipherSuite", "ExtensionType", "ProposalType", "CredentialType"})
	checkRegistryStructRoundTrip[Capabilities](t, corpus)
}

func TestRequiredCapabilitiesRoundTripsByteExactOverTheGeneratedCorpus(t *testing.T) {
	corpus := generatedRegistryStructs[RequiredCapabilities](t,
		[]string{"ExtensionType", "ProposalType", "CredentialType"})
	checkRegistryStructRoundTrip[RequiredCapabilities](t, corpus)
}

// checkEveryFieldChangesTheEncoding is the dropped field gate, stated over a class read off the
// struct type: for every field, two values differing only in that field must encode
// differently.
//
// A field dropped from BOTH codec halves is invisible to every round trip property in this
// file -- the octets never written are the octets never read, and the decode reproduces what
// the encode produced. What sees it is this: the field stops affecting the encoding at all.
//
// Derived over reflect rather than written out, because a field added to either structure
// enters the gate in the commit that declares it. That is rule 5 and it is not hypothetical
// here: Capabilities carries five vectors of the same shape, and a sixth would be exactly as
// easy to leave out of a hand written list as the fifth was.
func checkEveryFieldChangesTheEncoding[T any, PT interface {
	*T
	syntax.Codec
}](t *testing.T) {
	t.Helper()
	structType := reflect.TypeOf((*T)(nil)).Elem()
	if structType.NumField() == 0 {
		t.Fatalf("%s declares no field, so this gate judges nothing", structType.Name())
	}
	base := PT(new(T))
	baseValue := reflect.ValueOf(base).Elem()
	for i := 0; i < structType.NumField(); i++ {
		field := baseValue.Field(i)
		slice := reflect.MakeSlice(field.Type(), 1, 1)
		slice.Index(0).SetUint(0x0001)
		field.Set(slice)
	}
	baseEncoded, err := syntax.Marshal(base)
	if err != nil {
		t.Fatalf("%s: syntax.Marshal the base: %v", structType.Name(), err)
	}
	for i := 0; i < structType.NumField(); i++ {
		varied := PT(new(T))
		reflect.ValueOf(varied).Elem().Set(baseValue)
		field := reflect.ValueOf(varied).Elem().Field(i)
		slice := reflect.MakeSlice(field.Type(), 1, 1)
		slice.Index(0).SetUint(0x0002)
		field.Set(slice)

		encoded, err := syntax.Marshal(varied)
		if err != nil {
			t.Fatalf("%s.%s: syntax.Marshal: %v", structType.Name(), structType.Field(i).Name, err)
		}
		if bytes.Equal(encoded, baseEncoded) {
			t.Errorf("%s.%s: changing it left the encoding identical at %x, so nothing in the codec carries it",
				structType.Name(), structType.Field(i).Name, encoded)
			continue
		}
		decoded := PT(new(T))
		if err := syntax.Unmarshal(encoded, decoded); err != nil {
			t.Fatalf("%s.%s: syntax.Unmarshal: %v", structType.Name(), structType.Field(i).Name, err)
		}
		if !sameRegistryVectors(decoded, varied) {
			t.Errorf("%s.%s: it did not survive the round trip: %s came back as %s",
				structType.Name(), structType.Field(i).Name,
				describeRegistryVectors(varied), describeRegistryVectors(decoded))
		}
	}
}

func TestEveryCapabilitiesFieldChangesTheEncoding(t *testing.T) {
	checkEveryFieldChangesTheEncoding[Capabilities](t)
}

func TestEveryRequiredCapabilitiesFieldChangesTheEncoding(t *testing.T) {
	checkEveryFieldChangesTheEncoding[RequiredCapabilities](t)
}

// ---------------------------------------------------------------------------
// the registry code points
// ---------------------------------------------------------------------------

// registryCodePoints is what RFC 9420 section 17 assigns, transcribed from the RFC.
//
// The values and not merely the names, because two constants of one registry swapped compile,
// round trip, satisfy every predicate below and every symmetry property in this file, and are
// wrong in exactly the way that makes this implementation refuse a peer's Remove as an Update.
// Nothing else in this package would notice.
var registryCodePoints = map[string]map[string]uint64{
	"ProtocolVersion": {
		"ProtocolVersionMls10": 0x0001,
	},
	"CredentialType": {
		"CredentialTypeBasic": 0x0001,
	},
	"ProposalType": {
		"ProposalTypeReserved":               0x0000,
		"ProposalTypeAdd":                    0x0001,
		"ProposalTypeUpdate":                 0x0002,
		"ProposalTypeRemove":                 0x0003,
		"ProposalTypePreSharedKey":           0x0004,
		"ProposalTypeReInit":                 0x0005,
		"ProposalTypeExternalInit":           0x0006,
		"ProposalTypeGroupContextExtensions": 0x0007,
	},
	"ExtensionType": {
		"ExtensionTypeApplicationId":           0x0001,
		"ExtensionTypeRatchetTree":             0x0002,
		"ExtensionTypeRequiredCapabilities":    0x0003,
		"ExtensionTypeExternalPub":             0x0004,
		"ExtensionTypeExternalSenders":         0x0005,
		"ExtensionTypeUrmessageGroupPolicy":    0xF001,
		"ExtensionTypeUrmessageLeafKeys":       0xF002,
		"ExtensionTypeUrmessageOwnerSuccessor": 0xF003,
	},
}

// rfc9420Section72DefaultExtensionTypes is RFC 9420 section 7.2's default extension type list,
// transcribed as NAMES against code points:
//
//	The following proposal and extension types are considered "default" and MUST NOT be
//	listed:
//	...
//	*  Extension types:
//	   -  0x0001 - application_id
//	   -  0x0002 - ratchet_tree
//	   -  0x0003 - required_capabilities
//	   -  0x0004 - external_pub
//	   -  0x0005 - external_senders
//
// It is a SECOND transcription of the same five assignments the table above holds, and that is
// the point of it. registryCodePoints is one person reading section 17.3 and typing values
// beside Go identifiers; this is the same registry read out of section 7.2, where the RFC
// writes the code point beside the RFC's own name for it. The gate below joins the two on the
// name rather than on the value, so a constant declared at its neighbour's code point -- which
// is exactly what ExtensionTypeExternalSenders was, at external_pub's 0x0004, defended by a
// registryCodePoints entry written from the same misreading -- fails here. A pin transcribed
// once is a pin that agrees with whatever the transcriber believed.
var rfc9420Section72DefaultExtensionTypes = map[string]uint64{
	"application_id":        0x0001,
	"ratchet_tree":          0x0002,
	"required_capabilities": 0x0003,
	"external_pub":          0x0004,
	"external_senders":      0x0005,
}

// rfcNameOfRegistryConstant turns a declared constant's Go name into the RFC's own spelling of
// the same code point: the registry type prefix comes off and the CamelCase remainder becomes
// snake_case. ExtensionTypeExternalSenders is external_senders.
//
// Derived rather than a second map from Go name to RFC name, because a hand written join table
// can carry the very error the join exists to find: pair ExtensionTypeExternalSenders with
// "external_pub" and the gate passes at the wrong code point, which is the state this package
// was in.
func rfcNameOfRegistryConstant(typeName string, constantName string) string {
	remainder := strings.TrimPrefix(constantName, typeName)
	out := []rune{}
	for i, r := range remainder {
		if i > 0 && unicode.IsUpper(r) {
			out = append(out, '_')
		}
		out = append(out, unicode.ToLower(r))
	}
	return string(out)
}

// TestEveryRfc9420DefaultExtensionTypeIsDeclaredAtTheCodePointItAssigns joins the package's
// declared ExtensionType constants to section 7.2's list BY NAME, in both directions.
//
// Both directions, because the two failures are different and neither implies the other. A
// section 7.2 name this package declares nothing for is a default code point no gate here can
// cross check -- 0x0004 and 0x0005 were both in that state, which is how they came to be
// swapped -- and a declared constant whose RFC name sits at a different code point is the swap
// itself. isDefaultExtensionType is a numeric RANGE, so it is blind to both: it exempts
// 0x0001..0x0005 whatever this package calls them.
func TestEveryRfc9420DefaultExtensionTypeIsDeclaredAtTheCodePointItAssigns(t *testing.T) {
	declared := registryConstantsOfType(t, "ExtensionType")
	byRfcName := map[string]uint64{}
	for name, value := range declared {
		rfcName := rfcNameOfRegistryConstant("ExtensionType", name)
		if other, repeated := byRfcName[rfcName]; repeated {
			t.Fatalf("two declared constants spell the RFC name %s, at %#04x and %#04x, so the join below is ambiguous",
				rfcName, other, value)
		}
		byRfcName[rfcName] = value
	}
	for _, rfcName := range slices.Sorted(maps.Keys(rfc9420Section72DefaultExtensionTypes)) {
		assigned := rfc9420Section72DefaultExtensionTypes[rfcName]
		got, present := byRfcName[rfcName]
		if !present {
			t.Errorf("RFC 9420 section 7.2 names %s at %#04x and this package declares no ExtensionType constant spelling it; an unnamed code point is one nothing cross checks",
				rfcName, assigned)
			continue
		}
		if got != assigned {
			t.Errorf("this package declares %s at %#04x and RFC 9420 section 7.2 assigns it %#04x", rfcName, got, assigned)
		}
	}
	// and the other direction: a declared constant whose RFC name is one of the five must hold
	// that code point, which the loop above states, plus no declared constant may sit ON a
	// section 7.2 code point under a name section 7.2 does not give it.
	byValue := map[uint64]string{}
	for rfcName, value := range rfc9420Section72DefaultExtensionTypes {
		byValue[value] = rfcName
	}
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		value := declared[name]
		rfcName, isDefaultPoint := byValue[value]
		if !isDefaultPoint {
			continue
		}
		if got := rfcNameOfRegistryConstant("ExtensionType", name); got != rfcName {
			t.Errorf("%s is declared at %#04x, which RFC 9420 section 7.2 assigns to %s, and its own name spells %s",
				name, value, rfcName, got)
		}
	}
	if len(rfc9420Section72DefaultExtensionTypes) != int(defaultExtensionTypeHigh-defaultExtensionTypeLow+1) {
		t.Errorf("section 7.2 names %d default extension types and leaf_node.go's range spans %d code points",
			len(rfc9420Section72DefaultExtensionTypes), defaultExtensionTypeHigh-defaultExtensionTypeLow+1)
	}
}

// registryTypesDeclaredIn is every registry type extension.go declares, derived as every named
// type of that file whose underlying type is uint16.
//
// Derived and not listed, because the table above is a claim about a set and a claim about a
// set is worth what the set's derivation is worth. A fifth registry declared in this file with
// no entry in the table is then a failure naming it, rather than a registry whose code points
// nothing ever checked.
func registryTypesDeclaredIn(t *testing.T, file string) []string {
	t.Helper()
	declared := packageLevelDeclarations(t, ".")
	pkg := typeCheckedPackage(t)
	names := []string{}
	for name, declaredIn := range declared {
		if declaredIn != file {
			continue
		}
		typeName, isType := pkg.Scope().Lookup(name).(*types.TypeName)
		if !isType {
			continue
		}
		basic, isBasic := typeName.Type().Underlying().(*types.Basic)
		if !isBasic || basic.Kind() != types.Uint16 {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// TestEveryRegistryDeclaredHereHoldsTheCodePointsTheRfcAssigns is the enum value pin, stated in
// both directions over a derived class: extension.go's registry types are read off the type
// checker, the constants of each are read off it too, and the table above must hold exactly
// those names at exactly those values.
//
// Both directions matter and neither is redundant. A table richer than the source names a
// constant that no longer exists; a source richer than the table is a code point nothing
// checks, which is how a swapped pair survives. And a registry type with no table entry at all
// is the loud case, because that is the shape a new registry takes.
func TestEveryRegistryDeclaredHereHoldsTheCodePointsTheRfcAssigns(t *testing.T) {
	declared := registryTypesDeclaredIn(t, "extension.go")
	if len(declared) == 0 {
		t.Fatal("no registry type was derived from extension.go, which certainly declares several, so this gate read nothing")
	}
	if got, want := declared, slices.Sorted(maps.Keys(registryCodePoints)); !slices.Equal(got, want) {
		t.Fatalf("extension.go declares the registries %v and the table holds %v; a registry with no entry is one whose code points nothing checks",
			got, want)
	}
	for _, typeName := range declared {
		got := registryConstantsOfType(t, typeName)
		if want := registryCodePoints[typeName]; !maps.Equal(got, want) {
			t.Errorf("%s declares\n %v\nand RFC 9420 section 17 assigns\n %v", typeName, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// the validation surface
// ---------------------------------------------------------------------------

// requiredCapabilityFields pairs each RequiredCapabilities field with the Capabilities field
// that answers for it, matched by ELEMENT TYPE rather than by name.
//
// Matching by type is what makes this a derivation rather than a table: a fourth requirement
// added to RequiredCapabilities has to find a Capabilities field of its own element type or
// this fails, and a requirement silently answered by the wrong predicate is impossible because
// the two fields would have to carry the same registry.
//
// Every field of RequiredCapabilities must match exactly one field of Capabilities, and a
// field of either that does not is fatal rather than skipped, since a skipped requirement is
// exactly the hole this whole file exists to close.
func requiredCapabilityFields(t *testing.T) map[string]string {
	t.Helper()
	required := reflect.TypeOf(RequiredCapabilities{})
	capabilities := reflect.TypeOf(Capabilities{})
	pairs := map[string]string{}
	for i := 0; i < required.NumField(); i++ {
		field := required.Field(i)
		matches := []string{}
		for j := 0; j < capabilities.NumField(); j++ {
			if capabilities.Field(j).Type == field.Type {
				matches = append(matches, capabilities.Field(j).Name)
			}
		}
		if len(matches) != 1 {
			t.Fatalf("RequiredCapabilities.%s is a %s and Capabilities carries %d fields of that type (%v); the pairing has to be exactly one or a requirement is answered by the wrong predicate",
				field.Name, field.Type, len(matches), matches)
		}
		pairs[field.Name] = matches[0]
	}
	if len(pairs) != required.NumField() {
		t.Fatalf("%d of RequiredCapabilities' %d fields were paired", len(pairs), required.NumField())
	}
	return pairs
}

// mandatoryToImplement is every registry code point a member counts as supporting whether or
// not its own vector lists it, keyed by registry type and code point. RFC 9420 section 7.2
// makes the basic credential mandatory to implement, and it is the only such carve out in the
// specification.
//
// This is a written table and the sweeps below do NOT trust it: the same set is DERIVED by
// running each predicate against a member whose vectors are all empty, and the two must agree
// exactly. So a carve out that grows -- a predicate that started answering true for everything,
// which is precisely the permissive-in-the-wrong-direction defect this file is about -- fails
// by appearing in the derived set and not in this one, and a carve out that is removed fails
// the other way.
var mandatoryToImplement = map[string]bool{
	"CredentialType/0x0001": true,
}

// carveOutKey spells one entry of that table. Keyed by the registry TYPE rather than by a
// struct field name, because the two sweeps below reach the same code point from two different
// structs, and a key taken from either struct's field names would need a second name
// correspondence to be right.
func carveOutKey(registryName string, code uint64) string {
	return fmt.Sprintf("%s/%#04x", registryName, code)
}

// unregisteredProbes are the code points these sweeps use that no registry in this package
// declares, and they come in a pair for a reason the single one they replaced could not serve.
//
// One probe closes only the constant-range shape: a predicate answering true for everything
// below 0x0010 is caught by any probe above the declared block. It cannot close an ORDERING
// shape on a registry that declares a single constant -- CredentialType declares only basic --
// because with one declared value and one probe, every pair the sweep can build sits on the
// same side of the comparison and `<=` or `>=` answers exactly as `==` does. Two probes
// straddling the declared block make both directions visible on every registry, which is what
// TestEverySupportsPredicateReadsBothOfItsOperands needs to be able to state.
//
// 0x0bad sits above the dense low block every registry declares and below the 0xF00x private
// use values; 0xbeef sits above both.
var unregisteredProbes = []uint64{0x0bad, 0xbeef}

// capabilityProbes is the code point set each requirement is exercised over: every constant the
// package declares for that registry, plus the two the package declares for nothing.
//
// The GREASE probes are not decoration. The declared constants are small and dense, so a
// predicate comparing on a range rather than on equality -- everything below 0x0010 supported,
// say -- answers correctly for all of them.
func capabilityProbes(t *testing.T, typeName string) []uint64 {
	t.Helper()
	probes := sortedValues(registryConstantsOfType(t, typeName))
	for _, grease := range unregisteredProbes {
		if slices.Contains(probes, grease) {
			t.Fatalf("%s declares %#04x, so it is not an unregistered probe this sweep can use", typeName, grease)
		}
		probes = append(probes, grease)
	}
	return probes
}

// buildCapabilities returns a Capabilities whose named field holds exactly the given code
// points and whose other fields are empty.
func buildCapabilities(fieldName string, codes []uint64) *Capabilities {
	value := &Capabilities{}
	field := reflect.ValueOf(value).Elem().FieldByName(fieldName)
	slice := reflect.MakeSlice(field.Type(), 0, len(codes))
	for _, code := range codes {
		item := reflect.New(field.Type().Elem()).Elem()
		item.SetUint(code)
		slice = reflect.Append(slice, item)
	}
	field.Set(slice)
	return value
}

// buildRequiredCapabilities returns a RequiredCapabilities whose named field requires exactly
// the given code points and which requires nothing else.
//
// Variadic rather than one code point, because the arity of a requirement vector is itself an
// axis a sweep can leave fixed. Every requirement this file used to build carried exactly one
// entry, so Supports' three loops each ran their body exactly once and a loop truncated to its
// first element was indistinguishable from a whole one -- the group naming two required
// extensions has one of them enforced, at full width and in silence.
func buildRequiredCapabilities(fieldName string, codes ...uint64) *RequiredCapabilities {
	value := &RequiredCapabilities{}
	field := reflect.ValueOf(value).Elem().FieldByName(fieldName)
	slice := reflect.MakeSlice(field.Type(), len(codes), len(codes))
	for i, code := range codes {
		slice.Index(i).SetUint(code)
	}
	field.Set(slice)
	return value
}

// TestSupportsAnswersBothDirectionsForEveryRequirementItCarries is the validation property, and
// it is stated in both directions because the two failures are symmetric, both silent, and
// neither visible to any codec property in this file.
//
// A check that accepts a member missing a required extension admits a member who cannot process
// the group's messages, and nothing reports an error until that member cannot decrypt. A check
// that rejects a member who has everything is a group nobody can join, and the failure names
// the member rather than the check. So for every requirement RequiredCapabilities carries and
// every code point the package declares for it, this asserts a member holding it is accepted
// and a member without it is refused.
//
// The class is derived twice over: the requirements from the struct type, the code points from
// the type checker. A list of either would be the hand written class rule 5 objects to, and
// Capabilities is five vectors of one shape, which is the easiest possible thing to write four
// of.
func TestSupportsAnswersBothDirectionsForEveryRequirementItCarries(t *testing.T) {
	pairs := requiredCapabilityFields(t)
	requiredType := reflect.TypeOf(RequiredCapabilities{})
	derivedCarveOuts := map[string]bool{}
	registriesSwept := map[string]bool{}
	accepted, refused := 0, 0
	for _, requirement := range slices.Sorted(maps.Keys(pairs)) {
		capabilityField := pairs[requirement]
		field, found := requiredType.FieldByName(requirement)
		if !found {
			t.Fatalf("RequiredCapabilities declares no field %s", requirement)
		}
		registry := field.Type.Elem().Name()
		registriesSwept[registry] = true
		for _, code := range capabilityProbes(t, registry) {
			rc := buildRequiredCapabilities(requirement, code)

			// the member who has it
			holder := buildCapabilities(capabilityField, []uint64{code})
			if err := holder.Supports(rc); err != nil {
				t.Errorf("%s %#04x: a member listing it was refused with %v; a requirement nobody can satisfy is a group nobody can join",
					requirement, code, err)
			} else {
				accepted++
			}

			// the member who does not
			without := buildCapabilities(capabilityField, nil)
			err := without.Supports(rc)
			if err == nil {
				derivedCarveOuts[carveOutKey(registry, code)] = true
				continue
			}
			refused++
			if !errors.Is(err, errMissingRequiredCapability) {
				t.Errorf("%s %#04x: a member without it was refused with %v, want errMissingRequiredCapability",
					requirement, code, err)
			}
		}
	}
	if accepted == 0 || refused == 0 {
		t.Fatalf("%d requirements were accepted and %d refused; a sweep that never reached one of the two directions states only the other",
			accepted, refused)
	}
	// restricted to the registries this sweep reaches: versions and ciphersuites are not
	// required of a member through RequiredCapabilities at all, so a carve out in one of
	// those is the predicate sweep's to judge and would be a false failure here.
	expected := map[string]bool{}
	for key := range mandatoryToImplement {
		if registriesSwept[strings.SplitN(key, "/", 2)[0]] {
			expected[key] = true
		}
	}
	if !maps.Equal(derivedCarveOuts, expected) {
		t.Errorf("a member with empty capabilities is accepted for %v and the mandatory to implement set reachable from a requirement is %v; a carve out that grew is a predicate answering true for something the member never claimed",
			slices.Sorted(maps.Keys(derivedCarveOuts)), slices.Sorted(maps.Keys(expected)))
	}
	t.Logf("%d (requirement, code point) pairs accepted with it, %d refused without it, %d carve outs",
		accepted, refused, len(derivedCarveOuts))
}

// TestSupportsAcceptsAMemberThatHasEverythingAndRefusesOneShortOfAnything states the whole
// check rather than one requirement at a time.
//
// The one-at-a-time sweep above cannot see a Supports that stops at its first satisfied loop:
// each of its RequiredCapabilities names exactly one requirement, so a body that returned nil
// after the extensions loop passes every case it produces. This requires all three at once,
// then drops one requirement at a time from the member and requires a refusal each time.
func TestSupportsAcceptsAMemberThatHasEverythingAndRefusesOneShortOfAnything(t *testing.T) {
	pairs := requiredCapabilityFields(t)
	full := &Capabilities{}
	rc := &RequiredCapabilities{}
	// a probe per requirement, none of them a carve out, so dropping any one has to be
	// visible
	probes := map[string]uint64{}
	for _, requirement := range slices.Sorted(maps.Keys(pairs)) {
		probes[requirement] = 0xbeef
		fullField := reflect.ValueOf(full).Elem().FieldByName(pairs[requirement])
		slice := reflect.MakeSlice(fullField.Type(), 1, 1)
		slice.Index(0).SetUint(probes[requirement])
		fullField.Set(slice)

		rcField := reflect.ValueOf(rc).Elem().FieldByName(requirement)
		rcSlice := reflect.MakeSlice(rcField.Type(), 1, 1)
		rcSlice.Index(0).SetUint(probes[requirement])
		rcField.Set(rcSlice)
	}
	if len(probes) == 0 {
		t.Fatal("RequiredCapabilities carries no requirement, so this states nothing")
	}
	if err := full.Supports(rc); err != nil {
		t.Fatalf("a member listing every requirement was refused with %v", err)
	}
	if err := full.Supports(nil); err != nil {
		t.Errorf("a nil requirement was refused with %v; a group carrying no required_capabilities requires nothing", err)
	}
	if err := (&Capabilities{}).Supports(&RequiredCapabilities{}); err != nil {
		t.Errorf("an empty requirement was refused with %v", err)
	}
	for _, requirement := range slices.Sorted(maps.Keys(pairs)) {
		short := &Capabilities{}
		reflect.ValueOf(short).Elem().Set(reflect.ValueOf(full).Elem())
		reflect.ValueOf(short).Elem().FieldByName(pairs[requirement]).Set(
			reflect.MakeSlice(reflect.ValueOf(short).Elem().FieldByName(pairs[requirement]).Type(), 0, 0))
		err := short.Supports(rc)
		if !errors.Is(err, errMissingRequiredCapability) {
			t.Errorf("a member short of %s alone gave err = %v, want errMissingRequiredCapability; a loop that is never reached is a requirement that is never checked",
				requirement, err)
		}
	}
}

// capabilityPredicate pairs one field of Capabilities with the Supports predicate that answers
// for its registry, and carries the registry type so a probe of the right type can be built.
type capabilityPredicate struct {
	field     string
	registry  reflect.Type
	predicate reflect.Method
}

// ask runs the predicate over one member and one code point. It exists so the two sweeps below
// cannot spell the reflect call differently, which is the shape in which one of them comes to
// be asking a different question from the one its name claims.
func (self capabilityPredicate) ask(member *Capabilities, code uint64) bool {
	probe := reflect.New(self.registry).Elem()
	probe.SetUint(code)
	return self.predicate.Func.Call([]reflect.Value{reflect.ValueOf(member), probe})[0].Bool()
}

// capabilityPredicates derives the (field, predicate) pairing from Capabilities' own fields:
// each field is a slice of one registry, and the predicate for it is the method taking that
// registry's type. A sixth field added to Capabilities without a predicate fails here rather
// than being quietly unjudged, and so does a predicate that starts taking two arguments.
//
// One derivation site and two sweeps over it, because both of the sweeps below are claims about
// the SAME class -- every predicate the type carries -- and a second copy of the pairing is the
// second place for that class to be understated.
func capabilityPredicates(t *testing.T) []capabilityPredicate {
	t.Helper()
	capabilitiesType := reflect.TypeOf(Capabilities{})
	pointerType := reflect.TypeOf(&Capabilities{})
	pairs := []capabilityPredicate{}
	selected := map[string]bool{}
	for i := 0; i < capabilitiesType.NumField(); i++ {
		field := capabilitiesType.Field(i)
		registry := field.Type.Elem()
		predicates := []reflect.Method{}
		for m := 0; m < pointerType.NumMethod(); m++ {
			method := pointerType.Method(m)
			// THE SHAPE IS THE WHOLE TEST, and the name test that used to open this loop is
			// gone. It read !strings.HasPrefix(method.Name, "Supports") over a class the
			// compiler already describes, and measured at 81b97ca its complement was EMPTY:
			// MarshalMLS takes a *syntax.Writer, UnmarshalMLS a *syntax.Reader, and Supports a
			// *RequiredCapabilities answering an error, so all three were already removed by
			// the two clauses below. A narrowing whose complement is empty removes nothing
			// today and begins removing real members on the commit that adds the first one of
			// the shape it excludes -- here a predicate named Accepts or Knows, which would
			// have been unjudged while the one-field-one-predicate fatal below read clean.
			if method.Type.NumIn() != 2 || method.Type.In(1) != registry {
				continue
			}
			if method.Type.NumOut() != 1 || method.Type.Out(0).Kind() != reflect.Bool {
				continue
			}
			predicates = append(predicates, method)
		}
		if len(predicates) != 1 {
			t.Fatalf("Capabilities.%s is a slice of %s and %d methods take one %s and answer one bool (%v); one field, one predicate",
				field.Name, registry.Name(), len(predicates), registry.Name(), predicates)
		}
		selected[predicates[0].Name] = true
		pairs = append(pairs, capabilityPredicate{field: field.Name, registry: registry, predicate: predicates[0]})
	}
	if len(pairs) != capabilitiesType.NumField() {
		t.Fatalf("%d of Capabilities' %d fields were paired with a predicate", len(pairs), capabilitiesType.NumField())
	}
	// AND THE COMPLEMENT IS PRINTED, which is the operational half of the rule GATES.md
	// states: a gate that narrows names, at run time, every member it removed and the
	// predicate it removed them by. Empty here would mean every exported method of the type is
	// a registry predicate, which is not a pairing at all but the method set under another name.
	removed := []string{}
	for m := 0; m < pointerType.NumMethod(); m++ {
		method := pointerType.Method(m)
		if selected[method.Name] {
			continue
		}
		removed = append(removed, method.Name+" "+method.Type.String())
	}
	if len(removed) == 0 {
		t.Fatal("every exported method of *Capabilities was paired with a field as its predicate, so the shape test above removes nothing and this class is the method set under another name")
	}
	t.Logf("%d (field, predicate) pairs; the %d exported methods of *Capabilities this shape removes, none of which takes one registry code point and answers one bool: %v",
		len(pairs), len(removed), removed)
	return pairs
}

// TestEverySupportsPredicateAnswersBothDirections states the five leaf predicates the way the
// whole check is stated, because two of them -- versions and ciphersuites -- are not reachable
// through RequiredCapabilities at all and would otherwise be judged by nothing.
//
// This is the has-it / has-nothing axis only. The has-something-else axis is
// TestEverySupportsPredicateReadsBothOfItsOperands below, and neither subsumes the other: this
// one owns the carve out comparison, that one owns the shape of the comparison itself.
func TestEverySupportsPredicateAnswersBothDirections(t *testing.T) {
	derivedCarveOuts := map[string]bool{}
	judged := 0
	for _, entry := range capabilityPredicates(t) {
		for _, code := range capabilityProbes(t, entry.registry.Name()) {
			holder := buildCapabilities(entry.field, []uint64{code})
			if !entry.ask(holder, code) {
				t.Errorf("%s(%#04x) over a member listing it = false", entry.predicate.Name, code)
			}
			without := buildCapabilities(entry.field, nil)
			if entry.ask(without, code) {
				derivedCarveOuts[carveOutKey(entry.registry.Name(), code)] = true
			}
			judged++
		}
	}
	if judged == 0 {
		t.Fatal("no predicate was judged, so this gate read nothing off the type")
	}
	// this sweep reaches all five registries, so it owns the exact comparison: every carve out
	// the code makes has to be one the table argues for, and every one the table argues for has
	// to still be made.
	if !maps.Equal(derivedCarveOuts, mandatoryToImplement) {
		t.Errorf("the predicates answer true for %v over a member listing nothing, and the mandatory to implement set is %v",
			slices.Sorted(maps.Keys(derivedCarveOuts)), slices.Sorted(maps.Keys(mandatoryToImplement)))
	}
	t.Logf("%d (predicate, code point) pairs judged in both directions, %d carve outs",
		judged, len(derivedCarveOuts))
}

// TestEverySupportsPredicateReadsBothOfItsOperands states each predicate over the PAIR it is a
// function of: the code point the member holds, and the code point it is asked about.
//
// The sweep above varies only one of the two. Every member it builds either holds exactly the
// code point it is then asked about or holds nothing at all, so a predicate answering x >= t
// instead of x == t -- or x <= t -- satisfies every case it produces, in BOTH directions, and
// every other property in this file stays green while Supports admits a member whose vector
// lists nothing the group requires. Under >= a member listing only
// ExtensionTypeUrmessageOwnerSuccessor is reported as supporting ratchet_tree and
// required_capabilities. That is exactly the permissive-in-the-wrong-direction defect this
// file's header says it exists to close, and the has-it / has-nothing axis cannot see it,
// because a comparison is a function of two operands and that axis holds one of them equal to
// the other.
//
// So the class here is the CROSS PRODUCT of the probe set with itself, derived twice over --
// the predicates off the type, the code points off the type checker -- and the answer is
// required to depend on both operands: true exactly when the member holds what it is asked
// about, false for every other pair, unless the code point asked about is one the mandatory to
// implement table argues for.
//
// Two member shapes per code point, because arity is an axis too. The singleton holder is the
// smallest member that can tell the operands apart; the complement holder -- every probe the
// registry has EXCEPT the one being asked about -- is the shape a real leaf has, and it is what
// catches a predicate answering from the length of the vector rather than from its contents.
func TestEverySupportsPredicateReadsBothOfItsOperands(t *testing.T) {
	answers, crossPairs := 0, 0
	for _, entry := range capabilityPredicates(t) {
		registry := entry.registry.Name()
		probes := capabilityProbes(t, registry)
		if len(probes) < 2 {
			t.Fatalf("%s yielded %d probe(s), so no (holds A, asked B) pair with A != B exists for it and this sweep would state nothing about %s",
				registry, len(probes), entry.predicate.Name)
		}
		for _, asked := range probes {
			// the one carve out the specification argues for is the only code point a member
			// may be reported as supporting without listing it
			carveOut := mandatoryToImplement[carveOutKey(registry, asked)]
			for _, held := range probes {
				want := held == asked || carveOut
				if got := entry.ask(buildCapabilities(entry.field, []uint64{held}), asked); got != want {
					t.Errorf("%s(%#04x) over a member listing only %#04x = %v, want %v; the answer has to read both operands rather than their order",
						entry.predicate.Name, asked, held, got, want)
				}
				answers++
				if held != asked {
					crossPairs++
				}
			}
			complement := slices.DeleteFunc(slices.Clone(probes), func(code uint64) bool { return code == asked })
			if got := entry.ask(buildCapabilities(entry.field, complement), asked); got != carveOut {
				t.Errorf("%s(%#04x) over a member listing every other probe (%#04x) = %v, want %v; a predicate answering from the length of the vector rather than from its contents fails here",
					entry.predicate.Name, asked, complement, got, carveOut)
			}
			answers++
		}
	}
	if crossPairs == 0 {
		t.Fatal("no (holds A, asked B) pair with A != B was built, so this sweep only restates the axis the sweep above already owns")
	}
	t.Logf("%d predicate answers judged, %d of them over a member holding something other than what it was asked about",
		answers, crossPairs)
}

// TestSupportsEnforcesEveryEntryOfEveryRequirementVector states the whole check over the axis
// every other sweep in this file leaves fixed: how many code points one requirement carries.
//
// Every RequiredCapabilities built anywhere else here holds exactly one entry per field, so each
// of Supports' three loops runs its body exactly once and a loop truncated to its first element
// is indistinguishable from a whole one. A group whose required_capabilities names two
// extensions would then have one of them enforced and the other not: a member who cannot read
// what the group sends is admitted, silently, at full width, and nothing reports an error until
// that member cannot decrypt.
//
// So each requirement is built holding EVERY code point the package declares for its registry
// plus the unregistered probes, and the member is given all of them but one -- sweeping which
// one, so the LAST entry of the vector is dropped as well as the first. The refusal has to name
// the code point that was dropped, because a refusal naming a different one is a loop reading
// the wrong index, which passes an assertion that only checks the sentinel.
func TestSupportsEnforcesEveryEntryOfEveryRequirementVector(t *testing.T) {
	pairs := requiredCapabilityFields(t)
	requiredType := reflect.TypeOf(RequiredCapabilities{})
	refused, carvedOut := 0, 0
	for _, requirement := range slices.Sorted(maps.Keys(pairs)) {
		field, found := requiredType.FieldByName(requirement)
		if !found {
			t.Fatalf("RequiredCapabilities declares no field %s", requirement)
		}
		registry := field.Type.Elem().Name()
		probes := capabilityProbes(t, registry)
		if len(probes) < 2 {
			t.Fatalf("%s's registry %s yielded %d probe(s), so the requirement vector this builds has no second entry and a loop truncated to its first would still pass",
				requirement, registry, len(probes))
		}
		rc := buildRequiredCapabilities(requirement, probes...)

		// the member holding all of them is accepted, so every refusal below is attributable to
		// the one code point that was dropped rather than to the arity of the requirement
		if err := buildCapabilities(pairs[requirement], probes).Supports(rc); err != nil {
			t.Fatalf("%s: a member listing all %d required code points was refused with %v",
				requirement, len(probes), err)
		}
		for dropped, code := range probes {
			held := slices.Concat(probes[:dropped], probes[dropped+1:])
			err := buildCapabilities(pairs[requirement], held).Supports(rc)
			if mandatoryToImplement[carveOutKey(registry, code)] {
				if err != nil {
					t.Errorf("%s: dropping %#04x, which is mandatory to implement, was refused with %v",
						requirement, code, err)
				}
				carvedOut++
				continue
			}
			if !errors.Is(err, errMissingRequiredCapability) {
				t.Errorf("%s: a member holding every required %s except %#04x (entry %d of %d) gave err = %v, want errMissingRequiredCapability; a loop that stops before its last entry is a requirement nobody enforces",
					requirement, registry, code, dropped, len(probes), err)
				continue
			}
			if want := fmt.Sprintf("%#04x", code); !strings.Contains(err.Error(), want) {
				t.Errorf("%s: dropping %#04x was refused with %q, which does not name it; a refusal naming a different entry is a loop reading the wrong index",
					requirement, code, err)
			}
			refused++
		}
	}
	if refused == 0 {
		t.Fatal("no dropped requirement entry was refused, so this gate states nothing about the loops it exists to hold")
	}
	t.Logf("%d dropped requirement entries refused, %d passed over as mandatory to implement",
		refused, carvedOut)
}

// ---------------------------------------------------------------------------
// the name owed to the validation plan
// ---------------------------------------------------------------------------

// TestNoValidationOwnedNameHasLandedBesideItsStandIn fails on the commit that lands the real
// ErrMissingRequiredCapability, which is what stops the unexported stand in outliving it.
//
// Package mls is one package, so two exported declarations of one name would be a compile
// error -- the loud half. The quiet half is the one this closes: the stand in stays, the
// validation plan's sentinel lands beside it, this file's refusals keep answering to a value
// no caller outside the package can match, and errors.Is in the commit path reports false with
// nothing to say about it.
//
// The owed pair is derived from the package rather than listed. Every unexported err-prefixed
// declaration of every NON TEST file is a stand in for the exported name of the same spelling,
// so a second one added by a later task is watched without anybody editing this.
//
// The scan read extension.go alone until p5 task 5, which is the shortfall rule 5 names: the
// stand in shape is the package's convention and nothing confines it to one file. Task 5 landed
// credential.go carrying errProfileCredentialType for the same reason extension.go carries
// errMissingRequiredCapability, and under the narrower scan that second stand in would have been
// watched by nothing at all. Widening it took the watched set from one name to seven -- five of
// which psk.go and secret_tree.go had been carrying, unwatched, since before this gate existed.
//
// Not every one of those seven is a stand in for a name somebody else owns; some are internal
// invariants that simply happen to be spelled err-something. The property is the same either
// way and that is why the widened class is the right one: two values of one spelling, one
// exported and one not, is a pair errors.Is answers false for with nothing to say about it. What
// differs is the fix, so the message names both.
//
// Test files stay out: their errKat* comparator sentinels are the private business of the test
// that raises them, and there are dozens of them.
func TestNoValidationOwnedNameHasLandedBesideItsStandIn(t *testing.T) {
	declared := packageLevelDeclarations(t, ".")
	standIns := []string{}
	standInFiles := map[string]string{}
	for name, file := range declared {
		if strings.HasSuffix(file, "_test.go") || !strings.HasPrefix(name, "err") {
			continue
		}
		standIns = append(standIns, name)
		standInFiles[name] = file
	}
	slices.Sort(standIns)
	if !slices.Contains(standIns, "errMissingRequiredCapability") {
		t.Fatalf("the scan read the stand ins %v, and extension.go certainly declares errMissingRequiredCapability, so it read something other than this package", standIns)
	}
	for _, name := range standIns {
		owed := "Err" + strings.TrimPrefix(name, "err")
		if file, landed := declared[owed]; landed {
			t.Errorf("%s has landed in %s and %s still carries %s; if that is the stand in for it, replace every use with the landed sentinel and delete the stand in in the same commit, and if it is a different error then rename it -- errors.Is cannot tell two values of one spelling apart",
				owed, file, standInFiles[name], name)
		}
	}
	t.Logf("%d unexported error name(s) watched for an exported twin: %v", len(standIns), standIns)
}

// TestSupportsRefusalAnswersOnlyItsOwnSentinel keeps the stand in from being read as something
// else. It is wrapped with a detail naming the code point, so errors.Is has to hold through the
// wrap, and it must not answer to any sentinel of the plans that already have error files.
func TestSupportsRefusalAnswersOnlyItsOwnSentinel(t *testing.T) {
	caps := &Capabilities{}
	err := caps.Supports(&RequiredCapabilities{ExtensionTypes: []ExtensionType{ExtensionTypeUrmessageLeafKeys}})
	if !errors.Is(err, errMissingRequiredCapability) {
		t.Fatalf("err = %v, want errMissingRequiredCapability", err)
	}
	if !strings.Contains(err.Error(), "0xf002") {
		t.Errorf("err = %q and does not name the code point that failed; a caller told only that something is missing has to diff two vectors to find out what",
			err)
	}
	for _, class := range []map[string]error{treeOwnedErrors, treeMathOwnedErrors, keyScheduleOwnedErrors, cryptoOwnedErrors} {
		for _, name := range slices.Sorted(maps.Keys(class)) {
			if errors.Is(err, class[name]) {
				t.Errorf("the missing capability refusal answers to %s, and no plan argues for a wrap between a membership rule and that layer",
					name)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// urmessage_leaf_keys, extension type 0xF002
// ---------------------------------------------------------------------------

// The X-Wing encapsulation key size taken from the standard library instead of from this
// package, so that the 1216 has something to be wrong against.
//
// Every other statement of that number in this tree is prose or a copy of prose:
// draft-connolly-cfrg-xwing-kem-06 section 5.1 says 1216, Spec A section 3.4 says 1216, the
// interface registry says 1216, and p2 task 22 will assert that messagegroup.XwingPublicKeySize
// says 1216 too. A digit copied wrong out of any of them is invisible to all of the others,
// and it is invisible to every round trip and length test in this file as well, because those
// build their inputs out of XwingPublicKeyLen and would agree with a constant of 1217 exactly
// as well as with one of 1216. Measured rather than argued: with the constant set to 1217,
// every test the plan supplied for this task still passes.
//
// So the number is reassembled here out of its two halves as the draft defines them --
// X-Wing's public key is the ML-KEM-768 encapsulation key followed by the X25519 public key --
// and both halves come from crypto/mlkem and crypto/ecdh rather than from a literal. Neither
// is reachable from this package's non test source, so this import pair is confined to the
// test binary and TestTheCryptoIsBuiltFromExactlyThesePackages is unaffected by it.
//
// What this cannot check is the ORDER of the two halves, which is a property of the KEM and
// not of a length, and the KEM is p2's. It also cannot check the parameter set: an X-Wing over
// ML-KEM-1024 would be 1600 bytes and this would say so, but nothing here says 768 is the
// right choice. draft-connolly-cfrg-xwing-kem-06 fixes it, and A-ASSUME-3 pins this project to
// that draft.
func xwingPublicKeyLenFromTheStandardLibrary(t *testing.T) int {
	t.Helper()
	classical, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate an X25519 key to measure its public half: %v", err)
	}
	classicalLen := len(classical.PublicKey().Bytes())
	if classicalLen == 0 {
		t.Fatal("crypto/ecdh answered a zero length X25519 public key, so the sum below is the ML-KEM half alone")
	}
	if mlkem.EncapsulationKeySize768 == 0 {
		t.Fatal("crypto/mlkem gives ML-KEM-768 a zero length encapsulation key, so the sum below is the X25519 half alone")
	}
	return mlkem.EncapsulationKeySize768 + classicalLen
}

// TestXwingPublicKeyLenIsTheMlKem768AndX25519KeySizesAdded holds the one number in this file
// that nothing else in this package can check.
//
// p2 task 22 is titled "Pin message.XwingPublicKeySize against mls.XwingPublicKeyLen", so the
// cross package agreement is somebody else's task and it has not landed. Until it does, and
// after it does, this is what says the agreed number is the right one rather than the same
// mistake written twice.
func TestXwingPublicKeyLenIsTheMlKem768AndX25519KeySizesAdded(t *testing.T) {
	derived := xwingPublicKeyLenFromTheStandardLibrary(t)
	if XwingPublicKeyLen != derived {
		t.Errorf("XwingPublicKeyLen is %d and ML-KEM-768's encapsulation key (%d) plus X25519's public key (%d) is %d; draft-connolly-cfrg-xwing-kem-06 section 5.1 gives 1216 and one of these has a digit wrong",
			XwingPublicKeyLen, mlkem.EncapsulationKeySize768, derived-mlkem.EncapsulationKeySize768, derived)
	}
}

// One urmessage_leaf_keys body assembled field by field, so a body this package's own encoder
// refuses can still be handed to its own decoder.
//
// Every refusal on the decode side is otherwise untestable: Encode will not produce a body
// carrying alg 0x0013 or a 1215 byte key, so a decoder that had lost its own copy of those two
// checks would be exercised only by inputs that cannot reach it. This is the peer that does
// not run this profile, written out.
//
// It goes through syntax rather than through Encode on purpose. syntax is a different package
// with its own tests, so an input built with it is not built by the thing under test.
func leafKeysBodyBytes(t *testing.T, algId uint16, pub []byte) []byte {
	t.Helper()
	w := syntax.NewWriter()
	w.WriteUint16(algId)
	w.WriteOpaque(pub)
	body, err := w.Bytes()
	if err != nil {
		t.Fatalf("assemble a leaf keys body of alg %#04x over %d bytes: %v", algId, len(pub), err)
	}
	return body
}

// A device key of the length this profile requires, filled with a pattern rather than zeroes
// so a body that dropped or reordered it does not still compare equal.
func leafKeysTestKey() []byte {
	pub := make([]byte, XwingPublicKeyLen)
	for i := range pub {
		pub[i] = byte(i)
	}
	return pub
}

func TestLeafKeysExtensionRoundTrip(t *testing.T) {
	pub := leafKeysTestKey()
	in := &LeafKeysExtension{AlgId: AlgIdXwing, DeviceXwingPub: pub}
	ext, err := in.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if ext.ExtensionType != ExtensionTypeUrmessageLeafKeys {
		t.Fatalf("Encode tagged %#x, want %#x", ext.ExtensionType, ExtensionTypeUrmessageLeafKeys)
	}
	// the wire layout, written out by hand rather than captured from the encoder. A golden
	// taken from the thing under test pins nothing: alg_id written little endian, or the
	// device key written raw with no length prefix, round trips through this package
	// perfectly and disagrees with every peer.
	//
	//	uint16 alg_id                 -> 00 14
	//	opaque device_xwing_pub<V>    -> varint(1216) then the bytes
	//
	// 1216 is 0x04c0, which is above 63 and below 16384, so section 2.1.2's two octet form
	// applies: the top two bits of the first octet carry the log2 of the octet count, giving
	// 0x04|0x40 = 0x44 and then 0xc0.
	want := append([]byte{0x00, 0x14, 0x44, 0xc0}, pub...)
	if !bytes.Equal(ext.ExtensionData, want) {
		t.Fatalf("body is %d bytes beginning %x, want %d bytes beginning %x",
			len(ext.ExtensionData), ext.ExtensionData[:min(8, len(ext.ExtensionData))], len(want), want[:8])
	}
	out, err := ParseLeafKeysExtension(ext.ExtensionData)
	if err != nil {
		t.Fatalf("ParseLeafKeysExtension: %v", err)
	}
	if out.AlgId != AlgIdXwing {
		t.Fatalf("alg_id = %#x, want %#x", out.AlgId, AlgIdXwing)
	}
	if !bytes.Equal(out.DeviceXwingPub, pub) {
		t.Fatalf("device_xwing_pub mismatch")
	}
	again, err := out.Encode()
	if err != nil {
		t.Fatalf("re-Encode: %v", err)
	}
	if !bytes.Equal(again.ExtensionData, ext.ExtensionData) {
		t.Fatalf("re-encode differs")
	}
	if again.ExtensionType != ext.ExtensionType {
		t.Fatalf("re-encode tagged %#x, want %#x", again.ExtensionType, ext.ExtensionType)
	}
}

func TestLeafKeysExtensionRejectsWrongLength(t *testing.T) {
	short := &LeafKeysExtension{AlgId: AlgIdXwing, DeviceXwingPub: make([]byte, XwingPublicKeyLen-1)}
	if _, err := short.Encode(); !errors.Is(err, ErrLeafKeysExtensionInvalid) {
		t.Fatalf("Encode short key err = %v, want ErrLeafKeysExtensionInvalid", err)
	}
	good := &LeafKeysExtension{AlgId: AlgIdXwing, DeviceXwingPub: make([]byte, XwingPublicKeyLen)}
	ext, err := good.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	encoded := ext.ExtensionData
	if _, err := ParseLeafKeysExtension(encoded[:len(encoded)-1]); err == nil {
		t.Fatalf("ParseLeafKeysExtension(truncated) = nil error, want failure")
	}
	trailing := append(append([]byte{}, encoded...), 0x00)
	if _, err := ParseLeafKeysExtension(trailing); !errors.Is(err, syntax.ErrTrailingBytes) {
		t.Fatalf("ParseLeafKeysExtension(trailing) err = %v, want ErrTrailingBytes", err)
	}
	// the decode side's own length check, which the two cases above do not reach. A body
	// truncated by one byte is refused by the reader for declaring more than it carries, and
	// a body with a byte appended is refused for not being consumed in full; neither is a
	// body that decodes cleanly and then carries the wrong number of key octets, which is
	// what a peer running a different profile sends and what the check exists for.
	for _, wrong := range []int{XwingPublicKeyLen - 1, XwingPublicKeyLen + 1} {
		body := leafKeysBodyBytes(t, AlgIdXwing, make([]byte, wrong))
		if _, err := ParseLeafKeysExtension(body); !errors.Is(err, ErrLeafKeysExtensionInvalid) {
			t.Errorf("ParseLeafKeysExtension over a well formed body carrying %d key bytes err = %v, want ErrLeafKeysExtensionInvalid",
				wrong, err)
		}
	}
}

func TestLeafKeysExtensionRejectsUnimplementedAlg(t *testing.T) {
	// 0x0013 is reserved for hybrid X25519 + ML-KEM-1024 and is not implemented in
	// v1. MASTER section 7.1. it must be refused, not carried.
	in := &LeafKeysExtension{AlgId: 0x0013, DeviceXwingPub: make([]byte, XwingPublicKeyLen)}
	if _, err := in.Encode(); !errors.Is(err, ErrLeafKeysExtensionInvalid) {
		t.Fatalf("Encode alg 0x0013 err = %v, want ErrLeafKeysExtensionInvalid", err)
	}
	// and the same refusal on the decode side, which the encode side cannot reach: a peer
	// that implements 0x0013 sends this body, and a decoder without its own copy of the check
	// carries it into a LeafNode nothing in this profile can wrap a commit secret to.
	body := leafKeysBodyBytes(t, 0x0013, make([]byte, XwingPublicKeyLen))
	if _, err := ParseLeafKeysExtension(body); !errors.Is(err, ErrLeafKeysExtensionInvalid) {
		t.Fatalf("ParseLeafKeysExtension alg 0x0013 err = %v, want ErrLeafKeysExtensionInvalid", err)
	}
}

// TestLeafKeysExtensionAcceptsExactlyOneAlgIdOnBothSides sweeps the whole code point space
// rather than the two reserved values MASTER section 7.1 happens to name today.
//
// The class here is EVERY uint16, which is the one class over this field that cannot be
// understated. A test naming 0x0012 and 0x0013 says nothing about 0x0015, about a GREASE value
// a peer sends to keep the field exercised, or about the zero an uninitialised struct carries
// -- and the zero is the one a caller reaches by forgetting to set AlgId at all, which is the
// likeliest way this field is ever wrong.
//
// Both directions, because the two checks are separate lines of code and this project has
// twice shipped a refusal that existed on one side only.
func TestLeafKeysExtensionAcceptsExactlyOneAlgIdOnBothSides(t *testing.T) {
	pub := leafKeysTestKey()
	// one body, whose first two octets are the alg_id, patched in place rather than
	// reassembled 65536 times
	body := leafKeysBodyBytes(t, 0, pub)
	acceptedByEncode := []uint16{}
	acceptedByParse := []uint16{}
	for value := 0; value <= 0xffff; value++ {
		algId := uint16(value)
		in := &LeafKeysExtension{AlgId: algId, DeviceXwingPub: pub}
		if _, err := in.Encode(); err == nil {
			acceptedByEncode = append(acceptedByEncode, algId)
		} else if !errors.Is(err, ErrLeafKeysExtensionInvalid) {
			t.Fatalf("Encode alg %#04x err = %v, want ErrLeafKeysExtensionInvalid", algId, err)
		}
		body[0], body[1] = byte(algId>>8), byte(algId)
		out, err := ParseLeafKeysExtension(body)
		switch {
		case err == nil:
			acceptedByParse = append(acceptedByParse, algId)
			if out.AlgId != algId {
				t.Fatalf("ParseLeafKeysExtension accepted alg %#04x and reported %#04x", algId, out.AlgId)
			}
		case !errors.Is(err, ErrLeafKeysExtensionInvalid):
			t.Fatalf("ParseLeafKeysExtension alg %#04x err = %v, want ErrLeafKeysExtensionInvalid", algId, err)
		}
	}
	want := []uint16{AlgIdXwing}
	if !slices.Equal(acceptedByEncode, want) {
		t.Errorf("Encode accepted alg ids %#04x, want %#04x", acceptedByEncode, want)
	}
	if !slices.Equal(acceptedByParse, want) {
		t.Errorf("ParseLeafKeysExtension accepted alg ids %#04x, want %#04x", acceptedByParse, want)
	}
}

// leafKeysSweptLengths is the length class the test below is over: every length from nothing
// up to twice the X-Wing key, which covers the zero a caller reaches by forgetting the field,
// the off by ones either side, both halves of the hybrid on their own (32 and 1184), and the
// doubling a concatenation bug produces.
//
// A range rather than a list of interesting values, because the interesting values are the
// ones nobody thought of. It is the widest class this can be stated over that still runs in
// well under a second.
const leafKeysSweptLengths = 2 * XwingPublicKeyLen

// TestLeafKeysExtensionAcceptsExactlyOneKeyLengthOnBothSides says which length is accepted by
// naming the number independently, not by naming the constant the code under test reads.
//
// That is the whole difference between this and the length test the plan supplied. A test that
// builds its key as make([]byte, XwingPublicKeyLen) and expects that to be accepted passes
// under any value of the constant, so it cannot see the one mistake in this task that reaches
// the wire: a device key length this package and its peers disagree about.
func TestLeafKeysExtensionAcceptsExactlyOneKeyLengthOnBothSides(t *testing.T) {
	derived := xwingPublicKeyLenFromTheStandardLibrary(t)
	acceptedByEncode := []int{}
	acceptedByParse := []int{}
	for n := 0; n <= leafKeysSweptLengths; n++ {
		in := &LeafKeysExtension{AlgId: AlgIdXwing, DeviceXwingPub: make([]byte, n)}
		if _, err := in.Encode(); err == nil {
			acceptedByEncode = append(acceptedByEncode, n)
		} else if !errors.Is(err, ErrLeafKeysExtensionInvalid) {
			t.Fatalf("Encode over %d key bytes err = %v, want ErrLeafKeysExtensionInvalid", n, err)
		}
		body := leafKeysBodyBytes(t, AlgIdXwing, make([]byte, n))
		out, err := ParseLeafKeysExtension(body)
		switch {
		case err == nil:
			acceptedByParse = append(acceptedByParse, n)
			if len(out.DeviceXwingPub) != n {
				t.Fatalf("ParseLeafKeysExtension accepted %d key bytes and reported %d", n, len(out.DeviceXwingPub))
			}
		case !errors.Is(err, ErrLeafKeysExtensionInvalid):
			t.Fatalf("ParseLeafKeysExtension over %d key bytes err = %v, want ErrLeafKeysExtensionInvalid", n, err)
		}
	}
	want := []int{derived}
	if !slices.Equal(acceptedByEncode, want) {
		t.Errorf("Encode accepted key lengths %v, want %v", acceptedByEncode, want)
	}
	if !slices.Equal(acceptedByParse, want) {
		t.Errorf("ParseLeafKeysExtension accepted key lengths %v, want %v", acceptedByParse, want)
	}
}

// TestLeafKeysExtensionSharesNoStorageWithItsCallerOrItsInput holds the two aliasing claims
// the doc comment on LeafKeysExtension makes to wrap.go's reader.
//
// Both matter to that reader specifically. A DeviceXwingPub that viewed the leaf's own buffer
// would make a wrap target something the owner of that buffer can change after the leaf was
// validated and before the commit secret is wrapped to it, and the change would be invisible
// to every signature over the leaf because the leaf's bytes are what was signed. The encode
// direction is the same hazard read backwards: an Extension whose body aliased the caller's
// key would be a signed structure whose meaning changes after it was signed.
func TestLeafKeysExtensionSharesNoStorageWithItsCallerOrItsInput(t *testing.T) {
	pub := leafKeysTestKey()
	in := &LeafKeysExtension{AlgId: AlgIdXwing, DeviceXwingPub: pub}
	ext, err := in.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	held := bytes.Clone(ext.ExtensionData)
	for i := range pub {
		pub[i] ^= 0xff
	}
	if !bytes.Equal(ext.ExtensionData, held) {
		t.Errorf("mutating the caller's key changed an Extension already produced, so Encode kept a view of it")
	}
	for i := range pub {
		pub[i] ^= 0xff
	}

	body := bytes.Clone(ext.ExtensionData)
	out, err := ParseLeafKeysExtension(body)
	if err != nil {
		t.Fatalf("ParseLeafKeysExtension: %v", err)
	}
	parsed := bytes.Clone(out.DeviceXwingPub)
	for i := range body {
		body[i] ^= 0xff
	}
	if !bytes.Equal(out.DeviceXwingPub, parsed) {
		t.Errorf("mutating the body changed the parsed key, so ParseLeafKeysExtension returned a view of its input")
	}
}

// ---------------------------------------------------------------------------
// the sanctioned exception to C1, stated as the guarantee rather than as a spelling
// ---------------------------------------------------------------------------

// extensionBodyTypesIn derives the extension body class: the types of the scanned source that
// declare Encode() (Extension, error).
//
// Derived off that signature rather than off a name, a file or a table, because the signature
// IS the property. Extension.ExtensionData is opaque, so an extension body has to convert
// bytes to and from a struct somewhere; what makes this package's spelling of that safe is not
// the word Encode but the fact that what comes back is the whole Extension, tag and all, so
// there is no loose body for a call site to pair with the wrong type. A second extension body
// written to that signature is covered by having been written to it, and one written to any
// other signature -- Encode answering []byte, say -- is outside the class and is refused by
// TestNoTypeOfThisPackageCarriesAByteLevelCodecOfItsOwn as the second codec it is.
//
// The receiver is reported without its pointer star, since that is the spelling a type name is
// compared under everywhere else in these gates.
func extensionBodyTypesIn(files []parsedSource) []string {
	bodies := []string{}
	for _, parsed := range files {
		for _, one := range declaredIn(parsed) {
			if one.receiver == "" || one.name != "Encode" {
				continue
			}
			if !slices.Equal(one.results, []string{"Extension", "error"}) {
				continue
			}
			bodies = append(bodies, strings.TrimPrefix(one.receiver, "*"))
		}
	}
	slices.Sort(bodies)
	return slices.Compact(bodies)
}

// A file declaring one of each shape the rule below has to tell apart, so a matcher that
// stopped matching fails here rather than issuing this package a clean bill.
//
// The last two are the negative halves. SomeUnrelatedBytes is the shape this reading cannot
// see and the prose says so; leafKeysBodyBytesInternal is unexported and so outside the class
// on purpose, because the package taking its own body apart internally is not a call site that
// can pair it with a tag.
const extensionBodySurfaceControl = `package control

type LeafKeysExtension struct {
	AlgId          uint16
	DeviceXwingPub []byte
}

func (self *LeafKeysExtension) Encode() (Extension, error) { return Extension{}, nil }

func ParseLeafKeysExtension(data []byte) (*LeafKeysExtension, error) { return nil, nil }

// the shape the rule exists to report: the body handed back on its own, for a caller to pair
// with whatever tag it likes
func (self *LeafKeysExtension) Bytes() ([]byte, error) { return nil, nil }

func LeafKeysBodyOf(body *LeafKeysExtension) []byte { return nil }

// the same shape one level of slicing out, which is what the class used to stop short of
func (self *LeafKeysExtension) EveryBody() ([][]byte, error) { return nil, nil }

// exported, answers a byte run, and mentions no extension body at all
func SomeUnrelatedBytes(n int) []byte { return nil }

// unexported, so outside the class
func leafKeysBodyBytesInternal(body *LeafKeysExtension) []byte { return nil }
`

// What the rule must read out of the control, exactly rather than as a floor: a rule that
// widened to report ParseLeafKeysExtension would ban the sanctioned spelling, and one that
// narrowed to miss the method would issue this package the clean bill a working one issues.
var extensionBodySurfaceControlReports = []string{
	"(*LeafKeysExtension).Bytes",
	"(*LeafKeysExtension).EveryBody",
	"LeafKeysBodyOf",
}

// extensionBodyAnswersByteRuns is keyScheduleIsByteRun widened by the one step both rules
// below need: a result that is a SLICE of byte runs hands out byte runs too, and so does a
// slice of those, which is why this unwraps rather than spelling the two levels it has seen.
//
// The narrowing this replaced was measured and not supposed. (*RatchetTree).TreeHashes answers
// [][]byte, reaches the extension body encoder exactly as TreeHash and NodeTreeHash do -- all
// three go through marshalBytes -- and was outside the class of BOTH rules for the single
// reason that its result is a slice of runs rather than a run. What that cost is not the tree
// hash, which is exempt on its merits; it is that the exemption table could hold two of the
// three tree hash methods and its expiry check could never hold the third, so the table read as
// covering a class it was outside of. A rule whose class stops at one level of slicing is a
// rule a loose body is handed out through by answering two of them.
func extensionBodyAnswersByteRuns(rendered string, byteRuns []string) bool {
	for {
		if keyScheduleIsByteRun(rendered, byteRuns) {
			return true
		}
		if !strings.HasPrefix(rendered, "[]") {
			return false
		}
		rendered = strings.TrimPrefix(rendered, "[]")
	}
}

// exportedSymbolsHandingOutABodyIn is every exported declaration of one file that mentions an
// extension body type in its receiver or its parameters and answers a byte run.
//
// TWO honest limits, both measured rather than supposed, and both written here rather than
// left for the next reader to rediscover.
//
// The first is that this reads the SIGNATURE, so an exported function that assembles the same
// bytes without naming the type -- LeafKeysBody(algId uint16, pub []byte) []byte -- is
// invisible to it. That shape is left uncovered HERE rather than bought with a rule that
// reports every exported function in the package answering a byte slice, which is most of the
// crypto. It is covered next door instead, by
// TestNoExportedSymbolOfThisPackageAssemblesAnExtensionBodyThroughItsOwnEncoder, whose class is
// reachability of the encoder an extension body's Encode actually calls rather than the shape
// of a signature. The claim this comment used to carry -- that a body assembled that way has to
// duplicate the encoder and that duplicating the encoder is what
// TestNoTypeOfThisPackageCarriesAByteLevelCodecOfItsOwn reports -- was wrong, and was measured
// to be wrong: adding exactly that function left C1 green, because a free function answering a
// byte run is neither of the two shapes C1 reads.
//
// The second is that this reads FUNCTIONS AND METHODS, so a struct FIELD is outside the class
// altogether. Extension.ExtensionData is an exported field of an exported struct and hands out
// a body with no declaration for any signature rule to read; ext.ExtensionData is the shortest
// route to a loose body there is, and no derivation over declarations can ever report it.
// Nothing here can close that, because the codec needs those fields exported. What closes the
// harm it is a route to is on the read side: ParseLeafKeysFrom refuses a body arriving under
// another type's tag. This rule is the half that keeps the package from PRODUCING a loose body,
// and it is worth exactly that much and no more.
func exportedSymbolsHandingOutABodyIn(parsed parsedSource, bodies []string, byteRuns []string) []string {
	mentionsABody := func(rendered string) bool {
		return slices.ContainsFunc(bodies, func(one string) bool {
			return rendered == one || rendered == "*"+one || rendered == "[]"+one || rendered == "[]*"+one
		})
	}
	found := []string{}
	for _, one := range declaredIn(parsed) {
		if !one.exported {
			continue
		}
		reaches := mentionsABody(strings.TrimPrefix(one.receiver, "*")) || mentionsABody(one.receiver)
		for _, parameter := range one.params {
			reaches = reaches || mentionsABody(parameter)
		}
		if !reaches {
			continue
		}
		if !slices.ContainsFunc(one.results, func(result string) bool {
			return extensionBodyAnswersByteRuns(result, byteRuns)
		}) {
			continue
		}
		if one.receiver != "" {
			found = append(found, "("+one.receiver+")."+one.name)
			continue
		}
		found = append(found, one.name)
	}
	slices.Sort(found)
	return found
}

// extensionBodyByteRunsThatAreNotBodies is every exported declaration the two rules below
// report whose byte run is NOT an extension body, with the argument for each.
//
// An exemption table and not a filter, for the reason tree_kat_test.go's
// treeVectorFamiliesElsewhere gives: whether a byte run is an extension body is a judgement
// about that byte run, and neither rule can read it. One rule reads a SIGNATURE -- a
// declaration whose receiver is a type that has an Encode, answering bytes -- and the other
// reads a call graph -- a declaration that reaches the encoder those bodies are assembled
// with. A tree hash satisfies both and is neither: the preimage the encoder assembles is
// hashed and thrown away, and what leaves is Nh octets of digest.
//
// What makes an exemption table safe is that it is held in BOTH directions, which is three
// checks and not one. Each rule refuses a report with no entry here, so a genuine loose body
// added tomorrow fails rather than being waved through; and
// TestEveryExtensionBodyByteRunExemptionIsStillReported refuses an entry that neither rule
// reports any more, so an entry cannot outlive the declaration it excuses or the rule that
// made it necessary. This is the base name exemption this project keeps rediscovering, kept
// off by keying on the qualified name and by expiring on failure.
var extensionBodyByteRunsThatAreNotBodies = map[string]string{
	"(*Group).ProposeAdd":                    "answers a serialized MLSMessage carrying an RFC 9420 section 6.3 PrivateMessage -- a wire format header, an encrypted sender data blob and an AEAD ciphertext -- which is not an extension body, and no tag exists that would make one readable as any extension of this package: an entry pairing 0xF001 with these octets is one the first peer to read it refuses rather than one it misreads. It is reported for NewKeyPackage's reason, which is the rule reading a RESULT: the proposal it frames reaches the encoder because the leaf and the preimages inside it are assembled through it, and every one of those is signed or hashed and discarded inside the call. What this one hands back is the octets a peer receives, stores and replays, and the signature over it is this client's own",
	"(*Group).ProposeUpdate":                 "answers a serialized MLSMessage carrying an RFC 9420 section 6.3 PrivateMessage -- a wire format header, an encrypted sender data blob and an AEAD ciphertext -- which is not an extension body, and no tag exists that would make one readable as any extension of this package: an entry pairing 0xF001 with these octets is one the first peer to read it refuses rather than one it misreads. It is reported for NewKeyPackage's reason, which is the rule reading a RESULT: the proposal it frames reaches the encoder because the leaf and the preimages inside it are assembled through it, and every one of those is signed or hashed and discarded inside the call. The leaf it frames is the one place this method touches an extension at all, and it carries urmessage_leaf_keys re-encoded through that extension's own Encode -- which answers the tag and the body TOGETHER, and is the pairing this rule exists to keep out of a caller's hands",
	"(*Group).ProposeGroupContextExtensions": "answers a serialized MLSMessage carrying an RFC 9420 section 6.3 PrivateMessage -- a wire format header, an encrypted sender data blob and an AEAD ciphertext -- which is not an extension body, and no tag exists that would make one readable as any extension of this package: an entry pairing 0xF001 with these octets is one the first peer to read it refuses rather than one it misreads. It is reported for NewKeyPackage's reason, which is the rule reading a RESULT: the proposal it frames reaches the encoder because the leaf and the preimages inside it are assembled through it, and every one of those is signed or hashed and discarded inside the call. The extension set it frames is the CALLER'S, entry and tag alike, so no tag choice is handed anywhere: this method is given whole entries and hands back a sealed message, never a loose body somebody else has to pair with a type",
	"NewKeyPackage":                          "answers the two HPKE private halves of a fresh key package -- an init key and a leaf encryption key, one X25519 scalar each -- which are keys and not extension bodies, and no tag exists that would make a KEM scalar readable as any extension of this package. It reaches the encoder because the KeyPackageTBS it signs is assembled through marshalBytes, and that preimage is signed and discarded inside the call, exactly as the four tree hashes above hash and discard theirs. Unlike PskSecret's entry this is a REAL edge rather than a name collision: this constructor does reach the encoder, and what it answers is simply not a body",
	"NewKeyPackageWithSigner":                "answers the two HPKE private halves of a fresh key package -- an init key and a leaf encryption key, one X25519 scalar each -- which are keys and not extension bodies, and no tag exists that would make a KEM scalar readable as any extension of this package. It reaches the encoder for exactly NewKeyPackage's reason and by exactly NewKeyPackage's route: the KeyPackageTBS it signs is assembled through marshalBytes, and that preimage is signed and discarded inside the call. What it hands a caller that NewKeyPackage does not is a leaf bound to the CALLER'S signing key, which changes nothing about what the byte runs are: the two answers are still scalars and the extensions it is handed are whole entries, tag and body together, exactly as NewLeafNode takes them. The one extension this constructor touches -- urmessage_leaf_keys on the leaf -- arrives as a complete Extension from the caller and leaves inside a signed leaf, so no tag choice is handed anywhere. Like NewKeyPackage's entry and unlike PskSecret's this is a REAL edge rather than a name collision, and it expires with the declaration",
	"(*RatchetTree).NodeTreeHash":            "answers the RFC 9420 section 7.8 tree hash of one subtree, which is KDF.Nh octets of digest and not a ratchet_tree body; the TreeHashInput preimage the encoder assembles is hashed and discarded inside the call, and no tag exists that would make a bare digest readable as any extension of this package",
	"(*RatchetTree).TreeHash":                "answers the section 7.8 tree hash of the whole tree, which is what GroupContext.TreeHash is set from; the same argument as NodeTreeHash, and the signature is the one the key schedule and the group lifecycle plans compile against rather than one this package is free to change",
	"(*RatchetTree).TreeHashes":              "answers the section 7.8 tree hash of every node, which is the column the tree-validation corpus publishes and the one a parent hash check reads; the same argument as the two above, one level of slicing out -- KDF.Nh octets of digest per node, and a slice of digests is no more a body than one of them is",
	"(*RatchetTree).ParentHash":              "answers the RFC 9420 section 7.9 parent hash of one node, which is KDF.Nh octets of digest and not a ratchet_tree body; the ParentHashInput preimage the encoder assembles is hashed and discarded inside the call, and the value that leaves is the one a LeafNode.parent_hash field and a ParentNode.parent_hash field are compared against -- the same argument as the three tree hashes above, whose preimages this one is built out of",
	"(*GroupPolicyExtension).OwnerId":        "answers the OWNER'S MEMBER ID out of a group policy, which is an Ed25519 identity public key -- one FIELD read out of the structure -- and not a urmessage_group_policy body; no tag exists that would make an identity key readable as any extension of this package, and pairing it with 0xF001 produces an entry the first peer to read it refuses rather than one it misreads. It is reported for a reason the four tree hashes above share: the rule reads the RESULT, an exported method of a body type answering a byte run, and nothing a declaration scan can see says which of the type's fields the run came from. The signature is the interface registry's and this task does not own it",
	"PskSecret":                              "answers the RFC 9420 section 8.4 psk_secret, which is KDF.Nh octets of key schedule output and not an extension body; the PSKLabel preimage it assembles is built on a Writer marshalPskLabel opens for itself and never goes through the encoder this package's extension bodies are built with. It is in this table for a MECHANICAL reason rather than a judgement about what the bytes are, and that is the difference from the four above: it entered the closure on the commit that landed (*RatchetTree).Validate, because the closure is keyed by NAME and this package now declares three unrelated methods spelled Validate -- LeafNode's, PreSharedKeyId's and the ratchet tree's -- of which only the third reaches the encoder, while PskSecret calls the second. TestThePskSecretExemptionIsAReachThroughACollidingMethodNameAndNothingElse holds it to exactly that account, so the entry expires the moment the reach becomes a real one",
}

// extensionBodyByteRunsReportedByEitherRule is the union of what the two rules below report
// over this package, before the table above is subtracted.
//
// It exists so the expiry check reads the RULES rather than a third opinion about them: an
// entry stops being needed exactly when neither rule reports it, and that is a question only
// the rules can answer.
func extensionBodyByteRunsReportedByEitherRule(t *testing.T) []string {
	t.Helper()
	scanned := packageLevelFunctions(t).files
	files := []parsedSource{}
	for _, path := range scanned {
		files = append(files, mustParseSource(t, path))
	}
	bodies := extensionBodyTypesIn(files)
	helpers := extensionBodyEncoderHelpersIn(files, bodies)
	reaching := theNamesReachingTheExtensionBodyEncoder(declaredAcross(files), helpers)
	byteRuns := packageByteSliceTypeNames(t)
	reported := []string{}
	for at := range scanned {
		reported = append(reported, exportedSymbolsHandingOutABodyIn(files[at], bodies, byteRuns)...)
		reported = append(reported, exportedSymbolsAssemblingABodyIn(files[at], reaching, bodies, byteRuns)...)
	}
	slices.Sort(reported)
	return slices.Compact(reported)
}

// TestEveryExtensionBodyByteRunExemptionIsStillReported is the expiry half of the table above.
//
// An exemption that covers nothing is a hole with a name on it -- the same argument
// TestHkdfExtractHasOnlyTwoCallSites makes about its own allow list -- and the way this one
// stops covering something is either the declaration going away or a rule being narrowed until
// it no longer reports it. Both are changes somebody should have to notice, so both fail here.
func TestEveryExtensionBodyByteRunExemptionIsStillReported(t *testing.T) {
	reported := extensionBodyByteRunsReportedByEitherRule(t)
	if len(reported) == 0 {
		t.Fatal("neither rule reports anything at all over this package, so the table below is holding nothing and both gates are reporting clean having read nothing")
	}
	for name, why := range extensionBodyByteRunsThatAreNotBodies {
		if !slices.Contains(reported, name) {
			t.Errorf("%s is exempted as %q and neither rule reports it any more; delete the entry", name, why)
		}
	}
	t.Logf("the two rules report %v, of which %d are exempted as byte runs that are not bodies",
		reported, len(extensionBodyByteRunsThatAreNotBodies))
}

// TestNoExportedSymbolOfThisPackageHandsOutAnExtensionBodyOnItsOwn is ONE HALF of what the
// sanctioned exception is worth, stated as a rule rather than as a naming convention: this
// package does not hand out a loose extension body.
//
// It is not a guarantee that a body and a tag cannot come apart, and the doc on Encode no
// longer says it is. Extension carries two exported fields, so ext.ExtensionData is a field
// access and the mismatched pair is a three line composite literal built out of exported API;
// that route is outside every rule a declaration scan can state, and it is closed on the read
// side instead, by ParseLeafKeysFrom. What this rule is worth is the OTHER route: an exported
// (*LeafKeysExtension).Bytes added next to Encode for somebody's convenience puts the choice of
// tag back in the caller's hands on the ordinary path, and the wrong choice there -- 0xF001
// rather than 0xF002, one identifier apart in this file -- encodes, is covered by the leaf
// signature, and is refused by the first peer that tries to read a group policy out of an
// X-Wing key. Nothing about that failure points back at the call site that made it.
//
// The class is derived from the Encode signature, so a second extension body type is covered
// by the commit that adds it.
func TestNoExportedSymbolOfThisPackageHandsOutAnExtensionBodyOnItsOwn(t *testing.T) {
	control := mustParseText(t, "the extension body surface control", extensionBodySurfaceControl)
	controlBodies := extensionBodyTypesIn([]parsedSource{control})
	if !slices.Equal(controlBodies, []string{"LeafKeysExtension"}) {
		t.Fatalf("the derivation read %v out of the control, want [LeafKeysExtension]; a derivation that reads nothing exempts nothing and demands nothing",
			controlBodies)
	}
	if reported := exportedSymbolsHandingOutABodyIn(control, controlBodies,
		packageByteSliceTypeNamesIn(control)); !slices.Equal(reported, extensionBodySurfaceControlReports) {
		t.Fatalf("the rule reported %v out of the control, want %v", reported, extensionBodySurfaceControlReports)
	}

	scanned := packageLevelFunctions(t).files
	files := []parsedSource{}
	for _, path := range scanned {
		files = append(files, mustParseSource(t, path))
	}
	bodies := extensionBodyTypesIn(files)
	if !slices.Contains(bodies, "LeafKeysExtension") {
		t.Fatalf("the derivation read %v out of %v and LeafKeysExtension is not among them, so this gate is over a class that does not include the one extension body this package has",
			bodies, scanned)
	}
	t.Logf("the extension bodies of this package, by the Encode signature they declare: %v", bodies)
	byteRuns := packageByteSliceTypeNames(t)
	for at, path := range scanned {
		for _, handed := range exportedSymbolsHandingOutABodyIn(files[at], bodies, byteRuns) {
			if _, isExcused := extensionBodyByteRunsThatAreNotBodies[handed]; isExcused {
				continue
			}
			t.Errorf("%s exports %s, which hands an extension body out as bytes; Encode answers the whole Extension so that no call site can pair a body with another extension's type, and a byte run out of this package is exactly that pairing waiting to be made",
				path, handed)
		}
	}
}

// The tag each extension body of this package stamps, a value of it to ask, the tag checked
// entry point that reads one back, and the sentinel that entry point refuses with.
//
// A table, and the derived class above is what stops it being the enumeration this project has
// been walked past fourteen times: the two tests below both require the table and the derived
// class to be EQUAL, so an extension body added without an entry here fails rather than going
// unchecked, and an entry left here for a body that no longer exists fails too.
//
// readBack is written as a call into this package's exported surface rather than as a tag
// comparison spelled here, because a comparison spelled here would pass while the package
// shipped no tag checked entry point at all.
// TestEveryExtensionBodyDeclaresATagCheckedReadSideBesideItsEncode is what says the entry point
// exists; this row is what says it works.
var extensionBodyTagsToStamp = map[string]struct {
	tag      ExtensionType
	build    func() (Extension, error)
	readBack func(Extension) error
	refusal  error
}{
	"LeafKeysExtension": {
		tag: ExtensionTypeUrmessageLeafKeys,
		build: func() (Extension, error) {
			body := &LeafKeysExtension{AlgId: AlgIdXwing, DeviceXwingPub: make([]byte, XwingPublicKeyLen)}
			return body.Encode()
		},
		readBack: func(ext Extension) error {
			_, err := ParseLeafKeysFrom(ext)
			return err
		},
		refusal: ErrLeafKeysExtensionInvalid,
	},
	// the urmessage_group_policy body of MASTER section 6. Encode validates before it stamps,
	// so the value built here has to be one a group could actually run: exactly one owner, a non
	// empty member id, and a canonical role list. One entry rather than several, because what
	// the two tests below ask about is the TAG and a longer role list costs 65536 read backs of
	// it.
	//
	// The refusal is ErrMalformedExtension, which is also what this body's content refusals
	// carry, and that is deliberate rather than a shortcut: a caller here is asking one question
	// -- is there a group policy in this entry that I can act on -- and an entry of the wrong
	// type answers it exactly as a role byte this profile cannot render does. The sweep below is
	// what says nothing but 0xF001 is ACCEPTED, which is the half a shared sentinel cannot blur.
	"GroupPolicyExtension": {
		tag: ExtensionTypeUrmessageGroupPolicy,
		build: func() (Extension, error) {
			policy := &GroupPolicyExtension{
				Roles: []RoleEntry{{MemberId: []byte("the owner"), Role: RoleOwner}},
			}
			return policy.Encode()
		},
		readBack: func(ext Extension) error {
			_, err := ParseGroupPolicyFrom(ext)
			return err
		},
		refusal: ErrMalformedExtension,
	},
	// the urmessage_owner_successor body of MASTER section 11. Encode validates before it
	// stamps, so the value built here has to be one a group could actually run: a floor no
	// shorter than the ninety day minimum.
	//
	// The refusal is ErrMalformedExtension, for the group policy row's reason -- a caller here is
	// asking whether there is a nomination in this entry it can act on, and an entry of the wrong
	// type answers that exactly as a floor this profile refuses does. What makes the shared
	// sentinel safe is the sweep below, which says nothing but 0xF003 is ACCEPTED, and the tag it
	// has to be told apart from is 0xF001 -- two identifiers away in the same const block, and
	// carrying an opaque<V> at an offset a nomination body also has one at, so a nomination read
	// back as a group policy is a decode that can succeed.
	"OwnerSuccessorExtension": {
		tag: ExtensionTypeUrmessageOwnerSuccessor,
		build: func() (Extension, error) {
			nomination := &OwnerSuccessorExtension{
				Enabled:           true,
				SuccessorMemberId: []byte("the successor"),
				NominatedAtMs:     1770000000000,
				FloorMs:           SuccessionFloorMinMs,
			}
			return nomination.Encode()
		},
		readBack: func(ext Extension) error {
			_, err := ParseOwnerSuccessorFrom(ext)
			return err
		},
		refusal: ErrMalformedExtension,
	},
	// the ratchet_tree body of RFC 9420 section 12.4.3.3, whose Encode is the raised limit
	// encode with the tag stamped on. One occupied leaf and nothing else, because what the
	// two tests below ask about is the TAG: a wider tree costs 65536 read backs each.
	//
	// The refusal is ErrRatchetTreeExtensionTag and not the tree's own structural sentinels,
	// which is the one difference from the row above and is deliberate: the tree body has four
	// refusals of its own that a caller repairs differently, so only the wrong TAG answers
	// here. The sweep below is what says nothing else does.
	"RatchetTree": {
		tag: ExtensionTypeRatchetTree,
		build: func() (Extension, error) {
			tree := NewRatchetTree()
			if err := tree.SetLeaf(LeafIndex(0), &LeafNode{Credential: BasicCredential([]byte("a")), LeafNodeSource: LeafNodeSourceUpdate}); err != nil {
				return Extension{}, err
			}
			return tree.Encode()
		},
		readBack: func(ext Extension) error {
			_, err := ParseRatchetTreeFrom(ext)
			return err
		},
		refusal: ErrRatchetTreeExtensionTag,
	},
}

// TestEveryExtensionBodyEncodeStampsTheTagOfItsOwnType is the behavioural half of the same
// guarantee: the tag that comes back is this body's own, and is not any other extension type
// this package declares.
//
// The second half is not redundant with the first. ExtensionTypeUrmessageGroupPolicy and
// ExtensionTypeUrmessageOwnerSuccessor are declared eleven lines from the one this stamps and
// differ from it by one digit, so "the tag is 0xF002" and "the tag is not one of its
// neighbours" fail on different edits, and the second is the one an eye reading a diff misses.
func TestEveryExtensionBodyEncodeStampsTheTagOfItsOwnType(t *testing.T) {
	files := []parsedSource{}
	for _, path := range packageLevelFunctions(t).files {
		files = append(files, mustParseSource(t, path))
	}
	bodies := extensionBodyTypesIn(files)
	if covered := slices.Sorted(maps.Keys(extensionBodyTagsToStamp)); !slices.Equal(covered, bodies) {
		t.Fatalf("this package declares extension bodies %v and this table covers %v; an extension body with no entry is one nothing holds to its own tag",
			bodies, covered)
	}
	declared := everyExtensionTypeThisPackageDeclares(t)
	if len(declared) < 2 {
		t.Fatalf("this package declares %v extension types, so the neighbour half of this gate compares against nothing", declared)
	}
	for _, name := range bodies {
		entry := extensionBodyTagsToStamp[name]
		ext, err := entry.build()
		if err != nil {
			t.Errorf("%s.Encode over a valid body: %v", name, err)
			continue
		}
		if ext.ExtensionType != entry.tag {
			t.Errorf("%s.Encode tagged %#04x, want %#04x", name, uint16(ext.ExtensionType), uint16(entry.tag))
		}
		for _, other := range declared {
			if other != entry.tag && ext.ExtensionType == other {
				t.Errorf("%s.Encode tagged %#04x, which is another extension type this package declares", name, uint16(other))
			}
		}
	}
}

// Every ExtensionType code point this package's non test source declares, read off the const
// declarations rather than listed, so the neighbour comparison above grows with the registry.
func everyExtensionTypeThisPackageDeclares(t *testing.T) []ExtensionType {
	t.Helper()
	found := []ExtensionType{}
	for _, path := range packageLevelFunctions(t).files {
		parsed := mustParseSource(t, path)
		ast.Inspect(parsed.file, func(node ast.Node) bool {
			declaration, isDeclaration := node.(*ast.GenDecl)
			if !isDeclaration || declaration.Tok != token.CONST {
				return true
			}
			for _, specification := range declaration.Specs {
				valued, isValued := specification.(*ast.ValueSpec)
				if !isValued || valued.Type == nil || parsed.render(valued.Type) != "ExtensionType" {
					continue
				}
				for _, value := range valued.Values {
					literal, isLiteral := value.(*ast.BasicLit)
					if !isLiteral {
						continue
					}
					parsedValue, err := strconv.ParseUint(literal.Value, 0, 16)
					if err != nil {
						t.Fatalf("%s declares an ExtensionType of %s, which is not a uint16 literal", path, literal.Value)
					}
					found = append(found, ExtensionType(parsedValue))
				}
			}
			return true
		})
	}
	slices.Sort(found)
	return slices.Compact(found)
}

// ---------------------------------------------------------------------------
// the read side of the sanctioned exception: the entry point that is given the tag
// ---------------------------------------------------------------------------

// extensionBodyTagCheckedParsersIn is the read side counterpart of extensionBodyTypesIn: for
// each derived extension body, every exported package level function that is handed a whole
// Extension and answers that body.
//
// Being handed the Extension rather than the body's bytes is the whole property.
// ParseLeafKeysExtension takes bytes, so it is never told what tag they arrived under and
// cannot refuse a wrong one -- a body lifted out of a urmessage_group_policy entry parses there
// exactly as cleanly as one lifted out of its own, and answers a wrap target either way. The
// SIGNATURE is what separates the two, so the signature is what this reads: the body is taken
// off the result and the name after "Parse" is left free, because what sanctions this shape is
// that the tag is in its hands and not what somebody called it.
func extensionBodyTagCheckedParsersIn(files []parsedSource, bodies []string) map[string][]string {
	found := map[string][]string{}
	for _, parsed := range files {
		for _, one := range declaredIn(parsed) {
			if one.receiver != "" || !one.exported {
				continue
			}
			if !slices.Equal(one.params, []string{"Extension"}) {
				continue
			}
			if len(one.results) != 2 || one.results[1] != "error" || !strings.HasPrefix(one.results[0], "*") {
				continue
			}
			body := strings.TrimPrefix(one.results[0], "*")
			if !slices.Contains(bodies, body) {
				continue
			}
			found[body] = append(found[body], one.name)
		}
	}
	for body := range found {
		slices.Sort(found[body])
	}
	return found
}

// A file declaring one of each shape the read side rule has to tell apart, so a matcher that
// stopped matching fails here rather than issuing this package a clean bill.
//
// Three extension bodies and one tag checked parser between them. The two that go without are
// the near misses: the bytes taking half of the pair, which is a real and sanctioned entry
// point but has no tag to check and so does not stand in for this one, and the same shape as
// the real thing answering something that is not an extension body. The unexported twin is
// there because the surface this is about is the one a caller outside the package can reach
// for.
const extensionBodyReadSideControl = `package control

type LeafKeysExtension struct{}

func (self *LeafKeysExtension) Encode() (Extension, error) { return Extension{}, nil }

// the shape the rule is looking for: handed the whole entry, so the tag is in its hands
func ParseLeafKeysFrom(ext Extension) (*LeafKeysExtension, error) { return nil, nil }

type GroupPolicyExtension struct{}

func (self *GroupPolicyExtension) Encode() (Extension, error) { return Extension{}, nil }

// the first near miss: handed the body's bytes, so there is no tag for it to check
func ParseGroupPolicyExtension(data []byte) (*GroupPolicyExtension, error) { return nil, nil }

type OwnerSuccessorExtension struct{}

func (self *OwnerSuccessorExtension) Encode() (Extension, error) { return Extension{}, nil }

// the second near miss: handed the whole entry and answering something that is not the body
func ParseOwnerSuccessorFrom(ext Extension) (*Extension, error) { return nil, nil }

// unexported, so outside the surface a caller reaches for
func parseOwnerSuccessorFrom(ext Extension) (*OwnerSuccessorExtension, error) { return nil, nil }
`

// What the rule must read out of the control, exactly rather than as a floor: a rule that
// widened to accept the bytes taking half would report this package compliant while the tag was
// checked by nothing, and one that narrowed to miss the real shape would demand a parser that
// exists.
var extensionBodyReadSideControlReports = map[string][]string{
	"LeafKeysExtension": {"ParseLeafKeysFrom"},
}

// TestEveryExtensionBodyDeclaresATagCheckedReadSideBesideItsEncode is the half of the tag
// pairing that Encode cannot give on its own.
//
// Encode stamps the tag. Nothing about a Go return type stops a caller taking the body back out
// of the Extension -- ExtensionData is an exported field of an exported struct -- and pairing it
// with 0xF001, and nothing on the read side objects either, because ParseLeafKeysExtension is
// handed bytes and has no tag to compare. So the encode side guarantee has to have a read side
// counterpart or it is a statement about tidiness rather than about safety, and this is what
// says the counterpart is there.
//
// The class is derived from the Encode signature, the same derivation the encode side gate uses,
// so a second extension body type is owed a tag checked entry point by the commit that adds it.
func TestEveryExtensionBodyDeclaresATagCheckedReadSideBesideItsEncode(t *testing.T) {
	control := mustParseText(t, "the extension body read side control", extensionBodyReadSideControl)
	controlBodies := extensionBodyTypesIn([]parsedSource{control})
	wantBodies := []string{"GroupPolicyExtension", "LeafKeysExtension", "OwnerSuccessorExtension"}
	if !slices.Equal(controlBodies, wantBodies) {
		t.Fatalf("the derivation read %v out of the control, want %v; a derivation that reads nothing demands nothing",
			controlBodies, wantBodies)
	}
	reported := extensionBodyTagCheckedParsersIn([]parsedSource{control}, controlBodies)
	sameNames := func(left []string, right []string) bool { return slices.Equal(left, right) }
	if !maps.EqualFunc(reported, extensionBodyReadSideControlReports, sameNames) {
		t.Fatalf("the rule read %v out of the control, want %v", reported, extensionBodyReadSideControlReports)
	}

	scanned := packageLevelFunctions(t).files
	files := []parsedSource{}
	for _, path := range scanned {
		files = append(files, mustParseSource(t, path))
	}
	bodies := extensionBodyTypesIn(files)
	if !slices.Contains(bodies, "LeafKeysExtension") {
		t.Fatalf("the derivation read %v out of %v and LeafKeysExtension is not among them, so this gate is over a class that does not include the one extension body this package has",
			bodies, scanned)
	}
	parsers := extensionBodyTagCheckedParsersIn(files, bodies)
	for _, name := range bodies {
		if len(parsers[name]) == 0 {
			t.Errorf("%s declares Encode() (Extension, error) and this package exports nothing that takes an Extension and answers a *%s, so the tag Encode stamps is checked by nothing on the way back in: a body lifted out of one entry and read back through another parses clean and answers a wrap target",
				name, name)
			continue
		}
		t.Logf("%s is read back through %v", name, parsers[name])
	}
}

// TestEveryExtensionBodyRefusesAnEntryCarryingAnyTagButItsOwn is the behavioural half: the
// entry point the gate above requires actually refuses every tag but this body's.
//
// Over the whole uint16 space rather than over the two neighbours this file declares eleven
// lines apart. The neighbours are the likeliest mistake and they are covered by being in the
// space, but they are not the class: a tag a peer GREASEs, a code point registered after this
// was written, and the zero an uninitialised Extension carries are all bodies read back under
// something that is not urmessage_leaf_keys, and the zero is the one a caller reaches by
// building the Extension by hand and forgetting the field.
func TestEveryExtensionBodyRefusesAnEntryCarryingAnyTagButItsOwn(t *testing.T) {
	files := []parsedSource{}
	for _, path := range packageLevelFunctions(t).files {
		files = append(files, mustParseSource(t, path))
	}
	bodies := extensionBodyTypesIn(files)
	if covered := slices.Sorted(maps.Keys(extensionBodyTagsToStamp)); !slices.Equal(covered, bodies) {
		t.Fatalf("this package declares extension bodies %v and this table covers %v; an extension body with no entry is one nothing holds to its own tag",
			bodies, covered)
	}
	for _, name := range bodies {
		entry := extensionBodyTagsToStamp[name]
		if entry.readBack == nil || entry.refusal == nil {
			t.Errorf("%s has no tag checked read side in extensionBodyTagsToStamp, so nothing here says its body cannot be read back under another extension type's tag",
				name)
			continue
		}
		ext, err := entry.build()
		if err != nil {
			t.Errorf("%s.Encode over a valid body: %v", name, err)
			continue
		}
		accepted := []ExtensionType{}
		for value := 0; value <= 0xffff; value++ {
			tag := ExtensionType(value)
			err := entry.readBack(Extension{ExtensionType: tag, ExtensionData: ext.ExtensionData})
			switch {
			case err == nil:
				accepted = append(accepted, tag)
			case !errors.Is(err, entry.refusal):
				t.Fatalf("%s read back under tag %#04x err = %v, want %v", name, uint16(tag), err, entry.refusal)
			}
		}
		if want := []ExtensionType{entry.tag}; !slices.Equal(accepted, want) {
			t.Errorf("%s is read back under tags %#04x, want %#04x", name, accepted, want)
		}
	}
}

// ---------------------------------------------------------------------------
// the blind spot the signature rule leaves, closed by reachability instead
// ---------------------------------------------------------------------------

// extensionBodyEncoderHelpersIn is the seed of the reachability rule below: the package level
// functions of the scanned source that an extension body's Encode calls.
//
// Derived off the Encode bodies rather than written down, because "the encoder an extension
// body goes through" is a fact about this package that changes when the package does, and a
// name typed out here would go on naming the old one. Today that reads marshalBytes and nothing
// else; the point is that it will read whatever Encode calls tomorrow.
//
// Package level functions only. A method name would drag every receiver that declares one into
// the closure, and what this is trying to follow is the free encoder a free function can reach.
func extensionBodyEncoderHelpersIn(files []parsedSource, bodies []string) []string {
	packageLevel := map[string]bool{}
	for _, one := range declaredAcross(files) {
		if one.receiver == "" {
			packageLevel[one.name] = true
		}
	}
	helpers := []string{}
	for _, one := range declaredAcross(files) {
		if one.name != "Encode" || !slices.Contains(bodies, strings.TrimPrefix(one.receiver, "*")) {
			continue
		}
		if one.body == nil {
			continue
		}
		ast.Inspect(one.body, func(node ast.Node) bool {
			if identifier, isIdentifier := node.(*ast.Ident); isIdentifier && packageLevel[identifier.Name] {
				helpers = append(helpers, identifier.Name)
			}
			return true
		})
	}
	slices.Sort(helpers)
	return slices.Compact(helpers)
}

// theNamesInvokingTheStorage is theNamesReachingTheStorage's twin for a seed that is a
// FUNCTION rather than a field, and the difference between the two is the whole reason there
// are two.
//
// theNamesReachingTheStorage asks whether a declaration MENTIONS the name, which is the right
// question about a field: reading epochSecret is the entire risk that gate is about, and a
// read is a mention and not a call. Asking that question about a function name answers yes to
// every declaration that happens to spell it, whatever it spells it for -- and the collision
// is not hypothetical. (*RatchetTree).TreeHash is a method of this package and TreeHash is a
// FIELD of GroupContext, so with the mention reading, (*GroupContext).MarshalMLS reached the
// extension body encoder by writing self.TreeHash, every declaration that marshals anything
// reached it through that, and the rule below reported thirty exported symbols including
// SignWithLabel and PskSecret. A rule that reports thirty is a rule this package would learn
// to ignore, which the doc on exportedSymbolsAssemblingABodyIn already says about a seed that
// is too wide; the same is true of a closure that is.
//
// So this one asks whether a declaration INVOKES the name -- calls it, or names it as a value
// -- which is namesInvokedBy, the matcher key_schedule_kat_test.go's disjointness gate is
// built on and which carries its own control. A function is reached by being called or by
// being passed; a field of another type that happens to share its spelling is neither.
func theNamesInvokingTheStorage(declared []sourceDeclaration, storage string) []string {
	reaching := map[string]bool{storage: true}
	for {
		grew := false
		for _, one := range declared {
			if one.body == nil || reaching[one.name] {
				continue
			}
			for name := range namesInvokedBy(one.body) {
				if reaching[name] {
					reaching[one.name] = true
					grew = true
					break
				}
			}
		}
		if !grew {
			delete(reaching, storage)
			return slices.Sorted(maps.Keys(reaching))
		}
	}
}

// theNamesReachingTheExtensionBodyEncoder is every declared name that can invoke one of those
// helpers, plus the helpers themselves.
//
// It answers for one seed at a time and reachability is a union over seeds, so the loop is
// sound: every name in the closure of a set is reachable through a chain ending at exactly one
// member of it.
func theNamesReachingTheExtensionBodyEncoder(declared []sourceDeclaration, helpers []string) []string {
	reaching := slices.Clone(helpers)
	for _, helper := range helpers {
		reaching = append(reaching, theNamesInvokingTheStorage(declared, helper)...)
	}
	slices.Sort(reaching)
	return slices.Compact(reaching)
}

// exportedSymbolsAssemblingABodyIn is every exported declaration of one file that can reach the
// extension body encoder and answers a byte run.
//
// This is the shape the signature rule next door cannot see: LeafKeysBody(algId uint16, pub
// []byte) []byte names no extension body anywhere in its signature, so nothing about its types
// says what those bytes are. What says it is that they came out of the same encoder the
// sanctioned Encode goes through, and reaching that encoder is a property of the BODY rather
// than of the signature.
//
// The sanctioned Encode is excluded by name and by class rather than by file, because it is the
// one declaration that must reach the encoder -- reaching it is what it is for -- and what it
// answers is the tag and the body together rather than a byte run.
//
// The limit that remains, stated rather than left: a body assembled by taking a syntax Writer
// directly, without going through the helper Encode goes through, is not in this class. Seeding
// the closure with syntax.NewWriter instead of with what Encode calls was MEASURED, and reports
// twenty declarations -- (*KeySchedule).Export, (*HpkeContext).Export, PskSecret,
// InterimTranscriptHash, RefHash, MakeProposalRef, WelcomeKeyNonce and thirteen more -- every
// one of them a key schedule output or a wire preimage that is not an extension body. That is a
// rule this package would learn to ignore, which is worse than a rule with a stated hole. What
// reports the uncovered shape today is TestEveryConstructionInThisPackageLeavesItsInputAlone,
// whose coverage table is derived from every package level function handed a caller's bytes and
// demands a row for each: an assembler of a body is handed the device key, so it needs a row
// and does not have one.
func exportedSymbolsAssemblingABodyIn(parsed parsedSource, reaching []string, bodies []string, byteRuns []string) []string {
	found := []string{}
	for _, one := range declaredIn(parsed) {
		if !one.exported || !slices.Contains(reaching, one.name) {
			continue
		}
		if one.name == "Encode" && slices.Contains(bodies, strings.TrimPrefix(one.receiver, "*")) {
			continue
		}
		if !slices.ContainsFunc(one.results, func(result string) bool {
			return extensionBodyAnswersByteRuns(result, byteRuns)
		}) {
			continue
		}
		if one.receiver != "" {
			found = append(found, "("+one.receiver+")."+one.name)
			continue
		}
		found = append(found, one.name)
	}
	slices.Sort(found)
	return found
}

// A file declaring one of each shape the reachability rule has to tell apart.
//
// The indirect one is why this is a closure and not a check of Encode's own callers: a
// convenience wrapper one hop away from the encoder hands out the same bytes and mentions
// nothing this rule seeds on. The unexported pair are outside the class on purpose -- the
// package assembling its own body is not a call site that can pair it with a tag -- and the
// unrelated one is the negative half that keeps this from being "every exported function
// answering a byte slice".
const extensionBodyAssemblyControl = `package control

type LeafKeysExtension struct {
	AlgId          uint16
	DeviceXwingPub []byte
}

// the sanctioned encoder, which is what the seed is read out of and which must not be reported
func (self *LeafKeysExtension) Encode() (Extension, error) {
	body, err := marshalBytes(func(w *syntax.Writer) error { return nil })
	if err != nil {
		return Extension{}, err
	}
	return Extension{ExtensionData: body}, nil
}

func marshalBytes(encode func(w *syntax.Writer) error) ([]byte, error) { return nil, nil }

// the shape this rule exists to report: the body assembled through the same encoder and handed
// back loose, under a name and a signature that mention no extension body at all
func LeafKeysBody(algId uint16, pub []byte) []byte {
	body, _ := marshalBytes(func(w *syntax.Writer) error { return nil })
	return body
}

// and the same one hop further out, which is what makes this a closure
func LeafKeysBodyIndirect(algId uint16, pub []byte) []byte { return leafKeysBodyHelper() }

func leafKeysBodyHelper() []byte {
	body, _ := marshalBytes(func(w *syntax.Writer) error { return nil })
	return body
}

// and the same one level of slicing out, assembled through the same encoder
func LeafKeysBodies(count int) [][]byte {
	body, _ := marshalBytes(func(w *syntax.Writer) error { return nil })
	return [][]byte{body}
}

// unexported, so outside the class
func leafKeysBodyInternal() []byte {
	body, _ := marshalBytes(func(w *syntax.Writer) error { return nil })
	return body
}

// exported, answers a byte run, and reaches the encoder through nothing
func SomeUnrelatedBytes(n int) []byte { return make([]byte, n) }
`

// What the rule must read out of the control, exactly.
var extensionBodyAssemblyControlReports = []string{
	"LeafKeysBodies",
	"LeafKeysBody",
	"LeafKeysBodyIndirect",
}

// TestNoExportedSymbolOfThisPackageAssemblesAnExtensionBodyThroughItsOwnEncoder closes the
// blind spot the signature rule admits to and used to name the wrong catcher for.
//
// The doc on exportedSymbolsHandingOutABodyIn said the uncovered shape -- an exported free
// function assembling the body without naming the type -- was closed in practice because
// duplicating the encoder is what TestNoTypeOfThisPackageCarriesAByteLevelCodecOfItsOwn
// reports. It is not: adding exactly that function was measured to leave C1 green, because a
// free function answering a byte run is neither of the two shapes C1 reads. C1's encoder shape
// wants a receiver on an MLS structure and its decoder shape wants a single byte run in and a
// structure out, and LeafKeysBody(algId uint16, pub []byte) []byte is neither.
//
// So the shape is covered here instead, by what it actually has in common with the sanctioned
// encoder: it goes through the encoder. That is a property of the body rather than of the
// signature, which is exactly the half the rule next door cannot read.
func TestNoExportedSymbolOfThisPackageAssemblesAnExtensionBodyThroughItsOwnEncoder(t *testing.T) {
	control := mustParseText(t, "the extension body assembly control", extensionBodyAssemblyControl)
	controlFiles := []parsedSource{control}
	controlBodies := extensionBodyTypesIn(controlFiles)
	if !slices.Equal(controlBodies, []string{"LeafKeysExtension"}) {
		t.Fatalf("the derivation read %v out of the control, want [LeafKeysExtension]", controlBodies)
	}
	controlHelpers := extensionBodyEncoderHelpersIn(controlFiles, controlBodies)
	if !slices.Equal(controlHelpers, []string{"marshalBytes"}) {
		t.Fatalf("the seed read %v out of the control, want [marshalBytes]; a seed that reads nothing leaves the closure empty and the rule demands nothing",
			controlHelpers)
	}
	controlReaching := theNamesReachingTheExtensionBodyEncoder(declaredAcross(controlFiles), controlHelpers)
	if reported := exportedSymbolsAssemblingABodyIn(control, controlReaching, controlBodies,
		packageByteSliceTypeNamesIn(control)); !slices.Equal(reported, extensionBodyAssemblyControlReports) {
		t.Fatalf("the rule reported %v out of the control, want %v; the closure reached %v",
			reported, extensionBodyAssemblyControlReports, controlReaching)
	}

	scanned := packageLevelFunctions(t).files
	files := []parsedSource{}
	for _, path := range scanned {
		files = append(files, mustParseSource(t, path))
	}
	bodies := extensionBodyTypesIn(files)
	if !slices.Contains(bodies, "LeafKeysExtension") {
		t.Fatalf("the derivation read %v out of %v and LeafKeysExtension is not among them, so this gate is over a class that does not include the one extension body this package has",
			bodies, scanned)
	}
	helpers := extensionBodyEncoderHelpersIn(files, bodies)
	if len(helpers) == 0 {
		t.Fatalf("no extension body's Encode in %v calls a package level function of this package, so the closure below is empty and this gate demands nothing",
			scanned)
	}
	reaching := theNamesReachingTheExtensionBodyEncoder(declaredAcross(files), helpers)
	t.Logf("the extension body encoder of this package is %v, and %v reach it", helpers, reaching)
	byteRuns := packageByteSliceTypeNames(t)
	for at, path := range scanned {
		for _, handed := range exportedSymbolsAssemblingABodyIn(files[at], reaching, bodies, byteRuns) {
			if _, isExcused := extensionBodyByteRunsThatAreNotBodies[handed]; isExcused {
				continue
			}
			t.Errorf("%s exports %s, which reaches %v -- the encoder this package's extension bodies are built with -- and answers a byte run; an extension body handed out loose is a tag choice handed to the caller, and 0xF001 rather than 0xF002 encodes, signs and travels",
				path, handed, helpers)
		}
	}
}

// TestAWrapTargetReadOffALeafSharesNoStorageWithTheLeafsBytes holds the aliasing claim over the
// whole path the doc on LeafKeysExtension sends wrap.go's reader down, and not over the parse
// alone.
//
// The parse's own two claims are held next door, over a body handed to it directly. This is the
// same property stated where the reader will actually meet it: an extensions vector decoded off
// the wire, the lookup in front of the parse, and then a mutation of the leaf's own bytes. The
// lookup is the step that is easy to read as safe and is not -- FindExtension answers a VIEW --
// so a reader who kept the []byte instead of the parsed structure would be holding a wrap
// target the owner of the leaf can still change, invisibly to every signature over it.
func TestAWrapTargetReadOffALeafSharesNoStorageWithTheLeafsBytes(t *testing.T) {
	pub := leafKeysTestKey()
	in := &LeafKeysExtension{AlgId: AlgIdXwing, DeviceXwingPub: pub}
	ext, err := in.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// the leaf's extensions vector, written and read back, so what the lookup runs over is a
	// decoded vector rather than the one this test built
	w := syntax.NewWriter()
	if err := WriteExtensions(w, []Extension{ext}); err != nil {
		t.Fatalf("WriteExtensions: %v", err)
	}
	wire, err := w.Bytes()
	if err != nil {
		t.Fatalf("the extensions vector would not encode: %v", err)
	}
	exts, err := ReadExtensions(syntax.NewReader(wire))
	if err != nil {
		t.Fatalf("ReadExtensions: %v", err)
	}
	if len(exts) != 1 {
		t.Fatalf("the vector decoded to %d entries, want 1", len(exts))
	}

	found, ok, err := FindExtension(exts, ExtensionTypeUrmessageLeafKeys)
	if err != nil || !ok {
		t.Fatalf("FindExtension did not find the entry this test just wrote: %v", err)
	}
	// the documented behaviour of the lookup, held here so the doc and the code cannot drift
	// apart in silence. If this ever fails because FindExtension started copying, that is a
	// safer lookup and a stale comment: change both.
	found[0] ^= 0xff
	if exts[0].ExtensionData[0] != found[0] {
		t.Errorf("FindExtension answered a copy of the body, and its doc says a view; one of the two is wrong")
	}
	found[0] ^= 0xff

	out, err := ParseLeafKeysFrom(exts[0])
	if err != nil {
		t.Fatalf("ParseLeafKeysFrom: %v", err)
	}
	if !bytes.Equal(out.DeviceXwingPub, pub) {
		t.Fatalf("the wrap target read back off the leaf is not the key that went in")
	}
	held := bytes.Clone(out.DeviceXwingPub)
	for i := range exts[0].ExtensionData {
		exts[0].ExtensionData[i] ^= 0xff
	}
	if !bytes.Equal(out.DeviceXwingPub, held) {
		t.Errorf("mutating the leaf's own extension bytes changed the wrap target already read off it, so the parse kept a view of the leaf")
	}
}

// ---------------------------------------------------------------------------
// the one number nothing in this tree pins across a package boundary
// ---------------------------------------------------------------------------

// messagePackageDir and messagegroupPackageDir are the two sibling packages the X-Wing key
// size is stated in a second time. They are the same directories mls's other cross package
// guardrails scan, and each relative path is this package's directory to that one.
//
// The scope question is answered separately from the class question. The CLASS is every
// X-Wing named package level declaration; the SCOPE is every sibling directory one could be
// declared in, which was one directory until connect/message was split in two and is two
// now. Deriving the class and then writing its scope down against a single directory is what
// made this gate report 26 errors on the commit that moved those declarations one directory
// over: the table was right and the walk had stopped covering where they went.
const (
	messagePackageDir      = "../message"
	messagegroupPackageDir = "../messagegroup"
)

// xwingNamedDeclarationsIn is every package level declaration of one directory's non test
// source whose name mentions X-Wing.
//
// Over the NAME rather than over the value, because the failure this is watching for is a
// second declaration of the same quantity and the two would agree on the value on the day it
// lands -- and disagree later, silently, when one of them is corrected. A name is what a reader
// greps for and what a reviewer sees in a diff.
//
// Non test files only. A test helper named for the thing it measures is not a second statement
// of it, and this package has two.
func xwingNamedDeclarationsIn(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s, which this gate scans for a second statement of the X-Wing key size: %v", dir, err)
	}
	declared := map[string]string{}
	fileSet := token.NewFileSet()
	read := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.ToSlash(filepath.Join(dir, name))
		parsed, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		read++
		declarationsIn(parsed, path, declared)
	}
	if read == 0 {
		t.Fatalf("no non test go file was read out of %s, so this gate scanned nothing and would report a clean bill over any declaration at all",
			dir)
	}
	found := map[string]string{}
	for name, file := range declared {
		if strings.Contains(strings.ToLower(name), "xwing") {
			found[name] = file
		}
	}
	return found
}

// Every X-Wing named declaration of this package and of ../messagegroup, with what each one is.
//
// This is a table and it is held to the derived set in BOTH directions below, which is what
// makes it a classification rather than a list: a new one cannot land without being written
// down here, and one written down here cannot survive the declaration going away.
//
// The reason it is worth a gate at all is that XwingPublicKeyLen is stated twice, once here and
// once as messagegroup.XwingPublicKeySize, and nothing the compiler does across a package boundary
// notices a disagreement on its own. The second copy landed with p2 tasks 19 and 20 and brought
// its pin with it: messagegroup/xwing.go declares the difference in both directions as an array
// length, so a tree in which the two disagree fails to build. This gate is what forced that to
// be written on the commit that landed the copy rather than on a later one, and it is what will
// force the same of a third statement of the size, in either package, whenever one arrives.
var xwingNamedDeclarationsOfBothPackages = map[string]string{
	"AlgIdXwing":                "the wrap KEM code point, 0x0014, and not a size",
	"XwingPublicKeyLen":         "the encapsulation key size, derived against crypto/mlkem and crypto/ecdh by TestXwingPublicKeyLenIsTheMlKem768AndX25519KeySizesAdded",
	"XwingPublicKeySize":        "../messagegroup's second statement of the encapsulation key size, pinned to XwingPublicKeyLen at COMPILE time by the array length declarations in messagegroup/xwing.go",
	"XwingAlgId":                "../messagegroup's second statement of the wrap KEM code point, pinned to AlgIdXwing at COMPILE time beside it",
	"XwingSeedSize":             "the storable X-Wing private key size, 32, and not the encapsulation key size",
	"XwingExpandedSize":         "the SHAKE-256 seed expansion length, 96, and not a key size",
	"XwingMlkemSeedSize":        "the ML-KEM d and z seed length, 64, which is the half of the expansion that is NOT the encapsulation key size",
	"XwingMlkemPublicKeySize":   "the ML-KEM-768 encapsulation key size, 1184; one addend of XwingPublicKeySize, and held to a key crypto/mlkem actually produced by ../messagegroup's own tests",
	"XwingMlkemCiphertextSize":  "the ML-KEM-768 ciphertext size, 1088, and not a key size",
	"XwingCiphertextSize":       "the X-Wing ciphertext size, 1120, and not a key size",
	"XwingSharedSize":           "the shared secret size, 32, and not a key size",
	"XwingX25519KeySize":        "the x25519 key size, 32; the other addend of XwingPublicKeySize",
	"xwingLabel":                "XWingLabel, the six ascii bytes the combiner ends with, and not a size",
	"XwingPrivateKey":           "the private key type, and not a size",
	"XwingPublicKey":            "the public key type, and not a size",
	"XwingPrivateKey.Public":    "the private key type's public half, and not a size",
	"XwingPrivateKey.Seed":      "the private key type's stored seed, and not a size",
	"XwingPublicKey.Bytes":      "the public key type's pk_M then pk_X encoder, and not a size",
	"XwingKeyGenFromSeed":       "the draft section 5.2 key generation, and not a size",
	"XwingGenerateKey":          "key generation over a random source, and not a size",
	"ParseXwingPublicKey":       "the encapsulation key parser, and not a size",
	"xwingCombine":              "the draft section 5.3 combiner, and not a size",
	"XwingEncapsulate":          "the draft section 5.4 encapsulation, and not a size",
	"XwingDecapsulate":          "the draft section 5.5 decapsulation, and not a size",
	"ErrXwingBadSeedSize":       "the refusal of a seed that is not XwingSeedSize, and not a second statement of it",
	"ErrXwingBadPublicKeySize":  "the refusal of a key that is not XwingPublicKeySize, and not a second statement of it",
	"ErrXwingBadCiphertextSize": "the refusal of a ciphertext that is not XwingCiphertextSize, and not a second statement of it",
	"ErrXwingInvalidPoint":      "the refusal of an x25519 half that produced no usable secret, and not a size",
}

// TestNoXwingNamedDeclarationLandsInEitherPackageWithoutBeingClassifiedHere fails on the commit
// that lands a second statement of the X-Wing key size, which is the commit where the pin
// between the two has to be written.
//
// It is the shape TestNoValidationOwnedNameHasLandedBesideItsStandIn uses for the same problem:
// something is owed by another plan, nothing in this package can see whether it has arrived, and
// the reminder has to fail rather than log. A gate that logged "still owed" would go on logging
// it after the copy landed with a digit wrong.
//
// What a reader of a failure here should do: if the new name states the X-Wing encapsulation key
// size, it is p2 task 22's subject -- write the compile assertion that it equals
// XwingPublicKeyLen, in the package that may import both, and then classify it here. If it
// states something else about X-Wing -- a ciphertext size, a shared secret size, another code
// point -- classify it here and it is no longer this gate's business.
func TestNoXwingNamedDeclarationLandsInEitherPackageWithoutBeingClassifiedHere(t *testing.T) {
	found := map[string]string{}
	for _, dir := range []string{".", messagePackageDir, messagegroupPackageDir} {
		for name, file := range xwingNamedDeclarationsIn(t, dir) {
			found[name] = file
		}
	}
	if file, declared := found["XwingPublicKeyLen"]; !declared || file != "extension.go" {
		t.Fatalf("the scan of ., %s and %s read %v, and XwingPublicKeyLen is not in extension.go among them; it certainly is, so this scan is reading something other than these three packages",
			messagePackageDir, messagegroupPackageDir, slices.Sorted(maps.Keys(found)))
	}
	for _, name := range slices.Sorted(maps.Keys(found)) {
		if _, classified := xwingNamedDeclarationsOfBothPackages[name]; !classified {
			t.Errorf("%s declares %s and xwingNamedDeclarationsOfBothPackages does not classify it. If it states the X-Wing encapsulation key size, this is the commit that owes the compile assertion pinning it to mls.XwingPublicKeyLen (%d) -- p2 task 22 -- because nothing in this tree checks the two against each other. If it states something else about X-Wing, classify it there and this gate is done with it",
				found[name], name, XwingPublicKeyLen)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(xwingNamedDeclarationsOfBothPackages)) {
		if _, declared := found[name]; !declared {
			t.Errorf("xwingNamedDeclarationsOfBothPackages classifies %s and none of this package, %s and %s declares it, so this table is describing a tree that no longer exists",
				name, messagePackageDir, messagegroupPackageDir)
		}
	}
}

// TestThePskSecretExemptionIsAReachThroughACollidingMethodNameAndNothingElse is the
// measurement behind the PskSecret entry above, so that entry is evidence and not an assertion.
//
// The four entries beside it are judgements about a byte run -- a digest is not a body -- and
// there is nothing more to measure about them. This one is a claim about the CALL GRAPH: that
// PskSecret reaches the encoder only because one name in its body is spelled the same as a method
// of another type. A claim about the graph can be checked against the graph, and an exemption that
// can be checked and is not is the shape this package keeps finding in other people's tables.
//
// Two clauses, and each expires the entry on a different change. PskSecret must invoke no encoder
// helper of its own, so an edit that made it genuinely assemble a body fails here rather than
// being waved through by a table entry written before that was true. And every name it invokes
// that the closure holds must be one this package declares more than once, so a reach through an
// UNAMBIGUOUS name -- a real edge the closure got right -- fails here too.
//
// What it does not do is make the closure right. The closure attributes a call on a foreign
// expression to every declaration sharing the callee's spelling, which is the same base name
// conflation theNamesInvokingTheStorage's own doc records being fixed once already, one level in:
// that fix separated a field read from a method call, and what is left is method from method.
// Fixing it properly wants the receiver at the call site, which wants the type checker, which the
// synthetic control this gate is held by cannot be run through. So the over-approximation stays
// and this is what keeps its one consequence honest.
func TestThePskSecretExemptionIsAReachThroughACollidingMethodNameAndNothingElse(t *testing.T) {
	const excused = "PskSecret"
	if _, listed := extensionBodyByteRunsThatAreNotBodies[excused]; !listed {
		t.Fatalf("%s is not in the exemption table, so this control is holding an entry that is not there", excused)
	}
	scanned := packageLevelFunctions(t).files
	files := []parsedSource{}
	for _, path := range scanned {
		files = append(files, mustParseSource(t, path))
	}
	declared := declaredAcross(files)
	helpers := extensionBodyEncoderHelpersIn(files, extensionBodyTypesIn(files))
	if len(helpers) == 0 {
		t.Fatal("the seed read no encoder helper out of this package, so both clauses below demand nothing")
	}
	reaching := theNamesReachingTheExtensionBodyEncoder(declared, helpers)
	if !slices.Contains(reaching, excused) {
		t.Fatalf("the closure no longer holds %s, so its entry in extensionBodyByteRunsThatAreNotBodies excuses nothing; delete both",
			excused)
	}
	declaredTimes := map[string]int{}
	for _, one := range declared {
		declaredTimes[one.name] += 1
	}
	read := 0
	for _, one := range declared {
		if one.name != excused || one.receiver != "" || one.body == nil {
			continue
		}
		read += 1
		invoked := namesInvokedBy(one.body)
		for _, helper := range helpers {
			if invoked[helper] {
				t.Errorf("%s invokes %s, the encoder this package's extension bodies are assembled with, in its own body; its reach is no longer a name collision and the exemption is no longer the right answer",
					excused, helper)
			}
		}
		collisions := []string{}
		for _, name := range slices.Sorted(maps.Keys(invoked)) {
			if !slices.Contains(reaching, name) || name == excused {
				continue
			}
			if declaredTimes[name] > 1 {
				collisions = append(collisions, name)
				continue
			}
			t.Errorf("%s invokes %s, which the closure holds and this package declares exactly once, so that edge is a real one and the exemption is excusing more than a collision",
				excused, name)
		}
		if len(collisions) == 0 {
			t.Errorf("%s invokes no name the closure holds at all, so it cannot be reaching the encoder through this body; the closure is reading something other than this package",
				excused)
		}
		t.Logf("%s reaches the encoder through %v, each declared %d times in this package",
			excused, collisions, declaredTimes["Validate"])
	}
	if read != 1 {
		t.Fatalf("the scan found %d package level declarations of %s, want 1, so it read something other than this package",
			read, excused)
	}
}
