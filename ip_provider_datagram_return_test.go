package connect

import (
	"bytes"
	"context"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func newProviderDatagramReturnTest(t *testing.T) (*windowRoundFixture, *RemoteUserNatProvider, *UdpSequence, *TransferMemoryBudget) {
	return newProviderDatagramReturnPolicyTest(t, false)
}

func newProviderDatagramReturnPolicyTest(t *testing.T, noAck ...bool) (*windowRoundFixture, *RemoteUserNatProvider, *UdpSequence, *TransferMemoryBudget) {
	t.Helper()
	root := NewTransferMemoryBudget(kib(512))
	t.Cleanup(func() { assertRetainedBudgetBalance(t, root) })
	fixture := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
		settings.ResendQueueBudget = NewTransferMemoryBudgetWithParent(root.TotalByteCount(), root)
		settings.ResendQueueRetainedByteAccounting = true
		settings.SequenceBufferSize = 1
		settings.AckTimeout = 100 * time.Millisecond
		settings.MinResendInterval = 20 * time.Millisecond
		settings.RttMinResendInterval = 20 * time.Millisecond
		settings.MaxResendInterval = 40 * time.Millisecond
	}, nil)
	ctx, cancel := context.WithCancel(fixture.ctx)
	settings := DefaultRemoteUserNatProviderSettings()
	if len(noAck) > 0 {
		settings.UdpTransferNoAck = noAck[0]
	}
	settings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
	provider := &RemoteUserNatProvider{
		ctx: ctx, cancel: cancel, client: fixture.sender, settings: settings,
		memoryOperations:    newLifecycleAdmission(),
		localUserNat:        &LocalUserNat{ctx: ctx},
		securityPolicy:      DisableSecurityPolicyWithStats(ctx, DefaultSecurityPolicyStatsCollector()),
		packetStatsCounters: &packetStatsCounters{},
		sourceProvideMode:   map[Id]protocol.ProvideMode{},
	}
	provider.datagramSender = newProviderDatagramSender(provider)
	t.Cleanup(func() {
		cancel()
		<-provider.datagramSender.done
		provider.memoryOperations.close()
		<-provider.memoryOperations.Done()
	})
	udpSettings := DefaultUdpBufferSettingsWithBufferSize(1)
	udpSettings.MemoryBudget = root
	udpSettings.ReadBufferByteCount = 4096
	udpSettings.Log = NewNoopLogger()
	sequence := NewUdpSequence(ctx, nil, SourceId(fixture.receiver.ClientId()), protocol.ProvideMode_Network, 4,
		net.IPv4(192, 0, 2, 1).To4(), 42000, net.IPv4(203, 0, 113, 7).To4(), 8080, udpSettings)
	if sequence == nil {
		t.Fatal("flow admission")
	}
	sequence.sharedSocketLifecycle = true
	sequence.prepareReturnReadCallback = provider.datagramSender.prepareUdp
	t.Cleanup(sequence.Close)
	return fixture, provider, sequence, root
}

func TestProviderDatagramReturnReliableOwnerSurvivesFlowRetirement(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, provider, sequence, root := newProviderDatagramReturnTest(t)
		lease, err := sequence.prepareReturnRead(1000)
		if err != nil || lease == nil {
			t.Fatalf("prepare: %v", err)
		}
		payload := bytes.Repeat([]byte{0x5a}, 1000)
		packets, err := sequence.DataPackets(payload, len(payload), sequence.udpBufferSettings.Mtu)
		if err != nil {
			lease.abort()
			t.Fatal(err)
		}
		wanted := bytes.Clone(packets[0])
		lease.commit(packets)
		wire := fixture.takePack(0)
		if wire.pack.Nack || !bytes.Equal(wire.pack.Frames[0].MessageBytes, wanted) {
			t.Fatal("provider datagram was not the exact ACK-backed return")
		}
		synctest.Wait()
		if sequence.returnReadPending.Load() {
			t.Fatal("first reliable write did not release the next socket-read quantum")
		}
		sequence.Close()
		if root.UsedByteCount() <= providerDatagramAckMemoryByteCount {
			t.Fatal("flow retirement released the unacknowledged reliable flight")
		}
		fixture.drop(wire)
		time.Sleep(150 * time.Millisecond) // past ordinary AckTimeout
		synctest.Wait()
		retry := fixture.recovery(wire)
		fixture.forward(retry, fixture.receiverIn)
		fixture.acknowledge()
		if fixture.deliveredCount != 1 || provider.CongestionDropStats().ReturnSendPacketCount != 0 {
			t.Fatalf("reliable return failed: delivered=%d drops=%+v", fixture.deliveredCount, provider.CongestionDropStats())
		}
		assertRetainedBudgetBalance(t, root)
	})
}

