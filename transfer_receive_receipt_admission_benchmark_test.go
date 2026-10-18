// Private, uncompiled/unrun P3 benchmark. One measured operation owns and
// joins one complete 32-Pack Client/provider/NAT lifecycle, without synctest.
package connect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

const receiptBurstBenchmarkCount = 32

// Wall-clock intervals are separate from testing.B's full-lifecycle metrics.
// The completion booleans are proof observations, never inferred from timing.
type receiptBurstBenchmarkResult struct {
	queued bool
	// These two counts are consumed notifications / released final items.
	callbacks   int
	finalOwners int
	// These counts census actual producers after all owners have joined.
	actualCallbacks      int64
	deliveredFrames      int64
	constructedTcpOwners int64
	cumulativeAck        bool
	joined               bool
	witnesses            int
	diagnosticPackets    int
	wireAcks             int
	routeCollectorJoined bool
	receiveBytes         ByteCount
	packBytes            ByteCount
	deliveryElapsed      time.Duration
	joinedElapsed        time.Duration
}

// Each call returns only after every owned worker joins, including errors.
// The optional cancellation edge exists only for the owner-control test.
func runReceiptBurstBenchmarkOwner(cancelQueued bool) (result receiptBurstBenchmarkResult, resultErr error) {
	baselineRoots, baselineBytes := messagePoolOutstandingSnapshot()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	start := make(chan struct{})
	releaseStart := sync.OnceFunc(func() { close(start) })
	entered := make(chan struct{}, 1)
	captured := make(chan *TcpSequence, receiptBurstBenchmarkCount)
	callbacks := make(chan int, receiptBurstBenchmarkCount)
	errorsSeen := make(chan error, 1)
	route := make(Route, 2*receiptBurstBenchmarkCount)
	source, sequenceId := SourceId(NewId()), NewId()
	receiveBudget := NewTransferMemoryBudget(256 * 1024)
	packBudget := NewTransferMemoryBudget(64 * 1024)
	var client *Client
	var provider *RemoteUserNatProvider
	var nat *LocalUserNat
	var sequence *ReceiveSequence
	var removeTransport func()
	var witnesses [][]byte
	var uncanceledDials atomic.Int64
	var actualCallbacks, constructedTcpOwners atomic.Int64
	var delivered [receiptBurstBenchmarkCount]atomic.Int64
	var burstStart time.Time
	var gateway receiptGatewayEvidence
	var routeDone, routeProducersDone chan struct{}
	// Error delivery never blocks the shared callback or final-worker hook.
	fail := func(err error) {
		select {
		case errorsSeen <- err:
		default:
		}
		cancel()
	}
	defer func() {
		var cleanupErr error
		cancel()
		releaseStart()
		if provider != nil {
			provider.Close()
		}
		if nat != nil {
			cleanupErr = errors.Join(cleanupErr, nat.CloseAndWait(context.Background()))
		}
		if client != nil {
			cleanupErr = errors.Join(cleanupErr, client.CloseAndWait(context.Background()))
		}
		result.actualCallbacks = actualCallbacks.Load()
		result.constructedTcpOwners = constructedTcpOwners.Load()
		for index := range delivered {
			result.deliveredFrames += delivered[index].Load()
		}
		if removeTransport != nil {
			removeTransport()
		}
		if routeDone != nil {
			// No gateway producer survives Client.CloseAndWait. The collector
			// drains the remaining bounded route only after that proof.
			close(routeProducersDone)
			<-routeDone
			result.routeCollectorJoined = true
		} else {
			for len(route) != 0 {
				packet := <-route
				_, err := gateway.consume(packet, client.ClientId(), source.SourceId, provider)
				MessagePoolReturn(packet)
				cleanupErr = errors.Join(cleanupErr, err)
			}
		}
		result.diagnosticPackets, result.wireAcks = gateway.diagnosticPackets, gateway.ackPackets
		for _, witness := range witnesses {
			if !MessagePoolReturn(witness) {
				cleanupErr = errors.Join(cleanupErr, errors.New("benchmark packet retained another owner after all joins"))
			}
			result.witnesses++
		}
		select {
		case err := <-errorsSeen:
			cleanupErr = errors.Join(cleanupErr, err)
		default:
		}
		result.receiveBytes, result.packBytes = receiveBudget.UsedByteCount(), packBudget.UsedByteCount()
		if result.receiveBytes != 0 || result.packBytes != 0 || uncanceledDials.Load() != 0 {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("benchmark owner closure: receive=%d Pack=%d uncanceled-dials=%d",
				result.receiveBytes, result.packBytes, uncanceledDials.Load()))
		}
		if sequence != nil {
			select {
			case <-sequence.done:
				if sequence.deliveryQueue != nil && sequence.deliveryQueue.pending() {
					cleanupErr = errors.Join(cleanupErr, errors.New("benchmark retained a receipt after join"))
				}
				if result.cumulativeAck && sequence.nextSequenceNumber != receiptBurstBenchmarkCount {
					cleanupErr = errors.Join(cleanupErr, errors.New("benchmark cumulative ACK did not cover the actual receive head"))
				}
			default:
				cleanupErr = errors.Join(cleanupErr, errors.New("benchmark receive/ACK worker did not join"))
			}
		}
		roots, bytes := messagePoolOutstandingSnapshot()
		if roots != baselineRoots || bytes != baselineBytes {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("benchmark pool boundary changed: roots %d -> %d, bytes %d -> %d",
				baselineRoots, roots, baselineBytes, bytes))
		}
		// Joined means the owned synchronous joins returned and all custody
		// checks passed. A canceled workload may still have a joined owner.
		result.joined = cleanupErr == nil
		resultErr = errors.Join(resultErr, cleanupErr)
		if !burstStart.IsZero() {
			result.joinedElapsed = time.Since(burstStart)
		}
	}()

	settings := DefaultClientSettings()
	settings.Log = NewNoopLogger()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	receive := DefaultReceiveBufferSettingsWithBufferSize(receiptBurstBenchmarkCount)
	receive.ProgressObserver = nil
	receive.IdleTimeout, receive.GapTimeout, receive.MaxPeerAuditDuration = time.Hour, time.Hour, time.Hour
	receive.AckCompressTimeout, receive.WriteTimeout = 0, 0
	receive.ReliablePackHandoffTimeout, receive.H1PackHandoffTimeout = 0, 0
	receive.ReceiveQueueBudget, receive.ReceiveQueueMinByteCount = receiveBudget, 0
	receive.ReceiveQueueMaxByteCount = 512 * 1024
	receive.ReceiveQueueRetainedByteAccounting = true
	receive.PackQueueBudget, receive.PackQueueRetainedByteAccounting = packBudget, true
	receive.beforeRunReceiveSequenceForTest = func(receiveSequenceId) {
		select {
		case entered <- struct{}{}:
		default:
			fail(errors.New("benchmark created more than one receive owner"))
		}
		<-start
	}
	settings.ReceiveBufferSettings = receive
	client = NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	client.ContractManager().AddNoContractPeer(source.SourceId)
	transport := NewSendGatewayTransport()
	client.RouteManager().UpdateTransport(transport, []Route{route})
	removeTransport = func() { client.RouteManager().RemoveTransport(transport) }

	natSettings := DefaultLocalUserNatSettingsWithBufferSize(1)
	natSettings.Log = NewNoopLogger()
	natSettings.TcpBufferSettings.WriteTimeout = 0
	natSettings.TcpBufferSettings.beforeSequenceRunWithStateForTest = func(tcp *TcpSequence) {
		constructedTcpOwners.Add(1)
		select {
		case captured <- tcp:
		case <-ctx.Done():
		}
		<-tcp.ctx.Done()
	}
	natSettings.TcpBufferSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(dialCtx context.Context, _ string, _ string) (net.Conn, error) {
			if dialCtx.Err() == nil {
				uncanceledDials.Add(1)
				fail(errors.New("benchmark attempted an uncanceled upstream dial"))
			}
			return nil, context.Canceled
		},
	}
	nat = NewLocalUserNat(ctx, "synthetic receipt burst benchmark", natSettings)
	providerSettings := DefaultRemoteUserNatProviderSettings()
	providerSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
	provider = NewRemoteUserNatProvider(client, nat, providerSettings)
	client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		actualCallbacks.Add(1)
		for _, frame := range frames {
			packet, err := ipPacketToProviderBytes(frame)
			var path IpPath
			if err == nil {
				_, err = parseIpPathWithPayloadBorrowed(packet, &path)
			}
			index := path.SourcePort - 42000
			if err != nil || index < 0 || index >= receiptBurstBenchmarkCount {
				fail(errors.New("benchmark callback received a foreign or malformed packet"))
				return
			}
			if delivered[index].Add(1) != 1 {
				fail(errors.New("benchmark callback delivered a packet twice"))
				return
			}
			select {
			case callbacks <- index:
			default:
				fail(errors.New("benchmark callback evidence channel exceeded its fixed count"))
			}
		}
	})

	var lastId Id
	for index := 0; index < receiptBurstBenchmarkCount; index++ {
		lastId = NewId()
		path := &IpPath{Version: 4, Protocol: IpProtocolTcp,
			SourceIp: net.ParseIP("192.0.2.1").To4(), SourcePort: 42000 + index,
			DestinationIp: net.ParseIP("198.51.100.2").To4(), DestinationPort: 443}
		packet := MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagSyn, 100, nil))
		witnesses = append(witnesses, MessagePoolShareReadOnly(packet))
		frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
		if err != nil {
			MessagePoolReturn(packet)
			return result, err
		}
		pack := &ReceivePack{Source: source, SequenceId: sequenceId,
			Pack: &protocol.Pack{MessageId: lastId.Bytes(), SequenceId: sequenceId.Bytes(),
				SequenceNumber: uint64(index), Head: index == 0, Frames: []*protocol.Frame{frame}},
			ReceiveCallback: client.receiveCallback, Ctx: ctx, Unwrapped: true,
			EncryptionRole: sequenceTlsRoleServer, TransportType: TransportTypeH1, MessageByteCount: ByteCount(len(packet))}
		if err := gateway.expectPack(pack.Pack); err != nil {
			pack.messagePoolReturn()
			return result, err
		}
		if accepted, err := client.receiveBuffer.Pack(pack, 0); !accepted || err != nil {
			pack.messagePoolReturn()
			return result, fmt.Errorf("benchmark Pack admission: accepted=%t error=%v", accepted, err)
		}
	}
	select {
	case <-entered:
	case <-ctx.Done():
		return result, ctx.Err()
	}
	client.receiveBuffer.mutex.Lock()
	sequence = client.receiveBuffer.receiveSequences[receiveSequenceId{Source: source,
		SequenceId: sequenceId, EncryptionRole: sequenceTlsRoleServer}]
	client.receiveBuffer.mutex.Unlock()
	if sequence == nil || len(sequence.packs) != receiptBurstBenchmarkCount || receive.ProgressObserver != nil {
		return result, errors.New("benchmark did not establish one nil-observer queued 32-Pack burst")
	}
	result.queued = true
	finalAck, diagnosticSeen := make(chan struct{}, 1), make(chan struct{}, 1)
	routeDone, routeProducersDone = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(routeDone)
		consume := func(packet []byte) {
			before := gateway.diagnosticPackets
			ack, err := gateway.consume(packet, client.ClientId(), source.SourceId, provider)
			MessagePoolReturn(packet)
			if err != nil {
				fail(err)
				return
			}
			if before == 0 && gateway.diagnosticPackets == 1 {
				diagnosticSeen <- struct{}{}
			}
			if ack != nil && !ack.Selective {
				messageId, _ := IdFromBytes(ack.MessageId)
				if messageId == lastId {
					select {
					case finalAck <- struct{}{}:
					default:
					}
				}
			}
		}
		for {
			select {
			case packet := <-route:
				consume(packet)
			case <-routeProducersDone:
				for len(route) != 0 {
					consume(<-route)
				}
				return
			}
		}
	}()
	if cancelQueued {
		// The receive worker is still before Run. Run checks ctx.Err before
		// dequeuing any Pack; this narrow control admits no callback/TCP work.
		cancel()
		return result, context.Canceled
	}

	burstStart = time.Now()
	releaseStart()
	var finalSeen [receiptBurstBenchmarkCount]bool
	for index := 0; index < receiptBurstBenchmarkCount; index++ {
		var tcp *TcpSequence
		select {
		case tcp = <-captured:
		case <-ctx.Done():
			return result, ctx.Err()
		}
		select {
		case item := <-tcp.sendItems:
			port := int(item.tcp.sourcePort)
			position := port - 42000
			valid := position >= 0 && position < receiptBurstBenchmarkCount &&
				!finalSeen[position] && item.tcp.syn && item.tcp.seq == 100 && len(item.tcp.payload) == 0
			item.release()
			if !valid {
				return result, errors.New("benchmark final TCP owner identity changed")
			}
			finalSeen[position] = true
			result.finalOwners++
		case <-ctx.Done():
			return result, ctx.Err()
		}
	}
	for index := 0; index < receiptBurstBenchmarkCount; index++ {
		select {
		case <-callbacks:
			result.callbacks++
		case <-ctx.Done():
			return result, ctx.Err()
		}
	}
	select {
	case <-finalAck:
		result.cumulativeAck = true
	case <-ctx.Done():
		return result, ctx.Err()
	}
	// The unchanged provider publisher must actually reach the wire too.
	// This is observed, not silently dropped or given a fabricated peer ACK.
	select {
	case <-diagnosticSeen:
	case <-ctx.Done():
		return result, ctx.Err()
	}
	// Both variants include real delivery/ACK plus initial diagnostic
	// publication; the diagnostic resend owner remains until final cleanup.
	result.deliveryElapsed = time.Since(burstStart)
	return result, nil
}

