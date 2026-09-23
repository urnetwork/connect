package connect

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// A real-sized packet pays 150 ms at 64 kbit/s. The initial gate lets the
// compact queue accumulate a deterministic backlog before a probe arrives.
type p2pLegacyProbePacingConn struct {
	*p2pProbePressureConn
	start        <-chan struct{}
	probeWritten chan p2pLegacyProbeObservation
	dataWritten  []uint32
	bulkDone     chan struct{}
	bulkCount    int
}

type p2pLegacyProbeObservation struct {
	at        time.Time
	dataCount int
	kind      byte
}

func (self *p2pLegacyProbePacingConn) Write(message []byte) (int, error) {
	select {
	case <-self.ctx.Done():
		return 0, self.ctx.Err()
	case <-self.start:
	}
	time.Sleep(time.Duration(len(message)) * 8 * time.Second / 64_000)
	if isProbe, kind, _, _ := decodeP2pStreamProbe(message); isProbe {
		self.probeWritten <- p2pLegacyProbeObservation{at: time.Now(), dataCount: len(self.dataWritten), kind: kind}
	} else {
		self.dataWritten = append(self.dataWritten, binary.BigEndian.Uint32(message))
		if len(self.dataWritten) == self.bulkCount {
			close(self.bulkDone)
		}
	}
	return len(message), nil
}

func TestP2pStreamProbeLegacyBacklogPreservesBoundedService(t *testing.T) {
	for _, blocker := range []string{"probe-flush", "control-flush", "full-queue", "concurrent-probes"} {
		t.Run(blocker, func(t *testing.T) { testP2pLegacyBacklogProbe(t, blocker) })
	}
}

func testP2pLegacyBacklogProbe(t *testing.T, blocker string) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := DefaultP2pTransportSettings()
		settings.DataPlaneMode = P2pDataPlaneModeLegacyOnly
		settings.ChannelBufferSize = 4
		if blocker == "full-queue" {
			settings.LegacySendQueueByteCount = kib(16)
		}
		start := make(chan struct{})
		conn := &p2pLegacyProbePacingConn{
			p2pProbePressureConn: &p2pProbePressureConn{ctx: ctx},
			start:                start, probeWritten: make(chan p2pLegacyProbeObservation, 2),
			bulkDone: make(chan struct{}), bulkCount: 96,
		}
		if blocker == "control-flush" {
			conn.bulkCount++
		}
		streamId := NewId()
		transport, route := newP2pSendTransportForPeer(ctx, cancel, conn, NewId(), streamId, settings, true, nil)
		sender := transport.(*P2pSendTransport)
		defer sender.CloseAndWait(context.Background())
		admitted := make(chan struct{})
		go func() {
			defer close(admitted)
			for index := range conn.bulkCount {
				size := 1200
				if blocker == "control-flush" && index == conn.bulkCount-1 {
					size = 64
				}
				packet := MessagePoolGet(size)
				binary.BigEndian.PutUint32(packet, uint32(index))
				route <- packet
			}
		}()
		synctest.Wait()
		offeredAt := time.Now()
		go func() {
			sender.probeRequests <- encodeP2pStreamProbe(p2pStreamProbeRequestType, streamId, NewId())
		}()
		probeCount := 1
		if blocker == "concurrent-probes" {
			probeCount++
			go func() {
				sender.probeResponses <- encodeP2pStreamProbe(p2pStreamProbeResponseType, streamId, NewId())
			}()
		}
		synctest.Wait()
		close(start)
		seen := map[byte]bool{}
		for range probeCount {
			observation := <-conn.probeWritten
			dataAtProbe := observation.dataCount
			t.Logf("probe latency=%s bulk packets before probe=%d/%d", observation.at.Sub(offeredAt), dataAtProbe, conn.bulkCount)
			// One current data write and one already-selected next write are a
			// conservative bound; draining all 96 packets would take 14.4 seconds.
			if delay := observation.at.Sub(offeredAt); delay > time.Duration(300+5*probeCount)*time.Millisecond {
				t.Errorf("endpoint probe waited %s behind %d/%d bulk packets at 64 kbit/s", delay, dataAtProbe, conn.bulkCount)
			}
			if dataAtProbe >= conn.bulkCount {
				t.Error("endpoint probe was serviced only after the entire bulk queue drained")
			}
			if seen[observation.kind] {
				t.Error("probe overwritten or duplicated under concurrent request/response admission")
			}
			seen[observation.kind] = true
		}
		<-conn.bulkDone
		<-admitted
		synctest.Wait()
		for index, identity := range conn.dataWritten {
			if identity != uint32(index) {
				t.Fatalf("bulk order[%d]=%d", index, identity)
			}
		}
	})
}

