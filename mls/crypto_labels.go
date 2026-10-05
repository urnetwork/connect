// The RFC 9420 section 5.1 and 5.2 labelled constructions. Every one of them is a TLS
// presentation language struct, so all of them go through mls/syntax: there is one
// length prefix implementation in the system and one place for a length prefix bug to be.
//
// MLS derives every secret, and signs and macs over, these serialized forms, so a
// preimage that two implementations build differently is a protocol split, and a preimage
// that admits two readings is a signature bypass primitive. That is why nothing here hand
// rolls a prefix.
//
// The whole security argument of this file is the "MLS 1.0 " prefix and the length
// prefixed fields around it, and none of it is observable from the outside: dropping the
// prefix, altering it, dropping the uint16 length, writing the context raw, or
// transposing label and context each produce a well formed output of exactly the
// requested size. Only a published answer separates them, which is what
// crypto_labels_test.go holds this to.
//
// The section 5.2 references carry the same argument one step further. Their two labels
// differ in a single word, and swapping them, or letting both makers share one, produces
// two references that are still 32 bytes, still deterministic and still distinct for
// distinct inputs — while a key package reference and a proposal reference over the same
// bytes have quietly become the same value. No property of the output says which label
// built it, so what holds them is the corpora where another implementation wrote the
// reference down.
package mls

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"io"

	"github.com/urnetwork/connect/mls/syntax"
)

// The domain separator every MLS label carries before serialization. The version is part
// of it: a future MLS derives different secrets from the same transcript rather than
// silently interoperating with this one.
const MlsLabelPrefix = "MLS 1.0 "

// The sticky writer's error, carried OUT to the caller rather than taken down the process
// with it (convention C2).
//
// syntax.ErrLengthExceedsMax survives the wrap, so a caller asks errors.Is which refusal
// this was instead of reading the string.
func mlsLabelPreimage(w *syntax.Writer) ([]byte, error) {
	encoded, err := w.Bytes()
	if err != nil {
		return nil, fmt.Errorf("mls: a labelled preimage could not be encoded: %w", err)
	}
	return encoded, nil
}

