package connect

// transport_credential_refusal_test.go -- the hold a platform transport takes
// on a credential the platform definitively refused
// (transport_credential_refusal.go).
//
// A transport reports the exact point it parks on the hold through
// credentialRefusalHoldForTest. Once parked, a runner cannot dial the refused
// credential until the hold changes, so "no redial while held" is read at a
// barrier, not after a sleep. The hold's own expiry runs on the manual clock of
// the client limit tests.

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	quic "github.com/quic-go/quic-go"

	"github.com/urnetwork/connect/protocol"
)

const (
	testingRefusedCredential = "testing-refused"
	testingRenewedCredential = "testing-renewed"
)

// newTestingCredentialRefusalHold is a hold on the manual clock with no jitter.
func newTestingCredentialRefusalHold(clock *testingClientLimitClock) *credentialRefusalHold {
	hold := newCredentialRefusalHold()
	hold.now = clock.Now
	hold.afterFunc = clock.AfterFunc
	hold.jitter = func(time.Duration) time.Duration {
		return 0
	}
	return hold
}

// A hold ends only for a different credential or at its timeout, and a second
// refusal inside it never shortens it.
func TestCredentialRefusalHoldEndsForANewCredentialOrAtItsTimeout(t *testing.T) {
	clock := newTestingClientLimitClock()
	hold := newTestingCredentialRefusalHold(clock)
	start := clock.Now()
	if retryTime := hold.refuse(testingRefusedCredential); !retryTime.Equal(start.Add(CredentialRefusalHoldTimeout)) {
		t.Fatalf("hold until %s, want %s", retryTime, start.Add(CredentialRefusalHoldTimeout))
	}
	status, notify := hold.get()
	if status.byJwt != testingRefusedCredential {
		t.Fatalf("hold = %+v, want the refused credential", status)
	}
	hold.release(testingRefusedCredential)
	if clientLimitNotified(notify) {
		t.Fatal("installing the refused credential again ended its hold")
	}
	clock.advance(CredentialRefusalHoldTimeout / 2)
	if retryTime := hold.refuse(testingRefusedCredential); !retryTime.Equal(clock.Now().Add(CredentialRefusalHoldTimeout)) {
		t.Fatalf("a later refusal held until %s, want %s", retryTime, clock.Now().Add(CredentialRefusalHoldTimeout))
	}
	_, notify = hold.get()
	clock.advance(CredentialRefusalHoldTimeout - time.Nanosecond)
	if clientLimitNotified(notify) {
		t.Fatal("the hold ended before its timeout")
	}
	clock.advance(time.Nanosecond)
	if !clientLimitNotified(notify) {
		t.Fatal("the hold did not end at its timeout")
	}
	if status, _ := hold.get(); status.byJwt != "" {
		t.Fatalf("hold after its timeout = %+v, want none", status)
	}

	hold.refuse(testingRefusedCredential)
	_, notify = hold.get()
	hold.release(testingRenewedCredential)
	if !clientLimitNotified(notify) {
		t.Fatal("a different credential did not end the hold")
	}
	// the stopped expiry of the ended hold cannot end a later one
	hold.refuse(testingRefusedCredential)
	_, notify = hold.get()
	clock.advance(CredentialRefusalHoldTimeout - time.Nanosecond)
	if clientLimitNotified(notify) {
		t.Fatal("an earlier hold's expiry ended a later one")
	}
}

