package connect

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
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

func TestPlatformTransportH3DnsReceiveQueueRefusalDoesNotWait(t *testing.T) {
	stats := &PlatformTransportReceiveStats{}
	transport := &PlatformTransport{receiveStats: stats}
	receive := make(chan []byte, 1)
	queued := MessagePoolGet(1)
	receive <- queued
	defer func() { MessagePoolReturn(<-receive) }()

	var open bool
	var delivered bool
	requirePromptTransportOffer(t, func() {
		open, delivered = transport.offerReceive(
			make(chan struct{}),
			TransportModeH3Dns,
			receive,
			MessagePoolGet(137),
		)
	}, "platform H3 DNS receive")
	if !open || delivered {
		t.Fatalf("full receive offer = (open=%t, delivered=%t), want (true, false)", open, delivered)
	}
	snapshot := stats.Snapshot()
	if snapshot.H3Dns.QueueDropMessageCount != 1 ||
		snapshot.H3Dns.QueueDropByteCount != 137 {
		t.Fatalf("H3 DNS queue-drop stats = %+v, want one message / 137 bytes", snapshot.H3Dns)
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

func TestPlatformTransportControlRefusalTerminatesGenerationWithoutWait(t *testing.T) {
	stats := &PlatformTransportReceiveStats{}
	transport := &PlatformTransport{receiveStats: stats}
	controlSend := make(chan []byte, 1)
	queued := MessagePoolGet(1)
	controlSend <- queued
	defer func() { MessagePoolReturn(<-controlSend) }()

	accepted := true
	requirePromptTransportOffer(t, func() {
		accepted = transport.offerH1Control(
			make(chan struct{}),
			controlSend,
			MessagePoolGet(16),
		)
	}, "platform H1 control")
	if accepted {
		t.Fatal("full H1 control queue was accepted")
	}
	snapshot := stats.Snapshot()
	if snapshot.H1ControlRefusalCount != 1 || snapshot.H1ControlRefusalBytes != 16 {
		t.Fatalf("H1 control-refusal stats = %+v, want one message / 16 bytes", snapshot)
	}
}

func TestP2pReceiveRouteRefusalDoesNotWait(t *testing.T) {
	for _, testCase := range []struct {
		name string
		fast bool
	}{
		{name: "legacy", fast: false},
		{name: "fast", fast: true},
	} {
		ctx, cancel := context.WithCancel(context.Background())
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
			open = transport.offerReceive(MessagePoolGet(211), testCase.fast, 3, false, true)
		}, "P2P "+testCase.name+" receive")
		if !open {
			t.Errorf("%s full P2P route closed its live generation", testCase.name)
		}
		snapshot := stats.Snapshot()
		if testCase.fast {
			if snapshot.FastReceiveQueueDropCount != 1 ||
				snapshot.FastReceiveQueueDropByteCount != 211 ||
				snapshot.FastDropCount != 1 {
				t.Errorf("%s fast queue-drop stats = %+v", testCase.name, snapshot)
			}
		} else if snapshot.LegacyReceiveQueueDropCount != 1 ||
			snapshot.LegacyReceiveQueueDropByteCount != 211 {
			t.Errorf("%s legacy queue-drop stats = %+v", testCase.name, snapshot)
		}
		cancel()
		queued = <-pendingReceive
		transport.releasePendingReceive(len(queued))
		MessagePoolReturn(queued)
	}
}

// This inventory deliberately targets the carrier readers rather than relying
// only on timing tests. Platform H1 owns exactly one cancellable backpressure
// send inside offerReceive; H3/DNS and both P2P readers retain zero-wait
// admission. A direct carrier-reader send or a timer silently changes that
// ownership contract and must fail this audit.
func TestProductionCarrierReadersUseModeSpecificReceiveAdmission(t *testing.T) {
	checks := []struct {
		path              string
		required          map[string]int
		forbiddenSnippets []string
	}{
		{
			path: "transport.go",
			required: map[string]int{
				"self.offerReceive(":       2,
				"self.offerH1Control(":     4,
				"case receive <- message:": 1,
			},
			forbiddenSnippets: []string{
				"case controlSend <- message:",
				"resetOrCreateTimer(&receiveTimer",
			},
		},
		{
			path: "transport_p2p.go",
			required: map[string]int{
				"offerReceive(":                        4, // declaration plus prefetch, legacy, and fast callers
				"case self.pendingReceive <- message:": 1,
				"case self.receive <- message:":        1, // bounded queue's sole sender-owned forwarding worker
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
				"if mode == TransportModeH1",
				"case <-done:",
				"case receive <- message:",
				"recordQueueDrop(mode",
			} {
				if !strings.Contains(offerSource, required) {
					t.Fatalf("platform receive admission is missing %q", required)
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
			for name, readerSource := range map[string]string{
				"legacy": source[runStart:runFastStart],
				"fast":   source[runFastStart:runFastEnd],
			} {
				if strings.Contains(readerSource, "self.receive <-") ||
					strings.Contains(readerSource, "self.pendingReceive <-") {
					t.Fatalf("P2P %s carrier reader contains a direct blocking handoff", name)
				}
			}
		}
	}
}
