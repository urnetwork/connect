package connect

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// What the activator logs of the operator's probe (EXTENDER.md C2a, C2b,
// connect#230). The answer carries every probe step with its timing, and on an
// activation the optional carriers the probe left out, so a host that answered
// late reads differently from one that never answered. Decoded but not logged,
// a refusal read the same either way, and an activation that left quic or dns
// out of the published record said nothing at all.

// The operator's wire form of an activation that left two carriers out
// (server controller.ExtenderActivateResult).
const testActivateLeftOutDocument = `{"activated":true,"carriers":["tcp","dns"],"refused_carriers":[` +
	`{"carrier":"quic","port":443,"error":"no answer within the 6.4 s left of the 10 s probe budget",` +
	`"elapsed_ms":6400,"budget_left_ms":6400},` +
	`{"carrier":"dns","port":53,"error":"connection refused","elapsed_ms":12,"budget_left_ms":6400}]}`

// The left out carriers decode from the operator's json, in the order offered.
func TestExtenderActivateResultDecodesRefusedCarriers(t *testing.T) {
	result := &ExtenderActivateResult{}
	if err := json.Unmarshal([]byte(testActivateLeftOutDocument), result); err != nil {
		t.Fatal(err)
	}
	expected := []ExtenderActivateCarrierRefusal{
		{
			Carrier:      ExtenderCarrierQuic,
			Port:         443,
			Error:        "no answer within the 6.4 s left of the 10 s probe budget",
			ElapsedMs:    6400,
			BudgetLeftMs: 6400,
		},
		{
			Carrier:      ExtenderCarrierDns,
			Port:         53,
			Error:        "connection refused",
			ElapsedMs:    12,
			BudgetLeftMs: 6400,
		},
	}
	if len(result.RefusedCarriers) != len(expected) {
		t.Fatalf("refused carriers = %+v, expected %+v", result.RefusedCarriers, expected)
	}
	for i, refusedCarrier := range result.RefusedCarriers {
		if *refusedCarrier != expected[i] {
			t.Fatalf("refused carrier %d = %+v, expected %+v", i, *refusedCarrier, expected[i])
		}
	}
}

// The one line texts skip a nil entry and keep the operator's order.
func TestExtenderActivateProbeTexts(t *testing.T) {
	stepsText := extenderActivateProbeStepsText([]*ExtenderActivateProbeStep{
		{Name: ExtenderCarrierTcp, Port: 443, ElapsedMs: 9500, BudgetLeftMs: 10000, Outcome: "ok"},
		nil,
		{Name: "forward", Port: 443, ElapsedMs: 500, BudgetLeftMs: 500, Outcome: "timeout"},
	})
	if expected := "tcp:443 ok after 9.5s (10s left), forward:443 timeout after 500ms (500ms left)"; stepsText != expected {
		t.Fatalf("steps = %q, expected %q", stepsText, expected)
	}
	refusalsText := extenderActivateCarrierRefusalsText([]*ExtenderActivateCarrierRefusal{
		nil,
		{Carrier: ExtenderCarrierQuic, Port: 443, Error: "no answer", ElapsedMs: 3000, BudgetLeftMs: 6400},
		{Carrier: ExtenderCarrierDns, Port: 4053, Error: "refused", ElapsedMs: 12, BudgetLeftMs: 6400},
	})
	if expected := "quic:443 after 3s (6.4s left): no answer; dns:4053 after 12ms (6.4s left): refused"; refusalsText != expected {
		t.Fatalf("refusals = %q, expected %q", refusalsText, expected)
	}
}

