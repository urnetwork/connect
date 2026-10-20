// A channel/stream replacement is not a new provider NAT namespace. Renewal
// after an ambiguous written error requires a distinct authenticated source.
package connect

import (
	"context"
	"errors"
)

var errAffinityFreshIdentityUnsupported = errors.New("generator cannot provide a fresh affinity source identity")
var errAffinityIdentityReused = errors.New("affinity renewal received a reused source identity")

// Optional, context-bounded identity acquisition. Implementations MUST mint a
// new authenticated source, bypassing parked/restored credentials and key
// seeds, without closing or replacing any existing Client. An error never
// authorizes a fallback to a potentially reused ordinary destination identity.
// A returned non-nil distinct identity remains caller-owned even with error.
// An invalid reused result does not transfer revocation authority: the caller
// refuses it without removing an identity that may still serve other flows.
type MultiClientGeneratorWithFreshDestinationContext interface {
	NewClientArgsForFreshDestinationContext(context.Context, MultiHopId, Id) (*MultiClientGeneratorClientArgs, error)
}

// The production authority's raw mint already creates a new authenticated
// client id and instance id. Do not call the ordinary destination method here:
// it deliberately resumes parked/restored identities, including their NAT.
func (self *ApiMultiClientGenerator) NewClientArgsForFreshDestinationContext(
	ctx context.Context,
	destination MultiHopId,
	excludedSourceId Id,
) (*MultiClientGeneratorClientArgs, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	args, err := self.NewClientArgsContext(ctx)
	if err != nil || args == nil {
		return args, err
	}
	if args.ClientId == excludedSourceId || args.ClientId == (Id{}) || args.ClientAuth == nil {
		// A broken authority has not supplied an independently owned source.
		// Never revoke the incumbent identity while rejecting that result.
		return nil, errAffinityIdentityReused
	}
	if !self.identityState.recordFreshAffinityIdentity(&WindowClientIdentity{
		ClientId: args.ClientId, ByJwt: args.ClientAuth.ByJwt,
		InstanceId: args.ClientAuth.InstanceId, Destination: destination,
	}) {
		return nil, errAffinityIdentityReused
	}
	return args, ctx.Err()
}

// Publication cannot overwrite a live sibling or an identity awaiting restore.
// This is only on explicit renewal setup, never the packet or ordinary mint
// path. A generator's authority must still guarantee globally fresh ids.
func (self *windowIdentityState) recordFreshAffinityIdentity(identity *WindowClientIdentity) bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.live[identity.ClientId] != nil {
		return false
	}
	for _, restored := range self.restored {
		for _, previous := range restored {
			if previous.ClientId == identity.ClientId {
				return false
			}
		}
	}
	self.live[identity.ClientId] = identity
	self.storeSnapshotWithLock()
	return true
}

// Called only after a window acquisition owner reserves its normal setup and
// hard-max capacity. This helper neither launches a detached mint nor retries
// outside that owner's context. Unsupported generators preserve caller retry
// ownership instead of pretending a new channel is a safe new identity.
func (self *multiClientWindow) freshAffinityClientArgs(
	ctx context.Context,
	destination MultiHopId,
	excludedSourceId Id,
) (*MultiClientGeneratorClientArgs, error) {
	generator, ok := self.generator.(MultiClientGeneratorWithFreshDestinationContext)
	if !ok {
		return nil, errAffinityFreshIdentityUnsupported
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	args, err := generator.NewClientArgsForFreshDestinationContext(ctx, destination, excludedSourceId)
	if args != nil && (args.ClientId == excludedSourceId || args.ClientId == (Id{}) || len(args.clientKeySeed) != 0) {
		// Returning this through RemoveClientArgs could revoke the live old
		// source. No independent identity ownership was established.
		return nil, errAffinityIdentityReused
	}
	if args != nil {
		self.stateLock.Lock()
		previous := self.clients[args.ClientId]
		self.stateLock.Unlock()
		if previous != nil {
			return nil, errAffinityIdentityReused
		}
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		if args != nil {
			self.generator.RemoveClientArgs(args)
		}
		return nil, err
	}
	if args == nil {
		return nil, errors.New("fresh affinity identity acquisition returned no result")
	}
	return args, nil
}