// mlsLabelPreimage for the constructions whose signatures cannot carry a refusal.
//
// THE PREMISE THIS COMMENT USED TO STATE WAS FALSE, and the correction is written out
// rather than the sentence quietly deleted, because the false version was true FIELD BY
// FIELD and that is what let it survive review. It said: every value that reaches a
// labelled construction arrived through a decode or an encode already bounded by
// syntax.MaxVectorLength, so a panic here is unreachable.
//
// Every FIELD does obey that limit. A COMPOSITION of fields does not. RefHash wraps a
// whole serialized AuthenticatedContent in ONE opaque<V>, and that structure's group_id,
// authenticated_data, proposal arms and signature are each bounded by a mebibyte with an
// UNBOUNDED SUM. Measured, and measured HERE rather than only in this paragraph --
// TestTheEnormousAddIsTheOctetCountsTheseDisclosuresState holds every number in this
// sentence, because two files stated it and they stated two different numbers: an Add whose
// key package carries a BasicCredential of MaxVectorLength-64 octets marshals to 1050045
// octets, decodes back through syntax.Unmarshal, and signed and verified as an authentic
// member message on the build that had no bound — and it took
// the process down in five separate places, ProposalCache.Store, KeyPackage.Ref,
// DeriveJoinerSecret, EncryptWithLabel and VerifyWithLabel, the last of which runs BEFORE
// any application level check a caller could have made. One member, one valid proposal,
// every other member crashed.
//
// What makes the panic unreachable NOW is not that premise but a bound, applied by the
// outermost declaration that CAN report one, in the shape owner_successor.go's
// successionPreimage already uses. Two instruments, and which of them applies is decided by
// the signature rather than by taste: checkLabelledConstruction at the four entry points
// into this layer that carry an error — SignWithLabel, VerifyWithLabel, EncryptWithLabel and
// DecryptWithLabel — and marshalBoundedComposition where a composition is BUILT, for the one
// construction whose signature cannot carry a refusal at all.
//
// TestEveryCompositionEnteringALabelledConstructionIsBoundedBeforeItGetsThere derives that
// set of call sites off the source rather than trusting this paragraph, which is the whole
// lesson of the premise above: a sentence about reachability that no test reads is a
// sentence that stays written after it stops being true. It anchors on the declaration that
// PANICS on an encoder's refusal, walks BACK to every parameter whose bytes reach it, and
// requires every value this package builds with a syntax encoder and sends there to have
// been bounded first. A composition of a shape nobody has thought of yet is one that walk
// reports, which is exactly what a premise about fields could not do.
//
// It stays a panic rather than becoming a truncation for the reason it always did: a label
// that describes more bytes than it carries is exactly the shape a signature bypass is
// built out of. It stays a panic rather than an error only where the signature leaves no
// room for one — mlsKdfLabel under ExpandWithLabel, DeriveSecret and DeriveTreeSecret, which
// are CryptoProvider methods — and nowhere else. RefHash used to be named here too, on the
// ground that the tree and the framing layers reach it through a fixed reference maker; that
// was never a pin and it answers an error now. Answering nil where the panic does remain
// would be worse than the crash rather than safer: two references that are both nil are two
// proposals with one name, which is the collision the labels in this file exist to prevent.
//
// WHAT IS LEFT, written down rather than glossed. The walk is over THIS PACKAGE, so what it
// establishes is that no message a PEER sends reaches the panic: every path from decoded
// bytes to a labelled construction is one of the bounded sites it derives.
//
// The OUT-OF-PACKAGE caller is no longer part of that residual at any EXPORTED declaration.
// RefHash refuses both of its fields and the two reference makers carry that refusal out, and
// (*KeySchedule).Export refuses the label it takes from a caller, on a signature that already
// carried ErrExportLength for a caller's length. What used to stand here said closing them
// "means widening the CryptoProvider interface, which is pinned", and that was true of none
// of the four: not one of them is a method of that interface. The argument was borrowed from
// the provider methods standing next to them.
//
// What remains is a provider's own ExpandWithLabel, DeriveSecret or DeriveTreeSecret, called
// from outside this package with a label or a context past the limit. Those signatures the
// pinned interface really does fix. That is a local programming error of the same kind as the
// wrong length seed handed to ed25519.NewKeyFromSeed that SignWithLabel's own comment
// records, and closing it does mean widening that interface.
func mlsLabelBytes(w *syntax.Writer) []byte {
	encoded, err := mlsLabelPreimage(w)
	if err != nil {
		panic(err.Error())
	}
	return encoded
}

// checkLabelledFieldLength refuses a value too long to be ONE length prefixed field of a
// labelled construction.
//
// The bound is stated over the VALUE's own length and not over the finished preimage,
// because a labelled construction wraps the caller's value in exactly one opaque<V> and
// the limit is that field's. The label and the varint prefixes beside it are this file's
// own constants; a preimage that failed once the value had passed this check would be a
// bug here rather than a message a peer sent.
//
// A PRE-CHECK returning syntax.ErrLengthExceedsMax rather than a write that latches it,
// which is owner_successor.go:successionPreimage's shape, and it is that shape because the
// value being judged is a PEER's: a library that panics on a peer's message hands every
// member of a group a remote crash that one valid proposal reaches.
//
// It takes the LENGTH rather than the field, so that the one comparison in this file can
// also judge a concatenation that must not be built in order to be measured — the prefixed
// label of checkLabelledConstruction below. Two spellings of "does this fit in one field"
// are two things that can disagree, and this is the one.
//
// The name arrives in TWO PARTS and is joined HERE, on the refusal path and nowhere else. A
// caller that names a sub field — the prefixed label of checkLabelledConstruction below —
// would otherwise build that string on every signature and every verify in the system, to
// name a branch no message takes. A caller with nothing to add passes the empty string and
// its refusal reads exactly as it did, so no error text moved.
func checkLabelledFieldLength(what string, part string, length int) error {
	if length > syntax.MaxVectorLength {
		return fmt.Errorf("%w: the serialized %s is %d octets and one labelled field holds at most %d",
			syntax.ErrLengthExceedsMax, what+part, length, syntax.MaxVectorLength)
	}
	return nil
}

