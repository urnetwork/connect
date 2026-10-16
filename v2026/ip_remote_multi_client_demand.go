// Fixed-destination discovery hands off inert candidates, not spare identities.
package connect

import (
	"context"
	"errors"
	"time"
)

// Context-aware generators without destination persistence can also keep
// an accepted mint inside its expansion's acquisition deadline.
type multiClientGeneratorArgsContext interface {
	NewClientArgsContext(context.Context) (*MultiClientGeneratorClientArgs, error)
}

// Match discovery's exclusion at the moment a fixed offer is accepted. A
// warning permits replacement; a healthy sibling must not mint a duplicate.
func (self *multiClientWindow) fixedDestinationPresent(destination MultiHopId) bool {
	for _, client := range self.unorderedClients() {
		if !client.isWarning() && client.Destination() == destination {
			return true
		}
	}
	return false
}

// A fixed candidate's expansion owns mint and any retry, never the speculative
// discovery producer. Existing dynamic windows retain their pooled producer.
func (self *multiClientWindow) fixedClientArgs(ctx context.Context, destination MultiHopId) (*MultiClientGeneratorClientArgs, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		callCtx := ctx
		cancelCall := func() {}
		if self.settings.WindowGeneratorTimeout > 0 {
			callCtx, cancelCall = context.WithTimeout(ctx, self.settings.WindowGeneratorTimeout)
		}
		var call func() (*MultiClientGeneratorClientArgs, error)
		cooperative := true
		switch generator := self.generator.(type) {
		case MultiClientGeneratorWithDestinationContext:
			call = func() (*MultiClientGeneratorClientArgs, error) {
				return generator.NewClientArgsForDestinationContext(callCtx, destination)
			}
		case MultiClientGeneratorWithDestination:
			cooperative = false
			call = func() (*MultiClientGeneratorClientArgs, error) {
				return generator.NewClientArgsForDestination(destination)
			}
		case multiClientGeneratorArgsContext:
			call = func() (*MultiClientGeneratorClientArgs, error) {
				return generator.NewClientArgsContext(callCtx)
			}
		default:
			cooperative = false
			call = self.generator.NewClientArgs
		}
		var args *MultiClientGeneratorClientArgs
		var err error
		if cooperative {
			// This capability promises to return on cancellation. Join the
			// actual mint before ownership can leave or retire its identity.
			args, err = HandleError2(call, func(err error) (*MultiClientGeneratorClientArgs, error) { return nil, err })
		} else {
			// Preserve the legacy non-cooperative generator's bounded call
			// and its existing late-identity cleanup, never reuse a late result.
			args, err = windowGeneratorCall(callCtx, self.settings.WindowGeneratorTimeout, call, self.removeLateClientArgs)
		}
		callErr := callCtx.Err()
		cancelCall()
		if ctx.Err() != nil {
			if args != nil {
				self.generator.RemoveClientArgs(args)
			}
			return nil, ctx.Err()
		}
		if callErr != nil {
			err = callErr
		}
		if err == nil && args != nil {
			self.windowRetryReset()
			return args, nil
		}
		if args != nil {
			self.generator.RemoveClientArgs(args)
		}
		if err == nil {
			err = errors.New("client args returned no result")
		}
		self.log.Infof("[multi]create client args error = %s\n", err)
		class := self.recordEvaluationFailure(windowFailurePlatform, err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(self.windowRetryDelay(class)):
		}
	}
}
