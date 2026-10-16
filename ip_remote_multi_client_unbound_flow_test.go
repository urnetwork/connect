package connect

// A flow can be left with no client and no race while its exit is working:
// the silence inference unbinds a connect attempt whose answer is only slow,
// and the re-race of that retransmit can be refused. What the exit then sends
// for the flow is the first proof that it works, so its sender adopts the
// flow, rather than the packet being dropped ("[multi]receive no race and no
// client") until the app happens to send again.
//
// Every other way a flow loses its client is bounded here too. A provider's
// explicit dial-failure signal keeps the drop and waits for the app's
// retransmit. A send error and an exit removal reset the app and leave the
// flow dead, so a late answer is still dropped. A generation an app reset
// opened has no connect attempt and adopts nothing. The idle reaper and the
// quarantine hand-off detach the flow from the path map before they clear
// its client (TestDetachIdleFlowsCleansRoutingAndPreservesNextDeadline,
// ip_remote_multi_client_h1_blackhole_test.go), so they leave no flow
// without a client to adopt.

import (
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A fresh app tcp flow from a documentation source address to the first test
// destination.
func unboundFlowTcpPath(sourcePort int) *IpPath {
	path := reraceFlow(4, 0, sourcePort)
	path.SourceIp = net.IPv4(192, 0, 2, 2).To4()
	return path
}

// Opens a flow on the only exit, which answers nothing, and lets the silence
// inference unbind it at the app's 3s retransmit while that retransmit's
// re-race is refused admission, so the flow is left with no client and no
// race.
func unboundBySilenceInference(t *testing.T, harness *reraceHarness, path *IpPath) {
	t.Helper()
	a := harness.channel("a")
	start := time.Now()
	harness.syn(path)
	if harness.boundTo(path) != a {
		t.Fatal("setup: the first SYN did not bind the flow to the only exit")
	}
	harness.sleep(reraceSynSchedule[1])
	harness.syn(path)
	harness.sleep(start.Add(reraceSynSchedule[2]).Sub(time.Now()))
	a.sendGroupForTest = func(*parsedPacketGroup, time.Duration, bool) (bool, error) {
		return false, nil
	}
	harness.syn(path)
	a.sendGroupForTest = nil
	update := harness.update(path)
	if update == nil || update.client.Load() != nil || update.race != nil {
		t.Fatal("setup: the refused re-race did not leave the flow with no client and no race")
	}
	if got := harness.metrics().FlowsReraced; got != 1 {
		t.Fatalf("FlowsReraced = %d, want the one inferred re-race", got)
	}
}

// The exit's SYN-ACK for the attempt arrives after the silence inference
// unbound the flow and its re-race was refused. It reaches the app at once,
// and the flow is established on the exit that sent it.
func TestSilenceInferenceUnboundFlowAdoptsItsExitsLateSynAck(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", answer: reraceAlways(reraceSilent)},
		}, nil)
		path := unboundFlowTcpPath(50100)
		unboundBySilenceInference(t, harness, path)

		harness.inject("a", reraceSynAck(path))
		if _, ok := harness.synAck(path); !ok {
			t.Fatal("the exit's late SYN-ACK did not reach the app")
		}
		if got := len(harness.log.linesWith("receive no race and no client")); got != 0 {
			t.Errorf("%d no-race-no-client drops, want none", got)
		}
		if harness.boundTo(path) != harness.channel("a") {
			t.Error("the flow is not bound to the exit whose answer it adopted")
		}
		if !harness.update(path).receivedInbound.Load() {
			t.Error("the adopted flow is not established")
		}
		reraceAssertNoErrorDelivered(t, harness, path)
	})
}

// The first packet the exit gets through for the unbound attempt is a data
// segment, as in the proxy churn signature B capture (a 1048-byte ACK segment
// dropped 40s after the last inference). The segment acknowledges the
// attempt's own bytes, so it belongs to the attempt and is adopted.
func TestSilenceInferenceUnboundFlowAdoptsItsExitsLateData(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", answer: reraceAlways(reraceSilent)},
		}, nil)
		path := unboundFlowTcpPath(50200)
		unboundBySilenceInference(t, harness, path)

		segment := reraceTcpPacket(path.Reverse(), tcpFlagAck|tcpFlagPsh, reraceServerIsn+1, path.SequenceNumber+1+517, []byte("server flight"))
		harness.inject("a", segment)
		if got := len(harness.delivered(path)); got != 1 {
			t.Fatalf("the app received %d packets of the unbound flow, want the exit's segment", got)
		}
		if got := len(harness.log.linesWith("receive no race and no client")); got != 0 {
			t.Errorf("%d no-race-no-client drops, want none", got)
		}
		if harness.boundTo(path) != harness.channel("a") {
			t.Error("the flow is not bound to the exit whose segment it adopted")
		}
	})
}

