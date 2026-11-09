package connect

import (
	"context"
	"slices"
	"sync"
	"time"
	"unsafe"
)

// The origin socket and Transfer acknowledge different delivery boundaries.
// Keep bounded origin chunks until the source's inner TCP acknowledges them;
// a successful Transfer callback alone cannot prove that a TUN/kernel kept a
// segment. Chunk storage avoids retaining one pool root per MTU-sized packet.
type tcpReturnChunk struct {
	start   uint32
	end     uint32
	payload []byte
	charge  ByteCount
	fin     bool
}

func (self *TcpSequence) returnMemoryBudget() *TransferMemoryBudget {
	if self.tcpBufferSettings.MemoryBudget != nil {
		return self.tcpBufferSettings.MemoryBudget
	}
	return self.tcpBufferSettings.ReturnQueueBudget
}

// retainReturnChunk borrows payload and keeps an owned copy. Only the socket
// reader calls it; waiting here propagates pressure into this flow's origin.
// ACK callbacks merely release capacity and signal the dedicated replay worker.
//
// Every TCP flow of the NAT retains from one shared budget, and a reader that
// finds it full waits in the budget's arrival order: bytes an acknowledgement
// frees are reserved for the oldest waiting reader before any woken reader
// runs. Waking every reader to race for them let the runtime pick: with no
// idle processor the reader that waited first retained, read on and waited
// again ahead of the readers it beat, so one flow kept every freed byte. A
// reader held by its own per-flow limit takes no place in line until its
// acknowledgements lift it. A queued reader keeps the metadata plan it queued
// with: only this reader appends chunks, so the array it planned to grow
// stays valid, and its per-flow limit stays met, while acknowledgements drain
// the flow.
func (self *TcpSequence) retainReturnChunk(payload []byte, start uint32, fin bool) bool {
	// Account for the pool root and geometric slice growth of chunk metadata.
	// Reserve before copying: a blocked flow borrows its existing socket read
	// buffer and must not allocate another uncharged replay buffer.
	charge := ByteCount(128)
	if len(payload) != 0 {
		capacity := len(payload)
		for _, pool := range orderedMessagePools() {
			if capacity <= pool.size {
				capacity = pool.size
				break
			}
		}
		charge += ByteCount(capacity + MessagePoolMetaByteCount)
	}
	budget := self.returnMemoryBudget()
	limit := self.tcpBufferSettings.ReturnQueueMaxByteCount
	if limit <= 0 {
		limit = max(ByteCount(self.tcpBufferSettings.MaxWindowSize), mib(1))
		if budget != nil {
			limit = budget.TotalByteCount()
		}
	}
	if charge > limit || (budget != nil && charge > budget.TotalByteCount()) {
		self.cancel()
		return false
	}
	end := start + uint32(len(payload))
	if fin {
		end++
	}
	var waiter *transferMemoryReserveWaiter
	defer func() {
		if waiter != nil {
			waiter.abandon()
		}
	}()
	metadataCharge := ByteCount(0)
	metadataCapacity := 0
	for {
		if self.ctx.Err() != nil {
			return false
		}
		self.mutex.Lock()
		if int32(self.receiveSeqAck-end) >= 0 {
			self.mutex.Unlock()
			return true
		}
		reserved := false
		if waiter == nil {
			metadataCharge = 0
			metadataCapacity = cap(self.returnChunks)
			if self.tcpBufferSettings.MemoryBudget != nil && len(self.returnChunks) == cap(self.returnChunks) && self.returnHead != len(self.returnChunks) {
				metadataCapacity = max(4, 2*cap(self.returnChunks))
				metadataCharge = ByteCount(metadataCapacity) * ByteCount(unsafe.Sizeof(tcpReturnChunk{}))
			} else if self.tcpBufferSettings.MemoryBudget != nil && cap(self.returnChunks) == 0 {
				metadataCapacity = 4
				metadataCharge = ByteCount(metadataCapacity) * ByteCount(unsafe.Sizeof(tcpReturnChunk{}))
			}
			if self.returnByteCount+charge <= limit {
				reserved = budget == nil
				if budget != nil {
					reserved, waiter = budget.tryReserveOrQueue(charge + metadataCharge)
				}
			}
		} else {
			select {
			case <-waiter.ready:
				reserved, waiter = true, nil
			default:
			}
		}
		if reserved {
			var owned []byte
			if len(payload) != 0 {
				owned = MessagePoolCopy(payload)
			}
			if self.returnHead == len(self.returnChunks) {
				self.returnChunks = self.returnChunks[:0]
				self.returnHead = 0
				self.returnProgressTime = time.Now()
			}
			if metadataCharge > 0 {
				chunks := make([]tcpReturnChunk, len(self.returnChunks), metadataCapacity)
				copy(chunks, self.returnChunks)
				self.returnChunks = chunks
				// Growth temporarily retains both arrays. Admission paid for the
				// complete new backing before allocation; only now retire the old.
				budget.Release(self.returnMetadataByteCount)
				self.returnMetadataByteCount = metadataCharge
			}
			self.returnChunks = append(self.returnChunks, tcpReturnChunk{start: start, end: end, payload: owned, charge: charge, fin: fin})
			self.returnByteCount += charge
			self.signalReturnRecoveryWithLock()
			self.mutex.Unlock()
			return true
		}
		self.mutex.Unlock()
		var granted <-chan struct{}
		if waiter != nil {
			granted = waiter.ready
		}
		select {
		case <-self.ctx.Done():
			return false
		case <-granted:
		case <-self.returnCapacity:
		}
	}
}

