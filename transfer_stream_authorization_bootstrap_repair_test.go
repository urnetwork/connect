// Clock bootstrap preserves the original signed deadline and owns one bounded
// launcher; terminal control wins at explicit commit and replay boundaries.
package connect

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// The duplicated clock budget must stay equal to the ordinary API default.
// A silent OOB peer consumes one finite attempt, not a retry loop.
func TestStreamAuthorizationBootstrapRequestBudget(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		want := DefaultConnectSettings().RequestTimeout
		start := time.Now()
		var deadline time.Time
		attempts := 0
		oob := &bootstrapClockOob{send: func(ctx context.Context, _ Id, callback OobResultFunction) {
			attempts++
			deadline, _ = ctx.Deadline()
			<-ctx.Done()
			callback(nil, ctx.Err())
		}}
		manager := &StreamManager{
			ctx:           context.Background(),
			client:        &Client{clientOob: oob},
			authorization: newStreamAuthorizationState(),
		}
		manager.requestAuthorizationClock()
		state := manager.authorization
		if attempts != 1 || deadline.Sub(start) != want || time.Since(start) != want ||
			!state.clockLocal.IsZero() || state.clockPending {
			t.Fatalf("clock budget: attempts=%d deadline=%s elapsed=%s want=%s clock=%t pending=%t",
				attempts, deadline.Sub(start), time.Since(start), want, !state.clockLocal.IsZero(), state.clockPending)
		}
	})
}

// A consumed valid response is not permission to commit after cancellation.
func TestStreamAuthorizationBootstrapCancelAfterResult(t *testing.T) {
	checkBootstrapClockCanceledAfterResult(t, false)
}

// The same commit fence applies when the parent's absolute deadline expires.
func TestStreamAuthorizationBootstrapDeadlineAfterResult(t *testing.T) {
	checkBootstrapClockCanceledAfterResult(t, true)
}

// The seam is after result consumption, so select scheduling cannot turn this
// into a probabilistic both-ready test.
func checkBootstrapClockCanceledAfterResult(t *testing.T, deadline bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var ctx context.Context
		var cancel context.CancelFunc
		if deadline {
			ctx, cancel = context.WithTimeout(context.Background(), time.Second)
		} else {
			ctx, cancel = context.WithCancel(context.Background())
		}
		defer cancel()
		entered := make(chan struct{})
		release := make(chan struct{})
		done := make(chan struct{})
		var releaseOnce sync.Once
		serverMillis := time.Now().UnixMilli()
		oob := &bootstrapClockOob{send: func(_ context.Context, clockId Id, callback OobResultFunction) {
			respondBootstrapClock(clockId, serverMillis, callback)
		}}
		manager := &StreamManager{
			ctx:           ctx,
			client:        &Client{clientOob: oob},
			authorization: newStreamAuthorizationState(),
		}
		manager.authorization.beforeClockPublishForTest = func() {
			if ctx.Err() != nil {
				t.Error("parent canceled before the valid result was consumed")
			}
			close(entered)
			<-release
		}
		go func() {
			defer close(done)
			manager.requestAuthorizationClock()
		}()
		defer func() {
			releaseOnce.Do(func() { close(release) })
			waitForStreamLifecycleSignal(t, done, "clock commit worker did not join")
		}()
		waitForStreamLifecycleSignal(t, entered, "clock result was not consumed")
		if deadline {
			<-ctx.Done()
		} else {
			cancel()
		}
		releaseOnce.Do(func() { close(release) })
		waitForStreamLifecycleSignal(t, done, "clock commit worker did not return")
		state := manager.authorization
		if state.closed || !state.clockLocal.IsZero() || state.clockServer != 0 || state.clockPending {
			t.Fatalf("canceled result committed: closed=%t clock=%t server=%d pending=%t",
				state.closed, !state.clockLocal.IsZero(), state.clockServer, state.clockPending)
		}
	})
}

// Reset omission retires pending-only authority even after selection for replay.
func TestStreamAuthorizationBootstrapResetBeforeReplay(t *testing.T) {
	checkBootstrapClockRetiredBeforeReplay(t, false)
}

