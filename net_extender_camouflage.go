package connect

// net_extender_camouflage.go — the client half of the extender camouflage
// (EXTENDER.md P8).
//
// The camouflaged variant folds into the existing tcp carrier as one dialer,
// one strategy slot and one directory outcome — not a second carrier and not a
// second priority — the way the dns-port race is one dial over several ports
// (net_extender_dns_ports.go). The tcp dial becomes a camo-first-then-legacy
// staggered race: it launches the camouflaged attempt at once and the legacy
// attempt one stagger later while the first is pending, or at once if the first
// fails; the first to answer wins, the other is canceled and joined.
//
// The camouflaged attempt fronts a borrowed name with a faithful browser hello
// (the same Chrome hello the normal and resilient dialers present,
// net_tls_hello.go) whose 32-byte legacy session id seals the client's proof of
// the extender key (P1, generalized from vless_reality.go). The extender that
// recognizes the tag terminates with its identity leaf and negotiates http/1.1
// alone (P4), so the client's B3 leaf check passes and the ordinary A3 request
// runs inside. An extender that does not recognize the tag either terminates
// with {h2,http/1.1} (Phase A) — where the faithful hello negotiates h2 and the
// attempt cannot speak the http/1.1 extender request — or splices the hello to
// the real borrowed site (Phase B) — where the real site's certificate fails
// the B3 check. Either way the camouflaged attempt fails and the race falls to
// the legacy attempt, which in Phase A still reaches the extender and in Phase B
// is itself spliced and fails, so a client whose clock skew kept the tag from
// opening loses the tcp carrier and reaches the extender over its udp carriers.
//
// The kill switch is the sibling branch's ConnectSettings.TlsClientHelloFingerprint
// set to "go", which drops the camouflaged variant back to the legacy Go-TLS
// dial (P7).

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"

	"github.com/urnetwork/connect/protocol"
)

// The launch stagger between the camouflaged and legacy attempts (P8), the same
// stagger the dns carrier's ports race by (extenderDnsPortRaceStagger).
const extenderTcpCamouflageStagger = 250 * time.Millisecond

// The alpn a faithful Chrome hello offers (P4). The extender negotiates
// http/1.1 alone on the authenticated path, so the camouflaged attempt is
// authenticated only when it settles on http/1.1; a connection that settled on
// h2 is an unauthenticated terminate (Phase A) the attempt must not try to speak
// the extender request over.
var extenderCamouflageAlpn = []string{"h2", "http/1.1"}

// The stagger clock the race reads; a test replaces it.
func extenderTcpCamouflageStaggerC() <-chan time.Time {
	return time.After(extenderTcpCamouflageStagger)
}

// Whether the tcp dial runs the camouflaged attempt (P7, P8): the config
// carries both the camouflage static key and the identity key the B3 check
// needs, and the fingerprint kill switch is not "go".
func extenderCamouflageApplies(connectSettings *ConnectSettings, extenderConfig *ExtenderConfig) bool {
	if len(extenderConfig.RealityPublicKey) != extenderRealityX25519PublicKeyByteCount {
		return false
	}
	if len(extenderConfig.PublicKey) != ed25519.PublicKeySize {
		return false
	}
	return connectSettings.TlsClientHelloFingerprint != TlsClientHelloFingerprintGo
}

// The borrowed name the camouflaged attempt fronts (P5), drawn at dial time
// from the bundled borrow list so a later dial fronts a different one. Empty
// when no borrow name is bundled, which drops the camouflaged attempt and dials
// the legacy one alone — the same degrade-to-legacy an extender that verified no
// borrow name produces by publishing no camouflage key.
func extenderCamouflageFrontName(extenderConfig *ExtenderConfig) string {
	borrowDomains := BorrowDomains()
	if len(borrowDomains) == 0 {
		return ""
	}
	return borrowDomains[rand.IntN(len(borrowDomains))]
}

