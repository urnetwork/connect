// The RFC 9420 section 8.4 pre shared key identifier, the three validation rules that are
// about the value rather than about the proposal carrying it, and the psk_secret
// recurrence those rules stand in front of.
//
// The v1 profile refuses PreSharedKey proposals outright (spec A section 3.1), one layer
// above this one, at proposal parse. So it is fair to ask why the codec exists at all.
// It exists because a refusal you cannot parse is a refusal you cannot make correctly:
// ValSem401, ValSem402 and ValSem403 are rules ABOUT a PreSharedKeyId, and a rule cannot
// be checked against a structure the implementation declines to decode. It exists because
// the psk_secret vector family has to pass in both directions, and those vectors are not
// empty. And it exists because the empty psk secret is an input to every epoch this
// product does derive, so a defect in the non empty case is invisible from here and
// surfaces at the first interop test.
//
// Which is the reason it is written as though it ships. Nothing in this file takes the
// shortcut that the list is always empty.
package mls

import (
	"errors"
	"fmt"
	"math"

	"github.com/urnetwork/connect/mls/syntax"
)

// The three refusals this file makes are ValSem401, ValSem402 and ValSem403 in the
// validation plan's catalogue, and that plan owns the single declaration site for
// ErrPskNonceLength, ErrPskType and ErrDuplicatePsk. Neither those names nor ValSem itself
// has landed in this package yet, so the refusals are carried by the three unexported
// values below until they do.
//
// Unexported is the whole point of the shape. An exported ErrPskNonceLength declared
// here would be a second public declaration site for a name the validation plan also
// declares, the two would not be the same value, and a caller matching one would
// silently stop matching the other -- which is the failure errors_key_schedule.go's own
// header warns about. A name that cannot be reached from outside this package cannot be
// depended on from outside it either, so the swap costs nobody else anything.
//
// The swap itself is mechanical: wrap each detail in ValSem(ValSem401, ...) or
// ValSem(ValSem402, ...) or ValSem(ValSem403, ...) with the catalogue's sentinel as the
// detail, which is what the plan's body says and what makes errors.Is and CodeOf both
// hold. The messages here are the catalogue's own reason strings so that the text does
// not move either.
//
// The moment that swap is owed is not left to anybody's memory. ValSem, ValSem401,
// ValSem402, ValSem403, ErrPskNonceLength, ErrPskType and ErrDuplicatePsk are all listed in
// crossPlanSymbolsNotYetLanded in key_schedule_deps_test.go, and that gate fails on the
// commit that lands them, naming each one.
var (
	// errPskNonceLength is ValSem401. RFC 9420 section 8.4 requires psk_nonce to be a
	// fresh random value of KDF.Nh bytes, and a shorter one is not merely out of spec:
	// psk_nonce is the whole of what separates two uses of one pre shared key, so a
	// nonce an attacker can shorten is a nonce an attacker can collide.
	errPskNonceLength = errors.New("mls: psk: nonce length does not match the ciphersuite KDF output")

	// errPskType is ValSem402, which covers both a psktype outside the registry and a
	// resumption usage this profile does not implement. One code covers both because
	// they are one refusal from the receiver's point of view: this is not a
	// PreSharedKeyId it is willing to act on.
	errPskType = errors.New("mls: psk: resumption usage is not permitted here")

	// errDuplicatePsk is ValSem403. RFC 9420 section 12.2 invalidates a list that
	// "contains multiple PreSharedKey proposals that reference the same
	// PreSharedKeyID", and the same condition covers a GroupSecrets psks<V> vector.
	// A repeated id is one key mixed into the epoch twice, under two indices, by
	// members who each believe they are following the sender.
	errDuplicatePsk = errors.New("mls: psk: the list names the same PreSharedKeyID twice")
)

// PskType is the RFC 9420 section 8.4 PSKType enum. Zero is reserved and every value
// outside the two below is unregistered; both are refused rather than defaulted, for the
// reason the ciphersuite registry gives -- a default would let a peer's unsupported arm
// be silently read as one it never sent.
type PskType uint8

const (
	PskTypeExternal   PskType = 1
	PskTypeResumption PskType = 2
)

// ResumptionPskUsage is the RFC 9420 section 8.4 ResumptionPSKUsage enum. It is carried
// on the wire as a plain octet and judged separately, because decoding and accepting are
// different questions: a value this profile refuses still has to decode for the refusal
// to be able to name it.
type ResumptionPskUsage uint8

