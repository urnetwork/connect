// Stream authority remains fail-closed until one bounded, owned clock attempt
// establishes a conservative anchor; pending grants preserve exact identities.
package connect

import (
	"bytes"
	"context"
	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
	"sync"
	"time"
)

const MaxStreamAuthorizationLease = 90 * time.Second

// Fleet release requires server clock spread below this bound. Starting the
// anchor at the request's send time makes arbitrary response/network delay
// conservative. Client wall clock is never used in lease conversion.
const StreamAuthorizationClockMargin = 30 * time.Second
const maxStreamAuthorizationRecords = 4096
const maxPendingStreamAuthorizations = 128

// Match the ordinary API request budget, including an unproven route's hello.
// Acquisition does not extend the grant's absolute deadline or start retries.
const streamAuthorizationClockTimeout = 15 * time.Second

type streamAuthorizationRecord struct {
	generation  Id
	deadline    time.Time
	retainUntil time.Time
	retired     bool
	timer       *time.Timer
}
type streamAuthorizationState struct {
	sync.Mutex
	records      map[Id]*streamAuthorizationRecord
	clockServer  int64
	clockLocal   time.Time
	clockPending bool
	clockCancel  context.CancelFunc
	clockWorkers *lifecycleAdmission
	// Instance-private barriers expose result commit and replay ordering only.
	beforeClockPublishForTest func()
	beforeClockReplayForTest  func()
	afterClockRequestForTest  func()
	pending                   map[Id]*protocol.StreamOpen
	closed                    bool
	now                       func() time.Time
}

func newStreamAuthorizationState() *streamAuthorizationState {
	return &streamAuthorizationState{
		records:      map[Id]*streamAuthorizationRecord{},
		pending:      map[Id]*protocol.StreamOpen{},
		clockWorkers: newLifecycleAdmission(),
		now:          time.Now,
	}
}
func (self *StreamManager) closeAuthorizations() {
	state := self.authorization
	state.Lock()
	defer state.Unlock()
	state.closed = true
	if state.clockCancel != nil {
		state.clockCancel()
	}
	state.clockWorkers.close()
	for _, record := range state.records {
		record.retired = true
		if record.timer != nil {
			record.timer.Stop()
		}
	}
	state.pending = map[Id]*protocol.StreamOpen{}
}

