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

	"github.com/urnetwork/connect/v2026/protocol"
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

func testSeedMarshal(t *testing.T, message proto.Message) []byte {
	t.Helper()
	messageBytes, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	return messageBytes
}

// A body built field by field, for the bundles the publisher's checks would
// refuse to build.
func testSeedRawBody(now time.Time, sequence uint64, records [][]byte, revocations [][]byte) *protocol.ExtenderSeedBody {
	return &protocol.ExtenderSeedBody{
		FormatVersion:      ExtenderSeedFormatVersion,
		NetworkHost:        testExtenderNetworkHost,
		Sequence:           sequence,
		IssuedAtUnixMilli:  uint64(now.Add(-30 * time.Minute).UnixMilli()),
		ExpiresAtUnixMilli: uint64(now.Add(24 * time.Hour).UnixMilli()),
		Records:            records,
		Revocations:        revocations,
	}
}

func testSeedRevocation(t *testing.T, rootPrivateKey ed25519.PrivateKey, n byte, issueTime time.Time) *protocol.ExtenderRevocation {
	t.Helper()
	return signTestRevocation(t, rootPrivateKey, testSeedPublicKey(0x80+n), issueTime)
}

func testSeedVerify(t *testing.T, envelope *protocol.ExtenderSeedEnvelope, now time.Time) (*ExtenderSeedBundle, error) {
	t.Helper()
	return VerifyExtenderSeed(envelope, testSeedRootKeys(testSeedRootKey), []string{testExtenderNetworkHost}, now)
}

// A bundle for a space this client is not in is refused whole, even when the
// space's own root signed it.
func TestExtenderSeedForeignNetwork(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	body := testSeedRawBody(now, 1, nil, nil)
	body.NetworkHost = "other.example"
	_, err := testSeedVerify(t, testSeedSignRaw(t, testSeedRootKey, body), now)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeForeignNetwork)

	// a record of another space inside a bundle of this one
	foreignRecord := testSeedEditedRecord(t, testSeedRootKey, 1, now, func(recordBody *protocol.ExtenderRecordBody) {
		recordBody.NetworkHost = "other.example"
	}, "203.0.113.1")
	_, err = testSeedVerify(t, testSeedSignRaw(t, testSeedRootKey, testSeedRawBody(now, 1, [][]byte{testSeedMarshal(t, foreignRecord)}, nil)), now)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeRecordInvalid)
	// and the publisher refuses to build it
	if _, err := NewExtenderSeedBody(testExtenderNetworkHost, 1, now, now.Add(time.Hour), []*protocol.ExtenderRecord{foreignRecord}, nil); ExtenderSeedErrorCode(err) != ExtenderSeedCodeRecordInvalid {
		t.Fatalf("the publisher built a bundle with another space's record: %v", err)
	}
}

// The publisher and the importer both require the open tier with an empty
// canary channel (§2.2): a gated record, a canary in either tier, refuses the
// whole bundle.
func TestExtenderSeedRefusesGatedAndCanaryRecords(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	edits := map[string]func(body *protocol.ExtenderRecordBody){
		"gated": func(body *protocol.ExtenderRecordBody) {
			body.DirectoryTier = ExtenderDirectoryTierGated
		},
		"canary gated": func(body *protocol.ExtenderRecordBody) {
			body.DirectoryTier = ExtenderDirectoryTierGated
			body.CanaryChannel = "channel-a"
		},
		"canary open tier": func(body *protocol.ExtenderRecordBody) {
			body.CanaryChannel = "channel-a"
		},
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			restricted := testSeedEditedRecord(t, testSeedRootKey, 1, now, edit, "203.0.113.1")
			open := testSeedRecord(t, testSeedRootKey, 2, now, "203.0.113.2")
			body := testSeedRawBody(now, 1, [][]byte{testSeedMarshal(t, open), testSeedMarshal(t, restricted)}, nil)
			_, err := testSeedVerify(t, testSeedSignRaw(t, testSeedRootKey, body), now)
			requireExtenderSeedCode(t, err, ExtenderSeedCodeGatedRecord)
			if _, err := SignExtenderSeed(testSeedRootKey, body); ExtenderSeedErrorCode(err) != ExtenderSeedCodeGatedRecord {
				t.Fatalf("the publisher signed a bundle with a restricted record: %v", err)
			}
		})
	}
}

