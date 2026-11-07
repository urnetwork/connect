// Signed, valid large proofs exercise physical H1 wire bounds and the real
// logical-group ownership/ACK path without changing retransmission timers.
package connect

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// Produces a real CA/leaf chain. A noncritical private extension makes its PEM
// commitment at least the requested size while X.509 validation remains real.
func privateH1ProofChain(t *testing.T, targetByteCount int) [][]byte {
	t.Helper()
	caKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x41}, ed25519.SeedSize))
	leafKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x42}, ed25519.SeedSize))
	before, after := time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "H1 proof CA"},
		NotBefore: before, NotAfter: after, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDer, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDer)
	if err != nil {
		t.Fatal(err)
	}
	caPem := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDer})
	makeLeaf := func(padding int) []byte {
		value, err := asn1.Marshal(bytes.Repeat([]byte{0x51}, padding))
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "proof.internal.test"},
			DNSNames: []string{"proof.internal.test"}, NotBefore: before, NotAfter: after,
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			ExtraExtensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 55555, 1}, Value: value}}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, leafKey.Public(), caKey)
		if err != nil {
			t.Fatal(err)
		}
		return der
	}
	low, high := 0, targetByteCount
	for low < high {
		middle := low + (high-low)/2
		encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: makeLeaf(middle)})
		if len(encoded)+len(caPem) < targetByteCount {
			low = middle + 1
		} else {
			high = middle
		}
	}
	leafDer := makeLeaf(low)
	leaf, err := x509.ParseCertificate(leafDer)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "proof.internal.test", CurrentTime: time.Unix(1767225600, 0)}); err != nil {
		t.Fatalf("large proof is not a valid X.509 chain: %v", err)
	}
	chain := [][]byte{pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDer}), caPem}
	if ok, err := verifyPeerCertificateAgainstContract([]*x509.Certificate{leaf, ca}, chain); err != nil || !ok {
		t.Fatalf("production certificate commitment rejected its actual chain: %t/%v", ok, err)
	}
	if size := len(chain[0]) + len(chain[1]); size < targetByteCount || size > targetByteCount+8 {
		t.Fatalf("certificate commitment size=%d, target=%d", size, targetByteCount)
	}
	return chain
}

// Signs both commitment copies with the actual destination identity and the
// StoredContract with the receiver's real Network authority secret.
func privateH1SignedProof(t *testing.T, source Id, receiver *Client, chain [][]byte, credit uint64) *protocol.Contract {
	t.Helper()
	publicKey := receiver.ClientKeyManager().PublicKey()
	signature := receiver.ClientKeyManager().SignCertChain(chain)
	if !VerifyCertChainSignature(publicKey, chain, signature) {
		t.Fatal("identity certificate signature did not verify")
	}
	stored, err := proto.Marshal(&protocol.StoredContract{ContractId: NewId().Bytes(), SourceId: source.Bytes(),
		DestinationId: receiver.ClientId().Bytes(), TransferByteCount: credit,
		ProvideTlsCertificate: chain, DestinationClientPublicKey: publicKey, DestinationClientKeySignedTlsCertificate: signature,
		Roles: []string{"provider"}, Principal: "h1-proof-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	secret, ok := receiver.ContractManager().GetProvideSecretKey(protocol.ProvideMode_Network)
	if !ok {
		t.Fatal("receiver has no Network authority secret")
	}
	contract := &protocol.Contract{StoredContractBytes: stored, StoredContractHmac: SignStoredContract(receiver.ContractManager().settings, secret, stored),
		ProvideMode: protocol.ProvideMode_Network, ProvideTlsCertificate: chain, DestinationClientPublicKey: publicKey,
		DestinationClientKeySignedTlsCertificate: signature}
	if !VerifyStoredContract(receiver.ContractManager().settings, secret, stored, contract.StoredContractHmac) {
		t.Fatal("Network contract proof did not verify")
	}
	return contract
}

// The in-memory authority lends response bytes and takes request ownership.
// Its optional first grant holds exactly one eight-packet chunk plus opening.
type privateH1ProofAuthority struct {
	t          *testing.T
	source     Id
	peer       func() *Client
	owner      func() *Client
	provides   atomic.Int32
	chain      [][]byte
	firstSmall bool
	grants     atomic.Int32
}

// This plaintext fixture uses local signing identities but no published-key
// reader. Keep every unrelated periodic control disabled at its actual default.
func privateH1SkipUnusedKeyPublication(t *testing.T, settings *ClientSettings) {
	t.Helper()
	if settings.EncryptionSettings.Mode != EncryptionModeOff || settings.ClientKeyRegistrationRequired ||
		settings.ContractManagerSettings.CloseReportDomainHash != ([32]byte{}) ||
		settings.ControlPingTimeout != 0 || settings.ContractManagerSettings.ProvidePingTimeout != 0 || settings.SendPeerAudits {
		t.Fatal("proof fixture cannot skip a required key or model an enabled periodic control")
	}
	settings.SkipClientKeyPublication = true
}

// Match the actual owner and its immutable publication domain, including the
// legacy omission of the zero domain. A native ACK is not processed readiness.
func privateH1ValidClientKey(owner *Client, key *protocol.ClientKey) bool {
	if owner == nil || key == nil || len(key.PublicKey) != ed25519.PublicKeySize || !bytes.Equal(key.PublicKey, owner.ClientKeyManager().PublicKey()) {
		return false
	}
	domain := owner.settings.ContractManagerSettings.CloseReportDomainHash
	if domain == ([32]byte{}) {
		return len(key.HistoryDomainHash) == 0
	}
	return len(key.HistoryDomainHash) == len(domain) && bytes.Equal(key.HistoryDomainHash, domain[:])
}

// Network registration is checked against the exact client's generated secret.
func privateH1ValidProvide(owner *Client, provide *protocol.Provide) bool {
	if owner == nil || provide == nil {
		return false
	}
	secret, present := owner.ContractManager().GetProvideSecretKey(protocol.ProvideMode_Network)
	return present && len(provide.Keys) == 1 && provide.Keys[0].Mode == protocol.ProvideMode_Network &&
		len(provide.Keys[0].ProvideSecretKey) == 32 && bytes.Equal(provide.Keys[0].ProvideSecretKey, secret)
}

// Both live reports and shutdown checkpoints carry stable, nonzero identities.
// The exact original contract/report is asserted separately at actual delivery.
func privateH1ValidClose(report *protocol.CloseContract) bool {
	return report != nil && len(report.ContractId) == 16 && len(report.ReportId) == 16 &&
		RequireIdFromBytes(report.ContractId) != (Id{}) && RequireIdFromBytes(report.ReportId) != (Id{}) &&
		len(report.OriginalReport) == 0 && len(report.OriginalInventory) == 0
}

// Returns real signed contracts and acknowledges validated OOB registrations.
// No Transfer acknowledgement is fabricated or injected.
func (self *privateH1ProofAuthority) SendControl(frames []*protocol.Frame, callback func([]*protocol.Frame, error)) {
	defer func() {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
	}()
	var responses []*protocol.Frame
	reject := func(err error) {
		self.t.Errorf("proof authority rejected OOB control: %v", err)
		if callback != nil {
			callback(nil, err)
		}
	}
	for _, frame := range frames {
		message, err := FromFrame(frame)
		if err != nil {
			reject(err)
			return
		}
		owner := self.owner()
		if owner == nil || owner.ClientId() != self.source {
			reject(errors.New("control authority has no exact source owner"))
			return
		}
		switch value := message.(type) {
		case *protocol.Provide:
			if !privateH1ValidProvide(owner, value) {
				reject(errors.New("invalid Network provide registration"))
				return
			}
			self.provides.Add(1)
			continue
		case *protocol.ClientKey:
			if !privateH1ValidClientKey(owner, value) {
				reject(errors.New("invalid source client key registration"))
				return
			}
			continue
		case *protocol.CloseContract:
			if !privateH1ValidClose(value) {
				reject(errors.New("invalid cleanup close/checkpoint identity"))
				return
			}
			continue
		case *protocol.CreateContract:
			// Only peer proof requests reach the signing branch below.
		default:
			reject(errors.New("unexpected OOB control type"))
			return
		}
		create := message.(*protocol.CreateContract)
		if self.peer == nil || len(create.DestinationId) != 16 {
			reject(errors.New("contract request lacks an actual peer"))
			return
		}
		peer := self.peer()
		if peer == nil || RequireIdFromBytes(create.DestinationId) != peer.ClientId() {
			reject(errors.New("contract request names another destination"))
			return
		}
		chain, credit := self.chain, uint64(256*1024*1024)
		if self.grants.Add(1) == 1 && self.firstSmall {
			chain, credit = nil, uint64(8*DefaultMtu+1)
		}
		contract := privateH1SignedProof(self.t, self.source, peer, chain, credit)
		response, err := proto.Marshal(&protocol.CreateContractResult{Contract: contract})
		if err != nil {
			panic(err)
		}
		responses = append(responses, &protocol.Frame{MessageType: protocol.MessageType_TransferCreateContractResult, MessageBytes: response})
	}
	if callback != nil {
		callback(responses, nil)
	}
}

// Uses the existing controlled-wire capture helpers with real contract
// verification. Every wire share and raw witness retires after joined workers.
type privateH1ProofFixture struct {
	*windowRoundFixture
	budget              *TransferMemoryBudget
	transport           Transport
	chain               [][]byte
	delivered           [][]byte
	witnessFrames       []*protocol.Frame
	witnesses           [][]byte
	releaseBarrier      func()
	platform            *Client
	platformInput       Route
	platformAcksEnabled bool
	platformReports     chan privateH1PlatformClose
	platformSeen        []privateH1PlatformClose
	platformKeys        chan privateH1PlatformKey
}

// All protocol timers remain at their defaults. Constant windows and an exact
// retained leaf make memory eligibility, rather than wall-clock speed, causal.
func privateH1NewFixture(t *testing.T, version int, budgetBytes ByteCount, firstSmall bool, carrier TransportType, configure func(*SendBufferSettings)) *privateH1ProofFixture {
	t.Helper()
	if DefaultMtu != 1280 {
		t.Fatal("canonical H1 experiment requires product MTU1280")
	}
	ctx, cancel := context.WithCancel(t.Context())
	fixture := &privateH1ProofFixture{windowRoundFixture: &windowRoundFixture{t: t, ctx: ctx, cancel: cancel,
		senderOut: make(Route, 128), senderIn: make(Route, 128), receiverOut: make(Route, 128), receiverIn: make(Route, 128)},
		budget: NewTransferMemoryBudget(budgetBytes), chain: privateH1ProofChain(t, 3600)}
	settings := func() *ClientSettings {
		value := DefaultClientSettingsWithBufferSize(64)
		value.Log = NewNoopLogger()
		value.EncryptionSettings.Mode = EncryptionModeOff
		privateH1SkipUnusedKeyPublication(t, value)
		value.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Unix(1, 0)
		value.ContractManagerSettings.NetworkEventTimeChangeHmac = time.Unix(1, 0)
		value.ContractManagerSettings.LegacyCreateContract = false
		value.SendBufferSettings.ProtocolVersion = version
		value.SendBufferSettings.WindowSizing = WindowSizingConstant
		value.SendBufferSettings.ApplyWindowSizing()
		value.SendBufferSettings.ContractFillFraction = 1
		value.SendBufferSettings.ContractAheadScale = 0
		value.SendBufferSettings.CompactContractHead = false
		value.ReceiveBufferSettings.AckCompressTimeout = 0
		return value
	}
	senderId, receiverId := NewId(), NewId()
	senderSettings := settings()
	senderSettings.SendBufferSettings.ResendQueueBudget = fixture.budget
	senderSettings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
	if configure != nil {
		configure(senderSettings.SendBufferSettings)
	}
	senderAuthority := &privateH1ProofAuthority{t: t, source: senderId, owner: func() *Client { return fixture.sender }, peer: func() *Client { return fixture.receiver }, chain: fixture.chain, firstSmall: firstSmall}
	receiverAuthority := &privateH1ProofAuthority{t: t, source: receiverId, owner: func() *Client { return fixture.receiver }, peer: func() *Client { return fixture.sender }, chain: fixture.chain}
	fixture.sender = NewClient(ctx, senderId, senderAuthority, senderSettings)
	fixture.receiver = NewClient(ctx, receiverId, receiverAuthority, settings())
	t.Cleanup(func() {
		if fixture.releaseBarrier != nil {
			fixture.releaseBarrier()
		}
		cancel()
		for _, done := range fixture.workerDones {
			<-done
		}
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		for _, client := range []*Client{fixture.sender, fixture.receiver, fixture.platform} {
			if client == nil {
				continue
			}
			if err := client.CloseAndWait(closeCtx); err != nil {
				t.Error(err)
			}
			keys := client.ClientKeyManager()
			keys.controlSync.sendLock.Lock()
			publicationCount := keys.controlSync.syncCount
			keys.controlSync.sendLock.Unlock()
			if keys.skipPublication && publicationCount != 0 {
				t.Errorf("joined public-skip fixture published %d background client keys", publicationCount)
			}
		}
		for _, frame := range fixture.heldFrames {
			if frame.bytes != nil {
				MessagePoolReturn(frame.bytes)
				frame.bytes = nil
			}
		}
		for _, route := range []Route{fixture.senderOut, fixture.senderIn, fixture.receiverOut, fixture.receiverIn, fixture.platformInput} {
			for len(route) > 0 {
				MessagePoolReturn(<-route)
			}
		}
		releaseTransferGroupTestWitnesses(t, fixture.witnessFrames, fixture.witnesses)
		assertRetainedBudgetBalance(t, fixture.budget)
	})
	if !fixture.sender.ClientKeyManager().skipPublication || !fixture.receiver.ClientKeyManager().skipPublication {
		t.Fatal("proof fixture's public key-publication skip was not applicable")
	}
	// Platform registration has its own existing OOB owner and completion.
	// Settle it before measuring the retained leaf of peer Transfer work.
	for index, client := range []*Client{fixture.sender, fixture.receiver} {
		registered := make(chan error, 1)
		client.ContractManager().SetProvideModesWithOobAckCallback(map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true}, func(err error) { registered <- err })
		synctest.Wait()
		select {
		case err := <-registered:
			if err != nil {
				t.Fatalf("Network registration failed: %v", err)
			}
		default:
			t.Fatal("Network registration did not complete through its OOB owner")
		}
		if []*privateH1ProofAuthority{senderAuthority, receiverAuthority}[index].provides.Load() != 1 {
			t.Fatal("Network registration callback lacked one actual validated Provide")
		}
	}
	assertRetainedBudgetBalance(t, fixture.budget)
	fixture.transport = newPrivateH1PeerGateway(fixture.receiver.ClientId(), carrier)
	fixture.sender.RouteManager().UpdateTransportWithProperties(fixture.transport, []Route{fixture.senderOut}, TransferCarrierProperties{MaxMessageByteCount: 16 * 1024})
	fixture.sender.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{fixture.senderIn})
	fixture.receiver.RouteManager().UpdateTransport(newPrivateH1PeerGateway(fixture.sender.ClientId(), TransportTypeUnknown), []Route{fixture.receiverOut})
	fixture.receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{fixture.receiverIn})
	fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			fixture.delivered = append(fixture.delivered, bytes.Clone(frame.MessageBytes))
		}
		fixture.deliveredCount += len(frames)
	})

	return fixture
}

