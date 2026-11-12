package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// connectctl's extender with the peer-to-peer webrtc carrier, --webrtc
// (EXTENDER.md S8): the flags, the client id of the jwt, and a run that serves
// on the carrier when its tcp port does not bind. The carrier's own round
// trip, behind a nat, is connect's (net_extender_webrtc_test.go); here the
// manager the offers arrive on is in process, and nothing signals.

// A signal sender that delivers nothing, consuming each frame as a client's
// sender does.
type testDropSignalSender struct{}

func (testDropSignalSender) SendSignal(destinationId connect.Id, signal *protocol.Frame, opts ...any) {
	connect.MessagePoolReturn(signal.MessageBytes)
}

// An unsigned jwt with the given claims; only its claims are read.
func testExtenderJwt(t *testing.T, claims gojwt.MapClaims) string {
	t.Helper()
	jwt, err := gojwt.NewWithClaims(gojwt.SigningMethodNone, claims).SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	return jwt
}

// The flag turns the carrier on and --connect_url names where its client
// connects; without them the carrier is off and the default url stands.
func TestExtenderOptionsReadTheWebRtcFlags(t *testing.T) {
	opts, err := docopt.ParseArgs(
		connectCtlUsage(),
		[]string{"extender", "--jwt=test-jwt", "--webrtc", "--connect_url=wss://connect.space.example"},
		ConnectCtlVersion,
	)
	if err != nil {
		t.Fatal(err)
	}
	options, err := extenderOptionsFromOpts(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !options.webRtc {
		t.Error("--webrtc did not arrive")
	}
	if options.connectUrl != "wss://connect.space.example" {
		t.Errorf("connect url = %q", options.connectUrl)
	}

	opts, err = docopt.ParseArgs(connectCtlUsage(), []string{"extender", "--jwt=test-jwt"}, ConnectCtlVersion)
	if err != nil {
		t.Fatal(err)
	}
	options, err = extenderOptionsFromOpts(opts)
	if err != nil {
		t.Fatal(err)
	}
	if options.webRtc {
		t.Error("the webrtc carrier defaulted on")
	}
	if options.connectUrl != "" {
		t.Errorf("connect url = %q, expected the default", options.connectUrl)
	}
}

// The carrier's client signals under the client the jwt names, which is the
// rendezvous the operator signs into the record; a jwt that names none, or
// one that is not a jwt, is refused with a reason and without its value.
func TestExtenderJwtClientIdReadsTheClientClaim(t *testing.T) {
	clientId := connect.NewId()
	got, err := extenderJwtClientId(testExtenderJwt(t, gojwt.MapClaims{"client_id": clientId.String()}))
	if err != nil || got != clientId {
		t.Fatalf("client id = %s, %v, expected %s", got, err, clientId)
	}
	if _, err := extenderJwtClientId(testExtenderJwt(t, gojwt.MapClaims{"network_id": connect.NewId().String()})); err == nil {
		t.Fatal("a jwt with no client id was accepted")
	}
	if _, err := extenderJwtClientId(testExtenderJwt(t, gojwt.MapClaims{"client_id": "not-an-id"})); err == nil {
		t.Fatal("a jwt with an unreadable client id was accepted")
	}
	if _, err := extenderJwtClientId("not-a-jwt"); err == nil {
		t.Fatal("a value that is not a jwt was accepted")
	}
}

// Root cause: the command exited whenever tcp did not bind, since the operator
// accepted no activation without tcp; with the webrtc carrier a host behind
// nat is accepted on webrtc alone (C2a). Observable: with --webrtc and no tcp
// port the run starts, lists the carrier, answers on the manager its offers
// arrive on, and stops answering when it ends.
func TestExtenderCommandServesOnTheWebRtcCarrierWhenTcpDoesNotBind(t *testing.T) {
	operator := newTestExtenderOperator(t)
	operatorAddress := operator.server.Listener.Addr().String()
	_, operatorPort, err := net.SplitHostPort(operatorAddress)
	if err != nil {
		t.Fatal(err)
	}
	quicPacketConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dnsPacketConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	quicPort := quicPacketConn.LocalAddr().(*net.UDPAddr).Port

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := connect.NewWebRtcManager(ctx, testDropSignalSender{}, connect.DefaultWebRtcSettings())
	signalingClosed := make(chan struct{})

	runs := make(chan *extenderRun, 1)
	stateDir := t.TempDir()
	options := &extenderOptions{
		jwt:      "test-jwt",
		apiUrl:   fmt.Sprintf("http://%s:%s", testExtenderApiHost, operatorPort),
		keyFile:  filepath.Join(stateDir, "extender.key"),
		stateDir: stateDir,
		tcpPort:  quicPort,
		udpPort:  quicPort,
		dnsPort:  dnsPacketConn.LocalAddr().(*net.UDPAddr).Port,
		listen: func(network string, address string) (net.Listener, error) {
			return nil, errors.New("the test's port is in use")
		},
		listenPacket: func(network string, address string) (net.PacketConn, error) {
			if address == fmt.Sprintf(":%d", quicPort) {
				return quicPacketConn, nil
			}
			return dnsPacketConn, nil
		},
		dialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", operatorAddress)
		},
		packetConnFactory:      testLoopbackPacketConn,
		configureNetworkClient: testHermeticNetworkClient,
		webRtc:                 true,
		webRtcSignaling: func(ctx context.Context) (*connect.WebRtcManager, func(), error) {
			return manager, func() { close(signalingClosed) }, nil
		},
		onStart: func(run *extenderRun) {
			runs <- run
		},
	}
	runDone := make(chan error, 1)
	go func() {
		runDone <- runExtender(ctx, options)
	}()

	var run *extenderRun
	select {
	case run = <-runs:
	case err := <-runDone:
		t.Fatalf("the extender exited: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("the extender did not start")
	}
	carriers := run.server.Carriers()
	if !slices.Contains(carriers, connect.ExtenderCarrierWebRtc) || slices.Contains(carriers, connect.ExtenderCarrierTcp) {
		t.Fatalf("carriers = %v, expected webrtc and no tcp", carriers)
	}
	if !manager.HasExtenderCarrierAnswerer() {
		t.Fatal("the carrier's answerer is not installed on the manager")
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(60 * time.Second):
		t.Fatal("the extender did not end")
	}
	if manager.HasExtenderCarrierAnswerer() {
		t.Fatal("the answerer outlived the run")
	}
	select {
	case <-signalingClosed:
	default:
		t.Fatal("the signaling client was not closed")
	}
}
