// Additional controls for an enrolled probe's wire domain and owner lifetime.
package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Synthetic, distinct control-key directions for two test endpoints.
func p2pForwardProbeTestKeys(destination bool) p2pStreamReadinessKeys {
	keys := p2pStreamReadinessKeys{
		transcript: [32]byte{11}, sendKey: [32]byte{22}, receiveKey: [32]byte{33}, destination: destination,
		expirationTimeUnixMillis: time.Now().Add(time.Hour).UnixMilli(),
	}
	if destination {
		keys.sendKey, keys.receiveKey = keys.receiveKey, keys.sendKey
	}
	return keys
}

// Attaches the same owner and key-install transition as the stream enrollment.
func attachP2pForwardProbeTestOwner(ctx context.Context, probe *p2pStreamProbe, destination bool) (func() bool, context.CancelFunc) {
	ownerCtx, revoke := context.WithCancel(ctx)
	owner := newP2pStreamReadiness(ownerCtx)
	probe.forwardReadiness = owner
	keys := p2pForwardProbeTestKeys(destination)
	return func() bool { return owner.install(keys) }, revoke
}

// The original direct sibling still uses its stable wire version. Enrollment
// authentication covers the entire fixed header, purpose, stream and direction.
func TestP2pForwardReadinessControlBinding(t *testing.T) {
	source, destination := p2pForwardProbeTestKeys(false), p2pForwardProbeTestKeys(true)
	streamId, nonce := NewId(), NewId()
	wire := encodeP2pReadinessProbe(&source, p2pStreamProbeRequestType, streamId, nonce)
	defer MessagePoolReturn(wire)
	if len(wire) != p2pForwardProbeByteCount || !isP2pStreamProbe(wire) ||
		!destination.verifies(wire) || source.verifies(wire) {
		t.Fatal("control did not retain the enrolled stream direction")
	}
	other := destination
	other.transcript[0]++
	if other.verifies(wire) {
		t.Fatal("control matched another enrollment transcript")
	}
	for _, offset := range []int{len(p2pStreamProbePrefix) - 1, p2pStreamProbeTypeOffset, p2pStreamProbeStreamOffset,
		p2pStreamProbeNonceOffset, p2pStreamProbeByteCount} {
		wire[offset] ^= 1
		if destination.verifies(wire) {
			t.Errorf("control change at offset %d retained its acknowledgement", offset)
		}
		wire[offset] ^= 1
	}
	direct := encodeP2pReadinessProbe(nil, p2pStreamProbeRequestType, streamId, nonce)
	defer MessagePoolReturn(direct)
	if len(direct) != p2pStreamProbeByteCount || !isP2pStreamProbe(direct) || destination.verifies(direct) {
		t.Fatal("direct compatibility envelope crossed the enrollment boundary")
	}
}

