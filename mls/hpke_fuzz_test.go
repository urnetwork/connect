// Gate 4 property 1 over the hpke surface: attacker chosen bytes in a kem output, an
// info, an aad, a ciphertext, an ikm or an export length must be refused with one of
// this package's own sentinels — never accepted, never coerced, never a panic, and never
// an allocation sized from a length nobody bounded.
//
// What continuous integration runs is the seed corpus and nothing else. test.yml runs
// go test with no -fuzz, and f.Fuzz explores nothing beyond its seeds without it, so
// under continuous integration the three targets here are a table of exactly the inputs
// f.Add was given. That is the reason the seeds are generated as a field by alteration
// cross product over the published vectors rather than hand picked, and the reason every
// seed carries the outcome it is expected to produce instead of only being fed in. The
// mutation search these targets are shaped for is a -fuzz run somebody starts on purpose;
// the table is what they are worth the rest of the time.
//
// Random bytes do not reach the property on their own. p1 task 14 measured uniform random
// input reaching the round trip 14 times in 4096 — 0.34% — against the simplest wire type
// there is, and the open path here is far worse: a kem output has to be exactly Nenc bytes
// and a ciphertext has to carry a tag that verifies under a key derived from that kem
// output, which no mutator finds. So the seeds are the published vectors and near misses
// of them, and each target counts how far its inputs actually got and refuses to report
// success at zero. That count is the interface registry's requirement for p8's nine
// targets and this is the first place it is implemented. Without it a target whose seeds
// all bounce off the first length gate reports green having never once opened a message.
//
// Decode robustness here is four claims rather than one:
//
//   - a refusal is typed. Every error out of the open path has to be a sentinel
//     crypto_errors.go declares, so a raw crypto/ecdh, crypto/hkdf or aead error reaching
//     a caller is a failure rather than a detail.
//   - the gates fire in the order hpkeDecap fixes. A kem output of the wrong length is
//     ErrBadKemOutput whatever else is also wrong, and only past that gate is a malformed
//     recipient key ErrBadKeyLength. Both are asserted against every input rather than
//     against a table, so they hold for inputs nobody wrote down.
//   - an input the corpus never recorded must never open. That is the claim no panic
//     cannot make: a decoder that accepted everything satisfies no panic perfectly, and
//     only a success that has to be a published or recorded message catches it.
//   - a length nobody bounded is refused before it is allocated. crypto/hkdf.Expand dies
//     on a negative length rather than refusing it, and Export's is the one length in this
//     surface that comes from a caller, which is what the third target is for.
//
// The hpke inputs carry no length prefix — every length here is fixed by the ciphersuite,
// or is the message's own — so truncated, over long and zero length at every length
// prefixed field lands as: every field is offered empty, one byte short, one byte long,
// doubled and grown to 8192 bytes, and every field is also offered at exactly the length
// the suite fixes with the wrong bytes in it, which is where a length only check passes
// and a real decoder refuses.
//
// The oracle is RFC 9180's published corpus and not a second decoder written here, so
// there is no reference implementation for this file and hpke.go to be wrong together
// with. A seed either replays bytes the RFC published or is a named alteration of them,
// and the outcome each alteration is expected to produce is written down from the
// alteration itself rather than computed by running the implementation and recording
// whatever came back.
//
// One assertion is deliberately absent. A nil plaintext returned with a nil error is not a
// defect: both registered aeads return a nil slice for a zero length plaintext, so a
// correctly sealed empty message opens as exactly that pair, and a target asserting
// against the shape fails on correct code. The corpus carries such a message for both
// suites; what is asserted over it is the plaintext it has to equal, which is the claim
// that was wanted.
//
// What the corpus alters is every message it records as opening, and the messages it
// records are a cross product rather than a list. A recorded success nobody alters is a
// success no seed can tell from an unconditional accept: with the published 45 byte
// message as the only altered one, an open that hands back an empty plaintext for any
// ciphertext of exactly the tag length — an authentication bypass for every empty message
// — is refused by nothing here. And an alteration reaches a lenient fallback only if the
// message it alters can provoke one: an open that drops its aad binding above a thousand
// bytes, or that accepts an aad claimed over a message sealed with none, is invisible to a
// corpus whose one message is 45 bytes and carries both fields. So the basis is sealed at
// each of three plaintext lengths under each of nine info and aad pairings, and the field
// by alteration cross product runs over all of them rather than over one of them.
package mls

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"
)

// How many times the alteration set is applied to what it produced, in the two targets
// whose claims are about pairs of inputs rather than about one.
//
// One round is near misses of a published value; a second round is what puts two inputs in
// every length band, which is what an injectivity or a distinctness claim has to compare.
// A third would multiply the corpus again for bands that are already occupied.
const hpkeFuzzAlterationRounds = 2

// The narrowest exported value two different exporter contexts are required to differ in.
//
// Sixteen bytes because that is where a coincidence stops being plausible: a search
// comparing ten million inputs against the few thousand values held below runs a collision
// probability around two in a billion at this width and around one at four bytes. It is a
// property of the arithmetic rather than of the ciphersuite, so it is written here rather
// than read from Nh or Nt, both of which happen to sit near it.
const hpkeFuzzExportCollisionWidth = 16

// The most pairs the two collision maps hold.
//
// A seed run fills them with the whole corpus and stops well short of this. A -fuzz run
// would otherwise add an entry per execution and take the worker's memory with it, so past
// this many the claim is made against the inputs already held rather than against every
// input a search has ever produced — which still fails on any collapse that lands on one
// of them, and which is the whole corpus in the run continuous integration performs.
const hpkeFuzzDerivationsTracked = 4096

// The over long length every field is grown to. Large enough that an implementation
// sizing a buffer from it would be visible against the few hundred bytes everything else
// here runs at, small enough that a -fuzz run mutating a corpus full of them still
// executes at a useful rate.
const hpkeFuzzOverlongLength = 8192

// The bytes an over long info, an over long aad and an over long plaintext are sealed
// under.
//
// Three different fillers rather than one, and none of them the 0x5a the "grown"
// alteration writes, so that growing one of these fields produces bytes it did not already
// hold. An alteration that leaves a field equal to what it was is the unaltered message
// under another name, and the corpus would then be asserting a refusal against an input
// that has to open.
var (
	hpkeFuzzOverlongInfo      = bytes.Repeat([]byte{0xa5}, hpkeFuzzOverlongLength)
	hpkeFuzzOverlongAad       = bytes.Repeat([]byte{0x3c}, hpkeFuzzOverlongLength)
	hpkeFuzzOverlongPlaintext = bytes.Repeat([]byte{0xc3}, hpkeFuzzOverlongLength)
)

