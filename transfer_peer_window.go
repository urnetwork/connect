// Peer windows: an accounting mode, per peer and off by default, for a pair whose
// traffic must not show in its contract reports (C19 of the sidecar design).
//
// While a peer is in the mode, its contracts are accounted per window instead of
// per contract. A window is one direction's run of contracts: it opens with a
// contract and ends 5 minutes before that contract's signed deadline. Every
// report of a window's contracts, checkpoint or close, is withheld until the
// window ends. The client then sends one final report per contract: the
// window's count as `windowReportByteCount` shapes it on the first contract and
// zero on the others, so what the platform keeps depends neither on when the
// bytes moved nor, up to the floor, on how many there were.
//
// A contract held before the mark stays out of the ledger and reports exactly,
// as it always has. A client without the mode has no ledger at all.
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
	// values, so both clients of a pair decide it alike.
	EndTolerance time.Duration
	// The longest the worker waits between passes while a window is open. A
	// timer stops with a suspended process, and this bounds how late a resumed
	// client notices a window that ended meanwhile. Non-positive leaves only
	// the timer at the next window's end.
	CheckInterval time.Duration
}

func DefaultPeerWindowSettings() PeerWindowSettings {
	return PeerWindowSettings{
		WindowDuration:   55 * time.Minute,
		ExpirationMargin: 5 * time.Minute,
		EndTolerance:     2 * time.Minute,
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
	// Whether this client took it into use or saw a frame on it. A contract
	// that only waited in a queue never reached the peer, so it is never
	// first and takes no share of the report.
	ridden bool
	// what withheld reports counted
	reportedByteCount ByteCount
	// The running count of the sequence holding it, nil when none does. Its
	// owner keeps it current, which is how a window's end reads a contract
	// that no report has closed.
	live *contractStatsEntry
	// How many owners still have to report it: a queue or a send sequence, or
	// the receive sequences that registered it. A report that arrives after
	// the window has reported is dropped, so one contract reports once.
	holderCount int
}

func (self *peerWindowContract) countedByteCount() ByteCount {
	byteCount := self.reportedByteCount
	if self.live != nil {
		byteCount += ByteCount(self.live.usedByteCount.Load())
	}
	return byteCount
}

type peerWindow struct {
	peerId    Id
	direction PeerWindowDirection
	startTime time.Time
	endTime   time.Time
	contracts map[Id]*peerWindowContract
	// no longer open: nothing joins it, and sequences leave its contracts
	ended bool
	// its reports have been handed out
	reported bool
}

// The window's contracts in report order: those a frame rode, earliest created
// first, then the others. Contract ids are platform-minted ULIDs, so id order is
// creation order and both clients of a pair read the same one. Two clients that
// hold different sets can name different firsts, and each contract then carries
// the floor from one client and zero from the other. That costs the window's
// debit, since the platform settles a contract at the smaller count, and it
// exposes nothing and charges nothing twice.
func (self *peerWindow) orderedContracts() []*peerWindowContract {
	contracts := make([]*peerWindowContract, 0, len(self.contracts))
	for _, contract := range self.contracts {
		contracts = append(contracts, contract)
	}
	slices.SortFunc(contracts, func(a *peerWindowContract, b *peerWindowContract) int {
		if a.ridden != b.ridden {
			if a.ridden {
				return -1
			}
			return 1
		}
		return a.contractId.Cmp(b.contractId)
	})
	return contracts
}

// One final report: `windowReportByteCount` of the window's count on the first
// contract, what passes its grant on the next, each capped by its grant, and
// zero on the rest. A contract no frame rode takes no share: its peer never heard
// of it.
func (self *peerWindow) reports() ([]peerWindowReport, ByteCount) {
	contracts := self.orderedContracts()
	byteCount := ByteCount(0)
	for _, contract := range contracts {
		byteCount += contract.countedByteCount()
	}
	reports := make([]peerWindowReport, 0, len(contracts))
	remainingByteCount := windowReportByteCount(byteCount)
	for _, contract := range contracts {
		report := peerWindowReport{contractId: contract.contractId}
		if contract.ridden {
			report.byteCount = min(remainingByteCount, contract.grantByteCount)
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

// The millisecond in an id's high bits, here the platform's clock at creation.
func peerWindowContractCreateTime(contractId Id) time.Time {
	unixMilli := int64(0)
	for _, b := range contractId[:6] {
		unixMilli = unixMilli<<8 | int64(b)
	}
	return time.UnixMilli(unixMilli)
}

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
	// each direction's open window
	windows [peerWindowDirectionCount]*peerWindow
	// A receive contract created before this, by more than the tolerance, is
	// no part of any window: the mark, then each window's end. It predates what
	// the mode covers, or it is a straggler of a window already reported.
	receiveFloorTime time.Time
}

func (self *peerWindowPeer) idle() bool {
	return !self.accounting && self.windows[PeerWindowSend] == nil && self.windows[PeerWindowReceive] == nil
}

// One contract's final report.
type peerWindowReport struct {
	contractId Id
	byteCount  ByteCount
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
	// windows that have ended and have not reported
	endedWindows []*peerWindow
}

func newPeerWindowLedger(settings PeerWindowSettings) *peerWindowLedger {
	return &peerWindowLedger{
		settings:  settings,
		peers:     map[Id]*peerWindowPeer{},
		contracts: map[Id]*peerWindowContract{},
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
		peer.receiveFloorTime = now
	}
	if peer.idle() {
		delete(self.peers, peerId)
	}
	return true
}

func (self *peerWindowLedger) accounting(peerId Id) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	peer := self.peers[peerId]
	return peer != nil && peer.accounting
}

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

func (self *peerWindowLedger) endWindowWithLock(peer *peerWindowPeer, window *peerWindow) {
	peer.windows[window.direction] = nil
	window.ended = true
	if window.direction == PeerWindowReceive {
		peer.receiveFloorTime = window.endTime
	}
	self.endedWindows = append(self.endedWindows, window)
}

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
	return window
}

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

// A frame rode the contract, so it can be first, and the earliest contract a
// frame rode sets the window's end.
func (self *peerWindowLedger) rideWithLock(contract *peerWindowContract) {
	contract.ridden = true
	if contract.expirationTimeUnixMilli != nil {
		contract.window.endTime = minTime(
			contract.window.endTime,
			time.UnixMilli(*contract.expirationTimeUnixMilli).Add(-self.settings.ExpirationMargin),
		)
	}
}

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
	if contract := self.contracts[contractId]; contract != nil {
		// queued inside a window
		contract.holderCount = 1
		if !contract.window.ended {
			self.rideWithLock(contract)
		}
		return true
	}
	window := self.ridingWindowWithLock(peerId, peer, PeerWindowSend, contractId, now)
	if window == nil {
		return false
	}
	contract := self.addContractWithLock(window, contractId, grantByteCount, expirationTimeUnixMilli)
	contract.holderCount = 1
	self.rideWithLock(contract)
	return true
}

