// The idle trim (server CPUOPT6.md Part B #1; owner, 2026-11-15).
//
// A discovery window whose device is idle holds its refill (the idle refill
// hold, ip_remote_multi_client_idle_refill.go), but until the host reaps the
// device it kept every exit it had, with the exits' pings, transports and
// contract retries. On Main in the sweeps of 2026-11-15 about 36k of 48k live
// hosted devices had moved no user traffic in ten minutes, and they held an
// estimated 68-90k of the 147k window clients, which kept every proxy process
// at its Go memory limit.
//
// With the idle trim, a window whose device has had no user activity for the
// idle trim time keeps one exit and releases the others:
//   - The idle trim time is the longer of IdleRefillHold and the device's
//     sequence idle timeouts (SequenceIdleTimeout, TcpSequenceIdleTimeout). By
//     then every flow a user opened has sat idle for longer than its sequence
//     may, so no user flow can still be pinned to a released exit. The time
//     derives from those settings and adds none of its own.
//   - The exit kept is a selectable one, one carrying flows first, then the one
//     the capacity collapse would keep (collapseKeepOrder).
//   - An exit still carrying a flow is never released. The device's own upkeep
//     (the DoH keepalive pings its tun sends every 30 s while a resolver
//     connection is pooled) can keep a flow live past the trim time; its exit
//     stays until the flow ends, and a later pass releases it.
//   - The device's next user activity wakes the resize pass through the hold's
//     wake, and the pass refills the window to its target at once. Until the
//     refill lands, a returning user rides the kept exit.
//   - Every ping, health check, contract retry, timeout and detection bound is
//     unchanged, and so are the host's own holds on the device.
//
// The trim reverses, for windows idle past the trim time only, the hold's
// "nothing is disconnected" (owner, 2026-11-15). Apps leave IdleRefillHold off,
// so their windows never trim. Fixed destinations are unaffected.
package connect

import (
	"slices"
	"time"
)

// The idle trim time: the longer of IdleRefillHold and the sequence idle
// timeouts. Zero when the window does not hold its refill.
func (self *multiClientWindow) idleTrimTimeout() time.Duration {
	if !self.idleRefillHoldEnabled() {
		return 0
	}
	timeout := self.settings.IdleRefillHold
	if reliabilitySettings := self.reliabilitySettings(); reliabilitySettings != nil {
		timeout = max(timeout, reliabilitySettings.SequenceIdleTimeout, reliabilitySettings.TcpSequenceIdleTimeout)
	}
	return timeout
}

// Whether the window keeps one exit at `now`: its device has had no user
// activity for the idle trim time, and the window is older than that. A
// window without a parent (a bare fixture) reads as active, so it never trims.
// The hold's wake is armed by the same pass (idleRefillHeld) once the trim
// leaves the window below its target.
func (self *multiClientWindow) idleTrimDue(now time.Time) bool {
	timeout := self.idleTrimTimeout()
	if timeout <= 0 {
		return false
	}
	since := now.Add(-timeout)
	// a new window keeps its exits for its first trim time
	if since.Before(self.idleRefillSince) {
		return false
	}
	return !self.userSentSince(since)
}

// Keeps one exit of a window whose idle trim is due and releases each other
// exit that carries no flow, returning the released ones. `clients` are the
// pass's selectable clients and `warnedClients` its warned ones; `weights` and
// `durations` are the pass's collapse inputs. Must be called with no window or
// channel lock held: it reads flow counts (see flowCount).
func (self *multiClientWindow) idleTrim(
	now time.Time,
	clients []*multiClientChannel,
	warnedClients []*multiClientChannel,
	weights map[*multiClientChannel]float32,
	durations map[*multiClientChannel]time.Duration,
	release func(*multiClientChannel),
) []*multiClientChannel {
	if len(clients)+len(warnedClients) <= 1 || !self.idleTrimDue(now) {
		return nil
	}
	since := now.Add(-self.idleTrimTimeout())

	clientFlowCounts := map[*multiClientChannel]int{}
	selectableClients := map[*multiClientChannel]bool{}
	orderedClients := make([]*multiClientChannel, 0, len(clients)+len(warnedClients))
	for _, client := range clients {
		selectableClients[client] = true
		clientFlowCounts[client] = self.flowCount(client)
		orderedClients = append(orderedClients, client)
	}
	for _, client := range warnedClients {
		clientFlowCounts[client] = self.flowCount(client)
		orderedClients = append(orderedClients, client)
	}
	// the exit kept is first: selectable, then carrying flows, then as the
	// capacity collapse keeps
	slices.SortStableFunc(orderedClients, func(a *multiClientChannel, b *multiClientChannel) int {
		if selectableClients[a] != selectableClients[b] {
			if selectableClients[a] {
				return -1
			}
			return 1
		}
		if aFlows, bFlows := 0 < clientFlowCounts[a], 0 < clientFlowCounts[b]; aFlows != bFlows {
			if aFlows {
				return -1
			}
			return 1
		}
		return collapseKeepOrder(a, b, weights, durations)
	})

	releasedClients := []*multiClientChannel{}
	for _, client := range orderedClients[1:] {
		// never cut a flow; a later pass releases the exit once it ends
		if 0 < clientFlowCounts[client] {
			continue
		}
		// user activity since the decision ends the trim, and the same pass
		// refills the window
		if self.userSentSince(since) {
			break
		}
		// a flow bound since the counts were read keeps its exit
		if 0 < self.flowCount(client) {
			continue
		}
		release(client)
		releasedClients = append(releasedClients, client)
	}
	if 0 < len(releasedClients) && self.log.V(1).Enabled() {
		self.log.Infof("[multi]window idle trim -%d ->%d\n", len(releasedClients), len(orderedClients)-len(releasedClients))
	}
	return releasedClients
}
