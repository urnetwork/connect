// The typed errors raised by the key schedule, the psk secret, the transcript hashes
// and the secret tree. Kept out of errors.go so the validation plan and this one never
// edit a single file during parallel waves, which is a merge conflict rather than a
// design boundary but is a real one.
//
// The three psk errors this plan once declared are absent on purpose: ErrPskNonceLength,
// ErrPskType and ErrDuplicatePsk are ValSem401, ValSem402 and ValSem403, and the
// validation plan's errors.go is the single declaration site for every ValSem sentinel.
// This file must never grow one of them back — package mls is one package, so the second
// declaration is a compile error, and a shape where both compiled would mean an
// errors.Is check somewhere in the commit path quietly stopped matching.
package mls

import (
	"errors"
	"fmt"

	"github.com/urnetwork/connect/mls/syntax"
)

var (
	// ErrSecretLength is returned when a secret handed to the key schedule is not
	// KDF.Nh bytes. A short secret expands into a valid looking epoch that no peer
	// agrees with, which surfaces later as an undecryptable message rather than as
	// the length mistake it is.
	ErrSecretLength = errors.New("mls: secret has the wrong length")

	// ErrExportLength is returned when an exporter length exceeds 255*KDF.Nh, which
	// is the most HKDF-Expand can produce.
	ErrExportLength = errors.New("mls: exporter length out of range")

	// ErrEpochErased is returned when a derivation is asked for over an epoch whose
	// secrets have already been erased. An epoch leaving PastEpochWindow has all nine
	// zeroized in place, and a derivation over KDF.Nh zero bytes is not a weak secret,
	// it is a PUBLIC one: any party can compute it with no knowledge of the group. So
	// this is a refusal rather than an answer, for the reason WelcomeSecret returns nil
	// rather than a zero secret — a caller that wrapped a recovery blob to an aged out
	// epoch's export would be wrapping it to a key the attacker also holds, and nothing
	// would report an error.
	ErrEpochErased = errors.New("mls: the epoch's secrets have been erased")

	// ErrGroupContextTrailingBytes names the condition where a serialized GroupContext
	// carries bytes after its extensions vector. syntax.Unmarshal is what enforces full
	// consumption, so this wraps that package's sentinel and a caller matching either
	// name matches the same failure. MLS signs over serialized forms, so a decoder
	// tolerating trailing bytes would accept two encodings of one object and the
	// signature would cover only one of them.
	ErrGroupContextTrailingBytes = fmt.Errorf(
		"mls: group context has trailing bytes: %w", syntax.ErrTrailingBytes)

	// ErrNilGroupContext is returned when a derivation is handed no GroupContext at
	// all. It is a typed refusal rather than the nil pointer dereference the
	// serialization would otherwise raise: syntax.Marshal receives a non nil interface
	// holding a nil pointer, MarshalMLS dereferences it, and the caller gets a panic
	// out of the syntax package naming neither the argument nor the derivation. Every
	// epoch derivation takes its context off a struct field, so an unset field is the
	// way this arrives.
	ErrNilGroupContext = errors.New("mls: no group context was supplied")

	// ErrNilCryptoProvider is returned when a construction that derives every one of its
	// secrets from a CryptoProvider is handed none. It is deliberately not ErrSecretLength,
	// which is what the secret tree's constructor used to answer here: a caller branching on
	// a length failure re-derives and re-passes the secret it supplied, which is the wrong
	// repair for an argument that was never supplied at all, and every other refusal these
	// constructors make names its own condition rather than borrowing one.
	//
	// It is the whole class's answer and not one constructor's. Every declaration of this
	// package that takes a provider and can report an error returns this before it judges any
	// other argument -- ahead of the group context check and ahead of every length, because
	// each of those reads KDF.Nh off the provider and a body that checked them first would
	// dereference the thing it is about to refuse. The six that answer no error cannot report
	// it and stop instead, which is the alternative to handing back a plausibly shaped value
	// derived from nothing. Both halves are swept by
	// TestEveryDeclarationHandedANilProviderRefusesRatherThanDereferencingIt over a class read
	// off the type checker; before it, this sentinel was returned from exactly one of the
	// twenty one declarations that take a provider and the other twenty raised a nil pointer
	// dereference.
	ErrNilCryptoProvider = errors.New("mls: no crypto provider was supplied")

	// ErrTranscriptHashLength is returned when a transcript hash is not KDF.Nh bytes.
	ErrTranscriptHashLength = errors.New("mls: transcript hash has the wrong length")

	// ErrPskCount is returned when a psk list cannot be indexed by the uint16 index
	// and count fields PSKLabel carries.
	ErrPskCount = errors.New("mls: too many psks for a uint16 count")

	// ErrSecretTreeLeafOutOfRange is returned for a leaf index outside the tree the
	// secret tree was built for.
	ErrSecretTreeLeafOutOfRange = errors.New("mls: leaf index outside the secret tree")

	// ErrSecretTreeConsumed is returned when the node secrets covering a leaf have
	// already been deleted. It is the forward secrecy property working, not a fault.
	ErrSecretTreeConsumed = errors.New("mls: secret tree node already consumed")

	// errSecretTreeDescentDidNotStoreTheTarget names the secret tree invariant that a
	// descent stores the leaf it descended to, and is what the one refusal reachable only
	// by breaking that invariant wraps.
	//
	// Unexported, because no caller should branch on it: it is wrapped alongside
	// ErrSecretTreeConsumed, so a caller asking "was this leaf already taken" still gets
	// yes. It exists so a TEST can tell takeLeafSecret's two ErrSecretTreeConsumed returns
	// apart -- they were otherwise indistinguishable, which is how the second one came to
	// be reclassified to an unrelated sentinel with every test in this package still
	// passing.
	errSecretTreeDescentDidNotStoreTheTarget = errors.New(
		"mls: the secret tree descent did not store the leaf it descended to")

	// errRatchetTypeHasNoRoot names the secret tree invariant that ratchetFor stores a root
	// for every ratchet type it admits, and is what the one refusal reachable only by
	// breaking that invariant wraps.
	//
	// Unexported for the reason errSecretTreeDescentDidNotStoreTheTarget is: no caller
	// should branch on it, because it is wrapped alongside ErrSecretTreeLeafOutOfRange and
	// a caller asking "was this ratchet type refused" still gets yes. It exists so a TEST
	// can tell ratchetFor's two refusals of an unknown ratchet type apart. Without it they
	// are indistinguishable, and the type check at the top of ratchetFor can be replaced
	// by a constant false with every test in this package still passing -- measured, on
	// the commit that added the second one.
	errRatchetTypeHasNoRoot = errors.New("mls: the secret tree stored no root for this ratchet type")

	// ErrRatchetGenerationConsumed is returned for a generation already used and erased.
	ErrRatchetGenerationConsumed = errors.New("mls: ratchet generation already consumed")

	// ErrRatchetGenerationTooFarAhead is returned when a generation is more than
	// MaxGenerationSkip beyond the ratchet head. Without that bound a forged generation
	// number in a received message is an unbounded KDF loop an attacker chooses the
	// length of.
	//
	// It is a refusal of THIS message and not of the sender: the head catches up by
	// MaxGenerationSkip before this is returned, so the next message from that sender is
	// that much nearer. A refusal that left the head where it was made the same answer the
	// answer for every later generation of the epoch.
	ErrRatchetGenerationTooFarAhead = errors.New("mls: ratchet generation too far ahead")

	// ErrRatchetExhausted is returned when a ratchet has produced generation 2^32-1 and
	// has no successor to step to.
	ErrRatchetExhausted = errors.New("mls: ratchet generation space exhausted")

	// ErrUnknownContentType is returned for a framing ContentType outside the three RFC 9420
	// section 6 registers, the reserved zero included, by every layer that meets one.
	// One declaration for one condition, rather than two sentinels a caller would have to
	// match separately: framing_errors.go states that arrangement in full and
	// TestEveryStructuralFramingErrorHasExactlyOneDeclarationSite holds it.
	//
	// secret_tree.go raises it at the ratchet lookup, where the refusal is what keeps an
	// unregistered code point off a real ratchet. A default arm there would hand it generation
	// 0 of one, so a peer could consume a leaf's handshake generations by naming a content
	// type nobody has defined, and the caller has to be able to tell that from a key that no
	// longer exists.
	//
	// framing.go raises it at the codec, off the wire, where there is no ratchet in view at
	// all: the content type selects which arm of a FramedContentAuthData the remaining bytes
	// are, and an unregistered one has no arm to put them in. That is the shape an attacker
	// chosen header arrives in, and it is refused before any ratchet is asked for -- so the
	// message says what is wrong with the code point rather than what could not be found for
	// it, and each raiser names the offending value itself.
	//
	// framing_preimage.go raises it at the section 6.3 sender data AAD, which is a third place
	// again and the one this comment was written without. An AAD is neither a decode nor a
	// lookup: nothing downstream re-reads the field, so the refusal is not what stops a bad
	// header being parsed or a bad ratchet being reached -- it is what stops a message being
	// SEALED under associated data no peer computes the same way, which arrives at the far end
	// as a decryption that did not work with nothing in it that says which field was wrong. The
	// content AAD reaches this refusal through that one rather than through a switch of its
	// own, which is the same single assembly that keeps the shared header in one place.
	//
	// framing_protect.go raises it at both ends of the section 6.3.1 PrivateMessageContent, which
	// is a fourth place and is the one that runs on a DECRYPTED body. The encoder refuses ahead
	// of writing an arm, so a plaintext with no content arm in it cannot be produced; the decoder
	// refuses a content type read off the cleartext header, which is where a peer puts one, and
	// without that refusal an unregistered arm would fall through to the application case and be
	// handed up as application data. The refusal there is not the last word on the message: the
	// AEAD has already authenticated it, so what this says is that a peer holding this epoch's
	// keys sent a structure this package will not assemble, and each raiser names the offending
	// value itself.
	ErrUnknownContentType = errors.New("mls: unregistered content type")
)
