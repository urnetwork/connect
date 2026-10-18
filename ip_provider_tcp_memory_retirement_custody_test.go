// Saturates both prepaid return workspaces with healthy real socket callbacks,
// then retires a third flow whose pure ack is blocked before Transfer admission.
package connect

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// Reuses the unchanged retirement fixture's inspection and ownership helpers,
// but constructs a genuine two-MiB NAT/provider profile and four socket flows.
func newTcpProviderMemoryRetirementFixture(t *testing.T, budget *TransferMemoryBudget) *tcpProviderRetirementFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	fixture := &tcpProviderRetirementFixture{
		t:                   t,
		source:              SourceId(NewId()),
		path:                &IpPath{Version: 4, Protocol: IpProtocolTcp, SourceIp: net.IPv4(192, 0, 2, 3).To4(), SourcePort: 40001, DestinationIp: net.IPv4(198, 51, 100, 7).To4(), DestinationPort: 443},
		created:             make(chan *TcpSequence, 4),
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

	sockets := make(chan net.Conn, 4)
	for range 4 {
		sequenceSocket, upstream := net.Pipe()
		fixture.sequenceSockets = append(fixture.sequenceSockets, sequenceSocket)
		fixture.upstreams = append(fixture.upstreams, upstream)
		sockets <- sequenceSocket
	}
	natSettings := DefaultLocalUserNatSettingsWithBufferSize(8)
	natSettings.Log = NewNoopLogger()
	natSettings.MemoryBudget = budget
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
		fixture.created <- sequence
	}
	fixture.nat = NewLocalUserNat(ctx, "synthetic budgeted tcp retirement custody", natSettings)
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

