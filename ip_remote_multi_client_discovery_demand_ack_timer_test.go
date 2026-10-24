package connect

import (
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

type discoveryDemandTestPingIdentity struct {
	Message  Id
	Sequence Id
	Number   uint64
	Lane     uint32
}

type discoveryDemandTestAckTimerState struct {
	mutex                                                                   sync.Mutex
	client                                                                  Id
	first, second                                                           discoveryDemandTestPingIdentity
	firstSeen, secondSeen, firstWritten, anchored, timerSeen, secondIngress bool
	receive                                                                 receiveSequenceId
	anchorAt, timerAt, ingressAt                                            time.Time
	firstAck                                                                chan struct{}
	publication                                                             chan struct{}
	publicationOnce                                                         sync.Once
}

func (self *discoveryDemandTestAckTimerState) observeWire(observation TransferWireMessageObservation) {
	if observation.Resend {
		return
	}
	frame := &protocol.TransferFrame{}
	if proto.Unmarshal(observation.TransferFrameBytes, frame) != nil {
		return
	}
	pack := frame.Pack
	if pack == nil && frame.Frame != nil && frame.Frame.MessageType == protocol.MessageType_TransferPack {
		pack = &protocol.Pack{}
		if proto.Unmarshal(frame.Frame.MessageBytes, pack) != nil {
			return
		}
	}
	if pack == nil {
		return
	}
	ping := false
	for _, payload := range pack.Frames {
		ping = ping || payload.MessageType == protocol.MessageType_IpIpPing
	}
	if !ping {
		return
	}
	message, err := IdFromBytes(pack.MessageId)
	if err != nil {
		return
	}
	sequence, err := IdFromBytes(pack.SequenceId)
	if err != nil {
		return
	}
	identity := discoveryDemandTestPingIdentity{Message: message, Sequence: sequence, Number: pack.SequenceNumber, Lane: pack.LogicalLane}
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if !self.firstSeen {
		self.first, self.firstSeen = identity, true
	} else if !self.secondSeen && identity.Message != self.first.Message {
		self.second, self.secondSeen = identity, true
	}
}

func (self *discoveryDemandTestAckTimerState) observeProviderProgress(event TransferProgressEvent) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.firstSeen && event.Stage == "ack_write_end" && event.Success && event.PeerId == self.client && event.SequenceId == self.first.Sequence && event.MessageId == self.first.Message {
		self.firstWritten = true
	}
}

func (self *discoveryDemandTestAckTimerState) observeAckWritten(id receiveSequenceId) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if !self.anchored && self.firstWritten && id.Source.SourceId == self.client && id.SequenceId == self.first.Sequence && id.LogicalLane == self.first.Lane {
		self.receive, self.anchorAt, self.anchored = id, time.Now(), true
		close(self.firstAck)
	}
}

func (self *discoveryDemandTestAckTimerState) observeCompressionWait(id receiveSequenceId) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.anchored && self.secondSeen && id == self.receive && self.second.Sequence == self.first.Sequence && self.second.Lane == self.first.Lane && self.second.Number == self.first.Number+1 {
		self.timerSeen, self.timerAt = true, time.Now()
	}
}

func (self *discoveryDemandTestAckTimerState) observeClientProgress(event TransferProgressEvent) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.secondSeen && event.Stage == "receive_ack_end" && event.Success && event.SequenceId == self.second.Sequence && event.MessageId == self.second.Message {
		self.secondIngress, self.ingressAt = true, time.Now()
	}
}

// Adds exactly the canonical in-process exit, configuring its receive hooks
// before NewClient starts any worker. Routes, packets and buffering match.
func discoveryDemandTestAddAckTimerExit(h *reraceHarness, state *discoveryDemandTestAckTimerState) *reraceExit {
	settings := DefaultClientSettingsWithBufferSize(testClientTransferBufferSize)
	settings.Log = NewNoopLogger()
	settings.ReceiveBufferSettings.ProgressObserver = state.observeProviderProgress
	settings.ReceiveBufferSettings.afterAckWriteForTest = state.observeAckWritten
	settings.ReceiveBufferSettings.beforeAckCompressWaitForTest = state.observeCompressionWait
	exit := &reraceExit{reraceExitSpec: reraceExitSpec{name: "late", family: IpFamilyDualstack}, t: h.t,
		provider: NewClient(h.ctx, NewId(), NewNoContractClientOob(), settings), replies: make(chan reraceReply, 4096)}
	exit.destination = RequireMultiHopId(exit.provider.ClientId())
	exit.provider.AddReceiveCallback(exit.receive)
	h.exitWorkers.Add(1)
	go func() { defer h.exitWorkers.Done(); exit.run(h.ctx) }()
	h.generator.mutex.Lock()
	h.generator.exits = append(h.generator.exits, exit)
	h.generator.mutex.Unlock()
	return exit
}

