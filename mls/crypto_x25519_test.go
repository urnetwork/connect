// What the one x25519 call site must do, and the part of it a smoke test cannot see.
// Agreement alone is nearly free: a function returning a constant, or returning its own
// argument, makes both parties agree on 32 bytes and passes a round trip unchallenged.
// So the round trip is joined here by two things it cannot state — that the secret is
// the one RFC 7748 publishes for a known pair of keys, and that it moves when the peer
// key moves — and by the refusals, which are the reason this wrapper exists at all.
//
// The low order case is the whole point of guardrail 3. crypto/ecdh accepts any 32 byte
// u coordinate and refuses at the exchange, so the test that names that refusal counts
// how many points actually reached it: a parser that rejected everything would otherwise
// skip the loop body and report success. A blacklist of literals is only half an answer
// there, because the encoding is masked and reduced before it is used, so the same point
// arrives under several spellings; those are derived rather than transcribed.
//
// Key generation gets its own tests because the standard library's does not honour the
// reader it is handed. Delegating would leave the randomness parameter untestable and
// silently non-deterministic, so the two tests below state the opposite: a fixed reader
// fixes the key, and a broken one is an error rather than a key from somewhere else. A
// fixed reader is not enough on its own, though — a constant byte one is blind to every
// permutation of the scalar — so the ordering is stated with a scripted reader against a
// published vector.
//
// The last thing a smoke test cannot see is that the parsers hand back the bytes they
// were given. The curve masks and reduces the encoding itself, so a wrapper that
// normalised first would change no secret anywhere in this file; that claim therefore
// gets its own test rather than riding along with one about lengths.
//
// There is no hex helper declared here. This package has exactly one and it is the
// interop harness's MustHex, which has not landed yet, so the vectors below decode
// inline rather than growing a second one that would collide with it.
package mls

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"slices"
	"testing"
)

// The RFC 7748 section 6.1 key pairs. Both are also carried by the standard library's
// own crypto/ecdh vector table, which is where the values here were checked against
// something other than this package.
const (
	rfc7748AlicePrivate = "77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a"
	rfc7748AlicePublic  = "8520f0098930a754748b7ddcb43ef75a0dbf3a0d26381af4eba4a98eaa9b4e6a"
	rfc7748BobPrivate   = "5dab087e624a8a4b79e17f8b83800ee66f3bb1292618b6fd1c2f8b27ff88e0eb"
	rfc7748BobPublic    = "de9edb7d7b7dc1b4d35b61c2ece435373f8343c85b78674dadfc7e146f882b4f"
	rfc7748SharedSecret = "4a5d9d5ba4ce2de1728e3bf480350f25e07e21c947d19e3376f09b3c1e161742"
)

// One published exchange: a scalar, a peer u coordinate and the answer. The section 5.2
// row is not a two party exchange at all but a bare scalar multiplication, which is why
// its peer key is not any party's public key — it is there so the table holds a u
// coordinate that was never produced by this package's own key generation.
var x25519ExchangeVectors = []struct {
	name          string
	privateKey    string
	peerPublicKey string
	sharedSecret  string
}{
	{
		name:          "rfc 7748 section 6.1, alice to bob",
		privateKey:    rfc7748AlicePrivate,
		peerPublicKey: rfc7748BobPublic,
		sharedSecret:  rfc7748SharedSecret,
	},
	{
		name:          "rfc 7748 section 6.1, bob to alice",
		privateKey:    rfc7748BobPrivate,
		peerPublicKey: rfc7748AlicePublic,
		sharedSecret:  rfc7748SharedSecret,
	},
	{
		name:          "rfc 7748 section 5.2, first vector",
		privateKey:    "a546e36bf0527c9d3b16154b82465edd62144c0ac1fc5a18506a2244ba449ac4",
		peerPublicKey: "e6db6867583030db3594c1a424b15f7c726624ec26b3353b10a903a6d0ab1c4c",
		sharedSecret:  "c3da55379de9c6908e94ea4df28d084f32eccf03491c71f754b4075577a28552",
	},
}

// The public key each published scalar derives to. Separate from the exchange table
// because the scalar of the section 5.2 vector has no published public key, and a row
// carrying an empty expectation would be a row asserting nothing.
var x25519DerivationVectors = []struct {
	name       string
	privateKey string
	publicKey  string
}{
	{name: "rfc 7748 section 6.1, alice", privateKey: rfc7748AlicePrivate, publicKey: rfc7748AlicePublic},
	{name: "rfc 7748 section 6.1, bob", privateKey: rfc7748BobPrivate, publicKey: rfc7748BobPublic},
}

