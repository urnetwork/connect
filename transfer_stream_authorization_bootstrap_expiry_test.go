// An expired pending grant owns its retirement reservation until the binding
// is fenced; stale controls cannot consume a fresher pending or active grant.
package connect

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// The original sequence frees an expired pending slot, reuses capacity, loses
// exact revocation at the cap, then admits a delayed renewal after unrelated gc.
func TestStreamAuthorizationBootstrapExpiredReservationRetirement(t *testing.T) {
	assertMessagePoolOwnership(t)
	client := newBootstrapClockClient(t, &bootstrapClockOob{send: func(_ context.Context, _ Id, callback OobResultFunction) {
		callback(nil, errors.New("unexpected automatic clock"))
	}})
	manager := client.streamManager
	state := manager.authorization
	base := time.Now()
	serverMillis := base.UnixMilli()
	retention := 8*time.Hour + MaxStreamAuthorizationLease + StreamAuthorizationClockMargin
	now := base.Add(-retention + time.Second)
	state.Lock()
	state.now = func() time.Time { return now }
	state.Unlock()
	oldId := NewId()
	manager.retireStreamAuthorization(oldId, NewId().Bytes())
	state.Lock()
	now = base
	state.Unlock()
	for i := 0; i < maxStreamAuthorizationRecords-2; i++ {
		manager.retireStreamAuthorization(NewId(), NewId().Bytes())
	}
	expired := newBootstrapClockOpen(serverMillis)
	expired.AuthorizationDeadlineUnixMillis = serverMillis + StreamAuthorizationClockMargin.Milliseconds() - 1
	streamId := RequireIdFromBytes(expired.StreamId)
	generation := RequireIdFromBytes(expired.AuthorizationGeneration)
	queueBootstrapClockOpen(t, manager, expired)
	state.Lock()
	reserved := len(state.records) == maxStreamAuthorizationRecords-1 && len(state.pending) == 1
	// This is the exact post-clock-commit state with selected pending replay.
	state.clockLocal = base
	state.clockServer = serverMillis
	state.clockPending = false
	state.Unlock()
	if !reserved {
		t.Fatal("fixture did not reserve the final record slot")
	}
	admit := func(open *protocol.StreamOpen) bool {
		admitted := false
		if err := manager.authorizeStreamOpen(open, func() error { admitted = true; return nil }); err != nil {
			t.Fatal(err)
		}
		return admitted
	}
	if admit(expired) {
		t.Fatal("expired grant acquired authority")
	}
	state.Lock()
	record := state.records[streamId]
	expiryRetired := record != nil && record.retired && record.generation == generation
	state.Unlock()
	// Continue every original step even when the first invariant is red.
	capacityReused := admit(newBootstrapClockOpen(serverMillis))
	manager.retireStreamAuthorization(streamId, expired.AuthorizationGeneration)
	state.Lock()
	record = state.records[streamId]
	revokeRetained := record != nil && record.retired && record.generation == generation
	now = base.Add(2 * time.Second)
	state.Unlock()
	delayed := proto.Clone(expired).(*protocol.StreamOpen)
	delayed.AuthorizationDeadlineUnixMillis = serverMillis + MaxStreamAuthorizationLease.Milliseconds()
	replayAdmitted := admit(delayed)
	state.Lock()
	record = state.records[streamId]
	stillRetired := record != nil && record.retired && record.generation == generation
	gcCompleted := state.records[oldId] == nil
	recordCount := len(state.records)
	pendingCount := len(state.pending)
	state.Unlock()
	t.Logf("expired reservation: retired=%t capacity_reused=%t revoke_retained=%t unrelated_gc=%t replay_admitted=%t records=%d pending=%d",
		expiryRetired, capacityReused, revokeRetained, gcCompleted, replayAdmitted, recordCount, pendingCount)
	if !expiryRetired || capacityReused || !revokeRetained || !gcCompleted || replayAdmitted ||
		!stillRetired || pendingCount != 0 || recordCount > maxStreamAuthorizationRecords {
		t.Fatal("expired reservation lost its exact retirement fence across capacity reuse and unrelated GC")
	}
}