// Immutable report identities are observed only after actual platform delivery.
type privateH1PlatformClose struct {
	sourceId   Id
	contractId Id
	reportId   Id
}

// Key observations own fixed copies; the receive callback's frames stay borrowed.
type privateH1PlatformKey struct {
	sourceId          Id
	publicKey         [ed25519.PublicKeySize]byte
	historyDomainHash [32]byte
}

// An optional real ControlId peer serves ongoing reliable close reports on a
// separate destination route. Its workers join with the two existing clients.
func (self *privateH1ProofFixture) startPlatformControl(acknowledge bool) {
	self.t.Helper()
	self.startPlatformControlWithContext(self.ctx, acknowledge, nil)
}

// A context override exists only to force the real parent-before-child
// cancellation boundary; the ordinary fixture uses a valid public key skip.
func (self *privateH1ProofFixture) startPlatformControlWithContext(ctx context.Context, acknowledge bool, configure func(*ClientSettings)) {
	self.t.Helper()
	if self.platform != nil {
		self.t.Fatal("platform control fixture already started")
	}
	settings := DefaultClientSettingsWithBufferSize(64)
	settings.Log = NewNoopLogger()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	privateH1SkipUnusedKeyPublication(self.t, settings)
	settings.ReceiveBufferSettings.AckCompressTimeout = 0
	if configure != nil {
		configure(settings)
	}
	authority := &privateH1ProofAuthority{t: self.t, source: ControlId, owner: func() *Client { return self.platform }}
	self.platform = NewClient(ctx, ControlId, authority, settings)
	self.platformInput = make(Route, 128)
	self.platformReports = make(chan privateH1PlatformClose, 16)
	self.platformKeys = make(chan privateH1PlatformKey, 16)
	self.platform.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			message, err := FromFrame(frame)
			var owner *Client
			for _, client := range []*Client{self.sender, self.receiver, self.platform} {
				if client.ClientId() == source.SourceId {
					owner = client
					break
				}
			}
			if err != nil || owner == nil {
				self.t.Errorf("platform rejected control source/encoding: source=%s type=%v err=%v", source.SourceId, frame.MessageType, err)
				continue
			}
			switch value := message.(type) {
			case *protocol.ClientKey:
				if !privateH1ValidClientKey(owner, value) {
					self.t.Errorf("platform rejected actual source key: source=%s", source.SourceId)
					continue
				}
				observation := privateH1PlatformKey{sourceId: source.SourceId}
				copy(observation.publicKey[:], value.PublicKey)
				copy(observation.historyDomainHash[:], value.HistoryDomainHash)
				select {
				case self.platformKeys <- observation:
				default:
					self.t.Error("platform key observation overflow")
				}
			case *protocol.CloseContract:
				if !privateH1ValidClose(value) || owner == self.platform {
					self.t.Errorf("platform rejected close identity: source=%s", source.SourceId)
					continue
				}
				observation := privateH1PlatformClose{sourceId: source.SourceId, contractId: RequireIdFromBytes(value.ContractId), reportId: RequireIdFromBytes(value.ReportId)}
				select {
				case self.platformReports <- observation:
				default:
					self.t.Error("platform close observation overflow")
				}
			default:
				self.t.Errorf("platform received an unexpected control type: source=%s type=%v", source.SourceId, frame.MessageType)
			}
		}
	})
	self.platform.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{self.platformInput})
	for _, client := range []*Client{self.sender, self.receiver} {
		client.ContractManager().AddNoContractPeer(ControlId)
		self.platform.ContractManager().AddNoContractPeer(client.ClientId())
		client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(ControlId)), []Route{self.platformInput})
	}
	if acknowledge {
		self.enablePlatformAcknowledgements()
	}
}

