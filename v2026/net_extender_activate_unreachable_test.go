package connect

import (
	"encoding/json"
	"testing"
	"time"
)

// A family the operator's required test found no answer from (EXTENDER.md C2a,
// G3). An extender behind a NAT that does not forward its port, or behind a
// firewall that keeps the internet out, is refused alike at every post until
// someone opens the port, and the status says what it needs (the sdk's
// `activation_unreachable`, support inbox 8154). A private family that has not
// activated was already held for the day (2026-11-01); a public one went on
// the backoff, ten minutes doubling to six hours, each post a probe the
// operator ran for a host it could not reach.

// The operator's words for its required test finding no answer, as it sends
// them (server extenderProbeStepRun.refusal).
const testActivateUnreachableRefusal = "the tcp carrier did not answer within the 10 s probe budget: " +
	"dial tcp 198.51.100.7:443: i/o timeout"

// A refusal of another step, which reached the host.
const testActivateForwardRefusal = "the forward failed after 0.1 s: status 502"

// The answer says the required test found no answer in its steps when it sends
// them, and in its words when it predates them; a refusal of any other step,
// or one made after the probe passed, is not that.
func TestExtenderActivateResultRequiredTestUnreachable(t *testing.T) {
	tcpTimeout := &ExtenderActivateProbeStep{Name: ExtenderCarrierTcp, Outcome: "timeout"}
	tcpError := &ExtenderActivateProbeStep{Name: ExtenderCarrierTcp, Outcome: "error"}
	tcpOk := &ExtenderActivateProbeStep{Name: ExtenderCarrierTcp, Outcome: "ok"}
	forwardError := &ExtenderActivateProbeStep{Name: "forward", Outcome: "error"}
	forwardOk := &ExtenderActivateProbeStep{Name: "forward", Outcome: "ok"}
	cases := []struct {
		name     string
		result   *ExtenderActivateResult
		expected bool
	}{
		{
			name:     "the tcp step ran out of time",
			result:   &ExtenderActivateResult{Error: "refused", ProbeSteps: []*ExtenderActivateProbeStep{tcpTimeout}},
			expected: true,
		},
		{
			name:     "the tcp step failed",
			result:   &ExtenderActivateResult{Error: "refused", ProbeSteps: []*ExtenderActivateProbeStep{tcpError}},
			expected: true,
		},
		{
			name: "the forward failed",
			result: &ExtenderActivateResult{
				Error:      testActivateUnreachableRefusal,
				ProbeSteps: []*ExtenderActivateProbeStep{tcpOk, forwardError},
			},
			expected: false,
		},
		{
			name: "the store refused after the probe passed",
			result: &ExtenderActivateResult{
				Error:      "the activation could not be stored",
				ProbeSteps: []*ExtenderActivateProbeStep{tcpOk, forwardOk},
			},
			expected: false,
		},
		{
			name:     "the words of an operator that predates the steps, out of time",
			result:   &ExtenderActivateResult{Error: testActivateUnreachableRefusal},
			expected: true,
		},
		{
			name: "the words of an operator that predates the steps, failed",
			result: &ExtenderActivateResult{
				Error: "the tcp carrier failed after 0.05 s: dial tcp 198.51.100.7:443: connect: no route to host",
			},
			expected: true,
		},
		{
			name:     "the words of another step",
			result:   &ExtenderActivateResult{Error: testActivateForwardRefusal},
			expected: false,
		},
		{
			name:     "a refusal that probed nothing",
			result:   &ExtenderActivateResult{Error: "the operator is busy", RetryAfterSeconds: 60},
			expected: false,
		},
		{
			name:     "an activation",
			result:   &ExtenderActivateResult{Activated: true, ProbeSteps: []*ExtenderActivateProbeStep{tcpOk}},
			expected: false,
		},
		{
			name:     "no answer",
			result:   nil,
			expected: false,
		},
	}
	for _, c := range cases {
		if unreachable := c.result.RequiredTestUnreachable(); unreachable != c.expected {
			t.Fatalf("%s: RequiredTestUnreachable = %t, expected %t", c.name, unreachable, c.expected)
		}
	}

	// the steps decode from the operator's json
	refused := &ExtenderActivateResult{}
	document := `{"activated":false,"error":"refused","probe_steps":[{"name":"tcp","ip_version":4,` +
		`"port":443,"elapsed_ms":10000,"budget_left_ms":10000,"outcome":"timeout"}]}`
	if err := json.Unmarshal([]byte(document), refused); err != nil {
		t.Fatal(err)
	}
	expected := ExtenderActivateProbeStep{
		Name:         ExtenderCarrierTcp,
		IpVersion:    4,
		Port:         443,
		ElapsedMs:    10000,
		BudgetLeftMs: 10000,
		Outcome:      "timeout",
	}
	if len(refused.ProbeSteps) != 1 || *refused.ProbeSteps[0] != expected || !refused.RequiredTestUnreachable() {
		t.Fatalf("refusal = %+v, expected the tcp step that ran out of time", refused)
	}
}

