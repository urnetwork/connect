//go:build !js

package connect

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/pion/transport/v4/vnet"
	"github.com/pion/webrtc/v4"

	"github.com/urnetwork/connect/protocol"
)

// Tests of the peer-to-peer webrtc extender carrier (EXTENDER.md S, C2a):
// a dialer on the public side reaching an extender behind a simulated NAT
// through injected signaling, with no real network and no STUN server.
//
// The topology is pion's virtual network: a WAN the dialer sits on, and a LAN
// behind an endpoint-independent NAT the extender sits on. The extender's
// host candidate is unroutable from the WAN, so the only way the two can
// join is the extender's own connectivity check crossing the NAT and the
// dialer learning the mapped address as a peer-reflexive candidate -- the
// hole punch the carrier exists for.

const (
	testWebRtcWanCidr        = "203.0.113.0/24"
	testWebRtcDialerIp       = "203.0.113.10"
	testWebRtcNatIp          = "203.0.113.1"
	testWebRtcLanCidr        = "192.0.2.0/24"
	testWebRtcExtenderLanIp  = "192.0.2.5"
	testWebRtcHelloClientIp  = "198.51.100.7:443"
	testWebRtcDestinationApi = "api.example"
)

// testWebRtcNat is the simulated NAT topology.
type testWebRtcNat struct {
	wan         *vnet.Router
	dialerNet   *vnet.Net
	extenderNet *vnet.Net
}

func newTestWebRtcNat(t *testing.T) *testWebRtcNat {
	t.Helper()
	loggerFactory := logging.NewDefaultLoggerFactory()
	loggerFactory.DefaultLogLevel = logging.LogLevelError
	wan, err := vnet.NewRouter(&vnet.RouterConfig{
		CIDR:          testWebRtcWanCidr,
		MinDelay:      time.Millisecond,
		LoggerFactory: loggerFactory,
	})
	if err != nil {
		t.Fatal(err)
	}
	lan, err := vnet.NewRouter(&vnet.RouterConfig{
		CIDR:      testWebRtcLanCidr,
		StaticIPs: []string{testWebRtcNatIp},
		NATType: &vnet.NATType{
			MappingBehavior:   vnet.EndpointIndependent,
			FilteringBehavior: vnet.EndpointIndependent,
		},
		MinDelay:      time.Millisecond,
		LoggerFactory: loggerFactory,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := wan.AddRouter(lan); err != nil {
		t.Fatal(err)
	}
	dialerNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{testWebRtcDialerIp}})
	if err != nil {
		t.Fatal(err)
	}
	if err := wan.AddNet(dialerNet); err != nil {
		t.Fatal(err)
	}
	extenderNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{testWebRtcExtenderLanIp}})
	if err != nil {
		t.Fatal(err)
	}
	if err := lan.AddNet(extenderNet); err != nil {
		t.Fatal(err)
	}
	if err := wan.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := wan.Stop(); err != nil {
			t.Errorf("stop the virtual network: %v", err)
		}
	})
	return &testWebRtcNat{
		wan:         wan,
		dialerNet:   dialerNet,
		extenderNet: extenderNet,
	}
}

// The carrier settings of one side of the virtual network: no STUN (the
// public side is reached directly, the nated side by its mapped address),
// the browser dtls hello on, a short open bound.
func testWebRtcCarrierSettings(network *vnet.Net) *WebRtcSettings {
	settings := DefaultWebRtcExtenderSettings()
	settings.Log = NewNoopLogger()
	if network != nil {
		settings.Network = network
	}
	settings.IceServerUrls = nil
	settings.IceServerPoolUrls = nil
	settings.ExtenderCarrierOpenTimeout = 10 * time.Second
	return settings
}

