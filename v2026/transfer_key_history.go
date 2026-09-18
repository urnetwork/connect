package connect

// Client-key registration history: the verifier for operator-signed,
// hash-chained identity-key records.
//
// DESIGNNOTES3. Both defences of the sealed session verify against
// `peerClientPublicKey`, and the shipping client takes that value from the
// platform-authored contract (`SendSequence.setContract` ->
// `SetPeerClientPublicKey`). A platform that substitutes the certificate, the
// certificate signature and `destination_client_public_key` in lockstep
// defeats both. Comparing the contract value against the platform's own
// unsigned `/key/<clientId>` endpoint only catches a platform that is
// inconsistent between two of its own channels.
//
// A registration history is a stronger comparison target because the operator
// SIGNED it. Each record names the deployment domain it belongs to, carries a
// monotonic generation, and links to its predecessor by that predecessor's
// content hash. An operator that substitutes a key must therefore sign a
// record saying so, inside a chain it cannot fork without producing two signed
// and permanently attributable histories for the same client id.
//
// What this does NOT do, and no document describing it may imply otherwise: it
// does not stop an operator that is malicious from a client's first contact
// and internally consistent about it. Such an operator signs one coherent
// chain naming its own key. Detecting that needs an independent view of which
// signer is authoritative -- the plurality read across operators, which is
// deferred (DESIGNNOTES3 §10). "Signed" is not "unforgeable by the signer".
//
// The wire format is reproduced here from its canonical definition rather than
// imported, so that the mobile builds do not take a dependency on a chain
// client. `TestClientKeyRegistrationGoldenVector` pins the reproduction
// against bytes produced by the canonical implementation; if that test fails,
// this file is wrong and not the other way round.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/sha3"
)

const (
	clientKeyRegistrationSchema = "urnetwork-operator-client-key-registration-v1"
	clientKeyDomainTag          = "urnetwork-operator-client-key-domain-v1"

	// clientKeyAddressSize is the operator signer address width (the trailing
	// 20 bytes of the Keccak-256 of the uncompressed public key).
	clientKeyAddressSize = 20
	// clientKeySignatureSize is the recoverable signature width: R || S || V.
	clientKeySignatureSize = 65
	// clientKeyRecoveryMagic is the offset the compact-signature recovery code
	// is carried at by the recovery routine used below. The wire format keeps
	// the recovery code last and unbiased, so the two are converted at the
	// boundary.
	clientKeyRecoveryMagic = 27

	// MaxClientKeyRegistrationBytes bounds one encoded registration. The
	// canonical definition bounds a statement at 8 KiB; a registration is
	// smaller, and the bound exists to stop unbounded decode work on a
	// hostile response rather than to be tight.
	MaxClientKeyRegistrationBytes = 8 * 1024
)

// secp256k1 group order and its half, for the low-s (non-malleable) check.
var (
	clientKeyCurveOrder     = secp256k1.S256().N
	clientKeyCurveHalfOrder = new(big.Int).Rsh(secp256k1.S256().N, 1)
)

var (
	// ErrClientKeyRegistrationInvalid is returned for any registration or
	// chain that fails verification. It is deliberately one error: a caller
	// must not branch on WHY a chain failed, only on whether it did. The
	// distinction that callers do need -- a verified disagreement versus a
	// failure to obtain evidence at all -- lives at the transport boundary,
	// not here. See DESIGNNOTES3 §5.3.
	ErrClientKeyRegistrationInvalid = errors.New("client key registration is invalid")
)

// ClientKeyAddress is an operator root-signer address.
type ClientKeyAddress [clientKeyAddressSize]byte

// MarshalJSON renders the address as lowercase 0x-prefixed hex, which is the
// canonical encoding the content hash is taken over.
func (self ClientKeyAddress) MarshalJSON() ([]byte, error) {
	return json.Marshal("0x" + hex.EncodeToString(self[:]))
}

func (self *ClientKeyAddress) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if len(s) != 2+2*clientKeyAddressSize || s[0] != '0' || s[1] != 'x' {
		return fmt.Errorf("%w: signer address width", ErrClientKeyRegistrationInvalid)
	}
	raw, err := hex.DecodeString(s[2:])
	if err != nil {
		return fmt.Errorf("%w: signer address encoding", ErrClientKeyRegistrationInvalid)
	}
	copy(self[:], raw)
	return nil
}

func (self ClientKeyAddress) String() string {
	return "0x" + hex.EncodeToString(self[:])
}

