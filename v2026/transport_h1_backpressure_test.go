// Actual H1+ owners use pipe flow control and virtual time to distinguish
// temporary write pressure, terminal write timeout, and independent peer loss.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
)

// One completed payload parks the sole peer reader; later writes cannot make
// progress until the explicit gate opens. Its independent writer stays live.
func stopH1BackpressurePeerReads(t *testing.T, fixture *h1LivenessFixture) {
	t.Helper()
	fixture.send <- MessagePoolGet(64)
	synctest.Wait()
	select {
	case <-fixture.peerReadBlocked:
	default:
		t.Fatal("peer did not stop at the complete-message read boundary")
	}
	if fixture.payloads.Load() != 1 || fixture.client.writesInProgress.Load() != 0 {
		t.Fatal("initial payload did not finish before applying pressure")
	}
}

// Retains a second reference so the owner can be checked after terminal cleanup.
func blockH1BackpressurePayload(t *testing.T, fixture *h1LivenessFixture) []byte {
	t.Helper()
	message := MessagePoolGet(64)
	witness := MessagePoolShareReadOnly(message)
	fixture.send <- message
	synctest.Wait()
	if fixture.client.writesInProgress.Load() != 1 || fixture.payloads.Load() != 1 {
		t.Fatal("actual carrier writer did not block on the paused peer")
	}
	return witness
}

// Inbound heartbeats are explicitly independent of the backpressured writer.
func sendH1BackpressureHeartbeat(t *testing.T, fixture *h1LivenessFixture) {
	t.Helper()
	if err := fixture.peer.WriteMessage(websocket.BinaryMessage, nil); err != nil {
		t.Fatalf("healthy independent peer writer: %v", err)
	}
	synctest.Wait()
}

func TestH1WriteBackpressureResumesBeforeDeadline(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newH1LivenessFixtureWithBackpressure(t, true, 0)
		stopH1BackpressurePeerReads(t, fixture)
		witness := blockH1BackpressurePayload(t, fixture)
		time.Sleep(time.Second)
		sendH1BackpressureHeartbeat(t, fixture)
		fixture.assertConnected(t)
		if fixture.heartbeats.Load() != 0 || fixture.client.writesInProgress.Load() != 1 {
			t.Fatal("another writer bypassed the blocked payload")
		}
		close(fixture.peerReadResume)
		synctest.Wait()
		fixture.assertConnected(t)
		if fixture.payloads.Load() != 2 || fixture.client.writesInProgress.Load() != 0 || fixture.client.writeTimedOut.Load() {
			t.Fatal("cleared backpressure did not deliver the intact queued payload")
		}
		if !MessagePoolReturn(witness) {
			t.Fatal("successful payload retained the carrier's pooled reference")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		fixture.assertConnected(t)
		if fixture.heartbeats.Load() == 0 {
			t.Fatal("serialized heartbeat did not resume after payload progress")
		}
	})
}

// This is a healthy negative control, not a proposed timeout policy change:
// an expired partial/blocked framed write must retire the stream even when
// the reverse path is alive; replaying the frame on that stream is unsafe.
func TestH1WriteBackpressureTimeoutIsNotReadHeartbeatTimeout(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newH1LivenessFixtureWithBackpressure(t, true, 0)
		stopH1BackpressurePeerReads(t, fixture)
		witness := blockH1BackpressurePayload(t, fixture)
		for range 3 {
			time.Sleep(500 * time.Millisecond)
			sendH1BackpressureHeartbeat(t, fixture)
			fixture.assertConnected(t)
		}
		time.Sleep(500*time.Millisecond - time.Nanosecond)
		synctest.Wait()
		fixture.assertConnected(t)
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		fixture.assertWithdrawn(t)
		if !fixture.client.writeTimedOut.Load() || fixture.client.writesInProgress.Load() != 0 {
			t.Fatal("terminal boundary was not the exact writer deadline")
		}
		if !MessagePoolReturn(witness) {
			t.Fatal("write timeout retained the payload's pooled reference")
		}
	})
}

func TestH1WriteBackpressureHeartbeatWriteHasItsOwnBound(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newH1LivenessFixtureWithBackpressure(t, true, 0)
		stopH1BackpressurePeerReads(t, fixture)
		time.Sleep(time.Second)
		synctest.Wait()
		if fixture.client.writesInProgress.Load() != 1 || fixture.heartbeats.Load() != 0 {
			t.Fatal("the actual serialized heartbeat did not reach pipe pressure")
		}
		sendH1BackpressureHeartbeat(t, fixture)
		time.Sleep(time.Second)
		sendH1BackpressureHeartbeat(t, fixture)
		time.Sleep(time.Second - time.Nanosecond)
		synctest.Wait()
		fixture.assertConnected(t)
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		fixture.assertWithdrawn(t)
		if !fixture.client.writeTimedOut.Load() {
			t.Fatal("blocked heartbeat lacked the writer deadline")
		}
	})
}

func TestH1WriteBackpressureCancellationJoinsWithoutTimer(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newH1LivenessFixtureWithBackpressure(t, true, 0)
		stopH1BackpressurePeerReads(t, fixture)
		witness := blockH1BackpressurePayload(t, fixture)
		started := time.Now()
		if err := fixture.transport.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		fixture.assertWithdrawn(t)
		if !time.Now().Equal(started) || fixture.client.writesInProgress.Load() != 0 || fixture.client.writeTimedOut.Load() {
			t.Fatal("local cancellation waited for a timeout instead of joining I/O")
		}
		if !MessagePoolReturn(witness) {
			t.Fatal("canceled blocked writer retained its payload")
		}
	})
}

func TestH1WriteBackpressureDoesNotHideIndependentInboundBlackhole(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newH1LivenessFixtureWithBackpressure(t, true, 6*time.Second)
		stopH1BackpressurePeerReads(t, fixture)
		witness := blockH1BackpressurePayload(t, fixture)
		time.Sleep(3*time.Second - time.Nanosecond)
		synctest.Wait()
		fixture.assertConnected(t)
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		fixture.assertWithdrawn(t)
		if fixture.client.writeTimedOut.Load() || fixture.client.writesInProgress.Load() != 0 {
			t.Fatal("inbound timeout did not close and join the longer blocked writer")
		}
		if !MessagePoolReturn(witness) {
			t.Fatal("read timeout cleanup retained the blocked payload")
		}
	})
}