// A public family that has never activated, refused because the operator's
// required test found no answer from it, is posted again at its tick, not on
// the backoff: a firewall that keeps the internet out refuses every retry
// alike. A trigger still posts it at once.
func TestExtenderActivatorHoldsAnUnreachablePublicFamilyForTheDay(t *testing.T) {
	passHolds := newTestActivatePassHolds()
	fixture := newTestActivatorFixtureWithOperator(
		t,
		func(operator *testActivateOperator) {
			operator.setRefusal(testActivateUnreachableRefusal)
		},
		func(settings *ExtenderActivatorSettings) {
			settings.ApiUrlV6 = ""
			settings.IpVersionPublic = func(ipVersion int) bool {
				return true
			}
			passHolds.configure(settings)
		},
	)

	fixture.waitPost()
	if hold := passHolds.next(t, "the refusal"); hold != 24*time.Hour {
		t.Fatalf("the loop holds the unreachable public family %s, expected 24h0m0s", hold)
	}
	status := fixture.activator.Status().Family(4)
	if status.Activated || !status.LastRefused || status.LastError != testActivateUnreachableRefusal {
		t.Fatalf("v4 status = %+v, expected the operator's refusal", status)
	}

	fixture.networkChanged()
	fixture.waitPost()
	if hold := passHolds.next(t, "the trigger's refusal"); hold != 24*time.Hour {
		t.Fatalf("the loop holds the unreachable public family %s after a trigger, expected 24h0m0s", hold)
	}
}

// The steps decide it when the operator sends them: a tcp step that ran out of
// time holds the family for the day whatever the words, and a forward that
// failed holds it on the backoff whatever the words.
func TestExtenderActivatorReadsTheRequiredTestFromTheSteps(t *testing.T) {
	for _, c := range []struct {
		name  string
		steps []*ExtenderActivateProbeStep
		hold  time.Duration
	}{
		{
			name:  "the tcp step ran out of time",
			steps: []*ExtenderActivateProbeStep{{Name: ExtenderCarrierTcp, Outcome: "timeout"}},
			hold:  24 * time.Hour,
		},
		{
			name: "the forward failed",
			steps: []*ExtenderActivateProbeStep{
				{Name: ExtenderCarrierTcp, Outcome: "ok"},
				{Name: "forward", Outcome: "error"},
			},
			hold: 10 * time.Minute,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			passHolds := newTestActivatePassHolds()
			fixture := newTestActivatorFixtureWithOperator(
				t,
				func(operator *testActivateOperator) {
					// words that say the other thing, which the steps outrank
					if c.hold == 24*time.Hour {
						operator.setRefusal(testActivateForwardRefusal)
					} else {
						operator.setRefusal(testActivateUnreachableRefusal)
					}
					operator.setProbeSteps(c.steps)
				},
				func(settings *ExtenderActivatorSettings) {
					settings.ApiUrlV6 = ""
					passHolds.configure(settings)
				},
			)
			fixture.waitPost()
			if hold := passHolds.next(t, c.name); hold != c.hold {
				t.Fatalf("the loop holds the family %s, expected %s", hold, c.hold)
			}
		})
	}
}

// A refusal of a public family by a step that reached it says nothing of
// whether the internet reaches the host, so it is held on the backoff as
// before.
func TestExtenderActivatorHoldsAPublicFamilysOtherRefusalsOnTheBackoff(t *testing.T) {
	passHolds := newTestActivatePassHolds()
	fixture := newTestActivatorFixtureWithOperator(
		t,
		func(operator *testActivateOperator) {
			operator.setRefusal(testActivateForwardRefusal)
		},
		func(settings *ExtenderActivatorSettings) {
			settings.ApiUrlV6 = ""
			settings.IpVersionPublic = func(ipVersion int) bool {
				return true
			}
			passHolds.configure(settings)
		},
	)
	fixture.waitPost()
	if hold := passHolds.next(t, "the forward's refusal"); hold != 10*time.Minute {
		t.Fatalf("the loop holds the public family %s after the forward's refusal, expected 10m0s", hold)
	}
}

// A public family that has activated is reachable, so the required test finding
// no answer later -- a host that slept, a path that dropped for a moment -- is
// held on the backoff as any family's refusal is: posted again ten minutes
// later, and activated.
func TestExtenderActivatorRetriesAnUnreachableFamilyThatActivated(t *testing.T) {
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.ApiUrlV6 = ""
		settings.IpVersionPublic = func(ipVersion int) bool { return true }
	})
	fixture.waitPass()
	fixture.waitPost()
	fixture.waitActivation()

	fixture.step(5 * time.Minute)
	refusalTime := fixture.clock.Now()
	fixture.operator.setRefusal(testActivateUnreachableRefusal)
	fixture.networkChanged()
	if post := fixture.waitPost(); post.postTime != refusalTime {
		t.Fatalf("forced post at %s, expected %s", post.postTime, refusalTime)
	}
	fixture.operator.setRefusal("")

	fixture.step(9 * time.Minute)
	fixture.expectFamilyPostCounts("one minute short of the hold", 2, 0)
	fixture.clock.advance(time.Minute)
	if post := fixture.wakeForPost(); post.postTime != refusalTime.Add(10*time.Minute) {
		t.Fatalf("post at %s after the refusal, expected 10m", post.postTime.Sub(refusalTime))
	}
	if ipVersion := fixture.waitActivation(); ipVersion != 4 {
		t.Fatalf("activated v%d, expected v4", ipVersion)
	}
}