// Every contained record passes its own checks, and one that does not refuses
// the whole bundle: a valid envelope never carries an invalid record through.
func TestExtenderSeedRefusesInvalidRecords(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	valid := testSeedMarshal(t, testSeedRecord(t, testSeedRootKey, 9, now, "203.0.113.9"))
	cases := map[string][][]byte{
		"signed by an unauthorized root": {testSeedMarshal(t, testSeedRecord(t, testSeedForeignRootKey, 1, now, "203.0.113.1"))},
		"not a record":                   {[]byte("not a record")},
		"lists no address": {testSeedMarshal(t, testSeedEditedRecord(t, testSeedRootKey, 1, now, func(body *protocol.ExtenderRecordBody) {
			body.Addresses = nil
		}))},
		"lists an address that does not parse": {testSeedMarshal(t, testSeedRecord(t, testSeedRootKey, 1, now, "203.0.113.300"))},
		"lists a private address":              {testSeedMarshal(t, testSeedRecord(t, testSeedRootKey, 1, now, "10.0.0.1"))},
		"lists a loopback address":             {testSeedMarshal(t, testSeedRecord(t, testSeedRootKey, 1, now, "127.0.0.1"))},
		"lists a link-local address":           {testSeedMarshal(t, testSeedRecord(t, testSeedRootKey, 1, now, "fe80::1"))},
		"repeats a key":                        {testSeedMarshal(t, testSeedRecord(t, testSeedRootKey, 1, now, "203.0.113.1")), testSeedMarshal(t, testSeedRecord(t, testSeedRootKey, 1, now, "203.0.113.2"))},
		"repeats an address":                   {testSeedMarshal(t, testSeedRecord(t, testSeedRootKey, 1, now, "203.0.113.1")), testSeedMarshal(t, testSeedRecord(t, testSeedRootKey, 2, now, "203.0.113.1"))},
		"expires before the bundle": {testSeedMarshal(t, testSeedEditedRecord(t, testSeedRootKey, 1, now, func(body *protocol.ExtenderRecordBody) {
			body.ExpireTimeMs = uint64(now.Add(time.Hour).UnixMilli())
		}, "203.0.113.1"))},
		"never expires": {testSeedMarshal(t, testSeedEditedRecord(t, testSeedRootKey, 1, now, func(body *protocol.ExtenderRecordBody) {
			body.ExpireTimeMs = 0
		}, "203.0.113.1"))},
		"issued after the bundle": {testSeedMarshal(t, testSeedEditedRecord(t, testSeedRootKey, 1, now, func(body *protocol.ExtenderRecordBody) {
			body.IssueTimeMs = uint64(now.UnixMilli())
		}, "203.0.113.1"))},
		"carries no valid key": {testSeedMarshal(t, testSeedEditedRecord(t, testSeedRootKey, 1, now, func(body *protocol.ExtenderRecordBody) {
			body.PublicKey = body.PublicKey[:16]
		}, "203.0.113.1"))},
	}
	for name, records := range cases {
		t.Run(name, func(t *testing.T) {
			body := testSeedRawBody(now, 1, append([][]byte{valid}, records...), nil)
			_, err := testSeedVerify(t, testSeedSignRaw(t, testSeedRootKey, body), now)
			requireExtenderSeedCode(t, err, ExtenderSeedCodeRecordInvalid)
		})
	}
}

// A revocation inside a bundle is judged like a record: root signed, of the
// bundle's space, naming a key, once per key.
func TestExtenderSeedRefusesInvalidRevocations(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	otherHost, err := SignExtenderRevocation(testSeedRootKey, &protocol.ExtenderRevocationBody{
		PublicKey:   testSeedPublicKey(0x81),
		IssueTimeMs: uint64(now.UnixMilli()),
		NetworkHost: "other.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][][]byte{
		"signed by an unauthorized root": {testSeedMarshal(t, testSeedRevocation(t, testSeedForeignRootKey, 1, now))},
		"not a revocation":               {[]byte("not a revocation")},
		"of another space":               {testSeedMarshal(t, otherHost)},
		"repeats a key":                  {testSeedMarshal(t, testSeedRevocation(t, testSeedRootKey, 1, now)), testSeedMarshal(t, testSeedRevocation(t, testSeedRootKey, 1, now.Add(-time.Minute)))},
	}
	for name, revocations := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := testSeedVerify(t, testSeedSignRaw(t, testSeedRootKey, testSeedRawBody(now, 1, nil, revocations)), now)
			requireExtenderSeedCode(t, err, ExtenderSeedCodeRevocationInvalid)
		})
	}
}

