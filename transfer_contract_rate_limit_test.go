// A contract request refused as a rate limit (a 429 from a hosted owner's
// contract admission bound) holds the refused client's requests and leaves the
// process-wide degraded-backend gate alone. Counted as a backend failure, three
// refusals anywhere in a hosted process tripped the gate for every sequence in
// it, and the next success anywhere released them all within a second.
package connect

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A key for the manager-level rate-limit tests.
var contractRateLimitTestKey = ContractKey{Destination: DestinationId(NewId())}

// The 429 a hosted owner's contract admission bound answers with.
func contractRateLimitTestError() error {
	return &HttpStatusError{
		StatusCode: http.StatusTooManyRequests,
		Status:     "429 Too Many Requests",
		Body:       []byte("hosted contract creation is at its concurrent bound"),
	}
}

// Holds every contract request until the test answers it, so several are in
// flight at once, as on a slow control path, and records when each arrived.
// Takes the frames. Other control is answered empty at once.
type pendingContractOob struct {
	stateLock sync.Mutex
	requests  []contractRetryRequest
	pending   []pendingContractRequest
}

// One contract request in flight and the callback that answers it.
type pendingContractRequest struct {
	destinationId Id
	callback      OobResultFunction
}

func (self *pendingContractOob) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	defer func() {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
	}()
	pending := false
	for _, frame := range frames {
		message, err := FromFrame(frame)
		if err != nil {
			continue
		}
		createContract, ok := message.(*protocol.CreateContract)
		if !ok {
			continue
		}
		destinationId, err := IdFromBytes(createContract.DestinationId)
		if err != nil {
			continue
		}
		pending = true
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.requests = append(self.requests, contractRetryRequest{
				destinationId: destinationId,
				time:          time.Now(),
			})
			self.pending = append(self.pending, pendingContractRequest{
				destinationId: destinationId,
				callback:      callback,
			})
		}()
	}
	if !pending {
		callback(nil, nil)
	}
}

// Fails every request in flight with `err`, in arrival order. Returns how
// many it answered.
func (self *pendingContractOob) failAll(err error) int {
	return self.failWhere(func(Id) bool { return true }, err)
}

// Fails the requests in flight for `destinationId` with `err`.
func (self *pendingContractOob) fail(destinationId Id, err error) int {
	return self.failWhere(func(id Id) bool { return id == destinationId }, err)
}

func (self *pendingContractOob) failWhere(match func(Id) bool, err error) int {
	answered := func() []pendingContractRequest {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		answered := []pendingContractRequest{}
		kept := []pendingContractRequest{}
		for _, request := range self.pending {
			if match(request.destinationId) {
				answered = append(answered, request)
			} else {
				kept = append(kept, request)
			}
		}
		self.pending = kept
		return answered
	}()
	for _, request := range answered {
		request.callback(nil, err)
	}
	return len(answered)
}

// The times of the requests for `destinationId`, as offsets from `start`.
func (self *pendingContractOob) requestOffsets(destinationId Id, start time.Time) []time.Duration {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	offsets := []time.Duration{}
	for _, request := range self.requests {
		if request.destinationId == destinationId {
			offsets = append(offsets, request.time.Sub(start))
		}
	}
	return offsets
}

// A client refused with 429s holds its own contract requests for a jittered
// exponential hold and leaves the process-wide degraded gate alone. Drawing a
// quarter into each range gives holds of 7/8 of 5, then of the 10 s maximum,
// so of the sequence's fixed 0, 1, 3, 7, 12, 17, 22 and 27 s retries only
// those at 0, 7, 17 and 27 s ask; the rest fall in a hold. Before, each 429
// was a backend failure: the third tripped the gate and the sequence stopped
// asking at 3 s, with every other sequence in the process, until a success
// anywhere released them all within a second.
func TestContractRateLimitHoldsClientWithoutDegradingBackend(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		oob := &contractRetryOob{
			clientId: NewId(),
			answer:   func(int) contractRetryAnswer { return contractRetryRateLimit },
		}
		settings := contractRetryTestSettings()
		settings.SendBufferSettings.PrewarmOpeningContract = false
		// the fixed schedule, so only the hold moves the requests
		settings.SendBufferSettings.CreateContractRetryJitter = 0
		// no hold ends on a retry instant
		settings.ContractManagerSettings.createContractRateLimitRandomForTest = func(n int64) int64 { return n / 4 }
		client := newContractRetryTestClient(t, oob, settings)

		destinationId := NewId()
		if err := <-sendContractRetryMessage(t, client, destinationId); err == nil {
			t.Fatal("a rate-limited acquisition must fail the message")
		}
		requireContractRetryOffsets(t, "rate-limited sequence", oob.requestOffsets(destinationId, start), []time.Duration{
			0,
			7 * time.Second,
			17 * time.Second,
			27 * time.Second,
		})
		if isBackendDegraded() {
			t.Fatal("rate limits tripped the process-wide degraded-backend gate")
		}
		// four refusals started four holds; the retries at 1 and 3 s were
		// dropped inside the first, at 12 s inside the second and at 22 s
		// inside the third, the key counted once per hold
		stats := client.ContractManager().LocalStats()
		if stats.CreateContractRateLimitHoldCount != 4 || stats.CreateContractRateLimitDropCount != 3 {
			t.Fatalf("holds = %d, dropped = %d, want 4 and 3", stats.CreateContractRateLimitHoldCount, stats.CreateContractRateLimitDropCount)
		}
	})
}

