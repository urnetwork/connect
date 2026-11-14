package connect

import (
	"context"
	"time"
)

// NetworkClientResidentRetirement optionally captures the exact resident owned
// by a generated client. Prepare must be read-only and must not refresh its TTL.
// The returned callback may remove only that captured generation. The generator
// invokes it only after carrier, Client and OOB joins and successful identity
// retirement. An absent, failed or late capture leaves ordinary expiry in charge.
type NetworkClientResidentRetirement interface {
	PrepareResidentRetirement(context.Context, Id, Id) (func(context.Context) error, error)
}

const residentRetirementCaptureTimeout = time.Second

func (self *ApiMultiClientGenerator) prepareResidentRetirement(args *MultiClientGeneratorClientArgs) (func(context.Context) error, <-chan struct{}) {
	authority, ok := self.clientCredentials.(NetworkClientResidentRetirement)
	if !ok || args.ClientAuth == nil || args.ClientId == (Id{}) || args.ClientAuth.InstanceId == (Id{}) {
		return nil, nil
	}
	clientId, instanceId := args.ClientId, args.ClientAuth.InstanceId
	ctx, cancel := context.WithTimeout(context.Background(), residentRetirementCaptureTimeout)
	defer cancel()
	type captured struct {
		commit func(context.Context) error
		err    error
	}
	result := make(chan captured, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Optional cleanup must not panic away the existing drain owner. A
		// panic publishes no callback and leaves the original expiry backstop.
		defer func() {
			if recover() != nil {
				result <- captured{}
			}
		}()
		commit, err := authority.PrepareResidentRetirement(ctx, clientId, instanceId)
		result <- captured{commit: commit, err: err}
	}()
	select {
	case value := <-result:
		// A ready result must not win a deadline race and authorize late data.
		if ctx.Err() == nil && value.err == nil {
			return value.commit, done
		}
	case <-ctx.Done():
	}
	return nil, done
}
