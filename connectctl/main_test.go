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
