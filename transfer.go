package connect

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	// "runtime/debug"
	// "runtime"
	// "reflect"
	mathrand "math/rand"
	"slices"
	"strings"

	"golang.org/x/exp/maps"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

/*
Sends frames to destinations with properties:
- as long the sending client is active, frames are eventually delivered up to timeout
- frames are received in order of send
- sender is notified when frames are received
- sender and receiver account for mutual transfer with a shared contract
- return transfer is accounted to the sender
- support for multiple routes to the destination
- senders are verified with pre-exchanged keys
- high throughput and bounded resource usage

*/

/*
Each transport should apply the forwarding ACL:
- reject if source id does not match network id
- reject if not an active contract between sender and receiver

*/

// The transfer speed of each client is limited by its slowest destination.
// All traffic is multiplexed to a single connection, and blocking
// the connection ultimately limits the rate of `SendWithTimeout`.
// In this a client is similar to a socket. Multiple clients
// can be active in parallel, each limited by their slowest destination.

// *important* note on how "nack" transfer works with contracts
// nack data is associated with a contract, which is sent with ack=true
// on the other side, if the contract_id is not active when the nack arrives,
// the nack is dropped.
// To avoid racing the nack message with the ack contract,
// nacks are sent as ack until the contract is acked

// use 0 for deadlock testing
const defaultTransferBufferSize = 32

var DebugTransferCopyOnWrite = false

type AckFunction = func(err error)

// the identity of the source of received frames.
// `ProvideMode` is the mode of where these frames are from: network, friends and family, public.
// `Roles` and `Principal` are the source client's identity from the active contract,
// set only when the provide mode is network; nil roles and empty principal otherwise.
type Peer struct {
	ProvideMode protocol.ProvideMode
	Roles       []string
	Principal   string
}

type ReceiveFunction = func(source TransferPath, frames []*protocol.Frame, peer Peer)

// a forward callback receives a transfer frame addressed to another destination.
// like a receive callback, `transferFrameBytes` is valid only for the duration of
// the callback; the caller returns it after the callbacks run. A callback that
// retains the bytes (e.g. hands them off to a send or a channel) must
// `MessagePoolShareReadOnly` (or copy) them first.
type ForwardFunction = func(path TransferPath, transferFrameBytes []byte)

func DefaultClientSettings() *ClientSettings {
	return DefaultClientSettingsWithBufferSize(defaultTransferBufferSize)
}

func DefaultClientSettingsWithBufferSize(bufferSize int) *ClientSettings {
	settings := &ClientSettings{
		SendBufferSize:          bufferSize,
		ForwardBufferSize:       bufferSize,
		ReadTimeout:             30 * time.Second,
		BufferTimeout:           15 * time.Second,
		ControlPingTimeout:      time.Duration(0),
		SendBufferSettings:      DefaultSendBufferSettingsWithBufferSize(bufferSize),
		ReceiveBufferSettings:   DefaultReceiveBufferSettingsWithBufferSize(bufferSize),
		ForwardBufferSettings:   DefaultForwardBufferSettingsWithBufferSize(bufferSize),
		ContractManagerSettings: DefaultContractManagerSettingsWithBufferSize(bufferSize),
		StreamManagerSettings:   DefaultStreamManagerSettings(),
		PeerManagerSettings:     DefaultPeerManagerSettings(),
		WebRtcSettings:          DefaultWebRtcSettings(),
		EncryptionSettings:      DefaultEncryptionSettings(),
		ProtocolVersion:         DefaultProtocolVersion,
		DefaultTransferOpts:     DefaultTransferOpts(),
	}
	// A per-peer session is ref-held by both a send and a receive sequence, so
	// it must outlive the longer of the two — otherwise the next burst (after a
	// transport reform or lull) churns a fresh handshake instead of reusing the
	// live cipher.
	settings.EncryptionSettings.IdleTimeout = max(
		settings.SendBufferSettings.IdleTimeout,
		settings.ReceiveBufferSettings.IdleTimeout,
	)
	return settings
}

func DefaultClientSettingsNoNetworkEvents() *ClientSettings {
	clientSettings := DefaultClientSettings()
	clientSettings.ContractManagerSettings = DefaultContractManagerSettingsNoNetworkEvents()
	return clientSettings
}

func DefaultSendBufferSettings() *SendBufferSettings {
	return DefaultSendBufferSettingsWithBufferSize(defaultTransferBufferSize)
}

func DefaultSendBufferSettingsWithBufferSize(bufferSize int) *SendBufferSettings {
	return &SendBufferSettings{
		CreateContractTimeout:       30 * time.Second,
		CreateContractRetryInterval: 5 * time.Second,
		MinResendInterval:           2 * time.Second,
		MaxResendInterval:           8 * time.Second,
		// no backoff
		// ResendBackoffScale: 0,
		RttScale:         1.2,
		RttWindowSize:    128,
		RttWindowTimeout: 60 * time.Second,
		AckTimeout:       60 * time.Second,
		IdleTimeout:      300 * time.Second,
		// pause on resend for selectively acked messaged
		SelectiveAckTimeout: 60 * time.Second,
		SequenceBufferSize:  bufferSize,
		AckBufferSize:       bufferSize,
		MinMessageByteCount: ByteCount(1),
		// this includes transport reconnections
		WriteTimeout: 15 * time.Second,
		// per send sequence (per peer), so scaled by the memory budget.
		// when a shared budget is set, the max acts as the per-sequence
		// borrow cap and the min as the guaranteed floor.
		ResendQueueMaxByteCount: MemoryScaledByteCount(mib(2), kib(256)),
		ResendQueueMinByteCount: kib(256),
		ContractFillFraction:    0.8,
		ProtocolVersion:         DefaultProtocolVersion,
	}
}

func DefaultReceiveBufferSettings() *ReceiveBufferSettings {
	return DefaultReceiveBufferSettingsWithBufferSize(defaultTransferBufferSize)
}

func DefaultReceiveBufferSettingsWithBufferSize(bufferSize int) *ReceiveBufferSettings {
	return &ReceiveBufferSettings{
		GapTimeout: 60 * time.Second,
		// the receive idle timeout should be a bit longer than the send idle timeout
		IdleTimeout:        120 * time.Second,
		SequenceBufferSize: bufferSize,
		// AckBufferSize: DefaultTransferBufferSize,
		// coalesce acks into a periodic cumulative head ack
		// without coalescing, every received message emits an ack frame, which
		// doubles the per-pair message volume on the relay path for one-way
		// streams and feeds resend storms under load. The window is far below
		// the `MinResendInterval` floor (2s), so it does not affect resends.
		AckCompressTimeout:  10 * time.Millisecond,
		MinMessageByteCount: ByteCount(1),
		// ResendAbuseThreshold: 4,
		// ResendAbuseMultiple:  0.5,
		MaxPeerAuditDuration: 60 * time.Second,
		// this includes transport reconnections
		WriteTimeout: 15 * time.Second,
		// per receive sequence (per peer), so scaled by the memory budget.
		// when a shared budget is set, the max acts as the per-sequence
		// borrow cap and the min as the guaranteed floor.
		ReceiveQueueMaxByteCount: MemoryScaledByteCount(mib(2)+kib(512), kib(320)),
		ReceiveQueueMinByteCount: kib(320),
		AllowLegacyNack:          true,
		MaxOpenReceiveContract:   4,
		ProtocolVersion:          DefaultProtocolVersion,
	}
}

func DefaultForwardBufferSettings() *ForwardBufferSettings {
	return DefaultForwardBufferSettingsWithBufferSize(defaultTransferBufferSize)
}

func DefaultForwardBufferSettingsWithBufferSize(bufferSize int) *ForwardBufferSettings {
	return &ForwardBufferSettings{
		IdleTimeout:        300 * time.Second,
		SequenceBufferSize: bufferSize,
		WriteTimeout:       15 * time.Second,
	}
}

type SendPack struct {
	TransferOptions

	// frame and destination is repacked by the send buffer into a Pack,
	// with destination and frame from the tframe, and other pack properties filled in by the buffer
	Frame           *protocol.Frame
	Destination     TransferPath
	IntermediaryIds MultiHopId
	// called (true) when the pack is ack'd, or (false) if not ack'd (closed before ack)
	AckCallback      AckFunction
	MessageByteCount ByteCount
	Ctx              context.Context
	// ForceUnwrapped pins the wire frame to plaintext for the item's lifetime,
	// including retransmits. Used by session control messages (TLS handshake
	// bytes) that bootstrap the cipher: they must never be sent encrypted, since
	// the peer may not have completed its half of the handshake even after our
	// local cipher is established.
	ForceUnwrapped bool
	// EncryptionRole selects which per-peer session this pack uses, keying the
	// SendSequence so the roles run as distinct sequences: client (the default —
	// the client's own outbound data, whose handshake it initiates/restarts) or
	// server (EncryptedControl carriers and server-session replies, which never
	// restart the handshake).
	EncryptionRole sequenceTlsRole
	// EncryptionCompanion is the per-peer session identity companion this pack
	// uses — keys the SendSequence and its session, distinct from
	// `TransferOptions.CompanionContract` (the contract it rides; the two differ
	// only for a server-role EncryptedControl reply carrier).
	EncryptionCompanion bool
}

type ReceivePack struct {
	Source             TransferPath
	SequenceId         Id
	Pack               *protocol.Pack
	ReceiveCallback    ReceiveFunction
	MessageByteCount   ByteCount
	TransferFrameBytes []byte
	Ctx                context.Context
	// Unwrapped is true when the inbound TransferFrame arrived as plaintext (no
	// outer wrap). The ack for this pack (and any aggregated ack including it) is
	// sent plaintext to mirror, so a peer whose cipher isn't up yet isn't handed
	// a wrapped ack it can't open.
	Unwrapped bool
	// EncryptionRole is the local per-peer session role that owns this inbound
	// stream — the complement of the sender's role, keying the ReceiveSequence
	// and the session it holds. Normal peer data (peer is the TLS client) maps
	// to our server session (the default); EncryptedControl carriers and
	// server-session replies map to our client session.
	EncryptionRole sequenceTlsRole
	// EncryptionCompanion is the local session identity companion that owns this
	// inbound stream (shared by both peers, not complemented). Derived from the
	// wire companion hint, the decrypting session, or the EncryptedControl;
	// defaults false. Keys the ReceiveSequence and its session.
	EncryptionCompanion bool
}

type ForwardPack struct {
	Destination        TransferPath
	TransferFrameBytes []byte
	Ctx                context.Context
}

type TransferOptions struct {
	// items can choose to not be acked
	// in this case, the ack callback is called on send, and no retry is done
	// when false, items may arrive out of order amongst un-acked sequence neighbors
	Ack bool
	// use a companion contract
	// a companion contract replies to an existing contract
	// using this option limits the destination to clients that have an active contract to the sender
	CompanionContract bool
	// force contract streams, even when there are zero intermediaries
	ForceStream bool
}

func DefaultTransferOpts() TransferOptions {
	return TransferOptions{
		Ack:               true,
		CompanionContract: false,
		ForceStream:       false,
	}
}

type transferOptionsSetAck struct {
	Ack bool
}

func NoAck() transferOptionsSetAck {
	return transferOptionsSetAck{
		Ack: false,
	}
}

type transferOptionsSetCompanionContract struct {
	CompanionContract bool
}

func CompanionContract() transferOptionsSetCompanionContract {
	return transferOptionsSetCompanionContract{
		CompanionContract: true,
	}
}

type transferOptionsSetForceStream struct {
	ForceStream bool
}

func ForceStream() transferOptionsSetForceStream {
	return transferOptionsSetForceStream{
		ForceStream: true,
	}
}

type transferCtx struct {
	Ctx context.Context
}

func Ctx(ctx context.Context) transferCtx {
	return transferCtx{
		Ctx: ctx,
	}
}

type ClientSettings struct {
	SendBufferSize    int
	ForwardBufferSize int
	ReadTimeout       time.Duration
	BufferTimeout     time.Duration
	// if 0, the client will not send control pings
	ControlPingTimeout time.Duration

	// Log, when set, is used by the client and all nested components
	// (propagated to nested settings `Log` fields that are nil).
	// nil resolves to `DefaultLogger()`. See log.go.
	Log Logger

	SendBufferSettings      *SendBufferSettings
	ReceiveBufferSettings   *ReceiveBufferSettings
	ForwardBufferSettings   *ForwardBufferSettings
	ContractManagerSettings *ContractManagerSettings
	StreamManagerSettings   *StreamManagerSettings
	PeerManagerSettings     *PeerManagerSettings
	WebRtcSettings          *WebRtcSettings
	EncryptionSettings      *EncryptionSettings

	// ClientKeySeed, when set, is the long-lived Ed25519 client identity key
	// seed (`ed25519.NewKeyFromSeed`); must be `ed25519.SeedSize` (32) bytes.
	// When empty, `ClientKeyManager` generates a fresh seed. Persist the running
	// value (`Client.ClientKeyManager().Seed()`) and reload it on the next run
	// to keep the published `ClientKey` (and contract bindings to it) stable
	// across process lifetimes.
	ClientKeySeed []byte

	ProtocolVersion int

	DefaultTransferOpts TransferOptions
}

// MinimumMessageLenLimit returns the smallest per-transport framer
// `MaxMessageLen` (and receive-side caps, e.g. `websocket.SetReadLimit`) the
// runtime can reliably operate under. Below it, the per-peer handshake can
// deadlock: the TLS server flight ships as one large `EncryptedControl{Handshake}`
// Pack, and if any hop's framer rejects it as oversized the stream closes
// mid-handshake, the retransmit re-sends the same oversized pack, and both sides
// time out.
//
// Worst-case sizing (verified against the active TLS profile — TLS 1.3,
// X25519MLKEM768 hybrid group, ephemeral ECDSA P-256 cert, mTLS):
//
//	ServerHello ~1.2 KiB (MLKEM768 key share ~1.1 KiB), ChangeCipherSpec ~6 B,
//	EncryptedExtensions ~10 B, CertificateRequest ~30 B, Certificate ~500–600 B,
//	CertificateVerify ~80 B, Finished ~45 B, + ~5 B record header each
//	  ≈ 2 KiB raw; + ~200 B EC/Frame/Pack/TransferFrame proto wrap ≈ 2.2 KiB
//
// Rounded up to 4 KiB to absorb ASN.1 cert-size jitter, a future larger
// post-quantum key share, and protobuf field-tag drift. Production transports
// default well above this; tests and embedded callers should plumb it through
// their framer caps (and matching receive-side limits):
//
//	settings.FramerSettings.MaxMessageLen = max(yourValue, int(client.MinimumMessageLenLimit()))
func (self *ClientSettings) MinimumMessageLenLimit() ByteCount {
	return ByteCount(4 * 1024)
}

// note all callbacks are wrapped to check for nil and recover from errors
type Client struct {
	ctx    context.Context
	cancel context.CancelFunc

	clientId  Id
	clientTag string
	clientOob OutOfBandControl

	log Logger

	settings *ClientSettings

	receiveCallbacks *CallbackList[ReceiveFunction]
	forwardCallbacks *CallbackList[ForwardFunction]

	loopback chan *SendPack

	routeManager             *RouteManager
	contractManager          *ContractManager
	webRtcManager            *WebRtcManager
	streamManager            *StreamManager
	peerManager              *PeerManager
	sendBuffer               *SendBuffer
	receiveBuffer            *ReceiveBuffer
	forwardBuffer            *ForwardBuffer
	clientKeyManager         *ClientKeyManager
	encryptionSessionManager *EncryptionSessionManager

	// ready is closed by NewClientWithTag right before it returns, once every
	// manager, buffer, callback, and the `run` loop are wired up. See
	// ReadyNotify for the gating contract.
	ready chan struct{}

	// contractManagerUnsub func()
	webRtcManagerUnsub func()
	streamManagerUnsub func()
	peerManagerUnsub   func()
}

func NewClientWithDefaults(
	ctx context.Context,
	clientId Id,
	clientOob OutOfBandControl,
) *Client {
	return NewClient(
		ctx,
		clientId,
		clientOob,
		DefaultClientSettings(),
	)
}

func NewClient(
	ctx context.Context,
	clientId Id,
	clientOob OutOfBandControl,
	settings *ClientSettings,
) *Client {
	clientTag := clientId.String()
	return NewClientWithTag(ctx, clientId, clientTag, clientOob, settings)
}

func NewClientWithTag(
	ctx context.Context,
	clientId Id,
	clientTag string,
	clientOob OutOfBandControl,
	settings *ClientSettings,
) *Client {
	cancelCtx, cancel := context.WithCancel(ctx)
	log := loggerOrDefault(settings.Log)
	// nested components without a client reference resolve their own settings
	// `Log`. Propagate so a client-level logger covers the entire client tree.
	if settings.WebRtcSettings != nil && settings.WebRtcSettings.Log == nil {
		settings.WebRtcSettings.Log = log
	}
	client := &Client{
		ctx:              cancelCtx,
		cancel:           cancel,
		clientId:         clientId,
		clientTag:        clientTag,
		clientOob:        clientOob,
		log:              log,
		settings:         settings,
		receiveCallbacks: NewCallbackList[ReceiveFunction](),
		forwardCallbacks: NewCallbackList[ForwardFunction](),
		loopback:         make(chan *SendPack),
		ready:            make(chan struct{}),
	}

	routeManager := NewRouteManagerWithLogger(ctx, clientTag, log)
	contractManager := NewContractManager(ctx, client, settings.ContractManagerSettings)
	webRtcManager := NewWebRtcManager(ctx, NewClientSignalSender(client), settings.WebRtcSettings)
	streamManager := NewStreamManager(ctx, client, webRtcManager, settings.StreamManagerSettings)
	peerManager := NewPeerManager(ctx, client, settings.PeerManagerSettings)
	// ClientKeyManager must precede EncryptionSessionManager — the latter holds
	// a reference to sign the published TLS cert
	// (`EncryptedKey.ClientKeySignedTlsCertificate`) and per-peer identity proofs.
	clientKeyManager, err := NewClientKeyManager(client.ctx, client)
	if err != nil {
		log.Errorf("[key]%s could not initialize client key: %s\n", client.ClientTag(), err)
		clientKeyManager = nil
	}
	encryptionSessionManager := NewEncryptionSessionManager(client.ctx, client, clientKeyManager, client.settings.EncryptionSettings)

	// client.contractManagerUnsub = client.AddReceiveCallback(contractManager.Receive)
	client.webRtcManagerUnsub = ReceiveSignalsFromClient(client, webRtcManager)
	client.streamManagerUnsub = client.AddReceiveCallback(streamManager.Receive)
	client.peerManager = peerManager
	client.peerManagerUnsub = client.AddReceiveCallback(peerManager.Receive)

	client.initBuffers(routeManager, contractManager, webRtcManager, streamManager, clientKeyManager, encryptionSessionManager)

	go HandleError(client.run, cancel)

	// Mark the client fully constructed: manager goroutines started above (e.g.
	// `publishEncryptedKey`, `providePing`) gate their first send on this so they
	// don't race the wiring above.
	close(client.ready)

	return client
}

// ReadyNotify returns a channel closed once `NewClientWithTag` has finished
// wiring the client (managers, callbacks, buffers, `run` loop). Any goroutine
// launched during construction must wait on it (or `ctx.Done()`) before its
// first send into the client's send path.
func (self *Client) ReadyNotify() <-chan struct{} {
	return self.ready
}

// Log is the logger used by this client and its nested components.
func (self *Client) Log() Logger {
	return self.log
}

func (self *Client) initBuffers(
	routeManager *RouteManager,
	contractManager *ContractManager,
	webRtcManager *WebRtcManager,
	streamManager *StreamManager,
	clientKeyManager *ClientKeyManager,
	encryptionSessionManager *EncryptionSessionManager,
) {
	self.routeManager = routeManager
	self.contractManager = contractManager
	self.webRtcManager = webRtcManager
	self.streamManager = streamManager
	self.clientKeyManager = clientKeyManager
	self.encryptionSessionManager = encryptionSessionManager

	// sendBuffer / receiveBuffer / forwardBuffer come first because
	// `EncryptionSessionManager` publishes its cert (via `EncryptedKey`)
	// at construction time, and the publish path goes through
	// `sendBuffer.Pack`.
	self.sendBuffer = NewSendBuffer(self.ctx, self, self.settings.SendBufferSettings)
	self.receiveBuffer = NewReceiveBuffer(self.ctx, self, self.settings.ReceiveBufferSettings)
	self.forwardBuffer = NewForwardBuffer(self.ctx, self, self.settings.ForwardBufferSettings)
}

func (self *Client) EncryptionSessionManager() *EncryptionSessionManager {
	return self.encryptionSessionManager
}

// unwrapFrame opens an outer-wrapped TransferFrame from `sourceId`. `roleHint`
// (the sender's session role; may be `SequenceRoleUnknown`) selects the
// complement local session to try first; `companionHint` (the sender's session
// companion; nil when the sender omitted it) further pins the exact companion
// session. With a role hint but no companion hint, both companion sessions of
// the complement role are tried; with no role hint, every per-peer session is
// tried. Each candidate session's ciphers are tried (established plus, briefly
// during a rekey, the prior established) until one authenticates. Returns the
// plaintext inner bytes and the local session role and companion that
// decrypted them (used as the receive sequence's role/companion). Wait-free:
// it never blocks the receive loop.
func (self *Client) unwrapFrame(sourceId Id, roleHint protocol.SequenceRole, companionHint *bool, wrapped []byte) ([]byte, sequenceTlsRole, bool, error) {
	if self.encryptionSessionManager == nil {
		return nil, sequenceTlsRoleServer, false, fmt.Errorf("encryption disabled")
	}
	// A wrapped frame can only be opened by the complement of the sender's
	// session role (the other local session is the opposite TLS direction
	// with a different key), so a present role hint narrows us to that role —
	// and a present companion hint pins exactly one session (Option 1). A role
	// hint without a companion hint leaves both companion sessions as
	// candidates. With no role hint — the sender omitted it for on-wire
	// anonymity — trial-decrypt against every per-peer session (Option 2).
	var ordered []*peerEncryptionSession
	if senderRole, ok := sequenceTlsRoleFromProtobuf(roleHint); ok {
		complement := senderRole.complement()
		if companionHint != nil {
			if s := self.encryptionSessionManager.Lookup(sourceId, complement, *companionHint); s != nil {
				ordered = append(ordered, s)
			}
		} else {
			ordered = self.encryptionSessionManager.sessionsForPeerRole(sourceId, complement)
		}
	} else {
		ordered = self.encryptionSessionManager.sessionsForPeer(sourceId)
	}
	if len(ordered) == 0 {
		return nil, sequenceTlsRoleServer, false, fmt.Errorf("no encryption session for peer %s", sourceId)
	}
	for _, session := range ordered {
		for _, cipher := range session.decryptCiphers() {
			if plaintext, err := cipher.Open(wrapped); err == nil {
				return plaintext, session.role, session.companion, nil
			}
		}
	}
	return nil, sequenceTlsRoleServer, false, fmt.Errorf("no encryption session for peer %s could decrypt", sourceId)
}

