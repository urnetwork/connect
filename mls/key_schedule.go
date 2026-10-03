// The RFC 9420 section 8 epoch key schedule.
//
// Every secret an epoch holds is a function of that epoch's GroupContext, so nothing in
// this file may take a shortcut around it. Two members that expand over different context
// bytes derive different secrets and stop being able to talk, and that failure arrives as
// an undecryptable message rather than as the mistake it was.
//
// The one thing this file has to get right that no downstream check can see is the order
// of the two arguments to Extract. RFC 9420 writes KDF.Extract(salt, ikm); crypto/hkdf
// takes the input keying material first and the salt second. The swap is confined to
// crypto.go and hpke.go, and CryptoProvider.Extract — which takes (salt, ikm), the spec's
// order — is the only spelling this file may use. Transposing the two here compiles,
// returns KDF.Nh bytes, and satisfies every round trip and every self consistency check
// this package could write, because the wrong secret is exactly as well formed as the
// right one. The only thing that separates them is a known answer somebody else published,
// which is what key_schedule_test.go holds this to.
package mls

import (
	"bytes"
	"fmt"
)

// PastEpochWindow bounds how many past epochs of state, and therefore how many past
// resumption_psk values and eph_root values, a client retains. RFC 9420 ValSem400 makes
// bounding this a SHOULD and OpenMLS does not implement it at all (openmls#1122); here it
// is a hard bound. Thirty-two rather than eight because the window is a product promise
// about how long a laptop may stay closed, and an active group can burn eight epochs in a
// day.
const PastEpochWindow uint64 = 32

// ZeroSecret returns the KDF.Nh all-zero secret RFC 9420 writes as 0. It is the
// commit_secret of a commit with no UpdatePath and the psk_secret of an epoch with no
// PSKs.
//
// A fresh slice per call, and not a package level constant handed out repeatedly, because
// the key schedule zeroizes what it is finished with: a caller that erased a shared
// constant would leave every later call returning the same bytes and no way to tell.
// Callers cannot be asked to remember which secrets are safe to erase.
//
// What a test can honestly observe about this is its length, that every byte is zero, and
// that two calls do not share storage. That the value is the RIGHT zero — the one RFC 9420
// substitutes for a missing commit secret — is not a property of the returned bytes at
// all, since one run of Nh zero bytes is indistinguishable from another. What holds the
// spelling is the published key schedule and psk_secret corpora that expand over it, and
// the tasks that consume this function are where those comparisons live. A test here that
// claimed more would be reassuring rather than checking.
func ZeroSecret(crypto CryptoProvider) []byte {
	return make([]byte, crypto.HashSize())
}

// DeriveJoinerSecret computes joiner_secret for the epoch being entered:
//
//	joiner_secret = ExpandWithLabel(
//	    KDF.Extract(init_secret_[n-1], commit_secret),
//	    "joiner", GroupContext_[n], KDF.Nh)
//
// The GroupContext is the one for the epoch being ENTERED, not the one being left. A
// caller that passes the outgoing context derives a joiner secret every peer disagrees
// with, and there is nothing about the value that says so.
//
// init_secret_[n-1] is the salt and commit_secret is the input keying material, in that
// order, through CryptoProvider.Extract. See the file comment: the transposition is
// invisible to everything except a published answer.
//
// Both secrets must be exactly KDF.Nh bytes and a short one is refused rather than
// stretched. HKDF-Extract accepts any length of either argument and would hand back a
// perfectly well formed pseudorandom key, so a truncated init secret becomes an epoch that
// looks valid on this side and matches nobody — the length mistake would surface epochs
// later as an undecryptable message.
//
// The pseudorandom key is erased before returning. It is not the joiner secret and nothing
// downstream needs it, and it is one HKDF-Expand away from every key of the epoch.
//
// A nil context is refused rather than serialized. The marshal is handed a non nil
// interface holding a nil pointer, so MarshalMLS dereferences it and the caller gets a
// runtime panic raised inside the syntax package, naming neither this function nor the
// argument that was missing. Every caller of this takes its context off a struct field,
// which is exactly where an unset one comes from.
//
// The encoding goes through marshalBoundedComposition rather than through syntax.Marshal,
// and that is not tidiness. These bytes become the CONTEXT field of ExpandWithLabel, which
// is one opaque<V> of a KDFLabel, and ExpandWithLabel is a CryptoProvider method with no
// error to return: a GroupContext whose group_id, extensions and hashes each obey the
// mebibyte limit while their SUM does not would take the process down inside the key
// schedule. A group context that large is one a decoder accepts, so the refusal belongs
// here, where there is still a caller to tell.
func DeriveJoinerSecret(
	crypto CryptoProvider,
	initSecretPrev []byte,
	commitSecret []byte,
	groupContext *GroupContext,
) ([]byte, error) {
	// the provider is asked for ahead of the group context and ahead of every length
	// below, because each of those checks reads KDF.Nh off it: there is no argument this
	// function can judge without one, so "no provider" is the first thing it can say.
	if crypto == nil {
		return nil, fmt.Errorf("%w: joiner_secret is an extraction and an expansion through it", ErrNilCryptoProvider)
	}
	if groupContext == nil {
		return nil, ErrNilGroupContext
	}
	nh := crypto.HashSize()
	if len(initSecretPrev) != nh {
		return nil, fmt.Errorf("%w: init secret is %d bytes, want %d", ErrSecretLength, len(initSecretPrev), nh)
	}
	if len(commitSecret) != nh {
		return nil, fmt.Errorf("%w: commit secret is %d bytes, want %d", ErrSecretLength, len(commitSecret), nh)
	}
	encodedGroupContext, err := marshalBoundedComposition("group context", groupContext)
	if err != nil {
		return nil, err
	}
	// Extract takes (salt, ikm), the order the spec writes. init_secret is the salt.
	prk := crypto.Extract(initSecretPrev, commitSecret)
	joinerSecret := crypto.ExpandWithLabel(prk, "joiner", encodedGroupContext, nh)
	zeroizeSecret(prk)
	return joinerSecret, nil
}

