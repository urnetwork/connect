package main

import (
	"fmt"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

func TestSnapshotSinkReceiveDoesNotRetainBorrowedFrames(t *testing.T) {
	frame := &protocol.Frame{
		MessageType:  protocol.MessageType_TestSimpleMessage,
		MessageBytes: []byte("original"),
	}
	frames := []*protocol.Frame{frame}
	wantSummary := fmt.Sprint(frames)

	snapshot := snapshotSinkReceive(
		connect.SourceId(connect.NewId()),
		frames,
		connect.Peer{ProvideMode: protocol.ProvideMode_Network},
	)

	frame.MessageType = protocol.MessageType_IpIpPacketFromProvider
	frame.MessageBytes = []byte("reused")
	frames[0] = nil

	if snapshot.frameSummary != wantSummary {
		t.Fatalf("frame summary changed after borrowed frame reuse: got %q want %q", snapshot.frameSummary, wantSummary)
	}
}

// A full printer queue drops immediately instead of blocking the shared
// client receive pump.
func TestEnqueueSinkReceiveDropsWhenFull(t *testing.T) {
	receives := make(chan *sinkReceive, 1)
	first := &sinkReceive{frameSummary: "first"}
	second := &sinkReceive{frameSummary: "second"}

	if !enqueueSinkReceive(receives, first) {
		t.Fatal("first receive was not admitted")
	}
	if enqueueSinkReceive(receives, second) {
		t.Fatal("second receive was admitted to a full queue")
	}
	if got := <-receives; got != first {
		t.Fatalf("queued receive = %p, want %p", got, first)
	}
}

// The cli provider derives its family-pinned urls from --connect_url by the
// sdk's rule: suffix the service label, keep scheme, port and path, and give
// up on anything with no label to suffix.
func TestFamilyConnectUrl(t *testing.T) {
	cases := []struct {
		connectUrl string
		ipVersion  int
		want       string
	}{
		{"wss://connect.bringyour.com/", 4, "wss://connect-v4.bringyour.com/"},
		{"wss://connect.bringyour.com/", 6, "wss://connect-v6.bringyour.com/"},
		{"wss://g2-connect.bringyour.com/secret", 4, "wss://g2-connect-v4.bringyour.com/secret"},
		{"wss://connect.ur.network:8443/", 6, "wss://connect-v6.ur.network:8443/"},
		{"ws://127.0.0.1:8080/", 4, ""},
		{"wss://[::1]:8080/", 6, ""},
		{"wss://localhost/", 4, ""},
		{"wss://connect-v4.bringyour.com/", 6, ""},
		{"wss://connect.bringyour.com/", 5, ""},
		{"", 4, ""},
		{"not a url", 4, ""},
	}
	for _, c := range cases {
		if got := familyConnectUrl(c.connectUrl, c.ipVersion); got != c.want {
			t.Errorf("familyConnectUrl(%q, %d) = %q, want %q", c.connectUrl, c.ipVersion, got, c.want)
		}
	}
}
