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
	"runtime/debug"
	"strings"
	"sync"
	"time"

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
// `ClientId` is the TLS client and drives the handshake (sends ClientHello);
// the other side is the TLS server and waits until it sees ClientHello.
// There's no RequestHandshake kicker: a not-yet-created TLS-client-role
// session is created the moment the client opens a sequence to this peer or
// receives any frame from it (the unwrap path delivers EncryptedControl
// frames through the manager's `getOrCreate`, which spins up the session and
// starts the handshake).
//
// `EncryptedControl` messages (TLS handshake bytes only) ride the normal
// `Pack`/`Ack` flow as a regular `Frame` with `MessageType =
// TransferEncryptedControl`. Pack delivery is reliable and in-order so no
// chunk indexing is needed. The destination `ReceiveSequence` routes these
// frames into the session instead of the application receive callback.
//
// There is no opt-out wire control. A sender that doesn't want encryption
// simply never creates the session (e.g. `Encrypt = false` in
// `EncryptionSettings`); the receiver tolerates plaintext via the cipher-nil
// pass-through in `writeMaybeWrappedBytes` and the ack-mirroring in `writeAck`.
//
// After the handshake both sides export the same key via the TLS session
// exporter and wrap it in an AEAD. Application TransferFrames are outer-wrapped
// at the writer just before the wire and unwrapped by `Client.run` on receipt,
// using the session's cipher.

/*
With encrypt, there are two SendSequence/ReceiveSequence pairs per `(peerId, companion)` session.
Note there may be up to 16 total pairs for any two peers.
The reason we have the two pairs is the session EncryptedControl messages must flow in both directions
between the sender and receiver.

For a single `(peerId, companion)` session, the two pairs look like:
SendSequence(client=true)     --->     ReceiveSequence(server=true)
ReceiveSequence(client=true)  <---     SendSequence(server=true)

SendSequence(client=true) uses companion contracts for the EncryptedControl if set up to use companion contracts.
SendSequence(server=true) uses companion contracts if `settings.EncryptionControlUseCompanion=true`

The session is keyed by `(peerId, companion, role)`. `companion` is the TLS-client initiator's
contract-companion mode, carried on every EncryptedControl (and as a wire hint on wrapped frames and
carrier packs) and echoed unchanged by the responder, so replies route back to the initiator's session.
This keeps a companion-contract and a regular-contract flow to the same peer+role on separate sessions.
The identity `companion` is distinct from `carrierCompanion` — which contract the EC rides: the identity
bit for a client session, but EncryptionControlUseCompanion for a server reply.
*/

const (
	sequenceTlsKeyLabel      = "urnetwork-connect-aead"
	sequenceTlsKeyLength     = 32
	sequenceTlsAeadNonceSize = 12
	// sequenceTlsIdentityProofLabel is the exporter label both peers use to
	// derive the 32-byte payload signed with the long-lived client identity
	// key. Distinct from `sequenceTlsKeyLabel` so the two derivations are
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

// complement returns the role the peer holds for the same session (client
// for server, and vice versa). A session-tagged message we send carries our
// role; the peer routes it to its complement session.
func (r sequenceTlsRole) complement() sequenceTlsRole {
	if r == sequenceTlsRoleClient {
		return sequenceTlsRoleServer
	}
	return sequenceTlsRoleClient
}

func (r sequenceTlsRole) toProtobuf() protocol.SequenceRole {
	if r == sequenceTlsRoleClient {
		return protocol.SequenceRole_SequenceRoleClient
	}
	return protocol.SequenceRole_SequenceRoleServer
}

// sequenceTlsRoleFromProtobuf maps a wire `SequenceRole` to the local enum.
// `ok` is false for `SequenceRoleUnknown` so callers can distinguish "no hint"
// from a concrete role.
func sequenceTlsRoleFromProtobuf(r protocol.SequenceRole) (role sequenceTlsRole, ok bool) {
	switch r {
	case protocol.SequenceRole_SequenceRoleClient:
		return sequenceTlsRoleClient, true
	case protocol.SequenceRole_SequenceRoleServer:
		return sequenceTlsRoleServer, true
	default:
		return sequenceTlsRoleClient, false
	}
}

// sessionKey identifies a per-peer encryption session by
// `(peerId, companion, role)`. Keying by role — rather than one lex-ordered
// session per peer — lets each data direction run over a session whose
// initiator can always restart the handshake, recovering a lost responder
// session. The companion dimension separates a companion-contract and a
// regular-contract flow to the same peer+role (see the file header).
type sessionKey struct {
	peerId    Id
	companion bool
	role      sequenceTlsRole
}

// sequenceTlsTransport is the `net.Conn` the per-peer `tls.Conn` reads from /
// writes to. Bytes the TLS state machine sends accumulate in the outbox; bytes
// received from the peer (via `Deliver`) accumulate in the inbox the state
// machine reads from.
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
// ciphertext; only the destination decrypts. `sessionRole` is the wrapping
// session's role, carried as the destination's decrypt hint (it routes to the
// complement local session); `SequenceRoleUnknown` omits the hint so the
// destination trial-decrypts. `sessionCompanion` is a parallel decrypt hint
// (not complemented on the receiver) so the destination pins the exact
// companion session instead of trialing both.
func buildEncryptedOuterFrameBytes(path TransferPath, ciphertext []byte, sessionRole protocol.SequenceRole, sessionCompanion bool) ([]byte, error) {
	transferFrame := &protocol.TransferFrame{
		TransferPath:           path.ToProtobuf(),
		EncryptedTransferFrame: ciphertext,
	}
	// Carry only a concrete role; leave the unknown hint off the wire.
	if sessionRole != protocol.SequenceRole_SequenceRoleUnknown {
		transferFrame.SessionRole = &sessionRole
	}
	transferFrame.SessionCompanion = &sessionCompanion
	return ProtoMarshal(transferFrame)
}

// DefaultSequencePostQuantumCurves are the key-exchange groups the per-peer
// TLS session prefers. Led by the X25519MLKEM768 hybrid post-quantum group
// (RFC 9794 / draft-ietf-tls-mlkem) to protect against "harvest now, decrypt
// later" quantum attacks where supported, falling back to conventional X25519
// if the peer doesn't advertise the hybrid group.
var DefaultSequencePostQuantumCurves = []tls.CurveID{
	tls.X25519MLKEM768,
	tls.X25519,
}

// DefaultSequenceServerTlsConfig returns a TLS config for the server-role side
// of a per-peer session. It generates a fresh ephemeral self-signed
// certificate; sequences are short-lived and don't need a stable identity at
// this layer (auth is at the contract layer).
//
// Mutual TLS is required: `ClientAuth = RequireAnyClientCert` captures the
// peer's certificate into `ConnectionState.PeerCertificates` regardless of
// role. Go's TLS stack doesn't validate the cert (sequences use self-signed
// ephemeral certs); identity is verified at the sequence layer against the
// contract's `ProvideTlsCertificate` commitment. Without mTLS, whichever side
// lands in the TLS-server role would have no `PeerCertificates`, and contract
// verification would fail for half of all peer pairs (those where the local
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
		// Per-peer sessions never resume, and the client drops
		// post-handshake bytes anyway. Disabling tickets stops the TLS 1.3
		// server from emitting NewSessionTicket records the outbox loop would
		// otherwise ship as a spurious post-handshake EncryptedControl.
		SessionTicketsDisabled: true,
	}, nil
}

// DefaultSequenceClientTlsConfig returns a TLS config for the client-role side
// of a per-peer session. Server identity is not verified at this layer
// (verification happens against the contract commitment). The TLS-client also
// presents a certificate (set by the session manager from the same self cert
// the TLS-server role uses) so contract-based verification works regardless of
// role.
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
	// How long to wait for the TLS handshake before giving up. On timeout the
	// session stays cipher-nil — Cipher() returns nil and traffic flows in
	// plaintext. Sessions never block sequences; whether to accept plaintext
	// is the caller's choice via `Encrypt`.
	//
	// A negative value (the default) disables the timeout: the handshake runs
	// until it completes or the session is closed. The watcher is armed only
	// for a strictly positive duration.
	TlsTimeout time.Duration
	// NewPeerClientPublicKeyFetcher, when non-nil, is invoked once per per-peer
	// session at creation with the peer's ClientId. The returned per-session
	// fetcher is invoked at most once, asynchronously, the first time the
	// peer's long-lived public client identity key is learned from a contract
	// (`SetPeerClientPublicKey`). The session compares the returned key against
	// the contract-supplied key and logs a loud warning on mismatch; today no
	// further action is taken (the contract-supplied key is still used). This
	// is an early-warning signal for a platform substituting keys in contracts
	// (a possible MITM attack); a future iteration will harden this into a
	// refusal to trust the substituted key.
	//
	// The per-session factory shape is deliberate. The fetcher is held only by
	// the session; releasing the session makes the fetcher and any state it
	// captures eligible for GC. A shared fetcher would invite a manager-wide
	// per-peer cache that grows unboundedly with the number of peers ever
	// talked to. The factory makes the session-bounded lifetime explicit.
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
	// `ProvideTlsPrivateKeyPem`, loads the local sequence-level TLS server-role
	// cert + private key instead of generating a fresh pair on construction.
	// Both must be PEM-encoded; the cert is one or more concatenated
	// `-----BEGIN CERTIFICATE-----` blocks (leaf first), the key a single
	// `-----BEGIN ... PRIVATE KEY-----` block. Persisting and reloading these
	// across restarts keeps the client's `EncryptedKey` cert commitment stable
	// — peers holding contracts against the old commitment still recognize the
	// cert when it shows up in a TLS handshake.
	//
	// When either field is empty the manager generates a fresh ephemeral
	// self-signed cert. Setting `ServerTlsConfig` overrides both fields.
	ProvideTlsCertificatePem []byte
	ProvideTlsPrivateKeyPem  []byte

	// EncryptionControlUseCompanion, when true (default), sends
	// EncryptedControl handshake packs as companion-contract traffic.
	// This is required for asymmetric contract setups where the TLS
	// responder has no regular contract back to the initiator. Set
	// false only for test configurations that disable companion
	// contracts (e.g. symmetric-provide tests).
	EncryptionControlUseCompanion bool

	// IdleTimeout governs how long an established (not in-flight) per-peer
	// session with zero references is kept registered before it is reaped.
	// Sessions are ref-counted by the send/receive sequences that use them; a
	// transport reform or brief lull between bursts can drop the ref count to
	// zero. Reaping immediately destroys the established cipher, so the next
	// burst opens a fresh session while the peer is still wrapping under the
	// prior cipher — undecryptable until a full re-handshake completes (and,
	// because the two peers reap independently, this desync is asymmetric and
	// can wedge). Keeping the session lets the next burst reuse it — cipher
	// intact, no handshake churn.
	//
	// Three regimes:
	//   < 0  keep the session in memory forever (never idle-reap),
	//   == 0 reap immediately at refs==0 (legacy behavior),
	//   > 0  keep it registered and reap it once it has been idle
	//        (unreferenced) this long (the Run loop's CancelIfIdle).
	// A handshake in flight is always kept regardless.
	//
	// DefaultClientSettings derives this as max(send-sequence idle,
	// receive-sequence idle), since the session is ref-held by both a send and
	// a receive sequence and must outlive the longer of the two.
	IdleTimeout time.Duration
}