// The camouflaged tcp attempt: the Go-TLS dial replaced by an authenticated
// browser hello fronting frontName. It returns the raw stream; dialExtenderTcp
// wraps the carrier's one memory reservation around the winner.
func dialExtenderTcpCamouflageConn(
	ctx context.Context,
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
	frontName string,
	headerBytes []byte,
	roundTrip *ExtenderRoundTrip,
) (net.Conn, *protocol.ExtenderResponse, error) {
	authority := net.JoinHostPort(
		extenderConfig.Ip.String(),
		fmt.Sprintf("%d", extenderConfig.Profile.Port),
	)
	conn, err := connectSettings.DialContext(ctx, "tcp", authority)
	if err != nil {
		return nil, nil, err
	}
	success := false
	defer func() {
		if !success {
			conn.Close()
		}
	}()

	// the resilient fragment and reorder wrapping applies to the camouflaged
	// attempt as it does to the legacy one; the extender reassembles the
	// fragmented hello during its peek (P3, P8)
	var handshakeConn net.Conn = conn
	var rconn *ResilientTlsConn
	if extenderConfig.Profile.Fragment || extenderConfig.Profile.Reorder {
		rconn = NewResilientTlsConn(conn, extenderConfig.Profile.Fragment, extenderConfig.Profile.Reorder)
		handshakeConn = rconn
	}

	var serverConn net.Conn
	func() {
		tlsCtx, tlsCancel := context.WithTimeout(ctx, connectSettings.TlsTimeout)
		defer tlsCancel()
		serverConn, err = extenderCamouflageClientHandshake(tlsCtx, handshakeConn, extenderConfig, frontName)
	}()
	if err != nil {
		return nil, nil, err
	}
	if rconn != nil {
		if err := offResilientTlsConn(ctx, rconn, connectSettings.ConnectTimeout); err != nil {
			return nil, nil, err
		}
	}

	streamConn, response, err := extenderStreamRequest(
		ctx,
		connectSettings,
		extenderConfig,
		serverConn,
		headerBytes,
		roundTrip,
	)
	if err != nil {
		return nil, nil, err
	}

	success = true
	return streamConn, response, nil
}

// Runs the authenticated browser handshake over conn (P1): a faithful Chrome
// hello whose session id seals the client's proof under the extender static
// key, the leaf checked against the record identity key (B3). On success the
// connection must have negotiated http/1.1 (P4) — an h2 or empty negotiation is
// an unauthenticated terminate that cannot carry the extender request, which is
// a failure the tcp race falls to the legacy attempt on. The connection is
// closed on error.
func extenderCamouflageClientHandshake(
	ctx context.Context,
	conn net.Conn,
	extenderConfig *ExtenderConfig,
	frontName string,
) (net.Conn, error) {
	serverStaticPublicKey, err := ecdh.X25519().NewPublicKey(extenderConfig.RealityPublicKey)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("extender reality public key is invalid: %w", err)
	}
	spec, err := chromeClientHelloSpec(extenderCamouflageAlpn)
	if err != nil {
		conn.Close()
		return nil, err
	}

	uconfig := &utls.Config{
		ServerName: frontName,
		// the leaf is checked below against the record key (B3); the browser
		// trust store never sees it
		InsecureSkipVerify:     true,
		SessionTicketsDisabled: true,
		OmitEmptyPsk:           true,
		VerifyPeerCertificate:  newExtenderLeafVerifier(extenderConfig.PublicKey),
	}
	uconn := utls.UClient(conn, uconfig, utls.HelloCustom)
	success := false
	defer func() {
		if !success {
			uconn.Close()
		}
	}()
	if err := uconn.ApplyPreset(spec); err != nil {
		return nil, err
	}
	// marshal the hello so its random, session id and key shares are set before
	// the tag is sealed into the session id
	if err := uconn.BuildHandshakeState(); err != nil {
		return nil, err
	}
	hello := uconn.HandshakeState.Hello
	if len(hello.Raw) < vlessRealitySessionIdOffset+32 || len(hello.SessionId) != 32 || len(hello.Random) != 32 {
		return nil, errors.New("extender reality: the browser hello has no 32-byte session id")
	}
	clientEphemeral := extenderCamouflageClientEphemeral(uconn.HandshakeState.State13.KeyShareKeys)
	if clientEphemeral == nil {
		return nil, errors.New("extender reality: the browser hello offers no x25519 tls 1.3 key share")
	}
	sharedSecret, err := clientEphemeral.ECDH(serverStaticPublicKey)
	if err != nil {
		return nil, err
	}
	authKey, err := extenderRealityAuthKey(sharedSecret, hello.Random)
	if err != nil {
		return nil, err
	}
	// the additional data is the hello with a zero session id
	clear(hello.Raw[vlessRealitySessionIdOffset : vlessRealitySessionIdOffset+32])
	shortId := ExtenderKeyId(extenderConfig.PublicKey)
	sessionId, err := vlessRealitySealSessionId(
		authKey,
		hello.Raw,
		hello.Random,
		extenderRealitySessionIdPlaintext(shortId, time.Now().Add(extenderCamouflageClientClockOffset(extenderConfig))),
	)
	if err != nil {
		return nil, err
	}
	hello.SessionId = sessionId

	if err := uconn.HandshakeContext(ctx); err != nil {
		return nil, err
	}
	// the authenticated path negotiates http/1.1 alone (P4); any other outcome
	// is an unauthenticated terminate the attempt must fail on so the race falls
	// to legacy, never a refusal that would end the race
	if negotiated := uconn.ConnectionState().NegotiatedProtocol; negotiated != "http/1.1" {
		return nil, fmt.Errorf("extender reality: the hello was not authenticated (alpn %q)", negotiated)
	}
	success = true
	return uconn, nil
}