// Freshness is judged against the local clock with the documented allowance
// either way, and a clock outside it is told as a clock problem, never
// relaxed.
func TestExtenderSeedFreshness(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	records := []*protocol.ExtenderRecord{testSeedRecord(t, testSeedRootKey, 1, now, "203.0.113.1")}
	envelope := testSeedEnvelope(t, testSeedRootKey, 1, now, records, nil)
	issueTime := now.Add(-30 * time.Minute)
	expireTime := now.Add(24 * time.Hour)

	for _, at := range []time.Time{issueTime.Add(-ExtenderSeedClockSkew), now, expireTime, expireTime.Add(ExtenderSeedClockSkew)} {
		if _, err := testSeedVerify(t, envelope, at); err != nil {
			t.Fatalf("refused inside the allowance at %s: %v", at, err)
		}
	}
	_, err := testSeedVerify(t, envelope, issueTime.Add(-ExtenderSeedClockSkew-time.Millisecond))
	requireExtenderSeedCode(t, err, ExtenderSeedCodeNotYetValid)
	if !ExtenderSeedFreshnessError(err) || !strings.Contains(err.Error(), "clock") {
		t.Fatalf("a future-dated bundle is not reported as a clock problem: %v", err)
	}
	_, err = testSeedVerify(t, envelope, expireTime.Add(ExtenderSeedClockSkew+time.Millisecond))
	requireExtenderSeedCode(t, err, ExtenderSeedCodeExpired)
	if !ExtenderSeedFreshnessError(err) || !strings.Contains(err.Error(), "clock") {
		t.Fatalf("an expired bundle is not reported as a clock problem: %v", err)
	}
	if ExtenderSeedFreshnessError(errors.New("other")) || ExtenderSeedFreshnessError(nil) {
		t.Fatal("a non-seed error is a freshness error")
	}
}

// The parser limits of §2.2: the byte caps are checked before anything is
// decoded -- a payload too long and not even base64, or not even protobuf, is
// refused as too large, never as malformed -- and the counts before any record
// is decoded.
func TestExtenderSeedLimits(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()

	oversizeText := ExtenderSeedTextPrefix + strings.Repeat("!", ExtenderSeedMaxTextByteCount)
	_, err := DecodeExtenderSeedText(oversizeText)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeTooLarge)
	_, err = ParseExtenderSeedArtifact([]byte(oversizeText))
	requireExtenderSeedCode(t, err, ExtenderSeedCodeTooLarge)
	oversizeBinary := bytes.Repeat([]byte{0xff}, ExtenderSeedMaxEnvelopeByteCount+1)
	_, err = ParseExtenderSeedEnvelope(oversizeBinary)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeTooLarge)
	_, err = ParseExtenderSeedArtifact(oversizeBinary)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeTooLarge)
	// an envelope built in memory is held to the cap before its signature
	_, err = testSeedVerify(t, &protocol.ExtenderSeedEnvelope{
		Body:      bytes.Repeat([]byte{1}, ExtenderSeedMaxEnvelopeByteCount),
		Signature: bytes.Repeat([]byte{2}, ed25519.SignatureSize),
	}, now)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeTooLarge)

	recordsOf := func(count int, addressCount int) [][]byte {
		records := [][]byte{}
		for i := range count {
			ips := []string{}
			for j := range addressCount {
				ips = append(ips, netip.AddrFrom4([4]byte{203, 0, byte(i), byte(j + 1)}).String())
			}
			records = append(records, testSeedMarshal(t, testSeedRecord(t, testSeedRootKey, byte(i), now, ips...)))
		}
		return records
	}
	_, err = testSeedVerify(t, testSeedSignRaw(t, testSeedRootKey, testSeedRawBody(now, 1, recordsOf(ExtenderSeedMaxRecordCount+1, 1), nil)), now)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeTooMany)
	// 5 records of 7 addresses is 35, over the 32 of a bundle
	_, err = testSeedVerify(t, testSeedSignRaw(t, testSeedRootKey, testSeedRawBody(now, 1, recordsOf(5, 7), nil)), now)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeTooMany)
	// 8 records of 4 addresses is exactly the 32
	if _, err := testSeedVerify(t, testSeedSignRaw(t, testSeedRootKey, testSeedRawBody(now, 1, recordsOf(8, 4), nil)), now); err != nil {
		t.Fatalf("a bundle at every limit is refused: %v", err)
	}
	revocations := [][]byte{}
	for i := range ExtenderSeedMaxRevocationCount + 1 {
		revocations = append(revocations, testSeedMarshal(t, testSeedRevocation(t, testSeedRootKey, byte(i), now)))
	}
	_, err = testSeedVerify(t, testSeedSignRaw(t, testSeedRootKey, testSeedRawBody(now, 1, nil, revocations)), now)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeTooMany)

	bigRecord := testSeedEditedRecord(t, testSeedRootKey, 1, now, func(body *protocol.ExtenderRecordBody) {
		body.DnsTld = strings.Repeat("x", ExtenderSeedMaxRecordByteCount)
	}, "203.0.113.1")
	_, err = testSeedVerify(t, testSeedSignRaw(t, testSeedRootKey, testSeedRawBody(now, 1, [][]byte{testSeedMarshal(t, bigRecord)}, nil)), now)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeTooLarge)

	// the publisher refuses every one of them before signing
	if _, err := SignExtenderSeed(testSeedRootKey, testSeedRawBody(now, 1, recordsOf(ExtenderSeedMaxRecordCount+1, 1), nil)); ExtenderSeedErrorCode(err) != ExtenderSeedCodeTooMany {
		t.Fatalf("the publisher signed too many records: %v", err)
	}
	if _, err := SignExtenderSeed(testSeedRootKey, testSeedRawBody(now, 1, [][]byte{testSeedMarshal(t, bigRecord)}, nil)); ExtenderSeedErrorCode(err) != ExtenderSeedCodeTooLarge {
		t.Fatalf("the publisher signed an oversized record: %v", err)
	}
}