// Requests in flight together and refused together are one refusal: the
// client holds one step, not one step per request. Three sequences' first
// requests are all sent before any answer; refused together they hold 7/8 of
// 5 s, and each sequence asks again at its 7 s retry. Before, the three
// refusals stepped the backoff three times, the hold grew to 8.75 s, and every
// retry waited until 12 s.
func TestContractRateLimitRefusalsTogetherHoldOneStep(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		oob := &pendingContractOob{}
		settings := contractRetryTestSettings()
		settings.SendBufferSettings.PrewarmOpeningContract = false
		settings.SendBufferSettings.CreateContractRetryJitter = 0
		settings.ContractManagerSettings.createContractRateLimitRandomForTest = func(n int64) int64 { return n / 4 }
		client := newContractTestClientWithOob(t, NewId(), oob, settings)
		// the client joins its requests at close, so answer what is left first
		t.Cleanup(func() { oob.failAll(contractRateLimitTestError()) })

		destinationIds := []Id{NewId(), NewId(), NewId()}
		for _, destinationId := range destinationIds {
			sendContractRetryMessage(t, client, destinationId)
		}
		synctest.Wait()
		if count := oob.failAll(contractRateLimitTestError()); count != len(destinationIds) {
			t.Fatalf("%d requests in flight, want %d", count, len(destinationIds))
		}
		// past the 7 s retries, inside the 12 s ones
		time.Sleep(8 * time.Second)
		synctest.Wait()
		for _, destinationId := range destinationIds {
			requireContractRetryOffsets(t, "sequence refused with two others", oob.requestOffsets(destinationId, start), []time.Duration{0, 7 * time.Second})
		}
		if holds := client.ContractManager().LocalStats().CreateContractRateLimitHoldCount; holds != 1 {
			t.Fatalf("three refusals together started %d holds, want 1", holds)
		}
	})
}

// The hold is the client's, not the sequence's: a second sequence that starts
// inside it sends neither its prewarm nor its own request until the hold ends,
// when both sequences ask once and are granted. Before, the second sequence's
// prewarm went out at once, and so did the refused sequence's next request.
func TestContractRateLimitHoldCoversEveryRequestOfTheClient(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		oob := &contractRetryOob{
			clientId: NewId(),
			answer: func(index int) contractRetryAnswer {
				if index == 0 {
					return contractRetryRateLimit
				}
				return contractRetryGrant
			},
		}
		settings := contractRetryTestSettings()
		settings.SendBufferSettings.PrewarmOpeningContract = true
		settings.SendBufferSettings.CreateContractRetryJitter = 0
		settings.SendBufferSettings.backendDegradedForTest = func() bool { return false }
		settings.ContractManagerSettings.CreateContractRateLimitBackoffMin = 5 * time.Second
		settings.ContractManagerSettings.CreateContractRateLimitJitter = 0.5
		// a hold of 4.375 s
		settings.ContractManagerSettings.createContractRateLimitRandomForTest = func(n int64) int64 { return n / 4 }
		client := newContractRetryTestClient(t, oob, settings)

		// the first sequence's prewarm is refused at 0
		firstDestinationId := NewId()
		sendContractRetryMessage(t, client, firstDestinationId)
		synctest.Wait()
		requireContractRetryOffsets(t, "first sequence", oob.requestOffsets(firstDestinationId, start), []time.Duration{0})

		secondDestinationId := NewId()
		sendContractRetryMessage(t, client, secondDestinationId)
		// past the 1 and 3 s retries, both held, and the 7 s one, after the hold
		time.Sleep(8 * time.Second)
		synctest.Wait()
		requireContractRetryOffsets(t, "first sequence", oob.requestOffsets(firstDestinationId, start), []time.Duration{0, 7 * time.Second})
		requireContractRetryOffsets(t, "second sequence", oob.requestOffsets(secondDestinationId, start), []time.Duration{7 * time.Second})
		if stats := client.ContractManager().LocalStats(); stats.ContractOpenCount != 2 {
			t.Fatalf("open contracts = %d, want both sequences granted after the hold", stats.ContractOpenCount)
		}
	})
}

// A successor that comes due while the client is held is asked for by the
// first write after the hold ends. The hold drops the request; reported as
// made, the sequence marked its successor requested and never asked again
// until the contract ran out.
func TestContractRateLimitHoldDefersASuccessorUntilItEnds(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		oob := &contractRetryOob{
			clientId: NewId(),
			answer: func(index int) contractRetryAnswer {
				// the second request, another key's, is the refused one
				if index == 1 {
					return contractRetryRateLimit
				}
				return contractRetryGrant
			},
		}
		settings := contractRetryTestSettings()
		settings.SendBufferSettings.PrewarmOpeningContract = true
		settings.SendBufferSettings.backendDegradedForTest = func() bool { return false }
		// due once 4 KiB is used, at any rate the test writes
		settings.SendBufferSettings.ContractPrefetchMinUsedByteCount = kib(4)
		settings.SendBufferSettings.ContractPrefetchLeadTime = time.Hour
		// a hold of 4.375 s
		settings.ContractManagerSettings.createContractRateLimitRandomForTest = func(n int64) int64 { return n / 4 }
		client := newContractRetryTestClient(t, oob, settings)

		destinationId := NewId()
		sendContractRetryMessage(t, client, destinationId)
		synctest.Wait()
		requireContractRetryOffsets(t, "opening contract", oob.requestOffsets(destinationId, start), []time.Duration{0})

		otherKey := ContractKey{Destination: DestinationId(NewId())}
		client.ContractManager().CreateContract(otherKey, 0, ByteCount(1024))
		requireContractRetryOffsets(t, "refused request", oob.requestOffsets(otherKey.Destination.DestinationId, start), []time.Duration{0})

		// 8 KiB more brings the successor due inside the hold
		sendContractRetryContent(t, client, destinationId, strings.Repeat("x", 8*1024))
		synctest.Wait()
		requireContractRetryOffsets(t, "successor due inside the hold", oob.requestOffsets(destinationId, start), []time.Duration{0})

		time.Sleep(5 * time.Second)
		sendContractRetryMessage(t, client, destinationId)
		synctest.Wait()
		requireContractRetryOffsets(t, "successor after the hold", oob.requestOffsets(destinationId, start), []time.Duration{0, 5 * time.Second})
	})
}

