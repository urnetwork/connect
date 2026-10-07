// RFC 9180 HPKE, base mode only, DHKEM(X25519, HKDF-SHA256) with HKDF-SHA256 and
// either ChaCha20-Poly1305 or AES-128-GCM.
//
// MLS uses only single-shot base-mode seal and open, but this file implements the
// full context with its sequence number because the RFC's own test vectors exercise
// the sequence path and a one-shot helper cannot be tested against them.
//
// psk, auth and auth-psk modes are deliberately absent: the v1 profile has no PSKs
// and no external senders, so there is no caller for them and no untested code.
//
// Two of the identifiers this file concatenates collide. RFC 9180 section 7.2 gives
// HKDF-SHA256 the kdf code point 0x0001 and section 7.3 gives AES-128-GCM the aead
// code point 0x0001, in two separate registries, so the two compare equal. suite.go
// declares HpkeKdfId and HpkeAeadId as distinct types, which makes a registry entry
// writing one where the other belongs a compile error — but that does not reach the
// three appends below. binary.BigEndian.AppendUint16 takes a uint16, and the explicit
// conversion it demands is exactly where the typing is discarded, so writing
// uint16(params.AeadId) into the kdf position here still compiles. That is why the
// suite id is pinned by vectors from both registered suites rather than by an equality
// check on a single one: the two agree on the kdf and disagree on the aead, so a
// transposition that is invisible on 0x0001 moves every derived byte on 0x0003.
package mls

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"math"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	hpkeVersionLabel = "HPKE-v1"
	hpkeModeBase     = byte(0x00)
)

// The kdf output size both registered suites fix, and the longest output HKDF-Expand
// can produce from it. RFC 5869 section 2.3 stops the expansion counter at 255, so
// 255*Nh is a hard ceiling rather than a policy: past it there is no key to return.
// This is written against the one kdf the package instantiates rather than read from
// SuiteParams, because the ceiling belongs to HMAC-SHA256 and not to a ciphersuite,
// and a test asserts the registry has not drifted away from it.
const (
	hpkeKdfNh           = sha256.Size
	hpkeMaxExpandLength = 255 * hpkeKdfNh
)

// The kem half of the suite identifier, RFC 9180 section 5.1. Read the kem code point
// and nothing else: the kdf and aead ids are not part of a kem derivation, and a kem
// context that carried them would still produce 32 usable-looking bytes.
func hpkeKemSuiteId(params *SuiteParams) []byte {
	suiteId := make([]byte, 0, 5)
	suiteId = append(suiteId, "KEM"...)
	return binary.BigEndian.AppendUint16(suiteId, uint16(params.KemId))
}

// The whole suite identifier, RFC 9180 section 5.1, in the order the RFC fixes: kem,
// then kdf, then aead. The order is the part worth guarding — see the file comment on
// the 0x0001 collision — and the "HPKE" prefix is what keeps a suite id from being
// mistaken for the kem one above, which is five bytes shorter and starts differently
// for exactly that reason.
func hpkeSuiteId(params *SuiteParams) []byte {
	suiteId := make([]byte, 0, 10)
	suiteId = append(suiteId, "HPKE"...)
	suiteId = binary.BigEndian.AppendUint16(suiteId, uint16(params.KemId))
	suiteId = binary.BigEndian.AppendUint16(suiteId, uint16(params.KdfId))
	return binary.BigEndian.AppendUint16(suiteId, uint16(params.AeadId))
}

