package connect

import (
	"context"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/urnetwork/connect/protocol"
)

// Actual client-buffer admission, held Run, lookup removal, held final cleanup,
// and joined message ownership all contribute to one lifecycle. A snapshot must
// remain usable even while all three buffer lookup locks are held.
func TestTransferMemoryOwnerLedgerJoinedLifecycle(t *testing.T) {
	for _, kind := range []string{"send", "receive", "forward"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var ledger, laterSettingsLedger TransferMemoryOwnerLedger
			entered, releaseRun := make(chan struct{}), make(chan struct{})
			cleaning, releaseCleanup := make(chan struct{}), make(chan struct{})
			var enterOnce, cleanupOnce, releaseRunOnce, releaseCleanupOnce sync.Once
			onRun := func() { enterOnce.Do(func() { close(entered) }); <-releaseRun }
			onCleanup := func() { cleanupOnce.Do(func() { close(cleaning) }); <-releaseCleanup }
			destination, source, sequenceID := NewId(), SourceId(NewId()), NewId()
			forwardPath := DestinationId(destination)
			settings := closeWaitClientSettings()
			settings.MemoryOwnerLedger = &ledger
			settings.SendBufferSettings.SequenceBufferSize = 5
			settings.SendBufferSettings.AckBufferSize = 11
			settings.ReceiveBufferSettings.SequenceBufferSize = 7
			settings.ReceiveBufferSettings.H1SequenceBufferSize = 7
			settings.ReceiveBufferSettings.H1SequenceBufferAdaptiveMaxSize = 0
			settings.ForwardBufferSettings.SequenceBufferSize = 13
			switch kind {
			case "send":
				settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
					if id.Destination == destination {
						onRun()
					}
				}
				settings.SendBufferSettings.afterRunSendSequenceForTest = func(id sendSequenceId) {
					if id.Destination == destination {
						onCleanup()
					}
				}
			case "receive":
				settings.ReceiveBufferSettings.beforeRunReceiveSequenceForTest = func(id receiveSequenceId) {
					if id.Source == source && id.SequenceId == sequenceID {
						onRun()
					}
				}
				settings.ReceiveBufferSettings.afterRunReceiveSequenceForTest = func(id receiveSequenceId) {
					if id.Source == source && id.SequenceId == sequenceID {
						onCleanup()
					}
				}
			case "forward":
				settings.ForwardBufferSettings.beforeRunForwardSequenceForTest = func(path TransferPath) {
					if path == forwardPath {
						onRun()
					}
				}
				settings.ForwardBufferSettings.afterRunForwardSequenceForTest = func(path TransferPath) {
					if path == forwardPath {
						onCleanup()
					}
				}
			}
			client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
			// The optional scope is captured once, not reread from mutable settings
			// independently at admission and exit.
			settings.MemoryOwnerLedger = &laterSettingsLedger
			message := newCloseWaitPoolWitness(t)
			defer func() {
				releaseRunOnce.Do(func() { close(releaseRun) })
				releaseCleanupOnce.Do(func() { close(releaseCleanup) })
				cleanupCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
				defer done()
				if err := client.CloseAndWait(cleanupCtx); err != nil {
					t.Error(err)
				}
				message.cleanup()
			}()
			var accepted bool
			var err error
			switch kind {
			case "send":
				accepted, err = client.sendBuffer.Pack(&SendPack{
					Destination: destination, Ctx: ctx, TransferOptions: TransferOptions{Ack: true},
					Frame:            &protocol.Frame{MessageType: protocol.MessageType_TransferPack, MessageBytes: message.owner},
					MessageByteCount: ByteCount(len(message.owner)), AckCallback: func(error) {},
				}, 0)
			case "receive":
				client.ContractManager().AddNoContractPeer(source.SourceId)
				accepted, err = client.receiveBuffer.Pack(closeWaitReceivePack(ctx, source, sequenceID, message.owner), 0)
			case "forward":
				accepted, err = client.forwardBuffer.Pack(&ForwardPack{Destination: forwardPath, Ctx: ctx, TransferFrameBytes: message.owner}, 0)
			}
			if !accepted || err != nil {
				t.Fatalf("admission=%t err=%v", accepted, err)
			}
			waitCloseWaitBarrier(t, ctx, entered, "ledger admitted worker")
			before := ledgerSnapshotAgainstOwners(t, client, &ledger)
			group := ledgerTestGroup(before, kind)
			if group.Workers < 1 || group.CleanupWorkers != 0 || group.KnownChannelSlotBytes == 0 {
				t.Fatalf("admitted worker missing: %+v", before)
			}
			client.Close()
			releaseRunOnce.Do(func() { close(releaseRun) })
			waitCloseWaitBarrier(t, ctx, cleaning, "ledger cleanup worker")
			held := ledgerSnapshotAgainstOwners(t, client, &ledger)
			group = ledgerTestGroup(held, kind)
			if group.Workers < 1 || group.CleanupWorkers < 1 || group.CleanupKnownChannelSlotBytes == 0 {
				t.Fatalf("cleanup owner prematurely forgotten: %+v", held)
			}
			var indexed bool
			switch kind {
			case "send":
				client.sendBuffer.mutex.Lock()
				for id := range client.sendBuffer.sendSequences {
					indexed = indexed || id.Destination == destination
				}
				client.sendBuffer.mutex.Unlock()
			case "receive":
				client.receiveBuffer.mutex.Lock()
				for id := range client.receiveBuffer.receiveSequences {
					indexed = indexed || id.SequenceId == sequenceID
				}
				client.receiveBuffer.mutex.Unlock()
			case "forward":
				client.forwardBuffer.mutex.Lock()
				_, indexed = client.forwardBuffer.forwardSequences[forwardPath]
				client.forwardBuffer.mutex.Unlock()
			}
			if indexed {
				t.Fatal("control did not reach actual lookup removal")
			}
			releaseCleanupOnce.Do(func() { close(releaseCleanup) })
			if err := client.CloseAndWait(ctx); err != nil {
				t.Fatal(err)
			}
			after := ledger.Snapshot()
			if !after.Enabled || !after.Complete {
				t.Fatalf("joined sample unavailable: %+v", after)
			}
			for _, g := range []TransferMemoryOwnerGroup{after.Send, after.Receive, after.Forward} {
				if g.Workers != 0 || g.CleanupWorkers != 0 || g.KnownSequenceStructBytes != 0 ||
					g.KnownChannelSlotBytes != 0 || g.CleanupKnownChannelSlotBytes != 0 || g.AdmittedTotal != g.FinishedTotal {
					t.Fatalf("joined owner retained or accounting unbalanced: %+v", after)
				}
			}
			if got := laterSettingsLedger.Snapshot(); got.Revision != 0 {
				t.Fatalf("scope moved after construction: %+v", got)
			}
			message.requireOwnerReleased(t, "ledger worker")
		})
	}
}

