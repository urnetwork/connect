// Peer windows: an accounting mode, per peer and off by default, for a pair whose
// traffic must not show in its contract reports (C19 of the sidecar design).
//
// While a peer is in the mode, its contracts are accounted per window instead of
// per contract. A window is one direction's run of contracts: it opens with a
// contract and ends a margin before that contract's signed deadline. Every
// report of a window's contracts, checkpoint or close, is withheld until the
// window ends. The client then sends one final report per contract: the
// window's count as `windowReportByteCount` shapes it on the first contract and
// zero on the others, so what the platform keeps depends neither on when the
// bytes moved nor, up to the floor, on how many there were.
//
// A contract held before the mark stays out of the ledger and reports exactly,
// as it always has: the caller retires those (`RetirePeerContracts`) before its
// next frame. A receive window is opened by the contract that carried the peer's
// marking frame, or its answer to this client's, which the caller adopts
// (`AdoptPeerWindowContract`): the adopted contract is the window's first, and one
// created before it leaves the window, or never joins, and reports exactly, since
// its peer took it before marking and retired it. At a window's end the pair's send
// sequences move off its contracts,
// as they would at expiry, onto a new contract each, taken at once with its
// successor requested then, so a later window opens at a window's end and not at
// whatever frame comes next. The reports follow the move, so the contracts they
// close are done with and the pair is never left without a contract. A client
// without the mode has no ledger at all.
//
// `peerWindowLedger` holds the state and the rules. It reads no clock, does no
// I/O and calls nothing, so a test can run it on any timeline. The contract
// manager gives it the time and carries out what it answers.
package connect

