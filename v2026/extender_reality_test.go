package connect

// Root-cause tests for the extender camouflage crypto (EXTENDER.md P1, P2).
// Deterministic and in-process: a sealed browser hello is built with the real
// client machinery (no network), then opened with the server half, so the seal
// and the open are exercised against each other at the byte level.

import (
	"crypto/ecdh"
	"net"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// Builds a real sealed browser hello without a network: the uTLS Chrome hello
// the client sends, its session id sealed under staticPublicKey for shortId at
// sealTime. Returns the fields the server open reads.
func buildSealedExtenderHelloForTest(
	t *testing.T,
	staticPublicKey []byte,
	shortId []byte,
	sealTime time.Time,
) (helloRaw []byte, helloRandom []byte, sessionId []byte, ephemeralGroup uint16, ephemeralData []byte) {
	t.Helper()
	serverStaticPublicKey, err := ecdh.X25519().NewPublicKey(staticPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := chromeClientHelloSpec(extenderCamouflageAlpn)
	if err != nil {
		t.Fatal(err)
	}
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		clientConn.Close()
		serverConn.Close()
	})
	uconn := utls.UClient(clientConn, &utls.Config{
		ServerName:             "front.example",
		InsecureSkipVerify:     true,
		SessionTicketsDisabled: true,
		OmitEmptyPsk:           true,
	}, utls.HelloCustom)
	t.Cleanup(func() { uconn.Close() })
	if err := uconn.ApplyPreset(spec); err != nil {
		t.Fatal(err)
	}
	if err := uconn.BuildHandshakeState(); err != nil {
		t.Fatal(err)
	}
	hello := uconn.HandshakeState.Hello
	clientEphemeral := extenderCamouflageClientEphemeral(uconn.HandshakeState.State13.KeyShareKeys)
	if clientEphemeral == nil {
		t.Fatal("the browser hello offered no x25519 key share")
	}
	sharedSecret, err := clientEphemeral.ECDH(serverStaticPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	authKey, err := extenderRealityAuthKey(sharedSecret, hello.Random)
	if err != nil {
		t.Fatal(err)
	}
	clear(hello.Raw[vlessRealitySessionIdOffset : vlessRealitySessionIdOffset+32])
	sealed, err := vlessRealitySealSessionId(
		authKey,
		hello.Raw,
		hello.Random,
		extenderRealitySessionIdPlaintext(shortId, sealTime),
	)
	if err != nil {
		t.Fatal(err)
	}
	// the standalone X25519 key share (group 29) is what the client sealed with
	// (Ecdhe) and what the server reads
	parsed := utls.UnmarshalClientHello(hello.Raw)
	if parsed == nil {
		t.Fatal("could not reparse the sealed hello")
	}
	for _, keyShare := range parsed.KeyShares {
		if uint16(keyShare.Group) == ExtenderRealityGroupX25519 {
			ephemeralGroup = uint16(keyShare.Group)
			ephemeralData = keyShare.Data
		}
	}
	if ephemeralData == nil {
		t.Fatal("the sealed hello has no standalone x25519 key share")
	}
	return parsed.Raw, hello.Random, sealed, ephemeralGroup, ephemeralData
}

// The static key is a stable function of the identity seed, and a different seed
// yields a different key (P1): the key rotates with the identity and nothing new
// is persisted.
func TestExtenderRealityStaticKeyDerivationIsStable(t *testing.T) {
	seed, err := NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	first, err := ExtenderRealityStaticPublicKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ExtenderRealityStaticPublicKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("the static key is not a stable function of the seed")
	}
	if len(first) != extenderRealityX25519PublicKeyByteCount {
		t.Fatalf("static key is %d bytes, expected %d", len(first), extenderRealityX25519PublicKeyByteCount)
	}
	otherSeed, err := NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	other, err := ExtenderRealityStaticPublicKey(otherSeed)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) == string(other) {
		t.Fatal("two identities derived the same static key")
	}
}

// A tag sealed under the extender static key opens under its private half, and
// the plaintext carries the short id and a time in the sealed instant (P1).
func TestExtenderRealitySealOpensUnderTheStaticKey(t *testing.T) {
	seed, err := NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	staticPrivateKey, err := ExtenderRealityStaticPrivateKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	staticPublicKey := staticPrivateKey.PublicKey().Bytes()
	shortId := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	sealTime := time.Unix(1_700_000_000, 0)

	raw, random, sessionId, group, data := buildSealedExtenderHelloForTest(t, staticPublicKey, shortId, sealTime)
	plaintext, ok := ExtenderRealityOpenSessionId(staticPrivateKey, raw, random, sessionId, group, data)
	if !ok {
		t.Fatal("the tag did not open under the matching static key")
	}
	if !ExtenderRealitySessionIdAuthorized(plaintext, shortId) {
		t.Fatal("the opened plaintext did not carry the short id")
	}
	if !ExtenderRealitySessionIdInWindow(plaintext, sealTime, time.Minute) {
		t.Fatal("the opened plaintext did not carry the sealed time")
	}
}