// testWebRtcExtender is the extender's stream handler the real server plays
// in production (extender.ExtenderServer.HandleWebRtcExtenderStream): it
// answers the A3 request on the stream and forwards to the operator. It is
// also every refusal the carrier must carry back.
type testWebRtcExtender struct {
	publicKey   ed25519.PublicKey
	privateKey  ed25519.PrivateKey
	forwardAddr string
	// the key published in the response and the key the challenge is signed
	// with, when a test makes them differ from the identity
	publishKey ed25519.PublicKey
	signKey    ed25519.PrivateKey
	// a non-zero status refuses every request with it (and Retry-After)
	refuseStatus int
	retryAfter   string

	lock        sync.Mutex
	remoteAddrs []net.Addr
	streamCount int
}

func newTestWebRtcExtender(t *testing.T, forwardAddr string) *testWebRtcExtender {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &testWebRtcExtender{
		publicKey:   publicKey,
		privateKey:  privateKey,
		forwardAddr: forwardAddr,
		publishKey:  publicKey,
		signKey:     privateKey,
	}
}

func (self *testWebRtcExtender) seenRemoteAddrs() []net.Addr {
	self.lock.Lock()
	defer self.lock.Unlock()
	return append([]net.Addr(nil), self.remoteAddrs...)
}

func (self *testWebRtcExtender) HandleWebRtcExtenderStream(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	func() {
		self.lock.Lock()
		defer self.lock.Unlock()
		self.remoteAddrs = append(self.remoteAddrs, conn.RemoteAddr())
		self.streamCount += 1
	}()
	reader := bufio.NewReader(conn)
	request, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	headerBytes, err := io.ReadAll(io.LimitReader(request.Body, ExtenderMaxHeaderByteCount))
	if err != nil {
		return
	}
	header := &protocol.ExtenderHeader{}
	if err := ProtoUnmarshal(headerBytes, header); err != nil {
		return
	}
	if self.refuseStatus != 0 {
		response := &http.Response{
			StatusCode: self.refuseStatus,
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     http.Header{},
			Body:       http.NoBody,
			Close:      true,
		}
		if self.retryAfter != "" {
			response.Header.Set("Retry-After", self.retryAfter)
		}
		_ = response.Write(conn)
		return
	}
	// an extender with no signing key answers with no signature at all
	var challengeSignature []byte
	if self.signKey != nil {
		challengeSignature = SignExtenderChallenge(self.signKey, header.Challenge)
	}
	frameBytes, err := ExtenderResponseFrame(&protocol.ExtenderResponse{
		PublicKey:          self.publishKey,
		ChallengeSignature: challengeSignature,
		Carriers:           []string{ExtenderCarrierWebRtc},
	})
	if err != nil {
		return
	}
	response := &http.Response{
		StatusCode:    http.StatusOK,
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{ExtenderContentType}},
		ContentLength: int64(len(frameBytes)),
		Body:          io.NopCloser(bytes.NewReader(frameBytes)),
	}
	if err := response.Write(conn); err != nil {
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
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, reader)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, upstream)
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// testWebRtcExchanger bridges a dial straight to the extender's carrier,
// standing in for the exchange signaling.
type testWebRtcExchanger struct {
	ctx      context.Context
	answerer WebRtcExtenderOfferAnswerer
	// when set, replaces the bridge
	exchange func(ctx context.Context, offer webrtc.SessionDescription) (webrtc.SessionDescription, error)
}

func (self *testWebRtcExchanger) ExchangeOffer(
	ctx context.Context,
	offer webrtc.SessionDescription,
) (webrtc.SessionDescription, error) {
	if self.exchange != nil {
		return self.exchange(ctx, offer)
	}
	return self.answerer.AnswerWebRtcExtenderOffer(self.ctx, offer)
}

// The operator behind the extender: a plain http site answering /hello.
func newTestWebRtcOperator(t *testing.T) *httptest.Server {
	t.Helper()
	operator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hello" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Connection", "close")
		_ = json.NewEncoder(w).Encode(map[string]string{"client_address": testWebRtcHelloClientIp})
	}))
	t.Cleanup(operator.Close)
	return operator
}

