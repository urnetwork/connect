// This file pins the evaluation budget of a window candidate that requires
// encryption (post-quantum mode). Under `EncryptionModeRequired` the initial
// ping cannot leave until the session's cipher is up, and bringing the cipher
// up is network work: the tls handshake and the identity proofs take two
// round trips to the provider before the ping itself takes a third. That work
// is part of the candidate's answer, so it is bounded by `PingTimeout`, the
// answer budget an unencrypted ping gets, not by `PingWriteTimeout`, which
// bounds local admission. Charged to the write budget, every candidate more
// than about 2.5 s of round trip away (less once contract setup and loss are
// added) failed its evaluation however healthy it was, and a post-quantum
// window on a slow mobile link never formed (connect#220).
package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// newTestDelayLine forwards every message from in to out exactly delay after
// it was read, in order, like one direction of a link with a fixed one-way
// latency and no loss. Messages still queued when ctx ends go back to the
// pool.
func newTestDelayLine(ctx context.Context, delay time.Duration) (chan []byte, chan []byte) {
	in := make(chan []byte)
	out := make(chan []byte)
	type delayedMessage struct {
		deliverTime time.Time
		message     []byte
	}
	delayedMessages := make(chan delayedMessage, 1024)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case message := <-in:
				select {
				case <-ctx.Done():
					MessagePoolReturn(message)
					return
				case delayedMessages <- delayedMessage{deliverTime: time.Now().Add(delay), message: message}:
				}
			}
		}
	}()
	go func() {
		defer func() {
			for {
				select {
				case delayed := <-delayedMessages:
					MessagePoolReturn(delayed.message)
				default:
					return
				}
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case delayed := <-delayedMessages:
				select {
				case <-ctx.Done():
					MessagePoolReturn(delayed.message)
					return
				case <-time.After(time.Until(delayed.deliverTime)):
				}
				select {
				case <-ctx.Done():
					MessagePoolReturn(delayed.message)
					return
				case out <- delayed.message:
				}
			}
		}
	}()
	return in, out
}

// requiredEvaluationFixture is one window evaluating one candidate that
// requires encryption against an sdk-style provider (opportunistic, so it
// answers the handshake), across a link with a fixed one-way delay.
type requiredEvaluationFixture struct {
	window          *multiClientWindow
	providerClient  *Client
	failedPingCount func() int
}