// The pending owner is quiescent. A replaced raw route gets a fresh challenge;
// a valid response for the old generation cannot publish its successor.
func TestP2pForwardReadinessPendingRetirementAndReconnect(t *testing.T) {
	ownedTestWireSynctest(t, func(t *testing.T) {
		streamId := NewId()
		source := newP2pForwardProbeTestEndpoint(t, streamId, NewId(), false)
		edge := make(chan struct{})
		release := make(chan struct{})
		releaseTimer := sync.OnceFunc(func() { close(release) })
		source.probe.testingAfterProbeTimerEdge = func() {
			close(edge)
			<-release
		}
		go source.probe.run()
		defer source.close()
		defer releaseTimer()
		synctest.Wait()
		select {
		case initial := <-source.requests:
			MessagePoolReturn(initial)
			t.Fatal("pending enrollment emitted a challenge")
		default:
		}

		if !source.install() || !source.install() {
			t.Fatal("same accepted enrollment failed its one-time installation")
		}
		other := p2pForwardProbeTestKeys(false)
		other.transcript[0]++
		if source.probe.forwardReadiness.install(other) {
			t.Fatal("existing owner accepted a different enrollment")
		}
		synctest.Wait()
		request := takeP2pForwardProbeControl(t, source.requests, "enrolled challenge")
		defer MessagePoolReturn(request)
		_, _, _, firstNonce := decodeP2pStreamProbe(request)
		direct := encodeP2pStreamProbe(p2pStreamProbeResponseType, streamId, firstNonce)
		defer MessagePoolReturn(direct)
		deliverP2pForwardProbeControl(t, source.probe, direct)
		synctest.Wait()
		if len(source.writer.GetActiveRoutes()) != 0 || source.timerResets != 1 {
			t.Fatal("legacy echo promoted a pending forwarded writer")
		}
		destination := p2pForwardProbeTestKeys(true)
		response := encodeP2pReadinessProbe(&destination, p2pStreamProbeResponseType, streamId, firstNonce)
		defer MessagePoolReturn(response)
		source.probe.clearSendRoute(source.transport, source.transport.send)
		source.probe.setSendRoute(source.transport, source.transport.send)
		synctest.Wait()
		second := takeP2pForwardProbeControl(t, source.requests, "replacement challenge")
		defer MessagePoolReturn(second)
		_, _, _, secondNonce := decodeP2pStreamProbe(second)
		if secondNonce == firstNonce {
			t.Fatal("new physical route reused a retired challenge")
		}
		deliverP2pForwardProbeControl(t, source.probe, response)
		synctest.Wait()
		if len(source.writer.GetActiveRoutes()) != 0 || source.timerResets != 2 {
			t.Fatal("retired physical challenge published or renewed its replacement")
		}
		current := encodeP2pReadinessProbe(&destination, p2pStreamProbeResponseType, streamId, secondNonce)
		defer MessagePoolReturn(current)
		deliverP2pForwardProbeControl(t, source.probe, current)
		synctest.Wait()
		if len(source.writer.GetActiveRoutes()) != 1 || source.timerResets != 3 {
			t.Fatal("current generation response failed to establish eligibility")
		}
		// Deliver a duplicate at the timer edge. Even when both arms are
		// runnable, the elapsed lease uses its original last-response time.
		time.Sleep(source.probe.timeout + time.Second)
		source.ticks <- time.Now()
		<-edge
		deliverP2pForwardProbeControl(t, source.probe, current)
		releaseTimer()
		synctest.Wait()
		if len(source.writer.GetActiveRoutes()) != 0 {
			t.Fatal("duplicate at the expiry edge retained the stale writer")
		}
		renewal := takeP2pForwardProbeControl(t, source.requests, "expired lease challenge")
		defer MessagePoolReturn(renewal)
		_, _, _, renewalNonce := decodeP2pStreamProbe(renewal)
		renewed := encodeP2pReadinessProbe(&destination, p2pStreamProbeResponseType, streamId, renewalNonce)
		defer MessagePoolReturn(renewed)
		deliverP2pForwardProbeControl(t, source.probe, renewed)
		synctest.Wait()
		if len(source.writer.GetActiveRoutes()) != 1 {
			t.Fatal("fresh response did not restore expired eligibility")
		}
		source.revoke()
		synctest.Wait()
		if len(source.writer.GetActiveRoutes()) != 0 || source.install() {
			t.Fatal("revoked enrollment retained writer eligibility or accepted another installation")
		}
	})
}

// Revoking the enrollment while its ready bit is set stops the pending route
// update. The control path cannot subsequently report a granted writer.
func TestP2pForwardReadinessRevokedBeforePublication(t *testing.T) {
	ownedTestWireSynctest(t, func(t *testing.T) {
		streamId := NewId()
		source := newP2pForwardProbeTestEndpoint(t, streamId, NewId(), false)
		if !source.install() {
			t.Fatal("origin enrollment did not install")
		}
		entered := make(chan struct{})
		release := make(chan struct{})
		releasePublication := sync.OnceFunc(func() { close(release) })
		source.probe.testingAfterReadyBitSet = func(Transport, Route, uint64) {
			close(entered)
			<-release
		}
		var grants int
		source.probe.progressObserver = func(event TransferProgressEvent) {
			if event.Stage == "p2p_probe_"+P2pStreamProbeEventReadinessGranted {
				grants++
			}
		}
		go source.probe.run()
		defer source.close()
		defer releasePublication()
		synctest.Wait()
		request := takeP2pForwardProbeControl(t, source.requests, "origin challenge")
		defer MessagePoolReturn(request)
		_, _, _, nonce := decodeP2pStreamProbe(request)
		destination := p2pForwardProbeTestKeys(true)
		response := encodeP2pReadinessProbe(&destination, p2pStreamProbeResponseType, streamId, nonce)
		defer MessagePoolReturn(response)
		deliverP2pForwardProbeControl(t, source.probe, response)
		<-entered
		source.revoke()
		releasePublication()
		synctest.Wait()
		if source.transport.endToEndReady.Load() || len(source.writer.GetActiveRoutes()) != 0 || grants != 0 {
			t.Fatalf("retired enrollment published a pending readiness grant: grants=%d", grants)
		}
	})
}
