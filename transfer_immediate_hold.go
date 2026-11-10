// Immediate protocol receipts own already-received positions in the same
// sequence. DATA hold promises cover only predecessors still missing.
package connect

// Counts real holes without reserving DATA memory for secured protocol ranges.
// Pending receipts and other generations cannot grant admission credit.
func (self *ReceiveSequence) heldDataMissingBelow(number uint64, heldBelow int) int64 {
	missing := heldMissingBelow(number, self.nextSequenceNumber, heldBelow)
	history := self.immediateHistory
	if missing <= 0 || history == nil {
		return missing
	}
	history.stateLock.Lock()
	defer history.stateLock.Unlock()
	if history.sequenceId != self.sequenceId {
		return missing
	}
	for _, span := range history.ranges {
		if span == nil || !span.secured || span.pending || span.last < self.nextSequenceNumber || span.first >= number {
			continue
		}
		first := max(span.first, self.nextSequenceNumber)
		last := min(span.last, number-1)
		if first > last {
			continue
		}
		// Clamp before converting: a terminal range can span more ordinals
		// than fit in a signed count, but can never make missing negative.
		if last-first >= uint64(missing-1) {
			return 0
		}
		missing -= int64(last - first + 1)
	}
	return missing
}
