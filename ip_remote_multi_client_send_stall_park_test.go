package connect

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A window of idle clients and one sibling that receives, with the send-stall
// watchdog's passes counted. Every client signals the window's watchdog on
// the first send of a run, as a client the window constructs does.
func newParkingSendStallTestWindow(ctx context.Context, clientCount int) (*multiClientWindow, []*multiClientChannel, *multiClientChannel, *atomic.Int64) {
	settings := DefaultMultiClientSettings()
	reliabilitySettings := ReliabilitySettingsFrom(settings)
	// a stall convicts on its bar alone
	reliabilitySettings.BusyProbe = false
	window := &multiClientWindow{
		ctx:      ctx,
		settings: settings,
		reliabilitySettingsFunc: func() *ReliabilitySettings {
			return reliabilitySettings
		},
		resizeMonitor: NewMonitor(),
		clients:       map[Id]*multiClientChannel{},
	}
	passes := &atomic.Int64{}
	window.sendStallPassForTest = func() {
		passes.Add(1)
	}
	window.sendStallWake = newOutstandingSendWake()
	clients := []*multiClientChannel{}
	for i := range clientCount + 1 {
		client := stallTestChannel()
		client.ctx, client.cancel = context.WithCancel(ctx)
		client.log = NewNoopLogger()
		client.sendStallWake = window.sendStallWake
		// bare channels identify as the zero id; key uniquely for the fixture
		clientId := Id{}
		clientId[0] = byte(i + 1)
		window.clients[clientId] = client
		clients = append(clients, client)
	}
	return window, clients[:clientCount], clients[clientCount], passes
}

func sendStallParkTestPath() *IpPath {
	return &IpPath{
		Version:         4,
		Protocol:        IpProtocolTcp,
		SourceIp:        net.ParseIP("10.0.0.2"),
		SourcePort:      40000,
		DestinationIp:   net.ParseIP("192.0.2.1"),
		DestinationPort: 443,
	}
}

// The send-stall watchdog parks while no client of its window has a send
// outstanding, since a stall needs one. The first send of a run wakes it, and
// that send still convicts within one poll of the stall bound.
func TestWatchSendStallsParksWithNothingOutstanding(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		window, clients, sibling, passes := newParkingSendStallTestWindow(ctx, 4)
		stallTimeout := window.reliabilitySettings().SendStallTimeout
		pollTimeout := sendStallPollTimeout(stallTimeout, window.settings.WindowResizeTimeout)
		done := make(chan struct{})
		go func() {
			defer close(done)
			window.watchSendStalls()
		}()
		defer func() {
			cancel()
			<-done
		}()
		// the passes over `idle` once the watchdog had a pass to settle
		idlePasses := func(idle time.Duration) int64 {
			time.Sleep(2 * pollTimeout)
			synctest.Wait()
			before := passes.Load()
			time.Sleep(idle)
			synctest.Wait()
			return passes.Load() - before
		}

		if n := idlePasses(100 * pollTimeout); 0 < n {
			t.Fatalf("the send-stall watchdog judged %d times over %s with nothing outstanding", n, 100*pollTimeout)
		}

		// the sibling receives through the stall, which makes it admissible
		client := clients[1]
		client.addSend(64, sendStallParkTestPath())
		siblingReceive := time.AfterFunc(stallTimeout, func() {
			sibling.addReceiveAck(64)
		})
		defer siblingReceive.Stop()
		time.Sleep(stallTimeout - time.Nanosecond)
		synctest.Wait()
		if client.IsDone() {
			t.Fatal("the outstanding send was convicted before its stall bound")
		}
		time.Sleep(pollTimeout + time.Nanosecond)
		synctest.Wait()
		if !client.IsDone() {
			t.Fatalf("the stalled send was not convicted within one poll of its %s bound", stallTimeout)
		}
		client.stateLock.Lock()
		endErr := client.endErr
		client.stateLock.Unlock()
		if endErr == nil || !strings.HasPrefix(endErr.Error(), "send stalled") {
			t.Fatalf("the stalled client ended with %v", endErr)
		}

		// once resize reaps the convicted client, nothing is outstanding again
		func() {
			window.stateLock.Lock()
			defer window.stateLock.Unlock()
			for clientId, windowClient := range window.clients {
				if windowClient == client {
					delete(window.clients, clientId)
				}
			}
		}()
		if n := idlePasses(100 * pollTimeout); 0 < n {
			t.Fatalf("the send-stall watchdog judged %d times over %s after the stalled client was reaped", n, 100*pollTimeout)
		}
	})
}

