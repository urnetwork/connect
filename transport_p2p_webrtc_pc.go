//go:build !js

package connect

import (
	"context"

	"github.com/pion/datachannel"
	"github.com/pion/webrtc/v4"
)

func createWebRtcPeerConnection(ctx context.Context, active bool, settings *WebRtcSettings) (*webrtc.PeerConnection, error) {
	s := webrtc.SettingEngine{}
	s.DetachDataChannels()
	s.SetSCTPMaxReceiveBufferSize( /*16 * 1024 * 1024*/ uint32(settings.ReceiveBufferSize))
	s.SetReceiveMTU( /*16384*/ uint(settings.ReceiveMtu))
	s.SetICETimeouts(
		settings.DisconnectedTimeout,
		settings.FailedTimeout,
		settings.KeepAliveTimeout,
	)

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
	return dc.DetachWithDeadline()
}
