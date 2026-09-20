//go:build !js

package connect

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/pion/sctp"
	"github.com/urnetwork/connect/protocol"
)

// This lossless packet pipe deliberately excludes ICE/DTLS and link-rate
// serialization. It preserves SCTP packet boundaries and applies a constant
// one-way delay, isolating the pinned SCTP's ACK/congestion-window behavior.
// Its test-only I-bit arm requests an immediate SACK on every DATA/I-DATA
// chunk. No public Pion setter exists for this policy; this is an optimistic
// discriminator, not a proposed wire interceptor or production feature.
type udpSctpAckExperimentPacket struct {
	bytes []byte
	at    time.Time
}

type udpSctpAckExperimentConn struct {
	ctx       context.Context
	cancel    context.CancelFunc
	incoming  chan udpSctpAckExperimentPacket
	peer      *udpSctpAckExperimentConn
	delay     atomic.Int64
	immediate bool
	lock      sync.Mutex
	measuring bool
	firstAck  time.Time
	dataTsns  map[uint32]bool
	dataCount int
	repeated  int
	sackCount int
}

func udpSctpAckExperimentChunks(packet []byte, visit func(byte, []byte)) error {
	if len(packet) < 12 {
		return io.ErrUnexpectedEOF
	}
	for offset := 12; offset < len(packet); {
		if len(packet)-offset < 4 {
			return io.ErrUnexpectedEOF
		}
		length := int(binary.BigEndian.Uint16(packet[offset+2:]))
		if length < 4 || len(packet)-offset < length {
			return io.ErrUnexpectedEOF
		}
		visit(packet[offset], packet[offset:offset+length])
		offset += (length + 3) &^ 3
	}
	return nil
}

func (c *udpSctpAckExperimentConn) Write(packet []byte) (int, error) {
	owned := append([]byte(nil), packet...)
	c.lock.Lock()
	err := udpSctpAckExperimentChunks(owned, func(kind byte, chunk []byte) {
		if kind == 0 || kind == 64 { // DATA and I-DATA
			if c.immediate {
				chunk[1] |= 0x08 // RFC immediate-SACK request, checksum below.
			}
			if c.measuring && len(chunk) >= 8 {
				tsn := binary.BigEndian.Uint32(chunk[4:8])
				c.dataCount++
				if c.dataTsns[tsn] {
					c.repeated++
				}
				c.dataTsns[tsn] = true
			}
		} else if kind == 3 && c.measuring {
			c.sackCount++
		}
	})
	c.lock.Unlock()
	if err != nil {
		return 0, err
	}
	if c.immediate {
		clear(owned[8:12])
		binary.LittleEndian.PutUint32(owned[8:12], crc32.Checksum(owned, crc32.MakeTable(crc32.Castagnoli)))
	}
	select {
	case <-c.ctx.Done():
		return 0, net.ErrClosed
	case c.peer.incoming <- udpSctpAckExperimentPacket{owned, time.Now().Add(time.Duration(c.delay.Load()))}:
		return len(packet), nil
	default:
		return 0, fmt.Errorf("test wire overflow: this is not carrier backpressure")
	}
}

func (c *udpSctpAckExperimentConn) Read(packet []byte) (int, error) {
	select {
	case <-c.ctx.Done():
		return 0, net.ErrClosed
	case incoming := <-c.incoming:
		if delay := time.Until(incoming.at); delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-c.ctx.Done():
				return 0, net.ErrClosed
			case <-timer.C:
			}
		}
		if len(packet) < len(incoming.bytes) {
			return 0, io.ErrShortBuffer
		}
		c.lock.Lock()
		_ = udpSctpAckExperimentChunks(incoming.bytes, func(kind byte, _ []byte) {
			if kind == 3 && c.measuring && c.firstAck.IsZero() {
				c.firstAck = time.Now()
			}
		})
		c.lock.Unlock()
		return copy(packet, incoming.bytes), nil
	}
}

