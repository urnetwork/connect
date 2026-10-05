package connect

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"

	"github.com/urnetwork/connect/protocol"
)

func requirePayloadLedger(t *testing.T, ledger *TransferPayloadOwnerLedger, send, forward, bytes int64) TransferPayloadOwnerSnapshot {
	t.Helper()
	s := ledger.Snapshot()
	if !s.Enabled || !s.Complete || s.SendAck.Owners != send || s.Forward.Owners != forward {
		t.Fatalf("unexpected payload ownership: %+v, want send=%d forward=%d", s, send, forward)
	}
	for _, g := range []TransferPayloadOwnerGroup{s.SendAck, s.Forward} {
		if g.Owners < 0 || g.BackingByteCharges < 0 || int64(g.AdmittedTotal-g.ReleasedTotal) != g.Owners {
			t.Fatalf("payload conservation failed: %+v", s)
		}
	}
	if s.SendAck.BackingByteCharges+s.Forward.BackingByteCharges != bytes {
		t.Fatalf("backing charges=%d want=%d: %+v", s.SendAck.BackingByteCharges+s.Forward.BackingByteCharges, bytes, s)
	}
	return s
}

func TestTransferPayloadOwnerSnapshotFencesAndConcurrency(t *testing.T) {
	var disabled *TransferPayloadOwnerLedger
	if disabled.Snapshot() != (TransferPayloadOwnerSnapshot{}) || DefaultClientSettings().PayloadOwnerLedger != nil {
		t.Fatal("optional payload accounting enabled by default")
	}
	for _, selector := range []byte{0, transferPayloadOwnerShardCount - 1} {
		var ledger TransferPayloadOwnerLedger
		shard := &ledger.shards[selector]
		finished := false
		view := ledger.snapshot(func(final bool) {
			if !final {
				shard.writers.Add(1)
				shard.sendAck.owners.Add(1)
			} else if !finished {
				finished = true
				shard.sendAck.bytes.Add(2060)
				shard.sendAck.admitted.Add(1)
				shard.revision.Add(1)
				shard.writers.Add(-1)
			}
		})
		if view.Complete || view.SendAck.Owners != 1 || view.SendAck.BackingByteCharges != 0 {
			t.Fatalf("partial update qualified, shard=%d: %+v", selector, view)
		}
		requirePayloadLedger(t, &ledger, 1, 0, 2060)
	}
	var ledger TransferPayloadOwnerLedger
	var wg sync.WaitGroup
	start := make(chan struct{})
	for selector := range byte(transferPayloadOwnerShardCount) {
		wg.Go(func() {
			<-start
			for range 1000 {
				ledger.update(transferPayloadOwnerSendAck, selector, 1, 2060)
				ledger.update(transferPayloadOwnerSendAck, selector, -1, -2060)
			}
		})
	}
	close(start)
	for range 1000 {
		s := ledger.Snapshot()
		if s.Complete && (s.SendAck.Owners < 0 || s.SendAck.BackingByteCharges != s.SendAck.Owners*2060 ||
			int64(s.SendAck.AdmittedTotal-s.SendAck.ReleasedTotal) != s.SendAck.Owners) {
			t.Fatalf("a concurrent incoherent sample was qualified: %+v", s)
		}
	}
	wg.Wait()
	s := requirePayloadLedger(t, &ledger, 0, 0, 0)
	if s.SendAck.AdmittedTotal != 16000 || s.SendAck.ReleasedTotal != 16000 {
		t.Fatalf("sharded totals lost updates: %+v", s)
	}
	if n := testing.AllocsPerRun(100, func() { _ = ledger.Snapshot() }); n != 0 {
		t.Fatalf("snapshot allocates: %g", n)
	}
	// The payload instrumentation must not grow each retained packet object.
	if unsafe.Sizeof(sendItem{}) != 592 || unsafe.Sizeof(ForwardPack{}) != 88 {
		t.Fatalf("packet objects grew: send=%d forward=%d", unsafe.Sizeof(sendItem{}), unsafe.Sizeof(ForwardPack{}))
	}
}