// EpochSecrets is every secret RFC 9420 section 8 derives from epoch_secret, and it is
// the whole of what an epoch hands out.
//
// epoch_secret itself is deliberately absent, which is guardrail G6. It is the parent of
// all nine, so a caller holding it holds confirmation_key and membership_key too — the two
// that authenticate a commit — and every later secret this epoch will ever produce. A
// field added here for it, or an accessor added to KeySchedule, would be one line that
// gives the epoch away, so what stops that is a test rather than this paragraph: see
// TestNoExportedSurfaceOfTheKeyScheduleReturnsTheEpochSecret.
//
// All nine are KDF.Nh bytes and all nine are indistinguishable from random, which is why
// the only test that can tell them apart is a known answer somebody else published. A
// label copied from the line above compiles, produces a well formed secret of the right
// length, and disagrees with every peer.
type EpochSecrets struct {
	SenderData         []byte
	Encryption         []byte
	Exporter           []byte
	External           []byte
	Confirmation       []byte
	Membership         []byte
	ResumptionPsk      []byte
	EpochAuthenticator []byte
	InitSecret         []byte
}

// KeySchedule is one epoch of the RFC 9420 section 8 key schedule: the secrets that epoch
// holds and the serialized GroupContext they were all expanded over.
//
// Not safe for concurrent use. The owning Group serializes access, and the secrets are
// erased in place when the epoch leaves PastEpochWindow, so a second goroutine reading one
// of them across that erase reads a partly zeroed key rather than a stale one.
//
// Every secret the schedule is handed is COPIED into storage of its own, and every secret
// it derives is storage of its own from the start. So the epoch never writes through an
// array a caller still holds, which matters because the last thing this type does is erase
// every one of them in place: an epoch that retained its caller's joiner secret would clear
// that caller's slice as a side effect of leaving PastEpochWindow. The other half of that
// bargain is the caller's: whoever hands a secret in still owns the copy it handed, and this
// type's erase cannot reach it.
type KeySchedule struct {
	crypto            CryptoProvider
	groupContextBytes []byte
	joinerSecret      []byte
	welcomeSecret     []byte
	epochSecret       []byte
	secrets           EpochSecrets
}

// NewKeySchedule advances the schedule into the epoch groupContext describes, from the
// previous epoch's init_secret and this commit's commit_secret. This is the committer's
// path and the path of every member who processes that commit.
//
// The GroupContext is the one for the epoch being ENTERED. Every length check on the two
// secrets, and the refusal of a nil context, belong to DeriveJoinerSecret, which this
// delegates the first half of the derivation to.
func NewKeySchedule(
	crypto CryptoProvider,
	initSecretPrev []byte,
	commitSecret []byte,
	pskSecret []byte,
	groupContext *GroupContext,
) (*KeySchedule, error) {
	if crypto == nil {
		return nil, fmt.Errorf("%w: every secret of the epoch is derived through it", ErrNilCryptoProvider)
	}
	joinerSecret, err := DeriveJoinerSecret(crypto, initSecretPrev, commitSecret, groupContext)
	if err != nil {
		return nil, err
	}
	schedule, err := NewKeyScheduleFromJoiner(crypto, joinerSecret, pskSecret, groupContext)
	// the joiner secret this call derived reaches no caller of this function either way:
	// on the refusal it is dropped, and on success the schedule keeps a copy of its own. It
	// is one Extract and one Expand away from every key of the epoch, so the storage it was
	// computed into is erased rather than left for the collector. A psk secret of the wrong
	// length is exactly the shape that reaches the refusal.
	zeroizeSecret(joinerSecret)
	if err != nil {
		return nil, err
	}
	return schedule, nil
}

