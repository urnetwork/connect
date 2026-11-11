//go:build !js && !windows

package connect

// The storm test of RR-8 (see ip_remote_multi_client_rerace_test.go) also
// bounds the cpu the client spends idle after the storm. It reads this
// process's cpu time through syscall.Getrusage, which js/wasm and windows do
// not have, so it builds only where net_extender_scale_cpu_test.go does.

import (
	"runtime"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Each event copies scalar source/generation evidence only. Overflow is explicit.
type privateReraceReplayEvent struct {
	at        time.Time
	kind      string
	exit      string
	port      int
	source    Id
	sequence  Id
	message   Id
	number    uint64
	sendCount int
	resend    bool
	synRetry  bool
	serial    uint64
}

type privateReraceReplayTrace struct {
	stateLock sync.Mutex
	events    []privateReraceReplayEvent
	omitted   int
}

func (self *privateReraceReplayTrace) add(event privateReraceReplayEvent) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.events) == 8192 {
		self.omitted++
		return
	}
	self.events = append(self.events, event)
}

// Wire observation is set before construction and preserves previous observers.
func (self *privateReraceReplayTrace) settings(name string) func(*ClientSettings) {
	return func(settings *ClientSettings) {
		previous := settings.SendBufferSettings.TransferWireMessageObserver
		settings.SendBufferSettings.TransferWireMessageObserver = func(event TransferWireMessageObservation) {
			if previous != nil {
				previous(event)
			}
			wire := &protocol.TransferFrame{}
			if err := ProtoUnmarshal(event.TransferFrameBytes, wire); err != nil || wire.Pack == nil {
				return
			}
			var source, sequence Id
			if wire.TransferPath != nil {
				source, _ = IdFromBytes(wire.TransferPath.SourceId)
			}
			sequence, _ = IdFromBytes(wire.Pack.SequenceId)
			for _, frame := range wire.Pack.Frames {
				if frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
					continue
				}
				packet, err := ipPacketToProviderBytes(frame)
				if err != nil {
					continue
				}
				path, err := ParseIpPath(packet)
				if err != nil || path.Protocol != IpProtocolTcp || !path.Syn || path.Ack {
					continue
				}
				self.add(privateReraceReplayEvent{at: time.Now(), kind: "wire", exit: name, port: path.SourcePort,
					source: source, sequence: sequence, message: event.MessageId, number: event.SequenceNumber, sendCount: event.SendCount, resend: event.Resend})
			}
		}
	}
}

// Logged after the original workload settles; no trace is a replacement verdict.
func (self *privateReraceReplayTrace) logFlow(t *testing.T, port int, origin time.Time) {
	t.Helper()
	self.stateLock.Lock()
	events := append([]privateReraceReplayEvent(nil), self.events...)
	omitted := self.omitted
	self.stateLock.Unlock()
	count, selected := 0, 0
	for _, event := range events {
		if event.port != port {
			continue
		}
		count++
		if selected == 64 {
			continue
		}
		selected++
		t.Logf("RERACE_REPLAY_EVENT port=%d kind=%s at=%s exit=%s source=%s sequence=%s message=%s ordinal=%d send_count=%d transfer_resend=%t cached_syn_retry=%t serial=%d", port, event.kind, event.at.Sub(origin), event.exit, event.source, event.sequence, event.message, event.number, event.sendCount, event.resend, event.synRetry, event.serial)
	}
	t.Logf("RERACE_REPLAY_TRACE port=%d selected=%d matching=%d total=%d overflow_omitted=%d", port, selected, count, len(events), omitted)
}