// Both healthy socket callbacks retain their real consumed byte behind the
// unchanged two-slot Transfer pressure; no test-only workspace reservation is
// substituted for those owners. A third old flow must retire independently.
func runTcpProviderMemoryRetirementCustody(t *testing.T, replaceWhileBlocked bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	var budget *TransferMemoryBudget
	synctest.Test(t, func(t *testing.T) {
		budget = NewTransferMemoryBudget(mib(2))
		fixture := newTcpProviderMemoryRetirementFixture(t, budget)
		if fixture.provider.tcpReturnMemory == nil || fixture.nat.settings.MemoryBudget != budget {
			t.Fatal("fixture did not enter the bounded-memory constructor profile")
		}
		fixture.send(tcpFlagSyn, 1000, 0, nil)
		old := fixture.nextSequence(1000)
		if controls := fixture.takeControls(); len(controls) != 1 || !controls[0].syn || controls[0].ackNumber != 1001 {
			t.Fatalf("old handshake=%+v", controls)
		}
		fixture.path.SourcePort = 40002
		fixture.send(tcpFlagSyn, 3000, 0, nil)
		firstSibling := fixture.nextSequence(3000)
		if controls := fixture.takeControls(); len(controls) != 1 || !controls[0].syn || controls[0].ackNumber != 3001 {
			t.Fatalf("first sibling handshake=%+v", controls)
		}
		fixture.path.SourcePort = 40003
		fixture.send(tcpFlagSyn, 4000, 0, nil)
		secondSibling := fixture.nextSequence(4000)
		if controls := fixture.takeControls(); len(controls) != 1 || !controls[0].syn || controls[0].ackNumber != 4001 {
			t.Fatalf("second sibling handshake=%+v", controls)
		}
		fixture.path.SourcePort = 40001
		fixture.fillFromSocket()
		var started atomic.Int32
		fixture.provider.returnSendStarted = func() { started.Add(1) }
		for i, value := range []byte{'u', 'v'} {
			if n, err := fixture.upstreams[i+1].Write([]byte{value}); n != 1 || err != nil {
				t.Fatalf("sibling socket write=(%d,%v)", n, err)
			}
			synctest.Wait()
		}
		if fixture.provider.tcpReturnMemory.UsedByteCount() != kib(64) || started.Load() != 2 {
			t.Fatalf("healthy callbacks did not occupy both prepaid slots: bytes=%d sends=%d", fixture.provider.tcpReturnMemory.UsedByteCount(), started.Load())
		}
		fixture.transfer.packAdmission.mutex.Lock()
		waiters := append([]*sendPackAdmissionWaiter(nil), fixture.transfer.packAdmission.waiters...)
		fixture.transfer.packAdmission.mutex.Unlock()
		if len(waiters) != 2 {
			t.Fatal("healthy socket callbacks were not the two actual Transfer waiters")
		}
		fixture.upload(old, fixture.upstreams[0], 1001, "x")
		fixture.oldAckWitness = <-fixture.oldAckPackets
		fixture.provider.memoryOperations.stateLock.Lock()
		active := fixture.provider.memoryOperations.activeCount
		fixture.provider.memoryOperations.stateLock.Unlock()
		if started.Load() != 2 || active != 3 || fixture.provider.tcpReturnMemory.UsedByteCount() != kib(64) {
			t.Fatalf("old ack did not stop at the third prepaid-memory wait: sends=%d active=%d bytes=%d", started.Load(), active, fixture.provider.tcpReturnMemory.UsedByteCount())
		}

		var replacement *TcpSequence
		wantActive := 2
		if replaceWhileBlocked {
			fixture.send(tcpFlagSyn, 2000, 0, nil)
			replacement = fixture.nextSequence(2000)
			wantActive++
		} else {
			old.Cancel()
			synctest.Wait()
		}
		if old.ctx.Err() == nil || fixture.provider.ctx.Err() != nil || fixture.client.Ctx().Err() != nil || fixture.nat.ctx.Err() != nil {
			t.Fatal("the old flow alone was not canceled with shared parents still live")
		}
		if firstSibling.ctx.Err() != nil || secondSibling.ctx.Err() != nil {
			t.Fatal("retiring the old flow canceled a healthy workspace owner")
		}
		select {
		case <-fixture.oldAckWorkerDone:
		default:
			t.Error("canceled flow still owns its pure-ack worker at prepaid return-memory admission")
		}
		select {
		case <-old.retirementDone:
		default:
			t.Error("canceled flow did not retire while both healthy return-memory slots remained held")
		}
		fixture.provider.memoryOperations.stateLock.Lock()
		active = fixture.provider.memoryOperations.activeCount
		fixture.provider.memoryOperations.stateLock.Unlock()
		if active != wantActive {
			t.Errorf("old callbacks retained memory-operation ownership: active=%d want=%d", active, wantActive)
		}
		fixture.transfer.packAdmission.mutex.Lock()
		stillExact := len(fixture.transfer.packAdmission.waiters) == 2 &&
			fixture.transfer.packAdmission.waiters[0] == waiters[0] &&
			fixture.transfer.packAdmission.waiters[1] == waiters[1]
		count := fixture.transfer.packAdmission.count
		fixture.transfer.packAdmission.mutex.Unlock()
		if !stillExact || count != 2 || fixture.provider.tcpReturnMemory.UsedByteCount() != kib(64) {
			t.Error("old retirement consumed or released a healthy sibling's unchanged memory/Transfer capacity")
		}
		if !MessagePoolReturn(fixture.oldAckWitness) {
			t.Error("old pure-ack packet still has a production share at the full-memory retirement barrier")
		}
		fixture.oldAckWitness = nil

		// Only now release the two original data packs. At most five pending
		// returns existed: two siblings, old ack/reset, replacement syn-ack.
		for range 2 {
			pack := <-fixture.transfer.packs
			pack.returnFrames()
			pack.releaseRaw()
		}
		var controls []tcpProviderRetirementPacket
		for range 6 {
			synctest.Wait()
			controls = append(controls, fixture.takeControls()...)
		}
		synctest.Wait()
		for _, packet := range controls {
			if !packet.syn && packet.ack && !packet.fin && !packet.rst && packet.payloadByteCount == 0 && packet.ackNumber == 1002 {
				t.Error("retired old acknowledgement was admitted after a sibling workspace became available")
			}
		}
		select {
		case <-old.retirementDone:
		default:
			t.Fatal("old flow did not retire after bounded pressure cleanup")
		}
		if !replaceWhileBlocked {
			fixture.send(tcpFlagSyn, 2000, 0, nil)
			replacement = fixture.nextSequence(2000)
			controls = fixture.takeControls()
		}
		synAcks := 0
		for _, packet := range controls {
			if packet.syn && packet.ack && packet.ackNumber == 2001 {
				synAcks++
			}
		}
		if synAcks != 1 || replacement.ctx.Err() != nil {
			t.Fatalf("budgeted replacement handshake=%+v error=%v", controls, replacement.ctx.Err())
		}
		fixture.upload(replacement, fixture.upstreams[3], 2001, "healthy budgeted replacement")
		controls = fixture.takeControls()
		if len(controls) != 1 || !controls[0].ack || controls[0].syn || controls[0].rst ||
			controls[0].ackNumber != 2001+uint32(len("healthy budgeted replacement")) {
			t.Fatalf("budgeted replacement lost ordinary upload/ack progress: %+v", controls)
		}
		if fixture.provider.tcpReturnMemory.UsedByteCount() != 0 ||
			firstSibling.ctx.Err() != nil || secondSibling.ctx.Err() != nil ||
			fixture.provider.ctx.Err() != nil || fixture.client.Ctx().Err() != nil {
			t.Fatal("healthy budgeted progress left a workspace or canceled a sibling/shared owner")
		}
	})
	if stats := budget.Stats(); stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
		t.Fatalf("closed bounded-memory fixture retained a budget owner: %+v", stats)
	}
}

// A replacement SYN must cancel its old generation before either healthy
// sibling returns its prepaid workspace or a Transfer admission becomes free.
func TestProviderTcpReplacementRetiresWhileReturnMemorySlotsRemainFull(t *testing.T) {
	runTcpProviderMemoryRetirementCustody(t, true)
}

// Explicit flow cancellation has the same pre-memory custody requirement,
// with no replacement worker available to accidentally supply the wakeup.
func TestProviderTcpCancellationRetiresWhileReturnMemorySlotsRemainFull(t *testing.T) {
	runTcpProviderMemoryRetirementCustody(t, false)
}