func (self ClientKeyAddress) isZero() bool {
	return self == ClientKeyAddress{}
}

// ClientKeyHistoryDomain names one immutable deployment/history namespace. A
// registration cannot migrate across domains, so pinning the domain pins which
// deployment's history a chain belongs to.
type ClientKeyHistoryDomain struct {
	ChainID          uint64           `json:"chain_id"`
	GenesisHash      [32]byte         `json:"genesis_hash"`
	Netuid           uint16           `json:"netuid"`
	Coordinator      ClientKeyAddress `json:"coordinator"`
	SettlementVault  ClientKeyAddress `json:"settlement_vault"`
	DeploymentIDHash [32]byte         `json:"deployment_id_hash"`
	PolicyHash       [32]byte         `json:"policy_hash"`
	NoID             uint64           `json:"no_id"`
}

func (self ClientKeyHistoryDomain) validate() error {
	if self.ChainID == 0 ||
		self.GenesisHash == ([32]byte{}) ||
		self.Netuid == 0 ||
		self.Coordinator.isZero() ||
		self.SettlementVault.isZero() ||
		self.DeploymentIDHash == ([32]byte{}) ||
		self.PolicyHash == ([32]byte{}) ||
		self.NoID == 0 {
		return fmt.Errorf("%w: domain is incomplete", ErrClientKeyRegistrationInvalid)
	}
	return nil
}

// payload is the fixed-width tagged signing encoding. The JSON spellings above
// are the storage encoding and are deliberately not the signing authority.
func (self ClientKeyHistoryDomain) payload() []byte {
	data := binary.BigEndian.AppendUint64(nil, self.ChainID)
	data = append(data, self.GenesisHash[:]...)
	data = binary.BigEndian.AppendUint16(data, self.Netuid)
	data = append(data, self.Coordinator[:]...)
	data = append(data, self.SettlementVault[:]...)
	data = append(data, self.DeploymentIDHash[:]...)
	data = append(data, self.PolicyHash[:]...)
	return binary.BigEndian.AppendUint64(data, self.NoID)
}

// Digest names this domain. Pinning a domain digest is how a client refuses a
// chain signed for some other deployment.
func (self ClientKeyHistoryDomain) Digest() ([32]byte, error) {
	if err := self.validate(); err != nil {
		return [32]byte{}, err
	}
	data := append([]byte(clientKeyDomainTag), 0)
	return sha256.Sum256(append(data, self.payload()...)), nil
}

// ClientKeyEffectiveBoundary is the finalized chain identity the operator
// observed while handling the registration. The client does not resolve it
// against any chain; it is used only for monotonicity, so that a history
// cannot be rewound.
type ClientKeyEffectiveBoundary struct {
	Epoch uint64   `json:"epoch"`
	Block uint64   `json:"block"`
	Hash  [32]byte `json:"hash"`
}

func (self ClientKeyEffectiveBoundary) validate() error {
	if self.Block == 0 || self.Hash == ([32]byte{}) {
		return fmt.Errorf("%w: effective boundary is incomplete", ErrClientKeyRegistrationInvalid)
	}
	return nil
}

func (self ClientKeyEffectiveBoundary) payload() []byte {
	data := binary.BigEndian.AppendUint64(nil, self.Epoch)
	data = binary.BigEndian.AppendUint64(data, self.Block)
	return append(data, self.Hash[:]...)
}

// ClientKeyRegistration is one signed generation of a client's identity key.
// `Present` false is a signed tombstone: the client's key was withdrawn, and
// the record proving it is as durable as any other generation.
type ClientKeyRegistration struct {
	Schema            string                       `json:"schema"`
	Domain            ClientKeyHistoryDomain       `json:"domain"`
	ClientID          [16]byte                     `json:"client_id"`
	NetworkID         [16]byte                     `json:"network_id"`
	Generation        uint64                       `json:"generation"`
	Present           bool                         `json:"present"`
	PublicKey         [32]byte                     `json:"public_key"`
	PreviousHash      [32]byte                     `json:"previous_hash"`
	EffectiveBoundary ClientKeyEffectiveBoundary   `json:"effective_boundary"`
	Signer            ClientKeyAddress             `json:"signer"`
	Signature         [clientKeySignatureSize]byte `json:"signature"`
}

