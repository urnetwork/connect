package connect

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect/protocol"
)

// Sequence-level encryption between two peers.
//
// One TLS session per peer-pair (more precisely: per `TransferPath` destination
// mask — `{DestinationId, StreamId}`). The session is owned by
// `EncryptionSessionManager` on the local `Client` and shared by every local
// `SendSequence` and `ReceiveSequence` that talks to that peer. The first
// sequence created (sender or receiver) lazily creates the session; the
// session is GC'd when no sequences reference it.
//
// Roles are deterministic: the side with the lexicographically lower
// `ClientId` is the TLS client; the other side is the TLS server. The TLS
// client drives the TLS handshake (sends ClientHello). The server side
// just waits — it has nothing to send until it sees ClientHello. There's
// no RequestHandshake kicker: a TLS-client-role session that hasn't been
// created yet on the client side will be created the moment the client
// either opens a sequence to this peer or receives any frame from this
// peer (the unwrap path on the receive side delivers EncryptedControl
// frames through the session manager's `getOrCreate`, which spins up the
// session if needed and starts the handshake).
//
// `EncryptedControl` messages (TLS handshake bytes only) ride the normal
// `Pack`/`Ack` flow as a regular `Frame` with `MessageType =
// TransferEncryptedControl`. Pack delivery is reliable and in-order so no
// chunk indexing is needed. The destination `ReceiveSequence` intercepts
// these frames and routes them into the session instead of emitting them
// to the application receive callback.
//
// There is no opt-out wire control. A sender that doesn't want to use
// encryption simply never creates the session (e.g., `Encrypt = false`
// in `EncryptionSettings`); the receiver tolerates plaintext via the
// cipher-nil pass-through in `writeMaybeWrappedBytes` and the
// ack-mirroring in `writeAck`.
//
// After the handshake completes both sides export the same key via the TLS
// session exporter and wrap it in an AEAD. Application TransferFrames are
// outer-wrapped at the writer just before going on the wire and unwrapped
// by `Client.run` on receipt, using the session's cipher.

const (
	sequenceTlsKeyLabel      = "urnetwork-connect-aead"
	sequenceTlsKeyLength     = 32
	sequenceTlsAeadNonceSize = 12
	// sequenceTlsIdentityProofLabel is the exporter label that both
	// peers use to derive the 32-byte payload that gets signed with
	// the long-lived client identity key. A distinct label from
	// `sequenceTlsKeyLabel` so the two derivations are
	// cryptographically separated (per RFC 5705 exporter rules).
	sequenceTlsIdentityProofLabel  = "urnetwork-connect-identity-proof"
	sequenceTlsIdentityProofLength = 32
)

type sequenceTlsRole int

const (
	sequenceTlsRoleClient sequenceTlsRole = iota
	sequenceTlsRoleServer
)

func (r sequenceTlsRole) String() string {
	if r == sequenceTlsRoleClient {
		return "client"
	}
	return "server"
}

// sequenceTlsTransport is the `net.Conn` that the per-peer `tls.Conn` reads
// from / writes to. Bytes the TLS state machine wants to send accumulate in
// the outbox; bytes received from the peer (via `Deliver`) accumulate in the
// inbox where the TLS state machine reads them.
type sequenceTlsTransport struct {
	ctx    context.Context
	cancel context.CancelFunc

	inboxLock   sync.Mutex
	inboxBuf    []byte
	inboxNotify *Monitor

	outboxLock   sync.Mutex
	outboxBuf    []byte
	outboxNotify *Monitor

	closeOnce sync.Once
	closed    chan struct{}
}

func newSequenceTlsTransport(ctx context.Context) *sequenceTlsTransport {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &sequenceTlsTransport{
		ctx:          cancelCtx,
		cancel:       cancel,
		inboxNotify:  NewMonitor(),
		outboxNotify: NewMonitor(),
		closed:       make(chan struct{}),
	}
}

func (t *sequenceTlsTransport) Deliver(b []byte) {
	if len(b) == 0 {
		return
	}
	t.inboxLock.Lock()
	t.inboxBuf = append(t.inboxBuf, b...)
	t.inboxLock.Unlock()
	t.inboxNotify.NotifyAll()
}

func (t *sequenceTlsTransport) Read(p []byte) (int, error) {
	for {
		notify := t.inboxNotify.NotifyChannel()
		t.inboxLock.Lock()
		if 0 < len(t.inboxBuf) {
			n := copy(p, t.inboxBuf)
			t.inboxBuf = t.inboxBuf[n:]
			t.inboxLock.Unlock()
			return n, nil
		}
		t.inboxLock.Unlock()
		select {
		case <-t.ctx.Done():
			return 0, io.EOF
		case <-t.closed:
			return 0, io.EOF
		case <-notify:
		}
	}
}

func (t *sequenceTlsTransport) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	t.outboxLock.Lock()
	t.outboxBuf = append(t.outboxBuf, p...)
	t.outboxLock.Unlock()
	t.outboxNotify.NotifyAll()
	return len(p), nil
}

// TakeOutbox blocks until the TLS state machine has produced bytes (or the
// transport is closed / context is done), then returns all buffered bytes.
func (t *sequenceTlsTransport) TakeOutbox(ctx context.Context) ([]byte, error) {
	for {
		notify := t.outboxNotify.NotifyChannel()
		t.outboxLock.Lock()
		if 0 < len(t.outboxBuf) {
			b := t.outboxBuf
			t.outboxBuf = nil
			t.outboxLock.Unlock()
			return b, nil
		}
		t.outboxLock.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.ctx.Done():
			return nil, io.EOF
		case <-t.closed:
			return nil, io.EOF
		case <-notify:
		}
	}
}

func (t *sequenceTlsTransport) Close() error {
	t.closeOnce.Do(func() {
		t.cancel()
		close(t.closed)
		t.inboxNotify.NotifyAll()
		t.outboxNotify.NotifyAll()
	})
	return nil
}

type sequenceTlsAddr struct{}

func (sequenceTlsAddr) Network() string { return "sequence-tls" }
func (sequenceTlsAddr) String() string  { return "sequence-tls" }

func (t *sequenceTlsTransport) LocalAddr() net.Addr              { return sequenceTlsAddr{} }
func (t *sequenceTlsTransport) RemoteAddr() net.Addr             { return sequenceTlsAddr{} }
func (t *sequenceTlsTransport) SetDeadline(time.Time) error      { return nil }
func (t *sequenceTlsTransport) SetReadDeadline(time.Time) error  { return nil }
func (t *sequenceTlsTransport) SetWriteDeadline(time.Time) error { return nil }

// sequenceCipher is the AEAD used to outer-wrap a whole TransferFrame after
// the per-peer TLS handshake completes. Nonce is random per-message and
// prepended to the ciphertext.
type sequenceCipher struct {
	aead cipher.AEAD
}

func newSequenceCipher(tlsConn *tls.Conn) (*sequenceCipher, error) {
	state := tlsConn.ConnectionState()
	if !state.HandshakeComplete {
		return nil, errors.New("sequence cipher requires a completed TLS handshake")
	}
	key, err := state.ExportKeyingMaterial(sequenceTlsKeyLabel, nil, sequenceTlsKeyLength)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &sequenceCipher{aead: aead}, nil
}

func (c *sequenceCipher) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, sequenceTlsAeadNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, sequenceTlsAeadNonceSize, sequenceTlsAeadNonceSize+len(plaintext)+c.aead.Overhead())
	copy(out, nonce)
	return c.aead.Seal(out, nonce, plaintext, nil), nil
}

func (c *sequenceCipher) Open(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < sequenceTlsAeadNonceSize {
		return nil, errors.New("sequence cipher: ciphertext too short")
	}
	nonce := ciphertext[:sequenceTlsAeadNonceSize]
	sealed := ciphertext[sequenceTlsAeadNonceSize:]
	return c.aead.Open(nil, nonce, sealed, nil)
}

// buildEncryptedOuterFrameBytes wraps an AEAD ciphertext as the
// `encryptedTransferFrame` field of an outer `TransferFrame` with the given
// path. Forwarders read the outer `TransferPath` without touching the
// ciphertext; only the destination decrypts.
func buildEncryptedOuterFrameBytes(path TransferPath, ciphertext []byte) ([]byte, error) {
	transferFrame := &protocol.TransferFrame{
		TransferPath:           path.ToProtobuf(),
		EncryptedTransferFrame: ciphertext,
	}
	return ProtoMarshal(transferFrame)
}

// DefaultSequencePostQuantumCurves are the key-exchange groups that the
// per-peer TLS session prefers. Led by the X25519MLKEM768 hybrid
// post-quantum group (RFC 9794 / draft-ietf-tls-mlkem) so the session is
// protected against future ("harvest now, decrypt later") quantum attacks
// where supported, falling back to conventional X25519 if the peer doesn't
// advertise the hybrid group.
var DefaultSequencePostQuantumCurves = []tls.CurveID{
	tls.X25519MLKEM768,
	tls.X25519,
}