import (
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A window's report is a format, not a policy: the two clients of a pair have to
// reach the same number from the same count.
const (
	// the least a window reports, whatever it carried
	peerWindowFloorByteCount = ByteCount(256 * 1024)
	// the step past the floor
	peerWindowStepByteCount = ByteCount(128 * 1024)
)

// What a window that counted `byteCount` reports: the floor up to the floor, and
// past it the next whole step. The steps hide the count inside a step.
func windowReportByteCount(byteCount ByteCount) ByteCount {
	if byteCount <= peerWindowFloorByteCount {
		return peerWindowFloorByteCount
	}
	stepCount := (byteCount - peerWindowFloorByteCount + peerWindowStepByteCount - 1) / peerWindowStepByteCount
	return peerWindowFloorByteCount + stepCount*peerWindowStepByteCount
}

// The timing of a peer window. A client that never uses the mode never reads it.
type PeerWindowSettings struct {
	// The longest a window lasts from when it opens. The signed deadline of a
	// 60 minute contract normally ends it first, by `ExpirationMargin`.
	WindowDuration time.Duration
	// How long before its first contract's signed deadline a window ends, so
	// its reports reach the platform before the platform closes the contract
	// itself.
	ExpirationMargin time.Duration
	// How close to a window's end a contract's creation may fall before the
	// contract opens the next window instead of joining this one. Each client
	// ends a window by its own clock and a sender moves on at its own end, so
	// this covers the two clocks' difference (the transfer margin's
	// `ContractExpirationMargin` allows the same). The rule reads only signed
	// values, so both clients of a pair decide it alike. Zero leaves it to a
	// contract created past the end.
	EndTolerance time.Duration
	// A window that ends with no activity in this last stretch clears the mode
	// instead of moving on, until the caller marks the peer again. The caller
	// says what activity is (`NotePeerWindowActivity`), and marking counts.
	// Non-positive never clears.
	ActivityTimeout time.Duration
	// How long a send window's reports wait for its sequences to take their
	// next contract, which usually takes one control round trip. They go out at
	// the first take, or at this at the latest, since a stalled control plane
	// must not hold them. Non-positive sends them at the window's end.
	MoveTimeout time.Duration
	// The longest the worker waits between passes while a window is open. A
	// timer stops with a suspended process, and this bounds how late a resumed
	// client notices a window that ended meanwhile. Non-positive leaves only
	// the timer at the next window's end.
	CheckInterval time.Duration
}

// Windows of 55 minutes, on the platform's 60 minute contracts.
func DefaultPeerWindowSettings() PeerWindowSettings {
	return PeerWindowSettings{
		WindowDuration:   55 * time.Minute,
		ExpirationMargin: 5 * time.Minute,
		EndTolerance:     2 * time.Minute,
		ActivityTimeout:  2 * time.Minute,
		MoveTimeout:      5 * time.Second,
		CheckInterval:    1 * time.Minute,
	}
}

// Which way a window's bytes move, as this client sees them.
type PeerWindowDirection int

const (
	PeerWindowSend PeerWindowDirection = iota
	PeerWindowReceive
	peerWindowDirectionCount
)

// An open window.
type PeerWindow struct {
	PeerId    Id
	Direction PeerWindowDirection
	// when this client opened it
	StartTime time.Time
	// when it ends, for both clients of the pair
	EndTime time.Time
}

// One peer's state in the mode.
type PeerWindowStatus struct {
	// whether the peer is in the mode
	Accounting bool
	// the open window each way, nil where none is open
	Send    *PeerWindow
	Receive *PeerWindow
}

// A contract the ledger answers for, from the time it joined a window until its
// window has reported and every holder of it has reported to the ledger.
type peerWindowContract struct {
	contractId              Id
	grantByteCount          ByteCount
	expirationTimeUnixMilli *int64
	window                  *peerWindow
	// The reports withheld while its window was open, in order and as their
	// holders asked for them, so a contract that leaves its window before the end
	// reports exactly what it would have.
	heldReports []peerWindowHeldReport
	// A close was asked for from outside while its window was open: the other
	// party is done with the contract, or no sequence holds it.
	closeRequested bool
	// The running count of the sequence holding it, nil when none does. Its
	// owner keeps it current, which is how a window's end reads a contract
	// that no report has closed.
	live *contractStatsEntry
	// How many owners still have to report it: a queue or a send sequence, or
	// the receive sequences that registered it. A report that arrives after
	// the window has reported is dropped, so one contract reports once.
	holderCount int
}

// What this client has counted on the contract, acked and unacked.
func (self *peerWindowContract) countedByteCount() ByteCount {
	byteCount := ByteCount(0)
	for _, heldReport := range self.heldReports {
		byteCount += heldReport.ackedByteCount + heldReport.unackedByteCount
	}
	if self.live != nil {
		byteCount += ByteCount(self.live.usedByteCount.Load())
	}
	return byteCount
}

// One report a holder asked for, as it asked for it.
type peerWindowHeldReport struct {
	ackedByteCount   ByteCount
	unackedByteCount ByteCount
	checkpoint       bool
}

// A contract that left its window before the end, with what it withheld there.
type peerWindowRelease struct {
	contractId     Id
	heldReports    []peerWindowHeldReport
	closeRequested bool
}

// One direction's run of contracts with one peer.
type peerWindow struct {
	peerId    Id
	direction PeerWindowDirection
	startTime time.Time
	endTime   time.Time
	contracts map[Id]*peerWindowContract
	// The receive contract an adoption made first, nil before one. No contract
	// created before it belongs to the window.
	firstContractId *Id
	// no longer open: nothing joins it, and sequences leave its contracts
	ended bool
	// when its reports go out, once a pass has seen it end
	reportTime time.Time
	// its reports have been handed out
	reported bool
}

// One final report per contract, and the count they came from. The window's first
// is the lowest contract id among the contracts that carried a frame by this
// client's count: `windowReportByteCount` of the window's count goes on it, what
// passes its grant goes on the next in id order, each capped by its grant, and the
// rest report zero. A contract no frame rode reports zero, since its peer may never
// have heard of it.
//
// Contract ids are platform-minted ULIDs, so id order is creation order and both
// clients of a pair read the same one. Two clients that hold different sets can
// still name different firsts, and each contract then carries the floor from one
// client and zero from the other. Settling those costs at most the window's debit,
// and it exposes nothing and charges nothing twice.
func (self *peerWindow) reports() ([]peerWindowReport, ByteCount) {
	// read once: a live sequence keeps moving them
	contractByteCounts := make(map[Id]ByteCount, len(self.contracts))
	contractIds := make([]Id, 0, len(self.contracts))
	byteCount := ByteCount(0)
	for contractId, contract := range self.contracts {
		contractByteCounts[contractId] = contract.countedByteCount()
		contractIds = append(contractIds, contractId)
		byteCount += contractByteCounts[contractId]
	}
	slices.SortFunc(contractIds, func(a Id, b Id) int {
		aRode, bRode := 0 < contractByteCounts[a], 0 < contractByteCounts[b]
		if aRode != bRode {
			if aRode {
				return -1
			}
			return 1
		}
		return a.Cmp(b)
	})
	reports := make([]peerWindowReport, 0, len(contractIds))
	remainingByteCount := windowReportByteCount(byteCount)
	for _, contractId := range contractIds {
		report := peerWindowReport{contractId: contractId}
		if 0 < contractByteCounts[contractId] {
			report.byteCount = min(remainingByteCount, self.contracts[contractId].grantByteCount)
			remainingByteCount -= report.byteCount
		}
		reports = append(reports, report)
	}
	return reports, byteCount
}

// Whether a contract with this id was created so near the window's end that it
// opens the next window. Creation is in the id and the end comes from a signed
// deadline, so this needs no clock and both clients answer alike.
func (self *peerWindow) opensNext(contractId Id, endTolerance time.Duration) bool {
	return !peerWindowContractCreateTime(contractId).Before(self.endTime.Add(-endTolerance))
}

// Whether a contract with this id was created before the window's adopted first,
// so it belongs to no window.
func (self *peerWindow) precedesFirst(contractId Id) bool {
	return self.firstContractId != nil && contractId.Cmp(*self.firstContractId) < 0
}

// The millisecond in an id's high bits, here the platform's clock at creation.
func peerWindowContractCreateTime(contractId Id) time.Time {
	unixMilli := int64(0)
	for _, b := range contractId[:6] {
		unixMilli = unixMilli<<8 | int64(b)
	}
	return time.UnixMilli(unixMilli)
}

// What a caller sees of the window.
func (self *peerWindow) open() PeerWindow {
	return PeerWindow{
		PeerId:    self.peerId,
		Direction: self.direction,
		StartTime: self.startTime,
		EndTime:   self.endTime,
	}
}

// One peer's place in the ledger.
type peerWindowPeer struct {
	accounting bool
	// the last activity the caller noted, and the mark
	activityTime time.Time
	// each direction's open window
	windows [peerWindowDirectionCount]*peerWindow
	// A receive contract created before this, by more than the tolerance, is
	// no part of any window: the mark, then each window's end. It predates what
	// the mode covers, or it is a straggler of a window already reported.
	receiveFloorTime time.Time
}

// Whether nothing keeps this peer in the ledger.
func (self *peerWindowPeer) idle() bool {
	return !self.accounting && self.windows[PeerWindowSend] == nil && self.windows[PeerWindowReceive] == nil
}

// One contract's final report.
type peerWindowReport struct {
	contractId Id
	byteCount  ByteCount
}

// A send window that has ended, for the manager to move its sequences off it.
type peerWindowMove struct {
	window *peerWindow
	peerId Id
	// whether the mode holds, so each sequence takes a new contract at once
	take bool
}

// A window that has ended and what it reports.
type peerWindowEnd struct {
	peerId    Id
	direction PeerWindowDirection
	// the count the reports came from
	byteCount ByteCount
	reports   []peerWindowReport
}

// The windows of every peer in the mode. Safe for concurrent use. Its lock is a
// leaf: a caller may hold the contract manager's mutex, and nothing here calls
// out.
type peerWindowLedger struct {
	settings PeerWindowSettings

	stateLock sync.Mutex
	peers     map[Id]*peerWindowPeer
	// every contract of an open window, and of an ended one until its holders
	// have reported
	contracts map[Id]*peerWindowContract
	// windows that have ended and no pass has seen
	endedWindows []*peerWindow
	// windows a pass has seen, each to report at its `reportTime`
	reportingWindows []*peerWindow
	// the latest window opened each way per peer, until a callback has seen it
	openedWindows map[peerWindowKey]*peerWindow
}

// One way of one peer.
type peerWindowKey struct {
	peerId    Id
	direction PeerWindowDirection
}

// An empty ledger on `settings`.
func newPeerWindowLedger(settings PeerWindowSettings) *peerWindowLedger {
	return &peerWindowLedger{
		settings:      settings,
		peers:         map[Id]*peerWindowPeer{},
		contracts:     map[Id]*peerWindowContract{},
		openedWindows: map[peerWindowKey]*peerWindow{},
	}
}

// Sets the mode for a peer and reports whether that changed it. A window already
// open runs to its end either way, so turning the mode off shows nothing early.
func (self *peerWindowLedger) setAccounting(peerId Id, accounting bool, now time.Time) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	peer := self.peers[peerId]
	if peer == nil {
		if !accounting {
			return false
		}
		peer = &peerWindowPeer{}
		self.peers[peerId] = peer
	}
	if peer.accounting == accounting {
		return false
	}
	peer.accounting = accounting
	if accounting {
		peer.activityTime = now
		peer.receiveFloorTime = now
	}
	if peer.idle() {
		delete(self.peers, peerId)
	}
	return true
}