// checkLabelledConstruction refuses a labelled construction that cannot be encoded. It is
// what the four entry points into this layer whose signatures CAN carry a refusal ask before
// they build a preimage.
//
// BOTH fields and not only the value. A labelled construction is two opaque<V> in one
// preimage and either of them alone latches the writer, so a gate over the value only is a
// gate that reports half the shape it was written for.
//
// The VALUE is the half this exists for and the half a peer controls: it is a COMPOSITION, a
// whole serialized structure whose fields are each bounded by a mebibyte and whose sum is
// not, which is the defect the paragraph above mlsLabelBytes records. The LABEL is an RFC
// 9420 constant at every call this package makes and could be argued out of the check on
// that ground; it is checked anyway, because these entry points are exported and take it
// from a caller, and because a rule that holds for one field of a two field preimage is a
// rule the next reader has to derive over again.
//
// The label is measured WITH the prefix, since the prefix is inside the field whose length
// is being declared, and it is MEASURED rather than concatenated: building a second copy of
// mlsSignContent's label on every signature and every verify is a cost every message in the
// system pays for a branch no message takes.
//
// THE FIELD'S NAME USED TO CONTRADICT THAT SENTENCE, and the contradiction is written out
// rather than the line quietly repaired, because it is the third premise in this file to
// have outlived its own code. The label BYTES were measured, and then what+" label" was
// concatenated EAGERLY to name them, on every signature and every verify in the system, for
// the branch the sentence above says no message takes. The name now travels to
// checkLabelledFieldLength in two parts and is joined only where it is read, which is inside
// the refusal.
//
// WHAT THAT COSTS IS MEASURED HERE rather than carried over from the report that prompted it,
// because the report's figure does not reproduce and the difference is the whole shape of the
// answer. It named 16.72 ns/op and 24 B/op, one allocation. On go1.26.5 the eager form costs
// 7.99 ns/op against this one's 2.27 for the same call, and ZERO allocations at either name
// this package passes: Go concatenates into a 32 octet buffer on the STACK when the result
// cannot escape, and "signature content label" is 23 of them. The allocation the report named
// is real and is one bad name away — measured, a field name of 26 octets allocates nothing
// and one of 33 allocates once, since 26 plus " label" is exactly the buffer. So what every
// message was paying is 5.7 ns rather than an allocation, and what this closes is both.
// TestTheLabelledConstructionCheckBuildsNoNameOnThePathAMessageTakes sweeps the length rather
// than driving the two names, for exactly that reason: over this package's own names the
// defect is invisible to an allocation count.
func checkLabelledConstruction(what string, label string, value []byte) error {
	if err := checkLabelledFieldLength(what, " label", len(MlsLabelPrefix)+len(label)); err != nil {
		return err
	}
	return checkLabelledFieldLength(what, "", len(value))
}

// marshalBoundedComposition serializes a structure that is about to become ONE length
// prefixed field of a labelled construction, and refuses it when the SUM of its fields
// does not fit even though every field did.
//
// syntax.Marshal alone is not enough, and that gap IS the defect this exists for: it
// bounds each field it writes and says nothing about the total, so it answers 1050045
// octets happily and the labelled field that wraps them refuses. Every caller in this
// package that hands a serialized structure to RefHash or to ExpandWithLabel comes through
// here.
//
// ExpandWithLabel is now the only one of those two whose signature cannot report a refusal,
// and this declaration stays in front of RefHash's callers anyway. The reason is the error
// rather than the crash: what a caller of (*KeyPackage).Ref has to read is that a KEY PACKAGE
// did not fit, and RefHash one frame down can only say that a reference input did not. The
// two bounds are the same comparison over the same octets, so they cannot disagree about
// whether to refuse — only about what to call the thing refused.
func marshalBoundedComposition(what string, v syntax.Marshaler) ([]byte, error) {
	encoded, err := syntax.Marshal(v)
	if err != nil {
		return nil, err
	}
	if err := checkLabelledFieldLength(what, "", len(encoded)); err != nil {
		return nil, err
	}
	return encoded, nil
}

