package connect

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// Seed envelope tests (EXTENDER-UPDATE2.md §2.4): fixed signing keys and the
// fake clock, so every outcome is reproducible.

// A fixed key per byte, so a test names its keys rather than drawing them.
func testSeedKey(n byte) ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{n}, ed25519.SeedSize))
}

func testSeedPublicKey(n byte) ed25519.PublicKey {
	return testSeedKey(n).Public().(ed25519.PublicKey)
}

// The fixed keys the tests use.
var (
	testSeedRootKey        = testSeedKey(1)
	testSeedRotatedRootKey = testSeedKey(2)
	testSeedForeignRootKey = testSeedKey(3)
)

// One open-tier record of extender key `n`, issued an hour before `now` and
// expiring two days after it, at the given public addresses.
func testSeedRecord(
	t *testing.T,
	rootPrivateKey ed25519.PrivateKey,
	n byte,
	now time.Time,
	ips ...string,
) *protocol.ExtenderRecord {
	t.Helper()
	addresses := []*protocol.ExtenderAddress{}
	for _, ip := range ips {
		addresses = append(addresses, testExtenderAddress(ip))
	}
	return signTestRecord(t, rootPrivateKey, testSeedPublicKey(0x80+n), now.Add(-time.Hour), now.Add(48*time.Hour), addresses...)
}

// Signs a record body after `edit` changes it, for the records a bundle must
// refuse.
func testSeedEditedRecord(
	t *testing.T,
	rootPrivateKey ed25519.PrivateKey,
	n byte,
	now time.Time,
	edit func(body *protocol.ExtenderRecordBody),
	ips ...string,
) *protocol.ExtenderRecord {
	t.Helper()
	record := testSeedRecord(t, rootPrivateKey, n, now, ips...)
	body := &protocol.ExtenderRecordBody{}
	if err := proto.Unmarshal(record.Body, body); err != nil {
		t.Fatal(err)
	}
	edit(body)
	edited, err := SignExtenderRecord(rootPrivateKey, body)
	if err != nil {
		t.Fatal(err)
	}
	return edited
}

// A bundle body issued half an hour before `now`, valid for a day.
func testSeedBody(
	t *testing.T,
	sequence uint64,
	now time.Time,
	records []*protocol.ExtenderRecord,
	revocations []*protocol.ExtenderRevocation,
) *protocol.ExtenderSeedBody {
	t.Helper()
	body, err := NewExtenderSeedBody(
		testExtenderNetworkHost,
		sequence,
		now.Add(-30*time.Minute),
		now.Add(24*time.Hour),
		records,
		revocations,
	)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func testSeedEnvelope(
	t *testing.T,
	rootPrivateKey ed25519.PrivateKey,
	sequence uint64,
	now time.Time,
	records []*protocol.ExtenderRecord,
	revocations []*protocol.ExtenderRevocation,
) *protocol.ExtenderSeedEnvelope {
	t.Helper()
	envelope, err := SignExtenderSeed(rootPrivateKey, testSeedBody(t, sequence, now, records, revocations))
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

// Signs an arbitrary body with no structural check, for the bundles a reader
// must refuse that the publisher's sign would never produce.
func testSeedSignRaw(t *testing.T, rootPrivateKey ed25519.PrivateKey, body *protocol.ExtenderSeedBody) *protocol.ExtenderSeedEnvelope {
	t.Helper()
	bodyBytes, err := proto.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return testSeedSignRawBytes(rootPrivateKey, bodyBytes)
}

func testSeedSignRawBytes(rootPrivateKey ed25519.PrivateKey, bodyBytes []byte) *protocol.ExtenderSeedEnvelope {
	return &protocol.ExtenderSeedEnvelope{
		Body:      bodyBytes,
		Signature: ed25519.Sign(rootPrivateKey, extenderSigningBytes(ExtenderSeedSignatureDomain, bodyBytes)),
		KeyId:     ExtenderKeyId(rootPrivateKey.Public().(ed25519.PublicKey)),
	}
}

func testSeedRootKeys(rootPrivateKeys ...ed25519.PrivateKey) *ExtenderRootKeySet {
	publicKeys := []ed25519.PublicKey{}
	for _, rootPrivateKey := range rootPrivateKeys {
		publicKeys = append(publicKeys, rootPrivateKey.Public().(ed25519.PublicKey))
	}
	return NewExtenderRootKeySet(publicKeys...)
}

func requireExtenderSeedCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a %s refusal, got none", code)
	}
	if got := ExtenderSeedErrorCode(err); got != code {
		t.Fatalf("expected a %s refusal, got %q: %v", code, got, err)
	}
	if !errors.Is(err, &ExtenderSeedError{Code: code}) {
		t.Fatalf("errors.Is does not match the %s refusal: %v", code, err)
	}
}

