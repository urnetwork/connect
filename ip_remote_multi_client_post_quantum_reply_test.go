package connect

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A platform oob for a client that pays its peer: it grants regular contracts
// like grantingClientOob, and answers a companion request the way the platform
// answers one with no origin contract from the destination, refused with
// ContractError_Reliability (server controller contractResultError). Every
// companion request is counted, and the first closes companionRequested.
type noCompanionOriginClientOob struct {
	grantingClientOob
	companionRequestCount atomic.Int64
	companionRequested    chan struct{}
}

// Takes requests and lends results during the synchronous callback, as
// grantingClientOob does.
func (self *noCompanionOriginClientOob) SendControl(frames []*protocol.Frame, callback func([]*protocol.Frame, error)) {
	delegated := []*protocol.Frame{}
	refusals := []*protocol.Frame{}
	defer func() {
		for _, refusal := range refusals {
			MessagePoolReturn(refusal.MessageBytes)
		}
	}()
	for _, frame := range frames {
		message, err := FromFrame(frame)
		createContract, ok := message.(*protocol.CreateContract)
		if err != nil || !ok || !createContract.Companion {
			delegated = append(delegated, frame)
			continue
		}
		if self.companionRequestCount.Add(1) == 1 {
			close(self.companionRequested)
		}
		contractError := protocol.ContractError_Reliability
		refusal, err := ToFrame(&protocol.CreateContractResult{
			Error:          &contractError,
			CreateContract: createContract,
		}, DefaultProtocolVersion)
		MessagePoolReturn(frame.MessageBytes)
		if err == nil {
			refusals = append(refusals, refusal)
		}
	}
	if len(delegated) == 0 {
		if callback != nil {
			HandleError(func() {
				callback(refusals, nil)
			})
		}
		return
	}
	self.grantingClientOob.SendControl(delegated, func(results []*protocol.Frame, err error) {
		if callback != nil {
			callback(append(results, refusals...), err)
		}
	})
}