// A refusal logs the steps the operator ran after its words, and a refusal
// from an operator that predates the steps logs its words alone.
func TestExtenderActivatorLogsTheProbeStepsOfARefusal(t *testing.T) {
	for _, c := range []struct {
		name     string
		steps    []*ExtenderActivateProbeStep
		expected []string
	}{
		{
			name: "with steps",
			steps: []*ExtenderActivateProbeStep{
				{Name: ExtenderCarrierTcp, IpVersion: 4, Port: 443, ElapsedMs: 9500, BudgetLeftMs: 10000, Outcome: "ok"},
				{Name: "forward", IpVersion: 4, Port: 443, ElapsedMs: 500, BudgetLeftMs: 500, Outcome: "timeout"},
			},
			expected: []string{
				"[extender]activate v4 probe = tcp:443 ok after 9.5s (10s left), forward:443 timeout after 500ms (500ms left)\n",
			},
		},
		{
			name:     "without steps",
			steps:    nil,
			expected: []string{},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			log := &testActivateLogger{}
			fixture := newTestActivatorFixtureWithOperator(
				t,
				func(operator *testActivateOperator) {
					operator.setRefusal(testActivateForwardRefusal)
					operator.setProbeSteps(c.steps)
				},
				func(settings *ExtenderActivatorSettings) {
					settings.ApiUrlV6 = ""
					settings.Log = log
				},
			)
			fixture.waitOutcome(0)
			if refused := log.linesWithPrefix("[extender]activate v4 refused = "); len(refused) != 1 {
				t.Fatalf("refused lines = %q, expected the operator's words once", refused)
			}
			probeLines := log.linesWithPrefix("[extender]activate v4 probe = ")
			if strings.Join(probeLines, "") != strings.Join(c.expected, "") {
				t.Fatalf("probe lines = %q, expected %q", probeLines, c.expected)
			}
		})
	}
}

// An activation logs the optional carriers it left out, and one that left
// none out logs no such line.
func TestExtenderActivatorLogsTheCarriersAnActivationLeftOut(t *testing.T) {
	for _, c := range []struct {
		name            string
		refusedCarriers []*ExtenderActivateCarrierRefusal
		expected        []string
	}{
		{
			name: "left out",
			refusedCarriers: []*ExtenderActivateCarrierRefusal{
				{Carrier: ExtenderCarrierQuic, Port: 443, Error: "no answer", ElapsedMs: 6400, BudgetLeftMs: 6400},
			},
			expected: []string{
				"[extender]activate v4 left out = quic:443 after 6.4s (6.4s left): no answer\n",
			},
		},
		{
			name:            "none left out",
			refusedCarriers: nil,
			expected:        []string{},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			log := &testActivateLogger{}
			fixture := newTestActivatorFixtureWithOperator(
				t,
				func(operator *testActivateOperator) {
					operator.setRefusedCarriers(c.refusedCarriers)
				},
				func(settings *ExtenderActivatorSettings) {
					settings.ApiUrlV6 = ""
					settings.Log = log
				},
			)
			// the line is written before the activation is reported
			if ipVersion := fixture.waitActivation(); ipVersion != 4 {
				t.Fatalf("activated v%d, expected v4", ipVersion)
			}
			leftOutLines := log.linesWithPrefix("[extender]activate v4 left out = ")
			if strings.Join(leftOutLines, "") != strings.Join(c.expected, "") {
				t.Fatalf("left out lines = %q, expected %q", leftOutLines, c.expected)
			}
		})
	}
}