// LabeledExtract, RFC 9180 section 4. hkdf.Extract takes (ikm, salt) — the reverse of
// the HKDF-Extract(salt, ikm) the RFC and every spec text in this project write — so
// the swap lives here and nowhere else (spec A section 5.9, guardrail 1), and
// crypto_forbidden_test.go holds it to two files.
//
// The only error hkdf.Extract returns is from the fips140-only mode check, which
// refuses a non approved hash or a short secret. sha256 is approved and the secret
// always carries the seven byte version label, the suite id and a non empty label
// ahead of the caller's material, so under this file's own call sites it cannot fire.
// It is therefore a panic rather than a silently ignored error: a caller handed a nil
// prk would derive keys from nothing and report success.
func hpkeLabeledExtract(suiteId []byte, salt []byte, label string, ikm []byte) []byte {
	labeledIkm := make([]byte, 0, len(hpkeVersionLabel)+len(suiteId)+len(label)+len(ikm))
	labeledIkm = append(labeledIkm, hpkeVersionLabel...)
	labeledIkm = append(labeledIkm, suiteId...)
	labeledIkm = append(labeledIkm, label...)
	labeledIkm = append(labeledIkm, ikm...)
	prk, err := hkdf.Extract(sha256.New, labeledIkm, salt)
	if err != nil {
		panic("mls: hkdf extract failed with a compiled-in sha256: " + err.Error())
	}
	return prk
}

// LabeledExpand, RFC 9180 section 4. The info argument of crypto/hkdf.Expand is typed
// string but is not text; the conversion is byte preserving.
//
// The length is refused here rather than left to hkdf.Expand, because hkdf.Expand does
// not refuse a negative one — it dies on it. crypto/internal/fips140/hkdf opens Expand
// with out := make([]byte, 0, keyLen), reached before the expansion loop and before the
// counter overflow check that loop carries, so a negative keyLen is a makeslice panic
// and the process goes with it. Measured on go1.26.5: -1 panics with "makeslice: cap out
// of range", 8160 returns 8160 bytes, 8161 returns "hkdf: requested key length too
// large". So this guard converts a caller supplied length into a typed error instead of
// a process kill, which is what task 8's Export(exporterContext, length) needs — that
// length comes from a caller, not from the suite.
//
// The upper bound is one bound and not two. 255*Nh is 8160 and the two byte I2OSP prefix
// only misencodes above 65535, so the ceiling refuses every length the prefix could wrap
// long before the prefix could wrap: the uint16 conversion below is lossless because the
// ceiling made it so, and a separate length > 65535 branch here would be unreachable.
// The ceiling is not this package's own policy either — hkdf.Expand enforces the same
// 255*Nh limit, which is the fact hpke_test.go pins the constant against rather than
// pinning it against itself.
func hpkeLabeledExpand(suiteId []byte, prk []byte, label string, info []byte, length int) ([]byte, error) {
	if length < 0 || length > hpkeMaxExpandLength {
		return nil, ErrBadKeyLength
	}
	labeledInfo := make([]byte, 0, 2+len(hpkeVersionLabel)+len(suiteId)+len(label)+len(info))
	labeledInfo = binary.BigEndian.AppendUint16(labeledInfo, uint16(length))
	labeledInfo = append(labeledInfo, hpkeVersionLabel...)
	labeledInfo = append(labeledInfo, suiteId...)
	labeledInfo = append(labeledInfo, label...)
	labeledInfo = append(labeledInfo, info...)
	return hkdf.Expand(sha256.New, prk, string(labeledInfo), length)
}

// A serialized hpke public key, in the SerializePublicKey encoding RFC 9180 section 7.1.1
// fixes for DHKEM(X25519): the raw 32 byte u coordinate, no prefix. Named byte slices
// rather than an interface, because MLS carries them as opaque vectors and every consumer
// needs the bytes anyway — and named at all rather than left as []byte so a signature
// cannot silently take a private key where a public one belongs.
type HpkePublicKey []byte

// A serialized hpke private key, the 32 byte x25519 scalar as DeriveKeyPair expanded it.
// It is deliberately not the clamped form: crypto/ecdh clamps inside the multiplication
// and RFC 9180 serializes the unclamped scalar, so storing the clamped one here would
// disagree with every published vector.
type HpkePrivateKey []byte

