// Proves provenance on the actual authenticated H1 route and the retirement
// boundary shared by every physical platform lane.
package connect

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/urnetwork/connect/protocol"
)

// A real H1+ or WebSocket data receipt identifies its own verified TLS and
// login. The unverified WebSocket still carries ordinary data.
func TestPlatformTransportH1VerifiedReceiveProvenance(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		plus        bool
		legacy      bool
		unverified  bool
		fingerprint string
	}{
		{name: "framed-header", plus: true, fingerprint: TlsClientHelloFingerprintChrome},
		{name: "websocket-echo", legacy: true, fingerprint: TlsClientHelloFingerprintGo},
		{name: "unverified-websocket", unverified: true, fingerprint: TlsClientHelloFingerprintChrome},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			resetH1UpgradeTestState(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			clientId := NewId()
			testJwt := lateLocalJwt(t, clientId)
			wire := []byte("synthetic platform H1 Transfer frame")
			serverErrors := make(chan error, 1)
			authArrived := make(chan struct{}, 1)
			echoAuth := make(chan struct{})
			serve := func(w http.ResponseWriter, r *http.Request) error {
				if !testCase.legacy && r.Header.Get("Authorization") != "Bearer "+testJwt {
					http.Error(w, "test login rejected", http.StatusUnauthorized)
					return errors.New("missing exact header login")
				}
				var peer H1MessageConn
				if testCase.plus {
					conn, err := AcceptFramedUpgrade(w, r, H1FramerProtocol, time.Second)
					if err != nil {
						return err
					}
					peer, err = NewFramedMessageConn(conn, H1FramerProtocol, 65535, nil)
					if err != nil {
						conn.Close()
						return err
					}
				} else {
					upgrader := websocket.Upgrader{}
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						return err
					}
					peer = conn
				}
				defer peer.Close()
				if testCase.legacy {
					kind, encodedAuth, err := peer.ReadMessage()
					if err != nil {
						return err
					}
					decoded, err := DecodeFrame(encodedAuth)
					auth, ok := decoded.(*protocol.Auth)
					if err != nil || kind != websocket.BinaryMessage || !ok || auth.ByJwt != testJwt {
						return errors.New("missing exact binary login")
					}
					authArrived <- struct{}{}
					select {
					case <-echoAuth:
					case <-ctx.Done():
						return nil
					}
					if err := peer.WriteMessage(kind, encodedAuth); err != nil {
						return err
					}
				}
				if err := peer.WriteMessage(websocket.BinaryMessage, wire); err != nil {
					return err
				}
				<-ctx.Done()
				return nil
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := serve(w, r); err != nil {
					select {
					case serverErrors <- err:
					case <-ctx.Done():
					}
				}
			}))
			defer func() {
				cancel()
				server.Close()
			}()

			roots := x509.NewCertPool()
			roots.AddCert(server.Certificate())
			strategySettings := DefaultClientStrategySettings()
			strategySettings.EnableNormal = true
			strategySettings.EnableResilient = false
			strategySettings.ConnectSettings.TlsClientHelloFingerprint = testCase.fingerprint
			strategySettings.ConnectSettings.TlsConfig = &tls.Config{
				RootCAs:            roots,
				InsecureSkipVerify: testCase.unverified, // deliberately ineligible arm
			}
			strategy := NewClientStrategy(ctx, strategySettings)
			defer strategy.Close()
			routes := NewRouteManager(ctx, "verified-platform-h1")
			reader := routes.OpenMultiRouteReader(DestinationId(clientId))
			defer routes.CloseMultiRouteReader(reader)
			settings := testingPlatformTransportSettings()
			settings.V2H1Auth = !testCase.legacy
			settings.EnableH1Plus = testCase.plus
			transport := NewPlatformTransportWithTargetMode(
				ctx, strategy, routes, "wss"+strings.TrimPrefix(server.URL, "https"),
				&ClientAuth{ByJwt: testJwt, InstanceId: NewId()}, TransportModeH1, settings,
			)
			closeTransport := func() {
				closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer closeCancel()
				if err := transport.CloseAndWait(closeCtx); err != nil {
					t.Error(err)
				}
			}
			defer closeTransport()

			if testCase.legacy {
				select {
				case <-authArrived:
				case err := <-serverErrors:
					t.Fatal(err)
				case <-ctx.Done():
					t.Fatal("legacy login did not reach the TLS peer")
				}
				if transport.IsConnected() {
					t.Fatal("physical receive routes published before login echo")
				}
				close(echoAuth)
			}

			read := func() *platformReceiveProvenance {
				t.Helper()
				message, disposition, err := reader.(transferCarrierMultiRouteReader).readWithCarrier(ctx, 5*time.Second)
				defer disposition.h1Credit.release()
				defer MessagePoolReturn(message)
				if err != nil || !bytes.Equal(message, wire) || disposition.transportType != TransportTypeH1 {
					select {
					case serverErr := <-serverErrors:
						t.Fatal(serverErr)
					default:
					}
					t.Fatalf("physical H1 receive %q: disposition=%+v err=%v", message, disposition, err)
				}
				return disposition.platformReceive
			}
			receipt := read()
			if testCase.unverified {
				if receipt != nil {
					t.Fatal("unverified H1 issued a platform receive receipt")
				}
			} else if receipt == nil || receipt.lane != platformReceiveH1 || !receipt.withLiveOwner(clientId, func() {}) {
				t.Fatal("verified H1 receive missing its live platform owner")
			}

			closeTransport()
			if receipt.withLiveOwner(clientId, func() {}) {
				t.Fatal("closed H1 owner admitted a retained receipt")
			}
			// A public H1 label on a custom gateway conveys data only.
			labelled := NewReceiveGatewayTransportWithType(TransportTypeH1)
			labelledRoute := make(Route, 1)
			routes.UpdateTransport(labelled, []Route{labelledRoute})
			defer routes.RemoveTransport(labelled)
			labelledRoute <- MessagePoolCopy(wire)
			if read() != nil {
				t.Fatal("custom H1 route acquired platform provenance")
			}
		})
	}
}