// Consume one exact actual key publication independently of close reports.
func (self *privateH1ProofFixture) requirePlatformKey(owner *Client) {
	self.t.Helper()
	synctest.Wait()
	select {
	case key := <-self.platformKeys:
		if key.sourceId != owner.ClientId() || !bytes.Equal(key.publicKey[:], owner.ClientKeyManager().PublicKey()) || key.historyDomainHash != owner.settings.ContractManagerSettings.CloseReportDomainHash {
			self.t.Fatal("platform key observation changed its actual source identity")
		}
	default:
		self.t.Fatal("platform did not receive the actual source ClientKey")
	}
	if len(self.platformKeys) != 0 || len(self.platformReports) != 0 {
		self.t.Fatal("key-before-close boundary duplicated a key or observed a premature close")
	}
}

// Installing actual reply routes wakes the existing ACK writers. No encoded
// ACK is constructed and no retained control owner is returned by the fixture.
func (self *privateH1ProofFixture) enablePlatformAcknowledgements() {
	self.t.Helper()
	if self.platform == nil || self.platformAcksEnabled {
		self.t.Fatal("platform acknowledgement routes have an invalid lifetime")
	}
	self.platformAcksEnabled = true
	self.platform.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(self.sender.ClientId())), []Route{self.senderIn})
	self.platform.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(self.receiver.ClientId())), []Route{self.receiverIn})
	synctest.Wait()
}

// Exact contract/report identities prove that a counted close reached the
// platform, independently of the application group's lifecycle measurements.
func (self *privateH1ProofFixture) requirePlatformClose(sourceId Id, contractId Id) {
	self.t.Helper()
	synctest.Wait()
	for len(self.platformReports) > 0 {
		self.platformSeen = append(self.platformSeen, <-self.platformReports)
	}
	for _, report := range self.platformSeen {
		if report.sourceId == sourceId && report.contractId == contractId && report.reportId != (Id{}) {
			return
		}
	}
	self.t.Fatal("actual platform did not receive the expected original close")
}

// A real ACK must release both reliable Transfer and manager report custody;
// merely excluding close observations from the application stream is not enough.
func (self *privateH1ProofFixture) requirePlatformControlsSettled() {
	self.t.Helper()
	synctest.Wait()
	for _, client := range []*Client{self.sender, self.receiver, self.platform} {
		if client == nil {
			continue
		}
		manager := client.ContractManager()
		manager.mutex.Lock()
		pendingReports, pendingWorkers := len(manager.pendingCloseReportKVs), len(manager.closeControlSyncs)
		manager.mutex.Unlock()
		client.sendBuffer.mutex.Lock()
		var controls []*SendSequence
		for _, sequence := range client.sendBuffer.sendSequencesBySequenceId {
			if sequence.destination == ControlId {
				controls = append(controls, sequence)
			}
		}
		client.sendBuffer.mutex.Unlock()
		pendingTransfer := 0
		for _, sequence := range controls {
			pendingTransfer += sequence.resendQueue.Len()
		}
		if pendingReports != 0 || pendingWorkers != 0 || pendingTransfer != 0 {
			self.t.Fatalf("platform ACK left control custody: reports=%d workers=%d transfer=%d", pendingReports, pendingWorkers, pendingTransfer)
		}
		keys := client.ClientKeyManager()
		for _, workers := range []*lifecycleAdmission{keys.workers, keys.controlSync.workers} {
			workers.stateLock.Lock()
			active := workers.activeCount
			workers.stateLock.Unlock()
			if active != 0 {
				self.t.Fatalf("settled platform control retained %d client-key workers", active)
			}
		}
	}
}

// Publishes actual full-MTU TCP packets, retaining only a pool witness of each
// source root. Version-one packet wrappers remain part of the message budget.
func (self *privateH1ProofFixture) frames(count int, version int, configuredMtu ...int) []*protocol.Frame {
	self.t.Helper()
	mtu := DefaultMtu
	if len(configuredMtu) > 0 {
		mtu = configuredMtu[0]
	}
	payload := mtu - Ipv4HeaderSizeWithoutExtensions - TcpHeaderSizeWithoutExtensions
	stream := newFragmentTestStream(4)
	state := ConnectionState{ipVersion: 4, sourceIp: stream.sourceIp, sourcePort: stream.sourcePort,
		destinationIp: stream.destinationIp, destinationPort: stream.destinationPort, receiveSeq: 100, sendSeq: 200, windowSize: 65535}
	frames := make([]*protocol.Frame, count)
	for index := range frames {
		packet := state.tcpPacket(tcpFlagAck, uint32(100+payload*index), bytes.Repeat([]byte{byte(index + 1)}, payload))
		frame, err := ipPacketFromProviderFrame(packet, version)
		if err != nil {
			MessagePoolReturn(packet)
			self.t.Fatal(err)
		}
		if !frame.Raw {
			MessagePoolReturn(packet)
		}
		frames[index] = frame
		self.witnessFrames = append(self.witnessFrames, frame)
		self.witnesses = append(self.witnesses, MessagePoolShareReadOnly(frame.MessageBytes))
	}
	return frames
}

// Controls consume the real receiver contract verifier and ACK worker before
// the caller receives the next application Pack. No ACK is fabricated.
func (self *privateH1ProofFixture) data() *windowRoundFrame {
	self.t.Helper()
	for range cap(self.senderOut) {
		frame := self.take(self.senderOut)
		if frame.pack == nil {
			self.t.Fatal("sender emitted a non-Pack")
		}
		if len(frame.pack.Frames) > 0 {
			return frame
		}
		self.forward(frame, self.receiverIn)
		self.acknowledge()
	}
	self.t.Fatal("application Pack hidden behind a full control route")
	return nil
}

// Waits for actual wire custody under the unchanged owner's ACK-time bound.
// No test reads a mutable resend deadline or mistakes a saved original for a retry.
func (self *privateH1ProofFixture) retry(original *windowRoundFrame) *windowRoundFrame {
	self.t.Helper()
	if original.bytes != nil {
		self.t.Fatal("retry requires the original wire to be dropped")
	}
	sequence := self.sequence()
	bound := sequence.sendBufferSettings.AckTimeout
	if bound <= 0 {
		self.t.Fatal("retry capture requires the existing positive ACK-time bound")
	}
	deadline := time.NewTimer(bound)
	defer deadline.Stop()
	// Move the one actually received share into the existing decoder's input.
	// Its held-frame list owns cleanup; this never writes back to the live route.
	captured := make(Route, 1)
	for range cap(self.senderOut) {
		if sequence.resendQueue.GetBySequenceNumber(original.pack.SequenceNumber) == nil {
			self.t.Fatal("original retry owner disappeared before a peer ACK")
		}
		select {
		case wire := <-self.senderOut:
			captured <- wire
		case <-deadline.C:
			self.t.Fatal("bounded actual retry capture produced no original-identity recovery")
		case <-self.ctx.Done():
			self.t.Fatal("retry capture canceled before actual recovery")
		}
		frame := self.take(captured)
		if frame.pack == nil || !bytes.Equal(frame.pack.SequenceId, original.pack.SequenceId) {
			self.t.Fatal("retry collector received another sequence")
		}
		if frame.pack.SequenceNumber == original.pack.SequenceNumber {
			if !bytes.Equal(frame.pack.MessageId, original.pack.MessageId) {
				self.t.Fatal("retry changed message identity")
			}
			return frame
		}
	}
	self.t.Fatal("bounded actual retry wire count produced no original-identity recovery")
	return nil
}

// Checks the three phases of the one original logical admission, using the
// public observer after its terminal callback has joined the preceding phase.
func privateH1GroupLifecycle(t *testing.T, events <-chan SendPackLifecycleObservation, started SendPackLifecycleObservation, routeError bool, terminalError bool) {
	t.Helper()
	for _, row := range []struct {
		phase  SendPackLifecyclePhase
		failed bool
	}{
		{SendPackLifecyclePhaseFirstRouteWrite, routeError}, {SendPackLifecyclePhaseTerminal, terminalError},
	} {
		select {
		case event := <-events:
			if event.Phase != row.phase || event.Token != started.Token || event.ClientId != started.ClientId ||
				event.DestinationId != started.DestinationId || event.MessageType != started.MessageType || event.AckRequired != started.AckRequired || event.UpstreamRecoverable != started.UpstreamRecoverable || event.HealthProbe != started.HealthProbe || (event.Err != nil) != row.failed {
				t.Fatalf("expanded logical lifecycle=%+v, want phase%d failure%t", event, row.phase, row.failed)
			}
		default:
			t.Fatal("expanded logical group stranded a lifecycle phase")
		}
	}
	if len(events) != 0 {
		t.Fatal("expanded logical group duplicated a lifecycle phase")
	}
}

// Uses the production AEAD serializer, including outer role/companion fields.
func privateH1SealedByteCount(t *testing.T, path TransferPath, plaintext []byte) int {
	t.Helper()
	block, err := aes.NewCipher(bytes.Repeat([]byte{0x73}, 32))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	if aead.NonceSize() != sequenceTlsAeadNonceSize || aead.Overhead() != sendPackH1AeadTagByteCount {
		t.Fatal("wire bound disagrees with production AEAD geometry")
	}
	codec := &sequenceCipher{aead: aead}
	outer, err := codec.SealOuterFrame(path, plaintext, protocol.SequenceRole_SequenceRoleServer, true)
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(outer)
	var envelope protocol.TransferFrame
	if err := ProtoUnmarshal(outer, &envelope); err != nil {
		t.Fatal(err)
	}
	opened, err := codec.Open(envelope.EncryptedTransferFrame)
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(opened)
	if !bytes.Equal(opened, plaintext) {
		t.Fatal("AEAD round trip changed the full head")
	}
	return len(outer)
}

