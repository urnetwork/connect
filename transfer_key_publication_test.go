// The actual publisher writes to a captured route. Its private barrier is the
// decision point after readiness that publishing and skipping managers both
// reach; a joined close makes every send final before a negative is read.
package connect

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// One client whose every routed frame lands on route. Each publisher pass
// signals reached, then waits for release to close.
func newClientKeyPublicationFixture(
	t *testing.T,
	ctx context.Context,
	settings *ClientSettings,
) (*Client, Route, <-chan struct{}, chan struct{}) {
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
	route := make(Route, 64)
	client.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{route})
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := client.CloseAndWait(closeCtx); err != nil {
			t.Errorf("close publication client: %v", err)
		}
		drainFlightGateRoute(route)
	})
	return client, route, reached, release
}

// Returns the ClientKey control one routed transfer frame carries, if any, and
// returns the frame's pooled bytes.
func routedClientKey(t *testing.T, transferFrameBytes []byte) *protocol.ClientKey {
	t.Helper()
	if transferFrameBytes == nil {
		return nil
	}
	defer MessagePoolReturn(transferFrameBytes)
	var transferFrame protocol.TransferFrame
	if err := ProtoUnmarshal(transferFrameBytes, &transferFrame); err != nil {
		t.Fatalf("decode TransferFrame: %v", err)
	}
	pack := transferFrame.Pack
	if pack == nil {
		frame := transferFrame.GetFrame()
		if frame == nil || frame.GetMessageType() != protocol.MessageType_TransferPack {
			return nil
		}
		pack = &protocol.Pack{}
		if err := ProtoUnmarshal(frame.MessageBytes, pack); err != nil {
			t.Fatalf("decode Pack: %v", err)
		}
	}
	for _, frame := range pack.Frames {
		if frame.GetMessageType() != protocol.MessageType_TransferClientKey {
			continue
		}
		message, err := FromFrame(frame)
		if err != nil {
			t.Fatalf("decode ClientKey: %v", err)
		}
		clientKey, ok := message.(*protocol.ClientKey)
		if !ok {
			t.Fatalf("ClientKey frame decoded as %T", message)
		}
		return clientKey
	}
	return nil
}

// The zero value keeps today's behavior: the released publisher writes the
// client's actual key on its control route.
func TestClientKeyPublicationDefaultSendsClientKeyFrame(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	settings := closeWaitClientSettings()
	if settings.SkipClientKeyPublication {
		t.Fatal("default client settings skip client key publication")
	}
	client, route, reached, release := newClientKeyPublicationFixture(t, ctx, settings)
	waitCloseWaitBarrier(t, ctx, reached, "client key publisher")
	close(release)
	publicKey := client.ClientKeyManager().PublicKey()
	for {
		select {
		case message := <-route:
			if clientKey := routedClientKey(t, message); clientKey != nil {
				if !bytes.Equal(clientKey.PublicKey, publicKey) {
					t.Fatal("the published ClientKey is not the client's key")
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("the default client never wrote its ClientKey frame")
		}
	}
}

// Root cause: a short-lived client whose key nothing reads still published it,
// and the platform kept a permanent signed registration per client. With the
// skip, neither the constructor's publisher nor a rotation's sends a ClientKey
// control, and nothing waits for one.
func TestClientKeyPublicationSkippedSendsNoClientKeyFrame(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	settings := closeWaitClientSettings()
	settings.SkipClientKeyPublication = true
	client, route, reached, release := newClientKeyPublicationFixture(t, ctx, settings)
	manager := client.ClientKeyManager()
	if manager == nil {
		t.Fatal("a client whose key nothing reads was refused its key manager")
	}
	waitCloseWaitBarrier(t, ctx, reached, "constructed client key publisher")
	close(release)
	if err := manager.SetSeed(bytes.Repeat([]byte{31}, ed25519.SeedSize)); err != nil {
		t.Fatal(err)
	}
	waitCloseWaitBarrier(t, ctx, reached, "rotated client key publisher")
	if manager.Registered() {
		t.Fatal("an unpublished key reported registration")
	}
	if err := manager.WaitForRegistration(ctx); err == nil {
		t.Fatal("a registration wait succeeded for an unpublished key")
	}
	// the join ends both publishers and every control retry, so the sends
	// read below are final
	if err := client.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	manager.controlSync.sendLock.Lock()
	sendCount := manager.controlSync.syncCount
	manager.controlSync.sendLock.Unlock()
	if sendCount != 0 {
		t.Fatalf("a skipped publication sent %d ClientKey controls", sendCount)
	}
	for {
		select {
		case message := <-route:
			if routedClientKey(t, message) != nil {
				t.Fatal("a skipped publication wrote a ClientKey frame")
			}
		default:
			return
		}
	}
}

// A skipped key must have no reader. Every reader constructs without the skip
// and is refused with it, so the refusal is the contradiction, not the reader.
func TestClientKeyPublicationSkipRefusesEveryKeyReader(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	strategy := NewClientStrategyWithDefaults(ctx)
	defer strategy.Close()
	cases := []struct {
		reader    string
		configure func(*ClientSettings)
	}{
		{reader: "", configure: func(*ClientSettings) {}},
		{reader: "a required registration", configure: func(settings *ClientSettings) {
			settings.ClientKeyRegistrationRequired = true
		}},
		{reader: "a close-report domain", configure: func(settings *ClientSettings) {
			settings.ContractManagerSettings.CloseReportDomainHash = [32]byte{7}
		}},
		{reader: "opportunistic sessions", configure: func(settings *ClientSettings) {
			settings.EncryptionSettings.Mode = EncryptionModeOpportunistic
		}},
		{reader: "required sessions", configure: func(settings *ClientSettings) {
			settings.EncryptionSettings.Mode = EncryptionModeRequired
		}},
	}
	for _, c := range cases {
		for _, skip := range []bool{false, true} {
			clientCtx, clientCancel := context.WithCancel(ctx)
			settings := closeWaitClientSettings()
			settings.SkipClientKeyPublication = skip
			c.configure(settings)
			// every publisher parks before any send, so no request leaves
			settings.beforeClientKeyPublishForTest = func() { <-clientCtx.Done() }
			// the actual processed control, so a required registration
			// constructs without the skip
			control := NewApiOutOfBandControl(clientCtx, strategy, "synthetic-publication-token", "https://api.example")
			client := NewClient(clientCtx, NewId(), control, settings)
			refused := client.ClientKeyManager() == nil
			if want := skip && c.reader != ""; refused != want {
				t.Errorf("reader %q with skip %t: refused %t, want %t", c.reader, skip, refused, want)
			}
			clientCancel()
			if err := client.CloseAndWait(ctx); err != nil {
				t.Error(err)
			}
			if err := control.CloseAndWait(ctx); err != nil {
				t.Error(err)
			}
		}
	}
}
