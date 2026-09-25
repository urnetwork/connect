// First-provider publication wakes the real packet-group send path. Virtual
// time stays fixed for readiness checks; retry pacing is advanced explicitly.
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

// Builds a synthetic provider channel with only its final Transfer admission
// replaced. Ranking, family filtering, selection, and group ownership stay real.
func newProviderWakeChannel(ctx context.Context, settings *MultiClientSettings, family IpFamily, accept *atomic.Bool, calls *atomic.Int64) *multiClientChannel {
	return &multiClientChannel{
		ctx: ctx, log: NewNoopLogger(), settings: settings,
		args: &multiClientChannelArgs{
			MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: NewId()},
			Destination:                    RequireMultiHopId(NewId()),
			DestinationStats:               DestinationStats{IpFamily: family},
		},
		packetStats: &clientWindowStats{log: NewNoopLogger()},
		sendGroupForTest: func(group *parsedPacketGroup, _ time.Duration, _ bool) (bool, error) {
			calls.Add(1)
			if !accept.Load() {
				return false, nil
			}
			for _, packet := range group.packets {
				MessagePoolReturn(packet.packet)
			}
			return true, nil
		},
	}
}

// Atomically changes the fixture's offer and emits the existing window edge.
// The separate expansion control below verifies that the real producer emits it.
func publishProviderWakeChannel(window *multiClientWindow, client *multiClientChannel) {
	window.stateLock.Lock()
	defer window.stateLock.Unlock()
	window.clients[client.ClientId()] = client
	window.resizeMonitor.NotifyAll()
}

// Uses documentation-only addresses, never a real route or socket.
func providerWakePacket(version int) []byte {
	path := &IpPath{
		Version: version, Protocol: IpProtocolUdp,
		SourceIp: net.ParseIP("192.0.2.9"), SourcePort: 41000,
		DestinationIp: net.ParseIP("198.51.100.19"), DestinationPort: 8443,
	}
	if version == 6 {
		path.SourceIp = net.ParseIP("2001:db8::9")
		path.DestinationIp = net.ParseIP("2001:db8:1::19")
	}
	return MessagePoolCopy(ipOosUdpPacket(path, []byte{1}))
}

// Returns a packet owner whose real selector initially sees two empty windows.
// It also joins the sender and returns refused packet ownership on a RED path.
func newProviderWakeSend(t *testing.T, version int, timeout time.Duration, configure ...func(*MultiClientSettings)) (*RemoteUserNatMultiClient, <-chan bool, func()) {
	t.Helper()
	parent, _, closeParent := groupTestParent(t, DisableSecurityPolicy())
	parent.generator = &orderedClientsTestGenerator{fixedIsSet: false}
	parent.settings.FormationPollTimeout = time.Hour
	parent.settings.SendRetryTimeout = 2 * time.Hour
	parent.settings.MaxFlowsPerExit = 0
	for _, change := range configure {
		change(parent.settings)
	}
	for _, kind := range []WindowType{WindowTypeQuality, WindowTypeSpeed} {
		parent.windows[kind] = &multiClientWindow{
			ctx: parent.ctx, log: NewNoopLogger(), settings: parent.settings,
			windowType: kind, clients: map[Id]*multiClientChannel{},
			generator: parent.generator, resizeMonitor: NewMonitor(),
		}
	}
	packet := providerWakePacket(version)
	group := requireGroupTestPacketGroup(t, packet)
	result := make(chan bool, 1)
	done := make(chan struct{})
	var accepted atomic.Bool
	go func() {
		defer close(done)
		ok := parent.sendPacketGroup(SourceId(NewId()), protocol.ProvideMode_Network, group, timeout)
		accepted.Store(ok)
		result <- ok
	}()
	return parent, result, func() {
		parent.cancel()
		synctest.Wait()
		<-done
		if !accepted.Load() {
			MessagePoolReturn(packet)
		}
		closeParent()
	}
}

// Tests either window independently: one sibling's empty state cannot delay
// a usable offer from the other, regardless of the window preference order.
func checkProviderWakeFirstOffer(t *testing.T, kind WindowType, version int, zeroPoll bool) {
	t.Helper()
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		parent, result, closeFixture := newProviderWakeSend(t, version, -1, func(settings *MultiClientSettings) {
			if zeroPoll {
				settings.FormationPollTimeout = 0
			}
		})
		defer closeFixture()
		synctest.Wait()
		started := time.Now()
		var accept atomic.Bool
		accept.Store(true)
		var calls atomic.Int64
		family := IpFamilyV4Only
		if version == 6 {
			family = IpFamilyV6Only
		}
		client := newProviderWakeChannel(parent.ctx, parent.settings, family, &accept, &calls)
		publishProviderWakeChannel(parent.windows[kind], client)
		synctest.Wait()
		select {
		case ok := <-result:
			if !ok || calls.Load() != 1 {
				t.Fatalf("ready offer admission=%t calls=%d", ok, calls.Load())
			}
		default:
			t.Fatal("ready provider left the first packet waiting on the formation timer")
		}
		if !time.Now().Equal(started) {
			t.Fatal("provider readiness consumed a timer")
		}
	})
}