// DefaultSequenceServerTlsConfig returns a TLS config suitable for the
// server-role side of a per-peer session. It generates a fresh ephemeral
// self-signed certificate; sequences are short-lived and don't need a
// stable identity at this layer (auth is at the contract layer).
//
// Mutual TLS is required: `ClientAuth = RequireAnyClientCert` so the
// peer's certificate is captured into `ConnectionState.PeerCertificates`
// regardless of role. The cert itself is not validated by Go's TLS stack
// (sequences use self-signed ephemeral certs); identity is verified at
// the sequence layer against the contract's `ProvideTlsCertificate`
// commitment. Without mTLS, whichever side lands in the TLS-server role
// would have no `PeerCertificates` to verify, and contract verification
// would fail for half of all peer pairs (the half where the local
// ClientId is lexicographically greater).
func DefaultSequenceServerTlsConfig() (*tls.Config, error) {
	cert, err := generateSequenceTlsCertificate()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates:     []tls.Certificate{cert},
		ClientAuth:       tls.RequireAnyClientCert,
		MinVersion:       tls.VersionTLS13,
		NextProtos:       []string{"urnetwork/sequence"},
		CurvePreferences: append([]tls.CurveID(nil), DefaultSequencePostQuantumCurves...),
	}, nil
}

// DefaultSequenceClientTlsConfig returns a TLS config suitable for the
// client-role side of a per-peer session. Server identity is not verified
// at this layer (verification happens against the contract commitment).
// The TLS-client also presents a certificate (set by the session manager
// from the same self cert used by the TLS-server role) so contract-based
// verification works regardless of role.
func DefaultSequenceClientTlsConfig() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
		ServerName:         "urnetwork-sequence",
		NextProtos:         []string{"urnetwork/sequence"},
		CurvePreferences:   append([]tls.CurveID(nil), DefaultSequencePostQuantumCurves...),
	}
}

func generateSequenceTlsCertificate() (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		return tls.Certificate{}, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "urnetwork-sequence"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"urnetwork-sequence"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  priv,
		Leaf:        leaf,
	}, nil
}

// EncryptionSettings configures the per-peer TLS sessions managed by an
// `EncryptionSessionManager`.
type EncryptionSettings struct {
	// Enable per-peer TLS encryption between sequences. When false, the
	// session manager is inert and all reads/writes are unwrapped.
	Encrypt bool
	// TLS config used when local is in the TLS-client role for a peer.
	// When nil, a permissive default with InsecureSkipVerify is used.
	ClientTlsConfig *tls.Config
	// TLS config used when local is in the TLS-server role for a peer.
	// When nil, an ephemeral self-signed certificate is generated.
	ServerTlsConfig *tls.Config
	// How long to wait for the TLS handshake before giving up. On timeout,
	// the session stays in the cipher-nil state — Cipher() returns nil and
	// traffic flows in plaintext. Sessions never block sequences; whether
	// to accept plaintext traffic is the caller's choice via `Encrypt`.
	HandshakeTimeout time.Duration
	// NewPeerClientPublicKeyFetcher, when non-nil, is invoked once per
	// per-peer encryption session at session creation time with the
	// peer's ClientId. The returned function is the per-session
	// fetcher: the session invokes it at most once, asynchronously,
	// the first time the peer's long-lived public client identity key
	// is learned from a contract (`SetPeerClientPublicKey`). The
	// session compares the returned key against the contract-supplied
	// key and logs a loud warning on mismatch; today no further
	// action is taken (the contract-supplied key is still used). This
	// gives operators an early-warning signal for a platform that's
	// substituting keys in contracts (a possible MITM attack); a
	// future iteration will harden this into a refusal to trust the
	// substituted key.
	//
	// Per-session shape (factory) is deliberate. The fetcher reference
	// is held only by the session; when the session is released, the
	// fetcher and any state it captures are eligible for GC. A
	// shared-across-all-sessions fetcher would invite a manager-wide
	// per-peer cache that grows unboundedly with the number of peers
	// the local client ever talks to. The factory contract makes the
	// session-bounded lifetime explicit in the API.
	//
	// A typical implementation just closes over a `*BringYourApi` and
	// the peerId:
	//
	//   func(peerId Id) func(context.Context) ([]byte, error) {
	//       return func(ctx context.Context) ([]byte, error) {
	//           r, err := api.GetClientKeySync(&GetClientKeyArgs{ClientId: peerId})
	//           if err != nil { return nil, err }
	//           return r.PublicKey, nil
	//       }
	//   }
	//
	// Leaving nil disables the cross-check entirely.
	NewPeerClientPublicKeyFetcher func(peerId Id) func(ctx context.Context) ([]byte, error)

	// ProvideTlsCertificatePem, when set together with
	// `ProvideTlsPrivateKeyPem`, loads the local sequence-level TLS
	// server-role cert + private key instead of generating a fresh
	// pair on construction. Both must be PEM-encoded; the cert value
	// is one or more concatenated `-----BEGIN CERTIFICATE-----` blocks
	// (leaf first), the key value is a single `-----BEGIN ... PRIVATE
	// KEY-----` block. Persisting and reloading these across restarts
	// keeps the client's `EncryptedKey` cert commitment stable —
	// other peers that hold contracts against the old commitment
	// still recognize the cert when it shows up in a TLS handshake.
	//
	// When either field is empty the manager generates a fresh
	// ephemeral self-signed cert (same behavior as before this field
	// existed). Setting `ServerTlsConfig` independently overrides
	// both fields.
	ProvideTlsCertificatePem []byte
	ProvideTlsPrivateKeyPem  []byte
}

func DefaultEncryptionSettings() *EncryptionSettings {
	return &EncryptionSettings{
		Encrypt:          false,
		HandshakeTimeout: 30 * time.Second,
	}
}

// peerEncryptionSession owns the TLS state machine and derived AEAD for a
// single peer/stream pair. It is shared by every local SendSequence and
// ReceiveSequence that talks to the same peer/stream. The session lives
// inside `EncryptionSessionManager`.
type peerEncryptionSession struct {
	ctx    context.Context
	cancel context.CancelFunc

	manager *EncryptionSessionManager
	client  *Client
	peerId  Id // the other client's ClientId; the session is shared across every transport/route/stream to this peer
	role    sequenceTlsRole
	logTag  string

	settings *EncryptionSettings

	transport          *sequenceTlsTransport
	tlsConn            *tls.Conn
	handshakeStartOnce sync.Once
	handshakeDone      chan struct{}
	handshakeErr       error

	// peerClientPublicKeyFetcher is the session's own out-of-band
	// fetcher for the peer's public client identity key, produced by
	// `EncryptionSettings.NewPeerClientPublicKeyFetcher(peerId)` at
	// session creation. Nil when the manager has no factory
	// configured. Held by the session and nowhere else; releasing the
	// session releases this reference (and any state the application's
	// factory captured for this peer).
	peerClientPublicKeyFetcher func(ctx context.Context) ([]byte, error)

	// state (locked)
	stateLock sync.Mutex
	// derivedTlsCipher holds the AEAD derived from the completed TLS
	// handshake. It is NOT exposed via `Cipher()` until the
	// post-handshake identity exchange validates the peer (see
	// `peerIdentityVerified`). Held separately so identity-proof code
	// can sign the exporter without exposing the cipher early.
	derivedTlsCipher *sequenceCipher
	certVerified     bool
	refs             int // protected by stateLock
	// tlsExporter holds the 32-byte output of
	// `ExportKeyingMaterial("urnetwork-sequence-identity-proof", ...)`
	// taken from the completed TLS connection. Used as the input that
	// both peers sign with their client identity keys, binding the
	// session's negotiated key material to the long-lived identities
	// (defeats active MITM that splits the TLS session). Set once on
	// successful handshake completion; nil before that.
	tlsExporter []byte
	// peerClientPublicKey is the peer's long-lived Ed25519 public
	// identity key, set via `SetPeerClientPublicKey` (called from the
	// SendSequence after a contract for the peer arrives). May be nil
	// if no contract has been seen yet; in that case any peer identity
	// proof that arrives is buffered in `pendingPeerIdentityProof`
	// until the key is known.
	peerClientPublicKey ed25519.PublicKey
	// identityProofSent flips true after we've sent our identity proof
	// over the wire. Used so we don't double-send if the handshake or
	// the peer-key-arrival path tries to trigger it again.
	identityProofSent bool
	// pendingPeerIdentityProof buffers the peer's `IdentityProof`
	// payload when it arrives before we can verify it (either we
	// haven't completed the TLS handshake yet, so we have no
	// `tlsExporter`, or we don't yet have the peer's public key).
	// `maybeVerifyPendingPeerIdentityProof` drains this when both
	// pieces become available.
	pendingPeerIdentityProof []byte
	// peerIdentityVerified flips true after we've successfully
	// verified a peer identity proof against `peerClientPublicKey`
	// over `tlsExporter`. `Cipher()` returns nil until this is true:
	// without identity verification the session is treated as
	// unauthenticated and traffic flows in plaintext.
	peerIdentityVerified bool
	// identityFailed flips true on a definitive identity-proof
	// verification failure (wrong signature against a known peer
	// key). Sticky: the session never exposes a cipher after this.
	identityFailed bool
	// serverFlightSent flips true the first time `outboxLoop` drains a
	// batch in TLS-server role. For TLS 1.3 servers, that first batch is
	// the full server flight (ServerHello + EncryptedExtensions + Cert +
	// CertVerify + Finished). Once set, the session is in the brief
	// "sent server Finished, awaiting client Finished" window where
	// `Cipher()` will flip non-nil as soon as the client Finished is
	// processed. The unwrap path uses this as the *only* state in which
	// it's willing to block the receive loop on `ReadyNotify`; all
	// other server-side states (cipher already set, handshake failed,
	// no ClientHello processed yet) drop the wrapped frame immediately
	// to keep the receive-loop blocking surface — and therefore the
	// DoS surface — minimal.
	serverFlightSent bool
	// trustedPeerCertPems is the union of all peer cert PEM blocks the
	// session has learned about — one entry per PEM block (leaf + any
	// intermediates), keyed by the PEM bytes themselves so duplicates
	// from successive contracts collapse. Sources:
	//   - contracts the local sender holds for this peer
	//     (`Contract.ProvideTlsCertificate`).
	// A peer's TLS-handshake leaf cert is accepted as authentic if it
	// matches any entry in this set. Cert rotation is tolerated: as the
	// peer rotates its `EncryptedKey` publication, new contracts carry
	// the new cert, the set grows, and both old and new certs remain
	// trusted (until the session is torn down).
	trustedPeerCertPems map[string]bool

	readyMonitor *Monitor
}

