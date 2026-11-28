package connect

import (
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"
)

// The operator's refusal of a key another network holds (EXTENDER.md C2c), as
// the activation loop reads it. A key belongs to the network whose activation
// first stored it, so a device signed in to another account, or a state
// directory copied to a host under another account, posts a key the operator
// refuses at every post, whatever the post offers and however long it waits.

// The refusal as the operator first sent it, which predates `refusal`.
const testActivateKeyOtherNetworkRefusal = "This extender key is registered to another network, " +
	"so it cannot be activated for this one. Generate a new extender key, or ask support to " +
	"move the key to this network."

// The answer says the key is another network's in the code when it carries
// one, and in the fixed text of an operator that predates the code; a code
// that names anything else wins over the text, and nothing else is read as
// the refusal.
func TestExtenderActivateResultKeyHeldByAnotherNetwork(t *testing.T) {
	cases := []struct {
		name     string
		result   *ExtenderActivateResult
		expected bool
	}{
		{
			name:     "the code",
			result:   &ExtenderActivateResult{Refusal: ExtenderActivateRefusalKeyOtherNetwork, Error: "refused"},
			expected: true,
		},
		{
			name:     "the text of an operator that predates the code",
			result:   &ExtenderActivateResult{Error: testActivateKeyOtherNetworkRefusal},
			expected: true,
		},
		{
			name: "a later wording of the text",
			result: &ExtenderActivateResult{Error: "This extender key is registered to another network, " +
				"so it cannot be activated for this one. An app makes a new one."},
			expected: true,
		},
		{
			name:     "another code over the text",
			result:   &ExtenderActivateResult{Refusal: "another_refusal", Error: testActivateKeyOtherNetworkRefusal},
			expected: false,
		},
		{
			name:     "a probe's refusal",
			result:   &ExtenderActivateResult{Error: "the tcp carrier did not answer"},
			expected: false,
		},
		{
			name:     "the text inside another refusal",
			result:   &ExtenderActivateResult{Error: "the forward failed: " + testActivateKeyOtherNetworkRefusal},
			expected: false,
		},
		{
			name:     "an activation",
			result:   &ExtenderActivateResult{Activated: true, Refusal: ExtenderActivateRefusalKeyOtherNetwork},
			expected: false,
		},
		{
			name:     "no answer",
			result:   nil,
			expected: false,
		},
	}
	for _, c := range cases {
		if held := c.result.KeyHeldByAnotherNetwork(); held != c.expected {
			t.Fatalf("%s: KeyHeldByAnotherNetwork = %t, expected %t", c.name, held, c.expected)
		}
	}

	// the code decodes from the operator's json
	refused := &ExtenderActivateResult{}
	document := `{"activated":false,"error":"refused","refusal":"key_other_network"}`
	if err := json.Unmarshal([]byte(document), refused); err != nil {
		t.Fatal(err)
	}
	if refused.Refusal != ExtenderActivateRefusalKeyOtherNetwork || !refused.KeyHeldByAnotherNetwork() {
		t.Fatalf("refusal = %+v, expected the key refused as another network's", refused)
	}
}

// No wait changes the operator's refusal of a key another network holds, so
// the family is posted again at its tick, not on the backoff: the loop used to
// hold it ten minutes, doubling to six hours, and spend each post on the same
// refusal. A public family, whose other refusals the backoff holds.
func TestExtenderActivatorHoldsAKeyAnotherNetworkHoldsForTheDay(t *testing.T) {
	passHolds := newTestActivatePassHolds()
	fixture := newTestActivatorFixtureWithOperator(
		t,
		func(operator *testActivateOperator) {
			operator.setRefusal(testActivateKeyOtherNetworkRefusal)
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
	if hold := passHolds.next(t, "the first refusal"); hold != 24*time.Hour {
		t.Fatalf("the loop holds the family %s after the key's refusal, expected 24h0m0s", hold)
	}
	status := fixture.activator.Status()
	if family := status.Family(4); family.Activated || !family.LastRefused ||
		family.LastError != testActivateKeyOtherNetworkRefusal {
		t.Fatalf("v4 status = %+v, expected the key's refusal", family)
	}

	// a trigger still posts at once, and its refusal holds the family for the
	// day again rather than starting the backoff
	fixture.networkChanged()
	fixture.waitPost()
	if hold := passHolds.next(t, "the refusal of the trigger's post"); hold != 24*time.Hour {
		t.Fatalf("the loop holds the family %s after the trigger's refusal, expected 24h0m0s", hold)
	}
}

// The operator's code holds the family for the day as its text does, and the
// family it accepts is not held with it.
func TestExtenderActivatorHoldsAKeyRefusedByCodeForTheDay(t *testing.T) {
	passHolds := newTestActivatePassHolds()
	fixture := newTestActivatorFixtureWithOperator(
		t,
		func(operator *testActivateOperator) {
			operator.setFamilyRefusal(4, "refused")
			operator.setRefusalCode(ExtenderActivateRefusalKeyOtherNetwork)
		},
		func(settings *ExtenderActivatorSettings) {
			settings.ApiUrlV6 = ""
			passHolds.configure(settings)
		},
	)

	fixture.waitPost()
	if hold := passHolds.next(t, "the refusal"); hold != 24*time.Hour {
		t.Fatalf("the loop holds the family %s after the coded refusal, expected 24h0m0s", hold)
	}
}

// Each refusal reaches the role with its answer, once the status records it,
// and an activation does not: the role reads from it whether the key wants
// replacing (C2c).
func TestExtenderActivatorTellsTheRoleOfEachRefusal(t *testing.T) {
	type refusal struct {
		ipVersion int
		result    *ExtenderActivateResult
		status    *ExtenderFamilyActivationStatus
	}
	refusals := make(chan refusal, 8)
	// the callback reads the status through the activator, which exists once
	// the fixture returns; the operator answers nothing before then
	var activator atomic.Pointer[ExtenderActivator]
	answerHold := make(chan struct{})
	fixture := newTestActivatorFixtureWithOperator(
		t,
		func(operator *testActivateOperator) {
			operator.setFamilyRefusal(4, testActivateKeyOtherNetworkRefusal)
			operator.setRefusalCode(ExtenderActivateRefusalKeyOtherNetwork)
			operator.setAnswerHold(answerHold)
		},
		func(settings *ExtenderActivatorSettings) {
			settings.OnRefused = func(ipVersion int, result *ExtenderActivateResult) {
				refusals <- refusal{
					ipVersion: ipVersion,
					result:    result,
					status:    activator.Load().Status().Family(ipVersion),
				}
			}
		},
	)
	activator.Store(fixture.activator)
	close(answerHold)

	fixture.waitFirstPass()
	if ipVersion := fixture.waitActivation(); ipVersion != 6 {
		t.Fatalf("activated v%d, expected v6", ipVersion)
	}
	var told refusal
	select {
	case told = <-refusals:
	case <-time.After(30 * time.Second):
		t.Fatal("the role was not told of the refusal")
	}
	if told.ipVersion != 4 || !told.result.KeyHeldByAnotherNetwork() {
		t.Fatalf("the role was told of v%d %+v, expected v4's key refusal", told.ipVersion, told.result)
	}
	if !told.status.LastRefused || told.status.LastError != testActivateKeyOtherNetworkRefusal {
		t.Fatalf("the status when the role was told = %+v, expected the refusal recorded", told.status)
	}
	select {
	case extra := <-refusals:
		t.Fatalf("the role was told of v%d %+v, expected only v4's refusal", extra.ipVersion, extra.result)
	default:
	}
}
