// Canonical signed generation enrollment proves possession without selecting a
// boundary, a complete population, or an independently admitted request signer.
package protocol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
)

func TestWholeWorkOwnerEnrollmentBindsActualGenerationAndCanonicalOriginal(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{81}, 32))
	value, err := SignOriginalWorkOwnerEnrollment(t.Context(), OriginalWorkOwnerEnrollment{DomainHash: [32]byte{1}, ClientId: [16]byte{2}, Generation: [16]byte{3}}, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := value.Bytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	actual, err := DecodeOriginalWorkOwnerEnrollment(t.Context(), raw)
	if err != nil || actual != value {
		t.Fatal("canonical identity did not round-trip", err)
	}
	changed := value
	changed.Generation[0]++
	if err := changed.Verify(t.Context()); err == nil {
		t.Fatal("identity signature authorized a new generation")
	}
	if _, err := DecodeOriginalWorkOwnerEnrollment(t.Context(), append([]byte(" "), raw...)); err == nil {
		t.Fatal("alternate original encoding admitted")
	}
	if _, err := DecodeOriginalWorkOwnerEnrollment(t.Context(), make([]byte, MaximumOriginalWorkOwnerBytes+1)); err == nil {
		t.Fatal("unbounded enrollment parsed")
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("synthetic enrollment owner stopped")
	cancel(cause)
	if _, err := DecodeOriginalWorkOwnerEnrollment(ctx, raw); !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		t.Fatal("owner stop cause lost", err)
	}
}