// struct { uint16 length; opaque label<V>; opaque context<V> } KDFLabel, serialized.
//
// The length is the requested output size and not the size of anything in the preimage,
// so two derivations that differ only in how many bytes the caller wanted are still
// separated. The uint16 conversion cannot lose a bit that matters: Expand refuses
// anything above hpkeMaxExpandLength, which is 8160, so a length that survives the call
// is representable, and one that does not stops in Expand rather than deriving under a
// truncated label.
func mlsKdfLabel(label string, context []byte, length int) []byte {
	writer := syntax.NewWriter()
	writer.WriteUint16(uint16(length))
	writer.WriteOpaque([]byte(MlsLabelPrefix + label))
	writer.WriteOpaque(context)
	return mlsLabelBytes(writer)
}

// HKDF-Expand over a preimage that names the label, the context and the output size, so
// no two derivations in the protocol share an info string.
func (self *suiteCryptoProvider) ExpandWithLabel(secret []byte, label string, context []byte, length int) []byte {
	return self.Expand(secret, mlsKdfLabel(label, context, length), length)
}

// The Nh byte derivation with no context. The context field is still written — an empty
// vector is one length byte, not nothing — which is the difference between this and an
// encoder that dropped the field, and the only thing that can see the difference is a
// published answer.
func (self *suiteCryptoProvider) DeriveSecret(secret []byte, label string) []byte {
	return self.ExpandWithLabel(secret, label, nil, self.params.Nh)
}

// The generation is the context, encoded as a big endian uint32 (RFC 9420 section 9). It
// goes through the same writer as everything else: there is one integer encoder in this
// system and it is p1's, so a byte order cannot be chosen twice.
func (self *suiteCryptoProvider) DeriveTreeSecret(secret []byte, label string, generation uint32, length int) []byte {
	writer := syntax.NewWriter()
	writer.WriteUint32(generation)
	return self.ExpandWithLabel(secret, label, mlsLabelBytes(writer), length)
}

// The reference labels of RFC 9420 section 5.2, written out in full because RefHash does
// not add the "MLS 1.0 " prefix: here the prefix is part of the published label rather
// than something a construction contributes.
//
// They are the entire domain separation between the two kinds of reference, and the one
// input neither maker's own behaviour can expose. TestRefLabelsAreTheRfcStrings holds
// them to the RFC's text, and the published Welcome and Commit corpora hold them to a
// reference computed elsewhere, which is what a string this project typed twice cannot do.
const (
	KeyPackageRefLabel = "MLS 1.0 KeyPackage Reference"
	ProposalRefLabel   = "MLS 1.0 Proposal Reference"
)

