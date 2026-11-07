// The ledger's rules run on a timeline the test owns, since the ledger reads no
// clock. The manager tests drive a real client through its receive buffer and
// read each report at the contract manager, on synctest's fake clock.
package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// A platform contract, as the ledger sees one.
type peerWindowTestContract struct {
	contractId              Id
	grantByteCount          ByteCount
	expirationTimeUnixMilli *int64
}

// A contract the platform created at `createTime` with its 60 minute life.
// `entropy` orders contracts of one millisecond.
func newPeerWindowTestContract(createTime time.Time, entropy byte) peerWindowTestContract {
	var contractId Id
	binary.BigEndian.PutUint64(contractId[:8], uint64(createTime.UnixMilli())<<16)
	contractId[15] = entropy
	expirationTimeUnixMilli := createTime.Add(60 * time.Minute).UnixMilli()
	return peerWindowTestContract{
		contractId:              contractId,
		grantByteCount:          mib(1),
		expirationTimeUnixMilli: &expirationTimeUnixMilli,
	}
}

// A sequence's running count.
func newPeerWindowTestLive(byteCount ByteCount) *contractStatsEntry {
	live := &contractStatsEntry{}
	live.usedByteCount.Store(int64(byteCount))
	return live
}

// A ledger with one marked peer on a fixed timeline.
type peerWindowLedgerFixture struct {
	t      *testing.T
	ledger *peerWindowLedger
	peerId Id
	start  time.Time
}

func newPeerWindowLedgerFixture(t *testing.T) *peerWindowLedgerFixture {
	t.Helper()
	self := &peerWindowLedgerFixture{
		t:      t,
		ledger: newPeerWindowLedger(DefaultPeerWindowSettings()),
		peerId: NewId(),
		start:  time.UnixMilli(1_800_000_000_000),
	}
	if !self.ledger.setAccounting(self.peerId, true, self.start) {
		t.Fatal("the first mark changed nothing")
	}
	return self
}

// A sequence takes the contract and opens it with a head, which counts.
func (self *peerWindowLedgerFixture) take(contract peerWindowTestContract, now time.Time) {
	self.t.Helper()
	self.ledger.taken(self.peerId, contract.contractId, contract.grantByteCount, contract.expirationTimeUnixMilli, now)
	self.ledger.attachLive(contract.contractId, newPeerWindowTestLive(1))
}

func (self *peerWindowLedgerFixture) queue(contract peerWindowTestContract, now time.Time) {
	self.t.Helper()
	self.ledger.queued(self.peerId, contract.contractId, contract.grantByteCount, contract.expirationTimeUnixMilli, now)
}

// A sequence registers the contract, with the head that brought it counted.
func (self *peerWindowLedgerFixture) receive(contract peerWindowTestContract, now time.Time) bool {
	self.t.Helper()
	return self.ledger.received(self.peerId, contract.contractId, contract.grantByteCount, contract.expirationTimeUnixMilli, newPeerWindowTestLive(1), false, now)
}

// The holder closes the contract with this count, and the ledger must keep it.
func (self *peerWindowLedgerFixture) close(contract peerWindowTestContract, byteCount ByteCount) {
	self.t.Helper()
	if !self.ledger.withhold(contract.contractId, byteCount) {
		self.t.Fatalf("a report of contract %s went out inside its window", contract.contractId)
	}
}

// A pass at `now` with no sequence to move.
func (self *peerWindowLedgerFixture) pass(now time.Time) []*peerWindowEnd {
	self.t.Helper()
	for _, move := range self.ledger.takeEnded(now) {
		self.ledger.releaseMove(move.window, now)
	}
	return self.ledger.takeReports(now)
}

// The window that ends by `now`, which must be the only one.
func (self *peerWindowLedgerFixture) end(now time.Time) *peerWindowEnd {
	self.t.Helper()
	ends := self.pass(now)
	if len(ends) != 1 {
		self.t.Fatalf("%d windows ended by %s, want 1", len(ends), now)
	}
	return ends[0]
}

func (self *peerWindowLedgerFixture) requireNoEnd(now time.Time) {
	self.t.Helper()
	if ends := self.pass(now); len(ends) != 0 {
		self.t.Fatalf("%d windows ended by %s, want none", len(ends), now)
	}
}

func requirePeerWindowReports(t *testing.T, end *peerWindowEnd, expected ...peerWindowReport) {
	t.Helper()
	if !slices.Equal(end.reports, expected) {
		t.Fatalf("reports = %+v, want %+v", end.reports, expected)
	}
}

// R(T) at 0, the floor and each 128 KiB multiple, each one byte either side.
func TestWindowReportByteCountBoundaries(t *testing.T) {
	floor := ByteCount(256 * 1024)
	step := ByteCount(128 * 1024)
	cases := []struct {
		byteCount ByteCount
		expected  ByteCount
	}{
		{0, floor},
		{1, floor},
		{step - 1, floor},
		{step, floor},
		{step + 1, floor},
		{floor - 1, floor},
		{floor, floor},
		{floor + 1, floor + step},
	}
	for multiple := ByteCount(3); multiple <= 256; multiple += 1 {
		byteCount := multiple * step
		cases = append(cases,
			struct {
				byteCount ByteCount
				expected  ByteCount
			}{byteCount - 1, byteCount},
			struct {
				byteCount ByteCount
				expected  ByteCount
			}{byteCount, byteCount},
			struct {
				byteCount ByteCount
				expected  ByteCount
			}{byteCount + 1, byteCount + step},
		)
	}
	for _, c := range cases {
		if reported := windowReportByteCount(c.byteCount); reported != c.expected {
			t.Errorf("windowReportByteCount(%d) = %d, want %d", c.byteCount, reported, c.expected)
		}
	}
}

