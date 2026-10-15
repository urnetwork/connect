// The actual publisher writes to a captured route, or posts a processed
// registration to a recording endpoint. Its private barrier is the decision
// point after readiness that publishing and skipping managers both reach; a
// joined close makes every send final before a negative is read.
package connect

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	clientOob OutOfBandControl,
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
	client := NewClient(ctx, NewId(), clientOob, settings)
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

// Answers every processed ClientKey registration at once and hands its key to
// the test. Only the processed path posts here; native publication is routed.
func newClientKeyPublicationRegistrationEndpoint(t *testing.T) (*httptest.Server, <-chan []byte) {
	t.Helper()
	keys := make(chan []byte, 8)
	endpoint := newFamilyHttptestServer(t, 4, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hello" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/connect/control" {
			http.NotFound(w, r)
			return
		}
		var args ConnectControlArgs
		if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&args); err != nil {
			http.Error(w, "invalid control", 400)
			return
		}
		raw, err := base64.StdEncoding.DecodeString(args.Pack)
		if err != nil {
			http.Error(w, "invalid pack", 400)
			return
		}
		var pack protocol.Pack
		if err := ProtoUnmarshal(raw, &pack); err != nil {
			http.Error(w, "invalid frames", 400)
			return
		}
		for _, frame := range pack.Frames {
			message, err := FromFrame(frame)
			if err != nil {
				continue
			}
			if clientKey, ok := message.(*protocol.ClientKey); ok {
				select {
				case keys <- bytes.Clone(clientKey.PublicKey):
				default:
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"pack":""}`)
	}))
	t.Cleanup(endpoint.Close)
	return endpoint, keys
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
	client, route, reached, release := newClientKeyPublicationFixture(t, ctx, settings, NewNoContractClientOob())
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
	client, route, reached, release := newClientKeyPublicationFixture(t, ctx, settings, NewNoContractClientOob())
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

// A skip that meets a reader of the key is ignored, not refused: the client
// keeps its key manager and publishes its actual key, natively or through
// processed registration, as it would without the skip. A misapplied skip
// costs one registration, never the reader's traffic or signatures.
func TestClientKeyPublicationSkipYieldsToEveryKeyReader(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	strategy := NewClientStrategyWithDefaults(ctx)
	t.Cleanup(strategy.Close)
	endpoint, registeredKeys := newClientKeyPublicationRegistrationEndpoint(t)
	cases := []struct {
		reader       string
		configure    func(*ClientSettings)
		registration bool
	}{
		{reader: "a required registration", configure: func(settings *ClientSettings) {
			settings.ClientKeyRegistrationRequired = true
		}, registration: true},
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
		settings := closeWaitClientSettings()
		settings.SkipClientKeyPublication = true
		c.configure(settings)
		// the actual processed control, so a required registration completes
		control := NewApiOutOfBandControl(ctx, strategy, "synthetic-publication-token", endpoint.URL)
		t.Cleanup(func() {
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer closeCancel()
			if err := control.CloseAndWait(closeCtx); err != nil {
				t.Errorf("close publication control: %v", err)
			}
		})
		client, route, reached, release := newClientKeyPublicationFixture(t, ctx, settings, control)
		manager := client.ClientKeyManager()
		if manager == nil {
			t.Errorf("a skip with %s left the client without a key manager", c.reader)
			continue
		}
		waitCloseWaitBarrier(t, ctx, reached, "client key publisher")
		close(release)
		publicKey := manager.PublicKey()
		if c.registration {
			select {
			case key := <-registeredKeys:
				if !bytes.Equal(key, publicKey) {
					t.Fatalf("a skip with %s registered another key", c.reader)
				}
			case <-ctx.Done():
				t.Fatalf("a skip with %s registered no key", c.reader)
			}
			if err := manager.WaitForRegistration(ctx); err != nil {
				t.Fatalf("a skip with %s left registration unready: %v", c.reader, err)
			}
			continue
		}
		for published := false; !published; {
			select {
			case message := <-route:
				if clientKey := routedClientKey(t, message); clientKey != nil {
					if !bytes.Equal(clientKey.PublicKey, publicKey) {
						t.Fatalf("a skip with %s published another key", c.reader)
					}
					published = true
				}
			case <-ctx.Done():
				t.Fatalf("a skip with %s published no key", c.reader)
			}
		}
	}
}
