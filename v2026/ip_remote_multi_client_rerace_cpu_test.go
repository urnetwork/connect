//go:build !js && !windows

package connect

// The storm test of RR-8 (see ip_remote_multi_client_rerace_test.go) also
// bounds the cpu the client spends idle after the storm. It reads this
// process's cpu time through syscall.Getrusage, which js/wasm and windows do
// not have, so it builds only where net_extender_scale_cpu_test.go does.

import (
	"bytes"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
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
	offer     uint64
	native    uint64
	remote    Id
	valid     bool
}

type privateReraceReplayTrace struct {
	stateLock sync.Mutex
	events    []privateReraceReplayEvent
	omitted   int
	offers    map[*tcpSynRetryRequest]privateReraceReplayEvent
	natives   map[*multiClientChannelUpdate]uint64
	updates   map[*multiClientChannelUpdate]int
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
		previousDisposition := settings.SendBufferSettings.afterGroupDispositionForTest
		settings.SendBufferSettings.afterGroupDispositionForTest = func(source *SendSequence, disposition sendGroupDisposition, frames []*protocol.Frame) {
			if previousDisposition != nil {
				previousDisposition(source, disposition, frames)
			}
			group, ok := disposition.target.(*parsedPacketGroup)
			if !ok || group.ipPath.Protocol != IpProtocolTcp || !group.ipPath.Syn || group.ipPath.Ack {
				return
			}
			self.stateLock.Lock()
			offer := self.offers[group.synRetry]
			self.stateLock.Unlock()
			// Every workload SYN requires Transfer ACKs. The source has just
			// retained this exact item and has not exposed it to any writer.
			item := source.sendItems[len(source.sendItems)-1]
			self.add(privateReraceReplayEvent{at: time.Now(), kind: "materialized", exit: name, port: group.ipPath.SourcePort,
				source: source.client.ClientId(), remote: source.destination, sequence: source.sequenceId,
				message: item.messageId, number: item.sequenceNumber, offer: offer.offer, native: offer.native, synRetry: group.isSynRetry()})
		}
		previous := settings.SendBufferSettings.TransferWireMessageObserver
		settings.SendBufferSettings.TransferWireMessageObserver = func(event TransferWireMessageObservation) {
			if previous != nil {
				previous(event)
			}
			wire := &protocol.TransferFrame{}
			if err := ProtoUnmarshal(event.TransferFrameBytes, wire); err != nil || wire.Pack == nil {
				return
			}
			var source, remote, sequence Id
			if wire.TransferPath != nil {
				source, _ = IdFromBytes(wire.TransferPath.SourceId)
				remote, _ = IdFromBytes(wire.TransferPath.DestinationId)
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
					source: source, remote: remote, sequence: sequence, message: event.MessageId, number: event.SequenceNumber, sendCount: event.SendCount, resend: event.Resend})
			}
		}
	}
}

// Observe each real source offer before selection. Race copies share its
// immutable request; cached work must still own this exact live generation.
func (self *privateReraceReplayTrace) watch(harness *reraceHarness) {
	previous := harness.multiClient.beforePacketGroupGateForTest
	harness.multiClient.beforePacketGroupGateForTest = func(update *multiClientChannelUpdate, client *multiClientChannel, group *parsedPacketGroup) {
		if previous != nil {
			previous(update, client, group)
		}
		if group.ipPath.Protocol != IpProtocolTcp || !group.ipPath.Syn || group.ipPath.Ack {
			return
		}
		valid := group.synRetry != nil
		if group.isSynRetry() {
			update.stateLock.Lock()
			owner := update.synRetry
			valid = group.validSynRetryWithLock(update) && owner != nil && owner.credit != nil &&
				!owner.credit.released.Load() && bytes.Equal(owner.packet, group.packets[0].packet)
			update.stateLock.Unlock()
		}
		self.stateLock.Lock()
		if self.offers == nil {
			self.offers = map[*tcpSynRetryRequest]privateReraceReplayEvent{}
			self.natives = map[*multiClientChannelUpdate]uint64{}
			self.updates = map[*multiClientChannelUpdate]int{}
		}
		offer := uint64(len(self.offers) + 1)
		if !group.isSynRetry() {
			self.natives[update] = offer
		}
		event := privateReraceReplayEvent{at: time.Now(), kind: "offer", port: group.ipPath.SourcePort,
			offer: offer, native: self.natives[update], synRetry: group.isSynRetry(), valid: valid}
		if group.synRetry != nil {
			event.serial = group.synRetry.serial
		}
		self.offers[group.synRetry] = event
		self.updates[update] = group.ipPath.SourcePort
		self.stateLock.Unlock()
		self.add(event)
	}
}