// Digest is what the operator signs.
func (self ClientKeyRegistration) Digest() ([32]byte, error) {
	if err := errors.Join(self.Domain.validate(), self.EffectiveBoundary.validate()); err != nil {
		return [32]byte{}, err
	}
	// `Present` and a non-zero key are the same fact stated twice, so they must
	// agree; a zero key can never mean a usable Ed25519 identity. Generation 1
	// is the only generation permitted to have no predecessor.
	if self.Schema != clientKeyRegistrationSchema ||
		self.ClientID == ([16]byte{}) ||
		self.NetworkID == ([16]byte{}) ||
		self.Generation == 0 ||
		self.Signer.isZero() ||
		self.Present == (self.PublicKey == ([32]byte{})) ||
		(self.Generation == 1) != (self.PreviousHash == ([32]byte{})) {
		return [32]byte{}, fmt.Errorf("%w: identity, generation or key", ErrClientKeyRegistrationInvalid)
	}
	data := append([]byte(clientKeyRegistrationSchema), 0)
	data = append(data, self.Domain.payload()...)
	data = append(data, self.ClientID[:]...)
	data = append(data, self.NetworkID[:]...)
	data = binary.BigEndian.AppendUint64(data, self.Generation)
	if self.Present {
		data = append(data, 1)
	} else {
		data = append(data, 0)
	}
	data = append(data, self.PublicKey[:]...)
	data = append(data, self.PreviousHash[:]...)
	data = append(data, self.EffectiveBoundary.payload()...)
	data = append(data, self.Signer[:]...)
	return sha256.Sum256(data), nil
}

// VerifySignature checks that the signature is canonical and recovers to the
// stated signer.
//
// It does NOT establish that the signer is an authorized operator. That is a
// separate question the caller must answer against pinned state, and the whole
// value of the mechanism depends on the caller actually doing it. See
// `ClientKeyHistoryPolicy`.
func (self ClientKeyRegistration) VerifySignature() error {
	digest, err := self.Digest()
	if err != nil {
		return err
	}
	signer, err := recoverClientKeySigner(digest, self.Signature)
	if err != nil {
		return err
	}
	if signer != self.Signer {
		return fmt.Errorf("%w: signature recovers to a different signer", ErrClientKeyRegistrationInvalid)
	}
	return nil
}

// recoverClientKeySigner recovers the signer address from a canonical
// recoverable signature over digest.
func recoverClientKeySigner(digest [32]byte, signature [clientKeySignatureSize]byte) (ClientKeyAddress, error) {
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:64])
	v := signature[64]
	// Non-malleable form only: r and s in [1, n-1], s in the lower half, and a
	// recovery code of 0 or 1. A high-s twin of a valid signature verifies
	// just as well, so accepting one would let a signed record be restated
	// with different bytes -- and the content hash is what the chain links on.
	if 1 < v ||
		r.Sign() <= 0 || 0 <= r.Cmp(clientKeyCurveOrder) ||
		s.Sign() <= 0 || 0 < s.Cmp(clientKeyCurveHalfOrder) {
		return ClientKeyAddress{}, fmt.Errorf("%w: signature is not canonical", ErrClientKeyRegistrationInvalid)
	}
	compact := make([]byte, clientKeySignatureSize)
	compact[0] = clientKeyRecoveryMagic + v
	copy(compact[1:], signature[:64])
	publicKey, _, err := ecdsa.RecoverCompact(compact, digest[:])
	if err != nil || publicKey == nil {
		return ClientKeyAddress{}, fmt.Errorf("%w: signature does not recover", ErrClientKeyRegistrationInvalid)
	}
	uncompressed := publicKey.SerializeUncompressed()
	if len(uncompressed) != 65 {
		return ClientKeyAddress{}, fmt.Errorf("%w: recovered key width", ErrClientKeyRegistrationInvalid)
	}
	keccak := sha3.NewLegacyKeccak256()
	// drop the leading 0x04 uncompressed-point tag; the address is over the
	// coordinate bytes only
	keccak.Write(uncompressed[1:])
	sum := keccak.Sum(nil)
	var address ClientKeyAddress
	copy(address[:], sum[len(sum)-clientKeyAddressSize:])
	return address, nil
}

// ContentHash is the immutable byte identity a successor links to. It is taken
// over the canonical stored encoding, NOT over the signing digest; the two are
// different values and interchanging them would break the chain link in a way
// that still type-checks.
func (self ClientKeyRegistration) ContentHash() ([32]byte, error) {
	if err := self.VerifySignature(); err != nil {
		return [32]byte{}, err
	}
	encoded, err := json.Marshal(self)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}