// Raw operator responses reach the real activation loop, so a fixture's Go
// encoder cannot manufacture the timings an older operator never sent. The
// activation callback is the barrier after the log and remains a success for
// every timing shape; neither the operator's error nor accepted carriers change.
func TestExtenderActivatorLogsCarrierTimingPresenceFromJson(t *testing.T) {
	cases := []struct {
		name       string
		timingJson string
		timingText string
	}{
		{name: "legacy"},
		{name: "measured zeros", timingJson: `,"elapsed_ms":0,"budget_left_ms":0`, timingText: " after 0s (0s left)"},
		{name: "elapsed only", timingJson: `,"elapsed_ms":12`, timingText: " after 12ms"},
		{name: "budget only", timingJson: `,"budget_left_ms":6400`, timingText: " (6.4s left)"},
		{name: "null timings", timingJson: `,"elapsed_ms":null,"budget_left_ms":null`},
		{name: "null elapsed", timingJson: `,"elapsed_ms":null,"budget_left_ms":0`, timingText: " (0s left)"},
		{name: "null budget", timingJson: `,"elapsed_ms":0,"budget_left_ms":null`, timingText: " after 0s"},
		{name: "full timing", timingJson: `,"elapsed_ms":12,"budget_left_ms":6400`, timingText: " after 12ms (6.4s left)"},
	}
	const carrierFields = `"carrier":"quic","port":443,"error":"context deadline exceeded"`
	// A defined type uses the original plain-struct decoder. The same legacy
	// object used below then reproduces the invented zero timing deterministically.
	type oldCarrierRefusal ExtenderActivateCarrierRefusal
	var oldRefusal oldCarrierRefusal
	if err := json.Unmarshal([]byte(`{`+carrierFields+`}`), &oldRefusal); err != nil {
		t.Fatal(err)
	}
	oldText := extenderActivateCarrierRefusalsText([]*ExtenderActivateCarrierRefusal{(*ExtenderActivateCarrierRefusal)(&oldRefusal)})
	if expected := "quic:443 after 0s (0s left): context deadline exceeded"; oldText != expected {
		t.Fatalf("old decoder = %q, expected the pre-fix false timing %q", oldText, expected)
	}

	answers := make(chan string, 1)
	operator := newFamilyHttptestServer(t, 4, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/hello" {
			json.NewEncoder(w).Encode(map[string]string{"client_address": testActivateIpv4 + ":41001"})
			return
		}
		if r.URL.Path != ExtenderActivatePath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		select {
		case answer := <-answers:
			json.NewEncoder(w).Encode(json.RawMessage(answer))
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(operator.Close)
	log := &testActivateLogger{}
	activatedResults := make(chan *ExtenderActivateResult, 1)
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.ApiUrlV4 = operator.URL
		settings.ApiUrlV6 = ""
		settings.Log = log
		onActivated := settings.OnActivated
		settings.OnActivated = func(ipVersion int, result *ExtenderActivateResult) {
			activatedResults <- result
			onActivated(ipVersion, result)
		}
	})
	expectedLines := []string{}
	for i, c := range cases {
		answers <- `{"activated":true,"ip":"` + testActivateIpv4 + `","ip_version":4,"carriers":["tcp"],` +
			`"refused_carriers":[{` + carrierFields + c.timingJson + `}]}`
		if 0 < i {
			fixture.networkChanged()
		}
		if ipVersion := fixture.waitActivation(); ipVersion != 4 {
			t.Fatalf("%s: activated v%d, expected v4", c.name, ipVersion)
		}
		result := <-activatedResults
		if !result.Activated || result.Error != "" || strings.Join(result.Carriers, ",") != ExtenderCarrierTcp ||
			len(result.RefusedCarriers) != 1 || result.RefusedCarriers[0].Error != "context deadline exceeded" {
			t.Fatalf("%s: activation callback changed the operator's answer: %+v", c.name, result)
		}
		if family := fixture.activator.Status().Family(4); family == nil || !family.Activated || family.LastRefused || family.LastError != "" {
			t.Fatalf("%s: activation status = %+v, expected a success", c.name, family)
		}
		expectedLines = append(expectedLines, "[extender]activate v4 left out = quic:443"+c.timingText+": context deadline exceeded\n")
		lines := log.linesWithPrefix("[extender]activate v4 left out = ")
		if strings.Join(lines, "") != strings.Join(expectedLines, "") {
			t.Fatalf("%s: left out lines = %q, expected %q", c.name, lines, expectedLines)
		}
	}
}