func newPeerEncryptionSession(
	parentCtx context.Context,
	manager *EncryptionSessionManager,
	client *Client,
	peerId Id,
	role sequenceTlsRole,
	settings *EncryptionSettings,
) *peerEncryptionSession {
	ctx, cancel := context.WithCancel(parentCtx)
	s := &peerEncryptionSession{
		ctx:           ctx,
		cancel:        cancel,
		manager:       manager,
		client:        client,
		peerId:        peerId,
		role:          role,
		logTag:        fmt.Sprintf("%s %s %s", client.ClientTag(), role, peerId),
		settings:      settings,
		handshakeDone: make(chan struct{}),
		readyMonitor:  NewMonitor(),
	}
	// Mint a per-session out-of-band peer key fetcher from the
	// settings factory. Held by the session for its lifetime only —
	// any state the application captures in the returned closure dies
	// with the session, so the peer's key is not retained across
	// session boundaries.
	if settings != nil && settings.NewPeerClientPublicKeyFetcher != nil {
		s.peerClientPublicKeyFetcher = settings.NewPeerClientPublicKeyFetcher(peerId)
	}
	return s
}

// Run is the session's main goroutine, spawned by the manager when the
// session is created. It initializes the TLS state, drives the handshake,
// and waits for the session to be cancelled. When Run returns, the manager
// removes the session from its state — mirroring the SendBuffer /
// SendSequence pattern.
func (self *peerEncryptionSession) Run() {
	self.startInternal()
	<-self.ctx.Done()
	self.closeTls()
}

// startInternal initializes the TLS state on first invocation. Subsequent
// calls (e.g., when an EncryptedControl arrives for a session whose Run
// already initialized the TLS state) are no-ops.
func (self *peerEncryptionSession) startInternal() {
	self.handshakeStartOnce.Do(func() {
		var tlsCfg *tls.Config
		switch self.role {
		case sequenceTlsRoleClient:
			tlsCfg = self.manager.ClientTlsConfig()
			if tlsCfg == nil {
				tlsCfg = resolveSendTlsConfig(self.settings.ClientTlsConfig)
			}
		case sequenceTlsRoleServer:
			tlsCfg = self.manager.ServerTlsConfig()
			if tlsCfg == nil {
				self.completeHandshake(errors.New("server tls config unavailable"))
				return
			}
			if 0 < len(tlsCfg.Certificates) && tlsCfg.Certificates[0].Leaf != nil {
				glog.V(1).Infof(
					"[tls]%s server presenting cert subject=%q\n",
					self.logTag,
					tlsCfg.Certificates[0].Leaf.Subject.String(),
				)
			}
		}
		self.transport = newSequenceTlsTransport(self.ctx)
		switch self.role {
		case sequenceTlsRoleClient:
			self.tlsConn = tls.Client(self.transport, tlsCfg)
		case sequenceTlsRoleServer:
			self.tlsConn = tls.Server(self.transport, tlsCfg)
		}

		// drain TLS outbox → outbound EncryptedControl
		go HandleError(self.outboxLoop, self.cancel)

		// drive the TLS handshake
		go HandleError(self.runHandshake, self.cancel)

		// arm the handshake timeout
		if 0 < self.settings.HandshakeTimeout {
			go HandleError(self.handshakeTimeoutWatcher, self.cancel)
		}
	})
}

func (self *peerEncryptionSession) runHandshake() {
	err := self.tlsConn.HandshakeContext(self.ctx)
	self.completeHandshake(err)
}

func (self *peerEncryptionSession) handshakeTimeoutWatcher() {
	t := time.NewTimer(self.settings.HandshakeTimeout)
	defer t.Stop()
	select {
	case <-self.ctx.Done():
		return
	case <-self.handshakeDone:
		return
	case <-t.C:
		// give the run loop a synthetic timeout error if it hasn't completed
		self.completeHandshake(fmt.Errorf("tls handshake timeout after %s", self.settings.HandshakeTimeout))
	}
}

func (self *peerEncryptionSession) outboxLoop() {
	batchIndex := 0
	for {
		b, err := self.transport.TakeOutbox(self.ctx)
		if err != nil {
			glog.V(1).Infof("[tls]%s outbox loop exit: %s\n", self.logTag, err)
			return
		}
		if len(b) == 0 {
			continue
		}
		var firstByte byte
		if 0 < len(b) {
			firstByte = b[0]
		}
		glog.V(1).Infof(
			"[tls]%s outbox batch %d: %d bytes (record type 0x%02x)\n",
			self.logTag, batchIndex, len(b), firstByte,
		)
		batchIndex += 1
		// In TLS-server role, the first outbox batch is the server
		// flight (which contains the server's Finished). Mark the
		// "awaiting client Finished" state so the unwrap path knows
		// this session is in the narrow window where briefly blocking
		// the receive loop on `ReadyNotify` can pay off. Idempotent.
		if self.role == sequenceTlsRoleServer {
			self.markServerFlightSent()
		}
		self.sendEncryptedControl(&protocol.EncryptedControl{
			ControlType: protocol.EncryptedControlType_EncryptedControlHandshake,
			Payload:     b,
		})
	}
}

func (self *peerEncryptionSession) markServerFlightSent() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.serverFlightSent = true
}

// sendEncryptedControl pushes an EncryptedControl through the normal Pack
// flow to this session's peer.
func (self *peerEncryptionSession) sendEncryptedControl(ec *protocol.EncryptedControl) {
	if self.client == nil || self.client.sendBuffer == nil {
		return
	}
	self.client.sendBuffer.SendEncryptedControl(self.ctx, self.peerId, ec)
}

// completeHandshake sets the AEAD cipher (or records the error) and notifies
// any goroutine waiting on `ReadyNotify`. Runs at most once. On handshake
// failure the session stays in the cipher-nil state — encryption is a
// binary property of `Cipher() != nil`, so leaving the cipher unset is the
// same as never having one: subsequent traffic flows in plaintext.
func (self *peerEncryptionSession) completeHandshake(err error) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		select {
		case <-self.handshakeDone:
			return
		default:
		}
		if err == nil {
			c, cipherErr := newSequenceCipher(self.tlsConn)
			if cipherErr != nil {
				err = cipherErr
			} else {
				// Hold the AEAD on `derivedTlsCipher`, NOT on the
				// publicly observable `Cipher()`. The cipher only
				// becomes visible after the post-handshake identity
				// proof exchange validates the peer (see
				// `peerIdentityVerified`). Until then, the wrap path
				// observes cipher == nil and traffic flows in
				// plaintext, mirroring how a not-yet-completed
				// handshake is observed.
				self.derivedTlsCipher = c
				state := self.tlsConn.ConnectionState()
				exporter, exportErr := state.ExportKeyingMaterial(
					sequenceTlsIdentityProofLabel, nil, sequenceTlsIdentityProofLength,
				)
				if exportErr != nil {
					err = fmt.Errorf("identity-proof exporter: %w", exportErr)
				} else {
					self.tlsExporter = exporter
				}
			}
		}
		self.handshakeErr = err
		close(self.handshakeDone)
	}()
	glogTlsHandshake(self.logTag, err)
	if err == nil {
		glog.V(1).Infof(
			"[tls]%s completeHandshake: cipher derived, exporter %d bytes — starting identity proof exchange\n",
			self.logTag, len(self.tlsExporter),
		)
		if self.role == sequenceTlsRoleClient {
			glogTlsHandshakePeerCert(self.logTag, self.PeerCertificates())
		}
		// Send our identity proof and try to verify any peer proof
		// that arrived early. Both happen on completed-handshake
		// success; on failure neither path is meaningful.
		self.sendIdentityProofOnce()
		self.maybeVerifyPendingPeerIdentityProof()
	} else {
		glog.Errorf("[tls]%s completeHandshake FAILED: %s\n", self.logTag, err)
	}
	// Always notify subscribers — both the success case (cipher
	// derived; identity exchange may still need to complete before
	// it's observable) and the failure case (cipher will stay nil;
	// subscribers waiting on the cipher need to wake up and observe
	// the final state).
	self.readyMonitor.NotifyAll()
}

