package connect

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A family-agnostic transport backs off exponentially after consecutive
// failed dials instead of retrying at the shared staircase's ~1s pace for the
// whole of a platform outage (https://github.com/urnetwork/connect/issues/175).
// noteDialFailure also feeds the package-level degraded state, so these reset
// it and must not run in parallel.

func newDialBackoffTestTransport(base time.Duration, max time.Duration) *PlatformTransport {
	return &PlatformTransport{
		log:           NewNoopLogger(),
		pinnedBackoff: newPinnedDialBackoff(base, max),
		failBackoff:   newDialFailureBackoff(base, max),
	}
}

func requireDelayRange(t *testing.T, backoff *pinnedDialBackoff, low time.Duration, high time.Duration) {
	t.Helper()
	for i := 0; i < 64; i += 1 {
		d := backoff.delay()
		if d < low || high <= d {
			t.Fatalf("delay = %s, want in [%s, %s)", d, low, high)
		}
	}
}

// The first failure is free; from the second, the wait doubles from the base
// to the cap, jittered in [d/2, d).
func TestDialFailureBackoffSchedule(t *testing.T) {
	const base = 5 * time.Second
	const max = 60 * time.Second
	backoff := newDialFailureBackoff(base, max)

	if d := backoff.delay(); d != 0 {
		t.Fatalf("delay before any failure = %s, want 0", d)
	}
	backoff.fail()
	if d := backoff.delay(); d != 0 {
		t.Fatalf("delay after the first failure = %s, want 0 (the ordinary reconnect timing)", d)
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second, 60 * time.Second}
	for _, d := range want {
		backoff.fail()
		requireDelayRange(t, backoff, d/2, d)
	}
	for i := 0; i < 100; i += 1 {
		backoff.fail()
	}
	requireDelayRange(t, backoff, max/2, max)

	backoff.reset()
	if d := backoff.delay(); d != 0 {
		t.Fatalf("delay after reset = %s, want 0", d)
	}
}

// The pinned backoff keeps its original schedule: no grace.
func TestPinnedDialBackoffScheduleUnchanged(t *testing.T) {
	backoff := newPinnedDialBackoff(time.Second, 4*time.Second)
	backoff.fail()
	requireDelayRange(t, backoff, 500*time.Millisecond, time.Second)
	backoff.fail()
	requireDelayRange(t, backoff, time.Second, 2*time.Second)
}

func TestDialRetryAfterBacksOffOnConsecutiveFailures(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	transport := newDialBackoffTestTransport(time.Hour, 4*time.Hour)
	// an expired reconnect timer: the old loop retried at once
	expired := NewReconnect(0)

	fired := func() bool {
		select {
		case <-transport.dialRetryAfter(expired):
			return true
		case <-time.After(50 * time.Millisecond):
			return false
		}
	}

	if !fired() {
		t.Fatal("with no failures the retry must keep the reconnect timing")
	}
	transport.noteDialFailure()
	if !fired() {
		t.Fatal("the first failure must keep the reconnect timing")
	}
	transport.noteDialFailure()
	if fired() {
		t.Fatal("a second consecutive failure retried on the expired reconnect timer instead of backing off")
	}

	// a successful dial resets the backoff
	transport.noteDialSuccess()
	if !fired() {
		t.Fatal("a successful dial did not reset the backoff")
	}

	// a network change resets the backoff
	transport.noteDialFailure()
	transport.noteDialFailure()
	if fired() {
		t.Fatal("precondition: backing off")
	}
	transport.noteKick()
	if !fired() {
		t.Fatal("a network change did not reset the backoff")
	}
}

// A pinned transport paces itself before the dial (nextDialTime); its failure
// path keeps the reconnect timer and its failures do not touch failBackoff.
func TestDialRetryAfterPinnedKeepsReconnectTimer(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	transport := newDialBackoffTestTransport(time.Hour, 4*time.Hour)
	transport.ipFamily = 4
	transport.noteDialFailure()
	transport.noteDialFailure()
	transport.noteDialFailure()
	select {
	case <-transport.dialRetryAfter(NewReconnect(0)):
	case <-time.After(time.Second):
		t.Fatal("pinned transport did not keep the reconnect timer")
	}
	if d := transport.failBackoff.delay(); d != 0 {
		t.Fatalf("pinned failures fed the family-agnostic backoff: delay = %s", d)
	}
}

// The constructor wires the backoff with the settings cap, and a non-positive
// cap resolves to one minute.
func TestPlatformTransportFailBackoffSettings(t *testing.T) {
	AssertEqual(t, DefaultPlatformTransportSettings().ReconnectMaxTimeout, 60*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, c := range []struct {
		max  time.Duration
		want time.Duration
	}{
		{0, 60 * time.Second},
		{-time.Second, 60 * time.Second},
		{7 * time.Second, 7 * time.Second},
	} {
		settings := DefaultPlatformTransportSettings()
		settings.ReconnectMaxTimeout = c.max
		settings.StartDisabled = true
		transport := NewPlatformTransportWithTargetMode(
			ctx,
			NewClientStrategyWithDefaults(ctx),
			NewRouteManager(ctx, "test"),
			"wss://127.0.0.1:1",
			&ClientAuth{ByJwt: "testing", InstanceId: NewId(), AppVersion: "testing"},
			TransportModeH1,
			settings,
		)
		if transport.failBackoff == nil {
			t.Fatal("transport has no failure backoff")
		}
		AssertEqual(t, transport.failBackoff.base, settings.ReconnectTimeout)
		AssertEqual(t, transport.failBackoff.max, c.want)
		AssertEqual(t, transport.failBackoff.grace, 1)
		transport.Close()
	}
}

// Both mode runners must wait on dialRetryAfter after a failed dial.
func TestDialRetryAfterSourceAnchor(t *testing.T) {
	source, err := readSource("transport.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, signature := range []string{
		"func (self *PlatformTransport) runH1(",
		"func (self *PlatformTransport) runH3(",
	} {
		body, ok := functionBody(source, signature)
		if !ok {
			t.Fatalf("could not find %s", signature)
		}
		if !strings.Contains(body, "case <-self.dialRetryAfter(reconnect):") {
			t.Errorf("%s does not back off after a failed dial", signature)
		}
	}
}
