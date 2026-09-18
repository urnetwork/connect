package connect

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"math"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// The attestation crypto (DESIGNNOTES4.md §2, §3): what the provider signs,
// what verifies, and what must not.

// A provider's attestor over a fresh client key, and the public key the
// operator would hold for it.
func newTestProbeAttestor(t *testing.T) (*ExtenderProbeAttestor, ed25519.PublicKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &ExtenderProbeAttestor{
		ClientId: NewId(),
		Sign: func(data []byte) []byte {
			return ed25519.Sign(privateKey, data)
		},
	}, publicKey
}

// One well formed, unsigned attestation of the attestor against a fresh
// extender key.
func newTestProbeAttestation(t *testing.T, attestor *ExtenderProbeAttestor) *protocol.ExtenderProbeAttestation {
	t.Helper()
	nonce, err := NewExtenderProbeNonce()
	if err != nil {
		t.Fatal(err)
	}
	return &protocol.ExtenderProbeAttestation{
		ProbeClientId:     attestor.ClientId.Bytes(),
		ExtenderPublicKey: newTestExtenderKey(t),
		ProbeNonce:        nonce,
		RttMs:             42,
		TimestampMs:       uint64(time.Now().UnixMilli()),
	}
}

func TestExtenderProbeAttestationSignsAndVerifies(t *testing.T) {
	attestor, providerPublicKey := newTestProbeAttestor(t)
	attestation := newTestProbeAttestation(t, attestor)

	if err := SignExtenderProbeAttestation(attestor, attestation); err != nil {
		t.Fatal(err)
	}
	if len(attestation.Signature) != ed25519.SignatureSize {
		t.Fatalf("signature is %d bytes", len(attestation.Signature))
	}
	if !VerifyExtenderProbeAttestation(providerPublicKey, attestation) {
		t.Fatal("a freshly signed attestation does not verify")
	}
	// the frame carries it intact
	frameBytes, err := ExtenderProbeAttestationFrame(attestation)
	if err != nil {
		t.Fatal(err)
	}
	read, err := ReadExtenderProbeAttestationFrame(bytes.NewReader(frameBytes))
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(read, attestation) {
		t.Fatal("the frame did not round trip")
	}
	if !VerifyExtenderProbeAttestation(providerPublicKey, read) {
		t.Fatal("the attestation read from the frame does not verify")
	}
}

// Every signed field is bound: changing any one of them, or the signature,
// breaks the verification. This is what stops an extender from altering the
// rtt and a replay from naming another extender.
func TestExtenderProbeAttestationRejectsEveryTamperedField(t *testing.T) {
	attestor, providerPublicKey := newTestProbeAttestor(t)
	attestation := newTestProbeAttestation(t, attestor)
	if err := SignExtenderProbeAttestation(attestor, attestation); err != nil {
		t.Fatal(err)
	}

	tampers := map[string]func(a *protocol.ExtenderProbeAttestation){
		"client id": func(a *protocol.ExtenderProbeAttestation) {
			a.ProbeClientId = slices.Clone(a.ProbeClientId)
			a.ProbeClientId[0] ^= 1
		},
		"extender key": func(a *protocol.ExtenderProbeAttestation) {
			a.ExtenderPublicKey = newTestExtenderKey(t)
		},
		"nonce": func(a *protocol.ExtenderProbeAttestation) {
			a.ProbeNonce = slices.Clone(a.ProbeNonce)
			a.ProbeNonce[len(a.ProbeNonce)-1] ^= 1
		},
		"rtt down": func(a *protocol.ExtenderProbeAttestation) {
			a.RttMs -= 1
		},
		"rtt up": func(a *protocol.ExtenderProbeAttestation) {
			a.RttMs += 1
		},
		"timestamp": func(a *protocol.ExtenderProbeAttestation) {
			a.TimestampMs += 1
		},
		"signature": func(a *protocol.ExtenderProbeAttestation) {
			a.Signature = slices.Clone(a.Signature)
			a.Signature[10] ^= 1
		},
		"signature truncated": func(a *protocol.ExtenderProbeAttestation) {
			a.Signature = a.Signature[0 : len(a.Signature)-1]
		},
	}
	for name, tamper := range tampers {
		tampered := proto.Clone(attestation).(*protocol.ExtenderProbeAttestation)
		tamper(tampered)
		if VerifyExtenderProbeAttestation(providerPublicKey, tampered) {
			t.Fatalf("%s: a tampered attestation verifies", name)
		}
	}
	// and the untouched one still does
	if !VerifyExtenderProbeAttestation(providerPublicKey, attestation) {
		t.Fatal("the original no longer verifies")
	}
}

