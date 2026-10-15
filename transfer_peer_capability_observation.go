package connect

// Per-peer evidence of two transfer capabilities, measured on the wire rather
// than inferred from the version a peer reports (DESIGNNOTES §3.13):
//
//   - an end-to-end session: the peer's responder answered this client's
//     ClientHello, a TLS 1.3 handshake carried in EncryptedControl (§3.5),
//     with the key-exchange group the peer chose from this client's offer,
//     and the peer's identity proof verified so the session sealed
//     application traffic. Only sessions this client initiates count.
//   - resizable windows: the peer's acknowledgements carry
//     `Ack.receive_window_byte_count` (THROUGHPUTFIX §37.3).
//
// Recording happens only at edges that are already rare: a client-role epoch's
// first outbox batch (its ClientHello), a completed client-role handshake, its
// seal, a send sequence's first acknowledgement, and a send sequence's first or
// larger window advertisement. None is per packet. A nil recorder, the
// default, costs one nil check at those edges and records nothing.

import (
	"crypto/tls"
	"slices"
	"sync"
	"time"
)

// The ML-KEM TLS key exchanges by IANA NamedGroup codepoint, named here so the
// classification does not depend on which of them the toolchain names.
// `tls.X25519MLKEM768` (0x11ec) is the group the sequence TLS configs prefer.
const (
	tlsKeyExchangeSecP256r1MlKem768  tls.CurveID = 0x11eb
	tlsKeyExchangeSecP384r1MlKem1024 tls.CurveID = 0x11ed
	tlsKeyExchangeMlKem512           tls.CurveID = 0x0200
	tlsKeyExchangeMlKem768           tls.CurveID = 0x0201
	tlsKeyExchangeMlKem1024          tls.CurveID = 0x0202
)

// The peer bound of a zero-value recorder, or of one built with a non-positive
// bound.
const defaultPeerCapabilityRecorderMaxPeerCount = 1024

// Whether a negotiated group carries an ML-KEM component, hybrid or pure.
func isPostQuantumKeyExchange(curveId tls.CurveID) bool {
	switch curveId {
	case tls.X25519MLKEM768,
		tlsKeyExchangeSecP256r1MlKem768,
		tlsKeyExchangeSecP384r1MlKem1024,
		tlsKeyExchangeMlKem512,
		tlsKeyExchangeMlKem768,
		tlsKeyExchangeMlKem1024:
		return true
	default:
		return false
	}
}

// The group's TLS name, e.g. "X25519MLKEM768", and empty for none.
func tlsKeyExchangeName(curveId tls.CurveID) string {
	if curveId == 0 {
		return ""
	}
	return curveId.String()
}