// A successor whose own request was sent and refused as a rate limit is asked
// for again once the hold ends. A write inside the hold finds it held and
// asks nothing; the first write after it asks. Before, the refused successor
// stayed marked requested and was never asked for until the contract ran out.
func TestContractRateLimitRefusedSuccessorIsAskedAgainAfterTheHold(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		oob := &contractRetryOob{
			clientId: NewId(),
			answer: func(index int) contractRetryAnswer {
				// the second request, the successor's, is the refused one
				if index == 1 {
					return contractRetryRateLimit
				}
				return contractRetryGrant
			},
		}
		settings := contractRetryTestSettings()
		settings.SendBufferSettings.PrewarmOpeningContract = true
		settings.SendBufferSettings.backendDegradedForTest = func() bool { return false }
		// due once 4 KiB is used, at any rate the test writes
		settings.SendBufferSettings.ContractPrefetchMinUsedByteCount = kib(4)
		settings.SendBufferSettings.ContractPrefetchLeadTime = time.Hour
		// a hold of 4.375 s
		settings.ContractManagerSettings.createContractRateLimitRandomForTest = func(n int64) int64 { return n / 4 }
		client := newContractRetryTestClient(t, oob, settings)

		destinationId := NewId()
		sendContractRetryMessage(t, client, destinationId)
		synctest.Wait()
		// 8 KiB more brings the successor due, and its request is refused
		sendContractRetryContent(t, client, destinationId, strings.Repeat("x", 8*1024))
		synctest.Wait()
		requireContractRetryOffsets(t, "opening contract and refused successor", oob.requestOffsets(destinationId, start), []time.Duration{0, 0})

		time.Sleep(time.Second)
		sendContractRetryMessage(t, client, destinationId)
		synctest.Wait()
		requireContractRetryOffsets(t, "successor inside the hold", oob.requestOffsets(destinationId, start), []time.Duration{0, 0})

		time.Sleep(4 * time.Second)
		sendContractRetryMessage(t, client, destinationId)
		synctest.Wait()
		requireContractRetryOffsets(t, "successor after the hold", oob.requestOffsets(destinationId, start), []time.Duration{0, 0, 5 * time.Second})
	})
}

// With the hold disabled, a successor refused as a rate limit is not asked for
// again on every write: nothing would pace that, so it waits for its contract
// to run out, as before the hold. Before, every write asked again, and each ask
// was refused.
func TestContractRateLimitDisabledHoldDoesNotReaskARefusedSuccessor(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		oob := &contractRetryOob{
			clientId: NewId(),
			answer: func(index int) contractRetryAnswer {
				// the opening contract is granted, every successor refused
				if index == 0 {
					return contractRetryGrant
				}
				return contractRetryRateLimit
			},
		}
		settings := contractRetryTestSettings()
		settings.SendBufferSettings.PrewarmOpeningContract = true
		settings.SendBufferSettings.backendDegradedForTest = func() bool { return false }
		settings.SendBufferSettings.ContractPrefetchMinUsedByteCount = kib(4)
		settings.SendBufferSettings.ContractPrefetchLeadTime = time.Hour
		settings.ContractManagerSettings.CreateContractRateLimitBackoffMin = 0
		client := newContractRetryTestClient(t, oob, settings)

		destinationId := NewId()
		sendContractRetryMessage(t, client, destinationId)
		synctest.Wait()
		// these bring the successor due, and its one request is refused
		for range 40 {
			sendContractRetryContent(t, client, destinationId, strings.Repeat("x", 1024))
			synctest.Wait()
		}
		requireContractRetryOffsets(t, "opening contract and one refused successor", oob.requestOffsets(destinationId, start), []time.Duration{0, 0})
	})
}

