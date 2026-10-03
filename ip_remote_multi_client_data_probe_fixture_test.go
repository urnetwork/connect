// Joined real-client fixture for fixed-provider application evaluation. The
// safety horizon permits a 35s recovery without changing production budgets.
package connect

import (
	"context"
	"github.com/urnetwork/connect/protocol"
	"sync"
	"testing"
	"time"
)

func newDataOnlyProviderProbeFixture(t *testing.T, configureProvider func(*ClientSettings)) (*multiClientExpandLifecycleFixture, *Client) {
	t.Helper()
	waitCtx, cancelWait := context.WithTimeout(t.Context(), 90*time.Second)
	windowCtx, cancelWindow := context.WithCancel(waitCtx)
	log := newRecordingLogger()

	providerSettings := DefaultClientSettings()
	providerSettings.Log = NewNoopLogger()
	configureProvider(providerSettings)
	providerClient := NewClient(
		waitCtx,
		NewId(),
		NewNoContractClientOob(),
		providerSettings,
	)
	providerLocalNat := NewLocalUserNatWithDefaults(waitCtx, "expand-lifecycle-provider")
	provider := NewRemoteUserNatProvider(
		providerClient,
		providerLocalNat,
		DefaultRemoteUserNatProviderSettings(),
	)
	t.Cleanup(func() {
		provider.Close()
		providerLocalNat.Close()
		providerClient.Cancel()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := providerClient.CloseAndWait(closeCtx); err != nil {
			t.Errorf("join expand-lifecycle provider client: %v", err)
		}
	})

	argsRemoved := make(chan struct{})
	clientRemoved := make(chan struct{})
	var argsRemovedOnce sync.Once
	var clientRemovedOnce sync.Once
	generator := testMultiClientGenerator(providerClient)
	generator.removeClientArgs = func(*MultiClientGeneratorClientArgs) {
		argsRemovedOnce.Do(func() {
			close(argsRemoved)
		})
	}
	originalRemoveClientWithArgs := generator.removeClientWithArgs
	generator.removeClientWithArgs = func(
		client *Client,
		args *MultiClientGeneratorClientArgs,
	) {
		originalRemoveClientWithArgs(client, args)
		clientRemovedOnce.Do(func() {
			close(clientRemoved)
		})
	}
	var generatedClientLock sync.Mutex
	var generatedClient *Client
	originalNewClient := generator.newClient
	generator.newClient = func(
		clientCtx context.Context,
		args *MultiClientGeneratorClientArgs,
		clientSettings *ClientSettings,
	) (*Client, error) {
		client, err := originalNewClient(clientCtx, args, clientSettings)
		generatedClientLock.Lock()
		generatedClient = client
		generatedClientLock.Unlock()
		return client, err
	}
	t.Cleanup(func() {
		generatedClientLock.Lock()
		client := generatedClient
		generatedClientLock.Unlock()
		if client == nil {
			return
		}
		client.Cancel()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := client.CloseAndWait(closeCtx); err != nil {
			t.Errorf("join expand-lifecycle client: %v", err)
		}
	})

	settings := DefaultMultiClientSettings()
	settings.Log = log
	settings.PingWriteTimeout = expandLifecycleWriteTimeout
	settings.PingTimeout = expandLifecyclePingTimeout
	settings.WindowExpandTimeout = expandLifecycleRequestTimeout
	settings.EvaluationPoolMultiple = 1

	pingResultEntered := make(chan struct{})
	releasePingResult := make(chan struct{})
	pingResultDone := make(chan struct{})
	var pingResultEnteredOnce sync.Once
	var pingResultDoneOnce sync.Once
	window := &multiClientWindow{
		ctx:                          windowCtx,
		cancel:                       cancelWindow,
		log:                          log,
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
		pingFailThrottle:             newLogThrottle(evaluationFailureLogInterval),
		budgetFailThrottle:           newLogThrottle(evaluationFailureLogInterval),
		beforeExpandPingResultForTest: func() {
			pingResultEnteredOnce.Do(func() {
				close(pingResultEntered)
			})
			<-releasePingResult
		},
		afterExpandPingResultForTest: func() {
			pingResultDoneOnce.Do(func() {
				close(pingResultDone)
			})
		},
	}
	evaluationCtx, cancelEvaluation := context.WithCancel(windowCtx)
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

	fixture := &multiClientExpandLifecycleFixture{
		waitCtx:           waitCtx,
		cancelWindow:      cancelWindow,
		cancelEvaluation:  cancelEvaluation,
		log:               log,
		window:            window,
		argsRemoved:       argsRemoved,
		clientRemoved:     clientRemoved,
		pingResultEntered: pingResultEntered,
		releasePingResult: releasePingResult,
		pingResultDone:    pingResultDone,
	}
	t.Cleanup(func() {
		fixture.releasePing()
		cancelEvaluation()
		cancelWindow()
		cancelWait()
	})
	return fixture, providerClient
}