func (c *udpSctpAckExperimentConn) Close() error                   { c.cancel(); return nil }
func (*udpSctpAckExperimentConn) LocalAddr() net.Addr              { return &net.IPAddr{} }
func (*udpSctpAckExperimentConn) RemoteAddr() net.Addr             { return &net.IPAddr{} }
func (*udpSctpAckExperimentConn) SetDeadline(time.Time) error      { return nil }
func (*udpSctpAckExperimentConn) SetReadDeadline(time.Time) error  { return nil }
func (*udpSctpAckExperimentConn) SetWriteDeadline(time.Time) error { return nil }

// A detached SCTP stream has the same message-oriented Read/Write/deadline
// surface that the production peerConn presents to P2pSendTransport.
type udpSctpAckExperimentStream struct {
	*sctp.Stream
	writes      atomic.Int64
	wireBytes   atomic.Int64
	sampleRoots func()
	budget      *TransferMemoryBudget
}

func (s *udpSctpAckExperimentStream) legacySendMemoryBudget() *TransferMemoryBudget { return s.budget }

func (s *udpSctpAckExperimentStream) Write(packet []byte) (int, error) {
	if s.sampleRoots != nil {
		s.sampleRoots()
	}
	s.writes.Add(1)
	s.wireBytes.Add(int64(len(packet)))
	return s.Stream.Write(packet)
}

func (*udpSctpAckExperimentStream) LocalAddr() net.Addr  { return &net.IPAddr{} }
func (*udpSctpAckExperimentStream) RemoteAddr() net.Addr { return &net.IPAddr{} }

type udpSctpAckExperimentResult struct {
	admitted, refused, dataChunks, retransmits, sacks int
	beforeAckAdmitted, beforeAckRefused               int
	firstRefusalRouteCount, firstRefusalSctpBuffered  int
	firstRefusalDataChunks                            int
	firstRefusal, firstAck, drain                     time.Duration
	initialCwnd, finalCwnd, minReceiverWindow         uint32
	wireWrites, wireBytes, peakPoolBytes              int64
}

func runProviderUdpSctpAckExperiment(t *testing.T, roundTrip time.Duration, immediate bool) (result udpSctpAckExperimentResult) {
	return runProviderUdpSctpBatchExperiment(t, roundTrip, immediate, false)
}

func runProviderUdpSctpBatchExperiment(t *testing.T, roundTrip time.Duration, immediate bool, streamEnvelope bool) (result udpSctpAckExperimentResult) {
	return runProviderUdpSctpCapacityExperiment(t, roundTrip, immediate, streamEnvelope, 4)
}

func runProviderUdpSctpCapacityExperiment(t *testing.T, roundTrip time.Duration, immediate bool, streamEnvelope bool, carrierSlots int) (result udpSctpAckExperimentResult) {
	return runProviderUdpSctpQueueExperiment(t, roundTrip, immediate, streamEnvelope, carrierSlots, 0)
}

type udpSctpProductionQueueExperimentSettings struct {
	enabled bool
	budget  *TransferMemoryBudget
}

