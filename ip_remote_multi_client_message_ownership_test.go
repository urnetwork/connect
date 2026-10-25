// Refused control marshals keep no owner; refused raw inputs keep the caller's.
// These adapter regressions do not attribute the separate default-v2 DNS leak.
package connect

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// Fully close the actual transport owner before sampling a refusal. This
// excludes in-flight client work from the exact before/after pool census.
func closedMessageOwnershipChannel(t *testing.T) *multiClientChannel {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), closeWaitClientSettings())
	t.Cleanup(func() {
		if err := client.CloseAndWait(ctx); err != nil {
			t.Errorf("join message ownership client: %v", err)
		}
	})
	if err := client.CloseAndWait(ctx); err != nil {
		t.Fatal("close actual message ownership client: ", err)
	}
	return &multiClientChannel{client: client, settings: DefaultMultiClientSettings(),
		args: &multiClientChannelArgs{Destination: RequireMultiHopId(NewId())}}
}

// One real closed-client call is the causal OLD failure. Legacy ping and a
// current non-Raw control both marshal fresh owners; default-v2 ping does not.
func TestMultiClientMessageRefusalReturnsEncodedOwner(t *testing.T) {
	channel := closedMessageOwnershipChannel(t)
	for _, version := range []int{0, 1, 2} {
		channel.settings.ProtocolVersion = version
		var message proto.Message = &protocol.IpPing{}
		if version == 2 {
			message = &protocol.ControlPing{}
		}
		beforeCount, beforeBytes := messagePoolOutstandingSnapshot()
		var callbacks atomic.Int32
		accepted, err := channel.SendDetailedMessage(message, 0, func(error) { callbacks.Add(1) })
		if accepted || err == nil || callbacks.Load() != 0 {
			t.Fatalf("closed-client refusal premise: version=%d accepted=%t error=%v callbacks=%d",
				version, accepted, err, callbacks.Load())
		}
		afterCount, afterBytes := messagePoolOutstandingSnapshot()
		if afterCount != beforeCount || afterBytes != beforeBytes {
			t.Fatalf("OLD_CHANNEL_ENCODED_REFUSAL_OWNER: version=%d before_roots=%d after_roots=%d before_bytes=%d after_bytes=%d accepted=false callbacks=0",
				version, beforeCount, afterCount, beforeBytes, afterBytes)
		}
	}
}

// v0/1 encode the caller's packet into a new frame; v2 borrows it directly.
// Neither refused path may consume that original packet ownership.
func TestMultiClientMessageRefusalPreservesRawInput(t *testing.T) {
	assertMessagePoolOwnership(t)
	channel := closedMessageOwnershipChannel(t)
	for _, version := range []int{0, 1, 2} {
		for _, fromProvider := range []bool{false, true} {
			channel.settings.ProtocolVersion = version
			func() {
				packet := MessagePoolCopy([]byte("synthetic raw input"))
				witness := MessagePoolShareReadOnly(packet)
				defer func() {
					if witness != nil {
						MessagePoolReturn(witness)
					}
					if packet != nil {
						MessagePoolReturn(packet)
					}
				}()
				var message proto.Message = &protocol.IpPacketToProvider{IpPacket: &protocol.IpPacket{PacketBytes: packet}}
				if fromProvider {
					message = &protocol.IpPacketFromProvider{IpPacket: &protocol.IpPacket{PacketBytes: packet}}
				}
				beforeCount, beforeBytes := messagePoolOutstandingSnapshot()
				accepted, err := channel.SendDetailedMessage(message, 0, nil)
				if accepted || err == nil {
					t.Fatalf("raw refusal premise: version=%d from_provider=%t accepted=%t error=%v", version, fromProvider, accepted, err)
				}
				// With exactly one witness, its final-return result detects an
				// incorrect adapter Return without provoking a double Return.
				returned := MessagePoolReturn(witness)
				witness = nil
				if returned {
					packet = nil
					t.Fatalf("refused raw input consumed caller ownership: version=%d from_provider=%t", version, fromProvider)
				}
				afterCount, afterBytes := messagePoolOutstandingSnapshot()
				if afterCount != beforeCount || afterBytes != beforeBytes || !bytes.Equal(packet, []byte("synthetic raw input")) {
					t.Fatalf("refused input or marshaled owner changed: version=%d from_provider=%t roots=%d/%d bytes=%d/%d",
						version, fromProvider, beforeCount, afterCount, beforeBytes, afterBytes)
				}
				MessagePoolReturn(packet)
				packet = nil
			}()
		}
	}
}

