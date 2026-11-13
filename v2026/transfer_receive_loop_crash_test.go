package connect

// Regression tests for panics reachable from peer input on the client receive
// loop. `Client.run` runs under HandleError with the client's cancel, so any
// such panic stops the whole client for every peer, not just the sender.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026/protocol"
)

// A client whose receive loop reads the frames written to the returned route
// and whose receive callback reports the content of each simple message
// delivered from peerId. Closing the client and returning undelivered route
// bytes are registered as test cleanup.
func newReceiveLoopTestClient(
	t *testing.T,
	settings *ClientSettings,
	peerId Id,
) (*Client, Route, chan string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	client.ContractManager().AddNoContractPeer(peerId)
	inbound := make(Route, 8)
	client.RouteManager().UpdateTransport(
		NewReceiveGatewayTransportWithType(TransportTypeH1),
		[]Route{inbound},
	)
	delivered := make(chan string, 8)
	client.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, peer Peer) {
		if source.SourceId != peerId {
			return
		}
		for _, frame := range frames {
			message, err := FromFrame(frame)
			if err != nil {
				continue
			}
			if simpleMessage, ok := message.(*protocol.SimpleMessage); ok {
				select {
				case delivered <- simpleMessage.Content:
				default:
				}
			}
		}
	})
	t.Cleanup(func() {
		cancel()
		join, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := client.CloseAndWait(join); err != nil {
			t.Error(err)
		}
		for len(inbound) != 0 {
			MessagePoolReturn(<-inbound)
		}
	})
	return client, inbound, delivered
}

// One head pack opening the sequence, carrying a simple message with the given
// content on the lane the force-stream flag selects.
func newReceiveLoopTestPack(t *testing.T, sequenceId Id, forceStream bool, content string) *protocol.Pack {
	t.Helper()
	messageBytes, err := proto.Marshal(&protocol.SimpleMessage{Content: content})
	if err != nil {
		t.Fatal(err)
	}
	return &protocol.Pack{
		MessageId:      NewId().Bytes(),
		SequenceId:     sequenceId.Bytes(),
		SequenceNumber: 0,
		Head:           true,
		Frames: []*protocol.Frame{{
			MessageType:  protocol.MessageType_TestSimpleMessage,
			MessageBytes: messageBytes,
		}},
		ForceStream: forceStream,
	}
}

// Waits for the next delivered content, failing if the client stops first.
func awaitReceiveLoopDelivery(t *testing.T, client *Client, delivered chan string, stopped string) string {
	t.Helper()
	select {
	case content := <-delivered:
		return content
	case <-client.Done():
		t.Fatal(stopped)
	case <-time.After(10 * time.Second):
		t.Fatal("no delivery after the injected frame")
	}
	return ""
}

// A peer that holds the session key chooses everything inside the seal. A
// sealed inner frame without a transfer path is an inner/outer path mismatch
// to drop and audit; dereferencing the missing path on the receive loop
// cancelled the whole client.
func TestSealedFrameWithoutInnerPathDoesNotStopTheClient(t *testing.T) {
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOpportunistic
	peerId := NewId()
	client, inbound, delivered := newReceiveLoopTestClient(t, settings, peerId)

	// an established, identity-verified server-role session with a known key,
	// so the test can seal what the peer would
	key := make([]byte, sequenceTlsKeyLength)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	sessionCipher := &sequenceCipher{aead: aead}
	session := client.EncryptionSessionManager().Acquire(peerId, sequenceTlsRoleServer, false)
	if session == nil {
		t.Fatal("no encryption session for the peer")
	}
	t.Cleanup(session.Release)
	epochCtx, epochCancel := context.WithCancel(context.Background())
	t.Cleanup(epochCancel)
	epoch := &tlsHandshakeEpoch{
		ctx:                  epochCtx,
		cancel:               epochCancel,
		handshakeDone:        make(chan struct{}),
		establishmentDone:    make(chan struct{}),
		derivedTlsCipher:     sessionCipher,
		peerIdentityVerified: true,
	}
	close(epoch.handshakeDone)
	close(epoch.establishmentDone)
	func() {
		session.stateLock.Lock()
		defer session.stateLock.Unlock()
		session.epoch = epoch
		session.establishedEpoch = epoch
	}()

	path := TransferPath{SourceId: peerId, DestinationId: client.ClientId()}
	seal := func(transferFrame *protocol.TransferFrame) []byte {
		innerBytes, err := proto.Marshal(transferFrame)
		if err != nil {
			t.Fatal(err)
		}
		sealed, err := sessionCipher.SealOuterFrame(
			path,
			innerBytes,
			protocol.SequenceRole_SequenceRoleClient,
			false,
		)
		if err != nil {
			t.Fatal(err)
		}
		return sealed
	}

	inbound <- seal(&protocol.TransferFrame{
		Pack: newReceiveLoopTestPack(t, NewId(), false, "without inner path"),
	})
	inbound <- seal(&protocol.TransferFrame{
		TransferPath: path.ToProtobuf(),
		Pack:         newReceiveLoopTestPack(t, NewId(), false, "after"),
	})

	content := awaitReceiveLoopDelivery(
		t,
		client,
		delivered,
		"a sealed frame without an inner transfer path stopped the client",
	)
	AssertEqual(t, "after", content)
	AssertEqual(t, false, client.IsDone())
}

