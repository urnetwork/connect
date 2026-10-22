package connect

// A flow can be left with no client and no race while its exit is working:
// the silence inference unbinds a connect attempt whose answer is only slow,
// and the re-race of that retransmit can be refused. What the exit then sends
// for the flow is the first proof that it works, so its sender adopts the
// flow, rather than the packet being dropped ("[multi]receive no race and no
// client") until the app happens to send again.
//
// Adoption is a placement, bounded like one. Only the exit that carried the
// attempt adopts it, only with a packet that acknowledges what the attempt
// sent, and only while the exit could be raced for the flow: live, not
// quarantined, able to carry its ip version and under its flow cap. The
// inference never unbinds a flow whose first answer it can see, and a race
// whose every candidate refused ends rather than lingering to drop the
// answer.
//
// Every other way a flow loses its client is bounded here too. A provider's
// explicit dial-failure signal, before or after the inference, keeps the drop
// and waits for the app's retransmit. A send error and an exit removal reset
// the app and leave the flow dead, so a late answer is still dropped, until
// the app opens a new attempt on the tuple. A generation an app reset opened
// has no connect attempt and adopts nothing. The idle reaper and the
// quarantine hand-off detach the flow from the path map before they clear its
// client (TestDetachIdleFlowsCleansRoutingAndPreservesNextDeadline,
// ip_remote_multi_client_h1_blackhole_test.go), so they leave no flow without
// a client to adopt.

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"sync/atomic"
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

// Silence inference judges one retained healthy owner. Prove that premise
// with an unrelated real connection; every tested-flow answer stays unchanged.
// Build the answer policy before publishing any provider callback.
func newUnboundInferenceHarness(t *testing.T, path *IpPath, specs []reraceExitSpec) *reraceHarness {
	t.Helper()
	healthPath := reraceFlow(4, 1, 49111)
	if len(specs) == 0 || specs[0].name != "a" || reraceSameFlow(path, healthPath) {
		t.Fatal("unbound inference needs incumbent A and a distinct healthy sibling flow")
	}
	specs = slices.Clone(specs)
	answer := specs[0].answer
	specs[0].answer = func(candidate *IpPath) reraceAnswer {
		if reraceSameFlow(candidate, healthPath) {
			return reraceAccept
		}
		if answer != nil {
			return answer(candidate)
		}
		return reraceAccept
	}
	harness := newReraceHarness(t, specs, nil)
	// This helper verifies actual closed buckets, positive send/receive bytes
	// and both the returned WindowStats and channel's live healthy state.
	if actual := reraceHealthySibling(t, harness, 4); !reraceSameFlow(actual, healthPath) {
		t.Fatal("healthy sibling no longer matches the isolated answer policy")
	}
	return harness
}

// Opens a flow on exit a, the only exit of the best tier, which answers
// nothing, and lets the silence inference unbind it at the app's 3s
// retransmit while every exit refuses that retransmit's re-race. It returns
// how many offers of the re-race each exit refused.
func unboundBySilenceInference(t *testing.T, harness *reraceHarness, path *IpPath) map[*multiClientChannel]int {
	t.Helper()
	a := harness.channel("a")
	start := time.Now()
	harness.syn(path)
	if harness.boundTo(path) != a {
		t.Fatal("setup: the first SYN did not bind the flow to exit a")
	}
	harness.sleep(reraceSynSchedule[1])
	harness.syn(path)
	harness.sleep(start.Add(reraceSynSchedule[2]).Sub(time.Now()))
	var mutex sync.Mutex
	refused := map[*multiClientChannel]int{}
	channels := harness.liveChannels()
	for _, channel := range channels {
		channel.sendGroupForTest = func(*parsedPacketGroup, time.Duration, bool) (bool, error) {
			mutex.Lock()
			defer mutex.Unlock()
			refused[channel] += 1
			return false, nil
		}
	}
	harness.syn(path)
	for _, channel := range channels {
		channel.sendGroupForTest = nil
	}
	if harness.update(path) == nil || harness.boundTo(path) != nil {
		t.Fatal("setup: the inference did not unbind the flow")
	}
	if got := harness.metrics().FlowsReraced; got != 1 {
		t.Fatalf("FlowsReraced = %d, want the one inferred re-race", got)
	}
	return refused
}

