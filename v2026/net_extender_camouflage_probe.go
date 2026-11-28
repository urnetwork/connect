package connect

// net_extender_camouflage_probe.go — the operator's probes of an extender that
// publishes a camouflage key (EXTENDER-UPDATE2 §3.2, EXTENDER.md P6, C2, C3).
//
// A Phase B extender splices every unauthenticated tcp hello to its borrowed
// site, and the operator's own probes are no exception: a probe that dials
// the legacy hello alone meets the borrowed site and fails the B3 leaf check.
// That refuses the extender's activation and fails its uptime probe, so a
// splicing extender would drop out of the directory. The operator's probes are
// clients that depend on legacy termination, which §3.2 says must end before
// Phase B.
//
// So when the operator knows the extender's camouflage key, the probes dial
// the tcp carrier the way a compatible client does: the camouflaged attempt
// first, then the legacy one (dialExtenderTcp's race), and either passing
// passes the probe. In Phase A the probe still proves what it always has. It
// claims nothing about the key: a wrong key in Phase A still passes over the
// legacy attempt. Only in Phase B, where the legacy attempt is spliced away,
// does a probe need the extender's own key. The camouflaged attempt fronts a
// name of the operator's bundled borrow list (the global list, which every
// extender covers); without one it has no front name, and the probe is the
// legacy attempt alone, as before.
//
// The key rides the tcp carrier only. The quic and dns probes ignore it.

import (
	"context"
	"crypto/tls"
	"net/netip"
	"slices"

	"github.com/urnetwork/connect/v2026/protocol"
)

// ProbeExtenderCarrierWithCamouflageKey is ProbeExtenderCarrier for an extender
// whose camouflage key the operator has (see the file comment): on the tcp
// carrier the probe also runs the camouflaged attempt with realityPublicKey,
// so it passes against a Phase B extender. An empty or malformed key, or
// another carrier, probes exactly as ProbeExtenderCarrier.
func ProbeExtenderCarrierWithCamouflageKey(
	ctx context.Context,
	connectSettings *ConnectSettings,
	ip netip.Addr,
	connectMode ExtenderConnectMode,
	port int,
	dnsTld string,
	serverName string,
	expectPublicKey []byte,
	realityPublicKey []byte,
	destinationHost string,
	destinationPort int,
) (*protocol.ExtenderResponse, error) {
	return probeExtenderCarrier(
		ctx,
		connectSettings,
		ip,
		connectMode,
		port,
		dnsTld,
		serverName,
		expectPublicKey,
		realityPublicKey,
		destinationHost,
		destinationPort,
	)
}

// ProbeExtenderForwardWithCamouflageKey is ProbeExtenderForward for an extender
// whose camouflage key the operator has (see the file comment): the forward's
// tcp dial also runs the camouflaged attempt with realityPublicKey. An empty
// or malformed key probes exactly as ProbeExtenderForward.
func ProbeExtenderForwardWithCamouflageKey(
	ctx context.Context,
	connectSettings *ConnectSettings,
	ip netip.Addr,
	port int,
	serverName string,
	expectPublicKey []byte,
	realityPublicKey []byte,
	apiUrl string,
	tlsConfig *tls.Config,
) (string, error) {
	return probeExtenderForward(
		ctx,
		connectSettings,
		ip,
		port,
		serverName,
		expectPublicKey,
		realityPublicKey,
		apiUrl,
		tlsConfig,
	)
}

// The camouflage key a probe's config carries: on the tcp carrier only, and
// only a key of the right size, so a probe never fronts a malformed key.
func probeExtenderCamouflageKey(connectMode ExtenderConnectMode, realityPublicKey []byte) []byte {
	if connectMode != ExtenderConnectModeTcpTls || len(realityPublicKey) != extenderRealityX25519PublicKeyByteCount {
		return nil
	}
	return slices.Clone(realityPublicKey)
}
