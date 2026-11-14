package connect

// ip_remote_multi_client_exit_stall.go -- what a window does about an exit
// that has stopped making progress.
//
// An exit stops making progress in two ways the owner's logs show
// (2026-11-13): it takes packets and never acknowledges them, and every flow
// on it waits for the send-ack lifetime (MultiClientSettings.AckTimeout); or
// it delivers return items past one it never delivers, and every flow on it
// waits behind the hole until the receive sequence's GapTimeout. Both used to
// cost up to 30 s and 60 s; the apps now run 12 s and 15 s (sdk
// app_window_exit_timers.go), and three things kept piling onto such an exit:
//
//   - Placement. New flows and affinity donation kept choosing the exit while
//     it was stalled. An exit whose outstanding sends have made no ack progress
//     for ReliabilitySettings.SendStallTimeout, or whose return stream has held
//     a hole open for ReliabilitySettings.ReceiveGapStallTimeout, is left out of
//     the window's offer (orderedClients), of affinity donation
//     (affinityDonorEligible, adoptAffinityClientWithLock) and of the
//     replacements a dying exit's flows rebind to (rebindCandidates). It is
//     back the moment progress resumes: an ack restarts the stall clock and a
//     filled hole clears the gap, and nothing is latched. The last-resort
//     offer still holds it, so a window whose every exit is stalled keeps
//     placing flows.
//   - The hole. A receive gap that outlives GapTimeout ends its sequence, and
//     the stream behind it never resumes on that sequence. The Client's notice
//     (Client.receiveGapTimedOut) now errors the channel, and the resize pass
//     removes it, so its flows re-home at once.
//   - Restore. A restart restored an exit that was already stalling before
//     the kill (PROXYDRAIN1.md §3.5). The window keeps the identity of an exit
//     that is stalled, quarantined or warned on health evidence out of the
//     snapshot a restart restores (noteIdentityRestore), and puts it back once
//     the exit recovers. An exit removed with a verdict leaves the snapshot at
//     the removal, before its asynchronous retirement.
//
// The guard against churn. These rules act only on an exit that has stopped
// making progress, never on one that is merely slow: an ack, however late,
// restarts the stall clock, and a lossy but working stream fills its holes
// well inside the bars. They act only once the window is warm: the tunnel has
// received return traffic within the uplink freshness bar
// (ReliabilitySettings.UplinkStalenessGate), so the silence is the exit's and
// not ours. A cold window -- a window still forming, or a phone between
// networks -- places, keeps and restores every exit as before. The send-ack
// lifetime itself already renews on every advance of the cumulative head
// (SendSequence.ackProgressDeadline), so it too ends only a stopped exit.

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Why an exit has stopped making progress, for placement and restore.
type exitStallKind int

const (
	exitStallNone exitStallKind = iota
	// outstanding sends with no ack progress past SendStallTimeout
	exitStallAck
	// a receive hole open past ReceiveGapStallTimeout
	exitStallGap
)

// The error a channel ends with when a receive hole from its exit outlived
// GapTimeout in a warm window. Not a "Blackhole " verdict: the storm breaker
// budgets soft evidence, and a stream frozen behind a hole is not soft.
var errReceiveGapTimeout = errors.New("receive gap timeout")

// Whether a removal's error is the receive gap eviction.
func receiveGapTimeoutErr(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), errReceiveGapTimeout.Error())
}

// Whether a tunnel whose last return traffic arrived at lastIngress is warm at
// now: inside the uplink freshness bar (UplinkStalenessGate, else the ack
// freshness interval), so that one exit's silence is evidence about that exit.
// A tunnel that has received nothing yet is cold.
func windowWarmAt(lastIngress time.Time, now time.Time, reliabilitySettings *ReliabilitySettings) bool {
	if lastIngress.IsZero() {
		return false
	}
	freshness := reliabilitySettings.UplinkStalenessGate
	if freshness <= 0 {
		freshness = blackholeAckFreshnessInterval
	}
	return now.Sub(lastIngress) < freshness
}

// windowWarmAt over the parent's ingress stamp, for the placement reads that
// reach a channel without its window (affinity donation and adoption,
// rebinding). Lock-free; nil wiring (bare fixtures) reads as cold.
func (self *multiClientChannel) windowWarm(now time.Time) bool {
	if self.uplinkLastIngressFunc == nil {
		return false
	}
	return windowWarmAt(self.uplinkLastIngressFunc(), now, self.reliabilitySettings())
}

// windowWarmAt over the parent's ingress stamp, read once for a whole offer or
// restore pass. Lock-free; nil wiring (bare fixtures) reads as cold.
func (self *multiClientWindow) windowWarm(now time.Time) bool {
	if self.uplinkLastIngressFunc == nil {
		return false
	}
	return windowWarmAt(self.uplinkLastIngressFunc(), now, self.reliabilitySettings())
}

