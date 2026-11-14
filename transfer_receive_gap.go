package connect

// transfer_receive_gap.go -- the receive gaps a Client holds open, for the
// owner that routes through it.
//
// A ReceiveSequence delivers in order. When an item arrives past a missing
// predecessor, every later item waits behind the hole until the predecessor
// arrives or ReceiveBufferSettings.GapTimeout ends the sequence, so one lost
// item freezes the peer's whole return stream on that sequence. The owner of a
// window client (the multi client) reads two facts here: when the oldest hole
// still open began, so new flows can avoid an exit whose return stream is
// frozen, and the moment a hole times out, so the exit can leave its window and
// its flows re-home instead of waiting on a stream that will not resume.
//
// The sequence worker publishes its hole only when the hole changes, so the
// per-Pack cost is one comparison. The registry takes its own leaf lock and
// calls out to nothing while it holds it. Control sequences are not counted:
// a gap from the platform says nothing about the peer the Client routes to.

import (
	"sync"
	"time"
)

// Called once on the sequence worker when a hole times out and ends its
// sequence. Must not block and must not join the Client.
type receiveGapTimeoutFunction = func(source TransferPath)

// One open hole: the source of the sequence and when the earliest item past
// the missing predecessor arrived, which is when the hole became visible.
type receiveGap struct {
	source TransferPath
	since  time.Time
}

// The open holes of one Client, by sequence. The zero value is ready to use.
// Safe for concurrent use.
type receiveGapRegistry struct {
	stateLock sync.Mutex
	gaps      map[*ReceiveSequence]receiveGap
}

// Records the sequence's open hole, or clears it for a zero since.
func (self *receiveGapRegistry) set(sequence *ReceiveSequence, source TransferPath, since time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if since.IsZero() {
		delete(self.gaps, sequence)
		return
	}
	if self.gaps == nil {
		self.gaps = map[*ReceiveSequence]receiveGap{}
	}
	self.gaps[sequence] = receiveGap{
		source: source,
		since:  since,
	}
}

// When the oldest hole from a peer began; zero when none is open.
func (self *receiveGapRegistry) oldest() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	oldest := time.Time{}
	for _, gap := range self.gaps {
		if gap.source.IsControlSource() {
			continue
		}
		if oldest.IsZero() || gap.since.Before(oldest) {
			oldest = gap.since
		}
	}
	return oldest
}

// When the oldest receive gap still open from a peer began: an item arrived
// past a missing predecessor that has not arrived since. Zero when no gap is
// open. Safe for concurrent use; takes only the registry's leaf lock.
func (self *Client) oldestReceiveGap() time.Time {
	return self.receiveGaps.oldest()
}

// Installs the owner's notice for a hole that timed out (see
// receiveGapTimedOut). Nil removes it. Safe for concurrent use.
func (self *Client) setReceiveGapTimeoutCallback(callback receiveGapTimeoutFunction) {
	if callback == nil {
		self.receiveGapTimeoutCallback.Store(nil)
		return
	}
	self.receiveGapTimeoutCallback.Store(&callback)
}

// Tells the owner that a hole from source outlived GapTimeout and ended its
// sequence. Runs on that sequence's worker. A control source is not the owner's
// peer and is not reported.
func (self *Client) receiveGapTimedOut(source TransferPath) {
	if source.IsControlSource() {
		return
	}
	if callback := self.receiveGapTimeoutCallback.Load(); callback != nil {
		(*callback)(source)
	}
}

// Records this sequence's open hole on its Client when it changes: since is
// when the earliest item past the missing predecessor arrived, zero when no
// hole is open. Called only by the sequence worker.
func (self *ReceiveSequence) publishReceiveGap(since time.Time) {
	if self.publishedGapSince.Equal(since) {
		return
	}
	self.publishedGapSince = since
	if self.client == nil {
		return
	}
	self.client.receiveGaps.set(self, self.source, since)
}