// DeriveKeyPair, RFC 9180 section 7.1.3. For DHKEM(X25519) the expanded scalar is used
// directly: there is no rejection sampling — that is the NIST curves' branch of the same
// section — and no clamping, which is x25519's own and happens inside the multiplication.
//
// The kem suite id is the one that belongs here rather than the whole suite id. A key pair
// is a property of the kem alone, so deriving it under the full identifier would give the
// same ikm a different key pair per aead, and the two registered suites would stop sharing
// a keystore for no reason RFC 9180 states.
func HpkeDeriveKeyPair(params *SuiteParams, ikm []byte) (HpkePrivateKey, HpkePublicKey, error) {
	suiteId := hpkeKemSuiteId(params)
	dkpPrk := hpkeLabeledExtract(suiteId, nil, "dkp_prk", ikm)
	scalar, err := hpkeLabeledExpand(suiteId, dkpPrk, "sk", nil, params.Nsk)
	if err != nil {
		return nil, nil, err
	}
	priv, err := X25519PrivateKey(scalar)
	if err != nil {
		return nil, nil, err
	}
	return HpkePrivateKey(priv.Bytes()), HpkePublicKey(priv.PublicKey().Bytes()), nil
}

// The public half of an hpke key pair this package was HANDED rather than one it drew.
//
// It stands beside the derivation above, in one of the four files doc.go names as the whole
// cryptographic surface, for signaturePublicKeyOf's reason at the other end of the same
// argument: recovering a public key from a private one is a cryptographic operation whatever
// file it is written in, and the sentence doc.go makes -- that an audit reads four files -- stops
// being true the moment a fifth expands a key.
//
// IT IS NOT A CryptoProvider METHOD, and that is the other half of the placement.
// signaturePublicKeyOf states the reason in full: the interface is pinned by a gate that reads it
// off the type, every stub and wrapper in the test tree writes out every method by hand, and a
// package level derivation reaches the same primitive without moving that surface. "The provider
// has no private-to-public operation" is therefore a fact about CryptoProvider and never a reason
// a caller cannot ask the question -- which is what it had been read as at the join door, where
// the encryption half of a joiner's own key pair went unchecked on that premise.
//
// WHAT MAKES IT SOUND FOR EVERY REGISTERED SUITE is that both of them are DHKEM(X25519).
// HpkeDeriveKeyPair three lines up builds the pair off one *ecdh.PrivateKey and answers
// HpkePrivateKey(priv.Bytes()), so a private half this package hands out IS the raw x25519 scalar
// and its public half is one multiplication away. It reads no SuiteParams for that reason -- the
// callers hold a CryptoProvider and this is not one of its methods, which is signaturePublicKeyOf's
// position exactly -- and TestEveryRegisteredSuiteNamesTheKemThisDerivationAssumes holds the
// registry to it.
//
// The length gate is X25519PrivateKey's, so a truncated scalar answers ErrBadKeyLength here rather
// than whatever crypto/ecdh makes of it.
func hpkePublicKeyOf(priv HpkePrivateKey) (HpkePublicKey, error) {
	parsed, err := X25519PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return HpkePublicKey(parsed.PublicKey().Bytes()), nil
}

// ExtractAndExpand, RFC 9180 section 4.1: the hash that turns a raw diffie-hellman output
// into a kem shared secret. Skipping it and returning dh would produce 32 bytes that
// round-trip perfectly between an encap and a decap that both skipped it, which is why the
// vector table rather than the round trip is what holds this function in place.
func hpkeExtractAndExpand(params *SuiteParams, dh []byte, kemContext []byte) ([]byte, error) {
	suiteId := hpkeKemSuiteId(params)
	eaePrk := hpkeLabeledExtract(suiteId, nil, "eae_prk", dh)
	return hpkeLabeledExpand(suiteId, eaePrk, "shared_secret", kemContext, params.Nsecret)
}

// Encap, RFC 9180 section 4.1, drawing the ephemeral key from the reader it is handed.
// The reader is a parameter rather than a package level source so a caller can reproduce a
// published encapsulation; it reaches x25519 through X25519GenerateKey, which reads it
// itself because ecdh.GenerateKey no longer does.
func hpkeEncap(random io.Reader, params *SuiteParams, pub HpkePublicKey) ([]byte, []byte, error) {
	ephemeral, err := X25519GenerateKey(random)
	if err != nil {
		return nil, nil, err
	}
	return hpkeEncapDeterministic(params, pub, HpkePrivateKey(ephemeral.Bytes()))
}

