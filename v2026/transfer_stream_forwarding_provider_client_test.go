// A provider return uses a distinct signed Network contract on the same stream.
// Its TCP SYN+ACK and native delivery receipt must cross the sole P2P route.
package connect

import (
	"bytes"
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

// Install the already classified StreamOpen and original enrollment. The
// ordinary native receive and ACK producers establish the reciprocal scope.
func testStreamForwardingProviderEndpoint(t *testing.T, client *Client, enrollment streamForwardingEnrollment, adjacentId Id) *StreamSequence {
	t.Helper()
	sequence := testStreamForwardingBindingSequence(t, enrollment, adjacentId)
	sequence.streamManager = client.streamManager
	buffer := client.streamManager.streamBuffer
	buffer.mutex.Lock()
	if buffer.streamSequencesByStreamId == nil {
		buffer.streamSequencesByStreamId = map[Id]*StreamSequence{}
	}
	buffer.streamSequencesByStreamId[sequence.streamId] = sequence
	buffer.mutex.Unlock()
	t.Cleanup(func() {
		sequence.Cancel()
		buffer.mutex.Lock()
		delete(buffer.streamSequencesByStreamId, sequence.streamId)
		buffer.mutex.Unlock()
	})
	if !sequence.bindStreamForwarding(enrollment) {
		t.Fatal("original stream scope did not bind to the test endpoint")
	}
	return sequence
}

func TestStreamForwardingProviderTcpSynAckAndDeliveryReceiptWithoutPlatform(t *testing.T) {
	ownedTestWireSynctest(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		deviceId, providerId, streamId, adjacentId := NewId(), NewId(), NewId(), NewId()
		settings := DefaultClientSettingsWithBufferSize(64)
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Unix(1, 0)
		settings.ContractManagerSettings.NetworkEventTimeChangeHmac = time.Unix(1, 0)
		settings.ContractManagerSettings.LegacyCreateContract = false
		settings.ReceiveBufferSettings.AckCompressTimeout = 0
		device := NewClient(ctx, deviceId, &forgedSourceRecordingOob{}, settings)
		h1Receive, h1Ack, p2pReceive, p2pAck := make(Route, 16), make(Route, 32), make(Route, 16), make(Route, 32)
		defer func() {
			cancel()
			join, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			if err := device.CloseAndWait(join); err != nil {
				t.Error(err)
			}
			for _, route := range []Route{h1Receive, h1Ack, p2pReceive, p2pAck} {
				for len(route) > 0 {
					MessagePoolReturn(<-route)
				}
			}
		}()

		primary := testStreamForwardingBindingEnrollment(t, TransferPath{SourceId: deviceId, DestinationId: providerId, StreamId: streamId})
		deviceEndpoint := testStreamForwardingProviderEndpoint(t, device, primary, adjacentId)
		provider := testStreamForwardingWireClient(ctx, providerId)
		primary.destination = true
		providerEndpoint := testStreamForwardingProviderEndpoint(t, provider, primary, NewId())
		deviceOwner := deviceEndpoint.forwarding.association.Load()
		providerOwner := providerEndpoint.forwarding.association.Load()

		router := device.RouteManager()
		platformReceive := NewReceiveGatewayTransportWithType(TransportTypeH1)
		platformSend := newTypedPriorityRouteTestTransport(TransportTypeH1, 1)
		devicePlatform := newPlatformReceiveOwner(deviceId, ctx.Done(), nil, true)
		providerPlatform := newPlatformReceiveOwner(providerId, ctx.Done(), nil, true)
		router.UpdateTransportWithProperties(platformReceive, []Route{h1Receive}, TransferCarrierProperties{
			ReceiveReliability: CarrierReliabilityReliable, platformReceive: devicePlatform.provenance(platformReceiveH1),
		})
		router.UpdateTransport(platformSend, []Route{h1Ack})
		device.ContractManager().SetProvideModes(map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
		secret, ok := device.ContractManager().GetProvideSecretKey(protocol.ProvideMode_Network)
		if !ok {
			t.Fatal("receiver has no Network contract verification key")
		}
		deadline := time.Now().Add(time.Hour).UnixMilli()
		contractId := NewId()
		stored, err := proto.Marshal(&protocol.StoredContract{
			ContractId: contractId.Bytes(), SourceId: providerId.Bytes(), DestinationId: deviceId.Bytes(),
			StreamId: streamId.Bytes(), ExpirationTimeUnixMilli: &deadline, TransferByteCount: 1 << 20,
			DestinationClientPublicKey: device.ClientKeyManager().PublicKey(),
		})
		if err != nil {
			t.Fatal(err)
		}
		proof := &protocol.Contract{StoredContractBytes: stored, StoredContractHmac: SignStoredContract(settings.ContractManagerSettings, secret, stored), ProvideMode: protocol.ProvideMode_Network}
		contract, err := newSequenceContract(settings.Log, "provider-return", proof, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		role := protocol.SequenceRole_SequenceRoleServer
		sequenceId, introductionId := NewId(), NewId()
		sender := &SendSequence{ctx: ctx, client: provider, destination: deviceId, sequenceId: sequenceId, forceStream: true, networkPeer: true, encryptionRole: sequenceTlsRoleServer}
		offer := sender.streamForwardingIntroduction(contract, introductionId, 0, time.Now())
		if offer == nil {
			t.Fatal("signed provider return did not offer its native sequence")
		}
		proof.StreamForwardingOffer = offer.wire[:]
		contractBytes, err := proto.Marshal(proof)
		if err != nil {
			t.Fatal(err)
		}
		opening := &protocol.TransferFrame{TransferPath: contract.path.LocalMask().ToProtobuf(), SessionRole: &role, Pack: &protocol.Pack{
			MessageId: introductionId.Bytes(), SequenceId: sequenceId.Bytes(), Head: true, ForceStream: true,
			ContractFrame: &protocol.Frame{MessageType: protocol.MessageType_TransferContract, MessageBytes: contractBytes},
		}}
		h1Receive <- MessagePoolCopy(marshalStreamForwardingTestFrame(t, opening))
		synctest.Wait()
		for len(h1Ack) > 0 {
			wire := <-h1Ack
			var frame protocol.TransferFrame
			err := proto.Unmarshal(wire, &frame)
			path, pathErr := FilteredTransferPath(wire)
			MessagePoolReturn(wire)
			ack := frame.Ack
			if err != nil || pathErr != nil || ack == nil || ack.Selective ||
				!bytes.Equal(ack.MessageId, introductionId[:]) || !bytes.Equal(ack.SequenceId, sequenceId[:]) {
				continue
			}
			ingress := streamForwardingIngress(path, &receivePackCarrier{provenance: providerPlatform.provenance(platformReceiveH1)}, ack)
			sender.acceptStreamForwardingAck(receiveAckMessage{messageId: introductionId, sequenceId: sequenceId}, 0, true, ingress)
		}
		if !offer.published || device.ContractManager().ReceiveNoContract(providerId) {
			t.Fatal("actual signed introduction and native platform ACK did not commit the return sequence")
		}
		if deviceEndpoint.forwarding.association.Load() != deviceOwner || providerEndpoint.forwarding.association.Load() != providerOwner {
			t.Fatal("return enrollment replaced the original physical stream owner")
		}
		devicePlatform.retire()
		providerPlatform.retire()
		router.RemoveTransport(platformReceive)
		router.RemoveTransport(platformSend)
		removeAlias := router.AddWriterDestinationAlias(DestinationId(providerId), StreamId(streamId))
		defer removeAlias()
		p2pSettings := DefaultP2pTransportSettings()
		p2pSettings.DataPlaneMode = P2pDataPlaneModeLegacyOnly
		reply := &P2pSendTransport{ctx: ctx, settings: p2pSettings, transportId: NewId(), streamId: streamId, peerId: adjacentId,
			endpointDestinationRequired: true, forwarding: deviceEndpoint.forwarding, forwardingPhysicalLimit: 65536}
		router.UpdateTransportWithProperties(reply, []Route{p2pAck}, p2pTransferCarrierProperties(reply))
		lane := &streamForwardingReceiveLane{ctx: ctx, binding: deviceEndpoint.forwarding, wireLimit: 65536, streamId: streamId, adjacentId: adjacentId}
		updateP2pConnectionReceiveRoutes(ctx, router, []p2pReceiveRouteLane{{
			transport: NewReceiveGatewayTransportWithType(TransportTypeP2p), route: p2pReceive,
			reliability: CarrierReliabilityUnreliable, forwarding: lane,
		}}, adjacentId, true)

		packet := reraceSynAck(reraceFlow(4, 1, 31416))
		deliveries := 0
		device.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, peer Peer) {
			for _, frame := range frames {
				if frame.MessageType != protocol.MessageType_IpIpPacketFromProvider {
					continue
				}
				deliveries++
				actual, err := ipPacketFromProviderBytes(frame)
				if err != nil || !bytes.Equal(actual, packet) || source.SourceId != providerId || peer.ProvideMode != protocol.ProvideMode_Network ||
					peer.TransportType != TransportTypeP2p || !peer.TransferKey.ForceStream || peer.TransferKey.EncryptionRole != protocol.SequenceRole_SequenceRoleClient {
					t.Error("device callback lost the provider identity, native lane, physical route, or exact TCP SYN+ACK")
				}
			}
		})
		messageId := NewId()
		data := &protocol.TransferFrame{TransferPath: contract.path.LocalMask().ToProtobuf(), SessionRole: &role, Pack: &protocol.Pack{
			MessageId: messageId.Bytes(), SequenceId: sequenceId.Bytes(), SequenceNumber: 1, ForceStream: true,
			Frames: []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true, MessageBytes: packet}},
		}}
		raw := marshalStreamForwardingTestFrame(t, data)
		write := streamForwardingWrite{inner: raw, contractId: &contractId}
		if !write.matches(providerEndpoint.forwarding) {
			t.Fatal("charged return was refused by its sole forwarded writer")
		}
		wire, ok := providerOwner.seal(raw, 65536)
		if !ok {
			t.Fatal("provider could not protect the native return")
		}
		p2pReceive <- wire
		synctest.Wait()
		if deliveries != 1 || device.ReceiveStats().ForgedSourceDropCount != 0 || len(h1Ack) != 0 || len(p2pAck) != 1 {
			t.Fatalf("return deliveries=%d forged=%d platform=%d native acknowledgements=%d", deliveries, device.ReceiveStats().ForgedSourceDropCount, len(h1Ack), len(p2pAck))
		}
		replyBytes := <-p2pAck
		defer MessagePoolReturn(replyBytes)
		response, ok := deviceOwner.seal(replyBytes, 65536)
		if !ok {
			t.Fatal("device could not protect its delivery acknowledgement")
		}
		defer MessagePoolReturn(response)
		inner, class, ok := providerOwner.open(response, 65536)
		var decoded protocol.TransferFrame
		if !ok || class != streamForwardingAck || proto.Unmarshal(inner, &decoded) != nil || decoded.Ack == nil || decoded.Ack.Selective ||
			!bytes.Equal(decoded.Ack.MessageId, messageId[:]) || !bytes.Equal(decoded.Ack.SequenceId, sequenceId[:]) {
			t.Fatal("provider did not receive the exact native delivery acknowledgement")
		}
	})
}