// The contract that opens a window fixes its end 5 minutes before its signed
// deadline, and each report waits for that end: one a contract, the window's
// count on the first and zero on the others.
func TestPeerWindowReportsOncePerContractAtItsEnd(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	first := newPeerWindowTestContract(f.start.Add(time.Second), 1)
	second := newPeerWindowTestContract(f.start.Add(30*time.Minute), 2)
	third := newPeerWindowTestContract(f.start.Add(40*time.Minute), 3)
	f.take(first, f.start.Add(2*time.Second))
	f.take(second, f.start.Add(30*time.Minute))
	f.take(third, f.start.Add(40*time.Minute))
	f.close(second, 1_000)
	f.close(third, 2_000)
	f.close(first, 3_000)

	end := f.start.Add(time.Second + 55*time.Minute)
	f.requireNoEnd(end.Add(-time.Millisecond))
	ended := f.end(end)
	if ended.byteCount != 6_000 {
		t.Fatalf("the window counted %d, want 6000", ended.byteCount)
	}
	requirePeerWindowReports(t, ended,
		peerWindowReport{contractId: first.contractId, byteCount: peerWindowFloorByteCount},
		peerWindowReport{contractId: second.contractId},
		peerWindowReport{contractId: third.contractId},
	)
	f.requireNoEnd(end.Add(time.Hour))
}

// A checkpoint and the reports after it are one count, and a sequence that
// resumed the contract reports on top of what the last one withheld.
func TestPeerWindowSumsAContractsWithheldReports(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	contract := newPeerWindowTestContract(f.start, 1)
	f.receive(contract, f.start)
	f.close(contract, 90*1024)
	f.receive(contract, f.start.Add(10*time.Minute))
	f.close(contract, 90*1024)
	f.receive(contract, f.start.Add(20*time.Minute))
	f.close(contract, 90*1024)
	ended := f.end(f.start.Add(55 * time.Minute))
	if ended.byteCount != 270*1024 {
		t.Fatalf("the window counted %d", ended.byteCount)
	}
	requirePeerWindowReports(t, ended, peerWindowReport{
		contractId: contract.contractId,
		byteCount:  peerWindowFloorByteCount + peerWindowStepByteCount,
	})
}

// A count of bytes on a live sequence's contract is read at the end, acked and
// unacked alike, with no report from the sequence.
func TestPeerWindowReadsAHeldContractsRunningCount(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	contract := newPeerWindowTestContract(f.start, 1)
	live := &contractStatsEntry{}
	f.ledger.received(f.peerId, contract.contractId, contract.grantByteCount, contract.expirationTimeUnixMilli, live, false, f.start)
	live.usedByteCount.Store(int64(peerWindowFloorByteCount + 1))
	ended := f.end(f.start.Add(55 * time.Minute))
	requirePeerWindowReports(t, ended, peerWindowReport{
		contractId: contract.contractId,
		byteCount:  peerWindowFloorByteCount + peerWindowStepByteCount,
	})
}

// What passes the first contract's grant goes on the next contracts in creation
// order, each capped by its grant, however they were taken.
func TestPeerWindowSpillsPastTheFirstGrantInCreationOrder(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	first := newPeerWindowTestContract(f.start, 1)
	second := newPeerWindowTestContract(f.start.Add(time.Minute), 2)
	third := newPeerWindowTestContract(f.start.Add(2*time.Minute), 3)
	fourth := newPeerWindowTestContract(f.start.Add(3*time.Minute), 4)
	second.grantByteCount = kib(512)
	third.grantByteCount = kib(640)
	f.take(first, f.start)
	f.take(third, f.start.Add(3*time.Minute))
	f.take(second, f.start.Add(3*time.Minute))
	f.take(fourth, f.start.Add(3*time.Minute))
	f.close(first, kib(900))
	f.close(second, kib(460))
	f.close(third, 1)
	f.close(fourth, 0)
	// 1,360 KiB and a byte rounds to 1,408 KiB
	requirePeerWindowReports(t, f.end(f.start.Add(55*time.Minute)),
		peerWindowReport{contractId: first.contractId, byteCount: mib(1)},
		peerWindowReport{contractId: second.contractId, byteCount: kib(384)},
		peerWindowReport{contractId: third.contractId},
		peerWindowReport{contractId: fourth.contractId},
	)
}

// The reports stop at the grants, so a window never debits past its contracts.
func TestPeerWindowReportNeverPassesTheGrants(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	first := newPeerWindowTestContract(f.start, 1)
	second := newPeerWindowTestContract(f.start.Add(time.Minute), 2)
	f.take(first, f.start)
	f.take(second, f.start.Add(time.Minute))
	f.close(first, mib(1))
	f.close(second, mib(1)+1)
	requirePeerWindowReports(t, f.end(f.start.Add(55*time.Minute)),
		peerWindowReport{contractId: first.contractId, byteCount: mib(1)},
		peerWindowReport{contractId: second.contractId, byteCount: mib(1)},
	)
}

// A contract that only waited in the queue reached no one, so its peer reports
// nothing for it. It belongs to the window and reports zero, and it is never
// first, whatever its id.
func TestPeerWindowQueuedContractIsNeverFirst(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	queued := newPeerWindowTestContract(f.start, 1)
	first := newPeerWindowTestContract(f.start.Add(time.Second), 2)
	f.take(first, f.start.Add(time.Second))
	f.queue(queued, f.start.Add(time.Second))
	// its queue expired it inside the window
	f.close(queued, 0)
	f.close(first, kib(64))
	requirePeerWindowReports(t, f.end(f.start.Add(time.Second+55*time.Minute)),
		peerWindowReport{contractId: first.contractId, byteCount: peerWindowFloorByteCount},
		peerWindowReport{contractId: queued.contractId},
	)
}