// The exit's SYN-ACK for the attempt arrives after the silence inference
// unbound the flow and its re-race was refused. It reaches the app at once,
// the flow is established on the exit that sent it, and the adoption is
// logged at the default verbosity.
func TestSilenceInferenceUnboundFlowAdoptsItsExitsLateSynAck(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		path := unboundFlowTcpPath(50100)
		harness := newUnboundInferenceHarness(t, path, []reraceExitSpec{
			{name: "a", answer: reraceAlways(reraceSilent)},
		})
		unboundBySilenceInference(t, harness, path)

		harness.inject("a", reraceSynAck(path))
		if _, ok := harness.synAck(path); !ok {
			t.Fatal("the exit's late SYN-ACK did not reach the app")
		}
		if got := len(harness.log.linesWith("receive no race and no client")); got != 0 {
			t.Errorf("%d no-race-no-client drops, want none", got)
		}
		if got := len(harness.log.linesWith("[multi]receive adopted a flow with no race and no client")); got != 1 {
			t.Errorf("%d default-verbosity adoption lines, want one", got)
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
// dropped 40s after the last inference). It acknowledges ISN+1 though the
// attempt never completed: connect's provider relays a server-first
// upstream's bytes right after its SYN-ACK without waiting for the app's ACK
// (TcpSequence.Run), so when the SYN-ACK is lost the first data segment is
// the attempt's first answer, and it can acknowledge nothing past the SYN,
// which is all the app has sent. It belongs to the attempt and is adopted.
func TestSilenceInferenceUnboundFlowAdoptsItsExitsLateData(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		path := unboundFlowTcpPath(50200)
		harness := newUnboundInferenceHarness(t, path, []reraceExitSpec{
			{name: "a", answer: reraceAlways(reraceSilent)},
		})
		unboundBySilenceInference(t, harness, path)

		segment := reraceTcpPacket(path.Reverse(), tcpFlagAck|tcpFlagPsh, reraceServerIsn+1, path.SequenceNumber+1, []byte("220 server flight"))
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
		path := unboundFlowTcpPath(50250)
		harness := newUnboundInferenceHarness(t, path, []reraceExitSpec{
			{name: "a", answer: reraceAlways(reraceSilent)},
		})
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

// The unbound attempt's own exit adopts it only with a packet acknowledging
// what the attempt sent, its SYN, which carried no data. Segments that
// acknowledge one byte or 300 MiB past the SYN, both inside the quarter of
// sequence space adoption once took, are dropped as blind or stale; the
// SYN-ACK is then adopted.
func TestUnboundFlowIsNotAdoptedByAnAckPastWhatItsAttemptSent(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		path := unboundFlowTcpPath(50260)
		harness := newUnboundInferenceHarness(t, path, []reraceExitSpec{
			{name: "a", answer: reraceAlways(reraceSilent)},
		})
		unboundBySilenceInference(t, harness, path)

		for _, ack := range []uint32{path.SequenceNumber + 2, path.SequenceNumber + 1 + 300*1024*1024} {
			harness.inject("a", reraceTcpPacket(path.Reverse(), tcpFlagAck|tcpFlagPsh, reraceServerIsn+1, ack, []byte("blind")))
		}
		if got := len(harness.delivered(path)); got != 0 {
			t.Fatalf("the app received %d packets acknowledging bytes it never sent, want none", got)
		}
		if got := len(harness.log.linesWith("receive no race and no client")); got != 2 {
			t.Errorf("%d no-race-no-client drops, want both packets'", got)
		}
		if harness.boundTo(path) != nil {
			t.Fatal("a packet acknowledging bytes the app never sent adopted the flow")
		}

		harness.inject("a", reraceSynAck(path))
		if _, ok := harness.synAck(path); !ok || harness.boundTo(path) != harness.channel("a") {
			t.Error("the exit's SYN-ACK did not adopt the flow after the refused segments")
		}
	})
}

// Only the exit that carried the unbound attempt adopts it. Exit b, which the
// refused re-race only offered the retransmit to, sends a SYN-ACK for the
// attempt's own sequence, and it is dropped. Exit a's same SYN-ACK is then
// adopted.
func TestUnboundFlowIsAdoptedOnlyByTheExitThatCarriedItsAttempt(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		path := unboundFlowTcpPath(50270)
		harness := newUnboundInferenceHarness(t, path, []reraceExitSpec{
			{name: "a", tier: 0, answer: reraceAlways(reraceSilent)},
			{name: "b", tier: 1, answer: reraceAlways(reraceSilent)},
		})
		refused := unboundBySilenceInference(t, harness, path)
		if refused[harness.channel("b")] == 0 || 0 < len(harness.exit("b").syns(path)) {
			t.Fatal("setup: the re-race did not offer exit b the retransmit, refused, before any SYN reached it")
		}

		harness.inject("b", reraceSynAck(path))
		if got := len(harness.delivered(path)); got != 0 {
			t.Fatalf("the app received %d packets from an exit that never carried its attempt, want none", got)
		}
		if got := len(harness.log.linesWith("receive no race and no client")); got != 1 {
			t.Errorf("%d no-race-no-client drops, want exit b's SYN-ACK", got)
		}
		if harness.boundTo(path) != nil {
			t.Fatal("an exit that never carried the attempt adopted it")
		}

		harness.inject("a", reraceSynAck(path))
		if _, ok := harness.synAck(path); !ok || harness.boundTo(path) != harness.channel("a") {
			t.Error("the exit that carried the attempt did not adopt it")
		}
	})
}