func runProviderUdpSctpQueueExperiment(t *testing.T, roundTrip time.Duration, immediate bool, streamEnvelope bool, carrierSlots, compactByteLimit int, production ...udpSctpProductionQueueExperimentSettings) (result udpSctpAckExperimentResult) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		left := &udpSctpAckExperimentConn{ctx: ctx, cancel: cancel, incoming: make(chan udpSctpAckExperimentPacket, 1024), immediate: immediate, dataTsns: map[uint32]bool{}}
		right := &udpSctpAckExperimentConn{ctx: ctx, cancel: cancel, incoming: make(chan udpSctpAckExperimentPacket, 1024), dataTsns: map[uint32]bool{}}
		left.peer, right.peer = right, left
		type associationResult struct {
			association *sctp.Association
			err         error
		}
		opened := make(chan associationResult, 1)
		settings := DefaultWebRtcSettings()
		config := func(conn net.Conn) sctp.Config {
			return sctp.Config{NetConn: conn, BlockWrite: true, MTU: 1191,
				MaxReceiveBufferSize: uint32(settings.ReceiveBufferSize), MaxMessageSize: uint32(settings.MaxMessageSize),
				MinCwnd: settings.SctpMinCwnd, FastRtxWnd: settings.SctpFastRtxWnd, CwndCAStep: settings.SctpCwndCAStep}
		}
		go func() {
			association, err := sctp.ServerWithOptions(config(right))
			opened <- associationResult{association, err}
		}()
		leftAssociation, err := sctp.ClientWithOptions(config(left))
		if err != nil {
			t.Fatal(err)
		}
		defer leftAssociation.Close()
		rightResult := <-opened
		if rightResult.err != nil {
			t.Fatal(rightResult.err)
		}
		defer rightResult.association.Close()
		leftStream, err := leftAssociation.OpenStream(0, sctp.PayloadTypeWebRTCBinary)
		if err != nil {
			t.Fatal(err)
		}
		rightStream, err := rightResult.association.OpenStream(0, sctp.PayloadTypeWebRTCBinary)
		if err != nil {
			t.Fatal(err)
		}
		leftStream.SetReliabilityParams(true, sctp.ReliabilityTypeReliable, 0)
		rightStream.SetReliabilityParams(true, sctp.ReliabilityTypeReliable, 0)

		clientSettings := DefaultClientSettings()
		clientSettings.EncryptionSettings.Mode = EncryptionModeOff
		provider, client, _ := newProviderTransferKeyTestFixtureWithClientSettings(t, DefaultRemoteUserNatProviderSettings(), clientSettings)
		defer closeTransferGroupTestClient(t, client)
		defer provider.Close()
		peerId := NewId()
		client.ContractManager().AddNoContractPeer(peerId)
		p2pSettings := DefaultP2pTransportSettings()
		p2pSettings.DataPlaneMode = P2pDataPlaneModeLegacyOnly
		p2pSettings.ChannelBufferSize = carrierSlots
		if len(production) == 0 || !production[0].enabled {
			// Preserve the historical control independently of shipped defaults.
			p2pSettings.LegacySendQueueByteCount = 0
		}
		var rootsBaseline, peakRoots atomic.Int64
		var measuring atomic.Bool
		sampleRoots := func() {
			if !measuring.Load() {
				return
			}
			value := int64(MessagePoolOutstandingByteCount()) - rootsBaseline.Load()
			for prior := peakRoots.Load(); prior < value; prior = peakRoots.Load() {
				if peakRoots.CompareAndSwap(prior, value) {
					break
				}
			}
		}
		physicalStream := &udpSctpAckExperimentStream{Stream: leftStream, sampleRoots: sampleRoots}
		if len(production) > 0 {
			physicalStream.budget = production[0].budget
		}
		var sendConn net.Conn = physicalStream
		var compactQueue *udpSctpCompactQueueExperiment
		if compactByteLimit > 0 {
			compactQueue = newUdpSctpCompactQueueExperiment(physicalStream, compactByteLimit)
			sendConn = compactQueue
		}
		transport, route := newP2pSendTransportForPeer(ctx, cancel, sendConn, peerId, NewId(), p2pSettings, false, nil)
		advertised := transport
		if streamEnvelope {
			advertised = &providerUdpStreamDrainExperimentTransport{transport}
		}
		client.RouteManager().UpdateTransport(advertised, []Route{route})
		defer func() {
			client.RouteManager().RemoveTransport(advertised)
			cancel()
			_ = leftAssociation.Close() // unblock an unexpected pending physical write on failure
			if compactQueue != nil {
				compactQueue.closeAndWait()
			}
			if err := transport.(P2pRouteLifecycle).CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		const offeredBitRate = 3_750_000
		const interval = time.Duration(8 * 1000 * int64(time.Second) / offeredBitRate)
		const offerCount = int(time.Second / interval)
		var source, received [offerCount + 1]bool
		receivedCount := 0
		var receivedLock sync.Mutex
		receivedPacketCount := func() int {
			receivedLock.Lock()
			defer receivedLock.Unlock()
			return receivedCount
		}
		readerDone := make(chan struct{})
		go func() {
			defer close(readerDone)
			buffer := make([]byte, settings.MaxMessageSize)
			for {
				n, err := rightStream.Read(buffer)
				if err != nil {
					return
				}
				pack := decodeCompactContractTestPack(t, buffer[:n])
				if !pack.GetNack() {
					t.Error("UDP unexpectedly requested Transfer ACKs")
				}
				receivedLock.Lock()
				for _, frame := range pack.Frames {
					packet := frame.MessageBytes
					index := int(binary.BigEndian.Uint32(packet[len(packet)-4:]))
					if index < 0 || len(received) <= index || received[index] {
						t.Errorf("invalid/duplicate identity %d", index)
						continue
					}
					received[index] = true
					receivedCount++
				}
				receivedLock.Unlock()
			}
		}()
		defer func() { cancel(); _ = rightResult.association.Close(); <-readerDone }()
		completed := make(chan providerReturnSendResult, 1)
		provider.afterReturnSendForTest = func(value providerReturnSendResult) { completed <- value }
		template := craftSecurityPacket(IpProtocolUdp, net.ParseIP("203.0.113.7"), 8080, net.ParseIP("10.0.0.9"), 42001, false, make([]byte, 1000))
		ipPath, err := ParseIpPath(template)
		if err != nil {
			t.Fatal(err)
		}
		key := TransferKey{ForceStream: true, EncryptionRole: protocol.SequenceRole_SequenceRoleServer}
		offer := func(index int) bool {
			packet := MessagePoolCopy(template)
			binary.BigEndian.PutUint32(packet[len(packet)-4:], uint32(index))
			before := time.Now()
			provider.receiveTransfer(SourceId(peerId), key, protocol.ProvideMode_Public, ipPath, packet)
			MessagePoolReturn(packet)
			synctest.Wait()
			sampleRoots()
			select {
			case completion := <-completed:
				if time.Now() != before || completion.packetCount != 1 || completion.packetByteCount != ByteCount(len(template)) {
					t.Fatalf("changed nonblocking source contract: elapsed=%s completion=%+v", time.Since(before), completion)
				}
				source[index] = completion.sent
				return completion.sent
			default:
				t.Fatal("source did not publish a terminal result without waiting")
				return false
			}
		}
		if !offer(0) || receivedPacketCount() != 1 {
			t.Fatal("warmup did not establish the no-contract lane")
		}
		time.Sleep(201 * time.Millisecond)
		synctest.Wait()
		if leftAssociation.BufferedAmount() != 0 {
			t.Fatal("warmup still has unacknowledged SCTP bytes")
		}
		if cap(route) != carrierSlots || clientSettings.SendBufferSettings.SequenceBufferSize != 32 {
			t.Fatal("experiment must use its explicit carrier slots and unchanged 32-slot Transfer admission")
		}
		result.initialCwnd = leftAssociation.CWND()
		result.minReceiverWindow = leftAssociation.RWND()
		rootsBaseline.Store(int64(MessagePoolOutstandingByteCount()))
		measuring.Store(true)
		initialWrites, initialWireBytes := physicalStream.writes.Load(), physicalStream.wireBytes.Load()
		left.delay.Store(int64(roundTrip / 2))
		right.delay.Store(int64(roundTrip / 2))
		left.lock.Lock()
		left.measuring = true
		left.lock.Unlock()
		right.lock.Lock()
		right.measuring = true
		right.lock.Unlock()
		start := time.Now()
		for index := range offerCount {
			beforeFirstAck := time.Since(start) < roundTrip
			if offer(index + 1) {
				result.admitted++
				if beforeFirstAck {
					result.beforeAckAdmitted++
				}
			} else {
				result.refused++
				if beforeFirstAck {
					result.beforeAckRefused++
				}
				if result.firstRefusal == 0 {
					result.firstRefusal = time.Since(start)
					result.firstRefusalRouteCount = len(route)
					result.firstRefusalSctpBuffered = leftAssociation.BufferedAmount()
					left.lock.Lock()
					result.firstRefusalDataChunks = left.dataCount
					left.lock.Unlock()
				}
			}
			result.minReceiverWindow = min(result.minReceiverWindow, leftAssociation.RWND())
			time.Sleep(interval)
		}
		synctest.Wait()
		for deadline := time.Now().Add(10 * time.Second); receivedPacketCount() != 1+result.admitted && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
			synctest.Wait()
		}
		synctest.Wait()
		result.drain = time.Since(start)
		receivedLock.Lock()
		receivedSnapshot := received
		receivedLock.Unlock()
		if source != receivedSnapshot {
			t.Fatalf("SCTP lost/corrupted admitted datagrams: admitted=%d received=%d", result.admitted, receivedPacketCount()-1)
		}
		if client.ctx.Err() != nil {
			t.Fatal("bounded refusals closed the live source")
		}
		if drops := provider.CongestionDropStats(); drops.ReturnQueuePacketCount != 0 || drops.ReturnSendPacketCount != int64(result.refused) || drops.ReturnSendByteCount != ByteCount(result.refused*len(template)) {
			t.Fatalf("incorrect terminal refusal accounting: %+v result=%+v", drops, result)
		}
		left.lock.Lock()
		result.firstAck, result.dataChunks, result.retransmits = left.firstAck.Sub(start), left.dataCount, left.repeated
		left.lock.Unlock()
		right.lock.Lock()
		result.sacks = right.sackCount
		right.lock.Unlock()
		result.finalCwnd = leftAssociation.CWND()
		result.wireWrites = physicalStream.writes.Load() - initialWrites
		result.wireBytes = physicalStream.wireBytes.Load() - initialWireBytes
		result.peakPoolBytes = peakRoots.Load()
	})
	return
}

