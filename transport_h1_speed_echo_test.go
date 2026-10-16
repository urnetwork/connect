// The platform's synthetic speed test runs over the real H1+ owner against one
// in-memory peer. Pipe flow control stands in for an uplink slower than the
// downlink. synctest barriers order every step, and its virtual clock places
// the stall inside or exactly at the writer's deadline.
package connect

import (
	"bytes"
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
)

// The platform's probe chunk size.
const h1SpeedTestProbeByteCount = 1024

// The H1+ liveness owner with the production echo queue depth. Its peer stops
// reading after its first complete message, the start echo.
func newH1SpeedTestFixture(t *testing.T) (*h1LivenessFixture, int) {
	t.Helper()
	queueSize := DefaultPlatformTransportSettings().TransportBufferSize
	fixture := newH1LivenessFixtureWithBackpressure(t, true, 0, func(settings *PlatformTransportSettings) {
		settings.TransportBufferSize = queueSize
	})
	return fixture, queueSize
}

// Probe chunks that differ from each other, so the order of their echoes is
// visible. Each is filled with its index, so the count must stay below 256.
func h1SpeedTestProbeMessages(t *testing.T, probeCount int) [][]byte {
	t.Helper()
	if 256 <= probeCount {
		t.Fatalf("%d probes cannot each have their own fill byte", probeCount)
	}
	probeMessages := make([][]byte, probeCount)
	for index := range probeMessages {
		probeMessages[index] = bytes.Repeat([]byte{byte(index)}, h1SpeedTestProbeByteCount)
	}
	return probeMessages
}

// The index of the first message that differs between the two runs, or -1
// when they are equal.
func firstMessageMismatch(gotMessages [][]byte, wantMessages [][]byte) int {
	for index := range min(len(gotMessages), len(wantMessages)) {
		if !bytes.Equal(gotMessages[index], wantMessages[index]) {
			return index
		}
	}
	if len(gotMessages) != len(wantMessages) {
		return min(len(gotMessages), len(wantMessages))
	}
	return -1
}

// Starts a speed test, waits until its start echo is the peer's last read, and
// writes the probes from a separate peer writer. The returned channel yields
// how many probes the client reader took.
func startH1SpeedTestOnStalledUplink(
	t *testing.T,
	fixture *h1LivenessFixture,
	startMessage []byte,
	probeMessages [][]byte,
) <-chan int {
	t.Helper()
	if err := fixture.peer.WriteMessage(websocket.BinaryMessage, startMessage); err != nil {
		t.Fatal(err)
	}
	synctest.Wait()
	select {
	case <-fixture.peerReadBlocked:
	default:
		t.Fatal("the start echo did not reach the peer before it stopped reading")
	}
	probesWritten := make(chan int, 1)
	go func() {
		writtenCount := 0
		for _, probeMessage := range probeMessages {
			if fixture.peer.WriteMessage(websocket.BinaryMessage, probeMessage) != nil {
				break
			}
			writtenCount += 1
		}
		probesWritten <- writtenCount
	}()
	synctest.Wait()
	return probesWritten
}