// The small subgroup u coordinates every x25519 implementation blacklists, in canonical
// form: zero, one, and the two that generate the order eight torsion. Clamping makes
// every scalar a multiple of eight, so all four drive the x25519 output to zero, which
// is the case the banned sdk.GenerateSharedSecret answers with an all zero secret and a
// nil error.
var x25519LowOrderPoints = [][]byte{
	make([]byte, x25519KeySize),
	{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	{0xe0, 0xeb, 0x7a, 0x7c, 0x3b, 0x41, 0xb8, 0xae, 0x16, 0x56, 0xe3, 0xfa, 0xf1, 0x9f, 0xc4, 0x6a,
		0xda, 0x09, 0x8d, 0xeb, 0x9c, 0x32, 0xb1, 0xfd, 0x86, 0x62, 0x05, 0x16, 0x5f, 0x49, 0xb8, 0x00},
	{0x5f, 0x9c, 0x95, 0xbc, 0xa3, 0x50, 0x8c, 0x24, 0xb1, 0xd0, 0xb1, 0x55, 0x9c, 0x83, 0xef, 0x5b,
		0x04, 0x44, 0x5c, 0xc4, 0x58, 0x1c, 0x8e, 0x86, 0xd8, 0x22, 0x4e, 0xdd, 0xd0, 0x9f, 0x11, 0x57},
}

// A reader that never yields, so the entropy failure path is exercised rather than
// assumed. The error is the test's own value, which is what lets the assertion say the
// reader's error came back rather than that some error did.
type failingReader struct {
	err error
}

func (self failingReader) Read(p []byte) (int, error) {
	return 0, self.err
}

// A reader that yields one byte value forever. Two readers built the same way are the
// same entropy source, which is what makes the determinism claim below checkable.
type constantReader struct {
	value byte
}

func (self constantReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = self.value
	}
	return len(p), nil
}

// A reader with fewer bytes than a key needs. A short read is the failure a length check
// after the fact would miss, since the tail of the buffer would be zeros nobody chose.
type shortReader struct {
	remaining int
}

func (self *shortReader) Read(p []byte) (int, error) {
	if self.remaining <= 0 {
		return 0, io.EOF
	}
	n := min(len(p), self.remaining)
	for i := range n {
		p[i] = 0xa5
	}
	self.remaining -= n
	return n, nil
}

// The curve25519 field prime, 2^255 - 19.
func x25519FieldPrime() *big.Int {
	prime := new(big.Int).Lsh(big.NewInt(1), 255)
	return prime.Sub(prime, big.NewInt(19))
}

// A little endian u coordinate as a number, so the encodings of one point can be
// enumerated rather than transcribed from a list someone else assembled.
func x25519CoordinateOf(encoded []byte) *big.Int {
	bigEndian := slices.Clone(encoded)
	slices.Reverse(bigEndian)
	return new(big.Int).SetBytes(bigEndian)
}

// Every 32 byte string that denotes u. RFC 7748 section 5 masks bit 255 of the encoding
// and then reduces modulo the prime, so u, u + p, and either of those with bit 255 set
// are up to four spellings of one point; u + p only survives the mask while it stays
// below bit 255, and anything that would not fit in 32 bytes is not an encoding at all.
// The canonical form is first.
func x25519EncodingsOf(u *big.Int) [][]byte {
	highBit := new(big.Int).Lsh(big.NewInt(1), 255)
	limit := new(big.Int).Lsh(big.NewInt(1), 256)

	congruent := []*big.Int{u}
	if plusPrime := new(big.Int).Add(u, x25519FieldPrime()); plusPrime.Cmp(highBit) < 0 {
		congruent = append(congruent, plusPrime)
	}
	encodings := [][]byte{}
	for _, value := range congruent {
		for _, spelling := range []*big.Int{value, new(big.Int).Add(value, highBit)} {
			if spelling.Cmp(limit) >= 0 {
				continue
			}
			encoded := make([]byte, x25519KeySize)
			spelling.FillBytes(encoded)
			slices.Reverse(encoded)
			encodings = append(encodings, encoded)
		}
	}
	return encodings
}