// follows reports whether self is a valid successor of prior.
func (self ClientKeyRegistration) follows(prior *ClientKeyRegistration) error {
	if err := self.VerifySignature(); err != nil {
		return err
	}
	if prior == nil {
		if self.Generation != 1 || self.PreviousHash != ([32]byte{}) {
			return fmt.Errorf("%w: chain does not start at generation 1", ErrClientKeyRegistrationInvalid)
		}
		return nil
	}
	priorHash, err := prior.ContentHash()
	if err != nil {
		return err
	}
	// A successor may not skip a generation, relink, move deployment or
	// network, restate the same value as new work, or rewind the boundary.
	if self.Generation != prior.Generation+1 ||
		self.PreviousHash != priorHash ||
		self.Domain != prior.Domain ||
		self.ClientID != prior.ClientID ||
		self.NetworkID != prior.NetworkID ||
		self.Present == prior.Present && self.PublicKey == prior.PublicKey {
		return fmt.Errorf("%w: does not extend its predecessor", ErrClientKeyRegistrationInvalid)
	}
	if self.EffectiveBoundary.Epoch < prior.EffectiveBoundary.Epoch ||
		self.EffectiveBoundary.Block < prior.EffectiveBoundary.Block ||
		self.EffectiveBoundary.Block == prior.EffectiveBoundary.Block &&
			self.EffectiveBoundary != prior.EffectiveBoundary {
		return fmt.Errorf("%w: rolls back its effective boundary", ErrClientKeyRegistrationInvalid)
	}
	return nil
}

// decodeClientKeyRegistration decodes one canonical registration. Unknown
// fields, trailing JSON and any non-canonical re-encoding are refused: the
// content hash is taken over these exact bytes, so a decoder that accepted
// variant spellings would admit two distinct byte strings for one record.
func decodeClientKeyRegistration(encoded []byte) (ClientKeyRegistration, error) {
	var registration ClientKeyRegistration
	if len(encoded) == 0 || MaxClientKeyRegistrationBytes < len(encoded) {
		return registration, fmt.Errorf("%w: encoded width", ErrClientKeyRegistrationInvalid)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&registration); err != nil {
		return ClientKeyRegistration{}, fmt.Errorf("%w: %s", ErrClientKeyRegistrationInvalid, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return ClientKeyRegistration{}, fmt.Errorf("%w: trailing JSON", ErrClientKeyRegistrationInvalid)
	}
	canonical, err := json.Marshal(registration)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return ClientKeyRegistration{}, fmt.Errorf("%w: bytes are not canonical", ErrClientKeyRegistrationInvalid)
	}
	return registration, nil
}

// ClientKeyTrustedSigner pins one authorized (domain, signer) pair.
type ClientKeyTrustedSigner struct {
	DomainDigest [32]byte
	Signer       ClientKeyAddress
}

// ClientKeyPin is the per-peer ratchet state. Once a peer has been seen with a
// verified signed history, it is never again accepted without one: otherwise an
// operator could drop a client to the unsigned path by simply withholding the
// history, which is cheaper and quieter than forging it.
type ClientKeyPin struct {
	// DomainDigest and Signer are the domain and signer the peer was first
	// verified under.
	DomainDigest [32]byte
	Signer       ClientKeyAddress
	// Generation is the highest generation verified for this peer. A later
	// chain must reach at least this far.
	Generation uint64
	// PublicKey is the identity key at that generation.
	PublicKey [32]byte
}

// ClientKeyHistoryPolicy is the verification policy for one resolution.
type ClientKeyHistoryPolicy struct {
	// TrustedSigners pins which (domain, signer) pairs may sign. When empty,
	// build-pinning is disabled and only Pin (trust on first use) constrains
	// the signer -- which means first contact with an unknown peer establishes
	// rather than checks. That is a real weakening and is only appropriate
	// where the pin store is durable.
	TrustedSigners []ClientKeyTrustedSigner
	// Pin, when non-nil, is the peer's existing ratchet state.
	Pin *ClientKeyPin
	// MaxGenerations bounds an accepted chain.
	MaxGenerations int
}