// One dialer carrier and one extender carrier on the virtual network, bridged
// in memory, with the extender serving the fake handler.
type testWebRtcCarrierPair struct {
	ctx             context.Context
	nat             *testWebRtcNat
	extender        *testWebRtcExtender
	dialerCarrier   *WebRtcExtenderCarrier
	extenderCarrier *WebRtcExtenderCarrier
	exchanger       *testWebRtcExchanger
	connectSettings *ConnectSettings
}

func newTestWebRtcCarrierPair(t *testing.T, ctx context.Context) *testWebRtcCarrierPair {
	t.Helper()
	nat := newTestWebRtcNat(t)
	operator := newTestWebRtcOperator(t)
	extender := newTestWebRtcExtender(t, operator.Listener.Addr().String())
	extenderCarrier := NewWebRtcExtenderCarrier(ctx, testWebRtcCarrierSettings(nat.extenderNet), nil)
	t.Cleanup(extenderCarrier.Close)
	exchanger := &testWebRtcExchanger{
		ctx:      ctx,
		answerer: extenderCarrier.Answerer(extender),
	}
	dialerCarrier := NewWebRtcExtenderCarrier(
		ctx,
		testWebRtcCarrierSettings(nat.dialerNet),
		func(extenderPublicKey []byte) (WebRtcExtenderOfferExchanger, bool) {
			if !bytes.Equal(extenderPublicKey, extender.publicKey) {
				return nil, false
			}
			return exchanger, true
		},
	)
	t.Cleanup(dialerCarrier.Close)
	connectSettings := DefaultConnectSettings()
	connectSettings.ConnectTimeout = 10 * time.Second
	connectSettings.RequestTimeout = 20 * time.Second
	connectSettings.WebRtcExtenderCarrier = dialerCarrier
	return &testWebRtcCarrierPair{
		ctx:             ctx,
		nat:             nat,
		extender:        extender,
		dialerCarrier:   dialerCarrier,
		extenderCarrier: extenderCarrier,
		exchanger:       exchanger,
		connectSettings: connectSettings,
	}
}

func (self *testWebRtcCarrierPair) extenderConfig() *ExtenderConfig {
	return &ExtenderConfig{
		Profile: ExtenderProfile{
			ConnectMode: ExtenderConnectModeWebRtc,
			Port:        ExtenderTcpPort,
		},
		PublicKey: self.extender.publicKey,
	}
}

// Writes a plain GET /hello on the forwarded stream and reads the answer.
func testWebRtcHelloThrough(t *testing.T, ctx context.Context, conn net.Conn) string {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+testWebRtcDestinationApi+"/hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := request.Write(conn); err != nil {
		t.Fatalf("write hello through the carrier: %v", err)
	}
	httpResponse, err := http.ReadResponse(bufio.NewReader(conn), request)
	if err != nil {
		t.Fatalf("read hello through the carrier: %v", err)
	}
	defer httpResponse.Body.Close()
	if httpResponse.StatusCode != http.StatusOK {
		t.Fatalf("hello status = %d", httpResponse.StatusCode)
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(httpResponse.Body, 64*1024))
	if err != nil {
		t.Fatal(err)
	}
	helloResult := &struct {
		ClientAddress string `json:"client_address"`
	}{}
	if err := json.Unmarshal(bodyBytes, helloResult); err != nil {
		t.Fatalf("decode hello: %v", err)
	}
	return helloResult.ClientAddress
}

