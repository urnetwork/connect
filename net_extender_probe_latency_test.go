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

// The attestation crypto (DESIGNNOTES4.md §2, §3, GEOMAP §2.2, §2.3): what a
// pinger of either kind signs, what the target co-signs, what verifies, and
// what must not.

// A provider's attestor over a fresh client key, and the public key the
// operator would hold for it.
func newTestProbeAttestor(t *testing.T) (*ExtenderProbeAttestor, ed25519.PublicKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return NewExtenderProbeProviderAttestor(NewId(), func(data []byte) []byte {
		return ed25519.Sign(privateKey, data)
	}), publicKey
}

// An extender's attestor over a fresh identity key, signing through the peer
// probe signer as an extender does, and the identity key.
func newTestPeerProbeAttestor(t *testing.T) (*ExtenderProbeAttestor, ed25519.PublicKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return NewExtenderProbeExtenderAttestor(publicKey, NewExtenderPeerProbeSigner(privateKey)), publicKey
}

// One well formed, unsigned attestation of the attestor against a fresh
// extender key, naming the attestor by its kind.
func newTestProbeAttestation(t *testing.T, attestor *ExtenderProbeAttestor) *protocol.ExtenderProbeAttestation {
	t.Helper()
	nonce, err := NewExtenderProbeNonce()
	if err != nil {
		t.Fatal(err)
	}
	attestation := &protocol.ExtenderProbeAttestation{
		ExtenderPublicKey: newTestExtenderKey(t),
		ProbeNonce:        nonce,
		RttMs:             42,
		TimestampMs:       uint64(time.Now().UnixMilli()),
	}
	switch attestor.Kind() {
	case ExtenderPingerKindProvider:
		attestation.ProbeClientId = attestor.ClientId.Bytes()
	case ExtenderPingerKindExtender:
		attestation.PingerExtenderPublicKey = slices.Clone(attestor.ExtenderPublicKey)
	default:
		t.Fatal("the test attestor names no single identity")
	}
	return attestation
}

// A signed attestation of either kind, the pinger's key, and a target key
// pair the attestation names, for the co-signature tests.
func newTestCosignFixture(
	t *testing.T,
	kind ExtenderPingerKind,
) (*protocol.ExtenderProbeAttestation, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	var attestor *ExtenderProbeAttestor
	switch kind {
	case ExtenderPingerKindProvider:
		attestor, _ = newTestProbeAttestor(t)
	default:
		attestor, _ = newTestPeerProbeAttestor(t)
	}
	targetPublicKey, targetPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	attestation := newTestProbeAttestation(t, attestor)
	attestation.ExtenderPublicKey = targetPublicKey
	if err := SignExtenderProbeAttestation(attestor, attestation); err != nil {
		t.Fatal(err)
	}
	return attestation, targetPublicKey, targetPrivateKey
}

// A target's sign over its identity key.
func testTargetSign(privateKey ed25519.PrivateKey) func(data []byte) []byte {
	return func(data []byte) []byte {
		return ed25519.Sign(privateKey, data)
	}
}

