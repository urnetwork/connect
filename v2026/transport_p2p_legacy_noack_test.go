package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

type legacyNoAckCaptureWriter struct {
	windowPacingPolicyWriter
	wire []byte
}

type legacyNoAckFloodConn struct {
	*p2pProbePressureConn
	t             *testing.T
	start         <-chan struct{}
	done          chan struct{}
	bulkCount     int
	priorityCount int
	probeCount    int
	burst         int
	budget        *TransferMemoryBudget
}

func (self *legacyNoAckFloodConn) Write(wire []byte) (int, error) {
	<-self.start
	time.Sleep(time.Millisecond)
	priority := isP2pStreamProbe(wire) || messagePoolIsSmallUnordered(wire)
	if priority {
		self.burst++
		if self.bulkCount < 96 && self.burst > 2 {
			self.t.Errorf("%d priority messages bypassed ready bulk", self.burst)
		}
		if isP2pStreamProbe(wire) {
			self.probeCount++
		} else {
			self.priorityCount++
		}
	} else {
		if identity := binary.BigEndian.Uint32(wire); identity != uint32(self.bulkCount) {
			self.t.Errorf("bulk[%d]=%d", self.bulkCount, identity)
		}
		self.bulkCount++
		self.burst = 0
	}
	if self.budget.UsedByteCount() > self.budget.TotalByteCount() {
		self.t.Error("priority service exceeded shared memory budget")
	}
	if self.bulkCount == 96 && self.priorityCount == 192 && self.probeCount == 2 {
		close(self.done)
	}
	return len(wire), nil
}

// The ordinary route carries sustained datagram and explicit ACK floods while
// both endpoint queues are ready. All classes make progress, at most two priority
// writes bypass each ready data packet, and the reliable data stays in order.
func TestP2pLegacyNoAckFloodPreservesBulkAndEndpointFairness(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ack := legacyPriorityTestAck(t, 2, true)
		defer MessagePoolReturn(ack)
		start := make(chan struct{})
		budget := NewTransferMemoryBudget(kib(256))
		conn := &legacyNoAckFloodConn{
			p2pProbePressureConn: &p2pProbePressureConn{ctx: ctx},
			t:                    t, start: start, done: make(chan struct{}), budget: budget,
		}
		settings := DefaultP2pTransportSettings()
		settings.DataPlaneMode = P2pDataPlaneModeLegacyOnly
		settings.ChannelBufferSize = 4
		streamId := NewId()
		transport, route := newP2pSendTransportForPeer(ctx, cancel, &p2pLegacyQueueBudgetTestConn{Conn: conn, budget: budget}, NewId(), streamId, settings, true, nil)
		sender := transport.(*P2pSendTransport)
		defer sender.CloseAndWait(context.Background())
		for index := range 96 {
			route <- legacyQueueTestPacket(uint32(index), 1200)
		}
		synctest.Wait()
		admitted, acksAdmitted := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(admitted)
			for range 128 {
				wire := MessagePoolGet(230)
				messagePoolMarkSmallUnordered(wire)
				route <- wire
			}
		}()
		go func() {
			defer close(acksAdmitted)
			for range 64 {
				route <- MessagePoolShareReadOnly(ack)
			}
		}()
		sender.probeRequests <- encodeP2pStreamProbe(p2pStreamProbeRequestType, streamId, NewId())
		sender.probeResponses <- encodeP2pStreamProbe(p2pStreamProbeResponseType, streamId, NewId())
		synctest.Wait()
		close(start)
		<-conn.done
		<-admitted
		<-acksAdmitted
		if err := sender.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if budget.UsedByteCount() != 0 {
			t.Fatal("priority flood retained queue memory after join")
		}
	})
}

func TestP2pLegacyNoAckCancellationJoinsBorrowedWrite(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		budget := NewTransferMemoryBudget(kib(64))
		entered, release := make(chan struct{}), make(chan struct{})
		q := newP2pLegacySendQueue(ctx, cancel, func(wire []byte, _ time.Time) error {
			close(entered)
			<-release
			if !messagePoolIsSmallUnordered(wire) {
				t.Error("borrowed no-ack root returned early")
			}
			return ctx.Err()
		}, kib(16), budget)
		done := make(chan error, 1)
		go func() {
			wire := MessagePoolGet(230)
			messagePoolMarkSmallUnordered(wire)
			done <- q.enqueuePriority(wire, time.Time{})
		}()
		<-entered
		cancel()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("priority owner returned before borrowed physical write joined")
		default:
		}
		if budget.UsedByteCount() != q.ownerCharge {
			t.Fatal("priority owner released before join")
		}
		close(release)
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("result=%v", err)
		}
		q.stopAndWait()
		if budget.UsedByteCount() != 0 {
			t.Fatal("canceled no-ack retained queue memory")
		}
	})
}

