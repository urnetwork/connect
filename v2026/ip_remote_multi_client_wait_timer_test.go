package connect

import (
	"context"
	"net"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Counts the timer objects allocated while run executes whose nearest caller,
// past the time package and the shared timer helper, is inside the named
// function. Unlike timerAllocationsUnder it leaves out the timers of the
// function's callees, so a loop that calls into other timed work can still be
// held to one timer of its own.
func timerAllocationsCalledFrom(t *testing.T, function string, run func()) int64 {
	t.Helper()
	timerAllocationProfileLock.Lock()
	defer timerAllocationProfileLock.Unlock()

	previousRate := runtime.MemProfileRate
	runtime.MemProfileRate = 1
	defer func() {
		runtime.MemProfileRate = previousRate
	}()

	// the profile reports allocations as of the most recent completed cycle
	runtime.GC()
	runtime.GC()
	before := timerAllocationsCalledFromProfile(function)
	run()
	runtime.GC()
	runtime.GC()
	return timerAllocationsCalledFromProfile(function) - before
}

func timerAllocationsCalledFromProfile(function string) int64 {
	n, _ := runtime.MemProfile(nil, true)
	records := make([]runtime.MemProfileRecord, n+256)
	n, ok := runtime.MemProfile(records, true)
	if !ok {
		n = len(records)
	}
	total := int64(0)
	for _, record := range records[:n] {
		timer := false
		frames := runtime.CallersFrames(record.Stack())
		for {
			frame, more := frames.Next()
			switch frame.Function {
			case "time.NewTimer", "time.After", "time.AfterFunc", "time.newTimer", "time.NewTicker",
				"github.com/urnetwork/connect/v2026.resetOrCreateTimer":
				timer = true
			default:
				if timer {
					if strings.Contains(frame.Function, function) {
						total += record.AllocObjects
					}
					more = false
				}
			}
			if !more {
				break
			}
		}
	}
	return total
}

// The resize loop waits for its next pass on one timer for its life.
func TestResizeWaitsOnOneTimer(t *testing.T) {
	settings := DefaultMultiClientSettings()
	settings.WindowResizeTimeout = 20 * time.Millisecond
	settings.WindowExpandTimeout = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	newMultiClientWindow(
		ctx,
		cancel,
		&testingEmptyMultiClientGenerator{},
		nil,
		nil,
		false,
		nil,
		DisableSecurityPolicy(),
		nil,
		nil,
		WindowTypeQuality,
		nil,
		settings,
		func() *ReliabilitySettings { return ReliabilitySettingsFrom(settings) },
		nil,
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
	time.Sleep(5 * settings.WindowResizeTimeout)

	passes := 50
	timers := timerAllocationsCalledFrom(t, "(*multiClientWindow).resize", func() {
		time.Sleep(time.Duration(passes) * settings.WindowResizeTimeout)
	})
	if 4 < timers {
		t.Fatalf("the resize loop allocated %d timer objects over ~%d passes, want one timer for its life", timers, passes)
	}
}

// A send that waits for an exit to offer re-arms one timer for each wait.
func TestSendRetryWaitsOnOneTimer(t *testing.T) {
	// the shared pools start their workers outside the bubble
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		parent, _, closeParent := groupTestParent(t, DisableSecurityPolicy())
		defer closeParent()
		formationPoll := parent.reliabilitySettings().FormationPollTimeout
		sendTimeout := 20 * formationPoll

		path := &IpPath{
			Version:         4,
			Protocol:        IpProtocolUdp,
			SourceIp:        net.ParseIP("192.0.2.10"),
			SourcePort:      40000,
			DestinationIp:   net.ParseIP("198.51.100.1"),
			DestinationPort: 443,
		}
		group := requireGroupTestPacketGroup(t, MessagePoolCopy(ipOosUdpPacket(path, []byte{1})))
		accepted := true
		timers := timerAllocationsCalledFrom(t, "(*RemoteUserNatMultiClient).sendParsedPacketGroupAttempt", func() {
			accepted = parent.sendPacketGroup(SourceId(NewId()), protocol.ProvideMode_Network, group, sendTimeout)
		})
		if accepted {
			t.Fatal("a send with no exit to offer was accepted")
		}
		for _, packet := range group.packets {
			MessagePoolReturn(packet)
		}
		if 4 < timers {
			t.Fatalf("a send waiting %s for an exit allocated %d timer objects at a %s formation poll, want one timer", sendTimeout, timers, formationPoll)
		}
	})
}

// A bare window whose discovery is deferred until `notBefore`.
func newDeferredDiscoveryTestWindow(ctx context.Context, notBefore time.Time) *multiClientWindow {
	window := &multiClientWindow{
		ctx:                  ctx,
		settings:             DefaultMultiClientSettings(),
		generator:            &testingEmptyMultiClientGenerator{},
		resizeMonitor:        NewMonitor(),
		networkChangeMonitor: NewMonitor(),
		clientChannelArgs:    make(chan *multiClientChannelArgs),
	}
	window.discoveryRetry.notBefore = notBefore
	return window
}

// The enumerator's wait for a deferred discovery window re-arms one timer
// across the resize wakes that end each wait early.
func TestDiscoveryDeferralWaitsOnOneTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		deferral := time.Minute
		window := newDeferredDiscoveryTestWindow(ctx, time.Now().Add(deferral))

		wakes := 20
		ended := make(chan bool, 1)
		timers := timerAllocationsCalledFrom(t, "(*multiClientWindow).waitDiscoveryExpandDeferral", func() {
			go func() {
				ended <- window.waitDiscoveryExpandDeferral()
			}()
			for range wakes {
				time.Sleep(deferral / time.Duration(2*wakes))
				window.resizeMonitor.NotifyAll()
				synctest.Wait()
			}
			time.Sleep(deferral)
			synctest.Wait()
		})
		select {
		case ok := <-ended:
			if !ok {
				t.Fatal("the deferral wait reported the window closed")
			}
		default:
			t.Fatal("the deferral wait did not end when the deferral did")
		}
		if 4 < timers {
			t.Fatalf("the deferral wait allocated %d timer objects over %d wakes, want one timer", timers, wakes)
		}
	})
}

// A minted candidate held through a deferral waits on one timer, and is
// retired one resize interval after the deferral ends.
func TestDiscoveryOfferWaitsOnOneTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		deferral := time.Minute
		window := newDeferredDiscoveryTestWindow(ctx, time.Now().Add(deferral))

		offered := make(chan bool, 1)
		start := time.Now()
		var endTime time.Time
		timers := timerAllocationsCalledFrom(t, "(*multiClientWindow).offerClientArgs", func() {
			go func() {
				ok := window.offerClientArgs(&multiClientChannelArgs{}, start.Add(time.Second))
				endTime = time.Now()
				offered <- ok
			}()
			time.Sleep(deferral + 2*window.settings.WindowResizeTimeout)
			synctest.Wait()
		})
		select {
		case ok := <-offered:
			if ok {
				t.Fatal("a candidate nobody took was reported taken")
			}
		default:
			t.Fatal("the held candidate was not retired after the deferral")
		}
		if want := start.Add(deferral + window.settings.WindowResizeTimeout); !endTime.Equal(want) {
			t.Fatalf("the held candidate was retired at %v, want one resize interval after the deferral, %v", endTime, want)
		}
		if 4 < timers {
			t.Fatalf("the held candidate allocated %d timer objects through the deferral, want one timer", timers)
		}
	})
}
