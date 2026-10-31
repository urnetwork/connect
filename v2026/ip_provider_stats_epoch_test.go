// The provider's packet stats epoch parks once an epoch passes with no counted
// packet, and every counted packet path wakes it so its epoch reports.
package connect

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

type packetStatsEpochTestProvider struct {
	provider    *RemoteUserNatProvider
	reports     chan *PacketStats
	returnSends chan providerReturnSendResult
	wakes       atomic.Int64
	source      TransferPath
}

func newPacketStatsEpochTestProvider(t *testing.T, eventEpoch time.Duration) *packetStatsEpochTestProvider {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), DefaultClientSettings())
	t.Cleanup(client.Cancel)
	localUserNat := NewLocalUserNatWithDefaults(ctx, "stats epoch test")
	settings := DefaultRemoteUserNatProviderSettings()
	settings.WriteTimeout = time.Second
	settings.EventEpoch = eventEpoch
	f := &packetStatsEpochTestProvider{
		reports:     make(chan *PacketStats, 64),
		returnSends: make(chan providerReturnSendResult, 8),
		source:      SourceId(NewId()),
	}
	f.provider = NewRemoteUserNatProvider(client, localUserNat, settings)
	t.Cleanup(f.provider.Close)
	f.provider.afterReturnSendForTest = func(result providerReturnSendResult) {
		f.returnSends <- result
	}
	// set before the first callback starts the epoch worker
	f.provider.packetStatsWakeForTest = func() {
		f.wakes.Add(1)
	}
	unsubscribe := f.provider.AddPacketStatsCallback(func(packetStats *PacketStats) {
		select {
		case f.reports <- packetStats:
		default:
		}
	})
	t.Cleanup(unsubscribe)
	return f
}

// Waits for a report equal to the counters as they stand now.
func (self *packetStatsEpochTestProvider) waitReportOfCurrentStats(t *testing.T, step string) {
	t.Helper()
	want := self.provider.PacketStats()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case packetStats := <-self.reports:
			if packetStatsEqual(packetStats, want) {
				return
			}
			// an earlier change; keep waiting
		case <-timeout:
			t.Fatalf("%s: the stats epoch never reported %+v", step, want)
		}
	}
}

