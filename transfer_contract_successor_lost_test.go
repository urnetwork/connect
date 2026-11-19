package connect

// A successor that was requested, queued, and closed in the queue unused.
//
// A contract's successor is requested once the contract is projected to run
// out (`ContractPrefetchLeadTime`) and announced once the contract is low
// (`ContractAheadFloorByteCount`). An exchange that ends between the two
// leaves the successor in the destination queue, and the queue closes what no
// sequence took within `ContractQueueExpireTimeout`. The sequence itself idles
// for longer than that before it exits (`IdleTimeout`), so a peer that comes
// back in between resumes on a contract that still counts its successor as
// requested. It then reached the end of the contract with nothing requested
// and nothing queued, and the exhaustion path waited a whole
// `CreateContractRetryInterval` for the successor before asking for one: every
// flow of that peer stalled for a second.
//
// Measured on the latency simulator, whose clients return after about two and
// a half minutes: four in ten sessions of a returning client began with that
// stall, against one in three hundred sessions of a new client, and it began
// where the client's downloaded bytes reached the end of a contract.
//
// The fixtures are those of transfer_contract_successor_test.go. Every
// contract decision happens on the test goroutine. The sequence reads the
// row's clock; the queue expires by wall time, which these rows never wait
// for: the row expires the queue itself, as the manager's periodic pass would.

import (
	"testing"
	"time"
)

const lostSuccessorTestMessageByteCount = ByteCount(4 * 1024)

type lostSuccessorTestFixture struct {
	t        *testing.T
	client   *Client
	oob      *pendingContractCreates
	sequence *SendSequence
	now      time.Time
}

// A sequence whose first exchange was fast enough to have the successor
// requested, and ended while the contract was still too full to announce it.
// The platform granted the successor and the queue holds it.
func newLostSuccessorTestFixture(t *testing.T) *lostSuccessorTestFixture {
	t.Helper()
	client, oob, sequence := newContractSuccessorTestSequence(t, false, false)
	fixture := &lostSuccessorTestFixture{
		t:        t,
		client:   client,
		oob:      oob,
		sequence: sequence,
		now:      time.Unix(1_000_000, 0),
	}
	sequence.successorContractNowForTest = func() time.Time { return fixture.now }
	takeContractSuccessorTestOpening(t, client, oob, sequence)
	// a megabyte a second: due as soon as the minimum is charged
	for oob.count() < 2 {
		fixture.charge()
	}
	grantContractSuccessorTestRequest(t, client, oob, 1)
	if !fixture.queued() || fixture.pending() {
		t.Fatal("the granted successor is not waiting in the queue")
	}
	if !sequence.successorContractRequested || sequence.aheadSendContract != nil {
		t.Fatal("the successor was not left requested and unannounced")
	}
	if remaining := sequence.sendContractRemainingByteCount(); remaining <= sequence.sendBufferSettings.ContractAheadFloorByteCount {
		t.Fatalf("the first exchange left %d bytes, at or under the announcement floor", remaining)
	}
	return fixture
}

func (self *lostSuccessorTestFixture) key() ContractKey {
	return self.sequence.contractMetadata().key
}

func (self *lostSuccessorTestFixture) queued() bool {
	return self.client.ContractManager().hasQueuedContract(self.key())
}

func (self *lostSuccessorTestFixture) pending() bool {
	return self.client.ContractManager().hasPendingCreate(self.key())
}

// One message on the current contract, four milliseconds after the last.
func (self *lostSuccessorTestFixture) charge() {
	self.t.Helper()
	self.now = self.now.Add(4 * time.Millisecond)
	contract := self.sequence.sendContract
	if !contract.canUpdate(lostSuccessorTestMessageByteCount) || !self.sequence.updateContract(lostSuccessorTestMessageByteCount) {
		self.t.Fatalf("the contract ran out after %d contract requests", self.oob.count())
	}
}

// The exchange has ended and the sequence idles for `idle` on its own clock.
// Meanwhile the queue closes the one successor it holds, unused.
func (self *lostSuccessorTestFixture) idleWhileTheQueueExpires(idle time.Duration) {
	self.t.Helper()
	contractManager := self.client.ContractManager()
	expired := contractManager.expireQueuedContractsBefore(time.Now().Add(time.Hour))
	if len(expired) != 1 {
		self.t.Fatalf("the queue expired %d contracts, want the one unused successor", len(expired))
	}
	contractManager.closeContracts(expired)
	if self.queued() || self.pending() {
		self.t.Fatal("the expired successor is still on its way or queued")
	}
	self.now = self.now.Add(idle)
}

