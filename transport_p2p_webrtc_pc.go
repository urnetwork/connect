//go:build !js

package connect

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net"
	"strings"
	"sync"

	"github.com/pion/datachannel"
	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

var iceInterfacesLogOnce sync.Once

// logIceInterfaces dumps what the Go runtime's net.Interfaces() sees for
// ICE candidate gathering. On mobile this is the ground truth for the "0 host
// candidates" failure: if the physical egress (e.g. wlan0) is missing or has no
// usable unicast address, pion gathers nothing. It is process-once as well as
// V(1)-gated: logging every interface for every churned peer connection was
// itself a setup/CPU cost and rapidly displaced useful ICE evidence.
func logIceInterfaces(log Logger) {
	if !log.V(1).Enabled() {
		return
	}
	iceInterfacesLogOnce.Do(func() {
		ifaces, err := net.Interfaces()
		if err != nil {
			log.Infof("[ice-if]net.Interfaces err = %s\n", err)
			return
		}
		for _, ifc := range ifaces {
			addrs, _ := ifc.Addrs()
			strs := make([]string, 0, len(addrs))
			for _, a := range addrs {
				strs = append(strs, a.String())
			}
			log.Infof("[ice-if]%s flags=%s addrs=[%s]\n", ifc.Name, ifc.Flags.String(), strings.Join(strs, ","))
		}
	})
}

// newWebRtcPeerConnectionFactory builds immutable manager-scoped Pion state
// once. This transport is data-channel-only, so it needs neither the default
// media codecs nor RTP interceptors. A caller-provided certificate is also
// intentionally shared across the manager's peer connections, avoiding a new
// P-256 key and X.509 certificate for every retry.
func newWebRtcPeerConnectionFactory(settings *WebRtcSettings) (*webRtcPeerConnectionFactory, error) {
	s := webrtc.SettingEngine{}
	s.LoggerFactory = &pionLoggerFactory{log: loggerOrDefault(settings.Log)}
	logIceInterfaces(loggerOrDefault(settings.Log))
	// bind ICE sockets to the physical egress interface so p2p does not loop
	// into the tunnel this process provides (R1); a no-op off Windows and when
	// no egress index is set.
	if index4, index6 := EgressInterfaceIndex(); index4 != 0 || index6 != 0 {
		if egressNet, err := newEgressNet(); err == nil {
			s.SetNet(egressNet)
		}
	} else if iceNet, ok := newIceInterfaceNet(
		loggerOrDefault(settings.Log),
		settings.UseEgressOnlyIceInterfaces,
	); ok {
		// Android (API 30+) denies netlink, so pion's default net.Interfaces()
		// gathering yields zero host candidates and p2p never leaves the WAN
		// relay. Device clients also opt into the same egress-only view on
		// platforms that can enumerate: a macOS host may expose many utun,
		// bridge, AWDL, and VM addresses, and ICE checks their local×remote
		// cross-product for every peer connection. One current IPv4/IPv6 pair
		// is both the usable path and a bounded setup cost.
		// See OPTIMIZENETWORKPEER1.md §5.1.
		s.SetNet(iceNet)
	}
	s.DetachDataChannels()
	if settings.EnableSctpSnap {
		// SNAP is advertised as an optional SDP attribute. Two enabled peers
		// start from the exchanged INIT state and save the SCTP cookie
		// handshake; a peer that does not advertise it uses normal SCTP.
		s.EnableSctpSnap(true)
	}
	if settings.EnableSctpZeroChecksum {
		// RFC 9653 permits a zero SCTP checksum only after the remote endpoint
		// advertises an acceptable alternate integrity method. WebRTC's
		// authenticated DTLS encapsulation is that method, so CRC32c is
		// redundant. Older/mixed peers do not advertise the parameter and
		// retain normal checksums.
		s.EnableSCTPZeroChecksum(true)
	}
	// Native peers advertise literal host candidates (Pion's default is
	// query-only, not address-obscuring gather). They therefore do not need a
	// multicast-DNS listener per peer connection. Disabling mDNS removes that
	// socket/goroutine set and avoids repeated initialization failures on
	// Android. The JS transport is separate and retains browser defaults.
	s.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	// Detached data-channel writes otherwise only append to Pion's SCTP
	// pending queue. Blocking mode is required for transfer backpressure and
	// is what makes SetWriteDeadline effective.
	s.EnableDataChannelBlockWrite(true)
	s.SetSCTPMaxReceiveBufferSize( /*16 * 1024 * 1024*/ uint32(settings.ReceiveBufferSize))
	if 0 < settings.MaxMessageSize {
		s.SetSCTPMaxMessageSize(uint32(settings.MaxMessageSize))
	}
	if 0 < settings.SctpMinCwnd {
		s.SetSCTPMinCwnd(settings.SctpMinCwnd)
	}
	if 0 < settings.SctpFastRtxWnd {
		s.SetSCTPFastRtxWnd(settings.SctpFastRtxWnd)
	}
	if 0 < settings.SctpCwndCAStep {
		s.SetSCTPCwndCAStep(settings.SctpCwndCAStep)
	}
	if 0 < settings.ReceiveMtu {
		s.SetReceiveMTU(uint(settings.ReceiveMtu))
	}
	s.SetICETimeouts(
		settings.DisconnectedTimeout,
		settings.FailedTimeout,
		settings.KeepAliveTimeout,
	)
	if 0 < settings.StunGatherTimeout {
		s.SetSTUNGatherTimeout(settings.StunGatherTimeout)
	}
	api := webrtc.NewAPI(
		webrtc.WithSettingEngine(s),
		webrtc.WithMediaEngine(&webrtc.MediaEngine{}),
		webrtc.WithInterceptorRegistry(nil),
	)
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	certificate, err := webrtc.GenerateCertificate(privateKey)
	if err != nil {
		return nil, err
	}
	configuration := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: settings.IceServerUrls,
			},
		},
		Certificates: []webrtc.Certificate{*certificate},
	}
	return &webRtcPeerConnectionFactory{
		newPeerConnection: func() (*webrtc.PeerConnection, error) {
			return api.NewPeerConnection(configuration)
		},
	}, nil
}

// webRtcSctpProgress returns the native association signals used by the lazy
// no-progress watchdog. BytesReceived counts all SCTP packets read from DTLS,
// including SACKs; BufferedAmount covers pending plus in-flight user data.
func webRtcSctpProgress(pc *webrtc.PeerConnection) (bufferedAmount int, bytesReceived uint64, ok bool) {
	if pc == nil {
		return
	}
	sctp := pc.SCTP()
	if sctp == nil {
		return
	}
	stats := sctp.Stats()
	return sctp.BufferedAmount(), stats.BytesReceived, true
}

func detachWithDeadline(dc *webrtc.DataChannel) (datachannel.ReadWriteCloserDeadliner, error) {
	return dc.DetachWithDeadline()
}
