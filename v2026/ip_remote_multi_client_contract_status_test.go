package connect

import (
	"errors"
	"testing"

	"github.com/urnetwork/connect/v2026/protocol"
)

func newContractStatusWindowFixture(destination Id) (*multiClientWindow, *multiClientChannel) {
	settings := DefaultMultiClientSettings()
	window := &multiClientWindow{
		contractStatusCallbacks: NewCallbackList[*contractStatusCallbackWorker](),
		resizeMonitor:           NewMonitor(),
	}
	client := &multiClientChannel{
		args: &multiClientChannelArgs{
			Destination: RequireMultiHopId(NewId(), destination),
		},
		settings:     settings,
		eventBuckets: []*multiClientEventBucket{},
		packetStats:  &clientWindowStats{},
	}
	return window, client
}

func contractStatusClientState(client *multiClientChannel) (warning bool, err error) {
	client.stateLock.Lock()
	defer client.stateLock.Unlock()
	return client.warning, client.endErr
}

// A Reliability result is the platform's authoritative statement that the
// selected destination has gone stale. It must poison only the exact channel
// that requested that contract, and wake resize so the normal removal,
// migration, and replacement path runs immediately.
func TestContractReliabilityFailureMarksWindowClientBad(t *testing.T) {
	destination := NewId()
	window, client := newContractStatusWindowFixture(destination)
	wake := window.resizeMonitor.NotifyChannel()
	reliability := protocol.ContractError_Reliability
	manager := &ContractManager{
		client:                          &Client{log: loggerOrDefault(nil)},
		contractStatusCallbacks:         NewCallbackList[*contractStatusCallbackWorker](),
		contractStatusDispatchCallbacks: NewCallbackList[ContractStatusFunction](),
	}
	manager.addContractStatusDispatchCallback(func(status *ContractStatus) {
		window.contractStatusFromClient(client, status)
	})
	frame, err := ToFrame(
		&protocol.CreateContractResult{Error: &reliability},
		DefaultProtocolVersion,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.HandleControlFrame(
		ContractKey{Destination: DestinationId(destination)},
		frame,
	); err != nil {
		t.Fatal(err)
	}

	warning, err := contractStatusClientState(client)
	if !warning {
		t.Fatal("reliability failure did not exclude the channel from new-flow selection")
	}
	if !errors.Is(err, errContractReliability) {
		t.Fatalf("channel error = %v, want contract reliability failure", err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("reliability failure did not wake window resize")
	}
}

// Contract errors that describe the account, authorization, generic setup, or
// a malformed contract are not evidence that this provider route is bad. They
// remain observable through ContractStatus without mutating window health.
func TestOnlyContractReliabilityFailureMarksWindowClientBad(t *testing.T) {
	for _, contractError := range []protocol.ContractError{
		protocol.ContractError_NoPermission,
		protocol.ContractError_InsufficientBalance,
		protocol.ContractError_Setup,
		protocol.ContractError_Trust,
		protocol.ContractError_Invalid,
	} {
		t.Run(contractError.String(), func(t *testing.T) {
			destination := NewId()
			window, client := newContractStatusWindowFixture(destination)
			wake := window.resizeMonitor.NotifyChannel()
			window.contractStatusFromClient(client, &ContractStatus{
				Key:   ContractKey{Destination: DestinationId(destination)},
				Error: &contractError,
			})

			if warning, err := contractStatusClientState(client); warning || err != nil {
				t.Fatalf("%s changed channel health: warning=%t err=%v", contractError, warning, err)
			}
			select {
			case <-wake:
				t.Fatalf("%s woke window resize", contractError)
			default:
			}
		})
	}
}

// Even an explicit Reliability result cannot poison a neighboring exit. The
// result key must name the destination at the tail of the emitting channel.
func TestContractReliabilityFailureIsScopedToItsDestination(t *testing.T) {
	window, client := newContractStatusWindowFixture(NewId())
	reliability := protocol.ContractError_Reliability
	window.contractStatusFromClient(client, &ContractStatus{
		Key:   ContractKey{Destination: DestinationId(NewId())},
		Error: &reliability,
	})

	if warning, err := contractStatusClientState(client); warning || err != nil {
		t.Fatalf("another destination changed channel health: warning=%t err=%v", warning, err)
	}
}