func DefaultEncryptionSettings() *EncryptionSettings {
	return &EncryptionSettings{
		Encrypt:                       false,
		TlsTimeout:                    -1, // negative disables the handshake timeout
		EncryptionControlUseCompanion: true,
		// 0 reaps a standalone session immediately at refs==0 (<0 keeps it
		// forever, >0 idle-reaps after the timeout). DefaultClientSettings
		// overrides this with max(send-sequence idle, receive-sequence idle) so
		// the session outlives the sequences that ref-hold it.
		IdleTimeout: 0,
	}
}

// tlsHandshakeEpoch holds the resettable per-handshake TLS state for a
// peerEncryptionSession. Each TLS handshake — the first or any later
// re-handshake — runs in its own epoch. Resetting (see
// `buildAndStartEpochLocked`) cancels the old epoch's ctx so its goroutines
// exit, then swaps in a fresh epoch, without disturbing the session's per-peer
// state (peer key, trusted cert set, refs). The immutable fields
// (ctx/cancel/transport/tlsConn/handshakeDone) are set at construction and read
// without the lock; the mutable fields are guarded by the owning session's
// stateLock.
type tlsHandshakeEpoch struct {
	ctx    context.Context
	cancel context.CancelFunc

	transport     *sequenceTlsTransport
	tlsConn       *tls.Conn
	handshakeDone chan struct{}
	handshakeErr  error

	// derivedTlsCipher holds the AEAD derived from the completed TLS
	// handshake. It is not exposed via `Cipher()` until the
	// post-handshake identity exchange validates the peer (see
	// `peerIdentityVerified`). Held separately so identity-proof code
	// can sign the exporter without exposing the cipher early.
	derivedTlsCipher *sequenceCipher
	certVerified     bool
	// tlsExporter holds the 32-byte output of
	// `ExportKeyingMaterial("urnetwork-sequence-identity-proof", ...)` from the
	// completed TLS connection. Both peers sign it with their client identity
	// keys, binding the session's negotiated key material to the long-lived
	// identities (defeats active MITM that splits the TLS session). Set once on
	// successful handshake completion; nil before that.
	tlsExporter []byte
	// identityProofSent flips true after we've sent our identity proof over the
	// wire, so we don't double-send if the handshake or the peer-key-arrival
	// path tries to trigger it again.
	identityProofSent bool
	// pendingPeerIdentityProof buffers the peer's `IdentityProof` payload when
	// it arrives before we can verify it (no `tlsExporter` yet because our TLS
	// handshake hasn't completed, or no peer public key yet).
	// `maybeVerifyPendingPeerIdentityProof` drains this when both pieces become
	// available.
	pendingPeerIdentityProof []byte
	// peerIdentityVerified flips true after we've verified a peer identity
	// proof against `peerClientPublicKey` over `tlsExporter`. `Cipher()`
	// returns nil until this is true: an unverified session is treated as
	// unauthenticated and traffic flows in plaintext.
	peerIdentityVerified bool
	// identityFailed flips true on a definitive identity-proof
	// verification failure (wrong signature against a known peer
	// key). Sticky for the epoch: the cipher is never exposed after this.
	identityFailed bool
	// serverFlightSent flips true the first time `outboxLoop` drains a batch in
	// TLS-server role. For TLS 1.3 servers that first batch is the full server
	// flight (ServerHello + EncryptedExtensions + Cert + CertVerify +
	// Finished). Once set, the session is in the brief "sent server Finished,
	// awaiting client Finished" window where `Cipher()` flips non-nil as soon
	// as the client Finished is processed. The unwrap path uses this as the
	// only state in which `OptimisticallyDeliverHandshake` runs, so the cipher
	// is set in the same receive-loop turn that processed the inbound
	// EncryptedControl frame. All other server-side states (cipher already set,
	// handshake failed, no ClientHello processed yet) drop the wrapped frame
	// immediately, keeping the receive-loop work — and therefore the DoS
	// surface — minimal.
	serverFlightSent bool
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
	// companion is this session's identity companion (see the file header),
	// stamped onto every EncryptedControl it emits. Client role: the creating
	// SendSequence's flag. Server role: the initiator's bit from the inbound
	// ClientHello, echoed on every reply. Set once at creation.
	companion bool
	// carrierCompanion is which contract this session's EncryptedControl
	// carriers ride: `companion` for a client session, but
	// EncryptionControlUseCompanion for a server reply (the responder has no
	// regular contract back to the initiator in asymmetric setups).
	carrierCompanion bool
	logTag           string

	settings *EncryptionSettings

	// tlsConfig is the TLS config this session presents for its fixed role,
	// snapshotted from the manager's current key material at creation (in
	// `getOrCreateWithLock`, under the manager lock). Immutable for the session's
	// lifetime: every handshake epoch — the first and any re-handshake — reads it
	// without a lock. Snapshotting here is what lets `buildAndStartEpochWithLock`
	// build an epoch while holding the session lock without calling back into the
	// manager; doing so would take the manager lock under the session lock and
	// invert the manager→session order that `Release`/`CancelIfIdle` acquire them
	// in (an ABBA deadlock). Client role is always non-nil; server role is nil
	// only when no server cert is configured, which fails the epoch closed.
	tlsConfig *tls.Config

	// peerClientPublicKeyFetcher is the session's own out-of-band fetcher for
	// the peer's public client identity key, produced by
	// `EncryptionSettings.NewPeerClientPublicKeyFetcher(peerId)` at session
	// creation. Nil when the manager has no factory configured. Held by the
	// session and nowhere else; releasing the session releases this reference
	// (and any state the application's factory captured for this peer).
	peerClientPublicKeyFetcher func(ctx context.Context) ([]byte, error)

	// state (locked)
	stateLock sync.Mutex
	// epoch is the newest handshake epoch — in-flight (handshaking) or, once it
	// establishes, the established one. A client restart or an inbound
	// ClientHello on an already-established session installs a fresh in-flight
	// epoch here while `establishedEpoch` keeps serving the prior cipher
	// (gap-free rekey). nil until the first `startEpoch`. Guarded by stateLock.
	epoch *tlsHandshakeEpoch
	// establishedEpoch is the most recent epoch that completed its handshake
	// and verified the peer identity. Its cipher wraps outbound frames (see
	// `Cipher()`) and stays serving while a newer `epoch` re-handshakes in the
	// background — so a rekey never drops the cipher. nil until the first
	// successful establishment.
	establishedEpoch *tlsHandshakeEpoch
	// priorEstablishedEpoch is the established epoch from just before the
	// current one. Kept for decrypt only (not wrap): during the brief swap
	// window a peer may still send frames under the prior key before it
	// swaps to the new one. Dropped (cancelled) when superseded by the next
	// rekey.
	priorEstablishedEpoch *tlsHandshakeEpoch
	refs                  int // protected by stateLock
	// lastActivityTime is the last time a sequence acquired or released this
	// session (retain/Release). While refs is zero, the Run loop periodically
	// calls CancelIfIdle, which reaps the session once now-lastActivityTime
	// exceeds EncryptionSettings.IdleTimeout — so a transport reform or brief
	// lull reuses the live cipher instead of churning a fresh handshake.
	// Protected by stateLock.
	lastActivityTime time.Time
	// peerClientPublicKey is the peer's long-lived Ed25519 public identity key,
	// set via `SetPeerClientPublicKey` (from the SendSequence after a contract
	// for the peer arrives). nil until a contract has been seen; until then any
	// peer identity proof that arrives is buffered in the epoch's
	// `pendingPeerIdentityProof`. Per-peer: survives handshake resets.
	peerClientPublicKey ed25519.PublicKey
	// trustedPeerCertPems is the union of all peer cert PEM blocks the session
	// has learned — one entry per block (leaf + any intermediates), keyed by
	// the PEM bytes so duplicates from successive contracts collapse. Sources:
	//   - contracts the local sender holds for this peer
	//     (`Contract.ProvideTlsCertificate`).
	// A peer's TLS-handshake leaf cert is accepted as authentic if it matches
	// any entry. Cert rotation is tolerated: as the peer rotates its
	// `EncryptedKey` publication, new contracts carry the new cert, the set
	// grows, and both old and new certs stay trusted. Per-peer: survives
	// handshake resets.
	trustedPeerCertPems map[string]bool
}

func newPeerEncryptionSession(
	parentCtx context.Context,
	manager *EncryptionSessionManager,
	client *Client,
	peerId Id,
	role sequenceTlsRole,
	settings *EncryptionSettings,
	// tlsConfig is the role's TLS config, snapshotted by the caller from the
	// manager under the manager lock (see the `tlsConfig` field).
	tlsConfig *tls.Config,
	// companion is the session's identity companion (see the `companion` field).
	companion bool,
) *peerEncryptionSession {
	ctx, cancel := context.WithCancel(parentCtx)
	// Client carrier rides the identity bit; a server reply always rides
	// EncryptionControlUseCompanion.
	carrierCompanion := companion
	if role == sequenceTlsRoleServer {
		carrierCompanion = settings != nil && settings.EncryptionControlUseCompanion
	}
	s := &peerEncryptionSession{
		ctx:              ctx,
		cancel:           cancel,
		manager:          manager,
		client:           client,
		peerId:           peerId,
		role:             role,
		companion:        companion,
		carrierCompanion: carrierCompanion,
		logTag:           fmt.Sprintf("%s %s c=%t %s", client.ClientTag(), role, companion, peerId),
		settings:         settings,
		tlsConfig:        tlsConfig,
		lastActivityTime: time.Now(),
	}
	// Mint a per-session out-of-band peer key fetcher from the settings
	// factory. Held for the session's lifetime only — any state the returned
	// closure captures dies with the session, so the peer's key is not retained
	// across session boundaries.
	if settings != nil && settings.NewPeerClientPublicKeyFetcher != nil {
		s.peerClientPublicKeyFetcher = settings.NewPeerClientPublicKeyFetcher(peerId)
	}
	return s
}