// The caller saw this peer's traffic that keeps a window going.
func (self *peerWindowLedger) noteActivity(peerId Id, now time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if peer := self.peers[peerId]; peer != nil {
		peer.activityTime = now
	}
}

// Whether the peer is in the mode.
func (self *peerWindowLedger) accounting(peerId Id) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	peer := self.peers[peerId]
	return peer != nil && peer.accounting
}

// The peer's mode and its open windows.
func (self *peerWindowLedger) status(peerId Id) PeerWindowStatus {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	status := PeerWindowStatus{}
	peer := self.peers[peerId]
	if peer == nil {
		return status
	}
	status.Accounting = peer.accounting
	if window := peer.windows[PeerWindowSend]; window != nil {
		open := window.open()
		status.Send = &open
	}
	if window := peer.windows[PeerWindowReceive]; window != nil {
		open := window.open()
		status.Receive = &open
	}
	return status
}

// A window past its end has ended, whether or not a pass has seen it yet, so a
// contract arriving late never joins it.
func (self *peerWindowLedger) currentWindowWithLock(
	peer *peerWindowPeer,
	direction PeerWindowDirection,
	now time.Time,
) *peerWindow {
	window := peer.windows[direction]
	if window != nil && !now.Before(window.endTime) {
		self.endWindowWithLock(peer, window)
		return nil
	}
	return window
}

// Closes the window to new contracts, for the next pass to see.
func (self *peerWindowLedger) endWindowWithLock(peer *peerWindowPeer, window *peerWindow) {
	peer.windows[window.direction] = nil
	window.ended = true
	if window.direction == PeerWindowReceive {
		peer.receiveFloorTime = window.endTime
	}
	self.endedWindows = append(self.endedWindows, window)
}

// Opens a direction's window. The contract that opens it can bring its end nearer.
func (self *peerWindowLedger) openWindowWithLock(
	peerId Id,
	peer *peerWindowPeer,
	direction PeerWindowDirection,
	now time.Time,
) *peerWindow {
	window := &peerWindow{
		peerId:    peerId,
		direction: direction,
		startTime: now,
		endTime:   now.Add(self.settings.WindowDuration),
		contracts: map[Id]*peerWindowContract{},
	}
	peer.windows[direction] = window
	self.openedWindows[peerWindowKey{peerId: peerId, direction: direction}] = window
	return window
}

// Joins a contract to a window, and the ledger to the contract's reports.
func (self *peerWindowLedger) addContractWithLock(
	window *peerWindow,
	contractId Id,
	grantByteCount ByteCount,
	expirationTimeUnixMilli *int64,
) *peerWindowContract {
	contract := &peerWindowContract{
		contractId:              contractId,
		grantByteCount:          grantByteCount,
		expirationTimeUnixMilli: expirationTimeUnixMilli,
		window:                  window,
	}
	window.contracts[contractId] = contract
	self.contracts[contractId] = contract
	return contract
}

// A contract in use brings the window's end to its own signed deadline's margin,
// so the earliest of them sets it.
func (self *peerWindowLedger) boundWindowEndWithLock(contract *peerWindowContract) {
	if contract.expirationTimeUnixMilli != nil {
		contract.window.endTime = minTime(
			contract.window.endTime,
			time.UnixMilli(*contract.expirationTimeUnixMilli).Add(-self.settings.ExpirationMargin),
		)
	}
}