// Only an answer about the credential is a refusal. A failed check, a
// redirect and a transport failure are not.
func TestCredentialRefusalCauseReadsOnlyDefinitiveRefusals(t *testing.T) {
	for _, control := range []struct {
		name string
		err  error
		want AuthorizationCloseCause
	}{
		{name: "h1+ 401", err: &HTTPUpgradeError{StatusCode: http.StatusUnauthorized, Reason: "rejected", Terminal: true}, want: AuthorizationCloseRejected},
		{name: "h1+ 403", err: &HTTPUpgradeError{StatusCode: http.StatusForbidden, Reason: "rejected", Terminal: true}, want: AuthorizationCloseRejected},
		{name: "h1+ 401 after a capability miss", err: errors.Join(&HTTPUpgradeError{StatusCode: http.StatusUpgradeRequired, Reason: "rejected"}, &HTTPUpgradeError{StatusCode: http.StatusUnauthorized, Reason: "rejected", Terminal: true}), want: AuthorizationCloseRejected},
		{name: "h1+ redirect", err: &HTTPUpgradeError{StatusCode: http.StatusFound, Reason: "rejected", Terminal: true}, want: 0},
		{name: "h1+ 503", err: &HTTPUpgradeError{StatusCode: http.StatusServiceUnavailable, Reason: "rejected"}, want: 0},
		{name: "invalid url", err: &HTTPUpgradeError{Reason: "invalid-url", Terminal: true}, want: 0},
		{name: "websocket rejected", err: &websocket.CloseError{Code: int(AuthorizationCloseRejected)}, want: AuthorizationCloseRejected},
		{name: "websocket revoked", err: &websocket.CloseError{Code: int(AuthorizationCloseRevoked)}, want: AuthorizationCloseRevoked},
		{name: "websocket expired", err: &websocket.CloseError{Code: int(AuthorizationCloseExpired)}, want: AuthorizationCloseExpired},
		{name: "websocket unavailable", err: &websocket.CloseError{Code: int(AuthorizationCloseUnavailable)}, want: 0},
		{name: "websocket client limit", err: &websocket.CloseError{Code: ClientLimitCloseCode}, want: 0},
		{name: "quic rejected", err: &quic.ApplicationError{Remote: true, ErrorCode: quic.ApplicationErrorCode(AuthorizationCloseRejected)}, want: AuthorizationCloseRejected},
		{name: "quic local close", err: &quic.ApplicationError{ErrorCode: quic.ApplicationErrorCode(AuthorizationCloseRejected)}, want: 0},
		{name: "frame auth refusal", err: &transportAuthorizationRefusal{cause: AuthorizationCloseRevoked}, want: AuthorizationCloseRevoked},
		{name: "timeout", err: errors.New("Timeout."), want: 0},
	} {
		if got := credentialRefusalCause(control.err); got != control.want {
			t.Errorf("%s: cause = %d, want %d", control.name, got, control.want)
		}
	}
}

// testingCredentialRefusalDial is one dial the h1 platform received.
type testingCredentialRefusalDial struct {
	byJwt  string
	framed bool
}

// testingCredentialRefusalPlatform is an h1 platform that refuses one
// credential the way the platform does on each carrier: h1+ with a terminal
// 401 before its upgrade, a WebSocket after its 101 with the close control
// and close frame of refuseReason. Every other credential is accepted, and a
// frame-auth credential is echoed.
type testingCredentialRefusalPlatform struct {
	url          string
	refuseReason uint32
	dials        chan testingCredentialRefusalDial
	acceptCount  atomic.Int64
	// closed when the test ends, so a handler never waits on a full queue
	// that a failed test stopped reading
	closed chan struct{}
}