// Bodies and envelopes a v1 reader must not take: a field it does not know is
// signed semantics it would ignore, and a format, sequence or time it does
// not accept is refused before any record is read.
func TestExtenderSeedRefusesMalformedBodies(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	record := testSeedMarshal(t, testSeedRecord(t, testSeedRootKey, 1, now, "203.0.113.1"))
	edits := map[string]func(body *protocol.ExtenderSeedBody){
		"format version 2": func(body *protocol.ExtenderSeedBody) { body.FormatVersion = 2 },
		"no sequence":      func(body *protocol.ExtenderSeedBody) { body.Sequence = 0 },
		"no network host":  func(body *protocol.ExtenderSeedBody) { body.NetworkHost = " . " },
		"no issue time":    func(body *protocol.ExtenderSeedBody) { body.IssuedAtUnixMilli = 0 },
		"no expiry":        func(body *protocol.ExtenderSeedBody) { body.ExpiresAtUnixMilli = 0 },
		"expires before it is issued": func(body *protocol.ExtenderSeedBody) {
			body.ExpiresAtUnixMilli = body.IssuedAtUnixMilli
		},
		"a validity past the max": func(body *protocol.ExtenderSeedBody) {
			body.IssuedAtUnixMilli = uint64(now.Add(-ExtenderSeedMaxValidity).UnixMilli())
		},
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			body := testSeedRawBody(now, 1, [][]byte{record}, nil)
			edit(body)
			_, err := testSeedVerify(t, testSeedSignRaw(t, testSeedRootKey, body), now)
			requireExtenderSeedCode(t, err, ExtenderSeedCodeMalformed)
		})
	}

	t.Run("unknown body field", func(t *testing.T) {
		bodyBytes := testSeedMarshal(t, testSeedRawBody(now, 1, [][]byte{record}, nil))
		// field 99, varint 1
		bodyBytes = append(bodyBytes, 0x98, 0x06, 0x01)
		_, err := testSeedVerify(t, testSeedSignRawBytes(testSeedRootKey, bodyBytes), now)
		requireExtenderSeedCode(t, err, ExtenderSeedCodeMalformed)
	})
	t.Run("unknown envelope field", func(t *testing.T) {
		envelopeBytes := testSeedMarshal(t, testSeedEnvelope(t, testSeedRootKey, 1, now, []*protocol.ExtenderRecord{testSeedRecord(t, testSeedRootKey, 1, now, "203.0.113.1")}, nil))
		envelope, err := ParseExtenderSeedEnvelope(append(envelopeBytes, 0x98, 0x06, 0x01))
		if err != nil {
			t.Fatal(err)
		}
		_, err = testSeedVerify(t, envelope, now)
		requireExtenderSeedCode(t, err, ExtenderSeedCodeMalformed)
	})
	t.Run("envelope shape", func(t *testing.T) {
		envelope := testSeedEnvelope(t, testSeedRootKey, 1, now, []*protocol.ExtenderRecord{testSeedRecord(t, testSeedRootKey, 1, now, "203.0.113.1")}, nil)
		noBody := proto.Clone(envelope).(*protocol.ExtenderSeedEnvelope)
		noBody.Body = nil
		_, err := testSeedVerify(t, noBody, now)
		requireExtenderSeedCode(t, err, ExtenderSeedCodeMalformed)
		longKeyId := proto.Clone(envelope).(*protocol.ExtenderSeedEnvelope)
		longKeyId.KeyId = make([]byte, 9)
		_, err = testSeedVerify(t, longKeyId, now)
		requireExtenderSeedCode(t, err, ExtenderSeedCodeMalformed)
		shortSignature := proto.Clone(envelope).(*protocol.ExtenderSeedEnvelope)
		shortSignature.Signature = shortSignature.Signature[:63]
		_, err = testSeedVerify(t, shortSignature, now)
		requireExtenderSeedCode(t, err, ExtenderSeedCodeSignature)
		_, err = VerifyExtenderSeed(nil, testSeedRootKeys(testSeedRootKey), []string{testExtenderNetworkHost}, now)
		requireExtenderSeedCode(t, err, ExtenderSeedCodeMalformed)
	})
}

