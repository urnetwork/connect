package connect

// resident_move.go — the client side of server-directed connection moves
// (server/connect/MOVE.md §5.1, §5.2).
//
// The platform asks a client to move with the same `ResidentMigrate` frame a
// drain has always sent. A drain (kind 0) migrates by name, exactly as before.
// A move names the edge to use: an advisory move is guidance the client may
// decline, and a compulsory move says the platform closes the old transport at
// its close time. The client builds a replacement transport whose direct dials
// go to the move's target address of the dial's family while the platform
// hostname stays the TLS server name and the HTTP host, so certificate
// verification is unchanged and a wrong or hostile target can only fail a dial.
// Make-before-break is kept: the old transport closes only after the
// replacement has arrived.
//
// Three objects carry a move:
//   - `ResidentMove` is one parsed frame.
//   - `ResidentMoveTracker` is one client's memory of moves: the move ids it
//     has seen, when a move last completed, the targets that failed and the
//     targets it prefers for reconnects (D10). The transports of one client,
//     and every generation an owner migrates to, share one tracker through
//     `PlatformTransportSettings.ResidentMoveTracker`.
//   - `ResidentMoveDial` is one move's plan for the replacement: the targets
//     each family dials, whether the dials are strict (an advisory move dials
//     only its targets) and what the dials have done. The replacement's
//     transports read it through `PlatformTransportSettings.ResidentMoveDial`.
//
// A move redirects only the platform host's edges. A carrier an alt url
// redirects -- plain H3, and the DNS carriers, on every space with an alt
// deployment (EXTENDER.md L1, L4) -- keeps dialing alt: neither a move's
// targets nor a preference apply to it (transport_move.go). Such a transport
// is still moved, over H1, which always dials the platform host; only a
// failure in the carrier its old transport used counts against a target, so
// an H1 refusal on a client that ran on H3 through alt times the move out
// instead of holding the target.
//
// An advisory move is declined (D11) when no transport can use a target (an
// extender or VLESS relay carries it, it is a DNS carrier, or no target matches
// its family), when its target failed within `MoveTargetFailHold`, when a move
// completed within `MoveClientMinInterval`, when the owner's `MoveDecline` hook
// declines it (the sdk on a backgrounded or metered mobile device), when a move
// is already in progress, or when the memory budget cannot hold the replacement
// beside the old transport. A compulsory move is never declined: a path that
// cannot use a target migrates by name. Every decline and every failure sends
// one `ResidentMoveResult` to the control id, best effort.
//
// Stickiness (D10): a transport that registers through a move target records a
// preference for that target, keyed by its platform hostname and the target's
// family, for the move's prefer duration. Later dials to that hostname and
// family try the preferred target first; a dial that fails clears the
// preference so the next dial resolves the name again, and a newer move
// replaces it. Preferences live in memory only.
//
// Concurrency: `ResidentMoveTracker` and `ResidentMoveDial` are safe for
// concurrent use. `ResidentMove` and `ResidentMovePlan` are immutable after
// construction.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"time"

	quic "github.com/quic-go/quic-go"

	"github.com/urnetwork/connect/protocol"
)

// The move version this client honors, declared on the H1 header
// `HeaderResidentMoveVersion` and in `Auth.resident_move_version`. 1: the
// client dials a move's target while keeping the platform hostname, reports
// declines and failures, and prefers the target for reconnects.
const ResidentMoveVersion = 1

// The H1 form of `Auth.resident_move_version`.
const HeaderResidentMoveVersion = "X-UR-ResidentMoveVersion"

// One `ResidentMigrate` as the client acts on it.
type ResidentMove struct {
	// the platform's id for the move, nil for a drain migrate and for a frame
	// whose id is malformed. A move without an id migrates by name.
	MoveId *Id
	Kind   protocol.ResidentMoveKind
	// when the client begins the move
	MigrateTime time.Time
	// when the platform closes the old transport of a compulsory move. Zero for
	// every other kind.
	CloseTime time.Time
	// how long a target the client moved to stays preferred for reconnects to the
	// platform hostname. Zero records no preference.
	PreferDuration time.Duration
	// at most one per family, each a global unicast address. A family repeated in
	// the frame keeps its first address.
	TargetIps []netip.Addr
}

// The legacy migrate: same hostname at `migrateTime`.
func NewDrainResidentMove(migrateTime time.Time) *ResidentMove {
	return &ResidentMove{
		Kind:        protocol.ResidentMoveKind_ResidentMoveKindDrain,
		MigrateTime: migrateTime,
	}
}