func ledgerTestGroup(s TransferMemoryOwnerSnapshot, kind string) TransferMemoryOwnerGroup {
	switch kind {
	case "send":
		return s.Send
	case "receive":
		return s.Receive
	default:
		return s.Forward
	}
}

func ledgerSnapshotAgainstOwners(t *testing.T, c *Client, l *TransferMemoryOwnerLedger) TransferMemoryOwnerSnapshot {
	t.Helper()
	c.sendBuffer.mutex.Lock()
	defer c.sendBuffer.mutex.Unlock()
	c.receiveBuffer.mutex.Lock()
	defer c.receiveBuffer.mutex.Unlock()
	c.forwardBuffer.mutex.Lock()
	defer c.forwardBuffer.mutex.Unlock()
	result := make(chan TransferMemoryOwnerSnapshot, 1)
	go func() { result <- l.Snapshot() }()
	var s TransferMemoryOwnerSnapshot
	select {
	case s = <-result:
	case <-time.After(time.Second):
		t.Fatal("snapshot waited for an owner lock")
	}
	if !s.Enabled || !s.Complete {
		t.Fatalf("quiescent owner sample unavailable: %+v", s)
	}
	var sendBytes, receiveBytes, forwardBytes int64
	for sequence := range c.sendBuffer.activeSendSequences {
		sendBytes += int64(cap(sequence.packs))*int64(unsafe.Sizeof((*SendPack)(nil))) + int64(cap(sequence.acks))*int64(unsafe.Sizeof(receiveAckMessage{}))
	}
	for sequence := range c.receiveBuffer.activeReceiveSequences {
		receiveBytes += int64(cap(sequence.packs)) * int64(unsafe.Sizeof((*ReceivePack)(nil)))
	}
	for sequence := range c.forwardBuffer.activeForwardSequences {
		forwardBytes += int64(cap(sequence.packs)) * int64(unsafe.Sizeof((*ForwardPack)(nil)))
	}
	if s.Send.Workers != int64(len(c.sendBuffer.activeSendSequences)) || s.Receive.Workers != int64(len(c.receiveBuffer.activeReceiveSequences)) || s.Forward.Workers != int64(len(c.forwardBuffer.activeForwardSequences)) ||
		s.Send.KnownChannelSlotBytes != sendBytes || s.Receive.KnownChannelSlotBytes != receiveBytes || s.Forward.KnownChannelSlotBytes != forwardBytes {
		t.Fatalf("ledger differs from independently held owner maps/capacities: %+v, bytes=%d/%d/%d", s, sendBytes, receiveBytes, forwardBytes)
	}
	if n := testing.AllocsPerRun(100, func() { _ = l.Snapshot() }); n != 0 {
		t.Fatalf("snapshot allocations=%g", n)
	}
	return s
}