// A packet that does not acknowledge the unbound attempt's sequence space
// belongs to another connect attempt on the same 5-tuple and is still dropped.
func TestUnboundFlowDropsInboundOfAnotherConnectAttempt(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", answer: reraceAlways(reraceSilent)},
		}, nil)
		path := unboundFlowTcpPath(50250)
		unboundBySilenceInference(t, harness, path)

		// before the attempt's ISN, and half the sequence space away from it
		for _, ack := range []uint32{path.SequenceNumber + 1 - 4096, path.SequenceNumber + 1 + 2*1024*1024*1024} {
			harness.inject("a", reraceTcpPacket(path.Reverse(), tcpFlagAck|tcpFlagPsh, reraceServerIsn+1, ack, []byte("old")))
		}
		if got := len(harness.delivered(path)); got != 0 {
			t.Fatalf("the app received %d packets of another attempt, want none", got)
		}
		if got := len(harness.log.linesWith("receive no race and no client")); got != 2 {
			t.Errorf("%d no-race-no-client drops, want both packets'", got)
		}
		if harness.boundTo(path) != nil {
			t.Error("a packet of another attempt bound the flow")
		}
	})
}

// A provider signals a dial failure for the first SYN, which unbinds the flow
// and leaves it with no client and no race (clientDialFailure). Its NAT then
// answers after all: a SYN-ACK and a first data segment arrive before the app
// sends again. An exit that said it cannot reach the destination is not
// adopted, so both are dropped. That costs one SYN retransmit, never a stall:
// the attempt is still in its SYN schedule, and its first retransmit re-races
// onto the exit, whose answer reaches the app.
func TestLateAnswerAfterADialFailureSignalCostsOneSynRetransmit(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		syns := 0
		harness := newReraceHarness(t, []reraceExitSpec{{name: "a", answer: func(*IpPath) reraceAnswer {
			syns += 1
			if syns == 1 {
				return reraceSignal
			}
			return reraceAccept
		}}}, nil)
		a := harness.channel("a")
		path := unboundFlowTcpPath(50500)
		start := time.Now()
		harness.syn(path)
		harness.settle()
		if harness.boundTo(path) != nil || harness.update(path) == nil {
			t.Fatal("setup: the dial-failure signal did not unbind the flow")
		}
		metrics := harness.metrics()
		if metrics.DialFailuresIntercepted != 1 || metrics.FlowsReraced != 1 {
			t.Fatalf("DialFailuresIntercepted=%d FlowsReraced=%d, want 1 and 1", metrics.DialFailuresIntercepted, metrics.FlowsReraced)
		}

		// the provider's dial succeeded after all
		harness.inject("a", reraceSynAck(path))
		harness.inject("a", reraceTcpPacket(path.Reverse(), tcpFlagAck|tcpFlagPsh, reraceServerIsn+1, path.SequenceNumber+1, []byte("early")))
		if got := len(harness.delivered(path)); got != 0 {
			t.Fatalf("the app received %d packets of the unbound flow, want none", got)
		}
		if got := len(harness.log.linesWith("receive no race and no client")); got != 2 {
			t.Fatalf("%d no-race-no-client drops, want the SYN-ACK's and the segment's", got)
		}

		// the app's first retransmit
		harness.sleep(start.Add(reraceSynSchedule[1]).Sub(time.Now()))
		harness.syn(path)
		harness.settle()
		if got := len(harness.exit("a").syns(path)); got != 2 {
			t.Fatalf("the exit received %d SYNs, want the retransmit too", got)
		}
		if _, ok := harness.synAck(path); !ok {
			t.Fatal("the answer to the first retransmit did not reach the app")
		}
		if harness.boundTo(path) != a || !harness.update(path).receivedInbound.Load() {
			t.Error("the flow is not established on the exit after the first retransmit")
		}
		reraceAssertNoErrorDelivered(t, harness, path)
	})
}