// Reads a `ResidentMigrate` frame. Fields a drain does not carry decode to
// their zero values, which is the drain behavior. Targets that are not a 4 or
// 16 byte global unicast address are dropped, as are repeats of a family, so
// the message can never point a client at a loopback, link-local, multicast or
// unspecified address.
func NewResidentMove(message *protocol.ResidentMigrate) *ResidentMove {
	move := &ResidentMove{
		Kind:        message.GetKind(),
		MigrateTime: time.UnixMilli(int64(message.GetMigrateTime())),
	}
	if moveIdBytes := message.GetMoveId(); 0 < len(moveIdBytes) {
		if moveId, err := IdFromBytes(moveIdBytes); err == nil {
			move.MoveId = &moveId
		}
	}
	if move.Kind == protocol.ResidentMoveKind_ResidentMoveKindCompulsory && 0 < message.GetCloseTime() {
		move.CloseTime = time.UnixMilli(int64(message.GetCloseTime()))
	}
	move.PreferDuration = time.Duration(message.GetPreferDuration()) * time.Millisecond
	if move.PreferDuration < 0 {
		move.PreferDuration = 0
	}
	seenFamilies := map[int]bool{}
	for _, target := range message.GetTargets() {
		ip, ok := netip.AddrFromSlice(target.GetIp())
		if !ok {
			continue
		}
		ip = ip.Unmap()
		if !ip.IsGlobalUnicast() {
			continue
		}
		ipFamily := netipAddrFamily(ip)
		if seenFamilies[ipFamily] {
			continue
		}
		seenFamilies[ipFamily] = true
		move.TargetIps = append(move.TargetIps, ip)
	}
	return move
}

// Whether the frame is a move that names targets: an advisory or compulsory
// move with an id. Everything else, including a kind this build does not know,
// migrates by name.
func (self *ResidentMove) Moving() bool {
	if self == nil || self.MoveId == nil {
		return false
	}
	switch self.Kind {
	case protocol.ResidentMoveKind_ResidentMoveKindAdvisory,
		protocol.ResidentMoveKind_ResidentMoveKindCompulsory:
		return true
	default:
		return false
	}
}

// Whether the frame is an advisory move, which the client may decline.
func (self *ResidentMove) Advisory() bool {
	return self.Moving() && self.Kind == protocol.ResidentMoveKind_ResidentMoveKindAdvisory
}

// What a transport of `ipFamily` may dial: the target of its family for a
// pinned transport, and for a family-agnostic one every target the control
// family policy permits, ordered v6 first for the race.
func (self *ResidentMove) targetIpsForFamily(ipFamily int) []netip.Addr {
	targetIps := []netip.Addr{}
	for _, targetIp := range self.TargetIps {
		targetFamily := netipAddrFamily(targetIp)
		switch ipFamily {
		case 4, 6:
			if targetFamily != ipFamily {
				continue
			}
		default:
			switch ControlIpFamilyPolicy() {
			case IpFamilyForce4:
				if targetFamily != 4 {
					continue
				}
			case IpFamilyForce6:
				if targetFamily != 6 {
					continue
				}
			}
		}
		targetIps = append(targetIps, targetIp)
	}
	return orderDialAddrs(targetIps)
}

// 4 or 6 for a valid address.
func netipAddrFamily(ip netip.Addr) int {
	if ip.Unmap().Is4() {
		return 4
	}
	return 6
}

// One stickiness preference: the platform hostname a transport dials and the
// family of the target that stands in for it.
type residentMoveTargetKey struct {
	host     string
	ipFamily int
}

type residentMoveTargetPreference struct {
	targetIp netip.Addr
	endTime  time.Time
}

// One client's memory of the platform's moves. Every transport of the client
// and every generation an owner migrates to shares one, so a decline rule sees
// the whole client and a reconnect of any transport sees the preference a move
// left. It holds the most recent move ids, the time the last move completed,
// the targets that failed, the stickiness preferences and the edges a
// placement chose (platform_placement.go). Placements are kept apart from
// preferences: a transport without moves still dials its placement, and a
// move's preference outranks it. The failures are keyed by target address,
// which the platform draws from its edge inventory, and pruned past the
// longest fail hold asked about. Memory only. Safe for concurrent use.
type ResidentMoveTracker struct {
	stateLock sync.Mutex
	// the most recent move ids, oldest first, for dedupe
	recentMoveIds   []Id
	lastMoveTime    time.Time
	targetFailTimes map[netip.Addr]time.Time
	// the longest fail hold asked about. Zero prunes nothing.
	targetFailHold    time.Duration
	targetPreferences map[residentMoveTargetKey]residentMoveTargetPreference
	targetPlacements  map[residentMoveTargetKey]residentMoveTargetPreference
}

func NewResidentMoveTracker() *ResidentMoveTracker {
	return &ResidentMoveTracker{
		targetFailTimes:   map[netip.Addr]time.Time{},
		targetPreferences: map[residentMoveTargetKey]residentMoveTargetPreference{},
		targetPlacements:  map[residentMoveTargetKey]residentMoveTargetPreference{},
	}
}