func (self *TcpSequence) signalReturnRecoveryWithLock() {
	select {
	case self.returnWake <- struct{}{}:
	default:
	}
}

// Called only after the ACK was validated against the emitted sequence range.
func (self *TcpSequence) acknowledgeReturnWithLock(tcp *parsedTcp) {
	if self.returnHead == len(self.returnChunks) {
		return
	}
	ack := tcp.ackNumber
	if ack != self.receiveSeqAck {
		self.returnDuplicateAcks = 0
		self.returnAttempts = 0
		self.returnProgressTime = time.Now()
		if self.returnRecoveryActive {
			partial := int32(ack-self.returnRecoveryEnd) < 0
			if partial && !self.returnEvidence.lossProven {
				// The first advance after a timeout replay decides the episode.
				// An ACK for the original flight means the timer fired on a
				// late flight, not a loss: replaying at every later partial
				// ACK would duplicate delivered data on the slowest link.
				if self.returnTimeoutSpuriousWithLock(tcp) {
					self.returnEvidence.spuriousTimeoutCount++
					partial = false
				} else {
					self.returnEvidence.lossProven = true
				}
			}
			self.returnRecoveryReady = partial
			self.returnRecoveryActive = partial
		}
		for self.returnHead < len(self.returnChunks) {
			chunk := &self.returnChunks[self.returnHead]
			if int32(ack-chunk.end) < 0 {
				break
			}
			MessagePoolReturn(chunk.payload)
			self.returnByteCount -= chunk.charge
			if budget := self.returnMemoryBudget(); budget != nil {
				budget.Release(chunk.charge)
			}
			*chunk = tcpReturnChunk{}
			self.returnHead++
		}
		// Amortized compaction keeps metadata bounded by the active window.
		if self.returnHead > 0 && self.returnHead*2 >= len(self.returnChunks) {
			count := copy(self.returnChunks, self.returnChunks[self.returnHead:])
			clear(self.returnChunks[count:])
			self.returnChunks = self.returnChunks[:count]
			self.returnHead = 0
		}
		select {
		case self.returnCapacity <- struct{}{}:
		default:
		}
		self.signalReturnRecoveryWithLock()
	} else if self.returnDuplicateAcks < 3 && len(tcp.payload) == 0 {
		// RFC 5681: a segment carrying data is not a duplicate acknowledgement.
		self.returnDuplicateAcks++
		if self.returnDuplicateAcks == 3 {
			self.signalReturnRecoveryWithLock()
		}
	}
}

// Reports whether the first cumulative advance after a timeout replay
// acknowledges the original transmission rather than the replay (RFC 3522
// Eifel detection). The source echoes the timestamp of the segment that
// advanced its window (RFC 7323), and every original of the timed-out flight
// was stamped before the replay. Without timestamps neither an ACK beyond the
// replayed segment (a filled hole with later data already held) nor its
// timing proves which copy arrived, so the replay is presumed to have
// repaired a loss, as before.
func (self *TcpSequence) returnTimeoutSpuriousWithLock(tcp *parsedTcp) bool {
	if !self.returnEvidence.replayTsvalSet || !self.enableTimestamp || !tcp.enableTimestamp || tcp.timestampEcho == 0 {
		return false
	}
	return int32(tcp.timestampEcho-self.returnEvidence.replayTsval) < 0
}