// struct { opaque label<V>; opaque value<V> } RefHashInput, hashed.
//
// The label is used verbatim. That is not an oversight and not an inconsistency with
// ExpandWithLabel: RFC 9420 section 5.2 defines the reference labels with the prefix
// already inside them, and the crypto-basics vector exercises this with a bare label that
// must not gain one.
//
// Both fields are length prefixed, which is what keeps a reference from being forgeable
// by moving the boundary: without the prefixes, a label ending in the first byte of a
// value and a label one byte shorter hash the same input.
//
// The provider is a parameter rather than a receiver because the reference makers are
// what the tree and the framing layers call, and they hold a CryptoProvider rather than
// the concrete type.
//
// THE SENTENCE THAT USED TO FINISH THIS PARAGRAPH WAS FALSE, and it is written out rather
// than quietly deleted because it is the second retracted premise this file has carried as a
// live comment. It said: "No error return, by the registry's fixed signature — see
// mlsLabelBytes for why the writer cannot fail here." The paragraph it pointed AT, forty
// lines up, said the opposite in the same file — that an out-of-package caller handing this
// declaration an over-long value still panics — and that one was the correct one. Two
// sentences in one file that contradict each other, with the comment citing the one that
// disagrees with it.
//
// Nothing pinned the signature either. The registry the sentence named is a map[string]any
// of package level constructions, which fixes no arity and no result; what is pinned is the
// CryptoProvider INTERFACE, and this is not one of its methods. So the argument that leaves
// mlsKdfLabel panicking under ExpandWithLabel never applied here at all, and this refuses
// instead, on BOTH of its fields and through the same one comparison every other labelled
// construction asks.
//
// It refuses rather than answering a reference, which is the same decision mlsLabelBytes
// records for the panic it replaces: two references that are both nil are two proposals with
// one name, and that collision is the whole reason these labels exist. A caller that reads
// the slice without reading the error gets zero bytes rather than a plausible value.
//
// The boundary is MaxVectorLength on BOTH fields rather than MaxVectorLength minus the
// prefix on the label, which is the bytes-level decision above restated as a bound: this
// construction adds no "MLS 1.0 " of its own, so the caller's whole label is the field.
//
// The PROVIDER is judged first, ahead of both fields, which is the order
// (*AuthenticatedContent).ProposalRef gives the reason for: a body that measured the caller's
// value first would answer a length refusal to a caller whose actual mistake was passing no
// provider, and send it to shrink a value that was never the problem. It is also the only
// order that does not read a method off a nil interface. That refusal is not a decision this
// declaration got to make once it had an error to return --
// TestEveryDeclarationHandedANilProviderRefusesRatherThanDereferencingIt derives the class off
// the signature, so growing the error moved this construction into it.
func RefHash(crypto CryptoProvider, label string, value []byte) ([]byte, error) {
	if crypto == nil {
		return nil, fmt.Errorf("%w: the reference is the provider's hash", ErrNilCryptoProvider)
	}
	if err := checkLabelledFieldLength("reference input", " label", len(label)); err != nil {
		return nil, err
	}
	if err := checkLabelledFieldLength("reference input", "", len(value)); err != nil {
		return nil, err
	}
	writer := syntax.NewWriter()
	writer.WriteOpaque([]byte(label))
	writer.WriteOpaque(value)
	return crypto.Hash(mlsLabelBytes(writer)), nil
}

// The reference by which a Welcome addresses a joiner and a proposal names the key package
// it adds. The input is the serialized KeyPackage, not the MLSMessage that carried it.
//
// The refusal is RefHash's and is carried out unchanged rather than being restated here. A
// second length comparison written at this declaration would be the second spelling
// checkLabelledFieldLength's comment exists to prevent, and this maker adds nothing to the
// field being judged.
func MakeKeyPackageRef(crypto CryptoProvider, keyPackage []byte) ([]byte, error) {
	return RefHash(crypto, KeyPackageRefLabel, keyPackage)
}

// The reference by which a Commit names a proposal it does not carry by value. The input
// is the serialized AuthenticatedContent that framed the proposal and not the Proposal
// itself (RFC 9420 section 5.2), so the reference covers the sender and the signature
// rather than only the proposed change.
//
// The refusal is RefHash's, carried out for the reason MakeKeyPackageRef gives.
func MakeProposalRef(crypto CryptoProvider, authenticatedContent []byte) ([]byte, error) {
	return RefHash(crypto, ProposalRefLabel, authenticatedContent)
}

// struct { opaque label<V>; opaque content<V> } SignContent, serialized, with the
// "MLS 1.0 " prefix on the label.
//
// The two length prefixes are what make a signature cover one reading of its input rather
// than every reading. Without them a label ending where the content begins and a label one
// byte longer serialize to the same run of bytes, so a signature made for one purpose is a
// valid signature for another over content the signer never saw. That is not a difference
// the primitive can notice: ed25519 signs whatever it is handed.
//
// The prefix is on the label here and not on RefHash's, which is not an inconsistency:
// RFC 9420 section 5.2 writes the reference labels out with the prefix already inside
// them, and section 5.1.2 writes this one without.
//
// No error return, by the interface spec A section 3.3 fixes on the two callers below.
// See mlsLabelBytes for why the writer cannot fail here.
func mlsSignContent(label string, content []byte) []byte {
	writer := syntax.NewWriter()
	writer.WriteOpaque([]byte(MlsLabelPrefix + label))
	writer.WriteOpaque(content)
	return mlsLabelBytes(writer)
}

