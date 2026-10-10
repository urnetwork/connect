package connect

import (
	"context"
	"hash/crc64"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
)

func TestH1PhysicalProgressNilIsAllocationFree(t *testing.T) {
	wire := []byte("synthetic metadata-only trace test")
	if allocations := testing.AllocsPerRun(100, func() {
		p := newH1PhysicalProgress(nil, Id{}, nil)
		if p != nil {
			t.Fatal("disabled trace allocated a connection recorder")
		}
		p.queue(wire)
		p.beginWrite()
		p.endWrite(nil)
		p.beginWebSocketBatch()
		p.prepareWebSocketWrite(wire)
		p.finishWebSocketBatch()
		if event := p.event("h1_read", wire, true, nil); event.AtUnixNano != 0 || event.WireHash != 0 {
			t.Fatal("nil observer read a clock or hashed payload")
		}
	}); allocations != 0 {
		t.Fatalf("nil physical trace allocated %g objects", allocations)
	}
}

func h1TraceStage(events []TransferProgressEvent, stage string) []TransferProgressEvent {
	var result []TransferProgressEvent
	for _, event := range events {
		if event.Stage == stage {
			result = append(result, event)
		}
	}
	return result
}

func TestH1PhysicalProgressBoundedAndAbortedMetadata(t *testing.T) {
	observer, events := progressTraceTestObserver()
	p := newH1PhysicalProgress(observer, Id{}, nil)
	takeProgressTraceEvents(events)
	p.beginWebSocketBatch()
	wire := []byte("synthetic metadata-only bytes")
	wantHash := crc64.Checksum(wire, transferProgressChecksumTable())
	p.prepareWebSocketWrite(wire)
	// The producer can return/reuse its bytes before the delegated flush.
	// Metadata must not alias that storage or invent a successful flush.
	for index := range wire {
		wire[index] = 0
	}
	p.finishWebSocketBatch()
	aborted := takeProgressTraceEvents(events)
	if len(aborted) != 1 || aborted[0].Stage != "h1_write_aborted" || aborted[0].Success || aborted[0].WireHash != wantHash {
		t.Fatalf("aborted metadata lost ownership: %+v", aborted)
	}
	if p.count != 0 || p.pendingBytes != 0 || p.batching {
		t.Fatal("aborted batch retained pending metadata")
	}
	p.beginWebSocketBatch()
	for range platformWebSocketWriteBatchMaxMessages + 1 {
		p.prepareWebSocketWrite(nil)
	}
	if overflow := h1TraceStage(takeProgressTraceEvents(events), "h1_trace_overflow"); len(overflow) != 1 {
		t.Fatalf("bounded scratch overflow was not explicit: %+v", overflow)
	}
	p.finishWebSocketBatch()
	takeProgressTraceEvents(events)
	p.beginWebSocketBatch()
	p.prepareWebSocketWrite(make([]byte, webSocketWriteBatchMaxByteCount))
	if unsupported := h1TraceStage(takeProgressTraceEvents(events), "h1_trace_unsupported"); len(unsupported) != 1 {
		t.Fatalf("possible early WebSocket auto-flush was not explicit: %+v", unsupported)
	}
	p.finishWebSocketBatch()
}

// Real H1+ owners must correlate complete physical messages with the existing
// Transfer checksum without retaining any pooled bytes in the observer.
func TestH1PhysicalProgressWireReadWriteAndHeartbeat(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		observer, events := progressTraceTestObserver()
		fixture := newH1LivenessFixture(t, func(settings *PlatformTransportSettings) {
			settings.ProgressObserver = observer
		})
		initial := takeProgressTraceEvents(events)
		selected := h1TraceStage(initial, "h1_connected")
		if len(selected) != 1 || selected[0].Outcome != "h1plus" || selected[0].SequenceId == (Id{}) {
			t.Fatalf("selected H1 subtype was not observed: %+v", initial)
		}
		wire := h1PlusOwnedTestMessage(0x71, 64)
		wantHash := crc64.Checksum(wire, transferProgressChecksumTable())
		fixture.send <- wire
		synctest.Wait()
		written := takeProgressTraceEvents(events)
		begin, end := h1TraceStage(written, "h1_write_begin"), h1TraceStage(written, "h1_write_end")
		if len(begin) != 1 || len(end) != 1 || begin[0].WireHash != wantHash || end[0].WireHash != wantHash ||
			!end[0].Success || end[0].Outcome != "h1plus" || end[0].SequenceId != selected[0].SequenceId ||
			fixture.payloads.Load() != 1 {
			t.Fatalf("physical write join failed: %+v", written)
		}
		incoming := []byte("synthetic H1 incoming payload; no production identity")
		wantHash = crc64.Checksum(incoming, transferProgressChecksumTable())
		if err := fixture.peer.WriteMessage(websocket.BinaryMessage, incoming); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		readEvents := takeProgressTraceEvents(events)
		read, handoff := h1TraceStage(readEvents, "h1_read"), h1TraceStage(readEvents, "h1_receive_end")
		if len(read) != 1 || len(handoff) != 1 || read[0].WireHash != wantHash || !read[0].Success ||
			!handoff[0].Success || handoff[0].WireHash != wantHash {
			t.Fatalf("physical read/handoff join failed: %+v", readEvents)
		}
		if err := fixture.peer.WriteMessage(websocket.BinaryMessage, nil); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		heartbeats := h1TraceStage(takeProgressTraceEvents(events), "h1_read")
		if len(heartbeats) != 1 || heartbeats[0].ByteCount != 0 || !heartbeats[0].Success {
			t.Fatalf("zero-length heartbeat lost: %+v", heartbeats)
		}
	})
}