// sendIdentityProofOnce signs the TLS exporter output with the local
// client key and ships it to the peer in an
// `EncryptedControl{IdentityProof}`. Runs at most once per session
// (`identityProofSent` flag). Safe to call before the manager's
// client key is configured: in that case we log and skip — without a
// client key the session is unauthenticated and the peer will
// (correctly) never grant us authentication, so the cipher will
// never become observable and traffic stays plaintext.
func (self *peerEncryptionSession) sendIdentityProofOnce() {
	exporter, payload, skipReason := func() ([]byte, []byte, string) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.identityProofSent {
			return nil, nil, "already sent"
		}
		if len(self.tlsExporter) == 0 {
			return nil, nil, "no tls exporter (handshake not done?)"
		}
		if self.manager == nil || self.manager.clientKeyManager == nil {
			return nil, nil, "no client key manager configured"
		}
		self.identityProofSent = true
		return self.tlsExporter, self.manager.clientKeyManager.Sign(self.tlsExporter), ""
	}()
	if exporter == nil || payload == nil {
		glog.V(1).Infof("[tls]%s sendIdentityProofOnce skipped: %s\n", self.logTag, skipReason)
		return
	}
	glog.V(1).Infof(
		"[tls]%s sendIdentityProofOnce: signing %d-byte exporter, sending %d-byte proof\n",
		self.logTag, len(exporter), len(payload),
	)
	self.sendEncryptedControl(&protocol.EncryptedControl{
		ControlType: protocol.EncryptedControlType_EncryptedControlIdentityProof,
		Payload:     payload,
	})
}

// maybeVerifyPendingPeerIdentityProof tries to verify a buffered peer
// identity-proof payload. The proof can arrive over the wire before
// any of: our TLS handshake completing (no `tlsExporter` yet), or the
// peer's public client key being set via `SetPeerClientPublicKey`
// (the contract hasn't been delivered yet). Called from any path that
// could fill in the missing piece: `completeHandshake`,
// `DeliverEncryptedControl(IdentityProof, ...)`, and
// `SetPeerClientPublicKey`.
func (self *peerEncryptionSession) maybeVerifyPendingPeerIdentityProof() {
	payload, exporter, peerPub, ready, skipReason := func() ([]byte, []byte, ed25519.PublicKey, bool, string) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.peerIdentityVerified {
			return nil, nil, nil, false, "already verified"
		}
		if self.identityFailed {
			return nil, nil, nil, false, "identity already marked failed"
		}
		if len(self.pendingPeerIdentityProof) == 0 {
			return nil, nil, nil, false, "no peer identity proof received yet"
		}
		if len(self.tlsExporter) == 0 {
			return nil, nil, nil, false, "tls exporter not derived (handshake not done)"
		}
		if len(self.peerClientPublicKey) != ed25519.PublicKeySize {
			return nil, nil, nil, false, "peer client public key not yet known"
		}
		// Snapshot under the lock; verification (ed25519 check) is
		// cheap but we don't want to hold the lock across it.
		return self.pendingPeerIdentityProof, self.tlsExporter, self.peerClientPublicKey, true, ""
	}()
	if !ready {
		glog.V(2).Infof("[tls]%s maybeVerifyPendingPeerIdentityProof skipped: %s\n", self.logTag, skipReason)
		return
	}
	ok := VerifyClientKeySignature(peerPub, exporter, payload)
	flipped := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		// Drop the buffered payload either way: success means it's
		// served its purpose; failure means we don't want to keep
		// re-checking a known-bad proof.
		self.pendingPeerIdentityProof = nil
		if ok {
			self.peerIdentityVerified = true
		} else {
			self.identityFailed = true
		}
		return true
	}()
	if !flipped {
		return
	}
	if ok {
		glog.V(1).Infof("[tls]%s peer identity proof verified — cipher is now usable\n", self.logTag)
	} else {
		glog.Errorf(
			"[tls]%s peer identity proof FAILED (peer key %d bytes, exporter %d bytes, sig %d bytes) — session left unauthenticated\n",
			self.logTag, len(peerPub), len(exporter), len(payload),
		)
	}
	self.readyMonitor.NotifyAll()
}

// SetPeerClientPublicKey records the peer's long-lived Ed25519 public
// identity key for this session. Called from the SendSequence when a
// contract for the peer arrives (the contract carries the peer's
// public key in `destination_client_public_key`). Once set, the
// session can verify a buffered peer identity proof; if a proof
// already arrived, this triggers verification immediately.
//
// First-write-wins for the key value: subsequent calls with a
// matching key are no-ops; subsequent calls with a *different* key
// are logged and ignored (this would indicate either a bug in the
// platform's contract authoring or, more concerning, an MITM
// substituting different keys in different contracts to the same
// peer — either way, refusing to switch keys mid-session is the safe
// behavior).
func (self *peerEncryptionSession) SetPeerClientPublicKey(pub ed25519.PublicKey) {
	if len(pub) != ed25519.PublicKeySize {
		glog.V(1).Infof(
			"[tls]%s SetPeerClientPublicKey rejected: key length %d (expected %d)\n",
			self.logTag, len(pub), ed25519.PublicKeySize,
		)
		return
	}
	changed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if len(self.peerClientPublicKey) == 0 {
			self.peerClientPublicKey = append(ed25519.PublicKey(nil), pub...)
			return true
		}
		if !bytes.Equal(self.peerClientPublicKey, pub) {
			glog.Errorf(
				"[tls]%s peer client public key mismatch with prior commitment — refusing to rotate mid-session\n",
				self.logTag,
			)
		}
		return false
	}()
	if changed {
		glog.V(1).Infof("[tls]%s peer client public key set (first-time)\n", self.logTag)
		self.maybeVerifyPendingPeerIdentityProof()
		// Out-of-band cross-check: ask the per-session fetcher
		// (typically the platform's unauthenticated `/key/<peerId>`
		// API) what it thinks this peer's public key is, and log a
		// loud warning if that value disagrees with what the contract
		// just told us. We only do this once per session (`changed`
		// is true only on the first set) and asynchronously so the
		// contract-processing path is never blocked on an HTTP round
		// trip. Today we only log on mismatch; we still trust the
		// contract-supplied key for verification. The fetcher is
		// session-scoped; the fetched key is compared and discarded,
		// never retained on the session.
		if self.peerClientPublicKeyFetcher != nil {
			contractPub := append(ed25519.PublicKey(nil), pub...)
			go HandleError(func() {
				self.crossCheckPeerClientPublicKey(contractPub)
			})
		}
	}
}

// crossCheckPeerClientPublicKey fetches the peer's public client key
// via the session's per-session fetcher (set from
// `EncryptionSettings.NewPeerClientPublicKeyFetcher` at session
// creation) and compares it against `contractPub` — the value just
// delivered to `SetPeerClientPublicKey` by the contract. A mismatch
// is logged loudly: the platform either (a) is racing different
// contracts with inconsistent keys (data bug), or (b) is substituting
// keys to mount a MITM (security bug). Today, log only — the
// contract-supplied key is still trusted for cert and identity-proof
// verification. A future iteration will harden this to refuse the
// substituted key. The fetched key lives only on the goroutine
// stack: not stored on the session, not cached by the manager.
func (self *peerEncryptionSession) crossCheckPeerClientPublicKey(contractPub ed25519.PublicKey) {
	fetched, err := self.peerClientPublicKeyFetcher(self.ctx)
	if err != nil {
		glog.V(1).Infof("[key]%s peer public-key fetch failed: %s\n", self.logTag, err)
		return
	}
	if len(fetched) == 0 {
		glog.V(1).Infof("[key]%s peer public-key fetch returned no key (peer has not published yet)\n", self.logTag)
		return
	}
	if len(fetched) != ed25519.PublicKeySize {
		glog.Errorf(
			"[key]%s peer public-key fetch returned %d bytes (expected %d)\n",
			self.logTag, len(fetched), ed25519.PublicKeySize,
		)
		return
	}
	if !bytes.Equal(fetched, contractPub) {
		glog.Errorf(
			"[key]%s CONTRACT vs FETCHED peer client public key MISMATCH for %s — possible platform MITM (today: log only, contract value still trusted)\n",
			self.logTag, self.peerId,
		)
		return
	}
	glog.V(1).Infof("[key]%s peer public-key cross-check OK\n", self.logTag)
}

// PeerClientPublicKey returns the recorded peer public key (may be
// nil if none has been set yet). Read under stateLock and returned by
// value so callers can use it without coordination.
func (self *peerEncryptionSession) PeerClientPublicKey() ed25519.PublicKey {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.peerClientPublicKey) == 0 {
		return nil
	}
	return append(ed25519.PublicKey(nil), self.peerClientPublicKey...)
}