// A receive sequence registered a contract from this peer, or, with `adopted`,
// the frame that marked the peer already rode one that its sequence holds. A
// contract created before the mark or a previous window's end stays out, unless
// adopted: its sender reports it exactly. Reports whether the contract is the
// ledger's.
func (self *peerWindowLedger) received(
	peerId Id,
	contractId Id,
	grantByteCount ByteCount,
	expirationTimeUnixMilli *int64,
	live *contractStatsEntry,
	adopted bool,
	now time.Time,
) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	peer := self.peers[peerId]
	if peer == nil {
		return false
	}
	if contract := self.contracts[contractId]; contract != nil {
		// a later sequence resumed it, or its holder is adopting it again
		if !adopted {
			contract.holderCount += 1
		}
		if !contract.window.reported {
			contract.live = live
		}
		return true
	}
	if !adopted && peerWindowContractCreateTime(contractId).Before(
		peer.receiveFloorTime.Add(-self.settings.EndTolerance),
	) {
		return false
	}
	window := self.ridingWindowWithLock(peerId, peer, PeerWindowReceive, contractId, now)
	if window == nil {
		return false
	}
	contract := self.addContractWithLock(window, contractId, grantByteCount, expirationTimeUnixMilli)
	contract.holderCount = 1
	contract.live = live
	self.rideWithLock(contract)
	return true
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
func (self *peerWindowLedger) withhold(contractId Id, byteCount ByteCount) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	contract := self.contracts[contractId]
	if contract == nil {
		return false
	}
	if !contract.window.reported {
		contract.reportedByteCount += byteCount
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

// Whether the ledger answers for this contract's reports.
func (self *peerWindowLedger) owns(contractId Id) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.contracts[contractId] != nil
}