const (
	ResumptionPskUsageApplication ResumptionPskUsage = 1
	ResumptionPskUsageReInit      ResumptionPskUsage = 2
	ResumptionPskUsageBranch      ResumptionPskUsage = 3
)

// PreSharedKeyId identifies one pre shared key. The RFC's select() arms are flattened
// into one struct rather than modelled as a sum type, because the encoding is only ever
// produced and consumed here and the discriminant is already a field. What the
// flattening costs is that a value can hold fields its own arm does not encode, so the
// codec below is careful in both directions: MarshalMLS writes only the fields of the arm
// its discriminant names, and UnmarshalMLS clears the ones it did not read.
type PreSharedKeyId struct {
	PskType    PskType
	PskId      []byte
	Usage      ResumptionPskUsage
	PskGroupId []byte
	PskEpoch   uint64
	PskNonce   []byte
}

// MarshalMLS writes the id inline into a writer the caller owns and with no framing of
// its own, which is exactly what PSKLabel and GroupSecrets.psks<V> need -- a length
// prefix added here would be invisible to a caller reading only its own bytes and fatal
// to the psk_secret every member derives from them.
//
// The leaf writes return nothing and are no ops after the first failure (C2); the buffer
// error is collected by syntax.Marshal at Bytes. The error return carries the one
// semantic refusal this encoder has, an arm it cannot represent. Dropping that refusal
// would not be cosmetic: the discriminant octet and the nonce would still be written, so
// the result is a short but structurally plausible id that hashes into psk_input as
// though it were whole, and every member receiving it derives a psk_secret from bytes
// describing a key nobody named.
func (self *PreSharedKeyId) MarshalMLS(w *syntax.Writer) error {
	w.WriteUint8(uint8(self.PskType))
	switch self.PskType {
	case PskTypeExternal:
		w.WriteOpaque(self.PskId)
	case PskTypeResumption:
		w.WriteUint8(uint8(self.Usage))
		w.WriteOpaque(self.PskGroupId)
		w.WriteUint64(self.PskEpoch)
	default:
		return fmt.Errorf("%w: psktype %d", errPskType, self.PskType)
	}
	w.WriteOpaque(self.PskNonce)
	return nil
}

// UnmarshalMLS decodes exactly one id and leaves the rest of the reader alone, which is
// what a psks<V> vector and a PSKLabel preimage both depend on: eating the tail here
// would eat the next id, or the joiner secret sitting behind it. Full consumption of a
// standalone encoding is enforced one layer up, by syntax.Unmarshal.
//
// An unregistered psktype is a decode error rather than a value carried through. The
// alternative -- decode it, keep the octet, refuse later -- sounds more tolerant and is
// not: the arm decides how many bytes follow it, so an implementation that does not know
// the arm does not know where the id ends, and anything it does with the bytes after the
// discriminant is a guess.
//
// Nothing is assigned until every field has been read, and every field is assigned once
// they have. Both halves matter and they fail differently. A decoder that assigned as it
// read would leave a refused decode holding some fields from the new input and the rest
// from whatever the caller's value held before -- not a mangled struct anybody would
// notice, but a well formed PreSharedKeyId naming a key that was never sent. A decoder
// that assigned only the fields of the arm it read would leave the OTHER arm's fields
// behind on a reused receiver: decode a resumption id, then an external one into the same
// value, and PskGroupId and PskEpoch still describe the first.
func (self *PreSharedKeyId) UnmarshalMLS(r *syntax.Reader) error {
	typeOctet, err := r.ReadUint8()
	if err != nil {
		return fmt.Errorf("mls: psk type: %w", err)
	}
	var (
		pskType    = PskType(typeOctet)
		pskId      []byte
		usage      ResumptionPskUsage
		pskGroupId []byte
		pskEpoch   uint64
	)
	switch pskType {
	case PskTypeExternal:
		if pskId, err = r.ReadOpaque(); err != nil {
			return fmt.Errorf("mls: psk id: %w", err)
		}
	case PskTypeResumption:
		usageOctet, usageErr := r.ReadUint8()
		if usageErr != nil {
			return fmt.Errorf("mls: psk usage: %w", usageErr)
		}
		usage = ResumptionPskUsage(usageOctet)
		if pskGroupId, err = r.ReadOpaque(); err != nil {
			return fmt.Errorf("mls: psk group id: %w", err)
		}
		if pskEpoch, err = r.ReadUint64(); err != nil {
			return fmt.Errorf("mls: psk epoch: %w", err)
		}
	default:
		return fmt.Errorf("%w: psktype %d", errPskType, pskType)
	}
	pskNonce, err := r.ReadOpaque()
	if err != nil {
		return fmt.Errorf("mls: psk nonce: %w", err)
	}
	self.PskType = pskType
	self.PskId = pskId
	self.Usage = usage
	self.PskGroupId = pskGroupId
	self.PskEpoch = pskEpoch
	self.PskNonce = pskNonce
	return nil
}