func TestTransferMemoryOwnerLedgerUnavailableAndConcurrentUpdates(t *testing.T) {
	var disabled *TransferMemoryOwnerLedger
	if got := disabled.Snapshot(); got != (TransferMemoryOwnerSnapshot{}) {
		t.Fatalf("disabled became a zero-owner proof: %+v", got)
	}
	if DefaultClientSettings().MemoryOwnerLedger != nil {
		t.Fatal("ledger enabled by default")
	}
	var ledger TransferMemoryOwnerLedger
	ledger.writers.Add(1)
	if got := ledger.Snapshot(); !got.Enabled || got.Complete {
		t.Fatalf("overlapping update became coherent: %+v", got)
	}
	ledger.writers.Add(-1)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 1000 {
				ledger.admit(transferMemoryOwnerSend, 17)
				ledger.beginCleanup(transferMemoryOwnerSend, 17)
				ledger.finish(transferMemoryOwnerSend, 17, true)
			}
		})
	}
	for range 1000 {
		s := ledger.Snapshot()
		if s.Complete && (s.Send.Workers < 0 || s.Send.CleanupWorkers < 0 || s.Send.CleanupWorkers > s.Send.Workers ||
			s.Send.KnownChannelSlotBytes != s.Send.Workers*17 || s.Send.CleanupKnownChannelSlotBytes != s.Send.CleanupWorkers*17 ||
			uint64(s.Send.Workers) != s.Send.AdmittedTotal-s.Send.FinishedTotal) {
			t.Fatalf("incoherent concurrent sample claimed complete: %+v", s)
		}
	}
	wg.Wait()
	if got := ledger.Snapshot(); !got.Complete || got.Send.Workers != 0 || got.Send.AdmittedTotal != 8000 || got.Send.FinishedTotal != 8000 {
		t.Fatalf("shared-ledger churn failed conservation: %+v", got)
	}
}

func TestTransferMemoryOwnerLedgerHealthyExchange(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var ledger TransferMemoryOwnerLedger
	settingsA, settingsB := closeWaitClientSettings(), closeWaitClientSettings()
	settingsA.MemoryOwnerLedger, settingsB.MemoryOwnerLedger = &ledger, &ledger
	a := NewClient(ctx, NewId(), NewNoContractClientOob(), settingsA)
	b := NewClient(ctx, NewId(), NewNoContractClientOob(), settingsB)
	a.ContractManager().AddNoContractPeer(b.ClientId())
	b.ContractManager().AddNoContractPeer(a.ClientId())
	toB, toA := make(chan []byte, 8), make(chan []byte, 8)
	a.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{toB})
	b.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{toB})
	b.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{toA})
	a.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{toA})
	defer func() {
		a.Close()
		b.Close()
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if err := a.CloseAndWait(cleanup); err != nil {
			t.Error(err)
		}
		if err := b.CloseAndWait(cleanup); err != nil {
			t.Error(err)
		}
		for _, route := range []chan []byte{toA, toB} {
			for len(route) > 0 {
				MessagePoolReturn(<-route)
			}
		}
	}()
	const count = 16
	acks, received := make(chan error, count), make(chan struct{}, count)
	b.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			if frame.MessageType == protocol.MessageType_TestSimpleMessage {
				select {
				case received <- struct{}{}:
				default:
					t.Error("receive collector overflow")
				}
			}
		}
	})
	for range count {
		frame, err := ToFrame(&protocol.SimpleMessage{Content: "ledger healthy transfer"}, DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		if !a.SendWithTimeout(frame, b.ClientId(), func(err error) {
			select {
			case acks <- err:
			default:
				t.Error("ack collector overflow")
			}
		}, time.Second) {
			t.Fatal("healthy send not admitted")
		}
	}
	for range count {
		select {
		case err := <-acks:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		select {
		case <-received:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	active := ledger.Snapshot()
	if !active.Complete || active.Send.Workers < 1 || active.Receive.Workers < 1 || active.Send.KnownChannelSlotBytes == 0 || active.Receive.KnownChannelSlotBytes == 0 {
		t.Fatalf("healthy delivery owners missing: %+v", active)
	}
	a.Close()
	b.Close()
	if err := a.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	joined := ledger.Snapshot()
	if !joined.Complete {
		t.Fatalf("joined ledger unavailable: %+v", joined)
	}
	for _, g := range []TransferMemoryOwnerGroup{joined.Send, joined.Receive, joined.Forward} {
		if g.Workers != 0 || g.CleanupWorkers != 0 || g.KnownChannelSlotBytes != 0 || g.AdmittedTotal != g.FinishedTotal {
			t.Fatalf("healthy exchange retained lifecycle ownership: %+v", joined)
		}
	}
}

func BenchmarkTransferMemoryOwnerLedger(b *testing.B) {
	b.Run("snapshot", func(b *testing.B) {
		var ledger TransferMemoryOwnerLedger
		ledger.admit(transferMemoryOwnerSend, 1<<40)
		b.ReportAllocs()
		for b.Loop() {
			_ = ledger.Snapshot()
		}
	})
	b.Run("shared_lifecycle", func(b *testing.B) {
		var ledger TransferMemoryOwnerLedger
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				ledger.admit(transferMemoryOwnerSend, 1<<20)
				ledger.beginCleanup(transferMemoryOwnerSend, 1<<20)
				ledger.finish(transferMemoryOwnerSend, 1<<20, true)
			}
		})
	})
}
