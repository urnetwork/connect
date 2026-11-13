package connect

// Tests that a selected window's race field and the limited window's merge in
// one order by the weighted metrics (owner, 2026-11-13: "the windows need to
// merge in order based on the weighted metrics"; server FP2-PROTOCOL.md
// §7.5): the order of every race field, the bounded race through the send
// path, held clients still last, udp fields without a limited exit, and the
// window's own order kept when there is nothing to merge.
//
// The weighted shuffles are pinned by raceOrderTestRandom. Every exit's
// weight is 1 + its estimated rate (a fixture exit has no traffic), and the
// rates are chosen so each weight exceeds the sum of all smaller ones; the
// pinned draws then pick the heaviest exit left every time, whatever order
// the window maps hand the exits over in, and every order below is exactly
// descending weight.

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A scripted random source for the race order's weighted shuffles
// (MultiClientSettings.raceOrderRandomForTest): no pre-shuffle, and every draw
// at the middle of the remaining weight. It implements both spellings of the
// integer draw, so it serves the source interface before and after its
// math/rand/v2 rename.
type raceOrderTestRandom struct{}

func (raceOrderTestRandom) Float32() float32 {
	return 0.5
}

func (raceOrderTestRandom) Intn(n int) int {
	return 0
}

func (raceOrderTestRandom) IntN(n int) int {
	return 0
}

func (raceOrderTestRandom) Shuffle(n int, swap func(i int, j int)) {
}

// The estimated rates, in bytes per second, of the four exits: the race
// weights are each 1 more. A heavy tcp exit, then a heavy dualproto one, a
// light tcp one and a light dualproto one.
const (
	raceMergeTestHeavyLimitedRate ByteCount = 1_000_000
	raceMergeTestHeavyNormalRate  ByteCount = 100_000
	raceMergeTestLightLimitedRate ByteCount = 10_000
	raceMergeTestLightNormalRate  ByteCount = 1_000
)

// A quality window and, when asked for, the limited window, under a parent
// whose race order is pinned.
type raceMergeTestFixture struct {
	ctx      context.Context
	settings *MultiClientSettings
	parent   *RemoteUserNatMultiClient
	quality  *multiClientWindow
	limited  *multiClientWindow
	names    map[*multiClientChannel]string
}

// The fixture over a bare capability parent.
func newRaceMergeTestFixture(t *testing.T, withLimited bool) *raceMergeTestFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	settings := DefaultMultiClientSettings()
	settings.raceOrderRandomForTest = raceOrderTestRandom{}
	generator := newCapabilityTestGenerator(true, nil)
	windows := map[WindowType]*multiClientWindow{}
	parent := capabilityTestParent(ctx, cancel, generator, settings, windows)
	fixture := &raceMergeTestFixture{
		ctx:      ctx,
		settings: settings,
		parent:   parent,
		names:    map[*multiClientChannel]string{},
	}
	fixture.addWindows(generator, withLimited)
	return fixture
}

// Builds the windows into the parent's map.
func (self *raceMergeTestFixture) addWindows(generator MultiClientGenerator, withLimited bool) {
	self.quality = capabilityTestWindow(self.ctx, generator, self.settings, WindowTypeQuality)
	self.parent.windows[WindowTypeQuality] = self.quality
	if withLimited {
		self.limited = capabilityTestWindow(self.ctx, generator, self.settings, WindowTypeLimited)
		self.parent.windows[WindowTypeLimited] = self.limited
	}
}

// Adds an exit to the window with the labels and the estimated rate.
func (self *raceMergeTestFixture) add(t *testing.T, window *multiClientWindow, name string, capabilities ProviderCapabilities, rate ByteCount) *multiClientChannel {
	t.Helper()
	client := capabilityTestChannel(t, self.ctx, self.settings, window.windowType, capabilities)
	client.args.DestinationStats.EstimatedBytesPerSecond = rate
	window.stateLock.Lock()
	window.clients[client.ClientId()] = client
	window.stateLock.Unlock()
	self.names[client] = name
	return client
}

