package connect

// A responder restarted under the same identity (QD-1176): the peer that held a
// session with it, and that opens first after the restart, must reach it at
// once. The restarted client comes back under the same client id with the same
// client key, the same provide keys and the same sequence certificate, as a
// daemon that keeps its connect identity does, and waits: it sends nothing until
// the stale peer reaches it. The stale peer's established epoch is unreadable
// at the restarted responder, which has no session state at all, and the
// restarted responder never saw the head of the stale peer's send sequence.
//
// Before the fix the stale peer reached it only after its timeouts ended the
// session: its recovery ClientHello waited in the restarted responder's receive
// sequence behind the records sealed under the demoted epoch, which nothing
// could open, and the stale peer's earlier attempts' records met the restarted
// responder's TLS state where they came through. Real connect clients keep real
// timers, so the bounds are real time, each one a broken run's, never a sleep.

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The restarted-responder tests' bounds.
type restartedResponderTestSettings struct {
	// How soon after the restarted responder starts the stale initiator's
	// application send must reach it. Above a nack round trip, a handshake, the
	// identity proofs and a resend; below TlsTimeout, which a broken recovery
	// waits out before anything recovers.
	ReachTimeout time.Duration
	// Each step before the restart, and each barrier's wait, a broken run's
	// bound.
	StepTimeout time.Duration
	// The pair's TlsTimeout, as sidecar's test network sets it.
	TlsTimeout time.Duration
}

func defaultRestartedResponderTestSettings() *restartedResponderTestSettings {
	return &restartedResponderTestSettings{
		ReachTimeout: 10 * time.Second,
		StepTimeout:  30 * time.Second,
		TlsTimeout:   30 * time.Second,
	}
}

// What a client keeps across a restart.
type restartedResponderIdentity struct {
	clientKeySeed     []byte
	provideSecretKeys map[protocol.ProvideMode][]byte
	tlsCertificatePem []byte
	tlsPrivateKeyPem  []byte
}

// Whether other presents this identity: the same client key, provide keys and
// sequence certificate.
func (self *restartedResponderIdentity) same(other *restartedResponderIdentity) bool {
	if !bytes.Equal(self.clientKeySeed, other.clientKeySeed) ||
		!bytes.Equal(self.tlsCertificatePem, other.tlsCertificatePem) ||
		!bytes.Equal(self.tlsPrivateKeyPem, other.tlsPrivateKeyPem) ||
		len(self.provideSecretKeys) != len(other.provideSecretKeys) {
		return false
	}
	for mode, key := range self.provideSecretKeys {
		if !bytes.Equal(key, other.provideSecretKeys[mode]) {
			return false
		}
	}
	return true
}

// One EncryptedControl handshake record the initiator put on the wire toward
// the responder: its type, its attempt, and the Pack's place in its sequence.
type restartedResponderRecord struct {
	controlType    protocol.EncryptedControlType
	epochId        Id
	sequenceNumber uint64
}

// The EncryptedControl records a plaintext wire frame carries, none for a
// sealed one.
func restartedResponderRecordsOf(transferFrameBytes []byte) []restartedResponderRecord {
	var transferFrame protocol.TransferFrame
	if err := ProtoUnmarshal(transferFrameBytes, &transferFrame); err != nil || 0 < len(transferFrame.EncryptedTransferFrame) {
		return nil
	}
	pack := transferFrame.Pack
	if pack == nil {
		if transferFrame.Frame == nil || transferFrame.Frame.MessageType != protocol.MessageType_TransferPack {
			return nil
		}
		pack = &protocol.Pack{}
		if err := ProtoUnmarshal(transferFrame.Frame.MessageBytes, pack); err != nil {
			return nil
		}
	}
	var records []restartedResponderRecord
	for _, frame := range pack.Frames {
		if frame.MessageType != protocol.MessageType_TransferEncryptedControl {
			continue
		}
		ec := &protocol.EncryptedControl{}
		if err := ProtoUnmarshal(frame.MessageBytes, ec); err != nil {
			continue
		}
		epochId, _ := IdFromBytes(ec.GetEpochId())
		records = append(records, restartedResponderRecord{
			controlType:    ec.ControlType,
			epochId:        epochId,
			sequenceNumber: pack.SequenceNumber,
		})
	}
	return records
}

// Two Required clients on conditioned loopback routes, the responder of which
// can be restarted under its kept identity.
type restartedResponderPair struct {
	t        *testing.T
	ctx      context.Context
	settings *restartedResponderTestSettings

	initiator   *Client
	initiatorId Id
	responderId Id

	stateLock sync.Mutex
	responder *Client
	identity  *restartedResponderIdentity

	responderSend    chan []byte
	responderReceive chan []byte
	receives         chan string
}