// A tag sealed under one static key does not open under another (P1): the ECDH
// yields a different shared secret, so the auth key differs and the GCM open
// fails. This is the root cause of the wrong-key splice.
func TestExtenderRealityOpenRejectsAWrongStaticKey(t *testing.T) {
	seed, err := NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	staticPublicKey, err := ExtenderRealityStaticPublicKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	shortId := []byte{8, 7, 6, 5, 4, 3, 2, 1}
	raw, random, sessionId, group, data := buildSealedExtenderHelloForTest(t, staticPublicKey, shortId, time.Unix(1_700_000_000, 0))

	otherSeed, err := NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	otherPrivateKey, err := ExtenderRealityStaticPrivateKey(otherSeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ExtenderRealityOpenSessionId(otherPrivateKey, raw, random, sessionId, group, data); ok {
		t.Fatal("a tag opened under a static key it was not sealed to")
	}
}

// A tag cannot be lifted onto a different hello (P1): the additional data is the
// whole hello, so flipping a byte outside the session id fails the open.
func TestExtenderRealityOpenRejectsATamperedHello(t *testing.T) {
	seed, err := NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	staticPrivateKey, err := ExtenderRealityStaticPrivateKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	staticPublicKey := staticPrivateKey.PublicKey().Bytes()
	raw, random, sessionId, group, data := buildSealedExtenderHelloForTest(t, staticPublicKey, []byte{1, 1, 1, 1, 1, 1, 1, 1}, time.Unix(1_700_000_000, 0))

	// flip a byte in the random region of the raw hello, which is additional
	// data and not the session id
	tampered := append([]byte(nil), raw...)
	tampered[6] ^= 0xff
	if _, ok := ExtenderRealityOpenSessionId(staticPrivateKey, tampered, random, sessionId, group, data); ok {
		t.Fatal("a tag opened against a tampered hello")
	}
}

// The time window accepts a sealed time within the tolerance each way and
// rejects one outside it (P1), which is the skew the client falls back to legacy
// on.
func TestExtenderRealitySessionIdInWindow(t *testing.T) {
	window := 2 * time.Minute
	sealTime := time.Unix(1_700_000_000, 0)
	var plaintext [extenderRealitySessionIdPlaintextByteCount]byte
	copy(plaintext[0:3], extenderRealityClientVersion[:])
	// encode the sealed time
	plaintext = extenderRealitySessionIdPlaintext([]byte{0, 0, 0, 0, 0, 0, 0, 0}, sealTime)

	for _, c := range []struct {
		name    string
		now     time.Time
		inBound bool
	}{
		{"exact", sealTime, true},
		{"within ahead", sealTime.Add(time.Minute), true},
		{"within behind", sealTime.Add(-time.Minute), true},
		{"past ahead", sealTime.Add(3 * time.Minute), false},
		{"past behind", sealTime.Add(-3 * time.Minute), false},
	} {
		if got := ExtenderRealitySessionIdInWindow(plaintext, c.now, window); got != c.inBound {
			t.Errorf("%s: in window = %v, expected %v", c.name, got, c.inBound)
		}
	}
}

// The authorization check ties a tag to this extender's short id and bounds the
// client version (P1).
func TestExtenderRealitySessionIdAuthorized(t *testing.T) {
	shortId := []byte{9, 9, 9, 9, 9, 9, 9, 9}
	plaintext := extenderRealitySessionIdPlaintext(shortId, time.Unix(1_700_000_000, 0))
	if !ExtenderRealitySessionIdAuthorized(plaintext, shortId) {
		t.Fatal("the matching short id was not authorized")
	}
	otherShortId := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	if ExtenderRealitySessionIdAuthorized(plaintext, otherShortId) {
		t.Fatal("a tag was authorized under a short id it was not sealed for")
	}
	// a version out of bounds is refused
	badVersion := plaintext
	badVersion[0] = 3
	if ExtenderRealitySessionIdAuthorized(badVersion, shortId) {
		t.Fatal("a tag with an out-of-bounds client version was authorized")
	}
}

// The ephemeral extraction reads the whole X25519 share and the trailing 32
// bytes of the X25519MLKEM768 hybrid, and rejects a share of the wrong length
// (P1).
func TestExtenderRealityClientEphemeralX25519(t *testing.T) {
	x25519 := make([]byte, 32)
	for i := range x25519 {
		x25519[i] = byte(i)
	}
	if got, ok := extenderRealityClientEphemeralX25519(ExtenderRealityGroupX25519, x25519); !ok || string(got) != string(x25519) {
		t.Fatalf("x25519 share: got %x ok %v", got, ok)
	}
	hybrid := make([]byte, 1184+32)
	copy(hybrid[1184:], x25519)
	if got, ok := extenderRealityClientEphemeralX25519(ExtenderRealityGroupX25519Mlkem768, hybrid); !ok || string(got) != string(x25519) {
		t.Fatalf("hybrid share: got %x ok %v", got, ok)
	}
	if _, ok := extenderRealityClientEphemeralX25519(ExtenderRealityGroupX25519, x25519[:31]); ok {
		t.Fatal("a short x25519 share was accepted")
	}
	if _, ok := extenderRealityClientEphemeralX25519(23, x25519); ok {
		t.Fatal("a non-x25519 group was accepted")
	}
}

// The activation key parser accepts a valid 32-byte X25519 hex key and rejects a
// short, malformed or wrong-length one (P6).
func TestParseExtenderRealityPublicKeyHex(t *testing.T) {
	seed, err := NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := ExtenderRealityStaticPublicKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	validHex := ""
	for _, b := range publicKey {
		validHex += string("0123456789abcdef"[b>>4]) + string("0123456789abcdef"[b&0xf])
	}
	got, err := ParseExtenderRealityPublicKeyHex("  " + validHex + "\n")
	if err != nil {
		t.Fatalf("a valid key was rejected: %v", err)
	}
	if string(got) != string(publicKey) {
		t.Fatal("the parsed key does not match")
	}
	if _, err := ParseExtenderRealityPublicKeyHex("zzzz"); err == nil {
		t.Fatal("a malformed hex key was accepted")
	}
	if _, err := ParseExtenderRealityPublicKeyHex(validHex[:30]); err == nil {
		t.Fatal("a short key was accepted")
	}
}