// The RFC 9420 section 5.1.2 signature: ed25519 over the SignContent preimage rather than
// over the content, so a signature is bound to the purpose it was made for.
//
// The private key is the 32 byte RFC 8032 seed, which is what MLS carries on the wire and
// what the crypto-basics vectors supply. Go's ed25519.PrivateKey is the seed followed by
// the public key, so the expansion happens here and the 64 byte form never leaves this
// function; a caller holding one of those would be storing the public half twice and
// would fail the length check on the way back in.
//
// The length is checked against the registry's NsigPriv rather than against
// ed25519.SeedSize, so the refusal states the suite's own contract rather than this
// primitive's. That is the whole of what it buys, and it is worth being exact about which
// way the safety runs: the registry is the weaker of the two gates here, not the stronger.
// A suite naming a 64 byte private key would pass this check with a 64 byte key and panic
// inside ed25519.NewKeyFromSeed — measured, "ed25519: bad seed length: 64" — where the
// literal would have refused it. What keeps that unreachable is not this line but
// TestEverySuiteNamesTheSignatureSchemeTheProviderComputes, which holds every registered
// suite to ed25519 and to 32 and 32.
func (self *suiteCryptoProvider) SignWithLabel(priv SignaturePrivateKey, label string, content []byte) ([]byte, error) {
	if len(priv) != self.params.NsigPriv {
		return nil, ErrBadSignatureKey
	}
	if err := checkLabelledConstruction("signature content", label, content); err != nil {
		return nil, err
	}
	return ed25519.Sign(ed25519.NewKeyFromSeed(priv), mlsSignContent(label, content)), nil
}

// The inverse, where a failure is always an error and never a logged warning (spec A
// section 5.9, guardrail 7). The caller has no branch to take other than rejecting the
// message, and there is no fallback preimage: a verify that tried the bare label, the
// unframed concatenation or the content itself after this one would accept a signature
// minted for another purpose, which is the whole thing the label exists to prevent.
//
// A wrong key length is ErrBadSignatureKey and a wrong signature length is
// ErrCryptoBadSignature. That split is deliberate. The key is this side's own
// configuration and the length of it is a local bug worth naming; the signature arrived
// from the network, and how it failed is not something an attacker gets to learn.
//
// The key length gate is load bearing: crypto/ed25519.Verify panics on a public key of
// any other length rather than answering. It reads the registry for the same reason
// SignWithLabel's does and with the same caveat — a suite naming 64 would pass it and
// panic in the library, measured, so what makes the gate sufficient is the registered
// suites being held to 32 rather than anything this line does.
//
// The signature length gate is not load bearing, and that is worth writing down rather
// than leaving for the next reader to rediscover. The library refuses every length but 64
// as the first statement of its own verify, so removing this one changes no answer —
// measured over 10260 lengths and bodies from 0 to 512 bytes, 0 disagreements. It stays
// because the contract it states is this package's rather than the library's, and because
// it is the line that would still be right if the suite's signature scheme ever stopped
// being ed25519. TestTheSignatureMethodsAreOnlyTheirOwnPreimage is what holds it there,
// since no input can.
//
// ErrCryptoBadSignature rather than ErrBadSignature: the bare name is errors.go's
// ValSem010, and errors.go wraps this one, so a framing caller can ask either question.
func (self *suiteCryptoProvider) VerifyWithLabel(pub SignaturePublicKey, label string, content []byte, sig []byte) error {
	if len(pub) != self.params.NsigPub {
		return ErrBadSignatureKey
	}
	if len(sig) != ed25519.SignatureSize {
		return ErrCryptoBadSignature
	}
	if err := checkLabelledConstruction("signature content", label, content); err != nil {
		return err
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), mlsSignContent(label, content), sig) {
		return ErrCryptoBadSignature
	}
	return nil
}

