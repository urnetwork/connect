package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"testing/synctest"
	"time"
)

// The physical pipe enforces every installed deadline. Unlike a pacing fake
// that ignores SetWriteDeadline, it reproduces a healthy write losing its
// remaining budget while earlier messages drain the compact legacy queue.
func TestP2pLegacyWriteTimeoutStartsAtPhysicalWrite(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		bulkCount    int
		bulkDelay    time.Duration
		tailSize     int
		tailDelay    time.Duration
		priorityTail bool
		queueOff     bool
	}{
		{name: "raw-queue", bulkCount: 3, bulkDelay: 6 * time.Second},
		{name: "compact-queue", bulkCount: 20, bulkDelay: time.Second},
		{name: "ordered-small-tail", bulkCount: 2, bulkDelay: 6 * time.Second, tailSize: 64, tailDelay: 6 * time.Second},
		{name: "ordered-large-tail", bulkCount: 2, bulkDelay: 6 * time.Second, tailSize: 16 * 1024, tailDelay: 6 * time.Second},
		{name: "priority-tail", bulkCount: 1, bulkDelay: 12 * time.Second, tailSize: 64, tailDelay: 4 * time.Second, priorityTail: true},
		{name: "no-queue-control", bulkCount: 3, bulkDelay: 6 * time.Second, queueOff: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			assertMessagePoolOwnership(t)
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				left, right := net.Pipe()
				budget := NewTransferMemoryBudget(kib(512))
				settings := DefaultP2pTransportSettings()
				settings.DataPlaneMode = P2pDataPlaneModeLegacyOnly
				if testCase.queueOff {
					settings.LegacySendQueueByteCount = 0
				}
				var writes []TransferProgressEvent
				settings.ProgressObserver = func(event TransferProgressEvent) {
					if event.Stage == "p2p_write_end" {
						writes = append(writes, event)
					}
				}
				transport, route := NewP2pSendTransport(ctx, cancel, &p2pLegacyQueueBudgetTestConn{Conn: left, budget: budget}, NewId(), settings)
				defer func() {
					cancel()
					left.Close()
					right.Close()
					if err := transport.(P2pRouteLifecycle).CloseAndWait(context.Background()); err != nil {
						t.Error(err)
					}
					if stats := budget.Stats(); stats.ReservedByteCount != stats.ReleasedByteCount {
						t.Errorf("unbalanced physical/queued ownership: %+v", stats)
					}
				}()

				started := time.Now()
				for index := range testCase.bulkCount {
					route <- legacyQueueTestPacket(uint32(index), 1200)
				}
				// A priority tail may overtake queued bulk, but not the physical
				// write already borrowing its first root.
				synctest.Wait()
				count := testCase.bulkCount
				if testCase.tailSize != 0 {
					packet := legacyQueueTestPacket(uint32(count), testCase.tailSize)
					if testCase.priorityTail {
						messagePoolMarkSmallUnordered(packet)
					}
					route <- packet
					count++
				}
				synctest.Wait()
				if time.Now() != started {
					t.Fatal("fixture advanced time before the backlog was admitted")
				}
				for index := range count {
					size, delay := 1200, testCase.bulkDelay
					if index == testCase.bulkCount {
						size, delay = testCase.tailSize, testCase.tailDelay
					}
					// Sleep advances the synctest clock, not wall time. Each read
					// releases exactly one real physical Write below its own bound.
					time.Sleep(delay)
					if err := right.SetReadDeadline(time.Now().Add(time.Millisecond)); err != nil {
						t.Fatal(err)
					}
					packet := make([]byte, size)
					if _, err := io.ReadFull(right, packet); err != nil {
						synctest.Wait()
						t.Fatalf("healthy packet %d/%d failed after %s of queued service: %v; physical writes=%d last=%+v", index, count, time.Since(started), err, len(writes), writes[len(writes)-1])
					}
					if binary.BigEndian.Uint32(packet) != uint32(index) ||
						!bytes.Equal(packet[4:], bytes.Repeat([]byte{byte(index)}, size-4)) {
						t.Fatalf("packet %d lost FIFO identity or payload", index)
					}
					synctest.Wait()
					if len(writes) != index+1 || !writes[index].Success || writes[index].ElapsedNanos != int64(delay) {
						t.Fatalf("packet %d physical writes=%d last=%+v, want successful %s", index, len(writes), writes[len(writes)-1], delay)
					}
				}
				if time.Since(started) <= settings.WriteTimeout || ctx.Err() != nil {
					t.Fatalf("healthy backlog did not survive beyond one physical timeout: elapsed=%s ctx=%v", time.Since(started), ctx.Err())
				}
			})
		})
	}
}

