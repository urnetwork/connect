//go:build !js

package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"runtime"
	"slices"
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
	ctx                                       context.Context
	cancel                                    context.CancelFunc
	incoming                                  chan udpSctpAckExperimentPacket
	peer                                      *udpSctpAckExperimentConn
	delay                                     atomic.Int64
	immediate                                 bool
	lock                                      sync.Mutex
	measuring                                 bool
	firstAck                                  time.Time
	dataTsns                                  map[uint32]bool
	dataCount                                 int
	repeated                                  int
	sackCount                                 int
	dropEvery, dropFirst, dataWrites, dropped int
	ledgerLink                                *udpSctpLedgerLink
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
	hasData := false
	err := udpSctpAckExperimentChunks(owned, func(kind byte, chunk []byte) {
		if kind == 0 || kind == 64 { // DATA and I-DATA
			hasData = true
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
	drop := false
	if c.measuring && hasData {
		c.dataWrites++
		drop = c.dataWrites <= c.dropFirst || (c.dropEvery > 0 && c.dataWrites%c.dropEvery == 0)
		if drop {
			c.dropped++
		}
	}
	c.lock.Unlock()
	if err != nil {
		return 0, err
	}
	if drop {
		return len(packet), nil
	}
	if c.immediate {
		clear(owned[8:12])
		binary.LittleEndian.PutUint32(owned[8:12], crc32.Checksum(owned, crc32.MakeTable(crc32.Castagnoli)))
	}
	if c.ledgerLink != nil {
		return c.ledgerLink.submit(owned)
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
	writes           atomic.Int64
	wireBytes        atomic.Int64
	sampleRoots      func()
	budget           *TransferMemoryBudget
	yieldBeforeWrite bool
	writeDelay       atomic.Int64
	ctx              context.Context
	serviceLimit     uint64
	serviceWake      chan struct{}
	dynamicService   *udpSctpDynamicServiceExperiment
}

func (s *udpSctpAckExperimentStream) legacySendMemoryBudget() *TransferMemoryBudget { return s.budget }

func (s *udpSctpAckExperimentStream) Write(packet []byte) (int, error) {
	if delay := time.Duration(s.writeDelay.Load()); delay > 0 {
		time.Sleep(delay)
	}
	if s.yieldBeforeWrite {
		runtime.Gosched()
	}
	if s.dynamicService != nil {
		if err := s.dynamicService.admit(len(packet)); err != nil {
			return 0, err
		}
	} else if s.serviceLimit > 0 {
		if s.serviceLimit < uint64(len(packet)) {
			return 0, io.ErrShortBuffer
		}
		threshold := s.serviceLimit - uint64(len(packet))
		s.Stream.SetBufferedAmountLowThreshold(threshold)
		for s.Stream.BufferedAmount() > threshold {
			select {
			case <-s.ctx.Done():
				return 0, net.ErrClosed
			case <-s.serviceWake:
			}
		}
	}
	if s.sampleRoots != nil {
		s.sampleRoots()
	}
	s.writes.Add(1)
	s.wireBytes.Add(int64(len(packet)))
	n, err := s.Stream.Write(packet)
	if s.dynamicService != nil {
		s.dynamicService.finishWrite()
	}
	if s.sampleRoots != nil {
		s.sampleRoots()
	}
	return n, err
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
	cwndGrowthCount, cwndSkippedBacklogged            int64
	peakSctpBufferedBytes, peakPoolAndSctpBytes       int64
	serviceWindowCharge                               ByteCount
	peakServiceCharge, peakSharedBudgetBytes          ByteCount
	injectedLossPackets                               int
	finalSctpStreamBytes                              uint64
	finalSctpAssociationBytes                         int
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
	enabled                                   bool
	budget                                    *TransferMemoryBudget
	traceWindow                               bool
	yieldBeforeWrite                          bool
	writeDelay                                time.Duration
	serviceWindowByteCount                    ByteCount
	dynamicServiceCharge                      bool
	dropEveryDataPacket, dropFirstDataPackets int
	ledgerProfile                             *udpSctpLedgerProfile
	ledgerSeed                                int64
	settledLedger                             func(udpSctpLedgerPoint)
	closedLedgerLinks                         func(udpSctpLedgerLinkStats, udpSctpLedgerLinkStats)
	frozenEnvelope                            bool
}

func runProviderUdpSctpQueueExperiment(t *testing.T, roundTrip time.Duration, immediate bool, streamEnvelope bool, carrierSlots, compactByteLimit int, production ...udpSctpProductionQueueExperimentSettings) (result udpSctpAckExperimentResult) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if len(production) > 0 && production[0].serviceWindowByteCount > 0 {
			// The candidate pre-reserves twice its strict SCTP payload window,
			// plus 8 KiB for chunk/queue metadata. This is test-only conservative
			// accounting, not a claim about measured runtime span ownership.
			result.serviceWindowCharge = 2*production[0].serviceWindowByteCount + kib(8)
			if production[0].dynamicServiceCharge {
				result.serviceWindowCharge = kib(8)
			}
			if production[0].budget == nil || !production[0].budget.TryReserve(result.serviceWindowCharge) {
				t.Fatal("bounded SCTP service-window reservation failed")
			}
			defer production[0].budget.Release(result.serviceWindowCharge)
		}
		left := &udpSctpAckExperimentConn{ctx: ctx, cancel: cancel, incoming: make(chan udpSctpAckExperimentPacket, 1024), immediate: immediate, dataTsns: map[uint32]bool{}}
		right := &udpSctpAckExperimentConn{ctx: ctx, cancel: cancel, incoming: make(chan udpSctpAckExperimentPacket, 1024), dataTsns: map[uint32]bool{}}
		left.peer, right.peer = right, left
		if len(production) > 0 {
			left.dropEvery = production[0].dropEveryDataPacket
			left.dropFirst = production[0].dropFirstDataPackets
		}
		type associationResult struct {
			association *sctp.Association
			err         error
		}
		opened := make(chan associationResult, 1)
		settings := DefaultWebRtcSettings()
		windowTrace := newUdpSctpWindowExperimentLogger()
		config := func(conn net.Conn) sctp.Config {
			configuration := sctp.Config{NetConn: conn, BlockWrite: true, MTU: 1191,
				MaxReceiveBufferSize: uint32(settings.ReceiveBufferSize), MaxMessageSize: uint32(settings.MaxMessageSize),
				MinCwnd: settings.SctpMinCwnd, FastRtxWnd: settings.SctpFastRtxWnd, CwndCAStep: settings.SctpCwndCAStep}
			if conn == left && len(production) > 0 && production[0].traceWindow {
				configuration.LoggerFactory = windowTrace
			}
			if conn == left && len(production) > 0 && production[0].serviceWindowByteCount > 0 {
				configuration.BlockWrite = false
			}
			return configuration
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
		if len(production) > 0 && production[0].frozenEnvelope {
			clientSettings.DefaultTransferOpts.Ack = false
			clientSettings.Log = NewNoopLogger()
		}
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
		var peakSctpBuffered, peakPoolAndSctp atomic.Int64
		var peakSharedBudget atomic.Int64
		var measuring atomic.Bool
		sampleRoots := func() {
			if !measuring.Load() {
				return
			}
			value := int64(MessagePoolOutstandingByteCount()) - rootsBaseline.Load()
			if len(production) > 0 && production[0].budget != nil {
				used := int64(production[0].budget.UsedByteCount())
				for prior := peakSharedBudget.Load(); prior < used; prior = peakSharedBudget.Load() {
					if peakSharedBudget.CompareAndSwap(prior, used) {
						break
					}
				}
			}
			sctpBuffered := int64(leftAssociation.BufferedAmount())
			for prior := peakSctpBuffered.Load(); prior < sctpBuffered; prior = peakSctpBuffered.Load() {
				if peakSctpBuffered.CompareAndSwap(prior, sctpBuffered) {
					break
				}
			}
			for prior := peakPoolAndSctp.Load(); prior < value+sctpBuffered; prior = peakPoolAndSctp.Load() {
				if peakPoolAndSctp.CompareAndSwap(prior, value+sctpBuffered) {
					break
				}
			}
			for prior := peakRoots.Load(); prior < value; prior = peakRoots.Load() {
				if peakRoots.CompareAndSwap(prior, value) {
					break
				}
			}
		}
		physicalStream := &udpSctpAckExperimentStream{Stream: leftStream, sampleRoots: sampleRoots}
		physicalStream.ctx = ctx
		if len(production) > 0 {
			physicalStream.budget = production[0].budget
			physicalStream.yieldBeforeWrite = production[0].yieldBeforeWrite
			physicalStream.serviceLimit = uint64(production[0].serviceWindowByteCount)
			if physicalStream.serviceLimit > 0 {
				physicalStream.serviceWake = make(chan struct{}, 1)
				if production[0].dynamicServiceCharge {
					physicalStream.dynamicService = &udpSctpDynamicServiceExperiment{stream: physicalStream}
					leftStream.OnBufferedAmountLow(physicalStream.dynamicService.releaseAcknowledged)
					defer func() {
						_ = leftAssociation.Close()
						physicalStream.dynamicService.close()
					}()
				} else {
					leftStream.OnBufferedAmountLow(func() { notifyP2pLegacySendQueue(physicalStream.serviceWake) })
				}
			}
		}
		windowTrace.physicalWrites.Store(&physicalStream.writes)
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
		offeredBitRate := int64(3_750_000)
		if len(production) > 0 && production[0].ledgerProfile != nil {
			offeredBitRate = production[0].ledgerProfile.downRate * 3 / 4
		}
		interval := time.Duration(8 * 1000 * int64(time.Second) / offeredBitRate)
		offerCount := int(time.Second / interval)
		source, received := make([]bool, offerCount+1), make([]bool, offerCount+1)
		receivedCount := 0
		var receivedLock sync.Mutex
		receivedPacketCount := func() int {
			receivedLock.Lock()
			defer receivedLock.Unlock()
			return receivedCount
		}
		template := craftSecurityPacket(IpProtocolUdp, net.ParseIP("203.0.113.7"), 8080, net.ParseIP("10.0.0.9"), 42001, false, make([]byte, 1000))
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
					if len(production) > 0 && production[0].frozenEnvelope && (len(packet) != len(template) || !bytes.Equal(packet[:len(packet)-4], template[:len(template)-4])) {
						t.Error("frozen UDP payload/header changed")
					}
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
		ipPath, err := ParseIpPath(template)
		if err != nil {
			t.Fatal(err)
		}
		key := TransferKey{ForceStream: true, EncryptionRole: protocol.SequenceRole_SequenceRoleServer}
		if len(production) > 0 && production[0].frozenEnvelope {
			key.ForceStream = false
		}
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
				if completion.sent {
					windowTrace.admitted.Add(1)
				}
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
		rootCountBaseline := MessagePoolOutstandingCount()
		measuring.Store(true)
		windowTrace.measuring.Store(true)
		initialWrites, initialWireBytes := physicalStream.writes.Load(), physicalStream.wireBytes.Load()
		if len(production) > 0 {
			physicalStream.writeDelay.Store(int64(production[0].writeDelay))
		}
		left.delay.Store(int64(roundTrip / 2))
		right.delay.Store(int64(roundTrip / 2))
		left.lock.Lock()
		left.measuring = true
		left.lock.Unlock()
		right.lock.Lock()
		right.measuring = true
		right.lock.Unlock()
		start := time.Now()
		if len(production) > 0 && production[0].ledgerProfile != nil {
			profile := production[0].ledgerProfile
			left.ledgerLink = newUdpSctpLedgerLink(ctx, *profile, true, production[0].ledgerSeed, right.incoming)
			right.ledgerLink = newUdpSctpLedgerLink(ctx, *profile, false, production[0].ledgerSeed+1, left.incoming)
			defer func() {
				cancel()
				<-left.ledgerLink.done
				<-right.ledgerLink.done
				if production[0].closedLedgerLinks != nil {
					production[0].closedLedgerLinks(left.ledgerLink.snapshot(), right.ledgerLink.snapshot())
				}
			}()
		}
		for index := range offerCount {
			if want := start.Add(time.Duration(index) * interval); !time.Now().Equal(want) {
				t.Fatalf("changed fixed offer schedule: got=%s want=%s", time.Since(start), want.Sub(start))
			}
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
			if len(production) > 0 && production[0].settledLedger != nil {
				point := udpSctpLedgerPoint{
					At: time.Since(start), Offered: index + 1, Admitted: result.admitted, Refused: result.refused,
					PoolBytes:       ByteCount(MessagePoolOutstandingByteCount()) - ByteCount(rootsBaseline.Load()),
					PoolRoots:       MessagePoolOutstandingCount() - rootCountBaseline,
					SctpStreamBytes: leftStream.BufferedAmount(), SctpAssociationBytes: leftAssociation.BufferedAmount(),
					Cwnd: leftAssociation.CWND(), Rwnd: leftAssociation.RWND(), Route: len(route),
					PhysicalWrites: physicalStream.writes.Load() - initialWrites,
				}
				if production[0].budget != nil {
					point.QueueBudget = production[0].budget.UsedByteCount()
				}
				client.sendBuffer.mutex.Lock()
				for _, sequence := range client.sendBuffer.sendSequences {
					if sequence.destination == peerId && sequence.packAdmission != nil {
						sequence.packAdmission.mutex.Lock()
						point.PackAdmission = sequence.packAdmission.count
						sequence.packAdmission.mutex.Unlock()
						point.PackChannel = len(sequence.packs)
					}
				}
				client.sendBuffer.mutex.Unlock()
				production[0].settledLedger(point)
			}
			time.Sleep(interval)
		}
		synctest.Wait()
		terminal := func() bool {
			if receivedPacketCount() != 1+result.admitted {
				return false
			}
			if len(production) > 0 && production[0].ledgerProfile != nil {
				return leftStream.BufferedAmount() == 0 && leftAssociation.BufferedAmount() == 0
			}
			return true
		}
		for deadline := time.Now().Add(10 * time.Second); !terminal() && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
			synctest.Wait()
		}
		synctest.Wait()
		if !terminal() {
			t.Fatal("SCTP delivery/owner drain exceeded unchanged 10s deadline")
		}
		result.finalSctpStreamBytes = leftStream.BufferedAmount()
		result.finalSctpAssociationBytes = leftAssociation.BufferedAmount()
		result.drain = time.Since(start)
		receivedLock.Lock()
		receivedSnapshot := slices.Clone(received)
		receivedLock.Unlock()
		if !slices.Equal(source, receivedSnapshot) {
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
		result.injectedLossPackets = left.dropped
		left.lock.Unlock()
		right.lock.Lock()
		result.sacks = right.sackCount
		right.lock.Unlock()
		result.finalCwnd = leftAssociation.CWND()
		result.wireWrites = physicalStream.writes.Load() - initialWrites
		result.wireBytes = physicalStream.wireBytes.Load() - initialWireBytes
		result.peakPoolBytes = peakRoots.Load()
		result.cwndGrowthCount = windowTrace.growth.Load()
		result.cwndSkippedBacklogged = windowTrace.skippedBacklogged.Load()
		result.peakSctpBufferedBytes = peakSctpBuffered.Load()
		result.peakPoolAndSctpBytes = peakPoolAndSctp.Load()
		result.peakSharedBudgetBytes = ByteCount(peakSharedBudget.Load())
		result.peakServiceCharge = result.serviceWindowCharge
		if physicalStream.dynamicService != nil {
			result.peakServiceCharge += ByteCount(physicalStream.dynamicService.peakCharge.Load())
		}
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