// An attestor's kind is the one identity it names, and empty for both,
// neither or a key of the wrong size.
func TestExtenderProbeAttestorKind(t *testing.T) {
	provider, _ := newTestProbeAttestor(t)
	if kind := provider.Kind(); kind != ExtenderPingerKindProvider {
		t.Fatalf("provider kind = %q", kind)
	}
	extender, _ := newTestPeerProbeAttestor(t)
	if kind := extender.Kind(); kind != ExtenderPingerKindExtender {
		t.Fatalf("extender kind = %q", kind)
	}
	malformed := map[string]*ExtenderProbeAttestor{
		"both": {
			ClientId:          NewId(),
			ExtenderPublicKey: newTestExtenderKey(t),
			Sign:              provider.Sign,
		},
		"neither":   {Sign: provider.Sign},
		"short key": {ExtenderPublicKey: newTestExtenderKey(t)[0:31], Sign: provider.Sign},
		"long key":  {ExtenderPublicKey: append(slices.Clone(newTestExtenderKey(t)), 0), Sign: provider.Sign},
		"short key and client id": {
			ClientId:          NewId(),
			ExtenderPublicKey: newTestExtenderKey(t)[0:31],
			Sign:              provider.Sign,
		},
	}
	for name, attestor := range malformed {
		if kind := attestor.Kind(); kind != "" {
			t.Fatalf("%s: kind = %q, expected none", name, kind)
		}
	}
	var nilAttestor *ExtenderProbeAttestor
	if kind := nilAttestor.Kind(); kind != "" {
		t.Fatalf("nil kind = %q", kind)
	}
	// the constructor keeps its own copy of the key
	publicKey := newTestExtenderKey(t)
	copied := NewExtenderProbeExtenderAttestor(publicKey, provider.Sign)
	publicKey[0] ^= 1
	if bytes.Equal(copied.ExtenderPublicKey, publicKey) {
		t.Fatal("the extender attestor aliases the caller's key")
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
	if kind := ExtenderProbeAttestationPingerKind(attestation); kind != ExtenderPingerKindProvider {
		t.Fatalf("kind = %q", kind)
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

// An extender's claim of a peer probe verifies under its key, and under no
// other key or change.
func TestExtenderPeerProbeAttestationSignsAndVerifies(t *testing.T) {
	attestor, pingerPublicKey := newTestPeerProbeAttestor(t)
	attestation := newTestProbeAttestation(t, attestor)

	if err := SignExtenderProbeAttestation(attestor, attestation); err != nil {
		t.Fatal(err)
	}
	if len(attestation.Signature) != ed25519.SignatureSize {
		t.Fatalf("signature is %d bytes", len(attestation.Signature))
	}
	if kind := ExtenderProbeAttestationPingerKind(attestation); kind != ExtenderPingerKindExtender {
		t.Fatalf("kind = %q", kind)
	}
	if !VerifyExtenderProbeAttestation(pingerPublicKey, attestation) {
		t.Fatal("a freshly signed peer attestation does not verify")
	}
	frameBytes, err := ExtenderProbeAttestationFrame(attestation)
	if err != nil {
		t.Fatal(err)
	}
	read, err := ReadExtenderProbeAttestationFrame(bytes.NewReader(frameBytes))
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(read, attestation) || !VerifyExtenderProbeAttestation(pingerPublicKey, read) {
		t.Fatal("the peer attestation did not round trip the frame")
	}

	// a claim verifies only under the key it names: another key fails, and
	// so does the named key's signature presented under a claim naming
	// someone else
	otherAttestor, otherPublicKey := newTestPeerProbeAttestor(t)
	if VerifyExtenderProbeAttestation(otherPublicKey, attestation) {
		t.Fatal("a peer attestation verifies under a key that did not sign it")
	}
	renamed := proto.Clone(attestation).(*protocol.ExtenderProbeAttestation)
	renamed.PingerExtenderPublicKey = slices.Clone(otherAttestor.ExtenderPublicKey)
	if VerifyExtenderProbeAttestation(pingerPublicKey, renamed) {
		t.Fatal("a signature verifies under a key the claim does not name")
	}
	if VerifyExtenderProbeAttestation(otherPublicKey, renamed) {
		t.Fatal("a renamed claim verifies under the key it now names")
	}
	if VerifyExtenderProbeAttestation(nil, attestation) || VerifyExtenderProbeAttestation(pingerPublicKey[0:16], attestation) {
		t.Fatal("a peer attestation verifies under a malformed key")
	}
}

// Every signed field is bound: changing any one of them, or the signature,
// breaks the verification. This is what stops an extender from altering the
// rtt and a replay from naming another extender.
func TestExtenderProbeAttestationRejectsEveryTamperedField(t *testing.T) {
	provider, providerPublicKey := newTestProbeAttestor(t)
	extender, extenderPublicKey := newTestPeerProbeAttestor(t)
	for _, pinger := range []struct {
		attestor  *ExtenderProbeAttestor
		publicKey ed25519.PublicKey
	}{
		{attestor: provider, publicKey: providerPublicKey},
		{attestor: extender, publicKey: extenderPublicKey},
	} {
		attestation := newTestProbeAttestation(t, pinger.attestor)
		if err := SignExtenderProbeAttestation(pinger.attestor, attestation); err != nil {
			t.Fatal(err)
		}
		tampers := map[string]func(a *protocol.ExtenderProbeAttestation){
			"pinger identity": func(a *protocol.ExtenderProbeAttestation) {
				if 0 < len(a.ProbeClientId) {
					a.ProbeClientId = slices.Clone(a.ProbeClientId)
					a.ProbeClientId[0] ^= 1
				} else {
					a.PingerExtenderPublicKey = slices.Clone(a.PingerExtenderPublicKey)
					a.PingerExtenderPublicKey[0] ^= 1
				}
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
			if VerifyExtenderProbeAttestation(pinger.publicKey, tampered) {
				t.Fatalf("%s %s: a tampered attestation verifies", pinger.attestor.Kind(), name)
			}
		}
		// and the untouched one still does
		if !VerifyExtenderProbeAttestation(pinger.publicKey, attestation) {
			t.Fatalf("%s: the original no longer verifies", pinger.attestor.Kind())
		}
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
	provider, providerPublicKey := newTestProbeAttestor(t)
	extender, extenderPublicKey := newTestPeerProbeAttestor(t)

	malformed := map[string]struct {
		attestor  *ExtenderProbeAttestor
		publicKey ed25519.PublicKey
		malform   func(a *protocol.ExtenderProbeAttestation)
	}{
		"short client id": {attestor: provider, publicKey: providerPublicKey, malform: func(a *protocol.ExtenderProbeAttestation) {
			a.ProbeClientId = a.ProbeClientId[0:15]
		}},
		"long client id": {attestor: provider, publicKey: providerPublicKey, malform: func(a *protocol.ExtenderProbeAttestation) {
			a.ProbeClientId = append(slices.Clone(a.ProbeClientId), 0)
		}},
		"short pinger key": {attestor: extender, publicKey: extenderPublicKey, malform: func(a *protocol.ExtenderProbeAttestation) {
			a.PingerExtenderPublicKey = a.PingerExtenderPublicKey[0:31]
		}},
		"long pinger key": {attestor: extender, publicKey: extenderPublicKey, malform: func(a *protocol.ExtenderProbeAttestation) {
			a.PingerExtenderPublicKey = append(slices.Clone(a.PingerExtenderPublicKey), 0)
		}},
		"short extender key": {attestor: provider, publicKey: providerPublicKey, malform: func(a *protocol.ExtenderProbeAttestation) {
			a.ExtenderPublicKey = a.ExtenderPublicKey[0:31]
		}},
		"short extender key of a peer": {attestor: extender, publicKey: extenderPublicKey, malform: func(a *protocol.ExtenderProbeAttestation) {
			a.ExtenderPublicKey = a.ExtenderPublicKey[0:31]
		}},
		"short nonce": {attestor: provider, publicKey: providerPublicKey, malform: func(a *protocol.ExtenderProbeAttestation) {
			a.ProbeNonce = a.ProbeNonce[0:16]
		}},
		"no nonce": {attestor: extender, publicKey: extenderPublicKey, malform: func(a *protocol.ExtenderProbeAttestation) {
			a.ProbeNonce = nil
		}},
	}
	for name, c := range malformed {
		attestation := newTestProbeAttestation(t, c.attestor)
		c.malform(attestation)
		if _, err := ExtenderProbeAttestationSigningBytes(attestation); err == nil {
			t.Fatalf("%s: signing bytes were produced", name)
		}
		if err := SignExtenderProbeAttestation(c.attestor, attestation); err == nil {
			t.Fatalf("%s: the attestation was signed", name)
		}
		attestation.Signature = make([]byte, ed25519.SignatureSize)
		if VerifyExtenderProbeAttestation(c.publicKey, attestation) {
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

// An attestation names exactly one pinger. One naming both, or neither, has
// no signing bytes, is never signed, and never verifies -- even with a
// signature over what either kind's bytes would have been.
func TestExtenderProbeAttestationNamesExactlyOnePinger(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientId := NewId()
	provider := NewExtenderProbeProviderAttestor(clientId, testTargetSign(privateKey))
	extender := NewExtenderProbeExtenderAttestor(publicKey, testTargetSign(privateKey))

	both := newTestProbeAttestation(t, provider)
	both.PingerExtenderPublicKey = slices.Clone(publicKey)
	neither := newTestProbeAttestation(t, provider)
	neither.ProbeClientId = nil

	for name, attestation := range map[string]*protocol.ExtenderProbeAttestation{"both": both, "neither": neither} {
		if kind := ExtenderProbeAttestationPingerKind(attestation); kind != "" {
			t.Fatalf("%s: kind = %q", name, kind)
		}
		if _, err := ExtenderProbeAttestationSigningBytes(attestation); err == nil {
			t.Fatalf("%s: signing bytes were produced", name)
		}
		for _, attestor := range []*ExtenderProbeAttestor{provider, extender} {
			if err := SignExtenderProbeAttestation(attestor, proto.Clone(attestation).(*protocol.ExtenderProbeAttestation)); err == nil {
				t.Fatalf("%s: a %s attestor signed it", name, attestor.Kind())
			}
		}
		// a signature over each kind's bytes, as a forger would try
		asProvider := proto.Clone(attestation).(*protocol.ExtenderProbeAttestation)
		asProvider.ProbeClientId = clientId.Bytes()
		asProvider.PingerExtenderPublicKey = nil
		providerBytes, err := ExtenderProbeAttestationSigningBytes(asProvider)
		if err != nil {
			t.Fatal(err)
		}
		asExtender := proto.Clone(attestation).(*protocol.ExtenderProbeAttestation)
		asExtender.ProbeClientId = nil
		asExtender.PingerExtenderPublicKey = slices.Clone(publicKey)
		extenderBytes, err := ExtenderProbeAttestationSigningBytes(asExtender)
		if err != nil {
			t.Fatal(err)
		}
		for _, signingBytes := range [][]byte{providerBytes, extenderBytes} {
			signed := proto.Clone(attestation).(*protocol.ExtenderProbeAttestation)
			signed.Signature = ed25519.Sign(privateKey, signingBytes)
			if VerifyExtenderProbeAttestation(publicKey, signed) {
				t.Fatalf("%s: an attestation naming %s pingers verifies", name, name)
			}
		}
	}
	if kind := ExtenderProbeAttestationPingerKind(nil); kind != "" {
		t.Fatalf("nil kind = %q", kind)
	}
}

// The two pinger kinds sign under different domains, so a signature one kind
// made can never be read as the other's, even when the same key makes both
// and every other field is the same.
func TestExtenderProbeAttestationDomainsDoNotCrossVerify(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sign := testTargetSign(privateKey)
	provider := NewExtenderProbeProviderAttestor(NewId(), sign)
	extender := NewExtenderProbeExtenderAttestor(publicKey, sign)

	providerAttestation := newTestProbeAttestation(t, provider)
	extenderAttestation := proto.Clone(providerAttestation).(*protocol.ExtenderProbeAttestation)
	extenderAttestation.ProbeClientId = nil
	extenderAttestation.PingerExtenderPublicKey = slices.Clone(publicKey)
	if err := SignExtenderProbeAttestation(provider, providerAttestation); err != nil {
		t.Fatal(err)
	}
	if err := SignExtenderProbeAttestation(extender, extenderAttestation); err != nil {
		t.Fatal(err)
	}
	if !VerifyExtenderProbeAttestation(publicKey, providerAttestation) || !VerifyExtenderProbeAttestation(publicKey, extenderAttestation) {
		t.Fatal("the controls do not verify")
	}

	// each kind's signature moved onto the other's claim
	swappedToExtender := proto.Clone(extenderAttestation).(*protocol.ExtenderProbeAttestation)
	swappedToExtender.Signature = providerAttestation.Signature
	if VerifyExtenderProbeAttestation(publicKey, swappedToExtender) {
		t.Fatal("a provider signature verifies as an extender claim")
	}
	swappedToProvider := proto.Clone(providerAttestation).(*protocol.ExtenderProbeAttestation)
	swappedToProvider.Signature = extenderAttestation.Signature
	if VerifyExtenderProbeAttestation(publicKey, swappedToProvider) {
		t.Fatal("an extender signature verifies as a provider claim")
	}

	// each kind's body signed under the other kind's domain
	providerBytes, err := ExtenderProbeAttestationSigningBytes(providerAttestation)
	if err != nil {
		t.Fatal(err)
	}
	extenderBytes, err := ExtenderProbeAttestationSigningBytes(extenderAttestation)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(providerBytes, []byte(ExtenderProbeSignatureDomain)) ||
		!bytes.HasPrefix(extenderBytes, []byte(ExtenderPeerProbeSignatureDomain)) {
		t.Fatal("the signing bytes do not start with their kind's domain")
	}
	providerBody := providerBytes[len(ExtenderProbeSignatureDomain):]
	extenderBody := extenderBytes[len(ExtenderPeerProbeSignatureDomain):]
	crossed := proto.Clone(extenderAttestation).(*protocol.ExtenderProbeAttestation)
	crossed.Signature = ed25519.Sign(privateKey, append([]byte(ExtenderProbeSignatureDomain), extenderBody...))
	if VerifyExtenderProbeAttestation(publicKey, crossed) {
		t.Fatal("an extender body under the provider domain verifies")
	}
	crossed = proto.Clone(providerAttestation).(*protocol.ExtenderProbeAttestation)
	crossed.Signature = ed25519.Sign(privateKey, append([]byte(ExtenderPeerProbeSignatureDomain), providerBody...))
	if VerifyExtenderProbeAttestation(publicKey, crossed) {
		t.Fatal("a provider body under the peer domain verifies")
	}
	// and neither verifies under the domains the same key uses elsewhere
	for _, domain := range []string{ExtenderChallengeSignatureDomain, ExtenderProbeCosignDomain, ""} {
		for _, c := range []struct {
			attestation *protocol.ExtenderProbeAttestation
			body        []byte
		}{
			{attestation: providerAttestation, body: providerBody},
			{attestation: extenderAttestation, body: extenderBody},
		} {
			other := proto.Clone(c.attestation).(*protocol.ExtenderProbeAttestation)
			other.Signature = ed25519.Sign(privateKey, append([]byte(domain), c.body...))
			if VerifyExtenderProbeAttestation(publicKey, other) {
				t.Fatalf("a signature under domain %q verifies", domain)
			}
		}
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
// rebuilds the message from a transport form signs the same thing. The
// provider bytes are exactly what providers in the field already sign.
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
	// the provider layout, field by field
	expected := []byte(ExtenderProbeSignatureDomain)
	expected = append(expected, attestation.ProbeClientId...)
	expected = append(expected, attestation.ExtenderPublicKey...)
	expected = append(expected, attestation.ProbeNonce...)
	expected = append(expected, 0, 0, 0, 42)
	expected = append(expected, bigEndianTestBytes(attestation.TimestampMs)...)
	if !bytes.Equal(first, expected) {
		t.Fatal("the provider signing bytes changed layout")
	}
	if ExtenderProbeSignatureDomain != "ur-extender-probe-v1" {
		t.Fatalf("the provider domain is %q", ExtenderProbeSignatureDomain)
	}
}

// The peer signing bytes are the domain, the two keys, the nonce, the rtt and
// the timestamp, in that order, and the domains are fixed.
func TestExtenderPeerProbeAttestationSigningBytesLayout(t *testing.T) {
	attestor, pingerPublicKey := newTestPeerProbeAttestor(t)
	attestation := newTestProbeAttestation(t, attestor)
	signingBytes, err := ExtenderProbeAttestationSigningBytes(attestation)
	if err != nil {
		t.Fatal(err)
	}
	expected := []byte(ExtenderPeerProbeSignatureDomain)
	expected = append(expected, pingerPublicKey...)
	expected = append(expected, attestation.ExtenderPublicKey...)
	expected = append(expected, attestation.ProbeNonce...)
	expected = append(expected, 0, 0, 0, 42)
	expected = append(expected, bigEndianTestBytes(attestation.TimestampMs)...)
	if !bytes.Equal(signingBytes, expected) {
		t.Fatal("the peer signing bytes are not domain || pinger key || target key || nonce || rtt || timestamp")
	}
	if len(signingBytes) != len(ExtenderPeerProbeSignatureDomain)+3*32+4+8 {
		t.Fatalf("peer signing bytes are %d long", len(signingBytes))
	}
	if ExtenderPeerProbeSignatureDomain != "ur-extender-peer-probe-v1" || ExtenderProbeCosignDomain != "ur-extender-probe-cosign-v1" {
		t.Fatal("a signature domain changed")
	}
}

// Eight big-endian bytes of the value.
func bigEndianTestBytes(value uint64) []byte {
	out := make([]byte, 8)
	for i := 7; 0 <= i; i -= 1 {
		out[i] = byte(value)
		value >>= 8
	}
	return out
}

// A pinger signs only a claim that names it.
func TestSignExtenderProbeAttestationRefusesAnotherIdentity(t *testing.T) {
	provider, _ := newTestProbeAttestor(t)
	otherProvider, _ := newTestProbeAttestor(t)
	extender, _ := newTestPeerProbeAttestor(t)
	otherExtender, _ := newTestPeerProbeAttestor(t)

	cases := map[string]struct {
		attestor    *ExtenderProbeAttestor
		attestation *protocol.ExtenderProbeAttestation
	}{
		"another provider":           {attestor: provider, attestation: newTestProbeAttestation(t, otherProvider)},
		"another extender":           {attestor: extender, attestation: newTestProbeAttestation(t, otherExtender)},
		"a provider for an extender": {attestor: provider, attestation: newTestProbeAttestation(t, extender)},
		"an extender for a provider": {attestor: extender, attestation: newTestProbeAttestation(t, provider)},
		"a malformed attestor": {
			attestor:    &ExtenderProbeAttestor{Sign: provider.Sign},
			attestation: newTestProbeAttestation(t, provider),
		},
		"no sign": {
			attestor:    &ExtenderProbeAttestor{ClientId: provider.ClientId},
			attestation: newTestProbeAttestation(t, provider),
		},
		"a short signature": {
			attestor: NewExtenderProbeProviderAttestor(provider.ClientId, func(data []byte) []byte {
				return make([]byte, ed25519.SignatureSize-1)
			}),
			attestation: newTestProbeAttestation(t, provider),
		},
	}
	for name, c := range cases {
		if err := SignExtenderProbeAttestation(c.attestor, c.attestation); err == nil {
			t.Fatalf("%s: the attestation was signed", name)
		}
		if 0 < len(c.attestation.Signature) {
			t.Fatalf("%s: a refused attestation carries a signature", name)
		}
	}
	if err := SignExtenderProbeAttestation(nil, newTestProbeAttestation(t, provider)); err == nil {
		t.Fatal("a nil attestor signed")
	}
	if err := SignExtenderProbeAttestation(provider, nil); err == nil {
		t.Fatal("a nil attestation was signed")
	}
}

// The extender attestor's signer signs the peer probe domain and nothing else,
// since the identity key also signs challenges, co-signatures and the
// certificate authority.
func TestExtenderPeerProbeSignerSignsOnlyThePeerDomain(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sign := NewExtenderPeerProbeSigner(privateKey)
	peerBytes := append([]byte(ExtenderPeerProbeSignatureDomain), make([]byte, 108)...)
	signature := sign(peerBytes)
	if !ed25519.Verify(publicKey, peerBytes, signature) {
		t.Fatal("the signer did not sign the peer domain")
	}
	for _, data := range [][]byte{
		append([]byte(ExtenderProbeSignatureDomain), make([]byte, 92)...),
		append([]byte(ExtenderProbeCosignDomain), make([]byte, 64)...),
		append([]byte(ExtenderChallengeSignatureDomain), make([]byte, 32)...),
		[]byte("ur-extender-peer-probe"),
		{},
		nil,
	} {
		if signature := sign(data); signature != nil {
			t.Fatalf("the signer signed %q", data)
		}
	}
	if signature := NewExtenderPeerProbeSigner(privateKey[0:32])(peerBytes); signature != nil {
		t.Fatal("a malformed key signed")
	}
	// an attestor over the signer attests, and one over a signer that refuses
	// does not
	attestor := NewExtenderProbeExtenderAttestor(publicKey, sign)
	attestation := newTestProbeAttestation(t, attestor)
	if err := SignExtenderProbeAttestation(attestor, attestation); err != nil {
		t.Fatal(err)
	}
	if !VerifyExtenderProbeAttestation(publicKey, attestation) {
		t.Fatal("the signer's attestation does not verify")
	}
	provider := NewExtenderProbeProviderAttestor(NewId(), sign)
	if err := SignExtenderProbeAttestation(provider, newTestProbeAttestation(t, provider)); err == nil {
		t.Fatal("the peer signer signed a provider claim")
	}
}

// The co-signature is over the cosign domain, the claim's signing bytes with
// their domain, and the pinger's signature, in that order.
func TestExtenderProbeCosignBytesLayout(t *testing.T) {
	for _, kind := range []ExtenderPingerKind{ExtenderPingerKindProvider, ExtenderPingerKindExtender} {
		attestation, _, _ := newTestCosignFixture(t, kind)
		cosignBytes, err := ExtenderProbeCosignBytes(attestation)
		if err != nil {
			t.Fatal(err)
		}
		signingBytes, err := ExtenderProbeAttestationSigningBytes(attestation)
		if err != nil {
			t.Fatal(err)
		}
		expected := append([]byte(ExtenderProbeCosignDomain), signingBytes...)
		expected = append(expected, attestation.Signature...)
		if !bytes.Equal(cosignBytes, expected) {
			t.Fatalf("%s: the cosign bytes are not domain || signing bytes || signature", kind)
		}
		// the signature is required, at its size
		for _, signature := range [][]byte{nil, attestation.Signature[0:63], append(slices.Clone(attestation.Signature), 0)} {
			malformed := proto.Clone(attestation).(*protocol.ExtenderProbeAttestation)
			malformed.Signature = signature
			if _, err := ExtenderProbeCosignBytes(malformed); err == nil {
				t.Fatalf("%s: cosign bytes over a %d byte signature", kind, len(signature))
			}
		}
	}
	if _, err := ExtenderProbeCosignBytes(nil); err == nil {
		t.Fatal("cosign bytes over no attestation")
	}
}

// A target's co-signature over an accepted claim verifies for either kind of
// pinger, and a refusal or another claim never does.
func TestExtenderProbeVerdictCosignsAndVerifies(t *testing.T) {
	for _, kind := range []ExtenderPingerKind{ExtenderPingerKindProvider, ExtenderPingerKindExtender} {
		attestation, targetPublicKey, targetPrivateKey := newTestCosignFixture(t, kind)
		cosignature, err := SignExtenderProbeVerdict(testTargetSign(targetPrivateKey), attestation)
		if err != nil {
			t.Fatal(err)
		}
		verdict := &protocol.ExtenderProbeVerdict{
			Accepted:    true,
			Reason:      ExtenderProbeVerdictReasonOk,
			Cosignature: cosignature,
		}
		if !VerifyExtenderProbeVerdict(targetPublicKey, attestation, verdict) {
			t.Fatalf("%s: a co-signed verdict does not verify", kind)
		}

		// a refusal never verifies, whatever it carries
		refused := proto.Clone(verdict).(*protocol.ExtenderProbeVerdict)
		refused.Accepted = false
		if VerifyExtenderProbeVerdict(targetPublicKey, attestation, refused) {
			t.Fatalf("%s: a refusal with a valid co-signature verifies", kind)
		}
		refused.Reason = ExtenderProbeVerdictReasonRttBelowObserved
		if VerifyExtenderProbeVerdict(targetPublicKey, attestation, refused) {
			t.Fatalf("%s: a refusal with a reason verifies", kind)
		}
		if VerifyExtenderProbeVerdict(targetPublicKey, attestation, nil) {
			t.Fatalf("%s: no verdict verifies", kind)
		}
		if VerifyExtenderProbeVerdict(targetPublicKey, nil, verdict) {
			t.Fatalf("%s: a verdict verifies with no claim", kind)
		}

		// only the target the claim names can co-sign it
		otherPublicKey, otherPrivateKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if VerifyExtenderProbeVerdict(otherPublicKey, attestation, verdict) {
			t.Fatalf("%s: a co-signature verifies under another key", kind)
		}
		otherCosignature, err := SignExtenderProbeVerdict(testTargetSign(otherPrivateKey), attestation)
		if err != nil {
			t.Fatal(err)
		}
		otherVerdict := &protocol.ExtenderProbeVerdict{Accepted: true, Cosignature: otherCosignature}
		if VerifyExtenderProbeVerdict(otherPublicKey, attestation, otherVerdict) {
			t.Fatalf("%s: a co-signature by an extender the claim does not name verifies", kind)
		}
		if VerifyExtenderProbeVerdict(nil, attestation, verdict) || VerifyExtenderProbeVerdict(targetPublicKey[0:31], attestation, verdict) {
			t.Fatalf("%s: a verdict verifies under a malformed key", kind)
		}

		// the co-signature is the target's key over the cosign domain, not
		// over the bare claim or under another domain
		cosignBytes, err := ExtenderProbeCosignBytes(attestation)
		if err != nil {
			t.Fatal(err)
		}
		for _, data := range [][]byte{
			cosignBytes[len(ExtenderProbeCosignDomain):],
			append([]byte(ExtenderChallengeSignatureDomain), cosignBytes[len(ExtenderProbeCosignDomain):]...),
		} {
			forged := &protocol.ExtenderProbeVerdict{Accepted: true, Cosignature: ed25519.Sign(targetPrivateKey, data)}
			if VerifyExtenderProbeVerdict(targetPublicKey, attestation, forged) {
				t.Fatalf("%s: a co-signature outside the cosign domain verifies", kind)
			}
		}
	}
}

// The co-signature binds the exact claim and the pinger's own signature, so
// the pair is one object: change any field, or the signature, and it no
// longer verifies.
func TestExtenderProbeVerdictBindsTheExactClaim(t *testing.T) {
	for _, kind := range []ExtenderPingerKind{ExtenderPingerKindProvider, ExtenderPingerKindExtender} {
		attestation, targetPublicKey, targetPrivateKey := newTestCosignFixture(t, kind)
		cosignature, err := SignExtenderProbeVerdict(testTargetSign(targetPrivateKey), attestation)
		if err != nil {
			t.Fatal(err)
		}
		verdict := &protocol.ExtenderProbeVerdict{Accepted: true, Cosignature: cosignature}

		tampers := map[string]func(a *protocol.ExtenderProbeAttestation){
			"pinger identity": func(a *protocol.ExtenderProbeAttestation) {
				if 0 < len(a.ProbeClientId) {
					a.ProbeClientId = slices.Clone(a.ProbeClientId)
					a.ProbeClientId[3] ^= 1
				} else {
					a.PingerExtenderPublicKey = slices.Clone(a.PingerExtenderPublicKey)
					a.PingerExtenderPublicKey[3] ^= 1
				}
			},
			"nonce": func(a *protocol.ExtenderProbeAttestation) {
				a.ProbeNonce = slices.Clone(a.ProbeNonce)
				a.ProbeNonce[0] ^= 1
			},
			"rtt down":  func(a *protocol.ExtenderProbeAttestation) { a.RttMs -= 1 },
			"rtt up":    func(a *protocol.ExtenderProbeAttestation) { a.RttMs += 1 },
			"timestamp": func(a *protocol.ExtenderProbeAttestation) { a.TimestampMs += 1 },
			"signature": func(a *protocol.ExtenderProbeAttestation) {
				a.Signature = slices.Clone(a.Signature)
				a.Signature[0] ^= 1
			},
			"signature truncated": func(a *protocol.ExtenderProbeAttestation) {
				a.Signature = a.Signature[0:63]
			},
			"no signature": func(a *protocol.ExtenderProbeAttestation) {
				a.Signature = nil
			},
			"both pingers": func(a *protocol.ExtenderProbeAttestation) {
				if 0 < len(a.ProbeClientId) {
					a.PingerExtenderPublicKey = newTestExtenderKey(t)
				} else {
					a.ProbeClientId = NewId().Bytes()
				}
			},
		}
		for name, tamper := range tampers {
			tampered := proto.Clone(attestation).(*protocol.ExtenderProbeAttestation)
			tamper(tampered)
			if VerifyExtenderProbeVerdict(targetPublicKey, tampered, verdict) {
				t.Fatalf("%s %s: a co-signature verifies over a tampered claim", kind, name)
			}
		}
		// another signature over the very same fields -- which ed25519 cannot
		// produce by itself, so it is made with another key -- is another claim
		resigned := proto.Clone(attestation).(*protocol.ExtenderProbeAttestation)
		otherSignature := make([]byte, ed25519.SignatureSize)
		if _, err := rand.Read(otherSignature); err != nil {
			t.Fatal(err)
		}
		resigned.Signature = otherSignature
		if VerifyExtenderProbeVerdict(targetPublicKey, resigned, verdict) {
			t.Fatalf("%s: a co-signature verifies with another pinger signature", kind)
		}
		// the target key the claim names, changed together with the key it
		// is verified under, is another claim too
		targetTampered := proto.Clone(attestation).(*protocol.ExtenderProbeAttestation)
		otherTargetKey := newTestExtenderKey(t)
		targetTampered.ExtenderPublicKey = otherTargetKey
		if VerifyExtenderProbeVerdict(otherTargetKey, targetTampered, verdict) {
			t.Fatalf("%s: a co-signature moved to another target verifies", kind)
		}

		for name, cosign := range map[string][]byte{
			"flipped":   func() []byte { c := slices.Clone(cosignature); c[5] ^= 1; return c }(),
			"truncated": cosignature[0:63],
			"extended":  append(slices.Clone(cosignature), 0),
			"empty":     nil,
		} {
			if VerifyExtenderProbeVerdict(targetPublicKey, attestation, &protocol.ExtenderProbeVerdict{Accepted: true, Cosignature: cosign}) {
				t.Fatalf("%s %s: a malformed co-signature verifies", kind, name)
			}
		}
		// and the untouched pair still does
		if !VerifyExtenderProbeVerdict(targetPublicKey, attestation, verdict) {
			t.Fatalf("%s: the original pair no longer verifies", kind)
		}
	}
}

// No co-signature is made without a signer, from one that refuses or signs
// short, or over a claim that is not whole.
func TestSignExtenderProbeVerdictRefusesMalformed(t *testing.T) {
	attestation, _, targetPrivateKey := newTestCosignFixture(t, ExtenderPingerKindExtender)
	if _, err := SignExtenderProbeVerdict(nil, attestation); err == nil {
		t.Fatal("a verdict was signed with no signer")
	}
	if _, err := SignExtenderProbeVerdict(func(data []byte) []byte { return nil }, attestation); err == nil {
		t.Fatal("a signer that refused produced a co-signature")
	}
	if _, err := SignExtenderProbeVerdict(func(data []byte) []byte { return make([]byte, 63) }, attestation); err == nil {
		t.Fatal("a short co-signature was accepted")
	}
	unsigned := proto.Clone(attestation).(*protocol.ExtenderProbeAttestation)
	unsigned.Signature = nil
	if _, err := SignExtenderProbeVerdict(testTargetSign(targetPrivateKey), unsigned); err == nil {
		t.Fatal("an unsigned claim was co-signed")
	}
	malformed := proto.Clone(attestation).(*protocol.ExtenderProbeAttestation)
	malformed.ProbeNonce = malformed.ProbeNonce[0:8]
	if _, err := SignExtenderProbeVerdict(testTargetSign(targetPrivateKey), malformed); err == nil {
		t.Fatal("a malformed claim was co-signed")
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
	// an empty message is refused on the way out, as it is on the way in
	if _, err := ExtenderProbeAttestationFrame(&protocol.ExtenderProbeAttestation{}); err == nil {
		t.Fatal("an empty attestation was framed")
	}
	// an oversized message is refused on the way out too
	attestation.Signature = make([]byte, ExtenderMaxHeaderByteCount)
	if _, err := ExtenderProbeAttestationFrame(attestation); err == nil {
		t.Fatal("an oversized attestation was framed")
	}
}

// A verdict frame round-trips with its length prefix, and one past its bound
// is refused.
func TestExtenderProbeVerdictFrameRoundTripAndBounds(t *testing.T) {
	attestation, _, targetPrivateKey := newTestCosignFixture(t, ExtenderPingerKindProvider)
	cosignature, err := SignExtenderProbeVerdict(testTargetSign(targetPrivateKey), attestation)
	if err != nil {
		t.Fatal(err)
	}
	for _, verdict := range []*protocol.ExtenderProbeVerdict{
		{Accepted: true, Reason: ExtenderProbeVerdictReasonOk, Cosignature: cosignature},
		{Reason: ExtenderProbeVerdictReasonRttBelowObserved},
		{Reason: ExtenderProbeVerdictReasonRateLimited},
	} {
		frameBytes, err := ExtenderProbeVerdictFrame(verdict)
		if err != nil {
			t.Fatal(err)
		}
		// the prefix is the big-endian length of what follows
		if length := int(frameBytes[0])<<24 | int(frameBytes[1])<<16 | int(frameBytes[2])<<8 | int(frameBytes[3]); length != len(frameBytes)-4 {
			t.Fatalf("prefix %d for a %d byte message", length, len(frameBytes)-4)
		}
		// followed by anything: exactly one frame is read
		reader := bytes.NewReader(append(slices.Clone(frameBytes), 1, 2, 3))
		read, err := ReadExtenderProbeVerdictFrame(reader)
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(read, verdict) {
			t.Fatal("the verdict frame did not round trip")
		}
		if reader.Len() != 3 {
			t.Fatalf("the read consumed past its frame: %d left", reader.Len())
		}
		if _, err := ReadExtenderProbeVerdictFrame(bytes.NewReader(frameBytes[0 : len(frameBytes)-1])); err == nil {
			t.Fatal("a truncated verdict frame was read")
		}
	}
	for name, frameBytes := range map[string][]byte{
		"empty":            {0, 0, 0, 0},
		"past the ceiling": {0x00, 0x00, 0x04, 0x01},
		"huge":             {0xff, 0xff, 0xff, 0xff},
		"no prefix":        {0, 0},
		"nothing":          {},
		"not a verdict":    {0, 0, 0, 2, 0xff, 0xff},
	} {
		if _, err := ReadExtenderProbeVerdictFrame(bytes.NewReader(frameBytes)); err == nil {
			t.Fatalf("%s: a verdict frame was read", name)
		}
	}
	// the refusal with no reason is the one empty verdict, and it is never
	// framed, so a reader never sees one
	if _, err := ExtenderProbeVerdictFrame(&protocol.ExtenderProbeVerdict{}); err == nil {
		t.Fatal("an empty verdict was framed")
	}
	if _, err := ExtenderProbeVerdictFrame(&protocol.ExtenderProbeVerdict{Accepted: true, Cosignature: make([]byte, ExtenderMaxHeaderByteCount)}); err == nil {
		t.Fatal("an oversized verdict was framed")
	}
	// the ceiling itself is allowed
	atCeiling := &protocol.ExtenderProbeVerdict{Accepted: true, Cosignature: make([]byte, ExtenderMaxHeaderByteCount-2-3)}
	frameBytes, err := ExtenderProbeVerdictFrame(atCeiling)
	if err != nil {
		t.Fatalf("a verdict at the ceiling was refused: %v", err)
	}
	if len(frameBytes) != 4+ExtenderMaxHeaderByteCount {
		t.Fatalf("the ceiling verdict is %d bytes", len(frameBytes))
	}
	if _, err := ReadExtenderProbeVerdictFrame(bytes.NewReader(frameBytes)); err != nil {
		t.Fatalf("a verdict at the ceiling was not read: %v", err)
	}
}

// The reason codes are wire values.
func TestExtenderProbeVerdictReasonsAreFixed(t *testing.T) {
	cases := []struct {
		reason uint32
		want   uint32
	}{
		{reason: ExtenderProbeVerdictReasonOk, want: 0},
		{reason: ExtenderProbeVerdictReasonRttBelowObserved, want: 1},
		{reason: ExtenderProbeVerdictReasonNonce, want: 2},
		{reason: ExtenderProbeVerdictReasonWrongExtender, want: 3},
		{reason: ExtenderProbeVerdictReasonUnknownPinger, want: 4},
		{reason: ExtenderProbeVerdictReasonBadSignature, want: 5},
		{reason: ExtenderProbeVerdictReasonRateLimited, want: 6},
	}
	for _, c := range cases {
		if c.reason != c.want {
			t.Fatalf("reason %d, expected %d", c.reason, c.want)
		}
	}
	for outcome, want := range map[ExtenderPingOutcome]string{
		ExtenderPingCosigned:   "cosigned",
		ExtenderPingRejected:   "rejected",
		ExtenderPingUnknown:    "unknown",
		ExtenderPingUnattested: "",
	} {
		if string(outcome) != want {
			t.Fatalf("outcome %q, expected %q", outcome, want)
		}
	}
	if ExtenderPingerKindProvider != "provider" || ExtenderPingerKindExtender != "extender" {
		t.Fatal("a pinger kind changed")
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
