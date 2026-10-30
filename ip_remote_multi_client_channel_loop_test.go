package connect

import (
	"context"
	"net"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A channel over a no-contract client with one live transport and nothing in
// flight: every health pass finds nothing to judge. The returned close joins
// the client.
func newIdleHealthTestChannel(ctx context.Context, cancel context.CancelFunc) (*multiClientChannel, func()) {
	destination := NewId()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), DefaultClientSettings())
	route := make(Route, 64)
	client.ContractManager().AddNoContractPeer(destination)
	client.RouteManager().UpdateTransport(&h1SendClientTransportForGroupTest{sendClientTransport: NewSendClientTransport(DestinationId(destination))}, []Route{route})
	channel := newPacketTransferTestChannel()
	channel.settings = DefaultMultiClientSettings()
	// the parent's projection, read once as a constructed channel reads it
	reliabilitySettings := ReliabilitySettingsFrom(channel.settings)
	channel.reliabilitySettingsFunc = func() *ReliabilitySettings {
		return reliabilitySettings
	}
	channel.ctx, channel.cancel, channel.client, channel.log = ctx, cancel, client, NewNoopLogger()
	channel.args = &multiClientChannelArgs{Destination: RequireMultiHopId(destination)}
	return channel, func() {
		cancel()
		client.CloseAndWait(context.Background())
		for 0 < len(route) {
			MessagePoolReturn(<-route)
		}
	}
}

// An idle channel's health passes read the window stats into one value and
// wait on one timer: a pass allocates nothing.
func TestDetectBlackholeIdlePassesAllocateNothing(t *testing.T) {
	// the shared pools start their workers outside the bubble
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		channel, closeChannel := newIdleHealthTestChannel(ctx, cancel)
		defer closeChannel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			channel.detectBlackhole()
		}()
		time.Sleep(10 * blackholePollInterval)

		allocations := allocationsUnder(t, "(*multiClientChannel).detectBlackhole", func() {
			time.Sleep(100 * blackholePollInterval)
		})
		select {
		case <-done:
			t.Fatal("the idle channel's health loop ended")
		default:
		}
		cancel()
		<-done
		if 8 < allocations {
			t.Fatalf("the health loop allocated %d objects over 100 idle passes, want none", allocations)
		}
	})
}

// The ping loop's ack wait and its rest take turns on one timer.
func TestPingLoopReusesOneTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		channel := stallTestChannel()
		channel.ctx = ctx
		channel.cancel = cancel
		channel.log = NewNoopLogger()
		channel.args = &multiClientChannelArgs{Destination: RequireMultiHopId(NewId())}
		channel.settings.CPingTimeout = 30 * time.Second
		channel.settings.CPingRestTimeout = time.Second
		pings := atomic.Int64{}
		channel.pingSendForTest = func(_ time.Duration, ackCallback func(error)) (bool, error) {
			pings.Add(1)
			ackCallback(nil)
			return true, nil
		}

		done := make(chan struct{})
		timers := timerAllocationsUnder(t, "(*multiClientChannel).ping", func() {
			go func() {
				defer close(done)
				channel.ping()
			}()
			time.Sleep(100 * time.Second)
		})
		cancel()
		<-done
		if pings.Load() < 50 {
			t.Fatalf("only %d pings in 100s at a 1s rest", pings.Load())
		}
		if 4 < timers {
			t.Fatalf("the ping loop allocated %d timer objects over %d pings, want one timer for its life", timers, pings.Load())
		}
	})
}