// A successor due while its client is held costs a write nothing: no queue or
// admission lock and no dropped request, however many writes the hold sees.
// The first write after the hold takes the locks and asks. Before, every write
// in the hold locked the queue and the manager and counted a dropped request.
func TestContractRateLimitHeldSuccessorChecksTakeNoLocks(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		oob := &contractRetryOob{
			clientId: NewId(),
			answer: func(index int) contractRetryAnswer {
				// the second request, another key's, is the refused one
				if index == 1 {
					return contractRetryRateLimit
				}
				return contractRetryGrant
			},
		}
		destinationId := NewId()
		var lockedChecks atomic.Int64
		settings := contractRetryTestSettings()
		settings.SendBufferSettings.PrewarmOpeningContract = true
		settings.SendBufferSettings.backendDegradedForTest = func() bool { return false }
		settings.SendBufferSettings.ContractPrefetchMinUsedByteCount = kib(4)
		settings.SendBufferSettings.ContractPrefetchLeadTime = time.Hour
		settings.SendBufferSettings.beforeSuccessorContractRequestForTest = func(id sendSequenceId) {
			if id.Destination == destinationId {
				lockedChecks.Add(1)
			}
		}
		// a hold of 4.375 s
		settings.ContractManagerSettings.createContractRateLimitRandomForTest = func(n int64) int64 { return n / 4 }
		client := newContractRetryTestClient(t, oob, settings)

		sendContractRetryMessage(t, client, destinationId)
		synctest.Wait()
		client.ContractManager().CreateContract(ContractKey{Destination: DestinationId(NewId())}, 0, ByteCount(1024))

		// these bring the successor due inside the hold
		for range 20 {
			sendContractRetryContent(t, client, destinationId, strings.Repeat("x", 1024))
		}
		synctest.Wait()
		if checks := lockedChecks.Load(); checks != 0 {
			t.Fatalf("writes inside the hold took the successor locks %d times", checks)
		}
		if dropped := client.ContractManager().LocalStats().CreateContractRateLimitDropCount; dropped != 0 {
			t.Fatalf("writes inside the hold counted %d dropped requests", dropped)
		}

		time.Sleep(5 * time.Second)
		sendContractRetryMessage(t, client, destinationId)
		synctest.Wait()
		if checks := lockedChecks.Load(); checks != 1 {
			t.Fatalf("the write after the hold took the successor locks %d times, want 1", checks)
		}
		requireContractRetryOffsets(t, "successor after the hold", oob.requestOffsets(destinationId, start), []time.Duration{0, 5 * time.Second})
	})
}

// A hold counts each contract key it drops once, however often its sequence
// asks, and the next hold counts afresh. Before, the count was one per call.
func TestContractRateLimitDropsCountOncePerKeyPerHold(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		oob := &contractRetryOob{
			clientId: NewId(),
			answer:   func(int) contractRetryAnswer { return contractRetryRateLimit },
		}
		settings := contractRetryTestSettings()
		// a hold of 4.375 s, then of 8.75 s
		settings.ContractManagerSettings.createContractRateLimitRandomForTest = func(n int64) int64 { return n / 4 }
		client := newContractRetryTestClient(t, oob, settings)
		manager := client.ContractManager()

		first := ContractKey{Destination: DestinationId(NewId())}
		second := ContractKey{Destination: DestinationId(NewId())}
		manager.CreateContract(ContractKey{Destination: DestinationId(NewId())}, 0, ByteCount(1024))
		for i := range 5 {
			manager.CreateContract(first, uint64(i), ByteCount(1024))
		}
		for i := range 2 {
			manager.CreateContract(second, uint64(i), ByteCount(1024))
		}
		if dropped := manager.LocalStats().CreateContractRateLimitDropCount; dropped != 2 {
			t.Fatalf("one hold dropped %d keys, want 2", dropped)
		}

		time.Sleep(5 * time.Second)
		manager.CreateContract(ContractKey{Destination: DestinationId(NewId())}, 0, ByteCount(1024))
		for i := range 3 {
			manager.CreateContract(first, uint64(i), ByteCount(1024))
		}
		stats := manager.LocalStats()
		if stats.CreateContractRateLimitHoldCount != 2 || stats.CreateContractRateLimitDropCount != 3 {
			t.Fatalf("holds = %d, dropped = %d, want 2 and 3", stats.CreateContractRateLimitHoldCount, stats.CreateContractRateLimitDropCount)
		}
	})
}

// createContract reports a request in flight when it sent one or joined an
// identical one already pending, and none when a hold dropped it.
func TestCreateContractReportsWhetherARequestIsInFlight(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		oob := &pendingContractOob{}
		settings := contractRetryTestSettings()
		client := newContractTestClientWithOob(t, NewId(), oob, settings)
		t.Cleanup(func() { oob.failAll(contractRateLimitTestError()) })
		manager := client.ContractManager()

		key := ContractKey{Destination: DestinationId(NewId())}
		if !manager.createContract(key, 0, ByteCount(1024)) {
			t.Fatal("a sent request was reported not in flight")
		}
		if !manager.createContract(key, 0, ByteCount(1024)) {
			t.Fatal("a request joined to an identical pending one was reported not in flight")
		}
		requireContractRetryOffsets(t, "coalesced requests", oob.requestOffsets(key.Destination.DestinationId, start), []time.Duration{0})

		oob.failAll(contractRateLimitTestError())
		otherKey := ContractKey{Destination: DestinationId(NewId())}
		if manager.createContract(otherKey, 0, ByteCount(1024)) {
			t.Fatal("a request the hold dropped was reported in flight")
		}
		requireContractRetryOffsets(t, "held request", oob.requestOffsets(otherKey.Destination.DestinationId, start), []time.Duration{})

		time.Sleep(7 * time.Second)
		if !manager.createContract(otherKey, 0, ByteCount(1024)) {
			t.Fatal("a request after the hold was reported not in flight")
		}
		requireContractRetryOffsets(t, "request after the hold", oob.requestOffsets(otherKey.Destination.DestinationId, start), []time.Duration{7 * time.Second})
	})
}