// Exact source dispositions and wire identities replace time proximity: a
// newly published provider may lawfully consume an older paid SYN copy.
func privateReraceReplayProblems(events []privateReraceReplayEvent, port int) []string {
	type wireKey struct {
		source, sequence, message Id
		number                    uint64
	}
	type offerKey struct {
		offer  uint64
		remote Id
	}
	problems := []string{}
	offers := map[uint64]privateReraceReplayEvent{}
	materialized := map[wireKey]bool{}
	perOffer := map[offerKey]int{}
	written := map[wireKey]bool{}
	for _, event := range events {
		if event.port != port {
			continue
		}
		key := wireKey{source: event.source, sequence: event.sequence, message: event.message, number: event.number}
		switch event.kind {
		case "offer":
			offers[event.offer] = event
		case "materialized":
			offer, ok := offers[event.offer]
			if !ok || !offer.valid || offer.synRetry != event.synRetry || offer.native == 0 || offer.native != event.native {
				problems = append(problems, "materialized SYN lacks its exact live source owner offer")
			}
			perOffer[offerKey{offer: event.native, remote: event.remote}]++
			if perOffer[offerKey{offer: event.native, remote: event.remote}] > 1 {
				problems = append(problems, "duplicate SYN for one native offer and remote")
			}
			if materialized[key] {
				problems = append(problems, "duplicate Transfer materialization")
			}
			materialized[key] = true
		case "wire":
			if !materialized[key] {
				problems = append(problems, "unowned wire SYN")
			}
			if event.resend || event.sendCount != 1 || written[key] {
				problems = append(problems, "replayed wire SYN")
			}
			written[key] = true
		}
	}
	for key := range materialized {
		if !written[key] {
			problems = append(problems, "materialized SYN never reached its wire")
		}
	}
	return problems
}

// The finite trace retains generation references only through this verdict.
// A reset must match a real source-reset generation, once, byte for byte.
func (self *privateReraceReplayTrace) flowProblems(harness *reraceHarness, path *IpPath) []string {
	self.stateLock.Lock()
	events := append([]privateReraceReplayEvent(nil), self.events...)
	updates := map[*multiClientChannelUpdate]int{}
	for update, port := range self.updates {
		updates[update] = port
	}
	omitted := self.omitted
	self.stateLock.Unlock()
	problems := privateReraceReplayProblems(events, path.SourcePort)
	if omitted != 0 {
		problems = append(problems, "trace overflow")
	}
	nativeOffers := 0
	wireCounts := map[Id]int{}
	for _, event := range events {
		if event.port == path.SourcePort {
			if event.kind == "offer" && !event.synRetry {
				nativeOffers++
			}
			if event.kind == "wire" {
				wireCounts[event.source]++
			}
		}
	}
	if nativeOffers != len(harness.sentSyns(path)) {
		problems = append(problems, "native offers differ from the app SYN schedule")
	}
	for _, exit := range harness.generator.exitList() {
		for _, packet := range exit.syns(path) {
			wireCounts[packet.source]--
		}
	}
	for _, count := range wireCounts {
		if count != 0 {
			problems = append(problems, "provider SYNs differ from exact wire writes")
		}
	}
	var resets [][]byte
	for update, port := range updates {
		if port == path.SourcePort && update.sourceReset.Load() {
			if packet, ok := harness.multiClient.teardownSourcePacket(update.ipPath, update.sourceRstSequence()); ok {
				resets = append(resets, packet)
			}
		}
	}
	for _, received := range harness.delivered(path) {
		matched := -1
		for index, reset := range resets {
			if bytes.Equal(reset, received.packet) {
				matched = index
				break
			}
		}
		if matched < 0 {
			problems = append(problems, "app packet lacks an exact source-reset generation")
		} else {
			resets = append(resets[:matched], resets[matched+1:]...)
		}
	}
	return problems
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

// RR-8. Every provider SYN belongs to one actual native source offer or its
// paid continuation, with at most one SYN per remote and native offer. A later
// publication may resume that retained copy; quarantine may independently
// reset its exact generation. Dial failures themselves never reach the app.
// Concurrent failures leave no goroutines or ongoing idle work behind.
func TestDialFailureEveryExitFailsReracesOncePerRetransmit(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		trace := &privateReraceReplayTrace{}
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: reraceAlways(reraceSignal), clientSettings: trace.settings("a")},
			{name: "b", tier: 1, answer: reraceAlways(reraceSignal), clientSettings: trace.settings("b")},
			{name: "c", tier: 2, answer: reraceAlways(reraceSignal), clientSettings: trace.settings("c")},
		}, nil)
		trace.watch(harness)
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
			for _, problem := range privateReraceStormProblems(trace, harness, path) {
				flowFailed = true
				t.Errorf("flow %d: %s", i, problem)
			}
			for _, exit := range exits {
				exitSyns += len(exit.syns(path))
			}
			if flowFailed && tracedFlows < 4 {
				trace.logFlow(t, path.SourcePort, harness.start)
				tracedFlows++
			}
		}
		// The verdict has copied all scalar evidence; do not retain production
		// generation graphs while comparing the two idle intervals.
		trace.stateLock.Lock()
		trace.offers, trace.natives, trace.updates = nil, nil, nil
		trace.stateLock.Unlock()
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