func newTestingCredentialRefusalPlatform(t *testing.T, refuseReason uint32) *testingCredentialRefusalPlatform {
	t.Helper()
	platform := &testingCredentialRefusalPlatform{
		refuseReason: refuseReason,
		dials:        make(chan testingCredentialRefusalDial, 64),
		closed:       make(chan struct{}),
	}
	var handlers sync.WaitGroup
	server := newTestingLoopbackHttpServer(t, 4, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		dial := testingCredentialRefusalDial{
			byJwt:  strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
			framed: r.Header.Get("Upgrade") == H1FramerProtocol,
		}
		record := func() bool {
			select {
			case platform.dials <- dial:
				return true
			case <-platform.closed:
				return false
			}
		}
		var conn H1MessageConn
		if dial.framed {
			if dial.byJwt == testingRefusedCredential {
				record()
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			raw, err := AcceptFramedUpgrade(w, r, H1FramerProtocol, time.Second)
			if err != nil {
				return
			}
			framed, err := NewFramedMessageConn(raw, H1FramerProtocol, 65535, nil)
			if err != nil {
				raw.Close()
				return
			}
			conn = framed
		} else {
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			ws, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			conn = ws
		}
		defer conn.Close()
		var echo []byte
		if dial.byJwt == "" {
			// frame auth: the credential is the first message
			_, authBytes, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if decoded, err := DecodeFrame(authBytes); err == nil {
				if auth, ok := decoded.(*protocol.Auth); ok {
					dial.byJwt = auth.ByJwt
				}
			}
			echo = authBytes
		}
		if !record() {
			return
		}
		if dial.byJwt == testingRefusedCredential {
			control := []byte{TransportControlClose, byte(platform.refuseReason >> 24), byte(platform.refuseReason >> 16), byte(platform.refuseReason >> 8), byte(platform.refuseReason)}
			_ = conn.WriteMessage(websocket.BinaryMessage, control)
			if ws, ok := conn.(*websocket.Conn); ok {
				_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(int(platform.refuseReason)+4000, "authorization ended"), time.Now().Add(time.Second))
			}
			return
		}
		if echo != nil {
			if err := conn.WriteMessage(websocket.BinaryMessage, echo); err != nil {
				return
			}
		}
		platform.acceptCount.Add(1)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}), false)
	platform.url = "ws" + strings.TrimPrefix(server.URL, "http")
	t.Cleanup(func() {
		close(platform.closed)
		server.CloseClientConnections()
		server.Close()
		handlers.Wait()
	})
	return platform
}

func (self *testingCredentialRefusalPlatform) nextDial(t *testing.T) testingCredentialRefusalDial {
	t.Helper()
	select {
	case dial := <-self.dials:
		return dial
	case <-time.After(15 * time.Second):
		t.Fatal("no platform dial arrived")
		return testingCredentialRefusalDial{}
	}
}

// testingCredentialRefusalOwner is the owner side of a transport: the causes
// reported through AuthorizationClosed and each park on the refusal hold.
type testingCredentialRefusalOwner struct {
	causes chan AuthorizationCloseCause
	parked chan struct{}
}

func newTestingCredentialRefusalOwner(settings *PlatformTransportSettings) *testingCredentialRefusalOwner {
	owner := &testingCredentialRefusalOwner{
		causes: make(chan AuthorizationCloseCause, 64),
		parked: make(chan struct{}, 64),
	}
	settings.AuthorizationClosed = func(cause AuthorizationCloseCause) {
		select {
		case owner.causes <- cause:
		default:
		}
	}
	settings.credentialRefusalHoldForTest = func() {
		select {
		case owner.parked <- struct{}{}:
		default:
		}
	}
	return owner
}

// waitRefusalHeld reads the owner's report and the park on the hold, failing
// on the first dial of the refused credential that arrives instead: the
// defect is a redial of a credential the platform already refused.
func (self *testingCredentialRefusalOwner) waitRefusalHeld(t *testing.T, label string, dials <-chan testingCredentialRefusalDial) {
	t.Helper()
	reported, parked := false, false
	for !reported || !parked {
		select {
		case cause := <-self.causes:
			if cause != AuthorizationCloseRejected {
				t.Fatalf("%s: the owner heard cause %d, want %d", label, cause, AuthorizationCloseRejected)
			}
			reported = true
		case <-self.parked:
			parked = true
		case dial := <-dials:
			t.Fatalf("%s: redialed credential %q after the platform refused it (reported = %t, held = %t)", label, dial.byJwt, reported, parked)
		case <-time.After(15 * time.Second):
			t.Fatalf("%s: the refusal was never held (reported = %t, held = %t)", label, reported, parked)
		}
	}
}