// A contract a sequence took and gave up before a frame rode it counts nothing,
// so its peer may never have heard of it. It is never first, whatever its id, and
// the first is the lowest id that carried a frame by this client's count.
func TestPeerWindowContractNoFrameRodeIsNeverFirst(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	unused := newPeerWindowTestContract(f.start, 1)
	first := newPeerWindowTestContract(f.start.Add(time.Second), 2)
	second := newPeerWindowTestContract(f.start.Add(2*time.Second), 3)
	f.take(unused, f.start)
	f.take(second, f.start.Add(2*time.Second))
	f.take(first, f.start.Add(2*time.Second))
	f.close(unused, 0)
	f.close(first, 1)
	f.close(second, 1)
	requirePeerWindowReports(t, f.end(f.start.Add(55*time.Minute)),
		peerWindowReport{contractId: first.contractId, byteCount: peerWindowFloorByteCount},
		peerWindowReport{contractId: second.contractId},
		peerWindowReport{contractId: unused.contractId},
	)
}

// A window no frame rode has no first, so it reports nothing on any contract.
func TestPeerWindowWithNoFrameReportsZero(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	unused := newPeerWindowTestContract(f.start, 1)
	f.take(unused, f.start)
	f.close(unused, 0)
	requirePeerWindowReports(t, f.end(f.start.Add(55*time.Minute)), peerWindowReport{contractId: unused.contractId})
}

// A contract result can repeat after its contract was taken, and what the ledger
// holds of it stands.
func TestPeerWindowRepeatedQueueResultKeepsTheCount(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	contract := newPeerWindowTestContract(f.start, 1)
	f.take(contract, f.start)
	f.close(contract, kib(300))
	f.queue(contract, f.start.Add(time.Minute))
	requirePeerWindowReports(t, f.end(f.start.Add(55*time.Minute)), peerWindowReport{
		contractId: contract.contractId,
		byteCount:  peerWindowFloorByteCount + peerWindowStepByteCount,
	})
}

// With no listener a window's opening waits for one only as long as the window.
func TestPeerWindowOpeningIsForgottenWithItsWindow(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	f.take(newPeerWindowTestContract(f.start, 1), f.start)
	f.end(f.start.Add(55 * time.Minute))
	if opened := f.ledger.takeOpened(); len(opened) != 0 {
		t.Fatalf("a window that ended is still opening: %+v", opened)
	}
}

// With no window open a queued contract stays out, so its take opens the window,
// and until then its close goes out as it always has.
func TestPeerWindowQueuedContractOpensNothing(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	expired := newPeerWindowTestContract(f.start, 1)
	f.queue(expired, f.start)
	if status := f.ledger.status(f.peerId); status.Send != nil {
		t.Fatalf("a queued contract opened a window: %+v", status.Send)
	}
	if f.ledger.withhold(expired.contractId, 0) {
		t.Fatal("the close of a contract no window holds was withheld")
	}
	taken := newPeerWindowTestContract(f.start, 2)
	f.queue(taken, f.start)
	f.take(taken, f.start.Add(time.Minute))
	status := f.ledger.status(f.peerId)
	if status.Send == nil || !status.Send.StartTime.Equal(f.start.Add(time.Minute)) {
		t.Fatalf("the take did not open the window: %+v", status.Send)
	}
	if !status.Send.EndTime.Equal(f.start.Add(55 * time.Minute)) {
		t.Fatalf("the window ends at %s, not by the contract's signed deadline", status.Send.EndTime)
	}
}

// A peer outside the mode has no ledger state, and its contracts report exactly.
func TestPeerWindowUnmarkedPeerStaysOut(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	otherPeerId := NewId()
	contract := newPeerWindowTestContract(f.start, 1)
	if f.ledger.taken(otherPeerId, contract.contractId, contract.grantByteCount, contract.expirationTimeUnixMilli, f.start) {
		t.Fatal("a contract to an unmarked peer joined a window")
	}
	if f.ledger.received(otherPeerId, contract.contractId, contract.grantByteCount, contract.expirationTimeUnixMilli, newPeerWindowTestLive(1), false, f.start) {
		t.Fatal("a contract from an unmarked peer joined a window")
	}
	if f.ledger.withhold(contract.contractId, 1) {
		t.Fatal("an unmarked peer's report was withheld")
	}
}

// Each direction has its own window, count and end.
func TestPeerWindowDirectionsAreSeparate(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	up := newPeerWindowTestContract(f.start, 1)
	down := newPeerWindowTestContract(f.start.Add(time.Second), 2)
	f.take(up, f.start)
	f.receive(down, f.start.Add(time.Second))
	f.close(up, peerWindowFloorByteCount+1)
	f.close(down, 1)
	sendEnd := f.end(f.start.Add(55 * time.Minute))
	if sendEnd.direction != PeerWindowSend {
		t.Fatalf("direction %d ended first", sendEnd.direction)
	}
	requirePeerWindowReports(t, sendEnd, peerWindowReport{
		contractId: up.contractId,
		byteCount:  peerWindowFloorByteCount + peerWindowStepByteCount,
	})
	requirePeerWindowReports(t, f.end(f.start.Add(time.Second+55*time.Minute)), peerWindowReport{
		contractId: down.contractId,
		byteCount:  peerWindowFloorByteCount,
	})
}

// A client that cannot run at a window's end reports when it next runs: the same
// reports, on the same contracts, however late.
func TestPeerWindowSuspendedClientReportsWhenItNextRuns(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	contract := newPeerWindowTestContract(f.start, 1)
	unused := newPeerWindowTestContract(f.start.Add(time.Minute), 2)
	f.take(contract, f.start)
	f.queue(unused, f.start.Add(time.Minute))
	f.close(contract, kib(20))

	// no pass ran at the window's end
	resumed := f.start.Add(3 * time.Hour)
	wait, open := f.ledger.nextWait(resumed)
	if !open || wait != 0 {
		t.Fatalf("a resumed client waits %s (open=%t) on a window past its end", wait, open)
	}
	requirePeerWindowReports(t, f.end(resumed),
		peerWindowReport{contractId: contract.contractId, byteCount: peerWindowFloorByteCount},
		peerWindowReport{contractId: unused.contractId},
	)
}