// Reports whether `moveId` is new, remembering it among the last `dedupeCount`
// ids. A non-positive count remembers nothing and admits every id, which leaves
// only the owner's move-in-progress guard against a duplicate.
func (self *ResidentMoveTracker) AdmitMoveId(moveId Id, dedupeCount int) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if dedupeCount <= 0 {
		return true
	}
	if slices.Contains(self.recentMoveIds, moveId) {
		return false
	}
	self.recentMoveIds = append(self.recentMoveIds, moveId)
	if excess := len(self.recentMoveIds) - dedupeCount; 0 < excess {
		self.recentMoveIds = slices.Delete(self.recentMoveIds, 0, excess)
	}
	return true
}

// Records that a move completed: a replacement arrived at its target.
func (self *ResidentMoveTracker) NoteMoved(now time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lastMoveTime = now
}

// Reports whether a move completed less than `interval` ago. A non-positive
// interval is never within.
func (self *ResidentMoveTracker) MovedWithin(interval time.Duration, now time.Time) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if interval <= 0 || self.lastMoveTime.IsZero() {
		return false
	}
	return now.Sub(self.lastMoveTime) < interval
}

// Records that a dial to `targetIp` failed.
func (self *ResidentMoveTracker) NoteTargetFailed(targetIp netip.Addr, now time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.pruneTargetFailuresWithLock(now)
	self.targetFailTimes[targetIp.Unmap()] = now
}

// Reports whether `targetIp` failed less than `hold` ago. A non-positive hold
// is never within.
func (self *ResidentMoveTracker) TargetFailedWithin(targetIp netip.Addr, hold time.Duration, now time.Time) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if hold <= 0 {
		return false
	}
	self.targetFailHold = max(self.targetFailHold, hold)
	failTime, ok := self.targetFailTimes[targetIp.Unmap()]
	return ok && now.Sub(failTime) < hold
}

// Drops failures older than the longest hold any caller asked about, so the map
// holds only the targets that failed within one hold.
func (self *ResidentMoveTracker) pruneTargetFailuresWithLock(now time.Time) {
	if self.targetFailHold <= 0 {
		return
	}
	for targetIp, failTime := range self.targetFailTimes {
		if self.targetFailHold <= now.Sub(failTime) {
			delete(self.targetFailTimes, targetIp)
		}
	}
}

// Records that `host` tries each of `targetIps` first for dials of the target's
// family until `endTime`. A newer preference of a family replaces the older
// one.
func (self *ResidentMoveTracker) preferTargets(host string, targetIps []netip.Addr, endTime time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	keepTargetsWithLock(self.targetPreferences, host, targetIps, endTime)
}

// The live preferences of `host` for a transport of `ipFamily`: the one of its
// family when pinned, and every family's when family-agnostic, ordered for the
// race. Expired preferences are dropped.
func (self *ResidentMoveTracker) preferredTargetIps(host string, ipFamily int, now time.Time) []netip.Addr {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return liveTargetIpsWithLock(self.targetPreferences, host, ipFamily, now)
}

// Clears the preferences of `host` that point at one of `targetIps`, after a
// dial to them failed, so the next dial resolves the name. A preference a newer
// move already replaced is kept.
func (self *ResidentMoveTracker) dropPreferredTargets(host string, targetIps []netip.Addr) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	dropTargetsWithLock(self.targetPreferences, host, targetIps)
}

// Records that `host` dials each of `targetIps`, the edge a placement chose,
// for dials of the target's family until `endTime` (platform_placement.go). A
// newer placement of a family replaces the older one.
func (self *ResidentMoveTracker) placeTargets(host string, targetIps []netip.Addr, endTime time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.targetPlacements == nil {
		self.targetPlacements = map[residentMoveTargetKey]residentMoveTargetPreference{}
	}
	keepTargetsWithLock(self.targetPlacements, host, targetIps, endTime)
}

// The live placements of `host` for a transport of `ipFamily`, as
// `preferredTargetIps` reads preferences.
func (self *ResidentMoveTracker) placedTargetIps(host string, ipFamily int, now time.Time) []netip.Addr {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return liveTargetIpsWithLock(self.targetPlacements, host, ipFamily, now)
}

// Clears the placements of `host` that point at one of `targetIps`, after a
// dial to them failed. A placement a newer one already replaced is kept.
func (self *ResidentMoveTracker) dropPlacedTargets(host string, targetIps []netip.Addr) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	dropTargetsWithLock(self.targetPlacements, host, targetIps)
}