func TestProviderDatagramReadCreditCoversMinimumMtuFragmentation(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		for _, legacy := range []bool{false, true} {
			settings := DefaultUdpBufferSettingsWithBufferSize(1)
			settings.ReadBufferByteCount = 4096
			sequence := NewUdpSequence(context.Background(), nil, SourceId(NewId()), protocol.ProvideMode_Network, version,
				net.ParseIP("192.0.2.1").To4(), 42000, net.ParseIP("203.0.113.7").To4(), 8080, settings)
			if version == 6 {
				sequence.sourceIp = net.ParseIP("2001:db8::1")
				sequence.destinationIp = net.ParseIP("2001:db8::2")
			}
			sequence.sharedSocketLifecycle = true
			for _, size := range []int{1, 1000, 4096} {
				prepaid := providerUdpReadCredit(sequence, size, legacy)
				packets, err := sequence.DataPackets(make([]byte, size), size, ipMinimumPathMtu(version))
				if err != nil {
					t.Fatal(err)
				}
				actual := ByteCount(0)
				for _, packet := range packets {
					actual += providerDatagramPacketCredit(len(packet), legacy)
					MessagePoolReturn(packet)
				}
				if actual > prepaid {
					t.Fatalf("v%d legacy=%t payload=%d: actual=%d prepaid=%d", version, legacy, size, actual, prepaid)
				}
			}
			sequence.Close()
		}
	}
}

func TestProviderDatagramCancelJoinsAcceptedUnwrittenPack(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, provider, sequence, _ := newProviderDatagramReturnTest(t)
		// This peer deliberately has no contract. The queued bytes cannot
		// reach serialization, but Pack has transferred their ownership.
		manager := fixture.sender.ContractManager()
		manager.mutex.Lock()
		manager.sendNoContractClientIds[fixture.receiver.ClientId()] = false
		manager.mutex.Unlock()
		contractWait := make(chan struct{}, 1)
		fixture.sender.sendBuffer.beforeTakeContractForTest = func(sendSequenceId) {
			select {
			case contractWait <- struct{}{}:
			default:
			}
		}
		lease, err := sequence.prepareReturnRead(1000)
		if err != nil || lease == nil {
			t.Fatalf("prepare: %v", err)
		}
		packets, err := sequence.DataPackets(make([]byte, 1000), 1000, sequence.udpBufferSettings.Mtu)
		if err != nil {
			lease.abort()
			t.Fatal(err)
		}
		lease.commit(packets)
		synctest.Wait()
		select {
		case <-contractWait:
		default:
			t.Fatal("fixture did not reach accepted-but-unwritten ownership")
		}
		provider.cancel()
		synctest.Wait()
		select {
		case <-provider.datagramSender.done:
		default:
			t.Fatal("provider cancellation did not join the unwritten Pack")
		}
		if fixture.sender.ctx.Err() != nil {
			t.Fatal("provider cancellation closed the shared Client")
		}
		if used := fixture.sender.settings.SendBufferSettings.ResendQueueBudget.UsedByteCount(); used != 0 {
			t.Fatalf("provider cancellation left accepted unwritten bytes charged: %d", used)
		}
		if sequence.returnReadPending.Load() {
			t.Fatal("canceled queued owner retained the socket-read lease")
		}
		manager.AddNoContractPeer(fixture.receiver.ClientId())
		// The same destination remains usable and no canceled data Pack
		// consumed a sequence number or left a receiver-visible gap.
		fixture.forward(fixture.write(100), fixture.receiverIn)
		fixture.acknowledge()
	})
}

func TestProviderDatagramCancelAfterWriteDoesNotReportAdmissionDrop(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, provider, sequence, _ := newProviderDatagramReturnTest(t)
		lease, err := sequence.prepareReturnRead(1000)
		if err != nil || lease == nil {
			t.Fatalf("prepare: %v", err)
		}
		// Drive this one actor manually so cancellation deterministically
		// wins its next step after the physical writer has completed.
		owner := lease.consumer.(*providerDatagramReturn)
		owner.sender = &providerDatagramSender{provider: provider, wake: make(chan struct{}, 1)}
		defer func() {
			if owner.item.sourceLifecycle != nil {
				provider.cancel()
				fixture.sender.CloseAndWait(context.Background())
				owner.step(time.Millisecond)
			}
		}()
		packets, err := sequence.DataPackets(make([]byte, 1000), 1000, sequence.udpBufferSettings.Mtu)
		if err != nil {
			lease.abort()
			t.Fatal(err)
		}
		lease.commit(packets)
		owner.sender.takeAll()
		if !owner.step(time.Millisecond) {
			t.Fatal("actor did not retain its pending write owner")
		}
		wire := fixture.takePack(0)
		provider.cancel()
		if owner.step(time.Millisecond) {
			t.Fatal("written actor did not detach on provider cancellation")
		}
		if drops := provider.CongestionDropStats(); drops.ReturnSendPacketCount != 0 {
			t.Fatalf("accepted physical write reported as admission loss: %+v", drops)
		}
		fixture.forward(wire, fixture.receiverIn)
		fixture.acknowledge()
	})
}