// A timer stops with a suspended process, so the wait never runs past a check.
func TestPeerWindowWaitIsBoundedWhileAWindowIsOpen(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	if _, open := f.ledger.nextWait(f.start); open {
		t.Fatal("a marked peer with no window has something to wait for")
	}
	f.take(newPeerWindowTestContract(f.start, 1), f.start)
	wait, open := f.ledger.nextWait(f.start)
	if !open || wait != f.ledger.settings.CheckInterval {
		t.Fatalf("wait = %s (open=%t), want the check interval", wait, open)
	}
	wait, _ = f.ledger.nextWait(f.start.Add(55*time.Minute - time.Second))
	if wait != time.Second {
		t.Fatalf("wait = %s near the end, want 1s", wait)
	}
}

// A contract taken after the window's end, before any pass ran, opens the next
// window and never joins the old one.
func TestPeerWindowContractAfterTheEndOpensTheNextWindow(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	first := newPeerWindowTestContract(f.start, 1)
	next := newPeerWindowTestContract(f.start.Add(56*time.Minute), 2)
	f.take(first, f.start)
	f.ledger.noteActivity(f.peerId, f.start.Add(54*time.Minute))
	f.take(next, f.start.Add(56*time.Minute))
	requirePeerWindowReports(t, f.end(f.start.Add(56*time.Minute)), peerWindowReport{
		contractId: first.contractId,
		byteCount:  peerWindowFloorByteCount,
	})
	status := f.ledger.status(f.peerId)
	if status.Send == nil || !status.Send.EndTime.Equal(f.start.Add(111*time.Minute)) {
		t.Fatalf("the next window = %+v", status.Send)
	}
}

// The sender moves on by its own clock, so its next contract can reach a receiver
// whose window is still open. Created that near the end, it ends the window and
// opens the next, on both clients alike.
func TestPeerWindowContractNearTheEndOpensTheNextWindowOnBothClients(t *testing.T) {
	sender := newPeerWindowLedgerFixture(t)
	receiver := &peerWindowLedgerFixture{
		t:      t,
		ledger: newPeerWindowLedger(DefaultPeerWindowSettings()),
		peerId: sender.peerId,
		start:  sender.start,
	}
	receiver.ledger.setAccounting(receiver.peerId, true, receiver.start)
	first := newPeerWindowTestContract(sender.start, 1)
	// a sender 90 s ahead of the receiver's clock
	next := newPeerWindowTestContract(sender.start.Add(55*time.Minute-90*time.Second), 2)

	sender.take(first, sender.start)
	sender.close(first, 100)
	sender.ledger.noteActivity(sender.peerId, sender.start.Add(54*time.Minute))
	requirePeerWindowReports(t, sender.end(sender.start.Add(55*time.Minute)), peerWindowReport{
		contractId: first.contractId,
		byteCount:  peerWindowFloorByteCount,
	})
	sender.take(next, sender.start.Add(55*time.Minute))

	receiver.receive(first, receiver.start)
	receiver.close(first, 100)
	receiver.ledger.noteActivity(receiver.peerId, receiver.start.Add(54*time.Minute))
	receiver.receive(next, receiver.start.Add(55*time.Minute-90*time.Second))
	requirePeerWindowReports(t, receiver.end(receiver.start.Add(55*time.Minute-90*time.Second)), peerWindowReport{
		contractId: first.contractId,
		byteCount:  peerWindowFloorByteCount,
	})
	senderStatus, receiverStatus := sender.ledger.status(sender.peerId), receiver.ledger.status(receiver.peerId)
	if senderStatus.Send == nil || receiverStatus.Receive == nil ||
		!senderStatus.Send.EndTime.Equal(receiverStatus.Receive.EndTime) {
		t.Fatalf("the clients disagree on the next window: %+v and %+v", senderStatus.Send, receiverStatus.Receive)
	}
}

// A contract created before the tolerance reaches the window's last stretch
// belongs to the window, however late it arrives.
func TestPeerWindowContractBeforeTheToleranceJoinsTheWindow(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	first := newPeerWindowTestContract(f.start, 1)
	late := newPeerWindowTestContract(f.start.Add(53*time.Minute-time.Millisecond), 2)
	f.receive(first, f.start)
	f.receive(late, f.start.Add(54*time.Minute))
	requirePeerWindowReports(t, f.end(f.start.Add(55*time.Minute)),
		peerWindowReport{contractId: first.contractId, byteCount: peerWindowFloorByteCount},
		peerWindowReport{contractId: late.contractId},
	)
}

// A sequence that still holds a contract after its window reported reports it
// later, at a trim or at its exit. Nothing goes out: one contract reports once,
// and that count would be exact.
func TestPeerWindowHolderReportAfterTheWindowIsDropped(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	contract := newPeerWindowTestContract(f.start, 1)
	f.receive(contract, f.start)
	requirePeerWindowReports(t, f.end(f.start.Add(55*time.Minute)), peerWindowReport{
		contractId: contract.contractId,
		byteCount:  peerWindowFloorByteCount,
	})
	if !f.ledger.owns(contract.contractId) {
		t.Fatal("the ledger forgot a contract its holder has yet to report")
	}
	f.close(contract, 12_345)
	if f.ledger.owns(contract.contractId) {
		t.Fatal("the ledger kept a contract after its last holder reported")
	}
}

// A contract with no holder left is forgotten with its window.
func TestPeerWindowForgetsAContractWithNoHolder(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	contract := newPeerWindowTestContract(f.start, 1)
	f.take(contract, f.start)
	f.close(contract, 1)
	f.end(f.start.Add(55 * time.Minute))
	if f.ledger.owns(contract.contractId) {
		t.Fatal("the ledger kept a reported contract with no holder")
	}
}