// Keeps each of `targetIps` for `host` and the target's family until
// `endTime`, replacing what the family kept before.
func keepTargetsWithLock(
	targets map[residentMoveTargetKey]residentMoveTargetPreference,
	host string,
	targetIps []netip.Addr,
	endTime time.Time,
) {
	for _, targetIp := range targetIps {
		targets[residentMoveTargetKey{
			host:     host,
			ipFamily: netipAddrFamily(targetIp),
		}] = residentMoveTargetPreference{
			targetIp: targetIp.Unmap(),
			endTime:  endTime,
		}
	}
}

// The live targets kept for `host` and a transport of `ipFamily`: the one of
// its family when pinned, and every family's when family-agnostic, ordered for
// the race. Expired targets are dropped.
func liveTargetIpsWithLock(
	targets map[residentMoveTargetKey]residentMoveTargetPreference,
	host string,
	ipFamily int,
	now time.Time,
) []netip.Addr {
	targetIps := []netip.Addr{}
	for _, targetFamily := range []int{4, 6} {
		if ipFamily != 0 && ipFamily != targetFamily {
			continue
		}
		key := residentMoveTargetKey{host: host, ipFamily: targetFamily}
		target, ok := targets[key]
		if !ok {
			continue
		}
		if !now.Before(target.endTime) {
			delete(targets, key)
			continue
		}
		targetIps = append(targetIps, target.targetIp)
	}
	return orderDialAddrs(targetIps)
}

// Clears the kept targets of `host` that are one of `targetIps`. A target a
// newer one already replaced is kept.
func dropTargetsWithLock(
	targets map[residentMoveTargetKey]residentMoveTargetPreference,
	host string,
	targetIps []netip.Addr,
) {
	for _, targetIp := range targetIps {
		key := residentMoveTargetKey{host: host, ipFamily: netipAddrFamily(targetIp)}
		if target, ok := targets[key]; ok && target.targetIp == targetIp.Unmap() {
			delete(targets, key)
		}
	}
}

// A live platform transport as a move sees it. `*PlatformTransport` implements
// it.
type ResidentMoveTransport interface {
	// 4 or 6 for a pinned transport, 0 for a family-agnostic one
	IpFamily() int
	IsConnected() bool
	// the elected carrier, TransportModeNone while nothing is connected
	ActiveMode() TransportMode
	// a live connection reaches the platform through an extender or a VLESS relay,
	// which resolves the platform name itself
	RelayCarried() bool
}

// One family's part of a move's dial: what the replacement transport of that
// family dials, and how its outcome counts.
type residentMoveDialFamily struct {
	targetIps []netip.Addr
	// the carrier the old transport of this family used. Only a failure in this
	// mode is evidence about the target, since a carrier the old transport did not
	// use may be blocked on this network. TransportModeNone counts a failure in
	// any mode.
	decisiveMode TransportMode
	// the old transport of this family was connected and is being moved. Only
	// targeted families decide that a strict move failed.
	targeted bool
}

// The first target dial failure of a move that counts.
type ResidentMoveFailure struct {
	Reason protocol.ResidentMoveReason
	// 4 or 6, 0 when not family specific
	IpFamily int
	// from the start of the failing dial to its failure
	Elapsed time.Duration
}

// How a move's replacement resolved.
type ResidentMoveResolution int

const (
	// a strict move arrived at its target, or a move that is not strict connected,
	// through its target or by name
	ResidentMoveResolutionConnected ResidentMoveResolution = iota
	// every targeted family of a strict move failed
	ResidentMoveResolutionFailed
	ResidentMoveResolutionTimedOut
	ResidentMoveResolutionCanceled
)

// The replacement a move waits on: one platform transport, or a group of them.
type ResidentMoveReplacement interface {
	ConnectedNotify() <-chan struct{}
	IsConnected() bool
}

// One move's plan for its replacement transports, handed to them through
// `PlatformTransportSettings.ResidentMoveDial`. Each transport dials the
// targets of its family. A strict dial (an advisory move) dials only its
// targets, skips relays and holds the carriers that cannot use a target, so any
// connection it makes is an arrival at the target; a dial that is not strict (a
// compulsory move) tries its targets once per carrier and then dials by name.
// The owner waits on it and concludes it, after which every transport dials as
// it normally does. Safe for concurrent use.
type ResidentMoveDial struct {
	moveId         Id
	strict         bool
	preferDuration time.Duration
	tracker        *ResidentMoveTracker
	// immutable after construction
	families map[int]*residentMoveDialFamily

	stateLock      sync.Mutex
	concluded      bool
	arrived        bool
	failedFamilies map[int]bool
	failure        *ResidentMoveFailure
	// notified in the locked scope of every change above
	changeMonitor *Monitor
}

func (self *ResidentMoveDial) MoveId() Id {
	return self.moveId
}