// A queue generation begins one request per identical request, reports an
// identical one already pending, and begins nothing once drained.
func TestContractQueueBeginCreate(t *testing.T) {
	queue := newContractQueue(NewNoopLogger(), false)
	request := contractCreateRequest{key: ContractKey{Destination: DestinationId(NewId())}, seqIndex: 1, transferByteCount: 1024}
	if begun, pending := queue.beginCreate(request); !begun || pending {
		t.Fatalf("first request begun=%t pending=%t, want begun", begun, pending)
	}
	if begun, pending := queue.beginCreate(request); begun || !pending {
		t.Fatalf("identical request begun=%t pending=%t, want pending", begun, pending)
	}
	queue.finishCreate(request)
	queue.drained = true
	if begun, pending := queue.beginCreate(request); begun || pending {
		t.Fatalf("drained queue begun=%t pending=%t, want neither", begun, pending)
	}
}

// A request the platform answered between two rate limits ends the streak, so
// the second rate limit holds the minimum step again. The sequence asks at its
// fixed 0, 7, 12 and 17 s retries: refused at 0, answered with an error result
// at 7, refused at 12 for 7/8 of 5 s, and granted at 17. Had the answer not
// ended the streak, the refusal at 12 would have held 7/8 of 10 s and pushed
// the grant to 22 s.
func TestContractRateLimitStreakEndsWithACompletedRequest(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		oob := &contractRetryOob{
			clientId: NewId(),
			answer: func(index int) contractRetryAnswer {
				switch index {
				case 0, 2:
					return contractRetryRateLimit
				case 1:
					return contractRetryRefuse
				default:
					return contractRetryGrant
				}
			},
		}
		settings := contractRetryTestSettings()
		settings.SendBufferSettings.PrewarmOpeningContract = false
		settings.SendBufferSettings.CreateContractRetryJitter = 0
		settings.SendBufferSettings.backendDegradedForTest = func() bool { return false }
		settings.ContractManagerSettings.createContractRateLimitRandomForTest = func(n int64) int64 { return n / 4 }
		client := newContractRetryTestClient(t, oob, settings)

		destinationId := NewId()
		sendContractRetryMessage(t, client, destinationId)
		time.Sleep(25 * time.Second)
		synctest.Wait()
		requireContractRetryOffsets(t, "refused, answered, refused, granted", oob.requestOffsets(destinationId, start), []time.Duration{
			0,
			7 * time.Second,
			12 * time.Second,
			17 * time.Second,
		})
		if stats := client.ContractManager().LocalStats(); stats.ContractOpenCount != 1 {
			t.Fatalf("open contracts = %d, want the granted contract taken", stats.ContractOpenCount)
		}
	})
}

// With the hold disabled a 429 is still not a backend failure: three in a row
// reach the control service and leave the gate clear. The same three plain
// failures trip it (TestContractManagerCreateContract_OobFailureAccumulatesTowardDegraded).
func TestContractRateLimitDoesNotFeedBackendDegradedGate(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		oob := &contractRetryOob{
			clientId: NewId(),
			answer:   func(int) contractRetryAnswer { return contractRetryRateLimit },
		}
		settings := contractRetryTestSettings()
		settings.ContractManagerSettings.CreateContractRateLimitBackoffMin = 0
		client := newContractRetryTestClient(t, oob, settings)

		contractKey := ContractKey{Destination: DestinationId(NewId())}
		for i := range backendDegradedFailThreshold {
			client.ContractManager().CreateContract(contractKey, uint64(i), ByteCount(1024))
		}
		requireContractRetryOffsets(t, "unheld rate limits", oob.requestOffsets(contractKey.Destination.DestinationId, start), []time.Duration{0, 0, 0})
		if isBackendDegraded() {
			t.Fatalf("%d rate limits tripped the process-wide degraded-backend gate", backendDegradedFailThreshold)
		}
	})
}

// A 429 is a rate limit wherever it is wrapped, and so is an error that says so
// in words. A typed status other than 429 is not, and neither is a 429 that is
// only a port number.
func TestControlRateLimited(t *testing.T) {
	rateLimit := &HttpStatusError{
		StatusCode: http.StatusTooManyRequests,
		Status:     "429 Too Many Requests",
		Body:       []byte("hosted contract creation is at its concurrent bound"),
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"hosted bound", rateLimit, true},
		{"wrapped", fmt.Errorf("local control failed: %w", rateLimit), true},
		{"joined", errors.Join(context.Canceled, rateLimit), true},
		{"typed unavailable", &HttpStatusError{StatusCode: http.StatusServiceUnavailable, Status: "503 Service Unavailable", Body: []byte("rate limit")}, false},
		{"untyped status", errors.New("429 Too Many Requests: slow down"), true},
		{"untyped words", errors.New("connect control rejected: rate limited"), true},
		{"port", errors.New("read tcp 192.0.2.1:54290->198.51.100.2:443: i/o timeout"), false},
		{"deadline", context.DeadlineExceeded, false},
		{"unsupported", errors.New("Not supported."), false},
	}
	for _, c := range cases {
		if got := controlRateLimited(c.err); got != c.want {
			t.Errorf("%s: controlRateLimited(%v) = %t, want %t", c.name, c.err, got, c.want)
		}
	}
}