// The standard four exits: in quality, normal (A, heavy) and light (B); in
// the limited window, limited (D, heaviest) and slight (C). Without a
// limited window only the quality window's two.
func (self *raceMergeTestFixture) addExits(t *testing.T) {
	t.Helper()
	self.add(t, self.quality, "normal", capabilityClassA, raceMergeTestHeavyNormalRate)
	self.add(t, self.quality, "light", capabilityClassB, raceMergeTestLightNormalRate)
	if self.limited != nil {
		self.add(t, self.limited, "limited", capabilityClassD, raceMergeTestHeavyLimitedRate)
		self.add(t, self.limited, "slight", capabilityClassC, raceMergeTestLightLimitedRate)
	}
}

// The field's exits by name, in order.
func (self *raceMergeTestFixture) order(field []*multiClientChannel) string {
	names := make([]string, 0, len(field))
	for _, client := range field {
		names = append(names, self.names[client])
	}
	return strings.Join(names, " ")
}

// A tcp flow's race field is one order over both windows by weight: the
// heaviest exit, a tcp-only one from the limited window, comes first, ahead of
// the quality window's dualproto exits, and the light limited exit falls
// between them by its weight. The same holds for the cross-tier and the
// last-resort fields. Before, every limited exit followed the window's.
func TestRaceFieldOrdersTheWindowsByTheirWeights(t *testing.T) {
	fixture := newRaceMergeTestFixture(t, true)
	fixture.addExits(t)
	const want = "limited normal slight light"
	for _, need := range []flowNeed{
		{ipVersion: 4, ipProtocol: IpProtocolTcp},
		{ipVersion: 4, ipProtocol: IpProtocolIcmp},
	} {
		var offerChanged, limitedOfferChanged <-chan struct{}
		if got := fixture.order(fixture.parent.raceField(fixture.quality, need, &offerChanged, &limitedOfferChanged)); got != want {
			t.Errorf("protocol %d: race field %q, want %q", need.ipProtocol, got, want)
		}
		if got := fixture.order(fixture.parent.crossTierRaceField(fixture.quality, need)); got != want {
			t.Errorf("protocol %d: cross-tier field %q, want %q", need.ipProtocol, got, want)
		}
		if got := fixture.order(fixture.parent.lastResortRaceField(fixture.quality, need)); got != want {
			t.Errorf("protocol %d: last-resort field %q, want %q", need.ipProtocol, got, want)
		}
	}
	// a v6 tcp flow: the dualstack exits of both windows, by weight
	var offerChanged, limitedOfferChanged <-chan struct{}
	if got := fixture.order(fixture.parent.raceField(fixture.quality, flowNeed{ipVersion: 6, ipProtocol: IpProtocolTcp}, &offerChanged, &limitedOfferChanged)); got != "normal slight" {
		t.Errorf("v6 tcp race field %q, want %q", got, "normal slight")
	}
}

// A udp flow's fields never hold a limited exit, however heavy: the limited
// window's exits are tcp. The quality window's exits keep its own order.
func TestUdpRaceFieldsNeverHoldALimitedExit(t *testing.T) {
	fixture := newRaceMergeTestFixture(t, true)
	fixture.addExits(t)
	for _, need := range []flowNeed{
		{ipVersion: 4, ipProtocol: IpProtocolUdp},
		{ipVersion: 6, ipProtocol: IpProtocolUdp},
	} {
		var offerChanged, limitedOfferChanged <-chan struct{}
		fields := map[string][]*multiClientChannel{
			"race":        fixture.parent.raceField(fixture.quality, need, &offerChanged, &limitedOfferChanged),
			"cross-tier":  fixture.parent.crossTierRaceField(fixture.quality, need),
			"last-resort": fixture.parent.lastResortRaceField(fixture.quality, need),
		}
		for kind, field := range fields {
			if len(field) == 0 {
				t.Errorf("v%d udp: the %s field is empty", need.ipVersion, kind)
			}
			for _, client := range field {
				if client.windowType() == WindowTypeLimited {
					t.Errorf("v%d udp: the %s field %q holds a limited exit", need.ipVersion, kind, fixture.order(field))
				}
			}
		}
		if need.ipVersion == 4 {
			if got := fixture.order(fields["race"]); got != "normal light" {
				t.Errorf("v4 udp race field %q, want the quality window's own order", got)
			}
		}
	}
}