// An expired same-generation straggler arrives after a fresh grant is selected,
// then reset omission wins before replay. The straggler must preserve that slot.
func TestStreamAuthorizationBootstrapFresherPendingBeforeReplay(t *testing.T) {
	assertMessagePoolOwnership(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var releaseOnce sync.Once
	serverMillis := time.Now().UnixMilli()
	client := newBootstrapClockClient(t, &bootstrapClockOob{send: func(_ context.Context, clockId Id, callback OobResultFunction) {
		respondBootstrapClock(clockId, serverMillis, callback)
	}})
	manager := client.streamManager
	state := manager.authorization
	open := newBootstrapClockOpen(serverMillis)
	streamId := RequireIdFromBytes(open.StreamId)
	generation := RequireIdFromBytes(open.AuthorizationGeneration)
	queueBootstrapClockOpen(t, manager, open)
	state.beforeClockReplayForTest = func() {
		close(entered)
		<-release
	}
	go func() {
		defer close(done)
		manager.requestAuthorizationClock()
	}()
	defer func() {
		releaseOnce.Do(func() { close(release) })
		waitForStreamLifecycleSignal(t, done, "fresh pending replay did not join")
	}()
	waitForStreamLifecycleSignal(t, entered, "fresh pending grant was not selected")
	receive := func(frame *protocol.Frame) {
		manager.Receive(SourceId(ControlId), []*protocol.Frame{frame}, Peer{})
		clear(frame.MessageBytes)
		MessagePoolReturn(frame.MessageBytes)
	}
	straggler := proto.Clone(open).(*protocol.StreamOpen)
	straggler.AuthorizationDeadlineUnixMillis = serverMillis
	receive(RequireToFrameWithDefaultProtocolVersion(straggler))
	state.Lock()
	pending := state.pending[streamId]
	preserved := pending != nil && pending.AuthorizationDeadlineUnixMillis == open.AuthorizationDeadlineUnixMillis &&
		state.records[streamId] == nil
	state.Unlock()
	if !preserved {
		t.Error("expired straggler consumed or retired the fresher pending reservation")
	}
	receive(RequireToFrameWithDefaultProtocolVersion(&protocol.StreamReset{}))
	releaseOnce.Do(func() { close(release) })
	waitForStreamLifecycleSignal(t, done, "fresh selected replay did not finish")
	state.Lock()
	record := state.records[streamId]
	retired := record != nil && record.retired && record.generation == generation
	pendingCount := len(state.pending)
	state.Unlock()
	if !retired || pendingCount != 0 || manager.IsStreamOpen(streamId) {
		t.Fatalf("reset omission lost after expired straggler: retired=%t pending=%d open=%t",
			retired, pendingCount, manager.IsStreamOpen(streamId))
	}
}

// Expired selected bytes must not retire a same-generation newer active lease.
func TestStreamAuthorizationBootstrapActiveRefreshBeforeExpiredReplay(t *testing.T) {
	checkBootstrapClockActiveRefreshBeforeReplay(t, 0)
}

// Still-valid selected bytes must not shorten the newer active deadline.
func TestStreamAuthorizationBootstrapActiveRefreshBeforeValidReplay(t *testing.T) {
	checkBootstrapClockActiveRefreshBeforeReplay(t, 60*time.Second)
}

// The original clock replay is held while the real control entry admits and
// publishes a newer same-generation grant, then the old selected pointer resumes.
func checkBootstrapClockActiveRefreshBeforeReplay(t *testing.T, oldOffset time.Duration) {
	t.Helper()
	assertMessagePoolOwnership(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var releaseOnce sync.Once
	serverMillis := time.Now().UnixMilli()
	client := newBootstrapClockClient(t, &bootstrapClockOob{send: func(_ context.Context, clockId Id, callback OobResultFunction) {
		respondBootstrapClock(clockId, serverMillis, callback)
	}})
	manager := client.streamManager
	state := manager.authorization
	tracker := newStreamOpenTestTracker(manager.streamBuffer)
	open := newBootstrapClockOpen(serverMillis)
	open.AuthorizationDeadlineUnixMillis = serverMillis + oldOffset.Milliseconds()
	streamId := RequireIdFromBytes(open.StreamId)
	generation := RequireIdFromBytes(open.AuthorizationGeneration)
	queueBootstrapClockOpen(t, manager, open)
	state.beforeClockReplayForTest = func() {
		close(entered)
		<-release
	}
	go func() {
		defer close(done)
		manager.requestAuthorizationClock()
	}()
	defer func() {
		releaseOnce.Do(func() { close(release) })
		waitForStreamLifecycleSignal(t, done, "old selected replay did not join")
	}()
	waitForStreamLifecycleSignal(t, entered, "old pending grant was not selected")
	state.Lock()
	anchor := state.clockLocal
	now := anchor.Add(time.Second)
	state.now = func() time.Time { return now }
	state.Unlock()
	refreshed := proto.Clone(open).(*protocol.StreamOpen)
	refreshed.AuthorizationDeadlineUnixMillis = serverMillis + MaxStreamAuthorizationLease.Milliseconds()
	publication := tracker.expectPublication(streamId)
	defer publication.resume()
	frame := RequireToFrameWithDefaultProtocolVersion(refreshed)
	manager.Receive(SourceId(ControlId), []*protocol.Frame{frame}, Peer{})
	clear(frame.MessageBytes)
	MessagePoolReturn(frame.MessageBytes)
	publication.wait(t)
	state.Lock()
	record := state.records[streamId]
	deadline := anchor.Add(MaxStreamAuthorizationLease - StreamAuthorizationClockMargin)
	established := record != nil && !record.retired && record.generation == generation &&
		record.deadline.Equal(deadline) && len(state.pending) == 0
	state.Unlock()
	if !established {
		t.Fatal("newer grant was not established before old selected replay")
	}
	releaseOnce.Do(func() { close(release) })
	waitForStreamLifecycleSignal(t, done, "old selected replay did not finish")
	state.Lock()
	current := state.records[streamId]
	preserved := current == record && !record.retired && record.deadline.Equal(deadline) &&
		record.generation == generation && len(state.pending) == 0
	state.Unlock()
	if !preserved || !manager.IsStreamOpen(streamId) {
		t.Fatalf("old selected replay changed newer active authority: old_offset=%s preserved=%t open=%t",
			oldOffset, preserved, manager.IsStreamOpen(streamId))
	}
}