// Builds the exact legacy/v2 full head independently of the new sizing helper.
func privateH1FullWire(t *testing.T, sequence *SendSequence, frames []*protocol.Frame, maximal bool) []byte {
	t.Helper()
	proof, err := ProtoMarshal(sequence.sendContract.contract)
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(proof)
	pack := &protocol.Pack{MessageId: NewId().Bytes(), SequenceId: sequence.sequenceId.Bytes(), SequenceNumber: 1,
		Head: true, Frames: frames, ContractFrame: &protocol.Frame{MessageType: protocol.MessageType_TransferContract, MessageBytes: proof},
		Tag: &protocol.Tag{SendTime: uint64(time.Now().UnixMilli())}}
	if maximal {
		pack.SequenceNumber = ^uint64(0)
		pack.Tag.SendTime = ^uint64(0)
		pack.Nack, pack.ForceStream, pack.CompanionContract, pack.ContractAhead = true, true, true, true
		pack.LogicalLane = 8
		pack.ContractId = sequence.sendContract.contractId.Bytes()
	}
	role, companion := protocol.SequenceRole_SequenceRoleServer, true
	path := sendTransferPath(sequence.client.ClientId(), DestinationId(sequence.destination))
	transfer := &protocol.TransferFrame{TransferPath: path.ToProtobuf(), Pack: pack, SessionRole: &role, SessionCompanion: &companion}
	if sequence.sendBufferSettings.ProtocolVersion < 2 {
		nested, err := ProtoMarshal(pack)
		if err != nil {
			t.Fatal(err)
		}
		defer MessagePoolReturn(nested)
		transfer.Pack = nil
		transfer.Frame = &protocol.Frame{MessageType: protocol.MessageType_TransferPack, MessageBytes: nested}
	} else {
		kind := protocol.MessageType_TransferPack
		transfer.MessageType = &kind
	}
	wire, err := ProtoMarshal(transfer)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

// At product MTU1280, eight raw packets exceed16KiB with a valid3600-byte
// commitment under the unguarded eight-frame counterfactual. Version one is
// an adjacent envelope control; its wrappers already split at the payload cap.
func TestCanonicalH1FullProofEnvelopeSignedWireBoundary(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{1, 2} {
		synctest.Test(t, func(t *testing.T) {
			fixture := privateH1NewFixture(t, version, mib(1), false, TransportTypeH1, nil)
			contract := privateH1SignedProof(t, fixture.sender.ClientId(), fixture.receiver, fixture.chain, 256*1024*1024)
			settings := DefaultSendBufferSettings()
			settings.ProtocolVersion = version
			sequence := NewSendSequence(fixture.ctx, fixture.sender, nil, fixture.receiver.ClientId(), MultiHopId{}, false, false, false, sequenceTlsRoleClient, false, settings)
			defer sequence.Close()
			sequence.sendContract, _ = newSequenceContract(NewNoopLogger(), "proof", contract, 1, 1)
			frames := fixture.frames(8, version, DefaultMtu)
			defer func() {
				for _, frame := range frames {
					MessagePoolReturn(frame.MessageBytes)
				}
			}()
			path := sendTransferPath(fixture.sender.ClientId(), DestinationId(fixture.receiver.ClientId()))
			oldWire := privateH1FullWire(t, sequence, frames, false)
			oldOuter := privateH1SealedByteCount(t, path, oldWire)
			MessagePoolReturn(oldWire)
			if oldOuter <= 16*1024 {
				t.Fatalf("v%d valid large proof did not reproduce old full-head overflow: %d", version, oldOuter)
			}
			// Preserve the real signed3600-byte chain: six must fit, while
			// seven must exceed the actual complete encrypted H1 envelope.
			for _, count := range []int{6, 7} {
				for _, maximal := range []bool{false, true} {
					wire := privateH1FullWire(t, sequence, frames[:count], maximal)
					outer := privateH1SealedByteCount(t, path, wire)
					MessagePoolReturn(wire)
					if (count == 6 && outer > 16*1024) || (count == 7 && outer <= 16*1024) {
						t.Fatalf("v%d full1280 proof count=%d maximal=%t encrypted=%d", version, count, maximal, outer)
					}
				}
			}
			maxFrames, requested := 8, ByteCount(8*DefaultMtu)
			limit := sequence.h1PayloadEnvelopeLimit(frames, maxFrames, requested, 16*1024)
			chunks := 0
			for start := 0; start < len(frames); {
				end := nextSendGroupChunkEndWithLimits(frames, start, maxFrames, limit)
				for _, maximal := range []bool{false, true} {
					wire := privateH1FullWire(t, sequence, frames[start:end], maximal)
					outer := privateH1SealedByteCount(t, path, wire)
					MessagePoolReturn(wire)
					bound := h1FullHeadEnvelopeByteCount(h1FrameWireByteCount(frames[start:end]), proto.Size(contract), true, version < 2)
					if outer > bound || bound > 16*1024 {
						t.Fatalf("v%d bounded full head actual/bound=%d/%d", version, outer, bound)
					}
				}
				if chunks == 0 && end != 6 {
					t.Fatalf("v%d first physical chunk=%d, want6", version, end)
				}
				start, chunks = end, chunks+1
			}
			if chunks != 2 {
				t.Fatalf("v%d large-proof group produced%d chunks", version, chunks)
			}
			t.Logf("H1_FULL_PROOF version=%d certificate=%d contract=%d old_encrypted=%d bounded_payload=%d chunks=%d", version, len(fixture.chain[0])+len(fixture.chain[1]), proto.Size(contract), oldOuter, limit, chunks)
		})
	}
}

// Full-budget promotion retries the original identity while an old carrier
// share is retained, and the actual peer ACK completes exactly one group.
func TestCanonicalH1FullProofLogicalGroupRetryOwnership(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{1, 2} {
		synctest.Test(t, func(t *testing.T) {
			fixture := privateH1NewFixture(t, version, mib(1), false, TransportTypeH1, nil)
			frames := fixture.frames(8, version)
			results := make(chan error, 4)
			admitted, err := fixture.sender.sendGroupWithTimeoutDetailed(frames, fixture.receiver.ClientId(), func(err error) { results <- err }, time.Second)
			if !admitted || err != nil {
				for _, frame := range frames {
					MessagePoolReturn(frame.MessageBytes)
				}
				t.Fatalf("group admission=%t/%v", admitted, err)
			}
			first := fixture.data()
			var last *windowRoundFrame
			wantFirst := 6
			if wantFirst < 8 {
				last = fixture.data()
			}
			if len(first.pack.Frames) != wantFirst || last != nil && len(last.pack.Frames) != 8-wantFirst {
				t.Fatalf("v%d/MTU%d physical first group=%d, want%d", version, DefaultMtu, len(first.pack.Frames), wantFirst)
			}
			sequence := fixture.sequence()
			original := bytes.Clone(first.bytes)
			carrier := MessagePoolShareReadOnly(first.bytes)
			defer func() {
				if carrier != nil {
					MessagePoolReturn(carrier)
				}
			}()
			fixture.drop(first)
			used := fixture.budget.UsedByteCount()
			fixture.budget.SetTotalByteCount(used)
			retry := fixture.retry(first)
			if !retry.pack.Head || retry.pack.ContractFrame == nil || sequence.resendWriteCount.Load() == 0 {
				t.Fatal("real recovery did not write the original full-contract head")
			}
			if !bytes.Equal(carrier, original) || fixture.budget.UsedByteCount() != used {
				t.Fatal("recovery changed the old carrier or borrowed outside its lifetime claim")
			}
			path := sendTransferPath(fixture.sender.ClientId(), DestinationId(fixture.receiver.ClientId()))
			if privateH1SealedByteCount(t, path, retry.bytes) > 16*1024 {
				t.Fatal("actual full-head retry exceeded H1 after encryption")
			}
			MessagePoolReturn(carrier)
			carrier = nil
			fixture.forward(retry, fixture.receiverIn)
			fixture.acknowledge()
			if last != nil {
				if len(results) != 0 {
					t.Fatal("partial group ACK completed the original owner")
				}
				fixture.forward(last, fixture.receiverIn)
				if len(results) != 0 {
					t.Fatal("delivery without the last peer ACK completed the group")
				}
				fixture.acknowledge()
			}
			select {
			case err := <-results:
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("actual peer ACK did not complete the group")
			}
			if len(results) != 0 || len(fixture.delivered) != 8 {
				t.Fatal("logical group completed or delivered more than once")
			}
			for index := range frames {
				if !bytes.Equal(fixture.delivered[index], fixture.witnesses[index]) {
					t.Fatal("physical split changed packet bytes/order")
				}
			}
			assertRetainedBudgetBalance(t, fixture.budget)
		})
	}
}

// The first small proof admits eight frames. A real replacement grant grows the
// proof and divides the remaining eight into six plus two after the first ACK.
func TestCanonicalH1FullProofRotationCompletesExpandedGroup(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, cancelSuffix := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			counts := make(chan int, 4)
			lifecycle := make(chan SendPackLifecycleObservation, 8)
			observeOriginal := func(event SendPackLifecycleObservation) {
				select {
				case lifecycle <- event:
				default:
					panic("lifecycle observer overflow")
				}
			}
			fixture := privateH1NewFixture(t, 2, kib(64), true, TransportTypeH1, func(settings *SendBufferSettings) {
				settings.afterCreateSendGroupCompletionForTest = func(_ sendSequenceId, count int) { counts <- count }
			})
			fixture.startPlatformControl(true)
			frames := fixture.frames(16, 2)
			results := make(chan error, 4)
			admitted, err := fixture.sender.sendGroupWithTimeoutDetailed(frames, fixture.receiver.ClientId(), func(err error) { results <- err }, time.Second, sendPackLifecycleObserverOption{observer: observeOriginal})
			if !admitted || err != nil {
				for _, frame := range frames {
					MessagePoolReturn(frame.MessageBytes)
				}
				t.Fatalf("rotation group admission=%t/%v", admitted, err)
			}
			var started SendPackLifecycleObservation
			select {
			case started = <-lifecycle:
				if started.Phase != SendPackLifecyclePhaseStarted || started.Token == 0 || started.ClientId != fixture.sender.ClientId() || started.DestinationId != fixture.receiver.ClientId() || started.MessageType != frames[0].MessageType || !started.AckRequired {
					t.Fatal("group has no first lifecycle phase")
				}
			default:
				t.Fatal("group admission was not observed")
			}
			first := fixture.data()
			if len(first.pack.Frames) != 8 {
				t.Fatal("small opening proof did not materialize the original eight-frame partition")
			}
			select {
			case count := <-counts:
				if count != 2 {
					t.Fatalf("initial expected chunks=%d", count)
				}
			default:
				t.Fatal("group completion was not created")
			}
			firstSequence := fixture.sequence()
			if len(firstSequence.sendContract.provideTlsCertificate) != 0 {
				t.Fatal("replacement contract crossed the first-chunk ACK memory barrier")
			}
			firstItem := firstSequence.resendQueue.GetBySequenceNumber(first.pack.SequenceNumber)
			nextBound := firstSequence.retainedSendFrameByteCount(MessageByteCount(frames[8:]), len(frames[8:]))
			nextCharge := (&sendItem{}).retainedMemoryByteCount(nextBound, len(frames[8:]), false)
			if firstItem == nil || firstItem.QueueByteCount()+nextCharge <= fixture.budget.TotalByteCount() {
				t.Fatal("current canonical claims do not establish the first-ACK funding barrier")
			}
			firstContract := firstSequence.sendContract.contractId
			fixture.forward(first, fixture.receiverIn)
			fixture.acknowledge()
			middle := fixture.data()
			fixture.requirePlatformClose(fixture.sender.ClientId(), firstContract)
			fixture.requirePlatformControlsSettled()
			sequence := fixture.sequence()
			wantMiddle := 6
			if sequence.sendContract.contractId == firstContract || len(sequence.sendContract.provideTlsCertificate) != 2 || len(middle.pack.Frames) != wantMiddle {
				t.Fatal("actual replacement proof did not produce the bounded remaining partition")
			}
			if len(results) != 0 {
				t.Fatal("first ACK prematurely completed expanded group")
			}
			if cancelSuffix {
				fixture.cancel()
				synctest.Wait()
				select {
				case err := <-results:
					if err == nil {
						t.Fatal("canceled suffix reported delivery")
					}
				default:
					t.Fatal("canceled expanded group stranded terminal ownership")
				}
				if len(results) != 0 {
					t.Fatal("cancellation duplicated the original callback")
				}
				privateH1GroupLifecycle(t, lifecycle, started, true, true)
				return
			}
			fixture.forward(middle, fixture.receiverIn)
			fixture.acknowledge()
			if wantMiddle < 8 {
				last := fixture.data()
				if len(last.pack.Frames) != 8-wantMiddle || len(results) != 0 {
					t.Fatal("second ACK used the stale two-chunk completion count")
				}
				fixture.forward(last, fixture.receiverIn)
				if len(results) != 0 {
					t.Fatal("last delivery bypassed its ACK")
				}
				fixture.acknowledge()
			}
			select {
			case err := <-results:
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("last ACK did not complete expanded group")
			}
			if len(results) != 0 || len(fixture.delivered) != 16 {
				t.Fatal("expanded group ownership/delivery count changed")
			}
			privateH1GroupLifecycle(t, lifecycle, started, false, false)
			fixture.requirePlatformControlsSettled()
			assertRetainedBudgetBalance(t, fixture.budget)
			for index := range frames {
				if !bytes.Equal(fixture.delivered[index], fixture.witnesses[index]) {
					t.Fatal("rotation reordered inner packets")
				}
			}
		})
	}
}