// The earlier of two times.
func minTime(a time.Time, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// The window a contract that a frame rode belongs to, nil when it belongs to
// none. It ends a window it was created too late for, and opens the next one
// while the mode holds.
func (self *peerWindowLedger) ridingWindowWithLock(
	peerId Id,
	peer *peerWindowPeer,
	direction PeerWindowDirection,
	contractId Id,
	now time.Time,
) *peerWindow {
	window := self.currentWindowWithLock(peer, direction, now)
	if window != nil && window.opensNext(contractId, self.settings.EndTolerance) {
		self.endWindowWithLock(peer, window)
		window = nil
	}
	if window == nil && peer.accounting {
		window = self.openWindowWithLock(peerId, peer, direction, now)
	}
	return window
}

// A contract for this peer reached the send queue. Inside a window it belongs to
// the window and reports with it, used or not. With no window open it stays out:
// its take is what opens one.
func (self *peerWindowLedger) queued(
	peerId Id,
	contractId Id,
	grantByteCount ByteCount,
	expirationTimeUnixMilli *int64,
	now time.Time,
) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	peer := self.peers[peerId]
	if peer == nil {
		return
	}
	if self.contracts[contractId] != nil {
		// a repeated result: what the ledger counted of it stands
		return
	}
	window := self.currentWindowWithLock(peer, PeerWindowSend, now)
	if window == nil || window.opensNext(contractId, self.settings.EndTolerance) {
		return
	}
	self.addContractWithLock(window, contractId, grantByteCount, expirationTimeUnixMilli).holderCount = 1
}

// A send sequence took a contract for this peer. Reports whether a window's
// state changed, so the worker can look again.
func (self *peerWindowLedger) taken(
	peerId Id,
	contractId Id,
	grantByteCount ByteCount,
	expirationTimeUnixMilli *int64,
	now time.Time,
) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	peer := self.peers[peerId]
	if peer == nil {
		return false
	}
	// the take a moving window's reports wait for
	moved := false
	for _, window := range self.reportingWindows {
		if window.peerId == peerId && window.direction == PeerWindowSend && now.Before(window.reportTime) {
			window.reportTime = now
			moved = true
		}
	}
	if contract := self.contracts[contractId]; contract != nil {
		// queued inside a window
		contract.holderCount = 1
		if !contract.window.ended {
			self.boundWindowEndWithLock(contract)
		}
		return true
	}
	window := self.ridingWindowWithLock(peerId, peer, PeerWindowSend, contractId, now)
	if window == nil {
		return moved
	}
	contract := self.addContractWithLock(window, contractId, grantByteCount, expirationTimeUnixMilli)
	contract.holderCount = 1
	self.boundWindowEndWithLock(contract)
	return true
}

// A receive sequence registered a contract from this peer. A contract created
// before the mark, a previous window's end or the window's adopted first stays
// out: its sender reports it exactly. Reports whether the contract is the
// ledger's.
func (self *peerWindowLedger) received(
	peerId Id,
	contractId Id,
	grantByteCount ByteCount,
	expirationTimeUnixMilli *int64,
	live *contractStatsEntry,
	now time.Time,
) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	peer := self.peers[peerId]
	if peer == nil {
		return false
	}
	if contract := self.contracts[contractId]; contract != nil {
		// a later sequence resumed it
		contract.holderCount += 1
		if !contract.window.reported {
			contract.live = live
		}
		return true
	}
	if peerWindowContractCreateTime(contractId).Before(
		peer.receiveFloorTime.Add(-self.settings.EndTolerance),
	) {
		return false
	}
	window := self.ridingWindowWithLock(peerId, peer, PeerWindowReceive, contractId, now)
	if window == nil || window.precedesFirst(contractId) {
		return false
	}
	contract := self.addContractWithLock(window, contractId, grantByteCount, expirationTimeUnixMilli)
	contract.holderCount = 1
	contract.live = live
	self.boundWindowEndWithLock(contract)
	return true
}

// The frame that marked the peer, or the peer's answer to this client's marking
// frame, rode this contract, which its receive sequence holds. The contract joins
// the receive window, opening one if none is open and however old it is, and is the
// window's first: each of the window's contracts created before it leaves, and is
// returned with what it withheld so it reports exactly, and none created before it
// joins later. Its peer took those before marking and retired them at its mark, so
// it reports them exactly too. An open window is pinned once, by its first
// adoption; a later one only joins. Reports whether the contract is the ledger's.
func (self *peerWindowLedger) adopt(
	peerId Id,
	contractId Id,
	grantByteCount ByteCount,
	expirationTimeUnixMilli *int64,
	live *contractStatsEntry,
	now time.Time,
) (bool, []peerWindowRelease) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	peer := self.peers[peerId]
	if peer == nil {
		return false, nil
	}
	contract := self.contracts[contractId]
	if contract == nil {
		window := self.ridingWindowWithLock(peerId, peer, PeerWindowReceive, contractId, now)
		if window == nil || window.precedesFirst(contractId) {
			return false, nil
		}
		contract = self.addContractWithLock(window, contractId, grantByteCount, expirationTimeUnixMilli)
		contract.holderCount = 1
		self.boundWindowEndWithLock(contract)
	}
	if !contract.window.reported {
		contract.live = live
	}
	window := contract.window
	if window.direction != PeerWindowReceive || window.ended || window.firstContractId != nil {
		return true, nil
	}
	firstContractId := contractId
	window.firstContractId = &firstContractId
	releases := []peerWindowRelease{}
	for otherContractId, other := range window.contracts {
		if otherContractId.Cmp(contractId) < 0 {
			delete(window.contracts, otherContractId)
			delete(self.contracts, otherContractId)
			releases = append(releases, peerWindowRelease{
				contractId:     otherContractId,
				heldReports:    other.heldReports,
				closeRequested: other.closeRequested,
			})
		}
	}
	if 0 < len(releases) {
		// One of those opened the window, so it opens again with its first, as the
		// peer's did, and ends by that contract's deadline. A listener hears of it
		// again.
		window.startTime = now
		window.endTime = now.Add(self.settings.WindowDuration)
		for _, remaining := range window.contracts {
			self.boundWindowEndWithLock(remaining)
		}
		self.openedWindows[peerWindowKey{peerId: peerId, direction: PeerWindowReceive}] = window
	}
	return true, releases
}