func newRequiredEvaluationFixture(
	t *testing.T,
	ctx context.Context,
	oneWayDelay time.Duration,
	providerEncryptionMode EncryptionMode,
	settings *MultiClientSettings,
) *requiredEvaluationFixture {
	t.Helper()

	// the window client, created by the generator below; each side fetches the
	// other's identity key out of band, as the platform's key api does
	var candidateLock sync.Mutex
	var candidateClient *Client

	providerSettings := DefaultClientSettings()
	providerSettings.Log = NewNoopLogger()
	providerSettings.EncryptionSettings.Mode = providerEncryptionMode
	// no contracts in this fixture, so no companion contract carries the
	// encryption control replies
	providerSettings.EncryptionSettings.EncryptionControlUseCompanion = false
	providerSettings.EncryptionSettings.NewPeerClientPublicKeyFetcher = func(peerId Id) func(context.Context) ([]byte, error) {
		return func(context.Context) ([]byte, error) {
			candidateLock.Lock()
			defer candidateLock.Unlock()
			if candidateClient == nil {
				return nil, nil
			}
			return candidateClient.ClientKeyManager().PublicKey(), nil
		}
	}
	providerClient := NewClient(ctx, NewId(), NewNoContractClientOob(), providerSettings)
	providerLocalNat := NewLocalUserNatWithDefaults(ctx, "required-evaluation-provider")
	provider := NewRemoteUserNatProvider(providerClient, providerLocalNat, DefaultRemoteUserNatProviderSettings())
	t.Cleanup(func() {
		provider.Close()
		providerLocalNat.Close()
		providerClient.Cancel()
	})

	generator := testMultiClientGenerator(providerClient)
	generator.newClient = func(clientCtx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error) {
		clientSettings.Log = NewNoopLogger()
		clientSettings.EncryptionSettings.EncryptionControlUseCompanion = false
		clientSettings.EncryptionSettings.NewPeerClientPublicKeyFetcher = func(peerId Id) func(context.Context) ([]byte, error) {
			return func(context.Context) ([]byte, error) {
				return providerClient.ClientKeyManager().PublicKey(), nil
			}
		}
		client := NewClient(clientCtx, args.ClientId, NewNoContractClientOob(), clientSettings)
		func() {
			candidateLock.Lock()
			defer candidateLock.Unlock()
			candidateClient = client
		}()

		clientSend, providerReceive := newTestDelayLine(clientCtx, oneWayDelay)
		providerSend, clientReceive := newTestDelayLine(clientCtx, oneWayDelay)

		transportSend := NewSendGatewayTransport()
		transportReceive := NewReceiveGatewayTransport()
		client.RouteManager().UpdateTransport(transportSend, []Route{clientSend})
		client.RouteManager().UpdateTransport(transportReceive, []Route{clientReceive})
		client.ContractManager().AddNoContractPeer(providerClient.ClientId())

		providerTransportSend := NewSendClientTransport(DestinationId(args.ClientId))
		providerTransportReceive := NewReceiveGatewayTransport()
		providerClient.RouteManager().UpdateTransport(providerTransportReceive, []Route{providerReceive})
		providerClient.RouteManager().UpdateTransport(providerTransportSend, []Route{providerSend})
		providerClient.ContractManager().AddNoContractPeer(client.ClientId())

		go func() {
			<-client.Done()
			client.RouteManager().RemoveTransport(transportSend)
			client.RouteManager().RemoveTransport(transportReceive)
			providerClient.RouteManager().RemoveTransport(providerTransportReceive)
			providerClient.RouteManager().RemoveTransport(providerTransportSend)
		}()
		return client, nil
	}

	settings.EvaluationPoolMultiple = 1
	settings.Log = NewNoopLogger()
	windowCtx, cancelWindow := context.WithCancel(ctx)
	t.Cleanup(cancelWindow)
	window := &multiClientWindow{
		ctx:                          windowCtx,
		cancel:                       cancelWindow,
		log:                          settings.Log,
		generator:                    &multiClientExpandLifecycleGenerator{generator},
		clientReceivePacketCallback:  func(*multiClientChannel, TransferPath, protocol.ProvideMode, TransportType, *IpPath, []byte) {},
		clientReceivePacketsCallback: nil,
		ingressSecurityPolicy:        DefaultSecurityPolicy(windowCtx),
		windowType:                   WindowTypeQuality,
		settings:                     settings,
		clientChannelArgs:            make(chan *multiClientChannelArgs, 1),
		monitor:                      NewRemoteUserNatMultiClientMonitor(&settings.RemoteUserNatMultiClientMonitorSettings),
		contractStatusCallbacks:      NewCallbackList[*contractStatusCallbackWorker](),
		contractStatsCallbacks:       NewCallbackList[ContractStatsFunction](),
		peerIdentityChangeCallbacks:  NewCallbackList[func()](),
		clients:                      map[Id]*multiClientChannel{},
		generatorMonitor:             NewMonitor(),
		resizeMonitor:                NewMonitor(),
		failures:                     &windowFailureRecorder{},
		pingFailThrottle:             newLogThrottle(settings.EvaluationFailureLogInterval),
		budgetFailThrottle:           newLogThrottle(settings.EvaluationFailureLogInterval),
		// post-quantum mode: every window client runs EncryptionModeRequired
		performanceProfile: &PerformanceProfile{
			WindowType:            WindowTypeQuality,
			PostQuantumEncryption: true,
		},
	}
	evaluationCtx, cancelEvaluation := context.WithCancel(windowCtx)
	t.Cleanup(cancelEvaluation)
	window.evalEpochCtx = evaluationCtx
	window.evalEpochCancel = cancelEvaluation

	clientArgs, err := generator.NewClientArgs()
	if err != nil {
		t.Fatal(err)
	}
	window.clientChannelArgs <- &multiClientChannelArgs{
		MultiClientGeneratorClientArgs: *clientArgs,
		Destination:                    RequireMultiHopId(providerClient.ClientId()),
		DestinationStats:               DestinationStats{},
	}

	return &requiredEvaluationFixture{
		window:         window,
		providerClient: providerClient,
		failedPingCount: func() int {
			return window.failures.counts(time.Now())[windowFailureProvider]
		},
	}
}

// expand runs the single-candidate expansion pass and returns the admitted
// count.
func (self *requiredEvaluationFixture) expand() int {
	return self.window.expand(
		WindowSizeSettings{WindowSizeMin: 1, WindowSizeMax: 1, WindowSizeHardMax: 1},
		0,
		0,
		1,
		1,
		1,
		0,
		windowExpandPassPlan{},
	)
}