// A cold congestion window cannot admit a full-bandwidth fixed UDP offer
// before the first RTT returns. Even the optimistic immediate-SACK control
// must still refuse locally at 120 ms RTT; this rejects ACK-delay-only repair
// as sufficient for the frozen all-source-admitted PERFVAR gate.
func TestProviderUdpSctpImmediateAckCannotRemoveColdRttRefusal(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, arm := range []struct {
		name      string
		roundTrip time.Duration
		immediate bool
	}{
		{"2ms-normal-ack-control", 2 * time.Millisecond, false},
		{"120ms-normal-ack", 120 * time.Millisecond, false},
		{"120ms-immediate-ack-upper-bound", 120 * time.Millisecond, true},
	} {
		t.Run(arm.name, func(t *testing.T) {
			result := runProviderUdpSctpAckExperiment(t, arm.roundTrip, arm.immediate)
			t.Logf("%+v", result)
			if result.retransmits != 0 {
				t.Fatalf("lossless discriminator retransmitted %d DATA chunks", result.retransmits)
			}
			if result.minReceiverWindow < result.initialCwnd {
				t.Fatalf("receiver backpressure invalidated congestion-window isolation: %+v", result)
			}
			if arm.roundTrip < 100*time.Millisecond {
				if result.refused != 0 {
					t.Fatalf("low-RTT control refused %d datagrams", result.refused)
				}
			} else if result.beforeAckRefused == 0 || result.firstRefusal >= result.firstAck || result.firstAck < arm.roundTrip {
				t.Fatalf("did not isolate a pre-first-ACK capacity refusal: %+v", result)
			} else if result.firstRefusalRouteCount != 4 || result.firstRefusalSctpBuffered < int(result.initialCwnd) {
				t.Fatalf("first refusal did not exhaust the bounded SCTP/route pipeline: %+v", result)
			}
		})
	}
}