// Encap with the ephemeral key supplied, so the RFC's vectors drive production code rather
// than a parallel implementation written for the test. It is the whole of Encap; the
// randomized entry point above is only the key draw in front of it.
//
// kem_context is enc || pkRm and the order is load bearing. Both halves are 32 bytes of
// public key, so a transposition changes no length, returns no error and still agrees with
// a decap transposed the same way — it is visible only against a published shared secret.
// The same is true of which key goes where: pkRm is the recipient's static key and enc is
// this call's ephemeral public key, and writing enc twice, which is what reading the
// ephemeral key for both positions would do, is equally silent.
func hpkeEncapDeterministic(params *SuiteParams, pub HpkePublicKey, ephemeralPriv HpkePrivateKey) ([]byte, []byte, error) {
	if len(ephemeralPriv) != params.Nsk {
		return nil, nil, ErrBadKeyLength
	}
	if len(pub) != params.Npk {
		return nil, nil, ErrBadKeyLength
	}
	ephemeral, err := X25519PrivateKey(ephemeralPriv)
	if err != nil {
		return nil, nil, err
	}
	recipient, err := X25519PublicKey(pub)
	if err != nil {
		return nil, nil, err
	}
	dh, err := X25519DH(ephemeral, recipient)
	if err != nil {
		return nil, nil, err
	}
	kemOutput := ephemeral.PublicKey().Bytes()
	kemContext := make([]byte, 0, len(kemOutput)+len(pub))
	kemContext = append(kemContext, kemOutput...)
	kemContext = append(kemContext, pub...)
	sharedSecret, err := hpkeExtractAndExpand(params, dh, kemContext)
	if err != nil {
		return nil, nil, err
	}
	return sharedSecret, kemOutput, nil
}

// Decap, RFC 9180 section 4.1. The recipient's half of the kem context is recomputed from
// its own private key rather than taken from a caller, which is the only way the two sides
// can be made to agree on pkRm without a second wire field to get wrong — and it is why a
// decap that put its own key first would still round trip against nothing but itself.
//
// A wrong length is refused before the curve rather than at it, and the two lengths get
// different sentinels on purpose: a kem output is a peer's bytes off the wire, while a
// private key of the wrong length is this process's own bug, and a caller triaging the two
// needs to tell them apart.
func hpkeDecap(params *SuiteParams, priv HpkePrivateKey, kemOutput []byte) ([]byte, error) {
	if len(kemOutput) != params.Nenc {
		return nil, ErrBadKemOutput
	}
	if len(priv) != params.Nsk {
		return nil, ErrBadKeyLength
	}
	recipient, err := X25519PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	ephemeral, err := X25519PublicKey(kemOutput)
	if err != nil {
		return nil, err
	}
	dh, err := X25519DH(recipient, ephemeral)
	if err != nil {
		return nil, err
	}
	recipientPub := recipient.PublicKey().Bytes()
	kemContext := make([]byte, 0, len(kemOutput)+len(recipientPub))
	kemContext = append(kemContext, kemOutput...)
	kemContext = append(kemContext, recipientPub...)
	return hpkeExtractAndExpand(params, dh, kemContext)
}

// An established hpke context: the aead the key schedule produced, the base nonce that
// aead is used with, the exporter secret beside it, and the sequence number that keeps
// one message's nonce off every other message's.
//
// Not safe for concurrent use, and that is a property of the construction rather than an
// omission. Seal and Open each read the sequence number and then advance it, so two
// goroutines sealing at once take the same one and encrypt two messages under one nonce
// — which for chacha20-poly1305 and for aes-gcm alike hands an observer the xor of the
// two plaintexts and the material to forge under that key. Every caller in this tree owns
// its context for the length of one message.
type HpkeContext struct {
	params         *SuiteParams
	suiteId        []byte
	aead           cipher.AEAD
	baseNonce      []byte
	exporterSecret []byte
	sequence       uint64
}

// The aead a suite names, over a key of exactly the length that suite fixes. The length
// is refused here rather than left to the two constructors so a wrong key is one
// sentinel instead of two library specific error strings, and so the refusal reads the
// registry's own Nk rather than a constructor's opinion of what a key should be.
func hpkeNewAead(params *SuiteParams, key []byte) (cipher.AEAD, error) {
	if len(key) != params.Nk {
		return nil, ErrBadKeyLength
	}
	switch params.AeadId {
	case HpkeAeadChaCha20Poly1305:
		return chacha20poly1305.New(key)
	case HpkeAeadAes128Gcm:
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		return cipher.NewGCM(block)
	default:
		return nil, ErrUnknownCipherSuite
	}
}

