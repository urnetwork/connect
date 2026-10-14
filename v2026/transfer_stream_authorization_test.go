package connect

import (
	"github.com/urnetwork/connect/v2026/protocol"
	"testing"
	"time"
)

func TestStreamAuthorizationExpiryCancelsLifecycleAndRejectsReplay(t *testing.T) {
	client := NewClient(t.Context(), NewId(), NewNoContractClientOob(), DefaultClientSettings())
	defer client.Close()
	manager := client.streamManager
	tracker := newStreamOpenTestTracker(manager.streamBuffer)
	now := time.Now()
	serverClock := int64(1700000000000)
	state := manager.authorization
	state.Lock()
	state.now = func() time.Time { return now }
	state.clockLocal = now
	state.clockServer = serverClock
	state.Unlock()
	sid, generation, peer := NewId(), NewId(), NewId()
	open := &protocol.StreamOpen{StreamId: sid.Bytes(), DestinationId: peer.Bytes(), AuthorizationGeneration: generation.Bytes(), AuthorizationLeaseMillis: 90000, AuthorizationDeadlineUnixMillis: serverClock + 90000}
	receive := func(value *protocol.StreamOpen) {
		frame := RequireToFrameWithDefaultProtocolVersion(value)
		defer MessagePoolReturn(frame.MessageBytes)
		manager.Receive(SourceId(ControlId), []*protocol.Frame{frame}, Peer{})
	}
	publication := tracker.expectPublication(sid)
	receive(open)
	publication.wait(t)
	manager.streamBuffer.mutex.Lock()
	sequence := manager.streamBuffer.streamSequencesByStreamId[sid]
	manager.streamBuffer.mutex.Unlock()
	if sequence == nil || sequence.ctx.Err() != nil {
		t.Fatal("leased stream did not enter lifecycle")
	}
	state.Lock()
	record := state.records[sid]
	firstDeadline := record.deadline
	now = now.Add(20 * time.Second)
	state.Unlock()
	// Identical delayed control keeps its original absolute deadline.
	receive(open)
	state.Lock()
	if !state.records[sid].deadline.Equal(firstDeadline) {
		t.Fatal("replay restarted lease")
	}
	now = firstDeadline
	state.Unlock()
	removed := tracker.expectRemoval(sid)
	manager.expireStreamAuthorization(sid, record)
	publication.resume()
	waitForStreamLifecycleSignal(t, removed, "lease did not cancel complete stream lifecycle")
	if sequence.ctx.Err() == nil || manager.IsStreamOpen(sid) {
		t.Fatal("expired stream retained workers")
	}
	open.AuthorizationDeadlineUnixMillis = serverClock + 180000
	receive(open) // a delayed renewal cannot revive the retired generation
	frame := RequireToFrameWithDefaultProtocolVersion(&protocol.StreamReset{Streams: []*protocol.StreamOpen{open}})
	manager.Receive(SourceId(ControlId), []*protocol.Frame{frame}, Peer{})
	MessagePoolReturn(frame.MessageBytes)
	if manager.IsStreamOpen(sid) {
		t.Fatal("reset resurrected retired stream")
	}
	// An ordinary peer can never submit a new authoritative generation.
	open.StreamId = NewId().Bytes()
	frame = RequireToFrameWithDefaultProtocolVersion(open)
	manager.Receive(SourceId(peer), []*protocol.Frame{frame}, Peer{})
	MessagePoolReturn(frame.MessageBytes)
	if len(state.records) != 1 {
		t.Fatal("peer supplied platform authority")
	}
}
func TestStreamAuthorizationClockDelayAndRetirementGc(t *testing.T) {
	client := NewClient(t.Context(), NewId(), NewNoContractClientOob(), DefaultClientSettings())
	defer client.Close()
	manager := client.streamManager
	state := manager.authorization
	now := time.Now()
	serverClock := int64(1700000000000)
	state.Lock()
	state.now = func() time.Time { return now }
	state.clockLocal = now.Add(-time.Second)
	state.clockServer = serverClock
	state.Unlock()
	opens := 0
	first := &protocol.StreamOpen{StreamId: NewId().Bytes(), AuthorizationGeneration: NewId().Bytes(), AuthorizationLeaseMillis: 90000, AuthorizationDeadlineUnixMillis: serverClock + 90000}
	run := func() error { opens++; return nil }
	for i := 0; i < maxStreamAuthorizationRecords; i++ {
		open := *first
		open.StreamId = NewId().Bytes()
		if err := manager.authorizeStreamOpen(&open, run); err != nil {
			t.Fatal(err)
		}
	}
	if opens != maxStreamAuthorizationRecords {
		t.Fatal("unexpected admission count", opens)
	}
	// Full retirement memory refuses only P2P admission, without dropping records
	// which could authorize a delayed frame. After all server binding lifetimes
	// end, old grants have no future authority and storage can be reclaimed.
	_ = manager.authorizeStreamOpen(first, run)
	if opens != maxStreamAuthorizationRecords {
		t.Fatal("cap discarded a live tombstone")
	}
	state.Lock()
	now = now.Add(8*time.Hour + MaxStreamAuthorizationLease + StreamAuthorizationClockMargin + time.Second)
	state.Unlock()
	_ = manager.authorizeStreamOpen(first, run)
	if opens != maxStreamAuthorizationRecords {
		t.Fatal("expired delayed grant reopened after gc")
	}
	fresh := *first
	fresh.StreamId = NewId().Bytes()
	fresh.AuthorizationDeadlineUnixMillis = serverClock + int64((8*time.Hour+5*time.Minute)/time.Millisecond)
	_ = manager.authorizeStreamOpen(&fresh, run)
	if opens != maxStreamAuthorizationRecords+1 {
		t.Fatal("bounded gc permanently disabled future P2P")
	}
}
func TestAuthorizationCloseCausesStayDistinct(t *testing.T) {
	for code := 4002; code <= 4005; code++ {
		if validAuthorizationClose(code) != AuthorizationCloseCause(code) {
			t.Fatal(code)
		}
	}
	for _, code := range []int{0, 4001, 4006} {
		if validAuthorizationClose(code) != 0 {
			t.Fatal("unrelated close classified as credential rejection")
		}
	}
}