// Root cause: a NATed extender has no public inbound address, so without a
// webrtc carrier no client can reach it. Observable: a dial from the public
// side joins the extender behind the simulated NAT, the extender's challenge
// signature verifies over the carrier (C2a identity), a GET /hello forwards
// through the data channel (C2a forward), and the extender saw the stream
// arrive from the dialer's public address while the dialer saw the NAT's
// mapped address -- the hole punch, not a direct path.
func TestWebRtcExtenderCarrierRoundTripBehindSimulatedNat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pair := newTestWebRtcCarrierPair(t, ctx)

	challenge, err := NewExtenderChallenge()
	if err != nil {
		t.Fatal(err)
	}
	conn, response, err := DialExtender(
		ctx,
		pair.connectSettings,
		pair.extenderConfig(),
		&ExtenderDial{
			DestinationHost: testWebRtcDestinationApi,
			DestinationPort: 443,
			Challenge:       challenge,
			Service:         ExtenderServiceForward,
		},
	)
	if err != nil {
		t.Fatalf("dial the webrtc extender carrier: %v", err)
	}
	defer conn.Close()

	if !bytes.Equal(response.PublicKey, pair.extender.publicKey) {
		t.Fatalf("the response published another identity key")
	}
	if !VerifyExtenderChallenge(pair.extender.publicKey, challenge, response.ChallengeSignature) {
		t.Fatalf("the challenge signature does not verify over the carrier")
	}
	if len(response.Carriers) != 1 || response.Carriers[0] != ExtenderCarrierWebRtc {
		t.Fatalf("response carriers = %v, want [webrtc]", response.Carriers)
	}
	if clientAddress := testWebRtcHelloThrough(t, ctx, conn); clientAddress != testWebRtcHelloClientIp {
		t.Fatalf("hello client_address = %q, want %q", clientAddress, testWebRtcHelloClientIp)
	}

	// the stream addresses are the selected ICE pair's: the dialer's public
	// address on the extender, the NAT's mapped address on the dialer
	remoteAddrs := pair.extender.seenRemoteAddrs()
	if len(remoteAddrs) != 1 {
		t.Fatalf("extender streams = %d, want 1", len(remoteAddrs))
	}
	extenderSeen, ok := remoteAddrs[0].(*net.UDPAddr)
	if !ok || extenderSeen.IP.String() != testWebRtcDialerIp {
		t.Fatalf("the extender saw %v, want the dialer's public address %s", remoteAddrs[0], testWebRtcDialerIp)
	}
	dialerSeen, ok := conn.RemoteAddr().(*net.UDPAddr)
	if !ok || dialerSeen.IP.String() != testWebRtcNatIp {
		t.Fatalf("the dialer saw %v, want the NAT's mapped address %s", conn.RemoteAddr(), testWebRtcNatIp)
	}
}

// A dial of a webrtc profile through connect settings that carry no carrier
// is refused at once: the profile is not dialable here, and nothing is
// opened to find that out.
func TestWebRtcExtenderCarrierDialNeedsACarrier(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connectSettings := DefaultConnectSettings()
	conn, _, err := DialExtender(ctx, connectSettings, &ExtenderConfig{
		Profile:   ExtenderProfile{ConnectMode: ExtenderConnectModeWebRtc, Port: ExtenderTcpPort},
		PublicKey: make([]byte, ed25519.PublicKeySize),
	}, &ExtenderDial{DestinationHost: testWebRtcDestinationApi, DestinationPort: 443})
	if !errors.Is(err, ErrWebRtcExtenderCarrierUnavailable) {
		t.Fatalf("err = %v, want ErrWebRtcExtenderCarrierUnavailable", err)
	}
	if conn != nil {
		t.Fatalf("a refused dial returned a stream")
	}
}