// Caller owns state lock through OpenStream/CloseStream. A timer cannot close a
// generation before OpenStream queues it and then let a late open resurrect it.
func (self *StreamManager) authorizeStreamOpen(open *protocol.StreamOpen, run func() error) error {
	state := self.authorization
	state.Lock()
	defer state.Unlock()
	if state.closed || self.ctx.Err() != nil {
		return nil
	}
	id, err := IdFromBytes(open.StreamId)
	if err != nil {
		return err
	}
	now := state.now()
	for key, record := range state.records {
		if !now.Before(record.retainUntil) {
			if record.timer != nil {
				record.timer.Stop()
			}
			delete(state.records, key)
		}
	}
	previous := state.records[id]
	if len(open.AuthorizationGeneration) == 0 {
		if previous != nil || state.pending[id] != nil {
			return nil
		}
		return run()
	}
	generation, err := IdFromBytes(open.AuthorizationGeneration)
	if err != nil {
		return err
	}
	if previous != nil && (previous.retired || previous.generation != generation) {
		return nil
	}
	if open.AuthorizationLeaseMillis == 0 || open.AuthorizationLeaseMillis > uint32(MaxStreamAuthorizationLease/time.Millisecond) {
		return nil
	}
	pending := state.pending[id]
	// A stream binding is immutable on the server. Apply the same generation
	// fence before a grant can occupy or replace the pre-clock slot.
	if pending != nil && !bytes.Equal(pending.AuthorizationGeneration, open.AuthorizationGeneration) {
		return nil
	}
	if state.clockLocal.IsZero() {
		// Same-generation refresh remains admissible at capacity; a delayed
		// older grant must not replace a later absolute authorization deadline.
		canRetain := pending != nil || (len(state.pending) < maxPendingStreamAuthorizations && len(state.records)+len(state.pending) < maxStreamAuthorizationRecords)
		if canRetain && (pending == nil || pending.AuthorizationDeadlineUnixMillis <= open.AuthorizationDeadlineUnixMillis) {
			state.pending[id] = proto.Clone(open).(*protocol.StreamOpen)
		}
		// A fresh control can retry retained work even when its own new slot
		// is refused. There is no timer or internal retry owner.
		if len(state.pending) > 0 && !state.clockPending && state.clockWorkers.start() {
			state.clockPending = true
			go func() {
				defer state.clockWorkers.finish()
				self.requestAuthorizationClock()
			}()
		}
		return nil
	}
	// An expired straggler cannot consume a fresher pending reservation.
	if pending != nil && open.AuthorizationDeadlineUnixMillis < pending.AuthorizationDeadlineUnixMillis {
		return nil
	}
	// Keep pending visible until this lock owns admission, so reset/revoke
	// can retire it even after a clock result has selected it for replay.
	delete(state.pending, id)
	// Wire absolute deadline is converted relative to a monotonic request-start
	// anchor. A queued/replayed control frame cannot restart a 90-second timer.
	deadline := state.clockLocal.Add(time.Duration(open.AuthorizationDeadlineUnixMillis-state.clockServer)*time.Millisecond - StreamAuthorizationClockMargin)
	maxDeadline := now.Add(time.Duration(open.AuthorizationLeaseMillis) * time.Millisecond)
	if maxDeadline.Before(deadline) {
		deadline = maxDeadline
	}
	if !now.Before(deadline) {
		// Expiry consumes the reserved slot as retirement, not free capacity.
		// A later close must not lose this known binding when the table fills.
		if pending != nil && previous == nil {
			state.records[id] = &streamAuthorizationRecord{generation: generation, retired: true, retainUntil: now.Add(8*time.Hour + MaxStreamAuthorizationLease + StreamAuthorizationClockMargin)}
		}
		return nil
	}
	if previous == nil {
		if len(state.records)+len(state.pending) >= maxStreamAuthorizationRecords {
			return nil
		}
		previous = &streamAuthorizationRecord{generation: generation, retainUntil: now.Add(8*time.Hour + MaxStreamAuthorizationLease + StreamAuthorizationClockMargin)}
		state.records[id] = previous
	} else if !now.Before(previous.deadline) {
		previous.retired = true
		self.streamBuffer.CloseStream(id)
		return nil
	}
	if deadline.After(previous.deadline) {
		previous.deadline = deadline
	}
	if previous.timer != nil {
		previous.timer.Stop()
	}
	record := previous
	previous.timer = time.AfterFunc(max(0, record.deadline.Sub(now)), func() { self.expireStreamAuthorization(id, record) })
	return run()
}
func (self *StreamManager) expireStreamAuthorization(id Id, record *streamAuthorizationRecord) {
	state := self.authorization
	state.Lock()
	defer state.Unlock()
	if state.closed || state.records[id] != record || record.retired {
		return
	}
	if remaining := record.deadline.Sub(state.now()); remaining > 0 {
		record.timer = time.AfterFunc(remaining, func() { self.expireStreamAuthorization(id, record) })
		return
	}
	record.retired = true
	delete(state.pending, id)
	self.forgetRejectedStreamOpen(id)
	self.streamBuffer.CloseStream(id)
}
func (self *StreamManager) retireStreamAuthorization(id Id, generation []byte) {
	state := self.authorization
	state.Lock()
	defer state.Unlock()
	record := state.records[id]
	pending := state.pending[id]
	if record == nil && pending != nil {
		if len(generation) > 0 && !bytes.Equal(pending.AuthorizationGeneration, generation) {
			return
		}
		generation = pending.AuthorizationGeneration
	}
	if record != nil {
		if len(generation) > 0 && !bytes.Equal(record.generation.Bytes(), generation) {
			return
		}
		record.retired = true
		if record.timer != nil {
			record.timer.Stop()
		}
	} else if gen, err := IdFromBytes(generation); err == nil && (pending != nil || len(state.records)+len(state.pending) < maxStreamAuthorizationRecords) {
		state.records[id] = &streamAuthorizationRecord{generation: gen, retired: true, retainUntil: state.now().Add(8*time.Hour + MaxStreamAuthorizationLease + StreamAuthorizationClockMargin)}
	}
	delete(state.pending, id)
	self.forgetRejectedStreamOpen(id)
	self.streamBuffer.CloseStream(id)
}
func (self *StreamManager) streamAuthorizationActive(id Id) bool {
	state := self.authorization
	state.Lock()
	defer state.Unlock()
	record := state.records[id]
	return !state.closed && (record == nil || !record.retired && state.now().Before(record.deadline))
}
func (self *StreamManager) requestAuthorizationClock() {
	state := self.authorization
	if state.afterClockRequestForTest != nil {
		defer state.afterClockRequestForTest()
	}
	id := NewId()
	start := time.Now()
	state.Lock()
	if state.closed || self.ctx.Err() != nil {
		state.clockPending = false
		state.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(self.ctx, streamAuthorizationClockTimeout)
	state.clockCancel = cancel
	state.Unlock()
	defer cancel()
	request := RequireToFrameWithDefaultProtocolVersion(&protocol.StreamAuthorization{ClockId: id.Bytes()})
	// OOB responses are borrowed for the callback only. Decode before returning;
	// retaining pooled frame bytes races the owner's immediate reclamation.
	result := make(chan int64, 1)
	callback := func(frames []*protocol.Frame, err error) {
		serverMillis := int64(0)
		if err == nil {
			for _, frame := range frames {
				message, decodeErr := FromFrame(frame)
				if decodeErr == nil {
					if value, ok := message.(*protocol.StreamAuthorization); ok && bytes.Equal(value.ClockId, id.Bytes()) && value.ClockUnixMillis > 0 {
						serverMillis = value.ClockUnixMillis
					}
				}
			}
		}
		select {
		case <-ctx.Done():
		case result <- serverMillis:
		default:
		}
	}
	if oob, ok := self.client.ClientOob().(OutOfBandControlWithCtx); ok {
		oob.SendControlWithCtx(ctx, []*protocol.Frame{request}, callback)
	} else {
		self.client.ClientOob().SendControl([]*protocol.Frame{request}, callback)
	}
	serverMillis := int64(0)
	select {
	case <-ctx.Done():
	case serverMillis = <-result:
	}
	if state.beforeClockPublishForTest != nil {
		state.beforeClockPublishForTest()
	}
	state.Lock()
	state.clockPending = false
	state.clockCancel = nil
	if state.closed || ctx.Err() != nil || serverMillis == 0 {
		state.Unlock()
		return
	}
	if serverMillis > 0 {
		// Re-anchoring may only shorten deadlines, including a resident migration.
		if state.clockLocal.IsZero() || start.Before(state.clockLocal.Add(time.Duration(serverMillis-state.clockServer)*time.Millisecond)) {
			state.clockLocal = start
			state.clockServer = serverMillis
		}
	}
	pending := make([]*protocol.StreamOpen, 0, len(state.pending))
	for _, open := range state.pending {
		pending = append(pending, open)
	}
	state.Unlock()
	if state.beforeClockReplayForTest != nil {
		state.beforeClockReplayForTest()
	}
	for _, open := range pending {
		frame := RequireToFrameWithDefaultProtocolVersion(open)
		_ = self.handleControlFrame(frame)
		MessagePoolReturn(frame.MessageBytes)
	}
}

func (self *StreamManager) withStreamAuthorization(id Id, run func()) {
	state := self.authorization
	state.Lock()
	defer state.Unlock()
	record := state.records[id]
	if !state.closed && (record == nil || !record.retired && state.now().Before(record.deadline)) {
		run()
	}
}
func (self *StreamManager) retireAbsentAuthorizations(keep map[Id]bool) {
	state := self.authorization
	state.Lock()
	defer state.Unlock()
	// Pending slots reserve record capacity, so omission can create a durable
	// retirement fence before a selected clock replay reaches admission.
	for id, open := range state.pending {
		if !keep[id] {
			if state.records[id] == nil {
				generation, err := IdFromBytes(open.AuthorizationGeneration)
				if err == nil {
					state.records[id] = &streamAuthorizationRecord{generation: generation, retired: true, retainUntil: state.now().Add(8*time.Hour + MaxStreamAuthorizationLease + StreamAuthorizationClockMargin)}
				}
			}
			delete(state.pending, id)
		}
	}
	for id, record := range state.records {
		if !keep[id] {
			record.retired = true
			if record.timer != nil {
				record.timer.Stop()
			}
			delete(state.pending, id)
		}
	}
}

// Testing_AdvanceStreamAuthorizationClock delivers due production timer
// callbacks on an explicit monotonic clock. It never grants authority.
func (self *Client) Testing_AdvanceStreamAuthorizationClock(elapsed time.Duration) {
	manager := self.streamManager
	state := manager.authorization
	state.Lock()
	now := state.now().Add(elapsed)
	state.now = func() time.Time { return now }
	records := map[Id]*streamAuthorizationRecord{}
	for id, record := range state.records {
		records[id] = record
	}
	state.Unlock()
	for id, record := range records {
		manager.expireStreamAuthorization(id, record)
	}
}
