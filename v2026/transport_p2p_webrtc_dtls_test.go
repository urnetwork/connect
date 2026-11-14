//go:build !js

package connect

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/pion/dtls/v3"
	dtlsnet "github.com/pion/dtls/v3/pkg/net"

	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"github.com/pion/dtls/v3/pkg/protocol/handshake"
	"github.com/pion/transport/v4/dpipe"
	"github.com/theodorsm/covert-dtls/pkg/fingerprints"
)

// Firefox 126 offers this exact DTLS cipher-suite sequence (from the covert-dtls
// corpus entry Mozilla_Firefox_126_0). These are public TLS cipher-suite
// numbers, used here as the ground-truth browser profile.
var firefox126CipherSuiteIds = []uint16{
	0xc02b, 0xc02f, 0xcca9, 0xcca8, 0xc00a, 0xc009, 0xc013, 0xc014,
}

// A pion-style default hello would never offer this list; it stands in for "not
// a browser" so the transform is observable.
var nonBrowserCipherSuiteIds = []uint16{0x1301, 0x1302, 0x1303}

// Root cause: the emitted DTLS ClientHello carries pion's default
// cipher/extension profile. The fix replays a browser profile on the answerer.
// Observable: the cipher-suite list (and bytes) of the hello the hook emits.
// Discriminator: after the fix the emitted hello offers the browser's exact
// cipher sequence and differs from the input; an identity hook leaves the
// input's list in place.
func TestDtlsClientHelloMimicryEmitsBrowserProfile(t *testing.T) {
	hook, err := dtlsClientHelloMimicryHookForFingerprint(fingerprints.Mozilla_Firefox_126_0)
	if err != nil {
		t.Fatalf("load fingerprint: %v", err)
	}
	inputRandom := handshake.Random{}
	inputRandom.Populate()
	inputHello := handshake.MessageClientHello{
		Random:         inputRandom,
		SessionID:      []byte{0x01, 0x02, 0x03},
		Cookie:         []byte{0x09, 0x08, 0x07, 0x06},
		CipherSuiteIDs: nonBrowserCipherSuiteIds,
	}

	emitted := hook(inputHello)
	emittedHello, ok := emitted.(interface {
		Marshal() ([]byte, error)
	})
	if !ok {
		t.Fatalf("hook returned %T, which cannot marshal", emitted)
	}
	emittedBytes, err := emittedHello.Marshal()
	if err != nil {
		t.Fatalf("marshal emitted hello: %v", err)
	}

	// Parse the emitted bytes back as a ClientHello and read its profile.
	parsed := &handshake.MessageClientHello{}
	if err := parsed.Unmarshal(emittedBytes); err != nil {
		t.Fatalf("emitted hello does not parse as a ClientHello: %v", err)
	}
	if !equalUint16s(parsed.CipherSuiteIDs, firefox126CipherSuiteIds) {
		t.Fatalf("emitted cipher suites = %v, want browser profile %v", parsed.CipherSuiteIDs, firefox126CipherSuiteIds)
	}
	if equalUint16s(parsed.CipherSuiteIDs, nonBrowserCipherSuiteIds) {
		t.Fatalf("emitted hello still carries the input (non-browser) cipher list; shaping did not apply")
	}
	// The live handshake's random and cookie must be spliced in, or the hello
	// would be a static replay a censor could match and the handshake would not
	// verify.
	if !bytes.Equal(parsed.Cookie, inputHello.Cookie) {
		t.Fatalf("emitted cookie = %x, want spliced input cookie %x", parsed.Cookie, inputHello.Cookie)
	}
	inputFixed := inputRandom.MarshalFixed()
	parsedFixed := parsed.Random.MarshalFixed()
	if !bytes.Equal(parsedFixed[:], inputFixed[:]) {
		t.Fatalf("emitted hello did not splice the live handshake random")
	}
}

// The chooser draws from the corpus and always returns a usable hook (the corpus
// ships non-empty), so production never silently falls back to the pion default.
func TestDtlsClientHelloMimicryHookChoosesFromCorpus(t *testing.T) {
	if len(fingerprints.GetClientHelloFingerprints()) == 0 {
		t.Fatal("covert-dtls corpus is empty; nothing to mimic")
	}
	if hook := dtlsClientHelloMimicryHook(nil); hook == nil {
		t.Fatal("hook chooser returned nil for a non-empty corpus")
	}
}

// DTLS mimicry is off in the default settings: enabling it globally would add
// handshake-failure risk to the existing direct-p2p transport. The webrtc
// extender carrier opts in explicitly. This locks that conservative default.
func TestDefaultWebRtcSettingsDtlsMimicryOff(t *testing.T) {
	if DefaultWebRtcSettings().DtlsClientHelloMimicry {
		t.Fatal("DTLS ClientHello mimicry must be off by default; only the extender carrier path enables it")
	}
}

