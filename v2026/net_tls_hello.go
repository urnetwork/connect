package connect

// net_tls_hello.go — the tls client hello of the normal and resilient dialers.
//
// Go's crypto/tls hello is distinctive: its cipher suites, groups and
// extensions name the Go runtime to any observer, so a network that filters
// by fingerprint can single the strategy's direct dials out. These dialers
// present the newest stable Chrome hello of the vendored uTLS instead
// (`chromeClientHelloId`), with the grease values, the extension order
// shuffled per connection and the X25519MLKEM768 key share that profile
// defines. What belongs to the dial stays as with Go's hello: the server name,
// certificate verification (roots, the skip, the peer and connection
// verifiers) and the error types of a refused certificate, the application
// protocols, the timeouts, and the resilient fragment/reorder wrapping, which
// takes the larger hello (about 1.7 KiB, 2 KiB with a ticket) as one record
// like any other.
//
// The alpn list is the dial path's own, never Chrome's: the websocket dial
// offers http/1.1 alone, as Chrome itself does for a websocket. Application
// settings (alps) go only with an offered h2, the one protocol Chrome
// configures them for.
//
// Resumption follows Chrome. A first contact carries no pre_shared_key; a
// later dial to the same server offers the server's ticket as the final
// extension. Each dial path owns a bounded session cache, as with Go's hello
// (`newClientTlsConfig`), so a ticket never links two paths. uTLS cannot
// answer a hello retry after offering a ticket (it fails the dial, and drops
// the ticket), so a server whose first contact needed a retry gets no ticket
// kept: it is always dialed as a first contact, which retries fine
// (`chromeSessionCache`).
//
// net/http (Go 1.27) reads the negotiated protocol of a DialTLSContext
// connection from its crypto/tls ConnectionState method, which
// `chromeTlsConn` answers, so the api dial negotiates h2 as before. Older
// net/http (verified for Go 1.25) reads it only from a *tls.Conn and would
// speak http/1.1 into an h2 connection, so a build with a toolchain before
// Go 1.27 keeps Go's hello on a path that offers h2
// (`httpTransportReadsTlsConnectionState`).
//
// A configuration the Chrome hello cannot carry exactly keeps Go's hello too:
// client certificates, an ech configuration, or a restriction of the cipher
// suites, groups or versions, each of which the Chrome hello would drop or
// widen.

import (
	"context"
	"crypto/tls"
	"net"
	"slices"
	"sync"

	utls "github.com/refraction-networking/utls"
)

// The values of `ConnectSettings.TlsClientHelloFingerprint`.
const (
	TlsClientHelloFingerprintChrome = "chrome"
	TlsClientHelloFingerprintGo     = "go"
)

// The newest stable Chrome profile of the vendored uTLS (v1.8.2). Named
// rather than `utls.HelloChrome_Auto`, so a uTLS upgrade changes the hello
// only with a change here (a test fails when Auto moves past it).
var chromeClientHelloId = utls.HelloChrome_133

// Whether a dial path presents the Chrome hello: the fingerprint selects it, the
// configuration is one it carries exactly, and a path that offers h2 has a
// net/http that reads the negotiated protocol off the uTLS connection.
func chromeClientHelloApplies(fingerprint string, config *tls.Config, readsTlsConnectionState bool) bool {
	switch fingerprint {
	case "", TlsClientHelloFingerprintChrome:
	default:
		return false
	}
	if len(config.Certificates) != 0 || config.GetClientCertificate != nil {
		return false
	}
	if len(config.EncryptedClientHelloConfigList) != 0 {
		return false
	}
	// the hello offers its own suites and groups, and tls 1.2 and 1.3
	if len(config.CipherSuites) != 0 || len(config.CurvePreferences) != 0 {
		return false
	}
	if tls.VersionTLS12 < config.MinVersion || (config.MaxVersion != 0 && config.MaxVersion < tls.VersionTLS13) {
		return false
	}
	if slices.Contains(config.NextProtos, "h2") && !readsTlsConnectionState {
		return false
	}
	return true
}

// The Chrome hello of one dial, with the dial path's application protocols.
// The profile shuffles its extensions on every call, as Chrome does per
// connection.
func chromeClientHelloSpec(nextProtos []string) (*utls.ClientHelloSpec, error) {
	spec, err := utls.UTLSIdToSpec(chromeClientHelloId)
	if err != nil {
		return nil, err
	}
	h2 := slices.Contains(nextProtos, "h2")
	extensions := make([]utls.TLSExtension, 0, len(spec.Extensions)+1)
	for _, extension := range spec.Extensions {
		switch extension := extension.(type) {
		case *utls.ALPNExtension:
			if len(nextProtos) == 0 {
				continue
			}
			extension.AlpnProtocols = slices.Clone(nextProtos)
		case *utls.ApplicationSettingsExtensionNew:
			if !h2 {
				continue
			}
		}
		extensions = append(extensions, extension)
	}
	// last, after the closing grease, as Chrome sends it; omitted while the
	// session cache holds no ticket for the server (`utls.Config.OmitEmptyPsk`)
	extensions = append(extensions, &utls.UtlsPreSharedKeyExtension{})
	spec.Extensions = extensions
	return &spec, nil
}