// DeliverEncryptedControl is called by a ReceiveSequence when it sees a
// TransferEncryptedControl frame addressed to this session's peer.
//
// Two control types are recognized:
//
//   - `EncryptedControlHandshake`: raw TLS handshake bytes. Fed into
//     the per-peer TLS transport for the TLS state machine to process.
//     `Client.run` may have already applied this same payload via
//     `OptimisticallyDeliverHandshake` (for handshake bytes that
//     arrive when we're in the awaiting-client-Finished window) — if
//     so, the handshake has completed by now and the TLS state
//     machine no longer reads from `transport.inbox`. Re-delivering
//     would just enlarge the inbox buffer to no purpose. Skip when
//     `handshakeDone` is already closed.
//
//   - `EncryptedControlIdentityProof`: the peer's Ed25519 signature
//     over the TLS exporter output, made with its long-lived client
//     identity key. Verified against the peer's public client key
//     (set via `SetPeerClientPublicKey` from contract data). Verifies
//     immediately if both the peer key and our own exporter are
//     available; otherwise buffered and tried again as those become
//     available. Success flips `peerIdentityVerified`, at which point
//     `Cipher()` exposes the AEAD; failure flips `identityFailed`
//     (sticky), permanently keeping the cipher hidden so the session
//     stays plaintext.
func (self *peerEncryptionSession) DeliverEncryptedControl(ec *protocol.EncryptedControl) {
	switch ec.ControlType {
	case protocol.EncryptedControlType_EncryptedControlHandshake:
		select {
		case <-self.handshakeDone:
			glog.V(2).Infof(
				"[tls]%s DeliverEncryptedControl(Handshake) %d bytes — skipped, handshake already done\n",
				self.logTag, len(ec.Payload),
			)
			return
		default:
		}
		var firstByte byte
		if 0 < len(ec.Payload) {
			firstByte = ec.Payload[0]
		}
		glog.V(1).Infof(
			"[tls]%s DeliverEncryptedControl(Handshake): feeding %d bytes (record type 0x%02x) to TLS state\n",
			self.logTag, len(ec.Payload), firstByte,
		)
		self.startInternal()
		if self.transport != nil {
			self.transport.Deliver(ec.Payload)
		}
	case protocol.EncryptedControlType_EncryptedControlIdentityProof:
		glog.V(1).Infof(
			"[tls]%s DeliverEncryptedControl(IdentityProof): received %d-byte proof\n",
			self.logTag, len(ec.Payload),
		)
		self.receivePeerIdentityProof(ec.Payload)
	default:
		glog.V(1).Infof(
			"[tls]%s DeliverEncryptedControl: unknown control type %v (%d bytes)\n",
			self.logTag, ec.ControlType, len(ec.Payload),
		)
	}
}

// receivePeerIdentityProof buffers the peer's identity-proof payload
// and attempts verification. If the peer key and our TLS exporter are
// both already known, verification runs immediately. Otherwise the
// payload sits in `pendingPeerIdentityProof` and gets retried by
// `maybeVerifyPendingPeerIdentityProof` whenever one of the missing
// pieces becomes available (TLS handshake completes;
// `SetPeerClientPublicKey` is called).
//
// A second proof arriving while one is already buffered or after a
// definitive verification (success or failure) is ignored: the
// session has at most one identity-proof exchange per session
// lifetime, and a second proof from the peer is either a benign
// retransmit or a manipulation attempt — either way, refusing to
// re-evaluate is safe.
func (self *peerEncryptionSession) receivePeerIdentityProof(payload []byte) {
	stored, skipReason := func() (bool, string) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.peerIdentityVerified {
			return false, "already verified"
		}
		if self.identityFailed {
			return false, "identity already failed"
		}
		if len(self.pendingPeerIdentityProof) != 0 {
			return false, "another proof already buffered"
		}
		if len(payload) != ed25519.SignatureSize {
			// invalid shape; record as failed to keep the session
			// from ever exposing its cipher.
			self.identityFailed = true
			return false, fmt.Sprintf("malformed sig length %d (expected %d)", len(payload), ed25519.SignatureSize)
		}
		self.pendingPeerIdentityProof = append([]byte(nil), payload...)
		return true, ""
	}()
	if !stored {
		// either we already have a proof in flight, the session is
		// already in a final identity state, or we marked failure
		// because the payload was malformed.
		glog.V(1).Infof("[tls]%s receivePeerIdentityProof skipped: %s\n", self.logTag, skipReason)
		self.readyMonitor.NotifyAll()
		return
	}
	glog.V(1).Infof("[tls]%s receivePeerIdentityProof buffered %d-byte proof — attempting verify\n", self.logTag, len(payload))
	self.maybeVerifyPendingPeerIdentityProof()
}

// OptimisticallyDeliverHandshake feeds handshake payload bytes directly
// into the per-peer TLS transport from `Client.run` — bypassing the
// ReceiveSequence drain that would otherwise reach the session via
// `DeliverEncryptedControl`. The intent is to set the cipher *in the
// same receive-loop turn* that processed the inbound EncryptedControl
// frame, so any wrapped app-data frames in the immediately-following
// reads (which the unwrap path used to wait on `ReadyNotify` for) find
// the cipher already set.
//
// Gated on `IsAwaitingClientFinished` so it's a no-op outside the
// narrow TLS-server window where it pays off (and where the
// just-arrived EC frame is, by construction, the expected client
// second flight). Once the handshake completes, `IsAwaitingClientFinished`
// returns false and subsequent calls do nothing.
//
// Called from the single-threaded receive loop, so it must not block:
// `transport.Deliver` just appends to the inbox buffer under a quick
// lock and notifies the runHandshake goroutine to wake up and read.
//
// The normal-path `DeliverEncryptedControl` will still see this EC
// frame when the ReceiveSequence drains it; by then the handshake is
// (almost always) complete and the redelivery short-circuits via the
// `handshakeDone` check. The tiny race window where both paths
// deliver the same bytes just costs a few KB of bytes left in the
// inbox buffer — bounded by the size of the client second flight.
//
// Retransmit filter: the source of this pack is already validated by
// the transfer layer (every hop verifies source), so the only way
// stale handshake bytes can reach us is a sender-side resend of an
// earlier handshake message — practically, a ClientHello retransmit
// from before our server flight went out (the sender's resend timer
// for the ClientHello pack fired before our ack got back). We can't
// dedupe by (sequenceId, sequenceNumber) here without reaching into
// the ReceiveSequence's state; instead we use a one-byte structural
// check on the TLS record header. In TLS 1.3 the legitimate client
// second flight starts with either:
//   - record type 20 (legacy `ChangeCipherSpec`, sent for middlebox
//     compatibility), or
//   - record type 23 (encrypted `application_data`, which is how
//     post-handshake-secrets handshake messages are wrapped).
//
// A ClientHello (initial or HRR retry) is record type 22 — caught by
// this filter. Anything else (unencrypted alert at type 21, etc.) is
// not a legitimate client-second-flight prefix either. Skip and let
// the ReceiveSequence's normal dedupe handle the retransmit through
// the normal path (which short-circuits if duplicate or applies
// correctly if new — its sequence-number bookkeeping is what makes it
// safe to feed bytes to the TLS state machine).
func (self *peerEncryptionSession) OptimisticallyDeliverHandshake(payload []byte) {
	if !self.IsAwaitingClientFinished() {
		glog.V(2).Infof(
			"[tls]%s OptimisticallyDeliverHandshake skipped: not awaiting client Finished\n",
			self.logTag,
		)
		return
	}
	// isClientSecondFlightPrefix: the first byte of `payload` is a TLS
	// 1.3 record content type compatible with the start of a
	// legitimate client second flight: 20 (legacy ChangeCipherSpec) or
	// 23 (encrypted application_data carrying handshake messages).
	// Type 22 (unencrypted Handshake) is rejected — that's the type
	// used by ClientHello and HRR retry ClientHello, i.e. the messages
	// a sender would retransmit before our server flight reached them.
	// Type 21 (Alert) is also rejected — alerts after handshake-secrets
	// are derived are encrypted under type 23.
	isClientSecondFlightPrefix := func() bool {
		if len(payload) == 0 {
			return false
		}
		switch payload[0] {
		case 20, 23:
			return true
		default:
			return false
		}
	}
	if !isClientSecondFlightPrefix() {
		var firstByte byte
		if 0 < len(payload) {
			firstByte = payload[0]
		}
		glog.V(1).Infof(
			"[tls]%s OptimisticallyDeliverHandshake filtered out: %d bytes, first byte 0x%02x (not a client-second-flight prefix)\n",
			self.logTag, len(payload), firstByte,
		)
		return
	}
	glog.V(1).Infof(
		"[tls]%s OptimisticallyDeliverHandshake: feeding %d bytes (record type 0x%02x) to TLS state\n",
		self.logTag, len(payload), payload[0],
	)
	self.startInternal()
	if self.transport != nil {
		self.transport.Deliver(payload)
	}
}

// ReadyNotify returns a channel that closes when the session's ready
// state changes — that is, when the handshake completes (cipher set) or
// fails (cipher will stay nil). Single-use; callers re-check `Cipher()`
// after a notify.
func (self *peerEncryptionSession) ReadyNotify() chan struct{} {
	return self.readyMonitor.NotifyChannel()
}

