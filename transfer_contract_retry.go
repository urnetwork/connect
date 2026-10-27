// Pacing for contract requests that the platform did not grant.
//
// A connect deploy reconnects every hosted device at once, so thousands of
// send sequences ask for contracts in the same second and are refused in the
// same second. Every refused sequence asked again on the same fixed 1, 2, 4,
// 5 s schedule, so each retry arrived as one burst. The wait is now drawn
// around each interval (jitterContractRetryWait, CreateContractRetryJitter),
// which spreads a refused wave without asking more often: the mean wait is
// still the interval.
package connect

import (
	mathrand "math/rand"
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