// Pin the correction to the initial source hypothesis: default-v2 IpPing
// produces a Raw frame with nil bytes and adds no payload root on refusal.
func TestMultiClientMessageV2PingRefusalHasNoPayloadOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	channel := closedMessageOwnershipChannel(t)
	if channel.settings.ProtocolVersion != 2 {
		t.Fatal("current protocol default changed")
	}
	frame, err := ToFrame(&protocol.IpPing{}, channel.settings.ProtocolVersion)
	if err != nil || !frame.Raw || frame.MessageBytes != nil {
		t.Fatal("default ping is not the source-pinned Raw/nil path")
	}
	beforeCount, beforeBytes := messagePoolOutstandingSnapshot()
	accepted, err := channel.SendDetailedMessage(&protocol.IpPing{}, 0, nil)
	afterCount, afterBytes := messagePoolOutstandingSnapshot()
	if accepted || err == nil || afterCount != beforeCount || afterBytes != beforeBytes {
		t.Fatalf("default ping refusal changed payload ownership: accepted=%t error=%v roots=%d/%d bytes=%d/%d",
			accepted, err, beforeCount, afterCount, beforeBytes, afterBytes)
	}
}

// The existing native credit fixture supplies an actually retained original
// and a closed source admission gate. Exact peer ACK release permits a retry.
func TestMultiClientMessageUnfundedRefusalAndRelease(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		for _, version := range []int{0, 1, 2} {
			credit := newNativeBatchCreditFixture(t)
			channel := credit.base.selected
			channel.settings.ProtocolVersion = version
			message := &protocol.SimpleMessage{Content: "synthetic ready control"}
			var callbacks atomic.Int32
			beforeCount, beforeBytes := messagePoolOutstandingSnapshot()
			accepted, err := channel.SendDetailedMessage(message, 0, func(error) { callbacks.Add(1) }, protocol.ProvideMode_Network)
			synctest.Wait()
			if accepted || err != nil || callbacks.Load() != 0 || !credit.source.resendCapacityUnavailable.Load() || len(credit.base.route) != 0 {
				t.Fatalf("live closed-credit refusal premise: version=%d accepted=%t error=%v callbacks=%d gate_closed=%t wire=%d",
					version, accepted, err, callbacks.Load(), credit.source.resendCapacityUnavailable.Load(), len(credit.base.route))
			}
			afterCount, afterBytes := messagePoolOutstandingSnapshot()
			if afterCount != beforeCount || afterBytes != beforeBytes {
				t.Fatalf("live false/nil refusal retained an encoded owner: version=%d roots=%d/%d bytes=%d/%d",
					version, beforeCount, afterCount, beforeBytes, afterBytes)
			}
			credit.releasePeerCredit()
			ackResult := make(chan error, 1)
			accepted, err = channel.SendDetailedMessage(message, time.Second, func(err error) { ackResult <- err }, protocol.ProvideMode_Network)
			if !accepted || err != nil {
				t.Fatalf("actual credit release did not admit control: version=%d accepted=%t error=%v", version, accepted, err)
			}
			synctest.Wait()
			if len(credit.base.route) != 1 {
				t.Fatal("accepted control did not produce one actual wire frame")
			}
			wire := <-credit.base.route
			pack := decodeSendPackLifecycleWirePack(t, wire)
			valid := !pack.Nack && len(pack.Frames) == 1 && pack.Frames[0].MessageType == protocol.MessageType_TestSimpleMessage
			if valid {
				var decoded protocol.SimpleMessage
				valid = ProtoUnmarshal(pack.Frames[0].MessageBytes, &decoded) == nil && decoded.Content == message.Content
			}
			if !valid {
				MessagePoolReturn(wire)
				t.Fatal("accepted control changed its reliable wire payload")
			}
			acknowledged := credit.base.client.sendBuffer.Ack(credit.base.peer,
				&protocol.Ack{MessageId: pack.MessageId, SequenceId: pack.SequenceId}, 0)
			MessagePoolReturn(wire)
			if !acknowledged {
				t.Fatal("exact accepted control ACK was refused")
			}
			synctest.Wait()
			select {
			case err := <-ackResult:
				if err != nil {
					t.Fatal("accepted control completion: ", err)
				}
			default:
				t.Fatal("exact ACK did not complete the accepted control")
			}
			if len(ackResult) != 0 {
				t.Fatal("accepted control completion repeated")
			}
		}
	})
}

