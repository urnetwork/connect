package connect

// transport_egress_proxy.go -- a platform transport that leaves through a
// proxy says so on its connect handshake.
//
// A transport whose strategy dials through a socks proxy
// (ConnectSettings.ProxySettings, as an sn miner proxy slot's space does)
// reaches the platform from the proxy's address, not from this host's. It marks
// every h1 dial with HeaderEgressProxy, whatever its auth generation, and the
// platform counts such a client at no address of its own: it shows no SN25
// head-breadth unit, capture the flag unit or extender unit (sn WHITEPAPER.md
// §8.4). A proxy carries no udp, so no h3 transport dials through one, and a
// transport dialed from this host marks nothing.

import (
	"net/http"
)

// HeaderEgressProxy is the h1 handshake header of a transport dialed through
// a proxy. Absent on a transport dialed from this host.
const HeaderEgressProxy = "X-UR-Egress-Proxy"

// EgressProxyDeclared is the HeaderEgressProxy value of a proxied transport.
const EgressProxyDeclared = "1"

// applyEgressProxyHeader marks the handshake of a transport whose strategy
// dials through a proxy.
func applyEgressProxyHeader(header http.Header, clientStrategy *ClientStrategy) {
	if clientStrategy != nil && clientStrategy.ConnectSettings().ProxySettings != nil {
		header.Set(HeaderEgressProxy, EgressProxyDeclared)
	}
}
