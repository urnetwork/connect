// A window reads a contract rate-limit hold on one of its own clients as a
// degraded backend scoped to itself: it does not grow, and a race that would
// send to a held client dials at most MultiRaceDegradedClientCount exits, with
// the held ones last, while the process-wide degraded-backend gate and every
// sibling window are untouched. Every identity and destination here is
// synthetic.
package connect

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
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

// The growth gate is only worth anything where it is consulted: before expand
// in resize. The race bound is covered by behaviour
// (TestMultiClientRaceWithAContractRateLimitedClient).
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
}

// A client whose contract requests are held for about an hour: its one
// request was refused as a rate limit, and the fixture control answers before
// CreateContract returns.
func contractHeldTestClient(t *testing.T) *Client {
	t.Helper()
	settings := contractRetryTestSettings()
	settings.ContractManagerSettings.CreateContractRateLimitBackoffMin = time.Hour
	settings.ContractManagerSettings.CreateContractRateLimitBackoffMax = time.Hour
	client := newContractRetryTestClient(t, &contractRetryOob{
		clientId: NewId(),
		answer:   func(int) contractRetryAnswer { return contractRetryRateLimit },
	}, settings)
	client.ContractManager().CreateContract(ContractKey{Destination: DestinationId(NewId())}, 0, ByteCount(1024))
	if !client.ContractManager().createContractRateLimitHeld(time.Now()) {
		t.Fatal("the client's contract requests are not held after a refusal")
	}
	return client
}

func TestContractRateLimitHeldLast(t *testing.T) {
	heldClient := contractHeldTestClient(t)
	channels := make([]*multiClientChannel, 4)
	for i := range channels {
		channels[i] = stallTestChannel()
	}
	channels[0].client = heldClient
	channels[2].client = heldClient
	original := slices.Clone(channels)
	now := time.Now()

	sorted := contractRateLimitHeldLast(channels, now)
	if want := []*multiClientChannel{original[1], original[3], original[0], original[2]}; !slices.Equal(sorted, want) {
		t.Errorf("held channels not sorted last in their own order: got %v, want %v", channelIndexes(original, sorted), channelIndexes(original, want))
	}
	if !slices.Equal(channels, original) {
		t.Errorf("the caller's list was reordered: %v", channelIndexes(original, channels))
	}

	unheld := []*multiClientChannel{original[1], original[3]}
	if got := contractRateLimitHeldLast(unheld, now); &got[0] != &unheld[0] || len(got) != len(unheld) {
		t.Error("a list without a held channel was copied")
	}
}

func TestContractRateLimitHeldRaced(t *testing.T) {
	heldClient := contractHeldTestClient(t)
	channel := func(held bool, benched bool) *multiClientChannel {
		channel := stallTestChannel()
		if held {
			channel.client = heldClient
		}
		channel.warning = benched
		return channel
	}
	cases := []struct {
		name       string
		channels   []*multiClientChannel
		configured int
		want       bool
	}{
		{"none held", []*multiClientChannel{channel(false, false), channel(false, false), channel(false, false)}, 0, false},
		{"held within the unbounded width", []*multiClientChannel{channel(false, false), channel(false, false), channel(true, false)}, 0, true},
		{"held within the configured width", []*multiClientChannel{channel(false, false), channel(false, false), channel(true, false)}, 3, true},
		{"held past the configured width", []*multiClientChannel{channel(false, false), channel(false, false), channel(true, false)}, 2, false},
		{"benched field", []*multiClientChannel{channel(false, true), channel(false, true), channel(true, true)}, 0, false},
		{"one exit not benched", []*multiClientChannel{channel(false, true), channel(false, false), channel(true, true)}, 0, true},
	}
	for _, c := range cases {
		if got := contractRateLimitHeldRaced(c.channels, c.configured, time.Now()); got != c.want {
			t.Errorf("%s: contractRateLimitHeldRaced = %t, want %t", c.name, got, c.want)
		}
	}
}

