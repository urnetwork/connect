// Exercises the authenticated platform enrollment cut without a connection,
// timer, or packet retry. The carrier integration owns the real TLS witnesses.
package connect

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

// Make an exact synthetic ordinary grant and verify it with the same standard
// tag check as ReceiveSequence before exposing its proof to enrollment.
func testStreamForwardingContract(t *testing.T, now time.Time) *sequenceContract {
	t.Helper()
	return testStreamForwardingContractForPath(t, now, TransferPath{SourceId: NewId(), DestinationId: NewId(), StreamId: NewId()})
}

// Signs a test contract for a specific stream and its logical endpoints.
func testStreamForwardingContractForPath(t *testing.T, now time.Time, path TransferPath) *sequenceContract {
	t.Helper()
	expiration := now.Add(time.Minute).UnixMilli()
	stored, err := proto.Marshal(&protocol.StoredContract{
		ContractId: NewId().Bytes(), SourceId: path.SourceId.Bytes(), DestinationId: path.DestinationId.Bytes(),
		StreamId: path.StreamId.Bytes(), ExpirationTimeUnixMilli: &expiration, TransferByteCount: 64 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("synthetic-forwarding-provide-key")
	wire := &protocol.Contract{
		StoredContractBytes: stored,
		StoredContractHmac:  SignStoredContract(&ContractManagerSettings{}, secret, stored),
		ProvideMode:         protocol.ProvideMode_Stream,
	}
	if !VerifyStoredContract(nil, secret, wire.StoredContractBytes, wire.StoredContractHmac) {
		t.Fatal("test contract did not verify")
	}
	contract, err := newSequenceContract(NewNoopLogger(), "synthetic-forwarding", wire, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	contract.verifiedReceiveProofSet = true
	return contract
}

// Both live platform reads establish the same scope and directional keys.
// Foreign, closed, unbound, malformed, and expired inputs publish no key.
func TestStreamForwardingEnrollmentRequiresExactAuthenticatedEndpoints(t *testing.T) {
	now := time.Unix(1_900_000_000, 0)
	contract := testStreamForwardingContract(t, now)
	sequenceId, introductionId := NewId(), NewId()
	originId := contract.path.SourceId
	destinationId := contract.path.DestinationId
	reversePath := contract.path.Reverse().LocalMask()
	path := contract.path.LocalMask()
	offer := newStreamForwardingOffer(contract, sequenceId, introductionId, now)
	if offer == nil {
		t.Fatal("canonical scoped offer was not created")
	}

	origin := newPlatformReceiveOwner(originId, t.Context().Done(), nil, true)
	destination := newPlatformReceiveOwner(destinationId, t.Context().Done(), nil, true)
	originProvenance := origin.provenance(platformReceiveH3Datagram)
	destinationProvenance := destination.provenance(platformReceiveH1)
	publishAnswer := func(streamForwardingAnswer) bool {
		t.Error("invalid offer reached enrollment publication")
		return true
	}
	unknownPath := path
	unknownPath.SourceId = NewId()
	for _, refusal := range []struct {
		provenance *platformReceiveProvenance
		time       time.Time
		path       TransferPath
		wire       []byte
	}{
		{nil, now, path, offer.wire[:]},
		{originProvenance, now, path, offer.wire[:]},
		{destinationProvenance, now, unknownPath, offer.wire[:]},
		{destinationProvenance, now.Add(time.Minute), path, offer.wire[:]},
		{destinationProvenance, now, path, offer.wire[:len(offer.wire)-1]},
		{destinationProvenance, now, path, append([]byte{streamForwardingEnrollmentVersion}, make([]byte, streamForwardingPublicKeyByteCount)...)},
	} {
		if acceptStreamForwardingOffer(contract, refusal.path, sequenceId, introductionId, refusal.time, refusal.provenance, refusal.wire, publishAnswer) {
			t.Fatal("invalid offer reported accepted")
		}
	}
	contract.verifiedReceiveProofSet = false
	if acceptStreamForwardingOffer(contract, path, sequenceId, introductionId, now, destinationProvenance, offer.wire[:], publishAnswer) {
		t.Fatal("unverified grant reported accepted")
	}
	contract.verifiedReceiveProofSet = true
	standardTag := contract.contract.StoredContractHmac
	contract.contract.StoredContractHmac = make([]byte, len(contract.contract.StoredContractBytes)+sha256.Size)
	if acceptStreamForwardingOffer(contract, path, sequenceId, introductionId, now, destinationProvenance, offer.wire[:], publishAnswer) {
		t.Fatal("legacy grant reported accepted")
	}
	contract.contract.StoredContractHmac = standardTag

	var answer streamForwardingAnswer
	if !acceptStreamForwardingOffer(contract, path, sequenceId, introductionId, now, destinationProvenance, offer.wire[:], func(candidate streamForwardingAnswer) bool {
		answer = candidate
		return true
	}) {
		t.Fatal("legitimate verified offer did not publish an answer")
	}
	if answer.grant != offer.grant {
		t.Fatal("answer lost the original contract and introduction scope")
	}
	publishKeys := func(streamForwardingKeys) bool {
		t.Error("invalid answer reached enrollment publication")
		return true
	}
	changedCommitment := answer.wire
	changedCommitment[1] ^= 1
	for _, refusal := range []struct {
		provenance *platformReceiveProvenance
		path       TransferPath
		sequenceId Id
		wire       []byte
	}{
		{nil, reversePath, sequenceId, answer.wire[:]},
		{destinationProvenance, reversePath, sequenceId, answer.wire[:]},
		{originProvenance, path, sequenceId, answer.wire[:]},
		{originProvenance, reversePath, NewId(), answer.wire[:]},
		{originProvenance, reversePath, sequenceId, changedCommitment[:]},
	} {
		if offer.acceptAnswer(refusal.path, refusal.sequenceId, now, refusal.provenance, refusal.wire, publishKeys) {
			t.Fatal("invalid answer reported accepted")
		}
	}

	// An otherwise well-formed second introduction cannot use the first reply.
	otherOffer := newStreamForwardingOffer(contract, sequenceId, NewId(), now)
	if otherOffer.acceptAnswer(reversePath, sequenceId, now, originProvenance, answer.wire[:], publishKeys) {
		t.Fatal("answer from a different introduction reported accepted")
	}
	// The same ephemeral public key and message id under another grant do not
	// extend the original verified proof.
	wrongGrant := *offer
	wrongGrant.grant.contractDigest[0] ^= 1
	if wrongGrant.acceptAnswer(reversePath, sequenceId, now, originProvenance, answer.wire[:], publishKeys) {
		t.Fatal("answer from another signed grant reported accepted")
	}

	var originKeys streamForwardingKeys
	if !offer.acceptAnswer(reversePath, sequenceId, now, originProvenance, answer.wire[:], func(keys streamForwardingKeys) bool {
		originKeys = keys
		return true
	}) {
		t.Fatal("legitimate verified answer did not publish endpoint keys")
	}
	if originKeys != answer.keys || originKeys.transcript == ([sha256.Size]byte{}) {
		t.Fatal("the two endpoints disagree about the verified transcript or keys")
	}
	publishedKeys := map[[sha256.Size]byte]bool{
		{}: true,
	}
	for _, key := range [][sha256.Size]byte{originKeys.forwardReadiness, originKeys.reverseReadiness, originKeys.forwardApplication, originKeys.reverseApplication} {
		if publishedKeys[key] {
			t.Fatal("enrollment did not separate both directions and uses")
		}
		publishedKeys[key] = true
	}

	origin.retire()
	destination.retire()
	if offer.acceptAnswer(reversePath, sequenceId, now, originProvenance, answer.wire[:], publishKeys) ||
		acceptStreamForwardingOffer(contract, path, sequenceId, introductionId, now, destinationProvenance, offer.wire[:], publishAnswer) {
		t.Fatal("a retained receipt from a retired platform owner reported accepted")
	}
}
