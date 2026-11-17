// Actual advertised4096 and unknown-H1 paths retain wire, callback and owner bounds.
package connect

import (
	"bytes"
	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
	"testing"
	"testing/synctest"
	"time"
)

// Real compact contracts remain identity-signed and Network-HMAC verified.
// The existing encryption-off fixture already uses this nil-chain shape for
// its first-small grant; no forged contract or receiver bypass is introduced.
func TestH1Legacy4096ActualSplitRotationAndOwnership(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{1, 2} {
		for _, known := range []bool{true, false} {
			synctest.Test(t, func(t *testing.T) {
				fixture := privateH1NewFixture(t, version, mib(1), !known, TransportTypeH1, nil)
				for _, client := range []*Client{fixture.sender, fixture.receiver} {
					authority, ok := client.ClientOob().(*privateH1ProofAuthority)
					if !ok || authority.grants.Load() != 0 {
						t.Fatal("ordinary fixture already published peer proof")
					}
					authority.chain = nil
				}
				if !known {
					fixture.startPlatformControl(true)
				}
				if known {
					fixture.sender.RouteManager().UpdateTransportWithProperties(fixture.transport, []Route{fixture.senderOut}, TransferCarrierProperties{MaxMessageByteCount: 4096})
					if ok, err := fixture.send(32); !ok || err != nil {
						t.Fatalf("warm admission=%t/%v", ok, err)
					}
					warm := fixture.data()
					fixture.forward(warm, fixture.receiverIn)
					fixture.acknowledge()
					if !fixture.sequence().sendContractAcked {
						t.Fatal("real warm ACK did not establish contract")
					}
				} else {
					fixture.sender.RouteManager().UpdateTransport(fixture.transport, []Route{fixture.senderOut})
				}
				count := 7
				if !known {
					count = 16
				}
				frames := fixture.frames(count, version)
				results := make(chan error, 2)
				ok, err := fixture.sender.sendGroupWithTimeoutDetailed(frames, fixture.receiver.ClientId(), func(err error) { results <- err }, time.Second)
				if !ok || err != nil {
					for _, frame := range frames {
						MessagePoolReturn(frame.MessageBytes)
					}
					t.Fatalf("ordinary logical admission=%t/%v", ok, err)
				}
				delivered := 0
				chunks := 0
				firstContract := Id{}
				// Observe the real signed opening proof before the maintained control drain
				// forwards zero-frame heads; the first application Pack need not repeat it.
				nextData := func() *windowRoundFrame {
					for range cap(fixture.senderOut) {
						wire := fixture.take(fixture.senderOut)
						if wire.pack == nil {
							t.Fatal("sender emitted a non-Pack")
						}
						if !known && firstContract == (Id{}) && wire.pack.ContractFrame != nil {
							contract := &protocol.Contract{}
							if err := proto.Unmarshal(wire.pack.ContractFrame.MessageBytes, contract); err != nil {
								t.Fatal(err)
							}
							stored := &protocol.StoredContract{}
							if err := proto.Unmarshal(contract.StoredContractBytes, stored); err != nil {
								t.Fatal(err)
							}
							firstContract = RequireIdFromBytes(stored.ContractId)
						}
						if len(wire.pack.Frames) > 0 {
							return wire
						}
						fixture.forward(wire, fixture.receiverIn)
						fixture.acknowledge()
					}
					t.Fatal("application Pack hidden behind a full control route")
					return nil
				}
				for delivered < count {
					wire := nextData()
					if !known && firstContract == (Id{}) {
						t.Fatal("ordinary rotation has no observed signed opening proof")
					}
					path := sendTransferPath(fixture.sender.ClientId(), DestinationId(fixture.receiver.ClientId()))
					if size := privateH1SealedByteCount(t, path, wire.bytes); size > 4096 {
						t.Fatalf("H1_4096_ENCODED_OVERFLOW: v%d known=%t actualencrypted=%d", version, known, size)
					}
					expected := min(2, count-delivered)
					if len(wire.pack.Frames) != expected {
						t.Fatalf("H1_4096_PARTITION_WRONG: v%d known=%t chunk%d frames%d want%d", version, known, chunks, len(wire.pack.Frames), expected)
					}
					if len(results) != 0 {
						t.Fatal("logical callback completed before final physical ACK")
					}
					fixture.forward(wire, fixture.receiverIn)
					fixture.acknowledge()
					delivered += expected
					chunks++
					if delivered < count && len(results) != 0 {
						t.Fatal("partial ACK completed original group")
					}
				}
				select {
				case err := <-results:
					if err != nil {
						t.Fatal(err)
					}
				default:
					t.Fatal("final actual ACK did not complete original group")
				}
				if !known {
					fixture.requirePlatformClose(fixture.sender.ClientId(), firstContract)
					fixture.requirePlatformControlsSettled()
				}
				if len(results) != 0 || fixture.budget.UsedByteCount() != 0 || fixture.sequence().resendQueue.Len() != 0 {
					sequence := fixture.sequence()
					items, encoded := sequence.resendQueue.QueueSize()
					stats := fixture.budget.Stats()
					t.Fatalf("H1_4096_FINAL_CUSTODY: version=%d known=%t callbacks=%d used=%d reserved=%d released=%d original_items=%d original_encoded=%d send_items=%d prepaid=%d sender_out=%d", version, known, len(results), stats.UsedByteCount, stats.ReservedByteCount, stats.ReleasedByteCount, items, encoded, len(sequence.sendItems), sequence.resendMemoryGrant, len(fixture.senderOut))
				}
				offset := 0
				if known {
					offset = 1
				}
				if len(fixture.delivered) != count+offset {
					t.Fatal("ordinary group lost or duplicated delivered frames")
				}
				for index, witness := range fixture.witnesses {
					if !bytes.Equal(fixture.delivered[index+offset], witness) {
						t.Fatal("ordinary split changed payload order or ownership")
					}
				}
				authority := fixture.sender.ClientOob().(*privateH1ProofAuthority)
				if !known && authority.grants.Load() < 2 {
					t.Fatal("unknown-H1 control did not cross an actual contract rotation")
				}
				contract := fixture.sequence().sendContract.contract
				if size := proto.Size(contract); size <= 0 || size > 512 {
					t.Fatalf("ordinary signed proof no longer bounded: %d", size)
				}
				t.Logf("[h1-4096-actual] version=%d known=%t chunks=%d frames=%d grants=%d signedproof=%d", version, known, chunks, count, authority.grants.Load(), proto.Size(contract))
			})
		}
	}
}