// One peer's evidence as of the read.
//
// Every flag is sticky for the life of the owning PeerCapabilityRecorder:
// each records something the peer did, not a current state. A false flag is
// an absence of evidence, and it is evidence of absence only when the
// precondition named on the field holds.
type PeerCapabilityObservation struct {
	PeerId Id

	// A client-role session of an observing client began a handshake with the
	// peer and the send path accepted its ClientHello (which can still be lost
	// on the way). Implied by E2eNegotiated. Sessions exist only when that
	// client's `EncryptionSettings.Mode` is not `EncryptionModeOff`; under Off
	// the e2e fields say nothing about the peer.
	E2eAttempted bool
	// When the send path first accepted a ClientHello. With E2eNegotiated still
	// false, the time the peer has had to answer. The sender records the
	// attempt after the hand-over, so an answer recorded first, by a sender
	// descheduled in between, sets this to the answer's time instead.
	E2eFirstAttemptTime time.Time
	// The peer's TLS server answered a ClientHello of an observing client and
	// the TLS 1.3 handshake completed. Not authenticated by itself: the peer's
	// identity is verified only when E2eSealed is set. Only sessions an
	// observing client initiates count. A handshake the peer initiates, with
	// the client as the TLS server, records nothing: its group is the
	// client's choice from the peer's offer, not the peer's from the client's.
	E2eNegotiated bool
	// Such a session verified the peer's identity proof and became the
	// established epoch, which `PeerEncryptionState.Sealed` reports for it.
	// From then its cipher seals application traffic with the peer, unless the
	// Required mode's signed-identity gate withholds it (DESIGNNOTES3 §5.1).
	E2eSealed bool
	// The TLS group the peer's responder chose from the client's offer
	// (`DefaultSequencePostQuantumCurves`: X25519MLKEM768, then X25519), e.g.
	// "X25519MLKEM768" or "X25519": that of the most recent sealed session,
	// or, while none has sealed, of the most recent completed handshake. Empty
	// until E2eNegotiated.
	//
	// It is the `tls.CurveID.String()` name of the group that
	// `tls.ConnectionState.CurveID` reported, the value
	// `PeerEncryptionState.KeyExchange` holds as a `tls.CurveID`. A string, so
	// a consumer can store it without depending on crypto/tls; a group the
	// toolchain does not name renders as "CurveID(<decimal codepoint>)", and
	// PostQuantum classifies by codepoint, so it never depends on the name.
	KeyExchange string
	// KeyExchange carries an ML-KEM component, hybrid or pure: the peer's
	// responder chose a post-quantum group. With E2eSealed, a post-quantum key
	// exchange the peer's identity proof authenticated.
	PostQuantum bool

	// A send sequence to the peer processed an acknowledgement of its own
	// retained data. The precondition for reading WindowAdvertised as false.
	AckObserved bool
	// An acknowledgement from the peer carried `receive_window_byte_count`.
	WindowAdvertised bool
	// The largest advertised value, clamped at 2^32-1 as the sender keeps it.
	// With WindowAdvertised, zero is a real advertisement: a receiver that can
	// hold nothing out of order.
	MaxReceiveWindowByteCount ByteCount

	// When the record last received an observation of any kind, including one
	// that changed no field.
	LastObservationTime time.Time
}

// Records, per peer, what the peer has shown it supports on the wire (see
// PeerCapabilityObservation). A caller opts in by setting
// `ClientSettings.PeerCapabilityRecorder` before constructing the client;
// every client given the same recorder records into it, so an owner that
// rotates clients toward one peer (a multi-client window) keeps the evidence
// each of them saw.
//
// Read-only for its owner: recording is internal and changes no behaviour, and
// whether an end-to-end session is attempted is the client's
// `EncryptionSettings.Mode`, never this recorder.
//
// Safe for concurrent use. One small leaf lock guards the records: it is taken
// at the rare recording edges and by reads, and nothing is acquired while it is
// held. Retained memory is one record per distinct peer, at most the bound
// given to `NewPeerCapabilityRecorder`; an observation naming a peer first
// seen past it is not recorded and is counted by `DroppedObservationCount`,
// once per refused observation, since the bound keeps no memory of the peers
// it refused. The zero value is ready to use with the default bound of 1024
// peers. Create one per owner, never one per process: the bound and the
// records belong to that owner. Do not copy it after first use.
type PeerCapabilityRecorder struct {
	maxPeerCount int

	stateLock               sync.Mutex
	peerIdCapabilities      map[Id]*peerCapability
	droppedObservationCount uint64

	// Nil in production; tests pin the observation clock. Called under
	// stateLock, so it must not call back into the recorder.
	nowForTest func() time.Time
	// Nil in production; tests rendezvous after a recorded observation,
	// outside the state lock.
	afterObserveForTest func(peerId Id)
}

// One peer's evidence. Guarded by the owning recorder's stateLock.
type peerCapability struct {
	e2eAttempted        bool
	e2eFirstAttemptTime time.Time
	e2eNegotiated       bool
	e2eSealed           bool
	// the group KeyExchange reports, and whether a sealed session supplied it,
	// in which case a later unauthenticated handshake does not replace it
	keyExchange       tls.CurveID
	keyExchangeSealed bool

	ackObserved               bool
	windowAdvertised          bool
	maxReceiveWindowByteCount ByteCount

	lastObservationTime time.Time
}