// The body has no field that can carry root keys, network hosts, gossip urls
// or resolver settings, and a bundle that tries to carry K7 settings anyway --
// signed by the space's own root -- is refused rather than read. The format
// is pinned here: a new field is a new format version.
func TestExtenderSeedCannotReplaceSettings(t *testing.T) {
	fields := (&protocol.ExtenderSeedBody{}).ProtoReflect().Descriptor().Fields()
	expectedFields := map[string]bool{
		"FormatVersion":      true,
		"NetworkHost":        true,
		"Sequence":           true,
		"IssuedAtUnixMilli":  true,
		"ExpiresAtUnixMilli": true,
		"Records":            true,
		"Revocations":        true,
	}
	if fields.Len() != len(expectedFields) {
		t.Fatalf("the seed body has %d fields, expected exactly %d", fields.Len(), len(expectedFields))
	}
	for i := range fields.Len() {
		if !expectedFields[string(fields.Get(i).Name())] {
			t.Fatalf("the seed body carries an unexpected field %s", fields.Get(i).Name())
		}
	}
	envelopeFields := (&protocol.ExtenderSeedEnvelope{}).ProtoReflect().Descriptor().Fields()
	if envelopeFields.Len() != 3 {
		t.Fatalf("the seed envelope has %d fields, expected 3", envelopeFields.Len())
	}

	clock := newTestClock()
	now := clock.Now()
	settingsBytes := testSeedMarshal(t, &protocol.ExtenderShareSettings{
		DnsName:            "hostile.example",
		GossipUrl:          "https://hostile.example/gossip",
		RootPublicKeys:     [][]byte{testSeedPublicKey(3)},
		ControlDohUrlsIpv4: []string{"https://192.0.2.53/dns-query"},
	})
	bodyBytes := testSeedMarshal(t, testSeedRawBody(now, 1, [][]byte{testSeedMarshal(t, testSeedRecord(t, testSeedRootKey, 1, now, "203.0.113.1"))}, nil))
	// the settings as field 8, length delimited, where a careless reader might
	// look for them
	bodyBytes = append(bodyBytes, 0x42, byte(len(settingsBytes)))
	bodyBytes = append(bodyBytes, settingsBytes...)
	_, err := testSeedVerify(t, testSeedSignRawBytes(testSeedRootKey, bodyBytes), now)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeMalformed)
}

