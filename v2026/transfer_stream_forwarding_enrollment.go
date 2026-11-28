// Authenticated platform delivery enrolls ephemeral endpoint keys for one
// multihop stream contract. The platform authenticates both introductions;
// neither adjacent peer receives the private keys. P2P readiness and Transfer
// data use separate keys derived from the same exact enrollment.
package connect

import (
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"time"
)

const (
	streamForwardingEnrollmentVersion   = 1
	streamForwardingPublicKeyByteCount  = 32
	streamForwardingCommitmentByteCount = 16
	streamForwardingOfferByteCount      = 1 + streamForwardingPublicKeyByteCount
	streamForwardingAnswerByteCount     = 1 + streamForwardingCommitmentByteCount + streamForwardingPublicKeyByteCount
)

// Immutable transcript input owned by the exact introducing send/receive
// sequence and signed grant. The physical stream incarnation has its own
// owner after the endpoints confirm these keys over that stream.
type streamForwardingGrant struct {
	path                     TransferPath
	contractId               Id
	sequenceId               Id
	introductionId           Id
	contractDigest           [sha256.Size]byte
	expirationTimeUnixMillis int64
	reciprocal               bool
}

// A primary contract must have an exact routed stream, endpoints, and a
// signed deadline. The caller still owns contract verification and live
// sequence admission.
func streamForwardingGrantOf(contract *sequenceContract, sequenceId, introductionId Id) (streamForwardingGrant, bool) {
	if contract == nil || contract.contract == nil || contract.expirationTimeUnixMilli == nil ||
		contract.path.StreamId == (Id{}) || contract.path.SourceId == (Id{}) || contract.path.DestinationId == (Id{}) ||
		contract.path.SourceId == contract.path.DestinationId || contract.contractId == (Id{}) ||
		sequenceId == (Id{}) || introductionId == (Id{}) {
		return streamForwardingGrant{}, false
	}
	return streamForwardingGrant{
		path:                     contract.path,
		contractId:               contract.contractId,
		sequenceId:               sequenceId,
		introductionId:           introductionId,
		contractDigest:           sha256.Sum256(contract.contract.StoredContractBytes),
		expirationTimeUnixMillis: *contract.expirationTimeUnixMilli,
	}, true
}

// A version byte and fixed-size fields have one canonical representation.
func (self streamForwardingGrant) transcript(originPublicKey, destinationPublicKey []byte) [sha256.Size]byte {
	digest := sha256.New()
	digest.Write([]byte("urnetwork-stream-forwarding-enrollment-v1"))
	if self.reciprocal {
		digest.Write([]byte{streamForwardingReciprocalVersion})
	}
	digest.Write(self.path.SourceId[:])
	digest.Write(self.path.DestinationId[:])
	digest.Write(self.path.StreamId[:])
	digest.Write(self.contractId[:])
	digest.Write(self.sequenceId[:])
	digest.Write(self.introductionId[:])
	digest.Write(self.contractDigest[:])
	digest.Write(originPublicKey)
	digest.Write(destinationPublicKey)
	var result [sha256.Size]byte
	digest.Sum(result[:0])
	return result
}

const streamForwardingReciprocalVersion = byte(2)

// An answer commits to its specific offer, independently of which current
// cumulative ACK carries that answer.
func (self streamForwardingGrant) offerCommitment(originPublicKey []byte) [streamForwardingCommitmentByteCount]byte {
	digest := self.transcript(originPublicKey, nil)
	return [streamForwardingCommitmentByteCount]byte(digest[:streamForwardingCommitmentByteCount])
}

// Each direction and each use receive an independent HKDF output. The
// endpoint-specific association owner keeps only its needed directions.
type streamForwardingKeys struct {
	transcript         [sha256.Size]byte
	forwardReadiness   [sha256.Size]byte
	reverseReadiness   [sha256.Size]byte
	forwardApplication [sha256.Size]byte
	reverseApplication [sha256.Size]byte
}

// Both endpoints supply their actual private key. X25519 rejects public keys
// that yield the all-zero shared secret.
func (self streamForwardingGrant) derive(
	privateKey *ecdh.PrivateKey,
	remotePublicKey, originPublicKey, destinationPublicKey []byte,
) (streamForwardingKeys, bool) {
	publicKey, err := ecdh.X25519().NewPublicKey(remotePublicKey)
	if err != nil {
		return streamForwardingKeys{}, false
	}
	sharedSecret, err := privateKey.ECDH(publicKey)
	if err != nil {
		return streamForwardingKeys{}, false
	}
	defer clear(sharedSecret)
	keys := streamForwardingKeys{transcript: self.transcript(originPublicKey, destinationPublicKey)}
	material, err := hkdf.Key(sha256.New, sharedSecret, keys.transcript[:], "urnetwork-stream-forwarding-keys-v1", 4*sha256.Size)
	if err != nil {
		return streamForwardingKeys{}, false
	}
	defer clear(material)
	copy(keys.forwardReadiness[:], material[0*sha256.Size:1*sha256.Size])
	copy(keys.reverseReadiness[:], material[1*sha256.Size:2*sha256.Size])
	copy(keys.forwardApplication[:], material[2*sha256.Size:3*sha256.Size])
	copy(keys.reverseApplication[:], material[3*sha256.Size:4*sha256.Size])
	return keys, true
}

