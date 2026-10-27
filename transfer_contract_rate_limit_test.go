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
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

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
	callbacks []OobResultFunction
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
			self.callbacks = append(self.callbacks, callback)
		}()
	}
	if !pending {
		callback(nil, nil)
	}
}

// Fails every request in flight with `err`, in arrival order. Returns how
// many it answered.
func (self *pendingContractOob) failAll(err error) int {
	callbacks := func() []OobResultFunction {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		callbacks := self.callbacks
		self.callbacks = nil
		return callbacks
	}()
	for _, callback := range callbacks {
		callback(nil, err)
	}
	return len(callbacks)
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
	manager := &ContractManager{settings: settings}
	now := time.Unix(1000, 0)
	for _, want := range []time.Duration{5 * time.Second, 10 * time.Second, 10 * time.Second} {
		hold := manager.noteCreateContractRateLimited(now)
		if hold != want {
			t.Fatalf("hold = %s, want %s", hold, want)
		}
		if ignored := manager.noteCreateContractRateLimited(now.Add(time.Second)); ignored != 0 {
			t.Fatalf("a refusal inside the running hold started a %s hold", ignored)
		}
		if want := now.Add(hold); manager.createContractRateLimitHoldUntil != want {
			t.Fatalf("hold until %s, want %s", manager.createContractRateLimitHoldUntil, want)
		}
		now = now.Add(hold)
	}
	if manager.createContractRateLimitCount != 3 {
		t.Fatalf("refusal count = %d, want 3, one per hold", manager.createContractRateLimitCount)
	}
	// complete a request inside one more running hold
	holdUntil := now.Add(manager.noteCreateContractRateLimited(now))
	manager.noteCreateContractCompleted()
	if manager.createContractRateLimitHoldUntil != holdUntil {
		t.Fatalf("a completed request cut the running hold to %s, want %s", manager.createContractRateLimitHoldUntil, holdUntil)
	}
	if hold := manager.noteCreateContractRateLimited(holdUntil); hold != 5*time.Second {
		t.Fatalf("hold after a completed request = %s, want the minimum", hold)
	}

	jitterSettings := DefaultContractManagerSettings()
	jitterSettings.createContractRateLimitRandomForTest = func(n int64) int64 { return 0 }
	jitterManager := &ContractManager{settings: jitterSettings}
	if hold := jitterManager.noteCreateContractRateLimited(time.Unix(1000, 0)); hold != 3750*time.Millisecond {
		t.Fatalf("jittered hold = %s, want the bottom of [3.75s, 6.25s)", hold)
	}

	disabledSettings := DefaultContractManagerSettings()
	disabledSettings.CreateContractRateLimitBackoffMin = 0
	disabledManager := &ContractManager{settings: disabledSettings}
	if hold := disabledManager.noteCreateContractRateLimited(time.Unix(1000, 0)); hold != 0 {
		t.Fatalf("disabled hold = %s", hold)
	}
	if !disabledManager.createContractRateLimitHoldUntil.IsZero() {
		t.Fatalf("disabled hold set an end %s", disabledManager.createContractRateLimitHoldUntil)
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