// A fresh signature key pair, seeded from this provider's own source.
//
// The seed is read from self.random rather than from crypto/rand, which is what lets a
// test assert that the bytes offered are the bytes used, in order, and lets an interop
// failure reproduce byte for byte. ed25519.GenerateKey would be the obvious call and is
// deliberately not made: it substitutes crypto/rand.Reader for a nil reader, so a provider
// built over a caller's source that turned out to be nil would silently generate from the
// operating system and the pinned case would stop reproducing. Reading here means a nil
// source fails loudly instead.
//
// A short or failing source is an error rather than a panic, because this method has an
// error return where Random does not. What it must never be is a key: a seed assembled
// from a partial read is a tail of zeroes nobody chose, and it would sign and verify
// perfectly.
//
// The public half is copied out of the expanded key rather than sliced from it. No input
// can see the difference — measured over 65536 key pairs: the bytes agree, two calls never
// share storage either way, and neither form aliases the seed. What a slice would do is
// keep the whole 64 byte expanded key reachable from the public key, and its first 32 bytes
// are the secret seed, so a caller holding only a public key would be holding a private one
// as well. TestTheSignatureMethodsAreOnlyTheirOwnPreimage is what holds it, by reading this
// statement rather than by asking the answer a question it cannot hear.
//
// The private half is the seed buffer and not expanded[:32], which is a difference an input
// can see even though the bytes never differ: a window onto the expanded key has room for
// 32 more, so a caller appending to what it was told is a private key writes over the public
// half of the same pair. TestSignatureKeyPairAnswersStorageOfItsOwn is that assertion.
func (self *suiteCryptoProvider) SignatureKeyPair() (SignaturePrivateKey, SignaturePublicKey, error) {
	seed := make([]byte, self.params.NsigPriv)
	if _, err := io.ReadFull(self.random, seed); err != nil {
		return nil, nil, err
	}
	expanded := ed25519.NewKeyFromSeed(seed)
	return SignaturePrivateKey(seed), SignaturePublicKey(bytes.Clone(expanded[ed25519.SeedSize:])), nil
}

// The public half of a signature key pair this package was HANDED rather than one it drew.
//
// It lives here, in one of the four files doc.go names as the whole cryptographic surface,
// and not beside its caller. ed25519.NewKeyFromSeed is a cryptographic operation whatever
// file it is written in, and the sentence doc.go makes -- that an audit reads four files
// and a test substitutes a deterministic provider for all of it at once -- stops being true
// the moment a fifth file expands a private key. The plan for p5 task 6 writes this
// function into leaf_node.go; that placement is the only part of it not followed.
//
// It is not a CryptoProvider method, and that is the other half of the placement. The
// interface is pinned by a gate that reads it off the type, every stub and wrapper in the
// test tree writes out every method by hand, and a plan that has not landed yet compiles
// against the set as it stands. A package level derivation reaches the same primitive
// without moving that surface.
//
// The length is checked against ed25519.SeedSize rather than against a suite's NsigPriv,
// because there is no suite here to read: the caller holds a CryptoProvider and this is not
// one of its methods. That is the weaker of the two statements and it is written down
// rather than glossed -- what makes it sufficient is
// TestEverySuiteNamesTheSignatureSchemeTheProviderComputes holding every registered suite to
// ed25519 and to 32 and 32. The check itself is load bearing whichever constant it reads:
// ed25519.NewKeyFromSeed PANICS on any other length, and a constructor handed a truncated
// key would take the process down rather than refusing.
//
// The public half is COPIED out of the expanded key rather than sliced from it, for the
// reason SignatureKeyPair gives: a window onto the expanded key keeps the secret seed --
// its first 32 octets -- reachable from something the caller was told is a public key.
func signaturePublicKeyOf(priv SignaturePrivateKey) (SignaturePublicKey, error) {
	if len(priv) != ed25519.SeedSize {
		return nil, ErrBadSignatureKey
	}
	expanded := ed25519.NewKeyFromSeed(priv)
	return SignaturePublicKey(bytes.Clone(expanded[ed25519.SeedSize:])), nil
}