func (self *Client) ClientKeyManager() *ClientKeyManager {
	return self.clientKeyManager
}

func (self *Client) RouteManager() *RouteManager {
	return self.routeManager
}

func (self *Client) ContractManager() *ContractManager {
	return self.contractManager
}

func (self *Client) ClientId() Id {
	return self.clientId
}

func (self *Client) ClientTag() string {
	return self.clientTag
}

func (self *Client) ClientOob() OutOfBandControl {
	return self.clientOob
}

// a peer of this client on the network
type NetworkPeer struct {
	ClientId Id
	// the peer's enabled provide modes
	ProvideModes []protocol.ProvideMode
	// whether the peer has the network provide mode enabled
	ProvideEnabled bool
	Principal      string
	Roles          []string
	DeviceSpec     string
	DeviceName     string
}

func (self *Client) PeerManager() *PeerManager {
	return self.peerManager
}

// NetworkPeers enumerates the connected peers and the count of
// recently disconnected peers.
// The platform announces peers only to top-level clients;
// all other clients have no network peers.
func (self *Client) NetworkPeers() (connected []*NetworkPeer, disconnectedCount int) {
	return self.peerManager.NetworkPeers()
}

func (self *Client) ReportAbuse(source TransferPath) {
	peerAudit := NewSequencePeerAudit(self, source, 0)
	peerAudit.Update(func(peerAudit *PeerAudit) {
		peerAudit.Abuse = true
	})
	peerAudit.Complete()
}

func (self *Client) ForwardWithTimeout(transferFrameBytes []byte, timeout time.Duration, opts ...any) bool {
	success, err := self.ForwardWithTimeoutDetailed(transferFrameBytes, timeout, opts...)
	return success && err == nil
}

func (self *Client) ForwardWithTimeoutDetailed(transferFrameBytes []byte, timeout time.Duration, opts ...any) (bool, error) {
	select {
	case <-self.ctx.Done():
		return false, errors.New("Done")
	default:
	}

	path, err := FilteredTransferPath(transferFrameBytes)
	if err != nil {
		// bad protobuf
		return false, err
	}

	destination := path.DestinationMask()

	ctx := self.ctx
	for _, opt := range opts {
		switch v := opt.(type) {
		case transferCtx:
			ctx = v.Ctx
		}
	}

	forwardPack := &ForwardPack{
		Destination:        destination,
		TransferFrameBytes: transferFrameBytes,
		Ctx:                ctx,
	}

	return self.forwardBuffer.Pack(forwardPack, timeout)
}

func (self *Client) Forward(transferFrameBytes []byte, opts ...any) bool {
	return self.ForwardWithTimeout(transferFrameBytes, -1, opts...)
}

func (self *Client) SendWithTimeout(
	frame *protocol.Frame,
	destination TransferPath,
	ackCallback AckFunction,
	timeout time.Duration,
	opts ...any,
) bool {
	success, err := self.SendWithTimeoutDetailed(frame, destination, ackCallback, timeout, opts...)
	return success && err == nil
}

func (self *Client) SendWithTimeoutDetailed(
	frame *protocol.Frame,
	destination TransferPath,
	ackCallback AckFunction,
	timeout time.Duration,
	opts ...any,
) (bool, error) {
	return self.sendWithTimeoutDetailed(
		frame,
		destination,
		MultiHopId{},
		ackCallback,
		timeout,
		opts...,
	)
}

func (self *Client) SendMultiHopWithTimeout(
	frame *protocol.Frame,
	destination MultiHopId,
	ackCallback AckFunction,
	timeout time.Duration,
	opts ...any,
) bool {
	success, err := self.SendMultiHopWithTimeoutDetailed(frame, destination, ackCallback, timeout, opts...)
	return success && err == nil
}

func (self *Client) SendMultiHopWithTimeoutDetailed(
	frame *protocol.Frame,
	destination MultiHopId,
	ackCallback AckFunction,
	timeout time.Duration,
	opts ...any,
) (bool, error) {
	if destination.Len() == 0 {
		return false, errors.New("Must have at least one destination id.")
	}
	intermediaryIds, destinationId := destination.SplitTail()
	// note we do not force stream here
	// legacy no-intermediary will not use streams by default
	return self.sendWithTimeoutDetailed(
		frame,
		DestinationId(destinationId),
		intermediaryIds,
		ackCallback,
		timeout,
		opts...,
	)
}

func (self *Client) sendWithTimeout(
	frame *protocol.Frame,
	destination TransferPath,
	intermediaryIds MultiHopId,
	ackCallback AckFunction,
	timeout time.Duration,
	opts ...any,
) bool {
	success, err := self.sendWithTimeoutDetailed(frame, destination, intermediaryIds, ackCallback, timeout, opts...)
	return success && err == nil
}

func (self *Client) sendWithTimeoutDetailed(
	frame *protocol.Frame,
	destination TransferPath,
	intermediaryIds MultiHopId,
	ackCallback AckFunction,
	timeout time.Duration,
	opts ...any,
) (bool, error) {
	if !destination.IsDestinationMask() {
		panic(fmt.Errorf("Destination required for send: %s", destination))
	}
	if destination.IsStream() {
		panic(fmt.Errorf("Destination must not be a stream: %s", destination))
	}

	select {
	case <-self.ctx.Done():
		return false, errors.New("Done")
	default:
	}

	ctx := self.ctx
	var transferOpts TransferOptions
	transferOpts = self.settings.DefaultTransferOpts
	for _, opt := range opts {
		switch v := opt.(type) {
		case TransferOptions:
			transferOpts = v
		case transferOptionsSetAck:
			transferOpts.Ack = v.Ack
		case transferOptionsSetForceStream:
			transferOpts.ForceStream = v.ForceStream
		case transferOptionsSetCompanionContract:
			transferOpts.CompanionContract = v.CompanionContract
		case transferCtx:
			ctx = v.Ctx
		}
	}

	messageByteCount := ByteCount(len(frame.MessageBytes))
	sendPack := &SendPack{
		TransferOptions: transferOpts,
		Frame:           frame,
		Destination:     destination,
		IntermediaryIds: intermediaryIds,
		// store the raw callback; invoked via safeAck so no per-send wrapper
		// closure is allocated.
		AckCallback:      ackCallback,
		MessageByteCount: messageByteCount,
		Ctx:              ctx,
		// Ordinary application data: the session identity companion is the
		// sequence's own contract-companion bit (no client/server split here).
		EncryptionCompanion: transferOpts.CompanionContract,
	}

	if sendPack.Destination.DestinationId == self.clientId {
		// loopback
		// fast path without arming a timer
		select {
		case self.loopback <- sendPack:
			return true, nil
		default:
		}

		if timeout < 0 {
			select {
			case <-ctx.Done():
				return false, errors.New("Done")
			case <-self.ctx.Done():
				return false, errors.New("Done")
			case self.loopback <- sendPack:
				return true, nil
			}
		} else if timeout == 0 {
			select {
			case <-ctx.Done():
				return false, errors.New("Done")
			case <-self.ctx.Done():
				return false, errors.New("Done")
			case self.loopback <- sendPack:
				return true, nil
			default:
				return false, nil
			}
		} else {
			select {
			case <-ctx.Done():
				return false, errors.New("Done")
			case <-self.ctx.Done():
				return false, errors.New("Done")
			case self.loopback <- sendPack:
				return true, nil
			case <-time.After(timeout):
				return false, nil
			}
		}
	} else {
		return self.sendBuffer.Pack(sendPack, timeout)
	}
}

func (self *Client) SendControlWithTimeout(frame *protocol.Frame, ackCallback AckFunction, timeout time.Duration) bool {
	return self.SendWithTimeout(
		frame,
		DestinationId(ControlId),
		ackCallback,
		timeout,
	)
}

func (self *Client) Send(frame *protocol.Frame, destination TransferPath, ackCallback AckFunction) bool {
	return self.SendWithTimeout(frame, destination, ackCallback, -1)
}

func (self *Client) SendControl(frame *protocol.Frame, ackCallback AckFunction) bool {
	return self.Send(
		frame,
		DestinationId(ControlId),
		ackCallback,
	)
}

func (self *Client) SendMultiHop(frame *protocol.Frame, destination MultiHopId, ackCallback AckFunction) bool {
	return self.SendMultiHopWithTimeout(frame, destination, ackCallback, -1)
}

// ReceiveFunction
func (self *Client) receive(source TransferPath, frames []*protocol.Frame, peer Peer) {
	for _, receiveCallback := range self.receiveCallbacks.Get() {
		c := func() any {
			return HandleError(func() {
				receiveCallback(source, frames, peer)
			})
		}
		if self.log.V(2).Enabled() {
			TraceWithReturn(
				fmt.Sprintf("[c]receive callback %s %s", self.clientTag, CallbackName(receiveCallback)),
				c,
			)
		} else {
			c()
		}
	}
}

// ForwardFunction
// forward dispatches to the forward callbacks. It is itself a `ForwardFunction`:
// the bytes are valid only for the duration of the call, and the caller returns
// them after (mirrors `receive`).
func (self *Client) forward(path TransferPath, transferFrameBytes []byte) {
	for _, forwardCallback := range self.forwardCallbacks.Get() {
		c := func() any {
			return HandleError(func() {
				forwardCallback(path, transferFrameBytes)
			})
		}
		if self.log.V(2).Enabled() {
			TraceWithReturn(
				fmt.Sprintf("[c]forward callback %s %s", self.clientTag, CallbackName(forwardCallback)),
				c,
			)
		} else {
			c()
		}
	}
}

func (self *Client) AddReceiveCallback(receiveCallback ReceiveFunction) func() {
	callbackId := self.receiveCallbacks.Add(receiveCallback)
	return func() {
		self.receiveCallbacks.Remove(callbackId)
	}
}

func (self *Client) AddForwardCallback(forwardCallback ForwardFunction) func() {
	callbackId := self.forwardCallbacks.Add(forwardCallback)
	return func() {
		self.forwardCallbacks.Remove(callbackId)
	}
}

func (self *Client) run() {
	defer self.cancel()

	// receive
	multiRouteReader := self.routeManager.OpenMultiRouteReader(DestinationId(self.clientId))
	defer self.routeManager.CloseMultiRouteReader(multiRouteReader)

	updatePeerAudit := func(source TransferPath, callback func(*PeerAudit)) {
		// immediately send peer audits at this level
		peerAudit := NewSequencePeerAudit(self, source, 0)
		peerAudit.Update(callback)
		peerAudit.Complete()
	}

	// control ping
	if self.clientId != ControlId && 0 < self.settings.ControlPingTimeout {
		go HandleError(func() {
			for {
				// uniform timeout with mean `ControlPingTimeout`
				timeout := time.Duration(mathrand.Int63n(int64(2 * self.settings.ControlPingTimeout)))
				select {
				case <-self.ctx.Done():
					return
				case <-WakeupAfter(timeout, self.settings.ControlPingTimeout):
				}

				ack := make(chan error)
				frame, err := ToFrame(&protocol.ControlPing{}, self.settings.ProtocolVersion)
				if err != nil {
					self.log.Errorf("[c]could not create ping frame = %s", err)
					continue
				}

				success := self.SendControl(frame, func(err error) {
					select {
					case ack <- err:
					case <-self.ctx.Done():
					}
				})
				if !success {
					// the send did not take the frame: no ack will ever fire, so
					// free the frame and try again next interval instead of
					// wedging this loop on an ack that cannot come
					MessagePoolReturn(frame.MessageBytes)
					continue
				}
				// wait for the ack before sending another ping
				select {
				case err := <-ack:
					if err == nil {
						self.log.Infof("[c]ping\n")
					} else {
						self.log.Infof("[c]ping err = %s\n", err)
					}
				case <-self.ctx.Done():
					return
				}
			}
		})
	}

	// loopback messages must be serialized
	go HandleError(func() {
		for {
			select {
			case <-self.ctx.Done():
				return
			case sendPack := <-self.loopback:
				func() {
					defer MessagePoolReturn(sendPack.Frame.MessageBytes)
					HandleError(func() {
						self.receive(
							SourceId(self.clientId),
							[]*protocol.Frame{sendPack.Frame},
							Peer{ProvideMode: protocol.ProvideMode_Network},
						)
						safeAck(sendPack.AckCallback, nil)
					}, func(err error) {
						safeAck(sendPack.AckCallback, err)
					})
				}()
			}
		}
	}, self.cancel)

	for {
		select {
		case <-self.ctx.Done():
			return
		default:
		}

		var transferFrameBytes []byte
		var err error
		c := func() error {
			transferFrameBytes, err = multiRouteReader.Read(self.ctx, self.settings.ReadTimeout)
			return err
		}
		if self.log.V(2).Enabled() {
			TraceWithReturn(
				fmt.Sprintf("[c]multi route read %s<-", self.clientTag),
				c,
			)
		} else {
			c()
		}
		if err != nil {
			continue
		}

		// at this point, the route is expected to have already parsed the transfer frame
		// and applied basic validation and source/destination checks
		// because of this, errors in parsing the `FilteredTransferFrame` are not expected
		// decode a minimal subset of the full message needed to make a routing decision
		path, err := FilteredTransferPath(transferFrameBytes)
		if err != nil {
			// bad protobuf (unexpected, see route note above)
			MessagePoolReturn(transferFrameBytes)
			continue
		}
		if path.IsStream() {
			if self.log.V(1).Enabled() {
				self.log.Infof("[cr] %s cannot route message with stream\n", self.clientTag)
			}
			MessagePoolReturn(transferFrameBytes)
			continue
		}

		source := path.SourceMask()

		if self.log.V(1).Enabled() {
			self.log.Infof("[cr] %s %s<-%s s(%s)\n", self.clientTag, path.DestinationId, path.SourceId, path.StreamId)
		}

		if path.DestinationId == self.clientId {
			// the transports have typically not parsed the full `TransferFrame`
			// on error, discard the message and report the peer
			transferFrame := &protocol.TransferFrame{}
			// hand-rolled copy-safe decode (no reflection); skips the outer
			// transfer_path (routing already parsed it via FilteredTransferPath)
			// and the deprecated message_type. See frame_protobuf.go.
			if !unmarshalTransferFrame(transferFrameBytes, transferFrame, false) {
				// bad protobuf
				updatePeerAudit(source, func(a *PeerAudit) {
					a.badMessage(ByteCount(len(transferFrameBytes)))
				})
				MessagePoolReturn(transferFrameBytes)
				continue
			}

			// unwrapped tracks whether the frame arrived on the wire as
			// plaintext (true) or wrapped (false). Propagated through the
			// ReceivePack → receiveItem → ack path so an ack mirrors the
			// wrap state of the messages it acknowledges. Mirroring keeps
			// acks legible to peers whose ciphers haven't come up yet.
			unwrapped := true

			// receiveRole is the local per-peer session role that owns this
			// inbound stream, handed to the ReceiveBuffer so the
			// ReceiveSequence holds the right session. Default server: normal
			// peer data (the peer is the TLS client) decrypts under our
			// server session. Adjusted below to the role that actually
			// decrypted a wrapped frame, and to client for a plaintext
			// EncryptedControl carrier (the peer's server-role stream).
			receiveRole := sequenceTlsRoleServer
			// receiveCompanion is the local session identity companion owning
			// this inbound stream (not complemented). Default false; set below
			// from the decrypting session, the plaintext companion hint, or the
			// EncryptedControl.
			receiveCompanion := false

			// outer encrypted wrap: the inner bytes are themselves a
			// `TransferFrame`. A per-peer session for `source` carries the
			// cipher. Forwarders never see this branch — they only look at
			// the outer TransferPath, which is plaintext.
			if 0 < len(transferFrame.EncryptedTransferFrame) {
				unwrapped = false
				// Unwrap is fully non-blocking: if no session can decrypt
				// yet, drop the frame and let the sender's resend recover. A
				// client-role send sequence restarts the handshake on its
				// next burst, so a peer that lost (or never built) its
				// responder session rebuilds it — the drop is transient, not
				// a wedge. Keeping the unwrap path wait-free means no single
				// peer can park the single-threaded, all-peers receive loop.
				unwrappedTransferFrameBytes, decryptRole, decryptCompanion, err := self.unwrapFrame(
					path.SourceId, transferFrame.GetSessionRole(), transferFrame.SessionCompanion, transferFrame.EncryptedTransferFrame)
				if err != nil {
					if self.log.V(1).Enabled() {
						self.log.Infof("[cr]unwrap err = %s\n", err)
					}
					MessagePoolReturn(transferFrameBytes)
					continue
				}
				receiveRole = decryptRole
				receiveCompanion = decryptCompanion
				unwrappedTransferFrame := &protocol.TransferFrame{}
				// inner frame: decode the path too — it is tamper-checked against
				// the routing path below.
				if !unmarshalTransferFrame(unwrappedTransferFrameBytes, unwrappedTransferFrame, true) {
					updatePeerAudit(source, func(a *PeerAudit) {
						a.badMessage(ByteCount(len(transferFrameBytes)))
					})
					MessagePoolReturn(transferFrameBytes)
					MessagePoolReturn(unwrappedTransferFrameBytes)
					continue
				}
				// the inner TransferPath is AEAD-authenticated; the outer
				// is only the routing hint. A mismatch implies tampering
				// in flight or a routing/sender bug. Drop and audit.
				unwrappedPath, err := TransferPathFromProtobuf(unwrappedTransferFrame.TransferPath)
				if err != nil || unwrappedPath != path {
					if self.log.V(1).Enabled() {
						self.log.Infof("[cr] %s outer/inner TransferPath mismatch from %s\n", self.clientTag, path.SourceId)
					}
					updatePeerAudit(source, func(a *PeerAudit) {
						a.badMessage(ByteCount(len(transferFrameBytes)))
					})
					MessagePoolReturn(transferFrameBytes)
					MessagePoolReturn(unwrappedTransferFrameBytes)
					continue
				}
				MessagePoolReturn(transferFrameBytes)
				transferFrameBytes = unwrappedTransferFrameBytes
				transferFrame = unwrappedTransferFrame
			}

			// A plaintext pack with a sender-role hint is the peer's
			// EncryptedControl carrier (its server-role stream). Map the whole
			// sequence to the complement local session — across both the EC packs
			// and the non-EC open/contract packs — so they share one receive
			// sequence; deriving the role per-pack (from the EC frames below)
			// would split the open pack off and gap the handshake. Wrapped packs
			// use the decrypt role from above; the no-hint default is server. The
			// companion hint, when present, pins the companion session (shared by
			// both peers, so taken as-is, not complemented).
			if unwrapped {
				if senderRole, ok := sequenceTlsRoleFromProtobuf(transferFrame.GetSessionRole()); ok {
					receiveRole = senderRole.complement()
				}
				if transferFrame.SessionCompanion != nil {
					receiveCompanion = transferFrame.GetSessionCompanion()
				}
			}

			ack := transferFrame.Ack
			pack := transferFrame.Pack

			if frame := transferFrame.GetFrame(); frame != nil {

				switch frame.GetMessageType() {
				case protocol.MessageType_TransferAck:
					ack = &protocol.Ack{}
					if err := ProtoUnmarshal(frame.GetMessageBytes(), ack); err != nil {
						// bad protobuf
						updatePeerAudit(source, func(a *PeerAudit) {
							a.badMessage(ByteCount(len(transferFrameBytes)))
						})
						MessagePoolReturn(transferFrameBytes)
						continue
					}

				case protocol.MessageType_TransferPack:
					pack = &protocol.Pack{}
					if err := ProtoUnmarshal(frame.GetMessageBytes(), pack); err != nil {
						// bad protobuf
						updatePeerAudit(source, func(a *PeerAudit) {
							a.badMessage(ByteCount(len(transferFrameBytes)))
						})
						MessagePoolReturn(transferFrameBytes)
						continue
					}

				default:
					updatePeerAudit(source, func(a *PeerAudit) {
						a.badMessage(ByteCount(len(transferFrameBytes)))
					})
					MessagePoolReturn(transferFrameBytes)
					continue
				}
			}

			if ack != nil {
				c := func() bool {
					defer MessagePoolReturn(transferFrameBytes)
					return self.sendBuffer.Ack(
						source.Reverse(),
						ack,
						self.settings.BufferTimeout,
					)
				}
				if self.log.V(2).Enabled() {
					TraceWithReturn(
						fmt.Sprintf("[cr]ack %s %s<-%s s(%s)", self.clientTag, path.DestinationId, path.SourceId, path.SourceId),
						c,
					)
				} else {
					c()
				}
			}
			if pack != nil {
				sequenceId, err := IdFromBytes(pack.SequenceId)
				if err != nil {
					// bad protobuf
					MessagePoolReturn(transferFrameBytes)
					continue
				}
				// Optimistic EC apply: deliver EncryptedControl frames straight to
				// the per-peer session from the receive loop, bypassing the in-order
				// ReceiveSequence drain (which can stall on a sequence gap from a
				// transport reform or loss). EC frames only piggyback that ordering
				// to reuse the retransmit/route plumbing; each handler below is safe
				// to invoke off-order:
				//   - Handshake: gated on `IsAwaitingClientFinished` + a record-prefix
				//     check in `OptimisticallyDeliverHandshake` that rejects
				//     ClientHello-shaped retransmits, so no duplicate bytes reach the
				//     TLS state machine.
				//   - IdentityProof: `receivePeerIdentityProof` short-circuits once
				//     verified, failed, or already buffered — safe to re-deliver.
				// The ReceiveSequence's later in-order delivery still runs and
				// short-circuits in both handlers (just a re-unmarshal). Gated on
				// `unwrapped` (EC packs are always ForceUnwrapped) to skip the
				// wrapped app-data hot path.
				if unwrapped && self.encryptionSessionManager != nil {
					for _, frame := range pack.Frames {
						if frame == nil || frame.MessageType != protocol.MessageType_TransferEncryptedControl {
							continue
						}
						ec := &protocol.EncryptedControl{}
						if err := ProtoUnmarshal(frame.MessageBytes, ec); err != nil {
							continue
						}
						senderRole, ok := sequenceTlsRoleFromProtobuf(ec.SessionRole)
						if !ok {
							continue
						}
						// This stream maps to the complement local session —
						// the one the EncryptedControl drives — keyed by the
						// EC's echoed identity companion. The receive sequence
						// holds it (keeping it alive through the handshake),
						// matching where the EC routes below.
						receiveRole = senderRole.complement()
						receiveCompanion = ec.GetCompanion()
						// Optimistically apply to the complement local session
						// if it already exists; the ReceiveSequence's in-order
						// delivery getOrCreates it otherwise.
						session := self.encryptionSessionManager.Lookup(path.SourceId, senderRole.complement(), ec.GetCompanion())
						if session == nil {
							continue
						}
						switch ec.ControlType {
						case protocol.EncryptedControlType_EncryptedControlHandshake:
							if session.IsAwaitingClientFinished() {
								session.OptimisticallyDeliverHandshake(ec.Payload)
							}
						case protocol.EncryptedControlType_EncryptedControlIdentityProof:
							// Optimistic path must not create epoch state from a
							// stale/reordered/retransmitted proof; only deliver
							// against an epoch that already exists. The in-order
							// path (DeliverEncryptedControl) still handles a proof
							// that races ahead of the local handshake by creating
							// the epoch to buffer it.
							if session.currentEpoch() != nil {
								session.receivePeerIdentityProof(ec.Payload)
							}
						}
					}
				}
				messageByteCount := MessageByteCount(pack.Frames)
				c := func() bool {
					success, err := self.receiveBuffer.Pack(&ReceivePack{
						Source:              source,
						SequenceId:          sequenceId,
						Pack:                pack,
						ReceiveCallback:     self.receive,
						MessageByteCount:    messageByteCount,
						TransferFrameBytes:  transferFrameBytes,
						Unwrapped:           unwrapped,
						EncryptionRole:      receiveRole,
						EncryptionCompanion: receiveCompanion,
					}, self.settings.BufferTimeout)
					if !success {
						MessagePoolReturn(transferFrameBytes)
					}
					return success && err == nil
				}
				if self.log.V(2).Enabled() {
					TraceWithReturn(
						fmt.Sprintf("[cr]pack %s %s<-%s s(%s)", self.clientTag, path.DestinationId, path.SourceId, path.StreamId),
						c,
					)
				} else {
					c()
				}
			}
		} else {
			c := func() {
				// forward is a callback: the bytes are valid only for its duration
				// and are returned here. Without the return, a client with no
				// forwarder leaks one pool buffer for every frame that arrives
				// addressed to another destination (e.g. control-addressed frames
				// accepted by a gateway transport).
				defer MessagePoolReturn(transferFrameBytes)
				self.forward(
					path,
					transferFrameBytes,
				)
			}
			if self.log.V(1).Enabled() {
				Trace(
					fmt.Sprintf("[cr]forward %s %s<-%s s(%s)", self.clientTag, path.DestinationId, path.SourceId, path.StreamId),
					c,
				)
			} else {
				c()
			}
		}
	}
}