// The running count of a send contract, once its sequence has made it current.
// Reports whether the contract is the ledger's.
func (self *peerWindowLedger) attachLive(contractId Id, live *contractStatsEntry) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	contract := self.contracts[contractId]
	if contract == nil {
		return false
	}
	if !contract.window.reported {
		contract.live = live
	}
	return true
}

// A holder reports this contract. The ledger keeps every report of its contracts
// and answers true: the count goes to its window, and once the window has
// reported it goes nowhere. False leaves the report to go out as it always has.
func (self *peerWindowLedger) withhold(
	contractId Id,
	ackedByteCount ByteCount,
	unackedByteCount ByteCount,
	checkpoint bool,
) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	contract := self.contracts[contractId]
	if contract == nil {
		return false
	}
	if !contract.window.reported {
		contract.heldReports = append(contract.heldReports, peerWindowHeldReport{
			ackedByteCount:   ackedByteCount,
			unackedByteCount: unackedByteCount,
			checkpoint:       checkpoint,
		})
		contract.live = nil
	}
	if 0 < contract.holderCount {
		contract.holderCount -= 1
	}
	if contract.window.reported && contract.holderCount == 0 {
		delete(self.contracts, contractId)
	}
	return true
}

// A close was asked for from outside. The ledger keeps the request with the
// contract and answers true: the window's report is the contract's final, and a
// contract that leaves its window early carries the request out then. False
// leaves the request to be carried out as it always has.
func (self *peerWindowLedger) withholdCloseRequest(contractId Id) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	contract := self.contracts[contractId]
	if contract == nil {
		return false
	}
	if !contract.window.reported {
		contract.closeRequested = true
	}
	return true
}

// Whether the ledger answers for this contract's reports.
func (self *peerWindowLedger) owns(contractId Id) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.contracts[contractId] != nil
}

// Whether an open window holds this contract, so a sequence on it stays.
func (self *peerWindowLedger) holds(contractId Id) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	contract := self.contracts[contractId]
	return contract != nil && !contract.window.ended
}

// Ends every window past its end, and returns the send windows whose sequences
// have to leave. A window that saw no activity in its last stretch clears the
// mode, unless a contract has already opened the next window. A client that could
// not run at a window's end gets here when it next runs, and what clears the mode
// is the window's own last stretch, not how late that is.
func (self *peerWindowLedger) takeEnded(now time.Time) []peerWindowMove {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	for _, peer := range self.peers {
		for direction := range peer.windows {
			self.currentWindowWithLock(peer, PeerWindowDirection(direction), now)
		}
	}
	moves := []peerWindowMove{}
	for _, window := range self.endedWindows {
		window.reportTime = now
		peer := self.peers[window.peerId]
		moved := peer != nil && peer.windows[window.direction] != nil
		if peer != nil && peer.accounting && !moved && 0 < self.settings.ActivityTimeout &&
			peer.activityTime.Before(minTime(window.endTime, now).Add(-self.settings.ActivityTimeout)) {
			peer.accounting = false
		}
		self.reportingWindows = append(self.reportingWindows, window)
		if window.direction != PeerWindowSend {
			continue
		}
		take := peer != nil && peer.accounting
		if take && !moved {
			window.reportTime = now.Add(self.settings.MoveTimeout)
		}
		moves = append(moves, peerWindowMove{window: window, peerId: window.peerId, take: take})
	}
	self.endedWindows = nil
	return moves
}

// No sequence holds the window's contracts, so no take is coming.
func (self *peerWindowLedger) releaseMove(window *peerWindow, now time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	window.reportTime = minTime(window.reportTime, now)
}

// Hands out the reports that are due. The counts are read here, after the move,
// so what the sequences charged on their way off the contracts is in them.
func (self *peerWindowLedger) takeReports(now time.Time) []*peerWindowEnd {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	ends := []*peerWindowEnd{}
	waitingWindows := []*peerWindow{}
	for _, window := range self.reportingWindows {
		if now.Before(window.reportTime) {
			waitingWindows = append(waitingWindows, window)
			continue
		}
		reports, byteCount := window.reports()
		window.reported = true
		for contractId, contract := range window.contracts {
			if contract.holderCount == 0 {
				delete(self.contracts, contractId)
			}
		}
		// a listener has nothing to schedule from a window that is over
		key := peerWindowKey{peerId: window.peerId, direction: window.direction}
		if self.openedWindows[key] == window {
			delete(self.openedWindows, key)
		}
		ends = append(ends, &peerWindowEnd{
			peerId:    window.peerId,
			direction: window.direction,
			byteCount: byteCount,
			reports:   reports,
		})
		if peer := self.peers[window.peerId]; peer != nil && peer.idle() {
			delete(self.peers, window.peerId)
		}
	}
	self.reportingWindows = waitingWindows
	return ends
}

// The windows opened since the last look, each with the end it has now.
func (self *peerWindowLedger) takeOpened() []PeerWindow {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	opened := make([]PeerWindow, 0, len(self.openedWindows))
	for _, window := range self.openedWindows {
		opened = append(opened, window.open())
	}
	clear(self.openedWindows)
	return opened
}

