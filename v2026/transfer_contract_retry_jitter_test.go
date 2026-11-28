// The jittered wait before a send sequence asks again for a contract that has
// not arrived. On the fixed 1, 2, 4, 5 s schedule, sequences refused together
// asked again together, at every step. The jitter is centered on each
// interval, so it spreads them without asking more often.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// One refused sequence asks again after jittered waits. The lowest draw puts
// every wait at the bottom of its range, 0.75 of each 1, 2, 4, 5 s interval,
// so the requests land at 0, 0.75, 2.25, 5.25 and 9 s before the 10 s budget
// ends the acquisition. On the fixed schedule every refused sequence asked at
// 0, 1, 3 and 7 s alike.
func TestSendSequenceContractRetryWaitIsJittered(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		oob := &contractRetryOob{
			clientId: NewId(),
			answer:   func(int) contractRetryAnswer { return contractRetryRefuse },
		}
		settings := contractRetryTestSettings()
		settings.SendBufferSettings.PrewarmOpeningContract = false
		settings.SendBufferSettings.CreateContractTimeout = 10 * time.Second
		settings.SendBufferSettings.CreateContractRetryJitter = 0.5
		settings.SendBufferSettings.createContractRetryRandomForTest = func(n int64) int64 { return 0 }
		settings.SendBufferSettings.backendDegradedForTest = func() bool { return false }
		client := newContractRetryTestClient(t, oob, settings)

		destinationId := NewId()
		if err := <-sendContractRetryMessage(t, client, destinationId); err == nil {
			t.Fatal("a refused acquisition must fail the message")
		}
		if elapsed := time.Since(start); elapsed != 10*time.Second {
			t.Fatalf("acquisition ended after %s, want the 10s budget", elapsed)
		}
		requireContractRetryOffsets(t, "refused sequence", oob.requestOffsets(destinationId, start), []time.Duration{
			0,
			750 * time.Millisecond,
			2250 * time.Millisecond,
			5250 * time.Millisecond,
			9 * time.Second,
		})
	})
}

// Sequences refused in the same instant come back spread out. Eight
// destinations of one client are refused together; with the production random
// source each first retry lands in [0.75, 1.25) s, and they are not one
// instant. On the fixed schedule all eight came back at exactly 1 s.
func TestSendSequenceContractRetriesSpreadAfterMassRefusal(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		oob := &contractRetryOob{
			clientId: NewId(),
			answer:   func(int) contractRetryAnswer { return contractRetryRefuse },
		}
		settings := contractRetryTestSettings()
		settings.SendBufferSettings.PrewarmOpeningContract = false
		// room for exactly one retry: the second falls past the budget, at
		// 0.75 + 1.5 s at the earliest
		settings.SendBufferSettings.CreateContractTimeout = 2 * time.Second
		settings.SendBufferSettings.backendDegradedForTest = func() bool { return false }
		client := newContractRetryTestClient(t, oob, settings)

		destinationIds := []Id{}
		ackErrChannels := []chan error{}
		for range 8 {
			destinationId := NewId()
			destinationIds = append(destinationIds, destinationId)
			ackErrChannels = append(ackErrChannels, sendContractRetryMessage(t, client, destinationId))
		}
		for _, ackErrs := range ackErrChannels {
			if err := <-ackErrs; err == nil {
				t.Fatal("a refused acquisition must fail the message")
			}
		}

		retryOffsets := map[time.Duration]bool{}
		for _, destinationId := range destinationIds {
			offsets := oob.requestOffsets(destinationId, start)
			if len(offsets) != 2 || offsets[0] != 0 {
				t.Fatalf("refused sequence requests at %v, want one at 0 and one retry", offsets)
			}
			if offsets[1] < 750*time.Millisecond || 1250*time.Millisecond <= offsets[1] {
				t.Fatalf("first retry at %s, want within [750ms, 1.25s)", offsets[1])
			}
			retryOffsets[offsets[1]] = true
		}
		if len(retryOffsets) < 2 {
			t.Fatalf("eight refused sequences all retried at %v", retryOffsets)
		}
	})
}

// Uniform in [interval*(1-fraction/2), interval*(1+fraction/2)), clamped to
// that range for any source. The middle draw is the interval itself: the
// range is centered on it, so the mean wait is unchanged. A nonpositive
// fraction or interval is no jitter, and a fraction above 1 is 1.
func TestJitterContractRetryWait(t *testing.T) {
	lowest := func(n int64) int64 { return 0 }
	middle := func(n int64) int64 { return n / 2 }
	highest := func(n int64) int64 { return n - 1 }
	outOfRange := func(n int64) int64 { return 10 * n }
	negative := func(n int64) int64 { return -n }
	cases := []struct {
		interval time.Duration
		fraction float64
		random   func(n int64) int64
		want     time.Duration
	}{
		{time.Second, 0.5, lowest, 750 * time.Millisecond},
		{time.Second, 0.5, middle, time.Second},
		{time.Second, 0.5, highest, 1250*time.Millisecond - time.Nanosecond},
		{5 * time.Second, 0.5, lowest, 3750 * time.Millisecond},
		{5 * time.Second, 0.5, middle, 5 * time.Second},
		{5 * time.Second, 0.5, highest, 6250*time.Millisecond - time.Nanosecond},
		{5 * time.Second, 0.25, lowest, 4375 * time.Millisecond},
		{time.Second, 0.5, outOfRange, 1250*time.Millisecond - time.Nanosecond},
		{time.Second, 0.5, negative, 750 * time.Millisecond},
		{time.Second, 1, lowest, 500 * time.Millisecond},
		{time.Second, 2, highest, 1500*time.Millisecond - time.Nanosecond},
		{time.Second, 0, lowest, time.Second},
		{time.Second, -1, lowest, time.Second},
		{0, 0.5, lowest, 0},
		{-time.Second, 0.5, lowest, -time.Second},
	}
	for _, c := range cases {
		if got := jitterContractRetryWait(c.interval, c.fraction, c.random); got != c.want {
			t.Errorf("jitterContractRetryWait(%s, %v) = %s, want %s", c.interval, c.fraction, got, c.want)
		}
	}
	// the production source stays in range
	for range 1000 {
		if got := jitterContractRetryWait(time.Second, 0.5, nil); got < 750*time.Millisecond || 1250*time.Millisecond <= got {
			t.Fatalf("production jitter %s outside [750ms, 1.25s)", got)
		}
	}
}

// Half of each retry interval is jitter by default.
func TestCreateContractRetryJitterDefault(t *testing.T) {
	AssertEqual(t, DefaultSendBufferSettings().CreateContractRetryJitter, 0.5)
}