// An extender the carrier's resolver does not know has no signaling path:
// the dial is refused before a peer connection exists.
func TestWebRtcExtenderCarrierDialNeedsASignalingPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resolved := 0
	carrier := NewWebRtcExtenderCarrier(ctx, testWebRtcCarrierSettings(nil), func(extenderPublicKey []byte) (WebRtcExtenderOfferExchanger, bool) {
		resolved += 1
		return nil, false
	})
	defer carrier.Close()
	connectSettings := DefaultConnectSettings()
	connectSettings.WebRtcExtenderCarrier = carrier
	_, _, err := DialExtender(ctx, connectSettings, &ExtenderConfig{
		Profile:   ExtenderProfile{ConnectMode: ExtenderConnectModeWebRtc, Port: ExtenderTcpPort},
		PublicKey: make([]byte, ed25519.PublicKeySize),
	}, nil)
	if !errors.Is(err, ErrWebRtcExtenderNoSignaling) {
		t.Fatalf("err = %v, want ErrWebRtcExtenderNoSignaling", err)
	}
	if resolved != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolved)
	}
	// a carrier with no resolver at all is the same refusal
	noResolver := NewWebRtcExtenderCarrier(ctx, testWebRtcCarrierSettings(nil), nil)
	defer noResolver.Close()
	connectSettings.WebRtcExtenderCarrier = noResolver
	if _, _, err := DialExtender(ctx, connectSettings, &ExtenderConfig{
		Profile: ExtenderProfile{ConnectMode: ExtenderConnectModeWebRtc, Port: ExtenderTcpPort},
	}, nil); !errors.Is(err, ErrWebRtcExtenderNoSignaling) {
		t.Fatalf("err = %v, want ErrWebRtcExtenderNoSignaling", err)
	}
}

// Root cause: the carrier has no outer leaf to pin, so an extender that is
// not the one the record names could answer a dial. Observable: a response
// publishing another key, and one publishing the right key with a signature
// by another, are both refused and no stream is handed out.
func TestWebRtcExtenderCarrierRefusesAnotherIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pair := newTestWebRtcCarrierPair(t, ctx)
	otherPublicKey, otherPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		publishKey ed25519.PublicKey
		signKey    ed25519.PrivateKey
		expectErr  string
	}{
		{name: "another key published", publishKey: otherPublicKey, signKey: otherPrivateKey, expectErr: "another identity key"},
		{name: "another key signed", publishKey: pair.extender.publicKey, signKey: otherPrivateKey, expectErr: "does not verify"},
	}
	for _, c := range cases {
		pair.extender.publishKey = c.publishKey
		pair.extender.signKey = c.signKey
		conn, _, err := DialExtender(ctx, pair.connectSettings, pair.extenderConfig(), &ExtenderDial{
			DestinationHost: testWebRtcDestinationApi,
			DestinationPort: 443,
		})
		if err == nil || !bytes.Contains([]byte(err.Error()), []byte(c.expectErr)) {
			t.Fatalf("%s: err = %v, want %q", c.name, err, c.expectErr)
		}
		if conn != nil {
			t.Fatalf("%s: a refused identity handed out a stream", c.name)
		}
	}
}

// Root cause: a dial that brings no challenge would accept any response on a
// carrier with no outer leaf. Observable: the header the extender reads
// carries a challenge the dial generated, when the config names the key.
func TestWebRtcExtenderCarrierChallengesWhenTheDialBringsNone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pair := newTestWebRtcCarrierPair(t, ctx)
	// an extender that signs nothing is accepted only by a dial that did not
	// challenge; this one must challenge, so the empty signature is refused
	pair.extender.signKey = nil
	conn, _, err := DialExtender(ctx, pair.connectSettings, pair.extenderConfig(), &ExtenderDial{
		DestinationHost: testWebRtcDestinationApi,
		DestinationPort: 443,
	})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("does not verify")) {
		t.Fatalf("err = %v, want the challenge refused", err)
	}
	if conn != nil {
		t.Fatalf("an unsigned response handed out a stream")
	}
}