// Recovery evidence for the provider return path. Guarded by the connection
// mutex.
type tcpReturnEvidence struct {
	// The current recovery episode. Three duplicate ACKs prove loss at
	// entry; a timeout episode is proven or undone by its first advance.
	lossProven   bool
	referenceSet bool
	// The timestamp carried by the episode's first timeout replay.
	replayTsval    uint32
	replayTsvalSet bool
	// Diagnostics: replays by trigger and undone timeout episodes.
	timeoutReplayCount   uint64
	fastReplayCount      uint64
	partialReplayCount   uint64
	spuriousTimeoutCount uint64
}

// Three duplicate ACKs or the resend timer start recovery of the current
// flight. Once loss is evidenced, each advancing partial ACK permits the next
// oldest-segment replay; waiting for a fresh timer at each hole makes a lost
// burst repair one segment per timeout. Duplicate ACKs are that
// evidence at entry. A timeout is evidence only once its first cumulative
// advance answers the replay rather than the original flight; otherwise the
// flight was late, not lost, and the episode ends without further replays.
// A fixed frontier excludes newly issued data, and a silent peer retains
// exponential backoff. The return callback runs outside the flow mutex.
func (self *TcpSequence) runReturnRecovery() {
	timer := time.NewTimer(0)
	defer timer.Stop()
	defer func() {
		self.mutex.Lock()
		self.returnPersistPending = false
		self.mutex.Unlock()
	}()
	var persistProgress time.Time
	var persistDelay time.Duration
	var persistAck uint32
	for {
		if self.ctx.Err() != nil {
			return
		}
		var packet []byte
		var persistPacket bool
		var timeout <-chan time.Time
		self.mutex.Lock()
		persistEligible := self.returnPersistPending && self.receiveWindowSize == 0 &&
			self.receiveSeqAck == self.receiveSeq && self.returnHead == len(self.returnChunks) &&
			self.tcpBufferSettings.ReturnPersistTimeout > 0
		if !persistEligible || persistAck != self.receiveSeqAck {
			persistProgress = time.Time{}
		}
		if self.returnHead < len(self.returnChunks) {
			delay := self.tcpBufferSettings.ReturnResendTimeout
			if delay <= 0 {
				delay = time.Second
			}
			delay = min(30*time.Second, min(delay, 30*time.Second)*time.Duration(1<<min(self.returnAttempts, 5)))
			remaining := time.Until(self.returnProgressTime.Add(delay))
			lossProven := self.returnRecoveryActive && self.returnEvidence.lossProven
			if remaining <= 0 && self.returnDuplicateAcks != 3 && !self.returnRecoveryReady && !lossProven &&
				0 < self.returnUnsent.Load() {
				// Without loss evidence, this flow's own packets still waiting
				// to be handed to Transfer mean the oldest byte is queued
				// locally or ahead of them, not lost, and a direct replay would
				// overtake them. Loss behind a draining backlog still produces
				// duplicate ACKs; the timer restarts once nothing of this flow
				// waits locally. A proven loss keeps its ordinary timer.
				self.returnProgressTime = time.Now()
				remaining = delay
			}
			if remaining <= 0 || self.returnDuplicateAcks == 3 || self.returnRecoveryReady {
				partialReplay := self.returnRecoveryReady
				fastReplay := !partialReplay && self.returnDuplicateAcks == 3
				timeoutReplay := !partialReplay && !fastReplay
				if !self.returnRecoveryActive {
					self.returnRecoveryEnd = self.receiveSeq
					self.returnRecoveryActive = true
					// Duplicate ACKs are loss evidence; a timeout is not yet.
					self.returnEvidence.lossProven = fastReplay
					self.returnEvidence.referenceSet = false
					self.returnEvidence.replayTsvalSet = false
				} else if fastReplay {
					self.returnEvidence.lossProven = true
				}
				switch {
				case partialReplay:
					self.returnEvidence.partialReplayCount++
				case fastReplay:
					self.returnEvidence.fastReplayCount++
				default:
					self.returnEvidence.timeoutReplayCount++
				}
				self.returnRecoveryReady = false
				chunk := &self.returnChunks[self.returnHead]
				seq := chunk.start
				if int32(self.receiveSeqAck-seq) > 0 {
					seq = self.receiveSeqAck
				}
				// The first timeout replay of an undecided episode is the
				// reference its first cumulative advance is judged against.
				if timeoutReplay && !self.returnEvidence.lossProven && !self.returnEvidence.referenceSet {
					self.returnEvidence.referenceSet = true
					if self.enableTimestamp {
						// No later than the TSval this replay carries.
						self.returnEvidence.replayTsval = self.timestampValue()
						self.returnEvidence.replayTsvalSet = true
					}
				}
				if chunk.fin {
					packet = self.tcpPacket(tcpFlagAck|tcpFlagFin, seq, nil)
				} else {
					options := 0
					if self.enableTimestamp {
						options = tcpTimestampOptionByteCount
					}
					ipHeader := Ipv4HeaderSizeWithoutExtensions
					if self.ipVersion == 6 {
						ipHeader = Ipv6HeaderSize
					}
					count := self.clampPathMtu(self.tcpBufferSettings.Mtu) - ipHeader - TcpHeaderSizeWithoutExtensions - options
					if self.peerMss != 0 {
						count = min(count, int(self.peerMss)-options)
					}
					payload := chunk.payload[uint32(seq-chunk.start):]
					payload = payload[:min(len(payload), max(1, count))]
					packet = self.tcpPacket(tcpFlagAck, seq, payload)
				}
				self.returnProgressTime = time.Now()
				self.returnAttempts++
				self.returnDuplicateAcks = 4 // one fast replay per unchanged ACK
			} else {
				timer.Reset(remaining)
				timeout = timer.C
			}
		} else if persistEligible {
			if persistProgress.IsZero() {
				persistProgress = time.Now()
				persistDelay = self.tcpBufferSettings.ReturnPersistTimeout
				persistAck = self.receiveSeqAck
			}
			remaining := time.Until(persistProgress.Add(persistDelay))
			if remaining <= 0 {
				// Match TCP's zero-window probe: one already-ACKed byte elicits
				// the peer's current window without issuing any new stream byte.
				packet = self.tcpPacket(tcpFlagAck, persistAck-1, []byte{0})
				persistPacket = true
			} else {
				timer.Reset(remaining)
				timeout = timer.C
			}
		}
		self.mutex.Unlock()
		if packet != nil {
			if persistPacket {
				self.receivePacket(packet, receiveRecoveryModeDedicatedTcpControl)
				self.mutex.Lock()
				if self.ctx.Err() == nil && self.returnPersistPending && self.receiveWindowSize == 0 &&
					self.receiveSeqAck == persistAck && self.receiveSeq == persistAck && self.returnHead == len(self.returnChunks) {
					persistProgress = time.Now()
					maxDelay := max(self.tcpBufferSettings.ReturnPersistTimeout, self.tcpBufferSettings.ReturnPersistMaxTimeout)
					if persistDelay < maxDelay {
						persistDelay += min(persistDelay, maxDelay-persistDelay)
					}
				} else {
					persistProgress = time.Time{}
				}
				self.mutex.Unlock()
				continue
			}
			self.receivePacket(packet, receiveRecoveryModeTcpSocket)
			// A socket-owned replay can wait for Transfer admission. That wait
			// is not time the replay spent in flight: restart its interval at
			// the handoff, as the next retention or ACK would.
			handedOff := time.Now()
			self.mutex.Lock()
			if self.returnProgressTime.Before(handedOff) {
				self.returnProgressTime = handedOff
			}
			self.mutex.Unlock()
			continue
		}
		select {
		case <-self.ctx.Done():
			return
		case <-self.returnWake:
		case <-timeout:
		}
	}
}