func TestTransferPayloadOwnerSnapshotRefusesInconsistentCounters(t *testing.T) {
	for _, g := range []TransferPayloadOwnerGroup{
		{Owners: -1, BackingByteCharges: -2060, ReleasedTotal: 1},
		{Owners: 1, BackingByteCharges: -1, AdmittedTotal: 1},
		{Owners: 0, BackingByteCharges: 2060, AdmittedTotal: 1, ReleasedTotal: 1},
		{Owners: 1, BackingByteCharges: 2060},
	} {
		var ledger TransferPayloadOwnerLedger
		c := &ledger.shards[0].sendAck
		c.owners.Store(g.Owners)
		c.bytes.Store(g.BackingByteCharges)
		c.admitted.Store(g.AdmittedTotal)
		c.released.Store(g.ReleasedTotal)
		if s := ledger.Snapshot(); !s.Enabled || s.Complete {
			t.Fatalf("inconsistent instrument qualified its sample: %+v", s)
		}
	}
	// A return charged to the wrong shard can look balanced after summing.
	// Validate every shard so aggregation cannot conceal that discrepancy.
	var ledger TransferPayloadOwnerLedger
	ledger.update(transferPayloadOwnerForward, 0, 1, 2060)
	ledger.update(transferPayloadOwnerForward, 15, -1, -2060)
	if s := ledger.Snapshot(); s.Complete || s.Forward.Owners != 0 || s.Forward.BackingByteCharges != 0 {
		t.Fatalf("cross-shard inconsistency was hidden: %+v", s)
	}
}

