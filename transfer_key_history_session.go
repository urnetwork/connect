package connect

// Session-side resolution of a peer's signed client-key registration history.
//
// `transfer_key_history.go` verifies a chain. This file decides what to do
// with the answer, and the decisions are the security-relevant part.
//
// The shape is deliberately NOT "refuse to commit the contract key". The
// contract key is still committed, so the certificate chain and the identity
// proof verify exactly as before; what is withheld is `Cipher()`. Because the
// Required send gate parks application data on `Cipher()` and the receive gate
// discards plaintext application frames, withholding the cipher is sufficient:
// no application byte leaves for, or is accepted from, a peer whose identity
// key has not been corroborated. Withholding also keeps the change to the
// handshake path small, which matters in a file this delicate.
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
// The optional interface preserves compatibility with legacy Go stores.
type CheckedPeerClientKeyPinStore interface {
	PeerClientKeyPinStore
	GetPeerClientKeyPinChecked(Id) (ClientKeyPin, bool, error)
	CommitPeerClientKeyPin(Id, ClientKeyPin) error
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
			// A healthy store retains the existing network-availability policy.
			// Local closed/corrupt storage cannot be bypassed by that fallback.
			if store, ok := self.settings.PeerClientKeyPinStore.(CheckedPeerClientKeyPinStore); ok {
				if _, _, storeErr := store.GetPeerClientKeyPinChecked(self.peerId); storeErr != nil {
					self.blockKeyHistoryStore(storeErr)
					return
				}
			}
			// Availability failure, NOT evidence of substitution. Treating an
			// unreachable platform API as a verified disagreement would
			// exclude every provider for every Required client at once the
			// moment that API blipped — a self-inflicted global outage with
			// the same shape as the blackhole the window exists to route
			// around. Fall back to the contract key and say so.
			// DESIGNNOTES3 §5.3.
			self.client.log.Infof(
				"[key]%s signed identity evidence unavailable (%s) — falling back to the contract key\n",
				self.logTag, err,
			)
			self.openKeyHistoryGate(clientKeyHistoryVerified, "")
			return
		}
		self.applyPeerClientKeyHistory(contractPub, encodedHistory)
	})
}

// applyPeerClientKeyHistory is the decision table of DESIGNNOTES3 §5.2.
func (self *peerEncryptionSession) applyPeerClientKeyHistory(
	contractPub ed25519.PublicKey,
	encodedHistory [][]byte,
) {
	store := self.settings.PeerClientKeyPinStore
	var pin ClientKeyPin
	pinned := false
	if checked, ok := store.(CheckedPeerClientKeyPinStore); ok {
		var err error
		pin, pinned, err = checked.GetPeerClientKeyPinChecked(self.peerId)
		if err != nil {
			self.blockKeyHistoryStore(err)
			return
		}
	} else if store != nil {
		pin, pinned = store.GetPeerClientKeyPin(self.peerId)
	}

	if len(encodedHistory) == 0 {
		// Tier P: the peer has no signed registration. Legitimate for a
		// legacy client or a platform that does not run the signed path —
		// unless this peer, or this platform, has produced signed evidence
		// before, in which case its disappearance is a downgrade.
		switch {
		case pinned:
			self.rejectKeyHistory("peer previously verified with signed evidence now offers none")
		case store != nil && store.SignedHistorySeen():
			self.rejectKeyHistory("platform previously served signed evidence and now offers none for this peer")
		default:
			self.client.log.Infof(
				"[key]%s peer has no signed identity evidence (unsigned path)\n", self.logTag,
			)
			self.openKeyHistoryGate(clientKeyHistoryVerified, "")
		}
		return
	}

	policy := ClientKeyHistoryPolicy{
		TrustedSigners: self.settings.TrustedClientKeySigners,
		Pin:            nil,
		MaxGenerations: self.settings.MaxClientKeyHistoryGenerations,
	}
	if pinned {
		owned := pin
		policy.Pin = &owned
	}

	head, nextPin, err := VerifyClientKeyHistory(self.peerId, encodedHistory, policy)
	if err != nil {
		// A chain was offered and did not verify. That is a positive answer,
		// not a missing one: something signed, or failed to sign, evidence
		// about this peer that does not hold together.
		self.rejectKeyHistory(errors.Join(errors.New("signed identity evidence did not verify"), err).Error())
		return
	}

	if !bytes.Equal(head.ClientKeyRegistrationPublicKey(), contractPub) {
		// The substitution case the whole mechanism exists for: the platform
		// handed us one key in the contract and signed a different one.
		self.rejectKeyHistory("contract identity key contradicts the signed registration head")
		return
	}

	if checked, ok := store.(CheckedPeerClientKeyPinStore); ok {
		if err := checked.CommitPeerClientKeyPin(self.peerId, nextPin); err != nil {
			self.blockKeyHistoryStore(err)
			return
		}
	} else if store != nil {
		store.SetPeerClientKeyPin(self.peerId, nextPin)
		store.SetSignedHistorySeen()
	}
	self.client.log.Infof(
		"[key]%s signed identity verified — generation %d, signer %s\n",
		self.logTag, head.Generation, head.Signer,
	)
	self.openKeyHistoryGate(clientKeyHistoryVerified, "")
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
		if self.keyHistoryState == state {
			return false
		}
		self.keyHistoryState = state
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
