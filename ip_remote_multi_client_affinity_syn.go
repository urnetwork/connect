// Age and positive health gate a SYN's reuse of an existing selected client.
// Ordinary data and established same-ISN replay retain their exact owner.
package connect

import "time"

// SYN payload is smaller than half the sequence space. Reject malformed bounds
// instead of interpreting an underflow as permission for almost any ACK.
func tcpSynAckInRange(sequence, end, ack uint32) bool {
	base := sequence + 1
	span := end - base
	return span < 1<<31 && ack-base <= span
}

// A newly raced generation cannot establish from a blind or stale first packet.
// Use only source-dequeued SYN state, never the pre-offer dial-failure record:
// a refused larger TFO retry must not enlarge first-answer authority.
func (self *multiClientChannelUpdate) acceptsAffinityIngress(client *multiClientChannel, control tcpControlObservation) bool {
	if !self.affinitySources.renewal || self.receivedInbound.Load() {
		return true
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.receivedInbound.Load() {
		return true
	}
	if !control.valid || !control.ack || !control.syn && !control.rst {
		return false
	}
	if client == self.client.Load() && self.synGenerationSeen {
		return tcpSynAckInRange(self.synGenerationNumber, self.synGenerationEnd, control.ackSequenceNumber)
	}
	if self.race != nil {
		if candidate := self.race.clientStates[client]; candidate != nil && candidate.collapseControl.synSeen {
			state := &candidate.collapseControl
			return tcpSynAckInRange(state.synNumber, state.synEnd, control.ackSequenceNumber)
		}
	}
	return false
}

// This is a placement snapshot, not a migration timer. Only an actual outbound
// client SYN can ask to renew an expired or unhealthy existing binding.
func (self *RemoteUserNatMultiClient) reuseAffinitySyn(
	update *multiClientChannelUpdate,
	client *multiClientChannel,
	path *IpPath,
) bool {
	if update == nil || path == nil || path.Protocol != IpProtocolTcp || !path.Syn || path.Ack {
		return true
	}
	update.stateLock.Lock()
	seen, sequence, established := update.synGenerationSeen, update.synGenerationNumber, update.receivedInbound.Load()
	update.stateLock.Unlock()
	if established && seen && sequence == path.SequenceNumber {
		// Identical-ISN replay cannot be distinguished from a new connection
		// reusing that ISN without an observed close. Do not migrate a live flow.
		return true
	}
	if client == nil {
		// A fresh unbound update already races. A bound-client policy must not
		// replace every cold SYN before its first real provider admission.
		return true
	}
	selection := update.client.loadSelection()
	if selection == nil || selection.client != client || !client.hasActiveTransport() {
		return false
	}
	client.stateLock.Lock()
	defer client.stateLock.Unlock()
	if update.client.loadSelection() != selection || client.IsDone() || !client.healthy ||
		client.warning || client.quarantined || client.endErr != nil || !client.supportsIpVersion(path.Version) {
		return false
	}
	select {
	case <-client.clientDone():
		return false
	default:
	}
	return update.pinned || selection.eligible(client, time.Now(), self.reliabilitySettings().AffinityMaxTime)
}

// Wrap the existing lookup/bookkeeping rather than replacing normal packet
// routing. The new update is private until source admission commits its parent
// reservation. All-refused exits restore the incumbent and caller ownership.
func (self *RemoteUserNatMultiClient) sendAffinityClientPath(
	group *parsedPacketGroup,
	timeout time.Duration,
	callback func(*multiClientChannelUpdate, *multiClientChannel, time.Duration),
) {
	if group.isSynRetry() {
		self.sendOwnedSynRetryPath(group, timeout, callback)
		return
	}
	self.sendClientPath(group.ipPath, group.pin, func(update *multiClientChannelUpdate, current *multiClientChannel) {
		if self.sendClientPathForTest != nil || len(group.packets) == 0 || group.packets[0].ipPath == nil ||
			self.reuseAffinitySyn(update, current, group.packets[0].ipPath) {
			group.affinitySynReuse = self.sendClientPathForTest == nil && current != nil &&
				len(group.packets) != 0 && group.packets[0].ipPath != nil &&
				group.packets[0].ipPath.Protocol == IpProtocolTcp && group.packets[0].ipPath.Syn && !group.packets[0].ipPath.Ack
			callback(update, current, timeout)
			return
		}
		// A pending transition may not retain a tuple forever, including when
		// the public caller otherwise permits an indefinite ordinary send.
		limit := self.settings.WindowGeneratorTimeout
		if limit <= 0 {
			limit = 20 * time.Second
		}
		if timeout >= 0 {
			limit = min(limit, timeout)
		}
		deadline := time.Now().Add(max(limit, time.Nanosecond))
		// Zero-wait admission still needs a live reservation while callbacks
		// race its synchronous offer; it does not wait for the stated ceiling.
		if timeout == 0 {
			deadline = time.Now().Add(20 * time.Second)
		}
		transition := self.reserveAffinityTransition(update, group.ipPath, group.pin, deadline)
		if transition == nil {
			return
		}
		group.affinitySynReuse = false
		group.affinityTransition.Store(transition)
		defer func() {
			group.affinityTransition.CompareAndSwap(transition, nil)
			transition.finish()
		}()
		callback(transition.next, nil, limit)
		if transition.admitted.Load() {
			self.bindClientFlow(transition.next, transition.next.client.Load())
		}
	})
}