// The client ephemeral X25519 private key for the tag's ECDH (P1): the
// standalone X25519 key share when the hello carries one, else the X25519 half
// of the X25519MLKEM768 hybrid. A Chrome hello carries both, so the client uses
// the standalone, which is the share the extender reads on its side.
func extenderCamouflageClientEphemeral(keyShareKeys *utls.KeySharePrivateKeys) *ecdh.PrivateKey {
	if keyShareKeys == nil {
		return nil
	}
	for _, key := range []*ecdh.PrivateKey{keyShareKeys.Ecdhe, keyShareKeys.MlkemEcdhe} {
		if key != nil && key.Curve() == ecdh.X25519() {
			return key
		}
	}
	return nil
}

// The clock correction the client seals its time with (P8). connect keeps no
// verified server-time offset yet, so this is zero today; 14a adds the offset
// (the Date header of an authenticated api response or a verified DoH answer)
// here, which keeps a skewed client's camouflaged attempt from falling to
// legacy. Reads only the config so the seam is per-dial.
func extenderCamouflageClientClockOffset(extenderConfig *ExtenderConfig) time.Duration {
	return 0
}

// One attempt of the tcp camouflage race and what it came back with.
type extenderTcpCamouflageAttempt struct {
	camo      bool
	conn      net.Conn
	response  *protocol.ExtenderResponse
	roundTrip *ExtenderRoundTrip
	err       error
}

// Races the camouflaged and legacy tcp attempts (P8), exactly
// raceExtenderDnsPorts' shape for two attempts: the camouflaged attempt at
// once, the legacy attempt one stagger later while the first is pending or at
// once when the first has failed. The first to answer wins and its round trip
// is copied into roundTrip; a refusal or a limit (A4, A12) ends the race with
// that answer, since the extender gives the same one to either attempt. Every
// other attempt is canceled and joined before the race returns, and a loser
// that connected anyway is closed. With both attempts failed the errors are
// joined. stagger and the dials are the clock and the dials a test replaces.
func raceExtenderTcpCamouflage(
	ctx context.Context,
	roundTrip *ExtenderRoundTrip,
	stagger func() <-chan time.Time,
	camoDial func(ctx context.Context, roundTrip *ExtenderRoundTrip) (net.Conn, *protocol.ExtenderResponse, error),
	legacyDial func(ctx context.Context, roundTrip *ExtenderRoundTrip) (net.Conn, *protocol.ExtenderResponse, error),
) (net.Conn, *protocol.ExtenderResponse, error) {
	raceCtx, raceCancel := context.WithCancel(ctx)
	var attemptWorkers sync.WaitGroup
	defer func() {
		raceCancel()
		attemptWorkers.Wait()
	}()

	// A result transfers ownership only when the race receives it. An attempt
	// that finishes after the race ends keeps and closes its connection itself.
	attempts := make(chan *extenderTcpCamouflageAttempt)
	launch := func(camo bool, dial func(ctx context.Context, roundTrip *ExtenderRoundTrip) (net.Conn, *protocol.ExtenderResponse, error)) {
		attempt := &extenderTcpCamouflageAttempt{camo: camo}
		if roundTrip != nil {
			attempt.roundTrip = &ExtenderRoundTrip{}
		}
		attemptWorkers.Add(1)
		go func() {
			defer attemptWorkers.Done()
			HandleError(func() {
				attempt.conn, attempt.response, attempt.err = dial(raceCtx, attempt.roundTrip)
			}, func(err error) {
				attempt.err = err
			})
			select {
			case attempts <- attempt:
			case <-raceCtx.Done():
				if attempt.conn != nil {
					attempt.conn.Close()
				}
			}
		}()
	}

	launch(true, camoDial)
	legacyLaunched := false
	pendingCount := 1
	staggerC := stagger()
	errs := []error{}
	for {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-staggerC:
			staggerC = nil
			if !legacyLaunched {
				legacyLaunched = true
				pendingCount += 1
				launch(false, legacyDial)
			}
		case attempt := <-attempts:
			pendingCount -= 1
			if attempt.err != nil && attempt.conn != nil {
				// ownership transfers for every non-nil result, including a
				// rejected one
				attempt.conn.Close()
			}
			var limitedErr *ExtenderLimitedError
			var refusedErr *ExtenderRefusedError
			switch {
			case attempt.err == nil:
				if roundTrip != nil {
					*roundTrip = *attempt.roundTrip
				}
				return attempt.conn, attempt.response, nil
			case errors.As(attempt.err, &limitedErr), errors.As(attempt.err, &refusedErr):
				return nil, nil, attempt.err
			default:
				errs = append(errs, attempt.err)
				if !legacyLaunched {
					// the camouflaged attempt failed before the stagger: launch
					// the legacy attempt at once
					legacyLaunched = true
					pendingCount += 1
					staggerC = nil
					launch(false, legacyDial)
				} else if pendingCount == 0 {
					return nil, nil, errors.Join(errs...)
				}
			}
		}
	}
}
