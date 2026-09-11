// This file pins expansion-pass ownership across delayed initial-ping
// callbacks. A returned pass cannot add a client during a later resize pass.
package connect

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func TestEvaluationBudgetDeadlineOwnershipExcludesLifecycleCancellation(t *testing.T) {
	windowCtx, cancelWindow := context.WithCancel(context.Background())
	evaluationCtx, cancelEvaluation := context.WithCancel(windowCtx)

	if !evaluationBudgetDeadlineOwned(true, windowCtx, evaluationCtx) {
		t.Fatal("a natural live-window deadline lost ownership")
	}
	if evaluationBudgetDeadlineOwned(false, windowCtx, evaluationCtx) {
		t.Fatal("an ordinary pass return claimed deadline ownership")
	}

	cancelEvaluation()
	if evaluationBudgetDeadlineOwned(true, windowCtx, evaluationCtx) {
		t.Fatal("an evaluation-epoch rebuild became a provider deadline")
	}

	retiredWindowCtx, retireWindow := context.WithCancel(context.Background())
	retiredEvaluationCtx, cancelRetiredEvaluation := context.WithCancel(retiredWindowCtx)
	defer cancelRetiredEvaluation()
	retireWindow()
	if evaluationBudgetDeadlineOwned(true, retiredWindowCtx, retiredEvaluationCtx) {
		t.Fatal("window retirement became a provider deadline")
	}

	cancelWindow()
}

// A ping result held across the natural pass deadline is canceled and its
// constructed channel returns the generator args through RemoveClientWithArgs.
// The deadline must also contribute exactly one provider failure and emit one
// identity-free budget diagnostic. It must not call RemoveClientArgs: that
// would revoke the derived identity before the channel's final cleanup controls
// finish. Before the pass-boundary fix, releasing this callback installed one
// client from the ended pass; before the budget-attribution fix, cleanup hid the
// failure because the 15-second pass ended before the 30-second ping timeout.
func TestMultiClientExpandAccountsForPingAtPassDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	log := newRecordingLogger()

	providerSettings := DefaultClientSettings()
	providerSettings.Log = NewNoopLogger()
	providerClient := NewClient(
		ctx,
		NewId(),
		NewNoContractClientOob(),
		providerSettings,
	)
	providerLocalNat := NewLocalUserNatWithDefaults(ctx, "expand-pass-provider")
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
			t.Errorf("join expand-pass provider client: %v", err)
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
			t.Errorf("join ended expand-pass client: %v", err)
		}
	})
	settings := DefaultMultiClientSettings()
	settings.Log = log
	settings.PingWriteTimeout = 5 * time.Second
	settings.PingTimeout = 30 * time.Second
	settings.WindowExpandTimeout = 15 * time.Second

	pingResultEntered := make(chan struct{})
	releasePingResult := make(chan struct{})
	pingResultDone := make(chan struct{})
	expireExpandPass := make(chan struct{})
	var pingResultEnteredOnce sync.Once
	var pingResultDoneOnce sync.Once
	window := &multiClientWindow{
		ctx:                          ctx,
		cancel:                       cancel,
		log:                          log,
		generator:                    generator,
		clientReceivePacketCallback:  func(*multiClientChannel, TransferPath, protocol.ProvideMode, TransportType, *IpPath, []byte) {},
		clientReceivePacketsCallback: nil,
		ingressSecurityPolicy:        DefaultSecurityPolicy(ctx),
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
		expireExpandPassForTest: expireExpandPass,
	}
	clientArgs, err := generator.NewClientArgs()
	if err != nil {
		t.Fatal(err)
	}
	window.clientChannelArgs <- &multiClientChannelArgs{
		MultiClientGeneratorClientArgs: *clientArgs,
		Destination:                    RequireMultiHopId(providerClient.ClientId()),
		DestinationStats:               DestinationStats{},
	}

	expandDone := make(chan int, 1)
	go func() {
		expandDone <- window.expand(
			WindowSizeSettings{WindowSizeMin: 1, WindowSizeMax: 1, WindowSizeHardMax: 1},
			0,
			0,
			1,
			1,
			1,
			0,
		)
	}()

	select {
	case <-pingResultEntered:
	case <-ctx.Done():
		t.Fatalf("wait for held initial-ping result: %v", ctx.Err())
	}
	close(expireExpandPass)
	select {
	case admittedCount := <-expandDone:
		if admittedCount != 0 {
			t.Fatalf("ended expand pass reported %d admissions", admittedCount)
		}
	case <-ctx.Done():
		t.Fatalf("finish expand pass: %v", ctx.Err())
	}
	if got := window.failures.counts(time.Now())[windowFailureProvider]; got != 1 {
		t.Fatalf("provider failures at natural pass deadline=%d, want 1", got)
	}
	budgetLines := log.linesWith("event=evaluation_budget_exhausted")
	if len(budgetLines) != 1 {
		t.Fatalf("budget-expiry lines=%d, want 1: %v", len(budgetLines), budgetLines)
	}
	if line := budgetLines[0]; !strings.Contains(line, "window=quality") ||
		!strings.Contains(line, "candidates=1") ||
		!strings.Contains(line, "effective_min=") ||
		!strings.Contains(line, "observed_max=") ||
		!strings.Contains(line, "ping_timeout=30000") ||
		!strings.Contains(line, "expand_timeout=15000") ||
		strings.Contains(line, " exit=") ||
		strings.Contains(line, " client=") {
		t.Fatalf("budget-expiry diagnostic is incomplete or identity-bearing: %q", line)
	}
	select {
	case <-argsRemoved:
		t.Fatal("ended expand pass bypassed constructed-channel cleanup with RemoveClientArgs")
	default:
	}
	close(releasePingResult)
	select {
	case <-pingResultDone:
	case <-ctx.Done():
		t.Fatalf("join delayed initial-ping result: %v", ctx.Err())
	}
	select {
	case <-clientRemoved:
	case <-ctx.Done():
		t.Fatalf("join ended expand-pass generator cleanup: %v", ctx.Err())
	}
	select {
	case <-argsRemoved:
		t.Fatal("constructed client args were removed twice")
	default:
	}
	if got := window.failures.counts(time.Now())[windowFailureProvider]; got != 1 {
		t.Fatalf("provider failures after delayed callback=%d, want exactly 1", got)
	}

	window.stateLock.Lock()
	clientCount := len(window.clients)
	window.stateLock.Unlock()
	if clientCount != 0 {
		t.Fatalf("delayed result installed %d clients after its expand pass ended", clientCount)
	}
}