// Run is the session's supervisor goroutine, spawned by the manager when the
// session is created. The handshake epoch is started on demand by the acquire
// / inbound-delivery paths (`startEpoch`, `reset`), not here, so re-handshakes
// can swap epochs without restarting Run. When the session ctx is cancelled
// (refs reach zero, or the manager shuts down), Run tears down the current
// epoch's transport and returns; the manager then removes the session —
// mirroring the SendBuffer / SendSequence pattern.
func (self *peerEncryptionSession) Run() {
	defer self.closeTls()

	idleTimeout := time.Duration(0)
	if self.settings != nil {
		idleTimeout = self.settings.IdleTimeout
	}

	// Idle reaping is only armed for a strictly positive timeout. Otherwise a
	// zero-ref session is reaped immediately by Release, and Run just waits for
	// cancellation.
	if 0 < idleTimeout {
		// Poll for idle like resident connections do (see
		// server/connect/resident.go): once the session has been unreferenced
		// and not mid-handshake for longer than IdleTimeout, reap it. This keeps
		// the established cipher alive across a transport reform or brief lull
		// (the next burst reuses it) without leaking sessions indefinitely.
		for {
			if self.CancelIfIdle() {
				if self.client.log.V(1).Enabled() {
					self.client.log.Infof("[tls]%s session idle — reaped\n", self.logTag)
				}
				return
			}
			select {
			case <-self.ctx.Done():
				return
			case <-time.After(idleTimeout):
			}
		}
	}

	<-self.ctx.Done()
}

// currentEpoch returns the session's current handshake epoch, or nil if
// none has been started.
func (self *peerEncryptionSession) currentEpoch() *tlsHandshakeEpoch {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.epoch
}

// isCurrentEpoch reports whether e is still the session's current epoch.
// Goroutines belonging to an epoch that has since been reset use this to
// skip side effects (e.g. sending an identity proof for a dead handshake).
func (self *peerEncryptionSession) isCurrentEpoch(e *tlsHandshakeEpoch) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.epoch == e
}

// startEpoch ensures a handshake epoch exists, creating the first one if
// none has been started. Idempotent: a no-op when an epoch is already
// present. Called from the inbound-delivery paths, which reuse rather than
// restart an in-flight or established handshake.
func (self *peerEncryptionSession) startEpoch() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.epoch == nil {
		self.buildAndStartEpochWithLock()
	}
}

// restartHandshake begins a fresh handshake epoch for a client-role send
// sequence — the mechanism that lets a lost responder session recover, since
// every new client send re-initiates. A handshake already in flight is left to
// complete (so repeated send sequences don't thrash it). The established epoch,
// if any, keeps serving its cipher while the new handshake runs in the
// background, so the rekey is gap-free.
func (self *peerEncryptionSession) restartHandshake() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.epoch != nil && self.epoch != self.establishedEpoch {
		// a handshake is already in flight; let it finish rather than
		// thrash a new one on every send sequence
		return
	}
	self.buildAndStartEpochWithLock()
}

// reset starts a fresh handshake epoch when a definitively new handshake must
// begin — a server seeing a new inbound ClientHello after the prior handshake
// finished. Like `restartHandshake` it preserves the established epoch's cipher
// until the new handshake establishes.
func (self *peerEncryptionSession) reset() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.buildAndStartEpochWithLock()
}

// buildAndStartEpochWithLock builds a fresh in-flight epoch (new in-memory
// transport + tls.Conn in the session's role) and launches its
// handshake/outbox/timeout goroutines bound to the epoch's own ctx. A stale
// in-flight epoch is abandoned (cancelled), but the established epoch is left
// running so its cipher keeps serving wrap/decrypt until the new epoch
// establishes (gap-free rekey). Caller holds stateLock.
func (self *peerEncryptionSession) buildAndStartEpochWithLock() {
	if self.epoch != nil && self.epoch != self.establishedEpoch {
		self.epoch.cancel()
	}
	ctx, cancel := context.WithCancel(self.ctx)
	e := &tlsHandshakeEpoch{
		ctx:           ctx,
		cancel:        cancel,
		transport:     newSequenceTlsTransport(ctx),
		handshakeDone: make(chan struct{}),
	}
	// Use the role's TLS config snapshotted at session creation (see the
	// `tlsConfig` field). Reading it here — instead of calling back into
	// self.manager.ClientTlsConfig()/ServerTlsConfig() — keeps this path from
	// taking the manager lock while holding the session lock, which would invert
	// the manager→session lock order Release/CancelIfIdle rely on (ABBA deadlock).
	tlsCfg := self.tlsConfig
	switch self.role {
	case sequenceTlsRoleClient:
		e.tlsConn = tls.Client(e.transport, tlsCfg)
	case sequenceTlsRoleServer:
		if tlsCfg == nil {
			// Encryption misconfigured (no server cert): fail the epoch
			// closed so the cipher stays nil and traffic flows in
			// plaintext, matching a never-completed handshake.
			e.handshakeErr = errors.New("server tls config unavailable")
			close(e.handshakeDone)
			self.epoch = e
			return
		}
		if 0 < len(tlsCfg.Certificates) && tlsCfg.Certificates[0].Leaf != nil {
			if self.client.log.V(1).Enabled() {
				self.client.log.Infof(
					"[tls]%s server presenting cert subject=%q\n",
					self.logTag, tlsCfg.Certificates[0].Leaf.Subject.String(),
				)
			}
		}
		e.tlsConn = tls.Server(e.transport, tlsCfg)
	}
	self.epoch = e

	// drain TLS outbox → outbound EncryptedControl
	go HandleError(func() { self.outboxLoop(e) }, e.cancel)
	// drive the TLS handshake
	go HandleError(func() { self.runHandshake(e) }, e.cancel)
	// arm the handshake timeout
	if 0 < self.settings.TlsTimeout {
		go HandleError(func() { self.handshakeTimeoutWatcher(e) }, e.cancel)
	}
}

func (self *peerEncryptionSession) runHandshake(e *tlsHandshakeEpoch) {
	err := e.tlsConn.HandshakeContext(e.ctx)
	self.completeHandshake(e, err)
}

func (self *peerEncryptionSession) handshakeTimeoutWatcher(e *tlsHandshakeEpoch) {
	select {
	case <-e.ctx.Done():
		return
	case <-e.handshakeDone:
		return
	case <-time.After(self.settings.TlsTimeout):
		// give the run loop a synthetic timeout error if it hasn't completed
		self.completeHandshake(e, fmt.Errorf("tls handshake timeout after %s", self.settings.TlsTimeout))
	}
}

func (self *peerEncryptionSession) outboxLoop(e *tlsHandshakeEpoch) {
	batchIndex := 0
	for {
		b, err := e.transport.TakeOutbox(e.ctx)
		if err != nil {
			if self.client.log.V(1).Enabled() {
				self.client.log.Infof("[tls]%s outbox loop exit: %s\n", self.logTag, err)
			}
			return
		}
		// TakeOutbox returns buffered bytes even after the epoch ctx is
		// cancelled; don't emit handshake bytes for an epoch that has been
		// reset out from under us.
		select {
		case <-e.ctx.Done():
			return
		default:
		}
		if len(b) == 0 {
			continue
		}
		var firstByte byte
		if 0 < len(b) {
			firstByte = b[0]
		}
		if self.client.log.V(1).Enabled() {
			self.client.log.Infof(
				"[tls]%s outbox batch %d: %d bytes (record type 0x%02x)\n",
				self.logTag, batchIndex, len(b), firstByte,
			)
		}
		batchIndex += 1
		// In TLS-server role the first outbox batch is the server flight
		// (containing the server's Finished). Mark the "awaiting client
		// Finished" state (see `serverFlightSent`) so the unwrap path knows it
		// may deliver the client's second flight optimistically. Idempotent.
		if self.role == sequenceTlsRoleServer {
			self.markServerFlightSent(e)
		}
		self.sendEncryptedControl(&protocol.EncryptedControl{
			ControlType: protocol.EncryptedControlType_EncryptedControlHandshake,
			Payload:     b,
			SessionRole: self.role.toProtobuf(),
			Companion:   self.companion,
		})
	}
}

func (self *peerEncryptionSession) markServerFlightSent(e *tlsHandshakeEpoch) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	e.serverFlightSent = true
}

// isClosed reports whether a struct{} signal channel has been closed.
func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// isClientHelloRecord reports whether b begins with a TLS handshake record
// (content type 22) whose first handshake message is a ClientHello
// (message type 1). ClientHello is sent unencrypted, so its message type
// is in the clear at offset 5, right after the 5-byte TLS record header.
// Used by the TLS-server role to detect that the peer has started a new
// handshake (vs. a stale post-completion record).
func isClientHelloRecord(b []byte) bool {
	return 6 <= len(b) && b[0] == 22 && b[5] == 1
}

// peerCertificatesOfEpoch returns the peer cert chain observed during the
// epoch's completed TLS handshake, or nil if the handshake has not
// completed (or the epoch is nil).
func peerCertificatesOfEpoch(log Logger, e *tlsHandshakeEpoch) []*x509.Certificate {
	if e == nil || e.tlsConn == nil {
		return nil
	}
	// V(2) diagnostic: tls.Conn.ConnectionState() acquires the handshake mutex
	// and blocks until any in-progress handshake completes. The watchdog (armed
	// only under V(2), so it costs nothing otherwise) flags whether this call is
	// parking a caller (e.g. a SendSequence Run loop).
	if log.V(2).Enabled() {
		pcDone := make(chan struct{})
		defer close(pcDone)
		go func() {
			select {
			case <-pcDone:
			case <-time.After(2 * time.Second):
				log.Infof("[tls][peercert-block]ConnectionState() blocked >2s (handshake in progress)\n%s", debug.Stack())
			}
		}()
	}
	state := e.tlsConn.ConnectionState()
	if !state.HandshakeComplete {
		return nil
	}
	return state.PeerCertificates
}

// sendEncryptedControl pushes an EncryptedControl through the normal Pack
// flow to this session's peer. If the pack cannot be enqueued (all
// SendSequences for this destination closed), the session is closed so
// that the next Acquire creates a fresh session with a fresh handshake.
func (self *peerEncryptionSession) sendEncryptedControl(ec *protocol.EncryptedControl) {
	if self.client == nil || self.client.sendBuffer == nil {
		return
	}
	go HandleError(func() {
		// Carry the EC on this session's (peer, companion, role) send sequence,
		// keeping the session it drives referenced for the handshake. The
		// carrier rides `carrierCompanion`'s contract, decoupled from the
		// identity `companion` that keys the session and tags the EC.
		if !self.client.sendBuffer.SendEncryptedControl(self.ctx, self.peerId, self.role, ec, self.companion, self.carrierCompanion) {
			self.close()
		}
	}, self.cancel)
}