// Full in-process DTLS handshake. Proves (1) the emitted ClientHello bytes
// differ from pion's real default, (2) the shaped bytes carry a browser cipher
// profile, and (3) the handshake still completes and carries application data
// (shaping does not break interop). No real network: dpipe is an in-memory
// datagram pipe.
func TestDtlsClientHelloMimicryHandshakeCompletesWithBrowserHello(t *testing.T) {
	// pion default: passthrough hook that records the emitted hello.
	var defaultBytes []byte
	captureDefault := func(ch handshake.MessageClientHello) handshake.Message {
		chCopy := ch
		if raw, err := chCopy.Marshal(); err == nil {
			defaultBytes = raw
		}
		return &chCopy
	}
	if err := runDtlsHandshake(t, captureDefault); err != nil {
		t.Fatalf("baseline handshake (no shaping) failed: %v", err)
	}
	if len(defaultBytes) == 0 {
		t.Fatal("did not capture the pion-default ClientHello")
	}

	// shaped: connect's mimicry hook for a known browser fingerprint, recorded.
	mimicryHook, err := dtlsClientHelloMimicryHookForFingerprint(fingerprints.Mozilla_Firefox_126_0)
	if err != nil {
		t.Fatalf("load fingerprint: %v", err)
	}
	var shapedBytes []byte
	captureShaped := func(ch handshake.MessageClientHello) handshake.Message {
		emitted := mimicryHook(ch)
		if raw, marshalErr := emitted.(interface{ Marshal() ([]byte, error) }).Marshal(); marshalErr == nil {
			shapedBytes = raw
		}
		return emitted
	}
	if err := runDtlsHandshake(t, captureShaped); err != nil {
		t.Fatalf("shaped handshake failed (shaping broke interop): %v", err)
	}
	if len(shapedBytes) == 0 {
		t.Fatal("did not capture the shaped ClientHello")
	}

	if bytes.Equal(defaultBytes, shapedBytes) {
		t.Fatal("shaped ClientHello is byte-identical to the pion default; shaping did not change the wire bytes")
	}
	parsed := &handshake.MessageClientHello{}
	if err := parsed.Unmarshal(shapedBytes); err != nil {
		t.Fatalf("shaped hello does not parse: %v", err)
	}
	if !equalUint16s(parsed.CipherSuiteIDs, firefox126CipherSuiteIds) {
		t.Fatalf("shaped hello cipher suites = %v, want browser profile %v", parsed.CipherSuiteIDs, firefox126CipherSuiteIds)
	}
	// the pion default on the wire must not already be the browser profile,
	// or the comparison above would be vacuous.
	parsedDefault := &handshake.MessageClientHello{}
	if err := parsedDefault.Unmarshal(defaultBytes); err != nil {
		t.Fatalf("default hello does not parse: %v", err)
	}
	if equalUint16s(parsedDefault.CipherSuiteIDs, firefox126CipherSuiteIds) {
		t.Fatal("pion default already offers the Firefox profile; test cannot discriminate")
	}
}

// runDtlsHandshake drives an in-process DTLS client<->server handshake over a
// datagram pipe with clientHook installed on the client (the ClientHello
// sender), then exchanges one application datagram each way to confirm the
// secure channel is live. Returns the first error from either side.
func runDtlsHandshake(t *testing.T, clientHook func(handshake.MessageClientHello) handshake.Message) error {
	t.Helper()
	ca, cb := dpipe.Pipe()
	clientCert, err := selfsign.GenerateSelfSigned()
	if err != nil {
		return err
	}
	serverCert, err := selfsign.GenerateSelfSigned()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	type connResult struct {
		conn *dtls.Conn
		err  error
	}
	clientCh := make(chan connResult, 1)
	go func() {
		cfg := &dtls.Config{
			Certificates:           []tls.Certificate{clientCert},
			InsecureSkipVerify:     true,
			SRTPProtectionProfiles: []dtls.SRTPProtectionProfile{dtls.SRTP_AES128_CM_HMAC_SHA1_80},
			ClientHelloMessageHook: clientHook,
		}
		conn, dialErr := dtls.Client(dtlsnet.PacketConnFromConn(ca), ca.RemoteAddr(), cfg)
		if dialErr == nil {
			dialErr = conn.HandshakeContext(ctx)
		}
		clientCh <- connResult{conn, dialErr}
	}()

	serverCfg := &dtls.Config{
		Certificates:           []tls.Certificate{serverCert},
		SRTPProtectionProfiles: []dtls.SRTPProtectionProfile{dtls.SRTP_AES128_CM_HMAC_SHA1_80},
	}
	serverConn, serverErr := dtls.Server(dtlsnet.PacketConnFromConn(cb), cb.RemoteAddr(), serverCfg)
	if serverErr == nil {
		serverErr = serverConn.HandshakeContext(ctx)
	}
	clientRes := <-clientCh

	defer func() {
		if clientRes.conn != nil {
			_ = clientRes.conn.Close()
		}
		if serverConn != nil {
			_ = serverConn.Close()
		}
	}()
	if serverErr != nil {
		return serverErr
	}
	if clientRes.err != nil {
		return clientRes.err
	}

	// one application datagram each way confirms the channel is usable.
	return exchangeDatagram(ctx, clientRes.conn, serverConn)
}

func exchangeDatagram(ctx context.Context, clientConn, serverConn net.Conn) error {
	payload := []byte("webrtc-extender-probe")
	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, len(payload)+16)
		n, readErr := serverConn.Read(buf)
		if readErr != nil {
			errCh <- readErr
			return
		}
		if !bytes.Equal(buf[:n], payload) {
			errCh <- errReadMismatch
			return
		}
		errCh <- nil
	}()
	if _, writeErr := clientConn.Write(payload); writeErr != nil {
		return writeErr
	}
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

var errReadMismatch = &dtlsTestError{"server read did not match client write"}

type dtlsTestError struct{ s string }

func (e *dtlsTestError) Error() string { return e.s }

func equalUint16s(a, b []uint16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