func (self *legacyNoAckCaptureWriter) WriteDetailedWithTransport(_ context.Context, wire []byte, _ time.Duration) (bool, TransportType, error) {
	self.wire = wire
	return true, TransportTypeP2p, nil
}

// Exercise the real final send boundary: the scheduling flag must follow the
// encrypted outer root, not the private plaintext or a test-only annotation.
func legacyNoAckTestWire(t *testing.T, encrypted, ack bool, payloadSize int) []byte {
	t.Helper()
	sequence := newEstimatorFixture(t, nil)
	sequence.ctx, sequence.client, sequence.log = context.Background(), &Client{}, NewNoopLogger()
	writer := &legacyNoAckCaptureWriter{}
	sequence.contractMultiRouteWriter = writer
	if encrypted {
		sequence.session = &peerEncryptionSession{
			client: sequence.client, role: sequenceTlsRoleClient,
			establishedEpoch: &tlsHandshakeEpoch{peerIdentityVerified: true, derivedTlsCipher: newFrameCodecTestSequenceCipher(t)},
		}
	}
	frame := sendPackFrame{
		messageId: NewId(), sequenceId: NewId(), nack: !ack,
		frames: []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketFromProvider, MessageBytes: bytes.Repeat([]byte{0x5a}, payloadSize), Raw: true}},
	}
	inner := marshalSendPackTransferFrame(&frame)
	defer MessagePoolReturn(inner)
	item := &sendItem{transferFrameBytes: inner, expectsAck: ack}
	if _, err := sequence.writeMaybeWrappedBytes(inner, TransferPath{}, false, item, false, false); err != nil {
		t.Fatal(err)
	}
	if encrypted {
		var outer protocol.TransferFrame
		if err := ProtoUnmarshal(writer.wire, &outer); err != nil || len(outer.EncryptedTransferFrame) == 0 || outer.Pack != nil {
			t.Fatal("test did not exercise the encrypted outer frame")
		}
	}
	return writer.wire
}

func TestP2pLegacySmallNoAckClassificationSurvivesEncryption(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, encrypted := range []bool{false, true} {
		for _, ack := range []bool{false, true} {
			for _, size := range []int{60, 300} {
				t.Run(fmt.Sprintf("encrypted=%t/ack=%t/payload=%d", encrypted, ack, size), func(t *testing.T) {
					wire := legacyNoAckTestWire(t, encrypted, ack, size)
					defer MessagePoolReturn(wire)
					if got, want := messagePoolIsSmallUnordered(wire), !ack && len(wire) <= smallPacketPoolSize; got != want {
						t.Fatalf("wire length=%d priority=%t want=%t", len(wire), got, want)
					}
				})
			}
		}
	}
}

func TestP2pLegacySmallNoAckImmediateSenderClassification(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{1, 2} {
		for _, encrypted := range []bool{false, true} {
			for _, size := range []int{60, 300} {
				t.Run(fmt.Sprintf("version=%d/encrypted=%t/payload=%d", version, encrypted, size), func(t *testing.T) {
					sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) { settings.ProtocolVersion = version })
					sequence.ctx, sequence.client, sequence.log = context.Background(), &Client{}, NewNoopLogger()
					writer := &legacyNoAckCaptureWriter{}
					if encrypted {
						sequence.session = &peerEncryptionSession{
							client: sequence.client, role: sequenceTlsRoleClient,
							establishedEpoch: &tlsHandshakeEpoch{peerIdentityVerified: true, derivedTlsCipher: newFrameCodecTestSequenceCipher(t)},
						}
					}
					pack := &SendPack{Frame: &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, MessageBytes: MessagePoolGet(size), Raw: true}}
					if !sequence.writeNoAckFastPath(&noAckFastPathSnapshot{writer: writer}, pack) {
						MessagePoolReturn(pack.Frame.MessageBytes)
						t.Fatal("immediate sender refused its available route")
					}
					defer MessagePoolReturn(writer.wire)
					if got, want := messagePoolIsSmallUnordered(writer.wire), len(writer.wire) <= smallPacketPoolSize; got != want {
						t.Fatalf("wire length=%d priority=%t want=%t", len(writer.wire), got, want)
					}
					if encrypted {
						var outer protocol.TransferFrame
						if err := ProtoUnmarshal(writer.wire, &outer); err != nil || len(outer.EncryptedTransferFrame) == 0 || outer.Pack != nil {
							t.Fatal("immediate sender did not use encrypted outer root")
						}
					}
				})
			}
		}
	}
}

