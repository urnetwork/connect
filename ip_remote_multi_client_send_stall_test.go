package connect

import (
	"testing"
	"time"
)

// mirrors what the real constructor initializes for the send accounting path:
// the event bucket coalescing needs its window durations, and the per-source
// counts are assigned into, so the maps cannot be nil
func stallTestChannel() *multiClientChannel {
	return &multiClientChannel{
		settings: &MultiClientSettings{
			StatsWindowBucketDuration: 1 * time.Second,
			StatsWindowDuration:       30 * time.Second,
		},
		ip4DestinationSourceCount: map[Ip4Path]map[Ip4Path]int{},
		ip6DestinationSourceCount: map[Ip6Path]map[Ip6Path]int{},
		packetStats:               &clientWindowStats{log: DefaultLogger()},
	}
}

// a client holding unacked sends with no progress is failed, and the check has
// to fire well inside AckTimeout or the flows pinned to it stay frozen
func TestSendStalledDetectsNoAckProgress(t *testing.T) {
	stallTimeout := 60 * time.Millisecond
	client := stallTestChannel()

	// nothing outstanding: an idle client is not a stalled one
	AssertEqual(t, client.sendStalled(stallTimeout), false)

	client.addSend(1440, udpTestPath(4))

	// inside the window it is merely slow
	AssertEqual(t, client.sendStalled(stallTimeout), false)

	time.Sleep(stallTimeout + 30*time.Millisecond)

	AssertEqual(t, client.sendStalled(stallTimeout), true)
}

// an ack is progress, so it restarts the clock -- a busy client that keeps
// delivering must never be judged stalled however long it stays busy
func TestSendStalledResetsOnAck(t *testing.T) {
	stallTimeout := 60 * time.Millisecond
	client := stallTestChannel()

	client.addSend(1440, udpTestPath(4))
	time.Sleep(stallTimeout + 30*time.Millisecond)
	AssertEqual(t, client.sendStalled(stallTimeout), true)

	// progress arrives
	client.addSendAck(1440)

	// with nothing outstanding it cannot be stalled
	AssertEqual(t, client.sendStalled(stallTimeout), false)

	// and a fresh send starts a fresh window rather than inheriting the old one
	client.addSend(1440, udpTestPath(4))
	AssertEqual(t, client.sendStalled(stallTimeout), false)
}

// a client that is delivering steadily never trips the check, even across many
// sends, because each ack restarts the clock
func TestSendStalledSteadyProgress(t *testing.T) {
	stallTimeout := 60 * time.Millisecond
	client := stallTestChannel()

	for i := 0; i < 5; i++ {
		client.addSend(1440, udpTestPath(4))
		time.Sleep(20 * time.Millisecond)
		client.addSendAck(1440)
		AssertEqual(t, client.sendStalled(stallTimeout), false)
	}
}

// partial progress still counts: acking one of several outstanding sends means
// the client is alive, so the remaining ones are not a stall
func TestSendStalledPartialAckIsProgress(t *testing.T) {
	stallTimeout := 60 * time.Millisecond
	client := stallTestChannel()

	client.addSend(1440, udpTestPath(4))
	client.addSend(1440, udpTestPath(4))
	time.Sleep(40 * time.Millisecond)

	client.addSendAck(1440)

	// one is still outstanding, but the client just proved it is delivering
	AssertEqual(t, client.sendStalled(stallTimeout), false)
}

// 0 restores the previous behavior, where nothing but AckTimeout classified a
// client that accepts packets and never delivers them
func TestSendStalledDisabled(t *testing.T) {
	client := stallTestChannel()
	client.addSend(1440, udpTestPath(4))
	time.Sleep(80 * time.Millisecond)

	AssertEqual(t, client.sendStalled(0), false)
	AssertEqual(t, client.sendStalled(-1*time.Second), false)
}

// the StallExit diagnostic hook and the detector have to agree: a channel put
// into the stalled state swallows sends, which is exactly the no-ack-progress
// condition the detector looks for
func TestStallExitHookTripsTheDetector(t *testing.T) {
	stallTimeout := 60 * time.Millisecond
	client := stallTestChannel()
	client.setStalled(true)

	// the send is reported successful and never acked, as a real stall behaves
	success, err := client.SendDetailedWithAck(&parsedPacket{
		packet: make([]byte, 40),
		ipPath: udpTestPath(4),
	}, 0, true)
	AssertEqual(t, success, true)
	AssertEqual(t, err == nil, true)

	// no manual addSend here on purpose. this test used to compensate for the
	// swallowed packet never reaching addSend, which made it pass whether or
	// not the stall was actually detectable -- it asserted the detector worked
	// on accounting the test itself had supplied. the swallow now happens after
	// addSend, exactly as a real blackholing provider behaves, so the stall
	// must be visible from the send above alone.
	time.Sleep(stallTimeout + 30*time.Millisecond)

	AssertEqual(t, client.sendStalled(stallTimeout), true)
}

func TestDefaultMultiClientSettingsSetsSendStallTimeout(t *testing.T) {
	settings := DefaultMultiClientSettings()

	AssertEqual(t, 0 < settings.SendStallTimeout, true)
	// must fire well inside the AckTimeout it exists to preempt
	AssertEqual(t, settings.SendStallTimeout < settings.AckTimeout, true)
}