func (self *Client) ResendQueueSize(destination TransferPath, intermediaryIds MultiHopId, companionContract bool, forceStream bool) (int, ByteCount, Id) {
	count, byteSize, sequenceId, _ := self.ResendQueueSizeAndMessageTypes(destination, intermediaryIds, companionContract, forceStream)
	return count, byteSize, sequenceId
}

func (self *Client) ResendQueueSizeAndMessageTypes(
	destination TransferPath,
	intermediaryIds MultiHopId,
	companionContract bool,
	forceStream bool,
) (
	int,
	ByteCount,
	Id,
	[]protocol.MessageType,
) {
	if self.sendBuffer == nil {
		return 0, 0, Id{}, nil
	} else {
		return self.sendBuffer.ResendQueueSizeAndMessageTypes(destination, intermediaryIds, companionContract, forceStream)
	}
}

func (self *Client) ReceiveQueueSize(source TransferPath, sequenceId Id) (int, ByteCount) {
	count, byteSize, _ := self.ReceiveQueueSizeAndMessageTypes(source, sequenceId)
	return count, byteSize
}

func (self *Client) ReceiveQueueSizeAndMessageTypes(source TransferPath, sequenceId Id) (int, ByteCount, []protocol.MessageType) {
	if self.receiveBuffer == nil {
		return 0, 0, nil
	} else {
		return self.receiveBuffer.ReceiveQueueSizeAndMessageTypes(source, sequenceId)
	}
}

func (self *Client) IsDone() bool {
	select {
	case <-self.ctx.Done():
		return true
	default:
		return false
	}
}

func (self *Client) Done() <-chan struct{} {
	return self.ctx.Done()
}

func (self *Client) Ctx() context.Context {
	return self.ctx
}

// this does not need to be called if `Cancel` is called
func (self *Client) Close() {
	self.cancel()

	self.sendBuffer.Close()
	self.receiveBuffer.Close()
	self.forwardBuffer.Close()
	if self.encryptionSessionManager != nil {
		self.encryptionSessionManager.Close()
	}

	// self.contractManagerUnsub()
	self.webRtcManagerUnsub()
	self.streamManagerUnsub()
	self.peerManagerUnsub()
}

func (self *Client) Cancel() {
	self.cancel()

	self.sendBuffer.Cancel()
	self.receiveBuffer.Cancel()
	self.forwardBuffer.Cancel()
}

func (self *Client) Flush() {
	self.sendBuffer.Flush()
	self.receiveBuffer.Flush()
	self.forwardBuffer.Flush()

	self.contractManager.Flush(false)
}

type SendBufferSettings struct {
	CreateContractTimeout       time.Duration
	CreateContractRetryInterval time.Duration

	// resend timeout is the initial time between successive send attempts. Does linear backoff
	MinResendInterval time.Duration
	MaxResendInterval time.Duration
	// ResendBackoffScale float32

	RttScale         float32
	RttWindowSize    int
	RttWindowTimeout time.Duration

	// on ack timeout, no longer attempt to retransmit and notify of ack failure
	AckTimeout  time.Duration
	IdleTimeout time.Duration

	SelectiveAckTimeout time.Duration

	SequenceBufferSize int
	AckBufferSize      int

	MinMessageByteCount ByteCount

	WriteTimeout time.Duration

	ResendQueueMaxByteCount ByteCount
	// ResendQueueMinByteCount is the guaranteed per-sequence floor when
	// `ResendQueueBudget` is set: below it admission never consults the
	// shared budget, so every sequence progresses on floor capacity alone
	ResendQueueMinByteCount ByteCount
	// ResendQueueBudget, when set, is a byte budget shared across sequences
	// (typically all clients of one device): resend queue bytes above the
	// floor reserve from it, and admission pauses above the floor while it
	// is empty. nil keeps independent per-sequence caps.
	ResendQueueBudget *TransferMemoryBudget

	// as this ->1, there is more risk that noack messages will get dropped due to out of sync contracts
	ContractFillFraction float32

	ProtocolVersion int
}

type sendSequenceId struct {
	Destination       TransferPath
	IntermediaryIds   MultiHopId
	CompanionContract bool
	ForceStream       bool
	// EncryptionRole separates the client-role send sequence (normal
	// application data, which restarts the handshake) from the server-role
	// send sequence (EncryptedControl carriers + server replies, which never
	// restart). Zero value is client.
	EncryptionRole sequenceTlsRole
	// EncryptionCompanion is the per-peer session identity companion, distinct
	// from `CompanionContract`: a server-role reply carrier echoes the
	// initiator's bit while riding EncryptionControlUseCompanion, so it must key
	// the sequence separately to keep each session's carrier distinct.
	EncryptionCompanion bool
}

type SendBuffer struct {
	ctx    context.Context
	client *Client
	log    Logger

	sendBufferSettings *SendBufferSettings

	mutex                      sync.Mutex
	sendSequences              map[sendSequenceId]*SendSequence
	sendSequencesByDestination map[TransferPath]map[*SendSequence]bool
	sendSequenceDestinations   map[*SendSequence]map[TransferPath]bool
}

func NewSendBuffer(ctx context.Context,
	client *Client,
	sendBufferSettings *SendBufferSettings) *SendBuffer {
	return &SendBuffer{
		ctx:                        ctx,
		client:                     client,
		log:                        client.log,
		sendBufferSettings:         sendBufferSettings,
		sendSequences:              map[sendSequenceId]*SendSequence{},
		sendSequencesByDestination: map[TransferPath]map[*SendSequence]bool{},
		sendSequenceDestinations:   map[*SendSequence]map[TransferPath]bool{},
	}
}

func (self *SendBuffer) Pack(sendPack *SendPack, timeout time.Duration) (bool, error) {
	sendSequenceId := sendSequenceId{
		Destination:         sendPack.Destination,
		IntermediaryIds:     sendPack.IntermediaryIds,
		CompanionContract:   sendPack.TransferOptions.CompanionContract,
		ForceStream:         sendPack.TransferOptions.ForceStream,
		EncryptionRole:      sendPack.EncryptionRole,
		EncryptionCompanion: sendPack.EncryptionCompanion,
	}

	initSendSequence := func(skip *SendSequence) *SendSequence {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		sendSequence, ok := self.sendSequences[sendSequenceId]
		if ok {
			if skip == nil || skip != sendSequence {
				return sendSequence
			} else {
				sendSequence.Cancel()
				delete(self.sendSequences, sendSequenceId)
			}
		}
		sendSequence = NewSendSequence(
			self.ctx,
			self.client,
			self,
			sendPack.Destination,
			sendPack.IntermediaryIds,
			sendPack.TransferOptions.CompanionContract,
			sendPack.TransferOptions.ForceStream,
			sendPack.EncryptionRole,
			sendPack.EncryptionCompanion,
			self.sendBufferSettings,
		)
		self.sendSequences[sendSequenceId] = sendSequence
		// note we do not associate destination here
		// the sequence will call `AssociateDestination` before it writes
		go HandleError(func() {
			defer func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()
				sendSequence.Close()
				// clean up
				if sendSequence == self.sendSequences[sendSequenceId] {
					delete(self.sendSequences, sendSequenceId)
				}
				if destinations, ok := self.sendSequenceDestinations[sendSequence]; ok {
					for destination, _ := range destinations {
						if sendSequences, ok := self.sendSequencesByDestination[destination]; ok {
							delete(sendSequences, sendSequence)
							if len(sendSequences) == 0 {
								delete(self.sendSequencesByDestination, destination)
							}
						}
					}
					delete(self.sendSequenceDestinations, sendSequence)
				}
			}()
			sendSequence.Run()
		})
		return sendSequence
	}

	var sendSequence *SendSequence
	var success bool
	var err error
	for i := 0; i < 2; i += 1 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		default:
		}
		sendSequence = initSendSequence(sendSequence)
		if success, err = sendSequence.Pack(sendPack, timeout); err == nil {
			return success, nil
		}
		// sequence closed
	}
	return success, err
}

// SendEncryptedControl enqueues an `EncryptedControl` to `destination` as a
// regular Pack Frame (`MessageType = TransferEncryptedControl`); routing,
// retransmit, and in-order delivery reuse the sequence machinery, and the
// destination's ReceiveSequence intercepts these frames into the per-peer
// session.
//
// `ctx` gates whether the spawned goroutine may enqueue (it bails if done). The
// pack uses the SendBuffer's ctx — the session ctx must not propagate into
// `SendPack.Ctx`, since SendBuffer.Pack treats a canceled `SendPack.Ctx` as a
// sequence problem and cancels the SendSequence.
func (self *SendBuffer) SendEncryptedControl(ctx context.Context, peerId Id, role sequenceTlsRole, ec *protocol.EncryptedControl, encryptionCompanion bool, contractCompanion bool) bool {
	select {
	case <-ctx.Done():
		return false
	default:
	}
	ecBytes, err := ProtoMarshal(ec)
	if err != nil {
		return false
	}
	frame := &protocol.Frame{
		MessageType:  protocol.MessageType_TransferEncryptedControl,
		MessageBytes: ecBytes,
	}
	// Mirror the client's default TransferOptions — especially
	// `ForceStream` — so the SendSequence chosen by `SendBuffer.Pack`
	// matches the one the application's `Client.Send` chooses for this
	// destination.
	//
	// The carrier rides one send sequence per (peer, companion, role).
	// `contractCompanion` (the session's carrierCompanion) is which contract it
	// rides; `encryptionCompanion` (the session identity bit) keys the
	// sequence/session. They differ only for a server reply, where the identity
	// is the initiator's echoed bit but the contract is
	// EncryptionControlUseCompanion. Symmetric config: both false.
	opts := self.client.settings.DefaultTransferOpts
	opts.Ack = true
	opts.CompanionContract = contractCompanion
	// V(2) diagnostic: in symmetric mode no encryption-control carrier should
	// be a companion. Log the decision so a companion carrier (whose Stream-mode
	// contract the platform rejects → handshake stalls) can be caught.
	if self.log.V(2).Enabled() {
		self.log.Infof(
			"[sb][enc-ctrl]%s peer=%s role=%v companion=%t contract-companion=%t\n",
			self.client.ClientTag(), peerId, role, encryptionCompanion, contractCompanion,
		)
	}
	sendPack := &SendPack{
		TransferOptions:  opts,
		Frame:            frame,
		Destination:      DestinationId(peerId),
		AckCallback:      func(error) {},
		MessageByteCount: ByteCount(len(ecBytes)),
		Ctx:              self.ctx,
		// Pin to plaintext on every (re)send. These frames bootstrap the
		// per-peer cipher; sending them wrapped would deadlock the
		// handshake whenever the local cipher becomes available before
		// the peer's side completes its half. See writeMaybeWrappedBytes.
		ForceUnwrapped: true,
		// Carry EncryptedControl on the send sequence of the originating
		// session's role (client-session handshake bytes on the (peer,client)
		// sequence, server-session bytes on the (peer,server) one). For the
		// client role this is the same sequence the application data uses, so
		// the ClientHello produced by that sequence's own restart rides it
		// without spawning a second sequence (no recursion); the restart is a
		// no-op while a handshake is already in flight. The EncryptedControl's
		// `session_role` + `companion` tell the receiver which complement
		// session to route each frame to.
		EncryptionRole:      role,
		EncryptionCompanion: encryptionCompanion,
	}
	for {
		if success, _ := self.Pack(sendPack, self.client.settings.BufferTimeout); success {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-self.ctx.Done():
			return false
		default:
		}
	}
}

func (self *SendBuffer) Ack(destination TransferPath, ack *protocol.Ack, timeout time.Duration) bool {
	sendSequences := func() []*SendSequence {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		if sendSequences, ok := self.sendSequencesByDestination[destination]; ok {
			return maps.Keys(sendSequences)
		} else {
			return []*SendSequence{}
		}
	}

	anyFound := false
	anySuccess := false
	for _, seq := range sendSequences() {
		anyFound = true
		if success, err := seq.Ack(ack, timeout); success && err == nil {
			anySuccess = true
		}
	}
	if !anyFound {
		if self.log.V(1).Enabled() {
			self.log.Infof("[sb]ack miss sequence does not exist %s\n", destination)
		}
	}
	return anySuccess
}

func (self *SendBuffer) ResendQueueSizeAndMessageTypes(destination TransferPath, intermediaryIds MultiHopId, companionContract bool, forceStream bool) (int, ByteCount, Id, []protocol.MessageType) {
	sendSequence := func() *SendSequence {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		return self.sendSequences[sendSequenceId{
			Destination:       destination,
			IntermediaryIds:   intermediaryIds,
			CompanionContract: companionContract,
			ForceStream:       forceStream,
		}]
	}

	if seq := sendSequence(); seq != nil {
		return seq.ResendQueueSizeAndMessageTypes()
	}
	return 0, 0, Id{}, nil
}

// called before a send sequence writes a transfer frame with a stream id,
// once per destination
func (self *SendBuffer) AssociateDestination(sendSequence *SendSequence, destination TransferPath) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	sendSequences, ok := self.sendSequencesByDestination[destination]
	if !ok {
		sendSequences = map[*SendSequence]bool{}
		self.sendSequencesByDestination[destination] = sendSequences
	}
	sendSequences[sendSequence] = true

	destinations, ok := self.sendSequenceDestinations[sendSequence]
	if !ok {
		destinations = map[TransferPath]bool{}
		self.sendSequenceDestinations[sendSequence] = destinations
	}
	destinations[destination] = true
}

func (self *SendBuffer) Close() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// cancel all open sequences
	// the control of the sequence will close it
	for _, sendSequence := range self.sendSequences {
		sendSequence.Cancel()
	}
}

func (self *SendBuffer) Cancel() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// cancel all open sequences
	for _, sendSequence := range self.sendSequences {
		sendSequence.Cancel()
	}
}

func (self *SendBuffer) Flush() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// cancel all open sequences
	for _, sendSequence := range self.sendSequences {
		// if !sendSequenceId.Destination.IsControlDestination() {
		sendSequence.Cancel()
		// }
	}
}

type SendSequence struct {
	ctx    context.Context
	cancel context.CancelFunc

	client     *Client
	sendBuffer *SendBuffer
	log        Logger

	destination       TransferPath
	intermediaryIds   MultiHopId
	companionContract bool
	forceStream       bool
	// encryptionRole is the per-peer session role this send sequence uses:
	// client for normal application data (the default), server for
	// EncryptedControl carriers and server-session replies.
	encryptionRole sequenceTlsRole
	// encryptionCompanion is the per-peer session identity companion this
	// sequence uses (distinct from `companionContract`). Keys the acquired
	// session and is stamped on every pack as the `session_companion` wire hint.
	encryptionCompanion bool
	sequenceId          Id

	sendBufferSettings *SendBufferSettings

	// the head contract. this contract is also in `openSendContracts`
	sendContract      *sequenceContract
	sendContractAcked bool
	// contracts are closed when the data are acked
	// these contracts are waiting for acks to close
	openSendContracts map[Id]*sequenceContract

	packMutex sync.Mutex
	packs     chan *SendPack
	ackMutex  sync.Mutex
	acks      chan *protocol.Ack

	resendQueue        *resendQueue
	sendItems          []*sendItem
	nextSequenceNumber uint64

	idleCondition *IdleCondition

	rttWindow *RttWindow

	contractMultiRouteWriter            MultiRouteWriter
	contractMultiRouteWriterDestination TransferPath

	contractSeqIndex uint64

	// session is the per-peer TLS session shared by every local SendSequence
	// and ReceiveSequence to the same peer/stream. Acquired from the
	// `EncryptionSessionManager` at construction; released when the sequence
	// terminates. Nil when encryption is disabled on this client.
	session *peerEncryptionSession
}

func NewSendSequence(
	ctx context.Context,
	client *Client,
	sendBuffer *SendBuffer,
	destination TransferPath,
	intermediaryIds MultiHopId,
	companionContract bool,
	forceStream bool,
	encryptionRole sequenceTlsRole,
	encryptionCompanion bool,
	sendBufferSettings *SendBufferSettings) *SendSequence {
	cancelCtx, cancel := context.WithCancel(ctx)

	rttWindow := NewRttWindow(
		client.log,
		sendBufferSettings.RttWindowSize,
		sendBufferSettings.RttWindowTimeout,
		sendBufferSettings.RttScale,
		sendBufferSettings.MinResendInterval,
		sendBufferSettings.MaxResendInterval,
	)

	seq := &SendSequence{
		ctx:                 cancelCtx,
		cancel:              cancel,
		client:              client,
		sendBuffer:          sendBuffer,
		log:                 client.log,
		destination:         destination,
		intermediaryIds:     intermediaryIds,
		companionContract:   companionContract,
		forceStream:         forceStream,
		encryptionRole:      encryptionRole,
		encryptionCompanion: encryptionCompanion,
		sequenceId:          NewId(),
		sendBufferSettings:  sendBufferSettings,
		sendContract:        nil,
		sendContractAcked:   false,
		openSendContracts:   map[Id]*sequenceContract{},
		packs:               make(chan *SendPack, sendBufferSettings.SequenceBufferSize),
		acks:                make(chan *protocol.Ack, sendBufferSettings.AckBufferSize),
		resendQueue:         newResendQueue(sendBufferSettings.ResendQueueBudget, sendBufferSettings.ResendQueueMinByteCount),
		sendItems:           []*sendItem{},
		nextSequenceNumber:  0,
		idleCondition:       NewIdleCondition(),
		rttWindow:           rttWindow,
		contractSeqIndex:    0,
	}
	// Never encrypt control-plane traffic. A SendSequence's data source is
	// always this client (sourceId == client.ClientId()) and its destination
	// is destination.DestinationId; when `SendNoSession` holds for either
	// endpoint, no session is acquired and traffic flows in plaintext.
	if client != nil && client.encryptionSessionManager != nil &&
		!client.encryptionSessionManager.SendNoSession(destination.DestinationId) {
		// Acquire the (peer, encryptionRole) session. A client-role send
		// sequence restarts the handshake (recovery: every new client send
		// re-initiates, rebuilding a peer's lost responder session); a
		// server-role send sequence (EncryptedControl carrier / server
		// reply) never restarts.
		seq.session = client.encryptionSessionManager.AcquireForSend(destination.DestinationId, encryptionRole, encryptionCompanion)
	}
	return seq
}