func restartedResponderClientSettings(settings *restartedResponderTestSettings) *ClientSettings {
	s := DefaultClientSettingsWithBufferSize(64)
	s.SendBufferSettings.AckTimeout = 60 * time.Second
	s.SendBufferSettings.IdleTimeout = 1 * time.Second
	s.SendBufferSettings.MinResendInterval = 10 * time.Millisecond
	s.ReceiveBufferSettings.GapTimeout = 60 * time.Second
	s.ReceiveBufferSettings.IdleTimeout = 60 * time.Second
	s.ForwardBufferSettings.IdleTimeout = 1 * time.Second
	s.ContractManagerSettings.LegacyCreateContract = false
	s.EncryptionSettings.Mode = EncryptionModeRequired
	s.EncryptionSettings.TlsTimeout = settings.TlsTimeout
	return s
}

// A pair whose initiator's settings mutateInitiator adjusts, nil for none, and
// whose initiator-to-responder wire inspect sees, frame by frame, before the
// responder does, dropping a frame by answering false; nil forwards every one.
func newRestartedResponderPair(
	t *testing.T,
	ctx context.Context,
	settings *restartedResponderTestSettings,
	mutateInitiator func(*ClientSettings),
	inspect func(transferFrameBytes []byte) bool,
) *restartedResponderPair {
	pair := &restartedResponderPair{
		t:                t,
		ctx:              ctx,
		settings:         settings,
		initiatorId:      NewId(),
		responderId:      NewId(),
		responderReceive: make(chan []byte),
		receives:         make(chan string, 1024),
	}

	initiatorSend := make(chan []byte)
	pair.responderSend = make(chan []byte)
	_, conditioned := newConditioner(ctx, initiatorSend)
	_, initiatorReceive := newConditioner(ctx, pair.responderSend)
	// the tap owns each frame it takes until it forwards or returns it
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case message := <-conditioned:
				if inspect != nil && !inspect(message) {
					MessagePoolReturn(message)
					continue
				}
				if !forwardOwnedTestWire(ctx, pair.responderReceive, message) {
					return
				}
			}
		}
	}()

	initiatorOob := &grantingClientOob{
		sourceId: pair.initiatorId,
		settings: DefaultContractManagerSettings(),
		destSecretKey: func(destinationId Id) ([]byte, bool) {
			key, ok := pair.currentIdentity().provideSecretKeys[protocol.ProvideMode_Network]
			return key, ok
		},
		destClientPublicKey: func(destinationId Id) []byte {
			return pair.currentResponder().ClientKeyManager().PublicKey()
		},
	}
	initiatorSettings := restartedResponderClientSettings(settings)
	if mutateInitiator != nil {
		mutateInitiator(initiatorSettings)
	}
	pair.initiator = NewClient(ctx, pair.initiatorId, initiatorOob, initiatorSettings)
	pair.initiator.RouteManager().UpdateTransport(newDataGatewayTransport(), []Route{initiatorSend})
	pair.initiator.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{initiatorReceive})
	blackholeControlId(ctx, pair.initiator.RouteManager())
	pair.initiator.ContractManager().SetProvideModes(map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})

	pair.startResponder(nil)
	return pair
}

// Starts the responder, under identity when one is given and a fresh one
// otherwise, on the pair's routes.
func (self *restartedResponderPair) startResponder(identity *restartedResponderIdentity) {
	settings := restartedResponderClientSettings(self.settings)
	if identity != nil {
		settings.ClientKeySeed = identity.clientKeySeed
		settings.EncryptionSettings.ProvideTlsCertificatePem = identity.tlsCertificatePem
		settings.EncryptionSettings.ProvideTlsPrivateKeyPem = identity.tlsPrivateKeyPem
	}
	responderOob := &grantingClientOob{
		sourceId: self.responderId,
		settings: DefaultContractManagerSettings(),
		destSecretKey: func(destinationId Id) ([]byte, bool) {
			return self.initiator.ContractManager().GetProvideSecretKey(protocol.ProvideMode_Network)
		},
		destClientPublicKey: func(destinationId Id) []byte {
			return self.initiator.ClientKeyManager().PublicKey()
		},
	}
	responder := NewClient(self.ctx, self.responderId, responderOob, settings)
	if identity != nil {
		responder.ContractManager().LoadProvideSecretKeys(identity.provideSecretKeys)
	}
	blackholeControlId(self.ctx, responder.RouteManager())
	responder.ContractManager().SetProvideModes(map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
	responder.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			if m, err := FromFrame(frame); err == nil {
				if sm, ok := m.(*protocol.SimpleMessage); ok {
					select {
					case self.receives <- sm.Content:
					default:
					}
				}
			}
		}
	})
	kept := &restartedResponderIdentity{
		clientKeySeed:     responder.ClientKeyManager().Seed(),
		provideSecretKeys: responder.ContractManager().GetProvideSecretKeys(),
		tlsCertificatePem: responder.EncryptionSessionManager().ProvideTlsCertificatePem(),
		tlsPrivateKeyPem:  responder.EncryptionSessionManager().ProvideTlsPrivateKeyPem(),
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.responder = responder
		self.identity = kept
	}()
	responder.RouteManager().UpdateTransport(newDataGatewayTransport(), []Route{self.responderSend})
	responder.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{self.responderReceive})
}