// One recipient the open target's selector byte picks between: a suite, the private key an
// open is attempted with, and whether that key is the length its suite fixes. The last
// field is what lets the fuzz body state the private key gate as a rule over every input
// rather than as a row in a table.
type hpkeFuzzRecipient struct {
	name             string
	params           *SuiteParams
	priv             HpkePrivateKey
	privIsWellFormed bool
}

// The recipients the open target selects between: the published recipient key of every
// registered suite, then three malformed forms of the first, then one key that is
// malformed only relative to the suite it is offered under.
//
// The malformed ones are here because HpkeOpenBase's private key is not a fuzz argument —
// it is this process's own key rather than a peer's bytes — so the gate that refuses it is
// unreachable from a signature that fuzzes only the wire fields, and a target that cannot
// reach a gate says nothing about it. They are cut from a published key rather than
// invented, so the only thing wrong with any of them is its length.
//
// The three cut keys reach that gate but cannot say it answered. X25519PrivateKey refuses
// the same three lengths with the same sentinel one line further down, so deleting
// hpkeDecap's own check leaves every one of them producing ErrBadKeyLength anyway, and a
// check placed where it cannot observe its subject is worth nothing. The last recipient is
// what separates them: a whole published key under a SuiteParams whose Nsk is not the
// curve's length, which the gate refuses because it reads the registry and which the curve
// accepts because it reads RFC 7748. Delete the gate and that message opens, so the
// refusal asserted over it is the gate's and nothing else's.
//
// Widening rather than narrowing, and widening the field alone: no other length in the
// open path is Nsk, and every other field of the probe is the registered suite's, so an
// input offered under it reaches exactly the one gate under test and is answered by
// nothing downstream that happens to also refuse it.
func hpkeFuzzRecipients(tb testing.TB, vectors []hpkeVector) []hpkeFuzzRecipient {
	tb.Helper()
	recipients := make([]hpkeFuzzRecipient, 0, len(vectors)+3)
	for _, vector := range vectors {
		params := suiteForHpkeVector(tb, vector)
		recipients = append(recipients, hpkeFuzzRecipient{
			name:             "the published recipient key for " + params.Name,
			params:           params,
			priv:             HpkePrivateKey(decodeVectorField(tb, vector.name, "skRm", vector.SkRm)),
			privIsWellFormed: true,
		})
	}
	if len(recipients) == 0 {
		tb.Fatalf("the vector corpus yielded no recipient, so every case below would be about nothing")
	}
	whole := recipients[0]
	for _, malformed := range []struct {
		name string
		priv HpkePrivateKey
	}{
		{name: "a recipient key one byte short", priv: HpkePrivateKey(bytes.Clone(whole.priv[:len(whole.priv)-1]))},
		{name: "a recipient key one byte long", priv: HpkePrivateKey(append(bytes.Clone(whole.priv), 0x00))},
		{name: "an empty recipient key", priv: nil},
	} {
		recipients = append(recipients, hpkeFuzzRecipient{
			name:             malformed.name,
			params:           whole.params,
			priv:             malformed.priv,
			privIsWellFormed: false,
		})
	}
	widenedNsk := *whole.params
	widenedNsk.Nsk = whole.params.Nsk + 16
	recipients = append(recipients, hpkeFuzzRecipient{
		name:             fmt.Sprintf("a whole recipient key under a suite whose Nsk is %d", widenedNsk.Nsk),
		params:           &widenedNsk,
		priv:             whole.priv,
		privIsWellFormed: false,
	})
	return recipients
}

// One named alteration of a field, carrying the two properties the expected outcome is
// read from: whether it leaves the length alone, and whether what it leaves behind is all
// zero bytes. Nothing here consults the implementation — what an alteration produces is a
// statement about the ciphersuite and the curve, written down once in
// hpkeFuzzAlteredOutcome.
type hpkeFuzzAlteration struct {
	name         string
	apply        func(bs []byte) []byte
	keepsLength  bool
	leavesZeroes bool
}

// Every alteration the corpus applies to every field: the three length classes — zero
// length, truncated, over long, the last of those twice at different scales — and then
// three that move a byte without moving the length, which is where a length only check
// passes and a real decoder has to refuse.
//
// These are crossed with the fields rather than chosen per field. A hand written list is
// how a corpus ends up exercising one field five ways and another once, and the field that
// got one is always the one nobody thought about.
//
// Four of them hand back their input unchanged when it is empty, and every caller drops an
// alteration that changed nothing. An empty field is not hypothetical here: the message
// basis seals under an absent info and an absent aad on purpose, since that is the only
// way the corpus can offer one of them over a message that carried none. Indexing the
// first or the last byte of such a field would be an index out of range inside a seed
// constructor, which reads as a broken test binary rather than as a corpus that outgrew
// its helpers, and emptying an already empty field would produce the unaltered message
// with a refusal written beside it.
func hpkeFuzzAlterations() []hpkeFuzzAlteration {
	return []hpkeFuzzAlteration{
		{
			name:  "emptied",
			apply: func(bs []byte) []byte { return nil },
		},
		{
			name: "one byte short",
			apply: func(bs []byte) []byte {
				if len(bs) == 0 {
					return nil
				}
				return bytes.Clone(bs[:len(bs)-1])
			},
		},
		{
			name:  "one byte long",
			apply: func(bs []byte) []byte { return append(bytes.Clone(bs), 0x00) },
		},
		{
			name:  "doubled",
			apply: func(bs []byte) []byte { return append(bytes.Clone(bs), bs...) },
		},
		{
			name:  "grown",
			apply: func(bs []byte) []byte { return bytes.Repeat([]byte{0x5a}, hpkeFuzzOverlongLength) },
		},
		{
			name:         "zeroed",
			apply:        func(bs []byte) []byte { return make([]byte, len(bs)) },
			keepsLength:  true,
			leavesZeroes: true,
		},
		{
			name: "with the low bit of its first byte flipped",
			apply: func(bs []byte) []byte {
				if len(bs) == 0 {
					return nil
				}
				altered := bytes.Clone(bs)
				altered[0] ^= 0x01
				return altered
			},
			keepsLength: true,
		},
		{
			name: "with the high bit of its last byte flipped",
			apply: func(bs []byte) []byte {
				if len(bs) == 0 {
					return nil
				}
				altered := bytes.Clone(bs)
				altered[len(altered)-1] ^= 0x80
				return altered
			},
			keepsLength: true,
		},
	}
}

