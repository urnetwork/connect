package connect

import (
	"os"
	"strings"
	"testing"
	"time"
)

// The two blackhole signals are not equally strong and must not share a bound.
//
// A provider that acknowledges nothing is gone. A provider that acknowledges
// our sends but returns no destination data is demonstrably alive, and may
// simply be carrying a flow that is waiting on a slow origin. Removing an exit
// destroys every flow pinned to it, not just the quiet one, so the ambiguous
// case has to clear a higher bar.
//
// On mainnet the shared 5s bound removed 44 providers out of 44 -- roughly one
// every 18s under load -- and every one of those providers was still
// acknowledging sends, up to 602 of them / 222KB.
func TestBlackholeReceiveTimeoutIsSeparateFromSendTimeout(t *testing.T) {
	settings := DefaultMultiClientSettings()

	if settings.BlackholeReceiveTimeout <= settings.BlackholeTimeout {
		t.Errorf(
			"receive bound %v must be longer than the send bound %v: it is the weaker signal",
			settings.BlackholeReceiveTimeout, settings.BlackholeTimeout,
		)
	}
}

// The receive bound is compared against an age derived from surviving stat
// buckets, and coalesceEventBuckets drops every bucket older than
// StatsWindowDuration. So the age can never exceed roughly
// StatsWindowDuration + StatsWindowBucketDuration, and a receive bound at or
// above that ceiling never fires -- silently, with no error and no log.
//
// This guards the margin. Without it, reducing StatsWindowDuration to make
// stats more responsive would permanently disable the check while every other
// test still passed.
func TestBlackholeReceiveTimeoutIsReachable(t *testing.T) {
	settings := DefaultMultiClientSettings()

	ceiling := settings.StatsWindowDuration + settings.StatsWindowBucketDuration
	if ceiling <= settings.BlackholeReceiveTimeout {
		t.Fatalf(
			"receive bound %v is at or above the reachable ceiling %v (StatsWindowDuration %v + bucket %v): it can never fire",
			settings.BlackholeReceiveTimeout, ceiling,
			settings.StatsWindowDuration, settings.StatsWindowBucketDuration,
		)
	}

	// require real headroom, not a single bucket of luck
	if margin := ceiling - settings.BlackholeReceiveTimeout; margin < 2*settings.StatsWindowBucketDuration {
		t.Errorf(
			"receive bound %v leaves only %v under the ceiling %v; want at least %v",
			settings.BlackholeReceiveTimeout, margin, ceiling, 2*settings.StatsWindowBucketDuration,
		)
	}
}

// A provider acknowledging sends with nothing back must survive the shorter
// send bound -- this is the case that was killing healthy exits.
func TestAckingProviderSurvivesShortWindow(t *testing.T) {
	stats := &clientWindowStats{
		log:               DefaultLogger(),
		firstSendNackTime: time.Now().Add(-10 * time.Second),
		sendAckCount:      7,
		sendAckByteCount:  360,
		receiveAckCount:   0,
	}

	reason := blackholeReasonFromStats(time.Now(), stats, 5*time.Second, 20*time.Second, 30*time.Second)
	if reason != blackholeNone {
		t.Errorf("a provider still acknowledging sends was removed on the send bound: %s", reason)
	}

	// once the longer receive bound elapses it is removed, and the reason says
	// which signal did it
	stats.firstSendNackTime = time.Now().Add(-21 * time.Second)
	reason = blackholeReasonFromStats(time.Now(), stats, 5*time.Second, 20*time.Second, 30*time.Second)
	if reason != blackholeNoReceiveAck {
		t.Errorf("past the receive bound: reason = %q, want %q", reason, blackholeNoReceiveAck)
	}
}

// A provider acknowledging nothing is unambiguously gone and must still be
// removed quickly -- the fix must not slow down the case that works.
func TestSilentProviderStillRemovedOnSendBound(t *testing.T) {
	stats := &clientWindowStats{
		log:               DefaultLogger(),
		firstSendNackTime: time.Now().Add(-10 * time.Second),
		sendAckCount:      0,
		receiveAckCount:   0,
	}

	reason := blackholeReasonFromStats(time.Now(), stats, 5*time.Second, 20*time.Second, 30*time.Second)
	if reason != blackholeNoSendAck {
		t.Errorf("silent provider: reason = %q, want %q", reason, blackholeNoSendAck)
	}
}

