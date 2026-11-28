package connect

import (
	"sync/atomic"
	"time"
)

// The first terminal owner decision of each send sequence is counted for the
// process by reason (GetSendSequenceExitCounts), and described by one bounded
// metadata-only diagnostic at V(1). The diagnostic was an INFO line until
// 2026-11-15, about 900 lines a second on the services edges (server
// CPUOPT6 A #2(b)); the counts keep its rate by reason with verbose logging
// off. An idle exit is the ordinary end of a quiet sequence: it is neither
// logged nor counted.

// The reasons recordSendSequenceExit's callers pass, other than idle. A reason
// not listed is counted as sendSequenceExitReasonOther, so the counts stay a
// fixed set.
var sendSequenceExitReasons = [...]string{
	"run_return",
	"packs_closed",
	"context",
	"ack_worker_exit",
	"ack_lifetime",
	"retained_silence",
	"contract_canceled",
	"contract_creation",
	"head_rewrite",
}

const sendSequenceExitReasonOther = "other"

// one per sendSequenceExitReasons entry, then other
var sendSequenceExitCounts [len(sendSequenceExitReasons) + 1]atomic.Uint64

// GetSendSequenceExitCounts returns the process's send sequence exits since it
// started, by the reason of each sequence's first terminal owner decision.
// Every reason is present, zero or not, and other counts any reason not in the
// fixed set. Idle exits are not counted.
func GetSendSequenceExitCounts() map[string]uint64 {
	counts := make(map[string]uint64, len(sendSequenceExitCounts))
	for i, reason := range sendSequenceExitReasons {
		counts[reason] = sendSequenceExitCounts[i].Load()
	}
	counts[sendSequenceExitReasonOther] = sendSequenceExitCounts[len(sendSequenceExitReasons)].Load()
	return counts
}

// Generic callback errors intentionally retain their old contract. No packet
// history, payload, timer or additional queue is retained. Only the send owner
// calls this, including its synchronous pacing/route-write waits. With V(1)
// off nothing past the count is read, so the ACK window's lock is not taken.
func (self *SendSequence) recordSendSequenceExit(reason string, item *sendItem, deadline time.Time, cause error) {
	if self.exitDiagnosticWritten {
		return
	}
	self.exitDiagnosticWritten = true
	if reason == "idle" {
		return
	}
	reasonIndex := len(sendSequenceExitReasons)
	for i, knownReason := range sendSequenceExitReasons {
		if knownReason == reason {
			reasonIndex = i
			break
		}
	}
	sendSequenceExitCounts[reasonIndex].Add(1)
	if self.log == nil || self.client == nil {
		return
	}
	verbose := self.log.V(1)
	if !verbose.Enabled() {
		return
	}
	now := time.Now()
	if item == nil && len(self.ackLifetimes.items) > 0 {
		entry := self.ackLifetimes.items[0]
		item, deadline = entry.item, entry.at
	}
	pending := 0
	if self.resendQueue != nil {
		pending = self.resendQueue.Len()
	}
	var message Id
	var number uint64
	var sends int
	var lifetimeStart, writeStarted int64
	var lifetime time.Duration
	var selective, written, reliable, unreliable, carrierChanged, retained bool
	if item != nil {
		message, number, sends = item.messageId, item.sequenceNumber, item.sendCount
		// This is the start of the route attempt, not proof of a successful
		// P2P/SCTP physical write. The route-written bit is separate evidence.
		// SACKs may renew sendTime; this is the current lifetime anchor,
		// deliberately not labelled the original application offer time.
		lifetimeStart, writeStarted = item.sendTime.UnixNano(), item.pacingSentAtNanos
		lifetime = item.ackTimeout
		selective, written = item.selectiveAcked, item.transportWriteObserved
		reliable, unreliable, carrierChanged = item.reliableCarrierObserved, item.unreliableCarrierObserved, item.carrierChanged
		retained = item.acks.retainPastAckTimeout()
	}
	var headNumber uint64
	var headAt int64
	var headSet bool
	var pendingHead uint64
	var pendingSacks, pendingContracts int
	if self.ackWindow != nil {
		self.ackWindow.ackLock.Lock()
		headSet, headNumber, headAt = self.ackWindow.hasHeadAck, self.ackWindow.headAck.sequenceNumber, self.ackWindow.headAck.receivedAtNanos
		pendingHead = uint64(self.ackWindow.ackUpdateCount)
		pendingSacks, pendingContracts = len(self.ackWindow.selectiveAcks), len(self.ackWindow.contractMissingAcks)
		self.ackWindow.ackLock.Unlock()
	}
	deadlineNanos := int64(0)
	if !deadline.IsZero() {
		deadlineNanos = deadline.UnixNano()
	}
	var contextErr, parentErr error
	if self.ctx != nil {
		contextErr = self.ctx.Err()
	}
	if self.client.ctx != nil {
		parentErr = self.client.ctx.Err()
	}
	verbose.Infof("[s]event=sequence_exit reason=%s client=%s destination=%s sequence=%s stream=%s ctx=%v parent_ctx=%v cause=%v pending=%d message=%s number=%d sends=%d lifetime_start_ns=%d write_started_ns=%d lifetime_ns=%d deadline_ns=%d observed_ns=%d selective=%t route_written=%t reliable=%t unreliable=%t carrier_changed=%t retained=%t head_set=%t head_number=%d head_received_ns=%d pending_head=%d pending_sacks=%d pending_contracts=%d\n",
		reason, self.client.ClientTag(), self.destination, self.sequenceId, self.contractMultiRouteWriterAlias.StreamId,
		contextErr, parentErr, cause, pending, message, number, sends, lifetimeStart, writeStarted,
		lifetime, deadlineNanos, now.UnixNano(), selective, written, reliable, unreliable, carrierChanged, retained,
		headSet, headNumber, headAt, pendingHead, pendingSacks, pendingContracts)
}
