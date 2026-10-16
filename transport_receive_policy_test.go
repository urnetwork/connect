package connect

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func requirePromptTransportOffer(
	t *testing.T,
	offer func(),
	description string,
) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		offer()
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("%s waited for receive queue capacity", description)
	}
}

func platformReceiveModeSnapshot(
	snapshot PlatformTransportReceiveStatsSnapshot,
	mode TransportMode,
) PlatformTransportReceiveModeStatsSnapshot {
	switch mode {
	case TransportModeH1:
		return snapshot.H1
	case TransportModeH3:
		return snapshot.H3
	case TransportModeH3Dns:
		return snapshot.H3Dns
	case TransportModeH3DnsPump:
		return snapshot.H3DnsPump
	default:
		return PlatformTransportReceiveModeStatsSnapshot{}
	}
}

func TestPlatformH3ReceiveLaneSplitKeepsOnePayloadQueue(t *testing.T) {
	const bufferSize = 17
	for _, useDatagrams := range []bool{false, true} {
		reliable, unreliable := platformH3ReceiveRouteBufferSizes(bufferSize, useDatagrams)
		if reliable+unreliable != bufferSize {
			t.Fatalf(
				"use DATAGRAM=%t receive slots=%d+%d want total %d",
				useDatagrams,
				reliable,
				unreliable,
				bufferSize,
			)
		}
		if useDatagrams && reliable != 0 {
			t.Fatalf("hybrid reliable stream queue=%d, want unbuffered", reliable)
		}
		if !useDatagrams && unreliable != 0 {
			t.Fatalf("stream-only DATAGRAM queue=%d, want absent", unreliable)
		}
	}
}

func TestPlatformTransportH3DatagramLanesRefuseWithoutWaiting(t *testing.T) {
	for _, mode := range []TransportMode{
		TransportModeH3,
		TransportModeH3Dns,
		TransportModeH3DnsPump,
	} {
		stats := &PlatformTransportReceiveStats{}
		transport := &PlatformTransport{receiveStats: stats}
		receive := make(chan []byte, 1)
		receive <- MessagePoolGet(1)

		var open bool
		var delivered bool
		requirePromptTransportOffer(t, func() {
			open, delivered = transport.offerReceive(
				make(chan struct{}),
				mode,
				CarrierReliabilityUnreliable,
				receive,
				MessagePoolGet(137),
			)
		}, "platform "+string(mode)+" DATAGRAM receive")
		if !open || delivered {
			MessagePoolReturn(<-receive)
			t.Fatalf("%s full receive offer = (open=%t, delivered=%t), want (true, false)", mode, open, delivered)
		}
		modeStats := platformReceiveModeSnapshot(stats.Snapshot(), mode)
		if modeStats.QueueDropMessageCount != 1 ||
			modeStats.QueueDropByteCount != 137 {
			MessagePoolReturn(<-receive)
			t.Fatalf("%s DATAGRAM queue-drop stats = %+v", mode, modeStats)
		}
		MessagePoolReturn(<-receive)
	}
}