// NewKeyScheduleFromJoiner builds the schedule a joiner reaches from the joiner_secret
// carried in its GroupSecrets, which is the only path a member added by Welcome has: it
// never sees init_secret_[n-1] or commit_secret. RFC 9420 section 8:
//
//	member_secret  = KDF.Extract(joiner_secret, psk_secret)
//	welcome_secret = DeriveSecret(member_secret, "welcome")
//	epoch_secret   = ExpandWithLabel(member_secret, "epoch", GroupContext_[n], KDF.Nh)
//
// joiner_secret is the salt of that Extract and psk_secret is the input keying material,
// in that order, through CryptoProvider.Extract. See the file comment: both are KDF.Nh
// pseudorandom secrets, so the transposition compiles and answers with a secret exactly as
// well formed as the right one.
//
// Both must be exactly KDF.Nh bytes. HKDF-Extract accepts any length of either argument and
// would hand back a well formed pseudorandom key, so a short psk secret becomes an epoch
// that looks valid on this side and matches nobody — surfacing epochs later as an
// undecryptable message rather than as the length mistake it is.
//
// A nil context is refused rather than serialized, for the reason DeriveJoinerSecret gives:
// the marshal is handed a non nil interface holding a nil pointer and MarshalMLS
// dereferences it, so the caller would get a panic out of the syntax package naming neither
// this function nor the argument that was missing. The encoding is bounded for that
// function's other reason as well -- these bytes are the context field of the "epoch"
// expansion, and a composition past one field's limit reaches a construction that cannot
// refuse it.
//
// member_secret is erased once it has produced the two things it feeds. The epoch does not
// retain it and nothing downstream needs it, and it reproduces both welcome_secret and
// epoch_secret — which is to say all nine secrets — from one HKDF-Expand each.
//
// The joiner secret is copied rather than retained, for the reason the type comment gives.
// The caller keeps the array it passed and owes it a zeroize of its own.
func NewKeyScheduleFromJoiner(
	crypto CryptoProvider,
	joinerSecret []byte,
	pskSecret []byte,
	groupContext *GroupContext,
) (*KeySchedule, error) {
	// the provider is asked for ahead of the group context and ahead of every length
	// below, because each of those checks reads KDF.Nh off it: there is no argument this
	// function can judge without one, so "no provider" is the first thing it can say.
	if crypto == nil {
		return nil, fmt.Errorf("%w: every secret of the epoch is derived through it", ErrNilCryptoProvider)
	}
	if groupContext == nil {
		return nil, ErrNilGroupContext
	}
	nh := crypto.HashSize()
	if len(joinerSecret) != nh {
		return nil, fmt.Errorf("%w: joiner secret is %d bytes, want %d", ErrSecretLength, len(joinerSecret), nh)
	}
	if len(pskSecret) != nh {
		return nil, fmt.Errorf("%w: psk secret is %d bytes, want %d", ErrSecretLength, len(pskSecret), nh)
	}
	encodedGroupContext, err := marshalBoundedComposition("group context", groupContext)
	if err != nil {
		return nil, err
	}
	// Extract takes (salt, ikm), the order the spec writes. joiner_secret is the salt.
	memberSecret := crypto.Extract(joinerSecret, pskSecret)
	welcomeSecret := crypto.DeriveSecret(memberSecret, "welcome")
	epochSecret := crypto.ExpandWithLabel(memberSecret, "epoch", encodedGroupContext, nh)
	zeroizeSecret(memberSecret)
	return newKeyScheduleFromParts(
		crypto, encodedGroupContext, bytes.Clone(joinerSecret), welcomeSecret, epochSecret), nil
}

// newKeyScheduleFromParts expands one epoch_secret into the nine derived secrets.
//
// Both exported constructors route through here, so a label can only ever be wrong in one
// place rather than in two places that agree with each other. The nine labels are written
// as literals and are deliberately not exported as a table: a test that read its
// expectations off such a table would agree with any spelling of them, which is the one
// shape a known answer test must not have. What holds these is the published corpus in
// key_schedule_test.go.
//
// joinerSecret and welcomeSecret are nil on the group creation path, where neither is
// defined. The accessors return that nil unchanged rather than substituting a zero secret,
// so a caller cannot seal a Welcome under KDF.Nh zero bytes and believe it used a key.
func newKeyScheduleFromParts(
	crypto CryptoProvider,
	encodedGroupContext []byte,
	joinerSecret []byte,
	welcomeSecret []byte,
	epochSecret []byte,
) *KeySchedule {
	return &KeySchedule{
		crypto:            crypto,
		groupContextBytes: encodedGroupContext,
		joinerSecret:      joinerSecret,
		welcomeSecret:     welcomeSecret,
		epochSecret:       epochSecret,
		secrets: EpochSecrets{
			SenderData:         crypto.DeriveSecret(epochSecret, "sender data"),
			Encryption:         crypto.DeriveSecret(epochSecret, "encryption"),
			Exporter:           crypto.DeriveSecret(epochSecret, "exporter"),
			External:           crypto.DeriveSecret(epochSecret, "external"),
			Confirmation:       crypto.DeriveSecret(epochSecret, "confirm"),
			Membership:         crypto.DeriveSecret(epochSecret, "membership"),
			ResumptionPsk:      crypto.DeriveSecret(epochSecret, "resumption"),
			EpochAuthenticator: crypto.DeriveSecret(epochSecret, "authentication"),
			InitSecret:         crypto.DeriveSecret(epochSecret, "init"),
		},
	}
}