// TestX25519RoundTrip is the plan's agreement check: two freshly generated parties reach
// the same 32 bytes from opposite sides. On its own it is satisfied by any symmetric
// function, which is what the known answer and sensitivity tests below are for.
func TestX25519RoundTrip(t *testing.T) {
	a, err := X25519GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate a: %v", err)
	}
	b, err := X25519GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate b: %v", err)
	}
	ab, err := X25519DH(a, b.PublicKey())
	if err != nil {
		t.Fatalf("dh ab: %v", err)
	}
	ba, err := X25519DH(b, a.PublicKey())
	if err != nil {
		t.Fatalf("dh ba: %v", err)
	}
	if !bytes.Equal(ab, ba) {
		t.Fatalf("shared secrets differ: %x vs %x", ab, ba)
	}
	if len(ab) != x25519KeySize {
		t.Fatalf("shared secret is %d bytes, want %d", len(ab), x25519KeySize)
	}
}

// TestX25519KnownAnswer pins the output bytes themselves against RFC 7748, which is the
// only assertion here that a constant returning implementation cannot satisfy. It is
// also the only exercise of the two parsers on their accepting path, so a parser that
// refused everything fails here rather than quietly emptying the tests below.
func TestX25519KnownAnswer(t *testing.T) {
	for _, vector := range x25519ExchangeVectors {
		privateBytes, err := hex.DecodeString(vector.privateKey)
		if err != nil {
			t.Fatalf("%s: private key hex: %v", vector.name, err)
		}
		peerBytes, err := hex.DecodeString(vector.peerPublicKey)
		if err != nil {
			t.Fatalf("%s: peer public key hex: %v", vector.name, err)
		}
		want, err := hex.DecodeString(vector.sharedSecret)
		if err != nil {
			t.Fatalf("%s: shared secret hex: %v", vector.name, err)
		}
		priv, err := X25519PrivateKey(privateBytes)
		if err != nil {
			t.Errorf("%s: X25519PrivateKey: %v", vector.name, err)
			continue
		}
		pub, err := X25519PublicKey(peerBytes)
		if err != nil {
			t.Errorf("%s: X25519PublicKey: %v", vector.name, err)
			continue
		}
		secret, err := X25519DH(priv, pub)
		if err != nil {
			t.Errorf("%s: X25519DH: %v", vector.name, err)
			continue
		}
		if !bytes.Equal(secret, want) {
			t.Errorf("%s: shared secret = %x, want %x", vector.name, secret, want)
		}
	}
}

// TestX25519PrivateKeyDerivesThePublishedPublicKey states that a parsed scalar carries
// the key pair RFC 7748 says it does. Without it a parser could return some other
// party's key and every exchange in the table above would still agree, since both sides
// of a vector are checked against the published secret rather than against each other.
func TestX25519PrivateKeyDerivesThePublishedPublicKey(t *testing.T) {
	for _, vector := range x25519DerivationVectors {
		privateBytes, err := hex.DecodeString(vector.privateKey)
		if err != nil {
			t.Fatalf("%s: private key hex: %v", vector.name, err)
		}
		want, err := hex.DecodeString(vector.publicKey)
		if err != nil {
			t.Fatalf("%s: public key hex: %v", vector.name, err)
		}
		priv, err := X25519PrivateKey(privateBytes)
		if err != nil {
			t.Errorf("%s: X25519PrivateKey: %v", vector.name, err)
			continue
		}
		if got := priv.PublicKey().Bytes(); !bytes.Equal(got, want) {
			t.Errorf("%s: public key = %x, want %x", vector.name, got, want)
		}
	}
}

// TestX25519SecretDependsOnThePeerKey is the property a round trip cannot reach: hold
// one side fixed, change the other, and the secret must change. A helper that ignored
// its arguments and returned a constant agrees with itself perfectly and fails only
// here and at the known answer table.
func TestX25519SecretDependsOnThePeerKey(t *testing.T) {
	local, err := X25519GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate local: %v", err)
	}
	first, err := X25519GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate first peer: %v", err)
	}
	second, err := X25519GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate second peer: %v", err)
	}
	withFirst, err := X25519DH(local, first.PublicKey())
	if err != nil {
		t.Fatalf("dh with first peer: %v", err)
	}
	withSecond, err := X25519DH(local, second.PublicKey())
	if err != nil {
		t.Fatalf("dh with second peer: %v", err)
	}
	if bytes.Equal(withFirst, withSecond) {
		t.Fatalf("two different peer keys produced the same secret: %x", withFirst)
	}
	// and the same pair twice must not move, or the difference above says nothing
	again, err := X25519DH(local, first.PublicKey())
	if err != nil {
		t.Fatalf("dh with first peer again: %v", err)
	}
	if !bytes.Equal(withFirst, again) {
		t.Fatalf("the same pair produced %x then %x", withFirst, again)
	}
}

