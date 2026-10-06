package connect

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// A provider UDP shard calls the same immediate NoAck writer used by a
// reliable sequence. Holding that writer's wait lock must not hold the shard:
// a congested destination is refused and the next destination still sends.
func TestProviderUdpShardProgressesPastBusyReliableWriter(t *testing.T) {
	settings := DefaultRemoteUserNatProviderSettings()
	settings.WriteTimeout = time.Hour
	settings.ReturnSendWorkerCount = 1
	settings.ReturnSendQueueSize = 1
	provider, client, _ := newProviderTransferKeyTestFixtureWithSettings(t, settings)
	completed := make(chan providerReturnSendResult, 2)
	provider.afterReturnSendForTest = func(result providerReturnSendResult) {
		completed <- result
	}

	install := func(peerId Id, route Route) *MultiRouteSelector {
		sequence := installProviderReturnTestSequence(t, provider, client, sendSequenceId{
			Destination:       peerId,
			CompanionContract: true,
			ForceStream:       true,
			EncryptionRole:    sequenceTlsRoleServer,
		})
		sequence.client = client
		sequence.destination = peerId
		sequence.sequenceId = NewId()
		sequence.forceStream = true
		sequence.companionContract = true
		sequence.encryptionRole = sequenceTlsRoleServer
		sequence.sendBufferSettings = DefaultSendBufferSettings()
		// Nothing can enter the paused fallback queue, just as when a
		// reliable sender consumes its bounded admission capacity.
		sequence.packs = make(chan *SendPack)
		selector := NewMultiRouteSelector(provider.ctx, "provider-udp", nil, DestinationId(peerId), true)
		selector.updateTransport(NewSendClientTransport(DestinationId(peerId)), []Route{route})
		// Publish an established no-contract lane, avoiding unrelated
		// handshake timing while exercising the real provider-to-Pack path.
		sequence.noAckFastPath.Store(&noAckFastPathSnapshot{writer: selector})
		t.Cleanup(selector.Close)
		return selector
	}
	blockedPeer, healthyPeer := NewId(), NewId()
	blockedRoute := make(Route)
	healthyRoute := make(Route, 1)
	blockedWriter := install(blockedPeer, blockedRoute)
	install(healthyPeer, healthyRoute)
	blockedWriter.writeMutex.Lock()
	var unlock sync.Once
	defer unlock.Do(blockedWriter.writeMutex.Unlock)

	packet := MessagePoolCopy(craftSecurityPacket(
		IpProtocolUdp,
		net.ParseIP("203.0.113.7"), 8080,
		net.ParseIP("10.0.0.9"), 42001,
		false, []byte("provider shard progress"),
	))
	defer MessagePoolReturn(packet)
	ipPath, err := ParseIpPath(packet)
	if err != nil {
		t.Fatal(err)
	}
	transferKey := TransferKey{
		ForceStream:    true,
		EncryptionRole: protocol.SequenceRole_SequenceRoleServer,
	}
	provider.receiveTransfer(SourceId(blockedPeer), transferKey, protocol.ProvideMode_Public, ipPath, packet)
	guard := time.NewTimer(time.Second)
	defer guard.Stop()
	select {
	case result := <-completed:
		if result.sent || result.packetCount != 1 || result.packetByteCount != ByteCount(len(packet)) {
			t.Fatalf("congested return disposition=%+v", result)
		}
	case <-guard.C:
		// Release and join on a pre-fix failure so test cleanup cannot
		// retain a blocked provider worker or its borrowed packet.
		unlock.Do(blockedWriter.writeMutex.Unlock)
		waitProviderReturnSendCompletion(t, completed)
		t.Fatal("zero-wait NoAck write parked the provider UDP shard on a reliable writer")
	}

	provider.receiveTransfer(SourceId(healthyPeer), transferKey, protocol.ProvideMode_Public, ipPath, packet)
	waitProviderReturnSendResult(t, completed, true, 1, ByteCount(len(packet)))
	select {
	case wire := <-healthyRoute:
		pack := decodeCompactContractTestPack(t, wire)
		MessagePoolReturn(wire)
		if !pack.GetNack() || len(pack.Frames) != 1 {
			t.Fatalf("healthy UDP return was not one NoAck packet: %+v", pack)
		}
	default:
		t.Fatal("healthy destination did not receive a packet while the other writer remained blocked")
	}
	if drops := provider.CongestionDropStats(); drops.ReturnQueuePacketCount != 0 || drops.ReturnSendPacketCount != 1 {
		t.Fatalf("bounded per-destination refusal caused shared queue drops: %+v", drops)
	}
	if blockedWriter.writeTimer != nil {
		t.Fatal("zero-wait provider return created a writer timer")
	}
}