// Whether this exit has stopped making progress, and how, in a window the
// caller judged warm: outstanding sends with no ack progress for
// SendStallTimeout, measured from the start of the unacked run (a busy-probe
// answer is not delivery), or a receive hole open for ReceiveGapStallTimeout.
// A cold window holds no exit stalled. A channel whose own carrier is down is
// not stalled: its silence is ours, and sendStalled rebases its clock for the
// outage. Takes this channel's stateLock, the client's gap registry leaf lock
// and the route manager's lock, one at a time; callable with the parent
// stateLock held, never with this channel's.
func (self *multiClientChannel) exitStall(now time.Time, warm bool) exitStallKind {
	if !warm || self.dataOnlyProviderProbe {
		return exitStallNone
	}
	reliabilitySettings := self.reliabilitySettings()
	ackStallTimeout := reliabilitySettings.SendStallTimeout
	gapStallTimeout := reliabilitySettings.ReceiveGapStallTimeout
	if ackStallTimeout <= 0 && gapStallTimeout <= 0 {
		return exitStallNone
	}
	if 0 < ackStallTimeout {
		ackStalled := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return 0 < self.packetStats.sendNackCount &&
				!self.pendingSendTime.IsZero() &&
				ackStallTimeout <= now.Sub(self.pendingSendTime)
		}()
		if ackStalled && self.hasActiveTransport() {
			return exitStallAck
		}
	}
	if 0 < gapStallTimeout && self.client != nil {
		if gapSince := self.client.oldestReceiveGap(); !gapSince.IsZero() && gapStallTimeout <= now.Sub(gapSince) {
			return exitStallGap
		}
	}
	return exitStallNone
}

// Whether new flows and affinity donation must avoid this exit now, judging
// the window's warmth from the parent's ingress stamp. See exitStall.
func (self *multiClientChannel) placementStalled(now time.Time) bool {
	return self.exitStall(now, self.windowWarm(now)) != exitStallNone
}

// Whether a restart must not restore this exit: it has stopped making progress
// in a window the caller judged warm (exitStall, which covers a held
// send-stall verdict), it is quarantined, or the resize pass warned it on
// health evidence. Rotation warnings (draining, capacity, a family swap, a
// service refresh) are not evidence against the exit and keep its restore.
// Same locking as exitStall.
func (self *multiClientChannel) restoreUnsafe(now time.Time, warm bool) bool {
	if self.exitStall(now, warm) != exitStallNone {
		return true
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.quarantined {
		return true
	}
	if self.warning {
		switch self.warnCause {
		case warnUnhealthy, warnStarved, warnSilent:
			return true
		}
	}
	return false
}

// The Client's notice that a receive hole from this exit outlived GapTimeout
// and ended its sequence (Client.receiveGapTimedOut): every flow behind it is
// frozen. In a warm window the channel ends with errReceiveGapTimeout and the
// resize pass removes it, so its flows re-home now. In a cold window the
// silence may be ours, and the exit is kept. Runs on the receive sequence's
// worker with no lock held.
func (self *multiClientChannel) receiveGapTimedOut(source TransferPath) {
	if self.IsDone() {
		return
	}
	gapTimeout := time.Duration(0)
	if self.client != nil && self.client.settings != nil && self.client.settings.ReceiveBufferSettings != nil {
		gapTimeout = self.client.settings.ReceiveBufferSettings.GapTimeout
	}
	if !self.windowWarm(time.Now()) {
		loggerOrDefault(self.log).Infof("%s\n", relEvent(
			"gap_timeout",
			"exit", self.ClientId(),
			"action", "hold",
			"detail", "no return traffic in the tunnel",
			"timeout", gapTimeout,
		))
		return
	}
	// the removal line (event=removal) counts the flows; reading them here
	// would take the parent lock on a transfer worker
	self.addError(fmt.Errorf("%w: the return stream from %s stayed frozen for %s", errReceiveGapTimeout, source.SourceId, gapTimeout))
	loggerOrDefault(self.log).Infof("%s\n", relEvent(
		"gap_timeout",
		"exit", self.ClientId(),
		"action", "evict",
		"timeout", gapTimeout,
	))
	self.resizeWake()
}

// An optional generator capability: leave a window identity out of the snapshot
// a restart restores, and put it back (PROXYDRAIN1.md §3.5). The window calls
// it on each change of an exit's restore safety (noteIdentityRestore). The API
// generator implements it.
type multiClientGeneratorIdentityRestore interface {
	setIdentityRestorable(args *MultiClientGeneratorClientArgs, restorable bool)
}

// Keeps the identity of each of clients out of the restore snapshot while its
// exit is restore unsafe (restoreUnsafe), and puts it back when the exit
// recovers. Only changes reach the generator, so the store is written on a
// transition and never on a pass. The stall watchdog and the resize pass both
// call it; identityRestoreLock orders them. Called with no lock held.
func (self *multiClientWindow) noteIdentityRestore(now time.Time, clients []*multiClientChannel) {
	restore, ok := self.generator.(multiClientGeneratorIdentityRestore)
	if !ok {
		return
	}
	self.identityRestoreLock.Lock()
	defer self.identityRestoreLock.Unlock()
	warm := self.windowWarm(now)
	for _, client := range clients {
		if client == nil || client.args == nil || client.IsDone() {
			continue
		}
		unsafe := client.restoreUnsafe(now, warm)
		if client.restoreUnsafeNoted.Load() == unsafe {
			continue
		}
		client.restoreUnsafeNoted.Store(unsafe)
		restore.setIdentityRestorable(&client.args.MultiClientGeneratorClientArgs, !unsafe)
	}
}

// Leaves a client's identity out of the restore snapshot when the window
// removes it with a verdict, at the removal and not at its asynchronous
// retirement, which a kill can outrun. A cancellation or a transport loss says
// nothing against the exit and changes nothing. Called with no lock held.
func (self *multiClientWindow) forgetIdentityRestore(client *multiClientChannel, err error) {
	if client == nil || client.args == nil || windowRemovalCauseOf(err) != windowRemovalVerdict {
		return
	}
	restore, ok := self.generator.(multiClientGeneratorIdentityRestore)
	if !ok {
		return
	}
	self.identityRestoreLock.Lock()
	defer self.identityRestoreLock.Unlock()
	client.restoreUnsafeNoted.Store(true)
	restore.setIdentityRestorable(&client.args.MultiClientGeneratorClientArgs, false)
}