// the C1 pin: drift between this type and the one codec convention fails at build.
var _ syntax.Codec = (*PreSharedKeyId)(nil)

// Validate is ValSem401 (nonce length) and ValSem402 (type and resumption usage).
//
// The nonce length is read off the provider rather than compared against 32, because
// KDF.Nh is a property of the ciphersuite and the two suites registered today happen to
// agree on it. A literal here would be indistinguishable from the derivation until the
// first suite with a wider hash arrives, at which point it would accept a nonce half the
// length the RFC requires and nothing in this package would say so.
//
// ValSem402 refuses ReInit and Branch. Both name features the v1 profile does not
// implement, and the RFC permits either only in a context this implementation never
// constructs -- a ReInit commit, a Branch welcome. Accepting one here would let a
// resumption secret from a group nobody re-initialised be mixed into an epoch, which is
// the confusion the usage field exists to prevent.
func (self *PreSharedKeyId) Validate(crypto CryptoProvider) error {
	if crypto == nil {
		return fmt.Errorf("%w: the nonce this validates is KDF.Nh, read off it", ErrNilCryptoProvider)
	}
	if len(self.PskNonce) != crypto.HashSize() {
		return fmt.Errorf("%w: %d bytes, want %d",
			errPskNonceLength, len(self.PskNonce), crypto.HashSize())
	}
	switch self.PskType {
	case PskTypeExternal:
		return nil
	case PskTypeResumption:
		if self.Usage != ResumptionPskUsageApplication {
			return fmt.Errorf("%w: resumption usage %d", errPskType, self.Usage)
		}
		return nil
	}
	return fmt.Errorf("%w: psktype %d", errPskType, self.PskType)
}

// ---------------------------------------------------------------------------
// ValSem403: the same PreSharedKeyID named twice
// ---------------------------------------------------------------------------

// CheckNoDuplicatePsks is ValSem403. RFC 9420 section 12.2 makes a proposal list invalid
// when "it contains multiple PreSharedKey proposals that reference the same
// PreSharedKeyID", and the rule reaches the psks<V> vector of a GroupSecrets too, because
// section 8.4 folds both lists through one recurrence and a repeated entry is a psk mixed
// in twice under two indices.
//
// What "the same PreSharedKeyID" means is the whole of this rule, and it is decided over
// the SERIALIZED id rather than over a hand written list of fields. Two independent
// reasons point the same way.
//
// psk_nonce is a field of PreSharedKeyID and section 8.4 requires a fresh one on every
// injection, so two ids sharing a psk_id and differing in nonce are two different ids and
// refusing them would be stricter than the RFC -- a refusal of something a conformant
// peer may legitimately send, which is an interop break rather than a safety margin. The
// select() arms cut the other way: the external arm does not encode usage, psk_group_id
// or psk_epoch at all, so two external ids differing only in those are ONE id on the
// wire, contribute one psk_input, and are a duplicate whatever the go struct happens to
// hold. A field by field comparison gets one of those two backwards by construction, and
// gets every field added to the struct later wrong as well. The encoding is what the
// protocol agrees on, so the encoding is what is compared.
//
// An id this package cannot encode is refused rather than skipped. Marshal's one semantic
// refusal is an unregistered psktype, and treating that as "not a duplicate" would let a
// list carry two copies of an id nobody can name past the only check that looks at it.
//
// ValSem403 is untested in OpenMLS (openmls#1335), so differential agreement proves
// nothing here and the RFC text above is the only authority.
//
// The lookup is a map rather than the pairwise sweep this task was drafted with. The list
// is attacker supplied -- the psks<V> of a Welcome, or a commit's proposal list -- and a
// pairwise sweep over the 65535 entries the uint16 count admits is four billion
// comparisons whose length the peer chooses. Nothing compared here is secret: a
// PreSharedKeyID travels in the clear in the proposal that names it, and the secret it
// names is not in this structure at all. Guardrail 8's constant time rule is about tag
// comparisons, which this is not.
func CheckNoDuplicatePsks(ids []PreSharedKeyId) error {
	firstAt := make(map[string]int, len(ids))
	for i := range ids {
		encoded, err := syntax.Marshal(&ids[i])
		if err != nil {
			return fmt.Errorf("psk %d: %w", i, err)
		}
		if previous, seen := firstAt[string(encoded)]; seen {
			return fmt.Errorf("%w: entries %d and %d", errDuplicatePsk, previous, i)
		}
		firstAt[string(encoded)] = i
	}
	return nil
}

