//go:build !js

package connect

import (
	"context"

	"github.com/pion/datachannel"
	"github.com/pion/webrtc/v4"
)

func createWebRtcPeerConnection(ctx context.Context, active bool, settings *WebRtcSettings) (*webrtc.PeerConnection, error) {
	s := webrtc.SettingEngine{}
	s.LoggerFactory = &pionLoggerFactory{log: loggerOrDefault(settings.Log)}
	// bind ICE sockets to the physical egress interface so p2p does not loop
	// into the tunnel this process provides (R1); a no-op off Windows and when
	// no egress index is set.
	if index4, index6 := EgressInterfaceIndex(); index4 != 0 || index6 != 0 {
		if egressNet, err := newEgressNet(); err == nil {
			s.SetNet(egressNet)
		}
	}
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
