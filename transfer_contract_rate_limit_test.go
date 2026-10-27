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
	"testing"
	"testing/synctest"
	"time"
)

// A client refused with 429s holds its own contract requests for a jittered
// exponential hold and leaves the process-wide degraded gate alone. Drawing a
// quarter into each range gives holds of 7/8 of 5, 10 and 20 s, so of the
// sequence's fixed 0, 1, 3, 7, 12, 17, 22 and 27 s retries only those at 0, 7
// and 17 s ask; the rest fall in a hold. Before, each 429 was a backend
// failure: the third tripped the gate and the sequence stopped asking at 3 s,
// with every other sequence in the process, until a success anywhere released
// them all within a second.
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
		settings.ContractManagerSettings.CreateContractRateLimitBackoffMin = 5 * time.Second
		settings.ContractManagerSettings.CreateContractRateLimitBackoffMax = 30 * time.Second
		settings.ContractManagerSettings.CreateContractRateLimitJitter = 0.5
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
		})
		if isBackendDegraded() {
			t.Fatal("rate limits tripped the process-wide degraded-backend gate")
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

// Each rate limit takes the next step of the backoff and holds from `now`; a
// completed request restarts the steps without cutting a running hold short;
// the jitter draws from the top of the step; a disabled minimum holds nothing.
func TestContractManagerRateLimitHold(t *testing.T) {
	settings := DefaultContractManagerSettings()
	settings.CreateContractRateLimitJitter = 0
	manager := &ContractManager{settings: settings}
	now := time.Unix(1000, 0)
	for _, want := range []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 24 * time.Second, 24 * time.Second} {
		if hold := manager.noteCreateContractRateLimited(now); hold != want {
			t.Fatalf("hold = %s, want %s", hold, want)
		}
	}
	if want := now.Add(24 * time.Second); manager.createContractRateLimitHoldUntil != want {
		t.Fatalf("hold until %s, want %s", manager.createContractRateLimitHoldUntil, want)
	}
	manager.noteCreateContractCompleted()
	if hold := manager.noteCreateContractRateLimited(now); hold != 5*time.Second {
		t.Fatalf("hold after a completed request = %s, want the minimum", hold)
	}
	if want := now.Add(24 * time.Second); manager.createContractRateLimitHoldUntil != want {
		t.Fatalf("a shorter hold cut the running one to %s, want %s", manager.createContractRateLimitHoldUntil, want)
	}

	jitterSettings := DefaultContractManagerSettings()
	jitterSettings.createContractRateLimitRandomForTest = func(n int64) int64 { return 0 }
	jitterManager := &ContractManager{settings: jitterSettings}
	if hold := jitterManager.noteCreateContractRateLimited(now); hold != 3750*time.Millisecond {
		t.Fatalf("jittered hold = %s, want the bottom of [3.75s, 6.25s)", hold)
	}

	disabledSettings := DefaultContractManagerSettings()
	disabledSettings.CreateContractRateLimitBackoffMin = 0
	disabledManager := &ContractManager{settings: disabledSettings}
	if hold := disabledManager.noteCreateContractRateLimited(now); hold != 0 {
		t.Fatalf("disabled hold = %s", hold)
	}
	if !disabledManager.createContractRateLimitHoldUntil.IsZero() {
		t.Fatalf("disabled hold set an end %s", disabledManager.createContractRateLimitHoldUntil)
	}
}

// A rate limit holds the client from the sequences' longest retry interval,
// with half of each step as jitter, and the longest hold drawn ends at their
// acquisition budget.
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
	AssertEqual(t, longestHold+time.Nanosecond, sendBufferSettings.CreateContractTimeout)
}
