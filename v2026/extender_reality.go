package connect

// extender_reality.go — the native reality construction of the extender
// camouflage (EXTENDER.md P0, P1).
//
// The camouflage reuses, rather than imports, reality: the authenticated hello
// is built from connect's own X25519, HKDF and AES-GCM and from the reality
// client helpers that already live here for VLESS (vless_reality.go), on the
// extender's own identity key. The proof that the client holds the extender's
// key rides in the 32-byte legacy session id of the client's ClientHello,
// exactly as reality and exactly as the VLESS client already builds it, so
// there is no distinguishing first flight:
//
//	static key   = x25519(hkdf-sha256(identity seed,
//	               info = "ur-extender-reality-x25519-v1"))
//	auth key     = hkdf-sha256(x25519(client ephemeral, server static pub),
//	               salt = hello random[:20], info = "ur-extender-reality-v1")
//	session id   = aes-256-gcm(auth key, nonce = hello random[20:],
//	               plaintext = version (3) | 0 | unix time (4) | short id (8),
//	               aad = the client hello with a zero session id)
//
// The seal and the session-id offset are shared with the VLESS client
// (vlessRealitySealSessionId, vlessRealitySessionIdOffset): the only extender
// specifics are the key derivation, the domain-separation info strings and the
// client version. The server half — the open — has no VLESS counterpart, since
// VLESS is client only, and lives here beside the seal it inverts.
//
// What the tag binds: the identity, because only the holder of the matching
// static private key derives the auth key, and the short id in the plaintext
// ties the tag to this extender's identity key; and the time, in the plaintext,
// checked by the server against a window. The additional data is the whole
// hello, so a tag cannot be lifted onto a different hello: a different random
// or key share changes both the data and the nonce, and the open fails.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/mlkem"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Domain separation of the two HKDF derivations (P1). The static-key info ties
// the X25519 static key to the extender identity; the auth info ties the auth
// key to the extender camouflage and apart from the VLESS "REALITY" label.
const (
	extenderRealityStaticKeyInfo = "ur-extender-reality-x25519-v1"
	extenderRealityAuthKeyInfo   = "ur-extender-reality-v1"
)

// The 3-byte client version the server bounds min and max against, as reality
// does (P1). The client always seals the minimum; the server accepts the whole
// major-2 range so a later minor or patch client is not rejected by an older
// extender.
var (
	extenderRealityClientVersion    = [3]byte{2, 0, 0}
	extenderRealityClientVersionMin = [3]byte{2, 0, 0}
	extenderRealityClientVersionMax = [3]byte{2, 255, 255}
)

// The tls named groups whose client key share carries an X25519 ephemeral
// public key (P1). The extender reads the ephemeral from whichever of these
// the browser hello offered, so nothing extra goes on the wire. Exported so the
// server half (extender package) names the same groups.
const (
	ExtenderRealityGroupX25519         = 29
	ExtenderRealityGroupX25519Mlkem768 = 4588
)

// Length of the x25519 public key, and of the sealed session-id plaintext.
const (
	extenderRealityX25519PublicKeyByteCount    = 32
	extenderRealitySessionIdPlaintextByteCount = 16
)

// ExtenderRealityStaticPrivateKey derives the X25519 static private key of an
// extender identity (P1). It is HKDF-SHA256 of the ed25519 identity seed with
// an empty salt and the static-key info, through crypto/ecdh, so there is no
// new secret to persist and the static key rotates with the identity. Only the
// holder of the seed derives it.
func ExtenderRealityStaticPrivateKey(seed []byte) (*ecdh.PrivateKey, error) {
	keyBytes, err := hkdf.Key(sha256.New, seed, nil, extenderRealityStaticKeyInfo, extenderRealityX25519PublicKeyByteCount)
	if err != nil {
		return nil, err
	}
	return ecdh.X25519().NewPrivateKey(keyBytes)
}