// The platform sends a start control, a run of probe frames and a stop
// control, and times the round trip of exactly the bytes it sent. The client
// echoes each one through the H1 writer's bounded control queue. On a path
// whose uplink is slower than its downlink the reader fills that queue while
// the writer is still making progress. A full echo queue is backpressure, not
// a dead peer: the generation must stay up, every echo must return in full and
// in order, and frames after the test must be delivered rather than lost with
// a closed connection.
func TestH1SpeedTestEchoBackpressureKeepsGenerationAndFrames(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture, queueSize := newH1SpeedTestFixture(t)
		reader := fixture.routes.OpenMultiRouteReader(DestinationId(NewId()))
		defer fixture.routes.CloseMultiRouteReader(reader)

		startMessage := []byte{TransportControlSpeedStart, 0, 0, 0, 7}
		stopMessage := []byte{TransportControlSpeedStop, 0, 0, 0, 7}
		// more probes than the blocked writer, its queue and the reader hold
		probeCount := 2*queueSize + 16
		probeMessages := h1SpeedTestProbeMessages(t, probeCount)
		probesWritten := startH1SpeedTestOnStalledUplink(t, fixture, startMessage, probeMessages)
		// the uplink stays stalled for less than the writer's deadline
		time.Sleep(fixture.transport.settings.WriteTimeout / 2)
		synctest.Wait()

		refusalCount := fixture.transport.ReceiveStats().H1ControlRefusalCount
		connected := fixture.transport.IsConnected()
		if refusalCount != 0 || !connected {
			t.Errorf("a full speed-test echo queue closed a healthy H1 generation: refusals=%d connected=%t, want 0 and true", refusalCount, connected)
		} else {
			select {
			case writtenCount := <-probesWritten:
				t.Fatalf("the reader took all %d probes without filling the echo queue; the test applied no backpressure", writtenCount)
			default:
			}
			if fixture.transport.ReceiveStats().H1ControlBackpressureMessageCount == 0 {
				t.Fatal("the stalled reader held no echo as H1 control backpressure")
			}
		}

		close(fixture.peerReadResume)
		writtenCount := <-probesWritten
		stopErr := fixture.peer.WriteMessage(websocket.BinaryMessage, stopMessage)
		synctest.Wait()
		frameMessage := bytes.Repeat([]byte{0x5a}, 64)
		frameErr := fixture.peer.WriteMessage(websocket.BinaryMessage, frameMessage)
		synctest.Wait()

		// zero timeout: take only a frame the route already holds
		receivedMessage, readErr := reader.Read(context.Background(), 0)
		delivered := readErr == nil && bytes.Equal(receivedMessage, frameMessage)
		if receivedMessage != nil {
			MessagePoolReturn(receivedMessage)
		}
		echoCount := fixture.payloads.Load()
		echoByteCount := fixture.payloadByteCount.Load()
		wantEchoCount := int64(1 + probeCount + 1)
		wantEchoByteCount := int64(len(startMessage) + probeCount*h1SpeedTestProbeByteCount + len(stopMessage))
		if writtenCount != probeCount || stopErr != nil || frameErr != nil ||
			echoCount != wantEchoCount || echoByteCount != wantEchoByteCount || !delivered {
			t.Fatalf(
				"speed test lost frames: probes written %d/%d, stop error %v, echoes %d/%d messages and %d/%d bytes, later frame error %v delivered %t",
				writtenCount, probeCount, stopErr, echoCount, wantEchoCount, echoByteCount, wantEchoByteCount, frameErr, delivered,
			)
		}
		// the platform times exactly the messages it sent, in the order it
		// sent them, ending with the stop echo
		wantMessages := append(append([][]byte{startMessage}, probeMessages...), stopMessage)
		echoMessages := fixture.payloadMessagesSnapshot()
		if index := firstMessageMismatch(echoMessages, wantMessages); 0 <= index {
			t.Fatalf("echo %d of %d is not what the platform sent there (%d echoes)", index, len(wantMessages), len(echoMessages))
		}
		fixture.assertConnected(t)
		if refusalCount := fixture.transport.ReceiveStats().H1ControlRefusalCount; refusalCount != 0 {
			t.Fatalf("speed test refused %d echoes", refusalCount)
		}
	})
}

// The echo wait has no timer of its own; the writer's deadline bounds it. An
// uplink that makes no progress for that whole deadline is a dead connection,
// so the generation still ends, exactly at the writer deadline and not before.
func TestH1SpeedTestEchoStalledWriterEndsGenerationAtWriteDeadline(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture, queueSize := newH1SpeedTestFixture(t)
		writeTimeout := fixture.transport.settings.WriteTimeout
		probeCount := 2*queueSize + 16
		probesWritten := startH1SpeedTestOnStalledUplink(
			t,
			fixture,
			[]byte{TransportControlSpeedStart, 0, 0, 0, 9},
			h1SpeedTestProbeMessages(t, probeCount),
		)
		if !fixture.transport.IsConnected() {
			t.Fatal("a full speed-test echo queue closed the generation before the writer's deadline")
		}
		if fixture.client.writesInProgress.Load() != 1 {
			t.Fatal("the echo writer did not block on the stalled uplink")
		}
		if fixture.transport.ReceiveStats().H1ControlBackpressureMessageCount == 0 {
			t.Fatal("the stalled reader held no echo as H1 control backpressure")
		}

		time.Sleep(writeTimeout - time.Nanosecond)
		synctest.Wait()
		fixture.assertConnected(t)
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		fixture.assertWithdrawn(t)
		if !fixture.client.writeTimedOut.Load() || fixture.client.writesInProgress.Load() != 0 {
			t.Fatal("the generation did not end at the echo writer's deadline")
		}
		if writtenCount := <-probesWritten; probeCount <= writtenCount {
			t.Fatalf("the stalled reader took all %d probes", writtenCount)
		}
		if refusalCount := fixture.transport.ReceiveStats().H1ControlRefusalCount; refusalCount != 0 {
			t.Fatalf("the stalled speed test refused %d echoes", refusalCount)
		}
	})
}