// ---------------------------------------------------------------------------
// section 8.4: PSKLabel and psk_secret
// ---------------------------------------------------------------------------

// PreSharedKeyInput pairs an id with the secret bytes that id names.
//
// The secret is deliberately not a field of PreSharedKeyId. The id is the half that
// travels on the wire and the secret is the half that never does, and a single structure
// holding both would leave key material one careless syntax.Marshal away from a proposal.
type PreSharedKeyInput struct {
	Id     PreSharedKeyId
	Secret []byte
}

// marshalPskLabel encodes RFC 9420 section 8.4's
//
//	struct {
//	    PreSharedKeyID id;
//	    uint16 index;
//	    uint16 count;
//	} PSKLabel;
//
// The id is written inline through its own MarshalMLS with no framing of its own, which is
// why that method takes a writer rather than returning bytes: a length prefix added here
// would be invisible to a reader of this preimage and fatal to the psk_secret every member
// derives from it.
//
// index and count are what domain separate one psk's contribution from the same psk's
// contribution somewhere else. The chain in PskSecret already makes the fold order
// dependent on its own, so what these two fields add is narrower and worth stating
// exactly: a psk_input computed for position i of an n entry list is not the psk_input for
// any other position, or for the same position in a list of another length, so a
// contribution cannot be lifted out of one list and spliced into another.
//
// THE RESULT IS BOUNDED, and this is the second member of a class rather than a precaution.
// PskSecret hands these bytes to ExpandWithLabel as the CONTEXT of "derived psk", which is one
// opaque<V> of a KDFLabel, and ExpandWithLabel is a CryptoProvider method that cannot report a
// refusal -- an over long context takes the process down inside crypto_labels.go's
// mlsLabelBytes. A PSKLabel is a COMPOSITION: the psk_id it inlines carries a psk_group_id and a
// psk_nonce which are each bounded by syntax.MaxVectorLength on their own and are unbounded
// together, and both arrive from the wire in a Welcome or in a PreSharedKey proposal. So the same
// one valid message that crashed every member through the proposal cache reaches the key schedule
// by this route as well, and the bound is here for the reason it is there: this is the outermost
// declaration on the path whose signature can say no.
func marshalPskLabel(id *PreSharedKeyId, index uint16, count uint16) ([]byte, error) {
	encoded, err := encodePskLabel(id, index, count)
	if err != nil {
		return nil, err
	}
	if err := checkLabelledFieldLength("psk label", "", len(encoded)); err != nil {
		return nil, err
	}
	return encoded, nil
}

// The encoding on its own, with no bound, which is the half a caller must never reach.
//
// It is a declaration rather than the first four statements of the one above it for a reason
// that is about the TEST and is worth stating rather than leaving as a shape somebody tidies
// away. The boundary this defect turns on is a length, so the case that matters is a PSKLabel
// of EXACTLY syntax.MaxVectorLength octets and one either side of it, and a test cannot state
// that length unless it can encode a label the bound would refuse. The alternative is a test
// that re-encodes a PSKLabel to measure one, which is a SECOND OPINION about an encoding --
// exactly what this file's own comment above refuses a length prefix here for, and what
// key_package.go's "assembled exactly once" gate exists to stop. So the split is the encoding
// staying observable rather than a layer for its own sake, and nothing but marshalPskLabel and
// that boundary test calls it.
func encodePskLabel(id *PreSharedKeyId, index uint16, count uint16) ([]byte, error) {
	w := syntax.NewWriter()
	if err := id.MarshalMLS(w); err != nil {
		return nil, err
	}
	w.WriteUint16(index)
	w.WriteUint16(count)
	return w.Bytes()
}

