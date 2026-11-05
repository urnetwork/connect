package connect

import (
	"encoding/json"
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
