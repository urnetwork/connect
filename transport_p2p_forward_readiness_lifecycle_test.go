// A source and destination exchange their normal probes over bounded transport
// queues. The reverse writer is held at its actual route-publication seam.
package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// One real worker, route selector and pair of physical control queues.
type p2pForwardProbeTestEndpoint struct {
	probe       *p2pStreamProbe
	transport   *P2pSendTransport
	writer      MultiRouteWriter
	requests    Route
	responses   Route
	ticks       chan time.Time
	install     func() bool
	revoke      context.CancelFunc
	close       func()
	timerResets int
}

// Creates the ordinary physical probe fixture and defers start so the tests
// can attach an enrollment, deterministic timer, or publication barrier.
func newP2pForwardProbeTestEndpoint(t *testing.T, streamId Id, logicalPeer Id, destination bool) *p2pForwardProbeTestEndpoint {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	settings := DefaultP2pTransportSettings()
	settings.EndToEndProbeInterval = time.Second
	settings.EndToEndProbeTimeout = 3 * time.Second
	manager := NewRouteManager(ctx, "forward-readiness")
	route := make(Route, 4)
	transport := newP2pProbeTestSendTransport(NewId(), streamId, route, settings)
	transport.probeRequests = make(Route, 2)
	transport.probeResponses = make(Route, 2)
	manager.UpdateTransport(transport, []Route{route})
	removeAlias := manager.AddWriterDestinationAlias(DestinationId(logicalPeer), StreamId(streamId))
	writer := manager.OpenMultiRouteWriter(DestinationId(logicalPeer))
	probe := newStoppedP2pStreamProbe(ctx, manager, streamId, settings)
	install, revoke := attachP2pForwardProbeTestOwner(ctx, probe, destination)
	endpoint := &p2pForwardProbeTestEndpoint{
		probe: probe, transport: transport, writer: writer, requests: transport.probeRequests,
		responses: transport.probeResponses, ticks: make(chan time.Time, 1), install: install, revoke: revoke,
	}
	probe.testingProbeTimer = endpoint.ticks
	probe.testingAfterProbeTimerReset = func(time.Duration) { endpoint.timerResets++ }
	probe.setSendRoute(transport, route)
	endpoint.close = func() {
		probe.close()
		probe.clearSendRoute(transport, route)
		revoke()
		cancel()
		manager.CloseMultiRouteWriter(writer)
		removeAlias()
		for _, queue := range []Route{route, endpoint.requests, endpoint.responses} {
			for {
				select {
				case message := <-queue:
					MessagePoolReturn(message)
					continue
				default:
				}
				break
			}
		}
	}
	return endpoint
}

// Returns the caller-owned packet queued by the worker before the last
// synctest barrier. Failure points to the missing producer immediately.
func takeP2pForwardProbeControl(t *testing.T, queue Route, purpose string) []byte {
	t.Helper()
	select {
	case message := <-queue:
		return message
	default:
		t.Fatalf("missing %s in its physical control queue", purpose)
		return nil
	}
}

// Checks synchronous handling while the sending side retains the supplied
// observation. The physical reader returns it after its borrowed use.
func deliverP2pForwardProbeControl(t *testing.T, receiver *p2pStreamProbe, message []byte) {
	t.Helper()
	if !receiver.handle(message) {
		t.Fatal("stream endpoint did not recognize its peer's readiness control")
	}
}

// Moves every response already emitted by the destination across the physical
// link. Any resulting source promotion is visible at the synctest barrier.
func deliverQueuedP2pForwardProbeResponses(t *testing.T, from, to *p2pForwardProbeTestEndpoint) {
	t.Helper()
	for {
		select {
		case message := <-from.responses:
			deliverP2pForwardProbeControl(t, to.probe, message)
			MessagePoolReturn(message)
		default:
			synctest.Wait()
			return
		}
	}
}

// The origin must observe the destination's published reverse writer before
// a matching readiness response can expose its own logical destination route.
func TestP2pForwardReadinessWaitsForPublishedReverseWriter(t *testing.T) {
	ownedTestWireSynctest(t, func(t *testing.T) {
		streamId := NewId()
		source := newP2pForwardProbeTestEndpoint(t, streamId, NewId(), false)
		destination := newP2pForwardProbeTestEndpoint(t, streamId, NewId(), true)
		if !source.install() || !destination.install() {
			t.Fatal("endpoint enrollment was not installed")
		}
		entered := make(chan struct{})
		release := make(chan struct{})
		releasePublication := sync.OnceFunc(func() { close(release) })
		destination.probe.testingAfterReadyBitSet = func(Transport, Route, uint64) {
			close(entered)
			<-release
		}
		go source.probe.run()
		go destination.probe.run()
		defer source.close()
		defer destination.close()
		defer releasePublication()
		synctest.Wait()

		sourceRequest := takeP2pForwardProbeControl(t, source.requests, "origin challenge")
		defer MessagePoolReturn(sourceRequest)
		destinationRequest := takeP2pForwardProbeControl(t, destination.requests, "destination challenge")
		defer MessagePoolReturn(destinationRequest)

		// The destination received origin data and has enforced its enrollment.
		// Its own challenge has not reached the origin.
		deliverP2pForwardProbeControl(t, destination.probe, sourceRequest)
		deliverQueuedP2pForwardProbeResponses(t, destination, source)
		if got := len(source.writer.GetActiveRoutes()); got != 0 {
			t.Fatalf("origin published %d routes before destination verified and published its reverse writer", got)
		}

		deliverP2pForwardProbeControl(t, source.probe, destinationRequest)
		reverseResponse := takeP2pForwardProbeControl(t, source.responses, "origin response to destination")
		defer MessagePoolReturn(reverseResponse)
		deliverP2pForwardProbeControl(t, destination.probe, reverseResponse)
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("destination did not reach its reverse writer publication")
		}
		if len(destination.writer.GetActiveRoutes()) != 0 {
			t.Fatal("reverse writer published while its route update was paused")
		}
		deliverP2pForwardProbeControl(t, destination.probe, sourceRequest)
		deliverQueuedP2pForwardProbeResponses(t, destination, source)
		if got := len(source.writer.GetActiveRoutes()); got != 0 {
			t.Fatalf("origin published %d routes during destination writer registration", got)
		}

		releasePublication()
		synctest.Wait()
		if len(destination.writer.GetActiveRoutes()) != 1 || len(source.writer.GetActiveRoutes()) != 0 {
			t.Fatal("destination reverse publication did not precede origin readiness")
		}
		deliverP2pForwardProbeControl(t, destination.probe, sourceRequest)
		forwardResponse := takeP2pForwardProbeControl(t, destination.responses, "published destination response")
		defer MessagePoolReturn(forwardResponse)
		deliverP2pForwardProbeControl(t, source.probe, forwardResponse)
		synctest.Wait()
		if len(source.writer.GetActiveRoutes()) != 1 {
			t.Fatal("origin did not publish after verified destination response")
		}

		resets := source.timerResets
		deliverP2pForwardProbeControl(t, source.probe, forwardResponse)
		synctest.Wait()
		if source.timerResets != resets {
			t.Fatal("duplicate response renewed the origin lease")
		}
	})
}