// Exact generation revocation remains terminal at the same replay boundary.
func TestStreamAuthorizationBootstrapRevocationBeforeReplay(t *testing.T) {
	checkBootstrapClockRetiredBeforeReplay(t, true)
}

// A replay is held after the clock commits but before the actual control entry.
func checkBootstrapClockRetiredBeforeReplay(t *testing.T, revoke bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var releaseOnce sync.Once
	serverMillis := time.Now().UnixMilli()
	oob := &bootstrapClockOob{send: func(_ context.Context, clockId Id, callback OobResultFunction) {
		respondBootstrapClock(clockId, serverMillis, callback)
	}}
	client := newBootstrapClockClient(t, oob)
	manager := client.streamManager
	open := newBootstrapClockOpen(serverMillis)
	streamId := RequireIdFromBytes(open.StreamId)
	generation := RequireIdFromBytes(open.AuthorizationGeneration)
	queueBootstrapClockOpen(t, manager, open)
	manager.authorization.beforeClockReplayForTest = func() {
		close(entered)
		<-release
	}
	go func() {
		defer close(done)
		manager.requestAuthorizationClock()
	}()
	defer func() {
		releaseOnce.Do(func() { close(release) })
		waitForStreamLifecycleSignal(t, done, "selected clock replay did not join")
	}()
	waitForStreamLifecycleSignal(t, entered, "clock did not reach selected replay")
	var frame *protocol.Frame
	if revoke {
		frame = RequireToFrameWithDefaultProtocolVersion(&protocol.StreamClose{
			StreamId:                open.StreamId,
			AuthorizationGeneration: open.AuthorizationGeneration,
		})
	} else {
		frame = RequireToFrameWithDefaultProtocolVersion(&protocol.StreamReset{})
	}
	manager.Receive(SourceId(ControlId), []*protocol.Frame{frame}, Peer{})
	MessagePoolReturn(frame.MessageBytes)
	state := manager.authorization
	state.Lock()
	record := state.records[streamId]
	retiredBeforeReplay := record != nil && record.retired && record.generation == generation
	pendingBeforeReplay := len(state.pending)
	state.Unlock()
	if !retiredBeforeReplay || pendingBeforeReplay != 0 {
		t.Errorf("terminal control lost pending identity: revoke=%t retired=%t pending=%d",
			revoke, retiredBeforeReplay, pendingBeforeReplay)
	}
	releaseOnce.Do(func() { close(release) })
	waitForStreamLifecycleSignal(t, done, "selected replay did not finish")
	for _, nextGeneration := range []Id{generation, NewId()} {
		replay := proto.Clone(open).(*protocol.StreamOpen)
		replay.AuthorizationGeneration = nextGeneration.Bytes()
		replay.AuthorizationDeadlineUnixMillis += MaxStreamAuthorizationLease.Milliseconds()
		admitted := false
		if err := manager.authorizeStreamOpen(replay, func() error { admitted = true; return nil }); err != nil {
			t.Fatal(err)
		}
		if admitted {
			t.Errorf("retired stream admitted generation %s", nextGeneration)
		}
	}
	state.Lock()
	record = state.records[streamId]
	retired := record != nil && record.retired && record.generation == generation
	pending := len(state.pending)
	state.Unlock()
	if !retired || pending != 0 || manager.IsStreamOpen(streamId) {
		t.Fatalf("selected replay resurrected authority: retired=%t pending=%d open=%t",
			retired, pending, manager.IsStreamOpen(streamId))
	}
}

