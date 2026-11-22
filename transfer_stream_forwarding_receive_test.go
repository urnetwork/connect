// A real Client verifies a signed warm contract, then receives ordinary
// reliable and NoAck data over an enrolled P2P route with a distinct adjacency.
package connect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// The test supplies the already enrolled application association. Actual
// platform login and key exchange have separate composed coverage.
func TestStreamForwardingApplicationVerifiedClientReceipt(t *testing.T) {
	ownedTestWireSynctest(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		sourceId, destinationId, streamId, sequenceId, contractId, adjacentId := NewId(), NewId(), NewId(), NewId(), NewId(), NewId()
		settings := DefaultClientSettingsWithBufferSize(64)
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Unix(1, 0)
		settings.ContractManagerSettings.NetworkEventTimeChangeHmac = time.Unix(1, 0)
		settings.ContractManagerSettings.LegacyCreateContract = false
		settings.ReceiveBufferSettings.AckCompressTimeout = 0
		client := NewClient(ctx, destinationId, &forgedSourceRecordingOob{}, settings)
		bootstrap, received, reverse := make(Route, 16), make(Route, 16), make(Route, 32)
		defer func() {
			cancel()
			join, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			if err := client.CloseAndWait(join); err != nil {
				t.Error(err)
			}
			for _, route := range []Route{bootstrap, received, reverse} {
				for len(route) > 0 {
					MessagePoolReturn(<-route)
				}
			}
		}()
		client.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{bootstrap})
		client.RouteManager().UpdateTransport(newDataGatewayTransport(), []Route{reverse})
		client.ContractManager().SetProvideModes(map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
		secret, ok := client.ContractManager().GetProvideSecretKey(protocol.ProvideMode_Network)
		if !ok {
			t.Fatal("signed warm grant has no provider credential")
		}
		expiration := time.Now().Add(time.Hour).UnixMilli()
		stored, err := proto.Marshal(&protocol.StoredContract{ContractId: contractId.Bytes(), SourceId: sourceId.Bytes(), DestinationId: destinationId.Bytes(),
			StreamId: streamId.Bytes(), ExpirationTimeUnixMilli: &expiration, TransferByteCount: 1 << 20,
			DestinationClientPublicKey: client.ClientKeyManager().PublicKey()})
		if err != nil {
			t.Fatal(err)
		}
		proof, err := proto.Marshal(&protocol.Contract{StoredContractBytes: stored,
			StoredContractHmac: SignStoredContract(settings.ContractManagerSettings, secret, stored), ProvideMode: protocol.ProvideMode_Network})
		if err != nil {
			t.Fatal(err)
		}
		introductionId := NewId()
		grant := streamForwardingGrant{path: TransferPath{SourceId: sourceId, DestinationId: destinationId, StreamId: streamId}, sequenceId: sequenceId, contractId: contractId,
			introductionId: introductionId, contractDigest: sha256.Sum256(stored), expirationTimeUnixMillis: expiration}
		source, destination := streamForwardingApplicationPairForGrant(t, grant)
		binding := &streamForwardingBinding{readiness: destination.readiness}
		binding.association.Store(destination)
		lane := &streamForwardingReceiveLane{ctx: ctx, binding: binding, wireLimit: 65536, streamId: streamId, adjacentId: adjacentId}
		updateP2pConnectionReceiveRoutes(ctx, client.RouteManager(), []p2pReceiveRouteLane{{
			transport: NewReceiveGatewayTransportWithType(TransportTypeP2p), route: received,
			reliability: CarrierReliabilityUnreliable, forwarding: lane,
		}}, adjacentId, true)
		var delivered []string
		client.AddReceiveCallback(func(origin TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				if frame.MessageType == protocol.MessageType_TestSimpleMessage {
					if origin.SourceId != sourceId {
						t.Errorf("application received source %s instead of enrolled %s", origin.SourceId, sourceId)
					}
					delivered = append(delivered, string(frame.MessageBytes))
				}
			}
		})
		opening := &protocol.TransferFrame{TransferPath: grant.path.LocalMask().ToProtobuf(), Pack: &protocol.Pack{
			MessageId: introductionId.Bytes(), SequenceId: sequenceId.Bytes(), Head: true, ForceStream: true,
			ContractFrame: &protocol.Frame{MessageType: protocol.MessageType_TransferContract, MessageBytes: proof},
		}}
		bootstrap <- MessagePoolCopy(marshalStreamForwardingTestFrame(t, opening))
		synctest.Wait()
		buffer := client.receiveBuffer
		buffer.mutex.Lock()
		verified := false
		for _, sequence := range buffer.receiveSequences {
			if sequence.source.SourceId == sourceId && sequence.receiveContract != nil && sequence.receiveContract.contractId == contractId && sequence.receiveContract.verifiedReceiveProofSet {
				verified = true
			}
		}
		buffer.mutex.Unlock()
		if !verified || client.ContractManager().ReceiveNoContract(sourceId) {
			t.Fatal("receiver did not open the exact signed grant through ordinary contract admission")
		}
		for len(reverse) > 0 {
			MessagePoolReturn(<-reverse)
		}
		var reliableId []byte
		for i, text := range []string{"reliable", "noack"} {
			packet := streamForwardingTestPack(source, []byte(text), i != 0)
			if i == 0 {
				packet.Pack.SequenceNumber = 1
				reliableId = bytes.Clone(packet.Pack.MessageId)
			}
			raw := marshalStreamForwardingTestFrame(t, packet)
			wire, ok := source.seal(raw, 65536)
			if !ok {
				t.Fatal("legal forward client data was refused")
			}
			received <- wire
			synctest.Wait()
			if len(delivered) != i+1 || delivered[i] != text {
				t.Fatalf("legal forwarded %s delivered %d callbacks, expected %d from the enrolled source", text, len(delivered), i+1)
			}
			nativeAcks := 0
			for len(reverse) > 0 {
				ackBytes := <-reverse
				var frame protocol.TransferFrame
				err := proto.Unmarshal(ackBytes, &frame)
				if err == nil && frame.Ack != nil && bytes.Equal(frame.Ack.SequenceId, sequenceId[:]) {
					nativeAcks++
					if i == 0 && !bytes.Equal(frame.Ack.MessageId, reliableId) {
						t.Error("native ack did not cover exact reliable send")
					}
					answer, accepted := destination.seal(ackBytes, 65536)
					if !accepted {
						t.Error("matching reverse native ack was refused")
					} else {
						inner, kind, accepted := source.open(answer, 65536)
						if !accepted || kind != streamForwardingAck || !bytes.Equal(inner, ackBytes) {
							t.Error("origin failed to open exact native ack")
						}
						MessagePoolReturn(answer)
					}
				}
				MessagePoolReturn(ackBytes)
			}
			if (i == 0 && nativeAcks == 0) || (i == 1 && nativeAcks != 0) {
				t.Fatalf("%s emitted %d native acks", text, nativeAcks)
			}
		}
		if drops := client.ReceiveStats().ForgedSourceDropCount; drops != 0 {
			t.Fatalf("ordinary authenticated forward traffic incurred %d source drops", drops)
		}
	})
}