// What an altered field has to produce, stated from the alteration rather than read off a
// run of the implementation.
//
// A field whose length the ciphersuite fixes is refused the moment its length moves, and
// hpkeDecap refuses the kem output ahead of everything else — a precedence
// TestHpkeDecapRejectsWrongLengths pins separately. An all zero kem output of exactly the
// right length is the low order point case: the diffie-hellman produces an all zero shared
// secret, crypto/ecdh refuses it, and that refusal is the whole reason guardrail 3 exists,
// since the banned sdk helper returns it successfully. Every other alteration leaves a
// message that decapsulates to a key schedule the sender never used, or a ciphertext whose
// tag no longer verifies over the aad it now claims, and both of those are one sentinel:
// which of them a peer got wrong is not something the peer gets to learn.
//
// The high bit of the last byte of a kem output is the interesting row. RFC 7748 has
// x25519 mask that bit off, so the diffie-hellman is unchanged by flipping it — but DHKEM
// puts the received encapsulated key into kem_context verbatim, so the shared secret moves
// anyway and the message is refused. An implementation that hashed its own reserialization
// of the point instead would accept the altered bytes, which is a malleable wire format.
func hpkeFuzzAlteredOutcome(suiteFixesLength bool, alteration hpkeFuzzAlteration) error {
	switch {
	case suiteFixesLength && !alteration.keepsLength:
		return ErrBadKemOutput
	case suiteFixesLength && alteration.leavesZeroes:
		return ErrInvalidPoint
	default:
		return ErrAeadOpen
	}
}

// One input of the open corpus with the outcome it is expected to produce: either a
// plaintext it has to open to, or the sentinel it has to be refused with. name is what a
// failure prints, so it says which field was altered and how rather than which index of
// the corpus disagreed.
type hpkeFuzzOpenCase struct {
	name          string
	recipient     int
	kemOutput     []byte
	info          []byte
	aad           []byte
	ciphertext    []byte
	wantOpen      bool
	wantPlaintext []byte
	wantErr       error
}

// One named value a field of the message basis is sealed with: a plaintext length, an
// info, or an aad.
//
// Three classes for each rather than one value, because a length gate and an absent field
// are the two cheapest places for a lenient decoder to hide. The published plaintext is
// the known answer; the empty one is the input that separates a successful open from a
// refusal in the one place the two look alike, since both hand back a nil slice and no
// published vector seals one; the over long one is what puts a ciphertext that has to open
// above any length an implementation could gate a fallback on.
type hpkeFuzzValueClass struct {
	name  string
	bytes []byte
}

// One info and aad pairing the message basis is sealed under.
//
// Absent, published and over long for each of the two, crossed rather than listed. Absent
// is the half the published corpus cannot supply and the half that matters most: both
// vendored entries carry a non-empty info and a non-empty aad, so an alteration of them
// can only change a value that was already there and never claim one over a message that
// carried none — which is exactly the direction an open with a lenient fallback survives.
// Over long is the other half: a fallback an implementation gated on a length is reachable
// only from a message sealed above that length.
type hpkeFuzzBindingClass struct {
	name string
	info []byte
	aad  []byte
}

// One sealed message the corpus offers and then alters: the four wire fields as a peer
// would send them, the suite they belong to, and the plaintext the unaltered form has to
// open to.
//
// isPublished separates the one message of each suite whose bytes RFC 9180 printed from
// the rest of the basis, which this package sealed itself. The published one is a known
// answer; the others are not, and nothing about their bytes is asserted beyond the
// plaintext they were sealed over and the encapsulated key they had to reproduce.
type hpkeFuzzSealed struct {
	name        string
	suite       int
	params      *SuiteParams
	kemOutput   []byte
	info        []byte
	aad         []byte
	ciphertext  []byte
	plaintext   []byte
	isPublished bool
}

// The whole message basis: for every registered suite, the published single shot message,
// and then that suite's own seal of every plaintext class under every binding class.
//
// The first encryption is the one a single shot open can reach: HpkeOpenBase builds a
// receiving context at sequence zero and opens exactly one message with it, so the
// published encryptions past index zero belong to the context tests and not here.
//
// Everything past the published message is sealed from the vector's own ephemeral private
// key rather than from a random reader, so the corpus is the same bytes on every run and a
// crasher found against it is reproducible. That the seal reproduced the published
// encapsulated key is checked each time, since a fixed reader that stopped being the
// ephemeral key would silently turn the whole basis into messages for a different context.
//
// One member of the basis is the published message rather than a near miss of it: the
// published plaintext under the published info and aad is what the vector already printed,
// and sealing it here has to reproduce those bytes. It is left in the cross product rather
// than skipped, and the identity is asserted rather than deduplicated away, because that
// makes the one message this file seals under known conditions a known answer instead of a
// collision the corpus quietly drops.
func hpkeFuzzSealedMessages(tb testing.TB, vectors []hpkeVector) []hpkeFuzzSealed {
	tb.Helper()
	sealed := []hpkeFuzzSealed{}
	for index, vector := range vectors {
		if len(vector.Encryptions) == 0 {
			tb.Fatalf("%s carries no encryptions, so there is no published message to open", vector.name)
		}
		first := vector.Encryptions[0]
		params := suiteForHpkeVector(tb, vector)
		skEm := decodeVectorField(tb, vector.name, "skEm", vector.SkEm)
		pkRm := decodeVectorField(tb, vector.name, "pkRm", vector.PkRm)
		published := hpkeFuzzSealed{
			name:        "the published single shot message for " + params.Name,
			suite:       index,
			params:      params,
			kemOutput:   decodeVectorField(tb, vector.name, "enc", vector.Enc),
			info:        decodePossiblyEmptyVectorField(tb, vector.name, "info", vector.Info),
			aad:         decodePossiblyEmptyVectorField(tb, vector.name, "encryptions[0].aad", first.Aad),
			ciphertext:  decodeVectorField(tb, vector.name, "encryptions[0].ct", first.Ct),
			plaintext:   decodeVectorField(tb, vector.name, "encryptions[0].pt", first.Pt),
			isPublished: true,
		}
		sealed = append(sealed, published)

		plaintexts := []hpkeFuzzValueClass{
			{name: "the published plaintext", bytes: published.plaintext},
			{name: "an empty plaintext", bytes: nil},
			{name: "an over long plaintext", bytes: hpkeFuzzOverlongPlaintext},
		}
		infos := []hpkeFuzzValueClass{
			{name: "no info", bytes: nil},
			{name: "the published info", bytes: published.info},
			{name: "an over long info", bytes: hpkeFuzzOverlongInfo},
		}
		aads := []hpkeFuzzValueClass{
			{name: "no aad", bytes: nil},
			{name: "the published aad", bytes: published.aad},
			{name: "an over long aad", bytes: hpkeFuzzOverlongAad},
		}
		bindings := []hpkeFuzzBindingClass{}
		for _, info := range infos {
			for _, aad := range aads {
				bindings = append(bindings, hpkeFuzzBindingClass{
					name: info.name + " and " + aad.name,
					info: info.bytes,
					aad:  aad.bytes,
				})
			}
		}

		for _, plaintext := range plaintexts {
			for _, binding := range bindings {
				kemOutput, ciphertext, err := HpkeSealBase(
					bytes.NewReader(skEm), params, HpkePublicKey(pkRm), binding.info, binding.aad, plaintext.bytes)
				if err != nil {
					tb.Fatalf("sealing %s under %s for %s: %v", plaintext.name, binding.name, params.Name, err)
				}
				if !bytes.Equal(kemOutput, published.kemOutput) {
					tb.Fatalf("sealing %s under %s for %s encapsulated %x, want the published %x: the fixed reader was not the ephemeral key",
						plaintext.name, binding.name, params.Name, kemOutput, published.kemOutput)
				}
				if bytes.Equal(plaintext.bytes, published.plaintext) &&
					bytes.Equal(binding.info, published.info) && bytes.Equal(binding.aad, published.aad) &&
					!bytes.Equal(ciphertext, published.ciphertext) {
					tb.Fatalf("sealing the published plaintext under the published info and aad for %s produced %x, want the published %x",
						params.Name, ciphertext, published.ciphertext)
				}
				sealed = append(sealed, hpkeFuzzSealed{
					name:       fmt.Sprintf("%s sealed by this package under %s for %s", plaintext.name, binding.name, params.Name),
					suite:      index,
					params:     params,
					kemOutput:  kemOutput,
					info:       binding.info,
					aad:        binding.aad,
					ciphertext: ciphertext,
					plaintext:  plaintext.bytes,
				})
			}
		}
	}
	if len(sealed) == 0 {
		tb.Fatalf("the vector corpus yielded no message, so every case below would be about nothing")
	}
	return sealed
}

