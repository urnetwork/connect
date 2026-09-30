package connect

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// While the backend is degraded, one cold-start race fans the packet out to at
// most MultiRaceDegradedClientCount exits
// (https://github.com/urnetwork/connect/issues/181). Healthy, the shipped
// unbounded race is unchanged.

func TestMultiRaceClientCountTable(t *testing.T) {
	cases := []struct {
		name       string
		ordered    int
		configured int
		degraded   int
		isDegraded bool
		want       int
	}{
		// healthy: the shipped wide race and the configured bound, as before
		{"healthy unbounded", 6, 0, 2, false, 6},
		{"healthy configured bound", 6, 3, 2, false, 3},
		{"healthy configured above field", 2, 3, 2, false, 2},
		// degraded: the degraded bound applies on top of either
		{"degraded unbounded", 6, 0, 2, true, 2},
		{"degraded configured wider", 6, 4, 2, true, 2},
		{"degraded configured narrower", 6, 1, 2, true, 1},
		{"degraded small field", 1, 0, 2, true, 1},
		{"degraded empty field", 0, 0, 2, true, 0},
		// a zero degraded bound turns the degraded cap off
		{"degraded bound off", 6, 0, 0, true, 6},
	}
	for _, c := range cases {
		if got := multiRaceClientCount(c.ordered, c.configured, c.degraded, c.isDegraded); got != c.want {
			t.Errorf("%s: multiRaceClientCount(%d, %d, %d, %t) = %d, want %d",
				c.name, c.ordered, c.configured, c.degraded, c.isDegraded, got, c.want)
		}
	}
}

// The shipped defaults: the healthy race stays unbounded (see
// TestReliabilitySettingsMultiRaceClientCountDefault), the degraded bound is 2.
func TestMultiRaceDegradedClientCountDefault(t *testing.T) {
	settings := DefaultMultiClientSettings()
	AssertEqual(t, settings.MultiRaceClientCount, 0)
	AssertEqual(t, settings.MultiRaceDegradedClientCount, 2)
}

func TestMultiClientRaceBackendDegradedFollowsProcessState(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	parent := &RemoteUserNatMultiClient{}
	if parent.backendDegraded() {
		t.Fatal("healthy backend must not bound the race")
	}
	for i := 0; i < backendDegradedFailThreshold; i++ {
		noteBackendFailure()
	}
	if !parent.backendDegraded() {
		t.Fatal("race not bounded after the failure threshold was reached")
	}
	noteBackendSuccess()
	if parent.backendDegraded() {
		t.Fatal("race still bounded after a successful round-trip")
	}
}

// End to end through sendPacketGroup: a cold-start SYN with four race
// candidates registers (and sends to) all four when healthy and only the
// first two while degraded.
func TestMultiClientRaceFanOutBoundedWhileDegraded(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		testMultiClientRaceFanOut(t, false, 4)
	})
	t.Run("degraded", func(t *testing.T) {
		testMultiClientRaceFanOut(t, true, 2)
	})
}

func testMultiClientRaceFanOut(t *testing.T, degraded bool, wantRacers int) {
	t.Helper()
	parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
	defer closeParent()
	parent.settings.MultiRaceSetOnNoResponseTimeout = 50 * time.Millisecond
	parent.backendDegradedForTest = func() bool { return degraded }

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
		client.sendGroupForTest = func(group *parsedPacketGroup, timeout time.Duration, ack bool) (bool, error) {
			sent <- client
			for packetIndex := range group.packets {
				MessagePoolReturn(group.packets[packetIndex].packet)
			}
			return true, nil
		}
		candidates[i] = client
	}
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
	case <-time.After(5 * time.Second):
		t.Fatal("no race candidate was sent the packet")
	}
	update.stateLock.Lock()
	registered := map[*multiClientChannel]bool{}
	if update.race != nil {
		for client := range update.race.clientStates {
			registered[client] = true
		}
	}
	update.stateLock.Unlock()

	select {
	case <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("race did not complete")
	}
	sentTo := map[*multiClientChannel]bool{first: true}
	for done := false; !done; {
		select {
		case client := <-sent:
			sentTo[client] = true
		default:
			done = true
		}
	}

	if len(registered) != wantRacers {
		t.Errorf("race registered %d candidates, want %d", len(registered), wantRacers)
	}
	if len(sentTo) != wantRacers {
		t.Errorf("packet sent to %d candidates, want %d", len(sentTo), wantRacers)
	}
	// the bound keeps the head of the ordered field
	for client := range sentTo {
		index := -1
		for i, candidate := range candidates {
			if candidate == client {
				index = i
			}
		}
		if index < 0 || wantRacers <= index {
			t.Errorf("packet sent to candidate %d, outside the first %d", index, wantRacers)
		}
	}
}