// Whether the dial is target only (an advisory move).
func (self *ResidentMoveDial) Strict() bool {
	return self.strict
}

// What a transport of `ipFamily` dials for this move, and whether the dial is
// strict. False once the move concluded or when the move has no target for the
// family.
func (self *ResidentMoveDial) targetIpsFor(ipFamily int) ([]netip.Addr, bool, bool) {
	if self == nil {
		return nil, false, false
	}
	family := self.families[ipFamily]
	if family == nil || len(family.targetIps) == 0 {
		return nil, false, false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.concluded {
		return nil, false, false
	}
	return family.targetIps, self.strict, true
}

// Whether a transport of `ipFamily` must not dial a carrier that cannot use a
// target (a DNS carrier, or any carrier through an extender) while the move is
// strict, with a channel that closes on the next change.
func (self *ResidentMoveDial) holdsCarrier(ipFamily int) (bool, chan struct{}) {
	if self == nil {
		return false, nil
	}
	family := self.families[ipFamily]
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.strict && !self.concluded && family != nil && 0 < len(family.targetIps),
		self.changeMonitor.NotifyChannel()
}

// Records that a transport registered through its targets. The transport's
// hostname prefers the targets for the move's prefer duration.
func (self *ResidentMoveDial) noteArrived(host string, targetIps []netip.Addr, now time.Time) {
	arrived := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.concluded {
			return false
		}
		if !self.arrived {
			self.arrived = true
			self.changeMonitor.NotifyAll()
		}
		return true
	}()
	if arrived && 0 < self.preferDuration && self.tracker != nil {
		self.tracker.preferTargets(host, targetIps, now.Add(self.preferDuration))
	}
}

// Records a failed dial of a transport of `ipFamily` to its targets in `mode`.
// A failure of a moved family in its decisive mode is the move's failure for
// that family: the targets are held for the fail hold, and the first such
// failure is the one the move reports.
func (self *ResidentMoveDial) noteFailed(
	ipFamily int,
	mode TransportMode,
	targetIps []netip.Addr,
	reason protocol.ResidentMoveReason,
	elapsed time.Duration,
	now time.Time,
) {
	family := self.families[ipFamily]
	// a transport that was not moved may lack a path its old counterpart
	// never proved, so its failure says nothing about the target
	if family == nil || !family.targeted {
		return
	}
	if family.decisiveMode != TransportModeNone && family.decisiveMode != mode {
		return
	}
	decisive := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.concluded || self.arrived {
			return false
		}
		if self.failure == nil {
			failureFamily := ipFamily
			if failureFamily == 0 && len(targetIps) == 1 {
				failureFamily = netipAddrFamily(targetIps[0])
			}
			self.failure = &ResidentMoveFailure{
				Reason:   reason,
				IpFamily: failureFamily,
				Elapsed:  elapsed,
			}
		}
		self.failedFamilies[ipFamily] = true
		self.changeMonitor.NotifyAll()
		return true
	}()
	if decisive && self.tracker != nil {
		for _, targetIp := range targetIps {
			self.tracker.NoteTargetFailed(targetIp, now)
		}
	}
}

// Whether a replacement transport registered through a target.
func (self *ResidentMoveDial) Arrived() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.arrived
}

// The first target dial failure that counts, if any.
func (self *ResidentMoveDial) Failure() (ResidentMoveFailure, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.failure == nil {
		return ResidentMoveFailure{}, false
	}
	return *self.failure, true
}

// Whether every targeted family failed.
func (self *ResidentMoveDial) failedWithLock() bool {
	targetedCount := 0
	for ipFamily, family := range self.families {
		if !family.targeted {
			continue
		}
		targetedCount += 1
		if !self.failedFamilies[ipFamily] {
			return false
		}
	}
	return 0 < targetedCount
}

// Ends the move's hold on the dials. From now on every transport dials as it
// normally does: by name, or through a preference an arrival left.
func (self *ResidentMoveDial) Conclude() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.concluded {
		return
	}
	self.concluded = true
	self.changeMonitor.NotifyAll()
}

// Resolves the replacement: a strict move waits for an arrival at a target or
// for every targeted family to fail, and a move that is not strict waits for
// the replacement to connect. A non-positive timeout waits until `ctx` ends.
func (self *ResidentMoveDial) Wait(
	ctx context.Context,
	replacement ResidentMoveReplacement,
	timeout time.Duration,
) ResidentMoveResolution {
	var timeoutChannel <-chan time.Time
	if 0 < timeout {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		timeoutChannel = timer.C
	}
	for {
		// subscribe immediately before the reads
		connectedNotify := replacement.ConnectedNotify()
		resolution, resolved, change := func() (ResidentMoveResolution, bool, chan struct{}) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			change := self.changeMonitor.NotifyChannel()
			if self.strict {
				if self.arrived {
					return ResidentMoveResolutionConnected, true, change
				}
				if self.failedWithLock() {
					return ResidentMoveResolutionFailed, true, change
				}
			}
			return 0, false, change
		}()
		if resolved {
			return resolution
		}
		if !self.strict && replacement.IsConnected() {
			return ResidentMoveResolutionConnected
		}
		select {
		case <-ctx.Done():
			return ResidentMoveResolutionCanceled
		case <-timeoutChannel:
			return ResidentMoveResolutionTimedOut
		case <-change:
		case <-connectedNotify:
		}
	}
}