// Run calls this after all readers, callbacks and the replay worker have joined.
func (self *TcpSequence) releaseReturnChunks() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	for _, chunk := range self.returnChunks[self.returnHead:] {
		MessagePoolReturn(chunk.payload)
		if budget := self.returnMemoryBudget(); budget != nil {
			budget.Release(chunk.charge)
		}
	}
	self.returnChunks = nil
	if self.returnMetadataByteCount != 0 {
		self.returnMemoryBudget().Release(self.returnMetadataByteCount)
		self.returnMetadataByteCount = 0
	}
	self.returnHead = 0
	self.returnByteCount = 0
}

// Oldest-burst-first return scheduling across the TCP flows of one provider
// NAT.
//
// Every flow a provider egresses shares the provider's single uplink to its
// clients, and that uplink drains whatever the flows have emitted. Left alone
// this is processor sharing across every response in progress: k concurrent
// downloads each get 1/k of the uplink, every newcomer slows each transfer
// already under way, and under sustained load they all finish late together.
// That is the tail of download completion time on a busy exit.
//
// The scheduler instead serves return data oldest burst first. A burst starts
// when a flow's origin socket returns data while the flow has nothing
// unacknowledged and nothing waiting to be emitted (a response begins), and
// ends once everything the flow emitted is acknowledged by the source's inner
// TCP and nothing is waiting. A flow may emit a chunk while it is the oldest
// burst that still counts, or while no older counting burst is held and the
// unacknowledged bytes of the older counting bursts plus its own, including
// the chunk, stay within the shared allowance (ReturnScheduleByteCount). A
// younger burst never limits an older one: the oldest always proceeds at the
// pace of its own TCP window, up to the lead bound, and the allowance only
// bounds how far younger bursts overlap it. Bursts complete in arrival order
// instead of together, while the allowance keeps the uplink busy across each
// handover.
//
// The lead bound (tcpReturnScheduleLeadByteCount) caps the oldest burst's
// unacknowledged bytes. It sits above the bandwidth-delay product of a
// provider uplink, so the oldest burst still runs at the uplink's pace, but it
// bounds how much of one response queues in front of the uplink, and so how
// long the schedule takes to hand the uplink to another burst.
//
// Bulk transfers yield. A burst that has run for tcpReturnScheduleBulkTimeout
// and has already sent tcpReturnScheduleBulkByteCount is a bulk transfer (a
// download or a stream), not a page or an API response. While an interactive
// burst is active, a bulk burst stops counting against the others and keeps
// only tcpReturnScheduleBulkInFlightByteCount in flight, so one long download
// cannot hold every page of the provider's other clients behind it. Bulk
// bursts are served in arrival order whenever no interactive burst is active,
// and a bulk burst that has yielded for tcpReturnScheduleBulkYieldTimeout
// takes its place in arrival order again: bulk transfers are slowed, never
// starved.
//
// Data is held before it is emitted, on the read side: a held flow stops
// reading its origin socket, whose TCP flow control then pauses the origin.
// Held bytes have no inner sequence number yet, so the source sees no loss
// and the return recovery timer never runs for them.
//
// Two guards bound the policy:
//   - progress: a burst whose acknowledgements have not advanced for
//     ReturnScheduleStallTimeout stops counting against younger bursts, so a
//     client that stops reading, or a path that died, cannot block the other
//     clients of the same provider;
//   - age: a burst held for ReturnScheduleAgeTimeout may always keep
//     ReturnScheduleAgeByteCount in flight, so no burst starves behind a
//     sequence of older ones.
//
// Lock order: a flow's connection mutex may be held while taking the
// scheduler mutex, never the reverse. The scheduler never calls into a flow;
// it wakes a held reader through that flow's own one-slot channel, and a held
// reader waits without holding any lock, so acknowledgements never block.
type tcpReturnScheduler struct {
	byteCount    ByteCount
	stallTimeout time.Duration
	ageTimeout   time.Duration
	ageByteCount ByteCount

	mutex sync.Mutex
	// Active bursts in start order, oldest first. Starting a burst appends
	// under the mutex, which fixes the order and breaks ties.
	active []*tcpReturnScheduleFlow
	// Active flows with `waiting` set.
	waitingCount int
}

