// Flow cancellation must release a pure acknowledgement already waiting in
// provider admission. The real NAT/provider constructors deliver every packet;
// only the Transfer consumer is paused at its existing bounded admission gate.
package connect

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"testing/synctest"
)

// This is the stable Network wire enum value. Keeping the untyped fixture
// constant here avoids adding a parent-package import of its protocol child.
const tcpRetirementNetworkProvideMode = 1

// Two in-memory upstreams belong to successive generations of the same tuple.
// A paused Transfer sequence owns at most two admitted packets throughout.
type tcpProviderRetirementFixture struct {
	t                   *testing.T
	client              *Client
	nat                 *LocalUserNat
	provider            *RemoteUserNatProvider
	transfer            *SendSequence
	source              TransferPath
	path                *IpPath
	created             chan *TcpSequence
	upstreams           []net.Conn
	sequenceSockets     []net.Conn
	oldAckPackets       chan []byte
	oldAckWorkerDone    chan struct{}
	oldChildJoinStarted chan struct{}
	oldAckWitness       []byte
}

// The inspected result owns only scalar packet metadata, never borrowed bytes.
type tcpProviderRetirementPacket struct {
	syn              bool
	ack              bool
	fin              bool
	rst              bool
	ackNumber        uint32
	payloadByteCount int
}

// Creates all producers through production constructors. No socket factory
// opens a host socket, and no route or Transfer acknowledgement is synthesized.
func newTcpProviderRetirementFixture(t *testing.T) *tcpProviderRetirementFixture {
	return newTcpProviderRetirementFixtureWithSetup(t, nil, nil)
}

// Optional socket/read barriers and per-sequence hooks are installed before
// any flow starts; existing retirement callers retain the same fixture.
func newTcpProviderRetirementFixtureWithSetup(t *testing.T, wrapSocket func(net.Conn) net.Conn, setupSequence func(*TcpSequence)) *tcpProviderRetirementFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	fixture := &tcpProviderRetirementFixture{
		t:                   t,
		source:              SourceId(NewId()),
		path:                &IpPath{Version: 4, Protocol: IpProtocolTcp, SourceIp: net.IPv4(192, 0, 2, 3).To4(), SourcePort: 40001, DestinationIp: net.IPv4(198, 51, 100, 7).To4(), DestinationPort: 443},
		created:             make(chan *TcpSequence, 2),
		oldAckPackets:       make(chan []byte, 2),
		oldAckWorkerDone:    make(chan struct{}),
		oldChildJoinStarted: make(chan struct{}),
	}
	clientSettings := DefaultClientSettings()
	clientSettings.Log = NewNoopLogger()
	clientSettings.EncryptionSettings.Mode = EncryptionModeOff
	clientSettings.SendBufferSettings.LogicalDataLaneCount = 1
	clientSettings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	fixture.client = NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)

	sockets := make(chan net.Conn, 2)
	for range 2 {
		sequenceSocket, upstream := net.Pipe()
		fixture.sequenceSockets = append(fixture.sequenceSockets, sequenceSocket)
		fixture.upstreams = append(fixture.upstreams, upstream)
		if wrapSocket != nil {
			sequenceSocket = wrapSocket(sequenceSocket)
		}
		sockets <- sequenceSocket
	}
	natSettings := DefaultLocalUserNatSettingsWithBufferSize(8)
	natSettings.Log = NewNoopLogger()
	natSettings.TcpBufferSettings.AckCompressTimeout = 0
	natSettings.TcpBufferSettings.WriteBatchSize = 1
	natSettings.TcpBufferSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
			select {
			case socket := <-sockets:
				return socket, nil
			case <-dialCtx.Done():
				return nil, dialCtx.Err()
			}
		},
	}
	natSettings.TcpBufferSettings.beforeSequenceRunWithStateForTest = func(sequence *TcpSequence) {
		if sequence.initialSynSeq == 1000 {
			sequence.afterPureAckBuildForTest = func(packet []byte) {
				fixture.oldAckPackets <- MessagePoolShareReadOnly(packet)
			}
			sequence.afterPureAckWorkerStopForTest = func() { close(fixture.oldAckWorkerDone) }
			sequence.beforeChildWorkersWaitForTest = func() { close(fixture.oldChildJoinStarted) }
		}
		if setupSequence != nil {
			setupSequence(sequence)
		}
		fixture.created <- sequence
	}
	fixture.nat = NewLocalUserNat(ctx, "synthetic tcp retirement custody", natSettings)
	providerSettings := DefaultRemoteUserNatProviderSettings()
	providerSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
	providerSettings.WriteTimeout = -1
	providerSettings.ReturnSendAbandonTimeout = 0
	fixture.provider = NewRemoteUserNatProvider(fixture.client, fixture.nat, providerSettings)

	id := sendSequenceId{Destination: fixture.source.SourceId, EncryptionRole: sequenceTlsRoleClient}
	fixture.transfer = installProviderReturnTestSequence(t, fixture.provider, fixture.client, id)
	fixture.transfer.client = fixture.client
	fixture.transfer.sendBuffer = fixture.client.sendBuffer
	fixture.transfer.destination = fixture.source.SourceId
	fixture.transfer.encryptionRole = sequenceTlsRoleClient
	fixture.transfer.sendBufferSettings = clientSettings.SendBufferSettings
	fixture.transfer.log = NewNoopLogger()
	fixture.transfer.packs = make(chan *SendPack, 2)
	fixture.transfer.packAdmission = newSendPackAdmission(2)
	fixture.transfer.resendQueue = newResendQueue(clientSettings.SendBufferSettings.ResendQueueBudget, 0)
	t.Cleanup(func() {
		// Cleanup may close the shared owner after all flow-only assertions.
		// This also joins the deliberately failing pre-fix case safely.
		fixture.provider.Close()
		cancel()
		for _, socket := range fixture.upstreams {
			socket.Close()
		}
		for _, socket := range fixture.sequenceSockets {
			socket.Close()
		}
		if err := fixture.nat.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		if err := fixture.client.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		fixture.discardQueued()
		fixture.transfer.resendQueue.Clear()
		if fixture.oldAckWitness != nil {
			MessagePoolReturn(fixture.oldAckWitness)
			fixture.oldAckWitness = nil
		}
		for {
			select {
			case packet := <-fixture.oldAckPackets:
				MessagePoolReturn(packet)
			default:
				return
			}
		}
	})
	return fixture
}