// key_schedule_context for mode_base, RFC 9180 section 5.1: the mode byte, then the hash
// of the psk id, then the hash of the info, in that order and no other.
//
// The preimage is built here rather than inline in the key schedule because its order is
// the one mistake in this file that nothing downstream can see. Both hashes are 32 bytes
// out of the same kdf, so transposing them yields a context of exactly the right length
// that the key, the base nonce and the exporter secret all follow consistently, and a
// sender and a receiver transposed alike agree with each other on every byte. The mode is
// the same shape of mistake: 0x01 where 0x00 belongs moves every derived value and breaks
// nothing a round trip can observe. Only the published context separates any of them, and
// returning the preimage is what lets it be compared against one directly instead of
// through three expansions that each blame the wrong thing.
func hpkeKeyScheduleContext(suiteId []byte, info []byte) []byte {
	pskIdHash := hpkeLabeledExtract(suiteId, nil, "psk_id_hash", nil)
	infoHash := hpkeLabeledExtract(suiteId, nil, "info_hash", info)
	keyScheduleContext := make([]byte, 0, 1+len(pskIdHash)+len(infoHash))
	keyScheduleContext = append(keyScheduleContext, hpkeModeBase)
	keyScheduleContext = append(keyScheduleContext, pskIdHash...)
	return append(keyScheduleContext, infoHash...)
}

// KeySchedule for mode_base, RFC 9180 section 5.1. psk and psk_id are the empty defaults,
// which is not a shortcut taken here: the v1 profile has no psks at all, which is the
// same reason the psk, auth and auth-psk modes are absent from the file.
//
// The three expansions read three different suite fields — Nk, Nn and Nh — and the last
// of those holds the same 32 that six other fields hold in both registered suites, so a
// derivation that read one of those instead is invisible to any vector. hpke_test.go
// separates them with probe suites for that reason.
//
// The aead's own nonce size is checked against Nn rather than assumed to equal it. The
// base nonce is expanded to Nn and ComputeNonce sizes its output the same way, so a suite
// whose Nn disagreed with its aead would hand cipher.AEAD a nonce of the wrong length —
// and both aeads here panic on that rather than returning an error, so the first Seal
// would take the process with it. Refusing at construction turns that into a typed error
// before any key exists, and it is what makes the eight byte counter in ComputeNonce fit
// by construction rather than by assumption.
func hpkeKeySchedule(params *SuiteParams, sharedSecret []byte, info []byte) (*HpkeContext, error) {
	suiteId := hpkeSuiteId(params)
	keyScheduleContext := hpkeKeyScheduleContext(suiteId, info)

	secret := hpkeLabeledExtract(suiteId, sharedSecret, "secret", nil)
	key, err := hpkeLabeledExpand(suiteId, secret, "key", keyScheduleContext, params.Nk)
	if err != nil {
		return nil, err
	}
	baseNonce, err := hpkeLabeledExpand(suiteId, secret, "base_nonce", keyScheduleContext, params.Nn)
	if err != nil {
		return nil, err
	}
	exporterSecret, err := hpkeLabeledExpand(suiteId, secret, "exp", keyScheduleContext, params.Nh)
	if err != nil {
		return nil, err
	}
	aead, err := hpkeNewAead(params, key)
	if err != nil {
		return nil, err
	}
	if aead.NonceSize() != params.Nn {
		return nil, ErrBadNonceLength
	}
	return &HpkeContext{
		params:         params,
		suiteId:        suiteId,
		aead:           aead,
		baseNonce:      baseNonce,
		exporterSecret: exporterSecret,
		sequence:       0,
	}, nil
}

