//go:build js

package connect

import (
	"context"
	"fmt"

	"github.com/pion/datachannel"
	"github.com/pion/webrtc/v4"
)

func createWebRtcPeerConnection(ctx context.Context, active bool, settings *WebRtcSettings) (*webrtc.PeerConnection, error) {
	s := webrtc.SettingEngine{}

	api := webrtc.NewAPI(webrtc.WithSettingEngine(s))
	return api.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			webrtc.ICEServer{
				URLs: settings.IceServerUrls,
			},
		},
	})
}

func detachWithDeadline(dc *webrtc.DataChannel) (datachannel.ReadWriteCloserDeadliner, error) {
	// FIXME translate from callbacks to a net.Conn
	return nil, fmt.Errorf("Not yet supported")
}