// A full route below a reliable H1 socket used to drop the just-read message.
// The sender could not know which ordered message vanished, so every later
// message filled the receiver's reorder budget while Transfer retransmits
// queued behind the same new traffic. Keep the channel bounded, but make its
// full edge ordinary TCP backpressure rather than application-level loss.
func TestPlatformTransportH1ReceiveQueueBackpressuresWithoutDropping(t *testing.T) {
	stats := &PlatformTransportReceiveStats{}
	transport := &PlatformTransport{receiveStats: stats}
	receive := make(chan []byte, 1)
	queued := MessagePoolGet(1)
	receive <- queued

	type offerResult struct {
		open      bool
		delivered bool
	}
	result := make(chan offerResult, 1)
	started := make(chan struct{})
	pending := MessagePoolGet(137)
	if pooled, _ := MessagePoolCheck(pending); !pooled {
		t.Fatal("pending H1 message is not pool-owned before offer")
	}
	go func() {
		close(started)
		open, delivered := transport.offerReceive(
			make(chan struct{}),
			TransportModeH1,
			CarrierReliabilityReliable,
			receive,
			pending,
		)
		result <- offerResult{open: open, delivered: delivered}
	}()
	<-started
	deadline := time.Now().Add(time.Second)
	for stats.Snapshot().H1.QueueBackpressureMessageCount == 0 &&
		time.Now().Before(deadline) {
		runtime.Gosched()
	}
	snapshot := stats.Snapshot()
	if snapshot.H1.QueueBackpressureMessageCount != 1 ||
		snapshot.H1.QueueBackpressureByteCount != 137 ||
		snapshot.H1.QueueDropMessageCount != 0 {
		t.Fatalf("full H1 route stats = %+v", snapshot.H1)
	}
	select {
	case premature := <-result:
		t.Fatalf("full H1 route returned instead of applying backpressure: %+v", premature)
	default:
	}

	MessagePoolReturn(<-receive)
	select {
	case got := <-result:
		if !got.open || !got.delivered {
			t.Fatalf("H1 route result = %+v, want open delivered", got)
		}
	case <-time.After(time.Second):
		t.Fatal("H1 route did not resume after bounded channel space opened")
	}
	if got := <-receive; &got[0] != &pending[0] {
		t.Fatal("H1 route changed message ownership while backpressured")
	} else {
		if pooled, _ := MessagePoolCheck(got); !pooled {
			t.Fatal("H1 route returned pool ownership before delivery")
		}
		MessagePoolReturn(got)
		if pooled, _ := MessagePoolCheck(got); pooled {
			t.Fatal("receiver did not return delivered H1 pool ownership")
		}
	}
}