// Failed physical writes cannot be mistaken for accepted queue writes, and a
// reader deadline must be visible even when no complete frame ever arrives.
func TestH1PhysicalProgressTerminalIOEvidence(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, writeFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "read_deadline", true: "write_error"}[writeFailure], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				observer, events := progressTraceTestObserver()
				fixture := newH1LivenessFixture(t, func(settings *PlatformTransportSettings) {
					settings.ProgressObserver = observer
				})
				takeProgressTraceEvents(events)
				if writeFailure {
					fixture.client.failWrite.Store(true)
					fixture.send <- h1PlusOwnedTestMessage(0x31, 64)
				} else {
					time.Sleep(3 * time.Second)
				}
				synctest.Wait()
				fixture.assertWithdrawn(t)
				got := takeProgressTraceEvents(events)
				stage, errorKind := "h1_read_error", "io_timeout"
				if writeFailure {
					stage, errorKind = "h1_write_end", "error"
				}
				failures := h1TraceStage(got, stage)
				if len(failures) != 1 || failures[0].Success || failures[0].ErrorKind != errorKind {
					t.Fatalf("missing bounded terminal evidence: %+v", got)
				}
			})
		})
	}
}

// Exercise the actual WebSocket fallback and its ready-only batch wrapper,
// not a replacement writer. A successful trace end requires the peer read.
func TestH1PhysicalProgressWebSocketFallback(t *testing.T) {
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	platform := newTestingPlatformServer(t)
	observer, events := progressTraceTestObserver()
	settings := testingPlatformTransportSettings()
	settings.ProgressObserver = observer
	settings.EnableH1Plus = false
	ready := make(chan Route, 1)
	settings.SendRouteObserver = func(_ Transport, route Route, connected bool) {
		if connected {
			select {
			case ready <- route:
			default:
			}
		}
	}
	transport := testingPlatformTransport(t, ctx, platform.url, settings)
	if !testingWaitForActiveMode(transport, TransportModeH1, 15*time.Second) {
		t.Fatal("WebSocket fallback did not become ready")
	}
	var route Route
	select {
	case route = <-ready:
	case <-time.After(time.Second):
		t.Fatal("missing ready route")
	}
	selected := h1TraceStage(takeProgressTraceEvents(events), "h1_connected")
	if len(selected) != 1 || selected[0].Outcome != "websocket" {
		t.Fatalf("fallback subtype missing: %+v", selected)
	}
	wire := h1PlusOwnedTestMessage(0x69, 64)
	wantHash := crc64.Checksum(wire, transferProgressChecksumTable())
	route <- wire
	if !waitForCondition(time.Second, func() bool { return platform.dataMessages.Load() == 1 }) {
		t.Fatal("WebSocket payload was not read")
	}
	closeCtx, closeCancel := context.WithTimeout(ctx, time.Second)
	defer closeCancel()
	if err := transport.CloseAndWait(closeCtx); err != nil {
		t.Fatal(err)
	}
	writes := h1TraceStage(takeProgressTraceEvents(events), "h1_write_end")
	matched := 0
	for _, event := range writes {
		if event.WireHash == wantHash && event.Success && event.Outcome == "websocket" {
			matched++
		}
	}
	if matched != 1 {
		t.Fatalf("WebSocket physical completion missing: %+v", writes)
	}
}
