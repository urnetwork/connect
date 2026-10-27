// Pacing for contract requests that the platform did not grant.
//
// A connect deploy reconnects every hosted device at once, so thousands of
// send sequences ask for contracts in the same second and are refused in the
// same second. Two client behaviors turned that into synchronized waves:
//
//   - every refused sequence asked again on the same fixed 1, 2, 4, 5 s
//     schedule, so each retry arrived as one burst. The wait is now drawn
//     around each interval (jitterContractRetryWait,
//     CreateContractRetryJitter), which spreads a refused wave without asking
//     more often: the mean wait is still the interval.
//   - a 429 from a hosted owner's contract admission bound counted as a
//     backend failure. Three of them anywhere in the process tripped the
//     process-wide degraded-backend gate, which held every sequence in the
//     process; the next success anywhere released them all within a second,
//     which filled the bound again. A rate limit now holds only the client
//     that was refused, for a jittered exponential hold
//     (ContractManagerSettings.CreateContractRateLimitBackoffMin), and leaves
//     the gate alone (controlRateLimited).
package connect

import (
	"errors"
	mathrand "math/rand"
	"net/http"
	"strings"
	"time"
)

// Uniform in [interval*(1-fraction/2), interval*(1+fraction/2)), centered on
// the interval, so the mean wait is the interval: jitter spreads the waits of
// sequences refused together without adding requests, which a downward-only
// range would (0.5 downward is a 0.75 mean, a third more requests). The top of
// the range exceeds the interval by fraction/2. `random` draws uniformly from
// [0, n) and nil uses math/rand. A nonpositive fraction or interval returns the
// interval unchanged; a fraction above 1 is 1, so a wait is never under half
// the interval.
func jitterContractRetryWait(interval time.Duration, fraction float64, random func(n int64) int64) time.Duration {
	if interval <= 0 || fraction <= 0 {
		return interval
	}
	spread := time.Duration(float64(interval) * min(fraction, 1.0))
	if spread <= 0 {
		return interval
	}
	if random == nil {
		random = mathrand.Int63n
	}
	// clamp a test source to the documented range
	offset := time.Duration(max(0, min(int64(spread)-1, random(int64(spread)))))
	return interval - spread/2 + offset
}

// Whether a failed control round trip is the platform's rate limit rather than
// an outage: a 429 status, which a hosted owner's contract admission bound and
// the api both answer with, or an error that says so in words after losing its
// type on the way. A typed status other than 429 is never a rate limit, and a
// bare "429" in text is not matched: it is as likely a port or a count.
func controlRateLimited(err error) bool {
	if err == nil {
		return false
	}
	var statusErr *HttpStatusError
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode == http.StatusTooManyRequests
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "too many requests") ||
		strings.Contains(message, "rate limit")
}

// The unjittered hold after `count` consecutive rate limits: `minimum`,
// doubling per rate limit, capped at `maximum`. A nonpositive count or minimum
// is no hold.
func createContractRateLimitBackoff(count int, minimum time.Duration, maximum time.Duration) time.Duration {
	if count <= 0 || minimum <= 0 {
		return 0
	}
	if maximum < minimum {
		maximum = minimum
	}
	backoff := minimum
	// bounded: the count keeps climbing while the platform keeps refusing
	for i := 1; i < count && backoff < maximum; i += 1 {
		backoff *= 2
	}
	return min(backoff, maximum)
}