// completeHandshake sets the AEAD cipher (or records the error) and notifies
// the outcome of the handshake. Runs at most once. On failure the
// session stays cipher-nil — encryption is a binary property of
// `Cipher() != nil`, so an unset cipher is the same as never having one:
// subsequent traffic flows in plaintext.
func (self *peerEncryptionSession) completeHandshake(e *tlsHandshakeEpoch, err error) {
	done := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		select {
		case <-e.handshakeDone:
			return false
		default:
		}
		if err == nil {
			c, cipherErr := newSequenceCipher(e.tlsConn)
			if cipherErr != nil {
				err = cipherErr
			} else {
				// Hold the AEAD on `derivedTlsCipher`, not the publicly
				// observable `Cipher()`. The cipher becomes visible only after
				// the post-handshake identity proof exchange validates the peer
				// (see `peerIdentityVerified`). Until then the wrap path observes
				// cipher == nil and traffic flows in plaintext, mirroring a
				// not-yet-completed handshake.
				e.derivedTlsCipher = c
				state := e.tlsConn.ConnectionState()
				exporter, exportErr := state.ExportKeyingMaterial(
					sequenceTlsIdentityProofLabel, nil, sequenceTlsIdentityProofLength,
				)
				if exportErr != nil {
					err = fmt.Errorf("identity-proof exporter: %w", exportErr)
				} else {
					e.tlsExporter = exporter
				}
			}
		}
		e.handshakeErr = err
		close(e.handshakeDone)
		return true
	}()
	if !done {
		return
	}
	// If this epoch has already been reset out from under us, don't emit
	// an identity proof bound to a dead handshake — just wake subscribers
	// so they re-read the (now superseded) state.
	if !self.isCurrentEpoch(e) {
		return
	}
	logTlsHandshake(self.client.log, self.logTag, err)
	if err == nil {
		if self.client.log.V(1).Enabled() {
			self.client.log.Infof(
				"[tls]%s completeHandshake: cipher derived, exporter %d bytes — starting identity proof exchange\n",
				self.logTag, len(e.tlsExporter),
			)
		}
		if self.role == sequenceTlsRoleClient {
			logTlsHandshakePeerCert(self.client.log, self.logTag, peerCertificatesOfEpoch(self.client.log, e))
		}
		// Send our identity proof and try to verify any peer proof
		// that arrived early. Both happen on completed-handshake
		// success; on failure neither path is meaningful.
		self.sendIdentityProofOnce(e)
		self.maybeVerifyPendingPeerIdentityProof(e)
	} else {
		self.client.log.Errorf("[tls]%s completeHandshake failed: %s\n", self.logTag, err)
	}
}

// sendIdentityProofOnce signs the TLS exporter output with the local
// client key and ships it to the peer in an
// `EncryptedControl{IdentityProof}`. Runs at most once per session
// (`identityProofSent` flag). Safe to call before the manager's client key is
// configured: we log and skip — without a client key the session is
// unauthenticated and the peer will (correctly) never grant us authentication,
// so the cipher never becomes observable and traffic stays plaintext.
func (self *peerEncryptionSession) sendIdentityProofOnce(e *tlsHandshakeEpoch) {
	exporter, payload, skipReason := func() ([]byte, []byte, string) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if e.identityProofSent {
			return nil, nil, "already sent"
		}
		if len(e.tlsExporter) == 0 {
			return nil, nil, "no tls exporter (handshake not done?)"
		}
		if self.manager == nil || self.manager.clientKeyManager == nil {
			return nil, nil, "no client key manager configured"
		}
		e.identityProofSent = true
		return e.tlsExporter, self.manager.clientKeyManager.Sign(e.tlsExporter), ""
	}()
	if exporter == nil || payload == nil {
		if self.client.log.V(1).Enabled() {
			self.client.log.Infof("[tls]%s sendIdentityProofOnce skipped: %s\n", self.logTag, skipReason)
		}
		return
	}
	if self.client.log.V(1).Enabled() {
		self.client.log.Infof(
			"[tls]%s sendIdentityProofOnce: signing %d-byte exporter, sending %d-byte proof\n",
			self.logTag, len(exporter), len(payload),
		)
	}
	self.sendEncryptedControl(&protocol.EncryptedControl{
		ControlType: protocol.EncryptedControlType_EncryptedControlIdentityProof,
		Payload:     payload,
		SessionRole: self.role.toProtobuf(),
		Companion:   self.companion,
	})
}

// maybeVerifyPendingPeerIdentityProof tries to verify a buffered peer
// identity-proof payload. The proof can arrive over the wire before either of
// its prerequisites: our TLS handshake completing (no `tlsExporter` yet) or the
// peer's public client key being set via `SetPeerClientPublicKey` (contract not
// delivered yet). Called from any path that could fill in the missing piece:
// `completeHandshake`, `DeliverEncryptedControl(IdentityProof, ...)`, and
// `SetPeerClientPublicKey`. Operates on the given epoch (the proof binds that
// epoch's exporter); a nil epoch is a no-op.
func (self *peerEncryptionSession) maybeVerifyPendingPeerIdentityProof(e *tlsHandshakeEpoch) {
	if e == nil {
		return
	}
	payload, exporter, peerPub, ready, skipReason := func() ([]byte, []byte, ed25519.PublicKey, bool, string) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if e.peerIdentityVerified {
			return nil, nil, nil, false, "already verified"
		}
		if e.identityFailed {
			return nil, nil, nil, false, "identity already marked failed"
		}
		if len(e.pendingPeerIdentityProof) == 0 {
			return nil, nil, nil, false, "no peer identity proof received yet"
		}
		if len(e.tlsExporter) == 0 {
			return nil, nil, nil, false, "tls exporter not derived (handshake not done)"
		}
		if len(self.peerClientPublicKey) != ed25519.PublicKeySize {
			return nil, nil, nil, false, "peer client public key not yet known"
		}
		// Snapshot under the lock; verification (ed25519 check) is
		// cheap but we don't want to hold the lock across it.
		return e.pendingPeerIdentityProof, e.tlsExporter, self.peerClientPublicKey, true, ""
	}()
	if !ready {
		if self.client.log.V(2).Enabled() {
			self.client.log.Infof("[tls]%s maybeVerifyPendingPeerIdentityProof skipped: %s\n", self.logTag, skipReason)
		}
		return
	}
	ok := VerifyClientKeySignature(peerPub, exporter, payload)
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		e.pendingPeerIdentityProof = nil
		if ok {
			e.peerIdentityVerified = true
			// Promote e to the established epoch only if it is still the
			// current one. A newer epoch (a restart that superseded e) must
			// win; establishing a stale epoch would serve an outdated cipher.
			if self.epoch == e {
				self.markEstablishedWithLock(e)
			}
		} else {
			e.identityFailed = true
		}
	}()
	if ok {
		if self.client.log.V(1).Enabled() {
			self.client.log.Infof("[tls]%s peer identity proof verified — cipher is now usable\n", self.logTag)
		}
	} else {
		self.client.log.Errorf(
			"[tls]%s peer identity proof FAILED (peer key %d bytes, exporter %d bytes, sig %d bytes) — session left unauthenticated\n",
			self.logTag, len(peerPub), len(exporter), len(payload),
		)
	}
}

// SetPeerClientPublicKey records the peer's long-lived Ed25519 public
// identity key for this session. Called from the SendSequence when a
// contract for the peer arrives (the contract carries the peer's
// public key in `destination_client_public_key`). Once set, the
// session can verify a buffered peer identity proof; if a proof
// already arrived, this triggers verification immediately.
//
// First-write-wins for the key value: a later call with a matching key is a
// no-op; one with a different key is logged and ignored (indicating either a
// bug in the platform's contract authoring or, more concerning, an MITM
// substituting different keys in different contracts to the same peer — either
// way, refusing to switch keys mid-session is the safe behavior).
func (self *peerEncryptionSession) SetPeerClientPublicKey(pub ed25519.PublicKey) {
	if len(pub) != ed25519.PublicKeySize {
		if self.client.log.V(1).Enabled() {
			self.client.log.Infof(
				"[tls]%s SetPeerClientPublicKey rejected: key length %d (expected %d)\n",
				self.logTag, len(pub), ed25519.PublicKeySize,
			)
		}
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
			self.client.log.Errorf(
				"[tls]%s peer client public key mismatch with prior commitment — refusing to rotate mid-session\n",
				self.logTag,
			)
		}
		return false
	}()
	if changed {
		if self.client.log.V(1).Enabled() {
			self.client.log.Infof("[tls]%s peer client public key set (first-time)\n", self.logTag)
		}
		self.maybeVerifyPendingPeerIdentityProof(self.currentEpoch())
		// Out-of-band cross-check: ask the per-session fetcher (typically the
		// platform's unauthenticated `/key/<peerId>` API) for this peer's public
		// key and log a loud warning if it disagrees with what the contract just
		// told us. Once per session (`changed` is true only on the first set)
		// and asynchronously, so the contract-processing path never blocks on an
		// HTTP round trip. Today we only log on mismatch; the contract-supplied
		// key is still trusted. The fetched key is compared and discarded, never
		// retained on the session.
		if self.peerClientPublicKeyFetcher != nil {
			contractPub := append(ed25519.PublicKey(nil), pub...)
			go HandleError(func() {
				self.crossCheckPeerClientPublicKey(contractPub)
			})
		}
	}
}