// A recorder of at most maxPeerCount distinct peers; a non-positive count takes
// the default of 1024.
func NewPeerCapabilityRecorder(maxPeerCount int) *PeerCapabilityRecorder {
	if maxPeerCount <= 0 {
		maxPeerCount = defaultPeerCapabilityRecorderMaxPeerCount
	}
	return &PeerCapabilityRecorder{
		maxPeerCount:       maxPeerCount,
		peerIdCapabilities: map[Id]*peerCapability{},
	}
}

// The evidence recorded for one peer, and false when nothing has been recorded
// for it (always, for a nil recorder).
func (self *PeerCapabilityRecorder) Get(peerId Id) (PeerCapabilityObservation, bool) {
	if self == nil {
		return PeerCapabilityObservation{}, false
	}
	capability, ok := func() (peerCapability, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		capability, ok := self.peerIdCapabilities[peerId]
		if !ok {
			return peerCapability{}, false
		}
		return *capability, true
	}()
	if !ok {
		return PeerCapabilityObservation{}, false
	}
	return capability.observation(peerId), true
}

// A copy of every recorded peer's evidence, ordered by peer id. Nil for a nil
// recorder.
func (self *PeerCapabilityRecorder) Snapshot() []PeerCapabilityObservation {
	if self == nil {
		return nil
	}
	type recordedCapability struct {
		peerId     Id
		capability peerCapability
	}
	recordedCapabilities := func() []recordedCapability {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		out := make([]recordedCapability, 0, len(self.peerIdCapabilities))
		for peerId, capability := range self.peerIdCapabilities {
			out = append(out, recordedCapability{peerId: peerId, capability: *capability})
		}
		return out
	}()
	observations := make([]PeerCapabilityObservation, 0, len(recordedCapabilities))
	for _, recorded := range recordedCapabilities {
		observations = append(observations, recorded.capability.observation(recorded.peerId))
	}
	slices.SortFunc(observations, func(a PeerCapabilityObservation, b PeerCapabilityObservation) int {
		return a.PeerId.Cmp(b.PeerId)
	})
	return observations
}

// How many observations were not recorded because they named a peer not held
// when the bound was full. It counts observations, not peers: one refused peer
// observed three times counts three.
func (self *PeerCapabilityRecorder) DroppedObservationCount() uint64 {
	if self == nil {
		return 0
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.droppedObservationCount
}

// Records that the send path accepted a client-role epoch's ClientHello to the
// peer, keeping the first attempt's time.
func (self *PeerCapabilityRecorder) observeE2eAttempt(peerId Id) {
	if self == nil || peerId == (Id{}) {
		return
	}
	recorded := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		capability := self.capabilityWithLock(peerId)
		if capability == nil {
			return false
		}
		now := self.now()
		capability.markAttemptedWithLock(now)
		capability.lastObservationTime = now
		return true
	}()
	self.afterObserve(peerId, recorded)
}

// Records a completed client-role handshake with the peer and the group its
// responder chose, unless a sealed session already supplied the group. Called
// only for sessions this client initiated.
func (self *PeerCapabilityRecorder) observeE2eHandshake(peerId Id, keyExchange tls.CurveID) {
	if self == nil || peerId == (Id{}) {
		return
	}
	recorded := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		capability := self.capabilityWithLock(peerId)
		if capability == nil {
			return false
		}
		now := self.now()
		// an answer implies the ClientHello it answered was handed over, even
		// when its sender has not recorded that yet
		capability.markAttemptedWithLock(now)
		capability.e2eNegotiated = true
		if !capability.keyExchangeSealed {
			capability.keyExchange = keyExchange
		}
		capability.lastObservationTime = now
		return true
	}()
	self.afterObserve(peerId, recorded)
}

// Records a client-role session whose epoch, negotiated with the given group,
// verified the peer's identity proof and became established. Called only for
// sessions this client initiated.
func (self *PeerCapabilityRecorder) observeE2eSealed(peerId Id, keyExchange tls.CurveID) {
	if self == nil || peerId == (Id{}) {
		return
	}
	recorded := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		capability := self.capabilityWithLock(peerId)
		if capability == nil {
			return false
		}
		now := self.now()
		// a seal implies the handshake it verified, and the attempt
		capability.markAttemptedWithLock(now)
		capability.e2eNegotiated = true
		capability.e2eSealed = true
		capability.keyExchange = keyExchange
		capability.keyExchangeSealed = true
		capability.lastObservationTime = now
		return true
	}()
	self.afterObserve(peerId, recorded)
}