// Root cause: the extender's refusals must reach the dialer as the same
// typed errors the tcp carrier produces, or the strategy cannot tell a limit
// from a failure (A12). Observable: 403 is ExtenderRefusedError and 429 with
// Retry-After is ExtenderLimitedError, over the carrier.
func TestWebRtcExtenderCarrierCarriesRefusalsAndLimits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pair := newTestWebRtcCarrierPair(t, ctx)

	pair.extender.refuseStatus = http.StatusForbidden
	_, _, err := DialExtender(ctx, pair.connectSettings, pair.extenderConfig(), nil)
	var refusedErr *ExtenderRefusedError
	if !errors.As(err, &refusedErr) || refusedErr.StatusCode != http.StatusForbidden {
		t.Fatalf("err = %v, want a 403 ExtenderRefusedError", err)
	}

	pair.extender.refuseStatus = http.StatusTooManyRequests
	pair.extender.retryAfter = "42"
	_, _, err = DialExtender(ctx, pair.connectSettings, pair.extenderConfig(), nil)
	var limitedErr *ExtenderLimitedError
	if !errors.As(err, &limitedErr) {
		t.Fatalf("err = %v, want an ExtenderLimitedError", err)
	}
	if limitedErr.RetryAfter < 41*time.Second || 43*time.Second < limitedErr.RetryAfter {
		t.Fatalf("retry after = %s, want about 42s", limitedErr.RetryAfter)
	}
}

// A dial ends on the caller's context while the signaling has not answered,
// and nothing is left open.
func TestWebRtcExtenderCarrierDialEndsOnTheCallerContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pair := newTestWebRtcCarrierPair(t, ctx)
	dialCtx, dialCancel := context.WithCancel(ctx)
	offered := make(chan struct{}, 1)
	pair.exchanger.exchange = func(ctx context.Context, offer webrtc.SessionDescription) (webrtc.SessionDescription, error) {
		offered <- struct{}{}
		// the signaling never answers; the dialer gives up on its own context
		<-ctx.Done()
		return webrtc.SessionDescription{}, ctx.Err()
	}
	dialErr := make(chan error, 1)
	go func() {
		_, _, err := DialExtender(dialCtx, pair.connectSettings, pair.extenderConfig(), nil)
		dialErr <- err
	}()
	<-offered
	dialCancel()
	err := <-dialErr
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the caller's cancellation", err)
	}
	if count := len(pair.extender.seenRemoteAddrs()); count != 0 {
		t.Fatalf("the extender served %d streams of a dial that never completed", count)
	}
}

// Root cause: a data channel with another label on a carrier connection is
// not the carrier; serving it would hand an arbitrary channel to the
// extender's request handler. Observable: the extender resets the stream once
// it opens (the dialer's read of it ends), serves nothing, and releases the
// peer connection when no carrier channel opens within the open bound.
func TestWebRtcExtenderCarrierAnswerRefusesAnotherDataChannelLabel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pair := newTestWebRtcCarrierPair(t, ctx)
	// a short open bound, so the release is observed within the test
	pair.extenderCarrier.settings.ExtenderCarrierOpenTimeout = 2 * time.Second
	released := make(chan struct{}, 1)
	pair.extenderCarrier.answerReleasedForTest = func() {
		select {
		case released <- struct{}{}:
		default:
		}
	}

	// a hand-rolled dialer with the wrong label, through the same factory
	factory, err := pair.dialerCarrier.peerConnectionFactory()
	if err != nil {
		t.Fatal(err)
	}
	peerConnection, cancelResolve, err := factory.newPeerConnection(false)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelResolve()
	defer peerConnection.Close()
	dataChannel, err := peerConnection.CreateDataChannel("data", nil)
	if err != nil {
		t.Fatal(err)
	}
	// the dialer reads its channel once open: the extender's reset ends the
	// read, where a served channel would wait for bytes forever
	readDone := make(chan error, 1)
	dataChannel.OnOpen(func() {
		detached, err := detachWithDeadline(dataChannel)
		if err != nil {
			readDone <- err
			return
		}
		_, err = detached.Read(make([]byte, 1024))
		readDone <- err
	})
	if err := pair.dialerCarrier.negotiateOffer(ctx, peerConnection, pair.exchanger); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatalf("the extender served bytes on another label")
		}
	case <-ctx.Done():
		t.Fatalf("the extender did not reset the channel with another label")
	}
	select {
	case <-released:
	case <-ctx.Done():
		t.Fatalf("the extender did not release the peer connection without a carrier channel")
	}
	if count := len(pair.extender.seenRemoteAddrs()); count != 0 {
		t.Fatalf("the extender served %d streams on another label", count)
	}
}