// Takes one generated input on success. The checksum includes the current
// inner acknowledgement, so ordinary parser and established-flow checks run.
func (self *tcpProviderRetirementFixture) send(flags byte, sequenceNumber, ackNumber uint32, payload []byte) {
	self.t.Helper()
	packet := MessagePoolCopy(ipOosTcpPacketSequence(self.path, flags, sequenceNumber, payload))
	tcp := packet[Ipv4HeaderSizeWithoutExtensions:]
	binary.BigEndian.PutUint32(tcp[8:12], ackNumber)
	binary.BigEndian.PutUint16(tcp[16:18], 0)
	binary.BigEndian.PutUint16(tcp[16:18], ipPathTransportChecksum(self.path, ipProtocolNumberTcp, tcp))
	if !self.nat.SendPacket(self.source, tcpRetirementNetworkProvideMode, packet, 0) {
		MessagePoolReturn(packet)
		self.t.Fatal("synthetic packet was refused before the TCP flow")
	}
}

// Quiescence makes this a positive constructor observation, not queue polling.
func (self *tcpProviderRetirementFixture) nextSequence(want uint32) *TcpSequence {
	self.t.Helper()
	synctest.Wait()
	select {
	case sequence := <-self.created:
		if sequence.initialSynSeq != want || sequence.provideMode.String() != "Network" {
			self.t.Fatalf("created generation=%d mode=%s, want %d Network", sequence.initialSynSeq, sequence.provideMode, want)
		}
		return sequence
	default:
		self.t.Fatalf("no TCP generation for SYN %d after quiescence", want)
		return nil
	}
}

// Each byte is really read from the flow's upstream and admitted through the
// constructor's batch return callback; the paused consumer keeps both slots.
func (self *tcpProviderRetirementFixture) fillFromSocket() {
	self.t.Helper()
	for _, value := range []byte{'a', 'b'} {
		if n, err := self.upstreams[0].Write([]byte{value}); n != 1 || err != nil {
			self.t.Fatalf("upstream fill=(%d, %v)", n, err)
		}
		synctest.Wait()
	}
	self.transfer.packAdmission.mutex.Lock()
	count, capacity := self.transfer.packAdmission.count, self.transfer.packAdmission.capacity
	self.transfer.packAdmission.mutex.Unlock()
	if count != 2 || capacity != 2 || len(self.transfer.packs) != 2 || self.transfer.resendCapacityUnavailable.Load() {
		self.t.Fatalf("not at exact two-slot admission pressure: count=%d capacity=%d queued=%d", count, capacity, len(self.transfer.packs))
	}
}

