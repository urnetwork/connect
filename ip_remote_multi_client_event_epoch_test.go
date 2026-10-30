package connect

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
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

// An idle event epoch parks. Once an epoch passes with nothing to report the
// worker stops waking, and each kind of change wakes it to report one epoch
// later: a counted packet (through the counter methods, the asynchronous
// transport attribution, and a direct count), a collected block action, and
// the first packet stats listener.
func TestMultiClientEventEpochParksWhileIdle(t *testing.T) {
	// the shared pools start their workers outside the bubble
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		epoch := time.Second
		multiClient := newEventEpochTestMultiClient(t, epoch)
		wakes := atomic.Int64{}
		countWake := func() {
			wakes.Add(1)
		}
		multiClient.eventEpochWakeForTest.Store(&countWake)
		// the wakes over `idle` once the worker had an epoch to settle
		idleWakes := func(idle time.Duration) int64 {
			time.Sleep(2 * epoch)
			synctest.Wait()
			before := wakes.Load()
			time.Sleep(idle)
			synctest.Wait()
			return wakes.Load() - before
		}

		// with no listener there is nothing to report
		if n := idleWakes(100 * epoch); 0 < n {
			t.Fatalf("the event epoch woke %d times over 100 epochs with no listener", n)
		}

		packetStatsReports := make(chan *PacketStats, 16)
		unsubPacketStats := multiClient.AddPacketStatsCallback(func(packetStats *PacketStats) {
			packetStatsReports <- packetStats
		})
		defer unsubPacketStats()
		blockActionReports := make(chan []*BlockAction, 16)
		unsubBlockActions := multiClient.AddBlockActionCallback(func(blockActions []*BlockAction) {
			blockActionReports <- blockActions
		})
		defer unsubBlockActions()
		// one epoch after a change, its report has been delivered
		reportedWithinEpoch := func(name string) *PacketStats {
			t.Helper()
			synctest.Wait()
			time.Sleep(epoch)
			synctest.Wait()
			select {
			case packetStats := <-packetStatsReports:
				return packetStats
			default:
				t.Fatalf("%s was not reported within one epoch", name)
				return nil
			}
		}

		// the first listener is reported the starting counters
		reportedWithinEpoch("the first listener")
		if n := idleWakes(100 * epoch); 0 < n {
			t.Fatalf("the event epoch woke %d times over 100 idle epochs", n)
		}

		multiClient.packetStatsCounters.recordRemoteIngress(TransportTypeH3, 2, 300)
		if packetStats := reportedWithinEpoch("a remote ingress"); packetStats.TransportStats[TransportTypeH3].RemoteIngressByteCount != 300 {
			t.Fatalf("the remote ingress was reported as %+v", packetStats.TransportStats[TransportTypeH3])
		}
		if n := idleWakes(10 * epoch); 0 < n {
			t.Fatalf("the event epoch woke %d times over 10 idle epochs after a remote ingress", n)
		}

		// the attribution moves an admitted packet to its transport when the
		// route write is observed, after the send returned
		attribution := newTransportPacketAttribution(multiClient.packetStatsCounters, 1, 100)
		attribution.admit()
		if packetStats := reportedWithinEpoch("a remote egress"); packetStats.TransportStats[TransportTypeUnknown].RemoteEgressByteCount != 100 {
			t.Fatalf("the remote egress was reported as %+v", packetStats.TransportStats[TransportTypeUnknown])
		}
		if n := idleWakes(10 * epoch); 0 < n {
			t.Fatalf("the event epoch woke %d times over 10 idle epochs after a remote egress", n)
		}
		attribution.observe(TransportTypeH3)
		if packetStats := reportedWithinEpoch("a late transport attribution"); packetStats.TransportStats[TransportTypeH3].RemoteEgressByteCount != 100 {
			t.Fatalf("the late transport attribution was reported as %+v", packetStats.TransportStats[TransportTypeH3])
		}
		if n := idleWakes(10 * epoch); 0 < n {
			t.Fatalf("the event epoch woke %d times over 10 idle epochs after a transport attribution", n)
		}

		localPath := &IpPath{
			Version:         4,
			Protocol:        IpProtocolUdp,
			SourceIp:        net.ParseIP("192.0.2.10"),
			SourcePort:      40000,
			DestinationIp:   net.ParseIP("198.51.100.7"),
			DestinationPort: 53,
		}
		multiClient.localReceivePacket(TransferPath{}, protocol.ProvideMode_Network, localPath, make([]byte, 64))
		if packetStats := reportedWithinEpoch("a local ingress"); packetStats.LocalIngressPacketCount != 1 {
			t.Fatalf("the local ingress was reported as %d packets", packetStats.LocalIngressPacketCount)
		}
		if n := idleWakes(10 * epoch); 0 < n {
			t.Fatalf("the event epoch woke %d times over 10 idle epochs after a local ingress", n)
		}

		// plaintext smtp is rejected locally: a block action and a blocked count
		packet := smtpTestTcp4Packet(byte(tcpFlagAck|tcpFlagPsh), 12000, 34000, []byte("EHLO plaintext.example\r\n"))
		if multiClient.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, packet, 0) {
			t.Fatal("plaintext smtp was accepted")
		}
		MessagePoolReturn(packet)
		if packetStats := reportedWithinEpoch("a blocked packet"); packetStats.BlockEgressPacketCount != 1 {
			t.Fatalf("the blocked packet was reported as %d packets", packetStats.BlockEgressPacketCount)
		}
		select {
		case blockActions := <-blockActionReports:
			if len(blockActions) != 1 || !blockActions[0].Block {
				t.Fatalf("the block action was reported as %+v", blockActions)
			}
		default:
			t.Fatal("the block action was not reported within one epoch")
		}
		if n := idleWakes(100 * epoch); 0 < n {
			t.Fatalf("the event epoch woke %d times over 100 idle epochs after a block action", n)
		}
	})
}