// A receive contract older than the mark predates what the mode covers: its
// sender reports it exactly, and so must this client.
func TestPeerWindowReceiveContractBeforeTheMarkStaysOut(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	old := newPeerWindowTestContract(f.start.Add(-10*time.Minute), 1)
	if f.receive(old, f.start.Add(time.Second)) {
		t.Fatal("a contract from before the mark joined a window")
	}
	if status := f.ledger.status(f.peerId); status.Receive != nil {
		t.Fatalf("a contract from before the mark opened a window: %+v", status.Receive)
	}
}

// The frame that marks a peer on receipt rode a contract its sequence already
// holds. Adopted, it opens the window, count and all, however old it is.
func TestPeerWindowAdoptedContractOpensTheWindow(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	contract := newPeerWindowTestContract(f.start.Add(-10*time.Minute), 1)
	live := newPeerWindowTestLive(512)
	if !f.ledger.received(f.peerId, contract.contractId, contract.grantByteCount, contract.expirationTimeUnixMilli, live, true, f.start) {
		t.Fatal("the marking frame's contract was not adopted")
	}
	// adopting twice holds it once
	f.ledger.received(f.peerId, contract.contractId, contract.grantByteCount, contract.expirationTimeUnixMilli, live, true, f.start)
	status := f.ledger.status(f.peerId)
	if status.Receive == nil || !status.Receive.EndTime.Equal(f.start.Add(45*time.Minute)) {
		t.Fatalf("the adopted window = %+v", status.Receive)
	}
	f.close(contract, 512)
	ended := f.end(f.start.Add(45 * time.Minute))
	if ended.byteCount != 512 {
		t.Fatalf("the window counted %d, want the marking frame's 512", ended.byteCount)
	}
	if f.ledger.owns(contract.contractId) {
		t.Fatal("the one holder's report left the contract held")
	}
}

// A window lasts its setting at most, whatever the contract's deadline says.
func TestPeerWindowContractWithoutADeadlineLastsTheWindowDuration(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	contract := newPeerWindowTestContract(f.start, 1)
	contract.expirationTimeUnixMilli = nil
	f.take(contract, f.start.Add(time.Minute))
	f.requireNoEnd(f.start.Add(55*time.Minute + time.Minute - time.Millisecond))
	requirePeerWindowReports(t, f.end(f.start.Add(55*time.Minute+time.Minute)), peerWindowReport{
		contractId: contract.contractId,
		byteCount:  peerWindowFloorByteCount,
	})
}

// Turning the mode off lets an open window finish, and after it contracts report
// exactly. A second mark, or a clear of a clear peer, changes nothing.
func TestPeerWindowModeOffLetsTheOpenWindowFinish(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	if f.ledger.setAccounting(f.peerId, true, f.start) {
		t.Fatal("a repeat mark changed the mode")
	}
	contract := newPeerWindowTestContract(f.start, 1)
	inside := newPeerWindowTestContract(f.start.Add(time.Minute), 2)
	f.take(contract, f.start)
	if !f.ledger.setAccounting(f.peerId, false, f.start.Add(time.Second)) {
		t.Fatal("the clear changed nothing")
	}
	if f.ledger.status(f.peerId).Accounting {
		t.Fatal("the mode survived its clear")
	}
	f.take(inside, f.start.Add(time.Minute))
	f.close(contract, 1)
	f.close(inside, 1)
	requirePeerWindowReports(t, f.end(f.start.Add(55*time.Minute)),
		peerWindowReport{contractId: contract.contractId, byteCount: peerWindowFloorByteCount},
		peerWindowReport{contractId: inside.contractId},
	)
	after := newPeerWindowTestContract(f.start.Add(56*time.Minute), 3)
	if f.ledger.taken(f.peerId, after.contractId, after.grantByteCount, after.expirationTimeUnixMilli, f.start.Add(56*time.Minute)) {
		t.Fatal("a contract after the clear joined a window")
	}
	if len(f.ledger.peers) != 0 || len(f.ledger.contracts) != 0 {
		t.Fatalf("the ledger kept %d peers and %d contracts after the last window", len(f.ledger.peers), len(f.ledger.contracts))
	}
}

// Orders ids minted in one millisecond of the fake clock.
var peerWindowTestIdCount atomic.Uint64

// An id with the fake clock in its high bits, as a platform id carries the
// platform's. `NewId` keeps the real clock's last millisecond under synctest.
func newPeerWindowTestId() Id {
	var id Id
	binary.BigEndian.PutUint64(id[:8], uint64(time.Now().UnixMilli())<<16)
	binary.BigEndian.PutUint64(id[8:], peerWindowTestIdCount.Add(1))
	return id
}

// A contract from a source to the harness client, created now with the
// platform's 60 minute life and signed with the client's provider key.
func (self *closeSequencesHarness) newPeerWindowContract(sourceId Id) (Id, *protocol.Frame) {
	self.t.Helper()
	contractId := newPeerWindowTestId()
	expirationTimeUnixMilli := time.Now().Add(60 * time.Minute).UnixMilli()
	storedContractBytes, err := proto.Marshal(&protocol.StoredContract{
		ContractId:              contractId.Bytes(),
		TransferByteCount:       uint64(mib(1)),
		SourceId:                sourceId.Bytes(),
		DestinationId:           self.client.ClientId().Bytes(),
		ExpirationTimeUnixMilli: &expirationTimeUnixMilli,
	})
	if err != nil {
		self.t.Fatal(err)
	}
	contractManager := self.client.ContractManager()
	secret, ok := contractManager.GetProvideSecretKey(protocol.ProvideMode_Network)
	if !ok {
		self.t.Fatal("the client has no provider key")
	}
	return contractId, RequireToFrameWithDefaultProtocolVersion(&protocol.Contract{
		StoredContractBytes: storedContractBytes,
		StoredContractHmac:  SignStoredContract(contractManager.settings, secret, storedContractBytes),
		ProvideMode:         protocol.ProvideMode_Network,
	})
}