// End to end through sendPacketGroup, a cold-start SYN raced to four exits
// with the backend healthy: a held exit is sent the packet last, the race is
// cut to MultiRaceDegradedClientCount (2) only when a held exit would be
// raced, and a field of benched exits, the window's last resort, is raced at
// full width.
func TestMultiClientRaceWithAContractRateLimitedClient(t *testing.T) {
	t.Run("held exit races last in a cut race", func(t *testing.T) {
		testMultiClientRaceWithHeldClient(t, 0, false, []int{1, 2})
	})
	t.Run("held exit past the configured width", func(t *testing.T) {
		testMultiClientRaceWithHeldClient(t, 3, false, []int{1, 2, 3})
	})
	t.Run("benched last-resort field", func(t *testing.T) {
		testMultiClientRaceWithHeldClient(t, 0, true, []int{0, 1, 2, 3})
	})
}

// Races a cold-start SYN to four stalled exits, the first one held and each
// benched if `benched`, with MultiRaceClientCount `configured`; requires the
// race to register, and send to, exactly the candidates `want`.
func testMultiClientRaceWithHeldClient(t *testing.T, configured int, benched bool, want []int) {
	t.Helper()
	parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
	defer closeParent()
	parent.settings.MultiRaceSetOnNoResponseTimeout = 50 * time.Millisecond
	parent.settings.MultiRaceClientCount = configured
	parent.backendDegradedForTest = func() bool { return false }

	tcpPath := udpTestPath(4)
	tcpPath.Protocol = IpProtocolTcp
	packet := MessagePoolCopy(ipOosTcpPacketSequence(tcpPath, tcpFlagSyn, 1000, nil))
	group := requireGroupTestPacketGroup(t, packet)
	parent.ip4PathUpdates = map[Ip4Path]*multiClientChannelUpdate{
		tcpPath.ToIp4Path(): update,
	}

	const candidateCount = 4
	sent := make(chan *multiClientChannel, candidateCount)
	candidates := make([]*multiClientChannel, candidateCount)
	for i := range candidates {
		client := groupTestStalledChannel(parent.settings.ProtocolVersion)
		client.ctx = parent.ctx
		client.settings = parent.settings
		client.warning = benched
		client.sendGroupForTest = func(group *parsedPacketGroup, timeout time.Duration, ack bool) (bool, error) {
			sent <- client
			for packetIndex := range group.packets {
				MessagePoolReturn(group.packets[packetIndex].packet)
			}
			return true, nil
		}
		candidates[i] = client
	}
	candidates[0].client = contractHeldTestClient(t)
	parent.groupRaceCandidatesForTest = func(group *parsedPacketGroup) []*multiClientChannel {
		return candidates
	}

	result := make(chan bool, 1)
	go func() {
		result <- parent.sendPacketGroup(SourceId(NewId()), protocol.ProvideMode_Network, group, time.Second)
	}()

	// every racer is registered before the first SendGroup call
	var first *multiClientChannel
	select {
	case first = <-sent:
	case <-time.After(30 * time.Second):
		t.Fatal("no race candidate was sent the packet")
	}
	var registered []*multiClientChannel
	func() {
		update.stateLock.Lock()
		defer update.stateLock.Unlock()
		if update.race != nil {
			for client := range update.race.clientStates {
				registered = append(registered, client)
			}
		}
	}()

	select {
	case <-result:
	case <-time.After(30 * time.Second):
		t.Fatal("race did not complete")
	}
	sentTo := []*multiClientChannel{first}
	for done := false; !done; {
		select {
		case client := <-sent:
			sentTo = append(sentTo, client)
		default:
			done = true
		}
	}

	if got := sortedChannelIndexes(candidates, registered); !slices.Equal(got, want) {
		t.Errorf("race registered candidates %v, want %v", got, want)
	}
	if got := sortedChannelIndexes(candidates, sentTo); !slices.Equal(got, want) {
		t.Errorf("packet sent to candidates %v, want %v", got, want)
	}
}

// The index in `channels` of each of `of`, -1 for one not in it.
func channelIndexes(channels []*multiClientChannel, of []*multiClientChannel) []int {
	indexes := make([]int, len(of))
	for i, channel := range of {
		indexes[i] = slices.Index(channels, channel)
	}
	return indexes
}

func sortedChannelIndexes(channels []*multiClientChannel, of []*multiClientChannel) []int {
	indexes := channelIndexes(channels, of)
	slices.Sort(indexes)
	return indexes
}