// 0 disables the receive check, leaving only the unambiguous signal. This is
// the setting to compare against when measuring how much churn the receive
// branch is responsible for.
func TestBlackholeReceiveTimeoutZeroDisables(t *testing.T) {
	stats := &clientWindowStats{
		log:               DefaultLogger(),
		firstSendNackTime: time.Now().Add(-10 * time.Minute),
		sendAckCount:      7,
		receiveAckCount:   0,
	}

	reason := blackholeReasonFromStats(time.Now(), stats, 5*time.Second, 0, 30*time.Second)
	if reason != blackholeNone {
		t.Errorf("receive check ran with the bound disabled: %s", reason)
	}
}

// The connect signal is independent of the send/receive bounds and keeps its
// own reason, so a capture can tell the three apart. They previously all
// reported an identical error string, which is why 44 field removals could not
// be attributed to a branch.
func TestBlackholeReasonsAreDistinct(t *testing.T) {
	noSyn := &clientWindowStats{
		log:               DefaultLogger(),
		firstSendSynTime:  time.Now().Add(-31 * time.Second),
		firstSendNackTime: time.Time{},
		receiveSynCount:   0,
	}

	reason := blackholeReasonFromStats(time.Now(), noSyn, 5*time.Second, 20*time.Second, 30*time.Second)
	if reason != blackholeNoReceiveSyn {
		t.Errorf("no syn back: reason = %q, want %q", reason, blackholeNoReceiveSyn)
	}

	seen := map[blackholeReason]bool{}
	for _, r := range []blackholeReason{
		blackholeNone, blackholeNoSendAck, blackholeNoReceiveAck, blackholeNoReceiveSyn,
	} {
		if seen[r] {
			t.Errorf("duplicate blackhole reason %q -- a capture could not tell them apart", r)
		}
		seen[r] = true
	}
}

// A clean window is not a blackhole by any signal.
func TestHealthyProviderIsNotBlackholed(t *testing.T) {
	stats := &clientWindowStats{
		log:               DefaultLogger(),
		firstSendNackTime: time.Now().Add(-60 * time.Second),
		firstSendSynTime:  time.Now().Add(-60 * time.Second),
		sendAckCount:      100,
		receiveAckCount:   100,
		receiveSynCount:   3,
	}

	reason := blackholeReasonFromStats(time.Now(), stats, 5*time.Second, 20*time.Second, 30*time.Second)
	if reason != blackholeNone {
		t.Errorf("healthy provider removed: %s", reason)
	}
}

// detectBlackhole must actually consult the decision, and must pass the
// runtime-override receive bound rather than the static setting. Without this
// the decision function could be correct and simply not used -- which is the
// shape of bug that has already shipped here more than once.
func TestDetectBlackholeUsesTheReasonAndOverride(t *testing.T) {
	source, err := readSource("ip_remote_multi_client.go")
	if err != nil {
		t.Fatal(err)
	}

	body, ok := functionBody(source, "func (self *multiClientChannel) detectBlackhole()")
	if !ok {
		t.Fatal("could not find detectBlackhole")
	}

	if !strings.Contains(body, "blackholeReasonFromStats(") {
		t.Error("detectBlackhole does not call blackholeReasonFromStats: the decision is not reached")
	}
	if !strings.Contains(body, "self.reliabilitySettings().BlackholeReceiveTimeout") {
		t.Error("detectBlackhole does not read the receive bound from the runtime override, so the developer control has no effect")
	}
	if !strings.Contains(body, "reason") {
		t.Error("detectBlackhole does not report the reason, so a capture cannot attribute a removal to a branch")
	}
}

func readSource(name string) (string, error) {
	b, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// functionBody returns the source between a function's signature and the
// closing brace at column 0. Good enough for asserting a call site exists,
// which is all it is used for.
func functionBody(source string, signature string) (string, bool) {
	start := strings.Index(source, signature)
	if start < 0 {
		return "", false
	}
	rest := source[start:]
	if end := strings.Index(rest, "\n}\n"); 0 <= end {
		return rest[:end], true
	}
	return rest, true
}