// The decision for one move over the transports of one client.
type ResidentMovePlan struct {
	Move *ResidentMove
	// Declined is an advisory move the client declines with `DeclineReason`.
	// Nothing is built.
	Declined        bool
	DeclineReason   protocol.ResidentMoveReason
	DeclineIpFamily int
	// SameName migrates by name: a drain migrate, a client that does not honor
	// moves, or a compulsory move no transport can use a target for.
	SameName bool
	// Dial is the replacement's dial when the move uses targets.
	Dial *ResidentMoveDial
}

// Decides a move for the live transports of one client (MOVE.md §5.2). Every
// connected transport whose carrier can use a target -- direct, not a DNS
// carrier -- and that has a target of its family is moved. The replacement's
// other transports also dial the targets of their family, but only the moved
// ones decide that a strict move failed. The call reads the transports and the
// tracker, never blocks, and calls the settings' `MoveDecline` hook, which must
// not block either.
func PlanResidentMove(
	move *ResidentMove,
	transports []ResidentMoveTransport,
	settings *PlatformTransportSettings,
	tracker *ResidentMoveTracker,
	now time.Time,
) *ResidentMovePlan {
	plan := &ResidentMovePlan{Move: move}
	if move == nil || settings == nil || !settings.EnableMove || !move.Moving() {
		plan.SameName = true
		return plan
	}
	if tracker == nil {
		tracker = NewResidentMoveTracker()
	}
	advisory := move.Advisory()
	eligibleTargetIps := func(ipFamily int) ([]netip.Addr, bool) {
		targetIps := move.targetIpsForFamily(ipFamily)
		if !advisory || len(targetIps) == 0 {
			return targetIps, false
		}
		eligible := []netip.Addr{}
		for _, targetIp := range targetIps {
			if !tracker.TargetFailedWithin(targetIp, settings.MoveTargetFailHold, now) {
				eligible = append(eligible, targetIp)
			}
		}
		return eligible, len(eligible) == 0
	}

	families := map[int]*residentMoveDialFamily{}
	relayCarried := false
	dnsCarried := false
	targetFailed := false
	targetFailedIpFamily := 0
	for _, transport := range transports {
		if transport == nil || !transport.IsConnected() {
			continue
		}
		ipFamily := normalizeIpFamily(transport.IpFamily())
		if transport.RelayCarried() {
			relayCarried = true
			continue
		}
		mode := transport.ActiveMode()
		if mode == TransportModeH3Dns || mode == TransportModeH3DnsPump {
			dnsCarried = true
			continue
		}
		targetIps, held := eligibleTargetIps(ipFamily)
		if held {
			targetFailed = true
			targetFailedIpFamily = ipFamily
			continue
		}
		if len(targetIps) == 0 {
			continue
		}
		decisiveMode := TransportModeNone
		if mode == TransportModeH1 || mode == TransportModeH3 {
			decisiveMode = mode
		}
		families[ipFamily] = &residentMoveDialFamily{
			targetIps:    targetIps,
			decisiveMode: decisiveMode,
			targeted:     true,
		}
	}

	if len(families) == 0 {
		if !advisory {
			plan.SameName = true
			return plan
		}
		plan.Declined = true
		switch {
		case relayCarried:
			plan.DeclineReason = protocol.ResidentMoveReason_ResidentMoveReasonExtenderCarrier
		case dnsCarried:
			plan.DeclineReason = protocol.ResidentMoveReason_ResidentMoveReasonDnsCarrier
		case targetFailed:
			plan.DeclineReason = protocol.ResidentMoveReason_ResidentMoveReasonTargetFailedRecently
			plan.DeclineIpFamily = targetFailedIpFamily
		default:
			plan.DeclineReason = protocol.ResidentMoveReason_ResidentMoveReasonNoFamilyTarget
		}
		return plan
	}
	if advisory {
		if tracker.MovedWithin(settings.MoveClientMinInterval, now) {
			plan.Declined = true
			plan.DeclineReason = protocol.ResidentMoveReason_ResidentMoveReasonRecentMove
			return plan
		}
		if settings.MoveDecline != nil && settings.MoveDecline() {
			plan.Declined = true
			plan.DeclineReason = protocol.ResidentMoveReason_ResidentMoveReasonMobileBackgroundOrMetered
			return plan
		}
	}
	// the replacement's transports that were not moved -- a family whose old
	// transport was not connected, the standby of a provider group -- dial the
	// targets of their family too, so whichever of them connects lands on the
	// target edge, but their outcome decides nothing
	for _, ipFamily := range []int{0, 4, 6} {
		if families[ipFamily] != nil {
			continue
		}
		if targetIps, _ := eligibleTargetIps(ipFamily); 0 < len(targetIps) {
			families[ipFamily] = &residentMoveDialFamily{
				targetIps:    targetIps,
				decisiveMode: TransportModeNone,
			}
		}
	}
	plan.Dial = &ResidentMoveDial{
		moveId:         *move.MoveId,
		strict:         advisory,
		preferDuration: move.PreferDuration,
		tracker:        tracker,
		families:       families,
		failedFamilies: map[int]bool{},
		changeMonitor:  NewMonitor(),
	}
	return plan
}

