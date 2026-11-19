package connect

import (
	"errors"
)

// The udp of a space whose egress is a proxy (connect#227, support inbox 8395).
//
// A socks proxy (`ConnectSettings.ProxySettings`) carries streams only. Every
// udp socket a space opens is this host's own: a quic or dns extender carrier, an
// alt api dialer, an h3 platform transport, the ice of a webrtc carrier. Each
// would send the space's traffic from this host's address, outside the proxy the
// space is configured with -- for a miner proxy slot, from the host every slot
// shares. So a space whose egress is a proxy fails closed for udp, and its tcp
// carriers keep dialing through the proxy:
//   - The quic and dns extender carriers dial only through an endpoint the owner
//     supplied (`DialContextSettings.PacketConnFactory`). They dial an ip
//     literal, so nothing is looked up.
//   - The alt api dialers and the h3 platform transport modes are off, endpoint
//     or not: each resolves a platform name on this host before it dials, which
//     is a lookup outside the proxy.
//   - The webrtc carrier is off: ice gathers this host's own candidates.
//
// A space whose egress is a dialer of its own (`DialContextSettings`) with no
// proxy keeps that type's contract: a nil PacketConnFactory is the platform's
// ordinary socket.
//
// The carriers are left out where they are chosen -- the strategy's extender
// and alt dialers, the extender network client's feed and probe walks, the
// platform transport's modes -- so one a space cannot dial is never tried and
// never counts against its extender. The sockets refuse too
// (openExtenderPacketConn, dialExtenderWebRtc, openH3PacketConn), whoever
// reaches them, with ErrExtenderPacketCarrierUnavailable, which says nothing
// about the extender.

// ErrExtenderPacketCarrierUnavailable is a udp dial of a space whose egress is a
// proxy with no packet endpoint of its own. Nothing was sent, and the extender
// or the server it was meant for is not at fault.
var ErrExtenderPacketCarrierUnavailable = errors.New("the space's egress is a proxy, which carries no udp")

// Whether the quic and dns extender carriers of these settings have a path: no
// proxy, or an endpoint the owner supplied.
func connectSettingsHavePacketPath(connectSettings *ConnectSettings) bool {
	if connectSettings == nil || connectSettings.ProxySettings == nil {
		return true
	}
	return connectSettings.DialContextSettings != nil && connectSettings.DialContextSettings.PacketConnFactory != nil
}

// Whether a carrier of this connect mode can be dialed under these settings.
func extenderConnectModeDialable(connectSettings *ConnectSettings, connectMode ExtenderConnectMode) bool {
	switch connectMode {
	case ExtenderConnectModeQuic, ExtenderConnectModeDns:
		return connectSettingsHavePacketPath(connectSettings)
	case ExtenderConnectModeWebRtc:
		// ice opens its own sockets, whatever endpoint the owner supplied
		return connectSettings == nil || connectSettings.ProxySettings == nil
	default:
		return true
	}
}

// The configs of `extenderConfigs` these settings can dial, in order; a carrier
// the space's egress cannot carry is left out.
func dialableExtenderConfigs(connectSettings *ConnectSettings, extenderConfigs []*ExtenderConfig) []*ExtenderConfig {
	dialableConfigs := make([]*ExtenderConfig, 0, len(extenderConfigs))
	for _, extenderConfig := range extenderConfigs {
		if extenderConnectModeDialable(connectSettings, extenderConfig.Profile.ConnectMode) {
			dialableConfigs = append(dialableConfigs, extenderConfig)
		}
	}
	return dialableConfigs
}