// The whole open corpus: every message of the basis, the cross product of its four wire
// fields with every alteration, an input with nothing in any field; then, for each
// recipient key that is wrong for its suite, the published message and the published
// message with a kem output one byte short, which is the pair that pins which of the two
// length gates answers first.
//
// An alteration that left a field equal to what it was is dropped rather than recorded.
// The basis seals under an absent info and an absent aad, and emptying or truncating an
// already empty field produces the message itself — which has to open, and which would
// then be sitting in the corpus with a refusal written beside it.
func hpkeFuzzOpenCases(tb testing.TB, recipients []hpkeFuzzRecipient, messages []hpkeFuzzSealed) []hpkeFuzzOpenCase {
	tb.Helper()
	fields := []struct {
		name             string
		suiteFixesLength bool
		get              func(one hpkeFuzzOpenCase) []byte
		set              func(one *hpkeFuzzOpenCase, value []byte)
	}{
		{
			name:             "kem output",
			suiteFixesLength: true,
			get:              func(one hpkeFuzzOpenCase) []byte { return one.kemOutput },
			set:              func(one *hpkeFuzzOpenCase, value []byte) { one.kemOutput = value },
		},
		{
			name: "info",
			get:  func(one hpkeFuzzOpenCase) []byte { return one.info },
			set:  func(one *hpkeFuzzOpenCase, value []byte) { one.info = value },
		},
		{
			name: "aad",
			get:  func(one hpkeFuzzOpenCase) []byte { return one.aad },
			set:  func(one *hpkeFuzzOpenCase, value []byte) { one.aad = value },
		},
		{
			name: "ciphertext",
			get:  func(one hpkeFuzzOpenCase) []byte { return one.ciphertext },
			set:  func(one *hpkeFuzzOpenCase, value []byte) { one.ciphertext = value },
		},
	}

	cases := []hpkeFuzzOpenCase{}
	suitesSeen := map[int]bool{}
	for _, message := range messages {
		whole := hpkeFuzzOpenCase{
			name:          message.name,
			recipient:     message.suite,
			kemOutput:     message.kemOutput,
			info:          message.info,
			aad:           message.aad,
			ciphertext:    message.ciphertext,
			wantOpen:      true,
			wantPlaintext: message.plaintext,
		}
		cases = append(cases, whole)

		for _, field := range fields {
			for _, alteration := range hpkeFuzzAlterations() {
				value := alteration.apply(field.get(whole))
				if bytes.Equal(value, field.get(whole)) {
					continue
				}
				altered := whole
				altered.name = fmt.Sprintf("%s, %s %s", message.name, field.name, alteration.name)
				altered.wantOpen = false
				altered.wantPlaintext = nil
				altered.wantErr = hpkeFuzzAlteredOutcome(field.suiteFixesLength, alteration)
				field.set(&altered, value)
				cases = append(cases, altered)
			}
		}

		if !suitesSeen[message.suite] {
			suitesSeen[message.suite] = true
			cases = append(cases, hpkeFuzzOpenCase{
				name:      "every field empty for " + message.params.Name,
				recipient: message.suite,
				wantErr:   ErrBadKemOutput,
			})
		}
	}

	for index, recipient := range recipients {
		if recipient.privIsWellFormed {
			continue
		}
		message := messages[0]
		cases = append(cases,
			hpkeFuzzOpenCase{
				name:       "the published message under " + recipient.name,
				recipient:  index,
				kemOutput:  message.kemOutput,
				info:       message.info,
				aad:        message.aad,
				ciphertext: message.ciphertext,
				wantErr:    ErrBadKeyLength,
			},
			hpkeFuzzOpenCase{
				name:       "a kem output one byte short under " + recipient.name,
				recipient:  index,
				kemOutput:  bytes.Clone(message.kemOutput[:len(message.kemOutput)-1]),
				info:       message.info,
				aad:        message.aad,
				ciphertext: message.ciphertext,
				wantErr:    ErrBadKemOutput,
			})
	}
	return cases
}