// The client settings a post-quantum window gives its window clients, read
// from newMultiClientChannel itself.
func postQuantumWindowClientSettingsForTest(t *testing.T) *ClientSettings {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	generator := &testingRecordingGenerator{}
	_, err := newMultiClientChannel(
		ctx,
		&multiClientChannelArgs{},
		generator,
		nil,
		nil,
		nil,
		nil,
		nil,
		func() {},
		&PerformanceProfile{
			WindowType:            WindowTypeAuto,
			PostQuantumEncryption: true,
		},
		DefaultMultiClientSettings(),
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	// the recording generator fails creation after the settings are applied
	if err == nil || generator.clientSettings == nil {
		t.Fatal("the window did not ask for a window client")
	}
	return generator.clientSettings
}

// A post-quantum window client must be able to answer a session its provider
// starts. Providers built on older connect seal their return traffic in a
// session of their own, opened as TLS client on their companion lane, so the
// window client is the TLS server of it. The window client pays its provider
// and the platform never grants it a companion contract toward it (no origin
// contract from the provider), so a reply on a companion carrier never leaves:
// the provider's session never seals, everything it returns arrives in
// plaintext, and the required gate discards all of it. That was every return
// byte of the fork providers in the post-quantum pool on 2026-11-15. The reply
// must ride a regular contract, and the provider's traffic then arrives sealed.
func TestPostQuantumWindowClientAnswersProviderSessionsOnRegularContracts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	windowClientId := NewId()
	providerId := NewId()

	windowSend := make(chan []byte)
	providerSend := make(chan []byte)
	windowConditioner, providerReceive := newConditioner(ctx, windowSend)
	providerConditioner, windowReceive := newConditioner(ctx, providerSend)
	windowConditioner.update(func() { windowConditioner.randomDelay = 20 * time.Millisecond })
	providerConditioner.update(func() { providerConditioner.randomDelay = 20 * time.Millisecond })

	applyLinkSettings := func(s *ClientSettings) {
		s.Log = NewNoopLogger()
		s.SendBufferSettings.AckTimeout = 60 * time.Second
		s.SendBufferSettings.IdleTimeout = 10 * time.Second
		s.SendBufferSettings.MinResendInterval = 10 * time.Millisecond
		s.ReceiveBufferSettings.GapTimeout = 60 * time.Second
		s.ReceiveBufferSettings.IdleTimeout = 60 * time.Second
		s.ForwardBufferSettings.IdleTimeout = 10 * time.Second
		s.ContractManagerSettings.LegacyCreateContract = false
	}

	// the window client keeps the encryption settings the window gave it
	windowSettings := postQuantumWindowClientSettingsForTest(t)
	applyLinkSettings(windowSettings)

	// an sdk-style provider: opportunistic, replies on companion contracts
	providerSettings := DefaultClientSettingsWithBufferSize(64)
	applyLinkSettings(providerSettings)
	providerSettings.EncryptionSettings.Mode = EncryptionModeOpportunistic

	var windowClient, provider *Client
	windowOob := &noCompanionOriginClientOob{
		companionRequested: make(chan struct{}),
		grantingClientOob: grantingClientOob{
			sourceId: windowClientId,
			settings: DefaultContractManagerSettings(),
			destSecretKey: func(destinationId Id) ([]byte, bool) {
				return provider.ContractManager().GetProvideSecretKey(protocol.ProvideMode_Network)
			},
			destClientPublicKey: func(destinationId Id) []byte {
				return provider.ClientKeyManager().PublicKey()
			},
		},
	}
	// the provider's companion contracts have their origin in the window
	// client's regular contracts
	providerOob := &grantingClientOob{
		sourceId: providerId,
		settings: DefaultContractManagerSettings(),
		destSecretKey: func(destinationId Id) ([]byte, bool) {
			return windowClient.ContractManager().GetProvideSecretKey(protocol.ProvideMode_Network)
		},
		destClientPublicKey: func(destinationId Id) []byte {
			return windowClient.ClientKeyManager().PublicKey()
		},
	}

	provideModes := map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true}

	windowClient = NewClient(ctx, windowClientId, windowOob, windowSettings)
	defer windowClient.Cancel()
	windowClient.RouteManager().UpdateTransport(newDataGatewayTransport(), []Route{windowSend})
	windowClient.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{windowReceive})
	blackholeControlId(ctx, windowClient.RouteManager())
	windowClient.ContractManager().SetProvideModes(provideModes)

	provider = NewClient(ctx, providerId, providerOob, providerSettings)
	defer provider.Cancel()
	provider.RouteManager().UpdateTransport(newDataGatewayTransport(), []Route{providerSend})
	provider.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{providerReceive})
	blackholeControlId(ctx, provider.RouteManager())
	provider.ContractManager().SetProvideModes(provideModes)

	var windowReceiveCount int64
	windowClient.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ Peer) {
		if source.SourceId == providerId {
			countSimpleMessageFrames(frames, &windowReceiveCount)
		}
	})

	send := func(client *Client, destinationId Id, label string, opts ...any) {
		frame, err := ToFrame(&protocol.SimpleMessage{Content: label}, DefaultProtocolVersion)
		if err != nil {
			panic(err)
		}
		if !client.SendWithTimeout(frame, destinationId, func(error) {}, time.Second, opts...) {
			MessagePoolReturn(frame.MessageBytes)
		}
	}

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for i := 0; ; i += 1 {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			default:
			}
			// the window client's own traffic is the provider's origin
			send(windowClient, providerId, fmt.Sprintf("w%d", i))
			// a provider that returns traffic in its own companion-lane session
			send(provider, windowClientId, fmt.Sprintf("p%d", i), TransferOptions{CompanionContract: true})
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()

	// the required gate drops plaintext from the provider, so a delivered
	// message was sealed in the session the provider started
	providerSessionSealed := func() bool {
		session := windowClient.EncryptionSessionManager().Lookup(providerId, sequenceTlsRoleServer, true)
		return session != nil && session.Cipher() != nil
	}
	// a liveness bound only: a companion request fails the test as it is made
	deadline := time.After(60 * time.Second)
	for atomic.LoadInt64(&windowReceiveCount) < 3 {
		select {
		case <-windowOob.companionRequested:
			t.Fatalf(
				"the window client asked for a companion contract toward its provider, which the platform never grants (delivered=%d)",
				atomic.LoadInt64(&windowReceiveCount),
			)
		case <-deadline:
			t.Fatalf(
				"provider traffic never arrived sealed: delivered=%d provider session sealed=%t companion requests=%d",
				atomic.LoadInt64(&windowReceiveCount),
				providerSessionSealed(),
				windowOob.companionRequestCount.Load(),
			)
		case <-time.After(50 * time.Millisecond):
		}
	}
	AssertEqual(t, true, providerSessionSealed())
	// the window client never asked for a contract it cannot be granted
	AssertEqual(t, int64(0), windowOob.companionRequestCount.Load())
}