// A directory of the test space whose root keys are the given fixed ones.
func newTestSeedDirectory(t *testing.T, clock *testClock, rootPrivateKeys ...ed25519.PrivateKey) *ExtenderDirectory {
	t.Helper()
	settings := DefaultExtenderDirectorySettings()
	settings.Now = clock.Now
	settings.NetworkHosts = []string{testExtenderNetworkHost}
	ctx, cancel := context.WithCancel(context.Background())
	directory := NewExtenderDirectory(ctx, settings)
	directory.SetRootKeys(testSeedRootKeys(rootPrivateKeys...))
	t.Cleanup(func() {
		directory.Close()
		cancel()
	})
	return directory
}

// The whole round trip: sign, the text form both ways, verify, accept, accept
// again, and apply. A K7 round trip is not a signing test; this is.
func TestExtenderSeedRoundTrip(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	records := []*protocol.ExtenderRecord{
		testSeedRecord(t, testSeedRootKey, 1, now, "203.0.113.1", "2001:db8::1"),
		testSeedRecord(t, testSeedRootKey, 2, now, "203.0.113.2"),
	}
	envelope := testSeedEnvelope(t, testSeedRootKey, 7, now, records, nil)

	text, err := EncodeExtenderSeedText(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, ExtenderSeedTextPrefix) || strings.ContainsAny(text, "=+/ \n") {
		t.Fatalf("the text form is not prefixed unpadded base64url")
	}
	decoded, err := DecodeExtenderSeedText("  \n" + text + "\n ")
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(decoded, envelope) {
		t.Fatalf("the text form does not round trip")
	}

	bundle, err := VerifyExtenderSeed(decoded, testSeedRootKeys(testSeedRootKey), []string{"SPACE.example."}, now)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.NetworkHost != testExtenderNetworkHost || bundle.Sequence != 7 || len(bundle.Records) != 2 {
		t.Fatalf("unexpected bundle: host %q sequence %d records %d", bundle.NetworkHost, bundle.Sequence, len(bundle.Records))
	}
	if !bundle.SignerPublicKey.Equal(testSeedRootKey.Public()) {
		t.Fatalf("the bundle does not name its signer")
	}

	acceptor := NewExtenderSeedAcceptor(NewExtenderSeedMemoryStore(), nil)
	outcome, err := acceptor.Accept(bundle, testSeedRootKeys(testSeedRootKey))
	if err != nil || outcome != ExtenderSeedOutcomeAccepted {
		t.Fatalf("first accept = %q, %v", outcome, err)
	}
	outcome, err = acceptor.Accept(bundle, testSeedRootKeys(testSeedRootKey))
	if err != nil || outcome != ExtenderSeedOutcomeIdentical {
		t.Fatalf("second accept = %q, %v", outcome, err)
	}

	directory := newTestSeedDirectory(t, clock, testSeedRootKey)
	result, err := ApplyExtenderSeed(directory, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if result.RecordCount != 2 || result.UsableRecordCount != 2 {
		t.Fatalf("apply = %+v", result)
	}
	candidates := directory.VerifiedCandidates(0, 16)
	if len(candidates) != 3 {
		t.Fatalf("expected the 3 record addresses as verified candidates, got %d", len(candidates))
	}
	for _, candidate := range candidates {
		if !candidate.Verified || candidate.Source != ExtenderSourceSeed || len(candidate.PublicKey) == 0 {
			t.Fatalf("a seed candidate is not a verified seed record: %+v", candidate)
		}
	}
	if ip := netip.MustParseAddr("203.0.113.1"); !directory.AddressUsable(ip) {
		t.Fatalf("a seed record address is not usable")
	}
}

// A forged envelope -- signed by a key the space does not authorize, or with
// its body changed after signing -- is refused before its body is read.
func TestExtenderSeedForgedEnvelope(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	records := []*protocol.ExtenderRecord{testSeedRecord(t, testSeedRootKey, 1, now, "203.0.113.1")}
	rootKeys := testSeedRootKeys(testSeedRootKey)

	foreign := testSeedEnvelope(t, testSeedForeignRootKey, 1, now, records, nil)
	_, err := VerifyExtenderSeed(foreign, rootKeys, []string{testExtenderNetworkHost}, now)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeSignature)

	// the key id names an authorized key, which changes nothing: it is a hint
	foreign.KeyId = ExtenderKeyId(testSeedRootKey.Public().(ed25519.PublicKey))
	_, err = VerifyExtenderSeed(foreign, rootKeys, []string{testExtenderNetworkHost}, now)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeSignature)

	tampered := testSeedEnvelope(t, testSeedRootKey, 1, now, records, nil)
	tampered.Body = append(slicesCloneBytes(tampered.Body), 0x50, 0x01)
	_, err = VerifyExtenderSeed(tampered, rootKeys, []string{testExtenderNetworkHost}, now)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeSignature)

	_, err = VerifyExtenderSeed(testSeedEnvelope(t, testSeedRootKey, 1, now, records, nil), NewExtenderRootKeySet(), []string{testExtenderNetworkHost}, now)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeNoRootKeys)
}

func slicesCloneBytes(b []byte) []byte {
	return append([]byte(nil), b...)
}