// A send window's reports wait for its sequences' next contract, so the platform
// sees that first. They go out at the take.
func TestPeerWindowSendReportsWaitForTheTake(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	first := newPeerWindowTestContract(f.start, 1)
	f.take(first, f.start)
	f.ledger.noteActivity(f.peerId, f.start.Add(54*time.Minute))

	end := f.start.Add(55 * time.Minute)
	moves := f.ledger.takeEnded(end)
	if len(moves) != 1 || !moves[0].take || moves[0].peerId != f.peerId {
		t.Fatalf("moves = %+v, want one that takes", moves)
	}
	if f.ledger.holds(first.contractId) {
		t.Fatal("the ended window still holds its contract")
	}
	if ends := f.ledger.takeReports(end); len(ends) != 0 {
		t.Fatalf("the window reported before the take: %+v", ends[0])
	}
	if wait, open := f.ledger.nextWait(end); !open || wait != f.ledger.settings.MoveTimeout {
		t.Fatalf("wait = %s (open=%t), want the move's timeout", wait, open)
	}
	// what the sequence charged on its way off is in the report
	f.close(first, peerWindowFloorByteCount+1)

	taken := end.Add(100 * time.Millisecond)
	f.take(newPeerWindowTestContract(taken, 2), taken)
	ends := f.ledger.takeReports(taken)
	if len(ends) != 1 {
		t.Fatalf("%d windows reported at the take", len(ends))
	}
	requirePeerWindowReports(t, ends[0], peerWindowReport{
		contractId: first.contractId,
		byteCount:  peerWindowFloorByteCount + peerWindowStepByteCount,
	})
}

// With no take the reports wait out the timeout, and no longer.
func TestPeerWindowSendReportsGoAtTheMoveTimeout(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	first := newPeerWindowTestContract(f.start, 1)
	f.take(first, f.start)
	f.ledger.noteActivity(f.peerId, f.start.Add(54*time.Minute))
	end := f.start.Add(55 * time.Minute)
	f.ledger.takeEnded(end)
	timeout := end.Add(f.ledger.settings.MoveTimeout)
	if ends := f.ledger.takeReports(timeout.Add(-time.Millisecond)); len(ends) != 0 {
		t.Fatalf("the window reported early: %+v", ends[0])
	}
	if ends := f.ledger.takeReports(timeout); len(ends) != 1 {
		t.Fatalf("%d windows reported at the timeout", len(ends))
	}
}

// A receive window has nothing to move, and a window that clears the mode takes
// nothing, so both report at their end.
func TestPeerWindowReportsAtOnceWithNothingToWaitFor(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	f.receive(newPeerWindowTestContract(f.start, 1), f.start)
	f.take(newPeerWindowTestContract(f.start, 2), f.start)
	end := f.start.Add(55 * time.Minute)
	moves := f.ledger.takeEnded(end)
	if len(moves) != 1 || moves[0].take {
		t.Fatalf("moves = %+v, want one that takes nothing", moves)
	}
	if ends := f.ledger.takeReports(end); len(ends) != 2 {
		t.Fatalf("%d windows reported at their end, want both", len(ends))
	}
}

// What clears the mode is the window's own last stretch, however late the client
// runs: activity at its start holds the mode, and a moment before it does not.
func TestPeerWindowActivityIsReadAgainstTheWindowsEnd(t *testing.T) {
	end := time.UnixMilli(1_800_000_000_000).Add(55 * time.Minute)
	cases := []struct {
		activity   time.Duration
		accounting bool
	}{
		{activity: -2 * time.Minute, accounting: true},
		{activity: -2*time.Minute - time.Millisecond, accounting: false},
		{activity: -time.Hour, accounting: false},
		{activity: time.Hour, accounting: true},
	}
	for _, c := range cases {
		f := newPeerWindowLedgerFixture(t)
		f.take(newPeerWindowTestContract(f.start, 1), f.start)
		f.ledger.noteActivity(f.peerId, end.Add(c.activity))
		// a client that next runs three hours late
		f.ledger.takeEnded(end.Add(3 * time.Hour))
		if accounting := f.ledger.accounting(f.peerId); accounting != c.accounting {
			t.Errorf("activity at %s left the mode %t, want %t", c.activity, accounting, c.accounting)
		}
	}
}

// The window a contract's take ended is done with, and the next one it opened keeps
// the sequences already on it: a move leaves them there, and the mode holds whatever
// the activity, since a contract was just taken.
func TestPeerWindowContractNearTheEndKeepsItsPlaceThroughTheMove(t *testing.T) {
	f := newPeerWindowLedgerFixture(t)
	first := newPeerWindowTestContract(f.start, 1)
	near := newPeerWindowTestContract(f.start.Add(54*time.Minute), 2)
	f.take(first, f.start)
	f.take(near, f.start.Add(54*time.Minute))
	if f.ledger.holds(first.contractId) || !f.ledger.holds(near.contractId) {
		t.Fatal("the contract near the end did not take over from the window")
	}
	moves := f.ledger.takeEnded(f.start.Add(54 * time.Minute))
	if len(moves) != 1 || !moves[0].take {
		t.Fatalf("moves = %+v, want the other sequences moved at once", moves)
	}
	if ends := f.ledger.takeReports(f.start.Add(54 * time.Minute)); len(ends) != 1 {
		t.Fatalf("%d windows reported, want the one ended, with no take to wait for", len(ends))
	}
}

// A client with one marked peer, driven through its receive buffer.
func newPeerWindowHarness(t *testing.T) (*closeSequencesHarness, *closeSequencesSender) {
	t.Helper()
	harness := newCloseSequencesHarness(t)
	sender := harness.newSender()
	if !harness.client.ContractManager().SetPeerWindowAccounting(sender.sourceId, true) {
		t.Fatal("the first mark changed nothing")
	}
	return harness, sender
}

func (self *closeSequencesHarness) windowDuration() time.Duration {
	return self.client.settings.ContractManagerSettings.PeerWindow.WindowDuration
}