func (self *SendSequence) ResendQueueSizeAndMessageTypes() (int, ByteCount, Id, []protocol.MessageType) {
	unpackMessageTypes := func(item *sendItem) any {
		var messageTypes []protocol.MessageType
		var transferFrame protocol.TransferFrame
		err := proto.Unmarshal(item.transferFrameBytes, &transferFrame)
		if err == nil && transferFrame.Pack != nil {
			for _, frame := range transferFrame.Pack.Frames {
				messageTypes = append(messageTypes, frame.MessageType)
			}
		}
		return messageTypes
	}
	count, byteSize, summary := self.resendQueue.QueueSizeAndSummary(unpackMessageTypes)
	var messageTypes []protocol.MessageType
	for _, summaryMessageTypes := range summary {
		messageTypes = append(messageTypes, summaryMessageTypes.([]protocol.MessageType)...)
	}
	return count, byteSize, self.sequenceId, messageTypes
}

// success, error
func (self *SendSequence) Pack(sendPack *SendPack, timeout time.Duration) (bool, error) {
	self.packMutex.Lock()
	defer self.packMutex.Unlock()

	select {
	case <-sendPack.Ctx.Done():
		return false, errors.New("Done.")
	case <-self.ctx.Done():
		return false, errors.New("Done.")
	default:
	}

	if !self.idleCondition.UpdateOpen() {
		return false, errors.New("Done.")
	}
	defer self.idleCondition.UpdateClose()

	// fast path without arming a timer
	select {
	case self.packs <- sendPack:
		return true, nil
	default:
	}

	if timeout < 0 {
		select {
		case <-sendPack.Ctx.Done():
			return false, errors.New("Done.")
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.packs <- sendPack:
			return true, nil
		}
	} else if timeout == 0 {
		select {
		case <-sendPack.Ctx.Done():
			return false, errors.New("Done.")
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.packs <- sendPack:
			return true, nil
		default:
			return false, nil
		}
	} else {
		select {
		case <-sendPack.Ctx.Done():
			return false, errors.New("Done.")
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.packs <- sendPack:
			return true, nil
		case <-time.After(timeout):
			return false, nil
		}
	}
}

func (self *SendSequence) Ack(ack *protocol.Ack, timeout time.Duration) (bool, error) {
	self.ackMutex.Lock()
	defer self.ackMutex.Unlock()

	sequenceId, err := IdFromBytes(ack.SequenceId)
	if err != nil {
		return false, err
	}
	if self.sequenceId != sequenceId {
		// ack is for a different send sequence that no longer exists
		return false, nil
	}

	select {
	case <-self.ctx.Done():
		return false, errors.New("Done.")
	default:
	}

	// fast path without arming a timer
	select {
	case self.acks <- ack:
		return true, nil
	default:
	}

	if timeout < 0 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.acks <- ack:
			return true, nil
		}
	} else if timeout == 0 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.acks <- ack:
			return true, nil
		default:
			return false, nil
		}
	} else {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.acks <- ack:
			return true, nil
		case <-time.After(timeout):
			return false, nil
		}
	}
}