// ExtenderRealityStaticPublicKey is the 32-byte X25519 static public key the
// record publishes (P1, P6) and the activation carries. It is derived from the
// identity seed alone, so the activator and the client learn it without the
// seed ever leaving the extender.
func ExtenderRealityStaticPublicKey(seed []byte) ([]byte, error) {
	privateKey, err := ExtenderRealityStaticPrivateKey(seed)
	if err != nil {
		return nil, err
	}
	return privateKey.PublicKey().Bytes(), nil
}

// ParseExtenderRealityPublicKeyHex decodes and validates the hex form of the
// X25519 static public key carried in the activation args and the record (P6).
// It rejects anything that is not a valid 32-byte X25519 public key. The
// server and the role parse the activation arg through here so the stored and
// signed key is always one the open can use.
func ParseExtenderRealityPublicKeyHex(publicKeyHex string) ([]byte, error) {
	publicKey, err := hex.DecodeString(strings.TrimSpace(publicKeyHex))
	if err != nil {
		return nil, err
	}
	if len(publicKey) != extenderRealityX25519PublicKeyByteCount {
		return nil, fmt.Errorf("extender reality public key must be %d bytes, got %d", extenderRealityX25519PublicKeyByteCount, len(publicKey))
	}
	if _, err := ecdh.X25519().NewPublicKey(publicKey); err != nil {
		return nil, err
	}
	return publicKey, nil
}

// The session-id plaintext (P1): the client version, a reserved zero, the unix
// time and the 8-byte short id of the extender identity key.
func extenderRealitySessionIdPlaintext(shortId []byte, now time.Time) [extenderRealitySessionIdPlaintextByteCount]byte {
	var plaintext [extenderRealitySessionIdPlaintextByteCount]byte
	copy(plaintext[0:3], extenderRealityClientVersion[:])
	plaintext[3] = 0
	binary.BigEndian.PutUint32(plaintext[4:8], uint32(now.Unix()))
	copy(plaintext[8:16], shortId)
	return plaintext
}

// The fields the server checks out of an opened plaintext (P1, P2): the
// version it bounds, the sealed time it windows, and the short id it ties to
// this extender's identity key.
func extenderRealitySessionIdFields(plaintext [extenderRealitySessionIdPlaintextByteCount]byte) (version [3]byte, sealedTime time.Time, shortId []byte) {
	copy(version[:], plaintext[0:3])
	sealedTime = time.Unix(int64(binary.BigEndian.Uint32(plaintext[4:8])), 0)
	shortId = append([]byte(nil), plaintext[8:16]...)
	return version, sealedTime, shortId
}

// Whether a sealed client version is in the bounds the server accepts (P1).
func extenderRealityVersionInBounds(version [3]byte) bool {
	return bytes.Compare(version[:], extenderRealityClientVersionMin[:]) >= 0 &&
		bytes.Compare(version[:], extenderRealityClientVersionMax[:]) <= 0
}

// The auth key both ends derive from the x25519 exchange (P1).
func extenderRealityAuthKey(sharedSecret []byte, helloRandom []byte) ([]byte, error) {
	if len(helloRandom) != 32 {
		return nil, errors.New("extender reality: client hello random must be 32 bytes")
	}
	return hkdf.Key(sha256.New, sharedSecret, helloRandom[:20], extenderRealityAuthKeyInfo, 32)
}

// The client ephemeral X25519 public key carried by a tls 1.3 key share (P1):
// the whole 32 bytes of an X25519 share, and the trailing 32 bytes of an
// X25519MLKEM768 share, which is the ml-kem encapsulation key followed by the
// X25519 public key (crypto/tls, draft-kwiatkowski-tls-ecdhe-mlkem). Any other
// group, or a share of the wrong length, has no ephemeral for the extender.
func extenderRealityClientEphemeralX25519(group uint16, data []byte) ([]byte, bool) {
	switch group {
	case ExtenderRealityGroupX25519:
		if len(data) != extenderRealityX25519PublicKeyByteCount {
			return nil, false
		}
		return data, true
	case ExtenderRealityGroupX25519Mlkem768:
		if len(data) != mlkem.EncapsulationKeySize768+extenderRealityX25519PublicKeyByteCount {
			return nil, false
		}
		return data[mlkem.EncapsulationKeySize768:], true
	default:
		return nil, false
	}
}