// An acknowledged send leaves nothing outstanding, so the watchdog parks
// again, and the next run's first send wakes it again.
func TestWatchSendStallsParksAgainAfterProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		window, clients, _, passes := newParkingSendStallTestWindow(ctx, 2)
		stallTimeout := window.reliabilitySettings().SendStallTimeout
		pollTimeout := sendStallPollTimeout(stallTimeout, window.settings.WindowResizeTimeout)
		done := make(chan struct{})
		go func() {
			defer close(done)
			window.watchSendStalls()
		}()
		defer func() {
			cancel()
			<-done
		}()
		time.Sleep(2 * pollTimeout)
		synctest.Wait()

		for run := range 3 {
			before := passes.Load()
			clients[0].addSend(64, sendStallParkTestPath())
			time.Sleep(stallTimeout / 2)
			synctest.Wait()
			if passes.Load() == before {
				t.Fatalf("run %d: the first send of a run did not wake the parked watchdog", run)
			}
			clients[0].addSendAck(64)
			time.Sleep(2 * pollTimeout)
			synctest.Wait()
			before = passes.Load()
			time.Sleep(100 * pollTimeout)
			synctest.Wait()
			if n := passes.Load() - before; 0 < n {
				t.Fatalf("run %d: the send-stall watchdog judged %d times after the run's progress", run, n)
			}
			if clients[0].IsDone() {
				t.Fatalf("run %d: an acknowledged run was convicted", run)
			}
		}
	})
}

// A channel the window constructs holds the window's wake, and signals it at
// the first send of a run and only then: a send mid-run, which a parked
// watchdog cannot be waiting for, pays one atomic load.
func TestConstructedChannelSignalsItsWindowAtFirstSendOfRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wake := newOutstandingSendWake()
	generator := &cleanupOrderTestGenerator{removeCalled: make(chan struct{})}
	settings := DefaultMultiClientSettings()
	settings.CPingRestTimeout = time.Hour
	settings.BlackholeTimeout = time.Hour
	channel, err := newMultiClientChannel(
		ctx,
		&multiClientChannelArgs{
			MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: NewId()},
			Destination:                    RequireMultiHopId(NewId()),
			sendStallWake:                  wake,
		},
		generator,
		func(*multiClientChannel, TransferPath, protocol.ProvideMode, TransportType, *IpPath, []byte) {},
		nil,
		DefaultSecurityPolicy(ctx),
		func(*ContractStatus) {},
		func([]*ContractStatsEvent) {},
		func() {},
		nil,
		settings,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Cancel()
	signaled := func() bool {
		select {
		case <-wake.wake:
			return true
		default:
			return false
		}
	}

	wake.parked.Store(true)
	channel.addSend(64, sendStallParkTestPath())
	if !signaled() {
		t.Fatal("the first send of a run did not wake the parked watchdog")
	}
	wake.parked.Store(true)
	channel.addSend(64, sendStallParkTestPath())
	if signaled() {
		t.Fatal("a send mid-run woke the watchdog")
	}
	wake.parked.Store(false)
	channel.addSendAck(64)
	channel.addSendAck(64)

	// a group opens a run as a packet does
	group := &parsedPacketGroup{
		packets:   []parsedPacket{{ipPath: sendStallParkTestPath()}, {ipPath: sendStallParkTestPath()}},
		ipPath:    sendStallParkTestPath(),
		byteCount: 128,
	}
	wake.parked.Store(true)
	channel.addSendGroup(group)
	if !signaled() {
		t.Fatal("the first group of a run did not wake the parked watchdog")
	}
}

// The window hands its wake to every client it constructs, and wakes its
// watchdog when it admits one: a client with a send outstanding at admission
// would otherwise sit unjudged beside a parked watchdog.
func TestExpandWiresTheSendStallWake(t *testing.T) {
	source, err := readSource("ip_remote_multi_client.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := functionBody(source, "func (self *multiClientWindow) expand(")
	if !ok {
		t.Fatal("could not find expand")
	}
	if !strings.Contains(body, "args.sendStallWake = self.sendStallWake") {
		t.Error("expand does not hand the window's send-stall wake to the clients it constructs")
	}
	admit := strings.Index(body, "self.clients[clientId] = client")
	if admit < 0 {
		t.Fatal("could not find the admission in expand")
	}
	admitBlock := body[admit:]
	if end := strings.Index(admitBlock, "}()"); 0 <= end {
		admitBlock = admitBlock[:end]
	}
	if !strings.Contains(admitBlock, "self.sendStallWake.signal()") {
		t.Error("admitting a client does not wake the parked send-stall watchdog")
	}

	constructor, ok := functionBody(source, "func newMultiClientChannel(")
	if !ok {
		t.Fatal("could not find newMultiClientChannel")
	}
	if !strings.Contains(constructor, "args.sendStallWake") {
		t.Error("a constructed client does not hold its window's send-stall wake")
	}
}