// The corpus key of one input: the recipient the selector resolved to and then every
// field, each with its length written ahead of its bytes so no two different inputs can
// spell the same key by moving a boundary between two of them.
//
// The recipient is the resolved index rather than the selector byte, so an input the
// fuzzer produced by moving the selector to another byte that lands on the same recipient
// is recognised as the input it is.
func hpkeFuzzOpenKey(recipient int, kemOutput []byte, info []byte, aad []byte, ciphertext []byte) string {
	return fmt.Sprintf("%d|%d:%x|%d:%x|%d:%x|%d:%x",
		recipient, len(kemOutput), kemOutput, len(info), info, len(aad), aad, len(ciphertext), ciphertext)
}

// Whether a refusal is one this package declares.
//
// The list is every sentinel the open path can reach and nothing else: ErrBadSignatureKey
// and ErrCryptoBadSignature belong to a different primitive and one of them appearing here
// would mean an open had wandered into it, and an error from outside crypto_errors.go
// altogether means a crypto/ecdh, crypto/hkdf or aead error is being handed to a caller
// who can neither read it nor act on it. It is deliberately narrower than the file it is
// drawn from, so a sentinel added later fails here until somebody decides an open may
// return it.
func hpkeFuzzIsTypedRefusal(err error) bool {
	for _, sentinel := range []error{
		ErrBadKemOutput,
		ErrBadKeyLength,
		ErrInvalidPoint,
		ErrAeadOpen,
		ErrBadNonceLength,
		ErrUnknownCipherSuite,
		ErrSequenceOverflow,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

// Whether this process was asked to search the named target, rather than to run its seed
// corpus.
//
// The reachability accounting below is a statement about a seed run and only a seed run.
// Under -fuzz the body executes in worker processes that are handed whatever the
// coordinator picked, while the coordinator process — the one that runs the cleanups and
// reports the result — never executes the body at all and would therefore report zero
// reachability at the end of every successful search. Measured on go1.26.5: the
// coordinator's own cleanup sees zero inputs after two and a half million executions
// across its workers.
//
// The target's own name is what this is keyed on, because -fuzz names one target and the
// three here are accounted for separately. Reading the flag as a process wide yes or no
// would let a search of any one of them report success for the other two over a corpus
// that had stopped reaching the code under test — and a workflow that runs -fuzz once per
// target, which is the shape p1's syntax workflow already has, would do exactly that.
//
// The pattern is matched the way testing matches it: split on "/" and the first element
// treated as an unanchored regexp over the top level name. A pattern that will not compile
// is a failure the test binary reports on its own, and reporting a reachability failure
// beside it would only bury it, so it is read as a search.
func hpkeFuzzSearchRequested(target string) bool {
	requested := flag.Lookup("test.fuzz")
	if requested == nil || requested.Value.String() == "" {
		return false
	}
	pattern, _, _ := strings.Cut(requested.Value.String(), "/")
	matched, err := regexp.MatchString(pattern, target)
	if err != nil {
		return true
	}
	return matched
}

// Whether -run narrowed a target's corpus to part of itself.
//
// testing splits -run on "/" and applies the elements past the first to subtests, and every
// seed of a fuzz target is a subtest. So a pattern with a second element runs some seeds
// and not others, and the accounting below — which is a statement about the corpus as a
// whole — has nothing to say about what that run reached. Reporting five failures about
// the filter alongside the one real failure lands the cost on exactly the loop a developer
// reaches for when one seed fails.
//
// A pattern with a single element either selects a target whole or does not run it at all,
// and leaves the accounting live. That is what keeps continuous integration covered:
// test.yml passes no -run.
func hpkeFuzzCorpusNarrowed() bool {
	requested := flag.Lookup("test.run")
	return requested != nil && strings.Contains(requested.Value.String(), "/")
}

// FuzzHpkeOpenBase drives the whole receiving half — decapsulation, key schedule and aead
// open — from bytes a peer chose, and holds it to the four claims in this file's comment.
//
// The seed corpus is the coverage under continuous integration, so it is generated rather
// than listed: for each registered suite the published message and this package's own seal
// of every plaintext class under every info and aad pairing, each crossed with its four
// wire fields and eight alterations, an input with nothing in it, and the wrong recipient
// key rows. Every one of them carries the outcome it must produce, so the seed run is a
// table of known answers and not a panic watch.
//
// The counters are what stop this from passing while proving nothing. An open that never
// succeeds has evaluated no part of the success claim, and an open that never reaches the
// aead has tested two length gates and a curve; both report exactly what a healthy run
// reports unless something counts. Reaching the aead is inferred from ErrAeadOpen, which
// HpkeContext.Open is the only producer of in this package — an inference, and the weaker
// of the two counts for that reason, which is why the count that matters is the number of
// messages that actually opened to their published plaintext.
//
// Narrowing -run below the target's own name skips the accounting rather than tripping it.
// A run of one seed has not evaluated the corpus and cannot answer what the corpus
// reached, and answering anyway would put five failures about the filter alongside the one
// failure the developer narrowed the run to read.
func FuzzHpkeOpenBase(f *testing.F) {
	vectors := loadHpkeVectors(f)
	messages := hpkeFuzzSealedMessages(f, vectors)
	recipients := hpkeFuzzRecipients(f, vectors)
	cases := hpkeFuzzOpenCases(f, recipients, messages)

	// two alterations of two different messages can meet on one input — emptying the
	// ciphertext of any two messages sealed under the same info and aad spells the same
	// bytes, and this package's own seal of the published plaintext under the published
	// pairing is the published message — and such a pair is seeded once and named after the
	// first of them. What is still fatal is the two of them disagreeing about the outcome,
	// because then one of the two expectations can never be reached and the corpus is
	// asserting both.
	expected := map[string]hpkeFuzzOpenCase{}
	for _, one := range cases {
		key := hpkeFuzzOpenKey(one.recipient, one.kemOutput, one.info, one.aad, one.ciphertext)
		if previous, ok := expected[key]; ok {
			if previous.wantOpen != one.wantOpen || previous.wantErr != one.wantErr ||
				!bytes.Equal(previous.wantPlaintext, one.wantPlaintext) {
				f.Fatalf("%q and %q are the same input and disagree about what it produces",
					previous.name, one.name)
			}
			continue
		}
		expected[key] = one
		f.Add(uint8(one.recipient), one.kemOutput, one.info, one.aad, one.ciphertext)
	}

	// each suite's published message offered to the other suite's recipient, seeded
	// without an expectation on purpose. Every other seed is one the corpus recorded, so
	// the branch that refuses a success nobody wrote down would otherwise be reachable
	// only under -fuzz — which is to say, never in continuous integration. These are the
	// rows that keep it live: what holds them is the rule that an input the corpus never
	// recorded must not open, and nothing else.
	for _, message := range messages {
		if !message.isPublished {
			continue
		}
		for _, other := range messages {
			if !other.isPublished || other.suite == message.suite {
				continue
			}
			f.Add(uint8(other.suite), message.kemOutput, message.info, message.aad, message.ciphertext)
		}
	}

	inputs := 0
	opened := make([]int, len(recipients))
	reachedTheAead := make([]int, len(recipients))
	refusedAtTheKemOutputGate := 0
	refusedAtTheRecipientKeyGate := 0
	refusedAtTheCurve := 0

	f.Cleanup(func() {
		if hpkeFuzzSearchRequested(f.Name()) || hpkeFuzzCorpusNarrowed() {
			return
		}
		if inputs == 0 {
			f.Errorf("no input ran at all, so every claim in this target went unevaluated")
			return
		}
		for index, recipient := range recipients {
			if !recipient.privIsWellFormed {
				continue
			}
			if opened[index] == 0 {
				f.Errorf("nothing ever opened under %s, so no run reached a plaintext and the success half of this target proved nothing",
					recipient.name)
			}
			if reachedTheAead[index] == 0 {
				f.Errorf("nothing under %s ever reached the aead, so this whole run stopped at a length gate or at the curve",
					recipient.name)
			}
		}
		if refusedAtTheKemOutputGate == 0 {
			f.Errorf("no input was refused for its kem output length, so the gate this target exists to pin never fired")
		}
		if refusedAtTheRecipientKeyGate == 0 {
			f.Errorf("no input was refused for its recipient key length, so the second gate never fired")
		}
		if refusedAtTheCurve == 0 {
			f.Errorf("no input was refused by the curve, so the low order point path never ran")
		}
	})

	f.Fuzz(func(t *testing.T, selector uint8, kemOutput []byte, info []byte, aad []byte, ciphertext []byte) {
		index := int(selector) % len(recipients)
		recipient := recipients[index]
		plaintext, err := HpkeOpenBase(recipient.params, recipient.priv, kemOutput, info, aad, ciphertext)

		if err != nil && plaintext != nil {
			t.Fatalf("%s: refused with %v and handed back %d plaintext bytes anyway", recipient.name, err, len(plaintext))
		}
		if err != nil && !hpkeFuzzIsTypedRefusal(err) {
			t.Fatalf("%s: refused with %v, which is not one of this package's sentinels, so a library's error is reaching a caller",
				recipient.name, err)
		}
		switch {
		case len(kemOutput) != recipient.params.Nenc:
			if !errors.Is(err, ErrBadKemOutput) {
				t.Fatalf("a %d byte kem output produced %v, not ErrBadKemOutput: a length the ciphersuite fixes has to be refused by the length check and never by something downstream that happens to also refuse it",
					len(kemOutput), err)
			}
		case !recipient.privIsWellFormed:
			if !errors.Is(err, ErrBadKeyLength) {
				t.Fatalf("%s with a well formed kem output produced %v, not ErrBadKeyLength", recipient.name, err)
			}
		}

		want, recorded := expected[hpkeFuzzOpenKey(index, kemOutput, info, aad, ciphertext)]
		switch {
		case recorded && want.wantOpen:
			if err != nil {
				t.Fatalf("%s: refused with %v", want.name, err)
			}
			if !bytes.Equal(plaintext, want.wantPlaintext) {
				t.Fatalf("%s: opened to %x, want %x", want.name, plaintext, want.wantPlaintext)
			}
		case recorded:
			if !errors.Is(err, want.wantErr) {
				t.Fatalf("%s: error = %v, want %v", want.name, err, want.wantErr)
			}
		case err == nil:
			t.Fatalf("%s: opened %d bytes out of an input the corpus never recorded; an open that succeeds on bytes nobody sealed is an aead that is not checking its tag",
				recipient.name, len(plaintext))
		}

		inputs++
		switch {
		case err == nil:
			opened[index]++
			reachedTheAead[index]++
		case errors.Is(err, ErrAeadOpen):
			reachedTheAead[index]++
		case errors.Is(err, ErrBadKemOutput):
			refusedAtTheKemOutputGate++
		case errors.Is(err, ErrBadKeyLength):
			refusedAtTheRecipientKeyGate++
		case errors.Is(err, ErrInvalidPoint):
			refusedAtTheCurve++
		}
	})
}

// FuzzHpkeDeriveKeyPair holds the kem's key derivation to four claims over arbitrary input
// keying material: it refuses nothing, it is deterministic, it reads the kem rather than
// the rest of the suite, and it reproduces the published key pairs from the published ikm.
//
// A kdf refuses no input, so returning early on an error would be an accept anything: under
// it a derivation that failed on every ikm passes. Here an error is a failure.
//
// Deriving under both registered suites and comparing is the claim hpke.go's own comment
// makes: a key pair is a property of the kem, so the same ikm has to give the same pair
// under both, and a derivation that reached for the whole suite id instead of the kem's
// would hand the two suites different keys and stop them sharing a keystore. The published
// pairs are what pin the absolute value, so the two halves of that cannot be wrong
// together: the equality catches a derivation that reads the aead, the known answers catch
// one that is wrong the same way under both.
//
// The negative direction is asserted twice, and the second of the two is the one with
// teeth. An ikm that is not the published one must not produce the published key pair,
// which is what fails on a derivation that ignores its input and returns a constant — a
// derivation that would satisfy every length check, every determinism check and the cross
// suite equality at once. But a derivation that collapsed many ikms onto one key that is
// not a published one satisfies that too: it is deterministic, it agrees across the suites,
// its output is the right length, and it reproduces no published key. What denies it is
// injectivity — two different ikms may not derive one private key — and injectivity is a
// claim about a pair, so nothing that looks at one input at a time can make it.
func FuzzHpkeDeriveKeyPair(f *testing.F) {
	vectors := loadHpkeVectors(f)
	suites := Suites()

	type publishedPair struct {
		name string
		ikm  []byte
		priv []byte
		pub  []byte
	}
	published := []publishedPair{}
	for _, vector := range vectors {
		published = append(published, publishedPair{
			name: vector.name,
			ikm:  decodeVectorField(f, vector.name, "ikmR", vector.IkmR),
			priv: decodeVectorField(f, vector.name, "skRm", vector.SkRm),
			pub:  decodeVectorField(f, vector.name, "pkRm", vector.PkRm),
		})
	}

	// the alterations are the same cross product the open corpus uses, deduplicated
	// because several of them collapse onto one another on a single field: emptying a
	// vector and truncating a one byte one are the same seed, and adding it twice would
	// only run it twice.
	//
	// They are applied to what they produced as well as to the published ikm, which is
	// what an injectivity claim needs and what one round could not give it. One round
	// yields near misses of a published ikm and no two of them are alike past a length
	// band; a second round yields the pairs — two ikms over eight thousand bytes, two that
	// share a prefix and differ after it — that a derivation dropping part of its input
	// maps onto one key. Depth is a constant here rather than a search: each round
	// multiplies the corpus by the alteration count before the deduplication takes it back
	// down, and two rounds is where every length band the alterations name is occupied
	// twice.
	seeded := map[string]bool{}
	frontier := [][]byte{}
	add := func(ikm []byte) {
		if seeded[string(ikm)] {
			return
		}
		seeded[string(ikm)] = true
		frontier = append(frontier, ikm)
		f.Add(ikm)
	}
	for _, pair := range published {
		add(pair.ikm)
	}
	add(nil)
	add([]byte{0x00})
	add(bytes.Repeat([]byte{0xff}, 32))
	for round := 0; round < hpkeFuzzAlterationRounds; round++ {
		current := frontier
		frontier = nil
		for _, ikm := range current {
			for _, alteration := range hpkeFuzzAlterations() {
				add(alteration.apply(ikm))
			}
		}
	}

	inputs := 0
	matched := make([]int, len(published))
	derivations := map[string][]byte{}

	f.Cleanup(func() {
		if hpkeFuzzSearchRequested(f.Name()) || hpkeFuzzCorpusNarrowed() {
			return
		}
		if inputs == 0 {
			f.Errorf("no input ran at all, so every claim in this target went unevaluated")
			return
		}
		for index, pair := range published {
			if matched[index] == 0 {
				f.Errorf("no input was ever the published ikm of %s, so nothing here was held to a published key pair", pair.name)
			}
		}
	})

	f.Fuzz(func(t *testing.T, ikm []byte) {
		var priv HpkePrivateKey
		var pub HpkePublicKey
		for index, suite := range suites {
			params, err := LookupSuite(suite)
			if err != nil {
				t.Fatalf("LookupSuite(%#04x): %v", uint16(suite), err)
			}
			derivedPriv, derivedPub, err := HpkeDeriveKeyPair(params, ikm)
			if err != nil {
				t.Fatalf("%s: a %d byte ikm was refused with %v, but a kdf refuses no input", params.Name, len(ikm), err)
			}
			if len(derivedPriv) != params.Nsk || len(derivedPub) != params.Npk {
				t.Fatalf("%s: derived %d/%d bytes, want %d/%d", params.Name, len(derivedPriv), len(derivedPub), params.Nsk, params.Npk)
			}
			againPriv, againPub, err := HpkeDeriveKeyPair(params, ikm)
			if err != nil {
				t.Fatalf("%s: the second derivation from one ikm was refused with %v", params.Name, err)
			}
			if !bytes.Equal(derivedPriv, againPriv) || !bytes.Equal(derivedPub, againPub) {
				t.Fatalf("%s: one ikm derived two different key pairs, so the derivation is reading something that is not its input", params.Name)
			}
			if index == 0 {
				priv, pub = derivedPriv, derivedPub
				continue
			}
			if !bytes.Equal(priv, derivedPriv) || !bytes.Equal(pub, derivedPub) {
				t.Fatalf("%s derived a different key pair from the same ikm as %s, and the two share a kem, so the derivation is reading the aead",
					params.Name, mustSuiteName(t, suites[0]))
			}
		}

		if previous, ok := derivations[string(priv)]; ok {
			if !bytes.Equal(previous, ikm) {
				t.Fatalf("a %d byte ikm and a %d byte ikm both derived %x, so the derivation is not reading the whole of its input",
					len(previous), len(ikm), priv)
			}
		} else if len(derivations) < hpkeFuzzDerivationsTracked {
			derivations[string(priv)] = bytes.Clone(ikm)
		}

		for index, pair := range published {
			samePriv := bytes.Equal(priv, pair.priv)
			switch {
			case bytes.Equal(ikm, pair.ikm):
				if !samePriv {
					t.Fatalf("%s: the published ikm derived %x, want the published %x", pair.name, priv, pair.priv)
				}
				if !bytes.Equal(pub, pair.pub) {
					t.Fatalf("%s: the published ikm derived the public key %x, want the published %x", pair.name, pub, pair.pub)
				}
				matched[index]++
			case samePriv:
				t.Fatalf("%s: a %d byte ikm that is not the published one derived the published private key anyway, so the ikm is not reaching the derivation",
					pair.name, len(ikm))
			}
		}
		inputs++
	})
}

// The registered name of a suite, for a failure message that has to say which of two
// suites disagreed with the other.
func mustSuiteName(t *testing.T, suite CipherSuite) string {
	t.Helper()
	params, err := LookupSuite(suite)
	if err != nil {
		t.Fatalf("LookupSuite(%#04x): %v", uint16(suite), err)
	}
	return params.Name
}

// One published key schedule the export target exports from, with its published exported
// values keyed by the pair that produces them.
type hpkeFuzzExportContext struct {
	name         string
	params       *SuiteParams
	sharedSecret []byte
	info         []byte
	published    map[string][]byte
}

// The key of one export: the requested length and the exporter context, which is the whole
// of what an exported value depends on once the context is fixed.
func hpkeFuzzExportKey(length int, exporterContext []byte) string {
	return fmt.Sprintf("%d|%x", length, exporterContext)
}

// FuzzHpkeContextExport covers the one length in this surface a caller supplies, and is the
// reason a decode robustness target here is not only about wire bytes.
//
// hpkeLabeledExpand's guard is load bearing rather than defensive: crypto/hkdf.Expand opens
// with make([]byte, 0, keyLen) before any bound is checked, so a negative length is a
// makeslice panic that takes the process rather than an error a caller can handle, and a
// length above 255*Nh is the point past which there is no key to return at all. Both ends
// are asserted against the registry's own Nh rather than against the constant hpke.go
// computes from it, so a ceiling that drifted from the ciphersuite is a failure here and
// not a pair of constants agreeing with each other.
//
// The exported values are published, so the accepting direction is a known answer and not a
// length check. A guard that refused everything would satisfy every refusal in this target
// and is caught by the published rows; a guard that refused nothing panics on the first
// negative length.
//
// What the published rows cannot say is that the exporter reads the whole of its context.
// Every context RFC 9180 prints here is eleven bytes or shorter, so an exporter that
// stopped reading past some larger length reproduces all three published values and is
// refused by nothing. The corpus therefore carries the same alteration closure the derive
// target does — contexts at every length class, twice over, so that each band holds a pair
// — and the claim over them is that two different contexts do not export the same value at
// the same length.
//
// That claim holds only where the value is wide enough for a collision to be a statement
// about the exporter. A one byte export takes 256 values, so two contexts landing on one
// of them is the pigeonhole and not a defect — a search finds such a pair in seconds, and
// it did: the first bounded -fuzz run of this target produced a one byte export of two
// different contexts and reported it as a failure of the implementation. Below
// hpkeFuzzExportCollisionWidth the exported value is compared against nothing, which costs
// the claim nothing, since every published export and every context of the closure is
// seeded at thirty two bytes.
func FuzzHpkeContextExport(f *testing.F) {
	vectors := loadHpkeVectors(f)
	contexts := []hpkeFuzzExportContext{}
	for _, vector := range vectors {
		if len(vector.Exports) == 0 {
			f.Fatalf("%s carries no exported values, so nothing here would be a known answer", vector.name)
		}
		entry := hpkeFuzzExportContext{
			name:         vector.name,
			params:       suiteForHpkeVector(f, vector),
			sharedSecret: decodeVectorField(f, vector.name, "shared_secret", vector.SharedSecret),
			info:         decodePossiblyEmptyVectorField(f, vector.name, "info", vector.Info),
			published:    map[string][]byte{},
		}
		seeded := map[string]bool{}
		frontier := [][]byte{}
		lengths := []int{}
		add := func(exporterContext []byte) {
			if seeded[string(exporterContext)] {
				return
			}
			seeded[string(exporterContext)] = true
			frontier = append(frontier, exporterContext)
			for _, length := range lengths {
				f.Add(uint8(len(contexts)), length, exporterContext)
			}
		}
		for _, export := range vector.Exports {
			exporterContext := decodePossiblyEmptyVectorField(f, vector.name, "exports.exporter_context", export.ExporterContext)
			entry.published[hpkeFuzzExportKey(export.Length, exporterContext)] =
				decodeVectorField(f, vector.name, "exports.exported_value", export.ExportedValue)
			f.Add(uint8(len(contexts)), export.Length, exporterContext)
			lengths = append(lengths, export.Length)
			seeded[string(exporterContext)] = true
			frontier = append(frontier, exporterContext)
		}
		add([]byte("exporter context"))
		for round := 0; round < hpkeFuzzAlterationRounds; round++ {
			current := frontier
			frontier = nil
			for _, exporterContext := range current {
				for _, alteration := range hpkeFuzzAlterations() {
					add(alteration.apply(exporterContext))
				}
			}
		}
		// the boundaries the guard is written for: below zero, at zero, at the ceiling,
		// one past it, and the two extremes an int holds. hkdf.Expand dies rather than
		// refusing on the negative ones, so these are the rows that separate a guard from
		// a process kill.
		for _, length := range []int{math.MinInt, -1, 0, 1, 255 * entry.params.Nh, 255*entry.params.Nh + 1, math.MaxInt} {
			f.Add(uint8(len(contexts)), length, []byte("exporter context"))
			f.Add(uint8(len(contexts)), length, []byte(nil))
		}
		contexts = append(contexts, entry)
	}

	inputs := 0
	refused := 0
	produced := 0
	matched := make([]int, len(contexts))
	exported := map[string][]byte{}

	f.Cleanup(func() {
		if hpkeFuzzSearchRequested(f.Name()) || hpkeFuzzCorpusNarrowed() {
			return
		}
		if inputs == 0 {
			f.Errorf("no input ran at all, so every claim in this target went unevaluated")
			return
		}
		if refused == 0 {
			f.Errorf("no length was ever refused, so the guard between a caller's arithmetic and a makeslice panic never fired")
		}
		if produced == 0 {
			f.Errorf("no length ever produced an exported value, so a guard that refuses everything would pass this target")
		}
		for index, entry := range contexts {
			if matched[index] == 0 {
				f.Errorf("no input reached a published exported value of %s, so nothing here was held to a published byte", entry.name)
			}
		}
	})

	f.Fuzz(func(t *testing.T, selector uint8, length int, exporterContext []byte) {
		index := int(selector) % len(contexts)
		entry := contexts[index]
		ctx, err := hpkeKeySchedule(entry.params, entry.sharedSecret, entry.info)
		if err != nil {
			t.Fatalf("%s: key schedule: %v", entry.name, err)
		}
		value, err := ctx.Export(exporterContext, length)
		switch {
		case length < 0 || length > 255*entry.params.Nh:
			if !errors.Is(err, ErrBadKeyLength) {
				t.Fatalf("%s: an export of %d bytes produced %v, not ErrBadKeyLength", entry.name, length, err)
			}
			if value != nil {
				t.Fatalf("%s: an export of %d bytes was refused and returned %d bytes anyway", entry.name, length, len(value))
			}
			refused++
		default:
			if err != nil {
				t.Fatalf("%s: an export of %d bytes was refused with %v", entry.name, length, err)
			}
			if len(value) != length {
				t.Fatalf("%s: an export of %d bytes returned %d", entry.name, length, len(value))
			}
			produced++
		}
		if ctx.sequence != 0 {
			t.Fatalf("%s: an export moved the sequence number to %d, so exporting costs a message", entry.name, ctx.sequence)
		}
		if err == nil && length >= hpkeFuzzExportCollisionWidth {
			collision := fmt.Sprintf("%d|%d|%x", index, length, value)
			if previous, ok := exported[collision]; ok {
				if !bytes.Equal(previous, exporterContext) {
					t.Fatalf("%s: a %d byte exporter context and a %d byte one both exported %x at %d bytes, so the exporter is not reading the whole of its context",
						entry.name, len(previous), len(exporterContext), value, length)
				}
			} else if len(exported) < hpkeFuzzDerivationsTracked {
				exported[collision] = bytes.Clone(exporterContext)
			}
		}
		if want, ok := entry.published[hpkeFuzzExportKey(length, exporterContext)]; ok {
			if !bytes.Equal(value, want) {
				t.Fatalf("%s: the published export of %d bytes came back as %x, want %x", entry.name, length, value, want)
			}
			matched[index]++
		}
		inputs++
	})
}