// The provider profile's allowance: enough unacknowledged data to keep the
// uplink busy while one burst hands over to the next.
const defaultProviderTcpReturnScheduleByteCount = 128 * 1024

const defaultTcpReturnScheduleStallTimeout = 1 * time.Second
const defaultTcpReturnScheduleAgeTimeout = 4 * time.Second
const defaultTcpReturnScheduleAgeByteCount = 32 * 1024

// A held flow resumes for at least this much, or what it has left, so a
// nearly spent allowance does not dribble out runt segments.
const tcpReturnScheduleMinGrantByteCount = 4 * 1024

// The most the oldest burst keeps unacknowledged (the lead bound).
const tcpReturnScheduleLeadByteCount = 256 * 1024

// A burst that has run this long and sent this much is a bulk transfer.
const tcpReturnScheduleBulkTimeout = 6500 * time.Millisecond
const tcpReturnScheduleBulkByteCount = 512 * 1024

// What a yielding bulk burst keeps in flight, and how long it yields before it
// takes its place in arrival order again.
const tcpReturnScheduleBulkInFlightByteCount = 2 * 1024
const tcpReturnScheduleBulkYieldTimeout = 20 * time.Second

// Held readers re-evaluate at least this often. Wakes follow every change to
// the in-flight sums; the re-check notices the time-based guards and is the
// safety net for a lost wake.
const tcpReturnScheduleRecheckInterval = 50 * time.Millisecond