// The debit the current contract cannot carry, with the platform answering
// request `index` the moment the sequence waits on its queue for it. Returns
// the granted contract and how long the debit took by the wall clock.
func (self *lostSuccessorTestFixture) exhaust(index int) (Id, time.Duration) {
	self.t.Helper()
	if self.sequence.sendContract.canUpdate(lostSuccessorTestMessageByteCount) {
		self.t.Fatal("the contract still has room for a message")
	}
	var grantedContractId Id
	self.client.sendBuffer.beforeTakeContractForTest = func(sendSequenceId) {
		if self.oob.count() == index+1 && contractSuccessorTestRequestHeld(self.oob, index) {
			grantedContractId = grantContractSuccessorTestRequest(self.t, self.client, self.oob, index)
		}
	}
	defer func() { self.client.sendBuffer.beforeTakeContractForTest = nil }()
	start := time.Now()
	updated := self.sequence.updateContract(lostSuccessorTestMessageByteCount)
	waited := time.Since(start)
	if !updated || grantedContractId == (Id{}) || self.sequence.sendContract.contractId != grantedContractId {
		self.t.Fatalf("the exhausted contract was not replaced by request %d (updated=%t, %d contract requests)", index, updated, self.oob.count())
	}
	return grantedContractId, waited
}

// The peer returns after the queue expiry and within the sequence's idle
// timeout. The successor is requested again once it is due, the platform
// answers, and the debit the contract cannot carry finds the new successor in
// hand: it runs with a zero acquisition timeout, so it succeeds only without
// waiting.
func TestASuccessorClosedInTheQueueIsRequestedAgainWhenTheSequenceResumes(t *testing.T) {
	fixture := newLostSuccessorTestFixture(t)
	sequence, oob := fixture.sequence, fixture.oob
	fixture.idleWhileTheQueueExpires(150 * time.Second)
	if !sequence.successorContractRequested {
		t.Fatal("the idle sequence stopped counting its successor as requested before it looked")
	}

	contract := sequence.sendContract
	createContractTimeout := sequence.sendBufferSettings.CreateContractTimeout
	var grantedContractId Id
	for sequence.sendContract == contract {
		if oob.count() == 3 && contractSuccessorTestRequestHeld(oob, 2) {
			if sequence.sendContractRemainingByteCount() < lostSuccessorTestMessageByteCount {
				t.Fatal("the successor was requested again only as the contract ran out")
			}
			grantedContractId = grantContractSuccessorTestRequest(t, fixture.client, oob, 2)
		}
		if contract.canUpdate(lostSuccessorTestMessageByteCount) {
			fixture.charge()
			continue
		}
		sequence.sendBufferSettings.CreateContractTimeout = 0
		updated := sequence.updateContract(lostSuccessorTestMessageByteCount)
		sequence.sendBufferSettings.CreateContractTimeout = createContractTimeout
		if !updated {
			t.Fatalf("the resumed sequence stalled at the end of its contract after %d contract requests", oob.count())
		}
	}
	if oob.count() != 3 || sequence.successorContractLostCount != 0 {
		t.Fatalf("%d contract requests in all, want the opening, the lost successor and one more; the next contract counts %d lost", oob.count(), sequence.successorContractLostCount)
	}
	if grantedContractId == (Id{}) || sequence.sendContract.contractId != grantedContractId {
		t.Fatal("the sequence continued on a contract the platform did not grant after the resume")
	}
}

// A successor that is still in the queue when the sequence looks is kept: the
// look changes nothing and the sequence looks again an eighth of a queue
// expiry on. Nothing is requested for it.
func TestASuccessorStillInTheQueueIsNotRequestedAgain(t *testing.T) {
	fixture := newLostSuccessorTestFixture(t)
	sequence, oob := fixture.sequence, fixture.oob
	queueExpireTimeout := fixture.client.ContractManager().settings.ContractQueueExpireTimeout
	// the sequence's clock passes the queue expiry while the queue, on the
	// wall clock, has held the successor for milliseconds
	fixture.now = fixture.now.Add(queueExpireTimeout)
	fixture.charge()
	if !sequence.successorContractRequested || sequence.successorContractLostCount != 0 || oob.count() != 2 {
		t.Fatalf("a queued successor was forgotten: requested=%t lost=%d, %d contract requests", sequence.successorContractRequested, sequence.successorContractLostCount, oob.count())
	}
	if want := fixture.now.Add(queueExpireTimeout / 8); !sequence.successorContractLostCheckTime.Equal(want) {
		t.Fatalf("the next look is at %s, want %s", sequence.successorContractLostCheckTime, want)
	}
	if !fixture.queued() {
		t.Fatal("the look took the successor out of the queue")
	}
}