func newTestingCredentialRefusalTransport(t *testing.T, ctx context.Context, platformUrl string, settings *PlatformTransportSettings, mode TransportMode) *PlatformTransport {
	t.Helper()
	settings.ReconnectTimeout = time.Millisecond
	strategySettings := DefaultClientStrategySettings()
	strategySettings.EnableNormal = true
	strategySettings.EnableResilient = false
	strategy := NewClientStrategy(ctx, strategySettings)
	t.Cleanup(strategy.Close)
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		strategy,
		NewRouteManager(ctx, "credential-refusal"),
		platformUrl,
		&ClientAuth{
			ByJwt:      testingRefusedCredential,
			InstanceId: NewId(),
			AppVersion: "testing",
		},
		mode,
		settings,
	)
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := transport.CloseAndWait(closeCtx); err != nil {
			t.Errorf("transport did not join: %v", err)
		}
	})
	return transport
}

// The storm: a WebSocket refusal after its 101 looked like a connection that
// ran and died, and a refused h1+ dial only backed off, so a refused
// credential was redialed for as long as the transport ran. Every h1 refusal
// now reaches the owner and holds the credential until the owner installs a
// new one, which dials at once.
func TestPlatformTransportH1RefusalHoldsTheRefusedCredential(t *testing.T) {
	resetH1UpgradeTestState(t)
	for _, carrier := range []struct {
		name      string
		framed    bool
		frameAuth bool
	}{
		{name: "websocket"},
		{name: "websocket frame auth", frameAuth: true},
		{name: "h1+", framed: true},
	} {
		func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			platform := newTestingCredentialRefusalPlatform(t, TransportCloseReasonCredentialRejected)
			settings := testingPlatformTransportSettings()
			settings.EnableH1Plus = carrier.framed
			settings.V2H1Auth = !carrier.frameAuth
			owner := newTestingCredentialRefusalOwner(settings)
			transport := newTestingCredentialRefusalTransport(t, ctx, platform.url, settings, TransportModeH1)

			if first := platform.nextDial(t); first.byJwt != testingRefusedCredential || first.framed != carrier.framed {
				t.Fatalf("%s: first dial = %+v", carrier.name, first)
			}
			owner.waitRefusalHeld(t, carrier.name, platform.dials)
			if refused, _ := transport.credentialRefused(); !refused {
				t.Fatalf("%s: the parked transport holds no refusal", carrier.name)
			}

			transport.SetAuth(&ClientAuth{ByJwt: testingRenewedCredential, InstanceId: NewId(), AppVersion: "testing"})
			if second := platform.nextDial(t); second.byJwt != testingRenewedCredential {
				t.Fatalf("%s: the dial after the new credential carried %q", carrier.name, second.byJwt)
			}
			if !waitForCondition(10*time.Second, transport.IsConnected) {
				t.Fatalf("%s: the new credential never connected", carrier.name)
			}
		}()
	}
}

// A close for an unavailable check is no answer about the credential: the
// transport redials it at once and takes no hold.
func TestPlatformTransportH1UnavailableCloseTakesNoCredentialHold(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	platform := newTestingCredentialRefusalPlatform(t, TransportCloseReasonAuthorizationUnavailable)
	settings := testingPlatformTransportSettings()
	owner := newTestingCredentialRefusalOwner(settings)
	newTestingCredentialRefusalTransport(t, ctx, platform.url, settings, TransportModeH1)

	for range 3 {
		if dial := platform.nextDial(t); dial.byJwt != testingRefusedCredential {
			t.Fatalf("dial carried %q", dial.byJwt)
		}
	}
	select {
	case <-owner.parked:
		t.Fatal("an unavailable close held the credential")
	default:
	}
}

// testingCredentialRefusalH3Platform is a QUIC platform that closes a refused
// credential's connection with the authorization close right after its auth
// frame, and echoes every other credential.
type testingCredentialRefusalH3Platform struct {
	port       int
	nextProto  string
	framerSets *FramerSettings
	dials      chan testingCredentialRefusalDial
}

