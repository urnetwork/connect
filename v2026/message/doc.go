// The storage layer of the messaging work, in the half a message server is allowed to link:
// the record, its wire form, the four preimages it is authenticated by, and the one
// structured field of it the server may read.
//
// The X-Wing hybrid key encapsulation this sentence used to open with is no longer here. It
// moved to connect/messagegroup on the commit that split this package in two, because
// xwing.go was the only thing in the tree giving connect/message an import of connect/mls,
// and spec B section 2.2 forbids the message server from linking an MLS parser at all -- as
// a capability and not as a habit. The server's own dependency gate was red about this
// package until the split, and it went green because connect/mls left the closure rather
// than because anything was added to an allow list.
//
// The layering is one way, and both directions matter. This package must never import
// connect, and connect must never import it: a package that imports its own subpackage
// inverts the dependency, so the child's API is frozen by its own parent and cycles
// become easy to create. It must never import connect/mls either, and that is the rule the
// split exists to make true rather than to state: what it reads and writes wire vectors
// with is connect/mls/syntax, which spec B section 2.2 allows by name and which is not an
// MLS implementation. connect/messagegroup is where the one reviewed x25519 call site is
// reached from, and it MAY import this package — and since m1 wave 1's sealer it DOES,
// which is measured rather than remembered. writeauth_test.go's
// TestEveryPackageBuiltOnThisOneIsUnderTheConstantTimeGate walks this module for the
// production packages that import connect/message and reports connect/messagegroup as the
// one; its constant time class over that directory is armed rather than empty, and
// connect/layering_test.go holds the same direction as a rule from the other side. This
// package must never import connect/messagegroup, and connect/mls must not reach back into
// either — connect/mls imports only the standard library, golang.org/x/crypto, and its own
// child mls/syntax.
//
// What is here so far is the record and its two ladders, in record.go: the go form of
// the record master section 8 defines, the size ladder a body is padded to, the eph
// ladder a transient record expires on, the one pair of functions that crosses between
// the retention class as go carries it and the single byte the wire carries it in, and
// the class predicate spec B section 7.2 sweeps on. Beside them, in codec.go, is the
// wire form of that record: the layout is defined there rather than in any spec, because
// no spec gives one — spec B carries record_bytes opaquely and says only that this
// package's encoder produced it — and codec.go's opening comment states both the table
// and the rule that generated it. Beside them again, in aad.go and writeauth.go, are the
// four preimages a record is authenticated by: the two aeads master section 8 seals the
// body and the head under, and the two macs master section 9.2 authorizes a write and a
// read with, together with the two per epoch keys those macs run under. Beside those, in
// attachment.go, is the server attachment of spec A section 5.11: the one structured field
// of a record the server may read, its four bodies and their kind discriminator, and every
// question spec B section 5.1 check 3 asks of a parsed one, so that the server asks rather
// than re-derives. Beside those four is a fifth body, the epoch digest attachment of ruling
// 27, which is the epoch attachment with its two keys replaced by one digest over both of
// them; it has a door of its own and that file's comment argues which server refuses it and
// why that refusal is the rollout. The key schedule does NOT land beside them and no longer lands at all:
// it IS in connect/messagegroup, which no message server links, and it takes these types
// from here. As of m1 wave 1 that package holds the storage root and the three retention
// class keys, the record key ladder, both ratchets, the group engine and its connect/mls
// adapter, the session, and SealRecord and OpenRecord -- which are the only two callers of
// this package's two aad builders and of ComputeWriteAuth, and which are the reason the
// edge from there to here exists at all. The edge is one way: nothing here imports
// connect/messagegroup, and connect/layering_test.go holds that from the other side.
// Nothing in this package logs a failure and carries on: every error here is one of the
// sentinels in errors.go, and the only bare bools are the three constant time verifiers
// of spec A section 5.7 and that class predicate, none of which reports a failure. The
// one exception to reporting rather than stopping is the computing half of writeauth.go,
// which panics on an input its own published signature has no error to refuse through —
// that file's comment argues it at length — while the verifying half, which is the half a
// server reaches, never panics.
//
// Four rules in this package are enforced by a test rather than by the compiler, and
// every one of them walks the tree rather than reading a list of files. The forbidden
// primitive gate in mls/crypto_forbidden_test.go covers this package and
// connect/messagegroup as well as its own, which is why the directory existed before there
// was anything in it: a gate whose root is missing either fails outright or, worse, reports
// clean having read nothing. The join gate in record_test.go covers this package,
// connect/mls, connect/messagegroup and the sdk repository beside connect, and holds both
// halves of the class-and-bucket crossing — the join and
// the split alike — to record.go alone. It reads the syntax tree rather than the text,
// because the shapes it bans have more spellings than anybody can list. The last two are
// in writeauth_test.go and read the same tree — and, since the split, connect/messagegroup's
// tree beside it, because the constant time rule is about a kind of function and not about
// a directory: guardrail G8 of spec A section 5.9, that no
// verifier decides equality in variable time, and section 5.7's own obligation that no
// call path macs a request under a write key. Both compute the class they run over out of
// the tree — every function whose name begins with Verify, and every function that names
// the write key's label — and both have a positive control under testdata, because a gate
// that finds nothing because it is broken reports what a gate that finds nothing because
// the code is clean reports.
package message