// crossCheckPeerClientPublicKey fetches the peer's public client key via the
// session's per-session fetcher (set from
// `EncryptionSettings.NewPeerClientPublicKeyFetcher` at session creation) and
// compares it against `contractPub` — the value the contract just delivered to
// `SetPeerClientPublicKey`. A mismatch is logged loudly: the platform is either
// (a) racing different contracts with inconsistent keys (data bug) or (b)
// substituting keys to mount a MITM (security bug). Today, log only — the
// contract-supplied key is still trusted for cert and identity-proof
// verification; a future iteration will harden this to refuse the substituted
// key. The fetched key lives only on the goroutine stack: not stored on the
// session, not cached by the manager.
func (self *peerEncryptionSession) crossCheckPeerClientPublicKey(contractPub ed25519.PublicKey) {
	fetched, err := self.peerClientPublicKeyFetcher(self.ctx)
	if err != nil {
		if self.client.log.V(1).Enabled() {
			self.client.log.Infof("[key]%s peer public-key fetch failed: %s\n", self.logTag, err)
		}
		return
	}
	if len(fetched) == 0 {
		if self.client.log.V(1).Enabled() {
			self.client.log.Infof("[key]%s peer public-key fetch returned no key (peer has not published yet)\n", self.logTag)
		}
		return
	}
	if len(fetched) != ed25519.PublicKeySize {
		self.client.log.Errorf(
			"[key]%s peer public-key fetch returned %d bytes (expected %d)\n",
			self.logTag, len(fetched), ed25519.PublicKeySize,
		)
		return
	}
	if !bytes.Equal(fetched, contractPub) {
		self.client.log.Errorf(
			"[key]%s CONTRACT vs FETCHED peer client public key MISMATCH for %s — possible platform MITM (today: log only, contract value still trusted)\n",
			self.logTag, self.peerId,
		)
		return
	}
	if self.client.log.V(1).Enabled() {
		self.client.log.Infof("[key]%s peer public-key cross-check OK\n", self.logTag)
	}
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
//   - `EncryptedControlHandshake`: raw TLS handshake bytes, fed into the
//     per-peer TLS transport for the state machine to process. `Client.run`
//     may have already applied this same payload via
//     `OptimisticallyDeliverHandshake` (for bytes arriving in the
//     awaiting-client-Finished window) — if so, the handshake has completed by
//     now and the state machine no longer reads from `transport.inbox`, so
//     re-delivering would just enlarge the inbox to no purpose. Skip when
//     `handshakeDone` is already closed.
//
//   - `EncryptedControlIdentityProof`: the peer's Ed25519 signature over the
//     TLS exporter output, made with its long-lived client identity key.
//     Verified against the peer's public client key (set via
//     `SetPeerClientPublicKey` from contract data). Verifies immediately if
//     both the peer key and our exporter are available; otherwise buffered and
//     retried as those become available. Success flips `peerIdentityVerified`,
//     at which point `Cipher()` exposes the AEAD; failure flips `identityFailed`
//     (sticky), permanently keeping the cipher hidden so the session stays
//     plaintext.
func (self *peerEncryptionSession) DeliverEncryptedControl(ec *protocol.EncryptedControl) {
	switch ec.ControlType {
	case protocol.EncryptedControlType_EncryptedControlHandshake:
		self.deliverHandshake(ec.Payload)
	case protocol.EncryptedControlType_EncryptedControlIdentityProof:
		if self.client.log.V(1).Enabled() {
			self.client.log.Infof(
				"[tls]%s DeliverEncryptedControl(IdentityProof): received %d-byte proof\n",
				self.logTag, len(ec.Payload),
			)
		}
		self.receivePeerIdentityProof(ec.Payload)
	default:
		if self.client.log.V(1).Enabled() {
			self.client.log.Infof(
				"[tls]%s DeliverEncryptedControl: unknown control type %v (%d bytes)\n",
				self.logTag, ec.ControlType, len(ec.Payload),
			)
		}
	}
}

// deliverHandshake feeds inbound TLS handshake bytes into the current
// epoch's transport.
//
// A fresh ClientHello arriving at the TLS-server role after the current epoch
// has finished means the peer (the TLS-client) restarted the handshake —
// typically because a client-role SendSequence resumed and reset its side. We
// swap in a fresh epoch and feed the ClientHello to it, re-handshaking on the
// existing (still-referenced) session rather than dropping the bytes (point b).
// A ClientHello arriving mid-handshake is the initial or HelloRetryRequest
// ClientHello for the in-flight epoch and is fed as-is.
//
// Non-ClientHello handshake bytes arriving after the handshake completed are
// stale post-completion records; dropped rather than re-fed to a closed TLS
// state machine.
func (self *peerEncryptionSession) deliverHandshake(payload []byte) {
	if self.role == sequenceTlsRoleServer && isClientHelloRecord(payload) {
		if e := self.currentEpoch(); e == nil || isClosed(e.handshakeDone) {
			if self.client.log.V(1).Enabled() {
				self.client.log.Infof(
					"[tls]%s inbound ClientHello after prior handshake — resetting to a new handshake\n",
					self.logTag,
				)
			}
			self.reset()
		}
	} else if e := self.currentEpoch(); e != nil && isClosed(e.handshakeDone) {
		if self.client.log.V(2).Enabled() {
			self.client.log.Infof(
				"[tls]%s DeliverEncryptedControl(Handshake) %d bytes — skipped, handshake already done\n",
				self.logTag, len(payload),
			)
		}
		return
	}
	self.startEpoch()
	e := self.currentEpoch()
	if e == nil || e.transport == nil {
		return
	}
	var firstByte byte
	if 0 < len(payload) {
		firstByte = payload[0]
	}
	if self.client.log.V(1).Enabled() {
		self.client.log.Infof(
			"[tls]%s DeliverEncryptedControl(Handshake): feeding %d bytes (record type 0x%02x) to TLS state\n",
			self.logTag, len(payload), firstByte,
		)
	}
	e.transport.Deliver(payload)
}

// receivePeerIdentityProof buffers the peer's identity-proof payload
// and attempts verification. If the peer key and our TLS exporter are
// both already known, verification runs immediately. Otherwise the
// payload sits in `pendingPeerIdentityProof` and gets retried by
// `maybeVerifyPendingPeerIdentityProof` whenever one of the missing
// pieces becomes available (TLS handshake completes;
// `SetPeerClientPublicKey` is called).
//
// A second proof arriving while one is already buffered, or after a definitive
// verification (success or failure), is ignored: a session has at most one
// identity-proof exchange, and a second proof is either a benign retransmit or
// a manipulation attempt — either way, refusing to re-evaluate is safe.
func (self *peerEncryptionSession) receivePeerIdentityProof(payload []byte) {
	// The proof binds the current epoch's exporter; ensure an epoch exists to
	// buffer it against (a proof can race ahead of our handshake completing).
	self.startEpoch()
	e := self.currentEpoch()
	if e == nil {
		return
	}
	stored, skipReason := func() (bool, string) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if e.peerIdentityVerified {
			return false, "already verified"
		}
		if e.identityFailed {
			return false, "identity already failed"
		}
		if len(e.pendingPeerIdentityProof) != 0 {
			return false, "another proof already buffered"
		}
		if len(payload) != ed25519.SignatureSize {
			// invalid shape; record as failed to keep the session
			// from ever exposing its cipher.
			e.identityFailed = true
			return false, fmt.Sprintf("malformed sig length %d (expected %d)", len(payload), ed25519.SignatureSize)
		}
		e.pendingPeerIdentityProof = append([]byte(nil), payload...)
		return true, ""
	}()
	if !stored {
		// already have a proof in flight, in a final identity state, or marked
		// failure because the payload was malformed.
		if self.client.log.V(1).Enabled() {
			self.client.log.Infof("[tls]%s receivePeerIdentityProof skipped: %s\n", self.logTag, skipReason)
		}
		return
	}
	if self.client.log.V(1).Enabled() {
		self.client.log.Infof("[tls]%s receivePeerIdentityProof buffered %d-byte proof — attempting verify\n", self.logTag, len(payload))
	}
	self.maybeVerifyPendingPeerIdentityProof(e)
}

// OptimisticallyDeliverHandshake feeds handshake payload bytes directly
// into the per-peer TLS transport from `Client.run` — bypassing the
// ReceiveSequence drain that would otherwise reach the session via
// `DeliverEncryptedControl`. The intent is to set the cipher in the same
// receive-loop turn that processed the inbound EncryptedControl frame, so any
// wrapped app-data frames in the immediately-following reads find the cipher
// already set rather than being dropped.
//
// Gated on `IsAwaitingClientFinished` so it's a no-op outside the narrow
// TLS-server window where it pays off (and where the just-arrived EC frame is,
// by construction, the expected client second flight). Once the handshake
// completes, `IsAwaitingClientFinished` returns false and subsequent calls do
// nothing.
//
// Called from the single-threaded receive loop, so it must not block:
// `transport.Deliver` just appends to the inbox under a quick lock and notifies
// the runHandshake goroutine to wake and read.
//
// The normal-path `DeliverEncryptedControl` still sees this EC frame when the
// ReceiveSequence drains it; by then the handshake is (almost always) complete
// and the redelivery short-circuits via the `handshakeDone` check. The tiny
// race window where both paths deliver the same bytes just leaves a few KB in
// the inbox — bounded by the client second flight size.
//
// Retransmit filter: the transfer layer already validates the pack source
// (every hop verifies source), so the only way stale handshake bytes reach us
// is a sender-side resend of an earlier handshake message — practically, a
// ClientHello retransmit from before our server flight went out (the sender's
// resend timer for the ClientHello pack fired before our ack got back). We
// can't dedupe by (sequenceId, sequenceNumber) here without reaching into the
// ReceiveSequence's state, so we use a one-byte structural check on the TLS
// record header. In TLS 1.3 the legitimate client second flight starts with
// either:
//   - record type 20 (legacy `ChangeCipherSpec`, sent for middlebox
//     compatibility), or
//   - record type 23 (encrypted `application_data`, how post-handshake-secrets
//     handshake messages are wrapped).
//
// A ClientHello (initial or HRR retry) is record type 22 — caught by this
// filter. Anything else (unencrypted alert at type 21, etc.) is not a
// legitimate client-second-flight prefix either. Skip and let the
// ReceiveSequence's normal dedupe handle the retransmit (it short-circuits if
// duplicate, applies correctly if new — its sequence-number bookkeeping is what
// makes feeding bytes to the TLS state machine safe).
func (self *peerEncryptionSession) OptimisticallyDeliverHandshake(payload []byte) {
	if !self.IsAwaitingClientFinished() {
		if self.client.log.V(2).Enabled() {
			self.client.log.Infof(
				"[tls]%s OptimisticallyDeliverHandshake skipped: not awaiting client Finished\n",
				self.logTag,
			)
		}
		return
	}
	// isClientSecondFlightPrefix: the first byte of `payload` is a TLS 1.3
	// record content type that can start a legitimate client second flight: 20
	// (legacy ChangeCipherSpec) or 23 (encrypted application_data carrying
	// handshake messages). Type 22 (unencrypted Handshake) is rejected — that's
	// ClientHello and HRR retry ClientHello, the messages a sender retransmits
	// before our server flight reached them. Type 21 (Alert) is rejected too —
	// alerts after handshake-secrets are derived are encrypted under type 23.
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
		if self.client.log.V(1).Enabled() {
			self.client.log.Infof(
				"[tls]%s OptimisticallyDeliverHandshake filtered out: %d bytes, first byte 0x%02x (not a client-second-flight prefix)\n",
				self.logTag, len(payload), firstByte,
			)
		}
		return
	}
	if self.client.log.V(1).Enabled() {
		self.client.log.Infof(
			"[tls]%s OptimisticallyDeliverHandshake: feeding %d bytes (record type 0x%02x) to TLS state\n",
			self.logTag, len(payload), payload[0],
		)
	}
	// Only complete an already in-flight handshake; never create or restart an
	// epoch from the optimistic path. This runs on every EC frame the receive
	// loop sees — including stale, reordered, and retransmitted ones — so it
	// must not mutate epoch lifecycle state. IsAwaitingClientFinished already
	// established that a current in-flight epoch (server flight sent, handshake
	// not yet done) exists; deliver the client second flight to it and nothing
	// more.
	if e := self.currentEpoch(); e != nil && e.transport != nil && !isClosed(e.handshakeDone) {
		e.transport.Deliver(payload)
	}
}