// struct { opaque label<V>; opaque context<V> } EncryptContext, serialized, with the
// "MLS 1.0 " prefix on the label.
//
// It builds the same bytes mlsSignContent would build for the same two fields, and it is
// written out rather than delegating to it. RFC 9420 declares SignContent in section 5.1.2
// and EncryptContext in section 5.1.3 as two structures which happen to have the same
// shape; nothing binds them to stay that way, and an alias would make a revision to either
// silently a revision to both.
//
// The two length prefixes carry the same argument they carry there. Without them a label
// ending where the context begins and a label one byte longer serialize to the same run of
// bytes, so a message sealed for one purpose opens as a message for another under a context
// the sender never chose.
//
// No error return, by the interface spec A section 3.3 fixes on the two callers below. See
// mlsLabelBytes for why the writer cannot fail here.
func mlsEncryptContext(label string, context []byte) []byte {
	writer := syntax.NewWriter()
	writer.WriteOpaque([]byte(MlsLabelPrefix + label))
	writer.WriteOpaque(context)
	return mlsLabelBytes(writer)
}

// EncryptWithLabel, RFC 9420 section 5.1.3: the EncryptContext is the hpke info and the
// aead aad is empty.
//
// Which of the two the context travels in is the whole of this function, and no round trip
// can see it. MLS binds the context through the key schedule, so a construction that sealed
// it into aad instead encrypts and decrypts against itself perfectly, matches the published
// message in no corpus and talks to no peer alive. The published crypto-basics message is
// what separates the two, and TestProviderHpkeSealUsesAnEmptyAadForLabelledEncryption walks
// the other placement from a peer's side.
//
// The provider is a parameter rather than a receiver because the tree and the framing
// layers hold the interface rather than the concrete type, which is the same reason RefHash
// takes one. Reaching for a provider of its own here would agree with every corpus in this
// package, since they are all the suite it would have hardcoded.
//
// The two results are the encapsulated key and the ciphertext, in that order, and both are
// []byte, so a transposed return compiles. What separates them is Nenc against the
// plaintext plus Nt, which the round trip test asserts rather than assumes.
//
// The flat pair rather than an *HpkeCiphertext: that shape belongs beside the TreeKEM type
// it would return, and this package declares no TreeKEM types.
func EncryptWithLabel(crypto CryptoProvider, pub HpkePublicKey, label string, context []byte, plaintext []byte) ([]byte, []byte, error) {
	if crypto == nil {
		return nil, nil, fmt.Errorf("%w: the seal is the provider's HPKE", ErrNilCryptoProvider)
	}
	if err := checkLabelledConstruction("encryption context", label, context); err != nil {
		return nil, nil, err
	}
	return crypto.HpkeSeal(pub, mlsEncryptContext(label, context), nil, plaintext)
}

// DecryptWithLabel, the inverse, where a failure is always an error and never a plaintext
// (spec A section 5.9, guardrail 7).
//
// There is no fallback info. An open that retried with no info, with the bare label or with
// the unframed concatenation after this one would accept a message sealed for another
// purpose, which is the whole thing the label exists to prevent — and it is invisible from
// the sending side, because a receiver that retries still opens the message that was sealed
// correctly. TestDecryptWithLabelRefusesCiphertextsSealedUnderAnyOtherContext walks it from
// the side an attacker picks instead.
//
// The nil plaintext on the failure path is HpkeOpen's and is load bearing there for the
// reason AeadOpen records: an authentic empty message also comes back as a nil slice, so a
// caller reading the slice rather than the error accepts every forgery of exactly tag
// length.
func DecryptWithLabel(crypto CryptoProvider, priv HpkePrivateKey, label string, context []byte, kemOutput []byte, ciphertext []byte) ([]byte, error) {
	if crypto == nil {
		return nil, fmt.Errorf("%w: the open is the provider's HPKE", ErrNilCryptoProvider)
	}
	if err := checkLabelledConstruction("encryption context", label, context); err != nil {
		return nil, err
	}
	return crypto.HpkeOpen(priv, kemOutput, mlsEncryptContext(label, context), nil, ciphertext)
}
