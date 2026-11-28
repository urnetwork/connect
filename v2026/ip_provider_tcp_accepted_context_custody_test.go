// An admission-only cancellation link must not revoke an accepted reliable
// return. Both route writes and the peer's cumulative acknowledgement are real.
package connect

import (
	"context"
	"net"
	"sync"
	"testing"
	"testing/synctest"
)

// Holds native SendSequence.Run before serialization, then the actual peer's
// acknowledgement wire before sender delivery. No Pack is removed by a test
// consumer, no acknowledgement is injected, and no retry owner is synthesized.
func TestProviderTcpAdmittedReturnSurvivesLinkCloseBeforeNativeRun(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		peerId := NewId()
		events := make(chan SendPackLifecycleObservation, 16)
		nativeEntered := make(chan struct{})
		nativeRelease := make(chan struct{})
		ackRelease := make(chan struct{})
		var nativeOnce, nativeReleaseOnce, ackReleaseOnce sync.Once
		releaseNative := func() { nativeReleaseOnce.Do(func() { close(nativeRelease) }) }
		releaseAck := func() { ackReleaseOnce.Do(func() { close(ackRelease) }) }
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.SendBufferSettings.LogicalDataLaneCount = 1
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
			if id.Destination == peerId {
				nativeOnce.Do(func() { close(nativeEntered) })
				<-nativeRelease
			}
		}
		settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
			if event.DestinationId == peerId {
				events <- event
			}
		}
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		peerSettings := DefaultClientSettings()
		peerSettings.Log = NewNoopLogger()
		peerSettings.EncryptionSettings.Mode = EncryptionModeOff
		peerSettings.ReceiveBufferSettings.AckCompressTimeout = 0
		peerSettings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		peer := NewClient(ctx, peerId, NewNoContractClientOob(), peerSettings)
		client.ContractManager().AddNoContractPeer(peerId)
		peer.ContractManager().AddNoContractPeer(client.ClientId())
		dataWire := make(chan []byte, 8)
		ackWire := make(chan []byte, 8)
		ackIngress := make(chan []byte, 8)
		client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(peerId)), []Route{dataWire})
		peer.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{dataWire})
		peer.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(client.ClientId())), []Route{ackWire})
		client.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{ackIngress})
		bridgeDone := make(chan struct{})
		go func() {
			defer close(bridgeDone)
			select {
			case <-ctx.Done():
				return
			case <-ackRelease:
			}
			for {
				select {
				case <-ctx.Done():
					return
				case packet := <-ackWire:
					select {
					case ackIngress <- packet:
					case <-ctx.Done():
						MessagePoolReturn(packet)
						return
					}
				}
			}
		}()
		fixture := &tcpProviderRetirementFixture{
			t: t, client: client, source: SourceId(peerId),
			path:    &IpPath{Version: 4, Protocol: IpProtocolTcp, SourceIp: net.IPv4(192, 0, 2, 3).To4(), SourcePort: 40001, DestinationIp: net.IPv4(198, 51, 100, 7).To4(), DestinationPort: 443},
			created: make(chan *TcpSequence, 1), oldAckPackets: make(chan []byte, 1),
		}
		socket, upstream := net.Pipe()
		natSettings := DefaultLocalUserNatSettingsWithBufferSize(8)
		natSettings.Log = NewNoopLogger()
		natSettings.TcpBufferSettings.AckCompressTimeout = 0
		natSettings.TcpBufferSettings.WriteBatchSize = 1
		natSettings.TcpBufferSettings.DialContextSettings = &DialContextSettings{
			DialContext: func(context.Context, string, string) (net.Conn, error) { return socket, nil },
		}
		natSettings.TcpBufferSettings.beforeSequenceRunWithStateForTest = func(sequence *TcpSequence) {
			sequence.afterPureAckBuildForTest = func(packet []byte) {
				fixture.oldAckPackets <- MessagePoolShareReadOnly(packet)
			}
			fixture.created <- sequence
		}
		fixture.nat = NewLocalUserNat(ctx, "synthetic accepted TCP custody", natSettings)
		providerSettings := DefaultRemoteUserNatProviderSettings()
		providerSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
		providerSettings.WriteTimeout = -1
		providerSettings.ReturnSendAbandonTimeout = 0
		fixture.provider = NewRemoteUserNatProvider(client, fixture.nat, providerSettings)
		t.Cleanup(func() {
			releaseNative()
			releaseAck()
			fixture.provider.Close()
			cancel()
			socket.Close()
			upstream.Close()
			if err := fixture.nat.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			if err := peer.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			<-bridgeDone
			for _, queue := range []chan []byte{dataWire, ackWire, ackIngress} {
			drain:
				for {
					select {
					case packet := <-queue:
						MessagePoolReturn(packet)
					default:
						break drain
					}
				}
			}
			if fixture.oldAckWitness != nil {
				MessagePoolReturn(fixture.oldAckWitness)
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

		fixture.send(tcpFlagSyn, 1000, 0, nil)
		old := fixture.nextSequence(1000)
		<-nativeEntered
		fixture.upload(old, upstream, 1001, "x")
		fixture.oldAckWitness = <-fixture.oldAckPackets
		old.Cancel()
		synctest.Wait()
		select {
		case <-old.retirementDone:
		default:
			t.Fatal("flow did not retire after its accepted callbacks/link cleanup")
		}
		client.sendBuffer.mutex.Lock()
		var sequence *SendSequence
		for id, candidate := range client.sendBuffer.sendSequences {
			if id.Destination == peerId {
				if sequence != nil {
					t.Error("return controls unexpectedly created two native sequences")
				}
				sequence = candidate
			}
		}
		client.sendBuffer.mutex.Unlock()
		if sequence == nil || len(sequence.packs) != 2 || fixture.provider.ctx.Err() != nil || client.Ctx().Err() != nil {
			t.Fatal("native barrier did not retain exactly the accepted syn-ack/pure-ack with shared owners live")
		}
		started := map[uint64]bool{}
		for range 2 {
			event := <-events
			if event.Phase != SendPackLifecyclePhaseStarted || !event.AckRequired || event.Err != nil {
				t.Fatalf("pre-run lifecycle=%+v, want accepted reliable start", event)
			}
			started[event.Token] = true
		}
		if len(started) != 2 {
			t.Fatal("accepted return controls shared a lifecycle token")
		}
		releaseNative()
		synctest.Wait()
		written := map[uint64]bool{}
		for range 2 {
			event := <-events
			if event.Phase != SendPackLifecyclePhaseFirstRouteWrite || event.Err != nil || !started[event.Token] {
				t.Fatalf("canceled admission link revoked accepted wire work: %+v", event)
			}
			written[event.Token] = true
		}
		count, _ := sequence.resendQueue.QueueSize()
		if len(written) != 2 || count == 0 || len(ackWire) == 0 {
			t.Fatal("accepted returns did not retain real resend ownership before the real peer ACK")
		}
		select {
		case event := <-events:
			t.Fatalf("return terminalized before its held peer acknowledgement: %+v", event)
		default:
		}
		releaseAck()
		synctest.Wait()
		completed := map[uint64]bool{}
		for range 2 {
			event := <-events
			if event.Phase != SendPackLifecyclePhaseTerminal || event.Err != nil || !written[event.Token] {
				t.Fatalf("actual peer acknowledgement did not finish accepted work: %+v", event)
			}
			completed[event.Token] = true
		}
		synctest.Wait()
		count, _ = sequence.resendQueue.QueueSize()
		if len(completed) != 2 || count != 0 {
			t.Fatal("peer acknowledgement retained or duplicated accepted reliable custody")
		}
		if !MessagePoolReturn(fixture.oldAckWitness) {
			t.Fatal("acknowledged accepted return retained its original pooled pure-ack share")
		}
		fixture.oldAckWitness = nil
		if fixture.provider.ctx.Err() != nil || client.Ctx().Err() != nil || peer.Ctx().Err() != nil {
			t.Fatal("successful peer delivery relied on shared-owner cancellation")
		}
	})
}