// Doubles from the minimum per consecutive rate limit, capped at the maximum.
func TestCreateContractRateLimitBackoff(t *testing.T) {
	cases := []struct {
		count            int
		minimum, maximum time.Duration
		want             time.Duration
	}{
		{0, 5 * time.Second, 30 * time.Second, 0},
		{1, 5 * time.Second, 30 * time.Second, 5 * time.Second},
		{2, 5 * time.Second, 30 * time.Second, 10 * time.Second},
		{3, 5 * time.Second, 30 * time.Second, 20 * time.Second},
		{4, 5 * time.Second, 30 * time.Second, 30 * time.Second},
		{1000, 5 * time.Second, 30 * time.Second, 30 * time.Second},
		{3, 5 * time.Second, time.Second, 5 * time.Second},
		{3, 0, 30 * time.Second, 0},
	}
	for _, c := range cases {
		if got := createContractRateLimitBackoff(c.count, c.minimum, c.maximum); got != c.want {
			t.Errorf("createContractRateLimitBackoff(%d, %s, %s) = %s, want %s", c.count, c.minimum, c.maximum, got, c.want)
		}
	}
}

// A refusal after the running hold has ended takes the next step of the
// backoff from `now`; one that arrives while the hold runs answers a request
// sent before it and is ignored, so refusals that arrive together hold one
// step. A completed request restarts the steps without cutting a running hold
// short; the jitter draws around the step; a disabled minimum holds nothing.
func TestContractManagerRateLimitHold(t *testing.T) {
	settings := DefaultContractManagerSettings()
	settings.CreateContractRateLimitJitter = 0
	manager := &ContractManager{settings: settings, localStats: NewContractManagerStats()}
	now := time.Unix(1000, 0)
	for _, want := range []time.Duration{5 * time.Second, 10 * time.Second, 10 * time.Second} {
		hold := manager.noteCreateContractRateLimited(contractRateLimitTestKey, contractRateLimitTestQueue(), manager.createContractRateLimitHoldGeneration, now)
		if hold != want {
			t.Fatalf("hold = %s, want %s", hold, want)
		}
		if ignored := manager.noteCreateContractRateLimited(contractRateLimitTestKey, contractRateLimitTestQueue(), manager.createContractRateLimitHoldGeneration, now.Add(time.Second)); ignored != 0 {
			t.Fatalf("a refusal inside the running hold started a %s hold", ignored)
		}
		if want := now.Add(hold); manager.createContractRateLimitHoldEnd() != want {
			t.Fatalf("hold until %s, want %s", manager.createContractRateLimitHoldEnd(), want)
		}
		now = now.Add(hold)
	}
	if manager.createContractRateLimitCount != 3 {
		t.Fatalf("refusal count = %d, want 3, one per hold", manager.createContractRateLimitCount)
	}
	// complete a request inside one more running hold
	holdUntil := now.Add(manager.noteCreateContractRateLimited(contractRateLimitTestKey, contractRateLimitTestQueue(), manager.createContractRateLimitHoldGeneration, now))
	manager.noteCreateContractCompleted()
	if manager.createContractRateLimitHoldEnd() != holdUntil {
		t.Fatalf("a completed request cut the running hold to %s, want %s", manager.createContractRateLimitHoldEnd(), holdUntil)
	}
	if hold := manager.noteCreateContractRateLimited(contractRateLimitTestKey, contractRateLimitTestQueue(), manager.createContractRateLimitHoldGeneration, holdUntil); hold != 5*time.Second {
		t.Fatalf("hold after a completed request = %s, want the minimum", hold)
	}

	jitterSettings := DefaultContractManagerSettings()
	jitterSettings.createContractRateLimitRandomForTest = func(n int64) int64 { return 0 }
	jitterManager := &ContractManager{settings: jitterSettings, localStats: NewContractManagerStats()}
	if hold := jitterManager.noteCreateContractRateLimited(contractRateLimitTestKey, contractRateLimitTestQueue(), 0, time.Unix(1000, 0)); hold != 3750*time.Millisecond {
		t.Fatalf("jittered hold = %s, want the bottom of [3.75s, 6.25s)", hold)
	}

	disabledSettings := DefaultContractManagerSettings()
	disabledSettings.CreateContractRateLimitBackoffMin = 0
	disabledManager := &ContractManager{settings: disabledSettings, localStats: NewContractManagerStats()}
	if hold := disabledManager.noteCreateContractRateLimited(contractRateLimitTestKey, contractRateLimitTestQueue(), 0, time.Unix(1000, 0)); hold != 0 {
		t.Fatalf("disabled hold = %s", hold)
	}
	if !disabledManager.createContractRateLimitHoldEnd().IsZero() {
		t.Fatalf("disabled hold set an end %s", disabledManager.createContractRateLimitHoldEnd())
	}
}