// NewKeyScheduleFromEpochSecret builds the schedule of a group being created, from the
// fresh epoch_secret of KDF.Nh bytes RFC 9420 section 11 says to sample. It is the only
// entry point a NewGroup can use: there is no previous init_secret to advance from and no
// joiner_secret to be handed, so neither of the other two constructors has an argument to
// be called with.
//
// joiner_secret and welcome_secret are undefined here and the accessors return nil. The
// creator obtains real ones by committing the first Add, which runs the section 8
// derivation in NewKeySchedule.
//
// The sample is copied rather than retained, so an epoch cannot be changed under its own
// members by a caller that reuses its buffer. That leaves the caller holding the only other
// copy and this type's erase cannot reach it: whoever samples an epoch secret owes it a
// zeroize of its own once this has returned.
func NewKeyScheduleFromEpochSecret(
	crypto CryptoProvider,
	epochSecret []byte,
	groupContext *GroupContext,
) (*KeySchedule, error) {
	// the provider is asked for ahead of the group context and ahead of every length
	// below, because each of those checks reads KDF.Nh off it: there is no argument this
	// function can judge without one, so "no provider" is the first thing it can say.
	if crypto == nil {
		return nil, fmt.Errorf("%w: the nine derived secrets are expansions through it", ErrNilCryptoProvider)
	}
	if groupContext == nil {
		return nil, ErrNilGroupContext
	}
	nh := crypto.HashSize()
	if len(epochSecret) != nh {
		return nil, fmt.Errorf("%w: epoch secret is %d bytes, want %d", ErrSecretLength, len(epochSecret), nh)
	}
	encodedGroupContext, err := marshalBoundedComposition("group context", groupContext)
	if err != nil {
		return nil, err
	}
	return newKeyScheduleFromParts(
		crypto, encodedGroupContext, nil, nil, bytes.Clone(epochSecret)), nil
}

// JoinerSecret is the joiner_secret a Welcome carries to a new member, or nil on the group
// creation path, where the group was never joined and no such secret exists.
//
// Nil also for an epoch whose secrets have been erased, and that second nil is the one worth
// the check. An erase leaves KDF.Nh ZERO bytes behind rather than a short slice, and that is
// exactly the length NewKeyScheduleFromJoiner requires, so a caller handed those would rebuild
// the whole epoch out of a secret every party can compute — with err == nil, because nothing
// downstream is in a position to notice. The nine are protected at each derivation instead,
// since the nine are consumed inside this type where secretIsLive can be asked; joiner_secret
// is consumed by a package level function that is handed bytes and has no epoch to ask, so for
// this one the refusal has to be here.
func (self *KeySchedule) JoinerSecret() []byte {
	if !self.secretIsLive(self.joinerSecret) {
		return nil
	}
	return self.joinerSecret
}

// WelcomeSecret is the input to the Welcome AEAD key and nonce, or nil on the group
// creation path. Nil rather than a zero secret, so a caller that seals a Welcome with it
// fails a length check instead of sealing under KDF.Nh zero bytes.
//
// An erased epoch answers nil for that same sentence read the other way round. WelcomeKeyNonce
// refuses a welcome_secret of the wrong length and an erase leaves the length exactly as it
// was, so without this the one caller the sentence was written for — a creator holding nil —
// is protected, while a caller holding an aged out epoch seals encrypted_group_info under an
// expansion of KDF.Nh zero bytes that every party can recompute.
func (self *KeySchedule) WelcomeSecret() []byte {
	if !self.secretIsLive(self.welcomeSecret) {
		return nil
	}
	return self.welcomeSecret
}

// restoreSecret is the ONE secret a persisted epoch carries: the epoch_secret a created
// group sampled, or the joiner_secret every epoch a commit opened was built from.
//
// It exists so that a persisted state stores an INPUT rather than an output. EpochSecrets is
// nine expansions of one parent, and a blob carrying the nine would be a SECOND derivation
// path -- so a restored group and a live one would agree for exactly as long as the two
// paths agreed, and the first thing to notice a disagreement would be a peer refusing a
// confirmation tag. Rebuilding through the constructor that built the epoch in the first
// place is the only reading with one derivation in it.
//
// UNEXPORTED, and it answers the schedule's own storage rather than a copy: those are the
// same decision twice. Guardrail 6 says no exported symbol of this package answers
// epoch_secret, and the one caller is (*Group).marshalState, which reads these octets into
// one length prefixed field and retains nothing -- a copy here would be a second live copy
// of the parent secret for the length of a marshal, held by nothing that erases it.
//
// An epoch whose secrets have been erased REFUSES, for JoinerSecret's reason read one step
// further out: the erase leaves KDF.Nh zero bytes, which is exactly the length both
// constructors require, so a state persisted out of an erased epoch would restore a group
// every party on earth can recompute -- with a nil error, because nothing downstream is in a
// position to notice.
func (self *KeySchedule) restoreSecret(kind restoreKind) ([]byte, error) {
	var secret []byte
	switch kind {
	case restoreFromEpochSecret:
		secret = self.epochSecret
	case restoreFromJoiner:
		secret = self.joinerSecret
	default:
		return nil, fmt.Errorf("%w: %d", errGroupStateRestoreKind, uint8(kind))
	}
	if !self.secretIsLive(secret) {
		return nil, fmt.Errorf("%w: restore kind %d", errGroupStateRestoreSecret, uint8(kind))
	}
	return secret, nil
}