// The server binding is immutable: neither an older nor newer mismatched id,
// nor an untagged legacy open, may replace or bypass its pending slot.
func TestStreamAuthorizationBootstrapPendingGenerationFence(t *testing.T) {
	assertMessagePoolOwnership(t)
	client := newBootstrapClockClient(t, &bootstrapClockOob{send: func(_ context.Context, _ Id, callback OobResultFunction) {
		callback(nil, errors.New("unexpected automatic clock"))
	}})
	manager := client.streamManager
	older := NewId()
	open := newBootstrapClockOpen(time.Now().UnixMilli())
	newer := NewId()
	streamId := RequireIdFromBytes(open.StreamId)
	queueBootstrapClockOpen(t, manager, open)
	for _, generation := range []Id{older, newer} {
		other := proto.Clone(open).(*protocol.StreamOpen)
		other.AuthorizationGeneration = generation.Bytes()
		other.AuthorizationDeadlineUnixMillis += MaxStreamAuthorizationLease.Milliseconds()
		queueBootstrapClockOpen(t, manager, other)
	}
	legacy := proto.Clone(open).(*protocol.StreamOpen)
	legacy.AuthorizationGeneration = nil
	legacyAdmitted := false
	if err := manager.authorizeStreamOpen(legacy, func() error { legacyAdmitted = true; return nil }); err != nil {
		t.Fatal(err)
	}
	state := manager.authorization
	state.Lock()
	pending := state.pending[streamId]
	preserved := pending != nil && bytes.Equal(pending.AuthorizationGeneration, open.AuthorizationGeneration) &&
		pending.AuthorizationDeadlineUnixMillis == open.AuthorizationDeadlineUnixMillis
	state.Unlock()
	if !preserved || legacyAdmitted {
		t.Fatalf("pending binding replaced or bypassed: preserved=%t legacy=%t", preserved, legacyAdmitted)
	}
}

// A stale close must not create a mismatched tombstone or delete the pending
// authoritative grant; its exact-generation close still retires it.
func TestStreamAuthorizationBootstrapPendingCloseFence(t *testing.T) {
	assertMessagePoolOwnership(t)
	client := newBootstrapClockClient(t, &bootstrapClockOob{send: func(_ context.Context, _ Id, callback OobResultFunction) {
		callback(nil, errors.New("unexpected automatic clock"))
	}})
	manager := client.streamManager
	open := newBootstrapClockOpen(time.Now().UnixMilli())
	streamId := RequireIdFromBytes(open.StreamId)
	generation := RequireIdFromBytes(open.AuthorizationGeneration)
	queueBootstrapClockOpen(t, manager, open)
	manager.retireStreamAuthorization(streamId, NewId().Bytes())
	state := manager.authorization
	state.Lock()
	pending := state.pending[streamId]
	staleIgnored := pending != nil && bytes.Equal(pending.AuthorizationGeneration, open.AuthorizationGeneration) &&
		state.records[streamId] == nil
	state.Unlock()
	if !staleIgnored {
		t.Error("mismatched close poisoned a pending-only binding")
	}
	manager.retireStreamAuthorization(streamId, open.AuthorizationGeneration)
	state.Lock()
	record := state.records[streamId]
	exactRetired := record != nil && record.retired && record.generation == generation && state.pending[streamId] == nil
	state.Unlock()
	if !exactRetired {
		t.Fatal("exact pending generation was not retired")
	}
}

// At the existing 128-slot cap, a refresh of a retained identity must replace
// its older deadline; a later straggler must not undo that refresh.
func TestStreamAuthorizationBootstrapPendingCapacityRefresh(t *testing.T) {
	assertMessagePoolOwnership(t)
	client := newBootstrapClockClient(t, &bootstrapClockOob{send: func(_ context.Context, _ Id, callback OobResultFunction) {
		callback(nil, errors.New("unexpected automatic clock"))
	}})
	manager := client.streamManager
	serverMillis := time.Now().UnixMilli()
	first := newBootstrapClockOpen(serverMillis)
	queueBootstrapClockOpen(t, manager, first)
	for i := 1; i < 128; i++ {
		queueBootstrapClockOpen(t, manager, newBootstrapClockOpen(serverMillis))
	}
	overflow := newBootstrapClockOpen(serverMillis)
	queueBootstrapClockOpen(t, manager, overflow)
	refreshed := proto.Clone(first).(*protocol.StreamOpen)
	refreshed.AuthorizationDeadlineUnixMillis += 1000
	queueBootstrapClockOpen(t, manager, refreshed)
	queueBootstrapClockOpen(t, manager, first)
	state := manager.authorization
	state.Lock()
	pending := state.pending[RequireIdFromBytes(first.StreamId)]
	correct := len(state.pending) == 128 && state.pending[RequireIdFromBytes(overflow.StreamId)] == nil &&
		pending != nil && pending.AuthorizationDeadlineUnixMillis == refreshed.AuthorizationDeadlineUnixMillis
	state.Unlock()
	if !correct {
		t.Fatal("bounded pending refresh was refused or rolled back by an older grant")
	}
}