// A permanently replenished control source must still allow every data
// packet its turn. Probe roots are consumed by the existing writer and do not
// change the compact queue's byte reservation or leave a goroutine behind.
func TestP2pLegacyProbeFloodPreservesBulkFairnessAndBudget(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		budget := NewTransferMemoryBudget(kib(256))
		start := make(chan struct{})
		const count = 48
		var data []uint32
		burst, requests, responses := 0, 0, 0
		streamId := NewId()
		sender := &P2pSendTransport{probeRequests: make(chan []byte, 1), probeResponses: make(chan []byte, 1)}
		sender.probeRequests <- encodeP2pStreamProbe(p2pStreamProbeRequestType, streamId, NewId())
		sender.probeResponses <- encodeP2pStreamProbe(p2pStreamProbeResponseType, streamId, NewId())
		defer func() {
			MessagePoolReturn(<-sender.probeRequests)
			MessagePoolReturn(<-sender.probeResponses)
		}()
		q := newP2pLegacySendQueueWithProbes(ctx, cancel, func(wire []byte, _ time.Time) error {
			<-start
			if isProbe, kind, _, _ := decodeP2pStreamProbe(wire); isProbe {
				burst++
				if burst > 2 {
					t.Errorf("%d controls overtook a ready bulk packet", burst)
				}
				if kind == p2pStreamProbeRequestType {
					requests++
					sender.probeRequests <- encodeP2pStreamProbe(kind, streamId, NewId())
				} else {
					responses++
					sender.probeResponses <- encodeP2pStreamProbe(kind, streamId, NewId())
				}
			} else {
				data = append(data, binary.BigEndian.Uint32(wire))
				burst = 0
			}
			return nil
		}, kib(128), budget, sender)
		defer q.stopAndWait()
		for index := range count {
			if err := q.enqueue(legacyQueueTestPacket(uint32(index), 1200), time.Time{}, false); err != nil {
				t.Fatal(err)
			}
		}
		synctest.Wait()
		close(start)
		if err := q.flush(); err != nil {
			t.Fatal(err)
		}
		if len(data) != count || requests != count || responses != count {
			t.Fatalf("data=%d requests=%d responses=%d", len(data), requests, responses)
		}
		for index, identity := range data {
			if identity != uint32(index) {
				t.Fatalf("bulk order[%d]=%d", index, identity)
			}
		}
		q.mutex.Lock()
		retained, peak := q.retained, q.peakRetained
		q.mutex.Unlock()
		if retained != 0 || peak > q.limit || budget.UsedByteCount() != q.ownerCharge {
			t.Fatalf("retained=%d peak=%d budget=%d owner=%d", retained, peak, budget.UsedByteCount(), q.ownerCharge)
		}
		q.stopAndWait()
		if budget.UsedByteCount() != 0 {
			t.Fatal("queue retained its owner after shutdown")
		}
	})
}

// Canceling during the prioritized physical write cannot return its pooled
// root or release the queue owner before that borrowed write has joined.
func TestP2pLegacyProbeCancellationJoinsBorrowedWrite(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		budget := NewTransferMemoryBudget(kib(64))
		entered, release := make(chan struct{}), make(chan struct{})
		q := newP2pLegacySendQueue(ctx, cancel, func(wire []byte, _ time.Time) error {
			close(entered)
			<-release
			if !isP2pStreamProbe(wire) {
				t.Error("borrowed probe root changed")
			}
			return ctx.Err()
		}, kib(16), budget)
		done := make(chan error, 1)
		go func() {
			done <- q.enqueueProbe(encodeP2pStreamProbe(p2pStreamProbeRequestType, NewId(), NewId()), time.Time{})
		}()
		<-entered
		cancel()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("probe ownership returned before physical write joined")
		default:
		}
		if budget.UsedByteCount() != q.ownerCharge {
			t.Fatal("owner released before producer joined")
		}
		close(release)
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("probe result=%v", err)
		}
		q.stopAndWait()
		if budget.UsedByteCount() != 0 {
			t.Fatal("probe cancellation leaked the owner")
		}
	})
}