// Secrets is the epoch's nine derived secrets. The pointer is into the schedule's own
// storage, which is what lets the epoch erase them in place; a caller that keeps one past
// that erase keeps a slice of zeros rather than a live key.
func (self *KeySchedule) Secrets() *EpochSecrets {
	return &self.secrets
}

// GroupContextBytes is the serialized GroupContext this epoch expanded over, which is also
// what framing signs and MACs under. Handing back the same bytes the derivations used is
// the point: a caller that re-encoded the struct itself could sign over an encoding no key
// in this epoch was derived from.
func (self *KeySchedule) GroupContextBytes() []byte {
	return self.groupContextBytes
}

// Export is MLS-Exporter of RFC 9420 section 8.5:
//
//	MLS-Exporter(Label, Context, Length) =
//	    ExpandWithLabel(DeriveSecret(exporter_secret, Label),
//	                    "exported", Hash(Context), Length)
//
// This is the only epoch bound key material connect/message obtains from an epoch, and it
// carries more than the protocol: URmessage layers seed recovery on it, wrapping each
// epoch's exported secret to the member's recovery key, so a defect here is a defect in
// the ability to restore an account from a seedphrase. The nine named secrets are not
// reachable this way — they are the epoch's own and Secrets() is where they live.
//
// Three separations have to hold and each is a different mistake. The label separates one
// caller's exports from another's, and a body that ignored it would hand two subsystems
// one secret while both believed they had their own. The context separates one call from
// the next under a single label, and a body that ignored it would do the same thing one
// level down — every "different" export the same bytes. The length is bound into the
// preimage by ExpandWithLabel, so two lengths under one label and context are unrelated
// secrets rather than one truncated to two sizes.
//
// The caller's context is HASHED rather than passed through, which is what lets a caller
// pass a context of any length: the expansion's own context field is then always KDF.Nh
// octets, well under the two byte vector length the label encoding writes.
//
// The length is refused rather than clamped above 255*KDF.Nh, which is all HKDF-Expand
// can produce. The ceiling is read off the provider rather than written down, so a suite
// with a wider hash gets its own ceiling; and it is a typed error rather than a panic
// because the length here is a CALLER's number — connect/message asks for what its own
// format needs — and not one this package fixes. CryptoProvider.Expand panics on the same
// condition, which is correct for the call sites that ask for a length their suite fixes
// and wrong for this one.
//
// An epoch whose exporter_secret has been erased is refused with ErrEpochErased rather than
// exported from. See secretIsLive: the erase leaves KDF.Nh zero bytes behind, and MLS-Exporter
// over KDF.Nh zero bytes is a value anyone can compute with no knowledge of the group. Since
// URmessage wraps seed recovery to this answer, an export taken from an aged out epoch would
// be wrapped to a key the attacker also holds, and a length check alone would report nothing.
//
// The caller's LABEL is refused above what one length prefixed field holds, through the same
// single comparison every other labelled construction asks and not a second spelling of it.
// It travels into DeriveSecret's KDFLabel with the "MLS 1.0 " prefix ahead of it, and the
// prefix is INSIDE the field whose length is declared, so the boundary sits that many octets
// under syntax.MaxVectorLength; past it the writer latches and mlsLabelBytes turns its
// refusal into a process exit.
//
// This one is written HERE rather than left to the provider for the reason the length above
// is: the label is a CALLER's string, this method is exported and takes it by design, and the
// argument crypto_labels.go makes for the remaining panics — that closing them means widening
// the pinned CryptoProvider interface — does not reach this declaration, which is not one of
// that interface's methods and already answers a caller's mistake with a typed error.
func (self *KeySchedule) Export(label string, context []byte, length int) ([]byte, error) {
	if length < 0 || length > 255*self.crypto.HashSize() {
		return nil, fmt.Errorf("%w: %d", ErrExportLength, length)
	}
	if err := checkLabelledFieldLength("exporter", " label", len(MlsLabelPrefix)+len(label)); err != nil {
		return nil, err
	}
	if !self.secretIsLive(self.secrets.Exporter) {
		return nil, ErrEpochErased
	}
	derived := self.crypto.DeriveSecret(self.secrets.Exporter, label)
	exported := self.crypto.ExpandWithLabel(derived, "exported", self.crypto.Hash(context), length)
	// the per label secret is one HKDF-Expand away from every export under that label and
	// nothing downstream needs it, so the storage it was computed into is erased rather
	// than left for the collector. exporter_secret itself is the epoch's and stays.
	zeroizeSecret(derived)
	return exported, nil
}