// The existing OOB boundary lends result frames only until its callback returns.
// Reuse them immediately so retaining the bytes deterministically corrupts the
// clock response, without relying on a pool scheduling race.
type borrowedClockOob struct{ serverMillis int64 }

func (self *borrowedClockOob) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	request, err := FromFrame(frames[0])
	for _, frame := range frames {
		MessagePoolReturn(frame.MessageBytes)
	}
	if err != nil {
		callback(nil, err)
		return
	}
	clock, ok := request.(*protocol.StreamAuthorization)
	if !ok {
		callback(nil, nil)
		return
	}
	response := RequireToFrameWithDefaultProtocolVersion(&protocol.StreamAuthorization{ClockId: clock.ClockId, ClockUnixMillis: self.serverMillis})
	callback([]*protocol.Frame{response}, nil)
	clear(response.MessageBytes)
	MessagePoolReturn(response.MessageBytes)
}

func TestStreamAuthorizationClockBorrowsCallbackResponse(t *testing.T) {
	oob := &borrowedClockOob{serverMillis: 1700000000000}
	client := NewClient(t.Context(), NewId(), oob, DefaultClientSettings())
	defer client.Close()
	manager := client.streamManager
	manager.requestAuthorizationClock()
	state := manager.authorization
	state.Lock()
	defer state.Unlock()
	if state.clockServer != oob.serverMillis || state.clockLocal.IsZero() {
		t.Fatal("borrowed OOB response was retained beyond callback lifetime")
	}
}
