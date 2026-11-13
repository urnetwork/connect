//go:build !js && urnetwork_webrtc_loopback

package connect

// Real Pion gathering on loopback, behind its own tag since it binds sockets
// and waits on Pion's gatherer: go test -tags urnetwork_webrtc_loopback -run
// TestWebRtcPeerConnectionFactoryGathersOnlyThePinnedFamily . The family
// mapping it proves is tested without the tag
// (transport_p2p_webrtc_family_test.go).

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

// The families of the host candidates one factory gathers on loopback, with
// no STUN, under settings pinned to ipVersion.
func testGatheredCandidateFamilies(t *testing.T, ipVersion int) (ipv4 bool, ipv6 bool) {
	t.Helper()
	settings := DefaultWebRtcSettings()
	settings.Log = NewNoopLogger()
	settings.UseLoopbackOnlyIceInterfaces = true
	settings.IceServerUrls = nil
	settings.IceServerPoolUrls = nil
	settings.IceIpVersion = ipVersion
	factory, _, err := newWebRtcPeerConnectionFactory(settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	peerConnection, cancelResolve, err := factory.newPeerConnection(false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = peerConnection.Close()
		cancelResolve()
	}()
	if _, err := peerConnection.CreateDataChannel("test", nil); err != nil {
		t.Fatal(err)
	}
	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)
	if err := peerConnection.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gatherComplete:
	case <-time.After(30 * time.Second):
		t.Fatal("gathering did not complete")
	}
	for _, line := range strings.Split(peerConnection.LocalDescription().SDP, "\n") {
		candidateLine, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "a=")
		if !ok || !strings.HasPrefix(candidateLine, "candidate:") {
			continue
		}
		candidate, err := ice.UnmarshalCandidate(candidateLine)
		if err != nil {
			t.Fatalf("candidate %q: %v", candidateLine, err)
		}
		addr, err := netip.ParseAddr(candidate.Address())
		if err != nil {
			t.Fatalf("candidate address %q: %v", candidate.Address(), err)
		}
		if addr.Unmap().Is4() {
			ipv4 = true
		} else {
			ipv6 = true
		}
	}
	return
}

// Root cause: the operator's webrtc probe proves the family an activation was
// posted for, and the extender's forward follows the family of the ICE pair
// it was reached on (A7); a probe that gathers both families can be joined on
// the other one and fail the family check for nothing (EXTENDER.md S8).
// Observable: unpinned, the loopback factory gathers the ipv4 loopback; pinned
// to 4 it still does and gathers no ipv6; pinned to 6 it gathers no ipv4.
// Pion gathers no ipv6 loopback candidate, so the pin to 6 is proved by the
// family it leaves out.
func TestWebRtcPeerConnectionFactoryGathersOnlyThePinnedFamily(t *testing.T) {
	if ipv4, _ := testGatheredCandidateFamilies(t, 0); !ipv4 {
		t.Fatalf("unpinned: no ipv4 loopback candidate, so the pins below prove nothing")
	}
	if ipv4, ipv6 := testGatheredCandidateFamilies(t, 4); !ipv4 || ipv6 {
		t.Errorf("pinned to 4: gathered ipv4=%t ipv6=%t, want ipv4 alone", ipv4, ipv6)
	}
	if ipv4, _ := testGatheredCandidateFamilies(t, 6); ipv4 {
		t.Errorf("pinned to 6: gathered an ipv4 candidate")
	}
}