// The first answer can commit while the silence inference holds the parent
// lock, after its caller saw the flow unanswered: the receive path commits it
// under the flow lock alone. The inference then checks for the answer and
// unbinds in one step under that lock, so it finds the flow answered and
// leaves it on its exit, with no strike and no re-race. The hook commits the
// exit's SYN-ACK at that exact point, and the exit refuses the retransmit, so
// a flow the inference wrongly unbound stays unbound rather than racing back
// onto the same exit.
func TestAnswerDuringTheSilenceInferenceKeepsTheFlowOnItsExit(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		path := unboundFlowTcpPath(50280)
		harness := newUnboundInferenceHarness(t, path, []reraceExitSpec{
			{name: "a", answer: reraceAlways(reraceSilent)},
		})
		a := harness.channel("a")
		start := time.Now()
		harness.syn(path)
		harness.sleep(reraceSynSchedule[1])
		harness.syn(path)
		harness.sleep(start.Add(reraceSynSchedule[2]).Sub(time.Now()))

		synAck, err := ParseIpPath(reraceSynAck(path))
		if err != nil {
			t.Fatal(err)
		}
		answered := 0
		harness.multiClient.beforeDialFailureStandingForTest = func(update *multiClientChannelUpdate) {
			answered += 1
			if !update.markReceivedInbound(a, tcpControlFromIpPath(synAck)) {
				t.Error("setup: the exit's SYN-ACK did not commit the flow's first answer")
			}
		}
		a.sendGroupForTest = func(*parsedPacketGroup, time.Duration, bool) (bool, error) {
			return false, nil
		}
		harness.syn(path)
		a.sendGroupForTest = nil
		harness.multiClient.beforeDialFailureStandingForTest = nil
		if answered != 1 {
			t.Fatalf("setup: the inference ran %d times, want once", answered)
		}

		if harness.boundTo(path) != a {
			t.Error("the inference unbound a flow whose first answer had committed")
		}
		if got := harness.metrics().FlowsReraced; got != 0 {
			t.Errorf("FlowsReraced = %d, want 0 for an answered flow", got)
		}
		if got := a.dialFailureCount(); got != 0 {
			t.Errorf("exit a carries %d strikes for an attempt it answered, want 0", got)
		}
	})
}

// Exit a's first answer for the flow, its SYN-ACK, arrives as the silence
// inference unbinds the flow from a: the hook runs the inference inside the
// window a lock-free receive path leaves between its client check and its
// mark. A first answer is decided under the flow lock, where the inference
// unbinds, so it never takes that window: whatever reaches the app leaves the
// flow established on a, and the app's first bytes follow it there. Read
// lock-free, the answer reached the app for an unbound flow, and the bytes
// re-raced onto exit b, which never saw a SYN.
func answerRacingTheSilenceInference(t *testing.T, harness *reraceHarness, path *IpPath) {
	t.Helper()
	a := harness.channel("a")
	harness.syn(path)
	if harness.boundTo(path) != a {
		t.Fatal("setup: the first SYN did not bind the flow to exit a")
	}
	a.beforeFirstAnswerMarkForTest = func(update *multiClientChannelUpdate) {
		harness.multiClient.clientDialFailure(a, update.ipPath, false)
	}
	harness.inject("a", reraceSynAck(path))
	a.beforeFirstAnswerMarkForTest = nil
	if _, ok := harness.synAck(path); !ok {
		t.Fatal("exit a's SYN-ACK did not reach the app")
	}
	update := harness.update(path)
	if update.client.Load() != a || !update.receivedInbound.Load() {
		t.Error("the app received exit a's SYN-ACK for a flow that is not established on exit a")
	}
	if got := harness.metrics().FlowsReraced; got != 0 {
		t.Errorf("FlowsReraced = %d, want 0 for a flow its exit answered", got)
	}

	harness.send(reraceSegment(path, 0, []byte("hello")))
	carried := false
	for _, packet := range harness.exit("a").received(path) {
		if string(packet.payload) == "hello" {
			carried = true
		}
	}
	if !carried {
		t.Error("the app's first bytes did not reach exit a")
	}
	if got := len(harness.exit("b").received(path)); got != 0 {
		t.Errorf("exit b received %d packets of a connection it never saw a SYN for, want none", got)
	}
}