// With no limited exit to merge the field is exactly the window's: no limited
// window at all, or one with nothing for the flow. The merge hands back an
// empty part's partner as it is, never a copy or a new order.
func TestRaceFieldWithoutALimitedExitIsTheWindowsOwn(t *testing.T) {
	tcp := flowNeed{ipVersion: 4, ipProtocol: IpProtocolTcp}

	fixture := newRaceMergeTestFixture(t, false)
	fixture.addExits(t)
	own := fixture.order(fixture.parent.raceCandidates(fixture.quality, tcp))
	if own != "normal light" {
		t.Fatalf("the quality window's own field %q, want normal light", own)
	}
	var offerChanged, limitedOfferChanged <-chan struct{}
	if got := fixture.order(fixture.parent.raceField(fixture.quality, tcp, &offerChanged, &limitedOfferChanged)); got != own {
		t.Errorf("without a limited window: race field %q, want the window's %q", got, own)
	}

	// a limited window with an exit the flow cannot use: a v6 flow and a
	// limited exit that cannot carry v6
	fixture = newRaceMergeTestFixture(t, true)
	fixture.add(t, fixture.quality, "normal", capabilityClassA, raceMergeTestHeavyNormalRate)
	fixture.add(t, fixture.quality, "dual", capabilityClassA, raceMergeTestLightNormalRate)
	fixture.add(t, fixture.limited, "limited", capabilityClassD, raceMergeTestHeavyLimitedRate)
	tcp6 := flowNeed{ipVersion: 6, ipProtocol: IpProtocolTcp}
	if got := fixture.order(fixture.parent.raceField(fixture.quality, tcp6, &offerChanged, &limitedOfferChanged)); got != "normal dual" {
		t.Errorf("v6 tcp with a v4 limited exit: race field %q, want the window's normal dual", got)
	}

	// the merge itself
	field := fixture.parent.raceCandidates(fixture.quality, tcp6)
	if merged := fixture.parent.mergeRaceField(field, nil); len(merged) != len(field) || &merged[0] != &field[0] {
		t.Error("a field with no limited part was not handed back as it is")
	}
	limitedField := fixture.parent.raceCandidates(fixture.limited, tcp)
	if merged := fixture.parent.mergeRaceField(nil, limitedField); len(merged) != len(limitedField) || &merged[0] != &limitedField[0] {
		t.Error("a limited part with no window field was not handed back as it is")
	}
}