// PskSecret is the RFC 9420 section 8.4 recurrence, over the list in the order given:
//
//	psk_extracted_[i] = KDF.Extract(0, psk_[i])
//	psk_input_[i]     = ExpandWithLabel(psk_extracted_[i], "derived psk", PSKLabel_[i], KDF.Nh)
//	psk_secret_[0]    = 0
//	psk_secret_[i]    = KDF.Extract(psk_input_[i-1], psk_secret_[i-1])
//	psk_secret        = psk_secret_[n]
//
// where 0 is the KDF.Nh all zero string.
//
// Extract takes (salt, ikm) here, in the order the spec text writes it: the zero string is
// the SALT of the first line and psk_input is the SALT of the last. Either transposition
// compiles, returns KDF.Nh bytes and passes everything that is not compared against a
// published answer, which is why the corpus sweep over psk_secret.json is the check that
// matters and not any property this file could assert about its own output.
//
// The empty list is not a skipped contribution: it is psk_secret_[0], the all zero string,
// and it is what every epoch this product derives actually mixes in. See EmptyPskSecret.
//
// Validation and the duplicate check both happen here rather than being left to the
// caller, so there is no order of calls that reaches a psk_secret over a list ValSem401,
// ValSem402 or ValSem403 refuses.
func PskSecret(crypto CryptoProvider, psks []PreSharedKeyInput) ([]byte, error) {
	if crypto == nil {
		return nil, fmt.Errorf("%w: psk_secret is a chain of extractions through it", ErrNilCryptoProvider)
	}
	pskSecret := ZeroSecret(crypto)
	if len(psks) == 0 {
		return pskSecret, nil
	}
	// the uint16 count field cannot describe a longer list. truncating the length into it
	// would not be a smaller list, it would be every member labelling the same psk
	// differently, so this is a refusal rather than a wrap.
	if len(psks) > math.MaxUint16 {
		return nil, fmt.Errorf("%w: %d", ErrPskCount, len(psks))
	}
	ids := make([]PreSharedKeyId, 0, len(psks))
	for i := range psks {
		ids = append(ids, psks[i].Id)
	}
	if err := CheckNoDuplicatePsks(ids); err != nil {
		return nil, err
	}
	// the all zero salt of psk_extracted_[i]. hoisted because it is public constant data
	// rather than a secret, and kept distinct from the accumulator below, which is erased
	// on every step.
	zero := ZeroSecret(crypto)
	count := uint16(len(psks))
	for i := range psks {
		if err := psks[i].Id.Validate(crypto); err != nil {
			return nil, fmt.Errorf("psk %d: %w", i, err)
		}
		label, err := marshalPskLabel(&psks[i].Id, uint16(i), count)
		if err != nil {
			return nil, fmt.Errorf("psk %d: %w", i, err)
		}
		extracted := crypto.Extract(zero, psks[i].Secret)
		input := crypto.ExpandWithLabel(extracted, "derived psk", label, crypto.HashSize())
		next := crypto.Extract(input, pskSecret)
		zeroizeSecret(extracted)
		zeroizeSecret(input)
		zeroizeSecret(pskSecret)
		pskSecret = next
	}
	return pskSecret, nil
}

// EmptyPskSecret is psk_secret for an epoch with no pre shared keys: the KDF.Nh all zero
// string that section 8.4 calls psk_secret_[0].
//
// It exists because that case is not a skipped step. Every v1 epoch of this product has no
// PSKs, so NewGroup, Commit and JoinFromWelcome all mix this value into the key schedule,
// and an implementation that omitted the psk contribution instead would derive a different
// epoch from the same inputs and agree with nobody. Making it total rather than fallible is
// the other half: PskSecret(crypto, nil) cannot fail, and a caller forced to handle an
// error that cannot occur will eventually handle it wrongly.
//
// A fresh slice on every call, for the reason ZeroSecret gives: the key schedule erases
// what it has finished with, and a shared constant would come back cleared.
func EmptyPskSecret(crypto CryptoProvider) []byte {
	return ZeroSecret(crypto)
}