// Root cause: an answered offer whose dialer never connects would hold a
// peer connection forever. Observable: the answer side releases it once the
// open bound passes with no channel.
func TestWebRtcExtenderCarrierAnswerReleasesAnUnopenedPeerConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pair := newTestWebRtcCarrierPair(t, ctx)
	pair.extenderCarrier.settings.ExtenderCarrierOpenTimeout = 500 * time.Millisecond
	released := make(chan struct{}, 1)
	pair.extenderCarrier.answerReleasedForTest = func() {
		select {
		case released <- struct{}{}:
		default:
		}
	}

	// an offer whose dialer applies no answer: the peer connection is
	// thrown away as soon as the offer is out
	factory, err := pair.dialerCarrier.peerConnectionFactory()
	if err != nil {
		t.Fatal(err)
	}
	peerConnection, cancelResolve, err := factory.newPeerConnection(false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peerConnection.CreateDataChannel(webRtcExtenderDataChannelLabel, nil); err != nil {
		t.Fatal(err)
	}
	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)
	if err := peerConnection.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gatherComplete
	if _, err := pair.exchanger.ExchangeOffer(ctx, *peerConnection.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	peerConnection.Close()
	cancelResolve()

	select {
	case <-released:
	case <-ctx.Done():
		t.Fatalf("the answer side did not release the unopened peer connection")
	}
}

// A closed carrier answers nothing and dials nothing.
func TestWebRtcExtenderCarrierCloseRefusesSessions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	carrier := NewWebRtcExtenderCarrier(ctx, testWebRtcCarrierSettings(nil), func([]byte) (WebRtcExtenderOfferExchanger, bool) {
		return &testWebRtcExchanger{}, true
	})
	carrier.Close()
	connectSettings := DefaultConnectSettings()
	connectSettings.WebRtcExtenderCarrier = carrier
	if _, _, err := DialExtender(ctx, connectSettings, &ExtenderConfig{
		Profile: ExtenderProfile{ConnectMode: ExtenderConnectModeWebRtc, Port: ExtenderTcpPort},
	}, nil); !errors.Is(err, ErrWebRtcExtenderCarrierUnavailable) {
		t.Fatalf("dial err = %v, want ErrWebRtcExtenderCarrierUnavailable", err)
	}
	if _, err := carrier.Answer(ctx, webrtc.SessionDescription{}, newTestWebRtcExtender(t, "")); !errors.Is(err, ErrWebRtcExtenderCarrierUnavailable) {
		t.Fatalf("answer err = %v, want ErrWebRtcExtenderCarrierUnavailable", err)
	}
	if _, err := carrier.Answer(ctx, webrtc.SessionDescription{}, nil); err == nil {
		t.Fatalf("an answer with no handler must fail")
	}
}

// The carrier name maps to its connect mode and back, so a record that lists
// it reaches the dial and a response names it (A4, B2).
func TestWebRtcExtenderCarrierNameRoundTrips(t *testing.T) {
	connectMode, ok := ExtenderConnectModeForCarrier(ExtenderCarrierWebRtc)
	if !ok || connectMode != ExtenderConnectModeWebRtc {
		t.Fatalf("carrier mode = %q, %t", connectMode, ok)
	}
	if carrier := ExtenderCarrierForConnectMode(ExtenderConnectModeWebRtc); carrier != ExtenderCarrierWebRtc {
		t.Fatalf("mode carrier = %q", carrier)
	}
	ordered := orderedExtenderCarriers([]string{ExtenderCarrierWebRtc, ExtenderCarrierDns, ExtenderCarrierTcp})
	if fmt.Sprint(ordered) != fmt.Sprint([]string{ExtenderCarrierTcp, ExtenderCarrierDns, ExtenderCarrierWebRtc}) {
		t.Fatalf("ordered = %v, want tcp, dns, webrtc", ordered)
	}
}