func TestProviderDatagramCancelBeforeFirstWritePreservesMaterializedOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, provider, sequence, _ := newProviderDatagramReturnTest(t)
		// Fill the fixture's sole route rather than adding a second transport:
		// an additional transport would leave the old ready route selectable.
		for range cap(fixture.senderOut) {
			fixture.senderOut <- MessagePoolCopy([]byte{0})
		}
		lease, err := sequence.prepareReturnRead(1000)
		if err != nil || lease == nil {
			t.Fatalf("prepare: %v", err)
		}
		owner := lease.consumer.(*providerDatagramReturn)
		owner.sender = &providerDatagramSender{provider: provider, wake: make(chan struct{}, 1)}
		defer func() {
			if owner.item.sourceLifecycle != nil {
				provider.cancel()
				fixture.sender.CloseAndWait(context.Background())
				owner.step(time.Millisecond)
			}
		}()
		packets, err := sequence.DataPackets(make([]byte, 1000), 1000, sequence.udpBufferSettings.Mtu)
		if err != nil {
			lease.abort()
			t.Fatal(err)
		}
		lease.commit(packets)
		owner.sender.takeAll()
		if !owner.step(time.Millisecond) {
			t.Fatal("actor did not admit its pending write")
		}
		synctest.Wait()
		written, _, _ := owner.ack.disposition()
		if written || !owner.ack.handoff.transferred() {
			t.Fatal("fixture did not hold a materialized flight before its first physical write")
		}
		provider.cancel()
		if owner.step(time.Millisecond) || sequence.returnReadPending.Load() {
			t.Fatal("provider close waited for a physical write or peer ACK")
		}
		if used := fixture.sender.settings.SendBufferSettings.ResendQueueBudget.UsedByteCount(); used <= providerDatagramAckMemoryByteCount {
			t.Fatalf("unwritten materialized flight lost its charge: %d", used)
		}
		for range cap(fixture.senderOut) {
			MessagePoolReturn(<-fixture.senderOut)
		}
		wire := fixture.takePack(0)
		fixture.forward(wire, fixture.receiverIn)
		fixture.acknowledge()
		if drops := provider.CongestionDropStats(); drops.ReturnSendPacketCount != 0 {
			t.Fatalf("materialized owner reported as admission loss: %+v", drops)
		}
	})
}

func TestProviderDatagramReceiverInjectionOwnsAcknowledgement(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, _, sequence, _ := newProviderDatagramReturnTest(t)
		entered, release := make(chan struct{}), make(chan struct{})
		defer close(release)
		// Use the production final device callback, not a Transfer observer.
		remote := &RemoteUserNatClient{
			securityPolicy: DisableSecurityPolicy(),
			receivePacketCallback: func(_ TransferPath, _ protocol.ProvideMode, path *IpPath, packet []byte) {
				if path.Protocol != IpProtocolUdp || len(packet) != 1028 {
					t.Error("receiver did not retain the exact provider UDP return")
				}
				close(entered)
				<-release
			},
		}
		unsubscribe := fixture.receiver.AddReceiveCallback(remote.ClientReceive)
		defer unsubscribe()
		lease, err := sequence.prepareReturnRead(1000)
		if err != nil || lease == nil {
			t.Fatalf("prepare: %v", err)
		}
		packets, err := sequence.DataPackets(make([]byte, 1000), 1000, sequence.udpBufferSettings.Mtu)
		if err != nil {
			lease.abort()
			t.Fatal(err)
		}
		lease.commit(packets)
		fixture.forward(fixture.takePack(0), fixture.receiverIn)
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("device injection callback was not entered")
		}
		if len(fixture.receiverOut) != 0 {
			t.Fatal("receiver acknowledged before the device injection released its borrowed bytes")
		}
		if used := fixture.sender.settings.SendBufferSettings.ResendQueueBudget.UsedByteCount(); used <= providerDatagramAckMemoryByteCount {
			t.Fatal("blocked device injection released reliable sender ownership")
		}
		// A send releases this one wait without closing twice in cleanup.
		release <- struct{}{}
		fixture.acknowledge()
	})
}