// Cipher returns the AEAD cipher, or nil if the session is not in a
// state where it can be safely used to wrap/unwrap application
// frames. Nil means "treat as plaintext for this peer." The AEAD is
// withheld until ALL of:
//   - the TLS handshake has completed successfully (cipher derived),
//   - the peer's identity proof has been verified against the peer's
//     long-lived public client key.
//
// If either prerequisite fails — TLS handshake errors out, identity
// proof verifies to the wrong signature, or never arrives at all —
// the cipher stays nil for the lifetime of the session and traffic
// flows in plaintext. This matches the existing "cipher is a binary
// property of the session" wire semantics (see
// `writeMaybeWrappedBytes`): peers that cannot mutually authenticate
// fall back to plaintext rather than encrypted-but-unauthenticated.
func (self *peerEncryptionSession) Cipher() *sequenceCipher {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.peerIdentityVerified {
		if glog.V(2) {
			// Only at V(2): this is called per send. We trace the
			// specific reason the cipher isn't available so the
			// caller can correlate plaintext writes to the underlying
			// gate.
			reason := "tls handshake not done"
			if self.derivedTlsCipher != nil {
				if self.identityFailed {
					reason = "identity proof verification FAILED"
				} else if len(self.peerClientPublicKey) == 0 {
					reason = "peer client public key not yet known"
				} else if len(self.pendingPeerIdentityProof) == 0 {
					reason = "peer identity proof not yet received"
				} else {
					reason = "identity verification pending"
				}
			}
			glog.V(2).Infof("[tls]%s Cipher()=nil: %s\n", self.logTag, reason)
		}
		return nil
	}
	return self.derivedTlsCipher
}

// IsAwaitingClientFinished reports whether this session is in the narrow
// TLS-server-role window after sending its own Finished and before
// receiving the peer's Finished — the only state in which it's
// meaningful for the unwrap path to briefly block the receive loop
// waiting for the cipher.
//
// Returns false:
//   - in TLS-client role (the local cipher is already set first; a
//     wrapped frame arriving in that direction is the result of the
//     peer racing ahead with its own cipher, not anything we can
//     resolve by waiting locally — drop and let the sender resend);
//   - before this side has produced its server flight (no ClientHello
//     processed yet, so blocking can't resolve within any reasonable
//     window; also the natural state of a freshly-created session that
//     hasn't seen any handshake input — keeping wait off here closes
//     the obvious DoS surface where an attacker creates a session
//     reference and then stalls);
//   - once the handshake has completed (success: cipher is already
//     non-nil; failure: cipher will never be set and waiting is
//     pointless).
//
// Keeping this predicate tight bounds how long the single-threaded
// `Client.run` receive loop can be parked by any one wrapped frame.
func (self *peerEncryptionSession) IsAwaitingClientFinished() bool {
	if self.role != sequenceTlsRoleServer {
		return false
	}
	select {
	case <-self.handshakeDone:
		return false
	default:
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.serverFlightSent
}

func (self *peerEncryptionSession) closeTls() {
	if self.transport != nil {
		self.transport.Close()
	}
	if self.tlsConn != nil {
		_ = self.tlsConn.CloseWrite()
	}
}

// PeerCertificates returns the peer's certificate chain observed during
// the TLS handshake. Returns nil if the handshake has not completed.
func (self *peerEncryptionSession) PeerCertificates() []*x509.Certificate {
	if self.tlsConn == nil {
		return nil
	}
	state := self.tlsConn.ConnectionState()
	if !state.HandshakeComplete {
		return nil
	}
	return state.PeerCertificates
}

// CertVerificationState returns the cached certificate verification
// result. `verified` is true once a peer cert was successfully matched to
// the trusted set and is sticky for the session lifetime. `noCommitment`
// reflects the current state of the trusted set: it's true exactly when
// the set is empty, so callers can distinguish "no commitment held yet"
// (verification should skip without latching) from "have a commitment
// that hasn't been checked yet" (verification should run). Not sticky:
// if an empty-chain contract is later followed by one with a cert, the
// noCommitment status flips off and verification runs against the new
// commitment.
func (self *peerEncryptionSession) CertVerificationState() (verified bool, noCommitment bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.certVerified, len(self.trustedPeerCertPems) == 0
}

// MarkCertVerified records that the peer's TLS-handshake cert was matched
// against the trusted set. The result is sticky for the session lifetime.
func (self *peerEncryptionSession) MarkCertVerified() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.certVerified = true
}

// AddTrustedPeerCertChain extends the session's trusted peer cert set
// with `chain` (PEM-encoded, leaf first), gated on a successful
// signature check. `chainSig` must be the peer's Ed25519 signature
// over the canonical encoding of `chain` (concatenation of PEM
// blocks), made under the peer's long-lived client identity key. The
// verifier is the session's currently-known peer public client key
// (`peerClientPublicKey`, set via `SetPeerClientPublicKey`).
//
// All four states are handled:
//   - empty chain → no-op (no commitment to add).
//   - non-empty chain + no peer client key known yet → no-op; the
//     chain is dropped (and the contract's commitment is effectively
//     unverifiable until the peer key is delivered). A later contract
//     that arrives with both pieces will succeed.
//   - non-empty chain + peer key known + signature absent → no-op;
//     log a warning. Pre-identity-rollout peers may not carry sigs
//     yet; once they do, the cert is admitted.
//   - non-empty chain + peer key known + signature present and
//     verifies → admit all PEM blocks into `trustedPeerCertPems`.
//   - non-empty chain + peer key known + signature present and
//     FAILS → log and drop. Signals a substitution attempt
//     (platform-authored MITM) or a misbehaving peer; in either case
//     we refuse to extend trust to this cert.
//
// Skipping (rather than latching failure) on any of the "no-op" cases
// is deliberate: a single missing piece doesn't poison the session;
// a later contract that has everything still works. Hard-failure is
// reserved for an actively-bad signature.
func (self *peerEncryptionSession) AddTrustedPeerCertChain(chain [][]byte, chainSig []byte) {
	if len(chain) == 0 {
		return
	}
	peerPub := func() ed25519.PublicKey {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if len(self.peerClientPublicKey) != ed25519.PublicKeySize {
			return nil
		}
		return append(ed25519.PublicKey(nil), self.peerClientPublicKey...)
	}()
	if peerPub == nil {
		glog.V(1).Infof(
			"[tls]%s contract cert chain dropped: peer client public key not yet known\n",
			self.logTag,
		)
		return
	}
	if len(chainSig) == 0 {
		glog.Errorf(
			"[tls]%s contract cert chain dropped: no client-key signature provided\n",
			self.logTag,
		)
		return
	}
	if !VerifyCertChainSignature(peerPub, chain, chainSig) {
		glog.Errorf(
			"[tls]%s contract cert chain dropped: client-key signature verification FAILED\n",
			self.logTag,
		)
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.trustedPeerCertPems == nil {
		self.trustedPeerCertPems = map[string]bool{}
	}
	for _, blockPem := range chain {
		if len(blockPem) == 0 {
			continue
		}
		self.trustedPeerCertPems[string(blockPem)] = true
	}
}

// trustedPeerCertSnapshot returns a copy of the session's trusted PEM
// blocks suitable for handing to `verifyPeerCertificateAgainstContract`.
func (self *peerEncryptionSession) trustedPeerCertSnapshot() [][]byte {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.trustedPeerCertPems) == 0 {
		return nil
	}
	out := make([][]byte, 0, len(self.trustedPeerCertPems))
	for blockPem := range self.trustedPeerCertPems {
		out = append(out, []byte(blockPem))
	}
	return out
}

// Release drops one reference. When the count reaches zero, the session
// cancels its context — its `Run` goroutine then returns, and the manager's
// per-session supervisor goroutine removes the session from manager state.
// Mirrors the SendSequence / ReceiveSequence pattern.
func (self *peerEncryptionSession) Release() {
	if self == nil {
		return
	}
	free := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.refs--
		return self.refs <= 0
	}()
	if free {
		self.close()
	}
}

// retain bumps the session's reference count under stateLock. Called by
// the manager when handing the session to a caller (via `Acquire`).
func (self *peerEncryptionSession) retain() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.refs++
}

// close cancels the session context and tears down TLS state. Idempotent.
// The session's `Run` goroutine observes ctx.Done() and exits; its
// supervisor in the manager then unregisters the session.
func (self *peerEncryptionSession) close() {
	self.cancel()
	self.closeTls()
}

// EncryptionSessionManager owns per-peer TLS sessions for the local Client.
// Sessions are keyed by the **peer's `ClientId`** only — independent of
// transport, route, stream, or contract. Every local `SendSequence` to a
// given peer, and every `ReceiveSequence` from that peer, shares the
// same session and therefore the same cipher; the cipher derived by the
// original handshake is reused for every direction (including companion
// replies) until the session is torn down.
type EncryptionSessionManager struct {
	ctx              context.Context
	client           *Client
	clientKeyManager *ClientKeyManager
	settings         *EncryptionSettings

	// `serverTlsConfig` is the receiver-role TLS config used by every
	// per-peer session this manager owns. It carries one persistent
	// X.509 certificate (`selfCertPem` below) — the local client's public
	// identity for all sequence-level TLS sessions, independent of provide
	// mode or peer. The cert is generated at construction (or inherited
	// from `settings.ServerTlsConfig` if the caller supplied one) and
	// republished to the platform via `EncryptedKey` so contracts whose
	// destination is this client carry it as a commitment.
	serverTlsConfig *tls.Config
	clientTlsConfig *tls.Config
	// PEM-encoded chain of the local cert (leaf first). Published via
	// `EncryptedKey`. Empty when encryption is disabled.
	selfCertPem [][]byte
	// PEM-encoded private key (PKCS#8, single block) that matches the
	// leaf cert in `selfCertPem`. Held so persistence callers can read
	// the matched pair back through
	// `ProvideTlsCertificatePem()` / `ProvideTlsPrivateKeyPem()` and
	// load it on a subsequent run. Empty when encryption is disabled
	// or when the caller provided a `tls.Config` whose certificate
	// has no exposed private key (e.g., a `GetCertificate` callback).
	selfPrivateKeyPem []byte

	controlSyncEncryptedKey *ControlSync

	stateLock sync.Mutex
	sessions  map[Id]*peerEncryptionSession
}