// The root cause on the receive side: a sequence that idles out inside a window
// reports its current contract as a checkpoint, and that is the report the window
// must not show. It stays withheld, and the window's end reports the floor.
func TestPeerWindowIdleReceiveSequenceSendsNoCheckpoint(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		harness, sender := newPeerWindowHarness(t)
		previousContractId, previousFrame := harness.newPeerWindowContract(sender.sourceId)
		harness.deliver(sender, previousFrame, closeSequencesMessageFrame("previous"))
		currentContractId, currentFrame := harness.newPeerWindowContract(sender.sourceId)
		harness.deliver(sender, currentFrame, closeSequencesMessageFrame("current"))
		synctest.Wait()

		time.Sleep(harness.idleTimeout() + time.Second)
		synctest.Wait()
		if count := len(harness.receiveSequences(sender.sourceId)); count != 0 {
			t.Fatalf("the receive sequence did not idle out: %d live", count)
		}
		harness.requireReports(previousContractId)
		harness.requireReports(currentContractId)

		time.Sleep(harness.windowDuration() - harness.idleTimeout() - 2*time.Second)
		synctest.Wait()
		harness.requireReports(previousContractId)
		harness.requireReports(currentContractId)

		time.Sleep(2 * time.Second)
		synctest.Wait()
		harness.requireReports(previousContractId, closeSequencesReport{acked: uint64(peerWindowFloorByteCount)})
		harness.requireReports(currentContractId, closeSequencesReport{})
	})
}

// The same withholding covers every way a receive sequence reports: the overflow
// trim, and the platform's close request, which leaves the sequence running.
func TestPeerWindowWithholdsTrimAndCloseRequestReports(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		harness, sender := newPeerWindowHarness(t)
		contractIds := []Id{}
		for i := range harness.client.settings.ReceiveBufferSettings.MaxOpenReceiveContract + 2 {
			contractId, frame := harness.newPeerWindowContract(sender.sourceId)
			contractIds = append(contractIds, contractId)
			harness.deliver(sender, frame, closeSequencesMessageFrame(fmt.Sprintf("message-%d", i)))
		}
		synctest.Wait()
		harness.closeSequences(contractIds[len(contractIds)-1:], nil)
		synctest.Wait()
		for _, contractId := range contractIds {
			harness.requireReports(contractId)
		}
		if count := len(harness.receiveSequences(sender.sourceId)); count != 1 {
			t.Fatalf("the close request ended the window's sequence: %d live", count)
		}
		time.Sleep(harness.windowDuration() + time.Second)
		synctest.Wait()
		harness.requireReports(contractIds[0], closeSequencesReport{acked: uint64(peerWindowFloorByteCount)})
		for _, contractId := range contractIds[1:] {
			harness.requireReports(contractId, closeSequencesReport{})
		}
	})
}

// A contract no sequence holds any more is asked for a zero final: inside a window
// that request is withheld too, and the window's end still reports the contract
// once.
func TestPeerWindowWithholdsAZeroFinalRequest(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		harness, sender := newPeerWindowHarness(t)
		contractId, frame := harness.newPeerWindowContract(sender.sourceId)
		harness.deliver(sender, frame, closeSequencesMessageFrame("message"))
		synctest.Wait()
		time.Sleep(harness.idleTimeout() + time.Second)
		synctest.Wait()
		harness.client.ContractManager().requestContractClose(contractId)
		synctest.Wait()
		harness.requireReports(contractId)
		time.Sleep(harness.windowDuration())
		synctest.Wait()
		harness.requireReports(contractId, closeSequencesReport{acked: uint64(peerWindowFloorByteCount)})
	})
}

// A sequence that resumes a contract after its idle exit adds to what the exit
// withheld, and the live count is read at the window's end.
func TestPeerWindowResumedContractCountsBothSequences(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		harness, sender := newPeerWindowHarness(t)
		contractId, frame := harness.newPeerWindowContract(sender.sourceId)
		// the Pack takes the frame, and the sender sends the same proof again
		resumeFrame := &protocol.Frame{
			MessageType:  frame.MessageType,
			MessageBytes: MessagePoolShareReadOnly(frame.MessageBytes),
		}
		payload := closeSequencesMessageFrame(string(bytes.Repeat([]byte("x"), 200*1024)))
		first := harness.deliver(sender, frame, payload)
		synctest.Wait()
		time.Sleep(harness.idleTimeout() + time.Second)
		synctest.Wait()

		// the sender resumes on a new sequence with the same contract
		resumed := &closeSequencesSender{sourceId: sender.sourceId, sequenceId: NewId()}
		second := harness.deliver(resumed, resumeFrame, closeSequencesMessageFrame(string(bytes.Repeat([]byte("y"), 200*1024))))
		synctest.Wait()
		// past the floor, which only the two sequences together reach
		time.Sleep(harness.windowDuration())
		synctest.Wait()
		harness.requireReports(contractId, closeSequencesReport{
			acked: uint64(windowReportByteCount(first + second)),
		})
		if first+second <= peerWindowFloorByteCount {
			t.Fatalf("the two sequences counted %d, not past the floor", first+second)
		}
	})
}

// The report of a contract whose stats are watched shows the sequence closing
// when it does, though the report waits.
func TestPeerWindowWithheldReportClosesTheContractStats(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		harness, sender := newPeerWindowHarness(t)
		contractId, frame := harness.newPeerWindowContract(sender.sourceId)
		harness.deliver(sender, frame, closeSequencesMessageFrame("message"))
		synctest.Wait()
		time.Sleep(harness.idleTimeout() + time.Second)
		synctest.Wait()
		manager := harness.client.ContractManager()
		manager.contractStatsLock.Lock()
		_, open := manager.contractStatsEntries[contractStatsKey{contractId: contractId, receive: true}]
		manager.contractStatsLock.Unlock()
		if open {
			t.Fatal("a withheld close left the contract open in its stats")
		}
	})
}