func (self *SendSequence) Run() {
	defer func() {
		if r := recover(); r != nil {
			self.log.Errorf("[s]%s->%s...%s s(%s) abnormal exit =  %s\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId, r)
			panic(r)
		}
	}()
	defer func() {
		self.cancel()

		// close contract
		for _, sendContract := range self.openSendContracts {
			self.client.ContractManager().CloseContract(
				sendContract.contractId,
				sendContract.ackedByteCount,
				sendContract.unackedByteCount,
			)
			// flush queued contracts for already sent contracts
			// contractKey = ContractKey{
			// 	Destination:       sendContract.path.DestinationMask(),
			// 	IntermediaryIds:   self.intermediaryIds,
			// 	CompanionContract: self.companionContract,
			// 	ForceStream:       self.forceStream,
			// }
			// self.client.ContractManager().FlushContractQueue(contractKey, true)
		}

		// drain the buffer, releasing any borrowed budget
		for _, item := range self.resendQueue.Clear() {
			safeAck(item.ackCallback, errors.New("Send sequence closed."))
			item.messagePoolReturn()
		}

		// flush queued contracts (used ids were closed above). Keyed by
		// (EncryptionRole, EncryptionCompanion) so this exit-flush doesn't discard
		// a peer-paired sequence's pending contracts — the EC carrier and normal
		// data are separate sequences to the same destination.
		contractKey := ContractKey{
			Destination:         self.destination,
			IntermediaryIds:     self.intermediaryIds,
			CompanionContract:   self.companionContract,
			ForceStream:         self.forceStream,
			EncryptionRole:      self.encryptionRole,
			EncryptionCompanion: self.encryptionCompanion,
		}
		self.client.ContractManager().FlushContractQueue(contractKey, true)

		self.closeContractMultiRouteWriter()

		if self.session != nil {
			// No explicit close: a closing SendSequence must not tear down
			// the shared session (a concurrent ReceiveSequence may still be
			// using it) and must not emit anything on the wire. A future
			// initiator SendSequence resets the handshake when it resumes.
			self.session.Release()
		}
	}()

	ackWindow := newSequenceAckWindow()
	go HandleError(func() {
		defer self.cancel()

		for {
			select {
			case <-self.ctx.Done():
				return
			case ack, ok := <-self.acks:
				if !ok {
					return
				}
				if messageId, err := IdFromBytes(ack.MessageId); err == nil {
					if sequenceNumber, ok := self.resendQueue.ContainsMessageId(messageId); ok {
						ack := &sequenceAck{
							messageId:      messageId,
							sequenceNumber: sequenceNumber,
							selective:      ack.Selective,
							tag:            ack.Tag,
						}
						ackWindow.Update(ack)
					}
				}
			}
		}
	}, self.cancel)

	// reusable idle/resend timer: a per-iteration time.After would allocate a
	// timer per packet on this hot loop. created already-fired; the Reset before
	// each blocking select arms it (go1.23+ delivers no stale fire after Reset).
	idleTimer := time.NewTimer(0)
	defer idleTimer.Stop()

	for {
		// apply the acks
		ackSnapshot := ackWindow.Snapshot(true)
		if 0 < ackSnapshot.ackUpdateCount {
			self.receiveAck(ackSnapshot.headAck.messageId, false, ackSnapshot.headAck.tag)
		}
		for messageId, ack := range ackSnapshot.selectiveAcks {
			self.receiveAck(messageId, true, ack.tag)
		}

		sendTime := time.Now()
		var timeout time.Duration

		if self.resendQueue.Len() == 0 {
			timeout = self.sendBufferSettings.IdleTimeout
		} else {
			timeout = self.sendBufferSettings.AckTimeout

			for {
				item := self.resendQueue.PeekFirst()
				if item == nil {
					break
				}

				itemAckTimeout := item.sendTime.Add(self.sendBufferSettings.AckTimeout).Sub(sendTime)
				if itemAckTimeout <= 0 {
					// message took too long to ack
					// close the sequence
					if self.log.V(1).Enabled() {
						self.log.Infof("[s]%s->%s...%s s(%s) exit ack timeout (%s)\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId, self.sendBufferSettings.AckTimeout)
					}
					return
				}
				if itemAckTimeout < timeout {
					timeout = itemAckTimeout
				}

				if sendTime.Before(item.resendTime) {
					itemResendTimeout := item.resendTime.Sub(sendTime)
					if itemResendTimeout < timeout {
						timeout = itemResendTimeout
					}
					break
				}

				self.resendQueue.RemoveByMessageId(item.messageId)

				// resend
				var transferFrameBytes []byte
				if self.sendItems[0].sequenceNumber == item.sequenceNumber && !item.head {
					// set `head=true`
					var err error
					transferFrameBytes, err = self.setHead(item)
					if err != nil {
						self.log.Errorf("[s]%s->%s...%s s(%s) exit could not set head = %s\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId, err)
						return
					}
					MessagePoolReturn(item.transferFrameBytes)
					item.head = true
					item.transferFrameBytes = transferFrameBytes
				} else {
					// var err error
					// transferFrameBytes, err = self.setTag(item)
					// if err != nil {
					// 	self.log.Errorf("[s]%s->%s...%s s(%s) exit could not set tag = %s\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId, err)
					// 	return
					// }
					transferFrameBytes = item.transferFrameBytes
				}

				// resend uses the same path the item was originally sent on
				resendPath := self.destination.AddSource(self.client.ClientId())
				resendBytes := transferFrameBytes
				resendForceUnwrapped := item.forceUnwrapped
				c := func() error {
					return self.writeMaybeWrappedBytes(resendBytes, resendPath, resendForceUnwrapped)
				}
				if self.log.V(2).Enabled() {
					TraceWithReturn(
						fmt.Sprintf(
							"[s]resend %d multi route write %s->%s...%s s(%s)",
							item.sequenceNumber,
							self.client.ClientTag(),
							self.intermediaryIds,
							self.destination.DestinationId,
							self.destination.StreamId,
						),
						c,
					)
				} else {
					err := c()
					if err != nil {
						if self.log.V(1).Enabled() {
							self.log.Infof("[s]resend drop = %s", err)
						}
					}
				}

				item.sendCount += 1
				// back off the resend timeout multiplicatively with each resend
				// of the same item, up to `MaxResendInterval`. When acks are
				// delayed (not lost) by queueing, a flat timeout re-sends the
				// whole in-flight window every interval, and the duplicates
				// feed the congestion that delayed the acks in the first place.
				itemResendTimeout := self.rttWindow.ScaledRtt()
				// cap the shift so the backoff does not overflow
				if shift := uint(min(item.sendCount-1, 16)); 0 < shift {
					itemResendTimeout = min(
						itemResendTimeout<<shift,
						self.sendBufferSettings.MaxResendInterval,
					)
				}
				if itemAckTimeout <= itemResendTimeout {
					item.resendTime = sendTime.Add(itemAckTimeout)
				} else {
					item.resendTime = sendTime.Add(itemResendTimeout)
				}
				self.resendQueue.Add(item)
			}
		}

		checkpointId := self.idleCondition.Checkpoint()

		// approximate since this cannot consider the next message byte size.
		// an empty queue always admits at least one item (see CanAdd).
		canQueue := func() bool {
			return self.resendQueue.CanAdd(0, self.sendBufferSettings.ResendQueueMaxByteCount)
		}
		if !canQueue() {
			// wait for acks
			idleTimer.Reset(timeout)
			select {
			case <-self.ctx.Done():
				return
			case <-ackSnapshot.ackNotify:
			case <-idleTimer.C:
				if 0 == self.resendQueue.Len() {
					done := false
					func() {
						self.packMutex.Lock()
						defer self.packMutex.Unlock()
						// idle timeout
						if self.idleCondition.Close(checkpointId) {
							done = true
						}
						// else there are pending updates
					}()
					if done {
						// close the sequence
						if self.log.V(1).Enabled() {
							self.log.Infof("[s]%s->%s...%s s(%s) exit idle timeout\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId)
						}
						return
					}
				}
			}
		} else {
			processPack := func(sendPack *SendPack, ok bool) bool {
				if !ok {
					return false
				}

				// note messages of `size < MinMessageByteCount` get counted as `MinMessageByteCount` against the contract
				if self.updateContract(sendPack.MessageByteCount) {
					self.send(sendPack.Frame, sendPack.AckCallback, sendPack.Ack, sendPack.ForceUnwrapped)
					// ignore the error since there will be a retry
					return true
				}
				// no contract
				// close the sequence
				self.log.Errorf("[s]%s->%s...%s s(%s) exit could not create contract.\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId)
				safeAck(sendPack.AckCallback, errors.New("No contract"))
				MessagePoolReturn(sendPack.Frame.MessageBytes)
				return false
			}

			// fast path without arming a timer
			select {
			case <-self.ctx.Done():
				return
			case sendPack, ok := <-self.packs:
				if !processPack(sendPack, ok) {
					return
				}
				continue
			default:
			}

			idleTimer.Reset(timeout)
			select {
			case <-self.ctx.Done():
				return
			case <-ackSnapshot.ackNotify:
			case sendPack, ok := <-self.packs:
				if !processPack(sendPack, ok) {
					return
				}
			case <-idleTimer.C:
				if 0 == self.resendQueue.Len() {
					done := false
					func() {
						self.packMutex.Lock()
						defer self.packMutex.Unlock()
						// idle timeout
						if self.idleCondition.Close(checkpointId) {
							done = true
						}
						// else there are pending updates
					}()
					if done {
						// close the sequence
						if self.log.V(1).Enabled() {
							self.log.Infof("[s]%s->%s...%s s(%s) exit idle timeout\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId)
						}
						return
					}
				}
			}
		}
	}
}

func (self *SendSequence) updateContract(messageByteCount ByteCount) bool {
	// `sendNoContract` is a mutual configuration
	// both sides must configure themselves to require no contract from each other
	if self.client.ContractManager().SendNoContract(self.destination.DestinationId) {
		return true
	}
	if self.sendContract != nil && self.sendContract.update(messageByteCount) {
		return true
	}

	createContract := func() bool {
		// the max overhead of the pack frame
		// this is needed because the size of the contract pack is counted against the contract
		// maxContractMessageByteCount := ByteCount(256)

		effectiveContractTransferByteCount := ByteCount(float32(self.client.ContractManager().StandardContractTransferByteCount()) * self.sendBufferSettings.ContractFillFraction)
		if effectiveContractTransferByteCount < messageByteCount+self.sendBufferSettings.MinMessageByteCount /*+ maxContractMessageByteCount*/ {
			// this pack does not fit into a standard contract
			// TODO allow requesting larger contracts
			panic(fmt.Errorf("Message too large for contract. It can never be sent (%d).", messageByteCount))
		}

		setNextContract := func(contract *protocol.Contract) bool {
			nextSendContract, err := newSequenceContract(
				self.log,
				"s",
				contract,
				self.sendBufferSettings.MinMessageByteCount,
				self.sendBufferSettings.ContractFillFraction,
			)
			if err != nil {
				// malformed
				self.log.Errorf("[s]%s->%s...%s s(%s) exit next contract malformed error = %s\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId, err)
				return false
			}

			if _, ok := self.openSendContracts[nextSendContract.contractId]; ok {
				return false
			}

			// note `update(0)` will use `MinMessageByteCount` byte count
			// the min message byte count is used to avoid spam
			if nextSendContract.update(0) && nextSendContract.update(messageByteCount) {
				self.setContract(nextSendContract)

				// append the contract to the sequence
				self.sendWithSetContract(nil, func(error) {
					self.setContractAcked(nextSendContract, true)
				}, true, true, false)

				// FIXME
				self.log.Infof("[s]%s->%s...%s s(%s) contract set %s\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId, nextSendContract.contractId)

				return true

			} else {
				// this contract doesn't fit the message
				// the contract was requested with the correct size, so this is an error somewhere
				// just close it and let the platform time out the other side
				self.log.Errorf("[s]%s->%s...%s s(%s) contract too small %s\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId, nextSendContract.contractId)
				self.client.ContractManager().CloseContract(nextSendContract.contractId, 0, 0)
				return false
			}
		}

		nextContract := func(timeout time.Duration) bool {
			contractKey := ContractKey{
				Destination:         self.destination,
				IntermediaryIds:     self.intermediaryIds,
				CompanionContract:   self.companionContract,
				ForceStream:         self.forceStream,
				EncryptionRole:      self.encryptionRole,
				EncryptionCompanion: self.encryptionCompanion,
			}
			if contract := self.client.ContractManager().TakeContract(self.ctx, contractKey, timeout); contract != nil && setNextContract(contract) {
				self.contractSeqIndex += 1
				// async queue up the next contract
				self.client.ContractManager().CreateContract(
					contractKey,
					self.contractSeqIndex,
					ByteCount(32+float32(messageByteCount+self.sendBufferSettings.MinMessageByteCount)/self.sendBufferSettings.ContractFillFraction),
				)
				return true
			} else {
				return false
			}
		}
		traceNextContract := func(timeout time.Duration) bool {
			if self.log.V(2).Enabled() {
				return TraceWithReturn(
					fmt.Sprintf("[s]%s->%s...%s s(%s) next contract", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId),
					func() bool {
						return nextContract(timeout)
					},
				)
			} else {
				return nextContract(timeout)
			}
		}

		endTime := time.Now().Add(self.sendBufferSettings.CreateContractTimeout)

		if self.sendContract != nil {
			// there should be a queued up contract
			if traceNextContract(min(self.sendBufferSettings.CreateContractTimeout, self.sendBufferSettings.CreateContractRetryInterval)) {
				return true
			}
		}

		for {
			select {
			case <-self.ctx.Done():
				return false
			default:
			}

			timeout := endTime.Sub(time.Now())
			if timeout <= 0 {
				return false
			}

			// async queue up the next contract
			contractKey := ContractKey{
				Destination:         self.destination,
				IntermediaryIds:     self.intermediaryIds,
				CompanionContract:   self.companionContract,
				ForceStream:         self.forceStream,
				EncryptionRole:      self.encryptionRole,
				EncryptionCompanion: self.encryptionCompanion,
			}
			self.client.ContractManager().CreateContract(
				contractKey,
				self.contractSeqIndex,
				ByteCount(32+float32(messageByteCount+messageByteCount+self.sendBufferSettings.MinMessageByteCount)/self.sendBufferSettings.ContractFillFraction),
			)

			if traceNextContract(min(timeout, self.sendBufferSettings.CreateContractRetryInterval)) {
				return true
			}
		}
	}

	createStartTime := time.Now()
	var ok bool
	if self.log.V(2).Enabled() {
		ok = TraceWithReturn(
			fmt.Sprintf("[s]create contract c=%t %s->%s...%s s(%s)", self.companionContract, self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId),
			createContract,
		)
	} else {
		ok = createContract()
	}
	// surface slow contract acquisition at default verbosity. The send
	// sequence blocks here, so a slow create (e.g. a companion request that
	// cannot match an origin contract) stalls the entire sequence.
	if d := time.Since(createStartTime); 1*time.Second <= d {
		self.log.Infof("[s]contract wait %.1fs ok=%t c=%t %s->%s...%s s(%s)\n", d.Seconds(), ok, self.companionContract, self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId)
	}
	return ok
}

func (self *SendSequence) setContract(nextSendContract *sequenceContract) {
	// do not close the current contract unless it has no pending data
	// the contract is tracked in `openSendContracts` and will be closed on ack
	if self.sendContract != nil && self.sendContract.unackedByteCount == 0 {
		self.client.ContractManager().CloseContract(
			self.sendContract.contractId,
			self.sendContract.ackedByteCount,
			self.sendContract.unackedByteCount,
		)
		delete(self.openSendContracts, self.sendContract.contractId)
	}
	self.openSendContracts[nextSendContract.contractId] = nextSendContract
	self.sendContract = nextSendContract
	self.sendContractAcked = false
	nextSendContract.statsEntry = self.client.ContractManager().registerContractStats(
		nextSendContract.contractId,
		false,
		self.companionContract,
		nextSendContract.path,
		nextSendContract.transferByteCount,
	)
	// The contract carries the destination's `ProvideTlsCertificate`
	// commitment (possibly empty). Fold the chain into the session's
	// trusted-peer-cert set so the peer's TLS-handshake cert can be matched
	// against any cert version the destination has ever published — both
	// the cert in this contract and any cert in a previously seen contract.
	// An empty chain turns off verification entirely (the destination is
	// not committing to a TLS identity).
	//
	// In addition, contracts carry the destination's long-lived client
	// public identity key plus the destination's signature over the cert
	// chain by that key. Pass the public key to the session so it can
	// (a) verify the cert chain before trusting it (defeats a platform
	// MITM that substitutes the cert), and (b) verify the post-handshake
	// identity proof exchanged inside the per-peer TLS session (defeats
	// an active MITM that re-handshakes TLS on each leg).
	if self.session != nil {
		if 0 < len(nextSendContract.destinationClientPublicKey) {
			self.session.SetPeerClientPublicKey(ed25519.PublicKey(nextSendContract.destinationClientPublicKey))
		}
		self.session.AddTrustedPeerCertChain(
			nextSendContract.provideTlsCertificate,
			nextSendContract.destinationClientKeySignedTlsCertificate,
		)
	}
}

func (self *SendSequence) setContractAcked(nextSendContract *sequenceContract, ack bool) {
	if self.sendContract == nextSendContract {
		self.sendContractAcked = ack
	}
}

func (self *SendSequence) send(
	frame *protocol.Frame,
	ackCallback AckFunction,
	ack bool,
	forceUnwrapped bool,
) {
	self.sendWithSetContract(frame, ackCallback, ack, false, forceUnwrapped)
}

func (self *SendSequence) sendWithSetContract(
	frame *protocol.Frame,
	ackCallback AckFunction,
	ack bool,
	setContract bool,
	forceUnwrapped bool,
) {
	sendTime := time.Now()
	messageId := NewId()

	var contractId *Id
	if self.sendContract != nil {
		contractId = &self.sendContract.contractId

		if !self.sendContractAcked {
			// (see note above about contracts and nack)
			// send nack messages as ack until the send contract is acked
			// this avoid racing the messages with the contract
			ack = true
		}
	}

	var head bool
	var sequenceNumber uint64
	if ack {
		head = (0 == len(self.sendItems))
		sequenceNumber = self.nextSequenceNumber
		self.nextSequenceNumber += 1
	} else {
		head = false
		sequenceNumber = 0
	}

	var contractFrame *protocol.Frame
	if (head || setContract) && self.sendContract != nil {
		contractMessageBytes, _ := ProtoMarshal(self.sendContract.contract)
		contractFrame = &protocol.Frame{
			MessageType:  protocol.MessageType_TransferContract,
			MessageBytes: contractMessageBytes,
		}
		defer MessagePoolReturn(contractMessageBytes)
	}

	frames := []*protocol.Frame{}
	if frame != nil {
		frames = append(frames, frame)
	}
	defer func() {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
	}()

	// var path TransferPath
	// if self.sendContract == nil {
	// 	path = self.destination.AddSource(self.client.ClientId())
	// } else {
	// 	path = self.sendContract.path.LocalMask()
	// }
	path := self.destination.AddSource(self.client.ClientId())
	messageByteCount := MessageByteCount(frames)

	// Session role/companion stamping (applies to both encodings below):
	// A server-role sequence is the peer's EncryptedControl carrier. Stamp its
	// role on every pack — including the non-EC open/contract packs that carry no
	// EC frame to derive it from — so the receiver maps the whole sequence to one
	// complement session; otherwise the open pack splits into a separate receive
	// sequence and the handshake bytes (ServerHello, identity proof) gap forever.
	// Only the server role is marked: a client-role stream is the unencrypted
	// default, already the receiver's complement, so it stays off the wire.
	// Companion mirrors the role stamp (for either role): stamped only when true,
	// since false is the receiver's default and stays off the wire.

	var transferFrameBytes []byte
	if 2 <= self.sendBufferSettings.ProtocolVersion {
		// hand-rolled marshal of the hot TransferFrame{Pack}: wire-identical to
		// the proto structs in the legacy branch below (verified byte-for-byte in
		// frame_protobuf_test.go), without the intermediate Pack/TransferFrame/Tag/
		// TransferPath structs, the Id.Bytes() escapes, or reflection.
		spf := sendPackFrame{
			path:           path,
			messageId:      messageId,
			sequenceId:     self.sequenceId,
			sequenceNumber: sequenceNumber,
			head:           head,
			nack:           !ack,
			frames:         frames,
			contractFrame:  contractFrame,
			tagSendTime:    uint64(sendTime.UnixMilli()),
		}
		if !ack && contractId != nil {
			spf.contractId = contractId
		}
		if self.encryptionRole == sequenceTlsRoleServer {
			spf.sessionRole = self.encryptionRole.toProtobuf()
			spf.sessionRoleSet = true
		}
		if self.encryptionCompanion {
			spf.companion = true
		}
		transferFrameBytes = marshalSendPackTransferFrame(&spf)
	} else {
		// legacy (<v2) path: build and marshal via the proto structs.
		pack := &protocol.Pack{
			MessageId:      messageId.Bytes(),
			SequenceId:     self.sequenceId.Bytes(),
			SequenceNumber: sequenceNumber,
			Head:           head,
			Frames:         frames,
			ContractFrame:  contractFrame,
			Nack:           !ack,
			Tag:            self.rttWindow.OpenTag(),
		}
		if !ack && contractId != nil {
			pack.ContractId = contractId.Bytes()
		}
		packBytes, _ := ProtoMarshal(pack)
		defer MessagePoolReturn(packBytes)
		transferFrame := &protocol.TransferFrame{
			TransferPath: path.ToProtobuf(),
			Frame: &protocol.Frame{
				MessageType:  protocol.MessageType_TransferPack,
				MessageBytes: packBytes,
			},
		}
		if self.encryptionRole == sequenceTlsRoleServer {
			sessionRole := self.encryptionRole.toProtobuf()
			transferFrame.SessionRole = &sessionRole
		}
		if self.encryptionCompanion {
			sessionCompanion := true
			transferFrame.SessionCompanion = &sessionCompanion
		}
		transferFrameBytes, _ = ProtoMarshal(transferFrame)
	}

	item := &sendItem{
		transferItem: transferItem{
			messageId:        messageId,
			sequenceNumber:   sequenceNumber,
			messageByteCount: messageByteCount,
		},
		contractId:         contractId,
		sendTime:           sendTime,
		resendTime:         sendTime.Add(self.rttWindow.ScaledRtt()),
		sendCount:          1,
		head:               head,
		hasContractFrame:   (contractFrame != nil),
		transferFrameBytes: transferFrameBytes,
		ackCallback:        ackCallback,
		forceUnwrapped:     forceUnwrapped,
	}

	c := func() error {
		return self.writeMaybeWrappedBytes(item.transferFrameBytes, path, item.forceUnwrapped)
	}
	var err error
	if self.log.V(2).Enabled() {
		err = TraceWithReturn(
			fmt.Sprintf("[s]multi route write %s->%s...%s s(%s)", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId),
			c,
		)
	} else {
		err = c()
		if err != nil {
			if self.log.V(1).Enabled() {
				self.log.Infof("[s]drop = %s", err)
			}
		}
	}

	if ack {
		self.sendItems = append(self.sendItems, item)
		self.resendQueue.Add(item)
		// ignore the write error since the item will be resent
	} else {
		// immediately ack
		if err == nil {
			self.ackItem(item)
		} else {
			safeAck(item.ackCallback, err)
			item.messagePoolReturn()
		}
	}
}

func (self *SendSequence) setHead(item *sendItem) ([]byte, error) {
	if self.log.V(1).Enabled() {
		self.log.Infof("[s]set head %s->%s...%s s(%s)\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId)
	}

	var transferFrame protocol.TransferFrame
	err := ProtoUnmarshal(item.transferFrameBytes, &transferFrame)
	if err != nil {
		return nil, err
	}

	var pack *protocol.Pack
	if transferFrame.Pack != nil {
		pack = transferFrame.Pack
	} else {
		pack = &protocol.Pack{}
		err = ProtoUnmarshal(transferFrame.Frame.MessageBytes, pack)
		if err != nil {
			return nil, err
		}
	}

	pack.Head = true
	pack.Tag = self.rttWindow.OpenTag()
	// attach the contract frame to the head
	if item.contractId != nil && !item.hasContractFrame {
		sendContract := self.openSendContracts[*item.contractId]
		contractMessageBytes, _ := ProtoMarshal(sendContract.contract)
		pack.ContractFrame = &protocol.Frame{
			MessageType:  protocol.MessageType_TransferContract,
			MessageBytes: contractMessageBytes,
		}
		defer MessagePoolReturn(contractMessageBytes)
	}

	if transferFrame.Pack != nil {
		transferFrame.Pack = pack
	} else {
		packBytes, err := ProtoMarshal(pack)
		if err != nil {
			return nil, err
		}
		defer MessagePoolReturn(packBytes)
		transferFrame.Frame.MessageBytes = packBytes
	}

	transferFrameBytesWithHead, err := ProtoMarshal(&transferFrame)
	if err != nil {
		return nil, err
	}

	return transferFrameBytesWithHead, nil
}

/*
func (self *SendSequence) setTag(item *sendItem) ([]byte, error) {
	self.log.V(1).Infof("[s]set tag %s->%s...%s s(%s)\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId)

	var transferFrame protocol.TransferFrame
	err := proto.Unmarshal(item.transferFrameBytes, &transferFrame)
	if err != nil {
		return nil, err
	}

	var pack protocol.Pack
	err = proto.Unmarshal(transferFrame.Frame.MessageBytes, &pack)
	if err != nil {
		return nil, err
	}

	pack.Tag = self.rttWindow.OpenTag()

	packBytes, err := proto.Marshal(&pack)
	if err != nil {
		return nil, err
	}
	transferFrame.Frame.MessageBytes = packBytes

	transferFrameBytesWithTag, err := proto.Marshal(&transferFrame)
	if err != nil {
		return nil, err
	}

	return transferFrameBytesWithTag, nil
}
*/

func (self *SendSequence) receiveAck(messageId Id, selective bool, tag *protocol.Tag) {
	item := self.resendQueue.GetByMessageId(messageId)
	if item == nil {
		if self.log.V(1).Enabled() {
			self.log.Infof("[s]ack miss %s->%s...%s s(%s)\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId)
		}
		// message not pending ack
		return
	}

	if tag != nil {
		self.rttWindow.CloseTag(tag)
	}

	if selective {
		if self.log.V(1).Enabled() {
			self.log.Infof("[s]ack selective %s->%s...%s s(%s)\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId)
		}
		removed := self.resendQueue.RemoveByMessageId(messageId)
		if removed == nil {
			panic(errors.New("Missing item"))
		}
		// refresh sendTime so the ack-timeout deadline includes the selective-ack window
		item.sendTime = time.Now()
		item.resendTime = item.sendTime.Add(self.sendBufferSettings.SelectiveAckTimeout)
		self.resendQueue.Add(item)
		return
	}

	if self.log.V(1).Enabled() {
		self.log.Infof("[s]ack %d %s->%s...%s s(%s)\n", item.sequenceNumber, self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId)
	}

	// acks are cumulative
	// implicitly ack all earlier items in the sequence
	i := 0
	for ; i < len(self.sendItems); i += 1 {
		implicitItem := self.sendItems[i]
		if item.sequenceNumber < implicitItem.sequenceNumber {
			if self.log.V(2).Enabled() {
				self.log.Infof("[s]ack %d <> %d/%d (stop) %s->%s...%s s(%s)\n", item.sequenceNumber, implicitItem.sequenceNumber, self.nextSequenceNumber-1, self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId)
			}
			break
		}

		var a int
		var b ByteCount
		if self.log.V(2).Enabled() {
			a, b = self.resendQueue.QueueSize()
		}

		// self.ackedSequenceNumbers[implicitItem.sequenceNumber] = true
		removed := self.resendQueue.RemoveByMessageId(implicitItem.messageId)
		if removed == nil {
			panic(errors.New("Missing item"))
		}

		self.ackItem(implicitItem)
		self.sendItems[i] = nil

		if self.log.V(2).Enabled() {
			c, d := self.resendQueue.QueueSize()
			self.log.Infof("[s]ack %d <> %d/%d (pass %d->%d %dB->%dB) %s->%s...%s s(%s)\n", item.sequenceNumber, implicitItem.sequenceNumber, self.nextSequenceNumber-1, a, c, b, d, self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId)
		}
	}
	self.sendItems = self.sendItems[i:]
	if self.log.V(2).Enabled() {
		a, b := self.resendQueue.QueueSize()
		self.log.Infof("[s]ack %d/%d (stop %d %dB %d) %s->%s...%s s(%s)\n", item.sequenceNumber, self.nextSequenceNumber-1, a, b, len(self.sendItems), self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId)
	}
}

func (self *SendSequence) ackItem(item *sendItem) {
	if item.contractId != nil {
		if itemSendContract, ok := self.openSendContracts[*item.contractId]; ok {
			itemSendContract.ack(item.messageByteCount)
			// not current and closed
			if self.sendContract != itemSendContract && itemSendContract.unackedByteCount == 0 {
				self.client.ContractManager().CloseContract(
					itemSendContract.contractId,
					itemSendContract.ackedByteCount,
					itemSendContract.unackedByteCount,
				)
				delete(self.openSendContracts, itemSendContract.contractId)
			}
		}
	}
	safeAck(item.ackCallback, nil)
	// MessagePoolReturn(item.transferFrameBytes)
	// for _, frame := range item.frames {
	// 	MessagePoolReturn(frame.MessageBytes)
	// }
	item.messagePoolReturn()
}

// writeMaybeWrappedBytes writes `transferFrameBytes` through the contract
// multi-route writer. When the per-peer session has a cipher, the bytes are
// outer-wrapped as `TransferFrame{TransferPath, encryptedTransferFrame:
// <ciphertext>}` before being written. Encryption is a binary property of
// the session: cipher set → wrap; cipher nil → pass-through. `path` is the
// wire TransferPath the outer wrap reproduces so forwarders see the same
// routing path either way.
//
// `forceUnwrapped` pins this frame to plaintext regardless of cipher
// state. TLS handshake EncryptedControl frames use this to keep the
// handshake bootstrap legible to the peer — including on retransmit,
// where the local cipher may have become available after the original
// send but the peer has not yet completed its half of the handshake.
//
// Before wrapping, the peer's TLS certificate is verified against the
// active contract's `ProvideTlsCertificate` commitment. A mismatch is a
// loud error: the frame is dropped (the SendSequence will retry, and
// eventually time out, rather than transmit application data sealed under
// the wrong identity).
func (self *SendSequence) writeMaybeWrappedBytes(transferFrameBytes []byte, path TransferPath, forceUnwrapped bool) error {
	writer := self.openContractMultiRouteWriter()
	var cipher *sequenceCipher
	if self.session != nil && !forceUnwrapped {
		cipher = self.session.Cipher()
	}
	if cipher == nil {
		// guard the V(2) diagnostic: this is the per-packet plaintext write path,
		// and the disabled-level call would still box ClientTag/DestinationId/
		// StreamId/len into []any on the heap every packet.
		if self.log.V(2).Enabled() {
			self.log.Infof(
				"[s]%s->%s s(%s) write plaintext %d bytes (forceUnwrapped=%t, session=%t, cipher=nil)\n",
				self.client.ClientTag(),
				self.destination.DestinationId,
				self.destination.StreamId,
				len(transferFrameBytes),
				forceUnwrapped,
				self.session != nil,
			)
		}
		bytes := transferFrameBytes
		if DebugTransferCopyOnWrite {
			bytes = MessagePoolCopy(transferFrameBytes)
			// the copy is scoped to this write; the item's transferFrameBytes stays
			// owned by the sequence
			defer MessagePoolReturn(bytes)
		}
		shared := MessagePoolShareReadOnly(bytes)
		err := writer.Write(self.ctx, shared, self.sendBufferSettings.WriteTimeout)
		if err != nil {
			// on failure (abort/timeout) no route consumer took the message, so
			// ownership stays here: undo the consumer's share or the buffer can
			// never reach zero references and silently leaves the pool
			MessagePoolReturn(shared)
		}
		return err
	}
	if err := self.verifyPeerCertAgainstContract(); err != nil {
		return err
	}
	ciphertext, err := cipher.Seal(transferFrameBytes)
	if err != nil {
		return fmt.Errorf("outer wrap seal: %w", err)
	}
	// Carry the wrapping session's role + companion as the destination's
	// decrypt hint; the destination routes to its complement role / matching
	// companion session.
	wrapped, err := buildEncryptedOuterFrameBytes(path, ciphertext, self.session.role.toProtobuf(), self.session.companion)
	if err != nil {
		return fmt.Errorf("outer wrap marshal: %w", err)
	}
	// guard the V(2) diagnostic: this is the per-packet wrapped write path; see
	// the plaintext branch above for why the disabled-level call still allocates.
	if self.log.V(2).Enabled() {
		self.log.Infof(
			"[s]%s->%s s(%s) write wrapped %d -> %d bytes\n",
			self.client.ClientTag(),
			self.destination.DestinationId,
			self.destination.StreamId,
			len(transferFrameBytes), len(wrapped),
		)
	}
	defer MessagePoolReturn(wrapped)
	shared := MessagePoolShareReadOnly(wrapped)
	err = writer.Write(self.ctx, shared, self.sendBufferSettings.WriteTimeout)
	if err != nil {
		// see the plaintext branch: a failed write leaves ownership here
		MessagePoolReturn(shared)
	}
	return err
}

// verifyPeerCertAgainstContract checks (and caches) that the peer's TLS cert
// matches a chain the destination committed to in some contract this session
// has seen. The trusted set is maintained by `AddTrustedPeerCertChain` (from
// `setContract`); every cert the peer has published is acceptable, so rotation
// is tolerated without breaking in-flight sessions. Skipped when:
//
//   - this is a companion-mode reply: the companion sender re-uses the session
//     cipher established by the original direction's handshake.
//   - the trusted set is empty (no contract seen, or all carried an empty
//     `ProvideTlsCertificate`): skip without latching, so a later contract with
//     a cert re-arms verification.
//
// Once matched, the result is cached and not re-run for this session.
func (self *SendSequence) verifyPeerCertAgainstContract() error {
	if self.session == nil {
		return nil
	}
	if self.companionContract {
		if self.log.V(1).Enabled() {
			self.log.Infof(
				"[s]%s->%s s(%s) companion reply: reusing per-peer session cipher; skipping cert verification\n",
				self.client.ClientTag(),
				self.destination.DestinationId,
				self.destination.StreamId,
			)
		}
		return nil
	}
	verified, noCommitment := self.session.CertVerificationState()
	if verified || noCommitment {
		return nil
	}
	expected := self.session.trustedPeerCertSnapshot()
	// V(2) diagnostic: verify against the established epoch (whose cipher seals
	// this frame), not the in-flight currentEpoch() whose ConnectionState() blocks
	// on the running handshake. Logged so reaching this path (trusted set armed) is
	// observable.
	if self.log.V(2).Enabled() {
		self.log.Infof(
			"[s][cert-verify]%s->%s s(%s) verifying established-epoch peer certs (non-blocking); trustedSet=%d companion=%t\n",
			self.client.ClientTag(),
			self.destination.DestinationId,
			self.destination.StreamId,
			len(expected),
			self.companionContract,
		)
	}
	peerCerts := self.session.establishedPeerCertificates()
	ok, err := verifyPeerCertificateAgainstContract(peerCerts, expected)
	if err != nil {
		self.log.Errorf(
			"[s]%s->%s s(%s) sequence TLS cert verification failed: %s (peer presented %d cert(s); trusted set has %d)\n",
			self.client.ClientTag(),
			self.destination.DestinationId,
			self.destination.StreamId,
			err,
			len(peerCerts),
			len(expected),
		)
		return fmt.Errorf("sequence TLS cert verification failed: %w", err)
	}
	if !ok {
		self.log.Errorf(
			"[s]%s->%s s(%s) sequence TLS cert mismatch (peer presented %d cert(s); trusted set has %d)\n",
			self.client.ClientTag(),
			self.destination.DestinationId,
			self.destination.StreamId,
			len(peerCerts),
			len(expected),
		)
		return errors.New("sequence TLS cert verification failed")
	}
	self.session.MarkCertVerified()
	return nil
}

func (self *SendSequence) openContractMultiRouteWriter() MultiRouteWriter {
	var destination TransferPath
	if self.sendContract == nil {
		destination = self.destination
	} else {
		destination = self.sendContract.path.DestinationMask()
	}
	if self.contractMultiRouteWriter == nil || self.contractMultiRouteWriterDestination != destination {
		if self.contractMultiRouteWriter != nil {
			self.client.RouteManager().CloseMultiRouteWriter(self.contractMultiRouteWriter)
		}
		self.contractMultiRouteWriter = self.client.RouteManager().OpenMultiRouteWriter(destination)
		self.contractMultiRouteWriterDestination = destination

		// associate the destination with this sequence to receive acks
		self.sendBuffer.AssociateDestination(self, destination.LocalMask())
	}
	return self.contractMultiRouteWriter
}

func (self *SendSequence) closeContractMultiRouteWriter() {
	if self.contractMultiRouteWriter != nil {
		self.client.RouteManager().CloseMultiRouteWriter(self.contractMultiRouteWriter)
		self.contractMultiRouteWriter = nil
		self.contractMultiRouteWriterDestination = TransferPath{}
	}
}

func (self *SendSequence) Close() {
	self.cancel()

	func() {
		self.packMutex.Lock()
		defer self.packMutex.Unlock()
		close(self.packs)
	}()

	func() {
		self.ackMutex.Lock()
		defer self.ackMutex.Unlock()
		close(self.acks)
	}()

	// drain the channel
	func() {
		for {
			select {
			case sendPack, ok := <-self.packs:
				if !ok {
					return
				}
				safeAck(sendPack.AckCallback, errors.New("Send sequence closed."))
				MessagePoolReturn(sendPack.Frame.MessageBytes)
			default:
				return
			}
		}
	}()
}

func (self *SendSequence) Cancel() {
	self.cancel()
}

type sendItem struct {
	transferItem

	contractId         *Id
	head               bool
	hasContractFrame   bool
	sendTime           time.Time
	resendTime         time.Time
	sendCount          int
	transferFrameBytes []byte
	ackCallback        AckFunction
	// forceUnwrapped pins this item to plaintext on every (re)send, so the
	// outer wrap is skipped even if the per-peer cipher becomes available
	// between the initial send and a retransmit.
	forceUnwrapped bool

	// messageType protocol.MessageType
}

func (self *sendItem) messagePoolReturn() {
	MessagePoolReturn(self.transferFrameBytes)
}

// the resend queue accounts items by their actual transfer frame size rather
// than the app message size (`messageByteCount`, which still drives contract
// accounting). For small messages the frame overhead dominates, and
// content-based accounting would let `ResendQueueMaxByteCount` admit tens of
// thousands of in-flight messages, far more than the path can ack inside the
// resend timeout under load.
// note the resend loop only mutates `transferFrameBytes` (set head) while the
// item is removed from the queue, so the add/remove accounting stays consistent.
func (self *sendItem) MessageByteCount() ByteCount {
	return ByteCount(len(self.transferFrameBytes))
}

// a send event queue which is the union of:
// - resend times
// - ack timeouts
type resendQueue = transferQueue[*sendItem]

func newResendQueue(budget *TransferMemoryBudget, minByteCount ByteCount) *resendQueue {
	queue := newTransferQueue[*sendItem](func(a *sendItem, b *sendItem) int {
		if a.resendTime.Before(b.resendTime) {
			return -1
		} else if b.resendTime.Before(a.resendTime) {
			return 1
		} else {
			return 0
		}
	})
	queue.setBudget(budget, minByteCount)
	return queue
}

type ReceiveBufferSettings struct {
	GapTimeout  time.Duration
	IdleTimeout time.Duration

	SequenceBufferSize int
	// AckBufferSize int

	AckCompressTimeout time.Duration

	MinMessageByteCount ByteCount

	// min number of resends before checking abuse
	// ResendAbuseThreshold int
	// max legit fraction of sends that are resends
	// ResendAbuseMultiple float64

	MaxPeerAuditDuration time.Duration

	WriteTimeout time.Duration

	ReceiveQueueMaxByteCount ByteCount
	// ReceiveQueueMinByteCount is the guaranteed per-sequence floor when
	// `ReceiveQueueBudget` is set (see `ResendQueueMinByteCount`)
	ReceiveQueueMinByteCount ByteCount
	// ReceiveQueueBudget, when set, is a byte budget shared across sequences
	// (see `ResendQueueBudget`)
	ReceiveQueueBudget *TransferMemoryBudget

	// whether to allow nacks without a contract_id
	AllowLegacyNack bool

	MaxOpenReceiveContract int

	ProtocolVersion int
}

type receiveSequenceId struct {
	Source     TransferPath
	SequenceId Id
	// EncryptionRole separates the inbound streams that map to our server
	// session (normal peer data — the default) from those that map to our
	// client session (the peer's EncryptedControl carrier + server replies).
	// SequenceId alone is already unique; the role makes the owning session
	// explicit and keys the per-role head tracking.
	EncryptionRole sequenceTlsRole
	// EncryptionCompanion separates the inbound streams owned by the companion
	// session from those owned by the regular session of the same role, so a
	// peer running both modes maps each stream to the right per-peer session.
	EncryptionCompanion bool
}

// receiveSequenceHeadKey identifies the head (newest) receive sequence for a
// given (source, companion, role). Supersession — drop-older / upgrade-newer
// by SequenceId — happens within a single (source, companion, role): the
// peer's client and server streams, and its companion and regular streams,
// reform independently, so they must not supersede each other.
type receiveSequenceHeadKey struct {
	Source              TransferPath
	EncryptionRole      sequenceTlsRole
	EncryptionCompanion bool
}

type ReceiveBuffer struct {
	ctx    context.Context
	client *Client
	log    Logger

	receiveBufferSettings *ReceiveBufferSettings

	mutex sync.Mutex
	// the head receive sequences
	// source id -> receive sequence
	receiveSequences       map[receiveSequenceId]*ReceiveSequence
	headReceiveSequenceIds map[receiveSequenceHeadKey]receiveSequenceId
}

func NewReceiveBuffer(ctx context.Context,
	client *Client,
	receiveBufferSettings *ReceiveBufferSettings) *ReceiveBuffer {
	return &ReceiveBuffer{
		ctx:                    ctx,
		client:                 client,
		log:                    client.log,
		receiveBufferSettings:  receiveBufferSettings,
		receiveSequences:       map[receiveSequenceId]*ReceiveSequence{},
		headReceiveSequenceIds: map[receiveSequenceHeadKey]receiveSequenceId{},
	}
}

func (self *ReceiveBuffer) Pack(receivePack *ReceivePack, timeout time.Duration) (bool, error) {
	receiveSequenceId := receiveSequenceId{
		Source:              receivePack.Source,
		SequenceId:          receivePack.SequenceId,
		EncryptionRole:      receivePack.EncryptionRole,
		EncryptionCompanion: receivePack.EncryptionCompanion,
	}
	// Head/supersession is tracked per (source, companion, role): the peer's
	// client and server streams, and its companion and regular streams, reform
	// independently and must not supersede each other.
	headKey := receiveSequenceHeadKey{
		Source:              receiveSequenceId.Source,
		EncryptionRole:      receiveSequenceId.EncryptionRole,
		EncryptionCompanion: receiveSequenceId.EncryptionCompanion,
	}

	initReceiveSequence := func(skip *ReceiveSequence) *ReceiveSequence {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		receiveSequence, ok := self.receiveSequences[receiveSequenceId]
		if ok {
			if skip == nil || skip != receiveSequence {
				return receiveSequence
			} else {
				receiveSequence.Cancel()
				// delete(self.receiveSequences, receiveSequenceId)
				// delete(self.headSequenceIds, headKey)
			}
			if headReceiveSequenceId := self.headReceiveSequenceIds[headKey]; headReceiveSequenceId != receiveSequenceId {
				panic(fmt.Errorf("[r]incorrect head sequence %s != %s\n", headReceiveSequenceId.SequenceId, receivePack.SequenceId))
			}
		} else if headReceiveSequenceId, ok := self.headReceiveSequenceIds[headKey]; ok {
			if receivePack.SequenceId.LessThan(headReceiveSequenceId.SequenceId) {
				// drop older sequences for source
				// this case happens when a client closes a sequence, then opens a new one,
				// before messages from the first are received
				if self.log.V(2).Enabled() {
					self.log.Infof("[r]drop older sequence %s < %s\n", receivePack.SequenceId, headReceiveSequenceId.SequenceId)
				}
				MessagePoolReturn(receivePack.TransferFrameBytes)
				return nil
			} else {
				// newer sequence for source
				if headReceiveSequenceId.SequenceId == receivePack.SequenceId {
					panic(fmt.Errorf("[r]upgrade older sequence %s = %s\n", headReceiveSequenceId.SequenceId, receivePack.SequenceId))
				}
				if self.log.V(2).Enabled() {
					self.log.Infof("[r]upgrade older sequence %s < %s\n", headReceiveSequenceId.SequenceId, receivePack.SequenceId)
				}
				headReceiveSequence := self.receiveSequences[headReceiveSequenceId]
				headReceiveSequence.Cancel()
				// wait for exit to ensure receives are correctly ordered across sequence versions
				headReceiveSequence.WaitForExit()
				delete(self.receiveSequences, headReceiveSequenceId)
			}
		}

		if self.log.V(2).Enabled() {
			self.log.Infof("[r]new sequence %s\n", receivePack.SequenceId)
		}

		receiveSequence = NewReceiveSequence(
			self.ctx,
			self.client,
			receivePack.Source,
			receivePack.SequenceId,
			receivePack.EncryptionRole,
			receivePack.EncryptionCompanion,
			self.receiveBufferSettings,
		)
		self.receiveSequences[receiveSequenceId] = receiveSequence
		self.headReceiveSequenceIds[headKey] = receiveSequenceId
		go HandleError(func() {
			defer func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()
				receiveSequence.Close()
				// clean up
				if receiveSequence == self.receiveSequences[receiveSequenceId] {
					delete(self.receiveSequences, receiveSequenceId)
					// `headKey`/`receiveSequenceId` are values (no pointer to receivePack)
					delete(self.headReceiveSequenceIds, headKey)
				}
			}()
			receiveSequence.Run()
		})
		return receiveSequence
	}

	var receiveSequence *ReceiveSequence
	var success bool
	var err error
	for i := 0; i < 2; i += 1 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		default:
		}
		receiveSequence = initReceiveSequence(receiveSequence)
		if receiveSequence == nil {
			// drop
			return true, nil
		}
		if success, err = receiveSequence.Pack(receivePack, timeout); err == nil {
			return success, nil
		}
		// sequence closed
	}
	return success, err
}

func (self *ReceiveBuffer) ReceiveQueueSizeAndMessageTypes(source TransferPath, sequenceId Id) (int, ByteCount, []protocol.MessageType) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// SequenceId already uniquely identifies the sequence; the caller does not
	// know the encryption role or companion, so check every per-(role,companion)
	// key.
	for _, role := range []sequenceTlsRole{sequenceTlsRoleClient, sequenceTlsRoleServer} {
		for _, companion := range []bool{false, true} {
			receiveSequenceId := receiveSequenceId{
				Source:              source,
				SequenceId:          sequenceId,
				EncryptionRole:      role,
				EncryptionCompanion: companion,
			}
			if receiveSequence, ok := self.receiveSequences[receiveSequenceId]; ok {
				return receiveSequence.ReceiveQueueSizeAndMessageTypes()
			}
		}
	}
	return 0, 0, nil
}

func (self *ReceiveBuffer) Close() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// cancel all open sequences
	// the control of the sequence will close it
	for _, receiveSequence := range self.receiveSequences {
		receiveSequence.Cancel()
	}
}

func (self *ReceiveBuffer) Cancel() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// cancel all open sequences
	for _, receiveSequence := range self.receiveSequences {
		receiveSequence.Cancel()
	}
}

func (self *ReceiveBuffer) Flush() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// cancel all open sequences
	for _, receiveSequence := range self.receiveSequences {
		// if !receiveSequenceId.Source.IsControlSource() {
		receiveSequence.Cancel()
		// }
	}
}

type ReceiveSequence struct {
	ctx    context.Context
	cancel context.CancelFunc

	client *Client
	log    Logger

	source     TransferPath
	sequenceId Id
	// encryptionRole is the local per-peer session role that owns this
	// inbound stream (complement of the sender's role): server for normal
	// peer data (the default), client for the peer's EncryptedControl
	// carrier + server replies.
	encryptionRole sequenceTlsRole
	// encryptionCompanion is the per-peer session identity companion that owns
	// this inbound stream (not complemented); with encryptionRole it selects
	// which session the sequence holds.
	encryptionCompanion bool

	receiveBufferSettings *ReceiveBufferSettings

	openReceiveContracts map[Id]*sequenceContract
	receiveContract      *sequenceContract

	packMutex sync.Mutex
	packs     chan *ReceivePack

	receiveQueue       *receiveQueue
	nextSequenceNumber uint64

	idleCondition *IdleCondition

	peerAudit *SequencePeerAudit

	ackWindow *sequenceAckWindow

	exit chan struct{}

	// session is the per-peer TLS session that decrypts this inbound stream,
	// of role `encryptionRole` (the complement of the sender's role).
	// Acquired from the `EncryptionSessionManager` at construction without
	// starting a handshake — a ReceiveSequence follows the peer's handshake,
	// it never initiates one. Holding it keeps the session (and its cipher)
	// alive for the stream's lifetime; released when the sequence terminates.
	// Nil when encryption is disabled or this is control-plane traffic.
	session *peerEncryptionSession
}

func NewReceiveSequence(
	ctx context.Context,
	client *Client,
	source TransferPath,
	sequenceId Id,
	encryptionRole sequenceTlsRole,
	encryptionCompanion bool,
	receiveBufferSettings *ReceiveBufferSettings) *ReceiveSequence {
	cancelCtx, cancel := context.WithCancel(ctx)
	seq := &ReceiveSequence{
		ctx:                   cancelCtx,
		cancel:                cancel,
		client:                client,
		log:                   client.log,
		source:                source,
		sequenceId:            sequenceId,
		encryptionRole:        encryptionRole,
		encryptionCompanion:   encryptionCompanion,
		receiveBufferSettings: receiveBufferSettings,
		openReceiveContracts:  map[Id]*sequenceContract{},
		receiveContract:       nil,
		packs:                 make(chan *ReceivePack, receiveBufferSettings.SequenceBufferSize),
		receiveQueue:          newReceiveQueue(receiveBufferSettings.ReceiveQueueBudget, receiveBufferSettings.ReceiveQueueMinByteCount),
		nextSequenceNumber:    0,
		idleCondition:         NewIdleCondition(),
		ackWindow:             newSequenceAckWindow(),
		exit:                  make(chan struct{}),
	}
	// Never encrypt control-plane traffic. A ReceiveSequence's data source is
	// the peer (source.SourceId) and its destination is always this client
	// (client.ClientId()); when `ReceiveNoSession` holds for either endpoint,
	// no session is acquired and inbound traffic is taken in plaintext.
	if client != nil && client.encryptionSessionManager != nil &&
		!client.encryptionSessionManager.ReceiveNoSession(source.SourceId) {
		seq.session = client.encryptionSessionManager.Acquire(source.SourceId, encryptionRole, encryptionCompanion)
	}
	return seq
}

func (self *ReceiveSequence) ReceiveQueueSizeAndMessageTypes() (int, ByteCount, []protocol.MessageType) {
	unpackMessageTypes := func(item *receiveItem) any {
		var messageTypes []protocol.MessageType
		var transferFrame protocol.TransferFrame
		err := proto.Unmarshal(item.transferFrameBytes, &transferFrame)
		if err == nil && transferFrame.Pack != nil {
			for _, frame := range transferFrame.Pack.Frames {
				messageTypes = append(messageTypes, frame.MessageType)
			}
		}
		return messageTypes
	}
	count, byteSize, summary := self.receiveQueue.QueueSizeAndSummary(unpackMessageTypes)
	var messageTypes []protocol.MessageType
	for _, summaryMessageTypes := range summary {
		messageTypes = append(messageTypes, summaryMessageTypes.([]protocol.MessageType)...)
	}
	return count, byteSize, messageTypes
}

// success, error
func (self *ReceiveSequence) Pack(receivePack *ReceivePack, timeout time.Duration) (bool, error) {
	self.packMutex.Lock()
	defer self.packMutex.Unlock()

	select {
	case <-self.ctx.Done():
		return false, errors.New("Done.")
	default:
	}

	if !self.idleCondition.UpdateOpen() {
		return false, errors.New("Done.")
	}
	defer self.idleCondition.UpdateClose()

	// fast path without arming a timer
	select {
	case self.packs <- receivePack:
		return true, nil
	default:
	}

	if timeout < 0 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.packs <- receivePack:
			return true, nil
		}
	} else if timeout == 0 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.packs <- receivePack:
			return true, nil
		default:
			return false, nil
		}
	} else {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.packs <- receivePack:
			return true, nil
		case <-time.After(timeout):
			return false, nil
		}
	}
}

