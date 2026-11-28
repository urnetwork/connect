// A window client's companion lane toward its own exit is answered no permission
// when the exit registered no stream provide key, as a provider built before
// the 2026-07 consolidation did: it registered only its public and network
// modes. That answer repeats for as long as the pair lives, and each passive
// p2p generation, network quality hint or subprotocol reply opened a new send
// sequence that asked about 8 times in its 30 s budget, for nothing.
package connect

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// A companion refusal test's settings: fixed retry schedule, a hold of exactly
// its backoff step.
func companionRefusalTestSettings() *ClientSettings {
	settings := contractRetryTestSettings()
	settings.SendBufferSettings.PrewarmOpeningContract = false
	settings.SendBufferSettings.CreateContractRetryJitter = 0
	settings.ContractManagerSettings.companionRefusalRandomForTest = func(n int64) int64 { return n / 2 }
	return settings
}

// Queues one companion lane message for `destinationId`, as a passive p2p signal
// does when no reply key is bound, and returns where its ack lands.
func sendCompanionRefusalMessage(t *testing.T, client *Client, destinationId Id) chan error {
	t.Helper()
	frame, err := ToFrame(&protocol.SimpleMessage{Content: "companion lane"}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	ackErrs := make(chan error, 1)
	ack := func(err error) {
		select {
		case ackErrs <- err:
		default:
		}
	}
	if !client.SendWithTimeout(frame, destinationId, ack, -1, CompanionContract()) {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatal("the send must enqueue")
	}
	return ackErrs
}

// The answered exit keeps refusing. The first five requests, over 12 s, leave a
// registration that is still committing its time. The fifth starts a 30 s hold,
// so the sequence's retries at 17, 22 and 27 s ask nothing. The next sequence,
// opened as the first gives up, is held until 42 s and its refusal there doubles
// the hold. Before, the two sequences asked 16 times in the same minute, each
// answered no permission.
func TestCompanionNoPermissionHoldsRequests(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		oob := &contractRetryOob{
			clientId: NewId(),
			answer:   func(int) contractRetryAnswer { return contractRetryNoPermission },
		}
		client := newContractRetryTestClient(t, oob, companionRefusalTestSettings())

		destinationId := NewId()
		if err := <-sendCompanionRefusalMessage(t, client, destinationId); err == nil {
			t.Fatal("a refused acquisition must fail the message")
		}
		requireContractRetryOffsets(t, "refused companion sequence", oob.requestOffsets(destinationId, start), []time.Duration{
			0,
			1 * time.Second,
			3 * time.Second,
			7 * time.Second,
			12 * time.Second,
		})

		if err := <-sendCompanionRefusalMessage(t, client, destinationId); err == nil {
			t.Fatal("a held acquisition must fail the message")
		}
		requireContractRetryOffsets(t, "held companion sequence", oob.requestOffsets(destinationId, start), []time.Duration{
			0,
			1 * time.Second,
			3 * time.Second,
			7 * time.Second,
			12 * time.Second,
			42 * time.Second,
		})
		if isBackendDegraded() {
			t.Fatal("a no permission answer tripped the process-wide degraded-backend gate")
		}
	})
}

// The hold covers the refused destination's companion lane and nothing else: the
// data path to the same exit and the companion lane to another destination keep
// asking on their own schedule.
func TestCompanionNoPermissionHoldLeavesOtherRequests(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		refusedId := NewId()
		otherId := NewId()
		oob := &contractRetryOob{
			clientId: NewId(),
			answer:   func(int) contractRetryAnswer { return contractRetryNoPermission },
		}
		client := newContractRetryTestClient(t, oob, companionRefusalTestSettings())

		if err := <-sendCompanionRefusalMessage(t, client, refusedId); err == nil {
			t.Fatal("a refused acquisition must fail the message")
		}
		heldRequests := oob.requestOffsets(refusedId, start)
		// inside the hold, which runs to 42 s
		if !client.ContractManager().companionContractHeld(refusedId, time.Now()) {
			t.Fatal("five refusals in a row must hold the destination's companion requests")
		}

		sendContractRetryMessage(t, client, refusedId)
		sendCompanionRefusalMessage(t, client, otherId)
		synctest.Wait()
		if count := len(oob.requestOffsets(refusedId, start)); count != len(heldRequests)+1 {
			t.Fatalf("the data path's request to the held exit: %d requests, want %d", count, len(heldRequests)+1)
		}
		if count := len(oob.requestOffsets(otherId, start)); count != 1 {
			t.Fatalf("the companion request to another destination: %d requests, want 1", count)
		}
	})
}

// A same-network peer can answer no permission while its provide registration
// commits, and the retries are what carry the sender over it. Refused three
// times and then granted, the sequence takes its contract on the schedule it
// always had, and no hold follows.
func TestCompanionNoPermissionRaceResolvesWithoutHold(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		oob := &contractRetryOob{
			clientId: NewId(),
			answer: func(index int) contractRetryAnswer {
				if index < 3 {
					return contractRetryNoPermission
				}
				return contractRetryGrant
			},
		}
		settings := companionRefusalTestSettings()
		client := newContractRetryTestClient(t, oob, settings)

		destinationId := NewId()
		sendCompanionRefusalMessage(t, client, destinationId)
		time.Sleep(10 * time.Second)
		synctest.Wait()
		requireContractRetryOffsets(t, "racing companion sequence", oob.requestOffsets(destinationId, start), []time.Duration{
			0,
			1 * time.Second,
			3 * time.Second,
			7 * time.Second,
		})
		if client.ContractManager().companionContractHeld(destinationId, time.Now()) {
			t.Fatal("a granted destination must not be held")
		}
		// the grant ended the streak, so the next refusals start from the first
		manager := client.ContractManager()
		now := time.Now()
		for i := 1; i < settings.ContractManagerSettings.CompanionRefusalHoldAfter; i += 1 {
			manager.noteCompanionContractRefused(destinationId, now)
			if manager.companionContractHeld(destinationId, now) {
				t.Fatalf("held after %d refusals since the grant", i)
			}
		}
	})
}

