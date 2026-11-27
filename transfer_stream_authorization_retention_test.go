package connect

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Renew the same legitimate grant through the lifetime of its first
// tombstone. The owner and its timer must cover the final lease as well.
func TestRenewedStreamKeepsAuthorizationOwner(t *testing.T) {
	client := NewClient(t.Context(), NewId(), NewNoContractClientOob(), streamManagerTestSettings())
	defer client.Close()
	manager := client.streamManager
	state := manager.authorization
	start := time.Now()
	now := start
	state.Lock()
	state.now = func() time.Time { return now }
	state.clockLocal, state.clockServer = start, start.UnixMilli()
	state.Unlock()
	id := NewId()
	open := &protocol.StreamOpen{
		StreamId: id.Bytes(), AuthorizationGeneration: NewId().Bytes(),
		AuthorizationLeaseMillis: uint32(MaxStreamAuthorizationLease.Milliseconds()),
	}
	opens := 0
	admit := func() error { opens++; return nil }
	var first *streamAuthorizationRecord
	var firstTimer *time.Timer
	for elapsed := time.Duration(0); elapsed <= 8*time.Hour+3*time.Minute; elapsed += 30 * time.Second {
		state.Lock()
		now = start.Add(elapsed)
		open.AuthorizationDeadlineUnixMillis = now.Add(MaxStreamAuthorizationLease).UnixMilli()
		state.Unlock()
		if err := manager.authorizeStreamOpen(open, admit); err != nil {
			t.Fatal(err)
		}
		record := state.records[id]
		if elapsed == 0 {
			first = record
			firstTimer = record.timer
		}
		if record == nil || record != first || record.retired || !record.deadline.After(now) {
			t.Fatalf("renewal at %s lost its live authorization owner", elapsed)
		}
	}
	if firstTimer == nil || first.timer != firstTimer {
		t.Fatal("renewals replaced the original timer")
	}
	// Expiry is delivered on the injected clock, without waiting for a timer.
	state.Lock()
	now = first.deadline
	state.Unlock()
	manager.expireStreamAuthorization(id, first)
	if !first.retired || manager.streamAuthorizationActive(id) || !first.retainUntil.After(now.Add(8*time.Hour-time.Second)) {
		t.Fatal("retirement did not protect the final lease generation")
	}
	manager.withStreamAuthorization(id, func() { t.Fatal("retired stream admitted work") })
	if opens != int((8*time.Hour+3*time.Minute)/(30*time.Second))+1 {
		t.Fatalf("admitted %d renewals", opens)
	}
}

// A grant waiting for the platform clock already identifies a leased stream.
func TestPendingStreamNeverConfersLegacyAuthority(t *testing.T) {
	client := NewClient(t.Context(), NewId(), NewNoContractClientOob(), streamManagerTestSettings())
	defer client.Close()
	manager := client.streamManager
	id := NewId()
	manager.authorization.Lock()
	manager.authorization.pending[id] = &protocol.StreamOpen{StreamId: id.Bytes(), AuthorizationGeneration: NewId().Bytes()}
	manager.authorization.Unlock()
	if manager.streamAuthorizationActive(id) {
		t.Error("reset admitted a grant waiting for its clock")
	}
	manager.withStreamAuthorization(id, func() { t.Error("pending grant admitted forwarding or reconsideration") })
}

// A lifecycle admitted with a grant remembers its authority independent of
// the retention index. A legacy entry must never adopt that lifecycle.
func TestLeasedStreamRequiresItsIndexedGrant(t *testing.T) {
	client := NewClient(t.Context(), NewId(), NewNoContractClientOob(), streamManagerTestSettings())
	defer client.Close()
	manager := client.streamManager
	state := manager.authorization
	now := time.Now()
	state.Lock()
	state.clockLocal, state.clockServer = now, now.UnixMilli()
	state.Unlock()
	id := NewId()
	open := &protocol.StreamOpen{
		StreamId: id.Bytes(), DestinationId: NewId().Bytes(), AuthorizationGeneration: NewId().Bytes(),
		AuthorizationLeaseMillis: 90000, AuthorizationDeadlineUnixMillis: now.Add(MaxStreamAuthorizationLease).UnixMilli(),
	}
	tracker := newStreamOpenTestTracker(manager.streamBuffer)
	publication := tracker.expectPublication(id)
	frame := RequireToFrameWithDefaultProtocolVersion(open)
	manager.Receive(SourceId(ControlId), []*protocol.Frame{frame}, Peer{})
	MessagePoolReturn(frame.MessageBytes)
	publication.wait(t)
	defer publication.resume()
	state.Lock()
	record := state.records[id]
	delete(state.records, id)
	state.Unlock()
	defer func() {
		state.Lock()
		state.records[id] = record
		state.Unlock()
	}()
	if manager.streamAuthorizationActive(id) {
		t.Error("missing lease owner was interpreted as legacy")
	}
	manager.withStreamAuthorization(id, func() { t.Error("missing lease admitted a dependent operation") })
	_ = manager.authorizeStreamOpen(&protocol.StreamOpen{StreamId: id.Bytes()}, func() error {
		t.Error("legacy open reused a leased lifecycle")
		return nil
	})
}