func NewEncryptionSessionManager(ctx context.Context, client *Client, clientKeyManager *ClientKeyManager, settings *EncryptionSettings) *EncryptionSessionManager {
	if settings == nil {
		settings = DefaultEncryptionSettings()
	}
	m := &EncryptionSessionManager{
		ctx:              ctx,
		client:           client,
		clientKeyManager: clientKeyManager,
		settings:         settings,
		sessions:         map[Id]*peerEncryptionSession{},
	}
	if settings.Encrypt {
		serverTlsConfig, selfCertPem, selfPrivateKeyPem, err := resolveReceiveTlsConfig(
			settings.ServerTlsConfig,
			settings.ProvideTlsCertificatePem,
			settings.ProvideTlsPrivateKeyPem,
		)
		if err != nil {
			glog.Errorf("[tls]%s could not initialize server TLS config: %s\n", client.ClientTag(), err)
		} else {
			m.serverTlsConfig = serverTlsConfig
			m.selfCertPem = selfCertPem
			m.selfPrivateKeyPem = selfPrivateKeyPem
		}
		m.clientTlsConfig = resolveSendTlsConfig(settings.ClientTlsConfig)
		// Mutual TLS: the TLS-client side presents the same self cert the
		// TLS-server side does, so the peer (acting as TLS-server for this
		// session) can hand the cert to its sequence-level verifier. Roles
		// are lex-assigned on ClientId, so verification has to work in
		// either direction. The caller's ClientTlsConfig (if any) may
		// already carry Certificates — preserve those and only fill in
		// from the manager's self cert.
		if m.clientTlsConfig != nil && len(m.clientTlsConfig.Certificates) == 0 &&
			m.serverTlsConfig != nil && 0 < len(m.serverTlsConfig.Certificates) {
			m.clientTlsConfig.Certificates = append(
				[]tls.Certificate(nil),
				m.serverTlsConfig.Certificates...,
			)
		}
		m.controlSyncEncryptedKey = NewControlSync(ctx, client, "encrypted-key")
		// Publish the cert so every contract whose destination is this
		// client carries it. ControlSync retries until the platform acks.
		// The publisher waits on `client.ReadyNotify()` before its first
		// send: this constructor runs inside `NewClientWithTag`, before
		// `initBuffers` has wired up `sendBuffer`, so sending immediately
		// would race with (or precede) the buffer construction.
		if 0 < len(m.selfCertPem) {
			go HandleError(m.publishEncryptedKey)
		}
	}
	return m
}

// publishEncryptedKey sends an `EncryptedKey` control message to the
// platform with this manager's persistent cert chain. Idempotent on the
// platform side — every call replaces the stored cert for this client.
// Uses `ControlSync` so the publish retries until acked.
//
// Started as a goroutine from `NewEncryptionSessionManager`. Waits on
// the client's `ReadyNotify()` before its first send so the client is
// fully wired (especially `sendBuffer`) before any control traffic
// leaves; without this gate, the send path can race the buffer wiring
// in `Client.initBuffers`.
func (self *EncryptionSessionManager) publishEncryptedKey() {
	if self.controlSyncEncryptedKey == nil {
		return
	}
	select {
	case <-self.client.ReadyNotify():
	case <-self.ctx.Done():
		return
	}
	// Sign the cert chain with our long-lived client identity key.
	// Senders that fetch our public client key out-of-band can then
	// verify the cert chain attached to a contract by the platform
	// was authentically committed by us — substituting the cert
	// without forging this signature requires our private key.
	var certSig []byte
	if self.clientKeyManager != nil && 0 < len(self.selfCertPem) {
		certSig = self.clientKeyManager.SignCertChain(self.selfCertPem)
	}
	frame, err := ToFrame(&protocol.EncryptedKey{
		ProvideTlsCertificate:         self.selfCertPem,
		ClientKeySignedTlsCertificate: certSig,
	}, self.client.settings.ProtocolVersion)
	if err != nil {
		glog.Errorf("[tls]%s could not build EncryptedKey frame: %s\n", self.client.ClientTag(), err)
		return
	}
	self.controlSyncEncryptedKey.Send(frame, nil, nil)
}

// SelfCertPem returns the PEM-encoded chain of the local cert (leaf first)
// that this manager publishes via `EncryptedKey`. Returns nil when
// encryption is disabled or cert setup failed.
func (self *EncryptionSessionManager) SelfCertPem() [][]byte {
	return self.selfCertPem
}

// ProvideTlsCertificatePem returns the local TLS cert chain as a
// single concatenated PEM byte slice (one or more
// `-----BEGIN CERTIFICATE-----` blocks, leaf first). Pair with
// `ProvideTlsPrivateKeyPem` to persist and reload the cert across
// restarts via `EncryptionSettings.ProvideTlsCertificatePem` /
// `ProvideTlsPrivateKeyPem`. Returns nil when encryption is disabled
// or cert setup failed.
func (self *EncryptionSessionManager) ProvideTlsCertificatePem() []byte {
	if len(self.selfCertPem) == 0 {
		return nil
	}
	var n int
	for _, b := range self.selfCertPem {
		n += len(b)
	}
	out := make([]byte, 0, n)
	for _, b := range self.selfCertPem {
		out = append(out, b...)
	}
	return out
}

// ProvideTlsPrivateKeyPem returns the PEM-encoded PKCS#8 private key
// matching the leaf of `ProvideTlsCertificatePem()`. Empty when
// encryption is disabled, when the cert was supplied via a
// `tls.Config` with no exposed private key (e.g. `GetCertificate`
// callback), or when cert setup failed.
func (self *EncryptionSessionManager) ProvideTlsPrivateKeyPem() []byte {
	return self.selfPrivateKeyPem
}

// ServerTlsConfig returns the TLS config used by every per-peer session
// this manager owns when it takes the TLS-server role.
func (self *EncryptionSessionManager) ServerTlsConfig() *tls.Config {
	return self.serverTlsConfig
}

// ClientTlsConfig returns the TLS config used by every per-peer session
// this manager owns when it takes the TLS-client role.
func (self *EncryptionSessionManager) ClientTlsConfig() *tls.Config {
	return self.clientTlsConfig
}

func (self *EncryptionSessionManager) Settings() *EncryptionSettings {
	return self.settings
}

// roleForPeer chooses the local role for the per-peer session — the side
// with the lexicographically lower ClientId is the TLS client. Equal IDs
// shouldn't happen; default to client.
func roleForPeer(local Id, peer Id) sequenceTlsRole {
	if bytes.Compare(local[:], peer[:]) <= 0 {
		return sequenceTlsRoleClient
	}
	return sequenceTlsRoleServer
}

// Acquire returns the session for `peerId`, creating one if it doesn't
// exist. Increments the session's reference count; callers call
// `session.Release()` to drop their reference when done. Returns nil when
// encryption is disabled or `peerId` is the zero Id (e.g., control
// destinations).
func (self *EncryptionSessionManager) Acquire(peerId Id) *peerEncryptionSession {
	if !self.settings.Encrypt {
		return nil
	}
	if (peerId == Id{}) {
		return nil
	}
	session := self.getOrCreate(peerId)
	if session == nil {
		return nil
	}
	session.retain()
	return session
}

// getOrCreate returns the session for `peerId`, creating and supervising
// it if one doesn't exist. Takes the manager's stateLock internally. Logs
// at V(1) whether the session was reused or freshly created — the reuse
// log line tells you a companion reply (or any second-direction traffic)
// is riding the original direction's already-established keys.
func (self *EncryptionSessionManager) getOrCreate(peerId Id) *peerEncryptionSession {
	session, created := func() (*peerEncryptionSession, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if s, ok := self.sessions[peerId]; ok {
			return s, false
		}
		role := roleForPeer(self.client.ClientId(), peerId)
		s := newPeerEncryptionSession(self.ctx, self, self.client, peerId, role, self.settings)
		self.sessions[peerId] = s
		return s, true
	}()
	if created {
		glog.V(1).Infof("[tls]%s opened session for peer %s as %s\n", self.client.ClientTag(), peerId, session.role)
		go func() {
			defer func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				if self.sessions[peerId] == session {
					delete(self.sessions, peerId)
				}
			}()
			session.Run()
		}()
	} else {
		glog.V(1).Infof("[tls]%s reusing established session for peer %s as %s\n", self.client.ClientTag(), peerId, session.role)
	}
	return session
}