// Each protocol uses its normal real transfer encoding, actual receive and
// peer acknowledgement. The pool assertion covers exact-once owner release.
func TestMultiClientMessageAcceptedProtocolsReturnExactlyOnce(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		for _, version := range []int{0, 1, 2} {
			fixture := newWindowRoundFixture(t, func(settings *SendBufferSettings) { settings.ProtocolVersion = version }, nil)
			settings := DefaultMultiClientSettings()
			settings.ProtocolVersion = version
			channel := &multiClientChannel{client: fixture.sender, settings: settings,
				args: &multiClientChannelArgs{Destination: RequireMultiHopId(fixture.receiver.ClientId())}}
			var completions atomic.Int32
			message := &protocol.SimpleMessage{Content: "synthetic accepted control"}
			accepted, err := channel.SendDetailedMessage(message, time.Second, func(err error) {
				if err != nil {
					t.Errorf("accepted protocol %d completion: %v", version, err)
				}
				completions.Add(1)
			})
			if !accepted || err != nil {
				t.Fatalf("accepted protocol %d refused: %t %v", version, accepted, err)
			}
			wire := fixture.takePack(0)
			fixture.forward(wire, fixture.receiverIn)
			fixture.acknowledge()
			synctest.Wait()
			if fixture.deliveredCount != 1 || completions.Load() != 1 {
				t.Fatalf("protocol %d delivered=%d completed=%d", version, fixture.deliveredCount, completions.Load())
			}
		}
	})
}

// A successful v2 raw frame transfers its caller's pooled root. The sole
// witness must be the final owner after actual receive and ACK completion.
func TestMultiClientMessageAcceptedRawTransfersExactlyOnce(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		for _, fromProvider := range []bool{false, true} {
			fixture := newWindowRoundFixture(t, nil, nil)
			channel := &multiClientChannel{client: fixture.sender, settings: DefaultMultiClientSettings(),
				args: &multiClientChannelArgs{Destination: RequireMultiHopId(fixture.receiver.ClientId())}}
			func() {
				packet := MessagePoolCopy([]byte("synthetic accepted raw"))
				witness := MessagePoolShareReadOnly(packet)
				defer func() {
					if witness != nil {
						MessagePoolReturn(witness)
					}
					if packet != nil {
						MessagePoolReturn(packet)
					}
				}()
				var message proto.Message = &protocol.IpPacketToProvider{IpPacket: &protocol.IpPacket{PacketBytes: packet}}
				if fromProvider {
					message = &protocol.IpPacketFromProvider{IpPacket: &protocol.IpPacket{PacketBytes: packet}}
				}
				var completions atomic.Int32
				accepted, err := channel.SendDetailedMessage(message, time.Second, func(err error) {
					if err != nil {
						t.Errorf("accepted raw completion: %v", err)
					}
					completions.Add(1)
				})
				if !accepted || err != nil {
					t.Fatalf("raw success premise: %t %v", accepted, err)
				}
				packet = nil // true/nil transferred this caller's owner
				wire := fixture.takePack(0)
				fixture.forward(wire, fixture.receiverIn)
				fixture.acknowledge()
				synctest.Wait()
				if fixture.deliveredCount != 1 || completions.Load() != 1 {
					t.Fatal("raw accepted input lacked one actual delivery and completion")
				}
				returned := MessagePoolReturn(witness)
				witness = nil
				if !returned {
					t.Fatal("accepted raw input retained its transferred original owner")
				}
			}()
		}
	})
}
