//go:build !js

package connect

import (
	"slices"
	"testing"

	"github.com/pion/webrtc/v4"
)

// Root cause: the operator's webrtc probe proves the family an activation was
// posted for, and the extender's forward follows the family of the ICE pair
// it was reached on (A7); a probe that gathers both families can be joined on
// the other one and fail the family check for nothing (EXTENDER.md S8).
// Observable: a factory pinned to 4 or 6 gathers on udp of that family alone,
// and one pinned to anything else keeps Pion's default of both. That Pion
// gathers as the network types say is proved on loopback behind the
// urnetwork_webrtc_loopback tag (transport_p2p_webrtc_family_loopback_test.go).
func TestWebRtcIceNetworkTypesPinOneFamily(t *testing.T) {
	for _, c := range []struct {
		ipVersion int
		want      []webrtc.NetworkType
	}{
		{ipVersion: 4, want: []webrtc.NetworkType{webrtc.NetworkTypeUDP4}},
		{ipVersion: 6, want: []webrtc.NetworkType{webrtc.NetworkTypeUDP6}},
		{ipVersion: 0, want: nil},
		{ipVersion: 5, want: nil},
		{ipVersion: -4, want: nil},
	} {
		if got := webRtcIceNetworkTypes(c.ipVersion); !slices.Equal(got, c.want) {
			t.Errorf("ipv%d: network types = %v, want %v", c.ipVersion, got, c.want)
		}
	}
	if settings := DefaultWebRtcSettings(); webRtcIceNetworkTypes(settings.IceIpVersion) != nil {
		t.Errorf("the default settings pin ipv%d, want both families", settings.IceIpVersion)
	}
}