// Cipher returns the AEAD cipher, or nil if the session can't safely
// wrap/unwrap application frames. Nil means "treat as plaintext for this peer."
// The AEAD is withheld until both:
//   - the TLS handshake has completed successfully (cipher derived),
//   - the peer's identity proof has been verified against the peer's
//     long-lived public client key.
//
// If either prerequisite fails — TLS handshake errors out, identity proof
// verifies to the wrong signature, or never arrives — the cipher stays nil for
// the session's lifetime and traffic flows in plaintext. This matches the
// "cipher is a binary property of the session" wire semantics (see
// `writeMaybeWrappedBytes`): peers that can't mutually authenticate fall back
// to plaintext rather than encrypted-but-unauthenticated.
func (self *peerEncryptionSession) Cipher() *sequenceCipher {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.establishedEpoch == nil {
		if self.client.log.V(2).Enabled() {
			// V(2) only (called per send): trace the specific reason the
			// cipher isn't available so the caller can correlate plaintext
			// writes to the underlying gate.
			reason := "no established epoch"
			if e := self.epoch; e != nil {
				reason = "tls handshake not done"
				if e.derivedTlsCipher != nil {
					if e.identityFailed {
						reason = "identity proof verification FAILED"
					} else if len(self.peerClientPublicKey) == 0 {
						reason = "peer client public key not yet known"
					} else if len(e.pendingPeerIdentityProof) == 0 {
						reason = "peer identity proof not yet received"
					} else {
						reason = "identity verification pending"
					}
				}
			}
			self.client.log.V(2).Infof("[tls]%s Cipher()=nil: %s\n", self.logTag, reason)
		}
		return nil
	}
	// A send must not wrap under the previous (established) epoch while a new
	// handshake is in flight — fall back to plaintext until the new epoch
	// establishes. The receiver keeps the previous epoch in `decryptCiphers`, so
	// frames already on the wire under it stay decryptable; this asymmetry
	// (sends drop the old epoch on a restart, receives keep it) lets a rekey
	// proceed without the sender wrapping under an epoch the peer may have
	// already moved past. In particular the contract-open ride-along is sent in
	// the clear during a restart, so the contract always opens.
	if self.handshakeInFlightLocked() {
		if self.client.log.V(2).Enabled() {
			self.client.log.Infof("[tls]%s Cipher()=nil: new handshake in flight\n", self.logTag)
		}
		return nil
	}
	return self.establishedEpoch.derivedTlsCipher
}

// decryptCiphers returns the candidate ciphers for unwrapping an inbound
// frame: the established cipher and, briefly after a rekey, the prior
// established cipher (a peer may still send under the old key until it swaps
// to the new one). The unwrap path tries each until one authenticates. Empty
// until the session establishes.
func (self *peerEncryptionSession) decryptCiphers() []*sequenceCipher {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	var out []*sequenceCipher
	if self.establishedEpoch != nil && self.establishedEpoch.derivedTlsCipher != nil {
		out = append(out, self.establishedEpoch.derivedTlsCipher)
	}
	if self.priorEstablishedEpoch != nil && self.priorEstablishedEpoch.derivedTlsCipher != nil {
		out = append(out, self.priorEstablishedEpoch.derivedTlsCipher)
	}
	return out
}

// markEstablishedWithLock promotes epoch e to the established epoch — its
// cipher now serves wrap (`Cipher()`) and decrypt. The previously
// established epoch is demoted to priorEstablishedEpoch, kept for decrypt
// during the brief swap window; an even older prior is cancelled. Caller
// holds stateLock.
func (self *peerEncryptionSession) markEstablishedWithLock(e *tlsHandshakeEpoch) {
	if self.establishedEpoch == e {
		return
	}
	if self.priorEstablishedEpoch != nil && self.priorEstablishedEpoch != e {
		self.priorEstablishedEpoch.cancel()
	}
	self.priorEstablishedEpoch = self.establishedEpoch
	self.establishedEpoch = e
}

// IsAwaitingClientFinished reports whether this session is in the narrow
// TLS-server-role window after sending its own Finished and before receiving
// the peer's — the only state in which it's meaningful for the unwrap path to
// briefly block the receive loop waiting for the cipher.
//
// Returns false:
//   - in TLS-client role (the local cipher is set first; a wrapped frame
//     arriving in that direction is the peer racing ahead with its own cipher,
//     not anything we can resolve by waiting locally — drop and let the sender
//     resend);
//   - before this side has produced its server flight (no ClientHello processed
//     yet, so blocking can't resolve in any reasonable window; also the natural
//     state of a freshly-created session with no handshake input — keeping wait
//     off here closes the DoS surface where an attacker creates a session
//     reference and then stalls);
//   - once the handshake has completed (success: cipher already non-nil;
//     failure: cipher will never be set and waiting is pointless).
//
// Keeping this predicate tight bounds how long the single-threaded `Client.run`
// receive loop can be parked by any one wrapped frame.
func (self *peerEncryptionSession) IsAwaitingClientFinished() bool {
	if self.role != sequenceTlsRoleServer {
		return false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	e := self.epoch
	if e == nil {
		return false
	}
	select {
	case <-e.handshakeDone:
		return false
	default:
	}
	return e.serverFlightSent
}

func (self *peerEncryptionSession) closeTls() {
	if e := self.currentEpoch(); e != nil && e.transport != nil {
		e.transport.Close()
	}
	// Deliberately do not call tlsConn.CloseWrite(): it emits a TLS close_notify
	// alert the outbox loop would ship as an outbound EncryptedControl, and a
	// closing session must not emit any send messages. There is no
	// graceful-shutdown handshake — a later SendSequence resets the session and
	// starts a fresh handshake instead.
}

// PeerCertificates returns the peer's certificate chain observed during
// the current epoch's TLS handshake. Returns nil if the handshake has not
// completed.
func (self *peerEncryptionSession) PeerCertificates() []*x509.Certificate {
	return peerCertificatesOfEpoch(self.client.log, self.currentEpoch())
}

// establishedPeerCertificates returns the peer certificate chain from the
// established epoch — the epoch whose cipher `Cipher()` hands out and which
// therefore seals outbound frames. That is the identity a SendSequence must
// verify against the contract commitment.
//
// Unlike `currentEpoch()` — which during a re-handshake is an in-flight epoch
// whose `tls.Conn.ConnectionState()` blocks on the handshake mutex — the
// established epoch's handshake is by definition complete (it is promoted only
// after the handshake finished and the identity proof verified, see
// markEstablishedWithLock), so reading its peer certificates never blocks.
// Verifying the in-flight epoch here was the source of a deadlock: the
// SendSequence Run loop parked on the in-flight handshake's mutex while that
// same handshake's ClientHello waited to be packed onto the very same
// (unbuffered) send sequence.
//
// Returns nil if no epoch has established yet; in practice verification is only
// reached once `Cipher()` is non-nil, which implies an established epoch.
func (self *peerEncryptionSession) establishedPeerCertificates() []*x509.Certificate {
	self.stateLock.Lock()
	e := self.establishedEpoch
	self.stateLock.Unlock()
	return peerCertificatesOfEpoch(self.client.log, e)
}

// CertVerificationState returns the cached certificate verification result.
// `verified` is true once a peer cert was matched to the trusted set, and is
// sticky for the session lifetime. `noCommitment` is true exactly when the
// trusted set is empty, so callers can distinguish "no commitment held yet"
// (verification should skip without latching) from "have a commitment not yet
// checked" (verification should run). Not sticky: if an empty-chain contract is
// later followed by one with a cert, noCommitment flips off and verification
// runs against the new commitment.
func (self *peerEncryptionSession) CertVerificationState() (verified bool, noCommitment bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	verified = self.epoch != nil && self.epoch.certVerified
	return verified, len(self.trustedPeerCertPems) == 0
}

// MarkCertVerified records that the peer's TLS-handshake cert was matched
// against the trusted set. Sticky for the current epoch; a re-handshake
// (new epoch) re-verifies against the peer's freshly presented cert.
func (self *peerEncryptionSession) MarkCertVerified() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.epoch != nil {
		self.epoch.certVerified = true
	}
}