// ExternalKeyPair derives the epoch's external HPKE key pair from external_secret.
//
// v1 refuses external commits, so this key pair is never advertised in a GroupInfo and
// never used to accept an ExternalInit. It exists because key-schedule.json checks
// external_pub, and because a suite whose DeriveKeyPair disagrees here disagrees
// everywhere else it is used too. Keeping the derivation here rather than leaving it
// unwritten is also what makes v2 a policy change rather than a key schedule change.
//
// This is the one place in the key schedule that reaches into HPKE, and deliberately the
// only one: everything else in this file is arithmetic over byte slices, which is what
// keeps it auditable against RFC 9420 section 8 line by line.
//
// external_secret is passed as the input keying material and nothing else is mixed in.
// Any other secret of the epoch is the same length and derives a perfectly well formed
// key pair, so what separates the right one from the wrong one is the published
// external_pub and nothing about the value itself.
//
// An epoch whose external_secret has been erased is refused with ErrEpochErased, for the
// reason Export gives: DeriveKeyPair over KDF.Nh zero bytes answers a key pair whose private
// half every party can recompute, and it would come back with err == nil.
func (self *KeySchedule) ExternalKeyPair() (HpkePrivateKey, HpkePublicKey, error) {
	if !self.secretIsLive(self.secrets.External) {
		return nil, nil, ErrEpochErased
	}
	return self.crypto.DeriveKeyPair(self.secrets.External)
}

// secretIsLive answers whether one of this epoch's own secrets is still the secret it was
// derived as, rather than the run of zeros an erase leaves behind.
//
// Every method that DERIVES from one of the nine has to ask, and the reason is not that a
// zero secret is a weak one. It is that a derivation over KDF.Nh zero bytes is publicly
// computable: an epoch that has left PastEpochWindow is zeroized in place — the type
// comment says so, and Secrets() hands out the pointer that lets it happen — and an export
// taken afterwards is a value the attacker derives too, handed back with err == nil. The
// length alone cannot see that, because the erase leaves the header exactly as it was.
//
// The whole slice is read whatever the first byte holds, so an erased epoch's refusal takes
// the same time as a live epoch's answer and nothing about this epoch's own key is readable
// from how long the check ran.
//
// This is deliberately not an exported predicate. "Is this epoch still usable" asked and
// answered separately from the derivation is a race the caller cannot win, and the refusals
// above are the same question asked at the moment it is acted on.
func (self *KeySchedule) secretIsLive(secret []byte) bool {
	if len(secret) != self.crypto.HashSize() {
		return false
	}
	live := byte(0)
	for _, b := range secret {
		live |= b
	}
	return live != 0
}

// ConfirmationTag is MAC(confirmation_key, confirmed_transcript_hash): the value every
// Commit carries and every member who processes that Commit recomputes for itself.
//
// It is the fork detector, and it is the only one the protocol has. Two members whose
// transcripts diverged hold different confirmed_transcript_hash values, so they produce
// different tags and each of them can say so; without it a member that missed a proposal
// would go on encrypting to a group that had stopped listening, with every message it
// received still decrypting.
//
// The key is confirmation_key and NOT membership_key. The two are adjacent DeriveSecret
// calls over one parent under two labels, so they are the same length and the same shape,
// and swapping them yields a perfectly well formed KDF.Nh tag that this implementation
// would also accept back from itself. Nothing about the value separates them, which is why
// what holds this is the tag mlswg published in transcript-hashes.json rather than any
// round trip this package could write.
//
// The confirmed transcript hash is the caller's, and its length is not checked here. A hash
// that is not KDF.Nh bytes produces a tag no peer agrees with, and that failure lands on the
// RECEIVER, which recomputes over its own transcript and refuses — so the wrong length fails
// closed rather than authenticating anything. There is also no channel to report it through:
// the signature answers bytes, and a second meaning for nil would be indistinguishable from
// the erased epoch below.
//
// An epoch whose confirmation_key has been erased answers nil rather than a tag, for the
// reason Export refuses one: the erase leaves KDF.Nh zero bytes behind, and a MAC under
// KDF.Nh zero bytes is not a weak tag, it is a PUBLIC one that any party can compute with no
// knowledge of the group. A caller that framed it would be sending a Commit whose fork
// detector authenticates nobody, and a length check on the answer would report nothing.
func (self *KeySchedule) ConfirmationTag(confirmedTranscriptHash []byte) []byte {
	if !self.secretIsLive(self.secrets.Confirmation) {
		return nil
	}
	return self.crypto.Mac(self.secrets.Confirmation, confirmedTranscriptHash)
}