func (self *restartedResponderPair) currentResponder() *Client {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.responder
}

func (self *restartedResponderPair) currentIdentity() *restartedResponderIdentity {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.identity
}

// Stops the responder as a killed process stops, and joins it so no route
// reader of it is left. Answers the identity it kept.
func (self *restartedResponderPair) stopResponder() *restartedResponderIdentity {
	self.t.Helper()
	kept := self.currentIdentity()
	joinCtx, cancelJoin := context.WithTimeout(self.ctx, self.settings.StepTimeout)
	defer cancelJoin()
	if err := self.currentResponder().CloseAndWait(joinCtx); err != nil {
		self.t.Fatalf("the responder did not stop: %v", err)
	}
	return kept
}

// Stops the responder and starts it again under its kept identity. Answers
// when the restarted responder started.
func (self *restartedResponderPair) restartResponder() time.Time {
	self.t.Helper()
	kept := self.stopResponder()
	startTime := time.Now()
	self.startResponder(kept)
	return startTime
}

// Sends label from the initiator and waits up to deadline for the responder
// to receive it. Answers whether it did.
func (self *restartedResponderPair) initiatorReaches(label string, deadline time.Time) bool {
	self.t.Helper()
	if !self.initiator.SendWithTimeout(requiredGateFrame(self.t, label), self.responderId, func(error) {}, time.Until(deadline)) {
		return false
	}
	for {
		select {
		case got := <-self.receives:
			if got == label {
				return true
			}
		case <-time.After(time.Until(deadline)):
			return false
		}
	}
}

func (self *restartedResponderPair) close() {
	self.initiator.Cancel()
	self.currentResponder().Cancel()
}

// The stale initiator opens first to a responder restarted under the same
// identity, its established epoch unreadable there, and reaches it within
// ReachTimeout of the restart.
func TestRestartedResponderUnderSameIdentityIsReachedAtOnce(t *testing.T) {
	settings := defaultRestartedResponderTestSettings()
	ctx, cancel := context.WithTimeout(context.Background(), 3*settings.StepTimeout)
	defer cancel()
	pair := newRestartedResponderPair(t, ctx, settings, nil, nil)
	defer pair.close()

	if !pair.initiatorReaches("before", time.Now().Add(settings.StepTimeout)) {
		t.Fatal("the first send did not arrive")
	}
	waitForSealedSession(t, ctx, pair.initiator)
	before := pair.currentIdentity()

	startTime := pair.restartResponder()
	if !before.same(pair.currentIdentity()) {
		t.Fatal("the restarted responder presents another identity")
	}
	if !pair.initiatorReaches("after", startTime.Add(settings.ReachTimeout)) {
		t.Fatalf("the stale initiator did not reach the restarted responder within %s", settings.ReachTimeout)
	}
	t.Logf("reached the restarted responder %s after its start", time.Since(startTime))
}