// Reports a declined or failed move to the platform control. Best effort: the
// frame is offered without waiting and a refused offer is dropped, since a lost
// report only means the platform waits for its move intent to expire. Never
// blocks, so a receive callback may call it.
func SendResidentMoveResult(client *Client, result *protocol.ResidentMoveResult) bool {
	if client == nil || result == nil {
		return false
	}
	frame, err := ToFrame(result, client.settings.ProtocolVersion)
	if err != nil {
		return false
	}
	if !client.SendControlWithTimeout(frame, nil, 0) {
		MessagePoolReturn(frame.MessageBytes)
		return false
	}
	return true
}

// The report of a declined move.
func NewResidentMoveDeclined(
	moveId Id,
	reason protocol.ResidentMoveReason,
	ipFamily int,
) *protocol.ResidentMoveResult {
	return &protocol.ResidentMoveResult{
		MoveId:   moveId.Bytes(),
		Outcome:  protocol.ResidentMoveOutcome_ResidentMoveOutcomeDeclined,
		Reason:   reason,
		IpFamily: uint32(max(0, ipFamily)),
	}
}

// The report of a move whose replacement failed.
func NewResidentMoveFailed(moveId Id, failure ResidentMoveFailure) *protocol.ResidentMoveResult {
	return &protocol.ResidentMoveResult{
		MoveId:   moveId.Bytes(),
		Outcome:  protocol.ResidentMoveOutcome_ResidentMoveOutcomeFailed,
		Reason:   failure.Reason,
		IpFamily: uint32(max(0, failure.IpFamily)),
		Elapsed:  uint64(max(0, failure.Elapsed.Milliseconds())),
	}
}

// A relay dialer refused for a strict move dial: a relay resolves the platform
// name itself, so it cannot reach the target.
var errResidentMoveRelay = errors.New("a move target is dialed direct only")

// A dial whose network permits no target of the move: the pin, or the control
// family policy, rules out every family the move named.
var errResidentMoveNoFamilyTarget = errors.New("no move target of the dial family")

// Carries one transport dial's move targets to the dial layers the transport
// does not own, the same route the pinned family takes (transport_family.go).
type moveDialOverrideContextKey struct{}

// Stands the target addresses in for the platform hostname of one dial. The
// hostname stays the TLS server name and the HTTP host. A strict override also
// refuses relay dialers.
type moveDialOverride struct {
	host      string
	targetIps []netip.Addr
	strict    bool
}

func withMoveDialOverride(ctx context.Context, override *moveDialOverride) context.Context {
	if override == nil {
		return ctx
	}
	return context.WithValue(ctx, moveDialOverrideContextKey{}, override)
}

func moveDialOverrideFromContext(ctx context.Context) *moveDialOverride {
	if ctx == nil {
		return nil
	}
	override, _ := ctx.Value(moveDialOverrideContextKey{}).(*moveDialOverride)
	return override
}

// Whether a dial to `address` (host or host:port) is the one the override
// stands in for.
func (self *moveDialOverride) matches(address string) bool {
	if self == nil {
		return false
	}
	host := address
	if splitHost, _, err := net.SplitHostPort(address); err == nil {
		host = splitHost
	}
	return normalizeInternalDohName(host) == normalizeInternalDohName(self.host)
}

// The targets a (possibly narrowed) network can dial.
func (self *moveDialOverride) targetIpsForNetwork(network string) []netip.Addr {
	return dialAddrsMatchNetwork(network, self.targetIps)
}