// One operation includes setup, queued burst, actual callbacks/final owners,
// wire ACK, cancellation, all joins and exact root reconciliation. No owner
// from iteration i survives into i+1; no global AllocsPerRun races are used.
func BenchmarkReceiveReceiptAdmissionHealthyBurst32(b *testing.B) {
	assertMessagePoolOwnership(b)
	b.ReportAllocs()
	var deliveryTotal, joinedTotal time.Duration
	var joinedMinimum, joinedMaximum time.Duration
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		result, err := runReceiptBurstBenchmarkOwner(false)
		if err != nil || !result.joined || !result.queued ||
			result.callbacks != receiptBurstBenchmarkCount || result.finalOwners != receiptBurstBenchmarkCount ||
			result.actualCallbacks != receiptBurstBenchmarkCount || result.deliveredFrames != receiptBurstBenchmarkCount ||
			result.constructedTcpOwners != receiptBurstBenchmarkCount ||
			!result.cumulativeAck || result.witnesses != receiptBurstBenchmarkCount ||
			!result.routeCollectorJoined || result.diagnosticPackets < 1 || result.wireAcks < 1 {
			b.Fatalf("unqualified benchmark owner: result=%+v error=%v", result, err)
		}
		deliveryTotal += result.deliveryElapsed
		joinedTotal += result.joinedElapsed
		if index == 0 || result.joinedElapsed < joinedMinimum {
			joinedMinimum = result.joinedElapsed
		}
		if joinedMaximum < result.joinedElapsed {
			joinedMaximum = result.joinedElapsed
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(deliveryTotal.Nanoseconds())/float64(b.N), "delivery-ns/burst")
	b.ReportMetric(float64(joinedTotal.Nanoseconds())/float64(b.N), "joined-ns/burst")
	b.ReportMetric(float64(joinedMinimum.Nanoseconds()), "joined-min-ns")
	b.ReportMetric(float64(joinedMaximum.Nanoseconds()), "joined-max-ns")
	b.ReportMetric(receiptBurstBenchmarkCount, "Packs/op")
}