func TestExtenderProbeAttestationRejectsAnotherKey(t *testing.T) {
	attestor, _ := newTestProbeAttestor(t)
	_, otherPublicKey := newTestProbeAttestor(t)
	attestation := newTestProbeAttestation(t, attestor)
	if err := SignExtenderProbeAttestation(attestor, attestation); err != nil {
		t.Fatal(err)
	}
	if VerifyExtenderProbeAttestation(otherPublicKey, attestation) {
		t.Fatal("an attestation verifies under a key that did not sign it")
	}
	if VerifyExtenderProbeAttestation(otherPublicKey[0:16], attestation) {
		t.Fatal("an attestation verifies under a malformed key")
	}
	if VerifyExtenderProbeAttestation(nil, attestation) {
		t.Fatal("an attestation verifies under no key")
	}
}

// The signing bytes are fixed width per field, so a field of the wrong size
// is refused rather than shifted into its neighbour.
func TestExtenderProbeAttestationRejectsMalformedSizes(t *testing.T) {
	attestor, providerPublicKey := newTestProbeAttestor(t)

	malformed := map[string]func(a *protocol.ExtenderProbeAttestation){
		"short client id": func(a *protocol.ExtenderProbeAttestation) {
			a.ProbeClientId = a.ProbeClientId[0:15]
		},
		"long client id": func(a *protocol.ExtenderProbeAttestation) {
			a.ProbeClientId = append(slices.Clone(a.ProbeClientId), 0)
		},
		"short extender key": func(a *protocol.ExtenderProbeAttestation) {
			a.ExtenderPublicKey = a.ExtenderPublicKey[0:31]
		},
		"short nonce": func(a *protocol.ExtenderProbeAttestation) {
			a.ProbeNonce = a.ProbeNonce[0:16]
		},
		"no nonce": func(a *protocol.ExtenderProbeAttestation) {
			a.ProbeNonce = nil
		},
	}
	for name, malform := range malformed {
		attestation := newTestProbeAttestation(t, attestor)
		malform(attestation)
		if _, err := ExtenderProbeAttestationSigningBytes(attestation); err == nil {
			t.Fatalf("%s: signing bytes were produced", name)
		}
		if err := SignExtenderProbeAttestation(attestor, attestation); err == nil {
			t.Fatalf("%s: the attestation was signed", name)
		}
		attestation.Signature = make([]byte, ed25519.SignatureSize)
		if VerifyExtenderProbeAttestation(providerPublicKey, attestation) {
			t.Fatalf("%s: the attestation verifies", name)
		}
	}
	if _, err := ExtenderProbeAttestationSigningBytes(nil); err == nil {
		t.Fatal("a nil attestation produced signing bytes")
	}
	if VerifyExtenderProbeAttestation(providerPublicKey, nil) {
		t.Fatal("a nil attestation verifies")
	}
}

// The domain separator is part of what is signed: a signature over the same
// fields without it -- which is what a signature made for another purpose
// under the same client key would be -- does not verify as an attestation.
func TestExtenderProbeAttestationIsDomainSeparated(t *testing.T) {
	attestor, providerPublicKey := newTestProbeAttestor(t)
	attestation := newTestProbeAttestation(t, attestor)

	signingBytes, err := ExtenderProbeAttestationSigningBytes(attestation)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(signingBytes, []byte(ExtenderProbeSignatureDomain)) {
		t.Fatal("the signing bytes do not start with the domain")
	}
	// a signature over the fields alone, with no domain
	attestation.Signature = attestor.Sign(signingBytes[len(ExtenderProbeSignatureDomain):])
	if VerifyExtenderProbeAttestation(providerPublicKey, attestation) {
		t.Fatal("a signature without the domain verifies")
	}
	// and one under another domain
	attestation.Signature = attestor.Sign(append([]byte("ur-extender-challenge-v1"), signingBytes[len(ExtenderProbeSignatureDomain):]...))
	if VerifyExtenderProbeAttestation(providerPublicKey, attestation) {
		t.Fatal("a signature under another domain verifies")
	}
}

