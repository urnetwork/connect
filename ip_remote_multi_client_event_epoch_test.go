package connect

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func newEventEpochTestMultiClient(t *testing.T, eventEpoch time.Duration) *RemoteUserNatMultiClient {
	t.Helper()
	settings := DefaultMultiClientSettings()
	settings.EventEpoch = eventEpoch
	settings.HeartbeatInterval = 0
	settings.ProviderProbe = false
	settings.IpAssocSettings = nil
	multiClient := NewRemoteUserNatMultiClient(
		context.Background(),
		&testingEmptyMultiClientGenerator{},
		func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
		},
		protocol.ProvideMode_Network,
		settings,
	)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := multiClient.CloseAndWait(ctx); err != nil {
			t.Errorf("multi client workers did not join: %v", err)
		}
	})
	return multiClient
}

// The event epoch arms one timer for its life, and an epoch with no new
// packets compares the counters in place instead of building a snapshot.
func TestMultiClientEventEpochIdleTicksAllocateNothing(t *testing.T) {
	multiClient := newEventEpochTestMultiClient(t, 2*time.Millisecond)
	packetStatsChannel := make(chan *PacketStats, 64)
	unsub := multiClient.AddPacketStatsCallback(func(packetStats *PacketStats) {
		select {
		case packetStatsChannel <- packetStats:
		default:
		}
	})
	defer unsub()

	// the first change is reported
	multiClient.packetStatsCounters.localIngressPacketCount.Add(1)
	select {
	case packetStats := <-packetStatsChannel:
		AssertEqual(t, int64(1), packetStats.LocalIngressPacketCount)
	case <-time.After(5 * time.Second):
		t.Fatal("the first packet stats change was not reported")
	}

	allocations := allocationsUnder(t, ").runEventEpoch", func() {
		time.Sleep(100 * time.Millisecond)
	})
	if 4 < allocations {
		t.Fatalf("the event epoch allocated %d objects over ~50 idle epochs, want none past its one timer", allocations)
	}
	select {
	case packetStats := <-packetStatsChannel:
		t.Fatalf("unchanged packet stats were reported: %+v", packetStats)
	default:
	}

	// a change after the idle epochs is still reported, transport stats too
	multiClient.packetStatsCounters.recordRemoteIngress(TransportTypeH3, 2, 300)
	select {
	case packetStats := <-packetStatsChannel:
		AssertEqual(t, int64(2), packetStats.RemoteIngressPacketCount)
		AssertEqual(t, ByteCount(300), packetStats.TransportStats[TransportTypeH3].RemoteIngressByteCount)
	case <-time.After(5 * time.Second):
		t.Fatal("a packet stats change after the idle epochs was not reported")
	}

	// a move between transports changes no total, and is still reported
	multiClient.packetStatsCounters.recordRemoteEgress(TransportTypeUnknown, 1, 100)
	select {
	case <-packetStatsChannel:
	case <-time.After(5 * time.Second):
		t.Fatal("a remote egress was not reported")
	}
	multiClient.packetStatsCounters.moveRemoteEgress(TransportTypeUnknown, TransportTypeP2p, 1, 100)
	select {
	case packetStats := <-packetStatsChannel:
		AssertEqual(t, ByteCount(100), packetStats.TransportStats[TransportTypeP2p].RemoteEgressByteCount)
		AssertEqual(t, ByteCount(0), packetStats.TransportStats[TransportTypeUnknown].RemoteEgressByteCount)
	case <-time.After(5 * time.Second):
		t.Fatal("a move between transports was not reported")
	}
}

// The in-place comparison agrees with comparing a fresh snapshot, through every
// kind of counter change.
func TestPacketStatsCountersUnchangedSinceMatchesSnapshot(t *testing.T) {
	counters := &packetStatsCounters{}
	AssertEqual(t, false, counters.unchangedSince(nil))

	changes := []func(){
		func() { counters.remoteEgressPacketCount.Add(1) },
		func() { counters.remoteEgressByteCount.Add(10) },
		func() { counters.remoteIngressPacketCount.Add(1) },
		func() { counters.remoteIngressByteCount.Add(10) },
		func() { counters.localEgressPacketCount.Add(1) },
		func() { counters.localEgressByteCount.Add(10) },
		func() { counters.localIngressPacketCount.Add(1) },
		func() { counters.localIngressByteCount.Add(10) },
		func() { counters.blockEgressPacketCount.Add(1) },
		func() { counters.blockEgressByteCount.Add(10) },
		func() { counters.blockIngressPacketCount.Add(1) },
		func() { counters.blockIngressByteCount.Add(10) },
		func() { counters.recordRemoteEgress(TransportTypeH1, 1, 10) },
		func() { counters.recordRemoteIngress(TransportTypeH3Dns, 1, 10) },
		func() { counters.moveRemoteEgress(TransportTypeH1, TransportTypeH3, 1, 10) },
		func() { counters.recordRemoteIngress(TransportType("other"), 1, 10) },
	}
	last := counters.snapshot()
	for i, change := range changes {
		AssertEqual(t, true, counters.unchangedSince(last))
		AssertEqual(t, packetStatsEqual(counters.snapshot(), last), counters.unchangedSince(last))
		change()
		if counters.unchangedSince(last) {
			t.Fatalf("change %d was not seen", i)
		}
		AssertEqual(t, packetStatsEqual(counters.snapshot(), last), counters.unchangedSince(last))
		last = counters.snapshot()
	}

	if allocs := testing.AllocsPerRun(100, func() {
		counters.unchangedSince(last)
	}); allocs != 0 {
		t.Fatalf("the in-place comparison allocated %.1f objects per call, want none", allocs)
	}
}
