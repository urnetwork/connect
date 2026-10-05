// The record aead: the one authenticated encryption a record's two ciphertexts are built
// with, and the algorithm identifier that names it inside both of their preimages.
//
// MASTER section 8 pins the identifier and the primitive together and spec A section 5 states
// neither, which is why they are declared in one file rather than beside the builders that
// take them. MASTER section 8's own words:
//
//	alg_id in both record AADs is 0x0021, XChaCha20-Poly1305 [...] The derivation above
//	settles which: it hands out key_head | nonce_head as 56 octets, a 32-octet key and a
//	24-octet nonce, and a 24-octet nonce is XChaCha20-Poly1305's and no other v1 suite's.
//	0x0031 (HKDF-SHA-256) names the function that produced that key, not the one that
//	consumes it, and a client that wrote it here would build a preimage that round-trips
//	against itself and fails the AEAD against every other implementation, on every record
//	it sends.
//
// MASTER section 7.1's registry carries the same code point. Spec A section 5.1 to 5.5
// mention it nowhere, which is open item M1-10: MASTER is normative here and the omission is
// spec A's to repair.
//
// The identifier lives here and not beside AADHead and AADBody, which are one package over in
// connect/message and take algId as a bare argument. That is deliberate on both sides.
// The builders take the identifier so that the aad builder does not know which aead is in
// use; this file declares it because until the value and the primitive are fixed together
// there is no known answer test anybody can write, and a constant that named a primitive some
// other file chose would be a second place for the two to disagree. Every caller passes this
// constant, and the gate that says so is task 11's -- the class of callers is empty until
// SealRecord exists, and a gate over an empty class reports clean having read nothing.
//
// The hazard this file exists to close is two characters wide. chacha20poly1305.New takes a
// twelve octet nonce and chacha20poly1305.NewX takes twenty four; a build that reached for the
// first against section 5.3's fifty six octet expansion would silently use twelve octets of
// the nonce, discard the other twelve, and still round trip against itself on every record it
// wrote. Nothing but a second implementation, or the width refusals below, can tell the two
// apart. So the nonce width is checked before the primitive is constructed, with a sentinel of
// its own, rather than left to the library's own key length check -- which would not catch it
// at all, because both constructions take the same key.
//
// Neither half is exported. Nothing outside this package seals or opens a record: spec A
// section 12.1 gives the message server no decryption function, and connect/message -- the
// half the server links -- cannot import this package at all.
package messagegroup

import (
	"crypto/cipher"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

// RecordAeadAlgId is the algorithm identifier MASTER section 7.1 registers for
// XChaCha20-Poly1305, carried inside aad_head and aad_body so that the aead a record was
// sealed under is authenticated by the record itself and cannot be stripped or downgraded on
// the way.
const RecordAeadAlgId uint16 = 0x0021

// The three widths of the construction, each read off the library rather than written down.
//
// Writing 32, 24 and 16 here would put this file's opinion of the primitive beside the
// primitive, which is exactly the drift the aad's alg_id exists to make impossible: a
// literal 12 in place of NonceSizeX is the whole hazard the file comment describes, and a
// literal 16 is a tag width the size bucket ladder in connect/message also states. That
// ladder's tag is asserted against this one in the tests rather than shared as a constant,
// because connect/message must never import this package.
const (
	recordAeadKeyBytes   = chacha20poly1305.KeySize
	recordAeadNonceBytes = chacha20poly1305.NonceSizeX
	recordAeadTagBytes   = chacha20poly1305.Overhead
)

// The one construction, so the seal and the open cannot disagree about which variant they run
// and so both refusals are made in one place, before any arithmetic.
//
// It is NewX and never New. The two differ by one character in the name and by twelve octets
// in the nonce, and the file comment argues at length that nothing observable inside this
// package can tell them apart.
func newRecordAead(key []byte, nonce []byte) (cipher.AEAD, error) {
	if len(key) != recordAeadKeyBytes {
		return nil, fmt.Errorf("%w: %d octets, want %d", ErrRecordAeadKeyLength, len(key), recordAeadKeyBytes)
	}
	if len(nonce) != recordAeadNonceBytes {
		return nil, fmt.Errorf("%w: %d octets, want %d", ErrRecordAeadNonceLength, len(nonce), recordAeadNonceBytes)
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		// unreachable, and a panic rather than a second route to ErrRecordAeadKeyLength. The
		// only error this constructor returns is a bad key length, which the check above has
		// already refused, so anything arriving here is this process's own bug; answering the
		// width sentinel a second time would make that sentinel mean two things and would let a
		// deleted check keep reporting the same refusal it used to. Neither the key nor the
		// nonce comes from the network -- both are this member's own expansion of a record key
		// -- so nothing remote can reach it.
		panic(fmt.Errorf("messagegroup: the record aead refused a %d octet key that passed the width check: %w", len(key), err))
	}
	return aead, nil
}

// sealRecordAead seals one of a record's two plaintexts under its own key, its own nonce and
// its own additional authenticated data.
//
// The aad is not optional, and that is now a refusal rather than a sentence. ct_head is sealed
// against aad_head and ct_body against aad_body, which is MASTER invariant I7, and the two
// preimages differ in their label before they differ in anything else. This header used to
// claim the aad "is never nil in practice" while nothing enforced it: an empty aad sealed and
// returned a ciphertext whose epoch, stream index, sender handle and retention class were
// authenticated by nothing, and it opened again just as happily against the same nothing. The
// open side is deliberately NOT given the same refusal -- an empty aad on that side is a
// ciphertext that fails to authenticate, which is the answer it should get, and a width check
// there would answer a different error to an attacker's choice of input.
//
// Nothing is appended to a caller's buffer -- the destination is nil at every call -- so the
// ciphertext is a fresh allocation and an aliased plaintext cannot be produced.
func sealRecordAead(key []byte, nonce []byte, aad []byte, plaintext []byte) ([]byte, error) {
	if len(aad) == 0 {
		return nil, fmt.Errorf("%w: %d octets of plaintext", ErrRecordAeadAadMissing, len(plaintext))
	}
	aead, err := newRecordAead(key, nonce)
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, nonce, plaintext, aad), nil
}

// openRecordAead opens one of a record's two ciphertexts, or refuses.
//
// It answers no plaintext on a refusal, and that is the whole of its contract beyond the
// widths. THE GUARANTEE IS THE LIBRARY'S AND NOT THIS FILE'S, which is worth writing down
// because the sentence that used to stand here read as though this line were what held it:
// today's chacha20poly1305.Open answers a nil slice on a tag failure, so returning "plaintext"
// beside the error instead of nil is a mutation no test in this tree can tell apart. What the
// explicit nil buys is that the contract does not MOVE if the library's does -- a caller that
// ignored the error holds nothing rather than a prefix of unauthenticated octets, whatever
// crypto/cipher decides to do with its destination next. The underlying error is not
// wrapped: it says only "message authentication failed", it is the same for every cause, and a
// caller distinguishing causes here would be distinguishing what an attacker chose.
func openRecordAead(key []byte, nonce []byte, aad []byte, ciphertext []byte) ([]byte, error) {
	aead, err := newRecordAead(key, nonce)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("%w: %d octets of ciphertext", ErrRecordAeadOpen, len(ciphertext))
	}
	return plaintext, nil
}