// The real socket writer must deliver the upload. A joined in-memory read and
// synctest quiescence supply the barrier without sleeps or socket timeouts.
func (self *tcpProviderRetirementFixture) upload(sequence *TcpSequence, upstream net.Conn, sequenceNumber uint32, payload string) {
	self.t.Helper()
	sequence.mutex.Lock()
	ackNumber := sequence.receiveSeq
	sequence.mutex.Unlock()
	type readResult struct {
		payload []byte
		err     error
	}
	read := make(chan readResult, 1)
	go func() {
		packet := make([]byte, len(payload))
		_, err := io.ReadFull(upstream, packet)
		read <- readResult{payload: packet, err: err}
	}()
	self.send(tcpFlagAck, sequenceNumber, ackNumber, []byte(payload))
	synctest.Wait()
	select {
	case result := <-read:
		if result.err != nil || string(result.payload) != payload {
			self.t.Fatalf("upstream upload=%q err=%v, want %q", result.payload, result.err, payload)
		}
	default:
		self.t.Fatal("TCP upload did not reach its in-memory socket after quiescence")
	}
}

// Harness disposal releases only actual admitted owners. It never invokes an
// acknowledgement callback or claims application delivery for these packets.
func (self *tcpProviderRetirementFixture) discardQueued() {
	for {
		select {
		case pack := <-self.transfer.packs:
			pack.returnFrames()
			pack.releaseRaw()
		default:
			return
		}
	}
}

// Releases the two pre-existing data owners, then drains the bounded set of
// possible pre-fix controls: old ack, old socket-close reset, and new syn-ack.
// This follows the retirement oracle; it never supplies its cancellation cause.
func (self *tcpProviderRetirementFixture) releasePressure() []tcpProviderRetirementPacket {
	self.t.Helper()
	for range 2 {
		pack := <-self.transfer.packs
		pack.returnFrames()
		pack.releaseRaw()
	}
	var controls []tcpProviderRetirementPacket
	for range 3 {
		synctest.Wait()
		controls = append(controls, self.takeControls()...)
	}
	synctest.Wait()
	self.transfer.packAdmission.mutex.Lock()
	waiterCount, count := len(self.transfer.packAdmission.waiters), self.transfer.packAdmission.count
	self.transfer.packAdmission.mutex.Unlock()
	if waiterCount != 0 || count != 0 {
		self.t.Fatalf("bounded control drain left admission ownership: waiters=%d count=%d", waiterCount, count)
	}
	return controls
}

// Returns scalar controls and releases each Transfer-owned pooled share once.
func (self *tcpProviderRetirementFixture) takeControls() []tcpProviderRetirementPacket {
	self.t.Helper()
	var controls []tcpProviderRetirementPacket
	for {
		select {
		case pack := <-self.transfer.packs:
			func() {
				defer pack.releaseRaw()
				defer pack.returnFrames()
				packet := providerCallbackQueuePacketBytes(self.t, pack)
				var path IpPath
				if _, err := parseIpPathWithPayloadBorrowed(packet, &path); err != nil {
					self.t.Fatalf("decode admitted TCP control: %v", err)
				}
				controls = append(controls, tcpProviderRetirementPacket{
					syn: path.Syn, ack: path.Ack, fin: path.Fin, rst: path.Rst,
					ackNumber:        path.AckSequenceNumber,
					payloadByteCount: path.TcpPayloadByteCount,
				})
			}()
		default:
			return controls
		}
	}
}