// The uTLS form of a dial's crypto/tls configuration: its server name, its
// verification, and its clock, randomness, ticket, record sizing and key log
// settings. The Chrome hello supplies the rest (`chromeClientHelloApplies`
// admits only a configuration that leaves the rest to the hello).
func newChromeTlsConfig(config *tls.Config, sessionCache utls.ClientSessionCache) *utls.Config {
	chromeConfig := &utls.Config{
		Rand:                        config.Rand,
		Time:                        config.Time,
		RootCAs:                     config.RootCAs,
		ServerName:                  config.ServerName,
		InsecureSkipVerify:          config.InsecureSkipVerify,
		VerifyPeerCertificate:       config.VerifyPeerCertificate,
		SessionTicketsDisabled:      config.SessionTicketsDisabled,
		ClientSessionCache:          sessionCache,
		OmitEmptyPsk:                true,
		DynamicRecordSizingDisabled: config.DynamicRecordSizingDisabled,
		KeyLogWriter:                config.KeyLogWriter,
	}
	if verifyConnection := config.VerifyConnection; verifyConnection != nil {
		chromeConfig.VerifyConnection = func(state utls.ConnectionState) error {
			return verifyConnection(tlsConnectionState(state))
		}
	}
	return chromeConfig
}

// The crypto/tls form of a uTLS connection state. It has no keying material
// exporter (crypto/tls keeps that private), so ExportKeyingMaterial must not
// be called on it; the uTLS connection has its own.
func tlsConnectionState(state utls.ConnectionState) tls.ConnectionState {
	return tls.ConnectionState{
		Version:                     state.Version,
		HandshakeComplete:           state.HandshakeComplete,
		DidResume:                   state.DidResume,
		CipherSuite:                 state.CipherSuite,
		NegotiatedProtocol:          state.NegotiatedProtocol,
		NegotiatedProtocolIsMutual:  state.NegotiatedProtocolIsMutual,
		ServerName:                  state.ServerName,
		PeerCertificates:            state.PeerCertificates,
		VerifiedChains:              state.VerifiedChains,
		SignedCertificateTimestamps: state.SignedCertificateTimestamps,
		OCSPResponse:                state.OCSPResponse,
		TLSUnique:                   state.TLSUnique,
		ECHAccepted:                 state.ECHAccepted,
	}
}

// An established Chrome-hello connection. ConnectionState answers in
// crypto/tls's type, which is where net/http reads the negotiated protocol of
// a DialTLSContext connection, and what any caller inspecting a dialed
// connection already reads.
type chromeTlsConn struct {
	*utls.UConn
}

// The handshake's state in crypto/tls's type (`tlsConnectionState`).
func (self *chromeTlsConn) ConnectionState() tls.ConnectionState {
	return tlsConnectionState(self.UConn.ConnectionState())
}

// The Chrome hello's session cache of one dial path: a bounded cache of the
// servers' tickets that keeps none for a server that answered a first contact
// with a hello retry. uTLS fails a dial whose hello offered a ticket when the
// server asks for a retry, so offering one to such a server would fail every
// dial after its first. A server whose first contacts needed a retry stays
// marked while the path lives (a bounded set), and loses resumption, not
// dials.
//
// Safe for concurrent use.
type chromeSessionCache struct {
	sessionCache utls.ClientSessionCache

	stateLock sync.Mutex
	// the session keys (server names) of servers that asked for a hello retry
	helloRetrySessionKeys map[string]bool
}

// An empty cache, bounded as Go's hello's (`clientTlsSessionCacheCapacity`).
func newChromeSessionCache() *chromeSessionCache {
	return &chromeSessionCache{
		sessionCache:          utls.NewLRUClientSessionCache(clientTlsSessionCacheCapacity),
		helloRetrySessionKeys: map[string]bool{},
	}
}

// Whether the server of sessionKey is marked as one that asks for a hello
// retry.
func (self *chromeSessionCache) helloRetried(sessionKey string) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.helloRetrySessionKeys[sessionKey]
}