func newTestingCredentialRefusalH3Platform(t *testing.T) *testingCredentialRefusalH3Platform {
	t.Helper()
	host := testLoopbackIp(4)
	certPem, keyPem, err := selfSign([]string{host}, host, 24*time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		t.Fatal(err)
	}
	nextProto := "urnetwork-platform-credential-refusal-test"
	listener, err := quic.ListenAddrEarly(
		testLoopbackHostPort(4, 0),
		&tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{nextProto},
		},
		&quic.Config{MaxIdleTimeout: 30 * time.Second},
	)
	if err != nil {
		t.Fatal(err)
	}
	serverCtx, cancel := context.WithCancel(context.Background())
	platform := &testingCredentialRefusalH3Platform{
		port:       listener.Addr().(*net.UDPAddr).Port,
		nextProto:  nextProto,
		framerSets: DefaultFramerSettings(int(DefaultClientSettings().MinimumMessageLenLimit())),
		dials:      make(chan testingCredentialRefusalDial, 64),
	}
	var handlers sync.WaitGroup
	handlers.Add(1)
	go func() {
		defer handlers.Done()
		for {
			conn, err := listener.Accept(serverCtx)
			if err != nil {
				return
			}
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				stream, err := conn.AcceptStream(serverCtx)
				if err != nil {
					return
				}
				framer := NewFramer(platform.framerSets)
				authBytes, err := framer.Read(stream)
				if err != nil {
					return
				}
				defer MessagePoolReturn(authBytes)
				dial := testingCredentialRefusalDial{}
				if decoded, err := DecodeFrame(authBytes); err == nil {
					if auth, ok := decoded.(*protocol.Auth); ok {
						dial.byJwt = auth.ByJwt
					}
				}
				select {
				case platform.dials <- dial:
				case <-serverCtx.Done():
					return
				}
				if dial.byJwt == testingRefusedCredential {
					conn.CloseWithError(quic.ApplicationErrorCode(AuthorizationCloseRejected), "authorization refused")
					return
				}
				if err := framer.Write(stream, authBytes); err != nil {
					return
				}
				for {
					message, err := framer.Read(stream)
					if err != nil {
						return
					}
					MessagePoolReturn(message)
				}
			}()
		}
	}()
	t.Cleanup(func() {
		cancel()
		listener.Close()
		handlers.Wait()
	})
	return platform
}

func (self *testingCredentialRefusalH3Platform) nextDial(t *testing.T) testingCredentialRefusalDial {
	t.Helper()
	select {
	case dial := <-self.dials:
		return dial
	case <-time.After(15 * time.Second):
		t.Fatal("no h3 auth frame arrived")
		return testingCredentialRefusalDial{}
	}
}

// H3 refuses a credential with a QUIC application close; the transport
// reported it but redialed the refused credential. It now holds it like h1.
func TestPlatformTransportH3RefusalHoldsTheRefusedCredential(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	platform := newTestingCredentialRefusalH3Platform(t)
	settings := testingFamilyTransportSettings()
	settings.H3Port = platform.port
	settings.QuicConnectTimeout = 2 * time.Second
	settings.QuicHandshakeTimeout = 2 * time.Second
	settings.QuicTlsConfig = &tls.Config{
		InsecureSkipVerify: true, // test-only self-signed endpoint
		NextProtos:         []string{platform.nextProto},
	}
	settings.FramerSettings = platform.framerSets
	owner := newTestingCredentialRefusalOwner(settings)
	transport := newTestingCredentialRefusalTransport(t, ctx, "https://"+testLoopbackHost(4), settings, TransportModeH3)

	if first := platform.nextDial(t); first.byJwt != testingRefusedCredential {
		t.Fatalf("first h3 dial carried %q", first.byJwt)
	}
	owner.waitRefusalHeld(t, "h3", platform.dials)
	transport.SetAuth(&ClientAuth{ByJwt: testingRenewedCredential, InstanceId: NewId(), AppVersion: "testing"})
	if second := platform.nextDial(t); second.byJwt != testingRenewedCredential {
		t.Fatalf("the h3 dial after the new credential carried %q", second.byJwt)
	}
	if !waitForCondition(10*time.Second, transport.IsConnected) {
		t.Fatal("the new credential never connected over h3")
	}
}