// Ends every window past its end and hands out the reports of all that have
// ended. A client that could not run at a window's end gets them here when it
// next runs.
func (self *peerWindowLedger) takeEnded(now time.Time) []*peerWindowEnd {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	for _, peer := range self.peers {
		for direction := range peer.windows {
			self.currentWindowWithLock(peer, PeerWindowDirection(direction), now)
		}
	}
	ends := make([]*peerWindowEnd, 0, len(self.endedWindows))
	for _, window := range self.endedWindows {
		reports, byteCount := window.reports()
		window.reported = true
		for contractId, contract := range window.contracts {
			if contract.holderCount == 0 {
				delete(self.contracts, contractId)
			}
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
	self.endedWindows = nil
	return ends
}

// How long until a pass has something to do, and whether anything is open to
// wait for.
func (self *peerWindowLedger) nextWait(now time.Time) (time.Duration, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	wait := time.Duration(0)
	open := false
	for _, peer := range self.peers {
		for _, window := range peer.windows {
			if window == nil {
				continue
			}
			windowWait := max(0, window.endTime.Sub(now))
			if !open || windowWait < wait {
				wait = windowWait
			}
			open = true
		}
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
// window. A caller that marks on receipt of a frame also calls
// `AdoptPeerWindowContract`, since that frame rode a contract already held. Off
// lets the windows already open run to their end, and their reports go out then.
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

// Joins the contract the peer's last frame on `transferKey` rode to the receive
// window, for a caller that marks a peer on receipt of that frame: its contract
// was registered before the mark, so nothing else would bring it in and it would
// report exactly. `transferKey` is the callback's `Peer.TransferKey`, which names
// the one sequence the frame rode. Reports whether a contract joined.
func (self *ContractManager) AdoptPeerWindowContract(peerId Id, transferKey TransferKey) bool {
	ledger := self.peerWindows.Load()
	if ledger == nil {
		return false
	}
	contract := self.client.receiveBuffer.currentContract(peerId, transferKey)
	if contract == nil {
		return false
	}
	adopted := ledger.received(
		peerId,
		contract.contractId,
		contract.transferByteCount,
		contract.expirationTimeUnixMilli,
		contract.statsEntry,
		true,
		time.Now(),
	)
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

// Wakes the worker to look again. Never blocks.
func (self *ContractManager) wakePeerWindows() {
	select {
	case self.peerWindowWake <- struct{}{}:
	default:
	}
}

// Reports each window as it ends. A client that closes with a window open sends
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

// Sends the reports of every window that has ended, each through the same close
// path as any other report, so each keeps its own report id and retry.
func (self *ContractManager) reportPeerWindows(ledger *peerWindowLedger, now time.Time) {
	for _, end := range ledger.takeEnded(now) {
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

// Keeps a report the ledger answers for, and reports whether it did.
func (self *ContractManager) withholdPeerWindowReport(contractId Id, byteCount ByteCount) bool {
	ledger := self.peerWindows.Load()
	if ledger == nil || !ledger.withhold(contractId, byteCount) {
		return false
	}
	// its sequence stops updating the contract here, as at any report
	self.closeContractStats(contractId)
	return true
}

// Whether the ledger answers for this contract, so a close asked for from
// outside changes nothing: the report goes out at the window's end.
func (self *ContractManager) peerWindowOwns(contractId Id) bool {
	ledger := self.peerWindows.Load()
	return ledger != nil && ledger.owns(contractId)
}

// Whether the peer is in the mode.
func (self *ContractManager) peerWindowAccounting(peerId Id) bool {
	ledger := self.peerWindows.Load()
	return ledger != nil && ledger.accounting(peerId)
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
		false,
		time.Now(),
	) {
		self.wakePeerWindows()
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