// A decoder can reuse a refusal directly or a result slice's element. Missing
// and null timings clear prior measurements, and present zeros clear prior
// absence; a null object still leaves an existing value alone.
func TestExtenderActivateCarrierRefusalReusedDecodeReplacesTiming(t *testing.T) {
	refusal := &ExtenderActivateCarrierRefusal{Carrier: ExtenderCarrierQuic, Port: 443, Error: "no answer"}
	for _, c := range []struct {
		document     string
		elapsedMs    int64
		budgetLeftMs int64
		timingText   string
	}{
		{document: `{"elapsed_ms":12,"budget_left_ms":6400}`, elapsedMs: 12, budgetLeftMs: 6400, timingText: " after 12ms (6.4s left)"},
		{document: `{}`},
		{document: `{"elapsed_ms":0}`, timingText: " after 0s"},
		{document: `{"budget_left_ms":0}`, timingText: " (0s left)"},
		{document: `{"elapsed_ms":12,"budget_left_ms":6400}`, elapsedMs: 12, budgetLeftMs: 6400, timingText: " after 12ms (6.4s left)"},
		{document: `{"elapsed_ms":null,"budget_left_ms":null}`},
		{document: `{"elapsed_ms":0,"budget_left_ms":0}`, timingText: " after 0s (0s left)"},
		{document: `null`, timingText: " after 0s (0s left)"},
	} {
		if err := json.Unmarshal([]byte(c.document), refusal); err != nil {
			t.Fatal(err)
		}
		if refusal.ElapsedMs != c.elapsedMs || refusal.BudgetLeftMs != c.budgetLeftMs {
			t.Fatalf("%s: timings = %d, %d, expected %d, %d", c.document, refusal.ElapsedMs, refusal.BudgetLeftMs, c.elapsedMs, c.budgetLeftMs)
		}
		if text, expected := extenderActivateCarrierRefusalsText([]*ExtenderActivateCarrierRefusal{refusal}), "quic:443"+c.timingText+": no answer"; text != expected {
			t.Fatalf("%s: refusal = %q, expected %q", c.document, text, expected)
		}
	}

	result := &ExtenderActivateResult{}
	if err := json.Unmarshal([]byte(`{"refused_carriers":[{"carrier":"quic","port":443,"error":"no answer","elapsed_ms":12,"budget_left_ms":6400}]}`), result); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"refused_carriers":[{"carrier":"quic","port":443,"error":"no answer"}]}`), result); err != nil {
		t.Fatal(err)
	}
	if text, expected := extenderActivateCarrierRefusalsText(result.RefusedCarriers), "quic:443: no answer"; text != expected {
		t.Fatalf("reused result = %q, expected %q", text, expected)
	}
}

// Public Go literals keep reporting both int64 values, including zero, and
// encoding a refusal retains the existing five-field json schema. Presence is
// only decode metadata, never a new wire field or a custom encoder contract.
func TestExtenderActivateCarrierRefusalKeepsGoValuesAndJson(t *testing.T) {
	refusal := &ExtenderActivateCarrierRefusal{Carrier: ExtenderCarrierQuic, Port: 443, Error: "no answer"}
	if text, expected := extenderActivateCarrierRefusalsText([]*ExtenderActivateCarrierRefusal{refusal}), "quic:443 after 0s (0s left): no answer"; text != expected {
		t.Fatalf("constructed zero timings = %q, expected %q", text, expected)
	}
	const expectedJson = `{"carrier":"quic","port":443,"error":"no answer","elapsed_ms":0,"budget_left_ms":0}`
	for _, decodeLegacy := range []bool{false, true} {
		if decodeLegacy {
			if err := json.Unmarshal([]byte(`{"carrier":"quic","port":443,"error":"no answer"}`), refusal); err != nil {
				t.Fatal(err)
			}
		}
		encoded, err := json.Marshal(refusal)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != expectedJson {
			t.Fatalf("decoded legacy %t: json = %s, expected %s", decodeLegacy, encoded, expectedJson)
		}
	}
}
