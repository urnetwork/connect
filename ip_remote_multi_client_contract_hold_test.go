// A window reads a contract rate-limit hold on one of its own clients as a
// degraded backend scoped to itself: it does not grow, and its races dial at
// most MultiRaceDegradedClientCount exits, while the process-wide
// degraded-backend gate and every sibling window are untouched. Every identity
// and destination here is synthetic.
package connect

import (
	"context"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// A quality window whose one installed client is `client`, with the real
// resize loop's size math and no candidates to admit, so a pass that grows the
// window publishes its target and waits out an empty expansion. The window
// reads the real process-wide degraded-backend gate.
func newContractHoldTestWindow(t *testing.T, client *Client) *multiClientWindow {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	window := outcomeTestWindow(ctx, newRecordingLogger())
	window.cancel = cancel
	window.generator = &testingEmptyMultiClientGenerator{}
	window.settings.StandingReserve = false
	window.settings.WindowResizeTimeout = 2 * time.Second
	window.settings.WindowExpandTimeout = time.Second
	window.settings.MaxClientLifetime = 2 * time.Hour
	window.clientChannelArgs = make(chan *multiClientChannelArgs)
	window.contractStatusCallbacks = NewCallbackList[*contractStatusCallbackWorker]()
	window.budgetFailThrottle = newLogThrottle(window.settings.EvaluationFailureLogInterval)
	window.flowCountFunc = func(*multiClientChannel) int { return 1 }
	window.clientRemoveCallback = func(*multiClientChannel) { t.Error("the window removed its installed client") }

	channel := stallTestChannel()
	channel.ctx, channel.cancel = context.WithCancel(ctx)
	channel.clientReceiveUnsub = func() {}
	channel.log = NewNoopLogger()
	channel.args = &multiClientChannelArgs{
		MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: client.ClientId()},
		Destination:                    RequireMultiHopId(NewId()),
		DestinationStats:               DestinationStats{IpFamily: IpFamilyDualstack},
	}
	channel.client = client
	channel.firstEventTime = time.Now().Add(-time.Hour)
	channel.effectiveLifetime = 2 * time.Hour
	window.clients[channel.ClientId()] = channel
	return window
}

// Runs the window's resize loop until the returned stop, which joins it.
func runContractHoldTestWindow(window *multiClientWindow) (stop func()) {
	var loops sync.WaitGroup
	loops.Add(1)
	go func() {
		defer loops.Done()
		window.resize()
	}()
	return func() {
		window.cancel()
		loops.Wait()
	}
}

// A 429 holds one window's client. That window keeps its size, one exit,
// while the hold runs, and grows to its full target once it ends; a sibling
// window in the same process, whose client is not held, grows at once. Before,
// the 429 held only the refused client's own requests, and its window kept
// minting clients into the same refusal.
func TestWindowDoesNotGrowWhileOneOfItsClientsIsRateLimited(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		heldOob := &contractRetryOob{
			clientId: NewId(),
			answer:   func(int) contractRetryAnswer { return contractRetryRateLimit },
		}
		heldSettings := contractRetryTestSettings()
		// a hold of 4.375 s
		heldSettings.ContractManagerSettings.createContractRateLimitRandomForTest = func(n int64) int64 { return n / 4 }
		heldClient := newContractRetryTestClient(t, heldOob, heldSettings)
		siblingOob := &contractRetryOob{
			clientId: NewId(),
			answer:   func(int) contractRetryAnswer { return contractRetryGrant },
		}
		siblingClient := newContractRetryTestClient(t, siblingOob, contractRetryTestSettings())

		heldClient.ContractManager().CreateContract(ContractKey{Destination: DestinationId(NewId())}, 0, ByteCount(1024))
		if !heldClient.ContractManager().createContractRateLimitHeld(time.Now()) {
			t.Fatal("the 429 did not hold its client")
		}

		heldWindow := newContractHoldTestWindow(t, heldClient)
		siblingWindow := newContractHoldTestWindow(t, siblingClient)
		stopHeld := runContractHoldTestWindow(heldWindow)
		defer stopHeld()
		stopSibling := runContractHoldTestWindow(siblingWindow)
		defer stopSibling()
		synctest.Wait()

		fullTarget := siblingWindow.monitor.WindowExpandEvent().TargetSize
		if fullTarget <= 1 {
			t.Fatalf("the sibling window asked for %d exits, want it to grow past its one", fullTarget)
		}
		if target := heldWindow.monitor.WindowExpandEvent().TargetSize; target != 1 {
			t.Fatalf("the held window asked for %d exits, want its one while the hold runs", target)
		}
		if isBackendDegraded() {
			t.Fatal("the 429 tripped the process-wide degraded-backend gate")
		}

		// the 4 s pass is inside the hold, the 6 s one after it
		time.Sleep(4 * time.Second)
		synctest.Wait()
		if target := heldWindow.monitor.WindowExpandEvent().TargetSize; target != 1 {
			t.Fatalf("the held window asked for %d exits at 4 s, inside its hold", target)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if target := heldWindow.monitor.WindowExpandEvent().TargetSize; target != fullTarget {
			t.Fatalf("the window asked for %d exits after its hold, want %d", target, fullTarget)
		}
	})
}