// A rekey is in flight when the responder dies: its ClientHello is lost with
// the responder and stays in the initiator's resend queue. After the restart
// the initiator's first send is unreadable at the restarted responder, whose
// nack demotes every attempt the initiator minted, the rekey's among them, and
// starts a recovery attempt. A barrier holds the rekey ClientHello's resend
// until that demotion. The ClientHello then leaves its Pack, which still fills
// its place in the sequence, so no record of a dropped attempt ever reaches the
// restarted responder, and the send behind it arrives within ReachTimeout.
func TestRestartedResponderNeverMeetsADroppedAttemptsHandshake(t *testing.T) {
	settings := defaultRestartedResponderTestSettings()
	ctx, cancel := context.WithTimeout(context.Background(), 3*settings.StepTimeout)
	defer cancel()

	// the lost rekey ClientHello's place in its sequence, once the tap dropped it
	var rekeySequenceNumber atomic.Uint64
	var rekeySequenceNumberSet atomic.Bool
	// the tap's state, under tapLock
	var tapLock sync.Mutex
	dropNextClientHello := false
	rekeyClientHelloDropped := make(chan struct{})
	var rekeyId Id
	recording := false
	var restartedRecords []restartedResponderRecord
	inspect := func(transferFrameBytes []byte) bool {
		tapLock.Lock()
		defer tapLock.Unlock()
		for _, record := range restartedResponderRecordsOf(transferFrameBytes) {
			if dropNextClientHello && record.controlType == protocol.EncryptedControlType_EncryptedControlHandshake {
				// lost with the responder
				dropNextClientHello = false
				rekeyId = record.epochId
				rekeySequenceNumber.Store(record.sequenceNumber)
				rekeySequenceNumberSet.Store(true)
				close(rekeyClientHelloDropped)
				return false
			}
			if recording {
				restartedRecords = append(restartedRecords, record)
			}
		}
		return true
	}

	// holds the initiator's resends of the lost rekey ClientHello's Pack until
	// a nack demotion has happened
	demoted := make(chan struct{})
	holdRekeyResend := func(id sendSequenceId, number uint64) {
		if id.Destination == ControlId || id.EncryptionRole != sequenceTlsRoleClient ||
			!rekeySequenceNumberSet.Load() || number != rekeySequenceNumber.Load() {
			return
		}
		select {
		case <-demoted:
		case <-ctx.Done():
		case <-time.After(settings.StepTimeout):
		}
	}
	pair := newRestartedResponderPair(t, ctx, settings, func(s *ClientSettings) {
		s.SendBufferSettings.beforeDueResendForTest = holdRekeyResend
	}, inspect)
	defer pair.close()

	if !pair.initiatorReaches("before", time.Now().Add(settings.StepTimeout)) {
		t.Fatal("the first send did not arrive")
	}
	waitForSealedSession(t, ctx, pair.initiator)
	session := pair.initiator.encryptionSessionManager.Lookup(pair.responderId, sequenceTlsRoleClient, false)
	if session == nil {
		t.Fatal("no initiator session")
	}
	var recoveryId Id
	var demotedOnce sync.Once
	func() {
		session.stateLock.Lock()
		defer session.stateLock.Unlock()
		session.staleHandshakeRecordsForTest = func(epochId Id) {
			demotedOnce.Do(func() {
				recoveryId = epochId
				close(demoted)
			})
		}
	}()

	kept := pair.stopResponder()
	func() {
		tapLock.Lock()
		defer tapLock.Unlock()
		dropNextClientHello = true
	}()
	session.restartHandshake()
	select {
	case <-rekeyClientHelloDropped:
	case <-time.After(settings.StepTimeout):
		t.Fatal("the rekey sent no ClientHello")
	}
	func() {
		tapLock.Lock()
		defer tapLock.Unlock()
		recording = true
	}()
	startTime := time.Now()
	pair.startResponder(kept)
	if !pair.initiatorReaches("after", startTime.Add(settings.ReachTimeout)) {
		t.Fatalf("the stale initiator did not reach the restarted responder within %s", settings.ReachTimeout)
	}
	t.Logf("reached the restarted responder %s after its start", time.Since(startTime))

	select {
	case <-demoted:
	default:
		t.Fatal("the restarted responder's nack demoted nothing: the scenario did not happen")
	}
	session.stateLock.Lock()
	staleBefore := recoveryId
	session.stateLock.Unlock()
	tapLock.Lock()
	defer tapLock.Unlock()
	if !rekeyId.LessThan(staleBefore) {
		t.Fatalf("the rekey's attempt %s is not one the demotion to %s dropped", rekeyId, staleBefore)
	}
	recovered := false
	for _, record := range restartedRecords {
		switch record.controlType {
		case protocol.EncryptedControlType_EncryptedControlHandshake, protocol.EncryptedControlType_EncryptedControlIdentityProof:
		default:
			continue
		}
		if record.epochId.LessThan(staleBefore) {
			t.Errorf("the restarted responder met a %s record of dropped attempt %s at sequence number %d", record.controlType, record.epochId, record.sequenceNumber)
		}
		if record.epochId == staleBefore {
			recovered = true
		}
	}
	if !recovered {
		t.Error("no record of the recovery attempt reached the restarted responder")
	}
}