// One flow's place in the scheduler. All fields after `wake` are guarded by
// the scheduler mutex.
type tcpReturnScheduleFlow struct {
	scheduler *tcpReturnScheduler
	// A held reader waits on this. One slot: a wake never blocks the waker
	// and is never lost.
	wake chan struct{}

	active  bool
	pending bool // read from the origin and not yet emitted
	// Held. Stays set until the flow itself resumes, which keeps younger
	// bursts behind it in the meantime.
	waiting bool
	closed  bool
	// Emitted and not yet acknowledged.
	inflight ByteCount
	// The smallest grant a waiting flow resumes for.
	need ByteCount
	// The last acknowledgement advance, or the start of the burst or of a
	// flight that follows an empty one.
	progressTime time.Time
	// When this burst was first held; zero if it never was.
	heldSince time.Time
	// When this burst started, and what it has emitted since.
	startTime     time.Time
	sentByteCount ByteCount
	// When this burst was found to be a bulk transfer; zero if it was not.
	bulkSince time.Time
	// The bulk burst has yielded for the whole bulk yield timeout.
	bulkDone bool
	// Set by classifyWithLock: a bulk burst within its yield timeout.
	bulk bool
}

// Nil when the settings disable scheduling. The guards cannot be disabled: a
// non-positive value takes its default.
func newTcpReturnScheduler(settings *TcpBufferSettings) *tcpReturnScheduler {
	if settings == nil || settings.ReturnScheduleByteCount <= 0 {
		return nil
	}
	scheduler := &tcpReturnScheduler{
		byteCount:    settings.ReturnScheduleByteCount,
		stallTimeout: settings.ReturnScheduleStallTimeout,
		ageTimeout:   settings.ReturnScheduleAgeTimeout,
		ageByteCount: settings.ReturnScheduleAgeByteCount,
	}
	if scheduler.stallTimeout <= 0 {
		scheduler.stallTimeout = defaultTcpReturnScheduleStallTimeout
	}
	if scheduler.ageTimeout <= 0 {
		scheduler.ageTimeout = defaultTcpReturnScheduleAgeTimeout
	}
	if scheduler.ageByteCount <= 0 {
		scheduler.ageByteCount = defaultTcpReturnScheduleAgeByteCount
	}
	return scheduler
}

// Nil for a nil scheduler. Every flow method treats a nil flow as unscheduled.
func (self *tcpReturnScheduler) newFlow() *tcpReturnScheduleFlow {
	if self == nil {
		return nil
	}
	return &tcpReturnScheduleFlow{scheduler: self, wake: make(chan struct{}, 1)}
}

// The progress guard: whether a burst still counts against younger ones.
func (self *tcpReturnScheduler) countsWithLock(flow *tcpReturnScheduleFlow, now time.Time) bool {
	return now.Sub(flow.progressTime) <= self.stallTimeout
}