// A refusal of a request admitted before the last hold began does not step the
// backoff, even when it arrives after that hold ended: the request was in
// flight when the hold started, and the hold already answered its round. A
// request admitted after the hold, refused, does step it. Before, the slow
// refusal started a 10 s hold.
func TestContractManagerRateLimitIgnoresRequestsSentBeforeTheHold(t *testing.T) {
	settings := DefaultContractManagerSettings()
	settings.CreateContractRateLimitJitter = 0
	manager := &ContractManager{settings: settings, localStats: NewContractManagerStats()}
	start := time.Unix(1000, 0)
	before := manager.createContractRateLimitHoldGeneration
	if hold := manager.noteCreateContractRateLimited(contractRateLimitTestKey, contractRateLimitTestQueue(), before, start); hold != 5*time.Second {
		t.Fatalf("first hold = %s, want 5s", hold)
	}
	if hold := manager.noteCreateContractRateLimited(contractRateLimitTestKey, contractRateLimitTestQueue(), before, start.Add(6*time.Second)); hold != 0 {
		t.Fatalf("a late refusal of a request sent before the hold started a %s hold", hold)
	}
	if hold := manager.noteCreateContractRateLimited(contractRateLimitTestKey, contractRateLimitTestQueue(), manager.createContractRateLimitHoldGeneration, start.Add(7*time.Second)); hold != 10*time.Second {
		t.Fatalf("a refusal of a request sent after the hold held %s, want the next step's 10s", hold)
	}
}

// End to end: two requests are in flight; refusing one starts a hold, and the
// other's refusal, arriving after that hold ended, holds nothing, so a request
// right after it goes out. Before, the late refusal started a second, longer
// hold that dropped the next request.
func TestContractRateLimitLateRefusalOfAnEarlierRequestHoldsNothing(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		oob := &pendingContractOob{}
		settings := contractRetryTestSettings()
		// a hold of 4.375 s
		settings.ContractManagerSettings.createContractRateLimitRandomForTest = func(n int64) int64 { return n / 4 }
		client := newContractTestClientWithOob(t, NewId(), oob, settings)
		t.Cleanup(func() { oob.failAll(contractRateLimitTestError()) })
		manager := client.ContractManager()

		slow := ContractKey{Destination: DestinationId(NewId())}
		fast := ContractKey{Destination: DestinationId(NewId())}
		manager.CreateContract(slow, 0, ByteCount(1024))
		manager.CreateContract(fast, 0, ByteCount(1024))
		oob.fail(fast.Destination.DestinationId, contractRateLimitTestError())

		time.Sleep(6 * time.Second)
		oob.fail(slow.Destination.DestinationId, contractRateLimitTestError())
		next := ContractKey{Destination: DestinationId(NewId())}
		if !manager.createContract(next, 0, ByteCount(1024)) {
			t.Fatal("a request after the hold was dropped: the late refusal started a hold")
		}
		requireContractRetryOffsets(t, "request after the late refusal", oob.requestOffsets(next.Destination.DestinationId, start), []time.Duration{6 * time.Second})
		if holds := manager.LocalStats().CreateContractRateLimitHoldCount; holds != 1 {
			t.Fatalf("started %d holds, want 1", holds)
		}
	})
}

// A queue for the bare managers' refusals, never drained.
func contractRateLimitTestQueue() *contractQueue {
	return newContractQueue(NewNoopLogger(), false)
}

// A refused key is kept only while its sequence may ask for it again. Asking
// for it again clears it, and so does flushing its queue at the sequence's
// exit; a refusal that arrives after the flush adds none. Before, that
// refusal added a key nothing would clear.
func TestContractRateLimitRefusedKeyLifetime(t *testing.T) {
	refused := func(t *testing.T, run func(t *testing.T, manager *ContractManager, oob *pendingContractOob, key ContractKey)) {
		GetMessagePoolAggregateStats()
		resetBackendDegraded()
		defer resetBackendDegraded()
		synctest.Test(t, func(t *testing.T) {
			oob := &pendingContractOob{}
			settings := contractRetryTestSettings()
			// a hold of 4.375 s
			settings.ContractManagerSettings.createContractRateLimitRandomForTest = func(n int64) int64 { return n / 4 }
			client := newContractTestClientWithOob(t, NewId(), oob, settings)
			t.Cleanup(func() { oob.failAll(contractRateLimitTestError()) })
			run(t, client.ContractManager(), oob, ContractKey{Destination: DestinationId(NewId())})
		})
	}
	t.Run("asked for again", func(t *testing.T) {
		refused(t, func(t *testing.T, manager *ContractManager, oob *pendingContractOob, key ContractKey) {
			manager.CreateContract(key, 0, ByteCount(1024))
			oob.fail(key.Destination.DestinationId, contractRateLimitTestError())
			if !manager.createContractRateLimitedKey(key) {
				t.Fatal("a refused key was not kept")
			}
			time.Sleep(5 * time.Second)
			if !manager.createContract(key, 0, ByteCount(1024)) {
				t.Fatal("the key was not asked for again after the hold")
			}
			if manager.createContractRateLimitedKey(key) {
				t.Error("a key asked for again is still refused")
			}
		})
	})
	t.Run("flushed", func(t *testing.T) {
		refused(t, func(t *testing.T, manager *ContractManager, oob *pendingContractOob, key ContractKey) {
			manager.CreateContract(key, 0, ByteCount(1024))
			oob.fail(key.Destination.DestinationId, contractRateLimitTestError())
			if !manager.createContractRateLimitedKey(key) {
				t.Fatal("a refused key was not kept")
			}
			manager.FlushContractQueue(key, false)
			if manager.createContractRateLimitedKey(key) {
				t.Error("a key whose queue was flushed is still refused")
			}
		})
	})
	t.Run("refused after the flush", func(t *testing.T) {
		refused(t, func(t *testing.T, manager *ContractManager, oob *pendingContractOob, key ContractKey) {
			manager.CreateContract(key, 0, ByteCount(1024))
			manager.FlushContractQueue(key, false)
			oob.fail(key.Destination.DestinationId, contractRateLimitTestError())
			if manager.createContractRateLimitedKey(key) {
				t.Error("a refusal after the flush kept a key nothing clears")
			}
			if !manager.createContractRateLimitHeld(time.Now()) {
				t.Error("a refusal after the flush did not hold the client")
			}
		})
	})
}