// The seed and the K7 share are separate formats: neither text decodes as the
// other, and a share's bytes are no envelope.
func TestExtenderSeedTextIsSeparateFromTheShare(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	shareText, err := EncodeExtenderShare(&protocol.ExtenderShare{
		NetworkHost: testExtenderNetworkHost,
		Addresses:   [][]byte{{203, 0, 113, 1}},
		Settings: &protocol.ExtenderShareSettings{
			RootPublicKeys: [][]byte{testSeedPublicKey(3)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = DecodeExtenderSeedText(shareText)
	requireExtenderSeedCode(t, err, ExtenderSeedCodeMalformed)

	seedText, err := EncodeExtenderSeedText(testSeedEnvelope(t, testSeedRootKey, 1, now, []*protocol.ExtenderRecord{testSeedRecord(t, testSeedRootKey, 1, now, "203.0.113.1")}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeExtenderShare(seedText); err == nil {
		t.Fatal("a seed decoded as a share")
	}

	share, err := DecodeExtenderShare(shareText)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := ParseExtenderSeedArtifact(testSeedMarshal(t, share))
	if err == nil {
		_, err = testSeedVerify(t, envelope, now)
	}
	if err == nil {
		t.Fatal("a share's bytes verified as a seed")
	}

	_, err = DecodeExtenderSeedText("ur-seed:2:" + strings.TrimPrefix(seedText, ExtenderSeedTextPrefix))
	requireExtenderSeedCode(t, err, ExtenderSeedCodeMalformed)
	if !strings.Contains(err.Error(), "version") {
		t.Fatalf("a later seed version is not reported as one: %v", err)
	}
}

// A revocation in the same bundle at or after a record's issue time dominates
// it; an older one does not.
func TestExtenderSeedRevocationDominatesABundledRecord(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	records := []*protocol.ExtenderRecord{
		testSeedRecord(t, testSeedRootKey, 1, now, "203.0.113.1"),
		testSeedRecord(t, testSeedRootKey, 2, now, "203.0.113.2"),
	}
	revocations := []*protocol.ExtenderRevocation{
		// at the record's issue time
		testSeedRevocation(t, testSeedRootKey, 1, now.Add(-time.Hour)),
		// before the record was issued
		testSeedRevocation(t, testSeedRootKey, 2, now.Add(-2*time.Hour)),
	}
	bundle, err := testSeedVerify(t, testSeedEnvelope(t, testSeedRootKey, 1, now, records, revocations), now)
	if err != nil {
		t.Fatal(err)
	}
	if !bundle.Records[0].Revoked || bundle.Records[1].Revoked {
		t.Fatalf("revoked = %v %v, expected true false", bundle.Records[0].Revoked, bundle.Records[1].Revoked)
	}
	if unrevoked := bundle.UnrevokedRecords(); len(unrevoked) != 1 || unrevoked[0] != bundle.Records[1] {
		t.Fatal("the unrevoked records are not the one record no revocation dominates")
	}
}

// The key id only orders the keys tried: a wrong one and none both still
// verify under the authorized key, and the right one under an unauthorized key
// never does (TestExtenderSeedForgedEnvelope).
func TestExtenderSeedKeyIdIsOnlyAHint(t *testing.T) {
	clock := newTestClock()
	now := clock.Now()
	envelope := testSeedEnvelope(t, testSeedRootKey, 1, now, []*protocol.ExtenderRecord{testSeedRecord(t, testSeedRootKey, 1, now, "203.0.113.1")}, nil)
	rootKeys := testSeedRootKeys(testSeedRotatedRootKey, testSeedRootKey)
	for _, keyId := range [][]byte{nil, ExtenderKeyId(testSeedPublicKey(3)), ExtenderKeyId(testSeedPublicKey(2))} {
		hinted := proto.Clone(envelope).(*protocol.ExtenderSeedEnvelope)
		hinted.KeyId = keyId
		bundle, err := VerifyExtenderSeed(hinted, rootKeys, []string{testExtenderNetworkHost}, now)
		if err != nil {
			t.Fatalf("key id %x: %v", keyId, err)
		}
		if !bundle.SignerPublicKey.Equal(testSeedRootKey.Public()) {
			t.Fatal("the bundle names the hinted key rather than the key that verified")
		}
	}
}
