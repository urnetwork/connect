// Bounded FIX accessor reads only actual current-owner markers under the
// queue lock. Independent pending backing storage does not exist.
package connect

import "slices"

func pendingEvictionStateForTest(sequence *SendSequence) pendingEvictionTestState {
	queue := sequence.resendQueue
	queue.stateLock.Lock()
	defer queue.stateLock.Unlock()
	state := pendingEvictionTestState{
		pending:  sequence.pendingEvictionPhases,
		inFlight: sequence.pendingEvictionInFlight,
	}
	for _, item := range queue.messageIdItems {
		if item.pendingEvictionPhases != 0 {
			state.numbers = append(state.numbers, item.sequenceNumber)
		}
	}
	if sequence.detachedAck.active && sequence.detachedAck.pendingEvictionPhases != 0 {
		state.numbers = append(state.numbers, sequence.detachedAck.sequenceNumber)
		state.detached = true
	}
	slices.Sort(state.numbers)
	return state
}