// A blocked actual initial-write hook admits an already-ready burst. Its tails
// return to FIFO in original order when the full-proof wire bound trims it.
func TestCanonicalH1FullProofReadyDrainPreservesOriginalOwners(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var armed atomic.Bool
		entered, release := make(chan struct{}), make(chan struct{})
		var enterOnce, releaseOnce sync.Once
		fixture := privateH1NewFixture(t, 2, mib(1), false, TransportTypeH1, func(settings *SendBufferSettings) {
			settings.afterInitialWriteQueuedForTest = func(_ sendSequenceId, _ uint64) {
				if armed.CompareAndSwap(true, false) {
					enterOnce.Do(func() { close(entered) })
					<-release
				}
			}
		})
		fixture.releaseBarrier = func() { releaseOnce.Do(func() { close(release) }) }
		if admitted, err := fixture.send(32); !admitted || err != nil {
			t.Fatalf("warm admission=%t/%v", admitted, err)
		}
		warm := fixture.data()
		fixture.forward(warm, fixture.receiverIn)
		fixture.acknowledge()
		armed.Store(true)
		if admitted, err := fixture.send(32); !admitted || err != nil {
			t.Fatalf("resident admission=%t/%v", admitted, err)
		}
		<-entered
		resident := fixture.take(fixture.senderOut)
		frames := fixture.frames(8, 2)
		results := make(chan int, 16)
		for index, frame := range frames {
			admitted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(), func(err error) {
				if err != nil {
					results <- -index - 1
				} else {
					results <- index
				}
			}, time.Second)
			if !admitted || err != nil {
				for _, remaining := range frames[index:] {
					MessagePoolReturn(remaining.MessageBytes)
				}
				t.Fatalf("ready frame%d admission=%t/%v", index, admitted, err)
			}
		}
		fixture.releaseBarrier()
		first := fixture.data()
		var last *windowRoundFrame
		wantFirst := 6
		if len(first.pack.Frames) != wantFirst {
			t.Fatalf("ready physical first frames=%d, want%d", len(first.pack.Frames), wantFirst)
		}
		if wantFirst < 8 {
			last = fixture.data()
			if len(last.pack.Frames) != 8-wantFirst {
				t.Fatalf("ready physical tail frames=%d, want%d", len(last.pack.Frames), 8-wantFirst)
			}
		}
		fixture.forward(resident, fixture.receiverIn)
		fixture.acknowledge()
		fixture.forward(first, fixture.receiverIn)
		fixture.acknowledge()
		for want := range wantFirst {
			select {
			case got := <-results:
				if got != want {
					t.Fatalf("ready callback=%d want%d", got, want)
				}
			default:
				t.Fatal("coalesced original owner did not complete")
			}
		}
		if len(results) != 0 {
			t.Fatal("restored tail completed with its previous aggregate")
		}
		if last != nil {
			fixture.forward(last, fixture.receiverIn)
			fixture.acknowledge()
			for want := wantFirst; want < 8; want++ {
				select {
				case got := <-results:
					if got != want {
						t.Fatalf("restored tail callback=%d want%d", got, want)
					}
				default:
					t.Fatal("restored tail stranded")
				}
			}
		}
		if len(fixture.delivered) != 10 || len(results) != 0 {
			t.Fatal("ready burst duplicated an owner or payload")
		}
		for index := range frames {
			if !bytes.Equal(fixture.delivered[index+2], fixture.witnesses[index]) {
				t.Fatal("restored ready tail crossed a sibling")
			}
		}
	})
}

// An indivisible oversize frame reports an unwritten failure exactly once;
// the prior retained identity still receives its real ACK and accepts more.
func TestCanonicalH1FullProofOversizeRefusesWithoutRetiringResident(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := privateH1NewFixture(t, 2, mib(1), false, TransportTypeH1, nil)
		if admitted, err := fixture.send(64); !admitted || err != nil {
			t.Fatalf("resident admission=%t/%v", admitted, err)
		}
		resident := fixture.data()
		sequence := fixture.sequence()
		item := sequence.resendQueue.GetBySequenceNumber(resident.pack.SequenceNumber)
		used := fixture.budget.UsedByteCount()
		nextNumber := sequence.nextSequenceNumber
		frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, Raw: true, MessageBytes: MessagePoolGet(16 * 1024)}
		results := make(chan error, 4)
		admitted, err := fixture.sender.sendGroupWithTimeoutDetailed([]*protocol.Frame{frame}, fixture.receiver.ClientId(), func(err error) { results <- err }, time.Second)
		if !admitted || err != nil {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatalf("oversize fixture did not reach serialized-size admission: %t/%v", admitted, err)
		}
		synctest.Wait()
		select {
		case err := <-results:
			if !errors.Is(err, errSendPackH1Envelope) || !sendPackFailedUnwritten(err) {
				t.Fatalf("oversize result lost cause/unwritten proof: %v", err)
			}
		default:
			t.Fatal("indivisible oversized owner was requeued forever")
		}
		if fixture.sequence() != sequence || sequence.resendQueue.GetBySequenceNumber(resident.pack.SequenceNumber) != item || sequence.nextSequenceNumber != nextNumber || fixture.budget.UsedByteCount() != used || len(results) != 0 {
			t.Fatal("oversize refusal retired/replaced a resident owner")
		}
		fixture.forward(resident, fixture.receiverIn)
		fixture.acknowledge()
		if fixture.ackedCount != 1 {
			t.Fatal("resident lost its actual peer ACK")
		}
		if admitted, err := fixture.send(64); !admitted || err != nil {
			t.Fatalf("post-refusal admission=%t/%v", admitted, err)
		}
		later := fixture.data()
		if !bytes.Equal(later.pack.SequenceId, resident.pack.SequenceId) {
			t.Fatal("oversize refusal tore down the resident sequence")
		}
		fixture.forward(later, fixture.receiverIn)
		fixture.acknowledge()
	})
}