// VerifyClientKeyHistory verifies a complete registration chain for clientId
// and returns the head record and the pin state a caller should persist.
//
// The chain must start at generation 1 and be contiguous to the head: a
// caller cannot be handed a suffix, because a suffix would let an operator
// hide the generation in which it substituted a key.
func VerifyClientKeyHistory(
	clientId Id,
	encodedHistory [][]byte,
	policy ClientKeyHistoryPolicy,
) (head ClientKeyRegistration, pin ClientKeyPin, returnErr error) {
	if len(encodedHistory) == 0 {
		return head, pin, fmt.Errorf("%w: empty history", ErrClientKeyRegistrationInvalid)
	}
	maxGenerations := policy.MaxGenerations
	if maxGenerations <= 0 || maxGenerations < len(encodedHistory) {
		return head, pin, fmt.Errorf("%w: history exceeds its generation bound", ErrClientKeyRegistrationInvalid)
	}

	var prior *ClientKeyRegistration
	var domainDigest [32]byte
	for index, encoded := range encodedHistory {
		registration, err := decodeClientKeyRegistration(encoded)
		if err != nil {
			return ClientKeyRegistration{}, ClientKeyPin{}, err
		}
		if err := registration.follows(prior); err != nil {
			return ClientKeyRegistration{}, ClientKeyPin{}, err
		}
		if registration.ClientID != [16]byte(clientId) {
			return ClientKeyRegistration{}, ClientKeyPin{}, fmt.Errorf(
				"%w: history names another client", ErrClientKeyRegistrationInvalid)
		}
		if index == 0 {
			domainDigest, err = registration.Domain.Digest()
			if err != nil {
				return ClientKeyRegistration{}, ClientKeyPin{}, err
			}
		}
		// `follows` already pins Domain equality against the predecessor, so
		// checking the head's signer authority covers the chain -- except for
		// the signer itself, which may legitimately differ across generations
		// only if every generation is independently authorized.
		if err := policy.authorizeSigner(domainDigest, registration.Signer); err != nil {
			return ClientKeyRegistration{}, ClientKeyPin{}, err
		}
		owned := registration
		prior = &owned
	}

	head = *prior
	if !head.Present {
		// A tombstoned client has no usable identity key. This is a verified
		// answer, not missing evidence: the peer cannot complete a handshake.
		return ClientKeyRegistration{}, ClientKeyPin{}, fmt.Errorf(
			"%w: head generation withdraws the key", ErrClientKeyRegistrationInvalid)
	}

	if policy.Pin != nil {
		if err := policy.Pin.extendedBy(domainDigest, head); err != nil {
			return ClientKeyRegistration{}, ClientKeyPin{}, err
		}
	}

	pin = ClientKeyPin{
		DomainDigest: domainDigest,
		Signer:       head.Signer,
		Generation:   head.Generation,
		PublicKey:    head.PublicKey,
	}
	return head, pin, nil
}

// authorizeSigner answers the question the signature check cannot: is this
// signer allowed to speak for this domain?
func (self ClientKeyHistoryPolicy) authorizeSigner(domainDigest [32]byte, signer ClientKeyAddress) error {
	if self.Pin != nil && self.Pin.DomainDigest == domainDigest && self.Pin.Signer == signer {
		return nil
	}
	for _, trusted := range self.TrustedSigners {
		if trusted.DomainDigest == domainDigest && trusted.Signer == signer {
			return nil
		}
	}
	if len(self.TrustedSigners) == 0 && self.Pin == nil {
		// trust on first use: nothing to check against yet
		return nil
	}
	return fmt.Errorf("%w: signer %s is not authorized for this domain",
		ErrClientKeyRegistrationInvalid, signer)
}

// extendedBy checks that a freshly verified head is a forward extension of the
// pinned state rather than a fork or a rewind.
func (self *ClientKeyPin) extendedBy(domainDigest [32]byte, head ClientKeyRegistration) error {
	if self.DomainDigest != domainDigest {
		return fmt.Errorf("%w: history changed deployment domain", ErrClientKeyRegistrationInvalid)
	}
	if head.Generation < self.Generation {
		return fmt.Errorf("%w: history rewound below the pinned generation", ErrClientKeyRegistrationInvalid)
	}
	if head.Generation == self.Generation && head.PublicKey != self.PublicKey {
		return fmt.Errorf("%w: pinned generation now names a different key", ErrClientKeyRegistrationInvalid)
	}
	return nil
}

// ClientKeyRegistrationPublicKey returns the head's identity key as an
// ed25519.PublicKey for comparison against a contract-supplied value.
func (self ClientKeyRegistration) ClientKeyRegistrationPublicKey() ed25519.PublicKey {
	if !self.Present {
		return nil
	}
	key := make(ed25519.PublicKey, ed25519.PublicKeySize)
	copy(key, self.PublicKey[:])
	return key
}
