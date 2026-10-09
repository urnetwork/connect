package connect

// The operator's probes of the peer-to-peer webrtc extender carrier
// (EXTENDER.md C2a, S). They are the webrtc halves of net_extender_probe.go:
// the carrier probe proves the identity key over a data channel, the forward
// probe proves the forward with a verified `GET /hello` through it. Both run
// from the outside exactly as a client would, through the carrier the connect
// settings carry, whose resolver names the one extender being proved.
//
// There is no outer leaf on this carrier, so the carrier probe's identity
// half is the challenge signature alone, which the dial verifies itself when
// the config names the key. The forward probe challenges the same way: a
// `GET /hello` answered through a data channel whose far end did not sign the
// challenge proves nothing, so a forward that reaches the api through an
// unverified extender is refused before it is read.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/urnetwork/connect/protocol"
)

// ProbeExtenderWebRtcCarrier dials the webrtc carrier of one extender with a
// fresh challenge and verifies what comes back (C2a). connectSettings must
// carry the carrier (ConnectSettings.WebRtcExtenderCarrier) whose resolver
// knows expectPublicKey; expectPublicKey is required, since without it the
// carrier has nothing to verify an identity against. destinationHost and
// destinationPort are the forward destination of the probe request, which
// must match an operator pattern of the extender (A5). The probe stream is
// closed before returning; nothing is forwarded over it.
func ProbeExtenderWebRtcCarrier(
	ctx context.Context,
	connectSettings *ConnectSettings,
	expectPublicKey []byte,
	destinationHost string,
	destinationPort int,
) (*protocol.ExtenderResponse, error) {
	if connectSettings.WebRtcExtenderCarrier == nil {
		return nil, ErrWebRtcExtenderCarrierUnavailable
	}
	if len(expectPublicKey) == 0 {
		return nil, fmt.Errorf("extender webrtc probe needs the extender key")
	}
	if destinationHost == "" {
		return nil, fmt.Errorf("extender probe has no destination")
	}

	probeCtx, probeCancel := probeContext(ctx, connectSettings)
	defer probeCancel()

	// the dial challenges and verifies on its own when the config names the
	// key (DialWithExchanger); the response it returns has already passed
	conn, response, err := DialExtender(
		probeCtx,
		connectSettings,
		webRtcProbeExtenderConfig(expectPublicKey),
		&ExtenderDial{
			DestinationHost: destinationHost,
			DestinationPort: destinationPort,
			Service:         ExtenderServiceForward,
		},
	)
	if err != nil {
		// ownership transfers for every non-nil result, including a rejected one
		if conn != nil {
			conn.Close()
		}
		return nil, err
	}
	// the probe never forwards; release the stream and the destination
	// connection the extender opened for it
	conn.Close()
	if response == nil {
		return nil, fmt.Errorf("extender sent no response")
	}
	return response, nil
}

// ProbeExtenderWebRtcForward proves that the webrtc carrier forwards by
// performing a verified `GET /hello` to the api url through it, and returns
// the caller address the api saw (C2a). tlsConfig is the inner configuration,
// verified normally: the platform's pinned roots in production and the
// fixture roots in tests; nil keeps the connect settings configuration. The
// outer identity is the challenge signature the dial verifies against
// expectPublicKey, which is required.
func ProbeExtenderWebRtcForward(
	ctx context.Context,
	connectSettings *ConnectSettings,
	expectPublicKey []byte,
	apiUrl string,
	tlsConfig *tls.Config,
) (string, error) {
	if connectSettings.WebRtcExtenderCarrier == nil {
		return "", ErrWebRtcExtenderCarrierUnavailable
	}
	if len(expectPublicKey) == 0 {
		return "", fmt.Errorf("extender webrtc probe needs the extender key")
	}

	probeCtx, probeCancel := probeContext(ctx, connectSettings)
	defer probeCancel()

	// the inner tls is the caller's, and only the inner tls is verified
	probeSettings := *connectSettings
	if tlsConfig != nil {
		probeSettings.TlsConfig = tlsConfig
	}
	client := NewExtenderHttpClient(&probeSettings, webRtcProbeExtenderConfig(expectPublicKey))
	defer client.CloseIdleConnections()

	request, err := HelloRequestFromUrl(probeCtx, apiUrl, "")
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("extender hello answered with status %d", response.StatusCode)
	}
	// hello is small; a compromised extender cannot make the probe read forever
	bodyBytes, err := io.ReadAll(io.LimitReader(response.Body, extenderHelloMaxByteCount))
	if err != nil {
		return "", err
	}
	helloResult := &extenderHelloResult{}
	if err := json.Unmarshal(bodyBytes, helloResult); err != nil {
		return "", err
	}
	if helloResult.ClientAddress == "" {
		return "", fmt.Errorf("extender hello carried no client address")
	}
	return helloResult.ClientAddress, nil
}

// The dial config of a webrtc probe: the carrier mode and the key, no ip and
// no name. The port is the tcp carrier's only so the request authority is
// well formed; the carrier does not dial it.
func webRtcProbeExtenderConfig(expectPublicKey []byte) *ExtenderConfig {
	return &ExtenderConfig{
		Profile: ExtenderProfile{
			ConnectMode: ExtenderConnectModeWebRtc,
			Port:        ExtenderTcpPort,
		},
		PublicKey: expectPublicKey,
	}
}