func TestProviderDatagramActualUdpSocketAndProviderCloseBeforeAck(t *testing.T) {
	for _, noAck := range []bool{false, true} {
		name := "ack"
		if noAck {
			name = "noack"
		}
		t.Run(name, func(t *testing.T) { testProviderDatagramActualUdpSocketAndProviderClose(t, noAck) })
	}
}

func TestProviderDatagramActualUdpSocketDefaultNoAck(t *testing.T) {
	if !DefaultRemoteUserNatProviderSettings().UdpTransferNoAck {
		t.Fatal("provider default lost established-UDP NoAck policy")
	}
	testProviderDatagramActualUdpSocketAndProviderClose(t)
}

func testProviderDatagramActualUdpSocketAndProviderClose(t *testing.T, noAckOverride ...bool) {
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := NewTransferMemoryBudget(mib(2))
	settings := DefaultClientSettings()
	settings.Log = NewNoopLogger()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	settings.SendBufferSettings.ResendQueueBudget = NewTransferMemoryBudgetWithParent(root.TotalByteCount(), root)
	settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	peer := NewId()
	client.ContractManager().AddNoContractPeer(peer)
	route := make(Route, 16)
	client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(peer)), []Route{route})
	natSettings := DefaultProviderLocalUserNatSettingsWithMemoryTarget(mib(4))
	natSettings.MemoryBudget, natSettings.Log = root, NewNoopLogger()
	nat, err := TryNewLocalUserNat(ctx, "prepared-udp-return", natSettings)
	if err != nil {
		t.Fatal(err)
	}
	providerSettings := DefaultRemoteUserNatProviderSettings()
	if len(noAckOverride) > 0 {
		providerSettings.UdpTransferNoAck = noAckOverride[0]
	}
	noAck := providerSettings.UdpTransferNoAck
	providerSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
	provider, err := TryNewRemoteUserNatProvider(client, nat, providerSettings)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		provider.Close()
		if err := nat.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		for len(route) > 0 {
			MessagePoolReturn(<-route)
		}
		assertRetainedBudgetBalance(t, root)
	}()
	port, closeEcho := startUdpLoopbackEcho(t, 4)
	defer closeEcho()
	payload := bytes.Repeat([]byte{0x7b}, 1000)
	packet := craftSecurityPacket(IpProtocolUdp, net.IPv4(192, 0, 2, 1), 42000,
		net.IPv4(127, 0, 0, 1), int(port), false, payload)
	if !nat.SendPacket(SourceId(peer), protocol.ProvideMode_Network, packet, time.Second) {
		MessagePoolReturn(packet)
		t.Fatal("outbound socket request was refused")
	}
	var wire []byte
	select {
	case wire = <-route:
	case <-time.After(5 * time.Second):
		t.Fatal("actual provider socket return did not reach Transfer")
	}
	defer MessagePoolReturn(wire)
	pack := decodeSendPackLifecycleWirePack(t, wire)
	if pack.Nack != noAck || len(pack.Frames) != 1 || pack.Frames[0].MessageType != protocol.MessageType_IpIpPacketFromProvider {
		t.Fatalf("actual socket path did not use its selected UDP policy: Nack=%t want=%t", pack.Nack, noAck)
	}
	returned, err := ipPacketFromProviderBytes(pack.Frames[0])
	if err != nil {
		t.Fatal(err)
	}
	path, returnedPayload, err := ParseIpPathWithPayload(returned)
	if err != nil || path.Protocol != IpProtocolUdp || !bytes.Equal(returnedPayload, payload) {
		t.Fatalf("socket return bytes changed: path=%+v err=%v", path, err)
	}
	done := make(chan struct{})
	go func() { provider.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("provider close waited for the detached peer ACK")
	}
	if client.ctx.Err() != nil {
		t.Fatal("provider close canceled the Client to escape ACK ownership")
	}
	if !noAck {
		if used := settings.SendBufferSettings.ResendQueueBudget.UsedByteCount(); used <= providerDatagramAckMemoryByteCount {
			t.Fatalf("provider close released accepted reliable flight: %d", used)
		}
		acknowledgeSendPackLifecycleWirePack(t, client, peer, pack)
	}
	waitNatProviderMemoryUsed(t, settings.SendBufferSettings.ResendQueueBudget, 0)
	if drops := provider.CongestionDropStats(); drops.ReturnQueuePacketCount != 0 || drops.ReturnSendPacketCount != 0 {
		t.Fatalf("actual return suffered software admission loss: %+v", drops)
	}
}