// The per-packet receive path (no batch receive callback).
func TestFirstAnswerRacingTheSilenceInferenceStaysOnItsExit(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: reraceAlways(reraceSilent)},
			{name: "b", tier: 1},
		}, nil)
		answerRacingTheSilenceInference(t, harness, unboundFlowTcpPath(50310))
	})
}

// The batch receive path, which a batch receive callback turns on. The
// callback records each packet as the app receives it.
func TestFirstAnswerRacingTheSilenceInferenceStaysOnItsExitInABatch(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: reraceAlways(reraceSilent)},
			{name: "b", tier: 1},
		}, nil)
		var batches atomic.Int32
		harness.multiClient.SetReceivePacketsCallback(func(source TransferPath, provideMode protocol.ProvideMode, _ *IpPath, packets [][]byte) {
			batches.Add(1)
			for _, packet := range packets {
				if parsed, err := ParseIpPath(packet); err == nil {
					harness.receive(source, provideMode, parsed, packet)
				}
			}
		})
		answerRacingTheSilenceInference(t, harness, unboundFlowTcpPath(50320))
		if batches.Load() == 0 {
			t.Error("setup: no packet reached the app through the batch receive callback")
		}
	})
}

// A provider's dial-failure signal that lands after the silence inference
// already unbound the flow still marks its exit as one that said it cannot
// reach the destination. That exit's later SYN-ACK is dropped, and the app's
// retransmit re-races instead, as when the signal comes before the inference
// (TestLateAnswerAfterADialFailureSignalCostsOneSynRetransmit).
func TestDialFailureSignalAfterTheInferenceBarsItsExitsLateAnswer(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		path := unboundFlowTcpPath(50290)
		harness := newUnboundInferenceHarness(t, path, []reraceExitSpec{
			{name: "a", answer: reraceAlways(reraceSilent)},
		})
		unboundBySilenceInference(t, harness, path)

		harness.inject("a", reraceProviderSignal(t, path))
		if got := harness.metrics().DialFailuresIntercepted; got != 1 {
			t.Fatalf("setup: DialFailuresIntercepted = %d, want the late signal", got)
		}
		harness.inject("a", reraceSynAck(path))
		if got := len(harness.delivered(path)); got != 0 {
			t.Fatalf("the app received %d packets after its exit signaled a dial failure, want none", got)
		}
		if got := len(harness.log.linesWith("receive no race and no client")); got != 1 {
			t.Errorf("%d no-race-no-client drops, want the SYN-ACK's", got)
		}
		if harness.boundTo(path) != nil {
			t.Error("an exit that signaled a dial failure adopted the flow")
		}
	})
}

// Opens a flow on exit a, the only exit of the best tier, and lets the silence
// inference unbind it at the app's 3s retransmit, which re-races onto exits b
// and c. Every exit takes its SYN and none answers, so the flow is left
// racing b and c, which a race never commits on for a SYN.
func reracedOntoSilentExits(t *testing.T, harness *reraceHarness, path *IpPath) *multiClientChannelUpdate {
	t.Helper()
	a := harness.channel("a")
	start := time.Now()
	harness.syn(path)
	if harness.boundTo(path) != a {
		t.Fatal("setup: the first SYN did not bind the flow to exit a")
	}
	harness.sleep(reraceSynSchedule[1])
	harness.syn(path)
	harness.sleep(start.Add(reraceSynSchedule[2]).Sub(time.Now()))
	harness.syn(path)
	update := harness.update(path)
	racing := func() bool {
		update.stateLock.Lock()
		defer update.stateLock.Unlock()
		return update.client.Load() == nil && update.race != nil &&
			update.race.clientStates[harness.channel("b")] != nil &&
			update.race.clientStates[harness.channel("c")] != nil
	}()
	if !racing {
		t.Fatal("setup: the inference did not re-race the flow onto exits b and c")
	}
	for _, name := range []string{"b", "c"} {
		if got := len(harness.exit(name).syns(path)); got != 1 {
			t.Fatalf("setup: exit %s received %d SYNs, want the re-raced retransmit", name, got)
		}
	}
	return update
}