// A ping that is never answered still ends the loop at its ack timeout.
func TestPingLoopUnansweredEndsAtTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		channel := stallTestChannel()
		channel.ctx = ctx
		channel.cancel = cancel
		channel.log = NewNoopLogger()
		channel.args = &multiClientChannelArgs{Destination: RequireMultiHopId(NewId())}
		channel.settings.CPingTimeout = 30 * time.Second
		channel.settings.CPingRestTimeout = time.Second
		pings := atomic.Int64{}
		channel.pingSendForTest = func(_ time.Duration, ackCallback func(error)) (bool, error) {
			// the third ping goes unanswered
			if pings.Add(1) < 3 {
				ackCallback(nil)
			}
			return true, nil
		}

		start := time.Now()
		done := make(chan struct{})
		go func() {
			defer close(done)
			channel.ping()
		}()
		<-done
		AssertEqual(t, int64(3), pings.Load())
		if elapsed := time.Since(start); elapsed < 30*time.Second || 40*time.Second < elapsed {
			t.Fatalf("the unanswered ping ended the loop after %s, want its 30s ack timeout after two rests", elapsed)
		}
		if ctx.Err() != nil {
			t.Fatal("an unanswered ping canceled the channel")
		}
	})
}

// A window whose clients are all idle: each send-stall pass finds nothing.
func newIdleSendStallTestWindow(ctx context.Context, clientCount int) *multiClientWindow {
	settings := DefaultMultiClientSettings()
	reliabilitySettings := ReliabilitySettingsFrom(settings)
	window := &multiClientWindow{
		ctx:      ctx,
		settings: settings,
		reliabilitySettingsFunc: func() *ReliabilitySettings {
			return reliabilitySettings
		},
		resizeMonitor: NewMonitor(),
		clients:       map[Id]*multiClientChannel{},
	}
	for range clientCount {
		window.clients[NewId()] = stallTestChannel()
	}
	return window
}

// The send-stall watchdog waits on one timer and reads the clients into one
// list: a pass that finds nothing stalled allocates nothing.
func TestWatchSendStallsIdlePassesAllocateNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		window := newIdleSendStallTestWindow(ctx, 8)
		done := make(chan struct{})
		go func() {
			defer close(done)
			window.watchSendStalls()
		}()
		time.Sleep(10 * time.Second)

		allocations := allocationsUnder(t, "(*multiClientWindow).watchSendStalls", func() {
			time.Sleep(100 * time.Second)
		})
		cancel()
		<-done
		if 4 < allocations {
			t.Fatalf("the send-stall watchdog allocated %d objects over ~100 idle passes, want none", allocations)
		}
	})
}

// The window's client list is collected in one allocation.
func TestUnorderedClientsCollectsInOneAllocation(t *testing.T) {
	window := newIdleSendStallTestWindow(context.Background(), 16)
	AssertEqual(t, 16, len(window.unorderedClients()))
	// -race instruments allocations, so the count only holds without it
	if allocs := testing.AllocsPerRun(100, func() {
		window.unorderedClients()
	}); !raceEnabled && 1 < allocs {
		t.Fatalf("collecting 16 clients took %.1f allocations, want one", allocs)
	}
	AssertEqual(t, 0, len(newIdleSendStallTestWindow(context.Background(), 0).unorderedClients()))
}

// Reading the window stats builds the per-destination source counts in a
// list the channel keeps, so a read allocates only the stats it returns.
func TestWindowStatsReusesSourceCountList(t *testing.T) {
	channel := stallTestChannel()
	for i := range 32 {
		path := udpTestPath(4)
		path.DestinationIp = net.IPv4(93, 184, 216, byte(i))
		channel.addSend(1440, path)
	}
	stats, _ := channel.WindowStats()
	AssertEqual(t, 32, stats.sendDestinationCount)
	AssertEqual(t, 1, stats.sourceCount)
	if allocs := testing.AllocsPerRun(100, func() {
		channel.WindowStats()
	}); 1 < allocs {
		t.Fatalf("a window stats read took %.1f allocations, want only the stats it returns", allocs)
	}
}

