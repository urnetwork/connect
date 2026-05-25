package connect

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
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
	sequenceTlsKeyLabel      = "urnetwork-sequence-aead"
	sequenceTlsKeyLength     = 32
	sequenceTlsAeadNonceSize = 12
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

	// state (locked)
	stateLock    sync.Mutex
	cipher       *sequenceCipher
	certVerified bool
	refs         int // protected by stateLock
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
	for {
		b, err := self.transport.TakeOutbox(self.ctx)
		if err != nil {
			return
		}
		if len(b) == 0 {
			continue
		}
		self.sendEncryptedControl(&protocol.EncryptedControl{
			ControlType: protocol.EncryptedControlType_EncryptedControlHandshake,
			Payload:     b,
		})
	}
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
				self.cipher = c
			}
		}
		self.handshakeErr = err
		close(self.handshakeDone)
	}()
	glogTlsHandshake(self.logTag, err)
	if err == nil && self.role == sequenceTlsRoleClient {
		glogTlsHandshakePeerCert(self.logTag, self.PeerCertificates())
	}
	// Always notify subscribers — both the success case (cipher available)
	// and the failure case (cipher will stay nil; subscribers waiting on
	// the cipher need to wake up and observe the final state).
	self.readyMonitor.NotifyAll()
}

// DeliverEncryptedControl is called by a ReceiveSequence when it sees a
// TransferEncryptedControl frame addressed to this session's peer. The
// only control type now is `Handshake` — opt-out and request-handshake
// are gone (a sender that doesn't want encryption just doesn't start
// the session; the TLS-client side starts its handshake on its own as
// soon as it's created).
func (self *peerEncryptionSession) DeliverEncryptedControl(ec *protocol.EncryptedControl) {
	if ec.ControlType == protocol.EncryptedControlType_EncryptedControlHandshake {
		self.startInternal()
		if self.transport != nil {
			self.transport.Deliver(ec.Payload)
		}
	}
}

// ReadyNotify returns a channel that closes when the session's ready
// state changes — that is, when the handshake completes (cipher set) or
// fails (cipher will stay nil). Single-use; callers re-check `Cipher()`
// after a notify.
func (self *peerEncryptionSession) ReadyNotify() chan struct{} {
	return self.readyMonitor.NotifyChannel()
}

// Cipher returns the AEAD cipher, or nil if the handshake has not
// completed (or failed — leaving the cipher unset is the steady-state
// signal that traffic should flow in plaintext for this peer).
func (self *peerEncryptionSession) Cipher() *sequenceCipher {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.cipher
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

// AddTrustedPeerCertChain extends the session's trusted peer cert set with
// `chain` (PEM-encoded, leaf first). The verification routine accepts the
// peer's TLS-handshake leaf cert if it matches any entry currently in the
// set.
//
// When `chain` is empty — i.e., the contract carried no
// `ProvideTlsCertificate` commitment — the trusted set is left as-is.
// Verification then either skips (set still empty) or runs against
// whatever certs were added by earlier contracts. This means a
// race between the local sender getting a contract and the peer's
// `EncryptedKey` propagating doesn't permanently disable verification:
// once a later contract carries the peer's cert, verification turns on.
func (self *peerEncryptionSession) AddTrustedPeerCertChain(chain [][]byte) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(chain) == 0 {
		return
	}
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
	ctx      context.Context
	client   *Client
	settings *EncryptionSettings

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

	controlSyncEncryptedKey *ControlSync

	stateLock sync.Mutex
	sessions  map[Id]*peerEncryptionSession
}

func NewEncryptionSessionManager(ctx context.Context, client *Client, settings *EncryptionSettings) *EncryptionSessionManager {
	if settings == nil {
		settings = DefaultEncryptionSettings()
	}
	m := &EncryptionSessionManager{
		ctx:      ctx,
		client:   client,
		settings: settings,
		sessions: map[Id]*peerEncryptionSession{},
	}
	if settings.Encrypt {
		serverTlsConfig, selfCertPem, err := resolveReceiveTlsConfig(settings.ServerTlsConfig)
		if err != nil {
			glog.Errorf("[tls]%s could not initialize server TLS config: %s\n", client.ClientTag(), err)
		} else {
			m.serverTlsConfig = serverTlsConfig
			m.selfCertPem = selfCertPem
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
	frame, err := ToFrame(&protocol.EncryptedKey{
		ProvideTlsCertificate: self.selfCertPem,
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
// side along with the PEM-encoded chain (leaf first) of the cert it will
// present. When `cfg` provides a certificate, that cert is used and its
// chain is returned; otherwise a fresh self-signed cert is generated and
// folded into a default config.
func resolveReceiveTlsConfig(cfg *tls.Config) (*tls.Config, [][]byte, error) {
	if cfg != nil && (0 < len(cfg.Certificates) || cfg.GetCertificate != nil) {
		out := cfg.Clone()
		return out, certificateToPemChain(out.Certificates), nil
	}
	out, err := DefaultSequenceServerTlsConfig()
	if err != nil {
		return nil, nil, err
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
	return out, certificateToPemChain(out.Certificates), nil
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