// A provider 3 s of round trip away establishes the required session after
// two round trips (6 s), past the 5 s write budget, and answers the ping three
// seconds later, well inside the 30 s ping budget. It must be admitted.
func TestRequiredEvaluationAdmitsAHealthyCandidateWhoseHandshakeOutlastsTheWriteBudget(t *testing.T) {
	// initialize the process-owned pool worker before entering virtual time
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer func() {
			cancel()
			synctest.Wait()
		}()

		settings := DefaultMultiClientSettings()
		oneWayDelay := 1500 * time.Millisecond
		// the cipher cannot exist before two full round trips
		if establishTime := 4 * oneWayDelay; establishTime <= settings.PingWriteTimeout || settings.PingTimeout <= establishTime+2*oneWayDelay {
			t.Fatalf("fixture delay %s does not separate the write budget %s from the ping budget %s", oneWayDelay, settings.PingWriteTimeout, settings.PingTimeout)
		}
		fixture := newRequiredEvaluationFixture(t, ctx, oneWayDelay, EncryptionModeOpportunistic, settings)

		startTime := time.Now()
		admittedCount := fixture.expand()
		elapsed := time.Since(startTime)
		if admittedCount != 1 {
			t.Fatalf("a healthy required candidate was not admitted (admitted=%d, provider failures=%d, after %s): its handshake was cut off by the %s write budget instead of the %s ping budget",
				admittedCount, fixture.failedPingCount(), elapsed, settings.PingWriteTimeout, settings.PingTimeout)
		}
		if fixture.failedPingCount() != 0 {
			t.Fatalf("an admitted candidate recorded %d provider failures", fixture.failedPingCount())
		}
		if elapsed < 6*oneWayDelay || settings.PingTimeout <= elapsed {
			t.Fatalf("admitted after %s, want at or after three round trips (%s) and inside the ping budget %s", elapsed, 6*oneWayDelay, settings.PingTimeout)
		}
	})
}

// The fixture's positive control: on a fast link the same required candidate
// is admitted well inside the write budget, before and after the fix.
func TestRequiredEvaluationAdmitsAHealthyCandidateOnAFastLink(t *testing.T) {
	// initialize the process-owned pool worker before entering virtual time
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer func() {
			cancel()
			synctest.Wait()
		}()

		settings := DefaultMultiClientSettings()
		oneWayDelay := 100 * time.Millisecond
		fixture := newRequiredEvaluationFixture(t, ctx, oneWayDelay, EncryptionModeOpportunistic, settings)

		startTime := time.Now()
		admittedCount := fixture.expand()
		elapsed := time.Since(startTime)
		if admittedCount != 1 || fixture.failedPingCount() != 0 {
			t.Fatalf("a required candidate on a fast link: admitted=%d provider failures=%d after %s", admittedCount, fixture.failedPingCount(), elapsed)
		}
		if elapsed < 6*oneWayDelay || settings.PingWriteTimeout <= elapsed {
			t.Fatalf("admitted after %s, want at or after three round trips (%s) and inside the write budget %s", elapsed, 6*oneWayDelay, settings.PingWriteTimeout)
		}
	})
}

// The other half of the budget: a provider that never answers the handshake
// (a plaintext provider) keeps the candidate no longer than the ping budget,
// exactly as an unanswered unencrypted ping does, and the candidate fails as
// a provider failure without ever being admitted.
func TestRequiredEvaluationFailsAnUnansweredHandshakeAtThePingBudget(t *testing.T) {
	// initialize the process-owned pool worker before entering virtual time
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer func() {
			cancel()
			synctest.Wait()
		}()

		settings := DefaultMultiClientSettings()
		fixture := newRequiredEvaluationFixture(t, ctx, 100*time.Millisecond, EncryptionModeOff, settings)

		startTime := time.Now()
		admittedCount := fixture.expand()
		elapsed := time.Since(startTime)
		if admittedCount != 0 {
			t.Fatalf("a candidate whose provider never answers the handshake was admitted")
		}
		if fixture.failedPingCount() != 1 {
			t.Fatalf("the unanswered candidate recorded %d provider failures, want 1", fixture.failedPingCount())
		}
		if elapsed != settings.PingTimeout {
			t.Fatalf("the unanswered candidate failed after %s, want the ping budget %s", elapsed, settings.PingTimeout)
		}
	})
}

// newRequiredRefusalPingChannel is a bare channel whose continuous ping goes
// through send: a fake standing in for the client's send path, so each test
// decides exactly when the required session refuses a ping and when the
// provider answers one.
func newRequiredRefusalPingChannel(ctx context.Context, cancel context.CancelFunc, send func(ackCallback func(error)) (bool, error)) *multiClientChannel {
	channel := stallTestChannel()
	channel.ctx = ctx
	channel.cancel = cancel
	channel.log = NewNoopLogger()
	channel.args = &multiClientChannelArgs{Destination: RequireMultiHopId(NewId())}
	channel.settings.CPingWriteTimeout = 15 * time.Second
	channel.settings.CPingTimeout = 30 * time.Second
	channel.settings.CPingRestTimeout = 10 * time.Second
	channel.pingSendForTest = func(_ time.Duration, ackCallback func(error)) (bool, error) {
		return send(ackCallback)
	}
	return channel
}

