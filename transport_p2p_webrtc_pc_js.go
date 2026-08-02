//go:build js

package connect

import (
	"fmt"

	"github.com/pion/datachannel"
	"github.com/pion/webrtc/v4"
)

func newWebRtcPeerConnectionFactory(settings *WebRtcSettings) (*webRtcPeerConnectionFactory, error) {
	// The js/wasm SettingEngine wraps the browser's native WebRTC and exposes
	// none of the tuning the non-js build uses (LoggerFactory, SCTP/MTU/ICE
	// timeouts). Setting any of those here does not compile for GOOS=js.
	s := webrtc.SettingEngine{}

	api := webrtc.NewAPI(webrtc.WithSettingEngine(s))
	configuration := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: settings.IceServerUrls,
			},
		},
	}
	return &webRtcPeerConnectionFactory{
		newPeerConnection: func() (*webrtc.PeerConnection, error) {
			return api.NewPeerConnection(configuration)
		},
	}, nil
}

// Browsers do not expose association-level SCTP receive counters or aggregate
// in-flight bytes. The JS transport is callback-based and currently cannot
// detach a data channel into this net.Conn implementation, so disable the
// native association watchdog rather than pretending browser bufferedAmount
// has equivalent semantics.
func webRtcSctpProgress(*webrtc.PeerConnection) (bufferedAmount int, bytesReceived uint64, ok bool) {
	return 0, 0, false
}

func detachWithDeadline(dc *webrtc.DataChannel) (datachannel.ReadWriteCloserDeadliner, error) {
	// FIXME translate from callbacks to a net.Conn
	return nil, fmt.Errorf("Not yet supported")
}