// A client that closes with a window open reports nothing for it, not even at its
// own shutdown: the mode reports only at a window's end.
func TestPeerWindowCloseWithAnOpenWindowReportsNothing(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		harness, sender := newPeerWindowHarness(t)
		contractId, frame := harness.newPeerWindowContract(sender.sourceId)
		harness.deliver(sender, frame, closeSequencesMessageFrame("message"))
		synctest.Wait()
		if err := harness.client.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		harness.requireReports(contractId)
	})
}

// One scenario's reports in order, ids aside: a contract moved past, a current
// contract checkpointed at the idle exit, a queued contract flushed and a final
// from the manager.
func peerWindowOffScenario(t *testing.T, change func(*closeSequencesHarness)) []string {
	t.Helper()
	reports := []string{}
	synctest.Test(t, func(t *testing.T) {
		harness := newCloseSequencesHarness(t)
		change(harness)
		sender := harness.newSender()
		_, previousFrame := harness.newPeerWindowContract(sender.sourceId)
		harness.deliver(sender, previousFrame, closeSequencesMessageFrame("previous"))
		_, currentFrame := harness.newPeerWindowContract(sender.sourceId)
		harness.deliver(sender, currentFrame, closeSequencesMessageFrame("current"))
		synctest.Wait()
		time.Sleep(harness.idleTimeout() + time.Second)
		synctest.Wait()
		manager := harness.client.ContractManager()
		manager.CheckpointContract(NewId(), 7, 3)
		manager.CloseContract(NewId(), 11, 0)
		harness.queueSendContract(ContractKey{Destination: DestinationId(sender.sourceId)})
		manager.Flush(true)
		synctest.Wait()
		harness.stateLock.Lock()
		defer harness.stateLock.Unlock()
		for _, report := range harness.reports {
			reports = append(reports, fmt.Sprintf("acked=%d unacked=%d checkpoint=%t identity=%t", report.AckedByteCount, report.UnackedByteCount, report.Checkpoint, len(report.ReportId) != 0))
		}
	})
	return reports
}

// With the mode off nothing moves: a default client has no ledger, and a ledger
// marked for someone else leaves every other report byte for byte as it was.
func TestPeerWindowOffReportsAreIdentical(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	baseline := peerWindowOffScenario(t, func(harness *closeSequencesHarness) {
		if harness.client.ContractManager().peerWindows.Load() != nil {
			t.Error("a default client holds a peer window ledger")
		}
	})
	if len(baseline) != 5 {
		t.Fatalf("the scenario made %d reports: %v", len(baseline), baseline)
	}
	marked := peerWindowOffScenario(t, func(harness *closeSequencesHarness) {
		harness.client.ContractManager().SetPeerWindowAccounting(NewId(), true)
	})
	if !slices.Equal(baseline, marked) {
		t.Fatalf("a mark for another peer moved its reports:\n%v\n%v", baseline, marked)
	}
}

// The report a window sends is an ordinary close: it keeps one identity across
// native retries, and a later window's report has its own.
func TestPeerWindowReportKeepsItsIdentityAcrossNativeRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.ControlPingTimeout = 0
	settings.Log = NewNoopLogger()
	settings.SendBufferSettings.AckTimeout = 40 * time.Millisecond
	settings.SendBufferSettings.UnreliableAckTimeout = 40 * time.Millisecond
	settings.SendBufferSettings.SelectiveAckTimeout = 40 * time.Millisecond
	settings.ReceiveBufferSettings.WriteTimeout = time.Millisecond
	sender := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	receiver := NewClient(ctx, ControlId, NewNoContractClientOob(), settings)
	sender.ContractManager().AddNoContractPeer(ControlId)
	receiver.ContractManager().AddNoContractPeer(sender.ClientId())
	t.Cleanup(func() {
		cancel()
		join, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		for _, c := range []*Client{sender, receiver} {
			if err := c.CloseAndWait(join); err != nil {
				t.Error(err)
			}
		}
	})
	sendRoute, ackRoute := make(chan []byte), make(chan []byte)
	sender.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{sendRoute})
	receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{sendRoute})
	sender.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{ackRoute})
	observations := make(chan *protocol.CloseContract, 128)
	receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			message, err := FromFrame(frame)
			if err != nil {
				continue
			}
			if close, ok := message.(*protocol.CloseContract); ok {
				select {
				case observations <- proto.Clone(close).(*protocol.CloseContract):
				case <-ctx.Done():
				}
			}
		}
	})
	next := func() *protocol.CloseContract {
		t.Helper()
		select {
		case m := <-observations:
			return m
		case <-ctx.Done():
			t.Fatal("the window's report did not arrive")
			return nil
		}
	}

	manager := sender.ContractManager()
	peerId := NewId()
	manager.SetPeerWindowAccounting(peerId, true)
	ledger := manager.peerWindows.Load()
	contract := newPeerWindowTestContract(time.Now(), 1)
	ledger.taken(peerId, contract.contractId, contract.grantByteCount, contract.expirationTimeUnixMilli, time.Now())
	manager.CloseContract(contract.contractId, 100, 7)
	select {
	case report := <-observations:
		t.Fatalf("a report went out inside its window: %v", report)
	default:
	}

	manager.reportPeerWindows(ledger, time.Now().Add(time.Hour))
	first, retry := next(), next()
	if RequireIdFromBytes(first.ContractId) != contract.contractId ||
		first.AckedByteCount != uint64(peerWindowFloorByteCount) || first.UnackedByteCount != 0 || first.Checkpoint {
		t.Fatalf("the window's report = %v", first)
	}
	if len(first.ReportId) == 0 || !bytes.Equal(first.ReportId, retry.ReportId) {
		t.Fatalf("a retry changed the report's identity: %x, %x", first.ReportId, retry.ReportId)
	}
}