// The authoritative reset identifies leases by generation as well as stream id.
func TestStreamResetRelistingRequiresMatchingGrant(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		client := NewClient(t.Context(), NewId(), NewNoContractClientOob(), streamManagerTestSettings())
		m := client.streamManager
		now := time.Now()
		id, gen := NewId(), NewId()
		state := m.authorization
		state.Lock()
		state.clockLocal, state.clockServer = now, now.UnixMilli()
		state.Unlock()
		open := &protocol.StreamOpen{StreamId: id.Bytes(), DestinationId: NewId().Bytes(), AuthorizationGeneration: gen.Bytes(), AuthorizationLeaseMillis: 90000, AuthorizationDeadlineUnixMillis: now.Add(MaxStreamAuthorizationLease).UnixMilli()}
		if err := m.authorizeStreamOpen(open, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		open.AuthorizationGeneration = NewId().Bytes()
		if legacy {
			open.AuthorizationGeneration = nil
		}
		frame := RequireToFrameWithDefaultProtocolVersion(&protocol.StreamReset{Streams: []*protocol.StreamOpen{open}})
		m.Receive(SourceId(ControlId), []*protocol.Frame{frame}, Peer{})
		MessagePoolReturn(frame.MessageBytes)
		if m.streamAuthorizationActive(id) {
			t.Errorf("legacy=%v kept the superseded grant", legacy)
		}
		client.Close()
	}
}

// A retained platform decision is another lifecycle owner, even before a
// later relationship proof permits its transport to open.
func TestRejectedLeaseCannotBeReopenedAsLegacy(t *testing.T) {
	c := NewClient(t.Context(), NewId(), NewNoContractClientOob(), streamManagerTestSettings())
	defer c.Close()
	m := c.streamManager
	id, source := NewId(), NewId()
	m.retainRejectedStreamOpen(&source, nil, id, false, &streamAuthorizationRecord{generation: NewId()})
	if m.streamAuthorizationActive(id) {
		t.Fatal("lost index promoted a retained lease to legacy")
	}
}

// Keep a real published lifecycle across a full binding horizon. Its final
// timer withdraws the exact stream while the independently listed sibling stays up.
func TestLongRenewalRetiresOnlyItsPublishedStream(t *testing.T) {
	c := NewClient(t.Context(), NewId(), NewNoContractClientOob(), streamManagerTestSettings())
	defer c.Close()
	m := c.streamManager
	tracker := newStreamOpenTestTracker(m.streamBuffer)
	state := m.authorization
	start := time.Now()
	now := start
	state.Lock()
	state.now = func() time.Time { return now }
	state.clockLocal, state.clockServer = start, start.UnixMilli()
	state.Unlock()
	receive := func(open *protocol.StreamOpen) {
		f := RequireToFrameWithDefaultProtocolVersion(open)
		m.Receive(SourceId(ControlId), []*protocol.Frame{f}, Peer{})
		MessagePoolReturn(f.MessageBytes)
	}
	id, sibling, dest := NewId(), NewId(), NewId()
	side := tracker.expectPublication(sibling)
	receive(&protocol.StreamOpen{StreamId: sibling.Bytes(), DestinationId: dest.Bytes()})
	side.wait(t)
	defer side.resume()
	first := tracker.expectPublication(id)
	open := &protocol.StreamOpen{StreamId: id.Bytes(), DestinationId: dest.Bytes(), AuthorizationGeneration: NewId().Bytes(), AuthorizationLeaseMillis: 90000}
	for elapsed := time.Duration(0); elapsed <= 8*time.Hour+3*time.Minute; elapsed += 30 * time.Second {
		state.Lock()
		now = start.Add(elapsed)
		open.AuthorizationDeadlineUnixMillis = now.Add(MaxStreamAuthorizationLease).UnixMilli()
		state.Unlock()
		receive(open)
		if elapsed == 0 {
			first.wait(t)
			defer first.resume()
		}
	}
	m.streamBuffer.mutex.Lock()
	stream := m.streamBuffer.streamSequencesByStreamId[id]
	m.streamBuffer.mutex.Unlock()
	state.Lock()
	owner := state.records[id]
	if stream == nil || owner == nil || stream.authorization != owner {
		state.Unlock()
		t.Fatal("published stream lost its renewal owner")
	}
	now = owner.deadline
	state.Unlock()
	removed := tracker.expectRemoval(id)
	m.expireStreamAuthorization(id, owner)
	first.resume()
	waitForStreamLifecycleSignal(t, removed, "exact lease retirement")
	if m.IsStreamOpen(id) || m.streamAuthorizationActive(id) || !m.IsStreamOpen(sibling) {
		t.Fatal("retirement crossed stream ownership")
	}
}