// TestX25519LowOrderPointIsAnError is the refusal guardrail 3 exists for. Master
// section 7.2 and spec A section 5.4: the banned sdk.GenerateSharedSecret returns an
// all zero secret for these points, and a caller that logged it and continued would
// encrypt under a key its peer chose.
//
// The count at the end is what keeps the loop honest. crypto/ecdh accepts every 32 byte
// u coordinate today and refuses at the exchange, so all four points reach X25519DH; a
// parser that started refusing them would skip every assertion in the body and leave
// this test passing while covering nothing.
func TestX25519LowOrderPointIsAnError(t *testing.T) {
	priv, err := X25519GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	reachedTheExchange := 0
	for i, point := range x25519LowOrderPoints {
		pub, err := X25519PublicKey(point)
		if err != nil {
			// rejecting at parse is also acceptable, and is still not a zero
			// secret, but it has to be the point refusal: these are all 32 bytes
			// long, so a length error here would mean the gate misread them.
			if !errors.Is(err, ErrInvalidPoint) {
				t.Errorf("point %d: parse error = %v, want ErrInvalidPoint", i, err)
			}
			continue
		}
		reachedTheExchange++
		secret, err := X25519DH(priv, pub)
		if !errors.Is(err, ErrInvalidPoint) {
			t.Errorf("point %d: error = %v, want ErrInvalidPoint", i, err)
		}
		if secret != nil {
			t.Errorf("point %d: returned a secret alongside the error: %x", i, secret)
		}
	}
	if reachedTheExchange == 0 {
		t.Errorf("all %d low order points were refused at parse, so the exchange refusal this test is named for went unexercised", len(x25519LowOrderPoints))
	}
}

// TestX25519RefusesEveryEncodingOfALowOrderPoint closes the gap a literal blacklist
// leaves. A peer chooses the bytes on the wire, and the same low order point can be sent
// as u, as u plus the field prime, or with bit 255 set, all of which the masking and
// reduction in RFC 7748 section 5 collapse back to u before the scalar multiplication.
// A refusal that keyed on the canonical spelling would pass the test above and still
// hand a caller a zero secret for the others, so the spellings here are generated from
// the point rather than transcribed.
//
// The prime minus one row is a low order point the canonical list does not carry, kept
// as arithmetic rather than as a fifth hex blob so it cannot drift from the prime it is
// defined by.
//
// The counting is what stops this becoming the previous test under a new name. Only the
// spellings that are not the canonical one are counted, and every coordinate has to
// contribute at least one that reached the exchange and was refused there — so a
// generator that emitted only canonical forms, or a parser that started rejecting the
// non canonical ones before they got that far, fails here rather than passing quietly.
func TestX25519RefusesEveryEncodingOfALowOrderPoint(t *testing.T) {
	priv, err := X25519GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	coordinates := []*big.Int{new(big.Int).Sub(x25519FieldPrime(), big.NewInt(1))}
	for _, point := range x25519LowOrderPoints {
		coordinates = append(coordinates, x25519CoordinateOf(point))
	}
	for _, u := range coordinates {
		encodings := x25519EncodingsOf(u)
		refusedAtTheExchange := 0
		for i, encoded := range encodings {
			pub, err := X25519PublicKey(encoded)
			if err != nil {
				if !errors.Is(err, ErrInvalidPoint) {
					t.Errorf("u = %x as %x: parse error = %v, want ErrInvalidPoint", u, encoded, err)
				}
				continue
			}
			secret, err := X25519DH(priv, pub)
			if !errors.Is(err, ErrInvalidPoint) {
				t.Errorf("u = %x as %x: error = %v, want ErrInvalidPoint", u, encoded, err)
				continue
			}
			if secret != nil {
				t.Errorf("u = %x as %x: returned a secret alongside the error: %x", u, encoded, secret)
				continue
			}
			// index 0 is the canonical spelling the previous test already covers
			if i != 0 {
				refusedAtTheExchange++
			}
		}
		if refusedAtTheExchange == 0 {
			t.Errorf("u = %x contributed no non canonical spelling that was refused at the exchange, out of %d encodings", u, len(encodings))
		}
	}
}