// ComputeNonce, RFC 9180 section 5.2: base_nonce xor I2OSP(seq, Nn). The counter is big
// endian and right aligned, so it occupies the low bytes and a sequence number past 255
// moves the byte above them — which is exactly where an implementation that wrote the
// counter little endian, or at the front, agrees with itself for the first 256 messages
// and with nobody else ever.
func (self *HpkeContext) nonce() []byte {
	nonce := make([]byte, self.params.Nn)
	binary.BigEndian.PutUint64(nonce[self.params.Nn-8:], self.sequence)
	for i := range nonce {
		nonce[i] ^= self.baseNonce[i]
	}
	return nonce
}

// IncrementSeq, RFC 9180 section 5.2. The RFC's own limit is 2^(8*Nn)-1, which for the 12
// byte nonce both registered suites use is far past anything a uint64 holds, so what
// binds here is the counter's width: at the maximum the next increment wraps to zero and
// repeats a nonce under a key already used, which loses confidentiality and authenticity
// of every message on both sides of the repeat. The context stops instead of rolling
// over, and stopping is the whole of the recovery — a context that has run out is
// finished, not resettable.
func (self *HpkeContext) advance() error {
	if self.sequence == math.MaxUint64 {
		return ErrSequenceOverflow
	}
	self.sequence++
	return nil
}

// ContextS.Seal, RFC 9180 section 5.2: seal at the current sequence number, then advance.
// On the advance's refusal the ciphertext is dropped rather than returned, because it was
// produced at a sequence number the context cannot move past and handing it to a caller
// would put the next message on the same nonce.
func (self *HpkeContext) Seal(aad []byte, plaintext []byte) ([]byte, error) {
	ciphertext := self.aead.Seal(nil, self.nonce(), plaintext, aad)
	if err := self.advance(); err != nil {
		return nil, err
	}
	return ciphertext, nil
}

// ContextR.Open, RFC 9180 section 5.2. A failure is always ErrAeadOpen: which of the key,
// the nonce, the aad and the ciphertext was wrong is nothing a caller can act on and
// nothing a peer gets to learn from the error it provoked.
//
// The sequence advances only on success. A receiver that advanced on failure could be
// pushed past its sender by one injected packet, and every genuine message after it would
// then open under the wrong nonce and be refused — a denial of service available to
// anyone who can write to the transport.
func (self *HpkeContext) Open(aad []byte, ciphertext []byte) ([]byte, error) {
	plaintext, err := self.aead.Open(nil, self.nonce(), ciphertext, aad)
	if err != nil {
		return nil, ErrAeadOpen
	}
	if err := self.advance(); err != nil {
		return nil, err
	}
	return plaintext, nil
}

// Context.Export, RFC 9180 section 5.3. It is keyed on the exporter secret rather than on
// the aead key, so an exported value outlives a context whose messages are spent and can
// never be confused with one of them, and it leaves the sequence number alone, so
// exporting interleaves with sealing and cannot silently cost a message.
//
// The length is the caller's, and it is the only one in this file that is: every other
// expansion here takes a suite field. That makes hpkeLabeledExpand's guard load bearing
// rather than defensive — crypto/hkdf.Expand dies on a negative length instead of
// refusing it, so the guard is what stands between a caller's arithmetic and the process.
// The sentinel it returns is ErrBadKeyLength rather than an export specific one: the two
// fail at the same guard for the same reason and leave a caller the same nothing to do,
// and a second sentinel would have to be threaded through the crypto error contract to
// say so.
func (self *HpkeContext) Export(exporterContext []byte, length int) ([]byte, error) {
	return hpkeLabeledExpand(self.suiteId, self.exporterSecret, "sec", exporterContext, length)
}