func TestP2pLegacySmallNoAckPoolSharesAndReuse(t *testing.T) {
	assertMessagePoolOwnership(t)
	pool := orderedMessagePools()[0]
	root := pool.take(120, 0)
	if allocations := testing.AllocsPerRun(100, func() {
		messagePoolMarkSmallUnordered(root)
		if !messagePoolIsSmallUnordered(root) {
			t.Error("owned root lost its scheduling hint")
		}
	}); allocations != 0 {
		t.Errorf("scheduling hint allocated %g objects", allocations)
	}
	messagePoolMarkSmallUnordered(root)
	shared := MessagePoolShareReadOnly(root)
	if !messagePoolIsSmallUnordered(root) || !messagePoolIsSmallUnordered(shared) {
		t.Fatal("classification lost through a read-only share")
	}
	MessagePoolReturn(shared)
	if !messagePoolIsSmallUnordered(root) {
		t.Fatal("nonfinal return cleared a still-owned hint")
	}
	// Retain the identity only to prove the exact returned root is reused. No
	// stale owner reads its contents after return.
	identity := &root[0]
	MessagePoolReturn(root)
	var held [][]byte
	defer func() {
		for _, wire := range held {
			MessagePoolReturn(wire)
		}
	}()
	found := false
	for range 2 * (pool.snapshot().retained + messagePoolShardCount) {
		wire := pool.take(120, 0)
		held = append(held, wire)
		if messagePoolIsSmallUnordered(wire) {
			t.Fatal("returned no-ack hint contaminated a new owner")
		}
		if &wire[0] == identity {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("test did not reuse its marked root")
	}
	if messagePoolIsSmallUnordered(make([]byte, 120)) {
		t.Fatal("unowned bytes acquired scheduling priority")
	}
}

type legacyNoAckPacingConn struct {
	*p2pLegacyProbePacingConn
	want []byte
	seen chan p2pLegacyProbeObservation
}

func (self *legacyNoAckPacingConn) Write(wire []byte) (int, error) {
	if bytes.Equal(wire, self.want) {
		<-self.start
		time.Sleep(time.Duration(len(wire)) * 8 * time.Second / 64_000)
		self.seen <- p2pLegacyProbeObservation{at: time.Now(), dataCount: len(self.dataWritten)}
		return len(wire), nil
	}
	return self.p2pLegacyProbePacingConn.Write(wire)
}

// These are ordinary small datagram Packs, with no latency-probe payload or
// special endpoint-control marker. An encrypted no-ack Pack must avoid the
// compact 96-packet bulk barrier; ordered and large traffic must not overtake.
func TestP2pLegacyNoAckBacklogPreservesBoundedService(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, test := range []struct {
		name      string
		encrypted bool
		ack       bool
		size      int
	}{
		{name: "no-ack", size: 60},
		{name: "encrypted-no-ack", encrypted: true, size: 60},
		{name: "reliable", ack: true, size: 60},
		{name: "encrypted-reliable", encrypted: true, ack: true, size: 60},
		{name: "large-no-ack", size: 300},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				wire := legacyNoAckTestWire(t, test.encrypted, test.ack, test.size)
				wantPriority := !test.ack && len(wire) <= smallPacketPoolSize
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
				t.Logf("latency=%s prior bulk=%d/%d", got.at.Sub(offered), got.dataCount, conn.bulkCount)
				if wantPriority && (got.at.Sub(offered) > 332*time.Millisecond || got.dataCount >= conn.bulkCount) {
					t.Errorf("small no-ack Pack waited %s behind %d bulk packets", got.at.Sub(offered), got.dataCount)
				}
				if !wantPriority && got.dataCount != conn.bulkCount {
					t.Errorf("ordinary Pack bypassed reliable FIFO after %d packets", got.dataCount)
				}
				<-conn.bulkDone
			})
		})
	}
}