// How long until a pass has something to do, and whether anything is open to
// wait for.
func (self *peerWindowLedger) nextWait(now time.Time) (time.Duration, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	wait := time.Duration(0)
	open := false
	note := func(due time.Time) {
		dueWait := max(0, due.Sub(now))
		if !open || dueWait < wait {
			wait = dueWait
		}
		open = true
	}
	for _, peer := range self.peers {
		for _, window := range peer.windows {
			if window != nil {
				note(window.endTime)
			}
		}
	}
	for _, window := range self.reportingWindows {
		note(window.reportTime)
	}
	if open && 0 < self.settings.CheckInterval {
		wait = min(wait, self.settings.CheckInterval)
	}
	return wait, open
}

// Puts a peer in the mode, or takes it out, and reports whether that changed it.
// It is what opts a pair in, and nothing else does.
//
// From the mark on, the first contract this client takes to the peer opens the
// send window, and the first one it receives from the peer opens the receive
// window. A caller that gets true retires what it already holds
// (`RetirePeerContracts`) before its next frame to the peer. One that marks on
// receipt of a frame also calls `AdoptPeerWindowContract`, since that frame rode a
// contract already held, and one that marks before sending calls it on the peer's
// answer, which makes the answer's contract its receive window's first. Off lets
// the windows already open run to their end, and their reports go out then.
func (self *ContractManager) SetPeerWindowAccounting(peerId Id, accounting bool) bool {
	ledger := self.openPeerWindowLedger(accounting)
	if ledger == nil {
		return false
	}
	changed := ledger.setAccounting(peerId, accounting, time.Now())
	if changed {
		self.wakePeerWindows()
	}
	return changed
}

// The peer's place in the mode, for a caller that schedules by its windows.
func (self *ContractManager) PeerWindow(peerId Id) PeerWindowStatus {
	ledger := self.peerWindows.Load()
	if ledger == nil {
		return PeerWindowStatus{}
	}
	return ledger.status(peerId)
}

// Before this returns, every send sequence to the peer is committed to leave each
// contract no open window holds, and the queued ones are gone, so the next frame
// to the peer takes a new contract. Their reports go out at once, as they always
// have: no frame of the mode rode them. A repeat retires nothing, since the
// contracts in use by then are the window's.
func (self *ContractManager) RetirePeerContracts(peerId Id) {
	var contracts []*protocol.Contract
	func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		contracts = self.removeUnheldPeerContractsWithLock(peerId)
	}()
	self.closeContracts(contracts)
	self.client.sendBuffer.movePeerContracts(peerId, false)
}

// Notes this peer's traffic that keeps its window going. A window that ends with
// none in its last stretch clears the mode (`ActivityTimeout`). Connect does not
// read what its callers send, so they say.
func (self *ContractManager) NotePeerWindowActivity(peerId Id) {
	if ledger := self.peerWindows.Load(); ledger != nil {
		ledger.noteActivity(peerId, time.Now())
	}
}

// Receives each window as it opens.
type PeerWindowFunction = func(window PeerWindow)

// Adds a listener for each window's opening, for a caller that keeps a schedule
// from a window's start. Called off the packet paths, one window at a time, and a
// listener that blocks delays only other listeners: a slow one sees the latest
// window of each peer and direction. Returns its removal.
func (self *ContractManager) AddPeerWindowCallback(callback PeerWindowFunction) func() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.closed {
		return func() {}
	}
	if !self.peerWindowCallbacksStarted {
		self.peerWindowCallbacksStarted = self.startWorker("peer window callbacks", self.runPeerWindowCallbacks)
	}
	callbackId := self.peerWindowCallbacks.Add(callback)
	return func() {
		self.peerWindowCallbacks.Remove(callbackId)
	}
}

// Makes the contract the peer's last frame on `transferKey` rode the receive
// window's first. A caller that marks a peer on receipt of a frame adopts that
// frame's contract, which was registered before the mark, so nothing else would
// bring it in and it would report exactly. A caller that marked before sending
// adopts the contract of the peer's answer, so a contract its peer took before
// marking, such as one that carried a session's handshake, leaves the window. Such
// a contract reports at once what it withheld, as it would have without the
// window, and from then on reports exactly. `transferKey` is the callback's
// `Peer.TransferKey`, which names the one sequence the frame rode. Reports whether
// the contract is the window's. Never blocks, so a receive callback may call it.
func (self *ContractManager) AdoptPeerWindowContract(peerId Id, transferKey TransferKey) bool {
	ledger := self.peerWindows.Load()
	if ledger == nil {
		return false
	}
	contract := self.client.receiveBuffer.currentContract(peerId, transferKey)
	if contract == nil {
		return false
	}
	adopted, releases := ledger.adopt(
		peerId,
		contract.contractId,
		contract.transferByteCount,
		contract.expirationTimeUnixMilli,
		contract.statsEntry,
		time.Now(),
	)
	for _, release := range releases {
		for _, heldReport := range release.heldReports {
			self.closeContractWithPendingReport(
				release.contractId,
				heldReport.ackedByteCount,
				heldReport.unackedByteCount,
				heldReport.checkpoint,
				self.newContractCloseReport(release.contractId, heldReport.checkpoint),
			)
		}
		if release.closeRequested {
			// as the platform's close would have gone: to the receive sequence that
			// holds the contract, else a zero final behind the reports above
			receiveSequence := self.client.receiveBuffer.contractHolders.find(release.contractId)
			if receiveSequence == nil || !receiveSequence.requestCloseContract(release.contractId) {
				self.requestContractClose(release.contractId)
			}
		}
	}
	if adopted {
		self.wakePeerWindows()
	}
	return adopted
}