// Each hold doubles up to the maximum, a refusal inside a hold answers a request
// sent before it and steps nothing, and a grant or a quiet spell past the
// longest hold starts the streak over.
func TestCompanionRefusalHoldBacksOff(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		oob := &contractRetryOob{
			clientId: NewId(),
			answer:   func(int) contractRetryAnswer { return contractRetryNoPermission },
		}
		settings := companionRefusalTestSettings()
		contractManagerSettings := settings.ContractManagerSettings
		contractManagerSettings.CompanionRefusalHoldAfter = 2
		contractManagerSettings.CompanionRefusalHoldMin = 30 * time.Second
		contractManagerSettings.CompanionRefusalHoldMax = 2 * time.Minute
		client := newContractRetryTestClient(t, oob, settings)
		manager := client.ContractManager()

		destinationId := NewId()
		now := time.Now()
		if hold := manager.noteCompanionContractRefused(destinationId, now); hold != 0 {
			t.Fatalf("the first refusal held %s", hold)
		}
		for _, want := range []time.Duration{30 * time.Second, 60 * time.Second, 2 * time.Minute, 2 * time.Minute} {
			if hold := manager.noteCompanionContractRefused(destinationId, now); hold != want {
				t.Fatalf("hold %s, want %s", hold, want)
			}
			if !manager.companionContractHeld(destinationId, now) {
				t.Fatal("not held during the hold")
			}
			// an answer to a request sent before the hold steps nothing
			if hold := manager.noteCompanionContractRefused(destinationId, now.Add(want/2)); hold != 0 {
				t.Fatalf("a refusal inside the hold started one of %s", hold)
			}
			now = now.Add(want)
			if manager.companionContractHeld(destinationId, now) {
				t.Fatal("still held at the end of the hold")
			}
		}

		// quiet past the longest hold: the streak starts over
		now = now.Add(2*time.Minute + time.Second)
		if hold := manager.noteCompanionContractRefused(destinationId, now); hold != 0 {
			t.Fatalf("the first refusal after a quiet spell held %s", hold)
		}

		// a grant ends the streak at once
		if hold := manager.noteCompanionContractRefused(destinationId, now); hold != 30*time.Second {
			t.Fatalf("hold %s, want the minimum", hold)
		}
		manager.clearCompanionContractRefused(destinationId)
		if manager.companionContractHeld(destinationId, now) {
			t.Fatal("still held after a grant")
		}
		if hold := manager.noteCompanionContractRefused(destinationId, now); hold != 0 {
			t.Fatalf("the first refusal after a grant held %s", hold)
		}
	})
}

// A client remembers a bounded number of refused destinations, dropping the one
// whose hold ended first.
func TestCompanionRefusalMemoryIsBounded(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		oob := &contractRetryOob{
			clientId: NewId(),
			answer:   func(int) contractRetryAnswer { return contractRetryNoPermission },
		}
		settings := companionRefusalTestSettings()
		contractManagerSettings := settings.ContractManagerSettings
		contractManagerSettings.CompanionRefusalHoldAfter = 1
		contractManagerSettings.CompanionRefusalMaxDestinations = 2
		client := newContractRetryTestClient(t, oob, settings)
		manager := client.ContractManager()

		firstId, secondId, thirdId := NewId(), NewId(), NewId()
		now := time.Now()
		manager.noteCompanionContractRefused(firstId, now)
		manager.noteCompanionContractRefused(secondId, now.Add(time.Second))
		manager.noteCompanionContractRefused(thirdId, now.Add(2*time.Second))
		if manager.companionContractHeld(firstId, now.Add(2*time.Second)) {
			t.Fatal("the oldest hold was not dropped at the bound")
		}
		if !manager.companionContractHeld(secondId, now.Add(2*time.Second)) || !manager.companionContractHeld(thirdId, now.Add(2*time.Second)) {
			t.Fatal("a newer hold was dropped at the bound")
		}
	})
}

// A non-positive minimum turns the hold off.
func TestCompanionRefusalHoldDisabled(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		oob := &contractRetryOob{
			clientId: NewId(),
			answer:   func(int) contractRetryAnswer { return contractRetryNoPermission },
		}
		settings := companionRefusalTestSettings()
		settings.ContractManagerSettings.CompanionRefusalHoldMin = 0
		client := newContractRetryTestClient(t, oob, settings)

		destinationId := NewId()
		if err := <-sendCompanionRefusalMessage(t, client, destinationId); err == nil {
			t.Fatal("a refused acquisition must fail the message")
		}
		requireContractRetryOffsets(t, "unheld companion sequence", oob.requestOffsets(destinationId, start), []time.Duration{
			0,
			1 * time.Second,
			3 * time.Second,
			7 * time.Second,
			12 * time.Second,
			17 * time.Second,
			22 * time.Second,
			27 * time.Second,
		})
	})
}