// SetupBaseS, RFC 9180 section 5.1.1: draw an ephemeral key pair, encapsulate to the
// recipient, and turn the resulting shared secret into a sending context. The returned
// bytes are that call's encapsulated key, which the recipient needs and which is public.
//
// A context is built here and never cached. The ephemeral key is what makes two
// encapsulations to one recipient produce two unrelated keys, so anything that held a
// context or an ephemeral key across calls would put a second message on the first
// message's key at the first message's sequence number — one nonce, two plaintexts, and
// the aead's confidentiality and authenticity both gone. Nothing in this file has a
// package level variable for that reason.
//
// The shared secret is not returned. It and the encapsulated key are both 32 bytes under
// the registered kem, so a caller handed both could pass either into a key schedule and
// the mistake would only be visible against a published vector; the context is the only
// thing a caller has any use for, so it is the only thing that leaves.
func HpkeSetupBaseS(random io.Reader, params *SuiteParams, pub HpkePublicKey, info []byte) ([]byte, *HpkeContext, error) {
	sharedSecret, kemOutput, err := hpkeEncap(random, params, pub)
	if err != nil {
		return nil, nil, err
	}
	ctx, err := hpkeKeySchedule(params, sharedSecret, info)
	if err != nil {
		return nil, nil, err
	}
	return kemOutput, ctx, nil
}

// SetupBaseR, RFC 9180 section 5.1.1: decapsulate a sender's encapsulated key and build
// the receiving context from it. The context starts at sequence zero, which is where the
// sender's first message was sealed, and the two run in step from there.
//
// info is the sender's and it is bound into the key schedule rather than checked against
// anything, so a receiver that expected a different one does not learn that here — it
// learns it when the first Open refuses. That is the RFC's design and it is what makes
// the MLS label and group context, which travel through this argument, unforgeable rather
// than merely declared.
func HpkeSetupBaseR(params *SuiteParams, priv HpkePrivateKey, kemOutput []byte, info []byte) (*HpkeContext, error) {
	sharedSecret, err := hpkeDecap(params, priv, kemOutput)
	if err != nil {
		return nil, err
	}
	return hpkeKeySchedule(params, sharedSecret, info)
}

// SealBase, the single-shot api of RFC 9180 section 6.1 and the only form MLS uses: set
// up a context, seal exactly one message with it, and drop it.
//
// Dropping it is the point. The context is a local, so every call encapsulates under a
// fresh ephemeral key and seals at sequence zero under a key no other call holds. An
// implementation that kept the context instead — to save an encapsulation, to memoize on
// the recipient key, or by reaching for a package level one — would either seal the
// second message at sequence one, which no single-shot receiver can open because it
// builds its own context at zero, or reset the sequence and seal two plaintexts under one
// key and one nonce. The second of those round trips perfectly and is a total break: for
// both aeads here the ciphertexts differ from the plaintexts by the same keystream, so
// their xor is the xor of the plaintexts, and the authenticator's key falls out of the
// pair as well. hpke_test.go measures exactly that xor rather than trusting the round
// trip, which cannot see it.
//
// The two returned slices are the encapsulated key and the ciphertext, in that order, and
// both are []byte — a transposed return compiles. What separates them is that the
// encapsulated key is Nenc bytes and the ciphertext is the plaintext plus Nt, so the
// vector known answers and the length assertion beside them are what hold the order.
func HpkeSealBase(random io.Reader, params *SuiteParams, pub HpkePublicKey, info []byte, aad []byte, plaintext []byte) ([]byte, []byte, error) {
	kemOutput, ctx, err := HpkeSetupBaseS(random, params, pub, info)
	if err != nil {
		return nil, nil, err
	}
	ciphertext, err := ctx.Seal(aad, plaintext)
	if err != nil {
		return nil, nil, err
	}
	return kemOutput, ciphertext, nil
}

// OpenBase, RFC 9180 section 6.1. The context is per call here for the same reason it is
// on the sending side, with one more: a receiving context that survived a call would
// advance its sequence number past the zero every single-shot message is sealed at, and
// would then refuse every message after the first.
//
// info and aad are both authenticated and neither is recoverable from the ciphertext, so
// a message sealed under either of them opens under nothing else — info because it moves
// the whole key schedule, aad because the aead's tag covers it. Both come back as
// ErrAeadOpen, since which one a peer got wrong is not something the peer gets to learn
// from the error it provoked.
func HpkeOpenBase(params *SuiteParams, priv HpkePrivateKey, kemOutput []byte, info []byte, aad []byte, ciphertext []byte) ([]byte, error) {
	ctx, err := HpkeSetupBaseR(params, priv, kemOutput, info)
	if err != nil {
		return nil, err
	}
	return ctx.Open(aad, ciphertext)
}