// A supported large proof first uses conservative H3 chunks. The exact retained
// identity then moves to H1 and repairs with its original full signed proof.
func TestCanonicalH1FullProofSupportedCarrierPromotion(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := privateH1NewFixture(t, 2, mib(1), false, TransportTypeH3, nil)
		frames := fixture.frames(2, 2)
		results := make(chan error, 2)
		admitted, err := fixture.sender.sendGroupWithTimeoutDetailed(frames, fixture.receiver.ClientId(), func(err error) { results <- err }, time.Second)
		if !admitted || err != nil {
			for _, frame := range frames {
				MessagePoolReturn(frame.MessageBytes)
			}
			t.Fatalf("H3 group admission=%t/%v", admitted, err)
		}
		first, last := fixture.data(), fixture.data()
		if len(first.pack.Frames) != 1 || len(last.pack.Frames) != 1 {
			t.Fatal("initial H3 path lost its conservative chunk policy")
		}
		fixture.drop(first)
		sequence := fixture.sequence()
		fixture.sender.RouteManager().RemoveTransport(fixture.transport)
		fixture.transport = newPrivateH1PeerGateway(fixture.receiver.ClientId(), TransportTypeH1)
		fixture.sender.RouteManager().UpdateTransportWithProperties(fixture.transport, []Route{fixture.senderOut}, TransferCarrierProperties{MaxMessageByteCount: 16 * 1024})
		synctest.Wait()
		if !sequence.transferFlightPolicy().h1Only {
			t.Fatal("replacement carrier did not publish H1 policy")
		}
		retry := fixture.retry(first)
		if retry.pack.ContractFrame == nil || !retry.pack.Head {
			t.Fatal("promoted carrier did not repair a full head")
		}
		path := sendTransferPath(fixture.sender.ClientId(), DestinationId(fixture.receiver.ClientId()))
		if privateH1SealedByteCount(t, path, retry.bytes) > 16*1024 {
			t.Fatal("supported proof migration exceeded H1")
		}
		fixture.forward(retry, fixture.receiverIn)
		fixture.acknowledge()
		fixture.forward(last, fixture.receiverIn)
		fixture.acknowledge()
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("promoted original group did not ACK")
		}
		if len(results) != 0 || len(fixture.delivered) != 2 {
			t.Fatal("carrier migration changed ownership/delivery")
		}
	})
}

// Reducing a tentatively coalesced debit preserves the separate opening charge,
// minimum accounting, and concurrent NoAck allowance, even after expiry.
func TestCanonicalH1FullProofDebitReductionPreservesOpeningAndMinimum(t *testing.T) {
	for _, row := range [][2]ByteCount{{10240, 8960}, {10240, 10}, {10, 1}, {0, 0}} {
		contract := &sequenceContract{log: NewNoopLogger(), minUpdateByteCount: 64, effectiveTransferByteCount: mib(1), noAckBudgetPublished: true}
		contract.noAckRemainingByteCount.Store(int64(mib(1)))
		if !contract.update(0) || !contract.update(row[0]) {
			t.Fatal("test contract did not admit opening/data")
		}
		before := contract.noAckRemainingByteCount.Load()
		expired := int64(1)
		contract.expirationTimeUnixMilli = &expired
		if !contract.expired() {
			t.Fatal("fixture did not expire after admission")
		}
		contract.reduceUnwrittenDebit(row[0], row[1])
		want := ByteCount(64) + max(ByteCount(64), row[1])
		if contract.unackedByteCount != want || contract.noAckRemainingByteCount.Load() != int64(mib(1)-want) || contract.noAckRemainingByteCount.Load() < before {
			t.Fatal("shrinking debit changed opening/minimum/NoAck credit")
		}
		contract.rollbackUnwritten(row[1])
		if contract.unackedByteCount != 64 {
			t.Fatal("data refusal refunded the independent opening debit")
		}
	}
}

// A valid signed proof can leave room for one tiny frame but less than the
// conservative eight-frame overhead. That remainder must split, not refuse.
func TestCanonicalH1FullProofTightEnvelopeKeepsFittingSingleton(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := privateH1NewFixture(t, 2, mib(1), false, TransportTypeH1, nil)
		contract := privateH1SignedProof(t, fixture.sender.ClientId(), fixture.receiver, fixture.chain, 256*1024*1024)
		var stored protocol.StoredContract
		if err := proto.Unmarshal(contract.StoredContractBytes, &stored); err != nil {
			t.Fatal(err)
		}
		secret, _ := fixture.receiver.ContractManager().GetProvideSecretKey(protocol.ProvideMode_Network)
		resize := func(padding int) {
			stored.Principal = "h1-tight-proof/" + string(bytes.Repeat([]byte{'p'}, padding))
			value, err := proto.Marshal(&stored)
			if err != nil {
				t.Fatal(err)
			}
			contract.StoredContractBytes = value
			contract.StoredContractHmac = SignStoredContract(fixture.receiver.ContractManager().settings, secret, value)
		}
		low, high := 0, 16*1024
		for low < high {
			middle := low + (high-low+1)/2
			resize(middle)
			if h1FullHeadEnvelopeByteCount(0, proto.Size(contract), true, false) <= 16*1024-16 {
				low = middle
			} else {
				high = middle - 1
			}
		}
		resize(low)
		if !VerifyStoredContract(fixture.receiver.ContractManager().settings, secret, contract.StoredContractBytes, contract.StoredContractHmac) {
			t.Fatal("tight contract lost its actual authority signature")
		}
		settings := DefaultSendBufferSettings()
		settings.ProtocolVersion = 2
		sequence := NewSendSequence(fixture.ctx, fixture.sender, nil, fixture.receiver.ClientId(), MultiHopId{}, false, false, false, sequenceTlsRoleClient, false, settings)
		defer sequence.Close()
		sequence.sendContract, _ = newSequenceContract(NewNoopLogger(), "tight-proof", contract, 1, 1)
		frames := make([]*protocol.Frame, 8)
		for index := range frames {
			frames[index] = &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, Raw: true, MessageBytes: MessagePoolGet(1)}
		}
		defer func() {
			for _, frame := range frames {
				MessagePoolReturn(frame.MessageBytes)
			}
		}()
		if sequence.h1PayloadEnvelopeLimit(frames, 8, 8*DefaultMtu, 16*1024) != 0 || !sequence.h1FramesEnvelopeFits(frames[:1], 16*1024) {
			t.Fatal("fixture did not isolate conservative zero headroom with a fitting singleton")
		}
		group := &SendPack{Frames: frames, logicalGroup: true, groupChunkMaxFrames: 8, groupChunkMaxMessageByteCount: 8 * DefaultMtu}
		group.groupCompletion = newSendGroupCompletion(group, 1)
		end, err := sequence.boundH1LogicalGroup(group, 16*1024)
		if err != nil || end != 1 || group.groupChunkMaxFrames != 1 || group.groupCompletion.chunkCount != 8 {
			t.Fatalf("fitting singleton was refused or lost its completion count: end%d err%v count%d", end, err, group.groupCompletion.chunkCount)
		}
		path := sendTransferPath(fixture.sender.ClientId(), DestinationId(fixture.receiver.ClientId()))
		wire := privateH1FullWire(t, sequence, frames[:end], true)
		defer MessagePoolReturn(wire)
		if privateH1SealedByteCount(t, path, wire) > 16*1024 {
			t.Fatal("fitting singleton exceeded the real encrypted envelope")
		}
	})
}

// Existing custom H1 carriers may accept more than the runtime floor. An
// advertised larger maximum and legacy unknown metadata must retain that path.
func TestCanonicalH1FullProofCustomCarrierCeilingPreservesLargeMessage(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, maximum := range []int{0, 32 * 1024} {
		synctest.Test(t, func(t *testing.T) {
			fixture := privateH1NewFixture(t, 2, mib(1), false, TransportTypeH1, nil)
			fixture.sender.RouteManager().UpdateTransportWithProperties(fixture.transport, []Route{fixture.senderOut}, TransferCarrierProperties{MaxMessageByteCount: maximum})
			if admitted, err := fixture.send(17000); !admitted || err != nil {
				t.Fatalf("custom H1 admission=%t/%v", admitted, err)
			}
			frame := fixture.data()
			sequence := fixture.sequence()
			policy := sequence.transferFlightPolicy()
			if !policy.h1Only || policy.h1MaxMessageByteCount != maximum || policy.h1MessageLimitsKnown != (maximum > 0) {
				t.Fatalf("custom H1 ceiling changed to%+v", policy)
			}
			path := sendTransferPath(fixture.sender.ClientId(), DestinationId(fixture.receiver.ClientId()))
			outer := privateH1SealedByteCount(t, path, frame.bytes)
			if outer <= 16*1024 || outer > 32*1024 {
				t.Fatalf("custom carrier fixture did not cross only the default floor: %d", outer)
			}
			fixture.forward(frame, fixture.receiverIn)
			fixture.acknowledge()
			if fixture.ackedCount != 1 || len(fixture.delivered) != 1 {
				t.Fatal("custom H1 message lost its real receive/ACK path")
			}
		})
	}
}