// A fresh platform control retries retained work even when its own identity
// is refused at the pending cap. Errors do not create an internal retry loop.
func TestStreamAuthorizationBootstrapPendingFullRetry(t *testing.T) {
	assertMessagePoolOwnership(t)
	attempts := 0
	oob := &bootstrapClockOob{send: func(_ context.Context, _ Id, callback OobResultFunction) {
		attempts++
		callback(nil, context.DeadlineExceeded)
	}}
	client := newBootstrapClockClient(t, oob)
	manager := client.streamManager
	serverMillis := time.Now().UnixMilli()
	for i := 0; i < 128; i++ {
		queueBootstrapClockOpen(t, manager, newBootstrapClockOpen(serverMillis))
	}
	manager.requestAuthorizationClock()
	done := make(chan struct{})
	manager.authorization.afterClockRequestForTest = func() { close(done) }
	overflow := newBootstrapClockOpen(serverMillis)
	frame := RequireToFrameWithDefaultProtocolVersion(overflow)
	manager.Receive(SourceId(ControlId), []*protocol.Frame{frame}, Peer{})
	MessagePoolReturn(frame.MessageBytes)
	waitForStreamLifecycleSignal(t, done, "fresh control did not retry full pending work")
	state := manager.authorization
	state.Lock()
	correct := attempts == 2 && len(state.pending) == 128 &&
		state.pending[RequireIdFromBytes(overflow.StreamId)] == nil &&
		state.clockLocal.IsZero() && !state.clockPending
	state.Unlock()
	if !correct {
		t.Fatal("capacity refusal stranded retained work or started unbounded clock retry")
	}
}

// Pending identities reserve retirement capacity. Even clock-ready fresh
// admission cannot steal the final reserved slot before selected replay.
func TestStreamAuthorizationBootstrapPendingRecordReservation(t *testing.T) {
	assertMessagePoolOwnership(t)
	client := newBootstrapClockClient(t, &bootstrapClockOob{send: func(_ context.Context, _ Id, callback OobResultFunction) {
		callback(nil, errors.New("unexpected automatic clock"))
	}})
	manager := client.streamManager
	for i := 0; i < maxStreamAuthorizationRecords-1; i++ {
		manager.retireStreamAuthorization(NewId(), NewId().Bytes())
	}
	serverMillis := time.Now().UnixMilli()
	first := newBootstrapClockOpen(serverMillis)
	queueBootstrapClockOpen(t, manager, first)
	queueBootstrapClockOpen(t, manager, newBootstrapClockOpen(serverMillis))
	state := manager.authorization
	state.Lock()
	reserved := len(state.records) == maxStreamAuthorizationRecords-1 && len(state.pending) == 1
	// This is the real state between clock commit and selected replay.
	state.clockLocal = time.Now()
	state.clockServer = serverMillis
	state.Unlock()
	if !reserved {
		t.Error("pending admission did not reserve its bounded tombstone")
	}
	admitted := false
	if err := manager.authorizeStreamOpen(newBootstrapClockOpen(serverMillis), func() error { admitted = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if admitted {
		t.Error("clock-ready arrival consumed a pending retirement reservation")
	}
	manager.retireAbsentAuthorizations(map[Id]bool{})
	state.Lock()
	record := state.records[RequireIdFromBytes(first.StreamId)]
	bounded := len(state.records) == maxStreamAuthorizationRecords && len(state.pending) == 0 &&
		record != nil && record.retired
	state.Unlock()
	if !bounded {
		t.Fatal("reset lost pending retirement or exceeded the record cap")
	}
}

// Delayed clock acquisition keeps its send-time anchor: a queued short grant
// expires while a longer grant retains the exact original absolute deadline.
func TestStreamAuthorizationBootstrapExpiredPendingReplay(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		serverMillis := time.Now().UnixMilli()
		oob := &bootstrapClockOob{send: func(_ context.Context, clockId Id, callback OobResultFunction) {
			<-time.After(time.Second)
			respondBootstrapClock(clockId, serverMillis, callback)
		}}
		client := newBootstrapClockClient(t, oob)
		manager := client.streamManager
		expired := newBootstrapClockOpen(serverMillis)
		expired.AuthorizationDeadlineUnixMillis = serverMillis + StreamAuthorizationClockMargin.Milliseconds() + 500
		valid := newBootstrapClockOpen(serverMillis)
		expiredId := RequireIdFromBytes(expired.StreamId)
		validId := RequireIdFromBytes(valid.StreamId)
		queueBootstrapClockOpen(t, manager, expired)
		queueBootstrapClockOpen(t, manager, valid)
		tracker := newStreamOpenTestTracker(manager.streamBuffer)
		publication := tracker.expectPublication(validId)
		defer publication.resume()
		start := time.Now()
		manager.requestAuthorizationClock()
		publication.wait(t)
		state := manager.authorization
		state.Lock()
		record := state.records[validId]
		correct := state.clockLocal.Equal(start) && state.clockServer == serverMillis &&
			state.records[expiredId] != nil && state.records[expiredId].retired && state.records[expiredId].generation == RequireIdFromBytes(expired.AuthorizationGeneration) && len(state.pending) == 0 && record != nil &&
			record.deadline.Equal(start.Add(MaxStreamAuthorizationLease-StreamAuthorizationClockMargin))
		state.Unlock()
		if !correct || manager.IsStreamOpen(expiredId) {
			t.Fatal("clock acquisition restarted a lease or replayed expired pending authority")
		}
	})
}