// AddTrustedPeerCertChain extends the session's trusted peer cert set with
// `chain` (PEM-encoded, leaf first), gated on a signature check. `chainSig`
// must be the peer's Ed25519 signature over the canonical encoding of `chain`
// (concatenation of PEM blocks), made under the peer's long-lived client
// identity key. The verifier is the session's currently-known peer public
// client key (`peerClientPublicKey`, set via `SetPeerClientPublicKey`).
//
// All four states are handled:
//   - empty chain → no-op (no commitment to add).
//   - non-empty chain + no peer key yet → no-op; the chain is dropped (the
//     commitment is unverifiable until the peer key is delivered). A later
//     contract with both pieces succeeds.
//   - non-empty chain + peer key known + signature absent → no-op, log a
//     warning. Pre-identity-rollout peers may not carry sigs yet; once they do,
//     the cert is admitted.
//   - non-empty chain + peer key known + signature verifies → admit all PEM
//     blocks into `trustedPeerCertPems`.
//   - non-empty chain + peer key known + signature fails → log and drop.
//     Signals a substitution attempt (platform-authored MITM) or a misbehaving
//     peer; either way we refuse to extend trust to this cert.
//
// Skipping (rather than latching failure) on the "no-op" cases is deliberate: a
// single missing piece doesn't poison the session, and a later contract that
// has everything still works. Hard-failure is reserved for an actively-bad
// signature.
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
		if self.client.log.V(1).Enabled() {
			self.client.log.Infof(
				"[tls]%s contract cert chain dropped: peer client public key not yet known\n",
				self.logTag,
			)
		}
		return
	}
	if len(chainSig) == 0 {
		self.client.log.Errorf(
			"[tls]%s contract cert chain dropped: no client-key signature provided\n",
			self.logTag,
		)
		return
	}
	if !VerifyCertChainSignature(peerPub, chain, chainSig) {
		self.client.log.Errorf(
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

// Release drops one reference. When the count reaches zero the session is
// closed and unregistered immediately: a per-peer session exists only while
// some send or receive sequence is using it, and those sequences idle out on
// their own timers, so they are the session's lifecycle. A later sequence
// re-creates the session (and, for the client role, re-runs the handshake) on
// demand. The map delete happens here under the manager lock, atomically with
// the ref reaching zero, so a concurrent `acquireSession` can never adopt a
// session being torn down (the revival race that produced "no encryption
// session for peer").
func (self *peerEncryptionSession) Release() {
	if self == nil {
		return
	}
	free := func() bool {
		self.manager.stateLock.Lock()
		defer self.manager.stateLock.Unlock()

		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.refs--
		self.lastActivityTime = time.Now()
		if 0 < self.refs {
			return false
		}
		// Keep the session alive while a handshake is in flight: tearing it down
		// cancels the epoch ctx and aborts the handshake. Leaving it registered
		// is the important part — the next send reuses it instead of creating a
		// fresh session, which the peer would see as a brand new ClientHello and
		// reset to, churning the handshake forever.
		if self.handshakeInFlightLocked() {
			return false
		}
		// Established/idle with no refs. IdleTimeout semantics:
		//   < 0  keep the session in memory forever (never idle-reap),
		//   > 0  keep it registered; the Run loop's CancelIfIdle reaps it once
		//        it has been idle (unreferenced) for IdleTimeout,
		//   == 0 reap immediately here at refs==0.
		// Keeping it registered (the non-zero cases) lets the next burst reuse the
		// live cipher instead of churning a fresh handshake, and avoids the two
		// peers reaping independently and desyncing.
		idleTimeout := time.Duration(0)
		if self.settings != nil {
			idleTimeout = self.settings.IdleTimeout
		}
		if idleTimeout != 0 {
			return false
		}
		key := sessionKey{peerId: self.peerId, companion: self.companion, role: self.role}
		if self.manager.sessions[key] == self {
			delete(self.manager.sessions, key)
		}
		return true
	}()
	if free {
		self.close()
	}
}

// handshakeInFlightLocked reports whether the current epoch is mid-handshake —
// not yet established and not yet failed (TLS error or identity failure). The
// session must not be torn down in this state. Caller holds stateLock.
func (self *peerEncryptionSession) handshakeInFlightLocked() bool {
	e := self.epoch
	return e != nil && e != self.establishedEpoch && e.handshakeErr == nil && !e.identityFailed
}

// CancelIfIdle reaps the session if it has no references, no in-flight
// handshake, and has been idle (unreferenced) for at least
// EncryptionSettings.IdleTimeout. Called periodically from Run, mirroring the
// resident idle poll in server/connect/resident.go. Returns true if the session
// was reaped or was already cancelled.
//
// The map delete happens under the manager lock, atomically with the refs/idle
// check, so a concurrent acquireSession can never adopt a session being torn
// down (the revival race that produced "no encryption session for peer").
func (self *peerEncryptionSession) CancelIfIdle() bool {
	if self == nil || self.manager == nil {
		return true
	}
	select {
	case <-self.ctx.Done():
		return true
	default:
	}
	idleTimeout := time.Duration(0)
	if self.settings != nil {
		idleTimeout = self.settings.IdleTimeout
	}

	// Hold the manager lock across the entire refs/idle check and the map delete
	// (mirroring Release), so a concurrent acquireSession can't read this session
	// out of the map and retain it while it is being torn down, and so the delete
	// doesn't race acquireSession's getOrCreateWithLock on the sessions map. The
	// prior `Lock(); Unlock()` released the manager lock immediately, leaving the
	// delete guarded only by the per-session lock — a concurrent map write. close()
	// runs after the manager lock is dropped.
	free := func() bool {
		self.manager.stateLock.Lock()
		defer self.manager.stateLock.Unlock()

		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if 0 < self.refs {
			return false
		}
		if self.handshakeInFlightLocked() {
			return false
		}
		if 0 < idleTimeout && time.Now().Sub(self.lastActivityTime) < idleTimeout {
			return false
		}
		key := sessionKey{peerId: self.peerId, companion: self.companion, role: self.role}
		if self.manager.sessions[key] == self {
			delete(self.manager.sessions, key)
		}
		return true
	}()
	if free {
		self.close()
	}
	return free
}

// retain bumps the reference count and marks activity. Always called by the
// manager under its stateLock (in `acquireSession`), so the get-or-create and
// the retain are atomic — closing the revival race where a session could be
// torn down between being handed out and retained. Marking activity here (with
// Release) is what makes the next burst's acquire reset the idle clock, so a
// reused session is never reaped out from under it.
func (self *peerEncryptionSession) retain() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.refs++
	self.lastActivityTime = time.Now()
}

// close cancels the session context and tears down TLS state. Idempotent.
// Cancelling the session ctx cascades to every epoch's child ctx, so their
// goroutines exit. The session's `Run` goroutine observes ctx.Done() and
// returns; its supervisor in the manager then unregisters the session (a
// no-op if Release already did). A closing session emits nothing on the
// wire — there is no explicit TLS close. A later SendSequence that
// re-acquires the peer starts a fresh session and handshake.
func (self *peerEncryptionSession) close() {
	self.cancel()
	self.closeTls()
}

// EncryptionSessionManager owns per-peer TLS sessions for the local Client.
// Sessions are keyed by `(peerId, companion, role)` (see the file header) —
// independent of transport, route, stream, or contract — so a client can hold
// up to four sessions per peer. Send/receive sequences of a given
// (companion, role) share that session and its cipher; a session lives only
// while some sequence references it (the sequences idle out on their own
// timers, so they are the session's lifecycle).
type EncryptionSessionManager struct {
	ctx              context.Context
	client           *Client
	clientKeyManager *ClientKeyManager
	settings         *EncryptionSettings

	// `serverTlsConfig` is the receiver-role TLS config used by every per-peer
	// session this manager owns. It carries one persistent X.509 certificate
	// (`selfCertPem` below) — the local client's public identity for all
	// sequence-level TLS sessions, independent of provide mode or peer. The cert
	// is generated at construction (or inherited from `settings.ServerTlsConfig`
	// if the caller supplied one) and republished to the platform via
	// `EncryptedKey` so contracts whose destination is this client carry it as a
	// commitment.
	serverTlsConfig *tls.Config
	clientTlsConfig *tls.Config
	// PEM-encoded chain of the local cert (leaf first). Published via
	// `EncryptedKey`. Empty when encryption is disabled.
	selfCertPem [][]byte
	// PEM-encoded private key (PKCS#8, single block) matching the leaf cert in
	// `selfCertPem`. Held so persistence callers can read the matched pair back
	// through `ProvideTlsCertificatePem()` / `ProvideTlsPrivateKeyPem()` and load
	// it on a subsequent run. Empty when encryption is disabled or when the
	// caller provided a `tls.Config` whose certificate has no exposed private
	// key (e.g. a `GetCertificate` callback).
	selfPrivateKeyPem []byte

	controlSyncEncryptedKey *ControlSync

	stateLock sync.Mutex
	sessions  map[sessionKey]*peerEncryptionSession
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
		sessions:         map[sessionKey]*peerEncryptionSession{},
	}
	if settings.Encrypt {
		serverTlsConfig, selfCertPem, selfPrivateKeyPem, err := resolveReceiveTlsConfig(
			settings.ServerTlsConfig,
			settings.ProvideTlsCertificatePem,
			settings.ProvideTlsPrivateKeyPem,
		)
		if err != nil {
			client.log.Errorf("[tls]%s could not initialize server TLS config: %s\n", client.ClientTag(), err)
		} else {
			m.serverTlsConfig = serverTlsConfig
			m.selfCertPem = selfCertPem
			m.selfPrivateKeyPem = selfPrivateKeyPem
		}
		m.clientTlsConfig = resolveSendTlsConfig(settings.ClientTlsConfig)
		// Mutual TLS: the TLS-client side presents the same self cert the
		// TLS-server side does, so the peer (acting as TLS-server for this
		// session) can hand the cert to its sequence-level verifier. Roles are
		// lex-assigned on ClientId, so verification must work in either
		// direction. Preserve any Certificates the caller's ClientTlsConfig
		// already carries; only fill in from the manager's self cert.
		if m.clientTlsConfig != nil && len(m.clientTlsConfig.Certificates) == 0 &&
			m.serverTlsConfig != nil && 0 < len(m.serverTlsConfig.Certificates) {
			m.clientTlsConfig.Certificates = append(
				[]tls.Certificate(nil),
				m.serverTlsConfig.Certificates...,
			)
		}
		m.controlSyncEncryptedKey = NewControlSync(ctx, client, "encrypted-key")
		// Publish the cert so every contract whose destination is this client
		// carries it. ControlSync retries until the platform acks. The publisher
		// waits on `client.ReadyNotify()` before its first send: this constructor
		// runs inside `NewClientWithTag`, before `initBuffers` wires up
		// `sendBuffer`, so sending immediately would race (or precede) the buffer
		// construction.
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
// Started as a goroutine from `NewEncryptionSessionManager`. Waits on the
// client's `ReadyNotify()` before its first send so the client is fully wired
// (especially `sendBuffer`) before any control traffic leaves; without this
// gate the send path can race the buffer wiring in `Client.initBuffers`.
func (self *EncryptionSessionManager) publishEncryptedKey() {
	if self.controlSyncEncryptedKey == nil {
		return
	}
	select {
	case <-self.client.ReadyNotify():
	case <-self.ctx.Done():
		return
	}

	selfCertPem := self.SelfCertPem()

	// Sign the cert chain with our long-lived client identity key. Senders that
	// fetch our public client key out-of-band can then verify the cert chain the
	// platform attached to a contract was authentically committed by us —
	// substituting the cert without forging this signature requires our private
	// key.
	var certSig []byte
	if self.clientKeyManager != nil && 0 < len(selfCertPem) {
		certSig = self.clientKeyManager.SignCertChain(selfCertPem)
	}
	frame, err := ToFrame(&protocol.EncryptedKey{
		ProvideTlsCertificate:         selfCertPem,
		ClientKeySignedTlsCertificate: certSig,
	}, self.client.settings.ProtocolVersion)
	if err != nil {
		self.client.log.Errorf("[tls]%s could not build EncryptedKey frame: %s\n", self.client.ClientTag(), err)
		return
	}
	self.controlSyncEncryptedKey.Send(frame, nil, nil)
}

// SelfCertPem returns the PEM-encoded chain of the local cert (leaf first)
// that this manager publishes via `EncryptedKey`. Returns nil when
// encryption is disabled or cert setup failed.
func (self *EncryptionSessionManager) SelfCertPem() [][]byte {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return cloneByteSlices(self.selfCertPem)
}

// ProvideTlsCertificatePem returns the local TLS cert chain as a single
// concatenated PEM byte slice (one or more `-----BEGIN CERTIFICATE-----`
// blocks, leaf first). Pair with `ProvideTlsPrivateKeyPem` to persist and
// reload the cert across restarts via
// `EncryptionSettings.ProvideTlsCertificatePem` / `ProvideTlsPrivateKeyPem`.
// Returns nil when encryption is disabled or cert setup failed.
func (self *EncryptionSessionManager) ProvideTlsCertificatePem() []byte {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

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
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return bytes.Clone(self.selfPrivateKeyPem)
}

// ServerTlsConfig returns the TLS config used by every per-peer session
// this manager owns when it takes the TLS-server role.
func (self *EncryptionSessionManager) ServerTlsConfig() *tls.Config {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.serverTlsConfig == nil {
		return nil
	}
	return self.serverTlsConfig.Clone()
}

// ClientTlsConfig returns the TLS config used by every per-peer session
// this manager owns when it takes the TLS-client role.
func (self *EncryptionSessionManager) ClientTlsConfig() *tls.Config {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.clientTlsConfig == nil {
		return nil
	}
	return self.clientTlsConfig.Clone()
}

// sessionTlsConfigWithLock snapshots the TLS config a new session in `role`
// should present, from the manager's current key material. Caller holds
// stateLock. Mirrors the prior on-demand ClientTlsConfig/ServerTlsConfig
// resolution, but taken once at session creation so the handshake-build path
// never reaches back into the manager (see `peerEncryptionSession.tlsConfig`).
// A later `SetProvideTlsKeyMaterial` rotation swaps the manager's configs and so
// is picked up by sessions created afterward; live sessions keep the identity
// they were created with. Server role returns nil when no cert is configured —
// the misconfigured case the epoch build fails closed on.
func (self *EncryptionSessionManager) sessionTlsConfigWithLock(role sequenceTlsRole) *tls.Config {
	switch role {
	case sequenceTlsRoleClient:
		if self.clientTlsConfig != nil {
			return self.clientTlsConfig.Clone()
		}
		return resolveSendTlsConfig(self.settings.ClientTlsConfig)
	case sequenceTlsRoleServer:
		if self.serverTlsConfig != nil {
			return self.serverTlsConfig.Clone()
		}
	}
	return nil
}

// SetProvideTlsKeyMaterial replaces the local sequence-level TLS identity
// and republishes the EncryptedKey commitment.
func (self *EncryptionSessionManager) SetProvideTlsKeyMaterial(certPem []byte, keyPem []byte) error {
	if !self.settings.Encrypt {
		return nil
	}
	if len(certPem) == 0 && len(keyPem) == 0 {
		return nil
	}
	if len(certPem) == 0 || len(keyPem) == 0 {
		return fmt.Errorf("provide TLS certificate and private key must both be set")
	}

	serverTlsConfig, selfCertPem, selfPrivateKeyPem, err := resolveReceiveTlsConfig(
		self.settings.ServerTlsConfig,
		certPem,
		keyPem,
	)
	if err != nil {
		return err
	}

	clientTlsConfig := resolveSendTlsConfig(self.settings.ClientTlsConfig)
	if clientTlsConfig != nil && len(clientTlsConfig.Certificates) == 0 &&
		serverTlsConfig != nil && 0 < len(serverTlsConfig.Certificates) {
		clientTlsConfig.Certificates = append(
			[]tls.Certificate(nil),
			serverTlsConfig.Certificates...,
		)
	}

	self.stateLock.Lock()
	self.serverTlsConfig = serverTlsConfig
	self.clientTlsConfig = clientTlsConfig
	self.selfCertPem = selfCertPem
	self.selfPrivateKeyPem = bytes.Clone(selfPrivateKeyPem)
	self.stateLock.Unlock()

	if 0 < len(selfCertPem) {
		go HandleError(self.publishEncryptedKey)
	}
	return nil
}

func (self *EncryptionSessionManager) Settings() *EncryptionSettings {
	return self.settings
}

// SendNoSession reports whether a SendSequence to `destinationId` should run
// without a per-peer encryption session — i.e. in plaintext, even when
// `Encrypt` is true. This is the encryption analog of
// `ContractManager.SendNoContract`: control-plane traffic is never encrypted. A
// session is suppressed when either endpoint is the control/platform client —
// this local client (`client.ClientId() == ControlId`) or the destination
// (`destinationId == ControlId`). `NewSendSequence` is the hook point: when this
// returns true it acquires no session, so writes flow unwrapped.
func (self *EncryptionSessionManager) SendNoSession(destinationId Id) bool {
	return self.client.ClientId() == ControlId || destinationId == ControlId
}

// ReceiveNoSession reports whether a ReceiveSequence from `sourceId` should
// accept data without a per-peer encryption session — a session is neither
// created nor required to receive. This is the encryption analog of
// `ContractManager.ReceiveNoContract`: control-plane traffic is never
// encrypted. A session is suppressed when either endpoint is the control
// client — this local client (`client.ClientId() == ControlId`) or the source
// (`sourceId == ControlId`). `NewReceiveSequence` is the hook point: when this
// returns true it acquires no session, and inbound frames from the source are
// taken in plaintext via the receive path's dual wrapped/unwrapped handling.
func (self *EncryptionSessionManager) ReceiveNoSession(sourceId Id) bool {
	return self.client.ClientId() == ControlId || sourceId == ControlId
}

// acquireSession returns the ref-incremented (peerId, companion, role)
// session, creating it if needed, without starting a handshake. Returns nil
// when encryption is disabled or peerId is the zero Id (control destinations).
// The get-or-create and the retain run under the manager lock so a
// concurrent `Release` can't tear the session down between the two — the
// revival race that produced "no encryption session for peer".
func (self *EncryptionSessionManager) acquireSession(peerId Id, role sequenceTlsRole, companion bool) *peerEncryptionSession {
	if !self.settings.Encrypt {
		return nil
	}
	if (peerId == Id{}) {
		return nil
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	session := self.getOrCreateWithLock(peerId, role, companion)
	session.retain()
	return session
}

// Acquire returns the (peerId, companion, role) session for a ReceiveSequence.
// Receive sequences follow the peer's handshake — they never initiate or
// restart one — so no epoch is started here; a server-role session's epoch is
// created when the peer's ClientHello arrives. Increments the reference count;
// callers call `session.Release()`. Returns nil when encryption is disabled or
// `peerId` is the zero Id (e.g., control destinations).
func (self *EncryptionSessionManager) Acquire(peerId Id, role sequenceTlsRole, companion bool) *peerEncryptionSession {
	return self.acquireSession(peerId, role, companion)
}

// AcquireForSend returns the (peerId, companion, role) session for a
// SendSequence. A client-role send sequence restarts the handshake — a fresh
// epoch handshakes in the background while the established cipher keeps serving.
// That is the recovery mechanism: every new client send re-initiates, so a peer
// that lost its responder session rebuilds it on the next burst. A server-role
// send sequence never restarts; it only carries EncryptedControl and
// server-session replies, otherwise following the peer's ClientHello.
func (self *EncryptionSessionManager) AcquireForSend(peerId Id, role sequenceTlsRole, companion bool) *peerEncryptionSession {
	session := self.acquireSession(peerId, role, companion)
	if session == nil {
		return nil
	}
	if role == sequenceTlsRoleClient {
		session.restartHandshake()
	}
	return session
}

// getOrCreate returns the (peerId, companion, role) session, creating and
// supervising it if absent. Takes the manager's stateLock internally.
func (self *EncryptionSessionManager) getOrCreate(peerId Id, role sequenceTlsRole, companion bool) *peerEncryptionSession {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.getOrCreateWithLock(peerId, role, companion)
}

// getOrCreateWithLock returns the (peerId, companion, role) session, creating
// and supervising it if absent. Caller holds stateLock.
func (self *EncryptionSessionManager) getOrCreateWithLock(peerId Id, role sequenceTlsRole, companion bool) *peerEncryptionSession {
	key := sessionKey{peerId: peerId, companion: companion, role: role}
	if s, ok := self.sessions[key]; ok {
		return s
	}
	s := newPeerEncryptionSession(self.ctx, self, self.client, peerId, role, self.settings, self.sessionTlsConfigWithLock(role), companion)
	self.sessions[key] = s
	if self.client.log.V(1).Enabled() {
		self.client.log.Infof("[tls]%s opened session for peer %s as %s c=%t\n", self.client.ClientTag(), peerId, role, companion)
	}
	go func() {
		defer func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.sessions[key] == s {
				delete(self.sessions, key)
			}
		}()
		s.Run()
	}()
	return s
}

// Lookup returns the existing (peerId, companion, role) session without
// changing its reference count, or nil if none exists or encryption is
// disabled.
func (self *EncryptionSessionManager) Lookup(peerId Id, role sequenceTlsRole, companion bool) *peerEncryptionSession {
	if !self.settings.Encrypt {
		return nil
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.sessions[sessionKey{peerId: peerId, companion: companion, role: role}]
}

// sessionsForPeer returns the existing sessions for `peerId` across both roles
// and both companion modes (client first). The unwrap path uses it to
// trial-decrypt a wrapped frame that carries no role hint. Does not change
// reference counts.
func (self *EncryptionSessionManager) sessionsForPeer(peerId Id) []*peerEncryptionSession {
	if !self.settings.Encrypt {
		return nil
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	var out []*peerEncryptionSession
	for _, role := range []sequenceTlsRole{sequenceTlsRoleClient, sequenceTlsRoleServer} {
		for _, companion := range []bool{false, true} {
			if s, ok := self.sessions[sessionKey{peerId: peerId, companion: companion, role: role}]; ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// sessionsForPeerRole returns the existing sessions for `peerId` and `role`
// across both companion modes (non-companion first). The unwrap path uses it
// to trial-decrypt a wrapped frame that carries a role hint but no companion
// hint — both companion sessions of the complement role are candidates.
func (self *EncryptionSessionManager) sessionsForPeerRole(peerId Id, role sequenceTlsRole) []*peerEncryptionSession {
	if !self.settings.Encrypt {
		return nil
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	var out []*peerEncryptionSession
	for _, companion := range []bool{false, true} {
		if s, ok := self.sessions[sessionKey{peerId: peerId, companion: companion, role: role}]; ok {
			out = append(out, s)
		}
	}
	return out
}

// DeliverEncryptedControl routes an inbound EncryptedControl to the local
// session keyed by `(peerId, ec.companion, role)` — where `role` is the
// complement of the sender's role, computed by the caller — creating it if
// necessary (handshake bytes can arrive before any local sequence is up). The
// identity companion comes from the control itself, so a created server session
// echoes it on replies and the initiator's reply state matches.
func (self *EncryptionSessionManager) DeliverEncryptedControl(peerId Id, role sequenceTlsRole, ec *protocol.EncryptedControl) {
	if !self.settings.Encrypt {
		return
	}
	if (peerId == Id{}) {
		return
	}
	session := self.getOrCreate(peerId, role, ec.GetCompanion())
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
//  1. `cfg.Certificates` (or `cfg.GetCertificate`) — a caller-supplied full
//     `tls.Config` wins; the leaf cert/key are exported for persistence callers
//     via the returned PEM values.
//  2. `certPem` + `keyPem` — pre-loaded PEM bytes (e.g. from a previous run
//     saved by `Client.EncryptionSessionManager().ProvideTlsCertificatePem()` /
//     `ProvideTlsPrivateKeyPem()`). Parsed into a `tls.Certificate` and folded
//     into a default config.
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
			// No resumption on per-peer sessions; suppress post-handshake
			// NewSessionTicket records (see DefaultSequenceServerTlsConfig).
			SessionTicketsDisabled: true,
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

// exportCertAndKeyPem returns the leaf cert chain and private key of `certs[0]`
// as PEM bytes, suitable for persistence and later reload through
// `resolveReceiveTlsConfig`. Returns nil for the key if the cert has no private
// key set (e.g. the caller supplied a `tls.Config` with only a `GetCertificate`
// callback).
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

func cloneByteSlices(in [][]byte) [][]byte {
	if len(in) == 0 {
		return nil
	}
	out := make([][]byte, 0, len(in))
	for _, b := range in {
		out = append(out, bytes.Clone(b))
	}
	return out
}

func logTlsHandshake(log Logger, tag string, err error) {
	if err != nil {
		if log.V(1).Enabled() {
			log.Infof("[tls]%s handshake error = %s\n", tag, err)
		}
	} else {
		if log.V(1).Enabled() {
			log.Infof("[tls]%s handshake complete\n", tag)
		}
	}
}

// logTlsHandshakePeerCert logs the peer's leaf certificate observed after the
// TLS handshake on the client-role side. Useful when triaging
// `verifyPeerCertAgainstContract` failures: this line shows which cert the peer
// presented, the mismatch log which cert the contract committed to.
func logTlsHandshakePeerCert(log Logger, tag string, peerCerts []*x509.Certificate) {
	if len(peerCerts) == 0 {
		if log.V(1).Enabled() {
			log.Infof("[tls]%s peer presented no certificate\n", tag)
		}
		return
	}
	if log.V(1).Enabled() {
		log.Infof("[tls]%s peer leaf subject=%q (chain length=%d)\n", tag, peerCerts[0].Subject.String(), len(peerCerts))
	}
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
