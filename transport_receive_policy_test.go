package connect

import (
	"context"
	"os"
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

func TestPlatformTransportReceiveQueueRefusalDoesNotWait(t *testing.T) {
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

// This inventory deliberately targets the carrier readers rather than every
// channel send in these large files. Any reintroduction of their former
// blocking select shapes fails even if a timing test happens to get scheduled
// after capacity becomes available.
func TestProductionCarrierReadersUseZeroWaitReceiveAdmission(t *testing.T) {
	checks := []struct {
		path              string
		required          map[string]int
		forbiddenSnippets []string
	}{
		{
			path: "transport.go",
			required: map[string]int{
				"self.offerReceive(":   2,
				"self.offerH1Control(": 4,
			},
			forbiddenSnippets: []string{
				"case receive <- message:",
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
