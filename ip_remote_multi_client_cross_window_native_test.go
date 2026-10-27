// Full strict-window native ingress reproduces the public SDK's blocked fresh
// navigation without requesting another discovery slot or changing old flows.
package connect

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Native ingress takes the pooled packet and keeps an exact admission result.
// No provider or source retransmission is added by this helper.
func privateCrossWindowNativeSyn(harness *reraceHarness, path *IpPath) bool {
	packet := MessagePoolCopy(reraceTcpPacket(path, tcpFlagSyn, path.SequenceNumber, 0, nil))
	accepted := [1]bool{}
	count := harness.multiClient.SendNativePacketBatchWithResults(harness.source, protocol.ProvideMode_Network, [][]byte{packet}, reraceSendTimeout, accepted[:])
	harness.settle()
	return count == 1 && accepted[0]
}

// Five healthy quality owners plus one speed owner occupy both exact caps.
// Quality's actual same-service refusals cannot justify exceeding either cap.
// The next unchanged3s source SYN must try the already-owned healthy speed exit.
func TestPrivateCrossWindowNativeFullStrictSixUsesOwnedSpeed(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var specs []privateCrossWindowExitSpec
		qualityNames := []string{"quality-0", "quality-1", "quality-2", "quality-3", "quality-4"}
		for _, name := range qualityNames {
			specs = append(specs, privateCrossWindowExitSpec{exitSpec: reraceExitSpec{name: name, answer: reraceAlways(reraceSignal)}, windowType: WindowTypeQuality})
		}
		specs = append(specs, privateCrossWindowExitSpec{exitSpec: reraceExitSpec{name: "speed"}, windowType: WindowTypeSpeed})
		harness := newPrivateCrossWindowSourceRetryHarness(t, specs, nil)
		if harness.multiClient.reliabilitySettings().MaxFlowsPerExit != 16 {
			t.Fatal("native full-window control lost the SDK-like16-flow policy")
		}
		owners := map[Id]*multiClientChannel{}
		for _, windowType := range []WindowType{WindowTypeQuality, WindowTypeSpeed} {
			window := harness.multiClient.windows[windowType]
			want := 5
			if windowType == WindowTypeSpeed {
				want = 1
			}
			clients := window.unorderedClients()
			windowSize := window.settings.WindowSizes[windowType]
			if !window.settings.StrictWindowSizeHardMax || len(clients) != want || windowSize.WindowSizeHardMax != want || window.strictWindowAdmissionAllowed(NewId(), windowSize) {
				t.Fatalf("window=%v did not retain its exact full strict cap: owned=%d hard=%d", windowType, len(clients), windowSize.WindowSizeHardMax)
			}
			for _, client := range clients {
				if client.IsDone() || client.isWarning() || client.isQuarantined() {
					t.Fatal("full native control started with an unavailable owner")
				}
				owners[client.ClientId()] = client
			}
		}
		if len(owners) != 6 || len(harness.generator.clientList()) != 6 {
			t.Fatal("full native control did not start with six distinct actual owners")
		}
		path := privateCrossWindowFlow(4, 1, 44900)
		start := time.Now()
		if !privateCrossWindowNativeSyn(harness, path) {
			t.Fatal("first native source SYN was not admitted to healthy quality")
		}
		// These are the natural source retransmission and response deadlines,
		// not a production timer change or a fixture-triggered generator wake.
		if remaining := start.Add(3 * time.Second).Sub(time.Now()); remaining > 0 {
			time.Sleep(remaining)
		}
		harness.settle()
		privateCrossWindowAssertFailed(t, harness, path, qualityNames...)
		if len(harness.exit("speed").syns(path)) != 0 || harness.boundTo(path) != nil || len(harness.delivered(path)) != 0 {
			t.Fatal("quality refusals were bypassed before the actual source retransmit")
		}
		for _, name := range qualityNames {
			if len(harness.exit(name).syns(path)) != 1 {
				t.Fatalf("quality owner %s did not receive its one actual refusal", name)
			}
		}
		t.Log("native full-window premises: owned_quality5 owned_speed1 hard_quality5 hard_speed1 healthy6 actual_refusals5 speed_syns0 no_app_reply; no extra ownership slot")
		accepted := privateCrossWindowNativeSyn(harness, path)
		answered := harness.waitForSynAck(path, start.Add(5*time.Second))
		if !accepted || !answered || harness.boundTo(path) != privateCrossWindowChannel(t, harness, "speed") {
			t.Fatalf("SDK_CROSS_WINDOW_NATIVE_FULL_CAP_FAILED_FALLBACK: accepted=%t response=%t speed_syns=%d", accepted, answered, len(harness.exit("speed").syns(path)))
		}
		if len(harness.exit("speed").syns(path)) != 1 || len(harness.generator.clientList()) != 6 {
			t.Fatal("native recovery created a new owner or retried the speed exit")
		}
		for _, name := range qualityNames {
			if len(harness.exit(name).syns(path)) != 1 {
				t.Fatalf("known-failed quality %s was retried before healthy speed", name)
			}
		}
		for _, windowType := range []WindowType{WindowTypeQuality, WindowTypeSpeed} {
			window := harness.multiClient.windows[windowType]
			windowSize := window.settings.WindowSizes[windowType]
			if window.strictWindowAdmissionAllowed(NewId(), windowSize) {
				t.Fatal("selection recovery silently opened an owned admission slot")
			}
			for _, client := range window.unorderedClients() {
				if owners[client.ClientId()] != client || client.IsDone() {
					t.Fatal("selection recovery replaced or retired an existing owner")
				}
			}
		}
		reraceAssertNoErrorDelivered(t, harness, path)
	})
}