// ExtenderRealitySessionIdAuthorized reports whether an opened plaintext names
// a client version the server accepts and the short id of this extender's
// identity key (P1). It is the identity half of the check: the time window is a
// separate test (ExtenderRealitySessionIdInWindow) so the server can count a
// skew failure apart from a wrong key.
func ExtenderRealitySessionIdAuthorized(plaintext [extenderRealitySessionIdPlaintextByteCount]byte, shortId []byte) bool {
	version, _, sealedShortId := extenderRealitySessionIdFields(plaintext)
	if !extenderRealityVersionInBounds(version) {
		return false
	}
	return len(shortId) == len(sealedShortId) && subtleConstantTimeEqual(sealedShortId, shortId)
}

// ExtenderRealitySessionIdInWindow reports whether an opened plaintext's sealed
// time is within window of now, each way (P1). A client whose clock is outside
// it seals a time the server rejects and falls back to the legacy dial.
func ExtenderRealitySessionIdInWindow(plaintext [extenderRealitySessionIdPlaintextByteCount]byte, now time.Time, window time.Duration) bool {
	_, sealedTime, _ := extenderRealitySessionIdFields(plaintext)
	skew := now.Sub(sealedTime)
	if skew < 0 {
		skew = -skew
	}
	return skew <= window
}

// Constant-time equality of two equal-length byte slices, so the short-id check
// does not leak a timing side channel.
func subtleConstantTimeEqual(a []byte, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

// ExtenderRealityOpenSessionId is the server half of the seal (P1, P3): it
// derives the auth key from the extender static private key and the client
// ephemeral read out of the hello's key share, then opens the session-id tag
// under it with the hello (session id zeroed) as additional data. It returns
// the 16-byte plaintext on success and ok false when the group carries no
// X25519 ephemeral, the ephemeral is malformed, or the tag does not open —
// each of which is an unauthenticated hello, never an error. The ECDH is the
// cost the caller gates behind the cheap checks of P2.
func ExtenderRealityOpenSessionId(
	staticPrivateKey *ecdh.PrivateKey,
	helloRaw []byte,
	helloRandom []byte,
	sessionId []byte,
	ephemeralGroup uint16,
	ephemeralData []byte,
) (plaintext [extenderRealitySessionIdPlaintextByteCount]byte, ok bool) {
	if staticPrivateKey == nil || len(helloRandom) != 32 || len(sessionId) != 32 {
		return plaintext, false
	}
	if len(helloRaw) < vlessRealitySessionIdOffset+32 {
		return plaintext, false
	}
	ephemeral, ok := extenderRealityClientEphemeralX25519(ephemeralGroup, ephemeralData)
	if !ok {
		return plaintext, false
	}
	clientPublicKey, err := ecdh.X25519().NewPublicKey(ephemeral)
	if err != nil {
		return plaintext, false
	}
	sharedSecret, err := staticPrivateKey.ECDH(clientPublicKey)
	if err != nil {
		return plaintext, false
	}
	authKey, err := extenderRealityAuthKey(sharedSecret, helloRandom)
	if err != nil {
		return plaintext, false
	}
	block, err := aes.NewCipher(authKey)
	if err != nil {
		return plaintext, false
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return plaintext, false
	}
	// the additional data is the hello with a zero session id, the exact bytes
	// the client sealed against
	aad := append([]byte(nil), helloRaw...)
	clear(aad[vlessRealitySessionIdOffset : vlessRealitySessionIdOffset+32])
	opened, err := aead.Open(nil, helloRandom[20:], sessionId, aad)
	if err != nil || len(opened) != extenderRealitySessionIdPlaintextByteCount {
		return plaintext, false
	}
	copy(plaintext[:], opened)
	return plaintext, true
}