// VerifyConfirmationTag is RFC 9420 ValSem205: the confirmation_tag a Commit carries is the
// one this member's own confirmed_transcript_hash produces under this epoch's
// confirmation_key.
//
// A false answer is FATAL TO THE MESSAGE and the caller must return on it (spec A section
// 5.9, guardrail 7). It is not a warning to log beside a message that then goes on being
// processed: the whole content of a false here is that the sender's view of the group and
// this member's view of the group are not the same, so the commit must not be applied, the
// epoch must not be advanced, and nothing the message carries may be trusted. p7 is where
// that return is written; this sentence is here because the obligation belongs to the
// function that answers the bool, and a bool is the one result shape a caller can ignore by
// not looking at it.
//
// The comparison is CryptoProvider.MacVerify and nothing else, which is guardrail 8: it
// reaches crypto/subtle.ConstantTimeCompare, it refuses a length mismatch ahead of the
// comparison rather than comparing a prefix, and it is the only spelling this package
// permits. bytes.Equal, hmac.Equal and bytes.HasPrefix all answer the same bool for the
// tags that matter and none of them is this.
//
// A tag of the wrong length is refused rather than compared against as much of itself as
// fits — MacVerify checks the length first — because a prefix comparison accepts every
// truncation of a valid tag, and a receiver that accepted a one byte tag would be accepting
// a forgery an attacker finds in 256 tries.
//
// An epoch whose confirmation_key has been erased refuses everything, for the reason
// ConfirmationTag answers nil: the key is publicly computable once it is zero, so every
// party could forge a tag this would otherwise accept.
func (self *KeySchedule) VerifyConfirmationTag(confirmedTranscriptHash []byte, tag []byte) bool {
	if !self.secretIsLive(self.secrets.Confirmation) {
		return false
	}
	return self.crypto.MacVerify(self.secrets.Confirmation, confirmedTranscriptHash, tag)
}

// MembershipTag is MAC(membership_key, AuthenticatedContentTBM): what says a PublicMessage
// came from a member of this group rather than from anybody who can reach the transport.
//
// The caller serializes AuthenticatedContentTBM and passes the bytes — p6's
// AuthenticatedContentTBMBytes(authContent, groupContext), registry section 7.3 — because
// this plan owns the key schedule and never sees framing types. The consequence is that the
// binding of the GroupContext into the tag lives in the CALLER: this function authenticates
// exactly the bytes it is handed, so a TBM assembled without the group context would be a
// tag that verifies in any epoch of any group whose membership_key it was taken under.
//
// The key is membership_key and NOT confirmation_key, for the reason ConfirmationTag gives
// in the other direction. What holds this is the membership_tag mlswg published in
// message-protection.json.
//
// An epoch whose membership_key has been erased answers nil, for the reason ConfirmationTag
// answers nil.
func (self *KeySchedule) MembershipTag(authenticatedContentTbm []byte) []byte {
	if !self.secretIsLive(self.secrets.Membership) {
		return nil
	}
	return self.crypto.Mac(self.secrets.Membership, authenticatedContentTbm)
}

// VerifyMembershipTag is RFC 9420 ValSem008: the membership_tag a PublicMessage carries is
// the one this epoch's membership_key produces over the AuthenticatedContentTBM the receiver
// reassembled.
//
// A false answer is FATAL TO THE MESSAGE and the caller must return on it (spec A section
// 5.9, guardrail 7). Everything VerifyConfirmationTag says about that applies here and one
// thing more: this is the ONLY authentication a PublicMessage from a member carries besides
// the signature, so a caller that logged a false and went on would be applying a proposal or
// a commit that no member of the group sent.
//
// The comparison is CryptoProvider.MacVerify and nothing else, for the reasons
// VerifyConfirmationTag gives: constant time, and a length mismatch refused ahead of the
// comparison rather than a prefix compared.
func (self *KeySchedule) VerifyMembershipTag(authenticatedContentTbm []byte, tag []byte) bool {
	if !self.secretIsLive(self.secrets.Membership) {
		return false
	}
	return self.crypto.MacVerify(self.secrets.Membership, authenticatedContentTbm, tag)
}

