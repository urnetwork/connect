package connect

// Session-side resolution of a peer's signed client-key registration history.
//
// `transfer_key_history.go` verifies a chain. This file decides what to do
// with the answer, and the decisions are the security-relevant part.
//
// The contract key is committed for certificate and identity-proof checks while
// its history is resolved. Required application admission checks this gate at
// outbound Cipher(), inbound cipher selection, and queued/per-receipt delivery.
// Bootstrap controls can therefore complete while application data stays closed.
// Store-backed decisions compare and swap the complete pin/latch snapshot used
// for verification, including decisions that only read the store. The successful
// compare is the admission point; later rotations do not revoke prior admissions.
//
// See DESIGNNOTES3 §5 for the policy this implements and §8 for what it does
// and does not close.

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"strings"
	"time"
)

// clientKeyHistoryState is the per-session signed-identity gate.
type clientKeyHistoryState int

const (
	// clientKeyHistoryNotRequired: the session does not corroborate identity
	// keys, because the mode is not Required, no fetcher is configured, or no
	// contract key has arrived yet. `Cipher()` is ungated.
	clientKeyHistoryNotRequired clientKeyHistoryState = iota
	// clientKeyHistoryPending: a contract key was committed and resolution is
	// in flight. `Cipher()` is withheld.
	clientKeyHistoryPending
	// clientKeyHistoryVerified: the contract key is corroborated, or the peer
	// is legitimately on the unsigned path and the ratchet permits it.
	// `Cipher()` is ungated.
	clientKeyHistoryVerified
	// clientKeyHistoryRejected: signed evidence contradicts the contract key,
	// or a downgrade was refused. Terminal — never re-enters pending.
	clientKeyHistoryRejected
	// Local persistence/admission failure is terminal for this session, but
	// is not evidence against the peer and must not poison peer exclusion.
	clientKeyHistoryStoreUnavailable
)

func (self clientKeyHistoryState) String() string {
	switch self {
	case clientKeyHistoryPending:
		return "pending"
	case clientKeyHistoryVerified:
		return "verified"
	case clientKeyHistoryRejected:
		return "rejected"
	case clientKeyHistoryStoreUnavailable:
		return "local pin store unavailable"
	default:
		return "not required"
	}
}

// PeerClientKeyPinStore persists the tier ratchet across sessions and process
// lifetimes.
//
// The ratchet exists because the fallback would otherwise reintroduce the hole
// it is closing: if a client accepts "no signed history" whenever none is
// offered, an operator substitutes nothing and simply withholds the history.
// Omission is cheaper and quieter than forgery, so tier may only ratchet
// upward. See DESIGNNOTES3 §4.
//
// Implementations must be safe for concurrent use.
type PeerClientKeyPinStore interface {
	// GetPeerClientKeyPin returns the peer's pinned state, or false when the
	// peer has never been verified against signed evidence.
	GetPeerClientKeyPin(peerId Id) (ClientKeyPin, bool)
	// SetPeerClientKeyPin records a freshly verified head for the peer.
	SetPeerClientKeyPin(peerId Id, pin ClientKeyPin)
	// SignedHistorySeen reports whether ANY peer has ever been verified
	// against signed evidence through this store. Once true, a peer offering
	// no signed history at all is a downgrade rather than a legacy peer.
	SignedHistorySeen() bool
	// SetSignedHistorySeen latches the above.
	SetSignedHistorySeen()
}

// CheckedPeerClientKeyPinStore makes durable admission part of opening the
// Required cipher gate. Commit must atomically retain the pin and the global
// signed-history latch, or return an error without dropping existing pins.
// Kept for compatibility with existing Go stores; Required resolution uses the
// stronger atomic interface below to bind verification to durable admission.
type CheckedPeerClientKeyPinStore interface {
	PeerClientKeyPinStore
	GetPeerClientKeyPinChecked(Id) (ClientKeyPin, bool, error)
	CommitPeerClientKeyPin(Id, ClientKeyPin) error
}

