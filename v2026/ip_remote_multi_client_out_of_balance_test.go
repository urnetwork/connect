// What a window shows for an account the platform refuses, which is what the
// desktop tunnels' dead-tunnel failsafe reads (linux TunnelWatchdog.hpp, windows
// Service/TunnelWatchdog.h): the account is told through its contract status, and
// the exits say nothing about the balance.
package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// A session that starts out of balance: every contract is refused, so the initial
// ping never leaves and no provider is admitted. The window has no exit to prove,
// and the refusal arrives as ContractError_InsufficientBalance long before the
// failsafe's shortest window (20 s). The candidate's failure is local, so it does
// not count against the provider.
func TestOutOfBalanceWindowAdmitsNoExit(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newMultiClientExpandLifecycleFixture(t)
		fixture.window.settings.PingTimeout = 2 * time.Second
		fixture.window.settings.WindowExpandTimeout = time.Second
		fixture.window.settings.WindowClientSetupTimeout = 3 * time.Second
		fixture.window.beforeExpandPingResultForTest = nil
		fixture.window.afterExpandPingResultForTest = nil
		generator := fixture.window.generator.(*multiClientExpandLifecycleGenerator).TestMultiClientGenerator
		candidate := <-fixture.window.clientChannelArgs
		destination := candidate.Destination.Tail()
		fixture.window.clientChannelArgs <- candidate

		var statusLock sync.Mutex
		var statuses []*ContractStatus
		statusSub := fixture.window.AddContractStatusCallback(func(status *ContractStatus) {
			statusLock.Lock()
			defer statusLock.Unlock()
			statuses = append(statuses, status)
		})
		defer statusSub()

		created := make(chan *Client, 1)
		oob := &contractErrorOob{errorCount: -1}
		generator.newClient = func(ctx context.Context, args *MultiClientGeneratorClientArgs, settings *ClientSettings) (*Client, error) {
			settings.EncryptionSettings.Mode = EncryptionModeOff
			settings.Log = fixture.log
			// virtual time precedes the default historical enable date
			settings.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Unix(0, 0)
			settings.SendBufferSettings.CreateContractTimeout = time.Second
			settings.SendBufferSettings.CreateContractRetryInterval = 100 * time.Millisecond
			settings.SendBufferSettings.CreateContractRetryMaxInterval = 100 * time.Millisecond
			oob.clientId = args.ClientId
			client := NewClient(ctx, args.ClientId, oob, settings)
			packets := make(chan []byte, 128)
			client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(destination)), []Route{packets})
			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case packet := <-packets:
						MessagePoolReturn(packet)
						t.Error("a frame crossed the carrier without a contract")
					}
				}
			}()
			created <- client
			return client, nil
		}

		done := fixture.start()
		client := <-created
		defer func() {
			client.Cancel()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := client.CloseAndWait(ctx); err != nil {
				t.Errorf("join candidate: %v", err)
			}
		}()
		time.Sleep(time.Second)
		synctest.Wait()

		if got := fixture.result(t, done); got != 0 {
			t.Fatalf("admitted without a contract=%d", got)
		}
		if got := fixture.clientCount(); got != 0 {
			t.Fatalf("clients in the window=%d", got)
		}
		if oob.errorsSent.Load() == 0 {
			t.Fatal("no contract was refused")
		}

		// what the status says
		statusLock.Lock()
		refusals := 0
		for _, status := range statuses {
			if status.Error == nil || *status.Error != protocol.ContractError_InsufficientBalance {
				t.Fatalf("a status other than the refusal: %+v", status)
			}
			refusals += 1
		}
		statusLock.Unlock()
		if refusals == 0 {
			t.Fatal("the refusal never reached the contract status")
		}

		// what the exits and the window say: nothing to prove, and not formed
		multiClient := &RemoteUserNatMultiClient{windows: map[WindowType]*multiClientWindow{WindowTypeQuality: fixture.window}}
		if exits := multiClient.Exits(); len(exits) != 0 {
			t.Fatalf("exits=%d", len(exits))
		}
		windowExpandEvent, providerEvents := fixture.window.monitor.Events()
		if windowExpandEvent.MinSatisfied {
			t.Fatal("the window reads satisfied with nothing admitted")
		}
		for _, providerEvent := range providerEvents {
			if providerEvent.State == ProviderStateAdded {
				t.Fatalf("a provider reads added: %+v", providerEvent)
			}
		}

		// and the failure is the platform's, never the provider's
		counts := fixture.window.failures.counts(time.Now())
		if got := counts[windowFailureProvider]; got != 0 {
			t.Errorf("provider failures=%d", got)
		}
		if got := counts[windowFailurePlatform]; got != 1 {
			t.Errorf("platform failures=%d", got)
		}
	})
}

// A session that runs out while up: the refusal leaves an exit already in the
// window as it was, and a qualification stands until it ages out
// (QualificationMaxAge), so the exit goes on reading proven while nothing can be
// sent through it. The proven count alone says nothing about the balance in
// either direction: the status is the only reading that does.
func TestOutOfBalanceRefusalLeavesAProvenExitAsItWas(t *testing.T) {
	destination := NewId()
	window, client := newContractStatusWindowFixture(destination)
	client.ctx = t.Context()
	window.clients = map[Id]*multiClientChannel{client.ClientId(): client}
	multiClient := &RemoteUserNatMultiClient{windows: map[WindowType]*multiClientWindow{WindowTypeQuality: window}}
	multiClient.qualification = map[MultiHopId]*providerQualification{}
	multiClient.recordProbePass(client.probeDestination())

	refusal := protocol.ContractError_InsufficientBalance
	window.contractStatusFromClient(client, &ContractStatus{
		Key:   ContractKey{Destination: DestinationId(destination)},
		Error: &refusal,
	})

	exits := multiClient.Exits()
	if len(exits) != 1 {
		t.Fatalf("exits=%d", len(exits))
	}
	if !exits[0].Proven {
		t.Fatal("the refusal took the exit's proof away")
	}
	if exits[0].Warning {
		t.Fatal("the refusal warned the exit")
	}

	// time, not the refusal, is what ends the proof
	qualifiedAt := time.Now().Add(-QualificationMaxAge)
	multiClient.stateLock.Lock()
	multiClient.qualification[client.probeDestination()].qualifiedAt = qualifiedAt
	multiClient.stateLock.Unlock()
	if exits := multiClient.Exits(); len(exits) != 1 || exits[0].Proven {
		t.Fatalf("a qualification past its age still reads proven: %+v", exits)
	}
}
