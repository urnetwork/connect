package connect

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Capture the actual receive worker's final ACK root. The outer encryption
// deliberately hides the message type from the downstream physical writer.
func legacyPriorityTestAck(t *testing.T, version int, encrypted bool) []byte {
	t.Helper()
	opened, release := make(chan struct{}), make(chan struct{})
	fixture := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
		settings.ProtocolVersion = version
		settings.AckCompressTimeout = 10 * time.Millisecond
		settings.afterAckWriterOpenForTest = func(receiveSequenceId, MultiRouteWriter) {
			close(opened)
			<-release
		}
	})
	<-opened
	var cipher *sequenceCipher
	if encrypted {
		cipher = newFrameCodecTestSequenceCipher(t)
		fixture.receiveSequence.session = &peerEncryptionSession{
			client: fixture.receiveSequence.client, role: sequenceTlsRoleServer,
			establishedEpoch: &tlsHandshakeEpoch{peerIdentityVerified: true, derivedTlsCipher: cipher},
		}
		t.Cleanup(func() { fixture.receiveSequence.session = nil })
	}
	messageID := NewId()
	fixture.receiveSequence.sendAck(73, messageID, true, sequenceTag{}, false, TransportTypeP2p)
	close(release)
	var wire []byte
	select {
	case wire = <-fixture.route:
	case <-time.After(time.Second):
		t.Fatal("real receive worker did not publish its ACK")
	}
	var outer protocol.TransferFrame
	if err := ProtoUnmarshal(wire, &outer); err != nil {
		MessagePoolReturn(wire)
		t.Fatal(err)
	}
	if encrypted {
		if len(outer.EncryptedTransferFrame) == 0 || outer.Ack != nil {
			t.Fatal("ACK test did not exercise the encrypted outer root")
		}
		plain, err := cipher.Open(outer.EncryptedTransferFrame)
		if err != nil {
			t.Fatal(err)
		}
		defer MessagePoolReturn(plain)
		outer.Reset()
		if err := ProtoUnmarshal(plain, &outer); err != nil {
			t.Fatal(err)
		}
	}
	ack := outer.Ack
	if version == 1 {
		ack = &protocol.Ack{}
		if outer.Frame == nil || outer.Frame.MessageType != protocol.MessageType_TransferAck {
			t.Fatal("legacy carrier is not an ACK")
		}
		if err := ProtoUnmarshal(outer.Frame.MessageBytes, ack); err != nil {
			t.Fatal(err)
		}
	}
	if ack == nil || RequireIdFromBytes(ack.MessageId) != messageID || !ack.Selective {
		t.Fatal("ACK identity or selective feedback changed")
	}
	return wire
}

// The recorded failure admitted a selective ACK to the route, then never
// reached the physical writer while bulk was drained. Another ACK timed out
// behind that barrier, and the opposite reliable sequence expired. Even
// without network loss, this 96-packet queue adds 14.4 s to the feedback path.
// Explicit ACKs may overtake the opposite-direction Packs they acknowledge;
// all reliable Pack identities must retain their original FIFO order.
func TestP2pLegacyAckBacklogPreservesBoundedService(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{1, 2} {
		for _, encrypted := range []bool{false, true} {
			t.Run(fmt.Sprintf("version=%d/encrypted=%t", version, encrypted), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					wire := legacyPriorityTestAck(t, version, encrypted)
					start := make(chan struct{})
					conn := &legacyNoAckPacingConn{
						p2pLegacyProbePacingConn: &p2pLegacyProbePacingConn{
							p2pProbePressureConn: &p2pProbePressureConn{ctx: ctx},
							start:                start, bulkDone: make(chan struct{}), bulkCount: 96,
						},
						want: bytes.Clone(wire), seen: make(chan p2pLegacyProbeObservation, 1),
					}
					settings := DefaultP2pTransportSettings()
					settings.DataPlaneMode = P2pDataPlaneModeLegacyOnly
					settings.ChannelBufferSize = 4
					transport, route := newP2pSendTransportForPeer(ctx, cancel, conn, NewId(), NewId(), settings, false, nil)
					defer transport.(P2pRouteLifecycle).CloseAndWait(context.Background())
					for index := range conn.bulkCount {
						route <- legacyQueueTestPacket(uint32(index), 1200)
					}
					synctest.Wait()
					offered := time.Now()
					route <- wire
					synctest.Wait()
					close(start)
					got := <-conn.seen
					t.Logf("ACK latency=%s prior bulk=%d/%d", got.at.Sub(offered), got.dataCount, conn.bulkCount)
					if got.at.Sub(offered) > 332*time.Millisecond || got.dataCount >= conn.bulkCount {
						t.Errorf("explicit ACK waited %s behind %d bulk packets", got.at.Sub(offered), got.dataCount)
					}
					<-conn.bulkDone
					for index, identity := range conn.dataWritten {
						if identity != uint32(index) {
							t.Fatalf("ACK scheduling changed reliable FIFO: [%d]=%d", index, identity)
						}
					}
				})
			})
		}
	}
}