func (self *ReceiveSequence) Run() {
	defer func() {
		if r := recover(); r != nil {
			self.log.Errorf("[r]%s<-%s s(%s) abnormal exit =  %s\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId, r)
			panic(r)
		}
	}()
	defer func() {
		self.cancel()

		// close previous contracts and checkpoint the current contract
		for _, receiveContract := range self.openReceiveContracts {
			if self.receiveContract != receiveContract {
				if receiveContract.unackedByteCount != 0 {
					self.log.Infof("[r]%s<-%s s(%s) close contract with unacked =  %d\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId, receiveContract.unackedByteCount)
				}
				self.client.ContractManager().CloseContract(
					receiveContract.contractId,
					receiveContract.ackedByteCount,
					receiveContract.unackedByteCount,
				)
			}
		}
		if self.receiveContract != nil {
			// the sender may send again with this contract (set as head)
			// checkpoint the contract but do not close it
			if self.receiveContract.unackedByteCount != 0 {
				self.log.Infof("[r]%s<-%s s(%s) checkpoint contract with unacked =  %d\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId, self.receiveContract.unackedByteCount)
			}
			self.client.ContractManager().CheckpointContract(
				self.receiveContract.contractId,
				self.receiveContract.ackedByteCount,
				self.receiveContract.unackedByteCount,
			)
		}

		// drain the buffer, releasing any borrowed budget
		for _, item := range self.receiveQueue.Clear() {
			self.peerAudit.Update(func(a *PeerAudit) {
				a.discard(item.messageByteCount)
			})
			// MessagePoolReturn(item.transferFrameBytes)
			item.messagePoolReturn()
		}

		self.peerAudit.Complete()

		if self.session != nil {
			self.session.Release()
		}

		close(self.exit)
	}()

	self.peerAudit = NewSequencePeerAudit(
		self.client,
		self.source,
		self.receiveBufferSettings.MaxPeerAuditDuration,
	)

	// compress and send acks
	go HandleError(func() {
		defer self.cancel()

		multiRouteWriter := self.client.RouteManager().OpenMultiRouteWriter(self.source.Reverse())
		defer self.client.RouteManager().CloseMultiRouteWriter(multiRouteWriter)

		writeAck := func(sendAck *sequenceAck) {
			path := self.source.Reverse().AddSource(self.client.ClientId())

			var transferFrameBytes []byte
			if 2 <= self.receiveBufferSettings.ProtocolVersion {
				// hand-rolled marshal of the hot Ack TransferFrame; wire-identical
				// to the proto structs in the legacy branch (see frame_protobuf_test.go).
				saf := sendAckFrame{
					path:       path,
					messageId:  sendAck.messageId,
					sequenceId: self.sequenceId,
					selective:  sendAck.selective,
					tag:        sendAck.tag,
				}
				transferFrameBytes = marshalSendAckTransferFrame(&saf)
			} else {
				ack := &protocol.Ack{
					MessageId:  sendAck.messageId.Bytes(),
					SequenceId: self.sequenceId.Bytes(),
					Selective:  sendAck.selective,
					Tag:        sendAck.tag,
				}
				ackBytes, _ := ProtoMarshal(ack)
				defer MessagePoolReturn(ackBytes)
				transferFrame := &protocol.TransferFrame{
					TransferPath: path.ToProtobuf(),
					Frame: &protocol.Frame{
						MessageType:  protocol.MessageType_TransferAck,
						MessageBytes: ackBytes,
					},
				}
				transferFrameBytes, _ = ProtoMarshal(transferFrame)
			}
			defer MessagePoolReturn(transferFrameBytes)
			c := func() error {
				// outer-wrap the ack TransferFrame with the per-peer
				// session cipher when available. Mirror the wrap state
				// of the acked pack: if any pack covered by this ack
				// arrived plaintext, send the ack plaintext too — the
				// sender's cipher may not yet be established (it sent
				// plaintext because it had no cipher at send time), so
				// a wrapped ack would be unreadable on arrival.
				var cipher *sequenceCipher
				if self.session != nil && !sendAck.unwrapped {
					cipher = self.session.Cipher()
				}
				if cipher == nil {
					shared := MessagePoolShareReadOnly(transferFrameBytes)
					writeErr := multiRouteWriter.Write(
						self.ctx,
						shared,
						self.receiveBufferSettings.WriteTimeout,
					)
					if writeErr != nil {
						// a failed write leaves ownership here: undo the consumer's share
						MessagePoolReturn(shared)
					}
					return writeErr
				}
				ciphertext, sealErr := cipher.Seal(transferFrameBytes)
				if sealErr != nil {
					return fmt.Errorf("ack outer wrap seal: %w", sealErr)
				}
				// Carry our receive session's role + companion as the peer's
				// decrypt hint (it routes to the complement of our role / the
				// matching companion on its side).
				wrapped, marshalErr := buildEncryptedOuterFrameBytes(path, ciphertext, self.session.role.toProtobuf(), self.session.companion)
				if marshalErr != nil {
					return fmt.Errorf("ack outer wrap marshal: %w", marshalErr)
				}
				defer MessagePoolReturn(wrapped)
				shared := MessagePoolShareReadOnly(wrapped)
				writeErr := multiRouteWriter.Write(
					self.ctx,
					shared,
					self.receiveBufferSettings.WriteTimeout,
				)
				if writeErr != nil {
					// a failed write leaves ownership here: undo the consumer's share
					MessagePoolReturn(shared)
				}
				return writeErr
			}
			if self.log.V(2).Enabled() {
				TraceWithReturn(
					fmt.Sprintf(
						"[r]multi route write (ack %d) %s->%s s(%s)",
						sendAck.sequenceNumber,
						self.client.ClientTag(),
						self.source.SourceId,
						self.source.StreamId,
					),
					c,
				)
			} else {
				err := c()
				if err != nil {
					self.log.Infof("[r]drop = %s", err)
				}
			}
		}

		// reusable ack-compress timer (avoids a per-iteration time.After alloc on
		// the ack hot path). created already-fired; Reset before the blocking
		// select arms it (go1.23+ delivers no stale fire after Reset).
		ackCompressTimer := time.NewTimer(0)
		defer ackCompressTimer.Stop()

		for {
			select {
			case <-self.ctx.Done():
				return
			default:
			}

			ackSnapshot := self.ackWindow.Snapshot(false)
			if ackSnapshot.ackUpdateCount == 0 && len(ackSnapshot.selectiveAcks) == 0 {
				// wait for one ack
				select {
				case <-self.ctx.Done():
					return
				case <-ackSnapshot.ackNotify:
				}
			}

			if 0 < self.receiveBufferSettings.AckCompressTimeout {
				ackCompressTimer.Reset(self.receiveBufferSettings.AckCompressTimeout)
				select {
				case <-self.ctx.Done():
					return
				case <-ackCompressTimer.C:
				}
			}

			ackSnapshot = self.ackWindow.Snapshot(true)
			if 0 < ackSnapshot.ackUpdateCount {
				writeAck(ackSnapshot.headAck)
			}
			for messageId, ack := range ackSnapshot.selectiveAcks {
				writeAck(&sequenceAck{
					messageId:      messageId,
					sequenceNumber: ack.sequenceNumber,
					selective:      true,
					tag:            ack.tag,
					unwrapped:      ack.unwrapped,
				})
			}
		}
	}, self.cancel)

	// reusable idle/gap timer (avoids a per-iteration time.After alloc on the
	// receive hot path). created already-fired; Reset before the blocking select
	// arms it (go1.23+ delivers no stale fire after Reset).
	idleTimer := time.NewTimer(0)
	defer idleTimer.Stop()

	for {
		receiveTime := time.Now()
		var timeout time.Duration

		if queueSize, _ := self.receiveQueue.QueueSize(); 0 == queueSize {
			timeout = self.receiveBufferSettings.IdleTimeout
		} else {
			timeout = self.receiveBufferSettings.GapTimeout
			for {
				item := self.receiveQueue.PeekFirst()
				if item == nil {
					break
				}

				itemGapTimeout := item.receiveTime.Add(self.receiveBufferSettings.GapTimeout).Sub(receiveTime)
				if itemGapTimeout < 0 {
					self.log.Errorf("[r]%s<-%s s(%s) exit gap timeout\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
					// did not receive a preceding message in time
					return
				}

				if self.nextSequenceNumber < item.sequenceNumber {
					if itemGapTimeout < timeout {
						timeout = itemGapTimeout
					}
					break
				}
				// item.sequenceNumber <= self.nextSequenceNumber

				self.receiveQueue.RemoveByMessageId(item.messageId)

				if self.nextSequenceNumber == item.sequenceNumber {
					// this item is the head of sequence
					if err := self.registerContracts(item); err != nil {
						self.log.Errorf("[r]%s<-%s s(%s) exit could not register contracts = %s\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId, err)
						return
					}
					if self.updateContract(item) {
						if self.log.V(1).Enabled() {
							self.log.Infof("[r]seq+ %d->%d (queue) %s<-%s s(%s)\n", self.nextSequenceNumber, self.nextSequenceNumber+1, self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
						}
						self.nextSequenceNumber = self.nextSequenceNumber + 1
						self.receiveHead(item)
					} else {
						// no valid contract. it should have been attached to the head
						self.log.Errorf("[r]drop head no contract %s<-%s s(%s)\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
						return
					}
				} else {
					// this item is a resend of a previous item
					if item.ack {
						self.sendAck(item.sequenceNumber, item.messageId, false, nil, item.unwrapped)
					}
				}
			}
		}

		processPack := func(receivePack *ReceivePack, ok bool) bool {
			if !ok {
				return false
			}

			if receivePack.Pack.Nack {
				received, err := self.receiveNack(receivePack)
				if err != nil {
					// bad message
					// close the sequence
					self.log.Infof("[r]%s<-%s s(%s) exit could not receive nack = %s\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId, err)
					self.peerAudit.Update(func(a *PeerAudit) {
						a.badMessage(receivePack.MessageByteCount)
					})
					MessagePoolReturn(receivePack.TransferFrameBytes)
					return false
				} else if !received {
					if self.log.V(1).Enabled() {
						self.log.Infof("[r]drop nack %s<-%s s(%s)\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
					}
					// drop the message
					self.peerAudit.Update(func(a *PeerAudit) {
						a.discard(receivePack.MessageByteCount)
					})
					MessagePoolReturn(receivePack.TransferFrameBytes)
				}

				// note messages of `size < MinMessageByteCount` get counted as `MinMessageByteCount` against the contract
			} else {
				received, err := self.receive(receivePack)
				if err != nil {
					// bad message
					// close the sequence
					self.log.Errorf("[r]%s<-%s s(%s) exit could not receive ack = %s\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId, err)
					self.peerAudit.Update(func(a *PeerAudit) {
						a.badMessage(receivePack.MessageByteCount)
					})
					MessagePoolReturn(receivePack.TransferFrameBytes)
					return false
				} else if !received {
					if self.log.V(1).Enabled() {
						self.log.Infof("[r]drop ack %s<-%s s(%s)\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
					}
					// drop the message
					self.peerAudit.Update(func(a *PeerAudit) {
						a.discard(receivePack.MessageByteCount)
					})
					MessagePoolReturn(receivePack.TransferFrameBytes)
				}
			}
			return true
		}

		// fast path without arming a timer
		select {
		case <-self.ctx.Done():
			return
		case receivePack, ok := <-self.packs:
			if !processPack(receivePack, ok) {
				return
			}
			continue
		default:
		}

		checkpointId := self.idleCondition.Checkpoint()
		idleTimer.Reset(timeout)
		select {
		case <-self.ctx.Done():
			return
		case receivePack, ok := <-self.packs:
			if !processPack(receivePack, ok) {
				return
			}
		case <-idleTimer.C:
			if 0 == self.receiveQueue.Len() {
				done := false
				func() {
					self.packMutex.Lock()
					defer self.packMutex.Unlock()
					// idle timeout
					if self.idleCondition.Close(checkpointId) {
						done = true
					}
					// else there are pending updates
				}()
				if done {
					// close the sequence
					if self.log.V(1).Enabled() {
						self.log.Infof("[r]%s<-%s s(%s) exit idle timeout\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
					}
					return
				}
			}
		}
	}
}

func (self *ReceiveSequence) sendAck(sequenceNumber uint64, messageId Id, selective bool, tag *protocol.Tag, unwrapped bool) {
	ack := &sequenceAck{
		sequenceNumber: sequenceNumber,
		messageId:      messageId,
		selective:      selective,
		tag:            tag,
		unwrapped:      unwrapped,
	}
	self.ackWindow.Update(ack)
}

func (self *ReceiveSequence) receive(receivePack *ReceivePack) (bool, error) {
	receiveTime := time.Now()

	sequenceNumber := receivePack.Pack.SequenceNumber
	// var contractId *Id
	// if self.receiveContract != nil {
	// 	contractId = &self.receiveContract.contractId
	// }
	messageId, err := IdFromBytes(receivePack.Pack.MessageId)
	if err != nil {
		return false, errors.New("Bad message_id")
	}

	// note the receive contract is the contract active when this is at the head of the queue
	item := &receiveItem{
		transferItem: transferItem{
			messageId:        messageId,
			sequenceNumber:   sequenceNumber,
			messageByteCount: receivePack.MessageByteCount,
		},

		// contractId:      contractId,
		receiveTime:        receiveTime,
		frames:             receivePack.Pack.Frames,
		contractFrame:      receivePack.Pack.ContractFrame,
		receiveCallback:    receivePack.ReceiveCallback,
		head:               receivePack.Pack.Head,
		ack:                !receivePack.Pack.Nack,
		tag:                receivePack.Pack.Tag,
		transferFrameBytes: receivePack.TransferFrameBytes,
		unwrapped:          receivePack.Unwrapped,
	}

	// this case happens when the receiver is reformed or loses state.
	// the sequence id guarantees the sender is the same for the sequence
	// past head items are retransmits. Future head items depend on previous ack,
	// which represent some state the sender has that the receiver is missing
	// advance the receiver state to the latest from the sender
	if item.head && self.nextSequenceNumber < item.sequenceNumber {
		if self.log.V(2).Enabled() {
			self.log.Infof("[r]seq= %d->%d %s<-%s s(%s)\n", self.nextSequenceNumber, item.sequenceNumber, self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
		}
		self.nextSequenceNumber = item.sequenceNumber
		// the head must have a contract frame to reset the contract
	}

	if removedItem := self.receiveQueue.RemoveBySequenceNumber(sequenceNumber); removedItem != nil {
		self.peerAudit.Update(func(a *PeerAudit) {
			a.resend(removedItem.messageByteCount)
		})
		removedItem.messagePoolReturn()
	}

	// replace with the latest value (check both messageId and sequenceNumber)
	if removedItem := self.receiveQueue.RemoveByMessageId(messageId); removedItem != nil {
		self.peerAudit.Update(func(a *PeerAudit) {
			a.resend(removedItem.messageByteCount)
		})
		removedItem.messagePoolReturn()
	}

	if sequenceNumber <= self.nextSequenceNumber {
		if self.nextSequenceNumber == sequenceNumber {
			// this item is the head of sequence
			if self.log.V(2).Enabled() {
				self.log.Infof("[r]seq+ %d->%d %s<-%s s(%s)\n", self.nextSequenceNumber, self.nextSequenceNumber+1, self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
			}
			self.nextSequenceNumber = self.nextSequenceNumber + 1

			if err := self.registerContracts(item); err != nil {
				self.log.Errorf("[r]%s<-%s s(%s) ack could not register contracts = %s\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId, err)
				return false, err
			}
			if self.updateContract(item) {
				self.receiveHead(item)
				return true, nil
			} else {
				// no valid contract. it should have been attached to the head
				self.log.Errorf("[r]drop queue head no contract %s<-%s s(%s): head=%t, contract=%t, rcontract=%t\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId, item.head, item.contractFrame != nil, self.receiveContract != nil)
				return false, errors.New("No contract")
			}
		} else {
			if self.log.V(1).Enabled() {
				self.log.Infof("[r]drop past sequence number %d <> %d ack=%t %s<-%s s(%s)\n", sequenceNumber, self.nextSequenceNumber, item.ack, self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
			}
			// this item is a resend of a previous item
			if item.ack {
				self.sendAck(sequenceNumber, messageId, false, nil, item.unwrapped)
			}
			return false, nil
		}
	} else {
		// store only up to a max size in the receive queue.
		// an empty queue always admits at least one item (see CanAdd).
		canQueue := func(byteCount ByteCount) bool {
			return self.receiveQueue.CanAdd(byteCount, self.receiveBufferSettings.ReceiveQueueMaxByteCount)
		}

		// remove later items to fit
		for !canQueue(receivePack.MessageByteCount) {
			lastItem := self.receiveQueue.PeekLast()
			if receivePack.Pack.SequenceNumber < lastItem.sequenceNumber {
				self.receiveQueue.RemoveByMessageId(lastItem.messageId)
				lastItem.messagePoolReturn()
			} else {
				break
			}
		}

		if canQueue(receivePack.MessageByteCount) {
			self.receiveQueue.Add(item)
			self.sendAck(sequenceNumber, messageId, true, item.tag, item.unwrapped)
			return true, nil
		} else {
			if self.log.V(1).Enabled() {
				self.log.Infof("[r]drop ack cannot queue %s<-%s s(%s)\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
			}
			return false, nil
		}
	}
}

func (self *ReceiveSequence) receiveNack(receivePack *ReceivePack) (bool, error) {

	receiveTime := time.Now()

	sequenceNumber := receivePack.Pack.SequenceNumber
	// var contractId *Id
	// if self.receiveContract != nil {
	// 	contractId = &self.receiveContract.contractId
	// }
	messageId, err := IdFromBytes(receivePack.Pack.MessageId)
	if err != nil {
		return false, errors.New("Bad message_id")
	}

	var contractId *Id
	if receivePack.Pack.ContractId != nil {
		contractId_, err := IdFromBytes(receivePack.Pack.ContractId)
		if err != nil {
			return false, errors.New("Bad contract_id")
		}
		contractId = &contractId_
	}

	if contractId == nil && !self.receiveBufferSettings.AllowLegacyNack {
		self.log.Infof("[r]drop nack required contract id %s<-%s s(%s)\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
		return false, nil
	}

	item := &receiveItem{
		transferItem: transferItem{
			messageId:        messageId,
			sequenceNumber:   sequenceNumber,
			messageByteCount: receivePack.MessageByteCount,
		},
		contractId:         contractId,
		receiveTime:        receiveTime,
		frames:             receivePack.Pack.Frames,
		contractFrame:      receivePack.Pack.ContractFrame,
		receiveCallback:    receivePack.ReceiveCallback,
		head:               receivePack.Pack.Head,
		ack:                !receivePack.Pack.Nack,
		tag:                receivePack.Pack.Tag,
		transferFrameBytes: receivePack.TransferFrameBytes,
	}

	if err := self.registerContracts(item); err != nil {
		self.log.Errorf("[r]%s<-%s s(%s) nack could not register contracts = %s\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId, err)
		return false, err
	}

	if contractId != nil {
		if _, ok := self.openReceiveContracts[*contractId]; !ok {
			self.log.Infof("[r]drop nack contract mismatch %s<-%s s(%s)\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
			return false, nil
		}
	}

	if self.updateContract(item) {
		self.receiveHead(item)
		return true, nil
	} else {
		// no valid contract
		// drop the message. since this is a nack it will not block the sequence
		self.log.Infof("[r]drop nack no contract %s<-%s s(%s)\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
		return false, nil
	}
}

func (self *ReceiveSequence) receiveHead(item *receiveItem) {
	if self.log.V(1).Enabled() {
		frameMessageTypes := []string{}
		for _, frame := range item.frames {
			frameMessageTypes = append(frameMessageTypes, fmt.Sprintf("%v", frame.MessageType))
		}
		frameMessageTypesStr := strings.Join(frameMessageTypes, ", ")
		if item.ack {
			self.log.Infof("[r]head %d (%s) %s<-%s s(%s)\n", item.sequenceNumber, frameMessageTypesStr, self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
		} else {
			self.log.Infof("[r]head nack (%s) %s<-%s s(%s)\n", frameMessageTypesStr, self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
		}
	}
	self.peerAudit.Update(func(a *PeerAudit) {
		a.received(item.messageByteCount)
	})
	var peer Peer

	if item.contractId != nil {
		receiveContract := self.openReceiveContracts[*item.contractId]
		receiveContract.ack(item.messageByteCount)
		peer = Peer{
			ProvideMode: receiveContract.provideMode,
			Roles:       receiveContract.roles,
			Principal:   receiveContract.principal,
		}
	} else {
		// no contract peers are considered in network
		peer = Peer{
			ProvideMode: protocol.ProvideMode_Network,
		}
	}
	// EncryptedControl frames are routed into the per-peer session instead
	// of bubbling up to the receive callback. They carry the TLS handshake
	// bytes that bootstrap the per-peer cipher; the application shouldn't
	// see them.
	appFrames := item.frames
	if self.session != nil {
		appFrames = self.deliverEncryptedControlFrames(item.frames)
	}
	if 0 < len(appFrames) {
		item.receiveCallback(
			self.source,
			appFrames,
			peer,
		)
	}
	if item.ack {
		self.sendAck(item.sequenceNumber, item.messageId, false, item.tag, item.unwrapped)
	}
	item.messagePoolReturn()
}

// deliverEncryptedControlFrames splits an incoming Pack's frames: any
// `TransferEncryptedControl` frames are decoded and routed into the per-peer
// session of the complement of the sender's role (a client-role control —
// the peer's ClientHello — drives our server session, and vice versa),
// creating that session if needed. The remaining application frames are
// returned for delivery to the receive callback.
func (self *ReceiveSequence) deliverEncryptedControlFrames(frames []*protocol.Frame) []*protocol.Frame {
	var passthrough []*protocol.Frame
	for _, frame := range frames {
		if frame == nil {
			continue
		}
		if frame.MessageType != protocol.MessageType_TransferEncryptedControl {
			passthrough = append(passthrough, frame)
			continue
		}
		if self.client == nil || self.client.encryptionSessionManager == nil {
			continue
		}
		ec := &protocol.EncryptedControl{}
		if err := ProtoUnmarshal(frame.MessageBytes, ec); err != nil {
			if self.log.V(1).Enabled() {
				self.log.Infof("[r]%s<-%s bad encrypted control = %s\n", self.client.ClientTag(), self.source.SourceId, err)
			}
			continue
		}
		senderRole, ok := sequenceTlsRoleFromProtobuf(ec.SessionRole)
		if !ok {
			if self.log.V(1).Enabled() {
				self.log.Infof("[r]%s<-%s encrypted control with no session role — dropped\n", self.client.ClientTag(), self.source.SourceId)
			}
			continue
		}
		self.client.encryptionSessionManager.DeliverEncryptedControl(self.source.SourceId, senderRole.complement(), ec)
	}
	return passthrough
}

func (self *ReceiveSequence) registerContracts(item *receiveItem) error {
	if item.contractFrame == nil {
		return nil
	}

	var contract protocol.Contract
	err := ProtoUnmarshal(item.contractFrame.MessageBytes, &contract)
	if err != nil {
		// bad message
		// close sequence
		self.peerAudit.Update(func(a *PeerAudit) {
			a.badMessage(item.messageByteCount)
		})
		return err
	}

	// check the hmac with the local provider secret key
	if !self.client.ContractManager().Verify(
		contract.StoredContractHmac,
		contract.StoredContractBytes,
		contract.ProvideMode) {
		self.log.Errorf("[r]%s<-%s s(%s) exit contract verification failed (%s)\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId, contract.ProvideMode)
		// bad contract
		// close sequence
		self.peerAudit.Update(func(a *PeerAudit) {
			a.badContract()
		})
		return errors.New("Contract verification failed.")
	}

	nextReceiveContract, err := newSequenceContract(
		self.log,
		"r",
		&contract,
		self.receiveBufferSettings.MinMessageByteCount,
		1.0,
	)
	if err != nil {
		// bad contract
		// close sequence
		self.peerAudit.Update(func(a *PeerAudit) {
			a.badContract()
		})
		return err
	}

	if err := self.setContract(nextReceiveContract); err != nil {
		// the next contract has already been used
		// bad contract
		// close sequence
		self.peerAudit.Update(func(a *PeerAudit) {
			a.badContract()
		})
		return err
	}

	return nil
}

func (self *ReceiveSequence) setContract(nextReceiveContract *sequenceContract) error {
	// contract already set
	if self.receiveContract != nil && self.receiveContract.contractId == nextReceiveContract.contractId {
		return nil
	}

	if receiveContract, ok := self.openReceiveContracts[nextReceiveContract.contractId]; ok {
		// switch to the current contract
		self.receiveContract = receiveContract
		return nil
	}

	self.openReceiveContracts[nextReceiveContract.contractId] = nextReceiveContract
	self.receiveContract = nextReceiveContract
	// the receive side does not know companion-ness (the wire contract does not
	// carry it). listeners pair contracts to companions with the peer client id
	nextReceiveContract.statsEntry = self.client.ContractManager().registerContractStats(
		nextReceiveContract.contractId,
		true,
		false,
		nextReceiveContract.path,
		nextReceiveContract.transferByteCount,
	)

	if d := len(self.openReceiveContracts) - self.receiveBufferSettings.MaxOpenReceiveContract; 0 < d {
		// remove the least recently added
		orderedReceiveContracts := maps.Values(self.openReceiveContracts)
		// ascending where earliest created are first
		slices.SortFunc(orderedReceiveContracts, func(a *sequenceContract, b *sequenceContract) int {
			return a.localId.Cmp(b.localId)
		})
		for _, receiveContract := range orderedReceiveContracts[:d] {
			if receiveContract != self.receiveContract {
				self.client.ContractManager().CloseContract(
					receiveContract.contractId,
					receiveContract.ackedByteCount,
					receiveContract.unackedByteCount,
				)
				delete(self.openReceiveContracts, receiveContract.contractId)
			}
		}
	}

	return nil
}

func (self *ReceiveSequence) updateContract(item *receiveItem) bool {
	// always use a contract if present
	// the sender may send contracts even if `receiveNoContract` is set locally
	if item.contractId != nil {
		if receiveContract, ok := self.openReceiveContracts[*item.contractId]; ok && receiveContract.update(item.messageByteCount) {
			return true
		}
	} else if self.receiveContract != nil && self.receiveContract.update(item.messageByteCount) {
		item.contractId = &self.receiveContract.contractId
		return true
	}
	// `receiveNoContract` is a mutual configuration
	// both sides must configure themselves to require no contract from each other
	if self.client.ContractManager().ReceiveNoContract(self.source.SourceId) {
		return true
	}
	return false
}

func (self *ReceiveSequence) Close() {
	self.cancel()

	func() {
		self.packMutex.Lock()
		defer self.packMutex.Unlock()
		close(self.packs)
	}()

	// drain the channel
	func() {
		for {
			select {
			case receivePack, ok := <-self.packs:
				if !ok {
					return
				}
				MessagePoolReturn(receivePack.TransferFrameBytes)
			default:
				return
			}
		}
	}()
}

func (self *ReceiveSequence) Cancel() {
	self.cancel()
}

func (self *ReceiveSequence) WaitForExit() {
	select {
	case <-self.exit:
	}
}

type receiveItem struct {
	transferItem

	contractId         *Id
	head               bool
	receiveTime        time.Time
	frames             []*protocol.Frame
	contractFrame      *protocol.Frame
	receiveCallback    ReceiveFunction
	ack                bool
	tag                *protocol.Tag
	transferFrameBytes []byte
	// unwrapped is true when the originating TransferFrame arrived on
	// the wire as plaintext (no outer encrypted wrap). Propagated into
	// the sequenceAck so the ack format mirrors the incoming pack.
	unwrapped bool
}

func (self *receiveItem) messagePoolReturn() {
	MessagePoolReturn(self.transferFrameBytes)
	// note frames and contractFrame are slices/shared bytes of the transfer frame bytes
	// we expect these both to be false
	// for _, frame := range self.frames {
	// 	r := MessagePoolReturn(frame.MessageBytes)
	// 	if r {
	// 		self.log.Warningf("[ri]frame was not shared]\n")
	// 	}
	// }
	// if self.contractFrame != nil {
	// 	r := MessagePoolReturn(self.contractFrame.MessageBytes)
	// 	if r {
	// 		self.log.Warningf("[ri]contract frame was not shared]\n")
	// 	}
	// }
}

// ordered by sequenceNumber
type receiveQueue = transferQueue[*receiveItem]

func newReceiveQueue(budget *TransferMemoryBudget, minByteCount ByteCount) *receiveQueue {
	queue := newTransferQueue[*receiveItem](func(a *receiveItem, b *receiveItem) int {
		if a.sequenceNumber < b.sequenceNumber {
			return -1
		} else if b.sequenceNumber < a.sequenceNumber {
			return 1
		} else {
			return 0
		}
	})
	queue.setBudget(budget, minByteCount)
	return queue
}

type sequenceAck struct {
	sequenceNumber uint64
	messageId      Id
	selective      bool
	tag            *protocol.Tag
	// unwrapped is true when any pack covered by this ack arrived on
	// the wire as plaintext. The ack writer mirrors that state — a
	// plaintext-acked window emits a plaintext ack — so peers whose
	// ciphers haven't been established yet can read the ack. Cumulative
	// head acks or-in the bit across every absorbed lower ack.
	unwrapped bool
}

type sequenceAckWindowSnapshot struct {
	ackNotify      <-chan struct{}
	headAck        *sequenceAck
	ackUpdateCount int
	selectiveAcks  map[Id]*sequenceAck
}

type sequenceAckWindow struct {
	ackMonitor     *Monitor
	ackLock        sync.Mutex
	headAck        *sequenceAck
	ackUpdateCount int
	selectiveAcks  map[Id]*sequenceAck
}

func newSequenceAckWindow() *sequenceAckWindow {
	return &sequenceAckWindow{
		ackMonitor:     NewMonitor(),
		headAck:        nil,
		ackUpdateCount: 0,
		selectiveAcks:  map[Id]*sequenceAck{},
	}
}

func (self *sequenceAckWindow) Update(ack *sequenceAck) {
	self.ackLock.Lock()
	defer self.ackLock.Unlock()

	if self.headAck == nil || self.headAck.sequenceNumber < ack.sequenceNumber {
		if ack.selective {
			if prior, ok := self.selectiveAcks[ack.messageId]; ok && prior.unwrapped {
				// coalesced selective ack for the same message: preserve
				// any prior plaintext bit so a single late wrapped resend
				// doesn't upgrade the ack format past the sender's reach.
				ack.unwrapped = true
			}
			self.selectiveAcks[ack.messageId] = ack
		} else {
			// cumulative head ack: or-in the prior head's plaintext bit
			// (and any absorbed selective acks below the new head) so a
			// single plaintext pack anywhere under the head keeps the
			// ack plaintext. Selective acks at or below the new head are
			// already dropped by the Snapshot pass.
			if self.headAck != nil && self.headAck.unwrapped {
				ack.unwrapped = true
			}
			if !ack.unwrapped {
				for _, sel := range self.selectiveAcks {
					if sel.unwrapped && sel.sequenceNumber <= ack.sequenceNumber {
						ack.unwrapped = true
						break
					}
				}
			}
			self.ackUpdateCount += 1
			self.headAck = ack
			// no need to clean up `selectiveAcks` here
			// selective acks with sequence number <= head are ignored in a final pass during update
		}
	} else {
		// past the head
		// resend the head — fold this late ack's plaintext bit into the
		// head so the resend covers it. Copy-on-write: a prior Snapshot
		// may have published the current `headAck` pointer to writeAck,
		// which reads `unwrapped` without holding ackLock, so mutating
		// the struct in place would race. Swap in a fresh copy with the
		// bit set instead.
		if ack.unwrapped && self.headAck != nil && !self.headAck.unwrapped {
			updated := *self.headAck
			updated.unwrapped = true
			self.headAck = &updated
		}
		self.ackUpdateCount += 1
	}

	self.ackMonitor.NotifyAll()
}

// Snapshot is returned by value: it is consumed immediately by the caller and
// never retained, so a heap allocation per snapshot is pure waste. The caller
// always receives a copy of (or nil for) the selective acks, never the live
// map, so the live map can be cleared and reused on reset.
func (self *sequenceAckWindow) Snapshot(reset bool) sequenceAckWindowSnapshot {
	self.ackLock.Lock()
	defer self.ackLock.Unlock()

	// build the selective-ack copy lazily so the common in-order case (a
	// cumulative head ack with no selective acks) allocates no map.
	var selectiveAcksAfterHead map[Id]*sequenceAck
	if 0 < self.ackUpdateCount {
		for messageId, ack := range self.selectiveAcks {
			if self.headAck.sequenceNumber < ack.sequenceNumber {
				if selectiveAcksAfterHead == nil {
					selectiveAcksAfterHead = map[Id]*sequenceAck{}
				}
				selectiveAcksAfterHead[messageId] = ack
			}
		}
	} else if 0 < len(self.selectiveAcks) {
		selectiveAcksAfterHead = maps.Clone(self.selectiveAcks)
	}

	snapshot := sequenceAckWindowSnapshot{
		ackNotify:      self.ackMonitor.NotifyChannel(),
		headAck:        self.headAck,
		ackUpdateCount: self.ackUpdateCount,
		selectiveAcks:  selectiveAcksAfterHead,
	}

	if reset {
		// keep the head ack in place. clear() reuses the live map's storage
		// instead of allocating a fresh map; the caller holds only a copy.
		self.ackUpdateCount = 0
		clear(self.selectiveAcks)
	}

	return snapshot
}

type sequenceContract struct {
	log                        Logger
	localId                    Id
	tag                        string
	contract                   *protocol.Contract
	contractId                 Id
	transferByteCount          ByteCount
	effectiveTransferByteCount ByteCount
	provideMode                protocol.ProvideMode

	minUpdateByteCount ByteCount

	path TransferPath

	ackedByteCount   ByteCount
	unackedByteCount ByteCount

	// when set, the sequence stores the used byte count here on each debit,
	// so ongoing contract usage can be reported to stats listeners
	// (see transfer_contract_stats.go)
	statsEntry *contractStatsEntry

	// provideTlsCertificate is the PEM-encoded X.509 chain (leaf first)
	// that the destination committed to as its server TLS identity for
	// this contract. Empty when the destination did not publish a
	// certificate via `ContractManager.SetProvideTlsCertificate`. The
	// SendSequence uses this to verify the peer presented during the
	// per-peer TLS handshake against the platform-signed contract.
	provideTlsCertificate [][]byte
	// destinationClientPublicKey is the peer's 32-byte Ed25519
	// long-lived public identity key, as committed by the platform in
	// `Contract.destination_client_public_key`. The sender uses it to
	// (a) verify `destinationClientKeySignedTlsCertificate` against
	// `provideTlsCertificate` — only then is the cert chain admitted
	// to the per-peer session's trusted set — and (b) verify the
	// peer's post-handshake identity proof exchanged inside the per-
	// peer TLS session. Empty when the contract carries no key.
	destinationClientPublicKey []byte
	// destinationClientKeySignedTlsCertificate is the peer's Ed25519
	// signature over the canonical concatenation of every PEM block
	// in `provideTlsCertificate`. The signing key is the peer's
	// long-lived client identity key (private half held only by the
	// peer); the verifier is `destinationClientPublicKey`. Empty when
	// the contract carries no signature.
	destinationClientKeySignedTlsCertificate []byte

	// roles and principal are the source client's identity, sealed into the
	// platform-signed contract bytes. Honored only when the provide mode is
	// network; nil/empty for all other provide modes.
	roles     []string
	principal string
}

func newSequenceContract(log Logger, tag string, contract *protocol.Contract, minUpdateByteCount ByteCount, contractFillFraction float32) (*sequenceContract, error) {
	storedContract := &protocol.StoredContract{}
	err := ProtoUnmarshal(contract.StoredContractBytes, storedContract)
	if err != nil {
		return nil, err
	}

	contractId, err := IdFromBytes(storedContract.ContractId)
	if err != nil {
		return nil, err
	}

	path, err := TransferPathFromBytes(
		storedContract.SourceId,
		storedContract.DestinationId,
		storedContract.StreamId,
	)
	if err != nil {
		return nil, err
	}

	// The platform-signed `StoredContract.ProvideTlsCertificate` is the
	// authoritative cert commitment (signed under `storedContractHmac`); the
	// outer `Contract.ProvideTlsCertificate` is a convenience copy for
	// clients that don't unmarshal the stored bytes. Prefer the stored value;
	// fall back to the outer value only when the inner is missing.
	provideTlsCertificate := storedContract.ProvideTlsCertificate
	if len(provideTlsCertificate) == 0 && contract != nil {
		provideTlsCertificate = contract.ProvideTlsCertificate
	}

	// Same prefer-stored-fallback-to-outer convention for the destination's
	// client-identity public key and the destination's signature over the
	// cert chain (Option 1 of the long-lived-identity verification design).
	destinationClientPublicKey := storedContract.DestinationClientPublicKey
	if len(destinationClientPublicKey) == 0 && contract != nil {
		destinationClientPublicKey = contract.DestinationClientPublicKey
	}
	destinationClientKeySignedTlsCertificate := storedContract.DestinationClientKeySignedTlsCertificate
	if len(destinationClientKeySignedTlsCertificate) == 0 && contract != nil {
		destinationClientKeySignedTlsCertificate = contract.DestinationClientKeySignedTlsCertificate
	}

	// roles/principal live only in the signed stored bytes (no outer copy)
	// and apply only to network provide mode
	var roles []string
	var principal string
	if contract.ProvideMode == protocol.ProvideMode_Network {
		roles = storedContract.Roles
		principal = storedContract.Principal
	}

	return &sequenceContract{
		log:                                      log,
		localId:                                  NewId(),
		tag:                                      tag,
		contract:                                 contract,
		contractId:                               contractId,
		transferByteCount:                        ByteCount(storedContract.TransferByteCount),
		effectiveTransferByteCount:               ByteCount(float32(storedContract.TransferByteCount) * contractFillFraction),
		provideMode:                              contract.ProvideMode,
		minUpdateByteCount:                       minUpdateByteCount,
		path:                                     path,
		ackedByteCount:                           ByteCount(0),
		unackedByteCount:                         ByteCount(0),
		provideTlsCertificate:                    provideTlsCertificate,
		destinationClientPublicKey:               destinationClientPublicKey,
		destinationClientKeySignedTlsCertificate: destinationClientKeySignedTlsCertificate,
		roles:                                    roles,
		principal:                                principal,
	}, nil
}

func (self *sequenceContract) update(byteCount ByteCount) bool {
	effectiveByteCount := max(self.minUpdateByteCount, byteCount)

	if self.effectiveTransferByteCount < self.ackedByteCount+self.unackedByteCount+effectiveByteCount {
		// doesn't fit in contract
		// if self.log.V(1).Enabled() {
		self.log.Infof(
			"[%s]debit contract %s failed +%d->%d (%d/%d total %.1f%% full)\n",
			self.tag,
			self.contractId,
			effectiveByteCount,
			self.ackedByteCount+self.unackedByteCount+effectiveByteCount,
			self.ackedByteCount+self.unackedByteCount,
			self.effectiveTransferByteCount,
			100.0*float32(self.ackedByteCount+self.unackedByteCount)/float32(self.effectiveTransferByteCount),
		)
		// }
		return false
	}
	self.unackedByteCount += effectiveByteCount
	if self.statsEntry != nil {
		self.statsEntry.updateUsedByteCount(self.ackedByteCount + self.unackedByteCount)
	}
	if self.log.V(1).Enabled() {
		self.log.Infof(
			"[%s]debit contract %s passed +%d->%d (%d/%d total %.1f%% full)\n",
			self.tag,
			self.contractId,
			effectiveByteCount,
			self.ackedByteCount+self.unackedByteCount,
			self.ackedByteCount+self.unackedByteCount,
			self.effectiveTransferByteCount,
			100.0*float32(self.ackedByteCount+self.unackedByteCount)/float32(self.effectiveTransferByteCount),
		)
	}
	return true
}

func (self *sequenceContract) ack(byteCount ByteCount) {
	effectiveByteCount := max(self.minUpdateByteCount, byteCount)

	if self.unackedByteCount < effectiveByteCount {
		// debug.PrintStack()
		panic(fmt.Errorf("Bad accounting %d <> %d", self.unackedByteCount, byteCount))
	}

	self.unackedByteCount -= effectiveByteCount
	self.ackedByteCount += effectiveByteCount
}

type ForwardBufferSettings struct {
	IdleTimeout time.Duration

	SequenceBufferSize int

	WriteTimeout time.Duration
}

type ForwardBuffer struct {
	ctx    context.Context
	client *Client

	forwardBufferSettings *ForwardBufferSettings

	mutex sync.Mutex
	// destination -> forward sequence
	forwardSequences map[TransferPath]*ForwardSequence
}

func NewForwardBuffer(ctx context.Context,
	client *Client,
	forwardBufferSettings *ForwardBufferSettings) *ForwardBuffer {
	return &ForwardBuffer{
		ctx:                   ctx,
		client:                client,
		forwardBufferSettings: forwardBufferSettings,
		forwardSequences:      map[TransferPath]*ForwardSequence{},
	}
}

func (self *ForwardBuffer) Pack(forwardPack *ForwardPack, timeout time.Duration) (bool, error) {
	initForwardSequence := func(skip *ForwardSequence) *ForwardSequence {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		forwardSequence, ok := self.forwardSequences[forwardPack.Destination]
		if ok {
			if skip == nil || skip != forwardSequence {
				return forwardSequence
			} else {
				forwardSequence.Cancel()
				// delete(self.forwardSequences, forwardPack.Destination)
			}
		}
		forwardSequence = NewForwardSequence(
			self.ctx,
			self.client,
			forwardPack.Destination,
			self.forwardBufferSettings,
		)
		self.forwardSequences[forwardPack.Destination] = forwardSequence
		go HandleError(func() {
			defer func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()
				forwardSequence.Close()
				// clean up
				if forwardSequence == self.forwardSequences[forwardPack.Destination] {
					delete(self.forwardSequences, forwardPack.Destination)
				}
			}()
			forwardSequence.Run()
		})
		return forwardSequence
	}

	var forwardSequence *ForwardSequence
	var success bool
	var err error
	for i := 0; i < 2; i += 1 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		default:
		}
		forwardSequence = initForwardSequence(forwardSequence)
		if success, err = forwardSequence.Pack(forwardPack, timeout); err == nil {
			return success, nil
		}
		// sequence closed
	}
	return success, err
}

func (self *ForwardBuffer) Close() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// cancel all open sequences
	// the control of the sequence will close it
	for _, forwardSequence := range self.forwardSequences {
		forwardSequence.Cancel()
	}
}

func (self *ForwardBuffer) Cancel() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// cancel all open sequences
	for _, forwardSequence := range self.forwardSequences {
		forwardSequence.Cancel()
	}
}

func (self *ForwardBuffer) Flush() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// cancel all open sequences
	for _, forwardSequence := range self.forwardSequences {
		// if !destination.IsControlDestination() {
		forwardSequence.Cancel()
		// }
	}
}

type ForwardSequence struct {
	ctx    context.Context
	cancel context.CancelFunc

	client    *Client
	clientId  Id
	clientTag string
	log       Logger

	destination TransferPath

	forwardBufferSettings *ForwardBufferSettings

	packMutex sync.Mutex
	packs     chan *ForwardPack

	idleCondition *IdleCondition

	multiRouteWriter MultiRouteWriter
}

func NewForwardSequence(
	ctx context.Context,
	client *Client,
	destination TransferPath,
	forwardBufferSettings *ForwardBufferSettings) *ForwardSequence {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &ForwardSequence{
		ctx:                   cancelCtx,
		cancel:                cancel,
		client:                client,
		log:                   client.log,
		destination:           destination,
		forwardBufferSettings: forwardBufferSettings,
		packs:                 make(chan *ForwardPack, forwardBufferSettings.SequenceBufferSize),
		idleCondition:         NewIdleCondition(),
	}
}

// success, error
func (self *ForwardSequence) Pack(forwardPack *ForwardPack, timeout time.Duration) (bool, error) {
	self.packMutex.Lock()
	defer self.packMutex.Unlock()

	select {
	case <-forwardPack.Ctx.Done():
		return false, errors.New("Done.")
	case <-self.ctx.Done():
		return false, errors.New("Done.")
	default:
	}

	if !self.idleCondition.UpdateOpen() {
		return false, errors.New("Done.")
	}
	defer self.idleCondition.UpdateClose()

	// fast path without arming a timer
	select {
	case self.packs <- forwardPack:
		return true, nil
	default:
	}

	if timeout < 0 {
		select {
		case <-forwardPack.Ctx.Done():
			return false, errors.New("Done.")
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.packs <- forwardPack:
			return true, nil
		}
	} else if timeout == 0 {
		select {
		case <-forwardPack.Ctx.Done():
			return false, errors.New("Done.")
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.packs <- forwardPack:
			return true, nil
		default:
			return false, nil
		}
	} else {
		select {
		case <-forwardPack.Ctx.Done():
			return false, errors.New("Done.")
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.packs <- forwardPack:
			return true, nil
		case <-time.After(timeout):
			return false, nil
		}
	}
}

func (self *ForwardSequence) Run() {
	defer self.cancel()

	self.multiRouteWriter = self.client.RouteManager().OpenMultiRouteWriter(self.destination)
	defer self.client.RouteManager().CloseMultiRouteWriter(self.multiRouteWriter)

	// reusable idle timer (avoids a per-iteration time.After alloc on the
	// forward hot path). created already-fired; Reset before the blocking select
	// arms it (go1.23+ delivers no stale fire after Reset).
	idleTimer := time.NewTimer(0)
	defer idleTimer.Stop()

	for {
		processPack := func(forwardPack *ForwardPack, ok bool) bool {
			if !ok {
				return false
			}
			c := func() error {
				transferFrameBytes := forwardPack.TransferFrameBytes
				if DebugTransferCopyOnWrite {
					transferFrameBytes = MessagePoolCopy(forwardPack.TransferFrameBytes)
					// the write proceeds on the copy; the original is done here
					MessagePoolReturn(forwardPack.TransferFrameBytes)
				}
				defer MessagePoolReturn(transferFrameBytes)
				shared := MessagePoolShareReadOnly(transferFrameBytes)
				err := self.multiRouteWriter.Write(
					self.ctx,
					shared,
					self.forwardBufferSettings.WriteTimeout,
				)
				if err != nil {
					// a failed write leaves ownership here: undo the consumer's share
					MessagePoolReturn(shared)
				}
				return err
			}
			if self.log.V(2).Enabled() {
				TraceWithReturn(
					fmt.Sprintf("[f]multi route write %s->%s s(%s)", self.clientTag, self.destination.DestinationId, self.destination.StreamId),
					c,
				)
			} else {
				err := c()
				if err != nil {
					if self.log.V(2).Enabled() {
						self.log.Infof("[f]drop = %s", err)
					}
				}
			}
			return true
		}

		// fast path without arming a timer
		select {
		case <-self.ctx.Done():
			return
		case forwardPack, ok := <-self.packs:
			if !processPack(forwardPack, ok) {
				return
			}
			continue
		default:
		}

		checkpointId := self.idleCondition.Checkpoint()
		idleTimer.Reset(self.forwardBufferSettings.IdleTimeout)
		select {
		case <-self.ctx.Done():
			return
		case forwardPack, ok := <-self.packs:
			if !processPack(forwardPack, ok) {
				return
			}
		case <-idleTimer.C:
			done := false
			func() {
				self.packMutex.Lock()
				defer self.packMutex.Unlock()
				// idle timeout
				if self.idleCondition.Close(checkpointId) {
					done = true
				}
				// else there are pending updates
			}()
			if done {
				// close the sequence
				if self.log.V(1).Enabled() {
					self.log.Infof("[f]exit idle timeout %s->%s s(%s)", self.clientTag, self.destination.DestinationId, self.destination.StreamId)
				}
				return
			}
		}
	}
}

func (self *ForwardSequence) Close() {
	self.cancel()

	func() {
		self.packMutex.Lock()
		defer self.packMutex.Unlock()
		close(self.packs)
	}()

	// drain the channel (mirrors SendSequence.Close/ReceiveSequence.Close: queued
	// packs are owned by the sequence and must be returned)
	func() {
		for {
			select {
			case forwardPack, ok := <-self.packs:
				if !ok {
					return
				}
				MessagePoolReturn(forwardPack.TransferFrameBytes)
			default:
				return
			}
		}
	}()
}

func (self *ForwardSequence) Cancel() {
	self.cancel()
}

type PeerAudit struct {
	startTime           time.Time
	lastModifiedTime    time.Time
	Abuse               bool
	BadContractCount    int
	DiscardedByteCount  ByteCount
	DiscardedCount      int
	BadMessageByteCount ByteCount
	BadMessageCount     int
	SendByteCount       ByteCount
	SendCount           int
	ResendByteCount     ByteCount
	ResendCount         int
}

func NewPeerAudit(startTime time.Time) *PeerAudit {
	return &PeerAudit{
		startTime:           startTime,
		lastModifiedTime:    startTime,
		BadContractCount:    0,
		DiscardedByteCount:  ByteCount(0),
		DiscardedCount:      0,
		BadMessageByteCount: ByteCount(0),
		BadMessageCount:     0,
		SendByteCount:       ByteCount(0),
		SendCount:           0,
		ResendByteCount:     ByteCount(0),
		ResendCount:         0,
	}
}

func (self *PeerAudit) badMessage(byteCount ByteCount) {
	self.BadMessageCount += 1
	self.BadMessageByteCount += byteCount
}

func (self *PeerAudit) discard(byteCount ByteCount) {
	self.DiscardedCount += 1
	self.DiscardedByteCount += byteCount
}

func (self *PeerAudit) badContract() {
	self.BadContractCount += 1
}

func (self *PeerAudit) received(byteCount ByteCount) {
	self.SendCount += 1
	self.SendByteCount += byteCount
}

func (self *PeerAudit) resend(byteCount ByteCount) {
	self.ResendCount += 1
	self.ResendByteCount += byteCount
}

type SequencePeerAudit struct {
	client           *Client
	log              Logger
	source           TransferPath
	maxAuditDuration time.Duration

	peerAudit *PeerAudit
}

func NewSequencePeerAudit(client *Client, source TransferPath, maxAuditDuration time.Duration) *SequencePeerAudit {
	return &SequencePeerAudit{
		client:           client,
		log:              client.log,
		source:           source,
		maxAuditDuration: maxAuditDuration,
		peerAudit:        nil,
	}
}

func (self *SequencePeerAudit) Update(callback func(*PeerAudit)) {
	auditTime := time.Now()

	if self.peerAudit != nil && self.maxAuditDuration <= auditTime.Sub(self.peerAudit.startTime) {
		self.Complete()
	}
	if self.peerAudit == nil {
		self.peerAudit = NewPeerAudit(auditTime)
	}

	callback(self.peerAudit)
	self.peerAudit.lastModifiedTime = auditTime
	// TODO auto complete the peer audit after timeout
}

func (self *SequencePeerAudit) Complete() {
	if self.peerAudit == nil {
		return
	}

	peerAudit := &protocol.PeerAudit{
		PeerId:              self.source.SourceId.Bytes(),
		StreamId:            self.source.StreamId.Bytes(),
		Duration:            uint64(math.Ceil((self.peerAudit.lastModifiedTime.Sub(self.peerAudit.startTime)).Seconds())),
		Abuse:               self.peerAudit.Abuse,
		BadContractCount:    uint64(self.peerAudit.BadContractCount),
		DiscardedByteCount:  uint64(self.peerAudit.DiscardedByteCount),
		DiscardedCount:      uint64(self.peerAudit.DiscardedCount),
		BadMessageByteCount: uint64(self.peerAudit.BadMessageByteCount),
		BadMessageCount:     uint64(self.peerAudit.BadMessageCount),
		SendByteCount:       uint64(self.peerAudit.SendByteCount),
		SendCount:           uint64(self.peerAudit.SendCount),
		ResendByteCount:     uint64(self.peerAudit.ResendByteCount),
		ResendCount:         uint64(self.peerAudit.ResendCount),
	}
	frame, err := ToFrame(peerAudit, DefaultProtocolVersion)
	if err != nil {
		self.log.Errorf("[c]could not create audit frame = %s", err)
		return
	}
	self.client.ClientOob().SendControl(
		[]*protocol.Frame{frame},
		func(resultFrames []*protocol.Frame, err error) {},
	)
	self.peerAudit = nil
}

// contract frames are not counted towards the message byte count
// this is required since contracts can be attached post-hoc
func MessageByteCount(frames []*protocol.Frame) ByteCount {
	// messageByteCount := ByteCount(0)
	// for _, frame := range frames {
	// 	if frame.MessageType != protocol.MessageType_TransferContract {
	// 		messageByteCount += ByteCount(len(frame.MessageBytes))
	// 	}
	// }
	// return messageByteCount
	messageByteCount := ByteCount(0)
	for _, frame := range frames {
		messageByteCount += ByteCount(len(frame.MessageBytes))
	}
	return messageByteCount
}

// func MessageFrames(frames []*protocol.Frame) []*protocol.Frame {
// 	messages := []*protocol.Frame{}
// 	for _, frame := range frames {
// 		if frame.MessageType != protocol.MessageType_TransferContract {
// 			messages = append(messages, frame)
// 		}
// 	}
// 	return messages
// }
