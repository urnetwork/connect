//go:build !js && !windows

// Deterministic controls for the storm oracle use legal quarantine and late
// publication, then corrupt scalar receipts to prove its refusal boundaries.
package connect

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Both the concurrent storm and its deterministic policy controls use the
// same exact source, Transfer and reset-generation verdict.
func privateReraceStormProblems(trace *privateReraceReplayTrace, harness *reraceHarness, path *IpPath) []string {
	return trace.flowProblems(harness, path)
}

// The first source is bound when quarantine runs. Its real reset precedes a
// new source generation; only a later provider publication may retry that
// generation's paid SYN. No timing or source admission mechanism is replaced.
func TestReraceStormOracleAccountsForQuarantineAndLatePublication(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		trace := &privateReraceReplayTrace{}
		var initial atomic.Bool
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "quarantined", tier: 0, answer: func(*IpPath) reraceAnswer {
				if !initial.Swap(true) {
					return reraceSilent
				}
				return reraceSignal
			}, clientSettings: trace.settings("quarantined")},
			{name: "refused", tier: 1, answer: reraceAlways(reraceSignal), clientSettings: trace.settings("refused")},
		}, nil)
		trace.watch(harness)
		path := reraceFlow(4, 0, 53191)
		if !harness.syn(path) || harness.boundTo(path) != harness.channel("quarantined") {
			t.Fatal("original SYN did not acquire the actual quarantine candidate")
		}
		original := harness.update(path)
		client := harness.channel("quarantined")
		client.setQuarantined(blackholeNoReceiveSyn)
		harness.multiClient.migrateClientFlows(client, "bench")
		harness.settle()
		if !original.IsDone() || !original.sourceReset.Load() || len(harness.delivered(path)) != 1 ||
			harness.metrics().QuarantineTcpResets != 1 {
			t.Fatal("scheduled quarantine did not retire and reset exactly its bound source")
		}
		harness.sleep(time.Second)
		if !harness.syn(path) {
			t.Fatal("the second real source SYN was refused")
		}
		current := harness.update(path)
		if current == nil || current == original || len(harness.exit("refused").syns(path)) != 1 {
			t.Fatal("the second source did not establish a distinct refused generation")
		}
		owner := synRetryControlOwner(t, current)
		harness.sleep(time.Second)
		late := harness.addExit(reraceExitSpec{name: "late", tier: 2, answer: reraceAlways(reraceSignal), clientSettings: trace.settings("late")})
		harness.waitForExits(3)
		harness.settle()
		if len(late.syns(path)) != 1 || len(harness.sentSyns(path)) != 2 || harness.update(path) != current ||
			late.syns(path)[0].at.Sub(harness.sentSyns(path)[1]) <= reraceSendTimeout {
			t.Fatalf("real late publication did not retry the exact retained source after its native offer returned: late_syns=%d native_syns=%d same_generation=%t", len(late.syns(path)), len(harness.sentSyns(path)), harness.update(path) == current)
		}
		current.stateLock.Lock()
		sameOwner := current.synRetry == owner && owner.validWithLock(current)
		current.stateLock.Unlock()
		if !sameOwner {
			t.Fatal("late publication replaced the original paid SYN owner")
		}
		if problems := privateReraceStormProblems(trace, harness, path); len(problems) != 0 {
			t.Fatalf("legal quarantine/publication failed the storm oracle: %v", problems)
		}
	})
}

// Corrupt one boundary at a time. A first wire write is not proof of a source
// offer, and distinct Transfer identities cannot hide a duplicated source SYN.
func TestReraceStormOracleRejectsDuplicateAndUnownedSyns(t *testing.T) {
	const port = 53192
	source, sequence, message, remote := NewId(), NewId(), NewId(), NewId()
	valid := []privateReraceReplayEvent{
		{kind: "offer", port: port, offer: 1, native: 1, valid: true},
		{kind: "materialized", port: port, offer: 1, native: 1, source: source, sequence: sequence, message: message, number: 7, remote: remote},
		{kind: "wire", port: port, source: source, sequence: sequence, message: message, number: 7, remote: remote, sendCount: 1},
	}
	if problems := privateReraceReplayProblems(valid, port); len(problems) != 0 {
		t.Fatalf("positive source/wire control: %v", problems)
	}
	for _, test := range []struct {
		name   string
		change func([]privateReraceReplayEvent) []privateReraceReplayEvent
		want   string
	}{
		{name: "duplicate", want: "duplicate SYN", change: func(events []privateReraceReplayEvent) []privateReraceReplayEvent {
			materialized, wire := events[1], events[2]
			materialized.message, wire.message = NewId(), NewId()
			wire.message = materialized.message
			return append(events, materialized, wire)
		}},
		{name: "unowned", want: "unowned wire", change: func(events []privateReraceReplayEvent) []privateReraceReplayEvent {
			events[2].message = NewId()
			return events
		}},
		{name: "replayed", want: "replayed wire", change: func(events []privateReraceReplayEvent) []privateReraceReplayEvent {
			events[2].resend, events[2].sendCount = true, 2
			return events
		}},
		{name: "unpaid", want: "live source owner", change: func(events []privateReraceReplayEvent) []privateReraceReplayEvent {
			events[0].valid = false
			return events
		}},
	} {
		problems := privateReraceReplayProblems(test.change(append([]privateReraceReplayEvent(nil), valid...)), port)
		if !strings.Contains(fmt.Sprint(problems), test.want) {
			t.Errorf("%s control did not reject its exact boundary: %v", test.name, problems)
		}
	}
}