// The receive buffer finds a sequence by id but tracks heads by lane, and the
// force-stream and companion-contract flags belong to the lane only. A pack
// that reuses a live sequence's id under another lane while that sequence is
// closing must be dropped: asserting that the pack's lane head is that id
// panicked on the receive loop and cancelled the whole client.
func TestPackOfAnotherLaneForAClosingSequenceDoesNotStopTheClient(t *testing.T) {
	heldSequenceId := NewId()
	held := make(chan receiveSequenceId, 1)
	release := make(chan struct{})
	settings := DefaultClientSettings()
	// keep the first sequence's worker from running, so the sequence stays
	// indexed while it closes
	settings.ReceiveBufferSettings.beforeRunReceiveSequenceForTest = func(id receiveSequenceId) {
		if id.SequenceId != heldSequenceId {
			return
		}
		held <- id
		<-release
	}
	peerId := NewId()
	client, inbound, delivered := newReceiveLoopTestClient(t, settings, peerId)
	// registered after the client's cleanup, so it runs first
	t.Cleanup(func() {
		close(release)
	})

	path := TransferPath{SourceId: peerId, DestinationId: client.ClientId()}
	send := func(pack *protocol.Pack) {
		transferFrameBytes, err := proto.Marshal(&protocol.TransferFrame{
			TransferPath: path.ToProtobuf(),
			Pack:         pack,
		})
		if err != nil {
			t.Fatal(err)
		}
		inbound <- transferFrameBytes
	}

	send(newReceiveLoopTestPack(t, heldSequenceId, false, "opens"))
	var heldId receiveSequenceId
	select {
	case heldId = <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("the held sequence was not created")
	}
	var heldSequence *ReceiveSequence
	func() {
		client.receiveBuffer.mutex.Lock()
		defer client.receiveBuffer.mutex.Unlock()
		heldSequence = client.receiveBuffer.receiveSequences[heldId]
	}()
	if heldSequence == nil {
		t.Fatal("the held sequence is not indexed")
	}
	heldSequence.Cancel()

	send(newReceiveLoopTestPack(t, heldSequenceId, true, "other lane"))
	// a new sequence on the force-stream lane, which has no head, so the loop
	// never waits on the held sequence's exit to deliver it
	send(newReceiveLoopTestPack(t, NewId(), true, "after"))

	content := awaitReceiveLoopDelivery(
		t,
		client,
		delivered,
		"a pack of another lane for a closing sequence stopped the client",
	)
	AssertEqual(t, "after", content)
	AssertEqual(t, false, client.IsDone())
}

// Once a pack's own id has no live sequence, only a stale index can still name
// that id as the head of the pack's lane. Heal the index and open the
// sequence, as the stale path for an older head already does, instead of
// panicking on the receive loop.
func TestReceiveBufferHealsAStaleHeadIndexForThePacksOwnId(t *testing.T) {
	peerId := NewId()
	client, _, delivered := newReceiveLoopTestClient(t, DefaultClientSettings(), peerId)

	sequenceId := NewId()
	source := TransferPath{SourceId: peerId}
	func() {
		client.receiveBuffer.mutex.Lock()
		defer client.receiveBuffer.mutex.Unlock()
		staleHeadKey := receiveSequenceHeadKey{
			Source:         source,
			EncryptionRole: sequenceTlsRoleServer,
		}
		client.receiveBuffer.headReceiveSequenceIds[staleHeadKey] = receiveSequenceId{
			Source:         source,
			SequenceId:     sequenceId,
			EncryptionRole: sequenceTlsRoleServer,
		}
	}()

	pack := newReceiveLoopTestPack(t, sequenceId, false, "healed")
	success, err := client.receiveBuffer.Pack(&ReceivePack{
		Source:           source,
		SequenceId:       sequenceId,
		Pack:             pack,
		ReceiveCallback:  client.receiveCallback,
		MessageByteCount: MessageByteCount(pack.Frames),
		Unwrapped:        true,
		EncryptionRole:   sequenceTlsRoleServer,
	}, 5*time.Second)
	AssertEqual(t, nil, err)
	AssertEqual(t, true, success)

	content := awaitReceiveLoopDelivery(
		t,
		client,
		delivered,
		"the healed sequence stopped the client",
	)
	AssertEqual(t, "healed", content)
}