// Lookup returns an existing session for `peerId` without changing its
// reference count. Returns nil if no session exists or encryption is
// disabled.
func (self *EncryptionSessionManager) Lookup(peerId Id) *peerEncryptionSession {
	if !self.settings.Encrypt {
		return nil
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.sessions[peerId]
}

// DeliverEncryptedControl looks up the session for `peerId` (creating it
// if necessary, e.g. when handshake bytes arrive before any local
// sequence is up for this peer) and delivers the control to it.
func (self *EncryptionSessionManager) DeliverEncryptedControl(peerId Id, ec *protocol.EncryptedControl) {
	if !self.settings.Encrypt {
		return
	}
	if (peerId == Id{}) {
		return
	}
	session := self.getOrCreate(peerId)
	if session == nil {
		return
	}
	session.DeliverEncryptedControl(ec)
}

// Close tears down all sessions. Called when the Client is shutting down.
func (self *EncryptionSessionManager) Close() {
	sessions := func() []*peerEncryptionSession {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		out := make([]*peerEncryptionSession, 0, len(self.sessions))
		for _, s := range self.sessions {
			out = append(out, s)
		}
		return out
	}()
	for _, session := range sessions {
		session.close()
	}
}

// resolveSendTlsConfig returns a usable TLS config for the TLS-client side,
// falling back to the permissive default.
func resolveSendTlsConfig(cfg *tls.Config) *tls.Config {
	if cfg != nil {
		return cfg.Clone()
	}
	return DefaultSequenceClientTlsConfig()
}

// resolveReceiveTlsConfig returns a usable TLS config for the TLS-server
// side along with the PEM-encoded chain (leaf first) of the cert it
// will present and the PEM-encoded private key matching the leaf.
// Resolution precedence:
//
//  1. `cfg.Certificates` (or `cfg.GetCertificate`) — caller-supplied
//     full `tls.Config` wins; the leaf cert/key are exported for
//     persistence callers via the returned PEM values.
//  2. `certPem` + `keyPem` — pre-loaded PEM bytes (e.g., from a
//     previous run saved by `Client.EncryptionSessionManager().
//     ProvideTlsCertificatePem()` / `ProvideTlsPrivateKeyPem()`).
//     Parsed into a `tls.Certificate` and folded into a default
//     config.
//  3. Otherwise, generate a fresh ephemeral self-signed cert via
//     `DefaultSequenceServerTlsConfig`.
func resolveReceiveTlsConfig(cfg *tls.Config, certPem, keyPem []byte) (*tls.Config, [][]byte, []byte, error) {
	if cfg != nil && (0 < len(cfg.Certificates) || cfg.GetCertificate != nil) {
		out := cfg.Clone()
		_, exportedKey := exportCertAndKeyPem(out.Certificates)
		return out, certificateToPemChain(out.Certificates), exportedKey, nil
	}
	if 0 < len(certPem) && 0 < len(keyPem) {
		cert, err := tls.X509KeyPair(certPem, keyPem)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("load provide tls cert/key: %w", err)
		}
		if cert.Leaf == nil && 0 < len(cert.Certificate) {
			if leaf, leafErr := x509.ParseCertificate(cert.Certificate[0]); leafErr == nil {
				cert.Leaf = leaf
			}
		}
		out := &tls.Config{
			Certificates:     []tls.Certificate{cert},
			ClientAuth:       tls.RequireAnyClientCert,
			MinVersion:       tls.VersionTLS13,
			NextProtos:       []string{"urnetwork/sequence"},
			CurvePreferences: append([]tls.CurveID(nil), DefaultSequencePostQuantumCurves...),
		}
		if cfg != nil {
			if 0 < cfg.MinVersion {
				out.MinVersion = cfg.MinVersion
			}
			if 0 < len(cfg.NextProtos) {
				out.NextProtos = cfg.NextProtos
			}
			if cfg.ClientAuth != tls.NoClientCert {
				out.ClientAuth = cfg.ClientAuth
			}
		}
		return out, certificateToPemChain(out.Certificates), keyPem, nil
	}
	out, err := DefaultSequenceServerTlsConfig()
	if err != nil {
		return nil, nil, nil, err
	}
	if cfg != nil {
		if 0 < cfg.MinVersion {
			out.MinVersion = cfg.MinVersion
		}
		if 0 < len(cfg.NextProtos) {
			out.NextProtos = cfg.NextProtos
		}
		if cfg.ClientAuth != tls.NoClientCert {
			out.ClientAuth = cfg.ClientAuth
		}
	}
	_, generatedKey := exportCertAndKeyPem(out.Certificates)
	return out, certificateToPemChain(out.Certificates), generatedKey, nil
}

// exportCertAndKeyPem returns the leaf cert chain and the private key
// of `certs[0]` as PEM bytes, suitable for persistence and later
// reload through `resolveReceiveTlsConfig`. Returns nil for the key
// if the cert has no private key set (e.g., when the caller supplied
// a `tls.Config` with only a `GetCertificate` callback).
func exportCertAndKeyPem(certs []tls.Certificate) (certPem []byte, keyPem []byte) {
	if len(certs) == 0 {
		return nil, nil
	}
	cert := certs[0]
	for _, der := range cert.Certificate {
		certPem = append(certPem, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	if cert.PrivateKey != nil {
		if der, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey); err == nil {
			keyPem = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		}
	}
	return certPem, keyPem
}

// certificateToPemChain extracts the PEM-encoded chain of the first
// `tls.Certificate` in `certs`, leaf first. Returns nil when `certs` is
// empty or has no DER blocks. Each entry is one PEM block, suitable for
// publishing via `EncryptedKey` and round-tripping through
// `parsePemCertificates` on the verifier side.
func certificateToPemChain(certs []tls.Certificate) [][]byte {
	if len(certs) == 0 {
		return nil
	}
	cert := certs[0]
	out := make([][]byte, 0, len(cert.Certificate))
	for _, der := range cert.Certificate {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	}
	return out
}

func glogTlsHandshake(tag string, err error) {
	if err != nil {
		glog.V(1).Infof("[tls]%s handshake error = %s\n", tag, err)
	} else {
		glog.V(1).Infof("[tls]%s handshake complete\n", tag)
	}
}

// glogTlsHandshakePeerCert logs the peer's leaf certificate observed after
// the TLS handshake on the client-role side. Useful when triaging
// `verifyPeerCertAgainstContract` failures: this line tells you which cert
// the peer presented, the mismatch log tells you which cert the contract
// committed to.
func glogTlsHandshakePeerCert(tag string, peerCerts []*x509.Certificate) {
	if len(peerCerts) == 0 {
		glog.V(1).Infof("[tls]%s peer presented no certificate\n", tag)
		return
	}
	glog.V(1).Infof("[tls]%s peer leaf subject=%q (chain length=%d)\n", tag, peerCerts[0].Subject.String(), len(peerCerts))
}

// parsePemCertificates parses PEM-encoded certificate blocks into
// `*x509.Certificate` values. Each list entry is one PEM block; only PEM
// is accepted.
func parsePemCertificates(pems [][]byte) ([]*x509.Certificate, error) {
	out := make([]*x509.Certificate, 0, len(pems))
	for _, b := range pems {
		if len(b) == 0 {
			continue
		}
		block, rest := pem.Decode(b)
		if block == nil {
			return nil, errors.New("certificate is not PEM-encoded")
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("unexpected PEM block type %q (expected CERTIFICATE)", block.Type)
		}
		if 0 < len(bytes.TrimSpace(rest)) {
			return nil, errors.New("certificate has trailing content after PEM block")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("invalid PEM certificate: %w", err)
		}
		out = append(out, cert)
	}
	return out, nil
}

// verifyPeerCertificateAgainstContract checks that the peer's leaf
// certificate matches one of the certificates committed to in the contract.
// `expectedPems` is the PEM-encoded chain from
// `Contract.ProvideTlsCertificate` / `StoredContract.ProvideTlsCertificate`.
// When empty (no contract commitment), verification is skipped.
//
// On mismatch, the returned error carries the peer leaf's subject and the
// list of expected subjects so the SendSequence's `[s]` log line is
// self-contained — useful when triaging encrypted-send rejections from
// production logs.
func verifyPeerCertificateAgainstContract(peer []*x509.Certificate, expectedPems [][]byte) (bool, error) {
	if len(expectedPems) == 0 {
		return true, nil
	}
	if len(peer) == 0 {
		return false, errors.New("peer presented no certificate but contract committed to a TLS identity")
	}
	expected, err := parsePemCertificates(expectedPems)
	if err != nil {
		return false, fmt.Errorf("contract certificate parse error: %w", err)
	}
	peerLeaf := peer[0]
	for _, e := range expected {
		if bytes.Equal(peerLeaf.Raw, e.Raw) {
			return true, nil
		}
	}
	expectedSubjects := make([]string, 0, len(expected))
	for _, e := range expected {
		expectedSubjects = append(expectedSubjects, e.Subject.String())
	}
	return false, fmt.Errorf(
		"peer cert subject=%q does not match any of %d contract-committed cert(s): [%s]",
		peerLeaf.Subject.String(),
		len(expected),
		strings.Join(expectedSubjects, ", "),
	)
}
