//go:build !js

// These controls pin terminal replies and bounded duplicate setup admission.
package connect

import (
	"testing"

	"github.com/pion/rtp"
)

// A reply retains the legacy payload, marks the local receiver ready, and
// never requests another response that could create an idle ping-pong.
func TestWebRtcFastPathWarmupReplyDoesNotRequestAnotherReply(t *testing.T) {
	fixture := newP2pFastPathWarmupFixture(t, func(packet rtp.Packet) {
		t.Error("receive worker wrote a response synchronously")
	})
	fixture.receive(t, rtp.Packet{
		Header:  rtp.Header{Version: 2, Marker: true},
		Payload: []byte{'U', 'R', 'W', p2pFastPathVersion},
	})
	waitP2pFastPathTestReads(t, fixture.reader, 2)
	if !fixture.peer.FastPathReady() {
		t.Fatal("marked legacy warmup payload did not make the receiver ready")
	}
	select {
	case <-fixture.fastPath.warmupRequests:
		t.Fatal("warmup reply requested another reply")
	default:
	}
	if dropCount := fixture.peer.settings.DataPlaneStats.Snapshot().FastDropCount; dropCount != 0 {
		t.Fatalf("warmup reply counted as a data drop: %d", dropCount)
	}
}

// Duplicate requests consume one signal slot while their writer is unavailable.
func TestWebRtcFastPathWarmupCoalescesDuplicateRequests(t *testing.T) {
	fixture := newP2pFastPathWarmupFixture(t, func(packet rtp.Packet) {
		t.Error("receive worker wrote a response synchronously")
	})
	for range 4 {
		fixture.receive(t, rtp.Packet{
			Header:  rtp.Header{Version: 2},
			Payload: []byte{'U', 'R', 'W', p2pFastPathVersion},
		})
	}
	waitP2pFastPathTestReads(t, fixture.reader, 5)
	select {
	case <-fixture.fastPath.warmupRequests:
	default:
		t.Fatal("warmup request did not signal its writer")
	}
	select {
	case <-fixture.fastPath.warmupRequests:
		t.Fatal("duplicate warmup requests retained multiple responses")
	default:
	}
}