func TestPlatformTransportH1ReceiveBackpressureCancellationReturns(t *testing.T) {
	stats := &PlatformTransportReceiveStats{}
	transport := &PlatformTransport{receiveStats: stats}
	receive := make(chan []byte, 1)
	queued := MessagePoolGet(1)
	receive <- queued
	defer func() { MessagePoolReturn(<-receive) }()
	done := make(chan struct{})
	result := make(chan bool, 1)
	pending := MessagePoolGet(137)
	go func() {
		open, delivered := transport.offerReceive(
			done,
			TransportModeH1,
			CarrierReliabilityReliable,
			receive,
			pending,
		)
		result <- open || delivered
	}()
	deadline := time.Now().Add(time.Second)
	for stats.Snapshot().H1.QueueBackpressureMessageCount == 0 &&
		time.Now().Before(deadline) {
		runtime.Gosched()
	}
	close(done)
	select {
	case accepted := <-result:
		if accepted {
			t.Fatal("canceled H1 route transferred ownership")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled H1 route remained blocked")
	}
	snapshot := stats.Snapshot().H1
	if snapshot.QueueBackpressureMessageCount != 1 ||
		snapshot.QueueDropMessageCount != 0 {
		t.Fatalf("canceled H1 route stats = %+v", snapshot)
	}
	if pooled, _ := MessagePoolCheck(pending); pooled {
		t.Fatal("canceled H1 route retained pooled message ownership")
	}
}

func TestPlatformTransportH3StreamLanesBackpressureWithoutDropping(t *testing.T) {
	for _, mode := range []TransportMode{
		TransportModeH3,
		TransportModeH3Dns,
		TransportModeH3DnsPump,
	} {
		stats := &PlatformTransportReceiveStats{}
		transport := &PlatformTransport{receiveStats: stats}
		receive := make(chan []byte, 1)
		receive <- MessagePoolGet(1)
		pending := MessagePoolGet(149)
		witness := MessagePoolShareReadOnly(pending)
		type offerResult struct {
			open      bool
			delivered bool
		}
		result := make(chan offerResult, 1)
		go func() {
			open, delivered := transport.offerReceive(
				make(chan struct{}),
				mode,
				CarrierReliabilityReliable,
				receive,
				pending,
			)
			result <- offerResult{open: open, delivered: delivered}
		}()
		deadline := time.Now().Add(time.Second)
		for platformReceiveModeSnapshot(stats.Snapshot(), mode).
			QueueBackpressureMessageCount == 0 && time.Now().Before(deadline) {
			runtime.Gosched()
		}
		select {
		case premature := <-result:
			t.Fatalf("full %s stream returned early: %+v", mode, premature)
		default:
		}
		MessagePoolReturn(<-receive)
		select {
		case got := <-result:
			if !got.open || !got.delivered {
				t.Fatalf("%s stream result=%+v, want delivered", mode, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s stream did not resume with route capacity", mode)
		}
		MessagePoolReturn(<-receive)
		if !MessagePoolReturn(witness) {
			t.Fatalf("%s stream delivery changed pooled ownership", mode)
		}
		modeStats := platformReceiveModeSnapshot(stats.Snapshot(), mode)
		if modeStats.QueueBackpressureMessageCount != 1 ||
			modeStats.QueueDropMessageCount != 0 {
			t.Fatalf("%s stream stats=%+v", mode, modeStats)
		}
	}
}

// The release gate pins this name. A control echo waiting on a full queue is
// refused when its generation ends: the wait returns at that moment, gives
// back the echo's pooled ownership and counts no refusal. Before that the echo
// waits; the old reader refused at once and closed a live generation. Room on
// a live generation is TestPlatformTransportH1ControlQueueBackpressuresUntilRoom.
func TestPlatformTransportControlRefusalTerminatesGenerationWithoutWait(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		stats := &PlatformTransportReceiveStats{}
		transport := &PlatformTransport{receiveStats: stats}
		controlSend := make(chan []byte, 1)
		controlSend <- MessagePoolGet(1)
		defer func() { MessagePoolReturn(<-controlSend) }()

		done := make(chan struct{})
		pendingMessage := MessagePoolGet(16)
		witness := MessagePoolShareReadOnly(pendingMessage)
		result := make(chan bool, 1)
		go func() {
			result <- transport.offerH1Control(done, controlSend, pendingMessage)
		}()
		synctest.Wait()
		select {
		case accepted := <-result:
			t.Fatalf("a live generation returned %t from a full control queue instead of waiting", accepted)
		default:
		}

		close(done)
		synctest.Wait()
		select {
		case accepted := <-result:
			if accepted {
				t.Fatal("an ended H1 generation accepted a control echo")
			}
		default:
			// release the stuck offer so the bubble can exit, then fail
			MessagePoolReturn(<-controlSend)
			<-result
			t.Fatal("the control echo kept waiting after its generation ended")
		}
		if !MessagePoolReturn(witness) {
			t.Fatal("refused H1 control echo retained pooled ownership")
		}
		snapshot := stats.Snapshot()
		if snapshot.H1ControlRefusalCount != 0 || snapshot.H1ControlRefusalBytes != 0 {
			t.Fatalf("H1 control-refusal stats = %+v, want zero", snapshot)
		}
		if snapshot.H1ControlBackpressureMessageCount != 1 || snapshot.H1ControlBackpressureByteCount != 16 {
			t.Fatalf("H1 control backpressure = %d messages / %d bytes, want the one 16-byte wait",
				snapshot.H1ControlBackpressureMessageCount, snapshot.H1ControlBackpressureByteCount)
		}
	})
}

// During a speed test the reader echoes every frame through the H1 control
// queue, so a full queue is an uplink slower than the downlink, not a dead
// writer. The reader keeps its one already-read echo, unchanged, until the
// writer makes room, and then hands that same echo on.
func TestPlatformTransportH1ControlQueueBackpressuresUntilRoom(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		transport := &PlatformTransport{receiveStats: &PlatformTransportReceiveStats{}}
		controlSend := make(chan []byte, 1)
		controlSend <- MessagePoolGet(1)
		pendingMessage := MessagePoolGet(1024)
		witness := MessagePoolShareReadOnly(pendingMessage)
		result := make(chan bool, 1)
		go func() {
			result <- transport.offerH1Control(make(chan struct{}), controlSend, pendingMessage)
		}()
		synctest.Wait()
		select {
		case accepted := <-result:
			t.Fatalf("full H1 control queue returned %t instead of waiting", accepted)
		default:
		}

		MessagePoolReturn(<-controlSend)
		if !<-result {
			t.Fatal("the echo was refused after the writer made room")
		}
		deliveredMessage := <-controlSend
		if &deliveredMessage[0] != &pendingMessage[0] {
			t.Fatal("the waiting echo changed while it was held")
		}
		MessagePoolReturn(deliveredMessage)
		if !MessagePoolReturn(witness) {
			t.Fatal("the delivered echo's pooled ownership was not handed on")
		}
		snapshot := transport.ReceiveStats()
		if snapshot.H1ControlRefusalCount != 0 {
			t.Fatalf("H1 control refusals = %d, want 0", snapshot.H1ControlRefusalCount)
		}
		if snapshot.H1ControlBackpressureMessageCount != 1 || snapshot.H1ControlBackpressureByteCount != 1024 {
			t.Fatalf("H1 control backpressure = %d messages / %d bytes, want the one 1024-byte wait",
				snapshot.H1ControlBackpressureMessageCount, snapshot.H1ControlBackpressureByteCount)
		}
	})
}