// Giving each write its own budget must not turn an actually stuck SCTP write
// into an unbounded wait. The earlier successful write consumes twelve seconds
// of queue age; the next physical call still fails after exactly fifteen more.
func TestP2pLegacyPhysicalWriteTimeoutStillBoundsBlockedCarrier(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		left, right := net.Pipe()
		defer left.Close()
		defer right.Close()
		budget := NewTransferMemoryBudget(kib(512))
		settings := DefaultP2pTransportSettings()
		settings.DataPlaneMode = P2pDataPlaneModeLegacyOnly
		var writes []TransferProgressEvent
		settings.ProgressObserver = func(event TransferProgressEvent) {
			if event.Stage == "p2p_write_end" {
				writes = append(writes, event)
			}
		}
		transport, route := NewP2pSendTransport(ctx, cancel, &p2pLegacyQueueBudgetTestConn{Conn: left, budget: budget}, NewId(), settings)
		defer func() {
			left.Close()
			if err := transport.(P2pRouteLifecycle).CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		for index := range 20 {
			route <- legacyQueueTestPacket(uint32(index), 1200)
		}
		synctest.Wait()
		time.Sleep(12 * time.Second)
		if _, err := io.ReadFull(right, make([]byte, 1200)); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		blockedAt := time.Now()
		time.Sleep(settings.WriteTimeout - time.Nanosecond)
		synctest.Wait()
		if ctx.Err() != nil {
			t.Fatalf("queued age shortened the physical write budget: %v after %s", ctx.Err(), time.Since(blockedAt))
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		select {
		case <-transport.(P2pRouteLifecycle).Done():
		default:
			t.Fatal("blocked physical carrier outlived its unchanged WriteTimeout")
		}
		if len(writes) != 2 || !writes[0].Success || writes[1].Success ||
			writes[1].ElapsedNanos != int64(settings.WriteTimeout) || writes[1].ErrorKind != "io_timeout" {
			t.Fatalf("physical timeout disposition=%+v", writes)
		}
		if stats := budget.Stats(); stats.ReservedByteCount != stats.ReleasedByteCount {
			t.Fatalf("timeout did not return every queued root and owner: %+v", stats)
		}
	})
}

// Transfer owns its unchanged ACK lifetime even while a healthy physical
// carrier is still serving older queued messages. Resetting a physical write
// budget must neither renew the original Pack nor retain its resend owner.
func TestP2pLegacyQueuedWriteCannotExtendTransferAckLifetime(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, originalTransport, _ := newAckRetirementFixture(t, TransportTypeP2p, 2, NewNoopLogger())
		fixture.sender.RouteManager().RemoveTransport(originalTransport)
		ctx, cancel := context.WithCancel(fixture.ctx)
		left, right := net.Pipe()
		budget := NewTransferMemoryBudget(kib(512))
		settings := DefaultP2pTransportSettings()
		settings.DataPlaneMode = P2pDataPlaneModeLegacyOnly
		transport, route := NewP2pSendTransportForPeer(ctx, cancel, &p2pLegacyQueueBudgetTestConn{Conn: left, budget: budget}, fixture.receiver.ClientId(), NewId(), settings)
		defer func() {
			cancel()
			left.Close()
			right.Close()
			fixture.sender.RouteManager().RemoveTransport(transport)
			if err := transport.(P2pRouteLifecycle).CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			if stats := budget.Stats(); stats.ReservedByteCount != stats.ReleasedByteCount {
				t.Errorf("cancellation retained physical queue ownership: %+v", stats)
			}
		}()
		// Forty bounded opaque carrier messages precede the real Transfer Pack.
		// The pipe services each in one second without ever exceeding 15s per
		// physical write. The target cannot reach this peer before its 30s ACK
		// lifetime, so no fixture ACK or callback can manufacture progress.
		for index := range 40 {
			route <- legacyQueueTestPacket(uint32(index), 1200)
		}
		synctest.Wait()
		fixture.sender.RouteManager().UpdateTransport(transport, []Route{route})
		type result struct {
			at  time.Time
			err error
		}
		terminal := make(chan result, 2)
		started := time.Now()
		frame := budgetTestFrame(1100)
		admitted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(), func(err error) {
			terminal <- result{at: time.Now(), err: err}
		}, time.Second)
		if !admitted || err != nil {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatalf("Transfer Pack admission=%t err=%v", admitted, err)
		}
		sequence := fixture.sequence()
		if sequence.sendBufferSettings.AckTimeout != 30*time.Second {
			t.Fatal("fixture changed the production multi-client ACK lifetime")
		}
		for index := range 30 {
			time.Sleep(time.Second)
			if err := right.SetReadDeadline(time.Now().Add(time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			packet := make([]byte, 1200)
			if _, err := io.ReadFull(right, packet); err != nil {
				t.Fatalf("healthy carrier stopped before ACK lifetime at packet %d: %v", index, err)
			}
			if binary.BigEndian.Uint32(packet) != uint32(index) {
				t.Fatal("Transfer lifetime test lost the older FIFO backlog")
			}
			synctest.Wait()
			if index < 29 && len(terminal) != 0 {
				t.Fatalf("Transfer terminated before 30s: %+v", <-terminal)
			}
		}
		if len(terminal) != 1 {
			t.Fatalf("terminal callback count=%d, want exactly one at 30s", len(terminal))
		}
		got := <-terminal
		if got.err == nil || got.at != started.Add(30*time.Second) || ctx.Err() != nil ||
			sequence.resendQueue.Len() != 0 || len(sequence.ackLifetimes.items) != 0 {
			t.Fatalf("ACK lifetime changed: terminal=%+v elapsed=%s carrier=%v resend=%d lifetimes=%d", got, got.at.Sub(started), ctx.Err(), sequence.resendQueue.Len(), len(sequence.ackLifetimes.items))
		}
	})
}