// Until a ping has been answered, a required-encryption refusal means the
// session is still coming up: nothing was sent, and the channel's evaluation
// owns that wait under its own budget. The continuous ping must rest and ping
// again rather than convict the provider. It used to cancel the channel on the
// first refusal, which cut every required candidate's handshake off at
// CPingWriteTimeout, half the evaluation's ping budget.
func TestPingLoopRequiredRefusalBeforeAnAnswerKeepsTheChannel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sendCount := 0
		answered := make(chan struct{})
		channel := newRequiredRefusalPingChannel(ctx, cancel, func(ackCallback func(error)) (bool, error) {
			sendCount += 1
			if sendCount <= 2 {
				// the session is not up yet: refused, unsent
				return false, ErrEncryptionRequiredNotEstablished
			}
			if sendCount == 3 {
				close(answered)
			}
			ackCallback(nil)
			return true, nil
		})

		done := make(chan struct{})
		go func() {
			defer close(done)
			channel.ping()
		}()
		endErr := func() error {
			channel.stateLock.Lock()
			defer channel.stateLock.Unlock()
			return channel.endErr
		}
		select {
		case <-answered:
		case <-done:
			t.Fatalf("the ping loop ended after %d sends: a required-encryption refusal before any answered ping convicted the channel (canceled=%t, err=%v)",
				sendCount, ctx.Err() != nil, endErr())
		}
		if ctx.Err() != nil || endErr() != nil {
			t.Fatalf("a refusal before the first answer convicted the channel (err=%v)", endErr())
		}
		cancel()
		<-done
	})
}

// Once a ping has been answered the session was up, so a later refusal means
// it was lost, and the continuous ping still convicts the channel as before.
func TestPingLoopRequiredRefusalAfterAnAnswerConvicts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sendCount := 0
		channel := newRequiredRefusalPingChannel(ctx, cancel, func(ackCallback func(error)) (bool, error) {
			sendCount += 1
			if sendCount == 1 {
				ackCallback(nil)
				return true, nil
			}
			return false, ErrEncryptionRequiredNotEstablished
		})

		done := make(chan struct{})
		go func() {
			defer close(done)
			channel.ping()
		}()
		<-done
		if sendCount != 2 {
			t.Fatalf("the loop sent %d pings, want the answered one and the refused one", sendCount)
		}
		if ctx.Err() == nil {
			t.Fatal("a refusal after an answered ping did not convict the channel")
		}
	})
}

// A provider whose required handshake takes longer than the continuous
// ping's write budget (four one-way trips of 4 s: 16 s) and answers the
// evaluation ping 8 s later, inside the 30 s ping budget, must be admitted.
func TestRequiredEvaluationAdmitsAHealthyCandidateWhoseHandshakeOutlastsTheContinuousPingWriteBudget(t *testing.T) {
	// initialize the process-owned pool worker before entering virtual time
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer func() {
			cancel()
			synctest.Wait()
		}()

		settings := DefaultMultiClientSettings()
		oneWayDelay := 4 * time.Second
		if establishTime := 4 * oneWayDelay; establishTime <= settings.CPingWriteTimeout || settings.PingTimeout <= establishTime+2*oneWayDelay {
			t.Fatalf("fixture delay %s does not separate the continuous ping write budget %s from the ping budget %s", oneWayDelay, settings.CPingWriteTimeout, settings.PingTimeout)
		}
		fixture := newRequiredEvaluationFixture(t, ctx, oneWayDelay, EncryptionModeOpportunistic, settings)

		startTime := time.Now()
		admittedCount := fixture.expand()
		elapsed := time.Since(startTime)
		if admittedCount != 1 || fixture.failedPingCount() != 0 {
			t.Fatalf("a healthy required candidate was not admitted (admitted=%d, provider failures=%d, after %s): its handshake was cut off before the %s ping budget",
				admittedCount, fixture.failedPingCount(), elapsed, settings.PingTimeout)
		}
		if elapsed < 6*oneWayDelay || settings.PingTimeout <= elapsed {
			t.Fatalf("admitted after %s, want at or after three round trips (%s) and inside the ping budget %s", elapsed, 6*oneWayDelay, settings.PingTimeout)
		}
	})
}