// Channels read as held while any of their clients' managers holds, and not
// once the hold ends; a bare channel without a client never does.
func TestContractRateLimitHeldChannels(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		oob := &contractRetryOob{
			clientId: NewId(),
			answer:   func(int) contractRetryAnswer { return contractRetryRateLimit },
		}
		settings := contractRetryTestSettings()
		settings.ContractManagerSettings.createContractRateLimitRandomForTest = func(n int64) int64 { return n / 4 }
		heldClient := newContractRetryTestClient(t, oob, settings)
		freeClient := newContractRetryTestClient(t, &contractRetryOob{
			clientId: NewId(),
			answer:   func(int) contractRetryAnswer { return contractRetryGrant },
		}, contractRetryTestSettings())

		bare := stallTestChannel()
		free := stallTestChannel()
		free.client = freeClient
		held := stallTestChannel()
		held.client = heldClient

		if contractRateLimitHeldChannels([]*multiClientChannel{bare, free, held}, time.Now()) {
			t.Fatal("channels read as held before any refusal")
		}
		heldClient.ContractManager().CreateContract(ContractKey{Destination: DestinationId(NewId())}, 0, ByteCount(1024))
		if !contractRateLimitHeldChannels([]*multiClientChannel{bare, free, held}, time.Now()) {
			t.Fatal("a held client's channel did not read as held")
		}
		if contractRateLimitHeldChannels([]*multiClientChannel{bare, free}, time.Now()) {
			t.Fatal("channels without a held client read as held")
		}
		if contractRateLimitHeldChannels([]*multiClientChannel{held}, time.Now().Add(5*time.Second)) {
			t.Fatal("the channel still read as held after its 4.375 s hold")
		}
	})
}

// The growth gate and the race bound are only worth anything where they are
// consulted. Pin both call sites: the resize gate before expand, and the race
// truncation over the race's own candidates.
func TestContractRateLimitHeldWindowSourceAnchors(t *testing.T) {
	source, err := readSource("ip_remote_multi_client.go")
	if err != nil {
		t.Fatal(err)
	}
	resize, ok := functionBody(source, "func (self *multiClientWindow) resize()")
	if !ok {
		t.Fatal("could not find resize")
	}
	gate := strings.Index(resize, "degraded := self.backendDegraded() || contractRateLimitHeldChannels(self.unorderedClients(), time.Now())")
	if gate < 0 {
		t.Fatal("resize does not hold a window whose client is rate limited")
	}
	if expand := strings.Index(resize, "addedCount = self.expand("); expand < 0 || expand < gate {
		t.Error("the rate-limit gate must apply before expand")
	}
	race, ok := functionBody(source, "func (self *RemoteUserNatMultiClient) sendParsedPacketGroupAttempt(")
	if !ok {
		t.Fatal("could not find sendParsedPacketGroupAttempt")
	}
	if !strings.Contains(race, "self.backendDegraded() || contractRateLimitHeldChannels(orderedClients, time.Now())") {
		t.Error("a race that includes a rate-limited client is not bounded")
	}
}