func TestProviderFormationWakeQualityOffer(t *testing.T) {
	checkProviderWakeFirstOffer(t, WindowTypeQuality, 4, false)
}
func TestProviderFormationWakeSpeedOffer(t *testing.T) {
	checkProviderWakeFirstOffer(t, WindowTypeSpeed, 4, false)
}

func TestProviderFormationWakeIpv6Offer(t *testing.T) {
	checkProviderWakeFirstOffer(t, WindowTypeQuality, 6, false)
}

func TestProviderFormationWakeZeroPollStillUsesEvents(t *testing.T) {
	checkProviderWakeFirstOffer(t, WindowTypeQuality, 4, true)
}

// The actual initial-ping admission path must emit the edge, not merely the
// synthetic publisher above. Capture after ping entry to exclude setup events.
func TestProviderFormationWakeActualAdmissionPublishes(t *testing.T) {
	fixture := newMultiClientExpandLifecycleFixture(t)
	done := fixture.start()
	fixture.wait(t, "held initial ping", fixture.pingResultEntered)
	changed := fixture.window.resizeMonitor.NotifyChannel()
	fixture.releasePing()
	if got := fixture.result(t, done); got != 1 {
		t.Fatalf("admitted %d providers, want one", got)
	}
	select {
	case <-changed:
	default:
		t.Fatal("real provider admission did not publish a window readiness edge")
	}
}

// A canceled send joins promptly without any provider or synthetic timer.
func TestProviderFormationWakeCancellationControl(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		parent, result, closeFixture := newProviderWakeSend(t, 4, -1)
		defer closeFixture()
		synctest.Wait()
		parent.cancel()
		synctest.Wait()
		select {
		case ok := <-result:
			if ok {
				t.Fatal("canceled empty-window send was accepted")
			}
		default:
			t.Fatal("empty-window cancellation did not release sender")
		}
	})
}

// An explicit timeout still ends an unformed send; an event mechanism must not
// turn finite ownership into an unlimited wait when no provider ever arrives.
func TestProviderFormationWakeCallerDeadlineControl(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		_, result, closeFixture := newProviderWakeSend(t, 4, 11*time.Millisecond)
		defer closeFixture()
		synctest.Wait()
		<-time.After(11 * time.Millisecond)
		synctest.Wait()
		select {
		case ok := <-result:
			if ok {
				t.Fatal("empty-window deadline was accepted")
			}
		default:
			t.Fatal("empty-window sender overran its caller deadline")
		}
	})
}

// Candidate availability does not bypass the deliberate pause after an
// actual failed admission. An unrelated window wake cannot create a busy loop.
func TestProviderFormationWakePreservesFailedSendPacing(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		parent, result, closeFixture := newProviderWakeSend(t, 4, -1)
		defer closeFixture()
		synctest.Wait()
		var accept atomic.Bool
		var calls atomic.Int64
		client := newProviderWakeChannel(parent.ctx, parent.settings, IpFamilyV4Only, &accept, &calls)
		publishProviderWakeChannel(parent.windows[WindowTypeQuality], client)
		synctest.Wait()
		// Old code has not seen the first offer yet. Move to its formation
		// deadline so the pacing control is healthy on both implementations.
		if calls.Load() == 0 {
			<-time.After(time.Hour)
			synctest.Wait()
		}
		before := calls.Load()
		if before == 0 {
			t.Fatal("fixture never attempted its available provider")
		}
		accept.Store(true)
		parent.windows[WindowTypeQuality].resizeMonitor.NotifyAll()
		synctest.Wait()
		if calls.Load() != before {
			t.Fatal("window notification bypassed failed-send pacing")
		}
		select {
		case <-result:
			t.Fatal("failed-send cooldown completed before its timer")
		default:
		}
		<-time.After(2 * time.Hour)
		synctest.Wait()
		select {
		case ok := <-result:
			if !ok {
				t.Fatal("provider was not retried at its pacing deadline")
			}
		default:
			t.Fatal("failed-send pacing deadline did not retry")
		}
	})
}

// A time-expiring policy can change the selectable set without changing the
// window map. Preserve the bounded rescan as a fallback, not a readiness gate.
func TestProviderFormationWakeRetainsTimedRescanControl(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		parent, result, closeFixture := newProviderWakeSend(t, 4, -1)
		defer closeFixture()
		synctest.Wait()
		var accept atomic.Bool
		accept.Store(true)
		var calls atomic.Int64
		client := newProviderWakeChannel(parent.ctx, parent.settings, IpFamilyV4Only, &accept, &calls)
		window := parent.windows[WindowTypeQuality]
		window.stateLock.Lock()
		window.clients[client.ClientId()] = client
		window.stateLock.Unlock()
		<-time.After(time.Hour)
		synctest.Wait()
		select {
		case ok := <-result:
			if !ok {
				t.Fatal("timed rescan refused its provider")
			}
		default:
			t.Fatal("missing event removed the compatibility rescan fallback")
		}
	})
}
