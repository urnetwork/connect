//go:build js

package connect

// The webrtc extender carrier is not built for js/wasm (net_extender_webrtc.go):
// the browser SDK reaches webrtc through the browser, not pion's detached data
// channels. This stub keeps the carrier's names so the settings and the
// strategy compile unchanged; every session reports the carrier unavailable.

import (
	"context"
	"net"

	"github.com/pion/webrtc/v4"

	"github.com/urnetwork/connect/protocol"
)

type WebRtcExtenderCarrier struct{}

func DefaultWebRtcExtenderSettings() *WebRtcSettings {
	return DefaultWebRtcSettings()
}

func NewWebRtcExtenderCarrier(
	ctx context.Context,
	settings *WebRtcSettings,
	resolver WebRtcExtenderExchangerResolver,
) *WebRtcExtenderCarrier {
	return &WebRtcExtenderCarrier{}
}

func (self *WebRtcExtenderCarrier) Close() {}

func (self *WebRtcExtenderCarrier) Dial(
	ctx context.Context,
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
	extenderDial *ExtenderDial,
) (net.Conn, *protocol.ExtenderResponse, error) {
	return nil, nil, ErrWebRtcExtenderCarrierUnavailable
}

func (self *WebRtcExtenderCarrier) DialWithExchanger(
	ctx context.Context,
	connectSettings *ConnectSettings,
	exchanger WebRtcExtenderOfferExchanger,
	extenderConfig *ExtenderConfig,
	extenderDial *ExtenderDial,
) (net.Conn, *protocol.ExtenderResponse, error) {
	return nil, nil, ErrWebRtcExtenderCarrierUnavailable
}

func (self *WebRtcExtenderCarrier) Answerer(handler WebRtcExtenderStreamHandler) WebRtcExtenderOfferAnswerer {
	return self
}

func (self *WebRtcExtenderCarrier) AnswerWebRtcExtenderOffer(
	ctx context.Context,
	offer webrtc.SessionDescription,
) (webrtc.SessionDescription, error) {
	return webrtc.SessionDescription{}, ErrWebRtcExtenderCarrierUnavailable
}

func dialExtenderWebRtc(
	ctx context.Context,
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
	extenderDial *ExtenderDial,
) (net.Conn, *protocol.ExtenderResponse, error) {
	return nil, nil, ErrWebRtcExtenderCarrierUnavailable
}
