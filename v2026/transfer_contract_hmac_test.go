package connect

// Regression tests for the stored-contract HMAC formats. The legacy form is
// `mac.Sum(storedContractBytes)`: the signed bytes followed by HMAC(key, ""),
// a value of the key alone. One legacy tag therefore lets its holder append
// that suffix to any bytes and present a valid tag for them, so a verifier may
// accept the legacy form only while honest signers still emit it.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Signed bytes of one stored contract addressed to destinationId. Distinct
// allowances give distinct bytes.
func newTestStoredContractBytes(t *testing.T, destinationId Id, transferByteCount uint64) []byte {
	t.Helper()
	storedContractBytes, err := proto.Marshal(&protocol.StoredContract{
		ContractId:        NewId().Bytes(),
		TransferByteCount: transferByteCount,
		SourceId:          NewId().Bytes(),
		DestinationId:     destinationId.Bytes(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return storedContractBytes
}

// The tag a forger builds for storedContractBytes from one legacy tag of any
// other contract under the same key.
func forgeLegacyContractHmac(legacyHmac []byte, storedContractBytes []byte) []byte {
	forgedHmac := bytes.Clone(storedContractBytes)
	return append(forgedHmac, legacyHmac[len(legacyHmac)-sha256.Size:]...)
}

// Settings whose hmac cutover lies the given offset from now.
func newTestContractHmacSettings(cutoverOffset time.Duration) *ContractManagerSettings {
	settings := DefaultContractManagerSettings()
	settings.NetworkEventTimeChangeHmac = time.Now().Add(cutoverOffset)
	return settings
}

// Every consumer of a contract signed before the cutover held its legacy tag,
// and that tag forges a valid tag for any contract bytes under the same key.
// Past the cutover only the standard form may verify: the forgery and the
// legacy tag are refused, a standard tag still verifies, and without settings
// there is no cutover to honor so the legacy form never verifies.
func TestLegacyContractHmacForgeryRejectedAfterCutover(t *testing.T) {
	provideSecretKey := bytes.Repeat([]byte{0x5a}, 32)
	destinationId := NewId()
	issuedBytes := newTestStoredContractBytes(t, destinationId, 1024)
	forgedBytes := newTestStoredContractBytes(t, destinationId, 1024*1024*1024)
	beforeCutover := newTestContractHmacSettings(time.Hour)
	afterCutover := newTestContractHmacSettings(-time.Hour)

	legacyHmac := SignStoredContract(beforeCutover, provideSecretKey, issuedBytes)
	AssertEqual(t, len(issuedBytes)+sha256.Size, len(legacyHmac))
	forgedHmac := forgeLegacyContractHmac(legacyHmac, forgedBytes)

	AssertEqual(t, false, VerifyStoredContract(afterCutover, provideSecretKey, forgedBytes, forgedHmac))
	AssertEqual(t, false, VerifyStoredContract(afterCutover, provideSecretKey, issuedBytes, legacyHmac))
	AssertEqual(t, true, VerifyStoredContract(
		afterCutover,
		provideSecretKey,
		forgedBytes,
		SignStoredContract(afterCutover, provideSecretKey, forgedBytes),
	))

	// while signers still emit the legacy form, verifiers accept it
	AssertEqual(t, true, VerifyStoredContract(beforeCutover, provideSecretKey, issuedBytes, legacyHmac))

	AssertEqual(t, false, VerifyStoredContract(nil, provideSecretKey, issuedBytes, legacyHmac))
	AssertEqual(t, true, VerifyStoredContract(
		nil,
		provideSecretKey,
		issuedBytes,
		SignStoredContract(afterCutover, provideSecretKey, issuedBytes),
	))
}

// The standard tag of empty bytes is HMAC(key, ""), the same key-only value
// every legacy tag leaks, so empty contract bytes never verify in either form
// or under any settings.
func TestEmptyStoredContractBytesNeverVerify(t *testing.T) {
	provideSecretKey := bytes.Repeat([]byte{0x5b}, 32)
	keyOnlyMac := hmac.New(sha256.New, provideSecretKey).Sum(nil)
	settingsList := []*ContractManagerSettings{
		newTestContractHmacSettings(time.Hour),
		newTestContractHmacSettings(-time.Hour),
		nil,
	}
	for _, settings := range settingsList {
		AssertEqual(t, false, VerifyStoredContract(settings, provideSecretKey, nil, keyOnlyMac))
		AssertEqual(t, false, VerifyStoredContract(settings, provideSecretKey, []byte{}, keyOnlyMac))
	}
}

// The legacy form is built by appending to a slice. Neither the signer nor
// the verifier may write into spare capacity behind the caller's contract
// bytes, and a returned tag must own its array rather than alias theirs.
func TestStoredContractHmacLeavesCallerBytesUntouched(t *testing.T) {
	provideSecretKey := bytes.Repeat([]byte{0x5c}, 32)
	beforeCutover := newTestContractHmacSettings(time.Hour)
	issuedBytes := newTestStoredContractBytes(t, NewId(), 1024)

	backing := make([]byte, len(issuedBytes), len(issuedBytes)+2*sha256.Size)
	copy(backing, issuedBytes)
	spare := backing[len(issuedBytes):cap(backing)]
	sentinel := bytes.Repeat([]byte{0xa5}, len(spare))
	copy(spare, sentinel)
	storedContractBytes := backing[:len(issuedBytes)]

	legacyHmac := SignStoredContract(beforeCutover, provideSecretKey, storedContractBytes)
	AssertEqual(t, sentinel, spare)
	AssertEqual(t, true, VerifyStoredContract(beforeCutover, provideSecretKey, storedContractBytes, legacyHmac))
	AssertEqual(t, sentinel, spare)

	legacyHmac[0] ^= 0xff
	AssertEqual(t, issuedBytes, storedContractBytes)
}

// The provider's receive path verifies through ContractManager.Verify with
// its own settings, whose default cutover has passed. A forged legacy tag for
// a contract naming this provider is refused at that entry point, and a
// genuine contract still verifies there.
func TestContractManagerRefusesForgedLegacyContract(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), DefaultClientSettings())
	t.Cleanup(func() {
		cancel()
		join, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := client.CloseAndWait(join); err != nil {
			t.Error(err)
		}
	})
	manager := client.ContractManager()
	AssertEqual(t, true, manager.settings.NetworkEventTimeChangeHmac.Before(time.Now()))

	provideSecretKey := bytes.Repeat([]byte{0x5d}, 32)
	func() {
		manager.mutex.Lock()
		defer manager.mutex.Unlock()
		manager.provideModes[protocol.ProvideMode_Public] = true
		manager.provideSecretKeys[protocol.ProvideMode_Public] = provideSecretKey
	}()

	issuedBytes := newTestStoredContractBytes(t, client.ClientId(), 1024)
	forgedBytes := newTestStoredContractBytes(t, client.ClientId(), 1024*1024*1024)
	legacyHmac := SignStoredContract(newTestContractHmacSettings(time.Hour), provideSecretKey, issuedBytes)

	AssertEqual(t, false, manager.Verify(
		forgeLegacyContractHmac(legacyHmac, forgedBytes),
		forgedBytes,
		protocol.ProvideMode_Public,
	))
	AssertEqual(t, true, manager.Verify(
		SignStoredContract(manager.settings, provideSecretKey, issuedBytes),
		issuedBytes,
		protocol.ProvideMode_Public,
	))
}
