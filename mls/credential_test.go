package mls

import (
	"bytes"
	"errors"
	"testing"

	"github.com/urnetwork/connect/mls/syntax"
)

// handDerivedBasicCredentialGolden is RFC 9420 section 5.3 written out by hand for the one
// credential this profile constructs:
//
//	credential_type  uint16       -> 0001
//	identity<V>      5 octets     -> 05 "alice"
//
// 2 + 1 + 5 = 8 octets. The prefix is the varint one and not a uint16 length, which is the
// distinction a round trip cannot see: WriteOpaqueLP would write 00000005 here and this
// implementation would read its own bytes back perfectly.
func handDerivedBasicCredentialGolden() []byte {
	return joinBytes([]byte{0x00, 0x01}, []byte{0x05}, []byte("alice"))
}

// TestBasicCredentialMatchesTheHandDerivedGolden pins the field order and the prefix width
// against a derivation written from the RFC rather than read back through the encoder.
func TestBasicCredentialMatchesTheHandDerivedGolden(t *testing.T) {
	want := handDerivedBasicCredentialGolden()
	if len(want) != 8 {
		t.Fatalf("the hand derivation is %d octets, the arithmetic in its comment says 8", len(want))
	}
	in := BasicCredential([]byte("alice"))
	if in.CredentialType != CredentialTypeBasic {
		t.Fatalf("credential type = %#x, want basic", in.CredentialType)
	}
	encoded, err := syntax.Marshal(&in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Equal(encoded, want) {
		t.Errorf("Marshal = %x, want %x", encoded, want)
	}
	out := &Credential{}
	if err := syntax.Unmarshal(want, out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.CredentialType != CredentialTypeBasic || !bytes.Equal(out.Identity, []byte("alice")) {
		t.Fatalf("the golden decoded to %+v", out)
	}
	if err := syntax.Unmarshal(append(bytes.Clone(want), 0x00), &Credential{}); !errors.Is(err, syntax.ErrTrailingBytes) {
		t.Fatalf("trailing byte err = %v, want ErrTrailingBytes", err)
	}
}

// TestBasicCredentialCopiesTheIdentityItWasHanded says what the constructor does with the
// caller's array. A credential that aliased it changes under the leaf that carries it the next
// time the caller writes into its own buffer, which is a signature that verified when it was
// made and does not afterwards.
func TestBasicCredentialCopiesTheIdentityItWasHanded(t *testing.T) {
	identity := []byte("alice")
	credential := BasicCredential(identity)
	identity[0] = 'A'
	if !bytes.Equal(credential.Identity, []byte("alice")) {
		t.Errorf("the credential's identity followed the caller's array to %q", credential.Identity)
	}
}

// TestCredentialRefusesX509OnBothSides is the codec layer floor of Spec A section 3.2: x509
// bytes never reach a LeafNode this package accepted, and the same refusal is surfaced on the
// encode side as a returned error rather than dropped into the Writer, so no wrong bytes are
// ever signed.
func TestCredentialRefusesX509OnBothSides(t *testing.T) {
	x509 := syntax.NewWriter()
	x509.WriteUint16(0x0002)
	x509.WriteOpaque([]byte("cert"))
	encoded, err := x509.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	decoded := &Credential{}
	if err := syntax.Unmarshal(encoded, decoded); !errors.Is(err, errProfileCredentialType) {
		t.Fatalf("decode err = %v, want errProfileCredentialType", err)
	}
	// and the refusal happened before the identity was read, so the certificate bytes never
	// reached an allocation this package made on their behalf
	if decoded.Identity != nil {
		t.Errorf("a refused credential left %q in the identity", decoded.Identity)
	}
	bad := &Credential{CredentialType: CredentialType(0x0002), Identity: []byte("cert")}
	if _, err := syntax.Marshal(bad); !errors.Is(err, errProfileCredentialType) {
		t.Fatalf("encode err = %v, want errProfileCredentialType", err)
	}
}

// TestCredentialRefusesEveryTypeOutsideTheProfile states the refusal over the class rather than
// over the one code point x509 happens to occupy. A check written as "not x509" admits every
// registration made after this was written, and a registry is not a closed set.
func TestCredentialRefusesEveryTypeOutsideTheProfile(t *testing.T) {
	refused := 0
	for candidate := 0; candidate <= 0xffff; candidate += 1 {
		credentialType := CredentialType(candidate)
		if credentialType == CredentialTypeBasic {
			continue
		}
		w := syntax.NewWriter()
		w.WriteUint16(uint16(credentialType))
		w.WriteOpaque([]byte("identity"))
		encoded, err := w.Bytes()
		if err != nil {
			t.Fatalf("%#x: Bytes: %v", credentialType, err)
		}
		if err := syntax.Unmarshal(encoded, &Credential{}); !errors.Is(err, errProfileCredentialType) {
			t.Fatalf("%#x: decode err = %v, want errProfileCredentialType", credentialType, err)
			continue
		}
		if _, err := syntax.Marshal(&Credential{CredentialType: credentialType, Identity: []byte("identity")}); !errors.Is(err, errProfileCredentialType) {
			t.Fatalf("%#x: encode err = %v, want errProfileCredentialType", credentialType, err)
		}
		refused += 1
	}
	if refused != 0xffff {
		t.Errorf("%d credential types were refused, and the registry holds %d values outside the profile", refused, 0xffff)
	}
}