// The exact benchmark owner is also a correctness control, with no latency
// threshold and no measurement dependent on scheduler luck or synthetic time.
func TestReceiveReceiptAdmissionBenchmarkHealthyOwnerJoins(t *testing.T) {
	assertMessagePoolOwnership(t)
	result, err := runReceiptBurstBenchmarkOwner(false)
	if err != nil || !result.joined || !result.queued ||
		result.callbacks != receiptBurstBenchmarkCount || result.finalOwners != receiptBurstBenchmarkCount ||
		result.actualCallbacks != receiptBurstBenchmarkCount || result.deliveredFrames != receiptBurstBenchmarkCount ||
		result.constructedTcpOwners != receiptBurstBenchmarkCount ||
		!result.cumulativeAck || result.witnesses != receiptBurstBenchmarkCount ||
		!result.routeCollectorJoined || result.diagnosticPackets < 1 || result.wireAcks < 1 {
		t.Fatalf("healthy benchmark owner failed its evidence boundary: result=%+v error=%v", result, err)
	}
}

// Cancel with every Pack really queued and the actual receive worker still
// held; cleanup must release the gate, drain all roots, and join without ACK.
func TestReceiveReceiptAdmissionBenchmarkQueuedCancelJoins(t *testing.T) {
	assertMessagePoolOwnership(t)
	result, err := runReceiptBurstBenchmarkOwner(true)
	if !errors.Is(err, context.Canceled) || !result.queued || !result.joined ||
		result.callbacks != 0 || result.finalOwners != 0 || result.cumulativeAck ||
		result.actualCallbacks != 0 || result.deliveredFrames != 0 || result.constructedTcpOwners != 0 ||
		result.witnesses != receiptBurstBenchmarkCount || !result.routeCollectorJoined ||
		result.diagnosticPackets != 0 || result.wireAcks != 0 {
		t.Fatalf("queued benchmark cancellation escaped ownership: result=%+v error=%v", result, err)
	}
}