func TestDiscoveryDemandActualAckTimerPublication(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		path := unboundFlowTcpPath(52870)
		specs := make([]reraceExitSpec, 4)
		for index := range specs {
			specs[index] = reraceExitSpec{name: string(rune('a' + index)), answer: func(candidate *IpPath) reraceAnswer {
				if candidate.DestinationIp.Equal(path.DestinationIp) {
					return reraceSignal
				}
				return reraceAccept
			}}
		}
		h, generator := newDiscoveryDemandTestHarness(t, specs, func(settings *MultiClientSettings) { settings.InitialPingObservations = &InitialPingObservations{} })
		window, paths, owners := discoveryDemandTestCarryFour(t, h)
		synctest.Wait()
		window.stateLock.Lock()
		deadline := window.discoveryDemand.deadline
		liveSlot := window.discoveryDemand.owner != nil && window.discoveryDemand.slotOpen && time.Until(deadline) > 4*time.Second
		window.stateLock.Unlock()
		if !liveSlot || len(h.liveChannels()) != 4 {
			t.Fatal("actual ACK timer control lacks the original vacant acquisition")
		}
		state := &discoveryDemandTestAckTimerState{firstAck: make(chan struct{}), publication: make(chan struct{})}
		generator.mutex.Lock()
		generator.configureNewClient = func(args *MultiClientGeneratorClientArgs, settings *ClientSettings) {
			state.mutex.Lock()
			state.client = args.ClientId
			state.mutex.Unlock()
			settings.SendBufferSettings.TransferWireMessageObserver = state.observeWire
			settings.ReceiveBufferSettings.ProgressObserver = state.observeClientProgress
		}
		generator.mutex.Unlock()
		window.settings.startChannelPingForTest = func(start func()) {
			start()
			timer := time.NewTimer(time.Until(deadline))
			defer timer.Stop()
			select {
			case <-state.firstAck:
			case <-h.ctx.Done():
			case <-timer.C:
			}
		}
		window.afterExpandPingResultForTest = func() { state.publicationOnce.Do(func() { close(state.publication) }) }
		beforeReads, beforeEmpty, beforeMints, beforeRetired := generator.counts()
		start := time.Now()
		if !h.syn(path) {
			t.Fatal("actual source SYN was not admitted")
		}
		update := h.update(path)
		update.stateLock.Lock()
		failed, inbound := len(update.dialFailedClients), update.receivedInbound.Load()
		update.stateLock.Unlock()
		firstReads, firstEmpty, _, _ := generator.counts()
		if failed != 4 || inbound || firstReads != beforeReads+1 || firstEmpty != beforeEmpty+1 {
			t.Fatal("actual ACK timer control lacks four refusals followed by a real empty demand read")
		}
		time.Sleep(100 * time.Millisecond)
		late := discoveryDemandTestAddAckTimerExit(h, state)
		h.sleep(start.Add(reraceSynSchedule[1]).Sub(time.Now()))
		if !h.syn(path) {
			t.Fatal("unchanged1s source SYN was refused")
		}
		reads, empty, mints, retired := generator.counts()
		state.mutex.Lock()
		premise := state.firstSeen && state.secondSeen && state.firstWritten && state.anchored && state.timerSeen && !state.secondIngress && state.timerAt.Equal(state.anchorAt)
		anchorAt, timerAt := state.anchorAt, state.timerAt
		state.mutex.Unlock()
		terminal := uint64(0)
		for _, cell := range window.settings.InitialPingObservations.Snapshot() {
			terminal += cell.Count
		}
		owned := len(h.liveChannels())
		if !premise || owned != 4 || reads != beforeReads+2 || empty != beforeEmpty+1 || mints != beforeMints+1 || retired != beforeRetired || window.settings.InitialPingObservations.Started() != 5 || terminal != 4 || time.Since(start) != reraceSynSchedule[1] {
			t.Fatalf("actual ACK timer premise missing: anchored=%t owned=%d reads=%d mints=%d terminal=%d", premise, owned, reads-beforeReads, mints-beforeMints, terminal)
		}
		discoveryDemandTestAwaitPublication(t, state.publication, discoveryDemandTestEarlierTime(deadline, start.Add(reraceSynSchedule[2])))
		synctest.Wait()
		state.mutex.Lock()
		ingress, ingressAt := state.secondIngress, state.ingressAt
		state.mutex.Unlock()
		compression := late.provider.settings.ReceiveBufferSettings.AckCompressTimeout
		if !ingress || !ingressAt.After(timerAt) || ingressAt.After(anchorAt.Add(compression)) || len(h.liveChannels()) != 5 || !time.Now().Before(start.Add(reraceSynSchedule[2])) {
			t.Fatal("actual ACK timer did not publish the candidate within its original compression/nextSYN boundaries")
		}
		h.sleep(start.Add(reraceSynSchedule[2]).Sub(time.Now()))
		if !h.syn(path) {
			t.Fatal("unchanged3s source SYN was refused")
		}
		if _, ok := h.synAck(path); !ok || h.boundTo(path) != h.channel("late") || 5*time.Second <= time.Since(start) {
			t.Fatal("actual recovered source connect missed original3sSYN/5s outcome")
		}
		payload := []byte("held original CSS sibling")
		for index, original := range paths {
			if h.boundTo(original) != owners[index] || owners[index].IsDone() || owners[index].isWarning() || owners[index].dialStarved() || !h.send(reraceSegment(original, uint32(4*len(payload)), payload)) || len(h.delivered(original)) != 6 {
				t.Fatal("actual ACK timer control disturbed an original healthy owner")
			}
		}
	})
}
