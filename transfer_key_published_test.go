// The native publication of a client that requires no processed registration
// is published once the platform acknowledges the ClientKey control that
// carried the key. The platform here is an in-process control client wired to
// the publishing client, which acknowledges what it receives; a barrier before
// the publisher's send holds the before-acknowledgment state still.
package connect

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"testing"
	"time"
)

// One client wired to an in-process ControlId peer, which receives, and so
// acknowledges, every frame the client sends to the platform. Each publisher
// pass signals reached, then waits for release to close.
func newClientKeyPublishedFixture(t *testing.T, ctx context.Context, settings *ClientSettings) (*Client, <-chan struct{}, chan struct{}) {
	t.Helper()
	reached := make(chan struct{}, 4)
	release := make(chan struct{})
	settings.beforeClientKeyPublishForTest = func() {
		select {
		case reached <- struct{}{}:
		case <-ctx.Done():
			return
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	control := NewClientWithDefaults(ctx, ControlId, NewNoContractClientOob())
	control.ContractManager().AddNoContractPeer(client.ClientId())

	toControl := make(chan []byte)
	toClient := make(chan []byte)
	clientSendTransport := NewSendGatewayTransport()
	clientReceiveTransport := NewReceiveGatewayTransport()
	controlSendTransport := NewSendGatewayTransport()
	controlReceiveTransport := NewReceiveGatewayTransport()
	client.RouteManager().UpdateTransport(clientSendTransport, []Route{toControl})
	client.RouteManager().UpdateTransport(clientReceiveTransport, []Route{toClient})
	control.RouteManager().UpdateTransport(controlSendTransport, []Route{toClient})
	control.RouteManager().UpdateTransport(controlReceiveTransport, []Route{toControl})
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := client.CloseAndWait(closeCtx); err != nil {
			t.Errorf("close publishing client: %v", err)
		}
		if err := control.CloseAndWait(closeCtx); err != nil {
			t.Errorf("close control client: %v", err)
		}
	})
	return client, reached, release
}

// Waits until the manager's publication reads acknowledged.
func waitClientKeyPublished(t *testing.T, ctx context.Context, manager *ClientKeyManager) {
	t.Helper()
	if err := manager.WaitForPublication(ctx); err != nil {
		t.Fatal("the platform never acknowledged the client key", err)
	}
}

// Root cause of a standalone provider that never read ready: a client that
// requires no processed registration had no fact for its key reaching the
// platform, since Registered is false for it by design. Published is false
// until the platform acknowledges the ClientKey control, and true after.
func TestClientKeyPublishedOnceThePlatformAcknowledgesTheKey(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	client, reached, release := newClientKeyPublishedFixture(t, ctx, closeWaitClientSettings())
	manager := client.ClientKeyManager()
	waitCloseWaitBarrier(t, ctx, reached, "client key publisher")
	if manager.Published() {
		t.Fatal("a key the publisher has not sent reads published")
	}
	close(release)
	waitClientKeyPublished(t, ctx, manager)
	if !manager.Published() {
		t.Fatal("an acknowledged key does not read published")
	}
	if manager.Registered() {
		t.Fatal("a native acknowledgment reads as a processed registration")
	}
}

// A rotation clears the published state at once, since the platform holds the
// old key until the new one's control is acknowledged, which publishes it.
func TestClientKeyPublishedClearsOnRotationUntilTheNewKeyIsAcknowledged(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	client, reached, release := newClientKeyPublishedFixture(t, ctx, closeWaitClientSettings())
	manager := client.ClientKeyManager()
	waitCloseWaitBarrier(t, ctx, reached, "constructed client key publisher")
	close(release)
	waitClientKeyPublished(t, ctx, manager)

	if err := manager.SetSeed(bytes.Repeat([]byte{37}, ed25519.SeedSize)); err != nil {
		t.Fatal(err)
	}
	if manager.Published() {
		t.Fatal("a rotated key reads published before the platform acknowledged it")
	}
	waitCloseWaitBarrier(t, ctx, reached, "rotated client key publisher")
	waitClientKeyPublished(t, ctx, manager)
	if !manager.Published() {
		t.Fatal("the rotated key does not read published once acknowledged")
	}
}

// An acknowledgment that arrives after a rotation is of the replaced key, and
// records nothing; only the current generation's does. No transport is wired,
// so the only acknowledgments are the ones the test records.
func TestClientKeyPublishedIgnoresTheAcknowledgmentOfAReplacedKey(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), closeWaitClientSettings())
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := client.CloseAndWait(closeCtx); err != nil {
			t.Errorf("close client: %v", err)
		}
	})
	manager := client.ClientKeyManager()
	replaced := func() uint64 {
		manager.stateLock.RLock()
		defer manager.stateLock.RUnlock()
		return manager.publicationGeneration
	}()
	for _, value := range []byte{41, 42} {
		if err := manager.SetSeed(bytes.Repeat([]byte{value}, ed25519.SeedSize)); err != nil {
			t.Fatal(err)
		}
	}
	manager.publicationAcknowledged(replaced)
	if manager.Published() {
		t.Fatal("the acknowledgment of a replaced key published the current one")
	}
	current := func() uint64 {
		manager.stateLock.RLock()
		defer manager.stateLock.RUnlock()
		return manager.publicationGeneration
	}()
	manager.publicationAcknowledged(current)
	if !manager.Published() {
		t.Fatal("the acknowledgment of the current key did not publish it")
	}
	manager.Close()
	if manager.Published() {
		t.Fatal("a closed manager reads published")
	}
}

// A client that requires processed registration reads published exactly when
// it reads registered: a delivery acknowledgment never stands in for the
// processed response there.
func TestClientKeyPublishedFollowsRegistrationWhereRegistrationIsRequired(t *testing.T) {
	forEachIpVersion(t, func(t *testing.T, ipVersion int) {
		client, _, attempts, _ := newClientKeyRegistrationHttpFixture(t, ipVersion)
		manager := client.ClientKeyManager()
		attempt := nextClientKeyRegistrationAttempt(t, attempts)
		if manager.Published() {
			t.Fatal("an unprocessed registration reads published")
		}
		attempt.response <- `{"pack":""}`
		if err := manager.WaitForPublication(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !manager.Registered() || !manager.Published() {
			t.Fatal("a processed registration does not read published")
		}
	})
}

// A wait for a publication that cannot come ends with its caller's context.
func TestClientKeyPublishedWaitEndsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	settings := closeWaitClientSettings()
	settings.SkipClientKeyPublication = true
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := client.CloseAndWait(closeCtx); err != nil {
			t.Errorf("close client: %v", err)
		}
	})
	waitCtx, waitCancel := context.WithCancel(ctx)
	waitCancel()
	if err := client.ClientKeyManager().WaitForPublication(waitCtx); err == nil {
		t.Fatal("a skipped publication's wait succeeded")
	}
	if client.ClientKeyManager().Published() {
		t.Fatal("a skipped publication reads published")
	}
}