// Every property update publishes a fresh immutable generation. Unknown
// metadata adds no invented ceiling; an eligible smaller known H1 does bound it.
func TestCanonicalH1FullProofCarrierCeilingSnapshotGenerations(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer func() { cancel(); closeTransferGroupTestClient(t, client) }()
	manager := client.RouteManager()
	writer := manager.OpenMultiRouteWriter(DestinationId(NewId()))
	defer manager.CloseMultiRouteWriter(writer)
	provider := writer.(transferFlightPolicyProvider)
	large := NewSendGatewayTransportWithType(TransportTypeH1)
	small := NewSendGatewayTransportWithType(TransportTypeH1)
	mixed := NewSendGatewayTransportWithType(TransportTypeH3)
	largeRoute, smallRoute, mixedRoute := make(Route, 1), make(Route, 1), make(Route, 1)
	generation := provider.transferFlightPolicy().generation
	require := func(h1 bool, maximum int, known bool) {
		policy := provider.transferFlightPolicy()
		if policy.generation <= generation || policy.h1Only != h1 || policy.h1MaxMessageByteCount != maximum || policy.h1MessageLimitsKnown != known {
			t.Fatalf("carrier property generation=%+v, previous%d wantH1%t maximum%d", policy, generation, h1, maximum)
		}
		generation = policy.generation
	}
	manager.UpdateTransportWithProperties(large, []Route{largeRoute}, TransferCarrierProperties{MaxMessageByteCount: 32 * 1024})
	require(true, 32*1024, true)
	manager.UpdateTransportWithProperties(small, []Route{smallRoute}, TransferCarrierProperties{MaxMessageByteCount: 16 * 1024})
	require(true, 16*1024, true)
	manager.RemoveTransport(small)
	require(true, 32*1024, true)
	manager.UpdateTransport(large, []Route{largeRoute})
	require(true, 0, false)
	manager.UpdateTransportWithProperties(small, []Route{smallRoute}, TransferCarrierProperties{MaxMessageByteCount: 16 * 1024})
	require(true, 16*1024, false)
	manager.RemoveTransport(small)
	require(true, 0, false)
	manager.UpdateTransportWithProperties(mixed, []Route{mixedRoute}, TransferCarrierProperties{MaxMessageByteCount: 64 * 1024})
	require(false, 0, false)
	manager.RemoveTransport(mixed)
	require(true, 0, false)
}

// A contract wait can outlive the carrier generation used for initial chunk
// selection. The still-unwritten group must use the new16KiB ceiling on return.
func TestCanonicalH1FullProofContractWaitRefreshesCarrierCeiling(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var enterOnce, releaseOnce sync.Once
		fixture := privateH1NewFixture(t, 2, mib(1), false, TransportTypeH1, func(settings *SendBufferSettings) {
			settings.beforeTakeContractForTest = func(_ sendSequenceId) { enterOnce.Do(func() { close(entered); <-release }) }
		})
		fixture.releaseBarrier = func() { releaseOnce.Do(func() { close(release) }) }
		fixture.sender.RouteManager().UpdateTransportWithProperties(fixture.transport, []Route{fixture.senderOut}, TransferCarrierProperties{MaxMessageByteCount: 32 * 1024})
		frames := fixture.frames(8, 2)
		results := make(chan error, 2)
		type admission struct {
			accepted bool
			err      error
		}
		accepted, done := make(chan admission, 1), make(chan struct{})
		fixture.workerDones = append(fixture.workerDones, done)
		go func() {
			defer close(done)
			ok, err := fixture.sender.sendGroupWithTimeoutDetailed(frames, fixture.receiver.ClientId(), func(err error) { results <- err }, time.Second)
			if !ok {
				for _, frame := range frames {
					MessagePoolReturn(frame.MessageBytes)
				}
			}
			accepted <- admission{ok, err}
		}()
		<-entered
		sequence := fixture.sequence()
		if sequence.transferFlightPolicy().h1MaxMessageByteCount != 32*1024 {
			t.Fatal("contract wait did not start under the larger advertised carrier")
		}
		fixture.sender.RouteManager().UpdateTransportWithProperties(fixture.transport, []Route{fixture.senderOut}, TransferCarrierProperties{MaxMessageByteCount: 16 * 1024})
		fixture.releaseBarrier()
		admitted := <-accepted
		<-done
		if !admitted.accepted || admitted.err != nil {
			t.Fatalf("post-wait group admission=%+v", admitted)
		}
		first := fixture.data()
		wantFirst := 6
		if len(first.pack.Frames) != wantFirst {
			t.Fatalf("contract wait used a stale larger ceiling: first frames%d want%d", len(first.pack.Frames), wantFirst)
		}
		var last *windowRoundFrame
		if wantFirst < 8 {
			last = fixture.data()
		}
		fixture.forward(first, fixture.receiverIn)
		fixture.acknowledge()
		if last != nil {
			fixture.forward(last, fixture.receiverIn)
			fixture.acknowledge()
		}
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("post-wait bounded group did not receive its actual peer ACK")
		}
		if len(results) != 0 || len(fixture.delivered) != 8 {
			t.Fatal("carrier generation change duplicated or lost an owner")
		}
	})
}

// Losing the last known ceiling during a real contract wait restores the old
// H1 opening partition before ownership or the logical cursor advances.
func TestCanonicalH1FullProofContractWaitLosesKnownCeiling(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var enterOnce, releaseOnce sync.Once
		fixture := privateH1NewFixture(t, 2, mib(1), false, TransportTypeH1, func(settings *SendBufferSettings) {
			settings.beforeTakeContractForTest = func(_ sendSequenceId) { enterOnce.Do(func() { close(entered); <-release }) }
		})
		fixture.releaseBarrier = func() { releaseOnce.Do(func() { close(release) }) }
		fixture.sender.RouteManager().UpdateTransportWithProperties(fixture.transport, []Route{fixture.senderOut}, TransferCarrierProperties{MaxMessageByteCount: 32 * 1024})
		frames := fixture.frames(8, 2)
		results := make(chan error, 2)
		type admission struct {
			accepted bool
			err      error
		}
		accepted, done := make(chan admission, 1), make(chan struct{})
		fixture.workerDones = append(fixture.workerDones, done)
		go func() {
			defer close(done)
			ok, err := fixture.sender.sendGroupWithTimeoutDetailed(frames, fixture.receiver.ClientId(), func(err error) { results <- err }, time.Second)
			if !ok {
				for _, frame := range frames {
					MessagePoolReturn(frame.MessageBytes)
				}
			}
			accepted <- admission{ok, err}
		}()
		<-entered
		sequence := fixture.sequence()
		if sequence.transferFlightPolicy().h1MaxMessageByteCount != 32*1024 {
			t.Fatal("contract wait did not start under the larger advertised carrier")
		}
		fixture.sender.RouteManager().UpdateTransport(fixture.transport, []Route{fixture.senderOut})
		fixture.releaseBarrier()
		admitted := <-accepted
		<-done
		if !admitted.accepted || admitted.err != nil {
			t.Fatalf("post-wait group admission=%+v", admitted)
		}
		policy := sequence.transferFlightPolicy()
		if !policy.h1Only || policy.h1MessageLimitsKnown || policy.h1MaxMessageByteCount != 0 {
			t.Fatalf("contract wait did not publish the actual unknown replacement: %+v", policy)
		}
		for chunk := range 4 {
			wire := fixture.data()
			if len(wire.pack.Frames) != 2 {
				t.Fatalf("unknown replacement inherited enlarged admission: chunk%d frames%d want2", chunk, len(wire.pack.Frames))
			}
			if len(results) != 0 {
				t.Fatal("repartition completed before its final peer ACK")
			}
			fixture.forward(wire, fixture.receiverIn)
			fixture.acknowledge()
		}
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("post-wait bounded group did not receive its actual peer ACK")
		}
		if len(results) != 0 || len(fixture.delivered) != 8 {
			t.Fatal("carrier generation change duplicated or lost an owner")
		}
	})
}