// Exit a's SYN-ACK for the attempt arrives while the flow races the silent
// exits b and c. a carried the attempt, so its answer joins the race as a
// candidate's would and wins it: the app gets the SYN-ACK at once, the flow is
// established on a, and b and c are reset as any race commit resets its
// other candidates. Dropped as coming from no candidate of the race, a's
// answers would leave the app retransmitting into the silent race until its
// connect timed out.
func TestCarryingExitsAnswerWinsAReraceOfSilentExits(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		path := unboundFlowTcpPath(50330)
		harness := newUnboundInferenceHarness(t, path, []reraceExitSpec{
			{name: "a", tier: 0, answer: reraceAlways(reraceSilent)},
			{name: "b", tier: 1, answer: reraceAlways(reraceSilent)},
			{name: "c", tier: 1, answer: reraceAlways(reraceSilent)},
		})
		update := reracedOntoSilentExits(t, harness, path)

		harness.inject("a", reraceSynAck(path))
		if _, ok := harness.synAck(path); !ok {
			t.Fatal("exit a's SYN-ACK did not reach the app")
		}
		if got := len(harness.log.linesWith("receive client not part of race")); got != 0 {
			t.Errorf("%d not-part-of-race drops, want none", got)
		}
		if update.client.Load() != harness.channel("a") || !update.receivedInbound.Load() {
			t.Error("the flow is not established on exit a")
		}
		for _, name := range []string{"b", "c"} {
			resets := 0
			for _, packet := range harness.exit(name).received(path) {
				if packet.path.Rst {
					resets += 1
				}
			}
			if resets != 1 {
				t.Errorf("exit %s received %d resets, want the abandoned candidate's one", name, resets)
			}
		}
	})
}

// An exit that signaled a dial failure for the attempt does not win the race
// of silent exits with a later answer: its late signal still marks it
// (TestDialFailureSignalAfterTheInferenceBarsItsExitsLateAnswer), and its
// SYN-ACK is dropped as coming from no candidate of the race, which goes on.
func TestSignaledExitsAnswerDoesNotWinAReraceOfSilentExits(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		path := unboundFlowTcpPath(50335)
		harness := newUnboundInferenceHarness(t, path, []reraceExitSpec{
			{name: "a", tier: 0, answer: reraceAlways(reraceSilent)},
			{name: "b", tier: 1, answer: reraceAlways(reraceSilent)},
			{name: "c", tier: 1, answer: reraceAlways(reraceSilent)},
		})
		update := reracedOntoSilentExits(t, harness, path)

		harness.inject("a", reraceProviderSignal(t, path))
		harness.inject("a", reraceSynAck(path))
		if got := len(harness.delivered(path)); got != 0 {
			t.Fatalf("the app received %d packets after exit a signaled a dial failure, want none", got)
		}
		if got := len(harness.log.linesWith("receive client not part of race")); got != 1 {
			t.Errorf("%d not-part-of-race drops, want the SYN-ACK's", got)
		}
		stillRacing := func() bool {
			update.stateLock.Lock()
			defer update.stateLock.Unlock()
			return update.client.Load() == nil && update.race != nil
		}()
		if !stillRacing {
			t.Error("a signaled exit's answer ended the race")
		}
	})
}

// When every candidate of the retransmit's re-race refuses it, the race ends,
// as a race of one does, rather than lingering empty until the flow's next
// send. The exit that carried the attempt then adopts its late SYN-ACK, which
// a lingering race drops as coming from no candidate of it.
func TestReraceRefusedByEveryCandidateEndsForTheExitsLateAnswer(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		path := unboundFlowTcpPath(50295)
		harness := newUnboundInferenceHarness(t, path, []reraceExitSpec{
			{name: "a", tier: 0, answer: reraceAlways(reraceSilent)},
			{name: "b", tier: 1, answer: reraceAlways(reraceSilent)},
			{name: "c", tier: 1, answer: reraceAlways(reraceSilent)},
		})
		refused := unboundBySilenceInference(t, harness, path)
		if refused[harness.channel("b")] == 0 || refused[harness.channel("c")] == 0 {
			t.Fatal("setup: the re-race was not offered to both exits b and c")
		}
		if harness.update(path).race != nil {
			t.Error("the re-race whose every candidate refused still holds the flow")
		}

		harness.inject("a", reraceSynAck(path))
		if _, ok := harness.synAck(path); !ok {
			t.Fatal("the exit's late SYN-ACK did not reach the app")
		}
		if got := len(harness.log.linesWith("receive client not part of race")); got != 0 {
			t.Errorf("%d not-part-of-race drops, want none", got)
		}
		if harness.boundTo(path) != harness.channel("a") {
			t.Error("the flow is not bound to the exit whose answer it adopted")
		}
	})
}