// Retirement must join a decision admitted by the older owner. Both lanes of
// a closed QUIC connection refuse a later decision even if still referenced.
func TestPlatformReceiveProvenanceRetirement(t *testing.T) {
	clientId := NewId()
	physicalDone := make(chan struct{})
	owner := newPlatformReceiveOwner(clientId, t.Context().Done(), physicalDone, true)
	receipt := owner.provenance(platformReceiveH3Stream)
	if receipt.withLiveOwner(NewId(), func() { t.Error("wrong client admitted") }) {
		t.Fatal("platform receipt carried over to a different client")
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	committed := make(chan struct{})
	admitted := make(chan bool, 1)
	go func() {
		admitted <- receipt.withLiveOwner(clientId, func() {
			close(entered)
			<-release
			close(committed)
		})
	}()
	select {
	case <-entered:
	case <-admitted:
		t.Fatal("live platform receipt did not enter its commit")
	}
	// Probe the exact retirement mutex while the real commit is held open.
	if owner.stateLock.TryLock() {
		owner.stateLock.Unlock()
		t.Error("platform receipt did not pin its owner throughout the commit")
	}

	retiring := make(chan struct{})
	retired := make(chan bool, 1)
	go func() {
		close(retiring)
		owner.retire()
		select {
		case <-committed:
			retired <- true
		default:
			retired <- false
		}
	}()
	<-retiring
	close(release)
	committedBeforeRetirement := <-retired
	oldDecisionAdmitted := <-admitted
	if !committedBeforeRetirement || !oldDecisionAdmitted || receipt.withLiveOwner(clientId, func() {}) {
		t.Fatal("retirement failed to separate the old and new decision")
	}

	live := newPlatformReceiveOwner(clientId, t.Context().Done(), physicalDone, true)
	close(physicalDone)
	for _, lane := range []platformReceiveLane{platformReceiveH3Stream, platformReceiveH3Datagram} {
		if live.provenance(lane).withLiveOwner(clientId, func() {}) {
			t.Errorf("lane %d admitted a physically closed QUIC connection", lane)
		}
	}
}
