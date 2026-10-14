// A batch gives unrelated already-read flows one ready offer before waiting.
// It owns no goroutine or packet copy; continuations live only until return.
package connect

import "time"

// Policy preparation belongs to one synchronous input group, never a flow
// cache. A refused group remains caller-owned while this bounded scope waits.
type ipPacketGroupAdmission struct {
	prepared   bool
	probing    bool
	stop       bool
	local      bool
	parsed     *parsedPacketGroup
	firstOffer time.Time
}

// The pending slice is bounded by the caller's existing group count. SMTP and
// fragment callers finish it at their original ordering boundaries.
type ipPacketGroupBatch struct {
	timeout   time.Duration
	readyPass bool
	send      func(*ipPacketGroup, time.Duration) bool
	complete  func(*ipPacketGroup, bool)
	pending   []*ipPacketGroup
	blocked   map[ipPacketFlowKey]bool
}

// Takes the complete input group; complete transfers or returns its owners.
// Later bounded fragments of one exact flow cannot overtake its pending head.
func (self *ipPacketGroupBatch) offer(group *ipPacketGroup) {
	if !self.readyPass {
		self.complete(group, self.send(group, self.timeout))
		return
	}
	admission := &ipPacketGroupAdmission{}
	group.batchAdmission = admission
	key, _ := ipPacketFlowKeyFromPath(group.ipPath)
	if !self.blocked[key] {
		admission.firstOffer = time.Now()
		admission.probing = true
		success := self.send(group, 0)
		admission.probing = false
		if success {
			self.finish(group, true)
			return
		}
		if admission.stop || admission.prepared && !admission.local && admission.parsed == nil {
			// A policy refusal has no queue continuation.
			self.finish(group, false)
			return
		}
	}
	if self.blocked == nil {
		self.blocked = map[ipPacketFlowKey]bool{}
	}
	self.blocked[key] = true
	self.pending = append(self.pending, group)
}

// Each group retains its own original budget. A later same-flow group starts
// only after its predecessor's final disposition, as on the original path.
func (self *ipPacketGroupBatch) finishPending() {
	for _, group := range self.pending {
		admission := group.batchAdmission
		remaining := self.timeout
		if !admission.firstOffer.IsZero() && 0 < remaining {
			remaining = max(time.Duration(0), remaining-time.Since(admission.firstOffer))
		}
		self.finish(group, self.send(group, remaining))
	}
	clear(self.pending)
	self.pending = self.pending[:0]
	clear(self.blocked)
}

// Observer recovery belongs to this exact input owner, not a later call.
// Clear the continuation before returning or transferring the caller's roots.
func (self *ipPacketGroupBatch) finish(group *ipPacketGroup, success bool) {
	if admission := group.batchAdmission; admission != nil && admission.parsed != nil {
		if observations := admission.parsed.admissionObservations; observations != nil {
			observations.complete(success)
		}
	}
	group.batchAdmission = nil
	self.complete(group, success)
}