// Records a send sequence's first acknowledgement from the peer.
func (self *PeerCapabilityRecorder) observeAck(peerId Id) {
	if self == nil || peerId == (Id{}) {
		return
	}
	recorded := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		capability := self.capabilityWithLock(peerId)
		if capability == nil {
			return false
		}
		now := self.now()
		capability.ackObserved = true
		capability.lastObservationTime = now
		return true
	}()
	self.afterObserve(peerId, recorded)
}

// Records a send sequence's first or larger receive window advertisement from
// the peer.
func (self *PeerCapabilityRecorder) observeReceiveWindow(peerId Id, receiveWindowByteCount ByteCount) {
	if self == nil || peerId == (Id{}) {
		return
	}
	recorded := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		capability := self.capabilityWithLock(peerId)
		if capability == nil {
			return false
		}
		now := self.now()
		// an advertisement only ever arrives on an acknowledgement
		capability.ackObserved = true
		if !capability.windowAdvertised || capability.maxReceiveWindowByteCount < receiveWindowByteCount {
			capability.maxReceiveWindowByteCount = receiveWindowByteCount
		}
		capability.windowAdvertised = true
		capability.lastObservationTime = now
		return true
	}()
	self.afterObserve(peerId, recorded)
}

// The peer's record, created within the bound, or nil when the bound refuses a
// new peer. Caller holds stateLock.
func (self *PeerCapabilityRecorder) capabilityWithLock(peerId Id) *peerCapability {
	if capability, ok := self.peerIdCapabilities[peerId]; ok {
		return capability
	}
	maxPeerCount := self.maxPeerCount
	if maxPeerCount <= 0 {
		maxPeerCount = defaultPeerCapabilityRecorderMaxPeerCount
	}
	if maxPeerCount <= len(self.peerIdCapabilities) {
		self.droppedObservationCount += 1
		return nil
	}
	if self.peerIdCapabilities == nil {
		self.peerIdCapabilities = map[Id]*peerCapability{}
	}
	capability := &peerCapability{}
	self.peerIdCapabilities[peerId] = capability
	return capability
}

// Every recording reads its time here, inside its critical section, so
// recording order is time order: LastObservationTime never moves backwards and
// the most recent observation is the one recorded last. A time read before the
// lock would let an observation delayed between the two record an older time
// over a newer one.
func (self *PeerCapabilityRecorder) now() time.Time {
	if self.nowForTest != nil {
		return self.nowForTest()
	}
	return time.Now()
}

// Runs the test rendezvous, outside the state lock, after a recorded
// observation.
func (self *PeerCapabilityRecorder) afterObserve(peerId Id, recorded bool) {
	if recorded && self.afterObserveForTest != nil {
		self.afterObserveForTest(peerId)
	}
}

// Marks the record attempted, keeping the first attempt's time. Caller holds
// the owning recorder's stateLock.
func (self *peerCapability) markAttemptedWithLock(now time.Time) {
	if !self.e2eAttempted {
		self.e2eAttempted = true
		self.e2eFirstAttemptTime = now
	}
}

func (self peerCapability) observation(peerId Id) PeerCapabilityObservation {
	return PeerCapabilityObservation{
		PeerId:                    peerId,
		E2eAttempted:              self.e2eAttempted,
		E2eFirstAttemptTime:       self.e2eFirstAttemptTime,
		E2eNegotiated:             self.e2eNegotiated,
		E2eSealed:                 self.e2eSealed,
		KeyExchange:               tlsKeyExchangeName(self.keyExchange),
		PostQuantum:               isPostQuantumKeyExchange(self.keyExchange),
		AckObserved:               self.ackObserved,
		WindowAdvertised:          self.windowAdvertised,
		MaxReceiveWindowByteCount: self.maxReceiveWindowByteCount,
		LastObservationTime:       self.lastObservationTime,
	}
}