// A request admitted after a refusal has read its clock, but before that
// refusal takes the manager lock and starts its hold, was admitted before the
// hold began: its refusal, arriving after the hold ended, steps nothing.
// Before, the hold's start was the refusal's clock read, the request looked
// sent after it, and its late refusal started a second, longer hold.
func TestContractRateLimitRequestAdmittedAsTheHoldBeginsStepsNothing(t *testing.T) {
	GetMessagePoolAggregateStats()
	resetBackendDegraded()
	defer resetBackendDegraded()
	synctest.Test(t, func(t *testing.T) {
		oob := &pendingContractOob{}
		settings := contractRetryTestSettings()
		// a hold of 4.375 s
		settings.ContractManagerSettings.createContractRateLimitRandomForTest = func(n int64) int64 { return n / 4 }
		client := newContractTestClientWithOob(t, NewId(), oob, settings)
		t.Cleanup(func() { oob.failAll(contractRateLimitTestError()) })
		manager := client.ContractManager()

		first := ContractKey{Destination: DestinationId(NewId())}
		between := ContractKey{Destination: DestinationId(NewId())}
		manager.CreateContract(first, 0, ByteCount(1024))
		manager.testingBeforeCreateContractRateLimitedLock = func() {
			manager.testingBeforeCreateContractRateLimitedLock = nil
			time.Sleep(time.Millisecond)
			if !manager.createContract(between, 0, ByteCount(1024)) {
				t.Error("the request between the refusal's clock read and its lock was not admitted")
			}
		}
		oob.fail(first.Destination.DestinationId, contractRateLimitTestError())
		if !manager.createContractRateLimitHeld(time.Now()) {
			t.Fatal("the first refusal started no hold")
		}

		time.Sleep(6 * time.Second)
		oob.fail(between.Destination.DestinationId, contractRateLimitTestError())
		if manager.createContractRateLimitHeld(time.Now()) {
			t.Error("the late refusal of a request admitted before the hold began started a hold")
		}
		if holds := manager.LocalStats().CreateContractRateLimitHoldCount; holds != 1 {
			t.Fatalf("started %d holds, want 1", holds)
		}
	})
}

// The backoff starts over once the last hold ended more than the longest step
// ago. Four refusals in a row climb to the 10 s step, and one exactly the
// longest step after the last hold ended keeps climbing; one an hour later
// holds the minimum again. Before, the count never aged, and a refusal an hour
// after four others held for the longest step.
func TestContractManagerRateLimitStreakAgesOut(t *testing.T) {
	settings := DefaultContractManagerSettings()
	settings.CreateContractRateLimitJitter = 0
	manager := &ContractManager{settings: settings, localStats: NewContractManagerStats()}
	now := time.Unix(1000, 0)
	for range 4 {
		now = now.Add(manager.noteCreateContractRateLimited(contractRateLimitTestKey, contractRateLimitTestQueue(), manager.createContractRateLimitHoldGeneration, now))
	}
	edge := now.Add(settings.CreateContractRateLimitBackoffMax)
	if hold := manager.noteCreateContractRateLimited(contractRateLimitTestKey, contractRateLimitTestQueue(), manager.createContractRateLimitHoldGeneration, edge); hold != 10*time.Second {
		t.Fatalf("hold a longest step after the last = %s, want the streak's 10s", hold)
	}
	quiet := manager.createContractRateLimitHoldEnd().Add(time.Hour)
	if hold := manager.noteCreateContractRateLimited(contractRateLimitTestKey, contractRateLimitTestQueue(), manager.createContractRateLimitHoldGeneration, quiet); hold != 5*time.Second {
		t.Fatalf("hold after an hour of quiet = %s, want the minimum", hold)
	}
	if manager.createContractRateLimitCount != 1 {
		t.Fatalf("refusal count after an hour of quiet = %d, want 1", manager.createContractRateLimitCount)
	}
}

// A rate limit holds the client from the sequences' longest retry interval,
// with half of each step as jitter, and the longest hold drawn, 12.5 s, ends
// before a window judges its client unhealthy for the silence.
func TestCreateContractRateLimitDefaults(t *testing.T) {
	sendBufferSettings := DefaultSendBufferSettings()
	contractManagerSettings := DefaultContractManagerSettings()
	AssertEqual(t, contractManagerSettings.CreateContractRateLimitBackoffMin, sendBufferSettings.CreateContractRetryMaxInterval)
	AssertEqual(t, contractManagerSettings.CreateContractRateLimitJitter, 0.5)
	longestHold := jitterContractRetryWait(
		contractManagerSettings.CreateContractRateLimitBackoffMax,
		contractManagerSettings.CreateContractRateLimitJitter,
		func(n int64) int64 { return n - 1 },
	)
	AssertEqual(t, longestHold+time.Nanosecond, 12500*time.Millisecond)
	if unhealthy := DefaultMultiClientSettings().StatsWindowMaxUnhealthyDuration; unhealthy <= longestHold {
		t.Fatalf("the longest hold %s reaches the %s a window allows a silent client", longestHold, unhealthy)
	}
}
