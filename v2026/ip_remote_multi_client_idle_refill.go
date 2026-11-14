// The idle refill hold (server CPUOPT4.md, the no-refill lever; owner,
// 2026-11-05).
//
// A hosted device stays installed until its host reaps it for having no user
// traffic (the proxy service's idle timeout, 45 minutes), and for all of that
// time its below-target discovery windows refill: every expansion step lists
// providers, mints an identity, provides, dials and pings a candidate. A
// window that lost an exit, or one whose location has no selectable
// provider, does that on every step of its backoff for as long as the device
// is held, whether or not anyone uses it. On Main at 17:20Z on 2026-11-05,
// 22,200 hosted devices were live and 3,268 had moved user traffic in the
// last ten minutes, while the quality windows built about 48 candidates a
// second and 2,181 of them had no selectable provider to fill from.
//
// With IdleRefillHold:
//   - A discovery window whose device has had no user activity for
//     IdleRefillHold builds nothing in its expansion step and sends no balance
//     probe, and its enumerator lists and mints nothing.
//   - The device's next user activity wakes the resize pass, which finds the
//     activity and expands at once: the user waits for that one expansion,
//     exactly what they wait today when they arrive while it runs.
//   - A window refills freely for IdleRefillHold after it is created, so a new
//     device forms before its first packet, as before.
//   - The clients a held window keeps, their pings, health checks and contract
//     retries, the expansion backoff and every detection bound are unchanged,
//     and nothing is disconnected. A held window that loses an exit stays
//     smaller until the user is back.
//
// Activity is the host's record when it keeps one (UserActivitySince), and
// the device's last user packet otherwise. A hosted device's tun sends its
// own upkeep through the device, such as the DoH keepalive pings it sends
// every 30 s while a resolver connection is pooled (up to 15 minutes after
// the last lookup), so its packets alone would keep an idle device looking
// busy. The host signals new activity with NoteUserActivity.
//
// Apps leave IdleRefillHold off: their windows refill whether or not the user
// is sending. Fixed destinations are unaffected.
package connect

import (
	"time"
)

// The host's opt-in, on a discovery window.
func (self *multiClientWindow) idleRefillHoldEnabled() bool {
	if self == nil || self.settings == nil || self.settings.IdleRefillHold <= 0 || self.generator == nil {
		return false
	}
	if _, fixed := self.generator.FixedDestinationSize(); fixed {
		return false
	}
	return true
}

// Whether the window's refill is held at `now`: its device has had no user
// activity in the last IdleRefillHold, and the window is older than that.
// While held, the device's next user activity wakes the resize pass. A window
// without a parent (a bare fixture) reads as active, so its refill is never
// held.
func (self *multiClientWindow) idleRefillHeld(now time.Time) bool {
	if !self.idleRefillHoldEnabled() {
		return false
	}
	since := now.Add(-self.settings.IdleRefillHold)
	// a new window refills freely for its first hold
	if since.Before(self.idleRefillSince) {
		return false
	}
	if self.userSentSince(since) {
		self.idleRefillWake.Store(false)
		return false
	}
	// armed before the re-check: from here the parent's send path and the
	// host's activity signal wake the pass
	self.idleRefillWake.Store(true)
	// activity recorded before the wake was armed still ends the hold
	if self.userSentSince(since) {
		self.idleRefillWake.Store(false)
		return false
	}
	return true
}

// Whether the enumerator must list and mint nothing for the window now.
func (self *multiClientWindow) idleRefillListingHeld() bool {
	return self.idleRefillHeld(time.Now())
}

// The parent's activity paths, after a user packet or the host's activity
// signal: a held window's resize pass wakes, finds the activity, and expands.
// Lock-free while the window is not held.
func (self *multiClientWindow) wakeIdleRefill() {
	if self.idleRefillWake.CompareAndSwap(true, false) && self.resizeMonitor != nil {
		self.resizeMonitor.NotifyAll()
	}
}
