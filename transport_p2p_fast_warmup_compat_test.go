//go:build !js

// Pins the unchanged payload's acceptance in the original and current reader.
package connect

import (
	"testing"

	"github.com/pion/rtp"
)

// Older readers ignore the RTP marker bit and still accept this exact warmup
// payload. A response can therefore add the bit without changing wire versions.
func TestWebRtcFastPathWarmupMarkedPayloadRemainsCompatible(t *testing.T) {
	fixture := newP2pFastPathWarmupFixture(t, func(packet rtp.Packet) {
		t.Error("receive worker wrote a response synchronously")
	})
	fixture.receive(t, rtp.Packet{
		Header:  rtp.Header{Version: 2, Marker: true},
		Payload: []byte{'U', 'R', 'W', p2pFastPathVersion},
	})
	waitP2pFastPathTestReads(t, fixture.reader, 2)
	if !fixture.peer.FastPathReady() {
		t.Fatal("marked legacy payload did not make the carrier ready")
	}
	if dropCount := fixture.peer.settings.DataPlaneStats.Snapshot().FastDropCount; dropCount != 0 {
		t.Fatalf("marked legacy payload counted as a data drop: %d", dropCount)
	}
}