// The signing bytes are a pure function of the fields, so a verifier that
// rebuilds the message from a transport form signs the same thing.
func TestExtenderProbeAttestationSigningBytesAreCanonical(t *testing.T) {
	attestor, _ := newTestProbeAttestor(t)
	attestation := newTestProbeAttestation(t, attestor)
	first, err := ExtenderProbeAttestationSigningBytes(attestation)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := &protocol.ExtenderProbeAttestation{
		ProbeClientId:     slices.Clone(attestation.ProbeClientId),
		ExtenderPublicKey: slices.Clone(attestation.ExtenderPublicKey),
		ProbeNonce:        slices.Clone(attestation.ProbeNonce),
		RttMs:             attestation.RttMs,
		TimestampMs:       attestation.TimestampMs,
		// the signature is not part of what is signed
		Signature: []byte("anything"),
	}
	second, err := ExtenderProbeAttestationSigningBytes(rebuilt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("the same fields produced different signing bytes")
	}
	expectedLen := len(ExtenderProbeSignatureDomain) + ExtenderProbeClientIdByteCount + ed25519.PublicKeySize + ExtenderProbeNonceByteCount + 4 + 8
	if len(first) != expectedLen {
		t.Fatalf("signing bytes are %d long, expected %d", len(first), expectedLen)
	}
}

func TestExtenderProbeAttestationFrameBounds(t *testing.T) {
	attestor, _ := newTestProbeAttestor(t)
	attestation := newTestProbeAttestation(t, attestor)
	if err := SignExtenderProbeAttestation(attestor, attestation); err != nil {
		t.Fatal(err)
	}
	frameBytes, err := ExtenderProbeAttestationFrame(attestation)
	if err != nil {
		t.Fatal(err)
	}
	// truncated
	if _, err := ReadExtenderProbeAttestationFrame(bytes.NewReader(frameBytes[0 : len(frameBytes)-1])); err == nil {
		t.Fatal("a truncated frame was read")
	}
	// an empty frame
	if _, err := ReadExtenderProbeAttestationFrame(bytes.NewReader([]byte{0, 0, 0, 0})); err == nil {
		t.Fatal("an empty frame was read")
	}
	// a length past the ceiling is refused before anything is allocated
	if _, err := ReadExtenderProbeAttestationFrame(bytes.NewReader([]byte{0xff, 0xff, 0xff, 0xff})); err == nil {
		t.Fatal("an oversized frame was read")
	}
	// an oversized message is refused on the way out too
	attestation.Signature = make([]byte, ExtenderMaxHeaderByteCount)
	if _, err := ExtenderProbeAttestationFrame(attestation); err == nil {
		t.Fatal("an oversized attestation was framed")
	}
}

// The claim is rounded UP to the millisecond, so rounding alone can never put
// it below what the extender observed.
func TestExtenderProbeRttMsRoundsUp(t *testing.T) {
	cases := map[time.Duration]uint32{
		0:                            0,
		-1 * time.Millisecond:        0,
		1 * time.Nanosecond:          1,
		1 * time.Millisecond:         1,
		1*time.Millisecond + 1:       2,
		1500 * time.Microsecond:      2,
		250 * time.Millisecond:       250,
		time.Duration(math.MaxInt64): math.MaxUint32,
	}
	for rtt, expected := range cases {
		if got := extenderProbeRttMs(rtt); got != expected {
			t.Fatalf("%s -> %d, expected %d", rtt, got, expected)
		}
	}
}

func TestExtenderProbeNonceIsFreshAndSized(t *testing.T) {
	first, err := NewExtenderProbeNonce()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewExtenderProbeNonce()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != ExtenderProbeNonceByteCount || len(second) != ExtenderProbeNonceByteCount {
		t.Fatalf("nonces are %d and %d bytes", len(first), len(second))
	}
	if bytes.Equal(first, second) {
		t.Fatal("two nonces are equal")
	}
}