func (self *packetStatsEpochTestProvider) clientReceive(t *testing.T, packet []byte) {
	t.Helper()
	frame, err := ToFrame(&protocol.IpPacketToProvider{
		IpPacket: &protocol.IpPacket{PacketBytes: packet},
	}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	self.provider.ClientReceive(self.source, []*protocol.Frame{frame}, Peer{ProvideMode: protocol.ProvideMode_Public})
}

func (self *packetStatsEpochTestProvider) returnReceive(t *testing.T, packet []byte) {
	t.Helper()
	ipPath, err := ParseIpPath(packet)
	if err != nil {
		t.Fatal(err)
	}
	self.provider.Receive(self.source, protocol.ProvideMode_Public, ipPath, packet)
}

// With no packet counted, the epoch parks instead of waking every epoch and
// allocates nothing; the next counted packet wakes it and is reported.
func TestProviderPacketStatsEpochParksWhileIdle(t *testing.T) {
	f := newPacketStatsEpochTestProvider(t, 2*time.Millisecond)
	// the first epoch reports the counters as they stand
	f.waitReportOfCurrentStats(t, "first epoch")
	time.Sleep(20 * time.Millisecond)

	before := f.wakes.Load()
	idle := 100 * time.Millisecond
	allocations := allocationsUnder(t, ").runPacketStats", func() {
		time.Sleep(idle)
	})
	if idleWakes := f.wakes.Load() - before; 2 < idleWakes {
		t.Errorf("the stats epoch woke %d times in %s with no counted packet at a 2ms epoch, want it parked", idleWakes, idle)
	}
	if 4 < allocations {
		t.Errorf("the idle stats epoch allocated %d objects in %s, want none past its one timer", allocations, idle)
	}
	select {
	case packetStats := <-f.reports:
		t.Fatalf("unchanged packet stats were reported: %+v", packetStats)
	default:
	}

	// a blocked packet is the change, and the parked epoch reports it
	srcIp, dstIp := net.ParseIP("192.0.2.9"), net.ParseIP("203.0.113.7")
	f.clientReceive(t, craftSecurityPacket(IpProtocolTcp, srcIp, 42000, dstIp, 51413, false, bittorrentHandshakePacketPayload()))
	if f.provider.PacketStats().BlockIngressPacketCount != 1 {
		t.Fatal("the BitTorrent handshake was not counted as blocked")
	}
	f.waitReportOfCurrentStats(t, "after the epoch parked")
}

// Every path that counts a packet wakes a parked epoch: remote ingress, the
// return of an orphan reset, blocked ingress, a return, and a blocked return.
func TestProviderPacketStatsEveryCountedPathWakesTheEpoch(t *testing.T) {
	eventEpoch := 10 * time.Millisecond
	f := newPacketStatsEpochTestProvider(t, eventEpoch)
	f.waitReportOfCurrentStats(t, "first epoch")
	park := func() {
		// several epochs with no counted packet, so a fixed epoch has parked
		time.Sleep(5 * eventEpoch)
	}
	srcIp, dstIp := net.ParseIP("192.0.2.9"), net.ParseIP("203.0.113.7")

	park()
	// an allowed outbound packet with no flow: remote ingress, and the NAT's
	// orphan reset returns as remote egress
	f.clientReceive(t, craftSecurityPacket(IpProtocolTcp, srcIp, 42001, dstIp, 8080, false, []byte("GET / HTTP/1.1\r\n\r\n")))
	if result := waitProviderReturnSendCompletion(t, f.returnSends); !result.sent {
		t.Fatalf("the orphan reset was not returned: %+v", result)
	}
	stats := f.provider.PacketStats()
	if stats.RemoteIngressPacketCount != 1 || stats.RemoteEgressPacketCount != 1 {
		t.Fatalf("unexpected stats after the outbound packet: %+v", stats)
	}
	f.waitReportOfCurrentStats(t, "remote ingress and its reset")

	park()
	f.clientReceive(t, craftSecurityPacket(IpProtocolTcp, srcIp, 42000, dstIp, 51413, false, bittorrentHandshakePacketPayload()))
	if f.provider.PacketStats().BlockIngressPacketCount != 1 {
		t.Fatal("the BitTorrent handshake was not counted as blocked")
	}
	f.waitReportOfCurrentStats(t, "blocked ingress")

	park()
	f.returnReceive(t, craftSecurityPacket(IpProtocolTcp, dstIp, 8080, srcIp, 42001, false, []byte("HTTP/1.1 200 OK\r\n\r\n")))
	if result := waitProviderReturnSendCompletion(t, f.returnSends); !result.sent {
		t.Fatalf("the return was not sent: %+v", result)
	}
	if f.provider.PacketStats().RemoteEgressPacketCount != 2 {
		t.Fatal("the return was not counted as remote egress")
	}
	f.waitReportOfCurrentStats(t, "remote egress")

	park()
	f.returnReceive(t, craftSecurityPacket(IpProtocolTcp, dstIp, 6881, srcIp, 42002, false, []byte("stray")))
	if f.provider.PacketStats().BlockEgressPacketCount != 1 {
		t.Fatal("the stray return was not counted as blocked")
	}
	f.waitReportOfCurrentStats(t, "blocked egress")
}

type packetStatsEpochReport struct {
	at    time.Time
	stats *PacketStats
}

// In virtual time: the first epoch reports, a parked worker takes no wake, and
// a counted packet is reported exactly one epoch after it, together with the
// packets counted during that epoch.
func TestProviderPacketStatsEpochReportsOneEpochAfterTheChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		eventEpoch := time.Second
		provider := &RemoteUserNatProvider{
			ctx:                  ctx,
			settings:             &RemoteUserNatProviderSettings{EventEpoch: eventEpoch},
			packetStatsCounters:  &packetStatsCounters{},
			packetStatsChange:    newPacketStatsChange(),
			packetStatsCallbacks: NewCallbackList[PacketStatsFunction](),
		}
		wakes := atomic.Int64{}
		provider.packetStatsWakeForTest = func() {
			wakes.Add(1)
		}
		reports := make(chan packetStatsEpochReport, 8)
		provider.packetStatsCallbacks.Add(func(packetStats *PacketStats) {
			reports <- packetStatsEpochReport{at: time.Now(), stats: packetStats}
		})
		// as a registration does
		provider.packetStatsChange.raise()
		done := make(chan struct{})
		go func() {
			defer close(done)
			provider.runPacketStats()
		}()
		defer func() {
			cancel()
			<-done
		}()
		takeReport := func() (packetStatsEpochReport, bool) {
			synctest.Wait()
			select {
			case report := <-reports:
				return report, true
			default:
				return packetStatsEpochReport{}, false
			}
		}

		start := time.Now()
		time.Sleep(eventEpoch)
		report, ok := takeReport()
		if !ok || !report.at.Equal(start.Add(eventEpoch)) || !packetStatsEqual(report.stats, provider.PacketStats()) {
			t.Fatalf("first report ok=%t at %s, want the counters at %s", ok, report.at.Sub(start), eventEpoch)
		}

		time.Sleep(100 * eventEpoch)
		if _, ok := takeReport(); ok {
			t.Fatal("unchanged counters were reported")
		}
		if got := wakes.Load(); 2 < got {
			t.Fatalf("the worker woke %d times over 101 epochs with one change, want it parked after the epoch that found none", got)
		}

		// two packets an epoch apart from the first are one report
		changedAt := time.Now()
		provider.countBlockIngress(1, 100)
		time.Sleep(eventEpoch / 2)
		provider.countRemoteIngress(TransportTypeH3, 2, 300)
		time.Sleep(eventEpoch/2 - time.Nanosecond)
		if _, ok := takeReport(); ok {
			t.Fatal("a change was reported before its epoch ended")
		}
		time.Sleep(time.Nanosecond)
		report, ok = takeReport()
		if !ok || !report.at.Equal(changedAt.Add(eventEpoch)) {
			t.Fatalf("change report ok=%t at %s after the change, want %s", ok, report.at.Sub(changedAt), eventEpoch)
		}
		if report.stats.BlockIngressPacketCount != 1 || report.stats.RemoteIngressPacketCount != 2 ||
			report.stats.TransportStats[TransportTypeH3].RemoteIngressByteCount != 300 {
			t.Fatalf("the report missed a packet counted in its epoch: %+v", report.stats)
		}

		// a carrier move changes no total and is still reported
		attribution := newTransportPacketAttribution(provider.packetStatsCounters, 1, 64)
		provider.countReturnAdmitted(attribution)
		time.Sleep(eventEpoch)
		if report, ok = takeReport(); !ok || report.stats.RemoteEgressPacketCount != 1 {
			t.Fatalf("the admitted return was not reported: ok=%t %+v", ok, report.stats)
		}
		time.Sleep(10 * eventEpoch)
		movedAt := time.Now()
		provider.packetStatsChange.transportWriteObserver(attribution)(TransportTypeP2p)
		time.Sleep(eventEpoch)
		report, ok = takeReport()
		if !ok || !report.at.Equal(movedAt.Add(eventEpoch)) ||
			report.stats.TransportStats[TransportTypeP2p].RemoteEgressPacketCount != 1 ||
			report.stats.TransportStats[TransportTypeUnknown].RemoteEgressPacketCount != 0 {
			t.Fatalf("the carrier move was not reported an epoch later: ok=%t %+v", ok, report.stats)
		}
	})
}
