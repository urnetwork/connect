package connect

// Regression tests for two ways around the signed-identity ratchet. A pin
// records the key a peer was verified with at one generation. A later chain
// may add generations (a rotation) but may not rewrite the pinned one, and an
// unavailable history may not admit a key the pin contradicts.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"
)

// A rotation adds a generation and keeps the pinned one. A chain that
// rewrites the pinned generation is a fork even when it also advances past
// it and every record is validly signed.
func TestPinRefusesForkThatAdvancesPastThePinnedGeneration(t *testing.T) {
	signer := newTestClientKeySigner(t, goldenSignerPrivateKeyHex)
	policy := testPolicyFor(t, signer)
	pinnedChain := buildTestChain(t, signer, testClientId(), [][32]byte{
		testClientKeyPublicKey(0x61),
		testClientKeyPublicKey(0x62),
	})
	_, pin, err := VerifyClientKeyHistory(testClientId(), encodeTestChain(t, pinnedChain), policy)
	if err != nil {
		t.Fatalf("verify the pinned chain: %s", err)
	}
	policy.Pin = &pin

	rotatedChain := buildTestChain(t, signer, testClientId(), [][32]byte{
		testClientKeyPublicKey(0x61),
		testClientKeyPublicKey(0x62),
		testClientKeyPublicKey(0x63),
	})
	if _, _, err := VerifyClientKeyHistory(testClientId(), encodeTestChain(t, rotatedChain), policy); err != nil {
		t.Fatalf("a rotation that extends the pinned chain was refused: %s", err)
	}

	forkedChain := buildTestChain(t, signer, testClientId(), [][32]byte{
		testClientKeyPublicKey(0x61),
		testClientKeyPublicKey(0x69),
		testClientKeyPublicKey(0x63),
	})
	if _, _, err := VerifyClientKeyHistory(testClientId(), encodeTestChain(t, forkedChain), policy); !errors.Is(err, ErrClientKeyRegistrationInvalid) {
		t.Fatalf("a fork that rewrites the pinned generation and advances past it was accepted, err = %v", err)
	}
}

// A pinned peer's key may change only through a signed rotation. When the
// history that would show one cannot be fetched, a contract key that differs
// from the pinned key is refused: answering with an error must not be a
// cheaper downgrade than answering with nothing. The pinned key itself still
// verifies, so an unreachable history API costs no availability for peers
// whose key has not changed.
func TestSignedIdentityFetchErrorRefusesAChangedPinnedKey(t *testing.T) {
	store := newTestPeerClientKeyPinStore()
	first, doneFirst := newTestKeyHistorySession(t, store, nil)
	defer doneFirst()
	armKeyHistoryGate(first)
	first.applyPeerClientKeyHistory(goldenHeadPublicKey(t), goldenClientKeyHistory())
	if got := keyHistoryState(first); got != clientKeyHistoryVerified {
		t.Fatalf("setup state = %s, want verified", got)
	}

	resolveWithFetchError := func(contractPub ed25519.PublicKey) clientKeyHistoryState {
		sess, done := newTestKeyHistorySession(t, store, func(ctx context.Context) ([][]byte, error) {
			return nil, errors.New("connection refused")
		})
		defer done()
		resolved := make(chan struct{}, 1)
		sess.beforeWorkerDoneForTest = func(name string) {
			if name == "signed identity resolve" {
				resolved <- struct{}{}
			}
		}
		armKeyHistoryGate(sess)
		sess.resolvePeerClientKeyHistory(contractPub)
		select {
		case <-resolved:
		case <-time.After(5 * time.Second):
			t.Fatal("the signed identity resolve did not finish")
		}
		return keyHistoryState(sess)
	}

	changedKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x77}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	if got := resolveWithFetchError(changedKey); got != clientKeyHistoryRejected {
		t.Fatalf("state = %s, want rejected (pinned key changed while the history was unavailable)", got)
	}
	if got := resolveWithFetchError(goldenHeadPublicKey(t)); got != clientKeyHistoryVerified {
		t.Fatalf("state = %s, want verified (the pinned key, history unavailable)", got)
	}
}