// The H3 candidates for a transport of `ipFamily` on `port`.
func (self *moveDialOverride) udpAddrs(ipFamily int, port int) []*net.UDPAddr {
	udpAddrs := []*net.UDPAddr{}
	for _, targetIp := range self.targetIps {
		if ipFamily != 0 && netipAddrFamily(targetIp) != ipFamily {
			continue
		}
		udpAddrs = append(udpAddrs, &net.UDPAddr{IP: net.IP(targetIp.AsSlice()), Port: port})
	}
	return udpAddrs
}

// Dials the override's targets for `address` through `dial`, racing them when
// the network permits more than one.
func (self *moveDialOverride) dialTargets(
	ctx context.Context,
	network string,
	address string,
	dial DialContextFunction,
) (net.Conn, error) {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	targetIps := self.targetIpsForNetwork(network)
	switch len(targetIps) {
	case 0:
		return nil, errResidentMoveNoFamilyTarget
	case 1:
		return dial(ctx, familyDialNetwork(network, targetIps[0]), net.JoinHostPort(targetIps[0].String(), port))
	default:
		return dialHostPortRace(ctx, network, port, targetIps, DefaultDialFallbackDelay, dial)
	}
}

// The typed errors of the attempts one strategy dial made, which the strategy
// otherwise flattens to "Timeout.". Safe for concurrent use: the strategy's
// dialers race.
type residentMoveAttemptErrors struct {
	stateLock sync.Mutex
	reason    protocol.ResidentMoveReason
	set       bool
}

func (self *residentMoveAttemptErrors) observe(err error) {
	if err == nil {
		return
	}
	reason := residentMoveFailureReason(err)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.set || residentMoveReasonRank(self.reason) < residentMoveReasonRank(reason) {
		self.reason = reason
		self.set = true
	}
}

// The most telling attempt error, else the dial's own.
func (self *residentMoveAttemptErrors) failureReason(err error) protocol.ResidentMoveReason {
	reason := residentMoveFailureReason(err)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.set && residentMoveReasonRank(reason) < residentMoveReasonRank(self.reason) {
		return self.reason
	}
	return reason
}

// Orders failure reasons by how much they say about the target: a refused
// credential or certificate is definite, a refused connection is about the
// path, and a timeout says the least.
func residentMoveReasonRank(reason protocol.ResidentMoveReason) int {
	switch reason {
	case protocol.ResidentMoveReason_ResidentMoveReasonAuthError:
		return 4
	case protocol.ResidentMoveReason_ResidentMoveReasonTlsError:
		return 3
	case protocol.ResidentMoveReason_ResidentMoveReasonDialError:
		return 2
	case protocol.ResidentMoveReason_ResidentMoveReasonConnectTimeout:
		return 1
	default:
		return 0
	}
}

// Classifies one dial error.
func residentMoveFailureReason(err error) protocol.ResidentMoveReason {
	if err == nil {
		return protocol.ResidentMoveReason_ResidentMoveReasonOther
	}
	if credentialRefusalCause(err) != 0 {
		return protocol.ResidentMoveReason_ResidentMoveReasonAuthError
	}
	var certificateVerificationError *tls.CertificateVerificationError
	var hostnameError x509.HostnameError
	var unknownAuthorityError x509.UnknownAuthorityError
	var certificateInvalidError x509.CertificateInvalidError
	var alertError tls.AlertError
	var recordHeaderError tls.RecordHeaderError
	if errors.As(err, &certificateVerificationError) ||
		errors.As(err, &hostnameError) ||
		errors.As(err, &unknownAuthorityError) ||
		errors.As(err, &certificateInvalidError) ||
		errors.As(err, &alertError) ||
		errors.As(err, &recordHeaderError) {
		return protocol.ResidentMoveReason_ResidentMoveReasonTlsError
	}
	var transportError *quic.TransportError
	if errors.As(err, &transportError) && transportError.ErrorCode.IsCryptoError() {
		return protocol.ResidentMoveReason_ResidentMoveReasonTlsError
	}
	var handshakeTimeoutError *quic.HandshakeTimeoutError
	var idleTimeoutError *quic.IdleTimeoutError
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.As(err, &handshakeTimeoutError) ||
		errors.As(err, &idleTimeoutError) {
		return protocol.ResidentMoveReason_ResidentMoveReasonConnectTimeout
	}
	var netError net.Error
	if errors.As(err, &netError) && netError.Timeout() {
		return protocol.ResidentMoveReason_ResidentMoveReasonConnectTimeout
	}
	var opError *net.OpError
	if errors.As(err, &opError) || errors.Is(err, errResidentMoveNoFamilyTarget) {
		return protocol.ResidentMoveReason_ResidentMoveReasonDialError
	}
	return protocol.ResidentMoveReason_ResidentMoveReasonOther
}

// The H1 header value of the declared version.
func residentMoveVersionHeader() string {
	return strconv.Itoa(ResidentMoveVersion)
}