// reraceProcessCpu is this process's user plus system cpu time.
func reraceProcessCpu(t *testing.T) time.Duration {
	t.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatal(err)
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// RR-8. When every exit fails a destination there is no storm: each SYN the
// app sends reaches each exit at most once and is re-raced at most once, the
// exits see no SYN the app did not send, nothing reaches the app, and the
// connect fails at the app's own timeout. Many such flows at once leave no
// goroutines behind, and once the app gives up nothing more is sent for them
// and the client idles.
func TestDialFailureEveryExitFailsReracesOncePerRetransmit(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		trace := &privateReraceReplayTrace{}
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: reraceAlways(reraceSignal), clientSettings: trace.settings("a")},
			{name: "b", tier: 1, answer: reraceAlways(reraceSignal), clientSettings: trace.settings("b")},
			{name: "c", tier: 2, answer: reraceAlways(reraceSignal), clientSettings: trace.settings("c")},
		}, nil)
		harness.multiClient.beforePacketGroupGateForTest = func(_ *multiClientChannelUpdate, client *multiClientChannel, group *parsedPacketGroup) {
			if group.ipPath.Protocol != IpProtocolTcp || !group.ipPath.Syn || group.ipPath.Ack {
				return
			}
			event := privateReraceReplayEvent{at: time.Now(), kind: "offer", port: group.ipPath.SourcePort, synRetry: group.isSynRetry()}
			if client != nil {
				event.source = client.ClientId()
			}
			if group.synRetry != nil {
				event.serial = group.synRetry.serial
			}
			trace.add(event)
		}
		exits := []*reraceExit{harness.exit("a"), harness.exit("b"), harness.exit("c")}
		// the client's own idle cost over ten minutes, before any flow
		cpuBefore := reraceProcessCpu(t)
		harness.sleep(10 * time.Minute)
		baselineIdleCpu := reraceProcessCpu(t) - cpuBefore
		baselineGoroutines := runtime.NumGoroutine()

		const flowCount = 32
		paths := []*IpPath{}
		for i := range flowCount {
			paths = append(paths, reraceFlow(4, 1+i%4, 44000+i))
		}
		synCounts := make([]int, flowCount)
		completed := make([]bool, flowCount)
		harness.concurrent = true
		var flows sync.WaitGroup
		for i, path := range paths {
			flows.Add(1)
			go func() {
				defer flows.Done()
				synCounts[i], _, completed[i] = harness.connect(path)
			}()
		}
		flows.Wait()
		harness.concurrent = false
		end := time.Now()

		appSyns, exitSyns := 0, 0
		tracedFlows := 0
		for i, path := range paths {
			flowFailed := false
			if completed[i] {
				t.Errorf("flow %d completed although every exit fails", i)
			}
			if synCounts[i] != len(reraceSynSchedule) {
				t.Errorf("flow %d: the app sent %d SYNs, want its whole schedule", i, synCounts[i])
			}
			appSyns += synCounts[i]
			if got := len(harness.delivered(path)); got != 0 {
				flowFailed = true
				t.Errorf("flow %d: the app received %d packets, want none before its own timeout", i, got)
				if tracedFlows < 4 {
					for _, delivered := range harness.delivered(path) {
						parsed, err := ParseIpPath(delivered.packet)
						if err == nil {
							t.Logf("RERACE_APP_EVENT port=%d at=%s protocol=%v syn=%t ack=%t rst=%t fin=%t sequence=%d ack_number=%d bytes=%d", path.SourcePort, delivered.at.Sub(harness.start), parsed.Protocol, parsed.Syn, parsed.Ack, parsed.Rst, parsed.Fin, parsed.SequenceNumber, parsed.AckSequenceNumber, len(delivered.packet))
						}
					}
				}
			}
			sent := harness.sentSyns(path)
			for _, exit := range exits {
				perSyn := make([]int, len(sent))
				for _, packet := range exit.syns(path) {
					exitSyns += 1
					// the app SYN this one forwards: the last sent before it
					j := len(sent) - 1
					for 0 <= j && packet.at.Before(sent[j]) {
						j -= 1
					}
					if j < 0 || reraceSendTimeout < packet.at.Sub(sent[j]) {
						flowFailed = true
						t.Errorf("flow %d: %s received a SYN at +%s that the app did not send; actual_source_generation=%s", i, exit.name, packet.at.Sub(harness.start), packet.source)
						continue
					}
					perSyn[j] += 1
					if 1 < perSyn[j] {
						t.Errorf("flow %d: %s received app SYN %d %d times", i, exit.name, j, perSyn[j])
					}
				}
			}
			if flowFailed && tracedFlows < 4 {
				trace.logFlow(t, path.SourcePort, harness.start)
				tracedFlows++
			}
		}
		metrics := harness.metrics()
		if uint64(appSyns) < metrics.FlowsReraced {
			t.Errorf("FlowsReraced = %d for %d app SYNs, want at most one re-race each", metrics.FlowsReraced, appSyns)
		}
		if metrics.DialFailuresIntercepted != uint64(exitSyns) {
			t.Errorf("DialFailuresIntercepted = %d, want one per failed dial (%d)", metrics.DialFailuresIntercepted, exitSyns)
		}

		// the app has given up: ten idle minutes send nothing more for the
		// flows, and the flows' goroutines are gone
		cpuBefore = reraceProcessCpu(t)
		harness.sleep(10 * time.Minute)
		idleCpu := reraceProcessCpu(t) - cpuBefore
		for i, path := range paths {
			for _, exit := range exits {
				for _, packet := range exit.received(path) {
					if packet.at.After(end) && !packet.path.Rst {
						t.Errorf("flow %d: %s received %+v after the app gave up", i, exit.name, packet.path)
					}
				}
			}
		}
		goroutines := runtime.NumGoroutine()
		t.Logf("%d app SYNs, %d exit SYNs, %d re-races; goroutines %d before, %d after; cpu over ten idle minutes %s before, %s after",
			appSyns, exitSyns, metrics.FlowsReraced, baselineGoroutines, goroutines, baselineIdleCpu, idleCpu)
		if baselineGoroutines+16 < goroutines {
			t.Errorf("%d goroutines after the storm, want at most the %d before it plus 16", goroutines, baselineGoroutines)
		}
		// idle after the storm costs what idle cost before it; a loop over the
		// dead flows, however slow, would run for all ten minutes
		if 2*baselineIdleCpu+500*time.Millisecond < idleCpu {
			t.Errorf("ten idle minutes after the storm cost %s of cpu, against %s before it", idleCpu, baselineIdleCpu)
		}
	})
}