func payloadLedgerResident(t *testing.T, ledger *TransferPayloadOwnerLedger, runGate <-chan struct{}) (*Client, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultClientSettingsWithBufferSize(4096)
	settings.Log = NewNoopLogger()
	settings.PayloadOwnerLedger = ledger
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	if runGate != nil {
		settings.ForwardBufferSettings.beforeRunForwardSequenceForTest = func(TransferPath) {
			select {
			case <-runGate:
			case <-ctx.Done():
			}
		}
	}
	client := NewClient(ctx, ControlId, NewNoContractClientOob(), settings)
	// Mutating the original optional scope after construction cannot split an
	// admission from its return. No runtime path reads this settings field.
	settings.PayloadOwnerLedger = nil
	t.Cleanup(func() {
		cancel()
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return client, cancel
}

func payloadLedgerForwardWire(t *testing.T, destination Id) []byte {
	t.Helper()
	frame := budgetTestFrame(1200)
	defer MessagePoolReturn(frame.MessageBytes)
	wire, err := ProtoMarshal(&protocol.TransferFrame{
		TransferPath: NewTransferPath(NewId(), destination, Id{}).ToProtobuf(),
		Pack:         &protocol.Pack{MessageId: NewId().Bytes(), SequenceId: NewId().Bytes(), Head: true, Frames: []*protocol.Frame{frame}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func TestTransferPayloadOwnerForwardLoadedOffersAndFinalReturn(t *testing.T) {
	assertMessagePoolOwnership(t)
	oldCopy := DebugTransferCopyOnWrite
	DebugTransferCopyOnWrite = false
	defer func() { DebugTransferCopyOnWrite = oldCopy }()
	synctest.Test(t, func(t *testing.T) {
		var ledger TransferPayloadOwnerLedger
		gate := make(chan struct{})
		clients := make([]*Client, 2)
		stops := make([]context.CancelFunc, 2)
		destinations := []Id{NewId(), NewId()}
		// Exercise distinct shards even though both resident ClientIds are ControlId.
		destinations[0][15], destinations[1][15] = 0, 15
		caller, cancelCaller := context.WithCancel(context.Background())
		defer cancelCaller()
		var witnesses [][]byte
		t.Cleanup(func() {
			for _, b := range witnesses {
				if !MessagePoolReturn(b) {
					t.Error("a joined forward still owns the witness root")
				}
			}
		})
		var bytes int64
		for i := range clients {
			clients[i], stops[i] = payloadLedgerResident(t, &ledger, gate)
			var first []byte
			for n := range 4096 {
				var wire []byte
				if n == 4095 {
					// One root intentionally backs two owners: charges must sum
					// owners, not masquerade as unique physical allocation.
					wire = MessagePoolShareReadOnly(first)
				} else {
					wire = payloadLedgerForwardWire(t, destinations[i])
					witnesses = append(witnesses, MessagePoolShareReadOnly(wire))
					if n == 0 {
						first = wire
					}
				}
				bytes += int64(cap(wire))
				if !clients[i].ForwardWithTimeout(wire, 0, Ctx(caller)) {
					MessagePoolReturn(wire)
					t.Fatal("forward queue did not admit its configured capacity")
				}
			}
		}
		synctest.Wait()
		loaded := requirePayloadLedger(t, &ledger, 0, 8192, bytes)
		// A full queue refuses immediately and with a deadline. Both tracked
		// attempts release without taking caller ownership.
		for _, timeout := range []time.Duration{0, time.Millisecond} {
			wire := payloadLedgerForwardWire(t, destinations[0])
			if clients[0].ForwardWithTimeout(wire, timeout) {
				t.Fatal("full forward queue accepted a refused control")
			}
			MessagePoolReturn(wire)
		}
		pending, stopPending := context.WithCancel(context.Background())
		wire := payloadLedgerForwardWire(t, destinations[0])
		done := make(chan bool, 1)
		go func() { done <- clients[0].ForwardWithTimeout(wire, -1, Ctx(pending)) }()
		synctest.Wait()
		requirePayloadLedger(t, &ledger, 0, 8193, bytes+int64(cap(wire)))
		stopPending()
		if <-done {
			t.Fatal("canceled pending offer transferred caller ownership")
		}
		MessagePoolReturn(wire)
		afterRefusal := requirePayloadLedger(t, &ledger, 0, 8192, bytes)
		if afterRefusal.Forward.AdmittedTotal-loaded.Forward.AdmittedTotal != 3 || afterRefusal.Forward.ReleasedTotal != 3 {
			t.Fatalf("refused offers did not balance: %+v", afterRefusal)
		}
		cancelCaller()
		wire = payloadLedgerForwardWire(t, destinations[0])
		if clients[0].ForwardWithTimeout(wire, 0, Ctx(caller)) {
			t.Fatal("pre-canceled forward offer was accepted")
		}
		MessagePoolReturn(wire)
		if got := requirePayloadLedger(t, &ledger, 0, 8192, bytes); got.Forward.AdmittedTotal != afterRefusal.Forward.AdmittedTotal {
			t.Fatal("pre-canceled offer entered tracking before sequence admission")
		}
		close(gate)
		synctest.Wait()
		// Accepted caller cancellation cannot release either active write or
		// queued siblings. Both real writers are blocked on absent routes.
		requirePayloadLedger(t, &ledger, 0, 8192, bytes)
		for i, client := range clients {
			client.forwardBuffer.mutex.Lock()
			seq := client.forwardBuffer.forwardSequences[DestinationId(destinations[i])]
			client.forwardBuffer.mutex.Unlock()
			if len(seq.packs) != 4095 {
				t.Fatalf("absent route did not retain one active plus queued frames: %d", len(seq.packs))
			}
		}
		route := make(Route, 64)
		clients[0].RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(destinations[0])), []Route{route})
		var delivered int
		pumpDone := make(chan struct{})
		go func() {
			defer close(pumpDone)
			for range 4096 {
				b := <-route
				var frame protocol.TransferFrame
				if err := ProtoUnmarshal(b, &frame); err != nil || frame.Pack == nil {
					t.Error("accepted forward lost its native frame")
				}
				delivered++
				MessagePoolReturn(b)
			}
		}()
		<-pumpDone
		synctest.Wait()
		requirePayloadLedger(t, &ledger, 0, 4096, bytes/2)
		for i, client := range clients {
			stops[i]()
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		final := requirePayloadLedger(t, &ledger, 0, 0, 0)
		t.Logf("resident_clients=2 test_peer_clients=0 accepted=8192 delivered=%d initial_backing_charges=%d distinct_roots=8190 final=%+v", delivered, bytes, final)
	})
}

func BenchmarkTransferPayloadOwnerLedgerParallel(b *testing.B) {
	for _, enabled := range []bool{false, true} {
		b.Run(fmt.Sprintf("enabled=%t", enabled), func(b *testing.B) {
			var ledger *TransferPayloadOwnerLedger
			if enabled {
				ledger = &TransferPayloadOwnerLedger{}
			}
			var selector atomic.Uint32
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				shard := byte(selector.Add(1))
				for pb.Next() {
					if ledger != nil {
						ledger.update(transferPayloadOwnerSendAck, shard, 1, 2060)
						ledger.update(transferPayloadOwnerSendAck, shard, -1, -2060)
					}
				}
			})
		})
	}
}
