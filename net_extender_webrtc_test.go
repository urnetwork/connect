//go:build !js

package connect

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/pion/transport/v4/vnet"
	"github.com/pion/webrtc/v4"

	"github.com/urnetwork/connect/protocol"
)

// inMemoryWebRtcExchanger bridges a dialer's ExchangeOffer directly to the
// extender's AnswerWebRtcExtenderCarrierOffer, standing in for the exchange
// signaling. No real network: the SDP (with gathered vnet candidates) is handed
// across in process.
type inMemoryWebRtcExchanger struct {
	ctx              context.Context
	extenderSettings *WebRtcSettings
	handler          WebRtcExtenderCarrierHandler
}

func (self *inMemoryWebRtcExchanger) ExchangeOffer(
	ctx context.Context,
	offer webrtc.SessionDescription,
) (webrtc.SessionDescription, error) {
	return AnswerWebRtcExtenderCarrierOffer(self.ctx, self.extenderSettings, offer, self.handler)
}

// testWebRtcExtenderHandler is the extender serve side the server/sdk provides
// in production: it signs the challenge, writes the response frame, and for a
// forward relays the carrier stream to the operator upstream.
type testWebRtcExtenderHandler struct {
	publicKey   ed25519.PublicKey
	privateKey  ed25519.PrivateKey
	forwardAddr string
}

func (self *testWebRtcExtenderHandler) HandleExtenderStream(
	ctx context.Context,
	header *protocol.ExtenderHeader,
	conn net.Conn,
) {
	defer conn.Close()
	response := &protocol.ExtenderResponse{
		PublicKey:          self.publicKey,
		ChallengeSignature: SignExtenderChallenge(self.privateKey, header.Challenge),
		Carriers:           []string{ExtenderCarrierWebRtc},
	}
	frameBytes, err := ExtenderResponseFrame(response)
	if err != nil {
		return
	}
	if _, err := conn.Write(frameBytes); err != nil {
		return
	}
	if header.Service != ExtenderServiceForward {
		return
	}
	upstream, err := net.Dial("tcp", self.forwardAddr)
	if err != nil {
		return
	}
	defer upstream.Close()
	// relay until either side ends; the operator closes after the hello.
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, conn); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, upstream); done <- struct{}{} }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// Root cause: a NATed extender has no public inbound address, so without a
// webrtc carrier it cannot serve any client. The carrier reaches it over a
// webrtc data channel through signaling and ICE. Observable: a client<->
// extender round trip through the carrier over a simulated network -- the
// challenge signature verifies (C2a identity) and a GET /hello forwards through
// the data channel (C2a forward). A faithful revert that leaves the data
// channel undetached never produces the stream, so the dial times out.
func TestWebRtcExtenderCarrierRoundTripBehindSimulatedNat(t *testing.T) {
	// an in-process virtual network isolates the webrtc hop from the host; the
	// two peers reach each other only through it, so ICE must do the work.
	router, err := vnet.NewRouter(&vnet.RouterConfig{
		CIDR:          "10.77.0.0/24",
		MinDelay:      time.Millisecond,
		LoggerFactory: logging.NewDefaultLoggerFactory(),
	})
	if err != nil {
		t.Fatal(err)
	}
	clientNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{"10.77.0.2"}})
	if err != nil {
		t.Fatal(err)
	}
	extenderNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{"10.77.0.3"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := router.AddNet(clientNet); err != nil {
		t.Fatal(err)
	}
	if err := router.AddNet(extenderNet); err != nil {
		t.Fatal(err)
	}
	if err := router.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := router.Stop(); err != nil {
			t.Errorf("stop router: %v", err)
		}
	}()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	// the operator behind the extender: a GET /hello the forward proves.
	const helloClientAddress = "198.51.100.7:443"
	operator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hello" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Connection", "close")
		_ = json.NewEncoder(w).Encode(map[string]string{"client_address": helloClientAddress})
	}))
	defer operator.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	carrierSettings := func(network *vnet.Net) *WebRtcSettings {
		settings := DefaultWebRtcSettings()
		settings.Log = NewNoopLogger()
		settings.Network = network
		settings.IceServerUrls = nil
		settings.IceServerPoolUrls = nil
		settings.EnableDatagramFastPath = false
		settings.EnableSctpSnap = false
		settings.EnableSctpZeroChecksum = false
		return settings
	}

	handler := &testWebRtcExtenderHandler{
		publicKey:   publicKey,
		privateKey:  privateKey,
		forwardAddr: operator.Listener.Addr().String(),
	}
	exchanger := &inMemoryWebRtcExchanger{
		ctx:              ctx,
		extenderSettings: carrierSettings(extenderNet),
		handler:          handler,
	}

	challenge, err := NewExtenderChallenge()
	if err != nil {
		t.Fatal(err)
	}
	conn, response, err := DialWebRtcExtenderCarrier(
		ctx,
		carrierSettings(clientNet),
		exchanger,
		&ExtenderDial{
			DestinationHost: "api.example",
			DestinationPort: 443,
			Challenge:       challenge,
			Service:         ExtenderServiceForward,
		},
	)
	if err != nil {
		t.Fatalf("dial webrtc extender carrier: %v", err)
	}
	defer conn.Close()

	// C2a identity: the response publishes the extender key and a verifying
	// challenge signature.
	if string(response.PublicKey) != string(publicKey) {
		t.Fatalf("response published a different identity key")
	}
	if !VerifyExtenderChallenge(publicKey, challenge, response.ChallengeSignature) {
		t.Fatalf("challenge signature does not verify over the webrtc carrier")
	}
	if len(response.Carriers) != 1 || response.Carriers[0] != ExtenderCarrierWebRtc {
		t.Fatalf("response carriers = %v, want [webrtc]", response.Carriers)
	}

	// C2a forward: a GET /hello through the data channel returns the operator's
	// hello, proving the carrier forwards.
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://api.example/hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := request.Write(conn); err != nil {
		t.Fatalf("write hello request through carrier: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	httpResponse, err := http.ReadResponse(bufio.NewReader(conn), request)
	if err != nil {
		t.Fatalf("read hello response through carrier: %v", err)
	}
	defer httpResponse.Body.Close()
	if httpResponse.StatusCode != http.StatusOK {
		t.Fatalf("hello status = %d", httpResponse.StatusCode)
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(httpResponse.Body, 64*1024))
	if err != nil {
		t.Fatalf("read hello body: %v", err)
	}
	helloResult := &struct {
		ClientAddress string `json:"client_address"`
	}{}
	if err := json.Unmarshal(bodyBytes, helloResult); err != nil {
		t.Fatalf("decode hello: %v", err)
	}
	if helloResult.ClientAddress != helloClientAddress {
		t.Fatalf("hello client_address = %q, want %q", helloResult.ClientAddress, helloClientAddress)
	}
}