func TestP2pSctpReceiveBackpressuresAndCancelsWithOwnership(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	receive := make(chan []byte, 1)
	receive <- MessagePoolGet(1)
	defer func() { MessagePoolReturn(<-receive) }()
	transport := &P2pReceiveTransport{
		ctx:     ctx,
		receive: receive,
		settings: &P2pTransportSettings{
			DataPlaneStats: &P2pDataPlaneStats{},
		},
	}
	waiting := make(chan struct{})
	transport.beforeReliableReceiveWaitForTest = func() { close(waiting) }
	message := MessagePoolGet(211)
	witness := MessagePoolShareReadOnly(message)
	done := make(chan struct{})
	open := true
	go func() {
		defer close(done)
		open = transport.offerReceive(message, false, 0, false, true)
	}()
	<-waiting
	select {
	case <-done:
		t.Fatal("reliable SCTP receive returned while its route was full")
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reliable SCTP receive ignored cancellation")
	}
	if open {
		t.Fatal("canceled SCTP receive reported an open generation")
	}
	if !MessagePoolReturn(witness) {
		t.Fatal("canceled SCTP receive retained pooled bytes")
	}
}

func TestP2pFastReceiveRefusesFullQueueWithoutWaiting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stats := &P2pDataPlaneStats{}
	pendingReceive := make(chan []byte, 1)
	queued := MessagePoolGet(1)
	pendingReceive <- queued
	transport := &P2pReceiveTransport{
		ctx:                        ctx,
		pendingReceive:             pendingReceive,
		pendingReceiveMessageLimit: 1,
		pendingReceiveByteLimit:    1024,
		settings: &P2pTransportSettings{
			DataPlaneStats: stats,
		},
	}
	transport.pendingReceiveMessageCount.Store(1)
	transport.pendingReceiveByteCount.Store(int64(len(queued)))

	open := false
	requirePromptTransportOffer(t, func() {
		open = transport.offerReceive(MessagePoolGet(211), true, 3, false, true)
	}, "P2P native datagram receive")
	if !open {
		t.Fatal("full native P2P queue closed its live generation")
	}
	snapshot := stats.Snapshot()
	if snapshot.FastReceiveQueueDropCount != 1 ||
		snapshot.FastReceiveQueueDropByteCount != 211 ||
		snapshot.FastDropCount != 1 {
		t.Fatalf("fast queue-drop stats = %+v", snapshot)
	}
	queued = <-pendingReceive
	transport.releasePendingReceive(len(queued))
	MessagePoolReturn(queued)
}