// Owned by the introducing send sequence. Retries use the same offer and
// introduction identity. The sequence drops this object on retirement.
type streamForwardingOffer struct {
	grant          streamForwardingGrant
	privateKey     *ecdh.PrivateKey
	wire           [streamForwardingOfferByteCount]byte
	sequenceNumber uint64
	// These two flags belong to the normal ACK handler.
	delivered bool
	published bool
}

// Acquires a new endpoint key only for a pending, unexpired introduction.
func newStreamForwardingOffer(
	contract *sequenceContract,
	sequenceId, introductionId Id,
	now time.Time,
	reciprocal ...bool,
) *streamForwardingOffer {
	grant, valid := streamForwardingGrantOf(contract, sequenceId, introductionId)
	if !valid || grant.expirationTimeUnixMillis <= now.UnixMilli() {
		return nil
	}
	privateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil
	}
	offer := &streamForwardingOffer{grant: grant, privateKey: privateKey}
	offer.wire[0] = streamForwardingEnrollmentVersion
	if len(reciprocal) == 1 && reciprocal[0] {
		offer.grant.reciprocal = true
		offer.wire[0] = streamForwardingReciprocalVersion
	}
	copy(offer.wire[1:], privateKey.PublicKey().Bytes())
	return offer
}

// The receive sequence retains this immutable answer for each normal delivery
// ACK covering the original introduction. An ordinary selective queue receipt
// has no authority to publish it.
type streamForwardingAnswer struct {
	grant          streamForwardingGrant
	wire           [streamForwardingAnswerByteCount]byte
	keys           streamForwardingKeys
	sequenceNumber uint64
	// Only the delivery ACK writer changes this flag.
	published bool
}

// This admission follows ordinary grant verification on the same exact Pack.
// A standard tag is 32 bytes. The legacy tag includes the entire nonempty
// contract, which cannot satisfy that length after a successful Verify.
func acceptStreamForwardingOffer(
	contract *sequenceContract,
	path TransferPath,
	sequenceId, introductionId Id,
	now time.Time,
	provenance *platformReceiveProvenance,
	offer []byte,
	publish func(streamForwardingAnswer) bool,
) bool {
	grant, valid := streamForwardingGrantOf(contract, sequenceId, introductionId)
	if !valid || !contract.verifiedReceiveProofSet || len(contract.contract.StoredContractHmac) != sha256.Size ||
		grant.expirationTimeUnixMillis <= now.UnixMilli() || path != grant.path.LocalMask() ||
		len(offer) != streamForwardingOfferByteCount ||
		offer[0] != streamForwardingEnrollmentVersion && offer[0] != streamForwardingReciprocalVersion ||
		!provenance.withLiveOwner(grant.path.DestinationId, func() {}) {
		return false
	}
	grant.reciprocal = offer[0] == streamForwardingReciprocalVersion

	privateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return false
	}
	originPublicKey := offer[1:]
	destinationPublicKey := privateKey.PublicKey().Bytes()
	keys, valid := grant.derive(privateKey, originPublicKey, originPublicKey, destinationPublicKey)
	if !valid {
		return false
	}
	answer := streamForwardingAnswer{grant: grant, keys: keys}
	answer.wire[0] = offer[0]
	commitment := grant.offerCommitment(originPublicKey)
	copy(answer.wire[1:], commitment[:])
	copy(answer.wire[1+streamForwardingCommitmentByteCount:], destinationPublicKey)
	// Key work is outside the connection pin. Retirement after selection but
	// before this commit prevents the decision from being published.
	accepted := false
	provenance.withLiveOwner(grant.path.DestinationId, func() {
		accepted = publish(answer)
	})
	return accepted
}

// Only the expected peer's actual platform ACK can complete this offer. The
// caller must already have adjudicated its exact delivery or cumulative
// coverage and pin the original SendSequence for the local publication.
func (self *streamForwardingOffer) acceptAnswer(
	path TransferPath,
	sequenceId Id,
	now time.Time,
	provenance *platformReceiveProvenance,
	answer []byte,
	publish func(streamForwardingKeys) bool,
) bool {
	if self == nil || self.grant.expirationTimeUnixMillis <= now.UnixMilli() ||
		path != sendTransferPath(self.grant.path.DestinationId, DestinationId(self.grant.path.SourceId)) ||
		sequenceId != self.grant.sequenceId || len(answer) != streamForwardingAnswerByteCount ||
		answer[0] != self.wire[0] ||
		!provenance.withLiveOwner(self.grant.path.SourceId, func() {}) {
		return false
	}

	originPublicKey := self.wire[1:]
	destinationPublicKey := answer[1+streamForwardingCommitmentByteCount:]
	expected := self.grant.offerCommitment(originPublicKey)
	if expected != [streamForwardingCommitmentByteCount]byte(answer[1:1+streamForwardingCommitmentByteCount]) {
		return false
	}
	keys, valid := self.grant.derive(self.privateKey, destinationPublicKey, originPublicKey, destinationPublicKey)
	if !valid {
		return false
	}
	accepted := false
	provenance.withLiveOwner(self.grant.path.SourceId, func() {
		accepted = publish(keys)
	})
	return accepted
}