// The same return when the contract runs out before the sequence has looked:
// the exhaustion path asks at once rather than first waiting a retry interval
// for the successor the queue closed, and waits only for the platform's
// answer, which here comes the moment the sequence waits for it.
func TestAnExhaustedContractWhoseSuccessorWasClosedInTheQueueAsksAtOnce(t *testing.T) {
	fixture := newLostSuccessorTestFixture(t)
	sequence, oob := fixture.sequence, fixture.oob
	retryInterval := sequence.sendBufferSettings.CreateContractRetryInterval
	if retryInterval < 500*time.Millisecond {
		t.Fatalf("the retry interval is %s, too short for this row to tell a wait from an answer", retryInterval)
	}
	// the sequence's clock stays short of the queue expiry, so no debit looks
	fixture.idleWhileTheQueueExpires(time.Second)
	for sequence.sendContract.canUpdate(lostSuccessorTestMessageByteCount) {
		fixture.charge()
	}
	if oob.count() != 2 || !sequence.successorContractRequested {
		t.Fatalf("%d contract requests before the contract ran out, want the lost successor still counted as requested", oob.count())
	}
	_, waited := fixture.exhaust(2)
	if oob.count() != 3 {
		t.Fatalf("%d contract requests, want one at exhaustion", oob.count())
	}
	if retryInterval/2 <= waited {
		t.Fatalf("the sequence waited %s for a successor that was not coming before it asked (retry interval %s)", waited, retryInterval)
	}
}

// A successor still on its way when the contract runs out is taken when it
// arrives, and no second request is made for it.
func TestAnExhaustedContractTakesTheSuccessorThatIsOnItsWay(t *testing.T) {
	client, oob, sequence := newContractSuccessorTestSequence(t, false, false)
	fixture := &lostSuccessorTestFixture{t: t, client: client, oob: oob, sequence: sequence, now: time.Unix(1_000_000, 0)}
	sequence.successorContractNowForTest = func() time.Time { return fixture.now }
	takeContractSuccessorTestOpening(t, client, oob, sequence)
	for sequence.sendContract.canUpdate(lostSuccessorTestMessageByteCount) {
		fixture.charge()
	}
	if oob.count() != 2 || !fixture.pending() || fixture.queued() {
		t.Fatalf("%d contract requests as the contract ran out, want the successor's still in flight", oob.count())
	}
	fixture.exhaust(1)
	if oob.count() != 2 {
		t.Fatalf("%d contract requests, want no second request for a successor still in flight", oob.count())
	}
}

// A sequence that goes quiet again and again short of the end of its contract.
// The first successor the queue closes is requested again once it is due; the
// queue closes that one too; and from then on the contract requests none ahead
// of its end, not even at the announcement floor. It asks when it runs out,
// at once. Two unused successors for the contract, then, not one for every
// queue expiry, and the contract that follows starts its own count.
func TestAContractThatLostTwoSuccessorsAsksAgainOnlyWhenItRunsOut(t *testing.T) {
	fixture := newLostSuccessorTestFixture(t)
	sequence, oob := fixture.sequence, fixture.oob
	contract := sequence.sendContract
	retryInterval := sequence.sendBufferSettings.CreateContractRetryInterval

	fixture.idleWhileTheQueueExpires(150 * time.Second)
	for oob.count() == 2 {
		fixture.charge()
	}
	if sequence.successorContractLostCount != 1 || !sequence.successorContractRequested {
		t.Fatalf("after the first quiet spell: %d successors counted lost, requested=%t", sequence.successorContractLostCount, sequence.successorContractRequested)
	}
	grantContractSuccessorTestRequest(t, fixture.client, oob, 2)
	if !fixture.queued() {
		t.Fatal("the second successor is not waiting in the queue")
	}

	fixture.idleWhileTheQueueExpires(150 * time.Second)
	for contract.canUpdate(lostSuccessorTestMessageByteCount) {
		fixture.charge()
		if oob.count() != 3 {
			t.Fatalf("a third successor was requested ahead with %d bytes left", sequence.sendContractRemainingByteCount())
		}
	}
	if sequence.successorContractLostCount != successorContractLostLimit || sequence.successorContractRequested {
		t.Fatalf("after the second quiet spell: %d successors counted lost, requested=%t", sequence.successorContractLostCount, sequence.successorContractRequested)
	}
	if floorByteCount := sequence.sendBufferSettings.ContractAheadFloorByteCount; floorByteCount < sequence.sendContractRemainingByteCount() {
		t.Fatal("the row did not charge the contract down through the announcement floor")
	}

	_, waited := fixture.exhaust(3)
	if oob.count() != 4 || retryInterval/2 <= waited {
		t.Fatalf("at the end of the contract: %d contract requests, waited %s (retry interval %s)", oob.count(), waited, retryInterval)
	}
	if sequence.sendContract == contract || sequence.successorContractLostCount != 0 {
		t.Fatalf("the next contract did not start clean: %d successors counted lost", sequence.successorContractLostCount)
	}
}
