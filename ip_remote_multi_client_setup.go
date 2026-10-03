// Window setup has its own cooperative deadline. Its cancellation cannot
// retire a successfully admitted client or become provider-response evidence.
package connect

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The API generator already exposes this narrow capability. Keep the setup
// deadline separate from the context that owns the successfully created client.
type multiClientGeneratorSetup interface {
	NewClientContext(context.Context, context.Context, *MultiClientGeneratorClientArgs, *ClientSettings) (*Client, error)
}

// Owned setup deadlines and API registration failures carry this type.
// argsOwned prevents a constructed client from also taking unused-args cleanup.
type multiClientSetupError struct {
	err       error
	argsOwned bool
}

// Describes the local setup boundary without exposing a destination or token.
func (self *multiClientSetupError) Error() string {
	return fmt.Sprintf("client setup: %v", self.err)
}

// Retains lifecycle matching through the typed ownership wrapper.
func (self *multiClientSetupError) Unwrap() error {
	return self.err
}

// This cause is attached only to the API generator's own processed Provide
// and ClientKey registration waits. It says nothing about a selected peer.
type localControlRegistrationError struct {
	err error
}

func (self *localControlRegistrationError) Error() string {
	return self.err.Error()
}

func (self *localControlRegistrationError) Unwrap() error {
	return self.err
}

// A structural marker lets a caller retain this narrow cause through wrappers
// without treating generic channel setup or peer failures as local faults.
func (*localControlRegistrationError) LocalControlRegistrationFailure() bool {
	return true
}

// Older settings literals retain a finite setup allowance independent of ping.
func (self *MultiClientSettings) clientSetupTimeout() time.Duration {
	if 0 < self.WindowClientSetupTimeout {
		return self.WindowClientSetupTimeout
	}
	return 30 * time.Second
}

// Calls setup synchronously, never abandoning an unjoined constructor. Legacy
// generators must honor their client context; context-aware generators receive
// a separate setup context so a successful client outlives this deadline.
func newMultiClientChannelClient(
	ctx context.Context,
	cancel context.CancelFunc,
	args *MultiClientGeneratorClientArgs,
	generator MultiClientGenerator,
	settings *ClientSettings,
	timeout time.Duration,
) (*Client, error) {
	setupCtx, stopSetup := context.WithTimeoutCause(ctx, timeout, &multiClientSetupError{err: context.DeadlineExceeded})
	defer stopSetup()
	var client *Client
	var err error
	if setupGenerator, ok := generator.(multiClientGeneratorSetup); ok {
		client, err = setupGenerator.NewClientContext(ctx, setupCtx, args, settings)
	} else {
		// The legacy signature has one context. Cancel it only if setup fails
		// to finish in time; stop the bridge before handing the client onward.
		stopCancel := context.AfterFunc(setupCtx, cancel)
		defer stopCancel()
		client, err = generator.NewClient(ctx, args, settings)
		stopCancel()
	}
	var setupErr *multiClientSetupError
	if errors.As(context.Cause(setupCtx), &setupErr) {
		cancel()
		argsOwned := client != nil
		var resultSetupErr *multiClientSetupError
		if errors.As(err, &resultSetupErr) && resultSetupErr.argsOwned {
			argsOwned = true
		}
		if client != nil {
			generator.RemoveClientWithArgs(client, args)
			client.Cancel()
		}
		return nil, &multiClientSetupError{err: errors.Join(context.DeadlineExceeded, err), argsOwned: argsOwned}
	}
	return client, err
}