// The ledger, created on the first mark. Every hook reads it through one nil
// check, which is all the mode costs a client that never uses it.
func (self *ContractManager) openPeerWindowLedger(create bool) *peerWindowLedger {
	if ledger := self.peerWindows.Load(); ledger != nil || !create {
		return ledger
	}
	self.mutex.Lock()
	defer self.mutex.Unlock()

	ledger := self.peerWindows.Load()
	if ledger == nil && !self.closed {
		ledger = newPeerWindowLedger(self.settings.PeerWindow)
		self.peerWindows.Store(ledger)
		self.startWorker("peer windows", self.runPeerWindows)
	}
	return ledger
}

// Wakes the workers to look again. Never blocks.
func (self *ContractManager) wakePeerWindows() {
	select {
	case self.peerWindowWake <- struct{}{}:
	default:
	}
	select {
	case self.peerWindowCallbackWake <- struct{}{}:
	default:
	}
}

// Delivers window openings, apart from the worker that ends windows, so no
// listener holds a window's reports.
func (self *ContractManager) runPeerWindowCallbacks() {
	for {
		if ledger := self.peerWindows.Load(); ledger != nil {
			for _, window := range ledger.takeOpened() {
				for _, callback := range self.peerWindowCallbacks.Get() {
					HandleError(func() {
						callback(window)
					})
				}
			}
		}
		select {
		case <-self.ctx.Done():
			return
		case <-self.client.Done():
			return
		case <-self.peerWindowCallbackWake:
		}
	}
}

// Ends and reports each window. A client that closes with a window open sends
// nothing for it: the mode reports only at a window's end, and the platform's
// deadline close settles such a contract from what the other party reported.
func (self *ContractManager) runPeerWindows() {
	ledger := self.peerWindows.Load()
	for {
		self.reportPeerWindows(ledger, time.Now())

		// nil, never ready, while no window is open
		var wake <-chan time.Time
		if wait, open := ledger.nextWait(time.Now()); open {
			wake = time.After(wait)
		}
		select {
		case <-self.ctx.Done():
			return
		case <-self.client.Done():
			return
		case <-self.peerWindowWake:
		case <-wake:
		}
	}
}

// One pass over the windows. Ending a window and emptying its peer's queues is one
// step under the mutex that a take also holds, so no sequence can take a contract
// of a window that has ended. Each report then goes through the same close path as
// any other, so it keeps its own report id and retry.
func (self *ContractManager) reportPeerWindows(ledger *peerWindowLedger, now time.Time) {
	var moves []peerWindowMove
	var contracts []*protocol.Contract
	func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		moves = ledger.takeEnded(now)
		for _, move := range moves {
			contracts = append(contracts, self.removeUnheldPeerContractsWithLock(move.peerId)...)
		}
	}()
	self.closeContracts(contracts)
	for _, move := range moves {
		if self.client.sendBuffer.movePeerContracts(move.peerId, move.take) == 0 {
			ledger.releaseMove(move.window, now)
		}
	}
	for _, end := range ledger.takeReports(now) {
		if self.client.log.V(1).Enabled() {
			self.client.log.Infof("[contract]peer window end %s direction=%d count=%d contracts=%d\n", end.peerId, end.direction, end.byteCount, len(end.reports))
		}
		for _, report := range end.reports {
			self.closeContractWithPendingReport(
				report.contractId,
				report.byteCount,
				0,
				false,
				self.newContractCloseReport(report.contractId, false),
			)
		}
	}
}

// Takes the contracts queued for a peer that no open window holds, for the caller
// to close. The queue stays, so a request already in flight still lands.
func (self *ContractManager) removeUnheldPeerContractsWithLock(peerId Id) []*protocol.Contract {
	ledger := self.peerWindows.Load()
	contracts := []*protocol.Contract{}
	for contractKey, contractQueue := range self.destinationContracts {
		if contractKey.Destination.DestinationId != peerId {
			continue
		}
		contracts = append(contracts, contractQueue.removeWhere(func(contractId Id) bool {
			return ledger == nil || !ledger.holds(contractId)
		})...)
		if contractQueue.IsDone() {
			delete(self.destinationContracts, contractKey)
		}
	}
	return contracts
}

// Keeps a report the ledger answers for, and reports whether it did.
func (self *ContractManager) withholdPeerWindowReport(
	contractId Id,
	ackedByteCount ByteCount,
	unackedByteCount ByteCount,
	checkpoint bool,
) bool {
	ledger := self.peerWindows.Load()
	if ledger == nil || !ledger.withhold(contractId, ackedByteCount, unackedByteCount, checkpoint) {
		return false
	}
	// its sequence stops updating the contract here, as at any report
	self.closeContractStats(contractId)
	return true
}

// Keeps a close asked for from outside when the ledger answers for the contract,
// and reports whether it did. A kept request changes nothing now: the report goes
// out at the window's end, or the request is carried out if the contract leaves
// its window first.
func (self *ContractManager) withholdPeerWindowCloseRequest(contractId Id) bool {
	ledger := self.peerWindows.Load()
	return ledger != nil && ledger.withholdCloseRequest(contractId)
}

// Whether the peer is in the mode.
func (self *ContractManager) peerWindowAccounting(peerId Id) bool {
	ledger := self.peerWindows.Load()
	return ledger != nil && ledger.accounting(peerId)
}

// Whether a sequence on this contract stays on it through a move.
func (self *ContractManager) peerWindowHolds(contractId Id) bool {
	ledger := self.peerWindows.Load()
	return ledger != nil && ledger.holds(contractId)
}

