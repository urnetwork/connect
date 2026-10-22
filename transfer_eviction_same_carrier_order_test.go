// Private same-carrier boundary control. Requires SAME-CARRIER-SEAM.patch
// applied equally to OLD and candidate private afterimages; never canonical.
package connect

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// A wire-valid combined record is assembled from the genuine victim SACK and
// genuine withdrawal. This is a defensive accepted-format control, not a claim
// that this fixture's receiver emitted both fields in this same physical ACK.
func TestEvictionFeedbackSameCarrierCannotPublishWithdrawalAheadOfSack(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		publication := newEvictionFeedbackGate()
		f := newEvictionFeedbackFixture(t, true, func(settings *SendBufferSettings) {
			settings.afterEvictionMetadataForTest = func(sendSequenceId) {
				publication.park()
			}
		})
		// Register after the fixture: neither Client join may wait on this gate.
		t.Cleanup(publication.release)
		combined := proto.Clone(f.sack.ack).(*protocol.Ack)
		combined.EvictedSequenceNumbers = append([]uint64(nil), f.notice.ack.EvictedSequenceNumbers...)
		path := sendTransferPath(f.wire.receiver.ClientId(), DestinationId(f.wire.sender.ClientId()))
		wire, err := ProtoMarshal(&protocol.TransferFrame{
			TransferPath: path.ToProtobuf(),
			Ack:          combined,
		})
		if err != nil {
			t.Fatal(err)
		}
		carrier := &windowRoundFrame{bytes: wire, ack: combined}
		f.wire.heldFrames = append(f.wire.heldFrames, carrier)
		at := time.Now()
		publication.armed.Store(true)
		f.wire.forward(carrier, f.wire.senderIn)
		publication.requireEntered(t)
		if f.pendingCount() != 1 {
			t.Fatal("actual ACK ingress did not publish the combined withdrawal at its barrier")
		}
		// OLD has not published the carrier SACK yet. Candidate publishes it
		// before this same seam. Let the actual owner consume the available cut.
		f.afterInitial.release()
		synctest.Wait()
		prompt := f.wire.sender.SendRecoveryStats().SendEvictionResendCount == 1 &&
			len(f.wire.senderOut) == 1 && f.pendingCount() == 0
		if !prompt {
			t.Error("same-carrier withdrawal became extractable before its SACK; exact owner was not repaired")
		}
		if time.Now() != at || f.wire.ackedCount != 1 {
			t.Fatal("same-carrier boundary advanced a timer or invented terminal credit")
		}
		publication.release()
		synctest.Wait()
		f.finishRepairAndReconcile(t, "combined carrier", prompt)
	})
}