// One atomic view of the peer ratchet and the store-wide signed-history latch.
// Pinned distinguishes a missing peer from a stored zero value. Missing peers
// must have a zero Pin so snapshots have one comparable representation.
type ClientKeyPinSnapshot struct {
	Pin               ClientKeyPin
	Pinned            bool
	SignedHistorySeen bool
}

// Admits a resolution against the exact snapshot it verified. Implementations
// synchronize both methods with every other pin/latch mutation in the store,
// including legacy setters. A failed comparison returns false, nil and changes
// nothing; success with nextPin != nil durably commits that pin and the latch.
// A nil nextPin only checks the snapshot, without writing or setting the latch.
// Errors must preserve existing pins and keep admission closed.
// A non-nil nextPin must come from complete chain and signer-policy verification
// against the supplied snapshot. The store conditionally persists that decision;
// it does not establish cryptographic authority for a new key or signer itself.
//
// Required sessions with a configured store require this interface. The older
// interfaces remain source-compatible, but cannot safely admit a resolution
// across managers. Owners must upgrade their store once, shared by every user;
// independently wrapping it per client would recreate the race.
type AtomicPeerClientKeyPinStore interface {
	CheckedPeerClientKeyPinStore
	ReadPeerClientKeyPinSnapshot(Id) (ClientKeyPinSnapshot, error)
	CompareAndSwapPeerClientKeyPin(Id, ClientKeyPinSnapshot, *ClientKeyPin) (bool, error)
}

// keyHistoryRequiredWithLock reports whether this session gates `Cipher()` on
// signed identity corroboration. Callers hold stateLock.
//
// Scoped to `EncryptionModeRequired` deliberately: refusing under
// Opportunistic degrades to plaintext, which is exactly what a substituting
// operator wanted, so enforcement is only meaningful where fail-closed already
// holds (DESIGNNOTES2 §4).
func (self *peerEncryptionSession) keyHistoryRequiredWithLock() bool {
	return self.peerClientKeyHistoryFetcher != nil &&
		self.settings != nil &&
		self.settings.Mode == EncryptionModeRequired
}

// resolvePeerClientKeyHistory corroborates the contract-supplied identity key
// against the peer's signed registration history, then opens or closes the
// gate armed when the key was committed.
//
// Single-flight and rate-limited for the same reason the identity-key fetcher
// is: a verify gate can be re-entered on contract churn, and a peer with no
// published history must not turn that into a request storm.
func (self *peerEncryptionSession) resolvePeerClientKeyHistory(contractPub ed25519.PublicKey) {
	start := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.keyHistoryRequiredWithLock() {
			return false
		}
		if self.keyHistoryState != clientKeyHistoryPending {
			return false
		}
		if self.keyHistoryFetchInFlight {
			return false
		}
		now := time.Now()
		if now.Before(self.nextKeyHistoryFetchTime) {
			return false
		}
		self.keyHistoryFetchInFlight = true
		self.nextKeyHistoryFetchTime = now.Add(1 * time.Second)
		return true
	}()
	if !start {
		return
	}

	self.startWorker("signed identity resolve", func() {
		defer func() {
			self.stateLock.Lock()
			self.keyHistoryFetchInFlight = false
			self.stateLock.Unlock()
		}()

		encodedHistory, err := self.peerClientKeyHistoryFetcher(self.ctx)
		if err != nil {
			self.applyPeerClientKeyHistoryFetchError(contractPub, err)
			return
		}
		self.applyPeerClientKeyHistory(contractPub, encodedHistory)
	})
}

// Applies the availability policy through the same atomic admission path as a
// signed response. A changed pin cannot race a fallback into opening the gate.
func (self *peerEncryptionSession) applyPeerClientKeyHistoryFetchError(
	contractPub ed25519.PublicKey,
	fetchErr error,
) {
	self.applyPeerClientKeyHistoryResolution(contractPub, nil, fetchErr)
}