// A synchronous external launcher may ignore cancellation. Close must cancel
// its context and CloseAndWait must report it live until that call returns.
func TestStreamAuthorizationBootstrapOwnedLauncherJoin(t *testing.T) {
	assertMessagePoolOwnership(t)
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	done := make(chan struct{})
	var releaseOnce sync.Once
	serverMillis := time.Now().UnixMilli()
	oob := &bootstrapClockOob{send: func(ctx context.Context, clockId Id, callback OobResultFunction) {
		entered <- ctx
		<-release
		respondBootstrapClock(clockId, serverMillis, callback)
	}}
	client := newBootstrapClockClient(t, oob)
	manager := client.streamManager
	manager.authorization.afterClockRequestForTest = func() { close(done) }
	frame := RequireToFrameWithDefaultProtocolVersion(newBootstrapClockOpen(serverMillis))
	manager.Receive(SourceId(ControlId), []*protocol.Frame{frame}, Peer{})
	MessagePoolReturn(frame.MessageBytes)
	defer func() {
		releaseOnce.Do(func() { close(release) })
		waitForStreamLifecycleSignal(t, done, "owned synchronous clock did not return")
	}()
	var attemptCtx context.Context
	guard := time.NewTimer(5 * time.Second)
	defer guard.Stop()
	select {
	case attemptCtx = <-entered:
	case <-guard.C:
		t.Fatal("automatic clock launcher did not enter")
	}
	manager.Close()
	if attemptCtx.Err() == nil {
		t.Error("stream-manager close did not cancel its clock attempt")
	}
	waitCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.closeAndWait(waitCtx); !errors.Is(err, context.Canceled) {
		t.Errorf("closed wait did not identify the held launcher: %v", err)
	}
	releaseOnce.Do(func() { close(release) })
	waitForStreamLifecycleSignal(t, done, "owned clock did not reach method completion")
	joinCtx, joinCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer joinCancel()
	if err := manager.closeAndWait(joinCtx); err != nil {
		t.Fatal(err)
	}
	state := manager.authorization
	state.Lock()
	closed := state.closed && len(state.pending) == 0 && state.clockLocal.IsZero() && !state.clockPending
	state.Unlock()
	if !closed {
		t.Fatal("late synchronous callback republished closed clock authority")
	}
}