// Tells the ledger a contract reached the send queue. The caller holds the mutex.
func (self *ContractManager) notePeerWindowQueuedWithLock(
	contractKey ContractKey,
	storedContract *protocol.StoredContract,
) {
	ledger := self.peerWindows.Load()
	if ledger == nil {
		return
	}
	if contractId, err := IdFromBytes(storedContract.ContractId); err == nil {
		ledger.queued(
			contractKey.Destination.DestinationId,
			contractId,
			ByteCount(storedContract.TransferByteCount),
			storedContract.ExpirationTimeUnixMilli,
			time.Now(),
		)
	}
}

// Tells the ledger a send sequence took a contract. The caller holds the mutex.
func (self *ContractManager) notePeerWindowTakenWithLock(
	contractKey ContractKey,
	contractId Id,
	storedContract *protocol.StoredContract,
) {
	ledger := self.peerWindows.Load()
	if ledger != nil && ledger.taken(
		contractKey.Destination.DestinationId,
		contractId,
		ByteCount(storedContract.TransferByteCount),
		storedContract.ExpirationTimeUnixMilli,
		time.Now(),
	) {
		self.wakePeerWindows()
	}
}

// Gives the ledger a send contract's running count once its sequence makes it
// current. Runs on that sequence, the owner of the counts. An announced successor
// was charged before its stats existed, which this makes up.
func (self *ContractManager) notePeerWindowSendContract(contract *sequenceContract) {
	ledger := self.peerWindows.Load()
	if ledger != nil && ledger.attachLive(contract.contractId, contract.statsEntry) {
		contract.statsEntry.usedByteCount.Store(int64(contract.ackedByteCount + contract.unackedByteCount))
	}
}

// Tells the ledger a receive sequence registered a contract.
func (self *ContractManager) notePeerWindowReceiveContract(contract *sequenceContract) {
	ledger := self.peerWindows.Load()
	if ledger != nil && ledger.received(
		contract.path.SourceId,
		contract.contractId,
		contract.transferByteCount,
		contract.expirationTimeUnixMilli,
		contract.statsEntry,
		time.Now(),
	) {
		self.wakePeerWindows()
	}
}

// Asks each send sequence to the peer to leave the contracts no open window holds,
// and with `take` to move onto a new one at once. Returns how many it asked.
func (self *SendBuffer) movePeerContracts(peerId Id, take bool) int {
	sequences := []*SendSequence{}
	func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		for id, sequence := range self.sendSequences {
			if id.Destination == peerId {
				sequences = append(sequences, sequence)
			}
		}
	}()
	for _, sequence := range sequences {
		sequence.requestPeerContractMove(take)
	}
	return len(sequences)
}

const (
	// leave every contract no open window holds
	peerContractMoveRetire = uint32(1) << iota
	// and if that left the sequence with none, take a new one at once
	peerContractMoveTake
)

// Commits the sequence to the move, which its own goroutine carries out: before
// any later debit, and on its next pass even with nothing to send. A caller's
// no-acknowledgement write never reaches that goroutine, so it is fenced here: a
// caller past this returns to the queue rather than ride the contract.
func (self *SendSequence) requestPeerContractMove(take bool) {
	move := peerContractMoveRetire
	if take {
		move |= peerContractMoveTake
	}
	self.peerContractMove.Or(move)
	if snapshot := self.noAckFastPath.Load(); snapshot != nil && snapshot.contract != nil &&
		!self.client.ContractManager().peerWindowHolds(snapshot.contract.contractId) {
		snapshot.contract.noAckWriterState.Or(noAckContractRetired)
	}
	select {
	case self.retainedAdmissionWake <- struct{}{}:
	default:
	}
}

// Carries out a requested move, as a contract at its deadline retires: what the
// contract carries stays attributed to it in `openSendContracts` and closes as it
// is acknowledged, so ordering and acks are untouched, and the sequence's next
// Pack is in order behind everything it carried. A take that fails leaves the
// sequence with no contract, and its next Pack asks for one as usual.
func (self *SendSequence) applyPeerContractMove() {
	if self.peerContractMove.Load() == 0 {
		return
	}
	move := self.peerContractMove.Swap(0)
	manager := self.client.ContractManager()
	if headroom := self.noAckHeadroomContract; headroom != nil && !manager.peerWindowHolds(headroom.contractId) {
		self.endNoAckHeadroom()
	}
	if ahead := self.aheadSendContract; ahead != nil && !manager.peerWindowHolds(ahead.contractId) {
		self.discardAheadContract()
	}
	contract := self.sendContract
	if contract == nil || manager.peerWindowHolds(contract.contractId) {
		return
	}
	self.log.Infof(
		"[s]%s->%s...%s s(%s) contract %s moves off\n",
		self.client.ClientTag(),
		self.contractIntermediaryIds(),
		self.destination,
		self.contractMultiRouteWriterAlias.StreamId,
		contract.contractId,
	)
	self.sendContract = nil
	self.preparedContractByteCount.Store(0)
	self.sendContractAcked = false
	self.retireSendContract(contract)
	if move&peerContractMoveTake != 0 {
		// the clock moved this contract, not traffic, so it is sized as an
		// opening one: the ramp would otherwise reach the standard size after
		// a few idle windows and reserve that for every contract of the pair
		self.contractSeqIndex = 0
		self.updateContractForMode(0, sendContractUpdatePeerMove)
	}
}

// The contract the sequence on this lane from this peer currently holds, nil when
// there is none.
func (self *ReceiveBuffer) currentContract(peerId Id, transferKey TransferKey) *sequenceContract {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	for headKey, sequenceId := range self.headReceiveSequenceIds {
		if headKey.Source.SourceId != peerId || headKey.transferKey() != transferKey {
			continue
		}
		if sequence := self.receiveSequences[sequenceId]; sequence != nil {
			return sequence.publishedContract.Load()
		}
	}
	return nil
}