// This inventory deliberately targets the carrier readers rather than relying
// only on timing tests. Reliable stream/SCTP lanes and the H1 control echo each
// own one cancellation-bounded handoff; H3 DATAGRAM and native P2P retain
// bounded zero-wait admission.
func TestProductionCarrierReadersUseModeSpecificReceiveAdmission(t *testing.T) {
	checks := []struct {
		path              string
		required          map[string]int
		forbiddenSnippets []string
	}{
		{
			path: "transport.go",
			required: map[string]int{
				"self.offerReceive(":           2,
				"self.offerH1Control(":         4,
				"case receive <- message:":     1,
				"case controlSend <- message:": 1, // the bounded wait in offerH1Control
			},
			forbiddenSnippets: []string{
				"resetOrCreateTimer(&receiveTimer",
			},
		},
		{
			path: "transport_p2p.go",
			required: map[string]int{
				"offerReceive(":                           4, // declaration plus prefetch, legacy, and fast callers
				"case self.pendingReceive <- message:":    1,
				"case self.receive <- message:":           2, // reliable SCTP ready path plus bounded wait
				"case self.unreliableReceive <- message:": 1,
			},
			forbiddenSnippets: []string{
				"case self.receive <- transferFrameBytes:",
				"case self.receive <- received.message:",
			},
		},
	}
	for _, check := range checks {
		sourceBytes, err := os.ReadFile(check.path)
		if err != nil {
			t.Fatalf("read %s: %v", check.path, err)
		}
		source := string(sourceBytes)
		for token, want := range check.required {
			if got := strings.Count(source, token); got != want {
				t.Fatalf("%s contains %q %d time(s), want %d; audit the new receive boundary", check.path, token, got, want)
			}
		}
		for _, snippet := range check.forbiddenSnippets {
			if strings.Contains(source, snippet) {
				t.Fatalf("%s reintroduced blocking carrier receive handoff %q", check.path, snippet)
			}
		}
		if check.path == "transport.go" {
			offerStart := strings.Index(source, "func (self *PlatformTransport) offerReceive(")
			offerEnd := strings.Index(source, "func (self *PlatformTransport) offerH1Control(")
			if offerStart < 0 || offerEnd <= offerStart {
				t.Fatal("could not isolate platform receive-admission source boundary")
			}
			offerSource := source[offerStart:offerEnd]
			for _, required := range []string{
				"if reliability == CarrierReliabilityReliable",
				"case <-done:",
				"case receive <- message:",
				"recordQueueDrop(mode",
			} {
				if !strings.Contains(offerSource, required) {
					t.Fatalf("platform receive admission is missing %q", required)
				}
			}
			controlLength := strings.Index(source[offerEnd:], "\nfunc ")
			if controlLength < 0 {
				t.Fatal("could not isolate H1 control-admission source boundary")
			}
			controlSource := source[offerEnd : offerEnd+controlLength]
			for _, required := range []string{
				"recordH1ControlBackpressure(len(message))",
				"case <-done:",
				"case controlSend <- message:",
			} {
				if !strings.Contains(controlSource, required) {
					t.Fatalf("H1 control admission is missing %q", required)
				}
			}
		}
		if check.path == "transport_p2p.go" {
			runStart := strings.Index(source, "func (self *P2pReceiveTransport) run()")
			runFastStart := strings.Index(source, "func (self *P2pReceiveTransport) runFast(")
			runFastEnd := strings.Index(source, "func (self *P2pReceiveTransport) TransportId()")
			if runStart < 0 || runFastStart <= runStart || runFastEnd <= runFastStart {
				t.Fatal("could not isolate P2P carrier reader source boundaries")
			}
			fastSource := source[runFastStart:runFastEnd]
			if strings.Contains(fastSource, "self.receive <-") ||
				strings.Contains(fastSource, "self.pendingReceive <-") {
				t.Fatal("native P2P reader contains a direct blocking route handoff")
			}
			if !strings.Contains(fastSource, "true,") {
				t.Fatal("native P2P reader did not select fast zero-wait admission")
			}
		}
	}
}