// TestX25519RejectsWrongKeyLengths covers the lengths that are not 32. The refusal must
// come with nothing in hand: a key returned beside the error is a key some caller that
// checks the wrong half of the pair first will use. The accepting case is asserted in
// the same place because without it a parser hardwired to refuse everything passes this
// test and empties the low order one.
func TestX25519RejectsWrongKeyLengths(t *testing.T) {
	for _, n := range []int{0, 1, 31, 33, 64} {
		priv, err := X25519PrivateKey(make([]byte, n))
		if !errors.Is(err, ErrBadKeyLength) {
			t.Errorf("X25519PrivateKey(%d bytes) error = %v, want ErrBadKeyLength", n, err)
		}
		if priv != nil {
			t.Errorf("X25519PrivateKey(%d bytes) refused and returned a key anyway", n)
		}
		pub, err := X25519PublicKey(make([]byte, n))
		if !errors.Is(err, ErrBadKeyLength) {
			t.Errorf("X25519PublicKey(%d bytes) error = %v, want ErrBadKeyLength", n, err)
		}
		if pub != nil {
			t.Errorf("X25519PublicKey(%d bytes) refused and returned a key anyway", n)
		}
	}
	accepted := make([]byte, x25519KeySize)
	for i := range accepted {
		accepted[i] = byte(i + 1)
	}
	if _, err := X25519PrivateKey(accepted); err != nil {
		t.Errorf("X25519PrivateKey refused a %d byte scalar: %v", x25519KeySize, err)
	}
	if _, err := X25519PublicKey(accepted); err != nil {
		t.Errorf("X25519PublicKey refused a %d byte u coordinate: %v", x25519KeySize, err)
	}
}

// TestX25519ParsersReturnTheEncodingTheyWereGiven states that neither parser rewrites a
// key on the way through. Nothing else in this file can see that, because crypto/ecdh
// masks bit 255 and reduces modulo the field prime inside the scalar multiplication
// itself: a wrapper that normalised the encoding first would leave every shared secret
// here exactly where it is, and clearing bit 255 before ecdh.X25519().NewPublicKey does
// in fact pass every other test in the package. The refusal tests above do not reach it
// either — they ask only whether a spelling was refused, never what the accepted ones
// hold.
//
// It matters upstream of the curve rather than at it. A KeyPackage carries an hpke public
// key into structures that get signed and hashed, so a parser that quietly respells a
// peer's key is a signature that stops verifying later, not a wrong secret now.
//
// The spellings are generated rather than transcribed, for the same reason the low order
// ones are, and the counts at the end name the two normalisations actually exercised. A
// table that lost its bit 255 spelling, or the one that is still above the prime after
// masking, would otherwise compare canonical bytes to themselves and report success.
func TestX25519ParsersReturnTheEncodingTheyWereGiven(t *testing.T) {
	alicePublic, err := hex.DecodeString(rfc7748AlicePublic)
	if err != nil {
		t.Fatalf("alice public key hex: %v", err)
	}
	// the base point, small enough that u plus the prime is still a 32 byte encoding,
	// and a published key, which is large enough that it is not
	coordinates := []*big.Int{big.NewInt(9), x25519CoordinateOf(alicePublic)}
	prime := x25519FieldPrime()
	highBitSpellings := 0
	abovePrimeSpellings := 0
	for _, u := range coordinates {
		for _, encoded := range x25519EncodingsOf(u) {
			if encoded[x25519KeySize-1]&0x80 != 0 {
				highBitSpellings++
			}
			masked := slices.Clone(encoded)
			masked[x25519KeySize-1] &= 0x7f
			if x25519CoordinateOf(masked).Cmp(prime) >= 0 {
				abovePrimeSpellings++
			}
			priv, err := X25519PrivateKey(encoded)
			if err != nil {
				t.Errorf("X25519PrivateKey(%x): %v", encoded, err)
			} else if got := priv.Bytes(); !bytes.Equal(got, encoded) {
				t.Errorf("X25519PrivateKey rewrote the encoding it was given: %x, want %x", got, encoded)
			}
			pub, err := X25519PublicKey(encoded)
			if err != nil {
				t.Errorf("X25519PublicKey(%x): %v", encoded, err)
			} else if got := pub.Bytes(); !bytes.Equal(got, encoded) {
				t.Errorf("X25519PublicKey rewrote the encoding it was given: %x, want %x", got, encoded)
			}
		}
	}
	if highBitSpellings == 0 {
		t.Errorf("no spelling with bit 255 set was parsed, so the masking a parser could do went untested")
	}
	if abovePrimeSpellings == 0 {
		t.Errorf("no spelling above the prime after masking was parsed, so the reduction a parser could do went untested")
	}
}