// WelcomeKeyNonce derives the AEAD key and nonce that protect a Welcome's
// encrypted_group_info, RFC 9420 section 12.4.3.1:
//
//	welcome_key   = ExpandWithLabel(welcome_secret, "key", "", AEAD.Nk)
//	welcome_nonce = ExpandWithLabel(welcome_secret, "nonce", "", AEAD.Nn)
//
// The pair is one aead key and one aead nonce for one sealing, and everything that sealing
// promises rests on the pair being used once. Two properties carry that and neither is
// visible in the bytes.
//
// They are expanded under DIFFERENT labels. What that is worth cannot be measured by
// comparing the two answers: ExpandWithLabel binds the requested length into its own
// preimage, so ExpandWithLabel(s, "key", "", 32) and ExpandWithLabel(s, "key", "", 12) are
// already unrelated values — a body that expanded both halves under one label produces a
// nonce that is not the key's first Nn bytes and passes every comparison the two answers
// admit. Only a published answer separates the two labels, which is what the corpus tests
// hold this to.
//
// And they are two separate expansions into two separate arrays. A body that answered two
// headers over one backing array reads identically at every call site until the first one
// that erases its key, and then the nonce it seals the next Welcome under is zeros.
//
// Both lengths are read off the provider rather than written down. The registered suites
// disagree on Nk — 16 for AES-128-GCM, 32 for ChaCha20-Poly1305 — and agree on Nn at 12, so
// within this registry a hardcoded 12 is indistinguishable from a read one; the input that
// separates them is a provider whose Nn is neither, which is what
// TestWelcomeKeyNonceReadsBothLengthsOffTheProviderItWasHanded supplies.
//
// A welcome_secret of the wrong length is refused rather than expanded, and the caller that
// refusal exists for is the group creation path. HKDF-Expand takes a pseudorandom key of any
// length at or above the hash and would answer a perfectly well formed key and nonce, and
// (*KeySchedule).WelcomeSecret() is nil there because a group that was created was never
// joined — so without this the creator would seal encrypted_group_info under an expansion of
// nothing, which every party can recompute, and get err == nil for it.
func WelcomeKeyNonce(crypto CryptoProvider, welcomeSecret []byte) (key []byte, nonce []byte, err error) {
	if crypto == nil {
		return nil, nil, fmt.Errorf("%w: the welcome key and nonce are two expansions through it", ErrNilCryptoProvider)
	}
	nh := crypto.HashSize()
	if len(welcomeSecret) != nh {
		return nil, nil, fmt.Errorf("%w: welcome secret is %d bytes, want %d", ErrSecretLength, len(welcomeSecret), nh)
	}
	key = crypto.ExpandWithLabel(welcomeSecret, "key", nil, crypto.KeySize())
	nonce = crypto.ExpandWithLabel(welcomeSecret, "nonce", nil, crypto.NonceSize())
	return key, nonce, nil
}

// Zeroize erases every secret this epoch holds, in place. The owning Group calls it when the
// epoch leaves PastEpochWindow.
//
// All twelve go through zeroizeSecret rather than through a loop written here, which is what
// keeps this inside the class TestEveryEraseHelperCarriesTheNoInlineDirective derives: an
// erase spelled inline is one the compiler may inline and then elide, since in a caller that
// drops the schedule immediately afterwards these stores are writes to memory nothing reads
// again. Best effort in exactly the sense secret_zeroize.go sets out — it reaches the arrays
// this schedule points at, and no copy anybody took off them.
//
// It is idempotent, and has to be: an epoch may be dropped from the window by one path and
// released by another, and a second call over KDF.Nh zeros has to be a no-op rather than a
// fault.
//
// groupContextBytes is deliberately not erased. The serialized GroupContext is not a secret —
// every member of the group holds it, and it is the thing framing signs and MACs over — so
// erasing it would take away nothing an attacker lacks. It is the one byte slice of this type
// that is exempt, and it is exempt through a written row that
// TestZeroizeErasesEveryByteSliceThisTypeDeclares checks in both directions, so a tenth secret
// cannot join it by being forgotten.
//
// What the schedule does AFTERWARDS is not this function's doing and is the reason the erase
// can be this blunt: secretIsLive is asked at every derivation, so an erased epoch refuses
// rather than answering. Without that, every method here would go on answering — a tag, an
// export, an HPKE key pair, all derived from KDF.Nh zero bytes, all publicly computable, all
// the right length, and all with err == nil.
//
// The noinline directive goes with the paragraph above rather than around it. That the stores
// live inside zeroizeSecret is what keeps them across a call boundary; carrying the directive
// here as well is the class's rule, and a member that reasons its way out of a rule is how the
// rule stops being one. TestEveryEraseHelperCarriesTheNoInlineDirective derives that class
// transitively -- a body handing the receiver's storage to an eraser is a member exactly as
// one spelling the write is -- and this method and (*SecretTree).Zeroize are the two it was
// blind to while its own comment named this one.
//
//go:noinline
func (self *KeySchedule) Zeroize() {
	zeroizeSecret(self.joinerSecret)
	zeroizeSecret(self.welcomeSecret)
	zeroizeSecret(self.epochSecret)
	zeroizeSecret(self.secrets.SenderData)
	zeroizeSecret(self.secrets.Encryption)
	zeroizeSecret(self.secrets.Exporter)
	zeroizeSecret(self.secrets.External)
	zeroizeSecret(self.secrets.Confirmation)
	zeroizeSecret(self.secrets.Membership)
	zeroizeSecret(self.secrets.ResumptionPsk)
	zeroizeSecret(self.secrets.EpochAuthenticator)
	zeroizeSecret(self.secrets.InitSecret)
}