// How much more `flow` may have in flight, given what counts against it:
// `older`, the unacknowledged bytes of the older counting bursts, whether
// there is any (`counted`), and whether one of them is held itself (`behind`),
// since an older burst resumes first. The lead bound for the oldest counting
// burst. The age guard applies last.
func (self *tcpReturnScheduler) allowanceWithLock(
	flow *tcpReturnScheduleFlow, older ByteCount, counted bool, behind bool, now time.Time,
) ByteCount {
	if !counted {
		return tcpReturnScheduleLeadByteCount - flow.inflight
	}
	allowance := self.byteCount - older - flow.inflight
	if behind {
		allowance = min(allowance, 0)
	}
	if !flow.heldSince.IsZero() && self.ageTimeout <= now.Sub(flow.heldSince) {
		allowance = max(allowance, self.ageByteCount-flow.inflight)
	}
	return allowance
}

func (self *tcpReturnScheduler) startWithLock(flow *tcpReturnScheduleFlow, now time.Time) {
	flow.active = true
	flow.progressTime = now
	flow.heldSince = time.Time{}
	flow.startTime = now
	flow.sentByteCount = 0
	flow.bulkSince = time.Time{}
	flow.bulkDone = false
	flow.bulk = false
	self.active = append(self.active, flow)
}

// Marks the bulk bursts within their yield timeout (`bulk`) and reports
// whether an interactive burst is held or still counts, which is when bulk
// bursts yield, and whether a burst became bulk just now.
func (self *tcpReturnScheduler) classifyWithLock(now time.Time) (interactive bool, yielded bool) {
	for _, flow := range self.active {
		if !flow.bulkDone && flow.bulkSince.IsZero() &&
			tcpReturnScheduleBulkTimeout <= now.Sub(flow.startTime) &&
			tcpReturnScheduleBulkByteCount <= flow.sentByteCount {
			flow.bulkSince = now
			yielded = true
		}
		if !flow.bulkSince.IsZero() && !flow.bulkDone &&
			tcpReturnScheduleBulkYieldTimeout <= now.Sub(flow.bulkSince) {
			flow.bulkDone = true
		}
		flow.bulk = !flow.bulkSince.IsZero() && !flow.bulkDone
		if !flow.bulk && (flow.waiting || self.countsWithLock(flow, now)) {
			interactive = true
		}
	}
	return interactive, yielded && interactive
}

func (self *tcpReturnScheduler) endWithLock(flow *tcpReturnScheduleFlow, now time.Time) {
	flow.active = false
	if flow.waiting {
		flow.waiting = false
		self.waitingCount--
	}
	if i := slices.Index(self.active, flow); 0 <= i {
		self.active = slices.Delete(self.active, i, i+1)
	}
	self.wakeWithLock(now)
}

// Wakes each held flow that may now emit. Called after the changes that can
// widen an allowance: an acknowledgement, a burst end or close, and a held
// burst resuming, which lets the next one follow. A woken flow stays held,
// and ahead of younger bursts, until it re-evaluates.
func (self *tcpReturnScheduler) wakeWithLock(now time.Time) {
	if self.waitingCount == 0 {
		return
	}
	interactive, _ := self.classifyWithLock(now)
	older := ByteCount(0)
	counted, behind := false, false
	for _, flow := range self.active {
		yields := flow.bulk && interactive
		need := flow.need
		var allowance ByteCount
		if yields {
			allowance = tcpReturnScheduleBulkInFlightByteCount - flow.inflight
			need = min(need, tcpReturnScheduleBulkInFlightByteCount)
		} else {
			allowance = self.allowanceWithLock(flow, older, counted, behind, now)
		}
		if flow.waiting && need <= allowance {
			select {
			case flow.wake <- struct{}{}:
			default:
			}
		}
		if !yields && self.countsWithLock(flow, now) {
			older += flow.inflight
			counted = true
			behind = behind || flow.waiting
		}
	}
}

// The origin socket returned data. Starts a burst unless one is in progress.
func (self *tcpReturnScheduleFlow) read() {
	if self == nil {
		return
	}
	scheduler := self.scheduler
	scheduler.mutex.Lock()
	defer scheduler.mutex.Unlock()
	if self.closed {
		return
	}
	self.pending = true
	if !self.active {
		scheduler.startWithLock(self, time.Now())
	}
}