// An actual retired-contract close may outlive the application ACK. Its separate
// observer and retained charge cannot complete or consume the original group.
func TestCanonicalH1CloseLifecycleKeepsOriginalAndControlCustody(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		applicationEvents := make(chan SendPackLifecycleObservation, 8)
		controlEvents := make(chan SendPackLifecycleObservation, 16)
		fixture := privateH1NewFixture(t, 2, kib(64), true, TransportTypeH1, func(settings *SendBufferSettings) {
			settings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
				select {
				case controlEvents <- event:
				default:
					panic("close lifecycle observation overflow")
				}
			}
		})
		fixture.startPlatformControl(false)
		observeOriginal := func(event SendPackLifecycleObservation) {
			select {
			case applicationEvents <- event:
			default:
				panic("application lifecycle observation overflow")
			}
		}
		frames := fixture.frames(16, 2)
		results := make(chan error, 4)
		admitted, err := fixture.sender.sendGroupWithTimeoutDetailed(frames, fixture.receiver.ClientId(), func(err error) { results <- err }, time.Second, sendPackLifecycleObserverOption{observer: observeOriginal})
		if !admitted || err != nil {
			for _, frame := range frames {
				MessagePoolReturn(frame.MessageBytes)
			}
			t.Fatalf("close isolation group admission=%t/%v", admitted, err)
		}
		var started SendPackLifecycleObservation
		select {
		case started = <-applicationEvents:
			if started.Phase != SendPackLifecyclePhaseStarted || started.ClientId != fixture.sender.ClientId() || started.DestinationId != fixture.receiver.ClientId() || started.MessageType != frames[0].MessageType || !started.AckRequired || started.Token == 0 {
				t.Fatal("per-original lifecycle started with another identity")
			}
		default:
			t.Fatal("per-original application lifecycle has no Started observation")
		}
		first := fixture.data()
		if len(first.pack.Frames) != 8 {
			t.Fatal("close isolation lost the original eight-frame partition")
		}
		sequence := fixture.sequence()
		policy := sequence.transferFlightPolicy()
		if !policy.h1Only || !policy.h1MessageLimitsKnown || policy.h1MaxMessageByteCount != 16*1024 {
			t.Fatal("platform-only route polluted peer H1 eligibility")
		}
		firstContract := sequence.sendContract.contractId
		fixture.forward(first, fixture.receiverIn)
		fixture.acknowledge()
		for _, count := range []int{6, 2} {
			frame := fixture.data()
			if len(frame.pack.Frames) != count || len(results) != 0 {
				t.Fatal("close control changed the remaining partition or completed it early")
			}
			fixture.forward(frame, fixture.receiverIn)
			fixture.acknowledge()
		}
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("peer ACK did not complete the original application group")
		}
		if len(results) != 0 || len(fixture.delivered) != 16 || sequence.resendQueue.Len() != 0 || fixture.budget.UsedByteCount() <= 0 {
			t.Fatal("application completion consumed close credit or retained peer originals")
		}
		for index, witness := range fixture.witnesses {
			if !bytes.Equal(fixture.delivered[index], witness) {
				t.Fatal("close control changed peer payload bytes or order")
			}
		}
		privateH1GroupLifecycle(t, applicationEvents, started, false, false)
		fixture.requirePlatformClose(fixture.sender.ClientId(), firstContract)
		var controlStarted SendPackLifecycleObservation
		for _, phase := range []SendPackLifecyclePhase{SendPackLifecyclePhaseStarted, SendPackLifecyclePhaseFirstRouteWrite} {
			select {
			case event := <-controlEvents:
				if event.Phase != phase || event.ClientId != started.ClientId || event.DestinationId != ControlId || event.MessageType != protocol.MessageType_TransferCloseContract || event.Token == 0 || event.Token == started.Token || !event.AckRequired || event.Err != nil {
					t.Fatalf("platform close lifecycle lost its distinct original: %+v", event)
				}
				if phase == SendPackLifecyclePhaseStarted {
					controlStarted = event
				} else if event.Token != controlStarted.Token || event.AckRequired != controlStarted.AckRequired || event.UpstreamRecoverable != controlStarted.UpstreamRecoverable || event.HealthProbe != controlStarted.HealthProbe {
					t.Fatal("platform close route phase changed its original token")
				}
			default:
				t.Fatal("platform close lacked its actual started/route phase")
			}
		}
		if len(controlEvents) != 0 {
			t.Fatal("withheld platform ACK falsely completed a close")
		}
		fixture.enablePlatformAcknowledgements()
		fixture.requirePlatformControlsSettled()
		select {
		case event := <-controlEvents:
			if event.Phase != SendPackLifecyclePhaseTerminal || event.Token != controlStarted.Token || event.ClientId != controlStarted.ClientId || event.DestinationId != ControlId || event.MessageType != controlStarted.MessageType || event.AckRequired != controlStarted.AckRequired || event.UpstreamRecoverable != controlStarted.UpstreamRecoverable || event.HealthProbe != controlStarted.HealthProbe || event.Err != nil {
				t.Fatalf("actual platform ACK changed close completion identity: %+v", event)
			}
		default:
			t.Fatal("actual platform ACK did not complete its close original")
		}
		if len(controlEvents) != 0 || len(applicationEvents) != 0 {
			t.Fatal("one of the independent originals duplicated a lifecycle phase")
		}
		assertRetainedBudgetBalance(t, fixture.budget)
	})
}

// A key has already completed before this group. The real exhausted first
// grant must then close independently while all sixteen peer payloads receive ACKs.
func (self *privateH1ProofFixture) completeKeyThenCloseGroup() {
	self.t.Helper()
	frames := self.frames(16, 2)
	results := make(chan error, 2)
	admitted, err := self.sender.sendGroupWithTimeoutDetailed(frames, self.receiver.ClientId(), func(err error) { results <- err }, time.Second)
	if !admitted || err != nil {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
		self.t.Fatalf("key-before-close peer admission=%t/%v", admitted, err)
	}
	first := self.data()
	if len(first.pack.Frames) != 8 {
		self.t.Fatal("key publication changed the opening eight-frame partition")
	}
	sequence := self.sequence()
	policy := sequence.transferFlightPolicy()
	if !policy.h1Only || !policy.h1MessageLimitsKnown || policy.h1MaxMessageByteCount != 16*1024 {
		self.t.Fatal("key/control routes changed the actual peer H1 policy")
	}
	if len(sequence.sendContract.provideTlsCertificate) != 0 {
		self.t.Fatal("key-before-close lost the existing first-chunk ACK funding barrier")
	}
	firstContract := sequence.sendContract.contractId
	self.forward(first, self.receiverIn)
	self.acknowledge()
	for _, count := range []int{6, 2} {
		frame := self.data()
		if len(frame.pack.Frames) != count || len(results) != 0 {
			self.t.Fatal("key-before-close group lost an unacknowledged payload partition")
		}
		self.forward(frame, self.receiverIn)
		self.acknowledge()
	}
	self.requirePlatformClose(self.sender.ClientId(), firstContract)
	select {
	case err := <-results:
		if err != nil {
			self.t.Fatal(err)
		}
	default:
		self.t.Fatal("key-before-close group lacked the real final peer ACK")
	}
	self.requirePlatformControlsSettled()
	if len(results) != 0 || len(self.delivered) != 16 || len(self.platformSeen) != 1 || len(self.platformKeys) != 0 {
		self.t.Fatal("key-before-close completed or delivered another original")
	}
	for index, witness := range self.witnesses {
		if !bytes.Equal(self.delivered[index], witness) {
			self.t.Fatal("key-before-close changed peer payload bytes/order")
		}
	}
	assertRetainedBudgetBalance(self.t, self.budget)
}

// A public reliable ControlSync sends the real owner key before any close.
// Withholding its actual ACK proves it has its own retained owner and terminal.
func TestCanonicalH1PlatformClientKeyBeforeCloseUsesActualAck(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := privateH1NewFixture(t, 2, kib(64), true, TransportTypeH1, nil)
		fixture.startPlatformControl(false)
		control := NewControlSync(fixture.ctx, fixture.sender, "proof-fixture-explicit-client-key")
		t.Cleanup(func() {
			if err := control.closeAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		})
		frame, err := ToFrame(fixture.sender.ClientKeyManager().clientKeyMessage(fixture.sender.ClientKeyManager().PublicKey()), 2)
		if err != nil {
			t.Fatal(err)
		}
		acks := make(chan error, 2)
		control.Send(frame, nil, func(err error) { acks <- err })
		fixture.requirePlatformKey(fixture.sender)
		if len(acks) != 0 || fixture.budget.UsedByteCount() <= 0 {
			t.Fatal("key delivery without its platform ACK lost reliable ownership")
		}
		fixture.enablePlatformAcknowledgements()
		select {
		case err := <-acks:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("actual platform ACK did not complete the original ClientKey")
		}
		if err := control.closeAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(acks) != 0 || fixture.sender.ClientKeyManager().Registered() {
			t.Fatal("native key ACK duplicated completion or invented processed registration")
		}
		fixture.requirePlatformControlsSettled()
		assertRetainedBudgetBalance(t, fixture.budget)
		fixture.completeKeyThenCloseGroup()
	})
}

// The old fixture released publishers on the parent's cancellation before
// cancellation reached each child. Force that real ordering for ControlId's
// own constructor publisher; do not claim a canceled client can admit a send.
func TestCanonicalH1PlatformParentCancellationBeforeChildAcceptsLateKey(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := privateH1NewFixture(t, 2, kib(64), true, TransportTypeH1, nil)
		parent, cancelParent := context.WithCancel(fixture.ctx)
		delayed := &receiptFixtureDelayedCancelContext{Context: parent, entered: make(chan struct{}), release: make(chan struct{})}
		release := sync.OnceFunc(func() { close(delayed.release) })
		// This cleanup runs before the base fixture joins all three clients.
		t.Cleanup(func() {
			cancelParent()
			release()
			delayed.callbacks.Wait()
		})
		publisherEntered := make(chan struct{})
		fixture.startPlatformControlWithContext(delayed, true, func(settings *ClientSettings) {
			settings.SkipClientKeyPublication = false
			settings.beforeClientKeyPublishForTest = func() {
				close(publisherEntered)
				<-parent.Done()
			}
		})
		<-publisherEntered
		if delayed.registrations.Load() != 1 || fixture.platform.ctx.Err() != nil || len(fixture.platformKeys) != 0 {
			t.Fatal("constructor did not establish one live, held platform publisher")
		}
		cancelParent()
		<-delayed.entered
		synctest.Wait()
		if parent.Err() != context.Canceled || fixture.platform.ctx.Err() != nil {
			t.Fatal("fixture failed to hold parent-to-child cancellation propagation")
		}
		fixture.requirePlatformKey(fixture.platform)
		keys := fixture.platform.ClientKeyManager()
		keys.controlSync.sendLock.Lock()
		publications := keys.controlSync.syncCount
		keys.controlSync.sendLock.Unlock()
		if publications != 1 || keys.skipPublication || keys.Registered() {
			t.Fatal("late key did not traverse the actual unskipped native publisher exactly once")
		}
		fixture.requirePlatformControlsSettled()
		fixture.completeKeyThenCloseGroup()
		release()
		delayed.callbacks.Wait()
		if err := fixture.platform.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if fixture.platform.ctx.Err() != context.Canceled {
			t.Fatal("joined platform retained a live child context")
		}
	})
}