// Exercises replacement and explicit flow cancellation at the same real
// admission boundary. Shared lifecycle cancellation is reserved for cleanup.
func runTcpProviderRetirementCustody(t *testing.T, replaceWhileBlocked bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newTcpProviderRetirementFixture(t)
		fixture.send(tcpFlagSyn, 1000, 0, nil)
		old := fixture.nextSequence(1000)
		synAck := fixture.takeControls()
		if len(synAck) != 1 || !synAck[0].syn || !synAck[0].ack || synAck[0].ackNumber != 1001 {
			t.Fatalf("initial constructor-wired handshake=%+v", synAck)
		}
		fixture.fillFromSocket()
		started := make(chan struct{}, 4)
		attempts := make(chan providerReturnSendResult, 4)
		fixture.provider.returnSendStarted = func() { started <- struct{}{} }
		fixture.provider.afterReturnSendAttemptForTest = func(result providerReturnSendResult) { attempts <- result }
		fixture.upload(old, fixture.upstreams[0], 1001, "x")
		select {
		case <-started:
		default:
			t.Fatal("old pure acknowledgement did not enter provider return admission")
		}
		select {
		case fixture.oldAckWitness = <-fixture.oldAckPackets:
		default:
			t.Fatal("the real pure-acknowledgement worker built no retained packet")
		}
		select {
		case result := <-attempts:
			t.Fatalf("old pure acknowledgement completed instead of waiting for capacity: %+v", result)
		default:
		}
		fixture.transfer.packAdmission.mutex.Lock()
		waiters := fixture.transfer.packAdmission.waiters
		var oldWaiter *sendPackAdmissionWaiter
		if len(waiters) == 1 {
			oldWaiter = waiters[0]
		}
		fixture.transfer.packAdmission.mutex.Unlock()
		if oldWaiter == nil {
			t.Fatal("old acknowledgement is not the one explicit Transfer admission waiter")
		}

		var replacement *TcpSequence
		if replaceWhileBlocked {
			fixture.send(tcpFlagSyn, 2000, 0, nil)
			replacement = fixture.nextSequence(2000)
			if replacement == old {
				t.Fatal("different SYN reused the old sequence object")
			}
		} else {
			old.Cancel()
			synctest.Wait()
		}
		if old.ctx.Err() == nil {
			t.Fatal("flow cancellation was not published")
		}
		if fixture.provider.ctx.Err() != nil || fixture.client.Ctx().Err() != nil || fixture.nat.ctx.Err() != nil {
			t.Fatal("flow-only retirement canceled a shared owner")
		}
		select {
		case <-fixture.oldChildJoinStarted:
		default:
			t.Error("old sequence did not reach its child-worker join after cancellation")
		}
		select {
		case <-fixture.oldAckWorkerDone:
		default:
			t.Error("canceled TCP generation still owns its pure-acknowledgement worker inside provider admission")
		}
		select {
		case <-old.retirementDone:
		default:
			t.Error("canceled TCP generation did not retire while provider and Transfer capacity remained live and unchanged")
		}
		fixture.transfer.packAdmission.mutex.Lock()
		oldStillWaiting := false
		for _, waiter := range fixture.transfer.packAdmission.waiters {
			oldStillWaiting = oldStillWaiting || waiter == oldWaiter
		}
		count := fixture.transfer.packAdmission.count
		fixture.transfer.packAdmission.mutex.Unlock()
		if oldStillWaiting || count != 2 {
			t.Errorf("flow cancellation did not withdraw only its waiter: old_waiting=%t admitted=%d, want false and 2", oldStillWaiting, count)
		}
		if !MessagePoolReturn(fixture.oldAckWitness) {
			t.Error("canceled flow still owns a pooled pure acknowledgement at the retirement barrier")
		}
		fixture.oldAckWitness = nil

		controls := fixture.releasePressure()
		for _, packet := range controls {
			if !packet.syn && packet.ack && !packet.fin && !packet.rst && packet.payloadByteCount == 0 && packet.ackNumber == 1002 {
				t.Error("retired generation's acknowledgement entered Transfer after capacity returned")
			}
		}
		select {
		case <-old.retirementDone:
		default:
			t.Fatal("old generation still owns a worker after bounded cleanup drain")
		}
		if !replaceWhileBlocked {
			fixture.send(tcpFlagSyn, 2000, 0, nil)
			replacement = fixture.nextSequence(2000)
			controls = fixture.takeControls()
		}
		var replacementSynAcks int
		for _, packet := range controls {
			if packet.syn && packet.ack && packet.ackNumber == 2001 {
				replacementSynAcks++
			}
		}
		if replacementSynAcks != 1 || replacement.ctx.Err() != nil {
			t.Fatalf("replacement handshake is not healthy: synacks=%d controls=%+v err=%v", replacementSynAcks, controls, replacement.ctx.Err())
		}
		fixture.upload(replacement, fixture.upstreams[1], 2001, "healthy replacement")
		controls = fixture.takeControls()
		if len(controls) != 1 || controls[0].syn || !controls[0].ack || controls[0].fin || controls[0].rst || controls[0].payloadByteCount != 0 || controls[0].ackNumber != 2001+uint32(len("healthy replacement")) {
			t.Fatalf("new generation lost ordinary upload/ack progress: %+v", controls)
		}
		if fixture.provider.ctx.Err() != nil || fixture.client.Ctx().Err() != nil || replacement.ctx.Err() != nil {
			t.Fatal("replacement progress relied on closing a shared owner")
		}
	})
}

// A new SYN cancels the previous tuple generation while its exact pure ack is
// durably waiting on Transfer admission. Retirement must not await a free slot.
func TestProviderTcpReplacementRetiresBlockedPureAck(t *testing.T) {
	runTcpProviderRetirementCustody(t, true)
}

// Explicit flow cancellation shares the same custody obligation even when no
// replacement SYN is present to create or otherwise wake a second producer.
func TestProviderTcpCancellationRetiresBlockedPureAck(t *testing.T) {
	runTcpProviderRetirementCustody(t, false)
}