// One race of a cold-start SYN through sendPacketGroup over the fixture's
// windows, with the backend degraded or not. Returns the names of the exits
// the race registered and the ones it sent the packet to.
func raceMergeTestSend(t *testing.T, degraded bool, held string) (registeredNames []string, sentNames []string) {
	t.Helper()
	parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
	defer closeParent()
	parent.settings.MultiRaceSetOnNoResponseTimeout = 50 * time.Millisecond
	parent.settings.raceOrderRandomForTest = raceOrderTestRandom{}
	parent.backendDegradedForTest = func() bool { return degraded }

	fixture := &raceMergeTestFixture{
		ctx:      parent.ctx,
		settings: parent.settings,
		parent:   parent,
		names:    map[*multiClientChannel]string{},
	}
	fixture.addWindows(parent.generator, true)
	fixture.addExits(t)

	sent := make(chan *multiClientChannel, len(fixture.names))
	for client, name := range fixture.names {
		if name == held {
			// the platform is rate limiting this exit's contract requests
			window := fixture.quality
			if client.windowType() == WindowTypeLimited {
				window = fixture.limited
			}
			window.stateLock.Lock()
			delete(window.clients, client.ClientId())
			client.client = contractHeldTestClient(t)
			client.args.MultiClientGeneratorClientArgs.ClientId = client.client.ClientId()
			window.clients[client.ClientId()] = client
			window.stateLock.Unlock()
		}
		client.sendGroupForTest = func(group *parsedPacketGroup, timeout time.Duration, ack bool) (bool, error) {
			sent <- client
			for packetIndex := range group.packets {
				MessagePoolReturn(group.packets[packetIndex].packet)
			}
			return true, nil
		}
	}

	tcpPath := capabilityTestPath(4, IpProtocolTcp, 46000)
	packet := MessagePoolCopy(ipOosTcpPacketSequence(tcpPath, tcpFlagSyn, 1000, nil))
	group := requireGroupTestPacketGroup(t, packet)
	parent.ip4PathUpdates = map[Ip4Path]*multiClientChannelUpdate{
		tcpPath.ToIp4Path(): update,
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
	func() {
		update.stateLock.Lock()
		defer update.stateLock.Unlock()
		if update.race != nil {
			for client := range update.race.clientStates {
				registeredNames = append(registeredNames, fixture.names[client])
			}
		}
	}()
	select {
	case <-result:
	case <-time.After(30 * time.Second):
		t.Fatal("the race did not complete")
	}
	sentNames = append(sentNames, fixture.names[first])
	for done := false; !done; {
		select {
		case client := <-sent:
			sentNames = append(sentNames, fixture.names[client])
		default:
			done = true
		}
	}
	slices.Sort(registeredNames)
	slices.Sort(sentNames)
	return registeredNames, sentNames
}

// Under a degraded backend a cold-start race is bounded to
// MultiRaceDegradedClientCount (2) exits, and it takes the top 2 of the merged
// order across the windows: the heavy limited exit and the heavy normal one.
// Before, the bound spent both slots on the quality window's exits, so the
// race never reached the heaviest exit.
func TestDegradedRaceTakesTheTopOfTheMergedOrder(t *testing.T) {
	registered, sent := raceMergeTestSend(t, true, "")
	want := []string{"limited", "normal"}
	if !slices.Equal(registered, want) {
		t.Errorf("the degraded race registered %v, want %v", registered, want)
	}
	if !slices.Equal(sent, want) {
		t.Errorf("the degraded race sent to %v, want %v", sent, want)
	}
}

// A client whose contract requests are held still races last in the merged
// order, and a race that would send to it is bounded to 2: the heaviest exit
// is held, so the race goes to the next two by weight, one from each window.
// Before, both slots went to the quality window's exits.
func TestHeldClientStillRacesLastInTheMergedOrder(t *testing.T) {
	registered, sent := raceMergeTestSend(t, false, "limited")
	want := []string{"normal", "slight"}
	if !slices.Equal(registered, want) {
		t.Errorf("the race with a held exit registered %v, want %v", registered, want)
	}
	if !slices.Equal(sent, want) {
		t.Errorf("the race with a held exit sent to %v, want %v", sent, want)
	}
}

// With a healthy backend and no held exit the race is unbounded: every exit
// of both windows that carries the flow is dialed.
func TestHealthyRaceDialsTheWholeMergedField(t *testing.T) {
	registered, sent := raceMergeTestSend(t, false, "")
	want := []string{"light", "limited", "normal", "slight"}
	if !slices.Equal(registered, want) {
		t.Errorf("the race registered %v, want %v", registered, want)
	}
	if !slices.Equal(sent, want) {
		t.Errorf("the race sent to %v, want %v", sent, want)
	}
}