// The exit's answer can adopt the flow while the send that left it with no
// client finishes its bookkeeping. The bookkeeping keeps the exit's record of
// the adopted flow, which removeClient tears down and the flow cap counts,
// rather than dropping it as the record of a flow that moved away. The hook
// adopts the flow at that exact point.
func TestFlowAdoptedDuringItsSendStaysOnItsExitsRecord(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		path := unboundFlowTcpPath(50297)
		harness := newUnboundInferenceHarness(t, path, []reraceExitSpec{
			{name: "a", answer: reraceAlways(reraceSilent)},
		})
		a := harness.channel("a")
		start := time.Now()
		harness.syn(path)
		harness.sleep(reraceSynSchedule[1])
		harness.syn(path)
		harness.sleep(start.Add(reraceSynSchedule[2]).Sub(time.Now()))

		synAck := reraceSynAck(path)
		inbound, err := ParseIpPath(synAck)
		if err != nil {
			t.Fatal(err)
		}
		adopted := 0
		harness.multiClient.beforeUncommittedSendBookkeepingForTest = func(update *multiClientChannelUpdate) {
			adopted += 1
			harness.multiClient.clientReceivePacketResolve(a, TransferPath{}, protocol.ProvideMode_Network, inbound.Reverse(), synAck, tcpControlFromIpPath(inbound))
		}
		a.sendGroupForTest = func(*parsedPacketGroup, time.Duration, bool) (bool, error) {
			return false, nil
		}
		harness.syn(path)
		a.sendGroupForTest = nil
		harness.multiClient.beforeUncommittedSendBookkeepingForTest = nil
		if adopted != 1 || harness.boundTo(path) != a {
			t.Fatalf("setup: the hook ran %d times and the flow is not on exit a", adopted)
		}

		update := harness.update(path)
		recorded := func() bool {
			harness.multiClient.stateLock.Lock()
			defer harness.multiClient.stateLock.Unlock()
			return harness.multiClient.clientUpdates[a][update]
		}()
		if !recorded {
			t.Error("exit a carries the adopted flow without a record of it")
		}
	})
}