// An established flow whose send fails with an error is unbound, and that
// unbinding always resets the app at once and is logged at the default
// verbosity. The flow is dead to the app, so the provider's later packets for
// it are dropped as no race and no client rather than adopted.
func TestEstablishedFlowIsNeverUnboundWithoutResettingTheApp(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{{name: "a"}}, nil)
		a := harness.channel("a")
		path := unboundFlowTcpPath(50300)
		if _, _, ok := harness.connect(path); !ok {
			t.Fatal("setup: the connect did not complete")
		}
		harness.send(reraceSegment(path, 0, []byte("one")))
		if !harness.update(path).receivedInbound.Load() {
			t.Fatal("setup: the flow is not established")
		}
		before := len(harness.delivered(path))

		a.sendGroupForTest = func(*parsedPacketGroup, time.Duration, bool) (bool, error) {
			return false, errors.New("send refused with an error")
		}
		harness.send(reraceSegment(path, 3, []byte("two")))
		a.sendGroupForTest = nil

		update := harness.update(path)
		if update.client.Load() != nil || update.race != nil {
			t.Fatal("setup: the send error did not unbind the established flow")
		}
		resets := 0
		for _, packet := range harness.delivered(path)[before:] {
			if parsed, err := ParseIpPath(packet.packet); err == nil && parsed.Rst {
				resets += 1
			}
		}
		if resets != 1 {
			t.Errorf("the app received %d resets, want exactly one", resets)
		}
		if got := len(harness.log.linesWith("[multi]reset error")); got != 1 {
			t.Errorf("%d default-verbosity reset lines, want one", got)
		}

		delivered := len(harness.delivered(path))
		harness.inject("a", reraceTcpPacket(path.Reverse(), tcpFlagAck|tcpFlagPsh, reraceServerIsn+1+3, path.SequenceNumber+1+6, []byte("late")))
		if got := len(harness.delivered(path)) - delivered; got != 0 {
			t.Errorf("the reset app received %d late packets, want none", got)
		}
		if got := len(harness.log.linesWith("receive no race and no client")); got != 1 {
			t.Errorf("%d no-race-no-client drops, want the late packet's", got)
		}
		if harness.boundTo(path) != nil {
			t.Error("a late packet adopted the reset flow")
		}
	})
}

// An established tcp flow whose exit is removed is torn down: removeClient
// resets the app and leaves the flow with no client and no race. A late
// answer for it, from the removed exit or from a live one, is dropped rather
// than adopted, because the app's connection is gone.
func TestExitRemovalTeardownIsNotAdoptedByALateAnswer(t *testing.T) {
	parent, dying, forwarded, _ := rebindTestParent(t, false, nil)
	live := rebindTestCandidate(parent.settings)
	path := unboundFlowTcpPath(50600)
	path.Syn = false
	update := rebindTestFlow(parent, dying, path, true)

	parent.removeClient(dying)
	if update.client.Load() != nil || !update.sourceReset.Load() {
		t.Fatal("setup: the exit removal did not tear the flow down")
	}
	if got := len(*forwarded); got != 1 {
		t.Fatalf("the app received %d teardown packets, want its reset", got)
	}

	for _, sender := range []*multiClientChannel{dying, live} {
		late := reraceTcpPacket(path.Reverse(), tcpFlagAck|tcpFlagPsh, reraceServerIsn+1, path.SequenceNumber+1, []byte("late"))
		lateIpPath, err := ParseIpPath(late)
		if err != nil {
			t.Fatal(err)
		}
		parent.clientReceivePacketResolve(sender, TransferPath{}, protocol.ProvideMode_Network, lateIpPath.Reverse(), late, tcpControlFromIpPath(lateIpPath))
	}
	if got := len(*forwarded); got != 1 {
		t.Errorf("the app received %d packets after its reset, want none", got-1)
	}
	if update.client.Load() != nil {
		t.Error("a late answer adopted the torn-down flow")
	}
}

// The app's dead socket answers the provider's late data with a reset, and
// that reset opens a new generation on the tuple (the signature B aftermath
// the reaper retires). The generation has no connect attempt, so the
// provider's next data segment is not adopted into it.
func TestResetOpenedGenerationIsNotAdoptedByProviderData(t *testing.T) {
	parent, _, forwarded, _ := rebindTestParent(t, false, nil)
	live := rebindTestCandidate(parent.settings)
	path := unboundFlowTcpPath(50700)
	path.Syn = false
	update := rebindTestFlow(parent, live, path, false)
	update.client.Store(nil)
	delete(parent.clientUpdates, live)

	data := reraceTcpPacket(path.Reverse(), tcpFlagAck|tcpFlagPsh, reraceServerIsn+1, path.SequenceNumber+1, []byte("data"))
	dataIpPath, err := ParseIpPath(data)
	if err != nil {
		t.Fatal(err)
	}
	parent.clientReceivePacketResolve(live, TransferPath{}, protocol.ProvideMode_Network, dataIpPath.Reverse(), data, tcpControlFromIpPath(dataIpPath))
	if got := len(*forwarded); got != 0 {
		t.Errorf("the app received %d packets for a generation with no connect attempt, want none", got)
	}
	if update.client.Load() != nil {
		t.Error("provider data adopted a generation with no connect attempt")
	}
}