// Resolves signed or explicitly absent evidence against the current ratchet.
func (self *peerEncryptionSession) applyPeerClientKeyHistory(
	contractPub ed25519.PublicKey,
	encodedHistory [][]byte,
) {
	self.applyPeerClientKeyHistoryResolution(contractPub, encodedHistory, nil)
}

// Verifies without store or session locks, then conditionally admits the result.
// Conflicts re-read and re-verify within the owner's attempt bound. No registry
// or per-manager lock is needed: every manager compares against the same store.
// A rejection also compares its snapshot so a concurrent legitimate rotation
// cannot turn a stale disagreement into a terminal peer accusation.
func (self *peerEncryptionSession) applyPeerClientKeyHistoryResolution(
	contractPub ed25519.PublicKey,
	encodedHistory [][]byte,
	fetchErr error,
) {
	if self.ctx.Err() != nil {
		return
	}
	terminal := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.keyHistoryState == clientKeyHistoryRejected || self.keyHistoryState == clientKeyHistoryStoreUnavailable
	}()
	if terminal {
		return
	}

	var store AtomicPeerClientKeyPinStore
	attempts := 1 // No configured store needs no compare/retry.
	if configured := self.settings.PeerClientKeyPinStore; configured != nil {
		var ok bool
		store, ok = configured.(AtomicPeerClientKeyPinStore)
		if !ok {
			self.blockKeyHistoryStore(errors.New("peer identity pin store does not support atomic admission"))
			return
		}
		attempts = self.settings.MaxClientKeyPinAdmissionAttempts
		if attempts <= 0 {
			self.blockKeyHistoryStore(errors.New("peer identity pin admission attempts must be positive"))
			return
		}
	}

	var previousSnapshot ClientKeyPinSnapshot
	for attempt := 0; attempt < attempts; attempt++ {
		if self.ctx.Err() != nil {
			return
		}
		var snapshot ClientKeyPinSnapshot
		if store != nil {
			var err error
			snapshot, err = store.ReadPeerClientKeyPinSnapshot(self.peerId)
			if err != nil {
				self.blockKeyHistoryStore(err)
				return
			}
			if !snapshot.Pinned && snapshot.Pin != (ClientKeyPin{}) {
				self.blockKeyHistoryStore(errors.New("missing peer identity pin has nonzero snapshot state"))
				return
			}
			if attempt != 0 && snapshot == previousSnapshot {
				self.blockKeyHistoryStore(errors.New("peer identity pin compare failed without a snapshot change"))
				return
			}
		}

		state, reason, nextPin := func() (clientKeyHistoryState, string, *ClientKeyPin) {
			if fetchErr != nil {
				if snapshot.Pinned && !bytes.Equal(snapshot.Pin.PublicKey[:], contractPub) {
					return clientKeyHistoryRejected, "contract identity key differs from the pinned signed key while no signed history is available", nil
				}
				return clientKeyHistoryVerified, "", nil
			}
			if len(encodedHistory) == 0 {
				switch {
				case snapshot.Pinned:
					return clientKeyHistoryRejected, "peer previously verified with signed evidence now offers none", nil
				case snapshot.SignedHistorySeen:
					return clientKeyHistoryRejected, "platform previously served signed evidence and now offers none for this peer", nil
				default:
					return clientKeyHistoryVerified, "", nil
				}
			}

			policy := ClientKeyHistoryPolicy{
				TrustedSigners: self.settings.TrustedClientKeySigners,
				MaxGenerations: self.settings.MaxClientKeyHistoryGenerations,
			}
			if snapshot.Pinned {
				pin := snapshot.Pin
				policy.Pin = &pin
			}
			head, pin, err := VerifyClientKeyHistory(self.peerId, encodedHistory, policy)
			if err != nil {
				return clientKeyHistoryRejected, errors.Join(errors.New("signed identity evidence did not verify"), err).Error(), nil
			}
			if !bytes.Equal(head.ClientKeyRegistrationPublicKey(), contractPub) {
				return clientKeyHistoryRejected, "contract identity key contradicts the signed registration head", nil
			}
			return clientKeyHistoryVerified, "", &pin
		}()

		if self.ctx.Err() != nil {
			return
		}
		if store != nil {
			admitted, err := store.CompareAndSwapPeerClientKeyPin(self.peerId, snapshot, nextPin)
			if err != nil {
				self.blockKeyHistoryStore(err)
				return
			}
			if !admitted {
				previousSnapshot = snapshot
				continue
			}
		}
		if self.ctx.Err() != nil {
			return
		}
		if state == clientKeyHistoryRejected {
			self.rejectKeyHistory(reason)
			return
		}
		switch {
		case fetchErr != nil:
			self.client.log.Infof("[key]%s signed identity evidence unavailable (%s) — falling back to the contract key\n", self.logTag, fetchErr)
		case nextPin == nil:
			self.client.log.Infof("[key]%s peer has no signed identity evidence (unsigned path)\n", self.logTag)
		default:
			self.client.log.Infof("[key]%s signed identity verified — generation %d, signer %s\n", self.logTag, nextPin.Generation, nextPin.Signer)
		}
		self.openKeyHistoryGate(clientKeyHistoryVerified, "")
		return
	}
	if self.ctx.Err() == nil {
		self.blockKeyHistoryStore(errors.New("peer identity pin admission attempt limit exceeded"))
	}
}