// Grants up to `want` bytes to emit now and records them in flight; `inflight`
// is what the flow already has unacknowledged. Zero holds the flow, whose
// reader then waits in `wait`. The caller holds the connection mutex.
func (self *tcpReturnScheduleFlow) admit(inflight ByteCount, want int) int {
	if self == nil || want <= 0 {
		return want
	}
	scheduler := self.scheduler
	scheduler.mutex.Lock()
	defer scheduler.mutex.Unlock()
	if self.closed {
		return want
	}
	now := time.Now()
	if !self.active {
		// the reader reports each read first; this keeps a direct caller whole
		self.pending = true
		scheduler.startWithLock(self, now)
	}
	self.inflight = max(0, inflight)
	interactive, yielded := scheduler.classifyWithLock(now)
	if yielded {
		// a burst that now yields leaves room to the bursts it held
		scheduler.wakeWithLock(now)
	}
	var grant, need ByteCount
	if self.bulk && interactive {
		// a yielding bulk burst keeps a trickle in flight, in small grants
		grant = min(ByteCount(want), tcpReturnScheduleBulkInFlightByteCount-self.inflight)
		need = min(ByteCount(want), tcpReturnScheduleBulkInFlightByteCount)
	} else {
		older := ByteCount(0)
		counted, behind := false, false
		for _, flow := range scheduler.active {
			if flow == self {
				break
			}
			if !(flow.bulk && interactive) && scheduler.countsWithLock(flow, now) {
				older += flow.inflight
				counted = true
				behind = behind || flow.waiting
			}
		}
		grant = min(ByteCount(want), scheduler.allowanceWithLock(self, older, counted, behind, now))
		need = min(ByteCount(want), tcpReturnScheduleMinGrantByteCount)
	}
	if grant < need {
		if self.heldSince.IsZero() {
			self.heldSince = now
		}
		if self.inflight == 0 {
			// held with nothing in flight, the flow waits on the schedule
			// rather than on its client, so the progress guard does not run
			self.progressTime = now
		}
		self.need = need
		if !self.waiting {
			self.waiting = true
			scheduler.waitingCount++
		}
		return 0
	}
	if self.inflight == 0 {
		// a new flight: the progress guard measures from here
		self.progressTime = now
	}
	self.inflight += grant
	self.sentByteCount += grant
	if self.waiting {
		self.waiting = false
		scheduler.waitingCount--
		// the next held burst may follow within what is left
		scheduler.wakeWithLock(now)
	}
	return int(grant)
}

// The source's acknowledgement advanced; `inflight` is what remains
// unacknowledged. The caller holds the connection mutex.
func (self *tcpReturnScheduleFlow) acknowledged(inflight ByteCount) {
	if self == nil {
		return
	}
	scheduler := self.scheduler
	scheduler.mutex.Lock()
	defer scheduler.mutex.Unlock()
	if self.closed {
		return
	}
	previous := self.inflight
	self.inflight = max(0, inflight)
	if !self.active {
		return
	}
	now := time.Now()
	self.progressTime = now
	if self.inflight == 0 && !self.pending {
		scheduler.endWithLock(self, now)
	} else if self.inflight < previous {
		scheduler.wakeWithLock(now)
	}
}

// Everything read from the origin so far has been emitted.
func (self *tcpReturnScheduleFlow) drained() {
	if self == nil {
		return
	}
	scheduler := self.scheduler
	scheduler.mutex.Lock()
	defer scheduler.mutex.Unlock()
	if self.closed {
		return
	}
	self.pending = false
	if self.active && self.inflight == 0 {
		scheduler.endWithLock(self, time.Now())
	}
}

// The flow's reader is done and its burst no longer counts. Idempotent.
func (self *tcpReturnScheduleFlow) close() {
	if self == nil {
		return
	}
	scheduler := self.scheduler
	scheduler.mutex.Lock()
	defer scheduler.mutex.Unlock()
	if self.closed {
		return
	}
	self.closed = true
	self.pending = false
	if self.active {
		scheduler.endWithLock(self, time.Now())
	}
}

// A held reader waits here, holding no lock, until it is woken or the
// periodic re-check is due. Cancellation of `ctx` returns false at once.
func (self *tcpReturnScheduleFlow) wait(ctx context.Context, timer **time.Timer) bool {
	if self == nil {
		return ctx.Err() == nil
	}
	timeout := resetOrCreateTimer(timer, tcpReturnScheduleRecheckInterval)
	select {
	case <-ctx.Done():
		(*timer).Stop()
		return false
	case <-self.wake:
		(*timer).Stop()
		return true
	case <-timeout:
		return true
	}
}