// TestX25519DHRefusesANilKey states the guard that stands between a caller who ignored a
// parse error and a crash. crypto/ecdh reads the curve off both operands before it looks
// at either, so a nil on either side is a nil dereference rather than a refusal, and a
// panic in the exchange is a remotely reachable one: the nil a caller holds is what
// X25519PublicKey hands back when a peer's key was malformed. The sentinel is the length
// error, which is what parsing that missing key would have returned.
func TestX25519DHRefusesANilKey(t *testing.T) {
	priv, err := X25519GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	nilCases := []struct {
		name string
		priv *ecdh.PrivateKey
		pub  *ecdh.PublicKey
	}{
		{name: "nil public key", priv: priv, pub: nil},
		{name: "nil private key", priv: nil, pub: priv.PublicKey()},
		{name: "both nil", priv: nil, pub: nil},
	}
	for _, nilCase := range nilCases {
		secret, err := X25519DH(nilCase.priv, nilCase.pub)
		if !errors.Is(err, ErrBadKeyLength) {
			t.Errorf("%s: error = %v, want ErrBadKeyLength", nilCase.name, err)
		}
		if secret != nil {
			t.Errorf("%s: returned a secret alongside the error: %x", nilCase.name, secret)
		}
	}
}

// TestX25519GenerateKeyReadsTheReaderItIsGiven is the claim the standard library stopped
// making. Since Go 1.26 ecdh.GenerateKey ignores its reader unless
// GODEBUG=cryptocustomrand=1, so delegating would leave a caller that passed a fixed
// reader with a different key every call and no way to reproduce a published vector —
// the exact shape of the randomness parameters this plan's contract declares further up
// the stack. The scalar must be the bytes the reader supplied, in order, and two readers
// that differ must produce keys that differ.
//
// A constant byte reader cannot state the "in order" half of that, and for a while this
// test used nothing else. Its output is invariant under every permutation, so reversing
// the scalar, rotating it by one byte, or sorting it all passed the whole package. Sorting
// is the one that matters: it leaves a key that looks fine and is deterministic under a
// fixed reader while cutting the scalar to one of the 32 byte multisets over 256 symbols,
// about 141 bits rather than 256. The scripted case below is what closes that — a
// published scalar has no symmetry for a permutation to hide behind. An ascending byte
// pattern would not do, since a sorted input is a fixed point of the sort.
func TestX25519GenerateKeyReadsTheReaderItIsGiven(t *testing.T) {
	first, err := X25519GenerateKey(constantReader{value: 0x07})
	if err != nil {
		t.Fatalf("generate from a fixed reader: %v", err)
	}
	want := bytes.Repeat([]byte{0x07}, x25519KeySize)
	if got := first.Bytes(); !bytes.Equal(got, want) {
		t.Errorf("scalar = %x, want the reader's bytes %x", got, want)
	}
	again, err := X25519GenerateKey(constantReader{value: 0x07})
	if err != nil {
		t.Fatalf("generate from the same fixed reader: %v", err)
	}
	if !bytes.Equal(first.Bytes(), again.Bytes()) {
		t.Errorf("the same reader produced %x then %x", first.Bytes(), again.Bytes())
	}
	other, err := X25519GenerateKey(constantReader{value: 0x09})
	if err != nil {
		t.Fatalf("generate from a different fixed reader: %v", err)
	}
	if bytes.Equal(first.Bytes(), other.Bytes()) {
		t.Errorf("two different readers produced the same scalar %x", first.Bytes())
	}
	// the ordering claim, which no constant byte reader above can make. The published
	// scalar is the natural script: asserting the key it derives to as well turns this
	// into the reproducibility claim the whole deviation from ecdh.GenerateKey exists
	// for — hand this function a scripted reader and a published vector comes back.
	script, err := hex.DecodeString(rfc7748AlicePrivate)
	if err != nil {
		t.Fatalf("alice private key hex: %v", err)
	}
	scripted, err := X25519GenerateKey(bytes.NewReader(script))
	if err != nil {
		t.Fatalf("generate from a scripted reader: %v", err)
	}
	if got := scripted.Bytes(); !bytes.Equal(got, script) {
		t.Errorf("scalar = %x, want the reader's bytes in order %x", got, script)
	}
	wantPublic, err := hex.DecodeString(rfc7748AlicePublic)
	if err != nil {
		t.Fatalf("alice public key hex: %v", err)
	}
	if got := scripted.PublicKey().Bytes(); !bytes.Equal(got, wantPublic) {
		t.Errorf("public key = %x, want the published %x", got, wantPublic)
	}
	// and no reader at all is refused rather than filled in. This used to fall back to
	// the process source, on the reasoning that a nil reader is what ecdh.GenerateKey
	// accepts — and it made the reproducibility above a claim about this function alone.
	// Every caller in the tree reaches x25519 through here, so a provider built over a
	// caller's stream sealed under an ephemeral key drawn from somewhere nobody chose,
	// and the key was a good key, which is why no round trip could see it.
	fallback, err := X25519GenerateKey(nil)
	if !errors.Is(err, ErrNilRandomSource) {
		t.Errorf("generate from a nil reader error = %v, want ErrNilRandomSource", err)
	}
	if fallback != nil {
		t.Errorf("a nil reader still produced a scalar: %x", fallback.Bytes())
	}
}