// openKeyHistoryGate moves the gate out of pending. A rejected session is
// terminal and is never reopened.
func (self *peerEncryptionSession) openKeyHistoryGate(state clientKeyHistoryState, reason string) {
	changed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.keyHistoryState == clientKeyHistoryRejected || self.keyHistoryState == clientKeyHistoryStoreUnavailable {
			return false
		}
		if self.keyHistoryState == state || state == clientKeyHistoryVerified && self.ctx.Err() != nil {
			return false
		}
		self.keyHistoryState = state
		self.notifyRequiredCipherChangedWithLock()
		return true
	}()
	if !changed {
		return
	}
	if state == clientKeyHistoryRejected || state == clientKeyHistoryStoreUnavailable {
		// Tear the epoch down as well as withholding the cipher. A handshake
		// cannot make this terminal identity or local-store failure usable.
		self.stateLock.Lock()
		epoch := self.epoch
		self.stateLock.Unlock()
		if epoch != nil && epoch.cancel != nil {
			epoch.cancel()
		}
		if self.manager != nil {
			eventType := EncryptionEventKeyIdentityRejected
			if state == clientKeyHistoryStoreUnavailable {
				eventType = EncryptionEventKeyIdentityStoreUnavailable
			}
			self.manager.encryptionEvent(&EncryptionEvent{
				PeerId: self.peerId,
				Type:   eventType,
				Reason: reason,
			})
		}
	}
	self.notifyIdleStateChanged()
}

func (self *peerEncryptionSession) blockKeyHistoryStore(err error) {
	// An external checked implementation may return a verbose error; retain
	// only a bounded diagnostic, never arbitrary per-peer error payloads.
	reason := err.Error()
	// Clone even a short string: a custom error may itself return a small
	// substring backed by a much larger allocation.
	reason = strings.Clone(reason[:min(len(reason), 256)])
	self.client.log.Errorf("[key]%s local identity pin store unavailable: %s\n", self.logTag, reason)
	self.openKeyHistoryGate(clientKeyHistoryStoreUnavailable, reason)
}

func (self *peerEncryptionSession) rejectKeyHistory(reason string) {
	self.client.log.Errorf(
		"[key]%s SIGNED IDENTITY REJECTED for %s — %s\n", self.logTag, self.peerId, reason,
	)
	self.openKeyHistoryGate(clientKeyHistoryRejected, reason)
}

// KeyIdentityRejected reports whether this session refused the peer's identity
// key against signed evidence. A window uses it to exclude the provider rather
// than retry it.
func (self *peerEncryptionSession) KeyIdentityRejected() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.keyHistoryState == clientKeyHistoryRejected
}