// A parent with one window of idle exits whose qualification is fresh: every
// prober scan plans nothing.
func newIdleProberTestMultiClient(ctx context.Context, cancel context.CancelFunc, clientCount int) *RemoteUserNatMultiClient {
	settings := DefaultMultiClientSettings()
	settings.ProviderProbe = true
	window := newIdleSendStallTestWindow(ctx, clientCount)
	return &RemoteUserNatMultiClient{
		ctx:                        ctx,
		cancel:                     cancel,
		log:                        NewNoopLogger(),
		settings:                   settings,
		defaultReliabilitySettings: ReliabilitySettingsFrom(settings),
		windows:                    map[WindowType]*multiClientWindow{WindowTypeQuality: window},
		clientUpdates:              map[*multiClientChannel]map[*multiClientChannelUpdate]bool{},
		// the fixture's exits carry no destination, so they share the zero key
		qualification: map[MultiHopId]*providerQualification{
			{}: {qualifiedAt: time.Now(), lastProbeAt: time.Now(), passed: 1},
		},
	}
}

// The prober waits on one timer and refills one set of scan lists: a scan
// that plans nothing allocates nothing.
func TestProberIdleScansAllocateNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		multiClient := newIdleProberTestMultiClient(ctx, cancel, 8)
		done := make(chan struct{})
		go func() {
			defer close(done)
			multiClient.runProber()
		}()
		time.Sleep(2 * proberScanInterval)

		allocations := allocationsUnder(t, "(*RemoteUserNatMultiClient).runProber", func() {
			time.Sleep(100 * proberScanInterval)
		})
		cancel()
		<-done
		if 4 < allocations {
			t.Fatalf("the prober allocated %d objects over 100 scans that planned nothing, want none", allocations)
		}
	})
}

// The goroutines parked in a channel cleanup closure.
func channelCleanupWaiters() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	waiters := 0
	for _, goroutine := range strings.Split(string(buf), "\n\n") {
		for _, line := range strings.Split(goroutine, "\n") {
			if strings.HasPrefix(line, "github.com/urnetwork/connect.newMultiClientChannel.func") {
				waiters += 1
				break
			}
		}
	}
	return waiters
}

func newCleanupWaiterTestChannel(t *testing.T, ctx context.Context, generator *cleanupOrderTestGenerator) *multiClientChannel {
	t.Helper()
	settings := DefaultMultiClientSettings()
	settings.CPingRestTimeout = time.Hour
	settings.BlackholeTimeout = time.Hour
	channel, err := newMultiClientChannel(
		ctx,
		&multiClientChannelArgs{
			MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: NewId()},
			Destination:                    RequireMultiHopId(NewId()),
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
	return channel
}

// A live channel parks no goroutine for its cleanup, and the cleanup still
// runs once its channel or its client ends.
func TestMultiClientChannelCleanupParksNoGoroutine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	before := channelCleanupWaiters()
	generators := []*cleanupOrderTestGenerator{}
	channels := []*multiClientChannel{}
	for range 4 {
		generator := &cleanupOrderTestGenerator{removeCalled: make(chan struct{})}
		channels = append(channels, newCleanupWaiterTestChannel(t, ctx, generator))
		generators = append(generators, generator)
	}
	// a goroutine shows its frames once it has started, so watch a while
	maxWaiters := 0
	for range 20 {
		maxWaiters = max(maxWaiters, channelCleanupWaiters()-before)
		time.Sleep(5 * time.Millisecond)
	}
	if 0 < maxWaiters {
		t.Fatalf("4 live channels parked %d goroutines for their cleanup, want none", maxWaiters)
	}

	// a client that ends on its own
	generators[0].client.Cancel()
	select {
	case <-generators[0].removeCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("a client that ended on its own did not clean up its channel")
	}
	// a cancelled channel
	channels[1].Cancel()
	select {
	case <-generators[1].removeCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled channel did not clean up")
	}
	// the window ending
	cancel()
	for i, generator := range generators[2:] {
		select {
		case <-generator.removeCalled:
		case <-time.After(5 * time.Second):
			t.Fatalf("channel %d did not clean up when its window ended", i+2)
		}
	}
}