// TestX25519GenerateKeyFailsWhenRandomFails is the other half of that. A key generator
// that answers a dead entropy source with a usable key is worse than one that crashes,
// because nothing downstream can tell. The error has to be the reader's own and not one
// of this package's sentinels: a caller distinguishing a broken machine from a malformed
// input reads the wrong answer otherwise.
func TestX25519GenerateKeyFailsWhenRandomFails(t *testing.T) {
	entropyIsDown := errors.New("entropy source is down")
	priv, err := X25519GenerateKey(failingReader{err: entropyIsDown})
	if !errors.Is(err, entropyIsDown) {
		t.Errorf("error = %v, want the reader's own error", err)
	}
	if priv != nil {
		t.Errorf("a failing reader still produced a key: %x", priv.Bytes())
	}
	// a reader that runs dry part way is the same failure with a quieter shape: the
	// tail of the scalar would be zeros nobody chose
	priv, err = X25519GenerateKey(&shortReader{remaining: x25519KeySize - 1})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("short reader error = %v, want io.ErrUnexpectedEOF", err)
	}
	if priv != nil {
		t.Errorf("a short reader still produced a key: %x", priv.Bytes())
	}
	for _, sentinel := range []error{ErrBadKeyLength, ErrInvalidPoint} {
		if errors.Is(err, sentinel) {
			t.Errorf("an entropy failure was reported as %v", sentinel)
		}
	}
}

// TestX25519KeySizeAgreesWithTheRegistry ties the literal this file introduces to the
// registry that has to agree with it. Only the suites naming the x25519 kem are checked,
// since a suite on a different curve would rightly carry different lengths and is not
// this helper's business.
func TestX25519KeySizeAgreesWithTheRegistry(t *testing.T) {
	checked := 0
	for _, suite := range Suites() {
		params, err := LookupSuite(suite)
		if err != nil {
			t.Errorf("LookupSuite(%#04x): %v", uint16(suite), err)
			continue
		}
		if params.KemId != HpkeKemX25519HkdfSha256 {
			continue
		}
		checked++
		if params.Npk != x25519KeySize {
			t.Errorf("%s: Npk = %d, x25519 public keys are %d bytes", params.Name, params.Npk, x25519KeySize)
		}
		if params.Nsk != x25519KeySize {
			t.Errorf("%s: Nsk = %d, x25519 scalars are %d bytes", params.Name, params.Nsk, x25519KeySize)
		}
		if params.Nsecret != x25519KeySize {
			t.Errorf("%s: Nsecret = %d, x25519 shared secrets are %d bytes", params.Name, params.Nsecret, x25519KeySize)
		}
	}
	if checked == 0 {
		t.Errorf("no registered suite names kem %#04x, so this test compared nothing", HpkeKemX25519HkdfSha256)
	}
}