// A send error resets the app's connection and marks the flow's source reset,
// and the app then opens a new connect attempt on the same tuple. The new
// attempt is alive: when the silence inference unbinds it and its re-race is
// refused, its exit's late SYN-ACK is adopted like any attempt's.
func TestNewConnectAttemptAfterAResetIsAdoptedByItsExit(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		path := unboundFlowTcpPath(50298)
		harness := newUnboundInferenceHarness(t, path, []reraceExitSpec{{name: "a", answer: func(syn *IpPath) reraceAnswer {
			if syn.SequenceNumber == path.SequenceNumber {
				return reraceAccept
			}
			return reraceSilent
		}}})
		a := harness.channel("a")
		if _, _, ok := harness.connect(path); !ok {
			t.Fatal("setup: the first attempt did not connect")
		}
		a.sendGroupForTest = func(*parsedPacketGroup, time.Duration, bool) (bool, error) {
			return false, errors.New("send refused with an error")
		}
		harness.send(reraceSegment(path, 0, []byte("one")))
		a.sendGroupForTest = nil
		update := harness.update(path)
		if update.client.Load() != nil || !update.sourceReset.Load() {
			t.Fatal("setup: the send error did not reset the first attempt")
		}

		next := *path
		next.SequenceNumber = path.SequenceNumber + 1000000
		unboundBySilenceInference(t, harness, &next)
		if harness.update(&next) != update {
			t.Fatal("setup: the new attempt did not reuse the reset flow")
		}
		if update.sourceReset.Load() {
			t.Error("the new connect attempt is still marked as reset")
		}
		harness.inject("a", reraceSynAck(&next))
		answered := false
		for _, packet := range harness.delivered(&next) {
			if parsed, err := ParseIpPath(packet.packet); err == nil && parsed.Syn && parsed.Ack && parsed.AckSequenceNumber == next.SequenceNumber+1 {
				answered = true
			}
		}
		if !answered || harness.boundTo(&next) != a {
			t.Error("the exit's late SYN-ACK for the new attempt was not adopted")
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

// A bare parent, wired as for dialFailureTestParent, carrying one flow whose
// connect attempt on exit the silence inference unbound: the app's SYN was
// noted and committed on exit, and the inference took the flow off it. With no
// window or transfer around them, each case below depends only on the flow
// and the exit.
func unboundFlowFixture(t *testing.T, path *IpPath) (*RemoteUserNatMultiClient, *multiClientChannel, *multiClientChannelUpdate, *[]*receivePacket) {
	t.Helper()
	parent, exit, update, delivered := dialFailureTestParent(t, true, path)
	parent.log = newRecordingLogger()
	exit.ctx = context.Background()
	update.noteConnectAttempt(path)
	update.updateSequence(&parsedPacket{ipPath: path})
	if !parent.clientDialFailure(exit, path, false) || update.client.Load() != nil {
		t.Fatal("setup: the silence inference did not unbind the flow")
	}
	return parent, exit, update, delivered
}

// Hands the exit's packet to the receive path after its accounting, as
// clientReceivePackets does, and reports whether the app received it.
func unboundFlowAnswer(t *testing.T, parent *RemoteUserNatMultiClient, exit *multiClientChannel, delivered *[]*receivePacket, packet []byte) bool {
	t.Helper()
	inbound, err := ParseIpPath(packet)
	if err != nil {
		t.Fatal(err)
	}
	before := len(*delivered)
	parent.clientReceivePacketResolve(exit, TransferPath{}, protocol.ProvideMode_Network, inbound.Reverse(), packet, tcpControlFromIpPath(inbound))
	return before < len(*delivered)
}

// A quarantined exit does not adopt the unbound flow: quarantine excludes it
// from placement, and adoption places the flow. The receive accounting that
// lifts a quarantine runs before the receive path, so here the quarantine
// holds while the answer resolves. Once it lifts, the same SYN-ACK is
// adopted.
func TestUnboundFlowIsNotAdoptedByAQuarantinedExit(t *testing.T) {
	path := unboundFlowTcpPath(51000)
	parent, exit, update, delivered := unboundFlowFixture(t, path)
	setQuarantined := func(quarantined bool) {
		exit.stateLock.Lock()
		defer exit.stateLock.Unlock()
		exit.quarantined = quarantined
	}

	setQuarantined(true)
	if unboundFlowAnswer(t, parent, exit, delivered, reraceSynAck(path)) || update.client.Load() != nil {
		t.Fatal("a quarantined exit adopted the unbound flow")
	}
	setQuarantined(false)
	if !unboundFlowAnswer(t, parent, exit, delivered, reraceSynAck(path)) || update.client.Load() != exit {
		t.Error("the exit did not adopt the unbound flow once its quarantine lifted")
	}
}

// The exit's reset for the unbound attempt is not adopted: delivered, it
// would end an attempt the app's retransmit can still re-race onto another
// exit. The exit's SYN-ACK that follows is adopted.
func TestUnboundFlowIsNotAdoptedByAReset(t *testing.T) {
	path := unboundFlowTcpPath(51100)
	parent, exit, update, delivered := unboundFlowFixture(t, path)

	reset := reraceTcpPacket(path.Reverse(), tcpFlagRst|tcpFlagAck, 0, path.SequenceNumber+1, nil)
	if unboundFlowAnswer(t, parent, exit, delivered, reset) || update.client.Load() != nil {
		t.Fatal("a reset adopted the unbound flow")
	}
	if !unboundFlowAnswer(t, parent, exit, delivered, reraceSynAck(path)) || update.client.Load() != exit {
		t.Error("the exit's SYN-ACK did not adopt the flow after the refused reset")
	}
}

// A closed exit does not adopt the unbound flow, even before its removal
// drops it from the exits that failed the attempt (forgetDialFailedClient); a
// live one does.
func TestUnboundFlowIsNotAdoptedByAClosedExit(t *testing.T) {
	for _, closed := range []bool{false, true} {
		path := unboundFlowTcpPath(51200)
		parent, exit, update, delivered := unboundFlowFixture(t, path)
		if closed {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			exit.ctx = ctx
		}
		adopted := unboundFlowAnswer(t, parent, exit, delivered, reraceSynAck(path))
		if adopted == closed || (update.client.Load() == exit) == closed {
			t.Errorf("closed exit %t: adopted %t, want %t", closed, adopted, !closed)
		}
	}
}

// A flow that is done, retired with its generation while an answer was on the
// way to it, is not adopted; a live one is.
func TestDoneUnboundFlowIsNotAdopted(t *testing.T) {
	for _, done := range []bool{false, true} {
		path := unboundFlowTcpPath(51300)
		parent, exit, update, delivered := unboundFlowFixture(t, path)
		if done {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			update.ctx, update.cancel = ctx, cancel
		}
		adopted := unboundFlowAnswer(t, parent, exit, delivered, reraceSynAck(path))
		if adopted == done || (update.client.Load() == exit) == done {
			t.Errorf("done flow %t: adopted %t, want %t", done, adopted, !done)
		}
	}
}

// An exit at its flow cap does not adopt the unbound flow, as the cap keeps
// the exit out of the flow's race; under the cap it does.
func TestUnboundFlowIsNotAdoptedByAnExitAtItsFlowCap(t *testing.T) {
	for _, atCap := range []bool{false, true} {
		path := unboundFlowTcpPath(51400)
		parent, exit, update, delivered := unboundFlowFixture(t, path)
		parent.settings.MaxFlowsPerExit = 1
		if atCap {
			rebindTestFlow(parent, exit, unboundFlowTcpPath(51401), true)
		}
		adopted := unboundFlowAnswer(t, parent, exit, delivered, reraceSynAck(path))
		if adopted == atCap || (update.client.Load() == exit) == atCap {
			t.Errorf("exit at its flow cap %t: adopted %t, want %t", atCap, adopted, !atCap)
		}
	}
}

// An exit downgraded to IPv4 only (B5, which a v6 attempt's own strikes can
// trigger) does not adopt an unbound IPv6 flow, as it is no longer raced for
// one; a dual-stack exit does.
func TestUnboundIpv6FlowIsNotAdoptedByAnExitDowngradedToIpv4(t *testing.T) {
	for _, downgraded := range []bool{false, true} {
		path := reraceFlow(6, 0, 51500)
		path.SourceIp = net.ParseIP("2001:db8::2")
		parent, exit, update, delivered := unboundFlowFixture(t, path)
		exit.args = &multiClientChannelArgs{DestinationStats: DestinationStats{IpFamily: IpFamilyDualstack}}
		exit.ipFamilyDowngraded.Store(downgraded)
		adopted := unboundFlowAnswer(t, parent, exit, delivered, reraceSynAck(path))
		if adopted == downgraded || (update.client.Load() == exit) == downgraded {
			t.Errorf("exit downgraded to IPv4 %t: adopted %t, want %t", downgraded, adopted, !downgraded)
		}
	}
}

// The acknowledgement bound compares modularly, so an attempt whose sequence
// wraps past 2^32 is judged like any other: an ISN of 0xffffffff, whose
// SYN-ACK acknowledges 0, and a fast-open SYN at 0xfffffff0 whose 32 bytes of
// data end past the wrap, at 0x11. For each, the attempt's own ISN and one
// sequence past what its SYN sent are refused, and an acknowledgement of
// everything it sent, on the far side of the wrap, is adopted.
func TestUnboundFlowAdoptionComparesAcksAcrossTheSequenceWrap(t *testing.T) {
	for _, attempt := range []struct {
		isn          uint32
		payloadBytes int
		ack          uint32
	}{
		{isn: 0xffffffff, payloadBytes: 0, ack: 0},
		{isn: 0xfffffff0, payloadBytes: 32, ack: 0x11},
	} {
		path := unboundFlowTcpPath(51600)
		path.SequenceNumber = attempt.isn
		path.TcpPayloadByteCount = attempt.payloadBytes
		parent, exit, update, delivered := unboundFlowFixture(t, path)
		for _, ack := range []uint32{attempt.isn, attempt.ack + 1} {
			synAck := reraceTcpPacket(path.Reverse(), tcpFlagSyn|tcpFlagAck, reraceServerIsn, ack, nil)
			if unboundFlowAnswer(t, parent, exit, delivered, synAck) || update.client.Load() != nil {
				t.Errorf("ISN %#x: an acknowledgement of %#x adopted the flow", attempt.isn, ack)
			}
		}
		synAck := reraceTcpPacket(path.Reverse(), tcpFlagSyn|tcpFlagAck, reraceServerIsn, attempt.ack, nil)
		if !unboundFlowAnswer(t, parent, exit, delivered, synAck) || update.client.Load() != exit {
			t.Errorf("ISN %#x: the acknowledgement of everything the SYN sent, %#x, was not adopted", attempt.isn, attempt.ack)
		}
	}
}

// The adoption commit replays the exit's admitted claims on the flow, and a
// claim whose transfer already ended is released by it. The commit runs after
// the receive's own drain, so it drains what it released at once: the claim's
// retained memory returns to its budget, rather than waiting for the flow's
// next send or receive.
func TestUnboundFlowAdoptionReleasesTheClaimsItsCommitEnds(t *testing.T) {
	path := unboundFlowTcpPath(51700)
	parent, exit, update, delivered := unboundFlowFixture(t, path)
	budget := NewTransferMemoryBudget(1024 * 1024)
	budget.Reserve(4096)
	claim := &parsedPacketGroup{
		completionClient: exit,
		collapseOwner:    &tcpCollapseGroup{budget: budget, budgetBytes: 4096},
	}
	claim.collapseAdmission.update = update
	claim.completionFlags.Store(groupCompletionTracked | groupCompletionAdmitted | groupCompletionDequeued |
		groupCompletionLinked | groupCompletionDone | groupCompletionTerminal | groupCompletionReturned | 1<<16)
	update.sequenceClaims, update.sequenceClaimsTail = claim, claim

	if !unboundFlowAnswer(t, parent, exit, delivered, reraceSynAck(path)) || update.client.Load() != exit {
		t.Fatal("setup: the exit's SYN-ACK did not adopt the flow")
	}
	if update.sequenceReleasedClaims.Load() != nil {
		t.Error("the claim the adoption released is still waiting for a drain")
	}
	if got := budget.UsedByteCount(); got != 0 {
		t.Errorf("the ended claim still holds %d bytes of its budget, want 0", got)
	}
}