// Marks the server of sessionKey as one that asks for a hello retry and drops
// any ticket it holds.
func (self *chromeSessionCache) markHelloRetry(sessionKey string) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if clientTlsSessionCacheCapacity <= len(self.helloRetrySessionKeys) {
			clear(self.helloRetrySessionKeys)
		}
		self.helloRetrySessionKeys[sessionKey] = true
	}()
	self.sessionCache.Put(sessionKey, nil)
}

// Implements utls.ClientSessionCache: no ticket for a marked server.
func (self *chromeSessionCache) Get(sessionKey string) (*utls.ClientSessionState, bool) {
	if self.helloRetried(sessionKey) {
		return nil, false
	}
	return self.sessionCache.Get(sessionKey)
}

// Implements utls.ClientSessionCache: a marked server's ticket is not kept. A
// nil session, uTLS dropping a ticket, always goes through.
func (self *chromeSessionCache) Put(sessionKey string, session *utls.ClientSessionState) {
	if session != nil && self.helloRetried(sessionKey) {
		return
	}
	self.sessionCache.Put(sessionKey, session)
}

// The tls handshake of one dial path: the normal or one resilient dialer, for
// one set of application protocols. Built once per path from the path's
// configuration (`newClientTlsConfig`); safe for concurrent use.
type clientTlsHandshaker struct {
	chrome bool
	// the Chrome hello's tickets; nil for Go's hello, whose tickets live in the
	// path's crypto/tls configuration
	chromeSessionCache *chromeSessionCache
}

// The handshaker of a dial path with config, presenting the hello the
// fingerprint names where the Chrome hello applies (`chromeClientHelloApplies`)
// and Go's hello everywhere else.
func newClientTlsHandshaker(fingerprint string, config *tls.Config) *clientTlsHandshaker {
	handshaker := &clientTlsHandshaker{
		chrome: chromeClientHelloApplies(fingerprint, config, httpTransportReadsTlsConnectionState),
	}
	if handshaker.chrome {
		handshaker.chromeSessionCache = newChromeSessionCache()
	}
	return handshaker
}

// Runs the handshake over conn. config is the dial's own clone of the path's
// configuration, its server name set. On error the tls connection is closed,
// and with it conn.
func (self *clientTlsHandshaker) handshake(ctx context.Context, conn net.Conn, config *tls.Config) (net.Conn, error) {
	if !self.chrome {
		tlsConn := tls.Client(conn, config)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			tlsConn.Close()
			return nil, err
		}
		return tlsConn, nil
	}
	spec, err := chromeClientHelloSpec(config.NextProtos)
	if err != nil {
		conn.Close()
		return nil, err
	}
	uconn := utls.UClient(conn, newChromeTlsConfig(config, self.chromeSessionCache), utls.HelloCustom)
	if err := uconn.ApplyPreset(spec); err != nil {
		uconn.Close()
		return nil, err
	}
	// the groups the hello shares keys for, read before a retry replaces them
	var keyShareGroups []utls.CurveID
	for _, extension := range spec.Extensions {
		if keyShareExtension, ok := extension.(*utls.KeyShareExtension); ok {
			for _, keyShare := range keyShareExtension.KeyShares {
				keyShareGroups = append(keyShareGroups, keyShare.Group)
			}
		}
	}
	if err := uconn.HandshakeContext(ctx); err != nil {
		uconn.Close()
		// crypto/tls's type where it has one, so a refused certificate or a
		// peer that does not speak tls reads the same whichever hello was sent
		switch handshakeErr := err.(type) {
		case *utls.CertificateVerificationError:
			return nil, &tls.CertificateVerificationError{
				UnverifiedCertificates: handshakeErr.UnverifiedCertificates,
				Err:                    handshakeErr.Err,
			}
		case utls.RecordHeaderError:
			return nil, tls.RecordHeaderError{
				Msg:          handshakeErr.Msg,
				RecordHeader: handshakeErr.RecordHeader,
				Conn:         handshakeErr.Conn,
			}
		}
		return nil, err
	}
	// a server that settled on a group the hello shared no key for asked for a
	// retry; the session key is the server name (uTLS keys tickets by it)
	if serverHello := uconn.HandshakeState.ServerHello; serverHello != nil && serverHello.ServerShare.Group != 0 &&
		!slices.Contains(keyShareGroups, serverHello.ServerShare.Group) {
		self.chromeSessionCache.markHelloRetry(config.ServerName)
	}
	// only the handshake reads the hello's extensions and its key share
	// private keys; release them, as crypto/tls does, rather than keep about
	// 10 KiB and ephemeral keys for the life of the connection
	uconn.Extensions = nil
	uconn.HandshakeState.State13.KeyShareKeys = nil
	return &chromeTlsConn{UConn: uconn}, nil
}
