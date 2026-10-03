// What this package's production source is built out of, pinned as a WHOLE rather than as a ban
// list.
//
// connect/mls holds the same property over the union of its three scan roots, and this is not a
// duplicate of it: mls's list is one set across three directories and answers "may this module's
// crypto reach that package at all", while this one is per directory and answers "what is the
// CLIENT HALF of the record layer made of". The difference has a subject. Spec A section 5.6
// assigns the durable stream index store to sdk and the argument rests on a measured fact --
// neither half of the record layer imports an I/O package -- and that fact is exactly what an
// import gate can hold and a prose paragraph cannot. A file backed reserver landing in
// streamindex.go fails here on the commit that adds it, by name, with the reason beside it.
//
// A ban list is the shape this project has been walked past fourteen times: one edit outside the
// list and the gate reports the clean run a complete gate reports. What is pinned here is the
// complete set, so os fails exactly as loudly as a third party crypto library, and so does a
// second hash package brought in to spell an extraction the key schedule's own gate would then
// have to recognise.
//
// The SCOPE (R3a) is this directory's production source. The CLASS is every import of it, which
// is total by construction: there is no member of the class this reading can miss, because the
// reading IS the class.
package messagegroup

import (
	"go/ast"
	"maps"
	"slices"
	"strings"
	"testing"
)

// Every import this package's production source may hold, with what it is for.
//
// A row is a claim about what the package reaches, so each says the thing that would make its
// arrival worth a second look if it were ever wrong.
var messagegroupProductionImports = map[string]string{
	`"crypto/cipher"`:                        "the cipher.AEAD interface the record aead is handed back as",
	`"crypto/sha256"`:                        "H(ct_body), which is the header's body_hash and the one thing a pruned record still says about what it carried",
	`"crypto/subtle"`:                        "ConstantTimeCompare, and it is the only comparator this package may reach: guardrail G8 derives the banned class off the imports, so a bytes.Equal here would fail message/writeauth_test.go's gate over this directory",
	`"crypto/ecdh"`:                          "X-Wing's x25519 half, for the key TYPES only; the exchange itself goes through mls's four wrappers so the low order point refusal cannot be bypassed",
	`"crypto/mlkem"`:                         "X-Wing's ML-KEM-768 half, draft-connolly-cfrg-xwing-kem",
	`"crypto/sha3"`:                          "the SHAKE-256 that expands X-Wing's seed and the SHA3-256 its combiner is",
	`"encoding/binary"`:                      "the four octet big endian encoding of a leaf index, in the one place LP(leaf_index) is read",
	`"errors"`:                               "errors.New, for the sentinels of the two error files and nothing else",
	`"fmt"`:                                  "fmt.Errorf(\"%w: ...\", sentinel, detail) only: the sentinel stays the matchable identity and the wrap carries the octet counts a caller needs",
	`"io"`:                                   "the io.Reader an entropy taking function is handed, which entropy_test.go's derived class is keyed on",
	`"sync"`:                                 "the two ratchets' state locks and the receiver table's, which are what stop two concurrent senders consuming one stream index",
	`"github.com/urnetwork/connect/message"`: "the record types, the two aad builders, the write_auth mac and the size ladder. The edge is ONE WAY: connect/message must never import this package, and connect/layering_test.go holds that",
	`"github.com/urnetwork/connect/mls"`:     "the crypto provider every derivation runs on, the four X25519 wrappers, the group this package's one adapter wraps, and the two window bounds the receiver's defaults are read off",
	`"github.com/urnetwork/connect/mls/syntax"`: "the tree's one length prefix implementation, WriteOpaqueLP, and the writer every info preimage is assembled through",
	`"golang.org/x/crypto/chacha20poly1305"`:    "XChaCha20-Poly1305, the record aead MASTER section 7.1 registers as 0x0021",
}

// The packages this gate exists to keep OUT, named so the failure a reader sees carries the
// reason rather than only the diff.
//
// It is not the mechanism -- the mechanism is the complete set above, and an import absent from
// it fails whether or not it is named here. This is the message.
var messagegroupImportsWithAReason = map[string]string{
	`"os"`:            "a durable store. Section 8.2 assigns the stream index reservation to sdk's MessageStore and the argument rests on this package importing no I/O at all",
	`"bufio"`:         "a durable store",
	`"path/filepath"`: "a durable store",
	`"crypto/hmac"`:   "a keyed hash. Every extraction on this package's path goes through mls's Extract, which takes (salt, ikm) in the spec's order; a raw hmac.New is HKDF-Extract with the arguments free to be transposed, which is guardrail G1's defect",
	`"crypto/hkdf"`:   "an extraction with the arguments in the LIBRARY's order, ikm first. Guardrail G1 is that this package never spells it",
	`"crypto/rand"`:   "a process entropy source. Every draw here takes an injected io.Reader, which is what entropy_test.go's nil refusal is over",
	`"time"`:          "a clock. Nothing in this package takes one; a function that needs the time takes an injected nowMs func() int64",
	`"unsafe"`:        "section 5.5's unsafe.Pointer zeroization, which connect/mls declined first and open item M1-37 records",
	`"syscall"`:       "a platform specific call. The nine platform cross build is a gate and a windows-only lazy DLL passes every test on this machine",
}

func TestThisPackageIsBuiltFromExactlyTheseImports(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	found := map[string][]string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			general, isGeneral := declaration.(*ast.GenDecl)
			if !isGeneral {
				continue
			}
			for _, spec := range general.Specs {
				imported, isImport := spec.(*ast.ImportSpec)
				if !isImport {
					continue
				}
				found[imported.Path.Value] = append(found[imported.Path.Value], source.path)
			}
		}
	}
	if len(found) == 0 {
		t.Fatal("no import was read out of this package's production source, so this gate pinned an empty set")
	}
	for path, files := range found {
		if _, isPinned := messagegroupProductionImports[path]; isPinned {
			continue
		}
		reason := "it is not on the list of what the client half of the record layer is built from"
		if named, hasReason := messagegroupImportsWithAReason[path]; hasReason {
			reason = named
		}
		t.Errorf("%s is imported by %v and is not pinned: %s", path, files, reason)
	}
	for path := range messagegroupProductionImports {
		if _, isImported := found[path]; !isImported {
			t.Errorf("%s is pinned and nothing imports it; a row that outlived its import reads as coverage and would excuse the import's return with no second look",
				path)
		}
	}
	t.Logf("%d production files, %d imports: %v", len(sources), len(found), slices.Sorted(maps.Keys(found)))
}

// The two halves of the list must not overlap: a package that is both pinned and named as one to
// keep out is a contradiction, and the failure it would produce reads as the opposite of what it
// means.
func TestTheImportListsDoNotContradictEachOther(t *testing.T) {
	for path := range messagegroupImportsWithAReason {
		if _, isPinned := messagegroupProductionImports[path]; isPinned {
			t.Errorf("%s is both pinned and named as an import to keep out", path)
		}
	}
	// and every path in either list is a quoted import path, so a row that lost its quotes
	// -- and would therefore match nothing -- is a failure rather than a silent exemption
	for _, list := range []map[string]string{messagegroupProductionImports, messagegroupImportsWithAReason} {
		for path, reason := range list {
			if !strings.HasPrefix(path, `"`) || !strings.HasSuffix(path, `"`) {
				t.Errorf("%s is not written the way an import spec's path is, so it can never match one", path)
			}
			if strings.TrimSpace(reason) == "" {
				t.Errorf("%s carries no reason", path)
			}
		}
	}
}
