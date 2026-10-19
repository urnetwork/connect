// Reuse of a fixed destination's derived identity across failed attempts.
//
// A fixed-destination generator dials one destination per spec, so the
// identity minted for a destination is exactly the identity its next attempt
// needs. Retiring it after every failed attempt and minting another for the
// retry cost one registration and one retirement transaction per attempt.
// Against an unreachable destination that was most of the hosted proxy's
// database load, and it grew when the database was slow: a dependency timeout
// failed the attempt, and its retry wrote twice more.
//
// An open fixed generator therefore parks a torn-down identity instead of
// retiring it, at most one per destination, and the next dial of that
// destination takes it under a fresh instance id. The fresh instance id is
// the generation token that keeps a late cleanup of the old channel from
// touching the new one (RemoveIfCurrent). An identity the platform refused is
// never parked, nor is the identity of a client that served past a failed
// attempt, so lifetime rotation still changes the identity. A parked identity stays in the live set, so a store persists
// it like any live identity. The generator retires parked identities when it
// closes, except at process shutdown with a store, which keeps them for the
// restarted process.
//
// The next client of a reused identity reuses its identity key too. Every
// client otherwise generates a fresh key, and each registration of a different
// key appends one signed generation to the client id's history: one write per
// attempt again, a chain peers must verify from generation 1, and a finite
// lifetime bound (1024 generations) after which registration fails and the
// identity could never set up again. Re-registering the same key is idempotent.
// The seed is held in memory only; the persisted identity never carries it.
package connect

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"time"
)

// A failed attempt ends within its pass: acquisition, setup and one initial
// ping, 75s at the defaults. A client that lived longer was admitted and served,
// and its teardown is rotation or loss after service. Its identity retires as
// before, so lifetime rotation still yields a fresh identity.
const windowIdentityReuseMaxClientAge = 5 * time.Minute

// A parked identity and, when known, its last client's identity key seed.
type parkedWindowIdentity struct {
	identity      *WindowClientIdentity
	clientKeySeed []byte
}

// Parks the current generation of a live identity for the next dial of its
// destination, with its key seed when known. False when parking has closed,
// the identity is unknown or superseded, or its destination already holds a
// parked identity.
func (self *windowIdentityState) Park(clientId Id, instanceId Id, clientKeySeed []byte) bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.parkingClosed || instanceId == (Id{}) {
		return false
	}
	identity, ok := self.live[clientId]
	if !ok || identity.InstanceId != instanceId {
		return false
	}
	if self.parked == nil {
		self.parked = map[MultiHopId]*parkedWindowIdentity{}
	}
	if _, ok := self.parked[identity.Destination]; ok {
		return false
	}
	self.parked[identity.Destination] = &parkedWindowIdentity{
		identity:      identity,
		clientKeySeed: bytes.Clone(clientKeySeed),
	}
	return true
}

// Takes the identity parked for a destination and its key seed, if any.
func (self *windowIdentityState) TakeParked(destination MultiHopId) (*WindowClientIdentity, []byte) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	parked := self.parked[destination]
	delete(self.parked, destination)
	if parked == nil {
		return nil, nil
	}
	return parked.identity, parked.clientKeySeed
}

// Closes parking and takes every parked identity, atomically, so no identity
// can park after its owner has taken the set to retire.
func (self *windowIdentityState) CloseParking() []*WindowClientIdentity {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.parkingClosed = true
	identities := make([]*WindowClientIdentity, 0, len(self.parked))
	for _, parked := range self.parked {
		identities = append(identities, parked.identity)
	}
	self.parked = nil
	return identities
}

// Parks a torn-down identity of an open fixed generator with its last key
// seed. `reusable` is false when the platform refused the identity.
func (self *ApiMultiClientGenerator) parkIdentity(args *MultiClientGeneratorClientArgs, reusable bool, clientKeySeed []byte) bool {
	if !self.reuseFixedIdentities || !reusable || args == nil || args.ClientAuth == nil || self.ctx.Err() != nil {
		return false
	}
	if size, fixed := self.FixedDestinationSize(); !fixed || size == 0 {
		return false
	}
	return self.identityState.Park(args.ClientId, args.ClientAuth.InstanceId, clientKeySeed)
}

// The parked identity for a destination as fresh client args, under a new
// instance id and with its key seed, or nil.
func (self *ApiMultiClientGenerator) takeParkedIdentity(destination MultiHopId) *MultiClientGeneratorClientArgs {
	parked, clientKeySeed := self.identityState.TakeParked(destination)
	if parked == nil {
		return nil
	}
	identity := *parked
	identity.InstanceId = NewId()
	self.identityState.Record(&identity)
	return &MultiClientGeneratorClientArgs{
		ClientId: identity.ClientId,
		ClientAuth: &ClientAuth{
			ClientInfo: ClientInfoFromContext(self.ctx), StreamLeaseVersion: 1,
			ByJwt:      identity.ByJwt,
			InstanceId: identity.InstanceId,
			AppVersion: self.appVersion,
		},
		clientKeySeed: clientKeySeed,
	}
}

// Closes parking and retires every parked identity on the generator's
// retirement lifecycle, which the caller has not yet closed. At process
// shutdown with a store, the retirement keeps the identity for restoration.
func (self *ApiMultiClientGenerator) retireParkedIdentities() {
	for _, identity := range self.identityState.CloseParking() {
		args := &MultiClientGeneratorClientArgs{
			ClientId: identity.ClientId,
			ClientAuth: &ClientAuth{
				ByJwt:      identity.ByJwt,
				InstanceId: identity.InstanceId,
			},
		}
		retirements := self.retirementLifecycle()
		retirementAdmitted := retirements.start()
		go HandleError(func() {
			if retirementAdmitted {
				defer retirements.finish()
			}
			retireTimeout := self.clientStrategy.settings.RequestTimeout
			if retireTimeout < 30*time.Second {
				retireTimeout = 30 * time.Second
			}
			retireCtx, retireCancel := context.WithTimeout(context.Background(), retireTimeout)
			defer retireCancel()
			self.recordRetirementError("identity", self.removeClientArgsAndWait(retireCtx, args, false, nil))
		})
	}
}

// A definitive refusal of a window client's credential makes its identity
// unusable; an unavailable close says nothing about the credential.
func (self *ApiMultiClientGenerator) noteWindowCredentialRefused(client *Client, cause AuthorizationCloseCause) {
	if !definitiveAuthorizationClose(cause) {
		return
	}
	self.transportLock.Lock()
	defer self.transportLock.Unlock()
	if state := self.transports[client]; state != nil {
		state.refused = true
	}
}

// Whether a setup failure is the platform refusing the window credential.
func credentialRefusedSetupError(err error) bool {
	var status *HttpStatusError
	return errors.As(err, &status) &&
		(status.StatusCode == http.StatusUnauthorized || status.StatusCode == http.StatusForbidden)
}
