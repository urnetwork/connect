// The mobile H1 writer's ACK priority lane runs over the real H1+ owner
// against one in-memory peer. Pipe flow control holds the writer on a stalled
// uplink while the lane and the control queue fill, so the order in which the
// writer serves them after the stall is fixed by synctest barriers alone.
package connect

import (
	"bytes"
	"testing"
	"testing/synctest"

	"github.com/gorilla/websocket"
)

// The first byte of each message, a compact view of a run of messages.
func messageFirstBytes(messages [][]byte) []byte {
	firstBytes := make([]byte, 0, len(messages))
	for _, message := range messages {
		if 0 < len(message) {
			firstBytes = append(firstBytes, message[0])
		}
	}
	return firstBytes
}

// The writer served the ACK priority lane in a nonblocking pass ahead of the
// selects that drain the control queue, and went round again after every ACK
// batch. While the lane was ready each time the loop came round, as it is on a
// slow uplink that also carries acknowledgements for another carrier's
// traffic, a pending speed-test or latency echo never went out. The reader
// waits on a full control queue (offerH1Control), so during a speed test that
// froze H1 inbound. A pending control echo must go out ahead of the ACKs that
// are ready when the writer comes round.
func TestH1AckPriorityLaneDoesNotStarveControlEchoes(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newH1LivenessFixtureWithBackpressure(t, true, 0, func(settings *PlatformTransportSettings) {
			settings.TransportBufferSize = DefaultPlatformTransportSettings().TransportBufferSize
			// the mobile lane depth
			settings.H1AckPriorityBufferSize = 8
		})
		ackLane, ok := h1AckPriorityRoute(fixture.send)
		if !ok {
			t.Fatal("the mobile H1 carrier published no ACK priority lane")
		}

		// the first payload is the peer's last read; the second holds the
		// writer on the stalled uplink
		firstPayload := bytes.Repeat([]byte{0x01}, 64)
		blockedPayload := bytes.Repeat([]byte{0x02}, 64)
		fixture.sendMessage(t, MessagePoolCopy(firstPayload))
		synctest.Wait()
		select {
		case <-fixture.peerReadBlocked:
		default:
			t.Fatal("the peer did not stop reading after the first payload")
		}
		fixture.sendMessage(t, MessagePoolCopy(blockedPayload))
		synctest.Wait()
		if fixture.client.writesInProgress.Load() != 1 {
			t.Fatal("the writer did not block on the stalled uplink")
		}

		// while the writer is held, ACKs fill the lane and a latency echo
		// waits in the control queue
		ackMessages := make([][]byte, cap(ackLane))
		for index := range ackMessages {
			ackMessages[index] = bytes.Repeat([]byte{0x40 + byte(index)}, 24)
			message := MessagePoolCopy(ackMessages[index])
			if accepted, _ := fixture.writer.(*MultiRouteSelector).tryWriteH1AckPriorityWithCarrierPreference(message, TransportTypeH1); !accepted {
				MessagePoolReturn(message)
				t.Fatal("the ready acknowledgement lane refused selector admission")
			}
		}
		latencyProbe := bytes.Repeat([]byte{0x7f}, 16)
		if err := fixture.peer.WriteMessage(websocket.BinaryMessage, latencyProbe); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()

		// the uplink drains and the writer comes round with both ready
		close(fixture.peerReadResume)
		synctest.Wait()
		wantMessages := append([][]byte{firstPayload, blockedPayload, latencyProbe}, ackMessages...)
		gotMessages := fixture.payloadMessagesSnapshot()
		if index := firstMessageMismatch(gotMessages, wantMessages); 0 <= index {
			t.Fatalf("wire order % x, want % x: the latency echo (7f) must go out ahead of the ready ACKs",
				messageFirstBytes(gotMessages), messageFirstBytes(wantMessages))
		}
		fixture.assertConnected(t)
	})
}
